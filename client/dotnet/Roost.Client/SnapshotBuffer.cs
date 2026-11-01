using System;
using System.Collections.Generic;

namespace Roost.Client
{
    // 仅用于位姿等表现数据；不代替 EntityViews 的权威版本应用和删除回调。
    // 时间戳必须属于同一个服务端时钟域。输入为不可变快照；换 epoch/实体删除时 Clear。
    public sealed class SnapshotBuffer<T>
    {
        private readonly List<(double Time, T Value)> samples = new List<(double, T)>();
        private readonly int capacity;
        private readonly double minimumDelay, maximumDelay;
        private double lastArrival, jitter, interval;
        private double playhead = double.NegativeInfinity;
        public double Delay { get; private set; }
        public int Count => samples.Count;
        public SnapshotBuffer(int capacity = 32, double minimumDelay = 0.05, double maximumDelay = 0.2)
        {
            if (capacity < 2 || capacity > 65536 || !NetworkClock.Finite(minimumDelay) ||
                !NetworkClock.Finite(maximumDelay) || minimumDelay < 0 || maximumDelay < minimumDelay)
                throw new ArgumentOutOfRangeException(nameof(capacity));
            this.capacity = capacity;
            this.minimumDelay = minimumDelay;
            this.maximumDelay = maximumDelay;
            Delay = minimumDelay;
        }
        public void Clear()
        {
            samples.Clear();
            jitter = interval = lastArrival = 0;
            Delay = minimumDelay;
            playhead = double.NegativeInfinity;
        }
        public bool Add(double serverTime, double localArrival, T value)
        {
            if (!NetworkClock.Finite(serverTime) || !NetworkClock.Finite(localArrival))
                throw new ArgumentException("Invalid snapshot time.");
            if (samples.Count != 0)
            {
                double gap = serverTime - samples[samples.Count - 1].Time;
                if (gap <= 0 || localArrival < lastArrival)
                    return false;
                double variation = Math.Abs(localArrival - lastArrival - gap);
                jitter += (variation - jitter) * 0.125;
                interval = interval == 0 ? gap : interval + (gap - interval) * 0.125;
                Delay = Math.Max(minimumDelay, Math.Min(maximumDelay, interval + 2 * jitter));
            }
            if (samples.Count == capacity)
                samples.RemoveAt(0);
            samples.Add((serverTime, value));
            lastArrival = localArrival;
            return true;
        }
        public bool TrySample(double estimatedServerNow, Func<T, T, double, T> interpolate, out T value)
        {
            if (!NetworkClock.Finite(estimatedServerNow) || interpolate == null)
                throw new ArgumentException("Invalid sample arguments.");
            value = default!;
            if (samples.Count == 0)
                return false;
            // 延迟自适应增大时冻结显示时间，不能把角色倒放。
            double at = Math.Max(playhead, Math.Max(samples[0].Time,
                Math.Min(samples[samples.Count - 1].Time, estimatedServerNow - Delay)));
            playhead = at;
            if (at <= samples[0].Time)
            {
                value = samples[0].Value;
                return true;
            }
            for (int i = 1; i < samples.Count; i++)
                if (at <= samples[i].Time)
                {
                    var a = samples[i - 1];
                    var b = samples[i];
                    value = interpolate(a.Value, b.Value, (at - a.Time) / (b.Time - a.Time));
                    return true;
                }
            // 无新快照时保持最后权威位姿，不无限外推碰撞/资产。
            value = samples[samples.Count - 1].Value;
            return true;
        }
    }
}
