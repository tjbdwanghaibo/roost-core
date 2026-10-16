# 核心：Sync、Lockstep 与客户端：实现与维护

适用版本：v1.27.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）；源码目录最初按 v1.24.0 提取、之后随改动维护，链接由根包文档门禁保证存在，缺漏以源码为准。[设计与使用](../guide/04-sync.md)

## 如何阅读

服务器的数据改变后，客户端需要知道哪些变化；对局也可能需要把玩家输入按帧转发。Sync提供这些同步能力。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`framework/sync`、`framework/sync/syncstream`、`infra/network/gateway`、`client/wire`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 持久水位和 Guard 冻结共同约束状态可见性。
2. 客户端状态缺口恢复全量，不在失效 base 上继续应用。
3. Lockstep 席位来自认证 session；历史默认按 `HistoryRetainFrames`（3600 帧）在 Tick 末尾自动裁剪，越界追帧以 `ErrCatchupUnservable` 失败并发 C9 通知，客户端终止、不自动重同步。
4. 有限投递、流保留与业务 ACK 语义不能等同 exactly-once。
5. `Manager.Drain` 不放弃任何条目；ctx 结束时返回带卡住条目摘要的 `*DrainStalledError`，`errors.Is` ctx 错误不变。

## 2A. 冻结、发送和客户端权限实现走读

[mode.go](../../../framework/sync/entitysync/mode.go)的CaptureSync先取得当前subject及授权profiles，确认对象仍有效，调用SubjectSyncState.FreezeSyncViews；替换后的旧state不能留下可发送冻结物。reserveFrozen以CAS计入总字节，返回一次性释放函数，避免重试/取消双重释放。WakeSync合并通知，丢一个唤醒不等于丢pending事实。

[flush.go](../../../framework/sync/entitysync/flush.go)负责取pending、准备subject视图（prepareSubscribersLocked整理订阅者与跳过的会话）、按会话构建帧并逐帧交付（pushSessionFrames）、传输准入后结算；capturedAbove检查CommitLSN水位，requireSnapshotsAfterRetry处理部分会话已经接纳后的恢复。必须在实际Push结果之后结算基线，不能先标clean再尝试发送。

[command.go](../../../framework/sync/lockstep/command.go)的HandleCommand须在Match串行handler中运行：先拒绝closed/非法command，再查sessionOwners；旁观者只走SpectatorCatchup，普通玩家分别走StartCatchup/ReportHash/SubmitInput。没有seat字段让客户端覆盖服务端绑定，也没有把发送成功当模拟已执行的业务ACK。

[match.go](../../../framework/sync/lockstep/match.go)的Tick顺序是：切帧、追加历史、结算上次的追帧页、实时广播、发本次追帧页，最后retainWindow裁历史（hash报告按64帧步长）。裁剪必须在追帧页发出之后：页面发出前已编码成独立字节，先裁会让“从最早帧追帧”在同一Tick被判越界。startCatchup把起点0当帧1核对，越界时登记追帧状态并立即返回`*CatchupUnservableError`；pumpCatchup发现游标早于`History.start()`时把状态标为unservable、发C9通知（作为该会话最后一次可靠发送，结算后删除状态）。客户端侧[assembler.go](../../../framework/sync/lockstep/assembler.go)的Ingest按首字节0xC9识别通知，[robot/lockstep.go](../../../robot/lockstep.go)与C# LockstepClient随即终止。

[drain.go](../../../framework/sync/entitysync/drain.go)的Drain只在ctx结束时收集诊断：pending按ID升序，附Flush记录的deferral（原因、首次暂缓时间、最后错误，由settleDeferrals在Flush末尾维护：被暂缓的重新排队并保留首次时间，本轮处理过、没再暂缓的删除，forget/Close清空）；随后是waitingSnapshots、政策阶段的释放/重新提交通知与各政策的PolicyHooks.Stalls。诊断只读，不改变排空判定与重试；指标无标签。

[packet.go](../../../client/wire/packet.go)从flags解释Kind并在Header.Validate拒绝保留组合、超长载荷和非法心跳标记。头解码不解释业务payload，注册路由与Lockstep/Sync解码器仍各自校验。

## 2B. 工作分支的空间组件与政策发布顺序

[Block AOI实现说明](../INTEREST-BLOCK-AOI.md)给出锁与作用域。`policy.SpatialComponent`用手写writer写DAO，Queue*复用`SyncConditionFor`。`queueMu`不包住AOI计算；每批先写空间、再并行observer、最后归并所有来源，防止来源切换重建基线。

`SubjectSyncState.HoldSyncPublication`阻止内容越过尚未应用的政策事实；它只约束PrepareViews，不改变提交确认条件、不阻止锁内冻结。事实应用、丢弃、退出与关闭都必须释放，不能只在成功路径释放。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### client/wire

1 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [packet.go](../../../client/wire/packet.go) | `PayloadKind`、`Header`、`Packet` |

### gateway

20 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gateway.go](../../../infra/network/gateway/gateway.go) | `Principal`、`Session`、`Request`、`Endpoint`、`EndpointFunc`、`Middleware` |
| [middleware.go](../../../infra/network/gateway/middleware.go) | 函数/方法或内部实现；见源码 |

### sync/entitysync

16 个实现文件、9 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [counters.go](../../../framework/sync/entitysync/counters.go) | `ManagerCounters` |
| [drain.go](../../../framework/sync/entitysync/drain.go) | `DrainStall`、`DrainStalledError` |
| [errors.go](../../../framework/sync/entitysync/errors.go) | 函数/方法或内部实现；见源码 |
| [flush.go](../../../framework/sync/entitysync/flush.go) | 函数/方法或内部实现；见源码 |
| [manager.go](../../../framework/sync/entitysync/manager.go) | `ManagerConfig`、`Manager`、`ManagerStats` |
| [mode.go](../../../framework/sync/entitysync/mode.go) | `SyncMode` |
| [policy_queue.go](../../../framework/sync/entitysync/policy_queue.go) | `PolicyHooks`、`RetractedSubscription`、`SubscriptionSourceHooks`、`ReleasedSubscription`、`SubscriptionStamp` |
| [session.go](../../../framework/sync/entitysync/session.go) | 函数/方法或内部实现；见源码 |
| [snapshot_budget.go](../../../framework/sync/entitysync/snapshot_budget.go) | `SnapshotBudget` |
| [snapshot_requests.go](../../../framework/sync/entitysync/snapshot_requests.go) | 函数/方法或内部实现；见源码 |
| [subject.go](../../../framework/sync/entitysync/subject.go) | 函数/方法或内部实现；见源码 |
| [subscriptions.go](../../../framework/sync/entitysync/subscriptions.go) | `SubscriptionSource` |
| [trace.go](../../../framework/sync/entitysync/trace.go) | `SyncTraceEvent`、`SyncTrace` |
| [transport.go](../../../framework/sync/entitysync/transport.go) | `SessionID`、`Transport`、`TransportFunc`、`SessionLifecycle`、`AsyncTransport`、`FrameSizeLimiter` |
| [wire.go](../../../framework/sync/entitysync/wire.go) | 函数/方法或内部实现；见源码 |
| [workspace.go](../../../framework/sync/entitysync/workspace.go) | 函数/方法或内部实现；见源码 |

### sync/entitysync/policy

8 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [aoi.go](../../../framework/sync/entitysync/policy/aoi.go) | `InterestEventKind`、`InterestEvent`、`AOIConfig`、`AOI` |
| [aoi_cluster.go](../../../framework/sync/entitysync/policy/aoi_cluster.go) | `AreaID`、`AOICluster` |
| [direct.go](../../../framework/sync/entitysync/policy/direct.go) | `Direct` |
| [group.go](../../../framework/sync/entitysync/policy/group.go) | `GroupConfig`、`Group` |
| [interest.go](../../../framework/sync/entitysync/policy/interest.go) | `InterestConfig`、`Refusal`、`Interest` |
| [interest_queue.go](../../../framework/sync/entitysync/policy/interest_queue.go) | 函数/方法或内部实现；见源码 |
| [profiles.go](../../../framework/sync/entitysync/policy/profiles.go) | 函数/方法或内部实现；见源码 |
| [source.go](../../../framework/sync/entitysync/policy/source.go) | `Source`、`RelationSource` |

### sync/frame

3 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [codec.go](../../../framework/sync/frame/codec.go) | 函数/方法或内部实现；见源码 |
| [frame_limits.go](../../../framework/sync/frame/frame_limits.go) | `Limits`、`ObjectRef`、`SnapshotMeta`、`Kind`、`ObjectOperation`、`ComponentOperation`、`ComponentDelta`、`ObjectDelta`、`Frame` |
| [receiver.go](../../../framework/sync/frame/receiver.go) | `Receiver` |

### sync/lockstep

8 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assembler.go](../../../framework/sync/lockstep/assembler.go) | `FrameAssembler` |
| [command.go](../../../framework/sync/lockstep/command.go) | `Operation`、`Command` |
| [desync.go](../../../framework/sync/lockstep/desync.go) | `DesyncDetector`、`DesyncVerdict` |
| [history.go](../../../framework/sync/lockstep/history.go) | `History` |
| [match.go](../../../framework/sync/lockstep/match.go) | `MatchConfig`、`Match` |
| [sequencer.go](../../../framework/sync/lockstep/sequencer.go) | `PlayerID`、`FrameID`、`Input`、`Frame`、`SequencerConfig`、`Sequencer` |
| [tcp.go](../../../framework/sync/lockstep/tcp.go) | `TCPSender` |
| [wire.go](../../../framework/sync/lockstep/wire.go) | `RedundantEncoder`、`CatchupUnservableError` |

### sync/nettransport

9 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [channel.go](../../../framework/sync/nettransport/channel.go) | `SendError`、`ErrorHandler`、`AsyncTransportConfig`、`AsyncTransport`、`AsyncTransportStats` |
| [counters.go](../../../framework/sync/nettransport/counters.go) | `AsyncTransportCounters` |
| [kcp_transport.go](../../../framework/sync/nettransport/kcp_transport.go) | `KCPTransportConfig`、`KCPTransport`、`KCPTransportStats`、`KCPDatagramHandler` |
| [quic_transport.go](../../../framework/sync/nettransport/quic_transport.go) | `QUICTransportConfig`、`QUICTransport`、`QUICTransportStats` |
| [sender.go](../../../framework/sync/nettransport/sender.go) | `DatagramSender`、`DatagramPayloadLimiter`、`DatagramBatchSender`、`ReliableSender`、`CompositeTransport` |
| [session.go](../../../framework/sync/nettransport/session.go) | `SessionID`、`SessionInfo` |
| [transport.go](../../../framework/sync/nettransport/transport.go) | `Transport`、`DatagramBatchTransport`、`SessionTransport`、`TransportFunc` |
| [udp_crypto.go](../../../framework/sync/nettransport/udp_crypto.go) | `AEADSessionProtector` |
| [udp_transport.go](../../../framework/sync/nettransport/udp_transport.go) | `UDPTransportConfig`、`UDPTransport`、`UDPTransportStats`、`UDPReceiveHandler` |

### sync/syncbus

4 个实现文件、3 个测试文件（含示例 `example_test.go`）。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [delivery_id.go](../../../framework/sync/syncbus/delivery_id.go) | `DeliveryIDs` |
| [patch_syncer.go](../../../framework/sync/syncbus/patch_syncer.go) | `PatchSyncerConfig`、`PatchSyncer` |
| [subscription.go](../../../framework/sync/syncbus/subscription.go) | `Subscription` |
| [sync.go](../../../framework/sync/syncbus/sync.go) | `SyncMsg`、`Handler`、`IPublisher`、`IContextPublisher`、`ISubscriber`、`ILiveSubscriber`、`ISyncBus` |

### sync/syncbus/driver

2 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [jetstream.go](../../../framework/sync/syncbus/driver/jetstream.go) | `JetStreamSyncConfig` |
| [nats.go](../../../framework/sync/syncbus/driver/nats.go) | `Bus`（ISyncBus + StopWithContext，两种驱动都实现，SyncBusMod 在编译期依赖它） |

### sync/syncbus/mirror

1 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [envelope.go](../../../framework/sync/syncbus/mirror/envelope.go) | `Op`、`Envelope`、`Store`、`Replicator` |

### syncstream

8 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [file_durability_unix.go](../../../framework/sync/syncstream/file_durability_unix.go) | 函数/方法或内部实现；见源码 |
| [file_durability_windows.go](../../../framework/sync/syncstream/file_durability_windows.go) | 函数/方法或内部实现；见源码 |
| [file_journal.go](../../../framework/sync/syncstream/file_journal.go) | `FileHistoryJournal` |
| [lifecycle_journal.go](../../../framework/sync/syncstream/lifecycle_journal.go) | `HistoryMutationKind`、`HistoryMutation`、`HistoryTarget`、`HistoryJournal` |
| [observability.go](../../../framework/sync/syncstream/observability.go) | `MetricSink`、`HealthOptions`、`HealthStatus` |
| [observability_impl.go](../../../framework/sync/syncstream/observability_impl.go) | `PublisherHealthOptions`、`PublisherHealthStatus` |
| [publisher.go](../../../framework/sync/syncstream/publisher.go) | `ErrorHandler`、`ConfirmedSyncPublisher`、`Publisher`、`PublisherOptions`、`Handler`、`SubscribeOptions`、`PublisherMetrics`、`PacketPublisher`、`BufferedPublisherOptions`、`BufferedPublisherMetrics`、`BufferedPublisher` |
| [syncstream.go](../../../framework/sync/syncstream/syncstream.go) | `Observer`、`Stream`、`Packet`、`Sink`、`BatchSink`、`HistoryOptions`、`History`、`ResyncReason`、`ResyncRequest`、`ResyncResult`、`SnapshotProvider`、`StreamStatus`、`HistoryMetrics`、`HistorySnapshot`、`HistoryStreamSnapshot`、`HistoryStore` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Lockstep`、`Test.*Watermark`、`Test.*Session`、`Test.*Retry`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### client/wire

- [golden_test.go](../../../client/wire/golden_test.go)：`TestClientProtocolGolden`
- [packet_test.go](../../../client/wire/packet_test.go)：`TestPacketKindsAndGolden`、`TestPacketRefusesBadHeaderBeforeReadingPayload`

### gateway

- [binding_test.go](../../../infra/network/gateway/binding_test.go)：`TestConcurrentBindSharesOneOwnerAndOneIdentity`、`TestBindingActivationReplacementAndOldCloseAreIsolated`、`TestBindingTerminalCapacityIsReservedBeforeAdmission`、`TestSessionReplacementDoesNotCrossPlayerIdentity`
- [gateway_test.go](../../../infra/network/gateway/gateway_test.go)：`TestChainOrderAndAuthentication`、`TestCompleteChannelEnvelopeMustFitActualLimits`、`TestGateStopRetriesShareOneDrainWaiter`
- [middleware_test.go](../../../infra/network/gateway/middleware_test.go)：`TestRateLimitByPlayerAndMessage`、`TestRateLimitBackstopHoldsWithoutLimiter`、`TestTimeoutAndRecover`、`TestRecoverIsolatesReporterFailure`、`TestRecoverIsolatesDiagnosticFailure`；其余 2 项见文件
- [packet_test.go](../../../infra/network/gateway/packet_test.go)：`TestBindingPacketHasCompleteIdentityAndCurrentFormat`、`TestBudgetConsumesElapsedTimeAndDoesNotRestartAtReceiver`、`TestSubjectSourceAndAudienceAreExplicit`
- [tcp_handshake_test.go](../../../infra/network/gateway/tcp_handshake_test.go)：`TestTCPBindFinishesBeforeACKAndActivationCanPushAfterACK`、`TestTCPRefusedOrPanickingBindNeverAcknowledgesAuthentication`、`TestTCPCloseInterruptsBlockedWriteAndPushSequencesFollowStream`
- [tcp_test.go](../../../infra/network/gateway/tcp_test.go)：`TestTCPRuntimeBelongsToOneServerBeforeStart`、`TestTCPDefaultRequestBudgetsAreAppliedAtConstruction`、`TestHeartbeatKeepsAuthenticatedPushConnectionAlive`、`TestPerConnectionTokenBucketIncludesHeartbeats`、`TestFrameRoundTrip`；其余 29 项见文件

### sync/entitysync

- [batch_window_test.go](../../../framework/sync/entitysync/batch_window_test.go)：`TestOnChangeBatchWindowDoesNotSlide`、`TestOnChangeBatchWindowTickAndExplicitFlush`、`TestOnChangeZeroWindowAndConfiguration`、`TestOnChangeBatchWindowCancelAndRestart`、`TestOnChangeBatchWindowPreservesDelivery`；其余 1 项见文件
- [benchmark_test.go](../../../framework/sync/entitysync/benchmark_test.go)
- [manager_lifecycle_test.go](../../../framework/sync/entitysync/manager_lifecycle_test.go)：`TestInFlightDeliveryCannotOverwriteNewIntent`、`TestStopAndCloseRespectDeadlineWhileAnotherFlushIsInFlight`、`TestStopCancelsPushAndPreventsRestartUntilItReturns`、`TestCloseReleasesStateAfterFinalFlushError`、`TestModeConfigurationRequiresProducerAndKeepsDefault`；其余 12 项见文件
- [manager_test.go](../../../framework/sync/entitysync/manager_test.go)：`TestEachSessionGetsASnapshotThenDeltasAndOnePackServesThemAll`、`TestAFailingSessionIsClosedAloneAndTheOthersStillReceive`、`TestRetryLaterAbandonsTheTickWithoutBlamingAnySession`、`TestUnsubscribeAndUnregisterOweARemoveOnlyToWhoHoldsTheObject`、`TestCloseSessionForgetsItsSubscriptionsWithoutAFrame`；其余 14 项见文件
- [resubmit_test.go](../../../framework/sync/entitysync/resubmit_test.go)：`TestRetractedSubscriptionsWithoutAPolicyAreNotRestored`、`TestRetractedPolicySubscriptionsAreHandedBackOnceAfterReRegistration`、`TestReleasedOrUnregisteredRetractionsAreNotHandedBack`、`TestResubmitInterruptedByASecondRetractionIsHandedBackAgain`、`TestQueuedResubmitReturnsToTheRetractionTableOnce`；其余 4 项见文件
- [retirement_test.go](../../../framework/sync/entitysync/retirement_test.go)：`TestRetractSyncSubjectRemovesHeldObjectsBeforeRecreate`、`TestRetractSyncSubjectDoesNotRetireALaterRegistration`、`TestRegisterAfterRetirementCompletesOnceTheRemoveIsOut`、`TestRegisterAfterRetirementCompletesWhenTheLastSubscriberCloses`、`TestAQueuedRegistrationIsBoundedAndCancellable`；其余 11 项见文件
- [snapshot_budget_test.go](../../../framework/sync/entitysync/snapshot_budget_test.go)：`TestProfileDowngradeReplacesClientFields`、`TestWholeEntityPacketsSplitAndRetainIndependentFrameState`、`TestWholeEntityTooLargeIsExplicitAndNeverTruncated`、`TestWholeEntitySplitRetryContinuesFrameAndContentBaselines`、`TestSnapshotBudgetRotatesAndDoesNotDelayLiveUpdates`；其余 14 项见文件
- [snapshot_scheduling_test.go](../../../framework/sync/entitysync/snapshot_scheduling_test.go)：`TestLargeSnapshotFiniteBacklog`、`TestLargeSnapshotStarvedUnderChurn`、`TestLargeSnapshotSingleSession`、`TestByteBlockedWindowDoesNotRecaptureUntilNextWindow`、`TestByteBlockedSnapshotsAreNotRecapturedEveryWindow`；其余 9 项见文件
- [subscription_test.go](../../../framework/sync/entitysync/subscription_test.go)：`TestSubscriptionIndexTracksAllRemovalPaths`、`TestSessionRecoveryDoesNotLockUnsubscribedSubjects`、`TestSubscriptionIndexConcurrentLifecycle`、`TestFlushDoesNotSendOldSubscriptionToReopenedSession`、`TestPendingSnapshotStillRemovesAnAlreadyDeliveredObject`；其余 5 项见文件

### sync/entitysync/policy

- [aoi_test.go](../../../framework/sync/entitysync/policy/aoi_test.go)：`TestInterestEnterLeaveWithHysteresis`、`TestInterestBandsAndObserverMovement`、`TestInterestMaxVisibleEvictsFarthest`、`TestInterestObserverIsAlsoSubjectSymmetry`、`TestInterestDeterministicEventStream`；其余 23 项见文件
- [benchmark_test.go](../../../framework/sync/entitysync/policy/benchmark_test.go)
- [interest_test.go](../../../framework/sync/entitysync/policy/interest_test.go)：`TestObservedBlocksKeepOnePositionAndSubscription`、`TestSpatialAndBlockHandoverDoesNotRecreateBaseline`、`TestCoverageCommitRollbackRejectAndLifetime`、`TestCoverageLateObserverBudgetAndVisibilityLimit`、`TestFactAdmissionDoesNotWaitForPolicyLock`；其余 16 项见文件
- [policy_test.go](../../../framework/sync/entitysync/policy/policy_test.go)：`TestEnteringSubscribesToYourself`、`TestDistanceSubscribesReleasesAndDoesNotChurnOnTheBoundary`、`TestARelationKeepsASubscriptionDistanceDropped`、`TestLeavingReleasesBothDirections`、`TestARefusedSubscribeIsSaidAgainUntilItIsTaken`；其余 13 项见文件
- [relation_index_test.go](../../../framework/sync/entitysync/policy/relation_index_test.go)：`TestRelationDeltaMatchesFullReplacement`、`TestRelationForgetAfterReplacementClearAndReadd`、`TestRelationSetMatchesMembershipModel`、`TestRelationSetWorkspaceConcurrentLifecycle`
- [resubmit_test.go](../../../framework/sync/entitysync/policy/resubmit_test.go)：`TestInterestResubmitsASubscriptionTheFrameworkRetracted`、`TestInterestDoesNotResubmitAPairItReleasedWhileTheSubjectWasAway`、`TestEachPolicyResubmitsItsOwnRetractedSubscription`、`TestGroupAndDirectResubmitAfterRetraction`、`TestGroupResubmitsAfterASecondRetractionBeforeDelivery`；其余 5 项见文件

### sync/frame

- [benchmark_test.go](../../../framework/sync/frame/benchmark_test.go)
- [codec_test.go](../../../framework/sync/frame/codec_test.go)：`TestEncodeFrameRefusesEachMalformedDelta`、`TestFrameCodecEnforcesLimitsAndRefusesCorruptBytes`、`TestEncodeFrameRefusesCountsAndSizesTheWireFormatCannotCarry`、`TestDecodeFrameRefusesHeaderCountsBeforeReadingOrAllocating`
- [receiver_test.go](../../../framework/sync/frame/receiver_test.go)：`TestReceiverGapRequiresFullAndIgnoresOldEpoch`、`TestReceiverApplicationFailureRequiresRecovery`、`TestReceiverPanicDoesNotLeaveUsableBaselineOrBusyReceiver`、`TestReceiverResetInsideCallbackCannotRestoreOldBaseline`、`TestReceiverRejectsStreamAndSchemaSwitchAndMalformedFrame`

### sync/lockstep

- [command_test.go](../../../framework/sync/lockstep/command_test.go)：`TestCommandCodecAndOwnership`、`TestTCPSenderRejectsShortWorkerBeforeResolverOrSocket`、`TestMatchCommandUsesCurrentAuthenticatedBinding`、`TestTCPSenderBothLanesUseSameRouteAndBudget`
- [e2e_gate_test.go](../../../framework/sync/lockstep/e2e_gate_test.go)：`TestLockstepEndToEndGate`、`TestE2EBotShutdownWaitsForInFlightOutput`
- [history_long_test.go](../../../framework/sync/lockstep/history_long_test.go)：`TestMatchLongRunCatchupAfterRollingTrim`、`TestHistoryExtremeRangeAndTrim`、`TestHistoryLongRollingWindow`、`TestHistoryGradualShrinkReleasesBackingArray`、`TestMatchDefaultRetentionBoundsLongMatch`、`TestMatchAutoRetentionOvertakesStalledCatchup`、`TestMatchRetainFullHistoryKeepsEveryFrame`、`TestMatchHistoryRetainFramesValidation`
- [lockstep_test.go](../../../framework/sync/lockstep/lockstep_test.go)：`TestSequencerOptimisticLockingAndLateFolding`、`TestSequencerDeterministicFrameBytes`、`TestRedundancySurvivesPacketLoss`、`TestBroadcastCodecRoundTripAndFailFast`、`TestHistoryCatchupPagingAndSequenceGuard`；其余 13 项见文件
- [match_test.go](../../../framework/sync/lockstep/match_test.go)：`TestMatchTickBroadcastsRedundantFrames`、`TestMatchCatchupPagesHistoryThenGoesLive`、`TestMatchCatchupRequiresReliableLane`、`TestMatchDesyncVerdictSetSemantics`、`TestMatchTickSurvivesDeadSession`；其余 16 项见文件
- [submit_input_test.go](../../../framework/sync/lockstep/submit_input_test.go)：`TestSubmitInputPromiseReplayAfterCutIsIdempotent`、`TestSubmitInputPromiseOutOfOrderFutureFramesAreKept`、`TestSubmitInputPromiseExplicitInputStillOverridesFoldedPlaceholder`、`TestSubmitInputPromiseIdentityMemoryIsBoundedBetweenTicks`、`TestSubmitInputPromiseOutOfHorizonSubmissionDoesNotEvictAnIdentity`；其余 3 项见文件

### sync/nettransport

- [channel_test.go](../../../framework/sync/nettransport/channel_test.go)：`TestAsyncTransportReliableBackpressureKeepsAdmittedOrder`、`TestAsyncTransportReliableFailureIsTerminalAndHandlerPanicIsContained`、`TestAsyncTransportPreventsSessionIDReuseWhileOldSendDrains`、`TestTransportRejectsTypedNilDependencies`、`TestAsyncTransportCascadesSessionLifecycle`
- [governance_test.go](../../../framework/sync/nettransport/governance_test.go)：`TestConcurrentSessionsShareOneHardResidentBudget`、`TestGlobalResidentBudgetIncludesInFlightAndReleasesOnCancel`、`TestReliableAgeIncludesQueueWaitAndDoesNotSendExpiredSuffix`、`TestCloseDeadlineReleasesQueuedAndBusyBudget`、`TestRegisterSessionRefusesZeroDuplicateOverLimitAndClosed`；其余 4 项见文件
- [protocol_transport_test.go](../../../framework/sync/nettransport/protocol_transport_test.go)：`TestAEADSessionProtectorRejectsReplayAndTamper`、`TestAEADSessionProtectorNeverReusesNonceAfterSequenceExhaustion`、`TestUDPTransportEncryptedLoopback`、`TestQUICTransportDatagramAndReliableLoopback`、`TestKCPTransportOOBAndReliableLoopback`

### sync/syncbus

- [example_test.go](../../../framework/sync/syncbus/example_test.go)：`Example_reconcileAfterLoss`（有限投递兜底模板：序号缺口与周期对账触发快照替换，核对输出）
- [patch_syncer_test.go](../../../framework/sync/syncbus/patch_syncer_test.go)：`TestPatchSyncerPublishesAndAppliesRemotePatch`、`TestPatchSyncerRejectsMismatchedKey`、`TestPatchSyncerSkipsEmptyPatch`、`TestPatchSyncerRefusesIncompleteConfigurationAndZeroKeys`、`TestPatchSyncerStopWaitsForInFlightApply`
- [subscription_test.go](../../../framework/sync/syncbus/subscription_test.go)：`TestSubscriptionUnsubscribeStopContract`、`TestSubscriptionRefusesDeliveriesAfterUnsubscribe`、`TestSubscriptionSelfUnsubscribeWaitsOnlyForOthers`、`TestSubscriptionSelfUnsubscribeInReentrantDelivery`、`TestSubscriptionSelfUnsubscribeWithForeignContextTimesOut`；其余 3 项见文件

### sync/syncbus/driver

- [jetstream_contract_test.go](../../../framework/sync/syncbus/driver/jetstream_contract_test.go)：`TestDurableSyncNamePromiseDistinguishesPrefixes`、`TestDurableSyncNamePromiseKeepsTheDefaultPrefixNameStable`、`TestJetStreamSyncBusPromiseSameTopicSubscribersAllReceive`、`TestJetStreamSyncBusPromiseHandlerPanicIsIsolated`、`TestJetStreamSyncBusPromiseConcurrentFirstSubscribersShareOneConsumer`；其余 5 项见文件
- [jetstream_test.go](../../../framework/sync/syncbus/driver/jetstream_test.go)：`TestJetStreamSyncBusPublishesAndAcknowledgesHandlerError`、`TestJetStreamSyncBusSkipsSelfMessages`、`TestJetStreamSyncPublishHonorsCanceledContext`、`TestJetStreamSyncUnsubscribeRemovesTrackedSubscription`、`TestDurableSyncNameAvoidsSanitizationCollision`；其余 1 项见文件
- [lifecycle_test.go](../../../framework/sync/syncbus/driver/lifecycle_test.go)：`TestJetStreamSyncBusStopContract`、`TestUnsubscribeWaitsForInFlightHandler`、`TestUnsubscribeFromOwnHandlerDoesNotDeadlock`、`TestUnsubscribeStopContract`、`TestMirrorStopAndRestartWithOldDeliveryInFlight`
- [retirement_real_promises_test.go](../../../framework/sync/syncbus/driver/retirement_real_promises_test.go)：`TestRealRR25BoundedRetirement`、`TestRealRR25UnacknowledgedConnectionRecovery`
- [retirement_window_test.go](../../../framework/sync/syncbus/driver/retirement_window_test.go)：`TestLastUnsubscribeWindowDoesNotAcknowledgeUndelivered`、`TestRecreateWaitsForPreviousConsumerClosed`、`TestStoppingWindowKeepsDeliveryLimitAndBackoff`、`TestRR25HandlerOutcomeIsNotAdmission`、`TestRR25SelfUnsubscribeAndResubscribeDoesNotWaitForItself`；其余 5 项见文件

### sync/syncbus/mirror

- [envelope_test.go](../../../framework/sync/syncbus/mirror/envelope_test.go)：`TestReplicatorPublishesAndAppliesEnvelope`、`TestReplicatorRejectsForgedInnerIdentity`、`TestReplicatorHonorsCanceledPublishContext`、`TestPublishedMessagesCarryDistinctDeliveryIDs`、`TestReplicatorRefusesEachMalformedInboundMessage`；其余 3 项见文件
- [stop_test.go](../../../framework/sync/syncbus/mirror/stop_test.go)：`TestReplicatorStopContract`、`TestReplicatorStopWithContextWaitsForAdmittedHandlers`

### syncstream

- [adapter_test.go](../../../framework/sync/syncstream/adapter_test.go)：`TestPublisherAndSubscriberRoundTrip`、`TestSubscriberRejectsEnvelopeMismatch`、`TestEnqueueReportsPublishErrors`、`TestObserverAndPayloadGuards`、`TestBufferedPublisherSignalsBackpressureAndDrains`；其余 5 项见文件
- [history_contract_test.go](../../../framework/sync/syncstream/history_contract_test.go)：`TestHistoryPromiseRecreatedStreamDoesNotReuseSequences`、`TestHistoryPromiseRecreatedStreamSurvivesJournalReplay`、`TestHistoryOpensWhenTheFirstWALRecordIsTornBeforeAnyCheckpoint`、`TestHistoryReplayRestoresLastActivity`、`TestRecoverKeepsTheProvidersSchemaVersion`；其余 7 项见文件
- [journal_test.go](../../../framework/sync/syncstream/journal_test.go)：`TestFileHistoryJournalLoadFailsClosedOnEachIntegrityDefect`、`TestFileHistoryJournalRefusesRecordsAfterCloseAndFromOtherVersions`、`TestJournalPromiseIndeterminateSyncStopsTheJournal`、`TestJournalPromiseIndeterminatePublishStopsTheJournal`、`TestJournalPromisePreSideEffectFailureIsRetryable`；其余 4 项见文件
- [syncstream_test.go](../../../framework/sync/syncstream/syncstream_test.go)：`TestHistoryAppendReplayAndAck`、`TestHistoryDetectsGapSchemaMismatchAndClientAhead`、`TestFullPacketRepairsTruncatedHistory`、`TestObserverStreamsAreIsolated`、`TestRecoverAutomaticallyAppendsFullSnapshot`；其余 10 项见文件

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。没有新测量不能改容量承诺，性能结论以 [性能报告](../../performance/README.md) 为准。
