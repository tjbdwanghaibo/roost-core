using System;
using System.Collections.Generic;

namespace Roost.Client.Terrain
{
    // 把 entitysync 收到的 TerrainChunk 对象接到 TerrainView 与 TerrainRebuildScheduler 的确认上（可编辑地形方案 §31.9）。
    //
    // 确认来源：服务端的地形 Interest 里每一块都是一个 TerrainChunk 对象——改过的块是实体（Version ≥ 1，带高度与材质），
    // 没改过的块是就绪标记（Version 0，只有 BaseRevision）。所以“服务端此刻把这块的状态告诉了本会话”就是“这块的对象在
    // 本会话里”：
    //   - Apply（EntityChanged 的 Full / Delta）：Version ≥ 1 装覆盖块；Version 0 表示这块在当前原始地形包上没改过，用基础包。
    //     装进视图、且 BaseRevision 与视图的包一致后 Confirm。块第一次被改时服务端把同一个对象从 Version 0 换成实体的全量
    //     （中间没有 Remove），这里照常 Apply，确认不中断。
    //   - Remove（对象离开地形视野、或 Full 帧替换掉）：Unconfirm。视图里的数据保留（客户端缓存，不再是当前权威数据）。
    //   - 移动中新进入视距的块：服务端在它进入视野时发全量（标记或实体），与第一次进场相同；没收到之前不确认。
    //   - BeginSession（断线重连 / 重新登录 / GameClient.EntitiesReset）：全部说法作废、调度器会话代际加一；之后的 Full 帧
    //     重新带来视野里的每一块，逐块重新确认。
    //   - SwitchTerrain（换原始地形包）：调度器换视图、地形代际加一，确认全部作废；已收到的说法里 BaseRevision 等于新包的
    //     按新包重新装入并确认（对象没变时服务端不会重发，所以这里要留着说法）。BaseRevision 不同的说法不确认。
    //
    // 视图的 ChunkChanged：业务按视距 Unload 再 LoadBase 一块时，这里按留着的说法补回覆盖块（Version ≥ 1）或重新确认（Version 0）；
    // 没有说法的块不确认。说法与视图对不上（别处往视图里装了别的覆盖块）时撤销确认。
    //
    // 使用约定：覆盖块只经这里写进视图（业务不另调 SetOverlay）；heights / materials 是生成 Apply 返回的新值（之后不再被修改），
    // 这里只留引用。显示集合（Want）要比服务端地形视野内收：一块就绪要 8 个邻块都已确认，服务端按块中心离视点 ≤ 视野半径
    // （game-demo 200m，视点每 8m 上报一次）给对象，所以显示半径约为视野半径 − 视点步长 − 一块的对角线（约 100m）。
    //
    // 线程：公开方法与视图通知都在一把锁里处理，可以来自不同线程（LoadBase 放在后台线程时通知在那个线程上）。锁序是本类型 →
    // 视图写锁 → 调度器锁；视图与调度器都不会持着自己的锁回调这里。
    public sealed class TerrainChunkSync : IDisposable
    {
        private readonly object gate = new object();
        private readonly TerrainRebuildScheduler scheduler;
        private readonly Dictionary<ChunkKey, Record> records = new Dictionary<ChunkKey, Record>();
        private TerrainView view;
        private bool disposed;

        // 服务端对一块的最新说法（对象还在本会话里）。
        private sealed class Record
        {
            public long BaseRevision, Version;
            public IReadOnlyList<uint>? Heights, Materials;
        }

        public TerrainChunkSync(TerrainRebuildScheduler scheduler)
        {
            this.scheduler = scheduler ?? throw new ArgumentNullException(nameof(scheduler));
            view = scheduler.View;
            view.ChunkChanged += OnChunkChanged;
        }

        // 本会话里收到了服务端说法的块数（对象数）。
        public int Count { get { lock (gate) return records.Count; } }

        // 服务端是否告诉过本会话这块的状态（对象在本会话里）；version 是说法里的版本（0 = 没改过）。
        public bool TryGetVersion(ChunkKey key, out long version)
        {
            lock (gate)
            {
                version = 0;
                if (!records.TryGetValue(key, out var r)) return false;
                version = r.Version;
                return true;
            }
        }

        // TerrainChunk 对象的 Full / Delta（change.Current.Value 是生成 Apply 合并好的完整记录）：记下说法，装进视图并确认。
        // 返回这块现在是否已确认（视图里没装着这块的 Version 0 说法要等业务 LoadBase 之后才确认）。
        // 抛错（说法照样记下、这块不确认）：BaseRevision 与视图的包不同（TerrainBaseRevisionException：客户端资源过期，
        // 换包后 SwitchTerrain 会用这份说法）、形状不对（TerrainInvalidChunkException，说法不记）、比视图里的覆盖块旧
        // （TerrainStaleOverlayException）、读包失败（LoadBase 的异常）。块不在地图内抛 TerrainOutOfMapException。
        public bool Apply(ChunkKey key, long baseRevision, long version, IReadOnlyList<uint>? heights, IReadOnlyList<uint>? materials)
        {
            lock (gate)
            {
                CheckOpen();
                if (!view.Meta.Contains(key)) throw new TerrainOutOfMapException("chunk " + key);
                if (version < 0 || (version >= 1 && (heights == null || materials == null ||
                        heights.Count != TerrainGeometry.ChunkPoints || materials.Count != TerrainGeometry.ChunkPoints)))
                {
                    records.Remove(key);
                    scheduler.Unconfirm(key);
                    throw new TerrainInvalidChunkException($"chunk {key}: version {version} with {heights?.Count ?? 0} heights and {materials?.Count ?? 0} materials");
                }
                var record = new Record { BaseRevision = baseRevision, Version = version, Heights = version >= 1 ? heights : null, Materials = version >= 1 ? materials : null };
                records[key] = record;
                return Install(key, record);
            }
        }

        // TerrainChunk 对象被移除（离开地形视野、Full 帧替换）：撤销确认，视图里的数据保留。返回之前是否有说法。
        public bool Remove(ChunkKey key)
        {
            lock (gate)
            {
                CheckOpen();
                scheduler.Unconfirm(key);
                return records.Remove(key);
            }
        }

        // 断线重连 / 重新登录（GameClient.EntitiesReset 或连接版本变化）：全部说法作废，调度器 BeginSession。
        public void BeginSession()
        {
            lock (gate)
            {
                CheckOpen();
                records.Clear();
                scheduler.BeginSession();
            }
        }

        // 换原始地形包：调度器 SwitchTerrain，之后按留着的说法（BaseRevision 等于新包的）重新装入并确认。
        // 重新装入出的错交给返回的列表，不中断其余块。
        public IReadOnlyList<Exception> SwitchTerrain(TerrainView next)
        {
            if (next == null) throw new ArgumentNullException(nameof(next));
            var failures = new List<Exception>();
            lock (gate)
            {
                CheckOpen();
                view.ChunkChanged -= OnChunkChanged;
                scheduler.SwitchTerrain(next);
                view = next;
                view.ChunkChanged += OnChunkChanged;
                foreach (var pair in new List<KeyValuePair<ChunkKey, Record>>(records))
                {
                    if (pair.Value.BaseRevision != next.Meta.BaseRevision) continue;
                    if (!next.Meta.Contains(pair.Key)) { records.Remove(pair.Key); continue; }
                    try { Install(pair.Key, pair.Value); }
                    catch (TerrainException error) { failures.Add(error); }
                }
            }
            return failures.AsReadOnly();
        }

        // 把一份说法装进视图并确认（持锁）。见 Apply 的错误。
        private bool Install(ChunkKey key, Record record)
        {
            if (record.BaseRevision != view.Meta.BaseRevision)
            {
                scheduler.Unconfirm(key);
                throw new TerrainBaseRevisionException($"chunk {key} is on base revision {record.BaseRevision}, the pack is {view.Meta.BaseRevision}");
            }
            if (record.Version == 0)
            {
                var current = view.GetChunk(key);
                if (current == null)
                {
                    // 业务还没装这块：LoadBase 之后（ChunkChanged Loaded）再确认。
                    scheduler.Unconfirm(key);
                    return false;
                }
                if (current.Version != 0)
                {
                    // 视图里留着这块的旧覆盖块（客户端缓存），服务端说它没改过：换回基础包。Loaded 通知里确认。
                    view.Unload(key);
                    view.LoadBase(key);
                    return scheduler.IsConfirmed(key);
                }
                return scheduler.Confirm(key);
            }
            try { view.SetOverlay(key, record.BaseRevision, record.Version, record.Heights!, record.Materials!); }
            catch { scheduler.Unconfirm(key); throw; }
            return scheduler.Confirm(key);
        }

        // 视图通知：业务装入 / 重新装入一块时按说法补回或确认；与说法对不上时撤销确认。卸下由调度器自己撤销确认。
        private void OnChunkChanged(TerrainChunkChange change)
        {
            lock (gate)
            {
                if (disposed || change.Kind == TerrainChunkChangeKind.Unloaded) return;
                var key = change.Key;
                if (!records.TryGetValue(key, out var record) || record.BaseRevision != view.Meta.BaseRevision) return;
                var current = view.GetChunk(key);
                if (current == null) return;
                if (current.Version == record.Version) { scheduler.Confirm(key); return; }
                if (current.Version == 0 && record.Version >= 1)
                {
                    // 业务 Unload 后重新 LoadBase：补回覆盖块（嵌套的 Updated 通知里确认）。
                    try { view.SetOverlay(key, record.BaseRevision, record.Version, record.Heights!, record.Materials!); }
                    catch (TerrainException) { scheduler.Unconfirm(key); throw; }
                    return;
                }
                scheduler.Unconfirm(key);
            }
        }

        private void CheckOpen()
        {
            if (disposed) throw new ObjectDisposedException(nameof(TerrainChunkSync));
        }

        // 退订视图（不影响调度器）。
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
