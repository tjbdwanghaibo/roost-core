using System;
using System.Threading;
using Roost.Client;
using Roost.Client.Codecs;
using Roost.Dao.Sync;
using Roost.Protocol;
using Roost.Generated;

// 消费正式生成工程的协议，无复制的 DTO。此入口由 Go 联合验证宿主启动。
internal static class Program
{
    private static void Main(string[] args)
    {
        using var client = new GameClient(new ConnectionOptions {
            Host = "127.0.0.1", Port = int.Parse(args[0]), AutoReconnect = false,
            HeartbeatInterval = TimeSpan.FromMilliseconds(100), RequestTimeout = TimeSpan.FromSeconds(5)
        });
        client.Entities.RegisterDao("player", SyncPlayerDao.Parser, (patch, previous) => patch.Apply(previous), profile: "default");
        int full = 0, delta = 0, removed = 0;
        SyncPlayerDao? current = null;
        Exception? failure = null;
        client.Error += error => failure = error;
        client.EntityChanged += change => {
            if (change.Kind == EntityChangeKind.Remove) { removed++; current = null; return; }
            current = (SyncPlayerDao)change.Current!.Value;
            if (change.Kind == EntityChangeKind.Full) full++; else delta++;
        };
        void Wait(Func<bool> ready) {
            var deadline = DateTime.UtcNow.AddSeconds(10);
            while (!ready()) {
                client.Pump();
                if (failure != null) throw failure;
                if (DateTime.UtcNow >= deadline) throw new TimeoutException("Demo chain deadline exceeded");
                Thread.Sleep(1);
            }
        }
        bool rejected = false;
        try { client.Connection.ConnectAsync("invalid-ticket").GetAwaiter().GetResult(); }
        catch { rejected = true; }
        if (!rejected || client.Connection.State == ConnectionState.Ready) throw new Exception("Invalid ticket accepted");
        client.Connection.ConnectAsync(Environment.GetEnvironmentVariable("ROOST_GATE_TICKET") ?? "ticket").GetAwaiter().GetResult();
        Wait(() => full == 1);
        int before = current!.Profile?.Value?.Level ?? 0;
        bool response = false;
        client.Request(MessageIds.RequestAddExp, MessageIds.ResponseAddExp, new AddExpRequest { Amount = 250 }, AddExpResponse.Parser,
            result => {
                if (result.Code != 0 || result.LevelsGained <= 0) throw new Exception("AddExp rejected");
                response = true;
            }, error => failure = error);
        Wait(() => response && delta > 0);
        if ((current?.Profile?.Value?.Level ?? 0) <= before) throw new Exception("DAO patch did not update level");
        var committed = current!.Clone();
        // 测试宿主在这条观察屏障之后撤销订阅；不是生产网络中的控制协议。
        Console.WriteLine("READY_REMOVE"); Console.Out.Flush();
        Wait(() => removed == 1);
        if (client.Entities.Current.Count != 0) throw new Exception("Remove retained a view");
        client.Connection.Disconnect(); client.Pump();
        client.Connection.ConnectAsync(Environment.GetEnvironmentVariable("ROOST_GATE_TICKET") ?? "ticket").GetAwaiter().GetResult();
        Wait(() => full == 2);
        if (!committed.Equals(current)) throw new Exception("Reconnect snapshot differs from committed state");
        Console.WriteLine("PASS C# Gate/PB/Nest/DAO/Sync full/delta/remove/reconnect");
    }
}
