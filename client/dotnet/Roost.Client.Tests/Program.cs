using System;
using System.Collections.Generic;
using System.IO;
using System.Net;
using System.Net.Sockets;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client;

internal static partial class Program
{
    private static void Check(bool ok, string message) { if (!ok) throw new Exception(message); }
    private static byte[] RawSync = Array.Empty<byte>();
    private static async Task Main(string[] args)
    {
        string path = args[0];
        foreach (var item in JsonDocument.Parse(File.ReadAllText(path)).RootElement.EnumerateArray())
        {
            byte[] data = Convert.FromHexString(item.GetProperty("hex").GetString()!);
            Packet packet = PacketCodec.Decode(data);
            Check(Convert.ToHexString(PacketCodec.Encode(packet)) == Convert.ToHexString(data), "cross-language re-encode");
            if (item.GetProperty("name").GetString() == "sync")
            {
                RawSync = packet.Payload.ToArray();
                var frame = SyncFrame.Decode(packet.Payload.Span);
                Check(packet.Kind == PayloadKind.Sync && packet.IsPush && frame.Epoch == 2 && frame.Tick == 3, "sync routing");
                var subject = SubjectUpdate.Decode(frame.Objects[0].Components[0].Data);
                Check(subject.SubjectId == 9007199254740993L && subject.Full && subject.Version == 3 && subject.Namespace == "avatar" && subject.Profile == "owner", "subject 64-bit identity");
            }
            if (item.GetProperty("name").GetString() == "lockstep")
            {
                var frames = LockstepCodec.DecodeBroadcast(packet.Payload.Span);
                var hash = new LockstepInputHasher(); foreach (var f in frames) hash.Apply(f);
                Check(hash.Value == ulong.Parse(item.GetProperty("hash").GetString()!), "Go/C# input chain hash");
                Check(frames.Count == 2 && frames[0].Inputs[1].Player == -1, "Go/C# signed seat");
            }
            if (item.GetProperty("name").GetString()!.StartsWith("lockstep_"))
            {
                var command = LockstepCommand.Decode(packet.Payload.Span);
                Check(Convert.ToHexString(command.Encode()) == Convert.ToHexString(packet.Payload.Span), "Go/C# command");
            }
            Console.WriteLine("PASS golden " + item.GetProperty("name").GetString());
            foreach (byte flags in new byte[] { 6, 7, 8, 128 })
            {
                byte[] bad = (byte[])data.Clone(); bad[3] = flags;
                Refuses(() => PacketCodec.Decode(bad), "unsupported flags");
            }
        }
        Refuses(() => PacketCodec.Decode(new byte[15]), "short header");
        Refuses(() => PacketCodec.Encode(new Packet(1, 1, new byte[5]), 4), "oversized payload");
        Refuses(() => PacketCodec.Encode(new Packet(1, 1, new byte[0], (PayloadKind)3)), "reserved kind");
        Refuses(() => SyncFrame.Decode(RawSync.AsSpan(0, RawSync.Length - 1)), "truncated sync");
        Console.WriteLine("PASS malformed/limits/lockstep");
        TestLockstep();
        if (args.Length == 3)
            await GeneratedServer(args[1], int.Parse(args[2]));
        else
            { await LocalSession(); await Capacity(); await LockstepGapRetry(); }
    }
    private static void Refuses(Action run, string why)
    {
        try { run(); } catch (InvalidDataException) { return; }
        throw new Exception("accepted " + why);
    }

    private static async Task GeneratedServer(string host, int port)
    {
        using var session = await TcpSession.ConnectAsync(host, port, "ticket", TimeSpan.FromSeconds(10));
        var calls = new List<Task<Packet>>();
        for (int i = 0; i < 32; i++)
        {
            int n = i;
            calls.Add(Task.Run(() => session.RequestAsync(42, 43, new byte[] { 8, (byte)(n + 1) }, TimeSpan.FromSeconds(10))));
        }
        var replies = await Task.WhenAll(calls);
        for (int i = 0; i < replies.Length; i++) Check(replies[i].Payload.Span[0] == 24 && replies[i].Payload.Span[1] == (byte)(i + 1), "generated PB response/correlation");
        // 第33次请求显式触发真实Runtime的Sync推送，避免以连接建立推断session已注册。
        await session.RequestAsync(42, 43, new byte[] { 8, 42 }, TimeSpan.FromSeconds(10));
        using var budget = new CancellationTokenSource(TimeSpan.FromSeconds(10));
        while (true)
        {
            budget.Token.ThrowIfCancellationRequested();
            if (session.TryDequeuePush(out var push))
            {
                Check(push!.IsPush && push.Kind == PayloadKind.Sync && push.MessageId == 10103, "raw sync push");
                Check(SyncFrame.Decode(push.Payload.Span).Tick == 3, "generated raw sync decode");
                break;
            }
            await Task.Yield();
        }
        Console.WriteLine("PASS generated TCP auth/PB/concurrent sequence/raw Sync");
        await GeneratedLockstep(session);
    }

    private static async Task LocalSession()
    {
        var listener = new TcpListener(IPAddress.Loopback, 0); listener.Start();
        int port = ((IPEndPoint)listener.LocalEndpoint).Port;
        var lateRead = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var nextRead = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var release = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var peer = Task.Run(async () =>
        {
            using var tcp = await listener.AcceptTcpClientAsync(); using var stream = tcp.GetStream();
            var auth = await Read(stream); Check(auth.MessageId == 0 && auth.Sequence == 1, "auth sequence");
            await Send(stream, new Packet(0, auth.Sequence, Array.Empty<byte>()));
            var first = await Read(stream); Check(first.Sequence == 2, "first sequence");
            // 同一seq的Sync push不能冒充PB response。
            await Send(stream, new Packet(10103, first.Sequence, RawSync, PayloadKind.Sync, true));
            await Send(stream, new Packet(43, first.Sequence, new byte[] { 8, 7 }));
            var notify = await Read(stream);
            Check(notify.Kind == PayloadKind.Lockstep && notify.Sequence == 3 &&
                LockstepCommand.Decode(notify.Payload.Span).Operation == LockstepOperation.Input, "Notify type/sequence");
            var late = await Read(stream); Check(late.Sequence == 4, "PB sequence after Notify"); lateRead.SetResult(true);
            var next = await Read(stream); nextRead.SetResult(true);
            await release.Task; // 由客户端确认取消后才回晚应答，不靠sleep制造时序。
            await Send(stream, new Packet(43, late.Sequence, new byte[] { 8, 9 }));
            await Send(stream, new Packet(43, next.Sequence, new byte[] { 8, 10 }));
            // 保持连接直到消费完成；不因测试server退出提前清掉客户端push队列。
            await Read(stream);
        });
        try
        {
            using var session = await TcpSession.ConnectAsync("127.0.0.1", port, "ticket", TimeSpan.FromSeconds(5));
            var reply = await session.RequestAsync(42, 43, new byte[] { 8, 7 }, TimeSpan.FromSeconds(5));
            Check(reply.Sequence == 2 && reply.Payload.Span[1] == 7, "PB reply");
            Check(session.TryDequeuePush(out var push) && push!.Kind == PayloadKind.Sync && push.IsPush, "separate Sync push");
            using (var alreadyCanceled = new CancellationTokenSource())
            {
                alreadyCanceled.Cancel();
                try { await session.NotifyAsync(50001, PayloadKind.Lockstep, new byte[] { 0xc8, 1, 3, 1 }, TimeSpan.FromSeconds(5), alreadyCanceled.Token); throw new Exception("canceled Notify accepted"); }
                catch (OperationCanceledException) { }
                Check(session.CloseReason == null, "prewrite Notify cancellation keeps connection");
            }
            await session.NotifyAsync(50001, PayloadKind.Lockstep,
                new LockstepCommand(LockstepOperation.Input, 1, new byte[] { 42 }).Encode(), TimeSpan.FromSeconds(5));
            using var canceled = new CancellationTokenSource();
            Task<Packet> late = session.RequestAsync(42, 43, new byte[] { 8, 9 }, TimeSpan.FromSeconds(5), canceled.Token);
            await lateRead.Task.WaitAsync(TimeSpan.FromSeconds(5));
            Task<Packet> nextCall = session.RequestAsync(42, 43, new byte[] { 8, 10 }, TimeSpan.FromSeconds(5));
            await nextRead.Task.WaitAsync(TimeSpan.FromSeconds(5)); // 第三包已读，证明第二包写阶段已退出。
            canceled.Cancel();
            try { await late; throw new Exception("cancel did not happen"); } catch (OperationCanceledException) { }
            release.SetResult(true);
            var next = await nextCall;
            Check(next.Payload.Span[1] == 10 && !session.TryDequeuePush(out _), "late reply discarded");
            session.Dispose();
            await session.Completion;
            try { await peer; } catch (EndOfStreamException) { }
            Console.WriteLine("PASS session push/correlation/cancellation/late response/close");
        }
        finally { listener.Stop(); }
    }
    private static async Task Capacity()
    {
        var listener = new TcpListener(IPAddress.Loopback, 0); listener.Start();
        int port = ((IPEndPoint)listener.LocalEndpoint).Port;
        var firstRead = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var overflow = new TaskCompletionSource<bool>(TaskCreationOptions.RunContinuationsAsynchronously);
        var peer = Task.Run(async () =>
        {
            using var tcp = await listener.AcceptTcpClientAsync(); using var stream = tcp.GetStream();
            var auth = await Read(stream); await Send(stream, new Packet(0, auth.Sequence, Array.Empty<byte>()));
            await Read(stream); firstRead.SetResult(true);
            await overflow.Task;
            await Send(stream, new Packet(10103, 1, RawSync, PayloadKind.Sync, true));
            await Send(stream, new Packet(10103, 2, RawSync, PayloadKind.Sync, true));
        });
        try
        {
            using var session = await TcpSession.ConnectAsync("127.0.0.1", port, "ticket", TimeSpan.FromSeconds(5), maxPending: 1, maxPushes: 1);
            try { await session.RequestAsync(42,43,Array.Empty<byte>(),TimeSpan.FromDays(30)); throw new Exception("timeout accepted"); }
            catch (ArgumentOutOfRangeException) { }
            // 非法预算必须在占容量前拒绝；下一合法请求照常发出。
            var call = session.RequestAsync(42,43,Array.Empty<byte>(),TimeSpan.FromSeconds(5));
            await firstRead.Task.WaitAsync(TimeSpan.FromSeconds(5));
            try { await session.RequestAsync(42,43,Array.Empty<byte>(),TimeSpan.FromSeconds(5)); throw new Exception("capacity accepted"); }
            catch (InvalidOperationException) { }
            try { await session.NotifyAsync(50001,PayloadKind.Lockstep,new byte[]{0xc8,1,3,1},TimeSpan.FromSeconds(5)); throw new Exception("Notify bypassed shared capacity"); }
            catch (InvalidOperationException) { }
            overflow.SetResult(true);
            await session.Completion.WaitAsync(TimeSpan.FromSeconds(5));
            Check(session.CloseReason is IOException && session.CloseReason.Message.Contains("capacity"), "overflow disconnect");
            Check(!session.TryDequeuePush(out _), "overflow clears unusable deltas");
            try { await call; throw new Exception("pending survived close"); } catch (IOException) { }
            await peer;
            Console.WriteLine("PASS pending limit/invalid budget/push overflow/fail pending");
        }
        finally { listener.Stop(); }
    }

    private static async Task<Packet> Read(Stream stream)
    {
        var header = new byte[16]; await stream.ReadExactlyAsync(header);
        int size = checked((int)System.Buffers.Binary.BinaryPrimitives.ReadUInt32BigEndian(header.AsSpan(12, 4)));
        var data = new byte[16 + size]; header.CopyTo(data, 0);
        await stream.ReadExactlyAsync(data.AsMemory(16)); return PacketCodec.Decode(data);
    }
    private static async Task Send(Stream stream, Packet packet)
    { await stream.WriteAsync(PacketCodec.Encode(packet)); }
}
