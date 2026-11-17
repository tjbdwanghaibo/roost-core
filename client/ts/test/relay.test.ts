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
  type Disconnect,
  type Member,
  type RelayClientOptions,
  type RelayFrame,
  type Welcome,
  type WebSocketLike,
} from "../src/index.js";
import { GAME, startPeer, type Peer } from "./peer.js";

let peer: Peer;
const players: Player[] = [];

before(async () => {
  peer = await startPeer();
  console.log(`peer: lobby ${peer.lobbyUrl}, relay ws ${peer.relayWs}, room directory ${peer.backend}`);
});

after(async () => {
  for (const player of players) player.client.close();
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
  const bad = new LobbyClient(peer.lobbyUrl, "gt1.not-issued");
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
  assert.deepEqual(hostWelcome, { slot: 1, host: true, hostSlot: 1 });
  await until("room listed", async () => {
    const rooms = await host.lobby.listRooms(GAME, "duel");
    return rooms.length === 1 && rooms[0].id === ticket.room.id;
  });

  const m1 = await Player.create("ts-m1", { password: "pw" });
  const m2 = await Player.create("ts-m2", { password: "pw" });
  assert.equal((await rejects(m1.lobby.joinRoom(ticket.room.id, { game: GAME, password: "bad" }), LobbyError)).code, "wrong_password");
  assert.deepEqual(await m1.join(ticket.room.id, "pw"), { slot: 2, host: false, hostSlot: 1 });
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
  assert.equal((await rejects(m2.client.rejoin(), RelayClosedError)).reason, RelayReason.Kicked);

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
  assert.deepEqual(back, { slot: first.slot, host: false, hostSlot: 1 });
  assert.equal(member.joins - joinsBefore, 1, "one lobby join per reconnect (a fresh relay token)");
  await until("host sees the member again", () => host.joined.drain().some((m) => m.account === "ts-member-r"));
  member.client.sendGame(ALL, 1, text("back"));
  assertFrame(await host.game.next("frame after reconnect"), RelayChannel.Game, back.slot, 1, "back");
  host.client.sendGame(back.slot, 2, text("welcome back"));
  assertFrame(await member.game.next("host frame after reconnect"), RelayChannel.Game, 1, 2, "welcome back");
  await until("rtt measured", () => member.client.rtt > 0);
  assert.equal(pongs.length, 0, "host pings every 10s by default: no pong yet");

  // 房主断开：这一批关房；成员收到 room_closed，开着自动重连也不重连。
  host.client.close();
  const closed = await member.disconnected.next("room closed");
  assert.deepEqual([closed.reason, closed.reconnecting], [RelayReason.RoomClosed, false]);
  await member.client.done;
  assert.equal(member.client.connected, false);
  const hostClosed = await host.disconnected.next("host local close");
  assert.equal(hostClosed.reason, RelayReason.None);
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
