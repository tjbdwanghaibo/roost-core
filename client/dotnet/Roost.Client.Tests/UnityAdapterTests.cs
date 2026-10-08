using System;
using System.IO;
using System.Net;
using System.Net.Sockets;
using System.Reflection;
using System.Threading.Tasks;
using Roost.Client;
using Roost.Unity;

// 只替代引擎宿主类型；被测RoostConnection直接链接正式源码，连接使用真实TcpSession。
// 此文件不在Unity package中，不能进入真实引擎并覆盖UnityEngine。
namespace UnityEngine
{
    public class MonoBehaviour { }
    [AttributeUsage(AttributeTargets.Field)] public sealed class SerializeField : Attribute { }
    public static class Debug { public static void LogException(Exception error) => Console.Error.WriteLine(error); }
}

internal static partial class Program
{
    private static async Task TestUnitySyncAdapter()
    {
        foreach (bool applicationFailure in new[] { false, true })
        {
            var listener = new TcpListener(IPAddress.Loopback, 0);
            listener.Start();
            int port = ((IPEndPoint)listener.LocalEndpoint).Port;
            var sent = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            var peer = Task.Run(async () =>
            {
                using var tcp = await listener.AcceptTcpClientAsync();
                using var stream = tcp.GetStream();
                var auth = await Read(stream);
                await Send(stream, new Packet(0, auth.Sequence, Array.Empty<byte>()));
                await Send(stream, new Packet(10103, 1, SyncPacket(1, 1, 0), PayloadKind.Sync, true));
                await Send(stream, new Packet(10103, 2, SyncPacket(1, 3, 2), PayloadKind.Sync, true));
                sent.SetResult();
                try { await Read(stream); } catch (EndOfStreamException) { }
            });
            var adapter = new RoostConnection();
            int applied = 0, disconnected = 0;
            Exception? reason = null;
            adapter.PacketReceived += _ =>
            {
                applied++;
                if (applicationFailure) throw new InvalidOperationException("application failed");
            };
            adapter.Disconnected += error => { disconnected++; reason = error; };
            try
            {
                using var session = await TcpSession.ConnectAsync("127.0.0.1", port, "ticket", TimeSpan.FromSeconds(5));
                adapter.Attach(session);
                await sent.Task.WaitAsync(TimeSpan.FromSeconds(5));
                var update = typeof(RoostConnection).GetMethod("Update", BindingFlags.Instance | BindingFlags.NonPublic)!;
                var deadline = DateTime.UtcNow.AddSeconds(5);
                while (disconnected == 0 && DateTime.UtcNow < deadline)
                {
                    update.Invoke(adapter, null);
                    await Task.Delay(1);
                }
                Check(disconnected == 1 && applied == 1, "Unity continued applying after Sync failure");
                Check(applicationFailure ? reason is InvalidOperationException : reason is InvalidDataException, "Unity lost failure reason");
                await session.Completion.WaitAsync(TimeSpan.FromSeconds(5));
                await peer.WaitAsync(TimeSpan.FromSeconds(5));
            }
            finally { adapter.Detach(); listener.Stop(); }
        }
        Console.WriteLine("PASS Unity adapter source + real TCP: Sync gap/application failure closes stream");
    }
}
