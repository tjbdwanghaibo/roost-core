using System;
using Roost.Client;
using UnityEngine;

namespace Roost.Unity
{
    // 在主线程Attach/Detach；每帧消费有界数量。处理器按Kind选择PB或Sync解码。
    // 销毁时关闭自己的连接；切换连接时旧会话的push队列随Dispose清除。
    public sealed class RoostConnection : MonoBehaviour
    {
        [SerializeField] private int maxPacketsPerFrame = 64;
        private TcpSession session;
        public event Action<Packet> PacketReceived;
        public event Action<Exception> Disconnected;

        public void Attach(TcpSession next)
        {
            if (next == null) throw new ArgumentNullException(nameof(next));
            if (ReferenceEquals(session, next)) return;
            Detach(); session = next;
        }
        public void Detach() { session?.Dispose(); session = null; }
        private void Update()
        {
            var active = session;
            if (active == null) return;
            for (int i = 0; i < Math.Max(1, maxPacketsPerFrame) && ReferenceEquals(session, active); i++)
            {
                if (!active.TryDequeuePush(out var packet)) break;
                try { PacketReceived?.Invoke(packet); }
                catch (Exception error) { Debug.LogException(error); }
            }
            if (ReferenceEquals(session, active) && active.CloseReason != null)
            {
                session = null;
                try { Disconnected?.Invoke(active.CloseReason); }
                catch (Exception error) { Debug.LogException(error); }
            }
        }
        private void OnDestroy() { Detach(); }
    }
}
