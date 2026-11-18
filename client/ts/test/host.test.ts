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
  UnknownCompressionError,
  decodeEpoch,
  decodeSnapshot,
  encodeEpoch,
  encodeFrame,
  encodeHostChanged,
  encodeMember,
  encodePresence,
  encodeRejected,
  encodeYouAreHost,
  defaultCompressor,
  identityCompressor,
  packState,
  type Compressor,
  type MemberPresence,
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

async function connect(options: Partial<RelayClientOptions> & { slot?: number; host?: boolean; hostSlot?: number; epoch?: bigint } = {}) {
  const slot = options.slot ?? 2;
  const host = options.host ?? false;
  const fake = new FakeRelay({ slot, host, hostSlot: options.hostSlot ?? (host ? slot : 1), epoch: options.epoch });
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

  fake.push(youAreHost(3, 2n, StateSource.Snapshot, 7n, await packState(identityCompressor, text("state-7"))));
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
  fake.push(youAreHost(2, 1n, StateSource.Handoff, 0n, await packState(identityCompressor, new Uint8Array(32 * 1024).fill(7))));
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
  assert.deepEqual(decodeSnapshot(fake.controls(RelayOp.Snapshot)[0].payload).data, await packState(identityCompressor, text("s1")));
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

// 一次推送绑定发起时的连接与房主任期（epoch）：取状态（snapshot.take）或压缩还没结束时房主换了一圈（A→B→A，epoch 0→2），
// 这份是旧任期的状态，不能拿新任期的 seq 发出去（relay 会把它当成新任期最新的一份，下一任房主恢复成旧状态）。
// 过期的一份返回 undefined：不发、不占 seq、不触发 onSnapshotSent。任期不变时照常发出（对照）。
for (const where of ["take", "compress"] as const) {
  for (const changeTerm of [false, true]) {
    test(`a snapshot whose ${where} outlives the host term is dropped (term changed: ${changeTerm})`, async () => {
      let release!: () => void;
      const gate = new Promise<void>((resolve) => (release = resolve));
      const slowCompressor: Compressor = {
        name: "slow",
        id: identityCompressor.id,
        compress: async (data) => {
          if (where === "compress") await gate;
          return identityCompressor.compress(data);
        },
        decompress: (data, limit) => identityCompressor.decompress(data, limit),
      };
      const { client, fake } = await connect({
        slot: 1,
        host: true,
        autoHostReady: true,
        compressor: slowCompressor,
        snapshot: {
          intervalMs: 0,
          take: async () => {
            if (where === "take") await gate;
            return text("epoch-zero-state");
          },
        },
      });
      const sent: bigint[] = [];
      client.onSnapshotSent((s) => sent.push(s.seq));
      try {
        const pending = client.pushSnapshot();
        if (changeTerm) {
          fake.push(control(RelayOp.YouAreMember, 1, encodeEpoch(1n)));
          fake.push(youAreHost(1, 2n, StateSource.None, 0n, new Uint8Array(0)));
          await fake.waitFor("HostReady", () => fake.controls(RelayOp.HostReady).length > 0);
          fake.push(control(RelayOp.HostChanged, 1, encodeHostChanged(2n, "a1")));
          assert.equal(client.hostRole, "host");
          assert.equal(client.epoch, 2n);
        }
        release();
        const seq = await pending;
        const frames = fake.controls(RelayOp.Snapshot);
        if (!changeTerm) {
          assert.equal(seq, 1n);
          assert.equal(frames.length, 1);
          assert.deepEqual(sent, [1n]);
          return;
        }
        assert.equal(seq, undefined, "the stale snapshot is not sent");
        assert.equal(frames.length, 0, "no Snapshot frame for the old term");
        assert.deepEqual(sent, [], "onSnapshotSent is not called for the stale snapshot");
        assert.equal(client.snapshotSeq, 2n << 32n, "the stale snapshot did not use up a seq");
        assert.equal(await client.pushSnapshot(text("epoch-two-state")), (2n << 32n) + 1n, "the new term's first snapshot");
        assert.equal(decodeSnapshot(fake.controls(RelayOp.Snapshot)[0].payload).seq, (2n << 32n) + 1n);
      } finally {
        client.close();
      }
    });
  }
}

// 连接也绑定：等取状态时断线重连（同一任期回来仍是房主），这份不在新连接上发出、不占 seq；新连接上的下一份照常接着发。
test("a snapshot taken on one connection is not sent on the next one", async (t) => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  const lobby = new FakeLobby();
  const sockets: FakeRelay[] = [];
  const client = new RelayClient(lobby.client, {
    game: "party",
    compressor: identityCompressor,
    reconnect: { maxAttempts: 3, minBackoffMs: 5, maxBackoffMs: 10 },
    snapshot: {
      intervalMs: 0,
      take: async () => {
        await gate;
        return text("old-connection-state");
      },
    },
    webSocket: () => {
      const fake = new FakeRelay({ slot: 1, host: true, hostSlot: 1 });
      sockets.push(fake);
      return fake;
    },
  });
  t.after(() => client.close());
  await client.connect(TICKET);
  const pending = client.pushSnapshot();
  const back = new Promise((resolve) => client.onReconnected(resolve));
  sockets[0].close();
  await back;
  assert.ok(client.isHost, "back as host in the same term");
  release();
  assert.equal(await pending, undefined, "the snapshot from the old connection is dropped");
  assert.equal(sockets[0].controls(RelayOp.Snapshot).length + sockets[1].controls(RelayOp.Snapshot).length, 0);
  assert.equal(await client.pushSnapshot(text("new-connection-state")), 1n, "the dropped one did not use up a seq");
  client.close();
  await client.done;
});

// 同一任期内并发推送以发起顺序为准（维护者 2026-11-18 定 A，三份 SDK 一致）：先发起的一份卡在取状态或压缩里，后发起的
// newer-state 先发出（seq 1）。先发起的那份这时才准备好：它比已发出的一份旧，返回 undefined，不发、不占 seq、不触发
// onSnapshotSent。修前它拿到 seq 2 发出，relay 按 seq 递增存下，盖掉较新的 newer-state。
const stateOf = (payload: Uint8Array) => new TextDecoder().decode(decodeSnapshot(payload).data.subarray(1));
for (const where of ["take", "compress"] as const) {
  test(`an earlier-started snapshot whose ${where} finishes after a later one is sent is dropped`, async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => (release = resolve));
    const slowCompressor: Compressor = {
      name: "slow",
      id: identityCompressor.id,
      compress: async (data) => {
        if (where === "compress" && new TextDecoder().decode(data) === "older-state") await gate;
        return identityCompressor.compress(data);
      },
      decompress: (data, limit) => identityCompressor.decompress(data, limit),
    };
    const { client, fake } = await connect({
      slot: 1,
      host: true,
      compressor: slowCompressor,
      snapshotMinIntervalMs: 0,
      snapshot: {
        intervalMs: 0,
        take: async () => {
          if (where === "take") await gate;
          return text("older-state");
        },
      },
    });
    const sent: bigint[] = [];
    client.onSnapshotSent((s) => sent.push(s.seq));
    try {
      const older = client.pushSnapshot();
      assert.equal(await client.pushSnapshot(text("newer-state")), 1n, "the later-started snapshot goes out first");
      release();
      assert.equal(await older, undefined, "the earlier-started snapshot is dropped");
      assert.deepEqual(fake.controls(RelayOp.Snapshot).map((f) => stateOf(f.payload)), ["newer-state"], "relay only got the newer state");
      assert.deepEqual(sent, [1n], "onSnapshotSent is not called for the dropped snapshot");
      assert.equal(client.snapshotSeq, 1n, "the dropped snapshot did not use up a seq");
    } finally {
      client.close();
    }
  });
}

// 定时推送也一样：take 比 intervalMs 慢时，下一轮的推送会与上一轮重叠。卡住的第一轮 older-state 在后续轮次发出之后才
// 取完，它被丢弃，relay 收到的都是较新的状态，seq 连续递增。
test("timer pushes that overlap because take is slower than intervalMs never send an older state after a newer one", async () => {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => (release = resolve));
  let takes = 0;
  const { client, fake } = await connect({
    slot: 1,
    host: true,
    snapshotMinIntervalMs: 0,
    snapshot: {
      intervalMs: 10,
      take: async () => {
        if (++takes === 1) {
          await gate;
          return text("older-state");
        }
        return text("newer-state");
      },
    },
  });
  try {
    await fake.waitFor("a later timer push", () => fake.controls(RelayOp.Snapshot).length > 0);
    release();
    await sleep(50); // 卡住的第一轮已取完；期间定时推送照常进行
    client.close();
    const frames = fake.controls(RelayOp.Snapshot);
    const states = frames.map((f) => stateOf(f.payload));
    assert.ok(!states.includes("older-state"), `relay got the older state after a newer one: ${states.join(", ")}`);
    assert.deepEqual(frames.map((f) => decodeSnapshot(f.payload).seq), frames.map((_, i) => BigInt(i + 1)), "seqs are consecutive");
  } finally {
    client.close();
  }
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
  assert.deepEqual(back, { slot: 2, host: false, hostSlot: 1, epoch: 0n });
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

// 新加的用例用 t.after 关客户端：断言失败时也不留下心跳计时器（否则测试文件的子进程不退出）。
test("the Welcome's epoch is the room's: client.epoch follows it on connect and on every reconnect", async (t) => {
  const lobby = new FakeLobby();
  const sockets: FakeRelay[] = [];
  let epoch = 3n;
  const client = new RelayClient(lobby.client, {
    game: "party",
    compressor: identityCompressor,
    reconnect: { maxAttempts: 3, minBackoffMs: 5, maxBackoffMs: 10 },
    webSocket: () => {
      const fake = new FakeRelay({ slot: 2, host: false, hostSlot: 4, epoch });
      sockets.push(fake);
      return fake;
    },
  });
  t.after(() => client.close());
  assert.deepEqual(await client.connect(TICKET), { slot: 2, host: false, hostSlot: 4, epoch: 3n });
  assert.equal(client.epoch, 3n, "a member who joins after host changes knows the room's epoch");
  epoch = 7n; // 离线期间又换了 host
  const back = new Promise((resolve) => client.onReconnected(resolve));
  sockets[0].close();
  assert.deepEqual(await back, { slot: 2, host: false, hostSlot: 4, epoch: 7n });
  assert.equal(client.epoch, 7n, "the reconnect's Welcome updates the epoch");
  client.close();
  await client.done;
});

test("setForeground sends Presence (the golden frame), re-sent after HostChanged and after a reconnect; MemberPresence reaches the host", async (t) => {
  const lobby = new FakeLobby();
  const sockets: FakeRelay[] = [];
  const client = new RelayClient(lobby.client, {
    game: "party",
    compressor: identityCompressor,
    reconnect: { maxAttempts: 3, minBackoffMs: 5, maxBackoffMs: 10 },
    webSocket: () => {
      const fake = new FakeRelay({ slot: 2, host: false, hostSlot: 1 });
      sockets.push(fake);
      return fake;
    },
  });
  t.after(() => client.close());
  await client.connect(TICKET);
  const fake = sockets[0];
  fake.push(control(RelayOp.HostChanged, 3, encodeHostChanged(1n, "a3")));
  assert.equal(fake.controls(RelayOp.Presence).length, 0, "never set: nothing to re-send");
  assert.ok(client.setForeground(true));
  assert.equal(hex(encodeFrame(fake.controls(RelayOp.Presence)[0])), golden.get("relay_presence"));
  client.setForeground(false);
  fake.push(control(RelayOp.HostChanged, 4, encodeHostChanged(2n, "a4")));
  assert.deepEqual(fake.controls(RelayOp.Presence).map((f) => f.payload[0]), [1, 0, 0], "re-sent (background) after HostChanged");
  const back = new Promise((resolve) => client.onReconnected(resolve));
  fake.close();
  await back;
  await sockets[1].waitFor("presence after reconnect", () => sockets[1].controls(RelayOp.Presence).length === 1);
  assert.equal(sockets[1].controls(RelayOp.Presence)[0].payload[0], 0);
  client.close();
  await client.done;

  const host = await connect({ slot: 1, host: true });
  t.after(() => host.client.close());
  const seen: MemberPresence[] = [];
  host.client.onMemberPresence((m) => seen.push(m));
  host.fake.push(control(RelayOp.MemberPresence, 3, encodePresence(false)));
  assert.deepEqual(seen, [{ slot: 3, foreground: false }]);
  host.client.close();
});

test("the compression marker: an unknown method is UnknownCompressionError, a corrupt body a decompression error, built-ins always decode", async (t) => {
  const tagged: Compressor = {
    name: "tagged",
    id: 9,
    compress: async (data) => data,
    decompress: async (data) => data,
  };
  const takeovers: HostTakeover[] = [];
  const receiver = await connect({ slot: 3, compressor: defaultCompressor() });
  t.after(() => receiver.client.close());
  receiver.client.onYouAreHost((t) => void takeovers.push(t));
  const offer = async (state: Uint8Array) => {
    receiver.fake.push(youAreHost(3, 1n, StateSource.Handoff, 0n, state));
    await receiver.fake.waitFor("takeover", () => takeovers.length > 0);
    return takeovers.shift()!;
  };
  const state = text("seed=42 deck=[7,1,9]");
  // 混用：发的人用游戏自带的 9 号，收的人用缺省的 deflate
  const mixed = await offer(await packState(tagged, state));
  assert.equal(mixed.state, null);
  assert.ok(mixed.stateError instanceof UnknownCompressionError, String(mixed.stateError));
  const corrupt = await offer(Uint8Array.of(1, 0xff, 0xff, 0xff));
  assert.equal(corrupt.state, null);
  assert.ok(corrupt.stateError && !(corrupt.stateError instanceof UnknownCompressionError), String(corrupt.stateError));
  const fine = await offer(await packState(identityCompressor, state));
  assert.deepEqual([fine.state, fine.stateError], [state, undefined], "none (0) is built in");
  receiver.client.close();

  const custom = await connect({ slot: 3, compressor: tagged });
  t.after(() => custom.client.close());
  custom.client.onYouAreHost((t) => void takeovers.push(t));
  custom.fake.push(youAreHost(3, 1n, StateSource.Handoff, 0n, await packState(defaultCompressor(), state)));
  await custom.fake.waitFor("takeover", () => takeovers.length > 0);
  assert.deepEqual(takeovers[0].state, state, "a client with its own compressor still decodes deflate (1)");
  custom.client.close();
});
