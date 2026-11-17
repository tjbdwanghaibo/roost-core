// Web 端房主辅助（方案 §4.7.2～§4.7.4）：补帧与补帧上限、Worker 承载规则、visibilitychange 让出房主、
// sessionStorage 存档与刷新后恢复。浏览器 API 全部注入假的。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  ALL,
  FixedStepLoop,
  HostSessionStore,
  LocalRunner,
  RelayChannel,
  RelayOp,
  RelayReason,
  WebHost,
  WorkerRunner,
  decodeSnapshot,
  encodeEpoch,
  encodeMember,
  serveRules,
  type DocumentLike,
  type HostOutput,
  type HostRules,
  type MessageEndpoint,
  type RelayFrame,
  type StorageLike,
  type TimeoutTimers,
  type Timers,
} from "../src/index.js";
import { FakeLobby, FakeRelay, control, sleep } from "./fake.js";

const text = (s: string) => new TextEncoder().encode(s);
const str = (b: Uint8Array) => new TextDecoder().decode(b);

/** 规则替身：状态是一串文本；每一步把收到的操作广播出去。 */
class EchoRules implements HostRules {
  state = "";
  restored: (string | null)[] = [];
  steps = 0;
  pending: HostOutput[] = [];
  restore(state: Uint8Array | null): void {
    this.state = state ? str(state) : "fresh";
    this.restored.push(state ? str(state) : null);
  }
  input(slot: number, msgId: number, payload: Uint8Array): void {
    this.state += `|${slot}:${str(payload)}`;
    this.pending.push({ target: ALL, msgId, payload });
  }
  step(): HostOutput[] {
    this.steps++;
    return this.pending.splice(0);
  }
  snapshot(): Uint8Array {
    return text(this.state);
  }
}

/** 手动推进的计时器：tick(ms) 让到期的回调按时间顺序执行。 */
class ManualTimers implements Timers, TimeoutTimers {
  now = 0;
  private next = 1;
  private readonly entries = new Map<number, { at: number; every?: number; fn: () => void }>();
  setInterval(fn: () => void, ms: number): unknown {
    const id = this.next++;
    this.entries.set(id, { at: this.now + ms, every: ms, fn });
    return id;
  }
  setTimeout(fn: () => void, ms: number): unknown {
    const id = this.next++;
    this.entries.set(id, { at: this.now + ms, fn });
    return id;
  }
  clearInterval(handle: unknown): void {
    this.entries.delete(handle as number);
  }
  clearTimeout(handle: unknown): void {
    this.entries.delete(handle as number);
  }
  get pending(): number {
    return this.entries.size;
  }
  tick(ms: number): void {
    const end = this.now + ms;
    for (;;) {
      let first: [number, { at: number; every?: number; fn: () => void }] | undefined;
      for (const entry of this.entries) if (entry[1].at <= end && (!first || entry[1].at < first[1].at)) first = entry;
      if (!first) break;
      const [id, e] = first;
      this.now = e.at;
      if (e.every) e.at += e.every;
      else this.entries.delete(id);
      e.fn();
    }
    this.now = end;
  }
}

class FakeDocument implements DocumentLike {
  visibilityState = "visible";
  private listeners: (() => void)[] = [];
  addEventListener(_type: "visibilitychange", listener: () => void): void {
    this.listeners.push(listener);
  }
  removeEventListener(_type: "visibilitychange", listener: () => void): void {
    this.listeners = this.listeners.filter((l) => l !== listener);
  }
  set(state: "visible" | "hidden"): void {
    this.visibilityState = state;
    for (const listener of this.listeners) listener();
  }
}

class FakeStorage implements StorageLike {
  readonly items = new Map<string, string>();
  getItem(key: string): string | null {
    return this.items.get(key) ?? null;
  }
  setItem(key: string, value: string): void {
    this.items.set(key, value);
  }
  removeItem(key: string): void {
    this.items.delete(key);
  }
}

test("catch-up: steps follow elapsed time and stop at the catch-up cap", () => {
  let clock = 0;
  let steps = 0;
  const loop = new FixedStepLoop(() => steps++, { stepMs: 50, maxCatchUpSteps: 20, now: () => clock });
  assert.deepEqual(loop.advance(), { steps: 0, dropped: 0 });
  clock += 120;
  assert.deepEqual(loop.advance(), { steps: 2, dropped: 0 }, "120ms = 2 steps, 20ms carried");
  clock += 30;
  assert.deepEqual(loop.advance(), { steps: 1, dropped: 0 }, "carried 20 + 30 = one more step");
  clock += 1000; // 后台页的计时器每秒醒一次
  assert.deepEqual(loop.advance(), { steps: 20, dropped: 0 });
  clock += 5000; // 被冻结了 5 秒
  assert.deepEqual(loop.advance(), { steps: 20, dropped: 80 }, "at most 20 steps per wake; the rest of the time is dropped");
  assert.equal(steps, 43);
  clock += 10;
  assert.deepEqual(loop.advance(), { steps: 0, dropped: 0 }, "the dropped time is not chased later");
  assert.equal(new FixedStepLoop(() => {}, { stepMs: 50 }).maxCatchUpSteps, 20, "default cap = one second of steps");
});

test("LocalRunner restores, steps on its timer by elapsed time, and hands outputs to onStep", () => {
  const timers = new ManualTimers();
  const rules = new EchoRules();
  const runner = new LocalRunner(rules, { stepMs: 50, timers, now: () => timers.now });
  const outputs: HostOutput[][] = [];
  runner.onStep = (frames) => outputs.push(frames);
  runner.start(text("saved"));
  assert.deepEqual(rules.restored, ["saved"]);
  runner.input(2, 7, text("hit"));
  timers.tick(50);
  assert.equal(rules.steps, 1);
  assert.deepEqual(outputs.flat().map((o) => [o.target, o.msgId, str(o.payload)]), [[ALL, 7, "hit"]]);
  timers.tick(500);
  assert.equal(rules.steps, 11);
  runner.stop();
  timers.tick(500);
  assert.equal(rules.steps, 11, "no steps after stop");
});

test("WorkerRunner drives rules served in a worker over a real MessageChannel (structured clone)", async () => {
  const channel = new MessageChannel();
  const timers = new ManualTimers();
  const rules = new EchoRules();
  serveRules(rules, { stepMs: 50, timers, now: () => timers.now, scope: channel.port2 as unknown as MessageEndpoint });
  const runner = new WorkerRunner(channel.port1 as unknown as MessageEndpoint);
  const outputs: HostOutput[] = [];
  runner.onStep = (frames) => outputs.push(...frames);
  try {
    runner.start(text("from-snapshot"));
    runner.input(3, 9, text("go"));
    await sleep(20);
    assert.deepEqual(rules.restored, ["from-snapshot"]);
    timers.tick(50);
    await sleep(20);
    assert.deepEqual(outputs.map((o) => [o.target, o.msgId, str(o.payload)]), [[ALL, 9, "go"]]);
    assert.ok(outputs[0].payload instanceof Uint8Array, "payload survives structured clone");
    assert.equal(str(await runner.snapshot()), "from-snapshot|3:go");
    runner.stop();
    await sleep(20);
    timers.tick(500);
    assert.equal(rules.steps, 1);
  } finally {
    channel.port1.close();
    channel.port2.close();
  }
});

test("HostSessionStore keeps identity, seq and snapshot; only the same account resumes; storage failures are ignored", () => {
  const storage = new FakeStorage();
  const store = new HostSessionStore(storage);
  store.save({ room: "r9", game: "party", account: "a1", seq: 12n, state: Uint8Array.of(0, 255, 7), savedAt: 5 });
  assert.deepEqual(store.load("a1"), { room: "r9", game: "party", account: "a1", seq: 12n, state: Uint8Array.of(0, 255, 7), savedAt: 5 });
  assert.equal(store.load("a2"), undefined);
  store.clear();
  assert.equal(store.load("a1"), undefined);
  const broken: StorageLike = {
    getItem: () => {
      throw new Error("denied");
    },
    setItem: () => {
      throw new Error("quota");
    },
    removeItem: () => {},
  };
  const quiet = new HostSessionStore(broken);
  quiet.save({ room: "r", game: "g", account: "a", seq: 0n, state: null, savedAt: 0 });
  assert.equal(quiet.load(), undefined);
});

/** 一个 WebHost 页面：假 relay（每次连接一个）、假 lobby、假 document、手动计时器。 */
function page(options: { storage?: StorageLike; welcome?: { slot: number; host: boolean; hostSlot: number }; account?: string; hiddenHandoffMs?: number } = {}) {
  const sockets: FakeRelay[] = [];
  const doc = new FakeDocument();
  const timers = new ManualTimers();
  const rules = new EchoRules();
  const loopTimers = new ManualTimers();
  const runner = new LocalRunner(rules, { stepMs: 50, timers: loopTimers, now: () => loopTimers.now });
  const lobby = new FakeLobby();
  const host = new WebHost({
    lobby: lobby.client,
    account: options.account ?? "a1",
    runner,
    relay: {
      game: "party",
      snapshotIntervalMs: 0,
      snapshotMinIntervalMs: 0,
      webSocket: () => {
        const fake = new FakeRelay(options.welcome ?? { slot: 1, host: true, hostSlot: 1 });
        sockets.push(fake);
        return fake;
      },
    },
    document: doc,
    storage: options.storage ?? new FakeStorage(),
    timers,
    hiddenHandoffMs: options.hiddenHandoffMs,
  });
  const frames: RelayFrame[] = [];
  host.onFrame((f) => frames.push(f));
  return { host, sockets, doc, timers, rules, runner, loopTimers, lobby, frames };
}

function joinMember(fake: FakeRelay, slot: number): void {
  fake.push(control(RelayOp.MemberJoined, slot, encodeMember({ slot, account: `a${slot}`, reason: RelayReason.None })));
}

test("visibilitychange: a hidden host hands off to the first online successor with handoff data after hiddenHandoffMs", async () => {
  const p = page();
  await p.host.create({ game: "party", mode: "m" });
  const fake = p.sockets[0];
  joinMember(fake, 2);
  joinMember(fake, 3);
  p.host.client.setSuccessors([4, 3, 2]); // 4 不在线
  p.host.input(1, text("mine"));
  const handoffs: (number | undefined)[] = [];
  p.host.onHiddenHandoff((t) => handoffs.push(t));

  p.doc.set("hidden");
  p.timers.tick(4999);
  assert.equal(fake.controls(RelayOp.Transfer).length, 0, "not before hiddenHandoffMs");
  p.timers.tick(1);
  await fake.waitFor("transfer", () => fake.controls(RelayOp.Transfer).length === 1);
  const transfer = fake.controls(RelayOp.Transfer)[0];
  assert.equal(transfer.slot, 3, "first online successor");
  assert.equal(str(await p.host.client["compressor"].decompress(transfer.payload, 1 << 20)), "fresh|1:mine", "handoff carries the rules state");
  await sleep(0);
  assert.deepEqual(handoffs, [3]);

  // relay 把旧房主降为成员；回到前台不自动抢回
  fake.push(control(RelayOp.YouAreMember, 1, encodeEpoch(1n)));
  assert.equal(p.runner.running, false, "rules stop on YouAreMember");
  p.doc.set("visible");
  p.timers.tick(10_000);
  assert.equal(fake.controls(RelayOp.Transfer).length, 1);
  assert.equal(p.host.client.hostRole, "member");
  p.host.close();
});

test("visibilitychange: back in time cancels the handoff; no online successor keeps the host; mobile hands off at once", async () => {
  const p = page();
  await p.host.create({ game: "party", mode: "m" });
  const fake = p.sockets[0];
  joinMember(fake, 2);
  const handoffs: (number | undefined)[] = [];
  p.host.onHiddenHandoff((t) => handoffs.push(t));
  p.doc.set("hidden");
  p.timers.tick(3000);
  p.doc.set("visible"); // 5s 内回来：这一次的计时作废
  p.timers.tick(1000);
  p.doc.set("hidden"); // 又隐藏：从这里重新算 5s，不是从第一次隐藏算
  p.timers.tick(4999);
  await sleep(10);
  assert.equal(fake.controls(RelayOp.Transfer).length, 0, "the first hide's timer was cancelled");
  p.timers.tick(1);
  await fake.waitFor("transfer 5s after the second hide", () => fake.controls(RelayOp.Transfer).length === 1);
  fake.push(control(RelayOp.YouAreMember, 1, encodeEpoch(1n)));
  p.host.close();

  const alone = page();
  await alone.host.create({ game: "party", mode: "m" });
  const alonely: (number | undefined)[] = [];
  alone.host.onHiddenHandoff((t) => alonely.push(t));
  alone.doc.set("hidden");
  alone.timers.tick(5000);
  await sleep(10);
  assert.deepEqual(alonely, [undefined], "nobody online: stay host");
  assert.equal(alone.sockets[0].controls(RelayOp.Transfer).length, 0);
  assert.equal(alone.host.client.hostRole, "host");
  alone.host.close();

  const mobile = page({ hiddenHandoffMs: 0 });
  await mobile.host.create({ game: "party", mode: "m" });
  joinMember(mobile.sockets[0], 2); // 没声明候选：取在线成员里槽位最小的
  mobile.doc.set("hidden");
  mobile.timers.tick(0);
  await mobile.sockets[0].waitFor("mobile transfer", () => mobile.sockets[0].controls(RelayOp.Transfer).length === 1);
  assert.equal(mobile.sockets[0].controls(RelayOp.Transfer)[0].slot, 2);
  mobile.host.close();
});

test("the host's own rules output reaches the page; members' frames go to the rules; outputs go out over relay", async () => {
  const p = page();
  await p.host.create({ game: "party", mode: "m" });
  const fake = p.sockets[0];
  fake.push({ channel: RelayChannel.Game, slot: 2, msgId: 5, flags: 0, payload: text("up") });
  p.loopTimers.tick(50);
  const out = fake.frames.filter((f) => f.channel === RelayChannel.Game);
  assert.deepEqual(out.map((f) => [f.slot, f.msgId, str(f.payload)]), [[ALL, 5, "up"]]);
  assert.deepEqual(p.frames.map((f) => [f.slot, f.msgId, str(f.payload)]), [[1, 5, "up"]], "host renders its own broadcast");
  p.host.close();
});

test("refresh: sessionStorage keeps the snapshot; reconnecting within the grace as host restores it and continues the seq", async () => {
  const storage = new FakeStorage();
  const first = page({ storage });
  await first.host.create({ game: "party", mode: "m" });
  first.host.input(1, text("a"));
  assert.equal(await first.host.client.pushSnapshot(), 1n);
  first.host.input(1, text("b"));
  assert.equal(await first.host.client.pushSnapshot(), 2n);
  first.host.close(); // 页面没了（刷新）：存档留着

  const second = page({ storage }); // 同一个标签页、同一个账号
  const welcome = await second.host.resume();
  assert.deepEqual(welcome, { slot: 1, host: true, hostSlot: 1 }, "relay recognises the host within host_grace");
  assert.equal(second.lobby.joins, 1, "rejoin via the lobby for a fresh token");
  assert.deepEqual(second.rules.restored, ["fresh|1:a|1:b"], "rules restored from the stored snapshot");
  assert.ok(second.runner.running);
  assert.equal(await second.host.client.pushSnapshot(), 3n, "seq continues from the stored one");
  assert.equal(decodeSnapshot(second.sockets[0].controls(RelayOp.Snapshot)[0].payload).seq, 3n);
  second.host.close();

  // 宽限期过了才回来：relay 已换了房主，以成员身份进房，存档作废
  const late = page({ storage, welcome: { slot: 1, host: false, hostSlot: 2 } });
  assert.deepEqual(await late.host.resume(), { slot: 1, host: false, hostSlot: 2 });
  assert.equal(late.runner.running, false);
  assert.equal(new HostSessionStore(storage).load("a1"), undefined);
  late.host.close();

  // 另一个账号的存档不用
  const other = page({ storage, account: "a2" });
  new HostSessionStore(storage).save({ room: "r1", game: "party", account: "a1", seq: 1n, state: null, savedAt: 0 });
  assert.equal(await other.host.resume(), undefined);
  other.host.close();
});

test("a new host started by YouAreHost gets the decompressed snapshot and the SDK answers HostReady", async () => {
  const p = page({ welcome: { slot: 2, host: false, hostSlot: 1 } });
  await p.host.join("r1");
  const state = await p.host.client["compressor"].compress(text("seed=42"));
  const fake = p.sockets[0];
  const { encodeYouAreHost, StateSource } = await import("../src/index.js");
  fake.push(
    control(
      RelayOp.YouAreHost,
      2,
      encodeYouAreHost({ epoch: 1n, source: StateSource.Snapshot, snapshotSeq: 4n, snapshotAgeMs: 10, members: [{ slot: 2, online: true, account: "a2" }], state }),
    ),
  );
  await fake.waitFor("host ready", () => fake.controls(RelayOp.HostReady).length === 1);
  assert.deepEqual(p.rules.restored, ["seed=42"]);
  assert.ok(p.runner.running);
  assert.equal(p.host.client.snapshotSeq, 1n << 32n, "max(4, epoch 1 << 32)");
  p.host.close();
});
