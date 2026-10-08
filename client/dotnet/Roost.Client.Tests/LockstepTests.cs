using System;
using System.Collections.Generic;
using System.IO;
using System.Net;
using System.Net.Sockets;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client;

internal static partial class Program
{
    private static byte[] EmptyFrames(params byte[] ids)
    {
        var data = new List<byte> { 0xC7, 1, (byte)ids.Length };
        foreach (byte id in ids) { data.Add(id); data.Add(0); }
        return data.ToArray();
    }
    private static void TestLockstep()
    {
        var assembler = new LockstepAssembler(1);
        Check(assembler.Ingest(EmptyFrames(3)).Count == 0 && assembler.Gap && assembler.Next == 1, "buffer future");
        Refuses(() => assembler.Ingest(EmptyFrames(4)), "reorder overflow");
        Check(assembler.Next == 1 && assembler.Buffered == 1, "overflow transactional");
        var ready = assembler.Ingest(EmptyFrames(1, 2));
        Check(ready.Count == 3 && ready[2].Id == 3 && assembler.Next == 4 && !assembler.Gap, "gap healing ordered");
        Check(assembler.Ingest(EmptyFrames(1, 2, 3)).Count == 0, "duplicate suppression");
        foreach (var bad in new byte[][] {
            new byte[]{0xc7,1,1,0,0}, new byte[]{0xc7,1,2,2,0,1,0},
            new byte[]{0xc7,1,1,1,0,0}, new byte[]{0xc7,1,65},
            new byte[]{0xc7,1,1,255,255,255,255,255,255,255,255,255,2},
            new byte[]{0xc7,1,1,1,1,7,2,42},
            new byte[]{0xc7,1,1,1,1,7,129,8},
            new byte[]{0xc7,1,1,128,128,128,128,16,0},
            new byte[]{0xc7,1,1,1,1,128,128,128,128,16,0},
            new byte[]{0xc7,1,1,1,129,2} })
            Refuses(() => LockstepCodec.DecodeBroadcast(bad), "corrupt Lockstep broadcast");
        foreach (var bad in new byte[][] {
            new byte[]{0xc8,1,9,1}, new byte[]{0xc8,1,3,0}, new byte[]{0xc8,1,3,1,0},
            new byte[]{0xc8,1,1,1,2,42}, new byte[]{0xc8,1,2,1,128},
            new byte[]{0xc8,1,2,1,255,255,255,255,255,255,255,255,255,2} })
            Refuses(() => LockstepCommand.Decode(bad), "corrupt Lockstep command");
        Refuses(() => new LockstepCommand(LockstepOperation.Input, 1, new byte[1025]).Encode(), "oversized input");
        Refuses(() => new LockstepCommand(LockstepOperation.Catchup, 1, hash: 1).Encode(), "ambiguous command");
        Console.WriteLine("PASS Lockstep limits/overflow/ordering/duplicates/atomic gap healing");
    }

    private static async Task ConsumeLockstep(TcpSession session, LockstepClient client, uint until, List<uint> applied)
    {
        using var budget = new CancellationTokenSource(TimeSpan.FromSeconds(10));
        while (client.Next <= until)
        {
            budget.Token.ThrowIfCancellationRequested();
            if (session.TryDequeuePush(out var packet))
            {
                await client.HandlePushAsync(packet!, frame =>
                {
                    applied.Add(frame.Id);
                    if (frame.Id == 1 || frame.Id == 12)
                        Check(frame.Inputs.Count == 1 && frame.Inputs[0].Player == 7 &&
                            frame.Inputs[0].Payload.Span[0] == (frame.Id == 1 ? 42 : 9), "authenticated seat/input");
                }, budget.Token);
            }
            else await Task.Yield();
        }
    }

    private static async Task GeneratedLockstep(TcpSession session)
    {
        var client = new LockstepClient(session, 50001, 50002, TimeSpan.FromSeconds(5));
        var applied = new List<uint>();
        await client.SubmitInputAsync(1, new byte[] { 42 });
        await ConsumeLockstep(session, client, 1, applied);
        await client.ReportHashAsync(1, 123);
        // PB与Notify共用序列。夹具主动隐藏2..9的广播，frame10触发真实历史追帧。
        await session.RequestAsync(42, 43, new byte[] { 8, 43 }, TimeSpan.FromSeconds(5));
        await ConsumeLockstep(session, client, 11, applied);
        await client.SubmitInputAsync(12, new byte[] { 9 });
        await ConsumeLockstep(session, client, 12, applied);
        Check(applied.Count == 12, "catchup delivered every frame once");
        for (int i = 0; i < applied.Count; i++) Check(applied[i] == i + 1, "ordered simulation");
        var broken = new LockstepClient(session, 50001, 50002, TimeSpan.FromSeconds(5));
        int simulated = 0;
        var batch = new Packet(50002, 99, EmptyFrames(1, 2, 3), PayloadKind.Lockstep, true);
        try { await broken.HandlePushAsync(batch, _ => { if (++simulated == 2) throw new Exception("partial simulation"); }); throw new Exception("callback accepted"); }
        catch (Exception error) when (error.Message == "partial simulation") { }
        try { await broken.HandlePushAsync(batch, _ => simulated++); throw new Exception("terminal resumed"); }
        catch (InvalidOperationException) { }
        Check(simulated == 2 && broken.Failure != null, "no simulation retry after partial failure");
        Console.WriteLine("PASS generated TCP Lockstep input/hash/gap/catchup/seat/terminal simulation");
    }

    private static async Task LockstepGapRetry()
    {
        var listener = new TcpListener(IPAddress.Loopback, 0); listener.Start();
        int port = ((IPEndPoint)listener.LocalEndpoint).Port;
        var peer = Task.Run(async () =>
        {
            using var tcp = await listener.AcceptTcpClientAsync(); using var stream = tcp.GetStream();
            stream.ReadTimeout = 5000;
            var auth = await Read(stream); await Send(stream, new Packet(0, auth.Sequence, Array.Empty<byte>()));
            foreach (uint expected in new uint[] { 1, 0, 1, 2, 0 })
            {
                var packet = await Read(stream).WaitAsync(TimeSpan.FromSeconds(5));
                if (expected == 0)
                { Check(packet.Kind == PayloadKind.Protobuf && packet.MessageId == 42, "no redundant catchup before PB barrier"); await Send(stream, new Packet(43, packet.Sequence, Array.Empty<byte>())); }
                else
                { var command = LockstepCommand.Decode(packet.Payload.Span); Check(command.Operation == LockstepOperation.Catchup && command.Frame == expected, "gap retry cursor"); }
            }
            try { await Read(stream).WaitAsync(TimeSpan.FromSeconds(5)); throw new Exception("extra catchup after healing"); }
            catch (EndOfStreamException) { }
        });
        try
        {
            using var session = await TcpSession.ConnectAsync("127.0.0.1", port, "ticket", TimeSpan.FromSeconds(5));
            var client = new LockstepClient(session, 50001, 50002, TimeSpan.FromSeconds(5), retryPackets: 2);
            var applied = new List<uint>();
            Action<LockstepFrame> simulate = frame => applied.Add(frame.Id);
            Packet Future() => new Packet(50002, 1, EmptyFrames(3), PayloadKind.Lockstep, true);
            await client.HandlePushAsync(Future(), simulate); // from1
            await client.HandlePushAsync(Future(), simulate); // dedupe
            await session.RequestAsync(42, 43, Array.Empty<byte>(), TimeSpan.FromSeconds(5));
            await client.HandlePushAsync(Future(), simulate); // retry from1
            await client.HandlePushAsync(new Packet(50002, 2, EmptyFrames(1), PayloadKind.Lockstep, true), simulate); // advance Next, no duplicate request
            await client.HandlePushAsync(Future(), simulate);
            await client.HandlePushAsync(Future(), simulate); // retry from2
            await client.HandlePushAsync(new Packet(50002, 3, EmptyFrames(2), PayloadKind.Lockstep, true), simulate);
            await session.RequestAsync(42, 43, Array.Empty<byte>(), TimeSpan.FromSeconds(5));
            Check(applied.Count == 3 && applied[0] == 1 && applied[1] == 2 && applied[2] == 3, "retry applied once/in order");
            session.Dispose(); await peer;
            Console.WriteLine("PASS Lockstep catchup dedupe/stagnant retry/progress/healing");
        }
        finally { listener.Stop(); }
    }
}
