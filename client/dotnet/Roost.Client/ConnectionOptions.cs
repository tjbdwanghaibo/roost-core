using System;

namespace Roost.Client
{
    public sealed class ConnectionOptions
    {
        public string Host { get; set; } = "localhost";
        public int Port { get; set; } = 9000;
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
            if (string.IsNullOrWhiteSpace(Host)) throw new ArgumentException("Host is required.");
            if (Port < 1 || Port > 65535) throw new ArgumentOutOfRangeException(nameof(Port));
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

        private static void CheckTimeout(TimeSpan value)
        {
            if (value <= TimeSpan.Zero || value.TotalMilliseconds > int.MaxValue)
                throw new ArgumentOutOfRangeException(nameof(value));
        }
    }
}
