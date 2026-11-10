using System;
using System.IO;
using System.Security.Cryptography;
using System.Text.Json;
using Roost.Client.Terrain;

// 地形 SDK 的测试入口：dotnet run --project client/dotnet/Roost.Client.Terrain.Tests -- client/spec
// 金样（Go 写出的 terrain-golden.pack + terrain-queries.json）、坏包 / 坏块、覆盖块规则、mesh 网格与 DAO 同步夹具（地形块、
// 建筑）。
internal static partial class Program
{
    private static void Check(bool ok, string message) { if (!ok) throw new Exception("FAIL " + message); }

    // run 必须抛出恰好 T 类型的异常；返回它供进一步检查。
    private static T Throws<T>(Action run, string why) where T : Exception
    {
        try { run(); }
        catch (Exception error) when (error.GetType() == typeof(T)) { return (T)error; }
        catch (Exception error) { throw new Exception($"FAIL {why}: threw {error.GetType().Name}: {error.Message}"); }
        throw new Exception("FAIL accepted " + why);
    }

    private static string Hex(byte[] data) => Convert.ToHexString(data).ToLowerInvariant();
    private static string Sha(byte[] data) => Hex(SHA256.HashData(data));

    private static int Main(string[] args)
    {
        if (args.Length != 1)
        {
            Console.Error.WriteLine("usage: dotnet run --project client/dotnet/Roost.Client.Terrain.Tests -- <client/spec directory>");
            return 2;
        }
        string spec = args[0];
        var golden = JsonDocument.Parse(File.ReadAllText(Path.Combine(spec, "terrain-queries.json"))).RootElement;
        byte[] pack = File.ReadAllBytes(Path.Combine(spec, golden.GetProperty("pack").GetString()!));
        TestGolden(golden, pack, Path.Combine(spec, golden.GetProperty("pack").GetString()!));
        TestNeighbourRules(pack);
        TestBadPacks(pack);
        TestCorruptChunks(pack);
        TestOverlayRules(pack);
        TestConcurrentQueries(pack);
        TestMesh(pack);
        TestDaoSync(pack, Path.Combine(AppContext.BaseDirectory, "Fixtures", "terrain-chunk-sync.json"));
        TestBuildingSync(Path.Combine(AppContext.BaseDirectory, "Fixtures", "building-sync.json"));
        Console.WriteLine("PASS terrain");
        return 0;
    }
}
