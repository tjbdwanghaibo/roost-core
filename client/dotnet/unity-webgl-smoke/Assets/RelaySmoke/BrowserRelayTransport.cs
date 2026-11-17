#nullable enable
using System;
using System.Collections.Generic;
using System.IO;
using System.Runtime.InteropServices;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client.Relay;

namespace Roost.RelaySmoke
{
    /// <summary>
    /// Unity WebGL 上的 relay 连接：用浏览器的 WebSocket（Assets/Plugins/WebGL/RoostRelayWebSocket.jslib）实现
    /// Roost.Client.Relay 的 IRelayTransport。SDK 不带它（维护者 2026-11-17 定：桥接代码不进 SDK），这是给游戏照抄的参考写法。
    ///
    /// 用法：RelayClientOptions { Transport = RelayTransport.Ws, TransportFactory = () => new BrowserRelayTransport() }，
    /// 游戏每帧在主线程先调 BrowserRelayTransport.PollAll()、再调 RelayClient.Pump()。
    ///
    /// 做法是轮询，不从 JS 回调 C#：JS 一侧把收到的二进制消息排成字节队列，PollAll 每帧把字节交给等着的 ReceiveAsync、
    /// 并检查连接状态。续行在 PollAll 里（主线程）同步执行，不经线程池。WebGL 只有一个线程，所有调用都在主线程上。
    /// 只在 WebGL 播放器里能用（DllImport "__Internal" 只有 WebGL 构建链接得到）；编辑器与其他平台用 SDK 自带的 WebSocketRelayTransport。
    /// </summary>
    public sealed class BrowserRelayTransport : IRelayTransport
    {
        // JS 侧的连接状态，与 jslib 的 RoostWs 状态码一致。
        private const int Connecting = 0, Open = 1, Closed = 2, Failed = 3;

        [DllImport("__Internal")] private static extern int RoostWsOpen(string url);
        [DllImport("__Internal")] private static extern int RoostWsState(int id);
        [DllImport("__Internal")] private static extern int RoostWsSend(int id, byte[] data, int length);
        [DllImport("__Internal")] private static extern int RoostWsReceive(int id, byte[] buffer, int length);
        [DllImport("__Internal")] private static extern int RoostWsBuffered(int id);
        [DllImport("__Internal")] private static extern void RoostWsClose(int id);

        private static readonly List<BrowserRelayTransport> Live = new List<BrowserRelayTransport>();

        private readonly byte[] scratch = new byte[64 << 10];
        private int id;
        private bool disposed;
        private TaskCompletionSource<bool>? connecting;
        private TaskCompletionSource<int>? receiving;
        private byte[]? receiveBuffer;
        private int receiveOffset, receiveCount;

        /// <summary>每帧在主线程调一次（在 RelayClient.Pump 之前）：完成等着的连接与收字节。</summary>
        public static void PollAll()
        {
            BrowserRelayTransport[] live;
            lock (Live) live = Live.ToArray();
            foreach (var transport in live) transport.Poll();
        }

        public Task ConnectAsync(string address, CancellationToken cancellation)
        {
            if (!address.StartsWith("ws://", StringComparison.OrdinalIgnoreCase) && !address.StartsWith("wss://", StringComparison.OrdinalIgnoreCase))
                throw new ArgumentException($"'{address}' is not a ws:// or wss:// URL.");
            connecting = new TaskCompletionSource<bool>();
            id = RoostWsOpen(address);
            lock (Live) Live.Add(this);
            cancellation.Register(Dispose); // SDK 超时也会 Dispose；这里再保证一次
            return connecting.Task;
        }

        public void Send(byte[] frame)
        {
            if (disposed) throw new ObjectDisposedException(nameof(BrowserRelayTransport));
            // 浏览器的 send 不阻塞，网络跟不上时字节积在 bufferedAmount 里；超过与 SDK 相同的上限就当写失败（SDK 随之断开）。
            if (RoostWsBuffered(id) + frame.Length > WebSocketRelayTransport.MaxPendingBytes) throw new IOException("relay: websocket send buffer is full.");
            if (RoostWsSend(id, frame, frame.Length) == 0) throw new IOException("relay: websocket is not open.");
        }

        public Task<int> ReceiveAsync(byte[] buffer, int offset, int count, CancellationToken cancellation)
        {
            if (disposed) return Task.FromResult(0);
            int got = TryReceive(buffer, offset, count);
            if (got != 0) return Task.FromResult(got < 0 ? 0 : got);
            receiving = new TaskCompletionSource<int>();
            receiveBuffer = buffer; receiveOffset = offset; receiveCount = count;
            return receiving.Task;
        }

        public void Dispose()
        {
            if (disposed) return;
            disposed = true;
            lock (Live) Live.Remove(this);
            if (id != 0) RoostWsClose(id);
            Take(ref connecting)?.TrySetException(new IOException("relay: websocket closed."));
            Take(ref receiving)?.TrySetResult(0);
        }

        private void Poll()
        {
            if (disposed) return;
            if (connecting != null)
            {
                int state = RoostWsState(id);
                if (state == Open) Take(ref connecting)?.TrySetResult(true);
                else if (state != Connecting) Take(ref connecting)?.TrySetException(new IOException("relay: websocket failed to open."));
            }
            if (receiving != null)
            {
                int got = TryReceive(receiveBuffer!, receiveOffset, receiveCount);
                if (got != 0) Take(ref receiving)?.TrySetResult(got < 0 ? 0 : got); // 续行（SDK 的读循环）就在这里、主线程上跑
            }
        }

        /// <summary>从 JS 的字节队列里取：返回取到的字节数；0 表示还没有；-1 表示连接已结束（队列里的字节先交完）。</summary>
        private int TryReceive(byte[] buffer, int offset, int count)
        {
            int got = RoostWsReceive(id, scratch, Math.Min(count, scratch.Length));
            if (got > 0)
            {
                Buffer.BlockCopy(scratch, 0, buffer, offset, got);
                return got;
            }
            int state = RoostWsState(id);
            return state == Closed || state == Failed ? -1 : 0;
        }

        private static T? Take<T>(ref T? field) where T : class
        {
            var value = field;
            field = null;
            return value;
        }
    }
}
