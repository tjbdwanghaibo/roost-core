// 快照压缩：CompressionStream('deflate-raw') 与 node:zlib 的 raw deflate 往返、两者字节互通、与 Go 的 compress/flate
// （client/relay 的缺省 Deflate）双向互通、解压上限（16 MiB，防解压炸弹）、可替换。
import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import * as zlib from "node:zlib";
import {
  MAX_STATE_BYTES,
  RelayClient,
  RelayOp,
  decodeSnapshot,
  defaultCompressor,
  deflateRawCompressor,
  identityCompressor,
  noCompression,
  packState,
  unpackState,
  zlibCompressor,
  UnknownCompressionError,
  type Compressor,
} from "../src/index.js";
import { FakeRelay, TICKET } from "./fake.js";

/** 可压缩的数据（重复度高），加一段随机字节。 */
function sample(size: number): Uint8Array {
  const out = new Uint8Array(size);
  for (let i = 0; i < size; i++) out[i] = i % 97 < 80 ? (i % 13) + 65 : (Math.random() * 256) | 0;
  return out;
}

const LIMIT = MAX_STATE_BYTES;

test("compression round trips: CompressionStream, zlib, default and no compression", async () => {
  const data = sample(300 * 1024);
  for (const compressor of [deflateRawCompressor(), zlibCompressor(zlib), defaultCompressor(), identityCompressor]) {
    const packed = await compressor.compress(data);
    if (compressor !== identityCompressor) assert.ok(packed.length < data.length / 2, `${compressor.name} shrinks the sample`);
    assert.deepEqual(await compressor.decompress(packed, LIMIT), data, compressor.name);
  }
  assert.equal(noCompression, identityCompressor);
  assert.deepEqual(await deflateRawCompressor().decompress(await deflateRawCompressor().compress(new Uint8Array(0)), LIMIT), new Uint8Array(0));
});

test("decompression stops at the limit (inflate bomb)", async () => {
  const bomb = zlib.deflateRawSync(new Uint8Array(LIMIT + 1)); // 16 MiB + 1 个零，压缩后约 16 KiB
  assert.ok(bomb.length < 64 * 1024);
  for (const compressor of [deflateRawCompressor(), zlibCompressor(zlib), defaultCompressor()]) {
    await assert.rejects(compressor.decompress(new Uint8Array(bomb), LIMIT), /exceeds/, compressor.name);
    assert.equal((await compressor.decompress(new Uint8Array(bomb), LIMIT + 1)).length, LIMIT + 1, `${compressor.name} at exactly the limit`);
  }
  await assert.rejects(identityCompressor.decompress(new Uint8Array(11), 10), /exceeds/);
});

test("browser CompressionStream bytes and node zlib bytes are interchangeable", async () => {
  const data = sample(64 * 1024);
  assert.deepEqual(await zlibCompressor(zlib).decompress(await deflateRawCompressor().compress(data), LIMIT), data);
  assert.deepEqual(await deflateRawCompressor().decompress(await zlibCompressor(zlib).compress(data), LIMIT), data);
});

test("TS and Go (compress/flate, the Go SDK's default Deflate) inflate each other's bytes", async () => {
  // 小程序只用标准库的 compress/flate（client/relay 的 Deflate 就是 flate.NewWriter(DefaultCompression) / flate.NewReader）：
  // 第一个参数 deflate 时把 stdin 压缩后输出，inflate 时把 stdin 解压后输出。
  const dir = mkdtempSync(join(tmpdir(), "roost-flate-"));
  try {
    writeFileSync(
      join(dir, "main.go"),
      `package main

import (
	"compress/flate"
	"io"
	"os"
)

func main() {
	if os.Args[1] == "deflate" {
		w, _ := flate.NewWriter(os.Stdout, flate.DefaultCompression)
		if _, err := io.Copy(w, os.Stdin); err != nil {
			panic(err)
		}
		if err := w.Close(); err != nil {
			panic(err)
		}
		return
	}
	if _, err := io.Copy(os.Stdout, flate.NewReader(os.Stdin)); err != nil {
		panic(err)
	}
}
`,
    );
    const binary = join(dir, "flate");
    execFileSync("go", ["build", "-o", binary, join(dir, "main.go")], { cwd: dir, env: { ...process.env, GOWORK: "off", GO111MODULE: "off" } });
    const go = (mode: string, input: Uint8Array) => new Uint8Array(execFileSync(binary, [mode], { input, maxBuffer: 64 << 20 }));
    const data = sample(200 * 1024);
    for (const compressor of [deflateRawCompressor(), zlibCompressor(zlib)]) {
      assert.deepEqual(go("inflate", await compressor.compress(data)), data, `Go inflates ${compressor.name} from TS`);
      assert.deepEqual(await compressor.decompress(go("deflate", data), LIMIT), data, `TS inflates Go's bytes`);
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("without CompressionStream the default falls back to node:zlib", async () => {
  const g = globalThis as { CompressionStream?: unknown; DecompressionStream?: unknown };
  const saved = [g.CompressionStream, g.DecompressionStream];
  g.CompressionStream = undefined;
  g.DecompressionStream = undefined;
  try {
    assert.throws(() => deflateRawCompressor(), /no CompressionStream/);
    const fallback = defaultCompressor();
    const data = sample(10_000);
    const packed = await fallback.compress(data);
    assert.deepEqual(zlib.inflateRawSync(packed), Buffer.from(data));
    assert.deepEqual(await fallback.decompress(packed, LIMIT), data);
  } finally {
    [g.CompressionStream, g.DecompressionStream] = saved;
  }
});

test("a game-supplied compressor (stand-in for LZ4 / zstd) is what the SDK puts on the wire", async () => {
  const tagged: Compressor = {
    name: "tagged",
    id: 7,
    compress: async (data) => Uint8Array.from([0xee, ...data]),
    decompress: async (data) => data.subarray(1),
  };
  const fake = new FakeRelay({ slot: 1, host: true, hostSlot: 1 });
  const client = new RelayClient(undefined, { game: "party", compressor: tagged, webSocket: () => fake });
  await client.connect(TICKET);
  await client.pushSnapshot(Uint8Array.of(1, 2));
  assert.deepEqual([...decodeSnapshot(fake.controls(RelayOp.Snapshot)[0].payload).data], [7, 0xee, 1, 2], "marker 7, then the game's bytes");
  client.close();
});

// ---- client/ts/README.md“接入自己的 zstd / LZ4”的示例代码：原样放在这里编一遍（SDK 不引第三方库，ZstdCodec 是接口桩）。----

/** 代表游戏自带的 zstd 库（例如某个 zstd 的 wasm 包）；SDK 不带它。 */
interface ZstdCodec {
  compress(data: Uint8Array): Uint8Array;
  /** 解压，至多解出 maxSize 字节（库自己能限就在库里限）。 */
  decompress(data: Uint8Array, maxSize: number): Uint8Array;
}

/** 把它接成 Compressor。id 取 2：0、1 归内置的不压缩与 raw deflate，LZ4 照样接、取 3。 */
function zstdCompressor(codec: ZstdCodec): Compressor {
  return {
    name: "zstd",
    id: 2,
    compress: async (data) => codec.compress(data),
    decompress: async (data, limit) => {
      const out = codec.decompress(data, limit + 1);
      if (out.length > limit) throw new Error(`relay: inflated state exceeds ${limit} bytes`);
      return out;
    },
  };
}

test("the README's zstd example: its id goes on the wire, a client with the same compressor decodes it, a deflate-only client gets UnknownCompressionError", async () => {
  const copy: ZstdCodec = { compress: (data) => data.slice(), decompress: (data, maxSize) => data.slice(0, maxSize) }; // 桩：原样拷贝
  const zstd = zstdCompressor(copy);
  const state = sample(4096);
  const packed = await packState(zstd, state);
  assert.equal(packed[0], 2);
  assert.deepEqual(await unpackState(zstd, packed, LIMIT), state);
  await assert.rejects(unpackState(defaultCompressor(), packed, LIMIT), UnknownCompressionError);
  await assert.rejects(zstd.decompress(state, state.length - 1), /exceeds/);
  // README 的最后一行：房间里每个客户端都配上同一个 compressor
  const client = new RelayClient(undefined, { game: "party", compressor: zstd });
  client.close();
});
