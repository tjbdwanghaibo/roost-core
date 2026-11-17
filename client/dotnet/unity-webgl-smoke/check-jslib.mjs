// 在 node 里核对 RoostRelayWebSocket.jslib 的逻辑（不经 Unity、不经浏览器）：用最小的 Emscripten 替身（mergeInto、HEAPU8、
// UTF8ToString）加载 jslib，再用 node 自带的 WebSocket 连真实的测试对端（client/relay/peer_test.go）：经 lobby 要 ws 地址，
// 令牌与 Ping 分两条消息发（一条一帧），每次只收 7 字节（逼出跨消息、跨帧的拼接），解出 Welcome 与 Pong，最后关闭。
// 它只证明 jslib 里的 JS 对；Unity 的 DllImport 封送、WebGL 构建与浏览器里的行为仍要按 README 在 WebGL 构建里看。
//   node client/dotnet/unity-webgl-smoke/check-jslib.mjs     （ROOST_RELAYPEER_BIN 指向编好的对端时直接用它）
import { execFileSync, spawn } from "node:child_process";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const here = dirname(fileURLToPath(import.meta.url));
const repo = resolve(here, "../../..");

// ---- Emscripten 替身 ----
const library = {};
globalThis.LibraryManager = { library };
globalThis.mergeInto = (target, source) => Object.assign(target, source);
globalThis.autoAddDeps = () => {};
globalThis.HEAPU8 = new Uint8Array(1 << 20);
globalThis.UTF8ToString = (ptr) => {
  let end = ptr;
  while (HEAPU8[end] !== 0) end++;
  return new TextDecoder().decode(HEAPU8.subarray(ptr, end));
};
vm.runInThisContext(readFileSync(join(here, "Assets/Plugins/WebGL/RoostRelayWebSocket.jslib"), "utf8"));
globalThis.RoostWs = library.$RoostWs; // Emscripten 把 $RoostWs 变成全局的 RoostWs
const lib = library;

function fail(message) {
  console.error("check-jslib: FAIL " + message);
  process.exitCode = 1;
  throw new Error(message);
}
const sleep = (ms) => new Promise((ok) => setTimeout(ok, ms));
async function until(what, condition, ms = 5000) {
  const deadline = Date.now() + ms;
  while (!condition()) {
    if (Date.now() > deadline) fail("timed out: " + what);
    await sleep(5);
  }
}

// RS v2 帧：'R' 'S' 2 flags | msg u32 | seq u32 | len u32 | payload（大端）。控制帧 flags = 3 << 3。
function frame(flags, op, seq, payload) {
  const data = new Uint8Array(16 + payload.length);
  const view = new DataView(data.buffer);
  data.set([0x52, 0x53, 2, flags]);
  view.setUint32(4, op);
  view.setUint32(8, seq);
  view.setUint32(12, payload.length);
  data.set(payload, 16);
  return data;
}

// ---- 对端 ----
let binary = process.env.ROOST_RELAYPEER_BIN;
if (!binary) {
  binary = join(mkdtempSync(join(tmpdir(), "roost-jslib-")), "relaypeer");
  execFileSync("go", ["test", "-c", "-o", binary, "./client/relay"], { cwd: repo, stdio: "inherit" });
}
const peer = spawn(binary, [], { cwd: repo, stdio: ["pipe", "pipe", "inherit"], env: { ...process.env, ROOST_RELAYPEER_SERVE: "1" } });
const info = JSON.parse(await new Promise((ok) => createInterface({ input: peer.stdout }).once("line", ok)));
try {
  const post = async (path, body, token) => {
    const reply = await fetch(info.lobby_url + path, {
      method: "POST", headers: { "Content-Type": "application/json", ...(token ? { Authorization: "Bearer " + token } : {}) }, body: JSON.stringify(body),
    });
    if (!reply.ok) fail(`${path}: HTTP ${reply.status} ${await reply.text()}`);
    return reply.json();
  };
  const gt1 = (await post("/harness/game-token", { account: "jslib-check" })).token;
  const ticket = await post("/rooms", { game: info.game, mode: "jslib", transport: "ws" }, gt1);
  if (ticket.relay_addr !== info.relay_ws) fail("lobby did not give the ws url: " + ticket.relay_addr);

  // 打开：URL 以 0 结尾写进 HEAPU8。
  const url = new TextEncoder().encode(ticket.relay_addr + "\0");
  HEAPU8.set(url, 1024);
  const id = lib.RoostWsOpen(1024);
  if (lib.RoostWsState(id) !== 0) fail("state right after open should be connecting");
  await until("open", () => lib.RoostWsState(id) === 1);

  // 令牌与 Ping 各一条消息。
  const token = frame(0, 0, 0, new TextEncoder().encode(ticket.token));
  const ping = frame(3 << 3, 32, 0, new Uint8Array([5, 6, 7]));
  for (const data of [token, ping]) {
    HEAPU8.set(data, 4096);
    if (lib.RoostWsSend(id, 4096, data.length) !== 1) fail("send returned 0 on an open socket");
  }
  if (lib.RoostWsBuffered(id) < 0) fail("bufferedAmount");

  // 每次至多收 7 字节，拼成帧。
  const received = [];
  let pending = new Uint8Array(0);
  const frames = [];
  await until("welcome and pong", () => {
    const got = lib.RoostWsReceive(id, 8192, 7);
    if (got > 0) {
      received.push(got);
      const next = new Uint8Array(pending.length + got);
      next.set(pending);
      next.set(HEAPU8.subarray(8192, 8192 + got), pending.length);
      pending = next;
      while (pending.length >= 16) {
        const size = new DataView(pending.buffer, pending.byteOffset).getUint32(12);
        if (pending.length < 16 + size) break;
        const view = new DataView(pending.buffer, pending.byteOffset);
        frames.push({ flags: pending[3], op: view.getUint32(4), seq: view.getUint32(8), payload: pending.slice(16, 16 + size) });
        pending = pending.slice(16 + size);
      }
    }
    return frames.length >= 2;
  });
  const [welcome, pong] = frames;
  if (welcome.op !== 1 || welcome.flags !== 3 << 3 || welcome.payload.length !== 13 || welcome.payload[0] !== 1) fail("first frame is not a host welcome: " + JSON.stringify(welcome));
  if (pong.op !== 33 || pong.payload.join(",") !== "5,6,7") fail("second frame is not the pong: " + JSON.stringify(pong));
  if (Math.max(...received) > 7) fail("receive copied more than asked");

  lib.RoostWsClose(id);
  if (lib.RoostWsState(id) !== 2) fail("closed socket should report state 2");
  if (lib.RoostWsSend(id, 4096, 16) !== 0) fail("send after close should return 0");
  if (lib.RoostWsReceive(id, 8192, 7) !== 0) fail("receive after close should return 0");

  // 连不上的地址：状态变成 3（出错）或 2（关闭），不会停在 0。
  const bad = new TextEncoder().encode("ws://127.0.0.1:1/relay\0");
  HEAPU8.set(bad, 1024);
  const badId = lib.RoostWsOpen(1024);
  await until("refused connection reported", () => lib.RoostWsState(badId) >= 2);
  lib.RoostWsClose(badId);

  console.log(`check-jslib: ok (welcome slot ${welcome.seq}, pong, ${received.length} receives of at most 7 bytes, close, refused connection)`);
} finally {
  peer.stdin.end();
  await new Promise((ok) => peer.once("exit", ok));
}
