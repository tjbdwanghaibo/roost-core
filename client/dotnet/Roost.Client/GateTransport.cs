using System;
using System.Globalization;
using System.IO;
using System.Net.Sockets;
using System.Net.WebSockets;
using System.Threading;
using System.Threading.Tasks;

namespace Roost.Client
{
    /// <summary>
    /// TcpSession 与玩家接入（嵌入 TCP 或独立 Gate）之间的一条连接（维护者 2026-11-18：C# 客户端经 WebSocket 连玩家接入，
    /// 形状照开房间 SDK 的 IRelayTransport）。SDK 自带 TCP（<see cref="TcpGateTransport"/>，缺省）与 WebSocket
    /// （<see cref="WebSocketGateTransport"/>，用 ClientWebSocket，给编辑器、桌面与纯 C#）；Unity WebGL 上没有套接字，由游戏用浏览器的
    /// WebSocket（.jslib）实现这个接口，经 <see cref="ConnectionOptions.TransportFactory"/> 交给 SDK，参考写法见
    /// client/dotnet/unity-webgl-smoke（SDK 不带桥接代码，维护者 2026-11-17 定）。
    ///
    /// 约定（SDK 按这些调用，实现要做到）：
    /// - 一个实例只连一次：<see cref="ConnectAsync"/> 一次，之后 <see cref="SendAsync"/> / <see cref="ReceiveAsync"/>，最后 Dispose；
    ///   重连时 SDK 向工厂要一个新实例。
    /// - SendAsync 发一整帧（RS v2 帧的全部字节）。WebSocket 上一帧一条二进制消息（GATEWAY §6）。SDK 保证 SendAsync 不并发
    ///   （上一次完成之后才调下一次）；cancellation 取消时 SDK 随即 Dispose 这条连接（写到一半的帧不能续上）。
    ///   ConnectionOptions.PumpDriven（WebGL）下应同步完成（浏览器的 send 不阻塞）：Unity 的 Mono 运行库里，发送锁的异步等待
    ///   要线程池才能完成，而 WebGL 上线程池不运行。
    /// - ReceiveAsync 把收到的字节按到达顺序交出，不要求和帧对齐：SDK 自己按包头拼帧，一条消息里有几帧、一帧拆成几条消息都行。
    ///   返回 0 表示对端正常关闭；同一时刻只有一个 ReceiveAsync 在等。
    /// - Dispose 立即关闭连接（不发任何帧），可以从任意线程、重复调用；正在等的 ReceiveAsync 要随之结束（返回 0 或抛异常）。
    ///   SDK 的连接超时与断开都靠它，不依赖 CancellationToken 一定生效。
    /// - 握手、票据与连接代号（鉴权首包的信封）由 SDK 写成第一帧，传输不看帧的内容。
    /// </summary>
    public interface IGateTransport : IDisposable
    {
        /// <summary>连到玩家接入。address：TCP 是 host:port，WebSocket 是 ws:// / wss:// URL（ConnectionOptions.WebSocketUrl）。</summary>
        Task ConnectAsync(string address, CancellationToken cancellation);
        /// <summary>发一整帧。</summary>
        Task SendAsync(byte[] frame, CancellationToken cancellation);
        /// <summary>收字节，返回读到几个；0 表示连接已关闭。</summary>
        Task<int> ReceiveAsync(byte[] buffer, int offset, int count, CancellationToken cancellation);
    }

    /// <summary>TCP 连接（缺省）。address 是 host:port（IPv6 写成 [addr]:port）。</summary>
    public sealed class TcpGateTransport : IGateTransport
    {
        private readonly TcpClient tcp = new TcpClient();
        private NetworkStream? stream;

        public async Task ConnectAsync(string address, CancellationToken cancellation)
        {
            var (host, port) = ParseAddress(address);
            // 不收 token：Mono 的 TcpClient.ConnectAsync 不认它，超时与取消由 SDK Dispose 这个连接。
            await tcp.ConnectAsync(host, port).ConfigureAwait(false);
            tcp.NoDelay = true;
            stream = tcp.GetStream();
        }

        public Task SendAsync(byte[] frame, CancellationToken cancellation) =>
            Stream.WriteAsync(frame, 0, frame.Length, cancellation);

        public Task<int> ReceiveAsync(byte[] buffer, int offset, int count, CancellationToken cancellation) =>
            Stream.ReadAsync(buffer, offset, count, cancellation);

        public void Dispose() => tcp.Dispose();

        private NetworkStream Stream => stream ?? throw new InvalidOperationException("Not connected.");

        /// <summary>host 与 port 拼成 ConnectAsync 认的地址：IPv6 字面量加方括号。</summary>
        internal static string Address(string host, int port) =>
            (host.IndexOf(':') >= 0 && !host.StartsWith("[", StringComparison.Ordinal) ? "[" + host + "]" : host) + ":" + port.ToString(CultureInfo.InvariantCulture);

        private static (string, int) ParseAddress(string address)
        {
            if (string.IsNullOrEmpty(address) || address.Contains("://"))
                throw new ArgumentException($"'{address}' is not a TCP address (set ConnectionOptions.WebSocketUrl for a ws:// address).");
            int colon = address.LastIndexOf(':');
            if (colon <= 0 || !int.TryParse(address.Substring(colon + 1), NumberStyles.None, CultureInfo.InvariantCulture, out int port))
                throw new ArgumentException($"'{address}' is not host:port.");
            return (address.Substring(0, colon).Trim('[', ']'), port);
        }
    }

    /// <summary>
    /// WebSocket 连接，用 System.Net.WebSockets.ClientWebSocket：给编辑器、桌面与纯 C#（WebGL 上没有 ClientWebSocket，见
    /// <see cref="IGateTransport"/>）。address 是 ws:// / wss:// URL，路径要与服务端 websocket.path 一致（缺省 /ws）。
    /// 每帧一条二进制消息；收到的二进制消息按字节流交出；文本消息当协议错误（服务端以 1003 关闭的也是它）。
    /// 不发 Origin 头：服务端对不带 Origin 的请求总是放行，身份仍由首帧票据认证（GATEWAY §6）。
    /// </summary>
    public sealed class WebSocketGateTransport : IGateTransport
    {
        private readonly ClientWebSocket socket = new ClientWebSocket();

        public Task ConnectAsync(string address, CancellationToken cancellation)
        {
            if (!IsWebSocketUrl(address))
                throw new ArgumentException($"'{address}' is not a ws:// or wss:// URL.");
            return socket.ConnectAsync(new Uri(address), cancellation);
        }

        public Task SendAsync(byte[] frame, CancellationToken cancellation) =>
            socket.SendAsync(new ArraySegment<byte>(frame), WebSocketMessageType.Binary, true, cancellation);

        public async Task<int> ReceiveAsync(byte[] buffer, int offset, int count, CancellationToken cancellation)
        {
            while (true)
            {
                var result = await socket.ReceiveAsync(new ArraySegment<byte>(buffer, offset, count), cancellation).ConfigureAwait(false);
                if (result.MessageType == WebSocketMessageType.Close) return 0;
                if (result.MessageType != WebSocketMessageType.Binary) throw new InvalidDataException("Player access sent a non-binary websocket message.");
                if (result.Count > 0) return result.Count; // 空消息不是关闭：接着收
            }
        }

        public void Dispose()
        {
            try { socket.Abort(); } catch (Exception) { /* 已经关了 */ }
            socket.Dispose();
        }

        internal static bool IsWebSocketUrl(string? address) =>
            address != null && Uri.TryCreate(address, UriKind.Absolute, out var uri) && (uri.Scheme == "ws" || uri.Scheme == "wss");
    }
}
