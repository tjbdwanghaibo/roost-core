# Nest 消息吞吐压测

日期：2026-09-24。目的：在 [Nest 收尾与截止复核](NEST-COMPLETION-2026-09-24.md) 之后，测实际处理完成的 msg/s，替代仅用单线程 Request ns/op 推算整体吞吐。基线是 `967bc69` 加当前未提交的 Nest / Sync 工作树。

## 负载与计量

使用 [scripts/perf/nest-msg](../../scripts/perf/nest-msg/main.go) 独立进程调用正式 `nest.NewEngine` / `Client.Dispatch` / `Client.Request`。对象由 EntityManager 管理，getter 只读取已加载实体；使用真实 EntityBase、Touch、Guard、实体锁和 worker，业务 handler 在锁内将实体计数加一。DurabilityMemory / RollbackNone，不接 WAL 或 Sync。该目录是压测夹具，不是示例游戏接线。

- **hot**：10000 个实体已加载，全部消息发给同一个实体，刻画单路由热点。
- **spread**：消息均匀轮转到 10000 个实体，按正常哈希分配 worker。
- **Dispatch**：32 个并发调用者，每个最多 32 条在途，共 1024 条；Guard post-release callback 记录完成并归还额度。
- **Request**：32 个并发调用者，每个等待一次回复后再提交下一条，最多 32 条在途。
- 每个 worker 队列容量 4096；Dispatch 总在途小于单个队列容量，即使全落在一个 worker 也不靠排队拒绝测容量。
- 正式样本每轮 500 万条；每个独立进程先预热 10000 次 Request，不计入结果。每组 3 个独立进程，组间串行运行。
- GOMAXPROCS 与 worker 数分别记录；保留框架默认常规指标、慢请求 watch，新增细分阶段指标保持默认关闭。

**吞吐 = 成功完成消息数 / 从放行生产者到 Shutdown 排空结束的墙钟时间。** 不是入队速度，也不把未处理的积压计入吞吐。样本要求准入数、完成数、Nest.ProcessedMessages 和实体更新总数一致；队列拒绝、API 错误或计数不一致会令程序非零退出。计数核对不是逐消息唯一 ID 审计。

延迟按每个 producer 每 64 条采样一次：Dispatch 从调用前到 Guard 解锁后的完成 callback；Request 从调用前到收到回复。Dispatch 获取发送额度的背压等待在时间戳之前，**不包含在延迟分位数中**，但包含在总吞吐时间中。两者在途窗口不同，p99 不能直接解释为 API 的性能优劣。这里是有限并发的闭环压测，不是固定到达率、无限积压的过载测试。

每轮记录 p50/p95/p99/样本最大值、完成数、拒绝数、错误数、GC 次数/暂停、进程分配 B/msg 与 allocs/msg。后两者包含压测的 token、采样和完成统计成本，不能当成 Nest 函数自身的精确分配。排空属于计时，初始化和预热不属于计时。

## 复跑

```sh
# 默认：4 个 Go 执行核、4 worker、10000 实体分散、异步 Dispatch。
GOCACHE=/tmp/roost-nest-go-cache \
ROOST_PERF_LABEL=msg-local ROOST_PERF_CPU=4 ROOST_PERF_COUNT=3 \
bash scripts/perf/nest-msg.sh -messages=5000000

# 单实体热点，同步 Request。
GOCACHE=/tmp/roost-nest-go-cache \
ROOST_PERF_LABEL=msg-hot-request ROOST_PERF_CPU=4 ROOST_PERF_COUNT=3 \
bash scripts/perf/nest-msg.sh -mode=request -distribution=hot -workers=4 -messages=5000000
```

其他选项见 `go run ./scripts/perf/nest-msg -help`：workers / producers / window / queue / entities / messages / warmup / sample-every / timeout。脚本拒绝覆盖已有目录；产物写入 `.gitignore` 已排除的 `artifacts/perf/nest/<label>/`，包含 env.txt、独立编译二进制和逐轮 JSON/log。

正确性检查：

```sh
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test -race ./scripts/perf/nest-msg
```

四个模式×分布用例验证真实处理完成计数、预热排除和采样条数，不能用 race 版本做吞吐比较。

## 正式实测结果

环境：Apple M5，10 个逻辑 CPU，macOS arm64，Go 1.27.0；GOGC / GOMEMLIMIT / GODEBUG 未覆盖。GOMAXPROCS 见表，默认细分阶段指标关闭。共 **10 组 × 3 轮 × 500 万 = 1.5 亿条**正式消息，预热另计。全部 30 个样本正确性检查通过，零 API 错误、零队列拒绝，完成数与实体更新数一致。

下表吞吐和 p99 分别取三轮中位数；吞吐范围为三轮 min～max。最大样本是三轮采样中的最大值，不是未采样消息的全局最大值。

| GOMAXPROCS / worker | 分布 | API | 完成吞吐 msg/s（中位数） | 三轮范围 msg/s | p99 ms（中位数） | 最大样本 ms |
| --- | --- | --- | ---: | ---: | ---: | ---: |
| 4 / 1 | 单实体热点 | Dispatch | 453,616 | 445,990～455,665 | 3.077 | 10.760 |
| 4 / 1 | 单实体热点 | Request | 454,830 | 450,891～458,319 | 0.195 | 5.576 |
| 4 / 1 | 10000 实体分散 | Dispatch | 432,738 | 420,932～440,484 | 3.009 | 56.471 |
| 4 / 1 | 10000 实体分散 | Request | 437,093 | 429,149～440,028 | 0.201 | 6.500 |
| 4 / 4 | 单实体热点 | Dispatch | 449,666 | 446,944～451,382 | 2.689 | 17.781 |
| 4 / 4 | 单实体热点 | Request | 444,497 | 438,853～444,786 | 0.202 | 42.766 |
| 4 / 4 | 10000 实体分散 | Dispatch | 803,073 | 779,995～841,499 | 5.054 | 44.423 |
| 4 / 4 | 10000 实体分散 | Request | 586,412 | 566,699～641,555 | 0.305 | 37.520 |
| 8 / 8 | 10000 实体分散 | Dispatch | 724,412 | 698,908～725,747 | 7.683 | 25.309 |
| 8 / 8 | 10000 实体分散 | Request | 508,543 | 504,427～510,132 | 0.321 | 7.429 |

### 分配与延迟细项

| 配置 / 分布 / API | p50 ms | p95 ms | B/msg | allocs/msg |
| --- | ---: | ---: | ---: | ---: |
| P4/W1 hot dispatch | 2.197 | 2.489 | 1563 | 47.03 |
| P4/W1 hot request | 0.065 | 0.099 | 1851 | 48.03 |
| P4/W1 spread dispatch | 2.293 | 2.604 | 1563 | 47.03 |
| P4/W1 spread request | 0.068 | 0.104 | 1851 | 48.03 |
| P4/W4 hot dispatch | 2.207 | 2.471 | 1563 | 47.03 |
| P4/W4 hot request | 0.066 | 0.106 | 1851 | 48.03 |
| P4/W4 spread dispatch | 0.155 | 4.437 | 1565 | 47.04 |
| P4/W4 spread request | 0.038 | 0.153 | 1853 | 48.04 |
| P8/W8 spread dispatch | 0.610 | 5.868 | 1590 | 47.20 |
| P8/W8 spread request | 0.040 | 0.193 | 1876 | 48.19 |

原始结果目录均在 `artifacts/perf/nest/msg-20260924-load-*`，各含三轮 JSON / 日志 / 环境记录 / 可执行文件；汇总 JSON 为 `artifacts/perf/nest/msg-20260924-summary.json`。试跑的 100 万条样本目录不混入上述正式结果。


## 如何理解这组结果

1. 在本机当前轻业务夹具下，P4/W4 分散 Dispatch 为约 **80.3 万 msg/s**，Request 为约 **58.6 万 msg/s**。这是处理完成速度，已排空全部准入消息。
2. 单实体热点约 **45 万 msg/s**，worker 从 1 增到 4 基本无收益，同一个 ID 路由到同一 worker。要提升这类业务吞吐，应先减少热点实体上的业务成本，不能靠增加 worker 让同一实体并行改状态。
3. P8/W8 分散场景低于 P4/W4。此对比同时改变执行核数和 worker 数，仅说明本机这两种配置的实际结果；没有新的 CPU/锁争用 profile，不能直接将下降归因于某一个锁或指标库。增加 worker 不是线性扩容保证。
4. Dispatch 的在途窗口为 1024，Request 为 32，前者积压和 p99 更高是需要结合窗口理解的结果。不能把两者 p99 相除当成单次调度开销比。
5. 三轮中位数不是置信区间；机器没有 CPU 绑核和后台进程隔离。本轮是吞吐压力测试，不是长时间稳定性验收。全矩阵最大延迟采样 **56.471ms**，来自 P4/W1 spread Dispatch，不能仅凭 p99 小于 50ms 就声称硬延迟上限达标。

本次负载只改变一个锁内计数，不含真实组件 setter 的脏标记、事务快照、WAL fsync、跨服写、AOI 更新、Sync profile 编码和网络。此前的 1000 玩家 / 10000 全局实体 / 每人 50 可见 / 1% 或 5% 变化需求仍应以完整业务链路测量，不能直接拿 80 万 msg/s 换算线上人数或延迟承诺。

## 收尾和验证记录

本轮未修改 Nest 运行逻辑，复核第 8 节已修复的完成边界并维持现有接口与默认配置。七个关联包 race 通过，新增压测夹具四个模式用例 race 通过；详细结果见 [收尾第 9 节](NEST-COMPLETION-2026-09-24.md#9-消息吞吐压测前的再收尾)。Nest 与夹具 vet、全仓 build 通过；build 的模块 stat cache 写权限提示未影响退出。脚本 bash 语法、gofmt、文档相对链接和 diff whitespace 检查通过。性能样本使用普通构建，不带 race。

codebase-memory 使用 Verify，项目 Users-whb-roost-roost-core，代际 `2026-09-24T06:50:16Z`；核对 Client/队列/实体/事务完成与 stats 候选路径，覆盖为 metadata_match/no_recorded_issue，Nest scope 无记录缺口。图谱查询的同名/接口调用不足以穷尽实际调用，以源码和真实运行补证。新增 scripts 夹具由 fast 索引规则排除，直接核对源码、编译和 race，不将图谱未收录当作代码未使用。由于本轮没有修改索引范围内的运行代码，不重复全仓重建索引。

所有新增压测代码、脚本和文档尚未提交；大体积本地二进制与样本按既有规则忽略。
