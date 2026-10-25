using System;
using System.Collections.Concurrent;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Net.Sockets;
using System.Threading;
using UnityEngine;
using UnityEngine.SceneManagement;
using Roost.Client;
using Debug = UnityEngine.Debug;

namespace Roost.PhysicsViewer
{
    // 对照工程不拥有物理业务规则；输入带与服务器 scene v1 对齐。
    public sealed class PhysicsComparison : MonoBehaviour
    {
        public string Host = "127.0.0.1";
        public int Port = 19091;
        public bool Interpolate = true;
        public bool TraceContacts;
        readonly ConcurrentQueue<byte[]> packets = new ConcurrentQueue<byte[]>();
        readonly Dictionary<ulong, Transform> ghosts = new Dictionary<ulong, Transform>();
        readonly Dictionary<ulong, Rigidbody> local = new Dictionary<ulong, Rigidbody>();
        readonly Dictionary<ulong, BodyState> previous = new Dictionary<ulong, BodyState>();
        readonly ChunkReplica replica = new ChunkReplica();
        readonly SortedDictionary<ulong, Dictionary<ulong, BodyState>> localHistory =
            new SortedDictionary<ulong, Dictionary<ulong, BodyState>>();
        readonly Queue<(ulong tick, Dictionary<ulong, BodyState> states)> comparisons =
            new Queue<(ulong, Dictionary<ulong, BodyState>)>();
        float anchorTime;
        ulong anchorTick;
        Scene localScene;
        PhysicsScene localPhysics;
        TcpClient socket;
        Thread receiver;
        volatile bool stopping;
        volatile string failure;
        long receivedBytes;
        int pending;
        ulong localTick;
        float receivedAt, interval = .05f;
        double meanError, maxError, ballError, rotationError, velocityError, localMilliseconds;
        Vector3 serverBall;
        ulong samples;
        ulong comparedTick;
        StreamWriter metrics;
        public bool HasState => replica.Sequence != 0;
        public ulong ComparedFrames => samples;
        public ulong ObservedTick => replica.Tick;
        public ulong ComparedTick => comparedTick;
        public void FlushMetrics()
        {
            metrics?.Flush();
        }
        public string VerificationSummary => FormattableString.Invariant(
            $"frames={samples} compared_tick={comparedTick} received_tick={replica.Tick} mean_error_m={meanError:F6} max_error_m={maxError:F6} ball_error_m={ballError:F6} ball_position={serverBall} max_rotation_deg={rotationError:F6}");
        public string Status => failure ?? (HasState ? "Connected" : "Connecting");

        void Start()
        {
            Application.runInBackground = true;
            Setup();
            string dir = Path.Combine(Application.persistentDataPath, "PhysicsComparison");
            Directory.CreateDirectory(dir);
            metrics = new StreamWriter(Path.Combine(dir, DateTime.UtcNow.ToString("yyyyMMdd-HHmmss") + ".csv"));
            metrics.WriteLine(
                "tick,mean_position_error_m,max_position_error_m,ball_error_m,max_rotation_error_degrees," +
                "max_velocity_error_mps,ball_x,ball_y,ball_z,local_step_ms,total_received_bytes");
            receiver = new Thread(ReadLoop) { IsBackground = true, Name = "Roost Physics Receiver" };
            receiver.Start();
        }
        public void Setup()
        {
            localScene = SceneManager.CreateScene("LocalPhysX-" + GetInstanceID(),
                                                  new CreateSceneParameters(LocalPhysicsMode.Physics3D));
            localPhysics = localScene.GetPhysicsScene();
            Physics.gravity = new Vector3(0, -9.81f, 0);
            var material = new PhysicsMaterial("Shared scene v1") { dynamicFriction = .6f, staticFriction = .6f,
                                                                    bounciness = .1f };
            BuildArena(false, material);
            BuildArena(true, material);
            // 与服务器保持相同创建顺序：静态场景 → 球 → 胶囊。
            // 输入由 ID 和 tick 唯一决定，无随机采样，不依赖进程启动时间。
            for (int order = 0; order < 21; order++)
            {
                ulong id = order == 0 ? 21UL : (ulong)order;
                bool ball = id == 21;
                var remote = GameObject.CreatePrimitive(ball ? PrimitiveType.Sphere : PrimitiveType.Capsule);
                remote.name = "Server " + id;
                remote.layer = 8;
                remote.transform.localScale = ball ? Vector3.one * 4 : new Vector3(.8f, 1, .8f);
                DestroyImmediate(remote.GetComponent<Collider>());
                ghosts[id] = remote.transform;
                Colorize(remote, id);
                var bodyObject = new GameObject("Local " + id);
                SceneManager.MoveGameObjectToScene(bodyObject, localScene);
                bodyObject.layer = 9;
                Collider collider;
                if (ball)
                {
                    var c = bodyObject.AddComponent<SphereCollider>();
                    c.radius = 2;
                    collider = c;
                }
                else
                {
                    var c = bodyObject.AddComponent<CapsuleCollider>();
                    c.radius = .4f;
                    c.height = 2;
                    c.direction = 1;
                    collider = c;
                }
                collider.sharedMaterial = material;
                collider.contactOffset = .02f;
                var visual = GameObject.CreatePrimitive(ball ? PrimitiveType.Sphere : PrimitiveType.Capsule);
                DestroyImmediate(visual.GetComponent<Collider>());
                visual.transform.SetParent(bodyObject.transform, false);
                visual.layer = 9;
                visual.transform.localScale = remote.transform.localScale;
                Colorize(visual, id);
                var body = bodyObject.AddComponent<Rigidbody>();
                body.mass = ball ? 80 : 10;
                body.linearDamping = 0;
                body.angularDamping = .05f;
                body.maxAngularVelocity = 50;
                body.maxDepenetrationVelocity = 10;
                body.sleepThreshold = .005f;
                body.solverIterations = 6;
                body.solverVelocityIterations = 1;
                body.collisionDetectionMode = CollisionDetectionMode.ContinuousDynamic;
                if (!ball)
                    body.constraints = RigidbodyConstraints.FreezeRotation;
                body.position = ball ? new Vector3(0, 2.1f, 0) : Spawn((int)id - 1);
                local[id] = body;
                remote.transform.position = body.position;
            }
            CameraView("Server state", 8, new Rect(0, 0, .5f, 1));
            CameraView("Local Unity PhysX", 9, new Rect(.5f, 0, .5f, 1));
            var light = new GameObject("Sun").AddComponent<Light>();
            light.type = LightType.Directional;
            light.transform.rotation = Quaternion.Euler(55, -30, 0);
        }
        static Vector3 Spawn(int i)
        {
            double a = i * 2 * Math.PI / 20;
            return new Vector3((float)(12 * Math.Cos(a)), 1.1f, (float)(12 * Math.Sin(a)));
        }
        static Vector3 InputAt(int i, ulong tick)
        {
            var p = Spawn(i);
            float x = -p.x / 12, z = -p.z / 12;
            switch (((tick - 1) / 150) % 4)
            {
            case 1:
                (x, z) = (-z, x);
                break;
            case 2:
                (x, z) = (-x, -z);
                break;
            case 3:
                (x, z) = (z, -x);
                break;
            }
            float gain = 8 + i % 3 * 2;
            return new Vector3(x * gain, 0, z * gain);
        }
        void BuildArena(bool simulated, PhysicsMaterial material)
        {
            Vector3[] positions = { new Vector3(0, -.5f, 0), new Vector3(-25, 2, 0), new Vector3(25, 2, 0),
                                    new Vector3(0, 2, -25), new Vector3(0, 2, 25) };
            Vector3[] sizes = { new Vector3(50, 1, 50), new Vector3(1, 4, 50), new Vector3(1, 4, 50),
                                new Vector3(50, 4, 1), new Vector3(50, 4, 1) };
            for (int i = 0; i < positions.Length; i++)
            {
                var o = GameObject.CreatePrimitive(PrimitiveType.Cube);
                o.name = "Arena " + i;
                o.layer = simulated ? 9 : 8;
                if (simulated)
                    SceneManager.MoveGameObjectToScene(o, localScene);
                o.transform.position = positions[i];
                o.transform.localScale = sizes[i];
                o.GetComponent<Collider>().sharedMaterial = material;
                o.GetComponent<Collider>().contactOffset = .02f;
                if (!simulated)
                    DestroyImmediate(o.GetComponent<Collider>());
                o.GetComponent<Renderer>().sharedMaterial =
                    new Material(Shader.Find("Standard")) { color = new Color(.22f, .26f, .3f) };
            }
            foreach (bool axis in new[] { false, true })
            {
                var line = new GameObject("Chunk boundary");
                line.layer = simulated ? 9 : 8;
                if (simulated)
                    SceneManager.MoveGameObjectToScene(line, localScene);
                var lr = line.AddComponent<LineRenderer>();
                lr.sharedMaterial = new Material(Shader.Find("Sprites/Default"));
                lr.startColor = lr.endColor = Color.cyan;
                lr.startWidth = lr.endWidth = .08f;
                lr.positionCount = 2;
                lr.SetPosition(0, axis ? new Vector3(-25, .02f, 0) : new Vector3(0, .02f, -25));
                lr.SetPosition(1, axis ? new Vector3(25, .02f, 0) : new Vector3(0, .02f, 25));
            }
        }
        static void Colorize(GameObject o, ulong id)
        {
            var material = new Material(
                Shader.Find("Standard")) { color = id == 21 ? Color.white : Color.HSVToRGB((id - 1) / 20f, .7f, .9f) };
            if (id == 21)
            {
                var texture = new Texture2D(64, 32);
                for (int y = 0; y < 32; y++)
                    for (int x = 0; x < 64; x++)
                        texture.SetPixel(x, y, ((x / 8 + y / 8) % 2) == 0 ? Color.yellow : new Color(.25f, .17f, .02f));
                texture.Apply();
                material.mainTexture = texture;
            }
            o.GetComponent<Renderer>().sharedMaterial = material;
        }
        static void CameraView(string name, int layer, Rect rect)
        {
            var c = new GameObject(name).AddComponent<Camera>();
            c.rect = rect;
            c.cullingMask = 1 << layer;
            c.transform.position = new Vector3(0, 52, -38);
            c.transform.LookAt(Vector3.zero);
            c.fieldOfView = 55;
            c.backgroundColor = new Color(.08f, .1f, .13f);
        }
        void ReadLoop()
        {
            try
            {
                socket = new TcpClient();
                socket.Connect(Host, Port);
                socket.ReceiveTimeout = 5000;
                var stream = socket.GetStream();
                while (!stopping)
                {
                    byte[] header = ReadExactly(stream, 16);
                    uint length = Big(header, 12);
                    if (length > 1048576)
                        throw new InvalidDataException("Packet too large");
                    var payload = ReadExactly(stream, (int)length);
                    var data = new byte[16 + payload.Length];
                    Buffer.BlockCopy(header, 0, data, 0, 16);
                    Buffer.BlockCopy(payload, 0, data, 16, payload.Length);
                    var packet = PacketCodec.Decode(data);
                    if (packet.MessageId != 0x50485901 || packet.Kind != PayloadKind.Sync || !packet.IsPush)
                        throw new InvalidDataException("Unexpected physics packet");
                    if (Interlocked.Increment(ref pending) > 128)
                        throw new InvalidDataException("Viewer queue full: reconnect");
                    Interlocked.Add(ref receivedBytes, data.Length);
                    packets.Enqueue(payload);
                }
            }
            catch (Exception e)
            {
                if (!stopping)
                    failure = e.Message;
            }
            finally
            {
                socket?.Close();
            }
        }
        static uint Big(byte[] a, int i) => (uint)a[i] << 24 | (uint)a[i + 1] << 16 | (uint)a[i + 2] << 8 | a[i + 3];
        static byte[] ReadExactly(Stream s, int size)
        {
            var b = new byte[size];
            int n = 0;
            while (n < size)
            {
                int r = s.Read(b, n, size - n);
                if (r == 0)
                    throw new EndOfStreamException();
                n += r;
            }
            return b;
        }
        void Update()
        {
            for (int i = 0; i < 8 && packets.TryDequeue(out var payload); i++)
            {
                Interlocked.Decrement(ref pending);
                try
                {
                    Consume(payload);
                }
                catch (Exception e)
                {
                    failure = e.Message;
                    stopping = true;
                    socket?.Close();
                    break;
                }
            }
            if (HasState && failure == null)
            {
                ulong target = anchorTick + (ulong)Math.Max(0, Math.Floor((Time.unscaledTime - anchorTime) / .02));
                try
                {
                    AdvanceLocal(target);
                    CompareReady();
                }
                catch (Exception e)
                {
                    failure = e.Message;
                    stopping = true;
                    socket?.Close();
                }
            }
            float a = Interpolate ? Mathf.Clamp01((Time.unscaledTime - receivedAt) / interval) : 1;
            foreach (var item in replica.Bodies)
            {
                if (!ghosts.TryGetValue(item.Key, out var ghost))
                    continue;
                var next = item.Value;
                if (previous.TryGetValue(item.Key, out var old))
                {
                    ghost.position = Vector3.Lerp(old.Position, next.Position, a);
                    ghost.rotation = Quaternion.Slerp(old.Rotation, next.Rotation, a);
                }
                else
                {
                    ghost.position = next.Position;
                    ghost.rotation = next.Rotation;
                }
            }
        }
        public void Consume(byte[] payload)
        {
            previous.Clear();
            foreach (var kv in replica.Bodies)
                previous[kv.Key] = kv.Value;
            ulong oldTick = replica.Tick;
            bool full = replica.Apply(payload);
            interval = Mathf.Max(.02f, (replica.Tick - oldTick) * .02f);
            receivedAt = Time.unscaledTime;
            if (replica.Bodies.Count != 21)
                throw new InvalidDataException("Scene v1 expects 21 bodies");
            foreach (var kv in replica.Bodies)
                if (!local.ContainsKey(kv.Key))
                    throw new InvalidDataException("Unknown scene body");
            if (full)
            {
                foreach (var kv in replica.Bodies)
                {
                    var b = local[kv.Key];
                    var state = kv.Value;
                    b.position = state.Position;
                    b.rotation = state.Rotation;
                    b.linearVelocity = state.Velocity;
                    b.angularVelocity = state.AngularVelocity;
                    if (state.Sleeping)
                        b.Sleep();
                    else
                        b.WakeUp();
                }
                localTick = replica.Tick;
                anchorTick = localTick;
                anchorTime = Time.unscaledTime;
                localHistory.Clear();
                comparisons.Clear();
                Physics.SyncTransforms();
                SaveLocal();
                return;
            }
            if (comparisons.Count >= 128)
                throw new InvalidDataException("Comparison queue exceeded");
            comparisons.Enqueue((replica.Tick, new Dictionary<ulong, BodyState>(replica.Bodies)));
        }
        void SaveLocal()
        {
            var snapshot = new Dictionary<ulong, BodyState>();
            foreach (var kv in local)
            {
                var b = kv.Value;
                snapshot[kv.Key] = new BodyState { Id = kv.Key, Position = b.position, Rotation = b.rotation,
                                                   Velocity = b.linearVelocity, AngularVelocity = b.angularVelocity };
            }
            localHistory[localTick] = snapshot;
            while (localHistory.Count > 512)
            {
                using var it = localHistory.GetEnumerator();
                it.MoveNext();
                localHistory.Remove(it.Current.Key);
            }
        }
        void AdvanceLocal(ulong target)
        {
            if (target < localTick)
                return;
            if (target - localTick > 250)
                throw new InvalidDataException("Local simulation backlog: reconnect required");
            var timer = Stopwatch.StartNew();
            while (localTick < target)
            {
                localTick++;
                for (int i = 0; i < 20; i++)
                    local[(ulong)i + 1].AddForce(InputAt(i, localTick), ForceMode.Acceleration);
                localPhysics.Simulate(.02f);
                SaveLocal();
            }
            timer.Stop();
            localMilliseconds = timer.Elapsed.TotalMilliseconds;
        }
        void CompareReady()
        {
            while (comparisons.Count > 0 && comparisons.Peek().tick <= localTick)
            {
                var frame = comparisons.Dequeue();
                if (!localHistory.TryGetValue(frame.tick, out var history))
                    throw new InvalidDataException("Same-tick history expired");
                double sum = 0, max = 0;
                rotationError = 0;
                velocityError = 0;
                foreach (var kv in frame.states)
                {
                    var localState = history[kv.Key];
                    double d = Vector3.Distance(kv.Value.Position, localState.Position);
                    if (TraceContacts && kv.Key == 1 && frame.tick <= 12)
                        Debug.Log(FormattableString.Invariant($"ROOST_CONTACT tick={frame.tick} server_p={kv.Value.Position:F6} local_p={localState.Position:F6} server_v={kv.Value.Velocity:F6} local_v={localState.Velocity:F6}"));
                    sum += d;
                    max = Math.Max(max, d);
                    rotationError = Math.Max(rotationError, Quaternion.Angle(kv.Value.Rotation, localState.Rotation));
                    velocityError = Math.Max(velocityError, Vector3.Distance(kv.Value.Velocity, localState.Velocity));
                    if (kv.Key == 21)
                    {
                        ballError = d;
                        serverBall = kv.Value.Position;
                    }
                }
                meanError = sum / 21;
                maxError = max;
                samples++;
                comparedTick = frame.tick;
                metrics?.WriteLine(FormattableString.Invariant(
                    $"{frame.tick},{meanError:F6},{maxError:F6},{ballError:F6},{rotationError:F6},{velocityError:F6},{serverBall.x:F6},{serverBall.y:F6},{serverBall.z:F6},{localMilliseconds:F6},{Interlocked.Read(ref receivedBytes)}"));
                if (samples % 100 == 0)
                    metrics?.Flush();
            }
        }
        void OnGUI()
        {
            GUI.Box(new Rect(12, 12, Screen.width - 24, 110), "");
            GUI.Label(
                new Rect(24, 20, Screen.width - 48, 25),
                "LEFT: Roost native PhysX → Chunk sync        |        RIGHT: Unity local PhysX (same input tick)");
            GUI.Label(
                new Rect(24, 45, Screen.width - 48, 25),
                $"{Status}   Received/compared tick {replica.Tick}/{comparedTick}   Received {Interlocked.Read(ref receivedBytes)/1024.0:F1} KiB   Mean/max position error {meanError:F3}/{maxError:F3} m   Ball {ballError:F3} m");
            GUI.Label(
                new Rect(24, 70, Screen.width - 48, 25),
                $"Rotation error {rotationError:F2}°; local catch-up {localMilliseconds:F3} ms   Render interpolation adds up to one received interval; errors are compared at the SAME tick.");
            Interpolate = GUI.Toggle(new Rect(24, 95, 350, 25), Interpolate, "Interpolate server display");
        }
        void OnDestroy()
        {
            stopping = true;
            socket?.Close();
            receiver?.Join(1000);
            metrics?.Dispose();
            if (localScene.IsValid())
                SceneManager.UnloadSceneAsync(localScene);
        }
    }
}
