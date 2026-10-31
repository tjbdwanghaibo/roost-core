using System;
using System.Threading;
using Roost.Native;
internal static class Program
{
    private static void Main(string[] args)
    {
        if (args.Length != 0)
        {
            Game(ushort.Parse(args[0]));
            return;
        }
        using var client = new NativeClient();
        client.Connect("not-an-ip", 1, "ticket");
        var deadline = DateTime.UtcNow.AddSeconds(3);
        while (DateTime.UtcNow < deadline)
        {
            if (client.Poll(out var message))
            {
                if (message.Type != NativeClient.EventType.Closed || message.Generation != 1 ||
                    message.Payload.Length == 0)
                    throw new Exception("C ABI event layout mismatch");
                Console.WriteLine("PASS managed/native PInvoke layout, worker error and disposal");
                return;
            }
            Thread.Sleep(1);
        }
        throw new Exception("Native worker did not report failure");
    }

    private static void Game(ushort port)
    {
        using var game = new NativeGameClient();
        using var retry = new NativeReconnect(
            game,
            _ => new NativeReconnect.Ticket { Address = "127.0.0.1", Port = port, Value = "ticket" }, 2, 0.001, 0.002);
        int ready = 0, responses = 0, pushes = 0, frames = 0, recovered = 0, restored = 0;
        bool acknowledged = false;
        NativeLockstep match = null;
        retry.Reconnected += () => recovered++;
        retry.Error += e => throw e;
        using var subscription = game.Subscribe(9, b => b,
                                                b =>
                                                {
                                                    if (b.Length != 2 || b[1] != 3)
                                                        throw new Exception("push");
                                                    pushes++;
                                                });
        game.Ready += () =>
        {
            ready++;
            match?.Dispose();
            match = new NativeLockstep(game, 50001, 50002,
                                       (frame, inputs) =>
                                       {
                                           if (frame == 1 && (inputs.Count != 2 || inputs[1].Player != -1))
                                               throw new Exception("native frame callback layout");
                                           frames++;
                                       },
                                       snapshot =>
                                       {
                                           if (System.Text.Encoding.UTF8.GetString(snapshot.Data) !=
                                               "roost-snapshot-golden-24")
                                               throw new Exception("native snapshot callback layout");
                                           restored++;
                                           return 9223372036854780468UL;
                                       });
            game.Request(42, 43, new byte[] { 8, 1 }, b => b,
                         b =>
                         {
                             if (b.Length != 2 || b[1] != 2)
                                 throw new Exception("PB response");
                             responses++;
                         },
                         e => throw e);
            match.Submit(1, new byte[] { 8, 42 });
        };
        game.Connect("127.0.0.1", port, "ticket");
        var deadline = DateTime.UtcNow.AddSeconds(5);
        try
        {
            while (DateTime.UtcNow < deadline)
            {
                game.Pump();
                retry.Tick();
                if (!acknowledged && responses == 1 && pushes == 1 && frames == 2 && restored == 1)
                {
                    acknowledged = true;
                    game.Notify(99, 0, Array.Empty<byte>(),
                                _ =>
                                {});
                }
                if (ready == 2 && recovered == 1 && responses == 2 && pushes == 2 && frames == 4 && restored == 2)
                {
                    retry.Stop();
                    game.Disconnect();
                    Console.WriteLine("PASS managed/native PB callbacks, Lockstep reverse PInvoke, reconnect");
                    return;
                }
                Thread.Sleep(1);
            }
            throw new Exception("managed game deadline");
        }
        finally
        {
            match?.Dispose();
        }
    }
}
