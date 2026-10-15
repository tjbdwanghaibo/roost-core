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

    // 服务端追帧失败通知（C9 v1）：客户端缺的帧已被裁出服务端历史，本场无法继续。
    // 收到后LockstepClient终止；由业务决定踢出或开新局，SDK不自动重同步、不跳过缺帧。
    public sealed class LockstepCatchupUnservableException : Exception
    {
        public uint RequestedFrom { get; }
        public uint HistoryStart { get; }
        public LockstepCatchupUnservableException(uint requestedFrom, uint historyStart)
            : base($"Lockstep catch-up from frame {requestedFrom} cannot be served: server history starts at {historyStart}.")
        { RequestedFrom = requestedFrom; HistoryStart = historyStart; }
    }

    // 直接消费sync/lockstep的C7 v1广播和追帧页，不另定义客户端帧格式。
    public static class LockstepCodec
    {
        public static bool IsCatchupNotice(ReadOnlySpan<byte> data) => data.Length > 0 && data[0] == 0xC9;

        // 严格解码C9 01 from(uvarint) start(uvarint)，要求from<start；坏包抛InvalidDataException。
        public static LockstepCatchupUnservableException DecodeCatchupUnservable(ReadOnlySpan<byte> data)
        {
            if (data.Length < 4 || data[0] != 0xC9 || data[1] != 1) throw new InvalidDataException("Invalid catch-up notice header.");
            int offset = 2;
            uint from = ReadFrame(data, ref offset);
            ulong start = ReadVarint(data, ref offset);
            if (offset != data.Length || start > uint.MaxValue || from >= start) throw new InvalidDataException("Invalid catch-up notice.");
            return new LockstepCatchupUnservableException(from, (uint)start);
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
