using System;
using System.Buffers.Binary;
using System.IO;
using System.IO.Compression;
using System.Security.Cryptography;

namespace Roost.Client.Terrain
{
    // 一块的原始数据，形状与 TerrainChunk DAO 相同：Heights、Materials 各 4096 个元素，行优先（下标 = j·64 + i）。
    // TerrainPack.ReadChunk 每次返回新分配的数组，归调用方。
    public sealed class TerrainChunk
    {
        public ushort[] Heights { get; }
        public byte[] Materials { get; }
        public TerrainChunk(ushort[] heights, byte[] materials) { Heights = heights; Materials = materials; }
    }

    // 打开的原始地形包（格式版本 2，与 Go gameplay/terrain/pack.go 头注释的逐字节布局相同）：
    //
    //   偏移 0    包头 128 字节（magic "RTERRAIN"、格式版本、BaseRevision、块数、几何常量、HeightBase、WaterLevel、
    //             codec、索引偏移与项长、区域水位个数（偏移 68）、metaSHA256 = SHA-256(包头前 96 字节 ‖ 索引表 ‖ 区域表)）
    //   偏移 128  索引表，每块 44 字节：offset uint64、length uint32、SHA-256（块存储字节）
    //   之后      区域表，每项 20 字节：minX、minY、maxX、maxY（uint32 块坐标，半开）、level（int32）
    //   之后      每块独立的 raw DEFLATE 数据（明文 = 4096 个平面预测差分 zigzag uvarint 高度 ‖ 4096 字节材质）
    //
    // 版本 1 的包（没有区域水位）按 TerrainPackVersionException 拒绝，用新工具重新生成。
    // 打开时只读入并校验包头与索引表（1w 地图约 1MB），块数据按需 ReadChunk，每次读块校验该块的 SHA-256。
    // 来源可以是随机读的 Stream（文件、StreamingAssets 的 FileStream）或整份字节（Addressables / UnityWebRequest
    // 读到的 byte[]）。
    //
    // 并发：打开后只读。ReadChunk 可以被多个线程同时调用：字节来源无锁；Stream 来源的定位 + 读取在锁内串行。
    // Dispose 之后 ReadChunk 抛 ObjectDisposedException。
    public sealed class TerrainPack : IDisposable
    {
        private const string Magic = "RTERRAIN";
        private const uint FormatVersion = 2;
        private const int HeaderSize = 128;
        private const int IndexEntrySize = 44;
        private const int RegionEntrySize = 20;
        private const int MetaHashOffset = 96;
        private const uint CodecPlaneDeltaFlate = 1;
        // 单块存储字节的上限：明文最多 16KB，DEFLATE 对不可压数据只多很少的块头；超过即视为损坏（与 Go 相同）。
        private const int MaxStoredChunk = 64 << 10;
        // 明文上限：每个高度差 zigzag 后小于 2^18，uvarint 最多 3 字节；加 4096 字节材质。
        private const int MaxPlainChunk = 3 * TerrainGeometry.ChunkPoints + TerrainGeometry.ChunkPoints;

        private struct IndexEntry
        {
            public long Offset;
            public int Length;
            public byte[] Sum;
        }

        private readonly Source source;
        private readonly IndexEntry[] index;
        private int disposed;

        public TerrainMeta Meta { get; }

        private TerrainPack(Source source, TerrainMeta meta, IndexEntry[] index)
        { this.source = source; Meta = meta; this.index = index; }

        // 从整份包字节打开。字节不复制，打开后调用方不要再修改它们（改了会在读块时表现为 SHA-256 不符）。
        public static TerrainPack Open(ReadOnlyMemory<byte> data) => Open(new MemorySource(data));

        // 从随机读的 Stream 打开（CanRead、CanSeek，Length 可用）。leaveOpen 为 false 时 Dispose 一并关闭 stream。
        // 打开失败时按 leaveOpen 的约定处理 stream：leaveOpen 为 false 也会关闭它。
        public static TerrainPack Open(Stream stream, bool leaveOpen = false)
        {
            if (stream == null) throw new ArgumentNullException(nameof(stream));
            if (!stream.CanRead || !stream.CanSeek)
            {
                if (!leaveOpen) stream.Dispose();
                throw new ArgumentException("terrain pack stream must be readable and seekable", nameof(stream));
            }
            return Open(new StreamSource(stream, leaveOpen));
        }

        private static TerrainPack Open(Source source)
        {
            try
            {
                long size = source.Length;
                var head = new byte[HeaderSize];
                if (size < HeaderSize) throw new TerrainBadPackException($"read header: {size} bytes, want at least {HeaderSize}");
                source.Read(0, head);
                var meta = ParseHeader(head, out int regions);
                long indexEnd = HeaderSize + (long)meta.Chunks * IndexEntrySize;
                long dataStart = indexEnd + (long)regions * RegionEntrySize;
                if (dataStart > size)
                    throw new TerrainBadPackException($"file of {size} bytes is shorter than its index and water region tables ({dataStart} bytes)");
                var table = new byte[dataStart - HeaderSize];
                source.Read(HeaderSize, table);
                if (!MetaSum(head, table).AsSpan().SequenceEqual(head.AsSpan(MetaHashOffset, 32)))
                    throw new TerrainBadPackException("header / index checksum mismatch");
                if (regions > 0) meta = WithRegions(meta, table.AsSpan((int)(indexEnd - HeaderSize)), regions);
                var entries = new IndexEntry[meta.Chunks];
                for (int i = 0; i < entries.Length; i++)
                {
                    var e = table.AsSpan(i * IndexEntrySize, IndexEntrySize);
                    ulong offset = BinaryPrimitives.ReadUInt64LittleEndian(e);
                    uint length = BinaryPrimitives.ReadUInt32LittleEndian(e.Slice(8));
                    if (length == 0 || length > MaxStoredChunk || offset < (ulong)dataStart ||
                        offset > (ulong)size || length > (ulong)size - offset)
                        throw new TerrainBadPackException($"index entry {i} (offset {offset}, length {length}) outside the data area [{dataStart}, {size})");
                    entries[i] = new IndexEntry { Offset = (long)offset, Length = (int)length, Sum = e.Slice(12, 32).ToArray() };
                }
                return new TerrainPack(source, meta, entries);
            }
            catch (EndOfStreamException error)
            {
                source.Dispose();
                throw new TerrainBadPackException("truncated: " + error.Message);
            }
            catch
            {
                source.Dispose();
                throw;
            }
        }

        // 读区域表并按区域规则整体校验（与 Go openPack 相同：校验和通过之后），不合法的区域报坏包。
        private static TerrainMeta WithRegions(TerrainMeta meta, ReadOnlySpan<byte> table, int n)
        {
            var regions = new TerrainWaterRegion[n];
            for (int i = 0; i < n; i++)
            {
                var e = table.Slice(i * RegionEntrySize, RegionEntrySize);
                regions[i] = new TerrainWaterRegion(
                    BinaryPrimitives.ReadInt32LittleEndian(e), BinaryPrimitives.ReadInt32LittleEndian(e.Slice(4)),
                    BinaryPrimitives.ReadInt32LittleEndian(e.Slice(8)), BinaryPrimitives.ReadInt32LittleEndian(e.Slice(12)),
                    BinaryPrimitives.ReadInt32LittleEndian(e.Slice(16)));
            }
            try { return new TerrainMeta(meta.BaseRevision, meta.ChunksX, meta.ChunksY, meta.HeightBase, meta.WaterLevel, regions); }
            catch (ArgumentException error) { throw new TerrainBadPackException(error.Message); }
        }

        // 解析并校验包头的定长字段，检查顺序与 Go parseHeader 相同（决定同一个坏包报哪一类错误）。regions 是区域水位个数，
        // 区域本身在校验和通过之后读。
        private static TerrainMeta ParseHeader(byte[] head, out int regions)
        {
            var h = head.AsSpan();
            for (int i = 0; i < Magic.Length; i++)
                if (h[i] != (byte)Magic[i]) throw new TerrainBadPackException("magic " + BitConverter.ToString(head, 0, 8));
            uint version = BinaryPrimitives.ReadUInt32LittleEndian(h.Slice(8));
            if (version != FormatVersion) throw new TerrainPackVersionException($"format version {version}, this build reads {FormatVersion}");
            uint headerSize = BinaryPrimitives.ReadUInt32LittleEndian(h.Slice(12));
            if (headerSize != HeaderSize) throw new TerrainBadPackException($"header size {headerSize}");
            uint samples = BinaryPrimitives.ReadUInt32LittleEndian(h.Slice(32)), spacing = BinaryPrimitives.ReadUInt32LittleEndian(h.Slice(36));
            uint step = BinaryPrimitives.ReadUInt32LittleEndian(h.Slice(40)), codec = BinaryPrimitives.ReadUInt32LittleEndian(h.Slice(52));
            if (samples != TerrainGeometry.ChunkSamples || spacing != TerrainGeometry.SampleSpacing || step != TerrainGeometry.HeightStep || codec != CodecPlaneDeltaFlate)
                throw new TerrainPackVersionException($"chunk samples {samples}, spacing {spacing}, height step {step}, codec {codec}; this build uses " +
                    $"{TerrainGeometry.ChunkSamples}, {TerrainGeometry.SampleSpacing}, {TerrainGeometry.HeightStep}, {CodecPlaneDeltaFlate}");
            ulong indexOffset = BinaryPrimitives.ReadUInt64LittleEndian(h.Slice(56));
            uint entry = BinaryPrimitives.ReadUInt32LittleEndian(h.Slice(64));
            if (indexOffset != HeaderSize || entry != IndexEntrySize) throw new TerrainBadPackException($"index at {indexOffset} with {entry}-byte entries");
            uint cx = BinaryPrimitives.ReadUInt32LittleEndian(h.Slice(24)), cy = BinaryPrimitives.ReadUInt32LittleEndian(h.Slice(28));
            if (cx > int.MaxValue || cy > int.MaxValue) throw new TerrainBadPackException($"{cx} × {cy} chunks");
            uint regionCount = BinaryPrimitives.ReadUInt32LittleEndian(h.Slice(68));
            if (regionCount > TerrainMeta.MaxWaterRegions) throw new TerrainBadPackException($"{regionCount} water regions, at most {TerrainMeta.MaxWaterRegions}");
            regions = (int)regionCount;
            long baseRevision = BinaryPrimitives.ReadInt64LittleEndian(h.Slice(16));
            int heightBase = BinaryPrimitives.ReadInt32LittleEndian(h.Slice(44)), waterLevel = BinaryPrimitives.ReadInt32LittleEndian(h.Slice(48));
            if (cx < 1 || cy < 1 || cx > TerrainGeometry.MaxChunksPerSide || cy > TerrainGeometry.MaxChunksPerSide)
                throw new TerrainBadPackException($"{cx} × {cy} chunks, each side must be in [1, {TerrainGeometry.MaxChunksPerSide}]");
            if ((long)heightBase + ushort.MaxValue * (long)TerrainGeometry.HeightStep > int.MaxValue)
                throw new TerrainBadPackException($"height base {heightBase} overflows int32 at the highest raw height");
            return new TerrainMeta(baseRevision, (int)cx, (int)cy, heightBase, waterLevel);
        }

        private static byte[] MetaSum(byte[] head, byte[] table)
        {
            using (var sha = SHA256.Create())
            {
                sha.TransformBlock(head, 0, MetaHashOffset, null, 0);
                sha.TransformFinalBlock(table, 0, table.Length);
                return sha.Hash;
            }
        }

        // 块存储字节的 SHA-256（索引表里的值，不读块数据），客户端可以用它核对自己的资源。返回副本。
        public byte[] ChunkSha256(ChunkKey key)
        {
            if (!Meta.Contains(key)) throw new TerrainOutOfMapException("chunk " + key);
            return (byte[])index[Meta.IndexOf(key)].Sum.Clone();
        }

        // 读出一块：取存储字节、校验 SHA-256、解码。返回新分配的数组，归调用方。
        // 失败：块不在地图内抛 TerrainOutOfMapException；截断、校验和不符、解码失败抛 TerrainCorruptChunkException（带块坐标，
        // 其他块照常可读）；来源的 I/O 错误原样抛出；Dispose 之后抛 ObjectDisposedException。不会返回默认数据。
        // 一次调用约读 3KB 加一次解压，属于 I/O：不要在每帧的主线程热路径里成批调用。
        public TerrainChunk ReadChunk(ChunkKey key)
        {
            if (System.Threading.Volatile.Read(ref disposed) != 0) throw new ObjectDisposedException(nameof(TerrainPack));
            if (!Meta.Contains(key)) throw new TerrainOutOfMapException("chunk " + key);
            var e = index[Meta.IndexOf(key)];
            var stored = new byte[e.Length];
            try { source.Read(e.Offset, stored); }
            catch (EndOfStreamException) { throw new TerrainCorruptChunkException(key, "short read"); }
            using (var sha = SHA256.Create())
                if (!sha.ComputeHash(stored).AsSpan().SequenceEqual(e.Sum)) throw new TerrainCorruptChunkException(key, "SHA-256 mismatch");
            try { return DecodeChunk(stored); }
            catch (InvalidDataException error) { throw new TerrainCorruptChunkException(key, error.Message, error); }
        }

        // codec 1 的解码，与 Go decodeChunk 相同：明文必须恰好是 4096 个合法高度加 4096 字节材质，多一字节、少一字节、
        // 高度越出 uint16 都算损坏。DEFLATE 是 raw（RFC 1951，无 zlib / gzip 头），即 .NET 的 DeflateStream。
        internal static TerrainChunk DecodeChunk(byte[] stored)
        {
            var plain = new byte[MaxPlainChunk + 1];
            int n = 0;
            using (var inflate = new DeflateStream(new MemoryStream(stored, false), CompressionMode.Decompress))
            {
                while (n < plain.Length)
                {
                    int read = inflate.Read(plain, n, plain.Length - n);
                    if (read == 0) break;
                    n += read;
                }
            }
            if (n > MaxPlainChunk) throw new InvalidDataException($"plain text longer than {MaxPlainChunk} bytes");
            var heights = new ushort[TerrainGeometry.ChunkPoints];
            int pos = 0;
            for (int k = 0; k < heights.Length; k++)
            {
                int used = Uvarint(plain, pos, n, out ulong u);
                if (used <= 0) throw new InvalidDataException($"height {k}: bad varint");
                pos += used;
                // 与 Go 相同的 64 位回绕语义：C# 默认 unchecked。
                long v = Predict(heights, k) + ((long)(u >> 1) ^ -(long)(u & 1));
                if (v < 0 || v > ushort.MaxValue) throw new InvalidDataException($"height {k} out of range: {v}");
                heights[k] = (ushort)v;
            }
            if (n - pos != TerrainGeometry.ChunkPoints)
                throw new InvalidDataException($"{n - pos} material bytes, want {TerrainGeometry.ChunkPoints}");
            var materials = new byte[TerrainGeometry.ChunkPoints];
            Buffer.BlockCopy(plain, pos, materials, 0, materials.Length);
            return new TerrainChunk(heights, materials);
        }

        // 下标 k 的平面预测值（左邻 + 下邻 − 左下邻；第一行只用左邻，行首只用下邻，第一个点为 0），只读 k 之前已解出的点。
        private static long Predict(ushort[] h, int k)
        {
            int i = k % TerrainGeometry.ChunkSamples, j = k / TerrainGeometry.ChunkSamples;
            if (j == 0 && i == 0) return 0;
            if (j == 0) return h[k - 1];
            if (i == 0) return h[k - TerrainGeometry.ChunkSamples];
            return (long)h[k - 1] + h[k - TerrainGeometry.ChunkSamples] - h[k - TerrainGeometry.ChunkSamples - 1];
        }

        // 与 Go encoding/binary.Uvarint 相同：返回用掉的字节数；0 表示数据不够，负数表示超过 64 位。
        private static int Uvarint(byte[] buf, int start, int end, out ulong value)
        {
            ulong x = 0;
            int s = 0;
            value = 0;
            for (int i = 0; start + i < end; i++)
            {
                if (i == 10) return -(i + 1);
                byte b = buf[start + i];
                if (b < 0x80)
                {
                    if (i == 9 && b > 1) return -(i + 1);
                    value = x | (ulong)b << s;
                    return i + 1;
                }
                x |= (ulong)(b & 0x7f) << s;
                s += 7;
            }
            return 0;
        }

        public void Dispose()
        {
            if (System.Threading.Interlocked.Exchange(ref disposed, 1) == 0) source.Dispose();
        }

        // 随机读的字节来源。Read 必须读满，否则抛 EndOfStreamException。
        private abstract class Source : IDisposable
        {
            public abstract long Length { get; }
            public abstract void Read(long offset, Span<byte> destination);
            public virtual void Dispose() { }
        }

        private sealed class MemorySource : Source
        {
            private readonly ReadOnlyMemory<byte> data;
            public MemorySource(ReadOnlyMemory<byte> data) { this.data = data; }
            public override long Length => data.Length;
            public override void Read(long offset, Span<byte> destination)
            {
                if (offset < 0 || offset > data.Length || destination.Length > data.Length - offset)
                    throw new EndOfStreamException($"read of {destination.Length} bytes at {offset} past {data.Length}");
                data.Span.Slice((int)offset, destination.Length).CopyTo(destination);
            }
        }

        private sealed class StreamSource : Source
        {
            private readonly Stream stream;
            private readonly bool leaveOpen;
            public StreamSource(Stream stream, bool leaveOpen) { this.stream = stream; this.leaveOpen = leaveOpen; }
            public override long Length => stream.Length;
            public override void Read(long offset, Span<byte> destination)
            {
                // Stream 的位置是共享状态：定位与读取一起在锁内完成，多个线程同时 ReadChunk 不会互相打乱。
                lock (stream)
                {
                    stream.Position = offset;
                    while (destination.Length > 0)
                    {
                        int read = stream.Read(destination);
                        if (read == 0) throw new EndOfStreamException($"stream ended at {stream.Position}");
                        destination = destination.Slice(read);
                    }
                }
            }
            public override void Dispose() { if (!leaveOpen) stream.Dispose(); }
        }
    }
}
