#nullable disable
using System;
using Roost.Client;
using UnityEngine;

namespace Roost.Unity
{
    // 标准业务客户端的 Unity 生命周期桥接。业务在主线程创建并安装所有回调后 Attach，
    // 连接用 Client.Connection.ConnectAsync；自动重连由 ConnectionOptions 显式控制。
    // Unity WebGL：ConnectionOptions 设 WebSocketUrl、PumpDriven 与浏览器 WebSocket 的 TransportFactory，
    // Attach(client, BrowserGateTransport.PollAll)：每帧先轮询浏览器桥接（收字节、完成连接），再 Pump（client/README“C# 经 WebSocket 连玩家接入”）。
    public sealed class RoostClientDriver : MonoBehaviour
    {
        [SerializeField] private int callbackBudget = 64;
        private Action beforePump;
        public GameClient Client { get; private set; }
        public void Attach(GameClient client) => Attach(client, null);
        // beforePump 在每帧 Pump 之前、同一个 Update 里调用（WebGL 上传给浏览器桥接的 PollAll）；null 表示不需要。
        public void Attach(GameClient client, Action beforePump)
        {
            if (client == null) throw new ArgumentNullException(nameof(client));
            if (ReferenceEquals(Client, client)) { this.beforePump = beforePump; return; }
            Detach(); Client = client; this.beforePump = beforePump;
        }
        public void Detach() { Client?.Dispose(); Client = null; beforePump = null; }
        private void Update()
        {
            if (Client == null) return;
            try { beforePump?.Invoke(); }
            catch (Exception error) { Debug.LogException(error); }
            try { Client?.Pump(Math.Max(1, callbackBudget)); }
            catch (Exception error) { Debug.LogException(error); }
        }
        private void OnDestroy() { Detach(); }
    }
}
