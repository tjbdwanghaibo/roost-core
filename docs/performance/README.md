# 性能验证与复跑

性能报告按版本、场景和参数保留；通过仅对该次实际负载成立。业务功能验收归 [release](../release/v1.26.0-VALIDATION.md)，未关闭问题归 [已知限制](../maintenance/KNOWN-LIMITS.md)。

| 报告 | 范围与结论 |
| --- | --- |
| [性能四项（2026-10-15）](PERF-ROUND-20261015.md) | 长尾约 60% 集中在每分钟第 2 秒，与宿主 MDM / CorpLink 周期任务同秒，未归因到代码；nestwal 编码去反射保留（编码 −9%～−63%）；Remote 80 TPS×30m、Saga 正向 20/s 与全补偿 10/s 各 10m 通过；AOI 4 并行度（GOMAXPROCS=4、short 4）重档 30 分钟通过（旧样本约 803 秒失败，不做单变量归因） |
| [Nest 消息热路径优化](NEST-HOT-PATH-20261013.md) | 六项局部优化、96 组同机矩阵及异常交替复核；微基准不等于游戏混合负载容量 |
| [v1.27.0 重档 30 分钟复测](V1270-HEAVY-30M-20261014.md) | 2026-10-14 同参数延长到 30 分钟通过：业务 P99 12.49ms、Sync 提交→客户端 P99 14.71ms、Gate 往返 P99 6.15ms，>50ms delta 0.0038%；堆与 goroutine 不随时间增长 |
| [v1.26.0 重档 15 分钟复测](V1260-HEAVY-20261013.md) | 2026-10-13 发布后同参数重档通过：业务 P99 12.24ms、Sync 提交→客户端 P99 14.24ms、Gate 往返 P99 6.04ms，>50ms delta 0.0098% |
| [2026-10-12 新增提交复验](REVIEW-20261012.md) | 配置快照/基准夹具修复与局部性能；15分钟重档目标在约6分钟出现Gate通道超时，未通过 |
| [v1.24.0 稳定基线](STABLE-v1.24.0.md) | Nest、Sync、DataEngine、Remote/Saga 的历史验收；不代替新增 Gate 验收 |
| [Gate / AOI 混合负载](GATE-AOI.md) | 1000玩家、10000实体、10000消息/s；2026-10-09常规通过、4并行度重档失败；2026-10-10快8/慢8/默认并行度重档15分钟通过；2026-10-15 4并行度重档30分钟通过 |
| [Nest 调度对照](NEST-DISPATCH-BENCHMARK.md) | ID hash / ID tail、10～500μs handler 与 job 布局；微基准不能直接换算真实服务器容量 |

## 证据存放

- [benchmarks](benchmarks/)：随仓的精简原始样本与结果摘要，保留环境、参数及失败边界。
- 根目录 `artifacts/perf/`：本地完整日志、profile、二进制、源码差异和高频采样，由 `.gitignore` 排除；全新克隆不含这些文件。
- 历史报告中的 `/private/tmp/` 路径是当时的本地证据位置，不承诺在其他机器可访问。

不删除失败来留下全绿结果，不把中断、仅编译、沙箱拒绝连接计为性能通过。不同参数、硬件或夹具版本不得直接归因比较。Sync按P99≤50ms验收，保留max与超过50ms数量；on_change指一个Nest handler完成后统一处理变化，20Hz为兜底。

## 复跑入口

混合负载使用 [nest-game.sh](../../scripts/perf/nest-game.sh)。最新重档参数如下；私有NATS连接、独立标签及其他准备步骤见[完整报告](GATE-AOI.md)。不要指向共享生产资源。

```sh
ROOST_PERF_CPU=0 ROOST_PERF_COUNT=1 \
ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_IO_WORKERS=8 \
ROOST_NEST_GAME_SHORT_QUEUE=65536 ROOST_NEST_GAME_PLAYERS=1000 \
ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 \
ROOST_NEST_GAME_GATE=1 ROOST_NEST_GAME_SYNC=1 \
ROOST_NEST_GAME_DURATION=15m ROOST_NEST_GAME_HZ=10 ROOST_NEST_GAME_DIRTY=5 \
ROOST_NEST_GAME_RACE=0 ROOST_NEST_GAME_PROFILE=0 \
bash scripts/perf/nest-game.sh
```

运行前另设唯一 `ROOST_PERF_LABEL` 和私有 `ROOST_DATAENGINE_IT_NATS_URL`。`ROOST_PERF_CPU=0`保留Go自动选择，不代表运行时只有零个逻辑处理器；记录实际GOMAXPROCS。race、profile与索引刷新不要并跑正式延迟测量。其他模块命令及微基准见各报告，本文档整理未重新执行压测。

**读结果时分清压测端与服务端。** `Messages` / `Heartbeats` 的 `Completion` 从“计划时刻”起算、`QueueWait` 从“投递时刻”起算，两者都混有压测框架自己的等待，保留它们只为与历史样本同口径比较（P99≤50ms 门槛仍按 `Completion`，偏保守）。同一段时间按下面拆开：

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `Harness.AdmitLag` | 计划时刻 → ticker 投递 | 压测端：发生器只在 Nest 10ms tick 里批量投递，天然 0～10ms；ticker 迟到也记在这里 |
| `Harness.ClientQueue` | 投递 → 模拟客户端写出请求 | 压测端：每个玩家连接一次只有一条在途请求，含等前一条回包；直接投递（heartbeat、无 Gate）恒为 0 |
| `Server.QueueWait` | 请求写出 → handler 开始 | 服务端：Gate、NATS、Game 接入、Nest 排队 |
| `Handler` | handler 本身 | 服务端 |
| `Server.Completion` | 请求写出 → Entity 释放 | 服务端合计，评估框架延迟看这一项 |

`GateRoundTrip` 是客户端写出到收到回包（服务端为主）。Sync 的 `PlannedToClient` 从计划时刻起算、含压测端等待；`ChangedToClient` / `CommittedToClient` 从 handler 改值 / 提交起算，是服务端到客户端的同步延迟。2026-10-15 之前的样本没有这几项拆分。

长尾归因时在上面命令前加 `ROOST_NEST_GAME_TAIL=metrics`（每秒 runtime/metrics 与 rusage，开销小）或 `ROOST_NEST_GAME_TAIL=trace`（另开 Go flight recorder，超过 `ROOST_NEST_GAME_TAIL_THRESHOLD`（默认 50ms）的完成事件后保存前后窗口，`_TAIL_BASELINE_EVERY` 另存对照窗口）；其余变量见夹具 [tailtrace_test.go](../../codegen/internal/entity/testdata/nestgame/tailtrace_test.go) 文件头。产物在样本目录的 `sample-N.tail/`，脚本结束后自动用 [tailtrace](../../scripts/perf/tailtrace/main.go)（独立模块，依赖 `golang.org/x/exp/trace`）生成 `analysis.txt` / `analysis.json`：长尾事件与 GC / STW / 调度延迟 / 互斥等待 / 可运行队列的关联与富集比、长尾簇的墙钟分钟相位，以及 trace 中各分段的 goroutine 状态；单个 trace 的簇内对照用 [burst](../../scripts/perf/tailtrace/burst/main.go)。宿主噪声在本机很明显，见 [2026-10-15 报告 §1](PERF-ROUND-20261015.md#1-长尾延迟定位)。默认关闭；开启后有观测开销，结果只用于归因，不与无采集样本的延迟数字比较。
