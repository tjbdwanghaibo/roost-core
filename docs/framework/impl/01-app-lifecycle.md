# App 与生命周期：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/01-app-lifecycle.md)

## 如何阅读

一个游戏服启动时要加载配置、连接资源，退出时要把正在做的工作收好。App就是组织这些步骤的入口。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`framework/app`、`infra/base/lifecycle`、`framework/manager`、`internal/stopcontract`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 重复 Mod 名启动拒绝；依赖与 capability 区分。
2. 失锁、不可判定持久结果触发统一 fail-stop。
3. Shutdown 超时不能提前释放仍被使用的依赖。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### app

14 个实现文件、26 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [app.go](../../../framework/app/app.go) | `App` |
| [business_clock.go](../../../framework/app/business_clock.go) | 函数/方法或内部实现；见源码 |
| [business_time.go](../../../framework/app/business_time.go) | 函数/方法或内部实现；见源码 |
| [config_schema.go](../../../framework/app/config_schema.go) | `ConfigSchema`、`ModConfigSchema`、`ServiceIdentity` |
| [config_validation.go](../../../framework/app/config_validation.go) | 函数/方法或内部实现；见源码 |
| [config_values.go](../../../framework/app/config_values.go) | 函数/方法或内部实现；见源码 |
| [manager.go](../../../framework/app/manager.go) | `IManager`、`IManagerStopperWithContext`、`ManagerDependencyProvider` |
| [mod.go](../../../framework/app/mod.go) | `Mod`、`ModStopperWithContext`、`ModStopBudgetProvider`、`ModDependencyProvider`、`ModOptionalDependencyProvider` |
| [name.go](../../../framework/app/name.go) | `ModName`、`ServiceName` |
| [registry.go](../../../framework/app/registry.go) | `Registry`、`Capability` |
| [runtime_failure.go](../../../framework/app/runtime_failure.go) | `RuntimeFailure` |
| [service.go](../../../framework/app/service.go) | `Service` |
| [singleton.go](../../../framework/app/singleton.go) | `SingletonStore`、`SingletonOpener`、`SingletonLiveness`、`SingletonIncarnation` |
| [startup.go](../../../framework/app/startup.go) | 函数/方法或内部实现；见源码 |

### app/buildinfo

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [buildinfo.go](../../../framework/app/buildinfo/buildinfo.go) | `BuildInfo` |

### internal/stopcontract

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [stopcontract.go](../../../internal/stopcontract/stopcontract.go) | `Hooks` |

### lifecycle

2 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [lifecycle.go](../../../infra/base/lifecycle/lifecycle.go) | `Phase`、`Event`、`Handler`、`Hook`、`Registry` |
| [manager_group.go](../../../infra/base/lifecycle/manager_group.go) | `Manager`、`ManagerGroupState`、`ManagerGroup` |

### manager

2 个实现文件、9 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [engine.go](../../../framework/manager/engine.go) | `Engine` |
| [order.go](../../../framework/manager/order.go) | 函数/方法或内部实现；见源码 |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Singleton`、`Test.*Shutdown`、`Test.*Config`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### app

- [app_test.go](../../../framework/app/app_test.go)：`TestAppReturnsServeError`、`TestAppExplicitConfigPathFailsClosed`、`TestAppInvalidDefaultConfigFailsClosed`、`TestAppReturnsShutdownError`、`TestAppReturnsServiceStoppingLifecycleError`；其余 9 项见文件
- [business_clock_contract_test.go](../../../framework/app/business_clock_contract_test.go)：`TestBusinessClockFollowsTheConfiguredOffset`、`TestOffsetMovesBusinessTimeButNotTheSingletonLease`、`TestBusinessTimeMovingBackRefusesToStart`、`TestBusinessTimeMayMoveForwardOrStay`、`TestTheHighWaterMarkAdvancesWhileRunning`、`TestAnUnreadableHighWaterMarkRefusesToStart`、`TestAnOffsetWithoutTheSingletonStillGuardsBusinessTime`；其余 2 项见文件
- [config_contract_test.go](../../../framework/app/config_contract_test.go)：`TestCheckConfigRequiresAnExistingFile`、`TestCheckConfigAcceptsExistingDefaultFileWithoutStartingService`、`TestSortModsRejectsNameAlreadyOwnedBySharedMod`、`TestConfigIntAcceptsWholeNumbersOnly`、`TestCheckConfigReportsEveryModsErrorsAtOnce`、`TestLoadConfigFillsDefaultsAndRefusesOutOfRangeValues`、`TestServiceDeclarationsAreCheckedAndPrintedWithTheMods`、`TestFrameworkModsReadConfigOnlyThroughDeclarations`；其余 2 项见文件、`TestValidateServiceConfigRejectsABoolSwitchThatIsNotABool`、`TestValidateServiceConfigAcceptsTheBoolSpellingsItAlwaysAccepted`、`TestValidateServiceConfigRejectsADurationWithoutAUnit`、`TestValidateServiceConfigAcceptsDurationsWithUnits`
- [config_validation_test.go](../../../framework/app/config_validation_test.go)：`TestValidateServiceConfigAcceptsMinimalConfig`、`TestProductionServiceConfigBaselineIsValid`
- [example_test.go](../../../framework/app/example_test.go)
- [exit_reason_log_promises_test.go](../../../framework/app/exit_reason_log_promises_test.go)：`TestRunWritesTheExitReasonToTheFileLog`
- [guards_promises_test.go](../../../framework/app/guards_promises_test.go)：`TestStopModsReverseStopsNothingOnceTheContextIsDone`、`TestEmitLifecycleRequiresARegistryWithTheLifecycleCapability`、`TestSortModsRefusesNilUnnamedAndDuplicateMods`、`TestValidateServiceConfigAndRegisterBatchRefuseMissingInputs`
- [late_runtime_failure_promises_test.go](../../../framework/app/late_runtime_failure_promises_test.go)：`TestRuntimeFailureDuringShutdownIsReturned`、`TestRuntimeFailureThatStartsTheShutdownIsReturnedOnce`
- [log_rotation_promises_test.go](../../../framework/app/log_rotation_promises_test.go)：`TestNegativeLogRotationIsRejected`
- [logic_offset_production_promises_test.go](../../../framework/app/logic_offset_production_promises_test.go)：`TestProductionRefusesANonZeroLogicOffset`
- [mod_order_test.go](../../../framework/app/mod_order_test.go)：`TestSortModsOrdersPresentOptionalDependencies`、`TestSortModsIgnoresAbsentOptionalDependencies`、`TestSortModsDetectsOptionalDependencyCycle`
- [mod_validation_promises_test.go](../../../framework/app/mod_validation_promises_test.go)：`TestSingleModValidation`、`TestSingleModExecuteFailsBeforeInit`
- [registry_test.go](../../../framework/app/registry_test.go)：`TestNewRegistryInstallsRuntimeObsRegistry`、`TestRegistryRegisterBatchIsAtomic`
- [remaining_config_promises_test.go](../../../framework/app/remaining_config_promises_test.go)：`TestSharedKeyErrorIsReportedOnceAcrossOwners`
- [runtime_failure_test.go](../../../framework/app/runtime_failure_test.go)：`TestRuntimeFailureFirstReportWakesShutdown`
- [shutdown_hooks_promises_test.go](../../../framework/app/shutdown_hooks_promises_test.go)：`TestShutdownLifecycleHooksStayWithinTheShutdownBudget`
- [singleton_incarnation_promises_test.go](../../../framework/app/singleton_incarnation_promises_test.go)：`TestSingletonIncarnationIsTheHeldLocksIdentity`
- [singleton_test.go](../../../framework/app/singleton_test.go)：`TestSingletonWaitsForTheHolderBeforeAnyModInit`、`TestSingletonGivesUpAtStartupWaitWithoutTakingTheKey`、`TestSingletonTakesOverAfterTheHolderExpires`、`TestSingletonReportsAnUnavailableStoreAtStartupWait`、`TestSingletonClaimsItsOwnValueAfterALostAcquireReply`；其余 21 项见文件
- [startup_contract_test.go](../../../framework/app/startup_contract_test.go)：`TestStartupHookCancellationPreservesDependencies`、`TestSignalDuringModStartNeverReachesServiceInit`、`TestServiceInitFailureStopsWhatInitStartedBeforeTheMods`、`TestStartupCleanupThatDoesNotFinishKeepsTheModsAndTheLock`
- [stop_budget_floor_test.go](../../../framework/app/stop_budget_floor_test.go)：`TestDeclaredStopBudgetIsGrantedWhenTotalCoversItAndTheFloors`、`TestLargeDeclaredStopBudgetKeepsTheFloorOfEarlierMods`、`TestGeneratedDefaultShutdownBudgetsCoverEveryMod`、`TestStopBudgetsSplitEvenlyWhenTotalCannotCoverTheFloors`、`TestUndeclaredStopBudgetsKeepTheEvenSplit`
- [stop_budget_generated_test.go](../../../framework/app/stop_budget_generated_test.go)：`TestGeneratedGameServiceTotalCoversEveryModFloorAndTheDeclaredBudget`、`TestGeneratedGameServiceStopsWithoutBudgetWarnings`
- [stop_budget_test.go](../../../framework/app/stop_budget_test.go)：`TestDeclaredStopBudgetIsHonoredWithinTotalTimeout`、`TestStopBudgetsScaleProportionallyWhenTotalIsInsufficient`、`TestGeneratedDefaultsGrantDataEngineItsScaledDeclaredBudget`、`TestServiceModStopPlansAroundLaterSharedDeclaredBudget`、`TestDeclaredStopBudgetAppliesWithoutTotalDeadline`；其余 1 项见文件

### app/buildinfo

- [buildinfo_test.go](../../../framework/app/buildinfo/buildinfo_test.go)：`TestInfoDefaultsToDevVersion`、`TestInfoStringIncludesLinkedMetadata`

### internal/stopcontract

- [stopcontract_test.go](../../../internal/stopcontract/stopcontract_test.go)：`TestCheckPassesAStopperBuiltOnTheSharedLifetime`、`TestCheckCatchesKnownWrongStoppers`

### lifecycle

- [guards_promises_test.go](../../../infra/base/lifecycle/guards_promises_test.go)：`TestRegistryAndManagerGroupRefuseInvalidRegistrationsAndTransitions`
- [lifecycle_test.go](../../../infra/base/lifecycle/lifecycle_test.go)：`TestEmitOrder`、`TestEmitAllContinuesAfterFailure`、`TestRegisterReplacesSamePhaseAndName`、`TestEmitRecoversHookPanic`、`TestEmitAllWatchedReportsEachHookBeforeItRuns`
- [manager_group_test.go](../../../infra/base/lifecycle/manager_group_test.go)：`TestManagerGroupLifecycleAndIdempotentStop`、`TestManagerGroupInitFailureRollsBackInitializedOnly`、`TestManagerGroupStartFailureContainsPanicAndContinuesCleanup`、`TestNewManagerGroupRejectsInvalidNames`

### manager

- [abort_promises_test.go](../../../framework/manager/abort_promises_test.go)：`TestStartAbortedByShutdownReportsARollbackFailure`
- [engine_lifecycle_promises_test.go](../../../framework/manager/engine_lifecycle_promises_test.go)：`TestConcurrentStartHasOneOwner`、`TestSingleAttemptAfterFailureOrStop`、`TestProvidePreconditionDoesNotConsumeStartup`、`TestLastStartupHandoverAlsoContainsStopPanic`、`TestStopPanicPreservesCauseAndContinues`；其余 4 项见文件
- [engine_test.go](../../../framework/manager/engine_test.go)：`TestEngineStartsInRegistrationOrderAndStopsInReverse`、`TestEngineStartsDependenciesFirstAndTearsDownAfterDependents`、`TestEngineOrderIsStableAcrossRunsForIndependentManagers`、`TestEngineRollsBackStartedManagersWhenOneFails`、`TestEngineStopPrefersBoundedHookAndPassesContext`；其余 8 项见文件
- [guards_promises_test.go](../../../framework/manager/guards_promises_test.go)：`TestEngineReportsRollbackFailuresAndRefusesCycles`
- [order_test.go](../../../framework/manager/order_test.go)：`TestOrderRejectsCyclesByName`、`TestOrderRejectsMissingDependencyNamingBothSides`、`TestOrderRejectsDuplicateAndInvalidEntries`、`TestOrderHandlesDiamondDependencies`、`TestOrderIgnoresDependsOnListingOrder`；其余 2 项见文件
- [register_during_start_promises_test.go](../../../framework/manager/register_during_start_promises_test.go)：`TestRegisterDuringTheFirstStartIsRefused`
- [stop_contract_test.go](../../../framework/manager/stop_contract_test.go)：`TestEngineStopContract`
- [stop_during_last_start_promises_test.go](../../../framework/manager/stop_during_last_start_promises_test.go)：`TestStopDuringTheLastStartStillStopsWhatStarted`
- [stop_retry_promises_test.go](../../../framework/manager/stop_retry_promises_test.go)：`TestEngineStopRetryWaitsForTheManagerTheFirstStopCouldNotDrain`、`TestEngineConcurrentStopWaitsWithinItsOwnContext`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../performance/STABLE-v1.24.0.md)，没有新测量则不能改容量承诺。
