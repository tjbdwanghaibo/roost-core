using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.Net;
using System.Net.Http;
using System.Net.Http.Headers;
using System.Net.Sockets;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client.Relay;

// 单线程调度（WebGL A1，维护者 2026-11-18）：在一个“像 Unity WebGL 主线程”的环境里跑 RelayClientOptions.PumpDriven 的关键路径。
//
// 环境（StrictLoop）：一个专用线程 + 只在这个线程上执行的 SynchronizationContext；每一轮先执行投进来的续行，再轮询连接与 HTTP，
// 再对每个客户端调 Pump，像 Unity 每帧先 BrowserRelayTransport.PollAll() 再 Pump。WebGL 上线程池、计时器不运行，这里没法让它们
// 不运行，改成“用了就记一笔”：
//   - 往这个上下文投续行的必须是这个线程自己（Task.Delay、CancelAfter、IO 线程完成的任务把续行投回来时，调用者是别的线程）；
//   - SDK 调传输（ConnectAsync / Send / ReceiveAsync / Dispose）与 lobby HTTP 的线程必须是这个线程（Task.Run 起的读循环、心跳，
//     ConfigureAwait(false) 之后排进线程池的续行，都会在别的线程上调到它们）。
// 传输（PolledTcpTransport）与 HTTP（PolledLobbyHttp）只在这个线程上完成任务，像浏览器 WebSocket 桥接与 UnityWebRequest：
// 字节由轮询交出，HTTP 由线程池上的 HttpClient 发（那是“浏览器”），完成后在轮询里交回。
//
// 两组用例：
//   - 探测器自检：PumpDriven 关着时（现在桌面上的调度）建房、连上，必须记到违规——证明探测器抓得到线程池与计时器；
//   - PumpDriven 打开时的关键子集：建房、加入、收发、心跳撑过 relay 的 idle_timeout（对端改成 2s）、自动重连、不 Pump 就被
//     relay 断开而恢复 Pump 后重连回来、换 host（转交）、连接超时、离开（RoomGone 标志），全程零违规。
// ROOST_RELAY_SINGLE_THREAD_OFF=1 时第二组也关着 PumpDriven 跑（修前的红：第一步就记到违规）。
internal static partial class Program
{
    /// <summary>单线程用例的对端把 relay 的 idle_timeout 改成这么短（毫秒）。</summary>
    private const int SingleThreadIdleMs = 2000;

    private static async Task SingleThreadDetector(Peer peer)
    {
        // PumpDriven 的 RelayClient 要求 LobbyClient 也以 pumpDriven 构造（否则重连时的 lobby 续行排进线程池）。
        await Throws<ArgumentException>(() =>
        {
            _ = new RelayClient(new LobbyClient(peer.LobbyUrl, "gt2.unused"), new RelayClientOptions { Game = Game, PumpDriven = true });
            return Task.CompletedTask;
        });
        var violations = await StrictLoop.RunAsync("detector", TimeSpan.FromSeconds(20), async loop =>
        {
            var host = await StPlayer.CreateAsync(loop, peer, "st-detector-host", new RelayClientOptions { Game = Game, PumpDriven = false });
            var ticket = await host.Lobby.CreateRoomAsync(new CreateRoomRequest { Game = Game, Mode = "st-detector" });
            // 关着开关时 SDK 从线程池并发调进测试连接，连接本身可能出错；这里只要违规记录，不看连接结果。
            await loop.Catch(host.Client.ConnectAsync(ticket));
            await loop.Sleep(0.5);
            host.Dispose();
        }, expectViolations: true);
        Check(violations.Count > 0, "the detector catches the threaded scheduling (PumpDriven off)");
        Console.WriteLine("  detector caught, e.g.: " + violations[0]);
    }

    private static async Task SingleThreadPumpDriven(Peer peer)
    {
        bool pumpDriven = Environment.GetEnvironmentVariable("ROOST_RELAY_SINGLE_THREAD_OFF") != "1";
        await StrictLoop.RunAsync("pump-driven", TimeSpan.FromSeconds(90), async loop =>
        {
            RelayClientOptions Options(Action<RelayClientOptions>? edit = null)
            {
                var options = new RelayClientOptions { Game = Game, PumpDriven = pumpDriven, PingInterval = TimeSpan.FromMilliseconds(SingleThreadIdleMs / 3) };
                edit?.Invoke(options);
                return options;
            }
            void Clean(string step)
            {
                Check(loop.Violations.IsEmpty, $"{step}: everything ran on the loop thread; violations: {string.Join(" | ", loop.Violations)}");
            }

            // 建房、加入、收发。
            var host = await StPlayer.CreateAsync(loop, peer, "st-host", Options());
            var ticket = await host.Lobby.CreateRoomAsync(new CreateRoomRequest { Game = Game, Mode = "st" });
            var hostWelcome = await host.Client.ConnectAsync(ticket);
            Check(hostWelcome.IsHost && hostWelcome.Slot == 1, "single-thread host welcome");
            Clean("create and connect");
            var member = await StPlayer.CreateAsync(loop, peer, "st-m2", Options(o =>
            {
                o.ReconnectAttempts = 5; o.ReconnectMinBackoff = TimeSpan.FromMilliseconds(20); o.ReconnectMaxBackoff = TimeSpan.FromMilliseconds(100);
            }));
            var joined = await member.Lobby.JoinRoomAsync(ticket.Room.Id, new JoinRoomRequest { Game = Game });
            var memberWelcome = await member.Client.ConnectAsync(joined);
            Check(!memberWelcome.IsHost && memberWelcome.Slot == 2, "single-thread member welcome");
            Check((await loop.Next(host.Joined, "member joined")).Slot == 2, "host saw the member join");
            Check(member.Client.SendGame(0, 101, Encoding.UTF8.GetBytes("move")), "member send");
            Frame(await loop.Next(host.Game, "member game frame"), RelayChannel.Game, 2, 101, "move");
            Check(host.Client.SendGame(RelayCodec.All, 202, Encoding.UTF8.GetBytes("state")), "host broadcast");
            Frame(await loop.Next(member.Game, "host broadcast"), RelayChannel.Game, 1, 202, "state");
            Check(member.Client.SendRoom(RelayCodec.All, 7, Encoding.UTF8.GetBytes("hi")), "room send");
            Frame(await loop.Next(host.Room, "room frame"), RelayChannel.Room, 2, 7, "hi");
            Clean("join and frames");

            // 心跳：停 2.5 个 relay idle_timeout（房主还受 host_idle_timeout 2s 约束），只靠 Pump 里的心跳，两边都不断线、量到 Rtt。
            await loop.Sleep(SingleThreadIdleMs * 2.5 / 1000);
            Check(host.Disconnected.Count == 0 && member.Disconnected.Count == 0, "no disconnect while holding past the relay idle timeout");
            Check(host.Client.Rtt > TimeSpan.Zero && member.Client.Rtt > TimeSpan.Zero, "heartbeats on Pump measured an rtt");
            Clean("heartbeat on Pump");

            // 自动重连：断网（本地关掉连接），退避由 Pump 按时钟完成，再经 lobby 要令牌连回来。
            int joins = member.Http.Joins;
            member.Client.AbortTransportForTest();
            Check((await loop.Next(member.Disconnected, "transport lost")).Reconnecting, "transport loss → reconnecting");
            var back = await loop.Next(member.Reconnected, "reconnected");
            Check(back.Slot == 2 && member.Http.Joins - joins == 1, "reconnected to slot 2 with one lobby join");
            await loop.Until("host sees the member again", () => { while (host.Joined.Count > 0) if (host.Joined.Dequeue().Slot == 2) return true; return false; });
            Check(member.Client.SendGame(0, 102, Encoding.UTF8.GetBytes("back")), "send after reconnect");
            Frame(await loop.Next(host.Game, "frame after reconnect"), RelayChannel.Game, 2, 102, "back");
            Clean("reconnect");

            // 不 Pump：成员这边停掉 Pump（像加载场景时 Unity 不调 Pump），不发心跳，过了 relay 的 idle_timeout 被 relay 断开；
            // 停着的时候不派发任何通知；恢复 Pump 后先收到 Disconnected{IdleTimeout, 重连中}，再按退避重连回来。
            host.Left.Clear(); // 断网那一次的 MemberLeft{Disconnected}
            loop.Paused.Add(member.Client);
            var left = await loop.Next(host.Left, "member idle-timed out at the relay", (SingleThreadIdleMs + 3000) / 1000.0);
            Check(left.Slot == 2, "host saw the unpumped member leave: " + left.Reason);
            await loop.Sleep(0.3);
            Check(member.Disconnected.Count == 0 && member.Reconnected.Count == 0, "no notifications without Pump");
            loop.Paused.Remove(member.Client);
            var idle = await loop.Next(member.Disconnected, "disconnect after Pump resumed");
            Check(idle.Reason == RelayReason.IdleTimeout && idle.Reconnecting, "relay idle timeout → reconnecting: " + idle);
            Check((await loop.Next(member.Reconnected, "reconnected after Pump resumed")).Slot == 2, "back in slot 2 after Pump resumed");
            Clean("no Pump → idle timeout → reconnect");

            // 换 host：房主转交给成员；成员回 HostReady 后两边收到 HostChanged，角色随之更新。
            var handoff = Encoding.UTF8.GetBytes("handoff on one thread");
            Check(host.Client.Transfer(2, handoff) == RelayTransferResult.Sent, "transfer");
            var offer = await loop.Next(member.YouAreHost, "you are host");
            Check(offer.Source == RelayStateSource.Handoff && offer.State != null && Encoding.UTF8.GetString(offer.State) == "handoff on one thread", "handoff reached the new host");
            Check(member.Client.HostReady(offer.Epoch), "host ready");
            Check((await loop.Next(member.HostChanged, "host changed at the new host")).HostSlot == 2, "new host is slot 2");
            Check((await loop.Next(host.HostChanged, "host changed at the old host")).HostSlot == 2, "old host saw slot 2 take over");
            Check(member.Client.Role.IsHost && !host.Client.Role.IsHost, "roles after the transfer");
            // 新房主（HostPingInterval 600ms）再停一个 host_idle_timeout 以上，仍是房主。
            await loop.Sleep(2.5);
            Check(member.Disconnected.Count == 0 && member.Client.Role.IsHost, "new host kept alive by heartbeats on Pump");
            Clean("host transfer");

            // 连接超时：对端接受连接却不发 Welcome，超时由 Pump 按时钟取消。
            var silent = new TcpListener(IPAddress.Loopback, 0);
            silent.Start();
            try
            {
                var addr = "127.0.0.1:" + ((IPEndPoint)silent.LocalEndpoint).Port;
                using var waiting = new RelayClient(null, Options(o => o.ConnectTimeout = TimeSpan.FromMilliseconds(300)).WithTransport(loop));
                loop.Pumped.Add(waiting);
                var started = DateTime.UtcNow;
                var error = await loop.Catch(waiting.ConnectAsync(new RoomTicket(ticket.Room, addr, "rt1.silent", 0)));
                Check(error is TimeoutException, "connect timeout fired by Pump: " + error?.GetType().Name);
                // 超时之后的续行（这里）不在 Pump 里跑：到期的计时投到上下文再触发，所以这里能调 Pump（Pump 不可重入）。
                waiting.Pump();
                Check(DateTime.UtcNow - started < TimeSpan.FromSeconds(5), "connect timeout fired in time");
                loop.Pumped.Remove(waiting);
            }
            finally { silent.Stop(); }
            Clean("connect timeout");

            // 离开：旧房主（现在是成员）正常离开，RoomGone 为 false；最后一人离开不抛。
            Check(!(await host.Client.LeaveAsync()).RoomGone, "a normal leave: the room is still there");
            await member.Client.LeaveAsync();
            Clean("leave");
            host.Dispose();
            member.Dispose();
        }, expectViolations: false);
    }

    private static RelayClientOptions WithTransport(this RelayClientOptions options, StrictLoop loop)
    {
        options.Transport = RelayTransport.Tcp;
        options.TransportFactory = () => new PolledTcpTransport(loop);
        return options;
    }

    /// <summary>单线程环境里的一个玩家：事件都在 Pump 里、loop 线程上收进普通队列。</summary>
    private sealed class StPlayer : IDisposable
    {
        public readonly LobbyClient Lobby;
        public readonly RelayClient Client;
        public readonly PolledLobbyHttp Http;
        private readonly StrictLoop loop;
        public readonly Queue<RelayFrame> Game = new Queue<RelayFrame>(), Room = new Queue<RelayFrame>();
        public readonly Queue<RelayMember> Joined = new Queue<RelayMember>(), Left = new Queue<RelayMember>();
        public readonly Queue<RelayDisconnect> Disconnected = new Queue<RelayDisconnect>();
        public readonly Queue<RelayWelcome> Reconnected = new Queue<RelayWelcome>();
        public readonly Queue<RelayYouAreHost> YouAreHost = new Queue<RelayYouAreHost>();
        public readonly Queue<RelayHostEvent> HostChanged = new Queue<RelayHostEvent>();

        private StPlayer(StrictLoop loop, LobbyClient lobby, RelayClient client, PolledLobbyHttp http)
        {
            this.loop = loop; Lobby = lobby; Client = client; Http = http;
            void On<T>(Queue<T> queue, T item, string what) { loop.Assert("callback " + what); queue.Enqueue(item); }
            client.GameFrame += f => On(Game, f, "GameFrame");
            client.RoomFrame += f => On(Room, f, "RoomFrame");
            client.MemberJoined += m => On(Joined, m, "MemberJoined");
            client.MemberLeft += m => On(Left, m, "MemberLeft");
            client.Disconnected += d => On(Disconnected, d, "Disconnected");
            client.Reconnected += w => On(Reconnected, w, "Reconnected");
            client.OnYouAreHost(e => On(YouAreHost, e, "YouAreHost"));
            client.OnHostChanged(e => On(HostChanged, e, "HostChanged"));
            client.Error += e => loop.Violations.Enqueue("callback threw: " + e.Message);
            loop.Pumped.Add(client);
        }

        public static async Task<StPlayer> CreateAsync(StrictLoop loop, Peer peer, string account, RelayClientOptions options)
        {
            // 测试对端签 gt2 走普通的 HttpClient（不是被测的 SDK）：放到线程池上，经轮询交回 loop 线程。
            var token = await loop.Offload(() => peer.GameTokenAsync(account));
            var http = new PolledLobbyHttp(loop);
            var lobby = new LobbyClient(peer.LobbyUrl, token, http, options.PumpDriven);
            return new StPlayer(loop, lobby, new RelayClient(lobby, options.WithTransport(loop)), http);
        }

        public void Dispose()
        {
            loop.Pumped.Remove(Client);
            Client.Dispose();
        }
    }

    /// <summary>
    /// 像 Unity WebGL 主线程的单线程环境（见文件开头）。Violations 记下所有“不该在别的线程上发生”的事；RunAsync 跑完 body 后
    /// 按 expectViolations 核对，返回记下的违规。
    /// </summary>
    internal sealed class StrictLoop : SynchronizationContext
    {
        private readonly Thread thread;
        private readonly Queue<(SendOrPostCallback callback, object? state)> posted = new Queue<(SendOrPostCallback, object?)>();
        private readonly List<IPolled> polled = new List<IPolled>();
        private readonly List<(Task task, Action done)> offloaded = new List<(Task, Action)>();
        private readonly List<(Func<bool> done, DateTime deadline, TaskCompletionSource<bool> signal)> waits =
            new List<(Func<bool>, DateTime, TaskCompletionSource<bool>)>();
        public readonly ConcurrentQueue<string> Violations = new ConcurrentQueue<string>();
        /// <summary>每一轮调 Pump 的客户端；Paused 里的跳过（模拟游戏不 Pump）。</summary>
        public readonly List<RelayClient> Pumped = new List<RelayClient>();
        public readonly HashSet<RelayClient> Paused = new HashSet<RelayClient>();

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
                finally
                {
                    foreach (var client in loop.Pumped.ToArray()) client.Dispose();
                }
                if (failure != null) finished.TrySetException(failure);
                else finished.TrySetResult(new List<string>(loop.Violations));
            }) { IsBackground = true, Name = "strict-loop-" + name };
            thread.Start();
            var violations = await finished.Task;
            if (!expectViolations) Check(violations.Count == 0, $"single-thread run '{name}': no violations: {string.Join(" | ", violations)}");
            return violations;
        }

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
            lock (posted) posted.Enqueue((callback, state)); // 照样执行：违规记下，用例照常往下走，最后统一核对
        }

        public override void Send(SendOrPostCallback callback, object? state)
        {
            Violations.Enqueue("SynchronizationContext.Send");
            Post(callback, state);
        }

        public override SynchronizationContext CreateCopy() => this;

        // 列表加锁：PumpDriven 关着时（探测器自检、修前的红）SDK 会从线程池调进来，违规照记，但不能把列表弄坏。
        public void Register(IPolled item) { Assert("register a polled item"); lock (polled) polled.Add(item); }
        public void Unregister(IPolled item) { Assert("unregister a polled item"); lock (polled) polled.Remove(item); }

        /// <summary>在线程池上跑 work（测试自己的 HTTP，不是被测的 SDK），完成后在 loop 线程上交回结果。</summary>
        public Task<T> Offload<T>(Func<Task<T>> work)
        {
            Assert("offload");
            var done = new TaskCompletionSource<T>();
            var task = Task.Run(work);
            lock (offloaded) offloaded.Add((task, () =>
            {
                if (task.IsFaulted) done.TrySetException(task.Exception!.InnerExceptions);
                else if (task.IsCanceled) done.TrySetCanceled();
                else done.TrySetResult(task.Result);
            }));
            return done.Task;
        }

        /// <summary>等到 condition 成立（每一轮检查一次），超时抛错。</summary>
        public async Task Until(string what, Func<bool> condition, double seconds = 5)
        {
            var signal = new TaskCompletionSource<bool>();
            waits.Add((condition, DateTime.UtcNow.AddSeconds(seconds), signal));
            if (!await signal.Task) throw new Exception("Timed out waiting for " + what);
            checks++;
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

        /// <summary>等 task 结束，返回它抛的异常（没抛返回 null）。</summary>
        public async Task<Exception?> Catch(Task task)
        {
            try { await task; return null; }
            catch (Exception error) { return error; }
        }

        /// <summary>一轮：执行投进来的续行 → 轮询连接、HTTP 与测试自己的线程池任务 → 对每个客户端 Pump → 检查等待。</summary>
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
            var completed = new List<Action>();
            lock (offloaded)
            {
                for (int i = offloaded.Count - 1; i >= 0; i--)
                {
                    if (!offloaded[i].task.IsCompleted) continue;
                    completed.Add(offloaded[i].done);
                    offloaded.RemoveAt(i);
                }
            }
            foreach (var done in completed) done();
            foreach (var client in Pumped.ToArray()) if (!Paused.Contains(client)) client.Pump();
            var now = DateTime.UtcNow;
            for (int i = waits.Count - 1; i >= 0; i--)
            {
                var (done, deadline, signal) = waits[i];
                if (done()) { waits.RemoveAt(i); signal.TrySetResult(true); }
                else if (now > deadline) { waits.RemoveAt(i); signal.TrySetResult(false); }
            }
        }
    }

    internal interface IPolled
    {
        void Poll();
    }

    /// <summary>
    /// 像浏览器 WebSocket 桥接的 TCP 连接：不用 IO 线程，ReceiveAsync 的任务只在 loop 线程的轮询里完成（Available / Poll(0)
    /// 非阻塞地看 socket）。拨号在 loop 线程上同步完成（本机）。SDK 在别的线程上调它时记违规。
    /// </summary>
    private sealed class PolledTcpTransport : IRelayTransport, IPolled
    {
        private readonly StrictLoop loop;
        private readonly object state = new object(); // PumpDriven 关着时 SDK 会从线程池调进来：保护自己，违规照记
        private Socket? socket;
        private bool disposed;
        private TaskCompletionSource<int>? pending;
        private byte[]? buffer;
        private int offset, count;

        public PolledTcpTransport(StrictLoop loop) { this.loop = loop; }

        public Task ConnectAsync(string address, CancellationToken cancellation)
        {
            loop.Assert("transport ConnectAsync");
            int colon = address.LastIndexOf(':');
            var socket = new Socket(AddressFamily.InterNetwork, SocketType.Stream, ProtocolType.Tcp) { NoDelay = true };
            socket.Connect(IPAddress.Parse(address.Substring(0, colon)), int.Parse(address.Substring(colon + 1), CultureInfo.InvariantCulture));
            lock (state)
            {
                if (disposed) { socket.Dispose(); throw new IOException("relay: transport disposed while connecting."); }
                this.socket = socket;
            }
            loop.Register(this);
            return Task.CompletedTask;
        }

        public void Send(byte[] frame)
        {
            loop.Assert("transport Send");
            Socket? socket;
            lock (state) { if (disposed) throw new IOException("relay: transport closed."); socket = this.socket; }
            try { socket!.Send(frame); }
            catch (SocketException error) { throw new IOException("relay: send failed.", error); }
        }

        public Task<int> ReceiveAsync(byte[] buffer, int offset, int count, CancellationToken cancellation)
        {
            loop.Assert("transport ReceiveAsync");
            lock (state)
            {
                if (disposed) return Task.FromResult(0);
                int got = TryReceiveLocked(buffer, offset, count);
                if (got != 0) return Task.FromResult(got < 0 ? 0 : got);
                pending = new TaskCompletionSource<int>();
                this.buffer = buffer; this.offset = offset; this.count = count;
                return pending.Task;
            }
        }

        public void Poll()
        {
            TaskCompletionSource<int>? done = null;
            int got = 0;
            lock (state)
            {
                if (pending == null || disposed) return;
                got = TryReceiveLocked(buffer!, offset, count);
                if (got != 0) { done = pending; pending = null; buffer = null; }
            }
            done?.TrySetResult(got < 0 ? 0 : got); // 续行（SDK 的读循环）就在这里、loop 线程上
        }

        /// <summary>取到的字节数；0 还没有；-1 连接已结束。</summary>
        private int TryReceiveLocked(byte[] buffer, int offset, int count)
        {
            try
            {
                // 可读时 Receive 不会阻塞：拿到字节，或者 0（对端关了）。不能先看 Available 再看 Poll——两次之间字节可能刚到，
                // 会把“有数据”误当成“关了”。
                if (!socket!.Poll(0, SelectMode.SelectRead)) return 0;
                int got = socket.Receive(buffer, offset, count, SocketFlags.None);
                return got == 0 ? -1 : got;
            }
            catch (Exception) { return -1; }
        }

        public void Dispose()
        {
            loop.Assert("transport Dispose");
            TaskCompletionSource<int>? done;
            lock (state)
            {
                if (disposed) return;
                disposed = true;
                done = pending; pending = null;
                socket?.Dispose();
            }
            loop.Unregister(this);
            done?.TrySetResult(0);
        }
    }

    /// <summary>
    /// 像 UnityWebRequest 的 lobby HTTP：请求由线程池上的 HttpClient 发（那是“浏览器”），答复在 loop 线程的轮询里交回。
    /// SDK 在别的线程上调它时记违规。顺带数 join 次数（重连要重新要令牌）。
    /// </summary>
    private sealed class PolledLobbyHttp : ILobbyHttp
    {
        private static readonly HttpClient Shared = new HttpClient();
        private readonly StrictLoop loop;
        private int joins;
        public int Joins => Volatile.Read(ref joins);

        public PolledLobbyHttp(StrictLoop loop) { this.loop = loop; }

        public Task<LobbyHttpResponse> SendAsync(string method, string url, string bearer, string? jsonBody, CancellationToken cancellation)
        {
            loop.Assert("lobby http SendAsync");
            if (url.EndsWith("/join", StringComparison.Ordinal)) Interlocked.Increment(ref joins);
            return loop.Offload(async () =>
            {
                using var request = new HttpRequestMessage(new HttpMethod(method), url);
                request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", bearer);
                if (jsonBody != null) request.Content = new StringContent(jsonBody, Encoding.UTF8, "application/json");
                using var response = await Shared.SendAsync(request, cancellation).ConfigureAwait(false);
                return new LobbyHttpResponse((int)response.StatusCode, await response.Content.ReadAsStringAsync().ConfigureAwait(false));
            });
        }
    }
}
