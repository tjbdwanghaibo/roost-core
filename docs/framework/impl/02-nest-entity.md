# 核心：Nest 调度与实体：实现与维护

运行时代码基准：当前工作树，包含 v1.25.0 后的三池/Await 更新。[设计与使用](../guide/02-nest-entity.md)

## 如何阅读

同一个玩家可能同时收到购买、聊天、领取奖励等请求。Nest负责安排执行，并保护需要一起修改的数据。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`framework/nest`、`framework/entity`、`gameplay/actionflow`、`infra/base/fctx`、`infra/base/lock`、`infra/base/worker`、`infra/base/goroutine`、`framework/event`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 同 ID 消息准入顺序和冷目标改道不能丢位置。
2. memory 本地持久写拒绝，rollback=none 不承诺恢复。
3. pipelined 不可回滚边界早于持久确认，外化必须等确认。
4. 长短业务池不能阻塞等待池任务或 I/O；同一 GuardScope 持有并释放锁。I/O 池禁止取得 Guard。
5. Await 先预留 I/O 容量，段结束释放 Guard 和 tail 后执行查询；恢复段按完整目标重新准入。首版只支持无回滚的 memory handler。

## 2A. 事务实现走读

[nest/rollback.go](../../../framework/nest/rollback.go)的durableCommit先区分memory无effect分支。该分支检查fence及本地持久写，不能准备一份没有任何持久落点的记录后仍返回成功。其他分支先prepareCommitRecord；空记录可返回，非空记录检查外层回滚域与fence，再要求TransactionCommitter。Commit返回未知保持未知，其他错误包装ErrCommitRejected，成功后acceptPersistence。

这一函数只覆盖同步提交分支，不能据此推断pipelined所有等待都在其中：pipelined的Enqueue/票据、解锁及完成回调必须沿执行收尾路径检查。修改时同时验证拒绝是否回滚、未知是否fence、成功回复是否晚于相应确认点。

事务participantOrder/participantChanges记录本事务变化；refuseMemoryPersistentWrite排除Remote认领的参与者和mutation，并对剩余本地内容给出实体/字段错误。不能以DAO上一次遗留dirty替代这些事务局部集合。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

### 三池调度与续行

[dispatch_queue.go](../../../framework/nest/dispatch_queue.go) 使用一个 lanes 数组，每项聚合配置、等待/就绪链、运行数量、内部续行、唤醒条件和统计。短业务、I/O、长业务三条通道共用 mutex、tails、依赖链、pending；不能各建一套 ID 顺序。

[await.go](../../../framework/nest/await.go) 在当前 memory 段内预留 I/O 名额。pending 包含尚未启动的预留，保证 Shutdown 不会提前退出；普通 I/O 与 Await 共用容量，预算包含正在运行的 I/O。前段失败归还预留；成功则在队列释放 tail 后启动 work。resume 使用匿名 handlerEntry，复用正式加载、Guard、同步与回复链路，不把旧实体指针带到 I/O。

新目标和旧目标没有跨段原子性。当前实现只允许无回滚 memory handler，动态目标错误、取消、停机及过载均有明确终态。具体用法、旧 Cast 迁移、验证命令和限制见 [三池与 Await](../NEST-AWAIT.md)。早期两通道字段聚合的性能样本保留在 [调度对照基准](../../performance/NEST-DISPATCH-BENCHMARK.md)，不能拿它证明三池或反射恢复的性能。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### actionflow

6 个实现文件、12 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [action_runner.go](../../../gameplay/actionflow/action_runner.go) | `ActionSnapshot`、`ActionRunnerHooks`、`ActionRunnerConfig`、`ActionRunner` |
| [action_types.go](../../../gameplay/actionflow/action_types.go) | `ActionKind`、`ActionGroup`、`MissionKind`、`ActionStatus`、`MissionStatus`、`ActionResult`、`ActionReason`、`MissionNextMode`、`MissionNext`、`MissionStep`、`MissionPlan`、`MissionInfo` |
| [mission_runner.go](../../../gameplay/actionflow/mission_runner.go) | `MissionRunnerHooks`、`MissionRunnerConfig`、`MissionRunner` |
| [plan.go](../../../gameplay/actionflow/plan.go) | `PlanMission` |
| [registry.go](../../../gameplay/actionflow/registry.go) | `ActionBuilder`、`MissionBuilder`、`Registry` |
| [runtime.go](../../../gameplay/actionflow/runtime.go) | `ActionContext`、`Action`、`ActionList`、`MissionContext`、`Mission`、`MissionManager`、`MissionRuntimeSetter` |

### entity

28 个实现文件、46 个测试文件。

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
| [entity_manager.go](../../../framework/entity/entity_manager.go) | `EntityManager`、`EntityManagerOption`、`DeleteAdmission`、`DeleteAdmitter` |
| [entity_remote.go](../../../framework/entity/entity_remote.go) | `IThreadSafeRemoteEntity`、`RemoteEntityBase` |
| [idgen.go](../../../framework/entity/idgen.go) | `IDGen` |
| [local_executor.go](../../../framework/entity/local_executor.go) | 函数/方法或内部实现；见源码 |
| [manager_access.go](../../../framework/entity/manager_access.go) | `ManagerAccess` |
| [manager_factory.go](../../../framework/entity/manager_factory.go) | `CreatedEntityCapturer` |
| [policy.go](../../../framework/entity/policy.go) | `RemotePolicy`、`EntityLifetime` |
| [remote_manager.go](../../../framework/entity/remote_manager.go) | `IRemoteEntityLoader`、`IRemoteEntityLocalLookup`、`IRemoteEntityUnloader`、`IRemoteEntityBackend`、`IRemoteEntityOwnershipStore`、`RemoteEntityMarkerLease`、`IRemoteEntityManager` |
| [remote_mirror.go](../../../framework/entity/remote_mirror.go) | `RemoteObservation`、`RemoteSnapshotRead`、`RemoteSnapshotReadOnly`、`RemoteMirrorSpec`、`RemoteMirrorValue`、`RemoteMirrorReader` |
| [remote_protocol.go](../../../framework/entity/remote_protocol.go) | `RemoteOwnershipState`、`RemoteVersionVector`、`RemoteWriteMode`、`RemoteWriteLease`、`RemoteTransactionID`、`RemotePersistChange`、`RemotePersistChangeSource`、`RemoteDeleteIntentSource`、`RemoteTransactionOutcome`、`RemoteSnapshotRecord`、`RemoteDataMutation`、`RemoteDataDelete`、`RemoteCommit`、`RemoteCommitReceipt`、`RemoteCommitState`、`RemoteCommitStatus`、`IRemoteCommitParticipant`、`IRemoteCommitter`、`IRemoteAtomicBatchCommitter`、`IRemoteCommitWaiter`、`IRemoteCommitOutbox`、`RemoteOutboxCursor`、`IRemoteCommitOutboxPager`、`IRemoteStorageInitializer`、`IRemoteSnapshotPublisher`、`IRemoteSnapshotLoader`、`RemoteWriteBatch`、`RemoteOutcomeDeferrer`、`RemoteWriteBatchManager`、`RemoteOwnershipManager`、`RemoteSnapshotInterest`、`RemoteSnapshotInterestManager`、`RemoteCommitApplier`、`RemoteSnapshotReader` |
| [remote_snapshot.go](../../../framework/entity/remote_snapshot.go) | `RemoteReadConsistency`、`RemoteSnapshotKey`、`ImmutableRemoteSnapshot`、`FrozenRemoteSnapshotPayload`、`RemoteSnapshotEnvelope`、`RemoteSnapshotLoader`、`RemoteSnapshotCacheConfig`、`RemoteSnapshotCache`、`RemoteSnapshotReplica`、`RemoteSnapshotBootstrapStats`、`RemoteSnapshotVersionedDeleter` |
| [remote_snapshot_codec.go](../../../framework/entity/remote_snapshot_codec.go) | `RemoteSnapshotDecodeFunc`、`RemoteSnapshotDeltaFunc`、`RemoteChecksum` |
| [remote_view.go](../../../framework/entity/remote_view.go) | `RemoteAcquireMode`、`RemoteReadOption`、`RemoteSnapshotSource`、`RemoteSnapshot`、`RemoteSnapshotRequest`、`RemoteSnapshotProvider`、`RemoteSnapshotResolveRequest`、`RemoteViewRef` |
| [resolver.go](../../../framework/entity/resolver.go) | `EntityIDMeta` |
| [subject_sync.go](../../../framework/entity/subject_sync.go) | `SyncProfile`、`FrozenSyncPayload`、`SubjectSyncPacker`、`SubjectSyncPackFunc`、`SubjectSyncDirtyNotifier`、`SubjectSyncCreateParam`、`EntitySyncCreateParam`、`EntitySyncBuilderParam`、`SubjectSyncUpdate`、`SubjectSyncState`、`PreparedSubjectSync`、`PreparedSubjectSyncBatch` |
| [sync_commit.go](../../../framework/entity/sync_commit.go) | `SyncCommitObserver`、`SyncSubjectRetractor`、`SyncChangeCollector`、`SyncMutation`、`SyncChangeMapper` |
| [sync_frozen.go](../../../framework/entity/sync_frozen.go) | 函数/方法或内部实现；见源码 |
| [sync_view.go](../../../framework/entity/sync_view.go) | `SyncView`、`SyncViewSet`、`NamedSyncView` |
| [unload_resync.go](../../../framework/entity/unload_resync.go) | `UnloadedSubjectSync`、`UnloadResyncConfig`、`UnloadResyncStats` |

### event

4 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [event.go](../../../framework/event/event.go) | `EventType`、`EventGroupType`、`EventData` |
| [event_bus.go](../../../framework/event/event_bus.go) | `EventBus` |
| [event_handler.go](../../../framework/event/event_handler.go) | `EventHandler`、`AsyncDispatcher` |
| [event_mgr.go](../../../framework/event/event_mgr.go) | `EventUnit`、`EventMgr` |

### fctx

3 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [context.go](../../../infra/base/fctx/context.go) | `Context`、`ContextSnapshot`、`TraceMeta`、`RequestMeta`、`Option` |
| [runtime_context.go](../../../infra/base/fctx/runtime_context.go) | 函数/方法或内部实现；见源码 |
| [worker.go](../../../infra/base/fctx/worker.go) | 函数/方法或内部实现；见源码 |

### goroutine

6 个实现文件、8 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [goid_prod.go](../../../infra/base/goroutine/goid_prod.go) | 函数/方法或内部实现；见源码 |
| [goid_race.go](../../../infra/base/goroutine/goid_race.go) | 函数/方法或内部实现；见源码 |
| [mpsc_queue.go](../../../infra/base/goroutine/mpsc_queue.go) | `MPSCQueue` |
| [parallel.go](../../../infra/base/goroutine/parallel.go) | `ParallelMapOption` |
| [safe_func.go](../../../infra/base/goroutine/safe_func.go) | 函数/方法或内部实现；见源码 |
| [task_pool.go](../../../infra/base/goroutine/task_pool.go) | `Task`、`TaskFunc`、`TaskPoolConfig`、`TaskPool` |

### lock

3 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [lock.go](../../../infra/base/lock/lock.go) | `Mutex`、`LockIdGetter` |
| [lock_manager.go](../../../infra/base/lock/lock_manager.go) | `MutexFactory`、`LockManager` |
| [reentrant_mutex.go](../../../infra/base/lock/reentrant_mutex.go) | `ReentrantMutex` |

### nest

22 个实现文件、87 个测试文件。

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
| [slow_load.go](../../../framework/nest/slow_load.go) | 函数/方法或内部实现；见源码 |
| [stats.go](../../../framework/nest/stats.go) | `DispatcherWorkStats`、`DispatchLaneStats`、`DispatchQueueStats`、`DispatcherStats` |
| [ticker.go](../../../framework/nest/ticker.go) | `TickCallbackName`、`Ticker` |
| [trace.go](../../../framework/nest/trace.go) | 函数/方法或内部实现；见源码 |
| [transaction.go](../../../framework/nest/transaction.go) | `DurabilityPolicy`、`TransactionID`、`EntityMutation`、`Effect`、`CommitRecord`、`CommitFence`、`CommitWAL`、`TransactionCommitter`、`TransactionReleaseNotifier`、`LocalExecutorBinder`、`CommitTicket`、`PipelinedTransactionCommitter`、`CommitParticipant` |

### worker

2 个实现文件、4 个测试文件。

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

- [action_runner_test.go](../../../gameplay/actionflow/action_runner_test.go)：`TestActionRunnerExplicitStartPrecedesQueue`、`TestActionRunnerEnqueueStartsIdleGroup`、`TestActionRunnerQueuedStartFailureContinuesQueue`、`TestActionRunnerRecoversTickPanic`、`TestRegistrySealAndBuilderPanic`
- [deferred_mutation_promises_test.go](../../../gameplay/actionflow/deferred_mutation_promises_test.go)：`TestCallbackMutationsAreDeferredUntilTheOuterCallReturns`、`TestCancelReentryCancelsTheReplacedActionOnce`、`TestDirectStartFailureEndsTheActionOnce`、`TestDeferredStartThatCanNoLongerRunStillEndsItsID`、`TestHookStartStillPrecedesQueuedActions`；其余 4 项见文件
- [execution_cleanup_test.go](../../../gameplay/actionflow/execution_cleanup_test.go)：`TestOuterExecutionResetsAfterInternalPanic`
- [mission_deferred_promises_test.go](../../../gameplay/actionflow/mission_deferred_promises_test.go)：`TestMissionCallbackMutationsAreDeferredUntilTheOuterCallReturns`、`TestDeferredMissionStartErrorsAreReported`、`TestMissionCallbacksThatKeepTriggeringEachOtherAreBounded`、`TestOuterMissionStartReturnsRunaway`、`TestMissionDeferredQueueIsBounded`；其余 2 项见文件
- [mission_runner_test.go](../../../gameplay/actionflow/mission_runner_test.go)：`TestMissionRunnerBuildFailurePreservesCurrent`、`TestMissionRunnerStartFailureCleansState`
- [plan_guards_promises_test.go](../../../gameplay/actionflow/plan_guards_promises_test.go)：`TestNormalizePlanRefusesEveryInvalidShape`、`TestPlanMissionRefusesAStepIndexOutsideThePlan`、`TestMissionRunnerHookThatEndsTheMissionDuringStartEndsItAfterTheStart`、`TestActionRunnerRefusesABuilderThatReturnsNoAction`
- [promises_test.go](../../../gameplay/actionflow/promises_test.go)：`TestActionRunnerRefusesToStartInAFrozenGroup`、`TestMissionRunnerRefusesUnknownKindAndUnreplaceableMission`
- [queue_discard_promises_test.go](../../../gameplay/actionflow/queue_discard_promises_test.go)：`TestDiscardedQueuedActionsAreReportedAsEnded`、`TestClearMissionKeepsAnotherMissionsQueuedActions`
- [reentrancy_promises_test.go](../../../gameplay/actionflow/reentrancy_promises_test.go)：`TestActionRunnerCallbackStartReplacesTheOuterActionExactlyOnce`、`TestActionRunnerRefusesInvalidConfigUnknownGroupsAndExhaustedIDs`、`TestMissionRunnerDefersReentrantStartAndRefusesExhaustedIDsAndIdleCancel`
- [start_failure_reentrancy_promises_test.go](../../../gameplay/actionflow/start_failure_reentrancy_promises_test.go)：`TestStartFailureWhoseCancelStartsAnotherActionKeepsThatAction`、`TestQueuedStartFailureWhoseCancelStartsAnotherActionKeepsThatAction`
- [types_test.go](../../../gameplay/actionflow/types_test.go)：`TestActionReasonPreservesStructuredResult`
- [update_and_replace_promises_test.go](../../../gameplay/actionflow/update_and_replace_promises_test.go)：`TestUpdateWhoseFnPanicsLeavesTheRunnerUsable`、`TestReplacedActionWhoseCancelPanicsStillLetsTheNewActionStart`

### entity

- [category_contract_test.go](../../../framework/entity/category_contract_test.go)：`TestRegisteredCategoriesMakeTheCategoryValueTheLockOrder`、`TestEntityCategoriesBeyondTheIDMaskAreAllowed`
- [destroy_cancel_promises_test.go](../../../framework/entity/destroy_cancel_promises_test.go)：`TestEntityManagerDestroyCancelsWhileEntityLockIsHeld`、`TestEntityManagerDestroyCancellationAfterAdmissionStillFinalizes`、`TestEntityManagerDestroyPreservesReentrantLockOwnership`
- [entity_base_test.go](../../../framework/entity/entity_base_test.go)：`TestTouchUnTouch`、`TestUnTouchPanicsOnReferenceUnderflow`、`TestTouchAfterRemoved`、`TestClearCompletesAfterCleanupHookPanic`、`TestConcurrentTouch`；其余 18 项见文件
- [entity_factory_test.go](../../../framework/entity/entity_factory_test.go)：`TestNewEntity_Create`、`TestEntityManagerCreateUsesInstanceDependencies`、`TestEntityManagerCreateInScopeLocksBeforePublication`、`TestInitEntitySyncInstallsContentState`、`TestBuildEntity_RemoteManagedRequiresRemoteInterface`；其余 4 项见文件
- [entity_group_test.go](../../../framework/entity/entity_group_test.go)：`TestEntityGroupBaseDefaultsAndMutation`、`TestEntityGroupManagerIndexTracksAddUpdateRemove`、`TestEntityGroupManagerLifecycleEdges`
- [entity_contract_test.go](../../../framework/entity/entity_contract_test.go)：`TestGuardLocksNewInstanceWithDifferentMutex`、`TestGuardTreatsInstanceSharingTheHeldMutexAsHeld`、`TestGuardLockOrderCheckUsesInstance`、`TestGuardReleaseEntityKeepsSupersededLockAccounted`、`TestGuardUncomparableCustomMutexValueDoesNotPanic`；其余 2 项见文件、`TestManagerRangeCallbackMayDestroyAndStopsAtFalse`、`TestManagerRangeNeverHandsOutAClearedEntity`、`TestValueTypeEntityIsRejectedAtRegistrationAndPublish`
- [entity_manager_test.go](../../../framework/entity/entity_manager_test.go)：`TestEntityManager_AddGet`、`TestEntityManager_GetWithCategory`、`TestEntityManager_DuplicatePanics`、`TestEntityManager_Remove`、`TestEntityManagerDestroyRequiresDurableAdmissionBeforeRemoval`；其余 11 项见文件
- [entity_remote_test.go](../../../framework/entity/entity_remote_test.go)：`TestRemoteEntityBase_Interface`、`TestRemoteEntityBase_VersionVectorIsCoherent`、`TestRemoteEntityBase_IsRemoteCapable`、`TestRemoteEntityBase_TouchUnTouch`、`TestRemoteEntityBase_GUId`；其余 1 项见文件
- [example_gen_test.go](../../../framework/entity/example_gen_test.go)
- [fast_cold_miss_promises_test.go](../../../framework/entity/fast_cold_miss_promises_test.go)：`TestFastWorkerColdMissReturnsErrorInsteadOfPanicking`
- [guards_promises_test.go](../../../framework/entity/guards_promises_test.go)：`TestEntityGuardsRefuseNoneKindMissingFactoriesAndLateGeneratorChanges`
- [kind_contract_test.go](../../../framework/entity/kind_contract_test.go)：`TestRegisterEntityKindDefsLeavesNoHalfBatch`、`TestBuiltEntityPolicyUsesKindPolicy`、`TestBuilderDefaultLifetimeFollowsKindPolicy`、`TestLateKindPolicyUpgradeConflictingWithBuilderIsRefused`、`TestRegisterEntityKindCategoryRefusesEachInvalidPair`、`TestResolveEntityKindCategoryRefusesNoneAndUnregistered`、`TestEntityCreateParamNormalizeIDRefusesEachInconsistency`、`TestResolveEntityBuilderRefusesMissingKindAndBuilder`、`TestKindRegistryReadsDoNotBlockOnRegistrationWrites`、`TestKindRegistryKeepsItsRegistrationRules`
- [load_flight_detach_test.go](../../../framework/entity/load_flight_detach_test.go)：`TestSharedLoadOutlivesTheLeadersBudget`、`TestLastWaiterLeavingDoesNotCancelTheLoad`、`TestUnregisteringTheLoaderCancelsInFlightLoads`、`TestSharedLoadIsBoundedByTheFrameworkLoadTimeout`、`TestLoaderStopCauseReachesWaiters`
- [load_contract_test.go](../../../framework/entity/load_contract_test.go)：`TestLateRunLocalAfterNonNestLeaderLoadCompletedDoesNotBlock`、`TestNonNestLeaderHoldingAnotherLockColdLoadsWithoutDeadlock`、`TestNonNestLeaderHoldingLockNeededByPublishDoesNotDeadlock`、`TestALoaderPanicDoesNotWedgeTheEntity`、`TestWaitersOfAPanickingLoadAreReleased`
- [loaded_checker_promises_test.go](../../../framework/entity/loaded_checker_promises_test.go)：`TestManagerAccessIsLoadedNeverLoads`
- [lock_order_no_hook_promises_test.go](../../../framework/entity/lock_order_no_hook_promises_test.go)：`TestLockOrderIsTheCategoryWithNoApplicationHook`
- [manager_access_test.go](../../../framework/entity/manager_access_test.go)：`TestManagerAccessPreservesOrderAndMissingEntries`、`TestManagerAccessGetManyLoadsColdEntitiesConcurrently`、`TestManagerAccessColdLoadIsSingleFlight`、`TestManagerAccessFlightWaiterHonorsOwnContext`、`TestLoadedOnlyLookupNeverStartsOrJoinsColdLoad`
- [range_contract_promises_test.go](../../../framework/entity/range_contract_promises_test.go)：`TestEntityRangeContract`
- [registry_fixture_test.go](../../../framework/entity/registry_fixture_test.go)
- [remote_commit_promises_test.go](../../../framework/entity/remote_commit_promises_test.go)：`TestRemoteCommitValidateRefusesEachBrokenField`
- [remote_dao_scope_registry_promises_test.go](../../../framework/entity/remote_dao_scope_registry_promises_test.go)：`TestValidateEntityRegistryRejectsRemoteManagedServerScopedDao`
- [remote_mirror_promises_test.go](../../../framework/entity/remote_mirror_promises_test.go)：`TestRemoteObservationCoversFollowsAdmissionOrder`、`TestRemoteMirrorReaderDTOMutationDoesNotPolluteCache`、`TestRemoteMirrorReaderNeedsNoRegistrationBesideTheOwner`、`TestRemoteMirrorReaderRejectsForeignIdentityAndSchema`、`TestRemoteSnapshotReadExitsShareOnePostCondition`
- [remote_snapshot_test.go](../../../framework/entity/remote_snapshot_test.go)：`TestRemoteSnapshotCacheAppliesDeltaAndRejectsGap`、`TestRemoteSnapshotCacheBoundsVersionWaiters`、`TestRemoteSnapshotPayloadCannotMutateCache`、`TestRemoteSnapshotLinearizableAlwaysLoadsAuthority`、`TestRemoteSnapshotCachedMissDoesNotLoadAuthority`；其余 3 项见文件
- [remote_snapshot_write_guard_test.go](../../../framework/entity/remote_snapshot_write_guard_test.go)：`TestRemoteSnapshotCacheWritesStayInTheListedFunctions`
- [remote_version_vector_promises_test.go](../../../framework/entity/remote_version_vector_promises_test.go)：`TestSetRemoteVersionVectorRejectsSameFenceRegression`
- [remote_view_test.go](../../../framework/entity/remote_view_test.go)：`TestRemoteViewRefValidatesEntityIDAndKind`、`TestRemoteReadOptionDefaultsRequireFreshData`、`TestRemoteSnapshotAcceptsVersionAndPreservesImmutableData`
- [set_entity_version_no_rewind_promises_test.go](../../../framework/entity/set_entity_version_no_rewind_promises_test.go)：`TestSetEntityVersionRejectsRewindUnderSameFence`
- [snapshot_contract_test.go](../../../framework/entity/snapshot_contract_test.go)：`TestAuthoritativeLoaderRejectsForeignKeyBeforePublish`、`TestAuthoritativeReadChecksStoredMinimumVersion`、`TestAuthoritativeReadRechecksExpiryAfterL2Publish`、`TestDeleteFenceCoversInflightL2Refill`、`TestColdL1DeletePreservesNewerL2`、`TestPublishConflictAfterPreflight`、`TestRemoteSnapshotDeleteAtVersionPromiseKeepsNewerSnapshot`、`TestRemoteSnapshotDeleteAtVersionPromiseFencesOlderSnapshot`、`TestRemoteSnapshotDeleteAtVersionPromiseTombstoneExpires`、`TestAuthoritativeReadsNeverReturnAnExpiredSnapshot`、`TestExpiredSnapshotIsNotServedFromCache`、`TestPublishSurfacesAnL2VersionConflictAndKeepsItOutOfL1`、`TestL2BackfillCannotOverwriteAPublishedSameVersionValue`、`TestSnapshotLoadReturnsACanceledWaitersSlot`
- [subject_sync_test.go](../../../framework/entity/subject_sync_test.go)：`TestFrozenSyncPayloadOwnership`、`TestSubjectSyncPrepareCommitProfiles`、`TestSubjectSyncReclaimsAbandonedPrepare`、`TestSubjectSyncAbortAndConcurrentDirty`、`TestSubjectSyncSnapshotDoesNotAdvanceVersion`；其余 6 项见文件
- [sync_view_test.go](../../../framework/entity/sync_view_test.go)：`TestFullDirtyReusesSnapshotAndEmptyViewsStillCommit`
- [unload_resync_backlog_promises_test.go](../../../framework/entity/unload_resync_backlog_promises_test.go)：`TestUnloadResyncBacklogIsObservable`、`TestUnloadResyncBacklogIsZeroAfterStop`、`TestUnloadResyncBacklogSumsAcrossManagerAccesses`
- [unload_resync_test.go](../../../framework/entity/unload_resync_test.go)：`TestUnloadReloadsSubscribedSubjectFromAuthority`、`TestUnloadWithoutSubscribersDoesNotReload`、`TestUnloadRetractsWhenAuthorityHasNoEntity`、`TestUnloadRetractsAfterBoundedReloadFailures`、`TestUnloadResyncSharesTheLoadWithConcurrentAccess`；其余 2 项见文件

### event

- [event_test.go](../../../framework/event/event_test.go)：`TestEventMgr_PubSub`、`TestEventMgr_AsyncDispatch`、`TestEventMgr_Unsub`、`TestEventMgr_MultiGroup`、`TestEventBus`；其余 1 项见文件

### fctx

- [ctx_test.go](../../../infra/base/fctx/ctx_test.go)：`TestCurrentContext_Basic`、`TestCurrentContext_NilWithoutStore`、`TestCurrentContext_Release`、`TestCurrentContext_NestedReleaseRestoresParent`、`TestCurrentContext_PerGoroutine`；其余 3 项见文件
- [worker_test.go](../../../infra/base/fctx/worker_test.go)：`TestFastWorkerIsLocalExecutionState`

### goroutine

- [goroutine_test.go](../../../infra/base/goroutine/goroutine_test.go)：`TestParallelSlice`、`TestParallelSliceCollect`
- [mpsc_queue_test.go](../../../infra/base/goroutine/mpsc_queue_test.go)：`TestMPSCQueue_Basic`、`TestMPSCQueue_Full`、`TestMPSCQueue_MPSC`、`TestMPSCQueue_PowerOfTwo`
- [safe_func_test.go](../../../infra/base/goroutine/safe_func_test.go)：`TestSafeFuncWithTryCountWrapsLastError`、`TestSafeFuncWithTryCountRecoversPanic`、`TestSafeFuncWithTryCountZeroCountStillRunsOnce`
- [task_pool_contract_test.go](../../../infra/base/goroutine/task_pool_contract_test.go)：`TestSubmitRefusesAPoolThatIsNotRunningAndAClosedWorker`、`TestTaskPoolStartAfterShutdownIsRefused`、`TestTaskPoolShutdownBeforeStartStillStopsALaterStart`、`TestSubmitRacingShutdownNeverPanics`、`TestTaskPoolStatsCountSubmitBeforeTheTaskCanFinish`、`TestTaskPoolStatsReadFinishedBeforeSubmitted`、`TestTaskPoolRejectedSubmitIsNotCounted`
- [task_pool_test.go](../../../infra/base/goroutine/task_pool_test.go)：`TestTaskPoolNormalizesZeroWorkerCount`

### lock

- [context_test.go](../../../infra/base/lock/context_test.go)：`TestLockContextCancellationPreservesOwnerAndAllowsReuse`
- [cputime_unix_test.go](../../../infra/base/lock/cputime_unix_test.go)
- [cputime_windows_test.go](../../../infra/base/lock/cputime_windows_test.go)
- [lock_test.go](../../../infra/base/lock/lock_test.go)：`TestReentrantMutexInnerUnlockDoesNotReleaseTheLock`、`TestReentrantMutexTryLockIsReentrantForOwnerOnly`、`TestReentrantMutexUnlockWithoutOwnershipPanics`、`TestReentrantMutex_TryLock`、`TestReentrantMutex_Contention`；其余 9 项见文件
- [reentrant_mutex_bench_test.go](../../../infra/base/lock/reentrant_mutex_bench_test.go)

### nest

- [admission_boundary_test.go](../../../framework/nest/admission_boundary_test.go)：`TestPipelinedAdmissionRunsBeforeUnlock`、`TestPipelinedAdmissionPanicStillCompletesDurableTransaction`、`TestPipelinedAdmissionRejectAndEmptyRecord`
- [admission_loaded_probe_promises_test.go](../../../framework/nest/admission_loaded_probe_promises_test.go)：`TestAdmissionNeverRunsCustomGetterOnSender`、`TestDelayedAdmissionIsNotSerializedByCustomGetter`、`TestAdmissionColdProbeRequiresLoadedChecker`
- [admission_probe_bench_test.go](../../../framework/nest/admission_probe_bench_test.go)
- [broadcast_release_per_target_promises_test.go](../../../framework/nest/broadcast_release_per_target_promises_test.go)：`TestBroadcastReleasesDestroyedAndRecreatedLocksPerTarget`、`TestReleaseCastOnDeclaredRemoteTargetIsNoopUntilMessageEnds`
- [cast_capture_failure_promises_test.go](../../../framework/nest/cast_capture_failure_promises_test.go)：`TestCastCaptureFailureFailsTheTransactionEvenIfSwallowed`
- [cast_promises_test.go](../../../framework/nest/cast_promises_test.go)：`TestCastMultiRejectsEveryMissingPrecondition`、`TestCastThreeReportsTypeMismatchOnEveryPosition`、`TestDispatchBroadcastRejectsEmptyTargets`
- [cast_removed_target_id_promises_test.go](../../../framework/nest/cast_removed_target_id_promises_test.go)：`TestCastTargetRemovedWhileWaitingNamesTheTarget`
- [cast_test.go](../../../framework/nest/cast_test.go)：`TestCastPlayerCanCastAllianceAndOtherByCategoryOrder`、`TestCastAllianceCanCastOtherByCategoryOrder`、`TestCastRejectsReverseCategoryOrder`、`TestCastRejectsRemoteAfterLocalBeforePreparingDistributedLock`、`TestCastRejectsInvalidTargetID`；其余 3 项见文件
- [cast_transaction_test.go](../../../framework/nest/cast_transaction_test.go)：`TestCastTransactionRollbackWithoutSync`、`TestCastPipelinedReleasesAndStampsWithoutSync`
- [client_test.go](../../../framework/nest/client_test.go)：`TestClientRequestCarriesContextAndReturnsResult`、`TestClientDispatchCarriesOnlyFrameworkEnvelope`、`TestClientAdmissionReportsQueueFull`、`TestClientRejectsCanceledContextBeforeAdmission`、`TestClientFenceRejectsAdmissionWithCause`；其余 1 项见文件
- [cold_target_admission_promises_test.go](../../../framework/nest/cold_target_admission_promises_test.go)：`TestDefaultSenderPreparesColdDeclaredTargetsOnSlowWorker`、`TestDeclaredTargetEvictedAfterAdmissionMovesToSlowKeepingOrder`
- [commit_contract_test.go](../../../framework/nest/commit_contract_test.go)：`TestCommitRunsEveryCallbackAndReportsAPanickingOne`、`TestCommitIsIdempotent`、`TestPreCommitRejectionsCarryErrCommitRejected`
- [committed_requeue_promises_test.go](../../../framework/nest/committed_requeue_promises_test.go)：`TestCommittedRemoteReplyIsNotRequeued`、`TestCommittedLocalReplyIsNotRequeued`、`TestAfterCommitPanicKeepsCauseChain`
- [completion_release_panic_test.go](../../../framework/nest/completion_release_panic_test.go)：`TestPipelinedReleasePanicStillCompletesAndReplies`、`TestPipelinedInlineCompletionReportsCallbackFailure`
- [completion_unlock_test.go](../../../framework/nest/completion_unlock_test.go)：`TestAsyncCompletionWaitsForEntityRelease`、`TestTickerConcurrentStartStopAlwaysClosesStartedRun`
- [create_capture_failure_promises_test.go](../../../framework/nest/create_capture_failure_promises_test.go)：`TestCreateCaptureFailureFailsTheTransactionEvenIfSwallowed`
- [create_in_scope_commit_boundary_promises_test.go](../../../framework/nest/create_in_scope_commit_boundary_promises_test.go)：`TestCreateInScopePipelinedWithoutWatermarkWaitsForDurable`、`TestCreateInScopeRevokedWhenHandlerFails`、`TestCreateInScopeRevokedWhenStrictCommitRejected`、`TestCreateInScopeCommittedEntitySyncsAfterCommit`、`TestCreateOutsideTransactionUnchanged`
- [create_lock_bench_test.go](../../../framework/nest/create_lock_bench_test.go)
- [create_lock_order_promises_test.go](../../../framework/nest/create_lock_order_promises_test.go)：`TestHandlerCreateCrossOrderResolvesWithoutDeadlock`、`TestHandlerCreateThenHigherGroupCastDoesNotFormCycle`、`TestSameIDCreateDoesNotOccupyFastPool`、`TestSwallowedCreateLockConflictStillRollsBack`、`TestMemoryHandlerCreateHoldsLockUntilHandlerEnds`
- [create_revoke_destroy_reason_promises_test.go](../../../framework/nest/create_revoke_destroy_reason_promises_test.go)：`TestRevokedCreationIsDestroyedWithCreateRevokedReason`
- [created_contract_test.go](../../../framework/nest/created_contract_test.go)：`TestHandlerCreateInsideDestroyWindowIsTreatedAsLockConflict`、`TestHandlerCreateInsideRevokeWindowIsTreatedAsLockConflict`、`TestOuterCreateAfterNestedRevokeInSameGuardFailsDeterministically`、`TestCreateInOtherManagerAfterSameIDRevokeKeepsRemovalWindowConflict`
- [cross_create_requeue_budget_promises_test.go](../../../framework/nest/cross_create_requeue_budget_promises_test.go)：`TestSymmetricCrossCreatePairsResolveWithinRequeueBudget`
- [dataengine_record_test.go](../../../framework/nest/dataengine_record_test.go)：`TestPrepareCommitRecordPreservesMutationIdentity`
- [delayed_shutdown_promises_test.go](../../../framework/nest/delayed_shutdown_promises_test.go)：`TestShutdownAnswersDelayedRequestsInsteadOfDroppingThem`、`TestShutdownDropsDelayedFireAndForgetQuietly`
- [destroy_recreate_same_id_promises_test.go](../../../framework/nest/destroy_recreate_same_id_promises_test.go)：`TestDestroyThenRecreateSameIDInHandlerLocksNewInstance`、`TestDestroyThenRecreateSameIDInHandlerRollsBack`、`TestCastAfterDestroyDoesNotReturnUnlockedRecreatedInstance`、`TestDestroyThenRecreateSameIDRemoteEntityLocksNewInstance`
- [dispatch_guard_scope_promises_test.go](../../../framework/nest/dispatch_guard_scope_promises_test.go)：`TestDispatchLockingWithoutGuardScopeNeverReturnsCallerGuardToPool`、`TestDispatchLockingGroupRetryInScopeKeepsGuardUntilScopeEnds`、`TestDispatchEntriesRequireGuardScope`
- [dispatch_lifecycle_test.go](../../../framework/nest/dispatch_lifecycle_test.go)：`TestBroadcastReleasePanicBalancesTouchesAndContinues`、`TestDispatchRoutesPreserveArgumentsAndReleaseOnEveryOutcome`
- [dispatch_queue_test.go](../../../framework/nest/dispatch_queue_test.go)：`TestTwoPoolsOrderAllTargetsWithoutOccupyingFastWorker`、`TestSlowPoolUsesSharedWorkersAndBoundedWaiting`、`TestSlowLoadAndHandlerUseDifferentWorkers`、`TestQueueStatsSeparateDependencyAndWorkerWait`、`TestFastLogicMarksGetterContextAndSlowPreparationCanLoad`；其余 1 项见文件
- [dispatcher_series_lifecycle_promises_test.go](../../../framework/nest/dispatcher_series_lifecycle_promises_test.go)：`TestDestroyedDispatchersLeaveNoSeries`、`TestADispatcherSharingItsNameKeepsTheSeries`、`TestADispatcherThatDidNotDrainKeepsItsSeriesUntilItDoes`、`TestTheSlowRerouteSeriesGoesWithItsDispatcher`
- [fast_cold_miss_promises_test.go](../../../framework/nest/fast_cold_miss_promises_test.go)：`TestFastCastColdTargetLetsBusinessFallBack`
- [fast_stage_test.go](../../../framework/nest/fast_stage_test.go)：`TestFastStageRejectsDirectColdAccessBeforeIO`、`TestFastContinuationRejectsSelfDispatchAndSavedSlowExecutorRunsInline`、`TestFastBroadcastReportsColdTargetAndContinues`
- [fenced_memory_remote_no_effect_promises_test.go](../../../framework/nest/fenced_memory_remote_no_effect_promises_test.go)：`TestFencedMemoryRemoteWithoutEffectIsRefused`
- [group_lock_test.go](../../../framework/nest/group_lock_test.go)：`TestEntityLockGroupScopeAvailableForGroupedDispatch`、`TestEntityLockGroupScopeNilForNormalDispatch`、`TestEntityLockGroupSnapshotDetectsMembershipChange`、`TestEntityLockGroupSnapshotDetectsPendingTransition`、`TestLockDispatchEntitiesForHandlerRetriesEpochChangeWhileWaiting`；其余 4 项见文件
- [group_release_panic_test.go](../../../framework/nest/group_release_panic_test.go)：`TestGroupReleasePanicStillReleasesGroupLockAndScope`
- [group_transition_promises_test.go](../../../framework/nest/group_transition_promises_test.go)：`TestGroupTransitionRequestsRefuseZeroGroupsUnknownEntitiesAndOverlaps`
- [group_transition_test.go](../../../framework/nest/group_transition_test.go)：`TestEntityLockGroupTransitionJoinMoveLeaveUpdatesState`、`TestEntityLockGroupTransitionPendingGatesNormalDispatch`、`TestEntityLockGroupTransitionPendingRequeuesSyncDispatch`、`TestEntityLockGroupTransitionContinuationReturnsSyncResult`、`TestEntityLockGroupTransitionRetriesWhenEntityLockBusy`；其余 2 项见文件
- [handler_meta_durability_promises_test.go](../../../framework/nest/handler_meta_durability_promises_test.go)：`TestHandlerMetaRollbackRequiresExplicitDurability`
- [isolated_contract_test.go](../../../framework/nest/isolated_contract_test.go)：`TestIsolatedInSlowPrepareOfLocalMessageDoesNotClaimIt`、`TestIsolatedInSlowPrepareOfRemoteMessageIsRefused`、`TestIsolatedInClosingPhaseOfRemoteMessageIsRefused`、`TestIsolatedInClosingPhaseOfLocalMessageDoesNotClaimIt`
- [legacy_engine_test.go](../../../framework/nest/legacy_engine_test.go)
- [lifecycle_contract_test.go](../../../framework/nest/lifecycle_contract_test.go)：`TestLifecycleCreateInHandlerWaitsForDurable`、`TestLifecycleCreateInHandlerRevokedWhenHandlerFails`、`TestLifecycleCreateInHandlerRevokedWhenStrictCommitRejected`、`TestLifecycleCreateOutsideNestUnchanged`、`TestLifecycleGetOrCreateRetriesWhileRevokeFinishes`
- [local_after_commit_sentinel_promises_test.go](../../../framework/nest/local_after_commit_sentinel_promises_test.go)：`TestLocalReleaseHookPanicAfterStrictCommitCarriesSentinel`、`TestMemoryHandlerReleaseHookPanicCarriesSentinel`、`TestUncommittedLocalReleaseHookPanicHasNoSentinel`、`TestPipelinedReleasePanicAfterDurableCarriesSentinel`
- [memory_persistent_write_promises_test.go](../../../framework/nest/memory_persistent_write_promises_test.go)：`TestMemoryTransactionPersistentWriteFailsAndRollsBack`、`TestMemoryTransactionNonPersistentWriteSucceeds`、`TestMemoryFastPathPersistentSetterStillPanics`、`TestMemoryTransactionCreatedEntityPersistentWriteFails`、`TestMemoryRemoteBatchLocalPersistentWriteRefused`；其余 1 项见文件
- [missing_entity_promises_test.go](../../../framework/nest/missing_entity_promises_test.go)：`TestASingleDispatchToAMissingEntitySaysNotFound`、`TestABroadcastSkipsMissingEntitiesAndKeepsGoing`
- [nest_test.go](../../../framework/nest/nest_test.go)：`TestTransactionPolicyParsingRejectsLegacyAndUnknownValues`、`TestHandlerHotcodePatch`、`TestDispatcherObserveStatsRecordsQueueGauge`、`TestDispatcherStatsCountsProcessedAndSlowMessages`、`TestDispatcherDelaySendMsgDoesNotCreatePerMessageGoroutines`；其余 32 项见文件
- [nested_fence_outer_commit_nestwal_promises_test.go](../../../framework/nest/nested_fence_outer_commit_nestwal_promises_test.go)：`TestOuterCommitAfterNestedAcceptFailureIsRefusedBeforeWAL`
- [nested_isolated_commit_requeue_promises_test.go](../../../framework/nest/nested_isolated_commit_requeue_promises_test.go)：`TestIsolatedCommitThenOuterLockTimeoutIsNotRequeued`、`TestIsolatedCommitThenOuterFailureCarriesSentinel`、`TestIsolatedRejectedThenOuterLockTimeoutStillRequeues`
- [nested_isolated_indeterminate_fence_promises_test.go](../../../framework/nest/nested_isolated_indeterminate_fence_promises_test.go)：`TestNestedIsolatedIndeterminateFencesBeforeReturning`、`TestNestedIsolatedIndeterminatePropagatedKeepsSentinels`
- [nested_isolated_outer_snapshot_promises_test.go](../../../framework/nest/nested_isolated_outer_snapshot_promises_test.go)：`TestNestedIsolatedWriteToOuterSnapshotIsRefused`、`TestNestedIsolatedWriteUnderMemoryOuterStillCommits`、`TestNestedIsolatedWriteToUncapturedEntityCommits`
- [nested_isolated_raw_mutation_promises_test.go](../../../framework/nest/nested_isolated_raw_mutation_promises_test.go)：`TestNestedIsolatedRawMutationToOuterSnapshotIsRefused`、`TestNestedIsolatedRawMutationUnderMemoryOuterStillCommits`、`TestNestedIsolatedRawMutationToUncapturedEntityCommits`
- [nested_isolated_remote_message_promises_test.go](../../../framework/nest/nested_isolated_remote_message_promises_test.go)：`TestNestedIsolatedInRemoteMessageIsRefused`
- [non_rollback_create_conflict_promises_test.go](../../../framework/nest/non_rollback_create_conflict_promises_test.go)：`TestNonRollbackHandlerCreateConflictIsNotRequeued`、`TestNonRollbackHandlerCreateConflictSuppressesLaterLockTimeoutRequeue`、`TestNonRollbackHandlerCreateInLockOrderStillWaits`
- [non_rollback_transient_requeue_promises_test.go](../../../framework/nest/non_rollback_transient_requeue_promises_test.go)：`TestNonRollbackHandlerCastTargetRemovedWhileWaiting`、`TestNonRollbackHandlerTransientFailureIsNotRequeued`、`TestRemoteNonRollbackHandlerTransientFailureIsNotRequeued`、`TestRollbackHandlerTransientFailureStillRequeues`、`TestRemoteNonRollbackHandlerCreateConflictIsNotRequeued`；其余 1 项见文件
- [optimization_bench_test.go](../../../framework/nest/optimization_bench_test.go)
- [persist_change_test.go](../../../framework/nest/persist_change_test.go)：`TestMarkPersistRequiresActiveTransaction`、`TestPersistChangeCoalescesOneParticipant`、`TestMarkPersistFullDropsCoveredSubpaths`、`TestAddReceiptDeduplicatesAndRejectsDigestConflict`、`TestSetReceiptPayloadPreservesBoundIdentity`；其余 8 项见文件
- [pipelined_async_test.go](../../../framework/nest/pipelined_async_test.go)：`TestAsyncCompletionFreesWorkerDuringDurableWait`、`TestAsyncCompletionKeepsSameEntityCommitOrder`、`TestAsyncCompletionIndeterminateRepliesErrorWithoutRollback`、`TestAsyncCompletionShutdownDeliversPendingReplies`、`TestAsyncCompletionKeepsOrderWhenPumpIsSaturated`；其余 4 项见文件
- [pipelined_bench_test.go](../../../framework/nest/pipelined_bench_test.go)
- [pipelined_commit_test.go](../../../framework/nest/pipelined_commit_test.go)：`TestPipelinedCommitReleasesLocksBeforeDurable`、`TestPipelinedEnqueueRejectionRollsBack`、`TestPipelinedIndeterminateAbandonsWithoutRollback`、`TestPipelinedRequiresCapableCommitter`、`TestPipelinedAllowlistGatesHandlers`；其余 2 项见文件
- [production_limits_test.go](../../../framework/nest/production_limits_test.go)：`TestDelayedAdmissionIsBounded`、`TestTickerStateIsInstanceScoped`、`TestDefaultHandlerRegistrationUsesDurableAsyncPolicy`
- [promises_test.go](../../../framework/nest/promises_test.go)：`TestTypedCastsRefuseTheWrongEntityType`、`TestCastMultiRefusesAGetterThatReturnsTheWrongCount`、`TestEngineStartRefusesAfterShutdownAndWithoutGetter`、`TestRollbackTxRefusesLateAndUncomparableRegistrations`
- [release_cast_instance_promises_test.go](../../../framework/nest/release_cast_instance_promises_test.go)：`TestReleaseCastOfDestroyedInstanceKeepsRecreatedLock`
- [remaining_config_promises_test.go](../../../framework/nest/remaining_config_promises_test.go)：`TestAsyncContinuationCapturesCurrentRuntimeGeneration`
- [remote_after_commit_sentinel_test.go](../../../framework/nest/remote_after_commit_sentinel_test.go)：`TestRemoteReplyDistinguishesCommittedFromUncommitted`、`TestRemoteReleaseHookPanicAfterCommitCarriesSentinel`
- [remote_budget_test.go](../../../framework/nest/remote_budget_test.go)：`TestRemoteBudgetRejectionReleasesIDAndWorkers`
- [remote_contract_test.go](../../../framework/nest/remote_contract_test.go)：`TestCachedRemoteAccessWithAllowStaleStillAcceptsAnOlderSnapshot`、`TestNestedCommitInRemotePrepareAlongsideCommittedLocalPartKeepsReplyText`、`TestRemoteReleaseHookPanicAfterDurableCommitStillCommits`
- [remote_close_confirmation_test.go](../../../framework/nest/remote_close_confirmation_test.go)：`TestRemoteCloseFailureStillConfirmsEntitySync`
- [remote_committed_hook_test.go](../../../framework/nest/remote_committed_hook_test.go)：`TestRemoteAfterAdmissionPanicDoesNotAbortDurableCommit`、`TestPostRemoteCommitRunsAllCallbacksAfterPanic`
- [remote_deferred_outcome_test.go](../../../framework/nest/remote_deferred_outcome_test.go)：`TestStrictRemoteConfirmTimeoutDefersPostCommitToDurableOutcome`、`TestSyncMutationRejectReleasesLaterCommits`
- [remote_dispatch_test.go](../../../framework/nest/remote_dispatch_test.go)：`TestRemoteStagesDoNotBlockCostLogicWorker`、`TestRemoteStagesPreserveOrderAndDrainBeforeShutdown`、`TestRemoteStagesCancelAndFenceAfterPrepareCloseBatch`、`TestRemoteStagesFullFastQueueReservesContinuation`、`TestRemoteStagesReturnContextAndConfirmAfterGuardRelease`
- [remote_local_executor_test.go](../../../framework/nest/remote_local_executor_test.go)：`TestNestBindsRunLocalIntoRemoteManager`
- [remote_reject_mixed_test.go](../../../framework/nest/remote_reject_mixed_test.go)：`TestRemoteRejectKeepsCommittedLocalEntitySync`
- [remote_transaction_test.go](../../../framework/nest/remote_transaction_test.go)：`TestRemoteManagedDispatchRejectsBroadcast`、`TestFinalizeRemoteWriteBatchProducesValidWALMutation`、`TestCommitRecordAcceptsMixedRemoteAndOrdinaryMutations`、`TestRemoteTransactionUsesNestOutbox`、`TestIndeterminateRemoteBatchIsNotAborted`
- [requeue_jitter_promises_test.go](../../../framework/nest/requeue_jitter_promises_test.go)：`TestSymmetricTransientRequeuesAreNotReadmittedInLockstep`
- [rolled_back_release_hook_promises_test.go](../../../framework/nest/rolled_back_release_hook_promises_test.go)：`TestRolledBackReleaseHookPanicKeepsBusinessError`
- [run_local_promises_test.go](../../../framework/nest/run_local_promises_test.go)：`TestRunLocalExecutesOnTheFastPoolAndIsBoundToTheCommitter`、`TestRunLocalFromAFastWorkerFailsFast`、`TestRunLocalIsNotOrderedBehindTheEntitysSlowPreparation`
- [runlocal_stage_test.go](../../../framework/nest/runlocal_stage_test.go)：`TestSlowHandlerRunLocalDoesNotWaitForOwnPool`
- [shared_load_detach_test.go](../../../framework/nest/shared_load_detach_test.go)：`TestSlowPreparationLeaderTimeoutDoesNotCutTheSharedLoad`
- [slow_trace_gate_test.go](../../../framework/nest/slow_trace_gate_test.go)：`TestSlowTraceGateBoundsConcurrentDiagnostics`
- [stage_metrics_test.go](../../../framework/nest/stage_metrics_test.go)：`TestNestStageMetricsFollowExecutionPaths`、`TestTickRegistrationInsideCallbackUsesNextSnapshot`、`TestSlowDispatchWatchReuseAfterTimerExpiry`
- [sync_modes_test.go](../../../framework/nest/sync_modes_test.go)：`TestNestSyncModesShareCommitAndUnlockBoundaries`、`TestNestOnChangeRunsWithoutWaitingForInterval`、`TestNestSyncRollbackPreservesOlderPending`、`TestNestPipelinedSyncWaitsForConfirmationAndUnlock`、`TestNestSyncRejectedCommitDoesNotFreezeOrPublish`；其余 3 项见文件
- [worker_budget_test.go](../../../framework/nest/worker_budget_test.go)：`TestWorkerBudgetAdmitsBurstBeforeWorkersRun`、`TestWorkerBudgetDoesNotReserveWorkersForBlockedIDs`、`TestWorkerBudget1024WorkersDrainWithSmallQueue`

### worker

- [context_test.go](../../../infra/base/worker/context_test.go)：`TestWorkerCreatesFreshContextForEveryTask`、`TestPoolGoCreatesDetachedContext`、`TestPoolGoRejectsUntrackedLifetime`
- [pool_guards_promises_test.go](../../../infra/base/worker/pool_guards_promises_test.go)：`TestTryDispatchRefusesBeforeStartAfterStopAndWhenTheQueueIsFull`
- [pool_shutdown_test.go](../../../infra/base/worker/pool_shutdown_test.go)：`TestPoolStopWithContextReturnsWhenHandlerIsBlocked`
- [worker_test.go](../../../infra/base/worker/worker_test.go)：`TestWorker_ProcessesTasksAndStops`、`TestWorker_AcceptedTasksSurviveConcurrentClose`、`TestWorker_CloseWakesIdleWorker`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../performance/STABLE-v1.24.0.md)，没有新测量则不能改容量承诺。


### 排队对象的紧凑布局

`dispatchJob`保留ID tail和多ID依赖计数；三个布尔标志集中，指针/切片放在前部。准入和就绪时刻存为相对`dispatchQueue.clockOrigin`的单调偏移，等待统计用偏移差计算。原点在构造时固定，不随转慢或停机重试改变；不能改用UnixNano，以免墙钟调整破坏等待时间。ID和计数器不缩窄。64位对象与基准结果见[调度性能记录](../../performance/NEST-DISPATCH-BENCHMARK.md)。

### 调度目标的存储所有权

`newDispatchJob`直接初始化单目标的内嵌数组并建立只读切片视图；多个目标由`dispatchIDs`复制、排序和去重。不再向通用函数传入job的可变缓冲区。队列发布后目标集合直到收尾都保持不变；消息中的目标切片变化不影响队列的依赖登记和释放。ID tail顺序和快慢池容量契约不变。
