// 显式创建的一条玩家接入连接（对应 C# Connection）：取票、连接超时、心跳（独占槽位）、心跳超时、自动重连（退避加抖动、
// 每次重新取票、取新连接代号）、ReconnectResult、connectionVersion、heartbeatRoundTrips。不依赖账号、场景或引擎。
//
// 回调时机（维护者 2026-11-18 D6）：stateChanged / reconnected / push 都在 WebSocket 事件或计时器回调里同步派发，没有 Pump。
// 重连语义与 C# 相同：断线时在途请求失败，不进重连队列，也不自动重发业务请求。

import { CLOCK_PROBE, decodeClockSample, type ClockSample } from "./clock.js";
import {
  ConnectionState,
  GateClosedError,
  GateTimeoutError,
  resolveOptions,
  type ConnectionOptions,
  type ReconnectResult,
  type ResolvedOptions,
  type TicketProvider,
} from "./options.js";
import { GateProtocolError, type Packet } from "./packet.js";
import { GateSession, abortReason, type Completion } from "./session.js";

export interface ConnectionEvents {
  stateChanged: ConnectionState;
  reconnected: ReconnectResult;
  push: Packet;
  /** 监听者自己抛出的错误。 */
  error: Error;
}

type Listener<T> = (value: T) => void;

/** 一次 connect 的生命周期：disconnect / dispose 时中止，旧生命周期的迟到收尾不能改变后来的连接。 */
interface Lifetime {
  readonly controller: AbortController;
  readonly ticket: TicketProvider;
}

export class Connection {
  private readonly opts: ResolvedOptions;
  private currentState = ConnectionState.Disconnected;
  private error: Error | undefined;
  private version = 0;
  private roundTrips = 0;
  private lifetime: Lifetime | undefined;
  private session: GateSession | undefined;
  private heartbeatTimer: unknown;
  private readonly listeners: { [K in keyof ConnectionEvents]: Listener<ConnectionEvents[K]>[] } = {
    stateChanged: [],
    reconnected: [],
    push: [],
    error: [],
  };

  constructor(options: ConnectionOptions) {
    this.opts = resolveOptions(options);
  }

  get state(): ConnectionState {
    return this.currentState;
  }

  /** 最近一次断开或连接失败的原因；连上、disconnect、dispose 时清空。 */
  get lastError(): Error | undefined {
    return this.error;
  }

  /** 每连上一次（含重连）加 1；业务据此认出“这是新连接”（例如重新拉取全量）。 */
  get connectionVersion(): number {
    return this.version;
  }

  /** 收到应答的心跳次数（跨重连累计）。诊断用：例如浏览器冒烟据此确认心跳在跑。 */
  get heartbeatRoundTrips(): number {
    return this.roundTrips;
  }

  /** 选项里的单包载荷上限。 */
  get maxPayloadBytes(): number {
    return this.opts.maxPayloadBytes;
  }

  /** 订阅连接事件，返回取消函数。监听者抛错交给 "error" 监听者（它们自己的错误忽略）。 */
  on<K extends keyof ConnectionEvents>(event: K, listener: Listener<ConnectionEvents[K]>): () => void {
    const list = this.listeners[event] as Listener<ConnectionEvents[K]>[];
    list.push(listener);
    return () => {
      const at = list.indexOf(listener);
      if (at >= 0) list.splice(at, 1);
    };
  }

  /**
   * 连上并完成握手。ticket 是固定票据，或每次握手（含自动重连）都调一次的取票函数（短期票据用它）。连接超时覆盖取票与握手。
   * 首次连接失败交给调用方（reject）；连上之后异常断线才按 autoReconnect 重连。只能在 Disconnected 时调用。
   */
  async connect(ticket: string | TicketProvider, signal?: AbortSignal): Promise<void> {
    if (typeof ticket === "string" && ticket.length === 0) throw new RangeError("gate: a ticket is required");
    const provider: TicketProvider = typeof ticket === "string" ? async () => ticket : ticket;
    if (this.currentState === ConnectionState.Disposed) throw new GateClosedError("gate: connection disposed");
    if (this.currentState !== ConnectionState.Disconnected) throw new Error("gate: connection already active");
    const reconnect = this.version > 0;
    const lifetime: Lifetime = { controller: new AbortController(), ticket: provider };
    this.lifetime = lifetime;
    this.error = undefined;
    this.setState(ConnectionState.Connecting);
    try {
      await this.open(lifetime, signal);
    } catch (error) {
      if (reconnect && !signal?.aborted) this.recordReconnect(lifetime, 1, error as Error, false);
      this.finish(lifetime, error as Error);
      throw error;
    }
    if (reconnect) this.recordReconnect(lifetime, 1, undefined, false);
  }

  /** 发请求等应答。超时（requestTimeoutMs）是 GateTimeoutError：只取消等待，不能推断服务端没执行，也不自动重发。 */
  requestAsync(messageId: number, responseId: number, payload: Uint8Array, signal?: AbortSignal): Promise<Packet> {
    return new Promise((resolve, reject) => {
      try {
        this.beginRequest(messageId, responseId, payload, (error, packet) => (error ? reject(error) : resolve(packet!)), signal);
      } catch (error) {
        reject(error);
      }
    });
  }

  /**
   * 回调形式的请求：complete 在收到应答的那个 WebSocket 事件里同步调用（与同一条消息里的推送按线上顺序），或在超时 / 断开时
   * 调用。连接不可用、在途已满时同步抛出。GameClient.request 用它。
   */
  beginRequest(messageId: number, responseId: number, payload: Uint8Array, complete: Completion, signal?: AbortSignal): void {
    if (!isMessageId(messageId) || !isMessageId(responseId)) throw new RangeError("gate: message id 0 is reserved for Gate control");
    this.readySession().request(messageId, responseId, payload, complete, this.opts.requestTimeoutMs, signal);
  }

  /** 只写出、不等应答的业务消息。没有连接时抛错；不自动重发。 */
  notify(messageId: number, payload: Uint8Array): void {
    if (!isMessageId(messageId)) throw new RangeError("gate: message id 0 is reserved for Gate control");
    this.readySession().notify(messageId, payload);
  }

  /** 校时探测：返回样本，由业务交给 NetworkClock（重连后 reset）。本地时刻是 timers.now() 的秒（缺省 performance.now()）。 */
  probeClockAsync(signal?: AbortSignal): Promise<ClockSample> {
    return new Promise((resolve, reject) => {
      try {
        this.beginProbeClock((error, sample) => (error ? reject(error) : resolve(sample!)), signal);
      } catch (error) {
        reject(error);
      }
    });
  }

  beginProbeClock(complete: (error: Error | undefined, sample: ClockSample | undefined) => void, signal?: AbortSignal): void {
    const session = this.readySession();
    const sent = this.opts.timers.now() / 1000;
    session.request(
      0,
      0,
      CLOCK_PROBE,
      (error, packet) => {
        if (error) return complete(error, undefined);
        if (session !== this.session) return complete(new GateClosedError("gate: the clock sample belongs to a replaced connection"), undefined);
        let sample: ClockSample;
        try {
          sample = decodeClockSample(sent, this.opts.timers.now() / 1000, packet!.payload);
        } catch (failure) {
          return complete(failure as Error, undefined);
        }
        complete(undefined, sample);
      },
      this.opts.requestTimeoutMs,
      signal,
    );
  }

  /** 断开，回到 Disconnected（不重连），之后可以再 connect。lastError 清空。 */
  disconnect(): void {
    this.close(false);
  }

  /** 断开并销毁；之后的调用都报 GateClosedError。 */
  dispose(): void {
    this.close(true);
  }

  // ---------- 内部 ----------

  private readySession(): GateSession {
    if (this.currentState !== ConnectionState.Ready || !this.session) {
      throw new GateClosedError(`gate: connection is not ready (${ConnectionState[this.currentState]})`, this.error);
    }
    return this.session;
  }

  /** 取票 + 握手，预算 connectTimeoutMs；成功后接上会话、进入 Ready、开始心跳。 */
  private async open(lifetime: Lifetime, signal: AbortSignal | undefined): Promise<void> {
    const timers = this.opts.timers;
    const budget = new AbortController();
    const timeout = this.opts.connectTimeoutMs;
    const timer = timers.setTimeout(() => budget.abort(new GateTimeoutError(`gate: connecting timed out after ${timeout}ms (ticket and handshake)`)), timeout);
    const forward = (source: AbortSignal) => () => budget.abort(source.reason);
    const fromLifetime = forward(lifetime.controller.signal);
    const fromCaller = signal ? forward(signal) : undefined;
    lifetime.controller.signal.addEventListener("abort", fromLifetime, { once: true });
    if (signal && fromCaller) {
      if (signal.aborted) budget.abort(signal.reason);
      else signal.addEventListener("abort", fromCaller, { once: true });
    }
    try {
      // 取票函数必须异步返回，预算覆盖取票：卡住的取票由预算中止。
      const ticket = await raceAbort(lifetime.ticket(budget.signal), budget.signal);
      if (budget.signal.aborted) throw abortReason(budget.signal);
      // 每次连接（含重连）新建一个传输；握手首包与连接代号与传输无关。
      const session = await GateSession.open(this.opts.transportFactory(), this.opts.webSocketUrl, ticket, timeout, budget.signal, {
        maxPayloadBytes: this.opts.maxPayloadBytes,
        maxPendingRequests: this.opts.maxPendingRequests,
        timers,
      });
      if (this.lifetime !== lifetime) {
        session.close(new GateClosedError("gate: disconnected while connecting"));
        throw new GateClosedError("gate: disconnected while connecting");
      }
      this.session = session;
      this.version++;
      this.error = undefined;
      this.setState(ConnectionState.Ready);
      session.attach(
        (packet) => this.emit("push", packet),
        (reason) => this.sessionClosed(lifetime, session, reason),
      );
      if (this.session === session) this.scheduleHeartbeat(session);
    } finally {
      timers.clearTimeout(timer);
      lifetime.controller.signal.removeEventListener("abort", fromLifetime);
      if (signal && fromCaller) signal.removeEventListener("abort", fromCaller);
    }
  }

  /** 心跳：间隔从上一个心跳的应答（或连上）算起；在途时不发第二个；超时或应答不空就断开。 */
  private scheduleHeartbeat(session: GateSession): void {
    if (this.opts.heartbeatIntervalMs === 0) return;
    this.heartbeatTimer = this.opts.timers.setTimeout(() => {
      this.heartbeatTimer = undefined;
      if (session !== this.session) return;
      try {
        session.heartbeat(this.opts.heartbeatTimeoutMs, (error, ack) => {
          if (session !== this.session) return;
          if (error) return session.close(error);
          if (ack!.payload.length !== 0) return session.close(new GateProtocolError("heartbeat acknowledgement must be empty"));
          this.roundTrips++;
          this.scheduleHeartbeat(session);
        });
      } catch (error) {
        session.close(error as Error);
      }
    }, this.opts.heartbeatIntervalMs);
  }

  private stopHeartbeat(): void {
    if (this.heartbeatTimer !== undefined) this.opts.timers.clearTimeout(this.heartbeatTimer);
    this.heartbeatTimer = undefined;
  }

  private sessionClosed(lifetime: Lifetime, session: GateSession, reason: Error): void {
    if (this.session !== session || this.lifetime !== lifetime) return; // 旧会话的迟到收尾
    this.stopHeartbeat();
    this.session = undefined;
    this.error = reason;
    if (!this.opts.autoReconnect) {
      this.finish(lifetime, reason);
      return;
    }
    this.setState(ConnectionState.Reconnecting);
    void this.reconnect(lifetime);
  }

  /** 按退避重连：第 n 次等 max(1, min(max, initial × 2^n) × [0.5, 1))，每次重新取票。达到上限后停在 Disconnected。 */
  private async reconnect(lifetime: Lifetime): Promise<void> {
    const max = this.opts.maxReconnectAttempts;
    for (let attempt = 0; attempt < max; attempt++) {
      const cap = Math.min(this.opts.reconnectMaxDelayMs, this.opts.reconnectInitialDelayMs * 2 ** Math.min(attempt, 30));
      const delay = Math.max(1, cap * (0.5 + this.opts.random() * 0.5));
      if (!(await this.wait(delay, lifetime))) return;
      try {
        await this.open(lifetime, undefined);
        this.recordReconnect(lifetime, attempt + 1, undefined, false);
        return;
      } catch (error) {
        if (this.lifetime !== lifetime) return;
        this.error = error as Error;
        this.recordReconnect(lifetime, attempt + 1, error as Error, attempt + 1 < max);
      }
    }
    this.finish(lifetime, this.error); // 连续失败达到上限，等业务显式 connect
  }

  /** 等 ms 毫秒；生命周期在此期间结束时返回 false。 */
  private wait(ms: number, lifetime: Lifetime): Promise<boolean> {
    return new Promise((resolve) => {
      const signal = lifetime.controller.signal;
      if (signal.aborted) return resolve(false);
      const onAbort = () => {
        this.opts.timers.clearTimeout(timer);
        resolve(false);
      };
      const timer = this.opts.timers.setTimeout(() => {
        signal.removeEventListener("abort", onAbort);
        resolve(this.lifetime === lifetime);
      }, ms);
      signal.addEventListener("abort", onAbort, { once: true });
    });
  }

  private recordReconnect(lifetime: Lifetime, attempt: number, error: Error | undefined, willRetry: boolean): void {
    if (this.lifetime !== lifetime) return;
    const result: ReconnectResult = error ? { attempt, connectionVersion: this.version, error, willRetry } : { attempt, connectionVersion: this.version, willRetry };
    this.emit("reconnected", result);
  }

  /** 生命周期结束（失败、断开、重连放弃）：回到 Disconnected。旧生命周期的迟到收尾不能改变后来建立的连接。 */
  private finish(lifetime: Lifetime, error: Error | undefined): void {
    if (this.lifetime !== lifetime) return;
    this.lifetime = undefined;
    this.stopHeartbeat();
    const session = this.session;
    this.session = undefined;
    this.error = error;
    session?.close(error ?? new GateClosedError("gate: connection finished"));
    if (this.currentState !== ConnectionState.Disposed) this.setState(ConnectionState.Disconnected);
  }

  private close(dispose: boolean): void {
    if (this.currentState === ConnectionState.Disposed) return;
    const lifetime = this.lifetime;
    const session = this.session;
    this.lifetime = undefined;
    this.session = undefined;
    this.stopHeartbeat();
    this.error = undefined;
    this.setState(dispose ? ConnectionState.Disposed : ConnectionState.Disconnected);
    const reason = new GateClosedError(dispose ? "gate: connection disposed" : "gate: disconnected by the caller");
    lifetime?.controller.abort(reason);
    session?.close(reason);
    if (dispose) {
      for (const list of Object.values(this.listeners)) list.length = 0;
    }
  }

  private setState(state: ConnectionState): void {
    if (this.currentState === state) return;
    this.currentState = state;
    this.emit("stateChanged", state);
  }

  private emit<K extends keyof ConnectionEvents>(event: K, value: ConnectionEvents[K]): void {
    const list = [...(this.listeners[event] as Listener<ConnectionEvents[K]>[])];
    for (const listener of list) {
      try {
        listener(value);
      } catch (error) {
        if (event === "error") continue; // 错误观察者自身失败不能回流
        this.emit("error", error instanceof Error ? error : new Error(String(error)));
      }
    }
  }
}

function isMessageId(id: number): boolean {
  return Number.isInteger(id) && id > 0 && id <= 0xffff_ffff;
}

/** promise 与中止谁先到：中止时 reject 中止原因（promise 之后的结果被忽略）。 */
function raceAbort<T>(promise: Promise<T>, signal: AbortSignal): Promise<T> {
  if (signal.aborted) {
    promise.catch(() => {});
    return Promise.reject(abortReason(signal));
  }
  return new Promise<T>((resolve, reject) => {
    const onAbort = () => reject(abortReason(signal));
    signal.addEventListener("abort", onAbort, { once: true });
    promise.then(
      (value) => {
        signal.removeEventListener("abort", onAbort);
        resolve(value);
      },
      (error) => {
        signal.removeEventListener("abort", onAbort);
        reject(error);
      },
    );
  });
}
