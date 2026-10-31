using System;
using System.Collections.Generic;
using System.Collections.ObjectModel;
using System.IO;

namespace Roost.Client
{
    public sealed class EntityView
    {
        public ushort ObjectId { get; }
        public ushort Generation { get; }
        public ushort ComponentType { get; }
        public SubjectUpdate Update { get; }
        public object Value { get; }
        internal EntityView(ushort id, ushort generation, ushort component, SubjectUpdate update, object value)
        { ObjectId = id; Generation = generation; ComponentType = component; Update = update; Value = value; }
    }

    public enum EntityChangeKind { Full, Delta, Remove }
    public enum EntityRemovalReason { None, Server, SnapshotReplacement }

    // 这里的实体变化精确到同步组件视图；Previous/Current 是应用前后的完整状态。
    // Delta 的原始字段补丁仍可从 Current.Update.Payload 读取。
    public sealed class EntityChange
    {
        public EntityChangeKind Kind { get; }
        public EntityRemovalReason RemovalReason { get; }
        public EntityView? Previous { get; }
        public EntityView? Current { get; }
        public EntityView Identity => Current ?? Previous!;
        internal EntityChange(EntityChangeKind kind, EntityView? previous, EntityView? current,
            EntityRemovalReason reason = EntityRemovalReason.None)
        { Kind = kind; Previous = previous; Current = current; RemovalReason = reason; }
    }

    // 串行消费线程中的 EntitySync 视图。Full 清场，Delta 校验对象代际与 subject 版本；
    // 解码器必须返回新值，不修改 previous。完整帧解码成功后才替换可读视图。
    public sealed class EntityViews
    {
        private readonly SyncReceiver receiver = new SyncReceiver();
        private readonly Dictionary<(string, string, ushort, uint), Func<SubjectUpdate, object?, object>> codecs =
            new Dictionary<(string, string, ushort, uint), Func<SubjectUpdate, object?, object>>();
        private Dictionary<(ushort, ushort), EntityView> views = new Dictionary<(ushort, ushort), EntityView>();
        private Dictionary<ushort, ushort> generations = new Dictionary<ushort, ushort>();
        private readonly int maxObjects;
        private bool applying;
        private long resetVersion;
        public IReadOnlyDictionary<(ushort ObjectId, ushort ComponentType), EntityView> Current =>
            new ReadOnlyDictionary<(ushort, ushort), EntityView>(views);

        public EntityViews(int maxObjects = 10000)
        { if (maxObjects < 1) throw new ArgumentOutOfRangeException(nameof(maxObjects)); this.maxObjects = maxObjects; }

        // codec 编号只在 namespace/profile 的契约内有意义，不能把所有编码 1 都当成 PB。
        // 编码号必须与服务端 packer 一致；载荷解析由生成的 PB 类型或业务 codec 负责。
        public void Register<T>(string ns, string profile, ushort encoding, uint schema,
            Func<SubjectUpdate, T?, T> decode) where T : class
        {
            if (decode == null) throw new ArgumentNullException(nameof(decode));
            if (applying) throw new InvalidOperationException("Cannot register a codec while applying a frame.");
            codecs.Add((ns, profile, encoding, schema), (update, previous) =>
                decode(update, (T?)previous) ?? throw new InvalidDataException("Entity codec returned null."));
        }

        public void Reset()
        {
            resetVersion++; receiver.Reset();
            views = new Dictionary<(ushort, ushort), EntityView>();
            generations = new Dictionary<ushort, ushort>();
        }

        public bool Receive(ReadOnlySpan<byte> raw, Action<SyncFrame>? applied = null,
            Action<IReadOnlyList<EntityChange>>? changed = null)
        {
            if (applying) throw new InvalidOperationException("Recursive EntitySync receive.");
            applying = true;
            try
            {
                SyncFrame? committed = null;
                IReadOnlyList<EntityChange>? changes = null;
                bool accepted = receiver.Receive(raw, frame =>
                {
                    changes = Apply(frame, resetVersion);
                    committed = frame;
                });
                // 业务通知发生在 receiver 确认基线之后。回调异常不能使已提交帧变成缺口。
                if (accepted) { changed?.Invoke(changes!); applied?.Invoke(committed!); }
                return accepted;
            }
            finally { applying = false; }
        }

        private IReadOnlyList<EntityChange> Apply(SyncFrame frame, long reset)
        {
            var changes = new List<EntityChange>();
            var next = frame.Full ? new Dictionary<(ushort, ushort), EntityView>() : new Dictionary<(ushort, ushort), EntityView>(views);
            var ids = frame.Full ? new Dictionary<ushort, ushort>() : new Dictionary<ushort, ushort>(generations);
            foreach (var obj in frame.Objects)
            {
                bool exists = ids.TryGetValue(obj.Id, out var knownGeneration);
                if (obj.Operation == 1)
                {
                    if (exists || ids.Count >= maxObjects) throw new InvalidDataException("Entity create conflicts or exceeds object capacity.");
                    ids.Add(obj.Id, obj.Generation);
                }
                else if (!exists || knownGeneration != obj.Generation)
                    throw new InvalidDataException("Entity update/remove has no matching generation.");
                if (obj.Operation == 3)
                {
                    ids.Remove(obj.Id);
                    var removed = new List<(ushort, ushort)>();
                    foreach (var key in next.Keys) if (key.Item1 == obj.Id) removed.Add(key);
                    foreach (var key in removed)
                    {
                        changes.Add(new EntityChange(EntityChangeKind.Remove, next[key], null, EntityRemovalReason.Server));
                        next.Remove(key);
                    }
                    continue;
                }
                foreach (var component in obj.Components)
                {
                    var key = (obj.Id, component.TypeId);
                    if (component.Operation == 2)
                    {
                        if (next.TryGetValue(key, out var removed))
                            changes.Add(new EntityChange(EntityChangeKind.Remove, removed, null, EntityRemovalReason.Server));
                        next.Remove(key); continue;
                    }
                    if (component.SchemaVersion != 1) throw new InvalidDataException("Unsupported EntitySync envelope schema.");
                    var update = SubjectUpdate.Decode(component.Data);
                    next.TryGetValue(key, out var previous);
                    bool sameIdentity = previous != null && previous.Update.SubjectId == update.SubjectId &&
                        previous.Update.SubjectKind == update.SubjectKind && previous.Update.Namespace == update.Namespace &&
                        previous.Update.Profile == update.Profile && previous.Update.Encoding == update.Encoding &&
                        previous.Update.SchemaVersion == update.SchemaVersion && previous.Update.Lod == update.Lod;
                    if (!update.Full && (!sameIdentity || previous!.Update.Version != update.BaseVersion || update.Version <= update.BaseVersion))
                        throw new InvalidDataException("Entity subject baseline gap.");
                    if (!codecs.TryGetValue((update.Namespace, update.Profile, update.Encoding, update.SchemaVersion), out var decode))
                        throw new InvalidDataException($"No codec registered for EntitySync namespace={update.Namespace}, profile={update.Profile}, encoding={update.Encoding}, schema={update.SchemaVersion}.");
                    object value = update.Payload.Length == 0 && !update.Full ? previous!.Value : decode(update, update.Full ? null : previous?.Value);
                    var current = new EntityView(obj.Id, obj.Generation, component.TypeId, update, value);
                    next[key] = current;
                    // Full 重建清空解码基线，但通知仍保留相同代际视图的旧快照。
                    if (frame.Full && views.TryGetValue(key, out var old) && old.Generation == obj.Generation) previous = old;
                    changes.Add(new EntityChange(update.Full ? EntityChangeKind.Full : EntityChangeKind.Delta, previous, current));
                }
            }
            if (reset != resetVersion) throw new InvalidOperationException("EntitySync reset during decoding.");
            if (frame.Full)
            {
                var removed = new List<EntityChange>();
                foreach (var pair in views)
                    if (!next.TryGetValue(pair.Key, out var replacement) || replacement.Generation != pair.Value.Generation)
                        removed.Add(new EntityChange(EntityChangeKind.Remove, pair.Value, null, EntityRemovalReason.SnapshotReplacement));
                // 先撤销旧代际，再通知新代际入场。
                changes.InsertRange(0, removed);
            }
            views = next; generations = ids;
            return changes.AsReadOnly();
        }
    }
}
