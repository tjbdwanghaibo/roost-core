using System;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.Text;

namespace Roost.Client.Relay
{
    /// <summary>
    /// lobby 答复用的最小 JSON 读写。SDK 与 Roost.Client 一样不带第三方依赖（netstandard2.1 没有内置 System.Text.Json），
    /// lobby 的答复结构固定且很小，这里只实现读出对象 / 数组 / 字符串 / 数字 / 布尔 / null，并保留每个值的原文
    /// （房间的 attrs 原样交给游戏）。
    /// </summary>
    internal sealed class JsonObject
    {
        public readonly Dictionary<string, object?> Values = new Dictionary<string, object?>(StringComparer.Ordinal);
        public readonly Dictionary<string, string> Raw = new Dictionary<string, string>(StringComparer.Ordinal);

        public string? String(string key) => Values.TryGetValue(key, out var value) ? value as string : null;
        public long Long(string key) => Values.TryGetValue(key, out var value) && value is double number ? (long)number : 0;
        public ulong ULong(string key) => Raw.TryGetValue(key, out var raw) && ulong.TryParse(raw, NumberStyles.None, CultureInfo.InvariantCulture, out var number) ? number : 0;
        public bool Bool(string key) => Values.TryGetValue(key, out var value) && value is bool flag && flag;
        public JsonObject? Object(string key) => Values.TryGetValue(key, out var value) ? value as JsonObject : null;
        public List<object?>? Array(string key) => Values.TryGetValue(key, out var value) ? value as List<object?> : null;
        public string? RawValue(string key) => Raw.TryGetValue(key, out var raw) && raw != "null" ? raw : null;
    }

    internal static class MiniJson
    {
        public static object? Parse(string text)
        {
            int index = 0;
            var value = ParseValue(text, ref index);
            SkipSpace(text, ref index);
            if (index != text.Length) throw new InvalidDataException("Trailing characters after JSON value.");
            return value;
        }

        private static object? ParseValue(string text, ref int index)
        {
            SkipSpace(text, ref index);
            if (index >= text.Length) throw new InvalidDataException("Unexpected end of JSON.");
            char c = text[index];
            switch (c)
            {
                case '{': return ParseObject(text, ref index);
                case '[': return ParseArray(text, ref index);
                case '"': return ParseString(text, ref index);
                case 't': Expect(text, ref index, "true"); return true;
                case 'f': Expect(text, ref index, "false"); return false;
                case 'n': Expect(text, ref index, "null"); return null;
            }
            int start = index;
            while (index < text.Length && "+-0123456789.eE".IndexOf(text[index]) >= 0) index++;
            if (start == index || !double.TryParse(text.Substring(start, index - start), NumberStyles.Float, CultureInfo.InvariantCulture, out var number))
                throw new InvalidDataException($"Invalid JSON value at {start}.");
            return number;
        }

        private static JsonObject ParseObject(string text, ref int index)
        {
            var result = new JsonObject();
            index++; // {
            SkipSpace(text, ref index);
            if (index < text.Length && text[index] == '}') { index++; return result; }
            while (true)
            {
                SkipSpace(text, ref index);
                if (index >= text.Length || text[index] != '"') throw new InvalidDataException("Expected a JSON object key.");
                string key = ParseString(text, ref index);
                SkipSpace(text, ref index);
                if (index >= text.Length || text[index] != ':') throw new InvalidDataException("Expected ':' in JSON object.");
                index++;
                SkipSpace(text, ref index);
                int start = index;
                result.Values[key] = ParseValue(text, ref index);
                result.Raw[key] = text.Substring(start, index - start);
                SkipSpace(text, ref index);
                if (index < text.Length && text[index] == ',') { index++; continue; }
                if (index < text.Length && text[index] == '}') { index++; return result; }
                throw new InvalidDataException("Expected ',' or '}' in JSON object.");
            }
        }

        private static List<object?> ParseArray(string text, ref int index)
        {
            var result = new List<object?>();
            index++; // [
            SkipSpace(text, ref index);
            if (index < text.Length && text[index] == ']') { index++; return result; }
            while (true)
            {
                result.Add(ParseValue(text, ref index));
                SkipSpace(text, ref index);
                if (index < text.Length && text[index] == ',') { index++; continue; }
                if (index < text.Length && text[index] == ']') { index++; return result; }
                throw new InvalidDataException("Expected ',' or ']' in JSON array.");
            }
        }

        private static string ParseString(string text, ref int index)
        {
            var builder = new StringBuilder();
            index++; // "
            while (index < text.Length)
            {
                char c = text[index++];
                if (c == '"') return builder.ToString();
                if (c != '\\') { builder.Append(c); continue; }
                if (index >= text.Length) break;
                char escape = text[index++];
                switch (escape)
                {
                    case '"': builder.Append('"'); break;
                    case '\\': builder.Append('\\'); break;
                    case '/': builder.Append('/'); break;
                    case 'b': builder.Append('\b'); break;
                    case 'f': builder.Append('\f'); break;
                    case 'n': builder.Append('\n'); break;
                    case 'r': builder.Append('\r'); break;
                    case 't': builder.Append('\t'); break;
                    case 'u':
                        if (index + 4 > text.Length) throw new InvalidDataException("Truncated JSON \\u escape.");
                        builder.Append((char)int.Parse(text.Substring(index, 4), NumberStyles.HexNumber, CultureInfo.InvariantCulture));
                        index += 4;
                        break;
                    default: throw new InvalidDataException("Invalid JSON escape.");
                }
            }
            throw new InvalidDataException("Unterminated JSON string.");
        }

        private static void Expect(string text, ref int index, string word)
        {
            if (string.CompareOrdinal(text, index, word, 0, word.Length) != 0) throw new InvalidDataException($"Invalid JSON literal at {index}.");
            index += word.Length;
        }

        private static void SkipSpace(string text, ref int index)
        {
            while (index < text.Length && (text[index] == ' ' || text[index] == '\t' || text[index] == '\n' || text[index] == '\r')) index++;
        }

        /// <summary>写一个 JSON 对象：值可以是 string、int、long、bool 或 <see cref="RawJson"/>（原文写入）；null 的项不写。</summary>
        public static string WriteObject(IEnumerable<KeyValuePair<string, object?>> fields)
        {
            var builder = new StringBuilder("{");
            foreach (var field in fields)
            {
                if (field.Value == null) continue;
                if (builder.Length > 1) builder.Append(',');
                WriteString(builder, field.Key);
                builder.Append(':');
                switch (field.Value)
                {
                    case string text: WriteString(builder, text); break;
                    case bool flag: builder.Append(flag ? "true" : "false"); break;
                    case int number: builder.Append(number.ToString(CultureInfo.InvariantCulture)); break;
                    case long number: builder.Append(number.ToString(CultureInfo.InvariantCulture)); break;
                    case RawJson raw: builder.Append(raw.Text); break;
                    default: throw new ArgumentException($"Unsupported JSON value type {field.Value.GetType()}.");
                }
            }
            return builder.Append('}').ToString();
        }

        private static void WriteString(StringBuilder builder, string text)
        {
            builder.Append('"');
            foreach (char c in text)
            {
                switch (c)
                {
                    case '"': builder.Append("\\\""); break;
                    case '\\': builder.Append("\\\\"); break;
                    case '\n': builder.Append("\\n"); break;
                    case '\r': builder.Append("\\r"); break;
                    case '\t': builder.Append("\\t"); break;
                    default:
                        if (c < 0x20) builder.Append("\\u").Append(((int)c).ToString("x4", CultureInfo.InvariantCulture));
                        else builder.Append(c);
                        break;
                }
            }
            builder.Append('"');
        }
    }

    /// <summary>一段原样写进请求的 JSON（房间的自定义属性）。</summary>
    internal sealed class RawJson
    {
        public string Text { get; }
        public RawJson(string text) { Text = text; }
    }
}
