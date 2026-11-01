using System;
using System.Collections.Generic;

namespace Roost.Client
{
    // 消费线程独占；输入必须是值或不可变对象。重连/重生须创建新的历史。
    // 满容量时丢最旧输入并暂停预测，直到权威 ACK 覆盖缺口；绝不伪造 ACK。
    public sealed class PredictionHistory<T>
    {
        private readonly List<(ulong Sequence, T Input)> entries;
        private readonly int capacity;
        private ulong sent, acknowledged, lostThrough;
        public int Count => entries.Count;
        public bool Held => acknowledged < lostThrough;
        public T this[int index] => entries[index].Input;
        public PredictionHistory(int capacity = 128)
        {
            if (capacity < 1 || capacity > 65536)
                throw new ArgumentOutOfRangeException(nameof(capacity));
            this.capacity = capacity;
            entries = new List<(ulong, T)>(capacity);
        }
        public void Record(ulong sequence, T input)
        {
            if (sequence <= sent)
                throw new ArgumentException("Prediction input order.");
            if (entries.Count == capacity)
            {
                lostThrough = entries[0].Sequence;
                entries.RemoveAt(0);
            }
            entries.Add((sequence, input));
            sent = sequence;
        }
        public bool Acknowledge(ulong sequence, out T lastInput)
        {
            if (sequence < acknowledged || sequence > sent)
                throw new ArgumentException("Prediction acknowledgement order.");
            int count = 0;
            lastInput = default!;
            while (count < entries.Count && entries[count].Sequence <= sequence)
                lastInput = entries[count++].Input;
            entries.RemoveRange(0, count);
            acknowledged = sequence;
            return count != 0;
        }
        // clone 必须隔离可变状态；simulate 必须是纯模拟，不得发 RPC、落库或发奖。
        // 邮箱合并输入应自行按时间片重放，不能把每条输入当成一个服务端 tick。
        public TState Replay<TState>(TState authority, Func<TState, TState> clone, Func<TState, T, TState> simulate)
        {
            if (clone == null || simulate == null)
                throw new ArgumentNullException(nameof(clone));
            TState state = clone(authority);
            if (!Held)
                foreach (var entry in entries)
                    state = simulate(state, entry.Input);
            return state;
        }
    }
}
