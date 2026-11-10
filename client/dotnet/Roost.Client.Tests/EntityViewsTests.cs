using System;
using System.IO;
using System.Collections.Generic;
using Roost.Client;

internal static partial class Program
{
    private static void TestEntityViews()
    {
        var source = SyncFrame.Decode(RawSync);
        var subject = SubjectUpdate.Decode(source.Objects[0].Components[0].Data);
        var views = new EntityViews();
        int decoded = 0;
        views.Register<string>(subject.Namespace, subject.Profile, subject.Encoding, subject.SchemaVersion,
            (update, previous) => { decoded++; return Convert.ToHexString(update.Payload); });
        Check(views.Receive(RawSync) && views.Current.Count == 1 && decoded == 1, "create decoded subject view");
        Check(!views.Receive(RawSync) && decoded == 1, "duplicate frame not reapplied");
        // 在正式 Go 金样之后接一帧连续的增量帧，但 subject 基线有缺口：仍拒绝。增量信封不带身份，从基线继承。
        var obj = source.Objects[0];
        var gap = SyncWire.Frame(false, source.Epoch, source.Tick + 1, source.Tick,
            SyncWire.Object(2, obj.Id, obj.Generation, SyncWire.Delta(subject.Version + 9, subject.Version + 10, 8, 43)));
        Refuses(() => views.Receive(gap), "subject gap");
        Check(views.Current.Count == 1 && decoded == 1, "failed frame kept previous view");
        views.Reset();
        Check(views.Current.Count == 0, "connection reset clears views");
        views.Receive(RawSync);
        var delta = SyncWire.Frame(false, source.Epoch, source.Tick + 1, source.Tick,
            SyncWire.Object(2, obj.Id, obj.Generation, SyncWire.Delta(subject.Version, subject.Version + 1, 8, 43)));
        Check(views.Receive(delta) && decoded == 3, "continuous subject delta");
        var inherited = views.Current[(obj.Id, obj.Components[0].TypeId)].Update;
        Check(inherited.SubjectId == subject.SubjectId && inherited.Namespace == subject.Namespace && inherited.Profile == subject.Profile &&
            inherited.Encoding == subject.Encoding && inherited.Version == subject.Version + 1 && !inherited.Full, "delta inherits the subject identity");
        Refuses(() => SubjectUpdate.Decode(SyncWire.Delta(subject.Version, subject.Version + 1, 8, 43)), "delta without a baseline");
        var remove = SyncWire.Frame(false, source.Epoch, source.Tick + 2, source.Tick + 1, SyncWire.Remove(obj.Id, obj.Generation));
        Check(views.Receive(remove) && views.Current.Count == 0, "remove clears view");
        var notifications = new List<EntityChange>();
        views.Reset();
        views.Receive(RawSync, changed: batch => notifications.AddRange(batch));
        Check(notifications.Count == 1 && notifications[0].Kind == EntityChangeKind.Full &&
            notifications[0].Previous == null && notifications[0].Current != null, "full view callback");
        views.Receive(RawSync, changed: batch => notifications.AddRange(batch));
        Check(notifications.Count == 1, "duplicate has no callback");
        bool callbackThrew = false;
        try { views.Receive(delta, changed: batch => { notifications.AddRange(batch); throw new Exception("business callback"); }); }
        catch (Exception e) when (e.Message == "business callback") { callbackThrew = true; }
        Check(callbackThrew && notifications[1].Kind == EntityChangeKind.Delta &&
            notifications[1].Previous != null && notifications[1].Current != null, "delta before/after callback");
        views.Receive(remove, changed: batch => notifications.AddRange(batch));
        Check(notifications.Count == 3 && notifications[2].Kind == EntityChangeKind.Remove &&
            notifications[2].RemovalReason == EntityRemovalReason.Server && notifications[2].Current == null,
            "callback failure preserves baseline for next removal");
        views.Reset(); views.Receive(RawSync);
        var emptyFull = SyncWire.Frame(true, source.Epoch, source.Tick + 3, 0);
        notifications.Clear();
        views.Receive(emptyFull, changed: batch => notifications.AddRange(batch));
        Check(views.Current.Count == 0 && notifications.Count == 1 &&
            notifications[0].RemovalReason == EntityRemovalReason.SnapshotReplacement, "full snapshot removes absent view");
        var unknown = new EntityViews();
        Refuses(() => unknown.Receive(RawSync), "missing view codec");
        Check(unknown.Current.Count == 0, "unknown codec not partly applied");
        var failing = new EntityViews();
        failing.Register<string>(subject.Namespace, subject.Profile, subject.Encoding, subject.SchemaVersion,
            (_, __) => throw new InvalidDataException("bad payload"));
        Refuses(() => failing.Receive(RawSync), "payload decoder failure");
        Check(failing.Current.Count == 0, "decoder failure not committed");
        Console.WriteLine("PASS EntityViews full/delta/remove/reset/subject gap/codec failure");
    }
}
