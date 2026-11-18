using System;

namespace Roost.Client
{
    public sealed class ConnectionOptions
    {
        // TCP 地址；设了 WebSocketUrl 时不用。
        public string Host { get; set; } = "localhost";
        public int Port { get; set; } = 9000;

        // ---------- 传输（维护者 2026-11-18：C# 经 WebSocket 连玩家接入，照开房间 SDK 的形状） ----------

        /// <summary>
        /// ws:// 或 wss:// 地址（例如 ws://game.example.com:7080/ws，路径与服务端 player_access.{tcp,gate}.websocket.path 一致，缺省 /ws）。
        /// 设了就经 WebSocket 连玩家接入（嵌入 TCP 或独立 Gate 的 websocket 监听），Host / Port 不用；每帧一条二进制消息，握手、票据、
        /// 连接代号、心跳与 TCP 相同。null（缺省）走 TCP。地址原样使用，SDK 不从 TCP 地址推算：通常取 portal 进服应答的地址
        /// （进服请求带 "transport":"ws" 时返回完整的 ws(s)://host:port/path）。
        /// </summary>
        public string? WebSocketUrl { get; set; }
        /// <summary>
        /// 每次连接（含自动重连）新建一个传输。null 时按 WebSocketUrl 用 SDK 自带的 WebSocketGateTransport，否则 TcpGateTransport。
        /// Unity WebGL 上没有套接字，用浏览器 WebSocket 实现的 IGateTransport（Unity 包的 BrowserWebSocket；RoostPlatform.Configure
        /// 在 WebGL 上替你填，同时要求设了 WebSocketUrl）。传输的 ConnectAsync 收到的地址是 WebSocketUrl，没设时是 host:port。
        /// </summary>
        public Func<IGateTransport>? TransportFactory { get; set; }
        /// <summary>
        /// 由 Pump 驱动（Unity WebGL 必开；缺省 false，其他平台行为不变）。打开时 SDK 不用线程池与计时器：所有等待回到调用者的
        /// 上下文（Unity 主线程），连接 / 请求 / 心跳超时、心跳间隔、重连退避都由 GameClient.Pump（直接用 Connection 时是
        /// Connection.Pump）按时钟检查。要求：在有 SynchronizationContext 的线程（Unity 主线程）上 ConnectAsync 并每帧 Pump；
        /// 传输的 ConnectAsync / ReceiveAsync 在同一线程上完成、SendAsync 同步完成（浏览器桥接每帧先轮询再 Pump）。
        /// 代价：不 Pump（加载场景卡住主线程、切后台）就不发心跳、不判超时、不重连，超过服务端 idle_timeout 被断开，恢复 Pump 后
        /// 按 AutoReconnect 重连。
        /// </summary>
        public bool PumpDriven { get; set; }
        public TimeSpan ConnectTimeout { get; set; } = TimeSpan.FromSeconds(10);
        public TimeSpan RequestTimeout { get; set; } = TimeSpan.FromSeconds(10);
        public TimeSpan HeartbeatInterval { get; set; } = TimeSpan.FromSeconds(15);
        // 首次连接失败交给调用方；建立连接后异常断线才自动重连。
        // 由业务显式启用；默认只报告断线，业务可重新调用 ConnectAsync。
        public bool AutoReconnect { get; set; } = false;
        public int MaxReconnectAttempts { get; set; } = 8;
        public TimeSpan ReconnectInitialDelay { get; set; } = TimeSpan.FromSeconds(1);
        public TimeSpan ReconnectMaxDelay { get; set; } = TimeSpan.FromSeconds(30);
        public TimeSpan HeartbeatTimeout { get; set; } = TimeSpan.FromSeconds(5);
        public int MaxPayloadBytes { get; set; } = 1024 * 1024;
        public int MaxPendingRequests { get; set; } = 64;
        public int MaxQueuedPushes { get; set; } = 256;

        internal ConnectionOptions Snapshot()
        {
            if (WebSocketUrl != null)
            {
                if (!WebSocketGateTransport.IsWebSocketUrl(WebSocketUrl))
                    throw new ArgumentException($"WebSocketUrl '{WebSocketUrl}' is not a ws:// or wss:// URL.");
            }
            else
            {
                if (string.IsNullOrWhiteSpace(Host)) throw new ArgumentException("Host is required.");
                if (Port < 1 || Port > 65535) throw new ArgumentOutOfRangeException(nameof(Port));
            }
            CheckTimeout(ConnectTimeout); CheckTimeout(RequestTimeout); CheckTimeout(HeartbeatTimeout);
            CheckTimeout(ReconnectInitialDelay); CheckTimeout(ReconnectMaxDelay);
            if (MaxReconnectAttempts < 1 || ReconnectMaxDelay < ReconnectInitialDelay)
                throw new ArgumentOutOfRangeException(nameof(MaxReconnectAttempts));
            if (HeartbeatInterval < TimeSpan.Zero || HeartbeatInterval.TotalMilliseconds > int.MaxValue)
                throw new ArgumentOutOfRangeException(nameof(HeartbeatInterval));
            if (MaxPayloadBytes <= 0 || MaxPendingRequests <= 0 || MaxQueuedPushes <= 0)
                throw new ArgumentOutOfRangeException(nameof(MaxPayloadBytes));
            return (ConnectionOptions)MemberwiseClone();
        }

        /// <summary>交给传输 ConnectAsync 的地址。</summary>
        internal string Address => WebSocketUrl ?? TcpGateTransport.Address(Host, Port);

        /// <summary>按选项新建一条连接：TransportFactory，否则按 WebSocketUrl 用自带的 WebSocket / TCP。</summary>
        internal IGateTransport NewTransport()
        {
            if (TransportFactory != null)
                return TransportFactory() ?? throw new InvalidOperationException("ConnectionOptions.TransportFactory returned null.");
            return WebSocketUrl != null ? (IGateTransport)new WebSocketGateTransport() : new TcpGateTransport();
        }

        private static void CheckTimeout(TimeSpan value)
        {
            if (value <= TimeSpan.Zero || value.TotalMilliseconds > int.MaxValue)
                throw new ArgumentOutOfRangeException(nameof(value));
        }
    }
}
