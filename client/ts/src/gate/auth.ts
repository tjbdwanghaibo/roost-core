// 鉴权首包（message_id=0 的第一帧）的载荷：带连接代号的信封 0x00、'G'、8 字节大端代号（非 0）、票据原文
// （client/README“鉴权首包的连接代号”，Go 的 client/wire.EncodeAuth，C# 的 AuthPayload）。同一 SessionID 的两条连接落在
// 不同 Gate 实例时，Game 按代号认出迟到完成的旧握手，不让它断开更新的连接；服务端在鉴权前剥掉信封，业务鉴权只看到票据原文。
// Connection 每次连接（包括自动重连）自动取新代号并写成信封，调用方不需要直接用这里。

import { GateProtocolError } from "./packet.js";

const OVERHEAD = 2 + 8;

/** 票据上限：服务端缺省的鉴权首包上限 8 KiB（gateway TCPConfig.MaxHandshakeBytes）减去 10 字节信封。 */
export const MAX_TICKET_BYTES = 8182;

/** 写出信封；票据为空或以 0x00 开头（服务端会把它当成坏信封）、代号为 0 或超出 64 位时抛错。 */
export function encodeAuth(ticket: Uint8Array, generation: bigint): Uint8Array {
  if (ticket.length === 0 || ticket[0] === 0) {
    throw new RangeError("gate: ticket must be non-empty and must not start with a NUL byte");
  }
  if (generation <= 0n || generation > 0xffff_ffff_ffff_ffffn) {
    throw new RangeError("gate: connection generation must be a non-zero unsigned 64-bit value");
  }
  const payload = new Uint8Array(OVERHEAD + ticket.length);
  payload[0] = 0;
  payload[1] = 0x47; // 'G'
  new DataView(payload.buffer).setBigUint64(2, generation);
  payload.set(ticket, OVERHEAD);
  return payload;
}

/**
 * 拆开鉴权首包：信封返回票据原文与代号；不以 0x00 开头的是票据原文、代号为 0n；以 0x00 开头却不是完整信封（标记不对、
 * 代号为 0、票据为空或以 0x00 开头）时抛 GateProtocolError，与服务端拒绝规则一致。
 */
export function decodeAuth(payload: Uint8Array): { ticket: Uint8Array; generation: bigint } {
  if (payload.length === 0) throw new GateProtocolError("empty authentication payload");
  if (payload[0] !== 0) return { ticket: payload.slice(), generation: 0n };
  if (payload.length <= OVERHEAD || payload[1] !== 0x47) throw new GateProtocolError("malformed authentication envelope");
  const generation = new DataView(payload.buffer, payload.byteOffset, payload.byteLength).getBigUint64(2);
  if (generation === 0n || payload[OVERHEAD] === 0) throw new GateProtocolError("malformed authentication envelope");
  return { ticket: payload.slice(OVERHEAD), generation };
}

let lastGeneration = 0n;

/**
 * 新的连接代号：max(上一个 + 1, Unix 毫秒 × 1000000)，上一个只存在本页面内存。页面内严格递增、不受时钟回拨影响；刷新页面后
 * 从当时的时钟起算，自然大于刷新前发过的代号（与 C# 的 Unix 纳秒同一量纲，浏览器只有毫秒精度，见 client/README）。now 只给
 * 测试注入。
 */
export function nextConnectionGeneration(now: () => number = Date.now): bigint {
  const wall = BigInt(Math.max(0, Math.floor(now()))) * 1_000_000n;
  const next = lastGeneration + 1n > wall ? lastGeneration + 1n : wall;
  lastGeneration = next;
  return next;
}
