using System;
using System.IO;
using System.Net;
using System.Net.Sockets;
using System.Threading;
using System.Threading.Tasks;
using Google.Protobuf;
using Google.Protobuf.WellKnownTypes;
using Roost.Client;
using Roost.Client.Codecs;

internal static partial class Program
{
    private static void PumpUntil(GameClient client, Func<bool> done)
    {
        var deadline = DateTime.UtcNow.AddSeconds(5);
        while (!done())
        {
            client.Pump();
            if (DateTime.UtcNow > deadline) throw new Exception("GameClient completion deadline exceeded");
            Thread.Sleep(1);
        }
    }

    private static void TestGameClient()
    {
        var listener = new TcpListener(IPAddress.Loopback, 0); listener.Start();
        using var stop = new CancellationTokenSource();
        int heartbeats = 0;
        var peer = Task.Run(async () =>
        {
            using var tcp = await listener.AcceptTcpClientAsync(stop.Token);
            using var stream = tcp.GetStream();
            var auth = await Read(stream);
            await Send(stream, new Packet(0, auth.Sequence, Array.Empty<byte>()));
            await Send(stream, new Packet(9, 0, new StringValue { Value = "push" }.ToByteArray(), isPush: true));
            await Send(stream, new Packet(10103, 0, RawSync, PayloadKind.Sync, true));
            try
            {
                while (!stop.IsCancellationRequested)
                {
                    var req = await Read(stream);
                    if (req.MessageId == 0)
                    {
                        Interlocked.Increment(ref heartbeats);
                        await Send(stream, new Packet(0, req.Sequence, Array.Empty<byte>()));
                    }
                    else if (req.MessageId == 1)
                    {
                        string text = StringValue.Parser.ParseFrom(req.Payload.ToArray()).Value;
                        await Send(stream, new Packet(2, req.Sequence, new StringValue { Value = text + " reply" }.ToByteArray()));
                    }
                    // id 3 故意不应答：只验证业务预算耗尽，不靠断线代替超时。
                }
            }
            catch (IOException) { }
        });
        try
        {
            using var client = new GameClient(new ConnectionOptions
            {
                Host = "127.0.0.1", Port = ((IPEndPoint)listener.LocalEndpoint).Port,
                RequestTimeout = TimeSpan.FromMilliseconds(100), HeartbeatInterval = TimeSpan.FromMilliseconds(20),
                HeartbeatTimeout = TimeSpan.FromSeconds(2), MaxPendingRequests = 2
            });
            int callbacks = 0, consumer = Environment.CurrentManagedThreadId;
            bool pushed = false, received = false, timeout = false;
            int entityChanges = 0, resets = 0, reportedErrors = 0;
            var sync = SyncFrame.Decode(RawSync);
            var subject = SubjectUpdate.Decode(sync.Objects[0].Components[0].Data);
            client.Entities.Register<string>(subject.Namespace, subject.Profile, subject.Encoding, subject.SchemaVersion,
                (update, previous) => Convert.ToHexString(update.Payload));
            client.EntityChanged += _ => throw new InvalidOperationException("UI failed");
            client.Error += _ => { reportedErrors++; throw new Exception("error observer failed"); };
            client.EntityChanged += change =>
            {
                Check(change.Kind == EntityChangeKind.Full && client.Entities.Current.Count == 1 &&
                    consumer == Environment.CurrentManagedThreadId, "entity callback after commit on consumer");
                entityChanges++;
            };
            client.EntitiesReset += () => { Check(client.Entities.Current.Count == 0, "reset notification after clearing"); resets++; };
            using var subscription = client.Subscribe(9, StringValue.Parser, (StringValue value) =>
            { Check(value.Value == "push" && consumer == Environment.CurrentManagedThreadId, "PB push on consumer"); pushed = true; });
            client.Connection.ConnectAsync("ticket").GetAwaiter().GetResult();
            client.Request(1, 2, new StringValue { Value = "hello" }, StringValue.Parser,
                (StringValue value) => { Check(value.Value == "hello reply", "typed PB response"); received = true; callbacks++; }, error => throw error);
            client.Request(3, 4, Array.Empty<byte>(), StringValue.Parser.ParseFrom,
                (StringValue _) => throw new Exception("timeout succeeded"), error => { Check(error is TimeoutException, "typed timeout error"); timeout = true; callbacks++; });
            Refuses(() => client.Request(1, 2, Array.Empty<byte>(), StringValue.Parser.ParseFrom,
                (StringValue _) => { }, _ => { }), "unbounded callback admission", typeof(InvalidOperationException));
            PumpUntil(client, () => pushed && received && timeout && entityChanges == 1 && Volatile.Read(ref heartbeats) > 0);
            Check(callbacks == 2, "request callbacks exactly once");
            Check(reportedErrors == 1 && client.Connection.State == ConnectionState.Ready, "UI exceptions do not disconnect sync");
            client.Connection.Disconnect();
            client.Pump();
            Check(resets == 1, "disconnect resets views once");
            stop.Cancel();
            peer.WaitAsync(TimeSpan.FromSeconds(5)).GetAwaiter().GetResult();
        }
        finally { stop.Cancel(); listener.Stop(); }
        Console.WriteLine("PASS GameClient real PB callbacks/timeout/heartbeat/bounded admission");
    }

    private static void TestConnectionReconnect(int disposeAt = -1)
    {
        var listener = new TcpListener(IPAddress.Loopback, 0); listener.Start();
        var release = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var peer = Task.Run(async () =>
        {
            using (var first = await listener.AcceptTcpClientAsync())
            {
                var auth = await Read(first.GetStream());
                await Send(first.GetStream(), new Packet(0, auth.Sequence, Array.Empty<byte>()));
                await release.Task;
            }
            using var second = await listener.AcceptTcpClientAsync();
            var next = await Read(second.GetStream());
            Check(System.Text.Encoding.UTF8.GetString(next.Payload.Span) == "ticket2", "reconnect renews credential");
            await Send(second.GetStream(), new Packet(0, next.Sequence, Array.Empty<byte>()));
            try { await Read(second.GetStream()); } catch (IOException) { }
        });
        try
        {
            using var client = new GameClient(new ConnectionOptions
            {
                Host = "127.0.0.1", Port = ((IPEndPoint)listener.LocalEndpoint).Port,
                AutoReconnect = true, HeartbeatInterval = TimeSpan.Zero,
                ReconnectInitialDelay = TimeSpan.FromMilliseconds(5), ReconnectMaxDelay = TimeSpan.FromMilliseconds(5)
            });
            int tickets = 0; ReconnectResult? result = null;
            int callbacks = 0;
            for (int i = 0; i < 3; i++)
            {
                int subscriber = i;
                client.Reconnected += value =>
                {
                    result = value; callbacks++;
                    if (subscriber == disposeAt) client.Dispose();
                };
            }
            client.Connection.ConnectAsync(_ => Task.FromResult("ticket" + Interlocked.Increment(ref tickets))).GetAwaiter().GetResult();
            release.SetResult(true);
            PumpUntil(client, () => result != null);
            Check(result!.Succeeded && result.ConnectionVersion == 2 && tickets == 2, "reconnect result");
            Check(callbacks == (disposeAt < 0 ? 3 : disposeAt + 1), "reconnect callbacks after disposal");
            client.Dispose(); client.Dispose();
            peer.WaitAsync(TimeSpan.FromSeconds(5)).GetAwaiter().GetResult();
        }
        finally { listener.Stop(); release.TrySetResult(true); }
        Console.WriteLine("PASS GameClient reconnect/renewed ticket/generation/callback");
    }
    private static void TestReconnectFailureLimit()
    {
        var listener = new TcpListener(IPAddress.Loopback, 0); listener.Start();
        var release = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var peer = Task.Run(async () =>
        {
            using var tcp = await listener.AcceptTcpClientAsync();
            var auth = await Read(tcp.GetStream());
            await Send(tcp.GetStream(), new Packet(0, auth.Sequence, Array.Empty<byte>()));
            await release.Task;
        });
        try
        {
            using var client = new GameClient(new ConnectionOptions {
                Host = "127.0.0.1", Port = ((IPEndPoint)listener.LocalEndpoint).Port,
                AutoReconnect = true, MaxReconnectAttempts = 2, HeartbeatInterval = TimeSpan.Zero,
                ReconnectInitialDelay = TimeSpan.FromMilliseconds(5), ReconnectMaxDelay = TimeSpan.FromMilliseconds(5)
            });
            int ticketCalls = 0;
            var results = new System.Collections.Generic.List<ReconnectResult>();
            client.Reconnected += results.Add;
            client.Connection.ConnectAsync(_ => ++ticketCalls == 1 ? Task.FromResult("ticket") :
                Task.FromException<string>(new IOException("ticket service unavailable"))).GetAwaiter().GetResult();
            release.SetResult(true);
            PumpUntil(client, () => results.Count == 2 && client.Connection.State == ConnectionState.Disconnected);
            Check(!results[0].Succeeded && results[0].WillRetry && !results[1].Succeeded && !results[1].WillRetry,
                "each failed attempt reports whether it will retry");
            Check(ticketCalls == 3, "bounded reconnect attempts");
            peer.GetAwaiter().GetResult();
        }
        finally { release.TrySetResult(true); listener.Stop(); }
        Console.WriteLine("PASS bounded reconnect failure callbacks");
    }

}
