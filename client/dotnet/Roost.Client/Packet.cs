using System;
using System.Buffers.Binary;
using System.IO;
using System.Threading;
using System.Threading.Tasks;

namespace Roost.Client
{
    public enum PayloadKind : byte { Protobuf = 0, Sync = 1, Lockstep = 2 }

    // 包对象拥有载荷副本；主线程与网络线程不共享可变接收缓存。
    public sealed class Packet
    {
        public uint MessageId { get; }
        public uint Sequence { get; }
        public bool IsPush { get; }
        public PayloadKind Kind { get; }
        public ReadOnlyMemory<byte> Payload { get; }
        public Packet(uint messageId, uint sequence, ReadOnlyMemory<byte> payload,
            PayloadKind kind = PayloadKind.Protobuf, bool isPush = false)
        {
            MessageId = messageId; Sequence = sequence; Kind = kind; IsPush = isPush;
            Payload = payload.ToArray();
        }
    }

    public static class PacketCodec
    {
        public const int HeaderSize = 16;
        public const byte Version = 2;
        public const int DefaultMaxPayload = 1 << 20;

        public static byte[] Encode(Packet packet, int maxPayload = DefaultMaxPayload)
        {
            if (packet == null) throw new ArgumentNullException(nameof(packet));
            if (maxPayload <= 0) throw new ArgumentOutOfRangeException(nameof(maxPayload));
            if (packet.Kind != PayloadKind.Protobuf && packet.Kind != PayloadKind.Sync && packet.Kind != PayloadKind.Lockstep)
                throw new InvalidDataException("Unknown payload kind.");
            if (packet.MessageId == 0 && (packet.IsPush || packet.Kind != PayloadKind.Protobuf))
                throw new InvalidDataException("Authentication control frame has business flags.");
            if (packet.Payload.Length > maxPayload) throw new InvalidDataException("Payload exceeds limit.");
            var data = new byte[checked(HeaderSize + packet.Payload.Length)];
            data[0] = (byte)'R'; data[1] = (byte)'S'; data[2] = Version;
            data[3] = (byte)((packet.IsPush ? 1 : 0) | ((byte)packet.Kind << 1));
            BinaryPrimitives.WriteUInt32BigEndian(data.AsSpan(4, 4), packet.MessageId);
            BinaryPrimitives.WriteUInt32BigEndian(data.AsSpan(8, 4), packet.Sequence);
            BinaryPrimitives.WriteUInt32BigEndian(data.AsSpan(12, 4), (uint)packet.Payload.Length);
            packet.Payload.Span.CopyTo(data.AsSpan(HeaderSize));
            return data;
        }

        private static int CheckHeader(byte[] data, int maxPayload)
        {
            if (maxPayload <= 0) throw new ArgumentOutOfRangeException(nameof(maxPayload));
            if (data[0] != 'R' || data[1] != 'S' || data[2] != Version || (data[3] & ~7) != 0 || (data[3] & 6) == 6)
                throw new InvalidDataException("Invalid RS version/flags.");
            if (BinaryPrimitives.ReadUInt32BigEndian(data.AsSpan(4, 4)) == 0 && data[3] != 0)
                throw new InvalidDataException("Authentication control frame has business flags.");
            uint size = BinaryPrimitives.ReadUInt32BigEndian(data.AsSpan(12, 4));
            if (size > (uint)maxPayload) throw new InvalidDataException("Payload exceeds limit.");
            return (int)size;
        }

        public static Packet Decode(byte[] data, int maxPayload = DefaultMaxPayload)
        {
            if (data == null || data.Length < HeaderSize) throw new InvalidDataException("Truncated header.");
            int size = CheckHeader(data, maxPayload);
            if (data.Length - HeaderSize != size) throw new InvalidDataException("Incomplete or trailing packet bytes.");
            return new Packet(BinaryPrimitives.ReadUInt32BigEndian(data.AsSpan(4, 4)),
                BinaryPrimitives.ReadUInt32BigEndian(data.AsSpan(8, 4)), data.AsMemory(HeaderSize),
                (PayloadKind)((data[3] >> 1) & 3), (data[3] & 1) != 0);
        }

        internal static async Task<Packet> ReadAsync(Stream stream, int maxPayload)
        {
            var header = new byte[HeaderSize];
            await ReadExactlyAsync(stream, header).ConfigureAwait(false);
            int size = CheckHeader(header, maxPayload); // 校验在申请载荷内存前完成。
            var data = new byte[checked(HeaderSize + size)];
            Buffer.BlockCopy(header, 0, data, 0, HeaderSize);
            int offset = HeaderSize;
            while (offset < data.Length)
            {
                int read = await stream.ReadAsync(data, offset, data.Length - offset, CancellationToken.None).ConfigureAwait(false);
                if (read == 0) throw new EndOfStreamException();
                offset += read;
            }
            return Decode(data, maxPayload);
        }

        private static async Task ReadExactlyAsync(Stream stream, byte[] data)
        {
            int offset = 0;
            while (offset < data.Length)
            {
                int read = await stream.ReadAsync(data, offset, data.Length - offset, CancellationToken.None).ConfigureAwait(false);
                if (read == 0) throw new EndOfStreamException();
                offset += read;
            }
        }
    }
}
