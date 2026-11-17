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
  decodeMember,
  decodeWelcome,
  encodeFrame,
  encodeToken,
  type Member,
  type RelayFrame,
  type Welcome,
} from "./codec.js";
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
  /** 断线后自动重连，缺省关闭。 */
  reconnect?: ReconnectPolicy;
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
  welcome: Welcome = { slot: 0, host: false, hostSlot: 0 };
  /** Welcome 之后、attach 之前到达的帧（同一条消息或紧接着的消息），attach 时按序处理。 */
  pending: RelayFrame[] = [];
  earlyEnd: { reason: RelayReason; error?: Error } | undefined;

  constructor(
    readonly ws: WebSocketLike,
    readonly reader: FrameReader,
  ) {}

  send(data: Uint8Array): boolean {
    if (this.ended || this.ws.readyState !== 1) return false;
    try {
      this.ws.send(data);
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
 * 扩展点：换 host 的控制帧（64～127：you_are_host、host_changing、host_changed、host_resumed、transfer、successors、snapshot）
 * 下一批在这里加一项、在 RelayClient 上加对应的回调；不在表里的操作码只交给 onControl 注册的回调，没有回调就丢弃。
 */
const builtinControl: Record<number, (client: RelayClient, frame: RelayFrame) => RelayReason> = {
  [RelayOp.Welcome]: (client, frame) => {
    client["role"] = decodeWelcome(frame.slot, frame.payload);
    return RelayReason.None;
  },
  [RelayOp.MemberJoined]: (client, frame) => {
    client["handlers"].memberJoined?.(decodeMember(frame.slot, frame.payload));
    return RelayReason.None;
  },
  [RelayOp.MemberLeft]: (client, frame) => {
    client["handlers"].memberLeft?.(decodeMember(frame.slot, frame.payload));
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
  private readonly opts: Required<Omit<RelayClientOptions, "password" | "reconnect">> & {
    password?: string;
    reconnect: Required<ReconnectPolicy>;
  };
  private readonly handlers: {
    game?: Handler<RelayFrame>;
    room?: Handler<RelayFrame>;
    memberJoined?: Handler<Member>;
    memberLeft?: Handler<Member>;
    muted?: Handler<boolean>;
    disconnected?: Handler<Disconnect>;
    reconnected?: Handler<Welcome>;
    youAreHost?: Handler<RelayFrame>;
    hostChanging?: Handler<RelayFrame>;
    hostChanged?: Handler<RelayFrame>;
    hostResumed?: Handler<RelayFrame>;
    control: Map<number, (frame: RelayFrame) => void>;
  } = { control: new Map() };
  private closed = false;
  private running = false;
  private current: Session | undefined;
  private roomId = "";
  private role: Welcome = { slot: 0, host: false, hostSlot: 0 };
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
      reconnect: {
        maxAttempts: options.reconnect?.maxAttempts ?? 0,
        minBackoffMs: options.reconnect?.minBackoffMs ?? 500,
        maxBackoffMs: options.reconnect?.maxBackoffMs ?? 5_000,
      },
    };
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
  // 换 host 的回调位置（方案 §4.6、§4.7.5）。操作码在 64～127，由 host 组定编码表并追加进 client/spec/relay-packets.json；
  // 这一批还没有接线：relay 发来这些操作码时按“不认识的操作码”丢弃（注册了 onControl 的照样交给它）。接线时在
  // builtinControl 里加对应操作码、把载荷解成结构，再调用下面的回调（参数会从原始帧换成解好的结构）。
  /** 换 host：自己成了新房主（you_are_host）。尚未接线。 */
  onYouAreHost(fn: Handler<RelayFrame>): void {
    this.handlers.youAreHost = fn;
  }
  /** 换 host 开始：暂停发给房主的游戏帧（host_changing）。尚未接线。 */
  onHostChanging(fn: Handler<RelayFrame>): void {
    this.handlers.hostChanging = fn;
  }
  /** 换 host 完成：新房主是谁（host_changed）。尚未接线。 */
  onHostChanged(fn: Handler<RelayFrame>): void {
    this.handlers.hostChanged = fn;
  }
  /** 房主在宽限期内回来了（host_resumed）。尚未接线。 */
  onHostResumed(fn: Handler<RelayFrame>): void {
    this.handlers.hostResumed = fn;
  }

  /**
   * 控制帧的扩展口：relay 发来的这个操作码的每一帧都交给 fn（内置处理过的操作码先做内置处理）。fn 为 undefined 时撤销。
   */
  onControl(op: number, fn: ((frame: RelayFrame) => void) | undefined): void {
    if (fn) this.handlers.control.set(op, fn);
    else this.handlers.control.delete(op);
  }

  /** 当前连接的身份；没有连接时是最后一次连接的身份。 */
  get welcome(): Welcome {
    return this.role;
  }
  get isHost(): boolean {
    return this.role.host;
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

  /** 发一个控制帧：控制帧的扩展口，换 host 的控制帧（host_ready、transfer、successors、snapshot 等）下一批经它发出。 */
  sendControl(op: number, slot: number, payload: Uint8Array): boolean {
    return this.send({ channel: RelayChannel.Control, slot, msgId: op, flags: 0, payload });
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

  private requireHost(): void {
    if (!this.role.host) throw new Error("relay: only the room host may kick or mute");
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
    this.role = session.welcome;
    const pending = session.pending;
    session.pending = [];
    session.onFrame = (frame) => this.dispatch(session, frame);
    session.onEnd = (reason, error) => void this.ended(session, reason, error);
    const tick = Math.max(1, Math.min(this.opts.pingIntervalMs, this.opts.readTimeoutMs / 4));
    let lastPing = now();
    session.timer = session.ended ? undefined : setInterval(() => {
      const t = now();
      if (t - session.lastReceive > this.opts.readTimeoutMs) {
        session.abort(RelayReason.Disconnected, new Error("relay: no frame within the read timeout"));
        return;
      }
      if (t - lastPing < this.opts.pingIntervalMs) return;
      lastPing = t;
      const payload = new Uint8Array(8);
      new DataView(payload.buffer).setBigUint64(0, BigInt(Math.round(t * 1000)));
      session.send(encodeFrame({ channel: RelayChannel.Control, slot: 0, msgId: RelayOp.Ping, payload }, MAX_PING_BYTES));
    }, tick);
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
          this.handlers.game?.(frame);
          return;
        case RelayChannel.Room:
          this.handlers.room?.(frame);
          return;
        case RelayChannel.Control: {
          const builtin = builtinControl[frame.msgId];
          const reason = builtin ? builtin(this, frame) : RelayReason.None;
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
    if (this.closed) {
      this.handlers.disconnected?.({ reason: RelayReason.None, error: new RelayClientClosedError(), reconnecting: false });
      this.stop();
      return;
    }
    const retry = this.opts.reconnect.maxAttempts > 0 && retryableReason(reason);
    this.handlers.disconnected?.({ reason, error, reconnecting: retry });
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
        this.handlers.disconnected?.({ reason: RelayReason.None, error: new RelayClientClosedError(), reconnecting: false });
      } else {
        this.handlers.disconnected?.({
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
      this.handlers.disconnected?.({ reason: RelayReason.None, error: new RelayClientClosedError(), reconnecting: false });
      this.stop();
      return;
    }
    this.attach(next);
    this.handlers.reconnected?.(next.welcome);
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
      const session = new Session(ws, new FrameReader(this.opts.maxPayloadBytes));
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

/** 一次重连尝试的失败值不值得再试：lobby 的 5xx、网络错误值得；lobby 的 4xx 与 relay 的终止性 Closed 不值得。 */
function retryableError(error: unknown): boolean {
  if (error instanceof LobbyError) return error.temporary;
  if (error instanceof RelayClosedError) return retryableReason(error.reason) || error.reason === RelayReason.TokenRejected;
  return !(error instanceof RelayClientClosedError);
}
