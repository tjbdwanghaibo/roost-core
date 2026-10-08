# 次核心：Service 领域能力：实现与维护

运行时代码基准：v1.24.0（2fa1c7877b14c77b52e062bedcb3455cef8db0fb）。[设计与使用](../guide/09-kit-services.md)

## 如何阅读

账号、邮件、聊天、排行等能力具有自己的数据和接口。Service篇说明如何在本进程使用它们，或者把它们拆成单独进程。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`service`、`servicerpc`、`servicemetrics`、`kit/service`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. 公开接口与 .local 管理接口分开。
2. 幂等身份跨重试保持，未知结果不当作未执行。
3. account 身份、玩家号和名字规则是必需协作者。
4. kit/service 尚有七个领域实现，文档不能写成纯装配已完成。

## 2A. 一个服务的两层实现

以mail为例，[service.go](../../../service/mail/service.go)的New/Send/deliverAndRecord承担领域输入、请求身份、信封和投递结果；存储能力由store/redis_store提供。[mail_mod.go](../../../kit/service/mail/mail_mod.go)从配置和Registry接线，生成mail_rpc_assembly_gen.go发布能力、注册Server/ClientMod，不能在Mod里另发一份绕过Send幂等的邮件。

account是当前分层例外：[account_mod.go](../../../kit/service/account/account_mod.go)的NewMod要求IdentityVerifier、PlayerIDAllocator、NameValidator及Reporter；[service.go](../../../kit/service/account/service.go)仍在同一kit包承担领域逻辑。prefix通过声明必填，不能静默共用所有项目的默认键。

接口层的参数身份属于可信服务调用边界；外部玩家接入必须把认证连接身份映射到参数。RPC信封还原业务错误不等于自动校验终端玩家权限。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

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

### service/mail

8 个实现文件、25 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [mail.go](../../../service/mail/mail.go) | `Mail` |
| [mail_rpc_gen.go](../../../service/mail/mail_rpc_gen.go) | `BusClient` |
| [mailbox.go](../../../service/mail/mailbox.go) | `Mailbox`、`Entry`、`SettledClaim` |
| [redis_store.go](../../../service/mail/redis_store.go) | `RedisStores`、`RedisConfig` |
| [send_intent.go](../../../service/mail/send_intent.go) | 函数/方法或内部实现；见源码 |
| [service.go](../../../service/mail/service.go) | `Config`、`Service`、`SendRequest`、`Page`、`Item`、`Claim`、`Summary` |
| [store.go](../../../service/mail/store.go) | `EnvelopeStore`、`MailboxStore`、`SendLedger`、`SentRecord`、`Deliverer`、`DelivererFunc` |
| [types.go](../../../service/mail/types.go) | `Status`、`Audience`、`Envelope` |

### service/match

7 个实现文件、12 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [grouping.go](../../../service/match/grouping.go) | `Grouping`、`FirstComeGrouping`、`ScoreWindowGrouping` |
| [match_rpc.go](../../../service/match/match_rpc.go) | `Matchmaker` |
| [matchmaker_rpc_gen.go](../../../service/match/matchmaker_rpc_gen.go) | `BusClient` |
| [queue_store.go](../../../service/match/queue_store.go) | `Config` |
| [redis_store.go](../../../service/match/redis_store.go) | 函数/方法或内部实现；见源码 |
| [store.go](../../../service/match/store.go) | `Store` |
| [types.go](../../../service/match/types.go) | `SubjectKind`、`Subject`、`Queue`、`TicketState`、`Ticket`、`Match` |

### service/session

7 个实现文件、12 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [admin.go](../../../service/session/admin.go) | `Admin` |
| [redis_store.go](../../../service/session/redis_store.go) | `RedisStores`、`RedisConfig` |
| [service.go](../../../service/session/service.go) | `RunStore`、`ClaimStore`、`RequestLedger`、`LedgerEntry`、`Releaser`、`OwnerSource`、`OwnerSourceFunc`、`ReleaserFunc`、`Config`、`Service`、`EnterRequest` |
| [session_rpc.go](../../../service/session/session_rpc.go) | `Session` |
| [session_rpc_gen.go](../../../service/session/session_rpc_gen.go) | `BusClient` |
| [sweep_source.go](../../../service/session/sweep_source.go) | `AdmissionSource` |
| [types.go](../../../service/session/types.go) | `State`、`Resource`、`Run`、`Claim` |

### servicemetrics

3 个实现文件、2 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [metrics_reporter.go](../../../servicemetrics/metrics_reporter.go) | `MetricsReporter` |
| [recorder.go](../../../servicemetrics/recorder.go) | `Recorder` |
| [servicemetrics.go](../../../servicemetrics/servicemetrics.go) | `Reporter`、`KeyedReporter`、`Sink` |

### servicerpc

3 个实现文件、3 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [affinity.go](../../../servicerpc/affinity.go) | `KeyAffinityPicker` |
| [client.go](../../../servicerpc/client.go) | `BusClient`、`DiscoveryPicker`、`Transport`、`Option`、`TransportConfig`、`ReliableBus`、`ResponseStatusProvider`、`RoundRobinPicker` |
| [status.go](../../../servicerpc/status.go) | 函数/方法或内部实现；见源码 |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Idempot`、`Test.*Request`、`Test.*TTL`、`Test.*Affinity`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

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

### service/mail

- [atomic_refusal_promises_test.go](../../../service/mail/atomic_refusal_promises_test.go)：`TestRefusedDeliveryLeavesTheMailboxUntouched`
- [batch_pipeline_integration_test.go](../../../service/mail/batch_pipeline_integration_test.go)：`TestIntegrationEnvelopeBatchAcrossSlots`
- [bugfix_deleted_identity_test.go](../../../service/mail/bugfix_deleted_identity_test.go)：`TestBugfix4DeletedUnclaimedMailMustNotResurrect`、`TestDeletedTombstonesAreBoundedWithoutForgettingUnknownExpiry`
- [business_clock_promises_test.go](../../../service/mail/business_clock_promises_test.go)：`TestMailExpiryAndTheClaimLeaseRunOnTheMonotonicBusinessClock`
- [claim_expiry_promises_test.go](../../../service/mail/claim_expiry_promises_test.go)：`TestReserveClaimReportsWhenTheMailStopsBeingClaimable`
- [claim_identity_promises_test.go](../../../service/mail/claim_identity_promises_test.go)：`TestEvictionKeepsTheClaimIdentityOfAClaimedMail`、`TestEvictionPreservesUnclaimedDeletion`、`TestCommitClaimReplaysAfterTheEntryWasEvicted`、`TestSettledClaimsAgeOutWithTheirEnvelope`
- [default_clock_promises_test.go](../../../service/mail/default_clock_promises_test.go)：`TestMissingBusinessClockUsesProcessOffset`、`TestRedisEnvelopeDefaultClockMatchesBusinessExpiry`
- [entry_guards_promises_test.go](../../../service/mail/entry_guards_promises_test.go)：`TestNewRedisStoresRefusesEachMissingPrerequisite`、`TestServiceEntryPointsRefuseInvalidIdentifiers`
- [errcode_test.go](../../../service/mail/errcode_test.go)：`TestEverySentinelCarriesItsOwnCode`、`TestNoTwoSentinelsShareACode`、`TestTheCodeSegmentIsContiguousFromItsFirstCode`、`TestTheCodeSurvivesWrapping`、`TestErrorsFromRealCallPathsCarryTheirCodes`；其余 1 项见文件
- [fake_envelopes_test.go](../../../service/mail/fake_envelopes_test.go)
- [get_consistency_promises_test.go](../../../service/mail/get_consistency_promises_test.go)：`TestGetAndGetManyAgreeOnEveryStoredShape`
- [mail_test.go](../../../service/mail/mail_test.go)：`TestAReReservationReturnsTheSameTokenAfterTheLeaseLapses`、`TestAnInFlightReservationBlocksAnotherOne`、`TestACommitWithTheWrongTokenIsRefused`、`TestARetriedCommitIsANoOp`、`TestTheClaimTokenSurvivesTheCommit`；其余 28 项见文件
- [redis_guards_promises_test.go](../../../service/mail/redis_guards_promises_test.go)：`TestRedisStoresRefuseInvalidConfigAndEmptyIDs`
- [redis_integration_test.go](../../../service/mail/redis_integration_test.go)：`TestIntegrationBatchReadReturnsOneValuePerKey`、`TestIntegrationAnEmptyBatchIsNotSentToRedis`、`TestIntegrationConcurrentCreatesOfOneIDProduceOneEnvelope`、`TestIntegrationAnEnvelopeExpiresOnItsOwnDeadline`、`TestIntegrationTheServiceRunsEndToEndOnRedis`
- [redis_store_test.go](../../../service/mail/redis_store_test.go)：`TestRedisCreateRefusesAnExistingID`、`TestRedisCreateRefusesAnAlreadyExpiredEnvelope`、`TestRedisGetManyIsOneBoundedBatch`、`TestRedisGetManyReportsMissingEnvelopesAsAbsent`、`TestRedisGetManyRefusesAnOversizedBatch`；其余 6 项见文件
- [remaining_promises_test.go](../../../service/mail/remaining_promises_test.go)：`TestClaimLeaseRejectsSubsecond`、`TestDeliverRefusesMissingEnvelope`
- [rpc_test.go](../../../service/mail/rpc_test.go)：`TestMethodsCoversTheInterfaceExactly`、`TestRegisterHandlersPublishesExactlyTheDeclaredMethods`、`TestRegisterHandlersRefusesANilBusOrService`、`TestBothImplementationsBehaveTheSame`、`TestErrorCodesSurviveBothTransports`；其余 2 项见文件
- [rr_20260929_round1_test.go](../../../service/mail/rr_20260929_round1_test.go)：`TestReviewExpiredMailboxMakesRoom`
- [rr_20260929_round2_test.go](../../../service/mail/rr_20260929_round2_test.go)：`TestReviewTransientEnvelopeFailureCanRetrySameRequest`
- [rr_20260929_round3_test.go](../../../service/mail/rr_20260929_round3_test.go)：`TestReview3OldCancelCannotReleaseNewClaimAttempt`、`TestReview3ObserveExpiredCancelledClaimsRetainCapacity`、`TestCancelClaimGenerationCrossesLocalAndBusTransports`、`TestCancelClaimLegacyWireIsRejected`、`TestClaimAttemptGenerationNeverWraps`
- [rr_20261001_02_test.go](../../../service/mail/rr_20261001_02_test.go)：`TestSameRequestRecoveryToleratesEmptyVsNilSlices`、`TestSameRequestRecoveryStillRejectsSubstitutedEnvelope`、`TestSameSendIntentFieldwise`
- [send_race_promises_test.go](../../../service/mail/send_race_promises_test.go)：`TestSendReportsEachLedgerRaceAsConflict`
- [send_recovery_integration_test.go](../../../service/mail/send_recovery_integration_test.go)：`TestIntegrationSendRecoveryOnRealEnvelopes`
- [settled_claim_retention_promises_test.go](../../../service/mail/settled_claim_retention_promises_test.go)：`TestMailboxSnapshotDoesNotShareTheSettledClaims`、`TestSettledClaimsOutliveTheEnvelopeTheyProtect`、`TestSettledClaimsAreDroppedOnceTheEnvelopeExpires`
- [validate_promises_test.go](../../../service/mail/validate_promises_test.go)：`TestEnvelopeValidateRefusesEachBrokenField`

### service/match

- [bugfix_grouping_boundary_test.go](../../../service/match/bugfix_grouping_boundary_test.go)：`TestBugfix5GroupingValidatesBeforeReadingCandidates`
- [commit_promises_test.go](../../../service/match/commit_promises_test.go)：`TestCommitRefusesEachInvalidTicketSet`、`TestCancelRefusesBlankAndUnknownTickets`
- [default_clock_promises_test.go](../../../service/match/default_clock_promises_test.go)：`TestMissingBusinessClockUsesProcessOffset`
- [enqueue_request_owner_promises_test.go](../../../service/match/enqueue_request_owner_promises_test.go)：`TestEnqueueReplayRefusesARequestIDFromAnotherSubject`
- [errcode_test.go](../../../service/match/errcode_test.go)：`TestEverySentinelCarriesItsOwnCode`、`TestTheCodeSegmentIsExactlyAsAllocated`、`TestTheCodeSurvivesWrapping`、`TestAnUnclassifiedFailureReadsAsInternal`、`TestAForeignSentinelIsMappedIntoThisSegment`
- [match_test.go](../../../service/match/match_test.go)：`TestSubjectMayHoldOnlyOneLiveTicket`、`TestTicketIDsAreServerMintedAndDistinct`、`TestEnqueueIsIdempotentPerRequest`、`TestTicketOperationsRequireOwnership`、`TestTicketDeadlineIsEnforcedOnReadAndBySweep`；其余 20 项见文件
- [queue_key_collision_promises_test.go](../../../service/match/queue_key_collision_promises_test.go)：`TestDistinctQueuesNeverShareAKey`、`TestACommitCannotTakeTicketsFromAnotherQueue`
- [read_guards_promises_test.go](../../../service/match/read_guards_promises_test.go)：`TestReadsOfAnUnknownQueueMissCleanlyAndStoreErrorsPropagate`、`TestCancelRefusesUnknownQueuesAndTickets`、`TestCommitRefusesACorruptedQueueWhereOneSubjectHoldsTwoTickets`、`TestNewRedisStoreRequiresAKeyPrefix`
- [remaining_promises_test.go](../../../service/match/remaining_promises_test.go)：`TestTicketTTLRejectsSubsecond`、`TestCommitReplayReturnsOriginalMatchForSameTicketSet`
- [result_ownership_promises_test.go](../../../service/match/result_ownership_promises_test.go)：`TestReturnedAndInputSlicesDoNotAliasTheStore`
- [score_window_overflow_promises_test.go](../../../service/match/score_window_overflow_promises_test.go)：`TestScoreWindowArithmeticDoesNotOverflow`、`TestScoreWindowOrdersByTrueDistance`、`TestScoreWindowRefusesNegativeConfiguration`
- [sweep_failure_promises_test.go](../../../service/match/sweep_failure_promises_test.go)：`TestSweepFailureIsCountedNotJustReturned`

### service/session

- [admin_test.go](../../../service/session/admin_test.go)：`TestAResourceTheReleaserCanNeverFreeCanBeForced`、`TestForcedReleasesAccumulateAcrossResources`、`TestAForcedReleaseIsDistinguishableFromARealOne`、`TestForcingAnAlreadyReleasedResourceIsRefused`、`TestForcingSomethingThatIsNotThereIsRefused`；其余 4 项见文件
- [default_clock_promises_test.go](../../../service/session/default_clock_promises_test.go)：`TestMissingBusinessClockUsesProcessOffset`
- [enter_collision_cleanup_promises_test.go](../../../service/session/enter_collision_cleanup_promises_test.go)：`TestEnterCollisionCleanupNeverRemovesAReacquiredClaim`
- [enter_ledger_collision_promises_test.go](../../../service/session/enter_ledger_collision_promises_test.go)：`TestEnterRefusesTheLoserOfARequestIDRaceAndUndoesItsRun`
- [errcode_test.go](../../../service/session/errcode_test.go)：`TestEverySentinelCarriesItsOwnCode`、`TestTheCodeSegmentIsExactlyAsAllocated`、`TestTheCodeSurvivesWrapping`、`TestAnUnclassifiedFailureReadsAsInternal`、`TestAForeignSentinelIsMappedIntoThisSegment`
- [guards_promises_test.go](../../../service/session/guards_promises_test.go)：`TestEnterAndForceReleaseRejectInvalidIdentifiers`、`TestRedisStoresRequirePrefixAndRequestTTL`、`TestLedgerNamingAMissingRunIsAConflictNotANewRun`、`TestOperationsOnMissingRunsAreRunMissing`、`TestRunVanishingBetweenGetAndUpdateIsRunMissing`
- [ledger_failure_test.go](../../../service/session/ledger_failure_test.go)：`TestEnterReportsALostLedgerWriteInsteadOfSuccess`
- [remaining_promises_test.go](../../../service/session/remaining_promises_test.go)：`TestLostRunCreateReplyCanBeReclaimedWithoutOwnerClaim`、`TestSlowClaimCannotAdmitExpiredRun`、`TestEnterRetakesClaimReleasedBeforeRead`、`TestRunTTLRejectsSubsecond`、`TestRunCodecRequiresAdmissionFormat`；其余 1 项见文件
- [rr_20260929_round2_test.go](../../../service/session/rr_20260929_round2_test.go)：`TestReviewAttachCannotAssertAlreadyReleased`、`TestReviewSuccessfulFinishRetryFreesClaim`、`TestReviewTerminalFinishCannotDeleteReacquiredClaim`
- [session_test.go](../../../service/session/session_test.go)：`TestAnEnterWithoutAnIdempotencyKeyIsRefused`、`TestEnterIsIdempotentPerRequestID`、`TestAFreshRequestIDDoesNotBuyASecondRun`、`TestAnOwnerRacingItselfGetsOneRun`、`TestOneRequestIDCannotServeTwoOwners`；其余 21 项见文件
- [sweep_source_test.go](../../../service/session/sweep_source_test.go)：`TestConfiguredOwnerSourceReleasesExpiredRun`、`TestOwnerSourceIsBoundedAndFailuresAreRetryable`
- [validate_promises_test.go](../../../service/session/validate_promises_test.go)：`TestRunValidateRefusesEachBrokenField`、`TestEnterRequestValidateRefusesEachBrokenField`

### servicemetrics

- [metrics_reporter_promises_test.go](../../../servicemetrics/metrics_reporter_promises_test.go)：`TestMetricsReporterExportsEveryEventUnderAFixedName`、`TestDepthOfFallsBackToTheOldNameForAReporterWithoutKeys`
- [servicemetrics_test.go](../../../servicemetrics/servicemetrics_test.go)：`TestANilReporterIsSafeOnEveryMethod`、`TestEveryEventReachesTheReporter`、`TestAZeroDropCountIsNotReported`、`TestASinkIsSafeToShareAcrossGoroutines`、`TestRecorderTreatsDepthAsAGauge`；其余 3 项见文件

### servicerpc

- [discovery_budget_promises_test.go](../../../servicerpc/discovery_budget_promises_test.go)：`TestRPCBudgetDiscoveredCallTimeoutIncludesDiscovery`、`TestRPCBudgetDiscoveredCallPreservesCallerDeadline`、`TestRPCBudgetDiscoveryWaitUsesConfiguredBudget`、`TestRPCBudgetDiscoveryFiltersAndTransportRefuses`、`TestRPCBudgetDiscoveryPickerTransportShareDeadline`；其余 1 项见文件
- [guards_promises_test.go](../../../servicerpc/guards_promises_test.go)：`TestPickersAndReliableTransportRefuseWhatIsMissing`
- [servicerpc_test.go](../../../servicerpc/servicerpc_test.go)：`TestCheckResponseSurfacesTheEnvelopeStatus`、`TestBusClientRoutesToDiscoveredInstanceAndHonoursTransport`、`TestPickServerReportsAnEmptyDiscoverySet`、`TestRoundRobinSpreadsWhileKeyAffinityPins`、`TestKeyAffinityFallsBackWithoutAKey`；其余 2 项见文件

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。本次文档补丁的实际执行结果见 [发布验收](../../release/v1.24.1-IMPLEMENTATION.md)；性能沿用 [v1.24.0 基线](../../maintenance/PERFORMANCE.md)，没有新测量则不能改容量承诺。
