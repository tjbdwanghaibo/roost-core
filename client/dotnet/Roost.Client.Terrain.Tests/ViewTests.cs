using System;
using System.Collections.Generic;
using System.Linq;
using System.Threading;
using Roost.Client.Terrain;

internal static partial class Program
{
    // 覆盖块规则与 Go View.SetOverlay 相同：形状、Version ≥ 1、BaseRevision 等于包头、版本单调（相同则替换），
    // 拒绝时视图不变；以及变化通知（mesh 重建钩子）只在数据或版本真的变了时发。
    private static void TestOverlayRules(byte[] packBytes)
    {
        using var pack = TerrainPack.Open(packBytes);
        var view = LoadAll(pack);
        long rev = pack.Meta.BaseRevision;
        var key = new ChunkKey(1, 1);
        var changes = new List<TerrainChunkChange>();
        view.ChunkChanged += changes.Add;
        var heights = Ramp();
        var materials = Materials(5);

        Throws<TerrainOutOfMapException>(() => view.SetOverlay(new ChunkKey(3, 0), rev, 1, heights, materials), "overlay outside the map");
        Throws<TerrainInvalidChunkException>(() => view.SetOverlay(key, rev, 1, heights.AsSpan(1), materials), "4095 heights");
        Throws<TerrainInvalidChunkException>(() => view.SetOverlay(key, rev, 1, heights, materials.AsSpan(1)), "4095 materials");
        Throws<TerrainInvalidChunkException>(() => view.SetOverlay(key, rev, 0, heights, materials), "version 0");
        Throws<TerrainBaseRevisionException>(() => view.SetOverlay(key, rev + 1, 1, heights, materials), "other base revision");
        // DAO 形状（RepeatedField<uint>）：高度 > 65535、材质 > 255 都拒绝，不截断。
        var wide = heights.Select(h => (uint)h).ToList();
        var wideMaterials = materials.Select(m => (uint)m).ToList();
        wide[100] = 65536;
        Throws<TerrainInvalidChunkException>(() => view.SetOverlay(key, rev, 1, wide, wideMaterials), "uint32 height above 65535");
        wide[100] = heights[100];
        wideMaterials[200] = 256;
        Throws<TerrainInvalidChunkException>(() => view.SetOverlay(key, rev, 1, wide, wideMaterials), "uint32 material above 255");
        Throws<TerrainInvalidChunkException>(() => view.SetOverlay(key, rev, 1, wide, wideMaterials.Take(4095).ToList()), "4095 uint32 materials");
        Check(view.TryGetVersion(key, out long v0) && v0 == 0 && changes.Count == 0, "rejected overlays leave the base chunk");

        view.SetOverlay(key, rev, 2, heights, materials);
        Check(changes.Count == 1 && changes[0].Kind == TerrainChunkChangeKind.Updated && changes[0].WasLoaded &&
              changes[0].PreviousVersion == 0 && changes[0].Version == 2, "overlay replaces the base chunk");
        Check(changes[0].MeshesToRebuild.SequenceEqual(new[] { key, new ChunkKey(0, 1), new ChunkKey(1, 0), new ChunkKey(0, 0) }), "meshes sharing edges");
        int x = 6400 + 500, y = 6400 + 700;
        Check(view.Height(x, y) == pack.Meta.WorldHeight(heights[7 * 64 + 5]) && view.Material(x, y) == 5, "queries read the overlay");
        heights[7 * 64 + 5] = 1; // 调用方之后改自己的数组不影响视图
        Check(view.Height(x, y) == pack.Meta.WorldHeight(Ramp()[7 * 64 + 5]), "overlay copied its input");

        Throws<TerrainStaleOverlayException>(() => view.SetOverlay(key, rev, 1, Ramp(), materials), "older version");
        Check(view.TryGetVersion(key, out long v2) && v2 == 2 && changes.Count == 1, "stale overlay ignored");
        view.SetOverlay(key, rev, 2, Ramp(), materials);
        Check(changes.Count == 1, "same version and data: no notification (repeated full)");
        view.SetOverlay(key, rev, 2, Ramp(), Materials(6));
        Check(changes.Count == 2 && changes[1].PreviousVersion == 2 && changes[1].Version == 2 && view.Material(x, y) == 6, "same version, new data replaces and notifies");
        view.SetOverlay(key, rev, 3, Ramp().Select(h => (uint)h).ToList(), Materials(6).Select(m => (uint)m).ToList());
        Check(changes.Count == 3 && changes[2].Version == 3 && view.Material(x, y) == 6, "DAO-shaped overlay (uint32 lists)");
        view.LoadBase(key);
        Check(view.TryGetVersion(key, out long v3) && v3 == 3 && changes.Count == 3, "LoadBase never replaces an overlay");
        var data = view.GetChunk(key)!;
        Check(data.Version == 3 && data.Key == key && data.Heights.Length == 4096 && data.Materials[0] == 6, "GetChunk");

        Check(view.Unload(key) && !view.Unload(key) && view.Count == 5 && view.GetChunk(key) == null, "Unload");
        Check(changes.Count == 4 && changes[3].Kind == TerrainChunkChangeKind.Unloaded && changes[3].PreviousVersion == 3 && changes[3].Version == 0, "Unloaded notification");
        Throws<TerrainNotLoadedException>(() => view.Height(6400 + 50, 6400 + 50), "query after Unload");
        view.SetOverlay(key, rev, 1, Ramp(), Materials(1));
        Check(changes.Count == 5 && changes[4].Kind == TerrainChunkChangeKind.Loaded && !changes[4].WasLoaded && view.Count == 6,
            "after Unload any version installs again (the view keeps no history)");
        view.Unload(key);
        view.LoadBase(key);
        Check(changes.Count == 7 && changes[6].Kind == TerrainChunkChangeKind.Loaded && changes[6].Version == 0, "LoadBase notification");
        Check(TerrainMesh.AffectedChunks(pack.Meta, new ChunkKey(0, 0)).SequenceEqual(new[] { new ChunkKey(0, 0) }), "corner chunk affects only itself");

        // 订阅者抛错交给 Error，其他订阅者照常收到，数据已装入；Error 自身抛错被忽略。
        var errors = new List<Exception>();
        int after = 0;
        view.ChunkChanged += _ => throw new InvalidOperationException("business handler");
        view.ChunkChanged += _ => after++;
        view.Error += errors.Add;
        view.Error += _ => throw new Exception("error observer");
        view.SetOverlay(key, rev, 9, Ramp(), Materials(2));
        Check(errors.Count == 1 && errors[0] is InvalidOperationException && after == 1 && view.TryGetVersion(key, out long v9) && v9 == 9,
            "handler failure isolated");
        Console.WriteLine("PASS terrain overlay rules: shape, version, base revision, monotonic, copy, notifications");
    }

    // 查询不取锁：写线程在两个版本之间来回换，查询线程每次读到的都必须是某一个版本的完整结果（同一块内不混用版本）。
    private static void TestConcurrentQueries(byte[] packBytes)
    {
        using var pack = TerrainPack.Open(packBytes);
        var view = LoadAll(pack);
        var key = new ChunkKey(1, 0);
        var a = new ushort[4096];
        var b = new ushort[4096];
        a.AsSpan().Fill(1000);
        b.AsSpan().Fill(2000);
        int ha = pack.Meta.WorldHeight(1000), hb = pack.Meta.WorldHeight(2000);
        view.SetOverlay(key, pack.Meta.BaseRevision, 1, a, Materials(1));
        int stop = 0;
        long bad = 0, reads = 0;
        var readers = Enumerable.Range(0, 4).Select(_ => new Thread(() =>
        {
            while (Volatile.Read(ref stop) == 0)
            {
                // 块内一点（四个邻点都在块 (1,0)）：高度与材质必须出自同一版本。
                var g = view.Probe(6400 + 1234, 2345);
                bool ok = (g.Height == ha && g.Material == 1) || (g.Height == hb && g.Material == 2);
                if (!ok) Interlocked.Increment(ref bad);
                Interlocked.Increment(ref reads);
            }
        })).ToList();
        readers.ForEach(t => t.Start());
        // 至少换 2000 次，并且等查询线程读满 20000 次，避免写得太快时读线程还没跑起来就结束。
        long version = 2;
        for (; version < 2000 || Interlocked.Read(ref reads) < 20000; version++)
            view.SetOverlay(key, pack.Meta.BaseRevision, version, version % 2 == 0 ? b : a, Materials(version % 2 == 0 ? (byte)2 : (byte)1));
        Volatile.Write(ref stop, 1);
        readers.ForEach(t => t.Join());
        Check(bad == 0 && reads > 0, $"torn reads: {bad} of {reads}");
        Console.WriteLine($"PASS terrain concurrent queries: {reads} reads during {version - 2} overlay swaps, none mixed versions");
    }

    // 地图参数里的区域水位只读（RV-20261110-03，与 Go TestMetaIsACopyAtEveryPublicBoundary 对应）：区域配置只随原始地形包
    // 换版。构造时传入的列表事后再改不影响 TerrainMeta；把 WaterRegions 转回 IList（数组也是 IList）去写也改不到它。之后
    // 包头、视图的参数、视图按块算好的水位表与查询答的仍是同一条水位线。
    private static void TestMetaRegionsAreReadOnly()
    {
        var lake = new ChunkKey(1, 0);
        var changed = new TerrainWaterRegion(1, 0, 2, 1, 99999);
        var given = new[] { new TerrainWaterRegion(1, 0, 2, 1, 45000) };
        var meta = new TerrainMeta(1, 3, 2, 0, 40000, given);
        given[0] = changed;
        Check(meta.WaterLevelOf(lake) == 45000, "TerrainMeta keeps its own copy of the regions it was given");

        var flat = Deflate(Plain(new ushort[TerrainGeometry.ChunkPoints], Materials(1)));
        using var pack = TerrainPack.Open(BuildPack(1, 2, 1, 0, 40000, new[] { flat, flat }, new TerrainWaterRegion(1, 0, 2, 1, 45000)));
        var view = LoadAll(pack);
        foreach (var regions in new[] { meta.WaterRegions, pack.Meta.WaterRegions, view.Meta.WaterRegions })
        {
            // 只读集合拒绝写入（NotSupportedException）；拒绝不了的（直接交出内部数组）写进去，由下面的水位核对发现。
            if (!(regions is IList<TerrainWaterRegion> list)) continue;
            try { list[0] = changed; }
            catch (NotSupportedException) { }
        }
        var g = view.Probe(6400 + 1000, 1000);
        Check(meta.WaterLevelOf(lake) == 45000 && pack.Meta.WaterLevelOf(lake) == 45000 && view.Meta.WaterLevelOf(lake) == 45000 &&
              view.WaterLevelOf(lake) == 45000 && g.WaterLevel == 45000 && g.Water && g.Depth == 45000,
            $"water level of {lake}: meta {meta.WaterLevelOf(lake)}, pack {pack.Meta.WaterLevelOf(lake)}, view meta {view.Meta.WaterLevelOf(lake)}, view table {view.WaterLevelOf(lake)}, probe {g.WaterLevel}; all must stay 45000");
        Console.WriteLine("PASS terrain meta: water regions are read-only at TerrainMeta, TerrainPack.Meta and TerrainView.Meta");
    }

    // 水深是水位线与地面高度两个 int 之差，按 long 给出（RV-20261110-04）：高度基准可以为负、水位线是独立配置的 int，合法的
    // 地图参数下差值可以超出 int（最大 2³² − 1）。与 Go TestWaterDepthCoversTheWholeLevelRange 同一组数：2 × 1 块、地面原始值
    // 都是 0（地面高度就是高度基准），块 (0,0) 用全局水位、块 (1,0) 用区域水位；“无水”是 int.MinValue。
    private static void TestWaterDepthRange()
    {
        const int none = int.MinValue;
        var cases = new (string Name, int Base, int Global, int Lake, bool GlobalWet, bool LakeWet, long GlobalDepth, long LakeDepth)[]
        {
            ("ordinary levels", -10000, 0, 500, true, true, 10000, 10500),
            ("negative base, global level int.MaxValue (the review's reproduction)", -10000, int.MaxValue, none, true, false, int.MaxValue + 10000L, 0),
            ("regional level int.MaxValue, no global water", -10000, none, int.MaxValue, false, true, 0, int.MaxValue + 10000L),
            ("base int.MinValue: a difference of 2^32 - 1", int.MinValue, int.MaxValue, int.MinValue + 1, true, true, uint.MaxValue, 1),
            ("base int.MinValue, no water anywhere", int.MinValue, none, none, false, false, 0, 0),
            ("a level equal to the ground is not water", -10000, -10000, -9999, false, true, 0, 1),
        };
        var flat = Deflate(Plain(new ushort[TerrainGeometry.ChunkPoints], Materials(1)));
        foreach (var c in cases)
        {
            using var pack = TerrainPack.Open(BuildPack(1, 2, 1, c.Base, c.Global, new[] { flat, flat }, new TerrainWaterRegion(1, 0, 2, 1, c.Lake)));
            var view = LoadAll(pack);
            foreach (var (x, wet, depth) in new[] { (100L, c.GlobalWet, c.GlobalDepth), (6400L + 100, c.LakeWet, c.LakeDepth) })
            {
                var g = view.Probe(x, 100);
                long ground = g.Depth, query = view.WaterDepth(x, 100);
                Check(g.Height == c.Base && g.Water == wet && ground == depth && query == depth,
                    $"{c.Name}, x {x} (ground {g.Height}, level {g.WaterLevel}): water {g.Water}, TerrainGround.Depth {ground}, WaterDepth {query}; want {wet}, {depth}");
            }
        }
        // 不经视图直接构造的查询结果同一规则。
        long built = new TerrainGround(-10000, 0, true, int.MaxValue).Depth, dry = new TerrainGround(5, 0, false, 100).Depth;
        Check(built == int.MaxValue + 10000L && dry == 0, $"TerrainGround.Depth on constructed values: {built} and {dry}");
        Console.WriteLine($"PASS terrain water depth: {cases.Length} level / base combinations up to 2^32 - 1, global and regional, equal to Go");
    }
}
