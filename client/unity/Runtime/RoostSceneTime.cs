#nullable disable
using System;
using Roost.Client;
using UnityEngine;

namespace Roost.Unity
{
    // 场景时间域（服务端 gameplay/timedomain，D22）的 Unity 适配：每帧按场景的倍率推进一份“场景时间”，
    // 动画、倒计时、特效读它，就能跟上服务端的 2 倍速副本、慢放与时停。外推规则全在 Roost.Client.SceneTimeDomain，
    // 这里只把它接到 Unity 的帧循环上。
    //
    // 接线（主线程）：
    //   1. 业务解码场景的同步状态（game-demo 的 SceneDao：time_anchor_business / time_anchor_domain / time_rate），
    //      每次变化调 Apply；
    //   2. 设 ServerBusinessNanos：估计的服务端业务时间（纳秒）。校时探针给的是服务端 UTC
    //      （NetworkClock.Now(NetworkClock.LocalNow)，秒），业务时间 = 它 + 服务端的逻辑偏移（servertime 模块下发）；
    //      没设、或它抛异常（时钟还没有样本）时这一帧不推进；
    //   3. 读 NowNanos / Seconds（场景时间）与 DeltaSeconds（这一帧场景时间推进了多少秒）。
    //
    // 场景时间单调不减（SceneTimeDomain.Now 托底），所以 DeltaSeconds 不会为负；时停时为 0。
    public sealed class RoostSceneTime : MonoBehaviour
    {
        public SceneTimeDomain Domain { get; } = new SceneTimeDomain();
        public Func<long> ServerBusinessNanos { get; set; }
        // NowNanos 是最近一帧的场景时间（纳秒），Seconds 是同一读数的秒；还没推进过时为 0。
        public long NowNanos { get; private set; }
        public double Seconds => NowNanos / 1e9;
        // DeltaSeconds 是最近一帧场景时间推进的秒数；第一帧（还没有上一帧的读数）为 0。
        public float DeltaSeconds { get; private set; }
        public double Rate => Domain.Rate;

        private bool started;

        // Apply 换上场景同步下来的时间域参数（非法倍率抛 ArgumentOutOfRangeException，参数不变）。
        public void Apply(long anchorBusinessNanos, long anchorDomainNanos, double rate) =>
            Domain.Update(anchorBusinessNanos, anchorDomainNanos, rate);

        private void Update() => Tick();

        // Tick 是一帧：读估计的服务端业务时间，换算成场景时间，记下本帧的推进量。
        internal void Tick()
        {
            if (ServerBusinessNanos == null)
            {
                DeltaSeconds = 0;
                return;
            }
            long business;
            try { business = ServerBusinessNanos(); }
            catch (InvalidOperationException) { DeltaSeconds = 0; return; }
            long now = Domain.Now(business);
            DeltaSeconds = started ? (float)((now - NowNanos) / 1e9) : 0f;
            NowNanos = now;
            started = true;
        }
    }
}
