using System;
using System.Collections.Generic;
using System.Linq;
using Roost.Client.Terrain;

internal static partial class Program
{
    // LOD 三角形：每种本块级别 × 每种邻块级别（含没有网格）都恰好铺满整块（面积 4096、三角形都逆时针不退化），
    // 块边上只用到边步长倍数的顶点，且三角形的边不会跨过对方在公共边上用到的顶点（没有 T 接缝）。
    private static void TestLod()
    {
        int combos = 0;
        var levels = new int?[] { null, 0, 1, 2, 3, 4, 5 };
        for (int self = 0; self <= TerrainLod.MaxLevel; self++)
            foreach (var right in levels)
            {
                var idx = TerrainLod.BuildIndices(self, null, right, right, null);
                CheckCoverage(idx, $"lod {self} right/bottom {right}");
                // 右边（x = 64）的顶点集合与右邻块（级别 right，左邻是 self）左边（x = 0）的集合相同。
                if (right != null)
                {
                    var other = TerrainLod.BuildIndices(right.Value, self, null, null, null);
                    CheckCoverage(other, $"lod {right} left {self}");
                    var mine = EdgeVertices(idx, i => i % 65 == 64).Select(i => i / 65).ToArray();
                    var theirs = EdgeVertices(other, i => i % 65 == 0).Select(i => i / 65).ToArray();
                    Check(mine.SequenceEqual(theirs), $"lod {self}|{right}: shared edge uses the same vertices");
                    int step = TerrainLod.EdgeStep(self, right);
                    Check(mine.All(j => j % step == 0) && mine.Length == 64 / step + 1, $"lod {self}|{right}: edge step {step}");
                    CheckNoTJunction(idx, mine, i => i % 65 == 64, i => i / 65, $"lod {self}|{right} mine");
                    CheckNoTJunction(other, mine, i => i % 65 == 0, i => i / 65, $"lod {self}|{right} theirs");
                }
                combos++;
            }
        Throws<ArgumentOutOfRangeException>(() => TerrainLod.BuildIndices(6, null, null, null, null), "LOD level above the limit");
        Console.WriteLine($"PASS terrain lod: {combos} level combinations tile the chunk, shared edges use the same vertices, no T-junctions");
    }

    private static void CheckCoverage(ushort[] idx, string where)
    {
        Check(idx.Length % 3 == 0, where + ": whole triangles");
        long twiceArea = 0;
        for (int t = 0; t < idx.Length; t += 3)
        {
            int x0 = idx[t] % 65, y0 = idx[t] / 65, x1 = idx[t + 1] % 65, y1 = idx[t + 1] / 65, x2 = idx[t + 2] % 65, y2 = idx[t + 2] / 65;
            long a = (long)(x1 - x0) * (y2 - y0) - (long)(x2 - x0) * (y1 - y0);
            Check(a > 0, $"{where}: triangle {t / 3} is counter-clockwise and not degenerate");
            twiceArea += a;
        }
        Check(twiceArea == 2 * 64 * 64, $"{where}: triangles cover exactly the chunk (twice area {twiceArea})");
    }

    private static IEnumerable<int> EdgeVertices(ushort[] idx, Func<int, bool> onEdge) =>
        idx.Select(i => (int)i).Where(onEdge).Distinct().OrderBy(i => i);

    // 三角形落在块边上的边，端点必须是边上相邻的两个已用顶点（中间不夹着对方用到的顶点）。
    private static void CheckNoTJunction(ushort[] idx, int[] used, Func<int, bool> onEdge, Func<int, int> along, string where)
    {
        for (int t = 0; t < idx.Length; t += 3)
            for (int e = 0; e < 3; e++)
            {
                int a = idx[t + e], b = idx[t + (e + 1) % 3];
                if (!onEdge(a) || !onEdge(b)) continue;
                int lo = Math.Min(along(a), along(b)), hi = Math.Max(along(a), along(b));
                Check(!used.Any(j => j > lo && j < hi), $"{where}: edge {lo}–{hi} skips a shared vertex");
            }
    }

    // 块重建：顶点高度等于 TryBuildHeightGrid；声明的读取范围之外的邻块采样随便改，结果逐位不变（读取表不漏）；
    // 依赖与被依赖互逆；相邻块的结果接缝一致，编辑公共边后旧结果与新邻居不一致。
    private static void TestChunkBuild(byte[] packBytes)
    {
        using var pack = TerrainPack.Open(packBytes);
        var meta = pack.Meta;
        var view = LoadAll(pack);
        var builds = new Dictionary<ChunkKey, TerrainChunkBuild>();
        var grid = new int[TerrainMesh.VertexCount];
        for (int cy = 0; cy < meta.ChunksY; cy++)
            for (int cx = 0; cx < meta.ChunksX; cx++)
            {
                var key = new ChunkKey(cx, cy);
                Check(TerrainChunkBuilder.TryCapture(view, key, 0, null, null, null, null, ShoreOptions.Default, out var input, out _), "capture " + key);
                var b = TerrainChunkBuilder.Build(input!);
                Check(view.TryBuildHeightGrid(key, grid, out _) && b.Heights.SequenceEqual(grid), $"build {key}: heights equal the height grid");
                builds[key] = b;
            }
        foreach (var a in builds.Values)
            foreach (var b in builds.Values)
                if (a.Key != b.Key && Math.Abs(a.Key.X - b.Key.X) <= 1 && Math.Abs(a.Key.Y - b.Key.Y) <= 1)
                    Check(a.SeamMatches(b), $"seam {a.Key}–{b.Key}");
        // 坡度：内部顶点是中心差分；地图边钳位。
        var c00 = builds[new ChunkKey(0, 0)];
        Check(c00.GradientX[5 * 65 + 5] == view.Height(600, 500) - view.Height(400, 500), "central difference");
        Check(c00.GradientX[5 * 65 + 0] == view.Height(100, 500) - view.Height(0, 500), "map edge clamps");
        c00.NormalAt(5, 5, out _, out _, out float nz);
        Check(nz > 0 && nz <= 1, "normal points up");

        // 依赖：内部块 8 个（不含左下），被依赖互逆。
        var big = new TerrainMeta(1, 5, 5, 0, 0);
        var mid = new ChunkKey(2, 2);
        var deps = TerrainChunkBuilder.Dependencies(big, mid);
        Check(deps.Count == 8 && !deps.Contains(new ChunkKey(1, 1)), "interior chunk depends on 8 chunks, not the lower-left one");
        for (int dy = -1; dy <= 1; dy++)
            for (int dx = -1; dx <= 1; dx++)
            {
                var other = new ChunkKey(2 + dx, 2 + dy);
                bool reads = deps.Contains(other);
                bool listed = TerrainChunkBuilder.Dependents(big, other, 0, 63, 0, 63).Contains(mid);
                Check(reads == listed, $"dependents of {other} {(reads ? "include" : "exclude")} {mid}");
            }
        Check(TerrainChunkBuilder.Dependents(big, mid, 10, 50, 10, 50).SequenceEqual(new[] { mid }), "interior change touches only the chunk itself");
        Check(TerrainChunkBuilder.Dependents(big, mid, 0, 0, 30, 30).Count == 2, "left column change also rebuilds the left neighbour");
        Check(TerrainChunkBuilder.Dependents(big, mid, 1, 1, 30, 30).Count == 2, "second column feeds the left neighbour's normals");
        Check(TerrainChunkBuilder.Dependents(big, mid, 63, 63, 63, 63).Count == 3, "top-right sample feeds the right and top neighbours' normals, not the top-right one");

        // 读取表不漏：把每个邻块声明读取范围之外的采样改掉，重建结果逐位不变；改范围内的一个采样，结果变化。
        var centre = new ChunkKey(1, 0);
        Check(TerrainChunkBuilder.TryCapture(view, centre, 0, null, null, null, null, ShoreOptions.Default, out var baseInput, out _), "capture centre");
        var reference = TerrainChunkBuilder.Build(baseInput!);
        int poisoned = 0;
        foreach (var dep in TerrainChunkBuilder.Dependencies(meta, centre))
        {
            if (dep == centre) continue;
            var poisonView = LoadAll(pack);
            var heights = poisonView.GetChunk(dep)!.Heights.ToArray();
            for (int k = 0; k < heights.Length; k++)
            {
                int i = k % 64, j = k / 64;
                if (!TerrainChunkBuilder.Dependents(meta, dep, i, i, j, j).Contains(centre)) { heights[k] = (ushort)(heights[k] ^ 0x155); poisoned++; }
            }
            poisonView.SetOverlay(dep, meta.BaseRevision, 1, heights, poisonView.GetChunk(dep)!.Materials);
            Check(TerrainChunkBuilder.TryCapture(poisonView, centre, 0, null, null, null, null, ShoreOptions.Default, out var input, out _), "capture poisoned");
            var b = TerrainChunkBuilder.Build(input!);
            Check(b.Heights.SequenceEqual(reference.Heights) && b.GradientX.SequenceEqual(reference.GradientX) && b.GradientY.SequenceEqual(reference.GradientY),
                $"samples of {dep} outside the declared reads do not change {centre}");
        }

        // 编辑块 (1,0) 的第 0 列：新 (1,0) 与旧 (0,0) 接缝不一致；与新 (0,0) 一致。
        var right = new ChunkKey(1, 0);
        var edited = view.GetChunk(right)!.Heights.ToArray();
        for (int j = 0; j < 64; j++) edited[j * 64] = (ushort)(edited[j * 64] + 3);
        view.SetOverlay(right, meta.BaseRevision, 1, edited, view.GetChunk(right)!.Materials);
        TerrainChunkBuilder.TryCapture(view, right, 0, null, null, null, null, ShoreOptions.Default, out var newRightInput, out _);
        TerrainChunkBuilder.TryCapture(view, new ChunkKey(0, 0), 0, null, null, null, null, ShoreOptions.Default, out var newLeftInput, out _);
        var newRight = TerrainChunkBuilder.Build(newRightInput!);
        var newLeft = TerrainChunkBuilder.Build(newLeftInput!);
        Check(!newRight.SeamMatches(builds[new ChunkKey(0, 0)]), "edited edge no longer matches the old neighbour");
        Check(newRight.SeamMatches(newLeft), "edited edge matches the rebuilt neighbour");
        // LOD 不同但边步长一致才算接上。
        TerrainChunkBuilder.TryCapture(view, new ChunkKey(0, 0), 0, null, 2, null, null, ShoreOptions.Default, out var lodLeftInput, out _);
        TerrainChunkBuilder.TryCapture(view, right, 2, 0, null, null, null, ShoreOptions.Default, out var lodRightInput, out _);
        Check(TerrainChunkBuilder.Build(lodLeftInput!).SeamMatches(TerrainChunkBuilder.Build(lodRightInput!)), "LOD 0 | LOD 2 stitched");
        Check(!TerrainChunkBuilder.Build(lodLeftInput!).SeamMatches(newRight), "LOD 0 stitched for a LOD 2 neighbour does not match a LOD 0 neighbour");
        Console.WriteLine($"PASS terrain chunk build: heights equal the grid, seams agree, dependencies inverse, {poisoned} samples outside the declared reads ignored, edge edit and LOD stitching detected");
    }
}
