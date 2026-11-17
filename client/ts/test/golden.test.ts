// 跨语言金样：TS SDK 编出的帧与 client/spec/relay-packets.json（Go 的 client/wire 生成）逐字节相同；relay 发来的控制帧
// 解出预期字段；解码规则与 Go 的 ValidateRelay 一致。Go（client/relay/golden_test.go）与 C#（Roost.Client.Relay.Tests）
// 对同一份金样做同样的核对。
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
  ALL,
  FLAG_PUSH,
  FLAG_SYNC,
  FrameReader,
  RelayChannel,
  RelayOp,
  RelayProtocolError,
  RelayReason,
  RejectCode,
  StateSource,
  UnknownCompressionError,
  decodeEpoch,
  decodeFrame,
  decodeHostChanged,
  decodeMember,
  decodePresence,
  decodeRejected,
  decodeSnapshot,
  decodeSuccessors,
  decodeWelcome,
  decodeYouAreHost,
  encodeEpoch,
  encodeFrame,
  encodeHostChanged,
  encodeMember,
  encodePresence,
  encodeRejected,
  encodeSnapshot,
  encodeSuccessors,
  encodeToken,
  encodeWelcome,
  encodeYouAreHost,
  defaultCompressor,
  identityCompressor,
  unpackState,
  type RelayFrame,
} from "../src/index.js";
import { REPO_ROOT } from "./peer.js";

const golden = new Map<string, string>(
  (JSON.parse(readFileSync(join(REPO_ROOT, "client/spec/relay-packets.json"), "utf8")) as { name: string; hex: string }[]).map((e) => [e.name, e.hex]),
);
const hex = (data: Uint8Array) => Buffer.from(data).toString("hex");
const bytes = (text: string) => new Uint8Array(Buffer.from(text, "hex"));
const frame = (channel: RelayChannel, slot: number, msgId: number, payload: number[] | Uint8Array, flags = 0): RelayFrame => ({
  channel,
  slot,
  msgId,
  flags,
  payload: payload instanceof Uint8Array ? payload : Uint8Array.from(payload),
});

const sent: Record<string, RelayFrame> = {
  relay_game_member_up: frame(RelayChannel.Game, ALL, 101, [1, 2]),
  relay_game_to_host: frame(RelayChannel.Game, 3, 101, [1, 2]),
  relay_game_host_to_slot: frame(RelayChannel.Game, 3, 202, [9], FLAG_PUSH | FLAG_SYNC),
  relay_room_broadcast: frame(RelayChannel.Room, ALL, 7, new TextEncoder().encode("hi")),
  relay_welcome: frame(RelayChannel.Control, 3, RelayOp.Welcome, encodeWelcome({ slot: 3, host: false, hostSlot: 1, epoch: 5n })),
  relay_member_joined: frame(RelayChannel.Control, 3, RelayOp.MemberJoined, encodeMember({ slot: 3, account: "a3", reason: RelayReason.None })),
  relay_member_left: frame(RelayChannel.Control, 3, RelayOp.MemberLeft, encodeMember({ slot: 3, account: "a3", reason: RelayReason.Kicked })),
  relay_closed: frame(RelayChannel.Control, 0, RelayOp.Closed, [RelayReason.RoomClosed]),
  relay_muted: frame(RelayChannel.Control, 3, RelayOp.Muted, [1]),
  relay_kick: frame(RelayChannel.Control, 3, RelayOp.Kick, []),
  relay_mute: frame(RelayChannel.Control, 3, RelayOp.Mute, [1]),
  relay_ping: frame(RelayChannel.Control, 0, RelayOp.Ping, [0, 0, 0, 42]),
  relay_pong: frame(RelayChannel.Control, 0, RelayOp.Pong, [0, 0, 0, 42]),
  // 换 host（docs/framework/RELAY.md §2.3），共 13 条。状态字节以压缩方式标识开头（0 不压缩）
  relay_you_are_member: frame(RelayChannel.Control, 1, RelayOp.YouAreMember, encodeEpoch(2n)),
  relay_you_are_host_snapshot: frame(
    RelayChannel.Control,
    3,
    RelayOp.YouAreHost,
    encodeYouAreHost({
      epoch: 2n,
      source: StateSource.Snapshot,
      snapshotSeq: 7n,
      snapshotAgeMs: 1500,
      members: [
        { slot: 1, online: false, account: "a1" },
        { slot: 3, online: true, account: "a3" },
      ],
      state: Uint8Array.of(0, 0xab, 0xcd),
    }),
  ),
  relay_you_are_host_handoff: frame(
    RelayChannel.Control,
    2,
    RelayOp.YouAreHost,
    encodeYouAreHost({
      epoch: 1n,
      source: StateSource.Handoff,
      snapshotSeq: 0n,
      snapshotAgeMs: 0,
      members: [
        { slot: 1, online: true, account: "a1" },
        { slot: 2, online: true, account: "a2" },
      ],
      state: Uint8Array.of(0, 0x68),
    }),
  ),
  relay_host_changing: frame(RelayChannel.Control, 0, RelayOp.HostChanging, encodeEpoch(2n)),
  relay_host_changed: frame(RelayChannel.Control, 3, RelayOp.HostChanged, encodeHostChanged(2n, "a3")),
  relay_host_resumed: frame(RelayChannel.Control, 1, RelayOp.HostResumed, encodeEpoch(0n)),
  relay_rejected_snapshot_too_soon: frame(RelayChannel.Control, 0, RelayOp.Rejected, encodeRejected({ op: RelayOp.Snapshot, code: RejectCode.TooSoon, seq: 8n })),
  relay_transfer: frame(RelayChannel.Control, 3, RelayOp.Transfer, Uint8Array.of(0, ...new TextEncoder().encode("handoff"))),
  relay_leave: frame(RelayChannel.Control, 0, RelayOp.Leave, []),
  relay_successors: frame(RelayChannel.Control, 0, RelayOp.Successors, encodeSuccessors([3, 2])),
  relay_snapshot: frame(RelayChannel.Control, 0, RelayOp.Snapshot, encodeSnapshot(7n, Uint8Array.of(0, 0xab, 0xcd))),
  relay_host_ready: frame(RelayChannel.Control, 0, RelayOp.HostReady, encodeEpoch(2n)),
  relay_closed_lobby_rejected: frame(RelayChannel.Control, 0, RelayOp.Closed, [RelayReason.LobbyRejected]),
  // 前台上报：成员 → relay 的 Presence{前台}，relay → 房主的 MemberPresence{槽位 3，后台}
  relay_presence: frame(RelayChannel.Control, 0, RelayOp.Presence, encodePresence(true)),
  relay_member_presence: frame(RelayChannel.Control, 3, RelayOp.MemberPresence, encodePresence(false)),
};

test("TS encodes every relay golden frame byte for byte like Go", () => {
  // 金样以后会追加（换 host 的 64～127）；这里核对本 SDK 认识的每一条都在、都相同。
  for (const [name, f] of Object.entries(sent)) {
    assert.ok(golden.has(name), `golden is missing ${name}`);
    assert.equal(hex(encodeFrame(f)), golden.get(name), name);
  }
  assert.equal(hex(encodeToken("rt1.token")), golden.get("relay_token"));
});

test("TS decodes the relay golden frames", () => {
  for (const [name, text] of golden) {
    const decoded = decodeFrame(bytes(text));
    if (name === "relay_token") {
      assert.equal(decoded.channel, RelayChannel.None);
      assert.equal(new TextDecoder().decode(decoded.payload), "rt1.token");
      continue;
    }
    assert.equal(hex(encodeFrame(decoded)), text, `${name} round trip`);
  }
  const hostToSlot = decodeFrame(bytes(golden.get("relay_game_host_to_slot")!));
  assert.deepEqual([hostToSlot.channel, hostToSlot.slot, hostToSlot.msgId, hostToSlot.flags], [RelayChannel.Game, 3, 202, FLAG_PUSH | FLAG_SYNC]);
  const welcome = decodeFrame(bytes(golden.get("relay_welcome")!));
  assert.deepEqual(decodeWelcome(welcome.slot, welcome.payload), { slot: 3, host: false, hostSlot: 1, epoch: 5n });
  assert.throws(() => decodeWelcome(3, welcome.payload.subarray(0, 5)), RelayProtocolError, "the old 5-byte welcome is refused");
  const left = decodeFrame(bytes(golden.get("relay_member_left")!));
  assert.deepEqual(decodeMember(left.slot, left.payload), { slot: 3, account: "a3", reason: RelayReason.Kicked });
});

test("TS decodes the host-change golden frames into the expected fields", () => {
  const payload = (name: string) => decodeFrame(bytes(golden.get(name)!)).payload;
  assert.equal(golden.size, 31, "14 base frames + 13 host-change frames + 2 presence frames + 2 compression-marker frames");
  assert.equal(decodeEpoch(payload("relay_you_are_member")), 2n);
  assert.deepEqual(decodeYouAreHost(payload("relay_you_are_host_snapshot")), {
    epoch: 2n,
    source: StateSource.Snapshot,
    snapshotSeq: 7n,
    snapshotAgeMs: 1500,
    members: [
      { slot: 1, online: false, account: "a1" },
      { slot: 3, online: true, account: "a3" },
    ],
    state: Uint8Array.of(0, 0xab, 0xcd),
  });
  const handoff = decodeYouAreHost(payload("relay_you_are_host_handoff"));
  assert.deepEqual([handoff.epoch, handoff.source, handoff.snapshotSeq, [...handoff.state]], [1n, StateSource.Handoff, 0n, [0, 0x68]]);
  assert.deepEqual(handoff.members.map((m) => [m.slot, m.online, m.account]), [[1, true, "a1"], [2, true, "a2"]]);
  assert.equal(decodeEpoch(payload("relay_host_changing")), 2n);
  const changed = decodeFrame(bytes(golden.get("relay_host_changed")!));
  assert.deepEqual([changed.slot, decodeHostChanged(changed.payload)], [3, { epoch: 2n, account: "a3" }]);
  assert.equal(decodeEpoch(payload("relay_host_resumed")), 0n);
  assert.deepEqual(decodeRejected(payload("relay_rejected_snapshot_too_soon")), { op: RelayOp.Snapshot, code: RejectCode.TooSoon, seq: 8n });
  assert.deepEqual(decodeSuccessors(payload("relay_successors")), [3, 2]);
  const snapshot = decodeSnapshot(payload("relay_snapshot"));
  assert.deepEqual([snapshot.seq, [...snapshot.data]], [7n, [0, 0xab, 0xcd]]);
  assert.equal(decodeEpoch(payload("relay_host_ready")), 2n);
  assert.deepEqual([...payload("relay_closed_lobby_rejected")], [RelayReason.LobbyRejected]);
  // 截断的 YouAreHost、非法来源、长度不对的 epoch 都当协议错误
  const youAreHost = payload("relay_you_are_host_snapshot");
  assert.throws(() => decodeYouAreHost(youAreHost.subarray(0, 30)), RelayProtocolError);
  const badSource = youAreHost.slice();
  badSource[8] = 3;
  assert.throws(() => decodeYouAreHost(badSource), RelayProtocolError);
  assert.throws(() => decodeEpoch(Uint8Array.of(1, 2, 3)), RelayProtocolError);
});

test("presence frames and the compression marker in the golden", async () => {
  const payload = (name: string) => decodeFrame(bytes(golden.get(name)!)).payload;
  assert.equal(decodePresence(payload("relay_presence")), true);
  const memberPresence = decodeFrame(bytes(golden.get("relay_member_presence")!));
  assert.deepEqual([memberPresence.slot, decodePresence(memberPresence.payload)], [3, false]);
  assert.throws(() => decodePresence(Uint8Array.of(2)), RelayProtocolError);
  assert.throws(() => decodePresence(new Uint8Array(0)), RelayProtocolError);
  // 标识 1（raw deflate）：哪种 compressor 的客户端都解得开；标识 0 是原样；0x7f 没有分配。
  const deflated = decodeYouAreHost(payload("relay_you_are_host_deflate")).state;
  for (const compressor of [defaultCompressor(), identityCompressor]) {
    assert.equal(new TextDecoder().decode(await unpackState(compressor, deflated, 1 << 20)), "hi", compressor.name);
  }
  assert.deepEqual([...(await unpackState(defaultCompressor(), decodeYouAreHost(payload("relay_you_are_host_snapshot")).state, 1 << 20))], [0xab, 0xcd]);
  const unknown = decodeYouAreHost(payload("relay_you_are_host_unknown_compression")).state;
  await assert.rejects(unpackState(defaultCompressor(), unknown, 1 << 20), (error: unknown) => error instanceof UnknownCompressionError && error.method === 0x7f);
});

test("the stream reader does not depend on message boundaries", () => {
  const all = Buffer.concat([...golden.values()].map((h) => Buffer.from(h, "hex")));
  const reader = new FrameReader();
  const frames: RelayFrame[] = [];
  for (let i = 0; i < all.length; i += 7) frames.push(...reader.push(new Uint8Array(all.subarray(i, i + 7)))); // 一帧拆成几段
  assert.equal(frames.length, golden.size);
  assert.equal(reader.buffered, 0);
});

test("TS refuses the same headers as Go's ValidateRelay", () => {
  for (const [what, text] of Object.entries({
    "reserved bit 5": "52530238000000010000000000000000",
    "control frame without an op": "52530218000000000000000000000000",
    "control frame with the push bit": "52530219000000200000000000000000",
    "sync and lockstep together": "5253020e000000010000000000000000",
    "token frame with a flag": "52530201000000000000000000000000",
  })) {
    assert.throws(() => decodeFrame(bytes(text)), RelayProtocolError, what);
  }
  assert.throws(() => decodeFrame(bytes("5253020800000001000000000000000b" + "00".repeat(11)), 10), RelayProtocolError, "payload over the limit");
});
