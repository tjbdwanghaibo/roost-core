using System;
using System.Net;
using System.Net.Sockets;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client;

internal static partial class Program
{
    // RV-20261101-04：业务容量满不能阻止心跳；真正无应答仍须按期限断线。
    private static async Task TestHeartbeatCapacity()
    {
        using var deadline = new CancellationTokenSource(TimeSpan.FromSeconds(10));
        using var listener = new TcpListener(IPAddress.Loopback, 0);
        listener.Start();
        var peer = Task.Run(async () =>
        {
            using var tcp = await listener.AcceptTcpClientAsync(deadline.Token);
            var stream = tcp.GetStream();
            var auth = await Read(stream);
            await Send(stream, new Packet(0, auth.Sequence, Array.Empty<byte>()));
            var request = await Read(stream);
            Check(request.MessageId == 42, "business admitted before heartbeat");
            var heartbeat = await Read(stream);
            Check(heartbeat.MessageId == 0, "heartbeat admitted while business slot occupied");
            await Send(stream, new Packet(0, heartbeat.Sequence, Array.Empty<byte>()));
            await Send(stream, new Packet(43, request.Sequence, Array.Empty<byte>()));
            // 后续请求正常，下一次心跳故意不回；等待客户端按心跳期限关闭。
            while (true)
            {
                try
                {
                    var packet = await Read(stream);
                    if (packet.MessageId == 42)
                        await Send(stream, new Packet(43, packet.Sequence, Array.Empty<byte>()));
                }
                catch (System.IO.IOException) { break; }
            }
        });
        using var connection = new Connection(new ConnectionOptions
        {
            Host = "127.0.0.1", Port = ((IPEndPoint)listener.LocalEndpoint).Port,
            MaxPendingRequests = 1, HeartbeatInterval = TimeSpan.FromMilliseconds(100),
            HeartbeatTimeout = TimeSpan.FromMilliseconds(500), RequestTimeout = TimeSpan.FromSeconds(3)
        });
        await connection.ConnectAsync("ticket");
        var first = connection.RequestAsync(42, 43, ReadOnlyMemory<byte>.Empty);
        try
        {
            await connection.RequestAsync(42, 43, ReadOnlyMemory<byte>.Empty);
            throw new Exception("business capacity was enlarged");
        }
        catch (InvalidOperationException) { }
        await first;
        Check(connection.State == ConnectionState.Ready, "local backpressure disconnected healthy session");
        await connection.RequestAsync(42, 43, ReadOnlyMemory<byte>.Empty);
        while (connection.State == ConnectionState.Ready)
            await Task.Delay(10, deadline.Token);
        Check(connection.LastError != null, "missing heartbeat did not fail session");
        await peer.WaitAsync(deadline.Token);
        Console.WriteLine("PASS heartbeat capacity and genuine timeout");
    }
}
