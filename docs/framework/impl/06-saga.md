# Saga 长事务：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/06-saga.md)

## 如何阅读

有些操作需要多个服务分几步完成，例如先扣道具，再创建跨服活动记录。Saga记录这些步骤，并在失败时按业务定义执行补偿。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`framework/saga`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 步骤转换集中且持久终态由完整步骤集合决定。
2. native inbox 与 Mongo step inbox 各自和业务提交原子绑定。
3. 旧 incarnation 回执不能推进新执行；重试不重置总预算。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### saga

14 个实现文件、62 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assembly.go](../../../framework/saga/assembly.go) | `AssemblyConfig`、`Assembly` |
| [command_consumer.go](../../../framework/saga/command_consumer.go) | `StepHandler`、`MongoCommandInbox`、`CommandInboxOptions`、`StepConsumerConfig` |
| [dataengine_step_inbox.go](../../../framework/saga/dataengine_step_inbox.go) | `DataEngineStepInboxOptions`、`DataEngineStepInbox` |
| [engine.go](../../../framework/saga/engine.go) | `Options`、`StartRequest`、`ResumeRequest`、`Stats`、`Engine` |
| [errors.go](../../../framework/saga/errors.go) | 函数/方法或内部实现；见源码 |
| [jetstream.go](../../../framework/saga/jetstream.go) | `JetStreamPublisher`、`CompletionConsumerConfig` |
| [mongo_store.go](../../../framework/saga/mongo_store.go) | `MongoStoreOptions`、`MongoStore` |
| [nest.go](../../../framework/saga/nest.go) | 函数/方法或内部实现；见源码 |
| [nest_completion_consumer.go](../../../framework/saga/nest_completion_consumer.go) | `NestCompletionConsumerConfig`、`Completer` |
| [nest_start_consumer.go](../../../framework/saga/nest_start_consumer.go) | `NestStartConsumerConfig`、`Starter` |
| [record.go](../../../framework/saga/record.go) | `Status`、`Phase`、`Step`、`StepBudget`、`StepKey`、`StepBudgets`、`Definition`、`Lease`、`Record`、`Command`、`Completion`、`OutboxRecord` |
| [step_operation_inbox.go](../../../framework/saga/step_operation_inbox.go) | `Reservation` |
| [step_transition.go](../../../framework/saga/step_transition.go) | 函数/方法或内部实现；见源码 |
| [store.go](../../../framework/saga/store.go) | `ClaimRequest`、`Query`、`ApplyRequest`、`ApplyOutcome`、`Store`、`OperationClosure`、`CompletionHistory`、`CompletionHistoryStore`、`LateSuccessAlarmStore`、`Publisher`、`PublishFunc` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Transition`、`Test.*Inbox`、`Test.*Incarnation`、`Test.*Budget`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### saga

- [assembly_health_promises_test.go](../../../framework/saga/assembly_health_promises_test.go)：`TestAssemblyHealthIncludesEveryRequiredConsumer`、`TestAssemblyNativeConsumerClosureIsVisibleAfterFormalStart`
- [assembly_shutdown_review_test.go](../../../framework/saga/assembly_shutdown_review_test.go)：`TestSagaPartialSubscriptionsCleanUpAndRetry`、`TestSagaThirdConsumerDrainTimeoutCanFinishOnRetry`
- [assembly_test.go](../../../framework/saga/assembly_test.go)：`TestDrainSubscriptionsWaitsForConsumerClosure`、`TestAssembleBuildsEngineAndRegistersDefinitions`
- [command_consumer_test.go](../../../framework/saga/command_consumer_test.go)：`TestMongoCommandInboxExecutesStepOnceForMessageRedelivery`、`TestMongoCommandInboxAllowsNewSagaAttempt`、`TestMongoCommandInboxRejectsCommandIDReuse`、`TestStorageDigestsUseStableOperationIdentity`
- [compensate_completed_promises_test.go](../../../framework/saga/compensate_completed_promises_test.go)：`TestManualCompensateRefusesACompletedSaga`
- [compensation_version_promises_test.go](../../../framework/saga/compensation_version_promises_test.go)：`TestStepRefusalMovesSagaIntoCompensationOnMongoStore`、`TestRetryOrCompensateAdvancesVersionByOne`、`TestApplyCompletionAdvancesVersionByOne`
- [completion_contract_test.go](../../../framework/saga/completion_contract_test.go)：`TestCompletionConsumersTermTheSameTerminalErrors`、`TestACompletionBeforeItsDefinitionIsRegisteredIsRetriedNotTerminated`、`TestADefinitionThatNeverArrivesEndsInTheCoordinatorFence`
- [completion_transition_test.go](../../../framework/saga/completion_transition_test.go)：`TestJudgeCompletionIsTheUnifiedRule`
- [consumer_bad_envelope_promises_test.go](../../../framework/saga/consumer_bad_envelope_promises_test.go)：`TestSagaConsumersTermEveryBadEnvelopeAndAlarm`
- [consumer_nak_maxdeliver_real_integration_test.go](../../../framework/saga/consumer_nak_maxdeliver_real_integration_test.go)：`TestRealNatsCompletionNakBackoffAndMaxDeliver`
- [coordinator_takeover_review_test.go](../../../framework/saga/coordinator_takeover_review_test.go)：`TestCoordinatorLeaseTakeoverFencesTheLateApply`、`TestOutboxSupersedeAndUnknownAckOnMongoStore`
- [cross_process_real_integration_test.go](../../../framework/saga/cross_process_real_integration_test.go)：`TestRealMongoCoordinatorLeaseTakeover`、`TestRealSagaCrossProcessChild`、`TestRealSagaCrossProcessKillRecovers`
- [dataengine_step_benchmark_test.go](../../../framework/saga/dataengine_step_benchmark_test.go)
- [dataengine_step_inbox_mark_completed_promises_test.go](../../../framework/saga/dataengine_step_inbox_mark_completed_promises_test.go)：`TestReserveReportsSwallowedMarkCompletedFailure`
- [dataengine_step_inbox_real_mongo_integration_test.go](../../../framework/saga/dataengine_step_inbox_real_mongo_integration_test.go)：`TestRealMongoReserveDeterministicMarkCompletedFailureIsReportedNotSilent`
- [dataengine_step_inbox_test.go](../../../framework/saga/dataengine_step_inbox_test.go)：`TestDataEngineStepBindCarriesExplicitReservationFence`、`TestDataEngineStepBindRejectsReservationFromAnotherCommand`、`TestDataEngineStepInboxReservesCommandIdentityAndAllowsNewAttempt`、`TestDataEngineStepInboxReplaysAuthoritativeReceiptAndCompletesClaim`、`TestDataEngineStepInboxUsesAbsoluteOperationExpiry`；其余 1 项见文件
- [definition_fence_abandon_promises_test.go](../../../framework/saga/definition_fence_abandon_promises_test.go)：`TestDefinitionFenceDuringBackoffAbandonsTheOperation`
- [digest_promises_test.go](../../../framework/saga/digest_promises_test.go)：`TestCommandDigestReportsUnmarshalableCommands`、`TestMongoCommandInboxRefusesCommandsWhoseIdentityCannotBeDigested`、`TestCompletionDigestIsStableForMarshalableReceipts`
- [engine_promises_test.go](../../../framework/saga/engine_promises_test.go)：`TestEngineRegisterAndStartRefuseUnknownOrDuplicateDefinitions`、`TestEngineResumeRefusesEachIllegalRequest`、`TestEngineCompensateRefusesInFlightAndNothingToUndo`
- [engine_test.go](../../../framework/saga/engine_test.go)：`TestEngineCompletesForwardSteps`、`TestEngineCompensatesInReverseOrder`、`TestStartSagaIsIdempotentByBusinessKey`、`TestStartSagaRejectsBusinessKeyWithDifferentIntent`、`TestDefinitionVersionsRunSideBySideAndArePartOfIdentity`；其余 18 项见文件
- [fenced_entity_promises_test.go](../../../framework/saga/fenced_entity_promises_test.go)：`TestDataEngineStepHandsBackTheLeaseWhenTheEntityIsFenced`
- [guards_promises_test.go](../../../framework/saga/guards_promises_test.go)：`TestAssembleAndStepConsumerRefuseMissingPartsAndForeignEnvelopes`
- [late_alarm_real_mongo_integration_test.go](../../../framework/saga/late_alarm_real_mongo_integration_test.go)：`TestRealMongoLateSuccessAlarmIsMarkedOnce`
- [late_step_real_mongo_integration_test.go](../../../framework/saga/late_step_real_mongo_integration_test.go)：`TestRealMongoLateSuccessReopensACompensatedSagaOnce`
- [mongo_contract_test.go](../../../framework/saga/mongo_contract_test.go)：`TestMongoResumePersistsGenerationAndAcceptsFreshCompletion`、`TestMongoIncarnationSurvivesEveryRecordReadAndReplace`、`TestMongoStepConsumerFollowsTheOperationInbox`、`TestMongoStepAttemptsOfOneOperationTakeEffectOnce`、`TestMongoStoreSkipsACorruptRecordWithoutFailingTheBatch`
- [mongo_step_benchmark_real_mongo_integration_test.go](../../../framework/saga/mongo_step_benchmark_real_mongo_integration_test.go)
- [mongo_step_benchmark_test.go](../../../framework/saga/mongo_step_benchmark_test.go)
- [mongo_step_latency_real_mongo_integration_test.go](../../../framework/saga/mongo_step_latency_real_mongo_integration_test.go)
- [mongo_step_multiprocess_real_mongo_integration_test.go](../../../framework/saga/mongo_step_multiprocess_real_mongo_integration_test.go)：`TestStepProcessRole`、`TestRealMongoStepProcessesFenceAttemptsInFlightAcrossProcesses`、`TestRealMongoStepProcessesConcurrentAttemptsTakeEffectOnce`
- [mongo_step_operation_real_mongo_integration_test.go](../../../framework/saga/mongo_step_operation_real_mongo_integration_test.go)：`TestRealMongoStepAttemptsOfOneOperationTakeEffectOnce`、`TestRealMongoOperationAttemptsAccumulatedOverResumesDoNotBlockANewLife`
- [native_cancel_review_test.go](../../../framework/saga/native_cancel_review_test.go)：`TestNativeStepCancellationAndFenceRecovery`
- [nest_atomic_test.go](../../../framework/saga/nest_atomic_test.go)：`TestNativeSagaStepCommitsMutationReceiptAndCompletionEffectAtomically`
- [nest_contract_test.go](../../../framework/saga/nest_contract_test.go)：`TestNestCompletionRejectsForeignSagaRouteBeforeMutation`、`TestAssemblyConsumesNativeNestCompletionEffects`、`TestAssemblyKeepsItsExistingConsumerSubjects`
- [nest_start_consumer_test.go](../../../framework/saga/nest_start_consumer_test.go)：`TestSubscribeNestStartsUsesSharedDurableAndDecodesIntent`、`TestNestStartRejectsWrongEffectTopic`、`TestJetStreamPublisherUsesVersionedEnvelope`
- [outbox_backoff_promises_test.go](../../../framework/saga/outbox_backoff_promises_test.go)：`TestOutboxClaimRechecksDueAfterAnotherPublisher`
- [performance_real_integration_test.go](../../../framework/saga/performance_real_integration_test.go)：`TestRealSagaPerformance`
- [promises_impl_test.go](../../../framework/saga/promises_impl_test.go)：`TestSubscribeNestStartsRefusesEachUnsafeConfig`、`TestDecodeStepCommandRefusesOversizedForeignAndInvalidEnvelopes`、`TestHandleNestStartRefusesEachMalformedEnvelopePermanently`、`TestMongoCommandInboxRefusesUnrepresentableReceiptTTL`
- [promises_test.go](../../../framework/saga/promises_test.go)：`TestNewEngineRefusesEachUnsafeOption`、`TestCommandValidateRefusesEachBrokenField`、`TestCompletionValidateRefusesEachBrokenField`、`TestNestEffectCodecsRefuseEachMalformedPayload`
- [saga_contract_test.go](../../../framework/saga/saga_contract_test.go)：`TestNativeStepStaleAttemptRetryableFailureDoesNotAbandonTheLastAttempt`、`TestNativeStepReplayedRefusalOfAnEarlierAttemptIsAccepted`、`TestNativeStepLateSuccessReopensAFailedSagaToCompensateTheStep`、`TestNativeStepLateSuccessAfterCompensatedCompensatesOnlyThatStep`、`TestNativeStepLateSuccessDuringAnInFlightCompensationIsCompensatedNext`；其余 4 项见文件、`TestReopeningAFailedSagaIsCountedAndLogged`、`TestReopeningACompensatedSagaIsCountedAndLogged`、`TestLateSuccessDuringCompensationIsNotAReopen`、`TestResumeCompensatingALateStepIsCountedAndLogged`、`TestPlainResumeIsNotAReopen`；其余 1 项见文件
- [sagafix_real_mongo_integration_test.go](../../../framework/saga/sagafix_real_mongo_integration_test.go)：`TestRealMongoSuccessDuringBackoffRacesTheNextDispatch`、`TestRealMongoClaimDueAndListSkipACorruptRecord`
- [start_contract_test.go](../../../framework/saga/start_contract_test.go)：`TestStartIdentityCompatibilityAndForeignIntents`、`TestStartIdentitySurvivesProgressAndResume`、`TestHandleNestStartTermsDeterministicStartRefusalsAndAlarms`、`TestEmitStartRefusesDataTheCoordinatorWouldRefuse`
- [step_admission_promises_test.go](../../../framework/saga/step_admission_promises_test.go)：`TestStepConsumersAdmitBeforeTakingTheClaim`
- [step_consumer_promises_test.go](../../../framework/saga/step_consumer_promises_test.go)：`TestSubscribeMongoStepRefusesEachUnsafeConfig`、`TestSubscribeDataEngineStepRefusesEachUnsafeConfig`、`TestMongoCommandInboxReplayRefusesForeignReceipt`
- [step_expired_promises_test.go](../../../framework/saga/step_expired_promises_test.go)：`TestExpiredStepCommandWithoutReceiptIsAcknowledgedNotRedelivered`
- [step_operation_attempt_cap_promises_test.go](../../../framework/saga/step_operation_attempt_cap_promises_test.go)：`TestOperationAttemptsAccumulatedOverResumesDoNotBlockANewLife`
- [step_operation_benchmark_real_mongo_integration_test.go](../../../framework/saga/step_operation_benchmark_real_mongo_integration_test.go)
- [step_operation_incarnation_promises_test.go](../../../framework/saga/step_operation_incarnation_promises_test.go)：`TestCoordinatorChecksTheIncarnationOfACompletion`、`TestMongoStoreMarksALateSuccessAlarmOncePerLife`、`TestCommandIDIncarnationInvertsCommandID`
- [step_operation_promises_test.go](../../../framework/saga/step_operation_promises_test.go)：`TestNativeStepTakesEffectAtMostOncePerOperation`、`TestNativeStepOperationInterleavingsWithCoordinatorDecisions`、`TestNativeStepLeaseNeverOutlivesTheCommandDeadline`、`TestNativeStepConsumerHandlesOperationOutcomes`、`TestMongoStoreTombstoneTellsAbandonedFromResolved`
- [step_operation_real_mongo_integration_test.go](../../../framework/saga/step_operation_real_mongo_integration_test.go)：`TestRealMongoConcurrentAttemptsOfOneOperationReserveOnce`、`TestRealMongoSupersedeAndProjectionOfTheSameAttemptSerialize`、`TestRealMongoTakeoverFencesTheEarlierAttemptsProjection`
- [step_operation_review_test.go](../../../framework/saga/step_operation_review_test.go)：`TestMongoStoreTombstoneOfAFailureCloseIsAbandoned`、`TestNativeStepExpiredDeliveryStillReplaysTheOperationsSuccess`
- [step_operation_state_promises_test.go](../../../framework/saga/step_operation_state_promises_test.go)：`TestOperationStateKeepsTheRefusalsOfTheTwoNewestLives`、`TestOperationStateRemembersTheLatestSupersededAttempts`
- [step_success_in_backoff_promises_test.go](../../../framework/saga/step_success_in_backoff_promises_test.go)：`TestNativeStepSuccessDeliveredDuringBackoffIsNotLostWhenTheOperationIsClosed`、`TestMongoStoreSuccessDuringBackoffClosesTheOperationWithItsResult`
- [completion_transition_test.go](../../../framework/saga/completion_transition_test.go)：`TestStepTransitionAloneDecidesTheIncarnation`
- [transaction_cancel_review_test.go](../../../framework/saga/transaction_cancel_review_test.go)：`TestSagaCompletionTransactionCancellationAndRetry`、`TestMongoCommandInboxCancelledBusinessWriteCanRetryAtomically`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../performance/STABLE-v1.24.0.md)，没有新测量则不能改容量承诺。
