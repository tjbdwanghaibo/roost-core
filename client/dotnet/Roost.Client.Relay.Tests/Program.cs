using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Linq;
using System.Net.Http;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client.Relay;

// Roost.Client.Relay 的测试：
//   1. 金样：SDK 编出的帧与 client/spec/relay-packets.json（Go 的 client/wire 生成）逐字节相同，relay 发来的控制帧解出预期字段；
//   2. 真实对端：起 client/relay 的测试二进制当对端（peer_test.go，Go 写的真实 relay 与 lobby，Redis 用 ~/.roost-it 的），
//      跑建房、加入、游戏通道往返、房间通道直达、禁言、踢人、经 lobby 离开、断线重连、lobby 错误映射。
// 用法：dotnet run --project client/dotnet/Roost.Client.Relay.Tests [-- <仓库根目录>]
internal static class Program
{
    private const string Game = "party";
    private static int checks;

    private static async Task<int> Main(string[] args)
    {
        string root = args.Length > 0 ? args[0] : FindRepoRoot();
        try
        {
            Golden(Path.Combine(root, "client", "spec", "relay-packets.json"));
            Console.WriteLine("golden: ok");
            await UnknownControlOp();
            Console.WriteLine("unknown control op: ok");
            await using var peer = await Peer.StartAsync(root);
            Console.WriteLine($"peer: lobby {peer.LobbyUrl}, relay {peer.RelayTcp}, room directory {peer.Backend}");
            await LobbyErrors(peer);
            Console.WriteLine("lobby errors: ok");
            await RoomLifecycle(peer);
            Console.WriteLine("room lifecycle: ok");
            await Reconnect(peer);
            Console.WriteLine("reconnect: ok");
            Check(Player.CallbackErrors.IsEmpty, "no callback threw: " + string.Join("; ", Player.CallbackErrors));
            Console.WriteLine($"all {checks} checks passed");
            return 0;
        }
        catch (Exception error)
        {
            Console.Error.WriteLine("FAILED: " + error);
            return 1;
        }
    }

    private static void Check(bool condition, string what)
    {
        checks++;
        if (!condition) throw new Exception("Check failed: " + what);
    }

    // ---------- 金样 ----------

    private static void Golden(string path)
    {
        var golden = new Dictionary<string, string>();
        foreach (var item in JsonDocument.Parse(File.ReadAllText(path)).RootElement.EnumerateArray())
            golden[item.GetProperty("name").GetString()!] = item.GetProperty("hex").GetString()!;

        var sent = new Dictionary<string, RelayFrame>
        {
            ["relay_game_member_up"] = new RelayFrame(RelayChannel.Game, RelayCodec.All, 101, new byte[] { 1, 2 }),
            ["relay_game_to_host"] = new RelayFrame(RelayChannel.Game, 3, 101, new byte[] { 1, 2 }),
            ["relay_game_host_to_slot"] = new RelayFrame(RelayChannel.Game, 3, 202, new byte[] { 9 }, RelayCodec.FlagPush | RelayCodec.FlagSync),
            ["relay_room_broadcast"] = new RelayFrame(RelayChannel.Room, RelayCodec.All, 7, Encoding.ASCII.GetBytes("hi")),
            ["relay_welcome"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.Welcome, RelayCodec.EncodeWelcome(new RelayWelcome(3, false, 1))),
            ["relay_member_joined"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.MemberJoined, RelayCodec.EncodeMember(new RelayMember(3, "a3", RelayReason.None))),
            ["relay_member_left"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.MemberLeft, RelayCodec.EncodeMember(new RelayMember(3, "a3", RelayReason.Kicked))),
            ["relay_closed"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.Closed, new[] { (byte)RelayReason.RoomClosed }),
            ["relay_muted"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.Muted, new byte[] { 1 }),
            ["relay_kick"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.Kick, ReadOnlyMemory<byte>.Empty),
            ["relay_mute"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.Mute, new byte[] { 1 }),
            ["relay_ping"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.Ping, new byte[] { 0, 0, 0, 42 }),
            ["relay_pong"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.Pong, new byte[] { 0, 0, 0, 42 }),
        };
        // 金样以后会追加（换 host 的 64～127）：这里核对本 SDK 认识的每一条都在、都相同。
        foreach (var entry in sent)
            Check(golden.TryGetValue(entry.Key, out var expected) && Hex(RelayCodec.Encode(entry.Value)) == expected, "C# encodes " + entry.Key + " like Go");
        Check(Hex(RelayCodec.EncodeToken("rt1.token")) == golden["relay_token"], "C# encodes the token frame like Go");

        // 解码：每一条都能解开、再编回同样的字节；控制帧的载荷解出预期字段。
        foreach (var entry in golden.Where(e => e.Key != "relay_token"))
        {
            var frame = RelayCodec.Decode(Convert.FromHexString(entry.Value));
            Check(Hex(RelayCodec.Encode(frame)) == entry.Value, "C# round-trips " + entry.Key);
        }
        var token = RelayCodec.Decode(Convert.FromHexString(golden["relay_token"]));
        Check(token.Channel == RelayChannel.None && token.MessageId == 0 && Encoding.ASCII.GetString(token.Payload.Span) == "rt1.token", "token frame decodes");
        var hostToSlot = RelayCodec.Decode(Convert.FromHexString(golden["relay_game_host_to_slot"]));
        Check(hostToSlot.Channel == RelayChannel.Game && hostToSlot.Slot == 3 && hostToSlot.MessageId == 202 &&
              hostToSlot.GameFlags == (RelayCodec.FlagPush | RelayCodec.FlagSync), "game frame header fields");
        var room = RelayCodec.Decode(Convert.FromHexString(golden["relay_room_broadcast"]));
        Check(room.Channel == RelayChannel.Room && room.Slot == RelayCodec.All && room.MessageId == 7, "room frame header fields");
        var welcomeFrame = RelayCodec.Decode(Convert.FromHexString(golden["relay_welcome"]));
        Check(RelayCodec.DecodeWelcome(welcomeFrame.Slot, welcomeFrame.Payload.Span).Equals(new RelayWelcome(3, false, 1)), "welcome payload");
        var left = RelayCodec.Decode(Convert.FromHexString(golden["relay_member_left"]));
        var member = RelayCodec.DecodeMember(left.Slot, left.Payload.Span);
        Check(member.Slot == 3 && member.Account == "a3" && member.Reason == RelayReason.Kicked, "member payload");

        // 与 Go 的 ValidateRelay 相同的拒绝规则。
        Refuses(() => RelayCodec.Decode(Convert.FromHexString("52530238000000010000000000000000")), "reserved bit 5");
        Refuses(() => RelayCodec.Decode(Convert.FromHexString("52530218000000000000000000000000")), "control frame without an op");
        Refuses(() => RelayCodec.Decode(Convert.FromHexString("52530219000000200000000000000000")), "control frame with the push bit");
        Refuses(() => RelayCodec.Decode(Convert.FromHexString("5253020e000000010000000000000000")), "sync and lockstep together");
        Refuses(() => RelayCodec.Decode(Convert.FromHexString("52530201000000000000000000000000")), "token frame with a flag");
        Refuses(() => RelayCodec.Decode(Convert.FromHexString("5253020800000001000000000000000b" + new string('0', 22)), 10), "payload over the limit");
    }

    private static void Refuses(Action action, string what)
    {
        try { action(); }
        catch (InvalidDataException) { checks++; return; }
        throw new Exception("Check failed: codec accepted " + what);
    }

    private static string Hex(byte[] data) => Convert.ToHexString(data).ToLowerInvariant();

    // 不认识的控制帧操作码（换 host 留的 64～127，host 组交出编码表之前）丢弃、连接不断；注册了 OnControl 的交给它。
    // 用本地 TcpListener 扮 relay：收令牌后一次写出 Welcome + 操作码 64 + 一帧游戏帧。
    private static async Task UnknownControlOp()
    {
        var listener = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
        listener.Start();
        var port = ((System.Net.IPEndPoint)listener.LocalEndpoint).Port;
        var fake = Task.Run(async () =>
        {
            using var socket = await listener.AcceptTcpClientAsync();
            var stream = socket.GetStream();
            var token = await RelayCodec.ReadAsync(stream, 1024);
            var reply = RelayCodec.Encode(new RelayFrame(RelayChannel.Control, 2, RelayOps.Welcome, RelayCodec.EncodeWelcome(new RelayWelcome(2, false, 1))))
                .Concat(RelayCodec.Encode(new RelayFrame(RelayChannel.Control, 2, 64, new byte[] { 7 })))
                .Concat(RelayCodec.Encode(new RelayFrame(RelayChannel.Game, 1, 9, new byte[] { 1 }))).ToArray();
            await stream.WriteAsync(reply);
            int trailing = 0;
            try { trailing = await stream.ReadAsync(new byte[1]); } catch (IOException) { } // 等客户端断开
            Check(trailing == 0, "client sent nothing after the token");
            return Encoding.UTF8.GetString(token.Payload.Span);
        });
        using var client = new RelayClient(null, new RelayClientOptions { Game = Game });
        var games = new ConcurrentQueue<RelayFrame>();
        var unknown = new ConcurrentQueue<RelayFrame>();
        client.GameFrame += games.Enqueue;
        client.OnControl(64, unknown.Enqueue);
        var welcome = await client.ConnectAsync(new RoomTicket(new RoomInfo(), "127.0.0.1:" + port, "rt1.fake", 0));
        Check(welcome.Equals(new RelayWelcome(2, false, 1)), "fake relay welcome");
        var deadline = DateTime.UtcNow.AddSeconds(5);
        while (games.IsEmpty && DateTime.UtcNow < deadline) { client.Pump(); await Task.Delay(5); }
        Check(unknown.Count == 1 && games.Count == 1 && client.IsConnected, "op 64 handed to OnControl, the next game frame delivered, connection kept");
        client.Dispose();
        Check(await fake == "rt1.fake", "token frame carries the relay token");
        listener.Stop();
    }

    // ---------- 真实对端 ----------

    private static async Task LobbyErrors(Peer peer)
    {
        var bad = new LobbyClient(peer.LobbyUrl, "gt1.not-issued");
        var unauthorized = await Throws<LobbyException>(() => bad.ListRoomsAsync(Game, "duel"));
        Check(unauthorized.Code == LobbyErrorCode.Unauthorized && unauthorized.Status == 401 && unauthorized.ErrorName == "unauthorized", "bad token → unauthorized");
        var lobby = new LobbyClient(peer.LobbyUrl, await peer.GameTokenAsync("cs-errs"));
        Check((await Throws<LobbyException>(() => lobby.ListRoomsAsync("other", "duel"))).Code == LobbyErrorCode.GameNotServed, "other game → game_not_served");
        Check((await Throws<LobbyException>(() => lobby.JoinRoomAsync("0000000000000000", new JoinRoomRequest { Game = Game }))).Code == LobbyErrorCode.RoomNotFound, "missing room");
        Check((await Throws<LobbyException>(() => lobby.CreateRoomAsync(new CreateRoomRequest { Game = Game, Mode = "bad mode!" }))).Code == LobbyErrorCode.BadRequest, "bad mode");
        var full = await lobby.CreateRoomAsync(new CreateRoomRequest { Game = Game, Mode = "full", MaxMembers = 1 });
        var other = new LobbyClient(peer.LobbyUrl, await peer.GameTokenAsync("cs-errs-2"));
        Check((await Throws<LobbyException>(() => other.JoinRoomAsync(full.Room.Id, new JoinRoomRequest { Game = Game }))).Code == LobbyErrorCode.RoomFull, "full room");
        var noWs = await Throws<LobbyException>(() => lobby.CreateRoomAsync(new CreateRoomRequest { Game = Game, Mode = "ws", Transport = RelayTransport.Ws }));
        Check(noWs.Code == LobbyErrorCode.NoRelay && noWs.IsTemporary, "ws transport without a ws relay → no_relay");
        using var client = new RelayClient(lobby, new RelayClientOptions { Game = Game });
        var forged = await Throws<RelayClosedException>(() => client.ConnectAsync(new RoomTicket(full.Room, peer.RelayTcp, "rt1.forged", 0)));
        Check(forged.Reason == RelayReason.TokenRejected, "forged relay token → Closed{token_rejected}");
        await Throws<ArgumentException>(() => client.ConnectAsync(new RoomTicket(full.Room, "ws://127.0.0.1:1/relay", "x", 0)));
    }

    private static async Task RoomLifecycle(Peer peer)
    {
        using var host = await Player.CreateAsync(peer, "cs-host", new RelayClientOptions { Game = Game });
        var ticket = await host.Lobby.CreateRoomAsync(new CreateRoomRequest { Game = Game, Mode = "duel", Name = "cs", Password = "pw", MaxMembers = 4, AttrsJson = "{\"map\":\"a\"}" });
        Check(ticket.Room.Host == "cs-host" && ticket.Room.HasPassword && ticket.RelayAddr == peer.RelayTcp && ticket.Room.AttrsJson == "{\"map\":\"a\"}", "create ticket");
        var hostWelcome = await host.Client.ConnectAsync(ticket);
        Check(hostWelcome.IsHost && hostWelcome.Slot == 1 && hostWelcome.HostSlot == 1 && host.Client.IsHost, "host welcome");
        await Until("room listed", async () =>
        {
            var rooms = await host.Lobby.ListRoomsAsync(Game, "duel");
            return rooms.Count == 1 && rooms[0].Id == ticket.Room.Id;
        });

        using var m1 = await Player.CreateAsync(peer, "cs-m1", new RelayClientOptions { Game = Game, Password = "pw" });
        using var m2 = await Player.CreateAsync(peer, "cs-m2", new RelayClientOptions { Game = Game, Password = "pw" });
        Check((await Throws<LobbyException>(() => m1.Lobby.JoinRoomAsync(ticket.Room.Id, new JoinRoomRequest { Game = Game, Password = "bad" }))).Code == LobbyErrorCode.WrongPassword, "wrong password");
        var w1 = await m1.JoinAsync(ticket.Room.Id, "pw");
        var w2 = await m2.JoinAsync(ticket.Room.Id, "pw");
        Check(!w1.IsHost && w1.Slot == 2 && w1.HostSlot == 1 && w2.Slot == 3, "member welcomes");
        var j1 = await host.Next(host.Joined, "m1 joined");
        var j2 = await host.Next(host.Joined, "m2 joined");
        Check(j1.Slot == 2 && j1.Account == "cs-m1" && j1.Reason == RelayReason.None && j2.Slot == 3 && j2.Account == "cs-m2", "member joined notices");

        // 游戏通道：成员的帧只到房主（目标不看）；房主按目标回给一个成员（游戏位原样），或广播全体。
        Check(m1.Client.SendGame(3, 101, Encoding.UTF8.GetBytes("move")), "member send");
        Frame(await host.Next(host.Game, "member game frame"), RelayChannel.Game, 2, 101, "move");
        Check(host.Client.SendGame(2, 202, Encoding.UTF8.GetBytes("state"), RelayCodec.FlagPush | RelayCodec.FlagSync), "host send to slot");
        var state = await m1.Next(m1.Game, "host game frame");
        Frame(state, RelayChannel.Game, 1, 202, "state");
        Check(state.GameFlags == (RelayCodec.FlagPush | RelayCodec.FlagSync), "game flags carried through");
        Check(host.Client.SendGame(RelayCodec.All, 203, Encoding.UTF8.GetBytes("tick")), "host broadcast");
        Frame(await m1.Next(m1.Game, "broadcast at m1"), RelayChannel.Game, 1, 203, "tick");
        Frame(await m2.Next(m2.Game, "broadcast at m2"), RelayChannel.Game, 1, 203, "tick");
        Check(m2.Game.IsEmpty, "m2 got only the broadcast (202 went to m1, 101 only to the host)");

        // 房间通道：直达某个槽位（不经房主）；全体时房主也收到。
        Check(m1.Client.SendRoom(3, 7, Encoding.UTF8.GetBytes("psst")), "direct room frame");
        Frame(await m2.Next(m2.Room, "direct room frame"), RelayChannel.Room, 2, 7, "psst");
        Check(m2.Client.SendRoom(RelayCodec.All, 8, Encoding.UTF8.GetBytes("hi all")), "room broadcast");
        Frame(await host.Next(host.Room, "room broadcast at host"), RelayChannel.Room, 3, 8, "hi all");
        Frame(await m1.Next(m1.Room, "room broadcast at m1"), RelayChannel.Room, 3, 8, "hi all");
        Check(host.Room.IsEmpty, "host did not get the direct room frame");

        // 只有房主能踢人、禁言。
        await Throws<InvalidOperationException>(() => { m1.Client.Kick(3); return Task.CompletedTask; });

        // 禁言：m2 收到通知，房间帧被丢弃，游戏帧照常（同一连接按序：游戏帧到了说明前面的房间帧没被转出）。
        Check(host.Client.Mute(3, true), "mute");
        Check(await m2.Next(m2.Muted, "muted notice"), "muted notice says muted");
        m2.Client.SendRoom(RelayCodec.All, 9, Encoding.UTF8.GetBytes("muted"));
        m2.Client.SendGame(RelayCodec.All, 104, Encoding.UTF8.GetBytes("after-mute"));
        Frame(await host.Next(host.Game, "game frame after mute"), RelayChannel.Game, 3, 104, "after-mute");
        Check(host.Room.IsEmpty, "room frame from a muted member was dropped");

        // 踢人：被踢者收到 Closed{kicked}、不重连；房主收到 MemberLeft{kicked}；被踢的账号再进被 relay 拒绝。
        Check(host.Client.Kick(3), "kick");
        var kicked = await m2.Next(m2.Disconnected, "kicked");
        Check(kicked.Reason == RelayReason.Kicked && !kicked.Reconnecting && kicked.Error is RelayClosedException, "kicked disconnect");
        var gone = await host.Next(host.Left, "kicked member left");
        Check(gone.Slot == 3 && gone.Account == "cs-m2" && gone.Reason == RelayReason.Kicked, "member left (kicked)");
        await m2.Client.Completion;
        var again = await Throws<RelayClosedException>(() => m2.Client.RejoinAsync());
        Check(again.Reason == RelayReason.Kicked, "kicked account cannot come back");

        // 经 lobby 离开：relay 断开这个账号，房主收到 MemberLeft{left}。
        await m1.Client.LeaveAsync();
        var leftViaLobby = await host.Next(host.Left, "member left via lobby");
        Check(leftViaLobby.Slot == 2 && leftViaLobby.Reason == RelayReason.Left, "member left (left)");

        // 只有房主能改状态；开始后的房间新人不能进。
        Check((await Throws<LobbyException>(() => m2.Lobby.SetRoomStateAsync(ticket.Room.Id, new SetRoomStateRequest { Game = Game, State = RoomState.Started }))).Code == LobbyErrorCode.NotHost, "member cannot change state");
        var started = await host.Lobby.SetRoomStateAsync(ticket.Room.Id, new SetRoomStateRequest { Game = Game, State = RoomState.Started });
        Check(started.State == RoomState.Started, "host changed state");
        Check((await Throws<LobbyException>(() => m1.Lobby.JoinRoomAsync(ticket.Room.Id, new JoinRoomRequest { Game = Game, Password = "pw" }))).Code == LobbyErrorCode.RoomStarted, "started room refuses newcomers");
    }

    private static async Task Reconnect(Peer peer)
    {
        using var host = await Player.CreateAsync(peer, "cs-host-r", new RelayClientOptions { Game = Game });
        var ticket = await host.Lobby.CreateRoomAsync(new CreateRoomRequest { Game = Game, Mode = "coop" });
        await host.Client.ConnectAsync(ticket);
        using var member = await Player.CreateAsync(peer, "cs-member-r", new RelayClientOptions
        {
            Game = Game, ReconnectAttempts = 5, ReconnectMinBackoff = TimeSpan.FromMilliseconds(20), ReconnectMaxBackoff = TimeSpan.FromMilliseconds(100),
            PingInterval = TimeSpan.FromMilliseconds(100),
        });
        var first = await member.JoinAsync(ticket.Room.Id, null);
        await host.Next(host.Joined, "member joined");
        int joinsBefore = member.Joins;

        member.Client.AbortTransportForTest(); // 断网：本地 socket 关掉，relay 没发 Closed
        var lost = await member.Next(member.Disconnected, "disconnect");
        Check(lost.Reason == RelayReason.Disconnected && lost.Reconnecting, "transport loss → reconnecting");
        var back = await member.Next(member.Reconnected, "reconnected");
        Check(!back.IsHost && back.HostSlot == 1 && back.Slot == first.Slot, "reconnected welcome");
        Check(member.Joins - joinsBefore == 1, "reconnect asked the lobby for a fresh token (one join)");
        await Until("host sees the member again", () =>
        {
            while (host.Joined.TryDequeue(out var m)) if (m.Account == "cs-member-r") return Task.FromResult(true);
            return Task.FromResult(false);
        });
        member.Client.SendGame(RelayCodec.All, 1, Encoding.UTF8.GetBytes("back"));
        Frame(await host.Next(host.Game, "frame after reconnect"), RelayChannel.Game, back.Slot, 1, "back");
        host.Client.SendGame(back.Slot, 2, Encoding.UTF8.GetBytes("welcome back"));
        Frame(await member.Next(member.Game, "host frame after reconnect"), RelayChannel.Game, 1, 2, "welcome back");
        await Until("rtt measured", () => Task.FromResult(member.Client.Rtt > TimeSpan.Zero));

        // 房主断开：这一批关房；成员收到 room_closed，开着自动重连也不重连。
        host.Client.Dispose();
        var closed = await member.Next(member.Disconnected, "room closed");
        Check(closed.Reason == RelayReason.RoomClosed && !closed.Reconnecting, "room closed → no reconnect");
        await member.Client.Completion;
        Check(!member.Client.IsConnected, "member client finished");
    }

    private static void Frame(RelayFrame frame, RelayChannel channel, uint slot, uint messageId, string payload) =>
        Check(frame.Channel == channel && frame.Slot == slot && frame.MessageId == messageId && Encoding.UTF8.GetString(frame.Payload.Span) == payload,
            $"frame {frame.Channel}/{frame.Slot}/{frame.MessageId}/{Encoding.UTF8.GetString(frame.Payload.Span)} == {channel}/{slot}/{messageId}/{payload}");

    private static async Task<T> Throws<T>(Func<Task> action) where T : Exception
    {
        try { await action(); }
        catch (T error) { checks++; return error; }
        throw new Exception($"Check failed: expected {typeof(T).Name}");
    }

    private static async Task Until(string what, Func<Task<bool>> condition)
    {
        var deadline = DateTime.UtcNow.AddSeconds(5);
        while (!await condition())
        {
            Player.PumpAll();
            if (DateTime.UtcNow > deadline) throw new Exception("Timed out waiting for " + what);
            await Task.Delay(10);
        }
        checks++;
    }

    private static string FindRepoRoot()
    {
        for (var dir = new DirectoryInfo(AppContext.BaseDirectory); dir != null; dir = dir.Parent)
            if (File.Exists(Path.Combine(dir.FullName, "go.mod")) && Directory.Exists(Path.Combine(dir.FullName, "client", "spec"))) return dir.FullName;
        throw new Exception("Cannot find the roost-core root; pass it as the first argument.");
    }

    /// <summary>一个玩家：lobby 客户端 + relay 客户端 + 经 Pump 收到的事件。</summary>
    private sealed class Player : IDisposable
    {
        public static readonly ConcurrentQueue<Exception> CallbackErrors = new ConcurrentQueue<Exception>();
        private static readonly List<Player> Live = new List<Player>();
        public readonly LobbyClient Lobby;
        public readonly RelayClient Client;
        public readonly ConcurrentQueue<RelayFrame> Game = new ConcurrentQueue<RelayFrame>(), Room = new ConcurrentQueue<RelayFrame>();
        public readonly ConcurrentQueue<RelayMember> Joined = new ConcurrentQueue<RelayMember>(), Left = new ConcurrentQueue<RelayMember>();
        public readonly ConcurrentQueue<bool> Muted = new ConcurrentQueue<bool>();
        public readonly ConcurrentQueue<RelayDisconnect> Disconnected = new ConcurrentQueue<RelayDisconnect>();
        public readonly ConcurrentQueue<RelayWelcome> Reconnected = new ConcurrentQueue<RelayWelcome>();
        private readonly JoinCounter counter;
        public int Joins => counter.Joins;

        private Player(LobbyClient lobby, RelayClient client, JoinCounter counter)
        {
            Lobby = lobby; Client = client; this.counter = counter;
            client.GameFrame += Game.Enqueue;
            client.RoomFrame += Room.Enqueue;
            client.MemberJoined += Joined.Enqueue;
            client.MemberLeft += Left.Enqueue;
            client.Muted += Muted.Enqueue;
            client.Disconnected += Disconnected.Enqueue;
            client.Reconnected += Reconnected.Enqueue;
            client.Error += CallbackErrors.Enqueue;
            lock (Live) Live.Add(this);
        }

        public static async Task<Player> CreateAsync(Peer peer, string account, RelayClientOptions options)
        {
            var counter = new JoinCounter();
            var lobby = new LobbyClient(peer.LobbyUrl, await peer.GameTokenAsync(account), new HttpClient(counter));
            return new Player(lobby, new RelayClient(lobby, options), counter);
        }

        public async Task<RelayWelcome> JoinAsync(string roomId, string? password)
        {
            var ticket = await Lobby.JoinRoomAsync(roomId, new JoinRoomRequest { Game = "party", Password = password });
            return await Client.ConnectAsync(ticket);
        }

        public async Task<T> Next<T>(ConcurrentQueue<T> queue, string what)
        {
            var deadline = DateTime.UtcNow.AddSeconds(5);
            while (true)
            {
                PumpAll();
                if (queue.TryDequeue(out var item)) { checks++; return item; }
                if (DateTime.UtcNow > deadline) throw new Exception("Timed out waiting for " + what);
                await Task.Delay(5);
            }
        }

        public static void PumpAll()
        {
            Player[] players;
            lock (Live) players = Live.ToArray();
            foreach (var player in players) player.Client.Pump();
        }

        public void Dispose()
        {
            Client.Dispose();
            lock (Live) Live.Remove(this);
        }
    }

    /// <summary>数经 lobby 的 join 次数：重连要重新要令牌。</summary>
    private sealed class JoinCounter : DelegatingHandler
    {
        private int joins;
        public int Joins => Volatile.Read(ref joins);
        public JoinCounter() : base(new HttpClientHandler()) { }
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken cancellationToken)
        {
            if (request.RequestUri!.AbsolutePath.EndsWith("/join", StringComparison.Ordinal)) Interlocked.Increment(ref joins);
            return base.SendAsync(request, cancellationToken);
        }
    }

    /// <summary>Go 写的测试对端进程（client/relay/peer_test.go）：先 go test -c，再以 ROOST_RELAYPEER_SERVE=1 起二进制；stdin 关闭它就退出。</summary>
    private sealed class Peer : IAsyncDisposable
    {
        private readonly Process process;
        private readonly string directory;
        private static readonly HttpClient Http = new HttpClient();
        public string LobbyUrl { get; }
        public string RelayTcp { get; }
        public string Backend { get; }

        private Peer(Process process, string directory, JsonElement info)
        {
            this.process = process; this.directory = directory;
            LobbyUrl = info.GetProperty("lobby_url").GetString()!;
            RelayTcp = info.GetProperty("relay_tcp").GetString()!;
            Backend = info.GetProperty("backend").GetString()!;
        }

        public static async Task<Peer> StartAsync(string root)
        {
            var directory = Directory.CreateTempSubdirectory("roost-relaypeer-").FullName;
            var binary = Path.Combine(directory, "relaypeer");
            // 对端是 client/relay 的测试二进制（peer_test.go）：ROOST_RELAYPEER_SERVE=1 时它不跑用例，只当对端。
            var build = Process.Start(new ProcessStartInfo("go", $"test -c -o \"{binary}\" ./client/relay") { WorkingDirectory = root })!;
            await build.WaitForExitAsync();
            if (build.ExitCode != 0) throw new Exception("go test -c ./client/relay failed");
            var start = new ProcessStartInfo(binary)
            {
                RedirectStandardInput = true, RedirectStandardOutput = true, UseShellExecute = false, WorkingDirectory = root,
            };
            start.Environment["ROOST_RELAYPEER_SERVE"] = "1";
            // C# 只连 TCP：relay 不开 WebSocket，lobby 对 transport=ws 答 no_relay。
            start.Environment["ROOST_RELAYPEER_WS"] = "0";
            var process = Process.Start(start)!;
            var line = await process.StandardOutput.ReadLineAsync() ?? throw new Exception("relaypeer exited before printing its addresses");
            return new Peer(process, directory, JsonDocument.Parse(line).RootElement);
        }

        public async Task<string> GameTokenAsync(string account)
        {
            var reply = await Http.PostAsync(LobbyUrl + "/harness/game-token", new StringContent("{\"account\":\"" + account + "\"}", Encoding.UTF8, "application/json"));
            reply.EnsureSuccessStatusCode();
            return JsonDocument.Parse(await reply.Content.ReadAsStringAsync()).RootElement.GetProperty("token").GetString()!;
        }

        public async ValueTask DisposeAsync()
        {
            process.StandardInput.Close();
            using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(15));
            try { await process.WaitForExitAsync(timeout.Token); }
            catch (OperationCanceledException) { process.Kill(); }
            process.Dispose();
            try { Directory.Delete(directory, true); } catch (IOException) { }
        }
    }
}
