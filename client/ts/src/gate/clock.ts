// 校时（对应 C# ClockSample / NetworkClock，同一套判定）。探测是 MsgID=0、载荷 "RCN1" 的控制帧，Gate 回 20 字节：
// "RCN1" + 服务端收到与发出时刻（Unix 纳秒，大端 int64）。本地时钟用 performance.now() 的秒数。

import { GateProtocolError } from "./packet.js";

/** 探测载荷 "RCN1"。 */
export const CLOCK_PROBE = Uint8Array.of(82, 67, 78, 49);

/** 本地单调时钟（秒）。 */
export function localNow(): number {
  return (globalThis.performance ? globalThis.performance.now() : Date.now()) / 1000;
}

/** 一次校时样本：本地发出 / 收到（localNow 的秒），服务端收到 / 发出（Unix 秒）。 */
export interface ClockSample {
  sent: number;
  received: number;
  serverReceived: number;
  serverSent: number;
}

export function decodeClockSample(sent: number, received: number, payload: Uint8Array): ClockSample {
  if (payload.length !== 20 || payload[0] !== 82 || payload[1] !== 67 || payload[2] !== 78 || payload[3] !== 49) {
    throw new GateProtocolError("invalid Gate clock response");
  }
  const view = new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
  return {
    sent,
    received,
    serverReceived: Number(view.getBigInt64(4)) / 1e9,
    serverSent: Number(view.getBigInt64(12)) / 1e9,
  };
}

/**
 * 只估计提供样本的服务器时钟域；重连 / 换服必须 reset。roundTrip 含本地排队、网络、Gate 写队列与事件循环延迟，不能解释为
 * 纯网络耗时。
 */
export class NetworkClock {
  private offset = 0;
  private target = 0;
  private lastLocal = 0;
  private lastSample = -Infinity;
  private isReady = false;
  private rtt = 0;

  constructor(
    readonly maxRoundTrip = 2,
    readonly slewRate = 0.05,
  ) {
    if (!Number.isFinite(maxRoundTrip) || maxRoundTrip <= 0 || !Number.isFinite(slewRate) || slewRate <= 0 || slewRate >= 1) {
      throw new RangeError("gate: NetworkClock needs a positive maxRoundTrip and a slewRate in (0, 1)");
    }
  }

  get ready(): boolean {
    return this.isReady;
  }

  get roundTrip(): number {
    return this.rtt;
  }

  get uncertainty(): number {
    return this.isReady ? this.rtt / 2 + Math.abs(this.target - this.offset) : 0;
  }

  reset(): void {
    this.isReady = false;
    this.lastSample = -Infinity;
    this.rtt = 0;
  }

  /** 收一份样本；被拒（非有限数、乱序、负耗时、往返过长、已有低 RTT 时严重排队）返回 false。 */
  observe(sample: ClockSample): boolean {
    const elapsed = sample.received - sample.sent;
    const processing = sample.serverSent - sample.serverReceived;
    const rtt = elapsed - processing;
    if (
      !Number.isFinite(sample.sent) ||
      !Number.isFinite(sample.received) ||
      !Number.isFinite(sample.serverReceived) ||
      !Number.isFinite(sample.serverSent) ||
      sample.received <= this.lastSample ||
      elapsed < 0 ||
      processing < 0 ||
      rtt < 0 ||
      elapsed > this.maxRoundTrip
    ) {
      return false;
    }
    // 已有低 RTT 样本时拒绝严重排队的探针，不让一次卡顿直接改变时钟。
    if (this.isReady && rtt > Math.max(0.05, this.rtt * 3)) return false;
    const next = sample.serverSent + rtt / 2 - sample.received;
    if (!this.isReady) {
      this.offset = next;
      this.lastLocal = sample.received;
      this.isReady = true;
    }
    this.target = next;
    this.rtt = rtt;
    this.lastSample = sample.received;
    return true;
  }

  /** 服务器时钟（Unix 秒）：本地时刻加偏移，偏移按 slewRate 平滑追向最新样本。 */
  now(local: number): number {
    if (!this.isReady) throw new Error("gate: clock has no sample");
    if (!Number.isFinite(local) || local < this.lastLocal) throw new RangeError("gate: local time went backwards");
    const allowance = (local - this.lastLocal) * this.slewRate;
    this.offset += Math.max(-allowance, Math.min(allowance, this.target - this.offset));
    this.lastLocal = local;
    return local + this.offset;
  }
}
