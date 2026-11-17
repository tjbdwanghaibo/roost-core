// WebGL 冒烟用的本地服务（照 client/ts/demo/serve.mjs）：起测试对端（真实 relay + lobby，client/relay/peer_test.go，带 WebSocket 监听），
// 把 WebGL 构建目录当静态目录发出去，/lobby/* 同源代理到对端的 lobby（含测试用的 /lobby/harness/game-token），页面不用跨域。
//   source ~/.roost-it/roost-dataengine-it/env.sh   # 可选：房间目录用 Redis，否则用内存
//   node client/dotnet/unity-webgl-smoke/serve.mjs <WebGL 构建目录> [端口，缺省 8788]
// 然后在浏览器开 http://127.0.0.1:8788/ ，页面上的日志以 “SMOKE” 开头，最后一行 SMOKE PASS / SMOKE FAIL。
// 编辑器批处理（README）也可以连它：ROOST_SMOKE_LOBBY=http://127.0.0.1:8788/lobby。
// ROOST_RELAYPEER_BIN 指向事先编好的对端时直接用它，否则 go test -c ./client/relay。Ctrl-C 结束（对端随 stdin 关闭退出）。
import { spawn, execFileSync } from "node:child_process";
import { createReadStream, existsSync, mkdtempSync, statSync } from "node:fs";
import { createServer, request } from "node:http";
import { tmpdir } from "node:os";
import { dirname, extname, join, normalize, resolve } from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const root = resolve(process.argv[2] ?? ".");
const port = Number(process.argv[3] ?? 8788);

let binary = process.env.ROOST_RELAYPEER_BIN;
if (!binary) {
  binary = join(mkdtempSync(join(tmpdir(), "roost-smoke-")), "relaypeer");
  execFileSync("go", ["test", "-c", "-o", binary, "./client/relay"], { cwd: repo, stdio: "inherit" });
}
const peer = spawn(binary, [], { cwd: repo, stdio: ["pipe", "pipe", "inherit"], env: { ...process.env, ROOST_RELAYPEER_SERVE: "1" } });
const info = JSON.parse(await new Promise((ok) => createInterface({ input: peer.stdout }).once("line", ok)));
const lobby = new URL(info.lobby_url);
console.log(`peer: lobby ${info.lobby_url}, relay ${info.relay_ws} / ${info.relay_tcp}, rooms in ${info.backend}`);

const types = {
  ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".wasm": "application/wasm",
  ".data": "application/octet-stream", ".json": "application/json", ".css": "text/css", ".png": "image/png", ".ico": "image/x-icon",
};
createServer((req, res) => {
  const url = new URL(req.url, "http://x");
  if (url.pathname.startsWith("/lobby/")) {
    const upstream = request(
      { host: lobby.hostname, port: lobby.port, path: url.pathname.slice("/lobby".length) + url.search, method: req.method, headers: { ...req.headers, host: lobby.host } },
      (reply) => {
        res.writeHead(reply.statusCode ?? 502, reply.headers);
        reply.pipe(res);
      },
    );
    upstream.on("error", () => res.writeHead(502).end());
    req.pipe(upstream);
    return;
  }
  let path = normalize(join(root, decodeURIComponent(url.pathname)));
  if (!path.startsWith(root)) return res.writeHead(403).end();
  if (existsSync(path) && statSync(path).isDirectory()) path = join(path, "index.html");
  if (!existsSync(path)) return res.writeHead(404).end("not found");
  res.writeHead(200, { "Content-Type": types[extname(path)] ?? "application/octet-stream", "Cache-Control": "no-store" });
  createReadStream(path).pipe(res);
}).listen(port, "127.0.0.1", () => console.log(`smoke: http://127.0.0.1:${port}/  (lobby http://127.0.0.1:${port}/lobby)`));

process.on("SIGINT", () => {
  peer.stdin.end();
  setTimeout(() => process.exit(0), 3000);
  peer.once("exit", () => process.exit(0));
});
process.on("SIGTERM", () => process.emit("SIGINT"));
