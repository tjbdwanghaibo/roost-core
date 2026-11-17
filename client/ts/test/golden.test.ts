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
  decodeEpoch,
  decodeFrame,
  decodeHostChanged,
  decodeMember,
  decodeRejected,
  decodeSnapshot,
  decodeSuccessors,
  decodeWelcome,
  decodeYouAreHost,
  encodeEpoch,
  encodeFrame,
  encodeHostChanged,
  encodeMember,
  encodeRejected,
  encodeSnapshot,
  encodeSuccessors,
  encodeToken,
  encodeWelcome,
  encodeYouAreHost,
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
  relay_welcome: frame(RelayChannel.Control, 3, RelayOp.Welcome, encodeWelcome({ slot: 3, host: false, hostSlot: 1 })),
  relay_member_joined: frame(RelayChannel.Control, 3, RelayOp.MemberJoined, encodeMember({ slot: 3, account: "a3", reason: RelayReason.None })),
  relay_member_left: frame(RelayChannel.Control, 3, RelayOp.MemberLeft, encodeMember({ slot: 3, account: "a3", reason: RelayReason.Kicked })),
  relay_closed: frame(RelayChannel.Control, 0, RelayOp.Closed, [RelayReason.RoomClosed]),
  relay_muted: frame(RelayChannel.Control, 3, RelayOp.Muted, [1]),
  relay_kick: frame(RelayChannel.Control, 3, RelayOp.Kick, []),
  relay_mute: frame(RelayChannel.Control, 3, RelayOp.Mute, [1]),
  relay_ping: frame(RelayChannel.Control, 0, RelayOp.Ping, [0, 0, 0, 42]),
  relay_pong: frame(RelayChannel.Control, 0, RelayOp.Pong, [0, 0, 0, 42]),
  // 换 host（docs/framework/RELAY.md §2.3），共 13 条
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
      state: Uint8Array.of(0xab, 0xcd),
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
      state: new TextEncoder().encode("h"),
    }),
  ),
  relay_host_changing: frame(RelayChannel.Control, 0, RelayOp.HostChanging, encodeEpoch(2n)),
  relay_host_changed: frame(RelayChannel.Control, 3, RelayOp.HostChanged, encodeHostChanged(2n, "a3")),
  relay_host_resumed: frame(RelayChannel.Control, 1, RelayOp.HostResumed, encodeEpoch(0n)),
  relay_rejected_snapshot_too_soon: frame(RelayChannel.Control, 0, RelayOp.Rejected, encodeRejected({ op: RelayOp.Snapshot, code: RejectCode.TooSoon, seq: 8n })),
  relay_transfer: frame(RelayChannel.Control, 3, RelayOp.Transfer, new TextEncoder().encode("handoff")),
  relay_leave: frame(RelayChannel.Control, 0, RelayOp.Leave, []),
  relay_successors: frame(RelayChannel.Control, 0, RelayOp.Successors, encodeSuccessors([3, 2])),
  relay_snapshot: frame(RelayChannel.Control, 0, RelayOp.Snapshot, encodeSnapshot(7n, Uint8Array.of(0xab, 0xcd))),
  relay_host_ready: frame(RelayChannel.Control, 0, RelayOp.HostReady, encodeEpoch(2n)),
  relay_closed_lobby_rejected: frame(RelayChannel.Control, 0, RelayOp.Closed, [RelayReason.LobbyRejected]),
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
  assert.deepEqual(decodeWelcome(welcome.slot, welcome.payload), { slot: 3, host: false, hostSlot: 1 });
  const left = decodeFrame(bytes(golden.get("relay_member_left")!));
  assert.deepEqual(decodeMember(left.slot, left.payload), { slot: 3, account: "a3", reason: RelayReason.Kicked });
});

test("TS decodes the host-change golden frames into the expected fields", () => {
  const payload = (name: string) => decodeFrame(bytes(golden.get(name)!)).payload;
  assert.equal(golden.size, 27, "14 base frames + 13 host-change frames");
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
    state: Uint8Array.of(0xab, 0xcd),
  });
  const handoff = decodeYouAreHost(payload("relay_you_are_host_handoff"));
  assert.deepEqual([handoff.epoch, handoff.source, handoff.snapshotSeq, new TextDecoder().decode(handoff.state)], [1n, StateSource.Handoff, 0n, "h"]);
  assert.deepEqual(handoff.members.map((m) => [m.slot, m.online, m.account]), [[1, true, "a1"], [2, true, "a2"]]);
  assert.equal(decodeEpoch(payload("relay_host_changing")), 2n);
  const changed = decodeFrame(bytes(golden.get("relay_host_changed")!));
  assert.deepEqual([changed.slot, decodeHostChanged(changed.payload)], [3, { epoch: 2n, account: "a3" }]);
  assert.equal(decodeEpoch(payload("relay_host_resumed")), 0n);
  assert.deepEqual(decodeRejected(payload("relay_rejected_snapshot_too_soon")), { op: RelayOp.Snapshot, code: RejectCode.TooSoon, seq: 8n });
  assert.deepEqual(decodeSuccessors(payload("relay_successors")), [3, 2]);
  const snapshot = decodeSnapshot(payload("relay_snapshot"));
  assert.deepEqual([snapshot.seq, [...snapshot.data]], [7n, [0xab, 0xcd]]);
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
