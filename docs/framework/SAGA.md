# Roost Saga

Saga 用于跨越多个独立事务域的业务流程。单个 Nest handler、同一 MongoDB
事务或 `RemoteWriteBatch` 能覆盖的操作不应使用 Saga。

设计与取舍：[DESIGN-2026-10-28-saga-jetstream-authority](../maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md)
（维护者 2026-10-28 决定：协调器权威只保留 JetStream，Redis 权威、Store Service 与 `saga.redis.*` 在第 4 批删除）。
实现入口见 [impl/06](impl/06-saga.md)，使用见 [guide/06](guide/06-saga.md)。

## 一致性模型

每个步骤由本地事务提交，跨步骤提供最终一致性。失败时按相反顺序执行业务补偿，
补偿不是内存状态回滚。推荐把资源状态设计为 `available -> reserved -> consumed`
或 `reserved -> released`，并以 `Command.IdempotencyKey` 作为业务唯一键。

协调器的边界：

- 每个分区一个单写者（分区所有者进程里的一个循环）；saga 的每次推进 = 往分区事件 subject 追加一条“新状态 + 要派发的命令”的事件，
  **PubAck 之后才改内存、才回复**。命令就是事件本身，没有 outbox；
- 同一 attempt 的消息重投共享 `CommandID`，跨 attempt 共享稳定 `IdempotencyKey`（操作实例，见「原生步骤执行契约」）；
- 旧写者靠**期望序号**隔离：每次发布的首条带“分区事件过滤的期望序号 = 内存记的最后一条事件序号”，被别的写者推进过就被服务端拒绝，
  本进程经 `RuntimeFailure` 整体 fail-stop（App 级统一保证）；
- 按 `CommandID` 的回执摘要与状态推进在同一条事件里（同一 CommandID 不同结论 = 身份冲突）；
- operation 关闭时在 saga 状态里记 tombstone（关闭方式：带结果 / 放弃），随 saga 的保留期一起剪；
- 重复、迟到的 command/result 不会重复推进状态；协调器放弃一个**正向**步骤之后才到达的成功说明它已生效，协调器把 saga 带回补偿、
  只补偿这一步（`Failed` / `Compensated` 会被重开，saga 方向 ④）；补偿方向的只告警（计数 `saga.completion.late_after_abandon_total`、ERROR 日志）。

全部线上消息（分区事件、输入、快照清单、准入信号、Nest 启动 effect）使用严格的 `WireVersion=2`，不兼容 1（已删除的 Redis 协调器），
不通过猜测字段做隐式兼容。坏输入（解码失败、版本不对、内容校验不过）在分区里计 `saga.partition.rejected_input_total{kind,reason}` 并跳过
（日志里的输入不能 Term，输入水位照常前进）；步骤消费者与启动中继对坏信封 Term，计 `saga.consumer.rejected_total{consumer}` 并记 ERROR。
启动中继对 `StartSaga` 的确定性拒绝（`ErrInvalidRecord`、`ErrIdentityConflict`）同样 Term，计 `saga.start.rejected_total{saga_type,reason}`；
`ErrDefinitionMissing`、`ErrBackpressure`、`ErrStartUnconfirmed` 按可重试错误 nak。

## 协调器：JetStream 分区事件日志

**前提**（维护者确认）：JetStream R3 文件存储，收到 PubAck 即不丢（整集群同时断电不在范围）；NATS 服务端 ≥ **2.12.1**
（原子批量带 MsgID；`EnsureLogStream` 强制，低于它拒绝启动）；协调器与 NATS 同子网。Mongo、etcd 都不在协调器的关键路径上。

**布局**（`saga.subject_prefix` / `saga.stream` / `saga.partitions` / `saga.stream_groups`，全部进程相同）：saga ID 恒为
`IDOf(Type, BusinessKey)`（`sha256` 的前 128 位十六进制），分区 = FNV-1a(ID) mod P；分区 k 在第 k mod G 条流 `<stream>_G<g>` 上。
每条流的分区数 `ceil(P/G)` 必须 ≤ 50（服务端一条流同时在途的原子批上限，`Layout.Validate`，配置检查与协调器启动同一个函数）。
第一个启动的所有者把布局登记在快照桶的 `layout` 键，之后不一致的进程以 `ErrLayoutMismatch` 拒绝启动。**改 P / G 只能在全部 saga
终态、停全部协调器、删流与桶之后**，不支持在线改。

| subject | 内容 |
| --- | --- |
| `<prefix>.<k>.ev.cmd.<topic>` / `.ev.done` / `.ev.none` | 分区事件：带命令 / 终态 / 无命令的变化（事件携带这笔 saga 的完整新状态） |
| `<prefix>.<k>.in.start` / `.in.res` / `.in.op` | 输入：启动意图、步骤结果、运维意图（Resume / Compensate） |
| `<prefix>.q.<k>` / `<prefix>.q.list`（核心 NATS，不进流） | 所有者查询：Get / 判定补查；List 散播 |

流配置：文件存储、R = `saga.replicas`（**生产必须 3**）、`AllowAtomicPublish`、`AllowDirect`、去重窗口 `saga.duplicate_window`、
**`MaxAge=0`、`Discard=New`**、`MaxBytes=saga.stream_max_bytes`：权威日志不按时间或容量静默删旧，满了拒新写（协调器 fail-stop 告警），
只由快照后的清理删除。

**分区归属**（选项 P-A）：`saga.coordinators` 是跑协调器的 sid 列表；排序后本进程 sid 的下标 i，拥有 `{k : k mod len == i}`。不在列表里的
进程只做发起方（`StartSaga`、查询、`WaitTerminal`）。进程级唯一由 App 单实例锁保证——**协调器进程在生产必须 `singleton.enabled=true`**
（配置检查强制）；崩溃重启的短暂重叠由期望序号隔离、旧进程 fail-stop。写者身份（进事件的 MsgID）是 `<sid>-<单实例锁本次启动的 token>`。
没有分区租约、没有自动接管：一个协调器进程挂掉，它的分区停到它重启，在途 saga 的超时在重放后按截止时刻继续。改 `coordinators` 列表 =
改归属：按运维手册停全部协调器再起（不停也安全，但会有一轮 fail-stop）。

**部署规则：一个 sid 只能在一种服务类型里装 saga Mod**（维护者 2026-10-29“按推荐”：`saga.coordinators` 保持只写 sid）。列表只写 sid，
App 单实例锁按“服务类型 + sid”取锁，两种服务类型的同号进程（例如 game 1001 与 world 1001 都装了 saga Mod）都会认为自己拥有同一组分区、
同时写同一条分区日志，其中一方被期望序号隔离（`saga.partition.fail_stop_total{reason="fenced"}`）、整个进程 fail-stop，重启后可能反过来
隔离另一方。规则是：**装 saga Mod 的服务类型之间 sid 不重叠**；只做发起方的服务类型（sid 不在 `coordinators` 里）不受限。没有启动期检查：
每个服务类型的配置检查只看得到自己的配置，看不到别的服务类型是否也装了 saga Mod；可靠的跨类型检查需要另一份跨进程登记（与 App 级统一
保证的规则相悖），所以这条靠部署约定，违反时的表现就是上面的 fenced fail-stop。

**发布结果**：`nil` = 已持久；结果未知（没收到答复）→ 同 MsgID、同期望序号重发，按“Duplicate / 批重复 10201 / 分区过滤上最后一条是自己的
= 已提交”判定，超过 `saga.publish_unknown_budget`（缺省 30s，**≥ 3 × 7s 选主停顿**）→ `ErrPublishUnknown` fail-stop；批不完整（10176）立即重发；
服务端明确拒绝（流满等）→ `ErrPartitionRejected` fail-stop；期望序号不符且最后一条不是自己的 → `ErrPartitionFenced` fail-stop。去重窗口必须
≥ 2 × 预算。选主期间（实测 5～7s）这条流上的分区不推进；步骤缺省 `Timeout` 5s 短于停顿，停顿后可能多一次重试（新尝试读到回执回放），不影响正确性。

**快照、保留与清理**：每 `saga.snapshot_every_events`（2000）条事件或 `saga.snapshot_interval`（30s，期间有事件）做一次快照：循环里只浅拷贝已提交
状态，交给分区自己的快照 goroutine 编码、写对象存储（`<STREAM>_SNAPOBJ`）、按修订号 CAS 清单（`<STREAM>_SNAP` 的 `p<k>`）；CAS 冲突读回、
更新的不是自己 → fail-stop。加载时校验版本、分区、布局、对象长度与 SHA-256，不符 `ErrSnapshotIncompatible`，分区不启动、**不退回从头重放**
（日志已按快照清理）。终态（Completed / Compensated / Failed）在内存与快照里留 `saga.terminal_retention`（缺省 1h）后剪掉；**`ManualRequired`
与非终态常驻**。清理 `purge(below = min(输入水位+1, 最后一条事件, 每个 durable 的 ack floor + 1))`，保留最后一条事件；某个 durable 卡住超过
`saga.purge_stall_alarm`（10m）计 `saga.log.purge_blocked_seconds` 并 WARN，不跳过它——**下线的步骤要由运维删掉它的 durable**，否则清理一直被挡、
流最终满了拒新写。

**内存**：保留期内终态笔数 × 单笔记录大小，分散在全部协调器进程上。按 700 笔/秒、记录 1 KiB、1h 约 2.5 GB；业务 Data 小、速率低时按比例缩小。
配置检查不替部署算内存。

**冷副本（可选，缺省关闭，设计 §8 选项 C-A）**：`saga.cold_copy.enabled: true` 时，**协调器进程**（sid 在 `coordinators` 里）在每条流上建一个
durable `<stream>_G<g>-cold-copy`（消费者状态 R1、只放内存），缺省只过滤 `ev.done`（`terminal_only`），一批至多 `batch_size`（256）条、最多等
`max_wait`（50ms），同一 saga 只写这批里最新的一条，一次 BulkWrite 写进 `<saga.database>.<saga.cold_copy.collection>`（缺省 `sagas`）。每条写是
**版本门**控的整文档替换 upsert：同一生（`created_at` 相同）版本更大才替换；终态被剪掉后同业务键重开的一生（`created_at` 更晚、版本从头来）
盖住旧一生；重投、乱序、多个协调器进程同时投影都幂等。**写成功后逐条 ack、写失败逐条 nak 重投**，不用 AckAll（原型里 AckAll 把前一个进程取走
没确认的事件一并确认掉、冷副本永远缺那几条）。冷副本只供读回落（见「API」）：**不参与唯一性、启动判定、判定补查与 `WaitTerminal`**，Mongo
不进任何判定路径。

- **它是清理守卫**：分区清理不越过冷副本 durable 的 ack floor；**找不到它时不清理**（R1 内存消费者随承载它的服务端重启消失，投影器每 5s
  核对、发现后重建并从流里还在的事件重投，`saga.cold_copy.resubscribed_total`）。冷副本写不进 Mongo 时它的 floor 停住、清理也停住
  （`saga.cold_copy.write_failures_total`、`saga.log.purge_blocked_seconds{consumer="…-cold-copy"}`），流最终满了 fail-stop——冷副本开着就
  要求 Mongo 可写。
- **关掉冷副本后要删掉每条流上的 `-cold-copy` durable**（与下线步骤同一规则），否则它们一直挡住清理。开启之前已经被清理掉的终态不进冷副本。
- kill -9 协调器：被杀进程取走、没确认的终态事件在 AckWait（30s）后重投给重启的进程，冷副本最终与权威终态一致（真实用例
  `TestRealJetStreamColdCopyKill9Converges`，3000 笔里最后一笔到终态后约 28s 追平）。冷副本文档不过期，需要时由运维按 `updated_at` 清理。
- 本进程没有 Mongo Mod 时开启冷副本，配置检查拒绝（`CheckConfigAcrossMods`）。

**重放（先恢复再服务）**：所有者启动时读快照，从 `min(输入水位, 最后一条事件)+1` 读到分区过滤上的最后一条（`LastMsg` 的序号，确定终点）：事件只取快照之后的、
按序覆盖；输入只留水位之后的、交给循环重新处理（都幂等）；时间轮按记录的 `NextRunAt` 重建。Mod Start 在全部拥有的分区重放完才返回（上限 2 分钟）。

## API

| API | 语义 |
| --- | --- |
| `StartSaga(ctx, StartRequest)` | 追加启动意图到分区 `in.start` 后等所有者的判定：接受或同一意图已存在 → 返回记录；同业务键不同意图 → `ErrIdentityConflict`；准入关闭 → `ErrBackpressure`；所有者缺定义 → `ErrDefinitionMissing`；ctx 到期仍无判定 → `ErrStartUnconfirmed`（意图已持久、结果未知，**用同一请求重试安全**） |
| `Get` / `GetByBusinessKey` | 由分区所有者从内存回答；内存里没有（还没启动、或终态过了保留期）时，冷副本开启则回落读冷副本，否则 → `ErrNotFound`；所有者不在 → `ErrPartitionUnavailable`（**不拿冷副本顶替**：内存里可能有更新的版本） |
| `List(Query)` | 向全部分区所有者散播并合并；覆盖非终态、保留期内的终态与全部 `ManualRequired`；冷副本开启时再补上**内存里没有的 ID**（更早的终态）：冷副本的一页 ID 随散播发给所有者，所有者回答哪些在内存里，那些 ID 以内存为准；结果是按（更新时间，ID）排序的精确前缀，冷副本一页被内存持有的 ID 占掉时最多再翻 3 页，之后可能少于 `Limit` 条（按 `UpdatedBefore` 缩小范围再查）。有分区没回答 → `ErrPartitionUnavailable`（点名），不返回残缺结果 |
| `Resume` / `Compensate` | 追加运维意图到 `in.op` 并等判定（规则见「失败语义」）；记录最后一次更新早于“回执保留期 − 最长步骤窗口” → `ErrReceiptsExpired`（选项 RR-A） |
| `Complete(ctx, Completion) error` | 把结果追加到所属分区 `in.res`（MsgID `res.<CommandID>`），只承诺“已持久进入输入”；框架自带的步骤消费者直接发布，它留给自定义参与方 |
| `WaitTerminal(ctx, id)` | 等这个 ID 下一个终态；同一 ID 的多个等待者全部唤醒 |
| `AdmissionState()` | 本进程看到的准入状态（健康检查与测试用） |

**`ErrBackpressure` 的含义是“这次启动现在没有建立，用同一请求重试”**（维护者 2026-10-28 决定 R-A 的持久性选项 A）：准入拒绝只回复触发它的那次尝试、
不写日志、不建记录（`Status` 没有 Rejected）。拒绝不是持久承诺：所有者在下一次写事件之前崩溃时，被拒的意图（输入水位还没随事件持久）在重放时
按当时的准入重新判定，可能被接受——发起方先收到 `ErrBackpressure`，saga 之后却被建立。同一请求重试得到“已存在”；经 `EmitStart` 的路径本来就
nak 重试。只有“收到拒绝后放弃、之后用同一业务键发**不同**意图”的调用方会得到身份冲突，业务不要这样用。

**判定送达**：所有者处理后经核心 NATS 回复这一次尝试（写事件的判定在 PubAck 之后回复）；`saga.start_verdict_timeout`（2s）内没收到回复时每 200ms
向所有者的**内存**问 `Get(id)`（不回落冷副本：冷副本里旧一生的记录不能当成这次启动的判定）：有记录且启动摘要相同 → 视为已接受；摘要不同 → `ErrIdentityConflict`；没有 → 继续等。启动尝试标识 =
`<Engine 实例随机 128 位>.<实例内序号>`，全局唯一；第一次发布就被判重复 → `ErrStartIdentityReused`（标识生成坏了的显式失败）。

## 定义

```go
definition := saga.Definition{
    Type: "alliance_rally",
    Version: 1,
    Steps: []saga.Step{
        {
            Name: "reserve_troops",
            ForwardTopic: "alliance_rally.reserve_troops",
            CompensateTopic: "alliance_rally.reserve_troops.compensate",
            // Timeout / MaxAttempts / BackoffMin / BackoffMax 可以不写：Engine.Register 按
            // Options.StepBudgets 补齐（`wiring/saga` 的 saga Mod 从配置读，见下）。
        },
    },
}
```

**步骤预算是配置。** 一次操作（同一步骤、同一方向）最多派发 `MaxAttempts` 次尝试，每次等 `Timeout`，相邻两次按
`BackoffMin..BackoffMax` 退避。`Engine.Register` 按 `Options.StepBudgets` 补齐每个字段：按步骤覆盖 > 定义里写的值 >
配置默认值 > 框架默认（5s / 5 次 / 100ms..5s）。`wiring/saga` 的 saga Mod 从 `saga.step_defaults.{timeout,max_attempts,backoff_min,backoff_max}`
与 `saga.steps.<saga type>.<step name>.<字段>` 读取；`saga.steps` 下写了不存在的类型、步骤或字段时 `Init` 失败，不会静默失效。
**步骤 `Timeout` 必须短于步骤消费者的 `AckWait`**（`saga.DefaultStepAckWait` = 30s）：配置检查与 `Init` 拒绝不短于它的配置（点名键或
`saga "<type>" step "<name>"`）。生成工程的测试可以用 `kitsaga.StepBudgetsFromConfig(cfg, definitions...)` 得到和运行时相同的预算。

步骤顺序或补偿语义变化时递增 `Definition.Version`，并在滚动升级期间同时注册仍有存量流程的旧版本；记录固化该版本，不会让旧 saga 误跑新步骤。
只有确认该版本已无非终态记录后才能删除旧定义（`Engine.List(Query{Type, DefinitionVersion, Statuses})`）。运行时缺少指定版本时，协调器把记录
fence 到 `ManualRequired`，恢复定义后 `Resume`。

需要与当前 Nest handler 的实体修改可靠绑定时，在 handler 内调用 `saga.EmitStart`：启动意图与实体修改写入同一条 Nest WAL 记录，DataEngine
发布到效果流后，协调器进程的**启动中继**（`SubscribeNestStarts`，每个装了 saga Mod 的进程都跑）转发到分区。准入关闭或判定未达时中继 nak 退避，
意图是延迟而不是丢失。`EmitStart` 在事务里就按 `saga.max_payload_bytes`（缺省 64 KiB，发起方与协调器共用一份声明）校验 `Data`，超过时
返回 `ErrInvalidRecord`、事务回滚。直接 `StartSaga` 只用于已经在可靠消息消费者、运维任务或不需要与另一笔提交原子绑定的入口。

**业务键与查询**（选项 ID-A、W-A）：saga ID 由 `(Type, BusinessKey)` 派生，业务键查询就是按 ID 查，唯一性与推进由同一个单写者判定。
**唯一性窗口 = 运行期间 + 终态保留期**：同一业务键在 saga 运行时与结束后 `saga.terminal_retention` 内只能有一笔（同一意图返回已有记录，不同意图
`ErrIdentityConflict`），之后同一业务键可以再开一笔。`GetByBusinessKey` 还没启动或过了保留期返回 `ErrNotFound`。生成的
`saga/<name>/definition.go` 带 `Lookup(ctx, engine, businessKey)`。业务键应取发起方能交还给客户端的值（例如请求号）。终态通知没有默认实现。

`Completion.Data` 会成为下一步的 `Command.Payload`，用于传递流程状态。不要放入大对象（事件携带完整状态，Data 大时日志膨胀）；业务实体仍应存放在其权威服务中。

### 原生步骤执行契约（U-0280，维护者 2026-10-05 决定；2026-10-28 决定 2A 改为回执在实体里）

原生步骤 = 业务就是一笔 Nest 事务。业务侧形状是 `saga.NativeStep`：步骤运行时（`wiring/saga` 的 `Steps.Native`）为每次投递构造它，步骤体把它作为
**显式业务参数**传进 Nest handler；生成的 Nest 包装在业务函数之前调用 `step.Begin(第一个锁目标)`（选项 N-A），业务在事务里调用
`step.Complete(success, reason)`。不能把 handler context 或 `NativeStep` 交给异步 goroutine。

```go
// 步骤体（Steps.Native 登记）：
func debit(ctx context.Context, step saga.NativeStep) error {
    _, err := syncsender.NewGiftDebitSender(nest).Sync_GiftDebit(ctx, entityID, itemID, count, step)
    return err
}
// Nest handler（生成的包装已先调 step.Begin(target)）：
return left, step.Complete(true, "")
```

1. **最多生效一次的单位是操作实例**：saga + 步骤 + 方向（`Command.IdempotencyKey`，即 `<saga>:<phase>:<step>`）。一次操作可以有多次尝试
   （`MaxAttempts`），每次尝试有自己的 `CommandID`；同一操作实例的所有尝试里**至多一次**让业务写与回执落盘。
2. **回执在被改的实体里**：`roost add saga -native -entity X` 给 X 的 DAO 加 `SagaReceipts map[string][]byte`（持久、不同步）与
   `game/entities/<x>/saga_receipts.go`（实现 `saga.StepReceiptHost`）。回执的读写经 DAO 生成的 mutator，随事务提交 / 回滚；同一实体的事务由实体锁
   串行，回执与业务修改是同一条 WAL 记录。`Begin`（在事务里、实体锁内）：有**成功**回执（任何代际）或**本代际的拒绝** → 回放，不执行业务；
   否则命令已过截止 → 过期未执行；否则执行。回执编码归框架（带版本），坏回执报 `ErrInvalidRecord`（事务回滚、nak），不当作“没有回执”。
   回放时把同一份回执再写一次，让回放事务本身是一条 WAL 记录：pipelined 下回执可能是另一笔还没落盘的事务写的，这样结论一定晚于它的落盘外发。
3. **生效点是 Nest 事务的 WAL 落盘**（strict：锁内 fsync；pipelined：本事务的 WAL 票据落盘）。结论经 `nest.AfterCommit` 交给这次运行，Nest 请求
   返回之后消费者把结果发到命令所属分区的 `in.res`（MsgID `res.<回执里的 CommandID>`），**PubAck 之后才 ack 命令**；过期未执行直接 ack
   （`saga.step.expired_unexecuted_total`）；回放时命令过期也照样发回执里的成功（过期投递重发成功）。生成器要求这类 handler 的 `durability` 是
   `strict` 或 `pipelined`（async 不满足“落盘后才发”）。截止检查在事务开始时，WAL 落盘可能晚于截止——只能由迟到成功补偿处理（第 5 条）。
4. **步骤体返回错误 = 结果未知**（Nest 提交可能已经发生）：消费者 nak 退避重投，重投时 `Begin` 读到回执就回放。没有租约、操作状态文档或
   投影里的 fence。业务拒绝同样 `Complete(false, reason)`：只提交拒绝回执，协调器随即补偿（补偿被拒则 `ManualRequired`）。回执保留
   `saga.receipt_retention`（缺省 168h），`Complete` 在同一事务里顺手剪掉至多 16 条过期回执。
5. **协调器接收结果的统一规则**（v1.23.1 起，维护者 2026-10-07 选 A）：**成功是操作的结论，只要这个操作开过、还没有带结果关闭，就接收**——
   记录还停在这个操作上就接收为它的结果，记录已离开（放弃关闭）就补偿这一步（下一条），已带结果关闭是重复；**拒绝是本代际这个操作的结论、
   可重试失败是一次尝试的结论**，只在协调器正等着时接收。判定只在 `judgeCompletion` 一处（行为回归见 `framework/saga/completion_test.go`）：
   - completion 的代际从 `CommandID` 解析（第 0 代 `<key>:<attempt>`，第 N 代 `<key>:rN:<attempt>`），与 `Record.Incarnation` 比较。旧一生的
     拒绝 / 失败**不接收**，计 `saga.completion.stale_incarnation_total`；旧一生的成功在记录正停在这个操作上时**接收为该操作的结果**。
   - 同一生里较早一次尝试晚到的可重试失败不接收，计 `saga.completion.stale_attempt_total`（saga 方向 ③）。
   - 退避中送达的成功同样接收（RR-20261006-42）：操作已派发过、在重试退避时送达的成功接收为该操作的结果。
6. **放弃之后迟到的成功**：协调器在重试用尽、saga 截止、人工 `Compensate` 或定义缺失时关闭操作，tombstone 记为“放弃关闭”；只有接收了**成功**
   才关闭的记为“带结果关闭”。放弃关闭之后才到的成功说明那一步已生效、却不在 `CompletedSteps` 里：
   - **正向步骤 s：协调器补偿它**（saga 方向 ④）。同一条事件里记下这份成功的回执、tombstone 改为带结果关闭、记录写 `LateStep = s+1` 与
     `LateData = 这份成功的 Data`；记录在两个补偿之间或已终态（`Failed` / `Compensated`）时立即进入 `Compensating` 补偿第 s 步（**终态会被重开**）；
     某个补偿正在等结果或重试退避时不打断它，它接收结果后先补第 s 步；`ManualRequired` 只记下，运维 `Resume` / `Compensate` 时先补第 s 步。
     补偿第 s 步的命令载荷是 `LateData`，它的结果不进入 `Data` 链。计 `late_after_abandon_total{phase="forward"}`、WARN；终态被重开另计
     `saga.reopened_total`，见「运维观察」。
   - **补偿方向：只告警**：记 ERROR、计 `late_after_abandon_total{phase="compensate"}`，不改记录。补偿方向的放弃都停在 `ManualRequired`，运维核对该步
     业务数据后 `Resume`，新一生的同一补偿回放这次成功而不是再执行。
   - 迟到成功受终态保留期约束：终态被剪掉之后才到的成功，协调器找不到记录，只计 `saga.completion.unknown_saga_total` 并记 ERROR（点名 `saga_id`、
     `command_id`），**不能自动补偿**，要人核对。成功送达的延迟 ≈ 参与方重启 + AckWait，远小于缺省 1h。
   同一个成功会多次送达，**按（操作，代际）只计一次**：正向由回执去重，补偿方向由 tombstone 上已告警的代际；之后的送达计 `Duplicates`。
7. **原生步骤不能修改 Remote 实体**：`Complete` 发现当前事务带 Remote 写批次时返回 `ErrNativeStepRemoteWrite`（事务回滚）；生成器另在生成期拒绝
   `remote=managed` 与 `noPersist=true` 的回执宿主实体。Remote 实体的修改应放在 saga 之外、以 `IdempotencyKey` 幂等的 Remote 事务里。
8. **分工**：步骤模板不需要自己按 `IdempotencyKey` 做业务幂等——前提是业务写在生效点的同一事务里（原生：Nest 事务；Mongo 步骤：handler 拿到的
   Mongo 事务 ctx）。不在事务里的副作用（调用另一个服务，如 gift deliver 发邮件）不受这个保证，仍要按 `IdempotencyKey` 幂等（mail 的 `RequestID`）。

转交路径（另一个进程的 `WithAdmit` 判定命令不归它后经业务自己的通道送来）用 `Steps.RunHandedOver`：核对步骤名与主题、截止与归属后经同一个
`RunNativeStep` 运行步骤体，**不发布、不 ack**（原消息持有者的重投读到回执后发结果）；与消费者的互斥由实体锁与回执保证。

#### Mongo 步骤（选项 M-A）

`MongoStepInbox.Handle` 一个 Mongo 事务（`WithTransaction`，驱动重试）：读这个操作的回执文档（`_id = IdempotencyKey`）——成功（任何代际）或本代际
拒绝 → 回放，不执行；本次尝试自己的可重试失败 → 回放；过了截止 → 过期未执行；否则执行 handler（事务 ctx），再写回执（条件 upsert：文档不存在，
或存在的是更旧代际的拒绝 / 别的尝试的可重试失败），写前再查一次截止；条件不满足或已过截止 → 中止事务（业务写回滚），重跑时回放或过期。
同一操作的两次尝试并发写同一个回执文档，Mongo 写冲突让后者整体重跑、读到前者的回执回放。

- 集合是 saga 库（`saga.database`）的 `_<group>_step_inbox`（`wiring/saga` 的 `Steps`），TTL 索引 `expires_at = at + saga.receipt_retention`。
- 可重试失败也写回执（只对写它的那次尝试回放，不挡同一操作的下一次尝试）：协调器对同一 CommandID 只接受一种结论，不留回执时同一尝试的重投
  可能交出另一个结论。
- 业务写必须经 handler 拿到的事务 ctx 写进这笔事务，才在契约内；事务外副作用（发邮件）要按 `IdempotencyKey` 幂等：被回滚的尝试可能已经发出调用。
- `StepConsumerConfig.MongoWorkers` 控制并发（缺省 1 串行）；启用后业务 handler 必须允许不同命令并发。

## 准入

- **信号**：有原生步骤的进程（`Steps.Start`）每 `saga.admission.signal_interval`（200ms）把本进程 DataEngine 投影器的 WAL 未投影记录数写进
  `<STREAM>_LOAD` 的 `part.<group>.<sid>`；`Steps.Drain` 时删除自己的键（优雅下线不算停更）。桶的 MaxAge 与 LimitMarkerTTL 都是 30s：崩溃进程的
  键过期时协调器看得到删除，不再计入。Mongo 步骤没有投影积压，不发布。
- **判定**：每个协调器进程一份，全部分区共用；积压 = 全部参与方未投影记录数的最大值；`≥ high_records`（5000）关闭，关闭后 `≤ low_records`（2500）
  才重开。某个参与方的信号按本机时钟超过 `stale_after`（2s）没更新 → 关闭（`backpressure_stale_signal`）。**初始化 fail-closed**：watch 的初始快照
  读完之前关闭（`backpressure_not_ready`）。
- **只作用于新启动**：所有者处理 `in.start` 时判一次（权威），关闭 → 判定 rejected（见 API 一节 `ErrBackpressure`）。在途推进、重试、补偿、Resume 不受影响。
  所有者把状态写进同桶 `admit` 键，`StartSaga` 与启动中继先看 3s 内的提示，关闭时不写意图直接返回 `ErrBackpressure`。
- **硬上限**：`dataengine.projection.max_unacked_records`。本进程有 DataEngine 且准入开启时，它必须 > 0 且 ≥ 4 × `high_records`（生成配置写 20000），
  配置检查强制。到上限时 DataEngine 在 WAL 准入前拒绝，Nest 回滚、命令 nak，协调器按步骤预算重试。
- **调参**：高水位 = 投影器持续速率 × 允许的追平时间；阈值随部署变。

## 失败语义

- `Success=true`：进入下一步；
- `Success=false, Retryable=true`：指数退避后重试；
- `Success=false, Retryable=false`：立即开始补偿；
- 补偿持续失败：进入 `ManualRequired`（常驻内存，不受终态保留期剪枝）；
- `Resume(ResumeRequest)`：故障修复后继续失败或补偿流程，总是开新一生（`Incarnation+1`）；原 deadline 已过期时必须显式提供新的未来 deadline，
  或设置 `ClearDeadline`；
- 放弃之后才到的正向成功：自动补偿那一步，`Failed` / `Compensated` 会被重开回 `Compensating`（契约第 6 条）；
- `Compensate`：仅在没有 in-flight step 时允许人工发起补偿。**`Completed` 的 saga 不能再 `Compensate`**，返回 `ErrSagaCompleted`、记录不动
  （维护者 2026-10-07 选 A）。`Compensated` 幂等返回原记录。在补偿方向停下的 `ManualRequired` 上调用时与 `Resume` 一样进入新一生；**补偿方向
  `ManualRequired` 修复原因后的正确做法是 `Resume`**；
- 回执保留期守卫（选项 RR-A）：记录最后一次更新早于 `now − saga.receipt_retention + 最长步骤窗口` 时 `Resume` 与补偿方向的 `Compensate` 返回
  `ErrReceiptsExpired`：重新执行的操作可能已经没有参与方回执、会再生效一次。超过一周的人工 Resume 需要运维先核对业务数据再走业务冲正。

运维面通过 `Engine.List` 按 `ManualRequired`/`Failed` 查询（单次最多 1000 条），再 `Get` 查看错误与步骤，修复外部原因后 `Resume`。

等待不占用 goroutine：每个分区一个哈希时间轮（`saga.tick` 5ms、1024 槽），条目只是“到时看一眼”，状态机按记录当前的 `NextRunAt` / `DeadlineAt`
判定，陈旧条目不改状态；墙钟回拨时扫当前槽。同一轮先处理输入、后处理到期，已在输入里的结果先于同轮的超时被接收。

**故障与恢复**：

| 故障 | 行为 |
| --- | --- |
| 协调器 kill -9 | 已 PubAck 的事件在流里，内存丢失；同 sid 进程重启（单实例锁等旧锁过期）→ 快照 + 尾巴重放 → 服务。分区在重启前停顿 |
| 旧协调器还活着（SIGSTOP 恢复、重叠） | 下一次发布被期望序号拒绝 → `ErrPartitionFenced` → fail-stop |
| 参与方 kill -9 | strict 已确认的提交在 WAL；重启先排空 WAL 进 Mongo 再加载（回执随实体）；未 ack 的命令重投 → `Begin` 回放 |
| 参与方长时间停机 | 协调器每个步骤超时派一次新尝试；回来后每条积压命令经 Nest 跑一次 Begin（回放或过期 ack），实测 300 笔停 60s 约 1～3s 排空 |
| 发布结果未知 | 同 MsgID 重发判定；超过预算 fail-stop |
| 快照写冲突 / 快照不兼容 | fail-stop / 分区不启动 |
| 清理守卫卡住 | 不清理、告警；流满了拒新写 → fail-stop |

**进程被强杀时遗留的 Mongo 事务**：被 kill -9 的参与方留下的 DataEngine 投影事务（与 Mongo 步骤事务）在服务端保持打开、持锁，直到
`transactionLifetimeLimitSeconds`（服务端参数，缺省 60s）才回收；期间重启后的排空 WAL 对同一文档的写一直 `WriteConflict`，这段时间该参与方
不服务，在途步骤消耗重试预算。**部署要求：服务端 `transactionLifetimeLimitSeconds=20`**（`db.adminCommand({setParameter: 1,
transactionLifetimeLimitSeconds: 20})` 或启动参数 `--setParameter transactionLifetimeLimitSeconds=20`）。框架自己的事务由 `mongo.transaction_timeout`
约束，单次远短于 20s；业务里若有单次超过 20s 的 Mongo 事务，按它调大，同时调大受影响步骤的预算。服务端每半个参数扫一次过期事务，所以遗留
事务最晚在 1.5 倍参数时回收（20s → 最晚 30s；2026-10-29 私有 mongod 实测 27～29s）。

- 参与方启动时只读探测这个参数（`Steps.Start`，`getParameter`）：长于缺省步骤预算窗口（`saga.step_defaults` 补齐框架缺省后
  MaxAttempts × Timeout 加退避，框架缺省 26.5s）时 WARN 一次并计 `saga.step.mongo_txn_lifetime_exceeds_budget`；托管库不让读时只记 INFO
  “未探测”。探针从不拒绝启动（参数归部署）。
- DataEngine 启动排空撞上写冲突时在 `dataengine.startup_timeout`（**缺省 60s**，维护者 2026-10-29“按推荐”由 30s 调大）内重试：驱动在一次
  事务里重跑到 `mongo.transaction_timeout`，之后 Runtime 退避再排空（`dataengine.startup_drain_retry_total`），期限到了才启动失败（进程退出，
  由部署重启）。参数 20s 时最坏回收时刻 30s，60s 期限在此之上留 30s 余量（原缺省 30s 没有余量）；生成的部署启动等待（install.sh 的
  `HEALTH_ATTEMPTS`、compose 的 `start_period`）按它计算。调大参数或期限时同步调大部署侧的启动等待。直接装配、没有给期限的调用方上限 2 分钟。

## 运维观察

协调器的计数都在 `Engine.Stats()`（saga Mod 的健康检查消息里列出部分），指标标签只有 saga 类型与方向 / 原状态 / 原因，保持低基数；完整清单见
[OBSERVABILITY](OBSERVABILITY.md#sagacoresaga)。

| 指标 | `Stats()` | 含义 | 是否故障 |
| --- | --- | --- | --- |
| `saga.reopened_total{saga_type,from_status,reason}` | `Reopened` | saga 被**重开**（下面一节） | 不是故障，是记录被改过的信号 |
| `saga.completion.late_after_abandon_total{saga_type,phase}` | `LateAfterAbandon` | 放弃之后才到的成功，按（操作，代际）计一次 | `phase="forward"` 已自动补偿（WARN）；`phase="compensate"` 要运维核对（ERROR） |
| `saga.completion.unknown_saga_total` | `UnknownSaga` | 结果对应的 saga 已过终态保留期被剪掉 | **非零要人核对** |
| `saga.completion.stale_attempt_total{saga_type,phase}` | `StaleAttempt` | 同一生较早尝试晚到的可重试失败被忽略 | 正常现象 |
| `saga.completion.stale_incarnation_total{saga_type,phase}` | `StaleIncarnation` | Resume 之后旧一生的拒绝 / 失败被忽略 | 正常现象 |
| `saga.partition.fail_stop_total{reason}` | `FailStops` | 分区 fail-stop（`fenced` / `publish_unknown` / `rejected` / `error`） | **非零即查**，进程随之停下 |

`Completed` / `Compensated` / `Failed` / `ManualRequired` 按**到达终态的次数**计：重开的 saga 补偿完再次到达终态时会再计一次。

### saga 重开（saga 方向 ④ 的可观测性）

**什么时候发生**：正向步骤 s 在协调器放弃它之后才送达成功（契约第 6 条）。协调器把 saga 带回补偿、只补偿这一步，结束后回到 `Compensated`
（补偿失败停在 `ManualRequired`）。每次重开计一次 `saga.reopened_total`、写一条 WARN：

| `reason` | `from_status` | 场景 |
| --- | --- | --- |
| `late_success` | `failed` / `compensated` | 已结束的 saga 收到迟到的正向成功，立即重开为 `Compensating` 补偿第 s 步 |
| `resume` / `compensate` | `manual_required` | `ManualRequired` 期间收到的迟到成功只记下（`LateStep`，不自动跑）；运维 `Resume` / `Compensate` 时先补第 s 步 |

迟到成功到达时 saga 还在补偿中（非终态）不算重开，只计 `late_after_abandon_total{phase="forward"}`；没有迟到步骤的普通 `Resume` 也不算。
日志：重开 WARN `saga: reopened to compensate a step that took effect after the coordinator abandoned it`（`saga_id`、`saga_type`、`from_status`、
`reason`、`late_step`、`status`、`version`），同时还有 WARN `saga: step succeeded after the coordinator abandoned it; compensating it`（`command_id`、`operation`）。

**业务怎样识别重开**：saga 没有向业务推送终态的回调（`WaitTerminal` 只在进程内等一次），按终态做业务的一方用 `Get` / `List` 读记录，所以
`Failed` / `Compensated` 不是永远不变的：

1. 读到终态时记下 `Record.Version`。之后 `Get` 返回更大的 `Version` 就是记录被改过（重开或运维 `Resume`），按新状态重新处理；
2. 重开期间记录是 `Status=Compensating`、`LateStep>0`（正在补的是 `Step == LateStep-1`）；`ManualRequired` 且 `LateStep>0` 表示有一步等运维 `Resume` 时补偿；
3. 重开不改变业务结论：`Failed` 被重开后最终是 `Compensated`，`Compensated` 被重开后仍回到 `Compensated`。按终态做的业务动作按 saga id 幂等即可。

## 跨服交易：使用约束

维护者确认 Saga 的目标用途是跨服交易。它适合跨多个独立事务域的扣款、资源预留、交付和失败补偿；一次请求的接纳与整笔交易完成是两个时刻。客户端
收到已接纳的交易号后显示“处理中”，通过业务状态查询 / 结果通知确认结果；不能把启动成功、请求超时或客户端断线直接解释为交易成功或失败。
Nest handler 不应持有 Entity 锁等待整笔 Saga 完成。

容量：协调器原型（本机三节点 R3、参与方投影 8 通道、准入开）实测长期约 1200 笔/秒（云 Mongo 约 700 笔/秒），见
[原型报告](../performance/SAGA-JETSTREAM-LOG-PROTO-20261027.md)。正式实现（生成 DAO、实体内回执、`Steps` 运行时）的数字在性能验收批次另测，
**原型数字不是正式实现的容量结论**。Redis 权威与旧原生路径的历史测量（`SAGA-CHECKPOINT-20261018`、`NIGHT-20261020` 等）不再适用。

- **持久的交易身份**：同一笔交易使用稳定 BusinessKey 与一致的启动意图；`ErrBackpressure` / `ErrStartUnconfirmed` / 超时后重查或用同一请求重试，
  不换新键再扣一次款。
- **预留与可补偿性**：先预留余额 / 物品，再确认消耗或释放。框架不提供跨服瞬时原子可见，也不自动推导退款公式；补偿要符合业务账本与并发变更规则。
- **正确访问权威数据**：本服已加载玩家 Entity 走原生 Nest 步骤；独立 Mongo 事务域才使用 Mongo 步骤，事务内写入必须使用传入 ctx。
- **独立交易的资源冲突**：框架去重同一操作，不自动隔离不同交易对同一余额的业务竞争；预留和余额校验必须在权威事务内完成。热点玩家 / 公共库存必须单独验收。
- **有限恢复预算**：步骤 Timeout、MaxAttempts、JetStream MaxDeliver 都有上限。`ManualRequired` 必须告警、可查询并进入人工核对。
- **准入与观察**：分别监控接纳率（`AdmissionState`、`saga.start.rejected_total`）、正向成功率、补偿 / 失败 / 人工状态、端到端 P99、最老未完成交易、
  分区 fail-stop 与清理阻塞。
- **上线验收**：在目标跨机拓扑、真实交易步骤、热点分布下验证正常与补偿负载、长尾、积压恢复和故障切换（协调器 / 参与方 kill -9、NATS leader 切换）。

## 部署与升级

- NATS ≥ 2.12.1、JetStream 文件存储、saga 流 R3（`saga.replicas: 3`）；Mongo 副本集（Mongo 步骤与 DataEngine 投影的事务）且
  `transactionLifetimeLimitSeconds=20`；协调器进程 `singleton.enabled=true`。
- 步骤消费者是每条 saga 流上的同名 durable（`<group>-<step>`），参与方必须与协调器用同一份 `saga.subject_prefix` / `stream` / `partitions` /
  `stream_groups`（`kitsaga.ParticipantSettings`）。下线一个步骤时删掉它在每条流上的 durable，否则清理被它挡住；关掉冷副本时同样删掉
  `<stream>_G<g>-cold-copy`。
- 冷副本（`saga.cold_copy.enabled`）要求协调器进程有 Mongo Mod、Mongo 可写；投影跑在协调器进程里（选项 C-A），Mongo 与协调器
  同盘时有争用（原型第二轮），冷副本的 Mongo 不要与 NATS 数据盘共用。
- **升级需清空**（格式与存储不兼容，线上未部署）：旧版本 saga 数据（Redis 权威、Mongo 冷副本 `_sagas` / `_saga_completions` / `_saga_operations`、
  `_dataengine_step_operations`、Mongo 步骤的 `_operations` 与按 CommandID 的回执）不迁移。先在旧版本让全部 saga 到终态（`List` 非终态为 0），
  停旧协调器与步骤进程、排空参与方 WAL，删除旧 saga 流 / Redis 键前缀 / 上述集合，再起新版本（新流、新桶）。WireVersion 1 的信封一律拒绝。
