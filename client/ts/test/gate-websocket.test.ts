// 玩家接入 SDK 对假玩家接入（node 的 ws 包，test/gate-fake.ts）：照 C# 的 TestWebSocketMessageBoundaries、
// HeartbeatCapacityTests、DisposalTests 核对——
//   - 客户端每条消息恰好一帧；鉴权首包带代号信封，代号非 0、重连后更大；不带 Origin（node 的 ws 不带，浏览器的路径在冒烟里验）；
//   - 握手应答拆成两条消息、应答与推送合成一条、一帧拆成 7 字节一条、接近 1 MiB 的大帧，都能拼回；
//   - 回调形式按线上顺序派发；Promise 形式的续行在同一条消息的回调之后；
//   - 文本消息断开连接，lastError 是协议错误；
//   - 请求超时后迟到的应答丢弃、连接保留；在途上限；心跳独占槽位；
//   - 回调里 dispose：同一条消息里剩下的帧不再派发，之后不再通知。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  ConnectionState,
  GameClient,
  GateClosedError,
  GateProtocolError,
  GateTimeoutError,
  BrowserWebSocketTransport,
  PayloadKind,
  type ConnectionOptions,
  type Packet,
  type ReconnectResult,
} from "../src/gate/index.js";
import { FakeGate, control, push, reply, sleep, text, until, utf8, nodeWebSocket } from "./gate-fake.js";

function options(url: string, extra: Partial<ConnectionOptions> = {}): ConnectionOptions {
  return {
    webSocketUrl: url,
    transportFactory: () => new BrowserWebSocketTransport({ webSocket: nodeWebSocket }),
    heartbeatIntervalMs: 0,
    ...extra,
  };
}

async function withGate(body: (gate: FakeGate) => Promise<void>): Promise<void> {
  const gate = await FakeGate.start();
  try {
    await body(gate);
    assert.deepEqual(gate.violations(), [], "every client message is exactly one binary frame");
  } finally {
    await gate.close();
  }
}

test("handshake over a split acknowledgement; auth envelope with a generation; no Origin header", async () => {
  await withGate(async (gate) => {
    const client = new GameClient(options(gate.url));
    const states: ConnectionState[] = [];
    client.on("stateChanged", (s) => states.push(s));
    const connecting = client.connection.connect("ws-ticket");
    const conn = await gate.connection(0);
    const auth = await conn.handshake([5]);
    await connecting;
    assert.equal(auth.ticket, "ws-ticket");
    assert.ok(auth.generation > 0n, "the auth frame carries a non-zero generation");
    assert.equal(auth.packet.sequence, 1, "the handshake is the first sequence on the connection");
    assert.equal(conn.request.headers.origin, undefined, "node's ws sends no Origin (the browser smoke covers allowed_origins)");
    assert.equal(client.connection.state, ConnectionState.Ready);
    assert.equal(client.connection.connectionVersion, 1);
    assert.deepEqual(states, [ConnectionState.Connecting, ConnectionState.Ready]);
    client.dispose();
    assert.equal(client.connection.state, ConnectionState.Disposed);
  });
});

test("a response and a push merged in one message, a frame split into 7-byte messages and a near-1 MiB frame all decode", async () => {
  await withGate(async (gate) => {
    const client = new GameClient(options(gate.url));
    const seen: string[] = [];
    client.subscribe(9, text, (value) => seen.push(`push ${value}`));
    const connecting = client.connection.connect("t");
    const conn = await gate.connection(0);
    await conn.handshake();
    await connecting;

    client.request(42, 43, utf8("hello"), text, (value) => seen.push(`reply ${value}`), (error) => seen.push(`failed ${error.message}`));
    const request = await conn.frame();
    assert.deepEqual([request.messageId, text(request.payload)], [42, "hello"]);
    conn.send(reply(request, 43, utf8("world")), push(9, utf8("after")));
    await until("reply and push", () => seen.length === 2);
    assert.deepEqual(seen, ["reply world", "push after"], "callback form follows the wire order (reply first)");

    seen.length = 0;
    client.request(42, 43, utf8("again"), text, (value) => seen.push(`reply ${value}`), (error) => seen.push(`failed ${error.message}`));
    const second = await conn.frame();
    conn.send(push(9, utf8("before")), reply(second, 43, utf8("world2")));
    await until("push and reply", () => seen.length === 2);
    assert.deepEqual(seen, ["push before", "reply world2"], "callback form follows the wire order (push first)");

    // Promise 形式：续行在这条消息的全部帧派发之后。
    seen.length = 0;
    const pending = client.connection.requestAsync(42, 43, utf8("async")).then((packet) => seen.push(`await ${text(packet.payload)}`));
    const third = await conn.frame();
    conn.send(reply(third, 43, utf8("r")), push(9, utf8("same message")));
    await pending;
    assert.deepEqual(seen, ["push same message", "await r"], "a push in the same message runs before the awaited continuation");

    // 一帧拆成 7 字节一条消息。
    seen.length = 0;
    const split = new Uint8Array(Buffer.from("split across many websocket messages"));
    const bytes = (await import("../src/gate/index.js")).encodePacket(push(9, split));
    for (let i = 0; i < bytes.length; i += 7) conn.sendRaw(bytes.subarray(i, i + 7));
    await until("split push", () => seen.length === 1);
    assert.deepEqual(seen, ["push split across many websocket messages"]);

    // 接近 1 MiB 的大帧。
    let big: Uint8Array | undefined;
    client.subscribe(10, (b) => b, (value) => (big = value));
    const payload = new Uint8Array((1 << 20) - 3).map((_, i) => i % 251);
    conn.send(push(10, payload));
    await until("big push", () => big !== undefined);
    assert.equal(big!.length, payload.length);
    assert.ok(big!.every((b, i) => b === i % 251));
    client.dispose();
  });
});

test("a text message closes the connection with a protocol error", async () => {
  await withGate(async (gate) => {
    const client = new GameClient(options(gate.url));
    const states: ConnectionState[] = [];
    client.on("stateChanged", (s) => states.push(s));
    const connecting = client.connection.connect("t");
    const conn = await gate.connection(0);
    await conn.handshake();
    await connecting;
    let failed: Error | undefined;
    client.request(42, 43, utf8("in flight"), text, () => {}, (error) => (failed = error));
    await conn.frame();
    conn.sendRaw("not a frame");
    await until("disconnected", () => client.connection.state === ConnectionState.Disconnected);
    assert.ok(client.connection.lastError instanceof GateProtocolError, `lastError: ${client.connection.lastError}`);
    await until("in-flight request failed", () => failed !== undefined);
    assert.ok(failed instanceof GateClosedError, `in-flight request: ${failed}`);
    assert.deepEqual(states, [ConnectionState.Connecting, ConnectionState.Ready, ConnectionState.Disconnected]);
    await conn.waitClosed();
    client.dispose();
  });
});

test("a request that timed out drops its late reply and keeps the connection", async () => {
  await withGate(async (gate) => {
    const client = new GameClient(options(gate.url, { requestTimeoutMs: 100 }));
    const unhandled: Packet[] = [];
    client.on("unhandledPush", (p) => unhandled.push(p));
    const connecting = client.connection.connect("t");
    const conn = await gate.connection(0);
    await conn.handshake();
    await connecting;
    let failed: Error | undefined;
    let got: string | undefined;
    client.request(42, 43, utf8("slow"), text, (value) => (got = value), (error) => (failed = error));
    const slow = await conn.frame();
    await until("timeout", () => failed !== undefined, 2000);
    assert.ok(failed instanceof GateTimeoutError, `failed: ${failed}`);
    conn.send(reply(slow, 43, utf8("late")));
    client.request(42, 43, utf8("next"), text, (value) => (got = value), (error) => (failed = error));
    const next = await conn.frame();
    conn.send(reply(next, 43, utf8("fresh")));
    await until("next reply", () => got !== undefined);
    assert.equal(got, "fresh", "the late reply is not delivered to anyone");
    assert.equal(unhandled.length, 0, "a late reply is not a push");
    assert.equal(client.connection.state, ConnectionState.Ready);
    client.dispose();
  });
});

test("pending capacity is enforced and the heartbeat keeps its own slot", async () => {
  await withGate(async (gate) => {
    const client = new GameClient(options(gate.url, { maxPendingRequests: 2, heartbeatIntervalMs: 30, requestTimeoutMs: 5000 }));
    const connecting = client.connection.connect("t");
    const conn = await gate.connection(0);
    await conn.handshake();
    await connecting;
    const failures: Error[] = [];
    client.request(42, 43, utf8("a"), text, () => {}, (e) => failures.push(e));
    client.request(42, 43, utf8("b"), text, () => {}, (e) => failures.push(e));
    client.request(42, 43, utf8("c"), text, () => {}, (e) => failures.push(e));
    await until("third request refused", () => failures.length === 1);
    assert.match(failures[0].message, /capacity/);
    const beatsBefore = conn.heartbeats;
    // 服务端回了 3 个心跳，且客户端收到了它们的应答（两边计数各自到达，等两者）。
    await until("heartbeats while the business slots are full", () => conn.heartbeats >= beatsBefore + 3 && client.connection.heartbeatRoundTrips >= 3, 2000);
    const a = await conn.frame();
    conn.send(reply(a, 43, utf8("a")));
    let d: string | undefined;
    await sleep(10);
    client.request(42, 43, utf8("d"), text, (v) => (d = v), (e) => failures.push(e));
    const b = await conn.frame();
    const dFrame = await conn.frame();
    conn.send(reply(b, 43), reply(dFrame, 43, utf8("d")));
    await until("slot freed by a reply is reusable", () => d === "d");
    assert.equal(failures.length, 1);
    client.dispose();
  });
});

test("disposing inside a callback stops the rest of the message and every later notification", async () => {
  await withGate(async (gate) => {
    const client = new GameClient(options(gate.url));
    const seen: string[] = [];
    const errors: Error[] = [];
    client.on("error", (e) => errors.push(e));
    client.on("stateChanged", (s) => seen.push(`state ${ConnectionState[s]}`));
    client.subscribe(9, text, (value) => {
      seen.push(`push ${value}`);
      if (value === "dispose") client.dispose();
    });
    client.on("unhandledPush", (p) => seen.push(`unhandled ${p.messageId}`));
    const connecting = client.connection.connect("t");
    const conn = await gate.connection(0);
    await conn.handshake();
    await connecting;
    seen.length = 0;
    conn.send(push(9, utf8("first")), push(9, utf8("dispose")), push(9, utf8("after")), push(11, utf8("x")));
    await until("disposed", () => client.disposed);
    await conn.waitClosed();
    await sleep(20);
    assert.deepEqual(seen, ["push first", "push dispose"], "nothing after dispose, not even the state change");
    assert.deepEqual(errors, []);
    assert.throws(() => client.request(42, 43, utf8("x"), text, () => {}, () => {}), /disposed/);
  });
});

test("disconnecting inside a push callback drops the rest of that message", async () => {
  await withGate(async (gate) => {
    const client = new GameClient(options(gate.url));
    const seen: string[] = [];
    client.subscribe(9, text, (value) => {
      seen.push(value);
      if (value === "stop") client.connection.disconnect();
    });
    const connecting = client.connection.connect("t");
    const conn = await gate.connection(0);
    await conn.handshake();
    await connecting;
    conn.send(push(9, utf8("one")), push(9, utf8("stop")), push(9, utf8("dropped")));
    await until("disconnected", () => client.connection.state === ConnectionState.Disconnected);
    await conn.waitClosed();
    await sleep(20);
    assert.deepEqual(seen, ["one", "stop"], "frames after the disconnect in the same message are not delivered");
    assert.equal(client.connection.lastError, undefined, "a caller's disconnect is not an error");
    client.dispose();
  });
});

test("one subscriber per message id; a throwing handler reports to error and the connection stays", async () => {
  await withGate(async (gate) => {
    const client = new GameClient(options(gate.url));
    const errors: Error[] = [];
    client.on("error", (e) => errors.push(e));
    const off = client.subscribe(9, text, () => {
      throw new Error("handler bug");
    });
    assert.throws(() => client.subscribe(9, text, () => {}), /already/);
    const connecting = client.connection.connect("t");
    const conn = await gate.connection(0);
    await conn.handshake();
    await connecting;
    conn.send(push(9, utf8("x")));
    await until("error reported", () => errors.length === 1);
    assert.equal(errors[0].message, "handler bug");
    off();
    const unhandled: number[] = [];
    client.on("unhandledPush", (p) => unhandled.push(p.messageId));
    conn.send(push(9, utf8("y")), push(12, new Uint8Array(0), PayloadKind.Sync));
    await until("unhandled pushes", () => unhandled.length === 2);
    assert.deepEqual(unhandled, [9, 12], "after unsubscribe, and Sync pushes until round 2");
    assert.equal(client.connection.state, ConnectionState.Ready);
    client.dispose();
  });
});

test("auto reconnect after the server closes: fresh ticket, newer generation, reconnected event", async () => {
  await withGate(async (gate) => {
    const client = new GameClient(options(gate.url, { autoReconnect: true, reconnectInitialDelayMs: 20, reconnectMaxDelayMs: 40 }));
    const results: ReconnectResult[] = [];
    const states: ConnectionState[] = [];
    client.on("reconnected", (r) => results.push(r));
    client.on("stateChanged", (s) => states.push(s));
    let tickets = 0;
    const connecting = client.connection.connect(async () => `ticket-${++tickets}`);
    const first = await gate.connection(0);
    const one = await first.handshake();
    await connecting;
    first.close();
    const second = await gate.connection(1);
    const two = await second.handshake();
    await until("reconnected", () => results.length === 1);
    assert.deepEqual([one.ticket, two.ticket], ["ticket-1", "ticket-2"], "every handshake fetches a fresh ticket");
    assert.ok(two.generation > one.generation, `generation ${two.generation} > ${one.generation}`);
    assert.deepEqual(results[0], { attempt: 1, connectionVersion: 2, willRetry: false });
    assert.equal(client.connection.connectionVersion, 2);
    assert.deepEqual(states, [ConnectionState.Connecting, ConnectionState.Ready, ConnectionState.Reconnecting, ConnectionState.Ready]);
    client.dispose();
  });
});

test("a non-empty handshake acknowledgement fails the connect", async () => {
  await withGate(async (gate) => {
    const client = new GameClient(options(gate.url));
    const connecting = client.connection.connect("t");
    const conn = await gate.connection(0);
    const auth = await conn.frame();
    conn.send(control(auth.sequence, utf8("not empty")));
    await assert.rejects(connecting, GateProtocolError);
    assert.equal(client.connection.state, ConnectionState.Disconnected);
    client.dispose();
  });
});
