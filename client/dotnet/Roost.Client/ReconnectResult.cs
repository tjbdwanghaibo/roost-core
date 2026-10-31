using System;

namespace Roost.Client
{
    /// <summary>一次重连握手的结果；失败时 WillRetry=false 表示本轮不再重试。</summary>
    public sealed class ReconnectResult
    {
        public int Attempt { get; }
        public long ConnectionVersion { get; }
        public Exception? Error { get; }
        public bool Succeeded => Error == null;
        public bool WillRetry { get; }

        internal ReconnectResult(int attempt, long connectionVersion, Exception? error, bool willRetry)
        {
            Attempt = attempt;
            ConnectionVersion = connectionVersion;
            Error = error;
            WillRetry = willRetry;
        }
    }
}
