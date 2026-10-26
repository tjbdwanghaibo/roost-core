using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.IO;
using System.Net.Sockets;
using System.Threading;
using UnityEngine;
using Roost.Client;

namespace Roost.PhysicsViewer
{
    // 显示服务器权威刚体；Room 模式只发送输入，不运行第二套本地物理。
    public sealed partial class CrowdViewer : MonoBehaviour
    {
        public string Host = "127.0.0.1";
        public int Port = 19091;
        public bool Interpolate = true;
        public bool ControlRoom;
        public bool UseGate = true;
        public int PlayerSlot = 13;
        public bool VerifyControls;
        public float HorizontalTravel { get; private set; }
        public float VerticalRise { get; private set; }
        public int JumpRequests { get; private set; }
        readonly object inputLock = new object();
        float inputX, inputZ, uiX, uiZ;
        bool jumpPending, firstJump, secondJump, hasOrigin;
        Vector3 origin;
        float startedAt;
        Camera roomCamera;
        Thread sender;
        readonly ChunkReplica replica = new ChunkReplica();
        readonly ConcurrentQueue<byte[]> packets = new ConcurrentQueue<byte[]>();
        readonly Dictionary<ulong, Transform> bodies = new Dictionary<ulong, Transform>();
        readonly Dictionary<ulong, BodyState> previous = new Dictionary<ulong, BodyState>();
        TcpClient socket;
        Thread receiver;
        volatile bool stopping;
        volatile string failure;
        int pending;
        long bytes;
        float receivedAt, interval = .05f;
        public ulong Frames { get; private set; }
        public ulong Tick => replica.Tick;
        public string Status => failure ?? (Frames > 0 ? "Connected" : "Connecting");
        public string Summary => $"frames={Frames} tick={Tick} bodies={replica.Bodies.Count} bytes={Interlocked.Read(ref bytes)}";

        [Serializable] sealed class Terrain
        {
            public int version, players, capsules, big_ball_id;
            public TerrainBox[] boxes;
        }
        [Serializable] sealed class TerrainBox
        {
            public string name;
            public float[] position, half_extents;
            public float tilt_degrees;
        }
        static Color TerrainColor(string name)
        {
            if (name.EndsWith("wall")) return new Color(.4f, .55f, .65f, .12f);
            if (name.Contains("ramp") || name.Contains("slope")) return new Color(.25f, .65f, .65f);
            if (name.Contains("platform") || name.Contains("bridge")) return new Color(.35f, .45f, .75f);
            if (name.StartsWith("Stair")) return new Color(.55f, .55f, .7f);
            if (name.Contains("tunnel")) return new Color(.5f, .45f, .3f);
            return new Color(.25f, .29f, .32f);
        }
        void Start()
        {
            Application.runInBackground = true;
            startedAt = Time.unscaledTime;
            if (!(ControlRoom && UseGate)) {
            var resource = Resources.Load<TextAsset>("PhysicsTerrain");
            if (resource == null) throw new InvalidDataException("PhysicsTerrain resource missing");
            var terrain = JsonUtility.FromJson<Terrain>(resource.text);
            if (terrain.version != 1 || terrain.players != 20 || terrain.capsules != 50 || terrain.big_ball_id != 71)
                throw new InvalidDataException("Terrain version or population mismatch");
            foreach (var box in terrain.boxes)
            {
                var ground = Primitive(PrimitiveType.Cube, box.name, TerrainColor(box.name));
                ground.position = new Vector3(box.position[0], box.position[1], box.position[2]);
                ground.localScale = new Vector3(box.half_extents[0], box.half_extents[1], box.half_extents[2]) * 2;
                ground.rotation = Quaternion.Euler(0, 0, box.tilt_degrees);
            }
            for (ulong id = 1; id <= 71; id++)
            {
                bool capsule = id <= 50;
                var color = id == 71 ? Color.yellow : capsule ? new Color(.85f, .35f, .15f) : Color.HSVToRGB((id - 51) / 20f, .65f, .95f);
                var body = Primitive(capsule ? PrimitiveType.Capsule : PrimitiveType.Sphere,
                                     capsule ? "NPC " + id : id == 71 ? "Free ball" : "Client " + (id - 50), color);
                body.localScale = id == 71 ? Vector3.one * 4 : capsule ? new Vector3(.8f, 1, .8f) : Vector3.one;
                bodies[id] = body;
                body.gameObject.SetActive(false);
            }
            }
            var camera = new GameObject("Crowd Camera").AddComponent<Camera>();
            roomCamera = camera;
            camera.transform.position = new Vector3(40, 43, -48);
            camera.transform.LookAt(new Vector3(0, 2, 0));
            camera.fieldOfView = 55;
            var light = new GameObject("Sun").AddComponent<Light>();
            light.type = LightType.Directional;
            light.transform.rotation = Quaternion.Euler(55, -30, 0);
            receiver = new Thread(ReadLoop) { IsBackground = true, Name = "Roost Crowd Observer" };
            receiver.Start();
        }
        static Transform Primitive(PrimitiveType type, string name, Color color, bool keepCollider=false)
        {
            var obj = GameObject.CreatePrimitive(type);
            obj.name = name;
            if(!keepCollider) DestroyImmediate(obj.GetComponent<Collider>());
            var material = new Material(Shader.Find("Standard")) { color = color };
            if (color.a < 1)
            {
                material.SetFloat("_Mode", 3);
                material.SetInt("_SrcBlend", (int)UnityEngine.Rendering.BlendMode.SrcAlpha);
                material.SetInt("_DstBlend", (int)UnityEngine.Rendering.BlendMode.OneMinusSrcAlpha);
                material.SetInt("_ZWrite", 0);
                material.EnableKeyword("_ALPHABLEND_ON");
                material.renderQueue = 3000;
            }
            obj.GetComponent<Renderer>().sharedMaterial = material;
            return obj.transform;
        }
        static byte[] ReadExactly(Stream stream, int length)
        {
            byte[] data = new byte[length];
            for (int offset = 0; offset < length;)
            {
                int n = stream.Read(data, offset, length - offset);
                if (n == 0) throw new EndOfStreamException();
                offset += n;
            }
            return data;
        }
        static Packet ReadPacket(Stream stream)
        {
            byte[] header = ReadExactly(stream, PacketCodec.HeaderSize);
            uint length = (uint)header[12] << 24 | (uint)header[13] << 16 | (uint)header[14] << 8 | header[15];
            if (length > PacketCodec.DefaultMaxPayload) throw new InvalidDataException("Packet limit");
            byte[] payload = ReadExactly(stream, (int)length);
            byte[] packet = new byte[header.Length + payload.Length];
            Buffer.BlockCopy(header, 0, packet, 0, header.Length);
            Buffer.BlockCopy(payload, 0, packet, header.Length, payload.Length);
            return PacketCodec.Decode(packet);
        }
        // 与 room_input.proto 一致；这里只编码四个标量，不引入第二套网络客户端。
        static byte[] GateInput(ulong sequence, float x, float z, bool jump, ulong grab)
        {
            using (var payload = new MemoryStream())
            {
                payload.WriteByte(8);
                ulong remaining = sequence;
                while (remaining >= 128) { payload.WriteByte((byte)(remaining | 128)); remaining >>= 7; }
                payload.WriteByte((byte)remaining);
                foreach (var field in new[] { Tuple.Create((byte)21, x), Tuple.Create((byte)29, z) })
                {
                    payload.WriteByte(field.Item1);
                    byte[] value = BitConverter.GetBytes(field.Item2);
                    if (!BitConverter.IsLittleEndian) Array.Reverse(value);
                    payload.Write(value, 0, value.Length);
                }
                payload.WriteByte(32);
                payload.WriteByte((byte)(jump ? 1 : 0));
                payload.WriteByte(40);
                remaining=grab;
                while(remaining>=128){payload.WriteByte((byte)(remaining|128));remaining>>=7;}
                payload.WriteByte((byte)remaining);
                return PacketCodec.Encode(new Packet(0x50485902, checked((uint)sequence + 2), payload.ToArray()));
            }
        }
        void ReadLoop()
        {
            try
            {
                socket = new TcpClient();
                socket.Connect(Host, Port);
                socket.ReceiveTimeout = 5000;
                socket.SendTimeout = 1000;
                var stream = socket.GetStream();
                if (ControlRoom)
                {
                    if (PlayerSlot < 1 || PlayerSlot > 20) throw new InvalidDataException("PlayerSlot must be 1..20");
                    byte[] hello = new byte[] { (byte)'P', (byte)'R', (byte)'0', (byte)'1', 0, 0, 0, (byte)PlayerSlot };
                    if (UseGate)
                    {
                        hello = PacketCodec.Encode(new Packet(0, 1, System.Text.Encoding.UTF8.GetBytes("physics-slot-" + PlayerSlot)));
                        stream.Write(hello, 0, hello.Length);
                        var ack = ReadPacket(stream);
                        if (ack.MessageId != 0 || ack.Sequence != 1 || ack.IsPush || ack.Kind != PayloadKind.Protobuf)
                            throw new InvalidDataException("Gate authentication failed");
                        var request=PacketCodec.Encode(new Packet(0x50485903,2,Array.Empty<byte>()));
                        stream.Write(request,0,request.Length);
                        var level=ReadPacket(stream);
                        if(level.MessageId!=0x50485903||level.Sequence!=2||level.IsPush||level.Kind!=PayloadKind.Protobuf)throw new InvalidDataException("Level handshake");
                        ReceiveLevel(level.Payload.ToArray());
                    }
                    else stream.Write(hello, 0, hello.Length);
                    sender = new Thread(() => SendInputs(stream)) { IsBackground = true, Name = "Roost Room Input" };
                    sender.Start();
                }
                else stream.Write(new byte[4], 0, 4); // 槽号 0 只观察。
                while (!stopping)
                {
                    var decoded = ReadPacket(stream);
                    if (ControlRoom && UseGate && decoded.MessageId == 0x50485902)
                    {
                        if (decoded.IsPush || decoded.Kind != PayloadKind.Protobuf)
                            throw new InvalidDataException("Gate input rejected");
                        using(var receipt=new BinaryReader(new MemoryStream(decoded.Payload.ToArray()))) {
                            if(receipt.ReadByte()!=8)throw new InvalidDataException("Input receipt generation");
                            ulong generation=Varint(receipt);
                            if(generation==0||receipt.BaseStream.Position!=receipt.BaseStream.Length)throw new InvalidDataException("Input receipt");
                            Interlocked.Exchange(ref inputGeneration,unchecked((long)generation));
                        }
                        continue;
                    }
                    byte[] payload = decoded.Payload.ToArray();
                    if (decoded.MessageId != 0x50485901 || decoded.Kind != PayloadKind.Sync || !decoded.IsPush)
                        throw new InvalidDataException("Unexpected crowd packet");
                    if (Interlocked.Increment(ref pending) > 128) throw new InvalidDataException("Observer queue full");
                    Interlocked.Add(ref bytes, payload.Length + PacketCodec.HeaderSize);
                    packets.Enqueue(payload);
                }
            }
            catch (Exception e) { if (!stopping) failure = e.Message; }
            finally { stopping = true; socket?.Close(); }
        }
        void Update()
        {
            try
            {
                if(ControlRoom&&UseGate&&!levelReady) {if(levelContent==null)return;BuildLevel();}
                if (ControlRoom) ReadControls();
                for (int i = 0; i < 8 && packets.TryDequeue(out var data); i++)
                {
                    Interlocked.Decrement(ref pending);
                    previous.Clear();
                    foreach (var pair in replica.Bodies) previous[pair.Key] = pair.Value;
                    ulong old = replica.Tick;
                    replica.Apply(data);
                    if (!ControlRoom && replica.Bodies.Count != 71) throw new InvalidDataException("Expected 71 bodies");
                    foreach (var id in replica.Bodies.Keys)
                        if (!bodies.ContainsKey(id)) throw new InvalidDataException("Unknown crowd body");
                    interval = Mathf.Max(.02f, (replica.Tick - old) * .02f);
                    receivedAt = Time.unscaledTime;
                    Frames++;
                    if(ControlRoom&&UseGate&&replica.Bodies.TryGetValue(controlledBody,out var self)&&self.InputGeneration!=0&&self.InputGeneration==unchecked((ulong)Interlocked.Read(ref inputGeneration))) {
                        predictionReady=true;
                        prediction.Reconcile(self.InputSequence,self.InputActive,new PredictedState{Position=N(self.Position),Velocity=N(self.Velocity)},clock.Elapsed.TotalSeconds);
                    }
                }
                if (ControlRoom)
                    foreach (var pair in bodies) pair.Value.gameObject.SetActive(staticBodies.Contains(pair.Key)||replica.Bodies.ContainsKey(pair.Key));
                float alpha = Interpolate ? Mathf.Clamp01((Time.unscaledTime - receivedAt) / interval) : 1;
                foreach (var pair in replica.Bodies)
                {
                    var target = bodies[pair.Key];
                    target.gameObject.SetActive(true);
                    var state = pair.Value;
                    if (previous.TryGetValue(pair.Key, out var old))
                    {
                        target.position = Vector3.Lerp(old.Position, state.Position, alpha);
                        target.rotation = Quaternion.Slerp(old.Rotation, state.Rotation, alpha);
                    }
                    else { target.position = state.Position; target.rotation = state.Rotation; }
                    if (ControlRoom && pair.Key == (UseGate?controlledBody:(ulong)(50 + PlayerSlot)))
                    {
                        if (!hasOrigin) { origin = state.Position; hasOrigin = true; startedAt = Time.unscaledTime; }
                        HorizontalTravel = Mathf.Max(HorizontalTravel, new Vector2(state.Position.x-origin.x,state.Position.z-origin.z).magnitude);
                        VerticalRise = Mathf.Max(VerticalRise, state.Position.y-origin.y);
                        roomCamera.transform.position = target.position + new Vector3(0, 12, -16);
                        roomCamera.transform.LookAt(target.position + Vector3.up);
                    }
                }
                // 先更新所有可见碰撞体，再预测自己，避免依赖字典对象遍历次序。
                if(ControlRoom&&UseGate&&PredictLocalInput&&predictionReady&&bodies.TryGetValue(controlledBody,out var own)) {
                    UnityEngine.Physics.SyncTransforms();
                    var predicted=prediction.Predict(clock.Elapsed.TotalSeconds,PredictStep);
                    PredictionOffsetMeters=Vector3.Distance(own.position,U(predicted.Position));
                    own.position=U(predicted.Position);
                    roomCamera.transform.position=own.position+new Vector3(0,12,-16);
                    roomCamera.transform.LookAt(own.position+Vector3.up);
                }
            }
            catch (Exception e) { failure = e.Message; stopping = true; socket?.Close(); }
        }
        void OnGUI()
        {
            if (ControlRoom)
            {
                GUI.Box(new Rect(12, 12, Screen.width-24, 85), "Roost Room Entity / server-authoritative physics");
                GUI.Label(new Rect(24, 38, Screen.width-48, 25), $"{Status} | player {PlayerSlot} | {Summary} | sync 20Hz");
                GUI.Label(new Rect(24, 62, Screen.width-48, 25), $"WASD / arrows: move   Space: jump  E: hold grab   travel={HorizontalTravel:F1}m rise={VerticalRise:F1}m");
                float y = Screen.height - 170;
                bool up=GUI.RepeatButton(new Rect(92,y,70,60), "Forward");
                bool left=GUI.RepeatButton(new Rect(16,y+66,70,60), "Left");
                bool down=GUI.RepeatButton(new Rect(92,y+66,70,60), "Back");
                bool right=GUI.RepeatButton(new Rect(168,y+66,70,60), "Right");
                uiX=(right?1:0)-(left?1:0); uiZ=(up?1:0)-(down?1:0);
                if (GUI.Button(new Rect(Screen.width-160,Screen.height-110,140,85), "JUMP")) QueueJump();
                return;
            }
            GUI.Box(new Rect(12, 12, Screen.width - 24, 80), "");
            GUI.Label(new Rect(24, 20, Screen.width - 48, 25), "20 clients / small balls + 50 orange NPC capsules + 1 yellow free ball | Server authoritative");
            GUI.Label(new Rect(24, 45, Screen.width - 48, 25), $"{Status}   {Summary}   Physics 50Hz; controls 20Hz/client; interpolated Chunk state");
        }
        void QueueJump()
        {
            lock (inputLock) jumpPending = true;
            JumpRequests++;
        }
        void ReadControls()
        {
            float x = uiX + (Input.GetKey(KeyCode.D)||Input.GetKey(KeyCode.RightArrow)?1:0) - (Input.GetKey(KeyCode.A)||Input.GetKey(KeyCode.LeftArrow)?1:0);
            float z = uiZ + (Input.GetKey(KeyCode.W)||Input.GetKey(KeyCode.UpArrow)?1:0) - (Input.GetKey(KeyCode.S)||Input.GetKey(KeyCode.DownArrow)?1:0);
            if (Input.GetKeyDown(KeyCode.Space)) QueueJump();
            if (VerifyControls && hasOrigin)
            {
                float elapsed=Time.unscaledTime-startedAt;
                x=elapsed<3?1:0; z=0;
                if (elapsed>1.5f&&!firstJump) {firstJump=true;QueueJump();}
                if (elapsed>7&&!secondJump) {secondJump=true;QueueJump();}
            }
            Vector2 direction=Vector2.ClampMagnitude(new Vector2(x,z),1)*10;
            lock (inputLock) {inputX=direction.x;inputZ=direction.y;grabTarget=UseGate&&Input.GetKey(KeyCode.E)?NearestGrab():0;}
        }
        static void PutUInt(byte[] data, int offset, uint value)
        {
            for(int i=0;i<4;i++) data[offset+i]=(byte)(value>>(24-i*8));
        }
        void SendInputs(Stream stream)
        {
            try
            {
                ulong sequence=0;
                while(!stopping)
                {
                    float x, z;
                    bool jump;
                    ulong grab;
                    lock (inputLock)
                    {
                        x = inputX;
                        z = inputZ;
                        jump = jumpPending;
                        grab=grabTarget;
                        jumpPending = false;
                    }
                    byte[] data = new byte[17];
                    sequence++;
                    PutUInt(data,0,(uint)(sequence>>32));PutUInt(data,4,(uint)sequence);
                    PutUInt(data,8,BitConverter.ToUInt32(BitConverter.GetBytes(x),0));
                    PutUInt(data,12,BitConverter.ToUInt32(BitConverter.GetBytes(z),0));data[16]=(byte)(jump?1:0);
                    if (UseGate) {
                        prediction.Record(new PredictedInput{Sequence=sequence,Acceleration=new System.Numerics.Vector3(x,0,z),Jump=jump,At=clock.Elapsed.TotalSeconds});
                        data = GateInput(sequence, x, z, jump,grab);
                    }
                    stream.Write(data,0,data.Length);
                    Thread.Sleep(50);
                }
            }
            catch (Exception e)
            {
                if (!stopping) failure = e.Message;
                stopping = true;
                socket?.Close();
            }
        }
        void OnDestroy()
        {
            stopping = true;
            socket?.Close();
            receiver?.Join(1000);
            sender?.Join(1000);
        }
    }
}
