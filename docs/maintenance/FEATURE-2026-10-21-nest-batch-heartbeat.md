# 功能设计：Nest 批量心跳作业（选项 C，2026-10-21）

**状态：已实施，随 v1.31.0 交付（未发版）。** 维护者 2026-10-21 批准按推荐做 HB-STRUCTURES §6 的选项 C（同日批准两张按 goroutine 登记的全局 `sync.Map` 改分片表、方案 D 只保留“worker 同锁取下一项”，并追加同 ID 顺序硬约束：冲突的目标当场转单目标作业排在冲突消息之后、不跳拍），代码先合入、心跳投递做成开关 `heartbeat.delivery`；**维护者 2026-10-22 决定 B：缺省改为 `batch`，组大小 8 → 4（时间片 200µs、批 64 不变），合并参数当晚补测**（[性能报告 §3F](../performance/NEST-BATCH-HEARTBEAT-20261021.md#3f-维护者-2026-10-22-决定-b-与参数测量)）。代价：同时段进程 CPU 比单目标少约 42%～73%，同一快池上普通业务消息 p99 约 0.08 → 0.19ms。现行契约见 [impl/02 “批量作业”](../framework/impl/02-nest-entity.md) 与 [impl/10-time §2B](../framework/impl/10-time.md#2b-实体心跳)；以下 §1～§15 保留为设计与实施时的记录。

范围：三大块里的 Nest（`framework/nest`）与心跳投递层（`gameplay/heartbeat` 的 `deliverer`）。遵守 roost-coding 的执行契约：快池不阻塞等待；同 ID 顺序在统一准入处建立；内部续行使用既有准入资格。仓库里没有 `docs/feature/` 与 `docs/agent-skills/`（开发过程资料 2026-10-09 已移入 Git 历史，见 [DOCUMENTS](DOCUMENTS.md)），本篇按现行命名惯例放在 `docs/maintenance/FEATURE-*`，执行契约以本机 roost-coding skill 为准。

读码说明：codebase-memory 图的代际早于本轮，本篇的 `path:line` 全部按本分支源码核对（基线 `07ea71b9`）。

## 1. 为什么要做

[方案 D 的实验](../performance/NEST-BATCH-HEARTBEAT-20261021.md#7-前置实验方案-d批量入队逐个执行不合入) 的结论：心跳每拍一条单目标 `Dispatch`（X）在 50 万次/s 时约 3.8～4.3 核，ssr 式批量（Y）0.7 核；D 把派发队列锁与 worker 停放降了约 8 倍，CPU 只少 11%。贵的是**每条消息在 Nest 里的固定开销**：

| 每条消息都付一次的开销（X 的路径） | 位置 |
| --- | --- |
| worker 的请求上下文 + `ensureNestContext` 的第二个上下文（两次 fctx 表登记） | `dispatch_queue.go` `run`；`nest_dispatch.go:51` |
| 当前消息登记两次（`dispatchNest` 与 `runNestLogic` 各一次，第二次同值跳过）、Guard 作用域登记一次 | `nest_dispatch.go:52`、`:253-254` |
| 慢派发 trace 计时器（每条消息一次 `time.AfterFunc` / `Reset`）、`time.Now` 若干次 | `nest_dispatch.go:49-50`；`execution.go:23` |
| 指标：`nest.dispatch.total` / `nest.dispatch.cost` 每条拼一次标签 map；`nest.handler.lock_hold` 每条一次 | `trace.go:22-36`；`trace.go:411` |
| Fence 检查两次（`lifecycleMu`） | `nest_dispatch.go:87`、`:262` |
| 准入：消息对象、`newDispatchJob`、同 ID 登记、唤醒一个 worker | `dispatcher.go` `TrySendMsg`；`dispatch_queue.go:188` |

Y 不能原样采用（[HB-STRUCTURES §4.1](../performance/HB-STRUCTURES-20261021.md#41-y-在-roost-nest-锁模型下能否成立)）：它绕过目标实体的事务与 Sync 捕获，`QueueMove` 的提交门挂到承载实体的事务上。选项 C 的目标是**保留每个目标自己的 Guard 作用域、事务、Sync 捕获**，把能按批付的固定开销按批付一次。

## 2. 接口

```go
// framework/nest/try_batch.go
type TryBatchTarget struct {
	Handler HandlerName
	ID      int64
	Params  Params
}

// DispatchTryBatch 把一批互不相干的单目标投递交给一个 Nest 作业（按目标的业务池分成至多两个：short / long）。
// 返回 nil 表示已准入。每个目标的结局恰好是三者之一：在本批里执行；同 ID 冲突时当场转成单目标作业、排在冲突作业
// 之后执行；没有执行，handBack(target, reason) 恰好调用一次。
func (mgr *NestMgr) DispatchTryBatch(ctx context.Context, targets []TryBatchTarget, handBack func(TryBatchTarget, error)) error
```

- 不进 `Client` 接口：心跳在 `Bind` 时检查客户端是否实现 `nest.TryBatchDispatcher`，没有实现（测试替身、包装客户端）就退回单目标 `Dispatch`（§12）。
- 整批前置检查与 `Dispatch` 相同、只做一次（`validateClientDispatch`：引擎在跑、未 fence、ctx 未取消、不在 handler / 业务 worker 内）；失败时返回错误、不调用 `handBack`，没有任何目标被准入。
- 逐目标检查：ID 规范化失败 → `ErrInvalidMessage`；Remote 托管的 ID（`shouldPrepareRemoteID`）→ `ErrTryBatchUnsupportedTarget`。这两类在调用方 goroutine 上同步交回。
- 准入失败（快池满 `ErrQueueFull`、停止中 `ErrNestStopped`）：该作业的全部目标同步交回。
- 同 ID 冲突（该 ID 有已准入未完成的作业、实体锁或锁组取不到、实体处于锁组迁移待定）**不交回**：当场转成一条单目标作业（§4），计 `DispatchLaneStats.TryBatchConverted` 与 `nest.dispatch.try_batch.converted.total`。转单目标作业被准入拒绝（等待容量满 `ErrQueueFull`、停止中 `ErrNestStopped`）时才交回。
- 执行期交回（在 worker 上调用，`handBack` 必须不阻塞、不调用 Nest）：`ErrEntityNotFound`（目标不在内存：未加载、已卸载、正在驱逐）、`ErrNestFenced`、`ErrNestCanceled`、`ErrHandlerNotFound`，以及上一条的转单目标被拒。
- handler 执行过（不论提交还是回滚）就不再交回：错误按异步消息的规则记日志（`logAsyncDispatchError`），提交结果未知照常 fence 引擎（`invokeHandlerTransaction`）。

## 3. 执行流程

准入（`dispatchQueue.admitTryBatch`）：一个作业占一个准入名额，容量判定与普通作业相同；**不登记同 ID 顺序**（不写 `tails`，不建依赖边）；进通道就绪链尾、唤醒一个 worker。

worker 取到批量作业（`take` 在 `q.mu` 下，与普通作业同一条就绪链）后：

```text
持 q.mu：安排第 1 个目标——已 fence 则交回；该 ID 在 tails 有登记则当场转单目标作业；否则认领（§4）
放 q.mu，进入本批的请求上下文、登记当前消息（各一次，§11）
for 每个目标:
    不持 q.mu：已认领则执行（新 Guard 作用域 → TryRequire 实体锁 → 事务 → 提交 → 释放锁 → post-release）
    持 q.mu：执行时实体锁 / 锁组取不到则转单目标作业（挂在本目标的认领后面）；撤销认领、像普通作业结束一样唤醒后继；
            超出时间片（§5）则把作业放回就绪链尾、退出循环；否则安排下一个目标
    放 q.mu，调用 handBack（若有）
持 q.mu：结束作业（finishJobLocked），同一次持锁取下一项
```

每个目标在**自己的** Guard 作用域与事务里执行（`entity.NewGuardScope` + `invokeHandlerTransaction`，与 `broadcastDispatch` 逐目标的边界相同，`nest_dispatch.go:609`）：回滚、AfterCommit、Sync 捕获与提交门（`BeginSyncMutation` 在 `runTransaction` 里按本目标建立，`QueueMove` 取到的 `CurrentSyncMutation` 是本目标的）都按单个实体独立。pipelined 持久级别在批内按锁内提交执行（不提前放锁，与广播相同），因为提前放锁的完成泵会持有消息对象，而批内的消息对象逐目标复用。

快池内不阻塞：实体锁只 `TryRequireEntity`（有锁组时组锁同样 Try，`group_lock.go:304` 的现有路径），失败交回；worker 不等任何别的作业；`handBack` 由调用方保证不阻塞。

## 4. 同 ID 顺序（硬约束）

维护者 2026-10-21 问“批量心跳作业会不会打乱同一个 ID 的执行顺序”，并定下两条硬约束：

1. **“在途”包括已准入但还没开始执行的作业。** 判断依据是 `q.tails[id]` 有没有登记（`dispatch_queue.go` `admitLocked` 准入时登记，`releaseIDsLocked` 作业结束时撤销），不是“有没有在执行”。只要该 ID 有任何已准入、尚未完成的作业（包括等前驱的、已转 I/O 准备的），这一拍就不在本批执行，而是**当场转成一条单目标作业**：消息与 `Dispatch` 建的相同（`MsgTypeSingle`、同一 handler 与参数、批的上下文快照），走与单条准入相同的 `admitLocked`，登记到这个 ID 的 `tails` 后面。所以它紧跟在冲突的那条消息之后执行，心跳永远不会插到一条更早准入的消息前面，也不跳拍、不等下一格（维护者 2026-10-21 修正）。转换在 worker 已持有的 `q.mu` 内完成，不阻塞：只有 `admitLocked` 拒绝（等待容量满、停止中）时这一拍才交回、算跳拍（§12）。这一拍在转换后仍算在途（心跳的在途标志由 handler 返回时清），每个登记至多一拍在途不变。
2. **心跳回调执行期间，后来准入的同 ID 消息排在它后面**，不能被 worker 取走后再阻塞等实体锁（那是快池阻塞等待）。做法：worker 在 `q.mu` 下把这个目标**临时登记为该 ID 的尾作业**（实现按 8 个一组在一次持锁里认领，§11；同一实体在同组出现两次时，第二个留到下一组）（认领，claim：`q.tails[id] = 认领标记`，标记是批量作业里预分配的 `dispatchJob`）；之后准入的同 ID 普通作业按正常规则对它建依赖边、等在依赖队列里（不占 worker）；回调结束、提交完成、实体锁与 post-release 都执行完后，在 `q.mu` 下撤销登记，并用与作业结束相同的 `releaseIDsLocked` 让后继就绪、唤醒 worker。认领后实体锁或锁组仍取不到（持有者在 Nest 作业顺序之外，例如别的 handler 的 `Cast`），同样转成单目标作业：它先挂在认领后面，撤销认领时就绪，之后与普通 `Dispatch` 一样取锁——这与 X 下普通消息遇到 `Cast` 持锁时相同，不比 X 多出等待。

**与 ssr“立即 MoveCopy 到队列尾”的对应与区别**：ssr 的 worker 对广播消息逐个目标 `CtxTryLock`，失败就把这一个目标的消息 `MoveCopy` 原样重投到本队列尾（[HB-STRUCTURES §2](../performance/HB-STRUCTURES-20261021.md#2-ssr-心跳的驱动结构读源码)）；roost 的转单目标同样是“当场、只为这一个目标、立即重投”，不交回驱动等下一轮。区别：ssr 重投到队列尾时并不知道冲突的那条消息还要多久，轮到它时若锁仍被占着就再失败、再重投，可能反复空转；roost 把它登记到该 ID 的 `tails` 后面，由派发队列在冲突作业结束时才让它就绪，**只投一次**、执行时锁一定空闲（除非持有者在 Nest 顺序之外），也不占 worker 轮询。

**认领在取锁之前**：维护者原话的顺序是“`TryRequireEntity` 成功后再登记”。按这个顺序，取锁成功到登记之间有一个窗口：这期间准入的同 ID 作业看到 `tails` 为空、直接就绪，被别的 worker 取走后阻塞在实体锁上。所以实现把“检查 tails 为空 + 登记认领”在同一次持 `q.mu` 内完成，再 `TryRequireEntity`；取锁失败时转单目标作业挂在认领后面，再撤销认领（撤销同样唤醒认领期间挂上来的后继）。这比原话的顺序更强：认领与“无在途作业”的检查原子完成，取锁时不可能有已准入的同 ID 普通作业在等这把锁。

与停机排空、Fence 的关系：

- 转成的单目标作业是普通作业：计入 `pending`，停机排空等它；停止开始后（`q.stopping`）不再转换，这一拍交回 `ErrNestStopped`。
- 认领标记不计入 `pending`，它只在所属批量作业执行期间存在；批量作业本身计入 `pending`（准入时 +1，结束时 −1）。认领一定在作业结束之前撤销（每个目标的收尾步骤，panic 也撤销），所以停机排空（`stop` 等 `pending == 0`，`dispatch_queue.go:392`）等到的时刻不会留下认领，挂在认领后面的后继已就绪并计入 `pending`，排空照常等它们执行完。
- Fence 后：准备一组时已 fence 的目标不认领、不转、直接交回；组内每个目标开始执行前再过一次开始检查（§7），所以同组里前一个目标 fence 了引擎后，后面已认领的目标同样交回。认领期间发生 fence（本目标提交结果未知）时，认领仍在收尾时撤销，后继照常就绪，由 `dispatchNest` 开头的开始检查拒绝，与 fence 时队列里的其他作业一样。

**方案 D 与选项 C 在这一点上的区别**：

| | 方案 D（`DispatchBatch`，分支 `perf/nest-dispatch-batch`，不合入） | 选项 C（本功能） |
| --- | --- | --- |
| 同 ID 顺序怎么来 | 每项在准入时登记 `tails`，与单条 `Dispatch` 完全相同：心跳与业务消息按准入先后排成一条链 | 准入时不登记；执行时认领。已有在途作业就跳过（约束 1），执行中准入的排在它后面（约束 2） |
| 心跳遇到同 ID 的业务消息 | 排队等它执行完，心跳不丢，但业务消息若排在心跳后面要等心跳（D 为此加了“提升”） | 心跳当场转成单目标作业排在它后面，同一格内执行、不跳拍；业务消息从不排在一个还没开始的心跳后面 |
| 心跳之间 | 同一登记两拍按准入先后（每个登记至多一拍在途，本来就不会有两拍） | 同上，至多一拍在途由驱动保证；批内两个同 ID 目标（同一实体的两个登记）按批内顺序依次认领，第二个在第一个撤销后才认领 |
| 代价 | 每拍一次 `tails` 登记 + 依赖边 + 一次就绪唤醒（准入侧按块摊，但执行侧每项一个作业） | 每个目标一次认领 / 撤销，按 4 个一组（2026-10-22 前为 8）在一次持锁里做（§11）；只有冲突的目标付一次单目标作业的成本 |

## 5. 普通消息的公平性

- **两条就绪链交替取**：就绪的批量作业进通道的 `tryBatches`，普通作业仍进 `ready`；`take` 在两类之间交替（两类都有时，取过一个批量作业后下一次先取普通作业，反之亦然；只有一类时照取）。初稿是同一条 FIFO，原型探索性运行（5 万实体 100ms、100% 移动）里业务 p99 2.1ms：驱动一格一次准入约 78 个批量作业，之后准入的业务消息要等整格跑完；X 没有这个问题，是因为驱动逐条 `Dispatch` 约 1.4µs 一条，业务消息自然穿插在中间。交替后同一运行业务 p99 0.16ms。这与方案 D 的“批量就绪链交替取”是同一个思路（D 的代码不带过来，这里按批量作业的形态重写，没有 D 的“提升”与“一次多取”：冲突的同 ID 消息由 §4 的认领 / 转单目标处理，不需要提升）。
- **时间片**：一个 worker 连续执行同一批量作业不超过 `tryBatchSlice`（200µs）。每个目标结束后检查，超过且还有剩余目标时，把作业放回就绪链尾（`running` 归还、重新计入就绪），worker 去取链头的下一项；剩余目标由下一个取到它的 worker 接着执行。批大小 64、回调几微秒时一般一片做完；回调慢（D20 估 5～20µs 的移动、慢池实体）时按时间切开，排在后面的普通消息最多等一个时间片加上前面已就绪作业的执行。
- 业务消息遇到正在被认领的同 ID：等这一个目标的回调（不是整批），见 §4。冲突转成的单目标作业按普通作业排队，不提升。
- 批量作业不提升、不插队。业务消息最坏的等待是“它准入时就绪链上已有的作业”，与 X 相同；X 每拍一个作业，C 每 64 拍一个作业，同样数量的心跳在链上占的项数少 64 倍。

## 6. 停机排空

`DispatchTryBatch` 在 `validateClientDispatch` 处拒绝停止中 / 已停止的引擎；已准入的批量作业计入 `pending`，排空照常执行（与已准入的普通消息相同：停机不丢已准入的工作）。执行中的目标在 `nestBaseContext()` 已取消时交回 `ErrNestCanceled`（与 `runNestLogic` 的检查相同）。时间片放回的作业仍计入 `pending`，排空等它；认领见 §4。心跳驱动先于 Nest 停止（`wiring/heartbeat`），停止后到达的交回只清在途标志。

## 7. Fence

**契约**：Fence 允许已经开始执行的 handler 收尾，不允许还没开始的目标执行业务；没执行的目标交回，原因 `ErrNestFenced`（`FenceError()`，`errors.Is` 成立）。

整批前置检查拒绝已 fence 的引擎（`ErrNestFenced`）。已排队的批量作业有两处检查，读同一个原子标志（`NestMgr.fenced`，`Fence` 时置位，不取 `lifecycleMu`）：

1. 准备一组时（`prepareTryBatchGroupLocked`，持 `q.mu`）：已 fence 的目标不认领、不转单目标，交回。这一处只决定本组的安排。
2. 每个目标开始执行前（`tryBatchExec.run` 调用 `NestMgr.checkHandlerStart`，与普通派发的 `dispatchNest` / `runNestLogic`、广播的每个目标共用同一个入口，同时检查取消）：已 fence 的目标不加载、不取锁、不修改实体，交回。**一组执行期间 fence 状态会变**：同组前一个目标的 handler 可以主动 `Fence`，`invokeTryBatchTransaction` 遇到提交结果未知（`ErrCommitIndeterminate`）也会立即 fence 引擎，所以第 1 处读到的状态不能代表执行时。RV-20261022-01（2026-10-22 审查）就是只有第 1 处时，同组后续目标在 fence 之后照常执行。

另外，同组里先执行、取不到实体锁的目标在收尾时才转单目标；此时若同组后面的目标已经 fence 了引擎，`convertTryBatchTargetLocked` 不转、交回 `ErrNestFenced`（转成的作业只会被开始检查拒绝）。开始检查是一次性的、不是临界区：检查之后才被别的 goroutine fence 的，由事务交给 committer 之前的 `refuseCommitAfterFence` 与 WAL terminal 兜底，与普通派发相同。

## 8. Remote

Remote 托管实体的每次写都要在 I/O 池取得带 fence 的写批次（`prepareRemoteWriteBatch`，`nest_dispatch.go` 末尾；`checkRemoteId` 把消息转到 I/O 池），批量作业在快池执行、没有 I/O 阶段，所以**不接受 Remote 托管的目标**（`ErrTryBatchUnsupportedTarget`）。心跳对 Remote 托管实体（`Host.remote != nil`）照旧每拍一条单目标 `Dispatch`：所有权检查一条不变——驱动投递前 `mayOwn`（`driver.go:350`）、Nest 写准入、handler 内 `runBeat` 要求 `LocalOwned` 并在共享模式停掉登记（`host.go:308`）。共享模式本来就不允许挂心跳（D20 决定 ④）。

## 9. 卸载窗口：实例核对 + 令牌

不变：消息参数仍是 `*Reg`（实例令牌），handler 仍是 `beatHandler` → `runBeat`，它核对“目标是登记所属的实例、未 OnDestroy、登记仍在跑”（`host.go:315`）。批量作业只用**已加载**的实体（`loadedGetter`，冷目标交回 `ErrEntityNotFound`），所以比 X 多一条好处：X 的拍若在准入后遇到实体被卸载，会被 Nest 的冷目标规则转去 I/O 池重新加载这个实体（D20 决定 ⑨“接受冷加载一次”）；C 直接交回、驱动按 `not_loaded` 丢弃，不唤醒不在内存的实体。卸载后同 ID 重载的新实例收到旧登记的拍，照旧按 `stale` 丢弃。

## 10. 慢池实体

`DispatchTryBatch` 按目标的业务池（`businessLane`）把一批分成至多两个作业：快池目标进 short 通道、慢池目标进 long 通道，各在本池的 worker 上执行（D20 决定 ⑩“心跳投到实体所在的池”不变）。慢池回调可能较长，时间片在每个目标结束后检查，所以慢池批量作业至少每个目标让一次路；慢池同样不阻塞等锁。

## 11. 哪些开销按批付、哪些逐目标付

| 开销 | 按批一次 | 逐目标 |
| --- | --- | --- |
| 准入（消息对象、作业、容量判定、唤醒） | ✓（一个作业） | |
| 请求上下文（fctx） | ✓（worker 的一个 + 本批一个，带调用方快照与帧号） | |
| 当前消息登记（`currentNestDispatchMsgs`） | ✓：本批一个消息对象，逐目标重置运行状态后复用，登记只做一次 | |
| Guard 作用域登记 | | ✓：每个目标自己的作用域（硬要求），登记表已改分片 |
| 指标 | ✓：`nest.dispatch.total{type="TryBatch"}` 按批累加执行数、`nest.dispatch.cost` 观察整批耗时；`nest.dispatch.try_batch.handed_back.total{reason}` 按批累加 | 只有超过 `slowLockThreshold` 的目标单独计 `nest.handler.lock_hold.slow.total` 并告警 |
| 时间 | 本段开始读一次（时间片起点）；每个目标结束读一次（同时用于时间片与持锁时长） | 业务时间（`fctx.Now` / `Beat.Now`）与 `fctx.Frozen` 派生值（场景时间域）每个目标 `Context.Refreeze` 一次：D22-1 要求每一拍冻结一次，不能整批共用（协调方 2026-10-21 要求，两种投递方式各有断言 `TestEveryBeatFreezesItsOwnBusinessTime`） |
| Fence | 整批检查一次 + 准备每组时读一个原子标志 | ✓：每个目标开始前 `checkHandlerStart`（一个原子标志 + `ctx.Err`，不加锁、不分配） |
| 派发队列锁 | 取作业、结束作业 | 按组：一次持锁安排 4 个目标（2026-10-22 前为 8）（`tryBatchGroup`：认领 / 转单目标 / 交回），一组执行完在同一次持锁里收尾（取锁失败的转单目标、撤销认领）并安排下一组。初稿每个目标一次，探索性 profile 里这把锁的争用（`lockSlow` → 停放 → 调度器空转）约占进程 CPU 的 13%；代价是认领了还没轮到执行的目标上，新准入的同 ID 消息最多多等本组其余目标的执行时间（等在依赖队列里，不占 worker） |
| 事务、Guard、实体锁、Sync 捕获、handler 查表 | | ✓（不能省） |

不做的：慢派发 trace（`startSlowDispatchTraceWatch`）与按消息的 trace 事件不对批内目标发出；按 handler 的 `nest.handler.lock_hold` 直方图不逐目标观察（只计慢的）。这是观测口径的变化，写进 [OBSERVABILITY](../framework/OBSERVABILITY.md) 与 CHANGELOG。

## 12. 心跳投递层与回退

`gameplay/heartbeat` 的 `deliverer` 增加 `batchDeliverer`：

- 投递方式是配置开关（维护者 2026-10-21 决定，原待拍板 ②）：`heartbeat.Config.Delivery` / `Driver.SetDelivery`，`wiring/heartbeat` 的配置项 `heartbeat.delivery: dispatch | batch`（`ConfigSchema` 声明；缺省 2026-10-21 为 `dispatch`，维护者 2026-10-22 决定 B 后为 `batch`，驱动空值 `DefaultDelivery` 同此），Init 时交给驱动一次，第一次 `Start` 之后不能改。`batch` 要求 `Bind` 的客户端实现 `nest.TryBatchDispatcher`，否则 `Bind` / `SetDelivery` 报错，不静默退回单目标。
- 一格的拍分两路：Remote 托管实体的拍逐个单目标 `Dispatch`（§8）；其余按 64 个一批 `DispatchTryBatch`（批大小 `beatBatchSize`，由压测定，§14）。
- 同 ID 冲突由 Nest 当场转成单目标作业（§4），驱动看不到，也不计跳拍；次数看 Nest 的 `nest.dispatch.try_batch.converted.total` / `Stats().Queue.Short.TryBatchConverted`。
- `handBack` 在 worker 上只把（登记, 原因）放进驱动的交回队列（一把短锁的追加），不改驱动状态；驱动在下一格开头（以及本格投递之后，处理同步交回）取走：`ErrEntityNotFound` 清在途、计 `dropped_total{reason="not_loaded"}`；其余原因（`ErrQueueFull` 即转单目标被拒、fence、停止、handler 不存在）清在途、按原规则计 `skipped_total{reason="queue_full"|"rejected"}`、`Beat.Skipped` 加一并限频 Warn。驱动停止中只清在途。
- 每个登记至多一拍在途不变：转成单目标作业的拍、交回队列里的拍都仍在途，这期间到期的同一登记照原规则按 `inflight` 跳拍。

## 13. 测试（先红后绿，变异核对）

新功能没有“修前”，红用变异核对：把实现改成具体的错误版本，确认对应用例变红，失败文本原样记入实施结果。

| 用例 | 保证 | 变异 |
| --- | --- | --- |
| `TestTryBatchConflictRunsAfterAdmittedSameIDJob`：同 ID 的消息已准入、尚未执行（排在 I/O 准备之后、不持锁）时，心跳目标转单目标作业，执行顺序 I/O → 业务 → 心跳，没有交回，转单目标计 1 | 约束 1 | 冲突时直接执行而不排队 |
| `TestTryBatchClaimQueuesLaterSameIDJobBehindTheBeat`：回调执行期间准入的同 ID 消息留在依赖队列（Running=1、BlockedOnPredecessor=1），心跳提交后才执行 | 约束 2 | 不认领 |
| `TestTryBatchLockBusyConvertsToSingleJob`：锁被 Nest 顺序之外的持有者占着时转单目标作业，放锁后执行 | 约束 2 的取锁失败分支 | — |
| `TestTryBatchConversionRejectedIsHandedBack`：转单目标被准入拒绝时才交回 `ErrQueueFull` | §2 | 准入拒绝当作转成功 |
| `TestTryBatchKeepsPerTargetTransactionSemantics`：一项回滚不影响其他、AfterCommit 只对提交项、同实体两目标按批内先后、Sync 捕获到值 | §3 | 各目标共用第一项 Params |
| `TestTryBatchYieldsWorkerAfterTimeSlice`：慢回调的批量作业让出 worker | §5 | 不检查时间片 |
| `TestTryBatchRefusesTargetsAfterFence`：Fence 后排队中的目标交回 `ErrNestFenced`、不执行 | §7 | 准备组与开始检查都不检查 fence |
| `TestTryBatchStopsUnstartedTargetsAfterFenceInsideGroup`（RV-20261022-01 修复时补）：6 个目标跨两组，第一个目标 handler 主动 Fence / committer 返回结果未知，其余 5 个不执行、各交回一次 `ErrNestFenced` | §7 第 2 处 | 每个目标开始前不调用 `checkHandlerStart` |
| `TestTryBatchBusyTargetIsHandedBackAfterFenceInsideGroup`（同上）：同组先取锁失败的目标，在后面的目标 fence 之后交回 `ErrNestFenced`、不转单目标 | §7 收尾转换 | `convertTryBatchTargetLocked` 不检查 fence |
| `TestTryBatchDrainsOnShutdown`：停机排空执行已准入的批量作业，之后拒绝 | §6 | 批量作业不计入 pending |
| `TestTryBatchHandBackReasonsAndPoolRouting`：非法 ID / 空 handler / Remote / 不在内存 / handler 不存在各交回一次；慢池目标在 long 通道；批内 Await 被拒 | §2 §8 §10 | 慢池目标进 short；Await 不拒绝批量作业 |
| `TestTryBatchAlternatesWithOrdinaryJobs`：批量作业积压后准入的普通消息至多排在一个批量作业之后，反之亦然 | §5 | 同一条 FIFO；两类都有时总先取普通作业 |
| `TestTryBatchReadsOnlyLoadedTargets`：不在内存的目标交回 `ErrEntityNotFound`，Get 一律带 `LoadedEntitiesOnly` | §9 | 去掉执行器的 `loadedGetter` 仍绿（强制点是业务 worker 身份，见用例注释），契约检查 |
| 心跳 `TestBeatOfAnUnloadedInstanceIsDroppedWithoutColdLoad`：准入后卸载、未重载的实例，拍按 `not_loaded` 丢弃、不冷加载、不算跳拍 | §9 §12 | 交回的“不在内存”按跳拍计 |
| 心跳 `TestConflictingBeatRunsRightAfterTheAdmittedMessage`：业务消息已准入未执行时，拍在它之后、同一格内执行，不跳拍，转单目标计 1；转换后仍在途，下一周期按 inflight 跳过 | 约束 1、至多一拍在途 | ① Nest 冲突时直接执行而不排队；② 驱动不看在途标志 |
| 心跳 `TestConflictingBeatIsSkippedOnlyWhenAdmissionRefuses`：转单目标被拒时才计 `skipped{queue_full}` 并 Warn | §12 | 交回只清在途不计数 |
| 心跳 Remote 用例（`remote_test.go`）不改，照样通过 | §8 | Remote 也走批量作业 |

## 14. 压测与达标判据

夹具与门控沿用 `gameplay/heartbeat/internal/bench/delivery_test.go`（[HB-STRUCTURES §4.2](../performance/HB-STRUCTURES-20261021.md#42-负载与测量口径)）：5 万实体、100ms 登记、10ms 分辨率、1 个驱动、5000 条/s 业务消息、快池 8 worker，`HB_MODE` 新增 `c`。同机比较 X、Y、C，移动比例 100% / 20%，各至少 3 次；profile 与正式测量分开跑。普通 Nest 微基准（方案 D §6 的同一组）改前改后交替对比。

达标：进程 CPU 明显接近 Y；业务 p99 不差于 X；跳拍、准入失败、快池拒绝为 0 或可解释；普通短 handler 不退化。不达标就如实记录，不合入核心改动。

## 15. 待拍板（附推荐）

1. **批大小**：推荐 64（与 ssr 一致；按压测结果定，见性能报告）。 2026-10-22 对照（性能报告 §3B）：32 / 64 / 128 进程 CPU 相同，p99 0.195 / 0.254 / 0.282ms，32 的驱动 CPU +30%；64 不用改。
2. ~~单目标 `Dispatch` 回退~~：维护者 2026-10-21 已定——加配置开关 `heartbeat.delivery: dispatch | batch`，缺省 `dispatch`（§12）；2026-10-22 决定 B 改为缺省 `batch`，`dispatch` 保留为可选项。
3. **`DispatchTryBatch` 是否导出给业务**：A 导出但只在 PACKAGES / impl 里说明“目前只有心跳用；冲突的目标转单目标作业”（推荐，接口本身是通用的，内部化反而要在 heartbeat 里绕）；B 改成 `internal` 只给 `gameplay/heartbeat`。
4. **时间片 200µs**：推荐先用 200µs，按慢回调场景再调。 2026-10-22 对照（性能报告 §3A、§3B）：100 / 200 / 400µs 的业务 p99 0.19～0.22 / 0.24～0.25 / 0.33ms，CPU 不变；组大小 4 / 8 / 16 的 p99 0.187 / 0.235 / 0.316ms。改 100µs（或组 4）各降约 20%，两项合起来未测。维护者 2026-10-22：先用组 4、时间片保持 200µs（`tryBatchGroup` 8 → 4），合并组当晚补测（性能报告 §3F）。

## 16. 实施结果

分支 `perf/nest-batch-heartbeat`（基于 `07ea71b9`），压测、profile 与变异核对原文见 [NEST-BATCH-HEARTBEAT-20261021](../performance/NEST-BATCH-HEARTBEAT-20261021.md)。

**源码定位**

| 内容 | 位置 |
| --- | --- |
| worker 同锁取下一项（从方案 D 拆出） | `framework/nest/dispatch_queue.go` `work` / `run` |
| 分片登记表 | `infra/base/goroutine/local_map.go` `LocalMap`；`framework/entity/entity_guard.go` `guardScopes`；`framework/nest/group_transition.go` `currentNestDispatchMsgs` |
| 准入 | `framework/nest/try_batch.go` `DispatchTryBatch`、`trySendTryBatch`、`admitTryBatch`；`dispatch_queue.go` `admit` 拆出 `admitLocked` |
| 两条就绪链交替 | `dispatch_queue.go` `take`（`tryBatches` / `preferOrdinary`）、`enqueueReady` |
| 同 ID 顺序（约束 1 / 2） | `try_batch.go` `prepareTryBatchGroupLocked`（fence → 冲突转单目标 → 认领）、`convertTryBatchTargetLocked`（`admitLocked`）、`finishTryBatchGroupLocked` / `releaseTryBatchClaimsLocked`；`dispatch_queue.go` `finishJobLocked` 拆出 `releaseIDsLocked` |
| 执行 | `try_batch.go` `runTryBatchLocked`（按组、时间片 `yieldTryBatchLocked`）、`tryBatchExec`（`beginTryBatch` / `resetMsg` / `run` / `runLocked` / `end`）、`invokeTryBatchTransaction`、`tryLockTryBatchTarget` |
| Fence 原子标志与开始检查 | `framework/nest/nest.go` `NestMgr.fenced`（`Fence` 置位）；`nest_dispatch.go` `checkHandlerStart`（RV-20261022-01 修复时加，普通派发、广播、批量作业逐目标共用） |
| Await 拒绝 | `framework/nest/await.go`（`MsgTypeTryBatch`） |
| 统计 | `framework/nest/stats.go` `DispatchLaneStats.TryBatchJobs` / `TryBatchConverted` |
| 心跳投递层 | `gameplay/heartbeat/driver.go` `batchDeliverer`、`Driver.handBack` / `drainReturned` / `skip`，`Bind` 的选择 |
| 压测夹具 | `gameplay/heartbeat/internal/bench/delivery_test.go` `HB_MODE=c` |

**与评审稿不同处**（实施中按实测改的，§4、§5、§11 已改写）

1. 维护者在实施中修正约束 1：冲突的目标不交回驱动等下一格，而是当场转单目标作业排在冲突消息之后（§4）。初稿的 `ErrTryBatchBusy` 与驱动侧的 busy 重投已删除。
2. 就绪链由一条改为两条交替（§5）：同一条 FIFO 时探索性运行业务 p99 2.1ms。
3. 认领由逐目标改为 8 个一组（§11）：逐目标一次 `q.mu` 的争用在探索性 profile 里约占 13% CPU。
4. 认领先于取锁（§4“认领在取锁之前”），比原话的顺序少一个窗口。
5. 时间快照不按批付：任务书原本把“时间快照”列为按批付一次的开销；D22-1（业务时间一律取自 fctx、每条消息 / 每一拍冻结一次）合入后，协调方要求批量方式下 `Beat.Now` 取这一拍冻结的 fctx 时间、挂了时间域的登记取这一拍的域时间。实现为每个目标 `fctx.Context.Refreeze`（新增：重新冻结 Now / NowMilli、清 `Frozen`），代价是每个目标一次业务时钟读取。
6. 投递方式做成开关、缺省关闭（维护者 2026-10-21 决定 A，§12）；2026-10-22 决定 B 改为缺省开启，组大小 8 → 4（§4、§11 已改）。

**变异核对**：23 条，22 条变红；1 条（去掉执行器的 `loadedGetter`）仍绿，原因是“只读已加载实体”的强制点在 `entity.LoadedEntitiesOnly` 对业务 worker 身份的判断，执行器的包装只是防御，该用例按契约检查保留。失败文本原样见性能报告 §5 与样本目录的 `mutations.txt`。
