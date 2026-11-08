using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Text.Json;
using Roost.Client.Terrain;
using Roost.Dao.Sync;

internal static partial class Program
{
    // 业务接线（client/README.md“地形”一节的同一段代码）：EntityChanged 收到 TerrainChunk 的 Full / Delta 后，
    // change.Current.Value 就是生成 Apply 合并好的完整 SyncTerrainChunkDao，取出四个字段交给地形视图（高度与材质都是
    // RepeatedField<uint>，SetOverlay 检查值域：高度 ≤ 65535、材质 ≤ 255）。
    // Version 0 的记录（服务端建了实体、第一笔改动还没提交）按原始块看待，不装覆盖块（与服务端装入规则一致）。
    // 返回是否装入了覆盖块。
    private static bool ApplyTerrainChunk(TerrainView terrain, ChunkKey key, SyncTerrainChunkDao dao)
    {
        long version = dao.Version?.Value ?? 0;
        if (version == 0) return false;
        terrain.SetOverlay(key, dao.BaseRevision?.Value ?? 0, version, dao.Heights.Value, dao.Materials.Value);
        return true;
    }

    // Go 生成 DAO 写出的全量 / 增量（Fixtures/terrain-chunk-sync.json，regen.sh 生成）→ C# 生成 Apply 合并 → TerrainView，
    // 每一步与 Go 的合并结果逐字段相等、与 Go View 的查询逐点相等，并收到对应版本的变化通知。
    private static void TestDaoSync(byte[] packBytes, string fixturePath)
    {
        var fixture = JsonDocument.Parse(File.ReadAllText(fixturePath)).RootElement;
        using var pack = TerrainPack.Open(packBytes);
        var view = LoadAll(pack);
        var key = new ChunkKey(fixture.GetProperty("chunk_x").GetInt32(), fixture.GetProperty("chunk_y").GetInt32());
        long entityId = fixture.GetProperty("entity_id").GetInt64();
        var changes = new List<TerrainChunkChange>();
        view.ChunkChanged += changes.Add;
        SyncTerrainChunkDao? previous = null;
        int steps = 0, queries = 0;
        foreach (var step in fixture.GetProperty("steps").EnumerateArray())
        {
            string name = step.GetProperty("name").GetString()!;
            var patch = SyncTerrainChunkDao.Parser.ParseFrom(Convert.FromHexString(step.GetProperty("payload").GetString()!));
            var want = SyncTerrainChunkDao.Parser.ParseFrom(Convert.FromHexString(step.GetProperty("want").GetString()!));
            Check(patch.EntityId == entityId && patch.SyncFormat == 3, name + ": identity");
            // 补丁形状与 Go 记录一致：确认 list delta（按下标的高度 / 材质补丁）真的经过了 C# 的生成 Apply。
            Check((patch.Heights != null) == step.GetProperty("heights_sent").GetBoolean() &&
                  (patch.Heights?.Delta ?? false) == step.GetProperty("heights_delta").GetBoolean(), name + ": heights patch shape");
            if (patch.Heights != null && patch.Heights.Delta)
                Check(patch.Heights.Value.Count == 0 && patch.Heights.Updates.Count > 0 && patch.Heights.Length == 4096, name + ": sparse heights");
            Check((patch.Materials != null) == step.GetProperty("materials_sent").GetBoolean() &&
                  (patch.Materials?.Delta ?? false) == step.GetProperty("materials_delta").GetBoolean(), name + ": materials patch shape");
            if (patch.Materials != null && patch.Materials.Delta)
                Check(patch.Materials.Value.Count == 0 && patch.Materials.Updates.Count > 0 && patch.Materials.Length == 4096, name + ": sparse materials");
            var before = previous?.Clone();
            var next = patch.Apply(previous);
            Check(next.Equals(want), name + ": C# Apply equals the Go DAO");
            Check(previous == null || previous.Equals(before), name + ": baseline not mutated");
            Check(next.Heights.Value.Count == 4096 && next.Materials.Value.Count == 4096, name + ": full chunk after apply");

            int notified = changes.Count;
            Check(ApplyTerrainChunk(view, key, next), name + ": overlay installed");
            long version = step.GetProperty("version").GetInt64();
            Check(changes.Count == notified + 1 && changes[^1].Key == key && changes[^1].Version == version &&
                  changes[^1].MeshesToRebuild.Contains(new ChunkKey(0, 0)), name + ": change notification");
            // 同一份全量再到一次（重连 / 重新进入视野）：同版本同数据，不再通知。
            ApplyTerrainChunk(view, key, next);
            Check(changes.Count == notified + 1, name + ": repeated full is quiet");
            foreach (var q in step.GetProperty("queries").EnumerateArray())
            {
                long x = q.GetProperty("x").GetInt64(), y = q.GetProperty("y").GetInt64();
                var g = view.Probe(x, y);
                Check(g.Height == q.GetProperty("height").GetInt32() && g.Material == q.GetProperty("material").GetInt32() &&
                      g.Water == q.GetProperty("water").GetBoolean(), $"{name}: query ({x}, {y}) got {g.Height}/{g.Material}/{g.Water}");
                queries++;
            }
            previous = next;
            steps++;
        }
        Check(steps == 5, "fixture steps");

        // 旧补丁晚到：生成 Apply 拒绝缺基线的增量（高度与材质各一次）；视图拒绝更旧的版本。
        var delta = SyncTerrainChunkDao.Parser.ParseFrom(Convert.FromHexString(fixture.GetProperty("steps")[1].GetProperty("payload").GetString()!));
        Throws<InvalidDataException>(() => delta.Apply(null), "list delta without a baseline");
        var materialsDelta = SyncTerrainChunkDao.Parser.ParseFrom(Convert.FromHexString(fixture.GetProperty("steps")[2].GetProperty("payload").GetString()!));
        Throws<InvalidDataException>(() => materialsDelta.Apply(null), "materials list delta without a baseline");
        var full = SyncTerrainChunkDao.Parser.ParseFrom(Convert.FromHexString(fixture.GetProperty("steps")[0].GetProperty("payload").GetString()!)).Apply(null);
        Throws<TerrainStaleOverlayException>(() => ApplyTerrainChunk(view, key, full), "older full after newer versions");
        // Version 0（实体已建、未提交过改动，Heights 为空）：保持原始块。
        var empty = new SyncTerrainChunkDao { EntityId = entityId, SyncFormat = 3,
            BaseRevision = new SyncTerrainChunkDaoBaseRevisionField { Value = pack.Meta.BaseRevision },
            Version = new SyncTerrainChunkDaoVersionField(), Heights = new SyncTerrainChunkDaoHeightsField(), Materials = new SyncTerrainChunkDaoMaterialsField() };
        var fresh = LoadAll(pack);
        Check(!ApplyTerrainChunk(fresh, key, empty) && fresh.TryGetVersion(key, out long v) && v == 0, "version 0 keeps the base chunk");
        // 另一版原始地形上的记录：拒绝，提示客户端资源过期。
        var other = full.Clone();
        other.BaseRevision.Value = pack.Meta.BaseRevision + 1;
        Throws<TerrainBaseRevisionException>(() => ApplyTerrainChunk(fresh, key, other), "record on another base revision");
        // 材质超出一个字节（服务端不会写出，按坏数据拒绝、不截断）：视图不变。
        var bad = full.Clone();
        bad.Version.Value = 99;
        bad.Materials.Value[7] = 256;
        Throws<TerrainInvalidChunkException>(() => ApplyTerrainChunk(view, key, bad), "material above 255");
        Check(view.TryGetVersion(key, out long kept) && kept == 5, "a rejected material leaves the view");
        Console.WriteLine($"PASS terrain DAO sync: Go full + heights / materials list delta + whole materials + whole heights through the generated C# Apply, {queries} queries equal Go View");
    }
}
