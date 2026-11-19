// 一条连接的生命周期（对应 C# TcpSession）。断线后建新实例，旧应答不能跨实例匹配。
//
// 单线程：没有发送锁与 SemaphoreSlim。序列在发送时分配，顺序就是线上顺序。收到的字节同步拼帧、逐帧处理：推送立即交给
// onPush（维护者 2026-11-18 D6：网络事件里同步派发，不做 Pump）；应答交给登记它的请求的完成回调（同步调用，所以回调形式
// 的应答与推送严格按线上顺序到达）；没有等待者的晚应答丢弃，不当推送。处理过程中会话被关（回调里 dispose）时，这条消息里
// 剩下的帧不再处理。

import { encodeAuth, MAX_TICKET_BYTES, nextConnectionGeneration } from "./auth.js";
import { GateFrameReader, GateProtocolError, PayloadKind, encodePacket, type Packet } from "./packet.js";
import { GateAbortedError, GateClosedError, GateTimeoutError, type GateTimers } from "./options.js";
import type { GateTransport } from "./transport.js";
import { utf8Encode } from "../wire/header.js";

/** 请求的完成回调：error 与 packet 恰有一个。只调用一次。 */
export type Completion = (error: Error | undefined, packet: Packet | undefined) => void;

interface Pending {
  responseId: number;
  heartbeat: boolean;
  complete: Completion;
  timer: unknown;
  abort?: () => void;
  signal?: AbortSignal;
}

export interface SessionLimits {
  maxPayloadBytes: number;
  maxPendingRequests: number;
  timers: GateTimers;
}

export class GateSession {
  // 推送与结束的处理者由 attach 接上。握手应答之后、attach 之前（Promise 续行之间）到的推送先排着，attach 时按序交出；
  // 这期间结束的，attach 时补报。
  private onPush: ((packet: Packet) => void) | undefined;
  private onClosed: ((reason: Error) => void) | undefined;
  private early: Packet[] = [];

  private readonly reader: GateFrameReader;
  private readonly pending = new Map<number, Pending>();
  private nextSequence = 0;
  private inFlight = 0;
  private heartbeatInFlight = false;
  private closed: Error | undefined;

  private constructor(
    private readonly transport: GateTransport,
    private readonly limits: SessionLimits,
  ) {
    this.reader = new GateFrameReader(limits.maxPayloadBytes);
    transport.onData = (chunk) => this.receive(chunk);
    transport.onClose = (error) => this.close(error ?? new GateClosedError("gate: the server closed the connection"));
  }

  /**
   * 接上推送（同步调用；它可以关掉会话）与结束（对端断开、协议错误、写失败、心跳超时、主动 close，只调用一次）的处理者。
   * attach 之前排着的推送立即按序交出；已经结束的立即补报。
   */
  attach(onPush: (packet: Packet) => void, onClosed: (reason: Error) => void): void {
    this.onPush = onPush;
    this.onClosed = onClosed;
    const early = this.early;
    this.early = [];
    for (const packet of early) {
      if (this.closed) break;
      onPush(packet);
    }
    if (this.closed) onClosed(this.closed);
  }

  /** 关闭原因；还开着时为 undefined。 */
  get closeReason(): Error | undefined {
    return this.closed;
  }

  /**
   * 经 transport 连到 url 并完成鉴权握手：首包是带新连接代号的信封（每次连接都取新的，含重连）。transport 是新建、还没连过
   * 的实例，归本会话所有（失败时关闭）。timeoutMs 覆盖连接与握手；signal 中止时同样关闭 transport。
   */
  static async open(transport: GateTransport, url: string, ticket: string, timeoutMs: number, signal: AbortSignal, limits: SessionLimits): Promise<GateSession> {
    let auth: Uint8Array;
    try {
      const raw = utf8Encode(ticket);
      // 服务端缺省的鉴权首包上限是 8 KiB（gateway TCPConfig.MaxHandshakeBytes），其中 10 字节是连接代号信封。
      if (raw.length === 0 || raw.length > MAX_TICKET_BYTES) throw new RangeError(`gate: ticket must be 1..${MAX_TICKET_BYTES} UTF-8 bytes`);
      if (raw[0] === 0) throw new RangeError("gate: ticket must not start with a NUL byte");
      auth = encodeAuth(raw, nextConnectionGeneration());
    } catch (error) {
      transport.close();
      throw error;
    }
    const timers = limits.timers;
    let session: GateSession | undefined;
    let timer: unknown;
    let onAbort: (() => void) | undefined;
    const budget = new Promise<never>((_, reject) => {
      timer = timers.setTimeout(() => reject(new GateTimeoutError(`gate: connecting to ${url} timed out after ${timeoutMs}ms`)), timeoutMs);
      onAbort = () => reject(abortReason(signal));
      if (signal.aborted) onAbort();
      else signal.addEventListener("abort", onAbort, { once: true });
    });
    try {
      // 超时或中止时关掉连接：不依赖各实现对“连接中被打断”的处理。
      const connecting = transport.connect(url);
      connecting.catch(() => {}); // 预算先到时 close 让它 reject，没人再等
      await Promise.race([connecting, budget]);
      session = new GateSession(transport, limits);
      const opened = session;
      const ack = await Promise.race([
        new Promise<Packet>((resolve, reject) => opened.send(0, PayloadKind.Protobuf, auth, false, (error, packet) => (error ? reject(error) : resolve(packet!)))),
        budget,
      ]);
      if (ack.payload.length !== 0) throw new GateProtocolError("authentication acknowledgement must be empty");
      return opened;
    } catch (error) {
      session?.close(error as Error);
      transport.close();
      throw error;
    } finally {
      timers.clearTimeout(timer);
      if (onAbort) signal.removeEventListener("abort", onAbort);
      budget.catch(() => {}); // 预算在成功之后不再有人等
    }
  }

  /**
   * 发一个请求，应答（或失败）交给 complete。timeoutMs 为 undefined 时不计时（由上层计）；signal 中止时取消等待，不关连接。
   * 已发出后的超时与中止只取消等待，不能推断服务端没执行，也不自动重发。
   */
  request(messageId: number, responseId: number, payload: Uint8Array, complete: Completion, timeoutMs?: number, signal?: AbortSignal): void {
    this.send(messageId, PayloadKind.Protobuf, payload, false, complete, responseId, timeoutMs, signal);
  }

  /** 心跳：MsgID=0 空载荷，独占一个槽位，业务请求与校时不能借用。 */
  heartbeat(timeoutMs: number, complete: Completion): void {
    this.send(0, PayloadKind.Protobuf, new Uint8Array(0), true, complete, 0, timeoutMs);
  }

  /** 只写出、不等应答的业务消息（与请求共享序列）。不自动重发。 */
  notify(messageId: number, payload: Uint8Array): void {
    if (messageId === 0) throw new RangeError("gate: message id 0 is reserved for Gate control");
    this.send(messageId, PayloadKind.Protobuf, payload, false, undefined);
  }

  /** 关闭会话：在途请求都以 reason 失败，transport 关闭，onClosed 调用一次。可重复调用。 */
  close(reason: Error): void {
    if (this.closed) return;
    this.closed = reason;
    this.transport.onData = null;
    this.transport.onClose = null;
    this.transport.close();
    const pending = [...this.pending.values()];
    this.pending.clear();
    for (const item of pending) this.finish(item, new GateClosedError("gate: the connection closed before the response", reason), undefined);
    this.early = [];
    this.onClosed?.(reason);
  }

  private send(
    messageId: number,
    kind: PayloadKind,
    payload: Uint8Array,
    heartbeat: boolean,
    complete: Completion | undefined,
    responseId = 0,
    timeoutMs?: number,
    signal?: AbortSignal,
  ): void {
    if (this.closed) throw new GateClosedError("gate: session closed", this.closed);
    if (signal?.aborted) throw abortReason(signal);
    // 先编码：坏输入不写出半包，也不关掉健康的连接。
    const data = encodePacket({ messageId, sequence: 0, isPush: false, kind, payload }, this.limits.maxPayloadBytes);
    if (complete) {
      if (heartbeat) {
        if (this.heartbeatInFlight) throw new Error("gate: a heartbeat is already pending");
      } else if (this.inFlight >= this.limits.maxPendingRequests) {
        throw new Error(`gate: pending request capacity (${this.limits.maxPendingRequests}) exhausted`);
      }
    }
    let sequence: number;
    do {
      sequence = this.nextSequence = (this.nextSequence + 1) >>> 0;
    } while (sequence === 0 || this.pending.has(sequence));
    new DataView(data.buffer, data.byteOffset, data.byteLength).setUint32(8, sequence);
    if (complete) {
      const item: Pending = { responseId, heartbeat, complete, timer: undefined };
      if (heartbeat) this.heartbeatInFlight = true;
      else this.inFlight++;
      this.pending.set(sequence, item);
      if (timeoutMs !== undefined) {
        item.timer = this.limits.timers.setTimeout(() => {
          if (this.pending.get(sequence) !== item) return;
          this.pending.delete(sequence);
          const what = heartbeat ? "heartbeat" : messageId === 0 ? "control request" : `request ${messageId}`;
          this.finish(item, new GateTimeoutError(`gate: ${what} timed out after ${timeoutMs}ms; the server may already have executed it`), undefined);
        }, timeoutMs);
      }
      if (signal) {
        item.signal = signal;
        item.abort = () => {
          if (this.pending.get(sequence) !== item) return;
          this.pending.delete(sequence);
          this.finish(item, abortReason(signal), undefined);
        };
        signal.addEventListener("abort", item.abort, { once: true });
      }
    }
    try {
      // 一帧一次 send：WebSocket 上恰好一条二进制消息（GATEWAY §6）。
      this.transport.send(data);
    } catch (error) {
      // 写失败：连接不可用，整条关掉（在途请求包括这一个都失败）。
      const reason = error instanceof Error ? error : new Error(String(error));
      this.close(reason);
      // 已登记的请求由 close 结算；再抛错会让 GameClient.begin 再投递一次失败。
      // notify 没有回调，写失败仍须同步通知调用方。
      if (!complete) throw new GateClosedError("gate: write failed", reason);
    }
  }

  private finish(item: Pending, error: Error | undefined, packet: Packet | undefined): void {
    this.limits.timers.clearTimeout(item.timer);
    if (item.abort) item.signal?.removeEventListener("abort", item.abort);
    if (item.heartbeat) this.heartbeatInFlight = false;
    else this.inFlight--;
    item.complete(error, packet);
  }

  private receive(chunk: Uint8Array): void {
    let packets: Packet[];
    try {
      packets = this.reader.push(chunk);
    } catch (error) {
      this.close(error as Error);
      return;
    }
    for (const packet of packets) {
      if (this.closed) return; // 回调里关了会话（例如 dispose）：剩下的帧不再处理
      if (packet.isPush) {
        if (this.onPush) this.onPush(packet);
        else this.early.push(packet);
        continue;
      }
      const item = this.pending.get(packet.sequence);
      if (!item) continue; // 没有等待者的晚应答丢弃，不能当推送派发
      this.pending.delete(packet.sequence);
      if (packet.kind !== PayloadKind.Protobuf || packet.messageId !== item.responseId) {
        this.finish(item, new GateProtocolError(`response ${packet.messageId} (kind ${packet.kind}) does not match the expected ${item.responseId}`), undefined);
      } else {
        this.finish(item, undefined, packet);
      }
    }
  }
}

export function abortReason(signal: AbortSignal): Error {
  return signal.reason instanceof Error ? signal.reason : new GateAbortedError();
}
