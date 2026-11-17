// 测试对端：Go 写的真实 relay 与 lobby（client/relay/peer_test.go）。先 go test -c 到临时目录，再起二进制；
// 它在 stdout 打一行地址 JSON，stdin 关闭就退出（测试进程怎么结束都不会留下它）。Redis 取环境变量
// ROOST_REDIS_TEST_ADDR / ROOST_DATAENGINE_IT_REDIS_ADDR（source ~/.roost-it/roost-dataengine-it/env.sh）。
import { spawn, execFileSync, type ChildProcessWithoutNullStreams } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve, dirname } from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";

export const GAME = "party";

/** 仓库根目录：本文件编译后在 client/ts/dist-test/test/。 */
export const REPO_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../../../..");

export interface Peer {
  lobbyUrl: string;
  relayTcp: string;
  relayWs: string;
  backend: string;
  gameToken(account: string): Promise<string>;
  close(): Promise<void>;
}

export async function startPeer(): Promise<Peer> {
  const dir = mkdtempSync(join(tmpdir(), "roost-relaypeer-"));
  const binary = join(dir, "relaypeer");
  // 对端是 client/relay 的测试二进制（peer_test.go）：ROOST_RELAYPEER_SERVE=1 时它不跑用例，只当对端。
  execFileSync("go", ["test", "-c", "-o", binary, "./client/relay"], { cwd: REPO_ROOT, stdio: "inherit" });
  const child: ChildProcessWithoutNullStreams = spawn(binary, [], {
    cwd: REPO_ROOT,
    stdio: ["pipe", "pipe", "pipe"],
    env: { ...process.env, ROOST_RELAYPEER_SERVE: "1" },
  });
  child.stderr.on("data", () => {}); // 对端的日志（lobby 的 WARN）不进测试输出
  const line = await new Promise<string>((resolveLine, reject) => {
    const lines = createInterface({ input: child.stdout });
    lines.once("line", (text) => resolveLine(text));
    child.once("exit", (code) => reject(new Error(`relaypeer exited with ${code} before printing its addresses`)));
  });
  const info = JSON.parse(line) as { lobby_url: string; relay_tcp: string; relay_ws: string; backend: string };
  const exited = new Promise<void>((resolveExit) => child.once("exit", () => resolveExit()));
  return {
    lobbyUrl: info.lobby_url,
    relayTcp: info.relay_tcp,
    relayWs: info.relay_ws,
    backend: info.backend,
    async gameToken(account: string): Promise<string> {
      const reply = await fetch(`${info.lobby_url}/harness/game-token`, { method: "POST", body: JSON.stringify({ account }) });
      if (!reply.ok) throw new Error(`game token: HTTP ${reply.status}`);
      return ((await reply.json()) as { token: string }).token;
    },
    async close(): Promise<void> {
      child.stdin.end();
      const timer = setTimeout(() => child.kill("SIGKILL"), 15_000);
      await exited;
      clearTimeout(timer);
      rmSync(dir, { recursive: true, force: true });
    },
  };
}
