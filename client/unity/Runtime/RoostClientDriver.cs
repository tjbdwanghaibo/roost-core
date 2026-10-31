#nullable disable
using System;
using Roost.Client;
using UnityEngine;

namespace Roost.Unity
{
    // 标准业务客户端的 Unity 生命周期桥接。业务在主线程创建并安装所有回调后 Attach，
    // 连接用 Client.Connection.ConnectAsync；自动重连由 ConnectionOptions 显式控制。
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
            try { Client?.Pump(Math.Max(1, callbackBudget)); }
            catch (Exception error) { Debug.LogException(error); }
        }
        private void OnDestroy() { Detach(); }
    }
}
