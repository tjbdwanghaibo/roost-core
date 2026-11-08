using System;
using System.Collections.Generic;

namespace Roost.Client.Terrain
{
    // 给渲染侧（Unity 生成 Mesh）用的纯函数：不持有状态、不分配大对象，输入块数据，输出顶点高度。
    public static class TerrainMesh
    {
        // 每块网格每边的顶点数：64 个采样点加上右 / 上邻块的共享边。
        public const int VerticesPerSide = TerrainGeometry.ChunkSamples + 1;
        // 每块网格的顶点数（65 × 65）。
        public const int VertexCount = VerticesPerSide * VerticesPerSide;

        // 生成块 key 的顶点高度网格：destination[j·65 + i] 是世界采样点 (key.X·64 + i, key.Y·64 + j) 的世界高度（厘米），
        // i、j ∈ [0, 64]，顶点的世界坐标是 (采样点 × 100)。
        //
        // 数据来源与 TerrainView.Height 的邻点规则一致，因此每个顶点的值等于 Height 在该点的结果，相邻两块共享边上的顶点
        // 逐个相同（接缝连续）：
        //   - i、j < 64 取本块（self）；
        //   - i = 64 那一列取右邻块（right）的第 0 列，j = 64 那一行取上邻块（top）的第 0 行，角点取右上邻块（topRight）；
        //   - 右 / 上邻块在地图外（本块在地图右 / 上边上）时复制本块最后一列 / 一行（边缘复制），这时对应的邻块参数被忽略；
        //   - 需要的块是空 span（没装入）时返回 false、missing 为缺的那一块，destination 内容未定义；不猜测缺失的数据。
        // 每个非空 span 的长度必须是 4096。块不在地图内抛 TerrainOutOfMapException。
        //
        // 注意：网格三角形在 1m 格内是平面，而 Height 在格内是双线性插值，格内的点两者可能差几厘米；采样点（顶点）上完全相同。
        // 需要与服务端一致的判定（落地、站立高度）用 TerrainView 的查询，不要用渲染网格。
        public static bool TryBuildHeightGrid(TerrainMeta meta, ChunkKey key,
            ReadOnlySpan<ushort> self, ReadOnlySpan<ushort> right, ReadOnlySpan<ushort> top, ReadOnlySpan<ushort> topRight,
            Span<int> destination, out ChunkKey missing)
        {
            if (meta == null) throw new ArgumentNullException(nameof(meta));
            if (!meta.Contains(key)) throw new TerrainOutOfMapException("chunk " + key);
            if (destination.Length < VertexCount) throw new ArgumentException($"destination needs {VertexCount} elements", nameof(destination));
            bool hasRight = key.X + 1 < meta.ChunksX, hasTop = key.Y + 1 < meta.ChunksY;
            missing = default;
            if (!Present(self, key, ref missing)) return false;
            if (hasRight && !Present(right, new ChunkKey(key.X + 1, key.Y), ref missing)) return false;
            if (hasTop && !Present(top, new ChunkKey(key.X, key.Y + 1), ref missing)) return false;
            if (hasRight && hasTop && !Present(topRight, new ChunkKey(key.X + 1, key.Y + 1), ref missing)) return false;

            const int n = TerrainGeometry.ChunkSamples;
            for (int j = 0; j < VerticesPerSide; j++)
            {
                // 行 j 来自本块（j < 64）、上邻块的第 0 行，或在地图上边时复制本块第 63 行。
                bool upper = j == n && hasTop;
                int row = j < n ? j : (hasTop ? 0 : n - 1);
                ReadOnlySpan<ushort> inner = upper ? top : self, outer = upper ? topRight : right;
                for (int i = 0; i < VerticesPerSide; i++)
                {
                    ushort raw;
                    if (i < n) raw = inner[row * n + i];
                    else if (hasRight) raw = outer[row * n];
                    else raw = inner[row * n + n - 1];
                    destination[j * VerticesPerSide + i] = meta.WorldHeight(raw);
                }
            }
            return true;
        }

        // 同 TryBuildHeightGrid，缺块时抛 TerrainNotLoadedException。
        public static void BuildHeightGrid(TerrainMeta meta, ChunkKey key,
            ReadOnlySpan<ushort> self, ReadOnlySpan<ushort> right, ReadOnlySpan<ushort> top, ReadOnlySpan<ushort> topRight,
            Span<int> destination)
        {
            if (!TryBuildHeightGrid(meta, key, self, right, top, topRight, destination, out var missing))
                throw new TerrainNotLoadedException(missing);
        }

        // 块 changed 的数据变了之后需要重建网格的块：它自己，以及网格共享边读它的左 / 下 / 左下邻块（只含地图内的块）。
        // 邻块装入同理：右 / 上邻块装入后，之前因缺块建不成的网格可以建了。
        public static IReadOnlyList<ChunkKey> AffectedChunks(TerrainMeta meta, ChunkKey changed)
        {
            if (meta == null) throw new ArgumentNullException(nameof(meta));
            var keys = new List<ChunkKey>(4);
            foreach (var key in new[] { changed, new ChunkKey(changed.X - 1, changed.Y), new ChunkKey(changed.X, changed.Y - 1), new ChunkKey(changed.X - 1, changed.Y - 1) })
                if (meta.Contains(key)) keys.Add(key);
            return keys.AsReadOnly();
        }

        private static bool Present(ReadOnlySpan<ushort> heights, ChunkKey key, ref ChunkKey missing)
        {
            if (heights.IsEmpty) { missing = key; return false; }
            if (heights.Length != TerrainGeometry.ChunkPoints)
                throw new ArgumentException($"chunk {key} has {heights.Length} heights, want {TerrainGeometry.ChunkPoints}");
            return true;
        }
    }
}
