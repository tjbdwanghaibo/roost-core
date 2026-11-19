using System;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.Numerics;
using System.Text;

namespace Roost.PhysicsViewer
{
    // 关卡 JSON（gameplay/physics Level，schema 2）里只用于显示的部分：区域与出生点。
    // Unity 的 JsonUtility 读不了数组的数组（Spawn.Positions），所以这里自己读；不依赖 Unity，tests 里对 Go 导出的金样核对。
    // 关卡内容在此之前已经由服务端校验并核对过摘要，这里的检查只保证不拿坏数据去画。
    public static class LevelMarkers
    {
        public struct Region
        {
            public string Id;
            public Vector3 Center, HalfExtents;
            public Quaternion Rotation;
        }
        public struct Spawn
        {
            public string Id;
            public Vector3[] Positions;
            public Quaternion Rotation;
        }
        public static void Parse(string level, out Region[] regions, out Spawn[] spawns, out string defaultSpawn)
        {
            int at = 0;
            if (!(Value(level, ref at, 0) is Dictionary<string, object> root) || Skip(level, at) != level.Length)
                throw new InvalidDataException("Level document");
            var regionList = List(root, "Regions", 64);
            regions = new Region[regionList.Count];
            for (int i = 0; i < regions.Length; i++)
            {
                var item = regionList[i] as Dictionary<string, object> ?? throw new InvalidDataException("Level region");
                regions[i] = new Region { Id = Name(item), Center = Vector(item, "Center"), HalfExtents = Vector(item, "HalfExtents"), Rotation = Rotation(item) };
                if (regions[i].HalfExtents.X <= 0 || regions[i].HalfExtents.Y <= 0 || regions[i].HalfExtents.Z <= 0)
                    throw new InvalidDataException("Level region size");
            }
            var spawnList = List(root, "Spawns", 64);
            spawns = new Spawn[spawnList.Count];
            for (int i = 0; i < spawns.Length; i++)
            {
                var item = spawnList[i] as Dictionary<string, object> ?? throw new InvalidDataException("Level spawn");
                var positions = List(item, "Positions", 20);
                if (positions.Count == 0) throw new InvalidDataException("Level spawn positions");
                spawns[i] = new Spawn { Id = Name(item), Positions = new Vector3[positions.Count], Rotation = Rotation(item) };
                for (int p = 0; p < positions.Count; p++) spawns[i].Positions[p] = Vector(positions[p]);
            }
            root.TryGetValue("DefaultSpawn", out object fallback);
            defaultSpawn = fallback as string ?? "";
        }
        static List<object> List(Dictionary<string, object> owner, string key, int limit)
        {
            if (!owner.TryGetValue(key, out object value) || value == null) return new List<object>(); // Go 的空切片写成 null
            if (!(value is List<object> list) || list.Count > limit) throw new InvalidDataException("Level " + key);
            return list;
        }
        static string Name(Dictionary<string, object> item)
        {
            if (!item.TryGetValue("ID", out object id) || !(id is string name) || name.Length == 0) throw new InvalidDataException("Level marker id");
            return name;
        }
        static Vector3 Vector(Dictionary<string, object> item, string key)
        {
            item.TryGetValue(key, out object value);
            return Vector(value);
        }
        static Vector3 Vector(object value)
        {
            float[] n = Numbers(value, 3);
            return new Vector3(n[0], n[1], n[2]);
        }
        static Quaternion Rotation(Dictionary<string, object> item)
        {
            item.TryGetValue("Rotation", out object value);
            float[] n = Numbers(value, 4);
            var q = new Quaternion(n[0], n[1], n[2], n[3]);
            float norm = q.LengthSquared();
            if (norm == 0) return Quaternion.Identity; // 零值表示单位旋转
            if (Math.Abs(norm - 1) > .001f) throw new InvalidDataException("Level rotation");
            return q;
        }
        static float[] Numbers(object value, int count)
        {
            if (!(value is List<object> list) || list.Count != count) throw new InvalidDataException("Level vector");
            var result = new float[count];
            for (int i = 0; i < count; i++)
            {
                if (!(list[i] is double number) || double.IsNaN(number) || Math.Abs(number) > 1e6) throw new InvalidDataException("Level number");
                result[i] = (float)number;
            }
            return result;
        }
        // 下面是一个只够读关卡文档的 JSON 读取器：对象读成 Dictionary，数组读成 List，数字读成 double。
        static int Skip(string s, int at)
        {
            while (at < s.Length && (s[at] == ' ' || s[at] == '\t' || s[at] == '\n' || s[at] == '\r')) at++;
            return at;
        }
        static object Value(string s, ref int at, int depth)
        {
            at = Skip(s, at);
            if (at >= s.Length || depth > 32) throw new InvalidDataException("Level JSON");
            char c = s[at];
            if (c == '{' || c == '[')
            {
                var fields = c == '{' ? new Dictionary<string, object>() : null;
                var items = c == '[' ? new List<object>() : null;
                char close = c == '{' ? '}' : ']';
                at = Skip(s, at + 1);
                if (at < s.Length && s[at] == close) { at++; return (object)fields ?? items; }
                for (;;)
                {
                    if (fields != null)
                    {
                        at = Skip(s, at);
                        string key = String(s, ref at);
                        at = Skip(s, at);
                        if (at >= s.Length || s[at++] != ':' || fields.ContainsKey(key)) throw new InvalidDataException("Level JSON object");
                        fields[key] = Value(s, ref at, depth + 1);
                    }
                    else items.Add(Value(s, ref at, depth + 1));
                    at = Skip(s, at);
                    if (at >= s.Length) throw new InvalidDataException("Level JSON");
                    char next = s[at++];
                    if (next == close) return (object)fields ?? items;
                    if (next != ',') throw new InvalidDataException("Level JSON separator");
                }
            }
            if (c == '"') return String(s, ref at);
            foreach (var word in new[] { "true", "false", "null" })
                if (string.CompareOrdinal(s, at, word, 0, word.Length) == 0)
                {
                    at += word.Length;
                    return word == "null" ? null : (object)(word == "true");
                }
            int start = at;
            while (at < s.Length && (s[at] >= '0' && s[at] <= '9' || s[at] == '-' || s[at] == '+' || s[at] == '.' || s[at] == 'e' || s[at] == 'E')) at++;
            if (at == start || !double.TryParse(s.Substring(start, at - start), NumberStyles.Float, CultureInfo.InvariantCulture, out double number))
                throw new InvalidDataException("Level JSON value");
            return number;
        }
        static string String(string s, ref int at)
        {
            if (at >= s.Length || s[at++] != '"') throw new InvalidDataException("Level JSON string");
            var text = new StringBuilder();
            for (;;)
            {
                if (at >= s.Length) throw new InvalidDataException("Level JSON string");
                char c = s[at++];
                if (c == '"') return text.ToString();
                if (c < ' ') throw new InvalidDataException("Level JSON string");
                if (c != '\\') { text.Append(c); continue; }
                if (at >= s.Length) throw new InvalidDataException("Level JSON escape");
                char e = s[at++];
                int plain = "\"\\/bfnrt".IndexOf(e);
                if (plain >= 0) { text.Append("\"\\/\b\f\n\r\t"[plain]); continue; }
                if (e != 'u' || at + 4 > s.Length || !ushort.TryParse(s.Substring(at, 4), NumberStyles.AllowHexSpecifier, CultureInfo.InvariantCulture, out ushort unit))
                    throw new InvalidDataException("Level JSON escape");
                text.Append((char)unit);
                at += 4;
            }
        }
    }
}
