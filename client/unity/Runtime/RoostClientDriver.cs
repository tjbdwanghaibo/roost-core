#nullable disable
using System;
using Roost.Client;
using UnityEngine;

namespace Roost.Unity
{
    // 标准业务客户端的 Unity 生命周期桥接。业务在主线程创建并安装所有回调后 Attach，
    // 连接用 Client.Connection.ConnectAsync；自动重连由 ConnectionOptions 显式控制。
    // 每帧先 BrowserWebSocket.PollAll（即 RoostPlatform.Poll；WebGL 上轮询浏览器桥接：收字节、完成连接；其他平台没有浏览器连接，什么也不做），再 Pump。
    // WebGL 上选项经 RoostPlatform.Configure 换成浏览器 WebSocket 与 PumpDriven（client/README“C# 经 WebSocket 连玩家接入”）。
    public sealed class RoostClientDriver : MonoBehaviour
    {
        [SerializeField] private int callbackBudget = 64;
        public GameClient Client { get; private set; }
        public void Attach(GameClient client)
        {
            if (client == null) throw new ArgumentNullException(nameof(client));
            if (ReferenceEquals(Client, client)) return;
            Detach(); Client = client;
        }
        public void Detach() { Client?.Dispose(); Client = null; }
        private void Update()
        {
            if (Client == null) return;
            try { BrowserWebSocket.PollAll(); }
            catch (Exception error) { Debug.LogException(error); }
            try { Client?.Pump(Math.Max(1, callbackBudget)); }
            catch (Exception error) { Debug.LogException(error); }
        }
        private void OnDestroy() { Detach(); }
    }
}
