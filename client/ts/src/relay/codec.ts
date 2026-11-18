// relay 帧的编解码（docs/framework/RELAY.md §2）：与 Gate 同一个 16 字节 RS v2 包头（大端，共用 src/wire/header.ts），
// flags 的 bits3..4 是通道。
// 规则与 Go 的 client/wire（Header.ValidateRelay、EncodeRelay）一致，字节由 client/spec/relay-packets.json 核对。

import { DEFAULT_MAX_PAYLOAD, RawFrameReader, encodeRaw as encodeHeader } from "../wire/header.js";

export { DEFAULT_MAX_PAYLOAD, HEADER_SIZE, VERSION } from "../wire/header.js";
/** 目标槽位 0：发给全体（不含自己）。 */
export const ALL = 0;
/** 数据帧上归游戏的 flags 位（relay 原样带过）。 */
export const FLAG_PUSH = 1;
export const FLAG_SYNC = 2;
export const FLAG_LOCKSTEP = 4;
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

/**
 * 控制帧操作码。1～15 由 relay 发出，16～31 由房主发出，32～ 任何一端；64～127 是换 host 与前台上报
 * （docs/framework/RELAY.md §2.3）：64～79 由 relay 发出，80～95 由客户端发出。
 */
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
  /** relay → 被换下的旧房主、没在时限内回 HostReady 的候选。载荷 [epoch 8B]。 */
  YouAreMember: 64,
  /** relay → 新房主候选。载荷见 YouAreHost。 */
  YouAreHost: 65,
  /** relay → 成员：它发给房主的游戏帧被丢了（房主不在）。载荷 [epoch 8B]。 */
  HostChanging: 66,
  /** relay → 在线的每个人（含新房主）：新房主已就绪。seq 是新房主槽位，载荷 [epoch 8B][账号]。 */
  HostChanged: 67,
  /** relay → 成员：房主在宽限期内回来了。seq 是房主槽位，载荷 [epoch 8B]。 */
  HostResumed: 68,
  /** relay → 发快照或转交的连接：被拒（连接保留）。载荷见 Rejected。 */
  Rejected: 69,
  /** relay → 当前房主：成员上报的前台状态。seq 是成员槽位，载荷 1 字节：1 前台、0 后台。 */
  MemberPresence: 70,
  /** 房主 → relay：转交给 seq 上的在线成员，载荷是交接数据。 */
  Transfer: 80,
  /** 任何人 → relay：离开房间，relay 回 Closed{left} 后断开。 */
  Leave: 81,
  /** 房主 → relay：候选顺序，每个槽位 4B。 */
  Successors: 82,
  /** 房主 → relay：[seq 8B][快照字节]。 */
  Snapshot: 83,
  /** 新房主候选 → relay：[epoch 8B]，等于 YouAreHost 里的 epoch。 */
  HostReady: 84,
  /**
   * 任何成员 → relay：自己在前台（1）还是后台（0），1 字节，别的长度或取值被 relay 当协议错误断开。relay 转给当前房主
   * （MemberPresence）；没有已就绪的房主、或发送者就是房主时丢弃。
   */
  Presence: 85,
} as const;

/** Successors 里槽位个数的上限。 */
export const MAX_SUCCESSORS = 256;

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
  /** lobby 验权不让进（被踢、席位已让出、房间已关）：不重连。 */
  LobbyRejected = 15,
  /** 向 lobby 验权失败或超时：稍后可以重试。 */
  LobbyUnavailable = 16,
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

/** Welcome：自己的槽位（帧的 seq）、是不是房主、房主槽位（选人阶段为 0）、房间此刻的 host_epoch。 */
export interface Welcome {
  slot: number;
  host: boolean;
  hostSlot: number;
  epoch: bigint;
}

/** 成员上下线（只有房主收到）。上线时 reason 是 None（新加入）或 Replaced（同一账号重连，槽位不变）。 */
export interface Member {
  slot: number;
  account: string;
  reason: RelayReason;
}

/** 房主收到的成员前台上报（MemberPresence）：slot 是成员槽位，foreground 为 false 表示后台。成员可以谎报，只作排候选顺序的参考。 */
export interface MemberPresence {
  slot: number;
  foreground: boolean;
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
  return encodeHeader(flags, msgId, slot, payload);
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
  private readonly reader: RawFrameReader;

  constructor(maxPayload = DEFAULT_MAX_PAYLOAD) {
    this.reader = new RawFrameReader(
      (flags, msgId, size) => validate(flags, msgId, size, maxPayload),
      () => new RelayProtocolError("invalid RS magic or version"),
    );
  }

  get buffered(): number {
    return this.reader.buffered;
  }

  push(chunk: Uint8Array): RelayFrame[] {
    return this.reader.push(chunk).map((raw) => ({
      channel: (raw.flags >> CHANNEL_SHIFT) & 3,
      slot: raw.seq,
      msgId: raw.msgId,
      flags: raw.flags & GAME_BITS,
      payload: raw.payload,
    }));
  }
}

/**
 * Welcome 载荷：[角色 1 字节：1 房主、2 成员][房主槽位 4 字节大端][epoch 8 字节大端]，固定 13 字节。选人阶段进房的连接
 * 房主槽位为 0、epoch 是新的一轮。
 */
export function decodeWelcome(slot: number, payload: Uint8Array): Welcome {
  if (payload.length !== 13 || (payload[0] !== 1 && payload[0] !== 2)) {
    throw new RelayProtocolError("invalid welcome");
  }
  const view = new DataView(payload.buffer, payload.byteOffset);
  return { slot, host: payload[0] === 1, hostSlot: view.getUint32(1), epoch: view.getBigUint64(5) };
}

export function encodeWelcome(welcome: Welcome): Uint8Array {
  const payload = new Uint8Array(13);
  payload[0] = welcome.host ? 1 : 2;
  const view = new DataView(payload.buffer);
  view.setUint32(1, welcome.hostSlot);
  view.setBigUint64(5, welcome.epoch);
  return payload;
}

/** Presence 与 MemberPresence 的载荷：1 字节，1 前台、0 后台。 */
export function encodePresence(foreground: boolean): Uint8Array {
  return Uint8Array.of(foreground ? 1 : 0);
}

export function decodePresence(payload: Uint8Array): boolean {
  if (payload.length !== 1 || payload[0] > 1) throw new RelayProtocolError("invalid presence");
  return payload[0] === 1;
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

// ---- 换 host 的控制帧（docs/framework/RELAY.md §2.3；Go 的 client/wire/relay.go）。整数都是大端。----

/** YouAreHost 里状态字节的来源。 */
export enum StateSource {
  /** 没有可交的状态：新房主用自己本地的状态，或由游戏决定重开。 */
  None = 0,
  /** 主动转交时旧房主随 Transfer 带来的交接数据。 */
  Handoff = 1,
  /** relay 存的最新快照（房主离开或断线）。 */
  Snapshot = 2,
}

/** YouAreHost 里的一个座位：房间存续期间进过房、没被踢的账号，含离线的。 */
export interface Seat {
  slot: number;
  account: string;
  online: boolean;
}

/** YouAreHost 的载荷。snapshotSeq / snapshotAgeMs 只在来源为快照时有值（其余为 0）；state 是原样的字节（游戏压缩过的）。 */
export interface YouAreHost {
  epoch: bigint;
  source: StateSource;
  snapshotSeq: bigint;
  snapshotAgeMs: number;
  members: Seat[];
  state: Uint8Array;
}

/** Rejected 的原因。 */
export enum RejectCode {
  /** 快照或交接数据超过 snapshot_max_bytes。 */
  TooBig = 1,
  /** 离上一份快照不到 snapshot_min_interval。 */
  TooSoon = 2,
  /** 快照 seq 不大于 relay 存着的那一份。 */
  StaleSeq = 3,
  /** 转交的目标不在线，或是房主自己。 */
  BadTarget = 4,
  /** 发送者此刻不是房主。 */
  NotHost = 5,
  /** 转交带的 epoch 不是此刻的房主任期（在旧任期发起，期间房主换过，例如转出去又转回来）。 */
  StaleEpoch = 6,
}

/** Rejected 的载荷：被拒的操作码、原因、被拒快照的 seq（转交为 0）。 */
export interface Rejected {
  op: number;
  code: RejectCode;
  seq: bigint;
}

function view(payload: Uint8Array): DataView {
  return new DataView(payload.buffer, payload.byteOffset, payload.byteLength);
}

/** 只带 epoch 的载荷：YouAreMember、HostChanging、HostResumed、HostReady。 */
export function encodeEpoch(epoch: bigint): Uint8Array {
  const payload = new Uint8Array(8);
  view(payload).setBigUint64(0, epoch);
  return payload;
}

export function decodeEpoch(payload: Uint8Array): bigint {
  if (payload.length !== 8) throw new RelayProtocolError("invalid epoch payload");
  return view(payload).getBigUint64(0);
}

/** HostChanged 的载荷：[epoch 8B][新房主账号 UTF-8]。新房主槽位在帧的 seq。 */
export function encodeHostChanged(epoch: bigint, account: string): Uint8Array {
  const name = new TextEncoder().encode(account);
  const payload = new Uint8Array(8 + name.length);
  view(payload).setBigUint64(0, epoch);
  payload.set(name, 8);
  return payload;
}

export function decodeHostChanged(payload: Uint8Array): { epoch: bigint; account: string } {
  if (payload.length < 9) throw new RelayProtocolError("invalid host changed payload");
  return { epoch: view(payload).getBigUint64(0), account: new TextDecoder().decode(payload.subarray(8)) };
}

/**
 * YouAreHost 的载荷：[epoch 8B][来源 1B][快照 seq 8B][快照年龄 ms 4B][成员数 2B]，
 * 成员数 × {[槽位 4B][在线 1B][账号长度 2B][账号]}，最后是状态字节（剩下的全部）。
 */
export function encodeYouAreHost(host: YouAreHost): Uint8Array {
  const names = host.members.map((m) => new TextEncoder().encode(m.account));
  const size = 23 + names.reduce((n, name) => n + 7 + name.length, 0) + host.state.length;
  const payload = new Uint8Array(size);
  const v = view(payload);
  v.setBigUint64(0, host.epoch);
  payload[8] = host.source;
  v.setBigUint64(9, host.snapshotSeq);
  v.setUint32(17, host.snapshotAgeMs >>> 0);
  v.setUint16(21, host.members.length);
  let at = 23;
  host.members.forEach((m, i) => {
    v.setUint32(at, m.slot >>> 0);
    payload[at + 4] = m.online ? 1 : 0;
    v.setUint16(at + 5, names[i].length);
    payload.set(names[i], at + 7);
    at += 7 + names[i].length;
  });
  payload.set(host.state, at);
  return payload;
}

export function decodeYouAreHost(payload: Uint8Array): YouAreHost {
  if (payload.length < 23 || payload[8] > StateSource.Snapshot) throw new RelayProtocolError("invalid you_are_host");
  const v = view(payload);
  const count = v.getUint16(21);
  const members: Seat[] = [];
  let at = 23;
  for (let i = 0; i < count; i++) {
    if (payload.length - at < 7 || payload[at + 4] > 1) throw new RelayProtocolError("invalid you_are_host");
    const n = v.getUint16(at + 5);
    if (payload.length - at < 7 + n) throw new RelayProtocolError("invalid you_are_host");
    members.push({ slot: v.getUint32(at), online: payload[at + 4] === 1, account: new TextDecoder().decode(payload.subarray(at + 7, at + 7 + n)) });
    at += 7 + n;
  }
  return {
    epoch: v.getBigUint64(0),
    source: payload[8] as StateSource,
    snapshotSeq: v.getBigUint64(9),
    snapshotAgeMs: v.getUint32(17),
    members,
    state: payload.slice(at),
  };
}

/**
 * Transfer 的载荷：[epoch 8B][交接数据]。epoch 是房主发起转交时的任期，relay 只在它等于此刻的 epoch 时执行，
 * 否则回 Rejected{StaleEpoch}（旧任期的交接数据不开启新任期，见 docs/framework/RELAY.md §2.3）。
 */
export function encodeTransfer(epoch: bigint, data: Uint8Array): Uint8Array {
  const payload = new Uint8Array(8 + data.length);
  view(payload).setBigUint64(0, epoch);
  payload.set(data, 8);
  return payload;
}

export function decodeTransfer(payload: Uint8Array): { epoch: bigint; data: Uint8Array } {
  if (payload.length < 8) throw new RelayProtocolError("invalid transfer payload");
  return { epoch: view(payload).getBigUint64(0), data: payload.subarray(8) };
}

/** Snapshot 的载荷：[seq 8B][快照字节]。 */
export function encodeSnapshot(seq: bigint, data: Uint8Array): Uint8Array {
  const payload = new Uint8Array(8 + data.length);
  view(payload).setBigUint64(0, seq);
  payload.set(data, 8);
  return payload;
}

export function decodeSnapshot(payload: Uint8Array): { seq: bigint; data: Uint8Array } {
  if (payload.length < 8) throw new RelayProtocolError("invalid snapshot payload");
  return { seq: view(payload).getBigUint64(0), data: payload.subarray(8) };
}

/** Successors 的载荷：槽位列表，每个 4B，至多 MAX_SUCCESSORS 个。 */
export function encodeSuccessors(slots: readonly number[]): Uint8Array {
  if (slots.length > MAX_SUCCESSORS) throw new RelayProtocolError(`at most ${MAX_SUCCESSORS} successors`);
  const payload = new Uint8Array(4 * slots.length);
  slots.forEach((slot, i) => view(payload).setUint32(4 * i, slot >>> 0));
  return payload;
}

export function decodeSuccessors(payload: Uint8Array): number[] {
  if (payload.length % 4 !== 0 || payload.length / 4 > MAX_SUCCESSORS) throw new RelayProtocolError("invalid successors payload");
  return Array.from({ length: payload.length / 4 }, (_, i) => view(payload).getUint32(4 * i));
}

/** Rejected 的载荷：[被拒的操作码 1B][原因 1B][seq 8B]。 */
export function encodeRejected(rejected: Rejected): Uint8Array {
  const payload = new Uint8Array(10);
  payload[0] = rejected.op;
  payload[1] = rejected.code;
  view(payload).setBigUint64(2, rejected.seq);
  return payload;
}

export function decodeRejected(payload: Uint8Array): Rejected {
  if (payload.length !== 10) throw new RelayProtocolError("invalid rejected payload");
  return { op: payload[0], code: payload[1] as RejectCode, seq: view(payload).getBigUint64(2) };
}
