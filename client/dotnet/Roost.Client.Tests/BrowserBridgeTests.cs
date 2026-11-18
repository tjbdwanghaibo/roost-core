using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Text;
using System.Threading.Tasks;
using Roost.Client;
using Roost.GateSmoke;

// 浏览器桥接的 C# 部分（unity-webgl-smoke 的 BrowserGateTransport，正式源码链接进来）在 PumpDriven 单线程环境里拼帧：
// 假的浏览器一侧（FakeBrowserSocket，按 jslib 的做法把收到的消息排成字节队列、按调用方给的长度取）喂三种消息——
// 一帧拆成多条消息、多帧合成一条消息、接近 max_payload 的大帧——断言 SDK 收到的应答与推送完整、顺序不乱，
// 每次 Send 恰好一帧，全程零违规。jslib 本身的 JS 逻辑由 check-jslib-frames.mjs 在 node 里核对。
internal static partial class Program
{
    private static async Task TestBrowserBridgeFraming()
    {
        var fake = new FakeBrowserSocket();
        await StrictLoop.RunAsync("browser-bridge", TimeSpan.FromSeconds(30), async loop =>
        {
            loop.BeforePump.Add(BrowserGateTransport.PollAll);
            var client = loop.Client(new ConnectionOptions
            {
                WebSocketUrl = "ws://browser.test/ws", PumpDriven = true, HeartbeatInterval = TimeSpan.FromMilliseconds(100),
                TransportFactory = () => new BrowserGateTransport(fake),
            });
            var pushes = new List<string>();
            var replies = new Queue<byte[]>();
            var failures = new Queue<Exception>();
            client.Subscribe(9, bytes => Utf8.GetString(bytes), pushes.Add);
            async Task<byte[]> Call(string text)
            {
                client.Request(42, 43, Utf8.GetBytes(text), bytes => bytes, replies.Enqueue, failures.Enqueue);
                await loop.Until("reply to " + text, () => replies.Count > 0 || failures.Count > 0);
                if (failures.Count > 0) throw new Exception($"request '{text}' failed", failures.Dequeue());
                return replies.Dequeue();
            }

            await client.Connection.ConnectAsync("bridge"); // 握手应答拆成三条消息
            Check(Utf8.GetString(await Call("split")) == FakeBrowserSocket.SplitReply, "a frame split into 7-byte messages arrives whole");
            Check(Utf8.GetString(await Call("merged")) == "merged-reply", "the first of three frames merged in one message");
            await loop.Until("both merged pushes", () => pushes.Count >= 2);
            Check(pushes.SequenceEqual(new[] { "m1", "m2" }), "merged pushes arrive whole and in order: " + string.Join(",", pushes));
            var big = await Call("big");
            Check(big.AsSpan().SequenceEqual(FakeBrowserSocket.BigPayload), $"a {big.Length}-byte frame near max_payload arrives whole");
            Check(Utf8.GetString(await Call("after")) == "after", "frames after the big one stay aligned");
            await loop.Until("a heartbeat round trip", () => client.Connection.HeartbeatRoundTrips > 0);
            Check(fake.Errors.Count == 0, "every Send was exactly one frame: " + string.Join(" | ", fake.Errors));
            Check(loop.Violations.IsEmpty, "everything ran on the loop thread: " + string.Join(" | ", loop.Violations));
        }, expectViolations: false);
        Console.WriteLine("PASS browser bridge (BrowserGateTransport + fake jslib, PumpDriven): split / merged / near-max frames, one frame per Send");
    }

    /// <summary>假的浏览器一侧：jslib 的 RoostWs* 同样的语义（字节队列、按长度取），按收到的帧回预先编排好的消息。</summary>
    private sealed class FakeBrowserSocket : BrowserGateTransport.IBrowserSocket
    {
        internal static readonly string SplitReply = string.Concat(Enumerable.Range(0, 300).Select(i => (char)('a' + i % 26)));
        // 缺省 MaxPayloadBytes 是 1 MiB：差 3 字节到上限，跨过桥接 64 KiB 的中转缓冲很多次。
        internal static readonly byte[] BigPayload = Enumerable.Range(0, PacketCodec.DefaultMaxPayload - 3).Select(i => (byte)(i * 31 + 7)).ToArray();
        public readonly List<string> Errors = new List<string>();
        private readonly Queue<byte[]> chunks = new Queue<byte[]>();
        private int head, state = BrowserGateTransport.Closed, polls;
        private bool authed;

        public int Open(string url) { state = BrowserGateTransport.Connecting; polls = 0; authed = false; chunks.Clear(); head = 0; return 1; }
        public int State(int id)
        {
            if (state == BrowserGateTransport.Connecting && ++polls > 2) state = BrowserGateTransport.Open; // 过两轮才连上
            return state;
        }
        public int Buffered(int id) => 0;
        public void Close(int id) => state = BrowserGateTransport.Closed;

        public int Send(int id, byte[] data, int length)
        {
            if (state != BrowserGateTransport.Open) return 0;
            Packet packet;
            try { packet = PacketCodec.Decode(data.AsSpan(0, length).ToArray()); }
            catch (InvalidDataException error) { Errors.Add($"a {length}-byte send is not exactly one frame: {error.Message}"); return 1; }
            byte[] Frame(uint message, byte[] payload, bool push = false) => PacketCodec.Encode(new Packet(message, push ? 1u : packet.Sequence, payload, isPush: push));
            if (packet.MessageId == 0)
            {
                byte[] ack = Frame(0, Array.Empty<byte>());
                if (!authed) { authed = true; chunks.Enqueue(ack[..5]); chunks.Enqueue(ack[5..6]); chunks.Enqueue(ack[6..]); }
                else chunks.Enqueue(ack);
                return 1;
            }
            string text = Utf8.GetString(packet.Payload.Span);
            switch (text)
            {
                case "split":
                    var reply = Frame(43, Utf8.GetBytes(SplitReply));
                    for (int at = 0; at < reply.Length; at += 7) chunks.Enqueue(reply[at..Math.Min(reply.Length, at + 7)]);
                    break;
                case "merged":
                    chunks.Enqueue(Frame(43, Utf8.GetBytes("merged-reply")).Concat(Frame(9, Utf8.GetBytes("m1"), true)).Concat(Frame(9, Utf8.GetBytes("m2"), true)).ToArray());
                    break;
                case "big":
                    chunks.Enqueue(Frame(43, BigPayload));
                    break;
                default:
                    chunks.Enqueue(Frame(43, packet.Payload.ToArray()));
                    break;
            }
            return 1;
        }

        // 与 jslib 的 RoostWsReceive 相同：从队首消息接着上次的位置拷，至多 length 个，可以跨消息。
        public int Receive(int id, byte[] buffer, int length)
        {
            int copied = 0;
            while (copied < length && chunks.Count > 0)
            {
                var chunk = chunks.Peek();
                int take = Math.Min(length - copied, chunk.Length - head);
                Buffer.BlockCopy(chunk, head, buffer, copied, take);
                copied += take; head += take;
                if (head == chunk.Length) { chunks.Dequeue(); head = 0; }
            }
            return copied;
        }
    }
}
