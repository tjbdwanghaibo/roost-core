// Web 端房主辅助（方案 FEATURE-2026-11-16-lobby-relay §4.7.2～§4.7.4）：玩法规则是一个纯 TS 模块（HostRules），
// 与房间框架解耦；这里把它接到 RelayClient 上，并处理浏览器特有的三件事：
//
//   - 后台标签页的计时器限流：规则循环可以放进 Web Worker（WorkerRunner + serveRules），并且按实际经过的时间补帧
//     （FixedStepLoop，有补帧上限）；Worker 每一步发回的消息顺带驱动主线程的心跳（RelayClient.pulse）。
//   - visibilitychange：房主页面隐藏超过 hiddenHandoffMs（桌面推荐 5s，手机推荐 0）还没回来，就用 Transfer 把房主
//     连同交接数据让给候选里第一个在线的成员；回到前台不自动抢回。每个页面（成员也一样）在 visibilitychange 上
//     setForeground 上报自己在不在前台，房主据此把前台成员排在候选顺序前面（游戏显式 setSuccessors 过的以游戏为准）。
//   - 刷新页面：sessionStorage 存最近一份快照与身份（HostSessionStore）；刷新后在 relay 的 host_grace 内重连，
//     relay 认出同一账号仍是房主，WebHost 从存档恢复规则状态（方案 §4.7.4 的普通换 host 流程，relay 不回传快照）。
//
// 浏览器 API（document、sessionStorage、Worker、计时器、时钟）都可以注入，node 下测试用假的。

import { RelayClient, internalHooks, internalSuccessors, type LeaveResult, type RelayClientOptions } from "./client.js";
import { ALL, RelayChannel, RelayReason, type RelayFrame, type Welcome } from "./codec.js";
import { MAX_STATE_BYTES, defaultCompressor, unpackState, type Compressor } from "./compress.js";
import type { CreateRoomRequest, LobbyClient } from "./lobby.js";

// ---- 规则模块 ----

/** 规则输出的一帧：发给 target（成员槽位，或 ALL 全体）。 */
export interface HostOutput {
  target: number;
  msgId: number;
  payload: Uint8Array;
  flags?: number;
}

/**
 * 房主的玩法规则：输入操作 → 改状态 → 输出要广播的变化 / 快照。不碰网络、计时器与任何框架；
 * 同一份实现可以在主线程（LocalRunner）或 Worker（serveRules）里跑。
 */
export interface HostRules {
  /** 恢复状态：快照、交接数据或刷新前的存档；null 表示没有可用的状态（新开一局，或由规则自己决定）。 */
  restore(state: Uint8Array | null): void;
  /** 一个操作：slot 是发出者槽位（房主自己的操作也走这里）。 */
  input(slot: number, msgId: number, payload: Uint8Array): void;
  /** 推进一个固定步长，返回这一步要发出的帧。 */
  step(stepMs: number): HostOutput[] | void;
  /** 当前状态（压缩前），用于快照与转交。 */
  snapshot(): Uint8Array;
}

/** 补帧的结果：这次跑了几步、因为超过补帧上限丢掉了几步。 */
export interface Advance {
  steps: number;
  dropped: number;
}

/**
 * 按实际经过的时间推进的固定步长循环：每次 advance 用 now() 算出离上次过了多久，累计到够一步就跑一步；
 * 被限流后一次补上，但一次最多补 maxCatchUpSteps 步，多出来的时间直接丢掉（不追，免得补帧本身卡住页面）。
 */
export class FixedStepLoop {
  private last: number;
  private carry = 0;
  readonly stepMs: number;
  readonly maxCatchUpSteps: number;

  /**
   * @param step 跑一步
   * @param stepMs 步长
   * @param maxCatchUpSteps 一次最多补几步，缺省 1 秒的步数（Chrome 后台页的计时器每秒一次）
   */
  constructor(
    private readonly step: (stepMs: number) => void,
    options: { stepMs: number; maxCatchUpSteps?: number; now?: () => number },
  ) {
    if (!(options.stepMs > 0)) throw new Error("relay: stepMs must be positive");
    this.stepMs = options.stepMs;
    this.maxCatchUpSteps = Math.max(1, options.maxCatchUpSteps ?? Math.ceil(1000 / options.stepMs));
    this.now = options.now ?? defaultNow;
    this.last = this.now();
  }

  private readonly now: () => number;

  /** 从现在重新计时（开始或恢复时调用），之前累计的时间作废。 */
  reset(): void {
    this.last = this.now();
    this.carry = 0;
  }

  advance(): Advance {
    const t = this.now();
    this.carry += Math.max(0, t - this.last);
    this.last = t;
    let due = Math.floor(this.carry / this.stepMs);
    this.carry -= due * this.stepMs;
    let dropped = 0;
    if (due > this.maxCatchUpSteps) {
      dropped = due - this.maxCatchUpSteps;
      due = this.maxCatchUpSteps;
    }
    for (let i = 0; i < due; i++) this.step(this.stepMs);
    return { steps: due, dropped };
  }
}

function defaultNow(): number {
  return globalThis.performance ? globalThis.performance.now() : Date.now();
}

/** 计时器（注入用）。 */
export interface Timers {
  setInterval(fn: () => void, ms: number): unknown;
  clearInterval(handle: unknown): void;
}

const globalTimers: Timers = {
  setInterval: (fn, ms) => setInterval(fn, ms),
  clearInterval: (handle) => clearInterval(handle as ReturnType<typeof setInterval>),
};

/** 规则循环的设置。 */
export interface LoopOptions {
  /** 固定步长，缺省 50ms（20Hz）。 */
  stepMs?: number;
  /** 一次最多补几步，缺省 1 秒的步数。 */
  maxCatchUpSteps?: number;
  now?: () => number;
  timers?: Timers;
}

/** 规则在哪儿跑：WebHost 只经这个接口驱动它（主线程或 Worker）。 */
export interface RulesRunner {
  /** 恢复状态并开始循环；已经在跑时只恢复状态。 */
  start(state: Uint8Array | null): void;
  stop(): void;
  readonly running: boolean;
  input(slot: number, msgId: number, payload: Uint8Array): void;
  snapshot(): Promise<Uint8Array>;
  /** 每醒一次调用一次（frames 可以为空）：WebHost 发出 frames，并借这一下检查心跳。 */
  onStep: ((frames: HostOutput[], advance: Advance) => void) | undefined;
}

/** 在当前线程里跑规则（主线程，或者 Worker 里由 serveRules 用）。 */
export class LocalRunner implements RulesRunner {
  onStep: ((frames: HostOutput[], advance: Advance) => void) | undefined;
  private readonly loop: FixedStepLoop;
  private readonly timers: Timers;
  private handle: unknown;
  private out: HostOutput[] = [];

  constructor(
    private readonly rules: HostRules,
    options: LoopOptions = {},
  ) {
    this.timers = options.timers ?? globalTimers;
    this.loop = new FixedStepLoop(
      (stepMs) => {
        const frames = this.rules.step(stepMs);
        if (frames) this.out.push(...frames);
      },
      { stepMs: options.stepMs ?? 50, maxCatchUpSteps: options.maxCatchUpSteps, now: options.now },
    );
  }

  get running(): boolean {
    return this.handle !== undefined;
  }

  start(state: Uint8Array | null): void {
    this.rules.restore(state);
    if (this.handle !== undefined) return;
    this.loop.reset();
    this.handle = this.timers.setInterval(() => this.wake(), this.loop.stepMs);
  }

  stop(): void {
    if (this.handle === undefined) return;
    this.timers.clearInterval(this.handle);
    this.handle = undefined;
  }

  input(slot: number, msgId: number, payload: Uint8Array): void {
    this.rules.input(slot, msgId, payload);
  }

  async snapshot(): Promise<Uint8Array> {
    return this.rules.snapshot();
  }

  /** 醒一次：按经过的时间补帧，把这一批输出交出去。计时器调它；测试也可以直接调。 */
  wake(): Advance {
    const advance = this.loop.advance();
    const frames = this.out;
    this.out = [];
    this.onStep?.(frames, advance);
    return advance;
  }
}

/** 主线程与 Worker 之间的消息。 */
type ToWorker =
  | { type: "start"; state: Uint8Array | null }
  | { type: "stop" }
  | { type: "input"; slot: number; msgId: number; payload: Uint8Array }
  | { type: "snapshot"; id: number };
type FromWorker =
  | { type: "step"; frames: HostOutput[]; advance: Advance }
  | { type: "snapshot"; id: number; data: Uint8Array }
  | { type: "error"; message: string };

/** 浏览器 Worker（主线程一侧）与 Worker 里的 self 都满足的最小接口；node 的 MessagePort 也满足。 */
export interface MessageEndpoint {
  postMessage(message: unknown): void;
  onmessage: ((event: { data: unknown }) => void) | null;
}

/**
 * Worker 一侧：在 Worker 脚本里调用 `serveRules(new MyRules(), { stepMs: 50 })`。规则循环在 Worker 里按经过的时间补帧，
 * 每醒一次把输出发回主线程。scope 缺省是 Worker 的全局 self。
 */
export function serveRules(rules: HostRules, options: LoopOptions & { scope?: MessageEndpoint } = {}): LocalRunner {
  const scope = options.scope ?? (globalThis as unknown as MessageEndpoint);
  const runner = new LocalRunner(rules, options);
  const post = (message: FromWorker) => scope.postMessage(message);
  runner.onStep = (frames, advance) => post({ type: "step", frames, advance });
  scope.onmessage = (event) => {
    const message = event.data as ToWorker;
    try {
      switch (message.type) {
        case "start":
          runner.start(message.state);
          return;
        case "stop":
          runner.stop();
          return;
        case "input":
          runner.input(message.slot, message.msgId, message.payload);
          return;
        case "snapshot":
          post({ type: "snapshot", id: message.id, data: rules.snapshot() });
          return;
      }
    } catch (error) {
      post({ type: "error", message: String(error) });
    }
  };
  return runner;
}

/**
 * 主线程一侧：把规则交给 Worker 跑。`new WorkerRunner(new Worker(new URL("./rules.worker.js", import.meta.url), { type: "module" }))`。
 * 桌面浏览器对 Worker 的计时器基本不做后台限流（不是规范保证，只是缓解），并且 Worker 每一步的消息会唤醒主线程。
 */
export class WorkerRunner implements RulesRunner {
  onStep: ((frames: HostOutput[], advance: Advance) => void) | undefined;
  /** Worker 里规则抛的错。 */
  onError: ((message: string) => void) | undefined;
  private isRunning = false;
  private nextId = 1;
  private readonly waiting = new Map<number, (data: Uint8Array) => void>();

  constructor(private readonly worker: MessageEndpoint) {
    worker.onmessage = (event) => {
      const message = event.data as FromWorker;
      switch (message.type) {
        case "step":
          if (this.isRunning) this.onStep?.(message.frames, message.advance);
          return;
        case "snapshot":
          this.waiting.get(message.id)?.(message.data);
          this.waiting.delete(message.id);
          return;
        case "error":
          this.onError?.(message.message);
          return;
      }
    };
  }

  get running(): boolean {
    return this.isRunning;
  }

  start(state: Uint8Array | null): void {
    this.isRunning = true;
    this.post({ type: "start", state });
  }

  stop(): void {
    this.isRunning = false;
    this.post({ type: "stop" });
  }

  input(slot: number, msgId: number, payload: Uint8Array): void {
    this.post({ type: "input", slot, msgId, payload });
  }

  snapshot(): Promise<Uint8Array> {
    const id = this.nextId++;
    return new Promise((resolve) => {
      this.waiting.set(id, resolve);
      this.post({ type: "snapshot", id });
    });
  }

  private post(message: ToWorker): void {
    this.worker.postMessage(message);
  }
}

// ---- sessionStorage 存档 ----

/** sessionStorage 的最小接口。 */
export interface StorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

/** 刷新页面后接着当房主要的东西：房间与账号（身份），最近一份快照（压缩后的字节）与它的 seq。 */
export interface SavedHost {
  room: string;
  game: string;
  account: string;
  seq: bigint;
  /** 压缩后的快照；还没推过快照时为 null。 */
  state: Uint8Array | null;
  savedAt: number;
}

interface SavedJSON {
  room: string;
  game: string;
  account: string;
  seq: string;
  state: string | null;
  savedAt: number;
}

function toBase64(data: Uint8Array): string {
  let text = "";
  for (let i = 0; i < data.length; i += 0x8000) text += String.fromCharCode(...data.subarray(i, i + 0x8000));
  return btoa(text);
}

function fromBase64(text: string): Uint8Array {
  const raw = atob(text);
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

/**
 * 同一个标签页刷新后还在的存档（sessionStorage，每个标签页一份）。写满或被禁用时静默放弃：存档只是刷新后更平滑，
 * 丢了就走 relay 的普通换 host。
 */
export class HostSessionStore {
  constructor(
    private readonly storage: StorageLike,
    private readonly key = "roost.relay.host",
  ) {}

  save(saved: SavedHost): void {
    const json: SavedJSON = {
      room: saved.room,
      game: saved.game,
      account: saved.account,
      seq: saved.seq.toString(),
      state: saved.state ? toBase64(saved.state) : null,
      savedAt: saved.savedAt,
    };
    try {
      this.storage.setItem(this.key, JSON.stringify(json));
    } catch {
      // 写满或被禁用
    }
  }

  /** 读存档；account 给了时只认同一个账号的。 */
  load(account?: string): SavedHost | undefined {
    let text: string | null;
    try {
      text = this.storage.getItem(this.key);
    } catch {
      return undefined;
    }
    if (!text) return undefined;
    try {
      const json = JSON.parse(text) as SavedJSON;
      if (account !== undefined && json.account !== account) return undefined;
      return {
        room: json.room,
        game: json.game,
        account: json.account,
        seq: BigInt(json.seq),
        state: json.state === null ? null : fromBase64(json.state),
        savedAt: json.savedAt,
      };
    } catch {
      return undefined;
    }
  }

  clear(): void {
    try {
      this.storage.removeItem(this.key);
    } catch {
      // 被禁用
    }
  }
}

// ---- WebHost：把规则、RelayClient 与浏览器事件接在一起 ----

/** document 的最小接口（visibilitychange）。 */
export interface DocumentLike {
  readonly visibilityState: string;
  addEventListener(type: "visibilitychange", listener: () => void): void;
  removeEventListener(type: "visibilitychange", listener: () => void): void;
}

/** 一次性计时器（注入用）。 */
export interface TimeoutTimers {
  setTimeout(fn: () => void, ms: number): unknown;
  clearTimeout(handle: unknown): void;
}

export interface WebHostOptions {
  lobby: LobbyClient;
  /** 本页面的账号：存档只认同一个账号的。 */
  account: string;
  /** 规则在哪儿跑：new LocalRunner(rules) 或 new WorkerRunner(worker)。 */
  runner: RulesRunner;
  /** RelayClient 的设置；snapshot.take 由 WebHost 接到规则上（间隔 snapshotIntervalMs，缺省 3s）；autoHostReady 固定开。 */
  relay: Omit<RelayClientOptions, "snapshot" | "autoHostReady"> & { snapshotIntervalMs?: number };
  /** 页面隐藏多久后让出房主：桌面推荐 5000（缺省），手机推荐 0（后台页随时被冻结）。负数表示不让。 */
  hiddenHandoffMs?: number;
  /** 缺省 globalThis.document；没有时不监听可见性。 */
  document?: DocumentLike;
  /** 缺省 globalThis.sessionStorage；没有时不存档。传 null 关掉存档。 */
  storage?: StorageLike | null;
  timers?: TimeoutTimers;
}

/** 手机浏览器（后台页随时被冻结，应该一隐藏就让出房主）。 */
export function isMobileUserAgent(userAgent: string): boolean {
  return /Android|iPhone|iPad|iPod|Mobile/i.test(userAgent);
}

/**
 * 浏览器里的一个玩家：当房主时把成员的操作喂给规则、把规则的输出经 relay 发出去、定时推快照；当成员时把操作发给房主。
 * 换 host（YouAreHost / YouAreMember / HostChanged）时开停规则循环；页面隐藏时让出房主；刷新后从存档接着当房主。
 *
 * 游戏只用 input 发自己的操作、onFrame 收要显示的帧：房主自己的规则输出（目标为全体或自己的）也经 onFrame 交给本页面，
 * 所以房主和成员的显示代码是同一套。
 */
export class WebHost {
  readonly client: RelayClient;
  private readonly runner: RulesRunner;
  private readonly store: HostSessionStore | undefined;
  private readonly doc: DocumentLike | undefined;
  private readonly timers: TimeoutTimers;
  private readonly compressor: Compressor;
  private readonly hiddenHandoffMs: number;
  private hiddenTimer: unknown;
  private roomId = "";
  private frameHandler: ((frame: RelayFrame) => void) | undefined;
  private handoffHandler: ((target: number | undefined) => void) | undefined;
  /** 当房主时各成员上报的前台状态（槽位 → 是否前台）；换 host 后清空，等成员重发。 */
  private readonly presence = new Map<number, boolean>();
  /** 最近一次自动声明的候选顺序：没变就不重发。 */
  private autoSuccessors: number[] = [];
  private readonly onVisibility = () => {
    this.client.setForeground(this.doc?.visibilityState !== "hidden");
    this.visibilityChanged();
  };

  constructor(private readonly options: WebHostOptions) {
    this.runner = options.runner;
    this.compressor = options.relay.compressor ?? defaultCompressor();
    this.timers = options.timers ?? {
      setTimeout: (fn, ms) => setTimeout(fn, ms),
      clearTimeout: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
    };
    this.hiddenHandoffMs = options.hiddenHandoffMs ?? 5000;
    const storage = options.storage === undefined ? globalStorage() : options.storage;
    this.store = storage ? new HostSessionStore(storage) : undefined;
    this.doc = options.document ?? (globalThis as { document?: DocumentLike }).document;
    this.client = new RelayClient(options.lobby, {
      ...options.relay,
      compressor: this.compressor,
      autoHostReady: true, // 规则的 restore 是同步的：onYouAreHost 返回就能接手
      snapshot: { take: () => (this.runner.running ? this.runner.snapshot() : undefined), intervalMs: options.relay.snapshotIntervalMs },
    });
    this.wire();
    this.doc?.addEventListener("visibilitychange", this.onVisibility);
    // 进房前先记下当前的可见性：连上时 SDK 发出，之后换 host、重连都会重发
    if (this.doc) this.client.setForeground(this.doc.visibilityState !== "hidden");
  }

  /** 要显示的帧：成员收到房主的帧；房主收到自己规则的输出（slot 是自己的槽位）。 */
  onFrame(fn: ((frame: RelayFrame) => void) | undefined): void {
    this.frameHandler = fn;
  }

  /** 页面隐藏期满时的让出结果：target 是转交的目标槽位，没有可转交的成员时为 undefined（留在原地）。 */
  onHiddenHandoff(fn: ((target: number | undefined) => void) | undefined): void {
    this.handoffHandler = fn;
  }

  get room(): string {
    return this.roomId;
  }

  /** 建房并连上 relay（自己是房主，规则从空状态开始）。 */
  async create(req: CreateRoomRequest): Promise<Welcome> {
    return this.enter(await this.options.lobby.createRoom({ ...req, transport: "ws" }));
  }

  /** 加入房间（或用 join 的幂等性重新拿令牌回到房间）。 */
  async join(roomId: string, password?: string): Promise<Welcome> {
    return this.enter(await this.options.lobby.joinRoom(roomId, { game: this.options.relay.game, password, transport: "ws" }));
  }

  /**
   * 刷新页面后调用：有同一账号的存档就回到存档里的房间。relay 认出是宽限期内回来的房主时，规则从存档的快照恢复，
   * 快照 seq 从存档往上接；已经换了房主（宽限期过了）时以成员身份回来，存档作废。没有存档返回 undefined。
   */
  async resume(): Promise<Welcome | undefined> {
    const saved = this.store?.load(this.options.account);
    if (!saved || saved.game !== this.options.relay.game) return undefined;
    this.client.raiseSnapshotSeq(saved.seq);
    return this.join(saved.room);
  }

  /** 自己的一个操作：房主直接交给规则，成员发给房主（游戏通道）。 */
  input(msgId: number, payload: Uint8Array): boolean {
    if (this.client.hostRole === "host") {
      this.runner.input(this.client.welcome.slot, msgId, payload);
      return true;
    }
    return this.client.sendGame(ALL, msgId, payload);
  }

  /**
   * 让出房主：游戏声明过候选顺序时给其中第一个在线的成员；没声明时给前台成员里槽位最小的，没有前台成员（或都没上报）
   * 再取在线成员里槽位最小的（与 relay 的兜底规则相同）。交接数据是规则当前的状态。target 给了就转交给它（必须在线）。
   * 没有可转交的成员时返回 undefined。
   */
  async handOff(target: number | undefined = this.pickSuccessor()): Promise<number | undefined> {
    if (this.client.hostRole !== "host") return undefined;
    if (target === undefined || !this.client.onlineMembers.has(target)) return undefined;
    const handoff = await this.runner.snapshot();
    if (this.client.hostRole !== "host" || !this.client.onlineMembers.has(target)) return undefined;
    return (await this.client.transfer(target, handoff)) ? target : undefined;
  }

  /** 经 lobby 离开、停规则、删存档。结果同 RelayClient.leave（roomGone：房间早就不在）。 */
  async leave(): Promise<LeaveResult> {
    this.runner.stop();
    this.store?.clear();
    return this.client.leave();
  }

  /** 断开、停规则、不再监听页面事件（存档留着：刷新时也会走到这里之前的状态）。 */
  close(): void {
    this.doc?.removeEventListener("visibilitychange", this.onVisibility);
    this.cancelHidden();
    this.runner.stop();
    this.client.close();
  }

  private pickSuccessor(): number | undefined {
    const online = this.client.onlineMembers;
    const declared = this.client.successors;
    if (this.client.successorsDeclared && declared.length > 0) return declared.find((slot) => online.has(slot));
    return this.foregroundFirst()[0];
  }

  /** 在线成员的候选顺序：上报在前台的按槽位升序在前，其余（后台、没上报的）按槽位升序在后。 */
  private foregroundFirst(): number[] {
    const slots = [...this.client.onlineMembers.keys()].sort((a, b) => a - b);
    return [...slots.filter((slot) => this.presence.get(slot) === true), ...slots.filter((slot) => this.presence.get(slot) !== true)];
  }

  /**
   * 游戏没有显式声明候选顺序时，按前台状态自动声明（relay 在房主断线、来不及转交时按它选人）。没有任何上报时不发：
   * 这时的顺序与 relay 的兜底规则（槽位最小的在线成员）相同。
   */
  private declareForegroundFirst(): void {
    if (this.client.hostRole !== "host" || this.client.successorsDeclared || this.presence.size === 0) return;
    const order = this.foregroundFirst();
    if (order.length === this.autoSuccessors.length && order.every((slot, i) => slot === this.autoSuccessors[i])) return;
    this.autoSuccessors = order;
    this.client[internalSuccessors](order);
  }

  private async enter(ticket: { room: { id: string }; relayAddr: string; token: string }): Promise<Welcome> {
    const welcome = await this.client.connect(ticket);
    this.roomId = ticket.room.id;
    await this.entered(welcome);
    return welcome;
  }

  /** 进房（含自动重连）后按角色开停规则：是房主就从存档（刷新）或空状态开始。 */
  private async entered(welcome: Welcome): Promise<void> {
    if (!welcome.host) {
      this.runner.stop();
      this.store?.clear(); // 已经不是房主：存档作废
      return;
    }
    if (this.runner.running) return; // 同一页面里的自动重连：规则一直在跑
    const saved = this.store?.load(this.options.account);
    let state: Uint8Array | null = null;
    if (saved && saved.room === this.roomId && saved.state) {
      this.client.raiseSnapshotSeq(saved.seq);
      try {
        state = await unpackState(this.compressor, saved.state, MAX_STATE_BYTES);
      } catch {
        state = null;
      }
    }
    this.saveIdentity();
    this.runner.start(state);
    if (this.doc?.visibilityState === "hidden") this.visibilityChanged();
  }

  private saveIdentity(): void {
    if (!this.store) return;
    const old = this.store.load(this.options.account);
    const same = old && old.room === this.roomId;
    this.store.save({
      room: this.roomId,
      game: this.options.relay.game,
      account: this.options.account,
      seq: same ? old.seq : this.client.snapshotSeq,
      state: same ? old.state : null,
      savedAt: Date.now(),
    });
  }

  /**
   * 接到 RelayClient 的内部监听上（不占业务的 onXxx 槽位）：业务在 webHost.client 上照样可以注册任何回调，
   * 不会把这里顶掉。
   */
  private wire(): void {
    const client = this.client;
    this.runner.onStep = (frames) => {
      client.pulse(); // Worker 的消息不受主线程计时器限流：顺带让房主的心跳按时发
      if (client.hostRole !== "host") return;
      const self = client.welcome.slot;
      for (const out of frames) {
        if (out.target === ALL || out.target === self) this.frameHandler?.({ channel: RelayChannel.Game, slot: self, msgId: out.msgId, flags: out.flags ?? 0, payload: out.payload });
        if (out.target !== self) client.sendGame(out.target, out.msgId, out.payload, out.flags ?? 0);
      }
    };
    client[internalHooks]({
      game: (frame) => {
        if (client.hostRole === "host") this.runner.input(frame.slot, frame.msgId, frame.payload);
        else this.frameHandler?.(frame);
      },
      youAreHost: (takeover) => {
        this.runner.start(takeover.state);
        this.saveIdentity();
        if (this.doc?.visibilityState === "hidden") this.visibilityChanged();
      },
      youAreMember: () => this.runner.stop(),
      hostChanged: (change) => {
        // 新房主等成员重发前台状态（它们收到同一条 HostChanged 后各自重发）；relay 已清空旧的候选顺序
        this.presence.clear();
        this.autoSuccessors = [];
        if (!change.self) {
          this.runner.stop();
          this.store?.clear();
        }
      },
      memberPresence: (m) => {
        this.presence.set(m.slot, m.foreground);
        this.declareForegroundFirst();
      },
      memberJoined: () => this.declareForegroundFirst(),
      memberLeft: () => this.declareForegroundFirst(),
      snapshotSent: (sent) => {
        this.store?.save({ room: this.roomId, game: this.options.relay.game, account: this.options.account, seq: sent.seq, state: sent.data, savedAt: Date.now() });
      },
      reconnected: (welcome) => void this.entered(welcome),
      disconnected: (d) => {
        if (d.reconnecting) return;
        this.runner.stop();
        // 刷新页面时 SDK 不会走到这里（页面直接没了）；走到这里说明这一局对本页面结束了
        if (d.reason !== RelayReason.None) this.store?.clear();
      },
    });
  }

  private visibilityChanged(): void {
    if (this.doc?.visibilityState !== "hidden") {
      this.cancelHidden(); // 回到前台：不自动抢回房主
      return;
    }
    if (this.client.hostRole !== "host" || this.hiddenHandoffMs < 0 || this.hiddenTimer !== undefined) return;
    this.hiddenTimer = this.timers.setTimeout(() => {
      this.hiddenTimer = undefined;
      if (this.doc?.visibilityState !== "hidden") return;
      void this.handOff().then(
        (target) => this.handoffHandler?.(target),
        () => this.handoffHandler?.(undefined),
      );
    }, this.hiddenHandoffMs);
  }

  private cancelHidden(): void {
    if (this.hiddenTimer === undefined) return;
    this.timers.clearTimeout(this.hiddenTimer);
    this.hiddenTimer = undefined;
  }
}

function globalStorage(): StorageLike | undefined {
  try {
    return (globalThis as { sessionStorage?: StorageLike }).sessionStorage;
  } catch {
    return undefined; // 被禁用时访问就抛错
  }
}
