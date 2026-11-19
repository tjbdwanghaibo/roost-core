using System;
using System.Collections.Generic;
using System.IO;
using System.Text;

namespace Roost.PhysicsViewer
{
    // 关卡流程的公开状态，即 room_input.proto 的 RoomLevelView；与 Go 的 service/physicsroom.DecodeLevelView
    // 同样严格：未知字段、类型不符、重复的标量字段、没有阶段名、变量缺值或带多个值、同名的阶段或变量一律拒绝。
    // 不依赖 Unity，金样与坏包回归在 tests 里跑。
    public sealed class RoomLevelView
    {
        public const int MaxBytes = 16 << 10;
        public struct Stage
        {
            public string Id;
            public ulong Generation;
        }
        public struct Variable
        {
            public string Key, Text; // 键带作用域前缀：global/<名>、stage/<阶段>/<名>、trigger/<阶段>/<触发器>/<名>
            public int Kind;         // oneof 的字段号：2 整数、3 布尔、4 文本
            public long Int;
            public bool Flag;
            public override string ToString() => Kind == 2 ? Int.ToString(System.Globalization.CultureInfo.InvariantCulture) : Kind == 3 ? (Flag ? "true" : "false") : Text;
        }
        public ulong Revision;
        public string Phase = "", Result = "";
        public readonly List<Stage> Stages = new List<Stage>();
        public readonly List<Variable> Variables = new List<Variable>(); // 服务端按键排序

        // 同一条连接上 Revision 只会增加。相同的是重发，保留手上的；变小说明状态来自别处，按协议错误处理。
        public static RoomLevelView Advance(RoomLevelView current, byte[] payload)
        {
            var next = Decode(payload);
            if (current == null || next.Revision > current.Revision) return next;
            if (next.Revision < current.Revision) throw new InvalidDataException("Level view revision went backwards");
            return current;
        }
        public static RoomLevelView Decode(byte[] payload)
        {
            if (payload == null || payload.Length > MaxBytes) throw new InvalidDataException("Level view size");
            var view = new RoomLevelView();
            var names = new HashSet<string>();
            var keys = new HashSet<string>();
            int seen = 0;
            for (int at = 0; at < payload.Length;)
            {
                int field = Tag(payload, ref at, out int type);
                if (field < 1 || field > 5 || type != (field == 1 ? 0 : 2) || field <= 3 && (seen & 1 << field) != 0)
                    throw new InvalidDataException("Level view field");
                seen |= 1 << field;
                if (field == 1) { view.Revision = Varint(payload, ref at, payload.Length); continue; }
                int length = Length(payload, ref at, payload.Length);
                if (field == 2) view.Phase = Text(payload, at, length);
                else if (field == 3) view.Result = Text(payload, at, length);
                else if (field == 4)
                {
                    var stage = ReadStage(payload, at, at + length);
                    if (!names.Add(stage.Id)) throw new InvalidDataException("Level view stage repeated");
                    view.Stages.Add(stage);
                }
                else
                {
                    var variable = ReadVariable(payload, at, at + length);
                    if (!keys.Add(variable.Key)) throw new InvalidDataException("Level view variable repeated");
                    view.Variables.Add(variable);
                }
                at += length;
            }
            if (view.Phase.Length == 0) throw new InvalidDataException("Level view phase");
            return view;
        }
        static Stage ReadStage(byte[] data, int at, int end)
        {
            var stage = new Stage { Id = "" };
            int seen = 0;
            while (at < end)
            {
                int field = Tag(data, ref at, out int type, end);
                if (field < 1 || field > 2 || type != (field == 1 ? 2 : 0) || (seen & 1 << field) != 0)
                    throw new InvalidDataException("Level view stage field");
                seen |= 1 << field;
                if (field == 2) { stage.Generation = Varint(data, ref at, end); continue; }
                int length = Length(data, ref at, end);
                stage.Id = Text(data, at, length);
                at += length;
            }
            if (stage.Id.Length == 0) throw new InvalidDataException("Level view stage id");
            return stage;
        }
        static Variable ReadVariable(byte[] data, int at, int end)
        {
            var variable = new Variable { Key = "", Text = "" };
            int seen = 0;
            while (at < end)
            {
                int field = Tag(data, ref at, out int type, end);
                if (field < 1 || field > 4 || type != (field == 1 || field == 4 ? 2 : 0) || (seen & 1 << field) != 0)
                    throw new InvalidDataException("Level view variable field");
                seen |= 1 << field;
                if (type == 2)
                {
                    int length = Length(data, ref at, end);
                    string text = Text(data, at, length);
                    at += length;
                    if (field == 1) variable.Key = text;
                    else { variable.Kind = 4; variable.Text = text; }
                    continue;
                }
                ulong value = Varint(data, ref at, end);
                if (field == 3 && value > 1) throw new InvalidDataException("Level view flag");
                variable.Kind = field;
                if (field == 2) variable.Int = unchecked((long)value);
                else variable.Flag = value == 1;
            }
            // oneof：恰好一项。
            int values = (seen >> 2 & 1) + (seen >> 3 & 1) + (seen >> 4 & 1);
            if (variable.Key.Length == 0 || values != 1) throw new InvalidDataException("Level view variable value");
            return variable;
        }
        static int Tag(byte[] data, ref int at, out int type, int end = -1)
        {
            ulong tag = Varint(data, ref at, end < 0 ? data.Length : end);
            type = (int)(tag & 7);
            return tag >> 3 > 64 ? 0 : (int)(tag >> 3); // 字段号的范围由调用方检查；这里只防止转成 int 时溢出
        }
        static ulong Varint(byte[] data, ref int at, int end)
        {
            ulong value = 0;
            for (int i = 0; i < 10; i++)
            {
                if (at >= end) throw new InvalidDataException("Level view truncated");
                byte b = data[at++];
                if (i == 9 && b > 1) throw new InvalidDataException("Level view varint");
                value |= (ulong)(b & 127) << 7 * i;
                if ((b & 128) == 0) return value;
            }
            throw new InvalidDataException("Level view varint");
        }
        static int Length(byte[] data, ref int at, int end)
        {
            ulong length = Varint(data, ref at, end);
            if (length > (ulong)(end - at)) throw new InvalidDataException("Level view truncated");
            return (int)length;
        }
        static readonly UTF8Encoding strict = new UTF8Encoding(false, true);
        static string Text(byte[] data, int at, int length)
        {
            try { return strict.GetString(data, at, length); }
            catch (ArgumentException) { throw new InvalidDataException("Level view text"); }
        }
    }
}
