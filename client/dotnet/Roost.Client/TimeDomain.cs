using System;

namespace Roost.Client
{
    // SceneTimeDomain 是服务端场景时间域（gameplay/timedomain，DESIGN-TODO D22）在客户端的外推：
    // 域时间 D(B) = AnchorDomain + (B − AnchorBusiness) × Rate，B 是服务端业务时间（纳秒）。
    //
    // 三个参数随场景的同步状态下发（game-demo 的 SceneDao：time_anchor_business / time_anchor_domain /
    // time_rate，nopersist,sync）；SDK 不解码业务载荷，由游戏的解码器读出后交给 Update。客户端用自己估计的
    // 服务端业务时间调用 Now，就能在两帧之间按场景的倍率推进动画、倒计时（2 倍速副本、慢放、时停）。
    //
    // 规则与服务端一致：AnchorBusiness 为 0 表示从未改过倍率（恒等映射）；早于锚点的业务时间停在锚点；
    // 倍率 1 精确、0 为时停、其余向下取整。读数单调：参数更新后外推值不会低于之前交出的值
    // （服务端同样以“已交出的最大域时间”托底，改倍率前后两边可能差“发布延迟 × 倍率”，随后一致）。
    public sealed class SceneTimeDomain
    {
        private long _anchorBusiness;
        private long _anchorDomain;
        private double _rate = 1;
        private long _high = long.MinValue;

        public long AnchorBusinessNanos => _anchorBusiness;
        public long AnchorDomainNanos => _anchorDomain;
        // Rate 是当前倍率；从未初始化时为 1。
        public double Rate => _anchorBusiness == 0 ? 1 : _rate;

        // Update 换上同步下来的参数。倍率必须是有限的非负数，否则抛 ArgumentOutOfRangeException、参数不变。
        public void Update(long anchorBusinessNanos, long anchorDomainNanos, double rate)
        {
            if (double.IsNaN(rate) || double.IsInfinity(rate) || rate < 0)
            {
                throw new ArgumentOutOfRangeException(nameof(rate), rate, "time domain rate must be finite and non-negative");
            }
            _anchorBusiness = anchorBusinessNanos;
            _anchorDomain = anchorDomainNanos;
            _rate = rate;
        }

        // At 是业务时间 businessNanos 对应的域时间（不托底，纯公式，与服务端 State.At 相同）。
        public long At(long businessNanos)
        {
            if (_anchorBusiness == 0)
            {
                return businessNanos;
            }
            long delta = businessNanos - _anchorBusiness;
            if (delta <= 0)
            {
                return _anchorDomain;
            }
            return _anchorDomain + Scale(delta, _rate);
        }

        // Now 是按估计的服务端业务时间外推的域时间，单调不减。
        public long Now(long estimatedServerBusinessNanos)
        {
            long value = At(estimatedServerBusinessNanos);
            if (value < _high)
            {
                return _high;
            }
            _high = value;
            return value;
        }

        private static long Scale(long delta, double rate)
        {
            if (rate == 1)
            {
                return delta;
            }
            if (rate == 0)
            {
                return 0;
            }
            return (long)Math.Floor(delta * rate);
        }
    }
}
