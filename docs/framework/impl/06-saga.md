# Saga 长事务：实现与维护

适用版本：当前 main 与最新发布版本（JetStream 协调器，saga 第 4 批起；之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）；源码目录随改动维护，链接由根包文档门禁保证存在，缺漏以源码为准。[设计与使用](../guide/06-saga.md) · 对外契约 [SAGA.md](../SAGA.md) · 实施设计 [DESIGN-2026-10-28](../../maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md)

## 如何阅读

有些操作需要多个服务分几步完成，例如先扣道具，再创建跨服活动记录。Saga记录这些步骤，并在失败时按业务定义执行补偿。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`framework/saga`：协调器（JetStream 分区事件日志、单写者、快照与清理、准入、发起方 API）、参与方（原生步骤的实体内回执、Mongo 步骤收件箱、步骤消费者、准入信号）、纯状态机 `transition.go`。装配在 `wiring/saga`（saga Mod、`Steps` 步骤运行时、配置规则）。没有 Redis、Store Service；Mongo 冷副本是可选的只读回落（`cold_copy.go`，缺省关闭，第 6 批）。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. **步骤转换集中**：状态怎样变只在 [transition.go](../../../framework/saga/transition.go) 的纯函数里判定，终态由完整步骤集合决定（`judgeCompletion` 一处）。协调器分区只负责“取输入 → 判定 → 发布 → 提交”，不另写规则。回归：`TestTransitionRulesMatchTheRecordedRedisCoordinator`（删除 Redis 协调器前录下的 75 步金样，逐步比对）、`TestJudgeCompletionIsTheUnifiedRule`。
2. **单写者 + 期望序号**：每个分区只有一个写者（`saga.coordinators` 按 sid 静态分配，App 单实例锁保证一个“服务类型 + sid”一个进程；部署规则要求一个 sid 只在一种服务类型里装 saga Mod，见 SAGA.md「分区归属」）。一轮事件一次 fenced 发布（首条带分区事件过滤上的期望序号），PubAck 之后才改内存、才回复、才答查询；被拒且最后一条不是自己的 → `ErrPartitionFenced`，结果未知超过 `saga.publish_unknown_budget`、流满 → 都经 `RuntimeFailure` 让整个进程 fail-stop（App 级统一保证，模块不自己恢复）。回归：`TestRealJetStreamCoordinatorFencesAStaleWriter`、`TestRealJetStreamCoordinatorResolvesUnknownOutcomesAndRepublishesIncompleteBatches`、`TestRealJetStreamCoordinatorFailsStopWhenTheStreamIsFull`。
3. **重放 = 运行**：重启从快照（KV 清单 + 对象正文，校验版本、分区、布局、摘要）加日志尾巴重放，判定与在线运行同一套；快照只在循环外写，清理目标 `min(InHi+1, LastEv, durable ack floor+1)`。回归：`TestRealJetStreamCoordinatorSnapshotPurgeAndReplayAgree`、`TestRealJetStreamCoordinatorKill9Replays`、`TestPurgeBoundKeepsTheLastEventTheReplayStartAndUnackedCommands`。
4. **定时不变量**：每条非终态 saga 在时间轮里都有一个落在它当前 `NextRunAt` 的条目（安装时排、重放时排）；缺了它这条 saga 再也不会被看一眼。只在测试里查（`unitPartition.round` 每轮之后调用 `partition.checkTimers`），不加热路径开销；变异“Waiting 不排轮”时 5 个以上用例变红。到期比较必须与轮同一把尺子（墙钟纳秒，见 `partition.decide` 的注释）。
5. **参与方至多一次**：原生步骤的回执在被改的实体里（`StepReceipts`），业务修改与回执是同一笔 Nest 事务、同一条 WAL 记录；`Begin` 先查回执（成功：任何代际回放；拒绝：只在本代际回放）再判截止，`Complete` 写回执。Mongo 步骤在一个 Mongo 事务里读回执 → handler → 写回执，并发的两次尝试由写冲突串行。两者都是“回执是权威”：结果未知时 nak，重投回放。回执的键是操作键 `<saga ID>:<生命周期>:<方向>:<步骤>`（`operationKey`）：`Record.Lifecycle` 由分区新建记录时 `newLifecycle` 随机生成（128 位，随事件持久，重放、快照、冷副本原样恢复），同一笔之内重启、Resume（换代际，`CommandID` 带 `rN`）、人工 Compensate 都不换，所以成功回执跨尝试、跨代际回放；终态剪掉后同业务键新开的一笔换了生命周期，上一笔的回执与 tombstone 对不上它，上一笔迟到的结果在 `round.result` 按“分区里没有这笔”计 `UnknownSaga`，不进 `judgeCompletion`（RV-20261030-01）。没有采用“在 `newStartRecord` 里沿用启动尝试标识”：`StartAttempt` 是发起方给的、长度到 256，而随机标识只由协调器产生、长度固定。回执保留期 ≥ `ReceiptRetentionFactor`（2）× 每步 `Step.RetryWindow()`，由 `CheckReceiptRetention` 在 `Engine.Register`（补齐预算之后）与 `wiring/saga` 的 `checkStepBudgets`（Init 与 `CheckConfigAcrossMods`）检查（RV-20261030-02）；参与方不知道操作的重试计划，所以没有在剪枝处另加保护，超出计划的停顿是已知限制。回归：`step_receipt_test.go`（含 `TestNativeStepReceiptOutlivesEveryAttemptOfTheRetryWindow`）、`TestASagaReopenedUnderTheSameBusinessKeyIsANewOperation`（剪掉后同键新一笔执行、上一笔迟到结果不改新一笔、重启 + Resume 仍回放）、`TestRegisterRefusesStepsWhoseRetryWindowOutlivesTheReceipts`、`TestReceiptRetentionMustCoverEveryStepsRetryWindow`、`TestNativeSagaStepCommitsMutationAndReceiptAtomically`、`TestMongoStepInboxReceiptRules`、`TestRealMongoStepInboxSerializesConcurrentAttempts`。
6. **原生成功的结果数据 = 这一步的命令载荷**（维护者 2026-10-29 决定 A）：`StepReceipt` 不存 Data，`successData` 在 `Complete` 与回放时取命令载荷；同一操作的各次尝试载荷相同，补偿命令带上它。受 `saga.max_payload_bytes` 约束（命令发出前已检查），回执不变大。回归：`TestNativeStepSuccessCarriesItsCommandPayload` 与 `late_success_test.go` 的三处补偿载荷断言（修前三处为红）。
7. **消费者结算**：执行或回放后把结果发到命令所属分区的 `in.res`（MsgID `res.<生效的 CommandID>`），PubAck 之后才 ack；过期未执行直接 ack；步骤体出错或发布失败 nak。在途处理数原生 `min(32, MaxAckPending)`、Mongo 1。回归：`TestNativeStepConsumerPublishesToThePartitionThenAcks`、`TestConsumerWorkersBoundExecutionAndDeferSettlement`。
8. **准入只拦新启动**：参与方 `LoadPublisher` 把本进程 DataEngine 投影积压写进 LoadBucket，所有者 Watch、滞回、本机时钟判停更、初始化 fail-closed；被拒的启动返回 `ErrBackpressure`（这次没有建立，同一请求重试），已运行的 saga 不受影响。回归：`admission_test.go`、`TestRealJetStreamLoadPublisherDrivesAdmission`。
9. **配置规则写一次**：`wiring/saga` 的配置声明（结构体 + tag）在 Init、`app.CheckConfig`、`--check-config` 与 doctor 共用；跨字段规则（每流 ≤ 50 分区、`stream_groups ≤ partitions`、结果未知预算 ≥ 21s 且大于发布超时、去重窗口 ≥ 2 × 预算、终态保留 ≤ 回执保留、准入滞回与停更、步骤超时 < `DefaultStepAckWait`）在 `ValidateConfig`；要用到定义的规则（解析后每步超时 < `DefaultStepAckWait`、`receipt_retention` ≥ 2 × 每步重试窗口）在 `checkStepBudgets`，Init 与 `CheckConfigAcrossMods` 都调它，生产额外要求 `saga.replicas=3` 与协调器进程 `singleton.enabled`；跨 Mod 规则（效果流键一致、准入开启时 `dataengine.projection.max_unacked_records ≥ 4 × saga.admission.high_records`）在 `CheckConfigAcrossMods`。回归：`TestSagaConfigRulesNameTheirKeys`、`TestSagaProductionRules`、`TestConfigCheckRequiresAProjectionCapAboveTheAdmissionWatermark`。
10. **冷副本不进任何判定（第 6 批，设计 §8）**：`Engine.Get` / `GetByBusinessKey` 只在所有者回答“内存里没有”时回落 `ColdCopy.Get`（所有者不可用仍是 `ErrPartitionUnavailable`）；`List` 把冷副本一页的 ID 随散播发给所有者（`queryRequest.Known`），所有者回答其中在内存里的（`queryReply.Held`），这些 ID 不用冷副本那条，结果截到冷副本已读的最后一条（精确前缀，最多 4 页）。启动判定补查（`submitIntent`）与 `WaitTerminal` 用 `getOwned`，只看内存。投影器（`ColdCopyProjector`，只在拥有分区的进程）每条流一个 R1 内存 durable，攒批 ≤ `batch_size` / `max_wait`，同一 saga 取最新（`coldNewer`：先比 `created_at` 再比版本），一次 BulkWrite，每条是 `coldCopyNewer` 门控的 `$replaceWith` 管道 upsert；**每条投递由写它的那次 BulkWrite 逐条结算**（推迟结算 `DeferSettle`，写成功 ack、失败 nak），不用 AckAll。清理守卫：`partition.purge` 在冷副本开启时把缺失的 `-cold-copy` durable 按 floor 0 计（不清理），投影器每 5s 核对、缺了就重建订阅。回归：`TestColdCopyProjectorWritesTheNewestRecordPerSagaAndSettlesEachDelivery`、`TestColdCopyProjectorNaksEveryDeliveryOfAFailedBatchAndRedeliveryIsIdempotent`、`TestGetFallsBackToTheColdCopyOnlyWhenTheOwnerLacksTheRecord`、`TestListAddsOnlyColdRecordsTheOwnersDoNotHold`、`TestStartVerdictPollIgnoresTheColdCopy`、`TestWaitTerminalIgnoresTheColdCopy`、`TestColdCopyDurableGuardsThePartitionPurge`；真实 `TestRealMongoColdCopyVersionGate`、`TestRealJetStreamColdCopyKill9Converges`、`TestRealJetStreamColdCopyKeepsUpWithoutBlockingThePurge`。
11. **遗留 Mongo 事务（设计 T-A）**：被杀进程的事务持锁到 Mongo `transactionLifetimeLimitSeconds` 才回收（服务端每半个参数扫一次，最晚 1.5 倍）。部署要求 20s；参与方 `Steps.Start` 只读探测并在超过缺省步骤预算窗口时告警（`probeTransactionLifetime`，不拒绝启动）；DataEngine 启动排空在 `dataengine.startup_timeout`（缺省 60s，第 5 批维护者决定）内退避重试（`Runtime.drainAtStartup` / `startupDrainContext`：给了期限只用它，调用方没给期限时上限 2 分钟）。回归：`TestStepsProbeTheMongoTransactionLifetimeAgainstTheStepWindow`、`TestAssemblyStartupDrainRetriesALeftoverTransactionWithinTheDeadline`、`TestStartupDrainDeadlineFollowsTheCallerAndCapsOnlyWithoutOne`、`TestDataEngineStartupTimeoutDefaultOutlastsALeftoverTransaction`、真实 `TestRealMongoStartupDrainOutlastsALeftoverTransaction`。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认分别给出错误与收尾责任。分区状态只在分区循环 goroutine 里读写；查询与判定回复经队列进循环，不加锁读状态。改变公开类型、配置或线格式（`WireVersion`）时，同时更新生成器、调用方与使用篇；线上尚未部署期间不做旧格式兼容，版本号加一、旧版本拒绝。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。分区判定的单元用例用 `unitPartition` / `simEngine`（同步 decide + install，不发布），真实发布、隔离、重放走三节点私有集群（`scripts/nats-cluster.sh test`）。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### saga

| 源码 | 职责（导出类型） |
| --- | --- |
| [engine.go](../../../framework/saga/engine.go) | `Options`、`DefaultOptions`、`Engine`（生命周期、发起方 API `StartSaga` / `Complete` / `Resume` / `Compensate` / `Get` / `GetByBusinessKey` / `List`、`Stats`、`FailureCause`） |
| [assembly.go](../../../framework/saga/assembly.go) | `AssemblyConfig`、`Assembly`（协调器 + 启动中继，停机先排空中继再停协调器） |
| [inspect.go](../../../framework/saga/inspect.go) | `InspectPartitions`：离线重放全部分区（合服检查用） |
| [partition.go](../../../framework/saga/partition.go) | 分区循环：判定（`decide`）、发布与结果未知判定（`publishChunk`）、提交（`install`）、查询应答、剪枝 |
| [partition_replay.go](../../../framework/saga/partition_replay.go) | 重放、快照、清理与清理卡住告警 |
| [transition.go](../../../framework/saga/transition.go) | 纯状态机（`judgeCompletion`、步骤转换、`completionHistory`） |
| [wheel.go](../../../framework/saga/wheel.go) | 分区时间轮 |
| [log_layout.go](../../../framework/saga/log_layout.go) | `Layout`（分区 / 流 / subject / 桶名，`Validate` 含每流 ≤ 50 分区）、`IDOf` |
| [log_wire.go](../../../framework/saga/log_wire.go) | `WireVersion` 3 的事件、输入、判定、查询、快照清单与正文的编解码与校验 |
| [query.go](../../../framework/saga/query.go) | 判定回复路由、意图追加与等判定、`Get` / `List` 的请求与所有者订阅 |
| [terminal_watch.go](../../../framework/saga/terminal_watch.go) | `WaitTerminal` 与终态观察者 |
| [admission.go](../../../framework/saga/admission.go) | `AdmissionOptions`、`AdmissionState`、`OpenLoadBucket`、`LoadPublisher` |
| [record.go](../../../framework/saga/record.go) | `Status`、`Phase`、`Step`、`StepBudget`、`StepBudgets`、`Definition`、`Record`、`Command`、`Completion`、`StartRequest`、`ResumeRequest`、`Query`、`OperationClosure` |
| [observe.go](../../../framework/saga/observe.go) | 重开、迟到补偿、旧尝试 / 旧代际 / 放弃后到达的日志与计数 |
| [step_receipt.go](../../../framework/saga/step_receipt.go) | `StepReceipt`、`StepReceipts`、`StepReceiptHost`、`StepOutcome`、`RunNativeStep`；回执编码、`Begin` / `Complete`、`successData` |
| [native_step.go](../../../framework/saga/native_step.go) | `NativeStep`、`NativeStepBody` |
| [step_consumer.go](../../../framework/saga/step_consumer.go) | `StepConsumerConfig`、`StepHandler`、`SubscribeNativeStep`、`SubscribeMongoStep`、`publishCompletion`、`decodeCommandEvent` |
| [mongo_step_inbox.go](../../../framework/saga/mongo_step_inbox.go) | `MongoStepInbox`（M-A：按操作一个回执文档） |
| [nest.go](../../../framework/saga/nest.go) | `EmitStart`、启动 effect 编解码 |
| [nest_start_consumer.go](../../../framework/saga/nest_start_consumer.go) | `NestStartConsumerConfig`、`Starter`（启动中继：效果流 → 分区 `in.start`） |
| [cold_copy.go](../../../framework/saga/cold_copy.go) | `ColdCopySettings`、`ColdCopy`（冷副本读写、版本门 `coldCopyNewer`）、`ColdCopyProjector`（每流一个 R1 内存 durable、攒批、逐条结算、durable 丢失后重建） |
| [errors.go](../../../framework/saga/errors.go) | 对外错误（`ErrBackpressure`、`ErrStartUnconfirmed`、`ErrPartitionFenced` 等） |

### wiring/saga

| 源码 | 职责 |
| --- | --- |
| [config.go](../../../wiring/saga/config.go) | 配置声明（含 `saga.cold_copy.*`）与 `ValidateConfig`、`ownedPartitions`（P-A） |
| [mod.go](../../../wiring/saga/mod.go) | saga Mod（Writer = `<sid>-<单实例锁 incarnation>`；冷副本开启时取 Mongo 能力）、`CheckConfigAcrossMods`（含“冷副本要 Mongo Mod”）、`ParticipantSettings`（含 `StepWindow`） |
| [steps.go](../../../wiring/saga/steps.go) | `Steps`：登记、订阅、准入信号、`RunHandedOver`、`probeTransactionLifetime`、Drain / Stop 与指标 |
| [step_budgets.go](../../../wiring/saga/step_budgets.go) | `saga.step_defaults` / `saga.steps` → `coresaga.StepBudgets` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。第 4 批的迁移映射（每个删除的用例由哪个保留用例覆盖）见 [设计 §13.3](../../maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md#133-第-4-批实施记录)。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### saga（单元）

- [transition_test.go](../../../framework/saga/transition_test.go)：`TestTransitionRulesMatchTheRecordedRedisCoordinator`（金样 `testdata/transition_golden.json`）
- [jetstream_coordinator_test.go](../../../framework/saga/jetstream_coordinator_test.go)：分区判定、身份、准入回复、坏输入、剪枝、Resume 拒绝、选项校验、终态等待、回复路由、清理界；`unitPartition` / `simEngine` 测试驱动与 `checkTimers`
- [engine_test.go](../../../framework/saga/engine_test.go)、[engine_state_machine_test.go](../../../framework/saga/engine_state_machine_test.go)：正向、补偿、幂等启动、定义版本、截止、重试、Resume 代际、运维拒绝
- [completion_test.go](../../../framework/saga/completion_test.go)、[late_success_test.go](../../../framework/saga/late_success_test.go)、[native_step_incarnation_test.go](../../../framework/saga/native_step_incarnation_test.go)、[native_step_operation_test.go](../../../framework/saga/native_step_operation_test.go)：结果判定、迟到成功、重开、代际、tombstone、原生步骤与协调器交错
- [step_receipt_test.go](../../../framework/saga/step_receipt_test.go)、[nest_atomic_test.go](../../../framework/saga/nest_atomic_test.go)：实体内回执
- [step_consumer_test.go](../../../framework/saga/step_consumer_test.go)、[consumer_test.go](../../../framework/saga/consumer_test.go)：消费者结算、配置拒绝、坏信封、准入、过期、在途上限；Mongo 收件箱规则
- [start_contract_test.go](../../../framework/saga/start_contract_test.go)、[wire_validation_test.go](../../../framework/saga/wire_validation_test.go)、[log_wire_test.go](../../../framework/saga/log_wire_test.go)：启动身份、线格式
- [assembly_test.go](../../../framework/saga/assembly_test.go)、[admission_test.go](../../../framework/saga/admission_test.go)、[wheel_test.go](../../../framework/saga/wheel_test.go)
- [cold_copy_test.go](../../../framework/saga/cold_copy_test.go)：冷副本投影攒批 / 版本只升 / 逐条结算 / 写失败逐条 nak / 重投幂等 / 停止时 nak，回落读只在内存没有时、`List` 排除内存持有的 ID 并翻页，判定补查与 `WaitTerminal` 不读冷副本，冷副本 durable 的清理守卫

### saga（真实依赖，`-tags integration`）

- [jetstream_coordinator_real_integration_test.go](../../../framework/saga/jetstream_coordinator_real_integration_test.go)：正向与补偿、结果未知、隔离旧写者、快照清理重放、流满 fail-stop、准入停更、协调器 kill -9、参与方停机重试、迟到成功补偿、装配启停顺序（`ROOST_NATS_CLUSTER_URL`，`scripts/nats-cluster.sh test`）
- [step_participant_real_integration_test.go](../../../framework/saga/step_participant_real_integration_test.go)：端到端、参与方 kill -9（`ROOST_SAGA_JS_RESTART_DELAY` 模拟长停机，协调器 kill -9 用例同一个开关）、Mongo 步骤写冲突、准入信号（需要 Mongo 的在没有 `ROOST_DATAENGINE_IT_MONGO_URI` 时跳过）
- [cold_copy_real_integration_test.go](../../../framework/saga/cold_copy_real_integration_test.go)：真实 Mongo 版本门与字段往返、协调器 kill -9 后冷副本与权威终态最终一致、跟得上时不挡清理与 durable 丢失后重建（要 `ROOST_NATS_CLUSTER_URL` 与 `ROOST_DATAENGINE_IT_MONGO_URI`）
- 变异开关（只在测试里生效）：`ROOST_SAGA_MUTATE=nofence`（隔离用例必须红）、`noreceipt`（端到端与参与方 kill -9 必须红）、`noadmit`（`TestRealJetStreamLoadPublisherDrivesAdmission` 必须红）
- 压测一档：[jetstream_performance_integration_test.go](../../../framework/saga/jetstream_performance_integration_test.go) 的 `TestRealSagaJSScanLevel`（`scripts/perf/saga-js-scan.sh` 逐档调用）
- 基准：[mongo_step_benchmark_test.go](../../../framework/saga/mongo_step_benchmark_test.go)、[mongo_step_benchmark_real_mongo_integration_test.go](../../../framework/saga/mongo_step_benchmark_real_mongo_integration_test.go)、[mongo_step_latency_real_mongo_integration_test.go](../../../framework/saga/mongo_step_latency_real_mongo_integration_test.go)、[jetstream_performance_integration_test.go](../../../framework/saga/jetstream_performance_integration_test.go)

### wiring 与生成器

- [wiring/saga](../../../wiring/saga)：`config_test.go`、`saga_mod_test.go`、`steps_test.go`
- DataEngine 启动排空：[assembly_test.go](../../../framework/dataengine/engine/assembly_test.go) `TestAssemblyStartupDrainRetriesALeftoverTransactionWithinTheDeadline`、[failure_integration_test.go](../../../framework/dataengine/engine/failure_integration_test.go) `TestRealMongoStartupDrainOutlastsALeftoverTransaction`
- 生成工程真实依赖：`codegen/internal/roost` 的 `TestGeneratedSagaNativeStepsAndEffectConsumer`（`scripts/test-demo-generated.sh`）

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。真实协调器用例在私有三节点集群上跑（`ROOST_NATS_CLUSTER_ROOT=<私有目录> bash scripts/nats-cluster.sh start|test|stop|clean`），不要用共享的 nats-server。命令与发布记录见 [维护手册](../../maintenance/README.md)。没有新测量不能改容量承诺，性能结论以 [性能报告](../../performance/README.md) 为准（正式实现的性能验收在第 7 批，入口 `scripts/perf/saga-js-scan.sh` 已就绪、尚未跑正式一轮）。
