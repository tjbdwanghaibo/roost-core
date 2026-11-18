// 在 node 里核对 Unity 包的 RoostWebSocket.jslib（client/unity/Runtime/WebGL；BrowserWebSocket 连玩家接入与开房间 relay 共用）
// 交出的字节能拼回完整、有序的 RS v2 帧，不依赖消息边界。不要 Go 对端：脚本自己起一个最小的 WebSocket 服务端（RFC 6455），
// 用 node 自带的 WebSocket 当浏览器，按 jslib 的做法加载它（Emscripten 替身同 check-jslib.mjs）。
//   服务端发：一帧拆成三条消息、三帧合成一条消息、接近 1 MiB 上限的大帧（一条消息）、再一帧；最后一条文本消息。
//   客户端按两种长度取（每次 7 字节 / 每次 64 KiB，与 BrowserWebSocket 的中转缓冲相同）拼帧，逐帧比对内容与顺序；
//   RoostWsSend 两次，服务端核对恰好两条消息、每条一帧；文本消息之后状态是 3（出错）。
//   node client/dotnet/unity-webgl-smoke/check-jslib-frames.mjs
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { createServer } from "node:net";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import vm from "node:vm";

const here = dirname(fileURLToPath(import.meta.url));

// ---- Emscripten 替身 ----
const library = {};
globalThis.LibraryManager = { library };
globalThis.mergeInto = (target, source) => Object.assign(target, source);
globalThis.autoAddDeps = () => {};
globalThis.HEAPU8 = new Uint8Array(4 << 20);
globalThis.UTF8ToString = (ptr) => {
  let end = ptr;
  while (HEAPU8[end] !== 0) end++;
  return new TextDecoder().decode(HEAPU8.subarray(ptr, end));
};
vm.runInThisContext(readFileSync(join(here, "../../unity/Runtime/WebGL/RoostWebSocket.jslib"), "utf8"));
globalThis.RoostWs = library.$RoostWs;
const lib = library;

function fail(message) {
  console.error("check-jslib-frames: FAIL " + message);
  process.exit(1);
}
const sleep = (ms) => new Promise((ok) => setTimeout(ok, ms));
async function until(what, condition, ms = 10000) {
  const deadline = Date.now() + ms;
  while (!condition()) {
    if (Date.now() > deadline) fail("timed out: " + what);
    await sleep(2);
  }
}

// RS v2 帧：'R' 'S' 2 flags | msg u32 | seq u32 | len u32 | payload（大端）。
function frame(flags, msg, seq, payload) {
  const data = new Uint8Array(16 + payload.length);
  const view = new DataView(data.buffer);
  data.set([0x52, 0x53, 2, flags]);
  view.setUint32(4, msg);
  view.setUint32(8, seq);
  view.setUint32(12, payload.length);
  data.set(payload, 16);
  return data;
}
const concat = (...parts) => {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let at = 0;
  for (const p of parts) { out.set(p, at); at += p.length; }
  return out;
};
const pattern = (n, seed) => Uint8Array.from({ length: n }, (_, i) => (i * 31 + seed) & 0xff);

// ---- 最小 WebSocket 服务端 ----
function wsFrame(opcode, payload) {
  const n = payload.length;
  const head = n < 126 ? Uint8Array.of(0x80 | opcode, n)
    : n < 65536 ? Uint8Array.of(0x80 | opcode, 126, n >> 8, n & 0xff)
    : (() => { const h = new Uint8Array(10); h[0] = 0x80 | opcode; h[1] = 127; new DataView(h.buffer).setBigUint64(2, BigInt(n)); return h; })();
  return concat(head, payload);
}
const expected = [
  frame(0, 43, 1, pattern(300, 1)),           // 拆成三条消息
  frame(0, 43, 2, pattern(10, 2)),            // 以下三帧合成一条消息
  frame(1, 9, 1, new TextEncoder().encode("m1")),
  frame(1, 9, 2, new TextEncoder().encode("m2")),
  frame(0, 43, 3, pattern((1 << 20) - 3, 3)), // 接近缺省 max_payload_bytes（1 MiB）的大帧
  frame(0, 43, 4, pattern(5, 4)),
];
const clientMessages = [];
const server = createServer((socket) => {
  let buffered = Buffer.alloc(0);
  let upgraded = false;
  socket.on("data", (chunk) => {
    buffered = Buffer.concat([buffered, chunk]);
    if (!upgraded) {
      const end = buffered.indexOf("\r\n\r\n");
      if (end < 0) return;
      const key = /sec-websocket-key:\s*(\S+)/i.exec(buffered.subarray(0, end).toString())[1];
      const accept = createHash("sha1").update(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest("base64");
      socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
      buffered = buffered.subarray(end + 4);
      upgraded = true;
      const [a, b, c, d, big, last] = expected;
      for (const part of [a.subarray(0, 5), a.subarray(5, 6), a.subarray(6)]) socket.write(wsFrame(2, part));
      socket.write(wsFrame(2, concat(b, c, d)));
      socket.write(wsFrame(2, big));
      socket.write(wsFrame(2, last));
    }
    // 客户端发来的帧都带掩码：只认单片二进制消息（FIN=1、opcode=2），记下每条消息的载荷。
    while (buffered.length >= 2) {
      const fin = buffered[0] & 0x80, opcode = buffered[0] & 0x0f;
      let n = buffered[1] & 0x7f, at = 2;
      if (n === 126) { if (buffered.length < 4) return; n = buffered.readUInt16BE(2); at = 4; }
      else if (n === 127) { if (buffered.length < 10) return; n = Number(buffered.readBigUInt64BE(2)); at = 10; }
      if (buffered.length < at + 4 + n) return;
      const mask = buffered.subarray(at, at + 4);
      const payload = Uint8Array.from(buffered.subarray(at + 4, at + 4 + n), (v, i) => v ^ mask[i % 4]);
      buffered = buffered.subarray(at + 4 + n);
      if (opcode === 8) { socket.end(); return; }
      clientMessages.push({ fin, opcode, payload });
    }
  });
  socket.on("error", () => {});
  server.sendText = () => socket.write(wsFrame(1, new TextEncoder().encode("not a frame")));
});
await new Promise((ok) => server.listen(0, "127.0.0.1", ok));
const port = server.address().port;

async function readFrames(id, chunk) {
  // 收到的字节按顺序写进一块缓冲（大小是预期的总长），收齐后按包头切帧：每次只取 chunk 个，逼出跨消息、跨帧的拼接。
  const total = expected.reduce((n, f) => n + f.length, 0);
  const stream = new Uint8Array(total);
  let filled = 0, largest = 0;
  await until(`${total} bytes with ${chunk}-byte receives`, () => {
    for (let got; filled < total && (got = lib.RoostWsReceive(id, 8192, chunk)) > 0;) {
      largest = Math.max(largest, got);
      if (filled + got > total) fail(`received more than the ${total} bytes sent`);
      stream.set(HEAPU8.subarray(8192, 8192 + got), filled);
      filled += got;
    }
    return filled === total;
  });
  if (largest > chunk) fail(`receive copied ${largest} bytes, more than the ${chunk} asked`);
  const frames = [];
  for (let at = 0; at < total;) {
    const size = new DataView(stream.buffer, at).getUint32(12);
    frames.push(stream.subarray(at, at + 16 + size));
    at += 16 + size;
  }
  if (frames.length !== expected.length) fail(`cut ${frames.length} frames, want ${expected.length}`);
  frames.forEach((got, i) => {
    const want = expected[i];
    if (got.length !== want.length || !got.every((v, j) => v === want[j])) fail(`frame ${i} (${want.length} bytes) arrived as ${got.length} different bytes`);
  });
  return frames.length;
}

try {
  for (const chunk of [7, 64 << 10]) {
    HEAPU8.set(new TextEncoder().encode(`ws://127.0.0.1:${port}/ws\0`), 1024);
    const id = lib.RoostWsOpen(1024);
    await until("open", () => lib.RoostWsState(id) === 1);
    const count = await readFrames(id, chunk);
    if (chunk === 7) {
      // 发两帧：服务端应收到恰好两条单片二进制消息，各是一整帧。
      const sent = [frame(0, 42, 1, pattern(20, 9)), frame(0, 0, 2, new Uint8Array(0))];
      for (const data of sent) {
        HEAPU8.set(data, 4096);
        if (lib.RoostWsSend(id, 4096, data.length) !== 1) fail("send returned 0 on an open socket");
      }
      await until("two client messages", () => clientMessages.length >= 2);
      clientMessages.forEach((m, i) => {
        if (!m.fin || m.opcode !== 2 || m.payload.length !== sent[i].length || !m.payload.every((v, j) => v === sent[i][j])) fail(`client message ${i} is not exactly frame ${i}`);
      });
      server.sendText();
      await until("a text message marks the socket failed", () => lib.RoostWsState(id) === 3);
    }
    lib.RoostWsClose(id);
    console.log(`check-jslib-frames: ${count} frames whole and in order with ${chunk}-byte receives`);
  }
  console.log("check-jslib-frames: ok (split / merged / near-max frames, one frame per send, text fails the socket)");
} finally {
  server.close();
  process.exit(process.exitCode ?? 0);
}
