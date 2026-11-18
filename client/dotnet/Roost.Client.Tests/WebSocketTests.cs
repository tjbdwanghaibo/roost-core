using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.IO;
using System.Net;
using System.Net.Sockets;
using System.Net.WebSockets;
using System.Security.Cryptography;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client;

// C# 经 WebSocket 连玩家接入（维护者 2026-11-18；ConnectionOptions.WebSocketUrl / TransportFactory / PumpDriven）：
//   - TestWebSocketMessageBoundaries（本地，随金样自测跑）：假的 WebSocket 玩家接入逐条核对“客户端每条二进制消息恰好一帧”，
//     并把自己的帧拆成两条消息、几帧合成一条消息发回，核对 SDK 按字节流拼帧；鉴权首包带连接代号信封；文本消息断开连接；
//   - WebSocketAgainstPlayerAccess（Go 驱动：infra/network/gateway 的 TestTheCSharpClientWorksOverWebSocket）：连真实 TCPServer 的
//     WebSocket 监听，缺省调度与 PumpDriven 单线程调度各跑一遍；
//   - WebSocketAgainstGate（Go 驱动：…ThroughAStandaloneGate）：经独立 Gate，PumpDriven 单线程调度。
// 单线程环境 StrictLoop 照 Roost.Client.Relay.Tests/SingleThread.cs：一个专用线程 + 只在它上面执行的 SynchronizationContext，
// 每一轮先执行投进来的续行、再轮询连接、再对每个 GameClient Pump。线程池与计时器在 .NET 上没法真的停掉，改成“用了就记一笔”：
// 从别的线程往上下文投续行（Task.Delay、CancelAfter、IO 线程完成的任务），或在别的线程上调传输，都记违规。
// ROOST_CLIENT_SINGLE_THREAD_OFF=1 时 PumpDriven 那一轮关着开关跑（红：记到违规）。
internal static partial class Program
{
    private static readonly Encoding Utf8 = new UTF8Encoding(false);

    // ---------- 本地：消息边界 ----------

    private static async Task TestWebSocketMessageBoundaries()
    {
        Refuses(() => new Connection(new ConnectionOptions { WebSocketUrl = "http://127.0.0.1:1/ws" }), "http url as WebSocketUrl", typeof(ArgumentException));
        Refuses(() => new Connection(new ConnectionOptions { WebSocketUrl = "127.0.0.1:7080" }), "host:port as WebSocketUrl", typeof(ArgumentException));
        var listener = new TcpListener(IPAddress.Loopback, 0); listener.Start();
        string url = $"ws://127.0.0.1:{((IPEndPoint)listener.LocalEndpoint).Port}/ws";
        var heartbeats = 0;
        var textSent = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var peer = Task.Run(async () =>
        {
            var (socket, headers) = await AcceptWebSocketAsync(listener);
            using var _ = socket;
            Check(!headers.Contains("origin:", StringComparison.OrdinalIgnoreCase), "the SDK sends no Origin header");
            var auth = await ReadOneFrameMessage(socket);
            byte[] ticket = AuthPayload.Decode(auth.Payload.Span, out ulong generation);
            Check(auth.MessageId == 0 && Utf8.GetString(ticket) == "ws-ticket" && generation != 0, "auth frame carries the generation envelope");
            // 握手应答拆成两条消息：包头的前 5 字节一条，其余一条。
            byte[] ack = PacketCodec.Encode(new Packet(0, auth.Sequence, Array.Empty<byte>()));
            await socket.SendAsync(ack.AsMemory(0, 5), WebSocketMessageType.Binary, true, CancellationToken.None);
            await socket.SendAsync(ack.AsMemory(5), WebSocketMessageType.Binary, true, CancellationToken.None);
            while (true)
            {
                var request = await ReadOneFrameMessage(socket);
                if (request.MessageId == 0)
                {
                    Interlocked.Increment(ref heartbeats);
                    await socket.SendAsync(PacketCodec.Encode(new Packet(0, request.Sequence, Array.Empty<byte>())), WebSocketMessageType.Binary, true, CancellationToken.None);
                    continue;
                }
                Check(request.MessageId == 42 && Utf8.GetString(request.Payload.Span) == "hello", "request frame");
                // 应答与一条推送合在一条消息里。
                var merged = new MemoryStream();
                merged.Write(PacketCodec.Encode(new Packet(43, request.Sequence, Utf8.GetBytes("world"))));
                merged.Write(PacketCodec.Encode(new Packet(9, 1, Utf8.GetBytes("pushed"), isPush: true)));
                await socket.SendAsync(merged.ToArray(), WebSocketMessageType.Binary, true, CancellationToken.None);
                break;
            }
            await textSent.Task; // 客户端收完应答与推送之后
            while (Volatile.Read(ref heartbeats) < 2)
            {
                var beat = await ReadOneFrameMessage(socket);
                Check(beat.MessageId == 0, "only heartbeats after the request");
                Interlocked.Increment(ref heartbeats);
                await socket.SendAsync(PacketCodec.Encode(new Packet(0, beat.Sequence, Array.Empty<byte>())), WebSocketMessageType.Binary, true, CancellationToken.None);
            }
            await socket.SendAsync(Utf8.GetBytes("not a frame"), WebSocketMessageType.Text, true, CancellationToken.None);
            try { while (true) await ReadOneFrameMessage(socket); } catch (Exception) { /* 客户端断开 */ }
        });
        try
        {
            using var client = new GameClient(new ConnectionOptions
            {
                WebSocketUrl = url, HeartbeatInterval = TimeSpan.FromMilliseconds(50), HeartbeatTimeout = TimeSpan.FromSeconds(2),
            });
            string? pushed = null, reply = null;
            client.Subscribe(9, bytes => Utf8.GetString(bytes), value => pushed = value);
            var connect = client.Connection.ConnectAsync("ws-ticket");
            PumpUntilPeer(client, () => connect.IsCompleted, peer, "handshake over a split acknowledgement");
            if (connect.IsFaulted)
            {
                // 对端核对失败时会关掉连接：先报对端的断言，客户端一侧的断线只是它的后果。
                peer.ContinueWith(_ => { }).Wait(TimeSpan.FromSeconds(2));
                if (peer.IsFaulted) throw new Exception("websocket peer failed during the handshake", peer.Exception!.InnerException);
            }
            connect.GetAwaiter().GetResult();
            client.Request(42, 43, Utf8.GetBytes("hello"), bytes => Utf8.GetString(bytes), value => reply = value, error => throw error);
            PumpUntilPeer(client, () => reply != null && pushed != null, peer, "response and push merged in one message");
            Check(reply == "world" && pushed == "pushed", "response and push decoded from one message");
            textSent.SetResult(true);
            PumpUntilPeer(client, () => client.Connection.State == ConnectionState.Disconnected, peer, "a text message closes the connection");
            Check(client.Connection.LastError is InvalidDataException, "text message is a protocol error: " + client.Connection.LastError);
            await peer.WaitAsync(TimeSpan.FromSeconds(5));
            Console.WriteLine("PASS websocket: one frame per message, frames split / merged across messages, auth envelope, text closes");
        }
        finally { listener.Stop(); }
    }

    /// <summary>读一条完整的消息，要求它恰好是一帧（SDK 一帧一条消息，GATEWAY §6）。</summary>
    private static async Task<Packet> ReadOneFrameMessage(WebSocket socket)
    {
        var message = new MemoryStream();
        var buffer = new byte[64 << 10];
        while (true)
        {
            var result = await socket.ReceiveAsync(buffer, CancellationToken.None);
            if (result.MessageType == WebSocketMessageType.Close) throw new EndOfStreamException();
            message.Write(buffer, 0, result.Count);
            if (result.EndOfMessage) break;
        }
        try { return PacketCodec.Decode(message.ToArray()); }
        catch (InvalidDataException error) { throw new Exception($"a client message of {message.Length} bytes is not exactly one frame", error); }
    }

    /// <summary>最小的 WebSocket 服务端握手（RFC 6455 §4.2）：读请求头、回 101，再交给 WebSocket.CreateFromStream。</summary>
    private static async Task<(WebSocket, string)> AcceptWebSocketAsync(TcpListener listener)
    {
        var tcp = await listener.AcceptTcpClientAsync();
        var stream = tcp.GetStream();
        var head = new MemoryStream();
        var one = new byte[1];
        while (!head.ToArray().AsSpan().EndsWith("\r\n\r\n"u8))
        {
            if (await stream.ReadAsync(one) == 0) throw new EndOfStreamException();
            head.WriteByte(one[0]);
        }
        string headers = Encoding.ASCII.GetString(head.ToArray());
        string key = "";
        foreach (var line in headers.Split("\r\n"))
            if (line.StartsWith("Sec-WebSocket-Key:", StringComparison.OrdinalIgnoreCase)) key = line.Substring(18).Trim();
        string accept = Convert.ToBase64String(SHA1.HashData(Encoding.ASCII.GetBytes(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")));
        await stream.WriteAsync(Encoding.ASCII.GetBytes(
            "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"));
        return (WebSocket.CreateFromStream(stream, true, null, Timeout.InfiniteTimeSpan), headers);
    }

    private static void PumpUntilPeer(GameClient client, Func<bool> done, Task peer, string what)
    {
        var deadline = DateTime.UtcNow.AddSeconds(5);
        while (!done() || peer.IsFaulted)
        {
            if (peer.IsFaulted) throw new Exception("websocket peer failed during: " + what, peer.Exception!.InnerException);
            client.Pump();
            if (DateTime.UtcNow > deadline) throw new Exception("timed out: " + what);
            Thread.Sleep(1);
        }
    }

    // ---------- Go 驱动：真实玩家接入 ----------

    private static async Task<int> WebSocketAgainstPlayerAccess(string url, uint request, uint response, uint push)
    {
        var ids = new WsIds(request, response, push);
        // 缺省调度：线程池与计时器，GameClient 在本线程上 Pump。
        await Task.Run(() => WsThreaded(url, ids));
        Console.WriteLine("PASS websocket player access (threaded): handshake, request, clock probe, push, heartbeat past idle_timeout, reconnect with a newer generation");
        // 探测器自检：PumpDriven 关着时在单线程环境里连上，必须记到违规。
        var violations = await StrictLoop.RunAsync("detector", TimeSpan.FromSeconds(20), async loop =>
        {
            var client = loop.Client(WsOptions(url, pumpDriven: false, loop));
            await loop.Catch(client.Connection.ConnectAsync("ws-detector"));
            await loop.Sleep(0.5);
        }, expectViolations: true);
        Check(violations.Count > 0, "the detector catches the threaded scheduling (PumpDriven off)");
        Console.WriteLine("PASS websocket single-thread detector, e.g.: " + violations[0]);
        bool pumpDriven = Environment.GetEnvironmentVariable("ROOST_CLIENT_SINGLE_THREAD_OFF") != "1";
        await StrictLoop.RunAsync("pump-driven", TimeSpan.FromSeconds(60), loop => WsPumpDriven(loop, url, ids, pumpDriven, full: true), expectViolations: false);
        Console.WriteLine("PASS websocket player access (PumpDriven, one thread): handshake, request, clock probe, push, heartbeat, reconnect, request timeout, no Pump → idle close → reconnect, connect timeout");
        return 0;
    }

    private static async Task<int> WebSocketAgainstGate(string url, uint request, uint response)
    {
        bool pumpDriven = Environment.GetEnvironmentVariable("ROOST_CLIENT_SINGLE_THREAD_OFF") != "1";
        await StrictLoop.RunAsync("gate", TimeSpan.FromSeconds(60), loop => WsPumpDriven(loop, url, new WsIds(request, response, 0), pumpDriven, full: false), expectViolations: false);
        Console.WriteLine("PASS websocket standalone Gate (PumpDriven, one thread): handshake, request, heartbeat past idle_timeout, no Pump → idle close → reconnect");
        return 0;
    }

    private readonly struct WsIds
    {
        public readonly uint Request, Response, Push;
        public WsIds(uint request, uint response, uint push) { Request = request; Response = response; Push = push; }
    }

    /// <summary>Go 用例的空闲超时是 1s：心跳 250ms，重连退避短。</summary>
    private static ConnectionOptions WsOptions(string url, bool pumpDriven, StrictLoop? loop) => new ConnectionOptions
    {
        WebSocketUrl = url, AutoReconnect = true, HeartbeatInterval = TimeSpan.FromMilliseconds(250), HeartbeatTimeout = TimeSpan.FromSeconds(2),
        ReconnectInitialDelay = TimeSpan.FromMilliseconds(50), ReconnectMaxDelay = TimeSpan.FromMilliseconds(200), RequestTimeout = TimeSpan.FromSeconds(1),
        PumpDriven = pumpDriven, TransportFactory = loop == null ? null : () => new PolledWebSocketTransport(loop),
    };

    private static void WsThreaded(string url, WsIds ids)
    {
        using var client = new GameClient(WsOptions(url, pumpDriven: false, null));
        var pushes = new Queue<string>();
        var reconnects = new Queue<ReconnectResult>();
        client.Subscribe(ids.Push, bytes => Utf8.GetString(bytes), pushes.Enqueue);
        client.Reconnected += reconnects.Enqueue;
        client.Connection.ConnectAsync("ws-threaded").GetAwaiter().GetResult();
        string Call(string text)
        {
            string? got = null; Exception? failed = null;
            client.Request(ids.Request, ids.Response, Utf8.GetBytes(text), bytes => Utf8.GetString(bytes), value => got = value, error => failed = error);
            PumpUntil(client, () => got != null || failed != null);
            if (failed != null) throw new Exception($"request '{text}' failed", failed);
            return got!;
        }
        Check(Call("hello") == "hello", "threaded echo");
        ulong first = ulong.Parse(Call("gen"));
        ClockSample? sample = null;
        client.ProbeClock(value => sample = value, error => throw error);
        PumpUntil(client, () => sample != null);
        Check(Call("push:p1") == "ok", "push request");
        PumpUntil(client, () => pushes.Count > 0);
        Check(pushes.Dequeue() == "p1", "push payload");
        PumpFor(client, 2.5);
        Check(client.Connection.State == ConnectionState.Ready && client.Connection.ConnectionVersion == 1 && reconnects.Count == 0,
            "heartbeats kept the connection past idle_timeout: " + client.Connection.LastError);
        Check(client.Connection.HeartbeatRoundTrips >= 5, "heartbeat round trips while holding: " + client.Connection.HeartbeatRoundTrips);
        Check(Call("close") == "closing", "close request");
        PumpUntil(client, () => reconnects.Count > 0);
        var back = reconnects.Dequeue();
        Check(back.Succeeded && client.Connection.ConnectionVersion == 2, "reconnected after the server closed: " + back.Error);
        Check(ulong.Parse(Call("gen")) > first, "the reconnect carries a newer generation");
        Check(Call("again") == "again", "echo after reconnect");
    }

    private static void PumpFor(GameClient client, double seconds)
    {
        var until = DateTime.UtcNow.AddSeconds(seconds);
        while (DateTime.UtcNow < until) { client.Pump(); Thread.Sleep(5); }
    }

    /// <summary>
    /// PumpDriven 的关键路径，全在 loop 线程上；每一步之后核对零违规。full 为 false 时（独立 Gate，Game 只原样回）只跑握手、请求、
    /// 心跳与不 Pump 之后的重连。
    /// </summary>
    private static async Task WsPumpDriven(StrictLoop loop, string url, WsIds ids, bool pumpDriven, bool full)
    {
        void Clean(string step) =>
            Check(loop.Violations.IsEmpty, $"{step}: everything ran on the loop thread; violations: {string.Join(" | ", loop.Violations)}");
        var client = loop.Client(WsOptions(url, pumpDriven, loop));
        var replies = new Queue<string>();
        var failures = new Queue<Exception>();
        var pushes = new Queue<string>();
        var reconnects = new Queue<ReconnectResult>();
        if (full) client.Subscribe(ids.Push, bytes => Utf8.GetString(bytes), value => { loop.Assert("push callback"); pushes.Enqueue(value); });
        client.Reconnected += result => { loop.Assert("reconnect callback"); reconnects.Enqueue(result); };
        async Task<string> Call(string text)
        {
            client.Request(ids.Request, ids.Response, Utf8.GetBytes(text), bytes => Utf8.GetString(bytes),
                value => { loop.Assert("response callback"); replies.Enqueue(value); }, error => failures.Enqueue(error));
            await loop.Until($"reply to '{text}'", () => replies.Count > 0 || failures.Count > 0);
            if (failures.Count > 0) throw new Exception($"request '{text}' failed", failures.Dequeue());
            return replies.Dequeue();
        }

        await client.Connection.ConnectAsync(full ? "ws-pump" : "ws-gate");
        Check(await Call("hello") == "hello", "echo on one thread");
        Clean("connect and request");
        ulong first = 0;
        if (full)
        {
            first = ulong.Parse(await Call("gen"));
            var samples = new Queue<ClockSample>();
            client.ProbeClock(samples.Enqueue, failures.Enqueue);
            await loop.Until("clock sample", () => samples.Count > 0 || failures.Count > 0);
            Check(samples.Count > 0, "clock probe on one thread");
            Check(await Call("push:p2") == "ok", "push request");
            Check(await loop.Next(pushes, "push") == "p2", "push on one thread");
            Clean("clock probe and push");
        }

        // 心跳只靠 Pump：停 2.5 个 idle_timeout，连接还在。
        await loop.Sleep(2.5);
        Check(client.Connection.State == ConnectionState.Ready && client.Connection.ConnectionVersion == 1 && reconnects.Count == 0,
            "heartbeats on Pump kept the connection past idle_timeout: " + client.Connection.LastError);
        Check(client.Connection.HeartbeatRoundTrips >= 5, "heartbeat round trips on Pump while holding: " + client.Connection.HeartbeatRoundTrips);
        Clean("heartbeat on Pump");

        if (full)
        {
            // 服务端断开 → 自动重连：退避由 Pump 按时钟完成。
            Check(await Call("close") == "closing", "close request");
            var back = await loop.Next(reconnects, "reconnect after the server closed");
            Check(back.Succeeded && client.Connection.ConnectionVersion == 2, "reconnected: " + back.Error);
            Check(ulong.Parse(await Call("gen")) > first, "the reconnect carries a newer generation");
            Clean("reconnect");

            // 请求超时由 Pump 触发：服务端 1.5s 才回，RequestTimeout 1s。
            client.Request(ids.Request, ids.Response, Utf8.GetBytes("slow"), bytes => Utf8.GetString(bytes), replies.Enqueue, failures.Enqueue);
            var timedOut = await loop.Next(failures, "request timeout", 4);
            Check(timedOut is TimeoutException, "request timeout fired by Pump: " + timedOut.GetType().Name);
            await loop.Sleep(0.8); // 迟到的应答被丢弃，不交给业务
            Check(replies.Count == 0 && client.Connection.State == ConnectionState.Ready, "late reply dropped, connection kept");
            Clean("request timeout");
        }

        // 不 Pump：像加载场景时 Unity 不调 Pump，不发心跳，过了 idle_timeout 被服务端断开；停着时不派发通知，恢复后重连回来。
        long version = client.Connection.ConnectionVersion;
        loop.Paused.Add(client);
        await loop.Until("the server closed the unpumped connection", () => client.Connection.State != ConnectionState.Ready, 5);
        await loop.Sleep(0.3);
        Check(reconnects.Count == 0, "no notifications without Pump");
        loop.Paused.Remove(client);
        var resumed = await loop.Next(reconnects, "reconnect after Pump resumed");
        Check(resumed.Succeeded && client.Connection.ConnectionVersion == version + 1, "reconnected after Pump resumed: " + resumed.Error);
        Check(await Call("after") == "after", "echo after the idle reconnect");
        Clean("no Pump → idle close → reconnect");

        if (full)
        {
            // 连接超时由 Pump 触发：对端接受 TCP 却不回 WebSocket 升级。
            var silent = new TcpListener(IPAddress.Loopback, 0);
            silent.Start();
            try
            {
                var options = WsOptions($"ws://127.0.0.1:{((IPEndPoint)silent.LocalEndpoint).Port}/ws", pumpDriven, loop);
                options.ConnectTimeout = TimeSpan.FromMilliseconds(300);
                var waiting = loop.Client(options);
                var started = DateTime.UtcNow;
                var error = await loop.Catch(waiting.Connection.ConnectAsync("ws-pump"));
                Check(error is OperationCanceledException, "connect timeout fired by Pump: " + error?.GetType().Name);
                Check(DateTime.UtcNow - started < TimeSpan.FromSeconds(3), "connect timeout fired in time");
                // 超时之后的续行（这里）不在 Pump 里：到期的计时投到上下文再触发，所以这里能调 Pump（不可重入）。
                waiting.Pump();
                loop.Remove(waiting);
            }
            finally { silent.Stop(); }
            Clean("connect timeout");
        }
        loop.Remove(client);
        Clean("dispose");
    }

    /// <summary>像 Unity WebGL 主线程的单线程环境（见文件开头）。</summary>
    internal sealed class StrictLoop : SynchronizationContext
    {
        private readonly Thread thread;
        private readonly Queue<(SendOrPostCallback callback, object? state)> posted = new Queue<(SendOrPostCallback, object?)>();
        private readonly List<IPolled> polled = new List<IPolled>();
        private readonly List<(Func<bool> done, DateTime deadline, TaskCompletionSource<bool> signal)> waits = new List<(Func<bool>, DateTime, TaskCompletionSource<bool>)>();
        private readonly List<GameClient> clients = new List<GameClient>();
        public readonly ConcurrentQueue<string> Violations = new ConcurrentQueue<string>();
        /// <summary>每一轮 Pump 之前调用（像 Unity 每帧先 BrowserGateTransport.PollAll 再 Pump）。</summary>
        public readonly List<Action> BeforePump = new List<Action>();
        /// <summary>这些客户端这一轮不 Pump（模拟游戏不调 Pump）。</summary>
        public readonly HashSet<GameClient> Paused = new HashSet<GameClient>();

        private StrictLoop(Thread thread) { this.thread = thread; }

        public static async Task<List<string>> RunAsync(string name, TimeSpan limit, Func<StrictLoop, Task> body, bool expectViolations)
        {
            var finished = new TaskCompletionSource<List<string>>(TaskCreationOptions.RunContinuationsAsynchronously);
            var thread = new Thread(() =>
            {
                var loop = new StrictLoop(Thread.CurrentThread);
                SetSynchronizationContext(loop);
                Task? run = null;
                Exception? failure = null;
                loop.Post(_ => run = body(loop), null);
                var deadline = DateTime.UtcNow + limit;
                try
                {
                    while (run == null || !run.IsCompleted)
                    {
                        loop.Turn();
                        if (DateTime.UtcNow > deadline) throw new TimeoutException($"single-thread run '{name}' did not finish within {limit.TotalSeconds}s");
                        Thread.Sleep(1);
                    }
                    run.GetAwaiter().GetResult();
                }
                catch (Exception error) { failure = error; }
                finally { foreach (var client in loop.clients.ToArray()) client.Dispose(); }
                if (failure != null) finished.TrySetException(failure);
                else finished.TrySetResult(new List<string>(loop.Violations));
            }) { IsBackground = true, Name = "strict-loop-" + name };
            thread.Start();
            var violations = await finished.Task;
            if (!expectViolations) Check(violations.Count == 0, $"single-thread run '{name}': no violations: {string.Join(" | ", violations)}");
            return violations;
        }

        /// <summary>在 loop 线程上建一个由本环境每一轮 Pump 的 GameClient。</summary>
        public GameClient Client(ConnectionOptions options)
        {
            var client = new GameClient(options);
            client.Error += error => Violations.Enqueue("callback threw: " + error.Message);
            clients.Add(client);
            return client;
        }

        public void Remove(GameClient client) { clients.Remove(client); client.Dispose(); }

        /// <summary>当前线程不是 loop 线程时记一笔。</summary>
        public void Assert(string what)
        {
            var current = Thread.CurrentThread;
            if (current != thread)
                Violations.Enqueue($"{what} on thread {current.ManagedThreadId}{(current.IsThreadPoolThread ? " (thread pool)" : "")}");
        }

        public override void Post(SendOrPostCallback callback, object? state)
        {
            Assert("a continuation posted to the loop");
            lock (posted) posted.Enqueue((callback, state)); // 照样执行：违规记下，最后统一核对
        }

        public override void Send(SendOrPostCallback callback, object? state)
        {
            Violations.Enqueue("SynchronizationContext.Send");
            Post(callback, state);
        }

        public override SynchronizationContext CreateCopy() => this;

        // 列表加锁：PumpDriven 关着时 SDK 会从线程池调进来，违规照记，但不能把列表弄坏。
        public void Register(IPolled item) { Assert("register a polled item"); lock (polled) polled.Add(item); }
        public void Unregister(IPolled item) { Assert("unregister a polled item"); lock (polled) polled.Remove(item); }

        public async Task Until(string what, Func<bool> condition, double seconds = 5)
        {
            var signal = new TaskCompletionSource<bool>();
            waits.Add((condition, DateTime.UtcNow.AddSeconds(seconds), signal));
            if (!await signal.Task) throw new Exception("Timed out waiting for " + what);
        }

        public Task Sleep(double seconds)
        {
            var until = DateTime.UtcNow.AddSeconds(seconds);
            var signal = new TaskCompletionSource<bool>();
            waits.Add((() => DateTime.UtcNow >= until, until.AddSeconds(60), signal));
            return signal.Task;
        }

        public async Task<T> Next<T>(Queue<T> queue, string what, double seconds = 5)
        {
            await Until(what, () => queue.Count > 0, seconds);
            return queue.Dequeue();
        }

        public async Task<Exception?> Catch(Task task)
        {
            try { await task; return null; }
            catch (Exception error) { return error; }
        }

        /// <summary>一轮：执行投进来的续行 → 轮询连接 → 对每个客户端 Pump（像 Unity 每帧先 PollAll 再 Pump）→ 检查等待。</summary>
        private void Turn()
        {
            while (true)
            {
                (SendOrPostCallback callback, object? state) item;
                lock (posted) { if (posted.Count == 0) break; item = posted.Dequeue(); }
                item.callback(item.state);
            }
            IPolled[] items;
            lock (polled) items = polled.ToArray();
            foreach (var item in items) item.Poll();
            foreach (var poll in BeforePump.ToArray()) poll();
            foreach (var client in clients.ToArray()) if (!Paused.Contains(client)) client.Pump();
            var now = DateTime.UtcNow;
            for (int i = waits.Count - 1; i >= 0; i--)
            {
                var (done, deadline, signal) = waits[i];
                if (done()) { waits.RemoveAt(i); signal.TrySetResult(true); }
                else if (now > deadline) { waits.RemoveAt(i); signal.TrySetResult(false); }
            }
        }
    }

    internal interface IPolled { void Poll(); }

    /// <summary>
    /// 像浏览器 WebSocket 桥接（unity-webgl-smoke 的 BrowserGateTransport + jslib）的连接：“浏览器”一侧是线程池上的 ClientWebSocket，
    /// 收到的消息排成字节队列；SDK 看到的任务只在 loop 线程的轮询里完成，SendAsync 交给“浏览器”后同步完成。SDK 在别的线程上调它时记违规。
    /// </summary>
    private sealed class PolledWebSocketTransport : IGateTransport, IPolled
    {
        private readonly StrictLoop loop;
        private readonly ClientWebSocket socket = new ClientWebSocket();
        private readonly ConcurrentQueue<byte[]> chunks = new ConcurrentQueue<byte[]>();
        private readonly object state = new object();
        private byte[]? current;
        private int head;
        private volatile bool ended;
        private bool disposed;
        private Task? opening;
        private Task sending = Task.CompletedTask;
        private TaskCompletionSource<bool>? connecting;
        private TaskCompletionSource<int>? receiving;
        private byte[]? buffer;
        private int offset, count;

        public PolledWebSocketTransport(StrictLoop loop) { this.loop = loop; }

        public Task ConnectAsync(string address, CancellationToken cancellation)
        {
            loop.Assert("transport ConnectAsync");
            lock (state)
            {
                connecting = new TaskCompletionSource<bool>();
                opening = Task.Run(() => socket.ConnectAsync(new Uri(address), CancellationToken.None));
            }
            loop.Register(this);
            return connecting.Task;
        }

        public Task SendAsync(byte[] frame, CancellationToken cancellation)
        {
            loop.Assert("transport SendAsync");
            lock (state)
            {
                if (disposed || ended) throw new IOException("websocket closed.");
                // 浏览器的 send 不阻塞：按序交给后台发送，这里同步完成。
                sending = sending.ContinueWith(_ => socket.SendAsync(frame, WebSocketMessageType.Binary, true, CancellationToken.None),
                    CancellationToken.None, TaskContinuationOptions.None, TaskScheduler.Default).Unwrap()
                    .ContinueWith(task => { if (task.IsFaulted) ended = true; }, TaskScheduler.Default);
            }
            return Task.CompletedTask;
        }

        public Task<int> ReceiveAsync(byte[] buffer, int offset, int count, CancellationToken cancellation)
        {
            loop.Assert("transport ReceiveAsync");
            lock (state)
            {
                if (disposed) return Task.FromResult(0);
                int got = TryReceive(buffer, offset, count);
                if (got != 0) return Task.FromResult(got < 0 ? 0 : got);
                receiving = new TaskCompletionSource<int>();
                this.buffer = buffer; this.offset = offset; this.count = count;
                return receiving.Task;
            }
        }

        public void Poll()
        {
            TaskCompletionSource<bool>? opened = null;
            Exception? openError = null;
            TaskCompletionSource<int>? received = null;
            int got = 0;
            lock (state)
            {
                if (disposed) return;
                if (connecting != null && opening!.IsCompleted)
                {
                    opened = connecting; connecting = null;
                    if (opening.IsFaulted || opening.IsCanceled) openError = new IOException("websocket failed to open.", opening.Exception);
                    else Task.Run(ReceiveLoop);
                }
                if (receiving != null)
                {
                    got = TryReceive(buffer!, offset, count);
                    if (got != 0) { received = receiving; receiving = null; buffer = null; }
                }
            }
            // 续行（SDK 的握手、读循环）就在这里、loop 线程上跑。
            if (opened != null) { if (openError != null) opened.TrySetException(openError); else opened.TrySetResult(true); }
            received?.TrySetResult(got < 0 ? 0 : got);
        }

        /// <summary>“浏览器”一侧：把收到的二进制消息排成字节队列。</summary>
        private async Task ReceiveLoop()
        {
            var chunk = new byte[64 << 10];
            try
            {
                while (true)
                {
                    var result = await socket.ReceiveAsync(chunk, CancellationToken.None);
                    if (result.MessageType != WebSocketMessageType.Binary) break;
                    if (result.Count > 0) chunks.Enqueue(chunk.AsSpan(0, result.Count).ToArray());
                }
            }
            catch (Exception) { /* 连接结束 */ }
            ended = true;
        }

        /// <summary>取到的字节数；0 还没有；-1 连接已结束（队列里的字节先交完）。</summary>
        private int TryReceive(byte[] target, int at, int size)
        {
            int copied = 0;
            while (copied < size)
            {
                if (current == null && !chunks.TryDequeue(out current)) break;
                int take = Math.Min(size - copied, current!.Length - head);
                Buffer.BlockCopy(current, head, target, at + copied, take);
                copied += take; head += take;
                if (head == current.Length) { current = null; head = 0; }
            }
            if (copied > 0) return copied;
            return ended ? -1 : 0;
        }

        public void Dispose()
        {
            loop.Assert("transport Dispose");
            TaskCompletionSource<bool>? opened;
            TaskCompletionSource<int>? received;
            lock (state)
            {
                if (disposed) return;
                disposed = true;
                opened = connecting; connecting = null;
                received = receiving; receiving = null;
            }
            try { socket.Abort(); } catch (Exception) { }
            socket.Dispose();
            loop.Unregister(this);
            opened?.TrySetException(new IOException("websocket closed."));
            received?.TrySetResult(0);
        }
    }
}
