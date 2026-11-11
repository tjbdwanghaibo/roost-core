using System;
using System.Collections.Generic;

namespace Roost.Client.Terrain
{
    // 块与采样的几何常量，与服务端 gameplay/terrain 的同名常量逐个相同。它们决定 DAO 形状（Heights / Materials
    // 各 ChunkPoints 个元素）和查询算法，所以是常量而不是配置；原始地形包的包头也记录它们，读包时逐项核对。
    // 世界单位是厘米（cellworld 约定）。
    public static class TerrainGeometry
    {
        // 每块每边的采样点数。
        public const int ChunkSamples = 64;
        // 每块的采样点数，也是 Heights / Materials 的长度；块内下标行优先：下标 = j·64 + i（i 沿 X、j 沿 Y）。
        public const int ChunkPoints = ChunkSamples * ChunkSamples;
        // 相邻采样点的间距（世界单位，1m）。
        public const int SampleSpacing = 100;
        // 块边长（世界单位，64m）。
        public const int ChunkSize = ChunkSamples * SampleSpacing;
        // 高度原始值 1 对应的世界高度（世界单位，10cm）。
        public const int HeightStep = 10;
        // 地图每边最多的块数；超过的包当作损坏拒绝。
        public const int MaxChunksPerSide = 1024;

        // 包含世界坐标 (x, y) 的块：向负无穷取整，远在地图外的坐标饱和到 int32，不会回绕进地图。
        public static ChunkKey ChunkOf(long x, long y) =>
            new ChunkKey(ClampInt32(FloorDiv(x, ChunkSize)), ClampInt32(FloorDiv(y, ChunkSize)));

        // 世界坐标 (x, y) 所在 1m 格左下角的采样点：所属块与它在 Heights / Materials 里的下标。左下角采样点总在同一块里。
        public static ChunkKey PointIndex(long x, long y, out int index)
        {
            long gx = FloorDiv(x, SampleSpacing), gy = FloorDiv(y, SampleSpacing);
            index = (int)(FloorMod(gy, ChunkSamples) * ChunkSamples + FloorMod(gx, ChunkSamples));
            return new ChunkKey(ClampInt32(FloorDiv(gx, ChunkSamples)), ClampInt32(FloorDiv(gy, ChunkSamples)));
        }

        // C# 的 / 与 % 向零取整（与 Go 相同），负坐标要向负无穷取整，所以单独写。
        internal static long FloorDiv(long a, long b)
        {
            long q = a / b;
            if (a % b != 0 && (a < 0) != (b < 0)) q--;
            return q;
        }

        internal static long FloorMod(long a, long b) => a - FloorDiv(a, b) * b;

        internal static int ClampInt32(long v) => (int)Math.Min(Math.Max(v, int.MinValue), int.MaxValue);
    }

    // 块坐标：块 (X, Y) 覆盖世界坐标 [X·6400, (X+1)·6400) × [Y·6400, (Y+1)·6400)。地图内的块坐标都非负。
    public readonly struct ChunkKey : IEquatable<ChunkKey>
    {
        public int X { get; }
        public int Y { get; }

        public ChunkKey(int x, int y) { X = x; Y = y; }

        // 与 Go ChunkKey.ID() 相同的编码：高 32 位是 X、低 32 位是 Y（各按 uint32 位模式）。实体 ID 如何由它拼出
        // 是游戏工程的约定（game-demo：实体唯一号 = Id + 1），SDK 不假设。
        public ulong Id => (ulong)(uint)X << 32 | (uint)Y;

        // Id 的逆。
        public static ChunkKey FromId(ulong id) => new ChunkKey((int)(uint)(id >> 32), (int)(uint)id);

        // 块的最小角（世界坐标）。
        public long OriginX => (long)X * TerrainGeometry.ChunkSize;
        public long OriginY => (long)Y * TerrainGeometry.ChunkSize;

        public bool Equals(ChunkKey other) => X == other.X && Y == other.Y;
        public override bool Equals(object? obj) => obj is ChunkKey other && Equals(other);
        public override int GetHashCode() => unchecked(X * 397 ^ Y);
        public static bool operator ==(ChunkKey a, ChunkKey b) => a.Equals(b);
        public static bool operator !=(ChunkKey a, ChunkKey b) => !a.Equals(b);
        public override string ToString() => "(" + X + "," + Y + ")";
    }

    // 一片按块对齐的矩形区域的水位线（与 Go terrain.WaterRegion 相同）：块坐标半开区间 [MinX, MaxX) × [MinY, MaxY) 里的
    // 每一块，地面高度低于 Level（世界高度，厘米）的位置是水；Level 为 int.MinValue 表示没有水。区域互不重叠。
    public readonly struct TerrainWaterRegion
    {
        public int MinX { get; }
        public int MinY { get; }
        public int MaxX { get; }
        public int MaxY { get; }
        public int Level { get; }
        public TerrainWaterRegion(int minX, int minY, int maxX, int maxY, int level) { MinX = minX; MinY = minY; MaxX = maxX; MaxY = maxY; Level = level; }
        public bool Contains(ChunkKey key) => key.X >= MinX && key.Y >= MinY && key.X < MaxX && key.Y < MaxY;
        internal bool Overlaps(TerrainWaterRegion o) => MinX < o.MaxX && o.MinX < MaxX && MinY < o.MaxY && o.MinY < MaxY;
        public override string ToString() => $"[{MinX},{MaxX})×[{MinY},{MaxY}) level {Level}";
    }

    // 一张地图的参数，记在原始地形包的包头里（与 Go terrain.Meta 相同）。
    public sealed class TerrainMeta
    {
        // 一张图最多的区域水位数（与 Go terrain.MaxWaterRegions 相同）。
        public const int MaxWaterRegions = 1024;

        // 原始地形的版本；覆盖块（TerrainChunk DAO）的 BaseRevision 必须等于它。
        public long BaseRevision { get; }
        // 地图每边的块数：地图覆盖世界坐标 [0, ChunksX·6400) × [0, ChunksY·6400)。
        public int ChunksX { get; }
        public int ChunksY { get; }
        // 高度原始值 0 对应的世界高度（厘米）。
        public int HeightBase { get; }
        // 全局水位线（世界高度，厘米）：不在任何区域里的块，地面高度低于它的位置是水。
        public int WaterLevel { get; }
        // 区域水位（包格式版本 2，可编辑地形 P4 / D3）：区域里的块用区域的 Level 代替 WaterLevel。只读视图：区域配置只随
        // 原始地形包换版，不能把它转回数组或 IList 改写（内部数组不外露；TerrainView 的水位表在构造时按它算好）。
        public IReadOnlyList<TerrainWaterRegion> WaterRegions { get; }
        private readonly TerrainWaterRegion[] regions;

        public TerrainMeta(long baseRevision, int chunksX, int chunksY, int heightBase, int waterLevel)
            : this(baseRevision, chunksX, chunksY, heightBase, waterLevel, Array.Empty<TerrainWaterRegion>()) { }

        // 检查与 Go Meta.Validate 相同：块数、高度基准、区域个数 ≤ MaxWaterRegions、每片区域非空且在地图内、两两不重叠。
        public TerrainMeta(long baseRevision, int chunksX, int chunksY, int heightBase, int waterLevel, IReadOnlyList<TerrainWaterRegion> waterRegions)
        {
            if (chunksX < 1 || chunksY < 1 || chunksX > TerrainGeometry.MaxChunksPerSide || chunksY > TerrainGeometry.MaxChunksPerSide)
                throw new ArgumentOutOfRangeException(nameof(chunksX), $"{chunksX} × {chunksY} chunks, each side must be in [1, {TerrainGeometry.MaxChunksPerSide}]");
            if ((long)heightBase + ushort.MaxValue * (long)TerrainGeometry.HeightStep > int.MaxValue)
                throw new ArgumentOutOfRangeException(nameof(heightBase), $"height base {heightBase} overflows int32 at the highest raw height");
            if (waterRegions == null) throw new ArgumentNullException(nameof(waterRegions));
            if (waterRegions.Count > MaxWaterRegions)
                throw new ArgumentOutOfRangeException(nameof(waterRegions), $"{waterRegions.Count} water regions, at most {MaxWaterRegions}");
            var regions = new TerrainWaterRegion[waterRegions.Count];
            for (int i = 0; i < regions.Length; i++)
            {
                var r = waterRegions[i];
                if (r.MinX < 0 || r.MinY < 0 || r.MinX >= r.MaxX || r.MinY >= r.MaxY || r.MaxX > chunksX || r.MaxY > chunksY)
                    throw new ArgumentOutOfRangeException(nameof(waterRegions), $"water region {i} {r} is empty or outside the {chunksX} × {chunksY} chunks");
                for (int j = 0; j < i; j++)
                    if (r.Overlaps(regions[j])) throw new ArgumentException($"water regions {j} {regions[j]} and {i} {r} overlap", nameof(waterRegions));
                regions[i] = r;
            }
            BaseRevision = baseRevision; ChunksX = chunksX; ChunksY = chunksY; HeightBase = heightBase; WaterLevel = waterLevel;
            this.regions = regions;
            WaterRegions = Array.AsReadOnly(regions);
        }

        // 块的水位线：所在区域的 Level，不在任何区域里时是全局 WaterLevel（与 Go Meta.WaterLevelOf 相同）。逐个比较区域；
        // 查询热路径用 TerrainView 里按块预先算好的表（TerrainView.WaterLevelOf）。
        public int WaterLevelOf(ChunkKey key)
        {
            foreach (var r in regions)
                if (r.Contains(key)) return r.Level;
            return WaterLevel;
        }

        // 地图的总块数。
        public int Chunks => ChunksX * ChunksY;
        // 地图的宽 / 高（世界单位）。
        public long Width => (long)ChunksX * TerrainGeometry.ChunkSize;
        public long Depth => (long)ChunksY * TerrainGeometry.ChunkSize;

        // 块是否在地图内。
        public bool Contains(ChunkKey key) => key.X >= 0 && key.Y >= 0 && key.X < ChunksX && key.Y < ChunksY;

        // 世界坐标是否在地图内（半开区间）。
        public bool ContainsPoint(long x, long y) => x >= 0 && y >= 0 && x < Width && y < Depth;

        // 高度原始值换成世界高度（厘米）；构造时已保证不溢出。
        public int WorldHeight(ushort raw) => HeightBase + raw * TerrainGeometry.HeightStep;

        // 块在包索引表与视图块表里的序号（行优先），调用方先确认 Contains。
        internal int IndexOf(ChunkKey key) => key.Y * ChunksX + key.X;
    }
}
