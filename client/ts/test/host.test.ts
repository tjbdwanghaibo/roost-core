// 换 host 的客户端行为（假 relay）：控制帧解码成强类型回调、角色状态、HostReady、转交 / 候选 / 快照 / 离开的发送、
// 快照 seq 接续、定时快照、房主心跳、收帧上限、踢人的新语义（lobby_rejected 不重连、lobby_unavailable 退避重试）。
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
  LobbyError,
  RelayClient,
  RelayClosedError,
  RelayOp,
  RelayReason,
  RejectCode,
  SnapshotTooSoonError,
  StateSource,
  decodeEpoch,
  decodeSnapshot,
  encodeEpoch,
  encodeFrame,
  encodeHostChanged,
  encodeMember,
  encodeRejected,
  encodeYouAreHost,
  identityCompressor,
  type Disconnect,
  type HostChange,
  type HostTakeover,
  type RelayClientOptions,
  type Rejected,
} from "../src/index.js";
import { FakeLobby, FakeRelay, TICKET, control, sleep } from "./fake.js";
import { REPO_ROOT } from "./peer.js";

const golden = new Map<string, string>(
  (JSON.parse(readFileSync(join(REPO_ROOT, "client/spec/relay-packets.json"), "utf8")) as { name: string; hex: string }[]).map((e) => [e.name, e.hex]),
);
const hex = (data: Uint8Array) => Buffer.from(data).toString("hex");
const text = (s: string) => new TextEncoder().encode(s);

async function connect(options: Partial<RelayClientOptions> & { slot?: number; host?: boolean; hostSlot?: number } = {}) {
  const slot = options.slot ?? 2;
  const host = options.host ?? false;
  const fake = new FakeRelay({ slot, host, hostSlot: options.hostSlot ?? (host ? slot : 1) });
  const sockets: FakeRelay[] = [fake];
  const client = new RelayClient(undefined, { game: "party", compressor: identityCompressor, ...options, webSocket: () => fake });
  await client.connect(TICKET);
  return { client, fake, sockets };
}

function youAreHost(slot: number, epoch: bigint, source: StateSource, seq: bigint, state: Uint8Array) {
  return control(
    RelayOp.YouAreHost,
    slot,
    encodeYouAreHost({
      epoch,
      source,
      snapshotSeq: seq,
      snapshotAgeMs: 1500,
      members: [
        { slot: 1, online: false, account: "a1" },
        { slot, online: true, account: `a${slot}` },
        { slot: 4, online: true, account: "a4" },
      ],
      state,
    }),
  );
}

test("YouAreHost → candidate, HostReady after the callback, HostChanged{self} → host; seq continues from YouAreHost", async () => {
  const { client, fake } = await connect({ slot: 3, autoHostReady: true, snapshotMinIntervalMs: 0 });
  const takeovers: HostTakeover[] = [];
  const changes: HostChange[] = [];
  let release!: () => void;
  client.onYouAreHost(async (t) => {
    takeovers.push(t);
    await new Promise<void>((resolve) => (release = resolve)); // 游戏恢复状态要一会儿
  });
  client.onHostChanged((c) => changes.push(c));
  assert.equal(client.hostRole, "member");

  fake.push(youAreHost(3, 2n, StateSource.Snapshot, 7n, text("state-7")));
  assert.equal(client.hostRole, "candidate");
  assert.equal(client.epoch, 2n);
  await fake.waitFor("callback", () => takeovers.length === 1);
  assert.equal(new TextDecoder().decode(takeovers[0].state!), "state-7");
  assert.deepEqual([takeovers[0].source, takeovers[0].snapshotSeq, takeovers[0].snapshotAgeMs], [StateSource.Snapshot, 7n, 1500]);
  assert.deepEqual([...client.onlineMembers.entries()], [[4, "a4"]], "online members from the seat list, without self");
  await sleep(20);
  assert.equal(fake.controls(RelayOp.HostReady).length, 0, "HostReady waits for the game's callback");
  release();
  await fake.waitFor("host ready", () => fake.controls(RelayOp.HostReady).length === 1);
  assert.equal(hex(encodeFrame(fake.controls(RelayOp.HostReady)[0])), golden.get("relay_host_ready"), "HostReady{epoch 2} is the golden frame");
  assert.throws(() => client.kick(4), "a candidate is not the host yet");

  fake.push(control(RelayOp.HostChanged, 3, encodeHostChanged(2n, "a3")));
  assert.deepEqual(changes, [{ epoch: 2n, slot: 3, account: "a3", self: true }]);
  assert.equal(client.hostRole, "host");
  assert.ok(client.isHost);
  assert.equal(client.welcome.hostSlot, 3);
  const floor = 2n << 32n; // epoch 2 的下限大于 YouAreHost 里的 seq 7
  assert.equal(await client.pushSnapshot(Uint8Array.of(0xab, 0xcd)), floor + 1n, "next seq = max(local, YouAreHost seq, epoch<<32) + 1");
  assert.equal(decodeSnapshot(fake.controls(RelayOp.Snapshot)[0].payload).seq, floor + 1n);
  assert.equal(await client.pushSnapshot(Uint8Array.of(1)), floor + 2n);
  client.close();
  await client.done;
});

test("a YouAreHost seq above epoch<<32 wins: the next snapshot continues from it", async () => {
  const { client, fake } = await connect({ slot: 3, autoHostReady: true });
  fake.push(youAreHost(3, 1n, StateSource.Handoff, 5_000_000_000n, new Uint8Array(0))); // 来源是交接数据也带 seq
  fake.push(control(RelayOp.HostChanged, 3, encodeHostChanged(1n, "a3")));
  assert.equal(await client.pushSnapshot(Uint8Array.of(1)), 5_000_000_001n);
  client.close();
});

test("without autoHostReady the game answers hostReady() itself; only a candidate may", async () => {
  const { client, fake } = await connect({ slot: 3 });
  const takeovers: HostTakeover[] = [];
  client.onYouAreHost((t) => void takeovers.push(t));
  assert.equal(client.hostReady(), false, "a member cannot send HostReady");
  fake.push(youAreHost(3, 5n, StateSource.None, 0n, new Uint8Array(0)));
  await fake.waitFor("callback", () => takeovers.length === 1);
  assert.equal(takeovers[0].state, null, "source None: no state");
  await sleep(30);
  assert.equal(fake.controls(RelayOp.HostReady).length, 0, "no automatic HostReady by default");
  assert.ok(client.hostReady());
  assert.equal(decodeEpoch(fake.controls(RelayOp.HostReady)[0].payload), 5n, "epoch defaults to YouAreHost's");
  client.close();
});

test("a candidate whose callback throws does not send HostReady", async () => {
  const { client, fake } = await connect({ slot: 3, autoHostReady: true });
  client.onYouAreHost(() => {
    throw new Error("cannot take over");
  });
  fake.push(youAreHost(3, 1n, StateSource.None, 0n, new Uint8Array(0)));
  await sleep(30);
  assert.equal(fake.controls(RelayOp.HostReady).length, 0);
  fake.push(control(RelayOp.YouAreMember, 3, encodeEpoch(1n)));
  assert.equal(client.hostRole, "member");
  client.close();
});

test("the host transfers with handoff data, declares successors, and becomes a member on YouAreMember", async () => {
  const { client, fake } = await connect({ slot: 1, host: true });
  const members: bigint[] = [];
  const changes: HostChange[] = [];
  client.onYouAreMember((epoch) => members.push(epoch));
  client.onHostChanged((c) => changes.push(c));
  assert.equal(client.hostRole, "host");
  assert.ok(client.setSuccessors([3, 2]));
  assert.equal(hex(encodeFrame(fake.controls(RelayOp.Successors)[0])), golden.get("relay_successors"));
  assert.deepEqual(client.successors, [3, 2]);
  assert.ok(await client.transfer(3, text("handoff")));
  assert.equal(hex(encodeFrame(fake.controls(RelayOp.Transfer)[0])), golden.get("relay_transfer"), "Transfer to slot 3 carries the handoff bytes");

  fake.push(control(RelayOp.YouAreMember, 1, encodeEpoch(2n)));
  assert.deepEqual([client.hostRole, client.isHost, members], ["member", false, [2n]]);
  await assert.rejects(client.transfer(2), "a member cannot transfer");
  assert.throws(() => client.setSuccessors([2]));
  fake.push(control(RelayOp.HostChanged, 3, encodeHostChanged(2n, "a3")));
  assert.deepEqual(changes, [{ epoch: 2n, slot: 3, account: "a3", self: false }]);
  assert.deepEqual([client.welcome.hostSlot, client.successors], [3, []]);
  client.close();
});

test("members see HostChanging, HostResumed and HostChanged; Rejected reaches onRejected; Leave is the golden frame", async () => {
  const { client, fake } = await connect({ slot: 2 });
  const events: string[] = [];
  const rejected: Rejected[] = [];
  client.onHostChanging((epoch) => events.push(`changing ${epoch}`));
  client.onHostResumed((r) => events.push(`resumed ${r.epoch} ${r.slot}`));
  client.onHostChanged((c) => events.push(`changed ${c.epoch} ${c.slot} ${c.account}`));
  client.onRejected((r) => rejected.push(r));
  fake.push(control(RelayOp.HostChanging, 0, encodeEpoch(0n)));
  assert.ok(client.hostChanging);
  fake.push(control(RelayOp.HostResumed, 1, encodeEpoch(0n)));
  assert.ok(!client.hostChanging);
  fake.push(control(RelayOp.HostChanging, 0, encodeEpoch(1n)));
  fake.push(control(RelayOp.HostChanged, 4, encodeHostChanged(1n, "a4")));
  assert.deepEqual(events, ["changing 0", "resumed 0 1", "changing 1", "changed 1 4 a4"]);
  assert.deepEqual([client.hostChanging, client.welcome.hostSlot, client.epoch], [false, 4, 1n]);
  fake.push(control(RelayOp.Rejected, 0, encodeRejected({ op: RelayOp.Snapshot, code: RejectCode.TooSoon, seq: 8n })));
  assert.deepEqual(rejected, [{ op: RelayOp.Snapshot, code: RejectCode.TooSoon, seq: 8n }]);
  assert.ok(client.sendLeave());
  assert.equal(hex(encodeFrame(fake.controls(RelayOp.Leave)[0])), golden.get("relay_leave"));
  fake.push(control(RelayOp.Closed, 0, Uint8Array.of(RelayReason.Left)));
  await client.done;
  assert.equal(client.connected, false);
});

test("a YouAreHost larger than maxPayloadBytes is still read (read limit covers snapshotMaxBytes)", async () => {
  const { client, fake } = await connect({ slot: 2, maxPayloadBytes: 1024, snapshotMaxBytes: 64 * 1024 });
  const takeovers: HostTakeover[] = [];
  client.onYouAreHost((t) => void takeovers.push(t));
  fake.push(youAreHost(2, 1n, StateSource.Handoff, 0n, new Uint8Array(32 * 1024).fill(7)));
  await fake.waitFor("takeover", () => takeovers.length === 1);
  assert.equal(takeovers[0].state!.length, 32 * 1024);
  assert.ok(client.connected);
  client.close();
});

test("the host sends a Ping whenever it has been quiet for hostPingIntervalMs; members keep the slow ping", async () => {
  const host = await connect({ slot: 1, host: true, hostPingIntervalMs: 40, pingIntervalMs: 10_000 });
  const member = await connect({ slot: 2, hostPingIntervalMs: 40, pingIntervalMs: 10_000 });
  await sleep(400);
  const hostPings = host.fake.controls(RelayOp.Ping).length;
  assert.ok(hostPings >= 5, `host pinged ${hostPings} times in 400ms (want ≥ 5 at 40ms)`);
  assert.equal(member.fake.controls(RelayOp.Ping).length, 0, "a member pings every 10s");
  // 一直在发帧的房主不用另发 Ping
  const before = host.fake.controls(RelayOp.Ping).length;
  const busy = setInterval(() => host.client.sendGame(0, 1, Uint8Array.of(1)), 10);
  await sleep(300);
  clearInterval(busy);
  assert.ok(host.fake.controls(RelayOp.Ping).length - before <= 1, "no pings while game frames keep flowing");
  host.client.close();
  member.client.close();
});

test("the SDK pushes snapshots on the interval while host, with consecutive seqs, and stops as a member", async () => {
  let n = 0;
  const { client, fake } = await connect({
    slot: 1,
    host: true,
    snapshot: { take: () => text(`s${++n}`), intervalMs: 50 },
    snapshotMinIntervalMs: 0,
  });
  const sent: bigint[] = [];
  client.onSnapshotSent((s) => sent.push(s.seq));
  await fake.waitFor("three snapshots", () => fake.controls(RelayOp.Snapshot).length >= 3);
  const seqs = fake.controls(RelayOp.Snapshot).map((f) => decodeSnapshot(f.payload).seq);
  assert.deepEqual(seqs.slice(0, 3), [1n, 2n, 3n]);
  assert.deepEqual(sent.slice(0, 3), [1n, 2n, 3n]);
  assert.equal(new TextDecoder().decode(decodeSnapshot(fake.controls(RelayOp.Snapshot)[0].payload).data), "s1");
  fake.push(control(RelayOp.YouAreMember, 1, encodeEpoch(1n)));
  const count = fake.controls(RelayOp.Snapshot).length;
  await sleep(200);
  assert.equal(fake.controls(RelayOp.Snapshot).length, count, "no snapshots after stepping down");
  client.close();
});

test("snapshots closer than snapshotMinIntervalMs are not sent and do not use up a seq; becoming host resets the clock", async () => {
  const { client, fake } = await connect({ slot: 1, host: true, snapshotMinIntervalMs: 200 });
  assert.equal(await client.pushSnapshot(Uint8Array.of(1)), 1n);
  await assert.rejects(client.pushSnapshot(Uint8Array.of(2)), SnapshotTooSoonError);
  assert.equal(fake.controls(RelayOp.Snapshot).length, 1);
  await sleep(220);
  assert.equal(await client.pushSnapshot(Uint8Array.of(3)), 2n, "the refused one did not use seq 2");
  // 换下来又当上：relay 重新计算间隔，SDK 也是
  fake.push(control(RelayOp.YouAreMember, 1, encodeEpoch(1n)));
  fake.push(youAreHost(1, 2n, StateSource.None, 0n, new Uint8Array(0)));
  fake.push(control(RelayOp.HostChanged, 1, encodeHostChanged(2n, "a1")));
  assert.equal(await client.pushSnapshot(Uint8Array.of(4)), (2n << 32n) + 1n, "no wait after becoming host again");
  client.close();
});

test("kicking an offline seat: the host's second MemberLeft{kicked} reaches the callback and the seat is gone", async () => {
  const { client, fake } = await connect({ slot: 3, autoHostReady: true });
  const left: { slot: number; reason: RelayReason }[] = [];
  client.onMemberLeft((m) => left.push({ slot: m.slot, reason: m.reason }));
  fake.push(youAreHost(3, 1n, StateSource.None, 0n, new Uint8Array(0))); // 座位 1 离线、4 在线
  fake.push(control(RelayOp.HostChanged, 3, encodeHostChanged(1n, "a3")));
  fake.push(control(RelayOp.MemberLeft, 4, encodeMember({ slot: 4, account: "a4", reason: RelayReason.Disconnected })));
  assert.deepEqual([...client.onlineMembers.keys()], []);
  assert.ok(client.kick(4), "the host may kick a seat that is offline");
  assert.equal(fake.controls(RelayOp.Kick)[0].slot, 4);
  fake.push(control(RelayOp.MemberLeft, 4, encodeMember({ slot: 4, account: "a4", reason: RelayReason.Kicked })));
  assert.deepEqual(left, [
    { slot: 4, reason: RelayReason.Disconnected },
    { slot: 4, reason: RelayReason.Kicked },
  ]);
  assert.ok(client.connected);
  assert.deepEqual([...client.onlineMembers.keys()], []);
  client.close();
});

test("snapshots over snapshotMaxBytes are refused locally", async () => {
  const { client } = await connect({ slot: 1, host: true, snapshotMaxBytes: 16 });
  await assert.rejects(client.pushSnapshot(new Uint8Array(17)), /exceeds snapshotMaxBytes/);
  client.close();
});

/** 自动重连：每次 dial 用 behaviours 里的下一个（Closed 原因，或 Welcome）。 */
async function reconnecting(lobby: FakeLobby, behaviours: (number | "welcome")[]) {
  const sockets: FakeRelay[] = [];
  let i = 0;
  const client = new RelayClient(lobby.client, {
    game: "party",
    compressor: identityCompressor,
    reconnect: { maxAttempts: 5, minBackoffMs: 5, maxBackoffMs: 10 },
    webSocket: () => {
      const b = i === 0 ? "welcome" : behaviours[i - 1];
      i++;
      const fake = b === "welcome" ? new FakeRelay({ slot: 2, host: false, hostSlot: 1 }) : new FakeRelay(undefined, b);
      sockets.push(fake);
      return fake;
    },
  });
  const disconnects: Disconnect[] = [];
  client.onDisconnected((d) => disconnects.push(d));
  await client.connect(TICKET);
  sockets[0].close(); // 断网
  return { client, sockets, disconnects };
}

test("kick semantics: reconnect retries lobby_unavailable with backoff and stops at lobby_rejected", async () => {
  const lobby = new FakeLobby();
  const { client, sockets, disconnects } = await reconnecting(lobby, [RelayReason.LobbyUnavailable, RelayReason.LobbyUnavailable, RelayReason.LobbyRejected, "welcome"]);
  await client.done;
  assert.equal(sockets.length, 4, "two lobby_unavailable retries, then lobby_rejected ends it");
  assert.equal(lobby.joins, 3, "a fresh lobby join (token) per attempt");
  const last = disconnects.at(-1)!;
  assert.deepEqual([last.reason, last.reconnecting], [RelayReason.LobbyRejected, false]);
  assert.ok(last.error instanceof RelayClosedError);
  assert.equal(client.connected, false);
});

test("kick semantics: lobby_unavailable then success reconnects", async () => {
  const lobby = new FakeLobby();
  const { client, sockets } = await reconnecting(lobby, [RelayReason.LobbyUnavailable, "welcome"]);
  const back = await new Promise((resolve) => client.onReconnected(resolve));
  assert.deepEqual(back, { slot: 2, host: false, hostSlot: 1 });
  assert.equal(sockets.length, 3);
  client.close();
  await client.done;
});

test("kick semantics: an old token refused with Closed{kicked} (same relay) stops reconnecting", async () => {
  const lobby = new FakeLobby();
  const { client, sockets, disconnects } = await reconnecting(lobby, [RelayReason.Kicked, "welcome"]);
  await client.done;
  assert.equal(sockets.length, 2);
  assert.deepEqual([disconnects.at(-1)!.reason, disconnects.at(-1)!.reconnecting], [RelayReason.Kicked, false]);
});

test("kick semantics: the lobby's 403 kicked on join stops reconnecting", async () => {
  const lobby = new FakeLobby();
  lobby.joinError = { status: 403, code: "kicked" };
  const { client, sockets, disconnects } = await reconnecting(lobby, ["welcome"]);
  await client.done;
  assert.equal(lobby.joins, 1, "no retry after kicked");
  assert.equal(sockets.length, 1);
  const last = disconnects.at(-1)!;
  assert.ok(last.error instanceof LobbyError && last.error.code === "kicked" && last.error.status === 403);
  assert.equal(last.reconnecting, false);
});

test("decoding a malformed host-change frame is a protocol error", async () => {
  const { client, fake } = await connect({ slot: 2 });
  const ended: Disconnect[] = [];
  client.onDisconnected((d) => ended.push(d));
  fake.push(control(RelayOp.HostChanging, 0, Uint8Array.of(1, 2)));
  await client.done;
  assert.equal(ended[0].reason, RelayReason.Protocol);
  assert.equal(decodeEpoch(encodeEpoch(5n)), 5n);
});
