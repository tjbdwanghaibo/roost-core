using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.IO;
using System.Threading;
using System.Threading.Tasks;

namespace Roost.Client
{
    // 引擎无关的业务入口。构造、订阅、Request 与 Pump 在同一消费线程使用；
    // 网络和取票在后台执行，全部业务通知只由 Pump 派发。Dispose 后不再通知业务。
    public sealed class GameClient : IDisposable
    {
        public Connection Connection { get; }
        public EntityViews Entities { get; }
        public event Action<Exception>? Error;
        public event Action<ReconnectResult>? Reconnected;
        public event Action<ConnectionState>? StateChanged;
        public event Action<Packet>? UnhandledPush;
        public event Action<SyncFrame>? EntitiesChanged;
        public event Action<EntityChange>? EntityChanged;
        public event Action? EntitiesReset;
        private readonly Dictionary<uint, Action<Packet>> handlers = new Dictionary<uint, Action<Packet>>();
        private readonly ConcurrentQueue<Action> completions = new ConcurrentQueue<Action>();
        private readonly int capacity, thread = Environment.CurrentManagedThreadId;
        private readonly object completionGate = new object();
        private int pending;
        private bool disposed, pumping;
        private long version;
        private ConnectionState state;

        public GameClient(ConnectionOptions options, int maxEntities = 10000)
        {
            Entities = new EntityViews(maxEntities);
            Connection = new Connection(options);
            capacity = options.MaxPendingRequests;
        }

        // 业务可直接传生成 PB 的 Foo.Parser.ParseFrom，不在 SDK 再维护一份协议定义。
        // 每个 messageId 一位拥有者，避免页面重复注册导致同一奖励展示两遍。
        public IDisposable Subscribe<T>(uint messageId, Func<byte[], T> parse, Action<T> receive)
        {
            CheckThread();
            if (messageId == 0 || parse == null || receive == null) throw new ArgumentException("Business route and callbacks are required.");
            Action<Packet> handler = packet => receive(parse(packet.Payload.ToArray()));
            handlers.Add(messageId, handler);
            return new Subscription(() => { if (disposed) return; CheckThread(); if (handlers.TryGetValue(messageId, out var current) && current == handler) handlers.Remove(messageId); });
        }

        // 业务错误码仍在生成的 PB 应答中。failed 只表示传输、超时、取消或解析失败；
        // 请求结果未知时不自动重发。未 Pump 的回调也占容量，防止慢主线程无界积压。
        public void Request<T>(uint messageId, uint responseId, ReadOnlyMemory<byte> payload,
            Func<byte[], T> parse, Action<T> received, Action<Exception> failed, CancellationToken cancellation = default)
        {
            if (parse == null) throw new ArgumentNullException(nameof(parse));
            byte[] owned = payload.ToArray();
            BeginRequest(() => Connection.RequestAsync(messageId, responseId, owned, cancellation),
                packet => parse(packet.Payload.ToArray()), received, failed);
        }

        public void ProbeClock(Action<ClockSample> received, Action<Exception> failed, CancellationToken cancellation = default)
        {
            BeginRequest(() => Connection.ProbeClockAsync(cancellation), sample => sample, received, failed);
        }

        private void BeginRequest<TPacket, T>(Func<Task<TPacket>> request, Func<TPacket, T> parse,
            Action<T> received, Action<Exception> failed)
        {
            CheckThread();
            if (received == null || failed == null) throw new ArgumentNullException(nameof(received));
            lock (completionGate)
            {
                if (pending >= capacity) throw new InvalidOperationException("Business completion capacity exhausted.");
                pending++;
            }
            _ = CompleteRequest(request, parse, received, failed, Connection.ConnectionVersion);
        }

        private async Task CompleteRequest<TPacket, T>(Func<Task<TPacket>> request,
            Func<TPacket, T> parse, Action<T> received, Action<Exception> failed, long requestVersion)
        {
            TPacket response = default!;
            Exception? failure = null;
            try { response = await request().ConfigureAwait(false); }
            catch (Exception error) { failure = error; }
            lock (completionGate)
            {
                if (disposed) { pending--; return; }
                completions.Enqueue(() =>
                {
                    if (failure != null) { failed(failure); return; }
                    if (Connection.ConnectionVersion != requestVersion) { failed(new IOException("Response belongs to a previous connection.")); return; }
                    T value;
                    try { value = parse(response); }
                    catch (Exception error) { failed(error); return; }
                    received(value);
                });
            }
        }

        public void Pump(int budget = 64)
        {
            CheckThread();
            if (budget < 1) throw new ArgumentOutOfRangeException(nameof(budget));
            if (pumping) throw new InvalidOperationException("Recursive Pump is not supported.");
            pumping = true;
            try
            {
                long currentVersion = Connection.ConnectionVersion;
                if (version != currentVersion) { ResetEntities(); version = currentVersion; }
                var currentState = Connection.State;
                if (currentState != state)
                {
                    state = currentState;
                    if (state != ConnectionState.Ready) ResetEntities();
                    Invoke(() => StateChanged?.Invoke(state));
                }
                for (int i = 0; i < budget && !disposed; i++)
                {
                    if (!Connection.TryDequeueReconnectResult(out var result)) break;
                    Invoke(() => Reconnected?.Invoke(result!));
                }
                for (int i = 0; i < budget && !disposed && completions.TryDequeue(out var completion); i++)
                {
                    lock (completionGate) pending--;
                    Invoke(completion);
                }
                for (int i = 0; i < budget && !disposed; i++)
                {
                    if (!Connection.TryDequeuePush(out var packet, out var packetVersion)) break;
                    if (version != packetVersion) { ResetEntities(); version = packetVersion; }
                    try
                    {
                        if (packet!.Kind == PayloadKind.Sync)
                        {
                            Entities.Receive(packet.Payload.Span,
                                frame => { if (!disposed) Notify(EntitiesChanged, frame); },
                                changes =>
                                {
                                    foreach (var change in changes)
                                    {
                                        if (disposed) break;
                                        Notify(EntityChanged, change);
                                    }
                                });
                        }
                        else if (packet.Kind == PayloadKind.Protobuf && handlers.TryGetValue(packet.MessageId, out var handler)) handler(packet);
                        else UnhandledPush?.Invoke(packet);
                    }
                    catch (Exception error)
                    {
                        if (packet!.Kind == PayloadKind.Sync) { Connection.Disconnect(); ResetEntities(); }
                        Report(error);
                        if (packet.Kind == PayloadKind.Sync) break;
                    }
                }
            }
            finally { pumping = false; }
        }

        private void ResetEntities()
        {
            bool hadViews = Entities.Current.Count != 0;
            Entities.Reset();
            if (hadViews && !disposed && EntitiesReset != null)
                foreach (Action callback in EntitiesReset.GetInvocationList())
                { if (disposed) break; Invoke(callback); }
        }

        private void Notify<T>(Action<T>? handlers, T value)
        {
            if (handlers == null) return;
            foreach (Action<T> callback in handlers.GetInvocationList())
            { if (disposed) break; Invoke(() => callback(value)); }
        }

        private void Invoke(Action action) { try { action(); } catch (Exception error) { Report(error); } }
        // 错误观察者自身失败也不能回流成网络协议错误。
        private void Report(Exception error)
        {
            if (Error == null || disposed) return;
            foreach (Action<Exception> callback in Error.GetInvocationList())
            { if (disposed) break; try { callback(error); } catch { } }
        }
        private void CheckThread()
        {
            if (Environment.CurrentManagedThreadId != thread) throw new InvalidOperationException("Use the GameClient consumer thread.");
            if (disposed) throw new ObjectDisposedException(nameof(GameClient));
        }
        public void Dispose()
        {
            if (disposed) return;
            CheckThread();
            lock (completionGate) { disposed = true; while (completions.TryDequeue(out _)) pending--; }
            Connection.Dispose(); ResetEntities(); handlers.Clear();
        }
        private sealed class Subscription : IDisposable
        {
            private Action? remove;
            public Subscription(Action remove) { this.remove = remove; }
            public void Dispose() { remove?.Invoke(); remove = null; }
        }
    }
}
