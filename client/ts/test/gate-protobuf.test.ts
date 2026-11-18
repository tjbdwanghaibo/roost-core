// @roost/client/protobuf：按 protobuf-es 生成的 Schema 收发。生成物 test/gen/dao_sync_pb.ts 是 protoc 31.1 +
// protoc-gen-es（与 package.json 锁定同版本）对 codegen/internal/dao/testdata/golden/dao_sync.proto 的输出，
// scripts/test-client-ts.sh 的完整模式重新生成并核对它没过期。推送 client/spec/dao-sync.json 的 Go 字节，核对 64 位整数是
// bigint 且精确、零值包装存在；请求按 Schema 编码、应答按 Schema 解码。
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { create, fromBinary, toBinary } from "@bufbuild/protobuf";
import { BrowserWebSocketTransport, GameClient } from "../src/gate/index.js";
import { request, requestAsync, subscribe } from "../src/protobuf/index.js";
import { SyncHeroDaoLevelFieldSchema, SyncHeroDaoSchema, type SyncHeroDao } from "./gen/dao_sync_pb.js";
import { FakeGate, push, reply, until, nodeWebSocket } from "./gate-fake.js";
import { REPO_ROOT } from "./peer.js";

const rows = JSON.parse(readFileSync(join(REPO_ROOT, "client/spec/dao-sync.json"), "utf8")) as { name: string; hex: string }[];
const bytes = (hex: string) => new Uint8Array(Buffer.from(hex, "hex"));

test("push and request with protobuf-es schemas, 64-bit fields as exact bigint", async () => {
  const gate = await FakeGate.start();
  try {
    const client = new GameClient({
      webSocketUrl: gate.url,
      transportFactory: () => new BrowserWebSocketTransport({ webSocket: nodeWebSocket }),
      heartbeatIntervalMs: 0,
    });
    const pushes: SyncHeroDao[] = [];
    subscribe(client, 77, SyncHeroDaoSchema, (value) => pushes.push(value));
    const connecting = client.connection.connect("t");
    const conn = await gate.connection(0);
    await conn.handshake();
    await connecting;

    conn.send(push(77, bytes(rows[0].hex)), push(77, bytes(rows[1].hex)));
    await until("two pushes", () => pushes.length === 2);
    assert.equal(pushes[0].entityId, 9007199254740993n, "entity id above 2^53 stays exact");
    assert.equal(pushes[0].exp?.value, 1n << 60n);
    assert.equal(pushes[1].level?.value, 0, "a zero value inside a present wrapper");
    assert.equal(pushes[1].exp, undefined, "an unchanged field stays absent");

    let answered: SyncHeroDao | undefined;
    request(client, 31, 32, SyncHeroDaoSchema, { entityId: 9007199254740995n, level: { value: 7 } }, SyncHeroDaoSchema,
      (value) => (answered = value), (error) => assert.fail(error));
    const sent = await conn.frame();
    const decoded = fromBinary(SyncHeroDaoSchema, sent.payload);
    assert.equal(decoded.entityId, 9007199254740995n);
    assert.equal(decoded.level?.value, 7);
    conn.send(reply(sent, 32, toBinary(SyncHeroDaoSchema, create(SyncHeroDaoSchema, { entityId: decoded.entityId + 1n }))));
    await until("reply", () => answered !== undefined);
    assert.equal(answered!.entityId, 9007199254740996n);

    const pending = requestAsync(client.connection, 31, 32, SyncHeroDaoSchema, create(SyncHeroDaoSchema, { level: create(SyncHeroDaoLevelFieldSchema, { value: 1 }) }), SyncHeroDaoSchema);
    const third = await conn.frame();
    conn.send(reply(third, 32, toBinary(SyncHeroDaoSchema, create(SyncHeroDaoSchema, { entityId: 5n }))));
    assert.equal((await pending).entityId, 5n);

    let failed: Error | undefined;
    request(client, 31, 32, SyncHeroDaoSchema, {}, SyncHeroDaoSchema, () => assert.fail("garbage parsed"), (error) => (failed = error));
    const fourth = await conn.frame();
    conn.send(reply(fourth, 32, Uint8Array.of(0xff, 0xff, 0xff)));
    await until("parse failure", () => failed !== undefined);
    assert.ok(failed instanceof Error, "an unparseable reply goes to failed");
    client.dispose();
    assert.deepEqual(gate.violations(), []);
  } finally {
    await gate.close();
  }
});
