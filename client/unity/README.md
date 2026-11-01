# Unity 客户端接入

正式网络内核来自 `client/cpp`，Unity 主线程通过 `RoostNativeDriver` 消费事件；业务 PB 和 EntitySync 复用 `Roost.Client` 与项目生成的 DAO codec。不会启动第二条 C# TCP 连接。网络 ABI 为 2，插件、绑定和 DLL 必须一起更新。

## 构建可导入包

在仓库根目录执行，要求 CMake、C++17 编译器、.NET SDK 和 protoc。输出目录必须是新目录，脚本不修改业务 Unity 工程。

```bash
python3 scripts/client/build-unity.py --target macos --output /tmp/roost-unity-sdk \
  --protoc /path/to/protoc
```

得到 `/tmp/roost-unity-sdk/com.roost.client/package.json`；Unity Package Manager 用 **Add package from disk** 选择它。包包含托管 DLL、原生绑定、插件导入规则、主线程 Driver、IL2CPP 保留声明、稳定 GUID 和源码指纹。不要直接导入仓库中未经构建的 `client/unity`：那里不包含 DLL、native 插件和示例生成物。

当前打包入口支持 macOS ARM64、Windows/Linux x86_64 宿主构建、Android ARM64（`ANDROID_NDK_HOME`）和 iOS ARM64（Xcode/static）。移动包可用 `--merge-plugins-from /path/to/host/com.roost.client` 合入同源码的 Editor 插件；指纹不同会拒绝。构建入口存在不等于目标平台已验收；当前实跑范围见[报告](../../docs/performance/CLIENT-20261101.md)。iOS 使用 `__Internal`，Android 网络权限、设备签名及应用生命周期仍由业务工程负责。

## 业务接线

1. 在持续存在的 GameObject 上添加 `RoostNativeDriver`。业务先异步完成 DNS、账号取票，传入数值 IP 和当前票据。
2. 创建 `NativeGameClient`，注册 PB 回复/推送 parser，以及 EntityViews 的 DAO/profile/schema codec；使用生成的 `Parser.ParseFrom(payload).Apply(previous)` 合并增量。Full/Delta/Remove 通过 `EntityChanged` 通知，不能将 delta 当完整 DAO 覆盖。
3. 可选创建 `NativeReconnect`，再 `driver.Attach(game, reconnect)`、`game.Connect(...)`。Attach 将所有权交给 Driver，不要另外调用 Pump/Tick。每帧回调预算默认 64，可在 Inspector 调整；回调在 Unity 主线程执行。
4. 收到业务请求失败/超时按业务幂等规则处理；超时可能已经执行，重连不自动重发。重连票据 provider 只读取异步准备好的新票据，不能阻塞主线程。首次连接失败由业务处理。
5. Driver 禁用或进入后台时停止重连并断开；重新启用/回到前台发出 `ResumeRequested`，由业务重新取票登录。不会重用旧会话自动恢复。销毁/主动退出调用 `Detach`，先释放依赖 game 的 Lockstep 对象。禁止在后台或禁用状态 Attach。

业务 PB 的 `.proto` 是服务端协议源，客户端用 protoc 生成 C# 并引入匹配的 Google.Protobuf。SDK 不反射猜测业务路由。DAO 生成/回调语义和 C# API 见 [客户端手册](../README.md) 与 [native 手册](../cpp/README.md)。

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
