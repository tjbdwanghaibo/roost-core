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
