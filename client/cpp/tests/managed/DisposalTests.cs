using System;
using System.Threading;
using System.Net;
using System.Net.Sockets;
using System.Buffers.Binary;
using System.Threading.Tasks;
using Roost.Native;

internal static class DisposalTests
{
    // 真实 native 关闭事件触发 Error，销毁后不得继续通知观察者。
    public static void Run()
    {
        foreach (int disposeAt in new[] { -1, 0, 1 })
        {
            using var game = new NativeGameClient();
            int calls = 0;
            for (int i = 0; i < 3; i++)
            {
                int subscriber = i;
                game.Error += _ =>
                {
                    calls++;
                    if (subscriber == disposeAt) game.Dispose();
                };
            }
            game.Connect("not-an-ip", 1, "ticket");
            var deadline = DateTime.UtcNow.AddSeconds(3);
            while (calls == 0 && DateTime.UtcNow < deadline)
            {
                game.Pump();
                Thread.Sleep(1);
            }
            int expected = disposeAt < 0 ? 3 : disposeAt + 1;
            if (calls != expected)
                throw new Exception($"native Error callbacks after disposal: calls={calls}, expected={expected}");
        }
        foreach (string mode in new[] { "request", "parse", "failure", "notify", "push", "push-parse", "clock" })
            CheckResult(mode);
        Console.WriteLine("PASS managed/native error, request, parser, notification, push and clock disposal boundaries");
    }
    private static void CheckResult(string mode)
    {
        using var listener = new TcpListener(IPAddress.Loopback, 0);
        listener.Start();
        ushort port = (ushort)((IPEndPoint)listener.LocalEndpoint).Port;
        var peer = Task.Run(() =>
        {
            using var socket = listener.AcceptTcpClient();
            socket.ReceiveTimeout = socket.SendTimeout = 3000;
            using var stream = socket.GetStream();
            byte[] hello = ReadPacket(stream);
            Send(stream, 0, BinaryPrimitives.ReadUInt32BigEndian(hello.AsSpan(8)), Array.Empty<byte>());
            if (mode.StartsWith("push"))
                Send(stream, 9, 0, new byte[] { 8, 1 }, 1);
            else
            {
                byte[] request = ReadPacket(stream);
                if (mode == "failure") return;
                uint seq = BinaryPrimitives.ReadUInt32BigEndian(request.AsSpan(8));
                if (mode == "request" || mode == "parse") Send(stream, 43, seq, new byte[] { 8, 1 });
                if (mode == "clock")
                {
                    byte[] sample = new byte[20];
                    System.Text.Encoding.ASCII.GetBytes("RCN1").CopyTo(sample, 0);
                    BinaryPrimitives.WriteInt64BigEndian(sample.AsSpan(4), 1000000000);
                    BinaryPrimitives.WriteInt64BigEndian(sample.AsSpan(12), 1000000000);
                    Send(stream, 0, seq, sample);
                }
            }
            // 由客户端销毁结束连接，避免服务端提前关闭抢走已发回执。
            while (stream.ReadByte() != -1) { }
        });
        using var game = new NativeGameClient();
        bool disposed = false;
        int unexpected = 0;
        void Stop() { disposed = true; game.Dispose(); }
        Action<byte[]> completed = _ => Stop();
        completed += _ => unexpected++;
        Action<Exception> failed = _ => Stop();
        failed += _ => unexpected++;
        byte[] Parse(byte[] bytes)
        {
            if (mode == "parse" || mode == "push-parse") Stop();
            return bytes;
        }
        game.Error += _ => unexpected++;
        if (mode.StartsWith("push"))
            game.Subscribe(9, Parse, mode == "push-parse" ? _ => unexpected++ : completed);
        game.Ready += () =>
        {
            if (mode.StartsWith("push")) return;
            if (mode == "notify")
            {
                Action<NativeClient.Event> sent = _ => Stop();
                sent += _ => unexpected++;
                game.Notify(42, 0, Array.Empty<byte>(), sent);
            }
            else if (mode == "clock")
            {
                Action<Roost.Client.ClockSample> clock = _ => Stop();
                clock += _ => unexpected++;
                game.ProbeClock(clock, _ => unexpected++);
            }
            else
            {
                game.Request(42, 43, Array.Empty<byte>(), Parse,
                    mode == "parse" ? _ => unexpected++ : completed, failed);
            }
        };
        game.Connect("127.0.0.1", port, "ticket");
        var deadline = DateTime.UtcNow.AddSeconds(3);
        while (!disposed && DateTime.UtcNow < deadline) { game.Pump(); Thread.Sleep(1); }
        if (!disposed || unexpected != 0)
            throw new Exception($"{mode} disposal: disposed={disposed}, unexpected={unexpected}");
        if (!peer.Wait(TimeSpan.FromSeconds(3))) throw new Exception("peer deadline");
    }

    private static byte[] ReadPacket(NetworkStream stream)
    {
        byte[] header = new byte[16];
        stream.ReadExactly(header);
        int size = checked((int)BinaryPrimitives.ReadUInt32BigEndian(header.AsSpan(12)));
        if (size > 1024) throw new Exception("unexpected packet size");
        stream.ReadExactly(new byte[size]);
        return header;
    }

    private static void Send(NetworkStream stream, uint id, uint sequence, byte[] payload, byte kind = 0)
    {
        byte[] packet = new byte[16 + payload.Length];
        packet[0] = (byte)'R'; packet[1] = (byte)'S'; packet[2] = 2; packet[3] = kind;
        BinaryPrimitives.WriteUInt32BigEndian(packet.AsSpan(4), id);
        BinaryPrimitives.WriteUInt32BigEndian(packet.AsSpan(8), sequence);
        BinaryPrimitives.WriteUInt32BigEndian(packet.AsSpan(12), (uint)payload.Length);
        payload.CopyTo(packet, 16);
        stream.Write(packet);
    }

}
