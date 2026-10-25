using System;
using System.Collections.Generic;
using System.IO;
using UnityEngine;

namespace Roost.PhysicsViewer
{
    public struct BodyState
    {
        public ulong Id;
        public Vector3 Position, Velocity, AngularVelocity;
        public Quaternion Rotation;
        public bool Sleeping;
    }

    // 与 Go chunksync v1 对应。整帧校验成功才替换对象表，迁移按稳定 ID 覆盖。
    public sealed class ChunkReplica
    {
        public ulong Epoch, Sequence, Tick;
        public Dictionary<ulong, BodyState> Bodies = new Dictionary<ulong, BodyState>();
        public bool Apply(byte[] payload)
        {
            if (payload.Length < 36 || payload.Length > 1048576)
                throw new InvalidDataException("Chunk size");
            using var r = new BinaryReader(new MemoryStream(payload));
            if (r.ReadByte() != 'R' || r.ReadByte() != 'C' || r.ReadByte() != 1)
                throw new InvalidDataException("Chunk version");
            byte mode = r.ReadByte();
            if (mode > 1)
                throw new InvalidDataException("Chunk mode");
            bool full = mode == 1;
            ulong epoch = r.ReadUInt64(), seq = r.ReadUInt64(), tick = r.ReadUInt64();
            uint deletes = r.ReadUInt32(), groups = r.ReadUInt32();
            if (epoch == 0 || seq == 0 || deletes > 16384 || groups > 16384 || (full && deletes != 0))
                throw new InvalidDataException("Chunk header");
            if (!full && (Sequence == 0 || epoch != Epoch || seq != Sequence + 1 || tick < Tick))
                throw new InvalidDataException("Chunk gap: reconnect required");
            var next = full ? new Dictionary<ulong, BodyState>() : new Dictionary<ulong, BodyState>(Bodies);
            var seen = new HashSet<ulong>();
            for (uint i = 0; i < deletes; i++)
            {
                ulong id = r.ReadUInt64();
                if (id == 0 || !seen.Add(id))
                    throw new InvalidDataException("Duplicate change");
                next.Remove(id);
            }
            for (uint g = 0; g < groups; g++)
            {
                int x = r.ReadInt32(), z = r.ReadInt32();
                uint count = r.ReadUInt32();
                if (count > 16384)
                    throw new InvalidDataException("Object count");
                for (uint i = 0; i < count; i++)
                {
                    ulong id = r.ReadUInt64();
                    if (id == 0 || !seen.Add(id) || r.ReadUInt16() != 53)
                        throw new InvalidDataException("Body format");
                    var p = Vector(r) + new Vector3(x * 25f, 0, z * 25f);
                    var q = new Quaternion(Number(r), Number(r), Number(r), Number(r));
                    var v = Vector(r);
                    var w = Vector(r);
                    byte sleep = r.ReadByte();
                    if (sleep > 1 || Quaternion.Dot(q, q) < 0.5f || Quaternion.Dot(q, q) > 1.5f)
                        throw new InvalidDataException("Invalid body state");
                    next[id] = new BodyState { Id = id,      Position = p,        Rotation = q,
                                               Velocity = v, AngularVelocity = w, Sleeping = sleep == 1 };
                    if (next.Count > 16384)
                        throw new InvalidDataException("Object limit");
                }
            }
            if (r.BaseStream.Position != r.BaseStream.Length)
                throw new InvalidDataException("Trailing bytes");
            Epoch = epoch;
            Sequence = seq;
            Tick = tick;
            Bodies = next;
            return full;
        }
        static float Number(BinaryReader r)
        {
            float n = r.ReadSingle();
            if (float.IsNaN(n) || float.IsInfinity(n))
                throw new InvalidDataException("Non-finite state");
            return n;
        }
        static Vector3 Vector(BinaryReader r) => new Vector3(Number(r), Number(r), Number(r));
    }
}
