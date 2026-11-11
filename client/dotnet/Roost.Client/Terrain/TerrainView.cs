using System;
using System.Collections.Generic;
using System.Threading;

namespace Roost.Client.Terrain
{
    // 一个位置的查询结果。
    public readonly struct TerrainGround
    {
        // 地面的世界高度（厘米），双线性插值、整数定点，算法见 TerrainView.Height。
        public int Height { get; }
        // 所在 1m 格左下角采样点的材质编号。
        public byte Material { get; }
        // 地面低于所在块的水位线（Height < WaterLevel）。
        public bool Water { get; }
        // 所在块的水位线（区域水位或全局水位，TerrainMeta.WaterLevelOf）。
        public int WaterLevel { get; }
        // 水深（厘米）：水位线减地面高度；不是水时为 0，是水时为正（与 Go Ground.Depth 相同）。long：水位线与地面高度都是
        // int，但高度基准可以为负、水位线是独立配置的，合法的地图参数下两者之差可以超出 int（最大 2³² − 1）。
        public long Depth => Water ? (long)WaterLevel - Height : 0;
        public TerrainGround(int height, byte material, bool water, int waterLevel) { Height = height; Material = material; Water = water; WaterLevel = waterLevel; }
    }

    // TryProbe 的结果：不抛异常的查询入口给每帧热路径（射线、贴地）用。
    public enum TerrainQueryStatus { Ok, OutOfMap, NotLoaded }

    // 视图里装着的一块，装入后不可变（换版本是换一份新数据），所以可以不复制地交给调用方只读。
    public sealed class TerrainChunkData
    {
        private readonly ushort[] heights;
        private readonly byte[] materials;
        public ChunkKey Key { get; }
        // 0 = 原始块（来自包），≥ 1 = 覆盖块的 Version。
        public long Version { get; }
        public ReadOnlySpan<ushort> Heights => heights;
        public ReadOnlySpan<byte> Materials => materials;
        internal ushort[] RawHeights => heights;
        internal TerrainChunkData(ChunkKey key, long version, ushort[] heights, byte[] materials)
        { Key = key; Version = version; this.heights = heights; this.materials = materials; }

        internal bool SameData(TerrainChunkData other) =>
            Heights.SequenceEqual(other.Heights) && Materials.SequenceEqual(other.Materials);
    }

    public enum TerrainChunkChangeKind { Loaded, Updated, Unloaded }

    // 一块的数据变化通知（mesh 重建钩子）。只在数据或版本真的变了时发：同版本同数据的重复全量（重连、重新进入视野）不发。
    public sealed class TerrainChunkChange
    {
        public ChunkKey Key { get; }
        public TerrainChunkChangeKind Kind { get; }
        // 变化前是否装着、变化前的版本（没装着时为 0）。
        public bool WasLoaded { get; }
        public long PreviousVersion { get; }
        // 变化后的版本（Unloaded 时为 0）。
        public long Version { get; }
        // 需要重建 mesh 的块：这一块本身，以及共享边读它的左 / 下 / 左下邻块（只含地图内的块）。
        // 顶点网格的右边与上边取右 / 上邻块的数据（TerrainMesh），所以一块变了，左、下、左下三块的接缝也变了。
        public IReadOnlyList<ChunkKey> MeshesToRebuild { get; }
        internal TerrainChunkChange(ChunkKey key, TerrainChunkChangeKind kind, TerrainChunkData? previous, TerrainChunkData? current,
            IReadOnlyList<ChunkKey> meshes)
        {
            Key = key; Kind = kind; WasLoaded = previous != null; PreviousVersion = previous?.Version ?? 0;
            Version = current?.Version ?? 0; MeshesToRebuild = meshes;
        }
    }

    // 一张地图在客户端已装入的块：原始块（从 TerrainPack 读出）与覆盖块（entitysync 收到的 TerrainChunk DAO）。
    // 每个块坐标至多一份数据，覆盖块优先于原始块；不自动淘汰，装哪些、卸哪些由调用方按视距决定。
    // 规则与 Go gameplay/terrain 的 View 相同，查询结果与 Go 逐位一致（client/spec/terrain-golden.pack 金样）。
    //
    // 线程：
    //   - 查询（Height / Material / IsWater / Probe / TryProbe / GetChunk / TryGetVersion / BuildHeightGrid）不取锁，
    //     只读块表槽位，可以在任意线程并发调用。一次查询对每个用到的块只读一次槽位，同一块内不会混用两个版本。
    //   - 写入口 LoadBase / SetOverlay / Unload 由一把锁串行，各自原子地换掉一个槽位。LoadBase 读包与解压在锁外。
    //   - ChunkChanged 在写入口的调用线程、锁外、新数据已可查询之后触发。多个线程同时写时通知之间的先后不保证，
    //     处理者应按视图当前状态重建（例如只记“脏块”，下一帧主线程用 BuildHeightGrid 读当前数据）。
    //     订阅者抛出的异常交给 Error，不影响已装入的数据和其他订阅者；Error 自身抛错被忽略。
    public sealed class TerrainView
    {
        private readonly TerrainPack pack;
        private readonly TerrainChunkData?[] chunks;
        // 每块的水位线（TerrainMeta.WaterLevelOf，构造时算好、之后不变；与 Go View.water 相同）。
        private readonly int[] water;
        private readonly object writeGate = new object();
        private int loaded;

        public TerrainMeta Meta { get; }
        public event Action<TerrainChunkChange>? ChunkChanged;
        public event Action<Exception>? Error;

        // 原始块从 pack 读取，地图参数取自 pack 的包头。pack 要比视图活得久。
        public TerrainView(TerrainPack pack)
        {
            this.pack = pack ?? throw new ArgumentNullException(nameof(pack));
            Meta = pack.Meta;
            chunks = new TerrainChunkData?[Meta.Chunks];
            water = new int[Meta.Chunks];
            for (int i = 0; i < water.Length; i++) water[i] = Meta.WaterLevelOf(new ChunkKey(i % Meta.ChunksX, i / Meta.ChunksX));
        }

        // 块的水位线（区域水位或全局水位），O(1)；块不在地图内时是全局水位线。与 Go View.WaterLevelOf 相同。
        public int WaterLevelOf(ChunkKey key) => Meta.Contains(key) ? water[Meta.IndexOf(key)] : Meta.WaterLevel;

        // 装着的块数。
        public int Count => Volatile.Read(ref loaded);

        // 从原始地形包装入一块。这一块已经装着（原始或覆盖）时什么都不做：不会用原始块盖掉覆盖块。
        // 读包失败（含 TerrainCorruptChunkException）时不装入任何东西。块不在地图内抛 TerrainOutOfMapException。
        public void LoadBase(ChunkKey key)
        {
            int slot = Slot(key);
            if (Volatile.Read(ref chunks[slot]) != null) return;
            var chunk = pack.ReadChunk(key);
            var next = new TerrainChunkData(key, 0, chunk.Heights, chunk.Materials);
            lock (writeGate)
            {
                if (Volatile.Read(ref chunks[slot]) != null) return;
                Volatile.Write(ref chunks[slot], next);
                loaded++;
            }
            Notify(key, TerrainChunkChangeKind.Loaded, null, next);
        }

        // 装入（或更新）一块覆盖块，替换这一块现有的原始块或更旧的覆盖块。入参被复制，调用之后调用方可以继续改它。
        // 拒绝（不改视图）：块不在地图内（TerrainOutOfMapException）、形状不对或 version < 1（TerrainInvalidChunkException）、
        // baseRevision 不等于包头（TerrainBaseRevisionException）、比已装覆盖块的版本旧（TerrainStaleOverlayException）。
        // 版本相同则替换（数据也相同时不发通知）。
        public void SetOverlay(ChunkKey key, long baseRevision, long version, ReadOnlySpan<ushort> heights, ReadOnlySpan<byte> materials)
        {
            int slot = Slot(key);
            CheckShape(key, heights.Length, materials.Length);
            Install(slot, key, baseRevision, version, heights.ToArray(), materials.ToArray());
        }

        // 同上，高度与材质取自生成的 DAO 视图（SyncTerrainChunkDao.Heights.Value / Materials.Value 都是 RepeatedField<uint>：
        // protobuf 用 uint32 承载 Go DAO 的 []uint16；材质在 DAO 里为按下标增量而取 uint16，取值仍是 0～255）。
        // 任何一个高度超过 65535、材质超过 255 都按形状不对拒绝（TerrainInvalidChunkException），不截断。
        public void SetOverlay(ChunkKey key, long baseRevision, long version, IReadOnlyList<uint> heights, IReadOnlyList<uint> materials)
        {
            if (heights == null) throw new ArgumentNullException(nameof(heights));
            if (materials == null) throw new ArgumentNullException(nameof(materials));
            int slot = Slot(key);
            CheckShape(key, heights.Count, materials.Count);
            var heightCopy = new ushort[TerrainGeometry.ChunkPoints];
            var materialCopy = new byte[TerrainGeometry.ChunkPoints];
            for (int i = 0; i < heightCopy.Length; i++)
            {
                uint h = heights[i];
                if (h > ushort.MaxValue) throw new TerrainInvalidChunkException($"overlay {key}: height {i} is {h}, above {ushort.MaxValue}");
                heightCopy[i] = (ushort)h;
                uint m = materials[i];
                if (m > byte.MaxValue) throw new TerrainInvalidChunkException($"overlay {key}: material {i} is {m}, above {byte.MaxValue}");
                materialCopy[i] = (byte)m;
            }
            Install(slot, key, baseRevision, version, heightCopy, materialCopy);
        }

        private static void CheckShape(ChunkKey key, int heights, int materials)
        {
            if (heights != TerrainGeometry.ChunkPoints || materials != TerrainGeometry.ChunkPoints)
                throw new TerrainInvalidChunkException($"overlay {key}: {heights} heights and {materials} materials, want {TerrainGeometry.ChunkPoints} each");
        }

        private void Install(int slot, ChunkKey key, long baseRevision, long version, ushort[] heights, byte[] materials)
        {
            if (version < 1) throw new TerrainInvalidChunkException($"overlay {key} has version {version}, want ≥ 1");
            if (baseRevision != Meta.BaseRevision)
                throw new TerrainBaseRevisionException($"overlay {key} is based on revision {baseRevision}, pack is {Meta.BaseRevision}");
            var next = new TerrainChunkData(key, version, heights, materials);
            TerrainChunkData? current;
            lock (writeGate)
            {
                current = Volatile.Read(ref chunks[slot]);
                if (current != null && current.Version > version)
                    throw new TerrainStaleOverlayException($"overlay {key} version {version}, loaded {current.Version}");
                Volatile.Write(ref chunks[slot], next);
                if (current == null) loaded++;
            }
            if (current == null) Notify(key, TerrainChunkChangeKind.Loaded, null, next);
            else if (current.Version != version || !current.SameData(next)) Notify(key, TerrainChunkChangeKind.Updated, current, next);
        }

        // 卸下一块（原始或覆盖），返回它之前是否装着。之后依赖这块的查询报 NotLoaded。
        public bool Unload(ChunkKey key)
        {
            if (!Meta.Contains(key)) return false;
            int slot = Meta.IndexOf(key);
            TerrainChunkData? previous;
            lock (writeGate)
            {
                previous = Interlocked.Exchange(ref chunks[slot], null);
                if (previous == null) return false;
                loaded--;
            }
            Notify(key, TerrainChunkChangeKind.Unloaded, previous, null);
            return true;
        }

        // 一块是否装着，以及装着的版本（0 = 原始块，≥ 1 = 覆盖块的 Version）。
        public bool TryGetVersion(ChunkKey key, out long version)
        {
            var c = Load(key);
            version = c?.Version ?? 0;
            return c != null;
        }

        // 装着的一块（只读、不复制，装入后不再变化）；没装着或不在地图内返回 null。
        public TerrainChunkData? GetChunk(ChunkKey key) => Load(key);

        private TerrainChunkData? Load(ChunkKey key) => Meta.Contains(key) ? Volatile.Read(ref chunks[Meta.IndexOf(key)]) : null;

        private int Slot(ChunkKey key)
        {
            if (!Meta.Contains(key)) throw new TerrainOutOfMapException("chunk " + key);
            return Meta.IndexOf(key);
        }

        // 世界坐标 (x, y) 处地面的世界高度（厘米）。
        //
        // 算法（整数定点，与 Go View.Height 逐位相同）：S = 100，gx = x / S、fx = x % S（y 同理，地图内坐标非负）。
        // 四个采样点的原始值 h00 = (gx, gy)、h10 = (gx+1, gy)、h01 = (gx, gy+1)、h11 = (gx+1, gy+1)：
        //
        //   num    = (h00·(S−fx) + h10·fx)·(S−fy) + (h01·(S−fx) + h11·fx)·fy      （0 ≤ num ≤ 65535·S²，用 long）
        //   Height = HeightBase + (num·HeightStep + S²/2) / S²                      （num ≥ 0，整数除法即向下取整）
        //
        // 邻点：权重为 0 的邻点不读（fx = 0 不取 gx+1 列，fy = 0 不取 gy+1 行）；gx+1 越出地图右边界时用 gx 列（边缘复制），
        // 上边界同理；其余情况下邻点可能在右 / 上 / 右上邻块，邻块没装入抛 TerrainNotLoadedException（带缺的块），不猜测。
        // 坐标在地图外抛 TerrainOutOfMapException。
        public int Height(long x, long y) => Probe(x, y).Height;

        // 世界坐标 (x, y) 所在 1m 格左下角采样点的材质（只需要本块）。
        public byte Material(long x, long y)
        {
            if (!Meta.ContainsPoint(x, y)) throw new TerrainOutOfMapException($"point ({x}, {y})");
            var key = TerrainGeometry.PointIndex(x, y, out int i);
            var c = Load(key);
            if (c == null) throw new TerrainNotLoadedException(key);
            return c.Materials[i];
        }

        // 地面是否低于所在块的水位线（Height(x, y) < WaterLevelOf(包含 (x, y) 的块)，等于不算水），错误同 Height。
        public bool IsWater(long x, long y) => Probe(x, y).Water;

        // 水深（厘米，TerrainGround.Depth，long），错误同 Height。
        public long WaterDepth(long x, long y) => Probe(x, y).Depth;

        // 一次取得高度、材质与是否为水，错误同 Height。高度与材质出自本块的同一个版本。
        public TerrainGround Probe(long x, long y)
        {
            switch (TryProbe(x, y, out var ground, out var missing))
            {
                case TerrainQueryStatus.OutOfMap: throw new TerrainOutOfMapException($"point ({x}, {y})");
                case TerrainQueryStatus.NotLoaded: throw new TerrainNotLoadedException(missing);
                default: return ground;
            }
        }

        // 不抛异常的 Probe：返回 Ok / OutOfMap / NotLoaded（missing 是缺的那一块）。
        public TerrainQueryStatus TryProbe(long x, long y, out TerrainGround ground, out ChunkKey missing)
        {
            ground = default;
            missing = default;
            if (!Meta.ContainsPoint(x, y)) return TerrainQueryStatus.OutOfMap;
            const long s = TerrainGeometry.SampleSpacing;
            const int n = TerrainGeometry.ChunkSamples;
            long gx = x / s, gy = y / s, fx = x % s, fy = y % s;
            long gx1 = gx, gy1 = gy;
            if (fx > 0 && gx + 1 < (long)Meta.ChunksX * n) gx1 = gx + 1;
            if (fy > 0 && gy + 1 < (long)Meta.ChunksY * n) gy1 = gy + 1;
            int cx0 = (int)(gx / n), cx1 = (int)(gx1 / n), cy0 = (int)(gy / n), cy1 = (int)(gy1 / n);
            // 四个点所在的块：每块的槽位只读一次，相同的块复用同一次读取。
            var c00 = Load(new ChunkKey(cx0, cy0));
            if (c00 == null) { missing = new ChunkKey(cx0, cy0); return TerrainQueryStatus.NotLoaded; }
            var c10 = c00;
            if (cx1 != cx0 && (c10 = Load(new ChunkKey(cx1, cy0))) == null) { missing = new ChunkKey(cx1, cy0); return TerrainQueryStatus.NotLoaded; }
            TerrainChunkData? c01 = c00, c11 = c10;
            if (cy1 != cy0)
            {
                if ((c01 = Load(new ChunkKey(cx0, cy1))) == null) { missing = new ChunkKey(cx0, cy1); return TerrainQueryStatus.NotLoaded; }
                c11 = c01;
                if (cx1 != cx0 && (c11 = Load(new ChunkKey(cx1, cy1))) == null) { missing = new ChunkKey(cx1, cy1); return TerrainQueryStatus.NotLoaded; }
            }
            long h00 = At(c00, gx, gy), h10 = At(c10!, gx1, gy), h01 = At(c01!, gx, gy1), h11 = At(c11!, gx1, gy1);
            long num = (h00 * (s - fx) + h10 * fx) * (s - fy) + (h01 * (s - fx) + h11 * fx) * fy;
            int h = Meta.HeightBase + (int)((num * TerrainGeometry.HeightStep + s * s / 2) / (s * s));
            byte m = c00.Materials[(int)((gy % n) * n + gx % n)];
            int level = water[Meta.IndexOf(new ChunkKey(cx0, cy0))];
            ground = new TerrainGround(h, m, h < level, level);
            return TerrainQueryStatus.Ok;
        }

        private static long At(TerrainChunkData c, long gx, long gy) =>
            c.RawHeights[(int)((gy % TerrainGeometry.ChunkSamples) * TerrainGeometry.ChunkSamples + gx % TerrainGeometry.ChunkSamples)];

        // 这一块的顶点高度网格（65 × 65，见 TerrainMesh.TryBuildHeightGrid），数据取自视图当前装着的块。
        // 返回 false 表示需要的块没装（missing 是缺的那一块），destination 内容未定义。块不在地图内抛 TerrainOutOfMapException。
        public bool TryBuildHeightGrid(ChunkKey key, Span<int> destination, out ChunkKey missing)
        {
            Slot(key);
            var self = Load(key);
            var right = Load(new ChunkKey(key.X + 1, key.Y));
            var top = Load(new ChunkKey(key.X, key.Y + 1));
            var topRight = Load(new ChunkKey(key.X + 1, key.Y + 1));
            return TerrainMesh.TryBuildHeightGrid(Meta, key,
                self == null ? default : self.Heights, right == null ? default : right.Heights,
                top == null ? default : top.Heights, topRight == null ? default : topRight.Heights,
                destination, out missing);
        }

        private void Notify(ChunkKey key, TerrainChunkChangeKind kind, TerrainChunkData? previous, TerrainChunkData? current)
        {
            var handlers = ChunkChanged;
            if (handlers == null) return;
            var change = new TerrainChunkChange(key, kind, previous, current, TerrainMesh.AffectedChunks(Meta, key));
            foreach (Action<TerrainChunkChange> callback in handlers.GetInvocationList())
            {
                try { callback(change); }
                catch (Exception error) { Report(error); }
            }
        }

        private void Report(Exception error)
        {
            var handlers = Error;
            if (handlers == null) return;
            foreach (Action<Exception> callback in handlers.GetInvocationList())
            {
                try { callback(error); }
                catch { }
            }
        }
    }
}
