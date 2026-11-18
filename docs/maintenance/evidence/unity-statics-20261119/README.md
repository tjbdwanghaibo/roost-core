# Unity 包静态字段标注（UAL0010 / UAL0013）· 2026-11-19

来源：客户端模板会话报告，core 的 `BrowserWebSocket.cs` 有两个静态字段（`Live`、`JsLib.Instance`）没标 `[NoAutoStaticsCleanup]`。模板工程关闭了“进入 Play Mode 时重载域”，Unity 6000.7 的分析器对没标注的静态成员报 UAL0010（类型级）/ UAL0013（字段级）；模板 AGENTS.md 把它们当编译错误处理。同步进工程的文件按规则不手改，所以在 core 修。

## 修法

- `client/unity/Runtime/BrowserWebSocket.cs`：`Live`、`JsLib.Instance` 标 `[NoAutoStaticsCleanup]`。它们只在 WebGL 播放器里有内容（那里没有 Play Mode），跨 Play 保留是对的。
- `client/cpp/unity/NativeLockstep.cs`（打包后在 `Runtime/Native/`）：交给 native 的三个反向回调委托 `send`、`simulate`、`restore` 同样标注，它们必须一直活着。
- 特性 `Unity.Scripting.LifecycleManagement.NoAutoStaticsCleanupAttribute` 本机只有 6000.7.0b7 有（6000.3.12f1 的 UnityEngine 里没有），包声明支持到 2021.3，所以用 `#if UNITY_6000_7_OR_NEWER` 包住 `using` 与特性；.NET 测试工程不定义这个宏，照常编译。
- 包里其余 C#（`RoostPlatform`、`RoostClientDriver`、`UnityWebRequestLobbyHttp`、`NativeClient`、`NativeReconnect` 等）没有静态字段 / 自动属性 / 事件，分析器没有报。

## 验证（Unity 6000.7.0b7，batchmode）

探针工程：`client/dotnet/unity-webgl-smoke/prepare.sh` 生成的工程，把包（`build-unity.py --managed-only` 产物）整体挪进 `Assets/RoostPkg`（模拟模板把同步文件放进 `Assets/` 的做法），`ProjectSettings/EditorSettings.asset` 用模板的（`m_EnterPlayModeOptionsEnabled: 1`、`m_EnterPlayModeOptions: 1`）。

| 步骤 | 命令 | 结果 |
| --- | --- | --- |
| 修前（main 20ad2293 的两个文件） | `Unity -batchmode -nographics -quit -projectPath <探针>` | 退出码 0；包内 8 行分析器输出，见 [red.log](red.log)（路径已换成仓库路径；`Runtime/Native/NativeLockstep.cs` 对应 `client/cpp/unity/NativeLockstep.cs`） |
| 修后 | 同上 | 退出码 0；包内没有 UAL / CS 输出，见 [green.log](green.log) |
| .NET | `dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json` | 退出码 0 |

没验证：Unity 6000.4～6000.6 是否已有这个特性（本机没装；宏按 6000.7 起算，更早版本不加特性，分析器若存在会照旧提示）；2021.3 上编译（`#if` 排除，未实跑）。冒烟工程自己的 `Assets/Editor/*SmokeEditor.cs` 也有同类提示，它们不随包分发，没改。
