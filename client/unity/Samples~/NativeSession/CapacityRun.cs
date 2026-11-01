using System;
using System.Diagnostics;
using System.IO;
using Roost.Client;
using Roost.Dao.Sync;
using UnityEngine;
using UnityEngine.Profiling;
namespace Roost.Unity.Samples
{
public static class CapacityRun
{
    [Serializable]
    private sealed class Result
    {
        public int visible, dirty_percent, ticks, gc_collections;
        public double cold_ms, apply_p50_us, apply_p99_us, apply_max_us, allocated_bytes_per_tick;
        public long mono_used_bytes, unity_allocated_bytes;
        public bool verified;
    }
    private static long Allocated()
    {
        try
        {
            return GC.GetAllocatedBytesForCurrentThread();
        }
        catch (NotSupportedException)
        {
            return -1;
        }
    }
    public static void Run(string input, string output)
    {
        var result = new Result();
        using (var reader = new BinaryReader(File.OpenRead(input)))
        {
            if (System.Text.Encoding.ASCII.GetString(reader.ReadBytes(4)) != "RCB1")
                throw new Exception("Invalid corpus");
            result.visible = reader.ReadInt32();
            result.dirty_percent = reader.ReadInt32();
            var views = new EntityViews();
            views.Register<SyncHeroDao>("hero", "default", 2, 1,
                                        (update, previous) =>
                                            SyncHeroDao.Parser.ParseFrom(update.Payload).Apply(previous));
            byte[] Packet()
            {
                int size = reader.ReadInt32();
                if (size < 0 || size > 4 * 1024 * 1024)
                    throw new Exception("Invalid packet size");
                var data = reader.ReadBytes(size);
                if (data.Length != size)
                    throw new EndOfStreamException();
                return data;
            }
            var watch = Stopwatch.StartNew();
            int cold = reader.ReadInt32();
            for (int i = 0; i < cold; i++)
                if (!views.Receive(Packet()))
                    throw new Exception("Cold frame rejected");
            result.cold_ms = watch.Elapsed.TotalMilliseconds;
            result.ticks = reader.ReadInt32();
            var times = new double[result.ticks];
            long allocations = 0;
            // 部分 Unity 运行时返回固定 0，而不是抛出 NotSupportedException。
            // 先验证计数器确实观察到分配，不能把未支持的指标写成零分配。
            long probeBefore = Allocated();
            var allocationProbe = new byte[4096];
            long probeAfter = Allocated();
            GC.KeepAlive(allocationProbe);
            bool tracked = probeBefore >= 0 && probeAfter > probeBefore;
            int collections = GC.CollectionCount(0);
            for (int t = 0; t < result.ticks; t++)
            {
                int count = reader.ReadInt32();
                var packets = new byte[count][];
                for (int i = 0; i < count; i++)
                    packets[i] = Packet();
                long before = Allocated();
                watch.Restart();
                foreach (var packet in packets)
                    if (!views.Receive(packet))
                        throw new Exception("Delta rejected");
                times[t] = watch.Elapsed.TotalMilliseconds * 1000;
                long after = Allocated();
                if (before >= 0 && after >= 0)
                    allocations += after - before;
                else
                    tracked = false;
            }
            if (views.Current.Count != result.visible || reader.BaseStream.Position != reader.BaseStream.Length)
                throw new Exception("Corpus count mismatch");
            long updates = (long)result.ticks * result.visible * result.dirty_percent / 100;
            foreach (var entry in views.Current)
            {
                var value = (SyncHeroDao)entry.Value.Value;
                long expected = 1 + updates / result.visible + (value.EntityId <= updates % result.visible ? 1 : 0);
                if (value.Level.Value != expected)
                    throw new Exception("Final DAO mismatch");
            }
            Array.Sort(times);
            result.apply_p50_us = times[result.ticks / 2 - 1];
            result.apply_p99_us = times[(int)Math.Ceiling(result.ticks * .99) - 1];
            result.apply_max_us = times[result.ticks - 1];
            result.allocated_bytes_per_tick = tracked ? (double)allocations / result.ticks : -1;
            result.gc_collections = GC.CollectionCount(0) - collections;
            result.mono_used_bytes = Profiler.GetMonoUsedSizeLong();
            result.unity_allocated_bytes = Profiler.GetTotalAllocatedMemoryLong();
            result.verified = true;
        }
        File.WriteAllText(output, JsonUtility.ToJson(result, true));
    }
}
}
