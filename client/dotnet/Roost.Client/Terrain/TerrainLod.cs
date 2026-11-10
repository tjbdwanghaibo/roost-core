using System;
using System.Collections.Generic;

namespace Roost.Client.Terrain
{
    // 块网格的 LOD 与接缝（可编辑地形 T04，方案 §31）。顶点永远是 TerrainMesh 的 65 × 65 网格（下标 j·65 + i），LOD 只决定
    // 用哪些顶点连三角形：级别 l 的步长 2^l（0～5，即 1～32 个采样间隔）。
    //
    // 接缝规则：块边上的顶点步长 = max(本块步长, 共边邻块步长)；邻块没有网格（不在显示集合 / 地图外）时用本块步长。
    // 两块对公共边算出的是同一个步长、同一组顶点（顶点高度本来就逐个相同），所以公共边上没有 T 接缝、没有裂缝。
    // 内部用本块步长的规则四边形；边框是四个梯形，外沿按边步长、内沿按本块步长，两条平行链之间按位置交替连三角形
    // （两条链上的点各自严格递增，三角形都不退化、朝向一致）。角点（0 / 64）在任何步长下都在，四块交点处也不缺顶点。
    //
    // 三角形朝向：俯视（世界 X 向右、Y 向上、高度朝外）逆时针。Unity（Y 向上、左手系、顺时针为正面）接入时按其约定交换顶点次序。
    public static class TerrainLod
    {
        public const int MaxLevel = 5;

        public static int Step(int level)
        {
            if (level < 0 || level > MaxLevel) throw new ArgumentOutOfRangeException(nameof(level), $"LOD level must be in [0, {MaxLevel}]");
            return 1 << level;
        }

        // 本块某条边上的顶点步长：邻块级别为 null（没有网格）时用本块步长。
        public static int EdgeStep(int level, int? neighbourLevel) =>
            Math.Max(Step(level), neighbourLevel == null ? Step(level) : Step(neighbourLevel.Value));

        // 生成三角形下标（每三个一组）。neighbours 是左 / 右 / 下 / 上邻块的 LOD 级别（null = 没有网格）。
        public static ushort[] BuildIndices(int level, int? left, int? right, int? bottom, int? top)
        {
            int s = Step(level);
            const int n = TerrainGeometry.ChunkSamples;
            var indices = new List<ushort>();
            // 内部：[s, 64 − s]² 里步长 s 的四边形，沿 (i, j)–(i+s, j+s) 对角线切成两个三角形。
            for (int j = s; j + s <= n - s; j += s)
                for (int i = s; i + s <= n - s; i += s)
                {
                    Add(indices, i, j, i + s, j, i + s, j + s);
                    Add(indices, i, j, i + s, j + s, i, j + s);
                }
            Ring(indices, TerrainSide.Bottom, s, EdgeStep(level, bottom));
            Ring(indices, TerrainSide.Right, s, EdgeStep(level, right));
            Ring(indices, TerrainSide.Top, s, EdgeStep(level, top));
            Ring(indices, TerrainSide.Left, s, EdgeStep(level, left));
            return indices.ToArray();
        }

        // 一条边的梯形：外链在块边上（沿边坐标 a = 0, e, 2e … 64，内移 b = 0），内链在本块步长的内框上（a = s … 64 − s，b = s）。
        // (a, b) 按边旋转到网格坐标，四个旋转都保持逆时针。
        private static void Ring(List<ushort> indices, TerrainSide side, int s, int e)
        {
            const int n = TerrainGeometry.ChunkSamples;
            int outer = 0, inner = s;
            while (outer < n || inner < n - s)
            {
                // 外链下一点不比内链下一点靠后（或内链已到头）时推进外链，否则推进内链。
                bool advanceOuter = inner >= n - s || (outer < n && outer + e <= inner + s);
                if (advanceOuter)
                {
                    Emit(indices, side, outer, 0, outer + e, 0, inner, s);
                    outer += e;
                }
                else
                {
                    Emit(indices, side, outer, 0, inner + s, s, inner, s);
                    inner += s;
                }
            }
        }

        private static void Emit(List<ushort> indices, TerrainSide side, int a0, int b0, int a1, int b1, int a2, int b2)
        {
            Rotate(side, a0, b0, out int x0, out int y0);
            Rotate(side, a1, b1, out int x1, out int y1);
            Rotate(side, a2, b2, out int x2, out int y2);
            Add(indices, x0, y0, x1, y1, x2, y2);
        }

        private static void Rotate(TerrainSide side, int a, int b, out int x, out int y)
        {
            const int n = TerrainGeometry.ChunkSamples;
            switch (side)
            {
                case TerrainSide.Bottom: x = a; y = b; break;
                case TerrainSide.Right: x = n - b; y = a; break;
                case TerrainSide.Top: x = n - a; y = n - b; break;
                default: x = b; y = n - a; break;
            }
        }

        private static void Add(List<ushort> indices, int x0, int y0, int x1, int y1, int x2, int y2)
        {
            indices.Add((ushort)(y0 * TerrainMesh.VerticesPerSide + x0));
            indices.Add((ushort)(y1 * TerrainMesh.VerticesPerSide + x1));
            indices.Add((ushort)(y2 * TerrainMesh.VerticesPerSide + x2));
        }
    }
}
