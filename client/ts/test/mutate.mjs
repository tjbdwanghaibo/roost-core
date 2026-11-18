// 变异核对（证据用，不进 npm test）：逐条把 src 里被测的那段逻辑改坏，只跑对应的用例，要求变红；跑完恢复原文件。
//   node test/mutate.mjs            # 全部
//   node test/mutate.mjs M3 M5      # 指定几条
// 输出每条变异的结果与失败用例名；任何一条没变红时以 1 退出。
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

const mutations = [
  {
    id: "M1",
    what: "一帧拆成两条消息发（先发包头再发载荷）",
    file: "src/gate/session.ts",
    from: "      this.transport.send(data);",
    to: "      this.transport.send(data.subarray(0, 16));\n      if (data.length > 16) this.transport.send(data.subarray(16));",
    tests: ["gate-websocket"],
  },
  {
    id: "M2",
    what: "鉴权首包不带代号信封（只发票据原文）",
    file: "src/gate/session.ts",
    from: "      auth = encodeAuth(raw, nextConnectionGeneration());",
    to: "      auth = raw; void encodeAuth; void nextConnectionGeneration;",
    tests: ["gate-websocket"],
  },
  {
    id: "M3",
    what: "心跳不独占槽位（占业务在途名额）",
    file: "src/gate/session.ts",
    from: "        if (this.heartbeatInFlight) throw new Error(\"gate: a heartbeat is already pending\");\n      } else if",
    to: "        if (this.heartbeatInFlight) throw new Error(\"gate: a heartbeat is already pending\");\n        if (this.inFlight >= this.limits.maxPendingRequests) throw new Error(\"gate: pending request capacity exhausted\");\n      } else if",
    tests: ["gate-websocket"],
  },
  {
    id: "M4",
    what: "没有等待者的晚应答当推送派发",
    file: "src/gate/session.ts",
    from: "      if (!item) continue; // 没有等待者的晚应答丢弃，不能当推送派发",
    to: "      if (!item) {\n        this.onPush?.(packet);\n        continue;\n      }",
    tests: ["gate-websocket", "gate-timers"],
  },
  {
    id: "M5",
    what: "应答回调推迟到微任务（不再与推送按线上顺序）",
    file: "src/gate/session.ts",
    from: "    item.complete(error, packet);",
    to: "    queueMicrotask(() => item.complete(error, packet));",
    tests: ["gate-websocket"],
  },
  {
    id: "M6",
    what: "回调里断开之后仍处理同一条消息里剩下的帧（会话层不查 closed）",
    file: "src/gate/session.ts",
    from: "      if (this.closed) return; // 回调里关了会话（例如 dispose）：剩下的帧不再处理",
    to: "      // (变异) 不检查 closed",
    tests: ["gate-websocket"],
  },
  {
    id: "M7",
    what: "Gate 包头不查 bits3..7（把 relay 的通道位当合法）",
    file: "src/gate/packet.ts",
    from: "  if ((flags & ~7) !== 0 || (flags & 6) === 6) {",
    to: "  if ((flags & ~31) !== 0 || (flags & 6) === 6) {",
    tests: ["gate-golden"],
  },
  {
    id: "M8",
    what: "重连沿用第一次的票据（不重新取票）",
    file: "src/gate/connection.ts",
    from: "    const provider: TicketProvider = typeof ticket === \"string\" ? async () => ticket : ticket;",
    to: "    let cached: Promise<string> | undefined;\n    const provider: TicketProvider = typeof ticket === \"string\" ? async () => ticket : (signal) => (cached ??= ticket(signal));",
    tests: ["gate-websocket", "gate-timers"],
  },
  {
    id: "M9",
    what: "重连退避不加抖动、不按 2^n 增长（固定 initial）",
    file: "src/gate/connection.ts",
    from: "      const delay = Math.max(1, cap * (0.5 + this.opts.random() * 0.5));",
    to: "      const delay = Math.max(1, this.opts.reconnectInitialDelayMs + 0 * cap);",
    tests: ["gate-timers"],
  },
  {
    id: "M10",
    what: "心跳超时不断开连接（只当这次心跳失败，继续下一次）",
    file: "src/gate/connection.ts",
    from: "          if (error) return session.close(error);",
    to: "          if (error) return this.scheduleHeartbeat(session);",
    tests: ["gate-timers"],
  },
  {
    id: "M11",
    what: "连接超时不覆盖取票（取票卡住就一直等）",
    file: "src/gate/connection.ts",
    from: "      const ticket = await raceAbort(lifetime.ticket(budget.signal), budget.signal);",
    to: "      const ticket = await lifetime.ticket(budget.signal); void raceAbort;",
    tests: ["gate-timers"],
    timeoutMs: 20_000,
  },
  {
    id: "M12",
    what: "文本消息当二进制处理（不断开）",
    file: "src/gate/transport.ts",
    from: "          this.end(new GateProtocolError(\"a text WebSocket message is not an RS frame\"));\n          return;",
    to: "          chunk = new TextEncoder().encode(String(data)); void GateProtocolError;",
    tests: ["gate-websocket"],
  },
];

function run(tests, timeoutMs) {
  execFileSync("npx", ["tsc", "-p", "tsconfig.test.json"], { cwd: root, stdio: "pipe" });
  const files = tests.map((t) => `dist-test/test/${t}.test.js`);
  try {
    const out = execFileSync("node", ["--test", "--test-concurrency=1", `--test-timeout=${timeoutMs}`, ...files], { cwd: root, stdio: "pipe", encoding: "utf8" });
    return { ok: true, out };
  } catch (error) {
    return { ok: false, out: `${error.stdout ?? ""}${error.stderr ?? ""}` };
  }
}

const wanted = new Set(process.argv.slice(2));
let survivors = 0;
for (const m of mutations) {
  if (wanted.size && !wanted.has(m.id)) continue;
  const edits = [{ file: m.file, from: m.from, to: m.to }, ...(m.also ?? [])];
  const originals = new Map();
  for (const edit of edits) {
    const path = join(root, edit.file);
    const text = originals.get(path) ?? readFileSync(path, "utf8");
    if (!originals.has(path)) originals.set(path, text);
    const current = readFileSync(path, "utf8");
    if (!current.includes(edit.from)) throw new Error(`${m.id}: pattern not found in ${edit.file}`);
    writeFileSync(path, current.replace(edit.from, edit.to));
  }
  let result;
  try {
    result = run(m.tests, m.timeoutMs ?? 10_000);
  } finally {
    for (const [path, text] of originals) writeFileSync(path, text);
  }
  const failed = [...result.out.matchAll(/^✖ (.*?)(?: \([\d.]+m?s\))?$/gm)].map((x) => x[1]).filter((name) => !name.startsWith("failing tests"));
  if (result.ok) survivors++;
  console.log(`${m.id} ${result.ok ? "SURVIVED (still green)" : "RED"}: ${m.what}`);
  for (const name of [...new Set(failed)]) console.log(`    ✖ ${name}`);
}
run(["gate-golden"], 10_000); // 恢复后重新编译一次，dist-test 回到原样
process.exit(survivors ? 1 : 0);
