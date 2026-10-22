# D20 第 4 期：压测工具改为心跳驱动（2026-10-21，只交付代码，测量待补）

**2026-10-22 晚已做正式测量，结果见 [§4A](#4a-2026-10-22-晚正式测量v1310-发版前压测第-6-项)。** 下面一段是交付时的原话。

**本页没有性能结论。** 维护者 2026-10-21 要求先暂停压测、把代码收尾；同日宿主上有别的耗资源程序，冒烟也不跑。sync-aoi 与 nest-game 的心跳驱动模式、§7 的微基准都已实现、能编译，`go test` 里的短断言（几百毫秒、几十个实体）通过；正式对比放到 v1.31 发版前的压测，复跑命令见 §4。

设计来源：[DESIGN-TODO D20](../maintenance/DESIGN-TODO.md#d20-移动由实体自己的心跳驱动mapbasic--move-组件--actionflow-控制) P4 行与 P5 表里 sync-aoi、nest-game 两行；结构与投递方式的前置压测见 [HB-STRUCTURES-20261021](HB-STRUCTURES-20261021.md)。

## 1. 新增开关

### sync-aoi（`scripts/perf/sync-aoi`，提交 `3c5e41e3`）

| 开关 | 缺省 | 含义 |
| --- | --- | --- |
| `-move-driver=loop\|heartbeat` | `loop` | Nest 模式下谁驱动移动。`loop` 是原负载：压测端循环每 50ms tick 挑 `-dirty`% 实体、分 `-batches` 批发 `Request`，handler 里 `QueueMove`。`heartbeat`：每个实体装 `gameplay/heartbeat` 宿主、`gameplay/movement` 的 `MapBasic` / `Move` 与 `actionflow.BeatRunner`，被选中的移动者在原往返带上 `MoveTo` 随机目标巡逻，到达后在 `OnEnded` 里接下一段，runner 的移动组心跳每拍 `Move.Advance(Beat.Elapsed)` → `MapBasic.SetPos` → `QueueMove`。只支持 `-nest`（直接模式只有 loop） |
| `-move-period` | `200ms`（2026-10-22 起，跟 `movement.DefaultMovePeriod`；之前是 `100ms`） | 心跳模式移动组心跳的档位（`heartbeat.Periods` 之一）。**与 2026-10-22 之前的心跳模式结果对照时显式加 `-move-period=100ms`**，见 §4 |
| `-move-speed` | `30` | 巡逻速度（格/秒）。100ms 档每拍 3 格（与全局循环每次改值走的 3 格相同），200ms 档每拍 6 格；都保证每拍换格、每拍一次改值 |
| `-movers` | `0` | 巡逻的实体数；0 按与全局循环相同的改值速率折算：`ceil(每 tick 改值数 × 20/s × move-period)`。缺省参数（1 万实体、1%）在 100ms 档是 200 个、200ms 档是 400 个，都是每秒 2000 次改值 |

几何不变：基点与全局循环相同（`patrolBase`，原 `position` 的偏移 0），X 在 `[基点, 基点 + -move-amplitude]`，Y 不变；移动者用与全局循环相同的互素步长散在整张地图上。心跳 handler 用 memory 级别（与全局循环的 memory handler 一致）。

输出的区分：`config.json` 与 `server.json` 有 `move_driver`；标准输出第一列 `move_driver=`；心跳模式 `server.json` 另有 `heartbeat` 一节：

| 字段 | 含义 |
| --- | --- |
| `movers` / `period_ms` / `speed_cells_per_second` | 实际的移动者数、档位、速度 |
| `driver` | `heartbeat.Driver.Stats()`：投递、执行、跳拍（inflight / queue_full / rejected）、卡住、丢弃 |
| `beat_interval_ms` / `beat_interval_samples` / `beat_interval_over_period_plus_50ms` | 同一实体相邻两次写位置的业务时间差（只计测量 tick），是驱动与快池迟到的观察点 |
| `legs_completed` / `action_failures` / `first_action_error` | 巡逻段数、移动动作失败 |

口径差异（读结果时注意）：

- 心跳模式没有“计划时刻”：`Planned` 取这一拍 Nest 消息冻结的业务时间（≈ handler 开始），所以 `planned_event_to_client` 与 `state_change_to_client` 几乎相同；驱动与队列的迟到看 `beat_interval_ms`。`harness.admit_lag_ms` / `client_queue_ms` / `server.request_to_change_ms` 只在 loop 模式有值。
- `Step` 是改值时压测端的 tick（与 loop 的 `Step = tick` 同口径，客户端据此划测量窗口、判断全量是否本 tick 的改值）；同一实体在一个 tick 里改两次时（`-move-period=50ms` 偶有）第二次不计时，但照常核对。
- 客户端对 loop 逐项核对位置（位置由 `Step` 唯一确定）；心跳模式位置取决于心跳时机，只核对在本实体的巡逻带上，其余（版本链、HP 与 `Step`、负载字节、最终可见集合）两种模式相同。
- 心跳模式的额外门槛：没有跳拍、卡住、准入失败，没有移动动作失败，测量窗口里确实有心跳改值；不过时与 50ms 延迟门槛一样保留报告、以非零退出。
- 移动者在预热前开始巡逻（预热 10 个 tick 按 50ms 等待），预热与测量之间的屏障阶段也在移动（Manager 未启动，事实在 Interest 队列里，满了按背压整拍作废、下一拍再走）。

**缺省为什么还是 loop**：已有报告（D19 多视野、ApplySync / ApplyAsync、夜间补测）的复跑命令不加开关时必须得到原负载；心跳模式的数字还没有正式对比。v1.31 发版前的压测做完同机对照后，由维护者决定是否改缺省（见 DESIGN-TODO D20“P4 实施结果”的待定项）。

### nest-game（`codegen/internal/entity/testdata/nestgame`，提交 `22b01614`）

| 环境变量 | 缺省 | 含义 |
| --- | --- | --- |
| `ROOST_NEST_GAME_HEARTBEAT_DRIVER=ticker\|heartbeat` | `ticker` | `ticker` 是原负载：唯一的 Nest 帧回调按 `HZ × 实体数` 的速率逐个 `Dispatch` 一条心跳业务消息。`heartbeat`：每个生成的 `Unit` 在 `gameplay/heartbeat` 上一个周期 `1s / ROOST_NEST_GAME_HZ` 的登记（必须是档位：1、2、5、10、20Hz），回调在 Unit 自己的 worker、实体锁与事务内执行同一个 `HeartbeatComponent.Tick`，计数与 Sync 接线与 ticker 模式的 handler 相同；帧回调只投普通业务 |

`scripts/perf/nest-game.sh --check-heartbeat` 在生成工程里跑接线短测 `TestHeartbeatDriverTicksGeneratedUnits`（64 个 Unit、20Hz、0.5 秒）。

heartbeat 驱动下结果 JSON 增 `Beats`（`PeriodMS`、`Driver`、`Interval`、`BeatsMin/Max/Sum`、`ExpectedPerUnit`、`Verified`、`Problem`），标准输出多一行 `heartbeat_driver=heartbeat period=… beats/unit=…`。口径差异：

- `Heartbeats.Completion` / `Server.Completion` 从回调开始算（服务端：handler + 释放），没有计划时刻；驱动与快池迟到看 `Beats.Interval`（相邻两拍的 `Beat.Elapsed`；其中 `Over50MS` 是“间隔超过 50ms”的计数，对 ≥50ms 的档位没有意义）。`Harness.*`、`QueueWait` 对心跳流为空。
- 计数核对改为：投递 = 执行，没有跳拍、卡住、准入失败；每个 Unit 的拍数在 `运行时长 / 档位 ± 2`；每个 Unit 的 DAO（`Heartbeats`、`HP`）等于按它的拍数与 ticker 同一条规则（`(序号 + 第几拍) % 100 < DIRTY`）算出的改值次数；间隔 P99 不超过档位 + 50ms。
- `Heartbeats.Planned` / `Accepted` 取驱动的投递数，`Rejected` 取跳拍数。

## 2. §7 微基准（`gameplay/heartbeat/internal/bench`，提交 `cf2bd7d2`）

D20 §7 列的六项与本包的对应（前两组是 HB-STRUCTURES 时的原型，第 4 期用正式 `heartbeat.Driver` 补后三项）：

| §7 项 | 入口 | 状态 |
| --- | --- | --- |
| `BenchmarkWheelAdvance`（只测出入环） | `BenchmarkSched op=step / add / remove / settier`（原型 `TierRing` 与正式环同一写法） | 已有，结果见 HB-STRUCTURES §3 |
| 时间轮与最小堆对照 | `BenchmarkSched impl=wheel10 / heap10 / tier10` | 已有，HB-STRUCTURES §3 |
| `Dispatch` 与 `DispatchBroadcast` 对照 | `TestDeliveryRun HB_MODE=x / z` | 已有，HB-STRUCTURES §4 |
| `BenchmarkDriverDispatch`（假客户端） | `driver_test.go` `BenchmarkDriverDispatch`：正式驱动 + 假客户端（handler 在驱动 goroutine 上直接执行，回调为空），报 `cpu-ns/beat`、`driver-cores`、`beats/s`；ns/op 只是 1 / 目标速率 | 新增，待测 |
| 真实 Nest + 空 handler 开环投递（10 / 25 / 50 万拍每秒） | `TestDriverOpenLoop`（`HB_OPENLOOP=1`，一个进程一档，结果追加到 `HB_OUT`）：完成数、跳拍、卡住、快池等待峰值、最大 worker 等待、间隔 P50 / P99、进程核数。不设环境变量时跑 2000 拍/s × 0.3 秒的小档断言 | 新增，待测 |
| 快池被业务占满时的跳拍 | `TestBusyShortPoolSkipsBeatsAndResumes`（常跑，约 0.3 秒）：1 个 worker、队列 8 格、一条业务消息占住 worker，20 个 50ms 登记；各阶段按事件推进（每个登记先跳一拍 → 占位消息真正开始 → 驱动累计跳拍达到每个登记两拍 → 放开 → 每个登记再跳两拍），不按墙钟窗口划分（2026-10-21 起，原按“占住 250ms、放开后 100ms 窗口里至少两拍”计，宿主负载下偶发失败）。断言：占用期间不执行、有在途 / 队列满跳拍、没有卡住与其他准入失败、投递 = 执行、放开后每个登记都恢复、恢复后的第一拍报告跳过数且 `Elapsed` 覆盖这段时间（不超过 1 秒上限） | 新增，契约检查（不是性能数字） |

HB-STRUCTURES §7 记的“跳拍路径没有被压出来”由最后一项在功能上覆盖（在途与 `ErrQueueFull` 两种跳拍都出现：一次运行 `SkippedInflight:34 SkippedQueueFull:60`，`Stalled:0`，投递 126 = 执行 126）；规模与延迟的影响仍待开环投递测。

## 3. 已做的验证（不是性能测量）

| 项 | 结果 |
| --- | --- |
| sync-aoi `TestHeartbeatPatrolReachesClientsAndTheAOI`（真实 Nest + 心跳驱动 + Manager + Interest，16 实体 / 2 玩家 / 8 个移动者、50ms 档，约 0.6 秒） | 通过；`-race -count=3` 通过。模拟客户端按心跳模式逐帧核对无误，最终位置与 AOI 一致（进入 = 离开半径、上限足够时，每个玩家看到的正是半径内的实体） |
| 变异核对（sync-aoi） | `loadSpace.QueueMove` 不入队 → `player … sees [2808 5880], want … [2808 5880 6904]`；写位置不标同步脏 → `client latency samples 2, of which deltas 0`；客户端心跳模式改回按 Step 逐项核对 → `TestClientVerifiesThePatrolBandPerMoveDriver` 与端到端用例变红 |
| nest-game `TestHeartbeatDriverTicksGeneratedUnits`（生成工程，64 Unit、20Hz、0.5 秒） | 通过；`-race -count=2` 通过；每个 Unit 9～10 拍（期望 10），627 拍全部在 Nest 事务内 |
| 变异核对（nest-game） | 回调不调 `Tick` → `unit 0: 10 beats should change 10 times, DAO heartbeats 0 hp 0`；登记不启动 → `delivered 0 executed 0` |
| `TestDriverOpenLoop` 小档 | 2000 拍/s × 0.3 秒：投递 615 = 执行 615，跳拍 0 |
| `BenchmarkDriverDispatch` | 只确认能跑（`regs=50000/period=1s`，`-benchtime 20000x`），读数不作结论 |
| `TestNestGameLoad` 的 heartbeat 驱动路径 | **没有运行**（最短 1 秒负载，按 2026-10-21 要求不跑），只编译与 vet 通过 |

## 4. 待补测（2026-10-21 因宿主资源占用推迟）

原计划在交付前跑的几秒到几十秒冒烟、以及 D20 P4 要求的正式对比，全部推迟到 v1.31 发版前的压测，按同机交替、多轮、benchstat 的口径跑。命令（标签自取、唯一；不与 race、profile、索引重建并跑）：

**口径（2026-10-22 起）**：sync-aoi 的 `-move-period` 缺省由 `100ms` 改为 `200ms`（跟 `movement.DefaultMovePeriod`，维护者 2026-10-22：MMO / SLG 的移动一般 2～5Hz）。下面的心跳驱动命令显式写 `-move-period=100ms`，与本页 §4 冒烟及之前的心跳模式结果同一口径（移动者数、每拍位移、每秒消息数都相同）；想看新缺省就另跑一组 `-move-period=200ms`（移动者数按改值速率折算翻倍、每秒改值数不变，每秒心跳消息数减半——每个移动者每秒 5 拍）。全局循环（`-move-driver=loop`）不受这个开关影响。

```sh
# 1. sync-aoi：全局循环与心跳驱动对照（原场景；各 3 轮，交替跑）
ROOST_PERF_LABEL=p4-aoi-loop      bash scripts/perf/sync-aoi.sh -players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50 -move-driver=loop
ROOST_PERF_LABEL=p4-aoi-heartbeat bash scripts/perf/sync-aoi.sh -players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50 -move-driver=heartbeat -move-period=100ms

# 2. D19 多视野 Nest 模式复测（每玩家 4 / 8 / 16 个额外视野，两种 apply 模式，两种驱动）
for views in 4 8 16; do for apply in sync async; do for driver in loop heartbeat; do
  ROOST_PERF_LABEL=p4-aoi-v$views-$apply-$driver ROOST_PERF_COUNT=2 \
    bash scripts/perf/sync-aoi.sh -players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50 \
      -views-per-player=$views -apply-mode=$apply -move-driver=$driver -move-period=100ms
done; done; done
# ApplySync 的 16 视野超过 SyncApplyMaxExtraViews（8）时 NewInterest 拒绝，按 D19 记录用诊断构建或只跑 async。

# 3. nest-game：帧回调与心跳驱动对照（V1290 重档参数，HZ=10 是档位；各 15 分钟）
for driver in ticker heartbeat; do
  ROOST_PERF_LABEL=p4-game-$driver ROOST_PERF_COUNT=1 ROOST_PERF_CPU=0 \
  ROOST_NEST_GAME_HEARTBEAT_DRIVER=$driver \
  ROOST_NEST_GAME_DIRTY=5 ROOST_NEST_GAME_DURATION=15m ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_EMBEDDED=1 ROOST_NEST_GAME_HZ=10 \
  ROOST_NEST_GAME_IO_WORKERS=8 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_PROFILE=0 \
  ROOST_NEST_GAME_RACE=0 ROOST_NEST_GAME_SHORT_QUEUE=65536 ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_SYNC=1 \
    bash scripts/perf/nest-game.sh
done

# 4. 驱动微基准与开环投递
GOWORK=off go test -run '^$' -bench BenchmarkDriverDispatch -benchtime 2000000x -count 10 ./gameplay/heartbeat/internal/bench > driver.txt
for rate in 100000 250000 500000; do
  HB_OPENLOOP=1 HB_RATE=$rate HB_PERIOD=100ms HB_WORKERS=8 HB_QUEUE=65536 HB_SECONDS=20 HB_OUT=openloop.jsonl \
    GOWORK=off go test -run '^TestDriverOpenLoop$' -count=1 -timeout 0 ./gameplay/heartbeat/internal/bench
done
```

冒烟（缩小规模，只证明能跑通、指标有数；心跳驱动同样加 `-move-period=100ms` 才与下表同口径）可用 `-players=100 -entities=1000 -ticks=100` 与 `ROOST_NEST_GAME_ENTITIES=1000 ROOST_NEST_GAME_PLAYERS=100 ROOST_NEST_GAME_DURATION=10s`，结论栏写“正式对比待 v1.31 发版前压测”。

**冒烟结果（2026-10-22，main `11988df8` + 夹具修正，宿主 1 分钟 loadavg 约 2～3，只证明能跑通，正式对比仍待 v1.31 发版前压测）**：

| 命令（缩小规模） | 结果 |
| --- | --- |
| sync-aoi `-players=100 -entities=1000 -ticks=100 -move-driver=loop` | 跑通，帧 4177、改值→客户端 P50 / P99 26.2 / 49.0ms；进程以非零码退出是夹具自己的 50ms 延迟门槛（33 / 4570 个样本超 50ms），报告完整 |
| 同上 `-move-driver=heartbeat`（当时缺省 `-move-period=100ms`） | 跑通，门槛内（0 / 4768 超 50ms），帧 4033、P50 / P99 11.8 / 42.0ms；移动者 20、`Delivered = Executed = 1109`、跳拍 / 停滞 / 动作失败 0、巡逻段 181；`beat_interval_ms` P50 100.0 / P99 200.1（946 个间隔里 29 个超过周期 + 50ms：间隔按“位置真的变了”的两次 `SetPos` 计，换段那一拍不动，所以出现 200ms，属口径，不是跳拍） |
| 同上 + `-views-per-player=4 -apply-mode=sync` / `async` 心跳驱动 | 都跑通；sync 门槛内（P99 45.0ms），async 超门槛（P99 52.5ms，2220 / 19743 超 50ms，与 ApplyAsync 多约一个周期一致） |
| nest-game `ROOST_NEST_GAME_HEARTBEAT_DRIVER=ticker`，`ENTITIES=1000 PLAYERS=100 DURATION=10s`，其余按本节第 3 条（EMBEDDED、SYNC、HZ=10） | `TestNestGameLoad` PASS：消息 999.4/s、心跳 9993.9/s，消息 / 心跳 p99 11.96 / 11.40ms，计数、终值、延迟检查都为 true |
| 同上 `heartbeat`（`TestNestGameLoad` 的心跳驱动路径） | PASS：每 Unit 99～100 拍（期望 100）、间隔 p99 100.0ms、跳拍 0 / 0 / 0、停滞 0、`verified=true`；消息 999.5/s、心跳 9984.6/s，p99 11.50 / 0.040ms（心跳从回调开始计，口径见 §1） |
| `scripts/perf/nest-game.sh --check-heartbeat` | PASS（`TestHeartbeatDriverTicksGeneratedUnits`：632 拍全部在事务里执行，跳拍 0） |

sync-aoi 的 Nest 模式之前每 tick 调一次 `interest.Apply()`，Interest 单写者重构之后它会在压测线程执行排队移动，2026-10-22 已改为只在 direct 模式调用（`6cffbc03`，见 [INTEREST-SINGLE-WRITER-20261022](INTEREST-SINGLE-WRITER-20261022.md)）；上表在改后的夹具上跑。原始产物在当次 worktree 的 `artifacts/perf/`（未入库）。

## 4A. 2026-10-22 晚正式测量（v1.31.0 发版前压测第 6 项）

**被测**：发版候选 main `3d727c2f`；sync-aoi 用同一个预编二进制（附带只读的 `interest_metrics` 导出，见 [INTEREST-SINGLE-WRITER §7.1](INTEREST-SINGLE-WRITER-20261022.md#71-结果2026-10-22-晚v1310-发版前压测第-2-项)），16 视野 ApplySync 用诊断构建（`SyncApplyMaxExtraViews` 16，不入库）；GOMAXPROCS 4 / 进程，门控同 run-list（每次前等 10 秒无外来进程，运行中采到就作废重跑，作废 2 次：一次 Claude 进程 CPU>20%）。nest-game 两档背靠背、各 15 分钟，`nest-game.sh` 自己编译（编译时不测）。微基准与开环投递用 §1 第 1 项编好的 `gameplay/heartbeat/internal/bench` 测试二进制（缺省组 4 / 200µs）。样本：[benchmarks/v1310-prerelease-20261022/hb-load](benchmarks/v1310-prerelease-20261022/hb-load/)（`sync-aoi-summary.md`、`sync-aoi-runs.jsonl`、运行记录含 loadavg；nest-game 两份 JSON 与 env；`driver.txt` / `driver-benchstat.txt`；`openloop.jsonl`；`host-load.txt` 每 15 秒的 loadavg 与 CPU 前 6）。

### 1. sync-aoi 原场景：全局循环 vs 心跳驱动（各 3 轮轮转，21:00～21:04）

| 驱动 | loadavg（3 轮） | 改值→客户端 P50 / P99 ms | 新订阅全量 P99 ms | 帧数 | Flush 均值 ms | 服务端 CPU s | 客户端 CPU s |
| --- | --- | --- | --- | --- | --- | --- | --- |
| loop | 3.97 / 2.84 / 2.65 | 30.1 / 29.9 / 30.2；56.6 / 56.2 / 56.9 | 42.2 / 45.0 / 44.8 | 90558 ×3 | 10.4 / 10.2 / 10.7 | 4.24 / 4.23 / 4.31 | 2.49 / 2.52 / 2.48 |
| heartbeat 100ms（历史口径，200 移动者） | 3.65 / 3.54 / 2.59 | 31.5 / 26.7 / 27.7；55.9 / 51.4 / 58.9 | 51.5 / 16.2 / 46.4 | 91224 / 91076 / 91422 | 10.3 / 10.0 / 10.1 | 3.80 / 3.73 / 3.74 | 2.14 / 2.15 / 2.16 |
| heartbeat 200ms（新缺省，400 移动者，清单 §7） | 3.19 / 3.33 / 3.15 | 27.5 / 28.2 / 27.2；53.4 / 52.2 / 53.1 | 17.4 / 47.6 / 26.7 | 85380 / 84631 / 85372 | 10.4 / 9.9 / 10.1 | 3.88 / 3.67 / 3.78 | 2.07 / 2.07 / 2.08 |

心跳模式 9 次：`Delivered = Executed`（约 2.13 万拍 / 次），跳拍 / 卡住 / 动作失败全 0；`beat_interval_ms` P50 100.0（200ms 档 200.0），P99 是 2 倍周期（换段那一拍不写位置，口径见 §1）。三种驱动的改值→客户端 P99 都在 51～59ms（periodic 下 50ms 门槛失败是结构性的），心跳模式的服务端 CPU 少约 11%（4.26 → 3.76 核秒 / 次）、客户端少约 14%，200ms 档每秒改值数与 100ms 档相同（移动者翻倍），帧数少约 6%（同一移动者连续换格，与全局循环每 tick 换一批实体的空间分布不同）。

### 2. D19 多视野：4 / 8 / 16 视野 × 两种应用模式 × 两种驱动（各 2 轮，21:04～21:15）

| 视野 | 模式 | 驱动 | loadavg | 改值→客户端 P99 ms | 新订阅全量 P99 ms | 帧数 | Flush 均值 ms | 政策 P95 / max 每 tick ms | `budget_stop` | 服务端 CPU s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 4 | sync | loop | 2.76 / 4.01 | 61.9 / 58.0 | 39.6 / 36.9 | 189897 / 190037 | 17.5 / 16.2 | 9.8 / 8.7；11.9 / 22.9 | 0 / 0 | 9.1 / 8.8 |
| 4 | sync | heartbeat | 3.02 / 4.95 | 54.9 / 56.2 | 39.7 / 54.3 | 188993 / 190526 | 14.4 / 14.4 | 6.6 / 6.6；28.3 / 20.1 | 1 / 0 | 7.4 / 7.4 |
| 4 | async | loop | 2.84 / 3.85 | 67.0 / 68.8 | 62.3 / 62.6 | 190047 / 189930 | 14.7 / 14.4 | 5.2 / 5.0；10.5 / 17.5 | 0 / 0 | 10.3 / 10.0 |
| 4 | async | heartbeat | 3.00 / 4.34 | 60.4 / 61.0 | 52.5 / 60.9 | 188326 / 190829 | 11.4 / 10.9 | 1.9 / 3.3；5.4 / 4.0 | 0 / 0 | 8.7 / 8.3 |
| 8 | sync | loop | 3.19 / 3.84 | 75.4 / 67.3 | 45.3 / 40.4 | 199848 / 199928 | 25.3 / 24.2 | 13.6 / 13.4；29.5 / 29.8 | 6 / 5 | 14.3 / 14.1 |
| 8 | sync | heartbeat | 3.85 / 3.78 | 58.3 / 56.1 | 35.3 / 36.7 | 198556 / 198527 | 19.0 / 17.5 | 9.3 / 8.3；24.6 / 11.7 | 1 / 0 | 11.1 / 8.8 |
| 8 | async | loop | 4.84 / 4.14 | 71.2 / 70.5 | 67.4 / 62.2 | 199970 / 199934 | 19.6 / 19.3 | 6.5 / 6.3；25.5 / 21.2 | 0 / 0 | 15.4 / 15.0 |
| 8 | async | heartbeat | 4.81 / 3.24 | 63.1 / 63.7 | 67.7 / 57.2 | 198291 / 198536 | 14.1 / 13.9 | 2.6 / 3.1；15.6 / 22.8 | 0 / 0 | 12.6 / 10.6 |
| 16 | sync（诊断构建） | loop | 3.87 / 4.91 | 137.2 / 129.7 | 47.1 / 47.0 | 198984 / 198999 | 42.7 / 41.9 | 26.3 / 27.3；70.6 / 45.3 | 37 / 37 | 23.4 / 23.2 |
| 16 | sync（诊断构建） | heartbeat | 3.81 / 4.13 | 84.4 / 80.3 | 35.8 / 38.7 | 199844 / 199998 | 27.5 / 26.2 | 16.3 / 13.9；37.0 / 61.4 | 7 / 7 | 17.4 / 16.9 |
| 16 | async | loop | 4.69 / 5.10 | 117.8 / 115.5 | 77.4 / 76.3 | 197996 / 197998 | 33.0 / 31.9 | 29.3 / 22.5；53.5 / 51.1 | 0 / 0 | 24.8 / 24.6 |
| 16 | async | heartbeat | 4.39 / 5.71 | 69.7 / 69.8 | 61.1 / 63.3 | 199960 / 199987 | 18.3 / 17.9 | 4.6 / 5.5；39.0 / 35.0 | 0 / 0 | 18.7 / 17.8 |

- 心跳驱动下 **D19 多视野没有失守**：16 视野两种应用模式帧数与 Flush 次数都接近满额（≥ 19.9 万帧，213 次），ApplyAsync 改值→客户端 P99 70ms、ApplySync（诊断构建）80～84ms；同负载的全局循环 116～137ms。4 / 8 视野心跳驱动的 P99 比全局循环低 4～17ms，服务端 CPU 少 15～25%。
- 差距的来源（推断，没有单变量对照）：心跳模式的移动者是固定的 200 个、持续巡逻，全局循环每 tick 换一批（1%）实体，后者每 tick 进出视野的观察者更多、AOI 重评更多；两种驱动每秒改值数相同，但空间分布不同（§5 第 2 条），所以这组数说明“驱动方式不同、负载形状也不同”，不能把差距全部归给投递方式。
- 判据（清单 §5）：ApplySync 4 / 8 视野帧数满（19.0 / 20.0 万）、Flush 均值 14～25ms，**8 视野全局循环 24～25ms 贴着半个周期**；ApplyAsync 16 视野不失守（帧数 19.8 万、Flush 32～33ms，2026-10-20 夜间 P2 是 7.6 万帧、Flush 86～90ms）。

### 3. nest-game：帧回调（ticker）vs 心跳驱动（heartbeat），V1290 重档参数、嵌入 TCP + 场景 Sync，各 15 分钟

21:28～21:43（ticker）、21:43～21:58（heartbeat）背靠背，开始时 loadavg 3.77 / 5.83；运行中 5.8～8.5（含被测进程自己约 2～2.8 核；`host-load.txt`）。ticker 这一次同时就是清单 §4 第 1 项（嵌入 TCP + 场景 Sync 重档，命令只差标签与显式的 `HEARTBEAT_DRIVER=ticker`，即缺省）。

| 指标 | ticker（帧回调） | heartbeat（心跳驱动，缺省批量投递） | 参照：2026-10-20 夜间第 1 项（ticker） |
| --- | --- | --- | --- |
| 业务 / HB 完成 | 9999.9 / 99999.0 每秒，拒绝 0 | 9999.9 / 99998.3 每秒，拒绝 0 | 9999.9 / 99999.0 |
| 计数 / 终值 / 门槛 | 通过 / 通过 / 通过 | 通过 / 通过 / 通过；每 Unit 8999～9000 拍（期望 9000）、跳拍 0 / 0 / 0、卡住 0、`verified=true`，`Delivered = Executed = 89999458` | 通过 |
| **服务端** 业务 P50 / P95 / P99 / max ms，>50ms | 0.08 / 0.39 / 0.87 / 192.5，191 | 0.51 / 1.58 / 2.13 / 14.3，0 | 0.15 / 0.91 / 1.57 / 24.77，0 |
| **服务端** HB P50 / P95 / P99 / max ms（心跳模式从回调开始计，口径不同） | 0.03 / 0.18 / 0.47 / 164.3，1403 | 0.01 / 0.01 / 0.02 / 7.9，0 | 0.08 / 0.61 / 1.13 / 26.38，0 |
| 压测端 AdmitLag 业务 P50 / P99 / max ms | 6.46 / 10.97 / 180.8 | 5.05 / 10.34 / 16.4 | 6.33 / 10.53 / 19.51 |
| 嵌入往返 P50 / P99 / max ms，>50ms | 0.15 / 1.23 / 195.7，360 | 0.73 / 2.60 / 21.7，0 | 0.24 / 1.98 / 27.04，0 |
| Sync 提交→客户端 P50 / P95 / P99 / max ms，>50ms | 1.79 / 4.85 / 9.42 / 185.9，1714 / 4172 万 | 1.76 / 2.83 / 6.07 / 22.1，0 / 4290 万 | 2.70 / 5.10 / 10.14 / 28.03，0 |
| Sync 帧数 | 906 万 | 1417 万 | — |
| 出站计数增量（预算耗尽 / 溢出 / 推送关连接 / 写错误 / dispatch 错误 / 超时）、`push_refused`、滞留 | 全 0 | 全 0 | 全 0 |
| 写协程 / 读循环 | 1000 / 1000 → 1000 / 1000 | 同左 | 同左 |
| short 等待峰值 / 最大 worker 等待 | 2614 / 143.0ms | 139 / 7.5ms | 1989 / 16.4ms |
| 堆峰值 / goroutine | 203.6MB / 4042～4052 | 214.4MB / 4043～4052 | 208.0MB / 4042～4052 |
| 累计分配 / GC / 暂停 | 347.1GB / 3670 / 653ms | 277.3GB / 2810 / 650ms | 375.5GB / 3906 / 711ms |
| 进程 CPU（`ps` 每 15 秒采样的均值，近似） | 约 2.6 核 | 约 2.1 核 | — |

- **ticker 这次有一次约 0.2 秒的整体停顿**：21:40:57 前后所有口径同时出现 >50ms（服务端业务最大 192ms、HB 164ms、往返 196ms、Sync 186ms），game 日志同一秒有一条 `nest handler held entity locks beyond threshold … hold=149.6ms`；宿主采样同一时刻 WindowServer 从 15% 升到 43%、Slack 与 Claude 应用出现在 CPU 前列（有人在操作桌面）。只有这一簇；除去它各分位数与夜间同档同量级或更低（服务端业务 P99 0.87 对 1.57ms，Sync 提交→客户端 P99 9.42 对 10.14ms）。判断为宿主停顿，**尚未确认**（没有 trace，单样本）。
- **心跳驱动 vs 帧回调**：进程 CPU 约 −19%，short 等待峰值 2614 → 139，Sync 提交→客户端 P99 9.42 → 6.07ms、Sync 帧数多 56%（心跳把改动摊在整个周期里，帧回调按帧集中改动、按帧合并）；代价是**普通业务消息的服务端 P99 0.87 → 2.13ms**（批量作业占 worker，方向与 [NEST-BATCH-HEARTBEAT §3G](NEST-BATCH-HEARTBEAT-20261021.md#3g-2026-10-22-晚参数补测结果v1310-发版前压测第-1-项) 一致，这里回调带事务、更重，所以差距更大），业务端到端（混合口径）P99 11.25 → 11.44ms 基本不变（由压测端节拍主导）。

### 4. 驱动微基准与开环投递

`BenchmarkDriverDispatch -benchtime 2000000x -count 10`（21:59～22:15，开始 loadavg 4.65，结束 3.24；inline 客户端，不经 Nest，只量驱动本身；[driver-benchstat.txt](benchmarks/v1310-prerelease-20261022/hb-load/driver-benchstat.txt)）：

| 档位 | dispatch cpu-ns/拍 / 驱动核 | batch cpu-ns/拍 / 驱动核 | batch vs dispatch |
| --- | --- | --- | --- |
| 5 万登记 × 100ms（50 万拍/s） | 777.4 / 0.389 | 869.3 / 0.435 | **+11.8%**（p=0.000） |
| 10 万登记 × 100ms（100 万拍/s） | 495.2 / 0.495 | 513.0 / 0.513 | ~（p=0.066） |
| 5 万登记 × 1s（5 万拍/s） | 2413 / 0.121 | 2398 / 0.120 | ~ |

两种投递都追上目标速率（beats/s 与目标差 < 0.1%），无跳拍。inline 客户端下批量投递在 50 万拍/s 档多约 12% 驱动 CPU（组批的额外工作，Nest 的节省不在这个基准里体现）；端到端的节省见 §3 与 NEST-BATCH-HEARTBEAT §3G。

开环投递 `TestDriverOpenLoop`（真实 Nest + 空 handler，缺省批量投递，short 8 / 65536，每档 20 秒，两轮反序，22:16～22:20，开始 loadavg 3.26～3.52）：

| 目标拍/s | 实际执行/s | 进程 CPU 核（两轮） | 间隔 P50 / P99 / max ms（两轮） | 快池等待峰值 | 最大 worker 等待 ms | 跳拍 / 卡住 / 投递≠执行 |
| --- | --- | --- | --- | --- | --- | --- |
| 10 万 | 100000 | 0.89 / 0.90 | 100.0 / 104.1 / 105.4；100.0 / 103.8 / 105.4 | 30 / 9 | 12.4 / 2.8 | 0 / 0 / 0 |
| 25 万 | 250000 | 1.44 / 1.46 | 100.3 / 104.7 / 106.0；100.1 / 102.3 / 104.6 | 33 / 33 | 5.0 / 4.5 | 0 / 0 / 0 |
| 50 万 | 499982 / 500000 | 2.16 / 2.15 | 100.0 / 102.5 / 103.9；99.9 / 101.6 / 103.6 | 71 / 71 | 3.9 / 3.8 | 0 / 0 / 0 |

判据（清单 §6）：心跳模式零跳拍、零动作失败、投递 = 执行 **成立**；nest-game 心跳路径 `verified=true`、每 Unit 拍数 8999～9000（时长 / 档位 = 9000，± 2 内）**成立**；开环各档的 CPU、跳拍与间隔分布见上表（零跳拍，间隔 P99 ≤ 104.7ms）。

**对“压测工具缺省驱动”（DESIGN-TODO 待决定第 9 项）的数据**：心跳驱动更省 CPU、Sync 延迟更低、多视野下不失守；但它改变了负载形状（固定移动者 vs 每 tick 换一批），与历史报告不再可比，普通业务消息的服务端 P99 也更高。是否改缺省仍是产品选择，交维护者；本轮不改。

## 5. 不能证明什么

- 没有任何性能数字：心跳驱动与全局循环谁快、D19 多视野 Nest 模式在心跳驱动下是否仍失守，都未知。
- 心跳模式的移动者是固定集合、持续巡逻，不是“走一段、停一段”的随机漫游；改值速率与全局循环相同，空间分布不同（全局循环每 tick 换一批实体）。
- sync-aoi 端到端用例只有 16 个实体、0.6 秒；nest-game 的 `TestNestGameLoad` 心跳路径没有运行过。
