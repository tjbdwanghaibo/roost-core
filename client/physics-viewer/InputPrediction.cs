using System;
using System.Numerics;

namespace Roost.PhysicsViewer
{
    public struct PredictedState
    {
        public Vector3 Position, Velocity;
    }
    public struct PredictedInput
    {
        public ulong Sequence;
        public Vector3 Acceleration;
        public bool Jump;
        public double At;
    }
    // 输入历史有界，服务器每帧的实际采样序列用于删去已确认/已被新输入覆盖的历史。
    // 这是近似本地预测，碰撞由 Unity 适配器提供；不宣称跨 PhysX 版本逐位确定。
    public sealed class InputPrediction
    {
        readonly object mutex = new object();
        readonly Roost.Client.PredictionHistory<PredictedInput> history =
            new Roost.Client.PredictionHistory<PredictedInput>(32);
        PredictedInput acknowledged;
        PredictedState authority;
        double receivedAt;
        bool ready, active, discontinuous;
        public const double Horizon = .25;
        public bool Held
        {
            get {
                lock (mutex) return history.Held;
            }
        }
        public void Record(PredictedInput input)
        {
            lock (mutex)
            {
                if (double.IsNaN(input.At) || double.IsInfinity(input.At))
                    throw new ArgumentException("Prediction input order");
                if (history.Count != 0 && input.At < history[history.Count - 1].At)
                    throw new ArgumentException("Prediction clock order");
                history.Record(input.Sequence, input);
            }
        }
        // discontinuity：这一帧里自己的刚体带传送标记（被传送或重生）。权威状态照常成为基线，这一帧确认的输入
        // 及更早的历史照常删去，之后的输入照常重放；不同的是没有更新的输入可重放时，不再拿已确认的那条输入
        // 往前外推：它是在旧位置上按下的，而服务端传送时已经把速度清零。下一帧不带标记，恢复外推。
        public void Reconcile(ulong sequence, bool inputActive, PredictedState state, double now, bool discontinuity = false)
        {
            lock (mutex)
            {
                if (history.Acknowledge(sequence, out var input))
                    acknowledged = input;
                authority = state;
                receivedAt = now;
                ready = true;
                active = inputActive;
                discontinuous = discontinuity;
            }
        }
        public PredictedState Predict(double now, Func<PredictedState, PredictedInput, float, PredictedState> step)
        {
            lock (mutex)
            {
                if (!ready || history.Held || now < receivedAt || now - receivedAt > Horizon)
                    return authority;
                var state = authority;
                double budget = Horizon;
                // 邮箱输入会被合并；这里只重放尚未被确认的本地时间片，最新输入最多外推至 Horizon。
                for (int i = 0; i < history.Count && budget > 0; i++)
                {
                    var input = history[i];
                    if (now - input.At > Horizon)
                        continue;
                    double end = i + 1 < history.Count ? history[i + 1].At : now;
                    double span = Math.Min(budget, Math.Max(0, Math.Min(.05, end - input.At)));
                    Integrate(ref state, input, span, step);
                    budget -= span;
                }
                if (history.Count == 0 && !discontinuous)
                {
                    var input = acknowledged;
                    input.Jump = false;
                    if (!active)
                        input.Acceleration = Vector3.Zero;
                    Integrate(ref state, input, Math.Min(budget, now - receivedAt), step);
                }
                return state;
            }
        }
        static void Integrate(ref PredictedState state, PredictedInput input, double duration,
                              Func<PredictedState, PredictedInput, float, PredictedState> step)
        {
            while (duration > 1e-6)
            {
                float dt = (float)Math.Min(.02, duration);
                state = step(state, input, dt);
                input.Jump = false;
                duration -= dt;
            }
        }
    }
}
