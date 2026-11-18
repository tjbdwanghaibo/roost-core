// 连接选项、状态与错误类型（对应 C# ConnectionOptions、ConnectionState、ReconnectResult）。时间一律毫秒。

import { BrowserWebSocketTransport, isWebSocketUrl, type GateTransportFactory } from "./transport.js";
import { DEFAULT_MAX_PAYLOAD } from "../wire/header.js";

export enum ConnectionState {
  Disconnected = 0,
  Connecting = 1,
  Reconnecting = 2,
  Ready = 3,
  Disposed = 4,
}

/** 一次重连握手的结果；失败且 willRetry 为 false 表示这一轮不再重试（等业务显式 connect）。 */
export interface ReconnectResult {
  attempt: number;
  connectionVersion: number;
  error?: Error;
  willRetry: boolean;
}

/** 取票：每次握手（含自动重连）调一次，signal 在连接超时或断开时中止。 */
export type TicketProvider = (signal: AbortSignal) => Promise<string>;

/**
 * 计时与时钟。缺省用全局的 setTimeout / clearTimeout 与 performance.now()；测试注入假时钟，Worker 驱动的环境也可以换。
 * now 是毫秒、单调。
 */
export interface GateTimers {
  setTimeout(callback: () => void, ms: number): unknown;
  clearTimeout(handle: unknown): void;
  now(): number;
}

export const defaultTimers: GateTimers = {
  setTimeout: (callback, ms) => globalThis.setTimeout(callback, ms),
  clearTimeout: (handle) => globalThis.clearTimeout(handle as ReturnType<typeof setTimeout>),
  now: () => (globalThis.performance ? globalThis.performance.now() : Date.now()),
};

export interface ConnectionOptions {
  /**
   * 必填：ws:// 或 wss:// 完整地址，通常取 portal 进服应答的地址（进服请求带 "transport":"ws"），路径与服务端
   * player_access.{tcp,gate}.websocket.path 一致（缺省 /ws）。SDK 不从 TCP 地址推算（维护者 2026-11-18 D8）。
   */
  webSocketUrl: string;
  /** 每次连接（含自动重连）新建一个传输；缺省 () => new BrowserWebSocketTransport()（浏览器全局 WebSocket）。 */
  transportFactory?: GateTransportFactory;
  /** 取票与握手的总预算，缺省 10000。 */
  connectTimeoutMs?: number;
  /** 请求等应答的预算，缺省 10000；超时只取消等待，不能推断服务端没执行。 */
  requestTimeoutMs?: number;
  /** 心跳间隔，缺省 15000；0 关闭心跳（会被服务端 idle_timeout 断开，缺省 90s）。 */
  heartbeatIntervalMs?: number;
  /** 心跳等应答的预算，缺省 5000；超时断开连接。 */
  heartbeatTimeoutMs?: number;
  /** 建立连接后异常断线时自动重连（每次重新取票、取新代号），缺省 false：只报告断线。 */
  autoReconnect?: boolean;
  maxReconnectAttempts?: number;
  reconnectInitialDelayMs?: number;
  reconnectMaxDelayMs?: number;
  /** 单包载荷上限，缺省 1 MiB（与服务端 max_payload_bytes 一致）。 */
  maxPayloadBytes?: number;
  /** 在途请求上限，缺省 64；心跳另占一个槽位，不受它限制。 */
  maxPendingRequests?: number;
  /** 计时与时钟（测试、Worker 驱动时注入）。 */
  timers?: GateTimers;
  /** 重连抖动的随机数 [0, 1)，缺省 Math.random（测试注入）。 */
  random?: () => number;
}

/** 校验过、填好缺省值的选项。 */
export interface ResolvedOptions {
  webSocketUrl: string;
  transportFactory: GateTransportFactory;
  connectTimeoutMs: number;
  requestTimeoutMs: number;
  heartbeatIntervalMs: number;
  heartbeatTimeoutMs: number;
  autoReconnect: boolean;
  maxReconnectAttempts: number;
  reconnectInitialDelayMs: number;
  reconnectMaxDelayMs: number;
  maxPayloadBytes: number;
  maxPendingRequests: number;
  timers: GateTimers;
  random: () => number;
}

/** setTimeout 的上限（2^31−1 毫秒），超过时浏览器当 0 处理。 */
const MAX_TIMER_MS = 0x7fff_ffff;

function positive(name: string, value: number): number {
  if (!Number.isFinite(value) || value <= 0 || value > MAX_TIMER_MS) {
    throw new RangeError(`gate: ${name} must be a positive number of milliseconds up to ${MAX_TIMER_MS}`);
  }
  return value;
}

function count(name: string, value: number): number {
  if (!Number.isInteger(value) || value < 1) throw new RangeError(`gate: ${name} must be a positive integer`);
  return value;
}

export function resolveOptions(options: ConnectionOptions): ResolvedOptions {
  if (!options || typeof options.webSocketUrl !== "string" || !isWebSocketUrl(options.webSocketUrl)) {
    throw new TypeError(`gate: webSocketUrl '${options?.webSocketUrl}' is not a ws:// or wss:// URL`);
  }
  const heartbeatIntervalMs = options.heartbeatIntervalMs ?? 15_000;
  if (!Number.isFinite(heartbeatIntervalMs) || heartbeatIntervalMs < 0 || heartbeatIntervalMs > MAX_TIMER_MS) {
    throw new RangeError("gate: heartbeatIntervalMs must be 0 (off) or a positive number of milliseconds");
  }
  const resolved: ResolvedOptions = {
    webSocketUrl: options.webSocketUrl,
    transportFactory: options.transportFactory ?? (() => new BrowserWebSocketTransport()),
    connectTimeoutMs: positive("connectTimeoutMs", options.connectTimeoutMs ?? 10_000),
    requestTimeoutMs: positive("requestTimeoutMs", options.requestTimeoutMs ?? 10_000),
    heartbeatIntervalMs,
    heartbeatTimeoutMs: positive("heartbeatTimeoutMs", options.heartbeatTimeoutMs ?? 5_000),
    autoReconnect: options.autoReconnect ?? false,
    maxReconnectAttempts: count("maxReconnectAttempts", options.maxReconnectAttempts ?? 8),
    reconnectInitialDelayMs: positive("reconnectInitialDelayMs", options.reconnectInitialDelayMs ?? 1_000),
    reconnectMaxDelayMs: positive("reconnectMaxDelayMs", options.reconnectMaxDelayMs ?? 30_000),
    maxPayloadBytes: count("maxPayloadBytes", options.maxPayloadBytes ?? DEFAULT_MAX_PAYLOAD),
    maxPendingRequests: count("maxPendingRequests", options.maxPendingRequests ?? 64),
    timers: options.timers ?? defaultTimers,
    random: options.random ?? Math.random,
  };
  if (resolved.reconnectMaxDelayMs < resolved.reconnectInitialDelayMs) {
    throw new RangeError("gate: reconnectMaxDelayMs must not be less than reconnectInitialDelayMs");
  }
  return resolved;
}

/** 请求、握手或心跳等应答超时。请求超时不能推断服务端没执行。 */
export class GateTimeoutError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "GateTimeoutError";
  }
}

/** 连接不可用（还没连上、已断开、已销毁），或请求所在的连接在应答前断了。cause 是断开原因。 */
export class GateClosedError extends Error {
  constructor(message: string, cause?: unknown) {
    super(message, cause === undefined ? undefined : { cause });
    this.name = "GateClosedError";
  }
}

/** 调用方的 AbortSignal 中止了等待（请求已经发出时同样不能推断服务端没执行）。 */
export class GateAbortedError extends Error {
  constructor(message = "gate: aborted by the caller") {
    super(message);
    this.name = "GateAbortedError";
  }
}
