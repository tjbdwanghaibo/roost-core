#nullable enable
using System;
using System.Threading.Tasks;
using Roost.Client;

namespace Roost.GateSmoke
{
    /// <summary>GateSmoke 的设置。</summary>
    public sealed class GateSmokeConfig
    {
        /// <summary>玩家接入的 WebSocket 地址（serve-gate.mjs 起的对端，或真实的 player_access.{tcp,gate}.websocket）。</summary>
        public string Url = "ws://127.0.0.1:7080/ws";
        /// <summary>握手之后保持连接的秒数：要超过服务端的 idle_timeout（缺省 90s）至少 10s，证明心跳在跑。</summary>
        public float HoldSeconds = 100;
        /// <summary>WebGL：用浏览器 WebSocket（BrowserGateTransport）；否则用 SDK 自带的 WebSocketGateTransport。</summary>
        public bool BrowserWebSocket;
        /// <summary>ConnectionOptions.PumpDriven（WebGL 必开）。</summary>
        public bool PumpDriven = true;
        public string Ticket = "smoke";
    }

    /// <summary>
    /// 连玩家接入的最小冒烟（维护者 2026-11-18：Unity / WebGL 只验“经 WebSocket 连进玩家接入、握手成功”，再只靠 Pump 驱动的心跳
    /// 保持连接超过服务端 idle_timeout）。不做请求、推送、重连：WebSocket 之上的逻辑与 TCP 共用，由 .NET 用例覆盖。
    /// 日志行以 SMOKE 开头，最后一行 SMOKE PASS / SMOKE FAIL …；PASS 行带握手耗时、保持秒数与心跳往返次数。
    /// 驱动：游戏线程每帧先 BrowserGateTransport.PollAll（WebGL）、再 GameClient.Pump（播放器里由 RoostClientDriver 做），再调 Tick。
    /// </summary>
    public sealed class GateSmoke
    {
        private readonly GateSmokeConfig config;
        private readonly Action<string> log;
        private readonly Func<double> now;
        private double started = -1, connectedAt = -1, lastReport;
        private const double ConnectLimit = 30;

        public GameClient Client { get; }
        public bool Finished { get; private set; }
        public bool Passed { get; private set; }

        public GateSmoke(GateSmokeConfig config, Action<string> log, Func<double> now)
        {
            this.config = config; this.log = log; this.now = now;
            Client = new GameClient(new ConnectionOptions
            {
                WebSocketUrl = config.Url, PumpDriven = config.PumpDriven, AutoReconnect = false,
                TransportFactory = config.BrowserWebSocket ? () => new BrowserGateTransport() : (Func<IGateTransport>?)null,
            });
        }

        /// <summary>在主线程上调用：开始连接与握手（PumpDriven 时续行回到主线程）。</summary>
        public async Task RunAsync()
        {
            started = now();
            log($"SMOKE connecting {config.Url} (browser websocket {config.BrowserWebSocket}, pump-driven {config.PumpDriven}, hold {config.HoldSeconds}s)");
            try
            {
                await Client.Connection.ConnectAsync(config.Ticket);
                connectedAt = now();
                lastReport = connectedAt;
                log($"SMOKE handshake ok in {(connectedAt - started) * 1000:F0}ms (state {Client.Connection.State})");
            }
            catch (Exception error) { Fail("handshake failed: " + error); }
        }

        /// <summary>每帧在 Pump 之后调用：核对连接还在，到时判定。</summary>
        public void Tick()
        {
            if (Finished || started < 0) return;
            double t = now();
            if (connectedAt < 0)
            {
                if (t - started > ConnectLimit) Fail($"no handshake within {ConnectLimit}s");
                return;
            }
            var connection = Client.Connection;
            double held = t - connectedAt;
            if (connection.State != ConnectionState.Ready || connection.ConnectionVersion != 1)
            {
                Fail($"disconnected after {held:F1}s: state {connection.State}, error {connection.LastError}");
                return;
            }
            if (t - lastReport >= 10)
            {
                lastReport = t;
                log($"SMOKE holding {held:F0}s/{config.HoldSeconds}s, heartbeat round trips {connection.HeartbeatRoundTrips}");
            }
            if (held < config.HoldSeconds) return;
            // 缺省心跳间隔 15s：保持 hold 秒至少应有 hold/15 - 1 次往返。
            long expected = Math.Max(1, (long)(config.HoldSeconds / 15) - 1);
            if (connection.HeartbeatRoundTrips < expected)
            {
                Fail($"held {held:F0}s but only {connection.HeartbeatRoundTrips} heartbeat round trips (want ≥ {expected})");
                return;
            }
            Finished = Passed = true;
            log($"SMOKE PASS held {held:F0}s connected, heartbeat round trips {connection.HeartbeatRoundTrips}, handshake {(connectedAt - started) * 1000:F0}ms");
            Client.Connection.Disconnect();
        }

        private void Fail(string why)
        {
            if (Finished) return;
            Finished = true;
            log("SMOKE FAIL " + why);
        }
    }
}
