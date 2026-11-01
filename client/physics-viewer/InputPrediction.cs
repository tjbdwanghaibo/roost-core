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
        bool ready, active;
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
        public void Reconcile(ulong sequence, bool inputActive, PredictedState state, double now)
        {
            lock (mutex)
            {
                if (history.Acknowledge(sequence, out var input))
                    acknowledged = input;
                authority = state;
                receivedAt = now;
                ready = true;
                active = inputActive;
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
                if (history.Count == 0)
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
