#nullable disable
using System;
using Roost.Client;
using UnityEngine;

namespace Roost.Unity
{
    // 在主线程Attach/Detach；Attach前安装PacketReceived，每帧消费有界数量。
    // 处理器按Kind选择PB或Sync解码；Sync Full先清理旧流状态。
    // 销毁时关闭自己的连接；切换连接时旧会话的push队列随Dispose清除。
    public sealed class RoostConnection : MonoBehaviour
    {
        [SerializeField] private int maxPacketsPerFrame = 64;
        private TcpSession session;
        private readonly SyncReceiver syncReceiver = new SyncReceiver();
        public event Action<Packet> PacketReceived;
        public event Action<Exception> Disconnected;

        public void Attach(TcpSession next)
        {
            if (next == null) throw new ArgumentNullException(nameof(next));
            if (ReferenceEquals(session, next)) return;
            Detach(); session = next;
        }
        public void Detach() { session?.Dispose(); session = null; syncReceiver.Reset(); }
        private void Update()
        {
            var active = session;
            if (active == null) return;
            for (int i = 0; i < Math.Max(1, maxPacketsPerFrame) && ReferenceEquals(session, active); i++)
            {
                if (!active.TryDequeuePush(out var packet)) break;
                try
                {
                    if (packet.Kind == PayloadKind.Sync)
                    {
                        var receive = PacketReceived;
                        if (receive == null) throw new InvalidOperationException("Install the Sync consumer before attaching a session.");
                        syncReceiver.Receive(packet.Payload.Span, _ => receive(packet));
                    }
                    else
                        PacketReceived?.Invoke(packet);
                }
                catch (Exception error)
                {
                    // 增量缺口或业务应用失败后不能继续消费旧流。上层经正常鉴权重连，
                    // 新会话从Full开始；没有另开绕过鉴权的“请求任意全量”协议。
                    if (packet.Kind == PayloadKind.Sync)
                    {
                        if (ReferenceEquals(session, active)) Disconnect(active, error);
                        return;
                    }
                    Debug.LogException(error);
                }
            }
            if (ReferenceEquals(session, active) && active.CloseReason != null)
            {
                Disconnect(active, active.CloseReason);
            }
        }
        private void Disconnect(TcpSession active, Exception reason)
        {
            session = null;
            active.Dispose();
            syncReceiver.Reset();
            try { Disconnected?.Invoke(reason); }
            catch (Exception error) { Debug.LogException(error); }
        }
        private void OnDestroy() { Detach(); }
    }
}
