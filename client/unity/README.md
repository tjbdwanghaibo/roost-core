# Unity 客户端接入

## 两条接入路线

**缺省走托管路线**（维护者 2026-11-17）：`GameClient` + `RoostClientDriver`。网络、心跳、重连与 EntitySync 都在 C#（`Roost.Client`）里；业务把服务端导出目录（`roost.yaml` 的 `client_export`，见[客户端手册“前后端公共协议目录”](../README.md#前后端公共协议目录client_export)）的 `csharp/` 用 `roost client sync` 同步进 Unity 工程直接编译，再引用 `Roost.Client`、`Roost.Client.Codecs` 与 Google.Protobuf 3.31.1。业务工程不需要原生插件，也不需要 C++ 构建环境。在持续存在的 GameObject 上加 `RoostClientDriver`，`Attach(client)` 后它每帧在主线程 `Pump`、销毁时 `Dispose`；连接用 `client.Connection.ConnectAsync`，用法见[客户端手册“标准业务入口”](../README.md#标准业务入口)。

**原生路线是可选的**：`RoostNativeDriver` + `NativeGameClient`，网络内核来自 `client/cpp`。适合要原生网络内核（例如网络线程与 Unity 主线程隔离、移动端原生 socket 行为），或要与 C++ 客户端行为一致的项目；代价是要按平台构建并导入原生插件（下文“构建可导入包”），插件、绑定和 DLL 必须一起更新。业务 PB 与 EntitySync 在两条路线上都复用 `Roost.Client` 与同一份导出的 C#。

同一连接只选一条路线，不要两个 Driver 同时消费。下文“构建可导入包”“业务接线”是原生路线的步骤。

### 托管路线连 WebSocket 与 Unity WebGL（2026-11-18）

托管路线可以经 WebSocket 连玩家接入（嵌入 TCP 或独立 Gate 的 `websocket` 监听）：登录时在 portal 的进服请求里带 `"transport":"ws"`，把返回的完整 `ws(s)://` 地址填进 `ConnectionOptions.WebSocketUrl`，编辑器、桌面与移动端用 SDK 自带的 `ClientWebSocket`，其余用法不变。**WebGL 只能走托管路线**（浏览器里没有原生插件与套接字），额外两步：

1. `ConnectionOptions.PumpDriven = true`：SDK 不用线程池与计时器，超时、心跳、重连退避都由每帧的 `Pump` 驱动；在主线程上创建、连接。
2. 传输用浏览器 WebSocket：把 [unity-webgl-smoke](../dotnet/unity-webgl-smoke/README.md) 的 `Assets/GateSmoke/BrowserGateTransport.cs` 与 `Assets/Plugins/WebGL/RoostRelayWebSocket.jslib` 拷进游戏工程（SDK 不带桥接代码），`TransportFactory = () => new BrowserGateTransport()`，并用 `driver.Attach(client, BrowserGateTransport.PollAll)` 让 `RoostClientDriver` 每帧先轮询桥接再 `Pump`。

完整示例、不 `Pump` 的后果与验证见[客户端手册“C# 经 WebSocket 连玩家接入”](../README.md#c-经-websocket-连玩家接入含-unity-webgl)。`--managed-only` 打出的包够 WebGL 用：2026-11-18 用它构建 `unity-webgl-smoke` 的 WebGL 版（Unity 6000.7.0b7），结果见[证据](../../docs/maintenance/evidence/csharp-gate-ws-20261118/README.md)。包里原生绑定的 C# 源码在 WebGL 上也参与编译，只要不调用原生路线就不会去找原生库。

## 原生路线

正式网络内核来自 `client/cpp`，Unity 主线程通过 `RoostNativeDriver` 消费事件；业务 PB 和 EntitySync 复用 `Roost.Client` 与项目生成的 DAO codec。不会启动第二条 C# TCP 连接。网络 ABI 为 2，插件、绑定和 DLL 必须一起更新。

## 构建可导入包

**托管路线**只要 .NET SDK（不要 CMake、C++ 编译器、protoc）：

```bash
python3 scripts/client/build-unity.py --managed-only --output /tmp/roost-unity-sdk
```

包里是 `Roost.Client.dll`、`RoostClientDriver` 等主线程 Driver、原生绑定的 C# 源码（`RoostNativeDriver` 引用它，没有插件时只在真用原生路线时报找不到库）、IL2CPP 保留声明与稳定 GUID；没有 native 插件和原生示例，`build-info.json` 的 `target` 是 `managed`。`Roost.Client.Codecs`、Google.Protobuf 与导出的 C# 仍按上文放进业务工程。

**原生路线**在仓库根目录执行，要求 CMake、C++17 编译器、.NET SDK 和 protoc。输出目录必须是新目录，脚本不修改业务 Unity 工程。

```bash
python3 scripts/client/build-unity.py --target macos --output /tmp/roost-unity-sdk \
  --protoc /path/to/protoc
```

得到 `/tmp/roost-unity-sdk/com.roost.client/package.json`；Unity Package Manager 用 **Add package from disk** 选择它。包包含托管 DLL、原生绑定、插件导入规则、主线程 Driver、IL2CPP 保留声明、稳定 GUID 和源码指纹。不要直接导入仓库中未经构建的 `client/unity`：那里不包含 DLL、native 插件和示例生成物。

当前打包入口支持 macOS ARM64、Windows/Linux x86_64 宿主构建、Android ARM64（`ANDROID_NDK_HOME`）和 iOS ARM64（Xcode/static）。移动包可用 `--merge-plugins-from /path/to/host/com.roost.client` 合入同源码的 Editor 插件；指纹不同会拒绝。构建入口存在不等于目标平台已验收；当前实跑范围见[报告](../../docs/performance/CLIENT-20261101.md)。iOS 使用 `__Internal`，Android 网络权限、设备签名及应用生命周期仍由业务工程负责。

## 业务接线

1. 在持续存在的 GameObject 上添加 `RoostNativeDriver`。业务先异步完成 DNS、账号取票，传入数值 IP 和当前票据。每次连接（包括重连）时，native 库自动在鉴权首包里加上新的连接代号（[鉴权首包的连接代号](../README.md#鉴权首包的连接代号)），业务不用处理。旧包要重新打包才带代号。
2. 创建 `NativeGameClient`，注册 PB 回复/推送 parser，以及 EntityViews 的 DAO/profile/schema codec；使用生成的 `Parser.ParseFrom(payload).Apply(previous)` 合并增量。Full/Delta/Remove 通过 `EntityChanged` 通知，不能将 delta 当完整 DAO 覆盖。
3. 可选创建 `NativeReconnect`，再 `driver.Attach(game, reconnect)`、`game.Connect(...)`。Attach 将所有权交给 Driver，不要另外调用 Pump/Tick。每帧回调预算默认 64，可在 Inspector 调整；回调在 Unity 主线程执行。
4. 收到业务请求失败/超时按业务幂等规则处理；超时可能已经执行，重连不自动重发。重连票据 provider 只读取异步准备好的新票据，不能阻塞主线程。首次连接失败由业务处理。
5. Driver 禁用或进入后台时停止重连并断开；重新启用/回到前台发出 `ResumeRequested`，由业务重新取票登录。不会重用旧会话自动恢复。销毁/主动退出调用 `Detach`，先释放依赖 game 的 Lockstep 对象。禁止在后台或禁用状态 Attach。

业务 PB 的 `.proto` 是服务端协议源；C# 用服务端导出目录里 protoc 31.1 生成的那份（与 Google.Protobuf 3.31.1 同版本），客户端不再自己跑 protoc。SDK 不反射猜测业务路由。DAO 生成/回调语义和 C# API 见 [客户端手册](../README.md) 与 [native 手册](../cpp/README.md)。

## 完整可运行验收场景

包 Samples 中的 Native Session 使用 SDK 回环夹具路由，包含 PB 请求/推送、EntitySync 全量/零值与空集合增量/删除、Lockstep 输入与快照、断线重连。它不是账号服务示例；正式账号/Gate/WAL/Mongo 链路由 `scripts/test-gate-real.sh` 验证。

导入示例需要 Google.Protobuf 3.31.1 的 netstandard2.0 DLL 和 System.Runtime.CompilerServices.Unsafe 4.5.2 的 netstandard2.0 DLL；Unity 6 自带 System.Memory，不要重复拷入。业务应统一项目依赖版本，避免重复程序集。可创建独立验收工程：

```bash
python3 scripts/client/unity-project.py \
  --package /tmp/roost-unity-sdk/com.roost.client --project /tmp/roost-unity-check \
  --protobuf-dll /path/to/Google.Protobuf.dll \
  --unsafe-dll /path/to/System.Runtime.CompilerServices.Unsafe.dll
UNITY=/Applications/Unity/Hub/Editor/6000.3.12f1/Unity.app/Contents/MacOS/Unity
"$UNITY" -batchmode -nographics -quit -projectPath /tmp/roost-unity-check \
  -executeMethod UnityAcceptanceBuild.Run -logFile /tmp/roost-unity-build.log
# 用相同入口构建 IL2CPP，先等前一个 Unity 进程结束。
ROOST_UNITY_BACKEND=il2cpp "$UNITY" -batchmode -nographics -quit \
  -projectPath /tmp/roost-unity-check -executeMethod UnityAcceptanceBuild.Run \
  -logFile /tmp/roost-unity-il2cpp-build.log
ROOST_TEST_SYNC=1 python3 client/cpp/tests/game.py \
  /tmp/roost-unity-check/Build/RoostNative-mono.app/Contents/MacOS/RoostNative \
  -batchmode -nographics -logFile /tmp/roost-unity-player.log -roostPort
```

构建方法先在 Editor 加载插件检查 ABI/网络 worker，再生成场景并构建 ARM64 Development Player。架构通过 [Unity 的平台构建设置](https://github.com/Unity-Technologies/UnityCsReference/blob/master/Editor/Mono/BuildPipeline/DesktopStandaloneBuildWindowExtension.cs)指定，与包内 ARM64 插件保持一致。示例按钮用于手动连接夹具；实际游戏请替换消息 ID、票据获取与业务模型。容量与弱网复跑命令见性能报告。
