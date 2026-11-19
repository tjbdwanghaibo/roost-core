using System;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.Net.Sockets;
using System.Net.WebSockets;
using System.Threading;
using System.Threading.Tasks;

namespace Roost.Client.Relay
{
    /// <summary>
    /// RelayClient 与 relay 之间的一条连接（交接 C1，维护者 2026-11-17 定的选项 A）。SDK 自带 TCP（<see cref="TcpRelayTransport"/>）
    /// 与 WebSocket（<see cref="WebSocketRelayTransport"/>，用 ClientWebSocket，给编辑器与非 WebGL 平台）；Unity WebGL 上没有套接字，
    /// 浏览器 WebSocket（.jslib）的实现在 Unity 包里（client/unity/Runtime 的 BrowserWebSocket，维护者 2026-11-18 定方案 B），
    /// 由 RoostPlatform.Configure 填进 <see cref="RelayClientOptions.TransportFactory"/>；本程序集不依赖 Unity，不带它。
    ///
    /// 约定（SDK 按这些调用，实现要做到）：
    /// - 一个实例只连一次：<see cref="ConnectAsync"/> 一次，之后 <see cref="Send"/> / <see cref="ReceiveAsync"/>，最后 Dispose；
    ///   重连时 SDK 向工厂要一个新实例。
    /// - Send 发一整帧（RS v2 帧的全部字节）。WebSocket 上一帧一条二进制消息（relay 发来的也是一条一帧，收的一侧它按字节流读）。
    ///   SDK 保证 Send 不并发（串行调用），但可能在任意线程上调用；不要在里面长时间阻塞（WebGL 上它在主线程）。
    ///   写不出去时抛异常，SDK 随之断开这条连接。
    /// - ReceiveAsync 把收到的字节按到达顺序交出，不要求和帧对齐：SDK 自己按包头拼帧，一条消息里有几帧、一帧拆成几条消息都行。
    ///   返回 0 表示对端正常关闭；同一时刻只有一个 ReceiveAsync 在等。
    /// - Dispose 立即关闭连接（不发任何帧），可以从任意线程、重复调用；正在等的 ReceiveAsync 要随之结束（返回 0 或抛异常）。
    ///   SDK 的连接超时、读超时与断开都靠它，不依赖 CancellationToken 一定生效。
    ///   Dispose 可能先于 ConnectAsync：SDK 用 token.Register(transport.Dispose) 做超时 / 取消，令牌在调用前已取消时当场 Dispose。
    ///   之后的 ConnectAsync 不能再打开连接（没人会再关它），要以异常或取消结束（自带的 TCP / ClientWebSocket 在 Dispose 之后连接就抛异常；Unity 包的 BrowserWebSocket 见其说明）。
    /// </summary>
    public interface IRelayTransport : IDisposable
    {
        /// <summary>连到 relay。address 是 lobby 给的 relay_addr：TCP 是 host:port，WebSocket 是 ws:// / wss:// URL。</summary>
        Task ConnectAsync(string address, CancellationToken cancellation);
        /// <summary>发一整帧。</summary>
        void Send(byte[] frame);
        /// <summary>收字节，返回读到几个；0 表示连接已关闭。</summary>
        Task<int> ReceiveAsync(byte[] buffer, int offset, int count, CancellationToken cancellation);
    }

    /// <summary>TCP 连接（缺省）。address 是 host:port。</summary>
    public sealed class TcpRelayTransport : IRelayTransport
    {
        private readonly TcpClient tcp = new TcpClient { NoDelay = true };
        private readonly TimeSpan writeTimeout;
        private NetworkStream? stream;

        /// <param name="writeTimeout">一次写 socket 的期限，写不出去就失败（RelayClientOptions.WriteTimeout）。</param>
        public TcpRelayTransport(TimeSpan writeTimeout) { this.writeTimeout = writeTimeout; }

        public async Task ConnectAsync(string address, CancellationToken cancellation)
        {
            var (host, port) = ParseAddress(address);
            await tcp.ConnectAsync(host, port).ConfigureAwait(false); // 不收 token：超时由 SDK Dispose 这个连接
            tcp.SendTimeout = (int)writeTimeout.TotalMilliseconds;
            stream = tcp.GetStream();
        }

        public void Send(byte[] frame) => (stream ?? throw new InvalidOperationException("Not connected.")).Write(frame, 0, frame.Length);

        public Task<int> ReceiveAsync(byte[] buffer, int offset, int count, CancellationToken cancellation) =>
            (stream ?? throw new InvalidOperationException("Not connected.")).ReadAsync(buffer, offset, count, cancellation);

        public void Dispose() => tcp.Dispose();

        private static (string, int) ParseAddress(string address)
        {
            if (string.IsNullOrEmpty(address) || address.Contains("://"))
                throw new ArgumentException($"'{address}' is not a TCP address (set RelayClientOptions.Transport = RelayTransport.Ws for a ws:// relay address).");
            int colon = address.LastIndexOf(':');
            if (colon <= 0 || !int.TryParse(address.Substring(colon + 1), NumberStyles.None, CultureInfo.InvariantCulture, out int port))
                throw new ArgumentException($"'{address}' is not host:port.");
            return (address.Substring(0, colon).Trim('[', ']'), port);
        }
    }

    /// <summary>
    /// WebSocket 连接，用 System.Net.WebSockets.ClientWebSocket：给编辑器与非 WebGL 平台（WebGL 上没有 ClientWebSocket，见
    /// <see cref="IRelayTransport"/>）。address 是 ws:// / wss:// URL。
    ///
    /// ClientWebSocket 同一时刻只允许一个 SendAsync，而 Send 是同步的：帧先进发送队列，由一个发送任务按序一条一条发（每帧一条
    /// 二进制消息），所以 Send 不等网络。一条消息超过 WriteTimeout 没发完、或队列里积压的字节超过 MaxPendingBytes 时
    /// 关掉连接，之后的 Send 抛 IOException，读循环随之结束（与 TCP 写超时断开一样）。
    /// </summary>
    public sealed class WebSocketRelayTransport : IRelayTransport
    {
        /// <summary>发送队列积压的字节上限：网络跟不上时不无界占内存（TCP 上对应的是写阻塞到 WriteTimeout）。</summary>
        public const int MaxPendingBytes = 16 << 20;

        private readonly ClientWebSocket socket = new ClientWebSocket();
        private readonly TimeSpan writeTimeout;
        private readonly object sendLock = new object();
        private readonly Queue<byte[]> pending = new Queue<byte[]>();
        private long pendingBytes;
        private bool sending;
        private Exception? failed;

        /// <param name="writeTimeout">一条消息的发送期限（RelayClientOptions.WriteTimeout）。</param>
        public WebSocketRelayTransport(TimeSpan writeTimeout) { this.writeTimeout = writeTimeout; }

        public Task ConnectAsync(string address, CancellationToken cancellation)
        {
            if (string.IsNullOrEmpty(address) || !(address.StartsWith("ws://", StringComparison.OrdinalIgnoreCase) || address.StartsWith("wss://", StringComparison.OrdinalIgnoreCase)))
                throw new ArgumentException($"'{address}' is not a ws:// or wss:// URL (set RelayClientOptions.Transport = RelayTransport.Tcp for a host:port relay address).");
            return socket.ConnectAsync(new Uri(address), cancellation);
        }

        public void Send(byte[] frame)
        {
            lock (sendLock)
            {
                if (failed != null) throw new IOException("relay: websocket send failed.", failed);
                if (pendingBytes + frame.Length > MaxPendingBytes)
                {
                    failed = new IOException($"relay: websocket send queue exceeds {MaxPendingBytes} bytes.");
                    Abort();
                    throw failed;
                }
                pending.Enqueue(frame);
                pendingBytes += frame.Length;
                if (sending) return;
                sending = true;
            }
            _ = SendLoopAsync(); // 在锁外起：SendAsync 可能同步完成，续行里还要拿 sendLock
        }

        /// <summary>按序发完队列；发失败时记下错误、关掉连接（读循环随之结束，SDK 报断线）。</summary>
        private async Task SendLoopAsync()
        {
            while (true)
            {
                byte[] frame;
                lock (sendLock)
                {
                    if (pending.Count == 0 || failed != null) { sending = false; return; }
                    frame = pending.Dequeue();
                    pendingBytes -= frame.Length;
                }
                try
                {
                    using var timeout = new CancellationTokenSource(writeTimeout);
                    await socket.SendAsync(new ArraySegment<byte>(frame), WebSocketMessageType.Binary, true, timeout.Token).ConfigureAwait(false);
                }
                catch (Exception error)
                {
                    lock (sendLock) { failed = error; sending = false; pending.Clear(); pendingBytes = 0; }
                    Abort();
                    return;
                }
            }
        }

        public async Task<int> ReceiveAsync(byte[] buffer, int offset, int count, CancellationToken cancellation)
        {
            while (true)
            {
                var result = await socket.ReceiveAsync(new ArraySegment<byte>(buffer, offset, count), cancellation).ConfigureAwait(false);
                if (result.MessageType == WebSocketMessageType.Close) return 0;
                if (result.MessageType != WebSocketMessageType.Binary) throw new InvalidDataException("Relay sent a non-binary websocket message.");
                if (result.Count > 0) return result.Count; // 空消息不是关闭：接着收
            }
        }

        public void Dispose() => Abort();

        private void Abort()
        {
            try { socket.Abort(); } catch (Exception) { }
            socket.Dispose();
        }
    }
}
