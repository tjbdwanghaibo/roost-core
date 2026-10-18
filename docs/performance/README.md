# 性能验证与复跑

性能报告按版本、场景和参数保留；通过仅对该次实际负载成立。业务功能验收归 [release](../release/v1.26.0-VALIDATION.md)，未关闭问题归 [已知限制](../maintenance/KNOWN-LIMITS.md)。

| 报告 | 范围与结论 |
| --- | --- |
| [投影器合并 checkpoint 前后对照（2026-10-18）](SAGA-CHECKPOINT-20261018.md) | 特殊记录（带回执 / effect）与普通记录合并 checkpoint：积压时每条确认 0.56～0.75 次（修前 1 次），投影器约 15 → 20 条/s；正向 8/s 由崩溃变为 P50 517 ms / P99 793 ms，正向 10/s 由崩溃变为全部正确（P50 2.2 s），全补偿 5/s P50 3.9 s → 688 ms；正向 12/s、全补偿 7/s 起仍崩溃；持续档不变；真实 WAL 微基准全特殊 256 条 256 → 1 次 checkpoint；真实 Mongo 崩溃注入（引擎级 + saga 重启）恰好一次 |
| [Saga 原生步骤性能优化（2026-10-18）](SAGA-OPT-20261018.md) | 逐跳打点 + 投影后唤醒 outbox + 去冗余读 + 等回执移出 durable 回调，同机四阶段交替：持续档 Server P50 正向 440 / 399 → 278 / 314 ms、全补偿 597 / 610 → 352 / 424 ms（主要来自 outbox 唤醒）；吞吐上限未到正向 10/s（四阶段都在正向 10/s、全补偿 10/s 崩溃），正向 7～8/s、全补偿 5/s 从崩溃变为全部正确但延迟秒级；新的第一瓶颈是投影器逐条处理带回执 / effect 的记录（checkpoint 负载下 50～55 ms） |
| [Saga D12 前后对照（2026-10-18）](SAGA-D12-20261018.md) | `79d15114` 对 `63b63fa8`：旧路径（Mongo 步骤）代码相同、正向 20/s 与全补偿 10/s 两版本都通过（新版本一次样本因 Mongo 主节点超时失败，补跑通过，原因尚未确认）；原生步骤经真实 Nest + DataEngine，手写胶水与 `Steps` 在正向 5/s、全补偿 3/s 各两轮同档位无可测差异；两者在正向 10/s、全补偿 5/s 起饱和（单进程一个 durable 逐条处理 + 投影器单条处理带回执的记录），过载时仍恰好一次 |
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

长尾归因时在上面命令前加 `ROOST_NEST_GAME_TAIL=metrics`（每秒 runtime/metrics 与 rusage，开销小）或 `ROOST_NEST_GAME_TAIL=trace`（另开 Go flight recorder，超过 `ROOST_NEST_GAME_TAIL_THRESHOLD`（默认 50ms）的完成事件后保存前后窗口，`_TAIL_BASELINE_EVERY` 另存对照窗口）；其余变量见夹具 [tailtrace_test.go](../../codegen/internal/entity/testdata/nestgame/tailtrace_test.go) 文件头。产物在样本目录的 `sample-N.tail/`，脚本结束后自动用 [tailtrace](../../scripts/perf/tailtrace/main.go)（独立模块，依赖 `golang.org/x/exp/trace`）生成 `analysis.txt` / `analysis.json`：长尾事件与 GC / STW / 调度延迟 / 互斥等待 / 可运行队列的关联与富集比、长尾簇的墙钟分钟相位，以及 trace 中各分段的 goroutine 状态；单个 trace 的簇内对照用 [burst](../../scripts/perf/tailtrace/burst/main.go)。宿主噪声在本机很明显，见 [2026-10-15 报告 §1](PERF-ROUND-20261015.md#1-长尾延迟定位)。默认关闭；开启后有观测开销，结果只用于归因，不与无采集样本的延迟数字比较。

## 读结果：压测端与服务端

维护者 2026-10-15 要求所有负载型压测把延迟拆成“压测端”（发生器节拍、模拟客户端排队与解码、观测轮询）和“服务端”（框架实际处理）两部分。各入口原有的端到端字段与通过门槛**不变**，保留它们只为与历史样本同口径比较；新增的 `Harness.*` / `Server.*`（JSON 小写为 `harness.*` / `server.*`）是同一段时间的拆分。评估框架延迟看 `Server.*`，`Harness.*` 偏大说明发生器或观测本身成了瓶颈。2026-10-15 之前的样本没有这些拆分；同一入口的分段除特别说明外在同一进程的单调时钟上取，各分段的分位分别统计，不能把分位相加。

“已分清”指原字段的区间本来就不含压测端等待；“新拆分”指原字段混有压测端等待、本次新增了分段；微基准（`nest.sh`、`sync.sh`、`nest-dispatch.sh` 等 `go test -bench` 的 ns/op）没有压测端等待的概念，不在此列。

### nest-game.sh（新拆分，2026-10-15 `44896aa4`）

`Messages` / `Heartbeats` 的 `Completion` 从计划时刻、`QueueWait` 从投递时刻起算，混有压测端等待（P99≤50ms 门槛仍按 `Completion`，偏保守）：

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `Harness.AdmitLag` | 计划时刻 → ticker 投递 | 压测端：发生器只在 Nest 10ms tick 里批量投递，天然 0～10ms；ticker 迟到也记在这里 |
| `Harness.ClientQueue` | 投递 → 模拟客户端写出请求 | 压测端：每个玩家连接一次只有一条在途请求，含等前一条回包；直接投递（heartbeat、无 Gate）恒为 0 |
| `Server.QueueWait` | 请求写出 → handler 开始 | 服务端：Gate、NATS、Game 接入、Nest 排队 |
| `Handler` | handler 本身 | 服务端 |
| `Server.Completion` | 请求写出 → Entity 释放 | 服务端合计 |
| `Completion` / `QueueWait` | 计划 / 投递 → 释放 / 开始 | 混合（历史口径） |

`GateRoundTrip` 是客户端写出到收到回包（服务端为主）。Sync 的 `PlannedToClient` 从计划时刻起算、含压测端等待（混合）；`ChangedToClient` / `CommittedToClient` 从 handler 改值 / 提交起算，是服务端到客户端的同步延迟。

### nest-msg.sh（已分清，不改）

`p50_us` / `p95_us` / `p99_us`：dispatch 模式从取得 producer 信用之后、调用 `Dispatch` 之前，到 handler 释放后的 post-release 回调；request 模式从调用 `Request` 到返回。producer 等信用（闭环窗口）发生在计时起点之前，不计入，所以全部是服务端（Nest 排队 + handler + 释放）。闭环负载下这段排队由窗口大小决定，读数时与 `window_per_producer`、`producers` 一起看。

### dataengine.sh（新拆分）

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `harness.admit_lag_p95/p99/max_ms` | 计划时刻 → writer 调用 `Request` / `RequestMulti` | 压测端：计时器迟到，以及 32 个 writer 都在等上一条回复时的排队；只在 `ROOST_PERF_RATE>0` 时有值（`admit_lag_samples` = 请求数），闭环（默认 rate=0）为 0 |
| `request_p95_ms` / `request_p99_ms` | 调用 → 回复 | 服务端：Nest 排队、handler、按持久级别等 WAL / 提交 |
| `record_to_mongo_p95/p99_ms` | 提交记录生成（`CreatedAt`，墙钟）→ Mongo 投影返回 | 服务端（投影链路） |
| `scheduled_to_reply_p99_ms` | 计划时刻 → 回复 | 混合（历史口径），= admit_lag + request 按样本相加 |
| `mixed.StateAgeAtDecodeP99MS` / `ExistingObjectChangeToDecodeP99MS`（`ROOST_PERF_MIXED=1`） | handler 改值 → 进程内 Transport 解码 | 服务端（Transport 同步回调，无客户端排队） |

同一修改顺带修正了投影计时：Projector 对单条段优先走 `ProjectFenced`，夹具只包 `Project` / `ProjectBatch` 时这条路径绕过计时，`ROOST_PERF_RATE>0` 时稳定报 `projection samples=n-1 want=n` 失败（默认闭环全是批量段，所以没暴露）。

### remote.sh / remote-capacity.sh / remote-workers.sh（新拆分）

后两个入口逐档调用 `remote.sh`，字段相同。`result.json` 新增（1ms 桶向下取整，只计成功回复）：

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `Harness.AdmitLag` | 计划时刻 `due` → 发生器把任务放进通道 | 压测端：发生器在 1/rate 间隔的 ticker 上补发，ticker 迟到记在这里 |
| `Harness.ClientQueue` | 放进通道 → 会话 worker 调用 `RequestMulti` | 压测端：会话 worker 都在等上一笔回复时在通道里排队 |
| `Server.Completion` | 调用 `RequestMulti` → 返回 | 服务端：Nest 排队、加载、handler、WAL / Remote 写入与回复 |
| `LatencyP50MS`～`LatencyMaxMS` | `due` → 返回 | 混合（历史口径） |

`ErrorDetails[].ElapsedMS` 从调用起算，属服务端。三份直方图常驻约 1.4MB 堆，长稳 `heap_bytes` 的基线相应抬高，增长趋势不受影响。

### saga.sh（新拆分）

原 `P50MS`～`MaxMS` 从计划启动时刻算到 50ms 轮询读到持久终态，混有发生器排队与轮询间隔。服务端终态时刻改从引擎写出口取：测试把协调器的 Store 包一层 `terminalClock`，记下终态那次 `Store.Apply` 提交成功返回的时刻（`Store.Apply` 是协调器唯一的状态写出口，见 `stepTransition`），不改请求与结果。

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `Harness.AdmitLag` | 计划启动时刻 → 提交协程调用 `StartSaga` | 压测端：计时器迟到、64 格任务通道与 8 个提交协程 |
| `Server.Start` | `StartSaga` 调用本身 | 服务端（建记录并提交） |
| `Server.Completion` | 调用 `StartSaga` → 终态 `Apply` 返回 | 服务端：两步经 JetStream 派发、Mongo 步骤执行、完成回传与终态提交 |
| `Harness.ObserveLag` | 终态 `Apply` 返回 → 轮询读到终态 | 压测端：上界约一个 50ms 轮询间隔加一次 `Find` |
| `Minutes[].ServerP99MS` | 同一分钟分段的 `Server.Completion` P99 | 服务端 |
| `P50MS`～`MaxMS`、`Minutes[].P50MS/P99MS/MaxMS` | 计划 → 轮询读到 | 混合（历史口径） |

`Server.TerminalMissing` 非零说明终态有别的写出口、拆分不完整；各段带 `NegativeDurations`，非零说明时刻先后假设不成立（例如 `Find` 先于 `Apply` 返回读到提交）。

### sync-aoi.sh / sync-recovery.sh（新拆分）

服务端与模拟客户端是两个进程；`planned_event_to_client` / `state_change_to_client` / `commit_to_client` 的终点是客户端解码校验完该对象。`server.json`（服务端进程，只计正式测量 tick，样本是每次状态变化）：

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `harness.admit_lag_ms` | 批次计划时刻 → 批次醒来开始发送 | 压测端：`time.Sleep` 迟到 |
| `harness.client_queue_ms` | 批次开始 → 本次状态变化调用 `Request` | 压测端：一个发生器串行发出整批，后面的等前面的 `Request` 返回 |
| `server.request_to_change_ms` | 调用 `Request` → handler 改值 | 服务端（`-nest=false` 时是直接改值，近似 0） |

`client.json`（样本是每个观察者收到的每次变化；`Changed` / `Committed` 是服务端进程墙钟，同机两进程比较）：

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `server.change_to_receive_ms` | handler 改值 → 客户端读完整帧负载、解码之前 | 服务端：Sync 间隔与 flush、传输（含 async 队列）、回环 TCP；客户端读协程被调度的时间无法分开，按服务端上界计 |
| `server.commit_to_receive_ms` | Nest 提交 → 同上 | 服务端 |
| `harness.client_decode_ms` | 读完整帧 → 解码校验完该对象 | 压测端：含同一帧里排在前面的对象 |
| `planned_event_to_client_ms` | 计划 → 解码完 | 混合（50ms 门槛之一，历史口径） |
| `state_change_to_client_ms` / `commit_to_client_ms` / `delta_*` / `full_*` | 改值 / 提交 → 解码完 | 服务端为主，含 `client_decode`（门槛之一，历史口径） |
| `transport_admission_to_client_ms` | Transport 写出 → 整帧解码完 | 服务端为主，含整帧解码 |

`flush_start_lag_ms` 是驱动循环 `time.Sleep` 迟到（压测端）；`server_active_work_ms` 是驱动线程一个 tick 内的忙碌时间，包括串行发出的请求（服务端为主）。恢复项：`recovery_admission_caught_up_ms` 在每个 tick 末检查一次，含最多一个 tick（50ms）的观察间隔，是服务端值的上界；`recovery_client_verified_ms` 只在诊断模式（`-trace-capacity>0`）有值，且服务端为核验暂停了输入，不作延迟比较。

### sync-business.sh（新拆分）

单进程；样本是每个客户端收到的每一帧，按 tick 对齐：

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `harness.admit_lag_ms` | tick 计划时刻 → 开始发送（与 `start_lag_ms` 同一段，按帧加权） | 压测端 |
| `harness.capture_ms` | 夹具记下本 tick 期望值 | 压测端 |
| `harness.client_decode_ms` | 读完整帧 → 解码并核对期望值 | 压测端 |
| `server.requests_ms` | 本 tick 的业务请求（AddExp + Moved，发生器串行发出，每条等回复）减去 capture | 服务端 |
| `server.flush_to_receive_ms` | Sync flush 开始 → 客户端读协程读完负载 | 服务端：flush、推送与回环 TCP |
| `server.completion_ms` | `requests` + `flush_to_receive` | 服务端合计 |
| `scheduled_to_client_ms` | tick 计划时刻 → 解码完（墙钟） | 混合（历史口径）≈ 上面五段按帧相加 |

`tick_work_ms` / `flush_ms` / `deadline_misses` 是驱动线程按 tick 的服务端工作量。game-demo 场景接口改为 `PushSyncPlayer(ctx, id, messageID, payload []byte)` 之后（`ba0dd7c2` / `634ae6ce` 一带，未逐提交确认）该夹具无法编译（`*businessTCP does not implement scenePusher`），本次一并改为新接口；之前的 sync-business 历史样本早于该变更。
