// 真实对端：Go 写的 relay 与 lobby（relaypeer），浏览器那一侧用 ws 包代替全局 WebSocket，经 relay 的 WebSocket 监听接入。
// 覆盖 lobby 错误映射、建房、加入、游戏通道往返、房间通道直达、禁言、踢人、经 lobby 离开、断线重连。
import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import WebSocket from "ws";
import {
  ALL,
  FLAG_PUSH,
  FLAG_SYNC,
  LobbyClient,
  LobbyError,
  RelayChannel,
  RelayClient,
  RelayClosedError,
  RelayOp,
  RelayReason,
  StateSource,
  WebHost,
  LocalRunner,
  type HostOutput,
  type HostRules,
  type StorageLike,
  type Disconnect,
  type HostChange,
  type HostTakeover,
  type Member,
  type MemberPresence,
  type RelayClientOptions,
  type RelayFrame,
  type Welcome,
  type WebSocketLike,
} from "../src/index.js";
import { GAME, startPeer, type Peer } from "./peer.js";

let peer: Peer;
const players: Player[] = [];
const webHosts: WebHost[] = [];

before(async () => {
  peer = await startPeer();
  console.log(`peer: lobby ${peer.lobbyUrl}, relay ws ${peer.relayWs}, room directory ${peer.backend}`);
});

after(async () => {
  for (const player of players) player.client.close();
  for (const webHost of webHosts) webHost.close();
  await peer?.close();
});

/** 按种类排队的事件，next 等下一条。 */
class Events<T> {
  private items: T[] = [];
  private waiters: ((item: T) => void)[] = [];
  push = (item: T): void => {
    const waiter = this.waiters.shift();
    if (waiter) waiter(item);
    else this.items.push(item);
  };
  next(what: string, timeoutMs = 5000): Promise<T> {
    const item = this.items.shift();
    if (item !== undefined) return Promise.resolve(item);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`timed out waiting for ${what}`)), timeoutMs);
      this.waiters.push((value) => {
        clearTimeout(timer);
        resolve(value);
      });
    });
  }
  get empty(): boolean {
    return this.items.length === 0;
  }
  drain(): T[] {
    return this.items.splice(0);
  }
}

class Player {
  readonly game = new Events<RelayFrame>();
  readonly room = new Events<RelayFrame>();
  readonly joined = new Events<Member>();
  readonly left = new Events<Member>();
  readonly muted = new Events<boolean>();
  readonly disconnected = new Events<Disconnect>();
  readonly reconnected = new Events<Welcome>();
  readonly youAreHost = new Events<HostTakeover>();
  readonly youAreMember = new Events<bigint>();
  readonly hostChanging = new Events<bigint>();
  readonly hostChanged = new Events<HostChange>();
  readonly hostResumed = new Events<{ epoch: bigint; slot: number }>();
  readonly presence = new Events<MemberPresence>();
  readonly sockets: WebSocket[] = [];
  joins = 0;
  readonly lobby: LobbyClient;
  readonly client: RelayClient;

  constructor(readonly account: string, token: string, options: Partial<RelayClientOptions> = {}) {
    this.lobby = new LobbyClient(peer.lobbyUrl, token, (input, init) => {
      if (String(input).endsWith("/join")) this.joins++;
      return fetch(input, init);
    });
    this.client = new RelayClient(this.lobby, {
      game: GAME,
      autoHostReady: true,
      ...options,
      webSocket: (url) => {
        const ws = new WebSocket(url);
        this.sockets.push(ws);
        return ws as unknown as WebSocketLike;
      },
    });
    this.client.onGame(this.game.push);
    this.client.onRoom(this.room.push);
    this.client.onMemberJoined(this.joined.push);
    this.client.onMemberLeft(this.left.push);
    this.client.onMuted(this.muted.push);
    this.client.onDisconnected(this.disconnected.push);
    this.client.onReconnected(this.reconnected.push);
    this.client.onYouAreHost(this.youAreHost.push);
    this.client.onYouAreMember(this.youAreMember.push);
    this.client.onHostChanging(this.hostChanging.push);
    this.client.onHostChanged(this.hostChanged.push);
    this.client.onHostResumed(this.hostResumed.push);
    this.client.onMemberPresence(this.presence.push);
    players.push(this);
  }

  static async create(account: string, options: Partial<RelayClientOptions> = {}): Promise<Player> {
    return new Player(account, await peer.gameToken(account), options);
  }

  async join(roomId: string, password?: string): Promise<Welcome> {
    return this.client.connect(await this.lobby.joinRoom(roomId, { game: GAME, password }));
  }
}

const text = (s: string) => new TextEncoder().encode(s);
function assertFrame(f: RelayFrame, channel: RelayChannel, slot: number, msgId: number, payload: string): void {
  assert.deepEqual([f.channel, f.slot, f.msgId, new TextDecoder().decode(f.payload)], [channel, slot, msgId, payload]);
}
async function rejects<T extends Error>(promise: Promise<unknown>, type: new (...args: never[]) => T): Promise<T> {
  try {
    await promise;
  } catch (error) {
    assert.ok(error instanceof type, `expected ${type.name}, got ${String(error)}`);
    return error as T;
  }
  assert.fail(`expected ${type.name}`);
}
async function until(what: string, cond: () => Promise<boolean> | boolean): Promise<void> {
  const deadline = Date.now() + 5000;
  while (!(await cond())) {
    if (Date.now() > deadline) assert.fail(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 20));
  }
}

test("lobby errors map to LobbyError codes", async () => {
  const bad = new LobbyClient(peer.lobbyUrl, "gt2.not-issued");
  const unauthorized = await rejects(bad.listRooms(GAME, "duel"), LobbyError);
  assert.deepEqual([unauthorized.status, unauthorized.code], [401, "unauthorized"]);
  const p = await Player.create("ts-errs");
  assert.equal((await rejects(p.lobby.listRooms("other", "duel"), LobbyError)).code, "game_not_served");
  assert.equal((await rejects(p.lobby.joinRoom("0000000000000000", { game: GAME }), LobbyError)).code, "room_not_found");
  assert.equal((await rejects(p.lobby.createRoom({ game: GAME, mode: "bad mode!" }), LobbyError)).code, "bad_request");
  const full = await p.lobby.createRoom({ game: GAME, mode: "full", maxMembers: 1 });
  const other = await Player.create("ts-errs-2");
  const fullErr = await rejects(other.lobby.joinRoom(full.room.id, { game: GAME }), LobbyError);
  assert.deepEqual([fullErr.code, fullErr.temporary], ["room_full", false]);
  // 令牌被拒：relay 发 Closed{token_rejected}。
  const forged = await rejects(p.client.connect({ room: full.room, relayAddr: peer.relayWs, token: "rt1.forged" }), RelayClosedError);
  assert.equal(forged.reason, RelayReason.TokenRejected);
  // TS SDK 只连 WebSocket：拿 TCP 地址来连，本地就拒绝。
  await rejects(p.client.connect({ room: full.room, relayAddr: peer.relayTcp, token: "x" }), Error);
});

test("room lifecycle over WebSocket against the real relay and lobby", async () => {
  const host = await Player.create("ts-host");
  const ticket = await host.lobby.createRoom({ game: GAME, mode: "duel", name: "ts", password: "pw", maxMembers: 4, attrs: { map: "a" } });
  assert.equal(ticket.room.host, "ts-host");
  assert.equal(ticket.relayAddr, peer.relayWs);
  assert.deepEqual(ticket.room.attrs, { map: "a" });
  const hostWelcome = await host.client.connect(ticket);
  assert.deepEqual(hostWelcome, { slot: 1, host: true, hostSlot: 1, epoch: 0n });
  await until("room listed", async () => {
    const rooms = await host.lobby.listRooms(GAME, "duel");
    return rooms.length === 1 && rooms[0].id === ticket.room.id;
  });

  const m1 = await Player.create("ts-m1", { password: "pw" });
  const m2 = await Player.create("ts-m2", { password: "pw" });
  assert.equal((await rejects(m1.lobby.joinRoom(ticket.room.id, { game: GAME, password: "bad" }), LobbyError)).code, "wrong_password");
  assert.deepEqual(await m1.join(ticket.room.id, "pw"), { slot: 2, host: false, hostSlot: 1, epoch: 0n });
  assert.equal((await m2.join(ticket.room.id, "pw")).slot, 3);
  assert.deepEqual(await host.joined.next("m1 joined"), { slot: 2, account: "ts-m1", reason: RelayReason.None });
  assert.deepEqual(await host.joined.next("m2 joined"), { slot: 3, account: "ts-m2", reason: RelayReason.None });

  // 游戏通道：成员的帧只到房主（目标不看）；房主按目标回给一个成员（游戏位原样），或广播全体。
  assert.ok(m1.client.sendGame(3, 101, text("move")));
  assertFrame(await host.game.next("member game frame"), RelayChannel.Game, 2, 101, "move");
  assert.ok(host.client.sendGame(2, 202, text("state"), FLAG_PUSH | FLAG_SYNC));
  const state = await m1.game.next("host game frame");
  assertFrame(state, RelayChannel.Game, 1, 202, "state");
  assert.equal(state.flags, FLAG_PUSH | FLAG_SYNC);
  assert.ok(host.client.sendGame(ALL, 203, text("tick")));
  assertFrame(await m1.game.next("broadcast at m1"), RelayChannel.Game, 1, 203, "tick");
  assertFrame(await m2.game.next("broadcast at m2"), RelayChannel.Game, 1, 203, "tick");
  assert.ok(m2.game.empty, "m2 got only the broadcast");

  // 房间通道：直达某个槽位（不经房主）；全体时房主也收到。
  assert.ok(m1.client.sendRoom(3, 7, text("psst")));
  assertFrame(await m2.room.next("direct room frame"), RelayChannel.Room, 2, 7, "psst");
  assert.ok(m2.client.sendRoom(ALL, 8, text("hi all")));
  assertFrame(await host.room.next("room broadcast at host"), RelayChannel.Room, 3, 8, "hi all");
  assertFrame(await m1.room.next("room broadcast at m1"), RelayChannel.Room, 3, 8, "hi all");
  assert.ok(host.room.empty, "host did not get the direct room frame");

  // 只有房主能踢人、禁言。
  assert.throws(() => m1.client.kick(3));

  // 禁言：房间帧被丢弃，游戏帧照常（同一连接按序：游戏帧到了说明前面的房间帧没被转出）。
  assert.ok(host.client.mute(3, true));
  assert.equal(await m2.muted.next("muted notice"), true);
  m2.client.sendRoom(ALL, 9, text("muted"));
  m2.client.sendGame(ALL, 104, text("after-mute"));
  assertFrame(await host.game.next("game frame after mute"), RelayChannel.Game, 3, 104, "after-mute");
  assert.ok(host.room.empty, "room frame from a muted member was dropped");

  // 踢人：被踢者收到 Closed{kicked}、不重连；房主收到 MemberLeft{kicked}；被踢的账号再进被 relay 拒绝。
  assert.ok(host.client.kick(3));
  const kicked = await m2.disconnected.next("kicked");
  assert.deepEqual([kicked.reason, kicked.reconnecting], [RelayReason.Kicked, false]);
  assert.ok(kicked.error instanceof RelayClosedError);
  assert.deepEqual(await host.left.next("kicked member left"), { slot: 3, account: "ts-m2", reason: RelayReason.Kicked });
  await m2.client.done;
  // 被踢记在 lobby：rejoin 先向 lobby join，lobby 答 403 kicked（relay 之前的报告已经到了 lobby）
  await until("lobby recorded the kick", async () => {
    const error = await m2.client.rejoin().then(
      () => undefined,
      (e: unknown) => e,
    );
    if (error instanceof RelayClosedError && error.reason === RelayReason.Kicked) return false; // 报告还没到 lobby
    assert.ok(error instanceof LobbyError, `expected LobbyError, got ${String(error)}`);
    assert.deepEqual([error.status, error.code], [403, "kicked"]);
    return true;
  });

  // 经 lobby 离开：relay 断开这个账号，房主收到 MemberLeft{left}。
  await m1.client.leave();
  const leftViaLobby = await host.left.next("member left via lobby");
  assert.deepEqual([leftViaLobby.slot, leftViaLobby.reason], [2, RelayReason.Left]);

  // 只有房主能改状态；开始后的房间新人不能进。
  assert.equal((await rejects(m2.lobby.setRoomState(ticket.room.id, { game: GAME, state: "started" }), LobbyError)).code, "not_host");
  assert.equal((await host.lobby.setRoomState(ticket.room.id, { game: GAME, state: "started" })).state, "started");
  assert.equal((await rejects(m1.lobby.joinRoom(ticket.room.id, { game: GAME, password: "pw" }), LobbyError)).code, "room_started");
});

test("reconnect asks the lobby for a fresh token and resumes", async () => {
  const host = await Player.create("ts-host-r");
  const pongs: RelayFrame[] = [];
  host.client.onControl(RelayOp.Pong, (f) => pongs.push(f)); // 控制帧扩展口：内置处理过的操作码也交给它
  const ticket = await host.lobby.createRoom({ game: GAME, mode: "coop" });
  await host.client.connect(ticket);
  const member = await Player.create("ts-member-r", { pingIntervalMs: 100, reconnect: { maxAttempts: 5, minBackoffMs: 20, maxBackoffMs: 100 } });
  const first = await member.join(ticket.room.id);
  await host.joined.next("member joined");
  const joinsBefore = member.joins;

  member.sockets.at(-1)!.terminate(); // 断网：直接断开 TCP，没有关闭握手，relay 也没发 Closed
  const lost = await member.disconnected.next("disconnect");
  assert.deepEqual([lost.reason, lost.reconnecting], [RelayReason.Disconnected, true]);
  const back = await member.reconnected.next("reconnected");
  assert.deepEqual(back, { slot: first.slot, host: false, hostSlot: 1, epoch: 0n });
  assert.equal(member.joins - joinsBefore, 1, "one lobby join per reconnect (a fresh relay token)");
  await until("host sees the member again", () => host.joined.drain().some((m) => m.account === "ts-member-r"));
  member.client.sendGame(ALL, 1, text("back"));
  assertFrame(await host.game.next("frame after reconnect"), RelayChannel.Game, back.slot, 1, "back");
  host.client.sendGame(back.slot, 2, text("welcome back"));
  assertFrame(await member.game.next("host frame after reconnect"), RelayChannel.Game, 1, 2, "welcome back");
  await until("rtt measured", () => member.client.rtt > 0);

  // 房主断开：宽限期（5s）过后换 host，唯一在线的成员当上新房主（不再关房）。
  host.client.close();
  const hostClosed = await host.disconnected.next("host local close");
  assert.equal(hostClosed.reason, RelayReason.None);
  const takeover = await member.youAreHost.next("you are host", 10_000);
  assert.equal(takeover.source, StateSource.None, "no snapshot was ever pushed");
  const changed = await member.hostChanged.next("host changed");
  assert.deepEqual([changed.self, changed.slot, changed.account], [true, back.slot, "ts-member-r"]);
  assert.equal(member.client.hostRole, "host");
  member.client.close();
});

test("heartbeat pongs reach onControl", async () => {
  const host = await Player.create("ts-host-hb", { pingIntervalMs: 50 });
  const pongs: RelayFrame[] = [];
  host.client.onControl(RelayOp.Pong, (f) => pongs.push(f));
  await host.client.connect(await host.lobby.createRoom({ game: GAME, mode: "hb" }));
  await until("pong via onControl", () => pongs.length > 0);
  assert.equal(pongs[0].payload.length, 8);
  assert.ok(host.client.rtt > 0);
  host.client.close();
});

// ---- 换 host（真实 relay，缺省配置：host_idle_timeout 2s、host_grace 5s、snapshot_min_interval 1s）----

async function room(mode: string, ...members: string[]): Promise<{ host: Player; roomId: string; others: Player[] }> {
  const host = await Player.create(`${mode}-host`);
  const ticket = await host.lobby.createRoom({ game: GAME, mode });
  await host.client.connect(ticket);
  const others: Player[] = [];
  for (const account of members) {
    const p = await Player.create(account);
    await p.join(ticket.room.id);
    await host.joined.next(`${account} joined`);
    others.push(p);
  }
  return { host, roomId: ticket.room.id, others };
}

test("transfer: handoff data reaches the chosen member, everyone sees HostChanged, the lobby follows", async () => {
  const { host, roomId, others } = await room("ts-xfer", "ts-xfer-m1", "ts-xfer-m2");
  const [m1, m2] = others;
  host.client.setSuccessors([3, 2]);
  assert.ok(await host.client.transfer(3, text("handoff-state")));
  assert.equal(await host.youAreMember.next("old host demoted"), 1n);
  const takeover = await m2.youAreHost.next("m2 chosen");
  assert.deepEqual([takeover.epoch, takeover.source, takeover.state && new TextDecoder().decode(takeover.state)], [1n, StateSource.Handoff, "handoff-state"]);
  assert.deepEqual(takeover.members.map((m) => [m.slot, m.account, m.online]), [[1, "ts-xfer-host", true], [2, "ts-xfer-m1", true], [3, "ts-xfer-m2", true]]);
  for (const p of [host, m1, m2]) {
    const c = await p.hostChanged.next(`${p.account} host changed`);
    assert.deepEqual([c.epoch, c.slot, c.account, c.self], [1n, 3, "ts-xfer-m2", p === m2]);
  }
  assert.deepEqual([host.client.hostRole, m2.client.hostRole, m1.client.welcome.hostSlot], ["member", "host", 3]);
  // 转发方向跟着换：m1 的游戏帧到 m2，旧房主的帧也只到 m2
  m1.client.sendGame(ALL, 11, text("to-new-host"));
  assertFrame(await m2.game.next("m1 → new host"), RelayChannel.Game, 2, 11, "to-new-host");
  host.client.sendGame(ALL, 12, text("old-host-now-member"));
  assertFrame(await m2.game.next("old host → new host"), RelayChannel.Game, 1, 12, "old-host-now-member");
  m2.client.sendGame(ALL, 13, text("from-new-host"));
  assertFrame(await m1.game.next("new host → m1"), RelayChannel.Game, 3, 13, "from-new-host");
  assertFrame(await host.game.next("new host → old host"), RelayChannel.Game, 3, 13, "from-new-host");
  await until("lobby sees the new host", async () => (await m2.lobby.listRooms(GAME, "ts-xfer"))[0]?.host === "ts-xfer-m2");
  assert.equal((await m2.lobby.setRoomState(roomId, { game: GAME, state: "started" })).state, "started", "the new host may change the room");
  for (const p of [host, m1, m2]) p.client.close();
});

test("the Welcome carries the room's epoch: a member who joins after a transfer, and one who rejoins, see it", async () => {
  const { host, roomId, others } = await room("ts-epoch", "ts-epoch-m1", "ts-epoch-m2");
  const [m1, m2] = others;
  m1.sockets[0].terminate(); // m1 掉线（不自动重连），座位还在
  await m1.disconnected.next("m1 dropped");
  await host.left.next("m1 left");
  assert.ok(await host.client.transfer(3));
  await m2.youAreHost.next("m2 chosen");
  for (const p of [host, m2]) await p.hostChanged.next(`${p.account} host changed`);
  const late = await Player.create("ts-epoch-late");
  assert.deepEqual(await late.join(roomId), { slot: 4, host: false, hostSlot: 3, epoch: 1n });
  assert.equal(late.client.epoch, 1n);
  const back = await m1.client.rejoin();
  assert.deepEqual([back.epoch, m1.client.epoch, back.hostSlot], [1n, 1n, 3]);
  for (const p of [host, m1, m2, late]) p.client.close();
});

test("presence: members' foreground state reaches only the host, and is re-sent to a new host after HostChanged and after a reconnect", async () => {
  const host = await Player.create("ts-presence-host");
  const ticket = await host.lobby.createRoom({ game: GAME, mode: "ts-presence" });
  await host.client.connect(ticket);
  const m1 = await Player.create("ts-presence-m1");
  await m1.join(ticket.room.id);
  await host.joined.next("m1 joined");
  const m2 = await Player.create("ts-presence-m2", { reconnect: { maxAttempts: 5, minBackoffMs: 20, maxBackoffMs: 100 } });
  await m2.join(ticket.room.id);
  await host.joined.next("m2 joined");
  host.client.setForeground(true);
  m1.client.setForeground(false);
  m2.client.setForeground(true);
  const first = [await host.presence.next("presence 1"), await host.presence.next("presence 2")].sort((a, b) => a.slot - b.slot);
  assert.deepEqual(first, [{ slot: 2, foreground: false }, { slot: 3, foreground: true }]);
  assert.ok(m1.presence.empty && m2.presence.empty, "members do not see each other's presence");

  assert.ok(await host.client.transfer(2));
  await m1.youAreHost.next("m1 chosen");
  const resent = [await m1.presence.next("re-sent 1"), await m1.presence.next("re-sent 2")].sort((a, b) => a.slot - b.slot);
  assert.deepEqual(resent, [{ slot: 1, foreground: true }, { slot: 3, foreground: true }], "the old host and m2 re-send after HostChanged");

  m2.sockets[0].terminate();
  await m2.reconnected.next("m2 back");
  assert.deepEqual(await m1.presence.next("after m2 reconnected"), { slot: 3, foreground: true });
  for (const p of [host, m1, m2]) p.client.close();
});

test("host disconnect: HostChanging while waiting, the snapshot goes to the successor after host_grace, seq continues", async () => {
  const { host, others } = await room("ts-drop", "ts-drop-m1", "ts-drop-m2");
  const [m1, m2] = others;
  host.client.setSuccessors([3]);
  assert.equal(await host.client.pushSnapshot(text("snap-1")), 1n);
  await sleep(1100); // snapshot_min_interval
  assert.equal(await host.client.pushSnapshot(text("snap-2")), 2n);
  await sleep(100);
  host.sockets.at(-1)!.terminate(); // 房主断网
  await sleep(100);
  m1.client.sendGame(ALL, 1, text("lost"));
  assert.equal(await m1.hostChanging.next("host changing during grace"), 0n, "grace: the current epoch");
  const takeover = await m2.youAreHost.next("successor chosen after grace", 10_000);
  assert.deepEqual([takeover.source, takeover.snapshotSeq, takeover.state && new TextDecoder().decode(takeover.state)], [StateSource.Snapshot, 2n, "snap-2"]);
  assert.ok(takeover.snapshotAgeMs >= 4000, `snapshot age ${takeover.snapshotAgeMs}ms covers the grace`);
  const changed = await m1.hostChanged.next("m1 sees the new host");
  assert.deepEqual([changed.slot, changed.account], [3, "ts-drop-m2"]);
  await m2.hostChanged.next("m2 sees itself");
  const rejected: unknown[] = [];
  m2.client.onRejected((r) => rejected.push(r));
  assert.equal(await m2.client.pushSnapshot(text("snap-3")), (1n << 32n) + 1n, "new host continues from max(YouAreHost seq, epoch<<32)");
  m1.client.sendGame(ALL, 2, text("after"));
  assertFrame(await m2.game.next("m1 → new host"), RelayChannel.Game, 2, 2, "after");
  await sleep(200);
  assert.deepEqual(rejected, [], "relay accepted seq 3");
  for (const p of [m1, m2]) p.client.close();
});

test("host heartbeat: an idle host is not replaced within 5s", async () => {
  const { host, others } = await room("ts-idle", "ts-idle-m1");
  const [m1] = others;
  await sleep(5500); // host_idle_timeout 2s：没有心跳房主早就被判断线
  assert.ok(m1.hostChanging.empty && m1.hostChanged.empty && m1.hostResumed.empty, "no host change while the host only heartbeats");
  m1.client.sendGame(ALL, 3, text("still-there"));
  assertFrame(await host.game.next("member → host"), RelayChannel.Game, 2, 3, "still-there");
  assert.ok(host.disconnected.empty);
  for (const p of [host, m1]) p.client.close();
});

class MapStorage implements StorageLike {
  readonly items = new Map<string, string>();
  getItem = (key: string) => this.items.get(key) ?? null;
  setItem = (key: string, value: string) => void this.items.set(key, value);
  removeItem = (key: string) => void this.items.delete(key);
}

class TextRules implements HostRules {
  state = "";
  restored: (string | null)[] = [];
  restore(state: Uint8Array | null): void {
    this.state = state ? new TextDecoder().decode(state) : "new";
    this.restored.push(state ? this.state : null);
  }
  input(slot: number, _msgId: number, payload: Uint8Array): void {
    this.state += `|${slot}:${new TextDecoder().decode(payload)}`;
  }
  step(): HostOutput[] {
    return [];
  }
  snapshot(): Uint8Array {
    return text(this.state);
  }
}

test("refresh within host_grace: the same account is still host, members get HostResumed, state comes back from sessionStorage", async () => {
  const storage = new MapStorage();
  const account = "ts-refresh-host";
  const makePage = async () => {
    const rules = new TextRules();
    const sockets: WebSocket[] = [];
    const webHost = new WebHost({
      lobby: new LobbyClient(peer.lobbyUrl, await peer.gameToken(account)),
      account,
      runner: new LocalRunner(rules, { stepMs: 50 }),
      relay: { game: GAME, snapshotIntervalMs: 0, webSocket: (url) => {
        const ws = new WebSocket(url);
        sockets.push(ws);
        return ws as unknown as WebSocketLike;
      } },
      storage,
      document: undefined,
    });
    return { rules, sockets, webHost };
  };
  const before = await makePage();
  await before.webHost.create({ game: GAME, mode: "ts-refresh" });
  const m1 = await Player.create("ts-refresh-m1");
  await m1.join(before.webHost.room);
  await until("host sees m1", () => before.webHost.client.onlineMembers.has(2));
  m1.client.sendGame(ALL, 1, text("move"));
  await until("rules got the move", () => before.rules.state === "new|2:move");
  assert.equal(await before.webHost.client.pushSnapshot(), 1n);
  before.sockets[0].terminate(); // 刷新：页面连同连接一起没了
  before.webHost.close();

  const after = await makePage(); // 刷新后的同一个标签页
  const welcome = await after.webHost.resume();
  assert.deepEqual(welcome, { slot: 1, host: true, hostSlot: 1, epoch: 0n }, "still the host within host_grace");
  assert.deepEqual(after.rules.restored, ["new|2:move"], "state restored from sessionStorage");
  const resumed = await m1.hostResumed.next("host resumed");
  assert.deepEqual([resumed.epoch, resumed.slot], [0n, 1]);
  await until("relay re-announces online members", () => after.webHost.client.onlineMembers.get(2) === "ts-refresh-m1");
  assert.equal(await after.webHost.client.pushSnapshot(), 2n, "seq continues after the refresh (relay would reject a stale one)");
  m1.client.sendGame(ALL, 2, text("again"));
  await until("rules keep going", () => after.rules.state === "new|2:move|2:again");
  assert.ok(m1.hostChanged.empty, "no host change");
  after.webHost.close();
  m1.client.close();
});

test("kick: the lobby answers 403 kicked and an unused old token is refused; a left member's old token is lobby_rejected", async () => {
  const { host, roomId, others } = await room("ts-kick", "ts-kick-m1", "ts-kick-m2");
  const [m1, m2] = others;
  const spare = await m1.lobby.joinRoom(roomId, { game: GAME }); // 没用过的令牌（join 幂等，重新签）
  assert.ok(host.client.kick(2));
  const kicked = await m1.disconnected.next("kicked");
  assert.deepEqual([kicked.reason, kicked.reconnecting], [RelayReason.Kicked, false]);
  await host.left.next("kicked member left");
  await until("lobby recorded the kick", async () => {
    try {
      await m1.lobby.joinRoom(roomId, { game: GAME });
      return false;
    } catch (error) {
      return error instanceof LobbyError && error.status === 403 && error.code === "kicked" && !error.temporary;
    }
  });
  const refused = await rejects(new RelayClient(undefined, { game: GAME, webSocket: (url) => new WebSocket(url) as unknown as WebSocketLike }).connect(spare), RelayClosedError);
  assert.equal(refused.reason, RelayReason.Kicked, "same relay: its own kick record answers first (lobby_rejected only after a relay restart)");

  // 经 lobby 离开、席位已让出：手里没过期的旧令牌被 lobby 验权拒绝（lobby_rejected），不重连
  const spare2 = await m2.lobby.joinRoom(roomId, { game: GAME });
  await m2.client.leave();
  await host.left.next("m2 left");
  const rejected = await rejects(new RelayClient(undefined, { game: GAME, webSocket: (url) => new WebSocket(url) as unknown as WebSocketLike }).connect(spare2), RelayClosedError);
  assert.equal(rejected.reason, RelayReason.LobbyRejected);
  host.client.close();
});

test("after the start: a dropped member auto-reconnects to its seat; the host kicks an offline seat and the lobby bars it", async () => {
  const host = await Player.create("ts-start-host");
  const ticket = await host.lobby.createRoom({ game: GAME, mode: "ts-start", maxMembers: 3 });
  await host.client.connect(ticket);
  const roomId = ticket.room.id;
  const m1 = await Player.create("ts-start-m1", { reconnect: { maxAttempts: 5, minBackoffMs: 20, maxBackoffMs: 100 } });
  const m2 = await Player.create("ts-start-m2");
  const w1 = await m1.join(roomId);
  const w2 = await m2.join(roomId);
  await host.joined.next("m1");
  await host.joined.next("m2");
  assert.equal((await host.lobby.setRoomState(roomId, { game: GAME, state: "started" })).state, "started");
  const late = await Player.create("ts-start-late");
  assert.equal((await rejects(late.lobby.joinRoom(roomId, { game: GAME }), LobbyError)).code, "room_started");

  // 开局后掉线：席位保留（满员也放行），自动重连回到原座
  m1.sockets.at(-1)!.terminate();
  assert.equal((await m1.disconnected.next("m1 dropped")).reconnecting, true);
  assert.deepEqual(await m1.reconnected.next("m1 back"), { slot: w1.slot, host: false, hostSlot: 1, epoch: 0n });
  m1.client.sendGame(ALL, 1, text("back-after-start"));
  assertFrame(await host.game.next("m1 after reconnect"), RelayChannel.Game, w1.slot, 1, "back-after-start");

  // m2 离线（不经 lobby 离开）：房主踢这个离线座位，收到第二条 MemberLeft{kicked}
  host.left.drain();
  m2.sockets.at(-1)!.terminate();
  const dropped = await host.left.next("m2 offline");
  assert.deepEqual([dropped.slot, dropped.reason], [w2.slot, RelayReason.Disconnected]);
  assert.ok(host.client.kick(w2.slot));
  const kickedSeat = await host.left.next("kicked offline seat");
  assert.deepEqual([kickedSeat.slot, kickedSeat.reason, kickedSeat.account], [w2.slot, RelayReason.Kicked, "ts-start-m2"]);
  await until("lobby bars the kicked seat", async () => {
    try {
      await m2.lobby.joinRoom(roomId, { game: GAME });
      return false;
    } catch (error) {
      return error instanceof LobbyError && error.code === "kicked";
    }
  });
  for (const p of [host, m1]) p.client.close();
});

test("reconnect against lobby admission: down is retried with backoff until it recovers, reject stops", async () => {
  const { host, others } = await room("ts-admit", "ts-admit-m1");
  const m1 = await Player.create("ts-admit-m1b", { reconnect: { maxAttempts: 20, minBackoffMs: 50, maxBackoffMs: 100 } });
  const w = await m1.join(host.client.room);
  await host.joined.next("m1b joined");
  try {
    await peer.admission("down");
    m1.sockets.at(-1)!.terminate();
    assert.equal((await m1.disconnected.next("dropped")).reconnecting, true);
    const sockets = m1.sockets.length;
    await until("a few refused attempts", () => m1.sockets.length >= sockets + 2);
    assert.ok(m1.disconnected.empty, "lobby_unavailable keeps retrying without a final notice");
    await peer.admission("ok");
    assert.equal((await m1.reconnected.next("back once the lobby answers")).slot, w.slot);

    await peer.admission("reject");
    m1.sockets.at(-1)!.terminate();
    assert.equal((await m1.disconnected.next("dropped again")).reconnecting, true);
    const last = await m1.disconnected.next("rejected");
    assert.deepEqual([last.reason, last.reconnecting], [RelayReason.LobbyRejected, false]);
    await m1.client.done;
  } finally {
    await peer.admission("ok");
  }
  for (const p of [host, ...others]) p.client.close();
});

/** 每一步 tick 加 1 并广播 tick（msgId 2）；状态就是 tick。 */
class TickRules implements HostRules {
  tick = 0;
  restored: (number | null)[] = [];
  restore(state: Uint8Array | null): void {
    this.tick = state ? Number(new TextDecoder().decode(state)) : 0;
    this.restored.push(state ? this.tick : null);
  }
  input(): void {}
  step(): HostOutput[] {
    this.tick++;
    return [{ target: ALL, msgId: 2, payload: text(String(this.tick)) }];
  }
  snapshot(): Uint8Array {
    return text(String(this.tick));
  }
}

/** 一个 WebHost 页面，像 demo 一样在 webHost.client 上也注册了业务自己的回调（日志）。 */
async function webPage(account: string, storage: MapStorage) {
  const rules = new TickRules();
  const sockets: WebSocket[] = [];
  const webHost = new WebHost({
    lobby: new LobbyClient(peer.lobbyUrl, await peer.gameToken(account)),
    account,
    runner: new LocalRunner(rules, { stepMs: 50 }),
    relay: {
      game: GAME,
      snapshotIntervalMs: 1200,
      webSocket: (url) => {
        const ws = new WebSocket(url);
        sockets.push(ws);
        return ws as unknown as WebSocketLike;
      },
    },
    storage,
    document: undefined,
  });
  webHosts.push(webHost);
  const ticks: number[] = [];
  webHost.onFrame((f) => ticks.push(Number(new TextDecoder().decode(f.payload))));
  const log: string[] = [];
  const c = webHost.client;
  c.onYouAreHost((t) => void log.push(`you are host ${t.source}`));
  c.onYouAreMember((e) => void log.push(`you are member ${e}`));
  c.onHostChanged((h) => void log.push(`host changed ${h.slot}`));
  c.onSnapshotSent((s) => void log.push(`snapshot ${s.seq}`));
  c.onDisconnected((d) => void log.push(`disconnected ${d.reason}`));
  c.onReconnected(() => void log.push("reconnected"));
  return { rules, sockets, webHost, ticks, log };
}

test("transfer between WebHost pages that also register client callbacks: the new host's rules keep stepping, snapshots go out, a refresh within host_grace resumes", async () => {
  const storageA = new MapStorage();
  const storageB = new MapStorage();
  const a = await webPage("ts-wh-a", storageA);
  const b = await webPage("ts-wh-b", storageB);
  await a.webHost.create({ game: GAME, mode: "ts-wh" });
  const w = await b.webHost.join(a.webHost.room);
  assert.equal(w.slot, 2);
  await until("B sees A's ticks", () => b.ticks.length > 5);

  assert.equal(await a.webHost.handOff(), 2, "A hands off to the only online member");
  await until("B is host", () => b.webHost.client.hostRole === "host");
  assert.ok(b.log.includes(`you are host ${StateSource.Handoff}`), "the app's own onYouAreHost still runs");
  const atTakeover = b.ticks.length;
  await until("B's rules keep stepping after the takeover", () => b.ticks.length > atTakeover + 10);
  assert.ok(b.rules.restored.at(-1)! > 0, "B's rules started from the handoff state");
  await until("A renders B's broadcasts", () => a.ticks.at(-1)! > b.rules.restored.at(-1)!);
  await until("B pushes snapshots on the interval", () => b.log.some((l) => l.startsWith("snapshot")));
  assert.ok(storageB.items.size > 0, "B stored its snapshot in sessionStorage");

  // B 刷新：宽限期内回来仍是房主，规则从 sessionStorage 恢复
  b.sockets.at(-1)!.terminate();
  b.webHost.close();
  const b2 = await webPage("ts-wh-b", storageB);
  const back = await b2.webHost.resume();
  assert.deepEqual(back, { slot: 2, host: true, hostSlot: 2, epoch: 1n });
  assert.ok(b2.rules.restored[0]! > 0, "refreshed B restored the stored tick");
  const atResume = b2.ticks.length;
  await until("refreshed B steps again", () => b2.ticks.length > atResume + 5);
  b2.webHost.close();
  a.webHost.close();
});

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
