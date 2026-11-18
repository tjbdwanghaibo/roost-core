# C# 客户端经 WebSocket 连玩家接入，Unity WebGL 能联机（2026-11-18）

维护者 2026-11-18 要求：C# `Roost.Client` 能经 WebSocket 连玩家接入（嵌入 TCP 或独立 Gate），让 Unity WebGL 游戏能联机。基线 main `af2d1976`。
做法照开房间 SDK 的先例：可替换传输、`PumpDriven` 单线程调度、浏览器桥接参考写法不进 SDK。先例见[开房间 SDK WebGL A1 证据](../sdk-webgl-a1-20261118/README.md)。

## 结论

1. **SDK**（`client/dotnet/Roost.Client`）：
   - 连接抽成 `IGateTransport`（`GateTransport.cs`），自带 `TcpGateTransport`（缺省，行为不变）与 `WebSocketGateTransport`（`ClientWebSocket`）；
   - `ConnectionOptions` 加 `WebSocketUrl`、`TransportFactory`、`PumpDriven`；`Connection.Pump()`、`Connection.HeartbeatRoundTrips`；
   - 低层 `TcpSession.ConnectAsync(transport, address, …)`；
   - `RoostClientDriver.Attach(client, beforePump)`：WebGL 上每帧先轮询浏览器桥接再 `Pump`。
2. **真实 Go 玩家接入上通过**：嵌入 TCP 的 WebSocket 监听，缺省调度与 `PumpDriven` 单线程调度各跑一遍；独立 Gate 上跑 `PumpDriven`。单线程环境里零违规。
3. **Unity WebGL 构建并在浏览器里实跑通过**：
   - 用 `build-unity.py --managed-only` 的托管包，Unity 6000.7.0b7 构建成功；
   - Claude 内置浏览器面板里握手 95ms；
   - 靠 `Pump` 驱动的心跳保持 100s，超过服务端 `idle_timeout` 90s，期间 6 次心跳往返，`SMOKE PASS`；
   - 同一工程在编辑器 batchmode 里（`ClientWebSocket` + `PumpDriven`）也 `SMOKE PASS`。
4. **先红后绿与变异**：
   - 修前新用例编不过（没有 `IGateTransport`）；
   - 关掉 `PumpDriven` 时单线程用例红在第一步；
   - 7 处变异全部变红，红在对应的断言上。

## 1. 设计

| 项 | 做法 |
|---|---|
| 传输 | `IGateTransport`：`ConnectAsync(address)`、`SendAsync(frame)` 一整帧、`ReceiveAsync` 收字节、`Dispose`。形状照 `IRelayTransport`。差别：发送是 `SendAsync` 而不是同步 `Send`，这样 TCP 的写取消与写超时语义保持原样（写到一半取消就关闭连接）。 |
| 地址 | `WebSocketUrl` 是调用方给的完整 `ws(s)://host:port/path`，不从 TCP 地址推算。维护者 2026-11-18 定：WebGL 登录时在 portal 进服请求带 `"transport":"ws"` 拿这个地址，portal 一侧由另一个 agent 实现。 |
| 握手与代号 | 鉴权首包（连接代号信封）、请求序列、推送、心跳仍在 `TcpSession` / `Connection` 里，传输不看帧。每次连接（含重连）新建一个传输、取一个新代号。 |
| 拼帧 | `PacketCodec.ReadAsync(transport, …)` 按包头拼帧，不看消息边界。发送一帧一次 `SendAsync`，WebSocket 上恰好一条二进制消息。 |
| PumpDriven | `PumpScheduler`：缺省用 `CancelAfter` / `Task.Delay` / `ConfigureAwait(false)`，与以前相同。PumpDriven 时连接 / 请求 / 心跳超时、心跳间隔、重连退避都登记成计时，由 `GameClient.Pump` 开头的 `Connection.Pump` 按单调时钟触发。到期项投到主线程的 `SynchronizationContext` 再执行（`Pump` 不可重入），所有等待回到调用者的上下文。没有 `SynchronizationContext` 时 `ConnectAsync` 拒绝。 |
| 浏览器桥接 | `unity-webgl-smoke/Assets/GateSmoke/BrowserGateTransport.cs`，不进 SDK。JS 一侧与开房间共用 `RoostRelayWebSocket.jslib`（通用的字节队列）。浏览器函数抽成 `IBrowserSocket`，这样 .NET 用例能链接正式源码、用假的浏览器一侧测。`SendAsync` 同步完成：Unity 的 Mono 运行库里，发送锁的异步等待要线程池。 |

## 2. 用例（先红后绿）

**修前的红**：基线 `af2d1976` 的 SDK 加上新用例文件，编不过，原文见 [red-before-compile.txt](red-before-compile.txt)：

```
client/dotnet/Roost.Client.Tests/WebSocketTests.cs(477,53): error CS0246: 未能找到类型或命名空间名“IGateTransport”…
```

修前的 SDK 只有 `TcpClient`，没有任何传输入口，所以红是“API 不存在”。调度上的红用“关掉 `PumpDriven`”代替：关着开关就是修前的调度，那部分代码没改，做法同 relay A1。

`ROOST_CLIENT_SINGLE_THREAD_OFF=1` 时单线程那一轮关着开关跑，两个 Go 用例都红在第一步。原文见 [red-pumpdriven-off.txt](red-pumpdriven-off.txt)：

```
Unhandled exception. System.Exception: connect and request: everything ran on the loop thread; violations: transport ReceiveAsync on thread 4 (thread pool) | transport SendAsync on thread 4 (thread pool) | …
```

**绿**（最终代码）：

- `dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json`：原有全部 PASS（TCP 用例不变），新增两项：
  - `TestWebSocketMessageBoundaries`。假的 WebSocket 玩家接入：
    - 服务端手写 RFC 6455 握手，再交给 `WebSocket.CreateFromStream`；
    - 逐条核对客户端每条消息恰好一帧，请求头里没有 `Origin`；
    - 握手应答拆成两条消息发，应答与推送合成一条发；
    - 鉴权首包带代号信封；文本消息之后连接断开，`LastError` 是 `InvalidDataException`。
  - `TestBrowserBridgeFraming`：`BrowserGateTransport` 正式源码链接进来，在 `PumpDriven` 单线程环境里跑。
    - 假的浏览器一侧按 jslib 的语义喂三种消息：握手应答拆三条、一帧拆成 7 字节一条、三帧合成一条（应答 + 两条推送）、`1 MiB - 3` 字节的大帧；
    - 断言完整、推送顺序 m1、m2，每次 Send 恰好一帧，零违规。
- `ROOST_CLIENT_TEST_DLL=… GOWORK=off go test -count=1 -run '^TestTheCSharpClientWorksOverWebSocket' ./infra/network/gateway`：两项 PASS，约 14s。
  - **嵌入 TCP**（`TestTheCSharpClientWorksOverWebSocket`）：服务端 `idle_timeout` 1s，C# 心跳 250ms。
    - 缺省调度：握手、请求、校时、推送；保持 2.5s 不断线且心跳往返 ≥ 5；服务端关会话后自动重连，新连接代号更大。
    - 探测器自检：`PumpDriven` 关着在单线程环境里连，记到违规。
    - `PumpDriven`：同上全部，另加：
      - 请求超时由 `Pump` 触发（服务端 1.5s 才回，`RequestTimeout` 1s），迟到应答被丢弃、连接保留；
      - 停 `Pump`：服务端按 idle 断开，停着时没有通知，恢复后重连；
      - 连接超时由 `Pump` 触发：对端接 TCP 不升级，300ms，超时之后的续行里调 `Pump` 不报重入。
    - 每一步之后核对零违规。Go 一侧另核对两个会话看到的连接代号非 0 且递增。
  - **独立 Gate**（`…ThroughAStandaloneGate`）：Gate → 进程内 NATS 替身 → Game Ingress，Gate 的 `idle_timeout` 1s。`PumpDriven` 下握手、请求、心跳保持、停 `Pump` 被断开后恢复重连，零违规。
  - `-race` 下同样通过（连同原有的跨 Gate C# 用例）。
- `node client/dotnet/unity-webgl-smoke/check-jslib-frames.mjs`：jslib 的 JS 逻辑。
  - 脚本自带最小 WebSocket 服务端，发：一帧拆三条、三帧合一条、`1 MiB - 3` 字节的大帧、再一帧；
  - 按每次 7 字节和每次 64 KiB 两种取法拼回，逐帧比对；
  - 两次 send 恰好两条单片二进制消息；文本消息之后状态 3。
- 其他：
  - `GOWORK=off go test -count=1 ./infra/network/gateway ./infra/network/wsconn` 与根包 `go test -count=1 .` 通过；
  - 地形 C# 自测通过；relay C# 工程编译通过（relay 代码没改）。
- `scripts/test-client-dotnet.sh` 加跑上面两个 Go 用例与 node 检查，pretag 的 `ROOST_DOTNET_GOLDEN_ONLY` 也跑。

## 3. 变异

用 [run_mutations.py](run_mutations.py) 跑：每次改一处、构建、只跑覆盖它的用例，跑完从备份恢复并逐字节核对。全部输出见 [mutations.txt](mutations.txt)。

| 变异 | 结果 |
|---|---|
| M1 一帧拆成两条消息写（包头一条、载荷一条） | 红：`websocket peer failed during: handshake over a split acknowledgement ---> a client message of 16 bytes is not exactly one frame` |
| M2 `PumpDriven` 下读循环的等待用回线程池 | 红：`connect and request: … violations: a continuation posted to the loop on thread 6 (thread pool) \| transport ReceiveAsync on thread 6 (thread pool) …` |
| M3 `PumpDriven` 下等待（心跳间隔、重连退避）仍用 `Task.Delay` | 红：`heartbeat on Pump: … a continuation posted to the loop on thread 14 (thread pool) …` |
| M4 `GameClient.Pump` 不触发计时 | 红：`heartbeats on Pump kept the connection past idle_timeout: System.IO.EndOfStreamException …`（服务端按 idle 断开） |
| M5 `PumpDriven` 下超时仍用 `CancelAfter` | 红：`request timeout: … a continuation posted to the loop on thread 10 (thread pool)` |
| M6 浏览器桥接 C# 一侧拼帧时丢掉剩余字节 | 红：`Timed out waiting for reply to split` |
| M7 jslib 取了一部分就丢掉这条消息剩下的字节 | 红：`check-jslib-frames: FAIL timed out: 1048988 bytes with 7-byte receives` |

M3 / M5 第一次写成删掉 `if (!PumpDriven)` 时，触发 `CS0162` 无法访问的代码（`TreatWarningsAsErrors`），构建失败，不算结果；改成恒真条件后重跑。

## 4. Unity

维护者 2026-11-18 的要求是 Unity / WebGL 只验连入：

- 只证明能经 WebSocket 连进玩家接入、完成握手，再只靠 `Pump` 的心跳保持超过 `idle_timeout` 至少 10s，并记录心跳往返；
- 请求、推送、重连由上面的 .NET 用例覆盖。

**冒烟**：`unity-webgl-smoke` 新增 GateSmoke（见[冒烟 README](../../../../client/dotnet/unity-webgl-smoke/README.md#连玩家接入gatesmoke)），对端如下：

- `serve-gate.mjs` 起 `TestServeAWebSocketPlayerAccessForTheBrowserSmoke`；
- `DefaultTCPConfig` 的缺省值：`idle_timeout` 90s（`infra/network/gateway/tcp_config.go`），心跳间隔取 SDK 缺省 15s；
- 页面 `hold` 自动取 90 + 10 = 100s。

**构建**：

- `prepare.sh` 拷工程，并用 `build-unity.py --managed-only` 打托管包（`file:` 引用）；
- Unity 6000.7.0b7 `-buildTarget WebGL -executeMethod GateSmokeEditor.BuildWebGL`，`Build Finished, Result: Success.`，见 [webgl-build.txt](webgl-build.txt)；
- 托管包里原生绑定的 C# 源码（`DllImport("roost_client")`）在 WebGL 上参与编译，没有挡住构建，运行时也没走到它。

**浏览器实跑**：Claude 桌面应用内置浏览器面板，Chrome/156 内核，页面可见。全文见 [webgl-smoke.txt](webgl-smoke.txt)：

```
SMOKE handshake ok in 95ms (state Ready)
SMOKE holding 10s/100s, heartbeat round trips 0
…
SMOKE holding 91s/100s, heartbeat round trips 5
SMOKE PASS held 100s connected, heartbeat round trips 6, handshake 95ms
```

handshake 那一行附带的调用栈显示，续行经 `SynchronizationContextAwaitTaskContinuation` → `UnitySynchronizationContext.ExecuteTasks` 回到主线程，没有经线程池。

**编辑器**：同一工程、同一对端，`GateSmokeEditor.RunInEditor`（`ClientWebSocket` + `PumpDriven`），`SMOKE PASS held 100s connected, heartbeat round trips 6, handshake 193ms`，见 [editor-smoke.txt](editor-smoke.txt)。

对端的启动输出见 [serve-gate.txt](serve-gate.txt)。

**收尾**：

- 对端与 `serve-gate.mjs`（PID 85293 / 85285）已停，浏览器标签已关；
- Unity 工程、构建与 `gatepeer` 在 scratchpad，没有进仓库。

## 5. 没验证的部分

已登记在 KNOWN-LIMITS 的[客户端：遗留](../../KNOWN-LIMITS.md#客户端遗留)：

- **IL2CPP 裁剪**：WebGL 走 IL2CPP，冒烟只覆盖握手与心跳，进入游戏后自身视图同步（EntitySync / 生成的 DAO Apply）有没有被代码裁剪影响没验证，留给真实项目接入时验收（维护者 2026-11-18 要求登记）。
- **浏览器**：Safari、Firefox、移动浏览器没测；真实后台标签页没测（内置面板始终 `visible`）。
- **网络**：`wss://` 经真实负载均衡与 TLS 终结没测，跨机没测。
- **播放器**：Unity 桌面播放器、移动端没测。
