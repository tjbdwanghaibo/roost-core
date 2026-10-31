using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
namespace Roost.Native
{
// 每场比赛一个实例。所有 native 回调都由 Receive/Submit 的消费线程同步调用。
// Unity IL2CPP 使用静态反向 PInvoke；GCHandle 在解除 Push 订阅并销毁 native 后释放。
public sealed class NativeLockstep : IDisposable
{
#if UNITY_IOS && !UNITY_EDITOR
    private const string Library = "__Internal";
#else
    private const string Library = "roost_client";
#endif
    public sealed class Input
    {
        public int Player;
        public byte[] Payload;
    }
    public sealed class Snapshot
    {
        public uint Frame;
        public byte[] Data;
        public ulong ExpectedHash;
        public bool HasHash;
    }
    [StructLayout(LayoutKind.Sequential)]
    private struct RawInput
    {
        public int Player;
        public IntPtr Data;
        public UIntPtr Size;
    }
    [UnmanagedFunctionPointer(CallingConvention.Cdecl)]
    private delegate int SendCallback(IntPtr context, IntPtr data, UIntPtr size);
    [UnmanagedFunctionPointer(CallingConvention.Cdecl)]
    private delegate int SimulateCallback(IntPtr context, uint frame, IntPtr inputs, UIntPtr count);
    [UnmanagedFunctionPointer(CallingConvention.Cdecl)]
    private delegate int RestoreCallback(IntPtr context, uint frame, IntPtr data, UIntPtr size, ulong expected,
                                         int hasHash, out ulong actual);
    [StructLayout(LayoutKind.Sequential)]
    private struct Callbacks
    {
        public IntPtr Context;
        public SendCallback Send;
        public SimulateCallback Simulate;
        public RestoreCallback Restore;
    }
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern IntPtr roost_lockstep_create(Callbacks callbacks, UIntPtr buffered, UIntPtr retry,
                                                       UIntPtr snapshot);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern int roost_lockstep_receive(IntPtr state, byte[] data, UIntPtr size);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern int roost_lockstep_submit(IntPtr state, uint frame, byte[] data, UIntPtr size);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern int roost_lockstep_hash(IntPtr state, uint frame, ulong hash);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern int roost_lockstep_catchup(IntPtr state, uint frame);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern ulong roost_lockstep_next(IntPtr state);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern IntPtr roost_lockstep_error(IntPtr state);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern void roost_lockstep_destroy(IntPtr state);
    private static readonly SendCallback send = Send;
    private static readonly SimulateCallback simulate = Simulate;
    private static readonly RestoreCallback restore = Restore;
    private readonly NativeGameClient game;
    private readonly uint commandId, broadcastId;
    private readonly Action<uint, IReadOnlyList<Input>> apply;
    private readonly Func<Snapshot, ulong> applySnapshot;
    private readonly int thread = Environment.CurrentManagedThreadId;
    private GCHandle context;
    private IntPtr handle;
    private bool calling;
    public Exception Failure { get; private set; }
    public ulong Next
    {
        get {
            Check();
            return roost_lockstep_next(handle);
        }
    }
    public NativeLockstep(NativeGameClient game, uint commandId, uint broadcastId,
                          Action<uint, IReadOnlyList<Input>> apply, Func<Snapshot, ulong> applySnapshot = null,
                          uint maxBuffered = 256, uint retryPackets = 64, uint maxSnapshot = 4u << 20)
    {
        this.game = game ?? throw new ArgumentNullException(nameof(game));
        this.apply = apply ?? throw new ArgumentNullException(nameof(apply));
        if (commandId == 0 || broadcastId == 0)
            throw new ArgumentException("Lockstep route IDs required");
        this.commandId = commandId;
        this.broadcastId = broadcastId;
        this.applySnapshot = applySnapshot;
        context = GCHandle.Alloc(this);
        try
        {
            handle = roost_lockstep_create(new Callbacks { Context = GCHandle.ToIntPtr(context), Send = send,
                                                           Simulate = simulate,
                                                           Restore = applySnapshot == null ? null : restore },
                                           (UIntPtr)maxBuffered, (UIntPtr)retryPackets, (UIntPtr)maxSnapshot);
            if (handle == IntPtr.Zero)
                throw new ArgumentException("Invalid Lockstep options");
            game.Push += Receive;
            game.ConnectionReset += Reset;
        }
        catch
        {
            context.Free();
            throw;
        }
    }
    private void Check()
    {
        if (thread != Environment.CurrentManagedThreadId)
            throw new InvalidOperationException("Use the engine thread");
        if (handle == IntPtr.Zero)
            throw new ObjectDisposedException(nameof(NativeLockstep));
        if (Failure != null)
            throw new InvalidOperationException("Lockstep match failed", Failure);
    }
    private void Call(Func<int> action)
    {
        Check();
        if (calling)
            throw new InvalidOperationException("Recursive Lockstep call");
        calling = true;
        try
        {
            if (action() == 0)
                throw new InvalidOperationException(Marshal.PtrToStringAnsi(roost_lockstep_error(handle)), Failure);
        }
        catch (Exception e)
        {
            Failure = e;
            throw;
        }
        finally
        {
            calling = false;
        }
    }
    private void Receive(NativeClient.Event e)
    {
        if (e.Kind == 2 && e.MessageId == broadcastId)
            Call(() => roost_lockstep_receive(handle, e.Payload, (UIntPtr)e.Payload.Length));
    }
    private void Reset()
    {
        Failure = new InvalidOperationException("Connection closed; create a new match client after recovery");
    }
    public void Submit(uint frame, byte[] payload)
    {
        if (payload == null)
            throw new ArgumentNullException(nameof(payload));
        Call(() => roost_lockstep_submit(handle, frame, payload, (UIntPtr)payload.Length));
    }
    public void ReportHash(uint frame, ulong hash)
    {
        Call(() => roost_lockstep_hash(handle, frame, hash));
    }
    public void Catchup(uint frame)
    {
        Call(() => roost_lockstep_catchup(handle, frame));
    }
    private static NativeLockstep Target(IntPtr p) => (NativeLockstep)GCHandle.FromIntPtr(p).Target;
    private static byte[] Copy(IntPtr p, UIntPtr size)
    {
        var data = new byte[checked((int)size.ToUInt64())];
        if (data.Length != 0)
            Marshal.Copy(p, data, 0, data.Length);
        return data;
    }
#if ENABLE_IL2CPP
    [AOT.MonoPInvokeCallback(typeof(SendCallback))]
#endif
    private static int Send(IntPtr p, IntPtr data, UIntPtr size)
    {
        var self = Target(p);
        try
        {
            self.game.Notify(self.commandId, 2, Copy(data, size),
                             e =>
                             {
                                 if (e.Type != NativeClient.EventType.Sent)
                                     self.Failure =
                                         new InvalidOperationException("Lockstep write failed; result unknown");
                             });
            return 1;
        }
        catch (Exception e)
        {
            self.Failure = e;
            return 0;
        }
    }
#if ENABLE_IL2CPP
    [AOT.MonoPInvokeCallback(typeof(SimulateCallback))]
#endif
    private static int Simulate(IntPtr p, uint frame, IntPtr inputs, UIntPtr count)
    {
        var self = Target(p);
        try
        {
            var list = new List<Input>();
            int stride = Marshal.SizeOf<RawInput>();
            for (int i = 0; i < checked((int)count.ToUInt64()); i++)
            {
                var raw = Marshal.PtrToStructure<RawInput>(IntPtr.Add(inputs, i * stride));
                list.Add(new Input { Player = raw.Player, Payload = Copy(raw.Data, raw.Size) });
            }
            self.apply(frame, list);
            return 1;
        }
        catch (Exception e)
        {
            self.Failure = e;
            return 0;
        }
    }
#if ENABLE_IL2CPP
    [AOT.MonoPInvokeCallback(typeof(RestoreCallback))]
#endif
    private static int Restore(IntPtr p, uint frame, IntPtr data, UIntPtr size, ulong expected, int hasHash,
                               out ulong actual)
    {
        var self = Target(p);
        actual = 0;
        try
        {
            actual = self.applySnapshot(new Snapshot { Frame = frame, Data = Copy(data, size), ExpectedHash = expected,
                                                       HasHash = hasHash != 0 });
            return 1;
        }
        catch (Exception e)
        {
            self.Failure = e;
            return 0;
        }
    }
    public void Dispose()
    {
        if (handle == IntPtr.Zero)
            return;
        if (calling || thread != Environment.CurrentManagedThreadId)
            throw new InvalidOperationException("Dispose after callbacks on the engine thread");
        game.Push -= Receive;
        game.ConnectionReset -= Reset;
        roost_lockstep_destroy(handle);
        handle = IntPtr.Zero;
        context.Free();
    }
}
}
