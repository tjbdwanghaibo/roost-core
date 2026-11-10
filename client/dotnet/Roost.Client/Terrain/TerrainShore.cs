using System;
using System.Collections.Generic;

namespace Roost.Client.Terrain
{
    // 岸线上的一点（世界坐标，厘米）。块边上的交点由 TerrainShoreBuilder.Crossing 按“边的规范方向”计算，相邻两格、
    // 相邻两块对同一条边算出的值逐位相同，所以跨块的岸线端点可以用 == 直接对接。
    public readonly struct ShorePoint : IEquatable<ShorePoint>
    {
        public double X { get; }
        public double Y { get; }
        public ShorePoint(double x, double y) { X = x; Y = y; }
        public bool Equals(ShorePoint other) => X.Equals(other.X) && Y.Equals(other.Y);
        public override bool Equals(object? obj) => obj is ShorePoint other && Equals(other);
        public override int GetHashCode() => X.GetHashCode() * 397 ^ Y.GetHashCode();
        public static bool operator ==(ShorePoint a, ShorePoint b) => a.Equals(b);
        public static bool operator !=(ShorePoint a, ShorePoint b) => !a.Equals(b);
        public override string ToString() => FormattableString.Invariant($"({X:R}, {Y:R})");
    }

    // 一条岸线折线：TerrainShore.Points[Start .. Start+Count)。走向固定为“水在左手边”（俯视、X 向右、Y 向上），
    // 所以陆地一侧是右手边，泡沫 / 湿润带按这个方向外扩。Closed 表示最后一点连回第一点（整条在块内闭合，例如块内的小池塘）；
    // 不闭合的折线两端都在块边上，由相邻块（同一水位时）的折线接上，或在水位不同的块边 / 地图边上结束（见 ShoreSeam）。
    public readonly struct ShoreChain
    {
        public int Start { get; }
        public int Count { get; }
        public bool Closed { get; }
        public ShoreChain(int start, int count, bool closed) { Start = start; Count = count; Closed = closed; }
    }

    public enum TerrainSide { Left, Right, Bottom, Top }

    // 块边上一段“本块这一侧是水、但另一侧不是同一个水面”的线段：相邻块水位不同（区域水位边界，含另一侧没有水），
    // 或这条边是地图边（另一侧没有地形）。水面在这里不连续，表现层在这里补竖直的水面侧壁 / 瀑布 / 地图边的收口。
    // 线段在世界坐标的直线 Line 上（Left / Right 边是 x = Line，Bottom / Top 边是 y = Line），沿边的范围是 [From, To]（From < To）。
    // 端点与 ShoreChain 在这条边上的端点逐位相同：本块的“岸线 + 侧壁 + 块内其他边”围成本块水面的边界，不会留缝。
    public readonly struct ShoreSeam
    {
        public TerrainSide Side { get; }
        public long Line { get; }
        public double From { get; }
        public double To { get; }
        // 另一侧块的水位（int.MinValue = 没有水）；地图边上为 null。
        public int? NeighbourLevel { get; }
        public ShoreSeam(TerrainSide side, long line, double from, double to, int? neighbourLevel)
        { Side = side; Line = line; From = from; To = to; NeighbourLevel = neighbourLevel; }
    }

    public enum ShoreCellKind : byte { Land = 0, Water = 1, Shore = 2 }

    // 一格（1m × 1m）里的一块水面多边形：TerrainShore.PolygonPoints[Start .. Start+Count)，逆时针（俯视），
    // 边界由格边的一段与岸线组成；Cell = j·64 + i。只给 Shore 格（四角有水有陆）；整格是水的格只在 Cells 里标 Water。
    // 一格至多两块（鞍点格水不连通时）。
    public readonly struct ShorePolygon
    {
        public int Cell { get; }
        public int Start { get; }
        public int Count { get; }
        public ShorePolygon(int cell, int start, int count) { Cell = cell; Start = start; Count = count; }
    }

    // 岸线细分参数。Tolerance：岸线折线与精确等高线（见 TerrainShoreBuilder 的说明）之间允许的最大距离（厘米）；
    // MaxDepth：每段等高线最多二分的层数（至多 2^MaxDepth 段），到上限仍超差时记入 TerrainShore.DepthLimited，不再细分。
    public sealed class ShoreOptions
    {
        public static readonly ShoreOptions Default = new ShoreOptions(1.0, 4);
        public const int MaxSubdivisionDepth = 8;

        public double Tolerance { get; }
        public int MaxDepth { get; }

        public ShoreOptions(double tolerance, int maxDepth)
        {
            if (!(tolerance > 0) || double.IsInfinity(tolerance)) throw new ArgumentOutOfRangeException(nameof(tolerance), "tolerance must be a positive number of centimetres");
            if (maxDepth < 0 || maxDepth > MaxSubdivisionDepth) throw new ArgumentOutOfRangeException(nameof(maxDepth), $"depth must be in [0, {MaxSubdivisionDepth}]");
            Tolerance = tolerance; MaxDepth = maxDepth;
        }
    }

    // 四个共边邻块的水位；null 表示那一侧是地图外。用来决定哪些块边是 ShoreSeam。
    public readonly struct ShoreNeighbourLevels
    {
        public int? Left { get; }
        public int? Right { get; }
        public int? Bottom { get; }
        public int? Top { get; }
        public ShoreNeighbourLevels(int? left, int? right, int? bottom, int? top) { Left = left; Right = right; Bottom = bottom; Top = top; }

        public int? Of(TerrainSide side) => side switch
        {
            TerrainSide.Left => Left,
            TerrainSide.Right => Right,
            TerrainSide.Bottom => Bottom,
            _ => Top,
        };

        // 视图里块 key 的四个邻块水位（地图外为 null）。
        public static ShoreNeighbourLevels Of(TerrainView view, ChunkKey key)
        {
            int? At(int x, int y)
            {
                var k = new ChunkKey(x, y);
                return view.Meta.Contains(k) ? view.WaterLevelOf(k) : (int?)null;
            }
            return new ShoreNeighbourLevels(At(key.X - 1, key.Y), At(key.X + 1, key.Y), At(key.X, key.Y - 1), At(key.X, key.Y + 1));
        }
    }

    // 一块的岸线与水面数据（只出数据，不做渲染）。不可变，可以跨线程交给渲染侧。
    public sealed class TerrainShore
    {
        private readonly byte[] cells;
        private readonly int[] aboveWater;
        private readonly int[] polygonFirst;
        private readonly byte[] polygonCount;

        public ChunkKey Key { get; }
        // 块的水位线（区域水位或全局水位）；int.MinValue 表示这块没有水（其余字段为空，Cells 全是 Land）。
        public int WaterLevel { get; }
        public bool HasWater => WaterLevel != int.MinValue;
        public IReadOnlyList<ShorePoint> Points { get; }
        public IReadOnlyList<ShoreChain> Chains { get; }
        public IReadOnlyList<ShorePoint> PolygonPoints { get; }
        public IReadOnlyList<ShorePolygon> Polygons { get; }
        public IReadOnlyList<ShoreSeam> Seams { get; }
        // 64 × 64 格的分类（下标 j·64 + i，值是 ShoreCellKind）。
        public ReadOnlySpan<byte> Cells => cells;
        // 65 × 65 顶点的“高出水位”（厘米）= 顶点世界高度 − WaterLevel，饱和到 int；负数是水深。与 TerrainMesh 顶点一一对应，
        // 相邻块共享边上逐个相同。泡沫（浅水）、湿润带（刚出水的陆地）、岸边材质渐变按它在三角形内插值。没有水时为空。
        public ReadOnlySpan<int> AboveWater => aboveWater;
        // 所有最终折线段与精确等高线之间的最大距离（厘米，逐段精确计算，见 TerrainShoreBuilder）。
        public double MaxDeviation { get; }
        // 细分到 MaxDepth 仍超过 Tolerance 的等高线段数。
        public int DepthLimited { get; }

        internal TerrainShore(ChunkKey key, int level, List<ShorePoint> points, List<ShoreChain> chains, List<ShorePoint> polygonPoints,
            List<ShorePolygon> polygons, List<ShoreSeam> seams, byte[] cells, int[] aboveWater, double maxDeviation, int depthLimited)
        {
            Key = key; WaterLevel = level; Points = points.AsReadOnly(); Chains = chains.AsReadOnly(); PolygonPoints = polygonPoints.AsReadOnly();
            Polygons = polygons.AsReadOnly(); Seams = seams.AsReadOnly(); this.cells = cells; this.aboveWater = aboveWater;
            MaxDeviation = maxDeviation; DepthLimited = depthLimited;
            polygonFirst = new int[TerrainGeometry.ChunkPoints];
            polygonCount = new byte[TerrainGeometry.ChunkPoints];
            for (int p = polygons.Count - 1; p >= 0; p--) { polygonFirst[polygons[p].Cell] = p; polygonCount[polygons[p].Cell]++; }
        }

        public ShoreCellKind CellAt(int i, int j) => (ShoreCellKind)cells[j * TerrainGeometry.ChunkSamples + i];

        // 表现层的水陆分类：点 (x, y)（世界坐标，须在本块内）在本块的水面多边形里（整格水或 Shore 格的多边形内）。
        // 它是“视觉”判定，与权威查询 TerrainView.IsWater 只在岸线附近（距离不超过 MaxDeviation）可能不同；
        // 游泳、移动、放置等一律用权威查询，不用这个。
        public bool ContainsWater(double x, double y)
        {
            double lx = x - Key.OriginX, ly = y - Key.OriginY;
            if (!(lx >= 0 && ly >= 0 && lx < TerrainGeometry.ChunkSize && ly < TerrainGeometry.ChunkSize))
                throw new ArgumentOutOfRangeException(nameof(x), $"point ({x}, {y}) is outside chunk {Key}");
            int i = (int)(lx / TerrainGeometry.SampleSpacing), j = (int)(ly / TerrainGeometry.SampleSpacing);
            int cell = j * TerrainGeometry.ChunkSamples + i;
            switch ((ShoreCellKind)cells[cell])
            {
                case ShoreCellKind.Water: return true;
                case ShoreCellKind.Land: return false;
            }
            // 正好在格的左边 / 下边上：双线性沿格边是线性的，直接按两端 F 插值（与多边形沿格边的那段一致，且没有射线法在边上的歧义）。
            double fx = lx / TerrainGeometry.SampleSpacing - i, fy = ly / TerrainGeometry.SampleSpacing - j;
            if (fx == 0 || fy == 0)
            {
                const int v = TerrainMesh.VerticesPerSide;
                double f0 = 2.0 * aboveWater[j * v + i] + 1;
                double f1 = fx == 0 ? 2.0 * aboveWater[(j + 1) * v + i] + 1 : 2.0 * aboveWater[j * v + i + 1] + 1;
                double t = fx == 0 ? fy : fx;
                return f0 * (1 - t) + f1 * t < 0;
            }
            for (int p = polygonFirst[cell], end = p + polygonCount[cell]; p < end; p++)
                if (Inside(Polygons[p], x, y)) return true;
            return false;
        }

        // 射线法（向 +X）；点正好在多边形边上时结果取决于浮点，调用方只在误差带外依赖它。
        private bool Inside(ShorePolygon polygon, double x, double y)
        {
            bool inside = false;
            for (int a = polygon.Start, b = polygon.Start + polygon.Count - 1; a < polygon.Start + polygon.Count; b = a++)
            {
                var pa = PolygonPoints[a]; var pb = PolygonPoints[b];
                if ((pa.Y > y) != (pb.Y > y) && x < (pb.X - pa.X) * (y - pa.Y) / (pb.Y - pa.Y) + pa.X) inside = !inside;
            }
            return inside;
        }
    }

    // 岸线生成（可编辑地形 T03，方案 §31）。纯函数：输入一块的 65 × 65 顶点世界高度（TerrainMesh.TryBuildHeightGrid 的输出，
    // 右 / 上一列 / 行来自邻块，地图边复制）、本块水位与四个邻块水位，输出 TerrainShore。
    //
    // 与权威水域规则的关系（服务端 gameplay/terrain View.probe 与 TerrainView.TryProbe 相同）：
    //   点 p（整数厘米）是水 ⟺ round_half_up(双线性 num·10 / S²) + HeightBase < L，L 是 p 所在块（左下采样点所在块）的水位。
    //   记 Hc(p) 为四角世界高度的连续双线性插值（不取整），整数厘米点上 Hc = HeightBase + num / 1000，可推出
    //     水 ⟺ 10·num + 5000 < 10000·(L − HeightBase) ⟺ Hc(p) < L − 0.5。
    //   所以“权威水域 = 整数厘米点里 Hc < L − 0.5 的那些点”，严格成立、没有误差。本生成器提取的就是 Hc = L − 0.5 这条
    //   等高线：采样点高度是整数厘米、等高值是半整数，采样点永远不会恰好落在等高线上——“端点等于水位”“整边等于水位”
    //   都不需要特判（等于水位的采样点 Hc − (L − 0.5) = 0.5 > 0，按严格小于算陆地，与服务端一致），也不会除以零。
    //   为了用整数精确判断，令 F = 2·(H − L) + 1（奇数，F < 0 即水），格内 f = 四角 F 的双线性插值。
    //
    // Marching Squares：每格四角 F 的符号决定格内拓扑；跨号的边上交点 t = F0 / (F0 − F1)（沿边的规范方向：横边从左端、
    // 竖边从下端算起），位置在世界采样坐标里算（Crossing），同一条边从哪一格、哪一块算都逐位相同。
    // 鞍点格（对角同号、相邻异号）用 asymptotic decider：双线性在鞍点的值 fs = (AD − BC) / (A + D − B − C)，分母与 A 同号，
    // 所以“鞍点是水 ⟺ (AD − BC)·sign(A) < 0”，用 decimal 精确算（不受浮点影响，两端 / 两块结论相同）。fs < 0 时两个水角
    // 经格中心相连（岸线切掉两个陆角），否则两个水角分开（岸线切掉两个水角）。fs = 0（AD = BC，等高线是过鞍点的两条直线）
    // 按“严格小于才是水”算鞍点不是水、水角分开——与权威规则在鞍点上的判定一致，不随机。
    //
    // 有界细分：格内精确等高线是双曲线 (u − us)(v − vs) = K 的一支（d = A − B − C + D ≠ 0 时；d = 0 是直线，K = 0 是两条
    // 轴向直线，都精确无需细分）。一段弦与这支曲线的最大距离在“切线平行于弦”的点取到，对这支双曲线就是
    // u* = us ± √((u0 − us)(u1 − us))（两端 u 的几何平均），所以每段的误差是精确算出来的，不是估计。误差超过 Tolerance
    // 就在 u* 处二分，至多 MaxDepth 层。细分点都在格内部，不落在格边 / 块边上，所以不会在公共边上制造 T 接缝。
    //
    // 不做的：不用样条平滑轮廓代替这条等高线；不恢复 1m 采样以下的细节；渲染网格（每格两个平面三角形）与水面的交线和这条
    // 等高线不同，误差见方案 §31。
    public static class TerrainShoreBuilder
    {
        private const int N = TerrainGeometry.ChunkSamples;
        private const int V = TerrainMesh.VerticesPerSide;
        private const double S = TerrainGeometry.SampleSpacing;

        // 规范方向的边上，从采样点 g0（沿边那一维的世界采样下标）出发、端点值 F0 → F1（异号）的交点在该维的世界坐标。
        // 横边的 g0 是左端的 gx，竖边是下端的 gy。所有调用方都走这一个函数，保证同一条边逐位相同。
        public static double Crossing(long g0, long f0, long f1) => g0 * S + S * ((double)f0 / (double)(f0 - f1));

        // 精确判定值 F = 2·(H − L) + 1：F < 0 ⟺ 高度 H 的点是水（H < L，等于不算）。
        public static long Field(int height, int level) => 2L * ((long)height - level) + 1;

        // 鞍点格（a、d 同号，b、c 同号且与 a 异号）的格中心是否是水：(AD − BC)·sign(A) < 0。相等时不是水。
        public static bool SaddleCentreIsWater(long a, long b, long c, long d)
        {
            decimal det = (decimal)a * d - (decimal)b * c;
            return a < 0 ? det > 0 : det < 0;
        }

        // 用视图当前数据生成一块的岸线；需要的块（本块、右、上、右上）没装时返回 false，missing 是缺的那一块。
        public static bool TryBuild(TerrainView view, ChunkKey key, ShoreOptions options, out TerrainShore? shore, out ChunkKey missing)
        {
            if (view == null) throw new ArgumentNullException(nameof(view));
            var grid = new int[TerrainMesh.VertexCount];
            shore = null;
            if (!view.TryBuildHeightGrid(key, grid, out missing)) return false;
            shore = Build(key, grid, view.WaterLevelOf(key), ShoreNeighbourLevels.Of(view, key), options);
            return true;
        }

        public static TerrainShore Build(ChunkKey key, ReadOnlySpan<int> grid, int level, ShoreNeighbourLevels neighbours, ShoreOptions options)
        {
            if (options == null) throw new ArgumentNullException(nameof(options));
            if (grid.Length < TerrainMesh.VertexCount) throw new ArgumentException($"grid needs {TerrainMesh.VertexCount} heights", nameof(grid));
            var cells = new byte[TerrainGeometry.ChunkPoints];
            var points = new List<ShorePoint>();
            var chains = new List<ShoreChain>();
            var polygonPoints = new List<ShorePoint>();
            var polygons = new List<ShorePolygon>();
            var seams = new List<ShoreSeam>();
            if (level == int.MinValue)
                return new TerrainShore(key, level, points, chains, polygonPoints, polygons, seams, cells, Array.Empty<int>(), 0, 0);

            var f = new long[TerrainMesh.VertexCount];
            var above = new int[TerrainMesh.VertexCount];
            for (int k = 0; k < f.Length; k++)
            {
                f[k] = Field(grid[k], level);
                above[k] = TerrainGeometry.ClampInt32((long)grid[k] - level);
            }
            var builder = new CellBuilder(key, f, options, points, polygonPoints, polygons);
            for (int j = 0; j < N; j++)
                for (int i = 0; i < N; i++)
                    cells[j * N + i] = builder.Cell(i, j);
            builder.LinkChains(chains);
            foreach (TerrainSide side in new[] { TerrainSide.Left, TerrainSide.Right, TerrainSide.Bottom, TerrainSide.Top })
            {
                int? other = neighbours.Of(side);
                if (other == level) continue;
                AddSeams(key, f, side, other, seams);
            }
            return new TerrainShore(key, level, builder.ChainPoints, chains, polygonPoints, polygons, seams, cells, above,
                builder.MaxDeviation, builder.DepthLimited);
        }

        // 块边 side 上本块一侧是水的区间（按本块水位，沿边线性；与岸线端点同一个 Crossing），相邻的区间合并。
        private static void AddSeams(ChunkKey key, long[] f, TerrainSide side, int? neighbourLevel, List<ShoreSeam> seams)
        {
            bool vertical = side == TerrainSide.Left || side == TerrainSide.Right;
            int fixedIndex = side == TerrainSide.Left || side == TerrainSide.Bottom ? 0 : N;
            long along0 = vertical ? key.Y * (long)N : key.X * (long)N;
            long line = ((vertical ? key.X * (long)N : key.Y * (long)N) + fixedIndex) * TerrainGeometry.SampleSpacing;
            double? open = null, end = 0;
            for (int s = 0; s < N; s++)
            {
                long f0 = vertical ? f[s * V + fixedIndex] : f[fixedIndex * V + s];
                long f1 = vertical ? f[(s + 1) * V + fixedIndex] : f[fixedIndex * V + s + 1];
                double a = (along0 + s) * S, b = (along0 + s + 1) * S;
                double from, to;
                if (f0 < 0 && f1 < 0) { from = a; to = b; }
                else if (f0 < 0) { from = a; to = Crossing(along0 + s, f0, f1); }
                else if (f1 < 0) { from = Crossing(along0 + s, f0, f1); to = b; }
                else continue;
                if (open != null && end == from) { end = to; continue; }
                if (open != null) seams.Add(new ShoreSeam(side, line, open.Value, end!.Value, neighbourLevel));
                open = from; end = to;
            }
            if (open != null) seams.Add(new ShoreSeam(side, line, open.Value, end!.Value, neighbourLevel));
        }

        // 逐格生成：分类、水面多边形、岸线段（每段从“出水”的交点到“入水”的交点，水在左手边），最后按共边把段连成折线。
        private sealed class CellBuilder
        {
            private readonly ChunkKey key;
            private readonly long[] f;
            private readonly ShoreOptions options;
            private readonly long gx0, gy0;
            private readonly List<ShorePoint> polygonPoints;
            private readonly List<ShorePolygon> polygons;
            // 岸线段：起止边的块内编号（横边 (i, j) 为 (j·65 + i)·2，竖边为 (j·65 + i)·2 + 1）与 segmentPoints 里的范围。
            private readonly List<(int From, int To, int Start, int Count)> segments = new List<(int, int, int, int)>();
            private readonly List<ShorePoint> segmentPoints = new List<ShorePoint>();
            private readonly Event[] events = new Event[8];

            public List<ShorePoint> ChainPoints { get; }
            public double MaxDeviation { get; private set; }
            public int DepthLimited { get; private set; }

            public CellBuilder(ChunkKey key, long[] f, ShoreOptions options, List<ShorePoint> chainPoints, List<ShorePoint> polygonPoints, List<ShorePolygon> polygons)
            {
                this.key = key; this.f = f; this.options = options; ChainPoints = chainPoints;
                this.polygonPoints = polygonPoints; this.polygons = polygons;
                gx0 = key.X * (long)N; gy0 = key.Y * (long)N;
            }

            private enum EventKind { Corner, Exit, Entry }

            private struct Event
            {
                public EventKind Kind;
                public ShorePoint Point;
                public double U, V;
                public int Edge;
                public bool Horizontal;
            }

            public byte Cell(int i, int j)
            {
                long a = f[j * V + i], b = f[j * V + i + 1], c = f[(j + 1) * V + i], d = f[(j + 1) * V + i + 1];
                bool wa = a < 0, wb = b < 0, wc = c < 0, wd = d < 0;
                if (wa && wb && wc && wd) return (byte)ShoreCellKind.Water;
                if (!wa && !wb && !wc && !wd) return (byte)ShoreCellKind.Land;

                // 逆时针走格边：角 c0 (0,0) → 底边 → c1 (1,0) → 右边 → c2 (1,1) → 顶边 → c3 (0,1) → 左边 → c0。
                int n = 0;
                long gx = gx0 + i, gy = gy0 + j;
                AddCorner(ref n, wa, gx, gy, 0, 0);
                if (wa != wb) AddCrossing(ref n, wa, new ShorePoint(Crossing(gx, a, b), gy * S), Ratio(a, b), 0, (j * V + i) * 2, true);
                AddCorner(ref n, wb, gx + 1, gy, 1, 0);
                if (wb != wd) AddCrossing(ref n, wb, new ShorePoint((gx + 1) * S, Crossing(gy, b, d)), 1, Ratio(b, d), (j * V + i + 1) * 2 + 1, false);
                AddCorner(ref n, wd, gx + 1, gy + 1, 1, 1);
                if (wd != wc) AddCrossing(ref n, wd, new ShorePoint(Crossing(gx, c, d), (gy + 1) * S), Ratio(c, d), 1, ((j + 1) * V + i) * 2, true);
                AddCorner(ref n, wc, gx, gy + 1, 0, 1);
                if (wc != wa) AddCrossing(ref n, wc, new ShorePoint(gx * S, Crossing(gy, a, c)), 0, Ratio(a, c), (j * V + i) * 2 + 1, false);

                bool saddle = wa == wd && wb == wc && wa != wb;
                bool connected = !saddle || SaddleCentreIsWater(a, b, c, d);
                var shape = new Bilinear(a, b, c, d);
                var visited = 0;
                for (int s = 0; s < n; s++)
                {
                    if (events[s].Kind != EventKind.Entry || (visited & 1 << s) != 0) continue;
                    int start = polygonPoints.Count, cur = s;
                    do
                    {
                        visited |= 1 << cur;
                        polygonPoints.Add(events[cur].Point);
                        cur = (cur + 1) % n;
                        while (events[cur].Kind == EventKind.Corner) { polygonPoints.Add(events[cur].Point); cur = (cur + 1) % n; }
                        // events[cur] 是出水点：沿等高线走到与它配对的入水点。
                        int exit = cur, entry = Match(n, exit, connected);
                        polygonPoints.Add(events[exit].Point);
                        int segStart = segmentPoints.Count;
                        segmentPoints.Add(events[exit].Point);
                        Contour(shape, gx, gy, events[exit], events[entry]);
                        segmentPoints.Add(events[entry].Point);
                        for (int p = segStart + 1; p < segmentPoints.Count - 1; p++) polygonPoints.Add(segmentPoints[p]);
                        segments.Add((events[exit].Edge, events[entry].Edge, segStart, segmentPoints.Count - segStart));
                        cur = entry;
                    } while (cur != s);
                    polygons.Add(new ShorePolygon(j * N + i, start, polygonPoints.Count - start));
                }
                return (byte)ShoreCellKind.Shore;
            }

            private static double Ratio(long f0, long f1) => (double)f0 / (double)(f0 - f1);

            private void AddCorner(ref int n, bool water, long gx, long gy, double u, double v)
            {
                if (!water) return;
                events[n++] = new Event { Kind = EventKind.Corner, Point = new ShorePoint(gx * S, gy * S), U = u, V = v, Edge = -1 };
            }

            private void AddCrossing(ref int n, bool fromWater, ShorePoint point, double u, double v, int edge, bool horizontal) =>
                events[n++] = new Event { Kind = fromWater ? EventKind.Exit : EventKind.Entry, Point = point, U = u, V = v, Edge = edge, Horizontal = horizontal };

            // 出水点 exit 配对的入水点：水连通（非鞍点格，或鞍点是水）时取其后（逆时针）第一个入水点，岸线切掉中间的陆角；
            // 鞍点格水不连通时取其前第一个入水点，岸线把这个水角单独围起来。只有两个交点时两种取法相同。
            private int Match(int n, int exit, bool connected)
            {
                for (int step = 1; step < n; step++)
                {
                    int k = connected ? (exit + step) % n : (exit - step + n) % n;
                    if (events[k].Kind == EventKind.Entry) return k;
                }
                throw new InvalidOperationException("marching squares: exit without an entry");
            }

            // 格内从 p 到 q 的精确等高线，按需细分，内部点追加到 segmentPoints（不含两端）。
            private void Contour(Bilinear shape, long gx, long gy, Event p, Event q)
            {
                if (shape.D == 0) { Record(0); return; }        // 线性：等高线是直线，弦就是精确的
                if (shape.Det == 0)
                {
                    // AD = BC：等高线是 u = us 与 v = vs 两条直线。横边上的交点在 u = us 上、竖边上的在 v = vs 上；
                    // 一端在横边、一端在竖边时折在 (us, vs)，否则是一条直线。
                    if (p.Horizontal != q.Horizontal) segmentPoints.Add(Local(gx, gy, shape.Us, shape.Vs));
                    Record(0);
                    return;
                }
                Subdivide(shape, gx, gy, p.U, p.V, q.U, q.V, 0);
            }

            private void Subdivide(Bilinear shape, long gx, long gy, double u0, double v0, double u1, double v1, int depth)
            {
                double side = (u0 - shape.Us) * (u1 - shape.Us);
                // 同一支双曲线上的两点在渐近线 u = us 的同一侧；数值上落到另一侧说明两点几乎重合，弦即可。
                if (!(side > 0)) { Record(0); return; }
                double um = shape.Us + Math.Sign(u0 - shape.Us) * Math.Sqrt(side);
                double vm = shape.Vs + shape.K / (um - shape.Us);
                double dx = u1 - u0, dy = v1 - v0, length = Math.Sqrt(dx * dx + dy * dy);
                double deviation = length == 0 ? 0 : Math.Abs(dx * (vm - v0) - dy * (um - u0)) / length * S;
                if (deviation <= options.Tolerance) { Record(deviation); return; }
                if (depth >= options.MaxDepth) { Record(deviation); DepthLimited++; return; }
                Subdivide(shape, gx, gy, u0, v0, um, vm, depth + 1);
                segmentPoints.Add(Local(gx, gy, um, vm));
                Subdivide(shape, gx, gy, um, vm, u1, v1, depth + 1);
            }

            private void Record(double deviation) { if (deviation > MaxDeviation) MaxDeviation = deviation; }

            private static ShorePoint Local(long gx, long gy, double u, double v) => new ShorePoint(gx * S + u * S, gy * S + v * S);

            // 按共边把岸线段连成折线：段的终边在块内时，下一段是从这条边出发的那一段（相邻格里同一个交点是出水点）。
            // 先从没有前驱的段（起点在块边上）开始走出不闭合的折线，剩下的都在环上（块内闭合）。
            public void LinkChains(List<ShoreChain> chains)
            {
                var startOf = new Dictionary<int, int>(segments.Count);
                for (int s = 0; s < segments.Count; s++) startOf[segments[s].From] = s;
                var next = new int[segments.Count];
                var hasPrevious = new bool[segments.Count];
                for (int s = 0; s < segments.Count; s++)
                {
                    next[s] = startOf.TryGetValue(segments[s].To, out int t) ? t : -1;
                    if (next[s] >= 0) hasPrevious[next[s]] = true;
                }
                var used = new bool[segments.Count];
                for (int s = 0; s < segments.Count; s++)
                    if (!hasPrevious[s]) Walk(s, next, used, chains, false);
                for (int s = 0; s < segments.Count; s++)
                    if (!used[s]) Walk(s, next, used, chains, true);
            }

            private void Walk(int first, int[] next, bool[] used, List<ShoreChain> chains, bool closed)
            {
                int start = ChainPoints.Count;
                for (int s = first; s >= 0 && !used[s]; s = next[s])
                {
                    used[s] = true;
                    var seg = segments[s];
                    // 相邻两段共用一个交点：后一段跳过自己的第一点。
                    for (int p = s == first ? 0 : 1; p < seg.Count; p++) ChainPoints.Add(segmentPoints[seg.Start + p]);
                }
                // 闭合环的最后一点就是第一点，去掉重复。
                if (closed) ChainPoints.RemoveAt(ChainPoints.Count - 1);
                chains.Add(new ShoreChain(start, ChainPoints.Count - start, closed));
            }
        }

        // 格内 f(u, v) = A(1−u)(1−v) + B·u(1−v) + C(1−u)v + D·uv = d(u − us)(v − vs) + fs，等高线 (u − us)(v − vs) = K。
        private readonly struct Bilinear
        {
            public long D { get; }
            public decimal Det { get; }
            public double Us { get; }
            public double Vs { get; }
            public double K { get; }

            public Bilinear(long a, long b, long c, long d)
            {
                long dd = a - b - c + d;
                D = dd;
                Det = (decimal)a * d - (decimal)b * c;
                if (dd == 0) { Us = Vs = K = 0; return; }
                Us = -(double)(c - a) / dd;
                Vs = -(double)(b - a) / dd;
                K = -(double)Det / ((double)dd * dd);
            }
        }
    }
}
