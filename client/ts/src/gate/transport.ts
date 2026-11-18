// SDK 与玩家接入之间的一条连接（对应 C# IGateTransport，维护者 2026-11-18 D7：以后的微信 / 抖音小游戏传输实现同一个接口，
// 放进 SDK，上层不改）。浏览器没有 TCP，第一版只有 WebSocket 一种实现。

import { WEBSOCKET_OPEN, type WebSocketLike } from "../wire/websocket.js";
import { GateProtocolError } from "./packet.js";

/**
 * 一条连接。一个实例只连一次；重连时 SDK 向工厂要新的。
 *
 * 与 C# IGateTransport 的两点差别都来自单线程：send 是同步的（浏览器与小游戏的 send 都不阻塞），收字节是回调而不是拉取。
 */
export interface GateTransport {
  /** 连到 url（ws:// 或 wss://）。失败、或在连上之前被 close 打断时 reject。 */
  connect(url: string): Promise<void>;
  /** 发一整帧（恰好一条二进制消息，GATEWAY §6），同步返回；连接已断时抛错。SDK 保证不并发调用（单线程）。 */
  send(frame: Uint8Array): void;
  /** 立即关闭，可重复调用；之后不再回调 onData / onClose。 */
  close(): void;
  /** 收到的字节，按到达顺序，不要求与帧对齐（SDK 自己拼帧）。 */
  onData: ((chunk: Uint8Array) => void) | null;
  /** 连上之后连接断开（对端关闭、出错、收到文本消息）；error 为空表示对端正常关闭。只回调一次；SDK 自己调 close 时不回调。 */
  onClose: ((error?: Error) => void) | null;
}

export type GateTransportFactory = () => GateTransport;

export interface BrowserWebSocketTransportOptions {
  /** 建 WebSocket；缺省 `new globalThis.WebSocket(url)`（浏览器）。node 里传 ws 包：`(url) => new WebSocket(url)`。 */
  webSocket?: (url: string) => WebSocketLike;
}

/** url 是不是 ws:// 或 wss:// 地址（SDK 不从 TCP 地址推算，维护者 2026-11-18 D8）。 */
export function isWebSocketUrl(url: string): boolean {
  return /^wss?:\/\/[^/?#\s]+/i.test(url);
}

/**
 * 浏览器 WebSocket（binaryType = "arraybuffer"）。缺省实现；node 的 ws 包也满足 WebSocketLike，测试与浏览器共用这一份。
 * 文本消息不是 RS 帧（服务端也以 1003 拒收文本），当协议错误关闭。浏览器不让页面设 WebSocket 的请求头，会带 Origin，
 * 所以服务端的 allowed_origins 要放行页面来源（GATEWAY §6）。
 */
export class BrowserWebSocketTransport implements GateTransport {
  onData: ((chunk: Uint8Array) => void) | null = null;
  onClose: ((error?: Error) => void) | null = null;
  private socket: WebSocketLike | null = null;
  private closed = false;
  private abortConnect: ((error: Error) => void) | null = null;

  constructor(private readonly options: BrowserWebSocketTransportOptions = {}) {}

  connect(url: string): Promise<void> {
    if (this.socket || this.closed) return Promise.reject(new Error("gate: a transport connects only once"));
    if (!isWebSocketUrl(url)) return Promise.reject(new Error(`gate: ${url} is not a ws:// or wss:// URL`));
    return new Promise((resolve, reject) => {
      let socket: WebSocketLike;
      try {
        socket = this.options.webSocket ? this.options.webSocket(url) : BrowserWebSocketTransport.global(url);
      } catch (error) {
        reject(error);
        return;
      }
      this.socket = socket;
      socket.binaryType = "arraybuffer";
      let opened = false;
      this.abortConnect = (error) => {
        if (opened) return;
        opened = true;
        reject(error);
      };
      socket.onopen = () => {
        if (opened) return;
        opened = true;
        this.abortConnect = null;
        resolve();
      };
      socket.onerror = () => {
        // 浏览器的 error 事件不带原因，随后总有 close 事件；连上之前的失败在这里结束 connect。
        if (!opened) this.fail(new Error(`gate: WebSocket to ${url} failed`));
      };
      socket.onclose = (event) => {
        const detail = describeClose(event);
        if (!opened) {
          this.fail(new Error(`gate: WebSocket to ${url} closed before it opened${detail}`));
          return;
        }
        this.end(detail ? new Error(`gate: WebSocket closed${detail}`) : undefined);
      };
      socket.onmessage = (event) => {
        if (this.closed) return;
        const data = event.data;
        let chunk: Uint8Array;
        if (data instanceof ArrayBuffer) {
          chunk = new Uint8Array(data);
        } else if (ArrayBuffer.isView(data)) {
          chunk = new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
        } else {
          this.end(new GateProtocolError("a text WebSocket message is not an RS frame"));
          return;
        }
        this.onData?.(chunk);
      };
    });
  }

  send(frame: Uint8Array): void {
    if (this.closed || !this.socket || this.socket.readyState !== WEBSOCKET_OPEN) {
      throw new Error("gate: WebSocket is not open");
    }
    this.socket.send(frame);
  }

  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.abortConnect?.(new Error("gate: WebSocket closed while connecting"));
    this.abortConnect = null;
    this.onData = null;
    this.onClose = null;
    this.detach();
  }

  private fail(error: Error): void {
    const abort = this.abortConnect;
    this.abortConnect = null;
    this.closed = true;
    this.detach();
    abort?.(error);
  }

  /** 连上之后的断开：只通知一次，然后放开 socket。 */
  private end(error?: Error): void {
    if (this.closed) return;
    this.closed = true;
    const onClose = this.onClose;
    this.onData = null;
    this.onClose = null;
    this.detach();
    onClose?.(error);
  }

  private detach(): void {
    const socket = this.socket;
    if (!socket) return;
    socket.onopen = socket.onmessage = socket.onerror = socket.onclose = null;
    try {
      socket.close(1000);
    } catch {
      // 还在 CONNECTING 的 socket 在部分实现里 close 会抛；连接反正要放弃。
    }
  }

  private static global(url: string): WebSocketLike {
    const Ctor = (globalThis as { WebSocket?: new (url: string) => WebSocketLike }).WebSocket;
    if (!Ctor) throw new Error("gate: no global WebSocket; pass webSocket (node: the ws package)");
    return new Ctor(url);
  }
}

function describeClose(event: unknown): string {
  const close = event as { code?: number; reason?: string } | null;
  if (!close || typeof close.code !== "number" || close.code === 1000) return "";
  return ` (code ${close.code}${close.reason ? `: ${close.reason}` : ""})`;
}
