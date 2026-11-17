#nullable enable
using System;
using System.Collections.Generic;
using System.Net.Http;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client.Relay;

namespace Roost.RelaySmoke
{
    /// <summary>冒烟的设置：lobby 地址、传输、是否用浏览器 WebSocket 桥接、在房间里停多久。</summary>
    public sealed class RelaySmokeConfig
    {
        /// <summary>lobby 的根地址；WebGL 页面上用同源代理（serve.mjs 的 /lobby）。</summary>
        public string LobbyUrl = "http://127.0.0.1:8787/lobby";
        public RelayTransport Transport = RelayTransport.Ws;
        /// <summary>true 时用 .jslib 的浏览器 WebSocket（只在 WebGL 播放器里有）；false 时用 SDK 自带的 TCP / ClientWebSocket。</summary>
        public bool BrowserWebSocket;
        /// <summary>连上之后在房间里停的秒数：要大于 relay 的 idle_timeout（缺省 30s），才能看出心跳任务在不在跑。</summary>
        public float HoldSeconds = 40;
        /// <summary>账号名前缀（测试对端的 /harness/game-token 按它签 gt1）。</summary>
        public string Account = "unity-smoke";
    }

    /// <summary>
    /// Unity 上的最小冒烟（交接 C1，维护者 2026-11-17“做一个最小的测试”）：验证 Roost.Client.Relay 在 Unity 的调度下能不能跑，
    /// 重点是 WebGL 单线程下 SDK 用到的 Task.Run、Task.Delay、SemaphoreSlim.WaitAsync(超时)、CancelAfter、ConfigureAwait(false)
    /// 与后台读循环。
    ///
    /// 两段：
    /// 1. 探针：单独起上面每一种调度原语，看它们在规定时间内有没有完成（等待按帧计时，不借助被测的原语）；
    /// 2. 端到端：同一进程里一个房主一个成员，经 lobby 建房 / 加入 → 连 relay → 成员发、房主收，房主广播、成员收，房间通道 →
    ///    停 HoldSeconds 秒（超过 relay 的空闲超时）后两边仍在线且 Rtt 非 0（心跳在跑）→ 离开。
    ///
    /// 由宿主每帧调 Tick（播放器里是 RelaySmokeBehaviour.Update，编辑器批处理里是 EditorApplication.update）；结果写成
    /// “SMOKE ...”开头的日志行，最后一行是 “SMOKE PASS” 或 “SMOKE FAIL ...”。本类自己的等待都按帧检查，不用 Task.Delay。
    /// </summary>
    public sealed class RelaySmoke
    {
        private readonly RelaySmokeConfig config;
        private readonly Action<string> log;
        private readonly Func<double> now;
        private readonly List<(Func<bool> done, double deadline, TaskCompletionSource<bool> signal)> waits =
            new List<(Func<bool>, double, TaskCompletionSource<bool>)>();
        private readonly List<RelayClient> clients = new List<RelayClient>();
        private readonly int mainThread = Thread.CurrentThread.ManagedThreadId;
        private double started;
        private string? step;
        private double stepAt;

        /// <summary>结束时为 true；Passed 是结论。</summary>
        public bool Finished { get; private set; }
        public bool Passed { get; private set; }

        /// <param name="now">单调的秒数（Time.realtimeSinceStartup / EditorApplication.timeSinceStartup）。</param>
        public RelaySmoke(RelaySmokeConfig config, Action<string> log, Func<double> now)
        {
            this.config = config; this.log = log; this.now = now;
        }

        /// <summary>开始（在主线程上调）。返回的任务在结束时完成，不抛异常。</summary>
        public async Task RunAsync()
        {
            try
            {
                started = now();
                Log($"start: lobby {config.LobbyUrl}, transport {config.Transport}, browser websocket {config.BrowserWebSocket}, main thread {mainThread}");
                await ProbesAsync();
                if (config.BrowserWebSocket) await BridgeAsync();
                await EndToEndAsync();
                Passed = true;
                Log("PASS");
            }
            catch (Exception error)
            {
                if (!Finished) Log("FAIL " + error);
            }
            finally
            {
                foreach (var client in clients) client.Dispose();
                Finished = true;
            }
        }

        /// <summary>每帧调一次：先让浏览器桥接收字节，再派发 SDK 的通知，最后检查按帧的等待与卡住的步骤。</summary>
        public void Tick()
        {
            if (Finished) return;
            if (step != null && now() - stepAt > config.HoldSeconds + 20)
            {
                // 某一步一直没往下走（例如续行排进了不会运行的线程池）：报出卡在哪一步，结束。
                Log($"FAIL stuck at step '{step}' for {now() - stepAt:F0}s (at {now() - started:F1}s)");
                Finished = true;
                return;
            }
            if (config.BrowserWebSocket) BrowserRelayTransport.PollAll();
            foreach (var client in clients.ToArray()) client.Pump();
            double t = now();
            for (int i = waits.Count - 1; i >= 0; i--)
            {
                var (done, deadline, signal) = waits[i];
                bool ok;
                try { ok = done(); } catch (Exception error) { waits.RemoveAt(i); signal.TrySetException(error); continue; }
                if (ok) { waits.RemoveAt(i); signal.TrySetResult(true); }
                else if (t > deadline) { waits.RemoveAt(i); signal.TrySetResult(false); }
            }
        }

        // ---------- 1. 调度原语的探针 ----------

        private async Task ProbesAsync()
        {
            Step("probes");
            var results = new Dictionary<string, string>();
            bool reported = false;
            void Done(string name, string detail)
            {
                lock (results)
                {
                    results[name] = detail;
                    if (!reported) return;
                }
                Log($"probe {name}: completed LATE at {now() - started:F1}s, {detail}"); // 3s 之后才完成的也记下
            }
            string Where() => $"thread {Thread.CurrentThread.ManagedThreadId}{(Thread.CurrentThread.ManagedThreadId == mainThread ? " (main)" : "")}";

            // SDK 的读循环与心跳任务都经 Task.Run 起。
            _ = Task.Run(() => Done("Task.Run", "ran on " + Where()));
            // 重连的退避。
            _ = Probe(async () => { await Task.Delay(100).ConfigureAwait(false); Done("Task.Delay(100ms)", "completed on " + Where()); });
            // 心跳任务按间隔醒来：SemaphoreSlim.WaitAsync(超时)。
            _ = Probe(async () =>
            {
                bool signalled = await new SemaphoreSlim(0).WaitAsync(TimeSpan.FromMilliseconds(100)).ConfigureAwait(false);
                Done("SemaphoreSlim.WaitAsync(100ms)", $"timed out (signalled {signalled}) on " + Where());
            });
            // 连接超时：CancellationTokenSource.CancelAfter。
            var cts = new CancellationTokenSource();
            cts.Token.Register(() => Done("CancelAfter(100ms)", "cancelled on " + Where()));
            cts.CancelAfter(100);
            // SDK（与 HttpClient）里 ConfigureAwait(false) 之后的续行在哪里跑：等一个由主线程完成的任务。
            var fromMain = new TaskCompletionSource<bool>();
            _ = Probe(async () => { await fromMain.Task.ConfigureAwait(false); Done("ConfigureAwait(false) after a main-thread completion", "continued on " + Where()); });
            // 对照：不加 ConfigureAwait(false)，续行回到 Unity 的 SynchronizationContext（主线程）。
            _ = Probe(async () => { await fromMain.Task; Done("await (Unity context) after a main-thread completion", "continued on " + Where()); });
            ThreadPool.QueueUserWorkItem(_ => Done("ThreadPool.QueueUserWorkItem", "ran on " + Where()));

            await FramesAsync(2);
            fromMain.TrySetResult(true);
            string[] names =
            {
                "Task.Run", "Task.Delay(100ms)", "SemaphoreSlim.WaitAsync(100ms)", "CancelAfter(100ms)",
                "ConfigureAwait(false) after a main-thread completion", "await (Unity context) after a main-thread completion",
                "ThreadPool.QueueUserWorkItem",
            };
            await UntilAsync(() => { lock (results) return results.Count == names.Length; }, 3);
            int missing = 0;
            foreach (var name in names)
            {
                string? detail;
                lock (results) { results.TryGetValue(name, out detail); reported = true; }
                if (detail == null) missing++;
                Log($"probe {name}: {(detail ?? "DID NOT COMPLETE within 3s")}");
            }
            Log(missing == 0 ? "probes: all completed" : $"probes: {missing} did not complete (the SDK depends on them; see README)");
        }

        private async Task Probe(Func<Task> body)
        {
            try { await body(); }
            catch (Exception error) { Log("probe threw: " + error.Message); }
        }

        // ---------- 2. 只走桥接：不经 SDK 的调度 ----------

        /// <summary>
        /// 只在浏览器 WebSocket 下跑：不经 RelayClient、LobbyClient 与 HttpClient，用 UnityWebRequest 直接向 lobby 要 gt1 与 ws 票据，
        /// 用 BrowserRelayTransport 连 relay、发令牌与 Ping、收 Welcome 与 Pong。等待都不加 ConfigureAwait(false)（续行回主线程），
        /// 读字节也不经 RelayCodec.ReadAsync（它内部用 ConfigureAwait(false)）。用来把“桥接（jslib + DllImport）能不能用”与
        /// “SDK 的调度在单线程下能不能跑”分开。
        /// </summary>
        private async Task BridgeAsync()
        {
            Step("bridge: lobby via UnityWebRequest");
            string gt1 = Field(await RawPostAsync(config.LobbyUrl + "/harness/game-token", "{\"account\":\"" + config.Account + "-bridge\"}", null), "token");
            var reply = await RawPostAsync(config.LobbyUrl + "/rooms", "{\"game\":\"party\",\"mode\":\"bridge\",\"transport\":\"ws\"}", gt1);
            string url = Field(reply, "relay_addr"), token = Field(reply, "token");
            Step("bridge: open " + url);
            var transport = new BrowserRelayTransport();
            await transport.ConnectAsync(url, CancellationToken.None);
            transport.Send(RelayCodec.EncodeToken(token));
            transport.Send(RelayCodec.Encode(new RelayFrame(RelayChannel.Control, 0, RelayOps.Ping, new byte[] { 1, 2, 3 })));
            Step("bridge: read welcome and pong");
            var welcome = await ReadFrameAsync(transport);
            var pong = await ReadFrameAsync(transport);
            transport.Dispose();
            Require(welcome.MessageId == RelayOps.Welcome && RelayCodec.DecodeWelcome(welcome.Slot, welcome.Payload.Span).IsHost, "bridge welcome");
            Require(pong.MessageId == RelayOps.Pong && pong.Payload.Length == 3, "bridge pong");
            Log("bridge: ok (UnityWebRequest lobby → jslib WebSocket → welcome + pong, awaits on the Unity context)");
        }

        private static async Task<RelayFrame> ReadFrameAsync(IRelayTransport transport)
        {
            var header = new byte[RelayCodec.HeaderSize];
            await FillAsync(transport, header);
            int size = RelayCodec.ParseHeader(header, RelayCodec.DefaultMaxPayload);
            var data = new byte[RelayCodec.HeaderSize + size];
            Buffer.BlockCopy(header, 0, data, 0, header.Length);
            var payload = new byte[size];
            await FillAsync(transport, payload);
            Buffer.BlockCopy(payload, 0, data, header.Length, size);
            return RelayCodec.Decode(data);
        }

        private static async Task FillAsync(IRelayTransport transport, byte[] buffer)
        {
            for (int at = 0; at < buffer.Length;)
            {
                int got = await transport.ReceiveAsync(buffer, at, buffer.Length - at, CancellationToken.None);
                if (got == 0) throw new Exception("bridge: connection closed");
                at += got;
            }
        }

        private static Task<string> RawPostAsync(string url, string json, string? bearer)
        {
            var done = new TaskCompletionSource<string>();
            var web = new UnityEngine.Networking.UnityWebRequest(url, "POST")
            {
                uploadHandler = new UnityEngine.Networking.UploadHandlerRaw(Encoding.UTF8.GetBytes(json)),
                downloadHandler = new UnityEngine.Networking.DownloadHandlerBuffer(),
            };
            web.SetRequestHeader("Content-Type", "application/json");
            if (bearer != null) web.SetRequestHeader("Authorization", "Bearer " + bearer);
            web.SendWebRequest().completed += _ =>
            {
                if (web.responseCode == 200) done.TrySetResult(web.downloadHandler.text);
                else done.TrySetException(new Exception($"POST {url}: {web.responseCode} {web.error} {web.downloadHandler.text}"));
                web.Dispose();
            };
            return done.Task;
        }

        private static string Field(string json, string name)
        {
            string key = "\"" + name + "\":\"";
            int at = json.IndexOf(key, StringComparison.Ordinal);
            if (at < 0) throw new Exception($"no {name} in {json}");
            at += key.Length;
            return json.Substring(at, json.IndexOf('"', at) - at);
        }

        // ---------- 3. 端到端：经 SDK ----------

        private async Task EndToEndAsync()
        {
            Step("end to end: lobby (HttpClient + UnityWebRequestHandler)");
            var http = new HttpClient(new UnityWebRequestHandler()) { Timeout = Timeout.InfiniteTimeSpan }; // 不让 HttpClient 起超时计时器
            var host = await PlayerAsync(http, config.Account + "-host");
            var member = await PlayerAsync(http, config.Account + "-m2");

            var ticket = await host.lobby.CreateRoomAsync(new CreateRoomRequest { Game = "party", Mode = "smoke", Transport = config.Transport });
            Log($"lobby created room {ticket.Room.Id}, relay {ticket.RelayAddr}");
            Step("end to end: host connect (RelayClient.ConnectAsync)");
            var hostWelcome = await host.client.ConnectAsync(ticket);
            Require(hostWelcome.IsHost && hostWelcome.Slot == 1, "host welcome " + hostWelcome);
            Log("host connected: " + hostWelcome);

            Step("end to end: member join and connect");
            var joined = await member.lobby.JoinRoomAsync(ticket.Room.Id, new JoinRoomRequest { Game = "party", Transport = config.Transport });
            var memberWelcome = await member.client.ConnectAsync(joined);
            Require(!memberWelcome.IsHost && memberWelcome.Slot == 2, "member welcome " + memberWelcome);
            Log("member connected: " + memberWelcome);
            await WaitAsync(() => host.joined.Count == 1, 5, "host sees the member join (MemberJoined via Pump)");

            Step("end to end: frames");
            const int Frames = 5;
            for (uint i = 0; i < Frames; i++) Require(member.client.SendGame(0, 100 + i, Encoding.UTF8.GetBytes("move" + i)), "member send");
            await WaitAsync(() => host.game.Count == Frames, 5, $"host receives {Frames} game frames");
            for (int i = 0; i < Frames; i++) Require(host.game[i].MessageId == 100 + i && Encoding.UTF8.GetString(host.game[i].Payload.Span) == "move" + i, "frame order");
            Require(host.client.SendGame(RelayCodec.All, 200, Encoding.UTF8.GetBytes("state")), "host broadcast");
            await WaitAsync(() => member.game.Count == 1, 5, "member receives the host broadcast");
            Require(member.client.SendRoom(RelayCodec.All, 7, Encoding.UTF8.GetBytes("hi")), "room send");
            await WaitAsync(() => host.room.Count == 1, 5, "host receives the room frame");
            Log("frames: ok (member → host 5, host → member 1, room 1)");

            // 停一段比 relay 空闲超时（30s）更久：心跳任务（Task.Run + SemaphoreSlim.WaitAsync 超时）不跑的话这里会被断开。
            double until = now() + config.HoldSeconds;
            Step("end to end: hold");
            Log($"holding {config.HoldSeconds}s to see heartbeats (from {now() - started:F1}s) ...");
            await WaitAsync(() => now() >= until || host.disconnects.Count > 0 || member.disconnects.Count > 0, config.HoldSeconds + 5, "hold");
            Require(host.disconnects.Count == 0 && member.disconnects.Count == 0,
                $"no disconnect while holding (host {string.Join(",", host.disconnects)}; member {string.Join(",", member.disconnects)})");
            Require(host.client.Rtt > TimeSpan.Zero && member.client.Rtt > TimeSpan.Zero, $"heartbeats measured an rtt (host {host.client.Rtt}, member {member.client.Rtt})");
            Log($"held {config.HoldSeconds}s: still connected, rtt host {host.client.Rtt.TotalMilliseconds:F1}ms member {member.client.Rtt.TotalMilliseconds:F1}ms");

            Step("end to end: leave");
            await member.client.LeaveAsync();
            await WaitAsync(() => host.left.Count == 1, 5, "host sees the member leave");
            // 房间里最后一个人离开：LeaveAsync 先在 relay 上发 Leave，relay 随即关房，之后经 lobby 离开可能答 room_not_found
            // （Go / C# SDK 都是这个顺序，2026-11-17 编辑器冒烟里看到；不在 C1 范围，记为观察）。这里只记下，不算失败。
            try { await host.client.LeaveAsync(); }
            catch (LobbyException error) when (error.Code == LobbyErrorCode.RoomNotFound) { Log("last leave: lobby answered room_not_found (relay closed the room first)"); }
            Log("left: ok");
        }

        private sealed class SmokePlayer
        {
            public LobbyClient lobby = null!;
            public RelayClient client = null!;
            public readonly List<RelayFrame> game = new List<RelayFrame>(), room = new List<RelayFrame>();
            public readonly List<RelayMember> joined = new List<RelayMember>(), left = new List<RelayMember>();
            public readonly List<RelayDisconnect> disconnects = new List<RelayDisconnect>();
        }

        private async Task<SmokePlayer> PlayerAsync(HttpClient http, string account)
        {
            // 测试对端签 gt1（client/relay/peer_test.go 的 /harness/game-token）；正式游戏从账号服务拿。
            var reply = await http.PostAsync(config.LobbyUrl + "/harness/game-token", new StringContent("{\"account\":\"" + account + "\"}", Encoding.UTF8, "application/json"));
            var text = await reply.Content.ReadAsStringAsync();
            if (!reply.IsSuccessStatusCode) throw new Exception($"harness game token: HTTP {(int)reply.StatusCode} {text}");
            string token = Field(text, "token");

            var player = new SmokePlayer { lobby = new LobbyClient(config.LobbyUrl, token, http) };
            var options = new RelayClientOptions { Game = "party", Transport = config.Transport };
            if (config.BrowserWebSocket) options.TransportFactory = () => new BrowserRelayTransport();
            player.client = new RelayClient(player.lobby, options);
            player.client.GameFrame += player.game.Add;   // 回调都在 Pump 里、主线程上
            player.client.RoomFrame += player.room.Add;
            player.client.MemberJoined += player.joined.Add;
            player.client.MemberLeft += m => { player.left.Add(m); Log($"{account} saw slot {m.Slot} ({m.Account}) leave at {now() - started:F1}s: {m.Reason}"); };
            player.client.Disconnected += d => { player.disconnects.Add(d); Log($"{account} disconnected at {now() - started:F1}s: {d}"); };
            player.client.Error += error => Log("callback error: " + error.Message);
            clients.Add(player.client);
            return player;
        }

        // ---------- 按帧的等待 ----------

        private Task<bool> UntilAsync(Func<bool> done, double seconds)
        {
            var signal = new TaskCompletionSource<bool>();
            waits.Add((done, now() + seconds, signal));
            return signal.Task;
        }

        private async Task WaitAsync(Func<bool> done, double seconds, string what)
        {
            if (!await UntilAsync(done, seconds)) throw new Exception($"timed out after {seconds}s: {what}");
        }

        private async Task FramesAsync(int frames)
        {
            int left = frames;
            await UntilAsync(() => --left <= 0, 60);
        }

        private static void Require(bool condition, string what)
        {
            if (!condition) throw new Exception("check failed: " + what);
        }

        private void Log(string line) => log("SMOKE " + line);

        /// <summary>记下当前步骤（卡住时 Tick 报出它）。</summary>
        private void Step(string name)
        {
            step = name;
            stepAt = now();
        }
    }
}
