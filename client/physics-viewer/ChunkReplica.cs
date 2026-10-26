using System;
using System.Collections.Generic;
using System.IO;
using UnityEngine;

namespace Roost.PhysicsViewer
{
    public struct BodyState
    {
        public ulong Id, InputSequence, InputGeneration;
        public bool InputActive;
        public Vector3 Position, Velocity, AngularVelocity;
        public Quaternion Rotation;
        public bool Sleeping;
    }

    // 对应 Go RC1 无损对照及 Room RP1/RC2 字段增量。整帧校验成功才替换对象表，迁移按稳定 ID 覆盖。
    public sealed class ChunkReplica
    {
        public ulong Epoch, Sequence, Tick;
        public ulong InputSequence, InputGeneration;
        public bool InputActive;
        bool packedMode;
        Dictionary<ulong, byte[]> packed = new Dictionary<ulong, byte[]>();
        Dictionary<ulong, (int x,int z)> chunks = new Dictionary<ulong, (int x,int z)>();
        public Dictionary<ulong, BodyState> Bodies = new Dictionary<ulong, BodyState>();
        public bool Apply(byte[] payload)
        {
            if (payload.Length < 36 || payload.Length > 1048576)
                throw new InvalidDataException("Chunk size");
            using var r = new BinaryReader(new MemoryStream(payload));
            bool room=payload[0]=='R'&&payload[1]=='P';
            ulong inputSequence=0,inputGeneration=0;bool inputActive=false;
            if(room) {
                if(payload.Length<56||r.ReadByte()!='R'||r.ReadByte()!='P'||r.ReadByte()!=1)throw new InvalidDataException("Room version");
                byte active=r.ReadByte();if(active>1)throw new InvalidDataException("Input flag");inputActive=active==1;
                inputSequence=r.ReadUInt64();inputGeneration=r.ReadUInt64();
                if(inputSequence!=0&&inputGeneration==0||inputActive&&inputSequence==0)throw new InvalidDataException("Input acknowledgement");
            }
            if (r.ReadByte() != 'R' || r.ReadByte() != 'C' || r.ReadByte() != (room?2:1))
                throw new InvalidDataException("Chunk version");
            byte mode = r.ReadByte();
            if (mode > 1)
                throw new InvalidDataException("Chunk mode");
            bool full = mode == 1;
            ulong epoch = r.ReadUInt64(), seq = r.ReadUInt64(), tick = r.ReadUInt64();
            uint deletes = r.ReadUInt32(), groups = r.ReadUInt32();
            if (epoch == 0 || seq == 0 || deletes > 16384 || groups > 16384 || (full && deletes != 0))
                throw new InvalidDataException("Chunk header");
            if (!full && (packedMode!=room || Sequence == 0 || epoch != Epoch || seq != Sequence + 1 || tick < Tick))
                throw new InvalidDataException("Chunk gap: reconnect required");
            var next = full ? new Dictionary<ulong, BodyState>() : new Dictionary<ulong, BodyState>(Bodies);
            var nextPacked=full?new Dictionary<ulong,byte[]>():new Dictionary<ulong,byte[]>(packed);
            var nextChunks=full?new Dictionary<ulong,(int x,int z)>():new Dictionary<ulong,(int x,int z)>(chunks);
            var seen = new HashSet<ulong>();
            for (uint i = 0; i < deletes; i++)
            {
                ulong id = r.ReadUInt64();
                if (id == 0 || !seen.Add(id))
                    throw new InvalidDataException("Duplicate change");
                next.Remove(id);nextPacked.Remove(id);nextChunks.Remove(id);
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
                    if(id==0||!seen.Add(id))throw new InvalidDataException("Duplicate change");
                    int size=r.ReadUInt16();
                    BodyState state;
                    if(room) {
                        byte[] patch=r.ReadBytes(size);if(patch.Length!=size)throw new EndOfStreamException();
                        byte[] baseline=null;
                        if(!full&&nextChunks.TryGetValue(id,out var key)&&key.x==x&&key.z==z)nextPacked.TryGetValue(id,out baseline);
                        byte[] complete=PackedBodyCodec.Apply(baseline,patch);
                        var value=PackedBodyCodec.Decode(complete,x,z);
                        state=new BodyState{Id=id,Position=new Vector3(value.Position.X,value.Position.Y,value.Position.Z),
                            Rotation=new Quaternion(value.Rotation.X,value.Rotation.Y,value.Rotation.Z,value.Rotation.W),
                            Velocity=new Vector3(value.Velocity.X,value.Velocity.Y,value.Velocity.Z),
                            AngularVelocity=new Vector3(value.AngularVelocity.X,value.AngularVelocity.Y,value.AngularVelocity.Z),Sleeping=value.Sleeping};
                        nextPacked[id]=complete;nextChunks[id]=(x,z);
                    } else {
                        // Comparison/Crowd 的无损对照入口继续使用 RC1，不混入量化误差。
                        if(size!=70)throw new InvalidDataException("Body format");
                        var p=Vector(r)+new Vector3(x*25f,0,z*25f);
                        var q=new Quaternion(Number(r),Number(r),Number(r),Number(r));var v=Vector(r);var w=Vector(r);
                        byte sleep=r.ReadByte();ulong sequence=r.ReadUInt64();byte active=r.ReadByte();ulong generation=r.ReadUInt64();
                        if(sleep>1||active>1||Quaternion.Dot(q,q)<.5f||Quaternion.Dot(q,q)>1.5f)throw new InvalidDataException("Body state");
                        state=new BodyState{Id=id,Position=p,Rotation=q,Velocity=v,AngularVelocity=w,Sleeping=sleep!=0,InputSequence=sequence,InputGeneration=generation,InputActive=active!=0};
                    }
                    next[id]=state;
                    if (next.Count > 16384)
                        throw new InvalidDataException("Object limit");
                }
            }
            if (r.BaseStream.Position != r.BaseStream.Length)
                throw new InvalidDataException("Trailing bytes");
            Epoch = epoch;
            Sequence = seq;
            Tick = tick;
            Bodies = next;packed=nextPacked;chunks=nextChunks;
            InputSequence=inputSequence;InputGeneration=inputGeneration;InputActive=inputActive;packedMode=room;
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
