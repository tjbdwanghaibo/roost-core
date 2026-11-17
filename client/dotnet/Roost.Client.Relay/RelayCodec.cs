using System;
using System.Buffers.Binary;
using System.Collections.Generic;
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

    /// <summary>控制帧操作码（控制帧的 message_id）。1～15 由 relay 发出，16～31 由房主发出，32～ 任何一端；
    /// 换 host 与前台上报（docs/framework/RELAY.md §2.3）：64～79 由 relay 发出，80～95 由客户端发出。</summary>
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
        /// <summary>relay → 不再是（或没能成为）房主的连接。Slot=自己；载荷 [epoch 8B]。</summary>
        public const uint YouAreMember = 64;
        /// <summary>relay → 新房主候选。Slot=自己；载荷见 <see cref="RelayYouAreHost"/>。</summary>
        public const uint YouAreHost = 65;
        /// <summary>relay → 成员：它发给房主的游戏帧被丢了（宽限期或选人期间，每次换 host 至多一次）。载荷 [epoch 8B]。</summary>
        public const uint HostChanging = 66;
        /// <summary>relay → 在线的每个人：新房主已就绪。Slot=新房主；载荷 [epoch 8B][新房主账号 UTF-8]。</summary>
        public const uint HostChanged = 67;
        /// <summary>relay → 成员：房主在宽限期内回来了。Slot=房主；载荷 [epoch 8B]。</summary>
        public const uint HostResumed = 68;
        /// <summary>relay → 发快照或转交的连接：被拒（连接保留）。载荷见 <see cref="RelayRejected"/>。</summary>
        public const uint Rejected = 69;
        /// <summary>relay → 当前房主：成员上报的前台状态。Slot=成员槽位；载荷 1 字节：1 前台、0 后台。</summary>
        public const uint MemberPresence = 70;
        /// <summary>房主 → relay：主动转交给槽位 Slot。载荷是交接数据。</summary>
        public const uint Transfer = 80;
        /// <summary>任何人 → relay：离开房间，relay 回 Closed{left} 后断开。无载荷。</summary>
        public const uint Leave = 81;
        /// <summary>房主 → relay：候选顺序，槽位列表每个 4B，至多 <see cref="MaxSuccessors"/> 个。</summary>
        public const uint Successors = 82;
        /// <summary>房主 → relay：快照 [seq 8B][快照字节]。</summary>
        public const uint Snapshot = 83;
        /// <summary>新房主候选 → relay：已接手，载荷 [epoch 8B]（等于 YouAreHost 里的）。</summary>
        public const uint HostReady = 84;
        /// <summary>
        /// 任何成员 → relay：自己在前台（1）还是后台（0），1 字节；别的长度或取值被 relay 当协议错误断开。relay 转给当前房主
        /// （MemberPresence）；没有已就绪的房主、或发送者就是房主时丢弃。
        /// </summary>
        public const uint Presence = 85;
        /// <summary>Ping 载荷的上限；超出的 Ping 被 relay 当协议错误断开。</summary>
        public const int MaxPingBytes = 16;
        /// <summary>Successors 里槽位的个数上限。</summary>
        public const int MaxSuccessors = 256;
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
        /// <summary>relay 向 lobby 验权，lobby 不让进（不占席位、被踢、房间不在等）；自动重连遇到它就停。</summary>
        LobbyRejected = 15,
        /// <summary>relay 向 lobby 验权失败或超时；自动重连遇到它按退避再试。</summary>
        LobbyUnavailable = 16,
    }

    /// <summary><see cref="RelayYouAreHost.State"/> 从哪来。</summary>
    public enum RelayStateSource : byte
    {
        /// <summary>没有可交的状态：新房主用自己本地的状态，或由游戏决定重开。</summary>
        None = 0,
        /// <summary>主动转交时旧房主带来的交接数据。</summary>
        Handoff = 1,
        /// <summary>relay 存的最新快照（房主离开或断线）。</summary>
        Snapshot = 2,
    }

    /// <summary>YouAreHost 里的一个成员：房间存续期间进过房、没被踢的账号，含离线的、含旧房主，按槽位排列。</summary>
    public readonly struct RelaySeat : IEquatable<RelaySeat>
    {
        public uint Slot { get; }
        public string Account { get; }
        public bool Online { get; }
        public RelaySeat(uint slot, string account, bool online) { Slot = slot; Account = account; Online = online; }
        public bool Equals(RelaySeat other) => Slot == other.Slot && Account == other.Account && Online == other.Online;
        public override bool Equals(object? obj) => obj is RelaySeat other && Equals(other);
        public override int GetHashCode() => HashCode.Combine(Slot, Account, Online);
        public override string ToString() => $"slot {Slot} {Account} (online {Online})";
    }

    /// <summary>
    /// relay 选自己当新房主（YouAreHost）。游戏接手后调 <see cref="RelayClient.HostReady"/>(Epoch)；relay 的
    /// host_ready_timeout（缺省 10s）内不回就换下一个候选。
    /// </summary>
    public sealed class RelayYouAreHost
    {
        public ulong Epoch { get; }
        public RelayStateSource Source { get; }
        /// <summary>Source 为 Snapshot 时是那份快照的 seq；其余来源为 0。</summary>
        public ulong SnapshotSeq { get; }
        /// <summary>Source 为 Snapshot 时是 relay 收到那份快照到发出这一帧的时长；其余来源为 0。</summary>
        public TimeSpan SnapshotAge { get; }
        public IReadOnlyList<RelaySeat> Members { get; }
        /// <summary>
        /// 状态字节。<see cref="RelayCodec.DecodeYouAreHost"/> 给的是线上的原始字节；RelayClient 交给回调的是经
        /// RelayClientOptions.Compressor 解压后的（解压失败时为 null，原因在 StateError）。
        /// </summary>
        public byte[]? State { get; }
        public Exception? StateError { get; }

        public RelayYouAreHost(ulong epoch, RelayStateSource source, ulong snapshotSeq, TimeSpan snapshotAge, IReadOnlyList<RelaySeat> members,
            byte[]? state, Exception? stateError = null)
        {
            Epoch = epoch; Source = source; SnapshotSeq = snapshotSeq; SnapshotAge = snapshotAge; Members = members; State = state; StateError = stateError;
        }

        internal RelayYouAreHost WithState(byte[]? state, Exception? error) => new RelayYouAreHost(Epoch, Source, SnapshotSeq, SnapshotAge, Members, state, error);
    }

    /// <summary>HostChanged（新房主已就绪，Account 是新房主账号）与 HostResumed（宽限期内原房主回来了，Account 为空）。</summary>
    public readonly struct RelayHostEvent : IEquatable<RelayHostEvent>
    {
        public ulong Epoch { get; }
        public uint HostSlot { get; }
        public string Account { get; }
        public RelayHostEvent(ulong epoch, uint hostSlot, string account) { Epoch = epoch; HostSlot = hostSlot; Account = account; }
        public bool Equals(RelayHostEvent other) => Epoch == other.Epoch && HostSlot == other.HostSlot && Account == other.Account;
        public override bool Equals(object? obj) => obj is RelayHostEvent other && Equals(other);
        public override int GetHashCode() => HashCode.Combine(Epoch, HostSlot, Account);
        public override string ToString() => $"epoch {Epoch}, host slot {HostSlot} {Account}";
    }

    /// <summary>Rejected 的原因。</summary>
    public enum RelayRejectCode : byte
    {
        /// <summary>快照或交接数据超过 snapshot_max_bytes。</summary>
        TooBig = 1,
        /// <summary>离上一份快照不到 snapshot_min_interval。</summary>
        TooSoon = 2,
        /// <summary>快照 seq 不大于 relay 存着的那一份。</summary>
        StaleSeq = 3,
        /// <summary>转交的目标不在线，或是房主自己。</summary>
        BadTarget = 4,
        /// <summary>发送者此刻不是房主。</summary>
        NotHost = 5,
    }

    /// <summary>relay 拒收了自己发的快照或转交（连接保留）。Op 是被拒的操作码，Seq 是被拒快照的 seq（转交为 0）。</summary>
    public readonly struct RelayRejected : IEquatable<RelayRejected>
    {
        public uint Op { get; }
        public RelayRejectCode Code { get; }
        public ulong Seq { get; }
        public RelayRejected(uint op, RelayRejectCode code, ulong seq) { Op = op; Code = code; Seq = seq; }
        public bool Equals(RelayRejected other) => Op == other.Op && Code == other.Code && Seq == other.Seq;
        public override bool Equals(object? obj) => obj is RelayRejected other && Equals(other);
        public override int GetHashCode() => HashCode.Combine(Op, Code, Seq);
        public override string ToString() => $"op {Op} {Code} seq {Seq}";
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

    /// <summary>
    /// Welcome 的载荷：自己的槽位（帧的 seq）、是不是房主、房主的槽位（选人阶段为 0）、房间此刻的 host_epoch。
    /// RelayClient.Welcome 返回的值会随换 host 的控制帧更新（见 RelayRole）。
    /// </summary>
    public readonly struct RelayWelcome : IEquatable<RelayWelcome>
    {
        public uint Slot { get; }
        public bool IsHost { get; }
        public uint HostSlot { get; }
        public ulong Epoch { get; }
        public RelayWelcome(uint slot, bool isHost, uint hostSlot, ulong epoch = 0) { Slot = slot; IsHost = isHost; HostSlot = hostSlot; Epoch = epoch; }
        public bool Equals(RelayWelcome other) => Slot == other.Slot && IsHost == other.IsHost && HostSlot == other.HostSlot && Epoch == other.Epoch;
        public override bool Equals(object? obj) => obj is RelayWelcome other && Equals(other);
        public override int GetHashCode() => HashCode.Combine(Slot, IsHost, HostSlot, Epoch);
        public override string ToString() => $"slot {Slot}, host {IsHost}, host slot {HostSlot}, epoch {Epoch}";
    }

    /// <summary>房主收到的成员前台上报（MemberPresence）：Slot 是成员槽位，Foreground 为 false 表示后台。成员可以谎报，只作排候选顺序的参考。</summary>
    public readonly struct RelayMemberPresence : IEquatable<RelayMemberPresence>
    {
        public uint Slot { get; }
        public bool Foreground { get; }
        public RelayMemberPresence(uint slot, bool foreground) { Slot = slot; Foreground = foreground; }
        public bool Equals(RelayMemberPresence other) => Slot == other.Slot && Foreground == other.Foreground;
        public override bool Equals(object? obj) => obj is RelayMemberPresence other && Equals(other);
        public override int GetHashCode() => HashCode.Combine(Slot, Foreground);
        public override string ToString() => $"slot {Slot} {(Foreground ? "foreground" : "background")}";
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
        public static Task<RelayFrame> ReadAsync(Stream stream, int maxPayload, CancellationToken cancellation = default) =>
            ReadAsync(stream.ReadAsync, maxPayload, cancellation);

        /// <summary>
        /// 从连接上读一帧：按包头拼帧，不看字节是怎么到的（WebSocket 上一条消息里有几帧、一帧拆成几条消息都行）。
        /// 连接在帧中间结束时抛 EndOfStreamException。
        /// </summary>
        public static Task<RelayFrame> ReadAsync(IRelayTransport transport, int maxPayload, CancellationToken cancellation = default) =>
            ReadAsync(transport.ReceiveAsync, maxPayload, cancellation);

        private static async Task<RelayFrame> ReadAsync(Func<byte[], int, int, CancellationToken, Task<int>> read, int maxPayload, CancellationToken cancellation)
        {
            var header = new byte[HeaderSize];
            await ReadExactlyAsync(read, header, cancellation).ConfigureAwait(false);
            int size = ParseHeader(header, maxPayload);
            var payload = new byte[size];
            await ReadExactlyAsync(read, payload, cancellation).ConfigureAwait(false);
            return FromHeader(header, payload);
        }

        private static async Task ReadExactlyAsync(Func<byte[], int, int, CancellationToken, Task<int>> read, byte[] buffer, CancellationToken cancellation)
        {
            int offset = 0;
            while (offset < buffer.Length)
            {
                int got = await read(buffer, offset, buffer.Length - offset, cancellation).ConfigureAwait(false);
                if (got == 0) throw new EndOfStreamException();
                offset += got;
            }
        }

        /// <summary>
        /// Welcome 载荷：[角色 1 字节：1 房主、2 成员][房主槽位 4 字节大端][epoch 8 字节大端]，固定 13 字节。slot 是帧的 seq。
        /// 选人阶段进房的连接房主槽位为 0、epoch 是新的一轮。
        /// </summary>
        public static RelayWelcome DecodeWelcome(uint slot, ReadOnlySpan<byte> payload)
        {
            if (payload.Length != 13 || (payload[0] != 1 && payload[0] != 2)) throw new InvalidDataException("Invalid relay welcome.");
            return new RelayWelcome(slot, payload[0] == 1, BinaryPrimitives.ReadUInt32BigEndian(payload.Slice(1)), BinaryPrimitives.ReadUInt64BigEndian(payload.Slice(5)));
        }

        public static byte[] EncodeWelcome(RelayWelcome welcome)
        {
            var payload = new byte[13];
            payload[0] = welcome.IsHost ? (byte)1 : (byte)2;
            BinaryPrimitives.WriteUInt32BigEndian(payload.AsSpan(1), welcome.HostSlot);
            BinaryPrimitives.WriteUInt64BigEndian(payload.AsSpan(5), welcome.Epoch);
            return payload;
        }

        /// <summary>Presence 与 MemberPresence 的载荷：1 字节，1 前台、0 后台。</summary>
        public static byte[] EncodePresence(bool foreground) => new[] { foreground ? (byte)1 : (byte)0 };

        public static bool DecodePresence(ReadOnlySpan<byte> payload)
        {
            if (payload.Length != 1 || payload[0] > 1) throw new InvalidDataException("Invalid relay presence.");
            return payload[0] == 1;
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

        // ---------- 换 host（docs/framework/RELAY.md §2.3，与 Go 的 client/wire 一致） ----------

        /// <summary>只带 epoch 的载荷（YouAreMember、HostChanging、HostResumed、HostReady）：[epoch 8B]。</summary>
        public static byte[] EncodeEpoch(ulong epoch)
        {
            var payload = new byte[8];
            BinaryPrimitives.WriteUInt64BigEndian(payload, epoch);
            return payload;
        }

        public static ulong DecodeEpoch(ReadOnlySpan<byte> payload)
        {
            if (payload.Length != 8) throw new InvalidDataException("Invalid relay epoch.");
            return BinaryPrimitives.ReadUInt64BigEndian(payload);
        }

        /// <summary>HostChanged：[epoch 8B][新房主账号 UTF-8，至少 1 字节]。hostSlot 是帧的 seq。</summary>
        public static RelayHostEvent DecodeHostChanged(uint hostSlot, ReadOnlySpan<byte> payload)
        {
            if (payload.Length < 9) throw new InvalidDataException("Invalid relay host changed.");
            return new RelayHostEvent(BinaryPrimitives.ReadUInt64BigEndian(payload), hostSlot, Encoding.UTF8.GetString(payload.Slice(8)));
        }

        public static byte[] EncodeHostChanged(ulong epoch, string account)
        {
            var name = Encoding.UTF8.GetBytes(account);
            var payload = new byte[8 + name.Length];
            BinaryPrimitives.WriteUInt64BigEndian(payload, epoch);
            name.CopyTo(payload, 8);
            return payload;
        }

        /// <summary>
        /// YouAreHost：[epoch 8B][来源 1B][快照 seq 8B][快照年龄毫秒 4B][成员数 2B]，
        /// 成员数 × {[槽位 4B][在线 1B][账号长度 2B][账号]}，[状态字节：剩下的全部]（原始字节，不解压）。
        /// </summary>
        public static RelayYouAreHost DecodeYouAreHost(ReadOnlySpan<byte> payload)
        {
            if (payload.Length < 23 || payload[8] > (byte)RelayStateSource.Snapshot) throw new InvalidDataException("Invalid relay you_are_host.");
            ulong epoch = BinaryPrimitives.ReadUInt64BigEndian(payload);
            var source = (RelayStateSource)payload[8];
            ulong seq = BinaryPrimitives.ReadUInt64BigEndian(payload.Slice(9));
            uint ageMillis = BinaryPrimitives.ReadUInt32BigEndian(payload.Slice(17));
            int count = BinaryPrimitives.ReadUInt16BigEndian(payload.Slice(21));
            var rest = payload.Slice(23);
            var members = new List<RelaySeat>(count);
            for (int i = 0; i < count; i++)
            {
                if (rest.Length < 7 || rest[4] > 1) throw new InvalidDataException("Invalid relay you_are_host member.");
                int n = BinaryPrimitives.ReadUInt16BigEndian(rest.Slice(5));
                if (rest.Length < 7 + n) throw new InvalidDataException("Invalid relay you_are_host member.");
                members.Add(new RelaySeat(BinaryPrimitives.ReadUInt32BigEndian(rest), Encoding.UTF8.GetString(rest.Slice(7, n)), rest[4] == 1));
                rest = rest.Slice(7 + n);
            }
            return new RelayYouAreHost(epoch, source, seq, TimeSpan.FromMilliseconds(ageMillis), members, rest.ToArray());
        }

        public static byte[] EncodeYouAreHost(RelayYouAreHost host)
        {
            var output = new MemoryStream();
            var buffer = new byte[8];
            BinaryPrimitives.WriteUInt64BigEndian(buffer, host.Epoch); output.Write(buffer, 0, 8);
            output.WriteByte((byte)host.Source);
            BinaryPrimitives.WriteUInt64BigEndian(buffer, host.SnapshotSeq); output.Write(buffer, 0, 8);
            BinaryPrimitives.WriteUInt32BigEndian(buffer, (uint)host.SnapshotAge.TotalMilliseconds); output.Write(buffer, 0, 4);
            BinaryPrimitives.WriteUInt16BigEndian(buffer, (ushort)host.Members.Count); output.Write(buffer, 0, 2);
            foreach (var seat in host.Members)
            {
                var account = Encoding.UTF8.GetBytes(seat.Account);
                BinaryPrimitives.WriteUInt32BigEndian(buffer, seat.Slot); output.Write(buffer, 0, 4);
                output.WriteByte(seat.Online ? (byte)1 : (byte)0);
                BinaryPrimitives.WriteUInt16BigEndian(buffer, (ushort)account.Length); output.Write(buffer, 0, 2);
                output.Write(account, 0, account.Length);
            }
            if (host.State != null) output.Write(host.State, 0, host.State.Length);
            return output.ToArray();
        }

        /// <summary>Snapshot：[seq 8B][快照字节]。</summary>
        public static byte[] EncodeSnapshot(ulong seq, ReadOnlySpan<byte> data)
        {
            var payload = new byte[8 + data.Length];
            BinaryPrimitives.WriteUInt64BigEndian(payload, seq);
            data.CopyTo(payload.AsSpan(8));
            return payload;
        }

        /// <summary>Successors：槽位列表，每个 4 字节大端。</summary>
        public static byte[] EncodeSuccessors(IReadOnlyList<uint> slots)
        {
            if (slots.Count > RelayOps.MaxSuccessors) throw new ArgumentException($"At most {RelayOps.MaxSuccessors} successors.", nameof(slots));
            var payload = new byte[4 * slots.Count];
            for (int i = 0; i < slots.Count; i++) BinaryPrimitives.WriteUInt32BigEndian(payload.AsSpan(4 * i), slots[i]);
            return payload;
        }

        /// <summary>Rejected：[被拒的操作码 1B][原因 1B][seq 8B]。</summary>
        public static RelayRejected DecodeRejected(ReadOnlySpan<byte> payload)
        {
            if (payload.Length != 10) throw new InvalidDataException("Invalid relay rejected.");
            return new RelayRejected(payload[0], (RelayRejectCode)payload[1], BinaryPrimitives.ReadUInt64BigEndian(payload.Slice(2)));
        }

        public static byte[] EncodeRejected(RelayRejected rejected)
        {
            var payload = new byte[10];
            payload[0] = (byte)rejected.Op; payload[1] = (byte)rejected.Code;
            BinaryPrimitives.WriteUInt64BigEndian(payload.AsSpan(2), rejected.Seq);
            return payload;
        }
    }
}
