// 不连真实对端的客户端行为：用一个假的 WebSocket 扮 relay。
import { test } from "node:test";
import assert from "node:assert/strict";
import { RelayChannel, RelayClient, decodeFrame, encodeFrame, encodeWelcome, type RelayFrame, type WebSocketLike } from "../src/index.js";

/** 收到令牌首帧后，按 script 一次推一条消息（可以一条里几帧）。 */
class FakeRelay implements WebSocketLike {
  binaryType = "arraybuffer";
  readyState = 0;
  sent: Uint8Array[] = [];
  onopen: ((event: unknown) => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: unknown) => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;
  constructor(private readonly script: Uint8Array[]) {
    setTimeout(() => {
      this.readyState = 1;
      this.onopen?.({});
    }, 0);
  }
  send(data: Uint8Array): void {
    this.sent.push(data);
    if (this.sent.length === 1) {
      for (const message of this.script) this.onmessage?.({ data: message.slice().buffer });
    }
  }
  close(): void {
    this.readyState = 3;
    this.onclose?.({});
  }
}

const concat = (...parts: Uint8Array[]) => Uint8Array.from(parts.flatMap((p) => [...p]));

test("unknown control ops are dropped (or handed to onControl) and frames after the welcome are not lost", async () => {
  const welcome = encodeFrame({ channel: RelayChannel.Control, slot: 2, msgId: 1, payload: encodeWelcome({ slot: 2, host: false, hostSlot: 1 }) });
  const unknown = encodeFrame({ channel: RelayChannel.Control, slot: 2, msgId: 64, payload: Uint8Array.of(7) }); // 换 host 留的操作码
  const game = encodeFrame({ channel: RelayChannel.Game, slot: 1, msgId: 9, payload: Uint8Array.of(1) });
  const fake = new FakeRelay([concat(welcome, unknown), game]); // Welcome 与后面的帧在同一条消息里
  const client = new RelayClient(undefined, { game: "party", webSocket: () => fake });
  const games: RelayFrame[] = [];
  const unknowns: RelayFrame[] = [];
  client.onGame((f) => games.push(f));
  client.onControl(64, (f) => unknowns.push(f));
  const w = await client.connect({ room: { id: "r" }, relayAddr: "ws://fake/relay", token: "rt1.t" });
  assert.deepEqual(w, { slot: 2, host: false, hostSlot: 1 });
  assert.equal(unknowns.length, 1, "op 64 reached onControl");
  assert.equal(games.length, 1, "game frame after an unknown op still delivered");
  assert.ok(client.connected);
  assert.equal(decodeFrame(fake.sent[0]).channel, RelayChannel.None, "first frame is the token");
  client.onControl(64, undefined);
  client.close();
  await client.done;
});
