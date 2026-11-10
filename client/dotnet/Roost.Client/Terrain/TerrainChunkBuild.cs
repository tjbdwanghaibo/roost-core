using System;
using System.Collections.Generic;

namespace Roost.Client.Terrain
{
    // 一块重建的输入快照（不可变）：3 × 3 邻域里用到的块数据（TerrainChunkData 装入后不变，只存引用、不复制）、本块与
    // 四个共边邻块的水位、本块与四个共边邻块的 LOD 级别。由 TerrainChunkBuilder.TryCapture 从视图取，交给后台线程 Build。
    public sealed class TerrainBuildInput
    {
        internal readonly TerrainChunkData?[] Chunks; // [(dy+1)·3 + (dx+1)]，地图外或用不到的为 null
        public TerrainMeta Meta { get; }
        public ChunkKey Key { get; }
        public int WaterLevel { get; }
        public ShoreNeighbourLevels NeighbourLevels { get; }
        public int Lod { get; }
        public int? LeftLod { get; }
        public int? RightLod { get; }
        public int? BottomLod { get; }
        public int? TopLod { get; }
        public ShoreOptions Shore { get; }

        internal TerrainBuildInput(TerrainMeta meta, ChunkKey key, TerrainChunkData?[] chunks, int level, ShoreNeighbourLevels levels,
            int lod, int? left, int? right, int? bottom, int? top, ShoreOptions shore)
        {
            Meta = meta; Key = key; Chunks = chunks; WaterLevel = level; NeighbourLevels = levels; Lod = lod;
            LeftLod = left; RightLod = right; BottomLod = bottom; TopLod = top; Shore = shore;
        }

        // 偏移 (dx, dy) 处的块数据（dx、dy ∈ {−1, 0, 1}）。
        public TerrainChunkData? ChunkAt(int dx, int dy) => Chunks[(dy + 1) * 3 + dx + 1];
    }

    // 一块重建的结果：顶点高度、顶点坡度（法线）、LOD 三角形、岸线。不可变，可以跨线程交给渲染侧。
    public sealed class TerrainChunkBuild
    {
        private readonly int[] heights, gradX, gradY;
        private readonly ushort[] indices;

        public ChunkKey Key { get; }
        public TerrainBuildInput Input { get; }
        // 65 × 65 顶点世界高度（厘米），与 TerrainMesh.TryBuildHeightGrid 逐个相同。
        public ReadOnlySpan<int> Heights => heights;
        // 65 × 65 顶点的中心差分（厘米 / 2m）：GradX = H(gx+1) − H(gx−1)，GradY 同理，采样下标在地图内钳位（与查询的边缘复制一致）。
        // 法线 = normalize(−GradX, −GradY, 200)（世界坐标，高度朝上；见 NormalAt）。相邻块共享边上的值逐个相同。
        public ReadOnlySpan<int> GradientX => gradX;
        public ReadOnlySpan<int> GradientY => gradY;
        // LOD 三角形下标（TerrainLod.BuildIndices），指向 65 × 65 顶点。
        public ReadOnlySpan<ushort> Indices => indices;
        public TerrainShore Shore { get; }
        // 由调度器填写：这份结果属于哪一次会话（加载代际）与哪一份地形（基础包 / 水位）。
        public long Session { get; internal set; }
        public long Terrain { get; internal set; }

        internal TerrainChunkBuild(TerrainBuildInput input, int[] heights, int[] gradX, int[] gradY, ushort[] indices, TerrainShore shore)
        {
            Key = input.Key; Input = input; this.heights = heights; this.gradX = gradX; this.gradY = gradY; this.indices = indices; Shore = shore;
        }

        public void NormalAt(int i, int j, out float x, out float y, out float z)
        {
            int k = j * TerrainMesh.VerticesPerSide + i;
            double gx = -gradX[k], gy = -gradY[k], gz = 2 * TerrainGeometry.SampleSpacing;
            double length = Math.Sqrt(gx * gx + gy * gy + gz * gz);
            x = (float)(gx / length); y = (float)(gy / length); z = (float)(gz / length);
        }

        // 这份结果与相邻块（8 邻域）的另一份结果能否同时显示而不出现接缝：公共边 / 公共角上的顶点高度与坡度逐个相同，
        // 公共边两侧的 LOD 边步长相同，彼此记录的对方水位与对方的实际水位相同（岸线交点、侧壁一致）。不相邻时抛异常。
        public bool SeamMatches(TerrainChunkBuild other)
        {
            if (other == null) throw new ArgumentNullException(nameof(other));
            int dx = other.Key.X - Key.X, dy = other.Key.Y - Key.Y;
            if (Math.Abs(dx) > 1 || Math.Abs(dy) > 1 || (dx == 0 && dy == 0)) throw new ArgumentException($"chunks {Key} and {other.Key} are not neighbours");
            const int n = TerrainGeometry.ChunkSamples;
            if (dx != 0 && dy != 0)
                return SameVertex(other, dx > 0 ? n : 0, dy > 0 ? n : 0, dx > 0 ? 0 : n, dy > 0 ? 0 : n);
            var side = dx > 0 ? TerrainSide.Right : dx < 0 ? TerrainSide.Left : dy > 0 ? TerrainSide.Top : TerrainSide.Bottom;
            var back = Opposite(side);
            if (TerrainLod.EdgeStep(Input.Lod, LodOf(side)) != TerrainLod.EdgeStep(other.Input.Lod, other.LodOf(back))) return false;
            if (Input.NeighbourLevels.Of(side) != other.Input.WaterLevel || other.Input.NeighbourLevels.Of(back) != Input.WaterLevel) return false;
            for (int t = 0; t <= n; t++)
            {
                bool vertical = side == TerrainSide.Left || side == TerrainSide.Right;
                int mine = side == TerrainSide.Right || side == TerrainSide.Top ? n : 0;
                if (vertical ? !SameVertex(other, mine, t, n - mine, t) : !SameVertex(other, t, mine, t, n - mine)) return false;
            }
            return true;
        }

        private bool SameVertex(TerrainChunkBuild other, int i, int j, int oi, int oj)
        {
            int a = j * TerrainMesh.VerticesPerSide + i, b = oj * TerrainMesh.VerticesPerSide + oi;
            return heights[a] == other.heights[b] && gradX[a] == other.gradX[b] && gradY[a] == other.gradY[b];
        }

        internal int? LodOf(TerrainSide side) => side switch
        {
            TerrainSide.Left => Input.LeftLod,
            TerrainSide.Right => Input.RightLod,
            TerrainSide.Bottom => Input.BottomLod,
            _ => Input.TopLod,
        };

        internal static TerrainSide Opposite(TerrainSide side) => side switch
        {
            TerrainSide.Left => TerrainSide.Right,
            TerrainSide.Right => TerrainSide.Left,
            TerrainSide.Bottom => TerrainSide.Top,
            _ => TerrainSide.Bottom,
        };
    }

    // 块重建的纯函数部分：依赖哪些块、读它们的哪些采样、从视图取快照、生成结果。调度见 TerrainRebuildScheduler。
    public static class TerrainChunkBuilder
    {
        private const int N = TerrainGeometry.ChunkSamples;

        // 块 key 的重建要读的块（相对偏移）与各自读到的采样矩形 [I0, I1] × [J0, J1]（块内采样下标，含两端）：
        //   顶点网格（i、j ∈ [0, 64]）：本块全部、右邻第 0 列、上邻第 0 行、右上邻 (0, 0)；
        //   中心差分坡度：额外读顶点四周一圈——左邻第 63 列、下邻第 63 行、右 / 上邻第 1 列 / 行、右上邻 (1, 0) / (0, 1)、
        //   左上邻 (63, 0)（顶点 (0, 64) 的左邻点）、右下邻 (0, 63)（顶点 (64, 0) 的下邻点）。左下邻用不到。
        // 地图外的邻块不读（采样下标钳位到地图内，与查询的边缘复制一致）。
        private static readonly (int Dx, int Dy, int I0, int I1, int J0, int J1)[] Reads =
        {
            (0, 0, 0, N - 1, 0, N - 1),
            (1, 0, 0, 1, 0, N - 1),
            (0, 1, 0, N - 1, 0, 1),
            (1, 1, 0, 1, 0, 1),
            (-1, 0, N - 1, N - 1, 0, N - 1),
            (0, -1, 0, N - 1, N - 1, N - 1),
            (-1, 1, N - 1, N - 1, 0, 0),
            (1, -1, 0, 0, N - 1, N - 1),
        };

        // 块 key 的重建依赖的块（地图内）。
        public static IReadOnlyList<ChunkKey> Dependencies(TerrainMeta meta, ChunkKey key)
        {
            var keys = new List<ChunkKey>(Reads.Length);
            foreach (var r in Reads)
            {
                var k = new ChunkKey(key.X + r.Dx, key.Y + r.Dy);
                if (meta.Contains(k)) keys.Add(k);
            }
            return keys;
        }

        // 块 changed 里采样矩形 [i0, i1] × [j0, j1] 变了之后，重建结果会变的块（地图内）：读到这片采样的块。
        public static IReadOnlyList<ChunkKey> Dependents(TerrainMeta meta, ChunkKey changed, int i0, int i1, int j0, int j1)
        {
            var keys = new List<ChunkKey>(Reads.Length);
            foreach (var r in Reads)
            {
                if (r.I0 > i1 || i0 > r.I1 || r.J0 > j1 || j0 > r.J1) continue;
                var k = new ChunkKey(changed.X - r.Dx, changed.Y - r.Dy);
                if (meta.Contains(k)) keys.Add(k);
            }
            return keys;
        }

        // 两份数据在“块 dependent 从偏移 (dx, dy) 读的那片采样”上是否相同（引用相同直接算相同）。
        internal static bool SameReads(TerrainChunkData a, TerrainChunkData b, int dx, int dy)
        {
            if (ReferenceEquals(a, b)) return true;
            foreach (var r in Reads)
            {
                if (r.Dx != dx || r.Dy != dy) continue;
                var ha = a.Heights; var hb = b.Heights;
                for (int j = r.J0; j <= r.J1; j++)
                    if (!ha.Slice(j * N + r.I0, r.I1 - r.I0 + 1).SequenceEqual(hb.Slice(j * N + r.I0, r.I1 - r.I0 + 1))) return false;
                return true;
            }
            return true;
        }

        // 两份块数据里不同的采样所占的矩形；完全相同返回 false。
        internal static bool ChangedBox(TerrainChunkData a, TerrainChunkData b, out int i0, out int i1, out int j0, out int j1)
        {
            i0 = j0 = N; i1 = j1 = -1;
            var ha = a.Heights; var hb = b.Heights;
            for (int k = 0; k < TerrainGeometry.ChunkPoints; k++)
            {
                if (ha[k] == hb[k]) continue;
                int i = k % N, j = k / N;
                if (i < i0) i0 = i;
                if (i > i1) i1 = i;
                if (j < j0) j0 = j;
                if (j > j1) j1 = j;
            }
            return i1 >= 0;
        }

        // 从视图取块 key 的输入快照。依赖的块有没装的返回 false（missing 是第一个缺的块）。lods 给出本块与四个共边邻块的 LOD
        // 级别（邻块 null = 没有网格）。只取引用，不复制数据。
        public static bool TryCapture(TerrainView view, ChunkKey key, int lod, int? left, int? right, int? bottom, int? top,
            ShoreOptions shore, out TerrainBuildInput? input, out ChunkKey missing)
        {
            if (view == null) throw new ArgumentNullException(nameof(view));
            if (shore == null) throw new ArgumentNullException(nameof(shore));
            TerrainLod.Step(lod);
            var meta = view.Meta;
            if (!meta.Contains(key)) throw new TerrainOutOfMapException("chunk " + key);
            input = null; missing = default;
            var chunks = new TerrainChunkData?[9];
            foreach (var r in Reads)
            {
                var k = new ChunkKey(key.X + r.Dx, key.Y + r.Dy);
                if (!meta.Contains(k)) continue;
                var c = view.GetChunk(k);
                if (c == null) { missing = k; return false; }
                chunks[(r.Dy + 1) * 3 + r.Dx + 1] = c;
            }
            input = new TerrainBuildInput(meta, key, chunks, view.WaterLevelOf(key), ShoreNeighbourLevels.Of(view, key), lod, left, right, bottom, top, shore);
            return true;
        }

        // 生成结果（纯函数，可在任意线程跑；输入不可变）。
        public static TerrainChunkBuild Build(TerrainBuildInput input)
        {
            if (input == null) throw new ArgumentNullException(nameof(input));
            const int v = TerrainMesh.VerticesPerSide;
            var meta = input.Meta;
            long gx0 = input.Key.X * (long)N, gy0 = input.Key.Y * (long)N;
            long maxX = (long)meta.ChunksX * N - 1, maxY = (long)meta.ChunksY * N - 1;
            // 世界采样 (gx, gy) 的世界高度：下标钳位到地图内（边缘复制），按所在块从输入快照里取。
            int Sample(long gx, long gy)
            {
                gx = Math.Min(Math.Max(gx, 0), maxX);
                gy = Math.Min(Math.Max(gy, 0), maxY);
                int dx = (int)(TerrainGeometry.FloorDiv(gx, N) - input.Key.X), dy = (int)(TerrainGeometry.FloorDiv(gy, N) - input.Key.Y);
                var c = input.ChunkAt(dx, dy) ?? throw new InvalidOperationException($"build {input.Key}: sample ({gx}, {gy}) needs chunk offset ({dx}, {dy}), which was not captured");
                return meta.WorldHeight(c.Heights[(int)(TerrainGeometry.FloorMod(gy, N) * N + TerrainGeometry.FloorMod(gx, N))]);
            }
            var heights = new int[TerrainMesh.VertexCount];
            var gradX = new int[TerrainMesh.VertexCount];
            var gradY = new int[TerrainMesh.VertexCount];
            for (int j = 0; j < v; j++)
                for (int i = 0; i < v; i++)
                {
                    long gx = gx0 + i, gy = gy0 + j;
                    int k = j * v + i;
                    heights[k] = Sample(gx, gy);
                    gradX[k] = Sample(gx + 1, gy) - Sample(gx - 1, gy);
                    gradY[k] = Sample(gx, gy + 1) - Sample(gx, gy - 1);
                }
            var indices = TerrainLod.BuildIndices(input.Lod, input.LeftLod, input.RightLod, input.BottomLod, input.TopLod);
            var shore = TerrainShoreBuilder.Build(input.Key, heights, input.WaterLevel, input.NeighbourLevels, input.Shore);
            return new TerrainChunkBuild(input, heights, gradX, gradY, indices, shore);
        }
    }
}
