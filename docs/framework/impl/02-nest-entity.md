# 核心：Nest 调度与实体：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/02-nest-entity.md)

## 1. 实现边界

`nest`、`entity`、`actionflow`、`fctx`、`lock`、`worker`、`goroutine`、`event`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 同 ID 消息准入顺序和冷目标改道不能丢位置。
2. memory 本地持久写拒绝，rollback=none 不承诺恢复。
3. pipelined 不可回滚边界早于持久确认，外化必须等确认。
4. 快池不能阻塞 I/O；同一 GuardScope 持有并释放锁。

## 2A. 事务实现走读

[nest/rollback.go](../../../nest/rollback.go)的durableCommit先区分memory无effect分支。该分支检查fence及本地持久写，不能准备一份没有任何持久落点的记录后仍返回成功。其他分支先prepareCommitRecord；空记录可返回，非空记录检查外层回滚域与fence，再要求TransactionCommitter。Commit返回未知保持未知，其他错误包装ErrCommitRejected，成功后acceptPersistence。

这一函数只覆盖同步提交分支，不能据此推断pipelined所有等待都在其中：pipelined的Enqueue/票据、解锁及完成回调必须沿执行收尾路径检查。修改时同时验证拒绝是否回滚、未知是否fence、成功回复是否晚于相应确认点。

事务participantOrder/participantChanges记录本事务变化；refuseMemoryPersistentWrite排除Remote认领的参与者和mutation，并对剩余本地内容给出实体/字段错误。不能以DAO上一次遗留dirty替代这些事务局部集合。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

### actionflow

6 个实现文件、12 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [action_runner.go](../../../actionflow/action_runner.go) | `ActionSnapshot`、`ActionRunnerHooks`、`ActionRunnerConfig`、`ActionRunner` |
| [action_types.go](../../../actionflow/action_types.go) | `ActionKind`、`ActionGroup`、`MissionKind`、`ActionStatus`、`MissionStatus`、`ActionResult`、`ActionReason`、`MissionNextMode`、`MissionNext`、`MissionStep`、`MissionPlan`、`MissionInfo` |
| [mission_runner.go](../../../actionflow/mission_runner.go) | `MissionRunnerHooks`、`MissionRunnerConfig`、`MissionRunner` |
| [plan.go](../../../actionflow/plan.go) | `PlanMission` |
| [registry.go](../../../actionflow/registry.go) | `ActionBuilder`、`MissionBuilder`、`Registry` |
| [runtime.go](../../../actionflow/runtime.go) | `ActionContext`、`Action`、`ActionList`、`MissionContext`、`Mission`、`MissionManager`、`MissionRuntimeSetter` |

### entity

28 个实现文件、46 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [category_order.go](../../../entity/category_order.go) | `EntityCategoryDef` |
| [comp_sort.go](../../../entity/comp_sort.go) | 函数/方法或内部实现；见源码 |
| [component_base.go](../../../entity/component_base.go) | `ComponentBase` |
| [entity.go](../../../entity/entity.go) | `IThreadSafeEntity`、`IThreadSafeEntityBase`、`Getter`、`LoadedChecker`、`AggregateLoader`、`IDirty`、`DaoInterface`、`DatabaseScope`、`DatabaseScopedDao`、`PersistedDaoLoader`、`Guardable`、`EntityCreateParam` |
| [entity_base.go](../../../entity/entity_base.go) | `EntityBase`、`ComponentInterfaceBase`、`ComponentManager`、`ComponentFactory`、`DaoManager` |
| [entity_factory.go](../../../entity/entity_factory.go) | `EntityBuilderFunc`、`DaoBuilderFunc`、`EntityBuilderParam` |
| [entity_group.go](../../../entity/entity_group.go) | `EntityGroupTransitionState` |
| [entity_guard.go](../../../entity/entity_guard.go) | `EntityGuard`、`GuardScope` |
| [entity_kind.go](../../../entity/entity_kind.go) | `EntityCategory`、`EntityKind`、`EntityKindCategory`、`EntityKindDef`、`ComponentType`、`EntityDestroyReason` |
| [entity_manager.go](../../../entity/entity_manager.go) | `EntityManager`、`EntityManagerOption`、`DeleteAdmission`、`DeleteAdmitter` |
| [entity_remote.go](../../../entity/entity_remote.go) | `IThreadSafeRemoteEntity`、`RemoteEntityBase` |
| [idgen.go](../../../entity/idgen.go) | `IDGen` |
| [local_executor.go](../../../entity/local_executor.go) | 函数/方法或内部实现；见源码 |
| [manager_access.go](../../../entity/manager_access.go) | `ManagerAccess` |
| [manager_factory.go](../../../entity/manager_factory.go) | `CreatedEntityCapturer` |
| [policy.go](../../../entity/policy.go) | `RemotePolicy`、`EntityLifetime` |
| [remote_manager.go](../../../entity/remote_manager.go) | `IRemoteEntityLoader`、`IRemoteEntityLocalLookup`、`IRemoteEntityUnloader`、`IRemoteEntityBackend`、`IRemoteEntityOwnershipStore`、`RemoteEntityMarkerLease`、`IRemoteEntityManager` |
| [remote_mirror.go](../../../entity/remote_mirror.go) | `RemoteObservation`、`RemoteSnapshotRead`、`RemoteSnapshotReadOnly`、`RemoteMirrorSpec`、`RemoteMirrorValue`、`RemoteMirrorReader` |
| [remote_protocol.go](../../../entity/remote_protocol.go) | `RemoteOwnershipState`、`RemoteVersionVector`、`RemoteWriteMode`、`RemoteWriteLease`、`RemoteTransactionID`、`RemotePersistChange`、`RemotePersistChangeSource`、`RemoteDeleteIntentSource`、`RemoteTransactionOutcome`、`RemoteSnapshotRecord`、`RemoteDataMutation`、`RemoteDataDelete`、`RemoteCommit`、`RemoteCommitReceipt`、`RemoteCommitState`、`RemoteCommitStatus`、`IRemoteCommitParticipant`、`IRemoteCommitter`、`IRemoteAtomicBatchCommitter`、`IRemoteCommitWaiter`、`IRemoteCommitOutbox`、`RemoteOutboxCursor`、`IRemoteCommitOutboxPager`、`IRemoteStorageInitializer`、`IRemoteSnapshotPublisher`、`IRemoteSnapshotLoader`、`RemoteWriteBatch`、`RemoteOutcomeDeferrer`、`RemoteWriteBatchManager`、`RemoteOwnershipManager`、`RemoteSnapshotInterest`、`RemoteSnapshotInterestManager`、`RemoteCommitApplier`、`RemoteSnapshotReader` |
| [remote_snapshot.go](../../../entity/remote_snapshot.go) | `RemoteReadConsistency`、`RemoteSnapshotKey`、`ImmutableRemoteSnapshot`、`FrozenRemoteSnapshotPayload`、`RemoteSnapshotEnvelope`、`RemoteSnapshotLoader`、`RemoteSnapshotCacheConfig`、`RemoteSnapshotCache`、`RemoteSnapshotReplica`、`RemoteSnapshotBootstrapStats`、`RemoteSnapshotVersionedDeleter` |
| [remote_snapshot_codec.go](../../../entity/remote_snapshot_codec.go) | `RemoteSnapshotDecodeFunc`、`RemoteSnapshotDeltaFunc`、`RemoteChecksum` |
| [remote_view.go](../../../entity/remote_view.go) | `RemoteAcquireMode`、`RemoteReadOption`、`RemoteSnapshotSource`、`RemoteSnapshot`、`RemoteSnapshotRequest`、`RemoteSnapshotProvider`、`RemoteSnapshotResolveRequest`、`RemoteViewRef` |
| [resolver.go](../../../entity/resolver.go) | `EntityIDMeta` |
| [subject_sync.go](../../../entity/subject_sync.go) | `SyncProfile`、`FrozenSyncPayload`、`SubjectSyncPacker`、`SubjectSyncPackFunc`、`SubjectSyncDirtyNotifier`、`SubjectSyncCreateParam`、`EntitySyncCreateParam`、`EntitySyncBuilderParam`、`SubjectSyncUpdate`、`SubjectSyncState`、`PreparedSubjectSync`、`PreparedSubjectSyncBatch` |
| [sync_commit.go](../../../entity/sync_commit.go) | `SyncCommitObserver`、`SyncSubjectRetractor`、`SyncChangeCollector`、`SyncMutation`、`SyncChangeMapper` |
| [sync_frozen.go](../../../entity/sync_frozen.go) | 函数/方法或内部实现；见源码 |
| [sync_view.go](../../../entity/sync_view.go) | `SyncView`、`SyncViewSet`、`NamedSyncView` |
| [unload_resync.go](../../../entity/unload_resync.go) | `UnloadedSubjectSync`、`UnloadResyncConfig`、`UnloadResyncStats` |

### event

4 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [event.go](../../../event/event.go) | `EventType`、`EventGroupType`、`EventData` |
| [event_bus.go](../../../event/event_bus.go) | `EventBus` |
| [event_handler.go](../../../event/event_handler.go) | `EventHandler`、`AsyncDispatcher` |
| [event_mgr.go](../../../event/event_mgr.go) | `EventUnit`、`EventMgr` |

### fctx

3 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [context.go](../../../fctx/context.go) | `Context`、`ContextSnapshot`、`TraceMeta`、`RequestMeta`、`Option` |
| [runtime_context.go](../../../fctx/runtime_context.go) | 函数/方法或内部实现；见源码 |
| [worker.go](../../../fctx/worker.go) | 函数/方法或内部实现；见源码 |

### goroutine

6 个实现文件、8 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [goid_prod.go](../../../goroutine/goid_prod.go) | 函数/方法或内部实现；见源码 |
| [goid_race.go](../../../goroutine/goid_race.go) | 函数/方法或内部实现；见源码 |
| [mpsc_queue.go](../../../goroutine/mpsc_queue.go) | `MPSCQueue` |
| [parallel.go](../../../goroutine/parallel.go) | `ParallelMapOption` |
| [safe_func.go](../../../goroutine/safe_func.go) | 函数/方法或内部实现；见源码 |
| [task_pool.go](../../../goroutine/task_pool.go) | `Task`、`TaskFunc`、`TaskPoolConfig`、`TaskPool` |

### lock

3 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [lock.go](../../../lock/lock.go) | `Mutex`、`LockIdGetter` |
| [lock_manager.go](../../../lock/lock_manager.go) | `MutexFactory`、`LockManager` |
| [reentrant_mutex.go](../../../lock/reentrant_mutex.go) | `ReentrantMutex` |

### nest

22 个实现文件、87 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [cast.go](../../../nest/cast.go) | `CastTarget` |
| [client.go](../../../nest/client.go) | `Client` |
| [dispatch_queue.go](../../../nest/dispatch_queue.go) | `WorkerPoolConfig` |
| [dispatcher.go](../../../nest/dispatcher.go) | `Dispatcher` |
| [dispatcher_series.go](../../../nest/dispatcher_series.go) | 函数/方法或内部实现；见源码 |
| [execution.go](../../../nest/execution.go) | 函数/方法或内部实现；见源码 |
| [group_lock.go](../../../nest/group_lock.go) | `EntityLockGroupScope` |
| [group_transition.go](../../../nest/group_transition.go) | `GroupTransitionRequest`、`GroupTransitionContinuation`、`GroupTransitionOptions`、`GroupTransitionOption` |
| [handler.go](../../../nest/handler.go) | `HandlerOptionParam`、`HandlerOption`、`BaseHandler` |
| [msg.go](../../../nest/msg.go) | `MsgType`、`Msg`、`TickMsg` |
| [nest.go](../../../nest/nest.go) | `NestMgr`、`HandlerName`、`Params`、`NestOpts`、`NestOption`、`SendOpt` |
| [nest_dispatch.go](../../../nest/nest_dispatch.go) | 函数/方法或内部实现；见源码 |
| [persist_change.go](../../../nest/persist_change.go) | `PersistChange`、`MutationParticipant` |
| [pipelined_completion.go](../../../nest/pipelined_completion.go) | 函数/方法或内部实现；见源码 |
| [remote_access.go](../../../nest/remote_access.go) | `RemoteAcquireMode`、`RemoteAccess`、`RemoteAccessProvider`、`RemoteSnapshotResolver`、`RemoteKey`、`RemoteScopeProvider`、`RemoteDefaultTTLMillisProvider` |
| [remote_dispatch.go](../../../nest/remote_dispatch.go) | 函数/方法或内部实现；见源码 |
| [rollback.go](../../../nest/rollback.go) | `RollbackPolicy`、`HandlerMeta`、`RollbackParticipant`、`RollbackTx`、`PersistFieldNamer`、`RollbackSnapshotter` |
| [slow_load.go](../../../nest/slow_load.go) | 函数/方法或内部实现；见源码 |
| [stats.go](../../../nest/stats.go) | `DispatcherWorkStats`、`DispatchLaneStats`、`DispatchQueueStats`、`DispatcherStats` |
| [ticker.go](../../../nest/ticker.go) | `TickCallbackName`、`Ticker` |
| [trace.go](../../../nest/trace.go) | 函数/方法或内部实现；见源码 |
| [transaction.go](../../../nest/transaction.go) | `DurabilityPolicy`、`TransactionID`、`EntityMutation`、`Effect`、`CommitRecord`、`CommitFence`、`CommitWAL`、`TransactionCommitter`、`TransactionReleaseNotifier`、`LocalExecutorBinder`、`CommitTicket`、`PipelinedTransactionCommitter`、`CommitParticipant` |

### worker

2 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [pool.go](../../../worker/pool.go) | `PoolConfig`、`PoolStats`、`Pool` |
| [worker.go](../../../worker/worker.go) | `Task`、`Worker` |

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Memory`、`Test.*Pipelined`、`Test.*Guard`、`Test.*Cold`。

### actionflow

- [action_runner_test.go](../../../actionflow/action_runner_test.go)：`TestActionRunnerExplicitStartPrecedesQueue`、`TestActionRunnerEnqueueStartsIdleGroup`、`TestActionRunnerQueuedStartFailureContinuesQueue`、`TestActionRunnerRecoversTickPanic`、`TestRegistrySealAndBuilderPanic`
- [deferred_mutation_promises_test.go](../../../actionflow/deferred_mutation_promises_test.go)：`TestCallbackMutationsAreDeferredUntilTheOuterCallReturns`、`TestCancelReentryCancelsTheReplacedActionOnce`、`TestDirectStartFailureEndsTheActionOnce`、`TestDeferredStartThatCanNoLongerRunStillEndsItsID`、`TestHookStartStillPrecedesQueuedActions`；其余 4 项见文件
- [execution_cleanup_test.go](../../../actionflow/execution_cleanup_test.go)：`TestOuterExecutionResetsAfterInternalPanic`
- [mission_deferred_promises_test.go](../../../actionflow/mission_deferred_promises_test.go)：`TestMissionCallbackMutationsAreDeferredUntilTheOuterCallReturns`、`TestDeferredMissionStartErrorsAreReported`、`TestMissionCallbacksThatKeepTriggeringEachOtherAreBounded`、`TestOuterMissionStartReturnsRunaway`、`TestMissionDeferredQueueIsBounded`；其余 2 项见文件
- [mission_runner_test.go](../../../actionflow/mission_runner_test.go)：`TestMissionRunnerBuildFailurePreservesCurrent`、`TestMissionRunnerStartFailureCleansState`
- [plan_guards_promises_test.go](../../../actionflow/plan_guards_promises_test.go)：`TestNormalizePlanRefusesEveryInvalidShape`、`TestPlanMissionRefusesAStepIndexOutsideThePlan`、`TestMissionRunnerHookThatEndsTheMissionDuringStartEndsItAfterTheStart`、`TestActionRunnerRefusesABuilderThatReturnsNoAction`
- [promises_test.go](../../../actionflow/promises_test.go)：`TestActionRunnerRefusesToStartInAFrozenGroup`、`TestMissionRunnerRefusesUnknownKindAndUnreplaceableMission`
- [queue_discard_promises_test.go](../../../actionflow/queue_discard_promises_test.go)：`TestDiscardedQueuedActionsAreReportedAsEnded`、`TestClearMissionKeepsAnotherMissionsQueuedActions`
- [reentrancy_promises_test.go](../../../actionflow/reentrancy_promises_test.go)：`TestActionRunnerCallbackStartReplacesTheOuterActionExactlyOnce`、`TestActionRunnerRefusesInvalidConfigUnknownGroupsAndExhaustedIDs`、`TestMissionRunnerDefersReentrantStartAndRefusesExhaustedIDsAndIdleCancel`
- [start_failure_reentrancy_promises_test.go](../../../actionflow/start_failure_reentrancy_promises_test.go)：`TestStartFailureWhoseCancelStartsAnotherActionKeepsThatAction`、`TestQueuedStartFailureWhoseCancelStartsAnotherActionKeepsThatAction`
- [types_test.go](../../../actionflow/types_test.go)：`TestActionReasonPreservesStructuredResult`
- [update_and_replace_promises_test.go](../../../actionflow/update_and_replace_promises_test.go)：`TestUpdateWhoseFnPanicsLeavesTheRunnerUsable`、`TestReplacedActionWhoseCancelPanicsStillLetsTheNewActionStart`

### entity

- [category_lock_order_promises_test.go](../../../entity/category_lock_order_promises_test.go)：`TestRegisteredCategoriesMakeTheCategoryValueTheLockOrder`
- [category_taxonomy_promises_test.go](../../../entity/category_taxonomy_promises_test.go)：`TestEntityCategoriesBeyondTheIDMaskAreAllowed`
- [destroy_cancel_promises_test.go](../../../entity/destroy_cancel_promises_test.go)：`TestEntityManagerDestroyCancelsWhileEntityLockIsHeld`、`TestEntityManagerDestroyCancellationAfterAdmissionStillFinalizes`、`TestEntityManagerDestroyPreservesReentrantLockOwnership`
- [entity_base_test.go](../../../entity/entity_base_test.go)：`TestTouchUnTouch`、`TestUnTouchPanicsOnReferenceUnderflow`、`TestTouchAfterRemoved`、`TestClearCompletesAfterCleanupHookPanic`、`TestConcurrentTouch`；其余 18 项见文件
- [entity_factory_test.go](../../../entity/entity_factory_test.go)：`TestNewEntity_Create`、`TestEntityManagerCreateUsesInstanceDependencies`、`TestEntityManagerCreateInScopeLocksBeforePublication`、`TestInitEntitySyncInstallsContentState`、`TestBuildEntity_RemoteManagedRequiresRemoteInterface`；其余 4 项见文件
- [entity_group_test.go](../../../entity/entity_group_test.go)：`TestEntityGroupBaseDefaultsAndMutation`、`TestEntityGroupManagerIndexTracksAddUpdateRemove`、`TestEntityGroupManagerLifecycleEdges`
- [entity_guard_instance_promises_test.go](../../../entity/entity_guard_instance_promises_test.go)：`TestGuardLocksNewInstanceWithDifferentMutex`、`TestGuardTreatsInstanceSharingTheHeldMutexAsHeld`、`TestGuardLockOrderCheckUsesInstance`、`TestGuardReleaseEntityKeepsSupersededLockAccounted`、`TestGuardUncomparableCustomMutexValueDoesNotPanic`；其余 2 项见文件
- [entity_manager_range_promises_test.go](../../../entity/entity_manager_range_promises_test.go)：`TestManagerRangeCallbackMayDestroyAndStopsAtFalse`、`TestManagerRangeNeverHandsOutAClearedEntity`
- [entity_manager_test.go](../../../entity/entity_manager_test.go)：`TestEntityManager_AddGet`、`TestEntityManager_GetWithCategory`、`TestEntityManager_DuplicatePanics`、`TestEntityManager_Remove`、`TestEntityManagerDestroyRequiresDurableAdmissionBeforeRemoval`；其余 11 项见文件
- [entity_pointer_contract_promises_test.go](../../../entity/entity_pointer_contract_promises_test.go)：`TestValueTypeEntityIsRejectedAtRegistrationAndPublish`
- [entity_remote_test.go](../../../entity/entity_remote_test.go)：`TestRemoteEntityBase_Interface`、`TestRemoteEntityBase_VersionVectorIsCoherent`、`TestRemoteEntityBase_IsRemoteCapable`、`TestRemoteEntityBase_TouchUnTouch`、`TestRemoteEntityBase_GUId`；其余 1 项见文件
- [example_gen_test.go](../../../entity/example_gen_test.go)
- [fast_cold_miss_promises_test.go](../../../entity/fast_cold_miss_promises_test.go)：`TestFastWorkerColdMissReturnsErrorInsteadOfPanicking`
- [guards_promises_test.go](../../../entity/guards_promises_test.go)：`TestEntityGuardsRefuseNoneKindMissingFactoriesAndLateGeneratorChanges`
- [kind_defs_batch_atomic_promises_test.go](../../../entity/kind_defs_batch_atomic_promises_test.go)：`TestRegisterEntityKindDefsLeavesNoHalfBatch`
- [kind_policy_builder_promises_test.go](../../../entity/kind_policy_builder_promises_test.go)：`TestBuiltEntityPolicyUsesKindPolicy`、`TestBuilderDefaultLifetimeFollowsKindPolicy`、`TestLateKindPolicyUpgradeConflictingWithBuilderIsRefused`
- [kind_registration_promises_test.go](../../../entity/kind_registration_promises_test.go)：`TestRegisterEntityKindCategoryRefusesEachInvalidPair`、`TestResolveEntityKindCategoryRefusesNoneAndUnregistered`、`TestEntityCreateParamNormalizeIDRefusesEachInconsistency`、`TestResolveEntityBuilderRefusesMissingKindAndBuilder`
- [kind_registry_promises_test.go](../../../entity/kind_registry_promises_test.go)：`TestKindRegistryReadsDoNotBlockOnRegistrationWrites`、`TestKindRegistryKeepsItsRegistrationRules`
- [load_flight_detach_test.go](../../../entity/load_flight_detach_test.go)：`TestSharedLoadOutlivesTheLeadersBudget`、`TestLastWaiterLeavingDoesNotCancelTheLoad`、`TestUnregisteringTheLoaderCancelsInFlightLoads`、`TestSharedLoadIsBoundedByTheFrameworkLoadTimeout`、`TestLoaderStopCauseReachesWaiters`
- [load_flight_late_local_step_promises_test.go](../../../entity/load_flight_late_local_step_promises_test.go)：`TestLateRunLocalAfterNonNestLeaderLoadCompletedDoesNotBlock`
- [load_flight_non_nest_leader_promises_test.go](../../../entity/load_flight_non_nest_leader_promises_test.go)：`TestNonNestLeaderHoldingAnotherLockColdLoadsWithoutDeadlock`、`TestNonNestLeaderHoldingLockNeededByPublishDoesNotDeadlock`
- [load_flight_promises_test.go](../../../entity/load_flight_promises_test.go)：`TestALoaderPanicDoesNotWedgeTheEntity`、`TestWaitersOfAPanickingLoadAreReleased`
- [loaded_checker_promises_test.go](../../../entity/loaded_checker_promises_test.go)：`TestManagerAccessIsLoadedNeverLoads`
- [lock_order_no_hook_promises_test.go](../../../entity/lock_order_no_hook_promises_test.go)：`TestLockOrderIsTheCategoryWithNoApplicationHook`
- [manager_access_test.go](../../../entity/manager_access_test.go)：`TestManagerAccessPreservesOrderAndMissingEntries`、`TestManagerAccessGetManyLoadsColdEntitiesConcurrently`、`TestManagerAccessColdLoadIsSingleFlight`、`TestManagerAccessFlightWaiterHonorsOwnContext`、`TestLoadedOnlyLookupNeverStartsOrJoinsColdLoad`
- [range_contract_promises_test.go](../../../entity/range_contract_promises_test.go)：`TestEntityRangeContract`
- [registry_fixture_test.go](../../../entity/registry_fixture_test.go)
- [remote_commit_promises_test.go](../../../entity/remote_commit_promises_test.go)：`TestRemoteCommitValidateRefusesEachBrokenField`
- [remote_dao_scope_registry_promises_test.go](../../../entity/remote_dao_scope_registry_promises_test.go)：`TestValidateEntityRegistryRejectsRemoteManagedServerScopedDao`
- [remote_mirror_promises_test.go](../../../entity/remote_mirror_promises_test.go)：`TestRemoteObservationCoversFollowsAdmissionOrder`、`TestRemoteMirrorReaderDTOMutationDoesNotPolluteCache`、`TestRemoteMirrorReaderNeedsNoRegistrationBesideTheOwner`、`TestRemoteMirrorReaderRejectsForeignIdentityAndSchema`、`TestRemoteSnapshotReadExitsShareOnePostCondition`
- [remote_snapshot_test.go](../../../entity/remote_snapshot_test.go)：`TestRemoteSnapshotCacheAppliesDeltaAndRejectsGap`、`TestRemoteSnapshotCacheBoundsVersionWaiters`、`TestRemoteSnapshotPayloadCannotMutateCache`、`TestRemoteSnapshotLinearizableAlwaysLoadsAuthority`、`TestRemoteSnapshotCachedMissDoesNotLoadAuthority`；其余 3 项见文件
- [remote_snapshot_write_guard_test.go](../../../entity/remote_snapshot_write_guard_test.go)：`TestRemoteSnapshotCacheWritesStayInTheListedFunctions`
- [remote_version_vector_promises_test.go](../../../entity/remote_version_vector_promises_test.go)：`TestSetRemoteVersionVectorRejectsSameFenceRegression`
- [remote_view_test.go](../../../entity/remote_view_test.go)：`TestRemoteViewRefValidatesEntityIDAndKind`、`TestRemoteReadOptionDefaultsRequireFreshData`、`TestRemoteSnapshotAcceptsVersionAndPreservesImmutableData`
- [set_entity_version_no_rewind_promises_test.go](../../../entity/set_entity_version_no_rewind_promises_test.go)：`TestSetEntityVersionRejectsRewindUnderSameFence`
- [snapshot_authoritative_admission_promises_test.go](../../../entity/snapshot_authoritative_admission_promises_test.go)：`TestAuthoritativeLoaderRejectsForeignKeyBeforePublish`、`TestAuthoritativeReadChecksStoredMinimumVersion`、`TestAuthoritativeReadRechecksExpiryAfterL2Publish`
- [snapshot_delete_l2_promises_test.go](../../../entity/snapshot_delete_l2_promises_test.go)：`TestDeleteFenceCoversInflightL2Refill`、`TestColdL1DeletePreservesNewerL2`、`TestPublishConflictAfterPreflight`
- [snapshot_delete_version_promises_test.go](../../../entity/snapshot_delete_version_promises_test.go)：`TestRemoteSnapshotDeleteAtVersionPromiseKeepsNewerSnapshot`、`TestRemoteSnapshotDeleteAtVersionPromiseFencesOlderSnapshot`、`TestRemoteSnapshotDeleteAtVersionPromiseTombstoneExpires`
- [snapshot_expiry_authoritative_promises_test.go](../../../entity/snapshot_expiry_authoritative_promises_test.go)：`TestAuthoritativeReadsNeverReturnAnExpiredSnapshot`
- [snapshot_expiry_promises_test.go](../../../entity/snapshot_expiry_promises_test.go)：`TestExpiredSnapshotIsNotServedFromCache`
- [snapshot_l2_conflict_promises_test.go](../../../entity/snapshot_l2_conflict_promises_test.go)：`TestPublishSurfacesAnL2VersionConflictAndKeepsItOutOfL1`、`TestL2BackfillCannotOverwriteAPublishedSameVersionValue`
- [snapshot_load_waiters_promises_test.go](../../../entity/snapshot_load_waiters_promises_test.go)：`TestSnapshotLoadReturnsACanceledWaitersSlot`
- [subject_sync_test.go](../../../entity/subject_sync_test.go)：`TestFrozenSyncPayloadOwnership`、`TestSubjectSyncPrepareCommitProfiles`、`TestSubjectSyncReclaimsAbandonedPrepare`、`TestSubjectSyncAbortAndConcurrentDirty`、`TestSubjectSyncSnapshotDoesNotAdvanceVersion`；其余 6 项见文件
- [sync_view_test.go](../../../entity/sync_view_test.go)：`TestFullDirtyReusesSnapshotAndEmptyViewsStillCommit`
- [unload_resync_backlog_promises_test.go](../../../entity/unload_resync_backlog_promises_test.go)：`TestUnloadResyncBacklogIsObservable`、`TestUnloadResyncBacklogIsZeroAfterStop`、`TestUnloadResyncBacklogSumsAcrossManagerAccesses`
- [unload_resync_test.go](../../../entity/unload_resync_test.go)：`TestUnloadReloadsSubscribedSubjectFromAuthority`、`TestUnloadWithoutSubscribersDoesNotReload`、`TestUnloadRetractsWhenAuthorityHasNoEntity`、`TestUnloadRetractsAfterBoundedReloadFailures`、`TestUnloadResyncSharesTheLoadWithConcurrentAccess`；其余 2 项见文件

### event

- [event_test.go](../../../event/event_test.go)：`TestEventMgr_PubSub`、`TestEventMgr_AsyncDispatch`、`TestEventMgr_Unsub`、`TestEventMgr_MultiGroup`、`TestEventBus`；其余 1 项见文件

### fctx

- [ctx_test.go](../../../fctx/ctx_test.go)：`TestCurrentContext_Basic`、`TestCurrentContext_NilWithoutStore`、`TestCurrentContext_Release`、`TestCurrentContext_NestedReleaseRestoresParent`、`TestCurrentContext_PerGoroutine`；其余 3 项见文件
- [worker_test.go](../../../fctx/worker_test.go)：`TestFastWorkerIsLocalExecutionState`

### goroutine

- [goroutine_test.go](../../../goroutine/goroutine_test.go)：`TestParallelSlice`、`TestParallelSliceCollect`
- [mpsc_queue_test.go](../../../goroutine/mpsc_queue_test.go)：`TestMPSCQueue_Basic`、`TestMPSCQueue_Full`、`TestMPSCQueue_MPSC`、`TestMPSCQueue_PowerOfTwo`
- [safe_func_test.go](../../../goroutine/safe_func_test.go)：`TestSafeFuncWithTryCountWrapsLastError`、`TestSafeFuncWithTryCountRecoversPanic`、`TestSafeFuncWithTryCountZeroCountStillRunsOnce`
- [task_pool_guards_promises_test.go](../../../goroutine/task_pool_guards_promises_test.go)：`TestSubmitRefusesAPoolThatIsNotRunningAndAClosedWorker`
- [task_pool_restart_promises_test.go](../../../goroutine/task_pool_restart_promises_test.go)：`TestTaskPoolStartAfterShutdownIsRefused`、`TestTaskPoolShutdownBeforeStartStillStopsALaterStart`
- [task_pool_shutdown_promises_test.go](../../../goroutine/task_pool_shutdown_promises_test.go)：`TestSubmitRacingShutdownNeverPanics`
- [task_pool_stats_promises_test.go](../../../goroutine/task_pool_stats_promises_test.go)：`TestTaskPoolStatsCountSubmitBeforeTheTaskCanFinish`、`TestTaskPoolStatsReadFinishedBeforeSubmitted`、`TestTaskPoolRejectedSubmitIsNotCounted`
- [task_pool_test.go](../../../goroutine/task_pool_test.go)：`TestTaskPoolNormalizesZeroWorkerCount`

### lock

- [context_test.go](../../../lock/context_test.go)：`TestLockContextCancellationPreservesOwnerAndAllowsReuse`
- [cputime_unix_test.go](../../../lock/cputime_unix_test.go)
- [cputime_windows_test.go](../../../lock/cputime_windows_test.go)
- [lock_test.go](../../../lock/lock_test.go)：`TestReentrantMutexInnerUnlockDoesNotReleaseTheLock`、`TestReentrantMutexTryLockIsReentrantForOwnerOnly`、`TestReentrantMutexUnlockWithoutOwnershipPanics`、`TestReentrantMutex_TryLock`、`TestReentrantMutex_Contention`；其余 9 项见文件
- [reentrant_mutex_bench_test.go](../../../lock/reentrant_mutex_bench_test.go)

### nest

- [admission_boundary_test.go](../../../nest/admission_boundary_test.go)：`TestPipelinedAdmissionRunsBeforeUnlock`、`TestPipelinedAdmissionPanicStillCompletesDurableTransaction`、`TestPipelinedAdmissionRejectAndEmptyRecord`
- [admission_loaded_probe_promises_test.go](../../../nest/admission_loaded_probe_promises_test.go)：`TestAdmissionNeverRunsCustomGetterOnSender`、`TestDelayedAdmissionIsNotSerializedByCustomGetter`、`TestAdmissionColdProbeRequiresLoadedChecker`
- [admission_probe_bench_test.go](../../../nest/admission_probe_bench_test.go)
- [broadcast_release_per_target_promises_test.go](../../../nest/broadcast_release_per_target_promises_test.go)：`TestBroadcastReleasesDestroyedAndRecreatedLocksPerTarget`、`TestReleaseCastOnDeclaredRemoteTargetIsNoopUntilMessageEnds`
- [cast_capture_failure_promises_test.go](../../../nest/cast_capture_failure_promises_test.go)：`TestCastCaptureFailureFailsTheTransactionEvenIfSwallowed`
- [cast_promises_test.go](../../../nest/cast_promises_test.go)：`TestCastMultiRejectsEveryMissingPrecondition`、`TestCastThreeReportsTypeMismatchOnEveryPosition`、`TestDispatchBroadcastRejectsEmptyTargets`
- [cast_removed_target_id_promises_test.go](../../../nest/cast_removed_target_id_promises_test.go)：`TestCastTargetRemovedWhileWaitingNamesTheTarget`
- [cast_test.go](../../../nest/cast_test.go)：`TestCastPlayerCanCastAllianceAndOtherByCategoryOrder`、`TestCastAllianceCanCastOtherByCategoryOrder`、`TestCastRejectsReverseCategoryOrder`、`TestCastRejectsRemoteAfterLocalBeforePreparingDistributedLock`、`TestCastRejectsInvalidTargetID`；其余 3 项见文件
- [cast_transaction_test.go](../../../nest/cast_transaction_test.go)：`TestCastTransactionRollbackWithoutSync`、`TestCastPipelinedReleasesAndStampsWithoutSync`
- [client_test.go](../../../nest/client_test.go)：`TestClientRequestCarriesContextAndReturnsResult`、`TestClientDispatchCarriesOnlyFrameworkEnvelope`、`TestClientAdmissionReportsQueueFull`、`TestClientRejectsCanceledContextBeforeAdmission`、`TestClientFenceRejectsAdmissionWithCause`；其余 1 项见文件
- [cold_target_admission_promises_test.go](../../../nest/cold_target_admission_promises_test.go)：`TestDefaultSenderPreparesColdDeclaredTargetsOnSlowWorker`、`TestDeclaredTargetEvictedAfterAdmissionMovesToSlowKeepingOrder`
- [commit_callback_panic_promises_test.go](../../../nest/commit_callback_panic_promises_test.go)：`TestCommitRunsEveryCallbackAndReportsAPanickingOne`、`TestCommitIsIdempotent`
- [commit_rejected_sentinel_promises_test.go](../../../nest/commit_rejected_sentinel_promises_test.go)：`TestPreCommitRejectionsCarryErrCommitRejected`
- [committed_requeue_promises_test.go](../../../nest/committed_requeue_promises_test.go)：`TestCommittedRemoteReplyIsNotRequeued`、`TestCommittedLocalReplyIsNotRequeued`、`TestAfterCommitPanicKeepsCauseChain`
- [completion_release_panic_test.go](../../../nest/completion_release_panic_test.go)：`TestPipelinedReleasePanicStillCompletesAndReplies`、`TestPipelinedInlineCompletionReportsCallbackFailure`
- [completion_unlock_test.go](../../../nest/completion_unlock_test.go)：`TestAsyncCompletionWaitsForEntityRelease`、`TestTickerConcurrentStartStopAlwaysClosesStartedRun`
- [create_capture_failure_promises_test.go](../../../nest/create_capture_failure_promises_test.go)：`TestCreateCaptureFailureFailsTheTransactionEvenIfSwallowed`
- [create_in_scope_commit_boundary_promises_test.go](../../../nest/create_in_scope_commit_boundary_promises_test.go)：`TestCreateInScopePipelinedWithoutWatermarkWaitsForDurable`、`TestCreateInScopeRevokedWhenHandlerFails`、`TestCreateInScopeRevokedWhenStrictCommitRejected`、`TestCreateInScopeCommittedEntitySyncsAfterCommit`、`TestCreateOutsideTransactionUnchanged`
- [create_lock_bench_test.go](../../../nest/create_lock_bench_test.go)
- [create_lock_order_promises_test.go](../../../nest/create_lock_order_promises_test.go)：`TestHandlerCreateCrossOrderResolvesWithoutDeadlock`、`TestHandlerCreateThenHigherGroupCastDoesNotFormCycle`、`TestSameIDCreateDoesNotOccupyFastPool`、`TestSwallowedCreateLockConflictStillRollsBack`、`TestMemoryHandlerCreateHoldsLockUntilHandlerEnds`
- [create_revoke_destroy_reason_promises_test.go](../../../nest/create_revoke_destroy_reason_promises_test.go)：`TestRevokedCreationIsDestroyedWithCreateRevokedReason`
- [created_entity_removal_window_shapes_promises_test.go](../../../nest/created_entity_removal_window_shapes_promises_test.go)：`TestHandlerCreateInsideDestroyWindowIsTreatedAsLockConflict`
- [created_entity_revoke_window_promises_test.go](../../../nest/created_entity_revoke_window_promises_test.go)：`TestHandlerCreateInsideRevokeWindowIsTreatedAsLockConflict`
- [created_entity_self_revoke_promises_test.go](../../../nest/created_entity_self_revoke_promises_test.go)：`TestOuterCreateAfterNestedRevokeInSameGuardFailsDeterministically`、`TestCreateInOtherManagerAfterSameIDRevokeKeepsRemovalWindowConflict`
- [cross_create_requeue_budget_promises_test.go](../../../nest/cross_create_requeue_budget_promises_test.go)：`TestSymmetricCrossCreatePairsResolveWithinRequeueBudget`
- [dataengine_record_test.go](../../../nest/dataengine_record_test.go)：`TestPrepareCommitRecordPreservesMutationIdentity`
- [delayed_shutdown_promises_test.go](../../../nest/delayed_shutdown_promises_test.go)：`TestShutdownAnswersDelayedRequestsInsteadOfDroppingThem`、`TestShutdownDropsDelayedFireAndForgetQuietly`
- [destroy_recreate_same_id_promises_test.go](../../../nest/destroy_recreate_same_id_promises_test.go)：`TestDestroyThenRecreateSameIDInHandlerLocksNewInstance`、`TestDestroyThenRecreateSameIDInHandlerRollsBack`、`TestCastAfterDestroyDoesNotReturnUnlockedRecreatedInstance`、`TestDestroyThenRecreateSameIDRemoteEntityLocksNewInstance`
- [dispatch_guard_scope_promises_test.go](../../../nest/dispatch_guard_scope_promises_test.go)：`TestDispatchLockingWithoutGuardScopeNeverReturnsCallerGuardToPool`、`TestDispatchLockingGroupRetryInScopeKeepsGuardUntilScopeEnds`、`TestDispatchEntriesRequireGuardScope`
- [dispatch_lifecycle_test.go](../../../nest/dispatch_lifecycle_test.go)：`TestBroadcastReleasePanicBalancesTouchesAndContinues`、`TestDispatchRoutesPreserveArgumentsAndReleaseOnEveryOutcome`
- [dispatch_queue_test.go](../../../nest/dispatch_queue_test.go)：`TestTwoPoolsOrderAllTargetsWithoutOccupyingFastWorker`、`TestSlowPoolUsesSharedWorkersAndBoundedWaiting`、`TestSlowLoadAndHandlerUseDifferentWorkers`、`TestQueueStatsSeparateDependencyAndWorkerWait`、`TestFastLogicMarksGetterContextAndSlowPreparationCanLoad`；其余 1 项见文件
- [dispatcher_series_lifecycle_promises_test.go](../../../nest/dispatcher_series_lifecycle_promises_test.go)：`TestDestroyedDispatchersLeaveNoSeries`、`TestADispatcherSharingItsNameKeepsTheSeries`、`TestADispatcherThatDidNotDrainKeepsItsSeriesUntilItDoes`、`TestTheSlowRerouteSeriesGoesWithItsDispatcher`
- [fast_cold_miss_promises_test.go](../../../nest/fast_cold_miss_promises_test.go)：`TestFastCastColdTargetLetsBusinessFallBack`
- [fast_stage_test.go](../../../nest/fast_stage_test.go)：`TestFastStageRejectsDirectColdAccessBeforeIO`、`TestFastContinuationRejectsSelfDispatchAndSavedSlowExecutorRunsInline`、`TestFastBroadcastReportsColdTargetAndContinues`
- [fenced_memory_remote_no_effect_promises_test.go](../../../nest/fenced_memory_remote_no_effect_promises_test.go)：`TestFencedMemoryRemoteWithoutEffectIsRefused`
- [group_lock_test.go](../../../nest/group_lock_test.go)：`TestEntityLockGroupScopeAvailableForGroupedDispatch`、`TestEntityLockGroupScopeNilForNormalDispatch`、`TestEntityLockGroupSnapshotDetectsMembershipChange`、`TestEntityLockGroupSnapshotDetectsPendingTransition`、`TestLockDispatchEntitiesForHandlerRetriesEpochChangeWhileWaiting`；其余 4 项见文件
- [group_release_panic_test.go](../../../nest/group_release_panic_test.go)：`TestGroupReleasePanicStillReleasesGroupLockAndScope`
- [group_transition_promises_test.go](../../../nest/group_transition_promises_test.go)：`TestGroupTransitionRequestsRefuseZeroGroupsUnknownEntitiesAndOverlaps`
- [group_transition_test.go](../../../nest/group_transition_test.go)：`TestEntityLockGroupTransitionJoinMoveLeaveUpdatesState`、`TestEntityLockGroupTransitionPendingGatesNormalDispatch`、`TestEntityLockGroupTransitionPendingRequeuesSyncDispatch`、`TestEntityLockGroupTransitionContinuationReturnsSyncResult`、`TestEntityLockGroupTransitionRetriesWhenEntityLockBusy`；其余 2 项见文件
- [handler_meta_durability_promises_test.go](../../../nest/handler_meta_durability_promises_test.go)：`TestHandlerMetaRollbackRequiresExplicitDurability`
- [isolated_before_message_tx_promises_test.go](../../../nest/isolated_before_message_tx_promises_test.go)：`TestIsolatedInSlowPrepareOfLocalMessageDoesNotClaimIt`、`TestIsolatedInSlowPrepareOfRemoteMessageIsRefused`
- [isolated_closing_phase_promises_test.go](../../../nest/isolated_closing_phase_promises_test.go)：`TestIsolatedInClosingPhaseOfRemoteMessageIsRefused`、`TestIsolatedInClosingPhaseOfLocalMessageDoesNotClaimIt`
- [legacy_engine_test.go](../../../nest/legacy_engine_test.go)
- [lifecycle_create_commit_boundary_promises_test.go](../../../nest/lifecycle_create_commit_boundary_promises_test.go)：`TestLifecycleCreateInHandlerWaitsForDurable`、`TestLifecycleCreateInHandlerRevokedWhenHandlerFails`、`TestLifecycleCreateInHandlerRevokedWhenStrictCommitRejected`、`TestLifecycleCreateOutsideNestUnchanged`
- [lifecycle_get_or_create_removed_promises_test.go](../../../nest/lifecycle_get_or_create_removed_promises_test.go)：`TestLifecycleGetOrCreateRetriesWhileRevokeFinishes`
- [local_after_commit_sentinel_promises_test.go](../../../nest/local_after_commit_sentinel_promises_test.go)：`TestLocalReleaseHookPanicAfterStrictCommitCarriesSentinel`、`TestMemoryHandlerReleaseHookPanicCarriesSentinel`、`TestUncommittedLocalReleaseHookPanicHasNoSentinel`、`TestPipelinedReleasePanicAfterDurableCarriesSentinel`
- [memory_persistent_write_promises_test.go](../../../nest/memory_persistent_write_promises_test.go)：`TestMemoryTransactionPersistentWriteFailsAndRollsBack`、`TestMemoryTransactionNonPersistentWriteSucceeds`、`TestMemoryFastPathPersistentSetterStillPanics`、`TestMemoryTransactionCreatedEntityPersistentWriteFails`、`TestMemoryRemoteBatchLocalPersistentWriteRefused`；其余 1 项见文件
- [missing_entity_promises_test.go](../../../nest/missing_entity_promises_test.go)：`TestASingleDispatchToAMissingEntitySaysNotFound`、`TestABroadcastSkipsMissingEntitiesAndKeepsGoing`
- [nest_test.go](../../../nest/nest_test.go)：`TestTransactionPolicyParsingRejectsLegacyAndUnknownValues`、`TestHandlerHotcodePatch`、`TestDispatcherObserveStatsRecordsQueueGauge`、`TestDispatcherStatsCountsProcessedAndSlowMessages`、`TestDispatcherDelaySendMsgDoesNotCreatePerMessageGoroutines`；其余 32 项见文件
- [nested_fence_outer_commit_nestwal_promises_test.go](../../../nest/nested_fence_outer_commit_nestwal_promises_test.go)：`TestOuterCommitAfterNestedAcceptFailureIsRefusedBeforeWAL`
- [nested_isolated_commit_requeue_promises_test.go](../../../nest/nested_isolated_commit_requeue_promises_test.go)：`TestIsolatedCommitThenOuterLockTimeoutIsNotRequeued`、`TestIsolatedCommitThenOuterFailureCarriesSentinel`、`TestIsolatedRejectedThenOuterLockTimeoutStillRequeues`
- [nested_isolated_indeterminate_fence_promises_test.go](../../../nest/nested_isolated_indeterminate_fence_promises_test.go)：`TestNestedIsolatedIndeterminateFencesBeforeReturning`、`TestNestedIsolatedIndeterminatePropagatedKeepsSentinels`
- [nested_isolated_outer_snapshot_promises_test.go](../../../nest/nested_isolated_outer_snapshot_promises_test.go)：`TestNestedIsolatedWriteToOuterSnapshotIsRefused`、`TestNestedIsolatedWriteUnderMemoryOuterStillCommits`、`TestNestedIsolatedWriteToUncapturedEntityCommits`
- [nested_isolated_raw_mutation_promises_test.go](../../../nest/nested_isolated_raw_mutation_promises_test.go)：`TestNestedIsolatedRawMutationToOuterSnapshotIsRefused`、`TestNestedIsolatedRawMutationUnderMemoryOuterStillCommits`、`TestNestedIsolatedRawMutationToUncapturedEntityCommits`
- [nested_isolated_remote_message_promises_test.go](../../../nest/nested_isolated_remote_message_promises_test.go)：`TestNestedIsolatedInRemoteMessageIsRefused`
- [non_rollback_create_conflict_promises_test.go](../../../nest/non_rollback_create_conflict_promises_test.go)：`TestNonRollbackHandlerCreateConflictIsNotRequeued`、`TestNonRollbackHandlerCreateConflictSuppressesLaterLockTimeoutRequeue`、`TestNonRollbackHandlerCreateInLockOrderStillWaits`
- [non_rollback_transient_requeue_promises_test.go](../../../nest/non_rollback_transient_requeue_promises_test.go)：`TestNonRollbackHandlerCastTargetRemovedWhileWaiting`、`TestNonRollbackHandlerTransientFailureIsNotRequeued`、`TestRemoteNonRollbackHandlerTransientFailureIsNotRequeued`、`TestRollbackHandlerTransientFailureStillRequeues`、`TestRemoteNonRollbackHandlerCreateConflictIsNotRequeued`；其余 1 项见文件
- [optimization_bench_test.go](../../../nest/optimization_bench_test.go)
- [persist_change_test.go](../../../nest/persist_change_test.go)：`TestMarkPersistRequiresActiveTransaction`、`TestPersistChangeCoalescesOneParticipant`、`TestMarkPersistFullDropsCoveredSubpaths`、`TestAddReceiptDeduplicatesAndRejectsDigestConflict`、`TestSetReceiptPayloadPreservesBoundIdentity`；其余 8 项见文件
- [pipelined_async_test.go](../../../nest/pipelined_async_test.go)：`TestAsyncCompletionFreesWorkerDuringDurableWait`、`TestAsyncCompletionKeepsSameEntityCommitOrder`、`TestAsyncCompletionIndeterminateRepliesErrorWithoutRollback`、`TestAsyncCompletionShutdownDeliversPendingReplies`、`TestAsyncCompletionKeepsOrderWhenPumpIsSaturated`；其余 4 项见文件
- [pipelined_bench_test.go](../../../nest/pipelined_bench_test.go)
- [pipelined_commit_test.go](../../../nest/pipelined_commit_test.go)：`TestPipelinedCommitReleasesLocksBeforeDurable`、`TestPipelinedEnqueueRejectionRollsBack`、`TestPipelinedIndeterminateAbandonsWithoutRollback`、`TestPipelinedRequiresCapableCommitter`、`TestPipelinedAllowlistGatesHandlers`；其余 2 项见文件
- [production_limits_test.go](../../../nest/production_limits_test.go)：`TestDelayedAdmissionIsBounded`、`TestTickerStateIsInstanceScoped`、`TestDefaultHandlerRegistrationUsesDurableAsyncPolicy`
- [promises_test.go](../../../nest/promises_test.go)：`TestTypedCastsRefuseTheWrongEntityType`、`TestCastMultiRefusesAGetterThatReturnsTheWrongCount`、`TestEngineStartRefusesAfterShutdownAndWithoutGetter`、`TestRollbackTxRefusesLateAndUncomparableRegistrations`
- [release_cast_instance_promises_test.go](../../../nest/release_cast_instance_promises_test.go)：`TestReleaseCastOfDestroyedInstanceKeepsRecreatedLock`
- [remaining_config_promises_test.go](../../../nest/remaining_config_promises_test.go)：`TestAsyncContinuationCapturesCurrentRuntimeGeneration`
- [remote_after_commit_sentinel_test.go](../../../nest/remote_after_commit_sentinel_test.go)：`TestRemoteReplyDistinguishesCommittedFromUncommitted`、`TestRemoteReleaseHookPanicAfterCommitCarriesSentinel`
- [remote_budget_test.go](../../../nest/remote_budget_test.go)：`TestRemoteBudgetRejectionReleasesIDAndWorkers`
- [remote_cached_allow_stale_promises_test.go](../../../nest/remote_cached_allow_stale_promises_test.go)：`TestCachedRemoteAccessWithAllowStaleStillAcceptsAnOlderSnapshot`
- [remote_close_confirmation_test.go](../../../nest/remote_close_confirmation_test.go)：`TestRemoteCloseFailureStillConfirmsEntitySync`
- [remote_committed_hook_test.go](../../../nest/remote_committed_hook_test.go)：`TestRemoteAfterAdmissionPanicDoesNotAbortDurableCommit`、`TestPostRemoteCommitRunsAllCallbacksAfterPanic`
- [remote_deferred_outcome_test.go](../../../nest/remote_deferred_outcome_test.go)：`TestStrictRemoteConfirmTimeoutDefersPostCommitToDurableOutcome`、`TestSyncMutationRejectReleasesLaterCommits`
- [remote_dispatch_test.go](../../../nest/remote_dispatch_test.go)：`TestRemoteStagesDoNotBlockCostLogicWorker`、`TestRemoteStagesPreserveOrderAndDrainBeforeShutdown`、`TestRemoteStagesCancelAndFenceAfterPrepareCloseBatch`、`TestRemoteStagesFullFastQueueReservesContinuation`、`TestRemoteStagesReturnContextAndConfirmAfterGuardRelease`
- [remote_local_executor_test.go](../../../nest/remote_local_executor_test.go)：`TestNestBindsRunLocalIntoRemoteManager`
- [remote_part_rejected_nested_commit_promises_test.go](../../../nest/remote_part_rejected_nested_commit_promises_test.go)：`TestNestedCommitInRemotePrepareAlongsideCommittedLocalPartKeepsReplyText`
- [remote_reject_mixed_test.go](../../../nest/remote_reject_mixed_test.go)：`TestRemoteRejectKeepsCommittedLocalEntitySync`
- [remote_release_hook_promises_test.go](../../../nest/remote_release_hook_promises_test.go)：`TestRemoteReleaseHookPanicAfterDurableCommitStillCommits`
- [remote_transaction_test.go](../../../nest/remote_transaction_test.go)：`TestRemoteManagedDispatchRejectsBroadcast`、`TestFinalizeRemoteWriteBatchProducesValidWALMutation`、`TestCommitRecordAcceptsMixedRemoteAndOrdinaryMutations`、`TestRemoteTransactionUsesNestOutbox`、`TestIndeterminateRemoteBatchIsNotAborted`
- [requeue_jitter_promises_test.go](../../../nest/requeue_jitter_promises_test.go)：`TestSymmetricTransientRequeuesAreNotReadmittedInLockstep`
- [rolled_back_release_hook_promises_test.go](../../../nest/rolled_back_release_hook_promises_test.go)：`TestRolledBackReleaseHookPanicKeepsBusinessError`
- [run_local_promises_test.go](../../../nest/run_local_promises_test.go)：`TestRunLocalExecutesOnTheFastPoolAndIsBoundToTheCommitter`、`TestRunLocalFromAFastWorkerFailsFast`、`TestRunLocalIsNotOrderedBehindTheEntitysSlowPreparation`
- [runlocal_stage_test.go](../../../nest/runlocal_stage_test.go)：`TestSlowHandlerRunLocalDoesNotWaitForOwnPool`
- [shared_load_detach_test.go](../../../nest/shared_load_detach_test.go)：`TestSlowPreparationLeaderTimeoutDoesNotCutTheSharedLoad`
- [slow_trace_gate_test.go](../../../nest/slow_trace_gate_test.go)：`TestSlowTraceGateBoundsConcurrentDiagnostics`
- [stage_metrics_test.go](../../../nest/stage_metrics_test.go)：`TestNestStageMetricsFollowExecutionPaths`、`TestTickRegistrationInsideCallbackUsesNextSnapshot`、`TestSlowDispatchWatchReuseAfterTimerExpiry`
- [sync_modes_test.go](../../../nest/sync_modes_test.go)：`TestNestSyncModesShareCommitAndUnlockBoundaries`、`TestNestOnChangeRunsWithoutWaitingForInterval`、`TestNestSyncRollbackPreservesOlderPending`、`TestNestPipelinedSyncWaitsForConfirmationAndUnlock`、`TestNestSyncRejectedCommitDoesNotFreezeOrPublish`；其余 3 项见文件
- [worker_budget_test.go](../../../nest/worker_budget_test.go)：`TestWorkerBudgetAdmitsBurstBeforeWorkersRun`、`TestWorkerBudgetDoesNotReserveWorkersForBlockedIDs`、`TestWorkerBudget1024WorkersDrainWithSmallQueue`

### worker

- [context_test.go](../../../worker/context_test.go)：`TestWorkerCreatesFreshContextForEveryTask`、`TestPoolGoCreatesDetachedContext`、`TestPoolGoRejectsUntrackedLifetime`
- [pool_guards_promises_test.go](../../../worker/pool_guards_promises_test.go)：`TestTryDispatchRefusesBeforeStartAfterStopAndWhenTheQueueIsFull`
- [pool_shutdown_test.go](../../../worker/pool_shutdown_test.go)：`TestPoolStopWithContextReturnsWhenHandlerIsBlocked`
- [worker_test.go](../../../worker/worker_test.go)：`TestWorker_ProcessesTasksAndStops`、`TestWorker_AcceptedTasksSurviveConcurrentClose`、`TestWorker_CloseWakesIdleWorker`

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../maintenance/PERFORMANCE.md)，没有新测量则不能改容量承诺。
