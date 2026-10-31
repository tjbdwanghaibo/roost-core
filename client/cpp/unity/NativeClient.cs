using System;
using System.Runtime.InteropServices;
namespace Roost.Native
{
// Pure C# binding: Unity Update calls Poll; no native thread enters managed code.
public sealed class NativeClient : IDisposable
{
    public enum EventType : uint
    {
        Ready = 1,
        Response = 2,
        Push = 3,
        RequestFailed = 4,
        Closed = 5,
        Sent = 6
    }
#if UNITY_IOS && !UNITY_EDITOR
    private const string Library = "__Internal";
#else
    private const string Library = "roost_client";
#endif
    public enum RequestStatus : uint
    {
        Ok,
        TimeoutUnsent,
        TimeoutUnknown,
        CanceledUnsent,
        CanceledUnknown,
        InvalidPayload
    }
    [StructLayout(LayoutKind.Sequential)]
    public struct Options
    {
        public uint Payload, Events, Requests, ConnectTimeout, RequestTimeout, Heartbeat, HeartbeatTimeout;
    }
    public static Options DefaultOptions
    {
        get {
            if (roost_abi_version() != 2)
                throw new InvalidOperationException("Native ABI mismatch; rebuild plugin and bindings");
            return roost_default_options();
        }
    }
    [StructLayout(LayoutKind.Sequential)]
    private struct NativeEvent
    {
        public uint Type, MessageId, Sequence, Kind;
        public ulong Generation;
        public IntPtr Data;
        public UIntPtr Size;
        public uint Status;
    }
    public sealed class Event
    {
        public EventType Type;
        public uint MessageId, Sequence, Kind;
        public ulong Generation;
        public RequestStatus Status;
        public byte[] Payload = Array.Empty<byte>();
    }
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern Options roost_default_options();
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern IntPtr roost_create(Options options);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl, CharSet = CharSet.Ansi)]
    private static extern int roost_connect(IntPtr client, string endpoint, ushort port, byte[] ticket, UIntPtr size);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern uint roost_request_with_timeout(IntPtr client, uint id, uint reply, byte[] data, UIntPtr size,
                                                          uint timeout);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern int roost_poll(IntPtr client, out NativeEvent value);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern void roost_disconnect(IntPtr client);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern void roost_destroy(IntPtr client);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern uint roost_notify(IntPtr client, uint id, uint kind, byte[] data, UIntPtr size);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern int roost_cancel(IntPtr client, uint sequence);
    [DllImport(Library, CallingConvention = CallingConvention.Cdecl)]
    private static extern uint roost_abi_version();
    private IntPtr handle;
    private readonly int thread = Environment.CurrentManagedThreadId;
    public NativeClient() : this(DefaultOptions)
    {
    }
    public NativeClient(Options options)
    {
        if (roost_abi_version() != 2)
            throw new InvalidOperationException("Native ABI mismatch; rebuild plugin and bindings");
        handle = roost_create(options);
        if (handle == IntPtr.Zero)
            throw new InvalidOperationException("Native client allocation failed");
    }
    private void Check()
    {
        if (thread != Environment.CurrentManagedThreadId)
            throw new InvalidOperationException("Use the engine thread");
        if (handle == IntPtr.Zero)
            throw new ObjectDisposedException(nameof(NativeClient));
    }
    public void Connect(string numericAddress, ushort port, string ticket)
    {
        Check();
        var bytes = System.Text.Encoding.UTF8.GetBytes(ticket);
        if (roost_connect(handle, numericAddress, port, bytes, (UIntPtr)bytes.Length) == 0)
            throw new InvalidOperationException("Connection rejected");
    }
    public uint Request(uint id, uint reply, byte[] payload, uint timeoutMilliseconds = 0)
    {
        Check();
        if (payload == null)
            throw new ArgumentNullException(nameof(payload));
        var sequence =
            roost_request_with_timeout(handle, id, reply, payload, (UIntPtr)payload.Length, timeoutMilliseconds);
        if (sequence == 0)
            throw new InvalidOperationException("Request not admitted");
        return sequence;
    }
    public uint Notify(uint id, uint kind, byte[] payload)
    {
        Check();
        if (payload == null)
            throw new ArgumentNullException(nameof(payload));
        var seq = roost_notify(handle, id, kind, payload, (UIntPtr)payload.Length);
        if (seq == 0)
            throw new InvalidOperationException("Notification not admitted");
        return seq;
    }
    public bool Cancel(uint sequence)
    {
        Check();
        return roost_cancel(handle, sequence) != 0;
    }
    public bool Poll(out Event value)
    {
        Check();
        value = null;
        if (roost_poll(handle, out var raw) == 0)
            return false;
        var data = new byte[checked((int)raw.Size.ToUInt64())];
        if (data.Length != 0)
            Marshal.Copy(raw.Data, data, 0, data.Length);
        value = new Event {
            Type = (EventType)raw.Type,  MessageId = raw.MessageId,          Sequence = raw.Sequence, Kind = raw.Kind,
            Generation = raw.Generation, Status = (RequestStatus)raw.Status, Payload = data
        };
        return true;
    }
    public void Disconnect()
    {
        Check();
        roost_disconnect(handle);
    }
    public void Dispose()
    {
        if (handle == IntPtr.Zero)
            return;
        Check();
        roost_destroy(handle);
        handle = IntPtr.Zero;
    }
}
}
