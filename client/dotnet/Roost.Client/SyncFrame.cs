using System;
using System.Buffers.Binary;
using System.Collections.Generic;
using System.IO;
using System.Text;

namespace Roost.Client
{
    public sealed class SyncComponent
    {
        public byte Operation { get; internal set; }
        public ushort TypeId { get; internal set; }
        public ushort SchemaVersion { get; internal set; }
        public byte[] Data { get; internal set; } = Array.Empty<byte>();
    }
    public sealed class SyncObject
    {
        public byte Operation { get; internal set; }
        public ushort Id { get; internal set; }
        public ushort Generation { get; internal set; }
        public ushort Archetype { get; internal set; }
        public List<SyncComponent> Components { get; } = new List<SyncComponent>();
    }
    public sealed class SyncFrame
    {
        // 当前连接/同步上下文内的流标识，不是业务房间 ID。
        public ulong StreamId { get; private set; }
        public uint Epoch { get; private set; }
        public uint Tick { get; private set; }
        public uint BaseTick { get; private set; }
        public ushort SchemaVersion { get; private set; }
        public bool Full { get; private set; }
        public List<SyncObject> Objects { get; } = new List<SyncObject>();

        // 只解码外层frame；不凭解码成功推进基线或修改引擎对象。
        // 帧协议2（与Go framework/sync/frame一致）：magic "RF"(2) 协议(1) kind(1)，随后无符号varint：
        // StreamID、Epoch、Tick、[Delta: Tick-BaseTick]、SchemaVersion、对象数；对象：操作(1) varint ID、Generation、
        // [create: Archetype]、[非remove: 组件数]；组件：操作(1) varint TypeID、[set: SchemaVersion、长度、数据]。
        // varint须为最短编码、不越过字段范围，否则拒绝。
        public static SyncFrame Decode(ReadOnlySpan<byte> data, int maxFrameBytes = 4 << 20,
            int maxObjects = 100, int maxComponents = 64, int maxComponentBytes = 64 << 10)
        {
            if (maxObjects <= 0 || maxComponents <= 0 || maxComponentBytes <= 0 || data.Length < 4 || data.Length > maxFrameBytes)
                throw new InvalidDataException("Invalid Sync size/limits.");
            if (U16(data, 0) != 0x5246 || data[2] != 2 || (data[3] != 1 && data[3] != 2))
                throw new InvalidDataException("Unknown Sync frame header.");
            int offset = 4;
            var frame = new SyncFrame { Full = data[3] == 1 };
            frame.StreamId = Varint(data, ref offset, ulong.MaxValue);
            frame.Epoch = (uint)Varint(data, ref offset, uint.MaxValue);
            frame.Tick = (uint)Varint(data, ref offset, uint.MaxValue);
            if (!frame.Full) frame.BaseTick = frame.Tick - (uint)Varint(data, ref offset, frame.Tick);
            frame.SchemaVersion = (ushort)Varint(data, ref offset, ushort.MaxValue);
            if (frame.StreamId == 0 || frame.Epoch == 0 || frame.Tick == 0 || frame.SchemaVersion == 0 ||
                (frame.Full ? frame.BaseTick != 0 : frame.BaseTick == 0 || frame.BaseTick >= frame.Tick))
                throw new InvalidDataException("Invalid Sync baseline/metadata.");
            int count = (int)Varint(data, ref offset, ushort.MaxValue);
            if ((long)count > (long)maxObjects * 2 || count > (data.Length - offset) / 3)
                throw new InvalidDataException("Sync object limit.");
            var seen = new HashSet<uint>();
            for (int i = 0; i < count; i++)
            {
                Require(data, offset, 1);
                var obj = new SyncObject { Operation = data[offset++] };
                obj.Id = (ushort)Varint(data, ref offset, ushort.MaxValue);
                obj.Generation = (ushort)Varint(data, ref offset, ushort.MaxValue);
                if (obj.Operation == 1) obj.Archetype = (ushort)Varint(data, ref offset, ushort.MaxValue);
                int components = obj.Operation == 3 ? 0 : (int)Varint(data, ref offset, ushort.MaxValue);
                if (obj.Id == 0 || obj.Generation == 0 || obj.Operation < 1 || obj.Operation > 3 ||
                    (frame.Full && obj.Operation != 1) || (obj.Operation == 1 && obj.Archetype == 0) ||
                    !seen.Add(((uint)obj.Id << 16) | obj.Generation) ||
                    (long)components > (long)maxComponents * 2 || components > (data.Length - offset) / 2)
                    throw new InvalidDataException("Invalid Sync object.");
                var componentIds = new HashSet<ushort>();
                for (int c = 0; c < components; c++)
                {
                    Require(data, offset, 1);
                    var component = new SyncComponent { Operation = data[offset++] };
                    component.TypeId = (ushort)Varint(data, ref offset, ushort.MaxValue);
                    ulong size = 0;
                    if (component.Operation == 1)
                    {
                        component.SchemaVersion = (ushort)Varint(data, ref offset, ushort.MaxValue);
                        size = Varint(data, ref offset, uint.MaxValue);
                    }
                    if (size > (ulong)maxComponentBytes || size > (ulong)(data.Length - offset) || component.TypeId == 0 ||
                        !componentIds.Add(component.TypeId) || component.Operation < 1 || component.Operation > 2 ||
                        (obj.Operation == 1 && component.Operation != 1) ||
                        (component.Operation == 1 && component.SchemaVersion == 0))
                        throw new InvalidDataException("Invalid Sync component.");
                    component.Data = data.Slice(offset, (int)size).ToArray(); offset += (int)size;
                    obj.Components.Add(component);
                }
                frame.Objects.Add(obj);
            }
            if (offset != data.Length) throw new InvalidDataException("Trailing Sync bytes.");
            return frame;
        }
        internal static ushort U16(ReadOnlySpan<byte> d, int p) => BinaryPrimitives.ReadUInt16BigEndian(d.Slice(p, 2));
        internal static void Require(ReadOnlySpan<byte> d, int p, int size)
        { if (p < 0 || size < 0 || p > d.Length - size) throw new InvalidDataException("Truncated Sync data."); }
        internal static int VarintLength(ulong value) { int n = 1; while (value >= 0x80) { value >>= 7; n++; } return n; }
        // 读一个最短编码、不大于limit的无符号varint。
        internal static ulong Varint(ReadOnlySpan<byte> d, ref int p, ulong limit)
        {
            ulong value = 0; int shift = 0, start = p;
            while (true)
            {
                if (p >= d.Length || shift > 63) throw new InvalidDataException("Truncated or overlong Sync varint.");
                byte b = d[p++];
                if (shift == 63 && b > 1) throw new InvalidDataException("Overlong Sync varint.");
                value |= (ulong)(b & 0x7f) << shift;
                if (b < 0x80) break;
                shift += 7;
            }
            if (value > limit || p - start != VarintLength(value)) throw new InvalidDataException("Malformed Sync varint.");
            return value;
        }
    }

    public sealed class SubjectUpdate
    {
        public long SubjectId { get; private set; }
        public uint SubjectKind { get; private set; }
        public ulong Version { get; private set; }
        public ulong BaseVersion { get; private set; }
        public ulong Mask { get; private set; }
        public uint Reason { get; private set; }
        public bool Full { get; private set; }
        public ushort Encoding { get; private set; }
        public byte Lod { get; private set; }
        public uint SchemaVersion { get; private set; }
        public string Namespace { get; private set; } = "";
        public string Profile { get; private set; } = "";
        public byte[] Payload { get; private set; } = Array.Empty<byte>();

        // subject信封v3（与Go entitysync.EncodeSubjectUpdate一致）：flags(1)（bit0全量、bit1带Mask、bit2带Reason），
        // 全量：varint SubjectId、Kind、Version、BaseVersion、[Mask]、[Reason]、Encoding，Lod(1)，varint SchemaVersion，
        //       varint长度+Namespace，varint长度+Profile（"default"写成空串），载荷；
        // 增量：varint BaseVersion、Version-BaseVersion(>=1)、[Mask]、[Reason]，载荷。载荷是剩余字节。
        // 增量不带身份（SubjectId、Kind、Namespace、Profile、Lod、SchemaVersion、Encoding），从同一对象、同一组件上的
        // baseline继承，且BaseVersion必须等于baseline.Version；空载荷的增量不改视图。
        public static SubjectUpdate Decode(ReadOnlySpan<byte> data, SubjectUpdate? baseline = null, int maxBytes = 64 << 10)
        {
            if (data.Length < 1 || data.Length > maxBytes || (data[0] & ~7) != 0)
                throw new InvalidDataException("Invalid SubjectUpdate header.");
            byte flags = data[0];
            int offset = 1;
            var value = new SubjectUpdate { Full = (flags & 1) != 0 };
            if (value.Full)
            {
                value.SubjectId = unchecked((long)SyncFrame.Varint(data, ref offset, ulong.MaxValue));
                value.SubjectKind = (uint)SyncFrame.Varint(data, ref offset, uint.MaxValue);
                value.Version = SyncFrame.Varint(data, ref offset, ulong.MaxValue);
                value.BaseVersion = SyncFrame.Varint(data, ref offset, ulong.MaxValue);
                ReadOptional(data, ref offset, flags, value);
                value.Encoding = (ushort)SyncFrame.Varint(data, ref offset, ushort.MaxValue);
                SyncFrame.Require(data, offset, 1);
                value.Lod = data[offset++];
                value.SchemaVersion = (uint)SyncFrame.Varint(data, ref offset, uint.MaxValue);
                value.Namespace = Text(data, ref offset);
                value.Profile = Text(data, ref offset);
                if (value.Profile.Length == 0) value.Profile = "default"; // 与Go SyncProfile.Normalize一致。
                if (value.SubjectId == 0) throw new InvalidDataException("Subject id is zero.");
            }
            else
            {
                if (baseline == null) throw new InvalidDataException("Entity subject delta without a baseline.");
                value.BaseVersion = SyncFrame.Varint(data, ref offset, ulong.MaxValue);
                ulong step = SyncFrame.Varint(data, ref offset, ulong.MaxValue - value.BaseVersion);
                if (step == 0 || value.BaseVersion != baseline.Version) throw new InvalidDataException("Entity subject baseline gap.");
                value.Version = value.BaseVersion + step;
                ReadOptional(data, ref offset, flags, value);
                value.SubjectId = baseline.SubjectId; value.SubjectKind = baseline.SubjectKind; value.Namespace = baseline.Namespace;
                value.Profile = baseline.Profile; value.Lod = baseline.Lod; value.SchemaVersion = baseline.SchemaVersion;
                value.Encoding = baseline.Encoding;
            }
            value.Payload = data.Slice(offset).ToArray();
            return value;
        }
        private static void ReadOptional(ReadOnlySpan<byte> data, ref int offset, byte flags, SubjectUpdate value)
        {
            if ((flags & 2) != 0 && (value.Mask = SyncFrame.Varint(data, ref offset, ulong.MaxValue)) == 0)
                throw new InvalidDataException("SubjectUpdate mask flag with a zero mask.");
            if ((flags & 4) != 0 && (value.Reason = (uint)SyncFrame.Varint(data, ref offset, uint.MaxValue)) == 0)
                throw new InvalidDataException("SubjectUpdate reason flag with a zero reason.");
        }
        private static string Text(ReadOnlySpan<byte> data, ref int offset)
        {
            int length = (int)SyncFrame.Varint(data, ref offset, ushort.MaxValue);
            SyncFrame.Require(data, offset, length);
            var text = EncodingUTF8(data.Slice(offset, length)); offset += length;
            return text;
        }
        private static string EncodingUTF8(ReadOnlySpan<byte> data) => System.Text.Encoding.UTF8.GetString(data.ToArray());
    }
}
