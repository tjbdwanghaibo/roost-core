# Saga 协调器权威改为 JetStream 事件日志：函数级实施设计（2026-10-28）

**状态：设计稿，待维护者过目。本文只写设计，不改代码；维护者确认 §14 的选项后按 §13 分批实施。**

依据：

- 维护者 2026-10-28 决定（原话要点，记入[原型设计稿 §17](PROPOSAL-2026-10-27-saga-jetstream-log.md) 第 12 条）：原型指标已够（云 Mongo 约 64ms 往返、准入开、8 通道投影长期约 700 笔/秒，本机约 1200 笔/秒）；Redis 权威路线不要了，只保留 JetStream；不再等 Linux 跨机复测，先出函数级设计、过目后实施；实施时用 JetStream 协调器替换 Redis 协调器存储，同一批删掉 Redis 权威代码、Store Service 的 saga 投影与 `saga.redis.*` 配置，实施期间 main 上的 Redis 版继续可用；Redis 2c 未合入，已在本地 tag `archive/redis-authority-2c` 归档后删除。
- 原型：[PROPOSAL-2026-10-27-saga-jetstream-log](PROPOSAL-2026-10-27-saga-jetstream-log.md)（下文“原型稿 §N”）与 `scripts/perf/saga-jslog-proto/`；压测：[SAGA-JETSTREAM-LOG-PROTO-20261027](../performance/SAGA-JETSTREAM-LOG-PROTO-20261027.md)（下文“报告”）。
- 现行实现：main `e1957c88` 的 `framework/saga`、`framework/storeservice`、`wiring/saga`、`wiring/storeservice`、`wiring/redis/authority.go`、[SAGA.md](../framework/SAGA.md)、[guide/06](../framework/guide/06-saga.md)、[Redis 权威设计 v2](DESIGN-2026-10-26-redis-authority-v2.md)（2a `6709eb90`、2b `a3edef04` 在 main 上）、[KNOWN-LIMITS](KNOWN-LIMITS.md) 的 Saga 节。
- 前提（维护者确认，原型稿 §1）：JetStream R3 文件存储、收到 PubAck 即不丢（整集群同时断电不在范围）；生产 NATS ≥ 2.12（第 1 批核实：最低 2.12.1，见 §3.2 第 2 条）、同子网；etcd 只做发现与小锁；Mongo 不在关键路径，只做异步投影。本文不提议额外的存储或框架级兜底。
- 代码阅读：codebase-memory 项目 `Users-whb-roost-roost-core` 的根是主检出（与本 worktree 同为 `e1957c88`）；`scripts/` 与 `docs/` 不在索引范围，原型代码与文档按源码直接读。函数名、行为以当前源码为准。

## 0. 要点

1. **一句话**：每个分区一个单写者协调循环，状态在内存，推进一次 = 往分区事件 subject 发一条“新状态 + 要派发的命令”的事件，发布带通配期望序号做隔离；参与方把操作回执放在实体里（决定 2A），WAL 落盘后直接把结果发回分区。没有 outbox、没有领取租约、没有 Redis、没有 Store Service。
2. **状态机不重写**：现行 `judgeCompletion` / `applyCompletion` / `retryState` / `nextCompensation` / `stepTransition` 的判定逻辑抽成纯函数，原样驱动分区内存；Resume、代际、人工 Compensate、定义版本、`ManualRequired`、迟到成功补偿（方向 ④）全部保留。原型自己的简化状态机（`machine.go`）不进生产。
3. **按业务键查询**：saga ID 由 `(Type, BusinessKey)` 派生（`IDOf`），业务键查询 = 按派生 ID 查分区所有者内存；唯一性在分区单写者里判定，窗口是终态保留期（§2.3，需选择）。
4. **分区归属走 App 级保证**：分区按配置静态分给协调器 sid，单实例锁保证“一个 sid 一个进程”，期望序号保证“旧进程写不进去”；不实现模块自己的分区租约（§4.6，需选择）。
5. **准入**沿用原型第五轮参数（按记录数表达：高 5000 / 低 2500 条，即两步正向 2500 / 1250 笔），初始化 fail-closed（RV-20261028-03），硬上限用 DataEngine 现有的 `dataengine.projection.max_unacked_records`。
6. **分批**：先补基础设施与新协调器（不接线，main 上 Redis 版照跑），一批切换并删掉 Redis / Store Service / `saga.redis.*`，DataEngine 的 lease fence 机制单独一批删，最后冷副本与性能验收。估计 22～28 个工作日（§15）。

## 1. 目标与非目标

**目标**

- `framework/saga` 的协调器权威从“Redis 脚本 + Store Service 冷副本”换成“JetStream 分区事件日志 + 内存状态 + KV 清单 / 对象存储快照”。
- 参与方（原生步骤与 Mongo 步骤）的跨尝试至多一次，从“操作状态文档 + Reserve 事务 + lease fence + 实体屏障”换成“按操作的回执 + 实体锁（原生）/ Mongo 事务写冲突（Mongo 步骤）串行”。
- 业务 API 尽量保持（`EmitStart`、生成的 `Lookup` / `Start` / `RegisterSteps`、`Steps.Native` / `Steps.Mongo` / `WithAdmit` / `RunHandedOver`），改变的逐个列在 §2。
- 同一批删掉 Redis 权威、Store Service、`saga.redis.*` / `redis_authority.*` / `store_service.*` 配置与相关测试；main 在每批之后可用。

**非目标**

- Remote Entity 不在本次范围。可推广性：Remote 的“单写者 + 版本 fence”与这里同构（每实体一个 subject 的期望序号即版本 CAS，多实体原子写用原子批量），难点在热点读与 Nest WAL 的两阶段边界，另行设计（原型稿 §13）。
- 不做按 sid 的命令路由（`ev.<service>.<sid>`）：沿用现行 `WithAdmit` + `RunHandedOver`，由业务决定归属。
- 不做整集群断电、Sentinel / 跨地域等前提之外的保护；不加 Mongo、etcd 进关键路径。
- 不兼容旧数据与旧进程（线上未部署，§9.4 写升级要求）。

## 2. 对外契约变化

### 2.1 `saga.Engine`

| API | 结论 | 说明 |
| --- | --- | --- |
| `NewEngine(store Store, publisher Publisher, Options)` | **改变** | 改为 `NewEngine(log fnats.JetStreamLog, options Options) (*Engine, error)`；`Store` / `Publisher` 接口删除（§3.3）。只由 `Assemble` 调用，业务不直接构造 |
| `Register(Definition)` | 保持 | 步骤预算补齐、`registerStartDataLimit` 不变 |
| `StartSaga(ctx, StartRequest) (Record, error)` | **语义改变，签名保持** | 追加启动意图到分区 `in.start` 后等分区所有者的判定（§7.2）：接受或同一意图已存在 → 返回记录；同业务键不同意图 → `ErrIdentityConflict`；准入关闭 → `ErrBackpressure`（提示阶段不写意图，写入后被拒同样返回它）；所有者缺定义 → `ErrDefinitionMissing`；ctx 到期仍无判定 → 新错误 `ErrStartUnconfirmed`（`errors.Is` 同时满足 ctx 错误）：意图已持久、结果未知，用同一请求重试是安全的 |
| `StartRequest.ID` | **删除**（选项 ID-A） | saga ID 恒为 `IDOf(Type, BusinessKey)`；`Now` 只作测试钩子保留 |
| `Get(ctx, id)` | 保持 | 由分区所有者从内存回答（§2.3）；终态过了 `saga.terminal_retention` 且冷副本关闭时返回 `ErrNotFound` |
| `GetByBusinessKey(ctx, type, bk)` | 保持 | = `Get(ctx, IDOf(type, bk))`；“还没启动返回 `ErrNotFound`”的语义不变，另加“终态过了保留期也是 `ErrNotFound`（冷副本开启时回落冷副本）” |
| `List(ctx, Query)` | **语义改变** | 向全部分区所有者散播查询并合并；覆盖非终态、保留期内的终态与全部 `ManualRequired`（常驻，§4.5）；有分区没回答时返回 `ErrPartitionUnavailable`（点名分区），不返回残缺结果。更早的终态只在冷副本开启时由冷副本补 |
| `Resume(ctx, ResumeRequest)` / `Compensate(ctx, id, reason, now)` | 保持签名 | 改为向分区 `in.op` 追加运维意图并等判定（与 StartSaga 同一机制）；规则（Completed 不可 Compensate、补偿方向开新一生等）不变。新增：回执已过保留期的记录拒绝 Resume（`ErrReceiptsExpired`，选项 RR-A） |
| `Complete(ctx, Completion) (Record, error)` | **改变** | 改为 `Complete(ctx, Completion) error`：把结果追加到所属分区 `in.res`（MsgID `res.<CommandID>`），只承诺“已持久进入输入”；判定在分区循环里。框架自带的步骤消费者直接发布，不经它；它留给自定义参与方 |
| `WaitTerminal(ctx, id) (Record, error)` | **新增**（可选能力，决定 4） | 等这个 ID 下一个非 Rejected 终态；同一 ID 的多个等待者全部唤醒（RV-20261028-02）。经 `ev.done` 有序消费者（DeliverNew）+ 登记后先查一次当前状态，避免漏掉 |
| `AdmissionState() AdmissionState` | **新增** | 本进程看到的准入状态（提示），健康检查与测试用 |
| `Run` / `Stop` / `Stats` | 保持签名 | `Stats` 去掉 `Conflicts` / `PublishFailures` / `StoreFailures` / `WorkerFailures` 的旧含义，新增分区计数（§3.3） |
| `ErrBackpressure` | 保持 | 来源从“Redis 投影积压 / OOM”改为“参与方投影积压准入关闭”；可重试，已存在的 saga 照常推进 |
| `ErrConflict` | **删除** | 单写者内没有乐观并发冲突；内部重试路径一并删除 |
| `ErrDurabilityUnconfirmed` / `ErrRedisAuthorityMismatch` | **删除** | 换成 `ErrPartitionFenced`（期望序号被别的写者推进）、`ErrPublishUnknown`（结果未知超过预算），二者都经 `RuntimeFailure` fail-stop，不返回给业务 |
| 新错误 | 新增 | `ErrStartUnconfirmed`、`ErrPartitionUnavailable`、`ErrReceiptsExpired`、`ErrSnapshotIncompatible`、`ErrLayoutMismatch`（后两个只出现在启动） |

`Record` 删除 `Lease`；新增 `StartAttempt string`（开启这一轮的启动尝试，§7.3）。`Status` 枚举不变（选项 R-A 下没有 Rejected 状态）。`Query` 不变。`Command` 新增 `Partition uint16`（结果回到哪个分区）；`Completion` 字段不变。`OutboxRecord`、`ClaimRequest`、`ApplyRequest`、`ApplyOutcome`、`Lease`、`CompletionHistory*`、`LateSuccessAlarmStore`、`PublishFunc` 删除。

### 2.2 生成代码与 Nest 侧

| 入口 | 结论 | 说明 |
| --- | --- | --- |
| `saga.EmitStart(StartRequest)` / 生成的 `<saga>.EmitStart(bk, state, deadline)` | 保持 | 启动意图仍是 Nest effect、与实体修改同一条 WAL 记录；由协调器进程的启动中继转发到分区（§7.4） |
| 生成的 `<saga>.Lookup(ctx, engine, bk)` | 保持 | 内部仍是 `GetByBusinessKey`；注释补“终态保留期”（§2.3） |
| 生成的 `<saga>.Start(ctx, engine, bk, state, deadline)` | 保持 | 底层 `StartSaga` 语义见上表 |
| `saga.NativeStep` | **改变** | 新增 `Begin(StepReceiptHost) (replayed bool, err error)`；`Complete(success, reason)` 签名不变，改为写实体内回执（§5.1）。`Begin` 由生成的 Nest 包装调用（选项 N-A），业务 handler 不变 |
| 生成的 `RegisterSteps` / `Steps.Native` / `Steps.Mongo` / `WithAdmit` / `RunHandedOver` | 保持 | 消费者内部换实现；`StepConsumerConfig.ReceiptWaiters` 删除（不再等投影） |
| `saga.EmitCompletion` / `NewCompletionEffect` / `BindCommand` / `CompletionEffectTopicPrefix` / `StepReceiptNamespace` / `SubscribeNestCompletions` | **删除** | 原生步骤的结果不再经 DataEngine effect；参与方在 WAL 落盘后直接发 |
| `dataengine.ErrFencedEntityPending` 对 saga 的含义 | **删除** | 原生步骤不再带 lease fence；DataEngine 侧机制在第 5 批删除（§13） |
| `roost add saga -native -entity X` | **改变** | 额外给 X 的 DAO 加回执字段与适配组件（与 `roost add timer` 同形，§5.1） |

### 2.3 按业务键查询（重点）

现行：`(Type, BusinessKey)` 由 Mongo 唯一索引永久唯一，`GetByBusinessKey` 读 Redis 守卫或冷副本。原型没做。

**设计（选项 ID-A，推荐）**：

```go
// IDOf 是一笔 saga 的 ID：由 (Type, BusinessKey) 决定，所以业务键查询就是按 ID 查，唯一性由分区单写者判定。
// 128 位，十六进制，满足 subject token 约束（不含 '.', '*', '>'、空白，≤128）。
func IDOf(sagaType, businessKey string) string // hex(sha256(type + "\x00" + businessKey))[:32]

// PartitionOf 按 ID 分区（FNV-1a，与原型相同）。
func (l Layout) PartitionOf(id string) int
```

- `StartSaga` / 启动中继 / `EmitStart` 都只给 `(Type, BusinessKey)`；分区所有者收到启动意图时：内存里没有这个 ID → 新建；有且 `StartDigest` 相同 → 判定“已存在”，返回当前记录、不写事件；`StartDigest` 不同 → `ErrIdentityConflict`、不写事件。
- **唯一性窗口 = 终态保留期**：非终态与 `ManualRequired` 永远在内存；Completed / Compensated / Failed 在内存再留 `saga.terminal_retention`（缺省 1h，选项 W-A），之后从内存与快照里剪掉，同一业务键可以再开一笔（新的 v1）。冷副本开启时，`Get` 在内存没有时回落冷副本读（只读，不参与唯一性判定——Mongo 不进启动路径）。
- 内存成本：保留期内的终态笔数 × 单笔记录大小。按 700 笔/秒、记录 1 KiB、1h 算约 2.5 GB（分散在全部协调器进程上）；业务 Data 小、速率低时按比例缩小。这个公式写进 SAGA.md，配置检查不替部署算内存。
- 迟到成功补偿（方向 ④）也受这个窗口约束：终态被剪掉之后才到的放弃后成功，协调器找不到记录，只计 `saga.completion.unknown_saga_total` 并记 ERROR（点名 `saga_id`、`command_id`），不能自动补偿。按 §10 的分析，成功送达的延迟 ≈ 参与方重启 + AckWait，远小于 1h。

为什么不另建业务键索引（选项 ID-B）：索引桶的 Create 是另一次 R3 写、且和分区单写者不在同一个原子边界里（先写索引后写事件之间崩溃要另做修复），而派生 ID 让“唯一性判定”和“状态推进”落在同一个单写者里，没有第二个权威。

## 3. 包与文件

原则：saga 协调器、参与方、启动中继都留在 `framework/saga` 一个包里（按职责分文件）；JetStream 原语放在 `infra/network/nats`（接口）与 `infra/network/nats/driver`（实现，唯一 import nats.go 的地方，现状如此）；DataEngine 只删不加（第 5 批）。不新建 manager / service 层。

### 3.1 新增与改写（`framework/saga`）

| 文件 | 内容 | 主要类型与函数签名 |
| --- | --- | --- |
| `log_layout.go`（新） | 分区布局、subject、流、桶、ID | `type Layout struct{ Prefix, Stream string; Partitions, Groups int }`；`func IDOf(sagaType, businessKey string) string`；`func (Layout) PartitionOf(id string) int`；`StreamOf(k int) string`；`Streams() []string`；`StreamSubjects(g int) []string`；`EventSubject(k int, dest string) string`；`EventFilter(k int) string`；`InputFilter(k int) string`；`StartSubject/ResultSubject/OpSubject(k int) string`；`CommandFilter(topic string) string`；`DoneFilter() string`；`QuerySubject(k int) string`；`ListSubject() string`；`SnapshotBucket/ObjectBucket/LoadBucket() string`；`func (Layout) Validate() error` |
| `log_wire.go`（新） | WireVersion 2 的事件、输入、快照清单编解码与校验 | `const WireVersion uint16 = 2`；`type logEvent struct`；`type sagaState struct{ Record Record; Ops map[string]opTombstone }`；`type opTombstone struct{ Closure OperationClosure; LateAlarms []uint32 }`；`type startInput / opInput struct`；`type snapshotManifest struct`；`func encodeEvent(logEvent) ([]byte, error)`；`func decodeEvent([]byte) (logEvent, error)`；`decodeStartInput / decodeResultInput / decodeOpInput`；`decodeManifest([]byte, Layout, k int) (snapshotManifest, error)` |
| `transition.go`（由 `step_transition.go` 与 `engine.go` 的判定部分改写） | 纯状态机：输入一份记录与 tombstone，算出新状态、要关闭的操作、要派发的命令；不做 I/O | `type machine struct{ definitions func(string, uint32) (Definition, bool); now func() time.Time; maxPayload int }`；`func (m machine) start(prev *sagaState, in startInput, admitted admitVerdict) (decision, verdict)`；`func (m machine) complete(state *sagaState, c Completion) decision`；`func (m machine) due(state *sagaState) (decision, bool)`；`func (m machine) resume(state *sagaState, r ResumeRequest) (decision, verdict)`；`func (m machine) compensate(state *sagaState, reason string, now time.Time) (decision, verdict)`；`func planTransition(before, after Record, t transition) (Record, string /*close*/)`；保留 `judgeCompletion`、`applyCompletion`、`retryOrCompensate`、`retryState`、`beginCompensation`、`compensationState`、`nextCompensation`、`lateForwardStep`、`compensatingLateStep`、`positionedAt`、`openOperation`、`operationKey`、`commandID`、`commandIDIncarnation`、`backoff` |
| `partition.go`（新） | 分区单写者循环（取输入、判定、发布、提交、回复、定时器、查询） | `type partition struct`；`func (p *partition) run(ctx context.Context) error`；`nextBatch(ctx) (batch, error)`；`decide(batch, now time.Time) ([]logEvent, []verdict)`；`publish(ctx, []logEvent) (uint64, error)`；`resolveUnknown(ctx, lastMsgID string) (uint64, bool, error)`；`commit(events []logEvent, lastSeq, maxIn uint64)`；`reply(verdicts []verdict)`；`answer(q query)`；`prune(now time.Time)` |
| `partition_replay.go`（新） | 启动 / 重放、快照、清理 | `func replayPartition(ctx, log fnats.JetStreamLog, layout Layout, k int, m machine) (*partition, error)`；`func (p *partition) snapshotView() snapshotView`；`func writeSnapshot(ctx, log, layout, k int, view snapshotView, prevRev uint64) (uint64, error)`；`func (p *partition) purge(ctx context.Context, snapshotSeq uint64) error`；`func purgeBound(snapshotSeq, lastEv uint64, floors map[string]uint64) uint64` |
| `wheel.go`（从原型移植） | 分区内时间轮 | `type wheel struct`；`newWheel(tick time.Duration, slots int, now time.Time) *wheel`；`schedule(id string, at time.Time)`；`advance(now time.Time, out []string) []string` |
| `admission.go`（新） | 协调器侧准入闸门与参与方信号发布 | `type AdmissionOptions struct`；`type AdmissionState struct{ Open bool; Reason string; BacklogRecords uint64; At time.Time }`；`type admission struct`；`func newAdmission(AdmissionOptions) *admission`；`func (a *admission) run(ctx, kv fnats.LogKV) error`；`evaluate() AdmissionState`；`type LoadPublisher struct`；`func NewLoadPublisher(kv fnats.LogKV, name string, interval time.Duration, read func() uint64) *LoadPublisher`；`(*LoadPublisher) Run(ctx)`；`(*LoadPublisher) Remove(ctx) error` |
| `query.go`（新） | 所有者查询的应答与请求、启动 / 运维判定回复 | `type query struct`；`type verdict struct`；`func (e *Engine) ask(ctx, k int, q query) (queryReply, error)`；`func (e *Engine) askAll(ctx, q query) ([]queryReply, error)`；`func (e *Engine) awaitVerdict(ctx, k int, id, attempt, digest string) (Record, error)` |
| `engine.go`（改写） | Engine 外观：选项、定义注册、对外 API、角色（所有者 / 客户端） | §2.1 的签名；`type Options struct`（§9.2）；`type Stats struct` |
| `terminal_watch.go`（新） | `WaitTerminal` 的 ev.done 观察者 | `type terminalWatch struct`；`func (w *terminalWatch) register(id string) (<-chan Record, func())`；`func (w *terminalWatch) observe(ev logEvent)` |
| `native_step.go`（改写） | 原生步骤：实体内回执 | `type StepReceipt struct`；`type StepReceipts interface`；`type StepReceiptHost interface{ SagaStepReceipts() StepReceipts }`；`type NativeStep struct{ run *nativeRun }`；`func (step NativeStep) Begin(host StepReceiptHost) (bool, error)`；`func (step NativeStep) Complete(success bool, reason string) error`；`func (step NativeStep) Command() Command`；`type NativeStepBody`；`func RunNativeStep(ctx, command Command, receiptRetention time.Duration, body NativeStepBody) (stepOutcome, error)` |
| `step_consumer.go`（由 `command_consumer.go` 改写） | 原生 / Mongo 步骤消费者：每条流一个 durable，执行后发结果到分区 `in.res`、PubAck 后才 ack | `type StepConsumerConfig struct`（删 `ReceiptWaiters`）；`func SubscribeNativeStep(ctx, js fnats.IJetStream, layout Layout, config StepConsumerConfig, body NativeStepBody) ([]fnats.IJetStreamSubscription, error)`；`func SubscribeMongoStep(ctx, js, layout, inbox *MongoStepInbox, config, handler StepHandler) ([]fnats.IJetStreamSubscription, error)`；`func publishCompletion(ctx, js, layout, c Completion, partition int) error`；`func decodeCommandEvent(*fnats.JetStreamMsg) (Command, error)` |
| `mongo_step_inbox.go`（由 `step_operation_inbox.go` + Mongo 部分改写，选项 M-A） | Mongo 步骤按操作回执 | `type MongoStepInbox struct`；`func NewMongoStepInbox(client fmongo.IMongo, database, collection string, retention time.Duration) (*MongoStepInbox, error)`；`EnsureInfrastructure(ctx) error`；`Handle(ctx, command Command, handler StepHandler) (Completion, stepVerdict, error)`；`Replay(ctx, command Command) (Completion, bool, error)` |
| `nest.go`（瘦身） | 只剩启动 effect | 保留 `EmitStart`、`NewStartEffect`、`DecodeStartEffect`、`StartEffectTopic`、启动 Data 上限表；删除 completion effect 与 `BindCommand` |
| `nest_start_consumer.go`（改写） | 启动中继：effect → 分区 `in.start` | `func SubscribeNestStarts(ctx, js fnats.IJetStream, config NestStartConsumerConfig, starter Starter) (fnats.IJetStreamSubscription, error)`（签名不变，`Starter.StartSaga` 语义见 §7.4） |
| `cold_copy.go`（由 `mongo_store.go` 改写） | 可选冷副本：投影器与只读读者 | `type ColdCopy struct`；`func NewColdCopy(client fmongo.IMongo, database, collection string) (*ColdCopy, error)`；`Get / List`；`type ColdCopyProjector struct`；`func (p *ColdCopyProjector) Start(ctx, log, layout, terminalOnly bool) error`；`Stop(ctx) error` |
| `assembly.go`（改写） | 装配与生命周期 | `type AssemblyConfig struct{ Engine Options; Layout Layout; Stream LogStreamSettings; Starts NestStartConsumerConfig; ColdCopy *ColdCopySettings; Failure *app.RuntimeFailure }`；`func Assemble(log fnats.JetStreamLog, js fnats.IJetStream, raw fnats.RawClient, mongo fmongo.IMongo, cfg AssemblyConfig, definitions ...Definition) (*Assembly, error)`；`Start(ctx) error`；`Stop(ctx) error`；`Health() health.Result` |
| `inspect.go`（新） | 离线只读重放（sidmerge 用） | `func InspectPartitions(ctx, log fnats.JetStreamLog, layout Layout) ([]Record, error)` |
| `record.go` / `errors.go` | 类型与错误 | 按 §2.1 增删 |

### 3.2 `infra/network/nats`（接口）与 `driver`（实现）

**第 1 批已实现**（提交见 git log）。`infra/network/nats/jetstream_log.go`（接口）与 `driver/jetstream_log.go`（实现，driver 仍是唯一 import nats.go 的生产代码），只放分区日志需要的原语，不泄漏 nats.go 的类型。最终形态（完整注释以源码为准）：

```go
type JetStreamLog interface {
	EnsureLogStream(ctx context.Context, config LogStreamConfig) (LogStreamInfo, error) // 建或更新后按生效配置核对：文件存储、AllowAtomicPublish、AllowDirect、Discard=New、MaxAge=0、副本数、去重窗口；服务端 < 2.12.1 拒绝
	PublishFenced(ctx context.Context, msgs []LogMsg, fence LogFence) (LogPublishAck, error) // 1 条普通发布、多条原子批量；首条带通配期望序号；每条 MsgID 必填；只发一次，重发由调用方做
	LastMsg(ctx context.Context, stream, filter string) (LogMsg, error)                    // 流 leader 回答（STREAM.MSG.GET，不走 direct get）；没有 → ErrLogNotFound
	ReadRange(ctx context.Context, stream string, filters []string, from, to uint64, fn func(LogMsg) error) error // to = 0 读到追上为止
	Follow(ctx context.Context, stream string, filters []string, from uint64, fn func(LogMsg)) (LogFollow, error)  // from = 0 只要新消息（WaitTerminal 用）
	Purge(ctx context.Context, stream, filter string, below uint64) error
	DurableAckFloors(ctx context.Context, stream string) (map[string]uint64, error)
	KeyValue(ctx context.Context, config LogKVConfig) (LogKV, error)       // Get / Create / Update / Put / Delete / Watch(onEntry, onReady)
	Objects(ctx context.Context, config LogObjectConfig) (LogObjects, error) // Put / Get（校验 SHA-256）/ Delete
	RequestMany(ctx context.Context, subject string, data []byte, more func([]byte) bool) error
	ServerVersion() string
}
type LogMsg struct{ Subject string; Data []byte; MsgID string; Headers map[string]string; Seq uint64 }
type LogFence struct{ Filter string; LastSeq uint64 } // Filter 为空表示不带期望序号（仅测试变异）
type LogFollow interface{ Stop(ctx context.Context) error; Done() <-chan struct{} }
const MinLogServerVersion = "2.12.1"
const LogBatchMaxMessages = 1000
var (
	ErrLogWrongLastSequence // 10071 / 10164
	ErrLogDuplicateBatch    // 10201
	ErrLogBatchIncomplete   // 10176（缺号、批超时、同时打开的批数超限）
	ErrLogBatchTooLarge     // > LogBatchMaxMessages（发送前）或 10199
	ErrLogRejected          // 其余服务端明确拒绝
	ErrLogOutcomeUnknown    // 没收到答复，可能已落库
	ErrLogNotFound          // LastMsg / KV / 对象不存在
	ErrLogKVConflict        // KV Create 已存在 / Update 修订号不符（R1 10071、R3 10164 都归这里）
	ErrLogUnsupportedServer // 版本或生效配置不满足
)
```

**结果分类**（调用方据此写 §4.3 的重发判定）：返回 nil = 已持久（单条可能 `Duplicate`）；`errors.Is(err, ErrLogOutcomeUnknown)` = 可能已落库，同消息同 fence 重发；其余错误 = 这次调用确定没写入。驱动里没有重试循环，“结果未知预算”与 fail-stop 留在 saga 分区（第 2 批）。

**相对本节初稿的调整与理由**：

1. **哨兵错误多了 6 个**（`ErrLogBatchTooLarge`、`ErrLogRejected`、`ErrLogOutcomeUnknown`、`ErrLogNotFound`、`ErrLogKVConflict`、`ErrLogUnsupportedServer`）：初稿只列了三个服务端错误码，调用方无法区分“没收到答复、可能已写”与“服务端拒绝、确定没写”，leader kill 下的判定（§4.3）正依赖这个区分；KV 的冲突在 R1 / R3 上错误码不同（10071 / 10164），nats.go 的 `ErrKeyExists` 只认 10071，统一成一个哨兵。
2. **最低版本是 2.12.1，不是 2.12**：按 v2.12.0 源码，原子批拒绝批内 `Nats-Msg-Id`（10177 unsupported header），批重复的 10201 从 2.12.1 起才有（来源见 §16；2.12.0 未实测，本机只有 2.14.5）。`EnsureLogStream` 强制。开头“前提”里的“生产 NATS ≥ 2.12”应读作 ≥ 2.12.1。
3. **`LastMsg` 走 leader 读**：日志流开 AllowDirect 时 nats.go 的 `GetLastMsgForSubject` 走 direct get，可能由落后的副本回答旧消息；重放终点或结果未知判定读到旧值会导致误判为被隔离（多一次 fail-stop）。驱动改用 `$JS.API.STREAM.MSG.GET`。
4. **`Follow` 的 ctx 只约束建立**，句柄 `LogFollow{Stop(ctx), Done()}` 按三步停机（`Stop` 在 ctx 内等回调返回，超时可再调用），初稿没写句柄形状。`ReadRange`/`Follow` 用有序消费者的 `Messages().Next()` 在一个 goroutine 里顺序回调，避免 `Consume` 回调在消费者重建时与旧回调并发、乱序。
5. **`LogKVConfig.LimitMarkerTTL`**：KV 的 MaxAge 过期是静默的（Watch 看不到），§6 准入“键过期清除 → 该参与方下线”需要它（服务端在过期时写删除标记，实测 Watch 能收到）。
6. **`JetStreamPublishOptions.Headers`** 按初稿实现；以 `Nats-` 开头的键（保留给驱动与服务端）发布时拒绝，`LogMsg.Headers` 同样规则、读回时不含保留头。
7. **`wiring/nats` 发布能力 `mods.ModNatsLog`（`nats.log`）**，值是 `Assembly.Log`，与 `nats.jetstream` 同一连接。

### 3.3 删除

| 范围 | 文件 | 批次 |
| --- | --- | --- |
| Redis 权威 | `framework/saga/redis_store.go`、`redis_codec.go`、`redis_scripts.go`、`redis_startup.go`、`redis_projection.go`、`redis_lua/`（全部 lua）、`store.go`（Store / Publisher 等接口）、`jetstream.go`（`JetStreamPublisher`、`SubscribeCompletions`；`withConsumerWorkers`、`validDeliveryLimits`、`validSubjectPath` 移到 `step_consumer.go`） | 第 4 批 |
| 原生步骤旧收件箱 | `dataengine_step_inbox.go`、`step_operation_inbox.go`、`nest_completion_consumer.go`；`nest.go` 的 completion effect 与 `BindCommand` | 第 4 批 |
| Store Service | `framework/storeservice/`（`runner.go`、`projection.go`、`lua/`）、`wiring/storeservice/`；`mods.ModStoreService` | 第 4 批 |
| 权威 Redis 接线与基础设施（选项 D-A） | `wiring/redis/authority.go`（`AuthorityMod`、`LoadAuthorityConfig`、`NewAuthorityClient`）、`mods.ModRedisAuthority`；`infra/storage/redis/authority.go`（`CheckAuthority`）、`client.go` 里的 `DurableScriptBatcher` / `ScriptCall` / `ScriptResult` / `ErrWaitAOFFailed`、`driver/durable_script.go`、`driver/stream.go` 里只为 Store Service 加的方法、`lua_script.go`、`redistest/aof.go`、`scripts/it-redis-aof.sh`、`scripts/mirror-local.sh` 的 AOF 段 | 第 4 批 |
| 生成器 | `codegen/internal/roost/catalog.go` 的 `redis_authority`、`store_service` 条目与 saga 的依赖、`render.go` 开发环境 Redis 按权威契约起的分支、`render_docs.go` 的两行、`kitconfig_gen.go` 快照（重新生成） | 第 4 批 |
| DataEngine lease fence（选项 L-A） | `framework/dataengine/lease_fence.go`、`engine/fenced_step.go`，投影器里 fence 跳过 / 驱逐 / 屏障分支与 `FencedEntities` / `FencedAdmissionRejected` / `StaleEvictions` / `dataengine.fence.*` 指标、`ErrRemoteLeaseFenceUnsupported` / `ErrRetiredRemoteLeaseFenceRecord` | 第 5 批 |
| 旧压测脚本 | `scripts/perf/saga-scan.sh`、`saga.sh`、`saga-claim-explain/`（Mongo / Redis 领取对照），由新脚本取代；原型目录 `scripts/perf/saga-jslog-proto/` 在第 7 批验收后删除（结论已在报告里） | 第 4 / 7 批 |

## 4. 流与 subject、线格式、单写者、批、快照、定时器、选主、分区

### 4.1 布局（与原型的差异逐项写明）

| 项 | 正式实现 | 原型 | 差异原因 |
| --- | --- | --- | --- |
| 分区数 P | `saga.partitions`，缺省 16 | 4 | 16 让 2～8 个协调器进程分得开；本机 P=1/4/16 吞吐同量级（报告 §0 第 4 条） |
| 流数 G | `saga.stream_groups`，缺省 4，范围 1～8 且 ≤ P（决定 3A） | 4 | 同 |
| 流名 | `<saga.stream>_G<g>`，G=1 时也带 `_G0` | G=1 时无后缀 | 名字不随 G 变形 |
| 流的 subjects | 恒为本流各分区的 `<prefix>.<k>.>` 列表（G=1 也不用 `<prefix>.>`） | G=1 用 `<prefix>.>` | 查询 subject `<prefix>.q.*` 不能被流捕获 |
| 流配置 | R=`saga.replicas`（生产强制 3）、文件存储、`AllowAtomicPublish`、`AllowDirect`、去重窗口 `saga.duplicate_window`、**`MaxAge=0`、`Discard=New`**、`MaxBytes=saga.stream_max_bytes` | MaxAge 未设 | 权威日志不能按时间或容量静默删旧；满了拒新写（协调器 fail-stop、告警），只由清理删除 |
| 事件 subject | `<prefix>.<k>.ev.cmd.<topic>`（带命令）、`.ev.done`（终态）、`.ev.none`（无命令的变化） | `ev.acct` / `ev.done` / `ev.none` | 按步骤主题路由到现行每主题一个 durable |
| 输入 subject | `<prefix>.<k>.in.start`、`.in.res`、`.in.op`（Resume / Compensate） | 无 `in.op` | 运维操作也要经单写者 |
| 查询 subject（核心 NATS，不进流） | `<prefix>.q.<k>`（Get / 判定补查）、`<prefix>.q.list`（散播） | 无 | §2.3 |
| KV / 对象桶 | `<STREAM>_SNAP`（键 `p<k>` 快照清单、键 `layout` 布局元数据）、`<STREAM>_SNAPOBJ`（对象存储，快照正文）、`<STREAM>_LOAD`（准入信号与 `admit`） | `_SNAP`（整份快照）、`_OWN`（租约）、`_LOAD` | 快照分块（KV 值受 `max_payload` 约束，缺省 1 MiB）；没有分区租约（§4.6） |

**布局元数据**：第一个启动的所有者 `Create` 键 `layout`（`{v, partitions, groups, prefix}`）；之后每个进程启动时比对，不一致返回 `ErrLayoutMismatch` 拒绝启动。改 P / G 只能在全部 saga 终态、停全部协调器、删流与桶之后（写进运维手册），不支持在线改。

### 4.2 线格式（WireVersion 2）

- 事件 `logEvent{V, Part, Writer, In, At, Kind, Saga sagaState, Command *Command}`：`Saga` 是这笔 saga 的完整新状态（`Record` 去掉 `Lease`，加 `StartAttempt`；`Ops` 是操作 tombstone：关闭方式与已告警的代际），`Command` 是要派发的一次尝试（现行 `Command` + `Partition`）。`Kind` 取值：`start`、`dispatch`、`result`、`timeout`、`deadline`、`late_success`、`late_alarm`、`resume`、`compensate`、`definition_missing`、`invalid_step`。
- 输入：`startInput{V, Request StartRequest(无 ID), Attempt, Digest}`（MsgID `start.<id>.<attempt>`，头 `Roost-Saga-Reply`）；`in.res` 是 `completionEnvelope{V, Completion}`（MsgID `res.<CommandID>`）；`opInput{V, Kind, ID, Resume *ResumeRequest, Reason, Now, Attempt}`（MsgID `op.<id>.<attempt>`，同样带回复头）。
- 快照清单（KV）`snapshotManifest{V, Part, Layout{P,G,Prefix}, Seq, LastEv, InHi, Writer, At, Object, Size, SHA256}`；正文（对象存储）是 `[]sagaState` 的 JSON。
- 编码用 JSON（与原型、现行信封一致）；单条事件上限按 `saga.max_payload_bytes`（Data）× 2（Data 与命令载荷）+ 固定开销估，超出 `max_payload` 的启动意图在 `NewStartEffect` / `StartSaga` 就以 `ErrInvalidRecord` 拒绝（现行规则不变）。
- 不兼容 WireVersion 1：所有解码按版本精确匹配；坏输入（解码失败、版本不对、内容校验不过、缺 `Attempt`）计 `saga.partition.rejected_input_total{kind,reason}` 并跳过，`InHi` 照常前进（日志里的输入不能 Term）。

### 4.3 单写者与隔离（同原型 §5，差异见注）

- 分区内存记 `lastEv`（过滤 `<prefix>.<k>.ev.>` 上最后一条事件的流序号）。每轮发布的首条带 `Nats-Expected-Last-Subject-Sequence-Subject: <prefix>.<k>.ev.>` 与 `Nats-Expected-Last-Subject-Sequence: lastEv`；**PubAck 之后才改内存**（`commit`），`lastEv` 取 PubAck 序号（批取末条）。
- 被拒且过滤上最后一条不是自己的 → `ErrPartitionFenced`。**差异**：原型只停这个分区；正式实现交给 `RuntimeFailure.Fail`，整个进程 fail-stop（App 级统一保证：出现第二个写者只可能是崩溃重启的短暂重叠或部署错误，旧进程应该整体停下）。
- MsgID：`<writer>.<lastEv>.<轮号>.<i>`，`writer` = `<sid>-<SingletonIncarnation.Token 或进程随机串>`；结果未知时同 MsgID、同期望序号重发，判定规则同原型 §2：单条成功或 `Duplicate` = 已提交；批成功 = 已提交，10201 或“最后一条是自己的” = 之前已提交；序号不符且最后一条不是自己的 = 被隔离。重试总时长 `saga.publish_unknown_budget`（缺省 30s，必须 > 选主停顿上界 7s 的 3 倍），超过 → `ErrPublishUnknown` → fail-stop。去重窗口必须 ≥ 2 × 这个预算（配置检查）。

**第 2 批落地（§13.1 第 3、4、5 条）**：MsgID 实际是 `<writer>.<分区>.<轮首 lastEv>.<轮号>.<i>`（分区号必须在里面）；10176 立即重发、不算结果未知；`ErrLogRejected`（流满等）不重试，`ErrPartitionRejected` fail-stop；驱动把 10158 归为结果未知。

### 4.4 批（同原型 §6）

一轮取至多 `saga.max_batch`（缺省 256）条输入加到期定时器与待答查询，在内存副本（overlay，写时复制）上算完，事件一次发布：1 条普通发布、多条原子批量，单批超过服务端上限时按上限切成几次顺序 fenced 发布。一个分区同时只有一次发布在途；分区之间并行（每分区一个 goroutine）。判定回复（§7.2）在 `commit` 之后发出；不写事件的判定（已存在、身份冲突、拒绝、缺定义）在本轮结束时发出。

### 4.5 快照、保留与清理

- **何时**：每 `saga.snapshot_every_events`（缺省 2000）条事件或 `saga.snapshot_interval`（缺省 30s，且自上次以来有事件）。
- **差异：快照不阻塞循环**。原型在循环里同步编码与写 KV；正式实现在循环里只做一次浅拷贝（已提交的 `*sagaState` 是写时复制、提交后不再修改，所以浅拷贝就是一致视图），交给分区自己的快照 goroutine 编码、写对象、CAS 清单；同一分区同时最多一个快照在途，在途时到点就跳过。
- **写法**：先 `PutObject("p<k>-<seq>")`，再按修订号 CAS 清单（首次 `Create`）；CAS 冲突时读回清单，`LastEv` 比自己新 → `ErrPartitionFenced`（fail-stop），否则在其上覆盖。成功后删上一个对象。`Seq` 取输入水位 `InHi`（原型评审时的修正，原型稿 §8）。
- **保留**：快照与内存剪掉“终态（Completed / Compensated / Failed）且 `UpdatedAt` 早于 `now - terminal_retention`”的记录；**`ManualRequired` 与非终态常驻**（等运维 Resume；它们也是迟到成功补偿的对象）。剪枝在循环里按每轮至多 1024 条做，避免一次停顿。
- **清理**：`purge(filter=<prefix>.<k>.>, below = min(InHi+1, LastEv, 每个守卫 durable 的 ack floor + 1))`，保留最后一条事件（期望序号依赖它）。守卫 = 本流上全部 durable 消费者（步骤消费者、开启时的冷副本消费者），由 `DurableAckFloors` 列出；有序 / 临时消费者不算。某个 durable 的 ack floor 卡住超过 `saga.purge_stall_alarm`（缺省 10m）计 `saga.log.purge_blocked_seconds{stream,consumer}` 并 WARN 点名；不跳过它（跳过会删掉它还没取的命令）。下线的步骤要由运维删掉它的 durable（写进 SAGA.md）。
- **加载时校验（原型缺）**：`decodeManifest` 校验 `V == snapshotWireVersion`、`Part == k`、`Layout` 与本进程一致、对象长度与 SHA-256 一致，任何一项不符返回 `ErrSnapshotIncompatible`，分区不启动（Mod Start 失败）；**不退回从序号 1 重放**——日志已按快照清理过，从头重放会得到缺一段的状态。

**第 2 批落地（§13.1 第 6 条）**：重放不按版本“只前进”，而是从 `min(InHi, LastEv)+1` 读到 `LastMsg(分区过滤).Seq`（确定终点，第 1 批问题 4），事件只取 `LastEv` 之后的、输入只取 `InHi` 之后的；清单比初稿少 `Seq` 字段（由 `LastEv` / `InHi` 推出）。

### 4.6 分区归属（选项 P-A，推荐）

- `saga.coordinators`（共享键，sid 列表）：本进程 sid 在列表里时，拥有 `{k : k mod len(coordinators) == index(sid)}`（列表排序后取下标）；不在列表里时只有客户端一侧（StartSaga、查询、WaitTerminal、启动中继可选）。
- 进程级唯一由 App 单实例锁保证（`singleton.enabled`；生产配置检查要求协调器进程启用）；崩溃重启的短暂重叠由期望序号隔离、旧进程 fail-stop。没有分区租约，没有自动接管：一个协调器进程挂掉，它的分区停到它重启（与玩家静态绑定同一取舍）；在途 saga 的超时在重放后按截止时刻继续。
- 改 `coordinators` 列表 = 改归属：按运维手册停全部协调器再起（不停也安全——新旧所有者写同一分区时有一方被隔离并 fail-stop——但会有一轮 fail-stop 噪音）。
- 选项 P-B（原型做法：KV TTL 租约 + 自动接管）见 §14。

### 4.7 定时器（同原型 §9）

每分区一个哈希时间轮（`saga.tick` 缺省 5ms、1024 槽），在循环里推进；条目只是“到时看一眼”，状态机按记录当前的 `NextRunAt` / `DeadlineAt` 判定，陈旧条目不改状态。`due` 覆盖现行 `processClaimed` 的全部出口：定义缺失 → ManualRequired、saga 截止 → 补偿、等结果超时 → 重试或补偿、退避到期 → 派发、步骤越界 → ManualRequired。同一轮先处理输入、后处理到期（原型顺序），所以已在 inbox 里的结果先于同轮的超时被接收。原型的轮缺陷（未到期条目推迟一整圈）已修，移植时连同单元断言一起搬。

### 4.8 选主停顿

一条流选主期间（实测 5～7s）这条流上的分区都发不出事件：发布按“结果未知”重试（预算 30s），循环不处理新输入，到期判定顺延到恢复后的第一轮。步骤缺省 `Timeout` 5s 短于停顿：停顿结束后可能先判超时再收到结果，结果是一次多余的重试（新尝试读到回执回放，成功是操作的结论，被接收），不影响正确性。G=4 时一次选主只停 1/4 的分区。不做“停顿后顺延截止”的额外机制（§14 列为可选，不推荐）。

## 5. 参与方：回执放在实体里（决定 2A）

### 5.1 原生步骤

**DAO 与生成**（`roost add saga ... -native -entity X`，与 `roost add timer` 同形）：

- 在 X 的 DAO 定义里加 `SagaReceipts map[string][]byte`（持久、不同步；值是 `saga.StepReceipt` 的框架编码），生成 `game/entities/<x>/saga_receipts_component_gen.go`：组件持有 DAO 句柄，实现 `saga.StepReceipts`（读写都经 DAO 生成的 mutator，随事务提交 / 回滚，符合 A1“回滚统一走 DAO”）；实体生成方法 `SagaStepReceipts() saga.StepReceipts`，从而实现 `saga.StepReceiptHost`。字段类型是否被 DAO 生成器直接支持、存量文档缺字段的 schema 处理，实施时按 `roost add timer` 的同一处理核实（项目未上线，demo 与生成工程重新生成）。
- 框架类型：

```go
// StepReceipt 是一个操作实例（Command.IdempotencyKey）的结论：成功，或某一代际的业务拒绝。可重试失败与过期不留回执。
type StepReceipt struct {
	CommandID   string // 生效的那次尝试
	Incarnation uint32 // 由 CommandID 解析，拒绝只对本代际回放
	Success     bool
	Error       string
	Data        []byte
	At          int64 // unix ms，保留期按它剪
}

// StepReceipts 是实体 DAO 上的回执存储，只在实体的 Nest 锁内调用。
type StepReceipts interface {
	Receipt(operation string) (StepReceipt, bool)
	PutReceipt(operation string, receipt StepReceipt) error
	DeleteReceipt(operation string) error
	RangeReceipts(func(operation string, receipt StepReceipt) bool)
}
```

**执行路径**（`SubscribeNativeStep` → 步骤体 → Nest handler）：

1. 消费者解出事件里的命令（坏信封 Term + `saga.consumer.rejected_total`），过 `Admit`（现行），构造 `nativeRun{command, retention}`，调用步骤体 `body(ctx, NativeStep{run})`；步骤体经生成的 Sender 发 Nest 请求（不变）。
2. **生成的 Nest 包装**（选项 N-A）：handler 参数里有 `saga.NativeStep` 时，`codegen/internal/nest` 在调用业务函数之前插入 `if replayed, err := step.Begin(target); replayed || err != nil { return 零值, err }`，`target` 是第一个锁目标（必须实现 `StepReceiptHost`，否则编译失败）。生成器同时要求该 handler 的 `durability` 是 `strict` 或 `pipelined`（结果在 Nest 返回后直接外发，async 不满足“落盘后才发”）。
3. `Begin`（在事务里、实体锁内）：
   - 有回执且（`Success` 或 `Incarnation == 本命令代际`）→ 记下回放结论、返回 `replayed=true`，不执行业务；
   - 否则（没有回执，或只有旧一生的拒绝）若 `now ≥ command.DeadlineAt` → 记“过期未执行”、返回 `replayed=true`；
   - 否则返回 false，业务执行。
4. 业务调用 `step.Complete(success, reason)`：校验已 `Begin`，写回执（`PutReceipt`，覆盖旧一生的拒绝），同一事务里剪掉至多 16 条 `At < now - receipt_retention` 的旧回执，经 `nest.AfterCommit` 把结论交给 `nativeRun`（只在提交后可见）。现行“业务拒绝也提交”的规则不变。
5. Nest 请求返回 nil（strict：组提交 fsync 之后；pipelined：外化闸门之后）→ 消费者按结论发布：回放 / 本次结论 → `Completion` 发到 `<prefix>.<Command.Partition>.in.res`（MsgID `res.<回执里的 CommandID>`），PubAck 后才 ack 命令；过期未执行 → 直接 ack，计 `saga.step.expired_unexecuted_total`；回放时的命令过期也照样发回执里的成功（现行“过期投递重发成功”规则）。
6. Nest 请求返回错误 → 结果未知（可能已提交）：nak 退避重投；重投时 `Begin` 读到回执就回放。**不再有**租约保留、交还租约与 `ErrFencedEntityPending` 分支。
7. `RunHandedOver`：同一个 `RunNativeStep` 路径，不发布、不 ack（原消息持有者的重投负责）；与消费者的互斥由实体锁 + 回执保证。

与现行的差别（写进 SAGA.md）：生效点从“投影事务的条件写”变为“Nest 事务的 WAL 落盘”；截止检查在事务开始时，WAL 落盘可能晚于截止 → 只能由迟到成功补偿处理（规则不变，频率取决于截止相对 fsync 的余量；原型稿 §11）。“原生步骤不能修改 Remote 实体”仍保留为约束：`Complete` 发现当前事务有 Remote 写时返回 `ErrNativeStepRemoteWrite`（需要 Nest 暴露“本事务是否有 Remote 批次”，实施时核实接口；没有时由生成器在 handler 层面禁止）。

### 5.2 Mongo 步骤（选项 M-A，推荐）

`MongoStepInbox.Handle` 一个 Mongo 事务（`WithTransaction`，现行驱动重试）：

1. 读回执文档 `{_id: IdempotencyKey}`：成功（任何代际）或本代际拒绝 → 回放，不执行；
2. `now ≥ DeadlineAt` → 过期未执行；
3. 执行 handler（事务 ctx）；
4. 写回执：条件 upsert（文档不存在，或存在的是更旧代际的拒绝），写前再查一次截止（与现行“条件在事务最后一次写时检查”同一位置）；条件不满足或已过截止 → 中止事务（业务写回滚），重跑时走第 1 步回放或第 2 步过期。
5. 同一操作的两次尝试并发：两笔事务写同一个回执文档，Mongo 写冲突让后者整体重跑，重跑读到前者的回执回放。业务写随事务回滚，事务外副作用（发邮件）仍必须按 `IdempotencyKey` 幂等（现行要求不变）。

集合：`<收件箱集合>`（缺省 `_<group>_step_inbox`）存新形状的回执文档，TTL 索引 `expires_at = at + receipt_retention`；旧的 `<收件箱集合>_operations` 与按 `CommandID` 的回执不再读写（升级时删）。消费者结果发布同原生步骤第 5 步。删掉的现行机制：Reserve 事务、操作状态文档、租约与 `superseded`、`refusals` 两生保留、`saga.step_inbox.*` 指标。

选项 M-B（保留现行操作状态文档与租约，只改结果发布目的地）见 §14。

### 5.3 参与方投影保持

原生步骤所在进程的 DataEngine 投影不变：`dataengine.projection.workers`（并行投影器，`e11506a4`，缺省 8）、checkpoint 合并、`max_unacked_records` 硬上限（§6）。重启“先恢复再服务”不变：先把 WAL 尾巴投影进 Mongo，再从 Mongo 加载实体（回执随实体文档加载）。

## 6. 准入（原型稿 §12a 迁移）

- **信号**：每个装了原生步骤的进程由 `Steps.Start` 启动一个 `LoadPublisher`：每 `saga.admission.signal_interval`（缺省 200ms）把本进程 DataEngine 投影器的 `ProjectorStats.WALUnacked` 写进 `<STREAM>_LOAD` 的 `part.<service>.<sid>`；`Steps.Drain` 时删除自己的键（优雅下线不算过期）。桶 `MaxAge` 30s（崩溃进程的键最终消失）。没有原生步骤的进程不发布（Mongo 步骤没有投影积压）。
- **判定**（每个协调器进程一份 `admission`，全部分区共用）：watch 整桶；积压 = 全部参与方 `WALUnacked` 的最大值（**按记录数**，不再有“每笔记录数”配置）；`≥ high_records` 关闭，关闭后 `≤ low_records` 才重开（滞回）。
- **过期**：**差异**——原型按参与方写入的时刻判过期（跨机依赖时钟）；正式实现按本进程单调时钟记“最后一次收到该键更新”的时刻，超过 `stale_after`（缺省 2s）→ 关闭，原因 `backpressure_stale_signal`。键被删除 / 过期清除 → 该参与方下线，不再计入。
- **初始化 fail-closed（RV-20261028-03）**：watch 初始快照读完之前关闭，原因 `backpressure_not_ready`；读完且没有任何参与方信号 → 打开；读完时已超高水位 → 按积压关闭。重放与在途推进不等它，只有新启动被拒。
- **只作用于新启动**：所有者处理 `in.start` 时判一次（权威）；关闭 → 判定 `rejected`（选项 R-A：不写事件、不建记录，只回复这次尝试，§7.3）。在途推进、重试、补偿、Resume 不受影响。
- **提示**：所有者把准入状态写进同桶 `admit` 键（变化即写、否则每秒）；`StartSaga` 与启动中继先看 3s 内的提示，关闭则不写意图直接返回 `ErrBackpressure`（中继 nak 退避）。
- **硬上限**：`dataengine.projection.max_unacked_records`（现有键）。saga Mod 的跨 Mod 规则：本进程有 DataEngine 且准入开启时，它必须 > 0 且 ≥ 4 × `high_records`（原型推荐 20000 = 4 × 5000）。到上限时 DataEngine 在 WAL 准入前拒绝，Nest 回滚、命令 nak，协调器按步骤预算重试。
- **缺省值**（原型第五轮推荐值换算成记录数）：`saga.admission.enabled` true、`high_records` 5000、`low_records` 2500、`signal_interval` 200ms、`stale_after` 2s。阈值随部署变（高水位 = 投影器持续速率 × 允许的追平时间），写进 SAGA.md 的调参说明。

**第 2 批落地（§13.1 第 7、8 条）**：LoadBucket 的 `MaxAge` 与 `LimitMarkerTTL` 都取 `AdmissionOptions.SignalTTL`（缺省 30s，第 1 批问题 2）；过期按协调器本机时钟上“最后一次收到更新”判定（问题 5）。`LoadPublisher` 留到第 3 批（参与方一侧），本批用例直接写 KV。

## 7. 发起方

### 7.1 启动尝试身份（RV-20261028-01，设计约束沿用）

每次启动尝试的标识 = `<Engine 实例随机标识（crypto/rand.Text，128 位）>.<实例内序号>`；意图 MsgID = `start.<id>.<attempt>`，全局唯一。同一次 `StartSaga` 里结果未知的网络重发沿用同一标识；第一次发布就拿到 `Duplicate` → `ErrStartIdentityReused`（身份生成坏了的显式失败）。同一 ID 是否已在跑由所有者按 `StartDigest` 判定，不靠 MsgID。运维意图（`in.op`）用同一套标识。

### 7.2 判定的送达（`StartSaga` / `Resume` / `Compensate`）

1. 本地校验（定义、业务键、Data 上限）→ 看准入提示 → 追加输入（带 `Roost-Saga-Reply: _INBOX.<随机>`）。
2. 所有者处理后经核心 NATS 回复这一个尝试：`{attempt, outcome, record}`，`outcome ∈ accepted | existing | identity_conflict | rejected | definition_missing | invalid`；写事件的判定在 `commit` 之后回复。
3. `saga.start_verdict_timeout`（缺省 2s）内没收到回复（回复丢失、所有者在提交后崩溃）→ 每 200ms 向所有者 `Get(id)`：有记录且 `StartDigest` 相同 → 视为已接受，返回它；`StartDigest` 不同 → `ErrIdentityConflict`；没有 → 继续等（意图仍在流里，会被处理）。
4. ctx 到期仍无结论 → `ErrStartUnconfirmed`（意图已持久、结果未知；用同一请求重试安全）。

### 7.3 等待者与终态的启动尝试标识（RV-20261028-02、原型稿 §12b）

- 每条非 Rejected 终态记录都带 `StartAttempt`（开启这一轮的尝试，之后的版本原样带着）。
- **拒绝只给触发它的尝试**：R-A 下拒绝是逐尝试的判定回复，天然只到达这次尝试，不会唤醒同 ID 的别的调用；其他尝试的意图仍在流里，所有者接着处理并给它们各自的判定。这满足 §12b 的约束（拒绝不跨尝试唤醒）且不需要观察表的版本守卫处理旧拒绝回执。
- `WaitTerminal`：同一 ID 的每次调用都登记自己的通道（不覆盖），下一个被观察到的、版本大于登记时已知版本的非 Rejected 终态唤醒该 ID 的全部等待者（RV-02）；登记后先 `Get` 一次，已是终态直接返回。观察者是每条流一个 `DeliverNew` 有序消费者（不从流头回放历史，原型 Client 的 `latest` 表无界增长问题因此不存在）。
- 选项 R-B（原型同形：拒绝落日志为 `Rejected` 终态记录，版本接着往上走）见 §14。

**第 2 批发现（§13.1 第 9 条；维护者 2026-10-28 决定按推荐用 A）**：R-A 下拒绝不写日志，所以“拒绝”不是持久承诺：所有者在下一次写事件之前崩溃时，被拒的意图（输入水位还没随事件持久）在重放时按当时的准入重新判定，可能被接受。

### 7.4 `EmitStart` 的启动中继

`SubscribeNestStarts`（不变的 effect 流与 durable）→ `Engine.StartSaga`：

- 提示关闭或判定 `rejected` → 返回 `ErrBackpressure` → nak 退避重投（effect 不丢；下次投递用新的尝试标识）；
- `ErrIdentityConflict` / `ErrInvalidRecord` → Term + `saga.start.rejected_total`（现行）；`ErrDefinitionMissing` → nak（现行）；
- `ErrStartUnconfirmed` → nak（重投时所有者按 `StartDigest` 判“已存在”）。

所以经 `EmitStart` 提交的意图在准入关闭时是延迟而不是丢失，与现行 Redis 版 `ErrBackpressure` 时 nak 的行为一致。中继跑在协调器进程（有 saga Mod 的进程），并发与 AckWait 沿用 `saga.start_effect_*`。

## 8. 投影

- **参与方 DataEngine 投影**：保持（§5.3）。
- **saga 冷副本（缺省关闭，决定 5）**：`saga.cold_copy.enabled`。开启时由**协调器进程**（选项 C-A）为每条流建一个 durable（`<stream>-cold-copy`，状态 R1 内存，原型实测选择），缺省 `terminal_only=true`（过滤 `<prefix>.*.ev.done`），一批至多 256 条、最多等 50ms，同一 saga 只留版本最大的一条，一次无序 BulkWrite 写 `<saga.database>.<saga.cold_copy.collection>`（缺省 `sagas`），每条写是“版本更大才整文档替换”的管道 upsert；**逐条 ack**（原型 AckAll 丢版本的教训，原型稿 §12）。冷副本消费者是清理守卫之一。冷副本只供 `Get` / `GetByBusinessKey` / `List` 在内存没有时回落读取，不参与唯一性与任何判定。
- 冷副本的文档形状沿用 `mongo_store.go` 的 `recordDoc`（去掉租约字段，加 `start_attempt`），`completions` / `operations` 两个集合不再写。

## 9. 配置

### 9.1 删除

- `saga.redis.*`（`key_prefix`、`partitions`、`waitaof_timeout`、`terminal_retention`、`business_key_guard`、`max_projection_backlog`）、`redis_authority.*`、`store_service.*`。
- `saga.owner`、`saga.collections.{sagas,completions,operations}`（冷副本集合改为 `saga.cold_copy.collection`）、`saga.coordinator_workers`、`publisher_workers`、`coordinator_claim_batch`、`publisher_claim_batch`、`lease_duration`、`store_timeout`、`poll_interval`、`publish_backoff_min`、`publish_backoff_max`、`completion_receipt_ttl`（换成 `receipt_retention`）、`stream_max_age`（权威流不按时间删）、`result_durable`、`result_*`、`result_workers`、`result_effect_*`、`result_effect_workers`。

### 9.2 保留与新增（全部在 `wiring/saga` 的 `config` 结构体上声明，`app.LoadConfig` 一次读完）

| 键 | 缺省 | 范围 / 规则 |
| --- | --- | --- |
| `saga.subject_prefix`（保留，shared） | `roost.saga` | subject 路径 |
| `saga.stream`（保留，shared） | `ROOST_SAGA` | 流名前缀 |
| `saga.partitions`（新，shared） | 16 | 1～1024 |
| `saga.stream_groups`（新，shared） | 4 | 1～8，≤ partitions |
| `saga.replicas`（保留） | 1 | 生产（`ValidateConfig(production=true)`）必须为 3 |
| `saga.duplicate_window`（保留） | 2m | ≥ 2 × `publish_unknown_budget` |
| `saga.stream_max_bytes`（保留） | 8 GiB | 每条流；满了拒新写 |
| `saga.coordinators`（新，shared，required） | 无 | sid 列表；生成器写入本工程跑协调器的 sid |
| `saga.max_batch`（新） | 256 | 1～1000 |
| `saga.tick`（新） | 5ms | 1ms～100ms |
| `saga.snapshot_every_events` / `snapshot_interval`（新） | 2000 / 30s | ≥1 / ≥1s |
| `saga.terminal_retention`（新） | 1h | ≥ 1m；≤ `receipt_retention` |
| `saga.receipt_retention`（新，参与方用） | 168h | ≥ `terminal_retention`；必须长于步骤命令可能重投的最长时间 |
| `saga.publish_timeout`（保留） | 3s | < `publish_unknown_budget` |
| `saga.publish_unknown_budget`（新） | 30s | ≥ 3 × 7s |
| `saga.query_timeout` / `start_verdict_timeout`（新） | 2s / 2s | — |
| `saga.purge_stall_alarm`（新） | 10m | — |
| `saga.admission.{enabled,high_records,low_records,signal_interval,stale_after}`（新） | true / 5000 / 2500 / 200ms / 2s | low < high；stale_after ≥ 5 × signal_interval |
| `saga.cold_copy.{enabled,terminal_only,collection,batch_size,max_wait}`（新） | false / true / `sagas` / 256 / 50ms | — |
| `saga.database`、`saga.max_payload_bytes`、`saga.step_defaults`、`saga.steps`、`saga.start_effect_*`（保留） | 现值 | 步骤超时 < `DefaultStepAckWait` 的规则保留 |

跨 Mod 规则（`CheckConfigAcrossMods`）：保留 start effect 流与 `dataengine.effects` 一致；新增“协调器进程（sid 在 `saga.coordinators` 里）在生产要求 `singleton.enabled=true`”与 §6 的硬上限规则；删除 `checkAuthorityPool` 与 `checkEffectRetention`（结果不在效果流上了）。改完声明运行 `go generate ./codegen/internal/roost` 刷新 `kitconfig_gen.go`。

### 9.3 生成物

`roost add saga` 生成的配置段不写 `saga.redis.*`，写 `saga.coordinators: [<game sid>]`；catalog 里 saga 依赖改为 `nats`、`mongo`（Mongo 步骤与冷副本用；可选依赖 `dataengine`），去掉 `redis_authority`、`store_service`。

### 9.4 升级（写进 CHANGELOG `[Unreleased]`）

格式与存储不兼容：旧版本 saga 数据（Redis 权威、Mongo 冷副本、`_dataengine_step_operations`、Mongo 步骤 `_operations`、旧回执）不迁移。**升级需清空**：先在旧版本让全部 saga 到终态（`List` 非终态为 0），停旧协调器与步骤进程、排空参与方 WAL，删除旧 saga 流 / Redis 键前缀 / 上述集合，再起新版本（新流、新桶）。WireVersion 1 的信封一律拒绝。

## 10. 故障与恢复

| 故障 | 行为 | 验证 |
| --- | --- | --- |
| 协调器 kill -9 | 已 PubAck 的事件在流里，内存丢失；同 sid 进程重启（App 单实例锁等旧锁过期）→ 快照 + 尾巴重放 → 服务；`InHi` 之后的输入重新处理。分区在重启前停顿 | 移植 `TestCoordinatorKill9Takeover`（静态归属版：同 sid 重启）；时间轮重建 |
| 旧协调器还活着（SIGSTOP 恢复、重叠） | 下一次发布被期望序号拒绝 → `ErrPartitionFenced` → fail-stop | 移植 `TestFenceRejectsStaleWriter`；变异 `nofence` 必须变红（事件链分叉） |
| 参与方 kill -9 | strict 已确认的提交在 WAL；重启先排空 WAL 进 Mongo 再加载（回执随实体）；未 ack 的命令重投 → `Begin` 回放 | 移植 `TestParticipantKill9Recovery(Projected)` 到真实生成 DAO 夹具 |
| 参与方结果发布失败 | 重试到 PubAck 才 ack 命令；进程退出则命令重投、回放 | 单元注入发布失败 |
| 发布结果未知 | 同 MsgID、同期望序号重发，§4.3 判定；超过 30s → `ErrPublishUnknown` fail-stop | 移植 `TestPublishOutcomeUnknownIsResolved`（单条 Duplicate、批 10201 / 最后一条是自己的两条分支都要命中） |
| 选主停顿 | 发布重试，停顿内该流分区不推进；可能产生多余重试 | 移植 `TestLeaderKillKeepsAckedWritesAndResolvesUnknowns` |
| 快照写冲突 | CAS 冲突读回，更新的不是自己 → fail-stop | 单元 + 真实 KV |
| 快照不兼容 | `ErrSnapshotIncompatible`，分区不启动，不从头重放 | 新增：改 V / Part / 布局 / 摘要各一例 |
| 清理守卫卡住 | 不清理、告警；流按 `MaxBytes` 满了后拒新写 → fail-stop | 新增：停一个步骤 durable，核对不越过 ack floor 与告警 |
| 准入信号过期 / 未就绪 | 关闭准入（stale / not_ready），在途不受影响 | 移植三个准入用例 + `noadmit` 变异 |
| 终态保留期后到的迟到成功 | 记录已剪，只计 `saga.completion.unknown_saga_total` + ERROR | 新增单元 |
| 整集群同时断电 | 可能丢最近已确认消息（前提 1 承担） | 不处理 |
| **遗留 Mongo 事务（被杀参与方的投影事务，`transactionLifetimeLimitSeconds` 缺省 60s）** | 重启排空 WAL 时同文档写冲突，直到服务端回收；这段时间该参与方不服务，在途步骤消耗重试预算（原型 2026-10-28 一次 77s、在途一笔进 `manual_required`，KNOWN-LIMITS“尚未确认”） | 见下 |

**遗留 Mongo 事务的处理（需选择，§14 T）**。推荐 T-A：

1. 部署要求沿用 SAGA.md 现有推荐：服务端 `transactionLifetimeLimitSeconds=20`（框架自己的事务由 `mongo.transaction_timeout` 约束，单次远短于 20s）。
2. 参与方进程启动时只读探测 `getParameter transactionLifetimeLimitSeconds`：大于“缺省步骤预算窗口（Timeout × MaxAttempts + 退避，约 30s）”时 WARN 一次并计 `saga.step.mongo_txn_lifetime_exceeds_budget`；托管 Mongo 拒绝该命令时只记“未探测”。不拒绝启动（参数归部署）。
3. DataEngine 启动排空遇 `WriteConflict` / `TransientTransactionError` 时有界重试（上限 = 120s），超过才启动失败（原型的 3 分钟重试证实需要；现行 `wiring/dataengine` 是否已重试，实施时核实，没有就在第 4 批补）。
4. 先红后绿：把原型 `TestParticipantKill9RecoveryProjected` 的场景搬进真实夹具，服务端参数 60s 时用缺省预算稳定复现 `manual_required`（红），参数 20s 时通过（绿）；两份原始输出入样本目录。KNOWN-LIMITS 那一条改为“部署参数要求 + 探测告警”，不再写“尚未确认”。

T-B（把缺省步骤预算拉到覆盖 60s）与 T-C（重启时主动终止遗留事务）见 §14。

## 11. 测试迁移

### 11.1 现有测试

| 文件 | 处理 | 理由 / 去向 |
| --- | --- | --- |
| `completion_test.go`、`late_success_test.go`、`engine_state_machine_test.go` | **保留断言，改写驱动** | 统一判定规则、方向 ③④、代际、重开的承诺不变；从 `memoryStore` + Engine 改为驱动 `machine` 纯函数 |
| `engine_test.go` | 改写 | 状态机部分并入上一行；Engine 外观（Register、预算补齐、Stats）改为对新 Engine + 真实 NATS 或 `machine` |
| `start_contract_test.go` | 改写 | 启动身份、摘要、`IDOf`、同意图返回已有、冲突；新增判定送达与 `ErrStartUnconfirmed` |
| `wire_validation_test.go` | 改写 | WireVersion 2 的事件 / 输入 / 命令 / 结果 / 快照清单；坏输入跳过并计数 |
| `nest_atomic_test.go` | 保留 | EmitStart 与实体修改同一 WAL 记录 |
| `nest_effects_test.go` | 改写 | 只留启动 effect；completion effect 部分删除 |
| `consumer_test.go`、`consumer_nak_maxdeliver_real_integration_test.go` | 改写 | 步骤消费者（坏信封 Term、Admit、nak、发布后 ack）、启动中继；删 Nest completion 消费者 |
| `native_step_test.go`、`native_step_operation_test.go`、`native_step_incarnation_test.go` | 改写 | 至多一次跨尝试、代际拒绝回放规则、过期未执行、回放重发成功；改为实体内回执 + 真实 Nest |
| `command_inbox_test.go`、`mongo_step_*`（5 个）、`step_operation_*`（2 个） | 改写（M-A） | Mongo 步骤按操作回执；并发同操作的写冲突串行用真实 Mongo；benchmark 保留形状 |
| `mongo_store_test.go` | 改写 | 冷副本投影器：版本只升、逐条 ack、重投幂等 |
| `late_alarm_real_mongo_integration_test.go`、`late_step_real_mongo_integration_test.go`、`sagafix_real_mongo_integration_test.go` | 改写 | 同承诺，换真实 JetStream 协调器 |
| `cross_process_real_integration_test.go`、`performance_real_integration_test.go`、`jetstream_performance_integration_test.go` | 改写 | 两进程与压测夹具改到新装配 |
| `assembly_test.go` | 改写 | 新生命周期：先恢复再服务、停机三步 |
| `dataengine_step_inbox_test.go`、`dataengine_step_inbox_real_mongo_integration_test.go`、`dataengine_step_benchmark_test.go` | 删除 | 租约、屏障、等投影都已删除；“回放 / 代际拒绝”的独有断言先并入 `native_step_test.go` |
| `redis_store_test.go`、`redis_store_real_integration_test.go`、`redis_projection_real_integration_test.go`、`store_contract_test.go`、`engine_round_trip_test.go` | 删除 | 对象（Store 接口、Redis、脚本往返计数）已删除；往返计数由新的“每步 JetStream 消息数”单元取代 |
| `framework/storeservice/runner_test.go`、`wiring/storeservice/mod_test.go`、`wiring/redis/authority_test.go`、`infra/storage/redis/driver/durable_script_test.go` | 删除（D-A） | 对象删除 |
| `wiring/saga/config_test.go`、`saga_mod_test.go`、`real_fixture_integration_test.go`、`steps_*` | 改写 | 新键与跨键规则、Mod 生命周期、真实夹具、步骤运行时 |
| `wiring/sidmerge/*` | 改写 | 合服前检查改为 `InspectPartitions`（离线重放）：全部 saga 终态；不再有“投影积压为 0” |
| `codegen/internal/roost` 的 catalog / shutdown / singleton / kitconfig / add_saga 测试与 `testdata/saga_native` | 改写 | 依赖、配置段、回执字段与 Nest 包装 |
| 根包 `integration_coverage_test.go` 表、OBSERVABILITY 指标门禁 | 更新 | 新增 / 删除的真实依赖用例与指标名 |

### 11.2 新增

- **单元**：`machine` 全部出口（从 `processClaimed` 与 `Complete` 搬来的用例）、`planTransition` 唯一关闭 / 代际规则、`wheel`（含原型修过的整圈推迟断言）、`purgeBound`、`decodeManifest` 校验、准入（滞回、过期、not_ready、提示过期）、`Begin` / `Complete` 判定表、`WaitTerminal` 多等待者。
- **真实 JetStream**（私有三节点，沿用原型 `nats-cluster.sh` 移到 `scripts/nats-cluster.sh`，端口私有、用完 clean）：`infra/network/nats/driver` 的语义用例（原型 `semantics_test.go` / `failover_test.go`：通配期望序号、并发写恰好一个成功、批与期望序号、批与 MsgID 10201、缺号 10176、去重先于序号、ack 丢失、leader kill）；saga 端到端（正向、补偿、重复命令与结果、迟到成功重开、旧写者隔离、快照清理后重放一致、结果未知两分支、多流、pipelined）。
- **真实 Mongo**（共享隔离环境或 `mirror-local.sh`，唯一库名、用完删）：原生步骤投影开、冷副本、Mongo 步骤写冲突、遗留事务红绿（§10）。
- **kill -9**：协调器（同 sid 重启接管）、参与方（含投影）；子进程用测试二进制自身（原型 `roles_test.go` 做法）。
- **变异**（必须失败）：`SAGAJS_MUTATE=nofence`、`noreceipt`、`noadmit` 迁移为 `ROOST_SAGA_MUTATE`，只在测试构建里生效。
- **生成工程**：`scripts/test-demo-generated.sh` 的 `add saga -native` 段（一笔正向、一笔补偿、一条 effect）改到新路径并核对回执字段；`scripts/test-mmorpg-cross-sid.sh` 的 giftdrill 改为经 admin `gm.saga.get` 或开冷副本等结果（它现在直接读 saga 库）；game-demo 重新生成 build / vet / test，`go generate ./...` 后 porcelain 干净。

## 12. 性能验收（整轮 ≤ 30 分钟）

口径沿用原型（报告 §1、第五轮）：私有三节点 R3、本机隔离 Mongo（参与方投影开、8 通道、冷副本关）、GOMAXPROCS=4、闭环并发、每档 30s、稳态 [5s, 30s)、单样本为主；但参与方换成**正式代码**：生成 DAO 的实体、实体内回执、`Steps` 运行时、正式装配。脚本 `scripts/perf/saga-js-scan.sh`（取代 `saga-scan.sh`），负载门：每档前等 loadavg < 8，最多 60s。

| 档 | 并发 | 秒数 | 说明 |
| --- | --- | --- | --- |
| 原生正向 strict | 1 / 16 / 64 / 128 | 4 × 30 | 主档 |
| 原生全补偿 strict | 16 / 128 | 2 × 30 | |
| 原生正向 pipelined | 128 | 30 | |
| Mongo 步骤正向 | 16 / 128 | 2 × 30 | 新收件箱 |
| 准入开 + 投影闸门 | 128 | 30 | 验证关闭 / 重开路径与积压有界 |
| 快照开（每 2000 事件） | 64 | 30 | 测快照异步化后对吞吐的影响 |
| 重放 | — | — | 合成 1 万 / 5 万条日志各一次 |
| 原生正向 strict 第二样本 | 16 / 128 | 2 × 30 | |

13 档 × 30s = 6.5 分钟负载；每档建环境 + 追平 + 核对约 30s、负载门平均约 20s → 约 17 分钟；编译与重放约 2 分钟；**合计约 19～21 分钟，最坏（负载门每档等满 60s）约 28 分钟**。云 Mongo 一轮（只换参与方投影库）不计入这 30 分钟，单独约 8 分钟、按需另跑。

**达标判据**（以原型第五轮本机数字为参照，正式代码多了生成 DAO、回执与运行时开销，留 25% 余量）：

- 正确性（硬门槛）：每档余额 / 复读 / Mongo 账户逐笔 0 差异；非期望终态 0；发布结果未知 0（压测不注入故障）；停止加压后 30s 内积压归零。
- 原生正向 strict：c128 ≥ 900 笔/s（原型 1150～1210），c1 端到端 P50 ≤ 40ms（原型第三轮同口径约 30ms）；c16 不低于原型同档（179）。
- 准入档：积压峰值 ≤ 1.3 × `high_records`、准入关闭时有效吞吐不为 0。
- 不达标时：同机同参数补跑原型对应档作对照（`run-perf.sh round5-local` 单档），区分“正式实现退化”与“宿主负载”，退化定位到段后再决定；不放宽门槛、不换口径。

样本放 `docs/performance/benchmarks/saga-jetstream-authority-<日期>/`，报告 `docs/performance/SAGA-JETSTREAM-AUTHORITY-<日期>.md`。

## 13. 分批实施计划

每批：独立 worktree；中文提交；显式 `git add`；门禁全过才推 main；推前 `git pull --rebase`；跨包行为改动推前跑全量 `GOWORK=off go test ./...`。下表“门禁”是在通用门禁（`GOWORK=off go build ./... && go vet ./...`、`go run ./cmd/glsvet ./...`、根包 `go test -count=1 .`）之外的。

| 批 | 范围 | 提交粒度 | 门禁 | main 状态 |
| --- | --- | --- | --- | --- |
| 0 | 本设计稿与决定记录 | 2 笔（本次） | 根包 | 不变 |
| 1 | `infra/network/nats` 的 `JetStreamLog` 接口与 driver 实现；`JetStreamPublishOptions.Headers`；`wiring/nats` 发布 `ModNatsLog`；语义与 leader kill 用例；私有集群脚本 | 2～3 笔（接口+实现、语义用例、脚本） | driver 包 `-race`；真实三节点语义用例全过 | Redis 版照跑，新能力无人用。**已完成**（2026-10-28，提交见 git log；接口调整见 §3.2，脚本 `scripts/nats-cluster.sh`） |
| 2 | `framework/saga` 新协调器：`log_layout` / `log_wire` / `transition`（从 engine 抽纯函数，Redis 引擎改为调用它们，行为不变）/ `partition` / `partition_replay` / `wheel` / `admission` / `query` / `terminal_watch`；新 Engine 以 `NewJetStreamEngine` 暂名并存，不接线 | 4～6 笔（先抽纯函数并跑全部现有 saga 测试证明无行为变化，再逐文件加新代码与单元） | `framework/saga` `-race` 全过（现有测试不改）；新单元；真实 JetStream 端到端、结果未知、隔离、快照清理、kill -9（协调器） | Redis 版照跑。**已完成**（2026-10-28，提交见 git log；实现与本文的差异见 §13.1） |
| 3 | 参与方新路径并存：`StepReceipt*`、`NativeStep.Begin` 与新 `Complete` 分支（`run != nil` 时走新路径）、`SubscribeNativeStep`、`MongoStepInbox`、`publishCompletion`、`LoadPublisher`；codegen：`add saga -native` 生成回执字段与组件、`codegen/internal/nest` 的 Begin 包装（先在 testdata 生成工程里验证，不改 demo） | 3～5 笔 | 受影响包 `-race`；codegen golden（有意改动 `-update` 逐个看 diff）；参与方 kill -9；真实 Mongo 的 Mongo 步骤写冲突 | Redis 版照跑；生成器新输出只在 testdata（Begin 包装对 demo 生成物是空操作，见 §13.2 第 5 条）。**已完成**（2026-10-28，提交见 git log；实现与本文的差异见 §13.2） |
| 4 | **切换与删除**：`wiring/saga` Mod / Steps / config 换新装配；`StartSaga` 等 API 定稿（删暂名）；启动中继；demo（赠礼 saga、gm、PlayerDao 回执字段、配置、部署清单）；生成器 catalog / render / kitconfig / help / doctor；sidmerge；删除 §3.3 第 4 批全部；文档（SAGA.md、guide/06、impl/06、OBSERVABILITY、PACKAGES、impl/13、CONSISTENCY、KNOWN-LIMITS、CHANGELOG 升级要求）；测试按 §11 迁移；遗留事务红绿 | 多笔，**整批一起推**：先在分支内按“接线 → 删 Redis/Store Service → 生成器与 demo → 文档 → 测试迁移”顺序提交，每笔可编译；全部门禁过后一次推 main | 全量 `go test ./...`；受影响包 `-race`；`-tags integration` 的 vet；真实 NATS + Mongo 集成；kill -9 两类；`go generate ./...` 无漂移；重新生成 game-demo build / vet / test；`scripts/test-demo-generated.sh`、`test-mmorpg-cross-sid.sh`（真实依赖）；`TestEveryDeclaredConfigFieldIsRead` / kitconfig 快照测试 | 切到 JetStream；Redis 版不再存在。**已完成**（2026-10-29，实施记录 §13.3） |
| 5 | 删 DataEngine lease fence 机制（§3.3 第 5 批）与相关指标、文档；维护者 2026-10-29 的两项决定（`dataengine.startup_timeout` 缺省 60s、`saga.coordinators` 保持 sid + 部署规则） | 1～2 笔 | `framework/dataengine` 与 `framework/nest` `-race`；投影器真实 Mongo 用例；全量 `go test ./...` | 可用。**已完成**（2026-10-29，实施记录 §13.4） |
| 6 | 冷副本（可选能力）：`ColdCopyProjector`、`Get` / `List` 回落、配置、kill -9 后最终一致用例 | 1～2 笔 | 真实 Mongo + JetStream；`-race` | 可用（缺省关）。**已完成**（2026-10-29，实施记录 §13.5） |
| 7 | 性能验收（§12）、删除原型目录与旧压测脚本、报告与样本 | 1～2 笔 | 压测 ≤ 30 分钟；报告数字与样本一致 | 可用。**代码部分已提前到第 6 批完成**（删原型目录、`scripts/perf/saga-js-scan.sh` 与 `TestRealSagaJSScanLevel`、只做冒烟，§13.5）；**压测已完成（2026-10-29，全部达标）**：报告 [SAGA-JETSTREAM-AUTHORITY-20261029](../performance/SAGA-JETSTREAM-AUTHORITY-20261029.md)；首轮封顶约 500 笔/s 定位为 strict 在 short worker 上等 fsync，维护者决定原生步骤改 pipelined + Nest 异步完成（`65cda8c9`） |

Redis 删除放在第 4 批，与切换同一次推送：维护者要求“同一批删掉”，且切换后 Redis 代码再无调用方。第 2、3 批的并存代码（暂名 Engine、`NativeStep` 的双分支）在第 4 批删除，不留到 main 的稳定状态。

### 13.1 第 2 批实施记录（2026-10-28）

范围：`transition.go`（纯函数抽取）、`log_layout.go`、`log_wire.go`、`partition.go`、`partition_replay.go`、`wheel.go`、`admission.go`、`query.go`、`terminal_watch.go`、`jetstream_engine.go`（`NewJetStreamEngine` 暂名并存，未接线），单元 `transition_test.go`、`jetstream_coordinator_test.go`、`log_wire_test.go`、`wheel_test.go`、`admission_test.go`，真实三节点 `jetstream_coordinator_real_integration_test.go`（`-tags integration`，`scripts/nats-cluster.sh test` 一并跑）。与上文的差异与理由：

1. **抽取范围**：§3.1 列的判定函数之外，又从 `processClaimed` / `Resume` / `Compensate` / `completeNotWaiting` / `compensateLateStep` / `StartSaga` 抽出 `planDue`、`planResume` / `prepareResume`、`planCompensate`、`judgeHistory` / `lateStepState`、`prepareStart` / `newStartRecord`，`stepTransition` 的规则部分成为 `planTransition`。初稿的 `machine` 结构体没有做：两种协调器都直接调这些函数，多一层包装没有收益。Redis 引擎改为调用它们后，现有 `framework/saga`、`wiring/saga` 测试零改动全过（含 `-race` 与真实 Mongo / Redis / NATS 用例），另有差分用例 `TestTransitionRulesAreSharedByBothCoordinators` 把同一串事件交给两种协调器逐步比对记录与 tombstone。
2. **命名**：`WireVersion` 已是 Redis 引擎与 Nest effect 的公开常量（1），新格式叫 `LogWireVersion = 2`；结果输入的信封叫 `resultInput`（`completionEnvelope` 已被旧消费者占用）；`NewJetStreamEngine(log, raw, options)` 多一个 `fnats.RawClient`（查询与判定回复走核心 NATS，`JetStreamLog` 只有 `RequestMany`）。生命周期是 `Start` / `Stop`（三步停机）/ `Run`。第 4 批切换时合并命名。
3. **`sagaState` 多了 `Receipts`**（CommandID → 回执摘要）：存储按 CommandID 记回执，才能给出“同一尝试重复送达”与“同一 CommandID 不同结论 = 身份冲突”的判定（`judgeHistory` 的 Receipt 位、Apply 的回执检查）；分区要给同样的结论就得记同样的东西，随 saga 在保留期后剪掉。放弃后迟到成功的告警（late_alarm）与 Redis 一样不加版本，只改 tombstone。
4. **事件合并**：同一 saga 在一轮里的几次转移合并成最后一条事件（启动 + 立即派发、结果 + 下一步派发各一条），中间是终态的保留（要进 done subject）。一轮切成几次发布时只有最后一块带这一轮的输入水位（中途崩溃时这一轮的输入重新判定，都幂等）。
5. **MsgID 带分区号**（真实三节点上发现）：初稿的 `<writer>.<lastEv>.<轮号>.<i>` 让共用一条流的两个分区首轮 MsgID 相同，服务端报 10158（duplicate message id is in process）。同时把驱动对 10158 的归类从“确定没写入”改为“结果未知”（同 MsgID 的前一次提交还在处理，可能提交；这条是按语义推断，未单独复现）。
6. **重放终点与起点**：见 §4.5 后的注。快照清单加载校验版本 / 分区 / 布局 / 对象长度与 SHA-256，不符 `ErrSnapshotIncompatible`（真实用例各篡改一项）。
7. **第 1 批提出的 5 个问题**：① `ceil(P/G) ≤ 50` 在 `Layout.Validate`（`NewJetStreamEngine` 调用，第 4 批 `wiring/saga` 配置检查调同一函数），10176 计 `BatchIncomplete`、立即重发；② LoadBucket 开 `LimitMarkerTTL`，真实用例 `TestRealJetStreamCoordinatorAdmissionForgetsAnExpiredParticipant` 证明参与方键过期后准入重开（去掉它变异变红）；③ `ErrLogRejected` → `ErrPartitionRejected` fail-stop（流满真实用例）；④ 重放终点用 `LastMsg` 序号；⑤ 准入信号过期按本机时钟。
8. **时间轮**补了墙钟回拨：时钟往回调时新登记的条目被夹在 cursor 槽里，原实现要等时钟追回才扫到；现在 target < cursor 时扫 cursor 槽、按到期时刻判定（单元 `TestWheelHandlesAClockStepBack`）。
9. **R-A 拒绝的持久性（维护者 2026-10-28 决定：按推荐，用 A）**。拒绝（与其他不写事件的判定）只推进内存里的输入水位，水位随下一条事件持久。所有者在那之前崩溃，重放会重新处理这条启动意图；准入此时已打开就会接受它——发起方先收到 `ErrBackpressure`，saga 之后却被建立。同一请求重试会得到“已存在”，经 `EmitStart` 的路径本来就 nak 重试，所以影响只在“收到拒绝后放弃、之后用同一业务键发不同意图”的调用方（得到身份冲突）。A（推荐）：接受并写进 SAGA.md（`ErrBackpressure` = “现在没建立，用同一请求重试”）；B：本轮有拒绝而没有事件时写一条只带水位的事件（每个拒绝轮多一次 R3 写，背压时恰好加负载）。本批按 A 实现；维护者 2026-10-28 确认 A（原话“按推荐，用 A”）。SAGA.md 对外描述（`ErrBackpressure` = “现在没建立，用同一请求重试”）在第 4 批接线时写。
10. **未做、留给后续批**：`LoadPublisher`、步骤消费者与 `publishCompletion`（第 3 批）；`inspect.go`、冷副本（第 4 / 6 批）；`saga.coordinators` → `Owned` 的分配与生产配置规则（≥ 3 × 7s 的结果未知预算、R=3）在第 4 批配置层；准入提示只在 3s 内可信（发起方据它快速失败，所有者的判定才是权威，真实用例里提示比所有者晚一个发布周期）。

### 13.2 第 3 批实施记录（2026-10-28）

范围：`step_receipt.go`（`StepReceipt*`、`StepOutcome`、`RunNativeStep`、Begin / Complete 的新路径）、`native_step.go`（`NativeStep.Begin`、`Complete` 按 `run != nil` 分支）、`step_consumer.go`（`SubscribeNativeStep`、`SubscribeJetStreamMongoStep`、`publishCompletion`、`decodeCommandEvent`）、`mongo_step_inbox.go`（`MongoStepInbox`，M-A）、`admission.go` 的 `LoadPublisher`；`framework/nest` 新增只读 `CurrentDispatchHasRemoteWriteBatch`；生成器 `roost add saga -native` 的回执字段与适配、`codegen/internal/nest` 的 Begin 包装；两处顺带修复（第 1 批驱动的清理守卫、第 2 批协调器的时间轮判定，见第 9、10 条）。单元 `step_receipt_test.go`、`step_consumer_test.go`，真实依赖 `step_participant_real_integration_test.go`（`-tags integration`）。与上文的差异与理由：

1. **`StepReceipts` 存不透明字节**：方法是 `StepReceipt(op) ([]byte, bool)`、`PutStepReceipt(op, []byte)`、`DeleteStepReceipt(op)`、`RangeStepReceipts(func(op, []byte) bool)`，而不是初稿的类型化 `StepReceipt`。编码与校验归框架（`encodeStepReceipt` / `decodeStepReceipt`，带版本）：坏回执报 `ErrInvalidRecord`（事务回滚、nak），而不是被适配层当成“没有回执”让操作再执行一次；生成适配只把 DAO 的 `map[string][]byte` 访问器原样转过来，DAO 访问器本来就不返回错误。
2. **`Begin(target any)`，不是 `Begin(StepReceiptHost)`**：handler 的锁目标常是组件视图接口（`I<Comp>Entity`），接口里没有 `SagaStepReceipts`，具体实体类型才有；类型化参数会让生成代码编译失败。改为运行时检查，缺了返回 `ErrNoStepReceipts`（点名类型）。代价：初稿“编译期失败”的保证没了；`add saga -native` 对锁的实体生成适配，手写 handler 漏了会在第一次投递时报错。
3. **回放时重写同一份回执**（§16 “AfterCommit 时机”一条的结论）：pipelined 下进程内允许级联读，回执可能是另一笔还没落盘的事务写的；空事务没有 WAL 票据，AfterCommit 立即执行，结论会先于写回执那笔的落盘外发。回放把同一份回执再 `PutStepReceipt` 一次，回放事务就是一条 WAL 记录，它的 AfterCommit 晚于自己的落盘，而 WAL 按序落盘。只在重投时多一次写。单元断言回放写一次回执。
4. **生成物不是 ECS 组件**：初稿写“与 `roost add timer` 同形”的组件；实际只需要一个方法，生成 `game/entities/<x>/saga_receipts.go`（业务文件，缺失时创建一次，`SagaStepReceipts()` + 适配类型）与 DAO 字段 `SagaReceipts map[string][]byte \`bson:"saga_receipts" dao:"persist"\``，没有组件类型号、manifest 条目与 project sync 重写。同一实体上的第二个原生 saga 不再改动。`entity.InspectReceiptHost` 拒绝 `remote=managed` 与 `noPersist=true` 的实体。
5. **Begin 包装对全部带 `saga.NativeStep` 参数的 handler 生效**（不是只对 testdata）：包装调用的是 `step.Begin(e0)`，旧路径上 `Begin` 是空操作，所以 demo 模板生成出来的 game-demo 行为不变（demo 文件本身没改）。生成器同时要求这类 handler `durability=strict|pipelined`、只有一个 `NativeStep` 参数、第一个锁目标不是组（`TestNativeStepHandlerDispatchBeginsBeforeTheBusinessCall`）；现有 handler（demo 两个、testdata 四个）都是 strict。
6. **命名与配置**：Mongo 步骤新消费者暂名 `SubscribeJetStreamMongoStep`（`SubscribeMongoStep` 被旧路径占用，第 4 批合并）；`RunNativeStep` 返回导出的 `StepOutcome`（原生与 Mongo 共用）；`StepConsumerConfig` 加 `NativeWorkers`（缺省 min(32, MaxAckPending)：一次处理含一次 Nest 请求与落盘，回调逐条串行会把整条流压到一次落盘一条）与 `ReceiptRetention`；`MongoStepInbox` 的集合名必填（并存期不给缺省，免得两种文档形状落进同一集合），初稿的 `Replay` 方法没有调用方，没做。转交路径的新版 `RunHandedOver` 留给第 4 批（`RunNativeStep` 就是它的执行部分）。
7. **Mongo 步骤的可重试失败也写回执**：初稿写“可重试失败不留回执”。协调器对同一 CommandID 只接受一种结论（回执摘要不同 = 身份冲突），不留回执时这次尝试的重投会重新执行、可能交出另一个结论。现在回执记下它（`retryable`），只对写它的那次尝试回放，不挡同一操作的下一次尝试（下一次覆盖它）。原生步骤没有可重试结论（Nest 出错 = 结果未知、nak），不受影响。
8. **过期命令也经 Nest 跑一次**：截止判定在 Begin 里（要先查回执：已成功的要重发成功），消费者对过期命令不短路；处理时长以 AckWait 为界，不以命令截止为界（否则过期命令的 Nest 请求一开始就超时、nak 到 MaxDeliver）。
9. **§16 第 1 条（空闲步骤 durable 是否挡清理）已核实：挡**。私有三节点实测：只过滤自己主题的 durable 空闲时 `AckFloor.Stream` 停在 0（有活跃订阅也一样），10 次快照后 `Purges = 0`、流里一条没删；低频主题的 durable 停在它最后一条消息。修在驱动（第 1 批的 `DurableAckFloors`）：先读流末序号再列消费者，没有待投递也没有待确认消息的 durable 取 max(服务端 floor, 流末序号)。修后同一场景每次快照都清理、只留最后一条事件；端到端用例带一个从来没有命令的步骤 durable 核对清理照常。限制见 §16。
10. **第 2 批协调器的定时器会丢（本批的参与方停机用例发现、已修）**：时间轮按墙钟（`UnixNano`）落槽、取出，分区判定“到期没有”却用 `NextRunAt.After(now)`，两边都带单调读数时按单调钟比。墙钟比单调钟快约 3.5ppm，步骤超时又是 tick 的整数倍、判定那一轮的时刻常常正好落在截止上，轮已经取出的条目被当成“没到期的陈旧条目”丢掉，记录停在 waiting / pending 再也不重试。红：参与方停 10s、150 笔里 8～17 笔卡住（插桩确认每一笔都是“墙钟已到、单调钟差 3.3µs”）；改为按墙钟比较后绿（`TestRealJetStreamCoordinatorRetriesThroughAParticipantOutage`）。
11. **验证（本机 macOS，nats-server 2.14.5 私有三节点，共享隔离 Mongo 唯一库）**：端到端 60 笔（注入“已提交但报错”28 次，业务执行次数 = 操作数 132）；参与方 kill -9（300 笔，10% 注入结果未知）快速重启与停 60s 两种都过，Mongo 余额逐账户一致、660 条回执；Mongo 步骤并发尝试三轮业务写各恰好一份；`LoadPublisher` 关闭 / 滞回 / 重开 / `Remove`。变异：`noreceipt` 端到端与 kill -9 都红（余额不符）；回退清理守卫修复、回退时间轮修复（三次两红，随机性见第 10 条）、去掉回放重写、拒绝跨代际回放、去掉截止检查、Mongo 不查回执、发布失败仍 ack、去掉生成的 Begin 都红。
12. **未做、留给第 4 批**：`Steps` 接线（`LoadPublisher` 由 `Steps.Start` 启动、`Drain` 时 `Remove`）、新版 `RunHandedOver`、删旧路径（`NativeStep` 的双分支、`DataEngineStepInbox`、旧消费者）、demo 的 PlayerDao 回执字段与适配。

### 13.3 第 4 批实施记录

范围：`wiring/saga` Mod / Steps / config 换新装配，`framework/saga` 定名（`Engine`、`Options`、`NewEngine`、`SubscribeMongoStep`、`WireVersion` = 2），删除 §3.3 第 4 批全部（Redis 存储与 Lua、Mongo 协调器存储、`DataEngineStepInbox` 与操作状态文档、completion effect 与消费者、`framework/storeservice`、`wiring/storeservice`、`wiring/redis` 的 `AuthorityMod`、`infra/storage/redis` 的权威探针 / `DurableScriptBatch` / 流读取、Redis 版脚本），生成器与 game-demo，合服检查，文档，测试按 §11 迁移。提交按“接线 → 删除 → 生成器与 demo → 文档 → 测试迁移 → 决定 A 修复 → T-A → 定时不变量 → 全量测试发现的漏项”共 9 笔（见 git log；中间提交只保证 `go build` / `go vet`，测试迁移那一笔起单元恢复，决定 A 的四个用例在那一笔为红、下一笔转绿）。

**维护者决定的落实**

1. **拒绝的持久性 A**：`ErrBackpressure` 的文档（`errors.go`、SAGA.md、guide/06）改为“这次没有建立，用同一请求重试”；代码不改（§13.1 第 9 条）。
2. **原生步骤迟到成功的补偿载荷 A**：`StepReceipt` 不存 Data，`successData(command, success)` 在 `Complete` 与回放时取命令载荷（`step_receipt.go`）。同一操作各次尝试的载荷相同，回执不变大，`max_payload_bytes` 仍在命令发出前强制。先红后绿：迁移后的 `late_success_test.go` 三处“补偿载荷 = 正向命令载荷”断言在修前为红（补偿载荷 `""`，期望 `"x"`），修后绿；另加 `TestNativeStepSuccessCarriesItsCommandPayload`（成功、回放、拒绝三种）。真实 `TestRealJetStreamCoordinatorCompensatesALateSuccessOnce` 断言补偿命令带原载荷。
3. **T-A**：① 部署要求 `transactionLifetimeLimitSeconds=20`（SAGA.md）；② `fmongo.IMongo` 加只读 `GetParameter`（driver：admin `getParameter`；mongotest：`Parameters` 表），`Steps.Start` 探测，长于缺省步骤预算窗口（`ParticipantSettings.StepWindow`，框架缺省 26.5s）WARN 并计 `saga.step.mongo_txn_lifetime_exceeds_budget`，读不到记 INFO“未探测”，不拒绝启动；③ `engine.Runtime.Start` 的排空改为 `drainAtStartup`：Flush 失败（非 fence、非关闭、ctx 未到）时退避再排空，期限是调用方 ctx（`dataengine.startup_timeout`），没给期限时上限 2 分钟；④ 红绿见下。
4. **生产配置规则**：`Layout.Validate`（每流 ≤ 50 分区、G ≤ P）由 `wiring/saga` 的 `ValidateConfig` 调用；`publish_unknown_budget` 下限 21s（3 × 7s）、`publish_timeout` 上限 9s 且小于预算；生产 `saga.replicas` 必须 3、跑协调器的进程必须 `singleton.enabled`；Writer = `<sid>-<单实例锁 incarnation>`（没有锁时 `<sid>-<随机>`）；`saga.coordinators` → `Owned`（P-A：排序后第 i 个 sid 拥有 k mod len == i 的分区，`ownedPartitions`）。
5. **第 3 批评审建议**：kill -9 的协调器与参与方真实用例（含 `ROOST_SAGA_JS_RESTART_DELAY` 长停机）列入切换门禁；分区定时不变量 `partition.checkTimers`（每条非终态 saga 在轮里有一个落在当前 `NextRunAt` 的条目）只在测试里查——`unitPartition.round` 每轮之后调用，所有用 `unitPartition` / `simEngine` 的单元都过一遍；热路径不加。

**与设计的差异**

1. `StartRequest.Now` 保留（调用方可传判定时刻，测试与重放用）；`StartRequest` 去掉 `ID`。
2. `InspectPartitions(ctx, log, layout, replicas)` 返回 `(records, pendingInputs, err)`：合服检查要同时拒绝“还有没处理的输入”。
3. `saga.partitions` / `stream_groups` / `coordinators` 没有加 `shared` 标记（配置声明的 `shared` 只支持字符串）；全部服务相同由部署约定与协调器启动的 `ErrLayoutMismatch` 保证。
4. 生成器给所有带 dataengine 的服务写 `dataengine.projection.max_unacked_records: 20000`（准入开启时跨 Mod 规则要求 > 0 且 ≥ 4 × high）。
5. `wiring/storeservice` 在接线那一笔就删了（它依赖 saga 的旧类型）；`mongo_store.go`（冷副本）直接删除，第 6 批重写。装配不依赖 Mongo。
6. 原生回执不存 Data，回放的 Data 取命令载荷（决定 A 的实现方式，不扩回执）。
7. 测试驱动加了 `engineHooks.rejectedInput`（分区跳过输入时通知测试，`simEngine` 据它还原旧 `Complete` 的错误返回）；只在测试里设置。
8. T-A 第 3 条的上限：Mod 的启动期限仍是 `dataengine.startup_timeout`（当时 30s；第 5 批调到 60s，§13.4），没有改成 120s（改缺省会连带生成器的部署启动等待）；120s 只用于调用方没给期限的情形。实测服务端每半个参数扫一次过期事务（最晚 1.5 倍参数回收），参数 20s 时最坏回收时刻等于 30s，余量是被杀到重启之间的时间——是否把缺省调到 60s 列为待决定（KNOWN-LIMITS）。
9. DataEngine lease fence 的两个真实 Mongo 用例与生成工程的 `fenced_step_test.go` 原来靠 `DataEngineStepInbox` 产生 fenced 记录；fence 机制第 5 批才删，这里按旧收件箱写进 Mongo 的同一形状在测试里重建替身（操作状态文档、fence 回执、步骤回执、effect），断言不变。

**测试迁移（维护手册 §7：删除的用例写明覆盖它的保留用例）**

- `engine_test.go` 等协调器单元：驱动从内存 Store + Redis 引擎换成 `simEngine`（同步 decide + install），断言不变；分区同一轮派发到期步骤，返回记录从 Pending 变 Waiting 的几处按“派发后的记录”断言（`compensatingStep` 辅助：补偿中或补偿方向的 Waiting）。
- `TestTransitionRulesAreSharedByBothCoordinators`（差分）→ `TestTransitionRulesMatchTheRecordedRedisCoordinator`：删除 Redis 协调器之前在基线 `1fbd5d0e` 上把差分用例里 Redis 一侧逐步的 view、tombstone 与运维判定错误录成 `testdata/transition_golden.json`（8 个场景 75 步），分区逐步比对。
- 原生步骤（`native_step_operation_test.go`、`late_success_test.go`、`native_step_incarnation_test.go`）：`nativeWorld` 换成 `simEngine` + 实体回执 + 真实 Nest 事务；承诺改变一处——旧“已提交进 WAL、截止后才投影 → 被 fence 跳过”在回执进实体后变成“strict 提交即生效，迟到成功补偿”（决定 2A）。原生没有可重试结论，涉及可重试失败的交错改由自定义参与方经 `Complete` 送达。
- 删除（对象删除）与覆盖：Redis 存储契约 / 脚本 / AOF / 投影 / 冷热交接用例（对象删除；分区的隔离、结果未知、重放、快照清理由 `jetstream_coordinator_real_integration_test.go` 覆盖）；`command_inbox_test` / `mongo_store_test` / `mongo_step_*` 的操作收件箱用例 → `TestMongoStepInboxReceiptRules`、`TestRealMongoStepInboxSerializesConcurrentAttempts`；`native_step_test` → `step_receipt_test.go` 与 `TestNativeStepConsumerPublishesToThePartitionThenAcks`；`nest_effects_test` / `consumer_nak_maxdeliver` → 消费者单元与 `TestRealJetStreamNativeStepsEndToEnd`；`cross_process` → `TestRealJetStreamCoordinatorKill9Replays` + `TestRealJetStreamCoordinatorFencesAStaleWriter`；`late_alarm` / `late_step` 真实 Mongo → `TestRealJetStreamCoordinatorCompensatesALateSuccessOnce`；`sagafix` → `TestCoordinatorSuccessDuringBackoffClosesTheOperationWithItsResult`（冷副本部分在第 6 批）；`performance_real_integration_test` 与 `wiring/saga` 的旧压测 → 第 7 批重写；`wiring/saga` 的 `steps_real_integration_test` → 生成工程 `TestGeneratedSagaNativeStepsAndEffectConsumer` + `TestRealJetStreamParticipantKill9`。
- 其他包：`wiring/config_schema_test.go` 的 saga 用例换成现有键（越界 `max_batch`、无单位 `tick`）；`wiring/sidmerge` 的真实用例第 3 步换成私有 NATS 分区日志（`ROOST_NATS_CLUSTER_URL`）；生成工程 saga_native 的真实依赖用例去掉权威 Redis / Store Service。

**验证**（本机 macOS；结果与命令见提交说明与交接报告）：受影响包 `-race`、全量 `go test ./...`、`go generate` 无漂移、重新生成 game-demo 的 build / vet / test；私有三节点 NATS 上的协调器与参与方真实用例（含两种 kill -9 与长停机）；T-A 红绿在私有 mongod（单节点副本集）上：参数 60s、期限 30s 时启动排空失败（WriteConflict 到期），参数 20s 时排空在遗留事务开始后 27～29s 完成；单元 `TestAssemblyStartupDrainRetriesALeftoverTransactionWithinTheDeadline` 在去掉重试时为红。变异：`successData` 去掉（三处补偿载荷红）、`checkTimers`（Waiting 不排轮，5 个以上用例红）、探针阈值（`TestStepsProbe…` 红）、排空重试（上面的单元红）、生产配置规则（R=3、协调器要单实例锁各自去掉时 `TestSagaProductionRules` 红）、每流 ≤ 50 分区与结果未知预算下限（`TestSagaConfigRulesNameTheirKeys` 红；预算下限原有一个没人用的常量，删掉，声明的 `min:"21s"` 是唯一强制点）。

### 13.4 第 5 批实施记录

范围：删除 DataEngine lease fence 机制（§3.3 第 5 批，选项 L-A）；落实维护者 2026-10-29“按推荐”的两项决定。2 笔提交（见 git log）：① 删除 fence 与测试迁移；② `startup_timeout` 缺省、`coordinators` 部署规则与本批文档。

**删除**（第 1 笔）：`framework/dataengine/lease_fence.go`（`LeaseFence`、回执编解码、`ErrFencedEntityPending`、`ErrInvalidLeaseFence`）与 `ValidateCommitRecord` 的 fence 解码；`engine/fenced_step.go`（`FenceOutcomeProjectionStore`、原生步骤屏障、被跳过后的驱逐队列）；投影器的 skipped / unknown 结果分支、`ProjectorStats.FencedEntities` / `FencedAdmissionRejected` / `StaleEvictions`、`ErrRemoteLeaseFenceUnsupported` / `ErrRetiredRemoteLeaseFenceRecord`、`MongoStore.ProjectFenced` 与 `leaseFencesMatch`、事务标记的 `skipped` 字段、测试缝 `afterLeaseFence`；指标 `dataengine.fence.skipped.total`、`dataengine.fence.evictions.{started,failed}.total`。保留并移到 `entity_unload.go`：`Projector.BindLocalExecutor` / `localRun`（运维卸载在业务池取实体锁）、`recordEntityIDs`（卸载屏障）。非 saga 的承诺（strict / async / pipelined 持久、投影冲突 fail-stop、Remote）不变：删掉的分支只有带 fence 回执的记录才会走到，第 4 批之后没有生产路径再产生它们；受影响包 `-race` 与全量测试不改断言通过。

**测试迁移（维护手册 §7：删除的用例写明覆盖它的保留用例，或承诺为什么不再存在）**

| 删除 / 改写的用例 | 去向 |
| --- | --- |
| `TestTerminalAckWakesWaitersOfAPendingEviction`（`fenced_eviction_once_test.go`） | 承诺“checkpoint 报 WAL terminal 时唤醒其他在途记录的等待方，之后的等待与准入都拿 terminal”与 fence 无关，改写为 `projection_test.go` 的 `TestCheckpointTerminalWakesWaitersOfOtherPendingRecords`（普通在途记录；去掉 `observeAckError` 时为红：等待方 2s 内未被唤醒） |
| `TestSkippedFencedStepResyncsSubscribersFromMongo` / `TestSkippedFencedStepWithoutAuthorityRetractsSubscribers`（`fenced_step_resync_test.go`） | 卸载后重载的承诺改由运维卸载触发同一条 `ManagerAccess.Unload → unload_resync` 链路：`entity_unload_test.go` 的 `TestEntityUnloadResyncsSubscribersFromMongo` / `TestEntityUnloadWithoutAuthorityRetractsSubscribers`；同一承诺另有 `framework/entity/unload_resync_test.go` 与生成工程 `remoteflow/reject_test.go`（Remote 持久拒绝） |
| `TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint`（`lease_fence_integration_test.go`） | 文件改名 `projector_replay_integration_test.go`，同名用例改用不带 fence 的“回执 + effect + mutation”记录（真实 Nest 事务产出），断言首投结论重放不变、effect 不重新暂存；fence 替身 `leaseStep` 删除 |
| `TestRealMongoFencedProjectionSerializesWithLeaseTakeover`；`mongo_store_test.go` 的 `TestMongoStoreLeaseFenceSkipsStaleSagaTransaction`、`TestMongoStoreLeaseFenceRejectsExpiredLease`、`TestMongoStoreLeaseFenceAppliesOnlyMatchingOwnerAndToken`、`TestMongoStoreRefusesARetiredRemoteLeaseFenceRecordWithoutWriting`；`lease_fence_test.go`；`validate_test.go` 的 `TestLeaseFenceValidateRefusesEachIncompleteFence` 与伪造 fence 回执一例 | 承诺随机制消失（fence 谓词、租约接管写偏斜串行、退役混合记录、fence 回执校验）。`validate_test.go` 剩余的 Loader / Tracker 守卫改名 `TestLoaderAndTrackerGuards` |
| `lease_fence_skip_promises_test.go`（6 条）、`TestSkippedFencedStepIsQueuedForEvictionOnce` | 承诺随机制消失（原生步骤屏障、跳过后驱逐、驱逐只登记一次）。原生步骤的至多一次现在由实体内回执保证（`step_receipt_test.go`、`native_step_operation_test.go` 等，第 4 批） |
| `TestRemoteLeaseFenceRejectedBeforeWALAdmission`、remoteentity 的 `TestHistoricalLeaseFencedRemoteRecordStopsTheProjectorWithoutWriting` | 承诺随机制消失（“Remote 写 + fence”组合不再特殊）；WAL 级 Remote 不产生 Rejected 仍由 `TestWALRemoteAuthorityConflictNeverProducesRejected` 守 |
| 生成工程 `TestGeneratedDataEngineFencedStepSkipDoesNotFenceTheEntity`（`fenced_step_test.go`）与 `trade_fenced` handler | 承诺随机制消失；文件里其他用例共用的 `mongoState` / `memoryState` / `noFatal` 移到 `trade_fixture_test.go` |
| 测试替身的 `ProjectFenced` 包装（`projector_parallel_test.go`、生成工程 `pressure_test.go` / `trade_fixture_test.go` / `remoteflow`） | 改回包 `Project`（单条段现在只走 `Project`） |

**维护者决定（2026-10-29，“按推荐”）**

1. **`dataengine.startup_timeout` 缺省 30s → 60s**（第 2 笔）：遗留 Mongo 事务最晚 1.5 × `transactionLifetimeLimitSeconds` 回收，部署要求 20s 即最晚 30s，30s 期限没有余量。改了声明（`wiring/dataengine`，带说明）、生成器常量 `generatedDataEngineStartupTimeout`（打开单实例锁的服务，install.sh 的 `HEALTH_ATTEMPTS` 与 compose 的 `start_period` 随之加 30s）、`kitconfig_gen.go` 快照（重新生成）、文档。启动排空的有界重试跟随调用方期限（wiring 传 `startup_timeout`），2 分钟上限只用于没给期限的调用方：抽成 `startupDrainContext`，由 `TestStartupDrainDeadlineFollowsTheCallerAndCapsOnlyWithoutOne` 钉住；缺省值由 `TestDataEngineStartupTimeoutDefaultOutlastsALeftoverTransaction` 钉住。变异：缺省改回 30s、给了期限仍套 2 分钟、没给期限不套上限，三处各自变红。
2. **`saga.coordinators` 保持只写 sid**：部署规则“一个 sid 只能在一种服务类型里装 saga Mod”写进 SAGA.md「分区归属」、guide/06、impl/06、impl/13 与配置声明的说明。**没有加启动期检查**：每个服务类型的配置检查只看得到自己的配置；运行期能看到冲突的信号只有分区期望序号（已经 fail-stop）；可靠的跨类型检查要另建一份跨进程登记（带 TTL 就是模块自己的租约，与“App 级统一保证”相悖；不带 TTL 则迁移服务类型时要人工清理），不算便宜。

**验证**：本机 macOS；私有单节点副本集 mongod（`transactionLifetimeLimitSeconds=20`）。结果与命令见提交说明与交接报告。

### 13.5 第 6 批与第 7 批清理部分实施记录

范围：冷副本（§8，选项 C-A，缺省关闭）；第 7 批的代码清理提前做（维护者 2026-10-29：删原型目录、准备正式压测入口，**压测本身不跑**，维护者空闲时跑）；§11.2 的遗留项。分支 `feat/saga-js-batch6`，基于 main `c565c81b`，提交见 git log（冷副本 → 清理与压测入口 → 文档）。

**冷副本**（`framework/saga/cold_copy.go`）

1. **装配**：`AssemblyConfig.ColdCopy *ColdCopySettings`（Mongo 客户端放在 `ColdCopySettings.Client`），`Assemble` 签名不变（初稿写 `Assemble(log, js, raw, mongo, cfg)`：改签名要动全部调用方，而 Mongo 只在冷副本开启时需要）。每个进程都建 `ColdCopy`（读回落，经 `Options.ColdCopy` 交给 Engine）；只有拥有分区的进程另建 `ColdCopyProjector`，`Assembly.Start` 在协调器之后、启动中继之前启动它，`Stop` 在中继之后、协调器之前排空它。`wiring/saga`：`saga.cold_copy.{enabled,terminal_only,collection,batch_size,max_wait}` 声明在 `config` 结构体上（缺省 false / true / `sagas` / 256 / 50ms，`batch_size` 1～1000、`max_wait` 1ms～10s），`kitconfig_gen.go` 重新生成；Mongo 是可选依赖（`OptionalDependsOn` 加 `mongo`），开了冷副本却没有 Mongo Mod 由 `CheckConfigAcrossMods` 拒绝、`Provide` 再查一次能力。
2. **投影**：每条流一个 durable `ColdCopyDurable(stream) = <stream>-cold-copy`，消费者状态 R1 内存——`fnats.JetStreamConsumerConfig` 为此加了 `Replicas` / `MemoryStorage`（0 / false 跟随流，原有消费者不变，driver 映射到 nats.go）；过滤 `ev.done`（`terminal_only`）或 `<prefix>.*.ev.>`；坏信封（解码、版本、分区、内容校验与重放同一个 `decodeEvent`）Term。每条投递 `DeferSettle` 推迟结算、进批；一批不超过 `batch_size` 条或从第一条起 `max_wait`，同一 saga 取最新（`coldNewer`），一次 BulkWrite，**写成功逐条 ack、失败逐条 nak**（退避 250ms～10s，`MaxDeliver` 1e6），停止时手里没写的逐条 nak。AckWait 固定 30s（一批写入期限 10s）。
3. **版本门与初稿的差异**：初稿写“版本更大才整文档替换”。只比版本在 ID 复用时是错的：终态过了保留期被剪掉之后同业务键可以再开一笔（ID 相同、版本从 1 重来），新一生的终态版本常常小于旧一生，永远写不进去。改为先比 `created_at`（一生，只在启动时设定）、相同再比版本（`coldCopyNewer` 管道 `$replaceWith` + `$cond`，`coldNewer` 是同一规则的内存版）。两生之间至少隔一个 `terminal_retention`，协调器时钟回拨超过它不在威胁模型内（KNOWN-LIMITS）。
4. **回落读**：`Get` / `GetByBusinessKey` 只在所有者回答“内存里没有”时读 `ColdCopy.Get`；所有者不可用仍返回 `ErrPartitionUnavailable`（内存里可能有更新的版本）。`List`（初稿只写“由冷副本补”）：冷副本一页的 ID 随散播查询发给所有者（`queryRequest.Known`），所有者回答哪些在自己内存里（`queryReply.Held`，不论是否匹配条件），这些 ID 不用冷副本那条（落后的版本、或被新一生替代的旧一生）；结果截到冷副本已读的最后一条（按（更新时间，ID）的精确前缀），不够 `Limit` 再翻页，最多 4 页。**不参与判定**：启动判定补查（`submitIntent` 的 Get 补查）与 `WaitTerminal` 改用只看内存的 `getOwned`——否则冷副本里旧一生的记录（摘要不同）会被当成身份冲突、旧终态会被当成“下一个终态”。现有 Get / List / 唯一性断言一条未改（停止条件没有触发）。
5. **清理守卫**：清理本来就把流上全部 durable 当守卫（`DurableAckFloors`），冷副本 durable 落后时自然不越过它；另加一条：冷副本开启时**找不到它就按 floor 0 计、不清理**（`partition.purge`）——R1 内存消费者随承载它的服务端重启消失，那段时间清理会删掉它还没投影的终态。投影器每 5s 核对（订阅意外结束、或 `DurableAckFloors` 里没有它），重建订阅（DeliverAll，从流里还在的事件重投），计 `saga.cold_copy.resubscribed_total`。冷副本开启前已清理掉的终态不进冷副本；关掉冷副本后要运维删 durable（SAGA.md）。
6. **索引与文档形状**：`_id` + `(updated_at,_id)`、`(status,updated_at)`、`(type,updated_at)`、`(type,definition_version,status,updated_at)`；文档沿用原 `recordDoc`（去掉租约，加 `start_attempt`），时间存到毫秒，不设 TTL。

**测试**（单元 `cold_copy_test.go`，真实 `cold_copy_real_integration_test.go`）：投影攒批 / 版本只升 / 每条恰好结算一次、写失败逐条 nak 且不留文档、乱序重投与再重投幂等、停止时 nak、坏信封 Term；回落读只在内存没有时、所有者不可用不回落；`List` 排除内存持有的 ID（含旧一生）、按页补齐；判定补查与 `WaitTerminal` 不读冷副本；清理守卫三种情形（跟上 / 落后 / 缺失）与冷副本关闭时不是守卫。真实：Mongo 版本门（同一生旧版本在同一批与之后都不回退、新一生覆盖、旧一生迟到不回退）、全部字段往返、`List` 过滤 / 顺序 / 分页；协调器（带投影）子进程 kill -9 后同布局重启，3000 笔（751 笔补偿）全部终态后冷副本逐笔与所有者内存里的终态一致（最后一笔到终态后约 28s 追平，即被杀进程取走未确认的事件等 AckWait 30s 重投）；正式装配（`Assemble` + 冷副本）每 8 条事件快照时清理照常推进、删掉冷副本 durable 后清理停住、投影器重建后冷副本补齐且清理恢复、durable 是 R1 内存。

**变异**（必须变红，均已核对）：写失败仍逐条 ack（AckAll 同类：确认没写进去的投递）→ `TestColdCopyProjectorNaksEveryDeliveryOfAFailedBatchAndRedeliveryIsIdempotent` 红；内存版本门只比版本 → `TestColdCopyProjectorWritesTheNewestRecordPerSagaAndSettlesEachDelivery` 红；Mongo 版本门只比版本 / 去掉门 → `TestRealMongoColdCopyVersionGate` 红；清理时排除冷副本 durable → `TestColdCopyDurableGuardsThePartitionPurge` 红（落后、缺失两种）；判定补查 / `WaitTerminal` 用带回落的 `Get` → `TestStartVerdictPollIgnoresTheColdCopy`（得到 `ErrIdentityConflict`）/ `TestWaitTerminalIgnoresTheColdCopy` 红；所有者不回答 `Held` → 两个 `List` 用例红。

**第 7 批的代码清理（提前）**

1. 删除原型目录 `scripts/perf/saga-jslog-proto/`（Go 包 `jslogproto` 与 `run-perf.sh`、原型 `nats-cluster.sh`）；旧 `saga-scan.sh` / `saga.sh` 第 4 批已删。历史报告、样本与设计稿里的路径保留为历史，加了“已删除、`git checkout c565c81b -- scripts/perf/saga-jslog-proto` 取回”的说明（PROPOSAL、原型报告、投影器并行报告、DOCUMENTS、本文 §3.3 / §12）；性能 README 里旧 `saga.sh` 的字段一节标为历史口径。
2. 正式压测入口 `scripts/perf/saga-js-scan.sh`：自起私有三节点 NATS 与私有 mongod（`transactionLifetimeLimitSeconds=20`），开跑前查端口，结束删数据目录；`GOMAXPROCS=4`；每档前负载门；测试二进制编译一次，逐档调用 `framework/saga` 的 `TestRealSagaJSScanLevel`（放在 `jetstream_performance_integration_test.go`）；缺省 15 档（§12 的 13 个计时档 + 两次重放）；结果在 `artifacts/perf/saga-js/<label>/`。正确性门槛写在用例里（非期望终态、Nest 与 Mongo 逐账户余额、发布结果未知、积压 30s 归零）。**与 §12 的差异**：参与方是框架层正式路径（真实 Nest + nestwal WAL + DataEngine 投影 8 通道 + 实体内回执 + `SubscribeNativeStep`；Mongo 步骤 `SubscribeMongoStep` + `MongoStepInbox`），账户 DAO 与生成 DAO 同形但手写（第 3 批参与方用例的夹具），`wiring/saga` 的 `Steps` 胶水不在路径上——要用生成工程跑需要另起一个生成工程夹具，工作量另计。**只跑了冒烟**（`smoke`：一档 native-forward c2、3s，端到端 14s、清理干净），另各场景直接调用用例各 3s 确认能跑（原生正向 / 全补偿 / pipelined / Mongo 步骤 / 准入 / 快照 / 重放 600 条），数字不作结论。
3. §11.2 遗留：原型变异 `noadmit` 迁移为 `ROOST_SAGA_MUTATE=noadmit`（`TestRealJetStreamLoadPublisherDrivesAdmission` 在变异下红、正常绿）；mmorpg giftdrill 读 saga 库的问题第 4 批已改为经 `GiftStatus` 查分区所有者（`scripts/testdata/mmorpg/crosssid/main.go.txt`），本批核对无需再改。

**验证**：本机 macOS；私有三节点 nats-server 2.14.5（端口 29622 起）、私有单节点副本集 mongod 8.0（端口 29717，`transactionLifetimeLimitSeconds=20`）；命令与结果见提交说明与交接报告。

## 14. 需要维护者选择的点

**维护者决定（2026-10-28）：全部按推荐**——ID-A、W-A（终态保留 1h，业务键唯一性只在保留期内成立，`ManualRequired` 常驻）、P-A、R-A、N-A、M-A、RR-A、T-A、D-A、L-A、C-A、第 12 项 A（不顺延）。按 §13 从第 1 批开始实施。

1. **saga ID 与业务键**。ID-A（推荐）：ID = `IDOf(Type, BusinessKey)`，删 `StartRequest.ID`，业务键查询就是按 ID 查，唯一性与推进在同一个单写者里。代价：demo 赠礼与 gm 改用业务键 / `IDOf`；saga ID 不再可由业务指定。ID-B：保留业务给的 ID，另建 KV 业务键索引桶。代价：每次启动多一次 R3 写，索引与事件不在同一原子边界，崩溃窗口要另做修复。
2. **终态保留期 / 业务键唯一性窗口**。W-A（推荐）：一个键 `saga.terminal_retention` 缺省 1h，期间 Get / Lookup 可读、同业务键去重、迟到成功可补偿；`ManualRequired` 常驻；之后同业务键可开新单。代价：唯一性不再永久（现行由 Mongo 唯一索引永久保证），内存 = 速率 × 保留期 × 记录大小。W-B：两级（完整记录 10m + 去掉 Data 的精简记录 24h），唯一性与迟到补偿窗口更长、内存更省。代价：多一种记录形态与剪枝规则，Lookup 在精简期拿不到 Data。
3. **分区归属**。P-A（推荐）：静态按 `saga.coordinators` 分给 sid，App 单实例锁 + 期望序号；协调器进程挂了分区停到重启。代价：没有自动接管。P-B：原型的 KV TTL 分区租约，存活实例自动接管。代价：模块自己的租约 / 状态机（与“App 级统一保证”规则相悖），多一套接管与再平衡代码。
4. **准入拒绝的表示**。R-A（推荐）：拒绝是逐尝试的判定回复，不写日志、不建记录；`Status` 枚举与 “`ErrNotFound` = 未开始”语义不变；满足 §12b（拒绝只到触发它的尝试）。代价：拒绝不留持久痕迹（只有指标）。R-B：原型同形，拒绝写成 `Rejected` 终态记录（带 `StartAttempt`），版本接着往上走。代价：公开 `Status` 多一个值，Lookup / List / gm 都要处理“Rejected = 未开始”，被拒时 Get 有记录。
5. **原生步骤的回执检查入口**。N-A（推荐）：生成的 Nest 包装在业务 handler 之前调用 `step.Begin(target)`，业务 handler 不变。代价：`codegen/internal/nest` 认 `saga.NativeStep` 参数，生成器多一处特判。N-B：业务 handler 第一行手写 `Begin`。代价：漏写就是重复扣款，只能靠 doctor / glsvet 提示。
6. **Mongo 步骤收件箱**。M-A（推荐）：按操作回执文档 + Mongo 事务写冲突串行，删除 Reserve 事务、操作状态文档与租约（约 500 行）。代价：同一操作的并发尝试可能都执行一遍 handler（后者回滚），事务外副作用本来就要求幂等。M-B：保留现行操作状态文档与租约，只把结果改发到分区。代价：保留一套与原生步骤不同的机制与 527 行代码。
7. **回执保留期**。RR-A（推荐）：`saga.receipt_retention` 缺省 7d，`Resume` / 补偿方向的 `Compensate` 遇到 `UpdatedAt` 早于“现在 − 保留期 + 最长步骤窗口”的记录返回 `ErrReceiptsExpired`。代价：超过一周的人工 Resume 需要运维先核对业务数据再走业务冲正。RR-B：30d（现行 `completion_receipt_ttl`），不加守卫。代价：回执在实体文档里多存 4 倍时长，活跃玩家文档变大（每次操作约 100 字节）。
8. **遗留 Mongo 事务 60s**（KNOWN-LIMITS“尚未确认”那条）。T-A（推荐）：部署参数 `transactionLifetimeLimitSeconds=20`（SAGA.md 现有推荐）+ 参与方启动只读探测告警 + DataEngine 启动排空有界重试 + 红绿证据。代价：依赖部署参数，托管库可能不让探测。T-B：缺省步骤预算拉到覆盖 60s（例如 `max_attempts` 12）。代价：所有 saga 的失败发现变慢一倍以上。T-C：重启时主动终止被杀进程遗留的事务。代价：服务端没有按“已死进程”终止会话的安全手段，`killAllSessionsByPattern` 按用户会误杀同用户的活进程，不建议。
9. **Redis 基础设施删除范围**。D-A（推荐）：连同 `wiring/redis` 权威 Mod、`infra/storage/redis` 的权威探针、`DurableScriptBatch`、Stream 读、AOF 测试工具一起删。代价：以后要用再从 Git 历史取回。D-B：只删 saga 用法，保留 infra 通用件。代价：留下无调用方的代码与测试。
10. **DataEngine lease fence 删除时机**。L-A（推荐）：单独第 5 批（DataEngine 三大块之一，单独审、单独跑 `-race`）。代价：第 4 批之后有一段时间 DataEngine 里留着无人使用的分支。L-B：并入第 4 批。代价：切换批更大，DataEngine 改动混在 saga 切换里难审。
11. **冷副本写在哪里**。C-A（推荐）：协调器进程内、缺省只写终态。代价：与协调器同盘时有争用（原型第二轮），需要时运维开在别的机器上的协调器 sid。C-B：独立进程（新 Mod）。代价：多一个部署单元与配置段。
12. **选主停顿后顺延截止**（可选，不推荐）：A：不做（推荐），停顿后可能多一次重试。B：发布停顿超过 1s 时把该分区在途尝试的截止顺延停顿时长。代价：协调器与参与方的截止不再一致，需要论证迟到成功的频率不增加。

## 15. 工作量估计

| 批 | 内容 | 估计（工作日） |
| --- | --- | --- |
| 1 | JetStream 日志原语与语义用例 | 2～3 |
| 2 | 协调器（纯函数抽取、分区循环、重放 / 快照 / 清理、准入、查询、等待者）与真实端到端 | 6～8 |
| 3 | 参与方新路径与生成器（回执字段、Nest 包装） | 4～5 |
| 4 | 切换、删除、demo、生成器、文档、测试迁移、生成工程验收 | 6～7 |
| 5 | DataEngine lease fence 删除 | 1～2 |
| 6 | 冷副本 | 1～2 |
| 7 | 性能验收与报告 | 1 |
| 合计 | | **21～28** |

与原型稿 §16 的差异：分区归属走静态 sid（P-A）省掉租约与再平衡（约 3 天）；不做 Linux 跨机复测（维护者已取消）；测试迁移量大，单独算进第 4 批。代码量粗估：删除约 1.1 万行（Redis store / lua / Store Service / 权威基础设施约 3500 行、旧收件箱与 completion 消费者约 900 行、DataEngine fence 约 600 行、相关测试约 6000 行），新增约 5000～6500 行（协调器约 2500、driver 约 600、参与方与生成器约 900、测试约 2000～2500）。实施后按实际 diff 如实报。

## 16. 风险与未核实项

- **saga 是已知反复出问题的模块**（完成判定、迟到成功、代际已多轮修正）。本设计把判定逻辑原样抽成纯函数复用，测试按承诺迁移而不是重写断言；若第 2 批抽取后现有 saga 测试有任何一条需要改断言才能过，停下报告。
- **测试大面积迁移**：§11.1 约 30 个文件；迁移前后按维护手册 §7 做语法摘要与逐函数覆盖率对比，删除的用例写明覆盖它的保留用例。
- **单流 Raft 组吞吐与选主停顿**：多流缓解，跨机未测（维护者已接受）。
- **清理被最慢的 durable 牵制**：告警 + `MaxBytes` 拒新写兜底，没有自动跳过。
- **事件携带完整状态**：Data 大时日志膨胀；保持 `max_payload_bytes` 缺省 64 KiB 的约束。
- **原子批量的服务端上限（第 1 批已核实）**：
  - 来源：nats-server 源码 tag v2.12.0、v2.12.1、v2.14.5 的 `server/stream.go`（`streamDefaultMaxBatchSize = 1000`，2.14 改名 `streamDefaultMaxAtomicBatchSize`；`streamDefaultMaxBatchInflightPerStream = 50`、`...InflightTotal = 1000`、`streamDefaultMaxBatchTimeout = 10s`，均可由 `jetstream { limits { max_batch_size / max_batch_inflight_per_stream / max_batch_inflight_total / max_batch_timeout } }` 调整）、`server/jetstream_batching.go`（批内每条按 `max_payload` 与流的 `max_msg_size` 检查，没有批总字节检查）、`server/jetstream_errors_generated.go`（10199 “atomic publish batch is too large: {size}”、10176、10201 自 2.12.1 起）；实测（本机 macOS、nats-server v2.14.5、私有三节点 R3，`TestRealLogAtomicBatchLimits`）：1000 条一批成功，第 1001 条服务端回 `{"err_code":10199,"description":"atomic publish batch is too large: 1000"}`；256 × 256 KiB = 64 MiB 一批照常提交（约 145ms）；一条流同时打开 50 个未提交批时新批收到 10176，等被丢弃的批 10s 超时后恢复。
  - 结论：**单批 ≤ 1000 条（驱动发送前就按 `LogBatchMaxMessages` 拒绝，返回 `ErrLogBatchTooLarge`，saga 按此切成几次顺序 fenced 发布）；没有批总字节上限，单条 ≤ max_payload（缺省 1 MiB）**。2.12.0 不可用（批内 MsgID 被拒），最低 2.12.1。
  - **对第 2 批的影响**：① 一条流同时在途的批数上限 50：每个分区同时最多一次发布在途，所以每条流的分区数 `ceil(partitions / stream_groups)` 必须 ≤ 50，否则高负载下批会被服务端丢弃（10176，确定没写入，重发可恢复但会空转）；建议在 `wiring/saga` 的配置检查里加这条（缺省 16 / 4 = 4，远低于上限），并把 10176 按“确定没写入、立即重发”处理而不是结果未知。服务端总上限 1000 是整台服务端所有流共用的，同一 NATS 上有别的原子批用户时要一起算。② 批从首条到提交最长 10s：saga 发布不会接近，但 `publish_timeout` 应远小于它。③ 流满（`MaxBytes` + Discard=New）时发布返回 `ErrLogRejected`（确定没写），saga 应直接 fail-stop 告警，不当结果未知重试。
- **第 2 批发现、对第 3、4 批有影响**：（1）清理守卫是流上全部 durable 的 ack floor：参与方的 durable 只过滤自己主题的命令，空闲时 ack floor 是否随流前进——**第 3 批已核实：不前进，会挡住清理；已在驱动修复**（§13.2 第 9 条）；（2）拒绝的持久性（§13.1 第 9 条）：维护者 2026-10-28 已定 A，SAGA.md 措辞在第 4 批；（3）MsgID 必须带分区号、写者身份每次进程启动不同（`<sid>-<SingletonIncarnation>`），第 4 批接线时由 wiring 填 `Writer`。
- 未核实、实施时确认（第 3 批按源码与实测核实，2026-10-28）：
  - `nest.AfterCommit` 在 pipelined 下的时机：**在本事务的 WAL 票据落盘之后、回复之前**（`execution.go` `commitPipelined`：`<-ticket.Done()` 后 `tx.commit(true)`；完成泵路径 `pipelined_completion.go` 同样先 `commit(true)` 再回复）。strict 在 Guard 释放回调里执行，也在回复之前（`runNestLogic` 的 defer 先释放 Guard 作用域，`replyDispatch` 在它返回之后）；带 Remote 批次时排到批次最终确认之后。**例外**：记录为空的事务没有票据（`pipelinedEnqueue` 对空记录返回 nil），AfterCommit 立即执行——所以原生步骤回放时重写回执（§13.2 第 3 条）。
  - Nest 是否暴露“本事务有 Remote 批次”：**原来没有**（只有按实体问的 `CurrentRemoteWriteBatchContains(id)`），本批加只读的 `nest.CurrentDispatchHasRemoteWriteBatch()`，`Complete` 据它返回 `ErrNativeStepRemoteWrite`；`entity.InspectReceiptHost` 另在生成期拒绝 `remote=managed` 实体。
  - DAO 生成器对 `map[string][]byte`：**直接支持**（`KindMap`，值类型 `[]byte`，生成 `fmap.SmallSafeMap[string, []byte]` 与 `Get/Set/Del/Range<字段>`、按键的补丁路径 `saga_receipts.<操作>`；生成物 `go vet` 通过）。操作键 `<id>:<方向>:<步骤>` 不含 `.` / `$`，走按键补丁。存的切片不复制，调用方不得修改（框架只整份替换）。存量文档没有这个字段时加载为空 map，项目未上线，不做迁移。
  - `transactionLifetimeLimitSeconds` 探测：`fmongo.IMongo` / `IDatabase` **没有执行任意命令的接口**，探测要在第 4 批给驱动加一个只读入口（**第 4 批已加 `IMongo.GetParameter`**）。共享隔离环境（自建副本集）上 `getParameter` 可执行、值为 60（解释了本批 kill -9 实测一次 64s 的重启：被杀进程的投影事务持锁到期才回收）；托管库是否允许 `getParameter` **尚未确认**（手头没有托管实例）。
  - `wiring/dataengine` 启动排空：`engine.Runtime.Start` 只调一次 `Projector.Flush(ctx)`，投影器在 ctx 内按退避重试写冲突（实测日志里同一段在截止前反复重试）；够不够取决于 Mod Start 的 ctx 上限，**第 4 批按 T-A 第 3 条核对并补有界重试**。本批的 kill -9 夹具自己重试到 3 分钟。
- **第 3 批发现、对第 4 批有影响**：
  - （1）**原生步骤迟到成功的补偿载荷为空**（第 4 批已按维护者选的 A 修复，§13.3）（新旧两条路径都有，原登记于 KNOWN-LIMITS）：原生步骤的结果不带 `Data`，`lateStepState` 记 `LateData = completion.Data`（空），补偿迟到那一步的命令载荷就是空的；demo 的 `gift.Decode` 解不开，补偿被拒 → `ManualRequired`，方向 ④ 对原生步骤实际不自动生效。A（推荐）：原生步骤成功时 `Completion.Data = Command.Payload`（正向那一步的载荷就是 `record.Data`，Data 链的值不变，`LateData` 就是那一步自己的载荷；代价是结果输入多带一份载荷，受 `max_payload_bytes` 约束）；B：`planDue` 在 `LateData` 为空时退回 `record.Data`（改两种协调器共用的纯函数，第 2 批的差分用例要补）。
  - （2）清理守卫的 floor 取“列出消费者之前读到的流末序号”：消费者 leader 在落后的副本上时，可能对流 leader 已有、它还没应用的命令报“没有待投递”，那条命令可能被清掉——只影响活性（协调器按步骤超时重发），**尚未确认**会不会发生（取决于副本应用与清理在 Raft 日志里的先后），KNOWN-LIMITS 已登记。
    **2026-10-29 已确认并修复**（`infra/network/nats/driver/jetstream_log.go` `DurableAckFloors` / `durableAckFloors`）：
    - **源码结论**（nats-server v2.12.1 与 v2.14.5 相同）：① ConsumerInfo 由消费者 leader 回答（`consumer.go` `setLeader` 只在 leader 上订阅 `$JSC.CI`；列表由 meta leader 向各消费者 leader 发 `$JSC.CI` 收集，`jetstream_cluster.go` `jsClusteredConsumerListRequest`）。② 消费者 leader 由消费者自己的 Raft 组选出，候选是流的全部副本、首选随机（`createGroupForConsumer` / `raftGroup.setPreferred`），与流 leader 无关；R1 消费者只在一个随机副本上。③ `NumPending` 是消费者 leader 的缓存计数 `npc`：当选时按**本机流副本**算（`streamNumPending` → `calculateNumPending` 读 `o.mset.store`），之后本机副本存下消息、经异步信号队列（`stream.go` `processJetStreamMsg` 末尾 `sigq.push` → `signalConsumersLoop` → `consumer.go` `processStreamSignal`）才加一；`checkNumPending` 只在 `npc > 0` 时按本机 `FastState` 收紧。本机副本落后、或信号还没处理时，它对流 leader 已有的消息报 0。④ `AckFloor` 取消费者 store 的状态（`infoWithSnapAndReply`），是消费者 Raft 状态，与流副本无关。⑤ `$JS.API.STREAM.MSG.GET` 只由流 leader 回答，组内没有 leader 时回 10008、不由副本代答（`jetstream_api.go` `jsMsgGetRequest`）；Raft leader 处理完当选前的条目才对上层报 leader（`raft.go` `switchToLeader` 设 `aflr`、`Processed` 到达后才置 `leaderState`）。⑥ 列表里 4s 内没回答、或有 offline 原因的消费者只进 `missing`；承载它的副本全部离线的以零状态（只有配置）同时出现在 `consumers` 与 `missing` 里。nats.go v1.53.1 的 `ListConsumers` 不看 `missing`，翻页按收到的条数前进：没回答的 durable 排在别的消费者之前时一次都不出现，排在最后时一直翻页到 ctx 结束。
    - **结论**：会发生。Raft 日志里命令在清理之前，但消费者投递与副本应用不同步：落后副本应用这条命令之后、消费者取到它之前，同一副本接着应用清理，命令就没了。后果比原登记的更宽：步骤命令丢了靠协调器超时重发（活性）；**冷副本 durable**（R1，2/3 概率在 follower 上）丢的是还没投影的终态事件，冷副本永远缺这条；没回答的 durable 从列表里消失时根本不守卫；R1 冷副本所在节点离线时它以零状态出现、被当成“没有待投递”。
    - **实测**（本机 macOS，nats-server v2.14.5 私有三节点，`TestRealLogAckFloorsDoNotTrustALaggingConsumerReplica`：R1 durable 放在流 follower 上，SIGSTOP 这个节点，经另两个节点写 30000 条积压 + 一条匹配它过滤的 S，再写 3 条）。修复前每次跑到断言都红（4 次）：暂停中 `floors = map[zz-idle:30004]`（step 不在列表里），恢复后第一次回答 `floors = map[step:30004 zz-idle:30004]`，S=30001；最早一版用例只有一个 durable，暂停中列表翻页到 ctx 超时（60s 内一直报 `context deadline exceeded`）。另有 3 次连跑时在建流就失败（留下输出的那次是 10008：上一次暂停后集群还在选主），之后用例在清理时恢复节点并等集群就绪。修复后 3 次全绿，恢复后追赶期间消费者信息连续 17～83 次报“没有待投递”，floor 一直在 S 之下，按 floor 清理后 S 还在、durable 取得到。
    - **修复**：floor 不再用 `NumPending` / `NumAckPending` 与 nats.go 的列表。① 先读流末 `last`（StreamInfo）；② 自己翻 `$JS.API.CONSUMER.LIST`（按出现过的不同名字前进，每页 total 一致、最后名字数等于 total，否则报错、这一轮不清理），每个 durable 取 `AckFloor.Stream = a`；③ 向流 leader 要过滤匹配、序号 ≥ a+1 的第一条（`MSG.GET` 的 `seq` + `next_by_subj`，多个过滤取最小）：有，序号 n，floor = max(a, n−1)；没有，floor = max(a, last)。第 ③ 步在 ① 之后，“没有”才能推出 ≤ last 的匹配消息都已确认；n 只要求回答者存的是日志前缀。没回答的消费者 floor 记 0；全部副本离线、配置表明是临时 / 有序消费者的不列。空闲过滤 durable 不挡清理（第 3 批的修复）保持；还没追上的 durable 的 floor 从 a 提到 n−1（只会更高、仍正确）。
    - **剩下的边界**见 KNOWN-LIMITS“清理守卫的 floor”：依赖回答的是当前流 leader（被罢免的旧 leader 在网络分区里的旧读不防）；R1 冷副本 durable 所在节点不回来时清理一直停住，要运维删 durable（是否自动删除待维护者决定）。
    - **回归**：单测 `TestDurableAckFloorsDoNotTrustTheConsumerReplicaView`、`TestDurableAckFloorsHoldUnansweredConsumersAtZero`（修复前的判定原样放到同一接口上时红：`step:8`（应为 3）、`step` 缺失、`offline-durable:3`（应为 0））、`TestDurableAckFloorsReadTheStreamEndFirst`（读序守卫，修复前后都绿）；真实三节点 `TestRealLogAckFloorsDoNotTrustALaggingConsumerReplica`（没看到落后窗口时失败，提示加大积压）；`TestRealLogReadFollowPurgeAndAckFloors` 的期望随 n−1 规则从 `guard=8` 改为 `guard=9`（p0、p1 交替，9 是 p0）。nats-server 升级时这几条与 `TestRealLogAckFloorsOfIdleAndLaggingFilteredDurables` 一起跑：服务端改了 `missing` / `MSG.GET` 的语义会在这里变红。
  - （3）参与方长时间停机后的重试积压：协调器每个步骤超时都派一次新尝试，停机 60s、300 笔在途就是约 5000 条过期命令；回来后每条都要经 Nest 跑一次 Begin（查回执、过期 ack），实测约 1～3s 排空，之后正常。量大时可以考虑按 `Attempt` 合并，暂不做。
  - （4）拒绝的持久性（§13.1 第 9 条）：维护者 2026-10-28 确认 A，代码不改，SAGA.md 措辞在第 4 批（已改）。
  - （5）（第 4 批已做）第 4 批接线时 demo 的 PlayerDao 要加 `SagaReceipts` 与 `saga_receipts.go`（生成工程由 `add saga -native` 自动带上，demo 是参考实现、手写）。
- **第 4 批发现**：
  - Mongo 服务端每半个 `transactionLifetimeLimitSeconds` 扫一次过期事务，遗留事务最晚 1.5 倍参数才回收（私有 mongod 实测，参数 20s 时 27～29s）；`dataengine.startup_timeout` 缺省 30s 的余量只有被杀到重启的时间——**第 5 批已按维护者 2026-10-29 决定调到 60s**（§13.4）。
  - 驱动的 `WithTransaction` 在一次调用里重跑写冲突到 `mongo.transaction_timeout`（缺省 30s）；`Runtime.Start` 原来只调一次 Flush，`startup_timeout` 大于事务期限时多出来的时间用不上——已改为 `drainAtStartup` 重试。
  - `saga.coordinators` 只写 sid：两种服务类型用同一个 sid 且都装 saga Mod 时认领同样的分区，单实例锁按“类型 + sid”挡不住，期望序号隔离会让其中一个 fail-stop；SAGA.md 写了部署约定，是否改成 `<server_type>:<sid>` 待维护者决定。——**维护者 2026-10-29 决定保持 sid**，部署规则写进 SAGA.md，未加启动期检查（§13.4）。
