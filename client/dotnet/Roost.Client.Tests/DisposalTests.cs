using System;
using System.IO;
using System.Net;
using System.Net.Sockets;
using System.Threading.Tasks;
using Roost.Client;

internal static partial class Program
{
    // RV-20261101-06：消费线程中回调销毁客户端后，当前多播和后续事件必须停止。
    private static void TestGameClientDisposal()
    {
        foreach (int disposeAt in new[] { -1, 0, 1 })
        {
            using var client = new GameClient(new ConnectionOptions());
            var ticket = new TaskCompletionSource<string>(TaskCreationOptions.RunContinuationsAsynchronously);
            var opening = client.Connection.ConnectAsync(_ => ticket.Task);
            int calls = 0;
            for (int i = 0; i < 3; i++)
            {
                int subscriber = i;
                client.StateChanged += _ =>
                {
                    calls++;
                    if (subscriber == disposeAt) client.Dispose();
                };
            }
            client.Pump();
            Check(calls == (disposeAt < 0 ? 3 : disposeAt + 1), "state subscriber disposal boundary");
            client.Dispose();
            try { opening.GetAwaiter().GetResult(); } catch (OperationCanceledException) { }
        }

        using (var client = new GameClient(new ConnectionOptions()))
        {
            var frame = SyncFrame.Decode(RawSync);
            var subject = SubjectUpdate.Decode(frame.Objects[0].Components[0].Data);
            client.Entities.Register<string>(subject.Namespace, subject.Profile, subject.Encoding, subject.SchemaVersion,
                (update, previous) => "view");
            client.Entities.Receive(RawSync);
            Check(client.Entities.Current.Count > 0, "reset fixture has views");
            var ticket = new TaskCompletionSource<string>(TaskCreationOptions.RunContinuationsAsynchronously);
            var opening = client.Connection.ConnectAsync(_ => ticket.Task);
            int resetCalls = 0, stateCalls = 0;
            client.EntitiesReset += () => { resetCalls++; client.Dispose(); };
            client.EntitiesReset += () => resetCalls++;
            client.StateChanged += _ => stateCalls++;
            client.Pump();
            Check(resetCalls == 1 && stateCalls == 0 && client.Entities.Current.Count == 0,
                "reset disposal must stop later reset and state callbacks");
            try { opening.GetAwaiter().GetResult(); } catch (OperationCanceledException) { }
        }
        using (var client = new GameClient(new ConnectionOptions()))
        {
            int failures = 0;
            Action<Exception> failed = _ => { failures++; client.Dispose(); };
            failed += _ => failures++;
            // 未连接的请求确定失败，验证失败回调自身为多播时的销毁边界。
            client.Request(42, 43, ReadOnlyMemory<byte>.Empty, bytes => bytes, _ => { }, failed);
            PumpUntil(client, () => client.Connection.State == ConnectionState.Disposed);
            Check(failures == 1, "request failure callbacks after disposal");
        }
        Console.WriteLine("PASS GameClient state/reset/failure disposal boundaries");
    }

    private static void TestGameClientNetworkDisposal()
    {
        foreach (string mode in new[] { "push_first", "push_middle", "push_parse", "request_parse", "request_parse_throw", "request_callback" })
        {
            using var listener = new TcpListener(IPAddress.Loopback, 0);
            listener.Start();
            bool requestMode = mode.StartsWith("request", StringComparison.Ordinal);
            var peer = Task.Run(async () =>
            {
                using var tcp = await listener.AcceptTcpClientAsync();
                var stream = tcp.GetStream();
                var auth = await Read(stream);
                await Send(stream, new Packet(0, auth.Sequence, Array.Empty<byte>()));
                if (requestMode)
                {
                    var request = await Read(stream);
                    await Send(stream, new Packet(43, request.Sequence, Array.Empty<byte>()));
                }
                else await Send(stream, new Packet(42, 0, Array.Empty<byte>(), isPush: true));
                try { await Read(stream); } catch (IOException) { }
            });
            using var client = new GameClient(new ConnectionOptions
            {
                Host = "127.0.0.1", Port = ((IPEndPoint)listener.LocalEndpoint).Port,
                HeartbeatInterval = TimeSpan.Zero
            });
            int received = 0, failed = 0;
            client.Error += _ => failed++;
            if (mode.StartsWith("push_", StringComparison.Ordinal) && mode != "push_parse")
            {
                int disposeAt = mode == "push_first" ? 0 : 1;
                for (int i = 0; i < 3; i++)
                {
                    int subscriber = i;
                    client.UnhandledPush += _ => { received++; if (subscriber == disposeAt) client.Dispose(); };
                }
            }
            else if (mode == "push_parse")
                client.Subscribe(42, bytes => { client.Dispose(); return bytes; }, _ => received++);

            client.Connection.ConnectAsync("ticket").GetAwaiter().GetResult();
            if (requestMode)
            {
                Action<byte[]> receive = _ => { received++; client.Dispose(); };
                receive += _ => received++;
                client.Request(42, 43, ReadOnlyMemory<byte>.Empty,
                    bytes =>
                    {
                        if (mode != "request_callback") client.Dispose();
                        if (mode == "request_parse_throw") throw new InvalidDataException("parse failed after disposal");
                        return bytes;
                    }, receive, _ => failed++);
            }
            PumpUntil(client, () => client.Connection.State == ConnectionState.Disposed);
            int expected = mode == "push_first" || mode == "request_callback" ? 1 : mode == "push_middle" ? 2 : 0;
            Check(received == expected && failed == 0, "notification after disposal: " + mode);
            peer.WaitAsync(TimeSpan.FromSeconds(5)).GetAwaiter().GetResult();
        }
        Console.WriteLine("PASS GameClient TCP push/request disposal boundaries");
    }
}
