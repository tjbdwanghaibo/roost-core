# 核心：Nest 调度与实体：实现与维护

适用版本：v1.29.0 及当前主线（三池 / Await 随 v1.26.0 发布，之后变化见 [CHANGELOG](../../../CHANGELOG.md)）。[设计与使用](../guide/02-nest-entity.md)

## 如何阅读

同一个玩家可能同时收到购买、聊天、领取奖励等请求。Nest负责安排执行，并保护需要一起修改的数据。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`framework/nest`、`framework/entity`、`gameplay/actionflow`、`infra/base/fctx`、`infra/base/lock`、`infra/base/worker`、`infra/base/goroutine`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 同 ID 消息准入顺序和冷目标改道不能丢位置。
2. memory 本地持久写拒绝，rollback=none 不承诺恢复。
3. pipelined 不可回滚边界早于持久确认，外化必须等确认。
4. 长短业务池不能阻塞等待池任务或 I/O；同一 GuardScope 持有并释放锁。I/O 池禁止取得 Guard。
5. Await 先预留 I/O 容量，段结束释放 Guard 和 tail 后执行查询；恢复段按完整目标重新准入。首版只支持无回滚的 memory handler。
6. 声明目标在权威里不存在（loader 报告 `entity.ErrAuthorityEntityNotFound`，DataEngine 的 `ErrEntityAggregateNotFound` 满足它）时，回复包装成 `errcode.ErrEntityNotFound`（稳定码 `errcode.CodeEntityNotFound = 2`）；删过档（`entity.ErrAuthorityEntityDeleted`，DataEngine 的 `ErrEntityAggregateDeleted` 满足它，**不**满足 not found）包装成 `errcode.ErrEntityDeleted`（`CodeEntityDeleted = 3`，D10）。客户端得到该码而不是 `CodeInternal`，生成 endpoint 因此不按 ERROR 记录；原错误仍在链上，`errors.Is` 照旧判别。强制点是 [io_load.go](../../../framework/nest/io_load.go) 的 `entityStateError`（`targetLoadError` 调用它），在 `singleDispatch` / `dispatchMany` 取目标与 I/O 准备的多目标 `GetMany` 三处调用；链上已有业务码不改写，handler 内业务自己返回的错误不经过它。2026-10-16 维护者决定（DESIGN-TODO D7 方案 C），2026-10-17 删档拆出独立码（D10 方案 B，范围只守 GetOrCreate；handler 内直接 Create 不读存储，必须用 id-gen 新 ID）。

## 2A. 事务实现走读

[nest/rollback.go](../../../framework/nest/rollback.go)的durableCommit先区分memory无effect分支。该分支检查fence及本地持久写，不能准备一份没有任何持久落点的记录后仍返回成功。其他分支先prepareCommitRecord；空记录可返回，非空记录检查外层回滚域与fence，再要求TransactionCommitter。Commit返回未知保持未知，其他错误包装ErrCommitRejected，成功后acceptPersistence。

这一函数只覆盖同步提交分支，不能据此推断pipelined所有等待都在其中：pipelined的Enqueue/票据、解锁及完成回调必须沿执行收尾路径检查。修改时同时验证拒绝是否回滚、未知是否fence、成功回复是否晚于相应确认点。

事务participantOrder/participantChanges记录本事务变化；refuseMemoryPersistentWrite排除Remote认领的参与者和mutation，并对剩余本地内容给出实体/字段错误。不能以DAO上一次遗留dirty替代这些事务局部集合。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

### 三池调度与续行

[dispatch_queue.go](../../../framework/nest/dispatch_queue.go) 使用一个 lanes 数组，每项聚合配置、等待/就绪链、运行数量、内部续行、唤醒条件和统计。短业务、I/O、长业务三条通道共用 mutex、tails、依赖链、pending；不能各建一套 ID 顺序。

worker 循环（`work`）在循环顶部持锁：结束一项工作（`finishJobLocked`）后在同一次持锁里接着 `take` 下一项，没有工作才在通道的 `wake` 上停放。消息登记的出队后动作（`afterQueue`，await 的 I/O 启动）仍在本项结束、ID 尾撤下之后、不持锁、本 worker 执行下一项之前运行；执行本身在 `run` 里，请求上下文选项每个 worker 建一次。改前每条消息取两次锁；从 D20 方案 D 原型拆出（[实验记录](../../performance/NEST-BATCH-HEARTBEAT-20261021.md#7-前置实验方案-d批量入队逐个执行不合入)）。

**每个 handler 开始前的检查**（`NestMgr.checkHandlerStart`，[nest_dispatch.go](../../../framework/nest/nest_dispatch.go)）。普通派发（`dispatchNest` 阶段开始、`runNestLogic` 在 handler 之前）、广播的每个目标、批量作业的每个目标共用这一个入口：引擎已 fence 返回 `FenceError()`（包裹 `ErrNestFenced`），请求上下文已取消返回 `ErrNestCanceled`。只读 `NestMgr.fenced`（`Fence` 时置位的原子标志）与 `ctx.Err`，不加锁、不分配。Fence 允许已经开始的 handler 收尾，不允许还没开始的目标执行业务；一个作业里有多个目标时（广播、批量作业），前一个目标可以在执行中途 fence 引擎（handler 主动 `Fence`、提交结果未知），所以**每个目标开始前都要调用**，不能用作业或一组开始时读到的状态代替（RV-20261022-01：修前批量作业只在准备一组时读一次，同组后续目标照常执行；广播只在作业开始时查一次，同样如此）。它是一次性检查、不是临界区：检查之后才被别的 goroutine fence 的，由 `refuseCommitAfterFence` 与 WAL terminal 兜底。

实体有效性依赖取到的实体，不在这个函数里，但每条路径对每个目标都做同样两步：只读已加载实体并 `Touch`；取锁时 `RequireEntity` / `TryRequireEntity` 拒绝已清理、已摘除的实例，有锁组时 `validateDispatchGroupSnapshots` 核对锁组没变。失败后的回答按路径不同：普通派发可原位转 I/O 准备，批量作业交回，广播跳过。回归：`TestTryBatchStopsUnstartedTargetsAfterFenceInsideGroup`（handler 主动 Fence 与 committer 返回结果未知两种触发）、`TestTryBatchBusyTargetIsHandedBackAfterFenceInsideGroup`、`TestBroadcastSkipsUnstartedTargetsAfterFence`。

Guard 作用域（`entity.guardScopes`）与当前派发消息（`nest.currentNestDispatchMsgs`）两张按 goroutine 登记的表是 `goroutine.LocalMap`：按 goroutine ID 分 64 片、每片一把读写锁，与 fctx 的请求上下文表同一做法。不要换回 `sync.Map`：worker 的 goroutine ID 固定、每条消息先存后删，`sync.Map` 在同一内部节点上的 Store / Delete 争用会让空闲 P 偷活空转（方案 D 实验 §5）。

**批量作业**（[try_batch.go](../../../framework/nest/try_batch.go)，`NestMgr.DispatchTryBatch`，D20 选项 C，设计 [FEATURE-2026-10-21-nest-batch-heartbeat](../../maintenance/FEATURE-2026-10-21-nest-batch-heartbeat.md)）。Nest 侧入口与派发队列支持无条件存在（没有批量作业时，`take` 只多一次“批量链是否为空”的判断）；心跳是否用它由 `heartbeat.delivery` 决定，缺省用（维护者 2026-10-21 先合入、缺省关闭，2026-10-22 决定 B 改为缺省开启）。上面的 worker 同锁取下一项与分片登记表对全部消息生效。必须保持：

1. 准入（`admitTryBatch`）占一个名额、不写 `tails`；就绪后进通道的 `tryBatches`，`take` 在普通作业与批量作业之间交替（`preferOrdinary`），不与普通作业共用一条 FIFO。
2. 同 ID 顺序在 `q.mu` 下逐组安排（`prepareTryBatchGroupLocked`，一组 `tryBatchGroup` = 4 个；2026-10-22 前为 8，为压低普通消息 p99 改小，`TestTryBatchParametersAreTheMeasuredDefaults` 钉住，依据见 [性能报告 §3F](../../performance/NEST-BATCH-HEARTBEAT-20261021.md#3f-维护者-2026-10-22-决定-b-与参数测量)）：`tails[id]` 有登记（含已准入未执行）→ `convertTryBatchTargetLocked` 当场建一条与 `Dispatch` 相同的 `MsgTypeSingle` 消息、走 `admitLocked` 排在冲突作业之后；否则把 `claims[i]` 写进 `tails[id]` 认领，**认领先于 `TryRequireEntity`**。执行时实体锁 / 锁组取不到同样转单目标（先挂在认领后面）。一组结束在同一次持锁里转换取锁失败的目标、`releaseTryBatchClaimsLocked` 撤销认领（与作业结束共用 `releaseIDsLocked` 唤醒后继），再安排下一组。只有转单目标被 `admitLocked` 拒绝、目标不在内存、fence、取消、handler 不存在才交回调用方。引擎已 fence 时不转单目标（`convertTryBatchTargetLocked` 返回 `errTryBatchFenced`，交回 `ErrNestFenced`）：转成的作业只会被开始检查拒绝；这覆盖同组前面的目标取锁失败、收尾时才转换，而同组后面的目标已经 fence 了引擎的情况。
3. 每个目标开始前先过 `checkHandlerStart`（见上文“每个 handler 开始前的检查”）；`prepareTryBatchGroupLocked` 读的 fence 只决定本组的安排（已 fence 不认领、不转），不代表执行时。每个目标自己的 `entity.NewGuardScope` + `invokeTryBatchTransaction`（与 `invokeHandlerTransaction` 同样的 pipelined 白名单与结果未知 fence，不传提前放锁闭包、pipelined 锁内提交）；实体只从 `loadedGetter` 取、只 `TryRequire`。逐目标复用一个 `MsgTypeTryBatch` 消息对象（`resetMsg` 清零运行状态），当前消息只登记一次；每个目标开始前恢复请求上下文快照（`ctxSnapshot`）并 `fctx.Context.Refreeze`：业务时间（`fctx.Now`，心跳的 `Beat.Now`）与 `fctx.Frozen` 派生值（场景时间域的域时间）每个目标各冻结一次（D22-1），不是整批共用。`Await` 拒绝 `MsgTypeTryBatch`。
4. 时间片 `tryBatchSlice` = 200µs：一组结束后检查（所以一个 worker 一次被占住的时长约为“时间片 + 一组的执行时间”，这是普通消息 p99 的主要来源），超出则 `yieldTryBatchLocked` 放回 `tryBatches` 尾；pending 不变，认领已撤销。
5. 交回在放锁后调用（`collectHandBacks` / `flushHandBacks`），回调 panic 只记日志。执行循环意外退出时剩余目标以 `errTryBatchAborted` 交回、残留认领撤销。

[await.go](../../../framework/nest/await.go) 在当前 memory 段内预留 I/O 名额。pending 包含尚未启动的预留，保证 Shutdown 不会提前退出；普通 I/O 与 Await 共用容量，预算包含正在运行的 I/O。前段失败归还预留；成功则在队列释放 tail 后启动 work。resume 使用匿名 handlerEntry，复用正式加载、Guard、同步与回复链路，不把旧实体指针带到 I/O。

新目标和旧目标没有跨段原子性。当前实现只允许无回滚 memory handler，动态目标错误、取消、停机及过载均有明确终态。具体用法、旧 Cast 迁移、验证命令和限制见 [三池与 Await](../NEST-AWAIT.md)。早期两通道字段聚合的性能样本保留在 [调度对照基准](../../performance/NEST-DISPATCH-BENCHMARK.md)，不能拿它证明三池或反射恢复的性能。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### actionflow

7 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [action_runner.go](../../../gameplay/actionflow/action_runner.go) | `ActionSnapshot`、`ActionRunnerHooks`、`ActionRunnerConfig`、`ActionRunner` |
| [action_types.go](../../../gameplay/actionflow/action_types.go) | `ActionKind`、`ActionGroup`、`MissionKind`、`ActionStatus`、`MissionStatus`、`ActionResult`、`ActionReason`、`MissionNextMode`、`MissionNext`、`MissionStep`、`MissionPlan`、`MissionInfo` |
| [mission_runner.go](../../../gameplay/actionflow/mission_runner.go) | `MissionRunnerHooks`、`MissionRunnerConfig`、`MissionRunner` |
| [plan.go](../../../gameplay/actionflow/plan.go) | `PlanMission` |
| [registry.go](../../../gameplay/actionflow/registry.go) | `ActionBuilder`、`MissionBuilder`、`Registry` |
| [runtime.go](../../../gameplay/actionflow/runtime.go) | `ActionContext`、`Action`、`TickIntervaler`、`ActionList`、`MissionContext`、`Mission`、`MissionManager`、`MissionRuntimeSetter` |
| [heartbeat.go](../../../gameplay/actionflow/heartbeat.go) | `BeatConfig`、`BeatRunner`（心跳驱动的 runner，契约见 [impl/10-time §2C](10-time.md#2c-移动与动作的心跳驱动)） |
| [behavior.go](../../../gameplay/actionflow/behavior.go) | `Behavior`、`BehaviorConfig`、`Observer`（ActionList 的默认装配，契约见 [impl/10-time §2D](10-time.md#2d-行为控制behavior取代冲突冻结与状态)） |

### entity

28 个实现文件、16 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [category_order.go](../../../framework/entity/category_order.go) | `EntityCategoryDef` |
| [comp_sort.go](../../../framework/entity/comp_sort.go) | 函数/方法或内部实现；见源码 |
| [component_base.go](../../../framework/entity/component_base.go) | `ComponentBase` |
| [entity.go](../../../framework/entity/entity.go) | `IThreadSafeEntity`、`IThreadSafeEntityBase`、`Getter`、`LoadedChecker`、`AggregateLoader`、`IDirty`、`DaoInterface`、`DatabaseScope`、`DatabaseScopedDao`、`PersistedDaoLoader`、`Guardable`、`EntityCreateParam` |
| [entity_base.go](../../../framework/entity/entity_base.go) | `EntityBase`、`ComponentInterfaceBase`、`ComponentManager`、`ComponentFactory`、`DaoManager` |
| [entity_factory.go](../../../framework/entity/entity_factory.go) | `EntityBuilderFunc`、`DaoBuilderFunc`、`EntityBuilderParam` |
| [entity_group.go](../../../framework/entity/entity_group.go) | `EntityGroupTransitionState` |
| [entity_guard.go](../../../framework/entity/entity_guard.go) | `EntityGuard`、`GuardScope` |
| [entity_kind.go](../../../framework/entity/entity_kind.go) | `EntityCategory`、`EntityKind`、`EntityKindCategory`、`EntityKindDef`、`ComponentType`、`EntityDestroyReason` |
| [entity_manager.go](../../../framework/entity/entity_manager.go) | `EntityManager`、`EntityManagerOption`、`DeleteAdmission`、`DeleteAdmitter`；`WithLocked`（持实体锁执行、只对当前托管实例，D16 卸载屏障用它） |
| [entity_remote.go](../../../framework/entity/entity_remote.go) | `IThreadSafeRemoteEntity`、`RemoteEntityBase` |
| [idgen.go](../../../framework/entity/idgen.go) | `IDGen` |
| [local_executor.go](../../../framework/entity/local_executor.go) | 函数/方法或内部实现；见源码 |
| [manager_access.go](../../../framework/entity/manager_access.go) | `ManagerAccess` |
| [manager_factory.go](../../../framework/entity/manager_factory.go) | `CreatedEntityCapturer` |
| [policy.go](../../../framework/entity/policy.go) | `RemotePolicy`、`EntityLifetime` |
| [remote_manager.go](../../../framework/entity/remote_manager.go) | `IRemoteEntityLoader`、`IRemoteEntityLocalLookup`、`IRemoteEntityUnloader`、`IRemoteEntityBackend`、`IRemoteEntityOwnershipStore`、`RemoteEntityMarkerLease`、`IRemoteEntityManager` |
| [remote_mirror.go](../../../framework/entity/remote_mirror.go) | `RemoteObservation`、`RemoteSnapshotRead`、`RemoteSnapshotReadOnly`、`RemoteMirrorSpec`、`RemoteMirrorValue`、`RemoteMirrorReader` |
| [remote_protocol.go](../../../framework/entity/remote_protocol.go) | `RemoteOwnershipState`、`RemoteVersionVector`、`RemoteWriteMode`、`RemoteWriteLease`、`RemoteTransactionID`、`RemotePersistChange`、`RemotePersistChangeSource`、`RemoteDeleteIntentSource`、`RemoteTransactionOutcome`、`RemoteSnapshotRecord`、`RemoteDataMutation`、`RemoteDataDelete`、`RemoteCommit`、`RemoteCommitReceipt`、`RemoteCommitState`、`RemoteCommitStatus`、`IRemoteCommitParticipant`、`IRemoteCommitter`、`IRemoteAtomicBatchCommitter`、`IRemoteCommitOutbox`、`RemoteOutboxCursor`、`IRemoteCommitOutboxPager`、`IRemoteStorageInitializer`、`IRemoteSnapshotPublisher`、`IRemoteSnapshotLoader`、`RemoteWriteBatch`、`RemoteOutcomeDeferrer`、`RemoteWriteBatchManager`、`RemoteOwnershipManager`、`RemoteSnapshotInterest`、`RemoteSnapshotInterestManager`、`RemoteCommitApplier`、`RemoteSnapshotReader` |
| [remote_snapshot.go](../../../framework/entity/remote_snapshot.go) | `RemoteReadConsistency`、`RemoteSnapshotKey`、`FrozenRemoteSnapshotPayload`、`RemoteSnapshotEnvelope`、`RemoteSnapshotLoader`、`RemoteSnapshotCacheConfig`、`RemoteSnapshotCache`、`RemoteSnapshotReplica`、`RemoteSnapshotBootstrapStats`、`RemoteSnapshotVersionedDeleter` |
| [remote_snapshot_codec.go](../../../framework/entity/remote_snapshot_codec.go) | `RemoteSnapshotDecodeFunc`、`RemoteSnapshotDeltaFunc`、`RemoteChecksum` |
| [remote_view.go](../../../framework/entity/remote_view.go) | `RemoteAcquireMode`、`RemoteReadOption`、`RemoteSnapshotSource`、`RemoteSnapshot`、`RemoteViewRef` |
| [resolver.go](../../../framework/entity/resolver.go) | `EntityIDMeta` |
| [subject_sync.go](../../../framework/entity/subject_sync.go) | `SyncProfile`、`FrozenSyncPayload`、`SubjectSyncPacker`、`SubjectSyncPackFunc`、`SubjectSyncDirtyNotifier`、`SubjectSyncCreateParam`、`EntitySyncCreateParam`、`EntitySyncBuilderParam`、`SubjectSyncUpdate`、`SubjectSyncState`、`PreparedSubjectSync`、`PreparedSubjectSyncBatch` |
| [sync_commit.go](../../../framework/entity/sync_commit.go) | `SyncCommitObserver`、`SyncSubjectRetractor`、`SyncChangeCollector`、`SyncMutation`、`SyncChangeMapper` |
| [sync_frozen.go](../../../framework/entity/sync_frozen.go) | 函数/方法或内部实现；见源码 |
| [sync_view.go](../../../framework/entity/sync_view.go) | `SyncView`、`SyncViewSet`、`NamedSyncView` |
| [unload_resync.go](../../../framework/entity/unload_resync.go) | `UnloadedSubjectSync`、`UnloadResyncConfig`、`UnloadResyncStats` |

### fctx

3 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [context.go](../../../infra/base/fctx/context.go) | `Context`、`ContextSnapshot`、`TraceMeta`、`RequestMeta`、`Option` |
| [runtime_context.go](../../../infra/base/fctx/runtime_context.go) | 函数/方法或内部实现；见源码 |
| [worker.go](../../../infra/base/fctx/worker.go) | 函数/方法或内部实现；见源码 |

### goroutine

7 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [goid_prod.go](../../../infra/base/goroutine/goid_prod.go) | 函数/方法或内部实现；见源码 |
| [goid_race.go](../../../infra/base/goroutine/goid_race.go) | 函数/方法或内部实现；见源码 |
| [local_map.go](../../../infra/base/goroutine/local_map.go) | `LocalMap` |
| [mpsc_queue.go](../../../infra/base/goroutine/mpsc_queue.go) | `MPSCQueue` |
| [parallel.go](../../../infra/base/goroutine/parallel.go) | `ParallelMapOption` |
| [safe_func.go](../../../infra/base/goroutine/safe_func.go) | 函数/方法或内部实现；见源码 |
| [task_pool.go](../../../infra/base/goroutine/task_pool.go) | `Task`、`TaskFunc`、`TaskPoolConfig`、`TaskPool` |

### lock

3 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [lock.go](../../../infra/base/lock/lock.go) | `Mutex`、`LockIdGetter` |
| [lock_manager.go](../../../infra/base/lock/lock_manager.go) | `MutexFactory`、`LockManager` |
| [reentrant_mutex.go](../../../infra/base/lock/reentrant_mutex.go) | `ReentrantMutex` |

### nest

24 个实现文件、35 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [cast.go](../../../framework/nest/cast.go) | `CastTarget` |
| [client.go](../../../framework/nest/client.go) | `Client` |
| [dispatch_queue.go](../../../framework/nest/dispatch_queue.go) | `WorkerPoolConfig` |
| [dispatcher.go](../../../framework/nest/dispatcher.go) | `Dispatcher` |
| [dispatcher_series.go](../../../framework/nest/dispatcher_series.go) | 函数/方法或内部实现；见源码 |
| [execution.go](../../../framework/nest/execution.go) | 函数/方法或内部实现；见源码 |
| [group_lock.go](../../../framework/nest/group_lock.go) | `EntityLockGroupScope` |
| [group_transition.go](../../../framework/nest/group_transition.go) | `GroupTransitionRequest`、`GroupTransitionContinuation`、`GroupTransitionOptions`、`GroupTransitionOption` |
| [handler.go](../../../framework/nest/handler.go) | `HandlerOptionParam`、`HandlerOption`、`BaseHandler` |
| [msg.go](../../../framework/nest/msg.go) | `MsgType`、`Msg`、`TickMsg` |
| [nest.go](../../../framework/nest/nest.go) | `NestMgr`、`HandlerName`、`Params`、`NestOpts`、`NestOption`、`SendOpt` |
| [nest_dispatch.go](../../../framework/nest/nest_dispatch.go) | 函数/方法或内部实现；见源码 |
| [persist_change.go](../../../framework/nest/persist_change.go) | `PersistChange`、`MutationParticipant` |
| [pipelined_completion.go](../../../framework/nest/pipelined_completion.go) | 函数/方法或内部实现；见源码 |
| [remote_access.go](../../../framework/nest/remote_access.go) | `RemoteAcquireMode`、`RemoteAccess`、`RemoteAccessProvider`、`RemoteSnapshotResolver`、`RemoteKey`、`RemoteScopeProvider`、`RemoteDefaultTTLMillisProvider` |
| [remote_dispatch.go](../../../framework/nest/remote_dispatch.go) | 函数/方法或内部实现；见源码 |
| [rollback.go](../../../framework/nest/rollback.go) | `RollbackPolicy`、`HandlerMeta`、`RollbackParticipant`、`RollbackTx`、`PersistFieldNamer`、`RollbackSnapshotter` |
| [io_load.go](../../../framework/nest/io_load.go) | 函数/方法或内部实现；见源码 |
| [stats.go](../../../framework/nest/stats.go) | `DispatcherWorkStats`、`DispatchLaneStats`、`DispatchQueueStats`、`DispatcherStats` |
| [ticker.go](../../../framework/nest/ticker.go) | `TickCallbackName`、`Ticker` |
| [trace.go](../../../framework/nest/trace.go) | 函数/方法或内部实现；见源码 |
| [try_batch.go](../../../framework/nest/try_batch.go) | `TryBatchTarget`、`TryBatchDispatcher` |
| [transaction.go](../../../framework/nest/transaction.go) | `DurabilityPolicy`、`TransactionID`、`EntityMutation`、`Effect`、`CommitRecord`、`CommitFence`、`TransactionCommitter`、`TransactionReleaseNotifier`、`LocalExecutorBinder`、`CommitTicket`、`PipelinedTransactionCommitter`、`CommitParticipant` |

### worker

2 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [pool.go](../../../infra/base/worker/pool.go) | `PoolConfig`、`PoolStats`、`Pool` |
| [worker.go](../../../infra/base/worker/worker.go) | `Task`、`Worker` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Memory`、`Test.*Pipelined`、`Test.*Guard`、`Test.*Cold`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### actionflow

- [action_runner_test.go](../../../gameplay/actionflow/action_runner_test.go)：`TestActionRunnerExplicitStartPrecedesQueue`、`TestActionRunnerEnqueueStartsIdleGroup`、`TestActionRunnerQueuedStartFailureContinuesQueue`、`TestActionRunnerRecoversTickPanic`、`TestRegistrySealAndBuilderPanic`、D21 的 `TestAnActionThatCompletesInStartEndsAtOnceAndTheQueueMoves`、`TestTheCurrentActionCanKeepItselfOrRejectAReplacement`、`TestConflictRulesBlockOrPreemptOtherGroups`、`TestEndActionEndsTheCurrentOrAQueuedActionByID`、`TestFreezingIsCountedAndCancelModeInterruptsTheCurrentAction`、`TestCancelFreezeFromACallbackInterruptsAfterTheCallback`；其余 9 项见文件
- [deferred_test.go](../../../gameplay/actionflow/deferred_test.go)：`TestCallbackMutationsAreDeferredUntilTheOuterCallReturns`、`TestDirectStartFailureEndsTheActionOnce`、`TestDeferredStartThatCanNoLongerRunStillEndsItsID`、`TestHookStartStillPrecedesQueuedActions`、`TestEndAllEndsEverythingBeforeCallbackMutationsRun`；其余 6 项见文件
- [mission_test.go](../../../gameplay/actionflow/mission_test.go)：`TestMissionRunnerBuildFailurePreservesCurrent`、`TestMissionRunnerStartFailureCleansState`、`TestMissionCallbackMutationsAreDeferredUntilTheOuterCallReturns`、`TestDeferredMissionStartErrorsAreReported`、`TestMissionCallbacksThatKeepTriggeringEachOtherAreBounded`；其余 9 项见文件
- [heartbeat_test.go](../../../gameplay/actionflow/heartbeat_test.go)：`TestEachGroupBeatsAtItsCurrentActionsInterval`、`TestAGroupsBeatTicksOnlyThatGroupWithItsElapsed`、`TestAFrozenGroupStopsBeatingAndResumesOnRecover`（经真实 Nest 与心跳驱动的用例在 `gameplay/movement`）
- [behavior_test.go](../../../gameplay/actionflow/behavior_test.go)：`TestBehaviorDeliversActionEndsToTheMissionBeforeTheObserver`、`TestBehaviorEndsAMissionsActionsWithTheMissionOnly`、`TestBehaviorClearLeavesNothingRunning`、`TestBehaviorFreezeAndRecoverThroughTheActionList`、`TestNewBehaviorRefusesABeatHostOfAnotherEntity`、`TestAMissionStepTimesOutRetriesAndFailsOnTheMissionHeartbeat`、`TestTheMissionHeartbeatFollowsTheCurrentStep`、`TestPlanRetriesAreCountedPerStep`、`TestBehaviorMetricsCountStartsEndsAndRunningActions`
- [nest_test.go](../../../gameplay/actionflow/nest_test.go)：`TestPlanStepTimeoutsRunOnTheRealMissionHeartbeat`（真实 Nest + 心跳驱动）

### entity

- [entity_base_test.go](../../../framework/entity/entity_base_test.go)：`TestTouchUnTouch`、`TestUnTouchPanicsOnReferenceUnderflow`、`TestTouchAfterRemoved`、`TestClearCompletesAfterCleanupHookPanic`、`TestConcurrentTouch`；其余 17 项见文件
- [entity_contract_test.go](../../../framework/entity/entity_contract_test.go)：`TestGuardLocksNewInstanceWithDifferentMutex`、`TestGuardTreatsInstanceSharingTheHeldMutexAsHeld`、`TestGuardLockOrderCheckUsesInstance`、`TestGuardReleaseEntityKeepsSupersededLockAccounted`、`TestGuardUncomparableCustomMutexValueDoesNotPanic`；其余 5 项见文件
- [entity_factory_test.go](../../../framework/entity/entity_factory_test.go)：`TestNewEntity_Create`、`TestEntityManagerCreateUsesInstanceDependencies`、`TestEntityManagerCreateInScopeLocksBeforePublication`、`TestInitEntitySyncInstallsContentState`、`TestBuildEntity_RemoteManagedRequiresRemoteInterface`；其余 3 项见文件
- [entity_group_test.go](../../../framework/entity/entity_group_test.go)：`TestEntityGroupBaseDefaultsAndMutation`、`TestEntityGroupManagerIndexTracksAddUpdateRemove`、`TestEntityGroupManagerLifecycleEdges`
- [entity_manager_test.go](../../../framework/entity/entity_manager_test.go)：`TestEntityManager_AddGet`、`TestEntityManager_GetWithCategory`、`TestEntityManager_DuplicatePanics`、`TestEntityManager_Remove`、`TestEntityManagerDestroyRequiresDurableAdmissionBeforeRemoval`、`TestEntityManagerWithLockedRunsUnderTheEntityLockOnlyForTheManagedInstance`；其余 15 项见文件
- [entity_remote_test.go](../../../framework/entity/entity_remote_test.go)：`TestRemoteEntityBase_Interface`、`TestRemoteEntityBase_VersionVectorIsCoherent`、`TestRemoteEntityBase_IsRemoteCapable`、`TestRemoteEntityBase_GUId`、`TestRemoteSnapshotAllowStaleDoesNotBypassExpiry`；其余 7 项见文件
- [example_gen_test.go](../../../framework/entity/example_gen_test.go)
- [kind_contract_test.go](../../../framework/entity/kind_contract_test.go)：`TestRegisterEntityKindDefsLeavesNoHalfBatch`、`TestBuiltEntityPolicyUsesKindPolicy`、`TestBuilderDefaultLifetimeFollowsKindPolicy`、`TestLateKindPolicyUpgradeConflictingWithBuilderIsRefused`、`TestRegisterEntityKindCategoryRefusesEachInvalidPair`；其余 9 项见文件
- [load_test.go](../../../framework/entity/load_test.go)：`TestLateRunLocalAfterNonNestLeaderLoadCompletedDoesNotBlock`、`TestNonNestLeaderHoldingAnotherLockColdLoadsWithoutDeadlock`、`TestNonNestLeaderHoldingLockNeededByPublishDoesNotDeadlock`、`TestALoaderPanicDoesNotWedgeTheEntity`、`TestWaitersOfAPanickingLoadAreReleased`；其余 5 项见文件
- [manager_access_test.go](../../../framework/entity/manager_access_test.go)：`TestManagerAccessPreservesOrderAndMissingEntries`、`TestManagerAccessGetManyLoadsColdEntitiesConcurrently`、`TestManagerAccessColdLoadIsSingleFlight`、`TestManagerAccessFlightWaiterHonorsOwnContext`、`TestLoadedOnlyLookupNeverStartsOrJoinsColdLoad`；其余 2 项见文件
- [registry_fixture_test.go](../../../framework/entity/registry_fixture_test.go)
- [remote_mirror_test.go](../../../framework/entity/remote_mirror_test.go)：`TestRemoteObservationCoversFollowsAdmissionOrder`、`TestRemoteMirrorReaderDTOMutationDoesNotPolluteCache`、`TestRemoteMirrorReaderNeedsNoRegistrationBesideTheOwner`、`TestRemoteMirrorReaderRejectsForeignIdentityAndSchema`、`TestRemoteSnapshotReadExitsShareOnePostCondition`
- [remote_snapshot_test.go](../../../framework/entity/remote_snapshot_test.go)：`TestRemoteSnapshotCacheAppliesDeltaAndRejectsGap`、`TestRemoteSnapshotCacheBoundsVersionWaiters`、`TestRemoteSnapshotPayloadCannotMutateCache`、`TestRemoteSnapshotLinearizableAlwaysLoadsAuthority`、`TestRemoteSnapshotCachedMissDoesNotLoadAuthority`；其余 17 项见文件
- [remote_snapshot_write_guard_test.go](../../../framework/entity/remote_snapshot_write_guard_test.go)：`TestRemoteSnapshotCacheWritesStayInTheListedFunctions`
- [subject_sync_test.go](../../../framework/entity/subject_sync_test.go)：`TestFrozenSyncPayloadOwnership`、`TestSubjectSyncPrepareCommitProfiles`、`TestSubjectSyncReclaimsAbandonedPrepare`、`TestSubjectSyncAbortAndConcurrentDirty`、`TestSubjectSyncSnapshotDoesNotAdvanceVersion`；其余 7 项见文件
- [unload_resync_test.go](../../../framework/entity/unload_resync_test.go)：`TestUnloadReloadsSubscribedSubjectFromAuthority`、`TestUnloadWithoutSubscribersDoesNotReload`、`TestUnloadRetractsWhenAuthorityHasNoEntity`、`TestUnloadRetractsAfterBoundedReloadFailures`、`TestUnloadResyncSharesTheLoadWithConcurrentAccess`；其余 5 项见文件

### fctx

- [ctx_test.go](../../../infra/base/fctx/ctx_test.go)：`TestCurrentContext_NilWithoutStore`、`TestCurrentContext_Release`、`TestCurrentContext_NestedReleaseRestoresParent`、`TestCurrentContext_PerGoroutine`、`TestContext_KV`；其余 2 项见文件
- [worker_test.go](../../../infra/base/fctx/worker_test.go)：`TestShortWorkerIsLocalExecutionState`、`TestLongAndIOWorkerStateDoesNotTravelWithSnapshot`

### goroutine

- [goroutine_test.go](../../../infra/base/goroutine/goroutine_test.go)：`TestParallelSlice`、`TestParallelSliceCollect`、`TestSafeFuncWithTryCountWrapsLastError`、`TestSafeFuncWithTryCountRecoversPanic`、`TestSafeFuncWithTryCountZeroCountStillRunsOnce`、`TestLocalMapKeepsEachGoroutineEntrySeparate`
- [mpsc_queue_test.go](../../../infra/base/goroutine/mpsc_queue_test.go)：`TestMPSCQueue_Basic`、`TestMPSCQueue_Full`、`TestMPSCQueue_MPSC`、`TestMPSCQueue_PowerOfTwo`
- [task_pool_test.go](../../../infra/base/goroutine/task_pool_test.go)：`TestTaskPoolNormalizesZeroWorkerCount`、`TestSubmitRefusesAPoolThatIsNotRunningAndAClosedWorker`、`TestTaskPoolStartAfterShutdownIsRefused`、`TestTaskPoolShutdownBeforeStartStillStopsALaterStart`、`TestSubmitRacingShutdownNeverPanics`；其余 3 项见文件

### lock

- [cputime_unix_test.go](../../../infra/base/lock/cputime_unix_test.go)
- [cputime_windows_test.go](../../../infra/base/lock/cputime_windows_test.go)
- [lock_test.go](../../../infra/base/lock/lock_test.go)：`TestReentrantMutexInnerUnlockDoesNotReleaseTheLock`、`TestReentrantMutexTryLockIsReentrantForOwnerOnly`、`TestReentrantMutexUnlockWithoutOwnershipPanics`、`TestReentrantMutex_TryLock`、`TestReentrantMutex_Contention`；其余 9 项见文件
- [reentrant_mutex_bench_test.go](../../../infra/base/lock/reentrant_mutex_bench_test.go)

### nest

- [admission_probe_bench_test.go](../../../framework/nest/admission_probe_bench_test.go)
- [await_test.go](../../../framework/nest/await_test.go)：`TestBusinessPoolsShareAllTargetTails`、`TestAwaitReleasesTailAndRepliesOnlyAfterResume`、`TestAwaitDynamicTargetsAndWaitGuards`、`TestAwaitInvalidSignatureDoesNotStartWork`、`TestAwaitShutdownWaitsForIOAndReportsResumeRejection`；其余 4 项见文件
- [cast_test.go](../../../framework/nest/cast_test.go)：`TestCastPlayerCanCastAllianceAndOtherByCategoryOrder`、`TestCastAllianceCanCastOtherByCategoryOrder`、`TestCastRejectsReverseCategoryOrder`、`TestCastRejectsRemoteAfterLocalBeforePreparingDistributedLock`、`TestCastRejectsInvalidTargetID`；其余 7 项见文件
- [cast_transaction_test.go](../../../framework/nest/cast_transaction_test.go)：`TestCastTransactionRollbackWithoutSync`、`TestCastPipelinedReleasesAndStampsWithoutSync`、`TestCastCaptureFailureFailsTheTransactionEvenIfSwallowed`、`TestCastTargetRemovedWhileWaitingNamesTheTarget`、`TestReleaseCastOfDestroyedInstanceKeepsRecreatedLock`；其余 2 项见文件
- [client_test.go](../../../framework/nest/client_test.go)：`TestClientRequestCarriesContextAndReturnsResult`、`TestClientDispatchCarriesOnlyFrameworkEnvelope`、`TestClientAdmissionReportsQueueFull`、`TestClientRejectsCanceledContextBeforeAdmission`、`TestClientFenceRejectsAdmissionWithCause`；其余 10 项见文件
- [cold_target_test.go](../../../framework/nest/cold_target_test.go)：`TestColdTargetDeletedInAuthorityAnswersEntityDeletedCode`（删过档回 `CodeEntityDeleted`、不满足 not found，D10）、`TestColdTargetMissingInAuthorityAnswersEntityNotFoundCode`（单 / 多 / 分组三种形态回 `CodeEntityNotFound`、保留原错误）、`TestDefaultSenderPreparesColdDeclaredTargetsOnIOWorker`、`TestDeclaredTargetEvictedAfterAdmissionMovesToIOKeepingOrder`、`TestAdmissionNeverRunsCustomGetterOnSender`、`TestDelayedAdmissionIsNotSerializedByCustomGetter`、`TestAdmissionColdProbeRequiresLoadedChecker`；其余 2 项见文件
- [commit_reply_test.go](../../../framework/nest/commit_reply_test.go)：`TestCommittedRemoteReplyIsNotRequeued`、`TestCommittedLocalReplyIsNotRequeued`、`TestAfterCommitPanicKeepsCauseChain`、`TestMemoryHandlerReleaseHookPanicCarriesSentinel`、`TestUncommittedLocalReleaseHookPanicHasNoSentinel`；其余 1 项见文件
- [create_in_handler_test.go](../../../framework/nest/create_in_handler_test.go)：`TestCreateInScopePipelinedWithoutWatermarkWaitsForDurable`、`TestCreateInScopeRevokedWhenHandlerFails`、`TestCreateInScopeRevokedWhenStrictCommitRejected`、`TestCreateInScopeCommittedEntitySyncsAfterCommit`、`TestCreateOutsideTransactionUnchanged`；其余 5 项见文件
- [create_lock_bench_test.go](../../../framework/nest/create_lock_bench_test.go)
- [create_lock_conflict_test.go](../../../framework/nest/create_lock_conflict_test.go)：`TestHandlerCreateCrossOrderResolvesWithoutDeadlock`、`TestHandlerCreateThenHigherGroupCastDoesNotFormCycle`、`TestSameIDCreateDoesNotOccupyShortPool`、`TestSwallowedCreateLockConflictStillRollsBack`、`TestMemoryHandlerCreateHoldsLockUntilHandlerEnds`；其余 4 项见文件
- [create_removal_window_test.go](../../../framework/nest/create_removal_window_test.go)：`TestHandlerCreateInsideDestroyWindowIsTreatedAsLockConflict`、`TestHandlerCreateInsideRevokeWindowIsTreatedAsLockConflict`、`TestOuterCreateAfterNestedRevokeInSameGuardFailsDeterministically`、`TestCreateInOtherManagerAfterSameIDRevokeKeepsRemovalWindowConflict`、`TestDestroyThenRecreateSameIDInHandlerLocksNewInstance`；其余 3 项见文件
- [dispatch_compare_bench_test.go](../../../framework/nest/dispatch_compare_bench_test.go)：`TestDispatchComparisonSingleIDContract`、`TestDispatchComparisonIDsStayInShortPool`
- [dispatch_metrics_test.go](../../../framework/nest/dispatch_metrics_test.go)：`TestDestroyedDispatchersLeaveNoSeries`、`TestADispatcherSharingItsNameKeepsTheSeries`、`TestADispatcherThatDidNotDrainKeepsItsSeriesUntilItDoes`、`TestTheIORerouteSeriesGoesWithItsDispatcher`、`TestNestStageMetricsFollowExecutionPaths`；其余 10 项见文件
- [dispatch_queue_test.go](../../../framework/nest/dispatch_queue_test.go)：`TestTwoPoolsOrderAllTargetsWithoutOccupyingShortWorker`、`TestQueueSharedPredecessorsJoinAndSplit`、`TestIOPoolUsesSharedWorkersAndBoundedWaiting`、`TestIOLoadAndHandlerUseDifferentWorkers`、`TestQueueStatsSeparateDependencyAndWorkerWait`；其余 8 项见文件、`TestQueueWorkerRunsAfterQueueBetweenJobsWithoutLock`
- [dispatch_test.go](../../../framework/nest/dispatch_test.go)：`TestDispatchLockingWithoutGuardScopeNeverReturnsCallerGuardToPool`、`TestDispatchLockingGroupRetryInScopeKeepsGuardUntilScopeEnds`、`TestDispatchEntriesRequireGuardScope`、`TestBroadcastReleasePanicBalancesTouchesAndContinues`、`TestDispatchRoutesPreserveArgumentsAndReleaseOnEveryOutcome`；其余 7 项见文件
- [short_stage_test.go](../../../framework/nest/short_stage_test.go)：`TestShortStageRejectsDirectColdAccessBeforeIO`、`TestShortContinuationRejectsSelfDispatchAndSavedIOExecutorRunsInline`、`TestShortBroadcastReportsColdTargetAndContinues`、`TestIOHandlerRunLocalDoesNotWaitForOwnPool`、`TestRunLocalExecutesOnTheShortPoolAndIsBoundToTheCommitter`；其余 2 项见文件
- [fixtures_test.go](../../../framework/nest/fixtures_test.go)
- [group_lock_test.go](../../../framework/nest/group_lock_test.go)：`TestEntityLockGroupScopeAvailableForGroupedDispatch`、`TestEntityLockGroupScopeNilForNormalDispatch`、`TestEntityLockGroupSnapshotDetectsMembershipChange`、`TestEntityLockGroupSnapshotDetectsPendingTransition`、`TestLockDispatchEntitiesForHandlerRetriesEpochChangeWhileWaiting`；其余 14 项见文件
- [handler_meta_test.go](../../../framework/nest/handler_meta_test.go)：`TestHandlerMetaRollbackRequiresExplicitDurability`、`TestDefaultHandlerRegistrationUsesDurableAsyncPolicy`、`TestTransactionPolicyParsingRejectsLegacyAndUnknownValues`、`TestHandlerHotcodePatch`、`TestInstanceScopedHandlersDoNotCollideAcrossEngines`；其余 2 项见文件
- [hot_path_bench_test.go](../../../framework/nest/hot_path_bench_test.go)
- [memory_durability_test.go](../../../framework/nest/memory_durability_test.go)：`TestMemoryTransactionPersistentWriteFailsAndRollsBack`、`TestMemoryTransactionNonPersistentWriteSucceeds`、`TestMemoryFastPathPersistentSetterStillPanics`、`TestMemoryTransactionCreatedEntityPersistentWriteFails`、`TestMemoryRemoteBatchLocalPersistentWriteRefused`；其余 1 项见文件
- [nested_fence_wal_test.go](../../../framework/nest/nested_fence_wal_test.go)：`TestOuterCommitAfterNestedAcceptFailureIsRefusedBeforeWAL`
- [nested_isolated_test.go](../../../framework/nest/nested_isolated_test.go)：`TestIsolatedCommitThenOuterLockTimeoutIsNotRequeued`、`TestIsolatedCommitThenOuterFailureCarriesSentinel`、`TestIsolatedRejectedThenOuterLockTimeoutStillRequeues`、`TestNestedIsolatedIndeterminateFencesBeforeReturning`、`TestNestedIsolatedIndeterminatePropagatedKeepsSentinels`；其余 11 项见文件
- [non_rollback_requeue_test.go](../../../framework/nest/non_rollback_requeue_test.go)：`TestNonRollbackHandlerCreateConflictIsNotRequeued`、`TestNonRollbackHandlerCreateConflictSuppressesLaterLockTimeoutRequeue`、`TestNonRollbackHandlerCreateInLockOrderStillWaits`、`TestNonRollbackHandlerCastTargetRemovedWhileWaiting`、`TestNonRollbackHandlerTransientFailureIsNotRequeued`；其余 3 项见文件
- [optimization_bench_test.go](../../../framework/nest/optimization_bench_test.go)
- [persist_change_test.go](../../../framework/nest/persist_change_test.go)：`TestMarkPersistRequiresActiveTransaction`、`TestPersistChangeCoalescesOneParticipant`、`TestMarkPersistFullDropsCoveredSubpaths`、`TestAddReceiptDeduplicatesAndRejectsDigestConflict`、`TestSetReceiptPayloadPreservesBoundIdentity`；其余 8 项见文件
- [pipelined_bench_test.go](../../../framework/nest/pipelined_bench_test.go)
- [pipelined_commit_test.go](../../../framework/nest/pipelined_commit_test.go)：`TestPipelinedCommitReleasesLocksBeforeDurable`、`TestPipelinedEnqueueRejectionRollsBack`、`TestPipelinedIndeterminateAbandonsWithoutRollback`、`TestPipelinedRequiresCapableCommitter`、`TestPipelinedAllowlistGatesHandlers`；其余 4 项见文件
- [pipelined_completion_test.go](../../../framework/nest/pipelined_completion_test.go)：`TestAsyncCompletionFreesWorkerDuringDurableWait`、`TestAsyncCompletionKeepsSameEntityCommitOrder`、`TestAsyncCompletionIndeterminateRepliesErrorWithoutRollback`、`TestAsyncCompletionShutdownDeliversPendingReplies`、`TestAsyncCompletionKeepsOrderWhenPumpIsSaturated`；其余 7 项见文件
- [remote_commit_test.go](../../../framework/nest/remote_commit_test.go)：`TestCachedRemoteAccessWithAllowStaleStillAcceptsAnOlderSnapshot`、`TestNestedCommitInRemotePrepareAlongsideCommittedLocalPartKeepsReplyText`、`TestRemoteReleaseHookPanicAfterDurableCommitStillCommits`、`TestRemoteManagedDispatchRejectsBroadcast`、`TestFinalizeRemoteWriteBatchProducesValidWALMutation`；其余 9 项见文件
- [remote_dispatch_test.go](../../../framework/nest/remote_dispatch_test.go)：`TestRemoteStagesDoNotBlockCostLogicWorker`、`TestRemoteStagesPreserveOrderAndDrainBeforeShutdown`、`TestRemoteStagesCancelAndFenceAfterPrepareCloseBatch`、`TestRemoteStagesFullShortQueueReservesContinuation`、`TestRemoteStagesReturnContextAndConfirmAfterGuardRelease`；其余 8 项见文件
- [sync_modes_test.go](../../../framework/nest/sync_modes_test.go)：`TestNestSyncModesShareCommitAndUnlockBoundaries`、`TestNestOnChangeRunsWithoutWaitingForInterval`、`TestNestSyncRollbackPreservesOlderPending`、`TestNestPipelinedSyncWaitsForConfirmationAndUnlock`、`TestNestSyncRejectedCommitDoesNotFreezeOrPublish`；其余 3 项见文件
- [ticker_test.go](../../../framework/nest/ticker_test.go)：`TestTickerPanickingCallbackStopsNeitherTheTickerNorItsPeers`、`TestTickerStopBeforeStartIsSafeAndFinal`、`TestTickCallbacksRegisteredAfterStartTakeEffectInOrder`、`TestTickerConcurrentStartStopAlwaysClosesStartedRun`、`TestTickerStateIsInstanceScoped`
- [try_batch_test.go](../../../framework/nest/try_batch_test.go)：`TestTryBatchConflictRunsAfterAdmittedSameIDJob`（约束 1：冲突转单目标、排在已准入消息之后）、`TestTryBatchClaimQueuesLaterSameIDJobBehindTheBeat`（约束 2：认领期间后来的同 ID 消息等在依赖队列）、`TestTryBatchLockBusyConvertsToSingleJob`、`TestTryBatchConversionRejectedIsHandedBack`、`TestTryBatchKeepsPerTargetTransactionSemantics`、`TestTryBatchYieldsWorkerAfterTimeSlice`、`TestTryBatchRefusesTargetsAfterFence`、`TestTryBatchStopsUnstartedTargetsAfterFenceInsideGroup`、`TestTryBatchBusyTargetIsHandedBackAfterFenceInsideGroup`、`TestTryBatchDrainsOnShutdown`、`TestTryBatchHandBackReasonsAndPoolRouting`、`TestTryBatchAlternatesWithOrdinaryJobs`
- [transaction_test.go](../../../framework/nest/transaction_test.go)：`TestCommitRunsEveryCallbackAndReportsAPanickingOne`、`TestCommitIsIdempotent`、`TestPreCommitRejectionsCarryErrCommitRejected`、`TestPrepareCommitRecordPreservesMutationIdentity`、`TestRollbackStateRestoresDaoAndDirty`；其余 8 项见文件

### worker

- [context_test.go](../../../infra/base/worker/context_test.go)：`TestWorkerCreatesFreshContextForEveryTask`、`TestPoolGoCreatesDetachedContext`、`TestPoolGoRejectsUntrackedLifetime`
- [worker_test.go](../../../infra/base/worker/worker_test.go)：`TestWorker_ProcessesTasksAndStops`、`TestWorker_AcceptedTasksSurviveConcurrentClose`、`TestWorker_CloseWakesIdleWorker`、`TestTryDispatchRefusesBeforeStartAfterStopAndWhenTheQueueIsFull`、`TestPoolStopWithContextReturnsWhenHandlerIsBlocked`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。没有新测量不能改容量承诺，性能结论以 [性能报告](../../performance/README.md) 为准。


### 排队对象的紧凑布局

`dispatchJob`保留ID tail和多ID依赖计数；三个布尔标志集中，指针/切片放在前部。准入和就绪时刻存为相对`dispatchQueue.clockOrigin`的单调偏移，等待统计用偏移差计算。原点在构造时固定，不随转慢或停机重试改变；不能改用UnixNano，以免墙钟调整破坏等待时间。ID和计数器不缩窄。64位对象与基准结果见[调度性能记录](../../performance/NEST-DISPATCH-BENCHMARK.md)。

### 调度目标的存储所有权

`newDispatchJob`直接初始化单目标的内嵌数组并建立只读切片视图；多个目标由`dispatchIDs`复制、排序和去重。不再向通用函数传入job的可变缓冲区。队列发布后目标集合直到收尾都保持不变；消息中的目标切片变化不影响队列的依赖登记和释放。ID tail顺序和业务池与 I/O 池容量契约不变。
