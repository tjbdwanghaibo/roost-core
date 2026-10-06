# D-L1 / D-L2：定时器同期限顺序与 priority、未注册类型的节点

> **状态（2026-10-06 核对）**：已实施（`5abae51e`），已随 v1.21.0 发布。下文“未发版”是实施当时的状态。

2026-10-06，分支 `dl12`，基线 `ce79ef18`。来由：[DECISIONS-PENDING 第六轮 D-L1 / D-L2](../review/DECISIONS-PENDING-2026-10-05.md)，[revleft §2 O6 / O7、§5](../review/REVIEW-2026-10-06-revleft.md)（N11 O6 / O7）。同轮 D-L3（时间来源）另行实施，本次**不改**定时器取时间的来源。

图谱限制：codebase-memory 共享 generation 停在 09-30，`timer` 包与 World 模板（`.tmpl` 不在图里）以当前源码与 `rg` 为准。

## 1. 问题

- **O6 同期限顺序未定义**：`timer/scheduler.go` 的 `timerHeap.Less` 只比较 `End`，同期限的触发顺序由堆的形状决定——依次登记 1..6 按 1、6、5、4、3、2 触发。World 的 `TimerComponent` 每次调用都从 DAO 的 map 重建调度器（A1），map 遍历顺序随机，同期限的顺序每次都可能不同。World 同一 Tick 武装的节点期限相同（时钟被钉住），所以这不是边角情形。
- **O7 未注册类型静默删除**：到期节点的类型没有 handler 时，`Tick` 发 `ChangeDelete`、删除，没有日志也没有计数。下线一种定时器类型后，存量节点到期即无声消失。

## 2. 设计

**timer 包**（`timer/scheduler.go`，包注释写明规则）：

- `Node.Priority int32`（缺省 0）。排序键依次为 `End`、`Priority`（**数值小的先触发**）、`ID`（登记顺序；同一个 Scheduler 的 ID 单调递增，`ChangeTimer` 与按返回值重排都保留 ID，所以是“最初登记的顺序”）。ID 唯一，顺序是全序，与入堆顺序、存储遍历顺序无关。
- `NewTimerWithPriority(delay, type, priority, param1, param2, payload)`；`NewTimer` 等价于 priority 0。闭包定时器不加 priority（纯内存、调用方自己能控制）。
- 到期且类型没有 handler：节点照旧删除（不保留——留在堆顶会挡住之后的节点，Tick 要多一种“越过”状态，违背 N11 方向判断），另打 `slog.Warn("timer: dropped a due timer with no handler registered for its type", owner_id, timer_id, type, end)` 并计 `timer.unhandled_dropped_total{kind=<类型号>}`（常量 `timer.UnhandledDroppedMetric`）。类型号是代码常量，标签基数等于定义过的类型数。宿主事务回滚后重试同一次 Tick 会再计一次。
- `ReportUnhandledTypes() []int32`：对堆里没有 handler 的 typed 节点类型每种打一条 Warn（带节点数），升序返回；只报告，不删除、不计数。宿主在加载、注册完 handler 后调用一次。

**World `TimerComponent`（game-demo 模板）**：

- `db/def/world.go.tmpl` 的 `TimerNode` 加 `Priority int32 bson:"priority"`。持久格式只增不改：之前存下的节点没有 `priority` 键，解码为 0；不升 schema、不迁移。（DAO 生成器对嵌套字段不处理 `omitempty`，新写的节点都会带 `priority` 键。）
- `scheduler()` 读回 `Priority`，`persist()` 写入 `Priority`。
- `OnInitFinish` 改为从 DAO 建一次调度器：`ReportUnhandledTypes()` 告警、`settle` 写 `timer_next_due`（原来手写的最早期限循环由调度器的 `NextTime` 代替；不武装、不触发，时钟不被读取，也不写持久字段）。
- `ScheduleActivityPhase` 不需要优先级，保持 0、接口不变；需要同时刻先后的新类型用 `scheduler.NewTimerWithPriority`（组件注释写明）。

## 3. 兼容

- **行为变化**：同期限定时器从“未定义顺序”变为按 (priority, 登记顺序)。依赖旧顺序不可能（旧顺序取决于堆形状与 map 遍历）。
- 新增 API：`Node.Priority`、`Scheduler.NewTimerWithPriority`、`Scheduler.ReportUnhandledTypes`、`timer.UnhandledDroppedMetric`；内部 `add` / `addNode` 多一个参数（未导出）。
- 未注册类型的节点仍然删除，多一条 Warn 和一个计数；日志量以被删节点数为界。
- World 存储：新节点多一个 `priority` 键；旧文档按 0 读回（用例覆盖：剥掉全部 `priority` 键后 `RestorePersisted`）。已生成的工程要 `roost project sync` 或手工同步三个模板才得到新行为；不同步也能编译（core 只加 API）。

## 4. 验证

**先红后绿**（红文本取自修前代码，新 API 的用例在修前接口存根上跑出红）：

`timer/order_and_unhandled_promises_test.go`：

```
--- FAIL: TestTimersWithTheSameDeadlineFireInRegistrationOrder/armed_in_one_scheduler
    timers due at the same instant fired in order [1 6 5 4 3 2], want registration order [1 2 3 4 5 6]
--- FAIL: TestTimersWithTheSameDeadlineFireInRegistrationOrder/rebuilt_from_stored_nodes_in_any_order
    stored timers due at the same instant fired in order [4 3 5 1 6 2], want [1 2 3 4 5 6]
--- FAIL: TestTimersWithTheSameDeadlineFireInRegistrationOrder/rescheduled_timers_keep_their_place
    fired [2 1], want [1 2]: the postponed-then-advanced timer was registered first
--- FAIL: TestPriorityOrdersTimersWithTheSameDeadline
    fired [6 3 5 4 1 2], want [6 2 3 5 1 4] (deadline, then priority ascending, then registration order)
--- FAIL: TestADueTimerWithoutAHandlerIsDroppedWithAWarningAndACount
    timer.unhandled_dropped_total{kind="7"} = 0, want 2
    got 0 warnings, want one per dropped node:
```

（`TestPriorityIsKeptThroughStorageAndRescheduling`、`TestStoredTypesWithoutAHandlerAreReportedOncePerType` 是新 API，存根上红：`priority 0, want 7`、`ReportUnhandledTypes() = [], want [3 9]`。）

game-demo `game/entities/world/timer_component_test.go`（修前模板 + 修前 core 生成，replace 到本 worktree）：

```
--- FAIL: TestDeadlinesDueAtTheSameMomentFireInArmOrder
    deadlines due at the same moment fired in order [race-c race-f race-e race-b race-h race-g race-a race-d], want the order they were armed in [race-a race-b … race-h]
--- FAIL: TestAStoredTimerOfATypeWithNoHandlerIsReportedAndCountedWhenDropped
    loading stored timers of two unhandled types logged 0 warnings, want one per type:
    dropping three unhandled nodes logged 0 warnings, want 3:
    timer.unhandled_dropped_total{kind="99"} = 0, want 2
    timer.unhandled_dropped_total{kind="98"} = 0, want 1
```

`TestTimerPriorityIsStoredAndOrdersAfterARestart`（新字段，修前不能编译）：priority 写进存储、重启后按 priority 触发、剥掉 `priority` 键的旧文档按 0 读回。

修后全部通过。NC-140 / NC-141 / NC-147 的回归（`scheduler_promises_test.go`、World 的 `TestTheTimerHeapRollsBackWithTheTransaction` / `TestTimerRollbackIsTheDaoRollback`）照样通过。

命令（`GOWORK=off`）：`gofmt -l` 空；`go vet ./timer`；`go test -race -count=3 ./timer`；`go build ./... && go vet ./...`；根包 `go test -count=1 .`；`go test -count=1 ./codegen/...`；`go generate ./...` 后 porcelain 只有本次改动；全新生成 game-demo（replace 到 worktree）`go build ./... && go vet ./... && go test -count=1 ./...`，`go test -race -count=3 ./game/entities/world/`。

## 5. 文档

timer 包注释、`README.md`（工具包一段）、`docs/USER_GUIDE.md` §3、`OBSERVABILITY.md`（新节“实体定时器”）、`docs/feature/GAME_DEMO_TEMPLATE.md` §9.8.1、`docs/TROUBLESHOOTING.md` 新 T 行、`CHANGELOG.md`、交接 §7、revleft §2 O6 / O7。

## 6. 实施状态

已实施（提交号见 DECISIONS-PENDING 第六轮 D-L1 / D-L2 行），未发版。未做：无。
