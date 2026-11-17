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
//      换 host 的 13 条另用本地假 relay 走 SDK 的公开方法与分发路径核对（HostFramesAgainstFakeRelay）；
//   2. 真实对端：起 client/relay 的测试二进制当对端（peer_test.go，Go 写的真实 relay 与 lobby，Redis 用 ~/.roost-it 的），
//      跑建房、加入、游戏通道往返、房间通道直达、禁言、踢人（记在 lobby）、离开、断线重连、lobby 错误映射，以及换 host：
//      主动转交、房主断线后候选拿快照接手、宽限期内房主回来、候选顺序、快照节奏与 Rejected、定时快照、房主心跳、
//      重连遇到 lobby_unavailable 退避重试 / lobby_rejected 停止。
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
            Compressors();
            Console.WriteLine("compressors: ok");
            await HostFramesAgainstFakeRelay(Path.Combine(root, "client", "spec", "relay-packets.json"));
            Console.WriteLine("host frames against a fake relay: ok");
            await using var peer = await Peer.StartAsync(root);
            Console.WriteLine($"peer: lobby {peer.LobbyUrl}, relay {peer.RelayTcp}, room directory {peer.Backend}");
            // 真实对端用例；环境变量 ROOST_RELAY_TESTS_ONLY=<名字片段> 时只跑名字含它的那些（变异时用来把红落到指定用例上）。
            var only = Environment.GetEnvironmentVariable("ROOST_RELAY_TESTS_ONLY");
            var cases = new (string name, Func<Peer, Task> run)[]
            {
                ("lobby errors", LobbyErrors),
                ("room lifecycle", RoomLifecycle),
                ("reconnect", Reconnect),
                ("host transfer", Transfer),
                ("host loss → snapshot to the declared successor", HostLoss),
                ("host resumed within the grace period", HostResumed),
                ("host leave uses the successor order", LeaveWithSuccessors),
                ("snapshot pacing and rejections", SnapshotPacing),
                ("automatic snapshots", AutoSnapshot),
                ("host heartbeat under host_idle_timeout", HostHeartbeat),
                ("reconnect vs lobby_unavailable / lobby_rejected", ReconnectAdmission),
                ("started room: reconnect, rejoin, kick an offline seat", StartedRoom),
                ("welcome epoch for a late joiner and a rejoin", WelcomeEpoch),
                ("presence re-sent after host changed and reconnect", Presence),
            };
            foreach (var (name, run) in cases)
            {
                if (!string.IsNullOrEmpty(only) && !name.Contains(only, StringComparison.Ordinal)) continue;
                await run(peer);
                Console.WriteLine(name + ": ok");
            }
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
            ["relay_welcome"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.Welcome, RelayCodec.EncodeWelcome(new RelayWelcome(3, false, 1, 5))),
            ["relay_member_joined"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.MemberJoined, RelayCodec.EncodeMember(new RelayMember(3, "a3", RelayReason.None))),
            ["relay_member_left"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.MemberLeft, RelayCodec.EncodeMember(new RelayMember(3, "a3", RelayReason.Kicked))),
            ["relay_closed"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.Closed, new[] { (byte)RelayReason.RoomClosed }),
            ["relay_muted"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.Muted, new byte[] { 1 }),
            ["relay_kick"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.Kick, ReadOnlyMemory<byte>.Empty),
            ["relay_mute"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.Mute, new byte[] { 1 }),
            ["relay_ping"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.Ping, new byte[] { 0, 0, 0, 42 }),
            ["relay_pong"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.Pong, new byte[] { 0, 0, 0, 42 }),
            // 换 host（RelayCodec 的载荷编码；SDK 公开方法的发送路径在 HostFramesAgainstFakeRelay 里核对）。
            ["relay_you_are_member"] = new RelayFrame(RelayChannel.Control, 1, RelayOps.YouAreMember, RelayCodec.EncodeEpoch(2)),
            ["relay_you_are_host_snapshot"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.YouAreHost, RelayCodec.EncodeYouAreHost(new RelayYouAreHost(2, RelayStateSource.Snapshot, 7,
                TimeSpan.FromMilliseconds(1500), new[] { new RelaySeat(1, "a1", false), new RelaySeat(3, "a3", true) }, new byte[] { 0, 0xAB, 0xCD }))),
            ["relay_you_are_host_handoff"] = new RelayFrame(RelayChannel.Control, 2, RelayOps.YouAreHost, RelayCodec.EncodeYouAreHost(new RelayYouAreHost(1, RelayStateSource.Handoff, 0,
                TimeSpan.Zero, new[] { new RelaySeat(1, "a1", true), new RelaySeat(2, "a2", true) }, new byte[] { 0, 0x68 }))),
            ["relay_host_changing"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.HostChanging, RelayCodec.EncodeEpoch(2)),
            ["relay_host_changed"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.HostChanged, RelayCodec.EncodeHostChanged(2, "a3")),
            ["relay_host_resumed"] = new RelayFrame(RelayChannel.Control, 1, RelayOps.HostResumed, RelayCodec.EncodeEpoch(0)),
            ["relay_rejected_snapshot_too_soon"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.Rejected, RelayCodec.EncodeRejected(new RelayRejected(RelayOps.Snapshot, RelayRejectCode.TooSoon, 8))),
            ["relay_transfer"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.Transfer, new byte[] { 0 }.Concat(Encoding.ASCII.GetBytes("handoff")).ToArray()),
            ["relay_leave"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.Leave, ReadOnlyMemory<byte>.Empty),
            ["relay_successors"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.Successors, RelayCodec.EncodeSuccessors(new uint[] { 3, 2 })),
            ["relay_snapshot"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.Snapshot, RelayCodec.EncodeSnapshot(7, new byte[] { 0, 0xAB, 0xCD })),
            ["relay_host_ready"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.HostReady, RelayCodec.EncodeEpoch(2)),
            ["relay_closed_lobby_rejected"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.Closed, new[] { (byte)RelayReason.LobbyRejected }),
            // 前台上报：成员 → relay 的 Presence{前台}，relay → 房主的 MemberPresence{槽位 3，后台}
            ["relay_presence"] = new RelayFrame(RelayChannel.Control, 0, RelayOps.Presence, RelayCodec.EncodePresence(true)),
            ["relay_member_presence"] = new RelayFrame(RelayChannel.Control, 3, RelayOps.MemberPresence, RelayCodec.EncodePresence(false)),
        };
        // 金样以后可能追加：这里核对本 SDK 认识的每一条都在、都相同（换 host 的 13 条在内）。
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
        Check(RelayCodec.DecodeWelcome(welcomeFrame.Slot, welcomeFrame.Payload.Span).Equals(new RelayWelcome(3, false, 1, 5)), "welcome payload (epoch 5)");
        Refuses(() => RelayCodec.DecodeWelcome(3, welcomeFrame.Payload.Span.Slice(0, 5)), "the old 5-byte welcome");
        Check(RelayCodec.DecodePresence(RelayCodec.Decode(Convert.FromHexString(golden["relay_presence"])).Payload.Span), "presence payload");
        var presenceFrame = RelayCodec.Decode(Convert.FromHexString(golden["relay_member_presence"]));
        Check(presenceFrame.Slot == 3 && !RelayCodec.DecodePresence(presenceFrame.Payload.Span), "member presence payload");
        Refuses(() => RelayCodec.DecodePresence(new byte[] { 2 }), "presence 2");
        Refuses(() => RelayCodec.DecodePresence(Array.Empty<byte>()), "empty presence");
        var left = RelayCodec.Decode(Convert.FromHexString(golden["relay_member_left"]));
        var member = RelayCodec.DecodeMember(left.Slot, left.Payload.Span);
        Check(member.Slot == 3 && member.Account == "a3" && member.Reason == RelayReason.Kicked, "member payload");

        // 换 host：relay 发来的载荷解出预期字段。
        var snap = RelayCodec.DecodeYouAreHost(RelayCodec.Decode(Convert.FromHexString(golden["relay_you_are_host_snapshot"])).Payload.Span);
        Check(snap.Epoch == 2 && snap.Source == RelayStateSource.Snapshot && snap.SnapshotSeq == 7 && snap.SnapshotAge == TimeSpan.FromMilliseconds(1500) &&
              snap.Members.SequenceEqual(new[] { new RelaySeat(1, "a1", false), new RelaySeat(3, "a3", true) }) && Hex(snap.State!) == "00abcd", "you_are_host (snapshot) payload");
        var hand = RelayCodec.DecodeYouAreHost(RelayCodec.Decode(Convert.FromHexString(golden["relay_you_are_host_handoff"])).Payload.Span);
        Check(hand.Epoch == 1 && hand.Source == RelayStateSource.Handoff && hand.SnapshotSeq == 0 && hand.Members.Count == 2 && hand.Members[1].Online &&
              Hex(hand.State!) == "0068", "you_are_host (handoff) payload");
        // 压缩方式标识：1（raw deflate）哪种 Compressor 都解得开；0x7f 没有分配。
        var deflated = RelayCodec.DecodeYouAreHost(RelayCodec.Decode(Convert.FromHexString(golden["relay_you_are_host_deflate"])).Payload.Span).State!;
        foreach (var compressor in new[] { RelayCompressors.Deflate, RelayCompressors.None })
            Check(Encoding.ASCII.GetString(RelayCompressors.Unpack(compressor, deflated, 1 << 20)) == "hi", "deflate-marked golden state with compressor " + compressor.Id);
        var unknown = RelayCodec.DecodeYouAreHost(RelayCodec.Decode(Convert.FromHexString(golden["relay_you_are_host_unknown_compression"])).Payload.Span).State!;
        try { RelayCompressors.Unpack(RelayCompressors.Deflate, unknown, 1 << 20); throw new Exception("Check failed: unknown marker accepted"); }
        catch (RelayUnknownCompressionException error) { Check(error.Method == 0x7f, "unknown marker 0x7f"); }
        var changedFrame = RelayCodec.Decode(Convert.FromHexString(golden["relay_host_changed"]));
        Check(RelayCodec.DecodeHostChanged(changedFrame.Slot, changedFrame.Payload.Span).Equals(new RelayHostEvent(2, 3, "a3")), "host_changed payload");
        Check(RelayCodec.DecodeRejected(RelayCodec.Decode(Convert.FromHexString(golden["relay_rejected_snapshot_too_soon"])).Payload.Span)
              .Equals(new RelayRejected(RelayOps.Snapshot, RelayRejectCode.TooSoon, 8)), "rejected payload");
        Check(RelayCodec.DecodeEpoch(RelayCodec.Decode(Convert.FromHexString(golden["relay_you_are_member"])).Payload.Span) == 2, "you_are_member payload");
        Refuses(() => RelayCodec.DecodeYouAreHost(new byte[22]), "short you_are_host");
        Refuses(() => RelayCodec.DecodeEpoch(new byte[7]), "short epoch");

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

    // 不认识的控制帧操作码（127 没有分配）丢弃、连接不断；注册了 OnControl 的交给它。
    // 用本地 TcpListener 扮 relay：收令牌后一次写出 Welcome + 操作码 127 + 一帧游戏帧。
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
                .Concat(RelayCodec.Encode(new RelayFrame(RelayChannel.Control, 2, 127, new byte[] { 7 })))
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
        client.OnControl(127, unknown.Enqueue);
        var welcome = await client.ConnectAsync(new RoomTicket(new RoomInfo(), "127.0.0.1:" + port, "rt1.fake", 0));
        Check(welcome.Equals(new RelayWelcome(2, false, 1)), "fake relay welcome");
        var deadline = DateTime.UtcNow.AddSeconds(5);
        while (games.IsEmpty && DateTime.UtcNow < deadline) { client.Pump(); await Task.Delay(5); }
        Check(unknown.Count == 1 && games.Count == 1 && client.IsConnected, "op 127 handed to OnControl, the next game frame delivered, connection kept");
        client.Dispose();
        Check(await fake == "rt1.fake", "token frame carries the relay token");
        listener.Stop();
    }

    private static void Compressors()
    {
        var state = Encoding.ASCII.GetBytes(string.Concat(Enumerable.Repeat("seed=42 deck=[7,1,9] ", 200)));
        foreach (var compressor in new[] { RelayCompressors.Deflate, RelayCompressors.None })
        {
            var packed = compressor.Compress(state);
            Check(compressor.Decompress(packed, state.Length).SequenceEqual(state), "compressor round trip");
            Refuses(() => compressor.Decompress(packed, state.Length - 1), "decompress past the limit");
        }
        Check(RelayCompressors.Deflate.Compress(state).Length < state.Length / 4, "deflate shrinks a repetitive state");
        // 与 Go 的 compress/flate 互通：这是 Go relay.Deflate.Compress([]byte(strings.Repeat("hello ", 50))) 的输出。
        Check(Encoding.ASCII.GetString(RelayCompressors.Deflate.Decompress(Convert.FromHexString("ca48cdc9c95718258921010300"), 1000)) ==
              string.Concat(Enumerable.Repeat("hello ", 50)), "inflates Go deflate output");

        // 压缩方式标识（RELAY.md §2.3）：[Id][压缩后的字节]，空状态不带标识；内置的两种谁都解得开；不认识的标识是
        // RelayUnknownCompressionException，标识对、内容坏是解压错误（InvalidDataException，但不是“不认识”）。
        var custom = new MarkerCompressor(9);
        Check(RelayCompressors.Pack(RelayCompressors.Deflate, state)[0] == 1 && RelayCompressors.Pack(RelayCompressors.None, state)[0] == 0 &&
              RelayCompressors.Pack(custom, state)[0] == 9 && RelayCompressors.Pack(RelayCompressors.Deflate, ReadOnlySpan<byte>.Empty).Length == 0, "markers");
        Check(RelayCompressors.Unpack(custom, RelayCompressors.Pack(RelayCompressors.Deflate, state), 1 << 20).SequenceEqual(state), "a custom compressor still decodes deflate (1)");
        Check(RelayCompressors.Unpack(RelayCompressors.Deflate, RelayCompressors.Pack(RelayCompressors.None, state), 1 << 20).SequenceEqual(state), "deflate decodes none (0)");
        Check(RelayCompressors.Unpack(custom, RelayCompressors.Pack(custom, state), 1 << 20).SequenceEqual(state), "custom round trip");
        try { RelayCompressors.Unpack(RelayCompressors.Deflate, RelayCompressors.Pack(custom, state), 1 << 20); throw new Exception("Check failed: mixed compressors accepted"); }
        catch (RelayUnknownCompressionException error) { Check(error.Method == 9, "mixed compressors: unknown method 9"); }
        try { RelayCompressors.Unpack(RelayCompressors.Deflate, new byte[] { 1, 0xFF, 0xFF, 0xFF }, 1 << 20); throw new Exception("Check failed: corrupt deflate accepted"); }
        catch (InvalidDataException) { checks++; } // 解压错误；“不认识”是另一个类型（RelayUnknownCompressionException），不会落到这里

        // README 的 zstd 示例：用接口桩（原样拷贝）走一遍 Pack / Unpack；只认 deflate 的客户端收到它是“不认识的压缩方式”。
        var zstd = new ZstdRelayCompressor(new CopyCodec());
        var options = new RelayClientOptions { Game = "party", Compressor = zstd };
        Check(options.Compressor.Id == 2 && RelayCompressors.Unpack(zstd, RelayCompressors.Pack(zstd, state), 1 << 20).SequenceEqual(state), "README zstd example round trip");
        try { RelayCompressors.Unpack(RelayCompressors.Deflate, RelayCompressors.Pack(zstd, state), 1 << 20); throw new Exception("Check failed: zstd state accepted by a deflate-only client"); }
        catch (RelayUnknownCompressionException error) { Check(error.Method == 2, "zstd state at a deflate-only client: unknown method 2"); }
        Refuses(() => zstd.Decompress(state, state.Length - 1), "README zstd example past the limit");
    }

    /// <summary>IZstdCodec 的桩：原样拷贝（测试只验接线与上限）。</summary>
    private sealed class CopyCodec : IZstdCodec
    {
        public byte[] Wrap(ReadOnlySpan<byte> data) => data.ToArray();
        public byte[] Unwrap(ReadOnlySpan<byte> data, int maxOutput) => data.Slice(0, Math.Min(data.Length, maxOutput)).ToArray();
    }

    /// <summary>游戏自带压缩方式的替身：不改字节，只换标识。</summary>
    private sealed class MarkerCompressor : IRelayCompressor
    {
        public MarkerCompressor(byte id) { Id = id; }
        public byte Id { get; }
        public byte[] Compress(ReadOnlySpan<byte> data) => data.ToArray();
        public byte[] Decompress(ReadOnlySpan<byte> data, int limit) => RelayCompressors.None.Decompress(data, limit);
    }

    /// <summary>
    /// 换 host 的 13 条金样走 SDK 自己的路径：本地 TcpListener 扮 relay。
    /// 第一条连接：relay 先让客户端当上房主（Welcome 成员 → YouAreHost{epoch 0, 快照 seq 6} → HostChanged 槽位是自己），客户端经
    /// Transfer / SetSuccessors / Snapshot / HostReady / LeaveAsync 发出的字节与金样相同（Compressor 用 None；快照 seq 从 6 接到 7）。
    /// 第二条连接：relay 逐帧发金样里的换 host 帧，每帧之后核对经 Pump 收到的回调与 Role。
    /// </summary>
    private static async Task HostFramesAgainstFakeRelay(string path)
    {
        var golden = new Dictionary<string, string>();
        foreach (var item in JsonDocument.Parse(File.ReadAllText(path)).RootElement.EnumerateArray())
            golden[item.GetProperty("name").GetString()!] = item.GetProperty("hex").GetString()!;
        var listener = new System.Net.Sockets.TcpListener(System.Net.IPAddress.Loopback, 0);
        listener.Start();
        var port = ((System.Net.IPEndPoint)listener.LocalEndpoint).Port;
        var options = new RelayClientOptions { Game = Game, Compressor = RelayCompressors.None, SnapshotMinInterval = TimeSpan.FromTicks(1) };

        // ---- 发送 ----
        var sentNames = new[] { "relay_transfer", "relay_successors", "relay_snapshot", "relay_host_ready", "relay_presence", "relay_leave" };
        var fake = Task.Run(async () =>
        {
            using var socket = await listener.AcceptTcpClientAsync();
            var stream = socket.GetStream();
            await RelayCodec.ReadAsync(stream, 1024);
            var promote = RelayCodec.Encode(new RelayFrame(RelayChannel.Control, 3, RelayOps.Welcome, RelayCodec.EncodeWelcome(new RelayWelcome(3, false, 1))))
                .Concat(RelayCodec.Encode(new RelayFrame(RelayChannel.Control, 3, RelayOps.YouAreHost,
                    RelayCodec.EncodeYouAreHost(new RelayYouAreHost(0, RelayStateSource.Snapshot, 6, TimeSpan.Zero, Array.Empty<RelaySeat>(), new byte[] { 1 })))))
                .Concat(RelayCodec.Encode(new RelayFrame(RelayChannel.Control, 3, RelayOps.HostChanged, RelayCodec.EncodeHostChanged(0, "a3")))).ToArray();
            await stream.WriteAsync(promote);
            var got = new List<string>();
            while (got.Count < sentNames.Length)
            {
                var frame = await RelayCodec.ReadAsync(stream, 1 << 20);
                if (frame.Channel == RelayChannel.Control && frame.MessageId == RelayOps.Ping) continue; // 房主心跳
                got.Add(Hex(RelayCodec.Encode(frame)));
            }
            return got;
        });
        var client = new RelayClient(null, options);
        RelayHostEvent? promoted = null;
        client.OnHostChanged(e => promoted = e);
        await client.ConnectAsync(new RoomTicket(new RoomInfo(), "127.0.0.1:" + port, "rt1.fake", 0));
        var deadline = DateTime.UtcNow.AddSeconds(5);
        while (promoted == null && DateTime.UtcNow < deadline) { client.Pump(); await Task.Delay(5); }
        Check(client.Role.Equals(new RelayRole(3, true, false, 3, 0)) && client.IsHost, "fake relay made the client host: " + client.Role);
        Check(client.Transfer(3, Encoding.ASCII.GetBytes("handoff")), "transfer sent");
        Check(client.SetSuccessors(new uint[] { 3, 2 }), "successors sent");
        Check(client.Snapshot(new byte[] { 0xAB, 0xCD }) == RelaySnapshotResult.Sent, "snapshot sent");
        Check(client.HostReady(2), "host ready sent");
        Check(client.SetForeground(true), "presence sent");
        await client.LeaveAsync();
        var sent = await fake;
        for (int i = 0; i < sentNames.Length; i++)
            Check(sent[i] == golden[sentNames[i]], $"C# SDK sends {sentNames[i]} like Go ({sent[i]} vs {golden[sentNames[i]]})");

        // ---- 接收 ----
        var steps = new (string name, RelayRole role)[]
        {
            ("relay_welcome", new RelayRole(3, false, false, 1, 5)),
            ("relay_member_presence", new RelayRole(3, false, false, 1, 5)),
            ("relay_host_resumed", new RelayRole(3, false, false, 1, 0)),
            ("relay_host_changing", new RelayRole(3, false, false, 1, 0)),
            ("relay_you_are_host_snapshot", new RelayRole(3, false, true, 0, 2)),
            ("relay_host_changed", new RelayRole(3, true, false, 3, 2)),
            ("relay_rejected_snapshot_too_soon", new RelayRole(3, true, false, 3, 2)),
            ("relay_you_are_host_handoff", new RelayRole(2, false, true, 0, 1)),
            ("relay_you_are_member", new RelayRole(2, false, false, 0, 2)),
            ("relay_you_are_host_deflate", new RelayRole(3, false, true, 0, 2)),
            ("relay_you_are_host_unknown_compression", new RelayRole(3, false, true, 0, 2)),
        };
        var next = new SemaphoreSlim(0);
        var feeder = Task.Run(async () =>
        {
            using var socket = await listener.AcceptTcpClientAsync();
            var stream = socket.GetStream();
            await RelayCodec.ReadAsync(stream, 1024);
            foreach (var step in steps)
            {
                await stream.WriteAsync(Convert.FromHexString(golden[step.name]));
                await next.WaitAsync();
            }
            await stream.WriteAsync(Convert.FromHexString(golden["relay_closed_lobby_rejected"]));
            try { while (await stream.ReadAsync(new byte[64]) > 0) { } } catch (IOException) { }
        });
        using var receiver = new RelayClient(null, options);
        var seen = new ConcurrentQueue<string>();
        RelayYouAreHost? lastOffer = null;
        var disconnects = new ConcurrentQueue<RelayDisconnect>();
        receiver.OnHostResumed(e => seen.Enqueue("resumed " + e));
        receiver.OnHostChanging(e => seen.Enqueue("changing " + e));
        receiver.OnYouAreHost(e => { lastOffer = e; seen.Enqueue("offer " + e.Epoch + " " + e.Source); });
        receiver.OnHostChanged(e => seen.Enqueue("changed " + e));
        receiver.OnRejected(e => seen.Enqueue("rejected " + e));
        receiver.OnYouAreMember(e => seen.Enqueue("member " + e));
        receiver.OnMemberPresence(e => seen.Enqueue("presence " + e));
        receiver.Disconnected += disconnects.Enqueue;
        var welcome = await receiver.ConnectAsync(new RoomTicket(new RoomInfo(), "127.0.0.1:" + port, "rt1.fake", 0));
        var expected = new[]
        {
            "", "presence " + new RelayMemberPresence(3, false), "resumed " + new RelayHostEvent(0, 1, ""), "changing 2", "offer 2 Snapshot", "changed " + new RelayHostEvent(2, 3, "a3"),
            "rejected " + new RelayRejected(RelayOps.Snapshot, RelayRejectCode.TooSoon, 8), "offer 1 Handoff", "member 2", "offer 2 Snapshot", "offer 2 Snapshot",
        };
        for (int i = 0; i < steps.Length; i++)
        {
            if (expected[i] != "")
            {
                var until = DateTime.UtcNow.AddSeconds(5);
                string? item = null;
                while (!seen.TryDequeue(out item) && DateTime.UtcNow < until) { receiver.Pump(); await Task.Delay(2); }
                Check(item == expected[i], $"after {steps[i].name}: callback {item} == {expected[i]}");
            }
            else Check(welcome.Equals(new RelayWelcome(3, false, 1, 5)), "fake welcome (epoch 5)");
            Check(receiver.Role.Equals(steps[i].role), $"after {steps[i].name}: role {receiver.Role} == {steps[i].role}");
            if (steps[i].name == "relay_you_are_host_snapshot")
                Check(lastOffer!.SnapshotSeq == 7 && lastOffer.SnapshotAge == TimeSpan.FromMilliseconds(1500) && Hex(lastOffer.State!) == "abcd" && lastOffer.StateError == null, "snapshot offer fields");
            if (steps[i].name == "relay_you_are_host_deflate")
                Check(Encoding.ASCII.GetString(lastOffer!.State!) == "hi" && lastOffer.StateError == null, "deflate-marked offer decodes with Compressor None");
            if (steps[i].name == "relay_you_are_host_unknown_compression")
                Check(lastOffer!.State == null && lastOffer.StateError is RelayUnknownCompressionException, "unknown-marked offer: " + lastOffer.StateError);
            next.Release();
        }
        var end = DateTime.UtcNow.AddSeconds(5);
        RelayDisconnect? last = null;
        while (!disconnects.TryDequeue(out last) && DateTime.UtcNow < end) { receiver.Pump(); await Task.Delay(2); }
        Check(last != null && last.Reason == RelayReason.LobbyRejected && !last.Reconnecting, "closed lobby_rejected ends the connection");
        receiver.Dispose();
        await feeder;
        listener.Stop();
    }

    // ---------- 真实对端 ----------

    private static async Task LobbyErrors(Peer peer)
    {
        var bad = new LobbyClient(peer.LobbyUrl, "gt2.not-issued");
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

        // 踢人：被踢者收到 Closed{kicked}、不重连；房主收到 MemberLeft{kicked}。
        Check(host.Client.Kick(3), "kick");
        var kicked = await m2.Next(m2.Disconnected, "kicked");
        Check(kicked.Reason == RelayReason.Kicked && !kicked.Reconnecting && kicked.Error is RelayClosedException, "kicked disconnect");
        var gone = await host.Next(host.Left, "kicked member left");
        Check(gone.Slot == 3 && gone.Account == "cs-m2" && gone.Reason == RelayReason.Kicked, "member left (kicked)");
        await m2.Client.Completion;
        // 被踢记在 lobby（relay 报告 member_left{kicked}）：被踢的账号再加入，lobby 的 join 答 403 kicked。
        await Until("lobby records the kick", async () =>
        {
            try { await m2.Lobby.JoinRoomAsync(ticket.Room.Id, new JoinRoomRequest { Game = Game, Password = "pw" }); return false; }
            catch (LobbyException error) { return error.Code == LobbyErrorCode.Kicked; }
        });
        var again = await Throws<LobbyException>(() => m2.Client.RejoinAsync());
        Check(again.Code == LobbyErrorCode.Kicked && again.Status == 403 && again.ErrorName == "kicked", "kicked account: lobby join → 403 kicked");
        // 拿还没过期的旧令牌直接连 relay：relay 本地记着被踢，Closed{kicked}。
        using (var stale = new RelayClient(null, new RelayClientOptions { Game = Game }))
            Check((await Throws<RelayClosedException>(() => stale.ConnectAsync(m2.Ticket!))).Reason == RelayReason.Kicked, "kicked account's old token → Closed{kicked}");

        // 离开：先在 relay 上发 Leave，再经 lobby 让出席位；房主收到 MemberLeft{left}。
        await m1.Client.LeaveAsync();
        var leftViaLobby = await host.Next(host.Left, "member left");
        Check(leftViaLobby.Slot == 2 && leftViaLobby.Reason == RelayReason.Left, "member left (left)");
        // 席位让出后拿还没过期的旧令牌连 relay：令牌验签通过，lobby 验权不让进，Closed{lobby_rejected}。
        using (var stale = new RelayClient(null, new RelayClientOptions { Game = Game }))
            Check((await Throws<RelayClosedException>(() => stale.ConnectAsync(m1.Ticket!))).Reason == RelayReason.LobbyRejected, "left member's old token → Closed{lobby_rejected}");

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

        // 房主离开（不等宽限期）：成员被选为候选；它不回 HostReady，host_ready_timeout（10s）后收到 YouAreMember，没有别的候选就关房；
        // 成员收到 room_closed，开着自动重连也不重连。
        await host.Client.LeaveAsync();
        var offer = await member.Next(member.YouAreHost, "you are host");
        Check(offer.Epoch == 1 && offer.Source == RelayStateSource.None && member.Client.Role.IsCandidate, "member offered the host role");
        Check(await member.Next(member.YouAreMember, "candidate timed out", 15) == 1 && !member.Client.Role.IsCandidate, "candidate timed out → you are member");
        var closed = await member.Next(member.Disconnected, "room closed");
        Check(closed.Reason == RelayReason.RoomClosed && !closed.Reconnecting, "room closed → no reconnect");
        await member.Client.Completion;
        Check(!member.Client.IsConnected, "member client finished");
    }

    // ---------- 换 host（真实对端；relay 用缺省配置：host_idle_timeout 2s、host_grace 5s、snapshot_min_interval 1s） ----------

    private static async Task<(Player host, Player[] members, string room)> OpenRoom(Peer peer, string prefix, RelayClientOptions hostOptions, params RelayClientOptions[] memberOptions)
    {
        var host = await Player.CreateAsync(peer, prefix + "-host", hostOptions);
        var ticket = await host.Lobby.CreateRoomAsync(new CreateRoomRequest { Game = Game, Mode = "host" });
        await host.Client.ConnectAsync(ticket);
        var members = new Player[memberOptions.Length];
        for (int i = 0; i < members.Length; i++)
        {
            members[i] = await Player.CreateAsync(peer, $"{prefix}-m{i + 2}", memberOptions[i]);
            var welcome = await members[i].JoinAsync(ticket.Room.Id, null);
            Check(welcome.Slot == i + 2, $"{prefix} member slot {welcome.Slot}");
            await host.Next(host.Joined, "member joined");
        }
        return (host, members, ticket.Room.Id);
    }

    private static RelayClientOptions Opts(Action<RelayClientOptions>? edit = null)
    {
        var options = new RelayClientOptions { Game = Game };
        edit?.Invoke(options);
        return options;
    }

    private static void Dispose(Player host, Player[] members)
    {
        host.Dispose();
        foreach (var member in members) member.Dispose();
    }

    /// <summary>发一个 Ping 并等它的 Pong（同一连接按序：之前发出的控制帧都已被 relay 处理）。</summary>
    private static async Task WaitPong(Player player)
    {
        var got = new ConcurrentQueue<bool>();
        player.Client.OnControl(RelayOps.Pong, frame => { if (frame.Payload.Length == 1 && frame.Payload.Span[0] == 9) got.Enqueue(true); });
        Check(player.Client.SendControl(RelayOps.Ping, 0, new byte[] { 9 }), "ping sent");
        await player.Next(got, "pong");
        player.Client.OnControl(RelayOps.Pong, null);
    }

    // 主动转交：新房主拿到交接数据，旧房主收到 YouAreMember，迁移期间成员的游戏帧被丢并收到一次 HostChanging，HostReady 后全体收到
    // HostChanged，角色随之更新；新房主的快照 seq 接在旧房主之上（主动转交的 YouAreHost 不带 seq）。
    private static async Task Transfer(Peer peer)
    {
        var (host, members, _) = await OpenRoom(peer, "cs-xfer", Opts(), Opts(), Opts());
        var (a, b) = (members[0], members[1]);
        Check(host.Client.Role.Equals(new RelayRole(1, true, false, 1, 0)), "creator is host");
        Check(host.Client.Snapshot(Encoding.UTF8.GetBytes("old-host-state")) == RelaySnapshotResult.Sent, "old host snapshot");
        await Throws<InvalidOperationException>(() => { a.Client.Transfer(3, new byte[] { 1 }); return Task.CompletedTask; });
        var handoff = Encoding.UTF8.GetBytes("handoff: seed=42, deck=[7,1,9]");
        Check(host.Client.Transfer(3, handoff), "transfer");
        Check(await host.Next(host.YouAreMember, "old host demoted") == 1, "old host: you are member 1");
        Check(host.Client.Role.Equals(new RelayRole(1, false, false, 0, 1)), "old host role " + host.Client.Role);
        var offer = await b.Next(b.YouAreHost, "you are host");
        Check(offer.Epoch == 1 && offer.Source == RelayStateSource.Handoff && offer.State!.SequenceEqual(handoff) && offer.StateError == null && offer.Members.Count == 3 &&
              offer.Members[0].Equals(new RelaySeat(1, "cs-xfer-host", true)), "handoff offer");
        Check(b.Client.Role.Equals(new RelayRole(3, false, true, 0, 1)), "candidate role " + b.Client.Role);
        a.Client.SendGame(RelayCodec.All, 1, Encoding.UTF8.GetBytes("lost"));
        a.Client.SendGame(RelayCodec.All, 1, Encoding.UTF8.GetBytes("lost"));
        Check(await a.Next(a.HostChanging, "host changing") == 1, "host changing epoch 1");
        Check(b.Client.HostReady(offer.Epoch), "host ready");
        foreach (var p in new[] { host, a, b })
            Check((await p.Next(p.HostChanged, "host changed")).Equals(new RelayHostEvent(1, 3, "cs-xfer-m3")), "host changed at " + p.Client.Role.Slot);
        Check(a.HostChanging.IsEmpty, "one host changing per change");
        Check(b.Client.Role.Equals(new RelayRole(3, true, false, 3, 1)) && a.Client.Role.Equals(new RelayRole(2, false, false, 3, 1)) &&
              host.Client.Role.Equals(new RelayRole(1, false, false, 3, 1)), "roles after host changed");
        Check(b.Game.IsEmpty, "game frames dropped during the migration");
        a.Client.SendGame(RelayCodec.All, 2, Encoding.UTF8.GetBytes("after"));
        Frame(await b.Next(b.Game, "game frame at the new host"), RelayChannel.Game, 2, 2, "after");
        Check(b.Client.Snapshot(Encoding.UTF8.GetBytes("new-host-state")) == RelaySnapshotResult.Sent, "new host snapshot");
        await WaitPong(b);
        Player.PumpAll();
        Check(b.Rejected.IsEmpty, "new host snapshot accepted (seq continues above the old host)");
        await Throws<InvalidOperationException>(() => { host.Client.Kick(2); return Task.CompletedTask; });
        Dispose(host, members);
    }

    // 房主断线：宽限期内成员的游戏帧被丢、收到 HostChanging{0}；期满后 relay 按声明的候选顺序（槽位 3）把快照交给候选，候选自动回 HostReady。
    private static async Task HostLoss(Peer peer)
    {
        var (host, members, _) = await OpenRoom(peer, "cs-loss", Opts(), Opts(), Opts(o => o.AutoHostReady = true));
        var (a, b) = (members[0], members[1]);
        var state = Encoding.UTF8.GetBytes("only-the-host-knows: rng=123456789");
        Check(host.Client.Snapshot(state) == RelaySnapshotResult.Sent, "snapshot");
        Check(host.Client.SetSuccessors(new uint[] { 3 }), "successors");
        await Throws<InvalidOperationException>(() => { a.Client.SetSuccessors(new uint[] { 2 }); return Task.CompletedTask; });
        await WaitPong(host);
        host.Client.AbortTransportForTest(); // 断网，relay 读到 EOF
        await Until("grace period started", () =>
        {
            a.Client.SendGame(RelayCodec.All, 1, Encoding.UTF8.GetBytes("during grace"));
            return Task.FromResult(a.HostChanging.TryDequeue(out var epoch) && epoch == 0);
        });
        var offer = await b.Next(b.YouAreHost, "you are host after the grace period", 10);
        Check(offer.Epoch == 1 && offer.Source == RelayStateSource.Snapshot && offer.SnapshotSeq == 1 && offer.State!.SequenceEqual(state) &&
              offer.SnapshotAge < TimeSpan.FromSeconds(8) && !offer.Members[0].Online, "snapshot offer");
        Check(a.YouAreHost.IsEmpty, "undeclared member was not offered");
        foreach (var p in new[] { a, b })
            Check((await p.Next(p.HostChanged, "host changed")).Equals(new RelayHostEvent(1, 3, "cs-loss-m3")), "host changed");
        Check(b.Client.Role.Equals(new RelayRole(3, true, false, 3, 1)) && a.Client.Role.Equals(new RelayRole(2, false, false, 3, 1)), "roles after loss");
        Dispose(host, members);
    }

    // 宽限期内房主（自动重连）用同一账号回来：仍是房主，成员收到 HostResumed，房主再收到在线成员的 MemberJoined。
    private static async Task HostResumed(Peer peer)
    {
        var (host, members, _) = await OpenRoom(peer, "cs-resume", Opts(o =>
        {
            o.ReconnectAttempts = 5; o.ReconnectMinBackoff = TimeSpan.FromMilliseconds(20); o.ReconnectMaxBackoff = TimeSpan.FromMilliseconds(100);
        }), Opts());
        var a = members[0];
        host.Client.AbortTransportForTest();
        Check((await host.Next(host.Disconnected, "host transport lost")).Reconnecting, "host reconnecting");
        var back = await host.Next(host.Reconnected, "host reconnected");
        Check(back.IsHost && back.Slot == 1 && back.HostSlot == 1, "host welcome after resume");
        Check((await a.Next(a.HostResumed, "host resumed")).Equals(new RelayHostEvent(0, 1, "")), "host resumed event");
        var again = await host.Next(host.Joined, "online member told again");
        Check(again.Slot == 2 && again.Account == "cs-resume-m2", "member joined again");
        Check(host.Client.Role.Equals(new RelayRole(1, true, false, 1, 0)) && a.Client.Role.Equals(new RelayRole(2, false, false, 1, 0)), "roles after resume");
        Check(a.YouAreHost.IsEmpty, "no host change");
        a.Client.SendGame(RelayCodec.All, 5, Encoding.UTF8.GetBytes("still here"));
        Frame(await host.Next(host.Game, "game frame after resume"), RelayChannel.Game, 2, 5, "still here");
        Dispose(host, members);
    }

    // 房主离开（LeaveAsync：relay 上的 Leave 加 lobby 的 leave）不等宽限期；声明的候选顺序 [4, 2] 生效；没有快照时来源是 None。
    private static async Task LeaveWithSuccessors(Peer peer)
    {
        var (host, members, _) = await OpenRoom(peer, "cs-leave", Opts(), Opts(), Opts(), Opts());
        Check(host.Client.SetSuccessors(new uint[] { 4, 2 }), "successors");
        await WaitPong(host);
        await host.Client.LeaveAsync();
        var offer = await members[2].Next(members[2].YouAreHost, "you are host at slot 4");
        Check(offer.Epoch == 1 && offer.Source == RelayStateSource.None && offer.State == null && offer.Members.Count == 4, "leave offer");
        Check(members[0].YouAreHost.IsEmpty, "slot 2 was not offered");
        Dispose(host, members);
    }

    // 快照节奏：SDK 不发太密的（TooSoon）；SDK 的最小间隔比 relay 小时 relay 回 Rejected{TooSoon, seq}；转交给不在线的槽位回
    // Rejected{BadTarget}；成员不能发；压缩后太大的抛 ArgumentException。
    private static async Task SnapshotPacing(Peer peer)
    {
        var (host, members, _) = await OpenRoom(peer, "cs-pace", Opts(), Opts());
        Check(host.Client.Snapshot(new byte[] { 1 }) == RelaySnapshotResult.Sent, "first snapshot");
        Check(host.Client.Snapshot(new byte[] { 2 }) == RelaySnapshotResult.TooSoon, "second snapshot within a second → TooSoon");
        await Throws<InvalidOperationException>(() => { members[0].Client.Snapshot(new byte[] { 3 }); return Task.CompletedTask; });
        Check(host.Client.Transfer(9, new byte[] { 4 }), "transfer to nobody");
        Check((await host.Next(host.Rejected, "bad target")).Equals(new RelayRejected(RelayOps.Transfer, RelayRejectCode.BadTarget, 0)), "rejected bad target");
        var big = new byte[300 << 10];
        new Random(7).NextBytes(big);
        await Throws<ArgumentException>(() => { host.Client.Snapshot(big); return Task.CompletedTask; });
        Dispose(host, members);

        using var eager = await Player.CreateAsync(peer, "cs-pace-eager", Opts(o => o.SnapshotMinInterval = TimeSpan.FromMilliseconds(1)));
        await eager.Client.ConnectAsync(await eager.Lobby.CreateRoomAsync(new CreateRoomRequest { Game = Game, Mode = "eager" }));
        Check(eager.Client.Snapshot(new byte[] { 1 }) == RelaySnapshotResult.Sent, "eager first");
        await Task.Delay(5);
        Check(eager.Client.Snapshot(new byte[] { 2 }) == RelaySnapshotResult.Sent, "eager second (SDK lets it through)");
        Check((await eager.Next(eager.Rejected, "too soon at the relay")).Equals(new RelayRejected(RelayOps.Snapshot, RelayRejectCode.TooSoon, 2)), "relay rejects too soon");
    }

    // 定时快照：当房主时 SDK 在 Pump 里按 SnapshotInterval 向游戏要状态、压缩后推；房主离开后候选拿到的是最近一份，seq 与推的次数一致；
    // 候选在回 HostReady 之前不推。
    private static async Task AutoSnapshot(Peer peer)
    {
        int pushed = 0, candidatePushed = 0;
        Func<byte[]?> state = () => Encoding.UTF8.GetBytes("auto-" + Interlocked.Increment(ref pushed));
        Func<byte[]?> candidateState = () => { Interlocked.Increment(ref candidatePushed); return Encoding.UTF8.GetBytes("candidate"); };
        var (host, members, _) = await OpenRoom(peer, "cs-auto",
            Opts(o => { o.SnapshotState = state; o.SnapshotInterval = TimeSpan.FromMilliseconds(1100); }),
            Opts(o => { o.SnapshotState = candidateState; o.SnapshotInterval = TimeSpan.FromMilliseconds(100); }));
        await Until("two automatic snapshots", () => Task.FromResult(Volatile.Read(ref pushed) >= 2));
        await WaitPong(host);
        await host.Client.LeaveAsync();
        int n = Volatile.Read(ref pushed);
        var offer = await members[0].Next(members[0].YouAreHost, "you are host");
        Check(offer.Source == RelayStateSource.Snapshot && offer.SnapshotSeq >= 2 && (int)offer.SnapshotSeq <= n &&
              Encoding.UTF8.GetString(offer.State!) == "auto-" + offer.SnapshotSeq, $"auto snapshot offer seq {offer.SnapshotSeq} (pushed {n})");
        var until = DateTime.UtcNow.AddMilliseconds(1500);
        while (DateTime.UtcNow < until) { Player.PumpAll(); await Task.Delay(20); }
        Check(Volatile.Read(ref candidatePushed) == 0, "candidate does not push before HostReady");
        Check(members[0].Client.HostReady(offer.Epoch), "candidate host ready");
        await Until("the new host pushes snapshots", () => Task.FromResult(Volatile.Read(ref candidatePushed) >= 1));
        Dispose(host, members);
    }

    // 房主心跳：relay 的 host_idle_timeout 是缺省的 2s，房主不发游戏帧只靠 SDK 的心跳（缺省 600ms），5s 以上不被当成断线。
    private static async Task HostHeartbeat(Peer peer)
    {
        var (host, members, _) = await OpenRoom(peer, "cs-beat", Opts(), Opts());
        var a = members[0];
        var until = DateTime.UtcNow.AddMilliseconds(5500);
        while (DateTime.UtcNow < until) { Player.PumpAll(); await Task.Delay(50); }
        Check(host.Disconnected.IsEmpty && a.HostChanging.IsEmpty && a.YouAreHost.IsEmpty && a.HostChanged.IsEmpty, "no host change for 5.5s");
        a.Client.SendGame(RelayCodec.All, 1, Encoding.UTF8.GetBytes("ping the host"));
        Frame(await host.Next(host.Game, "game frame at the host"), RelayChannel.Game, 2, 1, "ping the host");
        Check(a.HostChanging.IsEmpty && host.Client.Role.Equals(new RelayRole(1, true, false, 1, 0)), "host still host");
        Dispose(host, members);
    }

    // 自动重连遇到 relay 向 lobby 验权失败：lobby_unavailable 按退避一直试，恢复后连回来；lobby_rejected 只试一次就停。
    private static async Task ReconnectAdmission(Peer peer)
    {
        var (host, members, _) = await OpenRoom(peer, "cs-admit", Opts(), Opts(o =>
        {
            o.ReconnectAttempts = 50; o.ReconnectMinBackoff = TimeSpan.FromMilliseconds(20); o.ReconnectMaxBackoff = TimeSpan.FromMilliseconds(40);
        }));
        var m = members[0];
        try
        {
            await peer.SetAdmissionAsync("down");
            int joins = m.Joins;
            m.Client.AbortTransportForTest();
            Check((await m.Next(m.Disconnected, "transport lost")).Reconnecting, "reconnecting");
            await Until("several attempts while the lobby is down", () => Task.FromResult(m.Joins - joins >= 3));
            Check(m.Disconnected.IsEmpty, "not giving up while the lobby is down");
            await peer.SetAdmissionAsync("ok");
            Check((await m.Next(m.Reconnected, "reconnected")).Slot == 2, "reconnected after the lobby came back");

            await peer.SetAdmissionAsync("reject");
            joins = m.Joins;
            m.Client.AbortTransportForTest();
            Check((await m.Next(m.Disconnected, "transport lost again")).Reconnecting, "reconnecting again");
            var final = await m.Next(m.Disconnected, "final disconnect");
            Check(!final.Reconnecting && final.Reason == RelayReason.LobbyRejected && final.Error is RelayClosedException, "stopped on lobby_rejected: " + final);
            Check(m.Joins - joins == 1, $"one attempt after lobby_rejected ({m.Joins - joins})");
        }
        finally { await peer.SetAdmissionAsync("ok"); }
        Dispose(host, members);
    }

    // 开局后：成员掉线自动重连照常、回原槽位；关掉自动重连的成员自己 RejoinAsync 回原座；房主踢有座位但不在线的槽位，收到这个槽位的
    // 第二条 MemberLeft（Kicked），之后它的 join 答 403 kicked；新来的人 join 答 room_started。
    private static async Task StartedRoom(Peer peer)
    {
        var (host, members, room) = await OpenRoom(peer, "cs-started", Opts(), Opts(o =>
        {
            o.ReconnectAttempts = 5; o.ReconnectMinBackoff = TimeSpan.FromMilliseconds(20); o.ReconnectMaxBackoff = TimeSpan.FromMilliseconds(100);
        }), Opts());
        var (a, b) = (members[0], members[1]);
        var started = await host.Lobby.SetRoomStateAsync(room, new SetRoomStateRequest { Game = Game, State = RoomState.Started });
        Check(started.State == RoomState.Started, "room started");

        a.Client.AbortTransportForTest();
        Check((await a.Next(a.Disconnected, "a transport lost")).Reconnecting, "a reconnecting");
        var back = await a.Next(a.Reconnected, "a reconnected in a started room");
        Check(back.Slot == 2 && back.HostSlot == 1, "a back in slot 2");

        b.Client.AbortTransportForTest();
        Check(!(await b.Next(b.Disconnected, "b transport lost")).Reconnecting, "b without auto reconnect");
        await Until("host sees b offline", () =>
        {
            while (host.Left.TryDequeue(out var m)) if (m.Slot == 3) return Task.FromResult(m.Reason == RelayReason.Disconnected);
            return Task.FromResult(false);
        });
        await b.Client.Completion;
        var rejoined = await b.Client.RejoinAsync();
        Check(rejoined.Slot == 3, "b rejoined its seat in a started room");
        await Until("host sees b back", () =>
        {
            while (host.Joined.TryDequeue(out var m)) if (m.Slot == 3) return Task.FromResult(true);
            return Task.FromResult(false);
        });

        b.Client.AbortTransportForTest();
        await b.Next(b.Disconnected, "b transport lost again");
        var offline = await host.Next(host.Left, "b offline again");
        Check(offline.Slot == 3 && offline.Reason == RelayReason.Disconnected, "b offline again");
        Check(host.Client.Kick(3), "kick the offline seat");
        var kicked = await host.Next(host.Left, "offline seat kicked");
        Check(kicked.Slot == 3 && kicked.Account == "cs-started-m3" && kicked.Reason == RelayReason.Kicked, "second member left (kicked) for an offline seat");
        await b.Client.Completion;
        await Until("lobby records the kick", async () =>
        {
            try { await b.Lobby.JoinRoomAsync(room, new JoinRoomRequest { Game = Game }); return false; }
            catch (LobbyException error) { return error.Code == LobbyErrorCode.Kicked; }
        });

        var newcomer = new LobbyClient(peer.LobbyUrl, await peer.GameTokenAsync("cs-started-new"));
        Check((await Throws<LobbyException>(() => newcomer.JoinRoomAsync(room, new JoinRoomRequest { Game = Game }))).Code == LobbyErrorCode.RoomStarted, "newcomer → room_started");
        a.Client.SendGame(RelayCodec.All, 7, Encoding.UTF8.GetBytes("still in"));
        Frame(await host.Next(host.Game, "a frame after the kick"), RelayChannel.Game, 2, 7, "still in");
        Dispose(host, members);
    }

    // Welcome 带 epoch：换过 host 之后才进房的人、离线期间换了 host 又回来的人（RejoinAsync），Role.Epoch 都是房间此刻的 epoch。
    private static async Task WelcomeEpoch(Peer peer)
    {
        var (host, members, room) = await OpenRoom(peer, "cs-epoch", Opts(), Opts(), Opts());
        var (a, b) = (members[0], members[1]);
        a.Client.AbortTransportForTest(); // a 掉线（不自动重连），座位还在
        await a.Next(a.Disconnected, "a transport lost");
        await host.Next(host.Left, "a left");
        await a.Client.Completion;
        Check(host.Client.Transfer(3, ReadOnlySpan<byte>.Empty), "transfer to b");
        var offer = await b.Next(b.YouAreHost, "you are host");
        Check(b.Client.HostReady(offer.Epoch), "host ready");
        await host.Next(host.HostChanged, "old host sees host changed");
        var late = await Player.CreateAsync(peer, "cs-epoch-late", Opts());
        var welcome = await late.JoinAsync(room, null);
        Check(welcome.Equals(new RelayWelcome(4, false, 3, 1)), "late welcome " + welcome);
        Check(late.Client.Role.Equals(new RelayRole(4, false, false, 3, 1)), "late role " + late.Client.Role);
        var back = await a.Client.RejoinAsync();
        Check(back.Epoch == 1 && a.Client.Role.Equals(new RelayRole(2, false, false, 3, 1)), "rejoin role " + a.Client.Role);
        late.Dispose();
        Dispose(host, members);
    }

    // 前台上报：成员 SetForeground 经 relay 只到房主；换 host 后每个设过状态的人（含降为成员的旧房主）收到 HostChanged 自动重发，
    // 新房主拿到全部；成员自动重连成功后也重发。
    private static async Task Presence(Peer peer)
    {
        var (host, members, _) = await OpenRoom(peer, "cs-presence", Opts(), Opts(), Opts(o =>
        {
            o.ReconnectAttempts = 5; o.ReconnectMinBackoff = TimeSpan.FromMilliseconds(20); o.ReconnectMaxBackoff = TimeSpan.FromMilliseconds(100);
        }));
        var (a, b) = (members[0], members[1]);
        Check(host.Client.SetForeground(true) && a.Client.SetForeground(false) && b.Client.SetForeground(true), "presence sent");
        var first = new[] { await host.Next(host.Presence, "presence 1"), await host.Next(host.Presence, "presence 2") }.OrderBy(p => p.Slot).ToArray();
        Check(first.SequenceEqual(new[] { new RelayMemberPresence(2, false), new RelayMemberPresence(3, true) }), "presence at the first host: " + string.Join(", ", first));
        Player.PumpAll();
        Check(a.Presence.IsEmpty && b.Presence.IsEmpty, "members do not see each other's presence");
        Check(host.Client.Transfer(2, ReadOnlySpan<byte>.Empty), "transfer to a");
        var offer = await a.Next(a.YouAreHost, "you are host");
        Check(a.Client.HostReady(offer.Epoch), "host ready");
        var resent = new[] { await a.Next(a.Presence, "re-sent 1"), await a.Next(a.Presence, "re-sent 2") }.OrderBy(p => p.Slot).ToArray();
        Check(resent.SequenceEqual(new[] { new RelayMemberPresence(1, true), new RelayMemberPresence(3, true) }), "re-sent to the new host: " + string.Join(", ", resent));
        b.Client.AbortTransportForTest();
        await b.Next(b.Reconnected, "b reconnected");
        Check((await a.Next(a.Presence, "after b reconnected")).Equals(new RelayMemberPresence(3, true)), "presence after a reconnect");
        Dispose(host, members);
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
        public readonly ConcurrentQueue<RelayYouAreHost> YouAreHost = new ConcurrentQueue<RelayYouAreHost>();
        public readonly ConcurrentQueue<ulong> YouAreMember = new ConcurrentQueue<ulong>(), HostChanging = new ConcurrentQueue<ulong>();
        public readonly ConcurrentQueue<RelayHostEvent> HostChanged = new ConcurrentQueue<RelayHostEvent>(), HostResumed = new ConcurrentQueue<RelayHostEvent>();
        public readonly ConcurrentQueue<RelayRejected> Rejected = new ConcurrentQueue<RelayRejected>();
        public readonly ConcurrentQueue<RelayMemberPresence> Presence = new ConcurrentQueue<RelayMemberPresence>();
        /// <summary>最近一次 join 拿到的票据（测旧令牌）。</summary>
        public RoomTicket? Ticket;
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
            client.OnYouAreHost(YouAreHost.Enqueue);
            client.OnYouAreMember(YouAreMember.Enqueue);
            client.OnHostChanging(HostChanging.Enqueue);
            client.OnHostChanged(HostChanged.Enqueue);
            client.OnHostResumed(HostResumed.Enqueue);
            client.OnRejected(Rejected.Enqueue);
            client.OnMemberPresence(Presence.Enqueue);
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
            Ticket = ticket;
            return await Client.ConnectAsync(ticket);
        }

        public async Task<T> Next<T>(ConcurrentQueue<T> queue, string what, double seconds = 5)
        {
            var deadline = DateTime.UtcNow.AddSeconds(seconds);
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

        /// <summary>切换对端 relay 向 lobby 验权的结果：ok（照 lobby 的真实答复）、reject、down。</summary>
        public async Task SetAdmissionAsync(string mode)
        {
            var reply = await Http.PostAsync(LobbyUrl + "/harness/admission", new StringContent("{\"mode\":\"" + mode + "\"}", Encoding.UTF8, "application/json"));
            reply.EnsureSuccessStatusCode();
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

// ---- Roost.Client.Relay/README.md“接入自己的 zstd / LZ4”的示例代码：原样放在这里编一遍（SDK 不引第三方库，IZstdCodec 是接口桩）。----

/// <summary>代表游戏自带的 zstd 库（例如 ZstdSharp 的 Compressor / Decompressor）；SDK 不带它。</summary>
public interface IZstdCodec
{
    byte[] Wrap(ReadOnlySpan<byte> data);
    /// <summary>解压，至多解出 maxOutput 字节（库自己能限就在库里限）。</summary>
    byte[] Unwrap(ReadOnlySpan<byte> data, int maxOutput);
}

/// <summary>把它接成 IRelayCompressor。Id 取 2：0、1 归内置的 None 与 Deflate，LZ4 照样接、取 3。</summary>
public sealed class ZstdRelayCompressor : IRelayCompressor
{
    private readonly IZstdCodec codec;
    public ZstdRelayCompressor(IZstdCodec codec) { this.codec = codec; }
    public byte Id => 2;
    public byte[] Compress(ReadOnlySpan<byte> data) => codec.Wrap(data);
    public byte[] Decompress(ReadOnlySpan<byte> data, int limit)
    {
        var output = codec.Unwrap(data, limit + 1);
        if (output.Length > limit) throw new InvalidDataException($"Inflated relay state exceeds {limit} bytes.");
        return output;
    }
}
