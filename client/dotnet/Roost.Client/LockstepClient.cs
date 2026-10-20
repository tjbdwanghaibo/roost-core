using System;
using System.Collections.Generic;
using System.IO;
using System.Security.Cryptography;
using System.Threading;
using System.Threading.Tasks;

namespace Roost.Client
{
    // 单所有者。每包先完整校验并在临时有界buffer中组装，失败不会丢已释放帧。
    // Receive处理C7 / C9 / CA：快照分块按transfer/offset重组并核对SHA-256，交出完整快照后等待调用方
    // ResumeAfterSnapshot；Ingest只用于只追帧的调用方，遇到CA抛LockstepSnapshotUnsupportedException。
    public sealed class LockstepAssembler
    {
        private Dictionary<uint, LockstepFrame> buffered = new Dictionary<uint, LockstepFrame>();
        private readonly int maxBuffered;
        private readonly int maxSnapshotBytes;
        // 快照重组状态（与Go lockstep.SnapshotReceiver同一规则）：transfer是最近一次见过的传输编号，完成后仍记着，
        // 用来丢弃它的重发分块；snapshotData非null表示一次传输已开始但未完成。
        private ulong transfer;
        private byte[]? snapshotData;
        private int snapshotReceived;
        private uint snapshotFrame;
        private ulong snapshotStateHash;
        private bool snapshotHasStateHash;
        private byte[]? snapshotDigest;
        private uint? unapplied; // 已交出、尚未ResumeAfterSnapshot的快照帧
        public ulong Next { get; private set; } = 1; // uint32末帧后停在2^32，不回绕到0。
        public bool Gap => buffered.Count != 0;
        public int Buffered => buffered.Count;
        // 有一次快照传输已开始但未完成；期间收到的C7帧丢弃（快照之后服务端从快照帧+1重发历史页）。
        public bool SnapshotInProgress => snapshotData != null;
        // 已交出的快照等待调用方恢复；在ResumeAfterSnapshot之前Receive / Ingest抛InvalidOperationException。
        public bool SnapshotPending => unapplied.HasValue;
        public LockstepAssembler(int maxBuffered = 256, int maxSnapshotBytes = LockstepCodec.DefaultMaxSnapshotBytes)
        {
            if (maxBuffered <= 0) throw new ArgumentOutOfRangeException(nameof(maxBuffered));
            if (maxSnapshotBytes <= 0 || maxSnapshotBytes > LockstepCodec.MaxSnapshotBytes) throw new ArgumentOutOfRangeException(nameof(maxSnapshotBytes));
            this.maxBuffered = maxBuffered; this.maxSnapshotBytes = maxSnapshotBytes;
        }

        // 只追帧：C7与C9同Receive；收到快照分块抛LockstepSnapshotUnsupportedException（不当坏包，也不改变状态）。
        public IReadOnlyList<LockstepFrame> Ingest(ReadOnlySpan<byte> packet)
        {
            if (LockstepCodec.IsSnapshotChunk(packet)) throw new LockstepSnapshotUnsupportedException();
            return Receive(packet).Frames;
        }

        // 事务式处理一个下行包：
        // - C9：抛LockstepCatchupUnservableException，不改变状态；
        // - C7：快照传输中整包丢弃；否则按序组装，坏包/溢出抛InvalidDataException且不提交部分结果；
        // - CA：按协议规则重组。坏包抛InvalidDataException、摘要不符抛LockstepSnapshotCorruptException，两者都丢弃
        //   进行中的传输（帧组装状态不变）；未齐返回空Delivery；到齐返回Snapshot，之后必须ResumeAfterSnapshot。
        public LockstepDelivery Receive(ReadOnlySpan<byte> packet)
        {
            if (unapplied.HasValue)
                throw new InvalidOperationException($"Lockstep snapshot of frame {unapplied.Value} not applied; call ResumeAfterSnapshot first.");
            if (LockstepCodec.IsCatchupNotice(packet)) throw LockstepCodec.DecodeCatchupUnservable(packet);
            if (LockstepCodec.IsSnapshotChunk(packet)) return new LockstepDelivery(Array.Empty<LockstepFrame>(), AcceptChunk(packet));
            var frames = LockstepCodec.DecodeBroadcast(packet);
            if (snapshotData != null) return new LockstepDelivery(Array.Empty<LockstepFrame>(), null);
            return new LockstepDelivery(Assemble(frames), null);
        }

        // 调用方用快照恢复状态（并核对状态hash）之后调用：下一帧期待frame+1，乱序缓冲整体清空。
        // 清空而不是只清<=frame：服务端快照之后从frame+1发历史页一直追到头，缓冲里的帧都会重发；保留它们反而
        // 会让frame+1起的页被当成重复而卡住缺口。frame可以与已交出的快照帧不同（调用方自行决定从哪帧继续），
        // 但没有待恢复的快照时调用抛InvalidOperationException。
        public void ResumeAfterSnapshot(uint frame)
        {
            if (!unapplied.HasValue) throw new InvalidOperationException("No Lockstep snapshot is waiting to be applied.");
            unapplied = null; buffered = new Dictionary<uint, LockstepFrame>(); Next = (ulong)frame + 1;
        }

        private IReadOnlyList<LockstepFrame> Assemble(IReadOnlyList<LockstepFrame> frames)
        {
            var pending = new Dictionary<uint, LockstepFrame>(buffered);
            ulong next = Next; var ready = new List<LockstepFrame>();
            foreach (var frame in frames)
            {
                if (frame.Id < next || pending.ContainsKey(frame.Id)) continue;
                if (frame.Id == next)
                {
                    ready.Add(frame); next++;
                    while (next <= uint.MaxValue && pending.TryGetValue((uint)next, out var waiting))
                    { pending.Remove((uint)next); ready.Add(waiting); next++; }
                }
                else
                {
                    if (pending.Count >= maxBuffered) throw new InvalidDataException("Lockstep reorder capacity exhausted; restore match.");
                    pending.Add(frame.Id, frame);
                }
            }
            buffered = pending; Next = next; return ready.AsReadOnly();
        }

        private LockstepSnapshot? AcceptChunk(ReadOnlySpan<byte> packet)
        {
            LockstepSnapshotChunk chunk;
            try { chunk = LockstepCodec.DecodeSnapshotChunk(packet, maxSnapshotBytes); }
            catch (InvalidDataException) { ResetSnapshot(); throw; }
            if (chunk.Transfer != transfer)
            {
                if (!chunk.First) { ResetSnapshot(); throw new InvalidDataException("Snapshot transfer must start at offset 0."); }
                // 新传输取代未完成的旧传输（服务端重取、窗口越过或换连接时换编号）。
                transfer = chunk.Transfer; snapshotData = new byte[chunk.Total]; snapshotReceived = 0;
                snapshotFrame = chunk.Frame; snapshotStateHash = chunk.StateHash; snapshotHasStateHash = chunk.HasStateHash;
                snapshotDigest = chunk.Digest.ToArray();
            }
            else
            {
                // 进行中的传输至少已收首块的1字节，所以同编号的首块重发也落在“小于已收”这一支。
                if (snapshotData == null || chunk.Offset < snapshotReceived) return null; // 已完成传输或已确认偏移之前的重发
                if (chunk.Offset > snapshotReceived) { ResetSnapshot(); throw new InvalidDataException("Snapshot chunk offset skips ahead."); }
            }
            var data = snapshotData!;
            if (chunk.Data.Length > data.Length - snapshotReceived) { ResetSnapshot(); throw new InvalidDataException("Snapshot chunk overruns total."); }
            chunk.Data.CopyTo(data.AsSpan(snapshotReceived)); snapshotReceived += chunk.Data.Length;
            if (snapshotReceived < data.Length) return null;
            byte[] sum;
            using (var sha = SHA256.Create()) sum = sha.ComputeHash(data);
            // 编号仍记着：之后到达的同编号重发分块被丢弃。
            snapshotData = null; snapshotReceived = 0;
            if (!sum.AsSpan().SequenceEqual(snapshotDigest!)) { snapshotDigest = null; throw new LockstepSnapshotCorruptException(snapshotFrame, data.Length); }
            snapshotDigest = null; unapplied = snapshotFrame;
            return new LockstepSnapshot(snapshotFrame, data, snapshotStateHash, snapshotHasStateHash);
        }

        private void ResetSnapshot()
        { transfer = 0; snapshotData = null; snapshotReceived = 0; snapshotDigest = null; }
    }

    // 与Go robot.FrameHasher相同的输入链摘要；不等于游戏模拟状态hash。
    public sealed class LockstepInputHasher
    {
        public ulong Value { get; private set; } = 14695981039346656037UL;
        public ulong Apply(LockstepFrame frame)
        {
            ulong sum = 14695981039346656037UL;
            void Feed(byte b) { sum = unchecked((sum ^ b) * 1099511628211UL); }
            void Feed64(ulong value) { for (int i = 0; i < 8; i++) { Feed((byte)value); value >>= 8; } }
            Feed64(Value); Feed64(frame.Id);
            foreach (var input in frame.Inputs)
            { Feed64(unchecked((ulong)(long)input.Player)); Feed64((ulong)input.Payload.Length); foreach (byte b in input.Payload.Span) Feed(b); }
            Value = sum; return sum;
        }
        // 快照恢复后以服务器给出的值（通常是快照的StateHash，或业务约定的链值）续链。
        public void Reset(ulong value) { Value = value; }
    }

    // 一实例一场比赛，调用方（Unity/Godot主线程）串行await消费；网络线程不运行模拟。
    // 业务负责固定步长/定点/确定性随机及比赛生命周期隔离；本类不拥有共享TcpSession。
    // restore非null时启用快照恢复：服务端判定走快照时，分块在HandlePushAsync里重组，完整且摘要相符后在调用方线程
    // 同步调用restore（业务用快照替换模拟状态并返回恢复后的状态hash），核对hash后从快照帧+1继续模拟。
    // restore为null时收到快照分块即终止（LockstepSnapshotUnsupportedException）。
    public sealed class LockstepClient
    {
        private readonly TcpSession session;
        private readonly uint commandId, broadcastId;
        private readonly TimeSpan timeout;
        private readonly LockstepAssembler assembler;
        private readonly int retryPackets;
        private readonly Func<LockstepSnapshot, ulong>? restore;
        private ulong requested;
        private int stagnant;
        private bool consuming;
        public Exception? Failure { get; private set; }
        public ulong Next => assembler.Next;
        // 有一次快照传输已开始但未完成（期间不模拟，也不因缺口重发追帧请求）。
        public bool SnapshotInProgress => assembler.SnapshotInProgress;
        // 成功恢复的快照次数（含旁观者）；业务可用于日志与指标。
        public int SnapshotsRestored { get; private set; }
        public LockstepClient(TcpSession session, uint commandId, uint broadcastId, TimeSpan timeout,
            int maxBuffered = 256, int retryPackets = 64,
            Func<LockstepSnapshot, ulong>? restore = null, int maxSnapshotBytes = LockstepCodec.DefaultMaxSnapshotBytes)
        {
            this.session = session ?? throw new ArgumentNullException(nameof(session));
            if (commandId == 0 || broadcastId == 0 || timeout <= TimeSpan.Zero || timeout.TotalMilliseconds > int.MaxValue || retryPackets <= 0)
                throw new ArgumentOutOfRangeException(nameof(commandId));
            this.commandId = commandId; this.broadcastId = broadcastId; this.timeout = timeout;
            this.retryPackets = retryPackets; this.restore = restore;
            assembler = new LockstepAssembler(maxBuffered, maxSnapshotBytes);
        }
        private Task Send(LockstepCommand command, CancellationToken cancellation)
        {
            if (Failure != null) throw new InvalidOperationException("Match client terminated; restore a new instance.", Failure);
            return session.NotifyAsync(commandId, PayloadKind.Lockstep, command.Encode(), timeout, cancellation);
        }
        public Task SubmitInputAsync(uint frame, ReadOnlyMemory<byte> payload, CancellationToken cancellation = default)
            => Send(new LockstepCommand(LockstepOperation.Input, frame, payload), cancellation);
        public Task ReportHashAsync(uint frame, ulong hash, CancellationToken cancellation = default)
            => Send(new LockstepCommand(LockstepOperation.Hash, frame, hash: hash), cancellation);
        public Task RequestCatchupAsync(uint from, CancellationToken cancellation = default)
            => Send(new LockstepCommand(LockstepOperation.Catchup, from), cancellation);

        // 全部模拟与快照恢复在首次await前运行。回调异常可能已部分修改状态，立即终止，不重复模拟。
        // 终止（记为Failure）的情形：坏包/容量溢出/快照分块坏包（InvalidDataException）、快照摘要不符
        // （LockstepSnapshotCorruptException）、恢复失败通知（LockstepCatchupUnservableException）、模拟或restore回调异常、
        // 状态hash不符（LockstepSnapshotMismatchException）、没有restore却收到快照（LockstepSnapshotUnsupportedException）。
        // 都不自动重试：同一份快照重试结果相同，业务新建实例或踢出。
        public async Task HandlePushAsync(Packet packet, Action<LockstepFrame> simulate, CancellationToken cancellation = default)
        {
            if (packet == null || simulate == null) throw new ArgumentNullException(nameof(packet));
            if (!packet.IsPush || packet.Kind != PayloadKind.Lockstep || packet.MessageId != broadcastId)
                throw new InvalidDataException("Packet is not this match's Lockstep broadcast.");
            if (Failure != null) throw new InvalidOperationException("Match client terminated.", Failure);
            if (consuming) throw new InvalidOperationException("Consume serially; await the previous push.");
            consuming = true;
            try
            {
                LockstepDelivery delivery = restore == null
                    ? new LockstepDelivery(assembler.Ingest(packet.Payload.Span), null)
                    : assembler.Receive(packet.Payload.Span);
                try
                {
                    if (delivery.Snapshot != null) Restore(delivery.Snapshot);
                    foreach (var frame in delivery.Frames) simulate(frame);
                }
                catch (Exception error) { Failure = error; throw; }
                // 快照分块未齐：不模拟、不计缺口重试（服务端在传快照时追帧请求是空操作，传完后自动续发历史页）。
                if (assembler.SnapshotInProgress) return;
                if (!assembler.Gap) { requested = 0; stagnant = 0; return; }
                if (requested == 0 || (requested == Next && ++stagnant >= retryPackets))
                {
                    await RequestCatchupAsync(checked((uint)Next), cancellation);
                    requested = Next; stagnant = 0;
                }
                else if (requested != Next) { requested = Next; stagnant = 0; }
            }
            // 解码/容量失败不能继续假装基线可用；应用重新建立比赛状态。
            catch (InvalidDataException error) { Failure = error; throw; }
            // 快照字节与摘要不符：重组或实现缺陷，再要一份结果相同。
            catch (LockstepSnapshotCorruptException error) { Failure = error; throw; }
            // 服务端无法完成恢复（历史已裁、快照不可用/过大/恢复过期）：终止本实例，由业务踢出或开新局；不再请求追帧。
            catch (LockstepCatchupUnservableException error) { Failure = error; throw; }
            // 服务端走了快照恢复但本实例没有restore回调：不能跳过快照继续模拟。
            catch (LockstepSnapshotUnsupportedException error) { Failure = error; throw; }
            finally { consuming = false; }
        }

        // 摘要已由Assembler核对；这里调用业务restore、比较状态hash，成功后从快照帧+1继续并重置追帧重试计数。
        private void Restore(LockstepSnapshot snapshot)
        {
            ulong got = restore!(snapshot);
            if (snapshot.HasStateHash && got != snapshot.StateHash)
                throw new LockstepSnapshotMismatchException(snapshot.Frame, snapshot.StateHash, got);
            assembler.ResumeAfterSnapshot(snapshot.Frame);
            requested = 0; stagnant = 0; SnapshotsRestored++;
        }
    }
}
