// relay 帧的编解码（docs/framework/RELAY.md §2）：与 Gate 同一个 16 字节 RS v2 包头（大端），flags 的 bits3..4 是通道。
// 规则与 Go 的 client/wire（Header.ValidateRelay、EncodeRelay）一致，字节由 client/spec/relay-packets.json 核对。

export const HEADER_SIZE = 16;
export const VERSION = 2;
/** 目标槽位 0：发给全体（不含自己）。 */
export const ALL = 0;
/** 数据帧上归游戏的 flags 位（relay 原样带过）。 */
export const FLAG_PUSH = 1;
export const FLAG_SYNC = 2;
export const FLAG_LOCKSTEP = 4;
export const DEFAULT_MAX_PAYLOAD = 1 << 20;
const GAME_BITS = FLAG_PUSH | FLAG_SYNC | FLAG_LOCKSTEP;
const CHANNEL_SHIFT = 3;

/** 通道：flags 的 bits3..4。 */
export enum RelayChannel {
  /** 不是 relay 帧：只有连接上的第一帧（令牌）。 */
  None = 0,
  /** 游戏通道：成员的帧只到房主，房主的帧按目标转出。 */
  Game = 1,
  /** 房间通道：聊天、表情、语音，按目标直达，房主也是收件人。 */
  Room = 2,
  /** 控制帧：msgId 是操作码。 */
  Control = 3,
}

/** 控制帧操作码。1～15 由 relay 发出，16～31 由房主发出，32～ 任何一端；64～127 留给换 host。 */
export const RelayOp = {
  Welcome: 1,
  MemberJoined: 2,
  MemberLeft: 3,
  Closed: 4,
  Muted: 5,
  Kick: 16,
  Mute: 17,
  Ping: 32,
  Pong: 33,
} as const;

/** Ping 载荷上限；超出的 Ping 被 relay 当协议错误断开。 */
export const MAX_PING_BYTES = 16;

/** Closed 与 MemberLeft 的原因码。 */
export enum RelayReason {
  None = 0,
  Disconnected = 1,
  Kicked = 2,
  Replaced = 3,
  RoomClosed = 4,
  FrameTooBig = 5,
  RateLimited = 6,
  SlowConsumer = 7,
  IdleTimeout = 8,
  Protocol = 9,
  TokenRejected = 10,
  RoomNotFound = 11,
  RoomFull = 12,
  Shutdown = 13,
  Left = 14,
}

/**
 * 一帧 relay 帧。slot：收到的数据帧是发送者槽位，要发的数据帧是目标槽位（ALL 为全体；成员的游戏帧只到房主，目标不看）；
 * 控制帧是控制对象的槽位。msgId：数据帧上由游戏自用，控制帧上是操作码。flags：数据帧上归游戏的 bits0..2。
 */
export interface RelayFrame {
  channel: RelayChannel;
  slot: number;
  msgId: number;
  flags: number;
  payload: Uint8Array;
}

/** Welcome：自己的槽位（帧的 seq）、是不是房主、房主槽位。 */
export interface Welcome {
  slot: number;
  host: boolean;
  hostSlot: number;
}

/** 成员上下线（只有房主收到）。上线时 reason 是 None（新加入）或 Replaced（同一账号重连，槽位不变）。 */
export interface Member {
  slot: number;
  account: string;
  reason: RelayReason;
}

export class RelayProtocolError extends Error {
  constructor(message: string) {
    super(`relay protocol: ${message}`);
    this.name = "RelayProtocolError";
  }
}

/** 与 Go 的 ValidateRelay 相同：保留位 5..7 为 0、Sync 与 Lockstep 不同时置位、控制帧只有通道位且操作码非 0、令牌帧 flags 为 0。 */
function validate(flags: number, msgId: number, size: number, maxPayload: number): void {
  if ((flags & 0xe0) !== 0 || (flags & (FLAG_SYNC | FLAG_LOCKSTEP)) === (FLAG_SYNC | FLAG_LOCKSTEP)) {
    throw new RelayProtocolError(`invalid flags ${flags}`);
  }
  const channel = (flags >> CHANNEL_SHIFT) & 3;
  if (channel === RelayChannel.None && msgId === 0 && flags !== 0) {
    throw new RelayProtocolError("token frame carries flags");
  }
  if (channel === RelayChannel.Control && (msgId === 0 || flags !== RelayChannel.Control << CHANNEL_SHIFT)) {
    throw new RelayProtocolError("invalid control frame");
  }
  if (size > (maxPayload > 0 ? maxPayload : DEFAULT_MAX_PAYLOAD)) {
    throw new RelayProtocolError(`payload ${size} exceeds the limit`);
  }
}

function encodeRaw(flags: number, msgId: number, slot: number, payload: Uint8Array, maxPayload: number): Uint8Array {
  validate(flags, msgId, payload.length, maxPayload);
  const data = new Uint8Array(HEADER_SIZE + payload.length);
  const view = new DataView(data.buffer);
  data[0] = 0x52; // R
  data[1] = 0x53; // S
  data[2] = VERSION;
  data[3] = flags;
  view.setUint32(4, msgId >>> 0);
  view.setUint32(8, slot >>> 0);
  view.setUint32(12, payload.length);
  data.set(payload, HEADER_SIZE);
  return data;
}

/** 编一帧：数据帧带 flags 的游戏位，控制帧只带通道位。 */
export function encodeFrame(frame: Omit<RelayFrame, "flags"> & { flags?: number }, maxPayload = DEFAULT_MAX_PAYLOAD): Uint8Array {
  let flags: number;
  switch (frame.channel) {
    case RelayChannel.Game:
    case RelayChannel.Room:
      flags = (frame.channel << CHANNEL_SHIFT) | ((frame.flags ?? 0) & GAME_BITS);
      break;
    case RelayChannel.Control:
      flags = RelayChannel.Control << CHANNEL_SHIFT;
      break;
    default:
      throw new RelayProtocolError("frame channel must be game, room or control");
  }
  return encodeRaw(flags, frame.msgId, frame.slot, frame.payload, maxPayload);
}

/** 令牌首帧：message_id=0、flags=0，载荷是 lobby 签的 rt1 令牌原文。 */
export function encodeToken(token: string): Uint8Array {
  const payload = new TextEncoder().encode(token);
  return encodeRaw(0, 0, 0, payload, Math.max(payload.length, 1));
}

/** 解恰好一帧。 */
export function decodeFrame(data: Uint8Array, maxPayload = DEFAULT_MAX_PAYLOAD): RelayFrame {
  const reader = new FrameReader(maxPayload);
  const frames = reader.push(data);
  if (frames.length !== 1 || reader.buffered !== 0) {
    throw new RelayProtocolError("expected exactly one frame");
  }
  return frames[0];
}

/**
 * 字节流 → 帧。WebSocket 上 relay 每条消息恰好一帧，但这里按流读：一条消息里几帧、一帧拆在几条消息里都能解，
 * 不依赖消息边界（TCP 与 WebSocket 同一套解码）。
 */
export class FrameReader {
  private buffer: Uint8Array = new Uint8Array(0);

  constructor(private readonly maxPayload = DEFAULT_MAX_PAYLOAD) {}

  get buffered(): number {
    return this.buffer.length;
  }

  push(chunk: Uint8Array): RelayFrame[] {
    if (this.buffer.length === 0) {
      this.buffer = chunk;
    } else {
      const joined = new Uint8Array(this.buffer.length + chunk.length);
      joined.set(this.buffer);
      joined.set(chunk, this.buffer.length);
      this.buffer = joined;
    }
    const frames: RelayFrame[] = [];
    while (this.buffer.length >= HEADER_SIZE) {
      const data = this.buffer;
      if (data[0] !== 0x52 || data[1] !== 0x53 || data[2] !== VERSION) {
        throw new RelayProtocolError("invalid RS magic or version");
      }
      const view = new DataView(data.buffer, data.byteOffset, data.byteLength);
      const flags = data[3];
      const msgId = view.getUint32(4);
      const size = view.getUint32(12);
      validate(flags, msgId, size, this.maxPayload); // 在等载荷之前检查
      if (data.length < HEADER_SIZE + size) {
        break;
      }
      frames.push({
        channel: (flags >> CHANNEL_SHIFT) & 3,
        slot: view.getUint32(8),
        msgId,
        flags: flags & GAME_BITS,
        payload: data.slice(HEADER_SIZE, HEADER_SIZE + size),
      });
      this.buffer = data.subarray(HEADER_SIZE + size);
    }
    return frames;
  }
}

/** Welcome 载荷：[角色 1 字节：1 房主、2 成员][房主槽位 4 字节大端]。 */
export function decodeWelcome(slot: number, payload: Uint8Array): Welcome {
  if (payload.length !== 5 || (payload[0] !== 1 && payload[0] !== 2)) {
    throw new RelayProtocolError("invalid welcome");
  }
  return { slot, host: payload[0] === 1, hostSlot: new DataView(payload.buffer, payload.byteOffset).getUint32(1) };
}

export function encodeWelcome(welcome: Welcome): Uint8Array {
  const payload = new Uint8Array(5);
  payload[0] = welcome.host ? 1 : 2;
  new DataView(payload.buffer).setUint32(1, welcome.hostSlot);
  return payload;
}

/** MemberJoined / MemberLeft 载荷：[原因 1 字节][账号 UTF-8，至少 1 字节]。 */
export function decodeMember(slot: number, payload: Uint8Array): Member {
  if (payload.length < 2) {
    throw new RelayProtocolError("invalid member notice");
  }
  return { slot, reason: payload[0], account: new TextDecoder().decode(payload.subarray(1)) };
}

export function encodeMember(member: Member): Uint8Array {
  const account = new TextEncoder().encode(member.account);
  const payload = new Uint8Array(1 + account.length);
  payload[0] = member.reason;
  payload.set(account, 1);
  return payload;
}
