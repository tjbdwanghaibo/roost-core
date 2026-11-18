// 玩家接入 SDK 的计时（假时钟 FakeClock + 不走网络的 ScriptedTransport）：心跳间隔与“上一个心跳回来才计下一个”、心跳超时断开、
// 取票与握手共用的连接超时、请求超时、重连退避（加抖动、每次重新取票、ReconnectResult 的 willRetry）、断开后不再重连。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  ConnectionState,
  GameClient,
  GateClosedError,
  GateProtocolError,
  GateTimeoutError,
  type ConnectionOptions,
  type ReconnectResult,
} from "../src/gate/index.js";
import { FakeClock, ScriptedNetwork, reply, settle, text, utf8 } from "./gate-fake.js";

function setup(extra: Partial<ConnectionOptions> = {}) {
  const clock = new FakeClock();
  const network = new ScriptedNetwork();
  const client = new GameClient({
    webSocketUrl: "ws://gate.test/ws",
    transportFactory: network.factory,
    timers: clock,
    random: () => 0.5,
    heartbeatIntervalMs: 1000,
    heartbeatTimeoutMs: 300,
    ...extra,
  });
  return { clock, network, client };
}

const heartbeats = (network: ScriptedNetwork) =>
  network.transports.reduce((n, t) => n + t.sent.filter((p, i) => i > 0 && p.messageId === 0 && p.payload.length === 0).length, 0);

test("heartbeats go every interval, the next one is timed from the previous ack", async () => {
  const { clock, network, client } = setup();
  await client.connection.connect("t");
  assert.equal(heartbeats(network), 0);
  await clock.advance(999);
  assert.equal(heartbeats(network), 0, "not before the interval");
  await clock.advance(1);
  assert.equal(heartbeats(network), 1);
  assert.equal(client.connection.heartbeatRoundTrips, 1);
  network.ackHeartbeats = false; // 下一个心跳不回：不发第三个，到超时断开
  await clock.advance(1000);
  assert.equal(heartbeats(network), 2);
  await clock.advance(299);
  assert.equal(client.connection.state, ConnectionState.Ready, "within heartbeatTimeoutMs");
  await clock.advance(1);
  assert.equal(heartbeats(network), 2, "no second heartbeat while one is in flight");
  assert.equal(client.connection.state, ConnectionState.Disconnected, "heartbeat timeout closes the connection");
  assert.ok(client.connection.lastError instanceof GateTimeoutError, `lastError ${client.connection.lastError}`);
  assert.ok(network.last.closedByClient);
  client.dispose();
});

test("heartbeatIntervalMs 0 sends no heartbeat", async () => {
  const { clock, network, client } = setup({ heartbeatIntervalMs: 0 });
  await client.connection.connect("t");
  await clock.advance(60_000);
  assert.equal(heartbeats(network), 0);
  assert.deepEqual(clock.pending(), [], "no timer left behind");
  client.dispose();
});

test("the connect timeout covers the ticket and the handshake", async () => {
  {
    const { clock, client } = setup({ connectTimeoutMs: 500 });
    let aborted = false;
    const connecting = client.connection.connect((signal) => {
      signal.addEventListener("abort", () => (aborted = true));
      return new Promise<string>(() => {}); // 取票卡住
    });
    const outcome = connecting.then(() => "connected", (e: Error) => e);
    await clock.advance(499);
    assert.equal(client.connection.state, ConnectionState.Connecting);
    await clock.advance(1);
    const error = await outcome;
    assert.ok(error instanceof GateTimeoutError, `ticket timeout: ${error}`);
    assert.ok(aborted, "the ticket provider's signal is aborted");
    assert.equal(client.connection.state, ConnectionState.Disconnected);
    client.dispose();
  }
  {
    const { clock, network, client } = setup({ connectTimeoutMs: 500 });
    network.connects = ["hang"];
    const outcome = client.connection.connect("t").then(() => "connected", (e: Error) => e);
    await clock.advance(500);
    const error = await outcome;
    assert.ok(error instanceof GateTimeoutError, `transport timeout: ${error}`);
    assert.ok(network.last.closedByClient, "the transport is closed on timeout");
    client.dispose();
  }
  {
    const { clock, network, client } = setup({ connectTimeoutMs: 500 });
    network.ackAuth = false; // 连上了，握手不回
    const outcome = client.connection.connect("t").then(() => "connected", (e: Error) => e);
    await clock.advance(500);
    const error = await outcome;
    assert.ok(error instanceof GateTimeoutError, `handshake timeout: ${error}`);
    assert.ok(network.last.closedByClient);
    client.dispose();
  }
});

test("the request timeout fails the request and keeps the connection", async () => {
  const { clock, network, client } = setup({ requestTimeoutMs: 1000, heartbeatIntervalMs: 0 });
  await client.connection.connect("t");
  let failed: Error | undefined;
  client.request(5, 6, utf8("x"), text, () => assert.fail("no reply"), (e) => (failed = e));
  await clock.advance(999);
  assert.equal(failed, undefined);
  await clock.advance(1);
  assert.ok((failed as Error | undefined) instanceof GateTimeoutError, `failed ${failed}`);
  network.last.deliver(reply(network.last.sent[1], 6, utf8("late")));
  await settle();
  assert.equal(client.connection.state, ConnectionState.Ready);
  client.dispose();
});

test("reconnect backs off with jitter, fetches a fresh ticket each attempt and reports every result", async () => {
  const { clock, network, client } = setup({
    autoReconnect: true,
    heartbeatIntervalMs: 0,
    reconnectInitialDelayMs: 100,
    reconnectMaxDelayMs: 350,
    maxReconnectAttempts: 4,
  });
  const results: ReconnectResult[] = [];
  client.on("reconnected", (r) => results.push(r));
  let tickets = 0;
  await client.connection.connect(async () => `t${++tickets}`);
  network.connects = ["fail", "fail", "fail", "fail"];
  network.last.drop(new Error("peer went away"));
  await settle();
  assert.equal(client.connection.state, ConnectionState.Reconnecting);
  // 退避 = max(1, min(max, initial × 2^attempt) × (0.5 + random × 0.5))，random 0.5 → × 0.75。
  const expected = [75, 150, 262.5, 262.5];
  for (let i = 0; i < expected.length; i++) {
    await clock.advance(expected[i] - 0.5);
    assert.equal(results.length, i, `attempt ${i + 1} not before ${expected[i]}ms`);
    await clock.advance(0.5);
    assert.equal(results.length, i + 1, `attempt ${i + 1} at ${expected[i]}ms`);
  }
  assert.equal(tickets, 5, "one ticket per handshake attempt");
  assert.deepEqual(
    results.map((r) => [r.attempt, r.willRetry, r.error !== undefined]),
    [
      [1, true, true],
      [2, true, true],
      [3, true, true],
      [4, false, true],
    ],
  );
  assert.equal(client.connection.state, ConnectionState.Disconnected, "gave up after maxReconnectAttempts");
  assert.ok(client.connection.lastError, "lastError is the last failure");
  assert.deepEqual(clock.pending(), []);
  client.dispose();
});

test("a successful reconnect bumps the version; disconnect during backoff stops reconnecting", async () => {
  const { clock, network, client } = setup({ autoReconnect: true, heartbeatIntervalMs: 0, reconnectInitialDelayMs: 100 });
  const results: ReconnectResult[] = [];
  client.on("reconnected", (r) => results.push(r));
  await client.connection.connect("t");
  network.last.drop();
  await clock.advance(75);
  assert.deepEqual(results, [{ attempt: 1, connectionVersion: 2, willRetry: false }]);
  assert.equal(client.connection.state, ConnectionState.Ready);
  network.last.drop();
  await settle();
  assert.equal(client.connection.state, ConnectionState.Reconnecting);
  client.connection.disconnect();
  assert.equal(client.connection.state, ConnectionState.Disconnected);
  await clock.advance(10_000);
  assert.equal(network.transports.length, 2, "no attempt after disconnect");
  assert.equal(results.length, 1);
  // 断开后可以再显式连。
  await client.connection.connect("again");
  assert.equal(client.connection.connectionVersion, 3);
  client.dispose();
});

test("without autoReconnect a drop ends in Disconnected and the in-flight request fails", async () => {
  const { network, client } = setup({ heartbeatIntervalMs: 0 });
  await client.connection.connect("t");
  let failed: Error | undefined;
  client.request(5, 6, utf8("x"), text, () => {}, (e) => (failed = e));
  network.last.drop(new Error("reset"));
  await settle();
  assert.equal(client.connection.state, ConnectionState.Disconnected);
  assert.ok(failed instanceof GateClosedError, `failed ${failed}`);
  assert.equal(client.connection.lastError?.message, "reset");
  client.dispose();
});

test("a non-empty heartbeat acknowledgement is a protocol error that closes the connection", async () => {
  const { clock, network, client } = setup();
  await client.connection.connect("t");
  network.ackHeartbeats = false;
  network.onFrame = (packet, transport) => {
    if (packet.messageId === 0) transport.deliver({ ...reply(packet, 0, utf8("x")) });
  };
  await clock.advance(1000);
  assert.equal(client.connection.state, ConnectionState.Disconnected);
  assert.ok(client.connection.lastError instanceof GateProtocolError, `lastError ${client.connection.lastError}`);
  assert.equal(client.connection.heartbeatRoundTrips, 0);
  client.dispose();
});
