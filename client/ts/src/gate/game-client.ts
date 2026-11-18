// 引擎无关的业务入口（对应 C# GameClient）：订阅推送、回调形式的请求与校时、错误 / 状态 / 重连 / 未处理推送事件。
//
// 回调时机（维护者 2026-11-18 D6：立即派发，不做 Pump）：推送、请求的 received / failed、stateChanged 都在 WebSocket 的
// message / close 事件或计时器回调里同步调用。一条消息里有几帧就按线上顺序逐帧处理，所以回调形式的应答与推送严格按线上
// 顺序到达；Connection.requestAsync 的续行在微任务里，晚于同一条消息里的推送回调——顺序要紧的业务用回调形式。
// 回调可以重入调用 SDK（例如在回调里 dispose 或再发请求）：每次通知前检查 disposed，dispose 之后这条消息里剩下的帧不再派发。
// 回调抛错交给 "error" 监听者，不断开连接（"error" 监听者自己的错误忽略）。

import { Connection } from "./connection.js";
import type { ClockSample } from "./clock.js";
import { GateClosedError, type ConnectionOptions, type ConnectionState, type ReconnectResult } from "./options.js";
import { PayloadKind, type Packet } from "./packet.js";

export interface GameClientEvents {
  error: Error;
  stateChanged: ConnectionState;
  reconnected: ReconnectResult;
  /** 没有订阅者的推送，以及 Sync / Lockstep 推送（自身视图同步在第 2 轮）。 */
  unhandledPush: Packet;
}

type Listener<T> = (value: T) => void;

export class GameClient {
  readonly connection: Connection;
  private readonly handlers = new Map<number, (packet: Packet) => void>();
  private readonly listeners: { [K in keyof GameClientEvents]: Listener<GameClientEvents[K]>[] } = {
    error: [],
    stateChanged: [],
    reconnected: [],
    unhandledPush: [],
  };
  private isDisposed = false;

  constructor(options: ConnectionOptions) {
    this.connection = new Connection(options);
    this.connection.on("push", (packet) => this.dispatchPush(packet));
    this.connection.on("stateChanged", (state) => this.notify("stateChanged", state));
    this.connection.on("reconnected", (result) => this.notify("reconnected", result));
    this.connection.on("error", (error) => this.report(error));
  }

  get disposed(): boolean {
    return this.isDisposed;
  }

  /** 订阅事件，返回取消函数。 */
  on<K extends keyof GameClientEvents>(event: K, listener: Listener<GameClientEvents[K]>): () => void {
    this.checkDisposed();
    const list = this.listeners[event] as Listener<GameClientEvents[K]>[];
    list.push(listener);
    return () => {
      const at = list.indexOf(listener);
      if (at >= 0) list.splice(at, 1);
    };
  }

  /**
   * 订阅一个推送号：parse 可以直接用生成 PB 的 fromBinary（或 @roost/client/protobuf 的 subscribe）。每个消息号只能有一位
   * 订阅者（避免页面重复注册把同一份奖励展示两遍），重复订阅抛错；返回取消函数。
   */
  subscribe<T>(messageId: number, parse: (payload: Uint8Array) => T, receive: (value: T) => void): () => void {
    this.checkDisposed();
    if (!Number.isInteger(messageId) || messageId <= 0) throw new RangeError("gate: subscribe needs a business message id");
    if (this.handlers.has(messageId)) throw new Error(`gate: message id ${messageId} already has a subscriber`);
    const handler = (packet: Packet) => {
      let value: T;
      try {
        value = parse(packet.payload);
      } catch (error) {
        this.report(error);
        return;
      }
      this.invoke(() => receive(value));
    };
    this.handlers.set(messageId, handler);
    return () => {
      if (this.handlers.get(messageId) === handler) this.handlers.delete(messageId);
    };
  }

  /**
   * 发请求。业务错误码仍在生成的 PB 应答里；failed 只表示传输、超时、中止、断线或解析失败（连接不可用、在途已满时在微任务里
   * 调用 failed）。请求结果未知时不自动重发。
   */
  request<T>(
    messageId: number,
    responseId: number,
    payload: Uint8Array,
    parse: (payload: Uint8Array) => T,
    received: (value: T) => void,
    failed: (error: Error) => void,
    signal?: AbortSignal,
  ): void {
    this.checkDisposed();
    if (typeof parse !== "function" || typeof received !== "function" || typeof failed !== "function") {
      throw new TypeError("gate: request needs parse, received and failed callbacks");
    }
    this.begin(failed, () =>
      this.connection.beginRequest(
        messageId,
        responseId,
        payload,
        (error, packet) => {
          if (this.isDisposed) return;
          if (error) return this.invoke(() => failed(error));
          let value: T;
          try {
            value = parse(packet!.payload);
          } catch (failure) {
            return this.invoke(() => failed(asError(failure)));
          }
          this.invoke(() => received(value));
        },
        signal,
      ),
    );
  }

  /** 校时探测（回调形式）。样本交给 NetworkClock.observe；重连后 NetworkClock 要 reset。 */
  probeClock(received: (sample: ClockSample) => void, failed: (error: Error) => void, signal?: AbortSignal): void {
    this.checkDisposed();
    this.begin(failed, () =>
      this.connection.beginProbeClock((error, sample) => {
        if (this.isDisposed) return;
        if (error) this.invoke(() => failed(error));
        else this.invoke(() => received(sample!));
      }, signal),
    );
  }

  /** 断开并销毁：之后不再有任何通知，调用其他方法抛 GateClosedError。可重复调用。 */
  dispose(): void {
    if (this.isDisposed) return;
    this.isDisposed = true;
    this.handlers.clear();
    this.connection.dispose();
    for (const list of Object.values(this.listeners)) list.length = 0;
  }

  // ---------- 内部 ----------

  /** start 同步抛出的错误（连接不可用、在途已满、参数错）在微任务里交给 failed，不在调用栈里重入业务。 */
  private begin(failed: (error: Error) => void, start: () => void): void {
    try {
      start();
    } catch (error) {
      if (error instanceof RangeError || error instanceof TypeError) throw error; // 调用方的参数错误照常抛出
      queueMicrotask(() => {
        if (!this.isDisposed) this.invoke(() => failed(asError(error)));
      });
    }
  }

  private dispatchPush(packet: Packet): void {
    if (this.isDisposed) return;
    const handler = packet.kind === PayloadKind.Protobuf ? this.handlers.get(packet.messageId) : undefined;
    if (handler) handler(packet);
    else this.notify("unhandledPush", packet);
  }

  private notify<K extends keyof GameClientEvents>(event: K, value: GameClientEvents[K]): void {
    for (const listener of [...(this.listeners[event] as Listener<GameClientEvents[K]>[])]) {
      if (this.isDisposed) return;
      this.invoke(() => listener(value));
    }
  }

  private invoke(action: () => void): void {
    try {
      action();
    } catch (error) {
      this.report(error);
    }
  }

  private report(error: unknown): void {
    for (const listener of [...this.listeners.error]) {
      if (this.isDisposed) return;
      try {
        listener(asError(error));
      } catch {
        // 错误观察者自身失败不能回流成网络错误。
      }
    }
  }

  private checkDisposed(): void {
    if (this.isDisposed) throw new GateClosedError("gate: GameClient disposed");
  }
}

function asError(error: unknown): Error {
  return error instanceof Error ? error : new Error(String(error));
}
