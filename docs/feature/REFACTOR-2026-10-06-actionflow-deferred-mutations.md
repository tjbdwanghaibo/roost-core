# B7：actionflow 回调里的变更进延后队列

2026-10-06，分支 `b7c7b3`，基线 `aa415fa6`。来由：[DECISIONS-PENDING B7](../review/DECISIONS-PENDING-2026-10-05.md)（第三轮决定：方向 b），[N10 方向判断](../review/REVIEW-2026-10-05-noncore-n10.md#方向判断)，NC-121 / NC-122、U-0077 / U-0100 / U-0125。

## 1. 问题

`ActionRunner` 允许回调（动作的 Start / Tick / Cancel，OnQueued / OnTransition / OnEnded 钩子）里任意重入变更方法，再在每个回调点之后用 `unit.cur != entry` 事后比对，比对不上就返回 `ErrReentrantMutation`。每多一个回调点就要多一处判定，漏一处就出孤儿（NC-122 是 U-0100 之后漏掉的第五处）。同一机制下还有 N10 记录的观察：Cancel 重入时同一动作被 Cancel 两次（O-A3），EndAll 与结束钩子里的推进交错（O-A1），非排队 Start 失败不发 OnEnded（O-A2）。

## 2. 目标与设计

回调里的变更不立即执行，进一个延后命令队列，最外层调用做完自己那一步后按发起顺序执行。判定只在一处：`ActionRunner.submit` 看 `executing`（某次最外层调用正在执行，含它触发的全部回调）。

- **命令**：`Start`、`Enqueue`、`Tick`、`End`、`EndAll`、`ClearQueue`、`ClearMission`、`Recover` 都组成一条 `runnerCommand` 交给 `submit`；不在回调里就作为最外层调用执行，并在返回前执行完延后队列（`drain`）；在回调里就排进队列。执行中引出的回调再发起的命令接着排在后面。
- **队列推进也是命令**（内部 `opAdvance`）：动作结束、启动失败、Enqueue 到空闲组、Recover 之后不直接启动队首，而是在队尾登记一次推进（每组至多一条）。这样同一时刻回调里显式发起的 Start 先执行，排队项不会“先启动再被替换”，保持“显式 Start 先于队列”的既有语义（`TestActionRunnerExplicitStartPrecedesQueue`、新增 `TestHookStartStillPrecedesQueuedActions`）。
- **不延后的**：`Freeze` 只置标志、不触发回调，始终立即生效（OnEnded 里冻结组以阻止推进的用法照旧）；`Update` 在回调里立即调用 fn，不在回调里时 fn 运行期间同样算回调；读方法在回调里读到尚未执行延后命令的状态。
- **命令按组 / 任务解释**，不是发起时的快照：等价于回调返回后立即按序调用。
- **删掉的分支**：`Start` 替换后、`Tick` 之后、`start` 的切换钩子 / Start / 失败 Cancel 之后、`finish` 的 Cancel 之后，共六处 `unit.cur != entry` 比对。`apply` 执行期间当前位只会被自己改。

### 2.1 返回值与“已延后”

- 回调里的 `Start` / `Enqueue` 立即解析组、构建动作，返回**已分配的 ID 与 nil**。新增 `ActionRunner.Deferring() bool`：为真表示此刻的变更调用会被延后（“已延后”状态）。没有改 `Start` 的签名，也没有用非 nil 的哨兵错误表示“已延后”——那会让把 `err != nil` 当失败的调用方（`ai.TaskflowAction`、`PlanMission.startStep`）丢掉一个其实会运行的动作。
- **拿到 ID 的动作一定有结论**：启动后照常结束；延后执行时已无法启动（组已被冻结）发一次失败状态的 OnEnded（`Err` 为 `ErrActionGroupFrozen`）；被预算截停的发一次取消状态的 OnEnded（`Err` 为 `ErrDeferredRunaway`）。
- **错误去向**：属于最外层调用的命令（它本身、它直接引出的队列推进）的错误返回给最外层调用方，与之前一致（例如 Recover / Tick / Enqueue 返回排队项启动失败）；回调发起的命令出错时发起方早已拿到 nil，改经 `OnError` 报告（`taskflow: deferred <op>: ...`）。排队项启动失败照旧既返回（若属于最外层）又报告一次。
- **直接 Start 失败也发 OnEnded**（O-A2）：进入切换的动作恰好一次离开、一次结束，直接 / 排队 / 延后三条路径一致。之前直接路径不发，NC-122 用例里那一次 OnEnded 是嵌套 Start 的 finish 顺带发的；改成延后后要保住“失败动作结束一次”的承诺，就由 `start` 自己发。

### 2.2 有界与防失控

- `ActionRunnerConfig.MaxDeferredCommands`（默认 64）：同时待执行的回调命令数上限，超出时变更方法立即返回 `ErrDeferredQueueFull`，被拒的 Start / Enqueue 不分配 ID。内部推进命令不计入（每组至多一条）。
- `ActionRunnerConfig.MaxDeferredSteps`（默认 1024）：一次最外层调用总共执行的延后命令数。超出时截停：置 `runaway`，剩余命令丢弃（已交出 ID 的各发一次 OnEnded），截停期间回调里的新变更直接返回 `ErrDeferredRunaway`（丢弃本身不会再引出命令），最外层调用返回 `ErrDeferredRunaway` 并经 OnError 报告；之后 runner 照常可用。被丢弃的推进让该组队列停在原处，直到下一次变更再推进。
- 之前的同类死循环（结束钩子里不断发起启动必然失败的动作）在旧代码上是无界递归；现在是有界循环。

## 3. 兼容

- `ErrReentrantMutation` 保留导出；`ActionRunner` 不再返回它，`MissionRunner` 照旧返回（任务启动 / 结束途中的重入守卫，形态是 (a)，本次不改：决定只针对 ActionRunner，MissionRunner 的 `starting` / `ending` 已是集中判定）。**更正（第五轮）**：MissionRunner 也改为延后队列，`ErrReentrantMutation` 不再由任何 runner 返回，见 §7。
- `ai`：没有改代码，只补注释。`TaskflowAction` 的 Launch 在回调里调用时拿到已分配的 ID 与 nil，照常等这个 ID 的结束（新增 `ai/deferred_launch_promises_test.go`）。`Controller` 的 EndActions → EndAllAction、MissionRunner 的 ClearActions → ClearMission 在回调里调用时同样延后。
- `PlanMission`：OnEnded → MissionRunner.OnActionEnd → 推进下一步 → CreateAction 在回调里，拿到的 ID 就是之后真正启动的动作 ID（新增 `TestPlanMissionAdvancesThroughADeferredStart`）。
- 行为差异（仓内无生产调用方，`rg` 全仓 import 只有 ai → actionflow）：回调里的 Start 之后、回调返回前，`Current` 仍是旧动作；外层调用不再返回 `ErrReentrantMutation`；在切换钩子里被替换的动作现在会先完整 Start 再被取消（之前未 Start）；直接 Start 失败多发一次 OnEnded。
- `ActionRunnerConfig` 新增两个可选字段，零值取默认。性能：`BenchmarkActionRunnerTick` 0 alloc/op，`Lifecycle` 2 alloc/op（动作 + 条目，与之前同）。

## 4. 测试

### 4.1 新增（`actionflow/deferred_mutation_promises_test.go`）

| 用例 | 钉住的承诺 |
| --- | --- |
| `TestCallbackMutationsAreDeferredUntilTheOuterCallReturns` | start / tick / cancel / transition / ended / queued 六个回调点：回调内 `Deferring()` 为真、Start 返回 ID 与 nil、动作未启动、当前位未变；外层返回 nil 时延后动作已启动一次；被替换的动作 Cancel 一次、结束一次 |
| `TestCancelReentryCancelsTheReplacedActionOnce` | **O-A3 消失**：替换路径与启动失败路径都只 Cancel 一次 |
| `TestDirectStartFailureEndsTheActionOnce` | **O-A2 消失**：直接 Start 失败离开一次、结束一次 |
| `TestDeferredStartThatCanNoLongerRunStillEndsItsID` | 延后 Start 执行时组已冻结：该 ID 收到失败 OnEnded，错误经 OnError 报告 |
| `TestHookStartStillPrecedesQueuedActions` | 结束钩子里的 Start 先于排队项（控制：旧代码也成立） |
| `TestEndAllEndsEverythingBeforeCallbackMutationsRun` | O-A1 的新定义：EndAll 先结束全部，回调发起的变更随后执行（不再交错） |
| `TestCallbacksThatKeepTriggeringEachOtherAreBounded` | 互相触发被预算截停、交出的 ID 各结束一次、之后可用 |
| `TestDeferredQueueIsBounded` | 队列上限、被拒不分配 ID |
| `TestPlanMissionAdvancesThroughADeferredStart` | 接线兼容 |

修前红（旧代码，临时桩 `Deferring()` 与两个哨兵，去掉需要新配置字段的两条）：

```
--- FAIL: TestCallbackMutationsAreDeferredUntilTheOuterCallReturns/start
    outer call from start = taskflow: reentrant mutation changed current action, want nil: a callback mutation is deferred, not a conflict
    （tick / cancel / transition 同；ended / queued：Deferring() was false inside ended / queued）
--- FAIL: TestCancelReentryCancelsTheReplacedActionOnce/replace
    taskflow: reentrant mutation changed current action
--- FAIL: TestCancelReentryCancelsTheReplacedActionOnce/start_failure
    failing action canceled 2 time(s), want 1
--- FAIL: TestDirectStartFailureEndsTheActionOnce
    failed action left 1 / ended 0 time(s), want 1 / 1
--- FAIL: TestDeferredStartThatCanNoLongerRunStillEndsItsID
    deferred start into a frozen group ended=false reason={...}, want one failed OnEnded with ErrActionGroupFrozen
--- FAIL: TestEndAllEndsEverythingBeforeCallbackMutationsRun
    order = [ended:1 start:follow-up ended:2], want [ended:1 ended:2 start:follow-up]
```

`TestHookStartStillPrecedesQueuedActions` 在旧代码上通过（控制）。队列上限 / 预算两条依赖新配置字段，旧代码无法编译，属新增能力；旧代码上同形的互相触发是无界递归。

### 4.2 改写的既有回归（逐条理由）

| 用例 | 原断言 | 改为 | 理由 |
| --- | --- | --- | --- |
| U-0100 `TestActionRunnerReportsReentrantMutationFromEachCallback` → `TestActionRunnerCallbackStartReplacesTheOuterActionExactlyOnce` | 外层返回 `ErrReentrantMutation`；transition 情形外层动作 `starts == 0` | 外层返回 nil；外层动作 `starts == 1`、`cancels == 1`、OnEnded 一次；cancel 情形 replacement 启动一次、取消一次 | 回调里的 Start 延后到外层那一步做完，没有冲突可报；外层动作先完整启动再被按序替换。原承诺“内层装上的动作是唯一当前动作、只启动一次”保留且加强（替换收尾恰好一次） |
| NC-122 `TestStartFailureWhoseCancelStartsAnotherActionKeepsThatAction` | 返回值同时 `Is` 启动错误与 `ErrReentrantMutation` | 只 `Is` 启动错误、不含 `ErrReentrantMutation`；加 `failing.cancels == 1` | Cancel 里的 Start 延后，失败动作先完整收尾。next 在当前位、只启动一次、可被 Tick 结束，失败动作离开一次、结束一次——这些断言原样保留 |
| NC-122 `TestQueuedStartFailureWhoseCancelStartsAnotherActionKeepsThatAction` | 同上（Recover 返回值） | 同上 | 同上；“排队路径不越过 next 启动下一项”原样保留（推进命令排在 Cancel 发起的 Start 之后） |

U-0077（冻结组 / 任务独占，`promises_test.go`）、U-0125（`plan_guards_promises_test.go`，含 MissionRunner 的 `ErrReentrantMutation`）、NC-120（ai 冻结期结束通知）、NC-121（丢弃排队项发 OnEnded）、NC-123 及其余既有用例不改、全部通过。

### 4.3 N10 观察在延后语义下

| 观察 | 状态 |
| --- | --- |
| O-A1 EndAll 之后仍有回调启动的动作 | 仍在，语义定义为“EndAll 先结束全部、回调发起的变更随后按序执行”，用例钉住顺序。若要“清场”语义需另加屏蔽，未做。**第五轮决定保持**，清场顺序写进文档，见 §8 |
| O-A2 直接 Start 失败不发 OnEnded | **消失**（为保住 NC-122 的承诺而统一），用例钉住 |
| O-A3 Cancel 重入时同一动作 Cancel 两次 | **自然消失**，用例钉住 |
| O-A4 替换时旧动作 Cancel panic 让新动作没启动 | 仍在（与重入无关，是 panic 处理选择），未改 |
| O-T3 新策略 Init 里发起的动作被 EndActions 结束 | 仍在（Controller 的事务顺序，与 runner 延后无关） |
| O-T4 Shutdown 不调 EndActions | 仍在（与 runner 无关） |

## 5. 验证

`GOWORK=off`：`gofmt -l` 空；`go vet ./actionflow ./ai`；`go test -race -count=3 ./actionflow ./ai` 通过；根包与全仓 build / vet 见提交说明。没有改三大模块，不跑 glsvet。

## 6. 实施状态

已实施（见 DECISIONS-PENDING B7 行的提交号）。未做：MissionRunner 改延后语义（形态 (a) 保留）；O-A1“清场”语义、O-A4。

**N10 第二批复核（2026-10-06，[记录](../review/REVIEW-2026-10-06-noncore-n10b.md)）**：O-A4 在延后语义下违反“拿到 ID 的动作一定有结论”，按缺陷修复（RR-20261005-NC-243：旧动作 Cancel 失败只报告、新动作照常启动）；另修 B7 引入的 Update fn panic 后 `executing` 不复位（RR-20261005-NC-242）。MissionRunner 延后语义与 O-A1“清场”没有确认缺陷，选项与推荐（都保持现状）见该记录“待维护者”。

## 7. MissionRunner 延后队列（维护者 2026-10-06 第五轮决定）

基线 `31b48bc0`，分支 `d1mr`。N10 第二批把“MissionRunner 是否改延后语义”列为待维护者（推荐保持），维护者决定改为与 ActionRunner 一致。

### 7.1 之前

形态 (a)：`starting` / `ending` 两个标志。任务启动途中（OnState / OnChanged / Mission.Start）或结束途中（Mission.End、ClearActions、OnState、OnChanged、OnEnded）调 `StartMission` 返回 `ErrReentrantMutation`；结束途中的 `EndCurMission` / `Tick` / `OnActionEnd` 静默忽略；而 Mission.Tick / Mission.OnActionEnd 里的 `StartMission` 会**嵌套立即执行**，外层随后用 `r.cur == cur` 事后比对决定是否 `update`。StartMission 自己还有三处事后比对（替换后、changed 之后、Start 之后）返回 `ErrReentrantMutation`。

### 7.2 设计

与 ActionRunner 同构，代码在 `actionflow/mission_runner.go`：

- **判定一处**：`StartMission`、`CancelMission`、`EndCurMission`、`Tick`、`OnActionEnd` 组成 `missionCommand` 交给 `submit`。`executing` 为假就作为最外层调用执行，返回前 `drain` 执行完全部延后命令；为真（某次调用的回调里）就进队列。`Deferring()` 报告此刻是否会被延后。`starting` / `ending` 与所有事后比对删除——执行期间当前任务只会被这条命令自己改。
- **回调范围**：任务的 Start / Tick / OnActionEnd / End / CanReplaceBy / SetRuntime、任务构建器，以及 Now / Context / OnState / OnChanged / OnEnded / ClearActions / OnError 钩子。
- **按执行时解释**：延后命令等价于回调返回后立刻按序调用；回调里先 `StartMission` 再 `EndCurMission`，结束的是新任务（与 ActionRunner 的按组解释一致）。
- **返回值与 OnError**（与 ActionRunner 一致）：回调里的 `StartMission` / `CancelMission` 返回 nil；参数校验仍立即返回（任务类型为 0 → `ErrKindInvalid`；`CancelMission` 时没有任务 → `ErrMissionNotRunning`，按调用时的状态）。延后命令执行时的错误（`ErrMissionRunning`、构建失败、Start 失败）发起方已拿不到，经 `OnError` 报告为 `taskflow: deferred start mission: …`；最外层调用自己的错误照旧返回。MissionRunner 的命令没有预先交出的 ID，所以不需要 ActionRunner 那样的“拿到 ID 必有结论”补发——Start 失败的任务照旧走完整收尾（End、ClearActions、OnEnded）。无返回值的 `EndCurMission` / `Tick` / `OnActionEnd` 在回调里被拒（队列满、已截停）时经 OnError 报告。
- **有界**：`MissionRunnerConfig.MaxDeferredCommands`（默认 64，超出 `ErrDeferredQueueFull`）、`MaxDeferredSteps`（默认 1024，超出截停：丢弃剩余命令、`ErrDeferredRunaway` 经 OnError 报告并由最外层返回，截停期间回调里的新变更直接返回它）；之后照常可用。复用 ActionRunner 的两个哨兵与默认值。
- **panic**（N10 第二批 / NC-242 的教训）：全部回调本来就经 `callMission*` / `recoverHook` 恢复；本次把 `SetRuntime` 也包进 `callMissionSetRuntime`（新的回调点）。`executing` 在 `submit` 的 defer 里复位，即使有未预料的 panic 穿出也不会停在“延后中”。
- `Reset` 不是变更命令，立即生效，只用于复用对象，不要在回调里调用。

### 7.3 兼容

- `ai`：不直接构造 MissionRunner（经 `ActionList.SetMission` / `MissionManager`），没有代码改动，只补注释（`Controller.SetStrategy` 的清场、`OnMissionEnd` 里 SetMission 的语义）。行为差异：接在 MissionRunner OnEnded 上的 `Controller.OnMissionEnd` 里，策略调 `SetMission` 之前得到 `ErrReentrantMutation`，现在得到 nil、任务在这次结束做完后启动，执行时的错误经 MissionRunner 的 OnError 报告。
- 其余行为差异（仓内 import actionflow 的只有 ai，`rg` 核对 `cube` / `ssr` / `chaos` 无 MissionRunner 调用）：
  - Mission.Tick / Mission.OnActionEnd 里的 `StartMission` / `EndCurMission` 不再嵌套执行，回调返回后才执行；`PlanMission` 在 OnActionEnd 里推进到结束时，`EndCurMission` 延后，外层先发一次 OnChanged（终态的 MissionInfo，任务仍是当前任务），随后结束时再发一次——多一次 OnChanged。
  - 任务启动途中钩子发起的结束不再让 StartMission 返回 `ErrReentrantMutation`：任务先完整启动，再被结束。
  - ActionRunner 的 OnEnded 在 MissionRunner 执行中送来的 `OnActionEnd`（例如任务 Start 里同步启动的动作立即失败）现在在任务 Start 返回之后送达，`PlanMission` 能按刚记下的动作 ID 匹配；之前在 Start 途中送达、ID 还没记下而被忽略。
  - `StartMission` 对 nil runner 返回 `ErrMissionBuilderNotFound`（之前是 `ErrReentrantMutation`）。
- `MissionRunnerConfig` 新增两个可选字段，零值取默认；`MissionRunner.Deferring()` 新增。

### 7.4 测试

新增 `actionflow/mission_deferred_promises_test.go`：

| 用例 | 钉住的承诺 |
| --- | --- |
| `TestMissionCallbackMutationsAreDeferredUntilTheOuterCallReturns` | state / changed / mission start / mission tick / mission action end / mission end / ended / clear actions 八个回调点：回调内 StartMission 返回 nil、`Deferring()` 为真、被发起的任务未启动；外层返回 nil 后它是当前任务、恰好启动一次，旧任务结束一次且先于新任务启动 |
| `TestDeferredMissionStartErrorsAreReported` | 延后的构建失败、`ErrMissionRunning` 经 OnError（`deferred start mission`），最外层不受影响 |
| `TestMissionCallbacksThatKeepTriggeringEachOtherAreBounded` | 结束钩子不断发起启动必然失败的任务：恰好执行预算次、`ErrDeferredRunaway` 报告、之后可用 |
| `TestOuterMissionStartReturnsRunaway` | 最外层是 StartMission 时截停错误直接返回 |
| `TestMissionDeferredQueueIsBounded` | 队列上限、超出返回 `ErrDeferredQueueFull` |
| `TestMissionRunnerStaysUsableAfterCallbacksPanic` | OnEnded 钩子发起命令后 panic、延后启动的任务 Start panic：两者都经 OnError，已发起的命令照常执行，`Deferring()` 复位 |
| `TestEndCurMissionBeforeEndAllLeavesNothingRunning` | §8 清场顺序（控制，修前即绿） |

修前红（旧代码 `31b48bc0` 的 `mission_runner.go`，临时桩 `func (r *MissionRunner) Deferring() bool { return r.starting || r.ending }`，去掉需要新配置字段的三条，改写后的两条既有用例一并跑）：

```
mission_deferred_promises_test.go:141: StartMission from state = taskflow: reentrant mutation changed current action, want nil: a callback mutation is deferred, not a conflict
（changed / mission start / mission end / ended / clear actions 同）
mission_deferred_promises_test.go:144: mission issued from mission tick started inside the callback (1 start(s)), want after the outer call
mission_deferred_promises_test.go:144: mission issued from mission action end started inside the callback (1 start(s)), want after the outer call
--- FAIL: TestDeferredMissionStartErrorsAreReported
    deferred StartMission returned [taskflow: reentrant mutation changed current action ×3], want nil, nil, nil
--- FAIL: TestMissionRunnerStaysUsableAfterCallbacksPanic
    deferred start after panics: current=<nil> next starts=0
--- FAIL: TestMissionRunnerHookThatEndsTheMissionDuringStartEndsItAfterTheStart
    StartMission with a hook ending the mission = taskflow: reentrant mutation changed current action, want nil (the end is deferred)
--- FAIL: TestMissionRunnerDefersReentrantStartAndRefusesExhaustedIDsAndIdleCancel
    inner StartMission = taskflow: reentrant mutation changed current action built=0 current=…; want nil, nothing built and the outer mission still current inside its own Start
--- PASS: TestEndCurMissionBeforeEndAllLeavesNothingRunning（控制）
```

预算 / 队列三条依赖新配置字段，旧代码无法编译，属新增能力；旧代码上同形的互相触发被 `ErrReentrantMutation` 挡住，不会循环。

改写的既有回归（逐条理由）：

| 用例 | 原断言 | 改为 | 理由 |
| --- | --- | --- | --- |
| U-0125 `TestMissionRunnerRefusesAHookThatEndsTheMissionDuringStart` → `TestMissionRunnerHookThatEndsTheMissionDuringStartEndsItAfterTheStart` | StartMission 返回 `ErrReentrantMutation`，当前任务为空 | StartMission 返回 nil，当前任务为空，任务结束一次 | 钩子发起的结束延后到启动做完之后执行，没有冲突可报；原承诺“不把已结束的任务当作当前任务”保留 |
| `TestMissionRunnerRefusesReentrantStartExhaustedIDsAndIdleCancel` → `TestMissionRunnerDefersReentrantStartAndRefusesExhaustedIDsAndIdleCancel` | Mission.Start 里的 StartMission 返回 `ErrReentrantMutation`、没有构建；外层任务仍是当前任务 | 回调内返回 nil、没有构建、当前任务仍是外层；外层返回后延后的启动构建一次、替换外层任务（外层结束一次）；ID 耗尽与空闲 Cancel 的断言不变 | 延后语义下回调里的启动在外层做完后执行；“回调期间当前任务不被换掉、回调内不构建”保留 |

其余既有用例（NC-120～123、U-0077、`TestPlanMissionAdvancesThroughADeferredStart`、ai 全部）不改、全部通过。

## 8. EndAll 清场（维护者 2026-10-06 第五轮决定：保持现状，只改文档）

`ActionRunner.EndAll`（`ActionList.EndAllAction`）先结束全部动作、丢弃排队项；结束钩子里随之发起的变更（典型是 OnEnded → `MissionRunner.OnActionEnd` → `PlanMission` 按 OnFail 推进到下一步 → CreateAction）在 EndAll 返回后按序执行，所以 **EndAll 返回时可能已有新动作在运行，EndAll 本身不是清场**。要让实体什么都不再运行：**先结束当前任务（`EndCurMission`），再调 `EndAll`**——任务结束后送来的动作结束被忽略、不再推进。没有采用“EndAll 期间发起的动作一律以取消结束”（会静默改写任务的失败去向）或“EndAll 顺带结束任务”（混淆任务与动作两层职责）。

写在：actionflow 包注释“清场顺序”、`ActionRunner.EndAll` 注释、`ActionList` 接线注释（`actionflow/runtime.go`）、`ai.Controller.SetStrategy` 注释（EndActions 接线）、kit/README actionflow 一条。用例 `TestEndCurMissionBeforeEndAllLeavesNothingRunning` 钉住两种顺序的结果（只 EndAll：下一步动作在运行；先 EndCurMission 再 EndAll：什么都不运行），现状即为绿。

## 9. 第五轮实施状态

MissionRunner 延后队列、EndAll 文档已实施（提交号见 DECISIONS-PENDING 第五轮表）。验证：`GOWORK=off` 下 `gofmt -l` 空、`go vet ./actionflow ./ai`、`go test -race -count=3 ./actionflow ./ai`、根包与全仓 build / vet（见提交说明）。没有改三大模块，不跑 glsvet。

