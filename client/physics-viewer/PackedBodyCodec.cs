using System;
using System.IO;
using System.Numerics;

namespace Roost.PhysicsViewer
{
    // 与 gameplay/physics QuantizedLayout 对应；不依赖 Unity，供金样与坏包回归复用。
    public static class PackedBodyCodec
    {
        static readonly int[] Sizes = {8,7,12,6,1};
        public struct State
        {
            public Vector3 Position, Velocity, AngularVelocity;
            public Quaternion Rotation;
            public bool Sleeping;
        }
        public static byte[] Apply(byte[] baseline, byte[] patch)
        {
            if(patch.Length==0 || patch[0]==0 || patch[0]>31 || baseline==null&&patch[0]!=31 || baseline!=null&&baseline.Length!=34)
                throw new InvalidDataException("Packed field baseline/mask");
            byte[] result=new byte[34];if(baseline!=null)Buffer.BlockCopy(baseline,0,result,0,34);
            int source=1,target=0;
            for(int i=0;i<Sizes.Length;i++)
            {
                int size=Sizes[i];
                if((patch[0]&(1<<i))!=0)
                {
                    if(source+size>patch.Length)throw new InvalidDataException("Truncated packed field");
                    Buffer.BlockCopy(patch,source,result,target,size);source+=size;
                }
                target+=size;
            }
            if(source!=patch.Length)throw new InvalidDataException("Trailing packed field");
            return result;
        }
        public static State Decode(byte[] bytes,int chunkX,int chunkZ)
        {
            if(bytes.Length!=34)throw new InvalidDataException("Packed body size");
            using var r=new BinaryReader(new MemoryStream(bytes));
            float x=r.ReadInt16()/100f,z=r.ReadInt16()/100f,y=r.ReadInt32()/100f;
            if(x<0||x>25||z<0||z>25)throw new InvalidDataException("Local position range");
            int largest=r.ReadByte();if(largest>3)throw new InvalidDataException("Quaternion index");
            float[] q=new float[4];double sum=0;
            for(int i=0;i<4;i++)if(i!=largest){double v=r.ReadInt16()/(Math.Sqrt(2)*32767);q[i]=(float)v;sum+=v*v;}
            if(sum>1)throw new InvalidDataException("Packed quaternion norm");
            q[largest]=(float)Math.Sqrt(1-sum);
            var velocity=new Vector3(r.ReadInt32()/100f,r.ReadInt32()/100f,r.ReadInt32()/100f);
            var angular=new Vector3(r.ReadInt16()/100f,r.ReadInt16()/100f,r.ReadInt16()/100f);
            byte sleeping=r.ReadByte();if(sleeping>1)throw new InvalidDataException("Packed sleeping flag");
            return new State{Position=new Vector3(chunkX*25f+x,y,chunkZ*25f+z),Rotation=new Quaternion(q[0],q[1],q[2],q[3]),Velocity=velocity,AngularVelocity=angular,Sleeping=sleeping!=0};
        }
    }
}
