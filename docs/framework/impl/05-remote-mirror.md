# Remote Entity 与 Mirror：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/05-remote-mirror.md)

## 如何阅读

一个公会可能被多个游戏进程使用。Remote让共享实体的写入有明确负责人，Mirror让其他进程读取它的快照。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`remoteentity`、`ownerroute`、`bus`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 同实体 outbox 保序，跨实体独立事务有界并行。
2. L2 是快照水位权威，Mongo 是数据权威。
3. interest 非零代际、完整身份与撤销水位一起校验。
4. 超时或断线不等于未提交，写额度必须按真实完成释放。

## 2A. 有界发布调度实现走读

[outbox_publish.go](../../../remoteentity/outbox_publish.go)的publishOutboxPage由持outboxSerial的唯一协调者调用。每页最多256项，worker缺省8、上限64；为事务全部Entity和重复TransactionID记录前序依赖。只有waiting归零的项才交给worker，等待热点链不占worker。

每次尝试结束释放后继，失败项保留持久Applied供恢复；取消后不再补新任务，但必须收完已经派发的结果才交还生命周期。因此这里保证依赖顺序的尝试，不承诺失败前序一定成功才尝试后继；消费仍依赖版本/水位拒绝陈旧状态。跨页先排空，保持页扫描的所有权。

[transaction_manager.go](../../../remoteentity/transaction_manager.go)的RecoverOutbox/runOutboxRepublish负责恢复扫描，publishAppliedRemoteTransaction负责发布单事务。finalizer的等待、投影确认和写许可释放需另外追踪，不能把发布调度的完成误用为提交确认。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### bus

8 个实现文件、16 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [admin.go](../../../bus/admin.go) | `DeadLetterCommand` |
| [bus.go](../../../bus/bus.go) | `IBus`、`Config`、`Bus` |
| [codec.go](../../../bus/codec.go) | `Codec` |
| [handler.go](../../../bus/handler.go) | `MsgContext`、`RpcContext`、`HandlerFunc`、`RpcHandlerFunc` |
| [jetstream_rpc.go](../../../bus/jetstream_rpc.go) | `JetStreamRPCConfig` |
| [json_codec.go](../../../bus/json_codec.go) | `JSONCodec` |
| [reliable.go](../../../bus/reliable.go) | `ReliableConfig`、`ReliableStore`、`DeadLetterBucket`、`ReliableDeadLetterStore`、`ReliableDeadLetterEntryDeleter`、`ReliableConsumer`、`RedisReliableStore`、`DeadLetterQuery`、`DeadLetterEntry` |
| [rpc_error.go](../../../bus/rpc_error.go) | 函数/方法或内部实现；见源码 |

### ownerroute

2 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [bus.go](../../../ownerroute/bus.go) | `BusTransport` |
| [route.go](../../../ownerroute/route.go) | `OwnerRoute`、`Resolver`、`Transport`、`Router` |

### remoteentity

26 个实现文件、108 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assemble.go](../../../remoteentity/assemble.go) | `AssemblyDeps`、`MongoBackendConfig`、`Assembly` |
| [assembly.go](../../../remoteentity/assembly.go) | `Stats` |
| [backend.go](../../../remoteentity/backend.go) | `StorageBackend`、`Backend` |
| [batch.go](../../../remoteentity/batch.go) | 函数/方法或内部实现；见源码 |
| [commit_encoding.go](../../../remoteentity/commit_encoding.go) | 函数/方法或内部实现；见源码 |
| [config.go](../../../remoteentity/config.go) | `Config` |
| [doc.go](../../../remoteentity/doc.go) | 函数/方法或内部实现；见源码 |
| [interest.go](../../../remoteentity/interest.go) | `InterestReplicaStore` |
| [interest_publish.go](../../../remoteentity/interest_publish.go) | 函数/方法或内部实现；见源码 |
| [interest_refresh.go](../../../remoteentity/interest_refresh.go) | `InterestRefreshStore` |
| [local_runtime.go](../../../remoteentity/local_runtime.go) | 函数/方法或内部实现；见源码 |
| [manager.go](../../../remoteentity/manager.go) | `Manager` |
| [marker.go](../../../remoteentity/marker.go) | 函数/方法或内部实现；见源码 |
| [mongo_authority.go](../../../remoteentity/mongo_authority.go) | `WriteAuthority`、`WriteGrant`、`WriteAuthorityProvider` |
| [mongo_committer.go](../../../remoteentity/mongo_committer.go) | `MongoCommitter`、`AtomicCommitStore` |
| [mongo_payload.go](../../../remoteentity/mongo_payload.go) | 函数/方法或内部实现；见源码 |
| [outbox_publish.go](../../../remoteentity/outbox_publish.go) | 函数/方法或内部实现；见源码 |
| [ownership.go](../../../remoteentity/ownership.go) | 函数/方法或内部实现；见源码 |
| [snapshot_client.go](../../../remoteentity/snapshot_client.go) | `SnapshotClient`、`SnapshotClientDeps`、`SnapshotClientStats` |
| [snapshot_l2.go](../../../remoteentity/snapshot_l2.go) | `SnapshotL2TombstoneWaitStats` |
| [syncer.go](../../../remoteentity/syncer.go) | `SnapshotReplicaStore` |
| [transaction_manager.go](../../../remoteentity/transaction_manager.go) | 函数/方法或内部实现；见源码 |
| [transaction_tracking.go](../../../remoteentity/transaction_tracking.go) | 函数/方法或内部实现；见源码 |
| [versioned_lock.go](../../../remoteentity/versioned_lock.go) | `ProcessIncarnation` |
| [versioned_lock_lua.go](../../../remoteentity/versioned_lock_lua.go) | 函数/方法或内部实现；见源码 |
| [wrapper.go](../../../remoteentity/wrapper.go) | 函数/方法或内部实现；见源码 |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Outbox`、`Test.*Interest`、`Test.*Snapshot`、`Test.*Indeterminate`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### bus

- [bus_lifecycle_test.go](../../../bus/bus_lifecycle_test.go)：`TestBusStopBeforeStartIsSafeAndFinal`、`TestBusStartFailureCleansPartialSubscriptions`、`TestBusRejectsRestartAfterStop`、`TestHandleRpcReturnsSubscribeError`、`TestHandleRejectsNilAndDuplicateRegistration`；其余 6 项见文件
- [dead_letter_admission_promises_test.go](../../../bus/dead_letter_admission_promises_test.go)：`TestRequeueDeadLettersRefusesWithoutAClientAndPartialRequeueOnAPurgeOnlyStore`、`TestAdminRegistrationAndNilBusEntryPointsRefuse`
- [jetstream_capture_promises_test.go](../../../bus/jetstream_capture_promises_test.go)：`TestJetStreamRPCRequestWithoutReplySubjectIsNotExecuted`、`TestLightweightCallReportsJetStreamCapture`
- [jetstream_rpc_in_progress_promises_test.go](../../../bus/jetstream_rpc_in_progress_promises_test.go)：`TestJetStreamRPCInProgressHeartbeatStopsAtTheRequestDeadline`
- [jetstream_rpc_test.go](../../../bus/jetstream_rpc_test.go)：`TestJetStreamRPCPublishesRequestAndDeliversResponse`、`TestJetStreamRPCRecordsTimeoutAndClearsPendingGauge`、`TestJetStreamRPCPendingGaugeIsPerMethod`、`TestJetStreamRPCHandleRpcPublishesResponseAfterHandler`、`TestBusCallStaysLightweightWhenJetStreamRPCEnabled`；其余 3 项见文件
- [jetstream_stop_promises_test.go](../../../bus/jetstream_stop_promises_test.go)：`TestJetStreamStopWaitsForInFlightHandlerAndDeliversItsResponse`、`TestJetStreamStopHandsInterruptedRequestBackToBroker`、`TestJetStreamDeliveryAfterStopDoesNotRunBusiness`
- [msg_label_bound_promises_test.go](../../../bus/msg_label_bound_promises_test.go)：`TestAsyncMessageLabelsAndDeadLetterKeysAreBoundedByRegistration`
- [promises_test.go](../../../bus/promises_test.go)：`TestDecodeRPCResponseRefusesEveryMalformedEnvelope`、`TestBusRefusesToRestartAfterStop`、`TestBusRefusesHandlerRegistrationAfterStop`、`TestBusCallsWithoutTransportNameTheMissingPiece`、`TestBusDeadLetterOperationsRefuseWithoutACapableStore`；其余 1 项见文件
- [reliable_redis_test.go](../../../bus/reliable_redis_test.go)：`TestRedisReliableStoreDeadLettersKeepNewestMaxEntries`、`TestRedisReliableStorePurgeDeadLettersDeletesOnlyQueryRange`、`TestRedisReliableStorePurgeWholeBucketReturnsEntryCount`
- [reliable_test.go](../../../bus/reliable_test.go)：`TestReliableBroadcastDedupIsPerConsumer`、`TestReliableDispatchDropGoesToDeadLetter`、`TestBusDeadLetterListAndRequeue`、`TestBusDeadLetterRequeueLimitKeepsUnselectedEntries`、`TestBusDeadLetterRequeueUsesStableIDWhenDeleteFails`；其余 2 项见文件
- [rpc_admission_promises_test.go](../../../bus/rpc_admission_promises_test.go)：`TestRefusedRPCIsAnsweredAtOnceWhenTheDispatcherIsNotRunning`、`TestRefusedRPCIsAnsweredAtOnceWhenTheQueueIsFull`
- [rpc_envelope_promises_test.go](../../../bus/rpc_envelope_promises_test.go)：`TestRPCBudgetJetStreamResponsesUseTheClientEnvelope`、`TestRPCBudgetJetStreamExpiredAndMalformedDoNotCallBusiness`、`TestRPCBudgetReliableCallerReceivesMissingHandlerRefusal`、`TestRPCBudgetMissingHandlerRetainsPublishAndMarshalFailures`
- [rpc_error_test.go](../../../bus/rpc_error_test.go)：`TestRPCErrorResponseUsesCodeReasonEnvelope`
- [rpc_method_label_bound_promises_test.go](../../../bus/rpc_method_label_bound_promises_test.go)：`TestCallerRPCMethodLabelsAreBounded`、`TestServedRPCMethodLabelsAreTheRegisteredMethods`
- [stop_contract_test.go](../../../bus/stop_contract_test.go)：`TestJetStreamRPCStopContract`
- [stop_retry_promises_test.go](../../../bus/stop_retry_promises_test.go)：`TestBusStopRetryWaitsForTheRetainedDrain`、`TestBusStopInsideHandlerAfterBudgetedStopReturns`

### ownerroute

- [guards_promises_test.go](../../../ownerroute/guards_promises_test.go)：`TestTransportAndRouterRefuseMissingParts`
- [promises_test.go](../../../ownerroute/promises_test.go)：`TestRouterRefusesInvalidKeysAndOwnerlessRoutes`
- [route_test.go](../../../ownerroute/route_test.go)：`TestRouterExecutesLocalCommand`、`TestRouterSendsRemoteCommand`

### remoteentity

- [ack_concurrency_test.go](../../../remoteentity/ack_concurrency_test.go)：`TestProjectorRetryAndFinalizerAcknowledgeConcurrently`
- [admission_context_test.go](../../../remoteentity/admission_context_test.go)：`TestColdRemoteAdmissionHonorsCallerCancellation`、`TestRemoteBatchSharesOneBoundedAdmissionDeadline`
- [assemble_admission_promises_test.go](../../../remoteentity/assemble_admission_promises_test.go)：`TestAssembleRefusesEachMissingDependency`、`TestPrepareRemoteWriteBatchRefusesWithoutManagerBackendOrFinalizeSlot`、`TestAssemblyRefusesMissingDurableAuthority`
- [assemble_dao_scope_test.go](../../../remoteentity/assemble_dao_scope_test.go)：`TestValidateRemoteManagedDaoScopes`、`TestAssembleRejectsRemoteManagedServerScopedDAO`
- [assemble_snapshot_config_promises_test.go](../../../remoteentity/assemble_snapshot_config_promises_test.go)：`TestAssembleRejectsTheSnapshotConfigNewSnapshotClientRejects`
- [assembly_lifecycle_test.go](../../../remoteentity/assembly_lifecycle_test.go)：`TestRemoteAssemblyDuplicateStart`、`TestRemoteAssemblyRetryAfterStorageFailure`、`TestRemoteAssemblyStopBeforeStartIsTerminal`、`TestRemoteAssemblyCanceledStartDoesNotSubscribe`、`TestRemoteAssemblyConcurrentLifecycleWaitIsCancelable`；其余 1 项见文件
- [assembly_replica_drain_promises_test.go](../../../remoteentity/assembly_replica_drain_promises_test.go)：`TestRemoteAssemblyStopWaitsForAnInFlightReplicaHandler`
- [assembly_subscription_retry_promises_test.go](../../../remoteentity/assembly_subscription_retry_promises_test.go)：`TestRemoteAssemblyRetryAfterSecondSubscriptionFailure`
- [authority_lock_test.go](../../../remoteentity/authority_lock_test.go)：`TestAuthorityLockUsesDurableVersionAndCleansFailedAdmission`、`TestAuthorityLockRetriesKnownLeaseExpiryBeforeBusinessAdmission`
- [batch_abort_local_test.go](../../../remoteentity/batch_abort_local_test.go)：`TestPrepareRemoteWriteBatchFailureAbortsWithoutFastPoolHop`、`TestRemoteWriteBatchAbortBeforeFinalizeStaysInPlace`、`TestRemoteWriteBatchAbortAfterFinalizeRollsBackOnLocalExecutor`
- [batch_test.go](../../../remoteentity/batch_test.go)：`TestRemoteWriteBatchUsesExplicitDeleteIntentBeforeEntityRemoval`、`TestRemoteWriteBatchUsesOnlyTransactionLocalChanges`、`TestRemoteWriteBatchMemoryCommitPublishesImmutableSnapshot`、`TestRemoteWriteBatchAsyncRetainsGateUntilWALApply`、`TestTransferRemoteOwnershipFencesPreviousOwner`；其余 9 项见文件
- [cluster_fault_integration_test.go](../../../remoteentity/cluster_fault_integration_test.go)：`TestRealRemoteRedisClusterFailover`、`TestRealRemoteRedisClusterUnreplicatedFence`
- [cluster_slot_migration_integration_test.go](../../../remoteentity/cluster_slot_migration_integration_test.go)：`TestMirrorLocalClusterSlotMigrationSnapshotReadWriteAndTombstone`
- [deferred_outcome_callback_test.go](../../../remoteentity/deferred_outcome_callback_test.go)：`TestStrictTimeoutOutcomeCommittedRunsPostCommitOnce`、`TestStrictTimeoutOutcomeRejectedDiscardsAfterUnload`、`TestDefinitelyRejectedMemoryOutcomeDiscardsAfterUnload`、`TestShutdownWithoutOutcomeKeepsPostCommitPending`、`TestOutcomeIsNotRunWhenFastPoolRefuses`
- [deferred_outcome_nest_test.go](../../../remoteentity/deferred_outcome_nest_test.go)：`TestNestDeferredPostCommitFollowsFinalizerOutcome`、`TestNestDeferredPostCommitIsNotRunOffThePool`
- [fast_stage_test.go](../../../remoteentity/fast_stage_test.go)：`TestFastWorkerRejectsRemoteWaitingBeforeMutation`
- [finalize_dao_scope_promises_test.go](../../../remoteentity/finalize_dao_scope_promises_test.go)：`TestFinalizeLockedRejectsServerScopedRemoteData`
- [finalizer_evicted_tracker_test.go](../../../remoteentity/finalizer_evicted_tracker_test.go)：`TestFinalizerQueriesDurableStatusWhenTrackerWasEvicted`
- [finalizer_mid_thaw_test.go](../../../remoteentity/finalizer_mid_thaw_test.go)：`TestOutboxMidThawDoesNotAdmitASecondPublisher`
- [finalizer_projection_load_test.go](../../../remoteentity/finalizer_projection_load_test.go)：`TestRemoteAsyncFinalizerProjectionLoad`
- [finalizer_projection_ownership_test.go](../../../remoteentity/finalizer_projection_ownership_test.go)：`TestAsyncFinalizerLeavesProjectionToProjector`、`TestOutboxOwnsPublicationAfterIndeterminateProjection`
- [finalizer_rollback_fast_pool_promises_test.go](../../../remoteentity/finalizer_rollback_fast_pool_promises_test.go)：`TestFinalizerRejectionRollbackRunsOnNestFastWorker`
- [finalizer_stop_handoff_promises_test.go](../../../remoteentity/finalizer_stop_handoff_promises_test.go)：`TestDeferredCloseIsRefusedOnceTheFinalizerHasStopped`、`TestDeferredCloseAcceptedBeforeTheStopIsStillDrained`
- [guards_promises_test.go](../../../remoteentity/guards_promises_test.go)：`TestRedisMarkerRefusesEachInvalidTransitionArgument`、`TestRemoteInterestRegistryRejectsUnusableInterest`、`TestBackendRequiresBothHalvesAndAtomicStoreForTransactions`
- [interest_generation_promises_test.go](../../../remoteentity/interest_generation_promises_test.go)：`TestStaleInterestReleaseDoesNotCancelANewerRenewal`、`TestManagerStampsInterestMessagesWithAdvancingGenerations`
- [interest_generation_seed_promises_test.go](../../../remoteentity/interest_generation_seed_promises_test.go)：`TestInterestGenerationSeedAdvancesPastIssuedGenerationsUnderAFrozenClock`
- [interest_handler_error_jetstream_integration_test.go](../../../remoteentity/interest_handler_error_jetstream_integration_test.go)：`TestRealJetStreamInterestHandlerErrorIsAcknowledged`
- [interest_overflow_expiry_promises_test.go](../../../remoteentity/interest_overflow_expiry_promises_test.go)：`TestInterestOverflowFenceExpiresOneTTLAfterTheLastRelease`、`TestInterestOverflowFencesAreOnePerConsumerAndReclaimedOnExpiry`
- [interest_payload_identity_promises_test.go](../../../remoteentity/interest_payload_identity_promises_test.go)：`TestInterestPayloadIdentityBeforeRegistryMutation`、`TestInterestWireRejectsLegacyAndPreservesOrdering`
- [interest_publish_stripe_promises_test.go](../../../remoteentity/interest_publish_stripe_promises_test.go)：`TestInterestPublishDoesNotBlockSameStripeCacheHits`、`TestInterestPublishFailureStillRollsBackOutsideTheStripeLock`
- [interest_read_budget_test.go](../../../remoteentity/interest_read_budget_test.go)：`TestInterestOutageDoesNotConsumeSnapshotReadBudget`、`TestReadInterestQueueIsBoundedAndStopDrainsPublisher`
- [interest_refresh_promises_test.go](../../../remoteentity/interest_refresh_promises_test.go)：`TestOwnerRestartWithTheSameSidGetsInterestBackWithoutWaitingForRenewal`、`TestInterestRefreshRenewsOnlyLiveInterests`、`TestInterestRefreshRequestsCoalesceAndAreValidated`、`TestInterestRefreshGapWaitEndsOnStop`、`TestInterestRefreshNeedsPushAndToleratesOldConsumers`；其余 1 项见文件
- [interest_release_full_promises_test.go](../../../remoteentity/interest_release_full_promises_test.go)：`TestInterestReleaseOnAFullRegistryStillFencesTheLateRenewal`
- [interest_test.go](../../../remoteentity/interest_test.go)：`TestRemoteInterestRegistryIsScopedAndExpires`、`TestLocalInterestCapacityPrunesExpiredAndCoalescesConcurrentRenewal`、`TestRemoteInterestRegistryHasHardCapacityLimits`、`TestRemoteSnapshotReplicaKeyIncludesFullScope`
- [lease_skip_rejection_e2e_test.go](../../../remoteentity/lease_skip_rejection_e2e_test.go)：`TestHistoricalLeaseFencedRemoteRecordRejectsAndReloads`
- [lock_generation_test.go](../../../remoteentity/lock_generation_test.go)：`TestDelayedUnlockCannotClearReacquiredGeneration`
- [lock_takeover_integration_test.go](../../../remoteentity/lock_takeover_integration_test.go)：`TestMirrorLocalSameSidRestartTakesOverTheOldIncarnationsLock`
- [lock_takeover_promises_test.go](../../../remoteentity/lock_takeover_promises_test.go)：`TestSameSidRestartTakesOverThePreviousIncarnationsSharedLock`、`TestLockTakeoverOnlyAppliesToThePreviousIncarnationOfTheSameSingletonHolder`、`TestAssembleRejectsAMalformedIncarnation`
- [manager_test.go](../../../remoteentity/manager_test.go)：`TestManagerCoalescesConcurrentWrapperCreation`、`TestWrapperCapacityEvictsIdleEntry`、`TestVersionedLockFactory`、`TestVersionedLockErrors`、`TestVersionedLockInvalidConfigurationReturnsError`；其余 2 项见文件
- [marker_epoch_format_promises_test.go](../../../remoteentity/marker_epoch_format_promises_test.go)：`TestMarkerLeaseStaysExactDecimalAtLargeEpochs`
- [marker_key_prefix_integration_test.go](../../../remoteentity/marker_key_prefix_integration_test.go)：`TestRealRedisMarkerKeyPrefixIsolatesDeployments`
- [marker_key_prefix_promises_test.go](../../../remoteentity/marker_key_prefix_promises_test.go)：`TestRedisMarkerDefaultKeyIsUnchanged`、`TestRedisMarkerKeyPrefixIsolatesDeploymentsSharingOneRedis`、`TestMarkerKeyPrefixValidation`
- [marker_test.go](../../../remoteentity/marker_test.go)：`TestMockOwnershipStorePreservesEpochAcrossModeTransitions`、`TestParseMarkerLeasePreservesModeAndFence`、`TestRedisOwnershipClaimAndEnterSharedRejectStaleLease`
- [memory_unresolved_rejection_test.go](../../../remoteentity/memory_unresolved_rejection_test.go)：`TestMemoryNeverCommittedTransientErrorGetsDurableRejection`、`TestMemoryLateCommitBeatsFinalizerRejection`、`TestWALDurabilityUnknownIsNotRejectedByFinalizer`、`TestMongoRejectUnresolvedDefersToExistingTransaction`
- [mirror_local_bench_integration_test.go](../../../remoteentity/mirror_local_bench_integration_test.go)
- [mirror_step4_jetstream_integration_test.go](../../../remoteentity/mirror_step4_jetstream_integration_test.go)：`TestRealJetStreamLiveDurableNameShape`、`TestRealJetStreamLiveSubscriptionConfirmsAndResumes`、`TestRealJetStreamLiveSnapshotPushReachesTheReader`
- [mirror_step4_promises_test.go](../../../remoteentity/mirror_step4_promises_test.go)：`TestSnapshotBootstrapBuffersDeltaDuringFirstLoad`、`TestInterestRenewReleaseConvergesInEveryDeliveryOrder`、`TestInterestCapacityIsPerConsumer`、`TestSnapshotBootstrapReplayMatrix`、`TestSnapshotBootstrapOverflowDropsTheBufferAndReloads`；其余 2 项见文件
- [mongo_authority_test.go](../../../remoteentity/mongo_authority_test.go)：`TestMongoAuthorityFencesUncommittedWriterAndReplaysCommittedTransaction`、`TestMongoAuthoritySharedTransitionsAndOverflow`、`TestMongoAuthorityRejectsUnsupportedMetadata`、`TestMongoAuthorityLostGrantReplyDoesNotAllocateTwice`、`TestOwnerRoutedAdmissionRechecksDurableOwnerDespiteHotCache`；其余 3 项见文件
- [mongo_committer_test.go](../../../remoteentity/mongo_committer_test.go)：`TestMongoCommitterCASIdempotencySnapshotAndOutbox`
- [mongo_payload_test.go](../../../remoteentity/mongo_payload_test.go)：`TestMongoPayloadBatchingPreservesDeletesAndTransactionRollback`
- [outbox_crash_integration_test.go](../../../remoteentity/outbox_crash_integration_test.go)：`TestRemoteOutboxCrashHelper`、`TestRealRemoteOutboxRecoversAfterCommitBeforePublicationCrash`
- [outbox_pagination_test.go](../../../remoteentity/outbox_pagination_test.go)：`TestOutboxFailedFirstPageDoesNotStarveLaterTransaction`
- [outbox_parallel_test.go](../../../remoteentity/outbox_parallel_test.go)：`TestOutboxIndependentTransactionsPublishWhileOverlappingTransactionWaits`、`TestOutboxWorkerLimitAndCancellationRetainUnpublishedTransactions`
- [outbox_stop_contract_test.go](../../../remoteentity/outbox_stop_contract_test.go)：`TestExplicitOutboxRecoveryIsDrainedBeforeStop`
- [owner_startup_election_integration_test.go](../../../remoteentity/owner_startup_election_integration_test.go)：`TestMirrorLocalOwnerStorageInitSurvivesAMongoElection`
- [ownership_mode_indeterminate_promises_test.go](../../../remoteentity/ownership_mode_indeterminate_promises_test.go)：`TestEnterSharedPromiseLostReplyFollowsAuthorityIntoSharedMode`、`TestEnterSharedPromiseLostReplyFreezesUntilAuthorityAnswers`、`TestLeaveSharedPromiseLostReplyFollowsAuthorityIntoLocalOwned`、`TestSharedModePromiseFailureBeforeSendRestoresPreviousMode`
- [ownership_redis_integration_test.go](../../../remoteentity/ownership_redis_integration_test.go)：`TestRealRemoteOwnershipClaimAndTransfer`
- [ownership_test.go](../../../remoteentity/ownership_test.go)：`TestConcurrentOwnershipClaimElectsExactlyOneOwner`、`TestOwnershipModeTransitionsAdvanceMarkerEpoch`
- [ownership_transfer_indeterminate_promises_test.go](../../../remoteentity/ownership_transfer_indeterminate_promises_test.go)：`TestTransferPromiseLostReplyFencesOldOwnerWhenAuthorityConfirmsTransfer`、`TestTransferPromiseLostReplyFreezesOldOwnerUntilAuthorityAnswers`、`TestTransferPromiseFailureBeforeSendRestoresOldOwner`
- [parallel_suffix_replay_e2e_test.go](../../../remoteentity/parallel_suffix_replay_e2e_test.go)：`TestParallelWindowReplaysSucceededSuffixUnderNewerFence`
- [pipelined_remote_local_fsync_promises_test.go](../../../remoteentity/pipelined_remote_local_fsync_promises_test.go)：`TestPipelinedRemoteBatchLocalRecordIsFsyncedBeforeVisible`
- [pipelined_remote_outcome_promises_test.go](../../../remoteentity/pipelined_remote_outcome_promises_test.go)：`TestPipelinedRemoteOutcomeMatchesStrict`
- [process_lock_integration_test.go](../../../remoteentity/process_lock_integration_test.go)：`TestRemoteLeaseProcessHelper`、`TestRealRemoteProcessKillRecoversLease`、`TestRealRemoteProcessPartitionFencesOldOwner`、`TestRealRemoteColdAdmissionDeadlineUnderLatency`、`TestRealRemoteDurableAuthorityProcessPartition`
- [projection_publication_hol_e2e_test.go](../../../remoteentity/projection_publication_hol_e2e_test.go)：`TestPublicationFailureDoesNotBlockLaterProjection`
- [promises_test.go](../../../remoteentity/promises_test.go)：`TestRemoteWriteBatchRefusesStepsOutOfOrder`、`TestPrepareRemoteWriteBatchRefusesOversizeAndFencedManagers`
- [redis_acquire_unknown_integration_test.go](../../../remoteentity/redis_acquire_unknown_integration_test.go)：`TestRealVersionedLockReacquiresAfterAcquireReplyLost`、`TestRealVersionedLockReacquiresAcrossUnknownChain`、`TestRealVersionedLockLateAcquireScript`
- [redis_lock_integration_test.go](../../../remoteentity/redis_lock_integration_test.go)：`TestRealVersionedLockExactCounters`、`TestRealVersionedLockCounterFailureDoesNotLeaveOwner`、`TestRealVersionedLockRenewExpiryAndHandoff`
- [redis_touch_generation_integration_test.go](../../../remoteentity/redis_touch_generation_integration_test.go)：`TestRealVersionedLockNextWriterLocksWhileOldRenewalReplyIsLate`、`TestRealVersionedLockRenewalGoroutineObservesExpiryThenNextWriterLocks`
- [redis_unlock_outage_integration_test.go](../../../remoteentity/redis_unlock_outage_integration_test.go)：`TestRealVersionedLockReacquiresAfterUnlockOutage`、`TestRealVersionedLockUnknownReleaseDefersToOtherHolder`
- [rejected_memory_quarantine_test.go](../../../remoteentity/rejected_memory_quarantine_test.go)：`TestRejectedMemoryWriteIsNotCarriedByNextWrite`、`TestRejectedAsyncWriteIsNotCarriedByNextWrite`、`TestRejectedAsyncWriteAfterQuarantineIsNotCarriedByNextWrite`、`TestRejectedStrictWriteIsNotCarriedByNextWrite`
- [rejected_memory_reload_test.go](../../../remoteentity/rejected_memory_reload_test.go)：`TestRejectedMemoryWriteReloadsFromAuthority`、`TestDefinitelyRejectedMemoryWriteReloadsFromAuthority`、`TestRejectedAsyncWriteReloadsFromAuthority`、`TestRejectedStrictWriteReloadsFromAuthority`、`TestRejectedWriteUnloadsOnNestFastPoolAndRebindsSync`
- [rejected_reload_sentinel_test.go](../../../remoteentity/rejected_reload_sentinel_test.go)：`TestRejectedWriteBeforeUnloadReturnsReloadingSentinel`、`TestDefinitelyRejectedWriteBeforeUnloadReturnsReloadingSentinel`、`TestReloadDuringUnloadReturnsReloadingSentinel`
- [rejected_resync_test.go](../../../remoteentity/rejected_resync_test.go)：`TestRejectedAsyncWriteResyncsSubscribersFromAuthority`、`TestRejectedWriteWithoutSubscribersIsNotReloaded`
- [remote_delete_ack_promises_test.go](../../../remoteentity/remote_delete_ack_promises_test.go)：`TestRemoteDeleteCommitPublishesItsTombstoneAfterTheInstanceIsCleared`
- [replay_outcome_test.go](../../../remoteentity/replay_outcome_test.go)：`TestCommittedReplayPreservesNewerLiveFenceAndDirtyState`、`TestMemoryLostCommitReplyKeepsFinalizerOwnership`、`TestMongoRejectedTransactionIsDurableAndDigestChecked`、`TestBackendForwardsConcurrentCommitCapability`、`TestDurableLeaseRejectionReleasesDeferredWriteGate`；其余 2 项见文件
- [replay_publication_e2e_test.go](../../../remoteentity/replay_publication_e2e_test.go)：`TestCommittedReplayAfterFinalizerAndNewerFenceAcksWAL`
- [reply_sentinel_table_test.go](../../../remoteentity/reply_sentinel_table_test.go)：`TestRemoteUnknownOutcomeRepliesHitDecisionTableRow2`、`TestRemoteFinalizeRejectionReplyHitsDecisionTableRow12`、`TestRemoteRejectedAfterLocalCommitReplyHitsDecisionTableRow4`、`TestRemoteStrictTrackerEvictedAfterCommitReplyHitsDecisionTableRow2`、`TestCallerWaitEndedReplyHitsDecisionTableRow14`
- [same_fence_version_regression_promises_test.go](../../../remoteentity/same_fence_version_regression_promises_test.go)：`TestSameFenceReplayCannotRewindStateVersion`
- [snapshot_client_promises_test.go](../../../remoteentity/snapshot_client_promises_test.go)：`TestSnapshotClientHasNoWriteCapability`、`TestSnapshotClientReadsWhatTheOwnerPublishesInTheSameProcess`、`TestSnapshotClientLinearizableNeedsADeclaredLoader`、`TestSnapshotClientStartFailureLeavesNoSubscription`、`TestSnapshotClientStopContract`；其余 1 项见文件
- [snapshot_delete_order_promises_test.go](../../../remoteentity/snapshot_delete_order_promises_test.go)：`TestApplyReplicaPromiseDelayedDeleteKeepsNewerSnapshot`、`TestApplyReplicaPromiseDeleteFencesDelayedOlderSnapshot`
- [snapshot_encoding_promises_test.go](../../../remoteentity/snapshot_encoding_promises_test.go)：`TestASnapshotCommitsWhicheverHalfOfTheHashSpaceItLandsIn`、`TestACommitWhoseCountersCannotBeEncodedIsRefusedUpFront`、`TestAChecksumStoredAsANumberStillReads`、`TestAChecksumIsStoredAsEightBytes`
- [snapshot_l2_cas_promises_test.go](../../../remoteentity/snapshot_l2_cas_promises_test.go)：`TestL2RejectsSameVersionWithADifferentSchemaOrCodec`、`TestL2ComparesVersionsExactlyBeyondFloatPrecision`
- [snapshot_l2_delete_promises_test.go](../../../remoteentity/snapshot_l2_delete_promises_test.go)：`TestRemoteSnapshotL2DeleteAtVersionKeepsNewerSnapshot`
- [snapshot_l2_key_prefix_integration_test.go](../../../remoteentity/snapshot_l2_key_prefix_integration_test.go)：`TestRealSnapshotL2KeyPrefixOnRedis`、`TestRealSnapshotL2KeyPrefixOnRedisCluster`
- [snapshot_l2_key_prefix_promises_test.go](../../../remoteentity/snapshot_l2_key_prefix_promises_test.go)：`TestSnapshotL2DefaultKeyIsUnchanged`、`TestSnapshotL2KeyPrefixIsolatesDeploymentsSharingOneRedis`、`TestSnapshotL2KeyPrefixValidation`、`TestAssembleWiresSnapshotL2KeyPrefix`
- [snapshot_l2_stale_backfill_promises_test.go](../../../remoteentity/snapshot_l2_stale_backfill_promises_test.go)：`TestLateReplicaOnColdNodeDoesNotPinL1BelowSharedL2`、`TestAuthoritativeLoadThatLosesTheL2RaceReturnsTheNewerSnapshot`、`TestStaleBackfillControls`
- [snapshot_l2_test.go](../../../remoteentity/snapshot_l2_test.go)：`TestRemoteSnapshotL2RejectsDelayedPublisher`、`TestRemoteSnapshotL2RejectsSameVersionDifferentContent`
- [snapshot_l2_tombstone_integration_test.go](../../../remoteentity/snapshot_l2_tombstone_integration_test.go)：`TestRealSnapshotL2StaleWriteIsReportedAndNotPinnedInL1`、`TestRealSnapshotL2TombstoneFencesEveryWriter`
- [snapshot_l2_tombstone_promises_test.go](../../../remoteentity/snapshot_l2_tombstone_promises_test.go)：`TestDeleteWatermarkHoldsInL2AgainstAnInflightLoadOnAnotherNode`、`TestDeleteWatermarkHoldsInL2AgainstALateReplicaOnAnotherNode`、`TestDeleteWatermarkControls`
- [snapshot_l2_tombstone_wait_integration_test.go](../../../remoteentity/snapshot_l2_tombstone_wait_integration_test.go)：`TestMirrorLocalTombstoneSurvivesAFailoverOnlyWithWait`、`TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary`
- [snapshot_l2_tombstone_wait_promises_test.go](../../../remoteentity/snapshot_l2_tombstone_wait_promises_test.go)：`TestSnapshotL2TombstoneWaitIsObservedButNeverFailsTheDelete`、`TestSnapshotL2TombstoneWaitDisabledAndUnsupported`、`TestSnapshotL2TombstoneWaitSettingsAreValidated`
- [snapshot_l2_watermark_bench_integration_test.go](../../../remoteentity/snapshot_l2_watermark_bench_integration_test.go)
- [snapshot_l2_watermark_matrix_integration_test.go](../../../remoteentity/snapshot_l2_watermark_matrix_integration_test.go)：`TestRealB2WatermarkMatrixStandalone`、`TestRealB2WatermarkMatrixCluster`
- [snapshot_l2_watermark_promises_test.go](../../../remoteentity/snapshot_l2_watermark_promises_test.go)：`TestB2CachedReadPastMaxStalenessReconfirmsAgainstL2`、`TestB2EntryWrittenDuringL2OutageIsNotServedUnconfirmed`、`TestB2PublisherRepairsALostL2Write`、`TestB2LostL2DeleteIsRepairedByTheNextRead`、`TestB2HistoricReplicaPastL2MemoryIsNotAdmitted`；其余 1 项见文件
- [snapshot_payload_identity_promises_test.go](../../../remoteentity/snapshot_payload_identity_promises_test.go)：`TestApplyReplicaRefusesPayloadIdentityThatContradictsTheEnvelope`
- [snapshot_read_exit_promises_test.go](../../../remoteentity/snapshot_read_exit_promises_test.go)：`TestReadRemoteSnapshotMonotonicMissLoadsAuthorityOnce`、`TestReadRemoteSnapshotMonotonicBelowMinimumLoadsAuthorityOnce`
- [snapshot_repair_admission_promises_test.go](../../../remoteentity/snapshot_repair_admission_promises_test.go)：`TestSnapshotReplicaAuthoritativeRepairAdmission`
- [snapshot_replay_jetstream_integration_test.go](../../../remoteentity/snapshot_replay_jetstream_integration_test.go)：`TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect`
- [stop_contract_test.go](../../../remoteentity/stop_contract_test.go)：`TestRemoteAssemblyStopContract`
- [transaction_tracking_test.go](../../../remoteentity/transaction_tracking_test.go)：`TestFlushRemoteAllKeepsTrackersAcrossConcurrentEviction`、`TestInvalidRemoteApplyDoesNotConsumeTrackerCapacity`、`TestRemoteTrackerEvictionUsesFirstCompletionAndKeepsPending`、`TestRemoteTrackerTTLPrunesOnlyExpiredCompletions`、`TestInvalidRemoteApplyPreservesExistingCompletion`
- [transaction_wait_eviction_promises_test.go](../../../remoteentity/transaction_wait_eviction_promises_test.go)：`TestTransactionWaitReturnsTheTerminalStatusAfterEviction`
- [unresolved_rejection_race_integration_test.go](../../../remoteentity/unresolved_rejection_race_integration_test.go)：`TestRealMongoRejectionWaitsForInFlightCommitThenReportsApplied`、`TestRealMongoRejectionWinsAfterInFlightCommitAborts`、`TestRealMongoRejectionFirstAbortsTheInFlightCommit`
- [versioned_lock_acquire_unknown_promises_test.go](../../../remoteentity/versioned_lock_acquire_unknown_promises_test.go)：`TestVersionedLockReacquiresAfterAcquireReplyLost`、`TestVersionedLockLostAcquireDefersToOtherHolder`、`TestVersionedLockUnsentAcquireKeepsUnknownReleaseReclaimable`、`TestVersionedLockReacquiresAcrossUnknownChain`、`TestVersionedLockLateAcquireScriptCannotTakeNewGeneration`；其余 2 项见文件
- [versioned_lock_release_unknown_promises_test.go](../../../remoteentity/versioned_lock_release_unknown_promises_test.go)：`TestVersionedLockReacquiresAfterUnlockRetriesExhausted`、`TestVersionedLockUnknownReleaseDefersToRedisOwner`、`TestVersionedLockUnknownReleaseAfterLeaseLapsed`、`TestVersionedLockUnlockContextExpiryEntersUnknownHold`、`TestVersionedLockUnknownReleaseRenewalFollowsNewGeneration`；其余 1 项见文件
- [versioned_lock_touch_generation_test.go](../../../remoteentity/versioned_lock_touch_generation_test.go)：`TestVersionedLockNewGenerationRenewsWhileOldTouchGoroutineExits`、`TestVersionedLockTouchRestartStressNoLostRenewal`
- [versioned_lock_unlock_test.go](../../../remoteentity/versioned_lock_unlock_test.go)：`TestVersionedLockUnlockRetryAfterLostResponseSucceeds`、`TestVersionedLockUsesNewOwnerTokenForEveryAcquisition`、`TestDelayedOldTouchDoesNotClearNewAcquisition`、`TestStaleFirstGenerationUnlockCannotEvictSecondOwner`
- [write_budget_promises_test.go](../../../remoteentity/write_budget_promises_test.go)：`TestAQueuedWriterIsBoundedByTheOperationBudget`、`TestConcurrentWritersEachReturnWithinTheBudget`、`TestACallerDeadlineShorterThanTheBudgetStillWins`
- [write_budget_test.go](../../../remoteentity/write_budget_test.go)：`TestRemoteWriteBudgetIsIndependentAndNonBlocking`、`TestRemoteWriteBudgetConfigCompatibility`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../maintenance/PERFORMANCE.md)，没有新测量则不能改容量承诺。
