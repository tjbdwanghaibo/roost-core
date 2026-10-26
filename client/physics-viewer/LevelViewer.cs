using System;
using System.IO;
using System.Text;
using System.Security.Cryptography;
using UnityEngine;
using NVector = System.Numerics.Vector3;

namespace Roost.PhysicsViewer
{
    public sealed partial class CrowdViewer
    {
        public bool PredictLocalInput = true;
        public float PredictionOffsetMeters { get; private set; }
        readonly InputPrediction prediction = new InputPrediction();
        static readonly System.Diagnostics.Stopwatch clock = System.Diagnostics.Stopwatch.StartNew();
        volatile byte[] levelContent;
        ulong controlledBody;
        float controlledRadius=.5f, controlledHalfHeight;
        bool levelReady, predictionReady;
        long inputGeneration;
        ulong grabTarget;
        [Serializable] sealed class LevelDocument
        {
            public int Version;
            public string Name;
            public uint Revision;
            public LevelBody[] Bodies;
            public ulong[] Players;
            public LevelMotion[] Motions;
        }
        [Serializable] sealed class LevelBody
        {
            public ulong ID;
            public int Shape, Motion;
            public float[] Position, Rotation, Dimensions;
            public float Mass;
        }
        [Serializable] sealed class LevelMotion
        {
            public ulong ID;
            public float[] Translation, Axis;
            public float PeriodSeconds, AngularSpeed;
        }
        readonly System.Collections.Generic.Dictionary<ulong,MotionDescription> motionDescriptions=new System.Collections.Generic.Dictionary<ulong,MotionDescription>();
        static ulong Varint(BinaryReader r)
        {
            ulong v=0;
            for(int i=0;i<10;i++)
            {
                byte b=r.ReadByte();if(i==9 && b>1) throw new InvalidDataException("Varint overflow");
                v|=(ulong)(b&127)<<(7*i); if((b&128)==0) return v;
            }
            throw new InvalidDataException("Varint length");
        }
        static byte[] Bytes(BinaryReader r)
        {
            ulong n=Varint(r);if(n>262144) throw new InvalidDataException("Level size");
            var data=r.ReadBytes((int)n);if(data.Length!=(int)n)throw new EndOfStreamException();return data;
        }
        void ReceiveLevel(byte[] payload)
        {
            using var r=new BinaryReader(new MemoryStream(payload));
            if(r.ReadByte()!=10)throw new InvalidDataException("Level content field");
            byte[] content=Bytes(r);
            if(r.ReadByte()!=18)throw new InvalidDataException("Level hash field");
            string expected=Encoding.UTF8.GetString(Bytes(r));
            if(r.ReadByte()!=24)throw new InvalidDataException("Level player field");
            controlledBody=Varint(r);
            if(controlledBody==0||r.BaseStream.Position!=r.BaseStream.Length)throw new InvalidDataException("Level player");
            using var sha=SHA256.Create();
            string actual=BitConverter.ToString(sha.ComputeHash(content)).Replace("-","").ToLowerInvariant();
            if(actual!=expected)throw new InvalidDataException("Level content hash mismatch");
            levelContent=content; // 发布完整校验后的字节，Unity 对象只在主线程创建。
        }
        void BuildLevel()
        {
            var l=JsonUtility.FromJson<LevelDocument>(Encoding.UTF8.GetString(levelContent));
            if(l.Version!=1||l.Revision==0||l.Bodies==null||l.Bodies.Length>256)throw new InvalidDataException("Level schema");
            bool found=false;
            foreach(var b in l.Bodies)
            {
                if(b.ID==0 || bodies.ContainsKey(b.ID)||b.Shape<0||b.Shape>2||b.Motion<0||b.Motion>3)throw new InvalidDataException("Level body");
                bool moving=b.Motion==2||b.Motion==3||(b.Motion==0&&b.Shape!=0);
                bool controlled=b.ID==controlledBody;
                var obj=Primitive(b.Shape==0?PrimitiveType.Cube:b.Shape==1?PrimitiveType.Capsule:PrimitiveType.Sphere,
                                  "Body "+b.ID,controlled?Color.cyan:moving?Color.yellow:new Color(.35f,.45f,.55f),!controlled);
                obj.position=new Vector3(b.Position[0],b.Position[1],b.Position[2]);
                var rotation=new Quaternion(b.Rotation[0],b.Rotation[1],b.Rotation[2],b.Rotation[3]);
                obj.rotation=Quaternion.Dot(rotation,rotation)<.5f?Quaternion.identity:rotation;
                float radius=b.Dimensions[0];
                obj.localScale=b.Shape==0?new Vector3(radius,b.Dimensions[1],b.Dimensions[2])*2:
                    b.Shape==1?new Vector3(radius*2,b.Dimensions[1]+radius,radius*2):Vector3.one*radius*2;
                bodies[b.ID]=obj;
                if(!moving) staticBodies.Add(b.ID);
                if(controlled){ found=true;controlledRadius=radius;controlledHalfHeight=b.Shape==1?b.Dimensions[1]:0; }
            }
            if(!found)throw new InvalidDataException("Controlled body missing");
            if(l.Motions!=null) foreach(var m in l.Motions) {
                LevelBody body=Array.Find(l.Bodies,b=>b.ID==m.ID);
                if(body==null||body.Motion!=3||motionDescriptions.ContainsKey(m.ID)||m.Translation==null||m.Translation.Length!=3||m.Axis==null||m.Axis.Length!=3||m.PeriodSeconds<.1f||m.PeriodSeconds>3600||float.IsNaN(m.PeriodSeconds)) throw new InvalidDataException("Level motion");
                motionDescriptions[m.ID]=new MotionDescription {Translation=new NVector(m.Translation[0],m.Translation[1],m.Translation[2]),Axis=new NVector(m.Axis[0],m.Axis[1],m.Axis[2]),PeriodSeconds=m.PeriodSeconds,AngularSpeed=m.AngularSpeed};
            }
            levelReady=true;
        }
        static NVector N(Vector3 v)=>new NVector(v.x,v.y,v.z);
        static Vector3 U(NVector v)=>new Vector3(v.X,v.Y,v.Z);
        PredictedState PredictStep(PredictedState state,PredictedInput input,float dt)
        {
            Vector3 p=U(state.Position),v=U(state.Velocity),up=Vector3.up*controlledHalfHeight;
            bool grounded=UnityEngine.Physics.CapsuleCast(p-up,p+up,controlledRadius*.98f,Vector3.down,out var ground,.08f,
                                                       ~0,QueryTriggerInteraction.Ignore)&&ground.normal.y>=.5f;
            Vector3 supportVelocity=Vector3.zero;
            if(grounded) {
                foreach(var pair in replica.Bodies) {
                    if(bodies.TryGetValue(pair.Key,out var support)&&support==ground.collider.transform&&!pair.Value.DisplayOnly) {
                        supportVelocity=pair.Value.Velocity+Vector3.Cross(pair.Value.AngularVelocity,ground.point-pair.Value.Position);
                        break;
                    }
                }
            }
            if(input.Jump&&grounded&&Mathf.Abs(v.y-supportVelocity.y)<=1)v.y+=7;
            v+=(U(input.Acceleration)+Vector3.down*9.81f)*dt;
            Vector3 move=v*dt;
            for(int i=0;i<3&&move.sqrMagnitude>1e-8f;i++)
            {
                if(!UnityEngine.Physics.CapsuleCast(p-up,p+up,controlledRadius*.98f,move.normalized,out var hit,move.magnitude+.01f,
                                                   ~0,QueryTriggerInteraction.Ignore)){p+=move;break;}
                float travel=Mathf.Max(0,hit.distance-.01f);
                p+=move.normalized*travel;
                move=Vector3.ProjectOnPlane(move.normalized*Mathf.Max(0,move.magnitude-travel),hit.normal);
                if(Vector3.Dot(v,hit.normal)<0)v=Vector3.ProjectOnPlane(v,hit.normal);
            }
            return new PredictedState{Position=N(p),Velocity=N(v)};
        }
        readonly System.Collections.Generic.HashSet<ulong> staticBodies=new System.Collections.Generic.HashSet<ulong>();
        ulong NearestGrab()
        {
            if(!replica.Bodies.TryGetValue(controlledBody,out var self))return 0;
            ulong best=0;float distance=2.5f*2.5f;
            foreach(var pair in replica.Bodies)
            {
                if(pair.Key==controlledBody)continue;
                float d=(pair.Value.Position-self.Position).sqrMagnitude;
                if(d<distance){distance=d;best=pair.Key;}
            }
            return best;
        }
    }
}
