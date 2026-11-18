# unity-webgl-smoke：Roost.Client.Relay 与 Roost.Client（连玩家接入）在 Unity（含 WebGL）上的最小冒烟

交接 C1（维护者 2026-11-17“做一个最小的测试”）：看 `Roost.Client.Relay` 在 Unity 的调度下能不能跑，重点是 **WebGL 单线程**。2026-11-18 起 WebGL 上以 `RelayClientOptions.PumpDriven` 跑（维护者定 A1）；同日维护者定方案 B，浏览器桥接（C# 传输、jslib、lobby 的 `UnityWebRequest` HTTP）挪进 Unity 包（`client/unity`，`RoostPlatform` 按平台选传输），冒烟工程不再自带副本，与游戏工程一样用包里的。

2026-11-18 另加 **GateSmoke**：`Roost.Client` 经 WebSocket 连玩家接入（嵌入 TCP 或独立 Gate），见下文“连玩家接入（GateSmoke）”。

## 内容

| 文件 | 作用 |
|---|---|
| `Assets/RelaySmoke/RelaySmoke.cs` | 冒烟本体：①调度原语探针（`Task.Run`、`Task.Delay`、`SemaphoreSlim.WaitAsync(超时)`、`CancelAfter`、`ConfigureAwait(false)` 续行、线程池；对照组是回到 Unity 上下文的 `await`）；②只走桥接（WebGL 下）：`UnityWebRequest` 向 lobby 要票据 → 包里的 `BrowserWebSocket` 连 relay → 令牌、Ping → 收 Welcome、Pong，不经 SDK 的调度；③端到端经 SDK（选项与 lobby 经 `RoostPlatform`，WebGL 上 `PumpDriven`）：房主建房、成员加入、成员 → 房主 5 帧、房主广播、房间通道、停 `hold` 秒（大于 relay 空闲超时 30s，看心跳）、离开（成员正常离开 `RoomGone` 为 false）。日志行以 `SMOKE` 开头，最后一行 `SMOKE PASS` / `SMOKE FAIL …`；某一步 `hold + 20` 秒没往下走时报 `SMOKE FAIL stuck at step '…'`。冒烟自己的等待按帧计时，不借助被测的原语。 |
| `Assets/RelaySmoke/RelaySmokeBehaviour.cs` | 播放器入口：Update 每帧驱动、OnGUI 显示日志、记录切前后台。WebGL 读页面查询参数 `lobby`（缺省同源 `/lobby`）、`hold`（传输与 `PumpDriven` 由 `RoostPlatform` 选，不再有 `pump=0`、`transport=tcp` 开关）；桌面播放器读 `-roostLobby`、`-roostTransport ws|tcp`、`-roostHold`、`-roostPumpDriven 1`。 |
| `Assets/Editor/RelaySmokeEditor.cs` | 批处理入口：`RunInEditor`（编辑器里直接跑，环境变量 `ROOST_SMOKE_LOBBY`、`ROOST_SMOKE_TRANSPORT`、`ROOST_SMOKE_HOLD`、`ROOST_SMOKE_PUMP_DRIVEN=1`；不要加 `-quit`）、`BuildWebGL`（不压缩、开发版，输出 `Build/WebGL`）、`BuildMac`。 |
| `prepare.sh` | 把工程拷到仓库外的新目录，用 `scripts/client/build-unity.py --managed-only` 打 Unity 包放进 `<目录>/RoostSdk` 并在 `Packages/manifest.json` 引用（Unity 会生成 `Library/` 与 `.meta`，不在仓库里打开）。包里有 `Roost.Client.dll`、`Roost.Client.Relay.dll`、`RoostClientDriver`、`RoostPlatform` 与浏览器桥接，冒烟工程自己不带。 |
| `serve.mjs` | 起测试对端（真实 relay + lobby，带 WebSocket 监听），发静态目录，`/lobby/*` 同源代理到 lobby。 |
| `check-jslib.mjs` | 不经 Unity、不经浏览器，在 node 里用 Emscripten 替身加载包里的 jslib（`client/unity/Runtime/WebGL/RoostWebSocket.jslib`），连真实对端核对它的 JS 逻辑。 |

`Packages/manifest.json` 只用编辑器自带的内置模块（imgui、jsonserialize、unitywebrequest），不从网上取包。

## 怎么跑

```sh
source ~/.roost-it/roost-dataengine-it/env.sh                       # 可选：房间目录用 Redis
go test -c -o /tmp/relaypeer ./client/relay                         # 对端，serve.mjs / check-jslib.mjs 用 ROOST_RELAYPEER_BIN 复用
node client/dotnet/unity-webgl-smoke/check-jslib.mjs                # jslib 的 JS 逻辑
client/dotnet/unity-webgl-smoke/prepare.sh /tmp/roost-relay-smoke   # 拷工程 + 打 Unity 包

# 编辑器里跑（SDK 自带的 ClientWebSocket / TCP）：先起服务，再批处理
ROOST_RELAYPEER_BIN=/tmp/relaypeer node client/dotnet/unity-webgl-smoke/serve.mjs /tmp/empty 8788 &
UNITY=/Applications/Unity/Hub/Editor/6000.3.12f1/Unity.app/Contents/MacOS/Unity
ROOST_SMOKE_LOBBY=http://127.0.0.1:8788/lobby ROOST_SMOKE_TRANSPORT=ws ROOST_SMOKE_HOLD=40 \
  "$UNITY" -batchmode -nographics -projectPath /tmp/roost-relay-smoke -executeMethod RelaySmokeEditor.RunInEditor -logFile /tmp/smoke-editor.log
grep SMOKE /tmp/smoke-editor.log

# WebGL（要装 WebGL 模块；2026-11-17 用 6000.7.0b7）：构建，再用 serve.mjs 发构建目录，浏览器开页面
UNITY7=/Applications/Unity/Hub/Editor/6000.7.0b7/Unity.app/Contents/MacOS/Unity
"$UNITY7" -batchmode -nographics -quit -projectPath /tmp/roost-relay-smoke -buildTarget WebGL \
  -executeMethod RelaySmokeEditor.BuildWebGL -logFile /tmp/smoke-webgl-build.log
ROOST_RELAYPEER_BIN=/tmp/relaypeer node client/dotnet/unity-webgl-smoke/serve.mjs /tmp/roost-relay-smoke/Build/WebGL 8789
# 浏览器开 http://127.0.0.1:8789/?hold=40 ，看页面上（或控制台里）的 SMOKE 行
```

看什么：探针各行是否完成（`DID NOT COMPLETE` 表示这个原语在该平台上不跑）；`bridge: ok`（桥接本身能用）；端到端每一步（`lobby created room`、`host connected`、`frames: ok`、`held …: still connected, rtt …`、`left: ok`、`PASS`），卡住时 `stuck at step`；切到别的标签页再回来时的 `application paused / focus` 行与之后连接是否还在。

## 连玩家接入（GateSmoke）

维护者 2026-11-18：Unity / WebGL 只验“经 WebSocket 连进玩家接入、完成鉴权握手”，再只靠 `Pump` 驱动的心跳把连接保持到超过服务端 `idle_timeout` 至少 10s；不做请求、推送、重连——WebSocket 之上的逻辑与 TCP 共用，由 `Roost.Client.Tests` 与 Go 驱动的用例覆盖（[client/README](../../README.md#c-经-websocket-连玩家接入含-unity-webgl)）。

| 文件 | 作用 |
|---|---|
| `Assets/GateSmoke/GateSmoke.cs` | 冒烟本体：`GameClient`（`RoostPlatform.Configure(new ConnectionOptions { WebSocketUrl, … })`，与游戏同样的写法）连上、握手，保持 `hold` 秒，期间连接一断即 `SMOKE FAIL`；到时心跳往返次数（`Connection.HeartbeatRoundTrips`）不少于 `hold/15 - 1`（缺省心跳 15s）则 `SMOKE PASS held …s connected, heartbeat round trips N`。30s 没握手成功也算失败。 |
| `Assets/GateSmoke/GateSmokeBehaviour.cs` | 播放器入口：用 Unity 包的 `RoostClientDriver`（`Attach(client)`，每帧先轮询浏览器桥接再 Pump，与游戏的接法相同）驱动，`LateUpdate` 判定，OnGUI 显示日志。WebGL 读页面参数 `ws`、`hold`；桌面播放器读 `-roostWs`、`-roostHold`。 |
| `Assets/Editor/GateSmokeEditor.cs` | `RunInEditor`（SDK 自带的 `ClientWebSocket`，`PumpDriven` 开，`EditorApplication.update` 里 Pump；环境变量 `ROOST_SMOKE_WS`、`ROOST_SMOKE_HOLD`；不要加 `-quit`）、`BuildWebGL`（输出 `Build/GateWebGL`）。 |
| `serve-gate.mjs` | 起玩家接入对端（`infra/network/gateway` 的 `TestServeAWebSocketPlayerAccessForTheBrowserSmoke`：`DefaultTCPConfig` 缺省值、加 WebSocket 监听、放行 `127.0.0.1:*` / `localhost:*` 来源、任何票据都认），发 WebGL 构建目录；打开 `/` 时自动补 `?ws=<对端>&hold=<idle_timeout + 10>`。目录写 `-` 只起对端。 |
| `check-jslib-frames.mjs` | 不经 Unity、不经浏览器、不要对端：脚本自带最小 WebSocket 服务端，核对包里的 jslib 交出的字节拼回完整、有序的帧（一帧拆成三条消息、三帧合成一条、接近 1 MiB 的大帧），每次 send 恰好一条消息，文本消息之后状态是 3。 |

GateSmoke 与 RelaySmoke 用同一个 `prepare.sh` 打的包（GateSmoke 用其中的 `Roost.Client.dll`、`RoostClientDriver`、`RoostPlatform` 与桥接）。

```sh
node client/dotnet/unity-webgl-smoke/check-jslib-frames.mjs                   # jslib 拼帧
client/dotnet/unity-webgl-smoke/prepare.sh /tmp/roost-gate-smoke              # 拷工程 + 托管包
UNITY7=/Applications/Unity/Hub/Editor/6000.7.0b7/Unity.app/Contents/MacOS/Unity
"$UNITY7" -batchmode -nographics -quit -projectPath /tmp/roost-gate-smoke -buildTarget WebGL \
  -executeMethod GateSmokeEditor.BuildWebGL -logFile /tmp/gate-webgl-build.log
GOWORK=off go test -c -o /tmp/gatepeer ./infra/network/gateway
ROOST_GATEPEER_BIN=/tmp/gatepeer node client/dotnet/unity-webgl-smoke/serve-gate.mjs /tmp/roost-gate-smoke/Build/GateWebGL 8790
# 浏览器开 http://127.0.0.1:8790/ ，等 hold（缺省 idle_timeout 90s + 10s = 100s），看页面上（或控制台里）的 SMOKE 行：
#   SMOKE handshake ok in …ms → SMOKE holding …（每 10s 一行）→ SMOKE PASS held 100s connected, heartbeat round trips N
# 编辑器（ClientWebSocket + PumpDriven）：serve-gate.mjs 打印的 ROOST_SMOKE_WS / ROOST_SMOKE_HOLD 加在下面命令前
"$UNITY7" -batchmode -nographics -projectPath /tmp/roost-gate-smoke -executeMethod GateSmokeEditor.RunInEditor -logFile /tmp/gate-editor.log
```

实测见[证据](../../../docs/maintenance/evidence/csharp-gate-ws-20261118/README.md)。

## 实测结果

2026-11-18 GateSmoke（连玩家接入），详见[证据](../../../docs/maintenance/evidence/csharp-gate-ws-20261118/README.md)：

- **WebGL**（6000.7.0b7，`--managed-only` 托管包，Claude 内置浏览器面板）：`SMOKE PASS held 100s connected, heartbeat round trips 6, handshake 95ms`。对端 `idle_timeout` 是缺省的 90s。
- **编辑器**（6000.7.0b7 batchmode，`ClientWebSocket` + `PumpDriven`）：`SMOKE PASS held 100s connected, heartbeat round trips 6, handshake 193ms`。

2026-11-18（WebGL A1 之后），详见 [证据](../../../docs/maintenance/evidence/sdk-webgl-a1-20261118/README.md)：

- **WebGL（6000.7.0b7，浏览器，`PumpDriven` 开）**：两次 `SMOKE PASS`（停 45s、60s 都没有断线，Rtt 非 0；成员离开 `RoomGone` 为 false，最后一人离开 `RoomGone` 为 true）。探针照旧 6 个不完成——平台限制没变，`PumpDriven` 不依赖它们。同一个构建以 `pump=0` 打开，照旧卡在第一个 lobby 请求（修前的调度）。
- 编辑器与桌面播放器这一轮没有重跑。

2026-11-17（修前），详见 [C1 证据](../../../docs/maintenance/evidence/c1-csharp-websocket-20261117/README.md)：

- **编辑器（6000.3.12f1，Mono，macOS）**：探针全部完成（在线程池线程上）；tcp `SMOKE PASS`；ws 13 次里 8 次 `SMOKE PASS`，5 次成员在心跳整 10 秒点被 relay 关掉连接——原因在 relay 的 WebSocket 写期限（证据里有分析，尚未确定性复现），不是 SDK 或 Unity 的问题。
- **WebGL（6000.7.0b7，浏览器）**：探针里 `Task.Run`、`Task.Delay`、`SemaphoreSlim.WaitAsync(超时)`、`CancelAfter`、`ConfigureAwait(false)` 之后的续行、`ThreadPool.QueueUserWorkItem` **全部没有完成**，只有回到 Unity 上下文的 `await` 完成；`bridge: ok`（jslib + `BrowserRelayTransport` 在浏览器里能连 relay、收发）；端到端卡在第一步（`HttpClient` 的请求已经答 200，但续行没有回来）。结论：**SDK 现有的调度（后台读循环、心跳、`ConfigureAwait(false)`）在 WebGL 上跑不起来**，换传输解决不了，要改调度，见证据里的工作量估计。
