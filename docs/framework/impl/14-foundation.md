# 其他包与公共基础设施：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/14-foundation.md)

## 如何阅读

除了核心链路，框架还有集合、索引、网络驱动和游戏AI等工具包。它们各自解决较小的问题。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`gameplay/ai`、`infra/base/container`、`infra/network/etcd`、`infra/base/index`、`infra/storage/migration`、`infra/base/misc`、`infra/base/safemap`、`infra/network/nats`、`internal/operation`、`internal/rangecontract`、`infra/network/ownerroute`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 驱动不反向污染 core 契约依赖。
2. 集合回调、对象池复用与生命周期按实际 API 限制。
3. 通用 migration 存在不代表 DAO 自动迁移存在。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### ai

7 个实现文件、8 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [behavior_strategy.go](../../../gameplay/ai/behavior_strategy.go) | `ActionEnd`、`BehaviorContext`、`BehaviorStrategyOptions`、`BehaviorStrategy`、`TaskflowAction` |
| [blackboard.go](../../../gameplay/ai/blackboard.go) | `Blackboard` |
| [controller.go](../../../gameplay/ai/controller.go) | `ControllerHooks`、`Controller` |
| [nodes.go](../../../gameplay/ai/nodes.go) | `ParallelPolicy`、`Parallel`、`Repeat`、`UntilSuccess`、`Succeeder`、`Condition`、`Guard`、`Cooldown`、`TimeLimit`、`RandomSelector` |
| [strategy.go](../../../gameplay/ai/strategy.go) | `Context`、`Strategy`、`StoppableStrategy` |
| [tree.go](../../../gameplay/ai/tree.go) | `Status`、`Node`、`FuncNode`、`Sequence`、`Selector`、`Inverter` |
| [tree_parser.go](../../../gameplay/ai/tree_parser.go) | `Registry` |

### container

4 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [bucket.go](../../../infra/base/container/bucket.go) | `BucketHolder`、`Bucket` |
| [keymap.go](../../../infra/base/container/keymap.go) | `Key`、`KeyMap` |
| [object_pool.go](../../../infra/base/container/object_pool.go) | `ObjectPool` |
| [topologic_sort.go](../../../infra/base/container/topologic_sort.go) | `TopologicalSortCache` |

### etcd

8 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [client.go](../../../infra/network/etcd/client.go) | `IEtcd`、`KV`、`Cmp`、`CmpTarget`、`CmpOp`、`Op`、`OpType`、`TxnResponse` |
| [config.go](../../../infra/network/etcd/config.go) | `Config` |
| [discovery.go](../../../infra/network/etcd/discovery.go) | `IDiscovery`、`ServiceInfo`、`IServiceWatcher`、`IServiceWatcherStatus`、`ServiceEvent` |
| [election.go](../../../infra/network/etcd/election.go) | `IElection`、`IFencedElection`、`IElectionFactory` |
| [errors.go](../../../infra/network/etcd/errors.go) | 函数/方法或内部实现；见源码 |
| [local_mirror.go](../../../infra/network/etcd/local_mirror.go) | `PrefixSnapshot`、`IPrefixSnapshotReader`、`LocalMirrorConfig`、`LocalMirrorPublishOptions`、`LocalMirrorEntry`、`LocalMirrorStatus`、`LocalMirrorChangeType`、`LocalMirrorChange`、`LocalMirrorHandler`、`LocalMirrorSubscribeOptions`、`ILocalMirror`、`ILocalMirrorSubscriber` |
| [watch_callback.go](../../../infra/network/etcd/watch_callback.go) | 函数/方法或内部实现；见源码 |
| [watcher.go](../../../infra/network/etcd/watcher.go) | `IWatcher`、`IWatcherError`、`IWatcherReady`、`WatchHandler`、`IWatchSubscription`、`WatchEvent`、`EventType`、`WatchOption` |

### etcd/driver

7 个实现文件、8 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assembly.go](../../../infra/network/etcd/driver/assembly.go) | `Assembly` |
| [client.go](../../../infra/network/etcd/driver/client.go) | `Client` |
| [discovery.go](../../../infra/network/etcd/driver/discovery.go) | `Discovery` |
| [election.go](../../../infra/network/etcd/driver/election.go) | `ElectionFactory` |
| [local_mirror.go](../../../infra/network/etcd/driver/local_mirror.go) | 函数/方法或内部实现；见源码 |
| [local_mirror_subscription.go](../../../infra/network/etcd/driver/local_mirror_subscription.go) | 函数/方法或内部实现；见源码 |
| [watcher.go](../../../infra/network/etcd/driver/watcher.go) | 函数/方法或内部实现；见源码 |

### index

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [index.go](../../../infra/base/index/index.go) | `Index`、`OrderedIndex` |

### internal/operation

2 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [lifetime.go](../../../internal/operation/lifetime.go) | `Lifetime` |
| [serial.go](../../../internal/operation/serial.go) | `Serial` |

### internal/rangecontract

1 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [rangecontract.go](../../../internal/rangecontract/rangecontract.go) | `Ops`、`Subject` |

### migration

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [migration.go](../../../infra/storage/migration/migration.go) | `Versioned`、`Step`、`Registry` |

### misc

2 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [hash.go](../../../infra/base/misc/hash.go) | 函数/方法或内部实现；见源码 |
| [integer.go](../../../infra/base/misc/integer.go) | `Integer` |

### nats

8 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [client.go](../../../infra/network/nats/client.go) | `IClient`、`MsgHandler`、`Msg`、`ISubscription` |
| [config.go](../../../infra/network/nats/config.go) | `Config` |
| [errors.go](../../../infra/network/nats/errors.go) | 函数/方法或内部实现；见源码 |
| [jetstream.go](../../../infra/network/nats/jetstream.go) | `JetStreamStorage`、`JetStreamDeliverPolicy`、`IJetStream`、`JetStreamConfig`、`JetStreamPublishOptions`、`JetStreamPublishAck`、`JetStreamConsumerConfig`、`JetStreamHandler`、`JetStreamMsg`、`IJetStreamSubscription` |
| [message.go](../../../infra/network/nats/message.go) | `NatsMsg`、`BroadcastType` |
| [rpc.go](../../../infra/network/nats/rpc.go) | `IRpc`、`RpcCallback`、`RetryPolicy` |
| [subject.go](../../../infra/network/nats/subject.go) | `SubjectBuilder` |

### nats/driver

8 个实现文件、7 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assembly.go](../../../infra/network/nats/driver/assembly.go) | `Assembly` |
| [client.go](../../../infra/network/nats/driver/client.go) | `Client` |
| [jetstream.go](../../../infra/network/nats/driver/jetstream.go) | `JetStreamClient` |
| [options.go](../../../infra/network/nats/driver/options.go) | `ClientOptions` |
| [rpc.go](../../../infra/network/nats/driver/rpc.go) | `RPCClient` |
| [subscription.go](../../../infra/network/nats/driver/subscription.go) | 函数/方法或内部实现；见源码 |

### ownerroute

2 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [bus.go](../../../infra/network/ownerroute/bus.go) | `BusTransport` |
| [route.go](../../../infra/network/ownerroute/route.go) | `OwnerRoute`、`Resolver`、`Transport`、`Router` |

### safemap

5 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [fast.go](../../../infra/base/safemap/fast.go) | `FastMap` |
| [hash.go](../../../infra/base/safemap/hash.go) | `Integer` |
| [map.go](../../../infra/base/safemap/map.go) | `IMap`、`Entry`、`HashFunc`、`ComputeFunc` |
| [sharded.go](../../../infra/base/safemap/sharded.go) | `ShardedSafeMap` |
| [small.go](../../../infra/base/safemap/small.go) | `SmallSafeMap` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Range`、`Test.*Close`、`Test.*Cancel`、`Test.*Replay`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### ai

- [behavior_test.go](../../../gameplay/ai/behavior_test.go)：`TestNodesGuardCooldownRepeatParallel`、`TestBehaviorStrategyDrivesTreeThroughController`、`TestTaskflowActionInterruptHook`、`TestParseTreeAssemblesAndRuns`、`TestParseTreeFailFast`；其余 3 项见文件
- [controller_test.go](../../../gameplay/ai/controller_test.go)：`TestControllerFailedInitKeepsPreviousStrategy`、`TestBehaviorTreeReadyDoesNotAdvanceSequence`、`TestBlackboardConcurrentAccessKeepsEveryWorkersWrite`
- [deferred_launch_promises_test.go](../../../gameplay/ai/deferred_launch_promises_test.go)：`TestTaskflowActionLaunchedInsideARunnerCallbackStillCompletes`
- [frozen_completion_promises_test.go](../../../gameplay/ai/frozen_completion_promises_test.go)：`TestAFrozenControllerStillDeliversTheCompletionATreeIsWaitingFor`、`TestAFrozenControllerStillDeliversMissionEndsButNotTicks`
- [guards_promises_test.go](../../../gameplay/ai/guards_promises_test.go)：`TestSetStrategyRefusesNilReentrantAndRejectedSwitches`、`TestParseTreeRefusesMissingRegistryClockAndMalformedPredicates`
- [parallel_decision_promises_test.go](../../../gameplay/ai/parallel_decision_promises_test.go)：`TestParallelStopsTickingChildrenOnceTheOutcomeIsDecided`、`TestParallelStillInterruptsRunningChildrenWhenItCompletes`
- [switch_in_callback_promises_test.go](../../../gameplay/ai/switch_in_callback_promises_test.go)：`TestSwitchRequestedInsideATickRunsAfterTheTickReturns`、`TestDeferredSwitchFailureIsReportedAndKeepsTheCurrentStrategy`
- [tree_parser_promises_test.go](../../../gameplay/ai/tree_parser_promises_test.go)：`TestTreeRegistryRefusesNamelessAndDuplicateFactories`、`TestParseTreeRefusesEachDefectByMessageAndPath`

### container

- [container_test.go](../../../infra/base/container/container_test.go)：`TestBucketHolder`、`TestKeyMap`、`TestTopologicalSort`、`TestObjectPoolReusesFreelistObjectsWithoutReset`、`TestObjectPoolPutMovesObjectBetweenLists`；其余 6 项见文件
- [range_contract_test.go](../../../infra/base/container/range_contract_test.go)：`TestContainerRangeContract`

### etcd/driver

- [assembly_test.go](../../../infra/network/etcd/driver/assembly_test.go)：`TestAssembleBuildsDiscoveryAndElectionsOnOneConnection`
- [cluster_failover_integration_test.go](../../../infra/network/etcd/driver/cluster_failover_integration_test.go)：`TestRealEtcdThreeNodeLeaderLossPreservesWatchDiscoveryAndElection`
- [discovery_shutdown_test.go](../../../infra/network/etcd/driver/discovery_shutdown_test.go)：`TestDiscoverySuppressesLeaseLostWarningAfterDeregister`、`TestDiscoveryWarnsWhenLeaseLostUnexpectedly`、`TestDiscoveryReregistersAfterUnexpectedLeaseLoss`、`TestDiscoveryDoesNotReregisterAfterDeregister`、`TestDiscoveryRejectsDuplicateRegisterWithoutLeakingFirstRegistration`；其余 5 项见文件
- [election_test.go](../../../infra/network/etcd/driver/election_test.go)：`TestElectionFirstCampaignKeepsPreCampaignLeaderChannel`、`TestElectionLeaderPreservesBackendError`、`TestElectionCampaignContextCancellationAfterElectionDoesNotLoseLeadership`、`TestElectionFenceTracksLeadershipTerm`、`TestElectionResignClearsLifecycleBeforeReturning`；其余 13 项见文件
- [local_mirror_test.go](../../../infra/network/etcd/driver/local_mirror_test.go)：`TestLocalMirrorAppliesWatchEventsAndReturnsIndependentValues`、`TestLocalMirrorWaitsForServerWatchReadiness`、`TestLocalMirrorResnapshotsAfterWatchCloses`、`TestLocalMirrorPublishesAndUsesRevisionCAS`、`TestLocalMirrorUsesNativeEtcdPrefixSemantics`；其余 13 项见文件
- [real_etcd_integration_test.go](../../../infra/network/etcd/driver/real_etcd_integration_test.go)：`TestRealEtcdGetOfAMissingKeyIsNotFound`、`TestRealEtcdCloseAfterLeaseVanishedIsClean`、`TestRealEtcdFailedCampaignRevokesItsLease`、`TestRealEtcdCanceledWinningCampaignLeavesNoPhantomLeader`、`TestRealEtcdCanceledWaitingCampaignRevokesItsLease`；其余 3 项见文件
- [stop_contract_test.go](../../../infra/network/etcd/driver/stop_contract_test.go)：`TestDiscoveryDeregisterStopContract`、`TestAssemblyCloseStopContract`、`TestClientCloseErrorIsReportedOnceThenNil`、`TestClientConcurrentCloseAllReturnNil`、`TestClientCallsAfterCloseFailFastWithErrClosed`；其余 2 项见文件
- [watcher_test.go](../../../infra/network/etcd/driver/watcher_test.go)：`TestWatcherReadyWaitsForServerResponse`、`TestWatcherReadyReportsCompactedStartRevision`、`TestWatcherReadyReportsUnexpectedChannelClose`、`TestServiceWatcherReportsUnexpectedChannelClosure`、`TestServiceWatcherCloseIsNotReportedAsFailure`

### etcd

- [watch_callback_test.go](../../../infra/network/etcd/watch_callback_test.go)：`TestWatchCallbackDeliversInOrderAndReportsHandlerError`、`TestWatchCallbackRecoversPanicAndExplicitCloseIsClean`、`TestWatchCallbackReportsWatcherAndContextTermination`、`TestSubscribeAndWatchCallbackRefuseMissingParts`
- [watch_lifetime_test.go](../../../infra/network/etcd/watch_lifetime_test.go)：`TestEtcdLifetimeConcurrentCloseWaitsForHandlerAndWatcherAndPreservesError`、`TestEtcdLifetimeParentCancellationStartsCleanupBeforeHandlerReturns`、`TestEtcdLifetimeCallbackCloseBudgetIncludesOwnedWatcher`、`TestEtcdLifetimeCallbackErrorAndExplicitCloseOwnership`

### index

- [index_test.go](../../../infra/base/index/index_test.go)：`TestIndexQuery`、`TestOrderedIndexDefaultOrdersIntegersNumerically`、`TestOrderedIndexCustomLessWins`、`TestUpsertAcceptsAValueThatIsNotEqualToItself`、`TestDefaultOrderHandlesKeysOfDifferentDynamicTypes`；其余 1 项见文件

### internal/operation

- [lifetime_test.go](../../../internal/operation/lifetime_test.go)：`TestLifetimeStopDrainsOnlyAdmittedCalls`、`TestLifetimeWaitIsBoundedRetryableAndReportsTheRealDrain`、`TestLifetimeZeroValueWaitWithoutCallsReturnsNil`
- [serial_test.go](../../../internal/operation/serial_test.go)：`TestSerialLaterCallerWaitsWithinItsOwnContext`

### migration

- [migration_test.go](../../../infra/storage/migration/migration_test.go)：`TestRegistryRun`、`TestRegistriesRefuseNilReceiversAndNilData`、`TestRegistryRefusesEachInvalidStepAndRequest`

### misc

- [misc_test.go](../../../infra/base/misc/misc_test.go)：`TestHash64IsDeterministicAndSpreads`

### nats/driver

- [client_test.go](../../../infra/network/nats/driver/client_test.go)：`TestNatsClientNilBoundaryFailsClosed`、`TestInvokeNatsHandlerContainsAndReportsPanic`、`TestSubscriptionValidationRejectsANilHandler`、`TestClientTranslatesEachTransportErrorToItsSentinel`、`TestClientRefusesInvalidSubjectsQueuesAndHandlers`；其余 4 项见文件
- [closed_state_test.go](../../../infra/network/nats/driver/closed_state_test.go)：`TestEveryExportedDriverMethodHasAClosedStateCheck`、`TestEveryExportedDriverMethodReportsErrClosedAfterClose`、`TestACallAdmittedBeforeCloseFinishingAfterItReportsErrClosed`、`TestInFlightCallAsyncExpiresAsErrClosedAfterClientClose`、`TestCallsRacingCloseEitherCompleteOrReportErrClosed`；其余 3 项见文件
- [jetstream_test.go](../../../infra/network/nats/driver/jetstream_test.go)：`TestJetStreamTerminalClassification`、`TestInvokeJetStreamHandlerContainsPanic`、`TestJetStreamStreamConfigMapping`、`TestJetStreamConsumerConfigMappingDefaults`、`TestJetStreamNakBackoffIsBounded`；其余 4 项见文件
- [options_test.go](../../../infra/network/nats/driver/options_test.go)：`TestHandleNatsDisconnectLogsExpectedCloseAsInfo`、`TestHandleNatsDisconnectLogsUnexpectedErrorAsError`、`TestIgnoreDiscoveredServersIsAnOptInThatReachesTheConnection`
- [real_nats_integration_test.go](../../../infra/network/nats/driver/real_nats_integration_test.go)：`TestRealRawRequestAndSubscriptionDrain`、`TestRealRawSlowConsumerAndCloseWaitForActualCallback`、`TestRealRawCancellationDoesNotReplayAnAdmittedRequest`、`TestRealScopedInboxReusesSubscriptionAndCancelsOnlyItsWaiter`、`TestRealNatsEveryExportedMethodAnswersFromTheDriverStateAfterAnUndrainedClose`；其余 1 项见文件
- [rpc_contract_test.go](../../../infra/network/nats/driver/rpc_contract_test.go)：`TestRPCBudgetAssemblyCloseBoundsCallbackDrain`、`TestRPCBudgetPendingTerminalRacesCompleteOnce`、`TestRPCBudgetAssemblyClosesCompletedPool`、`TestRPCBudgetStopRetainsFullQueueFallbackAndAllowsRetry`、`TestRPCBudgetCallbackCanCancelItsOwnStopWait`；其余 4 项见文件
- [rpc_test.go](../../../infra/network/nats/driver/rpc_test.go)：`TestRpcClientStopCancelsPendingCalls`、`TestRpcClientDispatchCallbackCompletesWhenPoolRejects`、`TestRpcClientCallAsyncAfterStopCancelsImmediately`、`TestRpcClientPendingHasSingleTerminalWinner`、`TestRpcClientCancelsOneHundredThousandPendingExactlyOnce`；其余 1 项见文件

### nats

- [nats_test.go](../../../infra/network/nats/nats_test.go)：`TestPermanentIsIdempotentAndNilSafe`、`TestDefaultRetryPolicyDoesNotRetryNonIdempotentRPC`

### ownerroute

- [route_test.go](../../../infra/network/ownerroute/route_test.go)：`TestRouterExecutesLocalCommand`、`TestRouterSendsRemoteCommand`、`TestRouterRefusesInvalidKeysAndOwnerlessRoutes`、`TestTransportAndRouterRefuseMissingParts`

### safemap

- [bench_test.go](../../../infra/base/safemap/bench_test.go)
- [map_test.go](../../../infra/base/safemap/map_test.go)：`TestSmallSafeMapContract`、`TestSmallSafeMapBSONV2RoundTrip`、`TestShardedSafeMapContract`、`TestFastMapContract`、`TestFastMapGrowthAndTombstoneReuse`；其余 7 项见文件
- [range_contract_test.go](../../../infra/base/safemap/range_contract_test.go)：`TestSafemapRangeContract`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../performance/STABLE-v1.24.0.md)，没有新测量则不能改容量承诺。

## 7. 示例与工具源码补充

这些目录不是生产模块能力承诺；示例展示具体接线，性能脚本提供专用负载。包索引包含它们，避免用目录数量冒充生产模块数量。

- [cmd/walinspect/main.go](../../../cmd/walinspect/main.go)
- [examples/configgen/cfg/cfg_gen.go](../../../examples/configgen/cfg/cfg_gen.go)
- [examples/configgen/main.go](../../../examples/configgen/main.go)
- [examples/lubanreal/gen/Item.go](../../../examples/lubanreal/gen/Item.go)
- [examples/lubanreal/gen/Tables.go](../../../examples/lubanreal/gen/Tables.go)
- [examples/lubanreal/gen/TbItem.go](../../../examples/lubanreal/gen/TbItem.go)
- [examples/lubanreal/main.go](../../../examples/lubanreal/main.go)
- [examples/robotdemo/main.go](../../../examples/robotdemo/main.go)
- [scripts/perf/nest-msg/main.go](../../../scripts/perf/nest-msg/main.go)
- [scripts/perf/sync-aoi/async.go](../../../scripts/perf/sync-aoi/async.go)
- [scripts/perf/sync-aoi/client.go](../../../scripts/perf/sync-aoi/client.go)
- [scripts/perf/sync-aoi/main.go](../../../scripts/perf/sync-aoi/main.go)
- [scripts/perf/sync-aoi/nest.go](../../../scripts/perf/sync-aoi/nest.go)
- [scripts/perf/sync-aoi/trace.go](../../../scripts/perf/sync-aoi/trace.go)
