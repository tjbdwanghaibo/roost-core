# 时间与定时器：实现与维护

适用版本：v1.29.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）；源码目录最初按 v1.24.0 提取、之后随改动维护，链接由根包文档门禁保证存在，缺漏以源码为准。[设计与使用](../guide/10-time.md)

## 如何阅读

测试时你可能想让活动时间快进，但不希望数据库连接也立刻超时。框架因此把业务时间和系统时间分开。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`infra/base/clock`（业务时钟与业务日历）、`infra/base/fctx`（执行上下文里冻结的业务时间，§2E）、`infra/base/timer`，按实体的持久定时器驱动 `framework/nest/entitytimer`（`wiring/entitytimer` 装配，生成器见 [impl/12-codegen](12-codegen.md#2c-roost-add-timer)），按实体的心跳 `gameplay/heartbeat`（`wiring/heartbeat` 装配，§2B），以及场景时间域 `gameplay/timedomain`（§2F）。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

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
4. **只投递到期的那一个、只投一次**：`DeliverDue` 取出唤醒时刻已到的条目，投递后条目转为“待报告”、唤醒时刻换成 `Redeliver`（缺省 10s）之后；Tick handler 提交时的报告把它换回新的最早期限。没有报告就重投（幂等：Tick 只触发此刻已到期的节点）。准入失败（队列满、停止中）1s 后重试；出锁投递回来按 `gen` 判断期间有没有新报告。投递走 `nest.Client.Dispatch`（正式准入、单目标、没有参数：Tick 按 handler 执行时冻结的业务时间触发，§2E），驱动的更新只持一把短锁、不做 I/O，可以在 Nest 业务池上调用。
5. **到期时核对实例**：`IsRemoved`（Destroy 在锁内先置位、`OnDestroy` 在解锁后）或查到同 ID 的另一个实例时丢弃条目、不投递——驱动不唤醒不在内存的实体。查不到任何实例时照常投递：那是实例刚构建、还没发布（加载中；Nest 冷加载会等这次加载完成）。剩余窗口：投递准入之后、消息执行之前实例被卸载，Nest 按“发给未加载实体的消息”冷加载它，于是加载后补触发一次（与任何发给该实体的消息相同；删档实体加载失败，不触发）。见 KNOWN-LIMITS“按实体的持久定时器”。
6. **离线语义**：加载（`OnInitFinish(isCreate=false)`）记下 `loadedAt`；触发时 `Fire.Overdue = 原定期限不晚于 loadedAt`。Scheduler 对一个期限早已过去的节点只触发一次、按返回值从 now 续期，所以“错过多少个周期都只触发一次”不需要额外代码；框架不计算错过的次数、不循环补触发。期限按毫秒：`Driver.Now` 与 `Host.Tick` / `TickAt` 都截到毫秒，与节点持久精度一致。
7. **时钟**：缺省 `fctx.Now`（D22-1，§2E）：Nest 消息里（Tick、handler 里增删改、加载时的 `OnInitFinish`）是这次执行冻结的业务时间，同一条消息里的期限都从同一时刻算；驱动循环不在消息里，现读进程业务时钟。`wiring/entitytimer` 不再 `SetClock`（进程业务时钟由 App 启动时按同一份 `time.logic_offset` 设定）。测试用 `SetClock` 换可控时钟（之后驱动与 `Host` 一律读它）、不启动循环直接调 `DeliverDue`。`Host.Tick()` 用驱动时钟，`TickAt(now)` 给测试与手写 handler。

验证：`framework/nest/entitytimer` 的 `driver_test.go`（只投到期的、待报告不重投、静默重投、旧报告丢弃、关闭后不投、核对实例、准入失败重试、同时刻按 ID、循环被更早期限叫醒、`-race` 下加载 / 卸载 / 期限变化 / 投递并发）、`host_test.go`（在线一次、周期续期、离线错过十个周期加载后只触发一次且参数正确、重建不复用 ID、多定时器最早期限、登记错误在初始化报出；回调内登记下一阶段、取消 / 改期同轮到期的另一阶段、显式改期自己、回调读到自己的写入、panic 清理、回滚后重试）、`nest_test.go`（真实 Nest 引擎：回滚不改索引、提交后才改、投递的 Tick 在锁内执行并清掉待报告）；变异核对（提交前立即报告、接受旧报告、不核对实例、关闭后仍接受报告、`Overdue` 恒假）各让对应用例变红，记录在 [D11 实施说明](../../maintenance/DESIGN-TODO.md#d11-按实体的持久定时器接入)。生成工程端到端见 `codegen/internal/roost/add_timer_test.go` 的 `TestEntityTimerGeneratedProjectEndToEnd`。

## 2B. 实体心跳

DESIGN-TODO D20 第 1 期（维护者 2026-10-21 定方案与压测后决定）。`gameplay/heartbeat` 三个类型：`Driver`（一个 Nest 引擎一个，业务包级变量，`wiring/heartbeat` 装配）、`Host`（每个实体实例一个）、`Reg`（一个登记）。依赖边界：只 import `framework/nest`、`framework/entity` 与 infra，根包门禁把它归入 nest 块（`dependency_boundary_test.go`）。

1. **调度结构**（`tier.go`）：按档分环，`Resolution` 10ms；10 个固定档位 `Periods`，`tierOf` 拒绝其他周期（`ErrUnsupportedPeriod`）。每档一个环、环长 = 该档格数，登记只进一格 `cellOf = hash % 6000 % 本档格数`（`hash = misc.Hash64(实体 ID)`，各档按同一相位对齐），格是切片、登记记下标，撤销 / 换档 O(1)。原型与压测见 [HB-STRUCTURES-20261021](../../performance/HB-STRUCTURES-20261021.md)。环只在 `Driver.mu` 下读写。
2. **一个驱动 goroutine**（`driver.go` `loop`）：10ms ticker，按单调时间追到应到的格逐格 `step`；每 100 格 `flush` 一次。`step` 在 `mu` 下取出本格到期的登记，出锁逐个核对后交给投递层：实例已 `OnDestroy` / `IsRemoved` / 查到同 ID 的另一个实例 / 查不到（未发布）→ 丢弃；Remote 托管实例的所有权状态不是 `LocalOwned` / `Unknown` / `Recovering`（`mayOwn`）→ 丢弃；在途 → 跳拍。核对不阻塞、不持锁。
3. **投递层只换一处**：`deliverer.deliverBatch(ctx, []beat, rejected)` 一次交出一格的一批。投递方式 `Config.Delivery` / `SetDelivery`（`wiring/heartbeat` 的配置项 `heartbeat.delivery`，Init 时交给驱动；第一次 `Start` 之后不能改，维护者 2026-10-21 决定批量方式先合入、缺省关闭）：缺省 `DeliveryDispatch` 用 `dispatchDeliverer` 逐个 `nest.Client.Dispatch`；`DeliveryBatch` 用 `batchDeliverer`（要求客户端实现 `nest.TryBatchDispatcher`，否则 `Bind` / `SetDelivery` 报错、不静默退回）：每 `beatBatchSize`（64）拍一个 Nest 批量作业（D20 选项 C，[设计](../../maintenance/FEATURE-2026-10-21-nest-batch-heartbeat.md)），每拍仍在实体自己的 Guard 作用域与事务里执行；同 ID 有已准入未完成的消息时 Nest 当场把这一拍转成单目标作业排在它后面（同一格内执行，不跳拍）；Remote 托管实体的拍逐个单目标 `Dispatch`（写批次要在 I/O 池准备）。参数是预先建好的 `nest.Params{*Reg}`（Nest 准入时复制，可复用）。批量方式下每个目标开始前 `fctx.Context.Refreeze`，`Beat.Now`（§2E）与时间域的域时间（§2F）按拍各冻结一次，与单目标方式相同。批量作业准入之后才发现不能执行的拍经 `Driver.handBack` 进交回队列，驱动下一格开头 `drainReturned`：不在内存计 `dropped{not_loaded}`，其余按跳拍计（`skip`）。
4. **至多一拍在途**：`Reg.inflight` 在投递前置位、在心跳 handler 返回时清掉（`defer`，panic 也清）；到期时仍在途就跳拍（`skipped` 计数交给下一拍的 `Beat.Skipped`）。准入失败（`ErrQueueFull` / 其他，含批量作业里冲突转单目标被拒）清在途、计跳拍，不在本格重试；冲突转成的单目标作业仍算在途，执行完才清；驱动停止时批里还没投的拍不再投、清在途（`deliverBatch` 看 `ctx`），`Stop` 返回后不再 `Dispatch`。没有水位让路与每格上限（维护者决定 ⑤）。
5. **卡住兜底**：在途超过 `StallAfter`（缺省 10s）视为丢失、重投并计 `stalled`；同一登记连续卡住时阈值翻倍（最多 3 次，80s），看到不在途时复位。丢失只来自 Nest 在执行 handler 之前放弃了消息（Remote 写准入被拒、准入后卸载且重载失败）。重投后两份消息至多一份执行回调：handler 先看在途标志，没有在途按 `duplicate` 丢弃。翻倍是因为拍没丢只是排得久时重投会多压消息（2s 不翻倍时 -race 冒烟里重投与积压互相放大，见 D20 实施记录）。
6. **事务内启停**（`host.go` `change`）：`Reg` 的 `active` / `tier` 是业务视角、只在实体锁内读写；变化时序号加一，在事务里登记 `DeferRollback`（恢复业务视角）与 `AfterCommit`（驱动生效），不在事务里立即生效。`Driver.apply` 只接受更大的序号（AfterCommit 在锁释放后执行、pipelined 下可能晚于后一个事务），实例 `OnDestroy` 后忽略。启动生效时先写 `startedAt` 再加 `startGen`，回调据此判 `First`。
7. **心跳 handler**（`runBeat`，包级注册 `heartbeat.beat.{memory,async,strict,pipelined}`，回滚策略 undo，持久级别由登记选）：依次核对在途、实例（`es[0]` 必须是登记所属实例且未 `OnDestroy`，卸载重载窗口里旧实例的拍按 `stale` 丢弃）、业务视角的启停（锁序上更早提交的 Stop）、Remote 实例是 `LocalOwned`；通过后算 `Beat`：`Now` 读一次驱动时钟（缺省 `fctx.Now`，即这一拍消息冻结的业务时间，§2E）；挂了时间域时改读 `Domain.Read()`（域时间与倍率同一次冻结，§2F）。`Elapsed` 从上次执行（首拍从 `startedAt`，挂域时 `startedAt` 也是域时间：`Reg.startClock`）起算，夹到 `[0, max(DefaultMaxElapsed, 档位)]`，挂域时上限再乘这一拍的倍率（时停为 0）。回调侧的 `lastRun` / `lastGen` 随本拍事务回滚恢复（`DeferRollback`），回滚的一拍不算执行过。
8. **Remote 托管实体**：发给 Remote 托管 ID 的消息由 Nest 自动走 I/O 阶段的 Remote 写准入（`prepareRemoteWriteBatch`）：非拥有方被拒、`Unknown` 转 `LocalOwned`。旧拥有方转移时先排空已准入的写、实例转 `Draining` 再改权威，所以它最后一拍先于新拥有方的第一拍；驱动侧 `mayOwn` 与 handler 侧 `LocalOwned` 核对让非拥有方不投，并避免非拥有期间被拒的拍卡住新拥有方（变异核对 M6）。
9. **Remote 共享模式不允许挂载心跳**（维护者 2026-10-21，D20 P1 后决定 ④）：共享是运行期所有权状态（`remoteentity.Manager.EnterRemoteSharedMode` / `LeaveRemoteSharedMode` 按实体 ID 切换；实例加载时是 `Unknown`，共享权威下第一次写准入经 `newWriteLease` 的 marked 分支转 `Shared`）；`entity.RemotePolicy` 只有 None / Managed / Mirror，没有种类级的“共享”声明，登记时只能看到当时的状态。强制点三处：① `Host.Register` 与 `Reg.Start` 在实例处于 `Sharing` / `Shared` 时返回包裹 `ErrHeartbeatRemoteShared` 的错误（`host.go` `sharedErr`）；② 驱动 `mayOwn` 对 `Sharing` / `Shared` 照常投一拍（只为了进到实体锁内：业务视角的 `Active` 只在锁内改，不由驱动 goroutine 改）；③ `runBeat` 在 `Active` 核对之后、`LocalOwned` 核对之前遇到共享模式就 `change(false)` 停掉登记（随本拍事务 AfterCommit 生效；提交失败则下一拍再停），计 `stoppedShared`，不执行回调；驱动 `flush` 汇总增量、按 `WarnInterval` 限频打 Warn（`emitSharedWarn`，带间隔内停掉的个数、样本实体与登记名）。每个登记在共享模式下至多多投这一拍（共享模式写准入，取一次分布式锁），停掉后移出环、不再投。回到独占后不自动重启。检测点：Remote 没有所有权状态切换的回调（`entity.RemoteEntityBase.TransitionRemoteOwnership` 只做 CAS），心跳也不应依赖 `framework/remoteentity` 的内部实现，所以在心跳自己的两处读实例的所有权状态（`IThreadSafeRemoteEntity.RemoteOwnershipState`，只依赖 `framework/entity` 接口）：驱动到期时（原子读）与 handler 执行时（实体锁内）。保证：共享模式下任何一拍都不执行回调（handler 在锁内核对）；每个在共享期间到期的已启动登记都被停掉。限制：共享窗口短于登记的周期、期间没有到期的拍时，这个登记不会被停掉（期间也没有执行），回到独占后照常跳。变异核对：去掉 ① 的 Register 核对、去掉 ① 的 Start 核对、去掉 ② 的共享投递、去掉 ③ 的停止、Warn 不限频，各让 `remote_test.go` 对应用例变红。
9. **计数与日志**：驱动内部按实体 ID 分片的原子计数（回调侧由全部 worker 并发写），`flush` 每秒把增量写进 metrics 注册表；跳拍 / 卡住汇总进 `warnState`，按 `WarnInterval`（缺省 10s）打一条 Warn，带一个样本实体与原因。
10. **不改 Nest**：只用 `Client.Dispatch`、`HandlerMeta`、`CurrentRollbackTx` / `AfterCommit` / `DeferRollback`；entitytimer 不改。

验证（`GOWORK=off go test -race -count=1 ./gameplay/heartbeat ./wiring/heartbeat`；`driver_test.go`、`nest_test.go`、`remote_test.go` 的用例在 `dispatch` / `batch` 两种投递方式下各跑一遍（`forEachDelivery`），批量方式特有的三个用例 `TestConflictingBeatRunsRightAfterTheAdmittedMessage`、`TestConflictingBeatIsSkippedOnlyWhenAdmissionRefuses`、`TestBeatOfAnUnloadedInstanceIsDroppedWithoutColdLoad` 只在 `batch` 下跑；开关本身 `TestDeliveryIsChosenOnceBeforeStart`、`wiring/heartbeat` 的 `TestModReadsTheDeliveryOnce`；两种方式下每拍各冻结一次业务时间与域时间 `TestEveryBeatFreezesItsOwnBusinessTime`）：`driver_test.go`（档位规则与相位打散、在途跳拍 / 准入满 / 限频 Warn、卡住重投翻倍与只执行一次、换档、多登记不合并与按登记选 handler、投递前与执行时的实例核对、Stop 顺序、并发启停 / 推进 / 执行）；`nest_test.go`（真实 Nest：回调在实体锁与事务内、与同实体业务消息串行；慢池实体在慢池；事务回滚作废启停、回调报错回滚回调侧记录；卸载重载令牌；5 万实体冒烟，-race 下 2000 实体）；`remote_test.go`（两个引擎 + 模拟 Remote 写准入的所有权迁移：旧拥有方离开 `LocalOwned` 后零执行、新拥有方成为拥有方前零执行；共享模式下登记被拒、运行期进入共享（拥有方切换与第二个写者首拍准入）后心跳被停掉且回调零执行、多个登记全部停掉且 Warn 限频、共享期间 `Start` 被拒、回到独占后不自动恢复且重新 `Start` 照常跳）；`wiring/heartbeat/heartbeat_test.go`（依赖 Nest、业务时钟、登记驱动、启停）。变异核对 M1～M11 各让对应用例变红，原文记录在 [D20 实施结果](../../maintenance/DESIGN-TODO.md#d20-移动由实体自己的心跳驱动mapbasic--move-组件--actionflow-控制)。

## 2C. 移动与动作的心跳驱动

DESIGN-TODO D20 第 2 期。`gameplay/actionflow` 的 `BeatRunner` 与 `gameplay/movement`（`MapBasic`、`Move`、`MoveTo` / `Follow` 动作），都建在 §2B 的心跳上，只用它的公共 API（`Host.Register`、`Reg.Start` / `Stop` / `SetPeriod` / `Active` / `Period`），不改投递层。依赖边界：`actionflow` 只多 import `gameplay/heartbeat`（同属 nest 块）；`movement` 建在三块之上（import nest、entity、`sync/entitysync/policy`、actionflow、heartbeat），不归入任何一块（`dependency_boundary_test.go`）。维护者 2026-10-21 的两条决定：移动由动作驱动（参考 ssr）、`Move` 被动；所有控制实体行为的逻辑都经 `ActionRunner`。

1. **只在实体锁内、只做内存与入队**：方法都在 Nest handler、心跳回调或加载 / 新建的初始化阶段调用（`Published` 除外）。心跳回调里只做内存计算与入队（写 DAO 字段、`QueueMove`、记提交后工作），不阻塞、不做 I/O、不 `Dispatch` / `Request`。
2. **每组一个心跳登记**（`actionflow/heartbeat.go`，对照 ssr `action_list.setCurAction`）：包住业务的 `OnTransition`；组第一次有动作时登记 `"<Name>/<组号>"`（停止状态，之后保留），当前动作进入时按 `intervalOf`（未实现 `TickIntervaler` 取 `BeatConfig.Period`，0 不挂，正数取不大于它的最长档）`SetPeriod` + `Start`，离开时 `Stop`。启停走心跳的事务语义（提交后生效、回滚作废）。一拍的回调只 `runner.Tick(本组)`；回调里先核对本组仍有需要周期 tick 的当前动作（锁序上更早提交的切换可能已让它空闲）。包住 `PopulateContext`（旧的 `Context` 钩子同样经它）补 `Owner`（取宿主实体）与 `Elapsed`（只在本组的一拍 tick 期间非零）。强制点：`transition`、`beat`、`populate`（变异 M2、M10、M11）。
3. **挂不上心跳的动作以失败结束**：`Start` 返回错误（Remote 共享模式 `ErrHeartbeatRemoteShared`）时经 `OnError` 报告，并 `runner.End(组, force, 原因)`——在切换回调里调用，按 ActionRunner 规则延后到回调返回后执行，动作的 `Cancel` 照常调用（移动动作停掉自己的移动）。运行中被框架停掉（下一拍在实体锁内 Stop）之后组件不感知：动作留在 runner 里不推进，`Sync` 重新 `Start` 需要周期 tick 而没在跳的组，返回挂不上的错误。强制点：`transition`、`Sync`（M17、M18）。
4. **runner 不随事务回滚（业务约束，框架不处理，维护者 2026-10-21）**：runner 的变更立即生效、没有回滚登记；handler 先做完会失败的校验与修改，最后一步才调用动作接口。拍的回调永远返回 nil（runner 的错误经 `OnError`），所以拍的事务只在提交失败时回滚，那时 runner 已按 Tick 的结论推进；动作在 Tick 里对组件的修改随这一拍回滚（DAO 的逆操作、`Move` 的 `guard`）。评估过“Tick 返回错误就不推进 runner”：`Action.Tick` 没有错误返回、拍回调也不返回错误，能触发回滚的只有提交失败，发生在 runner 推进之后，要做到一致只能把 runner 的推进挪到提交后，维护者决定不做。actionflow 包说明、guide §4B 写明这条约束。
5. **Move 被动**（`move.go`）：`MoveTo` / `Follow` 只设定 `moveState` 并发新编号（`nextToken` 只增、不回滚）；`Advance(token, elapsed)` 是唯一推进入口：编号不是进行中的移动 → `Replaced`、不改任何东西；否则 `guard`、核对场景、按 `速度 × elapsed` 沿路径 / 朝跟随目标推进，结论非 0 时清状态。不持有心跳、没有回调。强制点：`Advance`（M1、M4）。
6. **位置的权威是 DAO**（`map_basic.go` `Fields`）：组件不另存位置。`SetPos` 先 `Space.QueueMove`、成功才写字段与登记回滚；`QueueMove` 由 `policy.Interest` 绑定当前 `SyncMutation` 的提交条件，提交确认后应用、回滚丢弃，所以“回滚的移动不生效”由 Interest 的提交门保证，组件只负责“被拒时什么都不写”。强制点：`SetPos`（M6）。
7. **进出场景在提交之后**（`addOp` / `stagedCommit` / `afterCommit`）：一个事务里的进出与要发布的快照攒进一条按**事务 ID**识别的记录，第一次改动时登记 `nest.AfterCommit`；提交后在实体锁外按顺序调 `Space.Enter` / `Leave`，再发布快照。之后的事务各有自己的记录，提交后回调只读自己那条（锁外读组件的其他状态会与下一个事务竞争）。同一事务先进后出抵消；进场景后又 `SetPos` 时不入队（AOI 还不认识它），直接改记录里 Enter 的位置。回滚时记录作废，`mapState` 由 `guard` 登记的 `DeferRollback` 恢复。不在事务里立即执行。带 `SaveEvery` 的 `Enter` 先 `Start` `"map.save"`，挂不上就原样返回错误。强制点：`addOp`、`SetPos` 的 `entering` 分支、`guard`、`Enter`（M5、M7、M14、M16）。
8. **发布按序号**（`publish`）：快照带序号，CAS 只接受更大的；`OnDestroy` 用最大序号封住。回滚的改动从不发布；发布在提交后、实体锁释放之后，晚于同一拍事务里的事件（测试用同实体的下一条消息等它收尾：同实体消息要等前驱作业整个结束，含提交后回调）。
9. **一次 Advance 只按 elapsed 走一次**：位置变化只在结束时写一次（`commitStep`：一拍走三格只入队一次）；亚格位置 `x, y` 累积零头，`cell` 记上一次写进 `MapBasic` 的格，`MapBasic` 的位置被别处改过（瞬移）时从新位置接着走。AOI 排队满整拍回到 `beatStart`、返回“还在移动”；其他 `SetPos` 错误 → `Refused`（M3、M13）。
10. **跟随读提交后的快照**（`follow` + `LocateIn`）：`Locator` 在跟随者的锁内调用，只经实体管理器的只读查找拿到目标的 `MapBasic`、读原子快照，不取目标的锁。查不到 / `Present` 为假 → `TargetLost`，场景不同 → `TargetLeft`（M9）。
11. **移动动作**（`action.go`）：`TickInterval` 取参数 `Period`（缺省 100ms）；`Start` 要求 `ctx.Owner` 实现 `MoveOwner`，设定移动并记下编号；`Tick` 调 `Advance(编号, ctx.Elapsed)`，结论 0 继续、`Arrived` 成功、`Replaced` 取消、其余失败；`Cancel` `Stop(编号)`（M12、M19）。

验证（`GOWORK=off go test -race -count=1 ./gameplay/movement ./gameplay/actionflow`）：`move_test.go`（直接调 Advance：按速度与 Elapsed 推进到达、跳拍一次覆盖与亚格累积、取代、被阻挡、跟随 / 目标销毁 / 目标离开、进出场景与落库、背压与拒绝）；`nest_test.go`（真实 Nest + 同步 Manager + Interest + 启动的心跳驱动：位置经 QueueMove 进 AOI、回滚的移动不生效；进出场景随事务；回滚的 `Move.MoveTo` 不生效；runner 由组的心跳 tick 且空闲停；MoveTo 动作由 runner 心跳驱动、移动中只有一个登记、被新的取代；Follow 在目标卸载后收尾）；`shared_test.go`（Remote 共享模式：进场景被拒、启动的移动以失败结束并停掉移动、被停掉后 Sync 在共享期报错、回到独占后重新挂上）；`smoke_test.go`（3000 实体随机 MoveTo，观察者经同步帧收到每个实体的终点位置，-race 下 300）；`actionflow/heartbeat_test.go`（每组按当前动作的周期挂心跳、一拍只 tick 本组并带 Elapsed）。变异核对 M1～M19 原文见 [D20 P2 实施结果](../../maintenance/DESIGN-TODO.md#p2-实施结果2026-10-21)。

## 2D. 行为控制：Behavior、取代、冲突、冻结与状态

DESIGN-TODO D21（维护者 2026-10-21 决定调研一 13 条全部按推荐）。全部建在 §2C 的 `ActionRunner` / `BeatRunner` 上，方法只在实体锁内调用、只做内存计算与入队；runner 与 Behavior 不随事务回滚（§2C 第 4 条的业务约束不变）。codebase-memory 图的根是主检出、generation 早于本轮，D21 的文件不在图里，下列结构以 worktree 源码为准。

1. **Start 即完成**（`runtime.go` `Action.Start` 返回 `(done, result, err)`；`action_runner.go` `start`）：`err` 非空走原失败路径（Cancel → 离开切换 → 失败的 OnEnded）；`done` 为真时不调 Cancel，直接 `finish(unit, false, 结果, startNext=true)`——与 Tick 报告结束同一条收尾（结果状态为空记成功）。`BeatRunner` 在进入切换时照常 Start 心跳、离开时 Stop，事务里两次变化按序号生效，净效果是没挂上。`movement` 的动作在 `Start` 末尾调 `Move.Advance(token, 0)`，为此 `walkPath` 改为与当前位置重合的途经点不耗距离、0 时长也越过（原来 `budget > 0` 才进循环）。强制点：`start` 的 `done` 分支、`moveAction.Start`、`walkPath` 的循环条件（变异 D1、D12）。
2. **取代否决与跨组冲突**（`applyStart`）：判定顺序固定为 冻结 → `crossGroup`（其他组按组号升序问 `Conflict`，任一 `BlockIncoming` 立即拒绝、什么都不改）→ 本组当前动作的 `Replacer`（Keep：新动作以取消结束、返回 nil；Reject：拒绝）→ `preempt`（取消被抢占组的当前动作，`startNext=false`）→ 取代本组当前动作 → `start`。被拒绝的新动作：最外层调用返回错误、不发 OnEnded（调用方没拿到 ID，与冻结一致）；延后执行的（ID 已交出）发一次失败状态的 OnEnded。排队项成为当前动作（`applyAdvance`）同样过 `crossGroup`，被挡时以失败结束、经 OnError 报告、接着试下一个；此时同组空闲，不问 `Replacer`。`ReplaceBy` / `Conflict` 的 panic 分别按 Allow / NoConflict 处理并报告。强制点：`applyStart`、`crossGroup`、`preempt`、`replaceDecision`（D2、D3、D4）。
3. **按 ID 结束**（`applyEndAction`）：先找各组当前动作（`finish`，推进队列），再找排队项（`discardQueued` 只丢这一个，取消状态）；最外层调用找不到返回 `ErrActionNotFound`，延后命令找不到返回 nil（动作可能已在这之前结束）。强制点（D5）。
4. **计数冻结**（`actionGroupState.freezes`）：`Freeze` 立即加一（0→1 时调 `OnFreeze(group, true)`），`FreezeCancel` 再提交一条 `opInterrupt`（有回调，按 submit 规则延后），执行时组仍冻结且有当前动作才 `finish(cancel, startNext=false)`，原因 `action group frozen` / `ErrActionGroupFrozen`；`Recover` 是命令（`opRecover`）：计数为 0 返回 `ErrActionGroupNotFrozen`、不变负，减到 0 时 `OnFreeze(group, false)` 并在组空闲时推进队列。所有原来读 `frozen` 的地方改读 `freezes > 0`（Start 预检、Enqueue、Tick、finish 的推进、advance）。`BeatRunner.freeze` 包住 `OnFreeze`：冻结 `Stop` 组的心跳，解冻时当前动作需要周期 tick 就 `startGroup`（挂不上同 `transition`，以失败结束）；`transition` 进入与 `Sync` 都跳过冻结的组。强制点：`Freeze`、`opRecover`、`opInterrupt`、`BeatRunner.freeze`（D6、D7、D8）。
5. **Behavior 的接线**（`behavior.go`）：`ActionRunnerHooks` 只用三个——`PopulateContext` 填 Owner / ActionList（Behavior 自己）/ CurrentMission，`OnEnded` 先 `MissionRunner.OnActionEnd` 再 `Observer.OnActionEnd`，`OnError`；`MissionRunnerHooks` 的 `ClearActions` 接 `ActionRunner.ClearMission(id, cancel=true)`、`OnEnded` 接 `Observer.OnMissionEnd`、`Context` 给任务同一个 ActionList。动作的归属任务由 `missionTag` 决定：`MissionRunner.Deferring()`（任务 runner 正在执行，即在任务回调或它同步引出的回调里）时取当前任务 ID，否则 0。`Clear` 先 `EndCurMission` 再 `EndAll(force)`。`Beat` 非空时用 `NewBeatRunner`（宿主必须属于同一实体，`NewBehavior` 检查）。`ai.Controller` 有编译期断言实现 `actionflow.Observer`。强制点：`actionEnded`、`missionTag`、`Clear`（D9、D10、D11）。

6. **状态位**（`gameplay/state`，第 2 期）：`StateDao.sources` 是 `map[ID][]Source`，切片按 (Kind, ID) 升序、不重复、从不原地修改（`change` 总是换新切片），所以逆操作只浅拷贝 map。`change` 是唯一写入口：没有变化直接返回；事务外改 `Persist` 状态返回 `ErrNotInTransaction`；`rollback=undo` 下每个事务第一次改动前 `RecordUndo(dao, FieldStates)` 恢复整个来源集合，`rollback=state` 下靠 DAO 快照（`CaptureRollbackState` 含不持久的状态）；改完 `Persist` 的 `MarkPersist(FieldStates)`、`Sync` 的 `MarkSync(FieldStates)`。持久文档只含 `Persist` 状态，键是状态名；`RestorePersisted` 跳过不认识的名字与不再持久的状态（下次整文档写回时消失，同 D13），版本不等于 1 拒绝。派生状态不进 DAO。强制点：`change`、`snapshot(all)`、`apply`（变异 S2～S7）。
7. **按差通知**（`Set.notify` / `diff`）：`seen` 记每个被观察状态上次交出的值（`//roost:cache`，不随回滚）；每次改动之后按编号升序比较全部被观察状态的当前值与 `seen`，不同的记下新值并按注册顺序调用观察者（分发前快照观察者列表）。`notifying` 防止嵌套：观察者里的改动立即生效，只由外层循环在下一轮补发；超过 64 轮报告 `ErrNotifyRunaway`。回滚不触发通知（回滚期间不能再改别的组件），之后任何一次改动都比较“全部”被观察状态，所以回滚造成的差在下一次通知时补上；`Refresh` 就是一次不带改动的通知。强制点：`diff` 的比较、`notifying`、`Refresh`（S1、S9、S11）。
8. **门控**（`Gate` / `Gated.align`）：每条规则一个 `applied`（`//roost:cache`）：想要的值 = `Has(State)`，想要且未持有时 `Freeze(Group, Mode)`、不想要且持有时 `Recover(Group)`，所以每条规则至多持有一次冻结，回滚造成的不一致下一次 `align` 纠正、不累积。装上时 `align` 一次；`Close` 撤销观察并撤销持有的冻结。`Freezer` 是 `Freeze(group, mode) error` / `Recover(group) error`，`actionflow.Behavior` 实现它。`state` import `actionflow`，反向不 import。强制点：`align`、`Gate` 末尾的对齐、`Close`（S8、S10、S12）。

9. **Buff → 状态**（`combatcomponent/state_bridge.go`，第 3 期）：`BindStates` 对每个绑定 `Set.Provide(State, 读标签)`，提供函数每次都经 `component.dao.buffs` 读当前容器——回滚会换掉容器实例（`CombatDao.beginChange` 的逆操作重建容器），不能缓存（变异 B3）。组件里的接线是一个函数字段 `buffStatesChanged`（构造时安装，不是需要回滚的状态），在 `ApplyBuff`（真的施加了时）、`RemoveBuff`、`SetBuffStacks`、`AdoptBuff`、`DispelBuffs`、`TickBuffs`（有变化时）与 `OnInitFinish` 末尾调 `Set.Changed(全部绑定状态)`；`SetBuffDueTick` 不改集合、不通知。依赖方向：`combatcomponent` import `state`，`state` 不认识 combat。强制点：各 mutator 的通知、`OnInitFinish`、提供函数读当前容器（B1～B5）。

10. **任务心跳**（`behavior.go` `syncMissionBeat` / `missionBeat`，第 4 期）：`MissionRunnerHooks` 的 `OnState` 与 `OnChanged` 都接 `syncMissionBeat`——任务开始前后、每次推进（`update`）、结束时各对齐一次：当前任务在运行且 `missionPeriod`（`TickIntervaler` 经 `heartbeat.PeriodAtMost` 取档，不实现为 0）大于 0 时按它 `SetPeriod` + `Start`，否则 `Stop`；登记 `"<Name>/mission"` 第一次需要时才创建。`OnState(true)` 在任务 `Start` 之前触发，那时计划还没装上，所以真正的对齐靠 `Start` 之后的 `OnChanged`。挂不上时经 `OnError` 报告并 `EndCurMission`（在任务 runner 的回调里，延后执行）。回调只 `MissionRunner.Tick(beat.Now)`。强制点：`syncMissionBeat` 的启停（变异 H1、H5）。
11. **计划步骤的时限与重试**（`plan.go`）：`launch` 是一次尝试——创建动作，`Timeout > 0` 时记 `deadline = ctx.Now + Timeout`（每次尝试重新计时）；`startStep` 进入新步时 `attempts` 清零再 `launch`。`Tick(now)`：在运行、有当前动作、有时限且到时，就清掉时限并 `ActionList.EndAction(动作, force, 过期原因)`——结束在任务自己的回调里发生，结论经延后送达的 `OnActionEnd` 回来。`OnActionEnd`：结果为 `Failed` / `Expired` 且 `attempts < Retry` 时 `attempts++` 原地 `launch`，否则按原规则 `apply`（OnSuccess / OnFail）；`Canceled` 不重试。`TickInterval` 只在当前步有时限时返回 `Period`（缺省 `DefaultMissionPeriod`）。`NormalizePlan` 拒绝负的 `Timeout` / `Retry` / `Period`。强制点：`Tick`、`retryable`、`startStep` 的清零、`launch` 的计时（H2、H3、H4、H6）。
12. **AI 心跳与定时冻结**（`ai/beat.go`、`controller.go`）：`NewBeatController` 只多一个 `controllerBeat`；`syncBeat` 在 `SetStrategy`（真正切换之后，含回调里延后的切换）、`Shutdown`、`Freeze`、`FreezeFor`、`Recover` 与每一拍之后调用：`beatInterval` = 没有策略或无定时冻结时 0，否则策略的 `TickInterval`（不实现取 `BeatConfig.Period`）经 `PeriodAtMost` 取档；0 停，正数 `SetPeriod` + `Start`，登记 `"ai"` 第一次需要时才创建，挂不上经 `OnError` 报告。`Tick(now)` 先处理定时冻结：`frozen` 且 `frozenUntil` 非零且已到，就 `Recover` 再照常决策。`FreezeFor(d, interrupt)` 置 `frozenUntil = Now 钩子 + d`，`interrupt` 时调 `EndActions`。强制点：`beatInterval` 的冻结判断、`Tick` 的到期恢复、`onBeat` 末尾的对齐、`FreezeFor` 的打断（H7～H12）。

13. **AI 事件**（`controller.go` `OnEvent`、`behavior_strategy.go`）：`OnEvent` 用 `ready()`（有策略、不在切换、未冻结）判断能否送达，送达是一次策略回调（`enterCallback` / `leaveCallback`，回调里的切换照常延后），panic 恢复成错误。`BehaviorStrategy.OnEvent` 追加到 `events`，满 `MaxEvents` 时先删最早的；`Tick` 把 `events` 交给本拍的 `BehaviorContext` 并清空，本拍没被取走的随上下文丢弃。`EventReceived` 经接口 `eventSource` 读上下文（不是 `BehaviorContext` 时失败）；不取走的版本无状态，可以进守卫谓词（`validatePredicate` 只放行 `take` 为假的 `event`）。强制点：`OnEvent` 的 `ready`、`MaxEvents` 截断、谓词校验（变异 E1、E2、E6）。
14. **分阶段**（`BehaviorStrategy` 的 `stages` / `current` / `requested`）：`BehaviorContext` 带一个 `behaviorHost`（策略自己），树里的 `SetStage` 只经 `requestStage` 记下请求；`Tick` 里 `ticking` 为真，根节点求值完（含完成后的 Reset 与 `OnResult`）才 `applyStage`。策略外的 `SetStage` 在不 `ticking` 时立即 `applyStage`。`applyStage`：目标就是当前阶段时什么都不做，否则 Reset 旧阶段的根（`TaskflowAction.Reset` 调 `OnInterrupt`）、换 `current`、调 `OnStage`。`Init` Reset 全部阶段并回到起始阶段，`Stop` Reset 当前阶段。`ParseStages` 按阶段名排序解析（诊断确定），解析器记下每个 `set_stage` 的路径与目标，全部解析完再核对目标存在；`ParseTree` 遇到 `set_stage` 拒绝。`set_stage` / `event` 在解析时检查上下文类型 `*C` 实现了 `stageSwitcher` / `eventSource`。强制点：`requestStage` 与 `Tick` 末尾的 `applyStage`、`applyStage` 的 Reset、`ParseStages` 的目标核对（E3、E4、E5）。
15. **轨迹与指标**：`Registry.TraceNodes` 打开后解析器的 `node` 用 `Traced{Path, Child}` 包住每个节点，`Traced.Tick` 在子节点之后经 `tracer` 接口调策略的 `Trace`（没有钩子时只多一次接口判断）。`BehaviorConfig.Metrics` 为真时 Behavior 在 `OnTransition` 记 `started_total{kind}` 与 `running` ±1，在 `OnEnded` 记 `ended_total{kind,status}`（状态取 `ToActionResult(Success)`）再走原来的接线；`BeatRunner` 包住的 `OnTransition` 照常调用它。指标名登记在 OBSERVABILITY（根包门禁）。强制点：`Traced.Tick`、`countTransition`（E7、E8）。

验证（`GOWORK=off go test -race -count=1 ./gameplay/actionflow ./gameplay/movement`）：`action_runner_test.go`（Start 即完成并推进队列、延后的瞬时动作；Keep / Reject 与延后的 Reject、强制结束不问；Block / Preempt 与被挡的排队项、无规则互不影响；按 ID 结束当前与排队项、找不到；计数冻结、多撤销一次、暂停不 tick、打断保留队列、OnFreeze 只在 0↔1；回调里的打断延后）；`heartbeat_test.go`（冻结的组停心跳、迟到的拍不 tick、`Sync` 不重挂冻结的组、解冻重挂、打断后不再挂）；`behavior_test.go`（先任务后观察者、任务标签与随任务结束、Clear 后什么都不运行、经 ActionList 的计数冻结、拒绝别的实体的心跳宿主）；`movement/nest_test.go`（真实 Nest + 心跳：已经结束的移动在 handler 里结束且不挂心跳；暂停移动组时驱动上没有登记、位置不变，解冻后走到终点，打断模式取消在途移动）。`state/set_test.go`（声明校验、按来源不叠加与 RemoveSource、派生现算与只在变化时通知、观察者里的改动排下一轮与截停、查询与观察者包、事务外改持久状态被拒、持久文档只含 Persist 状态并跳过不认识 / 不再持久的）；`state/nest_test.go`（真实 Nest + 心跳：两种回滚策略下状态随回滚撤销、只有 Persist 状态进提交记录且 memory 持久级别下改 Persist 状态被拒、Sync 标同步脏；门控经 Behavior 打断移动组并停心跳、两个来源全撤才解冻、两个状态计数冻结施法组；回滚后的门控在下一次改动或 Refresh 时纠正且计数不累积；装上时对齐、Close 撤销）。涉及心跳的用例 `-race -count=30` 通过。`skill/combatcomponent/state_bridge_test.go`（真实 Nest：buff 标签驱动派生状态、不复制进 StateDao，回滚的 buff 当场不再生效、观察者在下一次通知跟上，到期与驱散通知，同步位；绑定的校验；OnInitFinish 通知加载出的 buff）。`actionflow/behavior_test.go`（第 4 期：时限到期以过期结束、重试重新计时、用完走 OnFail 且心跳停；心跳只跟有时限的步；取消不重试；重试按步计）；`actionflow/nest_test.go`（真实 Nest + 心跳：计划步骤的时限与重试经任务心跳生效，结束后没有登记）；`ai/beat_test.go`（周期按策略取档、冻结 / 无策略 / 策略要 0 时停、每拍后重新对齐、Shutdown 停；定时冻结期间照跳不决策、到期那拍恢复并决策、可选打断、Freeze 清定时；真实 Nest：决策在实体锁与事务内按策略周期执行、冻结时驱动上没有登记、定时冻结到期后自己恢复）。`ai/stage_test.go`（事件下一拍可读、冻结时丢弃、不接收事件的策略返回 false、缓冲上限丢最早；树里切阶段在本拍结束后生效、策略外切换立即生效并打断旧树、未知阶段、`Init` 回到起始阶段；`ParseStages` 的 stages / set_stage / event 与三种校验、`ParseTree` 拒绝 set_stage；`TraceNodes` 的每节点轨迹、不开时没有）；`actionflow/behavior_test.go`（指标：不开时不写，开时 started / ended / running 的计数）。变异核对 D1～D12、S1～S12、B1～B5、H1～H12、E1～E8 原文见 [D21 实施结果](../../maintenance/DESIGN-TODO.md#6-调研一--实施)。

## 2E. 业务时间取自 ctx（D22-1）

DESIGN-TODO D22-1（维护者 2026-10-21 决定 ①A、②A）。现行说明见 [guide §1A](../guide/10-time.md#1a-业务时间一律取自-ctxd22-1)。

1. **冻结点只有一个**：`fctx.NewContext` → `init` 固定 `Now = clock.Now()`、`NowMilli`（`infra/base/fctx/context.go`）。Nest 消息在 `ensureNestContext`（逻辑阶段）建上下文；`ContextSnapshot` 不带 `Now`，换池续行重新冻结。`fctx.Now()` / `NowMilli()` 在请求上下文里返回冻结值，之外现读进程业务时钟。
2. **一次执行里的派生值**：`fctx.Frozen(key, compute)` 存在 `Context.frozen`（切片，`init` / `Clear` 时清空、保留容量），不进 `Snapshot`、不进 `keyValue`（`keyValue` 会随快照交接，带着冻结值进入重新冻结 `Now` 的新执行就会不一致）。没有请求上下文时不缓存。
3. **框架内的调用点**（全部在 Nest 消息里执行，改为 `fctx.Now`）：心跳驱动缺省时钟（`gameplay/heartbeat/driver.go` `NewDriver`，`Beat.Now` 与启动时刻）；entitytimer 驱动缺省时钟（`framework/nest/entitytimer/driver.go` `NewDriver`，`Host` 的增删改、`OnInitFinish`、`Tick()`）；`actionflow` `ActionRunner.acquireContext`、`MissionRunner.now` 与 `Behavior.now`（D21 合入后补）、`ai.Controller.now` 的缺省（`FreezeFor` 的期限、计划步骤 `Timeout` 的起点经它们取时间，任务心跳与 AI 心跳的到期判断用 `Beat.Now`，两边同源）；`timer.Scheduler.now` 的缺省。`wiring/heartbeat`、`wiring/entitytimer` 不再 `SetClock(app.BusinessClock(r).Now)`：进程业务时钟（`clock.SetOffset`，`App.run` 启动时设）与 Registry 业务时钟读同一份 `time.logic_offset`，再接一份就是两个来源。`SetClock` 只给测试。
4. **entitytimer 的 Tick 不带时间**：驱动 `DeliverDue` 以 `nil` 参数投递（原来带投递时刻的 UnixMilli），`Host.Tick()` = `TickAt(driver.Now())`。生成的 Tick handler（`codegen/internal/roost/add_timer.go` `renderTimerTick`）与组件 `Tick()`、game-demo 的 `handlerTickWorldTimers(target)` 随之改。用 `Driver.Now` 而不是直接 `fctx.Now`：测试经 `SetClock` 换掉驱动时钟时，判定到期与触发仍是同一个时钟。
5. **业务时间参数由生成的分发包装填**（`codegen/internal/nest`）：`parse.go` 把参数名 `nowUnix` / `nowUnixMilli` 识别为业务时间参数（类型必须 `int64`，否则生成期报错），`NonEntityParam.BusinessTime` 是填入表达式、`WireIndex` 为 -1；`gen.go` 的分发包装按 `WireIndex` 从 `params` 取其余参数、参数个数只数 `wireParams`，业务时间参数写 `pN := fctx.Now().Unix()` / `fctx.NowMilli()`；Sender 的签名与 `nest.NewParams` 只用 `wireParams`。endpoint 骨架（`add_workflow.go` `endpointArguments`）跳过这两个参数，不取业务钟、不再要求 controller 有 `Registry()`。
6. **静态提示**：`cmd/glsvet` `reportHandlerClockHints`：业务目录（`-businessdirs`，缺省 `game`）里 Nest handler 本身（文档有 `//roost:nest`，或以 `handler` 开头）的函数体内读 `app.BusinessClock` 或进程业务时钟 `clock.Now` / `clock.UnixMilli` 打印 `hint:`；`//glsvet:live-business-clock <理由>` 豁免（同一行、上一行或函数文档）。不跟进 helper、不认改名的变量或包装函数——提示只覆盖最直接的写法，约束本身写在 guide §1A 与生成文档里。做提示而不是失败：Nest 之外的代码读 `app.BusinessClock` 是正确的，handler 里确实要现读（量耗时）也合法。
7. **测试的可控时钟**：`clock.SetSource(now)` 换进程业务时钟的真实时间来源（`logicClock.source`，原子指针，`nil` 恢复 `time.Now`；`Set` 也按来源算偏移）。用它让 `fctx` 冻结的时间可控或每读一次单步（证明“第二次现读会不同”）。只给测试：进程全局，用的包不能 `t.Parallel`。

变异核对（修前失败文本）：心跳 `Beat.Now` 现读第二次（`TestBeatNowIsTheMessagesFrozenBusinessTime`：`Beat.Now = …00.004, but the beat's message froze …00.003`）；entitytimer 按投递时刻触发（`TestTickFiresAtTheMessagesFrozenBusinessTime`：`Fire.Now = …00.005, but the Tick message froze …00.007`）；分发包装由调用方传时间（awaitflow `TestGeneratedDispatchFillsTheBusinessTimeAtExecution`：`handler got business time 1800000000001, but its message froze 1800000000003`）；actionflow / AI 缺省现读（`ActionContext.Now = …00.002, want … frozen …00.001`）；glsvet 不接 `reportHandlerClockHints`（`hints = 0, want 3`）。

## 2F. 场景时间域

DESIGN-TODO D22 场景时间域（原 D22-3；维护者 2026-10-21 决定 ③⑤⑦，全局不做变速）。现行说明见 [guide §4D](../guide/10-time.md#4d-场景时间域变速)。

1. **映射**（`gameplay/timedomain/timedomain.go` `State.At`）：`D = AnchorDomainNano + floor((B − AnchorBusinessNano) × Rate)`，按墙上时间的 UnixNano 算（域时间不带单调时钟读数）。`B` 早于锚点时停在锚点（不往回外推）。倍率 1 与 0 精确；其余 `float64` 乘法对非负倍率单调，所以 `At` 单调不减。零值 `State`（`AnchorBusinessNano == 0`）是恒等映射。
2. **改倍率**（`SetRate`，所属实体锁内）：生效时刻 `T = fctx.Now()`；新锚点 `max(当前参数.At(T), 已交出的最大域时间)`——当前参数读 `Fields`（含本事务里之前的修改，不读已发布的），所以连续；不低于已交出的值，所以成员不回退。写 `Fields`（DAO，随事务回滚、随同步下发），`seq++`，`nest.AfterCommit` 发布，不在事务里立即发布。倍率先校验（有限、在域的 `[min, max]` 内、域范围在 `[0, 100]` 内），失败什么都不改。
3. **发布按序号**（`publish`）：`atomic.Pointer[publication]` CAS，只接受更大的 `seq`；`seq` 只在所属实体锁内递增、不随回滚退回（只排序）。回滚的修改不发布：`Fields` 由 DAO 回滚，`AfterCommit` 不执行。
4. **读取**（`Read`）：`fctx.Frozen(d, …)`，一条消息第一次读取时用冻结的 `B` 与当时发布的参数算出并冻结，`Now` / `Rate` 同一次冻结。`read` 把结果与 `high`（已交出的最大域时间，原子）取较大者并 CAS 推进。保证：同一条消息内一致；所有读者看到的域时间单调不减（包括改倍率发布前后）。限制：发布之前开始的成员消息按旧倍率算，之后的读数被 `high` 托住，与场景 / 客户端的公式值差“发布延迟 × 倍率”，直到公式值追上（时停时一直差这一点点）。
5. **心跳接线**（`gameplay/heartbeat/host.go` `WithTimeDomain`、`runBeat`；`driver.go` `Reg.startClock`）：只改 `Beat.Now` 的来源、`Elapsed` 上限与启动时刻，调度、投递层不动。上限 `max(DefaultMaxElapsed, 档位) × 倍率`，时停为 0。
6. **actionflow / ai 的接线**（D21 合入后补，`actionflow/heartbeat.go` `BeatConfig.TimeDomain` / `registrationOptions` / `populate`，`actionflow/behavior.go` `NewBehavior`，`ai/beat.go` `NewBeatController`）：`TimeDomain` 非 nil 时登记选项追加 `heartbeat.WithTimeDomain`（动作组心跳、`Behavior` 的任务心跳、AI 心跳），拍里的 `ActionContext.Now` / `Elapsed`、`MissionRunner.Tick(beat.Now)`、AI 的 `Tick(beat.Now)` 是域时间；`BeatRunner.populate` 在心跳之外（`ticking` 为假）把 `ActionContext.Now` 换成 `Domain.Now()`；`Behavior.now` 与 `ai.Controller` 的 `Now` 钩子缺省取 `Domain.Now`，所以计划步骤 `Timeout` 的起点（`MissionContext.Now`）与任务心跳的到期判断、`FreezeFor` 的期限与 AI 心跳的到期判断各自同一把尺子。没挂域时这些缺省都是 `fctx.Now`（`Behavior` 原来缺省 `clock.Now`，D21 合入后补改）。
7. **不接的**：entitytimer（决定 ⑦）；技能 `Runtime`（业务在心跳里按域时间推进 tick）；`Domain` 不随场景卸载重载迁移（新实例新对象）。

变异核对（`timedomain_test.go`、`heartbeat/nest_test.go`）：去掉 `high` 托底（成员在时停发布后读到 `…16:00:00`，早于之前的 `…16:00:02`）；去掉 `fctx.Frozen`（一条消息先后读到 `{…00 1}` 与 `{…01 5}`）；提交前发布（`a rate change was published before its transaction committed`）；去掉序号比较（晚到的旧发布覆盖新的）；新锚点不托底（`the new anchor … is earlier than a domain time members already read`）；新锚点取业务时间而非当前映射（`the stop instant reads …16 want …22`）；去掉范围检查（`SetRate(-0.1) = <nil>`）；`BeatRunner` 心跳之外不换域时间、`Behavior` / `ai` 缺省不取域时间、登记不追加 `WithTimeDomain`（`TestBehaviorOnATimeDomainUsesDomainTime`、`TestABeatControllerOnATimeDomainUsesDomainTime`、`TestSceneMembersActOnTheSceneTimeDomain`）；心跳不读域（`Beat.Now … but the beat's message reads the domain at …`）；上限不乘倍率（`a 2s stall at 10x gave Elapsed 1s, want the 10s cap`）。

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

### timedomain

1 个实现文件、2 个测试文件（`scene_test.go` 经真实 Nest、心跳驱动与 `BeatRunner`）。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [timedomain.go](../../../gameplay/timedomain/timedomain.go) | `State`、`Fields`、`Config`、`Domain`、`Reading`（`New`、`ErrRateOutOfRange`、`MinRate`、`MaxRate`） |

### heartbeat

3 个实现文件、6 个测试文件（另有 `internal/bench` 压测原型）。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [tier.go](../../../gameplay/heartbeat/tier.go) | `Resolution`、`Periods`（`PeriodAtMost`：组件把想要的间隔换成档位，D21 第 4 期起 actionflow 与 ai 共用） |
| [driver.go](../../../gameplay/heartbeat/driver.go) | `Config`、`Lookup`、`Driver`、`Stats` |
| [host.go](../../../gameplay/heartbeat/host.go) | `Beat`、`Func`、`RegOption`（`WithDurability`、`WithTimeDomain`）、`Host`、`Reg` |
| [wiring/heartbeat/heartbeat.go](../../../wiring/heartbeat/heartbeat.go) | `Mod` |

### movement

3 个实现文件、8 个测试文件（含 `example_test.go`）。心跳驱动的 runner 在 [actionflow/heartbeat.go](../../../gameplay/actionflow/heartbeat.go)（`BeatConfig`、`BeatRunner`）。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [map_basic.go](../../../gameplay/movement/map_basic.go) | `Facing`、`Fields`、`Space`、`InterestSpace`、`Snapshot`、`MapConfig`、`MapBasic`、`MapOwner`、`MemFields` |
| [move.go](../../../gameplay/movement/move.go) | `StopReason`、`Result`、`Locator`、`MoveConfig`、`MoveOwner`、`Move` |
| [action.go](../../../gameplay/movement/action.go) | `MoveToParam`、`FollowParam`（`RegisterActions`） |

### state

4 个实现文件、2 个测试文件（D21 第 2 期，契约见 §2D 第 6～8 条）。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [catalog.go](../../../gameplay/state/catalog.go) | `ID`、`Def`、`Source`、`Catalog`（包说明） |
| [dao.go](../../../gameplay/state/dao.go) | `StateDao`（`FieldStates`、`CollectionName`） |
| [set.go](../../../gameplay/state/set.go) | `Set`、`SetConfig`、`Change`、`Handle`、`Requirement`、`Watcher` |
| [gate.go](../../../gameplay/state/gate.go) | `Freezer`、`Rule`、`Gated`（`Gate`） |

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
- [nest_test.go](../../../framework/nest/entitytimer/nest_test.go)：`TestHostReportsThroughNestTransactions`、`TestTickFiresAtTheMessagesFrozenBusinessTime`
- [wiring/entitytimer_test.go](../../../wiring/entitytimer/entitytimer_test.go)：`TestModBindsNestAndReadsTheFrozenBusinessTime`、`TestModRefusesADriverInstalledTwice`

### heartbeat

- [driver_test.go](../../../gameplay/heartbeat/driver_test.go)：`TestTiersFireOnTheirPhaseOncePerPeriod`、`TestInflightBeatSkipsCountsAndWarnsAtALimitedRate`、`TestStalledBeatIsRedeliveredAndRunsOnce`、`TestSetPeriodMovesToTheNewTier`、`TestRegistrationsOfOneEntityAreDeliveredSeparately`、`TestInstanceChecksDropStaleBeats`、`TestStopWaitsForTheBatchAndDispatchesNothingAfter`、`TestConcurrentChangesStepsAndBeats`
- [nest_test.go](../../../gameplay/heartbeat/nest_test.go)：`TestBeatRunsUnderTheEntityLockInsideATransaction`、`TestBeatRunsInTheEntitysPool`、`TestStartStopFollowTheTransaction`、`TestBeatOfAnUnloadedInstanceDoesNotRunOnItsReplacement`、`TestFiftyThousandEntitiesSmoke`、`TestBeatNowIsTheMessagesFrozenBusinessTime`、`TestBeatsOnATimeDomainFollowItsRate`
- [remote_test.go](../../../gameplay/heartbeat/remote_test.go)：`TestRemoteHeartbeatFollowsOwnershipWithoutDoubleBeats`
- [wiring/heartbeat_test.go](../../../wiring/heartbeat/heartbeat_test.go)：`TestModBindsNestAndReadsTheFrozenBusinessTime`

### 业务时间取自 ctx 与时间域

- [fctx/ctx_test.go](../../../infra/base/fctx/ctx_test.go)：`TestFrozenHoldsOneValuePerExecution`、`TestNowIsFrozenPerExecution`
- [actionflow/action_runner_test.go](../../../gameplay/actionflow/action_runner_test.go)：`TestDefaultNowIsTheExecutionsFrozenBusinessTime`；[actionflow/behavior_test.go](../../../gameplay/actionflow/behavior_test.go)：`TestBehaviorDefaultNowIsTheExecutionsFrozenBusinessTime`、`TestBehaviorOnATimeDomainUsesDomainTime`；[ai/controller_test.go](../../../gameplay/ai/controller_test.go)：`TestControllerDefaultNowIsTheExecutionsFrozenBusinessTime`；[ai/beat_test.go](../../../gameplay/ai/beat_test.go)：`TestABeatControllerOnATimeDomainUsesDomainTime`
- [codegen/internal/nest/handler_contract_test.go](../../../codegen/internal/nest/handler_contract_test.go)：`TestBusinessTimeParametersAreFilledAtExecution`；生成链路实跑 [awaitflow/await_test.go](../../../codegen/internal/entity/testdata/awaitflow/await_test.go)：`TestGeneratedDispatchFillsTheBusinessTimeAtExecution`；endpoint 骨架 [roost_test.go](../../../codegen/internal/roost/roost_test.go)：`TestEndpointLeavesBusinessTimeToTheDispatch`
- [cmd/glsvet/clockhints_test.go](../../../cmd/glsvet/clockhints_test.go)：`TestClockHintsFlagBusinessClockReadsInNestHandlers`
- [timedomain_test.go](../../../gameplay/timedomain/timedomain_test.go)：`TestDomainTimeIsContinuousAcrossRateChanges`、`TestOneMessageReadsOneDomainTime`、`TestRateChangesFollowTheScenesTransaction`、`TestRateRange`、`TestMembersNeverSeeTheDomainGoBackwards`、`TestConcurrentRateChangesAndReads`；[scene_test.go](../../../gameplay/timedomain/scene_test.go)：`TestSceneMembersActOnTheSceneTimeDomain`

### movement

- [move_test.go](../../../gameplay/movement/move_test.go)：`TestAdvanceMovesBySpeedAndElapsedThenArrives`、`TestSkippedBeatsAreCoveredByOneElapsedAndFractionsAccumulate`、`TestANewMoveReplacesTheOldOne`、`TestMoveToStopsWhereTheTerrainBlocks`、`TestFollowTracksTheTargetUntilItIsGone`、`TestEnterAndLeaveTheScene`、`TestRefusedPositionHoldsOnBackpressureAndEndsOtherwise`
- [nest_test.go](../../../gameplay/movement/nest_test.go)：`TestPositionChangesReachTheAOIOnlyWhenCommitted`、`TestEnterAndLeaveFollowTheTransaction`、`TestRolledBackMoveToLeavesNothingRunning`、`TestRunnerIsTickedByTheHeartbeatAndStopsWhenIdle`、`TestMoveToActionIsDrivenByTheRunnerHeartbeatAndReplacedByTheNextOne`、`TestFollowActionEndsWhenTheTargetIsUnloaded`
- [shared_test.go](../../../gameplay/movement/shared_test.go)：`TestSharedRemoteEntityRefusesHeartbeatsAndResumesAfterLeavingSharedMode`
- [smoke_test.go](../../../gameplay/movement/smoke_test.go)：`TestSmokeThousandsOfEntitiesMoveOnTheirOwnHeartbeats`
- [example_test.go](../../../gameplay/movement/example_test.go)：`Example`
- [actionflow/heartbeat_test.go](../../../gameplay/actionflow/heartbeat_test.go)：`TestEachGroupBeatsAtItsCurrentActionsInterval`、`TestAGroupsBeatTicksOnlyThatGroupWithItsElapsed`、`TestAFrozenGroupStopsBeatingAndResumesOnRecover`
- [movement/nest_test.go](../../../gameplay/movement/nest_test.go)（D21）：`TestAMoveThatIsAlreadySettledEndsInStartWithoutAHeartbeat`、`TestFreezingTheMoveGroupPausesOrInterruptsTheMove`

### state

- [set_test.go](../../../gameplay/state/set_test.go)：`TestCatalogRefusesBadDeclarations`、`TestASourcedStateHoldsWhileAnySourceRemains`、`TestADerivedStateIsComputedAndNotifiedOnlyWhenItChanges`、`TestWatcherChangesAreNotifiedInTheNextRoundAndRunawayIsBounded`、`TestQueriesAndWatcherBundles`、`TestAPersistentStateCannotChangeOutsideATransaction`、`TestTheDaoPersistsOnlyPersistentStatesByName`
- [nest_test.go](../../../gameplay/state/nest_test.go)：`TestStatesFollowTheTransaction`、`TestGateFreezesActionGroupsWhileStatesHold`、`TestGateRealignsAfterARolledBackStateChange`、`TestGateAlignsWhenInstalledAndReleasesOnClose`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。没有新测量不能改容量承诺，性能结论以 [性能报告](../../performance/README.md) 为准。
