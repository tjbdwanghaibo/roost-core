using System;
using System.Collections.Generic;
using System.IO;
using System.Threading;
using System.Threading.Tasks;

namespace Roost.Client
{
    public enum ConnectionState { Disconnected, Connecting, Reconnecting, Ready, Disposed }

    /// <summary>显式创建的一条 Gate 连接；不依赖账号、场景或 ServerManager。</summary>
    public sealed class Connection : IDisposable
    {
        private readonly object gate = new object();
        private readonly ConnectionOptions options;
        private CancellationTokenSource? lifetime;
        private TcpSession? session;
        private ConnectionState state;
        private Exception? lastError;
        private readonly Random reconnectRandom = new Random();
        private long connectionVersion;
        private readonly Queue<ReconnectResult> reconnectResults = new Queue<ReconnectResult>();

        // 结果保留至调用方拉取，不依赖逐帧状态采样；纯 C# 调用方也能消费。
        public bool TryDequeueReconnectResult(out ReconnectResult? result)
        {
            lock (gate)
            {
                result = reconnectResults.Count == 0 ? null : reconnectResults.Dequeue();
                return result != null;
            }
        }

        private void RecordReconnectResult(CancellationTokenSource owner, int attempt, Exception? error, bool willRetry)
        {
            lock (gate)
            {
                if (lifetime != owner || owner.IsCancellationRequested) return;
                // 未消费事件时停止连接，不能无限积累或静默覆盖失败通知。
                if (reconnectResults.Count >= 64)
                    throw new IOException("Reconnect result queue exhausted; consume callbacks before reconnecting.");
                reconnectResults.Enqueue(new ReconnectResult(attempt, connectionVersion, error, willRetry));
            }
        }
        public long ConnectionVersion { get { lock (gate) return connectionVersion; } }

        public Connection(ConnectionOptions options)
        {
            this.options = (options ?? throw new ArgumentNullException(nameof(options))).Snapshot();
        }

        public ConnectionState State { get { lock (gate) return state; } }
        public Exception? LastError { get { lock (gate) return lastError; } }

        // 固定凭证重连时复用；短期 Ticket 应使用 provider 重载，每次握手重新获取。
        public Task ConnectAsync(string token, CancellationToken cancellation = default)
        {
            if (string.IsNullOrEmpty(token)) throw new ArgumentException("Ticket is required.", nameof(token));
            return ConnectAsync(_ => Task.FromResult(token), cancellation);
        }

        public async Task ConnectAsync(Func<CancellationToken, Task<string>> ticketProvider,
            CancellationToken cancellation = default)
        {
            if (ticketProvider == null) throw new ArgumentNullException(nameof(ticketProvider));
            CancellationTokenSource owner;
            bool reconnect;
            lock (gate)
            {
                if (state == ConnectionState.Disposed) throw new ObjectDisposedException(nameof(Connection));
                if (state != ConnectionState.Disconnected) throw new InvalidOperationException("Connection already active.");
                reconnect = connectionVersion > 0;
                owner = new CancellationTokenSource();
                lifetime = owner;
                state = ConnectionState.Connecting;
                lastError = null;
            }
            TcpSession? connected = null;
            try
            {
                using (var handshake = CancellationTokenSource.CreateLinkedTokenSource(cancellation, owner.Token))
                    connected = await OpenAsync(ticketProvider, owner, handshake.Token).ConfigureAwait(false);
                if (reconnect) RecordReconnectResult(owner, 1, null, false);
            }
            catch (Exception error)
            {
                connected?.Dispose();
                try
                {
                    if (reconnect && !cancellation.IsCancellationRequested)
                        RecordReconnectResult(owner, 1, error, false);
                }
                finally { Finish(owner, error); owner.Dispose(); }
                throw;
            }
            _ = MonitorAsync(connected, owner, ticketProvider);
        }

        private async Task<TcpSession> OpenAsync(Func<CancellationToken, Task<string>> ticketProvider,
            CancellationTokenSource owner, CancellationToken cancellation)
        {
            using (var budget = CancellationTokenSource.CreateLinkedTokenSource(cancellation))
            {
                budget.CancelAfter(options.ConnectTimeout);
                // provider 必须异步返回，不得阻塞或访问 Unity 对象；预算覆盖取票和握手。
                Task<string> ticketTask = ticketProvider(budget.Token);
                Task expired = Task.Delay(Timeout.Infinite, budget.Token);
                if (await Task.WhenAny(ticketTask, expired).ConfigureAwait(false) != ticketTask)
                {
                    _ = ticketTask.ContinueWith(task => { _ = task.Exception; },
                        CancellationToken.None, TaskContinuationOptions.OnlyOnFaulted, TaskScheduler.Default);
                    budget.Token.ThrowIfCancellationRequested();
                }
                string ticket = await ticketTask.ConfigureAwait(false);
                budget.Token.ThrowIfCancellationRequested();
                TcpSession connected = await TcpSession.ConnectAsync(options.Host, options.Port, ticket,
                    options.ConnectTimeout, budget.Token, options.MaxPayloadBytes,
                    options.MaxPendingRequests, options.MaxQueuedPushes).ConfigureAwait(false);
                lock (gate)
                {
                    if (lifetime != owner || owner.IsCancellationRequested)
                    {
                        connected.Dispose();
                        throw new OperationCanceledException();
                    }
                    session = connected;
                    state = ConnectionState.Ready;
                    connectionVersion++;
                    lastError = null;
                }
                return connected;
            }
        }

        public async Task<Packet> RequestAsync(uint messageId, uint responseId, ReadOnlyMemory<byte> payload,
            CancellationToken cancellation = default)
        {
            if (messageId == 0 || responseId == 0) throw new ArgumentException("Message 0 is reserved for Gate control.");
            TcpSession connected = GetSession();
            using (var budget = CancellationTokenSource.CreateLinkedTokenSource(cancellation))
            {
                budget.CancelAfter(options.RequestTimeout);
                try
                {
                    // 超时由本层唯一计时，便于区分业务主动取消与请求预算耗尽。
                    return await connected.RequestAsync(messageId, responseId, payload,
                        TimeSpan.FromMilliseconds(int.MaxValue), budget.Token).ConfigureAwait(false);
                }
                catch (Exception error) when (budget.IsCancellationRequested && !cancellation.IsCancellationRequested)
                {
                    throw new TimeoutException("Gate request timed out; server execution may already have occurred.", error);
                }
            }
        }

        // 返回样本，由消费线程显式交给 NetworkClock；不在网络线程修改游戏时钟。
        public async Task<ClockSample> ProbeClockAsync(CancellationToken cancellation = default)
        {
            TcpSession connected = GetSession();
            double sent = ClockSample.LocalNow;
            Packet response = await connected.RequestAsync(0, 0, new byte[] { 82, 67, 78, 49 },
                options.RequestTimeout, cancellation).ConfigureAwait(false);
            lock (gate)
                if (session != connected) throw new IOException("Clock sample belongs to a replaced connection.");
            return ClockSample.Decode(sent, ClockSample.LocalNow, response.Payload.Span);
        }

        public Task NotifyAsync(uint messageId, ReadOnlyMemory<byte> payload, CancellationToken cancellation = default)
        {
            return GetSession().NotifyAsync(messageId, PayloadKind.Protobuf, payload, options.RequestTimeout, cancellation);
        }

        // 同时取包与代际，消费线程据此重置 Sync 基线；不可分两次读取。
        public bool TryDequeuePush(out Packet? packet, out long version)
        {
            lock (gate)
            {
                version = connectionVersion;
                packet = null;
                return session != null && session.TryDequeuePush(out packet);
            }
        }

        private TcpSession GetSession()
        {
            lock (gate)
            {
                if (state != ConnectionState.Ready) throw new InvalidOperationException("Gate connection is not ready.");
                return session!;
            }
        }

        private async Task MonitorAsync(TcpSession? connected, CancellationTokenSource owner,
            Func<CancellationToken, Task<string>> ticketProvider)
        {
            Exception? error = null;
            try
            {
                while (true)
                {
                    error = await WatchConnectionAsync(connected!, owner.Token).ConfigureAwait(false);
                    connected!.Dispose(); // 在途请求失败，不进入重连队列，也不会自动重发。
                    lock (gate)
                    {
                        if (lifetime != owner || owner.IsCancellationRequested) return;
                        session = null;
                        lastError = error;
                        if (!options.AutoReconnect) break;
                        state = ConnectionState.Reconnecting;
                    }
                    connected = null;
                    for (int attempt = 0; attempt < options.MaxReconnectAttempts; attempt++)
                    {
                        double cap = Math.Min(options.ReconnectMaxDelay.TotalMilliseconds,
                            options.ReconnectInitialDelay.TotalMilliseconds * Math.Pow(2, Math.Min(attempt, 30)));
                        double jitter;
                        lock (gate) jitter = 0.5 + reconnectRandom.NextDouble() * 0.5;
                        await Task.Delay(TimeSpan.FromMilliseconds(Math.Max(1, cap * jitter)), owner.Token).ConfigureAwait(false);
                        try
                        {
                            connected = await OpenAsync(ticketProvider, owner, owner.Token).ConfigureAwait(false);
                            RecordReconnectResult(owner, attempt + 1, null, false);
                            break;
                        }
                        catch (Exception failure)
                        {
                            owner.Token.ThrowIfCancellationRequested();
                            connected?.Dispose();
                            connected = null;
                            RecordReconnectResult(owner, attempt + 1, failure,
                                attempt + 1 < options.MaxReconnectAttempts);
                            error = failure;
                            lock (gate) { if (lifetime == owner) lastError = failure; }
                        }
                    }
                    if (connected == null) break; // 连续失败达到上限，等待显式重新连接。
                }
            }
            catch (Exception failure) { error = failure; }
            finally
            {
                Finish(owner, error);
                connected?.Dispose();
                owner.Dispose();
            }
        }

        private async Task<Exception?> WatchConnectionAsync(TcpSession connected, CancellationToken cancellation)
        {
            try
            {
                while (!connected.Completion.IsCompleted)
                {
                    TimeSpan interval = options.HeartbeatInterval == TimeSpan.Zero
                        ? Timeout.InfiniteTimeSpan : options.HeartbeatInterval;
                    using (var delay = CancellationTokenSource.CreateLinkedTokenSource(cancellation))
                    {
                        Task tick = Task.Delay(interval, delay.Token);
                        Task done = await Task.WhenAny(connected.Completion, tick).ConfigureAwait(false);
                        delay.Cancel();
                        cancellation.ThrowIfCancellationRequested();
                        if (done == connected.Completion) break;
                    }
                    Packet ack = await connected.HeartbeatAsync(options.HeartbeatTimeout, cancellation).ConfigureAwait(false);
                    if (ack.Payload.Length != 0) throw new InvalidDataException("Heartbeat acknowledgement must be empty.");
                }
                return connected.CloseReason;
            }
            catch (Exception error) { return error; }
        }

        private void Finish(CancellationTokenSource owner, Exception? error)
        {
            lock (gate)
            {
                // 旧连接的迟到收尾不能改变后来建立的连接。
                if (lifetime != owner) return;
                lifetime = null;
                session = null;
                lastError = error;
                if (state != ConnectionState.Disposed) state = ConnectionState.Disconnected;
            }
        }

        public void Disconnect() { Close(false); }
        public void Dispose() { Close(true); }

        private void Close(bool dispose)
        {
            CancellationTokenSource? closingLifetime;
            TcpSession? closingSession;
            lock (gate)
            {
                if (state == ConnectionState.Disposed) return;
                closingLifetime = lifetime;
                closingSession = session;
                lifetime = null;
                session = null;
                state = dispose ? ConnectionState.Disposed : ConnectionState.Disconnected;
                lastError = null;
                if (dispose) reconnectResults.Clear();
            }
            // CancellationToken 回调可能属于业务取票逻辑，不能持有内部锁调用外部代码。
            try
            {
                try { closingLifetime?.Cancel(); }
                catch (ObjectDisposedException) { /* 旧代际已经由 Monitor 完成收尾。 */ }
            }
            finally { closingSession?.Dispose(); }
        }
    }
}
