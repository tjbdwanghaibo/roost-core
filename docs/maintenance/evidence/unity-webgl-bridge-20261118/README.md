# Unity WebGL 浏览器桥接放进 Unity 包（方案 B，2026-11-18）

维护者 2026-11-18 定方案 B：把 Unity WebGL 的浏览器 WebSocket 桥接放进 core 的 Unity 包（`client/unity`，`Roost.Unity`），游戏工程不再从冒烟工程拷参考代码。取代 2026-11-17 C1 选的 A（桥接只在 `unity-webgl-smoke` 给参考写法，见 [C1 方案证据](../open-work-c1-webgl-websocket-20261117/README.md)）。基线 main `bf68b3b8`。

## 结论

1. **包里有桥接了**：`build-unity.py`（含 `--managed-only`）打的包新增 `Runtime/BrowserWebSocket.cs`、`Runtime/WebGL/RoostWebSocket.jslib`、`Runtime/UnityWebRequestLobbyHttp.cs`、`Runtime/RoostPlatform.cs` 与 `Runtime/Roost.Client.Relay.dll`，修前修后对照见 [package-files.txt](package-files.txt)。
2. **新接口 `RoostPlatform`**：`Configure(ConnectionOptions)` / `Configure(RelayClientOptions)` 在 WebGL 播放器里设 `PumpDriven` 与浏览器传输，其他平台原样返回；`CreateLobby`、`Poll`、`InBrowser`。`RoostClientDriver` 每帧先轮询桥接再 `Pump`。
3. **冒烟工程只用包里的桥接**：`unity-webgl-smoke` 删掉自带的 4 个桥接文件，GateSmoke 与 RelaySmoke 都经 `RoostPlatform`。用新包重新构建 WebGL（Unity 6000.7.0b7），在 Claude 内置浏览器面板（Chromium）里：
   - 连 game（GateSmoke）：`SMOKE PASS held 101s connected, heartbeat round trips 6, handshake 1994ms`；
   - 开房间 relay（RelaySmoke）：面板可见时 `SMOKE PASS`（桥接、建房、加入、收发帧、停 40s、离开）；面板隐藏（约 1 帧/秒）时两次 FAIL，旧构建在同样的慢帧下也会 FAIL，见第 4 节。
4. **.NET 用例**：新增 `TestBrowserBridgeRelay`（同一个类当 relay 传输）；变异核对变红。

## 1. 改了什么

| 项 | 做法 |
|---|---|
| 包含 relay 吗 | 修前包里只有 `Roost.Client.dll`，没有 `Roost.Client.Relay.dll`（`unity-webgl-smoke/prepare.sh` 另外把它放进 `Assets/Plugins`）。现在 `build-unity.py` 两个都构建、都放进 `Runtime/`，`Roost.Unity.asmdef` 两个都引用。取舍：开房间是框架正式能力，WebGL 上 relay 与连 game 共用一个桥接类，不放进包就得再分一个包或让游戏自己拷 DLL；代价是只连 game 的游戏也多一个约 90 KB 的 DLL（89600 字节）（`link.xml` 不 preserve 它，没用到的部分 IL2CPP 照常裁剪）。 |
| 桥接类 | 原 `BrowserGateTransport`（连 game）与 `BrowserRelayTransport`（relay）几乎相同，合成一个 `Roost.Unity.BrowserWebSocket`，同时实现 `IGateTransport` 与 `IRelayTransport`（`SendAsync` 包一层同步的 `Send`）。去掉了 relay 版在 `ConnectAsync` 里的 `cancellation.Register(Dispose)`：两个 SDK 的连接超时都自己 `Dispose` 传输（`RelayClient.DialAsync` 的 `using (timeout.Token.Register(transport.Dispose))`、`TcpSession.ConnectAsync` 同理），与 SDK 自带传输一致。 |
| jslib | 原 `Assets/Plugins/WebGL/RoostRelayWebSocket.jslib` 挪到 `client/unity/Runtime/WebGL/RoostWebSocket.jslib`（`git mv`），JS 对象名改 `RoostWebSocket`，导出函数名 `RoostWs*` 不变。放在 `Runtime/WebGL/` 而不是 `Runtime/Plugins/`：包的 `NativePluginImporter` 会把 `Runtime/Plugins/` 下的插件改成只认原生平台，`--merge-plugins-from` 也会整体复制那个目录。`.jslib` 的缺省导入设置就是只给 WebGL，构建里确认链接上了（`GateWebGL.framework.js` / `WebGL.framework.js` 里有 `_RoostWsOpen`）。 |
| lobby HTTP | `UnityWebRequestLobbyHttp` 挪进包（命名空间 `Roost.Unity`）；`package.json` 声明依赖内置模块 `com.unity.modules.unitywebrequest`，包版本 0.2.0 → 0.3.0。 |
| 按平台选 | `RoostPlatform.InBrowser` 只在 `UNITY_WEBGL && !UNITY_EDITOR` 为 true（编辑器切到 WebGL 目标仍用 SDK 自带传输，能在编辑器里跑）。WebGL 上 `Configure(ConnectionOptions)` 没设 `WebSocketUrl` 时抛 `ArgumentException`；`Configure(RelayClientOptions)` 另设 `Transport = Ws`；`CreateLobby` 用 `UnityWebRequestLobbyHttp` 并以 `pumpDriven: true` 构造，其他平台是 SDK 缺省的 `HttpClient`。 |
| Driver | `RoostClientDriver.Update` 每帧先 `BrowserWebSocket.PollAll()`（没有浏览器连接时直接返回）再 `Pump`；`Attach(client, beforePump)` 重载删除（只为传 `PollAll` 而设，未发版，不兼容）。只用 `RelayClient` 的游戏每帧 `RoostPlatform.Poll(); relay.Pump();`，没有另加 relay 的 Driver。 |
| 冒烟工程 | 删 `BrowserGateTransport.cs`、`BrowserRelayTransport.cs`、`UnityWebRequestLobbyHttp.cs`、jslib；`prepare.sh` 不再单独放 `Roost.Client.Relay.dll`；RelaySmoke 去掉页面参数 `pump=0`、`transport=tcp`（WebGL 上由 `RoostPlatform` 定）；`check-jslib*.mjs` 改读包里的 jslib。 |
| pretag 范围 | `scripts/pretag-scope.py`：改 `client/unity/` 也算 C# 受影响（`Roost.Client.Tests` 链接其中的 `RoostClientDriver` 与 `BrowserWebSocket`）。 |

## 2. .NET 用例与变异

- `dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json`：退出码 0，全部 PASS，见 [dotnet-tests.txt](dotnet-tests.txt)。其中
  - `PASS browser bridge for the gate (BrowserWebSocket + fake jslib, PumpDriven): split / merged / near-max frames, one frame per Send`（原用例，改链接包里的源码）；
  - `PASS browser bridge for the relay (BrowserWebSocket + fake jslib, PumpDriven): split welcome, merged game frames, ping / pong`（新增：Welcome 拆成三条消息、两帧游戏帧合成一条、Ping / Pong 测出 Rtt，单线程零违规）。
- 先红：新用例的承诺是“桥接按调用方给的长度交字节、跨消息拼帧不丢”。把 `TryReceive` 改成按中转缓冲长度取（多取的字节没有地方放），新用例单独排在最前也在 relay 连接时崩掉（`ArgumentException: Offset and length were out of bounds`，退出码 134），见 [red-relay-overread.txt](red-relay-overread.txt)；恢复源码后绿。修前（基线）这个用例不存在、也编不过（没有 `Roost.Unity.BrowserWebSocket`），那是平凡的红，不作为证据。
- `scripts/test-client-dotnet.sh`（完整模式）：见第 5 节。

## 3. 打包与 jslib

| 命令 | 结果 |
|---|---|
| `python3 scripts/client/build-unity.py --managed-only --output <新目录>`（修前 `bf68b3b8`） | 退出码 0；包里没有桥接、没有 `Roost.Client.Relay.dll` |
| 同上（修后） | 退出码 0；多出 `BrowserWebSocket.cs`、`Roost.Client.Relay.dll`、`RoostPlatform.cs`、`UnityWebRequestLobbyHttp.cs`、`WebGL/RoostWebSocket.jslib` |
| `python3 scripts/client/build-unity.py --target macos --output <新目录>`（修后，原生路线） | 退出码 0；同样带上述文件，另有 `Plugins/macOS/libroost_client.dylib` |
| `node client/dotnet/unity-webgl-smoke/check-jslib-frames.mjs` | 退出码 0：`check-jslib-frames: ok (split / merged / near-max frames, one frame per send, text fails the socket)` |
| `ROOST_RELAYPEER_BIN=<go test -c ./client/relay> node client/dotnet/unity-webgl-smoke/check-jslib.mjs` | 退出码 0：`check-jslib: ok (welcome slot 1, pong, 7 receives of at most 7 bytes, close, refused connection)` |
| `client/dotnet/unity-webgl-smoke/prepare.sh <新目录>` + Unity 6000.7.0b7 batchmode `GateSmokeEditor.BuildWebGL`、`RelaySmokeEditor.BuildWebGL` | 都是 `Build Finished, Result: Success.`；`Assets/` 里已没有桥接文件，`Assets/Plugins` 不存在 |

## 4. WebGL 实跑（Claude 内置浏览器面板，Chromium）

对端：`serve-gate.mjs`（`infra/network/gateway` 的测试对端，`idle_timeout` 90s）与 `serve.mjs`（`client/relay` 的测试对端，relay + lobby，房间目录用内存）；端口 18786～18790。日志是控制台里的 `SMOKE` 行。

### 4.1 连 game（GateSmoke），面板隐藏

[webgl-gate-smoke.txt](webgl-gate-smoke.txt)：`connecting … (in browser True, pump-driven True, hold 100s)` → `handshake ok in 1994ms` → 每 10s 一行 → `SMOKE PASS held 101s connected, heartbeat round trips 6, handshake 1994ms`。握手比 11-18 前一轮的 95ms 慢，是因为面板隐藏、页面约 1 帧/秒（下节实测），桥接每帧才轮询一次。

### 4.2 开房间（RelaySmoke）

[webgl-relay-smoke.txt](webgl-relay-smoke.txt)。

| 次 | 构建 | 条件 | 结果 |
|---|---|---|---|
| 1、2 | 新（包里的桥接） | 面板隐藏；页面里量 `requestAnimationFrame`：15s 内 19 帧，最大间隔 1011ms | FAIL：探针、`bridge: ok`、建房、房主连上之后，房主在 9～11s 被 relay 以 `IdleTimeout` 断开（`host_idle_timeout` 2s），成员加入时房间已关（`RoomNotFound`） |
| 3 | 旧（`bf68b3b8` 的冒烟工程，自带桥接） | 帧率正常（rtt 约 8ms，面板应已显示） | PASS，`rtt host 7.9ms member 8.8ms` |
| 4 | 新 | 面板可见（5s 内 600 帧） | **PASS**：`bridge: ok` → 建房 → 房主、成员连上 → `frames: ok (member → host 5, host → member 1, room 1)` → `held 40s: still connected, rtt host 8.0ms member 9.0ms` → `left: ok (member room gone False, last one room gone True)` |
| 5 | 旧 | 面板可见，`index.html` 里把 `requestAnimationFrame` 推迟 1000ms（约 1 帧/秒，模拟隐藏面板；脚本 `slowframes.py` 的做法见 webgl-relay-smoke.txt） | FAIL，与第 1、2 次相同：房主 14.1s 被 `IdleTimeout` 断开，成员 `RoomNotFound` |
| 6 | 新 | 同第 5 次 | PASS，`rtt host 1.7ms member 999.8ms` |

结论：

- 包里的桥接在 WebGL 上能连 relay、收发、心跳、离开（第 4 次），与旧的自带桥接同样（第 3 次）。
- 约 1 帧/秒时房主会不会被 `host_idle_timeout` 断开，新旧构建都是有时会、有时不会（新：2 FAIL 1 PASS；旧：1 FAIL，加上 11-18 前一轮隐藏面板里 rtt 约 1s 时 2 次 PASS），与本次改动无关。房主心跳间隔 600ms，按理 1 帧/秒时每帧都会发一个 Ping，间隔不该超过 2s；为什么还会超过，**尚未确认**（登记在 KNOWN-LIMITS“Unity WebGL 上 C# 开房间 SDK 要开 PumpDriven”一条）。

## 5. 其他验证

- `scripts/test-client-dotnet.sh`（完整模式）：退出码 0。金样与 SDK 自测（含上面两条桥接用例）、地形、跨 Gate 放弃握手、C# 经 WebSocket 连玩家接入（嵌入 TCP 与独立 Gate）、jslib 拼帧、开房间 SDK 对真实 relay 对端 924 项检查（含单线程 `PumpDriven`）、地形 DAO 夹具、生成工程 TCP，见 [test-client-dotnet.txt](test-client-dotnet.txt)。
- 根门禁 `GOWORK=off go test -count=1 .`：提交前跑，结果写在提交说明与汇报里。

## 6. 没测的

- Safari、Firefox、手机浏览器；真实切后台；`wss://` 经真实负载均衡与 TLS。
- 编辑器 batchmode 与桌面 Player 上的 GateSmoke / RelaySmoke 这一轮没有重跑（编辑器与其他平台上 `RoostPlatform` 原样返回选项，行为与改动前相同）。
- Unity 2021.3（`package.json` 声明的最低版本）上没有编译；只在 6000.7.0b7 上构建过。
- 原生路线包（`--target macos`）只打了包，没有导入 Unity 编译。
- 进入游戏后的自身视图同步在 IL2CPP 裁剪下没验证（与之前相同）。

## 7. 收尾

工程、构建与对端都在会话 scratchpad，没有进仓库；对端（`relaypeer`、`gatepeer`）与 `serve*.mjs` 已停，Unity batchmode 都已退出。
