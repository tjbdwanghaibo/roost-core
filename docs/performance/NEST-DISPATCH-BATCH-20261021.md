# D20 方案 D：Nest 批量入队、逐个执行（2026-10-21）

> **实验记录（2026-10-21 起）**：维护者 2026-10-21 按本报告 §7 推荐 A 决定：方案 D 的分支不合入，只保留“worker 结束一项后，在同一次持锁里取下一项”（本报告 §6 的普通短 handler 收益），连同它的用例与微基准证据随[选项 C：Nest 批量心跳作业](../maintenance/FEATURE-2026-10-21-nest-batch-heartbeat.md)一起合入；`DispatchBatch`、批量就绪链、提升、一次多取都不进 main。下文引用的原型提交都在分支 `perf/nest-dispatch-batch` 上，不在 main 历史里；“下一步”里的分片登记表与选项 C 的结果见 [NEST-BATCH-HEARTBEAT-20261021](NEST-BATCH-HEARTBEAT-20261021.md)。

维护者 2026-10-21 批准做原型和对比（[DESIGN-TODO D20](../maintenance/DESIGN-TODO.md#d20-移动由实体自己的心跳驱动mapbasic--move-组件--actionflow-控制) “压测后维护者决定”）。目标：保留单目标 `Dispatch`（X）的全部语义，把 worker 唤醒和抢派发队列锁的成本批量化，**CPU 接近 ssr 式批量（Y）、业务消息 p99 不差于 X、语义不变**即达标，达标则心跳切到 D。原型代码在 `framework/nest`（`NestMgr.DispatchBatch` 为实验性接口，未进 `Client`），夹具复用 [gameplay/heartbeat/internal/bench](../../gameplay/heartbeat/internal/bench/)（`HB_MODE=d`）。随仓摘要在 [benchmarks/nest-dispatch-batch-20261021](benchmarks/nest-dispatch-batch-20261021/)。

## 结论

**不达标。** 语义不变、业务 p99 与 X 相当，但 CPU 远没有接近 Y：5 万实体 100ms、100% 移动（50 万次/s）时进程 CPU X 3.80 核、**D 3.38 核**、Y 0.71 核；20% 移动时 1.25 / **1.11** / 0.29 核（第一组，各 3 次中位数）。D 只省下 X 与 Y 差距的 11～17%。

| 判据 | 结果 |
| --- | --- |
| CPU 接近 Y | **否**。D 比 X 少 10～12%（驱动 CPU 少约 80%：0.73 → 0.15 核），离 Y 还差约 2.7 核（100% 移动）/ 0.8 核（20%） |
| 业务 p99 不差于 X | **基本满足**。100% 移动 0.28ms 对 X 0.21ms（区间 [0.19–0.29] 对 [0.18–0.22]，略高、重叠）、比改动前的 X0 0.41ms 低；20% 移动 0.24 对 0.35ms。前提是“提升”（§2 第 3 条）：不提升时 D 的业务 p99 是 2.2～2.6ms（探索性运行） |
| 语义不变 | **是**。14 个语义用例（13 条变异核对全部变红），现有 Nest 用例未改全部通过，`-race` 通过 |

不达标的原因（§5）：方案 D 批量化的“worker 停放 / 唤醒与派发队列锁”确实降了——与派发队列有关的阻塞事件少了约 8 倍（执行追踪：X 约 24.6 万次，D 约 3.1 万次）——但 X 的 CPU 大头不在这里，而在**逐作业的 Nest 执行开销**（新 fctx 上下文、Guard 作用域与“当前消息”两个按 goroutine ID 登记的全局 `sync.Map`、事务、指标标签拼接、`time.Now`，D 原样保留，这正是它要保持的语义）以及**由此引起的 Go 调度器空转**（本机 macOS 上 `runqgrab` 的 `usleep(3)` 计入 CPU）。两个 `sync.Map` 的内部锁争用在 D 下仍有约 9 万次阻塞，与 X 相同量级。Y 省下的是“每 64 个回调才有一次这些开销”，D 结构上拿不到。

推荐（需要维护者决定，§7）：**心跳保持 X（`dispatchDeliverer`）、不切 D、`DispatchBatch` 不导出**；worker 循环“结束后同一次持锁取下一项”对所有短 handler 有独立收益（纯 Nest 微基准 −7.7% sec/op、分配不变），建议单独保留；批量就绪链 / 提升 / 多取是否撤回由维护者定（推荐撤回，A）。真正的下一个杠杆是两个按 goroutine 登记的 `sync.Map`（实验：换成与 `fctx` 相同的分片读写锁表后 X 100% 移动 3.80 → 3.28 核、−14%，见 §4.3），以及在 Linux 上复测。

## 1. Nest 现有派发结构（读源码，基线 `be5a76f5`）

codebase-memory 图的代际早于本轮（`framework/nest/dispatch_queue.go` 符号与源码一致，但本节以当前源码为准）。

| 问题 | 现状 |
| --- | --- |
| 共享还是分队列 | **共享**。一个 `dispatchQueue` 管三条通道（short / io / long），每条通道一条就绪 FIFO（`readyJobs` 单链表），该通道全部 worker 从同一条链上取；没有按 worker 或按 ID 分的队列。同 ID 顺序靠全局 `tails map[int64]*dispatchJob` + 前驱计数：作业只有在全部同 ID 前驱完成后才进就绪链，所以就绪链里任意两项都互不依赖 |
| 锁的粒度 | **一把** `dispatchQueue.mu`，三条通道、`tails`、依赖链、`pending`、统计全在它下面。准入（`admit`）、取作业（`take`）、结束作业（`finishJobLocked`）各取一次；改前 worker 每条消息取两次（取 → 放 → 执行 → 取 → 结束 → 放） |
| 停放 / 唤醒 | 每条通道一个 `sync.Cond`（绑定 `mu`）。worker 取不到作业就 `wake.Wait()`；每个作业变为就绪（准入时无前驱、或前驱结束时）`enqueueReady` 就 `Signal()` 一次，叫醒一个停放的 worker |
| 执行 | worker 在新 `fctx` 上下文里调 `NestDispatch`：Guard 作用域（`entity.NewGuardScope`，按 goroutine ID 登记在全局 `sync.Map`）、当前消息登记（`pushCurrentNestDispatchMsg`，同样是全局 `sync.Map`）、取实体锁、事务、提交、Sync 捕获、解锁后回调、回复、指标 |

单目标 Dispatch 每条几微秒，驱动又比 worker 慢（逐条准入约 1.4µs），所以就绪链几乎总是空的：每条消息都是“Signal 叫醒一个 worker → 它取一条 → 执行 → 发现链空 → 停放”。这就是 HB-STRUCTURES §4.4 里 X 的调度器自旋。

## 2. 方案 D 的改法（最终代码：分支 `perf/nest-dispatch-batch` 的原型提交，未合入 main）

最小改法：不改共享队列与单锁结构，只改“怎么进、怎么叫醒、怎么取”。

1. **批量准入** `dispatchQueue.admitBatch`（`DispatchBatch` → `Dispatcher.trySendBatch` → 这里）：每项仍走与单条完全相同的 `admitLocked`（同 ID 登记、容量判定、等待计数），但按 64 项一块持一次锁；块内新就绪的作业不逐条 Signal，解锁后每条通道 `Signal` min(新就绪数, worker 数) 次——每个停放的 worker 至多叫醒一次，醒来时不会撞上还没登记完的块。准入失败（满队列、已停止）逐项回调，不整批失败。`DispatchBatch` 的引擎状态 / ctx / handler 内调用检查与调用方快照只做一次，handler 名与 ID 逐项校验；每项建一条独立的 `MsgTypeSingle` 消息，与 `Dispatch` 建的完全相同。
2. **批量作业单独一条就绪链**（`dispatchLane.batchReady`）：`take` 在普通作业与批量作业之间交替（两类都有时，取过一个普通作业后下一次先取批量作业，反之亦然）。只有普通作业时取法与改前逐字节相同。就绪链里的作业互不依赖，换先后不影响同 ID 顺序。
3. **提升**（`promoteLocked`）：普通作业准入时若同 ID 前驱是还没开始的批量作业，把那个批量作业放进 `urgent`（按普通作业的级别取）。不提升时业务消息只要碰上自己实体在本格有一拍心跳，就要等整格积压跑完：只有批量准入 + 交替取的中间版本在 100% 移动下业务 p99 2.2～2.6ms（探索性运行，各 4 秒、未门控，不进 §4 的表）。只提升直接前驱；被提升的作业同时留在 `batchReady`，用 `started` 标志跳过，保证只执行一次。
4. **worker 循环**：结束作业后在同一次持锁里接着取下一项（改前放锁再抢一次）；取到批量作业时，按“就绪数 / worker 数”的份额一次最多再取 15 个（共 16），依次执行后一起结束（`takeMoreBatch`）。只在没有普通作业、没有续行在等时多取。出队后动作（await 的 I/O 启动）仍在本项结束后、不持锁、本 worker 执行下一项之前运行；组里某项要转 I/O 或有出队后动作时，先结束组内此前已执行的项。普通消息路径只少了一次放锁再取锁；worker 的 `fctx` 上下文选项每个 worker 建一次（§6）。

不变的：`Dispatch` / `Request` / `Cast` / `DispatchBroadcast` / `DispatchMulti*` 的代码路径与语义（现有 Nest 用例未改、全部通过）；快池不阻塞；I/O 池（慢池）取法不变（批量作业转 I/O 后按普通作业处理）；Remote / 冷目标仍按单条规则先进 I/O 池。

## 3. 语义测试（先红后绿）

新功能没有“修前”，红用变异核对：把实现改成一个具体的错误版本，确认对应用例变红，再改回。15 条变异（14 个用例，其中 2 个用例没有变异）。

| 用例 | 保证 | 变异 | 修前（变异后）失败文本 |
| --- | --- | --- | --- |
| `TestQueueBatchKeepsSameIDOrderWithSingleAdmissions` | 批内同 ID 按切片先后，并与批前 / 批后的单条准入同链 | 批量作业不登记同 ID 尾 | `id 1 ran [6 1 7 3] before its single-admitted predecessor finished` |
| `TestQueueBatchWakesEnoughParkedWorkers` | 一批新就绪作业叫醒 min(n, worker 数) 个 worker | 每条通道只 Signal 一次 | `1 of 4 batch jobs running concurrently; parked workers were not woken` |
| `TestQueueBatchBacklogDoesNotStarveOrdinaryJobs` | 公平一：批量积压时普通作业不排在整批后面 | 批量作业进普通 FIFO | `ordinary job ran at position 100 behind the batch backlog` |
| `TestQueueOrdinaryBacklogDoesNotStarveBatchJobs` | 公平二：普通作业积压时批量作业不被饿死 | 永远先取普通作业 | `batch job 0 ran at position 50; ordinary backlog starved batch jobs` |
| `TestQueueWorkerRunsAfterQueueBetweenJobsWithoutLock` | 出队后动作在本项结束后、不持锁、下一项之前 | ① 持锁调用 after；② after 早于结束 | ① 死锁超时 `panic: test timed out after 1m0s`；② `events [run0 after0 tail=true run1], want [run0 after0 tail=false run1]` |
| `TestQueuePromotesBatchJobAnOrdinaryJobWaitsOn` | 普通作业在同 ID 上等的批量作业（已就绪或还在等前驱）被提升 | 不提升 | `ordinary jobs waited behind the batch backlog: pos(1090)=96 pos(1080)=101` |
| `TestQueuePromotedBatchJobRunsOnce` | 被提升的作业即使先经 batchReady 取走也只执行一次 | urgent 不跳过已取走的作业 | `accounting after drain: running=0 ready=-1`（重复执行，handler 读到已回收消息 panic） |
| `TestQueueWorkerTakesOnlyItsShareOfBatchBacklog` | 一次多取只取“就绪数 / worker 数”的份额 | 多取不看份额 | `only 1 worker(s) running; one worker took the whole backlog` |
| `TestQueueWorkerGroupFinishesBeforeAfterQueue` | 组内某项有出队后动作时，此前已执行的项先结束 | 只结束该项 | `events [run0 run1 after1 tails=true/false/true run2], want [... tails=false/false/true run2]` |
| `TestQueueBatchRejectsPerItemWhenFull` | 容量不足时按项拒绝（ErrWorkerQueueFull），已准入的照常执行、统计计 3 | — | （容量拒绝在批内单调，“首个失败即整批失败”的变异在本用例上不可区分，由下一条在客户端层覆盖） |
| `TestDispatchBatchKeepsPerItemTransactionSemantics` | 每项各自 Guard / 事务：一项回滚不影响其他项、AfterCommit 只对提交项、同 ID 两项按序、Sync 捕获到值 | 各项共用第一项的 Params | `values e1=12 e2=11 e3=11, want 12 10 11 (e2 rolled back alone)` |
| `TestDispatchBatchReportsAdmissionFailurePerItem` | 无效 ID / 空 handler 名 / 满队列逐项返回，已准入的执行 | 第一个无效项让整批失败 | `item 0: err nest: invalid message: entity id -1 ..., want <nil>` |
| `TestDispatchBatchRoutesEachItemToItsBusinessPool` | 每项按 kind 进快池 / 长池 | 一律进 short | `pools [false short true short], want [false short true long]` |
| `TestDispatchBatchRejectsEveryItemLikeDispatch` | handler 内调用逐项 `ErrAsyncInHandler`、停止后逐项 `ErrNestStopped` | — | （与 `Dispatch` 同一检查函数，未做变异） |

变异在最终代码（分支 `perf/nest-dispatch-batch`）上逐条重跑过（[mutations.txt](benchmarks/nest-dispatch-batch-20261021/mutations.txt)，脚本 `scripts/mutate3.py`）。前 6 条最初在中间版本（只有批量准入 + 交替取）上做过一次，结果相同。

`-race`：`GOWORK=off go test -race -count=10 -run 'TestQueuePromote|TestQueueWorker|TestQueueBatch|TestQueueOrdinary|TestDispatchBatch' ./framework/nest/` 通过；`go test -race -count=1 ./framework/nest/... ./gameplay/heartbeat/...` 通过。

## 4. 心跳投递对比

### 4.1 负载与口径

与 [HB-STRUCTURES §4.2](HB-STRUCTURES-20261021.md#42-负载与测量口径) 相同：5 万实体（快池），20% / 100% 各一个 100ms 登记，按档分环 10ms 分辨率，1 个驱动；同时 5000 条/s 单目标业务消息打到全部实体；快池 8 worker、队列 65536、GOMAXPROCS 10；每次 3s 预热 + 15s 测量，一组参数一个进程，每组 3 次、轮内轮转顺序；handler 是 memory 元信息（无回滚、不写 WAL）。D 模式每格一次 `DispatchBatch`（本格全部到期登记）。

- 组：**X0** = 改动前代码（`be5a76f5`）的单目标 Dispatch；**X / Y / Z / D** = 改动后代码；**D+shard / X+shard** = 改动后代码再加一个未提交的实验补丁（[exp-sharded-registry.patch](benchmarks/nest-dispatch-batch-20261021/exp-sharded-registry.patch)：Guard 作用域与当前消息两个 `sync.Map` 换成 64 片读写锁 map，与 `fctx` 的做法相同），只用于定位成本，不是本轮提议的改动。
- 门控：每次运行前等到连续 30 秒没有别的 go 构建 / 测试进程（[scripts/foreign.sh](benchmarks/nest-dispatch-batch-20261021/scripts/foreign.sh)），运行中每 5 秒检查，采到就作废重跑。第一组作废 5 次（其他 agent 的 `go test -race`、`go build ./...` 等；其中 1 次是门控把本轮自己的二进制误判为外来进程，已修正），第二组作废 4 次。门控看不到非 go 负载；运行开始时的 1 分钟 loadavg 第一组 2.7～13.0、第二组 2.9～11.5（含门控前刚结束的外来构建与本测量自己）。
- **第一组**（第一版代码，45 次）是主要数据；**第二组**（最终代码，分支 `perf/nest-dispatch-batch`，33 次）在第一组之后修掉 worker 循环里每条消息多出的 2 次分配（`fctx` 选项闭包逃逸，§6）后重测 X / Y / D 与两个实验组，用来确认结论不变。第二组测量时最终代码的父提交是 `3e7b4011`；之后 rebase 到最新 main（movement P2 等），`framework/nest` 与夹具没有变化。

### 4.2 结果（中位数 [最小–最大]，完整列见 [delivery-summary.md](benchmarks/nest-dispatch-batch-20261021/delivery-summary.md)）

第一组：

| 组 | 移动 | 进程 CPU 核 | 驱动 CPU 核 | 业务 p99 ms | 迟到 p50 / p99 ms | 快池等待峰值 | 跳拍 / 准入失败 |
| --- | ---: | ---: | ---: | ---: | --- | ---: | --- |
| 无心跳 | — | 0.16 | — | 0.065 | — | 16 | — |
| X0（改前） | 20% | 1.29 [1.24–1.33] | 0.229 | 0.340 [0.24–0.46] | 2.71 / 4.94 | 853 | 0 / 0 |
| X | 20% | 1.25 [1.23–1.34] | 0.234 | 0.353 [0.35–0.37] | 2.98 / 5.48 | 809 | 0 / 0 |
| Y | 20% | 0.29 [0.19–0.29] | 0.009 | 0.069 [0.07–0.14] | 2.07 / 2.71 | 24 | 0 / 0 |
| Z | 20% | 0.61 [0.57–0.66] | 0.049 | 0.386 [0.19–0.71] | 2.72 / 3.88 | 72 | 0 / 0 |
| **D** | 20% | **1.11** [1.08–1.18] | **0.090** | **0.235** [0.14–0.35] | 3.30 / 5.71 | 1082 | 0 / 0 |
| D+shard（实验） | 20% | 1.05 [1.00–1.17] | 0.108 | 0.200 | 2.91 / 5.72 | 1082 | 0 / 0 |
| X+shard（实验） | 20% | 1.21 [1.20–1.26] | 0.242 | 0.186 | 2.99 / 5.57 | 1022 | 0 / 0 |
| X0（改前） | 100% | 4.06 [3.93–4.07] | 0.698 | 0.413 [0.20–0.52] | 6.84 / 11.10 | 2158 | 0 / 0 |
| X | 100% | 3.80 [3.78–3.96] | 0.734 | 0.214 [0.18–0.22] | 6.89 / 11.40 | 2385 | 0 / 0 |
| Y | 100% | 0.71 [0.54–0.75] | 0.044 | 0.130 [0.10–0.17] | 3.03 / 5.30 | 149 | 0 / 0 |
| Z | 100% | 1.87 [1.77–1.89] | 0.172 | 1.222 [0.95–1.52] | 5.43 / 6.80 | 158 | 0 / 0 |
| **D** | 100% | **3.38** [3.32–3.45] | **0.145** | **0.279** [0.19–0.29] | 7.67 / 10.60 | 10880 [5067–20588] | 0 / 0 |
| D+shard（实验） | 100% | 3.26 [3.20–3.33] | 0.166 | 0.302 | 6.52 / 9.81 | 5068 | 0 / 0 |
| X+shard（实验） | 100% | 3.28 [3.22–3.33] | 0.705 | 0.150 | 7.22 / 11.50 | 3112 | 0 / 0 |

第二组（最终代码）：

| 组 | 移动 | 进程 CPU 核 | 驱动 CPU 核 | 业务 p99 ms | 迟到 p50 / p99 ms | 快池等待峰值 |
| --- | ---: | ---: | ---: | ---: | --- | ---: |
| X | 20% | 1.40 [0.94–1.49] | 0.270 | 0.450 [0.14–0.54] | 3.31 / 6.29 | 678 |
| Y | 20% | 0.31 [0.31–0.34] | 0.011 | 0.122 | 1.92 / 2.64 | 75 |
| **D** | 20% | **1.21** [1.08–1.32] | 0.107 | **0.309** [0.13–0.38] | 3.37 / 6.12 | 1303 |
| X | 100% | 3.94 [3.84–4.03] | 0.706 | 1.153 [0.33–27.7] | 7.32 / 36.4 | 3510 |
| Y | 100% | 0.36 [0.33–0.85] | 0.021 | 0.080 | 2.63 / 4.13 | 76 |
| **D** | 100% | **3.54** [3.35–3.72] | 0.160 | **0.290** [0.28–0.36] | 7.32 / 19.2 | 16173 |
| D+shard（实验） | 100% | 3.25 [3.20–3.33] | 0.186 | 0.319 | 6.66 / 18.1 | 13612 |
| X+shard（实验） | 100% | 3.49 [3.20–4.07] | 0.835 | 0.967 [0.09–8.76] | 7.82 / 48.0 | 4634 |

第二组 X 100% 的 r2、X+shard 100% 的 r2 各有一次长尾（业务 p99 27.7 / 8.8ms、迟到 p99 456 / 380ms、间隔 p99 262 / 214ms），门控没有采到 go 进程，原因尚未确认；它们只拉高这两组的业务 / 迟到列，CPU 列不受影响。两组都没有跳拍、准入失败、业务消息失败或快池拒绝。

### 4.3 读表

- **CPU**：D 比 X 少 0.4 核（100%）/ 0.14～0.19 核（20%），几乎全部来自驱动（0.73 → 0.15 核：一次调用代替 5000 次 `Dispatch` 的校验、快照、逐条取锁）；worker 侧几乎没省。Y 是 X 的 1/5～1/10。
- **业务 p99**：D 与 X 同一量级（第一组 100% 略高、20% 略低；第二组 X 有长尾）。迟到 p50 D 比 X 高约 0.3～0.8ms：一格的 5000 拍一次进队，排在后面的要等前面的跑完；p99 相当。
- **快池等待峰值**：D 一格全部一次准入，峰值是 X 的 2～5 倍（5000～2.6 万，队列容量 65536）。心跳跳拍阈值看的是 `ErrQueueFull`，峰值离容量还远，但 50 万次/s 时队列水位明显比 X 高。
- **实验（未提交）**：两个 `sync.Map` 换成分片表后 X 100% 3.80 → 3.28 核（−14%）、20% 1.25 → 1.21 核；D 只再降 0.1 核。说明 X 的一大块成本在逐作业的登记争用，与派发队列无关；D 已经把队列侧的停放压下去，剩下的同样是逐作业开销。
- **探索性观察（未门控，各 4 秒，不进上表）**：同一二进制在 GOMAXPROCS=4 时 D 1.9 核、X 2.34 核、Y 0.74 核，完成数相同。空闲 P 越多，调度器空转越多，D 与 X 的差别在 GOMAXPROCS 小时更明显。

## 5. profile（与正式测量分开）

100% 移动、10ms，CPU + mutex profile 各一次 10s（第一版代码与 X0 的改前二进制；计数在 [profile-runs.jsonl](benchmarks/nest-dispatch-batch-20261021/profile-runs.jsonl)，每个标签最后一行是采用的那次），执行追踪各一次（2s 预热 + 1s 测量，整进程）。单位：核（= profile 样本秒 / 墙钟秒）。

| 组 | profile 期间 CPU | 调度器找活 `findRunnable` | 其中 `usleep`（`runqgrab` 偷 runnext 前的 3µs 睡眠） | `dispatchNest`（Nest 执行一条消息） | mutex 慢路径 `lockSlow` |
| --- | ---: | ---: | ---: | ---: | ---: |
| X0（改前） | 4.02 | 2.33 | 2.12 | 0.83 | 0.29 |
| X | 3.72 | 2.31 | 1.95 | 0.81 | 0.17 |
| D | 3.33 | 1.82 | 1.55 | 1.17 | 0.11 |
| D+shard（实验） | 2.83 | 0.93 | 0.59 | 1.34 | 0.03 |
| Y | 0.50 | 0.20 | 0.04 | 0.22 | — |

执行追踪里 goroutine 进入阻塞的次数（同样的 ~3s 窗口，[trace-block-events.txt](benchmarks/nest-dispatch-batch-20261021/trace-block-events.txt)）：

| 组 | 派发队列锁（worker 取 / 结束 + 准入） | `Cond.Wait`（worker 停放） | Guard 作用域 `sync.Map` | 当前消息 `sync.Map` | 合计 |
| --- | ---: | ---: | ---: | ---: | ---: |
| X | 15.0 万 | 9.6 万 | 5.4 万 | 2.4 万 + | 35.3 万 |
| D | 2.2 万 + | 0.9 万 | 4.9 万 | 4.1 万 | 12.7 万 |
| D+shard（实验） | 2.5 万 + | 1.0 万 | — | — | 4.3 万 |
| Y | 0.08 万 | 1.7 万 | 0.03 万 | 0.01 万 | 1.8 万 |

（“+”：该组前 5 名之外还有少量同类事件。）

- 方案 D 要打掉的两项——派发队列锁争用和每条消息一次的 worker 停放 / 唤醒——都降了约 8 倍（X 约 24.6 万次 → D 约 3.1 万次），mutex profile 里派发队列的慢路径 CPU 0.17 → 0.11 核。
- 但调度器找活只从 2.31 降到 1.82 核：剩下的阻塞主要是 Guard 作用域与当前消息两个按 goroutine ID 登记的全局 `sync.Map`（Go 1.24 起的 `HashTrieMap`，`Store` / `Delete` 取节点锁；8 个 worker 的 goroutine ID 固定、频繁删了再插，同一节点上争用），每次阻塞都会让空闲 P 去偷活空转。把它们换成分片表后（D+shard）阻塞降到 4.3 万次、找活降到 0.93 核。
- `dispatchNest` 在 D 下反而从 0.81 升到 1.17 核：同样 50 万条消息，worker 不再频繁停放，时间更多算在执行里（缓存更热的收益没有抵消争用）；它本身就是 Y 比 D 少掉的那部分（Y 0.22 核，每 64 个回调一次）。
- 本机是 macOS：`runqgrab` 在偷 runnext 前 `usleep(3)`，在 profile 里计入 CPU（占 X 的 52%）。Linux 上同一处是 `nanosleep`，不应占这么多 CPU，**X 与 D 的绝对核数在 Linux 上很可能低得多，D 相对 X 的比例也可能不同**，没有测。

## 6. 对普通短 handler 的影响（纯 Nest 微基准，不经心跳）

纯 Nest，不经心跳，改前 `be5a76f5` 与最终代码（分支 `perf/nest-dispatch-batch`）交替 6 遍、每遍前门控（[micro-benchstat.txt](benchmarks/nest-dispatch-batch-20261021/micro-benchstat.txt)），GOMAXPROCS 4 与 10。只有 worker 循环改动影响这些路径（批量就绪链 / 提升 / 多取在没有批量作业时不起作用）。

| 基准 | sec/op 变化 | 完成 p99 变化 |
| --- | --- | --- |
| `DispatchCompare/noop`（空 handler，均匀 / 碰撞 / 热点 ID） | −6% ～ −17%（p ≤ 0.04） | −14% ～ −31%（6 项中 3 项显著，其余不显著） |
| `DispatchCompareProducers`（1 / 16 个生产者，空 handler） | −15% ～ −17% | −27% ～ −30%（p1），p16 不显著 |
| `DispatchCompareMicro/cpu_10us`、`cpu_50us` | 不显著 ～ −4% | 不显著 |
| `ClientRequestSingle`（同步 Request 往返） | 不显著（+0.6% / +1.4%，p ≥ 0.09） | — |
| `NestHotPath` 单目标 8 worker（同步 / 异步） | 不显著 ～ −5% | — |
| 几何平均 | **−7.7%** | −13.8% |

B/op、allocs/op 与改前完全相同（177 B / 4 次；Request 1.5 KiB / 35 次）。第一版代码（rebase 前的中间提交，未入库）把 worker 的 `fctx` 选项放进了每条消息调用的 `run()`，选项闭包逃逸，每条消息多 2 次分配、+48 B；最终代码每个 worker 只建一次选项，已恢复。第一组心跳对比是在这一版上测的（X 与 D 同样受影响，第二组在修正后重测）。

结论：worker 循环“结束后同一次持锁取下一项”对普通短 handler 是净收益（尤其空 / 极短 handler 与多生产者），对 10µs 以上的 handler 无可测影响，同步 Request 无变化。

## 7. 达标判定与推荐

**不达标**：语义不变、业务 p99 与 X 相当，但 CPU 只比 X 少 10～12%，离 Y 差 2.7 核（100% 移动）。原因见 §5：X 的成本主要不是“唤醒与抢队列锁”，而是逐作业的 Nest 执行开销及其引起的调度器空转，D 在设计上保留每作业的上下文 / Guard / 事务，拿不到 Y 的摊销。

需要维护者决定（推荐 A）：

| 选项 | 做法 | 代价 / 收益 |
| --- | --- | --- |
| **A（推荐）** | 心跳保持 X；`DispatchBatch`、批量就绪链、提升、一次多取撤回；**只保留 worker 循环“结束后同一次持锁取下一项”**（并修好的选项构造） | 短 handler −7.7% sec/op、心跳 X 100% 移动 4.06 → 3.80 核；代码只多一个 `run` 拆分，`dispatch_queue.go` 回到单一就绪链。撤回需要一个提交（本分支是完整原型，可以从它拆） |
| B | 全部合入但不导出（`DispatchBatch` 改小写、心跳仍用 X），留作以后批量心跳作业的底座 | 派发队列多出两条就绪链、提升和组执行（`dispatch_queue.go` +270 行净增，`client.go` / `dispatcher.go` +150 行，14 个用例约 700 行），心跳收益 0.14～0.4 核但没人用；增加 Nest（三大块）的维护面 |
| C | 合入并让心跳切到 D（加 `batchDeliverer`） | CPU −10～12%、驱动 −80%，业务 p99 相当；快池等待峰值 2～5 倍；判据未达标，不推荐 |

按协调方要求，D 达标才在 `gameplay/heartbeat` 里加批量入队版 deliverer 并用正式心跳驱动复测 X 对 D；**本轮不达标，没有做**。正式驱动的 handler 带事务，逐作业成本更高，D 相对 X 的比例只会更小。

降 CPU 的下一步（另起，需维护者批准）：

1. **两个按 goroutine ID 登记的 `sync.Map`**（`entity.guardScopes`、`nest.currentNestDispatchMsgs`）换成分片读写锁表（`fctx` 已是这种做法）。实验补丁在本测试里让 X 100% 移动 3.80 → 3.28 核（−14%），对所有 Nest 消息生效，改动在 entity / nest 两个包、语义不变。需要单独的回归与 `-race`。
2. 每条消息的指标：`observeDispatch` 每次拼标签 key（profile 中 0.16～0.34 核 / 50 万条），可缓存。
3. **在 Linux 上复测** X / D / Y：macOS 的 `usleep(3)` 空转可能让本报告高估调度器成本。
4. 若仍需要接近 Y：只能做 HB-STRUCTURES §6 的选项 C（一条作业带一批目标、逐目标在各自 Guard + 事务里执行），它摊销的是上下文、队列与唤醒，不摊销 Guard / 事务；需要另行设计 Nest 契约。

## 8. 不能证明什么

- 只在 macOS / arm64（M5，10 核）上测；Linux 上调度器空转的成本不同（§5 最后一条），绝对核数与 D/X 比例都可能变。
- 心跳回调是几十纳秒的内存计算，handler 是 memory 元信息；真实移动回调（5～20µs、带事务）下逐作业执行占比更大，D 与 X 的差距更小。没有用 `gameplay/heartbeat` 的正式驱动测（不达标，按约定没做）。
- 非独占宿主：门控只能挡住 go 构建 / 测试；第二组有两次长尾，原因尚未确认。每组 3 次。
- profile 与执行追踪各一次，用的是第一版（每条消息多 2 次分配）的二进制；分配修正对 X 与 D 相同，不改变两者的相对关系，但具体核数会有小差别。
- 分片登记表只是定位实验，没有回归测试，不能据此宣称它的收益或正确性。
- 批量就绪链 / 提升的公平性只用确定的小规模用例证明（单 worker 交替、份额）；“提升只看直接前驱”对心跳成立（每个登记至多一拍在途），对一般的批量调用方，同一实体连续多拍的链不会整条提升。
