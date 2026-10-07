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
        public ulong RoomId { get; private set; }
        public uint Epoch { get; private set; }
        public uint Tick { get; private set; }
        public uint BaseTick { get; private set; }
        public ushort SchemaVersion { get; private set; }
        public bool Full { get; private set; }
        public List<SyncObject> Objects { get; } = new List<SyncObject>();

        // 只解码外层frame；不凭解码成功推进基线或修改引擎对象。
        public static SyncFrame Decode(ReadOnlySpan<byte> data, int maxFrameBytes = 4 << 20,
            int maxObjects = 100, int maxComponents = 64, int maxComponentBytes = 64 << 10)
        {
            if (maxObjects <= 0 || maxComponents <= 0 || maxComponentBytes <= 0 || data.Length < 32 || data.Length > maxFrameBytes)
                throw new InvalidDataException("Invalid Sync size/limits.");
            if (U32(data, 0) != 0x43525031 || U16(data, 4) != 1 || data[7] != 0 || (data[6] != 1 && data[6] != 2))
                throw new InvalidDataException("Unknown Sync frame header.");
            var frame = new SyncFrame { Full = data[6] == 1, RoomId = U64(data, 8), Epoch = U32(data, 16),
                Tick = U32(data, 20), BaseTick = U32(data, 24), SchemaVersion = U16(data, 28) };
            if (frame.RoomId == 0 || frame.Epoch == 0 || frame.Tick == 0 || frame.SchemaVersion == 0 ||
                (frame.Full ? frame.BaseTick != 0 : frame.BaseTick == 0 || frame.BaseTick >= frame.Tick))
                throw new InvalidDataException("Invalid Sync baseline/metadata.");
            int count = U16(data, 30), offset = 32;
            if ((long)count > (long)maxObjects * 2 || count > (data.Length - offset) / 9)
                throw new InvalidDataException("Sync object limit.");
            var seen = new HashSet<uint>();
            for (int i = 0; i < count; i++)
            {
                Require(data, offset, 9);
                var obj = new SyncObject { Operation = data[offset], Id = U16(data, offset + 1),
                    Generation = U16(data, offset + 3), Archetype = U16(data, offset + 5) };
                int components = U16(data, offset + 7); offset += 9;
                if (obj.Id == 0 || obj.Generation == 0 || obj.Operation < 1 || obj.Operation > 3 ||
                    (frame.Full && obj.Operation != 1) || (obj.Operation == 1 && obj.Archetype == 0) ||
                    (obj.Operation == 3 && components != 0) || !seen.Add(((uint)obj.Id << 16) | obj.Generation) ||
                    (long)components > (long)maxComponents * 2 || components > (data.Length - offset) / 9)
                    throw new InvalidDataException("Invalid Sync object.");
                var componentIds = new HashSet<ushort>();
                for (int c = 0; c < components; c++)
                {
                    Require(data, offset, 9);
                    var component = new SyncComponent { Operation = data[offset], TypeId = U16(data, offset + 1), SchemaVersion = U16(data, offset + 3) };
                    uint size = U32(data, offset + 5); offset += 9;
                    if (size > (uint)maxComponentBytes || size > (uint)(data.Length - offset) || component.TypeId == 0 ||
                        !componentIds.Add(component.TypeId) || component.Operation < 1 || component.Operation > 2 ||
                        (obj.Operation == 1 && component.Operation != 1) ||
                        (component.Operation == 1 && component.SchemaVersion == 0) ||
                        (component.Operation == 2 && (component.SchemaVersion != 0 || size != 0)))
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
        internal static uint U32(ReadOnlySpan<byte> d, int p) => BinaryPrimitives.ReadUInt32BigEndian(d.Slice(p, 4));
        internal static ulong U64(ReadOnlySpan<byte> d, int p) => BinaryPrimitives.ReadUInt64BigEndian(d.Slice(p, 8));
        internal static void Require(ReadOnlySpan<byte> d, int p, int size)
        { if (p < 0 || size < 0 || p > d.Length - size) throw new InvalidDataException("Truncated Sync data."); }
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

        public static SubjectUpdate Decode(ReadOnlySpan<byte> data, int maxBytes = 64 << 10)
        {
            if (data.Length < 64 || data.Length > maxBytes || SyncFrame.U32(data, 0) != 0x52535355 ||
                SyncFrame.U16(data, 4) != 2 || (SyncFrame.U16(data, 6) & ~1) != 0 || data[51] != 0)
                throw new InvalidDataException("Invalid SubjectUpdate header.");
            int ns = SyncFrame.U16(data, 56), profile = SyncFrame.U16(data, 58);
            uint size = SyncFrame.U32(data, 60);
            if ((long)64 + ns + profile + size != data.Length) throw new InvalidDataException("Invalid SubjectUpdate length.");
            var value = new SubjectUpdate { SubjectId = unchecked((long)SyncFrame.U64(data, 8)), SubjectKind = SyncFrame.U32(data, 16),
                Version = SyncFrame.U64(data, 20), BaseVersion = SyncFrame.U64(data, 28), Mask = SyncFrame.U64(data, 36),
                Reason = SyncFrame.U32(data, 44), Full = (SyncFrame.U16(data, 6) & 1) != 0, Encoding = SyncFrame.U16(data, 48),
                Lod = data[50], SchemaVersion = SyncFrame.U32(data, 52),
                Namespace = EncodingUTF8(data.Slice(64, ns)), Profile = EncodingUTF8(data.Slice(64 + ns, profile)),
                Payload = data.Slice(64 + ns + profile, (int)size).ToArray() };
            if (value.Profile.Length == 0) value.Profile = "default"; // 与Go SyncProfile.Normalize一致。
            if (value.SubjectId == 0) throw new InvalidDataException("Subject id is zero.");
            return value;
        }
        private static string EncodingUTF8(ReadOnlySpan<byte> data) => System.Text.Encoding.UTF8.GetString(data.ToArray());
    }
}
