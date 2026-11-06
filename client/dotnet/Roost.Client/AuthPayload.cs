using System;
using System.Buffers.Binary;
using System.Threading;

namespace Roost.Client
{
    // 鉴权首包（message_id=0 的第一帧）的载荷：带连接代号的信封 0x00、'G'、8 字节大端代号（非 0）、票据原文
    // （client/README“鉴权首包的连接代号”，Go 侧 client/wire.EncodeAuth）。同一 SessionID 的两条连接落在不同 Gate
    // 实例时，Game 按代号认出迟到完成的旧握手，不让它断开更新的连接；服务端在鉴权前剥掉信封，业务鉴权只看到票据原文。
    // TcpSession.ConnectAsync 每次连接（包括 Connection 的重连）自动取新代号并写成信封，调用方不需要使用本类。
    public static class AuthPayload
    {
        private const int Overhead = 2 + 8;
        private static long lastGeneration;
        private static readonly DateTime UnixEpoch = new DateTime(1970, 1, 1, 0, 0, 0, DateTimeKind.Utc);

        // 写出信封；票据为空或以 0x00 开头（服务端会把它当成坏信封）时抛 ArgumentException，代号必须非 0。
        public static byte[] Encode(ReadOnlySpan<byte> ticket, ulong generation)
        {
            if (ticket.IsEmpty || ticket[0] == 0)
                throw new ArgumentException("Ticket must be non-empty and must not start with a NUL byte.", nameof(ticket));
            if (generation == 0) throw new ArgumentOutOfRangeException(nameof(generation), "Connection generation must be non-zero.");
            var payload = new byte[Overhead + ticket.Length];
            payload[0] = 0; payload[1] = (byte)'G';
            BinaryPrimitives.WriteUInt64BigEndian(payload.AsSpan(2, 8), generation);
            ticket.CopyTo(payload.AsSpan(Overhead));
            return payload;
        }

        // 拆开鉴权首包：信封返回票据原文与代号；不以 0x00 开头的是票据原文、代号为 0；以 0x00 开头却不是完整
        // 信封（标记不对、代号为 0、票据为空或以 0x00 开头）时抛 FormatException，与服务端拒绝规则一致。
        public static byte[] Decode(ReadOnlySpan<byte> payload, out ulong generation)
        {
            generation = 0;
            if (payload.IsEmpty) throw new FormatException("Empty authentication payload.");
            if (payload[0] != 0) return payload.ToArray();
            if (payload.Length <= Overhead || payload[1] != (byte)'G') throw new FormatException("Malformed authentication envelope.");
            ulong value = BinaryPrimitives.ReadUInt64BigEndian(payload.Slice(2, 8));
            if (value == 0 || payload[Overhead] == 0) throw new FormatException("Malformed authentication envelope.");
            generation = value;
            return payload.Slice(Overhead).ToArray();
        }

        // 新的连接代号：max(上一个 + 1, 当前 UTC Unix 纳秒)，上一个只存在本进程内存。进程内严格递增、不受时钟回拨
        // 影响；进程重启后从当时的时钟起算，自然大于旧进程发过的代号（重启前后时钟被往回拨时可能偏小，后果只是与旧
        // 进程仍在途的握手重叠时被拒一次，重连即恢复）。DateTime 精度为 100 纳秒，换算后仍是纳秒单位。
        public static ulong NextConnectionGeneration()
        {
            while (true)
            {
                long last = Interlocked.Read(ref lastGeneration);
                ulong now = (ulong)(DateTime.UtcNow - UnixEpoch).Ticks * 100UL;
                ulong next = Math.Max(unchecked((ulong)last) + 1, now);
                if (Interlocked.CompareExchange(ref lastGeneration, unchecked((long)next), last) == last)
                    return next;
            }
        }
    }
}
