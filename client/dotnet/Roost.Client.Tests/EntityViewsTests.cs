using System;
using System.Buffers.Binary;
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
        // 在正式 Go 金样上更改帧操作与 subject 基线，验证帧连续但 subject 有缺口时仍拒绝。
        var delta = (byte[])RawSync.Clone();
        delta[6] = 2;
        BinaryPrimitives.WriteUInt32BigEndian(delta.AsSpan(20), source.Tick + 1);
        BinaryPrimitives.WriteUInt32BigEndian(delta.AsSpan(24), source.Tick);
        delta[32] = 2;
        const int subjectOffset = 32 + 9 + 9;
        BinaryPrimitives.WriteUInt16BigEndian(delta.AsSpan(subjectOffset + 6), 0);
        BinaryPrimitives.WriteUInt64BigEndian(delta.AsSpan(subjectOffset + 20), subject.Version + 1);
        BinaryPrimitives.WriteUInt64BigEndian(delta.AsSpan(subjectOffset + 28), subject.Version + 9);
        Refuses(() => views.Receive(delta), "subject gap");
        Check(views.Current.Count == 1 && decoded == 1, "failed frame kept previous view");
        views.Reset();
        Check(views.Current.Count == 0, "connection reset clears views");
        views.Receive(RawSync);
        BinaryPrimitives.WriteUInt64BigEndian(delta.AsSpan(subjectOffset + 28), subject.Version);
        Check(views.Receive(delta) && decoded == 3, "continuous subject delta");
        var remove = new byte[41];
        Array.Copy(delta, remove, 32);
        BinaryPrimitives.WriteUInt32BigEndian(remove.AsSpan(20), source.Tick + 2);
        BinaryPrimitives.WriteUInt32BigEndian(remove.AsSpan(24), source.Tick + 1);
        remove[32] = 3;
        BinaryPrimitives.WriteUInt16BigEndian(remove.AsSpan(33), source.Objects[0].Id);
        BinaryPrimitives.WriteUInt16BigEndian(remove.AsSpan(35), source.Objects[0].Generation);
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
        var emptyFull = new byte[32];
        Array.Copy(RawSync, emptyFull, 32);
        BinaryPrimitives.WriteUInt32BigEndian(emptyFull.AsSpan(20), source.Tick + 3);
        BinaryPrimitives.WriteUInt16BigEndian(emptyFull.AsSpan(30), 0);
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
