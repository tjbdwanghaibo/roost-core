// 快照与交接数据的压缩（方案 FEATURE-2026-11-16-lobby-relay §4.6.3）：压缩在房主客户端做，relay 不压缩、不解压、
// 不解析。缺省用浏览器自带的 CompressionStream('deflate-raw')，不加依赖；node 里没有它时退到 node:zlib 的 raw deflate，
// 两边字节互通。压缩方式可以换：实现 Compressor（LZ4、zstd 由游戏自带库实现，接法见 client/ts/README.md）。
//
// 压缩方式标识（三份 SDK 之间的约定，docs/framework/RELAY.md §2.3）：非空的状态字节在线上是 [标识 1B][压缩后的字节]
// （packState），收到时按标识选解压方式（unpackState）：自己的 Compressor，或内置的不压缩（0）、raw deflate（1）。
// 标识不认识抛 UnknownCompressionError，房间里混用了游戏自带的压缩方式时据此给出明确的错误。

/** YouAreHost 里状态解压后的上限（16 MiB，与 Go 的 MaxStateBytes 相同）：防解压炸弹。 */
export const MAX_STATE_BYTES = 16 << 20;

/**
 * 压缩方式。id 是 1 字节的压缩方式标识：0 与 1 归内置的不压缩与 raw deflate，游戏自带的用 2～255（README 的示例里 zstd
 * 用 2、LZ4 用 3）。内置的两种谁都解得开；游戏自带的压缩方式，房间里每个客户端都要配上。
 */
export interface Compressor {
  readonly name: string;
  readonly id: number;
  compress(data: Uint8Array): Promise<Uint8Array>;
  /** 解压；结果超过 limit 字节时抛错（不把整个炸弹解出来）。 */
  decompress(data: Uint8Array, limit: number): Promise<Uint8Array>;
}

function checkLimit(size: number, limit: number): void {
  if (size > limit) throw new Error(`relay: inflated state exceeds ${limit} bytes`);
}

/** 内置压缩方式的标识。 */
export const COMPRESSION_NONE = 0;
export const COMPRESSION_DEFLATE_RAW = 1;

/** 不压缩：字节原样（游戏自己已经压过，或快照本来就小）。 */
export const identityCompressor: Compressor = {
  name: "identity",
  id: COMPRESSION_NONE,
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
    id: COMPRESSION_DEFLATE_RAW,
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
    id: COMPRESSION_DEFLATE_RAW,
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
    id: COMPRESSION_DEFLATE_RAW,
    compress: async (data) => (await zlib()).compress(data),
    decompress: async (data, limit) => (await zlib()).decompress(data, limit),
  };
}

/** 收到的状态字节的压缩方式标识不认识（既不是自己的 Compressor.id，也不是内置的两种）。 */
export class UnknownCompressionError extends Error {
  constructor(readonly method: number, own: number) {
    super(`relay: state uses an unknown compression method ${method} (this client compresses with ${own})`);
    this.name = "UnknownCompressionError";
  }
}

/** 把状态打成线上的字节：[压缩方式标识 1B][压缩后的字节]；空状态就是空的（不带标识）。 */
export async function packState(compressor: Compressor, state: Uint8Array): Promise<Uint8Array> {
  if (state.length === 0) return new Uint8Array(0);
  const packed = await compressor.compress(state);
  const out = new Uint8Array(1 + packed.length);
  out[0] = compressor.id;
  out.set(packed, 1);
  return out;
}

/**
 * packState 的反面：按标识选解压方式（自己的 compressor 优先，再是内置的两种），解出的字节不超过 limit。标识不认识抛
 * UnknownCompressionError；标识认识、内容解不开抛解压错误。空的就是空状态。
 */
export async function unpackState(compressor: Compressor, data: Uint8Array, limit: number): Promise<Uint8Array> {
  if (data.length === 0) return data;
  const id = data[0];
  let method: Compressor;
  if (id === compressor.id) method = compressor;
  else if (id === COMPRESSION_DEFLATE_RAW) method = builtinDeflate();
  else if (id === COMPRESSION_NONE) method = identityCompressor;
  else throw new UnknownCompressionError(id, compressor.id);
  return method.decompress(data.subarray(1), limit);
}

let sharedDeflate: Compressor | undefined;
function builtinDeflate(): Compressor {
  sharedDeflate ??= defaultCompressor();
  return sharedDeflate;
}
