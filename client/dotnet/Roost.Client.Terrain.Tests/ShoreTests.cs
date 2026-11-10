using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Text.Json;
using Roost.Client.Terrain;

internal static partial class Program
{
    private const int V65 = TerrainMesh.VerticesPerSide;
    private static readonly ShoreNeighbourLevels SameLevelAround = new ShoreNeighbourLevels(0, 0, 0, 0);

    private static int[] Grid(Func<int, int, int> height)
    {
        var g = new int[TerrainMesh.VertexCount];
        for (int j = 0; j < V65; j++)
            for (int i = 0; i < V65; i++) g[j * V65 + i] = height(i, j);
        return g;
    }

    // 合成网格上的权威规则（与 TerrainView.TryProbe 同一算法，高度步长取 1 厘米）：
    // h = round_half_up(双线性 num / S²)，水 ⟺ h < level。块 (0, 0) 内的点。
    private static bool GridWater(int[] g, int level, long x, long y)
    {
        long gx = x / 100, gy = y / 100, fx = x % 100, fy = y % 100;
        long h00 = g[gy * V65 + gx], h10 = g[gy * V65 + gx + 1], h01 = g[(gy + 1) * V65 + gx], h11 = g[(gy + 1) * V65 + gx + 1];
        long num = (h00 * (100 - fx) + h10 * fx) * (100 - fy) + (h01 * (100 - fx) + h11 * fx) * fy;
        long h = (num + 5000) / 10000; // 位移到非负后取整与原算法相同：测试高度都 ≥ 0
        return h < level;
    }

    private static double DistanceToShore(TerrainShore shore, double x, double y)
    {
        double best = double.PositiveInfinity;
        foreach (var chain in shore.Chains)
        {
            int last = chain.Closed ? chain.Count : chain.Count - 1;
            for (int k = 0; k < last; k++)
            {
                var a = shore.Points[chain.Start + k];
                var b = shore.Points[chain.Start + (k + 1) % chain.Count];
                best = Math.Min(best, SegmentDistance(a, b, x, y));
            }
        }
        return best;
    }

    private static double SegmentDistance(ShorePoint a, ShorePoint b, double x, double y)
    {
        double dx = b.X - a.X, dy = b.Y - a.Y, len2 = dx * dx + dy * dy;
        double t = len2 == 0 ? 0 : Math.Clamp(((x - a.X) * dx + (y - a.Y) * dy) / len2, 0, 1);
        double px = a.X + t * dx - x, py = a.Y + t * dy - y;
        return Math.Sqrt(px * px + py * py);
    }

    private static double SignedArea(TerrainShore shore, ShoreChain chain)
    {
        double area = 0;
        for (int k = 0; k < chain.Count; k++)
        {
            var a = shore.Points[chain.Start + k]; var b = shore.Points[chain.Start + (k + 1) % chain.Count];
            area += a.X * b.Y - b.X * a.Y;
        }
        return area / 2;
    }

    private static void CheckFinite(TerrainShore shore, string where)
    {
        foreach (var p in shore.Points.Concat(shore.PolygonPoints))
            Check(double.IsFinite(p.X) && double.IsFinite(p.Y), where + ": finite points");
        foreach (var poly in shore.Polygons)
        {
            double area = 0;
            for (int k = 0; k < poly.Count; k++)
            {
                var a = shore.PolygonPoints[poly.Start + k]; var b = shore.PolygonPoints[poly.Start + (k + 1) % poly.Count];
                area += a.X * b.Y - b.X * a.Y;
            }
            Check(area > 0, $"{where}: polygon in cell {poly.Cell} is counter-clockwise with positive area ({area / 2})");
        }
    }

    // 判定规则：等于水位是陆地、整边等高无交点、鞍点三种情形（连通 / 不连通 / 恰好相等）、线性格精确、无 NaN。
    private static void TestShoreRules()
    {
        const int level = 1000;
        var key = new ChunkKey(0, 0);

        // 一个采样恰好等于水位、右邻低 10：等于水位按陆地，交点在离它 1/20 格处（F = 1 → −19）。
        var g = Grid((i, j) => i == 5 && j == 5 ? level : i == 6 && j == 5 ? level - 10 : level + 100);
        var shore = TerrainShoreBuilder.Build(key, g, level, SameLevelAround, ShoreOptions.Default);
        CheckFinite(shore, "equal sample");
        Check(shore.Points.Contains(new ShorePoint(505, 500)), "crossing next to a sample at the water level is 1/20 of a cell away from it");
        Check(!shore.Points.Any(p => p.X == 500 && p.Y == 500), "a sample at the water level is not on the shoreline");
        Check(shore.Chains.Count == 1 && shore.Chains[0].Closed, "single water sample gives one closed chain");
        Check(SignedArea(shore, shore.Chains[0]) > 0, "closed chain runs with water on the left (counter-clockwise)");

        // 整边等于水位：没有水、没有交点。
        g = Grid((i, j) => (j == 20 && (i == 20 || i == 21)) ? level : level + 100);
        shore = TerrainShoreBuilder.Build(key, g, level, SameLevelAround, ShoreOptions.Default);
        Check(shore.Points.Count == 0 && shore.Polygons.Count == 0 && shore.Cells.ToArray().All(c => c == (byte)ShoreCellKind.Land), "edge at the water level is land, no crossing");
        // 整块等于水位：全是陆地。
        shore = TerrainShoreBuilder.Build(key, Grid((i, j) => level), level, SameLevelAround, ShoreOptions.Default);
        Check(shore.Points.Count == 0 && shore.Cells.ToArray().All(c => c == (byte)ShoreCellKind.Land), "plateau at the water level is land");

        // 鞍点格 (10, 10)：A = (10,10)、D = (11,11) 是水，B、C 是陆地。
        int Saddle(int a, int bc, int i, int j) => (i == 10 && j == 10) || (i == 11 && j == 11) ? a : (i == 11 && j == 10) || (i == 10 && j == 11) ? bc : level + 300;
        int PolygonsIn(TerrainShore s, int cell) => s.Polygons.Count(p => p.Cell == cell);
        // A = D = −100、B = C = +10：F = −199 / 21，AD − BC > 0、A < 0 → 鞍点是水，两个水角相连，格里一块多边形。
        shore = TerrainShoreBuilder.Build(key, Grid((i, j) => Saddle(level - 100, level + 10, i, j)), level, SameLevelAround, ShoreOptions.Default);
        CheckFinite(shore, "connected saddle");
        Check(TerrainShoreBuilder.SaddleCentreIsWater(-199, 21, 21, -199) && PolygonsIn(shore, 10 * 64 + 10) == 1, "connected saddle: one polygon");
        Check(shore.Chains.Count == 1 && shore.Chains[0].Closed, "connected saddle: both water samples are one water body with one closed shoreline");
        // A = D = −10、B = C = +100：鞍点是陆地，两个水角分开，两块多边形。
        shore = TerrainShoreBuilder.Build(key, Grid((i, j) => Saddle(level - 10, level + 100, i, j)), level, SameLevelAround, ShoreOptions.Default);
        CheckFinite(shore, "separated saddle");
        Check(!TerrainShoreBuilder.SaddleCentreIsWater(-19, 201, 201, -19) && PolygonsIn(shore, 10 * 64 + 10) == 2, "separated saddle: two polygons");
        Check(shore.Chains.Count == 2 && shore.Chains.All(c => c.Closed), "separated saddle: each water sample is its own water body with its own closed shoreline");
        // A = D = −1、B = C = 0：F = −1 / 1，AD = BC，鞍点恰在等高线上：按严格小于不是水，水角分开；
        // 等高线是过鞍点 (10.5m, 10.5m) 的两条直线，两段岸线都在鞍点折一次。
        shore = TerrainShoreBuilder.Build(key, Grid((i, j) => Saddle(level - 1, level, i, j)), level, SameLevelAround, ShoreOptions.Default);
        CheckFinite(shore, "tied saddle");
        Check(!TerrainShoreBuilder.SaddleCentreIsWater(-1, 1, 1, -1) && PolygonsIn(shore, 10 * 64 + 10) == 2, "tied saddle: water corners separated");
        Check(shore.Points.Count(p => p == new ShorePoint(1050, 1050)) == 2, "tied saddle: both pieces bend exactly at the saddle point");
        // 同样四个角，结论与在哪个块、哪一格算无关（只看四角值）：把格平移到 (40, 30) 再算一次。
        var moved = TerrainShoreBuilder.Build(key, Grid((i, j) => Saddle(level - 1, level, i - 30, j - 20)), level, SameLevelAround, ShoreOptions.Default);
        Check(moved.Polygons.Count(p => p.Cell == 30 * 64 + 40) == 2, "tied saddle decision does not depend on position");

        // 线性格（d = 0，斜坡）：岸线是直线，误差 0，不细分。
        shore = TerrainShoreBuilder.Build(key, Grid((i, j) => level - 500 + i * 13 + j * 7), level, SameLevelAround, ShoreOptions.Default);
        CheckFinite(shore, "ramp");
        Check(shore.MaxDeviation == 0 && shore.DepthLimited == 0, "planar ramp shoreline is exact");
        Check(shore.Chains.Count == 1 && !shore.Chains[0].Closed, "ramp shoreline crosses the whole chunk");
        // 整块是水：没有岸线；左 / 下是地图边、右邻水位不同、上邻同水位 → 左、下、右三条整边侧壁，上边没有。
        shore = TerrainShoreBuilder.Build(key, Grid((i, j) => level - 50), level, new ShoreNeighbourLevels(null, level + 30, null, level), ShoreOptions.Default);
        Check(shore.Chains.Count == 0 && shore.Cells.ToArray().All(c => c == (byte)ShoreCellKind.Water), "all-water chunk has no shoreline");
        Check(shore.Seams.Count == 3 && shore.Seams.All(m => m.From == 0 && m.To == 6400)
            && shore.Seams.Count(m => m.NeighbourLevel == null) == 2 && shore.Seams.Single(m => m.Side == TerrainSide.Right).NeighbourLevel == level + 30
            && !shore.Seams.Any(m => m.Side == TerrainSide.Top), "all-water chunk: map-edge and level seams, none towards a same-level neighbour");
        Check(shore.AboveWater[0] == -50, "above-water field is height minus level");
        // 没有水的块：全空。
        shore = TerrainShoreBuilder.Build(key, Grid((i, j) => -5000), int.MinValue, SameLevelAround, ShoreOptions.Default);
        Check(!shore.HasWater && shore.Points.Count == 0 && shore.AboveWater.Length == 0 && shore.Seams.Count == 0, "no-water chunk is empty");
        Throws<ArgumentOutOfRangeException>(() => new ShoreOptions(0, 4), "zero tolerance");
        Throws<ArgumentOutOfRangeException>(() => new ShoreOptions(1, ShoreOptions.MaxSubdivisionDepth + 1), "depth above the limit");
        Console.WriteLine("PASS terrain shore rules: equal-to-level samples and edges are land, saddle connected / separated / tied, planar exact, no NaN");
    }

    // 块内圆形池塘（高度不是 10 的倍数，覆盖任意整数厘米）：一条闭合、逆时针的岸线；精确等高线上的密集点都在折线
    // MaxDeviation 以内；1cm 格点上视觉分类与权威规则只在误差带内不同；细分参数生效且误差如实报告。
    private static void TestShoreCircle()
    {
        const int level = 2000;
        var key = new ChunkKey(0, 0);
        // 圆心 (31.37m, 29.71m)，半径 7.3m，碗底 −180cm。
        int Bowl(int i, int j)
        {
            double dx = i * 100 - 3137, dy = j * 100 - 2971;
            return (int)Math.Min(level + 120, Math.Round(level - 180 + 300 * (dx * dx + dy * dy) / (730.0 * 730.0)));
        }
        var g = Grid(Bowl);
        var fine = TerrainShoreBuilder.Build(key, g, level, SameLevelAround, new ShoreOptions(0.25, 8));
        var coarse = TerrainShoreBuilder.Build(key, g, level, SameLevelAround, new ShoreOptions(1000, 0));
        var shallow = TerrainShoreBuilder.Build(key, g, level, SameLevelAround, new ShoreOptions(0.01, 1));
        CheckFinite(fine, "circle");
        Check(fine.Chains.Count == 1 && fine.Chains[0].Closed && SignedArea(fine, fine.Chains[0]) > 0, "circle: one closed counter-clockwise chain");
        Check(fine.MaxDeviation <= 0.25 && fine.DepthLimited == 0, $"circle: tolerance met within depth (max {fine.MaxDeviation})");
        Check(coarse.Points.Count < fine.Points.Count && coarse.MaxDeviation > 0.25, $"circle: coarse ({coarse.Points.Count} points, {coarse.MaxDeviation:F3}cm) vs fine ({fine.Points.Count} points)");
        Check(shallow.DepthLimited > 0 && shallow.MaxDeviation > 0.01, "circle: depth limit reported");
        Check(fine.Seams.Count == 0, "circle: no seams inside a same-level neighbourhood");

        // 精确等高线（f = 0，四角 F 的双线性）上逐格取密集点，到折线的距离不超过 MaxDeviation。
        double worst = 0;
        int samples = 0;
        foreach (var s in new[] { fine, coarse, shallow })
        {
            for (int j = 0; j < 64; j++)
                for (int i = 0; i < 64; i++)
                {
                    if (s.CellAt(i, j) != ShoreCellKind.Shore) continue;
                    long a = TerrainShoreBuilder.Field(g[j * V65 + i], level), b = TerrainShoreBuilder.Field(g[j * V65 + i + 1], level);
                    long c = TerrainShoreBuilder.Field(g[(j + 1) * V65 + i], level), d = TerrainShoreBuilder.Field(g[(j + 1) * V65 + i + 1], level);
                    for (int k = 1; k < 400; k++)
                    {
                        double u = k / 400.0;
                        // f(u, v) = a + (b − a)u + ((c − a) + (a − b − c + d)u)v = 0
                        double den = (c - a) + (double)(a - b - c + d) * u, v = -(a + (b - a) * u) / den;
                        if (den == 0 || !(v > 0 && v < 1)) continue;
                        double dist = DistanceToShore(s, i * 100 + u * 100, j * 100 + v * 100);
                        worst = Math.Max(worst, dist - s.MaxDeviation);
                        Check(dist <= s.MaxDeviation + 1e-6, $"exact contour point in cell ({i},{j}) is {dist}cm from the shoreline, bound {s.MaxDeviation}");
                        samples++;
                    }
                }
        }

        // 1cm 格点：视觉分类（多边形）与权威规则不同的点，都在岸线 MaxDeviation 以内。
        int mismatches = 0, points = 0;
        for (long y = 0; y < 6400; y++)
            for (long x = 0; x < 6400; x++)
            {
                int i = (int)(x / 100), j = (int)(y / 100);
                if (fine.CellAt(i, j) != ShoreCellKind.Shore && (x % 37 != 0 || y % 37 != 0)) continue;
                points++;
                bool truth = GridWater(g, level, x, y);
                if (fine.ContainsWater(x, y) == truth) continue;
                mismatches++;
                double dist = DistanceToShore(fine, x, y);
                Check(dist <= fine.MaxDeviation + 1e-6, $"circle point ({x}, {y}) classified {!truth} visually, {truth} authoritatively, {dist}cm from the shoreline (bound {fine.MaxDeviation})");
            }
        Console.WriteLine($"PASS terrain shore circle: {fine.Points.Count} points (coarse {coarse.Points.Count}), max deviation {fine.MaxDeviation:F4}cm " +
            $"(coarse {coarse.MaxDeviation:F3}cm), {samples} exact contour samples within bound, {points} lattice points, {mismatches} visual/authoritative differences all within the bound");
    }

    // Go 生成的岸线金样（client/spec/terrain-shore.*）：窗口里的每个点 C# 权威查询与 Go 相同；每块的视觉分类只在误差带内
    // 与权威查询不同（窗口点与全图岸线格的 1cm 格点）；块边上的岸线端点在两块之间逐位相接（同水位），或落在本块的
    // 侧壁 / 地图边线段的端点上（水位不同 / 地图边）；四块交点、两块公共边、区域水位边界、无水区域边界都覆盖。
    private static void TestShoreGolden(string spec)
    {
        var golden = JsonDocument.Parse(File.ReadAllText(Path.Combine(spec, "terrain-shore.json"))).RootElement;
        byte[] bytes = File.ReadAllBytes(Path.Combine(spec, golden.GetProperty("pack").GetString()!));
        Check(Sha(bytes) == golden.GetProperty("pack_sha256").GetString(), "shore pack SHA-256");
        using var pack = TerrainPack.Open(bytes);
        var view = LoadAll(pack);
        var meta = pack.Meta;
        var shores = new Dictionary<ChunkKey, TerrainShore>();
        for (int cy = 0; cy < meta.ChunksY; cy++)
            for (int cx = 0; cx < meta.ChunksX; cx++)
            {
                var key = new ChunkKey(cx, cy);
                Check(TerrainShoreBuilder.TryBuild(view, key, ShoreOptions.Default, out var s, out _), "shore " + key);
                CheckFinite(s!, "golden " + key);
                Check(s!.DepthLimited == 0 && s.MaxDeviation <= ShoreOptions.Default.Tolerance, $"golden {key}: tolerance met (max {s.MaxDeviation})");
                shores[key] = s;
            }
        TerrainShore ShoreAt(long x, long y) => shores[TerrainGeometry.ChunkOf(x, y)];

        // 窗口：Go 权威结果 = C# 权威结果；视觉分类只在误差带内不同。
        int windowPoints = 0, windowMismatch = 0;
        foreach (var w in golden.GetProperty("windows").EnumerateArray())
        {
            string name = w.GetProperty("name").GetString()!;
            long x0 = w.GetProperty("x0").GetInt64(), y0 = w.GetProperty("y0").GetInt64(), step = w.GetProperty("step").GetInt64();
            int columns = w.GetProperty("columns").GetInt32(), water = 0, r = 0;
            foreach (var row in w.GetProperty("rows").EnumerateArray())
            {
                string hex = row.GetString()!;
                for (int c = 0; c < columns; c++)
                {
                    long x = x0 + c * step, y = y0 + r * step;
                    bool goWater = (Convert.ToInt32(hex[c / 4].ToString(), 16) >> (3 - c % 4) & 1) == 1;
                    if (goWater) water++;
                    Check(view.IsWater(x, y) == goWater, $"{name}: C# IsWater ({x}, {y}) equals Go");
                    windowPoints++;
                    var s = ShoreAt(x, y);
                    if (s.ContainsWater(x, y) == goWater) continue;
                    windowMismatch++;
                    double dist = DistanceToShore(s, x, y);
                    Check(dist <= s.MaxDeviation + 1e-6, $"{name}: ({x}, {y}) visual {!goWater}, Go {goWater}, {dist}cm from the shoreline (bound {s.MaxDeviation})");
                }
                r++;
            }
            Check(water == w.GetProperty("water").GetInt32(), name + ": water count");
        }

        // 全图：岸线格的每个 1cm 点、其余格每 13cm 一点，视觉分类与 C# 权威查询（已与 Go 逐点一致）比较。
        int sweep = 0, sweepMismatch = 0;
        foreach (var pair in shores)
        {
            var s = pair.Value;
            for (int j = 0; j < 64; j++)
                for (int i = 0; i < 64; i++)
                {
                    bool shoreCell = s.CellAt(i, j) == ShoreCellKind.Shore;
                    int step = shoreCell ? 1 : 13;
                    long ox = pair.Key.OriginX + i * 100, oy = pair.Key.OriginY + j * 100;
                    for (long y = oy; y < oy + 100; y += step)
                        for (long x = ox; x < ox + 100; x += step)
                        {
                            sweep++;
                            bool truth = view.IsWater(x, y);
                            if (s.ContainsWater(x, y) == truth) continue;
                            Check(shoreCell, $"({x}, {y}) in a {s.CellAt(i, j)} cell of {pair.Key} disagrees with the authoritative query");
                            sweepMismatch++;
                            double dist = DistanceToShore(s, x, y);
                            Check(dist <= s.MaxDeviation + 1e-6, $"({x}, {y}) in {pair.Key}: visual {!truth}, authoritative {truth}, {dist}cm from the shoreline (bound {s.MaxDeviation})");
                        }
                }
        }

        // 块边：开放岸线的端点要么与同水位邻块的端点逐位相接（本块的起点 = 邻块的终点），要么落在本块侧壁线段的端点上。
        int joined = 0, sealedEnds = 0, corners = 0, fourChunk = 0;
        foreach (var pair in shores)
        {
            var s = pair.Value;
            foreach (var chain in s.Chains)
            {
                if (chain.Closed) continue;
                foreach (bool start in new[] { true, false })
                {
                    var p = s.Points[chain.Start + (start ? 0 : chain.Count - 1)];
                    var side = SideOf(pair.Key, p);
                    var other = Neighbour(pair.Key, side);
                    if (meta.Contains(other) && view.WaterLevelOf(other) == s.WaterLevel)
                    {
                        var n = shores[other];
                        bool found = n.Chains.Any(c => !c.Closed && n.Points[c.Start + (start ? c.Count - 1 : 0)] == p);
                        Check(found, $"chain {(start ? "start" : "end")} {p} of {pair.Key} continues in {other}");
                        joined++;
                    }
                    else
                    {
                        Check(s.Seams.Any(m => m.Side == side && (SeamEnd(m, true) == p || SeamEnd(m, false) == p)), $"chain end {p} of {pair.Key} closes on a seam ({side})");
                        sealedEnds++;
                    }
                }
            }
            foreach (var seam in s.Seams)
                foreach (bool from in new[] { true, false })
                {
                    var p = SeamEnd(seam, from);
                    bool atSample = (seam.Side == TerrainSide.Left || seam.Side == TerrainSide.Right ? p.Y : p.X) % 100 == 0;
                    if (atSample) { corners++; continue; }
                    Check(s.Chains.Any(c => !c.Closed && (s.Points[c.Start] == p || s.Points[c.Start + c.Count - 1] == p)), $"seam end {p} of {pair.Key} is a shoreline end");
                }
        }
        // 圆心在四块交点附近的池塘：四块都有岸线，且都在交点所在的四块里接上。
        foreach (var k in new[] { new ChunkKey(0, 0), new ChunkKey(1, 0), new ChunkKey(0, 1), new ChunkKey(1, 1) })
            if (shores[k].Chains.Any(c => !c.Closed)) fourChunk++;
        Check(fourChunk == 4, "pond around the four-chunk corner reaches all four chunks");
        Check(shores[new ChunkKey(1, 2)].Seams.Any(m => m.Side == TerrainSide.Right && m.NeighbourLevel == 50), "regional boundary seam on the low side");
        Check(shores[new ChunkKey(2, 2)].Seams.Any(m => m.Side == TerrainSide.Left && m.NeighbourLevel == 0), "regional boundary seam on the high side");
        Check(shores[new ChunkKey(0, 1)].Seams.Any(m => m.Side == TerrainSide.Top && m.NeighbourLevel == int.MinValue), "no-water region seam");
        Check(!shores[new ChunkKey(0, 2)].HasWater, "no-water region has no shore");
        Check(shores.Values.Sum(s => s.Seams.Count(m => m.NeighbourLevel == null)) == 0, "no water reaches the map edge in this map");
        Console.WriteLine($"PASS terrain shore golden: {windowPoints} Go window points equal ({windowMismatch} visual differences within the bound), " +
            $"{sweep} sweep points ({sweepMismatch} within the bound), {joined} chain ends joined across chunks, {sealedEnds} closed on seams, " +
            $"{corners} seam ends at samples, max deviation {shores.Values.Max(s => s.MaxDeviation):F4}cm");
    }

    private static ShorePoint SeamEnd(ShoreSeam seam, bool from)
    {
        double along = from ? seam.From : seam.To;
        return seam.Side == TerrainSide.Left || seam.Side == TerrainSide.Right ? new ShorePoint(seam.Line, along) : new ShorePoint(along, seam.Line);
    }

    private static TerrainSide SideOf(ChunkKey key, ShorePoint p)
    {
        if (p.X == key.OriginX) return TerrainSide.Left;
        if (p.X == key.OriginX + TerrainGeometry.ChunkSize) return TerrainSide.Right;
        if (p.Y == key.OriginY) return TerrainSide.Bottom;
        if (p.Y == key.OriginY + TerrainGeometry.ChunkSize) return TerrainSide.Top;
        throw new Exception($"FAIL open chain end {p} of {key} is not on the chunk border");
    }

    private static ChunkKey Neighbour(ChunkKey key, TerrainSide side) => side switch
    {
        TerrainSide.Left => new ChunkKey(key.X - 1, key.Y),
        TerrainSide.Right => new ChunkKey(key.X + 1, key.Y),
        TerrainSide.Bottom => new ChunkKey(key.X, key.Y - 1),
        _ => new ChunkKey(key.X, key.Y + 1),
    };
}
