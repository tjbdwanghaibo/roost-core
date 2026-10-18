# 时间与定时器：实现与维护

适用版本：v1.27.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）；源码目录最初按 v1.24.0 提取、之后随改动维护，链接由根包文档门禁保证存在，缺漏以源码为准。[设计与使用](../guide/10-time.md)

## 如何阅读

测试时你可能想让活动时间快进，但不希望数据库连接也立刻超时。框架因此把业务时间和系统时间分开。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`infra/base/clock`、`infra/base/timer`，以及按实体的持久定时器驱动 `framework/nest/entitytimer`（`wiring/entitytimer` 装配，生成器见 [impl/12-codegen](12-codegen.md#2c-roost-add-timer)）。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 业务时间与系统期限分开注入和使用。
2. 运行业务时间不倒退；恢复受高水位约束。
3. 宿主持久精度决定入队对齐，不隐式改变 Scheduler 纳秒契约。
4. **`byID` 持有全部活节点，节点带显式状态**（2026-10-17 重构，[方案](../../maintenance/REFACTOR-2026-10-17-timer-scheduler.md)）：`stateHeap`（在堆中）、`stateFiring`（已出堆、回调执行中，含外层重入帧）、`stateQueued`（等最外层 Tick 收尾入堆：Tick 内新建、Tick 内对堆中节点改期、触发中节点显式改期）、`stateDetached`（已取消 / 已结束 / 克隆值，不在 `byID`）。`byID[id]==n` ⇔ 状态是前三种；`stateHeap` ⇔ 在堆的 `index` 位置；Tick 外只有 `stateHeap`、`queued` 为空。`NewTimer*` / `ChangeTimer` / `RemoveTimer` / 回调返回值都在调用当下改节点（当时取 now、复制 payload），不再按 ID 推迟闭包；回调返回后只有仍是 `stateFiring` 的节点才按返回值处理，所以显式改期优先于返回值、取消（detach）后的改期找不到节点、不会复活。
5. **只有最外层 Tick 收尾**（`settle`）：先清 `running`、取走 `queued`、把仍是 `stateQueued` 的节点全部入堆，再按入队顺序对仍在堆里的 typed 节点各发一次 upsert。钩子 panic 时状态已经落定，没有残留可重放（`TestOnChangePanicDuringSettlementDoesNotReplay`）；回调 panic 由 `fire` 的 defer 丢弃仍在 `stateFiring` 的节点、保留已发生的显式改期。Tick 内取消从未通知过钩子的新节点时先补一次 upsert 再删除，宿主（World）据此推进持久 seed、ID 不复用。对不在 `byID` 的 ID 操作返回 false，Tick 内外一致（维护者 2026-10-17 决定；此前 Tick 内返回 true 表示“已接受”）。
6. **节点离开堆时按钩子视图标志发变化**（`emitLeave`，取消与到期出堆共用）：`announced` 为假先补一次 upsert（宿主据此推进持久 seed，ID 不复用），`persisted` 为真再发删除；出堆通知里被取消或改期的节点，回调不执行。原因是“先落定再回调”：收尾已把等待节点全部入堆、还没轮到它们的 upsert 时，钩子可以重入 Tick 或取消，此时堆里有从未通知（Tick 内新建）或钩子不持有（触发中显式改期、出堆时已删除）的节点。不要把“在堆里”当成“钩子持有”。
7. **读视图与操作用同一个活节点集合**：`Lookup(id)` / `LiveNodes()` 读 `byID`（堆中、触发中、等收尾入堆），返回副本，`LiveNodes` 按触发顺序 `(End, Priority, ID)`（`firesBefore` 复用 `timerHeap.Less`，顺序只定义一次，堆的比较路径不变）。只读，不改状态、标志与 `queued`，§2.1 不变量与 §2.2 行为表不受影响。所以 Tick 内新建的立即可读、改期的立即是新期限、取消或已结束的立即读不到，`Lookup` 为 true 当且仅当此刻 `ChangeTimer` / `RemoveTimer` 会成功。正在触发的节点在出堆通知、回调及其中重入 Tick 期间可读，期限是原定期限（≤ 本次 Tick 时刻），显式改期后是新期限，回调按返回值续期之前不变。`Nodes()` 仍是只含堆中 typed 节点、不排序的持久化视图，Tick 内看不到等待收尾的节点，不能拿来回答“回调刚写的在不在”。两张组合表（见第 8 条）都在每次操作前后核对读视图与返回值一致（`checkLiveView`）：回调位置操作前读得到 ⇔ 改期 / 取消返回 true；钩子位置（出堆通知中、收尾已入堆未通知）目标都在 `byID`、都读得到；操作后立即读到新期限或读不到。变异核对：读视图隐去等收尾节点，回调位置表 42 组、钩子位置表 2 组变红；隐去触发中节点，分别 12 组、8 组变红。
8. 修改这套状态时，用两张组合表核对行为表：`TestOperationsOnEveryNodeStateDuringTick`（操作 × 状态 × 返回值 × 回调里直接 / 重入 Tick 两种调用位置，82 组；在重构前的基线上同样全部通过）与 `TestHookOperationsOnEveryNodeStateItCanSee`（持久化钩子这一调用位置：出堆通知中、收尾已入堆未通知的新建 / 改期 / 自改期节点、补发首次 upsert 时 × 改期 / 取消 / 重入 Tick / 不操作，28 组）。两张表都断言钩子不收到不持有的删除、每个分配过的 ID 都经过 upsert。

## 2A. 按实体的持久定时器驱动

DESIGN-TODO D11（维护者 2026-10-17 决定方案 B 与离线语义）。`framework/nest/entitytimer` 两个类型：`Driver`（一种实体定时器一个，生成组件的包级变量）与 `Host`（每个实体实例一个，组件持有）。

1. **期限只在 DAO 里**：`Host` 每次最外层调用经 `Store`（生成组件把 DAO 的节点 map 与种子适配进来）现建 `timer.Scheduler`，钩子 `persist` 写回 DAO 并按 upsert 推进种子；`Host` 不持有任何事务会改的状态。Tick 回调里经同一组件的 `NewTimer` / `ChangeTimer` / `RemoveTimer` 复用这次 Tick 的调度器（`Host.active`，只在 `Tick` 调用栈内非空，返回或 panic 都清掉），按 `timer.Scheduler` 的 Tick 内规则生效：取消的同轮到期节点不再触发、改期的不按旧期限触发、新建的等收尾入堆。不能从中间 DAO 状态另建一个调度器：那份堆与外层不同步，外层会照旧触发已取消的节点，且外层最后的报告会覆盖内层登记的新期限（修前缺陷，回归 `TestHostCallbackOperationsShareTheCurrentTick`）。`Timer` / `Timers` 在 Tick 调用栈内（`active` 非空）读这份调度视图（`Lookup` / `LiveNodes`，见 §2 第 7 条），其余时候读 DAO：回调里新建、改期的节点要到最外层收尾才 upsert 回 DAO，读 DAO 会看不到自己刚写的，“没有就建一个”会在一次 Tick 内重复登记（13b9d9e0 引入、本条修复，回归同一矩阵的 `callback reads its own writes`）。正在触发的节点已在出堆时从 DAO 删除，但在自己的回调里可读（原定期限；显式改期后为新期限）：语义取“可读 ⇔ 可改期 / 取消”，避免周期回调查不到自己又登记一个。它的 `loadedAt`（发布前写一次）与 `seq`（实体锁内递增、不随回滚退回，只给报告排序）都不是事务状态。所有方法除 `OnDestroy` 外只在实体锁内调用。
2. **索引只在提交后变**：每次最外层调用末尾 `settle`（`active` 非空的内层调用不报告、不推进 `seq`） 把调度器剩下的最早期限（有非法存量节点时报“现在”，让事务内 Tick 清理）报告给驱动；在事务里经 `nest.AfterCommit`，不在事务里（加载）立即报告。回滚、提交被拒、结果未知（abandon）都不执行提交后回调，索引保持事务前的值，与回滚后的 DAO 一致。强制点：`host.go` `settle`。
3. **同一实例的报告按序号排定**：`AfterCommit` 在锁释放后执行，两个相继事务的回调可能乱序到达；驱动只接受序号更大的报告（`driver.go` `report`）。实例关闭（`OnDestroy`：卸载、删档、撤销新建）后永久丢弃它的报告，晚到的提交回调不会把它放回索引（`close`）。
4. **只投递到期的那一个、只投一次**：`DeliverDue` 取出唤醒时刻已到的条目，投递后条目转为“待报告”、唤醒时刻换成 `Redeliver`（缺省 10s）之后；Tick handler 提交时的报告把它换回新的最早期限。没有报告就重投（幂等：Tick 只触发此刻已到期的节点）。准入失败（队列满、停止中）1s 后重试；出锁投递回来按 `gen` 判断期间有没有新报告。投递走 `nest.Client.Dispatch`（正式准入、单目标、参数为业务时间 UnixMilli），驱动的更新只持一把短锁、不做 I/O，可以在 Nest 业务池上调用。
5. **到期时核对实例**：`IsRemoved`（Destroy 在锁内先置位、`OnDestroy` 在解锁后）或查到同 ID 的另一个实例时丢弃条目、不投递——驱动不唤醒不在内存的实体。查不到任何实例时照常投递：那是实例刚构建、还没发布（加载中；Nest 冷加载会等这次加载完成）。剩余窗口：投递准入之后、消息执行之前实例被卸载，Nest 按“发给未加载实体的消息”冷加载它，于是加载后补触发一次（与任何发给该实体的消息相同；删档实体加载失败，不触发）。见 KNOWN-LIMITS“按实体的持久定时器”。
6. **离线语义**：加载（`OnInitFinish(isCreate=false)`）记下 `loadedAt`；触发时 `Fire.Overdue = 原定期限不晚于 loadedAt`。Scheduler 对一个期限早已过去的节点只触发一次、按返回值从 now 续期，所以“错过多少个周期都只触发一次”不需要额外代码；框架不计算错过的次数、不循环补触发。期限按毫秒：`Driver.Now` 与 `Host.Tick` 都截到毫秒，与节点持久精度一致。
7. **时钟**：缺省 `clock.Now`；`wiring/entitytimer` 在 Provide 时换成 `app.BusinessClock`（实体加载之前）。测试用 `SetClock` 换可控时钟、不启动循环直接调 `DeliverDue`。

验证：`framework/nest/entitytimer` 的 `driver_test.go`（只投到期的、待报告不重投、静默重投、旧报告丢弃、关闭后不投、核对实例、准入失败重试、同时刻按 ID、循环被更早期限叫醒、`-race` 下加载 / 卸载 / 期限变化 / 投递并发）、`host_test.go`（在线一次、周期续期、离线错过十个周期加载后只触发一次且参数正确、重建不复用 ID、多定时器最早期限、登记错误在初始化报出；回调内登记下一阶段、取消 / 改期同轮到期的另一阶段、显式改期自己、回调读到自己的写入、panic 清理、回滚后重试）、`nest_test.go`（真实 Nest 引擎：回滚不改索引、提交后才改、投递的 Tick 在锁内执行并清掉待报告）；变异核对（提交前立即报告、接受旧报告、不核对实例、关闭后仍接受报告、`Overdue` 恒假）各让对应用例变红，记录在 [D11 实施说明](../../maintenance/DESIGN-TODO.md#d11-按实体的持久定时器接入)。生成工程端到端见 `codegen/internal/roost/add_timer_test.go` 的 `TestEntityTimerGeneratedProjectEndToEnd`。

## 3. 并发、失败与恢复的修改检查

持久化钩子是第三个调用位置（前两个是回调里直接调用、回调里重入 Tick）：钩子里取消、改期、新建和重入 Tick 都允许，钩子能看到回调里看不到的过渡状态（见 §2 第 6 条）。改动发变化的路径时，在 `TestHookOperationsOnEveryNodeStateItCanSee` 里补对应的状态行，而不是只加单点用例；2026-10-17 重构后第一处缺陷（收尾钩子重入 Tick 触发尚未通知的新节点，ID 未经 upsert）就出在这个维度，见[重构方案 §9](../../maintenance/REFACTOR-2026-10-17-timer-scheduler.md#9-实施后发现)。


修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### clock

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [clock.go](../../../infra/base/clock/clock.go) | `Business`、`BusinessFunc`、`Clock` |

### timer

1 个实现文件、4 个测试文件（含基准 `bench_test.go`）。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [scheduler.go](../../../infra/base/timer/scheduler.go) | `ChangeType`、`Node`、`Context`、`Handler`、`ChangeFunc`、`Scheduler` |

### entitytimer

2 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [driver.go](../../../framework/nest/entitytimer/driver.go) | `Config`、`Lookup`、`Driver` |
| [host.go](../../../framework/nest/entitytimer/host.go) | `Store`、`Fire`、`Handler`、`Handlers`、`Host` |
| [wiring/entitytimer/entitytimer.go](../../../wiring/entitytimer/entitytimer.go) | `Mod` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Clock`、`Test.*Time`、`Test.*Scheduler`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### clock

- [clock_test.go](../../../infra/base/clock/clock_test.go)：`TestLogicClockOffsetRoundTripsAtDurationResolution`、`TestLogicClockSetLandsWithinOneMillisecond`、`TestInjectedClocksAreIndependentOfTheGlobalOne`、`TestLogicClockIsSafeForConcurrentUse`、`TestProcessClockKeepsConfiguredSubmillisecondOffset`

### timer

- [order_test.go](../../../infra/base/timer/order_test.go)：`TestTimersWithTheSameDeadlineFireInRegistrationOrder`、`TestPriorityOrdersTimersWithTheSameDeadline`、`TestPriorityIsKeptThroughStorageAndRescheduling`、`TestADueTimerWithoutAHandlerIsDroppedWithAWarningAndACount`、`TestStoredTypesWithoutAHandlerAreReportedOncePerType`
- [scheduler_test.go](../../../infra/base/timer/scheduler_test.go)：`TestSchedulerFiresAndPersistsChanges`、`TestSchedulerReschedulesFromHandler`、`TestSchedulerSetClockStampsNewTimers`、`TestRemoveFromAHandlerStopsATimerDueInTheSameTick`、`TestChangeFromAHandlerPostponesATimerDueInTheSameTick`、`TestFiringTimerExplicitRescheduleOverridesReturn`（含 typed 持久节点恢复）、`TestFiringTimerOperationsComposeAcrossReentrantTicks`（改期、取消、重入和 panic 收尾）、`TestOperationsOnEveryNodeStateDuringTick`（操作 × 状态组合表）、`TestHookOperationsOnEveryNodeStateItCanSee`（钩子调用位置的组合表）、`TestOnChangePanicDuringSettlementDoesNotReplay`（收尾时钩子 panic 不重放）；其余入口见文件。基准见 [bench_test.go](../../../infra/base/timer/bench_test.go)（1 万常驻定时器、每 Tick 触发 100 个；World 式重建 + Tick），重构前后对照见 [样本](../../performance/benchmarks/timer-scheduler-refactor-20261017/)。登记快照回归见 [registration_test.go](../../../infra/base/timer/registration_test.go)：回调内创建的 typed/closure timer 保留调用时期限，payload 在调用返回前复制，持久化不受调用方复用缓冲影响；`TestHandlerThatArmsAChildAndReschedulesItselfUsesCallTime` 核对与回调内显式改期叠加时两者都按调用时刻定期限。

### entitytimer

- [driver_test.go](../../../framework/nest/entitytimer/driver_test.go)：`TestDriverDeliversOnlyDueEntitiesOnce`、`TestDriverReportClearsInflightAndRedeliversWhenSilent`、`TestDriverDropsStaleReports`、`TestDriverNeverDeliversToAClosedOwner`、`TestDriverValidatesTheOwnerBeforeDelivering`、`TestDriverRetriesRefusedAdmission`、`TestDriverDeliversSimultaneousDeadlinesInIDOrder`、`TestDriverLoopWakesForAnEarlierDeadline`、`TestDriverConcurrentReportsClosesAndDeliveries`
- [host_test.go](../../../framework/nest/entitytimer/host_test.go)：`TestHostFiresAnOnlineDeadlineOnce`、`TestHostRearmsAPeriodicTimerByItsReturnValue`、`TestHostFiresAnOverdueTimerOnceAfterLoad`、`TestHostDoesNotReuseIDsAcrossReload`、`TestHostIndexesTheEarliestOfSeveralTimers`、`TestHostReportsBadHandlerRegistrationAtInit`
- [nest_test.go](../../../framework/nest/entitytimer/nest_test.go)：`TestHostReportsThroughNestTransactions`
- [wiring/entitytimer_test.go](../../../wiring/entitytimer/entitytimer_test.go)：`TestModBindsNestAndTheBusinessClock`、`TestModRefusesADriverInstalledTwice`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。没有新测量不能改容量承诺，性能结论以 [性能报告](../../performance/README.md) 为准。
