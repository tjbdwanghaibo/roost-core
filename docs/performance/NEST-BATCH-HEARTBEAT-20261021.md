# D20 选项 C：Nest 批量心跳作业（2026-10-21）

设计见 [FEATURE-2026-10-21-nest-batch-heartbeat](../maintenance/FEATURE-2026-10-21-nest-batch-heartbeat.md)。随仓样本在 [benchmarks/nest-batch-heartbeat-20261021](benchmarks/nest-batch-heartbeat-20261021/)。方案 D 的实验记录（含本分支带过来的“worker 同锁取下一项”的微基准证据）见 [NEST-DISPATCH-BATCH-20261021](NEST-DISPATCH-BATCH-20261021.md)。

## 结论

**压测暂停，待补。** 合入方式按维护者 2026-10-21 决定 A：代码先合入，心跳缺省仍走单目标 `Dispatch`，批量方式是开关 `heartbeat.delivery: batch`（缺省关闭），本篇的补测（“合批优化压测”）通过后另起一笔改为缺省开启。补测时 C 组用 `HB_MODE=c`（夹具直接调 `DispatchTryBatch`，不经心跳开关）。 维护者 2026-10-21 18:55 要求先暂停压测、优先把代码做完。正式对比（X0 / X / Y / C × 20% / 100% 移动 × 3 轮）只跑完第 1 轮就停了，普通 Nest 微基准与 profile 没有开始。第 1 轮每组只有 1 次、宿主负载高（运行开始时 1 分钟 loadavg 8～10），**不作为结论**，只记录在 §3 供补测时对照。达标判定（进程 CPU 明显接近 Y、业务 p99 不差于 X、跳拍 / 准入失败 / 快池拒绝为 0 或可解释、普通短 handler 不退化）要等 §4 补测完成。

| 判据 | 状态 |
| --- | --- |
| 进程 CPU 明显接近 Y | 待补测 |
| 业务 p99 不差于 X | 待补测 |
| 跳拍 / 准入失败 / 快池拒绝 | 第 1 轮全部为 0（不作结论） |
| 普通短 handler 不退化 | 待补测（`take` 多了一次“批量链是否为空”的判断，worker 同锁取下一项与分片登记表对短 handler 的影响也要在同一组里测） |
| 语义 | 完成：17 条变异核对（§5） |

## 1. 被测与环境

- 被测：分支 `perf/nest-batch-heartbeat`（基于 `07ea71b9`）。对照二进制：改前 `07ea71b9`（X0）。
- 机器：Apple M5（10 核），32 GB，macOS / arm64，`go1.27.2`，GOMAXPROCS=10。同机有其他 agent 的构建 / 测试；门控同方案 D（`scripts/foreign.sh` 看到别的 go 构建 / 测试 / 压测进程、`dsymutil` 就等待或作废重跑，看不到非 go 负载）。
- 夹具：[gameplay/heartbeat/internal/bench/delivery_test.go](../../gameplay/heartbeat/internal/bench/delivery_test.go)，口径与 [HB-STRUCTURES §4.2](HB-STRUCTURES-20261021.md#42-负载与测量口径) 相同（5 万实体、100ms 登记、10ms 分辨率、1 个驱动、5000 条/s 业务消息、快池 8 worker、队列 65536、3s 预热 + 15s 测量，handler 是 memory 元信息）。`HB_MODE=c` 每 `HB_BATCH`（缺省 64）个到期登记一次 `DispatchTryBatch`；新增列 `c_converted_per_s`（Nest `DispatchLaneStats.TryBatchConverted` 在测量窗口内的增量 / 秒，即“冲突转单目标”次数 / 秒）与 `c_handed_back_total`。

## 2. 实施中的两次探索性测量（未门控，只用于改设计）

原型完成后各跑过几次 1s 预热 + 3s 测量的探索性运行（宿主 loadavg 约 7.6，未门控，不进结论），据此改了两处设计（设计 §5、§11）：

1. 批量作业与普通作业同一条就绪 FIFO 时，100% 移动下业务 p99 2.1ms（驱动一格一次准入约 78 个批量作业，之后的业务消息等整格）。改为两条就绪链交替取后同条件下 0.16ms。
2. 逐目标取放派发队列锁时，CPU profile 里 `Mutex.lockSlow` 累计约占 13%（全部在批量作业的执行循环里）。改为 8 个一组认领后，同条件下进程 CPU 1.32 → 0.95 核（同时段 Y 0.45～0.54、X 3.58 核）。剩下的大头是 Go 调度器找活（`findRunnable` 65%，其中 macOS 上 `runqgrab` 的 `usleep` 53%），与方案 D 报告 §5 的观察相同；Linux 上没有测。

## 3. 暂停前采到的第 1 轮（每组 1 次，不作结论）

[paused-round1-summary.md](benchmarks/nest-batch-heartbeat-20261021/paused-round1-summary.md)（原始 [paused-round1.jsonl](benchmarks/nest-batch-heartbeat-20261021/paused-round1.jsonl)、运行记录 [paused-round1-runs.log](benchmarks/nest-batch-heartbeat-20261021/paused-round1-runs.log)）：

| 组 | 移动 | 进程 CPU 核 | 驱动 CPU 核 | 业务 p99 ms | 迟到 p50 / p99 ms | 冲突转单目标/s | 跳拍 / 准入失败 / 快池拒绝 |
| --- | ---: | ---: | ---: | ---: | --- | ---: | --- |
| X0（改前） | 20% | 1.18 | 0.218 | 0.978 | 2.27 / 7.53 | — | 0 / 0 / 0 |
| X | 20% | 1.10 | 0.211 | 0.196 | 2.55 / 5.61 | — | 0 / 0 / 0 |
| Y | 20% | 0.31 | 0.010 | 0.122 | 1.12 / 1.81 | — | 0 / 0 / 0 |
| C | 20% | 0.45 | 0.037 | 0.100 | 1.92 / 2.99 | 0.1 | 0 / 0 / 0 |
| X0（改前） | 100% | 4.16 | 0.696 | 0.563 | 7.93 / 16.0 | — | 0 / 0 / 0 |
| X | 100% | 3.24 | 0.700 | 0.875 | 7.62 / 78.8 | — | 0 / 0 / 0 |
| Y | 100% | 0.44 | 0.031 | 0.178 | 4.37 / 7.01 | — | 0 / 0 / 0 |
| C | 100% | 1.33 | 0.162 | 0.172 | 5.89 / 7.61 | 3.7 | 0 / 0 / 0 |

X 100% 这一次的迟到 p99 78.8ms、间隔 p99 128ms 是长尾（方案 D 报告里 X 也出现过同类离群，原因尚未确认）。第 1 轮的“无心跳”基线被门控作废（同时有外来 `go build ./...`）后压测即暂停。

## 4. 待补测（复跑步骤）

脚本在 [benchmarks/nest-batch-heartbeat-20261021/scripts](benchmarks/nest-batch-heartbeat-20261021/scripts/)，在一个独立目录 `$S` 里跑（门控按路径排除本目录下自己的二进制）：

```sh
S=<scratch>/hbrun; mkdir -p $S && cp docs/performance/benchmarks/nest-batch-heartbeat-20261021/scripts/* $S/
# 改前对照（X0）与改后二进制
git worktree add --detach <scratch>/basewt 07ea71b9
(cd <scratch>/basewt && GOWORK=off go test -c -o $S/bin-base.test ./gameplay/heartbeat/internal/bench \
   && GOWORK=off go test -c -o $S/nest-base.test ./framework/nest)
GOWORK=off go test -c -o $S/bin-final.test ./gameplay/heartbeat/internal/bench
GOWORK=off go test -c -o $S/nest-final.test ./framework/nest
git worktree remove --force <scratch>/basewt
# 正式对比（3 轮 × 20%/100% × X0/X/Y/C + 每轮无心跳基线）→ 普通 Nest 微基准（base / final 交替 10 遍）→ profile（与正式测量分开）
mkdir -p $S/formal && nohup caffeinate -i $S/chain.sh $S > $S/chain.out 2>&1 &   # chain.sh 等 formal 完成再接 micro 与 prof
nohup caffeinate -i $S/run-formal.sh $S $S/formal 3 > $S/formal/nohup.out 2>&1 &
# 汇总
python3 $S/agg.py $S/formal/delivery.jsonl
/Users/whb/go/bin/benchstat $S/micro/base.txt $S/micro/final.txt
go tool pprof -top -cum $S/bin-final.test $S/prof/C.cpu
```

- `run-formal.sh`：每次运行前等 30 秒没有外来 go 进程，运行中每 5 秒检查，采到就作废重跑；轮内轮转顺序。约 27 次 × 50 秒。
- `run-micro.sh`：方案 D §6 的同一组基准（`DispatchCompare` noop 三种 ID 分布、`DispatchCompareMicro` 10 / 50µs、`DispatchCompareProducers` 1 / 16、`ClientRequestSingle`、`NestHotPath` 单目标 8 worker），`-test.cpu=4,10`，base / final 交替 10 遍（maintenance §8 第 4 条要求 count ≥ 10）。
- `run-prof.sh`：100% 移动的 X、C、Y、X0 与 20% 移动的 C，各一次 10s CPU + mutex profile；计数写 `profile-runs.jsonl`，不进正式表。
- 补测后要写的：本篇 §3 换成 3 轮中位数 [最小–最大] 的正式表（含冲突转单目标/s），profile 前后对比（X 对 C：调度器找活、`dispatchNest` / 批量作业执行、mutex 慢路径），微基准 benchstat，达标判定；同步设计 §16、DESIGN-TODO D20、KNOWN-LIMITS 与 CHANGELOG 的性能结论栏。
- 还应补：Linux 上复测 X / C / Y（macOS 的 `runqgrab` `usleep` 计入 CPU，可能高估调度器成本）；批大小 32 / 64 / 128 与组大小 4 / 8 / 16 的对照（`HB_BATCH`；组大小是 `tryBatchGroup` 常量，要改源码重编）。

## 5. 语义：变异核对

新功能没有“修前”，红用变异核对：把实现改成一个具体的错误版本，确认对应用例变红，再改回（脚本 `scripts/mutate.py` + `muts-all.json` / `muts-more.json`，原文 [mutations.txt](benchmarks/nest-batch-heartbeat-20261021/mutations.txt)）。17 条，16 条变红：

| 编号 | 变异 | 用例 | 修前（变异后）失败文本 |
| --- | --- | --- | --- |
| N1 | 冲突时直接执行而不排队 | `TestTryBatchConflictRunsAfterAdmittedSameIDJob` | `beat on A ran before the admitted same-ID jobs: [beat:A beat:C]` |
| N2 | 执行期间不认领 | `TestTryBatchClaimQueuesLaterSameIDJobBehindTheBeat` | `while the beat runs: running=2 blocked-on-predecessor=0, want 1 and 1 ...` |
| N3 | 转单目标被准入拒绝当作转成功 | `TestTryBatchConversionRejectedIsHandedBack` | `timed out waiting for hand-back` |
| N4 | 各目标共用第一项 Params | `TestTryBatchKeepsPerTargetTransactionSemantics` | `values e1=12 e2=11 e3=11, want 12 10 11 (e2 rolled back alone)` |
| N5 | 不检查时间片 | `TestTryBatchYieldsWorkerAfterTimeSlice` | `ordinary message ran at position 20 behind the batch backlog ...` |
| N6 | 不检查 fence | `TestTryBatchRefusesTargetsAfterFence` | `timed out waiting for hand-back`（handler 照常执行） |
| N7 | 批量与普通作业同一条 FIFO | `TestTryBatchAlternatesWithOrdinaryJobs` | `ordinary message waited behind the batch backlog: [beat ×10 biz]` |
| N8 | 两类都有时总先取普通作业 | `TestTryBatchAlternatesWithOrdinaryJobs` | `batch job starved behind the ordinary backlog: [biz ×10 beat]` |
| N9 | 批量作业不计入 pending | `TestTryBatchDrainsOnShutdown` | `shutdown did not drain` |
| N10 | 去掉执行器的 `loadedGetter` | `TestTryBatchReadsOnlyLoadedTargets` | **仍绿**：强制点是 `entity.LoadedEntitiesOnly` 对业务 worker 身份的判断，包装只是防御；用例按契约检查保留 |
| N11 | 慢池目标也进 short | `TestTryBatchHandBackReasonsAndPoolRouting` | `pools [long-entity on short ...], want [long-entity on long ...]` |
| N12 | Await 不拒绝批量作业 | `TestTryBatchHandBackReasonsAndPoolRouting` | `pools [... await=allowed ...], want [... await=rejected ...]` |
| H1 | Nest 冲突直接执行 | `TestConflictingBeatRunsRightAfterTheAdmittedMessage` | `events [beat]: the beat ran before the already admitted message` |
| H2 | 驱动不看在途标志 | 同上 | `stats {Delivered:3 Executed:1 ... DroppedDuplicate:2 ...}` |
| H3 | 交回只清在途不计跳拍 | `TestConflictingBeatIsSkippedOnlyWhenAdmissionRefuses` | `stats {... SkippedQueueFull:0 ...} inflight=false warns=0` |
| H4 | Remote 托管实体也走批量 | `TestRemoteHeartbeatFollowsOwnershipWithoutDoubleBeats` | `remote entity: invalid ownership transition: unknown -> draining` |
| H5 | “不在内存”按跳拍计 | `TestBeatOfAnUnloadedInstanceIsDroppedWithoutColdLoad` | `runs 0 stats {... SkippedRejected:1 ... DroppedNotLoaded:0 ...}` |

“worker 同锁取下一项”的两条变异（持锁调用出队后动作死锁、出队后动作早于结束作业）与分片登记表的一条变异（`Delete` 清空整片）记在各自的提交说明里。

## 6. 不能证明什么

- 没有正式性能结论（§3 只有 1 轮，§2 未门控）。
- 只在 macOS / arm64 上跑过；回调是几十纳秒的内存计算、handler 是 memory 元信息，真实移动回调（5～20µs、带事务）下 C 与 X 的差距会缩小。
- 冲突转单目标在第 1 轮里每秒只有 0.1～3.7 次（5000 条/s 业务消息打到 5 万实体），热点实体（同一实体高频业务消息）下的转换率与业务延迟没有测。
