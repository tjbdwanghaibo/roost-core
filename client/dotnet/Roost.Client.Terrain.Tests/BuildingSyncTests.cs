using System;
using System.IO;
using System.Linq;
using System.Text.Json;
using Roost.Dao.Sync;

internal static partial class Program
{
    // 建筑（可编辑地形方案 §27）是普通的同步实体：客户端收到命名空间 "building" 的 Full / Delta，用生成的 Apply 合并成完整的
    // SyncBuildingDao，按 ConfigId 查配置表 building 得到预制体，按 Params 设参数，按 Gx / Gy / Facing / Foundation 摆放。
    // 夹具（Fixtures/building-sync.json，regen.sh 生成）是 Go 生成 DAO 写出的放置全量、只有参数的增量、换成没有参数的配置、
    // 拆除（State 0）；逐步核对 C# Apply 的结果与 Go 的全量相同、摆放字段与 Go 记下的相同、基线不被修改。
    private static void TestBuildingSync(string fixturePath)
    {
        var fixture = JsonDocument.Parse(File.ReadAllText(fixturePath)).RootElement;
        long entityId = fixture.GetProperty("entity_id").GetInt64();
        SyncBuildingDao? previous = null;
        int steps = 0;
        foreach (var step in fixture.GetProperty("steps").EnumerateArray())
        {
            string name = step.GetProperty("name").GetString()!;
            var patch = SyncBuildingDao.Parser.ParseFrom(Convert.FromHexString(step.GetProperty("payload").GetString()!));
            var want = SyncBuildingDao.Parser.ParseFrom(Convert.FromHexString(step.GetProperty("want").GetString()!));
            Check(patch.EntityId == entityId && patch.SyncFormat == 3, name + ": identity");
            if (step.GetProperty("full").GetBoolean()) previous = null;
            var before = previous?.Clone();
            var next = patch.Apply(previous);
            Check(next.Equals(want), name + ": C# Apply equals the Go DAO");
            Check(previous == null || previous.Equals(before), name + ": baseline not mutated");
            var wantParams = step.GetProperty("params").EnumerateArray().Select(v => v.GetInt32()).ToArray();
            Check((next.State?.Value ?? 0) == step.GetProperty("state").GetInt32() &&
                  (next.Owner?.Value ?? 0) == step.GetProperty("owner").GetInt64() &&
                  (next.ConfigId?.Value ?? 0) == step.GetProperty("config_id").GetInt32() &&
                  (next.Params?.Value ?? new Google.Protobuf.Collections.RepeatedField<int>()).SequenceEqual(wantParams) &&
                  (next.Facing?.Value ?? 0) == step.GetProperty("facing").GetInt32() &&
                  (next.Gx?.Value ?? 0) == step.GetProperty("gx").GetInt32() &&
                  (next.Gy?.Value ?? 0) == step.GetProperty("gy").GetInt32() &&
                  (next.Foundation?.Value ?? 0) == step.GetProperty("foundation").GetInt32(), name + ": the fields a client places the prefab by");
            previous = next;
            steps++;
        }
        Check(steps == 4, "building fixture steps");
        // 改参数的增量只带 Params：配置、锚点、状态不在补丁里。
        var delta = SyncBuildingDao.Parser.ParseFrom(Convert.FromHexString(fixture.GetProperty("steps")[1].GetProperty("payload").GetString()!));
        Check(delta.Params != null && delta.ConfigId == null && delta.State == null && delta.Gx == null, "a parameter change carries only the parameters");
        Console.WriteLine("PASS building DAO sync: Go full + parameter delta + config change + removal through the generated C# Apply");
    }
}
