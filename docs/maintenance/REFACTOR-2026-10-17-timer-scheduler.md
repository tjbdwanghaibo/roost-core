# 重构方案：timer.Scheduler 用节点状态替代 Tick 期间的推迟闭包（2026-10-17）

维护者 2026-10-17 批准（“问题比较大的话需要重构”）。基线 `1d55586f`。范围只有 `infra/base/timer`；公开 API、持久 `Node` 结构、`ChangeFunc` 的最终持久状态都不变。

## 1. 为什么重构（方向判断）

“Tick 期间的推迟操作”已经三轮出缺陷，每次修复都在加状态：

| 轮次 | 提交 | 缺陷 | 修复加了什么 |
| --- | --- | --- | --- |
| 1 | `23a10f42`（NC-140～147） | Tick 期间取消 / 改期仍在堆里的定时器，它照旧按旧期限触发；重入 Tick 清掉 `running` | 堆中节点立即出堆、改期节点“移出堆 + 推迟放回”；只有最外层 Tick 收尾 |
| 2 | `50526a2c` | 非法存量节点在加载（构造）期就写存储，越过宿主事务 | `invalid` 暂存表 + `NeedsCleanup`，由事务内 Tick 清理 |
| 3 | `a975c070`（原 `6a90cc23`） | 触发中节点的显式改期在回调返回 0 / 负值时丢失、返回正值时被重入 Tick 提前触发 | `firing` 重入栈 + `firingChanges` 控制表 |
| 3 | `1fa5ef08`（原 `c958144f`） | Tick 内新建定时器把取时钟、复制 payload 也推迟了 | 新建时先构造节点，只推迟 `addNode` |

另有未修的既有问题：收尾执行推迟闭包时 `onChange` panic，`deferred` 与 `firingChanges` 不清空，同一个 Scheduler 再 Tick 会重放（`TestOnChangePanicDuringSettlementDoesNotReplay` 在基线上三个子用例全红，见 §7）。

共同根因：节点在“触发中 / Tick 内新建未入堆 / 改期移出堆”时不在 `byID` 里，任何按 ID 的操作都只能变成“按 ID 推迟的闭包”，收尾时再查一次；查的那一刻节点处在什么状态、闭包之间的先后、哪些闭包已经执行，都是隐含状态。每补一种状态组合就多一个表或一个分支。这是**前提问题**，不是细节：只要“活节点不一定能按 ID 找到”成立，下一种组合还会出错。

## 2. 目标结构

文件不变：`infra/base/timer/scheduler.go`（实现）、`scheduler_test.go` / `order_test.go` / `registration_test.go`（测试），新增 `bench_test.go`（基准，只用公开 API，前后共用）。包不拆不并。

**唯一的规则**：`byID` 持有调度器的全部活节点；每个节点带显式状态；`NewTimer*` / `ChangeTimer` / `RemoveTimer` / 回调返回值都在调用的那一刻直接改节点状态与期限（当时取 now、复制 payload）。最外层 Tick 收尾只把等待入堆的节点放回堆，再按入队顺序调用 `onChange`。

删除：`Scheduler.deferred []func()`、`Scheduler.firing []*Node`、`Scheduler.firingChanges`、`type firingChange`、`firingChange()`、`addNode()`。新增：`Node` 的私有字段 `state` / `persisted` / `announced`，`Scheduler.queued []*Node`。

### 2.1 节点状态

| 状态 | 含义 | 在 `byID` | 在堆 | 在 `queued` |
| --- | --- | --- | --- | --- |
| `stateHeap` | 等待到期 | 是 | 是（`index` 有效） | 否 |
| `stateFiring` | 已出堆、回调正在执行（含外层重入帧）；出堆时已向钩子发删除 | 是 | 否 | 否 |
| `stateQueued` | 等最外层 Tick 收尾入堆：Tick 内新建、Tick 内对堆中节点显式改期、触发中节点显式改期 | 是 | 否 | 是（恰一次） |
| `stateDetached` | 不属于调度器：已取消、到期结束、回调 panic 后丢弃，或调用方 / 克隆出的值（零值） | 否 | 否 | 可能残留（收尾跳过） |

“已取消”不单设状态：取消就是立即 `detach`，从 `byID` 删除。之后按 ID 的操作找不到节点，Tick 期间返回 true、无效果——正是原有“取消后的改期不复活”。

钩子视图两个标志（只对 typed 有意义，回调前先改标志）：

- `persisted`：钩子当前持有这个节点（最近一次是 upsert）。堆中节点为真；出堆触发时发删除后为假；Tick 内从堆移出等待放回的节点仍为真（移出不发变化）。
- `announced`：钩子至少收到过一次这个 ID 的 upsert。只有 Tick 内新建、尚未收尾的节点为假。

不变量（实现注释与测试 `checkSettled` 都按这几条）：

1. `byID[id] == n` ⇔ `n.state ∈ {heap, firing, queued}`；`n.state == heap` ⇔ `n` 在堆的 `n.index` 位置。
2. `stateFiring` / `stateQueued` 只在 `running` 期间存在；Tick 外 `byID` 里的节点全部是 `stateHeap`，`queued` 为空。
3. 一个节点在一次最外层 Tick 内至多进 `queued` 一次（进入 queued 只来自 heap / firing / 新建，queued 不会回到这些状态，只会 detach）。

### 2.2 操作 × 状态行为矩阵

“Tick 内”指 `running` 为真（含重入的内层 Tick 与回调里调用）；“立即”指在这次调用里完成。变化回调只对 typed 节点。

| 操作 \ 目标状态 | `stateHeap` | `stateFiring` | `stateQueued` | 不在 `byID`（已取消 / 已结束 / 未知） |
| --- | --- | --- | --- | --- |
| `ChangeTimer` Tick 外 | 改期限、`heap.Fix`、立即 upsert；true | — | — | false |
| `ChangeTimer` Tick 内 | 改期限、立即出堆、入 queued；不发变化；true | 改期限、入 queued（覆盖返回值）；true | 只改期限（最后一次生效）；true | 无效果；true（沿用“已接受”） |
| `RemoveTimer` Tick 外 | 出堆、detach、立即删除；true | — | — | false |
| `RemoveTimer` Tick 内 | 出堆、detach、立即删除；true | detach（出堆时已删除，不再发）；回调返回值不再生效；true | detach；`announced` 为假先补一次 upsert，`persisted` 为真发删除；收尾跳过；true | 无效果；true |
| 回调返回正间隔 | — | 期限 = 本次 Tick 的 now + 间隔，立即入堆并 upsert | 忽略（显式改期优先） | 忽略（已取消） |
| 回调返回 0 / 负 | — | detach（出堆时已删除） | 忽略（显式改期优先） | 忽略 |
| 回调 panic | — | detach（与原先一样不放回，宿主事务回滚） | 保留显式改期，收尾入堆 | 无 |
| `NewTimer*` Tick 外 | 新节点立即入堆、upsert | | | |
| `NewTimer*` Tick 内 | 新节点（期限与 payload 当场固定）进 `byID` 与 queued，`announced` 为假；本次 Tick 不触发 | | | |

最外层 Tick 收尾（`settle`）：

1. `running = false`，取走 `queued`（`s.queued = nil`）。
2. **先落定**：`queued` 里仍是 `stateQueued` 的节点全部入堆（`stateHeap`）；已 detach 的跳过。
3. **再回调**：按入队顺序，对仍为 `stateHeap` 的 typed 节点各发一次 upsert（携带最终期限）。钩子里调用调度器按 Tick 外规则立即生效；它移除的节点在这里被跳过。
4. 全部回调完成后才回收 `queued` 的底层数组（钩子 panic 时直接丢弃，下次重新分配）。

### 2.3 panic 语义

- **回调 panic**：`fire` 的 defer 在节点仍为 `stateFiring` 时 detach 它（不放回，与原先一致）；外层 Tick 的 defer 仍执行收尾，已显式改期 / 新建的节点照样入堆。重入时每一层 `fire` 各自清理自己的节点。
- **出堆时的删除通知 panic**：节点已置为 `stateFiring`，同样由 `fire` 的 defer detach。
- **收尾时钩子 panic**：调度器状态在调用钩子之前已经落定（全部入堆、`running` 已清、`queued` 已取走），panic 只让剩余 upsert 不再回调；没有可重放的东西。宿主应把这次 Tick 当作失败（回滚事务并重建 Scheduler，World 本来就每次重建）。这是选“状态先落定再回调”而不是 defer 清理的原因：后者只能保证清空队列，不能保证中途 panic 时堆与 `byID` 一致。

## 3. 与现有语义逐条对照

| 现有承诺 | 新实现怎样满足 | 回归 |
| --- | --- | --- |
| Tick 内取消仍在堆里、本次也到期的定时器，它不触发 | heap → 立即出堆 detach | `TestRemoveFromAHandlerStopsATimerDueInTheSameTick` |
| Tick 内改期仍在堆里的定时器：本次 Tick 既不按旧期限也不按新期限触发，收尾放回 | heap → 出堆入 queued | `TestChangeFromAHandlerPostponesATimerDueInTheSameTick` |
| 改期后再取消 = 取消；改期两次后一次生效；handler panic 后改期中节点照样放回 | queued → detach / 覆盖期限 / 收尾入堆 | `TestPostponedTimerDuringTickComposesWithLaterOperations` |
| 回调取消自己后返回正间隔不复活 | firing → detach，返回值检查 `state != firing` | `TestDeferredOperationsDuringTickKeepTheirMeaning/a handler cancels itself…` |
| 同一回调新建再取消；Tick 内新建本轮不触发；改自己覆盖返回值 | queued → detach；新建不入堆；firing → queued | `TestDeferredOperationsDuringTickKeepTheirMeaning` |
| 钩子最终视图与调度器一致 | 每个节点的变化在调用时或收尾时按最终状态发出 | `TestChangesSeenByTheHookDuringATickMatchTheFinalState` |
| 重入 Tick 后外层回调取消自己仍生效 | 外层节点在内层 Tick 期间保持 `stateFiring` 且在 `byID` | `TestReentrantTickKeepsTheOuterTickSemantics` |
| 触发中显式改期优先于返回值（0 / 负 / 正，typed 与闭包），typed 完整写回、可重建 | firing → queued，收尾 upsert 完整节点 | `TestFiringTimerExplicitRescheduleOverridesReturn` |
| 连续改期、先取消后改期 / 先改期后取消、其他回调操作外层节点、重入未来 Tick 不提前触发、panic 收尾 | 见矩阵 | `TestFiringTimerOperationsComposeAcrossReentrantTicks` |
| 期限与 payload 在调用时固定 | `add` 当场构造节点 | `TestTimerCreatedDuringTickKeepsItsRegistrationDeadline`、`TestTimerCreatedDuringTickOwnsItsPayloadImmediately`、`TestHandlerThatArmsAChildAndReschedulesItselfUsesCallTime` |
| 同期限按 (End, Priority, ID)；无 handler 删除并计数；非法存量 Tick 清理 | 堆比较与 `fire` 未改 | `order_test.go` 全部、`TestInvalidSavedNodesEmitPersistenceDeletion` |
| Tick 内分配的 typed ID 一定经过一次 upsert（World 用它推进持久 seed，ID 不复用） | 取消 `announced` 为假的节点时先补 upsert | 新增组合测试 `fresh` 行 |

测试只改两处引用被删字段的断言：`TestFiringTimerOperationsComposeAcrossReentrantTicks` 与 `TestPostponedTimerDuringTickComposesWithLaterOperations` 原先断言 `deferred` / `firing` / `firingChanges` 为空，改为 `checkSettled`（`running` 为假、`queued` 为空、`byID` 与堆一一对应且全是 `stateHeap`），表达同一个承诺“收尾后不残留回调控制状态”。

### 3.1 行为差异（只在钩子的中间序列上，最终持久状态相同）

| 编号 | 场景 | 基线 | 重构后 |
| --- | --- | --- | --- |
| D1 | 同一 Tick 内对一个节点显式改期多次 | 收尾按闭包各发一次 upsert | 收尾只发一次，携带最后的期限 |
| D2 | 收尾回调顺序 | 按操作登记顺序 | 按节点首次进入 queued 的顺序 |
| D3 | 取消一个等待放回的节点 | 收尾时先 upsert（新期限 / 新节点）再删除 | 取消调用里立即发：移出堆的发删除；Tick 内新建的发 upsert + 删除；触发中改期的不发 |
| D4 | 收尾时钩子 panic | 闭包与控制表残留，下次 Tick 重放（缺陷） | 不重放（§2.3） |
| D5 | 收尾回调里钩子操作调度器 | 只看到已执行到的闭包，未放回的节点查不到 | 所有节点已入堆，按 Tick 外规则生效 |
| D6 | Tick 内对“尚未分配、本轮稍后才分配”的 ID 操作 | 收尾时可能命中稍后新建的节点 | 无效果（只能靠猜 ID 触发，不视为承诺） |

公开返回值不变：Tick 内对不在 `byID` 的 ID 调 `ChangeTimer` / `RemoveTimer` 仍返回 true（原文档“Tick 内 true 只表示已接受”）。新结构已经能准确知道节点不存在，可以改为返回 false，与 Tick 外一致——这是 API 语义变化，留给维护者决定，本次不做。

## 4. 调用方

`infra/base/timer` 的正式调用方只有 game-demo 模板 `demo/game/entities/world/timer_component.go.tmpl`（`NewScheduler` / `RegisterHandler` / `SetClock` / `NewTimer` / `Tick` / `NextTime` / `NeedsCleanup` / `ReportUnhandledTypes`，每次调用重建，钩子写 DAO 并按 upsert 推进 `TimerSeed`）。Nest 的延迟投递、生成的 Delay、`framework/*` 里的 `time.NewTimer` 都不使用本包（图谱按 `NewTimer` 名字的命中都是标准库 `time.NewTimer`；按 import 路径 `rg` 核对只有该模板）。模板不改。

## 5. 性能预期

- 去掉每次推迟操作一个闭包分配、触发中显式操作的 map 分配与线性查找 `firing` 栈；`ExplicitRescheduleSelf` / `RescheduleOther` / `CreateInHandler` 应减少分配、略快。
- 主路径（回调按返回值重排）多一次状态赋值；`fire` 仍有一个 defer（基线也有）；`Node` 多 8 字节。预期持平。
- 用 `bench_test.go` 的六个基准（1 万常驻定时器、每 Tick 触发 100 个；World 式重建 + Tick），新旧二进制同机交替各 10 轮，benchstat 对照，样本存 `docs/performance/benchmarks/timer-scheduler-refactor-20261017/`。

## 6. 步骤、验证与回退

1. 本方案 + 基准文件 + 收尾 panic 回归（基线红）提交。
2. 改写 `scheduler.go`；两处测试断言改为 `checkSettled`；新增“操作 × 状态”组合表测试（直接 / 经重入 Tick 两种调用位置）。
3. 验证：gofmt；`GOWORK=off go vet ./...`；`infra/base/timer`、`infra/base/clock` `-race -count=20`；根包；全量 `GOWORK=off go test ./...`；生成 game-demo 跑 World 相关测试与 `go build/vet/test ./...`。
4. 文档：`docs/framework/impl/10-time.md` §2 契约、`guide/10-time.md` §3、CHANGELOG `[Unreleased]`。

回退：单包、无格式变化，回退提交即可。

## 7. 修前证据

基线 `1d55586f` 上 `GOWORK=off go test -count=1 -run TestOnChangePanicDuringSettlementDoesNotReplay ./infra/base/timer`：

```
--- FAIL: TestOnChangePanicDuringSettlementDoesNotReplay/child_then_idle_tick (0.00s)
    scheduler_test.go:566: an idle Tick replayed the failed settlement: events=[1:2@1h0m0s] nodes 1 -> 2 ([{ID:2 Type:2 … Delay:1h0m0s index:-1 handler:<nil>} {ID:2 Type:2 … Delay:1h0m0s index:-1 handler:<nil>}])
--- FAIL: TestOnChangePanicDuringSettlementDoesNotReplay/self_reschedule_then_idle_tick (0.00s)
    scheduler_test.go:566: an idle Tick replayed the failed settlement: events=[1:1@1m0s] nodes 1 -> 1 ([{ID:1 Type:1 … Delay:1m0s index:-1 handler:<nil>}])
--- FAIL: TestOnChangePanicDuringSettlementDoesNotReplay/self_reschedule_then_due_tick (0.00s)
    scheduler_test.go:573: after firing at the explicit deadline fired=2 next=1m0s nodes=1, want fired 2 and one node due 1m1s
```

含义：新建的子定时器在下一次空闲 Tick 被第二次压入堆（堆里同一节点两份）；显式改期在空闲 Tick 被重放一次 upsert；残留的控制表让节点在显式期限触发后不按返回值重排，又被重放的闭包按旧期限放回。完整输出见 [red-panic-replay.txt](../performance/benchmarks/timer-scheduler-refactor-20261017/red-panic-replay.txt)。
