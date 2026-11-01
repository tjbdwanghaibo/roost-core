using System;
using System.Numerics;

namespace Roost.PhysicsViewer
{
    // 只做展示；时间来自已提交服务器 tick，收包停顿后不无限外推。
    public struct DisplayPose
    {
        public Vector3 Position;
        public Quaternion Rotation;
    }
    public sealed class SparsePosePlayback
    {
        readonly Roost.Client.SnapshotBuffer<DisplayPose> snapshots = new Roost.Client.SnapshotBuffer<DisplayPose>(32, .02, .2);
        double received, serverTime;
        ulong tick;
        bool initialized;
        public void Sample(DisplayPose pose, ulong sampleTick, double now)
        {
            if (initialized && sampleTick <= tick) return;
            // 这里是物理模拟 tick（20ms 步长），不是 EntitySync 的每连接发送序号。
            double timestamp = sampleTick * .02;
            if (!snapshots.Add(timestamp, now, pose)) return;
            serverTime = timestamp;
            tick = sampleTick;
            received = now;
            initialized = true;
        }
        public DisplayPose Read(double now)
        {
            // 包中没有 UTC 锚点，只能从最近物理样本估计表现时间；不冒充 Gate 校时。
            snapshots.TrySample(serverTime + Math.Max(0, now - received), Interpolate, out var pose);
            return pose;
        }
        static DisplayPose Interpolate(DisplayPose a, DisplayPose b, double alpha) => new DisplayPose {
            Position = Vector3.Lerp(a.Position, b.Position, (float)alpha),
            Rotation = Quaternion.Slerp(a.Rotation, b.Rotation, (float)alpha)
        };
    }
    public struct MotionDescription
    {
        public Vector3 Translation, Axis;
        public float PeriodSeconds, AngularSpeed;
    }
    public static class MotionPlayback
    {
        // 周期与轴来自摘要校验后的关卡；用最近权威姿态校准，避免累积积分漂移。
        public static DisplayPose Evaluate(MotionDescription motion, DisplayPose sample, ulong sampleTick, double renderTick)
        {
            double step = (double)(float).02;
            double from = sampleTick * step, to = renderTick * step;
            double phaseFrom = Math.Sin(2 * Math.PI * (from % motion.PeriodSeconds) / motion.PeriodSeconds);
            double phaseTo = Math.Sin(2 * Math.PI * (to % motion.PeriodSeconds) / motion.PeriodSeconds);
            double angle = ((to - from) * motion.AngularSpeed) % (2 * Math.PI);
            Quaternion rotation = motion.AngularSpeed == 0 ? Quaternion.Identity : Quaternion.CreateFromAxisAngle(motion.Axis, (float)angle);
            return new DisplayPose { Position = sample.Position + motion.Translation * (float)(phaseTo - phaseFrom), Rotation = Quaternion.Normalize(rotation * sample.Rotation) };
        }
    }
}
