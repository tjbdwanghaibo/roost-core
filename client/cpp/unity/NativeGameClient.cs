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
        private readonly NativeClient connection = new NativeClient();
        private readonly Dictionary<uint, Action<NativeClient.Event>> pending = new Dictionary<uint, Action<NativeClient.Event>>();
        private readonly int thread = Environment.CurrentManagedThreadId;
        private bool pumping, disposed, connecting;
        private ulong generation;

        public EntityViews Entities { get; } = new EntityViews();
        public event Action<EntityChange> EntityChanged;
        public event Action EntitiesReset;
        public event Action Ready;
        public event Action<Exception> Error;
        public event Action<NativeClient.Event> Push;

        public void Connect(string address, ushort port, string ticket)
        {
            Check();
            if (pumping || connecting) throw new InvalidOperationException("Reconnect after the current operation returns");
            connecting = true;
            try
            {
                connection.Disconnect();
                FailRequests();
                Reset();
                if (!disposed) connection.Connect(address, port, ticket);
            }
            finally { connecting = false; }
        }

        public void Request<T>(uint id, uint reply, byte[] payload, Func<byte[], T> parse,
            Action<T> completed, Action<Exception> failed)
        {
            Check();
            if (connecting) throw new InvalidOperationException("Connect in progress");
            if (parse == null || completed == null || failed == null)
                throw new ArgumentNullException(nameof(parse));
            uint sequence = connection.Request(id, reply, payload);
            pending.Add(sequence, e =>
            {
                if (e.Type != NativeClient.EventType.Response)
                {
                    if (e.Type == NativeClient.EventType.RequestFailed)
                        failed(new TimeoutException(Encoding.UTF8.GetString(e.Payload)));
                    else
                        failed(new IOException("Connection closed; result may be unknown"));
                    return;
                }
                T value;
                try { value = parse(e.Payload); }
                catch (Exception error) { failed(error); return; }
                completed(value);
            });
        }

        public void Pump(int budget = 64)
        {
            Check();
            if (pumping || budget < 1) throw new InvalidOperationException("Invalid Pump");
            pumping = true;
            try
            {
                while (!disposed && budget-- > 0 && connection.Poll(out var e))
                {
                    if (generation != e.Generation)
                    {
                        generation = e.Generation;
                        Reset();
                    }
                    if (e.Type == NativeClient.EventType.Ready) { NotifyAll(Ready); continue; }
                    if (e.Type == NativeClient.EventType.Closed)
                    {
                        FailRequests();
                        Reset();
                        Report(new IOException(Encoding.UTF8.GetString(e.Payload)));
                        continue;
                    }
                    if (e.Type == NativeClient.EventType.Response || e.Type == NativeClient.EventType.RequestFailed)
                    {
                        if (pending.TryGetValue(e.Sequence, out var callback))
                        {
                            pending.Remove(e.Sequence);
                            Notify(() => callback(e));
                        }
                        continue;
                    }
                    if (e.Type != NativeClient.EventType.Push) continue;
                    if (e.Kind != (uint)PayloadKind.Sync) { NotifyAll(Push, e); continue; }
                    try
                    {
                        Entities.Receive(e.Payload, changed: changes =>
                        {
                            foreach (var change in changes)
                            {
                                if (disposed) break;
                                NotifyAll(EntityChanged, change);
                            }
                        });
                    }
                    catch (Exception error)
                    {
                        connection.Disconnect();
                        FailRequests();
                        Reset();
                        Report(error);
                        break;
                    }
                }
            }
            finally { pumping = false; }
        }

        public void Disconnect() { Check(); connection.Disconnect(); }

        public void Dispose()
        {
            if (disposed) return;
            Check();
            connection.Dispose();
            disposed = true;
            pending.Clear();
            Entities.Reset();
        }

        private void Check()
        {
            if (thread != Environment.CurrentManagedThreadId)
                throw new InvalidOperationException("Use the engine thread");
            if (disposed) throw new ObjectDisposedException(nameof(NativeGameClient));
        }

        private void Reset()
        {
            bool hadViews = Entities.Current.Count != 0;
            Entities.Reset();
            if (hadViews) NotifyAll(EntitiesReset);
        }

        private void FailRequests()
        {
            var old = new List<Action<NativeClient.Event>>(pending.Values);
            pending.Clear();
            foreach (var callback in old)
            {
                if (disposed) break;
                Notify(() => callback(new NativeClient.Event { Type = NativeClient.EventType.Closed }));
            }
        }

        private void Notify(Action action)
        {
            try { action(); }
            catch (Exception error) { Report(error); }
        }

        private void NotifyAll(Action callbacks)
        {
            if (callbacks == null) return;
            foreach (Action callback in callbacks.GetInvocationList())
            {
                if (disposed) break;
                Notify(callback);
            }
        }

        private void NotifyAll<T>(Action<T> callbacks, T value)
        {
            if (callbacks == null) return;
            foreach (Action<T> callback in callbacks.GetInvocationList())
            {
                if (disposed) break;
                Notify(() => callback(value));
            }
        }

        private void Report(Exception error)
        {
            if (disposed || Error == null) return;
            foreach (Action<Exception> callback in Error.GetInvocationList())
            {
                try { callback(error); }
                catch { } // 错误观察者自身失败不能再回流为网络错误。
            }
        }
    }
}
