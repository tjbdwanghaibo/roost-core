using System;
using System.Collections.Generic;
using System.IO;
using System.Threading;
using System.Threading.Tasks;

namespace Roost.Client
{
    // 单所有者。每包先完整校验并在临时有界buffer中组装，失败不会丢已释放帧。
    public sealed class LockstepAssembler
    {
        private Dictionary<uint, LockstepFrame> buffered = new Dictionary<uint, LockstepFrame>();
        private readonly int maxBuffered;
        public ulong Next { get; private set; } = 1; // uint32末帧后停在2^32，不回绕到0。
        public bool Gap => buffered.Count != 0;
        public int Buffered => buffered.Count;
        public LockstepAssembler(int maxBuffered = 256)
        { if (maxBuffered <= 0) throw new ArgumentOutOfRangeException(nameof(maxBuffered)); this.maxBuffered = maxBuffered; }
        public IReadOnlyList<LockstepFrame> Ingest(ReadOnlySpan<byte> packet)
        {
            var frames = LockstepCodec.DecodeBroadcast(packet);
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
    }

    // 一实例一场比赛，调用方（Unity/Godot主线程）串行await消费；网络线程不运行模拟。
    // 业务负责固定步长/定点/确定性随机及比赛生命周期隔离；本类不拥有共享TcpSession。
    public sealed class LockstepClient
    {
        private readonly TcpSession session;
        private readonly uint commandId, broadcastId;
        private readonly TimeSpan timeout;
        private readonly LockstepAssembler assembler;
        private readonly int retryPackets;
        private ulong requested;
        private int stagnant;
        private bool consuming;
        public Exception? Failure { get; private set; }
        public ulong Next => assembler.Next;
        public LockstepClient(TcpSession session, uint commandId, uint broadcastId, TimeSpan timeout,
            int maxBuffered = 256, int retryPackets = 64)
        {
            this.session = session ?? throw new ArgumentNullException(nameof(session));
            if (commandId == 0 || broadcastId == 0 || timeout <= TimeSpan.Zero || timeout.TotalMilliseconds > int.MaxValue || retryPackets <= 0)
                throw new ArgumentOutOfRangeException(nameof(commandId));
            this.commandId = commandId; this.broadcastId = broadcastId; this.timeout = timeout;
            this.retryPackets = retryPackets; assembler = new LockstepAssembler(maxBuffered);
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

        // 全部模拟在首次await前运行。回调异常可能已部分修改状态，立即终止，不重复模拟。
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
                var ready = assembler.Ingest(packet.Payload.Span);
                try { foreach (var frame in ready) simulate(frame); }
                catch (Exception error) { Failure = error; throw; }
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
            finally { consuming = false; }
        }
    }
}
