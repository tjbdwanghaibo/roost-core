// 玩家接入 SDK 测试的替身：
//   - FakeGate：node 里用 ws 包起的假玩家接入。逐条核对“客户端每条二进制消息恰好一帧”（GATEWAY §6），按用例脚本回帧，
//     可以把一帧拆成几条消息、几帧合成一条消息、发文本消息；
//   - FakeClock：注入 ConnectionOptions.timers 的假时钟，advance 推进并依次触发到期的计时；
//   - ScriptedTransport：不走网络的 GateTransport，用例脚本决定连不连得上、怎么回帧（计时用例用它配 FakeClock）。
import WebSocket, { WebSocketServer } from "ws";
import type { IncomingMessage } from "node:http";
import { PayloadKind, decodeAuth, decodePacket, encodePacket, type GateTimers, type GateTransport, type Packet, type WebSocketLike } from "../src/gate/index.js";
import { GateFrameReader } from "../src/gate/packet.js";

/** node 的 ws 包当浏览器 WebSocket 用（事件参数类型比 WebSocketLike 窄，只差在类型上）。 */
export const nodeWebSocket = (url: string) => new WebSocket(url) as unknown as WebSocketLike;
export const utf8 = (text: string) => new Uint8Array(Buffer.from(text));
export const text = (data: Uint8Array) => Buffer.from(data).toString();
export const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

export function reply(request: Packet, messageId: number, payload: Uint8Array = new Uint8Array(0)): Packet {
  return { messageId, sequence: request.sequence, isPush: false, kind: PayloadKind.Protobuf, payload };
}

export function push(messageId: number, payload: Uint8Array, kind = PayloadKind.Protobuf): Packet {
  return { messageId, sequence: 0, isPush: true, kind, payload };
}

export function control(sequence: number, payload: Uint8Array = new Uint8Array(0)): Packet {
  return { messageId: 0, sequence, isPush: false, kind: PayloadKind.Protobuf, payload };
}

/** 等 condition 成立，最多 ms 毫秒。 */
export async function until(what: string, condition: () => boolean, ms = 3000): Promise<void> {
  const deadline = Date.now() + ms;
  while (!condition()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await sleep(2);
  }
}

/** 假玩家接入上的一条连接。 */
export class FakeConn {
  readonly frames: Packet[] = [];
  readonly violations: string[] = [];
  heartbeats = 0;
  /** 为 true 时自动回心跳（MsgID=0、空载荷），frame() 不返回心跳。 */
  autoHeartbeat = true;
  closed = false;
  private waiters: ((packet: Packet) => void)[] = [];
  private closeWaiters: (() => void)[] = [];

  constructor(
    readonly socket: WebSocket,
    readonly request: IncomingMessage,
  ) {
    socket.on("message", (data, isBinary) => {
      const message = new Uint8Array(data as Buffer);
      if (!isBinary) {
        this.violations.push("the client sent a text message");
        return;
      }
      let packet: Packet;
      try {
        packet = decodePacket(message); // 恰好一帧，多一个字节都不行
      } catch (error) {
        this.violations.push(`a client message of ${message.length} bytes is not exactly one frame: ${(error as Error).message}`);
        return;
      }
      if (this.autoHeartbeat && packet.messageId === 0 && packet.payload.length === 0 && this.handshaken) {
        this.heartbeats++;
        this.send(control(packet.sequence));
        return;
      }
      const waiter = this.waiters.shift();
      if (waiter) waiter(packet);
      else this.frames.push(packet);
    });
    socket.on("close", () => {
      this.closed = true;
      for (const waiter of this.closeWaiters.splice(0)) waiter();
    });
  }

  private handshaken = false;

  /** 下一帧（不含自动回过的心跳）。 */
  frame(ms = 3000): Promise<Packet> {
    const queued = this.frames.shift();
    if (queued) return Promise.resolve(queued);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error("fake gate: no frame from the client in time")), ms);
      this.waiters.push((packet) => {
        clearTimeout(timer);
        resolve(packet);
      });
    });
  }

  /** 读鉴权首包并回应答；ackParts 把应答拆成几条消息（按给出的切点）。 */
  async handshake(ackSplit: number[] = []): Promise<{ ticket: string; generation: bigint; packet: Packet }> {
    const packet = await this.frame();
    if (packet.messageId !== 0) throw new Error(`fake gate: first frame has message id ${packet.messageId}`);
    const { ticket, generation } = decodeAuth(packet.payload);
    this.handshaken = true;
    const ack = encodePacket(control(packet.sequence));
    let at = 0;
    for (const cut of [...ackSplit, ack.length]) {
      this.sendRaw(ack.subarray(at, cut));
      at = cut;
    }
    return { ticket: text(ticket), generation, packet };
  }

  /** 几帧合成一条二进制消息发出。 */
  send(...packets: Packet[]): void {
    const parts = packets.map((p) => encodePacket(p));
    const joined = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
    let at = 0;
    for (const part of parts) {
      joined.set(part, at);
      at += part.length;
    }
    this.sendRaw(joined);
  }

  sendRaw(data: Uint8Array | string): void {
    this.socket.send(data, { binary: typeof data !== "string" });
  }

  /** 关连接（对端主动断开）。 */
  close(): void {
    this.socket.close();
  }

  waitClosed(ms = 3000): Promise<void> {
    if (this.closed) return Promise.resolve();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error("fake gate: the client did not close in time")), ms);
      this.closeWaiters.push(() => {
        clearTimeout(timer);
        resolve();
      });
    });
  }
}

export class FakeGate {
  readonly connections: FakeConn[] = [];
  private waiters: ((conn: FakeConn) => void)[] = [];

  private constructor(
    private readonly server: WebSocketServer,
    readonly url: string,
  ) {
    server.on("connection", (socket, request) => {
      const conn = new FakeConn(socket, request);
      this.connections.push(conn);
      const waiter = this.waiters.shift();
      if (waiter) waiter(conn);
    });
  }

  static start(): Promise<FakeGate> {
    return new Promise((resolve, reject) => {
      const server = new WebSocketServer({ host: "127.0.0.1", port: 0, path: "/ws", maxPayload: 64 << 20 });
      server.once("error", reject);
      server.once("listening", () => {
        const address = server.address();
        if (typeof address === "string" || !address) {
          reject(new Error("fake gate: no address"));
          return;
        }
        resolve(new FakeGate(server, `ws://127.0.0.1:${address.port}/ws`));
      });
    });
  }

  /** 第 index 条连接（从 0 起），还没来就等。 */
  connection(index: number, ms = 3000): Promise<FakeConn> {
    if (this.connections[index]) return Promise.resolve(this.connections[index]);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`fake gate: connection ${index} did not arrive`)), ms);
      const check = () => {
        if (this.connections[index]) {
          clearTimeout(timer);
          resolve(this.connections[index]);
        } else {
          this.waiters.push(check);
        }
      };
      check();
    });
  }

  violations(): string[] {
    return this.connections.flatMap((conn) => conn.violations);
  }

  close(): Promise<void> {
    for (const client of this.server.clients) client.terminate();
    return new Promise((resolve) => this.server.close(() => resolve()));
  }
}

/** 假时钟：setTimeout 只登记，advance 按到期先后触发，每触发一个都让微任务与 I/O 回调先跑完。 */
export class FakeClock implements GateTimers {
  private time = 0;
  private nextId = 0;
  private readonly timers = new Map<number, { at: number; callback: () => void }>();

  setTimeout(callback: () => void, ms: number): unknown {
    const id = ++this.nextId;
    this.timers.set(id, { at: this.time + Math.max(0, ms), callback });
    return id;
  }

  clearTimeout(handle: unknown): void {
    this.timers.delete(handle as number);
  }

  now(): number {
    return this.time;
  }

  /** 还挂着的计时的到期时刻（相对现在），从早到晚。 */
  pending(): number[] {
    return [...this.timers.values()].map((t) => t.at - this.time).sort((a, b) => a - b);
  }

  async advance(ms: number): Promise<void> {
    const target = this.time + ms;
    await settle();
    for (;;) {
      let due: [number, { at: number; callback: () => void }] | undefined;
      for (const entry of this.timers) {
        if (entry[1].at <= target && (!due || entry[1].at < due[1].at)) due = entry;
      }
      if (!due) break;
      this.timers.delete(due[0]);
      this.time = due[1].at;
      due[1].callback();
      await settle();
    }
    this.time = target;
    await settle();
  }
}

/** 让已排队的微任务与 setImmediate 都跑完。 */
export async function settle(): Promise<void> {
  for (let i = 0; i < 5; i++) await new Promise<void>((resolve) => setImmediate(resolve));
}

/**
 * 不走网络的传输。peer 收到客户端的每一帧（已解码），用 transport.deliver 回字节；connectMode 决定 connect 的结果。
 * 每个实例记在 ScriptedNetwork 里，用例据此看第几条连接发了什么。
 */
export class ScriptedTransport implements GateTransport {
  onData: ((chunk: Uint8Array) => void) | null = null;
  onClose: ((error?: Error) => void) | null = null;
  readonly sent: Packet[] = [];
  closedByClient = false;
  private readonly reader = new GateFrameReader();
  private connectReject: ((error: Error) => void) | null = null;

  constructor(private readonly network: ScriptedNetwork) {}

  connect(url: string): Promise<void> {
    this.network.urls.push(url);
    const mode = this.network.connectMode();
    if (mode === "fail") return Promise.reject(new Error("scripted: connection refused"));
    if (mode === "hang") {
      return new Promise((_, reject) => {
        this.connectReject = reject;
      });
    }
    return Promise.resolve();
  }

  send(frame: Uint8Array): void {
    if (this.closedByClient) throw new Error("scripted: send after close");
    for (const packet of this.reader.push(frame)) {
      this.sent.push(packet);
      this.network.peer(packet, this);
    }
  }

  close(): void {
    if (this.closedByClient) return;
    this.closedByClient = true;
    this.connectReject?.(new Error("scripted: closed while connecting"));
    this.connectReject = null;
    this.onData = null;
    this.onClose = null;
  }

  /** 对端发来字节（同步交给 SDK，像 WebSocket 的 message 事件）。 */
  deliver(...packets: Packet[]): void {
    for (const packet of packets) this.onData?.(encodePacket(packet));
  }

  /** 对端断开。 */
  drop(error?: Error): void {
    const onClose = this.onClose;
    this.onData = null;
    this.onClose = null;
    onClose?.(error);
  }
}

export class ScriptedNetwork {
  readonly transports: ScriptedTransport[] = [];
  readonly urls: string[] = [];
  /** 依次决定每次 connect 的结果；用完之后都是 "ok"。 */
  connects: ("ok" | "fail" | "hang")[] = [];
  /** 自动回鉴权首包与心跳（为 false 时用例自己回）。 */
  ackAuth = true;
  ackHeartbeats = true;
  /** 用例自己的处理（在自动应答之后调用，自动应答过的帧不再交给它）。 */
  onFrame: (packet: Packet, transport: ScriptedTransport) => void = () => {};

  factory = (): GateTransport => {
    const transport = new ScriptedTransport(this);
    this.transports.push(transport);
    return transport;
  };

  connectMode(): "ok" | "fail" | "hang" {
    return this.connects.shift() ?? "ok";
  }

  peer(packet: Packet, transport: ScriptedTransport): void {
    if (packet.messageId === 0 && transport.sent.length === 1) {
      if (this.ackAuth) {
        transport.deliver(control(packet.sequence));
        return;
      }
    } else if (packet.messageId === 0 && packet.payload.length === 0 && this.ackHeartbeats) {
      transport.deliver(control(packet.sequence));
      return;
    }
    this.onFrame(packet, transport);
  }

  get last(): ScriptedTransport {
    return this.transports[this.transports.length - 1];
  }
}
