#nullable enable
using System;
using System.Collections.Generic;
using System.IO;
using System.Runtime.InteropServices;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client;

namespace Roost.GateSmoke
{
    /// <summary>
    /// Unity WebGL 上连玩家接入（嵌入 TCP 或独立 Gate 的 WebSocket 监听）的连接：用浏览器的 WebSocket 实现 Roost.Client 的
    /// IGateTransport。SDK 不带它（维护者 2026-11-17 定：桥接代码不进 SDK），这是给游戏照抄的参考写法，与开房间 SDK 的
    /// BrowserRelayTransport 同一个做法、同一个 jslib（Assets/Plugins/WebGL/RoostRelayWebSocket.jslib，函数是通用的字节队列）。
    ///
    /// 用法（client/README“Unity WebGL”）：
    ///   var client = new GameClient(new ConnectionOptions {
    ///       WebSocketUrl = "wss://game.example.com/ws", PumpDriven = true, AutoReconnect = true,
    ///       TransportFactory = () => new BrowserGateTransport() });
    ///   driver.Attach(client, BrowserGateTransport.PollAll);   // RoostClientDriver：每帧先 PollAll 再 Pump
    ///   await client.Connection.ConnectAsync(ticketProvider);  // 在主线程上
    ///
    /// 做法是轮询，不从 JS 回调 C#：JS 一侧把收到的二进制消息排成字节队列，PollAll 每帧把字节交给等着的 ReceiveAsync、并检查
    /// 连接状态；续行在 PollAll 里（主线程）同步执行，不经线程池。SDK 按包头拼帧，所以这里只管按顺序交字节，不看消息边界。
    /// SendAsync 直接调浏览器的 send（不阻塞），同步完成——ConnectionOptions.PumpDriven 要求如此。
    /// 只在 WebGL 播放器里能用（DllImport "__Internal" 只有 WebGL 构建链接得到）；编辑器与其他平台用 SDK 自带的 WebSocketGateTransport。
    /// 浏览器一侧的函数抽成 <see cref="IBrowserSocket"/>，.NET 测试（Roost.Client.Tests 的 BrowserBridgeTests）用假的喂消息。
    /// </summary>
    public sealed class BrowserGateTransport : IGateTransport
    {
        // JS 侧的连接状态，与 jslib 的 RoostWs 状态码一致。
        public const int Connecting = 0, Open = 1, Closed = 2, Failed = 3;
        /// <summary>浏览器发送缓冲积压的上限：超过就当写失败，SDK 随之断开（与开房间 SDK 相同的 16 MiB）。</summary>
        public const int MaxBufferedBytes = 16 << 20;

        /// <summary>浏览器一侧的六个函数（jslib）。</summary>
        public interface IBrowserSocket
        {
            int Open(string url);
            int State(int id);
            int Send(int id, byte[] data, int length);
            int Receive(int id, byte[] buffer, int length);
            int Buffered(int id);
            void Close(int id);
        }

        private sealed class JsLib : IBrowserSocket
        {
            internal static readonly JsLib Instance = new JsLib();
            [DllImport("__Internal")] private static extern int RoostWsOpen(string url);
            [DllImport("__Internal")] private static extern int RoostWsState(int id);
            [DllImport("__Internal")] private static extern int RoostWsSend(int id, byte[] data, int length);
            [DllImport("__Internal")] private static extern int RoostWsReceive(int id, byte[] buffer, int length);
            [DllImport("__Internal")] private static extern int RoostWsBuffered(int id);
            [DllImport("__Internal")] private static extern void RoostWsClose(int id);
            public int Open(string url) => RoostWsOpen(url);
            public int State(int id) => RoostWsState(id);
            public int Send(int id, byte[] data, int length) => RoostWsSend(id, data, length);
            public int Receive(int id, byte[] buffer, int length) => RoostWsReceive(id, buffer, length);
            public int Buffered(int id) => RoostWsBuffered(id);
            public void Close(int id) => RoostWsClose(id);
        }

        private static readonly List<BrowserGateTransport> Live = new List<BrowserGateTransport>();

        private readonly IBrowserSocket js;
        private readonly byte[] scratch = new byte[64 << 10];
        private int id;
        private bool disposed;
        private TaskCompletionSource<bool>? connecting;
        private TaskCompletionSource<int>? receiving;
        private byte[]? receiveBuffer;
        private int receiveOffset, receiveCount;

        public BrowserGateTransport() : this(JsLib.Instance) { }
        public BrowserGateTransport(IBrowserSocket js) { this.js = js ?? throw new ArgumentNullException(nameof(js)); }

        /// <summary>每帧在主线程调一次（在 GameClient.Pump 之前）：完成等着的连接与收字节。</summary>
        public static void PollAll()
        {
            BrowserGateTransport[] live;
            lock (Live) live = Live.ToArray();
            foreach (var transport in live) transport.Poll();
        }

        public Task ConnectAsync(string address, CancellationToken cancellation)
        {
            if (!address.StartsWith("ws://", StringComparison.OrdinalIgnoreCase) && !address.StartsWith("wss://", StringComparison.OrdinalIgnoreCase))
                throw new ArgumentException($"'{address}' is not a ws:// or wss:// URL (set ConnectionOptions.WebSocketUrl).");
            connecting = new TaskCompletionSource<bool>();
            id = js.Open(address);
            lock (Live) Live.Add(this);
            return connecting.Task; // 超时与取消由 SDK Dispose 这个连接
        }

        public Task SendAsync(byte[] frame, CancellationToken cancellation)
        {
            if (disposed) throw new ObjectDisposedException(nameof(BrowserGateTransport));
            // 浏览器的 send 不阻塞，网络跟不上时字节积在 bufferedAmount 里；超过上限就当写失败（SDK 随之断开）。
            if (js.Buffered(id) + frame.Length > MaxBufferedBytes) throw new IOException("gate: websocket send buffer is full.");
            if (js.Send(id, frame, frame.Length) == 0) throw new IOException("gate: websocket is not open.");
            return Task.CompletedTask; // 一帧一条二进制消息，已交给浏览器
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
            if (id != 0) js.Close(id);
            Take(ref connecting)?.TrySetException(new IOException("gate: websocket closed."));
            Take(ref receiving)?.TrySetResult(0);
        }

        private void Poll()
        {
            if (disposed) return;
            if (connecting != null)
            {
                int state = js.State(id);
                if (state == Open) Take(ref connecting)?.TrySetResult(true);
                else if (state != Connecting) Take(ref connecting)?.TrySetException(new IOException("gate: websocket failed to open."));
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
            // 至多取 count 个：多取的字节没有地方放，会丢（JS 一侧把没取走的留在队列里，下次再取）。
            int got = js.Receive(id, scratch, Math.Min(count, scratch.Length));
            if (got > 0)
            {
                Buffer.BlockCopy(scratch, 0, buffer, offset, got);
                return got;
            }
            int state = js.State(id);
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
