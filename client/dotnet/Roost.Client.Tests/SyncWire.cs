using System.Collections.Generic;
using System.Text;

// 测试用的帧协议2 / subject 信封 v3 编码（与 Go framework/sync/frame、entitysync 一致），只用来构造金样之外的帧。
internal static class SyncWire
{
    private static void Varint(List<byte> b, ulong v) { while (v >= 0x80) { b.Add((byte)(v | 0x80)); v >>= 7; } b.Add((byte)v); }

    public static byte[] Frame(bool full, uint epoch, uint tick, uint baseline, params byte[][] objects)
    {
        var b = new List<byte> { 0x52, 0x46, 2, full ? (byte)1 : (byte)2 };
        Varint(b, 1); Varint(b, epoch); Varint(b, tick);
        if (!full) Varint(b, tick - baseline);
        Varint(b, 1); Varint(b, (ulong)objects.Length);
        foreach (var o in objects) b.AddRange(o);
        return b.ToArray();
    }

    // 带一个类型 1、schema 1 组件的对象（create 带 archetype 1）。
    public static byte[] Object(byte operation, ushort id, ushort generation, byte[] envelope)
    {
        var b = new List<byte> { operation };
        Varint(b, id); Varint(b, generation);
        if (operation == 1) Varint(b, 1);
        Varint(b, 1);
        b.Add(1); Varint(b, 1); Varint(b, 1); Varint(b, (ulong)envelope.Length); b.AddRange(envelope);
        return b.ToArray();
    }

    public static byte[] Remove(ushort id, ushort generation)
    {
        var b = new List<byte> { 3 };
        Varint(b, id); Varint(b, generation);
        return b.ToArray();
    }

    public static byte[] Delta(ulong baseVersion, ulong version, params byte[] payload)
    {
        var b = new List<byte> { 0 };
        Varint(b, baseVersion); Varint(b, version - baseVersion);
        b.AddRange(payload);
        return b.ToArray();
    }

    public static byte[] Full(long subject, uint kind, ulong version, string ns, string profile, ushort encoding, params byte[] payload)
    {
        var b = new List<byte> { 1 };
        Varint(b, (ulong)subject); Varint(b, kind); Varint(b, version); Varint(b, version); Varint(b, encoding);
        b.Add(0); Varint(b, 0);
        var n = Encoding.UTF8.GetBytes(ns); Varint(b, (ulong)n.Length); b.AddRange(n);
        var p = Encoding.UTF8.GetBytes(profile == "default" ? "" : profile); Varint(b, (ulong)p.Length); b.AddRange(p);
        b.AddRange(payload);
        return b.ToArray();
    }
}
