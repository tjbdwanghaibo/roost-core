using System;

namespace Roost.Client
{
    public enum ClientStage
    {
        Apply,
        Present
    }
    public readonly struct ClientTraceEvent
    {
        public ClientStage Stage { get; }
        public uint Epoch { get; }
        public uint Tick { get; }
        public double LocalTime { get; }
        public double Duration { get; }
        public ClientTraceEvent(ClientStage stage, uint epoch, uint tick, double localTime, double duration = 0)
        {
            Stage = stage;
            Epoch = epoch;
            Tick = tick;
            LocalTime = localTime;
            Duration = duration;
        }
    }

    // 消费线程中的有界诊断，不存业务载荷。Epoch/Tick 对应服务端 SyncTrace；
    // LocalTime 是本机单调时间，不能直接减服务端 Unix 时间。展示层自行记录 Present。
    public sealed class ClientTrace
    {
        private readonly ClientTraceEvent[] ring;
        private int head, count;
        public ulong Overwritten { get; private set; }
        public ClientTrace(int capacity = 1024)
        {
            if (capacity < 1 || capacity > 1048576)
                throw new ArgumentOutOfRangeException(nameof(capacity));
            ring = new ClientTraceEvent[capacity];
        }
        public void Record(ClientTraceEvent value)
        {
            if (count == ring.Length)
            {
                head = (head + 1) % ring.Length;
                count--;
                Overwritten++;
            }
            ring[(head + count++) % ring.Length] = value;
        }
        public ClientTraceEvent[] Drain()
        {
            var result = new ClientTraceEvent[count];
            for (int i = 0; i < count; i++)
                result[i] = ring[(head + i) % ring.Length];
            head = count = 0;
            return result;
        }
    }
}
