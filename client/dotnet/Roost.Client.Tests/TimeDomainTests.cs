using System;
using Roost.Client;

internal static partial class Program
{
    // 场景时间域的外推与服务端 gameplay/timedomain State.At 同一公式：恒等、锚点之前停在锚点、2 倍、时停、单调托底、非法倍率拒绝。
    private static void TestSceneTimeDomain()
    {
        var domain = new SceneTimeDomain();
        Check(domain.Now(5_000) == 5_000 && domain.Rate == 1, "uninitialized domain is the identity");
        domain.Update(1_000_000, 2_000_000, 2);
        Check(domain.At(1_000_500) == 2_001_000, "2x doubles the business time after the anchor");
        Check(domain.At(999_000) == 2_000_000, "before the anchor stays at the anchor");
        Check(domain.Now(1_001_000) == 2_002_000, "now extrapolates");
        domain.Update(1_001_000, 2_002_000, 0);
        Check(domain.Now(1_500_000) == 2_002_000 && domain.Rate == 0, "rate 0 stops domain time");
        // 回到更早的锚点（迟到的旧参数、或估计的服务端时间回退）也不往回走。
        domain.Update(1_000_000, 2_000_000, 1);
        Check(domain.Now(1_000_100) == 2_002_000, "readings never go backwards");
        bool refused = false;
        try { domain.Update(1, 1, double.NaN); } catch (ArgumentOutOfRangeException) { refused = true; }
        Check(refused && domain.AnchorBusinessNanos == 1_000_000, "a NaN rate is refused and changes nothing");
        Console.WriteLine("PASS scene time domain identity/anchor/2x/stop/monotonic/refused rate");
    }
}

internal static partial class Program
{
    // Unity 适配（unity/Runtime/RoostSceneTime.cs，正式源码链接进本工程，UnityEngine 用 UnityAdapterTests 里的替身）：
    // 每帧按估计的服务端业务时间推进场景时间；2 倍速时一帧的推进量是业务时间的 2 倍，时停为 0，时钟没有样本的帧不推进，
    // 迟到的旧参数不让场景时间倒退。
    private static void TestUnitySceneTime()
    {
        var scene = new Roost.Unity.RoostSceneTime();
        scene.Tick();
        Check(scene.NowNanos == 0 && scene.DeltaSeconds == 0, "no business clock: nothing advances");
        long business = 10_000_000_000;
        scene.ServerBusinessNanos = () => business;
        scene.Tick();
        Check(scene.NowNanos == business && scene.DeltaSeconds == 0, "first frame reads the identity and does not jump");
        business += 100_000_000;
        scene.Tick();
        Check(scene.NowNanos == 10_100_000_000 && Math.Abs(scene.DeltaSeconds - 0.1f) < 1e-6, "rate 1 advances with business time");
        scene.Apply(business, scene.NowNanos, 2);
        business += 100_000_000;
        scene.Tick();
        Check(scene.NowNanos == 10_300_000_000 && Math.Abs(scene.DeltaSeconds - 0.2f) < 1e-6 && scene.Rate == 2, "2x advances twice as fast");
        scene.Apply(business, scene.NowNanos, 0);
        business += 500_000_000;
        scene.Tick();
        Check(scene.NowNanos == 10_300_000_000 && scene.DeltaSeconds == 0, "rate 0 stops scene time");
        scene.Apply(1, 1, 0); // 迟到的旧参数：按它算场景时间停在 1 纳秒
        business += 1;
        scene.Tick();
        Check(scene.NowNanos == 10_300_000_000 && scene.DeltaSeconds == 0, "a stale anchor does not move scene time back");
        scene.ServerBusinessNanos = () => throw new InvalidOperationException("Clock has no sample.");
        scene.Tick();
        Check(scene.NowNanos == 10_300_000_000 && scene.DeltaSeconds == 0, "a frame without a clock sample does not advance");
        Console.WriteLine("PASS Unity scene time adapter: identity/1x/2x/stop/stale anchor/no clock");
    }
}
