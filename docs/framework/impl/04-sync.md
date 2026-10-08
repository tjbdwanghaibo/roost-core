# 核心：Sync、Lockstep 与客户端：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/04-sync.md)

## 1. 实现边界

`sync`、`syncstream`、`gateway`、`client/wire`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 持久水位和 Guard 冻结共同约束状态可见性。
2. 客户端状态缺口恢复全量，不在失效 base 上继续应用。
3. Lockstep 席位来自认证 session；历史保留由宿主管理。
4. 有限投递、流保留与业务 ACK 语义不能等同 exactly-once。

## 2A. 冻结、发送和客户端权限实现走读

[mode.go](../../../sync/entitysync/mode.go)的CaptureSync先取得当前subject及授权profiles，确认对象仍有效，调用SubjectSyncState.FreezeSyncViews；替换后的旧state不能留下可发送冻结物。reserveFrozen以CAS计入总字节，返回一次性释放函数，避免重试/取消双重释放。WakeSync合并通知，丢一个唤醒不等于丢pending事实。

[flush.go](../../../sync/entitysync/flush.go)负责取pending、准备subject视图、按会话构建帧、传输准入后结算；capturedAbove检查CommitLSN水位，requireSnapshotsAfterRetry处理部分会话已经接纳后的恢复。必须在实际Push结果之后结算基线，不能先标clean再尝试发送。

[command.go](../../../sync/lockstep/command.go)的HandleCommand须在Room串行handler中运行：先拒绝closed/非法command，再查sessionOwners；旁观者只走SpectatorCatchup，普通玩家分别走StartCatchup/ReportHash/SubmitInput。没有seat字段让客户端覆盖服务端绑定，也没有把发送成功当模拟已执行的业务ACK。

[packet.go](../../../client/wire/packet.go)从flags解释Kind并在Header.Validate拒绝保留组合、超长载荷和非法心跳标记。头解码不解释业务payload，注册路由与Lockstep/Sync解码器仍各自校验。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

### client/wire

1 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [packet.go](../../../client/wire/packet.go) | `PayloadKind`、`Header`、`Packet` |

### gateway

2 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [gateway.go](../../../gateway/gateway.go) | `Principal`、`Session`、`Request`、`Endpoint`、`EndpointFunc`、`Middleware` |
| [middleware.go](../../../gateway/middleware.go) | 函数/方法或内部实现；见源码 |

### sync/entitysync

16 个实现文件、36 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [counters.go](../../../sync/entitysync/counters.go) | `ManagerCounters` |
| [drain.go](../../../sync/entitysync/drain.go) | 函数/方法或内部实现；见源码 |
| [errors.go](../../../sync/entitysync/errors.go) | 函数/方法或内部实现；见源码 |
| [flush.go](../../../sync/entitysync/flush.go) | 函数/方法或内部实现；见源码 |
| [manager.go](../../../sync/entitysync/manager.go) | `ManagerConfig`、`Manager`、`ManagerStats` |
| [mode.go](../../../sync/entitysync/mode.go) | `SyncMode` |
| [policy_queue.go](../../../sync/entitysync/policy_queue.go) | `RetractedSubscription`、`SubscriptionSourceHooks`、`ReleasedSubscription`、`SubscriptionStamp` |
| [session.go](../../../sync/entitysync/session.go) | 函数/方法或内部实现；见源码 |
| [snapshot_budget.go](../../../sync/entitysync/snapshot_budget.go) | `SnapshotBudget` |
| [snapshot_requests.go](../../../sync/entitysync/snapshot_requests.go) | 函数/方法或内部实现；见源码 |
| [subject.go](../../../sync/entitysync/subject.go) | 函数/方法或内部实现；见源码 |
| [subscriptions.go](../../../sync/entitysync/subscriptions.go) | `SubscriptionSource` |
| [trace.go](../../../sync/entitysync/trace.go) | `SyncTraceEvent`、`SyncTrace` |
| [transport.go](../../../sync/entitysync/transport.go) | `SessionID`、`Transport`、`TransportFunc`、`SessionLifecycle`、`AsyncTransport`、`FrameSizeLimiter` |
| [wire.go](../../../sync/entitysync/wire.go) | 函数/方法或内部实现；见源码 |
| [workspace.go](../../../sync/entitysync/workspace.go) | 函数/方法或内部实现；见源码 |

### sync/entitysync/policy

8 个实现文件、18 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [aoi.go](../../../sync/entitysync/policy/aoi.go) | `InterestEventKind`、`InterestEvent`、`AOIConfig`、`AOI` |
| [aoi_cluster.go](../../../sync/entitysync/policy/aoi_cluster.go) | `AreaID`、`AOICluster` |
| [direct.go](../../../sync/entitysync/policy/direct.go) | `Direct` |
| [group.go](../../../sync/entitysync/policy/group.go) | `GroupConfig`、`Group` |
| [interest.go](../../../sync/entitysync/policy/interest.go) | `InterestConfig`、`Refusal`、`Interest` |
| [interest_queue.go](../../../sync/entitysync/policy/interest_queue.go) | 函数/方法或内部实现；见源码 |
| [profiles.go](../../../sync/entitysync/policy/profiles.go) | 函数/方法或内部实现；见源码 |
| [source.go](../../../sync/entitysync/policy/source.go) | `Source`、`RelationSource` |

### sync/frame

3 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [codec.go](../../../sync/frame/codec.go) | 函数/方法或内部实现；见源码 |
| [frame_limits.go](../../../sync/frame/frame_limits.go) | `Limits`、`ObjectRef`、`SnapshotMeta`、`Kind`、`ObjectOperation`、`ComponentOperation`、`ComponentDelta`、`ObjectDelta`、`Frame` |
| [receiver.go](../../../sync/frame/receiver.go) | `Receiver` |

### sync/lockstep

8 个实现文件、12 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assembler.go](../../../sync/lockstep/assembler.go) | `FrameAssembler` |
| [command.go](../../../sync/lockstep/command.go) | `Operation`、`Command` |
| [desync.go](../../../sync/lockstep/desync.go) | `DesyncDetector`、`DesyncVerdict` |
| [history.go](../../../sync/lockstep/history.go) | `History` |
| [room.go](../../../sync/lockstep/room.go) | `RoomConfig`、`Room` |
| [sequencer.go](../../../sync/lockstep/sequencer.go) | `PlayerID`、`FrameID`、`Input`、`Frame`、`SequencerConfig`、`Sequencer` |
| [tcp.go](../../../sync/lockstep/tcp.go) | `TCPSender` |
| [wire.go](../../../sync/lockstep/wire.go) | `RedundantEncoder` |

### sync/nettransport

9 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [channel.go](../../../sync/nettransport/channel.go) | `SendError`、`ErrorHandler`、`AsyncTransportConfig`、`AsyncTransport`、`AsyncTransportStats` |
| [counters.go](../../../sync/nettransport/counters.go) | `AsyncTransportCounters` |
| [kcp_transport.go](../../../sync/nettransport/kcp_transport.go) | `KCPTransportConfig`、`KCPTransport`、`KCPTransportStats`、`KCPDatagramHandler` |
| [quic_transport.go](../../../sync/nettransport/quic_transport.go) | `QUICTransportConfig`、`QUICTransport`、`QUICTransportStats` |
| [sender.go](../../../sync/nettransport/sender.go) | `DatagramSender`、`DatagramPayloadLimiter`、`DatagramBatchSender`、`ReliableSender`、`CompositeTransport` |
| [session.go](../../../sync/nettransport/session.go) | `SessionID`、`SessionInfo` |
| [transport.go](../../../sync/nettransport/transport.go) | `Transport`、`DatagramBatchTransport`、`SessionTransport`、`TransportFunc` |
| [udp_crypto.go](../../../sync/nettransport/udp_crypto.go) | `AEADSessionProtector` |
| [udp_transport.go](../../../sync/nettransport/udp_transport.go) | `UDPTransportConfig`、`UDPTransport`、`UDPTransportStats`、`UDPReceiveHandler` |

### sync/syncbus

4 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [delivery_id.go](../../../sync/syncbus/delivery_id.go) | `DeliveryIDs` |
| [patch_syncer.go](../../../sync/syncbus/patch_syncer.go) | `PatchSyncerConfig`、`PatchSyncer` |
| [subscription.go](../../../sync/syncbus/subscription.go) | `Subscription` |
| [sync.go](../../../sync/syncbus/sync.go) | `SyncMsg`、`Handler`、`IPublisher`、`IContextPublisher`、`ISubscriber`、`ILiveSubscriber`、`ISyncBus` |

### sync/syncbus/driver

2 个实现文件、13 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [jetstream.go](../../../sync/syncbus/driver/jetstream.go) | `JetStreamSyncConfig` |
| [nats.go](../../../sync/syncbus/driver/nats.go) | 函数/方法或内部实现；见源码 |

### sync/syncbus/mirror

1 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [envelope.go](../../../sync/syncbus/mirror/envelope.go) | `Op`、`Envelope`、`Store`、`Replicator` |

### syncstream

8 个实现文件、13 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [file_durability_unix.go](../../../syncstream/file_durability_unix.go) | 函数/方法或内部实现；见源码 |
| [file_durability_windows.go](../../../syncstream/file_durability_windows.go) | 函数/方法或内部实现；见源码 |
| [file_journal.go](../../../syncstream/file_journal.go) | `FileHistoryJournal` |
| [lifecycle_journal.go](../../../syncstream/lifecycle_journal.go) | `HistoryMutationKind`、`HistoryMutation`、`HistoryTarget`、`HistoryJournal` |
| [observability.go](../../../syncstream/observability.go) | `MetricSink`、`HealthOptions`、`HealthStatus` |
| [observability_impl.go](../../../syncstream/observability_impl.go) | `PublisherHealthOptions`、`PublisherHealthStatus` |
| [publisher.go](../../../syncstream/publisher.go) | `ErrorHandler`、`ConfirmedSyncPublisher`、`Publisher`、`PublisherOptions`、`Handler`、`SubscribeOptions`、`PublisherMetrics`、`PacketPublisher`、`BufferedPublisherOptions`、`BufferedPublisherMetrics`、`BufferedPublisher` |
| [syncstream.go](../../../syncstream/syncstream.go) | `Observer`、`Stream`、`Packet`、`Sink`、`BatchSink`、`HistoryOptions`、`History`、`ResyncReason`、`ResyncRequest`、`ResyncResult`、`SnapshotProvider`、`StreamStatus`、`HistoryMetrics`、`HistorySnapshot`、`HistoryStreamSnapshot`、`HistoryStore` |

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Lockstep`、`Test.*Watermark`、`Test.*Session`、`Test.*Retry`。

### client/wire

- [golden_test.go](../../../client/wire/golden_test.go)：`TestClientProtocolGolden`
- [packet_test.go](../../../client/wire/packet_test.go)：`TestPacketKindsAndGolden`、`TestPacketRefusesBadHeaderBeforeReadingPayload`

### gateway

- [gateway_test.go](../../../gateway/gateway_test.go)：`TestChainOrderAndAuthentication`
- [middleware_test.go](../../../gateway/middleware_test.go)：`TestRateLimitByPlayerAndMessage`、`TestRateLimitBackstopHoldsWithoutLimiter`、`TestTimeoutAndRecover`
- [ratelimit_owner_promises_test.go](../../../gateway/ratelimit_owner_promises_test.go)：`TestOnePlayerSprayingMessageIDsDoesNotRateLimitOthers`
- [recover_promises_test.go](../../../gateway/recover_promises_test.go)：`TestRecoverIsolatesReporterFailure`、`TestRecoverIsolatesDiagnosticFailure`、`TestRecoverPreservesNormalContract`

### sync/entitysync

- [async_load_test.go](../../../sync/entitysync/async_load_test.go)：`TestAsyncLoadSlowConsumerDoesNotDelayHealthyBaseline`
- [benchmark_test.go](../../../sync/entitysync/benchmark_test.go)
- [encoding_cache_test.go](../../../sync/entitysync/encoding_cache_test.go)：`TestTickEncodingSharesOnlyIdenticalCapturedContent`、`TestSharedEncodingKeepsSnapshotsDeltasAndProfilesSeparate`
- [failure_test.go](../../../sync/entitysync/failure_test.go)：`TestPolicyFailureRetainsCauseAndCountsOnce`、`TestCancelledFlushRetainsAdmissionCauseAndRecovers`
- [forget_notifier_test.go](../../../sync/entitysync/forget_notifier_test.go)：`TestRegistrationInTheForgetWindowKeepsItsDirtyNotifier`、`TestALateForgetOfTheOldSubjectLeavesTheNewOneAlone`
- [inflight_promises_test.go](../../../sync/entitysync/inflight_promises_test.go)：`TestInFlightDeliveryCannotOverwriteNewIntent`、`TestStopAndCloseRespectDeadlineWhileAnotherFlushIsInFlight`、`TestStopCancelsPushAndPreventsRestartUntilItReturns`、`TestCloseReleasesStateAfterFinalFlushError`
- [manager_promises_test.go](../../../sync/entitysync/manager_promises_test.go)：`TestEachSessionGetsASnapshotThenDeltasAndOnePackServesThemAll`、`TestAFailingSessionIsClosedAloneAndTheOthersStillReceive`、`TestRetryLaterAbandonsTheTickWithoutBlamingAnySession`、`TestUnsubscribeAndUnregisterOweARemoveOnlyToWhoHoldsTheObject`、`TestCloseSessionForgetsItsSubscriptionsWithoutAFrame`；其余 5 项见文件
- [mode_test.go](../../../sync/entitysync/mode_test.go)：`TestModeConfigurationRequiresProducerAndKeepsDefault`、`TestOnChangeSnapshotBudgetIsSharedAcrossFlushes`、`TestFrozenContentCoalescesAndRetainsBudgetUntilSettlement`、`TestFrozenCapacityFailureLeavesDirtyForRecovery`、`TestFrozenNewProfileRecapturesOneConsistentVersion`；其余 2 项见文件
- [optimization_test.go](../../../sync/entitysync/optimization_test.go)：`TestSessionReferenceCopiesAreIsolatedOnFirstWrite`、`TestExplicitProfilePriorityAndConfigurationOwnership`、`TestUnobservedCaptureStillRespectsDurabilityWatermark`、`TestHiddenFieldDeltaStillAdvancesTheClientVersion`
- [partial_retry_promises_test.go](../../../sync/entitysync/partial_retry_promises_test.go)：`TestPartialTickRetryRestoresAnApplicableSubjectBaseline`、`TestPartialSessionRetryContinuesAfterTheLastAdmittedFrame`
- [rebind_promises_test.go](../../../sync/entitysync/rebind_promises_test.go)：`TestReloadedSubjectIsRebound`、`TestRebindRefusesToReplaceALiveState`
- [reconnect_transport_test.go](../../../sync/entitysync/reconnect_transport_test.go)：`TestReopenRefusesDrainingTransportLifetime`、`TestConcurrentOpenResultMatchesSessionState`、`TestSessionInvisibleUntilTransportOpened`、`TestConcurrentReopenWhileDrainingIsRetryable`
- [recovery_benchmark_test.go](../../../sync/entitysync/recovery_benchmark_test.go)
- [recovery_fairness_test.go](../../../sync/entitysync/recovery_fairness_test.go)：`TestSnapshotArrivalAndRecoveryAlternate`、`TestSnapshotIdleClassBorrowsBudgetAndRotatesSessions`、`TestSnapshotClassesRotateAcrossSessions`
- [refresh_budget_test.go](../../../sync/entitysync/refresh_budget_test.go)：`TestExistingObjectRefreshDoesNotWaitForColdSnapshotBudget`、`TestExistingObjectRefreshLeavesColdQuotaAndHoldRestoresIt`
- [register_after_retirement_test.go](../../../sync/entitysync/register_after_retirement_test.go)：`TestRegisterAfterRetirementCompletesOnceTheRemoveIsOut`、`TestRegisterAfterRetirementCompletesWhenTheLastSubscriberCloses`、`TestAQueuedRegistrationIsBoundedAndCancellable`、`TestRegisterAfterRetirementIsRegisterWhenNothingIsRetiring`
- [release_stamp_promises_test.go](../../../sync/entitysync/release_stamp_promises_test.go)：`TestReleaseStampsSeparateTheReleasedLifetimeFromTheNextOne`
- [released_hook_promises_test.go](../../../sync/entitysync/released_hook_promises_test.go)：`TestReleasedHookReportsWhatTheFrameworkDropped`、`TestReleasesAreDeliveredBeforeResubmits`
- [retract_subject_promises_test.go](../../../sync/entitysync/retract_subject_promises_test.go)：`TestRetractSyncSubjectRemovesHeldObjectsBeforeRecreate`、`TestRetractSyncSubjectDoesNotRetireALaterRegistration`
- [retracted_again_promises_test.go](../../../sync/entitysync/retracted_again_promises_test.go)：`TestResubmitInterruptedByASecondRetractionIsHandedBackAgain`、`TestQueuedResubmitReturnsToTheRetractionTableOnce`
- [retracted_before_handback_test.go](../../../sync/entitysync/retracted_before_handback_test.go)：`TestRetractedBetweenRegisterAndHandBackIsHandedBackAfterTheNextRegistration`
- [retracted_resubmit_test.go](../../../sync/entitysync/retracted_resubmit_test.go)：`TestRetractedSubscriptionsWithoutAPolicyAreNotRestored`、`TestRetractedPolicySubscriptionsAreHandedBackOnceAfterReRegistration`、`TestReleasedOrUnregisteredRetractionsAreNotHandedBack`
- [scheduling_benchmark_test.go](../../../sync/entitysync/scheduling_benchmark_test.go)
- [scheduling_test.go](../../../sync/entitysync/scheduling_test.go)：`TestSnapshotWaitDoesNotRescanOrBlockLiveAndRemove`、`TestTraceIncludesEverySplitFrameAndByteBudgetResumes`、`TestRetiredSubjectCannotReenterSnapshotWait`、`TestProfileDemandCacheTracksIntentAndRetainsPublishedSlices`、`TestSyncTraceBoundedIndependentAndOptional`
- [session_capacity_promises_test.go](../../../sync/entitysync/session_capacity_promises_test.go)：`TestFullSessionCanReplaceAnObjectRegardlessOfSubjectID`
- [six_items_test.go](../../../sync/entitysync/six_items_test.go)：`TestProfileDowngradeReplacesClientFields`、`TestWholeEntityPacketsSplitAndRetainIndependentFrameState`、`TestWholeEntityTooLargeIsExplicitAndNeverTruncated`、`TestWholeEntitySplitRetryContinuesFrameAndContentBaselines`、`TestSnapshotBudgetRotatesAndDoesNotDelayLiveUpdates`；其余 2 项见文件
- [snapshot_attempt_test.go](../../../sync/entitysync/snapshot_attempt_test.go)：`TestSnapshotRetryOnlyChargesAttemptedSessions`、`TestSnapshotBudgetDoesNotChargeUnattemptedSessions`、`TestSnapshotRetryWithRotatedCursorDoesNotSkipUnattempted`、`TestSnapshotCursorStopsBeforeUnattemptedAcrossClasses`、`TestSnapshotAttemptBudgetPreservesSuccessfulFramePrefix`
- [snapshot_progress_test.go](../../../sync/entitysync/snapshot_progress_test.go)：`TestLargeSnapshotFiniteBacklog`、`TestLargeSnapshotStarvedUnderChurn`、`TestLargeSnapshotSingleSession`、`TestByteBlockedWindowDoesNotRecaptureUntilNextWindow`、`TestByteBlockedSnapshotsAreNotRecapturedEveryWindow`；其余 2 项见文件
- [snapshot_requests_test.go](../../../sync/entitysync/snapshot_requests_test.go)：`TestSnapshotIndexAndStatsFollowLifecycle`、`TestSnapshotPlanningAndStatsDoNotAcquireSubjectLocks`
- [snapshot_retry_progress_test.go](../../../sync/entitysync/snapshot_retry_progress_test.go)：`TestEverySessionIsDeliveredUnderPersistentRetryLater`
- [snapshot_window_test.go](../../../sync/entitysync/snapshot_window_test.go)：`TestSnapshotWindowDoesNotDriftOrAccumulate`
- [source_promises_test.go](../../../sync/entitysync/source_promises_test.go)：`TestSourcesSelectOneProfileAndReleaseIndependently`、`TestProfilePriorityHasDeterministicTieBreakers`、`TestRetirementCannotBeUndoneByReleasingOneSource`
- [subscription_index_test.go](../../../sync/entitysync/subscription_index_test.go)：`TestSubscriptionIndexTracksAllRemovalPaths`、`TestSessionRecoveryDoesNotLockUnsubscribedSubjects`、`TestSubscriptionIndexConcurrentLifecycle`、`TestFlushDoesNotSendOldSubscriptionToReopenedSession`
- [subscription_removal_promises_test.go](../../../sync/entitysync/subscription_removal_promises_test.go)：`TestPendingSnapshotStillRemovesAnAlreadyDeliveredObject`
- [unloaded_retract_test.go](../../../sync/entitysync/unloaded_retract_test.go)：`TestReloadDuringUnloadRetractionIsRegisteredAfterTheRemove`、`TestBusinessUnregisterStillRefusesRegistrationWhileRetiring`、`TestRegisterAfterRetirementDuringUnloadRetractionIsQueued`、`TestUnloadRetractionQueueLaterReplacesEarlier`
- [unregister_forgotten_test.go](../../../sync/entitysync/unregister_forgotten_test.go)：`TestUnregisterOfAForgottenSubjectDoesNotReleaseTheNextRegistration`、`TestUnregisterBetweenUnlinkAndForgottenRetiresTheCurrentRegistration`

### sync/entitysync/policy

- [aoi_budget_promises_test.go](../../../sync/entitysync/policy/aoi_budget_promises_test.go)：`TestObserverBlockBudgetRefusesARatioThatSubscribesTheWholeMap`、`TestObserverBlockBudgetIsConfigurable`、`TestTheNineBlockShapeFitsTheDefaultBudget`、`TestObserverBlockBudgetSaturatesInsteadOfWrapping`
- [aoi_guards_promises_test.go](../../../sync/entitysync/policy/aoi_guards_promises_test.go)：`TestInterestRefusesUnknownObserversAndZeroBlocks`
- [aoi_promises_test.go](../../../sync/entitysync/policy/aoi_promises_test.go)：`TestNewInterestManagerRefusesEachInvalidConfig`、`TestInterestManagerRefusesOutOfBoundsAndUnknownIDs`
- [aoi_test.go](../../../sync/entitysync/policy/aoi_test.go)：`TestInterestEnterLeaveWithHysteresis`、`TestInterestBandsAndObserverMovement`、`TestInterestMaxVisibleEvictsFarthest`、`TestInterestObserverIsAlsoSubjectSymmetry`、`TestInterestDeterministicEventStream`；其余 8 项见文件
- [direct_bindings_promises_test.go](../../../sync/entitysync/policy/direct_bindings_promises_test.go)：`TestDirectForgetsBindingsOfClosedSessionsAndUnregisteredSubjects`、`TestDirectKeepsABindingMadeAgainBeforeTheReleaseArrives`、`TestDirectDropsTheBindingOfARetractedSubjectWhenItsSessionCloses`
- [direct_release_lifetime_promises_test.go](../../../sync/entitysync/policy/direct_release_lifetime_promises_test.go)：`TestAStaleReleaseKeepsARebindingThatIsThenRetracted`
- [group_ownership_promises_test.go](../../../sync/entitysync/policy/group_ownership_promises_test.go)：`TestGroupAddFailureCanBeRetriedWithoutPartialMembership`、`TestPoliciesReleaseOnlyTheirOwnSubscriptions`
- [group_reconnect_promises_test.go](../../../sync/entitysync/policy/group_reconnect_promises_test.go)：`TestGroupRejoinRestoresReopenedSession`
- [interest_queue_retry_drain_promises_test.go](../../../sync/entitysync/policy/interest_queue_retry_drain_promises_test.go)：`TestQueuedRetryThatIsNeverAcceptedDoesNotHoldDrainOpen`
- [interest_queue_stale_promises_test.go](../../../sync/entitysync/policy/interest_queue_stale_promises_test.go)：`TestQueuedFactsInvalidatedByLeaveOrHideDoNotWedgeFlush`、`TestQueuedFactThatCannotApplyFailsOneFlushOnly`
- [interest_queue_test.go](../../../sync/entitysync/policy/interest_queue_test.go)：`TestQueuedInterestFactsFollowCommitAndRollback`
- [interest_remote_reject_test.go](../../../sync/entitysync/policy/interest_remote_reject_test.go)：`TestRemoteRejectKeepsCommittedLocalInterestFact`、`TestRejectEntitiesJudgesEntitiesWithoutSyncStateByID`、`TestPureRemoteRejectDiscardsItsInterestFacts`
- [optimization_test.go](../../../sync/entitysync/policy/optimization_test.go)：`TestFarthestTieKeepsDeterministicEviction`、`TestInterestSelectsSourceViewsBeforeComparingPriority`
- [policy_promises_test.go](../../../sync/entitysync/policy/policy_promises_test.go)：`TestEnteringSubscribesToYourself`、`TestDistanceSubscribesReleasesAndDoesNotChurnOnTheBoundary`、`TestARelationKeepsASubscriptionDistanceDropped`、`TestLeavingReleasesBothDirections`、`TestARefusedSubscribeIsSaidAgainUntilItIsTaken`；其余 4 项见文件
- [profiles_test.go](../../../sync/entitysync/policy/profiles_test.go)：`TestProfileAssemblyChecksFallbackSchemaAndPriority`、`TestProfileAssemblyCopiesSetsAndRejectsDynamicUnknownView`
- [retracted_again_promises_test.go](../../../sync/entitysync/policy/retracted_again_promises_test.go)：`TestGroupResubmitsAfterASecondRetractionBeforeDelivery`、`TestDirectResubmitsAfterASecondRetractionBeforeDelivery`、`TestInterestStillRecoversAfterASecondRetractionBeforeDelivery`、`TestResubmitRacesLeaveAndFlush`
- [retracted_resubmit_test.go](../../../sync/entitysync/policy/retracted_resubmit_test.go)：`TestInterestResubmitsASubscriptionTheFrameworkRetracted`、`TestInterestDoesNotResubmitAPairItReleasedWhileTheSubjectWasAway`、`TestEachPolicyResubmitsItsOwnRetractedSubscription`、`TestGroupAndDirectResubmitAfterRetraction`
- [unload_rebind_resubmit_test.go](../../../sync/entitysync/policy/unload_rebind_resubmit_test.go)：`TestARetractedSubjectRebindBeforeTheRemovesComesBackToInterestObservers`、`TestARetractedSubjectRebindAfterTheRemovesIsNotRegistered`

### sync/frame

- [benchmark_test.go](../../../sync/frame/benchmark_test.go)
- [codec_limits_promises_test.go](../../../sync/frame/codec_limits_promises_test.go)：`TestEncodeFrameRefusesCountsAndSizesTheWireFormatCannotCarry`、`TestDecodeFrameRefusesHeaderCountsBeforeReadingOrAllocating`
- [codec_promises_test.go](../../../sync/frame/codec_promises_test.go)：`TestEncodeFrameRefusesEachMalformedDelta`、`TestFrameCodecEnforcesLimitsAndRefusesCorruptBytes`
- [receiver_test.go](../../../sync/frame/receiver_test.go)：`TestReceiverGapRequiresFullAndIgnoresOldEpoch`、`TestReceiverApplicationFailureRequiresRecovery`、`TestReceiverPanicDoesNotLeaveUsableBaselineOrBusyReceiver`、`TestReceiverResetInsideCallbackCannotRestoreOldBaseline`、`TestReceiverRejectsStreamAndSchemaSwitchAndMalformedFrame`

### sync/lockstep

- [catchup_rate_promises_test.go](../../../sync/lockstep/catchup_rate_promises_test.go)：`TestRoomPromiseCatchupBatchMustOutpaceFrameProduction`
- [command_test.go](../../../sync/lockstep/command_test.go)：`TestCommandCodecAndOwnership`、`TestTCPSenderRejectsFastWorkerBeforeResolverOrSocket`、`TestRoomCommandUsesCurrentAuthenticatedBinding`、`TestTCPSenderBothLanesUseSameRouteAndBudget`
- [e2e_gate_test.go](../../../sync/lockstep/e2e_gate_test.go)：`TestLockstepEndToEndGate`
- [e2e_shutdown_promises_test.go](../../../sync/lockstep/e2e_shutdown_promises_test.go)：`TestE2EBotShutdownWaitsForInFlightOutput`
- [guards_promises_test.go](../../../sync/lockstep/guards_promises_test.go)：`TestRoomAndSequencerRefuseInvalidConfiguration`、`TestSpectatorSessionsAreExclusiveAndMustBeAttachedToCatchUp`、`TestClosedRoomRefusesSpectatorsAndHashReports`
- [input_replay_promises_test.go](../../../sync/lockstep/input_replay_promises_test.go)：`TestSubmitInputPromiseReplayAfterCutIsIdempotent`、`TestSubmitInputPromiseOutOfOrderFutureFramesAreKept`、`TestSubmitInputPromiseExplicitInputStillOverridesFoldedPlaceholder`
- [lockstep_test.go](../../../sync/lockstep/lockstep_test.go)：`TestSequencerOptimisticLockingAndLateFolding`、`TestSequencerDeterministicFrameBytes`、`TestRedundancySurvivesPacketLoss`、`TestBroadcastCodecRoundTripAndFailFast`、`TestHistoryCatchupPagingAndSequenceGuard`；其余 7 项见文件
- [replay_window_promises_test.go](../../../sync/lockstep/replay_window_promises_test.go)：`TestSubmitInputPromiseIdentityMemoryIsBoundedBetweenTicks`、`TestSubmitInputPromiseOutOfHorizonSubmissionDoesNotEvictAnIdentity`、`TestSubmitInputPromiseOutOfHorizonReplayStillFolds`
- [room_risk_promises_test.go](../../../sync/lockstep/room_risk_promises_test.go)：`TestRoomPromiseMetricNamesArePinned`、`TestRoomPromiseBudgetCountsTransportOverhead`、`TestRoomPromiseBudgetFollowsTheSender`、`TestRoomPromiseSlowCatchupDoesNotStallTick`、`TestRoomPromiseTwoSeatDesyncIsRuled`；其余 3 项见文件
- [room_test.go](../../../sync/lockstep/room_test.go)：`TestRoomTickBroadcastsRedundantFrames`、`TestRoomCatchupPagesHistoryThenGoesLive`、`TestRoomCatchupRequiresReliableLane`、`TestRoomDesyncVerdictSetSemantics`、`TestRoomTickSurvivesDeadSession`；其余 6 项见文件
- [seat_identity_promises_test.go](../../../sync/lockstep/seat_identity_promises_test.go)：`TestRoomPromiseNegativeSeatIsRefused`、`TestRoomPromiseSeatCountIsBoundByTheWireDecoder`
- [submit_validation_promises_test.go](../../../sync/lockstep/submit_validation_promises_test.go)：`TestSubmitInputPromiseRejectedFrameNeverTouchesTheIdentityRing`、`TestReplaySlotIndexPromiseStaysInRangeForEveryFrameID`

### sync/nettransport

- [admission_promises_test.go](../../../sync/nettransport/admission_promises_test.go)：`TestRegisterSessionRefusesZeroDuplicateOverLimitAndClosed`、`TestSendReliableRefusesEachMalformedMessage`
- [channel_test.go](../../../sync/nettransport/channel_test.go)：`TestAsyncTransportReliableBackpressureKeepsAdmittedOrder`、`TestAsyncTransportReliableFailureIsTerminalAndHandlerPanicIsContained`、`TestAsyncTransportPreventsSessionIDReuseWhileOldSendDrains`、`TestTransportRejectsTypedNilDependencies`、`TestAsyncTransportCascadesSessionLifecycle`
- [governance_test.go](../../../sync/nettransport/governance_test.go)：`TestConcurrentSessionsShareOneHardResidentBudget`、`TestGlobalResidentBudgetIncludesInFlightAndReleasesOnCancel`、`TestReliableAgeIncludesQueueWaitAndDoesNotSendExpiredSuffix`、`TestCloseDeadlineReleasesQueuedAndBusyBudget`
- [protocol_transport_test.go](../../../sync/nettransport/protocol_transport_test.go)：`TestAEADSessionProtectorRejectsReplayAndTamper`、`TestAEADSessionProtectorNeverReusesNonceAfterSequenceExhaustion`、`TestUDPTransportEncryptedLoopback`、`TestQUICTransportDatagramAndReliableLoopback`、`TestKCPTransportOOBAndReliableLoopback`
- [queue_budget_test.go](../../../sync/nettransport/queue_budget_test.go)：`TestReliableQueueByteBudgetAndAge`、`TestQueueReusesStorageWithoutReordering`
- [session_state_promises_test.go](../../../sync/nettransport/session_state_promises_test.go)：`TestSendReliableRefusesUnregisteredDrainingAndFailedSessions`

### sync/syncbus

- [delivery_id_test.go](../../../sync/syncbus/delivery_id_test.go)：`TestDeliveryIDsAreUniquePerMinterAndMonotonic`、`TestDeliveryIDsAreSafeForConcurrentUse`
- [patch_syncer_guards_promises_test.go](../../../sync/syncbus/patch_syncer_guards_promises_test.go)：`TestPatchSyncerRefusesIncompleteConfigurationAndZeroKeys`
- [patch_syncer_stop_promises_test.go](../../../sync/syncbus/patch_syncer_stop_promises_test.go)：`TestPatchSyncerStopWaitsForInFlightApply`
- [patch_syncer_test.go](../../../sync/syncbus/patch_syncer_test.go)：`TestPatchSyncerPublishesAndAppliesRemotePatch`、`TestPatchSyncerRejectsMismatchedKey`、`TestPatchSyncerSkipsEmptyPatch`
- [subscription_promises_test.go](../../../sync/syncbus/subscription_promises_test.go)：`TestSubscriptionUnsubscribeStopContract`、`TestSubscriptionRefusesDeliveriesAfterUnsubscribe`、`TestSubscriptionSelfUnsubscribeWaitsOnlyForOthers`、`TestSubscriptionSelfUnsubscribeInReentrantDelivery`、`TestSubscriptionSelfUnsubscribeWithForeignContextTimesOut`；其余 1 项见文件

### sync/syncbus/driver

- [guards_promises_test.go](../../../sync/syncbus/driver/guards_promises_test.go)：`TestSyncBusesRefuseEachInvalidArgumentBeforeTheWire`
- [jetstream_durable_identity_promises_test.go](../../../sync/syncbus/driver/jetstream_durable_identity_promises_test.go)：`TestDurableSyncNamePromiseDistinguishesPrefixes`、`TestDurableSyncNamePromiseKeepsTheDefaultPrefixNameStable`
- [jetstream_fanout_promises_test.go](../../../sync/syncbus/driver/jetstream_fanout_promises_test.go)：`TestJetStreamSyncBusPromiseSameTopicSubscribersAllReceive`、`TestJetStreamSyncBusPromiseHandlerPanicIsIsolated`、`TestJetStreamSyncBusPromiseConcurrentFirstSubscribersShareOneConsumer`
- [jetstream_live_promises_test.go](../../../sync/syncbus/driver/jetstream_live_promises_test.go)：`TestJetStreamSubscribeLiveUsesASeparateDeliverNewDurable`
- [jetstream_stop_drain_promises_test.go](../../../sync/syncbus/driver/jetstream_stop_drain_promises_test.go)：`TestJetStreamSyncBusStopWaitsForAnInFlightHandler`
- [jetstream_stop_retry_promises_test.go](../../../sync/syncbus/driver/jetstream_stop_retry_promises_test.go)：`TestJetStreamSyncBusStopWithContextIsBoundedAndRetryable`
- [jetstream_stream_name_test.go](../../../sync/syncbus/driver/jetstream_stream_name_test.go)：`TestJetStreamSyncStreamFollowsThePrefix`
- [jetstream_test.go](../../../sync/syncbus/driver/jetstream_test.go)：`TestJetStreamSyncBusPublishesAndAcknowledgesHandlerError`、`TestJetStreamSyncBusSkipsSelfMessages`、`TestJetStreamSyncPublishHonorsCanceledContext`、`TestJetStreamSyncUnsubscribeRemovesTrackedSubscription`、`TestDurableSyncNameAvoidsSanitizationCollision`；其余 1 项见文件
- [mirror_lifecycle_promises_test.go](../../../sync/syncbus/driver/mirror_lifecycle_promises_test.go)：`TestMirrorStopAndRestartWithOldDeliveryInFlight`
- [retirement_real_promises_test.go](../../../sync/syncbus/driver/retirement_real_promises_test.go)：`TestRealRR25BoundedRetirement`、`TestRealRR25UnacknowledgedConnectionRecovery`
- [retirement_window_promises_test.go](../../../sync/syncbus/driver/retirement_window_promises_test.go)：`TestLastUnsubscribeWindowDoesNotAcknowledgeUndelivered`、`TestRecreateWaitsForPreviousConsumerClosed`、`TestStoppingWindowKeepsDeliveryLimitAndBackoff`、`TestRR25HandlerOutcomeIsNotAdmission`、`TestRR25SelfUnsubscribeAndResubscribeDoesNotWaitForItself`；其余 5 项见文件
- [stop_contract_test.go](../../../sync/syncbus/driver/stop_contract_test.go)：`TestJetStreamSyncBusStopContract`
- [unsubscribe_drain_promises_test.go](../../../sync/syncbus/driver/unsubscribe_drain_promises_test.go)：`TestUnsubscribeWaitsForInFlightHandler`、`TestUnsubscribeFromOwnHandlerDoesNotDeadlock`、`TestUnsubscribeStopContract`

### sync/syncbus/mirror

- [envelope_test.go](../../../sync/syncbus/mirror/envelope_test.go)：`TestReplicatorPublishesAndAppliesEnvelope`、`TestReplicatorRejectsForgedInnerIdentity`、`TestReplicatorHonorsCanceledPublishContext`、`TestPublishedMessagesCarryDistinctDeliveryIDs`
- [guards_promises_test.go](../../../sync/syncbus/mirror/guards_promises_test.go)：`TestReplicatorRefusesNilUninitialisedAndMalformedPublishes`
- [promises_test.go](../../../sync/syncbus/mirror/promises_test.go)：`TestReplicatorRefusesEachMalformedInboundMessage`、`TestReplicatorFillsInnerIdentityFromTheOuterMessage`、`TestReplicatorPublishRefusesEachMalformedEnvelope`
- [stop_contract_test.go](../../../sync/syncbus/mirror/stop_contract_test.go)：`TestReplicatorStopContract`
- [stop_drain_promises_test.go](../../../sync/syncbus/mirror/stop_drain_promises_test.go)：`TestReplicatorStopWithContextWaitsForAdmittedHandlers`

### syncstream

- [adapter_test.go](../../../syncstream/adapter_test.go)：`TestPublisherAndSubscriberRoundTrip`、`TestSubscriberRejectsEnvelopeMismatch`、`TestEnqueueReportsPublishErrors`、`TestObserverAndPayloadGuards`、`TestBufferedPublisherSignalsBackpressureAndDrains`；其余 4 项见文件
- [file_journal_promises_test.go](../../../syncstream/file_journal_promises_test.go)：`TestFileHistoryJournalLoadFailsClosedOnEachIntegrityDefect`、`TestFileHistoryJournalRefusesRecordsAfterCloseAndFromOtherVersions`
- [history_identity_promises_test.go](../../../syncstream/history_identity_promises_test.go)：`TestHistoryPromiseRecreatedStreamDoesNotReuseSequences`、`TestHistoryPromiseRecreatedStreamSurvivesJournalReplay`
- [history_restart_promises_test.go](../../../syncstream/history_restart_promises_test.go)：`TestHistoryOpensWhenTheFirstWALRecordIsTornBeforeAnyCheckpoint`、`TestHistoryReplayRestoresLastActivity`、`TestRecoverKeepsTheProvidersSchemaVersion`
- [import_durability_promises_test.go](../../../syncstream/import_durability_promises_test.go)：`TestHistoryPromiseImportOnAJournaledHistoryIsDurable`
- [import_guards_promises_test.go](../../../syncstream/import_guards_promises_test.go)：`TestImportRefusesEveryInconsistentSnapshotAndLeavesTheHistoryUntouched`、`TestHistoryEntryPointsRefuseMissingOrMismatchedInputs`、`TestBufferedPublisherRefusesWithoutAPublisherAndAfterClose`、`TestSubscriberRefusesAnEnvelopeWithTrailingContent`
- [journal_failstop_promises_test.go](../../../syncstream/journal_failstop_promises_test.go)：`TestJournalPromiseIndeterminateSyncStopsTheJournal`、`TestJournalPromiseIndeterminatePublishStopsTheJournal`、`TestJournalPromisePreSideEffectFailureIsRetryable`
- [recover_replacement_promises_test.go](../../../syncstream/recover_replacement_promises_test.go)：`TestRecoverPromiseRejectsReplacementAtTheSamePosition`
- [recover_validation_promises_test.go](../../../syncstream/recover_validation_promises_test.go)：`TestRecoverPromiseDoesNotCommitAStaleCapture`
- [subscribe_promises_test.go](../../../syncstream/subscribe_promises_test.go)：`TestSubscribeRefusesEachOversizedOrMalformedEnvelope`
- [syncstream_test.go](../../../syncstream/syncstream_test.go)：`TestHistoryAppendReplayAndAck`、`TestHistoryDetectsGapSchemaMismatchAndClientAhead`、`TestFullPacketRepairsTruncatedHistory`、`TestObserverStreamsAreIsolated`、`TestRecoverAutomaticallyAppendsFullSnapshot`；其余 10 项见文件
- [wal_replay_promises_test.go](../../../syncstream/wal_replay_promises_test.go)：`TestWALReplayRefusesMutationsThatDoNotContinueTheCheckpoint`、`TestEntryPointsRefuseMissingDependencies`
- [wal_tail_truncate_promises_test.go](../../../syncstream/wal_tail_truncate_promises_test.go)：`TestJournalPromiseTruncatesAnUnterminatedTailBeforeAppending`、`TestJournalPromiseStillRejectsACorruptCompleteLine`

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../maintenance/PERFORMANCE.md)，没有新测量则不能改容量承诺。
