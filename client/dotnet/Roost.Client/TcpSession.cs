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
    public sealed class TcpSession : IDisposable
    {
        private sealed class Pending
        {
            internal readonly uint ResponseId;
            internal readonly TaskCompletionSource<Packet> Result = new TaskCompletionSource<Packet>(TaskCreationOptions.RunContinuationsAsynchronously);
            internal Pending(uint responseId) { ResponseId = responseId; }
        }
        private readonly Stream stream;
        private readonly object gate = new object();
        private readonly SemaphoreSlim send = new SemaphoreSlim(1, 1);
        private readonly Dictionary<uint, Pending> pending = new Dictionary<uint, Pending>();
        private readonly Queue<Packet> pushes = new Queue<Packet>();
        private readonly int maxPayload, maxPending, maxPushes;
        private uint nextSequence;
        private int inFlight;
        private Exception? closed;
        public Task Completion { get; }
        public Exception? CloseReason { get { lock (gate) return closed; } }

        private TcpSession(Stream stream, int maxPayload, int maxPending, int maxPushes)
        {
            this.stream = stream; this.maxPayload = maxPayload; this.maxPending = maxPending; this.maxPushes = maxPushes;
            Completion = ReadLoopAsync();
        }

        public static async Task<TcpSession> ConnectAsync(string host, int port, string token,
            TimeSpan timeout, CancellationToken cancellation = default,
            int maxPayload = PacketCodec.DefaultMaxPayload, int maxPending = 64, int maxPushes = 256)
        {
            if (maxPayload <= 0 || maxPending <= 0 || maxPushes <= 0 || timeout <= TimeSpan.Zero)
                throw new ArgumentOutOfRangeException(nameof(timeout), "Timeout and capacities must be positive.");
            ValidateTimeout(timeout);
            byte[] ticket = Encoding.UTF8.GetBytes(token ?? throw new ArgumentNullException(nameof(token)));
            if (ticket.Length == 0 || ticket.Length > 8192) throw new ArgumentException("Ticket must be 1..8192 UTF-8 bytes.", nameof(token));
            var tcp = new TcpClient();
            TcpSession? session = null;
            using (var budget = CancellationTokenSource.CreateLinkedTokenSource(cancellation))
            {
                budget.CancelAfter(timeout);
                try
                {
                    using (budget.Token.Register(() => tcp.Dispose()))
                        await tcp.ConnectAsync(host, port).ConfigureAwait(false);
                    budget.Token.ThrowIfCancellationRequested();
                    tcp.NoDelay = true;
                    session = new TcpSession(tcp.GetStream(), maxPayload, maxPending, maxPushes);
                    Packet ack = await session.RequestAsync(0, 0, ticket, timeout, budget.Token).ConfigureAwait(false);
                    if (ack.Payload.Length != 0) throw new InvalidDataException("Authentication acknowledgement must be empty.");
                    return session;
                }
                catch
                {
                    session?.Dispose(); tcp.Dispose();
                    budget.Token.ThrowIfCancellationRequested();
                    throw;
                }
            }
        }

        // 已发送后的超时仅取消等待，不能推断服务器业务未执行，也不自动重试。
        public async Task<Packet> RequestAsync(uint messageId, uint responseId, ReadOnlyMemory<byte> payload,
            TimeSpan timeout, CancellationToken cancellation = default)
        {
            return (await SendAsync(messageId, PayloadKind.Protobuf, payload, timeout, cancellation, new Pending(responseId)).ConfigureAwait(false))!;
        }

        // Notify完成只表示写出；与Request共享序列、发送锁和容量，禁止自动重试输入。
        public async Task NotifyAsync(uint messageId, PayloadKind kind, ReadOnlyMemory<byte> payload,
            TimeSpan timeout, CancellationToken cancellation = default)
        {
            if (messageId == 0 || (kind != PayloadKind.Protobuf && kind != PayloadKind.Lockstep))
                throw new ArgumentException("Notify requires a PB/Lockstep business route.");
            await SendAsync(messageId, kind, payload, timeout, cancellation, null).ConfigureAwait(false);
        }

        private async Task<Packet?> SendAsync(uint messageId, PayloadKind kind, ReadOnlyMemory<byte> payload,
            TimeSpan timeout, CancellationToken cancellation, Pending? item)
        {
            ValidateTimeout(timeout);
            uint sequence = 0;
            lock (gate)
            {
                if (closed != null) throw new IOException("Session closed.", closed);
                if (inFlight >= maxPending) throw new InvalidOperationException("Pending request capacity exhausted.");
                inFlight++; // 包括等发送锁的请求，不能无限积累发送等待者。
            }
            using (var budget = CancellationTokenSource.CreateLinkedTokenSource(cancellation))
            {
                budget.CancelAfter(timeout);
                try
                {
                    // 先编码，坏输入不会写出半包或关闭健康连接。
                    byte[] data = PacketCodec.Encode(new Packet(messageId, 0, payload, kind), maxPayload);
                    await send.WaitAsync(budget.Token).ConfigureAwait(false);
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
                        using (budget.Token.Register(() => Close(new OperationCanceledException(budget.Token))))
                            await stream.WriteAsync(data, 0, data.Length, budget.Token).ConfigureAwait(false);
                    }
                    catch (Exception error) { if (writing) Close(error); throw; }
                    finally { send.Release(); }
                    if (item == null) return null;
                    using (budget.Token.Register(() => item.Result.TrySetCanceled()))
                        return await item.Result.Task.ConfigureAwait(false);
                }
                finally
                {
                    lock (gate) { if (sequence != 0) pending.Remove(sequence); inFlight--; }
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
                    Packet packet = await PacketCodec.ReadAsync(stream, maxPayload).ConfigureAwait(false);
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
            stream.Dispose();
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
