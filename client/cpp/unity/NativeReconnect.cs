using System;
using System.Diagnostics;
namespace Roost.Native
{
// 可选重连策略：业务仍负责异步获取新票据和解析域名。每帧在 game.Pump 后调 Tick。
// Provider 返回 null 表示尚未准备好；不要在 Provider 里同步等待账号服务或 DNS。
public sealed class NativeReconnect : IDisposable
{
    public sealed class Ticket
    {
        public string Address, Value;
        public ushort Port;
    }
    private readonly NativeGameClient game;
    private readonly int thread = Environment.CurrentManagedThreadId;
    private void Check()
    {
        if (thread != Environment.CurrentManagedThreadId)
            throw new InvalidOperationException("Use the engine thread");
    }
    private readonly Func<int, Ticket> provider;
    private readonly int maxAttempts;
    private readonly double initialDelay, maxDelay;
    private readonly Stopwatch clock = Stopwatch.StartNew();
    private bool established, waiting, recovering, disposed;
    private int attempts;
    private double delay, due;
    public event Action<int> Attempting;
    public event Action Reconnected, Exhausted;
    public event Action<Exception> Error;
    public NativeReconnect(NativeGameClient game, Func<int, Ticket> provider, int maxAttempts = 8,
                           double initialDelaySeconds = 1, double maxDelaySeconds = 30)
    {
        this.game = game ?? throw new ArgumentNullException(nameof(game));
        this.provider = provider ?? throw new ArgumentNullException(nameof(provider));
        if (maxAttempts < 1 || !Finite(initialDelaySeconds) || !Finite(maxDelaySeconds) || initialDelaySeconds <= 0 ||
            maxDelaySeconds < initialDelaySeconds)
            throw new ArgumentException("Invalid reconnect policy");
        this.maxAttempts = maxAttempts;
        initialDelay = delay = initialDelaySeconds;
        maxDelay = maxDelaySeconds;
        game.Ready += Ready;
        game.Closed += Closed;
    }
    private static bool Finite(double value) => !double.IsNaN(value) && !double.IsInfinity(value);
    private void Ready()
    {
        bool recovered = recovering;
        established = true;
        waiting = recovering = false;
        attempts = 0;
        delay = initialDelay;
        if (recovered)
            Reconnected?.Invoke();
    }
    private void Closed(NativeClient.Event value)
    {
        if (established)
            Schedule();
    }
    private void Schedule()
    {
        if (disposed)
            return;
        if (attempts >= maxAttempts)
        {
            waiting = recovering = false;
            Exhausted?.Invoke();
            return;
        }
        waiting = recovering = true;
        due = clock.Elapsed.TotalSeconds + delay;
        delay = Math.Min(maxDelay, delay * 2);
    }
    public void Tick()
    {
        Check();
        if (disposed)
            throw new ObjectDisposedException(nameof(NativeReconnect));
        if (!waiting || clock.Elapsed.TotalSeconds < due)
            return;
        Ticket ticket;
        try
        {
            ticket = provider(attempts + 1);
        }
        catch (Exception e)
        {
            attempts++;
            Schedule();
            Error?.Invoke(e);
            return;
        }
        if (ticket == null || disposed || !waiting)
            return;
        waiting = false;
        attempts++;
        try
        {
            Attempting?.Invoke(attempts);
            if (!disposed && recovering)
                game.Connect(ticket.Address, ticket.Port, ticket.Value);
        }
        catch (Exception e)
        {
            Schedule();
            Error?.Invoke(e);
        }
    }
    // 主动离线/切账号前调用 Stop，再 Disconnect。再次显式 Connect 的 Ready 会重新启用策略。
    public void Stop()
    {
        Check();
        waiting = recovering = established = false;
        attempts = 0;
        delay = initialDelay;
    }
    public void Dispose()
    {
        Check();
        if (disposed)
            return;
        disposed = true;
        Stop();
        game.Ready -= Ready;
        game.Closed -= Closed;
    }
}
}
