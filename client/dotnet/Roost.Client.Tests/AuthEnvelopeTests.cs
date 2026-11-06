using System;
using System.Buffers.Binary;
using System.Text;
using System.Text.Json;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client;

// 鉴权首包的连接代号（client/README“鉴权首包的连接代号”）：金样逐字节核对、重连代号递增、跨 Gate 放弃握手。
internal static partial class Program
{
    // 测试侧独立拆开鉴权首包：0x00、'G'、8 字节大端代号、票据原文；不是信封时代号为 0。不复用运行库的编码，
    // 让“运行库发出的字节是否带代号”由测试自己判定。
    private static (string Ticket, ulong Generation) ParseAuth(ReadOnlySpan<byte> payload)
    {
        if (payload.Length > 10 && payload[0] == 0 && payload[1] == (byte)'G')
            return (Encoding.UTF8.GetString(payload.Slice(10)), BinaryPrimitives.ReadUInt64BigEndian(payload.Slice(2, 8)));
        return (Encoding.UTF8.GetString(payload), 0);
    }

    // Go 金样 auth / auth_generation：运行库用同一票据与代号编码后逐字节等于 Go 的载荷，解码出同一票据与代号；
    // 代号最高位为 1，检验按无符号 64 位大端写出。
    private static void CheckAuthGolden(string name, JsonElement item, Packet packet)
    {
        if (name != "auth" && name != "auth_generation") return;
        Check(packet.MessageId == 0 && packet.Sequence == 1 && packet.Kind == PayloadKind.Protobuf && !packet.IsPush, "auth control frame");
        byte[] ticket = AuthPayload.Decode(packet.Payload.Span, out ulong generation);
        Check(Encoding.UTF8.GetString(ticket) == "ticket", "Go/C# auth ticket");
        if (name == "auth") { Check(generation == 0, "plain ticket has no generation"); return; }
        ulong want = ulong.Parse(item.GetProperty("generation").GetString()!);
        Check(generation == want && want >= 1UL << 63, "Go/C# auth generation");
        Check(Convert.ToHexString(AuthPayload.Encode(Encoding.UTF8.GetBytes("ticket"), want)) == Convert.ToHexString(packet.Payload.Span), "Go/C# auth envelope bytes");
    }

    // 编码 / 解码的拒绝规则与 Go client/wire.DecodeAuth 一致；代号进程内严格递增、取自 UTC 纳秒时钟。
    private static void TestAuthPayload()
    {
        Refuses(() => AuthPayload.Encode(ReadOnlySpan<byte>.Empty, 1), "empty ticket", typeof(ArgumentException));
        Refuses(() => AuthPayload.Encode(new byte[] { 0, (byte)'t' }, 1), "NUL-led ticket", typeof(ArgumentException));
        Refuses(() => AuthPayload.Encode(new byte[] { (byte)'t' }, 0), "zero generation", typeof(ArgumentOutOfRangeException));
        foreach (var bad in new[]
        {
            new byte[0], new byte[] { 0 }, new byte[] { 0, (byte)'G', 0, 0, 0, 0, 0, 0, 0, 1 },
            new byte[] { 0, (byte)'X', 0, 0, 0, 0, 0, 0, 0, 1, (byte)'t' },
            new byte[] { 0, (byte)'G', 0, 0, 0, 0, 0, 0, 0, 0, (byte)'t' },
            new byte[] { 0, (byte)'G', 0, 0, 0, 0, 0, 0, 0, 1, 0, (byte)'t' },
        })
            Refuses(() => AuthPayload.Decode(bad, out _), "malformed auth envelope", typeof(FormatException));
        Refuses(() => TcpSession.ConnectAsync("127.0.0.1", 1, "\0ticket", TimeSpan.FromSeconds(1)).GetAwaiter().GetResult(), "NUL-led ticket on connect", typeof(ArgumentException));
        ulong before = (ulong)(DateTime.UtcNow - new DateTime(1970, 1, 1, 0, 0, 0, DateTimeKind.Utc)).Ticks * 100UL;
        ulong previous = AuthPayload.NextConnectionGeneration();
        Check(previous >= before, "generation starts from the UTC nanosecond clock");
        for (int i = 0; i < 1000; i++)
        {
            ulong next = AuthPayload.NextConnectionGeneration();
            Check(next > previous, "generation strictly increases within the process");
            previous = next;
        }
        Console.WriteLine("PASS auth payload envelope/generation");
    }

    // 跨 Gate 放弃握手的客户端一侧，由 infra/network/gateway 的
    // TestAcrossGatesTheCSharpClientThatAbandonsAHandshakeKeepsItsNewerConnection 驱动；先后经标准输入输出控制：
    // 在 Gate A 上发起握手 → 等 abandon（Go 确认这次 Bind 已停在 Game 鉴权里）→ 取消它 → 在 Gate B 上握手并答完
    // 一个请求、打印 new-ready → 等 check（Go 已放行旧 Bind 并看到 Game 的结果）→ 在新连接上再请求一次。
    // 新连接还能应答打印 new-alive 并以 0 退出，否则打印 new-closed 并以 1 退出。
    private static async Task<int> CrossGate(int portA, int portB, uint requestId, uint responseId)
    {
        using (var abandon = new CancellationTokenSource())
        {
            var old = TcpSession.ConnectAsync("127.0.0.1", portA, "old", TimeSpan.FromSeconds(30), abandon.Token);
            Console.WriteLine("old-started");
            if (await Console.In.ReadLineAsync() != "abandon") return 2;
            abandon.Cancel();
            try
            {
                (await old).Dispose();
                Console.WriteLine("old-acknowledged");
                return 1;
            }
            catch (OperationCanceledException) { Console.WriteLine("old-abandoned"); }
        }
        using var newer = await TcpSession.ConnectAsync("127.0.0.1", portB, "new", TimeSpan.FromSeconds(10));
        async Task Echo(string text)
        {
            byte[] body = Encoding.UTF8.GetBytes(text);
            Packet reply = await newer.RequestAsync(requestId, responseId, body, TimeSpan.FromSeconds(2));
            Check(reply.Payload.Span.SequenceEqual(body), "echo on the newer connection");
        }
        await Echo("first");
        Console.WriteLine("new-ready");
        if (await Console.In.ReadLineAsync() != "check") return 2;
        try
        {
            await Echo("after-late-bind");
            Console.WriteLine("new-alive");
            return 0;
        }
        catch (Exception error)
        {
            Console.WriteLine("new-closed " + error.GetType().Name + ": " + error.Message);
            return 1;
        }
    }
}
