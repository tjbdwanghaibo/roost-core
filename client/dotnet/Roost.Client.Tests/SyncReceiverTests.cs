using System;
using System.Buffers.Binary;
using System.IO;
using Roost.Client;

internal static partial class Program
{
    private static byte[] SyncPacket(uint epoch, uint tick, uint baseline)
    {
        // 同一正式wire头；空对象帧用于隔离基线契约，业务payload仍由golden测试覆盖。
        var raw = new byte[32];
        BinaryPrimitives.WriteUInt32BigEndian(raw.AsSpan(0), 0x43525031);
        BinaryPrimitives.WriteUInt16BigEndian(raw.AsSpan(4), 1);
        raw[6] = baseline == 0 ? (byte)1 : (byte)2;
        BinaryPrimitives.WriteUInt64BigEndian(raw.AsSpan(8), 1);
        BinaryPrimitives.WriteUInt32BigEndian(raw.AsSpan(16), epoch);
        BinaryPrimitives.WriteUInt32BigEndian(raw.AsSpan(20), tick);
        BinaryPrimitives.WriteUInt32BigEndian(raw.AsSpan(24), baseline);
        BinaryPrimitives.WriteUInt16BigEndian(raw.AsSpan(28), 1);
        return raw;
    }

    private static void TestSyncReceiver()
    {
        var receiver = new SyncReceiver();
        int count = 0;
        Action<SyncFrame> apply = _ => count++;
        Refuses(() => receiver.Receive(SyncPacket(1, 2, 1), apply), "first delta");
        Check(receiver.Receive(SyncPacket(1, 1, 0), apply), "first full");
        Check(receiver.Receive(SyncPacket(1, 2, 1), apply), "following delta");
        Check(!receiver.Receive(SyncPacket(1, 2, 1), apply) && count == 2, "duplicate applied twice");
        Refuses(() => receiver.Receive(SyncPacket(1, 4, 3), apply), "gap");
        Refuses(() => receiver.Receive(SyncPacket(1, 3, 2), apply), "delta after gap");
        Check(receiver.Receive(SyncPacket(2, 1, 0), apply), "new epoch full");
        Check(!receiver.Receive(SyncPacket(1, 5, 4), apply), "old epoch applied");
        var failure = new Exception("partial business application");
        try
        {
            receiver.Receive(SyncPacket(2, 2, 1), _ => throw failure);
            throw new Exception("application failure swallowed");
        }
        catch (Exception error) when (ReferenceEquals(error, failure)) { }
        Refuses(() => receiver.Receive(SyncPacket(2, 2, 1), apply), "failed application kept baseline");
        receiver.Reset();
        Refuses(() => receiver.Receive(SyncPacket(2, 2, 1), apply), "new connection accepted delta");
        try
        {
            receiver.Receive(SyncPacket(1, 1, 0), _ => receiver.Reset());
            throw new Exception("old callback restored reset baseline");
        }
        catch (InvalidOperationException) { }
        Refuses(() => receiver.Receive(SyncPacket(1, 2, 1), apply), "reset callback restored old state");
        Check(receiver.Receive(SyncPacket(1, 1, 0), apply), "reset could not recover");
        Refuses(() => receiver.Receive(new byte[3], apply), "malformed frame");
        Refuses(() => receiver.Receive(SyncPacket(1, 2, 1), apply), "malformed frame kept baseline");
        Console.WriteLine("PASS Sync receiver gap/recovery/application/reset");
    }
}
