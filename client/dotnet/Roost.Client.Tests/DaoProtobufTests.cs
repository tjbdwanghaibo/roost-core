using System;
using System.IO;
using System.Text.Json;
using Roost.Dao.Sync;

internal static partial class Program
{
    private static void TestDaoProtobuf(string path)
    {
        var rows = JsonDocument.Parse(File.ReadAllText(path)).RootElement;
        var full = SyncHeroDao.Parser.ParseFrom(Convert.FromHexString(rows[0].GetProperty("hex").GetString()!));
        Check(full.EntityId == 9007199254740993L && full.Exp.Value == 1L << 60, "Go/C# 64-bit DAO PB");
        Check(full.Equips.Value[4].Value == null && full.Squad.Value.Count == 1 && full.Squad.Value[0].Value == null, "nullable nested containers");
        var patch = SyncHeroDao.Parser.ParseFrom(Convert.FromHexString(rows[1].GetProperty("hex").GetString()!));
        Check(patch.Name != null && patch.Name.Value == "" && patch.Level != null && patch.Level.Value == 0, "zero scalar presence");
        Check(patch.Items != null && patch.Items.Value.Count == 0 && patch.Friends != null && patch.Friends.Value.Count == 0, "empty container presence");
        Check(patch.Exp == null && patch.Equips == null, "unchanged field absent");
        var merged = patch.Apply(full);
        Check(merged.Name.Value == "" && merged.Level.Value == 0 && merged.Items.Value.Count == 0 && merged.Friends.Value.Count == 0, "replace clears fields");
        Check(merged.Exp.Value == 1L << 60 && merged.Equips.Value.ContainsKey(4), "delta retains untouched state");
        Check(full.Name.Value == "hello" && full.Items.Value.Count == 1, "patch does not mutate old snapshot");
        foreach (var row in JsonDocument.Parse(File.ReadAllText(Path.Combine(Path.GetDirectoryName(path)!, "dao-collections.json"))).RootElement.EnumerateArray())
        {
            var baseline = SyncHeroDao.Parser.ParseFrom(Convert.FromHexString(row.GetProperty("full").GetString()!));
            var before = baseline.Clone();
            var delta = SyncHeroDao.Parser.ParseFrom(Convert.FromHexString(row.GetProperty("patch").GetString()!));
            var expected = SyncHeroDao.Parser.ParseFrom(Convert.FromHexString(row.GetProperty("want").GetString()!));
            Check(delta.Apply(baseline).Equals(expected), "Go/C# collection " + row.GetProperty("name").GetString());
            Check(baseline.Equals(before), "collection baseline unchanged");
        }
        var invalid = new SyncHeroDao { EntityId = full.EntityId, SyncFormat = 3,
            Name = new SyncHeroDaoNameField { Value = "must-not-apply" },
            Friends = new SyncHeroDaoFriendsField { Delta = true, Length = 100 } };
        bool rejected = false;
        try { invalid.Apply(full); } catch (InvalidDataException) { rejected = true; }
        Check(rejected && full.Name.Value == "hello", "invalid splice rejected atomically");
        Console.WriteLine("PASS generated DAO PB Go/C# full/delta/zero/empty/nil/precision");
    }
}
