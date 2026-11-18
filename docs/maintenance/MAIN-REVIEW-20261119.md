# main 增量审核 · 2026-11-19

截至本次核对，四项 RV-20261118-10～13 的原场景均已修复。另确认一个 Unity relay 取消问题和一个 portal 地址校验遗漏。[运行证据及复跑方法](evidence/main-review-20261119/) · [上轮报告](V156-MAIN-REVIEW-20261118.md)

## 提交范围

上轮 main `bf68b3b8cb4e040ab09eaa6fe3a0970697a05d0d` 是本轮 main `ba1585356f39bdffc2bdecd864d1dc4aea4bd7e3` 的祖先。增量 9 个提交、118 个文件，跨 v1.57.0（`1f3dd221b`）与 v1.57.1（`a2866561e`）。下表均在固定 main 上运行；四项修复都随 `20ad22932` 进入 v1.57.0。审核分支起点为 `bd63e13196a1f33dd0ff16ace8ba964aa97ec89c`。[提交差异](https://github.com/tjbdwanghaibo/roost-core/compare/bf68b3b8cb...ba1585356f)

## 四项复验

| 编号 | 结论 | 实测结果 |
|---|---|---|
| 10 · CORS 端口 | 已修 | `:0443` 和 `:08443` 均在配置阶段拒绝，提示分别为省略默认端口和写 `:8443`。两种规范写法的 OPTIONS 均为 204。[日志](evidence/main-review-20261119/origin.log) |
| 11 · TS Gate 主动断开 | 已修 | 在 Connecting 回调中断开时，取票调用次数为 0，connect 在虚拟时间 0 以 GateClosedError 结束。正常开始取票后断开的对照会中止已开始的请求。[日志](evidence/main-review-20261119/ts.log) |
| 12 · pretag 漏选 SDK 脚本 | 已修 | 原单提交范围用新选择器得到 run_ts=1、run_dotnet=0。C#、TS 各自的单脚本选择和全量 / 文档对照均通过仓内自测。[原提交](evidence/main-review-20261119/scope-real.log) · [自测](evidence/main-review-20261119/scope.log) |
| 13 · portal 接受越界 IPv4 | 已修 | 在生成工程实际 account 配置中，192.0.2.300 被进程校验拒绝，CLI 退出 3；192.0.2.30 与带方括号的合法 IPv6 均退出 0。[日志](evidence/main-review-20261119/portal.log) |

## 新发现

**RV-20261119-01 · P2 · Unity relay 在调用前已取消时仍打开浏览器连接，连接任务挂起。** 游戏把已经取消的 CancellationToken 交给启用 PumpDriven 的 RelayClient.ConnectAsync。SDK 注册的 Dispose 立即执行，BrowserWebSocket.ConnectAsync 随后仍调用 JS Open 并把自身加入轮询列表。由于对象已标记为 disposed，Poll 会直接返回，后续 Dispose 也不会关闭新句柄。使用实际打包的 DLL 和桥接源码，连接超时为 100 ms，持续 Pump 300 ms 后任务仍在等待，Open 1 次、Close 0 次。对照组在 ConnectAsync 返回后立即取消，任务进入 Canceled，Open 和 Close 各 1 次。验收时应同时核对任务结束、已取消调用不再打开句柄，以及正常连接中取消仍可清理。[SDK 注册位置](https://github.com/tjbdwanghaibo/roost-core/blob/ba1585356f39bdffc2bdecd864d1dc4aea4bd7e3/client/dotnet/Roost.Client.Relay/RelayClient.cs#L1077) · [桥接入口](https://github.com/tjbdwanghaibo/roost-core/blob/ba1585356f39bdffc2bdecd864d1dc4aea4bd7e3/client/unity/Runtime/BrowserWebSocket.cs#L96) · [对照日志](evidence/main-review-20261119/relay-cancel.log)

**RV-20261119-02 · P3 · portal 接受缺少方括号的 IPv6 WebSocket 地址。** 新生成工程中，`ws://2001:db8::1:7080/ws` 通过 account 的键和进程配置检查，退出 0；同一字符串交给 WHATWG URL 解析返回 ERR_INVALID_URL，浏览器无法据此建立连接。写成 `ws://[2001:db8::1]:7080/ws` 后配置和客户端均通过。数字主机检查仅对以左方括号开头的输入走 IPv6 解析，其余路径能将含冒号的主机作为普通名称返回。旧 main 的同一校验函数也接受该地址，因此这是新确认的既有问题。验收应在服务配置阶段拒绝缺少方括号的地址，保留规范 IPv6。[主机解析](https://github.com/tjbdwanghaibo/roost-core/blob/ba1585356f39bdffc2bdecd864d1dc4aea4bd7e3/infra/network/httpserver/browser_host.go#L26) · [生成工程](evidence/main-review-20261119/portal.log) · [新旧对照](evidence/main-review-20261119/host-history.log)

## 其他增量与边界

- Unity 的 WebGL 桥接进入托管包，RoostPlatform 负责传输选择。main 的 C# 自测、Gate / relay 桥接对照、JS 分帧用例和 Linux 托管打包通过。v1.57.1 涉及 Unity 静态成员标记，已核对版本条件和发布差异。未在 Unity Editor、IL2CPP 或真机重跑。[日志](evidence/main-review-20261119/unity.log)
- C++ send_stall 夹具改为由客户端结束事件协调。Linux 回环上原弱网脚本连续三轮通过，每轮五个模式，发送停滞均识别为发送超时。直接编译了测试目标，未运行 CMake 配置或跨平台矩阵。[日志](evidence/main-review-20261119/weak.log)
- S1、S2 是性能证据更新。本轮复算 S2 两份原始 JSONL，分别有 16、8 次运行，五类错误计数总和均为零。未在原 Apple M5 或本机重做性能测量，未复跑全仓 pretag 及 Mongo、NATS 等生产依赖。源码定位使用 Git 差异与精确源文件，当前环境没有代码图索引。[复算](evidence/main-review-20261119/perf.log)
