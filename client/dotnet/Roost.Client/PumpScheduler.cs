using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Threading;
using System.Threading.Tasks;

namespace Roost.Client
{
    /// <summary>
    /// Connection 与 TcpSession 的计时与等待（ConnectionOptions.PumpDriven，照开房间 SDK 的 RelayClientOptions.PumpDriven）。
    ///
    /// - 缺省（<see cref="Threaded"/>）：计时器与线程池——CancelAfter、Task.Delay，等待之后不回调用者的上下文（ConfigureAwait(false)），
    ///   与以前的行为相同。
    /// - PumpDriven：Unity WebGL 上线程池、计时器与 ConfigureAwait(false) 之后的续行都不运行（2026-11-17 实测）。计时登记在这里，
    ///   由 <see cref="Pump"/>（GameClient.Pump 开头调用）按单调时钟触发；所有等待回到调用者的上下文（<see cref="ContinueOnContext"/>）。
    ///   到期的计时投到调用 Pump 的线程的 SynchronizationContext 再触发，不在 Pump 里同步跑：被它唤醒的续行（例如业务 await
    ///   ConnectAsync 之后的代码）可能再调 Pump，而 Pump 不可重入。不 Pump 就不超时、不发心跳、不重连。
    /// </summary>
    internal sealed class PumpScheduler
    {
        internal static readonly PumpScheduler Threaded = new PumpScheduler(false);
        private static readonly Stopwatch Clock = Stopwatch.StartNew();

        private readonly object gate = new object();
        private readonly List<Timer> timers = new List<Timer>();

        internal PumpScheduler(bool pumpDriven) { PumpDriven = pumpDriven; }

        internal bool PumpDriven { get; }
        /// <summary>每个 await 的 ConfigureAwait 参数：PumpDriven 时回到调用者的上下文。</summary>
        internal bool ContinueOnContext => PumpDriven;

        /// <summary>
        /// delay 之后取消 source。返回的句柄要在不再需要时 Dispose（撤掉还没到期的计时）；缺省调度下返回 null（CancelAfter 自己管）。
        /// delay 为 Timeout.InfiniteTimeSpan 时不计时。
        /// </summary>
        internal IDisposable? CancelAfter(CancellationTokenSource source, TimeSpan delay)
        {
            if (delay == Timeout.InfiniteTimeSpan) return null;
            if (!PumpDriven) { source.CancelAfter(delay); return null; }
            return After(delay, () => { try { source.Cancel(); } catch (ObjectDisposedException) { /* 操作已经结束 */ } });
        }

        /// <summary>等 delay（Timeout.InfiniteTimeSpan 表示只等取消）；cancellation 取消时抛 OperationCanceledException。</summary>
        internal Task Delay(TimeSpan delay, CancellationToken cancellation)
        {
            if (!PumpDriven) return Task.Delay(delay, cancellation);
            // 不用 RunContinuationsAsynchronously：完成它的是 Pump 投到上下文的计时或调 Cancel 的线程，续行按 await 的上下文走。
            var done = new TaskCompletionSource<bool>();
            if (cancellation.IsCancellationRequested) { done.TrySetCanceled(cancellation); return done.Task; }
            Timer? timer = null;
            CancellationTokenRegistration registration = default;
            if (delay != Timeout.InfiniteTimeSpan)
                timer = After(delay, () => { registration.Dispose(); done.TrySetResult(true); });
            registration = cancellation.Register(() => { timer?.Dispose(); done.TrySetCanceled(cancellation); });
            return done.Task;
        }

        /// <summary>
        /// PumpDriven：触发到期的计时（投到当前线程的 SynchronizationContext；没有上下文时就地触发）。缺省调度下什么也不做。
        /// </summary>
        internal void Pump()
        {
            if (!PumpDriven) return;
            long now = Clock.Elapsed.Ticks;
            List<Timer>? due = null;
            lock (gate)
            {
                for (int i = timers.Count - 1; i >= 0; i--)
                {
                    if (timers[i].Due > now) continue;
                    (due ??= new List<Timer>()).Add(timers[i]);
                    timers.RemoveAt(i);
                }
            }
            if (due == null) return;
            var context = SynchronizationContext.Current;
            foreach (var timer in due)
            {
                if (context != null) context.Post(state => ((Timer)state!).Fire(), timer);
                else timer.Fire();
            }
        }

        private Timer After(TimeSpan delay, Action fire)
        {
            var timer = new Timer(this, Clock.Elapsed.Ticks + Math.Max(0, delay.Ticks), fire);
            lock (gate) timers.Add(timer);
            return timer;
        }

        private sealed class Timer : IDisposable
        {
            private readonly PumpScheduler owner;
            private readonly Action fire;
            private int state; // 0 等着，1 已触发或已撤掉
            internal readonly long Due;

            internal Timer(PumpScheduler owner, long due, Action fire) { this.owner = owner; Due = due; this.fire = fire; }

            /// <summary>到期触发；投到上下文之后、执行之前被撤掉的不再触发。</summary>
            internal void Fire()
            {
                if (Interlocked.Exchange(ref state, 1) == 0) fire();
            }

            public void Dispose()
            {
                if (Interlocked.Exchange(ref state, 1) != 0) return;
                lock (owner.gate) owner.timers.Remove(this);
            }
        }
    }
}
