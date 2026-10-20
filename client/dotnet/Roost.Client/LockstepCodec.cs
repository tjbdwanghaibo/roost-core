using System;
using System.Collections.Generic;
using System.IO;

namespace Roost.Client
{
    public enum LockstepOperation : byte { Input = 1, Hash = 2, Catchup = 3 }

    public sealed class LockstepCommand
    {
        public LockstepOperation Operation { get; }
        public uint Frame { get; }
        public ReadOnlyMemory<byte> Payload { get; }
        public ulong Hash { get; }
        public LockstepCommand(LockstepOperation operation, uint frame, ReadOnlyMemory<byte> payload = default, ulong hash = 0)
        { Operation = operation; Frame = frame; Payload = payload.ToArray(); Hash = hash; }

        public byte[] Encode()
        {
            if (Frame == 0 || Operation < LockstepOperation.Input || Operation > LockstepOperation.Catchup ||
                (Operation == LockstepOperation.Input && (Payload.Length > 1024 || Hash != 0)) ||
                (Operation != LockstepOperation.Input && Payload.Length != 0) ||
                (Operation == LockstepOperation.Catchup && Hash != 0))
                throw new InvalidDataException("Invalid Lockstep command.");
            var data = new List<byte> { 0xC8, 1, (byte)Operation };
            LockstepCodec.WriteVarint(data, Frame);
            if (Operation == LockstepOperation.Input)
            { LockstepCodec.WriteVarint(data, (ulong)Payload.Length); data.AddRange(Payload.ToArray()); }
            if (Operation == LockstepOperation.Hash) LockstepCodec.WriteVarint(data, Hash);
            return data.ToArray();
        }

        public static LockstepCommand Decode(ReadOnlySpan<byte> data)
        {
            if (data.Length < 4 || data[0] != 0xC8 || data[1] != 1) throw new InvalidDataException("Invalid command header.");
            var operation = (LockstepOperation)data[2]; int offset = 3;
            uint frame = LockstepCodec.ReadFrame(data, ref offset);
            byte[] payload = Array.Empty<byte>(); ulong hash = 0;
            if (operation == LockstepOperation.Input) payload = LockstepCodec.ReadPayload(data, ref offset);
            else if (operation == LockstepOperation.Hash) hash = LockstepCodec.ReadVarint(data, ref offset);
            else if (operation != LockstepOperation.Catchup) throw new InvalidDataException("Unknown command.");
            if (offset != data.Length) throw new InvalidDataException("Command trailing bytes.");
            return new LockstepCommand(operation, frame, payload, hash);
        }
    }

    public sealed class LockstepInput
    {
        public int Player { get; }
        public ReadOnlyMemory<byte> Payload { get; }
        internal LockstepInput(int player, byte[] payload) { Player = player; Payload = payload; }
    }
    public sealed class LockstepFrame
    {
        public uint Id { get; }
        public IReadOnlyList<LockstepInput> Inputs { get; }
        internal LockstepFrame(uint id, List<LockstepInput> inputs) { Id = id; Inputs = inputs.AsReadOnly(); }
    }

    // C9 v2恢复失败通知的原因，取值与Go lockstep.RecoveryFailureReason一致。
    public enum LockstepRecoveryFailureReason : byte
    {
        // 起点已被裁出服务端保留窗口（只追帧的对局；原C9 v1的唯一语义）。
        HistoryTrimmed = 1,
        // 服务端快照来源连续失败达到上限。
        SnapshotUnavailable = 2,
        // 快照超过服务端配置的上限。
        SnapshotTooLarge = 3,
        // 恢复期间钉住的历史超过服务端上限（链路太慢，追不上）。
        RecoveryExpired = 4,
    }

    // 服务端恢复失败通知（C9 v2）：追帧或快照恢复都无法完成，本场无法继续。Reason说明原因；
    // HistoryStart是服务端当时仍保留的最早帧（只有HistoryTrimmed保证RequestedFrom < HistoryStart，其余原因仅供日志）。
    // 收到后LockstepClient终止；由业务决定踢出或开新局，SDK不自动重同步、不跳过缺帧。
    public sealed class LockstepCatchupUnservableException : Exception
    {
        public LockstepRecoveryFailureReason Reason { get; }
        public uint RequestedFrom { get; }
        public uint HistoryStart { get; }
        public LockstepCatchupUnservableException(uint requestedFrom, uint historyStart,
            LockstepRecoveryFailureReason reason = LockstepRecoveryFailureReason.HistoryTrimmed)
            : base(reason == LockstepRecoveryFailureReason.HistoryTrimmed
                ? $"Lockstep catch-up from frame {requestedFrom} cannot be served: server history starts at {historyStart}."
                : $"Lockstep recovery from frame {requestedFrom} failed ({ReasonName(reason)}); server history starts at {historyStart}.")
        { Reason = reason; RequestedFrom = requestedFrom; HistoryStart = historyStart; }

        private static string ReasonName(LockstepRecoveryFailureReason reason) => reason switch
        {
            LockstepRecoveryFailureReason.SnapshotUnavailable => "snapshot_unavailable",
            LockstepRecoveryFailureReason.SnapshotTooLarge => "snapshot_too_large",
            LockstepRecoveryFailureReason.RecoveryExpired => "recovery_expired",
            _ => "history_trimmed",
        };
    }

    // 快照分块坏包（头部、偏移、超限）抛InvalidDataException；字节都到齐但SHA-256与首块摘要不符抛本异常
    // （InvalidDataException是密封类，不能派生；LockstepClient同样记为Failure终止）。可靠通道上出现即是实现缺陷，
    // 重试会得到同样结果。
    public sealed class LockstepSnapshotCorruptException : Exception
    {
        public uint Frame { get; }
        public LockstepSnapshotCorruptException(uint frame, int length)
            : base($"Lockstep snapshot of frame {frame} ({length} bytes) does not match its SHA-256 digest.")
        { Frame = frame; }
    }

    // 业务restore返回的状态hash与快照携带的不同：字节对而状态不同，说明客户端版本、规则或确定性与服务器参考不一致。
    // 再要一份快照结果相同，SDK终止实例；业务据此提示更新客户端或踢出（可选在终止前自己ReportHashAsync(Frame, Actual)留证）。
    public sealed class LockstepSnapshotMismatchException : Exception
    {
        public uint Frame { get; }
        public ulong Expected { get; }
        public ulong Actual { get; }
        public LockstepSnapshotMismatchException(uint frame, ulong expected, ulong actual)
            : base($"Lockstep snapshot of frame {frame} restored to state hash {actual}, server reference is {expected}.")
        { Frame = frame; Expected = expected; Actual = actual; }
    }

    // 服务端发来了快照分块，但调用方没有启用快照恢复（LockstepAssembler.Ingest，或LockstepClient没有restore回调）。
    // 不能跳过快照继续模拟，实例终止。
    public sealed class LockstepSnapshotUnsupportedException : Exception
    {
        public LockstepSnapshotUnsupportedException()
            : base("Lockstep server sent a snapshot but this client has no snapshot restore; supply a restore callback.") { }
    }

    // 一份重组完成、SHA-256已核对的快照。Data归调用方所有（SDK不再引用）；Frame是快照对应的帧，
    // 恢复后从Frame+1继续模拟。HasStateHash为true时StateHash是服务器参考的业务状态hash（与ReportHash同一算法）。
    public sealed class LockstepSnapshot
    {
        public uint Frame { get; }
        public ReadOnlyMemory<byte> Data { get; }
        public ulong StateHash { get; }
        public bool HasStateHash { get; }
        internal LockstepSnapshot(uint frame, byte[] data, ulong stateHash, bool hasStateHash)
        { Frame = frame; Data = data; StateHash = stateHash; HasStateHash = hasStateHash; }
    }

    // 一个下行包的处理结果：Frames是按序可模拟的帧；Snapshot非null表示快照已完整且摘要已核对，
    // 调用方恢复状态后必须调用LockstepAssembler.ResumeAfterSnapshot(Snapshot.Frame)才能继续接收。
    public sealed class LockstepDelivery
    {
        public IReadOnlyList<LockstepFrame> Frames { get; }
        public LockstepSnapshot? Snapshot { get; }
        internal LockstepDelivery(IReadOnlyList<LockstepFrame> frames, LockstepSnapshot? snapshot)
        { Frames = frames; Snapshot = snapshot; }
    }

    // 解码后的CA 01分块；First（Offset=0）时头部字段有效。Data引用解码时传入的包。
    internal readonly ref struct LockstepSnapshotChunk
    {
        public readonly ulong Transfer;
        public readonly int Offset;
        public readonly uint Frame;
        public readonly int Total;
        public readonly ulong StateHash;
        public readonly bool HasStateHash;
        public readonly ReadOnlySpan<byte> Digest;
        public readonly ReadOnlySpan<byte> Data;
        public bool First => Offset == 0;
        public LockstepSnapshotChunk(ulong transfer, int offset, uint frame, int total, ulong stateHash, bool hasStateHash,
            ReadOnlySpan<byte> digest, ReadOnlySpan<byte> data)
        {
            Transfer = transfer; Offset = offset; Frame = frame; Total = total;
            StateHash = stateHash; HasStateHash = hasStateHash; Digest = digest; Data = data;
        }
    }

    // 直接消费sync/lockstep的C7 v1广播和追帧页、C9 v2恢复失败通知与CA 01快照分块，不另定义客户端帧格式。
    public static class LockstepCodec
    {
        // 快照上限与Go lockstep.DefaultMaxSnapshotBytes / MaxSnapshotBytes一致：首块声明的总长超过客户端上限即坏包，
        // 不按恶意总长分配内存。
        public const int DefaultMaxSnapshotBytes = 4 << 20;
        public const int MaxSnapshotBytes = 64 << 20;
        internal const int SnapshotDigestBytes = 32;

        public static bool IsCatchupNotice(ReadOnlySpan<byte> data) => data.Length > 0 && data[0] == 0xC9;
        public static bool IsSnapshotChunk(ReadOnlySpan<byte> data) => data.Length > 0 && data[0] == 0xCA;

        // 严格解码C9 02 reason(uvarint) from(uvarint) start(uvarint)：reason只接受1..4，所有原因要求from>=1，
        // 只有HistoryTrimmed要求from<start；v1、未知原因、截断、尾字节都抛InvalidDataException。
        public static LockstepCatchupUnservableException DecodeCatchupUnservable(ReadOnlySpan<byte> data)
        {
            if (data.Length < 5 || data[0] != 0xC9 || data[1] != 2) throw new InvalidDataException("Invalid catch-up notice header.");
            int offset = 2;
            ulong reason = ReadVarint(data, ref offset);
            uint from = ReadFrame(data, ref offset);
            ulong start = ReadVarint(data, ref offset);
            if (offset != data.Length || reason < (ulong)LockstepRecoveryFailureReason.HistoryTrimmed ||
                reason > (ulong)LockstepRecoveryFailureReason.RecoveryExpired || start > uint.MaxValue ||
                ((LockstepRecoveryFailureReason)reason == LockstepRecoveryFailureReason.HistoryTrimmed && from >= start))
                throw new InvalidDataException("Invalid catch-up notice.");
            return new LockstepCatchupUnservableException(from, (uint)start, (LockstepRecoveryFailureReason)reason);
        }

        // 严格解码CA 01分块（与Go lockstep.DecodeSnapshotChunk同一规则）：
        //   chunk  = CA 01 transfer(uvarint≠0) offset(uvarint) [header，仅offset=0] data(>=1B)
        //   header = frame(uvarint32) total(uvarint 1..maxTotal) flags(1B，bit0=带状态hash，其余位为0) [stateHash(uvarint64)] digest(32B)
        // offset>=maxTotal、首块数据超过total也是坏包；非首块是否越过total由重组器判断。
        internal static LockstepSnapshotChunk DecodeSnapshotChunk(ReadOnlySpan<byte> data, int maxTotal)
        {
            if (data.Length < 5 || data[0] != 0xCA || data[1] != 1) throw new InvalidDataException("Invalid snapshot chunk header.");
            int offset = 2;
            ulong transfer = ReadVarint(data, ref offset);
            ulong chunkOffset = ReadVarint(data, ref offset);
            if (transfer == 0 || chunkOffset >= (ulong)maxTotal) throw new InvalidDataException("Invalid snapshot transfer/offset.");
            uint frame = 0; ulong total = 0, stateHash = 0; bool hasStateHash = false;
            ReadOnlySpan<byte> digest = default;
            if (chunkOffset == 0)
            {
                ulong id = ReadVarint(data, ref offset);
                total = ReadVarint(data, ref offset);
                // 帧号可以是0：MinFrame = 最早保留帧 - 1，开局快照对应第0帧（初始状态），恢复后从第1帧模拟。
                if (id > uint.MaxValue || total == 0 || total > (ulong)maxTotal || offset == data.Length)
                    throw new InvalidDataException("Invalid snapshot frame/total.");
                frame = (uint)id;
                byte flags = data[offset++];
                if ((flags & ~1) != 0) throw new InvalidDataException("Invalid snapshot flags.");
                if ((flags & 1) != 0) { stateHash = ReadVarint(data, ref offset); hasStateHash = true; }
                if (data.Length - offset < SnapshotDigestBytes) throw new InvalidDataException("Truncated snapshot digest.");
                digest = data.Slice(offset, SnapshotDigestBytes); offset += SnapshotDigestBytes;
                if ((ulong)(data.Length - offset) > total) throw new InvalidDataException("First snapshot chunk exceeds total.");
            }
            if (offset == data.Length) throw new InvalidDataException("Empty snapshot chunk.");
            return new LockstepSnapshotChunk(transfer, (int)chunkOffset, frame, (int)total, stateHash, hasStateHash, digest, data.Slice(offset));
        }

        public static IReadOnlyList<LockstepFrame> DecodeBroadcast(ReadOnlySpan<byte> data)
        {
            if (data.Length < 3 || data[0] != 0xC7 || data[1] != 1) throw new InvalidDataException("Invalid broadcast header.");
            int offset = 2; ulong count = ReadVarint(data, ref offset);
            if (count > 64) throw new InvalidDataException("Too many frames.");
            var frames = new List<LockstepFrame>((int)count); uint last = 0;
            for (ulong i = 0; i < count; i++)
            {
                uint id = ReadFrame(data, ref offset);
                if (id <= last) throw new InvalidDataException("Frames must increase."); last = id;
                ulong inputs = ReadVarint(data, ref offset);
                if (inputs > 256 || inputs > (ulong)(data.Length - offset)) throw new InvalidDataException("Too many inputs.");
                var items = new List<LockstepInput>((int)inputs);
                for (ulong j = 0; j < inputs; j++)
                {
                    ulong player = ReadVarint(data, ref offset);
                    if (player > uint.MaxValue) throw new InvalidDataException("Player overflow.");
                    items.Add(new LockstepInput(unchecked((int)(uint)player), ReadPayload(data, ref offset)));
                }
                frames.Add(new LockstepFrame(id, items));
            }
            if (offset != data.Length) throw new InvalidDataException("Broadcast trailing bytes.");
            return frames.AsReadOnly();
        }

        internal static uint ReadFrame(ReadOnlySpan<byte> data, ref int offset)
        {
            ulong value = ReadVarint(data, ref offset);
            if (value == 0 || value > uint.MaxValue) throw new InvalidDataException("Frame overflow/zero.");
            return (uint)value;
        }
        internal static byte[] ReadPayload(ReadOnlySpan<byte> data, ref int offset)
        {
            ulong length = ReadVarint(data, ref offset);
            if (length > 1024 || length > (ulong)(data.Length - offset)) throw new InvalidDataException("Invalid input length.");
            byte[] result = data.Slice(offset, (int)length).ToArray(); offset += (int)length; return result;
        }
        internal static ulong ReadVarint(ReadOnlySpan<byte> data, ref int offset)
        {
            ulong value = 0;
            for (int i = 0; i < 10; i++)
            {
                if (offset == data.Length) throw new InvalidDataException("Truncated varint.");
                byte b = data[offset++];
                if (i == 9 && b > 1) throw new InvalidDataException("Varint overflow.");
                value |= (ulong)(b & 127) << (7 * i);
                if (b < 128) return value;
            }
            throw new InvalidDataException("Varint overflow.");
        }
        internal static void WriteVarint(List<byte> data, ulong value)
        {
            while (value >= 128) { data.Add((byte)((value & 127) | 128)); value >>= 7; }
            data.Add((byte)value);
        }
    }
}
