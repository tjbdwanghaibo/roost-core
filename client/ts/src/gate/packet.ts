// 玩家接入（Gate）的包：RS v2 包头（src/wire/header.ts）上的 Gate 规则，与 Go 的 client/wire（Header.Validate）、
// C# 的 PacketCodec 相同，字节由 client/spec/packets.json 核对：
//   - flags：bit0 推送，bits1..2 载荷类型（0 Protobuf、1 Sync、2 Lockstep，3 保留），bits3..7 必须为 0；
//   - msgId 0 是 Gate 控制帧（鉴权首包、心跳、校时），flags 必须为 0；
//   - 载荷不超过 maxPayload（缺省 1 MiB，与服务端 max_payload_bytes 一致）。

import { DEFAULT_MAX_PAYLOAD, HEADER_SIZE, RawFrameReader, encodeRaw } from "../wire/header.js";

/** 载荷类型（flags 的 bits1..2）。 */
export enum PayloadKind {
  Protobuf = 0,
  Sync = 1,
  Lockstep = 2,
}

/** 一个 Gate 包。payload 是独立副本，不与接收缓冲共用内存。 */
export interface Packet {
  messageId: number;
  sequence: number;
  isPush: boolean;
  kind: PayloadKind;
  payload: Uint8Array;
}

/** 线上收到的字节不合 Gate 帧规则（坏包头、文本消息、应答与请求对不上……）；SDK 随即断开这条连接。 */
export class GateProtocolError extends Error {
  constructor(message: string) {
    super(`gate protocol: ${message}`);
    this.name = "GateProtocolError";
  }
}

function checkHeader(flags: number, msgId: number, size: number, maxPayload: number): void {
  if ((flags & ~7) !== 0 || (flags & 6) === 6) {
    throw new GateProtocolError(`invalid flags ${flags}`);
  }
  if (msgId === 0 && flags !== 0) {
    throw new GateProtocolError("control frame (message id 0) carries flags");
  }
  if (size > (maxPayload > 0 ? maxPayload : DEFAULT_MAX_PAYLOAD)) {
    throw new GateProtocolError(`payload ${size} exceeds the limit`);
  }
}

/** 编一个包；规则不合（未知类型、控制帧带业务位、超上限）时抛 GateProtocolError，不写出半包。 */
export function encodePacket(packet: Packet, maxPayload = DEFAULT_MAX_PAYLOAD): Uint8Array {
  if (packet.kind !== PayloadKind.Protobuf && packet.kind !== PayloadKind.Sync && packet.kind !== PayloadKind.Lockstep) {
    throw new GateProtocolError(`unknown payload kind ${packet.kind}`);
  }
  if (packet.messageId === 0 && (packet.isPush || packet.kind !== PayloadKind.Protobuf)) {
    throw new GateProtocolError("control frame (message id 0) carries flags");
  }
  const flags = (packet.isPush ? 1 : 0) | (packet.kind << 1);
  checkHeader(flags, packet.messageId, packet.payload.length, maxPayload);
  return encodeRaw(flags, packet.messageId, packet.sequence, packet.payload);
}

/** 解恰好一个包（多出或缺少字节都拒绝）。 */
export function decodePacket(data: Uint8Array, maxPayload = DEFAULT_MAX_PAYLOAD): Packet {
  if (data.length < HEADER_SIZE) throw new GateProtocolError("truncated header");
  const reader = new GateFrameReader(maxPayload);
  const packets = reader.push(data);
  if (packets.length !== 1 || reader.buffered !== 0) {
    throw new GateProtocolError("incomplete or trailing packet bytes");
  }
  return packets[0];
}

/** 字节流 → 包：一条 WebSocket 消息里几帧、一帧拆在几条消息里都能拼（GATEWAY §6 只保证服务端写出的每帧恰好一条消息）。 */
export class GateFrameReader {
  private readonly reader: RawFrameReader;

  constructor(maxPayload = DEFAULT_MAX_PAYLOAD) {
    this.reader = new RawFrameReader(
      (flags, msgId, size) => checkHeader(flags, msgId, size, maxPayload),
      () => new GateProtocolError("invalid RS magic or version"),
    );
  }

  get buffered(): number {
    return this.reader.buffered;
  }

  push(chunk: Uint8Array): Packet[] {
    return this.reader.push(chunk).map((raw) => ({
      messageId: raw.msgId,
      sequence: raw.seq,
      isPush: (raw.flags & 1) !== 0,
      kind: ((raw.flags >> 1) & 3) as PayloadKind,
      payload: raw.payload,
    }));
  }
}
