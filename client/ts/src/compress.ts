// 快照与交接数据的压缩（方案 FEATURE-2026-11-16-lobby-relay §4.6.3）：压缩在房主客户端做，relay 不压缩、不解压、
// 不解析。缺省用浏览器自带的 CompressionStream('deflate-raw')，不加依赖；node 里没有它时退到 node:zlib 的 raw deflate，
// 两边字节互通。压缩方式可以换：实现 Compressor（LZ4、zstd 由游戏自带库实现），同一个房间里所有客户端要用同一种。

/** YouAreHost 里状态解压后的上限（16 MiB，与 Go 的 MaxStateBytes 相同）：防解压炸弹。 */
export const MAX_STATE_BYTES = 16 << 20;

/** 压缩方式。同一个房间里的客户端要用同一种：新房主用它解开旧房主的交接数据或 relay 存的快照。 */
export interface Compressor {
  readonly name: string;
  compress(data: Uint8Array): Promise<Uint8Array>;
  /** 解压；结果超过 limit 字节时抛错（不把整个炸弹解出来）。 */
  decompress(data: Uint8Array, limit: number): Promise<Uint8Array>;
}

function checkLimit(size: number, limit: number): void {
  if (size > limit) throw new Error(`relay: inflated state exceeds ${limit} bytes`);
}

/** 不压缩：字节原样（游戏自己已经压过，或快照本来就小）。 */
export const identityCompressor: Compressor = {
  name: "identity",
  compress: async (data) => data,
  decompress: async (data, limit) => {
    checkLimit(data.length, limit);
    return data;
  },
};

/** identityCompressor 的别名（与 Go 的 NoCompression 对应）。 */
export const noCompression = identityCompressor;

interface TransformLike {
  readable: ReadableStream<Uint8Array>;
  writable: WritableStream<Uint8Array>;
}

/** 把 data 写进一个变换流，读出全部结果；写与读并行，大数据不会因背压卡住。结果超过 limit 时中止并抛错。 */
async function pipeThrough(data: Uint8Array, stream: TransformLike, limit = Infinity): Promise<Uint8Array> {
  const writer = stream.writable.getWriter();
  const written = writer.write(data).then(() => writer.close());
  written.catch(() => {}); // 超限中止时写端随之失败，结果以下面的错误为准
  const reader = stream.readable.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    chunks.push(value);
    size += value.length;
    if (size > limit) {
      await reader.cancel().catch(() => {});
      checkLimit(size, limit);
    }
  }
  await written;
  const out = new Uint8Array(size);
  let at = 0;
  for (const chunk of chunks) {
    out.set(chunk, at);
    at += chunk.length;
  }
  return out;
}

type StreamCtor = new (format: "deflate-raw") => TransformLike;

/** 浏览器（以及 node 18+）自带的 CompressionStream('deflate-raw')。环境里没有时抛错。 */
export function deflateRawCompressor(): Compressor {
  const g = globalThis as { CompressionStream?: StreamCtor; DecompressionStream?: StreamCtor };
  const Compress = g.CompressionStream;
  const Decompress = g.DecompressionStream;
  if (!Compress || !Decompress) throw new Error("relay: this environment has no CompressionStream");
  return {
    name: "deflate-raw",
    compress: (data) => pipeThrough(data, new Compress("deflate-raw")),
    decompress: (data, limit) => pipeThrough(data, new Decompress("deflate-raw"), limit),
  };
}

/** node:zlib 里用到的两个函数（注入，SDK 的浏览器代码不 import node 模块）。 */
export interface ZlibLike {
  deflateRawSync(data: Uint8Array): Uint8Array;
  inflateRawSync(data: Uint8Array, options?: { maxOutputLength?: number }): Uint8Array;
}

/** node 的 zlib 做 raw deflate，与 deflateRawCompressor 字节互通：`zlibCompressor(await import("node:zlib"))`。 */
export function zlibCompressor(zlib: ZlibLike): Compressor {
  const plain = (data: Uint8Array) => new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
  return {
    name: "deflate-raw",
    compress: async (data) => plain(zlib.deflateRawSync(data)),
    decompress: async (data, limit) => {
      try {
        const out = plain(zlib.inflateRawSync(data, { maxOutputLength: Math.min(limit + 1, 2 ** 31 - 1) }));
        checkLimit(out.length, limit);
        return out;
      } catch (error) {
        if (error instanceof RangeError) checkLimit(limit + 1, limit); // 超过 maxOutputLength
        throw error;
      }
    },
  };
}

/**
 * 缺省压缩：有 CompressionStream 就用它；没有时（老的 node）第一次用到才动态加载 node:zlib。两者都是 raw deflate。
 */
export function defaultCompressor(): Compressor {
  const g = globalThis as { CompressionStream?: unknown; DecompressionStream?: unknown };
  if (g.CompressionStream && g.DecompressionStream) return deflateRawCompressor();
  let loaded: Promise<Compressor> | undefined;
  const zlib = () => {
    const specifier = "node:zlib"; // 变量形式：打包器不去解析它，浏览器构建里不出现 node 模块
    loaded ??= (import(specifier) as Promise<ZlibLike>).then(zlibCompressor);
    return loaded;
  };
  return {
    name: "deflate-raw",
    compress: async (data) => (await zlib()).compress(data),
    decompress: async (data, limit) => (await zlib()).decompress(data, limit),
  };
}
