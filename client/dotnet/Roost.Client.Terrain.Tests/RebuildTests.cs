using System;
using System.Collections.Generic;
using System.Linq;
using Roost.Client.Terrain;

internal static partial class Program
{
    // 手动推进的执行器：任务只排队，由测试决定何时、按什么次序执行；记录队列最长的时候。
    private sealed class ManualExecutor : ITerrainBuildExecutor
    {
        public readonly List<Action> Queue = new List<Action>();
        public int MaxQueued;
        public void Run(Action work) { Queue.Add(work); MaxQueued = Math.Max(MaxQueued, Queue.Count); }
        public void RunAt(int index) { var work = Queue[index]; Queue.RemoveAt(index); work(); }
        public void RunAll() { while (Queue.Count > 0) RunAt(0); }
    }

    // 一套调度器 + 不变量检查：每次发布时，发布的结果与视图当前数据一致、属于当前会话与地形、整组与所有正在显示的
    // 相邻块接缝一致。
    private sealed class Rig
    {
        public readonly TerrainView View;
        public readonly ManualExecutor Exec = new ManualExecutor();
        public readonly TerrainRebuildScheduler Scheduler;
        public readonly List<TerrainPublishBatch> Batches = new List<TerrainPublishBatch>();
        public readonly HashSet<ChunkKey> Wanted = new HashSet<ChunkKey>();
        public TerrainView Current;

        public Rig(TerrainView view, int maxInFlight = 4, bool confirm = true, int maxChunks = 64)
        {
            View = Current = view;
            Scheduler = new TerrainRebuildScheduler(view, Exec, new TerrainRebuildOptions { MaxInFlight = maxInFlight, RequireConfirmation = confirm, MaxChunks = maxChunks });
            Scheduler.Error += e => throw new Exception("FAIL scheduler error: " + e);
            Scheduler.Published += OnPublished;
        }

        private void OnPublished(TerrainPublishBatch batch)
        {
            Batches.Add(batch);
            var grid = new int[TerrainMesh.VertexCount];
            foreach (var b in batch.Builds)
            {
                Check(b.Session == Scheduler.Session && b.Terrain == Scheduler.Terrain, $"published {b.Key} belongs to session {b.Session}/terrain {b.Terrain}, now {Scheduler.Session}/{Scheduler.Terrain}");
                Check(Current.TryBuildHeightGrid(b.Key, grid, out _) && b.Heights.SequenceEqual(grid), $"published {b.Key} matches the current terrain data");
                Check(b.Shore.WaterLevel == Current.WaterLevelOf(b.Key), $"published {b.Key} uses the current water level");
            }
            CheckShownSeams();
        }

        public void CheckShownSeams()
        {
            var shown = new Dictionary<ChunkKey, TerrainChunkBuild>();
            foreach (var key in Wanted)
                if (Scheduler.TryGetShown(key, out var b, out _)) shown[key] = b!;
            foreach (var a in shown.Values)
                foreach (var b in shown.Values)
                    if (a.Key != b.Key && Math.Abs(a.Key.X - b.Key.X) <= 1 && Math.Abs(a.Key.Y - b.Key.Y) <= 1)
                        Check(a.SeamMatches(b), $"shown {a.Key} and {b.Key} agree on their seam");
        }

        public void Want(ChunkKey key, int lod) { Scheduler.Want(key, lod); Wanted.Add(key); }
        public void Drop(ChunkKey key) { Scheduler.Drop(key); Wanted.Remove(key); }

        public void ConfirmAll()
        {
            for (int y = 0; y < Current.Meta.ChunksY; y++)
                for (int x = 0; x < Current.Meta.ChunksX; x++) Scheduler.Confirm(new ChunkKey(x, y));
        }

        // 跑完所有任务直到没有新任务。
        public void Settle()
        {
            for (int round = 0; round < 64; round++)
            {
                Scheduler.Pump();
                if (Exec.Queue.Count == 0) return;
                Exec.RunAll();
            }
            throw new Exception("FAIL scheduler did not settle");
        }

        public bool Shown(ChunkKey key, out TerrainChunkBuild? build, out bool current) => Scheduler.TryGetShown(key, out build, out current);

        // 正在显示的每块都与当前数据直接重建的结果相同。
        public void CheckAllCurrent()
        {
            foreach (var key in Wanted)
            {
                Check(Shown(key, out var b, out bool current) && current, $"{key} shown and current");
                var stats = Scheduler.Stats;
                Check(TerrainChunkBuilder.TryCapture(Current, key, b!.Input.Lod, b.Input.LeftLod, b.Input.RightLod, b.Input.BottomLod, b.Input.TopLod,
                    ShoreOptions.Default, out var input, out _), "capture " + key);
                var direct = TerrainChunkBuilder.Build(input!);
                Check(direct.Heights.SequenceEqual(b.Heights) && direct.GradientX.SequenceEqual(b.GradientX) && direct.GradientY.SequenceEqual(b.GradientY)
                    && direct.Indices.SequenceEqual(b.Indices) && direct.Shore.Points.SequenceEqual(b.Shore.Points), $"{key} shown equals a direct rebuild");
            }
        }
    }

    private static TerrainView ViewOf(TerrainPack pack, params ChunkKey[] keys)
    {
        var view = new TerrainView(pack);
        foreach (var k in keys) view.LoadBase(k);
        return view;
    }

    private static ChunkKey[] AllKeys(TerrainMeta meta)
    {
        var keys = new List<ChunkKey>();
        for (int y = 0; y < meta.ChunksY; y++)
            for (int x = 0; x < meta.ChunksX; x++) keys.Add(new ChunkKey(x, y));
        return keys.ToArray();
    }

    private static void EditChunk(TerrainView view, ChunkKey key, Action<ushort[]> edit)
    {
        var current = view.GetChunk(key)!;
        var heights = current.Heights.ToArray();
        edit(heights);
        view.SetOverlay(key, view.Meta.BaseRevision, Math.Max(1, current.Version + 1), heights, current.Materials);
    }

    private static void TestRebuildScheduler(byte[] packBytes)
    {
        using var pack = TerrainPack.Open(packBytes);
        var meta = pack.Meta;
        var all = AllKeys(meta);
        var k00 = new ChunkKey(0, 0); var k10 = new ChunkKey(1, 0); var k01 = new ChunkKey(0, 1); var k11 = new ChunkKey(1, 1);

        // 1. 邻块晚到、覆盖块未确认：依赖没装齐或没确认时不派发；确认的是覆盖块之后的数据。
        {
            var rig = new Rig(ViewOf(pack, k00));
            rig.Want(k00, 0);
            rig.Scheduler.Confirm(k00);
            rig.Scheduler.Pump();
            Check(rig.Exec.Queue.Count == 0 && rig.Scheduler.Stats.Pending == 1, "late neighbours: nothing dispatched while neighbours are missing");
            foreach (var k in new[] { k10, k01, k11 }) rig.View.LoadBase(k);
            rig.Scheduler.Confirm(k01); rig.Scheduler.Confirm(k11);
            rig.Settle();
            Check(rig.Batches.Count == 0, "late neighbours: base data of an unconfirmed neighbour is not used");
            EditChunk(rig.View, k10, h => { for (int j = 0; j < 64; j++) h[j * 64] = (ushort)(h[j * 64] + 40); });
            rig.Scheduler.Confirm(k10);
            rig.Settle();
            Check(rig.Batches.Count == 1 && rig.Batches[0].Builds.Single().Key == k00, "late neighbours: published once the overlay is confirmed");
            Check(rig.Shown(k00, out var b, out bool current) && current && b!.Input.ChunkAt(1, 0)!.Version == 1, "late neighbours: built from the confirmed overlay");
            rig.CheckAllCurrent();
        }

        // 2. 公共边编辑：读到它的四块一起重建；先完成的结果与旧邻居接缝不一致时暂存，整组完成后一次发布。
        {
            var rig = new Rig(ViewOf(pack, all));
            rig.ConfirmAll();
            foreach (var k in all) rig.Want(k, 0);
            rig.Settle();
            int before = rig.Batches.Count;
            EditChunk(rig.View, k10, h => { for (int j = 0; j < 64; j++) h[j * 64] = (ushort)(h[j * 64] + 25); });
            rig.Scheduler.Pump();
            Check(rig.Exec.Queue.Count == 4, $"edge edit: {rig.Exec.Queue.Count} rebuilds, want 4 (the chunk, left, top, top-left normals)");
            for (int outstanding = 4; outstanding > 1; outstanding--)
            {
                rig.Exec.RunAt(outstanding - 1);
                rig.Scheduler.Pump();
                Check(rig.Batches.Count == before, $"edge edit: nothing published with {outstanding - 1} of 4 rebuilds outstanding, got {string.Join(" ", rig.Batches.Skip(before).SelectMany(x => x.Builds).Select(x => x.Key))}");
                Check(rig.Shown(k10, out _, out bool cur) && !cur, "edge edit: the old mesh stays shown and is reported expired");
            }
            rig.Exec.RunAt(0);
            rig.Scheduler.Pump();
            Check(rig.Batches.Count == before + 1 && rig.Batches.Last().Builds.Select(x => x.Key).OrderBy(x => x.Y).ThenBy(x => x.X)
                .SequenceEqual(new[] { k00, k10, new ChunkKey(0, 1), k11 }), "edge edit: the four rebuilds publish together");
            rig.CheckAllCurrent();

            // 3. 只改块内部：只有这一块重建、单独发布。
            EditChunk(rig.View, k10, h => h[32 * 64 + 32] += 7);
            rig.Scheduler.Pump();
            Check(rig.Exec.Queue.Count == 1, "interior edit: only the chunk itself rebuilds");
            rig.Settle();
            Check(rig.Batches.Last().Builds.Single().Key == k10, "interior edit: published alone");

            // 4. 乱序：任务 1 在第二次编辑前算完，任务 2 在其后；任务 1 的结果过期，不发布。另一块的两个任务倒序完成，旧的丢弃。
            int published = rig.Batches.Count;
            EditChunk(rig.View, k10, h => h[30 * 64 + 30] += 3);
            rig.Scheduler.Pump();
            rig.Exec.RunAll();                       // 任务 1 已算完（读的是第一次编辑后的数据）
            EditChunk(rig.View, k10, h => h[31 * 64 + 31] += 3);
            rig.Scheduler.Pump();                    // 收回任务 1：数据已变，丢弃；派发任务 2
            Check(rig.Batches.Count == published, "out of order: a result built before a newer edit is not published");
            rig.Settle();
            Check(rig.Batches.Count == published + 1, "out of order: the newer rebuild publishes");
            var k20 = new ChunkKey(2, 0);
            EditChunk(rig.View, k20, h => h[20 * 64 + 20] += 3);
            rig.Scheduler.Pump();                    // 任务 A
            EditChunk(rig.View, k20, h => h[21 * 64 + 21] += 3);
            rig.Scheduler.Pump();                    // 任务 B（A 还在执行器里）
            Check(rig.Exec.Queue.Count == 2, "out of order: superseded job still occupies the executor");
            rig.Exec.RunAt(1); rig.Scheduler.Pump(); // B 先完成并发布
            rig.Exec.RunAt(0); rig.Scheduler.Pump(); // A 后完成，丢弃
            rig.CheckAllCurrent();
            long discardedBefore = rig.Scheduler.Stats.Discarded;
            Check(discardedBefore >= 2, $"out of order: stale results counted as discarded ({discardedBefore})");

            // 5. 卸下邻块后重新装入原始块：没确认前不重建（不把原始块当覆盖块已确认的数据），确认后补齐。
            EditChunk(rig.View, k01, h => { for (int i = 0; i < 64; i++) h[i] += 11; });  // 块 (0,1) 的第 0 行，影响 (0,0)
            rig.Settle();
            rig.View.Unload(k01);
            rig.Scheduler.Pump();
            Check(rig.Shown(k00, out _, out bool fresh00) && !fresh00, "unload: dependent mesh reported expired");
            rig.View.LoadBase(k01);                  // 只有原始块，覆盖块（服务端的全量）还没到
            int count = rig.Batches.Count;
            rig.Settle();
            Check(rig.Batches.Count == count, "reload: the base chunk is not used before it is confirmed");
            EditChunk(rig.View, k01, h => { for (int i = 0; i < 64; i++) h[i] += 11; });  // 全量重发到达
            rig.Scheduler.Confirm(k01);
            rig.Settle();
            rig.CheckAllCurrent();

            // 6. 断线重连：会话代际加一，在途任务的结果即使数据没变也不发布；重新确认后重建。
            EditChunk(rig.View, k11, h => h[10 * 64 + 10] += 5);
            rig.Scheduler.Pump();
            Check(rig.Exec.Queue.Count == 1, "reconnect: one job in flight");
            rig.Exec.RunAll();                       // 上一个会话的任务已算完、还没收回
            rig.Scheduler.BeginSession();
            rig.ConfirmAll();
            int beforeSession = rig.Batches.Count;
            rig.Scheduler.Pump();
            Check(rig.Batches.Count == beforeSession, "reconnect: the job from the previous session is not published, though its data did not change since");
            foreach (var k in all) Check(rig.Shown(k, out _, out bool cur) && !cur, "reconnect: old meshes are reported expired");
            rig.Settle();
            rig.CheckAllCurrent();
            Check(all.All(k => rig.Shown(k, out var bb, out _) && bb!.Session == rig.Scheduler.Session), "reconnect: everything rebuilt for the new session");

            // 7. LOD：改一块的级别，四个共边邻块一起重建接缝。
            rig.Want(k10, 2);
            rig.Scheduler.Pump();
            Check(rig.Exec.Queue.Count == 4, $"lod change: the chunk and its three in-map edge neighbours rebuild ({rig.Exec.Queue.Count})");
            rig.Settle();
            rig.CheckAllCurrent();
            Check(rig.Shown(k00, out var left, out _) && left!.Input.RightLod == 2, "lod change: the left neighbour is stitched to LOD 2");

            // 8. 不再显示：撤下、通知 Retired；邻块重建接缝。
            rig.Drop(k10);
            rig.Settle();
            Check(rig.Batches.Any(x => x.Retired.Contains(k10)), "drop: retired");
            Check(rig.Shown(k00, out left, out _) && left!.Input.RightLod == null, "drop: neighbour stitched to no neighbour");
            rig.CheckAllCurrent();
        }

        // 9. 基础包 / 水位换版：旧地形的在途结果不发布，换版后全部按新地形与新水位重建。
        {
            var rig = new Rig(ViewOf(pack, all));
            rig.ConfirmAll();
            foreach (var k in all) rig.Want(k, 0);
            rig.Settle();
            // 新包与旧包高度逐点相同，只有水位不同（换水位就是换包）：旧地形的结果数据上“看起来”仍然新鲜，只有地形代际能拦住。
            var stored = all.Select(k => { var c = pack.ReadChunk(k); return Deflate(Plain(c.Heights, c.Materials)); }).ToArray();
            int newLevel = meta.WaterLevel + 300;
            using var next = TerrainPack.Open(BuildPack(meta.BaseRevision + 1, meta.ChunksX, meta.ChunksY, meta.HeightBase, newLevel, stored));
            var nextView = ViewOf(next, all);
            rig.Want(k11, 1);                        // 数据不变、只改 LOD：在途任务读的高度与新包相同
            rig.Scheduler.Pump();
            Check(rig.Exec.Queue.Count > 0, "switch: jobs in flight on the old terrain");
            rig.Exec.RunAll();                       // 旧地形的任务已算完、还没收回
            int beforeSwitch = rig.Batches.Count;
            rig.Scheduler.SwitchTerrain(nextView);
            rig.Current = nextView;
            rig.ConfirmAll();
            rig.Scheduler.Pump();                    // 收回：属于旧地形，丢弃（OnPublished 还检查水位与数据）
            Check(rig.Batches.Count == beforeSwitch, "switch: results of the old terrain are not published, though the heights are identical");
            rig.Settle();
            rig.CheckAllCurrent();
            Check(all.All(k => rig.Shown(k, out var b, out _) && b!.Terrain == 2 && b.Shore.WaterLevel == newLevel), "switch: rebuilt for the new terrain and water level");
        }

        // 10. 有界：随机编辑 / LOD / 显示集合 / 卸载重装 / 重连 / 乱序执行，执行器积压 ≤ MaxInFlight、待重建 ≤ 显示集合；
        //     每次发布都满足不变量；最后全部稳定为与当前数据一致的结果。
        {
            var rig = new Rig(ViewOf(pack, all), maxInFlight: 2, maxChunks: 6);
            rig.ConfirmAll();
            var random = new Random(20261110);
            foreach (var k in all) rig.Want(k, random.Next(TerrainLod.MaxLevel + 1));
            int ops = 0, randomBatches = 0;
            for (int step = 0; step < 4000; step++)
            {
                var key = all[random.Next(all.Length)];
                switch (random.Next(14))
                {
                    case 0: case 1: case 2:
                        if (rig.View.GetChunk(key) != null)
                        {
                            int i0 = random.Next(64), j0 = random.Next(64), w = random.Next(1, 6);
                            EditChunk(rig.View, key, h => { for (int j = j0; j < Math.Min(64, j0 + w); j++) for (int i = i0; i < Math.Min(64, i0 + w); i++) h[j * 64 + i] += (ushort)random.Next(1, 30); });
                        }
                        break;
                    case 3: rig.Want(key, random.Next(TerrainLod.MaxLevel + 1)); break;
                    case 4: if (random.Next(3) == 0) rig.Drop(key); else rig.Want(key, random.Next(TerrainLod.MaxLevel + 1)); break;
                    case 5:
                        if (random.Next(3) == 0) { rig.View.Unload(key); rig.View.LoadBase(key); }
                        if (random.Next(2) == 0) rig.Scheduler.Confirm(key);
                        break;
                    case 6:
                        if (random.Next(8) == 0) { rig.Scheduler.BeginSession(); if (random.Next(5) != 0) rig.ConfirmAll(); }
                        else rig.Scheduler.Confirm(key);
                        break;
                    case 7: case 8: case 9:
                        if (rig.Exec.Queue.Count > 0) rig.Exec.RunAt(random.Next(rig.Exec.Queue.Count));
                        break;
                    default:
                        int batches = rig.Batches.Count;
                        rig.Scheduler.Pump();
                        randomBatches += rig.Batches.Count - batches;
                        break;
                }
                var stats = rig.Scheduler.Stats;
                Check(rig.Exec.Queue.Count <= 2 && stats.InFlight <= 2, $"bounded: executor backlog {rig.Exec.Queue.Count}, in flight {stats.InFlight}");
                Check(stats.Pending <= stats.Wanted && stats.Wanted <= 6 && stats.Held <= stats.Wanted, "bounded: pending and held within the wanted set");
                ops++;
            }
            var mid = rig.Scheduler.Stats;
            Check(randomBatches > 50, $"random phase published {randomBatches} batches");
            foreach (var k in all) { if (rig.View.GetChunk(k) == null) rig.View.LoadBase(k); rig.Want(k, 1); }
            rig.ConfirmAll();
            rig.Settle();
            rig.CheckAllCurrent();
            Check(rig.Exec.MaxQueued <= 2, "bounded: executor never held more than MaxInFlight jobs");
            var small = new Rig(ViewOf(pack, all), maxChunks: 2);
            small.Want(k00, 0); small.Want(k10, 0);
            Throws<InvalidOperationException>(() => small.Want(new ChunkKey(2, 0), 0), "more chunks than MaxChunks");
            Console.WriteLine($"PASS terrain rebuild scheduler: late / unconfirmed neighbours, edge edit published as one group, interior edit alone, out-of-order and superseded jobs dropped, " +
                $"unload/reload, reconnect, LOD stitching, drop, base/water switch; random {ops} ops: dispatched {mid.Dispatched}, published {mid.Published} in {randomBatches} batches, discarded {mid.Discarded}, executor backlog ≤ 2, then settled current");
        }
    }
}
