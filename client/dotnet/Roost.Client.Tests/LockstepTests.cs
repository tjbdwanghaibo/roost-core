using System;
using System.Collections.Generic;
using System.IO;
using System.Net;
using System.Net.Sockets;
using System.Security.Cryptography;
using System.Text;
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
        var unservable = new LockstepAssembler();
        Check(unservable.Ingest(EmptyFrames(3)).Count == 0 && unservable.Gap, "gap before notice");
        try { unservable.Ingest(new byte[] { 0xC9, 2, 1, 1, 20 }); throw new Exception("notice accepted as frames"); }
        catch (LockstepCatchupUnservableException error)
        { Check(error.Reason == LockstepRecoveryFailureReason.HistoryTrimmed && error.RequestedFrom == 1 && error.HistoryStart == 20, "notice fields"); }
        Check(unservable.Next == 1 && unservable.Buffered == 1, "notice leaves assembler unchanged");
        // C9 v2：只有history_trimmed要求from<start；所有原因要求from>=1；v1、未知原因、截断、尾字节、start溢出都是坏包。
        foreach (var bad in new byte[][] {
            new byte[]{0xc9,1,1,20}, new byte[]{0xc9,3,1,1,20}, new byte[]{0xc9,2,0,1,20}, new byte[]{0xc9,2,5,1,20},
            new byte[]{0xc9,2,2,0,20}, new byte[]{0xc9,2,1,20,20}, new byte[]{0xc9,2,1,1,20,0}, new byte[]{0xc9,2,1,1},
            new byte[]{0xc9,2,2,1,128,128,128,128,16} })
            Refuses(() => unservable.Ingest(bad), "corrupt catch-up notice");
        foreach (var (reason, raw) in new (LockstepRecoveryFailureReason, byte[])[] {
            (LockstepRecoveryFailureReason.SnapshotUnavailable, new byte[]{0xc9,2,2,30,20}),
            (LockstepRecoveryFailureReason.SnapshotTooLarge, new byte[]{0xc9,2,3,20,20}),
            (LockstepRecoveryFailureReason.RecoveryExpired, new byte[]{0xc9,2,4,1,0}) })
        {
            var notice = LockstepCodec.DecodeCatchupUnservable(raw);
            Check(notice.Reason == reason && notice.RequestedFrom == raw[3] && notice.HistoryStart == raw[4], "recovery notice " + reason);
        }
        Console.WriteLine("PASS Lockstep limits/overflow/ordering/duplicates/atomic gap healing");
    }

    private static void Uvarint(List<byte> data, ulong value)
    {
        while (value >= 128) { data.Add((byte)((value & 127) | 128)); value >>= 7; }
        data.Add((byte)value);
    }
    // 按CA 01编码一个分块（测试侧独立实现，不复用SDK编码器）；首块默认带data整体的SHA-256。
    private static byte[] Chunk(ulong transfer, int offset, ReadOnlySpan<byte> data, uint frame = 0, int total = 0,
        ulong? stateHash = null, byte[]? digest = null, byte flags = 0)
    {
        var chunk = new List<byte> { 0xCA, 1 };
        Uvarint(chunk, transfer); Uvarint(chunk, (ulong)offset);
        if (offset == 0)
        {
            Uvarint(chunk, frame); Uvarint(chunk, (ulong)total);
            chunk.Add((byte)(flags | (stateHash.HasValue ? 1 : 0)));
            if (stateHash.HasValue) Uvarint(chunk, stateHash.Value);
            chunk.AddRange(digest ?? throw new ArgumentNullException(nameof(digest)));
        }
        chunk.AddRange(data.ToArray());
        return chunk.ToArray();
    }
    private static byte[] Sha256(byte[] data) { using var sha = SHA256.Create(); return sha.ComputeHash(data); }
    // 一份快照按size字节切块；返回各分块包。
    private static List<byte[]> Snapshot(ulong transfer, uint frame, byte[] data, int size, ulong? stateHash = null, byte[]? digest = null)
    {
        var chunks = new List<byte[]>();
        for (int offset = 0; offset < data.Length; offset += size)
        {
            var part = data.AsSpan(offset, Math.Min(size, data.Length - offset));
            chunks.Add(offset == 0 ? Chunk(transfer, 0, part, frame, data.Length, stateHash, digest ?? Sha256(data)) : Chunk(transfer, offset, part));
        }
        return chunks;
    }

    // 快照重组的各分支与Go lockstep.SnapshotReceiver同一规则：重发丢弃、跳跃/越界坏包、新transfer取代、摘要不符。
    private static void TestLockstepSnapshot()
    {
        byte[] state = Encoding.ASCII.GetBytes("roost-snapshot-unit-test-state-32");
        // 正常路径：两块到齐、C7在传输中丢弃、交出后必须先Resume、Resume后从快照帧+1续帧且清空乱序缓冲。
        var assembler = new LockstepAssembler();
        Check(assembler.Receive(EmptyFrames(1)).Frames.Count == 1 && assembler.Receive(EmptyFrames(9)).Frames.Count == 0 && assembler.Gap, "buffer before snapshot");
        var parts = Snapshot(5, 20, state, 16, stateHash: 77);
        Check(parts.Count == 3, "three chunks");
        Check(assembler.Receive(parts[0]).Snapshot == null && assembler.SnapshotInProgress, "first chunk starts transfer");
        Check(assembler.Receive(EmptyFrames(2, 3)).Frames.Count == 0 && assembler.Next == 2, "C7 dropped during transfer");
        Refuses(() => assembler.Receive(new byte[] { 0xc7, 1, 1, 0, 0 }), "corrupt C7 during transfer");
        Check(assembler.SnapshotInProgress, "corrupt C7 does not cancel transfer");
        Check(assembler.Receive(parts[0]).Snapshot == null, "duplicate first chunk dropped");
        Check(assembler.Receive(parts[1]).Snapshot == null && assembler.Receive(parts[1]).Snapshot == null, "duplicate middle chunk dropped");
        var done = assembler.Receive(parts[2]).Snapshot;
        Check(done != null && done.Frame == 20 && done.StateHash == 77 && done.HasStateHash && done.Data.ToArray().AsSpan().SequenceEqual(state), "reassembled snapshot");
        Check(!assembler.SnapshotInProgress && assembler.SnapshotPending, "snapshot waits for resume");
        try { assembler.Receive(EmptyFrames(21)); throw new Exception("received before resume"); } catch (InvalidOperationException) { }
        assembler.ResumeAfterSnapshot(20);
        Check(assembler.Next == 21 && !assembler.Gap && !assembler.SnapshotPending, "resume at frame+1 clears buffer");
        Check(assembler.Receive(parts[2]).Snapshot == null && assembler.Receive(parts[0]).Snapshot == null, "late retransmit of completed transfer dropped");
        var after = assembler.Receive(EmptyFrames(19, 20, 21, 22)).Frames;
        Check(after.Count == 2 && after[0].Id == 21 && after[1].Id == 22, "frames continue after snapshot");
        try { assembler.ResumeAfterSnapshot(30); throw new Exception("resume without snapshot"); } catch (InvalidOperationException) { }

        // 新transfer且offset=0取代未完成的旧传输；被取代传输的后续分块是坏包。
        var replace = new LockstepAssembler();
        var old = Snapshot(1, 10, state, 16, stateHash: 1);
        var fresh = Snapshot(2, 11, state, 20);
        replace.Receive(old[0]); replace.Receive(fresh[0]);
        var replaced = replace.Receive(fresh[1]).Snapshot;
        Check(replaced != null && replaced.Frame == 11 && !replaced.HasStateHash, "new transfer supersedes unfinished one");
        replace.ResumeAfterSnapshot(11);
        Refuses(() => replace.Receive(old[1]), "superseded transfer continues");

        // 新transfer但offset≠0、offset跳跃、越过total：坏包，丢弃进行中的传输但不动帧组装状态。
        var skip = new LockstepAssembler();
        Refuses(() => skip.Receive(Chunk(4, 16, state.AsSpan(16))), "new transfer at nonzero offset");
        skip.Receive(parts[0]);
        Refuses(() => skip.Receive(Chunk(5, 20, state.AsSpan(20))), "offset skips ahead");
        Check(!skip.SnapshotInProgress && skip.Next == 1, "skip discards transfer only");
        Refuses(() => skip.Receive(parts[1]), "chunk after discarded transfer");
        skip.Receive(parts[0]); skip.Receive(parts[1]);
        Refuses(() => skip.Receive(Chunk(5, 32, Encoding.ASCII.GetBytes("xy"))), "chunk overruns total");

        // 字节齐了但摘要不符：LockstepSnapshotCorruptException；同编号的重发随后被丢弃。
        var corrupt = new LockstepAssembler();
        var wrong = Snapshot(6, 20, state, 16, digest: Sha256(Encoding.ASCII.GetBytes("other")));
        corrupt.Receive(wrong[0]); corrupt.Receive(wrong[1]);
        Refuses(() => corrupt.Receive(wrong[2]), "digest mismatch", typeof(LockstepSnapshotCorruptException));
        Check(!corrupt.SnapshotInProgress && !corrupt.SnapshotPending && corrupt.Receive(wrong[2]).Snapshot == null, "corrupt transfer not delivered");

        // 首块头部的严格校验与客户端上限：零transfer、flags保留位、total为0/超限、首块数据超过total、空数据、截断摘要、版本。
        byte[] digest = Sha256(state);
        var limited = new LockstepAssembler(maxSnapshotBytes: 32);
        foreach (var bad in new byte[][] {
            Chunk(0, 0, state.AsSpan(0, 4), 1, 4, digest: digest),
            Chunk(1, 0, state.AsSpan(0, 4), 1, 4, digest: digest, flags: 2),
            Chunk(1, 0, state.AsSpan(0, 4), 1, 0, digest: digest),
            Chunk(1, 0, state.AsSpan(0, 4), 1, 33, digest: digest),
            Chunk(1, 0, state.AsSpan(0, 8), 1, 4, digest: digest),
            Chunk(1, 0, ReadOnlySpan<byte>.Empty, 1, 4, digest: digest),
            Chunk(1, 0, ReadOnlySpan<byte>.Empty, 1, 4, digest: digest.AsSpan(0, 31).ToArray()),
            Chunk(1, 32, state.AsSpan(0, 1)),
            new byte[] { 0xCA, 2, 1, 1, 1 },
            new byte[] { 0xCA, 1, 1, 0, 128, 128, 128, 128, 16, 4, 0 } })
            Refuses(() => limited.Receive(bad), "corrupt snapshot chunk");
        Check(limited.Next == 1 && !limited.SnapshotInProgress, "corrupt chunks leave assembler idle");
        try { new LockstepAssembler(maxSnapshotBytes: LockstepCodec.MaxSnapshotBytes + 1); throw new Exception("oversized limit accepted"); }
        catch (ArgumentOutOfRangeException) { }

        // 只追帧的Ingest遇到快照：明确的不支持异常，不当坏包。
        Refuses(() => new LockstepAssembler().Ingest(parts[0]), "snapshot through Ingest", typeof(LockstepSnapshotUnsupportedException));
        var hasher = new LockstepInputHasher(); hasher.Reset(42);
        Check(hasher.Value == 42, "input hasher reset");
        Console.WriteLine("PASS Lockstep snapshot reassembly/duplicates/skip/supersede/digest/limits");
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
            // 追帧失败通知：终止实例、保留原因，之后不再消费也不再请求追帧（peer会把多余请求判为失败）。
            var rejected = new LockstepClient(session, 50001, 50002, TimeSpan.FromSeconds(5));
            try { await rejected.HandlePushAsync(new Packet(50002, 4, new byte[] { 0xC9, 2, 1, 1, 20 }, PayloadKind.Lockstep, true), simulate); throw new Exception("notice accepted"); }
            catch (LockstepCatchupUnservableException error) { Check(error.RequestedFrom == 1 && error.HistoryStart == 20, "client notice fields"); }
            Check(rejected.Failure is LockstepCatchupUnservableException, "notice is the terminal cause");
            try { await rejected.HandlePushAsync(Future(), simulate); throw new Exception("terminal client resumed"); }
            catch (InvalidOperationException) { }
            Check(applied.Count == 3, "no simulation after catch-up failure");
            session.Dispose(); await peer;
            Console.WriteLine("PASS Lockstep catchup dedupe/stagnant retry/progress/healing");
        }
        finally { listener.Stop(); }
    }

    // LockstepClient的快照恢复路径：缺口→追帧请求→快照分块（期间不模拟、不重发追帧）→restore→核对hash→从快照帧+1续帧；
    // 以及§7.3的各终止情形。真实回环TCP，peer核对客户端只发了一次追帧请求。
    private static async Task LockstepSnapshotClient()
    {
        var listener = new TcpListener(IPAddress.Loopback, 0); listener.Start();
        int port = ((IPEndPoint)listener.LocalEndpoint).Port;
        var peer = Task.Run(async () =>
        {
            using var tcp = await listener.AcceptTcpClientAsync(); using var stream = tcp.GetStream();
            var auth = await Read(stream); await Send(stream, new Packet(0, auth.Sequence, Array.Empty<byte>()));
            var packet = await Read(stream).WaitAsync(TimeSpan.FromSeconds(5));
            var command = LockstepCommand.Decode(packet.Payload.Span);
            Check(command.Operation == LockstepOperation.Catchup && command.Frame == 2, "catchup before snapshot");
            try { await Read(stream).WaitAsync(TimeSpan.FromSeconds(5)); throw new Exception("catchup re-sent during snapshot transfer"); }
            catch (EndOfStreamException) { }
        });
        try
        {
            using var session = await TcpSession.ConnectAsync("127.0.0.1", port, "ticket", TimeSpan.FromSeconds(5));
            byte[] state = Encoding.ASCII.GetBytes("snapshot-client-state-0123456789-abcdef");
            var restored = new List<LockstepSnapshot>();
            var applied = new List<uint>();
            Action<LockstepFrame> simulate = frame => applied.Add(frame.Id);
            Packet Push(byte[] payload) => new Packet(50002, 1, payload, PayloadKind.Lockstep, true);
            var client = new LockstepClient(session, 50001, 50002, TimeSpan.FromSeconds(5), retryPackets: 2,
                restore: snapshot => { restored.Add(snapshot); return 99; });
            await client.HandlePushAsync(Push(EmptyFrames(1)), simulate);
            await client.HandlePushAsync(Push(EmptyFrames(5)), simulate); // 缺口：请求从2追帧
            var chunks = Snapshot(9, 30, state, 8, stateHash: 99);
            Check(chunks.Count == 5, "five chunks");
            foreach (var chunk in chunks.GetRange(0, 4))
            {
                await client.HandlePushAsync(Push(chunk), simulate);
                await client.HandlePushAsync(Push(EmptyFrames(2, 3, 4)), simulate); // 传输中的C7丢弃，不计缺口重试
            }
            Check(client.SnapshotInProgress && restored.Count == 0 && applied.Count == 1, "no simulation during transfer");
            await client.HandlePushAsync(Push(chunks[4]), simulate);
            Check(restored.Count == 1 && restored[0].Frame == 30 && restored[0].Data.ToArray().AsSpan().SequenceEqual(state), "restore called once with snapshot");
            Check(client.Next == 31 && client.SnapshotsRestored == 1 && !client.SnapshotInProgress && client.Failure == null, "resume after restore");
            await client.HandlePushAsync(Push(EmptyFrames(29, 30, 31, 32)), simulate);
            Check(applied.Count == 3 && applied[1] == 31 && applied[2] == 32, "frames continue from snapshot frame + 1");

            // §7.3：状态hash不符、restore抛错、没有restore、摘要不符、分块坏包——都记为Failure并终止，不再消费。
            async Task Terminal(LockstepClient broken, IEnumerable<byte[]> packets, Type expected, string why)
            {
                try
                {
                    foreach (var payload in packets) await broken.HandlePushAsync(Push(payload), simulate);
                    throw new Exception("accepted " + why);
                }
                catch (Exception error) when (error.GetType() == expected) { }
                Check(broken.Failure != null && broken.Failure.GetType() == expected, why + " is the terminal cause");
                try { await broken.HandlePushAsync(Push(EmptyFrames(1)), simulate); throw new Exception(why + " resumed"); }
                catch (InvalidOperationException) { }
            }
            var mismatch = new LockstepClient(session, 50001, 50002, TimeSpan.FromSeconds(5), restore: _ => 98);
            await Terminal(mismatch, Snapshot(1, 30, state, 64, stateHash: 99), typeof(LockstepSnapshotMismatchException), "state hash mismatch");
            var mismatchError = (LockstepSnapshotMismatchException)mismatch.Failure!;
            Check(mismatchError.Frame == 30 && mismatchError.Expected == 99 && mismatchError.Actual == 98 && mismatch.SnapshotsRestored == 0, "mismatch fields");
            await Terminal(new LockstepClient(session, 50001, 50002, TimeSpan.FromSeconds(5), restore: _ => throw new ApplicationException("restore failed")),
                Snapshot(1, 30, state, 64), typeof(ApplicationException), "restore failure");
            await Terminal(new LockstepClient(session, 50001, 50002, TimeSpan.FromSeconds(5)),
                Snapshot(1, 30, state, 8), typeof(LockstepSnapshotUnsupportedException), "snapshot without restore");
            await Terminal(new LockstepClient(session, 50001, 50002, TimeSpan.FromSeconds(5), restore: _ => 0),
                Snapshot(1, 30, state, 8, digest: Sha256(Array.Empty<byte>())), typeof(LockstepSnapshotCorruptException), "digest mismatch");
            var skipped = Snapshot(1, 30, state, 8); skipped.RemoveAt(1);
            await Terminal(new LockstepClient(session, 50001, 50002, TimeSpan.FromSeconds(5), restore: _ => 0),
                skipped, typeof(InvalidDataException), "chunk offset skip");
            // 没有状态hash的快照不比较restore的返回值。
            var unhashed = new LockstepClient(session, 50001, 50002, TimeSpan.FromSeconds(5), restore: _ => 12345);
            foreach (var payload in Snapshot(2, 7, state, 64)) await unhashed.HandlePushAsync(Push(payload), simulate);
            Check(unhashed.Failure == null && unhashed.Next == 8 && unhashed.SnapshotsRestored == 1, "snapshot without state hash");
            Check(applied.Count == 3, "no simulation in terminal clients");
            session.Dispose(); await peer;
            Console.WriteLine("PASS Lockstep client snapshot restore/resume/no catchup during transfer/terminal failures");
        }
        finally { listener.Stop(); }
    }
}
