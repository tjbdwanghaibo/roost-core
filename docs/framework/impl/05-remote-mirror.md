# Remote Entity 与 Mirror：实现与维护

适用版本：当前 main 与最新发布版本（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）；源码目录最初按 v1.24.0 提取、之后随改动维护，链接由根包文档门禁保证存在，缺漏以源码为准。[设计与使用](../guide/05-remote-mirror.md)

## 如何阅读

一个公会可能被多个游戏进程使用。Remote让共享实体的写入有明确负责人，Mirror让其他进程读取它的快照。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`framework/remoteentity`、`infra/network/ownerroute`、`infra/network/bus`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 同实体 outbox 保序，跨实体独立事务有界并行。写者自己的提交立即发布，别的进程的提交过宽限期（或同实体已有要发布的后继）才代发；改扫描或标记逻辑时不能让非写者在宽限期内代发，否则写者的 tracker 没人通知（[REMOTE_ENTITY](../REMOTE_ENTITY.md#outbox-发布归属)）。
2. L2 是快照水位权威，Mongo 是数据权威。
3. interest 非零代际、完整身份与撤销水位一起校验。
4. 超时或断线不等于未提交，写额度必须按真实完成释放。
5. 实体归属守卫（`Config.ForeignKinds`）在写额度、wrapper 与任何所有权读写之前判，覆盖写准入与 `beginOwnershipTransition` 的全部所有权操作；读所有权与快照不经它。新增会认领或改所有权的入口必须同样先过 `rejectForeignKind`（[REMOTE_ENTITY](../REMOTE_ENTITY.md#实体归属守卫)）。
6. 路由登记（[route.go](../../../framework/remoteentity/route.go)、[route_store.go](../../../framework/remoteentity/route_store.go)）只是提示：写准入不读它；登记、删除与续期“不存在”分支的重登都在实体写门下做，与所有权转换串行；登记 / 续期 / 删除三个 Lua 脚本按值比较（登记比代际，续期不重建键，删除只删自己的值）；Redis 失败只记指标、不影响写准入。前置检查（`ServeRoutedRequest`）没有所有权记录时答不存在，不认领；预加载在写门下向权威复核 owner 后只加载、不写。新增登记点必须持写门，新增删除点用 `dropRoute`（[REMOTE_ENTITY](../REMOTE_ENTITY.md#路由登记)）。
7. 独占写准入（[REMOTE_ENTITY](../REMOTE_ENTITY.md#独占写准入)）：独占实体不取写许可，只在加载、取得归属、退出共享之后读一次权威对齐；稳定持有（`heldExclusively`：缓存的租约是本 sid 独占、当前托管实例就是在这份租约下对齐过的那份）不做 I/O。新增会改所有权或换实例的入口必须推进 `MarkerEpoch` 或换实例指针，否则对齐记录不会失效；新增“加载”入口要么不记对齐（交给写准入），要么按 `routedOwnership` 的做法在版本一致时 `markAligned`。投影元数据 CAS 的独占条件只有 `_ver` 与代际，改 `authorityCommitFilter` 时不能把共享条件（`_grant_fence` / `_grant_token`）加回独占，也不能从共享去掉。确认认写者要求 wrapper 的 `writingTx`（定稿时记、`releaseRemoteEntries` 清），新增释放写门的路径必须一并清除。

## 2A. 有界发布调度实现走读

[outbox_publish.go](../../../framework/remoteentity/outbox_publish.go)的publishOutboxPage由持outboxSerial的唯一协调者调用。每页最多256项，worker缺省32、上限64；为事务全部Entity和重复TransactionID记录前序依赖。只有waiting归零的项才交给worker，等待热点链不占worker。

每次尝试结束释放后继，失败项保留持久Applied供恢复；取消后不再补新任务。因此这里保证依赖顺序的尝试，不承诺失败前序一定成功才尝试后继；消费仍依赖版本/水位拒绝陈旧状态。

慢项转后台（2026-11-06）：并发额度是 `remoteState.outboxSlots`（容量即 worker 数，首次发布时建立），全部在途发布共用，包括转到后台的。每笔发布由 `startOutboxPublication` 起一个 goroutine：先登记 `outboxInflight`（事务）与 `outboxBusy`（实体到在途次数）、为 goroutine 另登记一份 `retryWG`，结束时失败就把 tracker 置为 Indeterminate、撤销登记、交还额度，再把结果交给页（`finish`）。建页时在途事务、涉及在途实体的事务多计一个永远不会到来的前序，本页不派发，同实体后继经依赖一起留下。后台模式（只有 `runOutboxRepublish` 经 `recoverOutbox(ctx, true)`）下，页里没有可派发的事务、在途的 `outboxStragglerWait`（100 ms，`remoteState.stragglerWait` 是测试缝）内没有任何一项结束时，置 `detached` 后返回：之后结束的发布自己收尾——失败记入 `outboxDetachedErr`，并调 `scheduleOutboxRepublish` 让留下的后继与重试有人扫；发布循环下一轮先取走 `outboxDetachedErr`，有就按失败退避。后台模式每笔发布的 ctx 取 `finalizeCtx` 加 `OpTimeout`，不随页返回取消；显式 `RecoverOutbox` 仍等本页派发的全部结束，用调用方 ctx。计数 `remote_entity.outbox.detached_total`。

发布前先经同文件的 `selectOutboxPage` 决定发布归属（[REMOTE_ENTITY“outbox 发布归属”](../REMOTE_ENTITY.md#outbox-发布归属)）：`RemoteCommitStatus.WriterSID` 等于本进程 sid、为 0 或没有 `CreatedAt` 的立即发；别的进程写的按 `CreatedAt + outboxTakeoverGrace()`（`Config.OutboxTakeoverGrace`，零值与下限取 `FinalizeProjectionTimeout`）判断到期；再从后往前把“同一实体上排在要发布的提交前面”的留存提交改为立即发（可连锁）。留下的提交里最早的到期时刻由 `RecoverOutbox` 收集，返回时（含出错返回）交 `armOutboxTakeover` 安排一次 `scheduleOutboxRepublish`（`remoteState.takeoverTimer`，只保留最早的一个，`StopFinalizer` 停掉）。不分页的自定义存储在一页有留存时结束本轮，不反复读同一页。`remoteState.outboxNow` 是只给测试用的时钟观察缝。写者 sid 由 [mongo_committer.go](../../../framework/remoteentity/mongo_committer.go) 的 `ApplyRemoteCommitsInTransaction` 写进事务记录（`writer_sid`），`remoteStatusFromMongoTransaction` 读回。

[transaction_manager.go](../../../framework/remoteentity/transaction_manager.go)的RecoverOutbox（recoverOutbox）/runOutboxRepublish负责恢复扫描，publishAppliedRemoteTransaction负责发布单事务。finalizer的等待、投影确认和写许可释放需另外追踪，不能把发布调度的完成误用为提交确认。

## 2B. Rejected 收尾走读

Rejected 的写入点（[batch.go](../../../framework/remoteentity/batch.go)、[transaction_manager.go](../../../framework/remoteentity/transaction_manager.go)）：Durability 0 的 `ApplyRemoteCommits` 把 fenced / 版本冲突 / 拒绝记为 Rejected；`remoteWriteBatch.Commit` 只在 Durability 0 且错误不带未知哨兵时同步回滚（`rejected`），其余失败一律标 `indeterminate` 交 finalizer；`Abort`（WAL 准入前）只在 tracker 记 Rejected；`rejectUnresolvedMemoryTransaction` 只对 Durability 0 调 `RejectUnresolvedRemoteCommits` 写持久拒绝。`processDeferredRemoteClose` 的 Rejected 分支（`settleRejectedRemoteEntries` → `finishRejectedRemoteClose` → `deliverRemoteOutcome(false)`）与 `Close` 的明确拒绝分支（`unloadRejectedEntities`）由这两条 Durability 0 来源共用。`waitedCommitError` 让等待投影器结论的 strict / pipelined 失败一律带 `ErrRemotePersistenceIndeterminate`。WAL 级没有拒绝写入点：MongoStore 投影把权威 CAS 冲突包成 `ErrProjectionConflict` 并 fatal，投影器在同一 Mongo 事务成功后再调 `ApplyRemoteCommits` 只做幂等读回。

## 2C. 多写者本地副本走读

[batch.go](../../../framework/remoteentity/batch.go) 的 `beginWrite` 先查稳定持有（[wrapper.go](../../../framework/remoteentity/wrapper.go) 的 `heldExclusively`，命中就经 `admitExclusive` 直接准入，不做 I/O）；否则 `confirmWriteOwnership` 刷新所有权（持久权威下缓存过期时用 `ReadAuthorityState`，同一次读取顺带给出版本）、必要时认领（`claimOwnership`：持久权威下是 `ClaimAuthorityState`，直接插入第一代记录、撞上已有记录才读回，带回认领之后的版本与所有权），独占走 `alignExclusive`（复用刚读到的或认领带回的状态，都没有才再读一次）→ `admitExclusive(align=true)`，共享走 `lockSharedWrite`（分布式锁 + `GrantWrite`）。两条对齐路径都调 `loadWriteTarget`：`staleWriteTarget` 判定本地实例版本落后于许可版本、或状态为 `Fenced`（持久拒绝等待卸载的实例除外，交给 2B 的路径）；`unloadStaleWriteTarget` 经 `runLocalStep`（ctx 的本地执行入口）调 `Backend.UnloadRemoteEntity`，成功后 `detachEntity`，再 `loadEntity` 从 loader 加载。持有的资源顺序是写门 → 所有权读锁 → 分布式锁 → 实体锁（卸载内），与带 Remote 目标的消息（I/O 阶段先写门、后 Guard）和所有权转换（先写门后实体锁）一致。ctx 带 `entity.WithRemoteWriteTargetHeld`（[entity_delete.go](../../../framework/dataengine/engine/entity_delete.go) 的独立删档）时不卸载。

独占准入成功后 `admitExclusive` 记下对齐（`markAligned`）；[route.go](../../../framework/remoteentity/route.go) 的 `routedOwnership`（路由前置检查、起服预加载、续期重登）用 `ReadAuthorityState` 读所有权，实例版本与权威一致、状态可写时同样记下对齐。`detachEntity` 清掉指向被卸载实例的对齐记录。

[transaction_manager.go](../../../framework/remoteentity/transaction_manager.go) 的 `acknowledgeRemoteCommit` 在 `remoteReceiptObsolete` 之后用 `remoteCommitHeldBy` 核对实例是这笔提交的写者（LockFence 相同、StateVersion 是 BaseVersion 或 NextVersion，且 wrapper 的 `writingTx` 就是这笔事务；写门释放后只放行向量已等于这笔结果的重复确认），不是就跳过确认。投影元数据 CAS 在 [mongo_committer.go](../../../framework/remoteentity/mongo_committer.go) 的 `authorityCommitFilter`（独占 / 共享两种条件）与 `applyCommitMetadata`；不命中时 `reportCommitConflict` 读回权威记 ERROR 与 `remote_entity.commit_conflict_total`。回归见 [write_admission_test.go](../../../framework/remoteentity/write_admission_test.go) 的 `TestSharedModeWriterReloadsALocalCopyLeftBehindByAnotherProcess`、`TestPublishingAnotherProcessesCommitDoesNotAdvanceTheLocalCopy`、`TestOwnerRoutedWriterReloadsALocalCopyLeftBehindWhileItWasNotTheOnlyWriter`、`TestOwnershipTransferredBackWithoutWritesLeavesTheOwnerWritable`、`TestStaleWriteTargetHeldByTheCallerIsRefusedNotUnloaded`、`TestSharedModeWritersAlternateThroughNest`（两个进程共用 Mongo 权威替身与 Redis 版本锁替身，正式 `ManagerAccess` loader），以及独占写准入一组（`TestExclusive*`、`TestRoutedColdLoadAlignsSoTheFirstRequestReadsNoAuthority`、`TestPublishingAnotherIncarnationsCommitDoesNotAdvanceAnIdleCopy`，见 [REMOTE_ENTITY](../REMOTE_ENTITY.md#独占写准入)）。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### bus

8 个实现文件、10 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [admin.go](../../../infra/network/bus/admin.go) | `DeadLetterCommand` |
| [bus.go](../../../infra/network/bus/bus.go) | `IBus`、`Config`、`Bus` |
| [codec.go](../../../infra/network/bus/codec.go) | `Codec` |
| [handler.go](../../../infra/network/bus/handler.go) | `MsgContext`、`RpcContext`、`HandlerFunc`、`RpcHandlerFunc` |
| [jetstream_rpc.go](../../../infra/network/bus/jetstream_rpc.go) | `JetStreamRPCConfig` |
| [msgpack_codec.go](../../../infra/network/bus/msgpack_codec.go) | `MessagePackCodec` |
| [reliable.go](../../../infra/network/bus/reliable.go) | `ReliableConfig`、`ReliableStore`、`DeadLetterBucket`、`ReliableDeadLetterStore`、`ReliableDeadLetterEntryDeleter`、`ReliableConsumer`、`RedisReliableStore`、`DeadLetterQuery`、`DeadLetterEntry` |
| [rpc_error.go](../../../infra/network/bus/rpc_error.go) | 函数/方法或内部实现；见源码 |

### ownerroute

2 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [bus.go](../../../infra/network/ownerroute/bus.go) | `BusTransport` |
| [route.go](../../../infra/network/ownerroute/route.go) | `OwnerRoute`、`Resolver`、`Transport`、`Router` |

### remoteentity

28 个实现文件、48 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assemble.go](../../../framework/remoteentity/assemble.go) | `AssemblyDeps`、`MongoBackendConfig`、`Assembly` |
| [assembly.go](../../../framework/remoteentity/assembly.go) | `Stats` |
| [backend.go](../../../framework/remoteentity/backend.go) | `StorageBackend`、`Backend` |
| [batch.go](../../../framework/remoteentity/batch.go) | 函数/方法或内部实现；见源码 |
| [commit_encoding.go](../../../framework/remoteentity/commit_encoding.go) | 函数/方法或内部实现；见源码 |
| [config.go](../../../framework/remoteentity/config.go) | `Config` |
| [doc.go](../../../framework/remoteentity/doc.go) | 函数/方法或内部实现；见源码 |
| [interest.go](../../../framework/remoteentity/interest.go) | `InterestReplicaStore` |
| [interest_publish.go](../../../framework/remoteentity/interest_publish.go) | 函数/方法或内部实现；见源码 |
| [interest_refresh.go](../../../framework/remoteentity/interest_refresh.go) | `InterestRefreshStore` |
| [local_runtime.go](../../../framework/remoteentity/local_runtime.go) | 函数/方法或内部实现；见源码 |
| [manager.go](../../../framework/remoteentity/manager.go) | `Manager` |
| [marker.go](../../../framework/remoteentity/marker.go) | 函数/方法或内部实现；见源码 |
| [mongo_authority.go](../../../framework/remoteentity/mongo_authority.go) | `WriteAuthority`、`WriteGrant`、`WriteAuthorityProvider`、`OwnedEntityLister`、`MongoOwnershipReader` |
| [mongo_committer.go](../../../framework/remoteentity/mongo_committer.go) | `MongoCommitter`、`AtomicCommitStore` |
| [mongo_payload.go](../../../framework/remoteentity/mongo_payload.go) | 函数/方法或内部实现；见源码 |
| [outbox_publish.go](../../../framework/remoteentity/outbox_publish.go) | 函数/方法或内部实现；见源码 |
| [ownership.go](../../../framework/remoteentity/ownership.go) | 函数/方法或内部实现；见源码 |
| [route.go](../../../framework/remoteentity/route.go) | `PreloadReport`（路由登记、续期、前置检查与预加载挂在 `Manager` 上） |
| [route_store.go](../../../framework/remoteentity/route_store.go) | `RouteStore`、`RouteEntry`、`RouteRenewResult`、`OwnershipReader`、`RouteLookup` |
| [snapshot_client.go](../../../framework/remoteentity/snapshot_client.go) | `SnapshotClient`、`SnapshotClientDeps`、`SnapshotClientStats` |
| [snapshot_l2.go](../../../framework/remoteentity/snapshot_l2.go) | `SnapshotL2TombstoneWaitStats` |
| [syncer.go](../../../framework/remoteentity/syncer.go) | `SnapshotReplicaStore` |
| [transaction_manager.go](../../../framework/remoteentity/transaction_manager.go) | 函数/方法或内部实现；见源码 |
| [transaction_tracking.go](../../../framework/remoteentity/transaction_tracking.go) | 函数/方法或内部实现；见源码 |
| [versioned_lock.go](../../../framework/remoteentity/versioned_lock.go) | `ProcessIncarnation` |
| [versioned_lock_lua.go](../../../framework/remoteentity/versioned_lock_lua.go) | 函数/方法或内部实现；见源码 |
| [wrapper.go](../../../framework/remoteentity/wrapper.go) | 函数/方法或内部实现；见源码 |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Outbox`、`Test.*Interest`、`Test.*Snapshot`、`Test.*Indeterminate`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### bus

- [bus_lifecycle_test.go](../../../infra/network/bus/bus_lifecycle_test.go)：`TestBusStopBeforeStartIsSafeAndFinal`、`TestBusStartFailureCleansPartialSubscriptions`、`TestBusRejectsRestartAfterStop`、`TestHandleRpcReturnsSubscribeError`、`TestHandleRejectsNilAndDuplicateRegistration`；其余 6 项见文件
- [guards_test.go](../../../infra/network/bus/guards_test.go)：`TestDecodeRPCResponseRefusesEveryMalformedEnvelope`、`TestBusRefusesHandlerRegistrationAfterStop`、`TestBusCallsWithoutTransportNameTheMissingPiece`、`TestBusDeadLetterOperationsRefuseWithoutACapableStore`、`TestDeadLetterRequeueIDIsStableAndDistinct`；其余 2 项见文件
- [jetstream_contract_test.go](../../../infra/network/bus/jetstream_contract_test.go)：`TestJetStreamRPCRequestWithoutReplySubjectIsNotExecuted`、`TestLightweightCallReportsJetStreamCapture`、`TestJetStreamRPCInProgressHeartbeatStopsAtTheRequestDeadline`、`TestJetStreamStopWaitsForInFlightHandlerAndDeliversItsResponse`、`TestJetStreamStopHandsInterruptedRequestBackToBroker`；其余 1 项见文件
- [jetstream_rpc_test.go](../../../infra/network/bus/jetstream_rpc_test.go)：`TestJetStreamRPCPublishesRequestAndDeliversResponse`、`TestJetStreamRPCRecordsTimeoutAndClearsPendingGauge`、`TestJetStreamRPCPendingGaugeIsPerMethod`、`TestJetStreamRPCHandleRpcPublishesResponseAfterHandler`、`TestBusCallStaysLightweightWhenJetStreamRPCEnabled`；其余 3 项见文件
- [msgpack_codec_test.go](../../../infra/network/bus/msgpack_codec_test.go)：`TestCodecWriterByteAndStringUseTheSameHardLimit`、`TestMessagePackCustomTypesAndNilValues`、`TestMessagePackCurrentFormatAndOwnership`、`TestMessagePackRefusesOldTruncatedAndOversizedDeclarations`、`TestMessagePackCodecConcurrentUse`
- [msgpack_real_test.go](../../../infra/network/bus/msgpack_real_test.go)：`TestRealMessagePackCoreRPCErrorAndAsyncBus`
- [reliable_redis_test.go](../../../infra/network/bus/reliable_redis_test.go)：`TestRedisReliableStoreDeadLettersKeepNewestMaxEntries`、`TestRedisReliableStorePurgeDeadLettersDeletesOnlyQueryRange`、`TestRedisReliableStorePurgeWholeBucketReturnsEntryCount`
- [reliable_test.go](../../../infra/network/bus/reliable_test.go)：`TestReliableBroadcastDedupIsPerConsumer`、`TestReliableDispatchDropGoesToDeadLetter`、`TestBusDeadLetterListAndRequeue`、`TestBusDeadLetterRequeueLimitKeepsUnselectedEntries`、`TestBusDeadLetterRequeueUsesStableIDWhenDeleteFails`；其余 3 项见文件
- [rpc_contract_test.go](../../../infra/network/bus/rpc_contract_test.go)：`TestRefusedRPCIsAnsweredAtOnceWhenTheDispatcherIsNotRunning`、`TestRefusedRPCIsAnsweredAtOnceWhenTheQueueIsFull`、`TestRPCBudgetJetStreamResponsesUseTheClientEnvelope`、`TestRPCBudgetJetStreamExpiredAndMalformedDoNotCallBusiness`、`TestRPCBudgetReliableCallerReceivesMissingHandlerRefusal`；其余 4 项见文件
- [stop_test.go](../../../infra/network/bus/stop_test.go)：`TestJetStreamRPCStopContract`、`TestBusStopRetryWaitsForTheRetainedDrain`、`TestBusStopInsideHandlerAfterBudgetedStopReturns`

### ownerroute

- [route_test.go](../../../infra/network/ownerroute/route_test.go)：`TestRouterExecutesLocalCommand`、`TestRouterSendsRemoteCommand`、`TestRouterRefusesInvalidKeysAndOwnerlessRoutes`、`TestTransportAndRouterRefuseMissingParts`

### remoteentity

- [assembly_test.go](../../../framework/remoteentity/assembly_test.go)：`TestAssembleRefusesEachMissingDependency`、`TestPrepareRemoteWriteBatchRefusesWithoutManagerBackendOrFinalizeSlot`、`TestAssemblyRefusesMissingDurableAuthority`、`TestAssembleRejectsTheSnapshotConfigNewSnapshotClientRejects`、`TestValidateRemoteManagedDaoScopes`；其余 11 项见文件
- [batch_test.go](../../../framework/remoteentity/batch_test.go)：`TestRemoteWriteBatchUsesExplicitDeleteIntentBeforeEntityRemoval`、`TestRemoteWriteBatchUsesOnlyTransactionLocalChanges`、`TestRemoteWriteBatchMemoryCommitPublishesImmutableSnapshot`、`TestRemoteWriteBatchAsyncRetainsGateUntilWALApply`、`TestTransferRemoteOwnershipFencesPreviousOwner`；其余 15 项见文件
- [cluster_fault_integration_test.go](../../../framework/remoteentity/cluster_fault_integration_test.go)：`TestRealRemoteRedisClusterFailover`、`TestRealRemoteRedisClusterUnreplicatedFence`
- [cluster_slot_migration_integration_test.go](../../../framework/remoteentity/cluster_slot_migration_integration_test.go)：`TestMirrorLocalClusterSlotMigrationSnapshotReadWriteAndTombstone`
- [deferred_outcome_test.go](../../../framework/remoteentity/deferred_outcome_test.go)：`TestStrictTimeoutOutcomeCommittedRunsPostCommitOnce`、`TestUnresolvedMemoryOutcomeRejectedDiscardsAfterUnload`、`TestDefinitelyRejectedMemoryOutcomeDiscardsAfterUnload`、`TestShutdownWithoutOutcomeKeepsPostCommitPending`、`TestOutcomeIsNotRunWhenShortPoolRefuses`；其余 2 项见文件
- [durable_rejection_test.go](../../../framework/remoteentity/durable_rejection_test.go)：`TestMemoryNeverCommittedTransientErrorGetsDurableRejection`、`TestMemoryLateCommitBeatsFinalizerRejection`、`TestWALDurabilityUnknownIsNotRejectedByFinalizer`、`TestMongoRejectUnresolvedDefersToExistingTransaction`、`TestWALRemoteAuthorityConflictNeverProducesRejected`
- [finalizer_test.go](../../../framework/remoteentity/finalizer_test.go)：`TestFinalizerRejectionRollbackRunsOnNestShortWorker`、`TestDeferredCloseIsRefusedOnceTheFinalizerHasStopped`、`TestDeferredCloseAcceptedBeforeTheStopIsStillDrained`、`TestFinalizerQueriesDurableStatusWhenTrackerWasEvicted`、`TestOutboxMidThawDoesNotAdmitASecondPublisher`；其余 3 项见文件
- [interest_handler_error_jetstream_integration_test.go](../../../framework/remoteentity/interest_handler_error_jetstream_integration_test.go)：`TestRealJetStreamInterestHandlerErrorIsAcknowledged`
- [interest_refresh_test.go](../../../framework/remoteentity/interest_refresh_test.go)：`TestOwnerRestartWithTheSameSidGetsInterestBackWithoutWaitingForRenewal`、`TestInterestRefreshRenewsOnlyLiveInterests`、`TestInterestRefreshRequestsCoalesceAndAreValidated`、`TestInterestRefreshGapWaitEndsOnStop`、`TestInterestRefreshNeedsPushAndToleratesOldConsumers`；其余 1 项见文件
- [interest_test.go](../../../framework/remoteentity/interest_test.go)：`TestRemoteInterestRegistryIsScopedAndExpires`、`TestLocalInterestCapacityPrunesExpiredAndCoalescesConcurrentRenewal`、`TestRemoteInterestRegistryHasHardCapacityLimits`、`TestRemoteSnapshotReplicaKeyIncludesFullScope`、`TestStaleInterestReleaseDoesNotCancelANewerRenewal`；其余 12 项见文件
- [lock_takeover_integration_test.go](../../../framework/remoteentity/lock_takeover_integration_test.go)：`TestMirrorLocalSameSidRestartTakesOverTheOldIncarnationsLock`
- [lock_takeover_test.go](../../../framework/remoteentity/lock_takeover_test.go)：`TestSameSidRestartTakesOverThePreviousIncarnationsSharedLock`、`TestLockTakeoverOnlyAppliesToThePreviousIncarnationOfTheSameSingletonHolder`、`TestAssembleRejectsAMalformedIncarnation`
- [manager_test.go](../../../framework/remoteentity/manager_test.go)：`TestManagerCoalescesConcurrentWrapperCreation`、`TestWrapperCapacityEvictsIdleEntry`、`TestManagerRefusesALockFactoryWithoutFences`
- [marker_key_prefix_integration_test.go](../../../framework/remoteentity/marker_key_prefix_integration_test.go)：`TestRealRedisMarkerKeyPrefixIsolatesDeployments`
- [mirror_local_bench_integration_test.go](../../../framework/remoteentity/mirror_local_bench_integration_test.go)
- [mirror_step4_jetstream_integration_test.go](../../../framework/remoteentity/mirror_step4_jetstream_integration_test.go)：`TestRealJetStreamLiveDurableNameShape`、`TestRealJetStreamLiveSubscriptionConfirmsAndResumes`、`TestRealJetStreamLiveSnapshotPushReachesTheReader`
- [mongo_authority_test.go](../../../framework/remoteentity/mongo_authority_test.go)：`TestMongoAuthorityFencesUncommittedWriterAndReplaysCommittedTransaction`、`TestMongoAuthoritySharedTransitionsAndOverflow`、`TestMongoAuthorityRejectsUnsupportedMetadata`、`TestMongoAuthorityLostGrantReplyDoesNotAllocateTwice`、`TestOwnerRoutedAdmissionRechecksDurableOwnerDespiteHotCache`；其余 5 项见文件
- [route_test.go](../../../framework/remoteentity/route_test.go)：`TestRouteIsRegisteredOnWriteAndDeletedWhenOwnershipMovesAway`、`TestRouteRenewalReRegistersMissingKeysOnlyWhileStillTheOwner`、`TestRedisOutageKeepsWriteAdmissionAndFallsBackToTheOwnershipRecord`、`TestServeRoutedRequestNeverClaimsAMissingEntity`、`TestPreloadLoadsOnlyEntitiesThisSidOwns`、`TestEmptyClaimIsAnOwnedEntityThatDoesNotExist`（空认领：预加载计 Empty、前置检查由本实例服务、同 ID 再建；删过档计 Deleted）、`TestListOwnedRemoteEntitiesListsThisSidsExclusiveEntities`；其余 2 项见文件
- [route_redis_integration_test.go](../../../framework/remoteentity/route_redis_integration_test.go)：`TestRealRouteScriptsCompareByValue`（单机与 Cluster）、`TestRealRouteRegistrationExpiresWithoutRenewal`、`TestRealListOwnedRemoteEntitiesFiltersByOwnerAndKind`、`TestRealRouteFollowsOwnershipAcrossTransferAndKill`
- [outbox_crash_integration_test.go](../../../framework/remoteentity/outbox_crash_integration_test.go)：`TestRemoteOutboxCrashHelper`、`TestRealRemoteOutboxRecoversAfterCommitBeforePublicationCrash`
- [outbox_test.go](../../../framework/remoteentity/outbox_test.go)：`TestOutboxFailedFirstPageDoesNotStarveLaterTransaction`、`TestOutboxIndependentTransactionsPublishWhileOverlappingTransactionWaits`、`TestOutboxWorkerLimitAndCancellationRetainUnpublishedTransactions`、`TestExplicitOutboxRecoveryIsDrainedBeforeStop`、`TestPublicationFailureDoesNotBlockLaterProjection`、发布归属 `TestOutboxWriterPublishesItsOwnCommitBeforeOtherCoordinators`；其余 8 项见文件
- [owner_startup_election_integration_test.go](../../../framework/remoteentity/owner_startup_election_integration_test.go)：`TestMirrorLocalOwnerStorageInitSurvivesAMongoElection`
- [ownership_redis_integration_test.go](../../../framework/remoteentity/ownership_redis_integration_test.go)：`TestRealRemoteOwnershipClaimAndTransfer`
- [ownership_test.go](../../../framework/remoteentity/ownership_test.go)：`TestConcurrentOwnershipClaimElectsExactlyOneOwner`、`TestOwnershipModeTransitionsAdvanceMarkerEpoch`、`TestEnterSharedPromiseLostReplyFollowsAuthorityIntoSharedMode`、`TestEnterSharedPromiseLostReplyFreezesUntilAuthorityAnswers`、`TestLeaveSharedPromiseLostReplyFollowsAuthorityIntoLocalOwned`；其余 12 项见文件（含实体归属守卫 `TestForeignKindIsRejectedBeforeOwnershipClaim`）
- [pipelined_contract_test.go](../../../framework/remoteentity/pipelined_contract_test.go)：`TestPipelinedRemoteBatchLocalRecordIsFsyncedBeforeVisible`、`TestPipelinedRemoteOutcomeMatchesStrict`
- [process_lock_integration_test.go](../../../framework/remoteentity/process_lock_integration_test.go)：`TestRemoteLeaseProcessHelper`、`TestRealRemoteProcessKillRecoversLease`、`TestRealRemoteProcessPartitionFencesOldOwner`、`TestRealRemoteColdAdmissionDeadlineUnderLatency`、`TestRealRemoteDurableAuthorityProcessPartition`
- [redis_acquire_unknown_integration_test.go](../../../framework/remoteentity/redis_acquire_unknown_integration_test.go)：`TestRealVersionedLockReacquiresAfterAcquireReplyLost`、`TestRealVersionedLockReacquiresAcrossUnknownChain`、`TestRealVersionedLockLateAcquireScript`
- [redis_lock_integration_test.go](../../../framework/remoteentity/redis_lock_integration_test.go)：`TestRealVersionedLockExactCounters`、`TestRealVersionedLockCounterFailureDoesNotLeaveOwner`、`TestRealVersionedLockRenewExpiryAndHandoff`
- [redis_touch_generation_integration_test.go](../../../framework/remoteentity/redis_touch_generation_integration_test.go)：`TestRealVersionedLockNextWriterLocksWhileOldRenewalReplyIsLate`、`TestRealVersionedLockRenewalGoroutineObservesExpiryThenNextWriterLocks`
- [redis_unlock_outage_integration_test.go](../../../framework/remoteentity/redis_unlock_outage_integration_test.go)：`TestRealVersionedLockReacquiresAfterUnlockOutage`、`TestRealVersionedLockUnknownReleaseDefersToOtherHolder`
- [rejected_write_test.go](../../../framework/remoteentity/rejected_write_test.go)：`TestRejectedMemoryWriteIsNotCarriedByNextWrite`、`TestRejectedMemoryWriteAfterQuarantineIsNotCarriedByNextWrite`、`TestDefinitelyRejectedMemoryWriteIsNotCarriedByNextWrite`、`TestRejectedMemoryWriteReloadsFromAuthority`、`TestDefinitelyRejectedMemoryWriteReloadsFromAuthority`；其余 6 项见文件
- [replay_test.go](../../../framework/remoteentity/replay_test.go)：`TestCommittedReplayPreservesNewerLiveFenceAndDirtyState`、`TestMemoryLostCommitReplyKeepsFinalizerOwnership`、`TestBackendForwardsConcurrentCommitCapability`、`TestAdmittedTransactionStatusRefreshesDurableRejection`、`TestCommittedTrackerSurvivesFailedReplayPublication`；其余 4 项见文件
- [reply_sentinel_table_test.go](../../../framework/remoteentity/reply_sentinel_table_test.go)：`TestRemoteUnknownOutcomeRepliesHitDecisionTableRow2`、`TestRemoteFinalizeRejectionReplyHitsDecisionTableRow12`、`TestRemoteRejectedAfterLocalCommitReplyHitsDecisionTableRow4`、`TestRemoteWALCommitFailureNeverRepliesRemotePartRejected`、`TestRemoteStrictTrackerEvictedAfterCommitReplyHitsDecisionTableRow2`；其余 1 项见文件
- [snapshot_client_test.go](../../../framework/remoteentity/snapshot_client_test.go)：`TestSnapshotClientHasNoWriteCapability`、`TestSnapshotClientReadsWhatTheOwnerPublishesInTheSameProcess`、`TestSnapshotClientLinearizableNeedsADeclaredLoader`、`TestSnapshotClientStartFailureLeavesNoSubscription`、`TestSnapshotClientStopContract`；其余 8 项见文件
- [snapshot_l2_consistency_test.go](../../../framework/remoteentity/snapshot_l2_consistency_test.go)：`TestDeleteWatermarkHoldsInL2AgainstAnInflightLoadOnAnotherNode`、`TestDeleteWatermarkHoldsInL2AgainstALateReplicaOnAnotherNode`、`TestDeleteWatermarkControls`、`TestLateReplicaOnColdNodeDoesNotPinL1BelowSharedL2`、`TestAuthoritativeLoadThatLosesTheL2RaceReturnsTheNewerSnapshot`；其余 7 项见文件
- [snapshot_l2_key_prefix_integration_test.go](../../../framework/remoteentity/snapshot_l2_key_prefix_integration_test.go)：`TestRealSnapshotL2KeyPrefixOnRedis`、`TestRealSnapshotL2KeyPrefixOnRedisCluster`
- [snapshot_l2_test.go](../../../framework/remoteentity/snapshot_l2_test.go)：`TestRemoteSnapshotL2RejectsDelayedPublisher`、`TestRemoteSnapshotL2RejectsSameVersionDifferentContent`、`TestL2RejectsSameVersionWithADifferentSchemaOrCodec`、`TestL2ComparesVersionsExactlyBeyondFloatPrecision`、`TestRemoteSnapshotL2DeleteAtVersionKeepsNewerSnapshot`；其余 7 项见文件
- [snapshot_l2_tombstone_integration_test.go](../../../framework/remoteentity/snapshot_l2_tombstone_integration_test.go)：`TestRealSnapshotL2StaleWriteIsReportedAndNotPinnedInL1`、`TestRealSnapshotL2TombstoneFencesEveryWriter`
- [snapshot_l2_tombstone_wait_integration_test.go](../../../framework/remoteentity/snapshot_l2_tombstone_wait_integration_test.go)：`TestMirrorLocalTombstoneSurvivesAFailoverOnlyWithWait`、`TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary`
- [snapshot_l2_watermark_bench_integration_test.go](../../../framework/remoteentity/snapshot_l2_watermark_bench_integration_test.go)
- [snapshot_l2_watermark_matrix_integration_test.go](../../../framework/remoteentity/snapshot_l2_watermark_matrix_integration_test.go)：`TestRealB2WatermarkMatrixStandalone`、`TestRealB2WatermarkMatrixCluster`
- [snapshot_replay_jetstream_integration_test.go](../../../framework/remoteentity/snapshot_replay_jetstream_integration_test.go)：`TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect`
- [snapshot_replica_test.go](../../../framework/remoteentity/snapshot_replica_test.go)：`TestApplyReplicaRefusesPayloadIdentityThatContradictsTheEnvelope`、`TestReadRemoteSnapshotMonotonicMissLoadsAuthorityOnce`、`TestReadRemoteSnapshotMonotonicBelowMinimumLoadsAuthorityOnce`、`TestSnapshotReplicaAuthoritativeRepairAdmission`、`TestApplyReplicaPromiseDelayedDeleteKeepsNewerSnapshot`；其余 5 项见文件
- [transaction_tracking_test.go](../../../framework/remoteentity/transaction_tracking_test.go)：`TestFlushRemoteAllKeepsTrackersAcrossConcurrentEviction`、`TestInvalidRemoteApplyDoesNotConsumeTrackerCapacity`、`TestRemoteTrackerEvictionUsesFirstCompletionAndKeepsPending`、`TestRemoteTrackerTTLPrunesOnlyExpiredCompletions`、`TestInvalidRemoteApplyPreservesExistingCompletion`；其余 1 项见文件
- [unresolved_rejection_race_integration_test.go](../../../framework/remoteentity/unresolved_rejection_race_integration_test.go)：`TestRealMongoRejectionWaitsForInFlightCommitThenReportsApplied`、`TestRealMongoRejectionWinsAfterInFlightCommitAborts`、`TestRealMongoRejectionFirstAbortsTheInFlightCommit`
- [versioned_lock_generation_test.go](../../../framework/remoteentity/versioned_lock_generation_test.go)：`TestVersionedLockNewGenerationRenewsWhileOldTouchGoroutineExits`、`TestVersionedLockTouchRestartStressNoLostRenewal`、`TestDelayedUnlockCannotClearReacquiredGeneration`
- [versioned_lock_test.go](../../../framework/remoteentity/versioned_lock_test.go)：`TestVersionedLockReacquiresAfterAcquireReplyLost`、`TestVersionedLockLostAcquireDefersToOtherHolder`、`TestVersionedLockUnsentAcquireKeepsUnknownReleaseReclaimable`、`TestVersionedLockReacquiresAcrossUnknownChain`、`TestVersionedLockLateAcquireScriptCannotTakeNewGeneration`；其余 13 项见文件
- [write_admission_test.go](../../../framework/remoteentity/write_admission_test.go)：`TestColdRemoteAdmissionHonorsCallerCancellation`、`TestRemoteBatchSharesOneBoundedAdmissionDeadline`、`TestAuthorityLockUsesDurableVersionAndCleansFailedAdmission`、`TestAuthorityLockRetriesKnownLeaseExpiryBeforeBusinessAdmission`、`TestRemoteWriteBudgetIsIndependentAndNonBlocking`；其余 18 项见文件

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。没有新测量不能改容量承诺，性能结论以 [性能报告](../../performance/README.md) 为准。
