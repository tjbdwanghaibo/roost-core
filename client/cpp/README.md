# Roost native 客户端

当前 main 新增的 C++17 共享网络核心。Unity、UE 与其他引擎复用同一份 socket / RS v2 / 请求关联 / 心跳代码，不把引擎对象传进网络线程。原 C# 客户端保留；native 不是已验证的全平台替代品，平台状态见末表。

## 结构与线程

- `src/client.cpp`：每连接一个网络线程，独占 nonblocking socket 的 connect/send/recv/close；Windows 用 Winsock，其他目标用 BSD sockets。通过 CMake 编译静态库或动态插件，不依赖 Unity/UE。
- `include/roost/client.h`：C ABI。结构只含定宽整数、指针与 size_t；不跨 ABI 传 STL、异常或分配器。`roost_poll` 返回的 payload 仅到下一次 Poll/Destroy 有效，跨线程需复制。
- `client.hpp`：C++ 消费线程封装，请求完成/失败回调、通用事件和 Pump。`request_pb` 接收 protoc 生成的消息，不复制业务协议定义。
- `sync.hpp`：EntitySync 流/epoch/tick、对象代际与 Subject 版本校验，输出 Full/Delta/Remove。重复帧不通知；失败后等待 Full。
- `protobuf.hpp` / `entity_views.hpp`：可选 Google Protobuf 完整 runtime。按 namespace/profile/encoding/schema 注册 DAO 类型；先解码和应用整批，再替换视图，回调获得只读前后快照。字段包装存在才替换，集合不做 MergeFrom 追加。空 Delta 仅推进版本。
- `unity/NativeClient.cs`：PInvoke；`NativeGameClient.cs` 把 native 事件转换为业务请求回调和现有 C# EntityViews 变化回调。Unity 的 Update 调 Pump。它复用 C++ 网络与既有 C# DAO 解码，不运行第二条 TCP 链路。
- `unreal/RoostClient.Build.cs`：UE 外部库接线参考。把 include 和对应平台库放进该模块；禁用 C++ exception 的 UE 模块使用 C ABI，不直接 include 会抛异常的 C++ 便利层。UE Tick 调 poll 后再访问 UObject。

生命周期与 Poll 只在消费线程使用；C ABI request 支持并发准入，但不得与 Destroy 竞态。C++/C# 便利层在消费线程使用。不要在 C++ Pump 的回调里销毁自身或重入 Pump/Connect；退出 Pump 后再处理生命周期。Dispose/Destroy 会通知网络线程退出并 join，不留下后台连接。

## 请求与失败语义

`roost_connect` 的成功返回只表示启动尝试；READY 才代表握手成功，CLOSED 携带失败原因。每次连接有独立 generation。重连由业务明确发起，重新取得 ticket、恢复登录/订阅；不自动重发请求。移动端进入后台时由引擎断开、恢复前台后重新取票连接，不承诺后台持续保活。

请求返回 sequence=0 表示未准入；已准入的请求以 RESPONSE 或 REQUEST_FAILED 完成，连接级 CLOSED 使所有未完成请求失效。REQUEST_FAILED 的原因载荷区分发送前过期与结果未知；不能把超时当作服务器没有执行。队列容量包含等响应和已完成但未消费的请求。

事件队列满时关闭连接、清理事件并发一个终态 CLOSED，丢弃当前 Sync 基线；不能丢单条 Delta 后继续运行。这个关闭也使还没派发的响应成为结果未知。网络帧有载荷上限，写入、连接鉴权、心跳和请求均有期限。计时使用 steady_clock。socket 由网络线程关闭，主线程只设停止标志；正常 select 唤醒间隔 10ms，不是硬实时关停保证。

端点目前必须是数值 IPv4/IPv6：DNS/地址候选选择由引擎或平台异步解析，避免销毁时卡住不可取消的系统 DNS。移动网络应使用平台解析得到的当前地址（包括 IPv6），不要在游戏逻辑里写死公网 IP。

## 同步与回调

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

Sync 元数据解码与 PB 应用都在消费线程，网络帧解析和 I/O 在网络线程；不以多线程名义同时改引擎状态。视图当前采用整帧 staging map，尚未做大可见集的客户端容量测试。Lockstep 包只保留 kind 和原始载荷透传，原 C# 的 Lockstep 追帧/检查点恢复尚未移植；native 当前面向业务 PB + EntitySync，不宣称完整 C# 功能等价。

## 构建

macOS/Linux 主机共享库（Unity 动态插件）或静态库（UE/其他宿主）：

```bash
cmake -S client/cpp -B /tmp/roost-client-build -DBUILD_SHARED_LIBS=ON
cmake --build /tmp/roost-client-build --parallel 4
```

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
ROOST_PROTOBUF_TESTS=ON ROOST_PROTOBUF_PREFIX=/path/to/protobuf bash scripts/test-client-cpp.sh
# 使用相同生成 game-demo、独立 NATS 与正式 Gate/Nest/WAL：
ROOST_GATE_CPP=1 ROOST_PROTOBUF_PREFIX=/path/to/protobuf bash scripts/test-gate-client.sh
# macOS 动态库 PInvoke 冒烟（替换成自己的构建目录）：
DYLD_LIBRARY_PATH=/tmp/roost-client-build dotnet run --project client/cpp/tests/managed/NativeInterop.csproj
```

2026-10-31 本机 macOS arm64：静态/共享构建、Go Sync 金样、分片 TCP/PB/心跳/超时/队列上限、ASan/UBSan、Google Protobuf 31.1 的 DAO 全量/增量/精度/空集合/空引用、C++ Gate 联合链路与实际 .NET→native PInvoke 均通过。使用临时安装的工具链，不把它视为仓库依赖或其他机器的前提路径。

| 目标 | 已提供 | 本轮实际验证 |
| --- | --- | --- |
| macOS arm64 | BSD sockets、静态/动态库、C/C++ 与托管绑定 | 上述主机功能通过；Unity Editor 未运行 |
| Linux | BSD sockets 与 CMake | 未编译/实跑 |
| Windows x64 | Winsock 生命周期、DLL 导出、MSVC CMake | 缺 Windows 工具链，未编译/实跑 |
| Android arm64 | NDK CMake 与 Unity 绑定 | 缺 NDK/设备，未编译/实跑 |
| iOS arm64 | CMake 静态库与 __Internal 绑定 | 缺完整 Xcode/设备，未编译/实跑 |
| UE | C ABI 与 External Module 接线 | 未运行 UE 工程 |

不能据此声称“全平台已验收”、网络吞吐优于 C#、低内存设备容量通过。TLS、WebSocket、UDP/KCP、DNS 自动解析、自动退避重连、Lockstep 状态机、Unity/UE 完整游戏展示与移动端生命周期实测不在当前 native 实现中。
