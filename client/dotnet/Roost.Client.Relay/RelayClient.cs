using System;
using System.Buffers.Binary;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Diagnostics;
using System.Globalization;
using System.IO;
using System.Net.Sockets;
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
        /// <summary>收帧的载荷上限；要不小于 relay 的 max_payload_bytes。</summary>
        public int MaxPayloadBytes { get; set; } = RelayCodec.DefaultMaxPayload;
        /// <summary>断线后自动重连的次数，0 不自动重连（缺省，与 Roost.Client 的 AutoReconnect 一致）。</summary>
        public int ReconnectAttempts { get; set; }
        public TimeSpan ReconnectMinBackoff { get; set; } = TimeSpan.FromMilliseconds(500);
        public TimeSpan ReconnectMaxBackoff { get; set; } = TimeSpan.FromSeconds(5);
        /// <summary>等 Pump 派发的通知上限；业务长期不 Pump、积压超过它时断开连接，不无界占内存。</summary>
        public int MaxPendingEvents { get; set; } = 65536;

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

    /// <summary>relay 发来 Closed 之后断开：令牌被拒、房间不存在、满员、被踢、房间关闭等。</summary>
    public sealed class RelayClosedException : IOException
    {
        public RelayReason Reason { get; }
        public RelayClosedException(RelayReason reason) : base("relay: closed by relay: " + reason) { Reason = reason; }
    }

    /// <summary>一条已进房的 relay 连接。网络线程独占读；写由 writeLock 串行。</summary>
    internal sealed class RelaySession
    {
        public readonly TcpClient Tcp;
        public readonly NetworkStream Stream;
        public readonly RelayWelcome Welcome;
        public readonly CancellationTokenSource Stop = new CancellationTokenSource();
        private readonly object writeLock = new object();
        private long lastReceive;

        public RelaySession(TcpClient tcp, NetworkStream stream, RelayWelcome welcome, long now)
        {
            Tcp = tcp; Stream = stream; Welcome = welcome; lastReceive = now;
        }

        public void Touch(long now) => Interlocked.Exchange(ref lastReceive, now);
        public long LastReceive => Interlocked.Read(ref lastReceive);

        public bool Write(byte[] data)
        {
            lock (writeLock)
            {
                try { Stream.Write(data, 0, data.Length); return true; }
                catch (Exception) { Abort(); return false; }
            }
        }

        /// <summary>立即关 socket（不发任何帧）；读循环随之结束。</summary>
        public void Abort()
        {
            try { Stop.Cancel(); } catch (ObjectDisposedException) { }
            Tcp.Dispose();
        }
    }

    /// <summary>
    /// 一个玩家在一个房间里的 relay TCP 连接（含断线重连），方案 FEATURE-2026-11-16-lobby-relay §4.5，契约见 docs/framework/RELAY.md。
    ///
    /// 线程：网络在后台线程收发；业务通知（帧、成员上下线、禁言、断线、重连）只由 <see cref="Pump"/> 在调用它的线程派发
    /// （Unity 每帧在主线程调用一次），与 Roost.Client 的 GameClient 相同。Send 一类方法可以从任意线程调用。
    ///
    /// 心跳：每 PingInterval 发一个 Ping，relay 原样回 Pong（测 RTT）；ReadTimeout 内一帧都没收到就当断线。
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
                    lock (client.gate) client.role = welcome;
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
            };

        private readonly object gate = new object();
        private readonly LobbyClient? lobby;
        private readonly RelayClientOptions options;
        private readonly CancellationTokenSource lifetime = new CancellationTokenSource();
        private readonly ConcurrentQueue<Action> events = new ConcurrentQueue<Action>();
        private readonly Dictionary<uint, Action<RelayFrame>> controlHandlers = new Dictionary<uint, Action<RelayFrame>>();
        private readonly Dictionary<HostEvent, Action<RelayFrame>> hostHandlers = new Dictionary<HostEvent, Action<RelayFrame>>();

        /// <summary>换 host 的通知种类（尚未接线，见 OnYouAreHost）。</summary>
        private enum HostEvent { YouAreHost, HostChanging, HostChanged, HostResumed }
        private int pendingEvents;
        private bool disposed, running, pumping;
        private string? roomId;
        private RelaySession? current;
        private RelayWelcome role;
        private TimeSpan rtt;
        private Task completion = Task.CompletedTask;

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
        }

        /// <summary>当前连接的身份；没有连接时是最后一次连接的身份。</summary>
        public RelayWelcome Welcome { get { lock (gate) return role; } }
        public bool IsHost => Welcome.IsHost;
        public bool IsConnected { get { lock (gate) return current != null; } }
        public string? RoomId { get { lock (gate) return roomId; } }
        /// <summary>最近一次心跳的往返时延；还没收到 Pong 时为 0。</summary>
        public TimeSpan Rtt { get { lock (gate) return rtt; } }
        /// <summary>当前这一轮连接（含自动重连）在网络侧彻底结束时完成；之后的通知仍在队列里等 Pump。</summary>
        public Task Completion { get { lock (gate) return completion; } }

        // 换 host 的回调位置（方案 §4.6）。操作码在 64～127，由 host 组定编码表并追加进 client/spec/relay-packets.json；
        // 这一批还没有接线：relay 发来这些操作码时按“不认识的操作码”丢弃（注册了 OnControl 的照样交给它）。接线时在
        // BuiltinControl 里加对应操作码、把载荷解成结构，经 Pump 调用这些回调（参数会从原始帧换成解好的结构）。

        /// <summary>自己成了新房主（you_are_host）。尚未接线。</summary>
        public void OnYouAreHost(Action<RelayFrame>? handler) => SetHostHandler(HostEvent.YouAreHost, handler);
        /// <summary>换 host 开始，暂停发给房主的游戏帧（host_changing）。尚未接线。</summary>
        public void OnHostChanging(Action<RelayFrame>? handler) => SetHostHandler(HostEvent.HostChanging, handler);
        /// <summary>换 host 完成（host_changed）。尚未接线。</summary>
        public void OnHostChanged(Action<RelayFrame>? handler) => SetHostHandler(HostEvent.HostChanged, handler);
        /// <summary>房主在宽限期内回来了（host_resumed）。尚未接线。</summary>
        public void OnHostResumed(Action<RelayFrame>? handler) => SetHostHandler(HostEvent.HostResumed, handler);

        private void SetHostHandler(HostEvent kind, Action<RelayFrame>? handler)
        {
            lock (gate)
            {
                if (handler == null) hostHandlers.Remove(kind);
                else hostHandlers[kind] = handler;
            }
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
            try { session = await DialAsync(ticket.RelayAddr, ticket.Token, cancellation).ConfigureAwait(false); }
            catch { Release(); throw; }
            Start(ticket.Room.Id, session);
            return session.Welcome;
        }

        /// <summary>重新向 lobby 要令牌（join 幂等）再连 relay；关掉自动重连时由业务自己重连。只能在 ConnectAsync 过、当前没有连接时调用。</summary>
        public async Task<RelayWelcome> RejoinAsync(CancellationToken cancellation = default)
        {
            Reserve();
            RelaySession session;
            try { session = await RejoinOnceAsync(cancellation).ConfigureAwait(false); }
            catch { Release(); throw; }
            Start(RoomId!, session);
            return session.Welcome;
        }

        /// <summary>派发积压的通知，返回派发了几个。在创建线程（Unity 主线程）上每帧调用；回调里不要再调 Pump。</summary>
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

        /// <summary>发一个控制帧：控制帧的扩展口，换 host 的控制帧（host_ready、transfer、successors、snapshot 等）下一批经它发出。</summary>
        public bool SendControl(uint op, uint slot, ReadOnlyMemory<byte> payload) =>
            Send(new RelayFrame(RelayChannel.Control, slot, op, payload));

        /// <summary>发一帧。没有连接时返回 false；写失败时断开连接（之后按 Disconnected 通知）并返回 false。</summary>
        public bool Send(RelayFrame frame)
        {
            var data = RelayCodec.Encode(frame, options.MaxPayloadBytes);
            RelaySession? session;
            lock (gate) session = current;
            return session != null && session.Write(data);
        }

        /// <summary>经 lobby 离开房间（让出席位；房主离开由 relay 走换 host），然后 Dispose。</summary>
        public async Task LeaveAsync(CancellationToken cancellation = default)
        {
            string? room = RoomId;
            try
            {
                if (lobby != null && room != null) await lobby.LeaveRoomAsync(room, options.Game, cancellation).ConfigureAwait(false);
            }
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
            lifetime.Cancel();
            session?.Abort();
            while (events.TryDequeue(out _)) { }
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
            if (!IsHost) throw new InvalidOperationException("Only the room host may kick or mute.");
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
                roomId = room; current = session; role = session.Welcome;
                completion = Task.Run(() => RunAsync(session));
            }
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
                    var (reason, error) = await ServeAsync(session).ConfigureAwait(false);
                    bool closed;
                    lock (gate) { closed = disposed; current = null; }
                    if (closed) return;
                    bool retry = options.ReconnectAttempts > 0 && Retryable(reason);
                    PostDisconnect(new RelayDisconnect(reason, error, retry));
                    if (!retry) return;
                    RelaySession next;
                    try { next = await ReconnectAsync().ConfigureAwait(false); }
                    catch (Exception failure)
                    {
                        if (!lifetime.IsCancellationRequested)
                            PostDisconnect(new RelayDisconnect(failure is RelayClosedException rejected ? rejected.Reason : RelayReason.Disconnected, failure, false));
                        return;
                    }
                    lock (gate)
                    {
                        if (disposed) { next.Abort(); return; }
                        current = next; role = next.Welcome;
                    }
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
            var heartbeat = Task.Run(() => HeartbeatAsync(session));
            try
            {
                while (true)
                {
                    var frame = await RelayCodec.ReadAsync(session.Stream, options.MaxPayloadBytes, session.Stop.Token).ConfigureAwait(false);
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
                try { await heartbeat.ConfigureAwait(false); } catch (Exception) { }
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

        /// <summary>每 PingInterval 发一个 Ping（载荷是发出时刻的单调时钟 tick，8 字节大端，Pong 原样带回）；ReadTimeout 内没收到帧就断开。</summary>
        private async Task HeartbeatAsync(RelaySession session)
        {
            var check = TimeSpan.FromTicks(Math.Max(1, Math.Min(options.PingInterval.Ticks, options.ReadTimeout.Ticks / 4)));
            long lastPing = Clock.Elapsed.Ticks;
            while (!session.Stop.IsCancellationRequested)
            {
                try { await Task.Delay(check, session.Stop.Token).ConfigureAwait(false); }
                catch (OperationCanceledException) { return; }
                long now = Clock.Elapsed.Ticks;
                if (TimeSpan.FromSeconds((double)(Clock.ElapsedTicks - session.LastReceive) / Stopwatch.Frequency) > options.ReadTimeout)
                {
                    session.Abort();
                    return;
                }
                if (now - lastPing < options.PingInterval.Ticks) continue;
                lastPing = now;
                var payload = new byte[8];
                BinaryPrimitives.WriteInt64BigEndian(payload, now);
                if (!session.Write(RelayCodec.Encode(new RelayFrame(RelayChannel.Control, 0, RelayOps.Ping, payload), options.MaxPayloadBytes))) return;
            }
        }

        private static bool Retryable(RelayReason reason) => reason == RelayReason.Disconnected || reason == RelayReason.IdleTimeout;

        private async Task<RelaySession> ReconnectAsync()
        {
            var backoff = options.ReconnectMinBackoff;
            Exception? last = null;
            for (int attempt = 0; attempt < options.ReconnectAttempts; attempt++)
            {
                await Task.Delay(backoff, lifetime.Token).ConfigureAwait(false);
                try { return await RejoinOnceAsync(lifetime.Token).ConfigureAwait(false); }
                catch (Exception error) when (!lifetime.IsCancellationRequested)
                {
                    last = error;
                    bool retry = error is LobbyException lobbyError ? lobbyError.IsTemporary
                        : error is RelayClosedException closed ? Retryable(closed.Reason) || closed.Reason == RelayReason.TokenRejected
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
            var ticket = await lobby.JoinRoomAsync(room, new JoinRoomRequest { Game = options.Game, Password = options.Password, Transport = RelayTransport.Tcp }, cancellation).ConfigureAwait(false);
            return await DialAsync(ticket.RelayAddr, ticket.Token, cancellation).ConfigureAwait(false);
        }

        /// <summary>拨号、发令牌首帧、等第一帧：Welcome 就进房成功，Closed 就是 relay 拒绝。relay 总是先发 Welcome。</summary>
        private async Task<RelaySession> DialAsync(string address, string token, CancellationToken cancellation)
        {
            var (host, port) = ParseAddress(address);
            using var timeout = CancellationTokenSource.CreateLinkedTokenSource(cancellation, lifetime.Token);
            timeout.CancelAfter(options.ConnectTimeout);
            var tcp = new TcpClient { NoDelay = true };
            try
            {
                using (timeout.Token.Register(() => tcp.Dispose()))
                {
                    await tcp.ConnectAsync(host, port).ConfigureAwait(false);
                    var stream = tcp.GetStream();
                    var tokenFrame = RelayCodec.EncodeToken(token);
                    await stream.WriteAsync(tokenFrame, 0, tokenFrame.Length, timeout.Token).ConfigureAwait(false);
                    var first = await RelayCodec.ReadAsync(stream, options.MaxPayloadBytes, timeout.Token).ConfigureAwait(false);
                    if (first.Channel == RelayChannel.Control && first.MessageId == RelayOps.Closed && first.Payload.Length == 1)
                        throw new RelayClosedException((RelayReason)first.Payload.Span[0]);
                    if (first.Channel != RelayChannel.Control || first.MessageId != RelayOps.Welcome)
                        throw new InvalidDataException("First relay frame is not a welcome.");
                    var welcome = RelayCodec.DecodeWelcome(first.Slot, first.Payload.Span);
                    tcp.SendTimeout = (int)options.WriteTimeout.TotalMilliseconds;
                    return new RelaySession(tcp, stream, welcome, Clock.ElapsedTicks);
                }
            }
            catch (Exception error) when (!(error is RelayClosedException) && timeout.IsCancellationRequested)
            {
                tcp.Dispose();
                cancellation.ThrowIfCancellationRequested();
                if (lifetime.IsCancellationRequested) throw new ObjectDisposedException(nameof(RelayClient));
                throw new TimeoutException($"relay: connecting to {address} timed out.", error);
            }
            catch
            {
                tcp.Dispose();
                throw;
            }
        }

        private static (string, int) ParseAddress(string address)
        {
            if (string.IsNullOrEmpty(address) || address.Contains("://"))
                throw new ArgumentException($"'{address}' is not a TCP address (the C# SDK connects over TCP; ask the lobby for transport tcp).");
            int colon = address.LastIndexOf(':');
            if (colon <= 0 || !int.TryParse(address.Substring(colon + 1), NumberStyles.None, CultureInfo.InvariantCulture, out int port))
                throw new ArgumentException($"'{address}' is not host:port.");
            return (address.Substring(0, colon).Trim('[', ']'), port);
        }
    }
}
