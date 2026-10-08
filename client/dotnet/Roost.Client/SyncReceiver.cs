using System;
using System.IO;

namespace Roost.Client
{
    // 一个连接中的一个Sync流，仅保留基线元数据；在消费线程串行使用。
    // 应用Full先清空旧流状态。Full可只是恢复的第一包，后续Delta继续create。
    public sealed class SyncReceiver
    {
        private ulong room;
        private uint epoch, tick;
        private ushort schema;
        private bool needsFull = true, applying;
        private ulong generation;

        // 切换连接时调用；旧连接包由接入层隔离。回调中Reset也不会被旧基线覆盖。
        public void Reset()
        {
            generation++;
            room = 0; epoch = 0; tick = 0; schema = 0;
            needsFull = true;
        }

        // false表示旧代/重复包；缺口和解码错误抛InvalidDataException。
        // apply失败原样抛出，之后只接受可恢复的新Full；不能继续推进Delta。
        public bool Receive(ReadOnlySpan<byte> raw, Action<SyncFrame> apply)
        {
            if (applying) throw new InvalidOperationException("Recursive Sync receive.");
            if (apply == null) throw new ArgumentNullException(nameof(apply));
            SyncFrame frame;
            try { frame = SyncFrame.Decode(raw); }
            catch { needsFull = true; throw; }
            if (room != 0)
            {
                if (frame.RoomId != room)
                {
                    needsFull = true;
                    throw new InvalidDataException("Sync stream changed without reset.");
                }
                if (frame.Epoch < epoch || (frame.Epoch == epoch && frame.Tick <= tick)) return false;
            }
            if (!frame.Full && (needsFull || frame.Epoch != epoch || frame.BaseTick != tick || frame.SchemaVersion != schema))
            {
                needsFull = true;
                throw new InvalidDataException("Sync baseline gap: a new full snapshot is required.");
            }
            ulong current = generation;
            applying = true;
            needsFull = true;
            try
            {
                apply(frame);
                if (generation != current) throw new InvalidOperationException("Sync receiver reset during application.");
                room = frame.RoomId; epoch = frame.Epoch; tick = frame.Tick; schema = frame.SchemaVersion;
                needsFull = false;
                return true;
            }
            finally { applying = false; }
        }
    }
}
