using System;
using Roost.Client;

internal static partial class Program
{
    private static void TestNetworkExperience()
    {
        var clock = new NetworkClock();
        Check(clock.Observe(new ClockSample(0, .02, 100.01, 100.01)), "clock first sample");
        Check(Math.Abs(clock.Now(1) - 101) < 1e-9, "symmetric clock estimate");
        Check(!clock.Observe(new ClockSample(1, 1.5, 101.1, 101.1)), "queued clock outlier");
        Check(clock.Observe(new ClockSample(2, 2.02, 101.91, 101.91)), "clock correction sample");
        double previous = clock.Now(2.02);
        Check(clock.Now(2.03) >= previous, "clock cannot move backwards");
        Refuses(() => clock.Now(2), "local time moved backwards", typeof(ArgumentOutOfRangeException));
        clock.Reset();
        Refuses(() => clock.Now(3), "reset requires sample", typeof(InvalidOperationException));

        var snapshots = new SnapshotBuffer<double>(2, 0, .2);
        snapshots.Add(1, 1, 0);
        snapshots.Add(1.1, 1.1, 10);
        Check(!snapshots.Add(1, 1.2, 100), "stale snapshot");
        Check(snapshots.TrySample(1.15, (a, b, t) => a + (b - a) * t, out var value) && Math.Abs(value - 5) < 1e-8,
              "interpolation");
        snapshots.Add(1.2, 2, 20);
        Check(snapshots.Count == 2 && snapshots.Delay <= .2, "bounded jitter and capacity");
        snapshots.TrySample(100, (a, b, t) => a + (b - a) * t, out value);
        Check(value == 20, "stall holds last value");
        snapshots.Add(1.3, 100, 30);
        snapshots.TrySample(1.45, (a,b,t)=>a+(b-a)*t, out value);
        Check(Math.Abs(value-25)<1e-8, "stall must not advance playhead beyond displayed sample");
        snapshots.Clear();
        Check(snapshots.Count == 0, "snapshot reset");

        var history = new PredictionHistory<int>(2);
        history.Record(1, 1);
        history.Record(2, 2);
        history.Record(3, 3);
        Check(history.Held && history.Replay(10, s => s, (s, i) => s + i) == 10, "overflow pauses prediction");
        history.Acknowledge(1, out _);
        Check(!history.Held && history.Replay(10, s => s, (s, i) => s + i) == 15, "ack repairs prediction gap");
        history.Acknowledge(3, out var last);
        Check(last == 3 && history.Count == 0, "prediction ack removes history");
        Refuses(() => history.Acknowledge(2, out _), "old ack", typeof(ArgumentException));

        var trace = new ClientTrace(2);
        for (uint i = 1; i <= 3; i++)
            trace.Record(new ClientTraceEvent(ClientStage.Apply, 1, i, i));
        var entries = trace.Drain();
        Check(trace.Overwritten == 1 && entries.Length == 2 && entries[0].Tick == 2 && trace.Drain().Length == 0,
              "trace bounded ring");
        Console.WriteLine("PASS network clock/interpolation/prediction/trace");
    }
}
