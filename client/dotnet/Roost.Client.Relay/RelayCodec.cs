using System;
using System.Buffers.Binary;
using System.IO;
using System.Text;
using System.Threading;
using System.Threading.Tasks;

namespace Roost.Client.Relay
{
    /// <summary>relay 帧的通道：RS v2 包头 flags 的 bits3..4（docs/framework/RELAY.md §2，与 Go 的 client/wire 一致）。</summary>
    public enum RelayChannel : byte
    {
        /// <summary>不是 relay 帧：只有连接上的第一帧（令牌）这样写。</summary>
        None = 0,
        /// <summary>游戏通道：成员的帧只到房主，房主的帧按目标转出。</summary>
        Game = 1,
        /// <summary>房间通道：聊天、表情、语音，按目标直达，房主也是收件人。</summary>
        Room = 2,
        /// <summary>控制帧：relay 与客户端之间的信令，MessageId 是操作码。</summary>
        Control = 3,
    }

    /// <summary>控制帧操作码（控制帧的 message_id）。1～15 由 relay 发出，16～31 由房主发出，32～ 任何一端；64～127 留给换 host。</summary>
    public static class RelayOps
    {
        public const uint Welcome = 1;
        public const uint MemberJoined = 2;
        public const uint MemberLeft = 3;
        public const uint Closed = 4;
        public const uint Muted = 5;
        public const uint Kick = 16;
        public const uint Mute = 17;
        public const uint Ping = 32;
        public const uint Pong = 33;
        /// <summary>Ping 载荷的上限；超出的 Ping 被 relay 当协议错误断开。</summary>
        public const int MaxPingBytes = 16;
    }

    /// <summary>RelayOps.Closed 与 MemberLeft 的原因码（1 字节）。</summary>
    public enum RelayReason : byte
    {
        None = 0,
        Disconnected = 1,
        Kicked = 2,
        Replaced = 3,
        RoomClosed = 4,
        FrameTooBig = 5,
        RateLimited = 6,
        SlowConsumer = 7,
        IdleTimeout = 8,
        Protocol = 9,
        TokenRejected = 10,
        RoomNotFound = 11,
        RoomFull = 12,
        Shutdown = 13,
        Left = 14,
    }

    /// <summary>
    /// 一帧 relay 帧。Slot：收到的数据帧是发送者槽位，要发的数据帧是目标槽位（<see cref="RelayCodec.All"/> 为全体；
    /// 成员的游戏帧只到房主，目标不看）；控制帧是控制对象的槽位。MessageId：数据帧上由游戏自用，控制帧上是操作码。
    /// GameFlags 是数据帧上归游戏的 flags bits0..2（推送位、载荷类型），relay 原样带过。
    /// </summary>
    public sealed class RelayFrame
    {
        public RelayChannel Channel { get; }
        public uint Slot { get; }
        public uint MessageId { get; }
        public byte GameFlags { get; }
        public ReadOnlyMemory<byte> Payload { get; }

        public RelayFrame(RelayChannel channel, uint slot, uint messageId, ReadOnlyMemory<byte> payload, byte gameFlags = 0)
        {
            Channel = channel; Slot = slot; MessageId = messageId; GameFlags = gameFlags;
            Payload = payload.ToArray();
        }
    }

    /// <summary>Welcome 的载荷：自己的槽位（帧的 seq）、是不是房主、房主的槽位。</summary>
    public readonly struct RelayWelcome : IEquatable<RelayWelcome>
    {
        public uint Slot { get; }
        public bool IsHost { get; }
        public uint HostSlot { get; }
        public RelayWelcome(uint slot, bool isHost, uint hostSlot) { Slot = slot; IsHost = isHost; HostSlot = hostSlot; }
        public bool Equals(RelayWelcome other) => Slot == other.Slot && IsHost == other.IsHost && HostSlot == other.HostSlot;
        public override bool Equals(object? obj) => obj is RelayWelcome other && Equals(other);
        public override int GetHashCode() => HashCode.Combine(Slot, IsHost, HostSlot);
        public override string ToString() => $"slot {Slot}, host {IsHost}, host slot {HostSlot}";
    }

    /// <summary>成员上下线（只有房主收到）。上线时 Reason 是 None（新加入）或 Replaced（同一账号重连，槽位不变）。</summary>
    public sealed class RelayMember
    {
        public uint Slot { get; }
        public string Account { get; }
        public RelayReason Reason { get; }
        public RelayMember(uint slot, string account, RelayReason reason) { Slot = slot; Account = account; Reason = reason; }
        public override string ToString() => $"slot {Slot} {Account} ({Reason})";
    }

    /// <summary>
    /// relay 帧的编解码：与 Gate 同一个 16 字节包头（RS、版本 2、flags、message_id、seq、载荷长度，大端），flags 的 bits3..4 是通道。
    /// 字节与 client/spec/relay-packets.json 一致（Go 的 client/wire 生成，测试核对）。
    /// </summary>
    public static class RelayCodec
    {
        public const int HeaderSize = 16;
        public const byte Version = 2;
        /// <summary>目标槽位 0：发给全体（不含自己）。</summary>
        public const uint All = 0;
        public const byte FlagPush = 1;
        public const byte FlagSync = 2;
        public const byte FlagLockstep = 4;
        public const int DefaultMaxPayload = 1 << 20;
        private const byte GameBits = FlagPush | FlagSync | FlagLockstep;
        private const byte ChannelShift = 3;

        /// <summary>编一帧。数据帧带 GameFlags 的游戏位；控制帧只带通道位且操作码非 0；None 通道只用于令牌首帧（<see cref="EncodeToken"/>）。</summary>
        public static byte[] Encode(RelayFrame frame, int maxPayload = DefaultMaxPayload)
        {
            if (frame == null) throw new ArgumentNullException(nameof(frame));
            byte flags;
            switch (frame.Channel)
            {
                case RelayChannel.Game:
                case RelayChannel.Room:
                    flags = (byte)(((byte)frame.Channel << ChannelShift) | (frame.GameFlags & GameBits));
                    break;
                case RelayChannel.Control:
                    flags = (byte)((byte)RelayChannel.Control << ChannelShift);
                    break;
                default:
                    throw new ArgumentException("Frame channel must be game, room or control.", nameof(frame));
            }
            return EncodeRaw(flags, frame.MessageId, frame.Slot, frame.Payload.Span, maxPayload);
        }

        /// <summary>令牌首帧：message_id=0、flags=0，载荷是 lobby 签的 rt1 令牌原文。</summary>
        public static byte[] EncodeToken(string token)
        {
            if (string.IsNullOrEmpty(token)) throw new ArgumentException("Relay token is required.", nameof(token));
            var payload = Encoding.UTF8.GetBytes(token);
            return EncodeRaw(0, 0, 0, payload, Math.Max(payload.Length, 1));
        }

        private static byte[] EncodeRaw(byte flags, uint messageId, uint slot, ReadOnlySpan<byte> payload, int maxPayload)
        {
            Validate(flags, messageId, (uint)payload.Length, maxPayload);
            var data = new byte[checked(HeaderSize + payload.Length)];
            data[0] = (byte)'R'; data[1] = (byte)'S'; data[2] = Version; data[3] = flags;
            BinaryPrimitives.WriteUInt32BigEndian(data.AsSpan(4, 4), messageId);
            BinaryPrimitives.WriteUInt32BigEndian(data.AsSpan(8, 4), slot);
            BinaryPrimitives.WriteUInt32BigEndian(data.AsSpan(12, 4), (uint)payload.Length);
            payload.CopyTo(data.AsSpan(HeaderSize));
            return data;
        }

        /// <summary>与 Go 的 Header.ValidateRelay 相同：保留位 5..7 为 0、Sync 与 Lockstep 不同时置位、控制帧只有通道位且操作码非 0、令牌帧 flags 为 0。</summary>
        private static void Validate(byte flags, uint messageId, uint payloadSize, int maxPayload)
        {
            if (maxPayload <= 0) maxPayload = DefaultMaxPayload;
            if ((flags & 0xE0) != 0 || (flags & (FlagSync | FlagLockstep)) == (FlagSync | FlagLockstep))
                throw new InvalidDataException("Invalid relay frame flags.");
            var channel = (RelayChannel)((flags >> ChannelShift) & 3);
            if (channel == RelayChannel.None && messageId == 0 && flags != 0)
                throw new InvalidDataException("Token frame carries flags.");
            if (channel == RelayChannel.Control && (messageId == 0 || flags != (byte)((byte)RelayChannel.Control << ChannelShift)))
                throw new InvalidDataException("Invalid relay control frame.");
            if (payloadSize > (uint)maxPayload) throw new InvalidDataException("Relay frame payload exceeds the limit.");
        }

        /// <summary>解恰好一帧（data 是整帧）。</summary>
        public static RelayFrame Decode(byte[] data, int maxPayload = DefaultMaxPayload)
        {
            if (data == null || data.Length < HeaderSize) throw new InvalidDataException("Truncated relay header.");
            int size = ParseHeader(data, maxPayload);
            if (data.Length - HeaderSize != size) throw new InvalidDataException("Incomplete or trailing relay frame bytes.");
            return FromHeader(data, data.AsMemory(HeaderSize));
        }

        /// <summary>检查包头，返回载荷长度（在申请载荷内存之前）。</summary>
        public static int ParseHeader(byte[] header, int maxPayload)
        {
            if (header[0] != 'R' || header[1] != 'S' || header[2] != Version)
                throw new InvalidDataException("Invalid RS magic or version.");
            uint size = BinaryPrimitives.ReadUInt32BigEndian(header.AsSpan(12, 4));
            Validate(header[3], BinaryPrimitives.ReadUInt32BigEndian(header.AsSpan(4, 4)), size, maxPayload);
            return (int)size;
        }

        private static RelayFrame FromHeader(byte[] header, ReadOnlyMemory<byte> payload)
        {
            byte flags = header[3];
            return new RelayFrame((RelayChannel)((flags >> ChannelShift) & 3), BinaryPrimitives.ReadUInt32BigEndian(header.AsSpan(8, 4)),
                BinaryPrimitives.ReadUInt32BigEndian(header.AsSpan(4, 4)), payload, (byte)(flags & GameBits));
        }

        /// <summary>从流里读一帧；流在帧中间结束时抛 EndOfStreamException。</summary>
        public static async Task<RelayFrame> ReadAsync(Stream stream, int maxPayload, CancellationToken cancellation = default)
        {
            var header = new byte[HeaderSize];
            await ReadExactlyAsync(stream, header, cancellation).ConfigureAwait(false);
            int size = ParseHeader(header, maxPayload);
            var payload = new byte[size];
            await ReadExactlyAsync(stream, payload, cancellation).ConfigureAwait(false);
            return FromHeader(header, payload);
        }

        private static async Task ReadExactlyAsync(Stream stream, byte[] buffer, CancellationToken cancellation)
        {
            int offset = 0;
            while (offset < buffer.Length)
            {
                int read = await stream.ReadAsync(buffer, offset, buffer.Length - offset, cancellation).ConfigureAwait(false);
                if (read == 0) throw new EndOfStreamException();
                offset += read;
            }
        }

        /// <summary>Welcome 载荷：[角色 1 字节：1 房主、2 成员][房主槽位 4 字节大端]。slot 是帧的 seq。</summary>
        public static RelayWelcome DecodeWelcome(uint slot, ReadOnlySpan<byte> payload)
        {
            if (payload.Length != 5 || (payload[0] != 1 && payload[0] != 2)) throw new InvalidDataException("Invalid relay welcome.");
            return new RelayWelcome(slot, payload[0] == 1, BinaryPrimitives.ReadUInt32BigEndian(payload.Slice(1)));
        }

        public static byte[] EncodeWelcome(RelayWelcome welcome)
        {
            var payload = new byte[5];
            payload[0] = welcome.IsHost ? (byte)1 : (byte)2;
            BinaryPrimitives.WriteUInt32BigEndian(payload.AsSpan(1), welcome.HostSlot);
            return payload;
        }

        /// <summary>MemberJoined / MemberLeft 载荷：[原因 1 字节][账号 UTF-8，至少 1 字节]。slot 是帧的 seq。</summary>
        public static RelayMember DecodeMember(uint slot, ReadOnlySpan<byte> payload)
        {
            if (payload.Length < 2) throw new InvalidDataException("Invalid relay member notice.");
            return new RelayMember(slot, Encoding.UTF8.GetString(payload.Slice(1)), (RelayReason)payload[0]);
        }

        public static byte[] EncodeMember(RelayMember member)
        {
            var account = Encoding.UTF8.GetBytes(member.Account);
            var payload = new byte[1 + account.Length];
            payload[0] = (byte)member.Reason;
            account.CopyTo(payload, 1);
            return payload;
        }
    }
}
