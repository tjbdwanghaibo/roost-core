# Roost native 客户端

发送顺序修复 RV-20261101-03：网络线程在与请求准入相同的 mutex 内选择下一包并为心跳取号，实际编码与 socket 操作仍在锁外；保留请求 handle、取消和响应关联，不关闭 Gate 的递增序号校验。[原审查](https://github.com/tjbdwanghaibo/roost-core/blob/30f9261bea91016c365f8c1f4d35d42752f2d48d/docs/maintenance/COMMIT-REVIEW.md)在 main `c31e65cc` 受控插桩复现 `(0,1) → (0,3) → (42,2)`；这不是自然触发频率测量。新增 `native_sequence` CTest 以独立应用线程提交 500 个请求、1ms 心跳，对端逐包验证严格递增，并保留全部响应/错误断言；本机 macOS ARM64 七组 CTest 通过。

C++17 客户端覆盖当前 Core 的 TCP/RS v2、业务 PB、EntitySync、Lockstep 和场景时间域。Unity、UE 与其他宿主复用网络核心，业务状态只在消费线程应用。功能实现与平台验收分开：原 C# 客户端保留，平台状态见末表。

本轮 native 网络 **ABI 2**：事件增加结构化请求状态，Options 增加独立心跳超时。库、头文件和托管绑定一起更新；不兼容旧二进制。C/C++ 宿主先检查 `roost_abi_version()==2`，托管绑定已自动检查。

## 结构与线程

- `src/client.cpp`：每连接一个网络线程，独占 nonblocking socket 的 connect/send/recv/close；Windows 用 Winsock，其他目标用 BSD sockets。通过 CMake 编译静态库或动态插件，不依赖 Unity/UE。
- `include/roost/client.h`：C ABI。结构只含定宽整数、指针与 size_t；不跨 ABI 传 STL、异常或分配器。`roost_poll` 返回的 payload 仅到下一次 Poll/Destroy 有效，跨线程需复制。
- `client.hpp`：C++ 消费线程封装，请求完成/失败回调、通用事件和 Pump。`request_pb` 接收 protoc 生成的消息，不复制业务协议定义。
- `game_client.hpp`：可选完整 Protobuf runtime 的业务入口，typed request/push 路由、DAO 变化/重置通知、Lockstep 路由及可选有界指数退避重连。
- `lockstep.hpp` / `lockstep.h`：输入/哈希/追帧、乱序组装、C9 v2 恢复失败、CA01 分块快照与 SHA-256/业务状态哈希校验；C ABI 无 STL/异常。
- `time_domain.hpp`：场景暂停/倍率/单调时间外推，使用估计的服务端业务时间。
- `network_time.hpp`、`prediction.hpp`、`client_trace.hpp`：显式 Gate 时钟探测、有界表现插值、输入 ACK/重放及客户端应用/展示诊断。[接入与边界](../../docs/framework/NETWORK-EXPERIENCE.md)。新 `roost_clock_probe` 导出需要同步更新 native 库和 Unity 绑定。
- `views.h`：可选 Protobuf 视图 C ABI，加载 protoc descriptor set，输出 Full/Delta/Remove 与应用增量后的完整 PB；供禁用异常的 UE 等宿主使用。
- `sync.hpp`：EntitySync 流/epoch/tick、对象代际与 Subject 版本校验，输出 Full/Delta/Remove。重复帧不通知；失败后等待 Full。
- `protobuf.hpp` / `entity_views.hpp`：可选 Google Protobuf 完整 runtime。按 namespace/profile/encoding/schema 注册 DAO 类型；先解码和应用整批，再替换视图，回调获得只读前后快照。字段包装存在才替换，集合不做 MergeFrom 追加。空 Delta 仅推进版本。
- `unity/NativeClient.cs`：PInvoke；`NativeGameClient.cs` 把 native 事件转换为业务请求回调和现有 C# EntityViews 变化回调。`NativeLockstep.cs` 绑定 native 状态机；`NativeReconnect.cs` 提供可选重连策略。Unity 的 Update 调 Pump，再调重连 Tick。它复用 C++ 网络与既有 C# DAO 解码，不运行第二条 TCP 链路。
- `unreal/RoostClient.Build.cs`：UE 外部库接线参考。把 include 和对应平台库放进该模块；禁用 C++ exception 的 UE 模块使用 C ABI，不直接 include 会抛异常的 C++ 便利层。UE Tick 调 poll 后再访问 UObject。

生命周期与 Poll 只在消费线程使用；C ABI request 支持并发准入，但不得与 Destroy 竞态。C++/C# 便利层在消费线程使用。不要在 C++ Pump 的回调里销毁自身或重入 Pump/Connect；退出 Pump 后再处理生命周期。Dispose/Destroy 会通知网络线程退出并 join，不留下后台连接。

## 请求与失败语义

`roost_connect` 的成功返回只表示启动尝试；READY 才代表握手成功，CLOSED 携带失败原因。每次连接有独立 generation（事件里的连接计数）。鉴权首包另外带**连接代号**信封 `0x00 'G' <8 字节大端代号> <票据>`（2026-11-06）：`roost_connect` 每次取进程内新代号 `max(上一个 + 1, 当前 UTC Unix 纳秒)`，全部 `roost_client` 共用一个递增序列。同一会话跨 Gate 重连时，Game 据此认出迟到完成的旧握手，不让它断开新连接；业务鉴权只看到票据原文。ABI 与托管绑定不变，替换 native 库即生效。票据为空或以 `0x00` 开头时原样发出，由服务端拒绝。布局与金样见[客户端说明](../README.md#鉴权首包的连接代号)，`native_client` 用 Go 金样 `auth_generation` 逐字节核对编码，并核对同一进程两次连接的代号递增。默认由业务明确重连；也可启用 `game_client.configure_reconnect` 或创建 Unity `NativeReconnect`。首次连接失败由业务处理，成功连接后的断线才触发自动策略。默认最多 8 次、1 秒起至 30 秒封顶；每次使用业务异步准备的新 ticket/数值地址。provider 返回空表示等待准备，抛错算一次失败；provider 不能阻塞。提供尝试、成功、耗尽和错误回调。主动离线先停止策略；重新登录/订阅由业务完成，不自动重发请求。移动端进入后台时由引擎断开、恢复前台后重新取票连接，不承诺后台持续保活。

C ABI 请求返回 sequence=0 表示未准入（C++/C# 便利层准入失败抛异常）；已准入的请求以 RESPONSE 或 REQUEST_FAILED 完成，连接级 CLOSED 使所有未完成请求失效。REQUEST_FAILED 的 `status` 区分 `TIMEOUT_UNSENT/TIMEOUT_UNKNOWN/CANCELED_UNSENT/CANCELED_UNKNOWN`；C++ 业务解码失败另为 `INVALID_PAYLOAD`。`roost_cancel` 是尽力取消，响应与取消竞争时只消费一个终态。不能把超时当作服务器没有执行。队列容量包含等响应和已完成但未消费的请求。

事件队列满时关闭连接、清理事件并发一个终态 CLOSED，丢弃当前 Sync 基线；不能丢单条 Delta 后继续运行。这个关闭也使还没派发的响应成为结果未知。网络帧有载荷上限，写入、连接鉴权、心跳和请求均有期限。请求可用 `roost_request_with_timeout` 单独指定从准入开始的期限；心跳间隔和应答期限分别配置。`roost_notify` 支持 PB(kind=0)/Lockstep(kind=2)，`SENT` 仅代表完整写入 socket，不是业务确认。计时使用 steady_clock。socket 由网络线程关闭，主线程只设停止标志；正常 select 唤醒间隔 10ms，不是硬实时关停保证。

端点目前必须是数值 IPv4/IPv6：DNS/地址候选选择由引擎或平台异步解析，避免销毁时卡住不可取消的系统 DNS。移动网络应使用平台解析得到的当前地址（包括 IPv6），不要在游戏逻辑里写死公网 IP。

## 同步与回调

`NativeGameClient` 支持在消费线程的回调内 Dispose；Error、请求成功/失败、Notify、PB 推送、校时和实体事件均逐观察者检查销毁状态。解析器或前一观察者销毁客户端后，不再调用后续业务回调；错误观察者自身抛错不会递归报告。此保证属于 C# 托管层，不改变上文 C++ 对象不能在自身 Pump 中销毁的限制。可复跑验证在 `tests/managed/DisposalTests.cs`，由 NativeInterop 入口执行。

```cpp
roost::client connection;
roost::entity_views entities;
entities.register_dao<roost::dao::sync::SyncPlayerDao>("player", "default");
connection.on_event = [&](const roost::client_event& event) {
    if (event.type == ROOST_CLOSED) { entities.reset(); return; }
    if (event.type != ROOST_PUSH || event.kind != 1) return;
    std::vector<roost::protobuf_change> changes;
    try {
        if (!entities.receive(event.payload.data(), event.payload.size(), changes)) return;
    } catch (...) {
        connection.disconnect(); entities.reset(); throw;
    }
    for (const auto& change : changes) {
        // change.wire.kind: full / delta / remove
        // previous/current: 不可变 PB 完整状态；Remove 的 current 为空。
        // 在此更新引擎展示；Remove 表示不可见，不代表业务实体销毁。
    }
};
```

完整可运行消费者见 `tests/gate.cpp`。DAO schema 与服务端一起生成，不手写通用字段字典。需要完整 Protobuf runtime 的 reflection；不是 protobuf-lite。原业务 PB 从 `protocol/proto/protocol.proto` 生成，DAO 从 `db/dao_sync.proto` 生成；所有目标平台使用匹配版本的 protoc 与 runtime。

DAO 同步要求 `sync_format=3`，map 支持 key 更新/删除，list 支持索引更新/追加，嵌套 struct/map/list 支持递归子字段补丁；全量、密集变化和清空可使用字段替换。服务端、生成 proto 与客户端库必须同时更新，旧格式明确拒绝。语义与回退规则见 [DAO PB 同步](../../docs/framework/DAO-SYNC-PROTOBUF.md)。

Sync 元数据解码与 PB 应用都在消费线程，网络帧解析和 I/O 在网络线程；不以多线程名义同时改引擎状态。视图采用整帧 staging map；大可见集容量仍需按目标设备测量，不能从服务器容量推导客户端容量。

## Lockstep 与引擎接入

每场比赛创建独立 `lockstep_client`，sender 用 `connection.notify(commandID, 2, payload, completion)`，确认通知失败时终止比赛恢复；广播 ID 的 kind=2 推送交给 `receive`，simulate 仅按顺序执行一次。默认乱序预算 256 帧、64 个停滞广播包后重发追帧、快照预算 4MiB（上限 64MiB）。不是墙钟定时重试，也不创建模拟线程或自动执行固定步长。

快照分块只缓存一份；完整 SHA-256 检查通过才调 restore，业务提供恢复后的状态 hash。校验、模拟、恢复或不可服务通知失败使本场终止；不能吞异常继续模拟。重连后重新创建实例并由业务选择追帧起点/恢复策略。低层 assembler 需在应用快照后显式 `resume_after_snapshot(frame)`。Core Go 金样、负 player ID 与 64 位 hash 均有跨语言核对。

Unity 使用 `NativeGameClient`：

```csharp
var options = NativeClient.DefaultOptions;
options.Heartbeat = 15000;
options.HeartbeatTimeout = 5000;
var game = new NativeGameClient(options);
game.Ready += OnReady;
game.Closed += OnClosed;
game.EntityChanged += OnEntityChanged; // Full / Delta / Remove，含前后状态
game.EntitiesReset += ClearScene;
game.Error += OnError;
// game.Subscribe(pushID, Reply.Parser.ParseFrom, OnReply) 返回可 Dispose 的订阅。
// game.Request(reqID, replyID, request.ToByteArray(), Reply.Parser.ParseFrom,
//              OnReply, OnRequestFailed) 返回 sequence，供 game.Cancel(sequence)。
// 每个比赛在 Ready/业务重新入场成功后创建 NativeLockstep，旧实例先 Dispose。
// Update: game.Pump(); 可选 reconnect.Tick();
```

完整可运行用法见 `tests/game.cpp` 与 `tests/managed/Program.cs`：业务 PB、native Lockstep、断线恢复；真实生成 DAO/Gate 见 `tests/gate.cpp`。Unity 沿用 `Roost.Client.SceneTimeDomain` 和已有 EntityViews/PB codec；不重复建网络连接。进入后台时 Stop 重连策略并 Disconnect；恢复前台重新获取票据。销毁前先 Dispose 比赛和重连对象，再 Dispose game。

UE 禁异常模块使用 `client.h` / `lockstep.h` / `views.h`。C ABI 返回值必须检查，错误不跨 ABI 抛出。Lockstep 回调在调用线程同步发生，输入指针仅回调期间有效；禁止在回调中销毁或重入实例。`roost_views_receive` 返回数组有效到下一次 Receive/Reset/Destroy，业务跨线程需复制。DAO descriptor set 由 `protoc --include_imports --descriptor_set_out=dao.desc db/dao_sync.proto` 生成（按项目设置 proto path）；注册 namespace/profile/schema 和完整消息名，与服务端保持一致。返回的是合并后的完整 PB，宿主可直接用生成消息 Parse，不自行 MergeFrom 增量。整包失败清空基线，重新连接/订阅取得 Full。

C++ 便利层要求异常支持；UE 模块可只通过 C ABI 调用已编译的库，不改引擎全局异常设置。`unreal/RoostClient.Build.cs` 仍是外部模块接线；PB 视图库的 Protobuf/Abseil 等依赖必须一同链接或打包，遵循对应平台的 runtime/编译器版本。C ABI 不等于免除这些二进制依赖。

## 构建

macOS/Linux 主机共享库（Unity 动态插件）或静态库（UE/其他宿主）：

```bash
cmake -S client/cpp -B /tmp/roost-client-build -DBUILD_SHARED_LIBS=ON
cmake --build /tmp/roost-client-build --parallel 4
cmake --install /tmp/roost-client-build --prefix /path/to/sdk
# 安装后消费者：find_package(roost_client CONFIG REQUIRED)
# target_link_libraries(game PRIVATE roost::roost_client)
```

需要 C ABI DAO 视图时追加 `-DROOST_WITH_PROTOBUF=ON -DCMAKE_PREFIX_PATH=/path/to/target-protobuf`；跨编译必须使用**目标平台**完整 Protobuf runtime，不把 macOS host 库链接到 Android/iOS。默认基础网络/Lockstep 库不依赖 Protobuf 或 OpenSSL。

Windows 在 MSVC 开发终端用同一 CMake 入口，加 `-A x64`，构建时 `--config Release`；自动链接 ws2_32。Unity 放置 `roost_client.dll`、macOS 的 `libroost_client.dylib` 或 Android `libroost_client.so` 时配置对应平台/架构。

Android（需本机 NDK；API level 按项目最低版本调整）：

```bash
cmake -S client/cpp -B /tmp/roost-android \
  -DCMAKE_TOOLCHAIN_FILE="$ANDROID_NDK_HOME/build/cmake/android.toolchain.cmake" \
  -DANDROID_ABI=arm64-v8a -DANDROID_PLATFORM=android-24 \
  -DBUILD_SHARED_LIBS=ON
cmake --build /tmp/roost-android --parallel 4
```

iOS（完整 Xcode，静态链接；模拟器与设备分别构建，不混架构）：

```bash
cmake -S client/cpp -B /tmp/roost-ios -G Xcode \
  -DCMAKE_SYSTEM_NAME=iOS -DCMAKE_OSX_ARCHITECTURES=arm64 -DBUILD_SHARED_LIBS=OFF
cmake --build /tmp/roost-ios --config Release
```

Unity iOS 绑定使用 `__Internal`，其他平台使用插件名；见 [Unity iOS 原生插件](https://docs.unity.com/en-us/engine/6000.6/manual/platform-specific/iphone/ios-developing/plugins-for-ios/ios-native-plugin-create)。工具链参数按 [Android NDK CMake](https://developer.android.com/ndk/guides/cmake) 和 [CMake Apple/Android 工具链](https://cmake.org/cmake/help/latest/manual/cmake-toolchains.7.html)；SDK、签名、插件导入、移动网络权限及打包由引擎工程负责。

## 验证入口与状态

```bash
bash scripts/test-client-cpp.sh
# 官方完整 Protobuf C++ runtime/protoc 安装前缀，二者版本一致：
ROOST_MANAGED_TESTS=1 ROOST_PROTOBUF_TESTS=ON ROOST_PROTOBUF_PREFIX=/path/to/protobuf bash scripts/test-client-cpp.sh
# 使用相同生成 game-demo、独立 NATS 与正式 Gate/Nest/WAL：
ROOST_GATE_CPP=1 ROOST_PROTOBUF_PREFIX=/path/to/protobuf bash scripts/test-gate-client.sh
# macOS 动态库 PInvoke 冒烟（替换成自己的构建目录）：
DYLD_LIBRARY_PATH=/tmp/roost-client-build dotnet run --project client/cpp/tests/managed/NativeInterop.csproj
```

2026-11-01 本轮（macOS arm64、AppleClang 21、Protobuf 31.1、.NET 10）：

- 完整脚本 5 组通过：Go Lockstep 金样/乱序/追帧/快照/SHA256/时间域，TCP 通知/取消/单请求超时，C++ typed PB/Lockstep/快照/双代重连，C ABI descriptor/Full/Delta/Remove，生成 DAO PB 字段语义。
- 2 组实际托管运行通过：ABI 布局/关闭，与 native PB/模拟/快照恢复反向 PInvoke/重连。不是 Unity Editor/IL2CPP 测试。
- 正式生成 Gate/Nest/WAL 与 C++ 消费者联合验证通过；C ABI 头文件通过 C11 编译，CMake 安装 SDK 被独立工程 `find_package` 后构建运行通过。
- 回环夹具核对客户端成功退出、全部请求路由和心跳；主动关闭允许 EOF/RST。一次 ASan 复跑在客户端所有断言通过后报 `ConnectionResetError(54)`，根因是夹具把正常关闭当错误，已修正该收尾判断，未修改网络实现或删除业务断言。
- ASan/UBSan 的上述 5 组通过，编译启用 `-Wall -Wextra -Werror`。首次启用 `detect_leaks=1` 时 Apple runtime 明确报 `detect_leaks is not supported on this platform`，因此泄漏检查未执行；关闭该不支持项后只记录 ASan/UBSan 结果，不记为 LeakSanitizer 通过。

内存检查可复跑：CMake 配置追加 `-DCMAKE_CXX_FLAGS="-fsanitize=address,undefined -fno-omit-frame-pointer -g"` 与 `-DCMAKE_EXE_LINKER_FLAGS="-fsanitize=address,undefined"`，构建后 `ctest --test-dir <build> --output-on-failure`；macOS 用 `ASAN_OPTIONS=detect_leaks=0`。配置 `ROOST_WITH_PROTOBUF/ROOST_PROTOBUF_TESTS/ROOST_BUILD_TESTS=ON` 及正式 Protobuf prefix。测试不构成网络吞吐或低端设备容量数据。

此前 2026-10-31 本机 macOS arm64：静态/共享构建、Go Sync 金样、分片 TCP/PB/心跳/超时/队列上限、ASan/UBSan、Google Protobuf 31.1 的 DAO 全量/增量/精度/空集合/空引用、C++ Gate 联合链路与实际 .NET→native PInvoke 均通过。使用临时安装的工具链，不把它视为仓库依赖或其他机器的前提路径。

| 目标 | 已提供 | 本轮实际验证 |
| --- | --- | --- |
| macOS arm64 | BSD sockets、静态/动态库、C/C++ 与托管绑定 | 主机功能、Unity Editor、Mono/IL2CPP Player 与容量/延迟分片通过（见下） |
| Linux | BSD sockets 与 CMake | 未编译/实跑 |
| Windows x64 | Winsock 生命周期、DLL 导出、MSVC CMake | 缺 Windows 工具链，未编译/实跑 |
| Android arm64 | NDK CMake 与 Unity 绑定 | 缺 NDK/设备，未编译/实跑 |
| iOS arm64 | CMake 静态库与 __Internal 绑定 | 缺完整 Xcode/设备，未编译/实跑 |
| UE | C ABI 与 External Module 接线 | 未运行 UE 工程 |

不能据此声称“全平台已验收”、网络吞吐优于 C#、低内存设备容量通过。当前实现的是 Core TCP 客户端协议；TLS、WebSocket、UDP/KCP 传输适配未实现，DNS 使用平台异步解析。Unity 正式打包、Driver 和示例见 [Unity 接入](../unity/README.md)；macOS Editor、Mono/IL2CPP Player、可见集容量与 TCP 故障实跑见 [当前报告](../../docs/performance/CLIENT-20261101.md)。UE、移动前后台/真实弱网和设备容量仍需实机验收。
