// TS 客户端连真实玩家接入：由 Go 用例驱动（infra/network/gateway 的 TestTheTSClientWorksOverWebSocket 与
// …ThroughAStandaloneGate，环境变量 ROOST_TS_GATE_DRIVER 指向本文件）。先 npm run build（用 dist/），node + ws 包代替浏览器的
// WebSocket。照 C# 的 WebSocketTests.cs（WsThreaded / WsPumpDriven）核对：
//   websocket <ws-url> <requestId> <responseId> <pushId>
//     握手、请求应答、校时、推送、心跳撑过 idle_timeout 且往返 ≥ 5、服务端关会话后自动重连且代号更大、请求超时（slow 1.5s 对 1s）
//     且迟到的应答被丢弃、计时器冻结（像手机浏览器冻结后台页面）时被 idle_timeout 断开而解冻后重连回来、连接超时（对端接 TCP 不升级）；
//   websocket-gate <ws-url> <requestId> <responseId>
//     经独立 Gate：握手、请求（Game 原样回）、心跳撑过 Gate 的 idle_timeout、冻结后被断开、解冻后重连。
// 服务端的请求协议见 Go 用例：gen 回本连接的代号；push:<x> 回 ok 并推一条 pushId；close 回 closing 后关会话；slow 1.5s 后才回；
// 其余原样回。每一步打印 PASS；失败打印原因并以 1 退出。
import { createServer } from "node:net";
import WebSocket from "ws";
import {
  BrowserWebSocketTransport,
  ConnectionState,
  GameClient,
  GateTimeoutError,
  NetworkClock,
} from "../dist/gate/index.js";

const [mode, url, requestArg, responseArg, pushArg] = process.argv.slice(2);
const ids = { request: Number(requestArg), response: Number(responseArg), push: Number(pushArg ?? 0) };
const decoder = new TextDecoder();
const encoder = new TextEncoder();
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function check(ok, message) {
  if (!ok) throw new Error(message);
}

async function until(what, condition, ms = 5000) {
  const deadline = Date.now() + ms;
  while (!condition()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await sleep(5);
  }
}

/** 计时器可冻结：冻结期间到期的回调排着，解冻时依次执行（像浏览器冻结后台页面时计时器不跑）。网络事件照常到达。 */
class FreezableTimers {
  frozen = false;
  deferred = [];
  setTimeout(callback, ms) {
    const handle = { callback, cleared: false, timer: undefined };
    handle.timer = setTimeout(() => {
      if (handle.cleared) return;
      if (this.frozen) this.deferred.push(handle);
      else callback();
    }, ms);
    return handle;
  }
  clearTimeout(handle) {
    if (!handle) return;
    handle.cleared = true;
    clearTimeout(handle.timer);
  }
  now() {
    return performance.now();
  }
  freeze() {
    this.frozen = true;
  }
  thaw() {
    this.frozen = false;
    const due = this.deferred;
    this.deferred = [];
    for (const handle of due) if (!handle.cleared) handle.callback();
  }
}

/** Go 用例的 idle_timeout 是 1s：心跳 250ms，重连退避短，请求超时 1s。 */
function newClient(timers, extra = {}) {
  return new GameClient({
    webSocketUrl: url,
    transportFactory: () => new BrowserWebSocketTransport({ webSocket: (u) => new WebSocket(u) }),
    autoReconnect: true,
    heartbeatIntervalMs: 250,
    heartbeatTimeoutMs: 2000,
    reconnectInitialDelayMs: 50,
    reconnectMaxDelayMs: 200,
    requestTimeoutMs: 1000,
    timers,
    ...extra,
  });
}

function call(client, body) {
  return new Promise((resolve, reject) => {
    client.request(ids.request, ids.response, encoder.encode(body), (b) => decoder.decode(b), resolve, (error) =>
      reject(new Error(`request '${body}' failed: ${error.message}`, { cause: error })),
    );
  });
}

function nextEvent(list, what, ms = 5000) {
  return until(what, () => list.length > 0, ms).then(() => list.shift());
}

/** 停 seconds 秒，期间连接保持 Ready、版本不变、没有重连，心跳往返 ≥ 5。 */
async function hold(client, reconnects, seconds) {
  const version = client.connection.connectionVersion;
  const before = client.connection.heartbeatRoundTrips;
  await sleep(seconds * 1000);
  check(client.connection.state === ConnectionState.Ready && client.connection.connectionVersion === version && reconnects.length === 0,
    `heartbeats kept the connection past idle_timeout: state ${ConnectionState[client.connection.state]}, lastError ${client.connection.lastError}`);
  const trips = client.connection.heartbeatRoundTrips - before;
  check(trips >= 5, `heartbeat round trips while holding: ${trips}`);
  return trips;
}

/** 冻结计时器：不发心跳，被服务端 idle_timeout 断开；冻结期间不重连；解冻后重连回来。 */
async function freezeAndRecover(client, timers, reconnects) {
  const version = client.connection.connectionVersion;
  timers.freeze();
  await until("the server closed the frozen connection", () => client.connection.state !== ConnectionState.Ready, 5000);
  await sleep(500);
  check(client.connection.state === ConnectionState.Reconnecting && reconnects.length === 0, `no reconnect while frozen: ${ConnectionState[client.connection.state]}, ${reconnects.length} result(s)`);
  timers.thaw();
  const resumed = await nextEvent(reconnects, "reconnect after thawing");
  check(resumed.error === undefined && client.connection.connectionVersion === version + 1, `reconnected after thawing: ${resumed.error}`);
}

async function full() {
  const timers = new FreezableTimers();
  const client = newClient(timers);
  const pushes = [];
  const reconnects = [];
  const errors = [];
  client.subscribe(ids.push, (b) => decoder.decode(b), (value) => pushes.push(value));
  client.on("reconnected", (result) => reconnects.push(result));
  client.on("error", (error) => errors.push(error));

  await client.connection.connect("ws-ts");
  check((await call(client, "hello")) === "hello", "echo");
  const first = BigInt(await call(client, "gen"));
  check(first > 0n, `the server saw a non-zero generation: ${first}`);
  const sample = await new Promise((resolve, reject) => client.probeClock(resolve, reject));
  const clock = new NetworkClock();
  check(clock.observe(sample), `clock sample accepted: ${JSON.stringify(sample)}`);
  check(Math.abs(clock.now(performance.now() / 1000) - Date.now() / 1000) < 1, "the server clock is within a second of this machine's");
  check((await call(client, "push:p1")) === "ok", "push request");
  check((await nextEvent(pushes, "push")) === "p1", "push payload");
  console.log("PASS websocket: handshake, request, clock probe, push");

  const trips = await hold(client, reconnects, 2.5);
  console.log(`PASS websocket: heartbeats held past idle_timeout (${trips} round trips in 2.5s)`);

  check((await call(client, "close")) === "closing", "close request");
  const back = await nextEvent(reconnects, "reconnect after the server closed");
  check(back.error === undefined && client.connection.connectionVersion === 2, `reconnected after the server closed: ${back.error}`);
  const second = BigInt(await call(client, "gen"));
  check(second > first, `the reconnect carries a newer generation: ${second} > ${first}`);
  check((await call(client, "again")) === "again", "echo after reconnect");
  console.log("PASS websocket: server close → auto reconnect with a fresh ticket and a newer generation");

  const replies = [];
  let timedOut;
  client.request(ids.request, ids.response, encoder.encode("slow"), (b) => decoder.decode(b), (v) => replies.push(v), (e) => (timedOut = e));
  await until("request timeout", () => timedOut !== undefined, 4000);
  check(timedOut instanceof GateTimeoutError, `request timeout: ${timedOut}`);
  await sleep(800); // 迟到的应答被丢弃，不交给业务
  check(replies.length === 0 && client.connection.state === ConnectionState.Ready, "late reply dropped, connection kept");
  console.log("PASS websocket: request timeout, late reply dropped, connection kept");

  await freezeAndRecover(client, timers, reconnects);
  const third = BigInt(await call(client, "gen"));
  check(third > second, `the reconnect after thawing carries a newer generation: ${third} > ${second}`);
  check((await call(client, "after")) === "after", "echo after the idle reconnect");
  console.log("PASS websocket: frozen timers → idle close → no reconnect while frozen → reconnect after thawing");

  // 连接超时：对端接受 TCP 却不回 WebSocket 升级。
  const silent = createServer(() => {});
  await new Promise((resolve) => silent.listen(0, "127.0.0.1", resolve));
  try {
    const waiting = new GameClient({
      webSocketUrl: `ws://127.0.0.1:${silent.address().port}/ws`,
      transportFactory: () => new BrowserWebSocketTransport({ webSocket: (u) => new WebSocket(u) }),
      connectTimeoutMs: 300,
    });
    const started = Date.now();
    const error = await waiting.connection.connect("ws-ts").then(() => undefined, (e) => e);
    check(error instanceof GateTimeoutError, `connect timeout: ${error}`);
    check(Date.now() - started < 3000, "connect timeout fired in time");
    check(waiting.connection.state === ConnectionState.Disconnected, "disconnected after the connect timeout");
    waiting.dispose();
  } finally {
    silent.close();
  }
  console.log("PASS websocket: connect timeout against a peer that never upgrades");
  check(errors.length === 0, `no callback errors: ${errors.map((e) => e.message).join("; ")}`);
  client.dispose();
}

async function gate() {
  const timers = new FreezableTimers();
  const client = newClient(timers);
  const reconnects = [];
  client.on("reconnected", (result) => reconnects.push(result));
  await client.connection.connect("ws-ts-gate");
  check((await call(client, "hello")) === "hello", "echo through the Gate");
  const trips = await hold(client, reconnects, 2.5);
  console.log(`PASS websocket standalone Gate: handshake, request, heartbeats held past the Gate's idle_timeout (${trips} round trips)`);
  await freezeAndRecover(client, timers, reconnects);
  check((await call(client, "after")) === "after", "echo after the idle reconnect");
  console.log("PASS websocket standalone Gate: frozen timers → idle close → reconnect after thawing");
  client.dispose();
}

try {
  if (mode === "websocket" && process.argv.length === 7) await full();
  else if (mode === "websocket-gate" && process.argv.length === 6) await gate();
  else {
    console.error("usage: node gate-driver.mjs websocket <ws-url> <requestId> <responseId> <pushId>");
    console.error("       node gate-driver.mjs websocket-gate <ws-url> <requestId> <responseId>");
    process.exit(2);
  }
  process.exit(0);
} catch (error) {
  console.error(`FAIL ${mode}: ${error.stack ?? error}`);
  process.exit(1);
}
