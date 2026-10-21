# D20 第 4 期：压测工具改为心跳驱动（2026-10-21，只交付代码，测量待补）

**本页没有性能结论。** 维护者 2026-10-21 要求先暂停压测、把代码收尾；同日宿主上有别的耗资源程序，冒烟也不跑。sync-aoi 与 nest-game 的心跳驱动模式、§7 的微基准都已实现、能编译，`go test` 里的短断言（几百毫秒、几十个实体）通过；正式对比放到 v1.31 发版前的压测，复跑命令见 §4。

设计来源：[DESIGN-TODO D20](../maintenance/DESIGN-TODO.md#d20-移动由实体自己的心跳驱动mapbasic--move-组件--actionflow-控制) P4 行与 P5 表里 sync-aoi、nest-game 两行；结构与投递方式的前置压测见 [HB-STRUCTURES-20261021](HB-STRUCTURES-20261021.md)。

## 1. 新增开关

### sync-aoi（`scripts/perf/sync-aoi`，提交 `3c5e41e3`）

| 开关 | 缺省 | 含义 |
| --- | --- | --- |
| `-move-driver=loop\|heartbeat` | `loop` | Nest 模式下谁驱动移动。`loop` 是原负载：压测端循环每 50ms tick 挑 `-dirty`% 实体、分 `-batches` 批发 `Request`，handler 里 `QueueMove`。`heartbeat`：每个实体装 `gameplay/heartbeat` 宿主、`gameplay/movement` 的 `MapBasic` / `Move` 与 `actionflow.BeatRunner`，被选中的移动者在原往返带上 `MoveTo` 随机目标巡逻，到达后在 `OnEnded` 里接下一段，runner 的移动组心跳每拍 `Move.Advance(Beat.Elapsed)` → `MapBasic.SetPos` → `QueueMove`。只支持 `-nest`（直接模式只有 loop） |
| `-move-period` | `100ms` | 心跳模式移动组心跳的档位（`heartbeat.Periods` 之一） |
| `-move-speed` | `30` | 巡逻速度（格/秒）。缺省每拍 3 格，与全局循环每次改值走的 3 格相同，保证每拍都换格、每拍一次改值 |
| `-movers` | `0` | 巡逻的实体数；0 按与全局循环相同的改值速率折算：`ceil(每 tick 改值数 × 20/s × move-period)`。缺省参数（1 万实体、1%）是 200 个，每秒 2000 次改值 |

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

```sh
# 1. sync-aoi：全局循环与心跳驱动对照（原场景；各 3 轮，交替跑）
ROOST_PERF_LABEL=p4-aoi-loop      bash scripts/perf/sync-aoi.sh -players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50 -move-driver=loop
ROOST_PERF_LABEL=p4-aoi-heartbeat bash scripts/perf/sync-aoi.sh -players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50 -move-driver=heartbeat

# 2. D19 多视野 Nest 模式复测（每玩家 4 / 8 / 16 个额外视野，两种 apply 模式，两种驱动）
for views in 4 8 16; do for apply in sync async; do for driver in loop heartbeat; do
  ROOST_PERF_LABEL=p4-aoi-v$views-$apply-$driver ROOST_PERF_COUNT=2 \
    bash scripts/perf/sync-aoi.sh -players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50 \
      -views-per-player=$views -apply-mode=$apply -move-driver=$driver
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

冒烟（缩小规模，只证明能跑通、指标有数）可用 `-players=100 -entities=1000 -ticks=100` 与 `ROOST_NEST_GAME_ENTITIES=1000 ROOST_NEST_GAME_PLAYERS=100 ROOST_NEST_GAME_DURATION=10s`，结论栏写“正式对比待 v1.31 发版前压测”。

**冒烟结果（2026-10-22，main `11988df8` + 夹具修正，宿主 1 分钟 loadavg 约 2～3，只证明能跑通，正式对比仍待 v1.31 发版前压测）**：

| 命令（缩小规模） | 结果 |
| --- | --- |
| sync-aoi `-players=100 -entities=1000 -ticks=100 -move-driver=loop` | 跑通，帧 4177、改值→客户端 P50 / P99 26.2 / 49.0ms；进程以非零码退出是夹具自己的 50ms 延迟门槛（33 / 4570 个样本超 50ms），报告完整 |
| 同上 `-move-driver=heartbeat` | 跑通，门槛内（0 / 4768 超 50ms），帧 4033、P50 / P99 11.8 / 42.0ms；移动者 20、`Delivered = Executed = 1109`、跳拍 / 停滞 / 动作失败 0、巡逻段 181；`beat_interval_ms` P50 100.0 / P99 200.1（946 个间隔里 29 个超过周期 + 50ms：间隔按“位置真的变了”的两次 `SetPos` 计，换段那一拍不动，所以出现 200ms，属口径，不是跳拍） |
| 同上 + `-views-per-player=4 -apply-mode=sync` / `async` 心跳驱动 | 都跑通；sync 门槛内（P99 45.0ms），async 超门槛（P99 52.5ms，2220 / 19743 超 50ms，与 ApplyAsync 多约一个周期一致） |
| nest-game `ROOST_NEST_GAME_HEARTBEAT_DRIVER=ticker`，`ENTITIES=1000 PLAYERS=100 DURATION=10s`，其余按本节第 3 条（EMBEDDED、SYNC、HZ=10） | `TestNestGameLoad` PASS：消息 999.4/s、心跳 9993.9/s，消息 / 心跳 p99 11.96 / 11.40ms，计数、终值、延迟检查都为 true |
| 同上 `heartbeat`（`TestNestGameLoad` 的心跳驱动路径） | PASS：每 Unit 99～100 拍（期望 100）、间隔 p99 100.0ms、跳拍 0 / 0 / 0、停滞 0、`verified=true`；消息 999.5/s、心跳 9984.6/s，p99 11.50 / 0.040ms（心跳从回调开始计，口径见 §1） |
| `scripts/perf/nest-game.sh --check-heartbeat` | PASS（`TestHeartbeatDriverTicksGeneratedUnits`：632 拍全部在事务里执行，跳拍 0） |

sync-aoi 的 Nest 模式之前每 tick 调一次 `interest.Apply()`，Interest 单写者重构之后它会在压测线程执行排队移动，2026-10-22 已改为只在 direct 模式调用（`9ad54c3c`，见 [INTEREST-SINGLE-WRITER-20261022](INTEREST-SINGLE-WRITER-20261022.md)）；上表在改后的夹具上跑。原始产物在当次 worktree 的 `artifacts/perf/`（未入库）。

## 5. 不能证明什么

- 没有任何性能数字：心跳驱动与全局循环谁快、D19 多视野 Nest 模式在心跳驱动下是否仍失守，都未知。
- 心跳模式的移动者是固定集合、持续巡逻，不是“走一段、停一段”的随机漫游；改值速率与全局循环相同，空间分布不同（全局循环每 tick 换一批实体）。
- sync-aoi 端到端用例只有 16 个实体、0.6 秒；nest-game 的 `TestNestGameLoad` 心跳路径没有运行过。
