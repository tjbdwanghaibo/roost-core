using System;
using System.Buffers.Binary;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Threading;
using System.Threading.Tasks;

namespace Roost.Client.Relay
{
    /// <summary>RelayClient 的设置。</summary>
    public sealed class RelayClientOptions
    {
        /// <summary>房间的游戏，断线重连调 lobby 的 join 时用。必填。</summary>
        public string Game { get; set; } = "";
        /// <summary>房间密码，重连时带上：席位已被让出（预留过期）的人重新加入要它。</summary>
        public string? Password { get; set; }
        /// <summary>一次连接（拨号、发令牌、等 Welcome）的期限。</summary>
        public TimeSpan ConnectTimeout { get; set; } = TimeSpan.FromSeconds(10);
        /// <summary>心跳间隔（relay 的 idle_timeout 缺省 30s，取它的三分之一）。</summary>
        public TimeSpan PingInterval { get; set; } = TimeSpan.FromSeconds(10);
        /// <summary>这么久一帧都没收到就当断线；要大于 PingInterval（每个 Ping 都有 Pong）。</summary>
        public TimeSpan ReadTimeout { get; set; } = TimeSpan.FromSeconds(30);
        /// <summary>一次写 socket 的期限；写不出去就断开。</summary>
        public TimeSpan WriteTimeout { get; set; } = TimeSpan.FromSeconds(10);
        /// <summary>收帧的载荷上限；要不小于 relay 的 max_payload_bytes。实际的收帧上限还要装得下 YouAreHost（见 SnapshotMaxBytes），取两者的大值。</summary>
        public int MaxPayloadBytes { get; set; } = RelayCodec.DefaultMaxPayload;
        /// <summary>断线后自动重连的次数，0 不自动重连（缺省，与 Roost.Client 的 AutoReconnect 一致）。</summary>
        public int ReconnectAttempts { get; set; }
        public TimeSpan ReconnectMinBackoff { get; set; } = TimeSpan.FromMilliseconds(500);
        public TimeSpan ReconnectMaxBackoff { get; set; } = TimeSpan.FromSeconds(5);
        /// <summary>等 Pump 派发的通知上限；业务长期不 Pump、积压超过它时断开连接，不无界占内存。</summary>
        public int MaxPendingEvents { get; set; } = 65536;

        // ---------- 调度（WebGL A1，维护者 2026-11-18） ----------

        /// <summary>
        /// 由 Pump 驱动（Unity WebGL 必开；缺省 false，其他平台行为不变）。打开时 SDK 不用线程池与计时器：读循环在调用
        /// ConnectAsync / RejoinAsync 的线程上起，所有等待回到调用者的上下文（Unity 主线程）；心跳、读超时、连接超时、
        /// 重连退避都由 Pump 按时钟检查。代价：游戏不调 Pump（加载场景、切后台时 Unity 停了循环）就不发心跳、不判超时、
        /// 不重连——超过 relay 的 idle_timeout（缺省 30s）会被 relay 断开，恢复 Pump 后按 ReconnectAttempts 重连回来。
        /// 要求：只在一个线程（有 SynchronizationContext 的主线程）上调用本类的方法；传入的 LobbyClient 也要以 pumpDriven 构造；
        /// 传输的 ConnectAsync / ReceiveAsync 在同一线程上完成（例如 unity-webgl-smoke 的 BrowserRelayTransport：每帧先
        /// PollAll 再 Pump）。
        /// </summary>
        public bool PumpDriven { get; set; }

        // ---------- 传输（交接 C1） ----------

        /// <summary>
        /// 连 relay 的方式。断线重连向 lobby 要令牌时按它要地址（重连沿用同一种）；ConnectAsync 的票据也要是按它向 lobby 要的
        /// （CreateRoomRequest / JoinRoomRequest 的 Transport 填同一个值），否则连接时抛 ArgumentException。
        /// </summary>
        public RelayTransport Transport { get; set; } = RelayTransport.Tcp;
        /// <summary>
        /// 每次连接（含重连）新建一个连接对象。null 时按 Transport 用 SDK 自带的 TcpRelayTransport / WebSocketRelayTransport；
        /// Unity WebGL 上没有套接字，填一个用浏览器 WebSocket 实现的 IRelayTransport（Transport 设成 Ws），见 README。
        /// </summary>
        public Func<IRelayTransport>? TransportFactory { get; set; }

        // ---------- 换 host（方案 §4.6） ----------

        /// <summary>当房主（含候选）时的心跳间隔：要短于 relay.host_idle_timeout（缺省 2s）的三分之一，否则房主没有别的帧要发时会被当成断线。</summary>
        public TimeSpan HostPingInterval { get; set; } = TimeSpan.FromMilliseconds(600);
        /// <summary>true 时 OnYouAreHost 的回调（在 Pump 里）返回后 SDK 自动回 HostReady；缺省 false：游戏准备好后自己调 HostReady。</summary>
        public bool AutoHostReady { get; set; }
        /// <summary>当房主时定时推送的状态字节（压缩前，返回 null 时这一轮不推）；null 时不定时推，游戏可以随时调 Snapshot。在 Pump 里调用。</summary>
        public Func<byte[]?>? SnapshotState { get; set; }
        /// <summary>定时推送的间隔（方案 §4.6.3）。</summary>
        public TimeSpan SnapshotInterval { get; set; } = TimeSpan.FromSeconds(3);
        /// <summary>两份快照之间的最小间隔（relay.snapshot_min_interval 的缺省）；更密的 Snapshot 不发，返回 TooSoon。</summary>
        public TimeSpan SnapshotMinInterval { get; set; } = TimeSpan.FromSeconds(1);
        /// <summary>快照与交接数据压缩后的上限（relay.snapshot_max_bytes 的缺省，要与 relay 一致）；收帧上限至少是它加 16 KiB。</summary>
        public int SnapshotMaxBytes { get; set; } = 256 << 10;
        /// <summary>
        /// 压缩快照与交接数据（发出前压缩并在前面加 Id 这 1 字节标识，YouAreHost 交来时按标识解压）。内置的 None / Deflate 谁都解得开；
        /// 游戏自带的压缩方式（Id 2～255）房间里每个客户端都要配上，否则新房主拿到 RelayUnknownCompressionException。
        /// </summary>
        public IRelayCompressor Compressor { get; set; } = RelayCompressors.Deflate;

        internal RelayClientOptions Snapshot()
        {
            if (string.IsNullOrEmpty(Game)) throw new ArgumentException("RelayClientOptions.Game is required.");
            var copy = (RelayClientOptions)MemberwiseClone();
            if (copy.ReadTimeout <= copy.PingInterval) copy.ReadTimeout = TimeSpan.FromTicks(copy.PingInterval.Ticks * 3);
            return copy;
        }
    }

    /// <summary>
    /// 一条连接结束的通知。Reconnecting 为 true 表示正按 ReconnectAttempts 重连，之后会有 Reconnected，或者一条
    /// Reconnecting 为 false 的 Disconnected；Reconnecting 为 false 的是最后一条。Reason 是 relay 的 Closed 原因
    /// （没收到 Closed 时是 Disconnected）。
    /// </summary>
    public sealed class RelayDisconnect
    {
        public RelayReason Reason { get; }
        public Exception? Error { get; }
        public bool Reconnecting { get; }
        public RelayDisconnect(RelayReason reason, Exception? error, bool reconnecting) { Reason = reason; Error = error; Reconnecting = reconnecting; }
        public override string ToString() => $"{Reason} (reconnecting {Reconnecting}): {Error?.Message}";
    }

    /// <summary>
    /// SDK 记录的当前角色，只按 relay 的控制帧更新：Welcome 给出角色、房主槽位与 epoch；YouAreHost → 候选；HostChanged → 槽位是
    /// 自己就是房主、否则成员；YouAreMember → 成员（房主槽位 0，还在选人）；HostResumed → 房主槽位。HostChanging 不改角色。
    /// Epoch 是最近一次见到的 host_epoch：Welcome 带着进房时房间的 epoch，换过 host 之后才进房、或重连回来的客户端也拿到房间此刻的值。
    /// </summary>
    public readonly struct RelayRole : IEquatable<RelayRole>
    {
        public uint Slot { get; }
        /// <summary>自己是已就绪的房主。</summary>
        public bool IsHost { get; }
        /// <summary>收到了 YouAreHost、还没收到 HostChanged / YouAreMember：应当回 HostReady(Epoch)。</summary>
        public bool IsCandidate { get; }
        /// <summary>当前房主的槽位；正在选新房主时为 0。</summary>
        public uint HostSlot { get; }
        public ulong Epoch { get; }
        public RelayRole(uint slot, bool isHost, bool isCandidate, uint hostSlot, ulong epoch)
        {
            Slot = slot; IsHost = isHost; IsCandidate = isCandidate; HostSlot = hostSlot; Epoch = epoch;
        }
        public bool Equals(RelayRole other) => Slot == other.Slot && IsHost == other.IsHost && IsCandidate == other.IsCandidate && HostSlot == other.HostSlot && Epoch == other.Epoch;
        public override bool Equals(object? obj) => obj is RelayRole other && Equals(other);
        public override int GetHashCode() => HashCode.Combine(Slot, IsHost, IsCandidate, HostSlot, Epoch);
        public override string ToString() => $"slot {Slot}, host {IsHost}, candidate {IsCandidate}, host slot {HostSlot}, epoch {Epoch}";
    }

    /// <summary>Snapshot 的结果。</summary>
    public enum RelaySnapshotResult
    {
        Sent,
        /// <summary>离上一份不到 SnapshotMinInterval，没有发出。</summary>
        TooSoon,
        /// <summary>没有连接，或写失败。</summary>
        NotConnected,
        /// <summary>
        /// 这一份的状态已经过时，没有发出，也没占用 seq：压缩期间换了连接或房主任期（例如转出去又转回来），或者同一任期里比它晚发起的
        /// 一份已经先发出了（并发推送以发起顺序为准）。
        /// </summary>
        Stale,
    }

    /// <summary>LeaveAsync 的结果（三份 SDK 一致：Go LeaveResult.RoomGone、TS LeaveResult.roomGone）。</summary>
    public sealed class RelayLeaveResult
    {
        /// <summary>
        /// true：lobby 答 room_not_found——房间早就关了或过了寿命，人已经不在里面，LeaveAsync 照样算成功（不抛）。最常见的是
        /// 房间里最后一个人离开：relay 收到 Leave 帧随即关房并报给 lobby，lobby 的 leave 晚到一步。false：lobby 正常让出了席位，
        /// 或者没有 lobby / 房间号、只在 relay 上离开了。
        /// </summary>
        public bool RoomGone { get; }
        public RelayLeaveResult(bool roomGone) { RoomGone = roomGone; }
        public override string ToString() => $"room gone {RoomGone}";
    }

    /// <summary>relay 发来 Closed 之后断开：令牌被拒、房间不存在、满员、被踢、房间关闭等。</summary>
    public sealed class RelayClosedException : IOException
    {
        public RelayReason Reason { get; }
        public RelayClosedException(RelayReason reason) : base("relay: closed by relay: " + reason) { Reason = reason; }
    }

    /// <summary>一条已进房的 relay 连接。网络线程独占读；写由 writeLock 串行（IRelayTransport.Send 不并发）。</summary>
    internal sealed class RelaySession
    {
        public readonly IRelayTransport Transport;
        public readonly RelayWelcome Welcome;
        public readonly CancellationTokenSource Stop = new CancellationTokenSource();
        /// <summary>角色变了：叫醒心跳任务按新角色重算间隔。</summary>
        public readonly SemaphoreSlim Wake = new SemaphoreSlim(0);
        private readonly object writeLock = new object();
        private long lastReceive;
        /// <summary>最近一次发 Ping 的时刻（Clock.Elapsed.Ticks）；只由心跳（后台任务，或 PumpDriven 下 Pump 里的 Tick）读写。</summary>
        public long LastPing;

        public RelaySession(IRelayTransport transport, RelayWelcome welcome, long now, long pingClock)
        {
            Transport = transport; Welcome = welcome; lastReceive = now; LastPing = pingClock;
        }

        public void Touch(long now) => Interlocked.Exchange(ref lastReceive, now);
        public long LastReceive => Interlocked.Read(ref lastReceive);

        public bool Write(byte[] data)
        {
            lock (writeLock)
            {
                try { Transport.Send(data); return true; }
                catch (Exception) { Abort(); return false; }
            }
        }

        /// <summary>立即关连接（不发任何帧）；读循环随之结束。</summary>
        public void Abort()
        {
            try { Stop.Cancel(); } catch (ObjectDisposedException) { }
            Transport.Dispose();
        }
    }

    /// <summary>
    /// 一个玩家在一个房间里的 relay 连接（TCP 或 WebSocket，见 Transport / TransportFactory；含断线重连），方案 FEATURE-2026-11-16-lobby-relay §4.5，
    /// 契约见 docs/framework/RELAY.md。
    ///
    /// 线程：网络在后台线程收发；业务通知（帧、成员上下线、禁言、断线、重连）只由 <see cref="Pump"/> 在调用它的线程派发
    /// （Unity 每帧在主线程调用一次），与 Roost.Client 的 GameClient 相同。Send 一类方法可以从任意线程调用。
    /// RelayClientOptions.PumpDriven 打开时（Unity WebGL）不用后台线程：读循环的续行回到主线程，心跳、超时与重连退避在 Pump 里检查。
    ///
    /// 心跳：每 PingInterval 发一个 Ping，relay 原样回 Pong（测 RTT）；ReadTimeout 内一帧都没收到就当断线。当房主（含候选）时
    /// 按 HostPingInterval（缺省 600ms）发：relay 的 host_idle_timeout 缺省 2s。
    ///
    /// 换 host：relay 发来的 YouAreHost / YouAreMember / HostChanging / HostChanged / HostResumed / Rejected 解成结构，经 Pump
    /// 交给 OnYouAreHost 等回调；SDK 在网络线程上按它们记当前角色（<see cref="Role"/>）。房主可以 Transfer、SetSuccessors、
    /// Snapshot；新房主回 HostReady。收到 HostChanging 时 SDK 不缓存、不重发，要不要重发由游戏决定。
    /// 断线后重连要重新向 lobby 要令牌：RejoinAsync 调 lobby 的 join（对已在房间里的人幂等）再连 relay；
    /// ReconnectAttempts 大于 0 时自动按退避重试。只有连接自己断掉或 relay 判空闲超时才重连；被踢、房间关闭、被顶号、
    /// 经 lobby 离开、relay 停机都不重连。
    /// </summary>
    public sealed class RelayClient : IDisposable
    {
        private static readonly Stopwatch Clock = Stopwatch.StartNew();

        /// <summary>
        /// relay 发来的控制帧的内置处理：操作码 → 处理（在网络线程上解码，业务通知交给 Pump）。返回非 None 时连接结束。
        /// 扩展点：换 host 的控制帧（64～127：you_are_host、host_changing、host_changed、host_resumed、transfer、successors、snapshot）
        /// 下一批在这里加一项、加对应的事件；不在表里的操作码只交给 <see cref="OnControl"/> 注册的回调，没有回调就丢弃。
        /// </summary>
        private static readonly Dictionary<uint, Func<RelayClient, RelayFrame, RelayReason>> BuiltinControl =
            new Dictionary<uint, Func<RelayClient, RelayFrame, RelayReason>>
            {
                [RelayOps.Welcome] = (client, frame) =>
                {
                    var welcome = RelayCodec.DecodeWelcome(frame.Slot, frame.Payload.Span);
                    lock (client.gate) client.SetWelcomeLocked(welcome);
                    client.RoleChanged();
                    return RelayReason.None;
                },
                [RelayOps.MemberJoined] = (client, frame) =>
                {
                    var member = RelayCodec.DecodeMember(frame.Slot, frame.Payload.Span);
                    client.Post(() => client.MemberJoined?.Invoke(member));
                    return RelayReason.None;
                },
                [RelayOps.MemberLeft] = (client, frame) =>
                {
                    var member = RelayCodec.DecodeMember(frame.Slot, frame.Payload.Span);
                    client.Post(() => client.MemberLeft?.Invoke(member));
                    return RelayReason.None;
                },
                [RelayOps.Closed] = (client, frame) =>
                {
                    if (frame.Payload.Length != 1 || frame.Payload.Span[0] == 0) throw new InvalidDataException("Invalid relay closed frame.");
                    return (RelayReason)frame.Payload.Span[0];
                },
                [RelayOps.Muted] = (client, frame) =>
                {
                    if (frame.Payload.Length != 1 || frame.Payload.Span[0] > 1) throw new InvalidDataException("Invalid relay muted frame.");
                    bool on = frame.Payload.Span[0] == 1;
                    client.Post(() => client.Muted?.Invoke(on));
                    return RelayReason.None;
                },
                [RelayOps.Pong] = (client, frame) =>
                {
                    if (frame.Payload.Length == 8) // 别的长度不是本 SDK 发的 Ping
                    {
                        long sent = BinaryPrimitives.ReadInt64BigEndian(frame.Payload.Span);
                        lock (client.gate) client.rtt = TimeSpan.FromTicks(Clock.Elapsed.Ticks - sent);
                    }
                    return RelayReason.None;
                },
                [RelayOps.YouAreHost] = (client, frame) =>
                {
                    var offer = RelayCodec.DecodeYouAreHost(frame.Payload.Span);
                    lock (client.gate)
                    {
                        client.role = new RelayWelcome(frame.Slot, false, 0, offer.Epoch);
                        client.candidate = true;
                        // 新房主的快照 seq 从 relay 存着的那份往上接（§4.6.3），也不低于这一轮 epoch 的下限（见 SeqFloor）。
                        client.snapshotSeq = Math.Max(client.snapshotSeq, Math.Max(offer.SnapshotSeq, SeqFloor(offer.Epoch)));
                    }
                    client.RoleChanged();
                    if (offer.Source != RelayStateSource.None)
                    {
                        try { offer = offer.WithState(RelayCompressors.Unpack(client.options.Compressor, offer.State, RelayCompressors.MaxStateBytes), null); }
                        catch (Exception error) { offer = offer.WithState(null, error); }
                    }
                    else offer = offer.WithState(null, null);
                    client.Post(() =>
                    {
                        Action<RelayYouAreHost>? handler;
                        lock (client.gate) handler = client.youAreHost;
                        handler?.Invoke(offer);
                        if (client.options.AutoHostReady) client.HostReady(offer.Epoch);
                    });
                    return RelayReason.None;
                },
                [RelayOps.YouAreMember] = (client, frame) =>
                {
                    ulong epoch = RelayCodec.DecodeEpoch(frame.Payload.Span);
                    lock (client.gate)
                    {
                        client.role = new RelayWelcome(client.role.Slot, false, 0, epoch); // 还在选人
                        client.candidate = false;
                    }
                    client.RoleChanged();
                    client.Post(() => { Action<ulong>? h; lock (client.gate) h = client.youAreMember; h?.Invoke(epoch); });
                    return RelayReason.None;
                },
                [RelayOps.HostChanging] = (client, frame) =>
                {
                    ulong epoch = RelayCodec.DecodeEpoch(frame.Payload.Span);
                    client.Post(() => { Action<ulong>? h; lock (client.gate) h = client.hostChanging; h?.Invoke(epoch); });
                    return RelayReason.None;
                },
                [RelayOps.HostChanged] = (client, frame) =>
                {
                    var changed = RelayCodec.DecodeHostChanged(frame.Slot, frame.Payload.Span);
                    lock (client.gate)
                    {
                        bool wasHost = client.role.IsHost, isHost = frame.Slot == client.role.Slot;
                        client.role = new RelayWelcome(client.role.Slot, isHost, frame.Slot, changed.Epoch);
                        client.candidate = false;
                        if (isHost && !wasHost) client.BecomeHostLocked();
                    }
                    client.RoleChanged();
                    client.ResendPresence(); // 新房主不知道之前的上报：设过状态的人各自再报一次
                    client.Post(() => { Action<RelayHostEvent>? h; lock (client.gate) h = client.hostChanged; h?.Invoke(changed); });
                    return RelayReason.None;
                },
                [RelayOps.HostResumed] = (client, frame) =>
                {
                    var resumed = new RelayHostEvent(RelayCodec.DecodeEpoch(frame.Payload.Span), frame.Slot, "");
                    lock (client.gate)
                    {
                        client.role = new RelayWelcome(client.role.Slot, client.role.IsHost, frame.Slot, resumed.Epoch);
                    }
                    client.Post(() => { Action<RelayHostEvent>? h; lock (client.gate) h = client.hostResumed; h?.Invoke(resumed); });
                    return RelayReason.None;
                },
                [RelayOps.MemberPresence] = (client, frame) =>
                {
                    var presence = new RelayMemberPresence(frame.Slot, RelayCodec.DecodePresence(frame.Payload.Span));
                    client.Post(() => { Action<RelayMemberPresence>? h; lock (client.gate) h = client.memberPresence; h?.Invoke(presence); });
                    return RelayReason.None;
                },
                [RelayOps.Rejected] = (client, frame) =>
                {
                    var rejected = RelayCodec.DecodeRejected(frame.Payload.Span);
                    client.Post(() => { Action<RelayRejected>? h; lock (client.gate) h = client.rejected; h?.Invoke(rejected); });
                    return RelayReason.None;
                },
            };

        /// <summary>
        /// 新房主快照 seq 的下限：relay 要求 seq 在房间内递增（跨房主也一样），可 YouAreHost 只在来源是快照时才带 relay 存着的
        /// 那份的 seq，主动转交时为 0，而 relay 可能还存着旧房主更大的 seq。epoch 每次换 host 加 1，所以让 seq 不低于
        /// epoch &lt;&lt; 32 就一定大于以前任何房主用本 SDK 发过的 seq（与 Go SDK 相同）。
        /// </summary>
        private static ulong SeqFloor(ulong epoch) => epoch << 32;

        private readonly object gate = new object();
        private readonly LobbyClient? lobby;
        private readonly RelayClientOptions options;
        private readonly CancellationTokenSource lifetime = new CancellationTokenSource();
        private readonly ConcurrentQueue<Action> events = new ConcurrentQueue<Action>();
        private readonly Dictionary<uint, Action<RelayFrame>> controlHandlers = new Dictionary<uint, Action<RelayFrame>>();
        /// <summary>收发帧的载荷上限：MaxPayloadBytes 与装得下 YouAreHost 的大小取大值。</summary>
        private readonly int frameLimit;
        private int pendingEvents;
        private bool disposed, running, pumping;
        private string? roomId;
        private RelaySession? current;
        private RelayWelcome role; // 当前角色与 epoch：Welcome 给出，换 host 的控制帧更新
        private TimeSpan rtt;

        // 换 host（由 gate 保护）。
        private Action<RelayYouAreHost>? youAreHost;
        private Action<ulong>? youAreMember, hostChanging;
        private Action<RelayHostEvent>? hostChanged, hostResumed;
        private Action<RelayRejected>? rejected;
        private Action<RelayMemberPresence>? memberPresence;
        private bool candidate;
        private int presence = -1;     // SetForeground 记下的前台状态：-1 没设过（不发），0 后台，1 前台；换 host、重连后重发
        private ulong snapshotSeq;     // 最近发出（或从 YouAreHost 接过来）的快照 seq；下一份是它加 1
        private long snapshotAt = -1;  // 最近一次发出快照的时刻（Clock tick）；-1 表示当上房主之后还没发过
        private ulong snapshotStarted; // 最近一份快照发起时取的号（CurrentHostTerm 里加 1，只增不减），见 HostTerm
        private ulong snapshotSent;    // 已分配 seq 的那些份里最大的号；号比它小的一份比已发出的旧，分配 seq 时丢弃
        private long autoAt = -1;      // 最近一次定时推送的尝试（State 返回 null 也算）
        private bool autoQueued;       // 定时推送已放进 Pump 队列、还没执行
        private Task completion = Task.CompletedTask;
        /// <summary>PumpDriven：所有等待回到调用者的上下文（ConfigureAwait(pumpDriven)），心跳与计时由 Pump 做。</summary>
        private readonly bool pumpDriven;
        /// <summary>PumpDriven 下由 Pump 按时钟触发的计时（连接超时、重连退避）；由 gate 保护。</summary>
        private readonly List<PumpTimer> timers = new List<PumpTimer>();

        /// <summary>游戏通道：房主收到成员的帧（Slot 是成员槽位），成员收到房主的帧（Slot 是房主槽位）。</summary>
        public event Action<RelayFrame>? GameFrame;
        /// <summary>房间通道（聊天、表情、语音），Slot 是发送者槽位。</summary>
        public event Action<RelayFrame>? RoomFrame;
        /// <summary>成员上线（只有房主收到）。</summary>
        public event Action<RelayMember>? MemberJoined;
        /// <summary>成员下线（只有房主收到）。</summary>
        public event Action<RelayMember>? MemberLeft;
        /// <summary>被房主禁言（true）/ 解除（false）：被禁言时房间通道的帧被 relay 丢弃，游戏通道照常。</summary>
        public event Action<bool>? Muted;
        public event Action<RelayDisconnect>? Disconnected;
        /// <summary>自动重连成功，参数是新连接的身份（成员重连后槽位可能变了）。</summary>
        public event Action<RelayWelcome>? Reconnected;
        /// <summary>业务回调抛出的异常（不影响连接与其他回调）。</summary>
        public event Action<Exception>? Error;

        /// <param name="lobby">断线后重新要令牌用；只用 ConnectAsync、不重连时可以为 null。</param>
        public RelayClient(LobbyClient? lobby, RelayClientOptions options)
        {
            this.lobby = lobby;
            this.options = (options ?? throw new ArgumentNullException(nameof(options))).Snapshot();
            frameLimit = Math.Max(this.options.MaxPayloadBytes, this.options.SnapshotMaxBytes + 8 + YouAreHostOverhead);
            pumpDriven = this.options.PumpDriven;
            // LobbyClient 的等待若不回到调用者的上下文，WebGL 上 lobby 请求（含重连时的 join）的续行排进不运行的线程池。
            if (pumpDriven && lobby != null && !lobby.PumpDriven)
                throw new ArgumentException("RelayClientOptions.PumpDriven needs a LobbyClient created with pumpDriven: true.", nameof(lobby));
        }

        /// <summary>YouAreHost 除状态字节以外的余量（固定头 + 成员名单），与 service/relay 相同。</summary>
        private const int YouAreHostOverhead = 16 << 10;

        /// <summary>当前连接的身份；没有连接时是最后一次连接的身份。</summary>
        public RelayWelcome Welcome { get { lock (gate) return role; } }
        /// <summary>自己是不是已就绪的房主（以 relay 的 Welcome 与换 host 的控制帧为准，见 Role）。</summary>
        public bool IsHost => Welcome.IsHost;
        /// <summary>SDK 记录的当前角色。</summary>
        public RelayRole Role { get { lock (gate) return new RelayRole(role.Slot, role.IsHost, candidate, role.HostSlot, role.Epoch); } }
        public bool IsConnected { get { lock (gate) return current != null; } }
        public string? RoomId { get { lock (gate) return roomId; } }
        /// <summary>最近一次心跳的往返时延；还没收到 Pong 时为 0。</summary>
        public TimeSpan Rtt { get { lock (gate) return rtt; } }
        /// <summary>当前这一轮连接（含自动重连）在网络侧彻底结束时完成；之后的通知仍在队列里等 Pump。</summary>
        public Task Completion { get { lock (gate) return completion; } }

        // 换 host 的回调（方案 §4.6），都经 Pump 调用；handler 为 null 时撤销。

        /// <summary>relay 选自己当新房主（you_are_host）。游戏准备好后调 HostReady(e.Epoch)（或打开 AutoHostReady）。</summary>
        public void OnYouAreHost(Action<RelayYouAreHost>? handler) { lock (gate) youAreHost = handler; }
        /// <summary>自己不是（或没能成为）房主：主动转交后的旧房主、没在时限内回 HostReady 的候选。参数是 epoch。</summary>
        public void OnYouAreMember(Action<ulong>? handler) { lock (gate) youAreMember = handler; }
        /// <summary>发给房主的游戏帧被丢了（宽限期或选人期间，每次换 host 至多一次）。relay 不缓存，SDK 不重发：要不要重发由游戏决定。</summary>
        public void OnHostChanging(Action<ulong>? handler) { lock (gate) hostChanging = handler; }
        /// <summary>新房主已就绪（房间里每个人都收到，含新房主自己）；之后游戏帧恢复转发。</summary>
        public void OnHostChanged(Action<RelayHostEvent>? handler) { lock (gate) hostChanged = handler; }
        /// <summary>房主在宽限期内回来了、仍是原来的房主（只有成员收到；Account 为空）。</summary>
        public void OnHostResumed(Action<RelayHostEvent>? handler) { lock (gate) hostResumed = handler; }
        /// <summary>relay 拒收了自己发的快照或转交（连接保留）。</summary>
        public void OnRejected(Action<RelayRejected>? handler) { lock (gate) rejected = handler; }
        /// <summary>
        /// 成员上报了前台 / 后台（MemberPresence，只有房主收到；换 host 后新房主收到各成员重发的状态）。要不要据此调 SetSuccessors
        /// 由游戏决定，C# SDK 不自动排候选顺序。
        /// </summary>
        public void OnMemberPresence(Action<RelayMemberPresence>? handler) { lock (gate) memberPresence = handler; }

        /// <summary>
        /// 告诉当前房主自己在前台（true）还是后台（false）：发 Presence，relay 转给房主（房主自己发的被丢弃）。Unity 在
        /// OnApplicationPause / OnApplicationFocus 里调。状态记在 SDK 里，之后每次收到 HostChanged、每次连上（含自动重连、RejoinAsync）
        /// 都自动重发；从没调过时 SDK 不发。没有连接时返回 false（状态仍记下，连上时发出）。
        /// </summary>
        public bool SetForeground(bool foreground)
        {
            lock (gate) presence = foreground ? 1 : 0;
            return SendControl(RelayOps.Presence, 0, RelayCodec.EncodePresence(foreground));
        }

        /// <summary>
        /// 注册某个控制帧操作码的回调，是控制帧的扩展口：relay 发来的这个操作码的每一帧都经 Pump 交给它
        /// （内置处理过的操作码先做内置处理）。handler 为 null 时撤销。
        /// </summary>
        public void OnControl(uint op, Action<RelayFrame>? handler)
        {
            lock (gate)
            {
                if (handler == null) controlHandlers.Remove(op);
                else controlHandlers[op] = handler;
            }
        }

        /// <summary>用 lobby 给的票据（建房或加入的答复）连上 relay：首帧发令牌，等到 Welcome。relay 拒绝时抛 RelayClosedException。</summary>
        public async Task<RelayWelcome> ConnectAsync(RoomTicket ticket, CancellationToken cancellation = default)
        {
            if (ticket == null) throw new ArgumentNullException(nameof(ticket));
            Reserve();
            RelaySession session;
            try { session = await DialAsync(ticket.RelayAddr, ticket.Token, cancellation).ConfigureAwait(pumpDriven); }
            catch { Release(); throw; }
            Start(ticket.Room.Id, session);
            return session.Welcome;
        }

        /// <summary>重新向 lobby 要令牌（join 幂等）再连 relay；关掉自动重连时由业务自己重连。只能在 ConnectAsync 过、当前没有连接时调用。</summary>
        public async Task<RelayWelcome> RejoinAsync(CancellationToken cancellation = default)
        {
            Reserve();
            RelaySession session;
            try { session = await RejoinOnceAsync(cancellation).ConfigureAwait(pumpDriven); }
            catch { Release(); throw; }
            Start(RoomId!, session);
            return session.Welcome;
        }

        /// <summary>
        /// 派发积压的通知，返回派发了几个。在创建线程（Unity 主线程）上每帧调用；回调里不要再调 Pump。
        /// PumpDriven 时先按时钟做一轮心跳与计时（发 Ping、判读超时、定时快照、连接超时、重连退避），不计入 budget。
        /// </summary>
        public int Pump(int budget = int.MaxValue)
        {
            lock (gate)
            {
                if (pumping) throw new InvalidOperationException("Recursive Pump is not supported.");
                pumping = true;
            }
            int count = 0;
            try
            {
                if (pumpDriven) TickOnPump();
                while (count < budget && events.TryDequeue(out var action))
                {
                    Interlocked.Decrement(ref pendingEvents);
                    lock (gate) if (disposed) return count;
                    count++;
                    try { action(); }
                    catch (Exception error)
                    {
                        try { Error?.Invoke(error); } catch (Exception) { /* Error 自己抛错被隔离 */ }
                    }
                }
                return count;
            }
            finally { lock (gate) pumping = false; }
        }

        /// <summary>游戏通道。房主：target 是成员槽位或 RelayCodec.All；成员：只到房主，target 不看。没有连接或写失败时返回 false。</summary>
        public bool SendGame(uint target, uint messageId, ReadOnlyMemory<byte> payload, byte gameFlags = 0) =>
            Send(new RelayFrame(RelayChannel.Game, target, messageId, payload, gameFlags));

        /// <summary>房间通道：target 是某个槽位（房主也可以）或 RelayCodec.All（除自己以外的全体）；超出房间通道限额或被禁言时被 relay 丢弃。</summary>
        public bool SendRoom(uint target, uint messageId, ReadOnlyMemory<byte> payload, byte gameFlags = 0) =>
            Send(new RelayFrame(RelayChannel.Room, target, messageId, payload, gameFlags));

        /// <summary>踢出槽位上的成员（只有房主能发，否则抛 InvalidOperationException）。被踢的账号在房间存续期间不能再进。</summary>
        public bool Kick(uint slot)
        {
            RequireHost();
            return SendControl(RelayOps.Kick, slot, ReadOnlyMemory<byte>.Empty);
        }

        /// <summary>禁言 / 解除禁言槽位的房间通道（只有房主能发）。</summary>
        public bool Mute(uint slot, bool on)
        {
            RequireHost();
            return SendControl(RelayOps.Mute, slot, new[] { on ? (byte)1 : (byte)0 });
        }

        /// <summary>告诉 relay 自己已接手（epoch 是 YouAreHost 里的）。relay 广播 HostChanged 之后自己才是房主；迟到的、epoch 不对的被 relay 忽略。</summary>
        public bool HostReady(ulong epoch) => SendControl(RelayOps.HostReady, 0, RelayCodec.EncodeEpoch(epoch));

        /// <summary>
        /// 把房主主动转交给槽位上的在线成员（只有房主能发，否则抛 InvalidOperationException）。handoff 经 Compressor 压缩（带标识）后不超过
        /// SnapshotMaxBytes（否则抛 ArgumentException），relay 原样放进新房主的 YouAreHost；目标不在线或是自己时 relay 回 Rejected{BadTarget}。
        /// </summary>
        public bool Transfer(uint slot, ReadOnlySpan<byte> handoff)
        {
            RequireHost();
            return SendControl(RelayOps.Transfer, slot, CompressState(handoff));
        }

        /// <summary>声明候选顺序（只有房主能发）：房主离开或断线时 relay 按这个顺序取第一个在线的成员。relay 只记最新一份，换 host 后清空。</summary>
        public bool SetSuccessors(IReadOnlyList<uint> slots)
        {
            var payload = RelayCodec.EncodeSuccessors(slots ?? throw new ArgumentNullException(nameof(slots)));
            RequireHost();
            return SendControl(RelayOps.Successors, 0, payload);
        }

        /// <summary>
        /// 推一份快照（只有房主能发）：state 经 Compressor 压缩，seq 由 SDK 递增。离上一份不到 SnapshotMinInterval 的不发（TooSoon）；
        /// 压缩期间换了连接或房主任期、或者比它晚发起的一份已经发出的不发（Stale，见 HostTerm）；压缩后超过 SnapshotMaxBytes 抛 ArgumentException。relay 仍可能拒收
        /// （例如它的最小间隔更大），经 OnRejected 通知。state 应是调用时刻的状态：SDK 只能把这一份绑定到调用时的任期。
        /// </summary>
        public RelaySnapshotResult Snapshot(ReadOnlySpan<byte> state)
        {
            var term = CurrentHostTerm();
            return term.Session == null ? RelaySnapshotResult.NotConnected : SnapshotIn(term, state);
        }

        /// <summary>
        /// 一份快照所属的连接与房主任期（epoch）。压缩（定时快照还有向游戏要状态）在锁外，可能很慢；Snapshot 可以从任意线程调，期间网络线程
        /// 可能已经把房主换了一圈（A→B→A，epoch 0→2）或断线重连。旧任期的状态拿新任期的 seq 发出去，relay 会把它当成新任期最新的一份存下，
        /// 下一任房主恢复的就是旧状态（v1.54.0 审查 RV-20261118-02）。所以准备之前记下任期，分配 seq 之前在同一把锁里核对：变了就丢弃，
        /// 不发、不占 seq（与 Go / TS SDK 相同）。核对通过后 seq 已按这一任期分配，之后再换任期的话，relay 按 seq 递增（新任期的 seq
        /// 不低于 epoch&lt;&lt;32，见 SeqFloor）拒收它。
        /// 同一任期里也可以并发推（游戏从几个线程调 Snapshot，或 Snapshot 与 Pump 里的定时快照重叠）：先发起的一份若压缩更慢，会拿到
        /// 更大的 seq，盖掉 relay 已存的、后发起的较新状态。所以每份在发起时取一个递增的号（Ticket），分配 seq 时同样在锁里核对：比它晚
        /// 发起的一份已经分配过 seq，就丢弃这一份（Stale），并发推送以发起顺序为准、后发起的胜出（维护者 2026-11-18 定，与 Go / TS SDK 相同）。
        /// </summary>
        private readonly struct HostTerm
        {
            public readonly RelaySession? Session;
            public readonly ulong Epoch;
            public readonly ulong Ticket;
            public HostTerm(RelaySession? session, ulong epoch, ulong ticket) { Session = session; Epoch = epoch; Ticket = ticket; }
        }

        /// <summary>此刻的连接与房主任期，并给这一份取发起号；不是已就绪的房主时抛 InvalidOperationException，没有连接时 Session 为 null。</summary>
        private HostTerm CurrentHostTerm()
        {
            lock (gate)
            {
                if (!role.IsHost) throw new InvalidOperationException("Only the room host may send snapshots.");
                return new HostTerm(current, role.Epoch, ++snapshotStarted);
            }
        }

        /// <summary>压缩 state，核对它仍属于 term，分配 seq 后在 term 的连接上发出。</summary>
        private RelaySnapshotResult SnapshotIn(HostTerm term, ReadOnlySpan<byte> state)
        {
            var data = CompressState(state);
            long now = Clock.Elapsed.Ticks;
            ulong seq;
            lock (gate)
            {
                if (!role.IsHost) throw new InvalidOperationException("Only the room host may send snapshots.");
                if (current != term.Session || role.Epoch != term.Epoch) return RelaySnapshotResult.Stale;
                if (term.Ticket < snapshotSent) return RelaySnapshotResult.Stale; // 晚发起的一份已经发出
                if (snapshotAt >= 0 && now - snapshotAt < options.SnapshotMinInterval.Ticks) return RelaySnapshotResult.TooSoon;
                seq = ++snapshotSeq;
                snapshotAt = now;
                snapshotSent = term.Ticket;
            }
            var frame = RelayCodec.Encode(new RelayFrame(RelayChannel.Control, 0, RelayOps.Snapshot, RelayCodec.EncodeSnapshot(seq, data)), frameLimit);
            return term.Session!.Write(frame) ? RelaySnapshotResult.Sent : RelaySnapshotResult.NotConnected;
        }

        /// <summary>打成线上的状态字节（[压缩方式标识][压缩后的字节]，空状态为空）；上限 SnapshotMaxBytes 算上标识。</summary>
        private byte[] CompressState(ReadOnlySpan<byte> state)
        {
            var data = RelayCompressors.Pack(options.Compressor, state);
            if (data.Length > options.SnapshotMaxBytes) throw new ArgumentException($"Compressed state is {data.Length} bytes, over SnapshotMaxBytes {options.SnapshotMaxBytes}.");
            return data;
        }

        /// <summary>重发 SetForeground 记下的状态；没设过就不发。发不出去（连接刚断）不报错：下次连上还会发。</summary>
        private void ResendPresence()
        {
            int state;
            lock (gate) state = presence;
            if (state >= 0) SendControl(RelayOps.Presence, 0, RelayCodec.EncodePresence(state == 1));
        }

        /// <summary>发一个控制帧：控制帧的扩展口（踢人、禁言、心跳、换 host 都有专门的方法）。</summary>
        public bool SendControl(uint op, uint slot, ReadOnlyMemory<byte> payload) =>
            Send(new RelayFrame(RelayChannel.Control, slot, op, payload));

        /// <summary>发一帧。没有连接时返回 false；写失败时断开连接（之后按 Disconnected 通知）并返回 false。</summary>
        public bool Send(RelayFrame frame)
        {
            var data = RelayCodec.Encode(frame, frameLimit);
            RelaySession? session;
            lock (gate) session = current;
            return session != null && session.Write(data);
        }

        /// <summary>
        /// 离开房间，然后 Dispose：先在 relay 连接上发 Leave（房主离开时 relay 不等宽限期，立即用存着的快照换 host；不经 lobby 也能离开），
        /// 再经 lobby 离开（让出席位）。没有连接时只经 lobby 离开。
        /// lobby 答 room_not_found 算离开成功，结果的 RoomGone 为 true：房间已关或过了寿命，人已经不在里面（维护者 2026-11-18 定：
        /// 让调用方分得出“房间早就不在”与“正常离开”）。最常见的是房间里最后一个人离开——relay 收到 Leave 随即关房并报给 lobby，
        /// lobby 的 leave 晚到一步就答 room_not_found。其余 lobby 错误照常抛出。
        /// </summary>
        public async Task<RelayLeaveResult> LeaveAsync(CancellationToken cancellation = default)
        {
            string? room = RoomId;
            SendControl(RelayOps.Leave, 0, ReadOnlyMemory<byte>.Empty); // relay 回 Closed{left} 后断开
            try
            {
                if (lobby != null && room != null) await lobby.LeaveRoomAsync(room, options.Game, cancellation).ConfigureAwait(pumpDriven);
                return new RelayLeaveResult(false);
            }
            catch (LobbyException error) when (error.Code == LobbyErrorCode.RoomNotFound) { return new RelayLeaveResult(true); }
            finally { Dispose(); }
        }

        /// <summary>断开连接、停止重连；之后不再通知业务。不经 lobby 离开：席位按 relay 的报告让出，要立即让出用 LeaveAsync。</summary>
        public void Dispose()
        {
            RelaySession? session;
            lock (gate)
            {
                if (disposed) return;
                disposed = true;
                session = current;
            }
            lifetime.Cancel(); // PumpDriven 下等着的重连退避、连接超时随之结束（取消回调同步执行）
            session?.Abort();
            while (events.TryDequeue(out _)) { }
            lock (gate) timers.Clear();
        }

        /// <summary>测试用：像断网一样关掉 socket，不发任何帧。</summary>
        internal void AbortTransportForTest()
        {
            RelaySession? session;
            lock (gate) session = current;
            session?.Abort();
        }

        private void RequireHost()
        {
            if (!IsHost) throw new InvalidOperationException("Only the room host may do this.");
        }

        /// <summary>按一条 Welcome 记角色与 epoch：不再是候选；当上房主（建房，或宽限期内回来）时重新算快照的最小间隔。</summary>
        private void SetWelcomeLocked(RelayWelcome welcome)
        {
            bool wasHost = role.IsHost;
            role = welcome;
            candidate = false;
            if (welcome.IsHost && !wasHost) BecomeHostLocked();
        }

        /// <summary>自己当上已就绪的房主：relay 对新房主重新计最小间隔，SDK 也重新算；定时推送立即开始。</summary>
        private void BecomeHostLocked() { snapshotAt = -1; autoAt = -1; }

        /// <summary>叫醒当前连接的心跳任务：角色变了，心跳间隔与定时快照要立即按新角色算。</summary>
        private void RoleChanged()
        {
            RelaySession? session;
            lock (gate) session = current;
            if (session != null && session.Wake.CurrentCount == 0) session.Wake.Release();
        }

        private void Reserve()
        {
            lock (gate)
            {
                if (disposed) throw new ObjectDisposedException(nameof(RelayClient));
                if (running) throw new InvalidOperationException("RelayClient is already connected.");
                running = true;
            }
        }

        private void Release() { lock (gate) running = false; }

        private void Start(string room, RelaySession session)
        {
            lock (gate)
            {
                if (disposed)
                {
                    running = false;
                    session.Abort();
                    throw new ObjectDisposedException(nameof(RelayClient));
                }
                roomId = room; current = session; SetWelcomeLocked(session.Welcome);
                if (!pumpDriven) completion = Task.Run(() => RunAsync(session));
            }
            // PumpDriven：读循环在调用线程上起（锁外：第一段可能同步跑到收帧、分发），之后的续行回到调用者的上下文。
            if (pumpDriven)
            {
                var run = RunAsync(session);
                lock (gate) completion = run;
            }
            ResendPresence(); // 连上之前（或上一轮连接里）设过的前台状态告诉房主
        }

        /// <summary>把业务通知放进 Pump 的队列；积压超过上限时断开当前连接。</summary>
        private void Post(Action action)
        {
            events.Enqueue(action);
            if (Interlocked.Increment(ref pendingEvents) > options.MaxPendingEvents)
            {
                RelaySession? session;
                lock (gate) session = current;
                session?.Abort();
            }
        }

        private void PostDisconnect(RelayDisconnect disconnect) => Post(() => Disconnected?.Invoke(disconnect));

        /// <summary>一轮连接：收帧直到连接结束，按策略自动重连。</summary>
        private async Task RunAsync(RelaySession session)
        {
            try
            {
                while (true)
                {
                    var (reason, error) = await ServeAsync(session).ConfigureAwait(pumpDriven);
                    bool closed;
                    lock (gate) { closed = disposed; current = null; }
                    if (closed) return;
                    bool retry = options.ReconnectAttempts > 0 && Retryable(reason);
                    PostDisconnect(new RelayDisconnect(reason, error, retry));
                    if (!retry) return;
                    RelaySession next;
                    try { next = await ReconnectAsync().ConfigureAwait(pumpDriven); }
                    catch (Exception failure)
                    {
                        if (!lifetime.IsCancellationRequested)
                            PostDisconnect(new RelayDisconnect(failure is RelayClosedException rejected ? rejected.Reason : RelayReason.Disconnected, failure, false));
                        return;
                    }
                    lock (gate)
                    {
                        if (disposed) { next.Abort(); return; }
                        current = next; SetWelcomeLocked(next.Welcome);
                    }
                    ResendPresence();
                    var welcome = next.Welcome;
                    Post(() => Reconnected?.Invoke(welcome));
                    session = next;
                }
            }
            finally { Release(); }
        }

        /// <summary>收帧、分发直到连接结束，返回结束原因：relay 的 Closed 原因，或 Disconnected / Protocol。</summary>
        private async Task<(RelayReason, Exception?)> ServeAsync(RelaySession session)
        {
            // PumpDriven 时不起心跳任务：Pump 每次调用对当前连接做一轮 Tick。
            var heartbeat = pumpDriven ? Task.CompletedTask : Task.Run(() => HeartbeatAsync(session));
            try
            {
                while (true)
                {
                    var frame = await RelayCodec.ReadAsync(session.Transport, frameLimit, session.Stop.Token, pumpDriven).ConfigureAwait(pumpDriven);
                    session.Touch(Clock.ElapsedTicks);
                    var reason = Dispatch(frame);
                    if (reason != RelayReason.None) return (reason, new RelayClosedException(reason));
                }
            }
            catch (InvalidDataException error) { return (RelayReason.Protocol, error); }
            catch (Exception error) { return (RelayReason.Disconnected, error); }
            finally
            {
                session.Abort();
                try { await heartbeat.ConfigureAwait(pumpDriven); } catch (Exception) { }
            }
        }

        private RelayReason Dispatch(RelayFrame frame)
        {
            switch (frame.Channel)
            {
                case RelayChannel.Game:
                    Post(() => GameFrame?.Invoke(frame));
                    return RelayReason.None;
                case RelayChannel.Room:
                    Post(() => RoomFrame?.Invoke(frame));
                    return RelayReason.None;
                case RelayChannel.Control:
                    var reason = BuiltinControl.TryGetValue(frame.MessageId, out var builtin) ? builtin(this, frame) : RelayReason.None;
                    Action<RelayFrame>? user;
                    lock (gate) controlHandlers.TryGetValue(frame.MessageId, out user);
                    if (user != null && reason == RelayReason.None) Post(() => user(frame));
                    return reason;
                default:
                    throw new InvalidDataException("Relay frame without a channel.");
            }
        }

        /// <summary>
        /// 后台心跳任务（PumpDriven 为 false 时）：反复做一轮 <see cref="Tick"/>，两轮之间睡到下一件事该做的时刻；
        /// 角色变了（RoleChanged）立即醒来按新角色重算，不等旧的间隔走完。
        /// </summary>
        private async Task HeartbeatAsync(RelaySession session)
        {
            while (!session.Stop.IsCancellationRequested)
            {
                long wait = Tick(session, Clock.Elapsed.Ticks);
                if (wait < 0) return;
                try { await session.Wake.WaitAsync(TimeSpan.FromTicks(Math.Max(1, wait)), session.Stop.Token).ConfigureAwait(false); }
                catch (OperationCanceledException) { return; }
            }
        }

        /// <summary>
        /// 一轮心跳判断（后台心跳任务与 PumpDriven 下的 Pump 共用）：ReadTimeout 内没收到帧就断开；按当前角色的间隔发 Ping
        /// （成员 PingInterval，房主与候选 HostPingInterval；载荷是发出时刻的单调时钟 tick，8 字节大端，Pong 原样带回）；
        /// 当房主且设了 SnapshotState 时把定时快照放进 Pump 队列。返回离下一件事该做还有多久（Clock tick），连接该结束时返回 -1。
        /// </summary>
        private long Tick(RelaySession session, long now)
        {
            if (TimeSpan.FromSeconds((double)(Clock.ElapsedTicks - session.LastReceive) / Stopwatch.Frequency) > options.ReadTimeout)
            {
                session.Abort();
                return -1;
            }
            long readCheck = Math.Max(1, options.ReadTimeout.Ticks / 4);
            long interval;
            long snapshotDue = long.MaxValue;
            lock (gate)
            {
                interval = role.IsHost || candidate ? options.HostPingInterval.Ticks : options.PingInterval.Ticks;
                if (role.IsHost && options.SnapshotState != null && !autoQueued)
                    snapshotDue = Math.Max(snapshotAt, autoAt) < 0 ? now : Math.Max(snapshotAt, autoAt) + options.SnapshotInterval.Ticks;
                if (snapshotDue <= now)
                {
                    autoQueued = true;
                    autoAt = now;
                }
            }
            if (snapshotDue <= now) { Post(AutoSnapshot); snapshotDue = long.MaxValue; }
            if (now - session.LastPing >= interval)
            {
                session.LastPing = now;
                var payload = new byte[8];
                BinaryPrimitives.WriteInt64BigEndian(payload, now);
                if (!session.Write(RelayCodec.Encode(new RelayFrame(RelayChannel.Control, 0, RelayOps.Ping, payload), frameLimit))) return -1;
            }
            return Math.Min(Math.Min(session.LastPing + interval, snapshotDue) - now, readCheck);
        }

        /// <summary>PumpDriven：Pump 开头调用。先触发到期的计时（连接超时、重连退避），再对当前连接做一轮心跳。</summary>
        private void TickOnPump()
        {
            long now = Clock.Elapsed.Ticks;
            List<PumpTimer>? due = null;
            lock (gate)
            {
                for (int i = timers.Count - 1; i >= 0; i--)
                {
                    if (timers[i].Due > now) continue;
                    (due ??= new List<PumpTimer>()).Add(timers[i]);
                    timers.RemoveAt(i);
                }
            }
            // 到期的计时投到调用者的上下文、在 Pump 之外触发：触发的续行（重连退避之后的 rejoin、连接超时之后游戏自己 await
            // ConnectAsync 的那段代码）会同步跑下去，不能跑在 Pump 里面（Pump 不可重入）。没有上下文时就地触发（锁外）。
            if (due != null)
            {
                var context = SynchronizationContext.Current;
                foreach (var timer in due)
                {
                    if (context != null) context.Post(_ => timer.Fire(), null);
                    else timer.Fire();
                }
            }
            RelaySession? session;
            lock (gate) session = current;
            if (session != null && !session.Stop.IsCancellationRequested) Tick(session, now);
        }

        /// <summary>PumpDriven 下由 Pump 触发的一个计时。</summary>
        private sealed class PumpTimer
        {
            public readonly long Due;
            public readonly Action Fire;
            public PumpTimer(long due, Action fire) { Due = due; Fire = fire; }
        }

        /// <summary>登记一个由 Pump 在 delay 之后触发的计时（只在 PumpDriven 下用）。</summary>
        private PumpTimer After(TimeSpan delay, Action fire)
        {
            var timer = new PumpTimer(Clock.Elapsed.Ticks + delay.Ticks, fire);
            lock (gate) timers.Add(timer);
            return timer;
        }

        private void CancelTimer(PumpTimer timer) { lock (gate) timers.Remove(timer); }

        /// <summary>
        /// 等 delay：PumpDriven 时由 Pump 按时钟完成（不用计时器与线程池；不 Pump 就一直等），否则 Task.Delay。
        /// cancellation 取消时抛 OperationCanceledException（PumpDriven 下取消回调在调 Cancel 的线程上同步执行）。
        /// </summary>
        private Task DelayAsync(TimeSpan delay, CancellationToken cancellation)
        {
            if (!pumpDriven) return Task.Delay(delay, cancellation);
            var done = new TaskCompletionSource<bool>();
            if (cancellation.IsCancellationRequested) { done.TrySetCanceled(); return done.Task; }
            CancellationTokenRegistration registration = default;
            var timer = After(delay, () => { registration.Dispose(); done.TrySetResult(true); });
            registration = cancellation.Register(() => { CancelTimer(timer); done.TrySetCanceled(); });
            return done.Task;
        }

        /// <summary>
        /// 定时快照（在 Pump 里执行）：向游戏要状态，返回 null 时这一轮不推；太密、不再是房主或换了任期时跳过。任期在向游戏要状态之前记下
        /// （见 HostTerm）。
        /// </summary>
        private void AutoSnapshot()
        {
            lock (gate) autoQueued = false;
            HostTerm term;
            try { term = CurrentHostTerm(); }
            catch (InvalidOperationException) { return; } // 刚被换下
            if (term.Session == null) return;
            var state = options.SnapshotState?.Invoke();
            if (state == null) return;
            try { SnapshotIn(term, state); }
            catch (ArgumentException error) { try { Error?.Invoke(error); } catch (Exception) { } }
            catch (InvalidOperationException) { } // 刚被换下
        }

        private static bool Retryable(RelayReason reason) => reason == RelayReason.Disconnected || reason == RelayReason.IdleTimeout;

        private async Task<RelaySession> ReconnectAsync()
        {
            var backoff = options.ReconnectMinBackoff;
            Exception? last = null;
            for (int attempt = 0; attempt < options.ReconnectAttempts; attempt++)
            {
                await DelayAsync(backoff, lifetime.Token).ConfigureAwait(pumpDriven);
                try { return await RejoinOnceAsync(lifetime.Token).ConfigureAwait(pumpDriven); }
                catch (Exception error) when (!lifetime.IsCancellationRequested)
                {
                    last = error;
                    // lobby 的 5xx、网络错误再试；lobby 的 4xx（房间没了、被拒、被踢）与 relay 的终止性 Closed 不再试。令牌被拒换新令牌再试；
                    // relay 向 lobby 验权：lobby 不让进（LobbyRejected）停，lobby 暂时不可用（LobbyUnavailable）按退避再试。
                    bool retry = error is LobbyException lobbyError ? lobbyError.IsTemporary
                        : error is RelayClosedException closed ? Retryable(closed.Reason) || closed.Reason == RelayReason.TokenRejected || closed.Reason == RelayReason.LobbyUnavailable
                        : true;
                    if (!retry) throw;
                }
                backoff = TimeSpan.FromTicks(Math.Min(backoff.Ticks * 2, options.ReconnectMaxBackoff.Ticks));
            }
            throw new IOException($"relay: reconnect gave up after {options.ReconnectAttempts} attempts.", last);
        }

        private async Task<RelaySession> RejoinOnceAsync(CancellationToken cancellation)
        {
            string? room = RoomId;
            if (lobby == null || room == null) throw new InvalidOperationException("Rejoin needs a lobby and a room from an earlier ConnectAsync.");
            var ticket = await lobby.JoinRoomAsync(room, new JoinRoomRequest { Game = options.Game, Password = options.Password, Transport = options.Transport }, cancellation).ConfigureAwait(pumpDriven);
            return await DialAsync(ticket.RelayAddr, ticket.Token, cancellation).ConfigureAwait(pumpDriven);
        }

        /// <summary>拨号、发令牌首帧、等第一帧：Welcome 就进房成功，Closed 就是 relay 拒绝。relay 总是先发 Welcome。</summary>
        private async Task<RelaySession> DialAsync(string address, string token, CancellationToken cancellation)
        {
            var transport = NewTransport();
            using var timeout = CancellationTokenSource.CreateLinkedTokenSource(cancellation, lifetime.Token);
            // 连接超时：PumpDriven 时由 Pump 按时钟取消（WebGL 上 CancelAfter 的计时器不跑；不 Pump 就不超时），否则 CancelAfter。
            PumpTimer? deadline = null;
            if (pumpDriven) deadline = After(options.ConnectTimeout, () => { try { timeout.Cancel(); } catch (ObjectDisposedException) { } });
            else timeout.CancelAfter(options.ConnectTimeout);
            try
            {
                // 超时或取消时关掉连接：不依赖各实现对 CancellationToken 的处理（Mono 的 TcpClient.ConnectAsync 不收 token）。
                using (timeout.Token.Register(transport.Dispose))
                {
                    await transport.ConnectAsync(address, timeout.Token).ConfigureAwait(pumpDriven);
                    transport.Send(RelayCodec.EncodeToken(token));
                    var first = await RelayCodec.ReadAsync(transport, frameLimit, timeout.Token, pumpDriven).ConfigureAwait(pumpDriven);
                    if (first.Channel == RelayChannel.Control && first.MessageId == RelayOps.Closed && first.Payload.Length == 1)
                        throw new RelayClosedException((RelayReason)first.Payload.Span[0]);
                    if (first.Channel != RelayChannel.Control || first.MessageId != RelayOps.Welcome)
                        throw new InvalidDataException("First relay frame is not a welcome.");
                    var welcome = RelayCodec.DecodeWelcome(first.Slot, first.Payload.Span);
                    return new RelaySession(transport, welcome, Clock.ElapsedTicks, Clock.Elapsed.Ticks);
                }
            }
            catch (Exception error) when (!(error is RelayClosedException) && timeout.IsCancellationRequested)
            {
                transport.Dispose();
                cancellation.ThrowIfCancellationRequested();
                if (lifetime.IsCancellationRequested) throw new ObjectDisposedException(nameof(RelayClient));
                throw new TimeoutException($"relay: connecting to {address} timed out.", error);
            }
            catch
            {
                transport.Dispose();
                throw;
            }
            finally
            {
                if (deadline != null) CancelTimer(deadline);
            }
        }

        /// <summary>按选项新建一条连接：TransportFactory，否则按 Transport 用自带的 TCP / WebSocket。</summary>
        private IRelayTransport NewTransport()
        {
            if (options.TransportFactory != null)
                return options.TransportFactory() ?? throw new InvalidOperationException("RelayClientOptions.TransportFactory returned null.");
            return options.Transport == RelayTransport.Ws ? (IRelayTransport)new WebSocketRelayTransport(options.WriteTimeout) : new TcpRelayTransport(options.WriteTimeout);
        }
    }
}
