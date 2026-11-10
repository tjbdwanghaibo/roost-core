using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Threading;

namespace Roost.Client.Terrain
{
    // 执行重建任务的地方（线程池、Unity Job 外壳、测试里手动推进的队列）。Run 不能同步回调调度器。
    public interface ITerrainBuildExecutor
    {
        void Run(Action work);
    }

    // 线程池执行器。
    public sealed class ThreadPoolTerrainBuildExecutor : ITerrainBuildExecutor
    {
        public static readonly ThreadPoolTerrainBuildExecutor Instance = new ThreadPoolTerrainBuildExecutor();
        public void Run(Action work) => ThreadPool.QueueUserWorkItem(_ => work());
    }

    public sealed class TerrainRebuildOptions
    {
        // 同时在显示集合里的块数上限（Want 超出抛异常）。待重建集合按块去重，所以它也是待重建队列的上限。
        public int MaxChunks { get; set; } = 1024;
        // 同时交给执行器、还没被 Pump 收回的任务数上限。
        public int MaxInFlight { get; set; } = 4;
        // 依赖块必须先 Confirm（本会话里它的覆盖块状态已确认）才算就绪；false 时装着就算就绪。
        public bool RequireConfirmation { get; set; } = true;
        public ShoreOptions Shore { get; set; } = ShoreOptions.Default;
    }

    // 一次 Pump 发布的一组结果：渲染侧应在同一帧里整组替换，组内接缝一致、与组外正在显示的结果接缝一致。
    public sealed class TerrainPublishBatch
    {
        public IReadOnlyList<TerrainChunkBuild> Builds { get; }
        // 不再显示的块（Drop 之后）：渲染侧在同一帧里删掉它们的网格。
        public IReadOnlyList<ChunkKey> Retired { get; }
        internal TerrainPublishBatch(List<TerrainChunkBuild> builds, List<ChunkKey> retired) { Builds = builds.AsReadOnly(); Retired = retired.AsReadOnly(); }
    }

    public readonly struct TerrainRebuildStats
    {
        public int Wanted { get; }
        public int Pending { get; }
        public int InFlight { get; }
        public int Held { get; }
        public long Dispatched { get; }
        public long Published { get; }
        // 收回后没有发布的结果：被更新的任务取代、会话 / 地形换代、依赖已变、块已不在显示集合。
        public long Discarded { get; }
        internal TerrainRebuildStats(int wanted, int pending, int inFlight, int held, long dispatched, long published, long discarded)
        { Wanted = wanted; Pending = pending; InFlight = inFlight; Held = held; Dispatched = dispatched; Published = published; Discarded = discarded; }
    }

    // 客户端地形重建调度（可编辑地形 T04，方案 §31）。把“数据变了”变成“哪几块要在后台重建”，并且只发布与当前数据一致、
    // 彼此接缝一致的结果。
    //
    // 输入（都会让相关块变脏、需要重建）：
    //   - 视图的 ChunkChanged：按变化的采样矩形算出读到它的块（TerrainChunkBuilder.Dependents，含法线的一圈邻域），
    //     只改块内部时邻块不重建；卸下（Unloaded）同时撤销这一块的确认。
    //   - Want / Drop：显示集合与每块的 LOD；LOD 变化让四个共边邻块的接缝也变脏。
    //   - Confirm：依赖块在本会话里的覆盖块状态已确认（例如已收到它的全量、或服务端已表明它没改过）。基础包可读不等于覆盖块已确认，
    //     所以缺省要求确认后才算就绪（TerrainRebuildOptions.RequireConfirmation）。
    //   - BeginSession（断线重连 / 重新登录）：会话代际加一，全部确认作废，全部块变脏，在途结果作废。
    //   - SwitchTerrain（基础包换版、水位随包换）：地形代际加一，同上，并改用新视图。
    //
    // 发布前的校验（Pump 里、持锁，逐项都要通过，否则丢弃并重新排队）：
    //   1. 结果来自这一块最新派发的任务（乱序完成的旧任务一律丢弃）；
    //   2. 会话代际、地形代际与当前相同；块仍在显示集合里，本块与四邻的 LOD 与派发时相同；
    //   3. 每个依赖块现在装着、（需要时）已确认，且它被读到的那片采样与快照相同（引用相同或内容相同）——
    //      只比较当前块 Version 不够，邻块的边界变了同样作废。
    // 通过校验的结果先“暂存”，再选出能一起发布的最大集合：其中每一块与每个相邻块（8 邻域）将要显示的结果（同批的新结果，
    // 否则是正在显示的旧结果）接缝一致（TerrainChunkBuild.SeamMatches）。与旧邻居对不上的继续暂存，等邻居的新结果一起发布，
    // 期间渲染侧继续显示上一组一致的结果（TryGetShown 报告它已过期）。相邻块从没显示过时不构成约束（那里本来就是空洞 / 占位）。
    //
    // 有界：每块至多一个待重建标记（去重），待重建 ≤ MaxChunks；交给执行器的任务 ≤ MaxInFlight（收回后才释放名额，
    // 被取代的旧任务也占名额直到收回，所以执行器里的积压同样 ≤ MaxInFlight）；暂存 ≤ 显示集合。
    //
    // 线程：Want / Drop / Confirm / BeginSession / SwitchTerrain / Pump 与视图通知都在一把锁里改状态，可以来自不同线程；
    // Published / Error 在 Pump 的调用线程、锁外触发（Unity 里在主线程调 Pump）。重建在执行器线程上跑，只读不可变快照。
    // 已知限制：某块的邻居长期不就绪（依赖的块一直没装 / 没确认）时，这块与该邻居对不上的新结果会一直暂存——显示集合应只放
    // 依赖能装齐的块（例如视距内收一圈），或 Drop 那个邻居。
    public sealed class TerrainRebuildScheduler : IDisposable
    {
        private readonly object gate = new object();
        private readonly ITerrainBuildExecutor executor;
        private readonly TerrainRebuildOptions options;
        private readonly Dictionary<ChunkKey, Slot> slots = new Dictionary<ChunkKey, Slot>();
        private readonly HashSet<ChunkKey> confirmed = new HashSet<ChunkKey>();
        // 每块上次处理通知（或第一次派发读到它）时的数据，用来算变化矩形；没有记录时按整块变化处理。
        private readonly Dictionary<ChunkKey, TerrainChunkData> seen = new Dictionary<ChunkKey, TerrainChunkData>();
        private readonly LinkedList<ChunkKey> pending = new LinkedList<ChunkKey>();
        private readonly ConcurrentQueue<Job> completed = new ConcurrentQueue<Job>();
        private readonly List<ChunkKey> retired = new List<ChunkKey>();
        private TerrainView view;
        private long session = 1, terrain = 1, jobSeq;
        private int inFlight;
        private long dispatched, published, discarded;
        private bool disposed;

        public event Action<TerrainPublishBatch>? Published;
        public event Action<Exception>? Error;

        public TerrainRebuildScheduler(TerrainView view, ITerrainBuildExecutor executor, TerrainRebuildOptions? options = null)
        {
            this.view = view ?? throw new ArgumentNullException(nameof(view));
            this.executor = executor ?? throw new ArgumentNullException(nameof(executor));
            this.options = options ?? new TerrainRebuildOptions();
            if (this.options.MaxChunks < 1 || this.options.MaxInFlight < 1) throw new ArgumentOutOfRangeException(nameof(options), "MaxChunks and MaxInFlight must be at least 1");
            if (this.options.Shore == null) throw new ArgumentNullException(nameof(options), "Shore options are required");
            view.ChunkChanged += OnChunkChanged;
        }

        public TerrainView View { get { lock (gate) return view; } }
        public long Session { get { lock (gate) return session; } }
        public long Terrain { get { lock (gate) return terrain; } }

        private sealed class Slot
        {
            public int Lod;
            public bool Pending;
            public long LatestJob;
            // 最新派发、还没收回的任务；变脏时标记取消，执行器可以跳过它（结果反正会因第 1 项被丢弃）。
            public Job? Running;
            public TerrainChunkBuild? Held;
            public TerrainChunkBuild? Shown;
        }

        private sealed class Job
        {
            public long Seq;
            public ChunkKey Key;
            public long Session, Terrain;
            public TerrainBuildInput Input = null!;
            public TerrainChunkBuild? Result;
            public Exception? Failure;
            public volatile bool Cancelled;
        }

        // 让块 key 显示（或改 LOD）。新块与 LOD 变化都会排队重建；LOD 变化还让四个共边邻块重建接缝。
        public void Want(ChunkKey key, int lod)
        {
            TerrainLod.Step(lod);
            lock (gate)
            {
                CheckOpen();
                if (!view.Meta.Contains(key)) throw new TerrainOutOfMapException("chunk " + key);
                if (!slots.TryGetValue(key, out var slot))
                {
                    if (slots.Count >= options.MaxChunks) throw new InvalidOperationException($"terrain rebuild: {slots.Count} chunks wanted, at most {options.MaxChunks}");
                    slots[key] = slot = new Slot { Lod = lod };
                    MarkDirty(key);
                    MarkEdgeNeighbours(key);
                    return;
                }
                if (slot.Lod == lod) return;
                slot.Lod = lod;
                MarkDirty(key);
                MarkEdgeNeighbours(key);
            }
        }

        // 不再显示块 key：撤下它的结果（下一次 Pump 的 Retired）、作废它的在途任务，四个共边邻块重建接缝。
        public bool Drop(ChunkKey key)
        {
            lock (gate)
            {
                CheckOpen();
                if (!slots.TryGetValue(key, out var slot)) return false;
                slots.Remove(key);
                if (slot.Pending) pending.Remove(key);
                if (slot.Shown != null) retired.Add(key);
                if (slot.Held != null) discarded++;
                if (slot.Running != null) slot.Running.Cancelled = true;
                MarkEdgeNeighbours(key);
                return true;
            }
        }

        // 块 key 在本会话里的覆盖块状态已确认；它必须装着（否则返回 false、不记）。卸下后确认自动撤销。
        public bool Confirm(ChunkKey key)
        {
            lock (gate)
            {
                CheckOpen();
                if (view.GetChunk(key) == null) return false;
                confirmed.Add(key);
                return true;
            }
        }

        public bool IsConfirmed(ChunkKey key) { lock (gate) return confirmed.Contains(key); }

        // 断线重连 / 重新登录：会话代际加一，全部确认作废，全部块变脏，在途与暂存的结果作废。正在显示的结果保留（过期）。
        public void BeginSession()
        {
            lock (gate)
            {
                CheckOpen();
                session++;
                confirmed.Clear();
                ResetAll();
            }
        }

        // 换一份地形（基础包换版；水位随包换）：地形代际加一，改用新视图，其余同 BeginSession。
        public void SwitchTerrain(TerrainView next)
        {
            if (next == null) throw new ArgumentNullException(nameof(next));
            lock (gate)
            {
                CheckOpen();
                view.ChunkChanged -= OnChunkChanged;
                view = next;
                view.ChunkChanged += OnChunkChanged;
                terrain++;
                confirmed.Clear();
                seen.Clear();
                foreach (var key in new List<ChunkKey>(slots.Keys))
                    if (!next.Meta.Contains(key)) Drop(key);
                ResetAll();
            }
        }

        private void ResetAll()
        {
            foreach (var pair in slots)
            {
                if (pair.Value.Held != null) { pair.Value.Held = null; discarded++; }
                if (pair.Value.Running != null) pair.Value.Running.Cancelled = true;
                MarkDirty(pair.Key);
            }
        }

        // 块 key 正在显示的结果；current 为 false 表示它已过期（数据 / LOD / 会话 / 地形变了，新结果还没发布），
        // 渲染侧可以暂留，但不能把它当作当前的权威数据。
        public bool TryGetShown(ChunkKey key, out TerrainChunkBuild? build, out bool current)
        {
            lock (gate)
            {
                build = null; current = false;
                if (!slots.TryGetValue(key, out var slot) || slot.Shown == null) return false;
                build = slot.Shown;
                current = Fresh(key, slot, slot.Shown, out _);
                return true;
            }
        }

        public TerrainRebuildStats Stats
        {
            get
            {
                lock (gate)
                {
                    int held = 0;
                    foreach (var s in slots.Values) if (s.Held != null) held++;
                    return new TerrainRebuildStats(slots.Count, pending.Count, inFlight, held, dispatched, published, discarded);
                }
            }
        }

        // 一次推进：收回完成的任务并校验、选出可以一起发布的结果、派发新任务；最后在锁外触发 Published / Error。
        public void Pump()
        {
            var failures = new List<Exception>();
            var dispatch = new List<Job>();
            TerrainPublishBatch? batch = null;
            lock (gate)
            {
                CheckOpen();
                Collect(failures);
                batch = Publish();
                Dispatch(dispatch);
            }
            foreach (var job in dispatch)
            {
                var j = job;
                executor.Run(() => Execute(j));
            }
            if (batch != null)
            {
                var handlers = Published;
                if (handlers != null)
                    foreach (Action<TerrainPublishBatch> callback in handlers.GetInvocationList())
                    {
                        try { callback(batch); }
                        catch (Exception error) { failures.Add(error); }
                    }
            }
            foreach (var failure in failures) Report(failure);
        }

        private void Collect(List<Exception> failures)
        {
            while (completed.TryDequeue(out var job))
            {
                inFlight--;
                if (!slots.TryGetValue(job.Key, out var slot) || job.Seq != slot.LatestJob)
                {
                    discarded++;
                    continue;
                }
                slot.Running = null;
                if (job.Session != session || job.Terrain != terrain || (job.Result == null && job.Failure == null))
                {
                    discarded++;
                    continue;
                }
                if (job.Failure != null)
                {
                    // 生成本身出错（不应发生）：不立即重排以免空转，等输入再变时重试。
                    discarded++;
                    failures.Add(job.Failure);
                    continue;
                }
                if (slot.Held != null) discarded++;
                slot.Held = job.Result;
            }
        }

        private TerrainPublishBatch? Publish()
        {
            // 暂存的结果先按当前状态重新校验。
            var candidates = new Dictionary<ChunkKey, TerrainChunkBuild>();
            foreach (var pair in slots)
            {
                var held = pair.Value.Held;
                if (held == null) continue;
                if (Fresh(pair.Key, pair.Value, held, out _)) { candidates[pair.Key] = held; continue; }
                pair.Value.Held = null;
                discarded++;
                MarkDirty(pair.Key);
            }
            // 去掉与相邻块将要显示的结果接缝不一致的，直到不再变化（最大的一致集合）。
            bool changed = true;
            while (changed && candidates.Count > 0)
            {
                changed = false;
                foreach (var pair in new List<KeyValuePair<ChunkKey, TerrainChunkBuild>>(candidates))
                {
                    if (SeamsAgree(pair.Key, pair.Value, candidates)) continue;
                    candidates.Remove(pair.Key);
                    changed = true;
                }
            }
            if (candidates.Count == 0 && retired.Count == 0) return null;
            var builds = new List<TerrainChunkBuild>();
            foreach (var pair in candidates)
            {
                var slot = slots[pair.Key];
                slot.Shown = pair.Value;
                slot.Held = null;
                builds.Add(pair.Value);
            }
            published += builds.Count;
            builds.Sort((a, b) => a.Key.Y != b.Key.Y ? a.Key.Y.CompareTo(b.Key.Y) : a.Key.X.CompareTo(b.Key.X));
            var gone = new List<ChunkKey>(retired);
            retired.Clear();
            return new TerrainPublishBatch(builds, gone);
        }

        private bool SeamsAgree(ChunkKey key, TerrainChunkBuild build, Dictionary<ChunkKey, TerrainChunkBuild> candidates)
        {
            for (int dy = -1; dy <= 1; dy++)
                for (int dx = -1; dx <= 1; dx++)
                {
                    if (dx == 0 && dy == 0) continue;
                    var n = new ChunkKey(key.X + dx, key.Y + dy);
                    if (!slots.TryGetValue(n, out var slot)) continue;
                    var other = candidates.TryGetValue(n, out var fresh) ? fresh : slot.Shown;
                    if (other != null && !build.SeamMatches(other)) return false;
                }
            return true;
        }

        private void Dispatch(List<Job> dispatch)
        {
            var node = pending.First;
            while (node != null && inFlight < options.MaxInFlight)
            {
                var next = node.Next;
                var key = node.Value;
                var slot = slots[key];
                if (Ready(key) && TerrainChunkBuilder.TryCapture(view, key, slot.Lod, LodOf(key, -1, 0), LodOf(key, 1, 0), LodOf(key, 0, -1), LodOf(key, 0, 1),
                        options.Shore, out var input, out _))
                {
                    pending.Remove(node);
                    slot.Pending = false;
                    // 视图里没收到过通知的块（调度器接上之前就装着），以这次读到的数据为“已看到”的基准，之后的通知按它算变化矩形。
                    for (int dy = -1; dy <= 1; dy++)
                        for (int dx = -1; dx <= 1; dx++)
                        {
                            var used = input!.ChunkAt(dx, dy);
                            var dep = new ChunkKey(key.X + dx, key.Y + dy);
                            if (used != null && !seen.ContainsKey(dep)) seen[dep] = used;
                        }
                    var job = new Job { Seq = ++jobSeq, Key = key, Session = session, Terrain = terrain, Input = input! };
                    slot.LatestJob = job.Seq;
                    slot.Running = job;
                    inFlight++;
                    dispatched++;
                    dispatch.Add(job);
                }
                node = next;
            }
        }

        private void Execute(Job job)
        {
            try
            {
                if (!job.Cancelled)
                {
                    var build = TerrainChunkBuilder.Build(job.Input);
                    build.Session = job.Session;
                    build.Terrain = job.Terrain;
                    job.Result = build;
                }
            }
            catch (Exception error) { job.Failure = error; }
            completed.Enqueue(job);
        }

        // 依赖的块都装着、（需要时）都已确认。
        private bool Ready(ChunkKey key)
        {
            foreach (var dep in TerrainChunkBuilder.Dependencies(view.Meta, key))
            {
                if (view.GetChunk(dep) == null) return false;
                if (options.RequireConfirmation && !confirmed.Contains(dep)) return false;
            }
            return true;
        }

        // 校验第 2、3 项（第 1 项在 Collect）：代际、显示集合与 LOD、每个依赖块被读到的采样。
        private bool Fresh(ChunkKey key, Slot slot, TerrainChunkBuild build, out string why)
        {
            var input = build.Input;
            why = "";
            if (build.Session != session) { why = "session"; return false; }
            if (build.Terrain != terrain) { why = "terrain"; return false; }
            if (input.Lod != slot.Lod || input.LeftLod != LodOf(key, -1, 0) || input.RightLod != LodOf(key, 1, 0)
                || input.BottomLod != LodOf(key, 0, -1) || input.TopLod != LodOf(key, 0, 1)) { why = "lod"; return false; }
            for (int dy = -1; dy <= 1; dy++)
                for (int dx = -1; dx <= 1; dx++)
                {
                    var used = input.ChunkAt(dx, dy);
                    if (used == null) continue;
                    var dep = new ChunkKey(key.X + dx, key.Y + dy);
                    var now = view.GetChunk(dep);
                    if (now == null) { why = "unloaded " + dep; return false; }
                    if (options.RequireConfirmation && !confirmed.Contains(dep)) { why = "unconfirmed " + dep; return false; }
                    if (!TerrainChunkBuilder.SameReads(now, used, dx, dy)) { why = "changed " + dep; return false; }
                }
            return true;
        }

        private int? LodOf(ChunkKey key, int dx, int dy) =>
            slots.TryGetValue(new ChunkKey(key.X + dx, key.Y + dy), out var s) ? s.Lod : (int?)null;

        private void MarkDirty(ChunkKey key)
        {
            if (!slots.TryGetValue(key, out var slot) || slot.Pending) return;
            slot.Pending = true;
            pending.AddLast(key);
            if (slot.Running != null) slot.Running.Cancelled = true;
        }

        private void MarkEdgeNeighbours(ChunkKey key)
        {
            MarkDirty(new ChunkKey(key.X - 1, key.Y));
            MarkDirty(new ChunkKey(key.X + 1, key.Y));
            MarkDirty(new ChunkKey(key.X, key.Y - 1));
            MarkDirty(new ChunkKey(key.X, key.Y + 1));
        }

        private void OnChunkChanged(TerrainChunkChange change)
        {
            lock (gate)
            {
                if (disposed) return;
                var source = view;
                var key = change.Key;
                if (change.Kind == TerrainChunkChangeKind.Unloaded) confirmed.Remove(key);
                var now = source.GetChunk(key);
                seen.TryGetValue(key, out var before);
                if (now == null) seen.Remove(key); else seen[key] = now;
                int i0 = 0, i1 = TerrainGeometry.ChunkSamples - 1, j0 = 0, j1 = TerrainGeometry.ChunkSamples - 1;
                if (before != null && now != null && !TerrainChunkBuilder.ChangedBox(before, now, out i0, out i1, out j0, out j1)) return;
                foreach (var dependent in TerrainChunkBuilder.Dependents(source.Meta, key, i0, i1, j0, j1))
                    MarkDirty(dependent);
            }
        }

        private void CheckOpen()
        {
            if (disposed) throw new ObjectDisposedException(nameof(TerrainRebuildScheduler));
        }

        // 订阅者抛错交给 Error（与 TerrainView 的做法相同）；Error 自身抛错被忽略。
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

        // 退订视图；在途任务完成后不再收回。
        public void Dispose()
        {
            lock (gate)
            {
                if (disposed) return;
                disposed = true;
                view.ChunkChanged -= OnChunkChanged;
            }
        }
    }
}
