using System;
using System.Collections.Generic;
using System.IO;
using System.Text;
using Roost.Client;

namespace Roost.Native
{
// Unity Update 调 Pump。网络由 C++ 工作线程处理，DAO 状态与业务通知只在消费线程执行。
public sealed class NativeGameClient : IDisposable
{
    private readonly NativeClient connection;
    private readonly Dictionary<uint, Action<NativeClient.Event>> routes =
        new Dictionary<uint, Action<NativeClient.Event>>();
    public NativeGameClient() : this(NativeClient.DefaultOptions)
    {
    }
    public NativeGameClient(NativeClient.Options options)
    {
        connection = new NativeClient(options);
    }
    private readonly Dictionary<uint, Action<NativeClient.Event>> pending =
        new Dictionary<uint, Action<NativeClient.Event>>();
    private readonly int thread = Environment.CurrentManagedThreadId;
    private bool pumping, disposed, connecting;
    private ulong generation;

    public EntityViews Entities { get; } = new EntityViews();
    public event Action<EntityChange> EntityChanged;
    public event Action EntitiesReset;
    public event Action ConnectionReset;
    public event Action Ready;
    public event Action<NativeClient.Event> Closed;
    public event Action<Exception> Error;
    public event Action<NativeClient.Event> Push;

    public void Connect(string address, ushort port, string ticket)
    {
        Check();
        if (pumping || connecting)
            throw new InvalidOperationException("Reconnect after the current operation returns");
        connecting = true;
        try
        {
            connection.Disconnect();
            FailRequests();
            NotifyAll(ConnectionReset);
            Reset();
            if (!disposed)
                connection.Connect(address, port, ticket);
        }
        finally
        {
            connecting = false;
        }
    }

    public uint Request<T>(uint id, uint reply, byte[] payload, Func<byte[], T> parse, Action<T> completed,
                           Action<Exception> failed, uint timeoutMilliseconds = 0)
    {
        Check();
        if (connecting)
            throw new InvalidOperationException("Connect in progress");
        if (parse == null || completed == null || failed == null)
            throw new ArgumentNullException(nameof(parse));
        uint sequence = connection.Request(id, reply, payload, timeoutMilliseconds);
        void Complete(NativeClient.Event e)
        {
            if (e.Type != NativeClient.EventType.Response)
            {
                if (e.Type == NativeClient.EventType.RequestFailed)
                    NotifyAll(failed, new NativeRequestException(e));
                else
                    NotifyAll(failed, new IOException("Connection closed; result may be unknown"));
                return;
            }
            T value;
            try
            {
                value = parse(e.Payload);
            }
            catch (Exception error)
            {
                NotifyAll(failed, error);
                return;
            }
            NotifyAll(completed, value);
        }
        pending.Add(sequence, Complete);
        return sequence;
    }

    public uint ProbeClock(Action<Roost.Client.ClockSample> completed, Action<Exception> failed,
                           uint timeoutMilliseconds = 0)
    {
        Check();
        if (connecting) throw new InvalidOperationException("Connect in progress");
        if (completed == null || failed == null) throw new ArgumentNullException(nameof(completed));
        double sent = Roost.Client.ClockSample.LocalNow;
        uint sequence = connection.ProbeClock(timeoutMilliseconds);
        pending.Add(sequence, e =>
        {
            if (e.Type != NativeClient.EventType.Response) { NotifyAll(failed, new NativeRequestException(e)); return; }
            Roost.Client.ClockSample sample;
            try { sample = Roost.Client.ClockSample.Decode(sent, Roost.Client.ClockSample.LocalNow, e.Payload); }
            catch (Exception error) { NotifyAll(failed, error); return; }
            NotifyAll(completed, sample);
        });
        return sequence;
    }

    public bool Cancel(uint sequence)
    {
        Check();
        return connection.Cancel(sequence);
    }

    // SENT 仅表示写入 socket，不等于服务端处理成功。
    public uint Notify(uint id, uint kind, byte[] payload, Action<NativeClient.Event> completed)
    {
        Check();
        if (connecting)
            throw new InvalidOperationException("Connect in progress");
        if (completed == null)
            throw new ArgumentNullException(nameof(completed));
        uint sequence = connection.Notify(id, kind, payload);
        pending.Add(sequence, e => NotifyAll(completed, e));
        return sequence;
    }

    public IDisposable Subscribe<T>(uint id, Func<byte[], T> parse, Action<T> received)
    {
        Check();
        if (id == 0 || parse == null || received == null)
            throw new ArgumentException("PB route requires ID/parser/callback");
        Action<NativeClient.Event> route = e => NotifyAll(received, parse(e.Payload));
        routes[id] = route;
        void Release()
        {
            Check();
            if (routes.TryGetValue(id, out var current) && current == route)
                routes.Remove(id);
        }
        return new Subscription(Release);
    }
    private sealed class Subscription : IDisposable
    {
        private Action release;
        public Subscription(Action release)
        {
            this.release = release;
        }
        public void Dispose()
        {
            var action = release;
            release = null;
            action?.Invoke();
        }
    }

    public void Pump(int budget = 64)
    {
        Check();
        if (pumping || budget < 1)
            throw new InvalidOperationException("Invalid Pump");
        pumping = true;
        try
        {
            while (!disposed && budget-- > 0 && connection.Poll(out var e))
            {
                if (generation != e.Generation)
                {
                    generation = e.Generation;
                    Reset();
                    if (disposed)
                        break;
                }
                if (e.Type == NativeClient.EventType.Ready)
                {
                    NotifyAll(Ready);
                    continue;
                }
                if (e.Type == NativeClient.EventType.Closed)
                {
                    FailRequests();
                    NotifyAll(ConnectionReset);
                    Reset();
                    NotifyAll(Closed, e);
                    Report(new IOException(Encoding.UTF8.GetString(e.Payload)));
                    continue;
                }
                if (e.Type == NativeClient.EventType.Response || e.Type == NativeClient.EventType.RequestFailed ||
                    e.Type == NativeClient.EventType.Sent)
                {
                    if (pending.TryGetValue(e.Sequence, out var callback))
                    {
                        pending.Remove(e.Sequence);
                        Notify(() => callback(e));
                    }
                    continue;
                }
                if (e.Type != NativeClient.EventType.Push)
                    continue;
                if (e.Kind != (uint)PayloadKind.Sync)
                {
                    if (e.Kind == (uint)PayloadKind.Protobuf && routes.TryGetValue(e.MessageId, out var route))
                        Notify(() => route(e));
                    else
                        NotifyAll(Push, e);
                    continue;
                }
                try
                {
                    Entities.Receive(e.Payload, changed: changes =>
                                                {
                                                    foreach (var change in changes)
                                                    {
                                                        if (disposed)
                                                            break;
                                                        NotifyAll(EntityChanged, change);
                                                    }
                                                });
                }
                catch (Exception error)
                {
                    connection.Disconnect();
                    FailRequests();
                    NotifyAll(ConnectionReset);
                    Reset();
                    Report(error);
                    break;
                }
            }
        }
        finally
        {
            pumping = false;
        }
    }

    public void Disconnect()
    {
        Check();
        connection.Disconnect();
    }

    public void Dispose()
    {
        if (disposed)
            return;
        Check();
        connection.Dispose();
        disposed = true;
        pending.Clear();
        routes.Clear();
        Entities.Reset();
    }

    private void Check()
    {
        if (thread != Environment.CurrentManagedThreadId)
            throw new InvalidOperationException("Use the engine thread");
        if (disposed)
            throw new ObjectDisposedException(nameof(NativeGameClient));
    }

    private void Reset()
    {
        bool hadViews = Entities.Current.Count != 0;
        Entities.Reset();
        if (hadViews)
            NotifyAll(EntitiesReset);
    }

    private void FailRequests()
    {
        var old = new List<Action<NativeClient.Event>>(pending.Values);
        pending.Clear();
        foreach (var callback in old)
        {
            if (disposed)
                break;
            Notify(() => callback(new NativeClient.Event { Type = NativeClient.EventType.Closed }));
        }
    }

    private void Notify(Action action)
    {
        if (disposed)
            return;
        try
        {
            action();
        }
        catch (Exception error)
        {
            Report(error);
        }
    }

    private void NotifyAll(Action callbacks)
    {
        if (callbacks == null)
            return;
        foreach (Action callback in callbacks.GetInvocationList())
        {
            if (disposed)
                break;
            Notify(callback);
        }
    }

    private void NotifyAll<T>(Action<T> callbacks, T value)
    {
        if (callbacks == null)
            return;
        foreach (Action<T> callback in callbacks.GetInvocationList())
        {
            if (disposed)
                break;
            Notify(() => callback(value));
        }
    }

    private void Report(Exception error)
    {
        if (disposed || Error == null)
            return;
        foreach (Action<Exception> callback in Error.GetInvocationList())
        {
            // 错误回调也可以销毁客户端；下一观察者不能再访问已释放的引擎对象。
            if (disposed)
                break;
            try
            {
                callback(error);
            }
            catch
            {
            } // 错误观察者自身失败不能再回流为网络错误。
        }
    }
}
}

namespace Roost.Native
{
public sealed class NativeRequestException : IOException
{
    public NativeClient.RequestStatus Status { get; }
    public bool ResultUnknown =>
        Status == NativeClient.RequestStatus.TimeoutUnknown || Status == NativeClient.RequestStatus.CanceledUnknown;
    public NativeRequestException(NativeClient.Event value) : base(Encoding.UTF8.GetString(value.Payload))
    {
        Status = value.Status;
    }
}
}
