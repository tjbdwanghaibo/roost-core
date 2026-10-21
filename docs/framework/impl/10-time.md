# 时间与定时器：实现与维护

适用版本：v1.29.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）；源码目录最初按 v1.24.0 提取、之后随改动维护，链接由根包文档门禁保证存在，缺漏以源码为准。[设计与使用](../guide/10-time.md)

## 如何阅读

测试时你可能想让活动时间快进，但不希望数据库连接也立刻超时。框架因此把业务时间和系统时间分开。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`infra/base/clock`（业务时钟与业务日历）、`infra/base/timer`，按实体的持久定时器驱动 `framework/nest/entitytimer`（`wiring/entitytimer` 装配，生成器见 [impl/12-codegen](12-codegen.md#2c-roost-add-timer)），以及按实体的心跳 `gameplay/heartbeat`（`wiring/heartbeat` 装配，§2B）。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 业务时间与系统期限分开注入和使用。
2. 运行业务时间不倒退；恢复受高水位约束。
3. 宿主持久精度决定入队对齐，不隐式改变 Scheduler 纳秒契约。
4. **`byID` 持有全部活节点，节点带显式状态**（2026-10-17 重构，[方案](../../maintenance/REFACTOR-2026-10-17-timer-scheduler.md)）：`stateHeap`（在堆中）、`stateFiring`（已出堆、回调执行中，含外层重入帧）、`stateQueued`（等最外层 Tick 收尾入堆：Tick 内新建、Tick 内对堆中节点改期、触发中节点显式改期）、`stateDetached`（已取消 / 已结束 / 克隆值，不在 `byID`）。`byID[id]==n` ⇔ 状态是前三种；`stateHeap` ⇔ 在堆的 `index` 位置；Tick 外只有 `stateHeap`、`queued` 为空。`NewTimer*` / `ChangeTimer` / `RemoveTimer` / 回调返回值都在调用当下改节点（当时取 now、复制 payload），不再按 ID 推迟闭包；回调返回后只有仍是 `stateFiring` 的节点才按返回值处理，所以显式改期优先于返回值、取消（detach）后的改期找不到节点、不会复活。
5. **只有最外层 Tick 收尾**（`settle`）：先清 `running`、取走 `queued`、把仍是 `stateQueued` 的节点全部入堆，再按入队顺序对仍在堆里的 typed 节点各发一次 upsert。钩子 panic 时状态已经落定，没有残留可重放（`TestOnChangePanicDuringSettlementDoesNotReplay`）；回调 panic 由 `fire` 的 defer 丢弃仍在 `stateFiring` 的节点、保留已发生的显式改期。Tick 内取消从未通知过钩子的新节点时先补一次 upsert 再删除，宿主（World）据此推进持久 seed、ID 不复用。对不在 `byID` 的 ID 操作返回 false，Tick 内外一致（维护者 2026-10-17 决定；此前 Tick 内返回 true 表示“已接受”）。
6. **节点离开堆时按钩子视图标志发变化**（`emitLeave`，取消与到期出堆共用）：`announced` 为假先补一次 upsert（宿主据此推进持久 seed，ID 不复用），`persisted` 为真再发删除；出堆通知里被取消或改期的节点，回调不执行。原因是“先落定再回调”：收尾已把等待节点全部入堆、还没轮到它们的 upsert 时，钩子可以重入 Tick 或取消，此时堆里有从未通知（Tick 内新建）或钩子不持有（触发中显式改期、出堆时已删除）的节点。不要把“在堆里”当成“钩子持有”。
7. **读视图与操作用同一个活节点集合**：`Lookup(id)` / `LiveNodes()` 读 `byID`（堆中、触发中、等收尾入堆），返回副本，`LiveNodes` 按触发顺序 `(End, Priority, ID)`（`firesBefore` 复用 `timerHeap.Less`，顺序只定义一次，堆的比较路径不变）。只读，不改状态、标志与 `queued`，§2.1 不变量与 §2.2 行为表不受影响。所以 Tick 内新建的立即可读、改期的立即是新期限、取消或已结束的立即读不到，`Lookup` 为 true 当且仅当此刻 `ChangeTimer` / `RemoveTimer` 会成功。正在触发的节点在出堆通知、回调及其中重入 Tick 期间可读，期限是原定期限（≤ 本次 Tick 时刻），显式改期后是新期限，回调按返回值续期之前不变。`Nodes()` 仍是只含堆中 typed 节点、不排序的持久化视图，Tick 内看不到等待收尾的节点，不能拿来回答“回调刚写的在不在”。两张组合表（见第 8 条）都在每次操作前后核对读视图与返回值一致（`checkLiveView`）：回调位置操作前读得到 ⇔ 改期 / 取消返回 true；钩子位置（出堆通知中、收尾已入堆未通知）目标都在 `byID`、都读得到；操作后立即读到新期限或读不到。变异核对：读视图隐去等收尾节点，回调位置表 42 组、钩子位置表 2 组变红；隐去触发中节点，分别 12 组、8 组变红。
8. 修改这套状态时，用两张组合表核对行为表：`TestOperationsOnEveryNodeStateDuringTick`（操作 × 状态 × 返回值 × 回调里直接 / 重入 Tick 两种调用位置，82 组；在重构前的基线上同样全部通过）与 `TestHookOperationsOnEveryNodeStateItCanSee`（持久化钩子这一调用位置：出堆通知中、收尾已入堆未通知的新建 / 改期 / 自改期节点、补发首次 upsert 时 × 改期 / 取消 / 重入 Tick / 不操作，28 组）。两张表都断言钩子不收到不持有的删除、每个分配过的 ID 都经过 upsert。

## 1A. 业务日历（DESIGN-TODO D18）

1. **配置只读一次**：`framework/app/config_schema.go` 的 `timeConfig` 声明 `time.business_timezone`（缺省 `UTC`）与 `time.business_day_start`（缺省 `00:00`），都带 `example`（生成器写进每个服务配置，`renderBusinessTimeConfig`）与 `shared`（跨服务一致）。`timeConfig.ValidateConfig` 经 `clock.LoadTimezone` / `clock.ParseDayStart` 解析并填 `calendar`：非法名、`Local`、空名、非两位 `HH:MM` 各自点名键报错。`App.run` 读出后 `clock.SetCalendar` 设进程级日历、写进 `starting server` 日志；`NewRegistry` 用同一份配置构造 `clock.NewBusinessIn(offset, calendar)`。强制点只有这一处，没有运行期修改入口。
2. **日历挂在业务时钟上**：`clock.CalendarOf(b)` 读时钟自带的日历（`NewBusinessIn`、`Process()`），没有的（`NewBusiness`、测试替换的 `BusinessFunc`、nil）取进程级日历。`app.BusinessCalendar(r)` = `CalendarOf(BusinessClock(r))`，所以注入了别的时钟的测试与 Registry 的日历不会分叉成两份来源。
3. **业务日的算法**（`calendar.go` `bounds`）：取 `t` 在业务时区的当地日期 `d`，起点 `startOf(d)` = `time.Date(d, 重置时刻, loc)`；`t` 早于起点就退一天，`t` 不早于下一天起点就进一天。`startOf` 对日期单调（夏令时最多挪一两个小时，一天不短于 22 小时），所以两个循环各至多一步就收敛，不变量是 `DayStart(t) ≤ t < NextDayStart(t)`、起点属于同一业务日、下一个起点属于日历上的下一天。重置时刻落在夏令时跳过的那一小时里时 `time.Date` 取哪一侧不保证，不变量照样成立。不要改成“减去重置时刻再取日期”或“起点 + 24h”：夏令时那天会错一小时（变异核对：改成固定 24 小时，`TestDaylightSavingDaysAreShortAndLong` 与整年不变量用例变红）。
4. **时区数据库**：`time.LoadLocation` 先找 `ZONEINFO`、再找系统目录，最后才用内嵌的 `time/tzdata`；只设 `ZONEINFO` 为空目录模拟不了“没有 zoneinfo”。生成的 `main.go`（`renderMain`）导入 `time/tzdata`；回归在 macOS 上用 `sandbox-exec` 拒绝读取系统 zoneinfo 与 `GOROOT/lib/time`，并先用不内嵌 tzdata 的对照程序确认环境确实看不到 zoneinfo（看得到就 Skip，例如自带 zoneinfo 的 Linux 主机）。

验证：`infra/base/clock/calendar_test.go`（缺省 UTC、东八区 05:00 重置跨日 / 跨月 / 跨年、负偏移、纽约 23h / 25h、重置落在跳过或重复的一小时、八种日历一整年每 17 分钟的不变量、解析拒绝、时钟带日历）；`framework/app/business_clock_contract_test.go` 的 `TestBusinessTimezoneIsDeclaredCheckedAndCarriedByTheBusinessClock`、`TestRunSetsTheProcessCalendarAndLogsIt`；`codegen/internal/roost/business_time_test.go`（生成配置写明、生成的 main 在看不到 zoneinfo 时加载 Asia/Shanghai）与 `config_contract_test.go` 的 `TestDoctorChecksCrossModConfigRulesThroughTheProcess`（编译出的 game-demo 进程拒绝拼错的时区；在看不到 zoneinfo 的环境里接受 Asia/Shanghai）；`cmd/glsvet/clockhints_test.go` 的 `TestClockHintsFlagMachineTimeZoneReads`。

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

## 2B. 实体心跳

DESIGN-TODO D20 第 1 期（维护者 2026-10-21 定方案与压测后决定）。`gameplay/heartbeat` 三个类型：`Driver`（一个 Nest 引擎一个，业务包级变量，`wiring/heartbeat` 装配）、`Host`（每个实体实例一个）、`Reg`（一个登记）。依赖边界：只 import `framework/nest`、`framework/entity` 与 infra，根包门禁把它归入 nest 块（`dependency_boundary_test.go`）。

1. **调度结构**（`tier.go`）：按档分环，`Resolution` 10ms；10 个固定档位 `Periods`，`tierOf` 拒绝其他周期（`ErrUnsupportedPeriod`）。每档一个环、环长 = 该档格数，登记只进一格 `cellOf = hash % 6000 % 本档格数`（`hash = misc.Hash64(实体 ID)`，各档按同一相位对齐），格是切片、登记记下标，撤销 / 换档 O(1)。原型与压测见 [HB-STRUCTURES-20261021](../../performance/HB-STRUCTURES-20261021.md)。环只在 `Driver.mu` 下读写。
2. **一个驱动 goroutine**（`driver.go` `loop`）：10ms ticker，按单调时间追到应到的格逐格 `step`；每 100 格 `flush` 一次。`step` 在 `mu` 下取出本格到期的登记，出锁逐个核对后交给投递层：实例已 `OnDestroy` / `IsRemoved` / 查到同 ID 的另一个实例 / 查不到（未发布）→ 丢弃；Remote 托管实例的所有权状态不是 `LocalOwned` / `Unknown` / `Recovering`（`mayOwn`）→ 丢弃；在途 → 跳拍。核对不阻塞、不持锁。
3. **投递层只换一处**：`deliverer.deliverBatch(ctx, []beat, rejected)` 一次交出一格的一批；P1 的 `dispatchDeliverer` 逐个 `nest.Client.Dispatch`（单目标、每个到期登记一条、不合并）。Nest 批量入队（D20 方案 D）落地后只换这个实现，驱动其余部分与语义不变。参数是预先建好的 `nest.Params{*Reg}`（Nest 准入时复制进消息，可复用）。
4. **至多一拍在途**：`Reg.inflight` 在投递前置位、在心跳 handler 返回时清掉（`defer`，panic 也清）；到期时仍在途就跳拍（`skipped` 计数交给下一拍的 `Beat.Skipped`）。准入失败（`ErrQueueFull` / 其他）立即清在途、计跳拍，不在本格重试；驱动停止时批里还没投的拍不再投、清在途（`deliverBatch` 看 `ctx`），`Stop` 返回后不再 `Dispatch`。没有水位让路与每格上限（维护者决定 ⑤）。
5. **卡住兜底**：在途超过 `StallAfter`（缺省 10s）视为丢失、重投并计 `stalled`；同一登记连续卡住时阈值翻倍（最多 3 次，80s），看到不在途时复位。丢失只来自 Nest 在执行 handler 之前放弃了消息（Remote 写准入被拒、准入后卸载且重载失败）。重投后两份消息至多一份执行回调：handler 先看在途标志，没有在途按 `duplicate` 丢弃。翻倍是因为拍没丢只是排得久时重投会多压消息（2s 不翻倍时 -race 冒烟里重投与积压互相放大，见 D20 实施记录）。
6. **事务内启停**（`host.go` `change`）：`Reg` 的 `active` / `tier` 是业务视角、只在实体锁内读写；变化时序号加一，在事务里登记 `DeferRollback`（恢复业务视角）与 `AfterCommit`（驱动生效），不在事务里立即生效。`Driver.apply` 只接受更大的序号（AfterCommit 在锁释放后执行、pipelined 下可能晚于后一个事务），实例 `OnDestroy` 后忽略。启动生效时先写 `startedAt` 再加 `startGen`，回调据此判 `First`。
7. **心跳 handler**（`runBeat`，包级注册 `heartbeat.beat.{memory,async,strict,pipelined}`，回滚策略 undo，持久级别由登记选）：依次核对在途、实例（`es[0]` 必须是登记所属实例且未 `OnDestroy`，卸载重载窗口里旧实例的拍按 `stale` 丢弃）、业务视角的启停（锁序上更早提交的 Stop）、Remote 实例是 `LocalOwned`；通过后算 `Beat`：`Elapsed` 从上次执行（首拍从 `startedAt`）起算，夹到 `[0, max(DefaultMaxElapsed, 档位)]`。回调侧的 `lastRun` / `lastGen` 随本拍事务回滚恢复（`DeferRollback`），回滚的一拍不算执行过。
8. **Remote 托管实体**：发给 Remote 托管 ID 的消息由 Nest 自动走 I/O 阶段的 Remote 写准入（`prepareRemoteWriteBatch`）：非拥有方被拒、`Unknown` 转 `LocalOwned`。旧拥有方转移时先排空已准入的写、实例转 `Draining` 再改权威，所以它最后一拍先于新拥有方的第一拍；驱动侧 `mayOwn` 与 handler 侧 `LocalOwned` 核对让非拥有方不投，并避免非拥有期间被拒的拍卡住新拥有方（变异核对 M6）。
9. **Remote 共享模式不允许挂载心跳**（维护者 2026-10-21，D20 P1 后决定 ④）：共享是运行期所有权状态（`remoteentity.Manager.EnterRemoteSharedMode` / `LeaveRemoteSharedMode` 按实体 ID 切换；实例加载时是 `Unknown`，共享权威下第一次写准入经 `newWriteLease` 的 marked 分支转 `Shared`）；`entity.RemotePolicy` 只有 None / Managed / Mirror，没有种类级的“共享”声明，登记时只能看到当时的状态。强制点三处：① `Host.Register` 与 `Reg.Start` 在实例处于 `Sharing` / `Shared` 时返回包裹 `ErrHeartbeatRemoteShared` 的错误（`host.go` `sharedErr`）；② 驱动 `mayOwn` 对 `Sharing` / `Shared` 照常投一拍（只为了进到实体锁内：业务视角的 `Active` 只在锁内改，不由驱动 goroutine 改）；③ `runBeat` 在 `Active` 核对之后、`LocalOwned` 核对之前遇到共享模式就 `change(false)` 停掉登记（随本拍事务 AfterCommit 生效；提交失败则下一拍再停），计 `stoppedShared`，不执行回调；驱动 `flush` 汇总增量、按 `WarnInterval` 限频打 Warn（`emitSharedWarn`，带间隔内停掉的个数、样本实体与登记名）。每个登记在共享模式下至多多投这一拍（共享模式写准入，取一次分布式锁），停掉后移出环、不再投。回到独占后不自动重启。检测点：Remote 没有所有权状态切换的回调（`entity.RemoteEntityBase.TransitionRemoteOwnership` 只做 CAS），心跳也不应依赖 `framework/remoteentity` 的内部实现，所以在心跳自己的两处读实例的所有权状态（`IThreadSafeRemoteEntity.RemoteOwnershipState`，只依赖 `framework/entity` 接口）：驱动到期时（原子读）与 handler 执行时（实体锁内）。保证：共享模式下任何一拍都不执行回调（handler 在锁内核对）；每个在共享期间到期的已启动登记都被停掉。限制：共享窗口短于登记的周期、期间没有到期的拍时，这个登记不会被停掉（期间也没有执行），回到独占后照常跳。变异核对：去掉 ① 的 Register 核对、去掉 ① 的 Start 核对、去掉 ② 的共享投递、去掉 ③ 的停止、Warn 不限频，各让 `remote_test.go` 对应用例变红。
9. **计数与日志**：驱动内部按实体 ID 分片的原子计数（回调侧由全部 worker 并发写），`flush` 每秒把增量写进 metrics 注册表；跳拍 / 卡住汇总进 `warnState`，按 `WarnInterval`（缺省 10s）打一条 Warn，带一个样本实体与原因。
10. **不改 Nest**：只用 `Client.Dispatch`、`HandlerMeta`、`CurrentRollbackTx` / `AfterCommit` / `DeferRollback`；entitytimer 不改。

验证（`GOWORK=off go test -race -count=1 ./gameplay/heartbeat ./wiring/heartbeat`）：`driver_test.go`（档位规则与相位打散、在途跳拍 / 准入满 / 限频 Warn、卡住重投翻倍与只执行一次、换档、多登记不合并与按登记选 handler、投递前与执行时的实例核对、Stop 顺序、并发启停 / 推进 / 执行）；`nest_test.go`（真实 Nest：回调在实体锁与事务内、与同实体业务消息串行；慢池实体在慢池；事务回滚作废启停、回调报错回滚回调侧记录；卸载重载令牌；5 万实体冒烟，-race 下 2000 实体）；`remote_test.go`（两个引擎 + 模拟 Remote 写准入的所有权迁移：旧拥有方离开 `LocalOwned` 后零执行、新拥有方成为拥有方前零执行；共享模式下登记被拒、运行期进入共享（拥有方切换与第二个写者首拍准入）后心跳被停掉且回调零执行、多个登记全部停掉且 Warn 限频、共享期间 `Start` 被拒、回到独占后不自动恢复且重新 `Start` 照常跳）；`wiring/heartbeat/heartbeat_test.go`（依赖 Nest、业务时钟、登记驱动、启停）。变异核对 M1～M11 各让对应用例变红，原文记录在 [D20 实施结果](../../maintenance/DESIGN-TODO.md#d20-移动由实体自己的心跳驱动mapbasic--move-组件--actionflow-控制)。

## 2C. 移动与动作的心跳驱动

DESIGN-TODO D20 第 2 期。`gameplay/movement`（`MapBasic`、`Move`、`MoveTo` / `Follow` 动作）与 `gameplay/actionflow` 的 `BeatRunner`，都建在 §2B 的心跳上，只用它的公共 API（`Host.Register`、`Reg.Start` / `Stop` / `SetPeriod` / `Active` / `Period`），不改投递层。依赖边界：`actionflow` 只多 import `gameplay/heartbeat`（同属 nest 块）；`movement` 建在三块之上（import nest、entity、`sync/entitysync/policy`、heartbeat、actionflow），不归入任何一块（`dependency_boundary_test.go`）。

1. **只在实体锁内、只做内存与入队**：三者的方法都在 Nest handler、心跳回调或加载 / 新建的初始化阶段调用（`Published` 除外）。心跳回调里只做内存计算与入队（写 DAO 字段、`QueueMove`、记提交后工作），不阻塞、不做 I/O、不 `Dispatch` / `Request`。
2. **位置的权威是 DAO**（`map_basic.go` `Fields`）：组件不另存位置。`SetPos` 先 `Space.QueueMove`、成功才写字段与登记回滚；`QueueMove` 由 `policy.Interest` 绑定当前 `SyncMutation` 的提交条件，提交确认后应用、回滚丢弃，所以“回滚的移动不生效”由 Interest 的提交门保证，组件只负责“被拒时什么都不写”。强制点：`SetPos`。
3. **进出场景在提交之后**（`addOp` / `stagedCommit` / `afterCommit`）：一个事务里的进出与要发布的快照攒进一条按**事务 ID**识别的记录，第一次改动时登记 `nest.AfterCommit`；提交后在实体锁外按顺序调 `Space.Enter` / `Leave`，再发布快照。之后的事务各有自己的记录，提交后回调只读自己那条，不读组件的其他状态（锁外读会与下一个事务竞争）。同一事务先进后出抵消；进场景后又 `SetPos` 时不入队（AOI 还不认识它），直接改记录里 Enter 的位置。回滚时记录作废（AfterCommit 不执行），`mapState`（所在 Space、是否待落库）由 `guard` 登记的 `DeferRollback` 恢复。不在事务里立即执行。强制点：`addOp`、`SetPos` 的 `entering` 分支、`guard`。
4. **发布按序号**（`publish`）：快照带序号，CAS 只接受更大的；`OnDestroy` 用最大序号封住。同一实体的提交后回调即使与下一个事务交错，发布也不倒退；回滚的改动从不发布。
5. **Move 的推进状态随事务回滚**（`move.go` `guard`）：`moveState`（模式、编号、亚格位置、路径进度、跟随参数）整体快照，每个事务第一次改动时登记 `DeferRollback`；编号 `nextToken` 只增、不回滚。路径切片设定后不再修改，快照可以共享。
6. **一拍只按 Elapsed 走一次**（`beat`）：位移 = 速度 × `Beat.Elapsed`，沿路径逐段推进；不追帧，`Beat.Skipped` 不参与计算。位置变化只在本拍结束时写一次（`commitStep`）：一拍走三格只入队一次。亚格位置 `x, y` 累积零头，`cell` 记上一次写进 `MapBasic` 的格；`MapBasic` 的位置被别处改过（瞬移）时从新位置接着走。
7. **结束即停心跳**（`finish`）：清状态、`reg.Stop()`，再回调 `Done`（回调里可以立刻开始新的移动）。调用方 `Stop` 不回调；`MoveTo` / `Follow` 取代进行中的移动时旧的以 `Replaced` 回调，回调在新状态生效之后。AOI 排队满（`policy.ErrInterestQueueFull`）整拍回到 `beatStart`、不结束；其他 `SetPos` 错误以 `Refused` 结束。
8. **跟随读提交后的快照**（`follow` + `LocateIn`）：`Locator` 在跟随者的锁内调用，只经实体管理器的只读查找拿到目标的 `MapBasic`，读它的原子快照；不取目标的锁。目标查不到 / `Present` 为假 → `TargetLost`，场景不同 → `TargetLeft`。
9. **动作由事件推进**（`action.go`）：`MoveTo` / `Follow` 的 `TickInterval` 为 0；`Start` 要求 `ctx.Owner` 实现 `MoveOwner`、`ctx.Waker` 非 nil（`BeatRunner` 填），调 `Move.MoveTo` / `Follow` 并记下编号；`Move` 的 `Done` 只认自己的编号，记下结论后 `Waker.Wake(action)`，runner 马上 tick 这个组、`Tick` 交出结果（`Arrived` 成功、`Replaced` 取消、其余失败）。`Cancel` 停自己那次移动（不回调）。
10. **BeatRunner 的启停**（`actionflow/heartbeat.go`）：包住业务的 `OnTransition` 记录每组的当前动作及其周期（`intervalOf`：未实现 `TickIntervaler` 取 `BeatConfig.Period`，0 为事件推进，正数取不大于它的最长档），每次切换 `Sync`：没有需要 tick 的动作就 `Stop`，否则换到最短档再 `Start`（同状态时心跳 API 不做事）。包住 `PopulateContext`（旧的 `Context` 钩子同样经它）补 `Owner`（取宿主实体）、`Waker`、`Elapsed`。每拍按组号顺序 tick 仍需周期 tick 的组（前面的组的回调可能换掉了后面的组的动作），再 tick 任务，最后 `Sync`；runner 的错误经 `OnError` 报告，本拍不回滚。
11. **runner 不随事务回滚**：这是 actionflow 的既有性质，心跳登记却随事务回滚，两者在“启停动作的事务回滚了”时可能不一致，直到下一次动作切换或业务调 `Sync`。见 KNOWN-LIMITS。
12. **Remote 共享模式**（main `96724b6a` / `d287031f`：`Reg.Start` 在共享模式返回 `ErrHeartbeatRemoteShared`，运行中进入共享时框架在下一拍停掉登记、回到独占不自动恢复）：`Move.begin` 先 `Start` 再改状态，挂不上就原样返回错误（进行中的移动照旧）；`MapBasic.Enter` 配了周期落库时先 `Start` `"map.save"`；`BeatRunner.Sync` 返回 `Start` 的错误（切换与每拍时经 `OnError` 报告）。被框架停掉之后组件不感知（心跳没有停止回调）：`Move` 仍处于移动中、不推进，`Move.Resume` / `BeatRunner.Sync` 由业务在回到独占后调用。强制点：`begin`、`Enter`、`Sync`（变异 M16～M18）。

验证（`GOWORK=off go test -race -count=1 ./gameplay/movement ./gameplay/actionflow`）：`move_test.go`（直接调回调：按速度与 Elapsed 推进、到达停心跳、跳拍一次覆盖与亚格累积、取代、被阻挡、跟随 / 目标销毁 / 目标离开、进出场景与落库、背压与拒绝）；`nest_test.go`（真实 Nest + 同步 Manager + Interest + 启动的心跳驱动：位置经 QueueMove 进 AOI、回滚的移动不生效；进出场景随事务；回滚的 MoveTo 不留心跳；runner 由心跳 tick 且空闲停；MoveTo 动作经移动心跳到达、被新的取代；Follow 在目标卸载后收尾）；`smoke_test.go`（3000 实体随机 MoveTo，观察者经同步帧收到每个实体的终点位置，-race 下 300）；`shared_test.go`（Remote 共享模式下拒绝、回到独占后 Resume / Sync）；`actionflow/heartbeat_test.go`（启停与档位、按组顺序 tick 与 Elapsed、Wake）。变异核对 M1～M18 原文见 [D20 P2 实施结果](../../maintenance/DESIGN-TODO.md#p2-实施结果2026-10-21)。

## 3. 并发、失败与恢复的修改检查

持久化钩子是第三个调用位置（前两个是回调里直接调用、回调里重入 Tick）：钩子里取消、改期、新建和重入 Tick 都允许，钩子能看到回调里看不到的过渡状态（见 §2 第 6 条）。改动发变化的路径时，在 `TestHookOperationsOnEveryNodeStateItCanSee` 里补对应的状态行，而不是只加单点用例；2026-10-17 重构后第一处缺陷（收尾钩子重入 Tick 触发尚未通知的新节点，ID 未经 upsert）就出在这个维度，见[重构方案 §9](../../maintenance/REFACTOR-2026-10-17-timer-scheduler.md#9-实施后发现)。


修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### clock

2 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [clock.go](../../../infra/base/clock/clock.go) | `Business`、`BusinessFunc`、`Clock` |
| [calendar.go](../../../infra/base/clock/calendar.go) | `Day`、`Calendar` |

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

### heartbeat

3 个实现文件、6 个测试文件（另有 `internal/bench` 压测原型）。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [tier.go](../../../gameplay/heartbeat/tier.go) | `Resolution`、`Periods` |
| [driver.go](../../../gameplay/heartbeat/driver.go) | `Config`、`Lookup`、`Driver`、`Stats` |
| [host.go](../../../gameplay/heartbeat/host.go) | `Beat`、`Func`、`RegOption`、`Host`、`Reg` |
| [wiring/heartbeat/heartbeat.go](../../../wiring/heartbeat/heartbeat.go) | `Mod` |

### movement

3 个实现文件、8 个测试文件（含 `example_test.go`）。心跳驱动的 runner 在 [actionflow/heartbeat.go](../../../gameplay/actionflow/heartbeat.go)（`BeatConfig`、`BeatRunner`）。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [map_basic.go](../../../gameplay/movement/map_basic.go) | `Facing`、`Fields`、`Space`、`InterestSpace`、`Snapshot`、`MapConfig`、`MapBasic`、`MapOwner`、`MemFields` |
| [move.go](../../../gameplay/movement/move.go) | `StopReason`、`Result`、`Done`、`Locator`、`MoveConfig`、`MoveOwner`、`Move` |
| [action.go](../../../gameplay/movement/action.go) | `MoveToParam`、`FollowParam`（`RegisterActions`） |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Clock`、`Test.*Time`、`Test.*Scheduler`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### clock

- [clock_test.go](../../../infra/base/clock/clock_test.go)：`TestLogicClockOffsetRoundTripsAtDurationResolution`、`TestLogicClockSetLandsWithinOneMillisecond`、`TestInjectedClocksAreIndependentOfTheGlobalOne`、`TestLogicClockIsSafeForConcurrentUse`、`TestProcessClockKeepsConfiguredSubmillisecondOffset`
- [calendar_test.go](../../../infra/base/clock/calendar_test.go)：`TestZeroCalendarIsUTCMidnight`、`TestPositiveOffsetWithAMorningReset`、`TestNegativeOffset`、`TestDaylightSavingDaysAreShortAndLong`、`TestResetInsideTheDaylightSavingGapOrOverlap`、`TestInvariantsHoldThroughAWholeYear`、`TestDayFormatsAndSplitsItsDate`、`TestParseCalendarRejectsBadValues`、`TestBusinessClocksCarryTheirCalendar`

### timer

- [order_test.go](../../../infra/base/timer/order_test.go)：`TestTimersWithTheSameDeadlineFireInRegistrationOrder`、`TestPriorityOrdersTimersWithTheSameDeadline`、`TestPriorityIsKeptThroughStorageAndRescheduling`、`TestADueTimerWithoutAHandlerIsDroppedWithAWarningAndACount`、`TestStoredTypesWithoutAHandlerAreReportedOncePerType`
- [scheduler_test.go](../../../infra/base/timer/scheduler_test.go)：`TestSchedulerFiresAndPersistsChanges`、`TestSchedulerReschedulesFromHandler`、`TestSchedulerSetClockStampsNewTimers`、`TestRemoveFromAHandlerStopsATimerDueInTheSameTick`、`TestChangeFromAHandlerPostponesATimerDueInTheSameTick`、`TestFiringTimerExplicitRescheduleOverridesReturn`（含 typed 持久节点恢复）、`TestFiringTimerOperationsComposeAcrossReentrantTicks`（改期、取消、重入和 panic 收尾）、`TestOperationsOnEveryNodeStateDuringTick`（操作 × 状态组合表）、`TestHookOperationsOnEveryNodeStateItCanSee`（钩子调用位置的组合表）、`TestOnChangePanicDuringSettlementDoesNotReplay`（收尾时钩子 panic 不重放）；其余入口见文件。基准见 [bench_test.go](../../../infra/base/timer/bench_test.go)（1 万常驻定时器、每 Tick 触发 100 个；World 式重建 + Tick），重构前后对照见 [样本](../../performance/benchmarks/timer-scheduler-refactor-20261017/)。登记快照回归见 [registration_test.go](../../../infra/base/timer/registration_test.go)：回调内创建的 typed/closure timer 保留调用时期限，payload 在调用返回前复制，持久化不受调用方复用缓冲影响；`TestHandlerThatArmsAChildAndReschedulesItselfUsesCallTime` 核对与回调内显式改期叠加时两者都按调用时刻定期限。

### entitytimer

- [driver_test.go](../../../framework/nest/entitytimer/driver_test.go)：`TestDriverDeliversOnlyDueEntitiesOnce`、`TestDriverReportClearsInflightAndRedeliversWhenSilent`、`TestDriverDropsStaleReports`、`TestDriverNeverDeliversToAClosedOwner`、`TestDriverValidatesTheOwnerBeforeDelivering`、`TestDriverRetriesRefusedAdmission`、`TestDriverDeliversSimultaneousDeadlinesInIDOrder`、`TestDriverLoopWakesForAnEarlierDeadline`、`TestDriverConcurrentReportsClosesAndDeliveries`
- [host_test.go](../../../framework/nest/entitytimer/host_test.go)：`TestHostFiresAnOnlineDeadlineOnce`、`TestHostRearmsAPeriodicTimerByItsReturnValue`、`TestHostFiresAnOverdueTimerOnceAfterLoad`、`TestHostDoesNotReuseIDsAcrossReload`、`TestHostIndexesTheEarliestOfSeveralTimers`、`TestHostReportsBadHandlerRegistrationAtInit`
- [nest_test.go](../../../framework/nest/entitytimer/nest_test.go)：`TestHostReportsThroughNestTransactions`
- [wiring/entitytimer_test.go](../../../wiring/entitytimer/entitytimer_test.go)：`TestModBindsNestAndTheBusinessClock`、`TestModRefusesADriverInstalledTwice`

### heartbeat

- [driver_test.go](../../../gameplay/heartbeat/driver_test.go)：`TestTiersFireOnTheirPhaseOncePerPeriod`、`TestInflightBeatSkipsCountsAndWarnsAtALimitedRate`、`TestStalledBeatIsRedeliveredAndRunsOnce`、`TestSetPeriodMovesToTheNewTier`、`TestRegistrationsOfOneEntityAreDeliveredSeparately`、`TestInstanceChecksDropStaleBeats`、`TestStopWaitsForTheBatchAndDispatchesNothingAfter`、`TestConcurrentChangesStepsAndBeats`
- [nest_test.go](../../../gameplay/heartbeat/nest_test.go)：`TestBeatRunsUnderTheEntityLockInsideATransaction`、`TestBeatRunsInTheEntitysPool`、`TestStartStopFollowTheTransaction`、`TestBeatOfAnUnloadedInstanceDoesNotRunOnItsReplacement`、`TestFiftyThousandEntitiesSmoke`
- [remote_test.go](../../../gameplay/heartbeat/remote_test.go)：`TestRemoteHeartbeatFollowsOwnershipWithoutDoubleBeats`
- [wiring/heartbeat_test.go](../../../wiring/heartbeat/heartbeat_test.go)：`TestModBindsNestAndTheBusinessClock`

### movement

- [move_test.go](../../../gameplay/movement/move_test.go)：`TestMoveToAdvancesBySpeedAndElapsedThenArrivesAndStopsTheHeartbeat`、`TestSkippedBeatsAreCoveredByOneElapsedAndFractionsAccumulate`、`TestNewMoveToReplacesTheOldOne`、`TestMoveToStopsWhereTheTerrainBlocks`、`TestFollowTracksTheTargetUntilItIsGone`、`TestEnterAndLeaveTheScene`、`TestRefusedPositionHoldsOnBackpressureAndEndsOtherwise`
- [nest_test.go](../../../gameplay/movement/nest_test.go)：`TestPositionChangesReachTheAOIOnlyWhenCommitted`、`TestEnterAndLeaveFollowTheTransaction`、`TestRolledBackMoveToLeavesNothingRunning`、`TestRunnerIsTickedByTheHeartbeatAndStopsWhenIdle`、`TestMoveToActionRunsOnTheMoveHeartbeatAndIsReplacedByTheNextOne`、`TestFollowActionEndsWhenTheTargetIsUnloaded`
- [shared_test.go](../../../gameplay/movement/shared_test.go)：`TestSharedRemoteEntityRefusesHeartbeatsAndResumesAfterLeavingSharedMode`
- [smoke_test.go](../../../gameplay/movement/smoke_test.go)：`TestSmokeThousandsOfEntitiesMoveOnTheirOwnHeartbeats`
- [example_test.go](../../../gameplay/movement/example_test.go)：`Example`
- [actionflow/heartbeat_test.go](../../../gameplay/actionflow/heartbeat_test.go)：`TestBeatRunnerHeartbeatFollowsTheActionsThatNeedTicks`、`TestBeatTicksTheGroupsInOrderAndWakeTicksAnEventDrivenAction`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。没有新测量不能改容量承诺，性能结论以 [性能报告](../../performance/README.md) 为准。
