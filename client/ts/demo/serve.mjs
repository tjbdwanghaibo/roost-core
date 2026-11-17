// 演示用的本地服务：起对端（真实 relay + lobby，见 client/relay/peer_test.go），把 client/ts 当静态目录发出去，
// /lobby/* 同源代理到对端的 lobby（含测试用的 /lobby/harness/game-token），浏览器不用跨域。
//   cd client/ts && npm run build && source ~/.roost-it/roost-dataengine-it/env.sh && node demo/serve.mjs [端口，缺省 8787]
// 然后在浏览器开 http://127.0.0.1:8787/demo/ （开两三个标签页当房主和成员）。Ctrl-C 结束（对端随 stdin 关闭退出）。
// ROOST_RELAYPEER_BIN 指向事先编好的对端时直接用它，否则 go test -c ./client/relay。
import { spawn, execFileSync } from "node:child_process";
import { createReadStream, existsSync, mkdtempSync, statSync } from "node:fs";
import { createServer, request } from "node:http";
import { tmpdir } from "node:os";
import { dirname, extname, join, normalize, resolve } from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const repo = resolve(root, "../..");
const port = Number(process.argv[2] ?? 8787);

let binary = process.env.ROOST_RELAYPEER_BIN;
if (!binary) {
  binary = join(mkdtempSync(join(tmpdir(), "roost-demo-")), "relaypeer");
  execFileSync("go", ["test", "-c", "-o", binary, "./client/relay"], { cwd: repo, stdio: "inherit" });
}
const peer = spawn(binary, [], { cwd: repo, stdio: ["pipe", "pipe", "inherit"], env: { ...process.env, ROOST_RELAYPEER_SERVE: "1" } });
const info = JSON.parse(await new Promise((ok) => createInterface({ input: peer.stdout }).once("line", ok)));
const lobby = new URL(info.lobby_url);
console.log(`peer: lobby ${info.lobby_url}, relay ${info.relay_ws}, rooms in ${info.backend}`);

const types = { ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8", ".map": "application/json" };
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
  let path = normalize(join(root, url.pathname));
  if (!path.startsWith(root)) return res.writeHead(403).end();
  if (existsSync(path) && statSync(path).isDirectory()) path = join(path, "index.html");
  if (!existsSync(path)) return res.writeHead(404).end("not found (run npm run build first?)");
  res.writeHead(200, { "Content-Type": types[extname(path)] ?? "application/octet-stream", "Cache-Control": "no-store" });
  createReadStream(path).pipe(res);
}).listen(port, "127.0.0.1", () => console.log(`demo: http://127.0.0.1:${port}/demo/`));

process.on("SIGINT", () => {
  peer.stdin.end();
  setTimeout(() => process.exit(0), 3000);
  peer.once("exit", () => process.exit(0));
});
