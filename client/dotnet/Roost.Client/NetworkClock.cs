using System;
using System.Buffers.Binary;
using System.Diagnostics;
using System.IO;

namespace Roost.Client
{
    public readonly struct ClockSample
    {
        public double Sent { get; }
        public double Received { get; }
        public double ServerReceived { get; }
        public double ServerSent { get; }
        public ClockSample(double sent, double received, double serverReceived, double serverSent)
        {
            Sent = sent;
            Received = received;
            ServerReceived = serverReceived;
            ServerSent = serverSent;
        }
        public static double LocalNow => (double)Stopwatch.GetTimestamp() / Stopwatch.Frequency;
        public static ClockSample Decode(double sent, double received, ReadOnlySpan<byte> payload)
        {
            if (payload.Length != 20 || payload[0] != 'R' || payload[1] != 'C' || payload[2] != 'N' ||
                payload[3] != '1')
                throw new InvalidDataException("Invalid Gate clock response.");
            return new ClockSample(sent, received, BinaryPrimitives.ReadInt64BigEndian(payload.Slice(4, 8)) / 1e9,
                                   BinaryPrimitives.ReadInt64BigEndian(payload.Slice(12, 8)) / 1e9);
        }
    }

    // 消费线程独占。只估计提供样本的服务器时钟域；重连/换服必须 Reset。
    // RTT 包含本地排队、网络、Gate 写队列和 Pump 延迟，不能解释为纯网络耗时。
    public sealed class NetworkClock
    {
        private double offset, target, lastLocal, lastSample = double.NegativeInfinity;
        public bool Ready { get; private set; }
        public double RoundTrip { get; private set; }
        public double Uncertainty => Ready ? RoundTrip / 2 + Math.Abs(target - offset) : 0;
        public double MaxRoundTrip { get; }
        public double SlewRate { get; }
        public NetworkClock(double maxRoundTrip = 2, double slewRate = 0.05)
        {
            if (!Finite(maxRoundTrip) || maxRoundTrip <= 0 || !Finite(slewRate) || slewRate <= 0 || slewRate >= 1)
                throw new ArgumentOutOfRangeException(nameof(maxRoundTrip));
            MaxRoundTrip = maxRoundTrip;
            SlewRate = slewRate;
        }
        internal static bool Finite(double value) => !double.IsNaN(value) && !double.IsInfinity(value);
        public void Reset()
        {
            Ready = false;
            lastSample = double.NegativeInfinity;
            RoundTrip = 0;
        }
        public bool Observe(ClockSample sample)
        {
            double elapsed = sample.Received - sample.Sent;
            double processing = sample.ServerSent - sample.ServerReceived;
            double rtt = elapsed - processing;
            if (!Finite(sample.Sent) || !Finite(sample.Received) || !Finite(sample.ServerReceived) ||
                !Finite(sample.ServerSent) || sample.Received <= lastSample || elapsed < 0 || processing < 0 ||
                rtt < 0 || elapsed > MaxRoundTrip)
                return false;
            // 已有低 RTT 样本时拒绝严重排队的探针，不让桌面卡顿直接改变时钟。
            if (Ready && rtt > Math.Max(0.05, RoundTrip * 3))
                return false;
            double next = sample.ServerSent + rtt / 2 - sample.Received;
            if (!Ready)
            {
                offset = next;
                lastLocal = sample.Received;
                Ready = true;
            }
            target = next;
            RoundTrip = rtt;
            lastSample = sample.Received;
            return true;
        }
        public double Now(double localNow)
        {
            if (!Ready)
                throw new InvalidOperationException("Clock has no sample.");
            if (!Finite(localNow) || localNow < lastLocal)
                throw new ArgumentOutOfRangeException(nameof(localNow));
            double allowance = (localNow - lastLocal) * SlewRate;
            offset += Math.Max(-allowance, Math.Min(allowance, target - offset));
            lastLocal = localNow;
            return localNow + offset;
        }
    }
}
