# C1：C# SDK 的 WebSocket 传输与 Unity WebGL 最小实测（2026-11-17）

方案：[open-work-c1-webgl-websocket-20261117](../open-work-c1-webgl-websocket-20261117/README.md)。维护者 2026-11-17 按推荐定：选项 A（传输接口公开、SDK 自带 TCP 与 ClientWebSocket，WebGL 桥接不进 SDK、README 给参考）；发送一帧一条消息、接收按字节流拼帧；传输由选项决定、重连沿用；Go SDK 不加 WebSocket；“做一个最小的测试”验证 WebGL 单线程。之后协调者转达：维护者已装 6000.7.0b7 的 WebGL 模块，要做真实 WebGL 实测。基线 `30131729`。

## 结论

1. **C# SDK 的 WebSocket 传输已实现并验证**（.NET 10 对真实 relay + lobby，与编辑器 / Mono）：`IRelayTransport` 公开，`TcpRelayTransport`、`WebSocketRelayTransport`（ClientWebSocket），`RelayClientOptions.Transport` / `TransportFactory`；全部真实对端用例对 TCP 与 WebSocket 各跑一遍通过（692 项检查）。
2. **WebGL 桥接（jslib + `BrowserRelayTransport`）在真实 WebGL 构建、浏览器里能连 relay、收发**（`bridge: ok`）。
3. **但 SDK 本身在 WebGL 上跑不起来**：Unity 6000.7.0b7 WebGL 下 `Task.Run`、`Task.Delay`、`SemaphoreSlim.WaitAsync(超时)`、`CancelAfter`、`ConfigureAwait(false)` 之后的续行、`ThreadPool.QueueUserWorkItem` 全部不运行（60 秒以上也没有迟到完成），只有回到 Unity 上下文的 `await` 运行。SDK 的读循环（`Task.Run` + `ConfigureAwait(false)`）、心跳（`Task.Run` + `SemaphoreSlim.WaitAsync(超时)`）、连接超时（`CancelAfter`）、重连退避（`Task.Delay`）与 `LobbyClient`（`ConfigureAwait(false)`）全都依赖它们；端到端在第一个 lobby 请求就停住（HTTP 已答 200，续行不回来）。**换传输解决不了，要改 SDK 的调度**——这是要维护者定的点（见文末）。我没有改调度。

## 代码

- `client/dotnet/Roost.Client.Relay/RelayTransport.cs`（新）：`IRelayTransport`（连接、发一整帧、收字节、Dispose 关闭；约定写在接口注释）、`TcpRelayTransport`（原来的 TCP 拨号与地址解析搬过来）、`WebSocketRelayTransport`（ClientWebSocket；`Send` 进队列、一个发送任务按序一条消息一帧发，单条超过 `WriteTimeout` 或积压超过 16 MiB 断开；收的时候不看消息边界，文本消息当协议错误，空消息跳过）。
- `RelayClient.cs`：会话持有 `IRelayTransport`；`DialAsync` 经工厂新建连接（超时仍靠 Dispose 连接，不依赖各实现对 token 的处理）；`RejoinOnceAsync` 向 lobby 要 `options.Transport`（原来写死 `Tcp`）；选项加 `Transport`、`TransportFactory`。
- `RelayCodec.cs`：`ReadAsync(IRelayTransport, …)` 与原来的 `ReadAsync(Stream, …)` 共用一个按包头拼帧的读法。
- `LobbyClient.cs` / csproj：注释。Go 侧只改了 `peer_test.go` 一行注释与 `client/relay/README.md`（Go 不加 WebSocket，维护者定）。
- 测试 `Roost.Client.Relay.Tests/Program.cs`：对端二进制只编一次；先起一个不开 WebSocket 的对端核对 lobby 对 ws 答 `no_relay`（原来在“lobby errors”里，挪出来，检查不变）；再对 TCP、WebSocket 各起一个新对端跑全部 14 个用例；WebSocket 一轮另加“向 lobby 要 ws 再连上”“令牌与 Ping 拼在一条消息里 relay 照样读”；新增 `MessageBoundaries`（假的消息型连接经 `TransportFactory`：Welcome 与一帧游戏帧同一条消息、一帧拆两条且包头被切开、后半与下一帧同一条；发的一侧每条消息恰好一帧）。
- `client/dotnet/unity-webgl-smoke/`（新）：最小 Unity 工程与 WebGL 参考桥接，说明见它的 README。

## 先红后绿

红（基线代码，只加了“向 lobby 要 ws 再连上”用例，`dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$PWD"`）：

```
lobby without a ws relay: ok
peer: lobby http://127.0.0.1:65125, relay 127.0.0.1:65119 / ws://127.0.0.1:65123/relay, room directory redis
FAILED: System.ArgumentException: 'ws://127.0.0.1:65123/relay' is not a TCP address (the C# SDK connects over TCP; ask the lobby for transport tcp).
   at Roost.Client.Relay.RelayClient.ParseAddress(String address) in .../RelayClient.cs:line 887
   at Roost.Client.Relay.RelayClient.DialAsync(...) in .../RelayClient.cs:line 848
   at Roost.Client.Relay.RelayClient.ConnectAsync(...) in .../RelayClient.cs:line 437
   at Program.WebSocketConnect(Peer peer) in .../Program.cs:line 464
```

绿（实现后同一命令，50s）：`golden`、`frames across message boundaries (fake transport)`、`unknown control op`、`compressors`、`host frames against a fake relay`、`lobby without a ws relay`，`[tcp]` 14 个用例，`[ws] ask the lobby for ws and connect`、`[ws] relay reads several frames from one message` 与 `[ws]` 14 个用例全部 ok，`all 692 checks passed`（改动前 409 项）。

## 变异

| 变异 | 结果 |
|---|---|
| `RejoinOnceAsync` 向 lobby 要的传输改回写死 `RelayTransport.Tcp` | `[ws] reconnect` 红：`Timed out waiting for reconnected`（Program.cs `Reconnect`） |
| `RelayCodec.ReadAsync(IRelayTransport)` 改成“一次收到的字节当恰好一帧” | `frames across message boundaries` 红：`InvalidDataException: Incomplete or trailing relay frame bytes.`（连接时 Welcome 与游戏帧在同一条消息里） |
| 方案第 4 步“发送时两帧拼成一条消息仍应可用” | 没有做成 SDK 变异，改成直接用例：`[ws] relay reads several frames from one message`（令牌帧与 Ping 拼成一条消息发出，relay 回 Welcome 与 Pong），绿 |
| jslib 的 `RoostWsReceive` 不按请求的长度截（一次交出整条消息剩下的） | `check-jslib.mjs` 红：`receive copied more than asked` |

变异都已撤回（与备份逐字节比对一致）。

## 其他验证

- `GOWORK=off go test -race -count=1 ./client/relay/`：ok（10.7s）；`GOWORK=off go vet ./client/relay/`：ok。
- `node client/dotnet/unity-webgl-smoke/check-jslib.mjs`（node 26 自带的 WebSocket + Emscripten 替身加载 jslib，连真实对端）：`check-jslib: ok (welcome slot 1, pong, 7 receives of at most 7 bytes, close, refused connection)`。
- 收尾全量 C# 用例（文档改完之后）：`dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$PWD"` rc=0，52s，`all 692 checks passed`；随后 `dotnet build-server shutdown`。

## Unity 实测

工程：`client/dotnet/unity-webgl-smoke`，经 `prepare.sh` 拷到 scratchpad 再打开（不在仓库里生成 Library）。对端：`serve.mjs`（测试对端 + 静态目录 + `/lobby` 同源代理），房间目录用 `~/.roost-it` 的 Redis。Unity 许可证：编辑器经本机 Hub 的 LicensingClient 正常拿到（日志 `Successfully connected to LicensingClient`），没有登录或许可证问题。没有下载任何东西：`Packages/manifest.json` 只用编辑器内置模块（`BuiltInPackages` 里有 imgui / jsonserialize / unitywebrequest）；WebGL 模块是维护者装的。

### 编辑器（6000.3.12f1，Mono，macOS ARM64，`-batchmode -nographics -executeMethod RelaySmokeEditor.RunInEditor`）

- 探针全部完成，都在线程池线程上（例如 `Task.Run: ran on thread 12`、`ConfigureAwait(false) after a main-thread completion: continued on thread 11`）。
- **TCP**：1 次，`SMOKE PASS`（建房、加入、成员 → 房主 5 帧、房主广播、房间通道、停 40s 后两边在线，RTT 0.2ms，离开）。
- **WebSocket**（SDK 自带的 ClientWebSocket）：停 40s 的 13 次里 8 次 `SMOKE PASS`，5 次在停留期间**成员**连接被对端直接关掉（`The remote party closed the WebSocket connection without completing the close handshake`，房主收到 `MemberLeft{Disconnected}`），断开时刻都是成员心跳 Ping 的整 10 秒点（20.5s、40.5s；成员 0.5s 时连上，`PingInterval` 10s）；房主（600ms 心跳）一次没断。另有 1 次把 `ClientWebSocket.KeepAliveInterval` 改成 200ms 的实验，通过（实验已撤回）。TCP 没有这个现象（1 次）。
- 断线的原因（在 relay 一侧加临时调试输出、不提交）：relay 读这条连接时报 `failed to get reader: use of closed network connection`，写循环没有报错——连接是被 relay 进程里的 coder/websocket 自己关掉的。代码对照：relay 的写循环（`service/relay/conn.go` `writeLoop`）每批写之前 `SetWriteDeadline(now + WriteTimeout)`（缺省 10s），写完不清；coder/websocket v1.8.15 的 `NetConn` 写期限计时器到期时如果正好有一次写持有写锁，就取消整条连接的写上下文（`netconn.go` 的 `writeTimer` → `writeCancel()`），写里注册的 `context.AfterFunc` 随之关掉连接（`conn.go` `setupWriteTimeout`）；`infra/network/wsconn` 的注释也写了“期限在一次读写进行中到期时……关闭整条连接”。成员每 10s 一个 Ping、relay 立刻回 Pong，上一次写的期限恰好在下一次写的时刻到期，于是偶发撞上。**这是 relay WebSocket 监听的缺陷，不是 C# SDK 或 Unity 的**：任何 WebSocket 客户端（TS SDK、浏览器）写的节奏接近 `WriteTimeout` 时都可能遇到。用 Go 客户端按 100ms 节奏、`WriteTimeout` 100ms 跑 3×10s 没有复现（Go 的 sleep 总是稍晚、计时器先在空闲时到期），所以根因是代码与日志推断，**尚未确认**（没有确定性复现）。可能的修法：写完一批后 `SetWriteDeadline(time.Time{})`，让期限只在写进行中存在；Gate 的 WebSocket 会话（`infra/network/gateway/tcp_session.go` 每次写设期限）是否同样受影响尚未确认。不在 C1 范围，没有改。
- 观察（不在 C1 范围）：房间里最后一个人 `LeaveAsync` 时 lobby 答 `room_not_found`（SDK 先在 relay 上发 Leave，relay 随即关房，再经 lobby 离开时房间已没了；Go SDK 同样的顺序）。冒烟把它记成一行、不算失败。

### WebGL（6000.7.0b7，`-buildTarget WebGL -executeMethod RelaySmokeEditor.BuildWebGL`，开发版、不压缩；Claude 桌面应用的内置浏览器面板，Chromium）

构建：首次约 1.5 分钟、改脚本后 20 秒，`Build Finished, Result: Success.`。页面 `http://127.0.0.1:18789/?hold=40`（`serve.mjs <Build/WebGL> 18789`）。页面上 / 控制台里的输出（摘要）：

```
SMOKE start: lobby http://127.0.0.1:18789/lobby, transport Ws, browser websocket True, main thread 1
SMOKE probe Task.Run: DID NOT COMPLETE within 3s
SMOKE probe Task.Delay(100ms): DID NOT COMPLETE within 3s
SMOKE probe SemaphoreSlim.WaitAsync(100ms): DID NOT COMPLETE within 3s
SMOKE probe CancelAfter(100ms): DID NOT COMPLETE within 3s
SMOKE probe ConfigureAwait(false) after a main-thread completion: DID NOT COMPLETE within 3s
SMOKE probe await (Unity context) after a main-thread completion: continued on thread 1 (main)
SMOKE probe ThreadPool.QueueUserWorkItem: DID NOT COMPLETE within 3s
SMOKE probes: 6 did not complete (the SDK depends on them; see README)
SMOKE bridge: ok (UnityWebRequest lobby → jslib WebSocket → welcome + pong, awaits on the Unity context)
SMOKE FAIL stuck at step 'end to end: lobby (HttpClient + UnityWebRequestHandler)' for 60s (at 66.3s)
```

- 探针在 66 秒内都没有迟到完成（冒烟会把 3 秒之后才完成的记成 `completed LATE`，一条也没有）。
- 卡住的那一步：浏览器网络记录里 `POST /lobby/harness/game-token → 200 OK` 已经返回，C# 的续行没有回来。卡在 `UnityWebRequestHandler` 自己的 `ConfigureAwait(false)` 还是 `HttpClient` 内部，**尚未确认**；不影响结论，因为 `LobbyClient` 自己也用 `ConfigureAwait(false)`。
- 第一次构建（还没有“只走桥接”一步与卡住检测）同样是 6 个探针不完成、端到端停在第一个 lobby 请求。第二次构建之后冒烟脚本只加了成员离开 / 断线的日志行（编辑器复查断线用），没有再构建 WebGL。
- 实测时内置浏览器面板处于隐藏状态（`tabs_context`：“The Browser pane is currently hidden”），页面作为后台页运行；Update 照常运行（桥接一步完成、卡住检测按时触发）。探针不完成与可见性无关的判断依据：`Task.Run`、线程池与 `ConfigureAwait(false)` 续行都不靠计时器；但面板可见时**没有重跑**，尚未确认。
- **切后台没有测**：SDK 这条路径连不上，切后台没有意义；桥接一步是短连接。内置浏览器面板的后台标签页 `visibilityState` 也始终是 `visible`（KNOWN-LIMITS 已有一条）。

复现（协调者或维护者在浏览器里看）：

```sh
go test -c -o /tmp/relaypeer ./client/relay
client/dotnet/unity-webgl-smoke/prepare.sh /tmp/roost-relay-smoke
/Applications/Unity/Hub/Editor/6000.7.0b7/Unity.app/Contents/MacOS/Unity -batchmode -nographics -quit \
  -projectPath /tmp/roost-relay-smoke -buildTarget WebGL -executeMethod RelaySmokeEditor.BuildWebGL -logFile /tmp/smoke-webgl-build.log
ROOST_RELAYPEER_BIN=/tmp/relaypeer node client/dotnet/unity-webgl-smoke/serve.mjs /tmp/roost-relay-smoke/Build/WebGL 8789
# 浏览器开 http://127.0.0.1:8789/?hold=40 ，预期输出即上面的摘要（约 66 秒后出现 stuck 一行）
```

## WebGL 为什么不行、改调度要多少工作（供维护者决定）

证据：上面的探针。SDK 里依赖这些原语的地方（行号按本分支）：

| 位置 | 用法 | WebGL 上 |
|---|---|---|
| `RelayClient.Start` | `completion = Task.Run(() => RunAsync(session))` | 读循环不启动 |
| `RelayClient.ServeAsync` | `Task.Run(() => HeartbeatAsync(session))` | 心跳不启动 |
| `RelayClient.HeartbeatAsync` | `SemaphoreSlim.WaitAsync(间隔)` | 不醒来：不发 Ping，30s 后被 relay 判空闲断开，读超时也不检查 |
| `RelayClient.DialAsync` | `timeout.CancelAfter(ConnectTimeout)` | 连接超时不生效 |
| `RelayClient.ReconnectAsync` | `Task.Delay(backoff)` | 自动重连不发生 |
| `RelayClient` / `RelayCodec.ReadAsync` / `LobbyClient` 全部 `await` | `ConfigureAwait(false)` | 续行排进线程池，不运行 |
| `LobbyClient` | `HttpClient` | 需要 UnityWebRequest；HttpClient 内部的续行能否回来尚未确认 |

可选方向：

- **A1（推荐）单线程模式，选项打开**：`RelayClientOptions` 加一个“由 Pump 驱动”的开关（WebGL 必开，其他平台保持现状）。打开时：读循环直接在调用线程起（不用 `Task.Run`），所有 `await` 回到调用者的上下文（`ConfigureAwait(!singleThread)`，机械替换）；心跳、读超时、连接超时、重连退避改由 `Pump` 按时钟检查（把 `HeartbeatAsync` 的一轮判断抽成 `Tick(now)`）；`LobbyClient` 的 HTTP 抽成一个小接口（缺省 HttpClient，WebGL 由游戏用 UnityWebRequest 实现，参考写法已在冒烟工程里）。估计 SDK 改动 200～350 行，加一组“单线程上下文 + 禁用线程池”的 .NET 用例跑现有对端用例的子集（约 150 行），再用本冒烟工程在 WebGL 里验收；约 1～2 天。代价：两条调度路径都要测；单线程模式下游戏不 `Pump`（加载场景、切后台时 Unity 停了循环）就不发心跳，超过 relay 的 `idle_timeout`（30s）会断开、靠重连回来。
- **A2 全部改成 Pump 驱动**：只有一条路径，测试面不翻倍；代价是桌面 / 移动端行为也变（不 `Pump` 就没有心跳），与 `Roost.Client` 的 GameClient 现有做法不一致，改动面比 A1 大。
- **B 不支持 C# SDK 上 WebGL**：文档写明 Unity WebGL 不能用 `Roost.Client.Relay`；WebGL 游戏要么不做开房间，要么自己在 jslib 里调用 TS SDK 再桥接事件（工作量在游戏侧，大）。
- 无论哪种，切后台：WebGL 页面在后台时 Unity 的循环会停或被限流，C# 里的心跳都会停，长时间后台必断线（TS SDK 用 Worker 计时器只是缓解）。要不要把心跳挪进 jslib 是 A1/A2 之后的问题，尚未确认。

## 尚未确认

- relay WebSocket 连接偶发被自己关掉（见上“Unity 实测 / 编辑器”）：根因按代码与日志推断，没有确定性复现；Gate 的 WebSocket 是否同样受影响。
- WebGL 下 `HttpClient` 本身（不经 `ConfigureAwait(false)` 的处理器）能否完成请求。
- 浏览器面板可见时重跑 WebGL 冒烟（这次面板隐藏）；Safari / Firefox / 移动浏览器没测；IL2CPP 裁剪对 SDK 没有报错（构建成功、桥接一步用到了 `RelayCodec`），但 SDK 的大部分路径在 WebGL 上没有跑到。
- 切后台、刷新页面（方案第 5 步）：SDK 路径连不上，没有测。
- Unity 桌面播放器（`BuildMac`）没有跑；编辑器只跑了 6000.3.12f1。

## 建议写进 CHANGELOG 的文字（[Unreleased]，协调者统一写）

- C# 开房间 SDK 支持 WebSocket（交接 C1，维护者 2026-11-17 按推荐）：传输抽成公开接口 `IRelayTransport`，自带 `TcpRelayTransport`（缺省）与 `WebSocketRelayTransport`（`ClientWebSocket`）；`RelayClientOptions.Transport` 决定向 lobby 要哪种地址，断线重连沿用同一种（原来重连固定要 TCP）；`TransportFactory` 可换成自己的实现。发送一帧一条二进制消息，接收按字节流拼帧。`RelayCodec.ReadAsync` 多一个 `IRelayTransport` 重载。Go SDK 不加 WebSocket。
- 新增 `client/dotnet/unity-webgl-smoke`：Unity 最小冒烟工程与 WebGL 桥接参考（jslib + `BrowserRelayTransport` + `UnityWebRequestHandler`）。实测：编辑器上 TCP / WebSocket 可用；**Unity WebGL 上 SDK 现在跑不起来**（线程池、计时器与 `ConfigureAwait(false)` 续行不运行），桥接本身可用。

## 建议写进 KNOWN-LIMITS 的文字（协调者统一写）

替换“开房间 SDK”下“Go 与 C# 只连 TCP，TS 只连 WebSocket”一条为：

- **Go 只连 TCP（维护者 2026-11-17 定不加），C# 连 TCP 或 WebSocket，TS 只连 WebSocket。**
- **Unity WebGL 上 C# 开房间 SDK 跑不起来**（2026-11-17 在 6000.7.0b7 WebGL 构建里实测，证据：本文件）：WebGL 上线程池、计时器与 `ConfigureAwait(false)` 之后的续行都不运行，SDK 的读循环、心跳、连接超时、重连与 `LobbyClient` 依赖它们。浏览器 WebSocket 桥接（`client/dotnet/unity-webgl-smoke` 的 jslib + `BrowserRelayTransport`）实测可用。要支持需改 SDK 调度（由 `Pump` 驱动），待维护者定（遗留）。
- （遗留，尚未确认）**relay 的 WebSocket 连接偶发被 relay 自己关掉**：写循环每批写前设 `WriteTimeout` 期限、写完不清，coder/websocket 的 `NetConn` 在期限到期时若正有一次写，就关闭整条连接；成员 10s 心跳的 Pong 正好落在上一次写期限的到期点上。Unity 编辑器冒烟 13 次里 5 次成员在 20.5s / 40.5s 断开（relay 读到 `use of closed network connection`）。没有确定性复现；修法候选是写完清期限；Gate 的 WebSocket 会话是否同样受影响待查（证据：本文件）。
- （观察，尚未确认是否算缺陷）房间里最后一个人 `LeaveAsync` / `Leave` 时 lobby 答 `room_not_found`：SDK 先在 relay 上发 Leave，relay 关房后再经 lobby 离开。Go 与 C# 同样的顺序。
