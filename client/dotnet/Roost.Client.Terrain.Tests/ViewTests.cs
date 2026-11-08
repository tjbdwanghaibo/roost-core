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
        var wide = heights.Select(h => (uint)h).ToList();
        wide[100] = 65536;
        Throws<TerrainInvalidChunkException>(() => view.SetOverlay(key, rev, 1, wide, materials), "uint32 height above 65535");
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
        view.SetOverlay(key, rev, 3, Ramp().Select(h => (uint)h).ToList(), Materials(6));
        Check(changes.Count == 3 && changes[2].Version == 3, "DAO-shaped overlay (uint32 list)");
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
}
