# 核心：DataEngine 持久化：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/03-dataengine.md)

## 如何阅读

进程突然退出后，已确认的游戏数据如何找回来？DataEngine负责把业务修改变成可恢复的持久记录。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`framework/dataengine`、`framework/nestwal`、`infra/storage/versionstore`、`framework/cache`、`infra/storage/mongo`、`infra/storage/redis`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. codec 7 单路径，拒绝旧格式不推进 checkpoint。
2. 所有 DAO schema 校验早于水合发布。
3. 投影依赖、held 与版本 CAS 保持；高版本不是幂等证明。
4. 提交结果未知 fence；成功 WAL 回复不等于 Mongo 已落库。

## 2A. 聚合恢复与投影实现走读

[EntityRepository](../../../framework/dataengine/engine/entity_repository.go)的Load合并同ID并发flight，等待该实体必要投影后进入loadAggregate。loadAggregate读取builder与完整一致性视图，先遍历全部DAO的Descriptor/SchemaVersion，再进行RestorePersisted与身份校验；只有所有DAO成功后，才RunLocal创建并发布实体及loaded hooks。前一DAO已解码、后一DAO失败时不能提前发布半实体。

readAggregate的读取回调可能因数据库事务重试再次执行，累积结果必须每次重置；flight遇panic也必须结束并唤醒等待者。这些恢复责任不应由调用方临时补sleep实现。

[codec.go](../../../framework/nestwal/codec.go)的encodeRecord与decodeRecord固定codecVersion=7；unsupported version在解释record body前拒绝。格式校验不等于尾部截断安全，WAL的文件恢复、fsync与checkpoint分别有守卫。

[projector_remote.go](../../../framework/dataengine/engine/projector_remote.go)按相邻、无共享Entity的纯Remote事务构造有界窗口：特殊effect/receipt、重复事务身份、记录/字节预算构成边界。执行窗口首次失败后停止补位、等待已派发工作，并只确认连续成功前缀。并行不能越过同实体因果依赖。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### cache

12 个实现文件、24 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [atomic_local.go](../../../framework/cache/atomic_local.go) | `ExpiringStore`、`AtomicLocalConfig`、`AtomicLocalStats`、`AtomicLocalStore` |
| [grouped_local.go](../../../framework/cache/grouped_local.go) | `GroupedLocalStore` |
| [layered.go](../../../framework/cache/layered.go) | `LayeredOptions`、`LayeredStore` |
| [local.go](../../../framework/cache/local.go) | `LocalStore`、`LocalStoreOption` |
| [mirror.go](../../../framework/cache/mirror.go) | `ReplicaConfig`、`ReplicaSyncer` |
| [read_through.go](../../../framework/cache/read_through.go) | `Loader`、`ReadThroughOptions`、`ReadThroughStats`、`ReadThroughStore` |
| [redis_hash.go](../../../framework/cache/redis_hash.go) | `RedisHashKey`、`RedisHashKeyFunc`、`RedisJSONHashStore` |
| [redis_json.go](../../../framework/cache/redis_json.go) | `RedisKeyFunc`、`RedisJSONStore` |
| [redis_raw.go](../../../framework/cache/redis_raw.go) | `RedisRawJSONStore`、`RedisRawSortedSetStore` |
| [ref_hmap.go](../../../framework/cache/ref_hmap.go) | `RefHMapKeyStringFunc`、`RefHMapPatcher`、`RefHMapConfig`、`RedisRefHMapStore` |
| [replica_local.go](../../../framework/cache/replica_local.go) | `ReplicaStore`、`ReplicaLocalStore` |
| [store.go](../../../framework/cache/store.go) | `Store`、`KeyFunc`、`StaleFunc`、`ValidateKeyFunc`、`ValidateValueFunc`、`StoreConfig` |

### dataengine

10 个实现文件、10 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [doc.go](../../../framework/dataengine/doc.go) | 函数/方法或内部实现；见源码 |
| [hook.go](../../../framework/dataengine/hook.go) | `DirtyHook` |
| [lease_fence.go](../../../framework/dataengine/lease_fence.go) | `LeaseFence` |
| [load.go](../../../framework/dataengine/load.go) | `RawDocument`、`LoadSpec`、`Store`、`EntityExister`、`LoadTemplate`、`Loader` |
| [mutation_types.go](../../../framework/dataengine/mutation_types.go) | `TransactionID`、`Durability`、`MutationKind`、`DatabaseScope`、`DocumentKey`、`FieldPatch`、`Mutation`、`Effect`、`Receipt`、`CommitRecord`、`SyncFieldMeta` |
| [projection.go](../../../framework/dataengine/projection.go) | `Descriptor`、`ProjectionTicket`、`SystemCommitter` |
| [scope.go](../../../framework/dataengine/scope.go) | 函数/方法或内部实现；见源码 |
| [sync_view.go](../../../framework/dataengine/sync_view.go) | 函数/方法或内部实现；见源码 |
| [tracker.go](../../../framework/dataengine/tracker.go) | `Tracker`、`TrackerSnapshot` |
| [validate.go](../../../framework/dataengine/validate.go) | 函数/方法或内部实现；见源码 |

### dataengine/engine

18 个实现文件、41 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assembly.go](../../../framework/dataengine/engine/assembly.go) | `AssemblyConfig`、`AssemblyDeps`、`Assembly` |
| [doc.go](../../../framework/dataengine/engine/doc.go) | 函数/方法或内部实现；见源码 |
| [entity_delete.go](../../../framework/dataengine/engine/entity_delete.go) | 函数/方法或内部实现；见源码 |
| [entity_projection.go](../../../framework/dataengine/engine/entity_projection.go) | 函数/方法或内部实现；见源码 |
| [entity_repository.go](../../../framework/dataengine/engine/entity_repository.go) | `RecoveryGate`、`EntityRepository` |
| [fenced_step.go](../../../framework/dataengine/engine/fenced_step.go) | `FenceOutcomeProjectionStore` |
| [health.go](../../../framework/dataengine/engine/health.go) | 函数/方法或内部实现；见源码 |
| [mongo_load.go](../../../framework/dataengine/engine/mongo_load.go) | 函数/方法或内部实现；见源码 |
| [mongo_projection.go](../../../framework/dataengine/engine/mongo_projection.go) | 函数/方法或内部实现；见源码 |
| [mongo_store.go](../../../framework/dataengine/engine/mongo_store.go) | `MongoStoreConfig`、`MongoStore`、`RemoteProjectionStore` |
| [operation_gate.go](../../../framework/dataengine/engine/operation_gate.go) | 函数/方法或内部实现；见源码 |
| [outbox_store.go](../../../framework/dataengine/engine/outbox_store.go) | `OutboxLease`、`OutboxItem`、`OutboxBacklog`、`OutboxStore`、`MongoOutboxStore` |
| [outbox_worker.go](../../../framework/dataengine/engine/outbox_worker.go) | `OutboxPublisher`、`OutboxWorkerOptions`、`OutboxWorkerStats`、`OutboxWorker` |
| [projection_plan.go](../../../framework/dataengine/engine/projection_plan.go) | 函数/方法或内部实现；见源码 |
| [projector.go](../../../framework/dataengine/engine/projector.go) | `ProjectionStore`、`BatchProjectionStore`、`RemoteParallelProjectionStore`、`ProjectorOptions`、`ProjectorStats`、`Projector` |
| [projector_remote.go](../../../framework/dataengine/engine/projector_remote.go) | 函数/方法或内部实现；见源码 |
| [projector_replay.go](../../../framework/dataengine/engine/projector_replay.go) | 函数/方法或内部实现；见源码 |
| [runtime.go](../../../framework/dataengine/engine/runtime.go) | `Runtime`、`PipelinedRuntimeConfig` |

### mongo

8 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [bulk.go](../../../infra/storage/mongo/bulk.go) | `WriteModel`、`WriteModelType` |
| [client.go](../../../infra/storage/mongo/client.go) | `IMongo` |
| [collection.go](../../../infra/storage/mongo/collection.go) | `ICollection`、`IStreamingCollection`、`FindOption`、`FindOneAndUpdateOption`、`UpdateResult`、`BulkWriteResult` |
| [config.go](../../../infra/storage/mongo/config.go) | `Config` |
| [database.go](../../../infra/storage/mongo/database.go) | `IDatabase` |
| [errors.go](../../../infra/storage/mongo/errors.go) | 函数/方法或内部实现；见源码 |
| [index.go](../../../infra/storage/mongo/index.go) | `IndexConflictPolicy`、`IndexModel` |
| [session.go](../../../infra/storage/mongo/session.go) | `ISession` |

### mongo/driver

4 个实现文件、9 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [client.go](../../../infra/storage/mongo/driver/client.go) | `Client` |
| [collection.go](../../../infra/storage/mongo/driver/collection.go) | `IndexMigrationPolicy` |
| [database.go](../../../infra/storage/mongo/driver/database.go) | 函数/方法或内部实现；见源码 |
| [session.go](../../../infra/storage/mongo/driver/session.go) | 函数/方法或内部实现；见源码 |

### mongo/mongotest

2 个实现文件、14 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [mongotest.go](../../../infra/storage/mongo/mongotest/mongotest.go) | `Client`、`Database`、`Collection` |
| [transaction.go](../../../infra/storage/mongo/mongotest/transaction.go) | 函数/方法或内部实现；见源码 |

### nestwal

11 个实现文件、22 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [checkpoint.go](../../../framework/nestwal/checkpoint.go) | 函数/方法或内部实现；见源码 |
| [codec.go](../../../framework/nestwal/codec.go) | 函数/方法或内部实现；见源码 |
| [dirsync_unix.go](../../../framework/nestwal/dirsync_unix.go) | 函数/方法或内部实现；见源码 |
| [dirsync_windows.go](../../../framework/nestwal/dirsync_windows.go) | 函数/方法或内部实现；见源码 |
| [effect_inbox.go](../../../framework/nestwal/effect_inbox.go) | `EffectHandler`、`MongoEffectInbox`、`EffectInboxOptions`、`JetStreamEffectConsumerConfig` |
| [inspect.go](../../../framework/nestwal/inspect.go) | `InspectOptions`、`CheckpointInspection`、`SegmentInspection`、`Inspection` |
| [inspect_snapshot.go](../../../framework/nestwal/inspect_snapshot.go) | 函数/方法或内部实现；见源码 |
| [jetstream_publisher.go](../../../framework/nestwal/jetstream_publisher.go) | `JetStreamEffectPublisher`、`EffectEnvelope` |
| [lock_unix.go](../../../framework/nestwal/lock_unix.go) | 函数/方法或内部实现；见源码 |
| [lock_windows.go](../../../framework/nestwal/lock_windows.go) | 函数/方法或内部实现；见源码 |
| [wal.go](../../../framework/nestwal/wal.go) | `Options`、`Stats`、`WAL` |

### redis

9 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [cas.go](../../../infra/storage/redis/cas.go) | `ScriptRunner`、`CompareAndSetCommand`、`CompareAndSetIndex`、`CompareAndSetResult` |
| [client.go](../../../infra/storage/redis/client.go) | `IRedis`、`EvalCall`、`DurableEvaler`、`DurableBatchEvaler`、`ReplicatedEvaler`、`ReplicatedEvalResult`、`ListTrimmer`、`ListRemover`、`Z` |
| [config.go](../../../infra/storage/redis/config.go) | `Config` |
| [errors.go](../../../infra/storage/redis/errors.go) | 函数/方法或内部实现；见源码 |
| [lock.go](../../../infra/storage/redis/lock.go) | `IDistLock`、`IDistLockFactory` |
| [pipeline.go](../../../infra/storage/redis/pipeline.go) | `IPipeline`、`FutureBytes`、`FutureStringMap`、`FutureInt64` |
| [pubsub.go](../../../infra/storage/redis/pubsub.go) | `IPubSub`、`PubSubMessage` |
| [script.go](../../../infra/storage/redis/script.go) | `IScript` |
| [versioned_lock.go](../../../infra/storage/redis/versioned_lock.go) | `IVersionedLock`、`IFencedVersionedLock`、`VersionedLockOptions`、`IVersionedLockFactory` |

### redis/driver

8 个实现文件、19 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assembly.go](../../../infra/storage/redis/driver/assembly.go) | `Assembly` |
| [client.go](../../../infra/storage/redis/driver/client.go) | `Client` |
| [cluster_recovery.go](../../../infra/storage/redis/driver/cluster_recovery.go) | 函数/方法或内部实现；见源码 |
| [lock.go](../../../infra/storage/redis/driver/lock.go) | `DistLockFactory`、`AutoExtendLock` |
| [pipeline.go](../../../infra/storage/redis/driver/pipeline.go) | 函数/方法或内部实现；见源码 |
| [pubsub.go](../../../infra/storage/redis/driver/pubsub.go) | 函数/方法或内部实现；见源码 |
| [replay.go](../../../infra/storage/redis/driver/replay.go) | 函数/方法或内部实现；见源码 |
| [replicated.go](../../../infra/storage/redis/driver/replicated.go) | 函数/方法或内部实现；见源码 |

### versionstore

5 个实现文件、14 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [codec.go](../../../infra/storage/versionstore/codec.go) | `JSONCodec` |
| [memory_store.go](../../../infra/storage/versionstore/memory_store.go) | `MemoryStore` |
| [redis_store.go](../../../infra/storage/versionstore/redis_store.go) | `RedisClient`、`RedisConfig`、`RedisIndex`、`RedisStore` |
| [versionstore.go](../../../infra/storage/versionstore/versionstore.go) | `Versioned`、`ConditionalDeleter`、`Mutate`、`Store`、`Codec`、`KeyFunc` |
| [write_token.go](../../../infra/storage/versionstore/write_token.go) | `UnknownOutcomeError` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Schema`、`Test.*Codec`、`Test.*Ack`、`Test.*Projection`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### cache

- [admission_controls_test.go](../../../framework/cache/admission_controls_test.go)：`TestCacheAdmissionRemoteErrorCompatibility`、`TestCacheAdmissionLayeredRejectedReadFailures`
- [admission_promises_test.go](../../../framework/cache/admission_promises_test.go)：`TestCacheAdmissionFatalRemotePolicy`、`TestCacheAdmissionStaleWriteAcrossStores`、`TestCacheAdmissionBackfillAdmission`
- [atomic_local_order_bounded_promises_test.go](../../../framework/cache/atomic_local_order_bounded_promises_test.go)：`TestAtomicLocalOrderBoundedUnderOverwrite`、`TestAtomicLocalOrderBoundedUnderDeleteChurn`、`TestAtomicLocalOrderBoundedUnderExpiryChurn`、`TestAtomicLocalCompactionKeepsEvictionOrder`、`TestAtomicLocalOrderBoundedConcurrent`
- [atomic_local_test.go](../../../framework/cache/atomic_local_test.go)：`TestAtomicLocalStoreVersionTTLAndBounds`、`TestReadThroughStoreCoalescesMisses`
- [cache_test.go](../../../framework/cache/cache_test.go)：`TestLocalStoreRejectsStaleVersion`、`TestLocalStoreEvictsLeastRecentlyUsedEntry`、`TestLayeredStoreReadsThroughAfterTTL`、`TestReplicaSyncerAppliesUpdate`、`TestRedisRawJSONStoreUsesRedisString`；其余 1 项见文件
- [guards_promises_test.go](../../../framework/cache/guards_promises_test.go)：`TestLayeredGetPropagatesRemoteErrorsAndDoesNotBackfillMisses`、`TestReadThroughStopsAtLocalErrorsLoaderMissesAndStrictRemoteWriteFailures`、`TestRedisStoresRefuseWritesWithoutUsableKeys`、`TestRefHMapStoreRefusesStaleWrites`
- [layered_capacity_test.go](../../../framework/cache/layered_capacity_test.go)：`TestLayeredColdKeysHaveBoundedMetadata`、`TestLayeredExpiryEvictionRevalidatesAuthority`、`TestLayeredExpiryChurnAndColdExpiration`
- [layered_expired_veto_promises_test.go](../../../framework/cache/layered_expired_veto_promises_test.go)：`TestLayeredExpiredLocalCopyDoesNotVetoAuthority`、`TestLayeredExpiredLocalCopyDoesNotVetoAuthorityRealRedis`
- [read_through_loader_fill_promises_test.go](../../../framework/cache/read_through_loader_fill_promises_test.go)：`TestReadThroughLoaderFillRefusalIsNotReadFailure`
- [read_through_promises_test.go](../../../framework/cache/read_through_promises_test.go)：`TestReadThroughRemoteFailurePolicyIsHonouredOnGetAndSet`
- [read_through_waiters_promises_test.go](../../../framework/cache/read_through_waiters_promises_test.go)：`TestReadThroughReturnsACanceledWaitersSlot`
- [redis_lost_write_ttl_promises_test.go](../../../framework/cache/redis_lost_write_ttl_promises_test.go)：`TestAWriteWhoseReplyIsLostStillGetsItsTTL`
- [ref_hmap_boundary_test.go](../../../framework/cache/ref_hmap_boundary_test.go)：`TestRefHMapBoundaryContractsRealRedis`、`TestRefHMapLayoutRejectsAliasesBeforeIO`
- [ref_hmap_contracts_test.go](../../../framework/cache/ref_hmap_contracts_test.go)：`TestRefHMapContractsRealRedis`
- [ref_hmap_patch_promises_test.go](../../../framework/cache/ref_hmap_patch_promises_test.go)：`TestRefHMapPatchPathRefusesEachShapeItCannotAddress`、`TestRefHMapLayoutRefusesTypesDeeperThanTheLimit`、`TestRedisJSONStoreRefusesStaleWritesAndMissingKeyFunc`
- [ref_hmap_patch_ttl_promises_test.go](../../../framework/cache/ref_hmap_patch_ttl_promises_test.go)：`TestRefHMapPatchKeepsTheWholeRecordAliveRealRedis`、`TestRefHMapGetTreatsAMissingReferencedHashAsMiss`
- [ref_hmap_registry_coverage_promises_test.go](../../../framework/cache/ref_hmap_registry_coverage_promises_test.go)：`TestRefHMapRegistryGuardAcceptsSameLayoutConcurrencyRealRedis`、`TestLayeredDeleteDropsL1WhenRemoteDeleteFails`
- [ref_hmap_schema_promises_test.go](../../../framework/cache/ref_hmap_schema_promises_test.go)：`TestRefHMapSchemaRegistryPromises`
- [ref_hmap_test.go](../../../framework/cache/ref_hmap_test.go)：`TestRedisRefHMapStoreStoresNestedStructsAsSameSlotKeyRefs`、`TestRedisRefHMapStoreSetDeletesPreviouslyRegisteredKeys`、`TestRedisRefHMapStoreDeleteUsesRegisteredKeys`、`TestRedisRefHMapStorePatchUpdatesOnlyTargetScalar`、`TestRedisRefHMapStoreTreatsTimeAsScalar`；其余 2 项见文件
- [ref_hmap_unknown_write_test.go](../../../framework/cache/ref_hmap_unknown_write_test.go)：`TestRefHMapWriteErrorBoundaries`、`TestRefHMapUnknownWrite`
- [replica_order_promises_test.go](../../../framework/cache/replica_order_promises_test.go)：`TestReplicaDeleteAndUpsertShareVersionOrder`、`TestReplicaRejectsMissingIdentityExtractors`、`TestReplicaLocalStoreRetainsDeleteWatermarkAtCapacity`、`TestReplicaLocalStoreConcurrentDeletesAndWrites`
- [replica_payload_identity_promises_test.go](../../../framework/cache/replica_payload_identity_promises_test.go)：`TestReplicaPayloadIdentityBeforeStoreMutation`、`TestReplicaNullPointerRefusedBeforeIdentityCallback`
- [replica_stop_promises_test.go](../../../framework/cache/replica_stop_promises_test.go)：`TestReplicaSyncerStopWaitsForInFlightStoreWrite`
- [stale_write_test.go](../../../framework/cache/stale_write_test.go)：`TestStaleSetIsReportedNotSwallowed`、`TestStaleSetAcceptsFirstWrite`、`TestLayeredStoreWithoutTTLAlwaysRevalidates`、`TestLayeredStoreServesFromL1WithinTTL`、`TestLayeredStoreWithoutRemoteStillServesLocal`

### dataengine

- [architecture_test.go](../../../framework/dataengine/architecture_test.go)：`TestLegacyCheckpointWritePathIsAbsent`
- [guards_promises_test.go](../../../framework/dataengine/guards_promises_test.go)：`TestLeaseFenceLoaderAndTrackerGuards`
- [lease_fence_test.go](../../../framework/dataengine/lease_fence_test.go)：`TestLeaseFenceReceiptRejectsIdentityAndPayloadTampering`
- [load_test.go](../../../framework/dataengine/load_test.go)：`TestLoaderRespectsDependenciesAndSkipsLoadedOrDeletedDocuments`、`TestLoaderRejectsUnknownCircularAndStrictCallbackFailures`、`TestLoaderNonStrictSkipIsCountedAndStrictStillFails`
- [model_test.go](../../../framework/dataengine/model_test.go)：`TestValidateMutationRequiresExactNextVersion`、`TestValidatePatchRejectsFullFallback`、`TestCommitRecordEmptyIncludesReceipts`、`TestValidateMutationRejectsUnsafePatchPath`、`TestCloneEffectPreservesAndDetachesHeaders`
- [projection_test.go](../../../framework/dataengine/projection_test.go)：`TestWaitProjectionHonorsTicketAndContext`
- [scope_test.go](../../../framework/dataengine/scope_test.go)：`TestResolveDatabaseScopeDefaultsAndUsesTypedScope`、`TestMapPatchPathBuildsOnlySafeNestedPaths`
- [sync_view_test.go](../../../framework/dataengine/sync_view_test.go)：`TestNamedSyncViewsSelectFieldsAndRejectUnknownViews`
- [tracker_test.go](../../../framework/dataengine/tracker_test.go)：`TestTrackerAcceptVersionIsConsecutiveAndCompareAndSwap`、`TestTrackerAdvanceVersionAllowsRemoteEntityJumpsAndIsIdempotent`、`TestTrackerPersistenceAcceptDoesNotConsumeSyncDirty`、`TestTrackerRollbackSyncRestoresMask`、`TestTrackerSnapshotRestoreCoversVersionAndSyncState`；其余 2 项见文件
- [validate_promises_test.go](../../../framework/dataengine/validate_promises_test.go)：`TestValidateMutationRefusesEachMalformedShape`、`TestValidateMutationCrossChecksRemoteCommitAgainstHeader`、`TestValidateCommitRecordRefusesEachMalformedPart`、`TestLeaseFenceValidateRefusesEachIncompleteFence`

### dataengine/engine

- [admission_reserve_benchmark_test.go](../../../framework/dataengine/engine/admission_reserve_benchmark_test.go)
- [assembly_promises_test.go](../../../framework/dataengine/engine/assembly_promises_test.go)：`TestAssembleRefusesEachMissingDependency`、`TestJetStreamOutboxPublisherRefusesIncompleteEffects`
- [assembly_retry_shutdown_owner_test.go](../../../framework/dataengine/engine/assembly_retry_shutdown_owner_test.go)：`TestAssemblyRetriedShutdownHandsOverWALWithoutCheckpointRegression`
- [assembly_shutdown_promises_test.go](../../../framework/dataengine/engine/assembly_shutdown_promises_test.go)：`TestAssemblyKeepsTheRuntimeUntilShutdownCompletes`
- [backlog_metrics_promises_test.go](../../../framework/dataengine/engine/backlog_metrics_promises_test.go)：`TestProjectionAdmissionAndAckPublishBacklogGauge`、`TestOutboxSamplingPublishesBacklogAndAge`
- [batch_limits_test.go](../../../framework/dataengine/engine/batch_limits_test.go)：`TestMultiBatchCapabilityAndSpecialBoundaries`、`TestMultiCheckpointThresholdsAndFailurePrefix`、`TestMultiCheckpointLossKeepsEntireSuccessfulPrefix`、`TestMultiCheckpointFlushesBeforeHeldAndOnCancel`、`TestMarkerlessSingleRecordStillCheckpointsImmediately`；其余 4 项见文件
- [entity_delete_fast_worker_promises_test.go](../../../framework/dataengine/engine/entity_delete_fast_worker_promises_test.go)：`TestFastWorkerRemoteDeleteIsDefiniteRejection`、`TestDeleteAdmissionOtherPanicStaysIndeterminate`
- [entity_delete_promises_test.go](../../../framework/dataengine/engine/entity_delete_promises_test.go)：`TestDeleteAdmissionRejectsUnconfiguredRuntime`、`TestDeleteAdmissionInsideTransactionRequiresPreparerAndDeclaredRemoteTarget`、`TestRemoteDeleteAdmissionRequiresRemoteWriteCapability`、`TestLocalDeleteAdmissionRequiresPreparer`
- [entity_delete_test.go](../../../framework/dataengine/engine/entity_delete_test.go)：`TestDataEngineDeleteDefersMemoryRemovalUntilTransactionAdmission`、`TestDataEngineDeleteRollbackLeavesEntityLive`、`TestDataEngineDeleteAdmissionPanicFencesAndStopsServingEntity`
- [entity_repository_flight_promises_test.go](../../../framework/dataengine/engine/entity_repository_flight_promises_test.go)：`TestARepositoryLoadPanicDoesNotWedgeTheAggregate`、`TestRepositoryWaitersOfAPanickingLoadAreReleased`
- [entity_repository_kind_policy_promises_test.go](../../../framework/dataengine/engine/entity_repository_kind_policy_promises_test.go)：`TestRepositoryRestoresRemoteEnvelopeByKindPolicy`、`TestRepositoryRejectsManagedKindRecordWithoutEnvelope`
- [entity_repository_load_promises_test.go](../../../framework/dataengine/engine/entity_repository_load_promises_test.go)：`TestEntityRepositoryRefusesEachUnloadableAggregate`
- [entity_repository_promises_test.go](../../../framework/dataengine/engine/entity_repository_promises_test.go)：`TestEntityRepositoryRefusesEachCorruptAggregateShape`
- [entity_repository_schema_test.go](../../../framework/dataengine/engine/entity_repository_schema_test.go)：`TestEntityRepositoryRejectsSchemaMismatchBeforePublishing`
- [entity_repository_test.go](../../../framework/dataengine/engine/entity_repository_test.go)：`TestEntityRepositorySingleFlightsCompleteAggregate`、`TestEntityRepositoryRecoveryBarrierAndIncompleteAggregate`、`TestEntityRepositoryRejectsTombstone`、`TestEntityRepositoryTreatsUniformAbsenceAsNotFound`、`TestEntityRepositoryPreservesRemoteVersionVector`；其余 4 项见文件
- [failure_integration_test.go](../../../framework/dataengine/engine/failure_integration_test.go)：`TestNATSOutageDoesNotBlockProjectionAndBacklogRecoversByEffectID`
- [fenced_eviction_once_test.go](../../../framework/dataengine/engine/fenced_eviction_once_test.go)：`TestSkippedFencedStepIsQueuedForEvictionOnce`、`TestTerminalAckWakesWaitersOfAPendingEviction`
- [fenced_step_resync_test.go](../../../framework/dataengine/engine/fenced_step_resync_test.go)：`TestSkippedFencedStepResyncsSubscribersFromMongo`、`TestSkippedFencedStepWithoutAuthorityRetractsSubscribers`
- [lease_fence_integration_test.go](../../../framework/dataengine/engine/lease_fence_integration_test.go)：`TestRealMongoFencedProjectionSerializesWithLeaseTakeover`
- [lease_fence_skip_promises_test.go](../../../framework/dataengine/engine/lease_fence_skip_promises_test.go)：`TestFencedLocalStepSkippedAfterLeaseExpiryDoesNotFenceNextTransaction`、`TestFencedLocalStepAppliedReleasesBarrierWithoutEviction`、`TestFencedStepRedeliveryWaitsOnlyUntilThePreviousRecordSettles`、`TestFencedStepBarrierIsReleasedOnEveryExit`、`TestFencedStepSkippedDuringStartupRecoveryNeedsNoEviction`；其余 1 项见文件
- [lifecycle_test.go](../../../framework/dataengine/engine/lifecycle_test.go)：`TestRuntimeRepeatedStartPreservesReadyAndOutbox`、`TestAssemblyConcurrentStartsShareRuntime`、`TestAssemblyShutdownWaitsForStartingOwner`、`TestAssemblyCanceledRecoveryRetainsCleanupOwnership`、`TestAssemblyOwnsWALWhenProjectorDoesNot`；其余 1 项见文件
- [mongo_benchmark_test.go](../../../framework/dataengine/engine/mongo_benchmark_test.go)
- [mongo_load_test.go](../../../framework/dataengine/engine/mongo_load_test.go)：`TestMongoStoreLoadCopiesVersionSchemaAndRemoteEnvelope`、`TestMongoStoreLoadExcludesTombstonesAndHonoursCallerFilter`、`TestMongoStoreStreamLoadUsesCursorAndStopsOnConsumerError`、`TestMongoStoreReadConsistentUsesOneSession`、`TestMongoStoreReadConsistentRetriesCallback`
- [mongo_store_test.go](../../../framework/dataengine/engine/mongo_store_test.go)：`TestMongoStoreUsesAbsoluteExpiryForReceipts`、`TestMongoStoreLeaseFenceSkipsStaleSagaTransaction`、`TestMongoStoreLeaseFenceRejectsExpiredLease`、`TestMongoStoreSkippedLeaseFenceNeverPublishesRemoteCommit`、`TestMongoStoreLeaseFenceAppliesOnlyMatchingOwnerAndToken`；其余 21 项见文件
- [outbox_lifecycle_test.go](../../../framework/dataengine/engine/outbox_lifecycle_test.go)：`TestOutboxCloseBeforeStartIsTerminal`、`TestOutboxConcurrentStartClose`
- [outbox_retry_test.go](../../../framework/dataengine/engine/outbox_retry_test.go)：`TestOutboxRetryDelayStartsAfterFailedPublish`
- [outbox_store_test.go](../../../framework/dataengine/engine/outbox_store_test.go)：`TestMongoOutboxStoreClaimTakesLeaseByTokenCAS`、`TestMongoOutboxStoreSkipsEffectsNotYetAvailable`、`TestMongoOutboxStoreAckRequiresMatchingLease`、`TestMongoOutboxStoreNackReschedulesAndCountsAttempt`、`TestMongoOutboxStoreBacklogReportsPendingAndOldestAge`
- [outbox_worker_failures_test.go](../../../framework/dataengine/engine/outbox_worker_failures_test.go)：`TestOutboxWorkerLogsStoreFailureStreakOnceAndItsRecovery`、`TestOutboxWorkerDoesNotReportShutdownAsAStoreFailure`、`TestDataEngineHealthMessageReportsStoreFailures`
- [outbox_worker_test.go](../../../framework/dataengine/engine/outbox_worker_test.go)：`TestOutboxWorkerAcknowledgesSuccessfulPublishByEffectID`、`TestOutboxWorkerHardLimitFencesWithoutTreatingPublishFailureAsProjectionFailure`、`TestOutboxWorkerRateLimitsBacklogProbe`、`TestOutboxInfrastructureIndexesCoverClaimAndBacklogQueries`
- [pipelined_projection_promises_test.go](../../../framework/dataengine/engine/pipelined_projection_promises_test.go)：`TestPipelinedCommitIsProjectedOnceDurableWithoutWaitingForIdlePoll`、`TestCommitAndEnqueueRejectAnOversizedRecordAlikeAndLeaveNoAdmission`
- [projection_lifetime_test.go](../../../framework/dataengine/engine/projection_lifetime_test.go)：`TestProjectorCloseWaitsForExternalProjection`、`TestEntityProjectionBarrierTracksAllDAOsAndReleasesOnFailure`、`TestProjectorOnFatalMayCloseProjector`、`TestEntityProjectionFatalWakesEveryPendingWaiter`、`TestEntityProjectionWaiterSeesFailedPipelinedDurability`；其余 3 项见文件
- [projection_plan_test.go](../../../framework/dataengine/engine/projection_plan_test.go)：`TestPlanProjectionSegmentsPreservesMixedOrder`、`TestBatchProjectionEligibilityMatchesMongoContract`、`TestPlanProjectionSegmentsBoundsOrdinaryBatches`、`TestPlanProjectionSegmentsHonorsMaxRecords`、`TestPlanProjectionSegmentsPreservesEveryRecordAndFence`；其余 4 项见文件
- [projector_benchmark_test.go](../../../framework/dataengine/engine/projector_benchmark_test.go)
- [projector_remote_test.go](../../../framework/dataengine/engine/projector_remote_test.go)：`TestRemoteProjectionIndependentEntityCompletesWhileFirstBlocked`、`TestRemoteProjectionFailureOnlyAcknowledgesPrefixAndReplaysSuffix`、`TestRemoteProjectionWindowBoundaries`、`TestRemoteProjectionCheckpointFailureKeepsWindow`、`TestRemoteProjectionRefillsWhileFirstRecordIsBlocked`；其余 4 项见文件
- [projector_test.go](../../../framework/dataengine/engine/projector_test.go)：`TestProjectorAckNotBlockedByPublisherFailure`、`TestProjectorFatalConflictInvokesFence`、`TestProjectorCommitSystemTicketCompletesAfterProjection`、`TestProjectorUsesAtomicBatchStoreForBacklog`、`TestProjectorAcknowledgesSuccessfulPrefixBeforeLaterSegmentFailure`；其余 11 项见文件
- [receipt_retention_test.go](../../../framework/dataengine/engine/receipt_retention_test.go)：`TestAssembleValidatesReceiptRetentionBeforeInfrastructure`
- [remote_conflict_test.go](../../../framework/dataengine/engine/remote_conflict_test.go)：`TestRemoteCreationConflictFencesProjectorAndKeepsWAL`
- [replay_read_budget_test.go](../../../framework/dataengine/engine/replay_read_budget_test.go)：`TestReplayReadBudgetRetainsOnlyBoundedPrefix`、`TestReplayOversizedRecordGetsExclusiveReadWindow`、`TestReplayBudgetCountsConsumedRecordsAcrossSegments`
- [shutdown_deadline_test.go](../../../framework/dataengine/engine/shutdown_deadline_test.go)：`TestProjectorWaitsRespectDeadline`、`TestProjectorShutdownDeadlineBehindBackgroundProjection`、`TestRuntimeConcurrentShutdownHonorsDeadline`
- [single_path_test.go](../../../framework/dataengine/engine/single_path_test.go)：`TestProjectorSinglePathHoldsRetriesAndSerializesReplay`
- [stage_effect_race_test.go](../../../framework/dataengine/engine/stage_effect_race_test.go)：`TestStageEffectDuplicateAfterSnapshotMissIsRetryable`

### mongo/driver

- [close_contract_promises_test.go](../../../infra/storage/mongo/driver/close_contract_promises_test.go)：`TestClientConcurrentCloseWaitsForTheFirstDisconnect`、`TestClientCloseRetriesAfterAFailedDisconnect`
- [collection_test.go](../../../infra/storage/mongo/driver/collection_test.go)：`TestIsIndexDefinitionConflict`、`TestIsIndexNotFound`、`TestIndexConflictPolicyRequiresExplicitAutoRecreate`、`TestMongoIndexModelDistinguishesRelativeAndAbsoluteExpiry`、`TestStringifyIDDoesNotExposeNilSentinel`；其余 1 项见文件
- [election_retry_promises_test.go](../../../infra/storage/mongo/driver/election_retry_promises_test.go)：`TestIndexCreationRetriesThroughAnElectionWithinBounds`
- [error_chain_test.go](../../../infra/storage/mongo/driver/error_chain_test.go)：`TestWrapErrorKeepsTheDriverChainBehindTheSentinel`
- [guards_promises_test.go](../../../infra/storage/mongo/driver/guards_promises_test.go)：`TestClientAndCollectionGuardsWithoutADeployment`
- [real_mongo_promises_test.go](../../../infra/storage/mongo/driver/real_mongo_promises_test.go)：`TestRealMongoValidateDeploymentRefusesAStandalone`、`TestRealMongoIndexConflictIsSurfacedUnlessRecreationIsAllowed`
- [real_mongo_transaction_test.go](../../../infra/storage/mongo/driver/real_mongo_transaction_test.go)：`TestRealMongoWriteErrorAbortsTransactionAndKeepsDriverChain`
- [transaction_deadline_integration_test.go](../../../infra/storage/mongo/driver/transaction_deadline_integration_test.go)：`TestRealMongoCommitIsBoundedByTransactionTimeout`、`TestRealMongoEndSessionAfterCommitTimeoutIsBounded`
- [transaction_retry_chain_test.go](../../../infra/storage/mongo/driver/transaction_retry_chain_test.go)：`TestWithTransactionKeepsTheLastCallbackErrorWhenTheWindowClosesInBackoff`、`TestWithTransactionFailuresBeforeTheCommitAreNotResultUnknown`

### mongo/mongotest

- [boundary_promises_test.go](../../../infra/storage/mongo/mongotest/boundary_promises_test.go)：`TestMongoBoundaryPromises`
- [contract_promises_test.go](../../../infra/storage/mongo/mongotest/contract_promises_test.go)：`TestFakeCollectionRefusalsMatchTheDriverContract`
- [guards_promises_test.go](../../../infra/storage/mongo/mongotest/guards_promises_test.go)：`TestFindAndModifyMissesAreNotFoundEvenWhenAskingForTheAfterImage`、`TestWritesRefuseIDCollisionsAndIDChanges`、`TestFiltersFailLoudlyOnShapesTheFakeDoesNotModel`
- [identity_boundary_test.go](../../../infra/storage/mongo/mongotest/identity_boundary_test.go)：`TestMongoIdentityBoundaries`
- [identity_promises_test.go](../../../infra/storage/mongo/mongotest/identity_promises_test.go)：`TestMongoIdentityPromises`
- [in_named_slice_promises_test.go](../../../infra/storage/mongo/mongotest/in_named_slice_promises_test.go)：`TestInAcceptsNamedSlices`
- [index_bulk_promises_test.go](../../../infra/storage/mongo/mongotest/index_bulk_promises_test.go)：`TestIndexAndBulkBoundaryPromises`
- [mongotest_test.go](../../../infra/storage/mongo/mongotest/mongotest_test.go)：`TestInsertRejectsDuplicateIDAndUniqueIndex`、`TestVersionCASSemantics`、`TestUpsertSeedsFromEqualityFieldsOnly`、`TestLeaseFilterEvaluatesOwnerTokenStatusAndExpiry`、`TestClaimStealUsesTokenCAS`；其余 9 项见文件
- [pagination_boundary_test.go](../../../infra/storage/mongo/mongotest/pagination_boundary_test.go)：`TestPaginationBoundaryPromises`
- [pagination_promises_test.go](../../../infra/storage/mongo/mongotest/pagination_promises_test.go)：`TestPaginationPromises`
- [transaction_abort_test.go](../../../infra/storage/mongo/mongotest/transaction_abort_test.go)：`TestTransactionWriteErrorAbortsLikeTheServer`、`TestTransactionCommitAfterSwallowedWriteErrorIsRetried`、`TestTransactionNotFoundDoesNotAbortAndPlainErrorsAreNotRetried`
- [transaction_promises_test.go](../../../infra/storage/mongo/mongotest/transaction_promises_test.go)：`TestTransactionSnapshotPromises`
- [unique_array_promises_test.go](../../../infra/storage/mongo/mongotest/unique_array_promises_test.go)：`TestUniqueIndexOverAnArrayIsRefusedNotGuessed`
- [unique_null_promises_test.go](../../../infra/storage/mongo/mongotest/unique_null_promises_test.go)：`TestUniqueIndexMissingFieldPromises`

### nestwal

- [close_replay_test.go](../../../framework/nestwal/close_replay_test.go)：`TestAckSyncsAsyncRecordBeforeCheckpoint`、`TestCloseRetainsDirectoryUntilExternalReplayReturns`
- [codec_promises_test.go](../../../framework/nestwal/codec_promises_test.go)：`TestEncodeRejectsUnboundedUnsetPathsAndHeaders`、`TestDecodeRejectsEveryTruncationPoint`
- [corruption_promises_test.go](../../../framework/nestwal/corruption_promises_test.go)：`TestOpenRefusesSegmentGapsAndUnacknowledgedTruncation`、`TestOpenRefusesEachCorruptFrameHeaderField`
- [crash_test.go](../../../framework/nestwal/crash_test.go)：`TestNestWALCrashChildProcess`、`TestNestWALCrashKeepsDurablePrefix`
- [effect_inbox_test.go](../../../framework/nestwal/effect_inbox_test.go)：`TestMongoEffectInboxDeduplicatesAndRejectsIdentityConflict`、`TestMongoEffectInboxHandlerFailureLeavesNoReceipt`、`TestMongoEffectInboxSurvivesTransactionRetry`
- [export_test.go](../../../framework/nestwal/export_test.go)
- [fsync_failure_promises_test.go](../../../framework/nestwal/fsync_failure_promises_test.go)：`TestAckRefusesTerminalWALAndKeepsCheckpoint`、`TestInflightAckFailsAfterFailedFinalSync`、`TestInflightAckAfterCleanCloseStillSucceeds`、`TestAckFirstFsyncFailureIsIndeterminateForTickets`、`TestSyncAfterTerminalNeverFsyncsOrAdvancesDurableLSN`；其余 1 项见文件
- [inspect_test.go](../../../framework/nestwal/inspect_test.go)：`TestInspectAcceptsDurableCheckpointBoundary`、`TestInspectRefusesRunningWriterWithoutCreatingSnapshot`、`TestInspectPreservesCorruptRecordAndSuffix`、`TestInspectValidWALCancellationAndUnsafeDestinations`、`TestInspectNeverRepairsTornTailOrCheckpoint`
- [pipelined_strict_fallback_bench_test.go](../../../framework/nestwal/pipelined_strict_fallback_bench_test.go)
- [pipelined_strict_fallback_promises_test.go](../../../framework/nestwal/pipelined_strict_fallback_promises_test.go)：`TestAppendWaitsForFsyncOnStrictPathForStrictAndPipelined`、`TestBroadcastPipelinedIsInvisibleUntilFsync`、`TestPipelinedFastPathStillReleasesLockBeforeFsync`
- [pipelined_test.go](../../../framework/nestwal/pipelined_test.go)：`TestWALEnqueueTicketResolvesDurableAndSurvivesReopen`、`TestWALEnqueueRejectsSynchronously`、`TestWALTerminalFailsPendingAndLateTickets`
- [promises_test.go](../../../framework/nestwal/promises_test.go)：`TestOpenRefusesEachInvalidOption`、`TestAppendRefusesRecordsTheLogCannotHold`、`TestAckRefusesFencesThatWouldSkipRecords`、`TestHealthyReportsDiskAndAgeLimits`、`TestReadCheckpointRefusesTruncatedForeignAndCorruptFiles`
- [record_codec_test.go](../../../framework/nestwal/record_codec_test.go)：`TestCodecPatchRoundTrip`、`TestCodecDeleteCarriesNoPayload`、`TestCodecRejectsUnknownRecordVersion`、`TestWALDefaultWriterReplaysPatch`、`TestCodecUnknownVersionDoesNotAdvanceCheckpoint`
- [remote_codec_test.go](../../../framework/nestwal/remote_codec_test.go)：`TestRemoteCommitCodecRoundTrip`、`TestRemoteDeleteCommitCodecRoundTrip`
- [single_format_test.go](../../../framework/nestwal/single_format_test.go)：`TestRetiredFormatsLeaveWALUntouched`
- [sync_barrier_promises_test.go](../../../framework/nestwal/sync_barrier_promises_test.go)：`TestWALSyncPromiseCoversAdmittedTickets`、`TestWALSyncPromiseDoesNotBlockAfterClose`
- [sync_closing_promises_test.go](../../../framework/nestwal/sync_closing_promises_test.go)：`TestWALSyncPromiseWaitsForDrainWhileClosing`、`TestWALSyncPromiseAfterCompletedClose`
- [terminal_projection_wait_test.go](../../../framework/nestwal/terminal_projection_wait_test.go)：`TestWALTerminalWakesEntityProjectionWaiters`
- [torn_page_tail_promises_test.go](../../../framework/nestwal/torn_page_tail_promises_test.go)：`TestOpenRefusesFrameWhosePayloadPageWasNotWrittenBack`
- [wal_benchmark_test.go](../../../framework/nestwal/wal_benchmark_test.go)
- [wal_test.go](../../../framework/nestwal/wal_test.go)：`TestCodecRoundTrip`、`TestWALReplayAckAndReopen`、`TestWALReplayStartsAtAckFenceAcrossRotation`、`TestWALConcurrentAppendAndRotation`、`TestWALRecoversTornTail`；其余 5 项见文件
- [zero_tail_promises_test.go](../../../framework/nestwal/zero_tail_promises_test.go)：`TestOpenTruncatesZeroFilledTailOfLastSegment`、`TestOpenTruncatesZeroFilledTailStartingAtCheckpointFence`、`TestOpenRefusesTailThatIsNotAllZero`、`TestOpenRefusesZeroFillInsideAcknowledgedPrefix`、`TestOpenRefusesTornTailBeforeCheckpointWithoutTruncating`；其余 1 项见文件

### redis

- [cas_delete_promises_test.go](../../../infra/storage/redis/cas_delete_promises_test.go)：`TestCompareAndDeleteOnlyRemovesTheValueItWasShownIntegration`、`TestCompareAndDeleteRefusesACommandItCannotHonour`
- [cas_index_promises_test.go](../../../infra/storage/redis/cas_index_promises_test.go)：`TestCompareAndSetRefusesAnIncompleteIndex`、`TestIndexIsMaintainedInTheSameWriteIntegration`、`TestIndexScorePrecisionSurvivesLuaIntegration`
- [cas_promises_test.go](../../../infra/storage/redis/cas_promises_test.go)：`TestCompareAndSetRejectsIncompleteCommandsBeforeIO`、`TestCompareAndSetRejectsMalformedScriptReply`
- [cas_test.go](../../../infra/storage/redis/cas_test.go)：`TestCompareAndSetAppliesWhenExpectedValueMatches`、`TestCompareAndSetReturnsCurrentWhenExpectedValueDiffers`、`TestCompareAndSetCanRequireMissingKey`

### redis/driver

- [assembly_test.go](../../../infra/storage/redis/driver/assembly_test.go)：`TestAssembleBuildsClientAndLocksOnOnePool`
- [client_deadline_test.go](../../../infra/storage/redis/driver/client_deadline_test.go)：`TestRedisClientsHonourContextDeadlinesOnTheWire`
- [client_export_test.go](../../../infra/storage/redis/driver/client_export_test.go)：`TestNewClientRequiresAnAddress`
- [client_promises_test.go](../../../infra/storage/redis/driver/client_promises_test.go)：`TestMissingKeysSurfaceAsContractErrNil`、`TestEvalDurableRejectsMalformedWAITAOFReply`、`TestRedisIntegerRejectsUnsignedOverflow`、`TestPipelineExecToleratesNilButPropagatesRealErrors`
- [client_test.go](../../../infra/storage/redis/driver/client_test.go)：`TestRedisIntegerParsesWAITAOFReplyValues`、`TestRedisIntegerRejectsInvalidWAITAOFReplyValue`、`TestEvalDurableRejectsClusterTopologyBeforeIO`
- [close_contract_promises_test.go](../../../infra/storage/redis/driver/close_contract_promises_test.go)：`TestClientRepeatedCloseReturnsNilOnEveryDeployment`、`TestClientCloseErrorIsReportedOnce`、`TestClientConcurrentCloseAllReturnNil`、`TestAssemblyRepeatedCloseReturnsNil`、`TestDistLockAfterClientCloseKeepsReportingErrClosed`
- [cluster_recovery_test.go](../../../infra/storage/redis/driver/cluster_recovery_test.go)：`TestClusterRecoveryPreservesErrorsWithoutReplayingCommands`
- [lock_integration_test.go](../../../infra/storage/redis/driver/lock_integration_test.go)：`TestDistLockExpiryAndReacquireIntegration`
- [lock_promises_test.go](../../../infra/storage/redis/driver/lock_promises_test.go)：`TestDistLockReleaseAndExtendRequireAHeldLease`、`TestDistLockRejectsMissingClientAndInvalidTTL`、`TestAutoExtendLockNilReceiverIsAnError`、`TestAutoExtendLockDoesNotStartWatchdogWhenAcquireFails`
- [lock_state_test.go](../../../infra/storage/redis/driver/lock_state_test.go)：`TestDistLockRefusesATTLBelowOneMillisecond`、`TestDistLockOwnerTokenIsPerAcquisition`、`TestDistLockUncertainStateBlocksReuseUntilReleaseReconciles`、`TestDistLockReleaseErrorLeavesTheLockUncertain`
- [lock_test.go](../../../infra/storage/redis/driver/lock_test.go)：`TestAutoExtendLockKeepsLeaseAliveUntilRelease`、`TestAutoExtendLockSurvivesTransientExtendErrors`、`TestAutoExtendLockSurfacesLostLease`、`TestAutoExtendLockRejectsInvalidConfiguration`、`TestAutoExtendLockRejectsConcurrentAcquire`
- [lock_toxic_integration_test.go](../../../infra/storage/redis/driver/lock_toxic_integration_test.go)：`TestToxicRedisDroppedReleaseReplyLeavesTheLockUncertainUntilReconciled`、`TestToxicRedisDroppedAcquireReplyIsReconciledNotRetried`、`TestToxicRedisLatencyKeepsAcquireWithinItsDeadline`
- [mget_integration_test.go](../../../infra/storage/redis/driver/mget_integration_test.go)：`TestMGetReturnsOneElementPerKeyIncludingMisses`、`TestMGetWithNoKeysMakesNoRoundTrip`、`TestMGetPreservesBinaryValues`
- [pipeline_errors_integration_test.go](../../../infra/storage/redis/driver/pipeline_errors_integration_test.go)：`TestIntegrationPipelineDoesNotHideWriteErrors`、`TestIntegrationPipelineFuturesAndLifecycle`、`TestIntegrationPipelinePreservesPostWriteUnknownError`
- [pipeline_errors_test.go](../../../infra/storage/redis/driver/pipeline_errors_test.go)：`TestPipelineExecChecksEveryCommandError`、`TestPipelineExecPopulatesAllFuturesOnFailure`
- [replicated_promises_test.go](../../../infra/storage/redis/driver/replicated_promises_test.go)：`TestEvalReplicatedWaitsOnTheScriptsConnection`、`TestEvalReplicatedScriptErrorsAndDisabledWait`
- [script_no_retry_promises_test.go](../../../infra/storage/redis/driver/script_no_retry_promises_test.go)：`TestAScriptWhoseReplyIsLostIsNotReplayedByTheDriver`、`TestNoReplayMarkSurvivesCloneAndUnmarkedCommandsKeepTheDriverRetry`
- [write_lost_reply_integration_test.go](../../../infra/storage/redis/driver/write_lost_reply_integration_test.go)：`TestRealRedisAWriteWhoseReplyIsLostRunsOnce`
- [write_no_replay_promises_test.go](../../../infra/storage/redis/driver/write_no_replay_promises_test.go)：`TestAWriteWhoseReplyIsLostIsNotReplayedByTheDriver`、`TestNotExecutedErrorsAreStillResent`、`TestResendsStopAtTheConfiguredBudget`、`TestAPipelineWithAnExecutedCommandIsNotResent`、`TestReadsKeepTheDriverRetry`；其余 2 项见文件

### versionstore

- [bugfix_index_retirement_test.go](../../../infra/storage/versionstore/bugfix_index_retirement_test.go)：`TestBugfix6ConditionalIndexRetirementIntegration`
- [cas_metrics_promises_test.go](../../../infra/storage/versionstore/cas_metrics_promises_test.go)：`TestUpdateCountsEveryCompareAndSetAndTheExhaustedConflict`
- [conditional_delete_test.go](../../../infra/storage/versionstore/conditional_delete_test.go)：`TestConditionalDeleteProtectsRecreatedIdentity`
- [fake_redis_test.go](../../../infra/storage/versionstore/fake_redis_test.go)
- [guards_promises_test.go](../../../infra/storage/versionstore/guards_promises_test.go)：`TestStoresRefuseNilMutatorsMissingClientsAndStaleDeletes`
- [index_promises_test.go](../../../infra/storage/versionstore/index_promises_test.go)：`TestAnIndexedCreateEntersTheIndex`、`TestAnIndexedUpdateRetiresTheEntry`、`TestTheIndexIsReadByScoreAndBounded`、`TestAStoreWithoutAnIndexSaysSo`、`TestAPerOwnerIndexKeepsOwnersApart`；其余 1 项见文件
- [index_score_promises_test.go](../../../infra/storage/versionstore/index_score_promises_test.go)：`TestAnIndexedWriteWithANaNScoreChangesNothing`
- [lost_reply_integration_test.go](../../../infra/storage/versionstore/lost_reply_integration_test.go)：`TestRealRedisUpdateWhoseReplyIsLostNeverWritesTwice`
- [malformed_promises_test.go](../../../infra/storage/versionstore/malformed_promises_test.go)：`TestAnUnreadableRecordIsReportedAsMalformed`、`TestATransportFailureIsNotMalformed`、`TestIndexDeferMovesAnExistingEntryAndCreatesNothingIntegration`
- [remaining_promises_test.go](../../../infra/storage/versionstore/remaining_promises_test.go)：`TestResendsAndCASRacesShareOneSendBudget`、`TestResumedUpdateCountsItsResolvedCAS`
- [retry_freshness_promises_test.go](../../../infra/storage/versionstore/retry_freshness_promises_test.go)：`TestARetryAfterBackoffSeesWritesThatLandedDuringTheBackoff`、`TestARetryAfterBackoffSeesADeletionAsAbsence`
- [versionstore_test.go](../../../infra/storage/versionstore/versionstore_test.go)：`TestStoredVersionIsNeverZeroAndIncrementsPerWrite`、`TestUpdateDecliningToSaveChangesNothing`、`TestUpdatePropagatesMutateErrorWithoutWriting`、`TestCreateRefusesAnExistingKey`、`TestDeleteRequiresTheCallersVersion`；其余 7 项见文件
- [write_token_integration_test.go](../../../infra/storage/versionstore/write_token_integration_test.go)：`TestRealRedisAWriteWhoseReplyIsLostIsRecognisedByItsToken`、`TestRealRedisClusterAWriteWhoseReplyIsLostIsRecognisedByItsToken`、`TestRealRedisAnIndexedWriteWithANaNScoreChangesNothing`
- [write_token_promises_test.go](../../../infra/storage/versionstore/write_token_promises_test.go)：`TestAnUpdateWhoseReplyIsLostIsNotAppliedTwiceWhenTheCallerRetries`、`TestACreateWhoseReplyIsLostIsNotReportedAsTakenWhenTheCallerRetries`、`TestAnUpdateWhoseReplyIsLostStaysAppliedOnceAfterSomeoneElseWritesOnTop`、`TestAnUpdateThatNeverRanIsResentOnce`、`TestResumeReturnsTheEarlierWriteInsteadOfWritingAgain`；其余 5 项见文件

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../maintenance/PERFORMANCE.md)，没有新测量则不能改容量承诺。
