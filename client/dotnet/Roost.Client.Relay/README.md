# Roost.Client.Relay：开房间场景的 C# SDK

lobby 的 HTTP 调用（`LobbyClient`）与 relay 的 TCP / WebSocket 连接（`RelayClient`）。`netstandard2.1`、可空引用、警告即错误；与 `Roost.Client` 一样不带第三方依赖（lobby 的 JSON 用内置的最小读写）。三份 SDK 的共同说明见 [client/README](../../README.md#开房间-sdklobby--relay)。

```csharp
var lobby = new LobbyClient("https://lobby.example.com", gt2);
var ticket = await lobby.CreateRoomAsync(new CreateRoomRequest { Game = "party", Mode = "duel" }); // 成员用 JoinRoomAsync
var client = new RelayClient(lobby, new RelayClientOptions { Game = "party", ReconnectAttempts = 5 });
client.GameFrame += frame => { /* 房主：frame.Slot 是成员槽位；成员：来自房主 */ };
client.RoomFrame += frame => { };
client.MemberJoined += member => { };            // 只有房主收到
client.Disconnected += d => { };                 // d.Reconnecting 为 false 是最后一条
var welcome = await client.ConnectAsync(ticket); // relay 拒绝时 RelayClosedException
// 引擎主线程每帧：client.Pump();
client.SendGame(RelayCodec.All, 101, payload);   // 房主：全体或某个槽位；成员：只到房主
client.SendRoom(3, 7, chat);
client.Kick(3); client.Mute(3, true);            // 只有房主，否则 InvalidOperationException
var left = await client.LeaveAsync();            // relay 上发 Leave、再经 lobby 离开、Dispose；left.RoomGone：房间早就不在；只断开用 Dispose

// 换 host（方案 §4.6）：回调都经 Pump
var options = new RelayClientOptions { Game = "party", SnapshotState = () => game.HostOnlyState() }; // 当房主时每 3s 推一次
client.OnYouAreHost(e => { game.TakeOver(e.Source, e.State, e.Members); client.HostReady(e.Epoch); }); // 或 AutoHostReady = true
client.OnHostChanging(epoch => { });             // 发给房主的帧被丢了；SDK 不重发，游戏决定
client.OnHostChanged(e => { });                  // 全体：新房主就绪
client.OnHostResumed(e => { });                  // 成员：房主在宽限期内回来了
client.OnYouAreMember(epoch => { });             // 被换下的旧房主、超时的候选
client.OnRejected(r => { });                     // 快照 / 转交被拒
client.Transfer(3, handoff); client.SetSuccessors(new uint[] { 3, 2 }); client.Snapshot(state); // 只有房主
var role = client.Role;                          // Slot、IsHost、IsCandidate、HostSlot、Epoch
client.SetForeground(false);                     // 成员：切到后台（Unity 在 OnApplicationPause 里调）；换 host、重连后 SDK 自动重发
client.OnMemberPresence(p => { });               // 房主：成员的前台状态；要不要据此 SetSuccessors 由游戏决定
```

- **线程**：网络在后台收发；全部通知只由 `Pump` 在调用它的线程派发（与 `GameClient` 相同），回调抛错交给 `Error`。Unity WebGL 上改用单线程调度（`PumpDriven`，见下“Unity WebGL”）。`Send*` 可以从任意线程调用，没有连接或写失败返回 false。积压超过 `MaxPendingEvents` 时断开连接。`Dispose` 之后不再通知。
- **lobby 错误**：`LobbyException`（`Status`、`ErrorName`、`Code`、`IsTemporary`）；被踢的账号再 join 是 403、`Code == LobbyErrorCode.Kicked`。
- **离开**：`LeaveAsync` 返回 `RelayLeaveResult`。lobby 答 `RoomNotFound`（房间已关或过了寿命，例如自己是最后一个人、relay 收到 `Leave` 随即关了房）算离开成功，不抛，`RoomGone` 为 true；正常让出席位时为 false（维护者 2026-11-18，三份 SDK 一致）；其余 lobby 错误照常抛。直接调 `LobbyClient.LeaveRoomAsync` 的仍抛 `RoomNotFound`。
- **心跳、重连、扩展点**：与 Go 版相同（成员 10s Ping、房主与候选 `HostPingInterval` 600ms、30s 读超时；`RejoinAsync` / `ReconnectAttempts` 先调 lobby 的 join 拿新令牌，开局后掉线也能回原座；遇到 `LobbyRejected` 停、`LobbyUnavailable` 按退避再试；`OnControl`、`SendControl`）。读超时由心跳任务检查，不依赖 `ReadAsync` 的取消（Unity Mono 上不一定生效）。
- **换 host**：角色规则（Welcome 带房间此刻的 epoch，`Role.Epoch` 进房就对）、快照 seq 接续（不低于 `epoch << 32`）、压缩（`RelayCompressors.Deflate` 缺省，与 Go 的 `relay.Deflate` 互通；实现 `IRelayCompressor` 可换 LZ4 / zstd，见下）、压缩方式标识（`[Id 1B][压缩后的字节]`，内置的 None / Deflate 谁都解得开，不认识的标识 `StateError` 是 `RelayUnknownCompressionException`，标识对、内容坏是 `InvalidDataException`）、前台上报（`SetForeground` / `OnMemberPresence`，收到 HostChanged 与每次连上后自动重发，C# SDK 不自动排候选顺序）、收帧上限、被踢（房主可踢离线座位，收到第二条 `MemberLeft{Kicked}`）都与 [Go 版](../../relay/README.md) 相同。差别：定时快照在 `Pump` 里向游戏要状态（`SnapshotState`），`AutoHostReady` 在 `OnYouAreHost` 回调（Pump 里）返回后回；`Snapshot` 返回 `Sent` / `TooSoon` / `NotConnected` / `Stale`（压缩期间换了连接或房主任期，或者比它晚发起的一份已经发出，没发、不占 seq；定时快照在向游戏要状态之前记下任期；可以从几个线程并发调 `Snapshot`，以发起顺序为准、后发起的胜出，见 RELAY.md §4A“快照”），不是房主或压缩后太大抛异常；`SetForeground` 没有连接时返回 false（状态仍记下）。
- **接入自己的 zstd / LZ4**：SDK 不带第三方依赖（维护者 2026-11-17 定）。游戏自带库（Unity 下能不能用由游戏自己在 Unity 里验），实现 `IRelayCompressor`、`Id` 取 2～255，房间里每个客户端都配上同一个。下面的代码原样在 `Roost.Client.Relay.Tests` 里编译并走一遍（`IZstdCodec` 是代表库的接口桩）：

  ```csharp
  /// <summary>代表游戏自带的 zstd 库（例如 ZstdSharp 的 Compressor / Decompressor）；SDK 不带它。</summary>
  public interface IZstdCodec
  {
      byte[] Wrap(ReadOnlySpan<byte> data);
      /// <summary>解压，至多解出 maxOutput 字节（库自己能限就在库里限）。</summary>
      byte[] Unwrap(ReadOnlySpan<byte> data, int maxOutput);
  }

  /// <summary>把它接成 IRelayCompressor。Id 取 2：0、1 归内置的 None 与 Deflate，LZ4 照样接、取 3。</summary>
  public sealed class ZstdRelayCompressor : IRelayCompressor
  {
      private readonly IZstdCodec codec;
      public ZstdRelayCompressor(IZstdCodec codec) { this.codec = codec; }
      public byte Id => 2;
      public byte[] Compress(ReadOnlySpan<byte> data) => codec.Wrap(data);
      public byte[] Decompress(ReadOnlySpan<byte> data, int limit)
      {
          var output = codec.Unwrap(data, limit + 1);
          if (output.Length > limit) throw new InvalidDataException($"Inflated relay state exceeds {limit} bytes.");
          return output;
      }
  }

  var client = new RelayClient(lobby, new RelayClientOptions { Game = "party", Compressor = new ZstdRelayCompressor(codec) });
  ```

  要不要换：deflate 与 zstd 在真实状态上的耗时与大小差距没有测过，先量自己的状态再决定。
- **传输：TCP 或 WebSocket**（交接 C1，维护者 2026-11-17 定的选项 A）。连接抽成公开接口 `IRelayTransport`（`ConnectAsync`、`Send` 一整帧、`ReceiveAsync` 收字节、`Dispose` 关闭；约定见接口注释）。`RelayClientOptions.Transport`（缺省 `Tcp`）决定向 lobby 要哪种地址，断线重连沿用同一种；`ConnectAsync` 的票据要用同一个 `Transport` 向 lobby 要（`CreateRoomRequest` / `JoinRoomRequest` 的 `Transport`），地址对不上时抛 `ArgumentException`。SDK 自带两种实现：`TcpRelayTransport`（缺省）与 `WebSocketRelayTransport`（`System.Net.WebSockets.ClientWebSocket`，给编辑器与非 WebGL 平台；不加依赖）。发送一帧一条二进制消息（`ClientWebSocket` 只能一个一个发，`Send` 先进队列、由一个发送任务按序发出，单条超过 `WriteTimeout` 或积压超过 `WebSocketRelayTransport.MaxPendingBytes`（16 MiB）就断开）；接收按字节流拼帧，不假设一条消息一帧（与 TS 相同；relay 一侧也是字节流，几帧拼在一条消息里它照样读）。`TransportFactory` 可以换成自己的实现（每次连接、重连各新建一个）。

  ```csharp
  var ticket = await lobby.CreateRoomAsync(new CreateRoomRequest { Game = "party", Mode = "duel", Transport = RelayTransport.Ws });
  var client = new RelayClient(lobby, new RelayClientOptions { Game = "party", Transport = RelayTransport.Ws });
  await client.ConnectAsync(ticket);               // 经 ws:// / wss:// 连 relay；重连也向 lobby 要 ws
  ```
- **Unity WebGL**：浏览器里没有套接字，`ClientWebSocket`、`TcpClient` 和 `HttpClient` 自带的网络都用不了（Unity 文档所述的平台限制；本仓只实测了下面的替代写法），线程池与计时器也不运行（2026-11-17 实测：`Task.Run`、`Task.Delay`、`SemaphoreSlim.WaitAsync(超时)`、`CancelAfter`、`ConfigureAwait(false)` 之后的续行都不跑）。三样都要换（维护者 2026-11-18 定 A1）：
  1. **调度：`RelayClientOptions.PumpDriven = true`**（WebGL 必开；缺省 false，其他平台行为不变）。打开时 SDK 不用线程池与计时器：读循环在调用 `ConnectAsync` / `RejoinAsync` 的线程（主线程）上起，所有等待回到调用者的上下文；心跳、读超时、连接超时、重连退避都由 `Pump` 按时钟检查（`Pump` 开头先做一轮，不计入 `budget`）。只在主线程上调本类的方法。`LobbyClient` 也要以 `pumpDriven: true` 构造（否则 `RelayClient` 构造时抛 `ArgumentException`）。
  2. **lobby 的 HTTP：`ILobbyHttp`**。`LobbyClient` 发请求的方式抽成这个小接口（`SendAsync(method, url, bearer, jsonBody)` → 状态码与正文；4xx / 5xx 照常返回，只有网络错误抛异常），缺省 `HttpClientLobbyHttp`；WebGL 上用 `UnityWebRequest` 实现，参考写法 `Assets/RelaySmoke/UnityWebRequestLobbyHttp.cs`：`new LobbyClient(url, gt2, new UnityWebRequestLobbyHttp(), pumpDriven: true)`。
  3. **传输：浏览器 WebSocket 的 `IRelayTransport`**：`Assets/Plugins/WebGL/RoostRelayWebSocket.jslib`（JS 一侧，收到的二进制消息排成字节队列，C# 每帧轮询，不从 JS 回调 C#）+ `Assets/RelaySmoke/BrowserRelayTransport.cs`（C# 一侧）。SDK 不带桥接代码（维护者定）。

  ```csharp
  var lobby = new LobbyClient(lobbyUrl, gt2, new UnityWebRequestLobbyHttp(), pumpDriven: true);
  var client = new RelayClient(lobby, new RelayClientOptions
  {
      Game = "party", Transport = RelayTransport.Ws, PumpDriven = true, ReconnectAttempts = 5,
      TransportFactory = () => new BrowserRelayTransport(),
  });
  // MonoBehaviour.Update：先收字节，再 Pump（心跳、超时、重连退避都在这里）
  BrowserRelayTransport.PollAll();
  client.Pump();
  ```

  **不 Pump 的后果**（加载场景卡住主线程、切后台时浏览器停了 Unity 的循环）：不发心跳、不判超时、不重连。成员超过 relay 的 `idle_timeout`（缺省 30s）没有帧就被 relay 断开（`Closed{idle_timeout}`）；房主受 `host_idle_timeout`（缺省 2s）约束，主线程卡住超过 2s 就被断开、进入宽限期（缺省 5s），宽限期内恢复 `Pump` 并重连回来仍是房主（成员收到 `OnHostResumed`），否则 relay 换 host。恢复 `Pump` 之后先收到 `Disconnected{Reconnecting}`，按 `ReconnectAttempts` 重连回来（`ReconnectAttempts` 为 0 时只收到最后一条 `Disconnected`，要自己 `RejoinAsync`）；`ConnectAsync` 的连接超时同样只在 `Pump` 时检查。

  实测见 [unity-webgl-smoke](../unity-webgl-smoke/README.md#实测结果) 与 [证据](../../../docs/maintenance/evidence/sdk-webgl-a1-20261118/README.md)（2026-11-17 修前的实测见 [C1 证据](../../../docs/maintenance/evidence/c1-csharp-websocket-20261117/README.md)）。Unity 编辑器（Mono）上 TCP 与 WebSocket 都实测可用；桌面播放器没有跑。

## 测试

```sh
source ~/.roost-it/roost-dataengine-it/env.sh
dotnet run --project client/dotnet/Roost.Client.Relay.Tests
dotnet build-server shutdown
```

先核对 [relay-packets.json](../../spec/relay-packets.json) 金样与拒绝规则、用假的消息型连接（经 `TransportFactory`）核对收的一侧按字节流拼帧（两帧一条消息、一帧拆两条、包头被切开）与发的一侧一条消息一帧、用本地 TcpListener 扮 relay 核对不认识的操作码与换 host 的 13 条（经 SDK 的公开方法发、经分发路径收），再 `go test -c ./client/relay` 并以 `ROOST_RELAYPEER_SERVE=1` 起对端（真实 relay 与 lobby；先起一个 `ROOST_RELAYPEER_WS=0` 的核对 lobby 对 ws 答 `no_relay`），对 TCP 与 WebSocket 各起一个对端、各跑一遍全部用例（WebSocket 一轮另加“向 lobby 要 ws 再连上”和“令牌与 Ping 拼在一条消息里 relay 照样读”）：lobby 错误、建房 / 加入 / 游戏通道往返 / 房间通道直达 / 禁言 / 踢人 / 离开 / 改状态、最后一人离开（房间已关时离开算成功）、断线重连，以及换 host（转交、断线后候选拿快照、宽限期内回来、候选顺序、快照节奏、定时快照、房主心跳、重连遇到 lobby 拒绝或不可用、开局后重连与踢离线座位）。另起一个 relay `idle_timeout` 改成 2s 的对端（`ROOST_RELAYPEER_IDLE_MS`）跑单线程用例（`SingleThread.cs`）：一个专用线程加只在这个线程上执行的 `SynchronizationContext`、像浏览器桥接一样只在轮询里完成的 TCP 连接与 lobby HTTP，凡是续行从别的线程投回来、或 SDK 在别的线程上调传输 / HTTP（`Task.Run`、计时器、`ConfigureAwait(false)` 排进线程池）都记违规；先核对 `PumpDriven` 关着时探测器抓得到违规，再以 `PumpDriven` 跑建房、加入、收发、心跳撑过 idle、断网重连、不 Pump 被断开后恢复 Pump 重连回来、转交房主、连接超时、离开，全程零违规（`ROOST_RELAY_SINGLE_THREAD_OFF=1` 时关着开关跑这一组，即修前的红）。`ROOST_RELAY_TESTS_ONLY=<名字片段>` 只跑名字含它的真实对端用例（名字形如 `[ws] room lifecycle`、`[single-thread]`）。需要本机 Go 工具链。Unity 上的运行见 [unity-webgl-smoke](../unity-webgl-smoke/README.md)。
