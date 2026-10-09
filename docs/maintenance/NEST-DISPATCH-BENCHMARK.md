# Nest 共享调度与按 ID 分片的对照基准

这份基准回答一个具体问题：不执行游戏业务，只运行简单 handler 时，当前 Nest 调度与 `ID % worker数` 分片的完成吞吐有什么区别？它没有改动生产调度器，也不能独自解释 v1.25.0 的整链路重档失败。

## 怎么理解两种方式

当前 Nest 像一个公共候车区：任务先登记要访问的 ID，同 ID 必须等前面的任务结束；没有依赖的任务由任意空闲 worker 执行。共享锁保护准入、就绪队列、依赖关系、统计和完成收尾。一个不同 ID 的慢任务不会把某个 worker 永久绑定到一批其他 ID。

ID 分片像固定窗口：`uint64(ID) % worker数` 决定排哪个窗口，每个 worker 顺序消费自己的 channel。不同窗口能并行，同窗口的任务必须排队，即使其他窗口已经空闲。channel 自身也有同步成本，不能把它叫作“完全无锁”。

本次对照的 ID 分片只放在 `_test.go` 中，只接受单个正整数 ID。不实现多 ID 原子执行、慢准备、内部续行、冷目标转慢、动态目标或生产停机重试，不能直接替换正式 Nest。

## 测量边界与公平条件

源码入口：[基准及正确性检查](../../framework/nest/dispatch_compare_bench_test.go)、[正式队列](../../framework/nest/dispatch_queue.go)、[复跑脚本](../../scripts/perf/nest-dispatch.sh)。

| 项目 | 两边的设置 |
| --- | --- |
| worker | 等于 GOMAXPROCS；本轮都是 4 |
| 生产者 | 主矩阵 4 个；额外空 handler 对比 1、4、16 个 |
| 未完成窗口 | 总计最多 64 条，均分到生产者；每完成一条才补一条 |
| 等待容量 | 总计 65,536；分片均分容量，本轮远未触及容量上限 |
| 执行包装 | 都创建 fctx 快 worker 上下文，使用相同 SafeFunc 和 Msg.OnRelease |
| 请求生成 | 都用 GenMsg/OnSend 和相同 Params、时间采集、完成反馈 |
| 完成口径 | handler 完成并反馈；整组吞吐还包括最终消息释放和队列排空 |
| 排除项 | 无 Entity 获取、Guard、事务、DAO、WAL、Sync、AOI、Gate、网络 |
| 错误 | 任何准入拒绝、未完成、非法时间或排空失败都会使样本失败；不忙重试 |

这是**固定并发窗口下的完成吞吐**，不是向系统持续灌入 110,000 条/秒的开环测试。队列可以容纳 65,536 条不表示本次真的堆积了这么多条。`ns/op` 是整组耗时除以完成数，不是单个 handler 的运行时间；并行执行时两者本来就不同。

正式队列依旧承担依赖登记和统计，分片对照组不复制这些机制。因此空 handler 的差距代表整个调度机制的成本差异，不能全部归因于一把 mutex。需要结合单独的锁和 CPU profile 判断。

### handler 怎么模拟

| 名称 | 做什么 | 适用解释 |
| --- | --- | --- |
| noop | 不做业务运算 | 放大调度、上下文、消息和测试测量开销 |
| cpu_1ms | 固定数量的整数运算 | 单线程校准约 1ms 的 CPU 工作量 |
| cpu_200ms | 上一行运算次数的 200 倍 | 约 200ms 的 CPU 工作量 |
| wait_1ms | Sleep(1ms) | 占住 worker、让出 CPU 的等待模型 |
| wait_200ms | Sleep(200ms) | 慢等待导致 worker 不可用的模型 |
| cpu_mixed_1pct | 每第 100 条执行 200 倍运算，其余执行 1 倍 | 少量慢任务和多数快任务混合 |

CPU 工作量在进程内只校准一次，两种调度器及三次重复使用相同运算次数。校准取五次测量的中位数；受频率、抢占、系统负载影响，运行中未必精确等于 1ms/200ms，所以同时报告 `handler-avg-ms`。没有用“忙等到墙钟截止”来伪造固定 CPU 工作：那会把被系统抢占的时间也当作已经完成计算。

这里的“200ms”是 handler 耗时，**不是启用 Nest 的慢池**。两边都只用同一数量的快 worker。等待模型故意模拟错误地占住快 worker 的情况，不建议业务在快 handler 内 Sleep 或等网络。

### ID 怎么分布

- `uniform`：序号循环映射到 1～10,000。4 个生产者分别产生序号余数 0～3；均匀负载时四个分片的请求数量平衡。
- `colliding_ids`：ID 为 `(序号 % 10000) × worker数 + 1`。它们是不同实体，却全部落入同一分片，用于显式观察取模碰撞的代价；这是构造的边界，不代表真实玩家 ID 一定如此。
- `hot_id`：全部为 ID=1。两种方式都必须串行，增加 worker 不会提高这个实体的并行度。

混合负载的慢请求由 `序号 % 100 == 0` 决定，所以在 4 分片情况下落在同一分片、同一生产者上；这是有意暴露慢请求集中时的失衡，不能称作随机均匀的慢请求分布。各生产者任务数固定，尾部排空也计时。

## 本轮环境与结果

运行基线为 `cae44d8d`，生产运行时与 v1.25.0 相同。环境：Windows/amd64、AMD Ryzen 7 5800H、Go 1.27.0；GOMAXPROCS=4。Windows 结果用于本机对照，不扩大框架 macOS/Linux 的支持保证，也不能换算成此前 Apple M5 的容量。

主矩阵为 6 种 handler × 3 种 ID 分布 × 2 种调度方式，每组 3 次，`benchtime=2s`。Go 会自动调整每组操作数，真实耗时和操作数见原始记录；这不是每组持续稳定压测两分钟或十五分钟。

下表为三次完成吞吐的**中位数（最小～最大）**，单位为次/秒；倍率为 ID 分片中位数除以 Nest 中位数。小幅差异不作为显著性能结论。每条原始样本的操作数、handler 平均耗时、分位数和分配见[完整 benchmark 记录](benchmarks/nest-dispatch-20261009.txt)。

| handler | ID 分布 | Nest 次/秒 | ID 分片 次/秒 | 分片/Nest |
| --- | --- | ---: | ---: | ---: |
| noop | uniform | 742,393.00（735,986.00～743,867.00） | 4,698,013.00（4,626,268.00～4,741,935.00） | 6.33× |
| noop | colliding_ids | 775,101.00（733,821.00～776,779.00） | 1,882,628.00（1,798,699.00～1,894,311.00） | 2.43× |
| noop | hot_id | 671,843.00（646,979.00～721,645.00） | 1,667,869.00（1,666,237.00～1,715,972.00） | 2.48× |
| cpu_1ms | uniform | 3,920.00（3,850.00～3,935.00） | 3,670.00（3,638.00～3,737.00） | 0.94× |
| cpu_1ms | colliding_ids | 3,353.00（3,254.00～3,571.00） | 927.90（890.20～939.60） | 0.28× |
| cpu_1ms | hot_id | 924.30（908.90～925.30） | 930.00（916.30～937.30） | 1.01× |
| cpu_200ms | uniform | 18.24（17.99～19.37） | 17.44（16.54～19.12） | 0.96× |
| cpu_200ms | colliding_ids | 17.19（17.16～18.68） | 5.29（4.99～5.34） | 0.31× |
| cpu_200ms | hot_id | 5.28（4.88～5.32） | 5.24（4.94～5.30） | 0.99× |
| wait_1ms | uniform | 2,635.00（2,627.00～2,639.00） | 2,702.00（2,694.00～2,727.00） | 1.03× |
| wait_1ms | colliding_ids | 2,715.00（2,679.00～2,760.00） | 705.70（692.70～716.60） | 0.26× |
| wait_1ms | hot_id | 685.30（683.80～691.70） | 678.40（670.10～700.20） | 0.99× |
| wait_200ms | uniform | 19.47（19.46～19.47） | 19.46（19.46～19.47） | 1.00× |
| wait_200ms | colliding_ids | 19.47（19.45～19.47） | 4.99（4.99～4.99） | 0.26× |
| wait_200ms | hot_id | 4.99（4.99～4.99） | 4.99（4.99～4.99） | 1.00× |
| cpu_mixed_1pct | uniform | 1,253.00（1,176.00～1,296.00） | 433.40（424.30～440.00） | 0.35× |
| cpu_mixed_1pct | colliding_ids | 1,264.00（1,237.00～1,268.00） | 319.90（295.40～320.00） | 0.25× |
| cpu_mixed_1pct | hot_id | 336.40（334.80～336.60） | 336.70（335.50～343.70） | 1.00× |

均匀 ID 的延迟指标（三次样本指标的中位数，不是将三次请求混合后重算的分位数；单位 ms）：

| handler | Nest 等待 P99 | 分片等待 P99 | Nest 请求总耗时 P99 | 分片请求总耗时 P99 |
| --- | ---: | ---: | ---: | ---: |
| noop | 1.002 | 0.000 | 1.002 | 0.000 |
| cpu_1ms | 14.790 | 17.400 | 15.820 | 18.540 |
| cpu_200ms | 2,182.000 | 2,301.000 | 2,374.000 | 2,510.000 |
| wait_1ms | 30.510 | 24.370 | 32.740 | 25.900 |
| wait_200ms | 1,803.000 | 1,803.000 | 2,003.000 | 2,004.000 |
| cpu_mixed_1pct | 66.200 | 220.500 | 200.500 | 221.500 |

空 handler 的生产者竞争对照（4 worker，总窗口仍为 64）：

| 生产者 | Nest 次/秒 | ID 分片 次/秒 |
| --- | ---: | ---: |
| 1 | 780,597.00（697,493.00～825,530.00） | 1,989,974.00（1,598,305.00～2,015,624.00） |
| 4 | 668,080.00（644,917.00～748,807.00） | 5,117,370.00（4,443,733.00～5,349,965.00） |
| 16 | 727,071.00（680,044.00～730,973.00） | 4,681,354.00（4,676,470.00～4,729,470.00） |

### 单独的锁诊断

正式测量之后另跑了 3s 的 noop/uniform CPU、mutex、block profile，两种实现分开运行。Nest 的 mutex profile 共记录约 18.18 秒**累计等待时间**，准入路径约 8.43 秒，worker 路径约 9.72 秒；定位到 worker 取任务后的解锁约 2.63 秒、完成收尾的解锁约 6.98 秒。多个 goroutine 的等待会相加，不能把 18.18 秒当成一次请求延迟，也不能把这里约 99.6% 的 mutex 归因比例说成 CPU 占用 99.6%。

这证明在本次空 handler 场景，共享队列锁是明确的争用点。CPU profile 同时包含信号量等待/唤醒、自旋、map、fctx 等成本；ID 对照省去的也不仅是锁。profile 覆盖整个测试进程，包括校准和框架开销，吞吐仍只采用前面无 profile 的正式样本。原始 profile 在 `artifacts/perf/nest/dispatch-compare/`；[锁诊断文本](benchmarks/nest-dispatch-mutex-20261009.txt)可随仓库查看。

### 时间精度及失败样本

本机极短调用可能在同一时钟刻度内完成，逐条耗时会被记录为 0。`zero-duration-pct` 保留这个比例；0ms 不表示没有开销。整组秒级耗时可用于吞吐计算，但本机 noop 的细粒度延迟分位数不宜用于延迟承诺。

第一版夹具把 `total <= 0` 当成未完成，在 noop 和部分短任务上误报。已经改成独立 `completed` 标记，并保留负耗时检查。原始失败日志单独保留为 `invalid-clock-selfcheck.txt`；它不能用来证明生产队列丢消息，也不混入结果。首个 PowerShell 命令的未引用参数被错误解析，日志另存 `cli-first-attempt.txt`，没有产生有效性能样本。

200ms 场景的样本数量较少，P99 常等于最大值；这些数字展示固定窗口排队的量级，不是长期尾延迟保证。64 条都排到一个 200ms worker 时，尾部可能等约 12.8 秒；本轮部分自适应样本数少于 64，因此不能假设每组一直占满窗口。

## 如何复跑

macOS/Linux 在仓库根目录运行，不需要数据库或 NATS：

```sh
ROOST_PERF_LABEL=dispatch-baseline ROOST_PERF_CPU=4 \
ROOST_PERF_COUNT=3 ROOST_PERF_BENCHTIME=2s bash scripts/perf/nest-dispatch.sh

# 单独放大空 handler 的锁竞争，使用新的标签；worker 数也随 CPU 参数变化。
ROOST_PERF_LABEL=dispatch-8workers ROOST_PERF_CPU=8 \
ROOST_PERF_BENCH='^BenchmarkDispatchCompareProducers$' bash scripts/perf/nest-dispatch.sh

# 延长慢任务采样；不与其他编译、压测或索引重建并跑。
ROOST_PERF_LABEL=dispatch-slow-long ROOST_PERF_BENCHTIME=30s \
ROOST_PERF_BENCH='^BenchmarkDispatchCompare$/(cpu_200ms|wait_200ms)' \
ROOST_PERF_TIMEOUT=60m bash scripts/perf/nest-dispatch.sh
```

Windows 可直接用 Go 命令；PowerShell 调用编译后的测试二进制时，把 `-test.bench=...` 等整项参数放在引号里：

```powershell
go test ./framework/nest -run '^TestDispatchComparisonSingleIDContract$' -count=3
go test ./framework/nest -run '^$' -bench '^BenchmarkDispatchCompare(Producers)?$' -benchmem -benchtime=2s -count=3 -cpu=4 -timeout=20m
```

结果是标准 Go benchmark 文本，可以交给 benchstat 做后续同机、同参数、同运行模式比较。脚本保存 Git、源码摘要、Go、CPU、参数和原始结果；同标签已经存在会拒绝覆盖。大日志、测试二进制、profile 留在被忽略的 `artifacts/perf/nest/<label>/`。

使用 `ROOST_PERF_PROFILE=1` 会在正式测量结束后，另跑 noop/uniform 的两种实现，分别生成 CPU、mutex、block profile。profile 开启后的 TPS 只能作为诊断结果，不能混入无 profile 的表格。

```sh
go tool pprof -top artifacts/perf/nest/<label>/nest.test artifacts/perf/nest/<label>/nest.mutex
go tool pprof -top artifacts/perf/nest/<label>/nest.test artifacts/perf/nest/<label>/nest.cpu
```

## 对重档排查有什么帮助

4 个 worker 每次处理 1ms，忽略一切开销时也只有约 4,000 次/秒；每次 200ms 则约 20 次/秒。同 ID 必须串行时分别约 1,000、5 次/秒。实际应使用测得的 handler 耗时计算，而不是把校准名称当精确计时。

此前重档输入约 110,000 条 Nest 任务/秒，若仅 4 个 worker 要承接这批任务，平均每条占用 worker 的预算约为 **36.4 微秒**，还没有扣掉其他进程内工作与调度开销。这说明 1ms/200ms 是用来观察调度规律的模拟场景，不是对原压测 handler 实际成本的测量。

因此，先用空 handler 和 profile 判断调度成本，再用真实重档记录 handler 占用、CPU、GC、前驱等待、AOI/Sync 和客户端背压。不能因为分片在空 handler 下更快就直接替换 Nest，也不能因为 1ms 场景接近就证明真实 110,000 条/秒下共享队列没有瓶颈。

## 验证记录

2026-10-09 完成原始采样，2026-10-10 整理交付：

- 单 ID 串行、FIFO、完整排空回归 `TestDispatchComparisonSingleIDContract`，`-count=3` 通过。
- 正式主矩阵 108 个样本、生产者竞争 18 个样本，共 126 个，均 PASS、拒绝为 0、全部预定请求完成；固定 CPU 校准值为 524,700 次整数运算/名义 1ms。
- 独立 race：同 ID 契约测试，以及 noop/1ms CPU × 三种 ID 分布 × 两种调度器的 `-benchtime=100x` 样本通过。race 数据不混入性能结果。
- 本地 `go build ./...` 返回成功；Git Bash 脚本语法、脚本单次冒烟（两种调度器均 PASS）、`TestTrackedMarkdownRelativeLinksResolve` 和 `git diff --check` 通过。脚本冒烟不是额外容量样本。
- MCP 最初连接关闭；恢复后确认图谱仍为 2026-10-08 的旧目录代际，本轮依据工作树精确源码核对，没有把旧索引当成新实现。
