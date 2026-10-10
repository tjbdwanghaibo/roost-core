# 观测、安全与运维：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/11-observability.md)

## 如何阅读

服务变慢或出错时，指标告诉你发生了多少次，日志帮助定位某一次，健康检查决定是否继续接流量。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`infra/observe/metrics`、`infra/observe/health`、`infra/observe/log`、`infra/observe/failurelog`、`infra/observe/admin`、`infra/base/security`、`infra/network/httpclient`、`infra/network/httpserver`、`infra/network/webroute`、`infra/base/errcode`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. Degraded 仍 ready，Fail 与生命周期就绪位决定准入。
2. 指标定义和导出文档同源校验；分位数不由累计量猜测。
3. admin 认证审计与上层审批分开；敏感数据不入日志。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### admin

1 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [admin.go](../../../infra/observe/admin/admin.go) | `Command`、`Result`、`Handler`、`CommandDef`、`RiskLevel`、`CommandMeta`、`Registry`、`MetadataRegistry` |

### errcode

1 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [errcode.go](../../../infra/base/errcode/errcode.go) | `Definition`、`IntError` |

### failurelog

1 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [failurelog.go](../../../infra/observe/failurelog/failurelog.go) | `Config`、`RedisList` |

### health

1 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [health.go](../../../infra/observe/health/health.go) | `Status`、`Result`、`Snapshot`、`Checker`、`CheckerFunc`、`Registry` |

### httpclient

1 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [client.go](../../../infra/network/httpclient/client.go) | `Option`、`Client`、`StatusError` |

### httpserver

1 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [server.go](../../../infra/network/httpserver/server.go) | `Config`、`Option`、`Engine`、`RouteRegistrar`、`Group` |

### log

4 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [elog.go](../../../infra/observe/log/elog.go) | `IDProvider`、`ELog`、`ELogOption` |
| [log.go](../../../infra/observe/log/log.go) | `Options` |
| [ordered_text_handler.go](../../../infra/observe/log/ordered_text_handler.go) | 函数/方法或内部实现；见源码 |
| [rotation.go](../../../infra/observe/log/rotation.go) | 函数/方法或内部实现；见源码 |

### metrics

2 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [metrics.go](../../../infra/observe/metrics/metrics.go) | `Kind`、`Labels`、`Metric`、`Registry`、`RegistryOption` |
| [prometheus.go](../../../infra/observe/metrics/prometheus.go) | 函数/方法或内部实现；见源码 |

### security

3 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [payload_signature.go](../../../infra/base/security/payload_signature.go) | 函数/方法或内部实现；见源码 |
| [ratelimit.go](../../../infra/base/security/ratelimit.go) | `RateLimitKey`、`RateLimitConfig`、`RateLimitStats`、`RateLimiter` |
| [session_token.go](../../../infra/base/security/session_token.go) | `SessionClaims` |

### webroute

1 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [route.go](../../../infra/network/webroute/route.go) | `Registerer`、`Module`、`Registrar`、`RawRequest`、`ErrorMapper` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Metric`、`Test.*Ready`、`Test.*Admin`、`Test.*Security`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### admin

- [admin_test.go](../../../infra/observe/admin/admin_test.go)：`TestRegistryExecute`、`TestRegistryRegisterReplacesExistingCommand`、`TestRegistryExecuteRecoversHandlerPanic`、`TestMetadataRegistryRegistersAndListsSortedCommands`
- [audit_promises_test.go](../../../infra/observe/admin/audit_promises_test.go)：`TestAdminExecutionWritesCorrelatedAuditWithoutPayload`
- [guards_promises_test.go](../../../infra/observe/admin/guards_promises_test.go)：`TestRegistriesRefuseNilReceiversAndUnknownCommands`
- [promises_test.go](../../../infra/observe/admin/promises_test.go)：`TestAdminRegistriesRefuseNamelessAndHandlerlessCommands`
- [schema_ownership_promises_test.go](../../../infra/observe/admin/schema_ownership_promises_test.go)：`TestMetadataOwnership`、`TestSchemaPreservesNilAndEmptyContainers`、`TestSchemaNestedArraysAndStringsHaveIndependentOwnership`、`TestConcurrentMetadataConsumersOnlyMutateTheirCopies`

### errcode

- [errcode_test.go](../../../infra/base/errcode/errcode_test.go)：`TestIntErrorMapsClientCodeAndPreservesCause`、`TestClientErrorFallsBackForPlainError`、`TestRemoteErrorPreservesClientCode`

### failurelog

- [failurelog_fix_test.go](../../../infra/observe/failurelog/failurelog_fix_test.go)：`TestTrimUsesInPlaceLTrimWithoutDelete`、`TestDeleteRawUsesInPlaceLRemWithoutDelete`、`TestScriptFallbackIsCountedAsDegraded`
- [failurelog_test.go](../../../infra/observe/failurelog/failurelog_test.go)：`TestRedisListAppendRawKeepsNewestEntries`、`TestRedisListListRawSupportsSingleElementRange`、`TestRedisListPurgeDeletesKey`、`TestRedisListPurgeReturnsEntryCount`、`TestRedisListCountRawReturnsListLength`；其余 2 项见文件
- [key_guards_promises_test.go](../../../infra/observe/failurelog/key_guards_promises_test.go)：`TestRedisListRefusesAnEmptyKeyOnEveryOperation`
- [trim_metrics_promises_test.go](../../../infra/observe/failurelog/trim_metrics_promises_test.go)：`TestLuaAppendCountsTrimmedRecords`
- [unknown_result_integration_test.go](../../../infra/observe/failurelog/unknown_result_integration_test.go)：`TestIntegrationLostScriptReplyDoesNotAppendTwice`
- [unknown_result_promises_test.go](../../../infra/observe/failurelog/unknown_result_promises_test.go)：`TestUnknownScriptResultIsReturnedWithoutReplayingTheWrite`、`TestAdapterWithoutLuaStillUsesTheFallback`

### health

- [checker_deadline_promises_test.go](../../../infra/observe/health/checker_deadline_promises_test.go)：`TestSnapshotBoundsEveryCheckerByOneDeadline`
- [health_test.go](../../../infra/observe/health/health_test.go)：`TestRegistrySnapshotAggregatesDependencyHealth`、`TestRegistrySnapshotRecoversCheckerPanic`、`TestRegistrySnapshotCountsDegradedAsAvailable`

### httpclient

- [client_boundaries_promises_test.go](../../../infra/network/httpclient/client_boundaries_promises_test.go)：`TestClientDeadlines`、`TestCloneHeaderIsolation`、`TestStatusErrorClassification`、`TestCloneTimeoutPreservesParentAndCustomClient`、`TestCloneTimeoutConcurrentRequestsKeepParentDeadline`；其余 1 项见文件
- [guards_promises_test.go](../../../infra/network/httpclient/guards_promises_test.go)：`TestDoJSONOnANilClientFails`
- [httpclient_test.go](../../../infra/network/httpclient/httpclient_test.go)：`TestClientPostJSONSendsSignatureHeadersAndDecodesResponse`、`TestClientPostJSONDecodesErrorBodyAndReturnsStatusError`、`TestClientClonePreservesBaseURLAndAddsHeaders`

### httpserver

- [httpserver_test.go](../../../infra/network/httpserver/httpserver_test.go)：`TestEngineRoutesGroupsAndJSONHandlersWithRequestID`、`TestEngineRejectsTooLargeJSONBody`、`TestReadBodyUsesEngineBodyLimit`、`TestEngineRecoversPanicAsJSON`、`TestNewServerAppliesProductionTimeouts`
- [json_boundary_promises_test.go](../../../infra/network/httpserver/json_boundary_promises_test.go)：`TestJSONRequestBoundary`
- [response_buffering_promises_test.go](../../../infra/network/httpserver/response_buffering_promises_test.go)：`TestJSONDoesNotCopyTheEncodedBodyPerResponse`
- [response_integrity_promises_test.go](../../../infra/network/httpserver/response_integrity_promises_test.go)：`TestJSONEncodeFailureAnswers500InsteadOfAnEmptySuccess`、`TestJSONSuccessKeepsTheEncoderByteShape`、`TestPanicAfterTheResponseStartedAbortsTheConnection`、`TestErrAbortHandlerIsPropagatedNotAnswered`、`TestEarlyHintsDoNotCountAsAStartedResponse`；其余 3 项见文件

### log

- [close_fallback_promises_test.go](../../../infra/observe/log/close_fallback_promises_test.go)：`TestLogsAfterCloseOfAFileOnlySinkReachStderr`、`TestLogsAfterCloseKeepTheConsoleWriter`
- [log_test.go](../../../infra/observe/log/log_test.go)：`TestContextAttrsArePrepended`、`TestRuntimeAttrsAreWrittenBetweenLevelAndMessage`、`TestCallerAttrsIncludeFileLineAndFunction`、`TestELogCallerAttrsIncludeUserFunction`、`TestGoIDKey`；其余 4 项见文件
- [rotation_guards_promises_test.go](../../../infra/observe/log/rotation_guards_promises_test.go)：`TestRotatingWriterRefusesNonPositiveIntervalsAndWritesAfterClose`
- [system_time_promises_test.go](../../../infra/observe/log/system_time_promises_test.go)：`TestServerTimeUsesSystemClockInsideBusinessContext`
- [write_failure_promises_test.go](../../../infra/observe/log/write_failure_promises_test.go)：`TestRotationFailureKeepsWritingTheCurrentSlice`、`TestAFailingConsoleDoesNotStopTheFileSink`

### metrics

- [delete_series_promises_test.go](../../../infra/observe/metrics/delete_series_promises_test.go)：`TestDeleteSeriesRemovesEveryKindByLabelAndReturnsTheQuota`
- [histogram_quantile_bounds_promises_test.go](../../../infra/observe/metrics/histogram_quantile_bounds_promises_test.go)：`TestHistogramQuantileNeverExceedsTheSlowestObservation`、`TestHistogramQuantileInTheOverflowIsNotUnderstated`、`TestHistogramQuantileOfIdenticalSamplesIsThatSample`
- [metrics_test.go](../../../infra/observe/metrics/metrics_test.go)：`TestRegistrySnapshot`、`TestPrometheusTextDoesNotDoubleTotalSuffix`、`TestRegistryLimitsMetricSeriesCardinality`、`TestSeriesLimitDropIsVisible`、`TestHistogramObserveQuantileAndExport`；其余 1 项见文件
- [prometheus_escape_promises_test.go](../../../infra/observe/metrics/prometheus_escape_promises_test.go)：`TestPrometheusLabelValuesUseTheExpositionEscapes`

### security

- [admission_promises_test.go](../../../infra/base/security/admission_promises_test.go)：`TestOversizedDemandHasNoKeySideEffects`
- [owner_capacity_promises_test.go](../../../infra/base/security/owner_capacity_promises_test.go)：`TestOneOwnerCannotFillTheKeyTableForOthers`、`TestOwnerKeyLimitIsCountedAndReleasedByIdleSweep`、`TestFullTableRejectionDoesNotSweepBeforeTheSweepInterval`
- [ratelimit_test.go](../../../infra/base/security/ratelimit_test.go)：`TestRateLimiter`、`TestRateLimiterBoundsKeyCardinality`、`TestRateLimiterAutomaticallyReclaimsIdleKeys`、`TestRateLimiterActivityExtendsIdleLifetime`、`TestRateLimiterRefillsContinuously`；其余 1 项见文件
- [session_token_promises_test.go](../../../infra/base/security/session_token_promises_test.go)：`TestSessionTokenRefusesEachMalformedOrForgedToken`
- [session_token_test.go](../../../infra/base/security/session_token_test.go)：`TestSessionTokenRoundTrip`、`TestSessionTokenRejectsWrongPlayerAndExpired`

### webroute

- [guards_promises_test.go](../../../infra/network/webroute/guards_promises_test.go)：`TestRegisterModulesRefusesANilModule`
- [pattern_promises_test.go](../../../infra/network/webroute/pattern_promises_test.go)：`TestRegisterRejectsMalformedPatternsBeforeInstallation`、`TestRegisterInstallationPanicDoesNotReservePair`、`TestRegisterAcceptsChiPatterns`
- [promises_test.go](../../../infra/network/webroute/promises_test.go)：`TestRegistrarRefusesEachInvalidRouteByMessage`
- [result_encoding_promises_test.go](../../../infra/network/webroute/result_encoding_promises_test.go)：`TestUnencodableRouteResultReachesTheClientAs500`
- [route_test.go](../../../infra/network/webroute/route_test.go)：`TestRegisterRejectsDuplicateMethodPath`、`TestDecodeJSONRejectsMalformedBody`、`TestDecodeJSONAndWriteResult`、`TestReadRawCopiesRequestData`、`TestRegisterModulesStopsOnFirstError`

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../performance/STABLE-v1.24.0.md)，没有新测量则不能改容量承诺。
