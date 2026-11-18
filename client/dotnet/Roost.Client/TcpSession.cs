using System;
using System.Collections.Generic;
using System.IO;
using System.Net.Sockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;

namespace Roost.Client
{
    // 一实例对应一个连接生命周期。断线后创建新实例，旧应答不能跨实例匹配。
    // 名字沿用 TcpSession（公开 API），底下的连接是 IGateTransport：缺省 TCP，也可以是 WebSocket 或游戏自己的实现（GateTransport.cs）。
    public sealed class TcpSession : IDisposable
    {
        private sealed class Pending
        {
            internal readonly uint ResponseId;
            internal readonly TaskCompletionSource<Packet> Result = new TaskCompletionSource<Packet>(TaskCreationOptions.RunContinuationsAsynchronously);
            internal Pending(uint responseId) { ResponseId = responseId; }
        }
        private readonly IGateTransport transport;
        private readonly PumpScheduler scheduler;
        private readonly bool context; // 每个 await 的 ConfigureAwait 参数：PumpDriven 时回到调用者的上下文
        private readonly object gate = new object();
        private readonly SemaphoreSlim send = new SemaphoreSlim(1, 1);
        private readonly Dictionary<uint, Pending> pending = new Dictionary<uint, Pending>();
        private readonly Queue<Packet> pushes = new Queue<Packet>();
        private readonly int maxPayload, maxPending, maxPushes;
        private uint nextSequence;
        private int inFlight;
        private bool heartbeatInFlight;
        private Exception? closed;
        public Task Completion { get; }
        public Exception? CloseReason { get { lock (gate) return closed; } }

        private TcpSession(IGateTransport transport, PumpScheduler scheduler, int maxPayload, int maxPending, int maxPushes)
        {
            this.transport = transport; this.scheduler = scheduler; context = scheduler.ContinueOnContext;
            this.maxPayload = maxPayload; this.maxPending = maxPending; this.maxPushes = maxPushes;
            // PumpDriven 时读循环在调用线程上起，之后的续行回到调用者的上下文（WebGL 上由浏览器桥接的轮询完成收字节）。
            Completion = ReadLoopAsync();
        }

        public static Task<TcpSession> ConnectAsync(string host, int port, string token,
            TimeSpan timeout, CancellationToken cancellation = default,
            int maxPayload = PacketCodec.DefaultMaxPayload, int maxPending = 64, int maxPushes = 256)
        {
            if (host == null) throw new ArgumentNullException(nameof(host));
            return ConnectAsync(new TcpGateTransport(), TcpGateTransport.Address(host, port), token, timeout, cancellation,
                maxPayload, maxPending, maxPushes, PumpScheduler.Threaded);
        }

        /// <summary>
        /// 经指定的传输连接并完成鉴权握手（缺省调度：计时器与线程池）。transport 是新建、还没连过的实例，归本会话所有（失败或
        /// Dispose 时关闭）；address 交给 transport.ConnectAsync（WebSocket 是 ws:// / wss:// URL）。握手首包与连接代号与 TCP 相同。
        /// 要单线程调度（Unity WebGL）用 Connection / GameClient 的 ConnectionOptions.PumpDriven。
        /// </summary>
        public static Task<TcpSession> ConnectAsync(IGateTransport transport, string address, string token,
            TimeSpan timeout, CancellationToken cancellation = default,
            int maxPayload = PacketCodec.DefaultMaxPayload, int maxPending = 64, int maxPushes = 256)
        {
            if (transport == null) throw new ArgumentNullException(nameof(transport));
            return ConnectAsync(transport, address, token, timeout, cancellation, maxPayload, maxPending, maxPushes, PumpScheduler.Threaded);
        }

        internal static async Task<TcpSession> ConnectAsync(IGateTransport transport, string address, string token,
            TimeSpan timeout, CancellationToken cancellation, int maxPayload, int maxPending, int maxPushes, PumpScheduler scheduler)
        {
            byte[] auth;
            try
            {
                if (maxPayload <= 0 || maxPending <= 0 || maxPushes <= 0 || timeout <= TimeSpan.Zero)
                    throw new ArgumentOutOfRangeException(nameof(timeout), "Timeout and capacities must be positive.");
                ValidateTimeout(timeout);
                byte[] ticket = Encoding.UTF8.GetBytes(token ?? throw new ArgumentNullException(nameof(token)));
                // 服务端缺省的鉴权首包上限是 8 KiB（gateway TCPConfig.MaxHandshakeBytes），其中 10 字节是连接代号信封。
                if (ticket.Length == 0 || ticket.Length > 8182) throw new ArgumentException("Ticket must be 1..8182 UTF-8 bytes.", nameof(token));
                if (ticket[0] == 0) throw new ArgumentException("Ticket must not start with a NUL byte.", nameof(token));
                // 鉴权首包带本次连接的新代号（AuthPayload）：同一会话跨 Gate 重连时，Game 据此认出迟到完成的旧握手。
                // 代号在发起连接时取，越晚发起的连接越大；每次 ConnectAsync（包括 Connection 的自动重连）都取新的，与传输无关。
                auth = AuthPayload.Encode(ticket, AuthPayload.NextConnectionGeneration());
            }
            catch { transport.Dispose(); throw; }
            bool context = scheduler.ContinueOnContext;
            TcpSession? session = null;
            using (var budget = CancellationTokenSource.CreateLinkedTokenSource(cancellation))
            using (scheduler.CancelAfter(budget, timeout))
            {
                try
                {
                    // 超时或取消时关掉连接：不依赖各实现对 CancellationToken 的处理。
                    using (budget.Token.Register(transport.Dispose))
                        await transport.ConnectAsync(address, budget.Token).ConfigureAwait(context);
                    budget.Token.ThrowIfCancellationRequested();
                    session = new TcpSession(transport, scheduler, maxPayload, maxPending, maxPushes);
                    Packet ack = await session.RequestAsync(0, 0, auth, timeout, budget.Token).ConfigureAwait(context);
                    if (ack.Payload.Length != 0) throw new InvalidDataException("Authentication acknowledgement must be empty.");
                    return session;
                }
                catch
                {
                    session?.Dispose(); transport.Dispose();
                    budget.Token.ThrowIfCancellationRequested();
                    throw;
                }
            }
        }

        // 已发送后的超时仅取消等待，不能推断服务器业务未执行，也不自动重试。
        public async Task<Packet> RequestAsync(uint messageId, uint responseId, ReadOnlyMemory<byte> payload,
            TimeSpan timeout, CancellationToken cancellation = default)
        {
            return (await SendAsync(messageId, PayloadKind.Protobuf, payload, timeout, cancellation, new Pending(responseId)).ConfigureAwait(context))!;
        }

        // 仅连接监控器使用一个独立槽位，业务及校时探测不能借用；序列与写锁仍统一。
        internal async Task<Packet> HeartbeatAsync(TimeSpan timeout, CancellationToken cancellation)
        {
            return (await SendAsync(0, PayloadKind.Protobuf, ReadOnlyMemory<byte>.Empty,
                timeout, cancellation, new Pending(0), heartbeat: true).ConfigureAwait(context))!;
        }

        // Notify完成只表示写出；与Request共享序列、发送锁和容量，禁止自动重试输入。
        public async Task NotifyAsync(uint messageId, PayloadKind kind, ReadOnlyMemory<byte> payload,
            TimeSpan timeout, CancellationToken cancellation = default)
        {
            if (messageId == 0 || (kind != PayloadKind.Protobuf && kind != PayloadKind.Lockstep))
                throw new ArgumentException("Notify requires a PB/Lockstep business route.");
            await SendAsync(messageId, kind, payload, timeout, cancellation, null).ConfigureAwait(context);
        }

        private async Task<Packet?> SendAsync(uint messageId, PayloadKind kind, ReadOnlyMemory<byte> payload,
            TimeSpan timeout, CancellationToken cancellation, Pending? item, bool heartbeat = false)
        {
            ValidateTimeout(timeout);
            uint sequence = 0;
            lock (gate)
            {
                if (closed != null) throw new IOException("Session closed.", closed);
                if (heartbeat)
                {
                    if (heartbeatInFlight) throw new InvalidOperationException("Heartbeat already pending.");
                    heartbeatInFlight = true;
                }
                else
                {
                    if (inFlight >= maxPending) throw new InvalidOperationException("Pending request capacity exhausted.");
                    inFlight++; // 包括等发送锁的请求，不能无限积累发送等待者。
                }
            }
            using (var budget = CancellationTokenSource.CreateLinkedTokenSource(cancellation))
            using (scheduler.CancelAfter(budget, timeout))
            {
                try
                {
                    // 先编码，坏输入不会写出半包或关闭健康连接。
                    byte[] data = PacketCodec.Encode(new Packet(messageId, 0, payload, kind), maxPayload);
                    await send.WaitAsync(budget.Token).ConfigureAwait(context);
                    bool writing = false;
                    try
                    {
                        budget.Token.ThrowIfCancellationRequested();
                        lock (gate)
                        {
                            if (closed != null) throw new IOException("Session closed.", closed);
                            // 序列在发送锁内分配，顺序就是线上顺序；并发调用不能先发seq2再发seq1。
                            do { sequence = ++nextSequence; } while (sequence == 0 || pending.ContainsKey(sequence));
                            if (item != null) pending.Add(sequence, item);
                        }
                        System.Buffers.Binary.BinaryPrimitives.WriteUInt32BigEndian(data.AsSpan(8, 4), sequence);
                        // 写期间取消可能留下半包，保守关闭整个连接；等待应答期间取消不关连接。
                        writing = true;
                        // 一帧一次 SendAsync：WebSocket 上恰好一条二进制消息（GATEWAY §6）。
                        using (budget.Token.Register(() => Close(new OperationCanceledException(budget.Token))))
                            await transport.SendAsync(data, budget.Token).ConfigureAwait(context);
                    }
                    catch (Exception error) { if (writing) Close(error); throw; }
                    finally { send.Release(); }
                    if (item == null) return null;
                    using (budget.Token.Register(() => item.Result.TrySetCanceled()))
                        return await item.Result.Task.ConfigureAwait(context);
                }
                finally
                {
                    lock (gate)
                    {
                        if (sequence != 0) pending.Remove(sequence);
                        if (heartbeat) heartbeatInFlight = false;
                        else inFlight--;
                    }
                    // 发送失败已经抛给调用者；关闭同时失败的TCS也要观察，避免遗留未观察任务异常。
                    if (item != null && item.Result.Task.IsFaulted) _ = item.Result.Task.Exception;
                }
            }
        }

        // 由Unity/Godot主线程拉取；网络线程不调用引擎对象。队列满即断线，不能静默丢Sync delta。
        public bool TryDequeuePush(out Packet? packet)
        {
            lock (gate)
            {
                if (pushes.Count == 0) { packet = null; return false; }
                packet = pushes.Dequeue(); return true;
            }
        }

        private async Task ReadLoopAsync()
        {
            try
            {
                while (true)
                {
                    // 按包头拼帧，不看字节是怎么到的：WebSocket 上一条消息里有几帧、一帧拆成几条消息都行。
                    Packet packet = await PacketCodec.ReadAsync(transport, maxPayload, context).ConfigureAwait(context);
                    lock (gate)
                    {
                        if (closed != null) return;
                        if (packet.IsPush)
                        {
                            if (pushes.Count >= maxPushes) throw new IOException("Push queue capacity exhausted; reconnect and restore baseline.");
                            pushes.Enqueue(packet);
                        }
                        else if (pending.TryGetValue(packet.Sequence, out Pending item))
                        {
                            pending.Remove(packet.Sequence);
                            if (packet.Kind != PayloadKind.Protobuf || packet.MessageId != item.ResponseId)
                                item.Result.TrySetException(new InvalidDataException("Response message id/payload kind mismatch."));
                            else item.Result.TrySetResult(packet);
                        }
                        // 没有等待者的晚应答丢弃，不能作为push投递。
                    }
                }
            }
            catch (Exception error) { Close(error); }
        }

        private void Close(Exception reason)
        {
            lock (gate)
            {
                if (closed != null) return;
                closed = reason;
                foreach (Pending item in pending.Values) item.Result.TrySetException(reason);
                pending.Clear(); pushes.Clear();
            }
            transport.Dispose();
        }
        private static void ValidateTimeout(TimeSpan timeout)
        {
            // netstandard不同运行时的计时器上限不同；在登记inFlight前统一拒绝。
            if (timeout <= TimeSpan.Zero || timeout.TotalMilliseconds > int.MaxValue)
                throw new ArgumentOutOfRangeException(nameof(timeout));
        }
        public void Dispose() { Close(new ObjectDisposedException(nameof(TcpSession))); }
    }
}
