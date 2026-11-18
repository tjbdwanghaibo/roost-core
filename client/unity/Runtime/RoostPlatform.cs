#nullable enable
using System;
using System.Threading;
using System.Threading.Tasks;
using Roost.Client;
using Roost.Client.Relay;

namespace Roost.Unity
{
    /// <summary>
    /// 按平台选传输（维护者 2026-11-18 定方案 B）：游戏在所有平台上写同一份代码，WebGL 播放器里由这里换成浏览器 WebSocket
    /// 并打开 PumpDriven；编辑器与其他平台原样返回，行为不变。
    ///
    /// 连玩家接入：
    ///   var client = new GameClient(RoostPlatform.Configure(new ConnectionOptions { WebSocketUrl = wsUrl, AutoReconnect = true }));
    ///   driver.Attach(client);                                  // RoostClientDriver：每帧先轮询浏览器桥接，再 Pump
    ///   await client.Connection.ConnectAsync(ticketProvider);   // 在主线程上
    /// 开房间（relay）：
    ///   var options = RoostPlatform.Configure(new RelayClientOptions { Game = "party", Transport = RelayTransport.Ws });
    ///   var lobby = RoostPlatform.CreateLobby(lobbyUrl, gt2);   // WebGL 上经 UnityWebRequest、pumpDriven
    ///   var relay = new RelayClient(lobby, options);            // 建房 / 加入请求的 Transport 填 options.Transport
    ///   每帧在主线程：RoostPlatform.Poll(); relay.Pump();       // 同一帧里有 RoostClientDriver 时它已经 Poll 过，多调一次无妨
    /// </summary>
    public static class RoostPlatform
    {
        /// <summary>在 WebGL 播放器（浏览器）里：只能用浏览器 WebSocket，且必须 PumpDriven。编辑器里切到 WebGL 目标也是 false。</summary>
        public static bool InBrowser
        {
            get
            {
#if UNITY_WEBGL && !UNITY_EDITOR
                return true;
#else
                return false;
#endif
            }
        }

        /// <summary>
        /// 连玩家接入的选项：浏览器里设 PumpDriven = true、TransportFactory 为 <see cref="BrowserWebSocket"/>（覆盖原值），并要求已设
        /// WebSocketUrl（浏览器里没有 TCP）；其他平台原样返回。返回同一个对象。
        /// </summary>
        public static ConnectionOptions Configure(ConnectionOptions options)
        {
            if (options == null) throw new ArgumentNullException(nameof(options));
            if (!InBrowser) return options;
            if (string.IsNullOrEmpty(options.WebSocketUrl))
                throw new ArgumentException("Unity WebGL connects over WebSocket only: set ConnectionOptions.WebSocketUrl (portal enter with \"transport\":\"ws\").", nameof(options));
            options.PumpDriven = true;
            options.TransportFactory = () => new BrowserWebSocket();
            return options;
        }

        /// <summary>
        /// relay 的选项：浏览器里设 Transport = Ws、PumpDriven = true、TransportFactory 为 <see cref="BrowserWebSocket"/>（覆盖原值）；
        /// 其他平台原样返回。返回同一个对象。建房 / 加入请求的 Transport 要与 options.Transport 相同。
        /// </summary>
        public static RelayClientOptions Configure(RelayClientOptions options)
        {
            if (options == null) throw new ArgumentNullException(nameof(options));
            if (!InBrowser) return options;
            options.Transport = RelayTransport.Ws;
            options.PumpDriven = true;
            options.TransportFactory = () => new BrowserWebSocket();
            return options;
        }

        /// <summary>
        /// lobby 客户端：浏览器里经 <see cref="UnityWebRequestLobbyHttp"/> 发、pumpDriven（与 Configure 后的 RelayClientOptions 配套；在主线程上调）；
        /// 其他平台是 SDK 缺省的 HttpClient。gameToken 每个请求取一次（gt2 有寿命，长时间运行时在这里换新的）。
        /// </summary>
        public static LobbyClient CreateLobby(string baseUrl, Func<CancellationToken, Task<string>> gameToken) =>
            InBrowser ? new LobbyClient(baseUrl, gameToken, new UnityWebRequestLobbyHttp(), pumpDriven: true) : new LobbyClient(baseUrl, gameToken);

        public static LobbyClient CreateLobby(string baseUrl, string gameToken) => CreateLobby(baseUrl, _ => Task.FromResult(gameToken));

        /// <summary>
        /// 轮询浏览器桥接：完成等着的连接、把收到的字节交给 SDK 的读循环（续行在这里、主线程上跑）。每帧在 Pump 之前调；
        /// RoostClientDriver 已经替 GameClient 调了，只用 RelayClient 的游戏自己调。其他平台上没有浏览器连接，什么也不做。
        /// </summary>
        public static void Poll() => BrowserWebSocket.PollAll();
    }
}
