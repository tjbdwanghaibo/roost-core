# Interest 单写者重构前后对比（2026-10-22）

方案与验证计划见 [REFACTOR-2026-10-21-interest-single-writer §6](../maintenance/REFACTOR-2026-10-21-interest-single-writer.md#6-性能预期与验证压测之后由维护者统一安排)。随仓样本在 [benchmarks/interest-single-writer-20261022](benchmarks/interest-single-writer-20261022/)。

## 结论

| 方案 §6 的预期 | 结果 |
| --- | --- |
| 1. 登记侧入口与基线同量级或更快，AOI 基准不变 | **成立**。政策阶段一直在跑时业务线程上的一次直接调用：ApplySync 下 `Enter`+`Leave` 均值 23.4µs → 0.47µs、p99 55.9µs → 1.5µs、最大 4.1ms → 1.8ms；`SetObserverView` 最大 2.85ms → 17µs。AOI / Cluster 基准全部在 ±2% 内。视野登记表一类的小操作变慢（每次多一条日志、多 2～3 次分配，例如 `SharedSpatialView/players=1` 226 → 451ns），绝对值是纳秒级 |
| 2. Nest 两种模式 P99 不高于改前；ApplySync 16 视野仍超周期 | **成立（ApplySync 有改善，ApplyAsync 持平）**。原场景 Nest ApplySync 0 / 4 / 8 视野 P50 低 6～11ms、P99 低 4～8ms、新订阅全量 P99 低 19～32ms；ApplyAsync 各项在噪声内。16 视野 ApplySync（诊断构建）P99 126 → 128ms、政策阶段每 tick P95 29.7 → 38.1ms、帧数 197000 → 194991，仍超周期，且比改前略差（见 §4） |
| 3. demo 登录风暴 handler P99 不再有 AOI 批次长度的尾巴 | **规模受限，没有可分辨的差异**（§6）：80 个机器人同时登录（更多时 demo 的复制会话因 `MaxObjects` 被关闭），进场景 handler 改后 3 次均值 0.10～0.32ms、改前 0.02～8.28ms（改前一次 8.28ms、最大 18.9ms，另两次与改后同量级）；game 进程只有均值 / 最大，没有 handler 的 P99 直方图 |
| 业务 p99 / CPU | 服务端 CPU 秒数前后相同（±0.7s / 次，单次约 15s）；`ErrInterestQueueFull` 0（夹具里任何 `QueueMove` 错误都会让运行失败，所有运行都正常结束）；队列峰值夹具不导出，没有数 |

另有两项要维护者知道：

1. **压测程序本身的问题，已修**（`6cffbc03`，只改夹具）：sync-aoi 的 Nest 模式每个 tick 末尾调一次 `interest.Apply()`。重构后 `Apply` 会在调用方线程执行已确认的排队移动，于是压测线程替政策阶段做了 AOI，Nest ApplySync 的政策阶段与 Flush 看上去变短（0 视野 Flush 11.0 → 7.0ms、政策 4.6 → 0.8ms），压测线程每 tick 多出 5～17ms。第一遍对照就是这样跑的（原文 [aoi-o-harness-apply-summary.md](benchmarks/interest-single-writer-20261022/aoi-o-harness-apply-summary.md)），作废；改为 Nest 模式不调 `Apply`（业务路径本来也不调，`254d61cf`），两边用同一份改过的压测程序重跑，本页的数都是重跑的。
2. **direct 基线的语义变了**（方案 §5 第 6 条）：direct 模式下直接调用不再在压测线程当场算 AOI，而是进日志、在压测线程调 `Apply()` 时（ApplySync）或由应用线程（ApplyAsync）执行。原场景 direct ApplySync 8 / 16 视野 P99 50.6 → 59.6ms、63.5 → 79.9ms，ApplyAsync 16 视野 61.0 → 72.3ms；压测线程每 tick 耗时 ApplyAsync 下降（16 视野 20.5 → 8.8ms，AOI 去了应用线程）、ApplySync 下不变。**direct 不再是“AOI 全在压测线程”的下界，与改前和 D19 历史表的 direct 行不能直接比**。

宿主负载：sync-aoi 原场景运行开始时 1 分钟 loadavg 1.84～4.73，小块场景 1.87～3.68，policy 基准 1.78～4.00（同时段维护者的其他程序在跑；门控只看得见外来 go 进程）。单机、每个配置 2 轮，P99 的轮间差 1～9ms（16 视野更大），小于这个量级的差异不下结论。

## 1. 被测与方法

- 改前 `494134ae`（方案基线），改后 `11988df8`（main，含不稳定用例的修复 `d3a4486d`：异步模式已执行未结算的命令计入 Drain）。两边的 sync-aoi 是同一份源码（`494134ae..11988df8` 没有改 `scripts/perf/sync-aoi`），加上 `6cffbc03` 的夹具修正；ApplySync 16 视野超过 `SyncApplyMaxExtraViews`（8），两边都用只把它改成 16 的诊断构建（与 D19 历史表同一做法），未入库。
- 机器：Apple M5（10 核）、32 GB、macOS / arm64、go1.27.2；sync-aoi 每个进程 GOMAXPROCS 4。
- sync-aoi：原场景 `-players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50`，`-views-per-player` 0 / 4 / 8 / 16 × `-apply-mode sync|async` × Nest / direct（`-nest=false`）；小块场景另加 `-world=4000 -block=20 -enter-radius=30 -leave-radius=36 -move-amplitude=10 -entities=50000`，4 / 8 视野。每次 200 个测量 tick（约 15 秒）。每个配置 2 轮，轮内轮转、base / after 先后按轮交替；每次运行前等 10 秒没有外来 go 进程，运行中每 2 秒检查并采样服务端 / 客户端进程 CPU（`ps` 累计 CPU 时间，含预热），采到外来进程就作废重跑（本组没有作废）。脚本 [run-aoi.sh](benchmarks/interest-single-writer-20261022/run-aoi.sh)、汇总 [agg-aoi.py](benchmarks/interest-single-writer-20261022/agg-aoi.py)，口径与 [D19“两种应用模式”](../maintenance/DESIGN-TODO.md#两种应用模式2026-10-21维护者决定) 的表相同（改值→客户端 = `state_change_to_client_ms`；Flush 均值 = `FlushDuration / FlushCalls`；政策阶段每 tick P95；压测线程每 tick P50 = `interest.Move` + `Apply`）。
- sync-aoi 进程以非零码退出是它自己的 50ms 延迟门槛（`50ms latency gate failed; reports retained`），报告完整，按正常样本处理；每次运行的 `manager_last_error` 都是停机时的 `sync admission ... context canceled`（两边相同）。
- policy 基准：`go test -c` 出两个测试二进制，`-bench . -benchmem -count=1` base / after 交替 10 遍（[run-policy.sh](benchmarks/interest-single-writer-20261022/run-policy.sh)）。方案 §6 第 1 项要求另加的两条基准写成压测专用文件 [extra_bench_test.go.txt](benchmarks/interest-single-writer-20261022/extra_bench_test.go.txt)（只用两边都有的 API，编进两边的测试二进制，未入库为测试）：
  - `BenchmarkXDirectCallUnderPolicy`：1000 个玩家，后台一直“提交 200 条移动 + Flush”，业务 goroutine 上逐次计时一次 `SetObserverView` 或一对 `Enter` + `Leave`，报均值、p99、最大；
  - `BenchmarkXSyncFlushBacklog`：ApplySync 下积压 4096 条（4 × `BatchSize`）已确认移动，第一次 Flush 的耗时与“第一次 Flush + `Drain`”的总耗时。

## 2. sync-aoi 原场景（两轮均值，改前 → 改后）

| 视野 | 驱动 | 模式 | P50 ms | P99 ms | 帧数（应发 ≤200000） | Flush 均值 ms | 政策阶段 P95/tick ms | 压测线程 P50/tick ms | 新订阅全量 P99 ms | 服务端 CPU s |
| ---: | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 0 | Nest | ApplySync | 35.6 → 29.6 | 62.3 → 57.4 | 90634 → 90540 | 10.8 → 10.7 | 6.9 → 7.2 | 0 → 0 | 55.5 → 36.8 | 4.8 → 4.8 |
| 0 | Nest | ApplyAsync | 34.1 → 34.3 | 61.4 → 61.0 | 90900 → 91100 | 9.1 → 9.2 | 3.5 → 3.7 | 0 → 0 | 56.2 → 57.3 | 5.2 → 5.2 |
| 0 | direct | ApplySync | 31.7 → 32.0 | 50.8 → 51.2 | 91625 → 91625 | 7.3 → 6.1 | 0 → 0 | 6.1 → 6.0 | 50.6 → 51.1 | 3.5 → 3.2 |
| 0 | direct | ApplyAsync | 31.7 → 32.1 | 50.4 → 50.6 | 91625 → 91625 | 7.4 → 7.4 | 0 → 0 | 6.3 → 2.7 | 50.2 → 50.6 | 3.6 → 3.7 |
| 4 | Nest | ApplySync | 39.7 → 30.8 | 65.8 → 58.2 | 190220 → 189988 | 15.9 → 15.9 | 8.5 → 8.7 | 0 → 0 | 67.5 → 37.3 | 8.7 → 8.7 |
| 4 | Nest | ApplyAsync | 36.5 → 36.6 | 63.9 → 64.3 | 190147 → 190169 | 13.0 → 12.8 | 4.3 → 4.2 | 0 → 0 | 61.6 → 62.4 | 9.6 → 9.5 |
| 4 | direct | ApplySync | 32.2 → 35.2 | 49.6 → 50.5 | 190716 → 190716 | 9.5 → 8.5 | 0 → 0 | 8.5 → 7.9 | 49.5 → 50.5 | 6.8 → 6.5 |
| 4 | direct | ApplyAsync | 32.2 → 32.5 | 49.6 → 50.0 | 190716 → 190716 | 9.4 → 9.5 | 0 → 0 | 8.7 → 3.3 | 49.5 → 49.9 | 7.2 → 7.2 |
| 8 | Nest | ApplySync | 46.7 → 35.8 | 76.6 → 72.0 | 200000 → 199674 | 23.6 → 23.8 | 12.5 → 12.8 | 0 → 0 | 74.5 → 42.5 | 13.8 → 13.9 |
| 8 | Nest | ApplyAsync | 40.9 → 41.4 | 72.1 → 70.2 | 199968 → 199958 | 18.6 → 18.8 | 6.1 → 6.1 | 0 → 0 | 68.1 → 65.8 | 14.6 → 14.8 |
| 8 | direct | ApplySync | 34.6 → 41.4 | 50.6 → 59.6 | 200000 → 200000 | 12.9 → 12.3 | 0 → 0 | 12.6 → 12.0 | 50.4 → 59.4 | 11.4 → 11.2 |
| 8 | direct | ApplyAsync | 34.6 → 35.2 | 51.1 → 53.0 | 200000 → 200000 | 13.0 → 12.8 | 0 → 0 | 12.7 → 5.0 | 51.2 → 53.0 | 11.3 → 11.4 |
| 16 | Nest | ApplySync（诊断构建） | 59.5 → 47.3 | 126.4 → 128.1 | 197000 → 194991 | 39.6 → 41.4 | 29.7 → 38.1 | 0 → 0 | 87.6 → 48.1 | 22.4 → 23.1 |
| 16 | Nest | ApplyAsync | 51.1 → 51.2 | 120.2 → 113.6 | 198500 → 198000 | 31.4 → 31.2 | 18.5 → 16.4 | 0 → 0 | 76.4 → 73.4 | 24.0 → 24.0 |
| 16 | direct | ApplySync（诊断构建） | 36.6 → 43.5 | 63.5 → 79.9 | 200000 → 200000 | 19.6 → 19.8 | 0 → 0 | 20.4 → 20.3 | 62.7 → 79.3 | 18.2 → 18.0 |
| 16 | direct | ApplyAsync | 36.6 → 40.7 | 61.0 → 72.3 | 200000 → 200000 | 19.8 → 20.2 | 0 → 0 | 20.5 → 8.8 | 62.0 → 70.9 | 18.1 → 18.2 |

逐轮数值见 [aoi-o-summary.md](benchmarks/interest-single-writer-20261022/aoi-o-summary.md)，每次运行一行的原始摘要 [aoi-o-runs.jsonl](benchmarks/interest-single-writer-20261022/aoi-o-runs.jsonl)。

读法：

- **Nest ApplySync 变好**：Flush 与政策阶段的耗时不变（AOI 计算量没变），但改值到客户端的整条分布前移（P50 低 6～12ms），新订阅全量 P99 低 19～40ms。两轮方向一致、小块场景同样（§3）。原因**尚未确认**：重构把直接调用、视野操作与排队事实收进同一条日志、在同一次政策阶段按序执行并结算，推测是改前一部分事实（或它们带来的订阅）要多等一次 Flush 才被捕获；没有做逐帧追踪证明。
- **Nest ApplyAsync 持平**：所有差异在两轮的轮间差以内。
- **业务线程不再等 AOI 批次**：Nest 模式下压测线程本来就不调 Interest，这一项看 §5 的基准。

## 3. sync-aoi 小块场景（两轮均值，改前 → 改后）

| 视野 | 驱动 | 模式 | P50 ms | P99 ms | 帧数 | Flush 均值 ms | 政策阶段 P95/tick ms | 压测线程 P50/tick ms | 新订阅全量 P99 ms | 服务端 CPU s |
| ---: | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 4 | Nest | ApplySync | 35.3 → 29.5 | 61.2 → 56.4 | 80210 → 79922 | 10.3 → 10.1 | 5.6 → 6.0 | 0 → 0 | 56.2 → 35.6 | 5.8 → 5.7 |
| 4 | Nest | ApplyAsync | 33.9 → 34.1 | 60.9 → 60.9 | 80330 → 80173 | 8.6 → 8.7 | 1.7 → 1.6 | 0 → 0 | 55.9 → 55.4 | 6.6 → 6.6 |
| 4 | direct | ApplySync | 32.8 → 32.0 | 50.6 → 51.1 | 80240 → 80240 | 8.1 → 6.7 | 0 → 0 | 3.7 → 4.6 | 50.7 → 51.3 | 3.6 → 3.3 |
| 4 | direct | ApplyAsync | 32.7 → 32.9 | 50.6 → 51.1 | 80240 → 80240 | 8.0 → 8.1 | 0 → 0 | 3.8 → 1.3 | 50.6 → 51.2 | 3.6 → 3.8 |
| 8 | Nest | ApplySync | 35.6 → 30.0 | 61.9 → 57.3 | 119553 → 119584 | 11.1 → 11.4 | 6.0 → 6.5 | 0 → 0 | 53.0 → 32.7 | 5.6 → 5.7 |
| 8 | Nest | ApplyAsync | 34.4 → 34.5 | 61.2 → 61.5 | 119636 → 119494 | 10.6 → 10.5 | 2.0 → 1.8 | 0 → 0 | 52.6 → 52.8 | 6.7 → 6.7 |
| 8 | direct | ApplySync | 33.4 → 32.5 | 50.4 → 51.3 | 119597 → 119597 | 9.8 → 7.3 | 0 → 0 | 5.5 → 5.6 | 50.4 → 51.4 | 4.2 → 3.8 |
| 8 | direct | ApplyAsync | 33.7 → 33.9 | 50.3 → 51.0 | 119597 → 119597 | 10.0 → 9.9 | 0 → 0 | 5.9 → 1.9 | 50.4 → 50.7 | 4.4 → 4.5 |

逐轮见 [aoi-n-summary.md](benchmarks/interest-single-writer-20261022/aoi-n-summary.md)。

## 4. 16 视野与 D19 历史数字

D19“两种应用模式”的历史表（2026-10-21，`b6ef5573`，同一夹具口径、原场景，那时 sync-aoi 的 Nest 模式也调 `Apply`，但那时的 `Apply` 不执行排队事实，口径相同）：

| 16 视野 | 历史（b6ef5573，2 样本） | 今天改前（494134ae） | 今天改后（11988df8） |
| --- | --- | --- | --- |
| Nest ApplySync（诊断构建）P99 ms | 150.1 / 144.3 | 122.2 / 130.6 | 132.5 / 123.6 |
| 同上 Flush 均值 ms | 47.3 / 45.7 | 39.4 / 39.9 | 42.2 / 40.6 |
| 同上 政策阶段 P95/tick ms | 38.8 / 39.0 | 31.1 / 28.2 | 38.3 / 38.0 |
| 同上 帧数 / Flush 次数 | 190000 / 195170；203～208 次 | 198000 / 196000；211 / 209 次 | 193000 / 196982；206 / 210 次 |
| Nest ApplyAsync P99 ms | 129.6 / 137.6 | 128.5 / 111.9 | 116.6 / 110.5 |
| 同上 Flush 均值 ms | 35.1 / 35.6 | 31.3 / 31.4 | 31.5 / 30.9 |
| direct P99 ms | 68.5 / 90.0 | 61.0～64.0 | 70.0～82.6（语义变化，不可直接比） |

- 结论不变：**ApplySync 16 视野仍超周期**（Flush 约 40ms、政策阶段 P95 约 38ms 贴着 50ms 周期，帧数与 Flush 次数不满），8 视野的阈值依据仍成立；ApplyAsync 16 视野前后持平。
- 改后 ApplySync 16 视野比改前略差（政策阶段 P95 +8ms、Flush +1.8ms、两轮都少 1～3 次 Flush）。改后的政策阶段“按预算排空”：预算是同步周期一半（25ms），只在两步之间检查（`settleQueued`，`framework/sync/entitysync/policy/interest_queue.go`），检查之后还有一步与结算，所以 16 视野时一次政策阶段可以到 38ms。改前每次政策阶段最多一批。16 视野超出 ApplySync 的上限（9），属于诊断构建，不影响缺省配置；是否给预算加上“结算也计入”交维护者决定——维护者 2026-10-22 决定 A，已实施，压测待补（§7）。
- 今天两边都比历史数低约 10～20ms（Flush 与 P99），历史数是另一时段、另一份代码（`b6ef5573` 之后还有 D19 / D20 的改动），只看趋势。

## 5. policy 基准（benchstat，base / after 交替 10 遍）

原文 [policy-benchstat.txt](benchmarks/interest-single-writer-20261022/policy-benchstat.txt)（原始输出 `policy-base.txt` / `policy-after.txt`）。

| 基准 | 改前 | 改后 | 变化 |
| --- | --- | --- | --- |
| `XDirectCallUnderPolicy` sync / `SetObserverView` 均值 · p99 · 最大 | 128ns · 125ns · 2.85ms | 41ns · 42ns · 17µs | −68% · −66% · −99% |
| 同上 sync / `Enter`+`Leave` | 23.4µs · 55.9µs · 4.15ms | 0.47µs · 1.5µs · 1.77ms | −98% · −97% · −57% |
| 同上 async / `SetObserverView` | 355ns · 166ns · 7.94ms | 42ns · 208ns · 71µs | −88% · +25% · −99% |
| 同上 async / `Enter`+`Leave` | 29.0µs · 76.1µs · 10.3ms | 0.48µs · 1.7µs · 1.25ms | −98% · −98% · −88% |
| `XSyncFlushBacklog`（4096 条积压）第一次 Flush | 13.8ms（只应用一批 1024） | 47.2ms（一次排空） | +242% |
| 同上 第一次 Flush + `Drain` 总耗时 | 160.8ms | 47.2ms | −71% |
| AOI / Cluster（16 项） | — | — | 全部在 ±2% 内 |
| `RelationViewMemberChange` players=1 | 433 / 512ns | 630 / 712ns | +39～45%；players≥64 时 +0.5～4.7% |
| `RelationViewAttach` members=32 | 2.89µs | 3.27µs | +13%；members≥1024 持平 |
| `SpatialViewsPerPlayer` views=1 / 16 / 64 | 2.08 / 35.4 / 107µs | 2.36 / 38.0 / 113µs | +14% / +7% / +6% |
| `SharedSpatialView` players=1 / 64 / 1024 | 226ns / 2.73µs / 45.0µs | 451ns / 3.04µs / 46.1µs | +100% / +11% / +2% |

- 登记侧入口的目标达成：直接调用不再取应用侧锁，均值与 p99 降一到两个数量级，最大值从“一个 AOI 批次”（ms 级）降下来。`Enter`+`Leave` 改后最大仍有 0.5～2.5ms（两种模式 20 个样本全部在 0.5ms 以上），没有定位：不是等应用侧锁（`SetObserverView` 同一负载最大 17～71µs），可能是 GC 或登记侧短锁在日志扩容时的停顿，**尚未确认**。
- ApplySync 积压时第一次 Flush 变长（一次排空，受“周期一半”预算约束；本基准的周期是 Manager 缺省值），但排空总耗时短得多——改前一次 Flush 只应用一批、剩下的要等后续 Flush（本基准里 `Drain` 在批之间等周期）。这就是方案 §6 说的“Flush 变长但不再丢吞吐”。
- 视野登记表一类的小操作变慢：每次操作现在多一条日志条目（`B/op` +38～960%、`allocs/op` +2～3），在“只有 1 个挂接玩家”的场景相对变化大、绝对值是几百纳秒；挂接玩家多时被扇出成本淹没。

## 6. demo 登录风暴与不能证明的

**登录风暴**（方案 §6 第 3 项；样本 [storm/](benchmarks/interest-single-writer-20261022/storm/)）：改前 / 改后各生成一个 game-demo 工程（`roost project new … -template game-demo --skip-deps`，`replace` 指回 `494134ae` / 当前 worktree），依赖用私有隔离环境（`dataengine-env.sh`，端口偏移 26000），十个进程。demo 的 loadtest 没有 `scene_view` 动作，两边的生成工程里加了同一处补丁（[patch-loadtest.py](benchmarks/interest-single-writer-20261022/storm/patch-loadtest.py)，未入库到 demo 模板）和场景 [storm.yaml](benchmarks/interest-single-writer-20261022/storm/storm.yaml)：`connect` → `enter_game` → `scene_watch` → `scene_ready` → `scene_view` → 等 4 秒（让压测端指标能被抓到）。每次运行重启十个进程（指标从零开始）、80 个机器人同时开始、每次换一批新玩家 ID，3 轮、先后交替；loadavg 1.6～2.2。

| 指标 | 改前（r1 / r2 / r3） | 改后（r1 / r2 / r3） |
| --- | --- | --- |
| 机器人成功 | 80 / 80 / 80 | 80 / 80 / 80 |
| `handlerEnterScene`（进场景，Nest）均值 ms | 8.28 / 0.39 / 0.02 | 0.20 / 0.10 / 0.32 |
| 同上 最大 ms | 18.88 / 12.10 / 0.32 | 8.78 / 6.09 / 11.92 |
| `handlerRecordEnter` 均值 / 最大 ms | 8.79 / 18.86 · 7.72 / 14.87 · 8.52 / 20.09 | 8.54 / 18.75 · 8.24 / 18.49 · 8.09 / 13.26 |
| 玩家 TCP 分发 均值 / 最大 ms（全部消息） | 106.5 / 658 · 89.4 / 561 · 97.9 / 630 | 100.3 / 634 · 98.4 / 615 · 92.0 / 593 |
| 压测端 `scene_view` 往返 p99（2 的幂桶上界）ms | 8 / 2 / 1 | 1 / 1 / 1 |
| 压测端 `enter_game` 往返 p50 / p99（桶上界）ms | 512 / 1024（三次相同） | 512 / 1024（三次相同） |

读法：这个规模下 AOI 批次很小，改前只有第 1 次出现进场景 handler 均值 8ms（与改前“直接调用可能等一个 AOI 批次”一致），另两次与改后同量级，**不能据此下结论**。登录往返主要花在账号登录与建角色链路上（TCP 分发均值约 100ms，两边相同）。game 进程的 `nest_dispatch_cost` 只有计数 / 总和 / 最大，没有直方图，拿不到服务端 P99；压测端直方图是 2 的幂桶，只能看量级。

**规模上限是 demo 的配置**：100 个机器人时 100 个复制会话全部因 `frame: object limit exceeded` 被关闭，200 个时 128 个被关闭、另有 72 个在 `connect` 失败（2026-10-22 后补：当次 game 指标 `player_tcp_connection_rejected_total{reason="per_ip"} 72`，是单 IP 连接上限 128，见 KNOWN-LIMITS）——所有玩家挤在同一出生点，加上附近的怪超过 Manager `Limits.MaxObjects`（缺省 100），demo 的空间视野又没有设 `MaxVisible`。两边都会这样（Manager 未改）。已记入 [KNOWN-LIMITS“game-demo 的移动、怪物与场景时间”](../maintenance/KNOWN-LIMITS.md#game-demo-的移动怪物与场景时间d20-第-3-期)，是 demo 配置的产品选择；维护者 2026-10-22 按推荐给主视野设了 `MaxVisible` 64。

**不能证明的**：

- 队列峰值：sync-aoi 不导出 Interest 的日志长度 / 队列峰值，没有数；game 进程的 `entitysync_interest_fact_backlog` 是瞬时值，风暴期间没有连续采样。
- 只在 macOS / arm64、单机、每个配置 2 轮（风暴 3 轮）；P99 的轮间差 1～9ms（16 视野最大约 17ms）。
- ApplySync 的 P50 / 新订阅全量改善原因尚未确认（§2）。

## 7. ApplySync 预算计入结算（待补测：今晚）

§4 发现 16 视野 ApplySync 的政策阶段 P95 38ms：预算只在两步之间检查，检查后还有一整步与结算。维护者 2026-10-22 决定 A（预算把结算也算进去，按单位估计下一步，预计超出就停），实现与规则见 [REFACTOR §12](../maintenance/REFACTOR-2026-10-21-interest-single-writer.md#12-applysync-预算把结算也算进去2026-10-22维护者决定-a)，代码提交 `be49684b`（改前 `bb7a734f`）。维护者同日要求先处理代码、压测放到今晚，**本节的数还没有测**。

**预期**（不符合时如实记录，不改结论）：16 视野 ApplySync 政策阶段 P95 与 Flush 均值落到预算（25ms）附近、帧数与 Flush 次数接近满额；`entitysync.interest.log_backlog` 与 `budget_stop` 不为 0（AOI 可见性滞后一两个周期，改值→客户端 P99 可能不降反升）；0 / 8 视野不退化（预算内本来就做得完，`budget_stop` 应为 0 或零星）。

**要记录**：每次运行开始时的 `sysctl -n vm.loadavg`、改值→客户端 P50 / P99、帧数、Flush 均值与次数、政策阶段每 tick P95、`entitysync.interest.log_backlog`（运行末的值；sync-aoi 若不导出，在报告里注明并从 `metrics.Snapshot()` 补）、`entitysync.interest.budget_stop.total`、`entitysync.interest.policy_duration` / `settle_duration` 的 P95。

**复跑命令**（同机同时段、改前 / 改后交替各 2 轮；门控同 §1：每次运行前等 10 秒没有外来 go 进程，运行中采到就作废重跑——宿主上同时有 demo loadtest 与选项 C 参数压测，会互相等待）：

```sh
# 两份检出：改前 bb7a734f、改后 be49684b（各自 git worktree add --detach）
for w in base after; do
  (cd "$WT_$w" && GOWORK=off go build -o "$BIN/aoi-$w" ./scripts/perf/sync-aoi)
  # 16 视野诊断构建：只把 SyncApplyMaxExtraViews 改成 16，不入库
  (cd "$WT_$w" && sed -i '' 's/SyncApplyMaxExtraViews     = 8/SyncApplyMaxExtraViews     = 16/' framework/sync/entitysync/policy/interest.go \
     && GOWORK=off go build -o "$BIN/aoi-$w-d16" ./scripts/perf/sync-aoi && git checkout framework/sync/entitysync/policy/interest.go)
done
ARGS="-players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50 -apply-mode=sync -nest=true"
export GOMAXPROCS=4 GOWORK=off
for r in 1 2; do
  order="base after"; [ $r -eq 2 ] && order="after base"
  for v in 16 0 8; do for w in $order; do
    bin="$BIN/aoi-$w"; [ $v = 16 ] && bin="$BIN/aoi-$w-d16"
    echo "$w v$v r$r loadavg=$(sysctl -n vm.loadavg)" >> "$OUT/run.log"
    "$bin" $ARGS -views-per-player=$v -output="$OUT/runs/o-v$v-sync-nest-$w-r$r" > "$OUT/runs/o-v$v-$w-r$r.log" 2>&1
  done; done
done
```

汇总沿用 [agg-aoi.py](benchmarks/interest-single-writer-20261022/agg-aoi.py)（同一口径），结果写进本节，样本放 `benchmarks/interest-applysync-budget-20261022/`。
