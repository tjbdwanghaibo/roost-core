// 玩家接入（Gate）的跨语言金样与纯函数：client/spec/packets.json（Go 的 client/wire 生成，C# 的 Program.cs 同样核对）
// 逐字节编出与解开；Gate 包头的拒绝规则与 Go 的 Header.Validate、C# 的 PacketCodec.CheckHeader 一致；鉴权信封与连接代号；
// NetworkClock 的拒绝条件照 C# 的判定；不依赖 TextEncoder 的 UTF-8 与 TextEncoder 逐字节相同。
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
  CLOCK_PROBE,
  GateFrameReader,
  GateProtocolError,
  MAX_TICKET_BYTES,
  NetworkClock,
  PayloadKind,
  decodeAuth,
  decodeClockSample,
  decodePacket,
  encodeAuth,
  encodePacket,
  nextConnectionGeneration,
  type Packet,
} from "../src/gate/index.js";
import { utf8Encode } from "../src/wire/header.js";
import { REPO_ROOT } from "./peer.js";

interface GoldenRow {
  name: string;
  hex: string;
  generation?: string;
}

const rows = JSON.parse(readFileSync(join(REPO_ROOT, "client/spec/packets.json"), "utf8")) as GoldenRow[];
const golden = new Map(rows.map((row) => [row.name, row]));
const bytes = (text: string) => new Uint8Array(Buffer.from(text, "hex"));
const hex = (data: Uint8Array) => Buffer.from(data).toString("hex");
const packet = (fields: Partial<Packet>): Packet => ({
  messageId: 1,
  sequence: 0,
  isPush: false,
  kind: PayloadKind.Protobuf,
  payload: new Uint8Array(0),
  ...fields,
});
const header = (flags: number, msgId: number, size: number) => {
  const data = new Uint8Array(16);
  data.set([0x52, 0x53, 2, flags]);
  new DataView(data.buffer).setUint32(4, msgId);
  new DataView(data.buffer).setUint32(12, size);
  return data;
};

test("every packets.json row decodes and re-encodes byte for byte", () => {
  assert.ok(rows.length >= 12, `packets.json has ${rows.length} rows`);
  for (const row of rows) {
    const decoded = decodePacket(bytes(row.hex));
    assert.equal(hex(encodePacket(decoded)), row.hex, `${row.name} re-encodes`);
  }
});

test("pb, sync, auth and auth_generation carry the fields Go wrote", () => {
  const pb = decodePacket(bytes(golden.get("pb")!.hex));
  assert.deepEqual({ ...pb, payload: hex(pb.payload) }, { messageId: 42, sequence: 7, isPush: false, kind: PayloadKind.Protobuf, payload: "089601" });

  const sync = decodePacket(bytes(golden.get("sync")!.hex));
  assert.equal(sync.kind, PayloadKind.Sync);
  assert.equal(sync.isPush, true);
  assert.equal(sync.messageId, 10103);
  assert.equal(hex(sync.payload.subarray(0, 3)), "524602", "raw Sync frame (RF v2), not PB");

  const auth = decodePacket(bytes(golden.get("auth")!.hex));
  assert.deepEqual([auth.messageId, auth.sequence, auth.isPush, auth.kind], [0, 1, false, PayloadKind.Protobuf]);
  assert.deepEqual(decodeAuth(auth.payload), { ticket: utf8Encode("ticket"), generation: 0n });

  const row = golden.get("auth_generation")!;
  const enveloped = decodePacket(bytes(row.hex));
  const { ticket, generation } = decodeAuth(enveloped.payload);
  assert.equal(generation, BigInt(row.generation!));
  assert.equal(Buffer.from(ticket).toString(), "ticket");
  assert.equal(hex(encodePacket(packet({ messageId: 0, sequence: 1, payload: encodeAuth(utf8Encode("ticket"), generation) }))), row.hex, "TS writes the same envelope");
});

test("Gate header rules match client/wire: flag bits 3..7, kind 3, control flags, size", () => {
  const reject = (data: Uint8Array, why: string) =>
    assert.throws(() => new GateFrameReader(1 << 20).push(data), GateProtocolError, why);
  reject(header(1 << 3, 1, 0), "bit 3 (a relay channel bit) is not a Gate flag");
  reject(header(1 << 7, 1, 0), "bit 7");
  reject(header(6, 1, 0), "payload kind 3 is reserved");
  reject(header(1, 0, 0), "a control frame (message id 0) with the push bit");
  reject(header(2, 0, 0), "a control frame with a payload kind");
  reject(header(0, 1, (1 << 20) + 1), "payload over the limit, before the payload arrives");
  const bad = header(0, 1, 0);
  bad[2] = 1;
  reject(bad, "RS version 1");
  assert.throws(() => decodePacket(new Uint8Array(15)), GateProtocolError, "truncated header");
  assert.throws(() => decodePacket(Uint8Array.of(...encodePacket(packet({})), 0)), GateProtocolError, "trailing byte");
  assert.throws(() => encodePacket(packet({ messageId: 0, isPush: true })), GateProtocolError, "encoder refuses a control push");
  assert.throws(() => encodePacket(packet({ kind: 3 as PayloadKind })), GateProtocolError, "encoder refuses kind 3");
  assert.throws(() => encodePacket(packet({ payload: new Uint8Array(11) }), 10), GateProtocolError, "encoder refuses an oversized payload");
});

test("the frame reader joins split frames and splits merged ones", () => {
  const frames = [packet({ messageId: 9, sequence: 1, payload: utf8Encode("abc") }), packet({ messageId: 10, sequence: 2, isPush: true, kind: PayloadKind.Sync })];
  const stream = Uint8Array.from([...encodePacket(frames[0]), ...encodePacket(frames[1])]);
  for (const step of [1, 7, 16, stream.length]) {
    const reader = new GateFrameReader();
    const got: Packet[] = [];
    for (let i = 0; i < stream.length; i += step) got.push(...reader.push(stream.slice(i, i + step)));
    assert.deepEqual(got.map((p) => [p.messageId, p.sequence, p.isPush, p.kind, hex(p.payload)]), [
      [9, 1, false, PayloadKind.Protobuf, "616263"],
      [10, 2, true, PayloadKind.Sync, ""],
    ], `chunks of ${step}`);
    assert.equal(reader.buffered, 0);
  }
});

test("auth envelope rules match the server: ticket bytes, NUL, generation", () => {
  assert.throws(() => encodeAuth(new Uint8Array(0), 1n), RangeError);
  assert.throws(() => encodeAuth(Uint8Array.of(0, 1), 1n), RangeError);
  assert.throws(() => encodeAuth(utf8Encode("t"), 0n), RangeError);
  assert.throws(() => encodeAuth(utf8Encode("t"), 1n << 64n), RangeError);
  assert.throws(() => decodeAuth(new Uint8Array(0)), GateProtocolError);
  assert.throws(() => decodeAuth(Uint8Array.of(0, 0x48, 0, 0, 0, 0, 0, 0, 0, 1, 0x74)), GateProtocolError, "wrong tag");
  assert.throws(() => decodeAuth(Uint8Array.of(0, 0x47, 0, 0, 0, 0, 0, 0, 0, 0, 0x74)), GateProtocolError, "generation 0");
  assert.throws(() => decodeAuth(Uint8Array.of(0, 0x47, 0, 0, 0, 0, 0, 0, 0, 1)), GateProtocolError, "no ticket");
  assert.equal(MAX_TICKET_BYTES, 8192 - 10);
});

test("connection generations are Unix ms × 1e6 and strictly increase even when the clock stands still or goes back", () => {
  const first = nextConnectionGeneration(() => 1_700_000_000_000);
  assert.ok(first >= 1_700_000_000_000n * 1_000_000n);
  const same = nextConnectionGeneration(() => 1_700_000_000_000);
  const back = nextConnectionGeneration(() => 1_600_000_000_000);
  assert.ok(same > first && back > same, `${first} < ${same} < ${back}`);
  const later = nextConnectionGeneration(() => 1_800_000_000_000);
  assert.equal(later, 1_800_000_000_000n * 1_000_000n);
});

test("utf8Encode (no TextEncoder) matches TextEncoder", () => {
  for (const text of ["ticket", "中文票据", "emoji 😀", "lone \ud800 surrogate", "\u0000\u007f\u0080߿ࠀ￿"]) {
    assert.equal(hex(utf8Encode(text)), hex(new TextEncoder().encode(text)), text);
  }
});

test("clock probe payload and response decode", () => {
  assert.equal(Buffer.from(CLOCK_PROBE).toString(), "RCN1");
  const reply = new Uint8Array(20);
  reply.set(CLOCK_PROBE);
  new DataView(reply.buffer).setBigInt64(4, 1_700_000_000_123_000_000n);
  new DataView(reply.buffer).setBigInt64(12, 1_700_000_000_124_000_000n);
  const sample = decodeClockSample(1, 1.01, reply);
  // 与 C# 相同：int64 纳秒 / 1e9 得双精度秒（这一量级上精度约 0.2 微秒）。
  assert.ok(Math.abs(sample.serverReceived - 1_700_000_000.123) < 1e-6, `${sample.serverReceived}`);
  assert.ok(Math.abs(sample.serverSent - 1_700_000_000.124) < 1e-6, `${sample.serverSent}`);
  assert.throws(() => decodeClockSample(0, 0, reply.subarray(0, 19)), GateProtocolError);
  const wrong = reply.slice();
  wrong[3] = 50;
  assert.throws(() => decodeClockSample(0, 0, wrong), GateProtocolError);
});

test("NetworkClock rejects the samples C# rejects and slews toward the newest", () => {
  assert.throws(() => new NetworkClock(0), RangeError);
  assert.throws(() => new NetworkClock(2, 1), RangeError);
  const clock = new NetworkClock();
  assert.throws(() => clock.now(1), /no sample/);
  const at = (sent: number, received: number, serverReceived: number, serverSent: number) => ({ sent, received, serverReceived, serverSent });
  assert.equal(clock.observe(at(10, 9, 100, 100)), false, "negative elapsed");
  assert.equal(clock.observe(at(10, 10.1, 100.02, 100.01)), false, "negative processing");
  assert.equal(clock.observe(at(10, 10.01, 100, 100.05)), false, "processing longer than elapsed");
  assert.equal(clock.observe(at(10, 13, 100, 100)), false, "round trip over maxRoundTrip");
  assert.equal(clock.observe(at(Number.NaN, 10, 100, 100)), false, "not finite");
  assert.equal(clock.observe(at(10, 10.02, 100.005, 100.005)), true);
  assert.ok(clock.ready);
  assert.ok(Math.abs(clock.roundTrip - 0.02) < 1e-9);
  assert.ok(Math.abs(clock.now(10.02) - 100.015) < 1e-9, "offset = serverSent + rtt/2 - received");
  assert.equal(clock.observe(at(10.01, 10.02, 100, 100)), false, "not newer than the last sample");
  assert.equal(clock.observe(at(11, 11.5, 101, 101)), false, "queued probe (rtt > max(0.05, 3 × rtt))");
  assert.equal(clock.observe(at(12, 12.02, 103.01, 103.01)), true, "a newer good sample moves the target");
  const before = clock.now(12.02);
  const after = clock.now(13.02);
  assert.ok(after - 13.02 - (before - 12.02) <= 0.05 + 1e-9, "offset moves at most slewRate per second");
  assert.throws(() => clock.now(1), RangeError, "local time went backwards");
  clock.reset();
  assert.equal(clock.ready, false);
});
