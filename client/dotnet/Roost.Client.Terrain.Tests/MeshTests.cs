using System;
using System.Linq;
using Roost.Client.Terrain;

internal static partial class Program
{
    // 顶点网格：每个地图内的顶点等于 Height 在该点的值；相邻块共享边上的顶点逐个相同；地图边上复制最后一列 / 一行；
    // 缺块时报缺的那一块；改了一块的第 0 列后，左邻块网格的右边跟着变（接缝在编辑后仍连续）。
    private static void TestMesh(byte[] packBytes)
    {
        using var pack = TerrainPack.Open(packBytes);
        var meta = pack.Meta;
        var view = LoadAll(pack);
        const int side = TerrainMesh.VerticesPerSide;
        var grids = new int[meta.ChunksX, meta.ChunksY][];
        int compared = 0;
        for (int cy = 0; cy < meta.ChunksY; cy++)
            for (int cx = 0; cx < meta.ChunksX; cx++)
            {
                var grid = new int[TerrainMesh.VertexCount];
                Check(view.TryBuildHeightGrid(new ChunkKey(cx, cy), grid, out _), $"grid ({cx},{cy})");
                grids[cx, cy] = grid;
                for (int j = 0; j < side; j++)
                    for (int i = 0; i < side; i++)
                    {
                        long x = ((long)cx * 64 + i) * 100, y = ((long)cy * 64 + j) * 100;
                        if (meta.ContainsPoint(x, y))
                        {
                            Check(grid[j * side + i] == view.Height(x, y), $"vertex ({cx},{cy}) [{i},{j}] equals Height");
                            compared++;
                        }
                    }
                // 地图右 / 上边：复制本块最后一列 / 一行。
                if (cx == meta.ChunksX - 1)
                    for (int j = 0; j < side; j++) Check(grid[j * side + 64] == grid[j * side + 63], $"right map edge replicated ({cx},{cy}) row {j}");
                if (cy == meta.ChunksY - 1)
                    for (int i = 0; i < side; i++) Check(grid[64 * side + i] == grid[63 * side + i], $"top map edge replicated ({cx},{cy}) column {i}");
            }
        CheckSeams(meta, grids);

        // 缺块：没装右邻块 / 自己时报缺的那一块；地图右边的块不需要右邻块。
        var partial = new TerrainView(pack);
        partial.LoadBase(new ChunkKey(0, 0));
        var scratch = new int[TerrainMesh.VertexCount];
        Check(!partial.TryBuildHeightGrid(new ChunkKey(0, 0), scratch, out var missing) && missing == new ChunkKey(1, 0), "missing right neighbour");
        partial.LoadBase(new ChunkKey(1, 0));
        Check(!partial.TryBuildHeightGrid(new ChunkKey(0, 0), scratch, out missing) && missing == new ChunkKey(0, 1), "missing top neighbour");
        partial.LoadBase(new ChunkKey(0, 1));
        Check(!partial.TryBuildHeightGrid(new ChunkKey(0, 0), scratch, out missing) && missing == new ChunkKey(1, 1), "missing top-right neighbour");
        Check(!partial.TryBuildHeightGrid(new ChunkKey(2, 0), scratch, out missing) && missing == new ChunkKey(2, 0), "missing self");
        partial.LoadBase(new ChunkKey(2, 0));
        Check(!partial.TryBuildHeightGrid(new ChunkKey(2, 0), scratch, out missing) && missing == new ChunkKey(2, 1), "right map edge chunk needs only its top neighbour");
        partial.LoadBase(new ChunkKey(2, 1));
        Check(partial.TryBuildHeightGrid(new ChunkKey(2, 1), scratch, out _) && scratch.AsSpan().SequenceEqual(grids[2, 1]), "top-right map corner needs only itself");
        Throws<TerrainOutOfMapException>(() => partial.TryBuildHeightGrid(new ChunkKey(0, 2), scratch, out _), "grid outside the map");
        Throws<TerrainNotLoadedException>(() => TerrainMesh.BuildHeightGrid(meta, new ChunkKey(0, 0), view.GetChunk(new ChunkKey(0, 0))!.Heights,
            default, default, default, scratch), "pure function with a missing neighbour");
        Throws<ArgumentException>(() => TerrainMesh.BuildHeightGrid(meta, new ChunkKey(2, 1), new ushort[10], default, default, default, scratch), "short chunk data");

        // 编辑块 (1,0) 的第 0 列：块 (0,0) 网格的右边（由 ChunkChanged 的 MeshesToRebuild 指出）随之变化，接缝仍连续。
        var key = new ChunkKey(1, 0);
        var edited = view.GetChunk(key)!.Heights.ToArray();
        for (int j = 0; j < 64; j++) edited[j * 64] = (ushort)(1000 + j * 7);
        TerrainChunkChange? change = null;
        view.ChunkChanged += c => change = c;
        view.SetOverlay(key, meta.BaseRevision, 1, edited, view.GetChunk(key)!.Materials);
        Check(change != null && change.MeshesToRebuild.Count == 2 && change.MeshesToRebuild.Contains(new ChunkKey(0, 0)), "left neighbour listed for rebuild");
        foreach (var k in change!.MeshesToRebuild)
            Check(view.TryBuildHeightGrid(k, grids[k.X, k.Y], out _), "rebuild " + k);
        for (int j = 0; j < 64; j++)
            Check(grids[0, 0][j * side + 64] == meta.WorldHeight((ushort)(1000 + j * 7)), $"left neighbour seam follows the edit, row {j}");
        CheckSeams(meta, grids);
        Console.WriteLine($"PASS terrain mesh: {compared} vertices equal Height, seams continuous before and after an edge edit, map edge, missing chunks");
    }

    private static void CheckSeams(TerrainMeta meta, int[,][] grids)
    {
        const int side = TerrainMesh.VerticesPerSide;
        for (int cy = 0; cy < meta.ChunksY; cy++)
            for (int cx = 0; cx < meta.ChunksX; cx++)
            {
                if (cx + 1 < meta.ChunksX)
                    for (int j = 0; j < side; j++)
                        Check(grids[cx, cy][j * side + 64] == grids[cx + 1, cy][j * side], $"seam ({cx},{cy})|({cx + 1},{cy}) row {j}");
                if (cy + 1 < meta.ChunksY)
                    for (int i = 0; i < side; i++)
                        Check(grids[cx, cy][64 * side + i] == grids[cx, cy + 1][i], $"seam ({cx},{cy})/({cx},{cy + 1}) column {i}");
            }
    }
}
