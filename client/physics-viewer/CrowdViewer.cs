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
    // 旁观 20 个网络控制小球的权威状态。这里不另造一套本地输入，避免伪称同输入物理对照。
    public sealed class CrowdViewer : MonoBehaviour
    {
        public string Host = "127.0.0.1";
        public int Port = 19091;
        public bool Interpolate = true;
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
            var camera = new GameObject("Crowd Camera").AddComponent<Camera>();
            camera.transform.position = new Vector3(40, 43, -48);
            camera.transform.LookAt(new Vector3(0, 2, 0));
            camera.fieldOfView = 55;
            var light = new GameObject("Sun").AddComponent<Light>();
            light.type = LightType.Directional;
            light.transform.rotation = Quaternion.Euler(55, -30, 0);
            receiver = new Thread(ReadLoop) { IsBackground = true, Name = "Roost Crowd Observer" };
            receiver.Start();
        }
        static Transform Primitive(PrimitiveType type, string name, Color color)
        {
            var obj = GameObject.CreatePrimitive(type);
            obj.name = name;
            DestroyImmediate(obj.GetComponent<Collider>());
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
        void ReadLoop()
        {
            try
            {
                socket = new TcpClient();
                socket.Connect(Host, Port);
                socket.ReceiveTimeout = 5000;
                var stream = socket.GetStream();
                stream.Write(new byte[4], 0, 4); // 槽号 0 只观察，不占任何控制小球。
                while (!stopping)
                {
                    byte[] header = ReadExactly(stream, 16);
                    uint length = (uint)header[12] << 24 | (uint)header[13] << 16 | (uint)header[14] << 8 | header[15];
                    if (length > 1048576) throw new InvalidDataException("Packet limit");
                    byte[] payload = ReadExactly(stream, (int)length);
                    byte[] packet = new byte[16 + payload.Length];
                    Buffer.BlockCopy(header, 0, packet, 0, 16);
                    Buffer.BlockCopy(payload, 0, packet, 16, payload.Length);
                    var decoded = PacketCodec.Decode(packet);
                    if (decoded.MessageId != 0x50485901 || decoded.Kind != PayloadKind.Sync || !decoded.IsPush)
                        throw new InvalidDataException("Unexpected crowd packet");
                    if (Interlocked.Increment(ref pending) > 128) throw new InvalidDataException("Observer queue full");
                    Interlocked.Add(ref bytes, packet.Length);
                    packets.Enqueue(payload);
                }
            }
            catch (Exception e) { if (!stopping) failure = e.Message; }
            finally { socket?.Close(); }
        }
        void Update()
        {
            try
            {
                for (int i = 0; i < 8 && packets.TryDequeue(out var data); i++)
                {
                    Interlocked.Decrement(ref pending);
                    previous.Clear();
                    foreach (var pair in replica.Bodies) previous[pair.Key] = pair.Value;
                    ulong old = replica.Tick;
                    replica.Apply(data);
                    if (replica.Bodies.Count != 71) throw new InvalidDataException("Expected 71 bodies");
                    foreach (var id in replica.Bodies.Keys)
                        if (!bodies.ContainsKey(id)) throw new InvalidDataException("Unknown crowd body");
                    interval = Mathf.Max(.02f, (replica.Tick - old) * .02f);
                    receivedAt = Time.unscaledTime;
                    Frames++;
                }
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
                }
            }
            catch (Exception e) { failure = e.Message; stopping = true; socket?.Close(); }
        }
        void OnGUI()
        {
            GUI.Box(new Rect(12, 12, Screen.width - 24, 80), "");
            GUI.Label(new Rect(24, 20, Screen.width - 48, 25), "20 clients / small balls + 50 orange NPC capsules + 1 yellow free ball | Server authoritative");
            GUI.Label(new Rect(24, 45, Screen.width - 48, 25), $"{Status}   {Summary}   Physics 50Hz; controls 20Hz/client; interpolated Chunk state");
        }
        void OnDestroy()
        {
            stopping = true;
            socket?.Close();
            receiver?.Join(1000);
        }
    }
}
