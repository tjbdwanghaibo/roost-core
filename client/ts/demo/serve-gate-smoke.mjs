// 浏览器冒烟（demo/gate-smoke.html）用的本地服务：起玩家接入对端（infra/network/gateway 的
// TestServeAWebSocketPlayerAccessForTheBrowserSmoke：缺省配置 + WebSocket 监听，放行 127.0.0.1 / localhost 页面），
// 把 client/ts 当静态目录发出去；打开 / 时跳到 /demo/gate-smoke.html?ws=<对端地址>&hold=<idle_timeout + 10s>。
//   cd client/ts && npm run build && node demo/serve-gate-smoke.mjs [端口，缺省 8791]
// 然后在浏览器开 http://127.0.0.1:8791/ ，页面日志以 SMOKE 开头，最后一行 SMOKE PASS / SMOKE FAIL。
// ROOST_GATEPEER_BIN 指向事先编好的对端时直接用它，否则 go test -c ./infra/network/gateway。Ctrl-C 结束（对端随 stdin 关闭退出）。
import { spawn, execFileSync } from "node:child_process";
import { createReadStream, existsSync, mkdtempSync, statSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { dirname, extname, join, normalize, resolve, sep } from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const repo = resolve(root, "../..");
const port = Number(process.argv[2] ?? 8791);

let binary = process.env.ROOST_GATEPEER_BIN;
if (!binary) {
  binary = join(mkdtempSync(join(tmpdir(), "roost-gatepeer-")), "gatepeer");
  execFileSync("go", ["test", "-c", "-o", binary, "./infra/network/gateway"], { cwd: repo, stdio: "inherit", env: { ...process.env, GOWORK: "off" } });
}
const peer = spawn(binary, ["-test.run", "^TestServeAWebSocketPlayerAccessForTheBrowserSmoke$", "-test.timeout", "0"], {
  cwd: join(repo, "infra/network/gateway"), stdio: ["pipe", "pipe", "inherit"], env: { ...process.env, ROOST_GATEPEER_SERVE: "1" },
});
const info = await new Promise((ok) => {
  createInterface({ input: peer.stdout }).on("line", (line) => {
    if (line.startsWith("GATEPEER ")) ok(JSON.parse(line.slice("GATEPEER ".length)));
  });
});
const hold = Math.ceil(info.idle_timeout_ms / 1000) + 10;
console.log(`peer: player access ${info.ws} (pid ${peer.pid}), idle_timeout ${info.idle_timeout_ms}ms -> hold ${hold}s`);

const types = { ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".map": "application/json" };
createServer((req, res) => {
  const url = new URL(req.url, "http://x");
  if (url.pathname === "/") {
    res.writeHead(302, { Location: `/demo/gate-smoke.html?ws=${encodeURIComponent(info.ws)}&hold=${hold}` });
    return res.end();
  }
  const path = normalize(join(root, decodeURIComponent(url.pathname)));
  if (!path.startsWith(root + sep) || !existsSync(path) || statSync(path).isDirectory()) return res.writeHead(404).end("not found");
  res.writeHead(200, { "Content-Type": types[extname(path)] ?? "application/octet-stream", "Cache-Control": "no-store" });
  createReadStream(path).pipe(res);
}).listen(port, "127.0.0.1", () => console.log(`smoke: http://127.0.0.1:${port}/ (pid ${process.pid})`));

process.on("SIGINT", () => {
  peer.stdin.end();
  setTimeout(() => process.exit(0), 3000);
  peer.once("exit", () => process.exit(0));
});
process.on("SIGTERM", () => process.emit("SIGINT"));
