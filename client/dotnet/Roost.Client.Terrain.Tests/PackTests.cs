using System;
using System.Buffers.Binary;
using System.Collections.Generic;
using System.IO;
using System.IO.Compression;
using System.Security.Cryptography;
using System.Text.Json;
using Roost.Client.Terrain;

internal static partial class Program
{
    private const int HeaderSize = 128, EntrySize = 44, MetaHashOffset = 96;

    // Go 写出的金样：整包 SHA-256、包头参数、6 块的存储 / 解码校验和，以及 246 个位置的高度 / 材质 / 水。
    private static void TestGolden(JsonElement golden, byte[] packBytes, string packPath)
    {
        Check(Sha(packBytes) == golden.GetProperty("pack_sha256").GetString(), "golden pack file SHA-256");
        var meta = golden.GetProperty("meta");
        using var fromBytes = TerrainPack.Open(packBytes);
        using var fromStream = TerrainPack.Open(new FileStream(packPath, FileMode.Open, FileAccess.Read, FileShare.Read));
        foreach (var pack in new[] { fromBytes, fromStream })
        {
            var m = pack.Meta;
            Check(m.BaseRevision == meta.GetProperty("base_revision").GetInt64() && m.ChunksX == meta.GetProperty("chunks_x").GetInt32() &&
                  m.ChunksY == meta.GetProperty("chunks_y").GetInt32() && m.HeightBase == meta.GetProperty("height_base").GetInt32() &&
                  m.WaterLevel == meta.GetProperty("water_level").GetInt32(), "golden meta");
            Check(meta.GetProperty("chunk_samples").GetInt32() == TerrainGeometry.ChunkSamples &&
                  meta.GetProperty("sample_spacing").GetInt32() == TerrainGeometry.SampleSpacing &&
                  meta.GetProperty("height_step").GetInt32() == TerrainGeometry.HeightStep, "golden geometry constants");
            int chunks = 0;
            foreach (var c in golden.GetProperty("chunks").EnumerateArray())
            {
                var key = new ChunkKey(c.GetProperty("x").GetInt32(), c.GetProperty("y").GetInt32());
                Check(Hex(pack.ChunkSha256(key)) == c.GetProperty("stored_sha256").GetString(), "stored SHA-256 " + key);
                var chunk = pack.ReadChunk(key);
                var le = new byte[chunk.Heights.Length * 2];
                for (int i = 0; i < chunk.Heights.Length; i++) BinaryPrimitives.WriteUInt16LittleEndian(le.AsSpan(2 * i), chunk.Heights[i]);
                Check(Sha(le) == c.GetProperty("heights_sha256").GetString(), "decoded heights SHA-256 " + key);
                Check(Sha(chunk.Materials) == c.GetProperty("materials_sha256").GetString(), "decoded materials SHA-256 " + key);
                chunks++;
            }
            Check(chunks == 6, "golden has 6 chunks");

            var view = LoadAll(pack);
            int queries = 0, outside = 0;
            foreach (var q in golden.GetProperty("queries").EnumerateArray())
            {
                long x = q.GetProperty("x").GetInt64(), y = q.GetProperty("y").GetInt64();
                string where = $"query ({x}, {y})";
                if (q.TryGetProperty("error", out var error))
                {
                    Check(error.GetString() == "out_of_map", where + " error kind");
                    Check(view.TryProbe(x, y, out _, out _) == TerrainQueryStatus.OutOfMap, where + " TryProbe out of map");
                    Throws<TerrainOutOfMapException>(() => view.Height(x, y), where + " Height out of map");
                    Throws<TerrainOutOfMapException>(() => view.Material(x, y), where + " Material out of map");
                    Throws<TerrainOutOfMapException>(() => view.IsWater(x, y), where + " IsWater out of map");
                    outside++;
                }
                else
                {
                    int height = q.GetProperty("height").GetInt32();
                    byte material = (byte)q.GetProperty("material").GetInt32();
                    bool water = q.GetProperty("water").GetBoolean();
                    var g = view.Probe(x, y);
                    Check(g.Height == height && g.Material == material && g.Water == water,
                        $"{where}: got {g.Height}/{g.Material}/{g.Water}, Go {height}/{material}/{water}");
                    Check(view.Height(x, y) == height && view.Material(x, y) == material && view.IsWater(x, y) == water, where + " single queries");
                    Check(view.TryProbe(x, y, out var t, out _) == TerrainQueryStatus.Ok && t.Height == height, where + " TryProbe");
                }
                queries++;
            }
            Check(queries == 246 && outside > 0, $"golden queries: {queries}");
        }
        Console.WriteLine("PASS terrain golden: pack SHA-256, 6 chunks decoded, 246 queries equal Go (bytes and Stream)");
    }

    private static TerrainView LoadAll(TerrainPack pack)
    {
        var view = new TerrainView(pack);
        for (int y = 0; y < pack.Meta.ChunksY; y++)
            for (int x = 0; x < pack.Meta.ChunksX; x++)
                view.LoadBase(new ChunkKey(x, y));
        Check(view.Count == pack.Meta.Chunks, "all chunks loaded");
        return view;
    }

    // 邻点规则：权重为 0 的邻点不读、地图边缘复制、邻块没装报缺的那一块（与 Go View.probe 的读取次序相同）。
    private static void TestNeighbourRules(byte[] packBytes)
    {
        using var pack = TerrainPack.Open(packBytes);
        var full = LoadAll(pack);
        var view = new TerrainView(pack);
        view.LoadBase(new ChunkKey(0, 0));
        Check(view.Height(6300, 6300) == full.Height(6300, 6300), "sample point needs only its own chunk");
        Check(view.Height(6299, 6299) == full.Height(6299, 6299), "inside the chunk, both neighbours in the chunk");
        Check(Throws<TerrainNotLoadedException>(() => view.Height(6350, 0), "right neighbour").Key == new ChunkKey(1, 0), "missing right chunk");
        Check(Throws<TerrainNotLoadedException>(() => view.Height(0, 6350), "top neighbour").Key == new ChunkKey(0, 1), "missing top chunk");
        Check(Throws<TerrainNotLoadedException>(() => view.Height(6350, 6350), "corner").Key == new ChunkKey(1, 0), "corner reports the right chunk first");
        view.LoadBase(new ChunkKey(1, 0));
        view.LoadBase(new ChunkKey(0, 1));
        Check(view.TryProbe(6350, 6350, out _, out var missing) == TerrainQueryStatus.NotLoaded && missing == new ChunkKey(1, 1), "corner needs the top-right chunk");
        Check(Throws<TerrainNotLoadedException>(() => view.Material(12800, 0), "material of an unloaded chunk").Key == new ChunkKey(2, 0), "material chunk");

        var edge = new TerrainView(pack);
        edge.LoadBase(new ChunkKey(2, 1));
        foreach (var (x, y) in new[] { (19199L, 12799L), (19150L, 12000L), (13000L, 12799L) })
            Check(edge.Height(x, y) == full.Height(x, y) && edge.Probe(x, y).Material == full.Probe(x, y).Material, $"map edge ({x}, {y}) replicates without neighbours");
        Check(edge.Height(19199, 12799) == pack.Meta.WorldHeight(full.GetChunk(new ChunkKey(2, 1))!.Heights[63 * 64 + 63]), "corner sample replicated");
        Throws<TerrainOutOfMapException>(() => edge.Height(19200, 0), "x at the map width");
        Throws<TerrainOutOfMapException>(() => edge.Height(0, -1), "negative y");
        Throws<TerrainOutOfMapException>(() => edge.LoadBase(new ChunkKey(3, 0)), "LoadBase outside");
        Throws<TerrainOutOfMapException>(() => pack.ReadChunk(new ChunkKey(-1, 0)), "ReadChunk outside");

        var key = TerrainGeometry.PointIndex(-1, 6400, out int index);
        Check(key == new ChunkKey(-1, 1) && index == 63, "PointIndex floors negative coordinates");
        Check(TerrainGeometry.ChunkOf(long.MinValue, long.MaxValue) == new ChunkKey(int.MinValue, int.MaxValue), "ChunkOf saturates");
        Check(ChunkKey.FromId(new ChunkKey(-3, 7).Id) == new ChunkKey(-3, 7) && new ChunkKey(1, 2).Id == (1UL << 32 | 2), "ChunkKey id round trip");
        Console.WriteLine("PASS terrain neighbour rules: zero weight, map edge, not loaded, out of map");
    }

    // 包头 / 索引表损坏：类型与 Go 的 ErrBadPack / ErrPackVersion 对应，检查顺序与 Go parseHeader 相同。
    private static void TestBadPacks(byte[] good)
    {
        void Bad<T>(string why, Action<byte[]> edit, bool reseal = false) where T : Exception
        {
            var data = (byte[])good.Clone();
            edit(data);
            if (reseal) Reseal(data);
            Throws<T>(() => TerrainPack.Open(data).Dispose(), why + " (bytes)");
            Throws<T>(() => TerrainPack.Open(new MemoryStream(data)).Dispose(), why + " (stream)");
        }
        Bad<TerrainBadPackException>("magic", d => d[0] ^= 1);
        Bad<TerrainPackVersionException>("format version", d => BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(8), 2));
        Bad<TerrainBadPackException>("header size", d => BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(12), 64));
        Bad<TerrainPackVersionException>("chunk samples", d => BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(32), 32));
        Bad<TerrainPackVersionException>("sample spacing", d => BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(36), 50));
        Bad<TerrainPackVersionException>("height step", d => BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(40), 1));
        Bad<TerrainPackVersionException>("codec", d => BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(52), 2));
        Bad<TerrainBadPackException>("index offset", d => BinaryPrimitives.WriteUInt64LittleEndian(d.AsSpan(56), 0));
        Bad<TerrainBadPackException>("index entry size", d => BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(64), 40));
        Bad<TerrainBadPackException>("zero chunks", d => BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(24), 0));
        Bad<TerrainBadPackException>("too many chunks", d => BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(28), 1025));
        Bad<TerrainBadPackException>("chunk count above int32", d => BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(24), 0x80000000));
        Bad<TerrainBadPackException>("height base overflow", d => BinaryPrimitives.WriteInt32LittleEndian(d.AsSpan(44), int.MaxValue));
        Bad<TerrainBadPackException>("reserved byte (covered by the checksum)", d => d[70] = 1);
        Bad<TerrainBadPackException>("BaseRevision changed without the checksum", d => d[16] ^= 1);
        Bad<TerrainBadPackException>("index byte", d => d[HeaderSize + 20] ^= 1);
        Bad<TerrainBadPackException>("meta checksum byte", d => d[MetaHashOffset] ^= 1);
        Bad<TerrainBadPackException>("zero-length chunk", d => BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(HeaderSize + 8), 0), reseal: true);
        Bad<TerrainBadPackException>("chunk above 64KB", d => BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(HeaderSize + 8), 65537), reseal: true);
        Bad<TerrainBadPackException>("chunk inside the index", d => BinaryPrimitives.WriteUInt64LittleEndian(d.AsSpan(HeaderSize), HeaderSize), reseal: true);
        Bad<TerrainBadPackException>("chunk past the end", d => BinaryPrimitives.WriteUInt64LittleEndian(d.AsSpan(HeaderSize), (ulong)d.Length - 10), reseal: true);
        foreach (int length in new[] { 0, 100, HeaderSize, HeaderSize + 50, HeaderSize + 6 * EntrySize, good.Length - 1 })
        {
            var cut = good.AsSpan(0, length).ToArray();
            Throws<TerrainBadPackException>(() => TerrainPack.Open(cut).Dispose(), $"truncated to {length} (bytes)");
            Throws<TerrainBadPackException>(() => TerrainPack.Open(new MemoryStream(cut)).Dispose(), $"truncated to {length} (stream)");
        }
        // 重新封口后包头仍须通过，确认上面的 reseal 用例确实走到了索引检查而不是校验和。
        var resealed = (byte[])good.Clone();
        Reseal(resealed);
        TerrainPack.Open(resealed).Dispose();
        // 打开失败时按 leaveOpen 处理 stream：false 关闭，true 保留。
        var closed = new MemoryStream(new byte[HeaderSize]);
        Throws<TerrainBadPackException>(() => TerrainPack.Open(closed, leaveOpen: false), "zero header (leaveOpen false)");
        Check(!closed.CanRead, "failed open closes the stream when leaveOpen is false");
        var kept = new MemoryStream(new byte[HeaderSize]);
        Throws<TerrainBadPackException>(() => TerrainPack.Open(kept, leaveOpen: true), "zero header (leaveOpen true)");
        Check(kept.CanRead, "failed open keeps the stream when leaveOpen is true");
        Throws<ArgumentException>(() => TerrainPack.Open(new NonSeekable(good)), "non-seekable stream");
        Console.WriteLine("PASS terrain bad pack: magic, version, geometry, sizes, checksum, index range, truncation");
    }

    // 单块损坏：只影响这一块；SHA-256 先于解压，解码失败（校验和对得上但内容非法）同样报坏块。
    private static void TestCorruptChunks(byte[] good)
    {
        var flipped = (byte[])good.Clone();
        var (offset, length) = Entry(flipped, 4); // 块 (1,1)
        flipped[offset + length / 2] ^= 0x10;
        using (var pack = TerrainPack.Open(flipped))
        {
            Check(Throws<TerrainCorruptChunkException>(() => pack.ReadChunk(new ChunkKey(1, 1)), "flipped bit").Key == new ChunkKey(1, 1), "corrupt chunk key");
            var view = new TerrainView(pack);
            Throws<TerrainCorruptChunkException>(() => view.LoadBase(new ChunkKey(1, 1)), "LoadBase of a corrupt chunk");
            Check(view.Count == 0 && !view.TryGetVersion(new ChunkKey(1, 1), out _), "nothing installed for a corrupt chunk");
            view.LoadBase(new ChunkKey(0, 0));
            pack.ReadChunk(new ChunkKey(2, 1));
            Check(view.Count == 1, "other chunks still readable");
        }
        // Stream 声称的长度比能读到的长：打开成功，读最后一块时截断 → 坏块。
        using (var pack = TerrainPack.Open(new ShortStream(good, good.Length - 100)))
        {
            Throws<TerrainCorruptChunkException>(() => pack.ReadChunk(new ChunkKey(2, 1)), "short read");
            pack.ReadChunk(new ChunkKey(0, 0));
        }

        var plain = Plain(Ramp(), Materials(3));
        Check(DecodeOne(Deflate(plain)).Heights[4095] == Ramp()[4095], "test encoder round trip");
        var extremes = new ushort[TerrainGeometry.ChunkPoints];
        for (int i = 0; i < extremes.Length; i++) extremes[i] = (i + i / 64) % 2 == 0 ? (ushort)0 : ushort.MaxValue;
        Check(DecodeOne(Deflate(Plain(extremes, Materials(1)))).Heights.AsSpan().SequenceEqual(extremes), "0 / 65535 checkerboard round trip");
        void Corrupt(string why, byte[] stored)
        {
            using var pack = TerrainPack.Open(BuildPack(1, 1, 1, 0, 0, new[] { stored }));
            Check(Throws<TerrainCorruptChunkException>(() => pack.ReadChunk(new ChunkKey(0, 0)), why).Key == new ChunkKey(0, 0), why + " key");
        }
        Corrupt("one material byte short", Deflate(plain.AsSpan(0, plain.Length - 1).ToArray()));
        Corrupt("one material byte extra", Deflate(Append(plain, 7)));
        var negative = (byte[])plain.Clone();
        negative[0] = 1; // zigzag 1 = −1：第一个点的预测值是 0
        Corrupt("height below zero", Deflate(negative));
        Corrupt("varint runs off the end", Deflate(new byte[] { 0x80, 0x80 }));
        Corrupt("varint above 64 bits", Deflate(Append(new byte[] { 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff }, 0x01)));
        Corrupt("plain text above the limit", Deflate(new byte[3 * 4096 + 4096 + 1]));
        Corrupt("not DEFLATE (reserved block type)", new byte[] { 0x07, 0x00, 0x00, 0x00 });
        var whole = Deflate(plain);
        Corrupt("truncated DEFLATE stream", whole.AsSpan(0, whole.Length / 2).ToArray());
        Corrupt("empty DEFLATE stream (one final fixed-Huffman block)", new byte[] { 0x03, 0x00 });

        var disposed = TerrainPack.Open(good);
        disposed.Dispose();
        disposed.Dispose();
        Throws<ObjectDisposedException>(() => disposed.ReadChunk(new ChunkKey(0, 0)), "ReadChunk after Dispose");
        Console.WriteLine("PASS terrain corrupt chunk: SHA-256, short read, malformed plain text, isolation");
    }

    private static TerrainChunk DecodeOne(byte[] stored)
    {
        using var pack = TerrainPack.Open(BuildPack(1, 1, 1, 0, 0, new[] { stored }));
        return pack.ReadChunk(new ChunkKey(0, 0));
    }

    private static ushort[] Ramp()
    {
        var h = new ushort[TerrainGeometry.ChunkPoints];
        for (int i = 0; i < h.Length; i++) h[i] = (ushort)(1000 + i % 64 * 3 + i / 64 * 5);
        return h;
    }

    private static byte[] Materials(byte value)
    {
        var m = new byte[TerrainGeometry.ChunkPoints];
        m.AsSpan().Fill(value);
        return m;
    }

    private static byte[] Append(byte[] data, byte value)
    {
        var next = new byte[data.Length + 1];
        data.CopyTo(next, 0);
        next[data.Length] = value;
        return next;
    }

    // 测试侧编码器（与 Go encodeChunk 同一规则）：平面预测差分 zigzag uvarint ‖ 材质。
    private static byte[] Plain(ushort[] heights, byte[] materials)
    {
        var plain = new List<byte>();
        for (int k = 0; k < heights.Length; k++)
        {
            int i = k % 64, j = k / 64;
            long p = j == 0 && i == 0 ? 0 : j == 0 ? heights[k - 1] : i == 0 ? heights[k - 64] : (long)heights[k - 1] + heights[k - 64] - heights[k - 65];
            long d = heights[k] - p;
            ulong u = (ulong)(d << 1 ^ d >> 63);
            while (u >= 0x80) { plain.Add((byte)(u | 0x80)); u >>= 7; }
            plain.Add((byte)u);
        }
        plain.AddRange(materials);
        return plain.ToArray();
    }

    private static byte[] Deflate(byte[] plain)
    {
        var output = new MemoryStream();
        using (var z = new DeflateStream(output, CompressionLevel.SmallestSize, leaveOpen: true)) z.Write(plain);
        return output.ToArray();
    }

    // 测试侧写包：按 pack.go 头注释的布局写包头、索引表、块数据与两级 SHA-256。
    private static byte[] BuildPack(long revision, int chunksX, int chunksY, int heightBase, int waterLevel, byte[][] stored)
    {
        int dataStart = HeaderSize + stored.Length * EntrySize, size = dataStart;
        foreach (var s in stored) size += s.Length;
        var d = new byte[size];
        Encoding("RTERRAIN").CopyTo(d, 0);
        BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(8), 1);
        BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(12), HeaderSize);
        BinaryPrimitives.WriteInt64LittleEndian(d.AsSpan(16), revision);
        BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(24), (uint)chunksX);
        BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(28), (uint)chunksY);
        BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(32), 64);
        BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(36), 100);
        BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(40), 10);
        BinaryPrimitives.WriteInt32LittleEndian(d.AsSpan(44), heightBase);
        BinaryPrimitives.WriteInt32LittleEndian(d.AsSpan(48), waterLevel);
        BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(52), 1);
        BinaryPrimitives.WriteUInt64LittleEndian(d.AsSpan(56), HeaderSize);
        BinaryPrimitives.WriteUInt32LittleEndian(d.AsSpan(64), EntrySize);
        int offset = dataStart;
        for (int i = 0; i < stored.Length; i++)
        {
            var e = d.AsSpan(HeaderSize + i * EntrySize);
            BinaryPrimitives.WriteUInt64LittleEndian(e, (ulong)offset);
            BinaryPrimitives.WriteUInt32LittleEndian(e.Slice(8), (uint)stored[i].Length);
            SHA256.HashData(stored[i]).CopyTo(e.Slice(12));
            stored[i].CopyTo(d, offset);
            offset += stored[i].Length;
        }
        Reseal(d);
        return d;
    }

    private static byte[] Encoding(string s) => System.Text.Encoding.ASCII.GetBytes(s);

    // 按当前包头的块数重算 metaSHA256（包头前 96 字节 ‖ 索引表）。
    private static void Reseal(byte[] d)
    {
        int chunks = (int)(BinaryPrimitives.ReadUInt32LittleEndian(d.AsSpan(24)) * BinaryPrimitives.ReadUInt32LittleEndian(d.AsSpan(28)));
        var buffer = new byte[MetaHashOffset + chunks * EntrySize];
        d.AsSpan(0, MetaHashOffset).CopyTo(buffer);
        d.AsSpan(HeaderSize, chunks * EntrySize).CopyTo(buffer.AsSpan(MetaHashOffset));
        SHA256.HashData(buffer).CopyTo(d, MetaHashOffset);
    }

    private static (int Offset, int Length) Entry(byte[] d, int index)
    {
        var e = d.AsSpan(HeaderSize + index * EntrySize);
        return ((int)BinaryPrimitives.ReadUInt64LittleEndian(e), (int)BinaryPrimitives.ReadUInt32LittleEndian(e.Slice(8)));
    }

    private sealed class NonSeekable : MemoryStream
    {
        public NonSeekable(byte[] data) : base(data) { }
        public override bool CanSeek => false;
    }

    // Length 报整份，但只能读到前 readable 字节。
    private sealed class ShortStream : MemoryStream
    {
        private readonly int readable;
        public ShortStream(byte[] data, int readable) : base(data) { this.readable = readable; }
        // 派生的 MemoryStream 上 Read(Span) 会转到 Read(byte[], …)，所以只在后者限长。
        public override int Read(byte[] buffer, int offset, int count)
        {
            long left = readable - Position;
            return left <= 0 ? 0 : base.Read(buffer, offset, (int)Math.Min(left, count));
        }
    }
}
