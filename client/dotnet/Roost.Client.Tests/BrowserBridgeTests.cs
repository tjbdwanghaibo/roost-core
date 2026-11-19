using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client;
using Roost.Client.Relay;
using Roost.Unity;

// 浏览器桥接的 C# 部分（Unity 包 client/unity/Runtime 的 BrowserWebSocket，正式源码链接进来）在 PumpDriven 单线程环境里拼帧：
// 假的浏览器一侧（FakeBrowserQueue，按 jslib 的做法把收到的消息排成字节队列、按调用方给的长度取）喂消息。
// 连玩家接入（GameClient）：一帧拆成多条消息、多帧合成一条消息、接近 max_payload 的大帧——应答与推送完整、顺序不乱；
// 开房间（RelayClient，同一个类当 IRelayTransport）：Welcome 拆成三条消息、两帧游戏帧合成一条、Ping / Pong 测出 Rtt。
// 两边都断言每次 Send 恰好一帧、全程零违规。jslib 本身的 JS 逻辑由 check-jslib-frames.mjs 在 node 里核对。
internal static partial class Program
{
    private static async Task TestBrowserBridgeFraming()
    {
        var fake = new FakeBrowserSocket();
        await StrictLoop.RunAsync("browser-bridge", TimeSpan.FromSeconds(30), async loop =>
        {
            loop.BeforePump.Add(BrowserWebSocket.PollAll);
            var client = loop.Client(new ConnectionOptions
            {
                WebSocketUrl = "ws://browser.test/ws", PumpDriven = true, HeartbeatInterval = TimeSpan.FromMilliseconds(100),
                TransportFactory = () => new BrowserWebSocket(fake),
            });
            var pushes = new List<string>();
            var replies = new Queue<byte[]>();
            var failures = new Queue<Exception>();
            client.Subscribe(9, bytes => Utf8.GetString(bytes), pushes.Add);
            async Task<byte[]> Call(string text)
            {
                client.Request(42, 43, Utf8.GetBytes(text), bytes => bytes, replies.Enqueue, failures.Enqueue);
                await loop.Until("reply to " + text, () => replies.Count > 0 || failures.Count > 0);
                if (failures.Count > 0) throw new Exception($"request '{text}' failed", failures.Dequeue());
                return replies.Dequeue();
            }

            await client.Connection.ConnectAsync("bridge"); // 握手应答拆成三条消息
            Check(Utf8.GetString(await Call("split")) == FakeBrowserSocket.SplitReply, "a frame split into 7-byte messages arrives whole");
            Check(Utf8.GetString(await Call("merged")) == "merged-reply", "the first of three frames merged in one message");
            await loop.Until("both merged pushes", () => pushes.Count >= 2);
            Check(pushes.SequenceEqual(new[] { "m1", "m2" }), "merged pushes arrive whole and in order: " + string.Join(",", pushes));
            var big = await Call("big");
            Check(big.AsSpan().SequenceEqual(FakeBrowserSocket.BigPayload), $"a {big.Length}-byte frame near max_payload arrives whole");
            Check(Utf8.GetString(await Call("after")) == "after", "frames after the big one stay aligned");
            await loop.Until("a heartbeat round trip", () => client.Connection.HeartbeatRoundTrips > 0);
            Check(fake.Errors.Count == 0, "every Send was exactly one frame: " + string.Join(" | ", fake.Errors));
            Check(loop.Violations.IsEmpty, "everything ran on the loop thread: " + string.Join(" | ", loop.Violations));
        }, expectViolations: false);
        Console.WriteLine("PASS browser bridge for the gate (BrowserWebSocket + fake jslib, PumpDriven): split / merged / near-max frames, one frame per Send");
    }

    private static async Task TestBrowserBridgeRelay()
    {
        var fake = new FakeRelaySocket();
        await StrictLoop.RunAsync("browser-bridge-relay", TimeSpan.FromSeconds(30), async loop =>
        {
            // lobby 不会被调到（只连 relay、不重连、不离开）；PumpDriven 的 RelayClient 要求 LobbyClient 也是 pumpDriven。
            var lobby = new LobbyClient("http://lobby.test", "gt2.test", new UnusedLobbyHttp(), pumpDriven: true);
            var relay = new RelayClient(lobby, new RelayClientOptions
            {
                Game = "party", Transport = RelayTransport.Ws, PumpDriven = true, PingInterval = TimeSpan.FromMilliseconds(100),
                TransportFactory = () => new BrowserWebSocket(fake),
            });
            var frames = new List<string>();
            relay.GameFrame += frame => frames.Add($"{frame.Slot}:{frame.MessageId}:{Utf8.GetString(frame.Payload.Span)}");
            relay.Error += error => loop.Violations.Enqueue("relay callback threw: " + error.Message);
            loop.BeforePump.Add(BrowserWebSocket.PollAll); // 像 Unity 每帧：RoostPlatform.Poll(); relay.Pump();
            loop.BeforePump.Add(() => relay.Pump());
            try
            {
                var ticket = new RoomTicket(new RoomInfo(), "ws://relay.test/ws", "rt1.test", 0);
                var welcome = await relay.ConnectAsync(ticket); // Welcome 拆成三条消息
                Check(welcome.Slot == 2 && !welcome.IsHost && welcome.HostSlot == 1, "a welcome split into three messages arrives whole: " + welcome);
                Check(relay.SendGame(1, 100, Utf8.GetBytes("move")), "the member sends a game frame");
                await loop.Until("both echoed game frames", () => frames.Count >= 2);
                Check(frames.SequenceEqual(new[] { "1:100:move", "1:101:move-again" }), "two frames merged in one message arrive whole and in order: " + string.Join(",", frames));
                await loop.Until("a ping / pong round trip", () => relay.Rtt > TimeSpan.Zero);
                Check(fake.Errors.Count == 0, "every Send was exactly one frame: " + string.Join(" | ", fake.Errors));
                Check(loop.Violations.IsEmpty, "everything ran on the loop thread: " + string.Join(" | ", loop.Violations));
            }
            finally { relay.Dispose(); }
        }, expectViolations: false);
        Console.WriteLine("PASS browser bridge for the relay (BrowserWebSocket + fake jslib, PumpDriven): split welcome, merged game frames, ping / pong");
    }

    // 取消与连接句柄：浏览器连接由 JS Open 打开、Close 关闭，桥接要保证每个打开的句柄最终都关掉、连接任务一定结束。
    // SDK 在连接超时 / 取消时 Dispose 这个连接（token.Register(transport.Dispose)），令牌在调用前就已取消时 Dispose 会先于
    // ConnectAsync 执行——这时不能再打开浏览器连接（打开了也没人关、Poll 不再推进，连接任务挂起）。
    // 场景：relay / Gate（GameClient、公开的 TcpSession.ConnectAsync）各自的“调用前已取消”与“连接中取消”，以及桥接自身
    // 在没人 Dispose 时也按令牌结束。每个场景都断言连接任务以取消结束、Open 与 Close 次数相等。
    private static async Task TestBrowserBridgeCancellation()
    {
        static void Balanced(CountingBrowserSocket js, int opens, string what) =>
            Check(js.Opens == opens && js.Closes == js.Opens, $"{what}: every opened browser socket is closed (open={js.Opens} close={js.Closes}, want open={opens})");
        static void Canceled(Task task, string what) =>
            Check(task.IsCanceled, $"{what}: the connect task ends canceled (status={task.Status}, error={task.Exception?.GetBaseException().Message})");

        static async Task Ends(StrictLoop loop, Task task, CountingBrowserSocket js)
        {
            try { await loop.Until("the connect task to end", () => task.IsCompleted, 1); }
            catch (Exception) { throw new Exception($"the connect task did not end within 1s of pumping (status={task.Status}, open={js.Opens} close={js.Closes})"); }
        }
        // 每个场景单独记结果，一次列出全部失败（修前的红要能看到每个场景各自的状态）。
        var failures = new List<string>();
        async Task Scenario(string name, Func<Task> run)
        {
            try { await run(); }
            catch (Exception error) { failures.Add(name + ": " + error.Message); }
        }

        await StrictLoop.RunAsync("browser-bridge-cancel", TimeSpan.FromSeconds(60), async loop =>
        {
            loop.BeforePump.Add(BrowserWebSocket.PollAll);
            foreach (bool before in new[] { true, false })
            {
                string when = before ? "relay, token canceled before ConnectAsync" : "relay, canceled while connecting";
                await Scenario(when, async () =>
                {
                    var js = new CountingBrowserSocket();
                    var relay = new RelayClient(null, new RelayClientOptions
                    {
                        Game = "party", Transport = RelayTransport.Ws, PumpDriven = true, ConnectTimeout = TimeSpan.FromSeconds(10),
                        TransportFactory = () => new BrowserWebSocket(js),
                    });
                    Action pump = () => relay.Pump();
                    loop.BeforePump.Add(pump);
                    try
                    {
                        using var cancel = new CancellationTokenSource();
                        if (before) cancel.Cancel();
                        var task = relay.ConnectAsync(new RoomTicket(new RoomInfo(), "ws://relay.test/ws", "rt1.test", 0), cancel.Token);
                        if (!before) { await loop.Sleep(0.05); cancel.Cancel(); }
                        await Ends(loop, task, js);
                        Canceled(task, when);
                        Balanced(js, before ? 0 : 1, when);
                    }
                    finally { loop.BeforePump.Remove(pump); relay.Dispose(); }
                });
            }

            foreach (bool before in new[] { true, false })
            {
                string when = before ? "gate, token canceled before ConnectAsync" : "gate, canceled while connecting";
                await Scenario(when, async () =>
                {
                    var js = new CountingBrowserSocket();
                    var client = loop.Client(new ConnectionOptions
                    {
                        WebSocketUrl = "ws://browser.test/ws", PumpDriven = true, ConnectTimeout = TimeSpan.FromSeconds(10),
                        TransportFactory = () => new BrowserWebSocket(js),
                    });
                    try
                    {
                        using var cancel = new CancellationTokenSource();
                        if (before) cancel.Cancel();
                        var task = client.Connection.ConnectAsync("ticket", cancel.Token);
                        if (!before) { await loop.Sleep(0.05); cancel.Cancel(); }
                        await Ends(loop, task, js);
                        Canceled(task, when);
                        Balanced(js, before ? 0 : 1, when);
                    }
                    finally { loop.Remove(client); }
                });
            }

            // 桥接自身：调用方没有注册 Dispose，令牌取消后下一次 PollAll 就以取消结束并关掉句柄；之后的 Dispose 不再重复关。
            await Scenario("bridge alone, canceled while connecting", async () =>
            {
                var js = new CountingBrowserSocket();
                var socket = new BrowserWebSocket(js);
                try
                {
                    using var cancel = new CancellationTokenSource();
                    var task = socket.ConnectAsync("ws://browser.test/ws", cancel.Token);
                    await loop.Sleep(0.05);
                    cancel.Cancel();
                    await Ends(loop, task, js);
                    Canceled(task, "bridge alone");
                    Balanced(js, 1, "bridge alone");
                    socket.Dispose();
                    Balanced(js, 1, "bridge alone, Dispose after the canceled connect");
                }
                finally { socket.Dispose(); }
            });
        }, expectViolations: false);

        // 公开的 TcpSession.ConnectAsync（线程池调度，不经 Connection 的取票前检查）：调用前已取消也不能打开浏览器连接。
        await Scenario("TcpSession, token canceled before ConnectAsync", async () =>
        {
            var js = new CountingBrowserSocket();
            using var cancel = new CancellationTokenSource();
            cancel.Cancel();
            var task = TcpSession.ConnectAsync(new BrowserWebSocket(js), "ws://browser.test/ws", "ticket", TimeSpan.FromSeconds(10), cancel.Token);
            for (int i = 0; i < 100 && !task.IsCompleted; i++) { BrowserWebSocket.PollAll(); await Task.Delay(10); }
            Check(task.IsCompleted, $"the connect task ends (status={task.Status}, open={js.Opens} close={js.Closes})");
            Canceled(task, "TcpSession");
            Balanced(js, 0, "TcpSession");
        });
        Check(failures.Count == 0, "browser bridge cancellation:\n  " + string.Join("\n  ", failures));
        Console.WriteLine("PASS browser bridge cancellation (relay / gate / TcpSession / bridge alone): canceled connects end canceled, every Open has a Close");
    }

    /// <summary>只数 Open / Close 的浏览器一侧：连接一直停在“连接中”，只能靠取消或 Dispose 结束。</summary>
    private sealed class CountingBrowserSocket : BrowserWebSocket.IBrowserSocket
    {
        public int Opens, Closes;
        public int Open(string url) => ++Opens;
        public int State(int id) => BrowserWebSocket.Connecting;
        public void Close(int id) => Closes++;
        public int Buffered(int id) => 0;
        public int Send(int id, byte[] data, int length) => 0;
        public int Receive(int id, byte[] buffer, int length) => 0;
    }

    /// <summary>
    /// 假的浏览器一侧：jslib 的 RoostWs* 同样的语义（Open 后过两轮才连上；收到的消息排成字节队列，按调用方给的长度取，可以跨消息）。
    /// 子类在 OnSend 里拿到一次 Send 的字节，用 Reply 排回要“收到”的消息。
    /// </summary>
    private abstract class FakeBrowserQueue : BrowserWebSocket.IBrowserSocket
    {
        public readonly List<string> Errors = new List<string>();
        private readonly Queue<byte[]> chunks = new Queue<byte[]>();
        private int head, state = BrowserWebSocket.Closed, polls;

        public int Open(string url) { state = BrowserWebSocket.Connecting; polls = 0; chunks.Clear(); head = 0; OnOpen(); return 1; }
        public int State(int id)
        {
            if (state == BrowserWebSocket.Connecting && ++polls > 2) state = BrowserWebSocket.Open; // 过两轮才连上
            return state;
        }
        public int Buffered(int id) => 0;
        public void Close(int id) => state = BrowserWebSocket.Closed;

        public int Send(int id, byte[] data, int length)
        {
            if (state != BrowserWebSocket.Open) return 0;
            OnSend(data.AsSpan(0, length).ToArray());
            return 1;
        }

        // 与 jslib 的 RoostWsReceive 相同：从队首消息接着上次的位置拷，至多 length 个，可以跨消息。
        public int Receive(int id, byte[] buffer, int length)
        {
            int copied = 0;
            while (copied < length && chunks.Count > 0)
            {
                var chunk = chunks.Peek();
                int take = Math.Min(length - copied, chunk.Length - head);
                Buffer.BlockCopy(chunk, head, buffer, copied, take);
                copied += take; head += take;
                if (head == chunk.Length) { chunks.Dequeue(); head = 0; }
            }
            return copied;
        }

        protected void Reply(byte[] message) => chunks.Enqueue(message);
        protected virtual void OnOpen() { }
        protected abstract void OnSend(byte[] frame);
    }

    /// <summary>连玩家接入：按收到的帧回预先编排好的消息。</summary>
    private sealed class FakeBrowserSocket : FakeBrowserQueue
    {
        internal static readonly string SplitReply = string.Concat(Enumerable.Range(0, 300).Select(i => (char)('a' + i % 26)));
        // 缺省 MaxPayloadBytes 是 1 MiB：差 3 字节到上限，跨过桥接 64 KiB 的中转缓冲很多次。
        internal static readonly byte[] BigPayload = Enumerable.Range(0, PacketCodec.DefaultMaxPayload - 3).Select(i => (byte)(i * 31 + 7)).ToArray();
        private bool authed;

        protected override void OnOpen() => authed = false;

        protected override void OnSend(byte[] data)
        {
            Packet packet;
            try { packet = PacketCodec.Decode(data); }
            catch (InvalidDataException error) { Errors.Add($"a {data.Length}-byte send is not exactly one frame: {error.Message}"); return; }
            byte[] Frame(uint message, byte[] payload, bool push = false) => PacketCodec.Encode(new Packet(message, push ? 1u : packet.Sequence, payload, isPush: push));
            if (packet.MessageId == 0)
            {
                byte[] ack = Frame(0, Array.Empty<byte>());
                if (!authed) { authed = true; Reply(ack[..5]); Reply(ack[5..6]); Reply(ack[6..]); }
                else Reply(ack);
                return;
            }
            switch (Utf8.GetString(packet.Payload.Span))
            {
                case "split":
                    var reply = Frame(43, Utf8.GetBytes(SplitReply));
                    for (int at = 0; at < reply.Length; at += 7) Reply(reply[at..Math.Min(reply.Length, at + 7)]);
                    break;
                case "merged":
                    Reply(Frame(43, Utf8.GetBytes("merged-reply")).Concat(Frame(9, Utf8.GetBytes("m1"), true)).Concat(Frame(9, Utf8.GetBytes("m2"), true)).ToArray());
                    break;
                case "big":
                    Reply(Frame(43, BigPayload));
                    break;
                default:
                    Reply(Frame(43, packet.Payload.ToArray()));
                    break;
            }
        }
    }

    /// <summary>开房间：令牌首帧回 Welcome（拆成三条消息），游戏帧回两帧合成的一条消息，Ping 回 Pong。</summary>
    private sealed class FakeRelaySocket : FakeBrowserQueue
    {
        protected override void OnSend(byte[] data)
        {
            RelayFrame frame;
            try { frame = RelayCodec.Decode(data, int.MaxValue); }
            catch (InvalidDataException error) { Errors.Add($"a {data.Length}-byte send is not exactly one frame: {error.Message}"); return; }
            if (frame.Channel == RelayChannel.None)
            {
                var welcome = RelayCodec.Encode(new RelayFrame(RelayChannel.Control, 2, RelayOps.Welcome, RelayCodec.EncodeWelcome(new RelayWelcome(2, false, 1))));
                Reply(welcome[..5]); Reply(welcome[5..17]); Reply(welcome[17..]);
            }
            else if (frame.Channel == RelayChannel.Control && frame.MessageId == RelayOps.Ping)
                Reply(RelayCodec.Encode(new RelayFrame(RelayChannel.Control, 0, RelayOps.Pong, frame.Payload)));
            else if (frame.Channel == RelayChannel.Game)
                Reply(RelayCodec.Encode(new RelayFrame(RelayChannel.Game, 1, frame.MessageId, frame.Payload))
                    .Concat(RelayCodec.Encode(new RelayFrame(RelayChannel.Game, 1, frame.MessageId + 1, Utf8.GetBytes("move-again")))).ToArray());
        }
    }

    private sealed class UnusedLobbyHttp : ILobbyHttp
    {
        public Task<LobbyHttpResponse> SendAsync(string method, string url, string bearer, string? jsonBody, CancellationToken cancellation) =>
            throw new InvalidOperationException($"the bridge test must not call the lobby ({method} {url})");
    }
}
