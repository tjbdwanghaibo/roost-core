# 次核心：Kit 装配：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/13-kit.md)

## 如何阅读

Kit把数据库、调度器、同步总线等能力接进一个App。你通过配置和Mod组合这些能力。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`kit`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 资源所有权随创建关系唯一，借用者不重复关闭。
2. 依赖写 Mod 名，Registry 查询 capability 接口。
3. StopBudget 与 StopWithContext 的真实完成条件一致。

## 2A. Mod实现核对方式

沿NewMod → ConfigSchema/Init → Provide → Start → StopWithContext逐段读。配置结构只负责本Mod读取的键；Provide发布接口，不应为了取一个接口再创建第二个后台实例。DependsOn指定生产者Mod名称；可选依赖仅在实际装配时进入排序。

验证所有启动失败分支：连接已创建但下一项校验失败、Provide失败、Start失败、Service启动失败。排空超时应保留仍在用的对象，成功Stop后才释放；同一个借用的Redis/NATS客户端不由每个服务分别关闭。

kit/service与基础设施Mod共用这些生命周期规则，但其当前领域实现分布仍按09篇统计，不能把目录名当作“纯装配”证明。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### kit/configdata

1 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [configdata.go](../../../kit/configdata/configdata.go) | `Mod` |

### kit/dataengine

1 个实现文件、13 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [mod.go](../../../kit/dataengine/mod.go) | `Mod`、`ModOption`、`EffectsConfig` |

### kit/etcd

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [etcd_mod.go](../../../kit/etcd/etcd_mod.go) | `EtcdMod` |

### kit/internal/configschemagen

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [main.go](../../../kit/internal/configschemagen/main.go) | 函数/方法或内部实现；见源码 |

### kit/lock

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [lock_mod.go](../../../kit/lock/lock_mod.go) | `LockMod` |

### kit/manager

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [manager_mod.go](../../../kit/manager/manager_mod.go) | `ManagerMod` |

### kit/mods

6 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [name.go](../../../kit/mods/name.go) | 函数/方法或内部实现；见源码 |
| [persistence.go](../../../kit/mods/persistence.go) | `PersistenceConfig` |
| [registry.go](../../../kit/mods/registry.go) | `Capability` |
| [saga_payload.go](../../../kit/mods/saga_payload.go) | `SagaPayloadConfig` |
| [service_name.go](../../../kit/mods/service_name.go) | 函数/方法或内部实现；见源码 |
| [service_servicemods.go](../../../kit/mods/service_servicemods.go) | `ServiceMetricsConfig` |

### kit/mongo

1 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [mongo_mod.go](../../../kit/mongo/mongo_mod.go) | `MongoMod` |

### kit/nats

1 个实现文件、12 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [nats_mod.go](../../../kit/nats/nats_mod.go) | `NatsMod` |

### kit/nest

2 个实现文件、10 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [entity_sync.go](../../../kit/nest/entity_sync.go) | `EntitySyncSetup` |
| [nest_mod.go](../../../kit/nest/nest_mod.go) | `Mod` |

### kit/ops

1 个实现文件、11 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [ops_mod.go](../../../kit/ops/ops_mod.go) | `OpsMod` |

### kit/redis

2 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [redis_mod.go](../../../kit/redis/redis_mod.go) | `RedisMod`、`ClusterConfig`、`Config` |
| [singleton.go](../../../kit/redis/singleton.go) | 函数/方法或内部实现；见源码 |

### kit/remoteentity

3 个实现文件、11 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [config.go](../../../kit/remoteentity/config.go) | 函数/方法或内部实现；见源码 |
| [remote_entity_mod.go](../../../kit/remoteentity/remote_entity_mod.go) | `RemoteEntityMod`、`ModOption` |
| [remote_mirror_mod.go](../../../kit/remoteentity/remote_mirror_mod.go) | `RemoteMirrorMod`、`MirrorOption` |

### kit/saga

3 个实现文件、7 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [config.go](../../../kit/saga/config.go) | 函数/方法或内部实现；见源码 |
| [mod.go](../../../kit/saga/mod.go) | `Mod` |
| [step_budgets.go](../../../kit/saga/step_budgets.go) | 函数/方法或内部实现；见源码 |

### kit/service/account

12 个实现文件、20 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [account_mod.go](../../../kit/service/account/account_mod.go) | `Mod` |
| [account_rpc.go](../../../kit/service/account/account_rpc.go) | `Accounts` |
| [accounts_rpc_assembly_gen.go](../../../kit/service/account/accounts_rpc_assembly_gen.go) | `Server`、`ClientMod` |
| [accounts_rpc_gen.go](../../../kit/service/account/accounts_rpc_gen.go) | `BusClient` |
| [admin.go](../../../kit/service/account/admin.go) | `Admin` |
| [create_role.go](../../../kit/service/account/create_role.go) | 函数/方法或内部实现；见源码 |
| [creation_table.go](../../../kit/service/account/creation_table.go) | 函数/方法或内部实现；见源码 |
| [identity.go](../../../kit/service/account/identity.go) | `IdentityVerifier`、`Verified`、`VerifierFunc`、`PlayerIDAllocator`、`AllocatorFunc`、`RegistryBound`、`NameValidator`、`NameValidatorFunc` |
| [redis_store.go](../../../kit/service/account/redis_store.go) | `RedisStores` |
| [server_run.go](../../../kit/service/account/server_run.go) | 函数/方法或内部实现；见源码 |
| [service.go](../../../kit/service/account/service.go) | `Config`、`Service` |
| [types.go](../../../kit/service/account/types.go) | `Channel`、`Identity`、`Account`、`ServerStatus`、`GameServer`、`Role`、`Session`、`Slot`、`RoleCreation` |

### kit/service/chat

9 个实现文件、13 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [chat.go](../../../kit/service/chat/chat.go) | `ChannelKind`、`ChannelScope`、`ChannelRule`、`Channel`、`ChannelRef`、`Sender`、`Origin`、`MessageType`、`Message`、`PublishRequest`、`SystemPublishRequest`、`HistoryQuery`、`Page`、`Stats`、`ChannelPolicy`、`PolicyFuncs`、`BodyValidator`、`BodyValidatorFunc`、`BodySpec`、`BodyRegistry`、`SystemToken`、`SystemAuthenticator`、`SystemAuthenticatorFunc`、`Metrics` |
| [chat_mod.go](../../../kit/service/chat/chat_mod.go) | `Mod` |
| [chat_rpc.go](../../../kit/service/chat/chat_rpc.go) | `Messaging` |
| [messaging_rpc_assembly_gen.go](../../../kit/service/chat/messaging_rpc_assembly_gen.go) | `Server`、`ClientMod` |
| [messaging_rpc_gen.go](../../../kit/service/chat/messaging_rpc_gen.go) | `BusClient` |
| [redis_store.go](../../../kit/service/chat/redis_store.go) | 函数/方法或内部实现；见源码 |
| [server_run.go](../../../kit/service/chat/server_run.go) | 函数/方法或内部实现；见源码 |
| [service.go](../../../kit/service/chat/service.go) | `ServiceConfig`、`Service` |
| [store.go](../../../kit/service/chat/store.go) | `Store`、`StateStore`、`Config` |

### kit/service/directory

4 个实现文件、7 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [directory.go](../../../kit/service/directory/directory.go) | `State`、`Owner`、`Entry`、`Claim`、`Normalizer`、`Directory` |
| [directory_mod.go](../../../kit/service/directory/directory_mod.go) | `Mod` |
| [redis_store.go](../../../kit/service/directory/redis_store.go) | 函数/方法或内部实现；见源码 |
| [store.go](../../../kit/service/directory/store.go) | `Config` |

### kit/service/examples/split

4 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [consumer.go](../../../kit/service/examples/split/consumer.go) | `RewardFlow` |
| [doc.go](../../../kit/service/examples/split/doc.go) | 函数/方法或内部实现；见源码 |
| [gameprocess.go](../../../kit/service/examples/split/gameprocess.go) | `GameService` |
| [mailprocess.go](../../../kit/service/examples/split/mailprocess.go) | 函数/方法或内部实现；见源码 |

### kit/service/global/activity

11 个实现文件、28 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [activity_mod.go](../../../kit/service/global/activity/activity_mod.go) | `Mod` |
| [activity_rpc.go](../../../kit/service/global/activity/activity_rpc.go) | `Coordinator` |
| [admin.go](../../../kit/service/global/activity/admin.go) | `Admin` |
| [coordinator_rpc_assembly_gen.go](../../../kit/service/global/activity/coordinator_rpc_assembly_gen.go) | `Server`、`ClientMod` |
| [coordinator_rpc_gen.go](../../../kit/service/global/activity/coordinator_rpc_gen.go) | `BusClient` |
| [groups.go](../../../kit/service/global/activity/groups.go) | `Group`、`Groups` |
| [redis_store.go](../../../kit/service/global/activity/redis_store.go) | `RedisStores`、`RedisDispatches` |
| [server_run.go](../../../kit/service/global/activity/server_run.go) | 函数/方法或内部实现；见源码 |
| [service.go](../../../kit/service/global/activity/service.go) | `Config`、`Service`、`OwedDispatchIndex` |
| [types.go](../../../kit/service/global/activity/types.go) | `Phase`、`Key`、`Status`、`CompletionReason`、`Activity`、`Result`、`ParticipantKey`、`ProgressDelta`、`Participant`、`RequestKey`、`ReservationState`、`ProgressReservation`、`NotifyRefusal`、`NotifyAudit`、`NotifyAuditLog`、`DispatchKey`、`DispatchState`、`Dispatch`、`Window`、`OpeningEntry` |
| [window_entries.go](../../../kit/service/global/activity/window_entries.go) | `WindowList`、`MalformedWindowEntry` |

### kit/service/global

8 个实现文件、8 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [global_mod.go](../../../kit/service/global/global_mod.go) | `Mod` |
| [global_rpc.go](../../../kit/service/global/global_rpc.go) | `Routing` |
| [redis_store.go](../../../kit/service/global/redis_store.go) | `RedisStores` |
| [routing_rpc_assembly_gen.go](../../../kit/service/global/routing_rpc_assembly_gen.go) | `Server`、`ClientMod` |
| [routing_rpc_gen.go](../../../kit/service/global/routing_rpc_gen.go) | `BusClient` |
| [server_run.go](../../../kit/service/global/server_run.go) | 函数/方法或内部实现；见源码 |
| [service.go](../../../kit/service/global/service.go) | `Config`、`Service` |
| [types.go](../../../kit/service/global/types.go) | `RouteState`、`RouteBinding` |

### kit/service/integration

1 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [doc.go](../../../kit/service/integration/doc.go) | 函数/方法或内部实现；见源码 |

### kit/service/mail

4 个实现文件、7 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [alias.go](../../../kit/service/mail/alias.go) | 函数/方法或内部实现；见源码 |
| [mail_mod.go](../../../kit/service/mail/mail_mod.go) | `Mod` |
| [mail_rpc_assembly_gen.go](../../../kit/service/mail/mail_rpc_assembly_gen.go) | `Server`、`ClientMod` |
| [server_run.go](../../../kit/service/mail/server_run.go) | 函数/方法或内部实现；见源码 |

### kit/service/match

4 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [alias.go](../../../kit/service/match/alias.go) | 函数/方法或内部实现；见源码 |
| [match_mod.go](../../../kit/service/match/match_mod.go) | `Mod` |
| [matchmaker_rpc_assembly_gen.go](../../../kit/service/match/matchmaker_rpc_assembly_gen.go) | `Server`、`ClientMod` |
| [server_run.go](../../../kit/service/match/server_run.go) | 函数/方法或内部实现；见源码 |

### kit/service/platform

10 个实现文件、17 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [admin.go](../../../kit/service/platform/admin.go) | `Admin` |
| [identity.go](../../../kit/service/platform/identity.go) | `Credential`、`Verified`、`Verifier`、`VerifierFunc`、`PlayerResolver`、`PlayerResolverFunc`、`RegistryBound` |
| [platform_mod.go](../../../kit/service/platform/platform_mod.go) | `Mod` |
| [platform_rpc.go](../../../kit/service/platform/platform_rpc.go) | `Platform` |
| [platform_rpc_assembly_gen.go](../../../kit/service/platform/platform_rpc_assembly_gen.go) | `Server`、`ClientMod` |
| [platform_rpc_gen.go](../../../kit/service/platform/platform_rpc_gen.go) | `BusClient` |
| [redis_store.go](../../../kit/service/platform/redis_store.go) | `RedisOrders`、`PendingRetirer` |
| [server_run.go](../../../kit/service/platform/server_run.go) | `PendingDeferrer` |
| [service.go](../../../kit/service/platform/service.go) | `OrderStore`、`Deliverer`、`DelivererFunc`、`Config`、`PendingOrders`、`PendingOrdersFunc`、`Service`、`Session`、`Receipt` |
| [types.go](../../../kit/service/platform/types.go) | `DeliveryState`、`Order` |

### kit/service/rank

9 个实现文件、14 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [member.go](../../../kit/service/rank/member.go) | 函数/方法或内部实现；见源码 |
| [rank.go](../../../kit/service/rank/rank.go) | `Rank` |
| [rank_mod.go](../../../kit/service/rank/rank_mod.go) | `Mod` |
| [rank_rpc_assembly_gen.go](../../../kit/service/rank/rank_rpc_assembly_gen.go) | `Server`、`ClientMod` |
| [rank_rpc_gen.go](../../../kit/service/rank/rank_rpc_gen.go) | `BusClient` |
| [redis_store.go](../../../kit/service/rank/redis_store.go) | `RedisClient`、`RedisConfig`、`RedisStore` |
| [server_run.go](../../../kit/service/rank/server_run.go) | 函数/方法或内部实现；见源码 |
| [store.go](../../../kit/service/rank/store.go) | `Store` |
| [types.go](../../../kit/service/rank/types.go) | `Scope`、`Board`、`Score`、`UpdateMode`、`Entry`、`Page` |

### kit/service/servicemetrics

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [servicemetrics.go](../../../kit/service/servicemetrics/servicemetrics.go) | `Reporter`、`Sink`、`Recorder`、`KeyedReporter`、`MetricsReporter` |

### kit/service/session

5 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [alias.go](../../../kit/service/session/alias.go) | 函数/方法或内部实现；见源码 |
| [options.go](../../../kit/service/session/options.go) | `ModOption` |
| [server_run.go](../../../kit/service/session/server_run.go) | 函数/方法或内部实现；见源码 |
| [session_mod.go](../../../kit/service/session/session_mod.go) | `Mod` |
| [session_rpc_assembly_gen.go](../../../kit/service/session/session_rpc_assembly_gen.go) | `Server`、`ClientMod` |

### kit/statslog

1 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [statslog.go](../../../kit/statslog/statslog.go) | `ProviderFunc`、`RuntimeStats`、`EntityStats`、`StatsRecord`、`NestStats`、`NestQueueStats`、`StatsLogMod` |

### kit/syncbus

1 个实现文件、7 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [mod.go](../../../kit/syncbus/mod.go) | `SyncBusMod` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Mod`、`Test.*Stop`、`Test.*Config`、`Test.*Dependency`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### kit

- [assembly_boundary_test.go](../../../kit/assembly_boundary_test.go)：`TestKitModsDoNotReachForRawDriverHandles`、`TestRawHandleCallsDetector`
- [config_schema_promises_test.go](../../../kit/config_schema_promises_test.go)：`TestEveryKitModLoadsWhatItDeclares`、`TestKitModsRefuseOutOfRangeValuesAtLoadAndAtStartup`、`TestStartupCheckRefusesJetStreamRPCWithoutPositiveTimeouts`、`TestStartupCheckRefusesOpsAdminWithoutAToken`、`TestProductionRefusesDevSecrets`；其余 3 项见文件
- [dependency_boundary_test.go](../../../kit/dependency_boundary_test.go)：`TestKitDependencyBoundary`、`TestForbiddenKitImport`
- [lifecycle_gate_test.go](../../../kit/lifecycle_gate_test.go)：`TestBuiltInModsImplementContextStop`
- [mod_dependencies_test.go](../../../kit/mod_dependencies_test.go)：`TestEveryModDependencyNamesAKitMod`
- [strict_config_promises_test.go](../../../kit/strict_config_promises_test.go)：`TestKitModsRefuseConfigValuesOfTheWrongType`

### kit/configdata

- [configdata_test.go](../../../kit/configdata/configdata_test.go)：`TestConfigDataStopStillReleasesHooksAfterDeadline`、`TestConfigDataModInitHonorsConfiguredDir`、`TestConfigDataModProvidesStoreAndStopUnregistersHooks`
- [mod_guards_promises_test.go](../../../kit/configdata/mod_guards_promises_test.go)：`TestConfigDataModRefusesBareRegistriesAndStartWithoutStore`
- [reload_visibility_promises_test.go](../../../kit/configdata/reload_visibility_promises_test.go)：`TestFailedReloadAndRollbackAreCountedAndLogged`、`TestRevertedOperatorRollbackIsCounted`

### kit/dataengine

- [backlog_integration_test.go](../../../kit/dataengine/backlog_integration_test.go)：`TestRealDataEngineLargeBacklogRecovery`
- [failover_integration_test.go](../../../kit/dataengine/failover_integration_test.go)：`TestRealMongoPrimaryFailoverContinuesProjection`、`TestRealNATSOutageDoesNotBlockProjectionAndRecoversOutbox`、`TestRealJetStreamLeaderFailoverPreservesDedupAndOrder`
- [fatal_fence_test.go](../../../kit/dataengine/fatal_fence_test.go)：`TestModFatalFencesNestAndSignalsApplication`
- [mod_promises_test.go](../../../kit/dataengine/mod_promises_test.go)：`TestModProvideRefusesEachMissingCapability`、`TestModRefusesLifecycleCallsBeforeProvideAndStart`
- [mod_test.go](../../../kit/dataengine/mod_test.go)：`TestDataEngineModReadsProjectionBatchByteLimit`、`TestDataEngineModKeepsProjectionBatchByteDefaultForZero`、`TestEffectStreamDefaultsMatchTheDeclaration`、`TestDataEngineModRecoversBeforeReadyAndOwnsNestOptions`、`TestDataEngineProjectionCheckpointAndBacklogConfig`；其余 3 项见文件
- [multi_batch_integration_test.go](../../../kit/dataengine/multi_batch_integration_test.go)：`TestRealLocalMultiBatchOrderIdentityAndLostCheckpoint`、`TestRealLocalMultiBatchLateConflictRollsBackThenAcknowledgesPrefix`
- [real_fixture_integration_test.go](../../../kit/dataengine/real_fixture_integration_test.go)
- [real_integration_test.go](../../../kit/dataengine/real_integration_test.go)：`TestRealMultiDocumentReceiptAndOutboxAreAtomic`、`TestRealMultiDocumentFailureRollsBackEarlierMutation`、`TestRealPatchConflictFencesWithoutFullFallback`、`TestRealLoadRestoresTrackerVersion`、`TestRealIntegrationDeadlineIsBounded`；其余 4 项见文件
- [receipt_identity_integration_test.go](../../../kit/dataengine/receipt_identity_integration_test.go)：`TestRealProjectionIdentityVerdictsAcrossTransactions`
- [receipt_retention_promises_test.go](../../../kit/dataengine/receipt_retention_promises_test.go)：`TestTransactionReceiptRetentionExceedsWALWindow`
- [remote_integration_test.go](../../../kit/dataengine/remote_integration_test.go)：`TestRealDataEngineRemotePublicationAndWALRecovery`
- [stop_budget_test.go](../../../kit/dataengine/stop_budget_test.go)：`TestDataEngineModDeclaresShutdownTimeoutAsStopBudget`
- [toxic_integration_test.go](../../../kit/dataengine/toxic_integration_test.go)：`TestToxicNATSLatencyKeepsTheCommitOnTheDurablePath`、`TestToxicNATSConnectionResetDeliversTheEffectExactlyOnce`、`TestToxicNATSHalfOpenAckLossIsBoundedAndDeliversExactlyOnce`

### kit/lock

- [lock_mod_test.go](../../../kit/lock/lock_mod_test.go)：`TestLockModProvidesReentrantLockManager`

### kit/manager

- [manager_mod_test.go](../../../kit/manager/manager_mod_test.go)：`TestManagerModPublishesItselfUnderTheManagerModName`、`TestManagerModForwardsTheLifecycleToTheEngine`

### kit/mods

- [guards_promises_test.go](../../../kit/mods/guards_promises_test.go)：`TestRegisterAllAndLookupsRefuseMissingInputs`
- [persistence_test.go](../../../kit/mods/persistence_test.go)：`TestPersistenceConfigDefaultsToDataEngine`、`TestPersistenceConfigRejectsOtherOrDisabledEngines`
- [registry_test.go](../../../kit/mods/registry_test.go)：`TestRegisterAllPreflightPreventsPartialPublication`
- [service_metrics_promises_test.go](../../../kit/mods/service_metrics_promises_test.go)：`TestServiceMetricsSwitch`
- [service_servicemods_test.go](../../../kit/mods/service_servicemods_test.go)：`TestEveryCapabilityNameIsUnique`、`TestEveryCapabilityNameIsNamespaced`、`TestKeyPrefixRejectsWhitespace`、`TestClusterKeyPrefixUsesFirstRedisHashTag`

### kit/mongo

- [close_contract_promises_test.go](../../../kit/mongo/close_contract_promises_test.go)：`TestMongoModConcurrentStopIsSafe`
- [close_contract_real_promises_test.go](../../../kit/mongo/close_contract_real_promises_test.go)：`TestRealMongoModCloseContract`
- [stop_retry_promises_test.go](../../../kit/mongo/stop_retry_promises_test.go)：`TestMongoModStopConvergesWhenTheClientIsAlreadyDisconnected`、`TestMongoModStopTwiceReturnsNil`
- [uri_log_promises_test.go](../../../kit/mongo/uri_log_promises_test.go)：`TestStartDoesNotLogTheMongoPassword`、`TestRedactedURIKeepsEverythingButThePassword`

### kit/nats

- [client_mod_transport_real_promises_test.go](../../../kit/nats/client_mod_transport_real_promises_test.go)：`TestRealGeneratedClientModCallsAJetStreamDeployment`
- [close_contract_promises_test.go](../../../kit/nats/close_contract_promises_test.go)：`TestNatsModConcurrentStopIsSafe`
- [close_contract_real_promises_test.go](../../../kit/nats/close_contract_real_promises_test.go)：`TestRealNatsModCloseContract`、`TestRealNatsModUndrainedCloseIsReportedOnce`
- [jetstream_capture_real_promises_test.go](../../../kit/nats/jetstream_capture_real_promises_test.go)：`TestRealLightweightCallIntoJetStreamDeploymentIsRefused`
- [jetstream_rpc_ackwait_real_promises_test.go](../../../kit/nats/jetstream_rpc_ackwait_real_promises_test.go)：`TestRealJetStreamRPCLongerThanAckWaitRunsOnce`
- [jetstream_rpc_toxic_integration_test.go](../../../kit/nats/jetstream_rpc_toxic_integration_test.go)：`TestToxicJetStreamRPCCallHonoursItsDeadlineWhileHalfOpen`
- [jetstream_stop_real_promises_test.go](../../../kit/nats/jetstream_stop_real_promises_test.go)：`TestRealJetStreamStopWaitsForInFlightHandler`
- [nats_mod_drain_budget_promises_test.go](../../../kit/nats/nats_mod_drain_budget_promises_test.go)：`TestNatsModStopAfterConnectionDrainBudgetConverges`、`TestNatsModStopReleasesAssemblyWhoseConnectionIsAlreadyClosed`
- [nats_mod_drain_budget_real_promises_test.go](../../../kit/nats/nats_mod_drain_budget_real_promises_test.go)：`TestRealNatsModStopAfterConnectionDrainBudgetConverges`
- [nats_mod_real_promises_test.go](../../../kit/nats/nats_mod_real_promises_test.go)：`TestRealNatsModProvideRefusesBareRegistriesAndReliableWithoutRedis`
- [nats_mod_stop_retry_promises_test.go](../../../kit/nats/nats_mod_stop_retry_promises_test.go)：`TestNatsModStopRetryAfterBusBudgetClosesAssembly`、`TestNatsModStopClosesAssemblyAfterTerminalBusError`
- [nats_mod_test.go](../../../kit/nats/nats_mod_test.go)：`TestJetStreamRPCConfigFromViper`、`TestJetStreamRPCConfigFromViperDisabledByDefault`

### kit/nest

- [durable_watermark_promises_test.go](../../../kit/nest/durable_watermark_promises_test.go)：`TestEntitySyncModWiresPipelinedDurableWatermark`
- [entity_sync_resync_test.go](../../../kit/nest/entity_sync_resync_test.go)：`TestEntitySyncModResyncsSubscribersAfterUnload`
- [entity_sync_test.go](../../../kit/nest/entity_sync_test.go)：`TestEntitySyncModConfigurationAndLifecycle`、`TestEntitySyncModRejectsInvalidInterval`、`TestEntitySyncModRebindsReloadedEntities`
- [entitysync_health_promises_test.go](../../../kit/nest/entitysync_health_promises_test.go)：`TestProvidedEntitySyncHealthIsRegistered`
- [mod_guards_promises_test.go](../../../kit/nest/mod_guards_promises_test.go)：`TestModRefusesMissingGetterRegistryAndDataEngine`
- [nest_mod_test.go](../../../kit/nest/nest_mod_test.go)：`TestModProvidesInstanceClientAndHealth`、`TestModSelectsDataEngineCommitterWithoutLegacyWALRuntime`、`TestRuntimeFailureFencesNestDispatch`
- [saga_start_limit_promises_test.go](../../../kit/nest/saga_start_limit_promises_test.go)：`TestEmitStartRefusesOverTheSharedLimitWithoutALocalCoordinator`
- [stop_contract_test.go](../../../kit/nest/stop_contract_test.go)：`TestNestModStopContract`
- [unload_resync_config_promises_test.go](../../../kit/nest/unload_resync_config_promises_test.go)：`TestNestModWiresUnloadResyncAndLoadTimeoutConfig`
- [unload_resync_stop_retry_promises_test.go](../../../kit/nest/unload_resync_stop_retry_promises_test.go)：`TestNestModStopRetryKeepsUnloadResyncUntilItDrains`

### kit/ops

- [admin_audit_promises_test.go](../../../kit/ops/admin_audit_promises_test.go)：`TestAdminRefusalIsAuditedWithoutToken`、`TestAdminSeparatesUnknownCommandFromHandlerFailure`
- [admin_deadline_promises_test.go](../../../kit/ops/admin_deadline_promises_test.go)：`TestOpsAdminCommandRunsUnderTheConfiguredDeadline`
- [listen_promises_test.go](../../../kit/ops/listen_promises_test.go)：`TestOpsStartFailsWhenTheAddressIsTaken`
- [mod_guards_promises_test.go](../../../kit/ops/mod_guards_promises_test.go)：`TestOpsModRefusesTokenlessAdminAndBareRegistries`、`TestOpsAdminTimeoutDefaultMatchesTheDeclaration`
- [ops_mod_test.go](../../../kit/ops/ops_mod_test.go)：`TestOpsAdminRequiresExplicitSecureToken`、`TestOpsAdminEndpointIsHiddenWhenDisabled`、`TestOpsReadyReflectsLifecycleState`、`TestOpsReadyIncludesDependencyHealth`、`TestOpsModStopWithContextUsesCallerContext`；其余 3 项见文件
- [production_secret_promises_test.go](../../../kit/ops/production_secret_promises_test.go)：`TestProductionRejectsExplicitlyAllowedDevToken`、`TestAdminTokenIsDeclaredSecret`、`TestDisabledProductionAdminDoesNotRequireUnusedToken`
- [readyz_checker_deadline_promises_test.go](../../../kit/ops/readyz_checker_deadline_promises_test.go)：`TestReadyzReportsAStuckCheckerAsFailInsteadOfHanging`
- [readyz_degraded_promises_test.go](../../../kit/ops/readyz_degraded_promises_test.go)：`TestReadyzTreatsDegradedAsReadyAndNamesTheDegradedChecker`、`TestReadyzStillFailsOnFailOrNotReady`
- [response_encoding_promises_test.go](../../../kit/ops/response_encoding_promises_test.go)：`TestAdminResultThatCannotBeEncodedIsNotReportedAsSuccess`、`TestNonCooperativeAdminCommandStopRetryDrains`
- [shutdown_ownership_promises_test.go](../../../kit/ops/shutdown_ownership_promises_test.go)：`TestInterruptedShutdownKeepsServerOwnership`、`TestIdleShutdownAndReadinessControls`、`TestShutdownDeadlineRetainsServerUntilRetryDrains`、`TestConcurrentShutdownCallersKeepTheirContexts`、`TestConcurrentOpsStartStopUsesCapturedServer`
- [stop_contract_test.go](../../../kit/ops/stop_contract_test.go)：`TestOpsStartServesOnTheBoundAddress`、`TestOpsStopContract`

### kit/redis

- [business_time_integration_test.go](../../../kit/redis/business_time_integration_test.go)：`TestBusinessTimeHighWaterMarkOnRealRedis`
- [close_contract_promises_test.go](../../../kit/redis/close_contract_promises_test.go)：`TestSingletonStoreCloseErrorIsReportedOnce`、`TestRedisModConcurrentStopIsSafe`
- [config_types_promises_test.go](../../../kit/redis/config_types_promises_test.go)：`TestClusterAddrsAcceptAYAMLListAndTrimEntries`、`TestRedisIntegerKeysAreReadStrictly`、`TestProductionRedisNeedsAnAddrOrClusterSeeds`
- [singleton_integration_test.go](../../../kit/redis/singleton_integration_test.go)：`TestSingletonStoreAcquireRenewRelease`、`TestSingletonStoreRenewAfterTheKeyExpiredIsNotHeld`、`TestSingletonStoreDoesNotTouchAnotherHoldersKey`、`TestSingletonStoreGetReadsEveryKeyInOrder`、`TestSingletonStoreGetAcrossClusterSlots`；其余 1 项见文件
- [singleton_test.go](../../../kit/redis/singleton_test.go)：`TestSingletonStoreRequiresAnExplicitRedisAddress`、`TestRedisModKeepsTheLocalhostDefault`、`TestSingletonStoreCloseIsIdempotent`
- [stop_retry_promises_test.go](../../../kit/redis/stop_retry_promises_test.go)：`TestRedisModStopConvergesAfterACloseError`

### kit/remoteentity

- [cached_max_staleness_test.go](../../../kit/remoteentity/cached_max_staleness_test.go)：`TestCachedMaxStalenessConfiguration`
- [cluster_config_test.go](../../../kit/remoteentity/cluster_config_test.go)：`TestClusterRequiresNonEmptyLockHashTag`
- [config_declaration_promises_test.go](../../../kit/remoteentity/config_declaration_promises_test.go)：`TestRemoteEntityDeclaredDefaultsMatchCoreDefaults`
- [interest_health_promises_test.go](../../../kit/remoteentity/interest_health_promises_test.go)：`TestExpiredLocalInterestsDoNotKeepHealthFailing`
- [interest_quota_config_test.go](../../../kit/remoteentity/interest_quota_config_test.go)：`TestInterestPerConsumerConfiguration`
- [lock_incarnation_promises_test.go](../../../kit/remoteentity/lock_incarnation_promises_test.go)：`TestRemoteEntityModPassesTheSingletonIncarnationToTheLocks`
- [remote_mirror_mod_promises_test.go](../../../kit/remoteentity/remote_mirror_mod_promises_test.go)：`TestRemoteMirrorModRegistersOnlyReadCapability`、`TestRemoteMirrorModRefusesASecondClientBesideTheOwner`、`TestRemoteMirrorModConfiguration`、`TestRemoteMirrorModStopContract`、`TestRemoteMirrorModStopCancelsInFlightReads`；其余 1 项见文件
- [snapshot_l2_key_prefix_test.go](../../../kit/remoteentity/snapshot_l2_key_prefix_test.go)：`TestSnapshotL2KeyPrefixConfiguration`
- [stop_log_promises_test.go](../../../kit/remoteentity/stop_log_promises_test.go)：`TestRemoteEntityModDoesNotLogStoppedWhenStopFails`
- [tombstone_wait_config_test.go](../../../kit/remoteentity/tombstone_wait_config_test.go)：`TestTombstoneWaitConfiguration`
- [write_budget_test.go](../../../kit/remoteentity/write_budget_test.go)：`TestWriteBudgetConfiguration`

### kit/saga

- [config_declaration_promises_test.go](../../../kit/saga/config_declaration_promises_test.go)：`TestSagaDeclaredDefaultsMatchCoreDefaults`、`TestSagaPayloadLimitIsOneDeclarationSharedWithNest`
- [config_types_promises_test.go](../../../kit/saga/config_types_promises_test.go)：`TestStepBudgetDurationsRefuseValuesWithoutAUnit`
- [effect_retention_promises_test.go](../../../kit/saga/effect_retention_promises_test.go)：`TestModRefusesAnEffectStreamThatOutlivesTheCompletionReceipts`、`TestModChecksEffectRetentionOnlyAgainstTheStreamItReadsResultsFrom`
- [health_promises_test.go](../../../kit/saga/health_promises_test.go)：`TestSagaModHealthDetectsEveryConsumerAndRecovers`
- [step_ack_wait_promises_test.go](../../../kit/saga/step_ack_wait_promises_test.go)：`TestStepTimeoutMustBeShorterThanTheStepConsumersAckWait`
- [step_budgets_test.go](../../../kit/saga/step_budgets_test.go)：`TestStepBudgetsComeFromConfigWithPerStepOverrides`、`TestStepBudgetConfigRejectsTyposAndImpossibleValues`、`TestStepBudgetConfigRejectsNamesThatDifferOnlyInCase`
- [step_override_case_promises_test.go](../../../kit/saga/step_override_case_promises_test.go)：`TestPerStepOverrideAppliesToMixedCaseNamesWithoutDefinitions`、`TestExactOverrideWinsOverTheLowercaseFallback`

### kit/service/account

- [account_mod_test.go](../../../kit/service/account/account_mod_test.go)：`TestModRefusesWithoutItsRequiredCollaborators`、`TestModRefusesAnEmptySessionSecret`、`TestModInitAndProvideContract`
- [account_test.go](../../../kit/service/account/account_test.go)：`TestNewRefusesAConfigThatCannotAuthenticate`、`TestLoginRequiresAVerifiedIdentity`、`TestAccountIsDerivedFromTheVerifiedIdentity`、`TestChannelOutageIsDistinctFromDenial`、`TestOneRolePerAccountPerServer`；其余 15 项见文件
- [bind_registry_promises_test.go](../../../kit/service/account/bind_registry_promises_test.go)：`TestProvideBindsRegistryAwareCollaborators`、`TestProvideReportsACollaboratorThatRefusesToBind`
- [bugfix_recovery_test.go](../../../kit/service/account/bugfix_recovery_test.go)：`TestCommittedLegacyNameOwnerIsNeverCompensated`、`TestUnknownSlotOutcomePreservesRoleAndName`、`TestLegacyVerifiedIdentityRetainsAccountAndRefusesCollision`
- [business_clock_promises_test.go](../../../kit/service/account/business_clock_promises_test.go)：`TestAccountTimesAreBusinessTimeAndSessionsAreSystemTime`
- [create_role_race_promises_test.go](../../../kit/service/account/create_role_race_promises_test.go)：`TestCreateRoleRefusesEachCommitTailAnomalyAndLeavesNothingBehind`、`TestCreateRoleRefusesAZeroPlayerIDAndRollsBack`
- [create_role_tail_test.go](../../../kit/service/account/create_role_tail_test.go)：`TestACreateWithAnUnknownWriteRetainsOneRecoverablePlan`
- [creation_table_test.go](../../../kit/service/account/creation_table_test.go)：`TestCreationTableEveryCell`、`TestCreationTableClassifiers`
- [default_clock_promises_test.go](../../../kit/service/account/default_clock_promises_test.go)：`TestMissingBusinessClockUsesProcessOffset`、`TestMissingSystemClockDoesNotFollowBusinessClock`
- [errcode_test.go](../../../kit/service/account/errcode_test.go)：`TestEverySentinelCarriesItsOwnCode`、`TestTheCodeSegmentIsExactlyAsAllocated`、`TestTheCodeSurvivesWrapping`、`TestAnUnclassifiedFailureReadsAsInternal`、`TestAForeignSentinelIsMappedIntoThisSegment`
- [pending_creation_other_name_promises_test.go](../../../kit/service/account/pending_creation_other_name_promises_test.go)：`TestADifferentNameReleasesAPlanWhoseNameWasCommittedElsewhere`、`TestADifferentNameKeepsAPlanThatCanStillComplete`、`TestAFailedReleaseOfADeadPlanIsCountedAndRetriedByTheNextName`、`TestAFailedReleaseOnTheSameNamePathIsCounted`
- [pending_creation_release_promises_test.go](../../../kit/service/account/pending_creation_release_promises_test.go)：`TestNameCommittedElsewhereReleasesThePendingSlot`、`TestNameReservedElsewhereKeepsThePendingSlot`、`TestResolvePendingCreationFreesTheSlotAndRecordsTheNote`、`TestResolvePendingCreationRefusesWhatItCannotProve`
- [pending_creation_unadmitted_rename_promises_test.go](../../../kit/service/account/pending_creation_unadmitted_rename_promises_test.go)：`TestADifferentNameReleasesAnUnadmittedPlanWhoseNameIsReservedElsewhere`、`TestADifferentNameReleasesAnUnadmittedPlanWhoseNameIsFree`
- [plan_release_count_promises_test.go](../../../kit/service/account/plan_release_count_promises_test.go)：`TestConcurrentReleasesOfOneDeadPlanCountOnce`
- [remaining_promises_test.go](../../../kit/service/account/remaining_promises_test.go)：`TestUpsertServerRejectsNegativeIDAndUnknownStatus`
- [role_guards_promises_test.go](../../../kit/service/account/role_guards_promises_test.go)：`TestSelectRoleRefusesARoleThatVanishedOrChangedOwnerBeforeTheWrite`、`TestSessionAndProfileOperationsRefuseAMissingRole`、`TestEntryPointsRefuseBlankIdentifiers`
- [rpc_glue_promises_test.go](../../../kit/service/account/rpc_glue_promises_test.go)：`TestGeneratedServerInitRefusesAClientOnlyProcessAndNamesTheMissingMod`、`TestGeneratedClientModRefusesANegativeCallTimeout`
- [rr_20260929_round1_test.go](../../../kit/service/account/rr_20260929_round1_test.go)：`TestReviewCrossServerRoleRollbackKeepsExistingName`
- [rr_20260929_round2_test.go](../../../kit/service/account/rr_20260929_round2_test.go)：`TestReviewVerifiedIdentityEncodingIsInjective`、`TestReviewLostSlotReplyCannotDeleteCommittedRole`
- [rr_20260929_round3_test.go](../../../kit/service/account/rr_20260929_round3_test.go)：`TestReview3BeforeWriteFailuresRemainRetryable`、`TestReview3NameCommitLostReplyCannotBurnName`、`TestReview3RoleCreateLostReplyCannotLeaveDuplicateNames`、`TestReview3InitialSlotUnknownCannotPermanentlyBlockCreate`、`TestReview3ProfileOutputCannotMutateRoleWithoutCAS`；其余 5 项见文件

### kit/service/chat

- [bugfix_retention_gap_test.go](../../../kit/service/chat/bugfix_retention_gap_test.go)：`TestBugfix5AgePruneHandlesInteriorAndTailGaps`、`TestBugfix5GapOnlyReportsTheRangeActuallyPaged`
- [business_clock_promises_test.go](../../../kit/service/chat/business_clock_promises_test.go)：`TestDisplayTimeIsBusinessTimeAndRetentionIsSystemTime`、`TestAMessageStoredBeforeSentAtUnixShowsItsStoredTime`
- [chat_mod_test.go](../../../kit/service/chat/chat_mod_test.go)：`TestModRefusesWithoutItsRequiredCollaborators`、`TestModInitAndProvideContract`、`TestModCopiesTheRulesItWasGiven`
- [chat_test.go](../../../kit/service/chat/chat_test.go)：`TestRequestTypesCannotCarryTrustOrIdentity`、`TestSystemTokenCannotBeDecodedFromARequestBody`、`TestOnlyThePrivilegedEntryPointSendsSystemMessages`、`TestPrivateHistoryIncludesOwnMessagesAndIsScopedToOnePeer`、`TestPrivateChannelRequiresAPeerThatIsNotTheCaller`；其余 24 项见文件
- [default_clock_promises_test.go](../../../kit/service/chat/default_clock_promises_test.go)：`TestMissingBusinessClockUsesProcessOffset`、`TestMissingSystemClockDoesNotFollowBusinessClock`
- [errcode_test.go](../../../kit/service/chat/errcode_test.go)：`TestEverySentinelCarriesItsOwnCode`、`TestTheCodeSegmentIsExactlyAsAllocated`、`TestTheCodeSurvivesWrapping`、`TestAnUnclassifiedFailureReadsAsInternal`、`TestAForeignSentinelIsMappedIntoThisSegment`
- [latest_page_gap_promises_test.go](../../../kit/service/chat/latest_page_gap_promises_test.go)：`TestLatestPageAfterCapacityEvictionIsNotAGap`、`TestLatestPageReportsOnlyInteriorAndTailHoles`
- [prune_failure_promises_test.go](../../../kit/service/chat/prune_failure_promises_test.go)：`TestPruneFailureIsCountedNotJustReturned`
- [remaining_promises_test.go](../../../kit/service/chat/remaining_promises_test.go)：`TestEvictedRequestKeyStillIdentifiesSender`、`TestReplaySurvivesPublishPolicyChange`、`TestZeroRetentionAgeDisablesAgePruning`、`TestChannelCodecRejectsOldUnidentifiedLedger`
- [replica_prune_paging_promises_test.go](../../../kit/service/chat/replica_prune_paging_promises_test.go)：`TestSkewedReplicasPruneAndPageWithHonestGaps`
- [retention_run_test.go](../../../kit/service/chat/retention_run_test.go)：`TestTheRetentionLoopPrunesTheEnumeratedChannels`、`TestPruneChannelsConfigurationFailsClosed`
- [rr_20260929_round1_test.go](../../../kit/service/chat/rr_20260929_round1_test.go)：`TestReviewCustomSharedChannelCannotReadPrivatePair`
- [validate_guards_promises_test.go](../../../kit/service/chat/validate_guards_promises_test.go)：`TestChannelSenderAndBodyTypeValidation`

### kit/service/directory

- [contention_test.go](../../../kit/service/directory/contention_test.go)：`TestAContendedWriteIsAcceptedOnce`
- [directory_mod_test.go](../../../kit/service/directory/directory_mod_test.go)：`TestModRefusesWithoutANormalizer`、`TestModRequiresAPositiveReservationTTL`、`TestModInitAndProvideContract`
- [directory_test.go](../../../kit/service/directory/directory_test.go)：`TestReserveIsCaseAndSpaceInsensitive`、`TestReserveIsIdempotentForTheSameOwner`、`TestLapsedReservationFreesTheKey`、`TestCommitRequiresTheHeldToken`、`TestCommitIsPermanentAndIdempotent`；其余 8 项见文件
- [errcode_test.go](../../../kit/service/directory/errcode_test.go)：`TestEverySentinelCarriesItsOwnCode`、`TestTheCodeSegmentIsExactlyAsAllocated`、`TestTheCodeSurvivesWrapping`、`TestAnUnclassifiedFailureReadsAsInternal`、`TestAForeignSentinelIsMappedIntoThisSegment`
- [guards_promises_test.go](../../../kit/service/directory/guards_promises_test.go)：`TestDirectoryRefusesBlankIdentifiersAndUnknownClaims`
- [remaining_promises_test.go](../../../kit/service/directory/remaining_promises_test.go)：`TestReleaseCannotBypassReservationToken`、`TestCASConflictHasRetryableDirectoryCode`、`TestReleaseExpiredReservationIsNoOp`
- [rr_20260929_round3_test.go](../../../kit/service/directory/rr_20260929_round3_test.go)：`TestReview3DirectoryDeleteCannotRemoveRecreatedOwner`、`TestDirectoryRequiresConditionalDelete`

### kit/service/examples/split

- [cancel_failure_promises_test.go](../../../kit/service/examples/split/cancel_failure_promises_test.go)：`TestGrantFailureReportsAFailedClaimReleaseToo`
- [guards_promises_test.go](../../../kit/service/examples/split/guards_promises_test.go)：`TestRewardFlowRefusesARegistryWithoutMail`
- [split_test.go](../../../kit/service/examples/split/split_test.go)：`TestTheConsumerResolvesInBothDeployments`、`TestBindingToTheConcreteTypeFailsInBothDeployments`、`TestOnlyTheOwningProcessPublishesTheLocalCapability`、`TestAProcessCannotOwnAndCallTheSameService`
- [stubs_test.go](../../../kit/service/examples/split/stubs_test.go)

### kit/service/global/activity

- [activity_mod_test.go](../../../kit/service/global/activity/activity_mod_test.go)：`TestModRequiresAReservationTTL`、`TestModRequiresAKeyPrefix`、`TestModRefusesANonPositiveDispatchBudget`、`TestModInitAndProvideContract`、`TestModReadsSweepGroups`
- [activity_test.go](../../../kit/service/global/activity/activity_test.go)：`TestOpenActivityIsInsertOnlyAndBounded`、`TestPendingWindowIsBoundedAndCountsRefusals`、`TestFirstNotifyCreatesTheCollectingSnapshot`、`TestCollectingEveryExpectedGameCompletesImmediately`、`TestDuplicateNotifyIsAnUnauditedNoOp`；其余 22 项见文件
- [admin_test.go](../../../kit/service/global/activity/admin_test.go)：`TestAnExhaustedDispatchCanBeReopened`、`TestReopeningPreservesTheAckToken`、`TestReopeningRefusesAnAckedOrPendingDispatch`、`TestReopenDispatchRequiresANote`、`TestReopeningAMissingDispatchIsRefused`；其余 2 项见文件
- [bugfix_cluster_prefix_test.go](../../../kit/service/global/activity/bugfix_cluster_prefix_test.go)：`TestBugfix7ActivityClusterRecoversPartialCompletion`、`TestBugfix7ActivityClusterRejectsInvalidPrefix`、`TestBugfix7ActivityStandaloneKeepsPlainPrefix`、`TestBugfix7ActivityTaggedClusterLifecycle`
- [bugfix_opening_recovery_test.go](../../../kit/service/global/activity/bugfix_opening_recovery_test.go)：`TestBugfix5LateCreateCannotBorrowAnAlreadyPromisedSlot`、`TestBugfix5LostAdmissionReplyRecoversItsOriginalPlan`、`TestBugfix5CreatedOrConfirmedReplyLossRemainsRecoverable`、`TestBugfix5OversizedLegacyWindowRotatesAcrossServiceRebuild`、`TestBugfix5LegacyOpeningNeedsAPlanInsteadOfTimeoutReclaim`
- [bugfix_progress_proof_test.go](../../../kit/service/global/activity/bugfix_progress_proof_test.go)：`TestStaleReservedReaderCannotApplyAfterProofWasReclaimed`、`TestUnconfirmedProgressBackpressureNeverEvictsProof`
- [business_clock_promises_test.go](../../../kit/service/global/activity/business_clock_promises_test.go)：`TestTheModWiresTheCoordinatorToTheBusinessClock`
- [confirmed_key_ownership_promises_test.go](../../../kit/service/global/activity/confirmed_key_ownership_promises_test.go)：`TestSweepSkipsConfirmedKeysOfAnotherGroup`、`TestSweepSkipsInvalidConfirmedKeys`
- [default_clock_promises_test.go](../../../kit/service/global/activity/default_clock_promises_test.go)：`TestMissingBusinessClockUsesProcessOffset`
- [dispatch_rediscovery_promises_test.go](../../../kit/service/global/activity/dispatch_rediscovery_promises_test.go)：`TestSweepRetriesDispatchesAcrossRounds`、`TestDeliveringIndexRetiresAtTerminalStates`
- [errcode_test.go](../../../kit/service/global/activity/errcode_test.go)：`TestAForeignSentinelIsMappedIntoThisSegment`、`TestTheCodeSurvivesWrappingAndTheOuterCodeWins`、`TestANilErrorIsCodeOK`
- [groups_live_limit_promises_test.go](../../../kit/service/global/activity/groups_live_limit_promises_test.go)：`TestAGroupFitsOneLiveQuery`
- [groups_promises_test.go](../../../kit/service/global/activity/groups_promises_test.go)：`TestAGroupLargerThanOneWindowIsRefusedWhenLoaded`、`TestAFullGroupOpensAWindowWithTheCoordinator`、`TestAGroupsFileThatCannotBeUsedIsRefusedByName`、`TestGroupsAnswerWhichGroupASIDIsIn`、`TestModSweepsTheGroupsInTheGroupsFile`
- [guards_promises_test.go](../../../kit/service/global/activity/guards_promises_test.go)：`TestActivityEntryPointsRefuseBlankAndNonPositiveArguments`
- [legacy_opening_sweep_promises_test.go](../../../kit/service/global/activity/legacy_opening_sweep_promises_test.go)：`TestSweepReclaimsLegacyOpeningAndSkipsMalformedIntent`、`TestEveryMalformedIntentShapeIsSkippedNotFatal`、`TestOneFailingOpeningCreateDoesNotStallTheRestOfTheGroup`
- [monotonic_business_time_promises_test.go](../../../kit/service/global/activity/monotonic_business_time_promises_test.go)：`TestDispatchBackoffAndProofExpiryRunOnTheMonotonicBusinessClock`、`TestAReopenedDispatchIsOwedNow`
- [open_expected_group_promises_test.go](../../../kit/service/global/activity/open_expected_group_promises_test.go)：`TestTheCoordinatorRefusesAnExpectedSetTheGroupsFileDoesNotAllow`、`TestTheCoordinatorOpensAnyLiveSubsetOfTheGroup`、`TestTheCoordinatorRefusesToStartWithoutAGroupsFile`、`TestNewRefusesACoordinatorWithoutGroups`
- [open_window_lifecycle_promises_test.go](../../../kit/service/global/activity/open_window_lifecycle_promises_test.go)：`TestOpenSweepInterleaveKeepsTheActivityInTheWindow`、`TestSweepHealsAnOpeningEntryWhoseActivityExists`、`TestSweepRetainsAndRecoversAnUnknownOpeningAfterGrace`
- [opening_identity_promises_test.go](../../../kit/service/global/activity/opening_identity_promises_test.go)：`TestOpenActivityRejectsMalformedPersistedIntentBeforeCreate`、`TestOpeningSweepRejectsInvalidOrForeignGroupBeforeSideEffects`、`TestOpeningRecoveryKeepsValidIntentAndLegacyAdmission`、`TestMalformedForeignOpeningDiagnosticsClearWithItsWindow`、`TestProgressReconcileRetainsAConcurrentNewProof`
- [owed_dispatch_promises_test.go](../../../kit/service/global/activity/owed_dispatch_promises_test.go)：`TestAGameFindsWhatItIsOwedWithoutGuessingActivityIDs`、`TestTheSweepDoesNotConsumeDeliveryAttempts`
- [pending_proof_expiry_promises_test.go](../../../kit/service/global/activity/pending_proof_expiry_promises_test.go)：`TestExpiredLedgerEntriesFreeOrphanedPendingProofs`、`TestFullPendingProofWindowReportsBacklogNotConflict`、`TestReconcileProgressCompletesLostMarksAndFreesTheParticipant`
- [remaining_promises_test.go](../../../kit/service/global/activity/remaining_promises_test.go)：`TestGraceWindowRejectsSubsecond`、`TestUnreadableActivityDoesNotStallHealthyExpiry`、`TestSweepStillRetiresDeliveriesWhenAnotherActivityIsUnreadable`、`TestOpeningCreateCollisionReadsWinnerBeforeClassification`、`TestOperatorCanRetirePermanentlyOfflineGameAndReopenSameDelivery`
- [rr_20260929_round1_test.go](../../../kit/service/global/activity/rr_20260929_round1_test.go)：`TestReviewLastDispatchRetryKeepsAckWindow`、`TestReviewUnconfirmedProgressSurvivesRingEviction`
- [rr_20260929_round2_test.go](../../../kit/service/global/activity/rr_20260929_round2_test.go)：`TestReviewPositiveProgressCannotWrapNegative`
- [sweep_failures_promises_test.go](../../../kit/service/global/activity/sweep_failures_promises_test.go)：`TestSweepFailuresAreCountedNotJustLogged`
- [sweep_loop_promises_test.go](../../../kit/service/global/activity/sweep_loop_promises_test.go)：`TestTheSweepLoopAdvancesTheConfiguredGroups`、`TestTheSweepLoopWithoutGroupsSweepsNothingAndStopsCleanly`
- [sweep_promises_test.go](../../../kit/service/global/activity/sweep_promises_test.go)：`TestABoundedSweepCompletesTheEarliestDeadlineFirst`、`TestTheSweepHealsACompletedActivityWhoseDispatchesWereNeverCreated`
- [window_entries_promises_test.go](../../../kit/service/global/activity/window_entries_promises_test.go)：`TestDeliveringSkipsMalformedEntriesAndKeepsThem`、`TestPendingActivitiesSkipsMalformedEntries`、`TestRetireDeliveredReportsOnlyWhatItRemoved`、`TestOperatorRemovesOnlyMalformedWindowEntries`

### kit/service/global

- [bind_retry_promises_test.go](../../../kit/service/global/bind_retry_promises_test.go)：`TestBindRetriedAfterUnknownOutcomeReturnsTheSameBinding`
- [errcode_test.go](../../../kit/service/global/errcode_test.go)：`TestEverySentinelCarriesItsOwnCode`、`TestTheCodeSegmentIsExactlyAsAllocated`、`TestTheCodeSurvivesWrapping`、`TestAnUnclassifiedFailureReadsAsInternal`、`TestAForeignSentinelIsMappedIntoThisSegment`
- [global_mod_test.go](../../../kit/service/global/global_mod_test.go)：`TestModRequiresAKeyPrefix`、`TestModIgnoresTheActivitySettingsThatMovedOut`、`TestModInitAndProvideContract`
- [global_test.go](../../../kit/service/global/global_test.go)：`TestBindIsInsertOnly`、`TestMigrationRequiresTheCurrentEpoch`、`TestMigrationCanBeAborted`、`TestConcurrentMigrationsHaveOneWinner`、`TestNewRejectsAnIncompleteConfig`；其余 2 项见文件
- [guards_promises_test.go](../../../kit/service/global/guards_promises_test.go)：`TestMigrationRequestsRefuseEachInvalidShape`
- [missing_guards_promises_test.go](../../../kit/service/global/missing_guards_promises_test.go)：`TestMigrationOperationsRefuseUnknownGames`
- [promises_test.go](../../../kit/service/global/promises_test.go)：`TestARetriedCompletionIsReportedAsAReplay`
- [remaining_promises_test.go](../../../kit/service/global/remaining_promises_test.go)：`TestCompleteMigrationReplaysTheOriginalEpoch`、`TestRouteCodecRequiresCompletionReceiptFormat`

### kit/service/integration

- [business_clock_test.go](../../../kit/service/integration/business_clock_test.go)：`TestOffsetMovesMatchChatAndAccountBusinessTimesButNotRetentionOrSessions`
- [client_mods_test.go](../../../kit/service/integration/client_mods_test.go)：`TestEveryClientModDependsOnTheNATSMod`
- [mods_test.go](../../../kit/service/integration/mods_test.go)：`TestEveryDeclaredCapabilityIsPublished`、`TestEveryPublishedCapabilityIsUsable`、`TestEveryModWritesUnderItsConfiguredPrefix`、`TestTheOwningModPublishesMailAsTheInterfaceOnly`、`TestEveryOwningModPublishesTheInterfaceAndNotTheImplementation`；其余 8 项见文件
- [redis_test.go](../../../kit/service/integration/redis_test.go)：`TestDirectoryRunsOnRedis`、`TestADirectoryIsNotAMutexEvenOnRealRedis`、`TestSessionRunsOnRedis`、`TestSessionAllowsOneLiveRunPerOwnerOnRedis`、`TestAccountRunsOnRedis`；其余 10 项见文件

### kit/service/mail

- [batch_cluster_integration_test.go](../../../kit/service/mail/batch_cluster_integration_test.go)：`TestIntegrationMailModClusterPagination`
- [client_mod_transport_promises_test.go](../../../kit/service/mail/client_mod_transport_promises_test.go)：`TestClientModFollowsTheConfiguredRPCTransport`
- [fake_envelopes_test.go](../../../kit/service/mail/fake_envelopes_test.go)
- [harness_test.go](../../../kit/service/mail/harness_test.go)
- [mail_mod_test.go](../../../kit/service/mail/mail_mod_test.go)：`TestModRequiresASendTTL`、`TestAMissingBroadcastDelivererIsAllowedAndRefusesBroadcasts`、`TestModInitAndProvideContract`
- [rpc_test.go](../../../kit/service/mail/rpc_test.go)：`TestBothModsPublishTheSameCapabilityName`、`TestTheClientModPublishesTheInterfaceNotTheConcreteType`、`TestTheClientModFailsWithoutTheBus`、`TestTheServerRefusesToRunOnAClientCapability`、`TestTheServerRefusesAMissingCapability`
- [wiring_promises_test.go](../../../kit/service/mail/wiring_promises_test.go)：`TestGeneratedWiringRefusesEachMisassembledProcess`

### kit/service/match

- [grouping_contract_promises_test.go](../../../kit/service/match/grouping_contract_promises_test.go)：`TestGroupingIsNotAnInjectionPointOfTheStore`
- [sweep_run_test.go](../../../kit/service/match/sweep_run_test.go)：`TestTheExpiryLoopSweepsTheConfiguredQueues`、`TestAStoreWithoutAQueueSetSweepsNothing`、`TestSweepQueuesConfigurationFailsClosed`

### kit/service/platform

- [admin_test.go](../../../kit/service/platform/admin_test.go)：`TestAnExhaustedOrderCanBeReopenedAndThenDelivers`、`TestReopeningRefusesAnyStateThatWouldGrantTwice`、`TestSettlingRefusesAnOrderTheRetryLoopStillOwns`、`TestSettledIsTerminalAndDistinctFromDelivered`、`TestBothOperationsRequireANote`；其余 3 项见文件
- [bugfix_external_outcome_test.go](../../../kit/service/platform/bugfix_external_outcome_test.go)：`TestBugfix4UnknownGrantMustBlockSettlement`、`TestBugfix4UnknownGrantMustNotGrantAgain`、`TestUnknownGrantProofSurvivesServiceReconstruction`、`TestBugfix4KnownNoGrantControl`、`TestBugfix4CallbackReceiptMustOwnPendingProof`
- [bugfix_pending_retirement_test.go](../../../kit/service/platform/bugfix_pending_retirement_test.go)：`TestBugfix6RetirementCannotHideLatePaidOrder`、`TestBugfix6PendingMaintenanceControls`、`TestBugfix6PlatformClusterRejectsMalformedTag`、`TestBugfix6PlatformTaggedClusterControl`
- [bugfix_reconciliation_test.go](../../../kit/service/platform/bugfix_reconciliation_test.go)：`TestReconciledSettlementRejectsStaleDeliveryCompletion`
- [errcode_test.go](../../../kit/service/platform/errcode_test.go)：`TestEverySentinelCarriesItsOwnCode`、`TestTheCodeSegmentIsExactlyAsAllocated`、`TestTheCodeSurvivesWrapping`、`TestAnUnclassifiedFailureReadsAsInternal`、`TestAForeignSentinelIsMappedIntoThisSegment`
- [order_guards_promises_test.go](../../../kit/service/platform/order_guards_promises_test.go)：`TestOrderOperationsRefuseAnUnrecordedOrder`、`TestCallbackAndDeliveryRefuseEmptyInputs`、`TestAuthSessionRefusesANonPositivePlayerID`
- [order_race_promises_test.go](../../../kit/service/platform/order_race_promises_test.go)：`TestCallbackReportsEachOrderRaceAndGrantsNothing`
- [pending_index_promises_test.go](../../../kit/service/platform/pending_index_promises_test.go)：`TestAStoredOrderIsImmediatelyEnumerableIntegration`、`TestATerminalOrderLeavesTheIndexIntegration`、`TestBackoffMovesAnOrderInTheIndexIntegration`、`TestTheStoreIsTheDefaultPendingSource`、`TestAClusterWithoutAHashTagIsRefused`；其余 1 项见文件
- [pending_orders_promises_test.go](../../../kit/service/platform/pending_orders_promises_test.go)：`TestPendingOrderSourceFeedsTheRetryLoop`、`TestWithoutAPendingSourceTheLoopIsExplicitlyOff`、`TestPendingSourceErrorIsReportedNotFatal`、`TestBackgroundPathRecoversAPaidOrderWithoutAnotherCallback`
- [platform_mod_test.go](../../../kit/service/platform/platform_mod_test.go)：`TestModRefusesWithoutItsRequiredCollaborators`、`TestModRefusesAnEmptySecretAtStartupNotAtCallTime`、`TestModRefusesANonPositiveAttemptBudget`、`TestModInitAndProvideContract`
- [platform_test.go](../../../kit/service/platform/platform_test.go)：`TestASessionRequiresACredentialTheChannelAccepts`、`TestACredentialWithoutASecretIsRefused`、`TestAnUnreachableChannelIsNotADenial`、`TestAVerifierAnsweringForAnotherChannelIsRefused`、`TestAReplayedCallbackDeliversExactlyOnce`；其余 13 项见文件
- [poison_pending_promises_test.go](../../../kit/service/platform/poison_pending_promises_test.go)：`TestAnUndecodableOrderStopsBlockingThePage`
- [race_test.go](../../../kit/service/platform/race_test.go)：`TestElapsedBackoffDoesNotStartAnotherExternalGrant`、`TestClaimingASpentBudgetRecordsExhaustionRatherThanRefusingInPlace`
- [registry_bound_promises_test.go](../../../kit/service/platform/registry_bound_promises_test.go)：`TestCollaboratorsAreBoundBeforeTheServiceIsBuilt`、`TestABindingFailureStopsTheProcess`、`TestCollaboratorsThatDoNotAskAreNotTouched`
- [remaining_promises_test.go](../../../kit/service/platform/remaining_promises_test.go)：`TestValidateSessionRejectsZeroPlayer`、`TestModCanExplicitlyDisableBackgroundRetry`、`TestUnknownClaimDoesNotCallOrRepeatExternalGrant`
- [rr_20260929_round1_test.go](../../../kit/service/platform/rr_20260929_round1_test.go)：`TestReviewSettledOrderSurvivesLateDelivery`
- [validate_promises_test.go](../../../kit/service/platform/validate_promises_test.go)：`TestOrderValidateRefusesEachBrokenField`、`TestCredentialAndVerifiedValidateRefuseEachBlankField`

### kit/service/rank

- [bugfix_add_overflow_test.go](../../../kit/service/rank/bugfix_add_overflow_test.go)：`TestBugfix5OverflowDoesNotChangeScoreOrConsumeRequest`、`TestBugfix5ConcurrentAddsCannotWrapAfterCASRetry`
- [bugfix_cluster_prefix_test.go](../../../kit/service/rank/bugfix_cluster_prefix_test.go)：`TestBugfix6RankClusterRejectsInvalidPrefix`、`TestBugfix6RankTaggedClusterLifecycle`
- [bugfix_ring_compatibility_test.go](../../../kit/service/rank/bugfix_ring_compatibility_test.go)：`TestLegacyRequestRingUpgradesWithoutDroppingPriorIDs`、`TestMalformedRequestRingFailsClosed`
- [default_clock_promises_test.go](../../../kit/service/rank/default_clock_promises_test.go)：`TestMissingBusinessClockUsesProcessOffset`
- [errcode_test.go](../../../kit/service/rank/errcode_test.go)：`TestEverySentinelCarriesItsOwnCode`、`TestTheCodeSegmentIsExactlyAsAllocated`、`TestTheCodeSurvivesWrapping`、`TestAnUnclassifiedFailureReadsAsInternal`
- [fake_redis_test.go](../../../kit/service/rank/fake_redis_test.go)
- [lua_shape_promises_test.go](../../../kit/service/rank/lua_shape_promises_test.go)：`TestStoreRefusesLuaResultsOfTheWrongShape`
- [member_promises_test.go](../../../kit/service/rank/member_promises_test.go)：`TestDecodeEntryRefusesEachMalformedMember`
- [member_test.go](../../../kit/service/rank/member_test.go)：`TestMemberByteOrderIsTheRankingOrder`、`TestMemberFieldsAreFixedWidth`、`TestEntryRoundTripsAndBriefDoesNotAffectOrder`、`TestDecodeRejectsMalformedMembers`、`TestEncodingIsDeterministic`
- [owner_guards_promises_test.go](../../../kit/service/rank/owner_guards_promises_test.go)：`TestOwnerZeroAndMissingMembersAreRefused`
- [rank_mod_test.go](../../../kit/service/rank/rank_mod_test.go)：`TestModRefusesAConfigurationWithoutAKeyPrefix`、`TestModReadsItsKeyPrefix`、`TestModFailsWithoutTheRedisCapability`、`TestModDeclaresItsRedisDependency`、`TestModName`；其余 1 项见文件
- [redis_integration_test.go](../../../kit/service/rank/redis_integration_test.go)：`TestIntegrationSwapScriptLeavesNoOrphanMember`、`TestIntegrationConcurrentAddsAgainstRealRedis`、`TestIntegrationOrderingMatchesTheEncoding`
- [rr_20260929_round1_test.go](../../../kit/service/rank/rr_20260929_round1_test.go)：`TestReviewCommaRequestIsIdempotent`、`TestReviewAroundAcceptedMaximumRadius`、`TestReviewUnchangedMemberCASProtectsRequestRing`
- [store_test.go](../../../kit/service/rank/store_test.go)：`TestPageRanksMatchTheirScores`、`TestPageLimitCannotBeBypassed`、`TestAddModeIsIdempotentPerRequest`、`TestIdempotencyRingIsBoundedAndRecognisesRecentRequests`、`TestEqualScoresOrderByWhoArrivedFirst`；其余 13 项见文件

### kit/service/session

- [admin_test.go](../../../kit/service/session/admin_test.go)：`TestTheOperatorSurfaceIsNotOnTheSessionInterface`
- [harness_test.go](../../../kit/service/session/harness_test.go)
- [options_test.go](../../../kit/service/session/options_test.go)：`TestWithSweepOwnersCarriesDeploymentRoster`
- [session_mod_test.go](../../../kit/service/session/session_mod_test.go)：`TestModRefusesWithoutAReleaser`、`TestModRequiresARequestTTL`、`TestModInitAndProvideContract`

### kit/statslog

- [gauge_lifecycle_promises_test.go](../../../kit/statslog/gauge_lifecycle_promises_test.go)：`TestEntityGaugesReturnToZeroWhenAKindEmpties`
- [gauges_test.go](../../../kit/statslog/gauges_test.go)：`TestEachCollectionPublishesRuntimeAndEntityGauges`
- [readonly_snapshot_promises_test.go](../../../kit/statslog/readonly_snapshot_promises_test.go)：`TestStatsEndpointDoesNotConsumeFileWindow`
- [statslog_test.go](../../../kit/statslog/statslog_test.go)：`TestStatsLogModWritesSeparateJSONLFile`、`TestStatsLogModDisabledDoesNotCreateFile`、`TestStatsLogProviderUnregisterDoesNotRemoveReplacement`、`TestStatsLogProviderPanicIsCaptured`、`TestStatsLogStopWithContextReturnsWhenFlushIsBlocked`；其余 1 项见文件
- [write_failure_promises_test.go](../../../kit/statslog/write_failure_promises_test.go)：`TestStatsLogStartReportsAnUnwritableDirectory`、`TestStatsLogPeriodicFailuresAreCountedWarnedOnceAndRecoveryIsLogged`

### kit/syncbus

- [config_test.go](../../../kit/syncbus/config_test.go)：`TestSyncBusReadsTheSyncbusSection`、`TestSyncBusRefusesAnUnknownTransport`
- [mod_guards_promises_test.go](../../../kit/syncbus/mod_guards_promises_test.go)：`TestSyncBusModRefusesRegistriesWithoutHealthOrNats`
- [mod_stop_test.go](../../../kit/syncbus/mod_stop_test.go)：`TestSyncModStopWithContextPrefersContextStopper`、`TestSyncModStopTimeoutKeepsTheBusForRetry`
- [stop_drain_integration_test.go](../../../kit/syncbus/stop_drain_integration_test.go)：`TestRealJetStreamSyncBusStopDrainsAnInFlightHandler`
- [stream_migration_integration_test.go](../../../kit/syncbus/stream_migration_integration_test.go)：`TestNonDefaultPrefixUpgradeFailsOnOverlapAndResumesOnTheLegacyStream`
- [stream_name_test.go](../../../kit/syncbus/stream_name_test.go)：`TestSyncBusStreamIsDerivedFromThePrefix`
- [unsubscribe_drain_integration_test.go](../../../kit/syncbus/unsubscribe_drain_integration_test.go)：`TestRealSyncBusUnsubscribeDrainsTheSubscription`、`TestRealSyncBusUnsubscribeFromOwnHandler`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../maintenance/PERFORMANCE.md)，没有新测量则不能改容量承诺。
