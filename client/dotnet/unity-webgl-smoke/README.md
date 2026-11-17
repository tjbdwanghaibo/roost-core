# unity-webgl-smoke：Roost.Client.Relay 在 Unity（含 WebGL）上的最小冒烟

交接 C1（维护者 2026-11-17“做一个最小的测试”）：看 `Roost.Client.Relay` 在 Unity 的调度下能不能跑，重点是 **WebGL 单线程**。同时是 WebGL 桥接的参考写法（SDK 不带桥接代码，维护者定）。

## 内容

| 文件 | 作用 |
|---|---|
| `Assets/RelaySmoke/RelaySmoke.cs` | 冒烟本体：①调度原语探针（`Task.Run`、`Task.Delay`、`SemaphoreSlim.WaitAsync(超时)`、`CancelAfter`、`ConfigureAwait(false)` 续行、线程池；对照组是回到 Unity 上下文的 `await`）；②只走桥接（WebGL 下）：`UnityWebRequest` 向 lobby 要票据 → `BrowserRelayTransport` 连 relay → 令牌、Ping → 收 Welcome、Pong，不经 SDK 的调度；③端到端经 SDK：房主建房、成员加入、成员 → 房主 5 帧、房主广播、房间通道、停 `hold` 秒（大于 relay 空闲超时 30s，看心跳）、离开。日志行以 `SMOKE` 开头，最后一行 `SMOKE PASS` / `SMOKE FAIL …`；某一步 `hold + 20` 秒没往下走时报 `SMOKE FAIL stuck at step '…'`。冒烟自己的等待按帧计时，不借助被测的原语。 |
| `Assets/RelaySmoke/BrowserRelayTransport.cs` + `Assets/Plugins/WebGL/RoostRelayWebSocket.jslib` | WebGL 上的 `IRelayTransport` 参考实现：浏览器 WebSocket，JS 把二进制消息排成字节队列，C# 每帧 `BrowserRelayTransport.PollAll()` 轮询（不从 JS 回调 C#，续行在主线程同步执行）。 |
| `Assets/RelaySmoke/UnityWebRequestHandler.cs` | 让 `LobbyClient` 的 `HttpClient` 经 `UnityWebRequest` 发请求（投到主线程）。 |
| `Assets/RelaySmoke/RelaySmokeBehaviour.cs` | 播放器入口：Update 每帧驱动、OnGUI 显示日志、记录切前后台。WebGL 读页面查询参数 `lobby`（缺省同源 `/lobby`）、`hold`；桌面播放器读 `-roostLobby`、`-roostTransport ws|tcp`、`-roostHold`。 |
| `Assets/Editor/RelaySmokeEditor.cs` | 批处理入口：`RunInEditor`（编辑器里直接跑，环境变量 `ROOST_SMOKE_LOBBY`、`ROOST_SMOKE_TRANSPORT`、`ROOST_SMOKE_HOLD`；不要加 `-quit`）、`BuildWebGL`（不压缩、开发版，输出 `Build/WebGL`）、`BuildMac`。 |
| `prepare.sh` | 把工程拷到仓库外的新目录并放进刚编好的 `Roost.Client.Relay.dll`（Unity 会生成 `Library/` 与 `.meta`，不在仓库里打开）。 |
| `serve.mjs` | 起测试对端（真实 relay + lobby，带 WebSocket 监听），发静态目录，`/lobby/*` 同源代理到 lobby。 |
| `check-jslib.mjs` | 不经 Unity、不经浏览器，在 node 里用 Emscripten 替身加载 jslib，连真实对端核对它的 JS 逻辑。 |

`Packages/manifest.json` 只用编辑器自带的内置模块（imgui、jsonserialize、unitywebrequest），不从网上取包。

## 怎么跑

```sh
source ~/.roost-it/roost-dataengine-it/env.sh                       # 可选：房间目录用 Redis
go test -c -o /tmp/relaypeer ./client/relay                         # 对端，serve.mjs / check-jslib.mjs 用 ROOST_RELAYPEER_BIN 复用
node client/dotnet/unity-webgl-smoke/check-jslib.mjs                # jslib 的 JS 逻辑
client/dotnet/unity-webgl-smoke/prepare.sh /tmp/roost-relay-smoke   # 拷工程 + 放 DLL

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

## 实测结果

2026-11-17，详见 [证据](../../../docs/maintenance/evidence/c1-csharp-websocket-20261117/README.md)：

- **编辑器（6000.3.12f1，Mono，macOS）**：探针全部完成（在线程池线程上）；tcp `SMOKE PASS`；ws 13 次里 8 次 `SMOKE PASS`，5 次成员在心跳整 10 秒点被 relay 关掉连接——原因在 relay 的 WebSocket 写期限（证据里有分析，尚未确定性复现），不是 SDK 或 Unity 的问题。
- **WebGL（6000.7.0b7，浏览器）**：探针里 `Task.Run`、`Task.Delay`、`SemaphoreSlim.WaitAsync(超时)`、`CancelAfter`、`ConfigureAwait(false)` 之后的续行、`ThreadPool.QueueUserWorkItem` **全部没有完成**，只有回到 Unity 上下文的 `await` 完成；`bridge: ok`（jslib + `BrowserRelayTransport` 在浏览器里能连 relay、收发）；端到端卡在第一步（`HttpClient` 的请求已经答 200，但续行没有回来）。结论：**SDK 现有的调度（后台读循环、心跳、`ConfigureAwait(false)`）在 WebGL 上跑不起来**，换传输解决不了，要改调度，见证据里的工作量估计。
