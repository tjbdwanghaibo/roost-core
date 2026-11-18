// 连玩家接入冒烟（GateSmoke）用的本地服务：起玩家接入对端（infra/network/gateway 的 TestServeAWebSocketPlayerAccessForTheBrowserSmoke：
// 缺省配置 + WebSocket 监听，放行 127.0.0.1 / localhost 页面），把 WebGL 构建目录当静态目录发出去；打开 / 时自动补上
// ?ws=<对端地址>&hold=<idle_timeout + 10s>，页面不用手填。
//   node client/dotnet/unity-webgl-smoke/serve-gate.mjs <WebGL 构建目录> [端口，缺省 8790]
// 然后在浏览器开 http://127.0.0.1:8790/ ，页面上的日志以 “SMOKE” 开头，最后一行 SMOKE PASS / SMOKE FAIL。
// 只起对端不发页面（编辑器批处理用）：目录写 -。ROOST_GATEPEER_BIN 指向事先编好的对端时直接用它，否则
// go test -c ./infra/network/gateway。Ctrl-C 结束（对端随 stdin 关闭退出）。
import { spawn, execFileSync } from "node:child_process";
import { createReadStream, existsSync, mkdtempSync, statSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { dirname, extname, join, normalize, resolve } from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const root = process.argv[2] === "-" ? null : resolve(process.argv[2] ?? ".");
const port = Number(process.argv[3] ?? 8790);

let binary = process.env.ROOST_GATEPEER_BIN;
if (!binary) {
  binary = join(mkdtempSync(join(tmpdir(), "roost-gatepeer-")), "gatepeer");
  execFileSync("go", ["test", "-c", "-o", binary, "./infra/network/gateway"], { cwd: repo, stdio: "inherit", env: { ...process.env, GOWORK: "off" } });
}
const peer = spawn(binary, ["-test.run", "^TestServeAWebSocketPlayerAccessForTheBrowserSmoke$", "-test.timeout", "0"], {
  cwd: join(repo, "infra/network/gateway"), stdio: ["pipe", "pipe", "inherit"], env: { ...process.env, ROOST_GATEPEER_SERVE: "1" },
});
const info = await new Promise((ok) => {
  const lines = createInterface({ input: peer.stdout });
  lines.on("line", (line) => {
    if (line.startsWith("GATEPEER ")) ok(JSON.parse(line.slice("GATEPEER ".length)));
  });
});
const hold = Math.ceil(info.idle_timeout_ms / 1000) + 10;
console.log(`peer: player access ${info.ws}, idle_timeout ${info.idle_timeout_ms}ms → hold ${hold}s`);
console.log(`editor: ROOST_SMOKE_WS=${info.ws} ROOST_SMOKE_HOLD=${hold}`);

if (root) {
  const types = {
    ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".wasm": "application/wasm",
    ".data": "application/octet-stream", ".json": "application/json", ".css": "text/css", ".png": "image/png", ".ico": "image/x-icon",
  };
  createServer((req, res) => {
    const url = new URL(req.url, "http://x");
    if (url.pathname === "/" && !url.searchParams.has("ws")) {
      res.writeHead(302, { Location: `/?ws=${encodeURIComponent(info.ws)}&hold=${hold}` });
      return res.end();
    }
    let path = normalize(join(root, decodeURIComponent(url.pathname)));
    if (!path.startsWith(root)) return res.writeHead(403).end();
    if (existsSync(path) && statSync(path).isDirectory()) path = join(path, "index.html");
    if (!existsSync(path)) return res.writeHead(404).end("not found");
    res.writeHead(200, { "Content-Type": types[extname(path)] ?? "application/octet-stream", "Cache-Control": "no-store" });
    createReadStream(path).pipe(res);
  }).listen(port, "127.0.0.1", () => console.log(`smoke: http://127.0.0.1:${port}/`));
}

process.on("SIGINT", () => {
  peer.stdin.end();
  setTimeout(() => process.exit(0), 3000);
  peer.once("exit", () => process.exit(0));
});
process.on("SIGTERM", () => process.emit("SIGINT"));
