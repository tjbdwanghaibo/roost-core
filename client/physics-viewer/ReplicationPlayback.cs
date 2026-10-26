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
        DisplayPose start, target;
        double received, duration;
        ulong tick;
        bool initialized;
        public void Sample(DisplayPose pose, ulong sampleTick, double now)
        {
            if (initialized && sampleTick == tick) return;
            start = initialized ? Read(now) : pose;
            duration = initialized && sampleTick > tick ? Math.Clamp((sampleTick - tick) * .02, .02, .2) : .02;
            target = pose;
            tick = sampleTick;
            received = now;
            initialized = true;
        }
        public DisplayPose Read(double now)
        {
            float alpha = (float)Math.Clamp((now - received) / duration, 0, 1);
            return new DisplayPose { Position = Vector3.Lerp(start.Position, target.Position, alpha), Rotation = Quaternion.Slerp(start.Rotation, target.Rotation, alpha) };
        }
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
