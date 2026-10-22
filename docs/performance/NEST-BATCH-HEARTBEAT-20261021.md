# D20 选项 C：Nest 批量心跳作业（2026-10-21）

设计见 [FEATURE-2026-10-21-nest-batch-heartbeat](../maintenance/FEATURE-2026-10-21-nest-batch-heartbeat.md)。随仓样本在 [benchmarks/nest-batch-heartbeat-20261021](benchmarks/nest-batch-heartbeat-20261021/)。方案 D 的实验记录（含本分支带过来的“worker 同锁取下一项”的微基准证据）见 [NEST-DISPATCH-BATCH-20261021](NEST-DISPATCH-BATCH-20261021.md)。

## 结论

**2026-10-22 补测完成：不达标，不建议现在把 `heartbeat.delivery` 缺省改为 `batch`。** 维护者 2026-10-22 同意开始压测，按 §4 的步骤在 main `b86c4fa4` 上跑完正式对比（3 轮 + 1 次补跑）、普通 Nest 微基准（交替 10 遍）、profile，另加批大小 / 时间片 / 组大小对照与一组同时段补充轮（§3、§3A～§3D）。进程 CPU 有明显下降但只在一半测量时段里“接近 Y”；业务 p99 在宿主较稳定的时段里是 X 的约 3 倍（0.08 → 0.24ms）；跳拍 / 准入失败 / 快池拒绝全部为 0；普通短 handler 不退化（反而快约 10%）。是否接受“业务 p99 从约 0.08ms 到约 0.2ms”换“进程 CPU 约 −40%～−70%”是产品选择，交维护者决定（§3E 的 A / B）。合入方式不变（维护者 2026-10-21 决定 A：缺省 `dispatch`，`heartbeat.delivery: batch` 开启）。

| 判据 | 状态 | 依据 |
| --- | --- | --- |
| 进程 CPU 明显接近 Y | **部分**：时段 A 成立、时段 B 不够 | 100% 移动：时段 A（第 1～2 轮）X 3.42～3.48 / Y 0.33 / C 0.91 核，C 去掉了 X−Y 差距的 81%；时段 B（第 3 轮起，含补充轮）X 3.03 / Y 0.93 / C 1.76 核，只去掉 60%（§3、§3A） |
| 业务 p99 不差于 X | **不成立** | 时段 B 补充轮 100%：X 0.078 / C 0.235ms（3 轮都在 0.23～0.25）；时段 A 第 2 轮 C 0.113 < X 0.190；20% 移动 3 轮里 C 两次差于 X。差距随时间片变化（100µs 0.19、200µs 0.24～0.25、400µs 0.33ms），是批量作业占着 worker 的时间片决定的（§3B） |
| 跳拍 / 准入失败 / 快池拒绝 | 全部为 0 | 正式 28 次、补充 21 次、对照 15 次、profile 5 次 |
| 普通短 handler 不退化 | 成立 | 普通 Nest 微基准 base `07ea71b9` → main 几何平均 −9.9%（同锁取下一项等改动在内），没有一项变慢（§3C） |
| 语义 | 完成 | 23 条变异核对（§5） |

**宿主时段（如实记录）**：正式对比第 2 轮末尾（00:28 左右，门控看到另一个 agent 的 `go test -count=1 .` 并作废了一次）之后，所有组的进程 CPU 分成两档：“无心跳”基线 0.05 → 0.21～0.22 核，Y 100% 0.33 → 0.93～0.96 核，C 100% 0.91 → 1.76～1.81 核，X 100% 反而 3.42 → 3.03～3.06 核；业务 p99 也整体变化（Y 0.026 → 0.09～0.13ms）。门控看不到非 go 负载，1 分钟 loadavg 在两个时段都是 2.6～5.7，没有对应的跳变。轻负载组（none / Y / C）CPU 翻倍、重负载组（X）不升反降，像是 Go 调度器空转（macOS 上 `runqgrab` 的 `usleep`，§3D profile 里 X 52%、C 35%）的代价随宿主状态变了，**原因尚未确认**；所以结论按“同一时段内的相对位置”给，两个时段分开列，不跨时段比。Linux 上没有测（§4“还应补”仍然有效）。

## 1. 被测与环境

- 被测：分支 `perf/nest-batch-heartbeat`（基于 `07ea71b9`）。对照二进制：改前 `07ea71b9`（X0）。2026-10-22 补测的被测是合入后的 main `b86c4fa4`（X / Y / C 与 final 微基准），X0 与 base 微基准仍是 `07ea71b9`；样本在 [benchmarks/nest-batch-heartbeat-20261021/rerun-20261022](benchmarks/nest-batch-heartbeat-20261021/rerun-20261022/)。
- 机器：Apple M5（10 核），32 GB，macOS / arm64，`go1.27.2`，GOMAXPROCS=10。同机有其他 agent 的构建 / 测试；门控同方案 D（`scripts/foreign.sh` 看到别的 go 构建 / 测试 / 压测进程、`dsymutil` 就等待或作废重跑，看不到非 go 负载）。
- 夹具：[gameplay/heartbeat/internal/bench/delivery_test.go](../../gameplay/heartbeat/internal/bench/delivery_test.go)，口径与 [HB-STRUCTURES §4.2](HB-STRUCTURES-20261021.md#42-负载与测量口径) 相同（5 万实体、100ms 登记、10ms 分辨率、1 个驱动、5000 条/s 业务消息、快池 8 worker、队列 65536、3s 预热 + 15s 测量，handler 是 memory 元信息）。`HB_MODE=c` 每 `HB_BATCH`（缺省 64）个到期登记一次 `DispatchTryBatch`；新增列 `c_converted_per_s`（Nest `DispatchLaneStats.TryBatchConverted` 在测量窗口内的增量 / 秒，即“冲突转单目标”次数 / 秒）与 `c_handed_back_total`。

## 2. 实施中的两次探索性测量（未门控，只用于改设计）

原型完成后各跑过几次 1s 预热 + 3s 测量的探索性运行（宿主 loadavg 约 7.6，未门控，不进结论），据此改了两处设计（设计 §5、§11）：

1. 批量作业与普通作业同一条就绪 FIFO 时，100% 移动下业务 p99 2.1ms（驱动一格一次准入约 78 个批量作业，之后的业务消息等整格）。改为两条就绪链交替取后同条件下 0.16ms。
2. 逐目标取放派发队列锁时，CPU profile 里 `Mutex.lockSlow` 累计约占 13%（全部在批量作业的执行循环里）。改为 8 个一组认领后，同条件下进程 CPU 1.32 → 0.95 核（同时段 Y 0.45～0.54、X 3.58 核）。剩下的大头是 Go 调度器找活（`findRunnable` 65%，其中 macOS 上 `runqgrab` 的 `usleep` 53%），与方案 D 报告 §5 的观察相同；Linux 上没有测。

## 3. 2026-10-22 补测：正式对比（3 轮，中位数 [最小–最大]）

按 §4 的脚本跑（`run-formal.sh` 3 轮、轮内轮转；每次运行前等 30 秒无外来 go 进程，运行中采到就作废重跑；本轮作废 1 次“无心跳”基线）。第 1 轮 C 100% 那一次与本 agent 自己的一次短编译 / 基准重叠、门控没采到，已剔除并在最后补跑一次（r4，[redo-c100.sh](benchmarks/nest-batch-heartbeat-20261021/rerun-20261022/redo-c100.sh)）；原始行都在 [formal-raw.jsonl](benchmarks/nest-batch-heartbeat-20261021/rerun-20261022/formal-raw.jsonl)，进表的是 [formal.jsonl](benchmarks/nest-batch-heartbeat-20261021/rerun-20261022/formal.jsonl)，汇总 [formal-summary.md](benchmarks/nest-batch-heartbeat-20261021/rerun-20261022/formal-summary.md)，运行记录（含每次的 loadavg）[formal-runs.log](benchmarks/nest-batch-heartbeat-20261021/rerun-20261022/formal-runs.log)。运行开始时 1 分钟 loadavg 2.89～5.70。

| 组 | 移动 | 次数 | 进程 CPU 核 | 驱动 CPU 核 | 业务 p99 ms | 业务 p99.9 ms | 迟到 p50 / p99 ms | 间隔 p99 ms | 快池等待峰值 | 冲突转单目标/s |
| --- | ---: | ---: | --- | --- | --- | --- | --- | --- | --- | --- |
| 无心跳 | — | 3 | 0.05 [0.05–0.22] | 0 | 0.027 [0.026–0.097] | 0.04 [0.04–0.24] | — | — | 3 [1–17] | — |
| X0（改前） | 20% | 3 | 0.97 [0.94–1.74] | 0.167 [0.149–0.318] | 0.222 [0.091–0.671] | 0.59 [0.34–1.42] | 2.15 / 3.14 | 100.7 | 971 | — |
| X | 20% | 3 | 0.85 [0.82–1.67] | 0.153 [0.152–0.329] | 0.059 [0.056–0.235] | 0.19 [0.18–0.81] | 1.91 / 2.81 | 100.4 | 697 | — |
| Y | 20% | 3 | 0.25 [0.13–0.40] | 0.007 [0.003–0.014] | 0.095 [0.037–0.100] | 0.28 [0.12–0.36] | 1.14 / 1.77 | 100.5 | 15 | — |
| C | 20% | 3 | 0.33 [0.24–0.60] | 0.024 [0.015–0.050] | 0.099 [0.065–0.157] | 0.36 [0.11–0.37] | 1.29 / 2.58 | 100.9 | 33 | 0.5 [0.4–0.6] |
| X0（改前） | 100% | 3 | 4.35 [4.21–4.38] | 0.770 [0.702–0.800] | 0.375 [0.369–0.453] | 1.00 [0.85–1.75] | 6.29 / 10.70 | 101.2 | 1978 | — |
| X | 100% | 3 | 3.42 [3.06–3.48] | 0.757 [0.635–0.775] | 0.159 [0.090–0.190] | 0.75 [0.73–0.79] | 6.38 / 10.50 | 101.4 | 1350 | — |
| Y | 100% | 3 | 0.33 [0.33–0.96] | 0.015 [0.015–0.086] | 0.026 [0.026–0.133] | 0.04 [0.04–1.25] | 2.63 / 2.89 | 100.2 | 77 | — |
| C | 100% | 3 | 1.79 [0.91–1.81] | 0.246 [0.119–0.251] | 0.238 [0.113–0.272] | 0.55 [0.18–1.19] | 5.93 / 8.69 | 102.3 | 77 | 3.7 [1.3–3.9] |

跳拍 / 准入失败 / 业务失败 / 快池拒绝全部为 0。中位数跨了两个宿主时段（见“结论”），按轮看（进程 CPU 核 / 业务 p99 ms）：

| 移动 | 轮（时段） | X0 | X | Y | C |
| --- | --- | --- | --- | --- | --- |
| 20% | r1（A） | 0.94 / 0.091 | 0.82 / 0.059 | 0.13 / 0.037 | 0.33 / 0.099 |
| 20% | r2（A） | 0.97 / 0.222 | 0.85 / 0.056 | 0.25 / 0.100 | 0.24 / 0.065 |
| 20% | r3（B） | 1.74 / 0.671 | 1.67 / 0.235 | 0.40 / 0.095 | 0.60 / 0.157 |
| 100% | r1（A） | 4.35 / 0.369 | 3.48 / 0.159 | 0.33 / 0.026 | （剔除） |
| 100% | r2（A） | 4.38 / 0.375 | 3.42 / 0.190 | 0.33 / 0.026 | 0.91 / 0.113 |
| 100% | r3（B） | 4.21 / 0.453 | 3.06 / 0.090 | 0.96 / 0.133 | 1.79 / 0.272 |
| 100% | r4 补跑（B） | — | — | — | 1.81 / 0.238 |

### 3A. 同时段补充轮（100% 移动，3 轮轮转）

为了在同一宿主时段里确认相对位置，正式对比、微基准、profile、对照组与第 2 组压测之后又跑了一组（[run-supp.sh](benchmarks/nest-batch-heartbeat-20261021/rerun-20261022/run-supp.sh)，[supp-summary.md](benchmarks/nest-batch-heartbeat-20261021/rerun-20261022/supp-summary.md)；loadavg 2.01～3.46，时段 B，3 轮之间很稳定）：

| 组 | 进程 CPU 核 | 驱动 CPU 核 | 业务 p50 / p99 / p99.9 ms | 迟到 p99 ms | 快池等待峰值 | 冲突转单目标/s |
| --- | --- | --- | --- | --- | --- | --- |
| 无心跳 | 0.21 [0.21–0.21] | 0 | 0.029 / 0.071 / 0.15 | — | 3 | — |
| X | 3.03 [2.99–3.04] | 0.623 | 0.014 / 0.078 / 0.33 | 10.30 | 1058 | — |
| Y | 0.93 [0.93–0.94] | 0.075 | 0.027 / 0.089 / 0.97 | 6.33 | 75 | — |
| C（组 8、时间片 200µs、批 64，即当前实现） | 1.76 [1.74–1.78] | 0.243 | 0.022 / 0.235 [0.232–0.252] / 0.57 | 8.68 | 71 | 3.6 |
| C 时间片 100µs | 1.72 [1.70–1.77] | 0.220 | 0.021 / 0.189 [0.188–0.193] / 0.52 | 8.26 | 75 | 2.3 |
| C 组 4 | 1.67 [1.67–1.72] | 0.153 | 0.021 / 0.187 [0.178–0.188] / 0.41 | 8.78 | 75 | 3.2 |
| C 组 16 | 1.77 [1.64–1.79] | 0.299 | 0.023 / 0.316 [0.302–0.325] / 0.79 | 8.45 | 103 | 3.7 |

同一时段内：C 比 X 省 42% 进程 CPU（3.03 → 1.76 核），离 Y 还差 0.83 核（X 与 Y 差 2.10 核）；业务 p99 是 X 的 3 倍。驱动 CPU 只有 X 的 39%。

### 3B. 批大小 / 时间片对照（100% 移动，3 轮轮转，时段 B）

[run-sweep.sh](benchmarks/nest-batch-heartbeat-20261021/rerun-20261022/run-sweep.sh)，[sweep-summary.md](benchmarks/nest-batch-heartbeat-20261021/rerun-20261022/sweep-summary.md)；批大小用 `HB_BATCH`，时间片是常量 `tryBatchSlice`（`framework/nest/try_batch.go`），改常量另编的二进制（未入库）；组大小 `tryBatchGroup` 同样另编（§3A 的 g4 / g16）。loadavg 2.56～4.55。

| 配置 | 准入/s | 进程 CPU 核 | 驱动 CPU 核 | 业务 p99 / p99.9 ms | 迟到 p99 ms | 冲突转单目标/s |
| --- | --- | --- | --- | --- | --- | --- |
| 批 32 | 15680 | 1.84 [1.81–1.89] | 0.339 | 0.195 / 0.46 | 8.73 | 2.1 |
| 批 64（当前） | 7865 | 1.88 [1.78–1.90] | 0.260 | 0.254 / 0.66 | 8.90 | 4.0 |
| 批 128 | 3970 | 1.91 [1.80–1.92] | 0.183 | 0.282 / 0.69 | 8.68 | 3.9 |
| 时间片 100µs（批 64） | 7870 | 1.86 [1.74–1.92] | 0.251 | 0.215 / 0.53 | 9.21 | 2.8 |
| 时间片 400µs（批 64） | 7870 | 1.86 [1.83–1.91] | 0.270 | 0.328 / 0.77 | 8.64 | 4.2 |

读法：进程 CPU 在这几个参数上几乎不动（1.84～1.91 核，噪声内）；业务 p99 跟着“一个 worker 一次被批量作业占住多久”走——时间片越长、批越大、组越大，p99 越高（时间片 100 / 200 / 400µs → 0.215 / 0.254 / 0.328ms；组 4 / 8 / 16 → 0.187 / 0.235 / 0.316ms）；批越小驱动 CPU 越高（准入次数翻倍）。**参数建议**：批大小 64 不用改（32 只在 p99 上好 0.06ms、驱动 CPU +30%，128 两头都不好）；时间片改 100µs、组改 4 各能把 p99 降约 20%，CPU 不变或略降，但都到不了 X 的 0.08ms；两者合起来没有测。

### 3C. 普通 Nest 微基准（base `07ea71b9` / final `b86c4fa4`，交替 10 遍，`-test.cpu=4,10`）

[micro-benchstat.txt](benchmarks/nest-batch-heartbeat-20261021/rerun-20261022/micro-benchstat.txt)（原始 `micro-base.txt` / `micro-final.txt`，loadavg 3.13～7.20，无作废）：

| 基准 | 4 核 | 10 核 |
| --- | --- | --- |
| `DispatchCompare/noop` uniform / colliding_ids / hot_id | −18.2% / −18.6% / −11.3% | −10.3% / −10.9% / −11.4% |
| `DispatchCompareMicro` cpu_10us / cpu_50us | −2.1% / −1.3% | −4.3% / −1.6% |
| `DispatchCompareProducers` p1 / p16 | −17.0% / −18.3% | −18.6% / −15.1% |
| `ClientRequestSingle` | −2.7% | −1.6% |
| `NestHotPath` single async=false / true | −2.7% / −11.0% | −7.0% / −10.4% |
| 几何平均（sec/op） | −9.9%（completed/s +13.1%） | |

全部 p=0.000～0.009，没有一项变慢。这一组量的是 `07ea71b9` → main 的总变化（`take` 多一次“批量链是否为空”判断、worker 同锁取下一项、分片登记表都在里面），不能把收益单独归给某一项。

### 3D. profile（100% 移动，与正式测量分开，各 10 秒）

`run-prof.sh`，文本在 [rerun-20261022/prof](benchmarks/nest-batch-heartbeat-20261021/rerun-20261022/prof/)。profile 运行本身的 CPU 是 X 3.09 / C 1.81 / Y 1.00 / X0 3.95 核（时段 B），不进正式表。

| | X（单目标 `Dispatch`） | C（批量作业） |
| --- | --- | --- |
| 采样总量 | 38.2s / 13.2s = 2.89 核 | 22.9s / 13.2s = 1.73 核 |
| 调度器找活 `findRunnable`（其中 `runqgrab` 的 `usleep`） | 72%（52%） | 44%（35%） |
| worker 执行 `dispatchQueue.work` | 20%（`dispatchNest` 11.6%） | 48%（`runTryBatchLocked` 47%，逐目标执行 `tryBatchExec.run` 39%） |
| 逐目标开销里较大的几项 | — | `entity.NewGuardScope` 9.3%、`tryLockTryBatchTarget` 8.8%、`fctx.CurrentContext`（`InIOWorker` 的按 goroutine 查表）6.9%、`invokeTryBatchTransaction` 7.3% |
| mutex 等待（`-mutexprofilefraction 5`） | 4.2s，主要在 `Dispatch` 准入的 `q.mu` | 8.6s，93% 在 worker 的 `runTryBatchLocked`（按组认领 / 结束时取 `q.mu`） |

C 把调度器空转从 X 的大头降下来了（CPU 的主要收益来源），剩下的成本一半在逐目标执行（实体锁、guard scope、fctx 查表、事务包装），另有 8 个 worker 按组争派发队列锁。组 4 比组 8 的 p99 好、CPU 略低（§3A），与这个锁的争用方向不一致（组越小取锁越多），说明 p99 主要由“一次占用多久”决定、不是锁。

### 3E. 是否把缺省改为 `batch`（需要维护者决定）

- **A（推荐）：暂不改，缺省仍 `dispatch`**。判据“业务 p99 不差于 X”不成立（同时段 0.08 → 0.24ms），CPU 判据只在一半时段成立。下一步可做：时间片 100µs + 组 4 合起来测一次；逐目标开销（guard scope、fctx 查表）与按组取锁的优化；Linux 上复测（macOS 的 `usleep` 计入 CPU，可能高估 X 的调度器成本，也就高估了 C 的收益）；真实移动回调（5～20µs、带事务）下复测。代价：心跳仍多花约 1.3～2.5 核（5 万实体 100ms、100% 移动的规模）。
- **B：改缺省为 `batch`，同时把时间片调到 100µs（或组调到 4）**。进程 CPU 约 −40%（时段 B）～ −70%（时段 A），代价是普通业务消息 p99 从约 0.08ms 升到约 0.19ms（p99.9 0.33 → 0.4～0.5ms），都在亚毫秒；Remote 托管实体照旧单目标。需要先补测“两项参数合起来”的一组。

## 3'. 暂停前采到的第 1 轮（2026-10-21，每组 1 次，不作结论，保留对照）

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

2026-10-22 已按本节补测完成（结果见 §3～§3E；标题保留以免外部链接失效）。下面“还应补”的 Linux 复测与组大小 / 批大小对照：后者已做（§3A、§3B），Linux 仍未做。

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

新功能没有“修前”，红用变异核对：把实现改成一个具体的错误版本，确认对应用例变红，再改回（脚本 `scripts/mutate.py` + `scripts/muts-final.json`，在最终代码上一次重跑；原文 [mutations.txt](benchmarks/nest-batch-heartbeat-20261021/mutations.txt)）。23 条，22 条变红：

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
| S1 | `SetDelivery` 不检查已启动 | `TestDeliveryIsChosenOnceBeforeStart` | `SetDelivery changed the delivery of a running driver` |
| S2 | Init 不把 `heartbeat.delivery` 交给驱动 | `TestModReadsTheDeliveryOnce` | `heartbeat.delivery="batch": driver delivery "dispatch", want "batch"` |
| S3 | 批量方式下客户端不支持时静默退回单目标 | `TestDeliveryIsChosenOnceBeforeStart` | `Bind a client without try-batch support in batch delivery = <nil>` |
| F1 | `Refreeze` 不清 `Frozen` | `TestRefreezeStartsANewExecutionInTheSameContext` | `Frozen after Refreeze = 1, the previous execution's value` |
| F2 | 批量作业逐目标不 `Refreeze` | `TestEveryBeatFreezesItsOwnBusinessTime/batch` | `r0 and r1 share Beat.Now ...: each beat must freeze its own business time` |
| G1 | 去掉 `Reg.GoString`（慢派发 trace 按反射打印 `*Reg`） | `TestRegFormatsWithoutReadingMutableState`（`-race`） | `WARNING: DATA RACE` |

“worker 同锁取下一项”的两条变异（持锁调用出队后动作死锁、出队后动作早于结束作业）与分片登记表的一条变异（`Delete` 清空整片）记在各自的提交说明里。

## 6. 不能证明什么

- 2026-10-22 的结论只对本机、本夹具成立；宿主在测量中途换了一个“时段”（见结论），两个时段的绝对值不能互比，原因尚未确认。§2 未门控、§3' 只有 1 轮，都不作结论。
- 只在 macOS / arm64 上跑过；回调是几十纳秒的内存计算、handler 是 memory 元信息，真实移动回调（5～20µs、带事务）下 C 与 X 的差距会缩小。
- 冲突转单目标在补测里每秒 0.4～4.2 次（第 1 轮 0.1～3.7 次）（5000 条/s 业务消息打到 5 万实体），热点实体（同一实体高频业务消息）下的转换率与业务延迟没有测。
