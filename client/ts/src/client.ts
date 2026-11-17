// relay 的 WebSocket 连接（方案 FEATURE-2026-11-16-lobby-relay §4.7.5；契约 docs/framework/RELAY.md）。
// 每条 WebSocket 二进制消息发一帧 client/wire 帧；收的时候按字节流解（FrameReader），不依赖消息边界。

import {
  DEFAULT_MAX_PAYLOAD,
  FrameReader,
  MAX_PING_BYTES,
  RelayChannel,
  RelayOp,
  RelayProtocolError,
  RelayReason,
  StateSource,
  decodeEpoch,
  decodeHostChanged,
  decodeMember,
  decodePresence,
  decodeRejected,
  decodeWelcome,
  decodeYouAreHost,
  encodeEpoch,
  encodeFrame,
  encodePresence,
  encodeSnapshot,
  encodeSuccessors,
  encodeToken,
  type Member,
  type MemberPresence,
  type Rejected,
  type RelayFrame,
  type Welcome,
  type YouAreHost,
} from "./codec.js";
import { MAX_STATE_BYTES, defaultCompressor, packState, unpackState, type Compressor } from "./compress.js";
import type { LobbyClient } from "./lobby.js";
import { LobbyError } from "./lobby.js";

/** 浏览器的 WebSocket 与 node 的 ws 包都满足的最小接口。 */
export interface WebSocketLike {
  binaryType: string;
  readonly readyState: number;
  send(data: Uint8Array): void;
  close(code?: number, reason?: string): void;
  onopen: ((event: unknown) => void) | null;
  onmessage: ((event: { data: unknown }) => void) | null;
  onclose: ((event: unknown) => void) | null;
  onerror: ((event: unknown) => void) | null;
}

export interface ReconnectPolicy {
  /** 0 不自动重连（缺省）。 */
  maxAttempts: number;
  minBackoffMs?: number;
  maxBackoffMs?: number;
}

export interface RelayClientOptions {
  /** 房间的游戏，重连调 lobby 的 join 时用。 */
  game: string;
  /** 房间密码，重连时带上：席位已被让出（预留过期）的人重新加入要它。 */
  password?: string;
  /** 建 WebSocket；缺省 `new globalThis.WebSocket(url)`（浏览器）。node 里传 ws 包：`url => new WebSocket(url)`。 */
  webSocket?: (url: string) => WebSocketLike;
  /** 一次连接（握手、发令牌、等 Welcome）的期限，缺省 10s。 */
  connectTimeoutMs?: number;
  /** 心跳间隔，缺省 10s（relay 的 idle_timeout 缺省 30s，取三分之一）。 */
  pingIntervalMs?: number;
  /** 这么久一帧都没收到就当断线，缺省 30s；要大于 pingIntervalMs。 */
  readTimeoutMs?: number;
  /** 收帧的载荷上限，缺省 1 MiB；要不小于 relay 的 max_payload_bytes。 */
  maxPayloadBytes?: number;
  /**
   * relay 的 snapshot_max_bytes，缺省 256 KiB。快照与交接数据（压缩后）超过它本地就拒绝；收帧上限至少放到它加 64 KiB
   * （成员名单），装得下带满快照的 YouAreHost（它可以比 max_payload_bytes 大）。
   */
  snapshotMaxBytes?: number;
  /** 当房主（含等 HostReady 的候选）时的心跳间隔，缺省 600ms：relay 的 host_idle_timeout 缺省 2s，取三分之一不到。 */
  hostPingIntervalMs?: number;
  /**
   * 快照与交接数据的压缩，缺省 defaultCompressor()（raw deflate）。状态字节带压缩方式标识（Compressor.id）：内置的
   * 不压缩与 raw deflate 谁都解得开；游戏自带的压缩方式，房间里每个客户端都要配上。
   */
  compressor?: Compressor;
  /** 当房主时定时推快照：take 返回当前状态（压缩前），每 intervalMs（缺省 3s，0 只手动推）推一次。 */
  snapshot?: SnapshotOptions;
  /**
   * 两份快照之间的最小间隔，缺省 1000ms（relay.snapshot_min_interval 的缺省）：更密的 pushSnapshot 不发，
   * 抛 SnapshotTooSoonError。当上房主时重新计算（relay 也是换了房主重新算）。
   */
  snapshotMinIntervalMs?: number;
  /**
   * true 时 onYouAreHost 的回调返回（返回 Promise 的等它完成）后 SDK 自动回 HostReady；缺省 false：游戏准备好后自己调
   * hostReady()（与 Go 的 Options.AutoHostReady 相同）。
   */
  autoHostReady?: boolean;
  /** 断线后自动重连，缺省关闭。 */
  reconnect?: ReconnectPolicy;
}

export interface SnapshotOptions {
  /** 当前要交出去的状态（压缩前）；返回 undefined 表示这一次不推。 */
  take: () => Uint8Array | undefined | Promise<Uint8Array | undefined>;
  /** 缺省 3000；0 表示只手动推（pushSnapshot）。relay 拒收间隔小于 snapshot_min_interval（缺省 1s）的快照。 */
  intervalMs?: number;
}

/** 自己在换 host 里的角色：成员、等 HostReady 的候选、房主。 */
export type HostRole = "member" | "candidate" | "host";

/** onYouAreHost 的参数：YouAreHost 解好的字段，state 换成解压后的状态（来源为 None 或解压失败时为 null）。 */
export interface HostTakeover extends Omit<YouAreHost, "state"> {
  state: Uint8Array | null;
  /** relay 交来的原始字节（压缩过的）。 */
  rawState: Uint8Array;
  /** 解压失败的原因；成功或没有状态时为 undefined。 */
  stateError?: Error;
}

/** HostChanged：新房主已就绪。self 为 true 表示新房主就是自己。 */
export interface HostChange {
  epoch: bigint;
  slot: number;
  account: string;
  self: boolean;
}

/** 推出去的一份快照（压缩后的字节）：sessionStorage 辅助据此存档。 */
export interface SentSnapshot {
  seq: bigint;
  data: Uint8Array;
}

/**
 * 一条连接结束的通知。reconnecting 为 true 表示正在重连，之后会有 onReconnected，或者一条 reconnecting 为 false 的通知；
 * reconnecting 为 false 的是最后一条。reason 是 relay 的 Closed 原因（没收到 Closed 时是 Disconnected）；本地 close 时是 None。
 */
export interface Disconnect {
  reason: RelayReason;
  error?: Error;
  reconnecting: boolean;
}

/**
 * SDK 自带辅助（web.ts 的 WebHost）用的内部监听，不从 index 导出。每个 onXxx 只有一个业务回调槽位（与 Go、C# 相同），
 * 辅助要是也用这些槽位，业务再注册一次就把辅助顶掉了（2026-11-17 演示页上转交后新房主规则不跑就是这么来的）。
 * 内部监听在业务回调之前调用，互不覆盖。
 */
export const internalHooks = Symbol("roost.relay.internalHooks");

/**
 * WebHost 自动排候选顺序用的内部入口（不从 index 导出）：和 setSuccessors 一样发 Successors，但不算“游戏显式声明过”，
 * 所以之后游戏自己调 setSuccessors 时以游戏为准。
 */
export const internalSuccessors = Symbol("roost.relay.internalSuccessors");

export interface InternalHooks {
  game?: (frame: RelayFrame) => void;
  memberJoined?: (member: Member) => void;
  memberLeft?: (member: Member) => void;
  memberPresence?: (presence: MemberPresence) => void;
  youAreHost?: (takeover: HostTakeover) => void | Promise<void>;
  youAreMember?: (epoch: bigint) => void;
  hostChanged?: (change: HostChange) => void;
  snapshotSent?: (sent: SentSnapshot) => void;
  reconnected?: (welcome: Welcome) => void;
  disconnected?: (disconnect: Disconnect) => void;
}

/** pushSnapshot 离上一份不到 snapshotMinIntervalMs，这一份没有发出。 */
export class SnapshotTooSoonError extends Error {
  constructor() {
    super("relay: snapshot sent too soon after the previous one");
    this.name = "SnapshotTooSoonError";
  }
}

/** relay 发来 Closed 之后断开：令牌被拒、房间不存在、满员、被踢、房间关闭等。 */
export class RelayClosedError extends Error {
  constructor(readonly reason: RelayReason) {
    super(`relay: closed by relay: ${RelayReason[reason] ?? reason}`);
    this.name = "RelayClosedError";
  }
}

export class RelayClientClosedError extends Error {
  constructor() {
    super("relay: client closed");
    this.name = "RelayClientClosedError";
  }
}

/** 一条已进房的连接。 */
class Session {
  closedReason = RelayReason.None;
  lastReceive = now();
  ended = false;
  timer: ReturnType<typeof setInterval> | undefined;
  /** 当前的帧处理：握手阶段等 Welcome，进房后交给 RelayClient。 */
  onFrame: (frame: RelayFrame) => void = () => {};
  /** 结束时调用一次（onclose，或本地 abort）。 */
  onEnd: (reason: RelayReason, error?: Error) => void = () => {};
  welcome: Welcome = { slot: 0, host: false, hostSlot: 0, epoch: 0n };
  /** Welcome 之后、attach 之前到达的帧（同一条消息或紧接着的消息），attach 时按序处理。 */
  pending: RelayFrame[] = [];
  earlyEnd: { reason: RelayReason; error?: Error } | undefined;

  /** 最近一次发 Ping 的时刻（成员按它定期测 RTT）。 */
  lastPing = now();
  /** 最近一次发帧的时刻：任何一帧都重置 relay 的空闲计时，房主的心跳只在这么久没发东西时补一个 Ping。 */
  lastSend = now();

  constructor(
    readonly ws: WebSocketLike,
    readonly reader: FrameReader,
  ) {}

  send(data: Uint8Array): boolean {
    if (this.ended || this.ws.readyState !== 1) return false;
    try {
      this.ws.send(data);
      this.lastSend = now();
      return true;
    } catch {
      this.abort(RelayReason.Disconnected, new Error("relay: send failed"));
      return false;
    }
  }

  /** 立即结束（不等 WebSocket 的关闭握手）：断线检测、协议错误、本地 close。 */
  abort(reason: RelayReason, error?: Error): void {
    try {
      this.ws.close();
    } catch {
      // 已经关了
    }
    this.end(reason, error);
  }

  end(reason: RelayReason, error?: Error): void {
    if (this.ended) return;
    this.ended = true;
    if (this.timer !== undefined) clearInterval(this.timer);
    this.onEnd(reason, error);
  }
}

function now(): number {
  return globalThis.performance ? globalThis.performance.now() : Date.now();
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

type Handler<T> = ((value: T) => void) | undefined;

/**
 * relay 控制帧的内置处理：操作码 → 处理。返回非 None 时连接结束。
 * 不在表里的操作码只交给 onControl 注册的回调，没有回调就丢弃。换 host 的五个 relay → 客户端操作码（64～69）在这里
 * 改角色状态，再调对应的强类型回调。
 */
const builtinControl: Record<number, (client: RelayClient, frame: RelayFrame, session: Session) => RelayReason> = {
  [RelayOp.Welcome]: (client, frame) => {
    client["welcomed"](decodeWelcome(frame.slot, frame.payload));
    return RelayReason.None;
  },
  [RelayOp.MemberJoined]: (client, frame) => {
    const member = decodeMember(frame.slot, frame.payload);
    client["online"].set(member.slot, member.account);
    client["hooks"].memberJoined?.(member);
    client["handlers"].memberJoined?.(member);
    return RelayReason.None;
  },
  [RelayOp.MemberLeft]: (client, frame) => {
    const member = decodeMember(frame.slot, frame.payload);
    client["online"].delete(member.slot);
    client["hooks"].memberLeft?.(member);
    client["handlers"].memberLeft?.(member);
    return RelayReason.None;
  },
  [RelayOp.YouAreMember]: (client, frame) => {
    const epoch = decodeEpoch(frame.payload);
    client["epochValue"] = epoch;
    client["becomeMember"]();
    client["hooks"].youAreMember?.(epoch);
    client["handlers"].youAreMember?.(epoch);
    return RelayReason.None;
  },
  [RelayOp.YouAreHost]: (client, frame, session) => {
    client["takeOver"](session, decodeYouAreHost(frame.payload));
    return RelayReason.None;
  },
  [RelayOp.HostChanging]: (client, frame) => {
    const epoch = decodeEpoch(frame.payload);
    client["changing"] = true;
    client["handlers"].hostChanging?.(epoch);
    return RelayReason.None;
  },
  [RelayOp.HostChanged]: (client, frame) => {
    const { epoch, account } = decodeHostChanged(frame.payload);
    client["hostChanged"]({ epoch, slot: frame.slot, account, self: frame.slot === client["role"].slot });
    return RelayReason.None;
  },
  [RelayOp.HostResumed]: (client, frame) => {
    const epoch = decodeEpoch(frame.payload);
    client["epochValue"] = epoch;
    client["changing"] = false;
    client["role"] = { ...client["role"], hostSlot: frame.slot };
    client["handlers"].hostResumed?.({ epoch, slot: frame.slot });
    return RelayReason.None;
  },
  [RelayOp.MemberPresence]: (client, frame) => {
    const presence: MemberPresence = { slot: frame.slot, foreground: decodePresence(frame.payload) };
    client["hooks"].memberPresence?.(presence);
    client["handlers"].memberPresence?.(presence);
    return RelayReason.None;
  },
  [RelayOp.Rejected]: (client, frame) => {
    client["handlers"].rejected?.(decodeRejected(frame.payload));
    return RelayReason.None;
  },
  [RelayOp.Closed]: (_client, frame) => {
    if (frame.payload.length !== 1 || frame.payload[0] === 0) throw new RelayProtocolError("invalid closed frame");
    return frame.payload[0] as RelayReason;
  },
  [RelayOp.Muted]: (client, frame) => {
    if (frame.payload.length !== 1 || frame.payload[0] > 1) throw new RelayProtocolError("invalid muted frame");
    client["handlers"].muted?.(frame.payload[0] === 1);
    return RelayReason.None;
  },
  [RelayOp.Pong]: (client, frame) => {
    if (frame.payload.length === 8) {
      // 别的长度不是本 SDK 发的 Ping
      const sentMicros = Number(new DataView(frame.payload.buffer, frame.payload.byteOffset).getBigUint64(0));
      client["rttMs"] = now() - sentMicros / 1000;
    }
    return RelayReason.None;
  },
};

/**
 * 一个玩家在一个房间里的 relay 连接（含断线重连）。回调都在事件循环上同步调用；回调要快。
 *
 * 心跳：每 pingIntervalMs 发一个 Ping，relay 原样回 Pong（测 RTT）；readTimeoutMs 内一帧都没收到就当断线。
 * 断线后重连要重新向 lobby 要令牌：rejoin 调 lobby 的 join（对已在房间里的人幂等）再连 relay；reconnect.maxAttempts
 * 大于 0 时自动按退避重试。只有连接自己断掉或 relay 判空闲超时才重连；被踢、房间关闭、被顶号、经 lobby 离开、
 * relay 停机都不重连。
 */
export class RelayClient {
  private readonly opts: Required<Omit<RelayClientOptions, "password" | "reconnect" | "compressor" | "snapshot">> & {
    password?: string;
    reconnect: Required<ReconnectPolicy>;
    snapshot?: SnapshotOptions;
  };
  private readonly handlers: {
    game?: Handler<RelayFrame>;
    room?: Handler<RelayFrame>;
    memberJoined?: Handler<Member>;
    memberLeft?: Handler<Member>;
    muted?: Handler<boolean>;
    disconnected?: Handler<Disconnect>;
    reconnected?: Handler<Welcome>;
    youAreHost?: (takeover: HostTakeover) => void | Promise<void>;
    youAreMember?: Handler<bigint>;
    hostChanging?: Handler<bigint>;
    hostChanged?: Handler<HostChange>;
    hostResumed?: Handler<{ epoch: bigint; slot: number }>;
    rejected?: Handler<Rejected>;
    snapshotSent?: Handler<SentSnapshot>;
    memberPresence?: Handler<MemberPresence>;
    control: Map<number, (frame: RelayFrame) => void>;
  } = { control: new Map() };
  private closed = false;
  private running = false;
  private current: Session | undefined;
  private roomId = "";
  private role: Welcome = { slot: 0, host: false, hostSlot: 0, epoch: 0n };
  /** 换 host 的角色：Welcome 定初值，YouAreHost / HostChanged / YouAreMember 改它。 */
  private stage: HostRole = "member";
  private epochValue = 0n;
  /** 收到 HostChanging 之后、HostChanged / HostResumed 之前：发给房主的游戏帧会被 relay 丢掉。 */
  private changing = false;
  /** 最近用过的快照 seq：新的一份取它加 1；当上房主时抬到 YouAreHost 里的 seq（跨房主递增）。 */
  private seq = 0n;
  /** 最近一份快照发出的时刻；当上房主时清零（relay 换了房主重新算最小间隔）。 */
  private lastSnapshotAt: number | undefined;
  private successorList: number[] = [];
  /** 游戏自己调过 setSuccessors（换 host 后清零）：WebHost 的自动排序不覆盖它。 */
  private declaredByGame = false;
  /** setForeground 记下的前台状态；undefined 表示没设过（不发）。换 host、重连后重发。 */
  private foreground: boolean | undefined;
  /** 当房主时在线的成员（槽位 → 账号），来自 YouAreHost 的名单与 MemberJoined / MemberLeft。 */
  private readonly online = new Map<number, string>();
  private snapshotTimer: ReturnType<typeof setInterval> | undefined;
  private readonly compressor: Compressor;
  private hooks: InternalHooks = {};
  private rttMs = 0;
  private finished: Promise<void> = Promise.resolve();
  private finish: () => void = () => {};

  /** @param lobby 断线后重新要令牌用；只用 connect、不重连时可以为 undefined。 */
  constructor(
    private readonly lobby: LobbyClient | undefined,
    options: RelayClientOptions,
  ) {
    const pingIntervalMs = options.pingIntervalMs ?? 10_000;
    let readTimeoutMs = options.readTimeoutMs ?? 30_000;
    if (readTimeoutMs <= pingIntervalMs) readTimeoutMs = 3 * pingIntervalMs;
    this.opts = {
      game: options.game,
      password: options.password,
      webSocket:
        options.webSocket ??
        ((url) => {
          const Ctor = (globalThis as { WebSocket?: new (url: string) => WebSocketLike }).WebSocket;
          if (!Ctor) throw new Error("relay: no global WebSocket; pass options.webSocket (node: the ws package)");
          return new Ctor(url);
        }),
      connectTimeoutMs: options.connectTimeoutMs ?? 10_000,
      pingIntervalMs,
      readTimeoutMs,
      maxPayloadBytes: options.maxPayloadBytes ?? DEFAULT_MAX_PAYLOAD,
      snapshotMaxBytes: options.snapshotMaxBytes ?? 256 * 1024,
      hostPingIntervalMs: options.hostPingIntervalMs ?? 600,
      snapshot: options.snapshot,
      snapshotMinIntervalMs: options.snapshotMinIntervalMs ?? 1000,
      autoHostReady: options.autoHostReady ?? false,
      reconnect: {
        maxAttempts: options.reconnect?.maxAttempts ?? 0,
        minBackoffMs: options.reconnect?.minBackoffMs ?? 500,
        maxBackoffMs: options.reconnect?.maxBackoffMs ?? 5_000,
      },
    };
    this.compressor = options.compressor ?? defaultCompressor();
  }

  /** 游戏通道：房主收到成员的帧（slot 是成员槽位），成员收到房主的帧（slot 是房主槽位）。 */
  onGame(fn: Handler<RelayFrame>): void {
    this.handlers.game = fn;
  }
  /** 房间通道（聊天、表情、语音），slot 是发送者槽位。 */
  onRoom(fn: Handler<RelayFrame>): void {
    this.handlers.room = fn;
  }
  /** 成员上线（只有房主收到）。 */
  onMemberJoined(fn: Handler<Member>): void {
    this.handlers.memberJoined = fn;
  }
  /** 成员下线（只有房主收到）。 */
  onMemberLeft(fn: Handler<Member>): void {
    this.handlers.memberLeft = fn;
  }
  /** 被房主禁言（true）/ 解除（false）。 */
  onMuted(fn: Handler<boolean>): void {
    this.handlers.muted = fn;
  }
  onDisconnected(fn: Handler<Disconnect>): void {
    this.handlers.disconnected = fn;
  }
  /** 自动重连成功，参数是新连接的身份（成员重连后槽位可能变了）。 */
  onReconnected(fn: Handler<Welcome>): void {
    this.handlers.reconnected = fn;
  }
  // 换 host（方案 §4.6、§4.7.5；docs/framework/RELAY.md §2.3、§4A）。
  /**
   * 自己被选为新房主候选（YouAreHost）。fn 用 takeover.state 恢复玩法状态，准备好后调 hostReady()；开了 autoHostReady
   * 时 fn 返回（返回 Promise 的等它完成）后 SDK 自动回 HostReady，fn 抛错或 Promise 失败则不回。之后收到
   * HostChanged{self} 才算正式当上房主。候选在 relay 的 host_ready_timeout 内不回 HostReady，relay 换下一个候选。
   */
  onYouAreHost(fn: ((takeover: HostTakeover) => void | Promise<void>) | undefined): void {
    this.handlers.youAreHost = fn;
  }
  /** 自己不再是房主（主动转交之后的旧房主），或候选没在时限内回 HostReady（YouAreMember）。 */
  onYouAreMember(fn: Handler<bigint>): void {
    this.handlers.youAreMember = fn;
  }
  /** 房主不在（宽限期或选人）：自己发给房主的游戏帧被丢了，暂停发送，等 HostChanged / HostResumed。 */
  onHostChanging(fn: Handler<bigint>): void {
    this.handlers.hostChanging = fn;
  }
  /** 新房主已就绪（HostChanged），游戏帧恢复转发；self 为 true 时自己就是新房主。 */
  onHostChanged(fn: Handler<HostChange>): void {
    this.handlers.hostChanged = fn;
  }
  /** 房主在宽限期内回来了，仍是原来的房主（HostResumed，只有成员收到）。 */
  onHostResumed(fn: Handler<{ epoch: bigint; slot: number }>): void {
    this.handlers.hostResumed = fn;
  }
  /** 快照或转交被 relay 拒收（连接保留）。 */
  onRejected(fn: Handler<Rejected>): void {
    this.handlers.rejected = fn;
  }
  /** 一份快照发出去了（压缩后的字节与 seq）：sessionStorage 辅助据此存档。 */
  onSnapshotSent(fn: Handler<SentSnapshot>): void {
    this.handlers.snapshotSent = fn;
  }
  /**
   * 成员上报了前台 / 后台（MemberPresence，只有房主收到；换 host 后新房主收到各成员重发的状态）。WebHost 据此自动排
   * 候选顺序（游戏没有显式 setSuccessors 时）；直接用 RelayClient 的游戏自己决定怎么用。
   */
  onMemberPresence(fn: Handler<MemberPresence>): void {
    this.handlers.memberPresence = fn;
  }

  /**
   * 控制帧的扩展口：relay 发来的这个操作码的每一帧都交给 fn（内置处理过的操作码先做内置处理）。fn 为 undefined 时撤销。
   */
  onControl(op: number, fn: ((frame: RelayFrame) => void) | undefined): void {
    if (fn) this.handlers.control.set(op, fn);
    else this.handlers.control.delete(op);
  }

  /** 内部监听（WebHost 用）：在业务回调之前调用，业务注册 onXxx 不会顶掉它们。 */
  [internalHooks](hooks: InternalHooks): void {
    this.hooks = hooks;
  }

  /** 当前连接的身份；没有连接时是最后一次连接的身份。 */
  get welcome(): Welcome {
    return this.role;
  }
  get isHost(): boolean {
    return this.role.host;
  }
  /** 换 host 里的角色（成员 / 候选 / 房主）。 */
  get hostRole(): HostRole {
    return this.stage;
  }
  /** 最近一次见到的 host_epoch（Welcome / YouAreHost / HostChanged / HostResumed / YouAreMember 里的）；进房时就是房间此刻的值。 */
  get epoch(): bigint {
    return this.epochValue;
  }
  /** 收到 HostChanging 之后、HostChanged / HostResumed 之前为 true。 */
  get hostChanging(): boolean {
    return this.changing;
  }
  /** 最近用过的快照 seq。 */
  get snapshotSeq(): bigint {
    return this.seq;
  }
  /** 最近一次声明的候选顺序（游戏的 setSuccessors，或 WebHost 自动排的）。 */
  get successors(): readonly number[] {
    return this.successorList;
  }
  /** 游戏自己调过 setSuccessors（换 host 后清零）。WebHost 只在它为 false 时自动按前台状态排候选顺序。 */
  get successorsDeclared(): boolean {
    return this.declaredByGame;
  }
  /** 当房主时在线的成员（槽位 → 账号）；不是房主时为空。 */
  get onlineMembers(): ReadonlyMap<number, string> {
    return this.online;
  }
  get connected(): boolean {
    return this.current !== undefined;
  }
  get room(): string {
    return this.roomId;
  }
  /** 最近一次心跳的往返时延（毫秒）；还没收到 Pong 时为 0。 */
  get rtt(): number {
    return this.rttMs;
  }
  /** 当前这一轮连接（含自动重连）结束、最后一个回调返回之后 resolve。 */
  get done(): Promise<void> {
    return this.finished;
  }

  /** 用 lobby 给的 Ticket（transport 为 ws 的建房或加入的答复）连上 relay，等到 Welcome。relay 拒绝时抛 RelayClosedError。 */
  async connect(ticket: { room: { id: string }; relayAddr: string; token: string }): Promise<Welcome> {
    this.reserve();
    let session: Session;
    try {
      session = await this.dial(ticket.relayAddr, ticket.token);
    } catch (error) {
      this.running = false;
      throw error;
    }
    this.start(ticket.room.id, session);
    return session.welcome;
  }

  /** 重新向 lobby 要令牌（join 幂等）再连 relay；关掉自动重连时由业务自己重连。只能在 connect 过、当前没有连接时调用。 */
  async rejoin(): Promise<Welcome> {
    this.reserve();
    let session: Session;
    try {
      session = await this.rejoinOnce();
    } catch (error) {
      this.running = false;
      throw error;
    }
    this.start(this.roomId, session);
    return session.welcome;
  }

  /** 游戏通道。房主：target 是成员槽位或 ALL；成员：只到房主，target 不看。没有连接时返回 false。 */
  sendGame(target: number, msgId: number, payload: Uint8Array, flags = 0): boolean {
    return this.send({ channel: RelayChannel.Game, slot: target, msgId, flags, payload });
  }

  /** 房间通道：target 是某个槽位（房主也可以）或 ALL（除自己以外的全体）；超限或被禁言时被 relay 丢弃。 */
  sendRoom(target: number, msgId: number, payload: Uint8Array, flags = 0): boolean {
    return this.send({ channel: RelayChannel.Room, slot: target, msgId, flags, payload });
  }

  /** 踢出槽位上的成员（只有房主能发，否则抛错）。被踢的账号在房间存续期间不能再进。 */
  kick(slot: number): boolean {
    this.requireHost();
    return this.sendControl(RelayOp.Kick, slot, new Uint8Array(0));
  }

  /** 禁言 / 解除禁言槽位的房间通道（只有房主能发）。 */
  mute(slot: number, on: boolean): boolean {
    this.requireHost();
    return this.sendControl(RelayOp.Mute, slot, Uint8Array.of(on ? 1 : 0));
  }

  /** 发一个控制帧（控制帧的扩展口）。 */
  sendControl(op: number, slot: number, payload: Uint8Array): boolean {
    return this.send({ channel: RelayChannel.Control, slot, msgId: op, flags: 0, payload });
  }

  /**
   * 主动把房主转交给 slot 上的在线成员（只有房主能发）。handoff 是交接数据（压缩前），压缩后随 Transfer 交给新房主，
   * 不超过 snapshotMaxBytes。发出后 relay 回 YouAreMember、再广播 HostChanged；目标不对时回 Rejected{bad_target}。
   */
  async transfer(slot: number, handoff: Uint8Array = new Uint8Array(0)): Promise<boolean> {
    this.requireHost("transfer");
    const data = await packState(this.compressor, handoff); // 空的交接数据不带压缩方式标识
    this.checkStateSize(data.length);
    if (!this.role.host) return false; // 压缩期间已不是房主
    return this.sendBig(RelayOp.Transfer, slot, data);
  }

  /**
   * 声明候选顺序（只有房主能发，至多 256 个）。relay 只记最新一份；换 host 之后清空，由新房主重新声明。游戏声明过之后
   * WebHost 不再自动排（successorsDeclared）。
   */
  setSuccessors(slots: readonly number[]): boolean {
    this.declaredByGame = true;
    return this[internalSuccessors](slots);
  }

  /** WebHost 的自动排序：同 setSuccessors，但不算游戏声明过。 */
  [internalSuccessors](slots: readonly number[]): boolean {
    this.requireHost("set successors");
    this.successorList = [...slots];
    return this.sendControl(RelayOp.Successors, 0, encodeSuccessors(slots));
  }

  /**
   * 告诉当前房主自己在前台（true）还是后台（false）：发 Presence，relay 转给房主（房主自己发的被丢弃）。状态记在 SDK 里，
   * 之后每次收到 HostChanged、每次连上（含自动重连、rejoin）都自动重发，新房主不用问；从没调过时 SDK 不发。
   * 没有连接时返回 false（状态仍记下，连上时发出）。WebHost 在 visibilitychange 上自动调它。
   */
  setForeground(foreground: boolean): boolean {
    this.foreground = foreground;
    return this.sendControl(RelayOp.Presence, 0, encodePresence(foreground));
  }

  /**
   * 推一份快照（只有房主能推）：data 缺省取 snapshot.take()，压缩后带上递增的 seq 发给 relay。返回用的 seq；
   * 没东西可推、没有连接或已不是房主时返回 undefined；离上一份不到 snapshotMinIntervalMs 时抛 SnapshotTooSoonError
   * （relay 仍可能拒收，经 onRejected 通知）。之后的定时推送从现在重新计时。
   */
  async pushSnapshot(data?: Uint8Array): Promise<bigint | undefined> {
    this.requireHost("push a snapshot");
    this.restartSnapshotTimer();
    const state = data ?? (await this.opts.snapshot?.take());
    if (state === undefined) return undefined;
    const compressed = await packState(this.compressor, state);
    this.checkStateSize(compressed.length);
    if (!this.role.host || !this.current) return undefined;
    const t = now();
    if (this.lastSnapshotAt !== undefined && t - this.lastSnapshotAt < this.opts.snapshotMinIntervalMs) throw new SnapshotTooSoonError();
    const seq = this.seq + 1n; // 压缩完再取 seq：并发的几次推送按发出顺序递增
    if (!this.sendBig(RelayOp.Snapshot, 0, encodeSnapshot(seq, compressed))) return undefined;
    this.seq = seq;
    this.lastSnapshotAt = t;
    this.hooks.snapshotSent?.({ seq, data: compressed });
    this.handlers.snapshotSent?.({ seq, data: compressed });
    return seq;
  }

  /**
   * 告诉 relay 自己已接手（只有候选能发；epoch 缺省是 YouAreHost 里的）。relay 广播 HostChanged 之后自己才是房主；
   * 迟到的、epoch 不对的 HostReady 被 relay 忽略。不是候选或没有连接时返回 false。
   */
  hostReady(epoch: bigint = this.epochValue): boolean {
    if (this.stage !== "candidate") return false;
    return this.sendControl(RelayOp.HostReady, 0, encodeEpoch(epoch));
  }

  /** 快照 seq 至少从 seq 往上接：刷新页面后从 sessionStorage 恢复时用（relay 拒收不大于它存着的 seq 的快照）。 */
  raiseSnapshotSeq(seq: bigint): void {
    if (seq > this.seq) this.seq = seq;
  }

  /** 经 relay 离开房间（Leave 控制帧）：relay 回 Closed{left} 后断开，不重连。房主离开立即换 host。 */
  sendLeave(): boolean {
    return this.sendControl(RelayOp.Leave, 0, new Uint8Array(0));
  }

  /**
   * 现在检查一次心跳（到期就发 Ping、读超时就断开）。SDK 自己每隔一小段检查一次；后台标签页的主线程计时器被限流时，
   * 由 Worker 发来的消息驱动这里（web.ts 的 WebHost），房主的心跳不至于断。
   */
  pulse(): void {
    if (this.current) this.heartbeat(this.current);
  }

  /** 发一帧（每帧一条 WebSocket 二进制消息）。没有连接时返回 false。 */
  send(frame: RelayFrame): boolean {
    const data = encodeFrame(frame, this.opts.maxPayloadBytes);
    return this.current?.send(data) ?? false;
  }

  /** 经 lobby 离开房间（让出席位；房主离开由 relay 走换 host），然后 close。 */
  async leave(): Promise<void> {
    try {
      if (this.lobby && this.roomId) await this.lobby.leaveRoom(this.roomId, this.opts.game);
    } finally {
      this.close();
    }
  }

  /** 断开连接、停止重连。不经 lobby 离开：席位按 relay 的报告让出，要立即让出用 leave。 */
  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.current?.abort(RelayReason.None, new RelayClientClosedError());
  }

  private requireHost(what = "kick or mute"): void {
    if (!this.role.host) throw new Error(`relay: only the room host may ${what}`);
  }

  private checkStateSize(size: number): void {
    if (size > this.opts.snapshotMaxBytes) {
      throw new Error(`relay: state of ${size} bytes after compression exceeds snapshotMaxBytes ${this.opts.snapshotMaxBytes}`);
    }
  }

  /** 快照与转交帧不受 max_payload_bytes 限制，上限是 snapshot_max_bytes（加 seq 的 8 字节）。 */
  private sendBig(op: number, slot: number, payload: Uint8Array): boolean {
    const data = encodeFrame({ channel: RelayChannel.Control, slot, msgId: op, payload }, Math.max(this.opts.maxPayloadBytes, this.opts.snapshotMaxBytes + 8));
    return this.current?.send(data) ?? false;
  }

  /** 收帧上限：装得下带满快照的 YouAreHost（快照上限加 64 KiB 的成员名单）。 */
  private get readLimit(): number {
    return Math.max(this.opts.maxPayloadBytes, this.opts.snapshotMaxBytes + 64 * 1024);
  }

  /** 进房（含重连）：角色与 epoch 以这条连接的 Welcome 为准。 */
  private welcomed(welcome: Welcome): void {
    this.role = welcome;
    this.epochValue = welcome.epoch;
    this.stage = welcome.host ? "host" : "member";
    if (welcome.host) this.lastSnapshotAt = undefined;
    this.changing = !welcome.host && welcome.hostSlot === 0; // 正在选新房主
    this.online.clear(); // 房主重连后 relay 用 MemberJoined 把在线成员再告诉一遍
    this.syncSnapshotTimer();
  }

  private becomeMember(): void {
    this.stage = "member";
    this.role = { ...this.role, host: false };
    this.online.clear();
    this.syncSnapshotTimer();
  }

  /** YouAreHost：成为候选，解压状态交给游戏，游戏接手后回 HostReady。 */
  private takeOver(session: Session, info: YouAreHost): void {
    this.stage = "candidate";
    this.epochValue = info.epoch;
    // 与 Go / C# 相同：seq 取 max(本地, YouAreHost 的 seq, epoch<<32)，下一份加 1。epoch<<32 保证大于以前任何房主
    // 用本 SDK 发过的 seq（每个房主 2^32 份以内），旧房主最后一份快照没到 relay 时也不会 stale_seq。
    this.raiseSnapshotSeq(info.snapshotSeq);
    this.raiseSnapshotSeq(info.epoch << 32n);
    this.online.clear();
    for (const seat of info.members) {
      if (seat.online && seat.slot !== this.role.slot) this.online.set(seat.slot, seat.account);
    }
    const stillCandidate = () => this.current === session && this.stage === "candidate" && this.epochValue === info.epoch;
    void (async () => {
      const takeover: HostTakeover = { ...info, state: null, rawState: info.state };
      if (info.source !== StateSource.None) {
        try {
          takeover.state = await unpackState(this.compressor, info.state, MAX_STATE_BYTES);
        } catch (error) {
          takeover.stateError = error as Error;
        }
      }
      if (!stillCandidate()) return;
      try {
        await this.hooks.youAreHost?.(takeover);
        await this.handlers.youAreHost?.(takeover);
      } catch {
        return; // 游戏接不了手：不回 HostReady，relay 超时后换下一个候选
      }
      if (this.opts.autoHostReady && stillCandidate()) this.hostReady(info.epoch);
    })();
  }

  private hostChanged(change: HostChange): void {
    this.epochValue = change.epoch;
    this.changing = false;
    this.successorList = []; // relay 换 host 后清空候选顺序，由新房主重新声明
    this.declaredByGame = false;
    if (change.self) {
      this.stage = "host";
      this.lastSnapshotAt = undefined;
      this.role = { ...this.role, host: true, hostSlot: change.slot };
      this.syncSnapshotTimer();
    } else {
      this.role = { ...this.role, hostSlot: change.slot };
      if (this.stage !== "member") this.becomeMember();
    }
    this.resendPresence(); // 新房主不知道之前的上报：设过状态的人各自再报一次
    this.hooks.hostChanged?.(change);
    this.handlers.hostChanged?.(change);
  }

  /** 重发 setForeground 记下的状态；没设过就不发。 */
  private resendPresence(): void {
    if (this.foreground !== undefined) this.sendControl(RelayOp.Presence, 0, encodePresence(this.foreground));
  }

  /** 定时快照只在“有连接、是房主、配了 take 且间隔大于 0”时开着。 */
  private syncSnapshotTimer(): void {
    const interval = this.opts.snapshot?.intervalMs ?? 3000;
    const want = this.current !== undefined && this.stage === "host" && this.opts.snapshot !== undefined && interval > 0;
    if (want && this.snapshotTimer === undefined) {
      this.snapshotTimer = setInterval(() => void this.pushSnapshot().catch(() => {}), interval);
    } else if (!want && this.snapshotTimer !== undefined) {
      clearInterval(this.snapshotTimer);
      this.snapshotTimer = undefined;
    }
  }

  private restartSnapshotTimer(): void {
    if (this.snapshotTimer === undefined) return;
    clearInterval(this.snapshotTimer);
    this.snapshotTimer = undefined;
    this.syncSnapshotTimer();
  }

  /** 心跳：读超时断开；房主（含候选）在 hostPingIntervalMs 内没发过帧、成员在 pingIntervalMs 内没发过 Ping 就发一个。 */
  private heartbeat(session: Session): void {
    const t = now();
    if (t - session.lastReceive > this.opts.readTimeoutMs) {
      session.abort(RelayReason.Disconnected, new Error("relay: no frame within the read timeout"));
      return;
    }
    const hosting = this.stage !== "member";
    const due = hosting
      ? t - session.lastSend >= Math.min(this.opts.hostPingIntervalMs, this.opts.pingIntervalMs)
      : t - session.lastPing >= this.opts.pingIntervalMs;
    if (!due) return;
    session.lastPing = t;
    const payload = new Uint8Array(8);
    new DataView(payload.buffer).setBigUint64(0, BigInt(Math.round(t * 1000)));
    session.send(encodeFrame({ channel: RelayChannel.Control, slot: 0, msgId: RelayOp.Ping, payload }, MAX_PING_BYTES));
  }

  private reserve(): void {
    if (this.closed) throw new RelayClientClosedError();
    if (this.running) throw new Error("relay: already connected");
    this.running = true;
  }

  private start(roomId: string, session: Session): void {
    if (this.closed) {
      this.running = false;
      session.abort(RelayReason.None);
      throw new RelayClientClosedError();
    }
    this.roomId = roomId;
    this.finished = new Promise((resolve) => (this.finish = resolve));
    this.attach(session);
  }

  /** 把进房的连接接到本客户端：处理积压的帧、开心跳、接上结束处理。 */
  private attach(session: Session): void {
    this.current = session;
    this.welcomed(session.welcome);
    const pending = session.pending;
    session.pending = [];
    session.onFrame = (frame) => this.dispatch(session, frame);
    session.onEnd = (reason, error) => void this.ended(session, reason, error);
    // 检查间隔取房主心跳的一半：当上房主后不用重开计时器。
    const tick = Math.max(1, Math.min(this.opts.pingIntervalMs, this.opts.readTimeoutMs / 4, this.opts.hostPingIntervalMs / 2));
    session.timer = session.ended ? undefined : setInterval(() => this.heartbeat(session), tick);
    this.resendPresence(); // 连上之前（或上一轮连接里）设过的前台状态告诉房主
    for (const frame of pending) {
      this.dispatch(session, frame);
    }
    if (session.earlyEnd) {
      const { reason, error } = session.earlyEnd;
      queueMicrotask(() => void this.ended(session, reason, error));
    }
  }

  private dispatch(session: Session, frame: RelayFrame): void {
    try {
      switch (frame.channel) {
        case RelayChannel.Game:
          this.hooks.game?.(frame);
          this.handlers.game?.(frame);
          return;
        case RelayChannel.Room:
          this.handlers.room?.(frame);
          return;
        case RelayChannel.Control: {
          const builtin = builtinControl[frame.msgId];
          const reason = builtin ? builtin(this, frame, session) : RelayReason.None;
          if (reason !== RelayReason.None) {
            session.closedReason = reason;
            session.abort(reason, new RelayClosedError(reason));
            return;
          }
          this.handlers.control.get(frame.msgId)?.(frame);
          return;
        }
        default:
          throw new RelayProtocolError("frame without a channel");
      }
    } catch (error) {
      if (error instanceof RelayProtocolError) session.abort(RelayReason.Protocol, error);
      else throw error;
    }
  }

  /** 一条连接结束：通知，按策略自动重连。 */
  private async ended(session: Session, reason: RelayReason, error?: Error): Promise<void> {
    if (this.current === session) this.current = undefined;
    this.syncSnapshotTimer();
    if (this.closed) {
      this.emitDisconnect({ reason: RelayReason.None, error: new RelayClientClosedError(), reconnecting: false });
      this.stop();
      return;
    }
    const retry = this.opts.reconnect.maxAttempts > 0 && retryableReason(reason);
    this.emitDisconnect({ reason, error, reconnecting: retry });
    if (!retry) {
      this.stop();
      return;
    }
    let next: Session;
    try {
      next = await this.reconnect();
    } catch (failure) {
      const err = failure as Error;
      if (this.closed) {
        this.emitDisconnect({ reason: RelayReason.None, error: new RelayClientClosedError(), reconnecting: false });
      } else {
        this.emitDisconnect({
          reason: err instanceof RelayClosedError ? err.reason : RelayReason.Disconnected,
          error: err,
          reconnecting: false,
        });
      }
      this.stop();
      return;
    }
    if (this.closed) {
      next.abort(RelayReason.None);
      this.emitDisconnect({ reason: RelayReason.None, error: new RelayClientClosedError(), reconnecting: false });
      this.stop();
      return;
    }
    this.attach(next);
    this.hooks.reconnected?.(next.welcome);
    this.handlers.reconnected?.(next.welcome);
  }

  private emitDisconnect(disconnect: Disconnect): void {
    this.hooks.disconnected?.(disconnect);
    this.handlers.disconnected?.(disconnect);
  }

  private stop(): void {
    this.running = false;
    this.finish();
  }

  private async reconnect(): Promise<Session> {
    const policy = this.opts.reconnect;
    let backoff = policy.minBackoffMs;
    let last: unknown;
    for (let attempt = 0; attempt < policy.maxAttempts; attempt++) {
      await sleep(backoff);
      if (this.closed) throw new RelayClientClosedError();
      try {
        return await this.rejoinOnce();
      } catch (error) {
        last = error;
        if (!retryableError(error)) throw error;
      }
      backoff = Math.min(2 * backoff, policy.maxBackoffMs);
    }
    throw new Error(`relay: reconnect gave up after ${policy.maxAttempts} attempts: ${String(last)}`);
  }

  private async rejoinOnce(): Promise<Session> {
    if (!this.lobby || !this.roomId) throw new Error("relay: rejoin needs a lobby and a room from an earlier connect");
    const ticket = await this.lobby.joinRoom(this.roomId, { game: this.opts.game, password: this.opts.password, transport: "ws" });
    return this.dial(ticket.relayAddr, ticket.token);
  }

  /** 开 WebSocket、首帧发令牌、等第一帧：Welcome 就进房成功，Closed 就是 relay 拒绝。relay 总是先发 Welcome。 */
  private dial(url: string, token: string): Promise<Session> {
    if (!/^wss?:\/\//.test(url)) {
      return Promise.reject(new Error(`relay: ${url} is not a ws:// or wss:// URL (ask the lobby for transport ws)`));
    }
    return new Promise((resolve, reject) => {
      let ws: WebSocketLike;
      try {
        ws = this.opts.webSocket(url);
      } catch (error) {
        reject(error);
        return;
      }
      ws.binaryType = "arraybuffer";
      const session = new Session(ws, new FrameReader(this.readLimit));
      let settled = false;
      const fail = (error: Error) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        session.abort(RelayReason.None);
        reject(error);
      };
      const timer = setTimeout(() => fail(new Error(`relay: connecting to ${url} timed out`)), this.opts.connectTimeoutMs);
      session.onEnd = (reason, error) => fail(error ?? new Error(`relay: connection closed before welcome (${RelayReason[reason]})`));
      session.onFrame = (frame) => {
        if (settled) {
          session.pending.push(frame); // Welcome 之后同一批到的帧，attach 时处理
          return;
        }
        if (frame.channel === RelayChannel.Control && frame.msgId === RelayOp.Closed && frame.payload.length === 1) {
          fail(new RelayClosedError(frame.payload[0] as RelayReason));
          return;
        }
        if (frame.channel !== RelayChannel.Control || frame.msgId !== RelayOp.Welcome) {
          fail(new RelayProtocolError("first relay frame is not a welcome"));
          return;
        }
        try {
          session.welcome = decodeWelcome(frame.slot, frame.payload);
        } catch (error) {
          fail(error as Error);
          return;
        }
        settled = true;
        clearTimeout(timer);
        // Welcome 之后、attach 之前连接就断了：记下来，attach 时补报。
        session.onEnd = (reason, error) => {
          session.earlyEnd = { reason, error };
        };
        resolve(session);
      };
      ws.onopen = () => {
        session.send(encodeToken(token));
      };
      ws.onmessage = (event) => {
        session.lastReceive = now();
        let frames: RelayFrame[];
        try {
          frames = session.reader.push(toBytes(event.data));
        } catch (error) {
          session.abort(RelayReason.Protocol, error as Error);
          return;
        }
        for (const frame of frames) {
          if (session.ended) return;
          session.onFrame(frame);
        }
      };
      ws.onerror = () => {
        // 错误之后总有 onclose
      };
      ws.onclose = () => {
        session.end(session.closedReason !== RelayReason.None ? session.closedReason : RelayReason.Disconnected, new Error("relay: connection closed"));
      };
    });
  }
}

function toBytes(data: unknown): Uint8Array {
  if (data instanceof ArrayBuffer) return new Uint8Array(data);
  if (ArrayBuffer.isView(data)) return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
  throw new RelayProtocolError("relay sent a text message; every message must be a binary frame");
}

/** 值得自动重连的结束原因：连接自己断了（没收到 Closed）或 relay 判空闲超时。 */
function retryableReason(reason: RelayReason): boolean {
  return reason === RelayReason.Disconnected || reason === RelayReason.IdleTimeout;
}

/**
 * 一次重连尝试的失败值不值得再试：lobby 的 5xx、网络错误、relay 向 lobby 验权失败（lobby_unavailable）值得；
 * lobby 的 4xx（含被踢的 403 kicked）与 relay 的终止性 Closed（含 lobby_rejected）不值得。
 */
function retryableError(error: unknown): boolean {
  if (error instanceof LobbyError) return error.temporary;
  if (error instanceof RelayClosedError) {
    return retryableReason(error.reason) || error.reason === RelayReason.TokenRejected || error.reason === RelayReason.LobbyUnavailable;
  }
  return !(error instanceof RelayClientClosedError);
}
