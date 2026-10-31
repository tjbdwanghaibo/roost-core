using System;
using System.Threading;
using Roost.Native;
internal static class Program
{
    private static void Main()
    {
        using var client = new NativeClient();
        client.Connect("not-an-ip", 1, "ticket");
        var deadline = DateTime.UtcNow.AddSeconds(3);
        while (DateTime.UtcNow < deadline)
        {
            if (client.Poll(out var message))
            {
                if (message.Type != NativeClient.EventType.Closed || message.Generation != 1 || message.Payload.Length == 0) throw new Exception("C ABI event layout mismatch");
                Console.WriteLine("PASS managed/native PInvoke layout, worker error and disposal"); return;
            }
            Thread.Sleep(1);
        }
        throw new Exception("Native worker did not report failure");
    }
}
