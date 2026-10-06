# Roost Saga

Saga 用于跨越多个独立事务域的业务流程。单个 Nest handler、同一 MongoDB
事务或 `RemoteWriteBatch` 能覆盖的操作不应使用 Saga。

## 一致性模型

每个步骤由本地事务提交，跨步骤提供最终一致性。失败时按相反顺序执行业务补偿，
补偿不是内存状态回滚。推荐把资源状态设计为 `available -> reserved -> consumed`
或 `reserved -> released`，并以 `Command.IdempotencyKey` 作为业务唯一键。

协调器持久化以下边界：

- Saga 状态与待发布 command 在一个存储事务中提交；
- command 通过 durable outbox 至少一次发布；
- 同一 attempt 的消息重投共享 `CommandID`，跨 attempt 共享稳定 `IdempotencyKey`（操作实例，见「原生步骤执行契约」）；
- `version + lease token` 阻止过期 worker 推进状态；
- 按 `CommandID` 保存的 completion receipt 与状态推进在一个存储事务中提交；
- operation 关闭时写入有 TTL 的 tombstone，并在同一事务清理尚未发布的旧 attempt；
- 新 attempt 会在同一事务替换该 operation 仍排队的旧 outbox，避免重试扇出；
- 重复、迟到的 command/result 不会重复推进状态；协调器放弃一个**正向**步骤之后才到达的成功说明它已生效，协调器把 saga 带回补偿、
  只补偿这一步（`Failed` / `Compensated` 会被重开，saga 方向 ④）；补偿方向的只告警（计数 `saga.completion.late_after_abandon_total`、ERROR 日志，U-0280）。

Nest start、command 和 completion 使用严格的 `WireVersion=1` envelope；未知版本直接
拒绝并进入受退避约束的重新投递，不通过猜测字段做隐式兼容。协议变更必须显式升级版本。

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
            // Options.StepBudgets 补齐（kit 的 saga Mod 从配置读，见下）。
        },
    },
}
```

**步骤预算是配置。** 一次操作（同一步骤、同一方向）最多派发 `MaxAttempts` 次尝试，每次等 `Timeout`，相邻两次按
`BackoffMin..BackoffMax` 退避。`Engine.Register` 按 `Options.StepBudgets` 补齐每个字段：按步骤覆盖 > 定义里写的值 >
配置默认值 > 框架默认（5s / 5 次 / 100ms..5s）。kit 的 saga Mod 从 `saga.step_defaults.{timeout,max_attempts,backoff_min,backoff_max}`
与 `saga.steps.<saga type>.<step name>.<字段>` 读取；`saga.steps` 下写了不存在的类型、步骤或字段时 `Init` 失败，不会静默失效。
`roost add saga` 生成的定义不再写预算。生成工程的测试可以用 `kitsaga.StepBudgetsFromConfig(cfg, definitions...)` 与
`StepBudgets.Resolve(definition)` 得到和运行时相同的预算（game-demo 的 `gift_saga_budget_test.go`）。

步骤顺序或补偿语义变化时递增 `Definition.Version`，并在滚动升级期间同时注册仍有
存量流程的旧版本；Record/Command 固化该版本，不会让旧 Saga 误跑新步骤。只有确认
该版本已无非终态记录后才能删除旧定义。
可用 `Engine.List(Query{Type: ..., DefinitionVersion: ..., Statuses: ...})` 检查存量版本。
运行时缺少指定版本时，协调器会把记录 fence 到 `ManualRequired`，不会猜测使用最新版；
恢复定义后再执行 `Resume`。

需要与当前 Nest handler 的 Entity 修改可靠绑定时，不要直接调用 `StartSaga`，而应
在 handler 内调用 `saga.EmitStart`。启动意图会与 Entity mutation 写入同一个 Nest
WAL record，再由 kit 的 durable consumer 幂等创建 Saga。直接 `StartSaga` 只用于
本身已经处于可靠消息消费者、运维任务或不需要与另一笔提交原子绑定的入口。
相同 type/business key 只有在 ID、payload、deadline 表示同一意图时才返回已有记录，
否则返回 `ErrIdentityConflict`。deadline 会规范到毫秒精度，以适配 MongoDB datetime。

Native Entity step 使用 Data Engine inbox。Kit consumer 在同步调用 handler 前分配
`Reservation`；业务从当前调用 context 读取一次，并把它作为**显式业务参数**传进 Nest
handler，不能把 handler context 交给异步 goroutine：

```go
reservation, ok := sagaKit.ReservationFromContext(ctx)
if !ok {
    return saga.Completion{}, errors.New("missing saga reservation")
}
// 将 command 与 reservation 作为 Nest Params 发送。
// Nest handler 内：
if err := inbox.Bind(command, reservation); err != nil {
    return nil, err
}
// 业务修改产生 Put/Patch，最后 saga.EmitCompletion(completion)。
```

handler 的返回值不使用：原生步骤的结果是它在事务里 `EmitCompletion` 的那一份（生成模板返回零值 `Completion`）。

Entity mutation、`saga-step/CommandID` receipt、lease fence control receipt，以及 ID 为
`saga-completion:{CommandID}` 的 completion effect 会形成同一个 CommitRecord。Mongo
投影在同一事务里对这个操作的状态文档（`_dataengine_step_operations`，`_id = IdempotencyKey`，见下文「操作状态文档」）
做一次条件写（当前尝试的 owner、lease token、digest、`pending`、`lease_until > now` 都匹配才写 `updated_at`）；
不匹配就只写入幂等的 skipped transaction marker，不应用业务 mutation/effect，并让 WAL 安全 ACK。条件写让投影与另一
worker 的过期接管（换当前尝试、lease token 加一）写同一文档，二者只能有一个提交：租约到期附近不会出现
“投影落库、接管也成功”的双执行（RR-20260926-30 §6）。控制 receipt 不进入业务 receipt
collection。

被跳过的记录已经改过 Nest 内存（扣减、状态推进都在准入前完成），所以框架还保证内存与后续
WAL 不以它为基础（RR-20260926-30）：

- **实体屏障**：原生步骤记录准入 WAL 后、投影结果确定前，写到同一实体的其他事务在 WAL
  准入处被拒绝，错误可 `errors.Is(err, dataengine.ErrFencedEntityPending)`（同时满足
  `nest.ErrCommitRejected`），Nest 在 Guard 内整体回滚，没有写 WAL。这是**可重试**错误：
  正常只持续一次投影（毫秒级），Mongo 变慢或中断时与投影积压同量级。业务入口应按可重试
  失败回复客户端或稍后重投，不要当成业务拒绝。同一命令的重投（新 token）同样被挡到屏障
  解除；`SubscribeDataEngineStep` 在 handler 以该错误失败时交还本次刚拿到的租约，屏障解除
  后的重投能立刻重新 Reserve，而不是等租约自然过期。
- **跳过后驱逐**：记录被跳过时，受影响的常驻实体在 Nest 快池内持锁驱逐（不持久化），屏障
  保持到驱逐完成。之后的访问从 Mongo 重载；已登记的 Sync subject 在重载后被重新绑定，
  订阅者收到整份全量，而不是在旧版本链上续发增量（驱逐到重载之间该对象不再同步）。
  `Projector.Stats()` 的 `FencedEntities`、`FencedAdmissionRejected`、`StaleEvictions`
  以及 `dataengine.fence.*` 指标可观测这两步。
- **重启**：屏障保证被跳过记录之后没有同实体依赖记录；启动恢复在实体可加载前排空 WAL，
  跳过后实体从 Mongo 读取，不会 fence。

副作用仍然只发生一次：被跳过的记录不落库、不发 completion；步骤由同一操作实例的下一次尝试执行（见下面的契约）。
客户端可能短暂看到被跳过的扣减（它在准入时已经同步出去），重载后的全量会纠正。

**原生步骤不能修改 Remote 实体。** 原生步骤的 CommitRecord 必然带 lease fence control
receipt；同一 Nest 事务里若还有 Remote 实体的修改，DataEngine 在写 WAL 前返回
`engine.ErrRemoteLeaseFenceUnsupported`，Nest 在 Guard 内整体回滚（RR-20260926-19）。原因：
lease fence 在投影时失效会跳过整笔记录，而 Remote 写在准入时已经改了内存、占住写权限与
分布式锁；生成 Entity 的 `RollbackRemoteCommit` 不保存跨实体前像，投影阶段无法撤销，旧实现
下这笔 Remote 事务会一直得不到结论。这个拒绝不是业务 Completion：handler 返回错误后消息
重投，同一 CommandID 仍是当前尝试、租约有效，重投得到 `Duplicate` 并等待 completion 直到步骤
deadline，之后按 `Timeout` / `MaxAttempts` 重试、最终补偿，而不是立即得到业务拒绝。框架目前
没有让原生步骤与 Remote 写原子提交的入口：Remote 实体的修改应放在 Saga 之外、以
`IdempotencyKey` 幂等的 Remote 事务里，原生步骤只修改本地 Entity。

handler 返回后不得直接 publish NATS；Mongo 投影负责原子保存 mutation、receipt 和
outbox，独立 publisher 再投递 effect。重复 CommandID 由操作状态文档上的短租约协调，最终以
receipt 为权威；同 ID 不同 digest 返回 `ErrIdentityConflict`。不同 CommandID、相同 IdempotencyKey 是同一操作实例的
不同尝试，由下面的契约保证最多一次生效，业务 step 不需要再按 IdempotencyKey 自己做幂等。
Reserve 读到 receipt 时顺手把状态文档里这次尝试结算为 settled；这一步失败不改变 Reserve 的结论（receipt 仍是权威，
写错误会让 Mongo 中止事务并由驱动重跑），但每次失败记 Warn（带 `command_id` 与原因）并累加
`saga.step_inbox.mark_completed_error_total`。该计数持续增长、同一步骤反复 Duplicate 直到 deadline 时，
检查操作状态文档集合的写入（权限、索引、文档校验）（RR-20260927-16）。

raw Mongo step 继续使用 `MongoCommandInbox`，其 handler 运行在 Mongo transaction 中；
不要在这类 handler 中混用 Nest Entity 修改。它与原生步骤共用同一套操作实例契约（下文「Mongo 步骤」，saga 方向 ②）。两种 inbox 分开是为了保持各自的原子边界，
不是让 Saga coordinator 改走 Entity WAL。Coordinator 的 state/outbox/completion receipt
仍由 Saga Store 的 Mongo transaction 负责。

`Completion.Data` 会成为下一步的 `Command.Payload`，用于传递流程状态。不要放入
大对象；业务实体仍应存放在其权威服务中。

### 原生步骤执行契约（U-0280，维护者 2026-10-05 决定）

本节的契约自 2026-10-06 起同样适用于 Mongo 步骤（`MongoCommandInbox`，维护者第六轮决定 saga 方向 ②），差别只在生效点，见本节末「Mongo 步骤」。

1. **最多生效一次的单位是操作实例**：saga + 步骤 + 方向（`Command.IdempotencyKey`，即 `<saga>:<phase>:<step>`）。
   一次操作可以有多次尝试（`MaxAttempts`，配置），每次尝试有自己的 `CommandID`；同一操作实例的所有尝试里**至多一次**
   让业务写、回执与 completion 落库。跨 Resume 的代际也算同一操作实例：新一生遇到旧一生已生效的成功会回放它。
2. **每次尝试的生效窗口包含在协调器等它的窗口内**：租约 `lease_until = min(now + LeaseDuration, Command.DeadlineAt)`。
   协调器只在 `DeadlineAt` 之后才判这次尝试超时、放弃或发出下一次尝试；之后才投影的记录（kill -9 重启后的 WAL 重放、投影积压）
   lease fence 不再匹配，被跳过（受影响实体按 RR-20260926-30 驱逐重载），不留回执、不发 completion。
3. **新尝试先看同一操作实例的其他尝试**（`DataEngineStepInbox.Reserve`，在一个 Mongo 事务里只读这个操作的状态文档，
   同一操作实例的并发 Reserve 在这份文档上写冲突串行化，见「操作状态文档」）：
   - 已有**成功**回执（任何一生）或**本生的拒绝**（`Success=false, Retryable=false`）→ 不执行，把那次的 completion 经 saga 结果流
     重发给协调器（它的 completion effect 可能在退避期间被丢弃）；
   - 只有**可重试失败** → 那次尝试已有结论且没有生效，新尝试照常执行；
   - 仍 pending 且租约有效 → 不执行，返回可重试错误（nak 后重投），等它有结论；
   - pending 且租约已过期 → **接替**（新尝试成为当前尝试、`lease_token+1`，被接替的记进 `superseded`），它在 WAL 里未投影的记录随后被 fence 跳过；
     被接替尝试的迟到投递直接 ack、不执行。
   - 过了自己截止的投递（U-0281 的过期 ack、Reserve 的 `ErrCommandExpired`）不执行，但 ack 前同样把同一操作实例已生效的
     **成功**经 saga 结果流重发：它可能是最后一次尝试，较早尝试的成功又在退避期间被丢弃，不重发就没人再送达（审查 2026-10-05）。
   - **协调器用同一张表接收结果**（B1，维护者决定 2026-10-05）：completion 的代际从 `CommandID` 解析（第 0 代 `<key>:<attempt>`，
     第 N 代 `<key>:rN:<attempt>`，与铸造 ID 的 `commandID` 同一处），与记录当前代际（`Record.Incarnation`）比较。
     旧一生的拒绝 / 失败**不接收**，只计 `Stats().StaleIncarnation` 与 `saga.completion.stale_incarnation_total{saga_type,phase}`
     （新一生照常执行，收件箱也不回放它）；旧一生的成功在记录正停在这个操作上（在等它，或 Resume 之后还没派发、新一生的尝试在退避）时
     **接收为该操作的结果**，不再派发这一步；同一生的结果按原规则接收。不在这个操作上的旧一生成功按第 4 条处理。
   - **可重试失败只接收正在等的那次尝试的**（saga 方向 ③，2026-10-07）：同一生里较早一次尝试晚到的可重试失败只说明那一次没生效，
     正在等的尝试照常执行；协调器不接收它，计 `Stats().StaleAttempt` 与 `saga.completion.stale_attempt_total{saga_type,phase}`（WARN）。
     成功与本生的拒绝是操作的结论，从哪次尝试来都接收——下一次尝试回放的正是较早那次的 completion（`CommandID` 是那次的）。
4. **放弃之后迟到的成功**：协调器在重试用尽、saga 截止、人工 `Compensate` 或定义缺失时关闭操作，tombstone 记为“放弃关闭”；
   只有接收了**成功**才关闭的记为“带结果关闭”，以失败关闭（可重试失败用尽、拒绝）同样记为放弃关闭。放弃关闭之后才到的成功说明那一步已生效、
   却不在 `CompletedSteps` 里（saga 方向 ③ 之后来源只剩“截止前已生效、completion 在放弃后才送达”与时钟偏差）：
   - **正向步骤 s：协调器补偿它**（saga 方向 ④，即 U-0280 的方向 C，2026-10-07）。同一个 Store 事务里记下这份成功的回执、tombstone 改为带结果关闭、
     记录写 `LateStep = s+1` 与 `LateData = Data`；记录在两个补偿之间或已终态（`Failed` / `Compensated`）时立即进入 `Compensating` 补偿第 s 步
     （**终态会被重开**，结束后回到 `Compensated`）；某个补偿正在等结果或重试退避时不打断它，它接收结果后先补第 s 步；`ManualRequired` 只记下，
     运维 `Resume` / `Compensate` 时先补第 s 步。补偿第 s 步的载荷是 `LateData`，它的结果不进入 `Data` 链；之后回到 `CompletedSteps` 的倒序。
     第 s 步在“下一个边界”补偿，可能排在一个已在途的补偿之后（在途的补偿不能放弃，它可能生效）。计 `late_after_abandon_total{phase="forward"}`、WARN。
   - **补偿方向：只告警**：记 ERROR、`Stats().LateAfterAbandon` 与 `saga.completion.late_after_abandon_total{phase="compensate"}`，不改记录。
     补偿方向的放弃都停在 `ManualRequired`，运维按 TROUBLESHOOTING T-226 核对后 `Resume`，新一生的同一补偿回放这次成功而不是再执行。
   同一个成功会多次送达（effect 重投、过期投递的回放、JetStream 重投），**按（操作，代际）只计一次**：正向由回执去重，补偿方向由 tombstone 上的
   `late_alarms.r<代际>`（`LateSuccessAlarmStore`，B1）；之后的送达计 `Duplicates`。成功若在 Resume 之后、新一生派发之前才到达，协调器直接把它接收为
   这一步的结果（第 3 条），不是迟到。
5. **分工**：框架兑现跨尝试幂等（收件箱 + 租约封顶 + 协调器告警），步骤模板不再需要自己按 `IdempotencyKey` 做业务幂等——
   前提是业务写在生效点的同一事务里（原生：Nest 事务；Mongo 步骤：handler 拿到的 Mongo 事务 ctx）。不在事务里的副作用
   （调用另一个服务，如 gift deliver 发邮件）不受这个保证，仍要按 `IdempotencyKey` 幂等（mail 的 `RequestID`）。
   2026-10-06 之前 Mongo 步骤不在本契约内（只保证同一命令最多一次），见下。

#### Mongo 步骤（2026-10-06 纳入）

`MongoCommandInbox` 与 `DataEngineStepInbox` 共用操作状态文档与判定代码（`saga/step_operation_inbox.go`），第 1～4 条逐条相同：
新尝试先看同一操作实例的其他尝试（成功或本生的拒绝 → 回放，不执行；在途且租约有效 → nak 等待；租约过期 → 接替），租约封顶到命令截止。
差别只在**生效点**：

- **提交点是 handler 的 Mongo 事务**。`Handle` 先在一个 Reserve 事务里拿租约，再开执行事务：handler 的业务写 →
  对状态文档的条件写（当前尝试是自己、owner、token、`pending`、`lease_until > now`，结算为 settled 并存 completion）→ 插入回执（`_id = CommandID`，格式不变）。
  条件写不匹配（截止已过、已被接替）时整笔事务中止，业务写随之回滚；接替写的是同一份状态文档，两者只能有一个提交。
  条件在事务最后一次写时检查，剩下的窗口是提交本身的延迟（与原生投影相同）。
- **业务写必须经 handler 拿到的事务 ctx 写进这笔事务**，才在契约内；这时业务按 `IdempotencyKey` 幂等从“必需”降为可选的纵深防御。
  调用另一个服务的步骤不在事务里：被中止的尝试可能已经发出调用，仍要业务幂等。
- 执行事务失败（handler 错误、取消、fence）后交还租约，重投立即重试，与改动前“失败后重投立即重跑”一致；提交结果未知时回执已在，重投回放。
- 消费者：回放的 completion 原样发布（`CommandID` 是生效那次的）；截止已过、被 fence、被接替的投递不执行，ack 前把同一操作实例已生效的成功
  经 saga 结果流重发；在途等待按可重试错误 nak。
- `CommandInboxOptions` 增 `Owner`（缺省每个实例随机）与 `LeaseDuration`（缺省 1 分钟，再封顶到命令截止）。
- 状态文档集合是 `<收件箱集合>_operations`（缺省 `_saga_step_inbox_operations`），回执集合不变。已生成工程不迁移（维护者决定），仓库模板与
  `roost add saga` 生成物已同步注释。
- 代价：每次执行多一个 Reserve 事务（读回执、读状态文档、写状态文档），执行事务多一次条件更新（测量见
  [方案](docs/feature/SAGA-DIRECTION-STEP-TRANSITION-AND-MONGO-INBOX-2026-10-06.md)、[状态文档方案](docs/feature/SAGA-OPERATION-STATE-DOC-2026-10-06.md)）。

**两个结果消费者的终态分类相同**（O-S5-1）：普通结果流（`SubscribeCompletions`）与原生 effect 流对 `ErrNotWaiting`、`ErrNotFound`、`ErrIdentityConflict`、
`ErrInvalidRecord` 都 Term，不再 nak 到 `MaxDeliver`。退避中到达、被丢弃的成功由下一次尝试回放或由过期投递重发（上面第 3 条）。
`ErrDefinitionMissing` 不是终态（发版前审查更正）：Complete 只在记录正等着这个操作时才查定义，这时缺定义是滚动发布中“派发它的进程已升级、
处理结果的协调器还没升级”的暂时状态，两条流都按可重试错误 nak 退避；新定义上线后重投被接收。定义一直不来时，步骤超时后没有定义的协调器把记录
fence 到 `ManualRequired`（放弃关闭，第 4 条），之后的重投按迟到成功 ack 并告警，`MaxDeliver` 兜底。

**代价与运维要点**：
- **投影积压超过步骤 `Timeout` 时步骤停住而不是重复执行**：每次尝试都在截止后才投影、被跳过，步骤要等积压消退后的那次尝试才能成功；
  积压持续到重试用尽则补偿或 Failed。现象是 `dataengine.fence.skipped.total{resource="_dataengine_step_operations"}` 与
  `saga.step_inbox.superseded_total` 增长、步骤超时。调大 `Timeout` 或解决 Mongo 变慢，不要调大 `LeaseDuration`（它已被截止时间封顶）。
- `LeaseDuration` 现在只是上限，实际租约不超过命令截止；`LeaseDuration > AckWait` 的校验保留。
- 依赖协调器、步骤进程与投影进程的时钟偏差远小于 `Timeout`。
- 每次授予租约（新建、接管、接替）只读写这个操作的一份状态文档，不按操作查询、没有上限：尝试次数、Resume 次数与历史都不进入判定
  （取代了 RR-20261006-15 / -16 的 claim 扫描、`outcome` 字段与 4096 / 8192 上限）。
- **持久格式**：tombstone（`_saga_operations`）有 `closure` 字段，B1 再多 `late_alarms` 子文档（`r<代际>: 首次告警时间`）；没有 `closure` 的 tombstone
  不告警，没有 `late_alarms` 的 tombstone 第一次迟到成功照常告警并补上标记。saga 记录（`_sagas`）在有待补偿的迟到步骤时多 `late_step` / `late_data`
  （saga 方向 ④，正常记录没有）；自定义 Store 要在 `Create` / `Get` / `Apply` 中原样保存 `Record.LateStep` / `LateData`。收件箱的持久格式见下面「操作状态文档」。
- **混跑**：B1 之前的协调器按 `IdempotencyKey` 接收任一代际的结果、每次送达都告警、人工 `Compensate` 不换代——它处理的 completion 与运维操作不受 B1 约束，
  记录本身仍按版本号 fence，新旧协调器不会互相覆盖。步骤进程不支持新旧混跑（见「操作状态文档」的升级步骤）。
  已生成工程不提供迁移（维护者决定）；仓库内模板与生成物已同步。

#### 操作状态文档（2026-10-06，维护者决定“直接改成一份状态文档”）

两种收件箱对每个操作实例（`Command.IdempotencyKey`）只写一份状态文档，判定只读它（[方案](docs/feature/SAGA-OPERATION-STATE-DOC-2026-10-06.md)）：

- **集合**：原生 `_dataengine_step_operations`；Mongo 步骤 `<收件箱集合>_operations`（缺省 `_saga_step_inbox_operations`）。`_id = IdempotencyKey`，
  索引只有 `ttl_expires_at`（`expires_at` 每次写入刷新为 `now + receiptTTL`，缺省 30 天）。`EnsureInfrastructure` / 订阅时自动建立。
- **内容**：当前尝试（`command_id`、`incarnation`、`digest`、`owner`、`lease_token`、`lease_until`、`deadline_at`、`status` = `pending` / `settled`、
  结算后的 `result` 与 `completion`）——原生投影的 lease fence 直接匹配这几个顶层字段；`refusals.r<代际>`（只留代际最大的两生，剪掉的记
  `refusals_dropped_through`，那一生迟到的投递不执行）；`superseded`（最近 16 个被接替的尝试，它们迟到的投递不执行）；`version`（Reserve 的 CAS）。
- **Reserve 一个事务三条命令**：读本命令回执、读状态文档、需要时以 `version` 做条件写；同一操作的并发 Reserve 在这份文档上写冲突串行化。
  `lease_token` 每次授予租约加一，旧尝试的 fence 与结算条件从此不再匹配。
- **升级（不兼容 v1.22.0 及以前）**：存储形状改变，新代码不读也不写旧的 claims 集合（`_dataengine_inbox_claims`、`<收件箱集合>_claims`），
  不支持与旧步骤进程混跑（维护者 2026-10-06 决定，线上未部署）。步骤服务先停旧再起新：停掉全部旧步骤进程（原生步骤进程要排空 WAL：
  旧记录的 fence 指向旧 claim 文档），清空或丢弃旧 claims 集合（`db.<name>.drop()`），再起新进程。回执集合、协调器记录、tombstone、wire 不变，
  协调器的升级顺序不受影响。
- **运维观察**：`saga.step_inbox.superseded_total`（接替）、`saga.step_inbox.mark_completed_error_total`（读到回执后结算失败）、
  `dataengine.fence.skipped.total{resource="_dataengine_step_operations"}`（原生记录因租约失效被跳过）。看某个操作卡在哪里，直接
  `db._dataengine_step_operations.findOne({_id: "<saga>:<phase>:<step>"})`：`status=pending` 且 `lease_until` 在未来就是有尝试在途。

## 失败语义

- `Success=true`：进入下一步；
- `Success=false, Retryable=true`：指数退避后重试；
- `Success=false, Retryable=false`：立即开始补偿；
- 补偿持续失败：进入 `ManualRequired`；
- `Resume(ResumeRequest)`：故障修复后继续失败或补偿流程；原 deadline 已过期时必须
  显式提供新的未来 deadline，或设置 `ClearDeadline`；
- 放弃之后才到的正向成功：自动补偿那一步，`Failed` / `Compensated` 会被重开回 `Compensating`（上文契约第 4 条，saga 方向 ④）；
- `Compensate`：仅在没有 in-flight step 时允许人工发起补偿（中止正向、开始补偿）。在补偿方向停下的 `ManualRequired`
  上调用时与 `Resume` 一样进入新一生（`Incarnation+1`）：要重新执行的补偿步骤在这一生里已经派发过，不换代就会复用上一轮
  的 `CommandID`，收件箱只会回放旧的拒绝或报身份冲突（B1）。**补偿方向 `ManualRequired` 修复原因后的正确做法是 `Resume`**
  （它同时处理截止时间）；`Compensate` 在这种状态下与之等价。

运维面通过 `Engine.List` 按 `ManualRequired`/`Failed` 和更新时间分页查询，再使用
`Get` 查看错误与步骤，修复外部原因后调用 `Resume`。单次查询最多返回 1000 条，
避免管理请求退化成无界全表扫描。

等待不占用 goroutine，也不依赖持久化进程 timer。`NextRunAt` 由带索引的批量
worker 扫描；进程内 signal 只用于降低新任务延迟。

## 性能原则

- `ClaimDue` 与 `ClaimOutbox` 必须按 `NextRunAt` 使用索引并限制 batch；
- 不允许全表扫描或全局 Saga 锁；
- 网络发布不能发生在数据库事务中；
- payload 应保持紧凑，建议小于 64 KiB；
- 领取批量按协调器/发布器分开配置；Engine 会拒绝 batch、store/publish timeout
  可能超过 lease 的组合，避免尚未处理的批内任务提前失租；
- worker 数量、batch、Mongo pool 和 JetStream ack 参数必须通过压测确定；
- 监控 `StoreFailures`、`WorkerFailures`、冲突、发布失败、手工处理量以及各步骤延迟。

原生步骤的有效租约是 `min(LeaseDuration, 命令截止)`，应覆盖投影的高分位延迟：租约只在 Reserve 时设置、不随投影续期，
投影积压超过它时记录会被跳过（上面的屏障与驱逐保证不会 fence，该步骤由下一次尝试执行，
期间同一实体的写入会被可重试地拒绝）。监控 `StaleEvictions` 的增长可以发现步骤 `Timeout` 相对投影延迟偏短。

**屏障期间的重投会消耗 JetStream `MaxDeliver`（RR-20260926-63）。** 实体屏障（`ErrFencedEntityPending`）让 step 消费者
返回可重试错误（租约内的 `Duplicate` 则等待 completion，等不到时同样返回错误），消息被 Nak 并按 `NakBackoffMin` 起倍增、
封顶 `NakBackoffMax` 的退避重投，每次都计入该 consumer 的 `MaxDeliver`。屏障时长与投影积压同量级：Mongo 中断或变慢时它可能远长于一次投影。投递次数达到
`MaxDeliver` 后适配层对消息 `Term`（计数 `nats.jetstream.terminal.total{reason="max_deliver"}`），JetStream 不再投递，
这条命令不会因屏障解除而自动执行——只能等 coordinator 的步骤 `Timeout` / `MaxAttempts` 发起新 attempt（新 CommandID，同一操作实例）重试，
或进入补偿，恢复时间受步骤 deadline 约束。运维要点：

- 按预期最长的 Mongo 故障 / 投影积压时长核算 `MaxDeliver` 与 `NakBackoffMax`：默认 25 000 次、250ms～30s，约 8 天才会耗尽；
  调小 `MaxDeliver` 或退避上限时，要确认 `MaxDeliver × NakBackoffMax` 仍覆盖这段时间；
- 步骤 `Timeout` / `MaxAttempts` 必须能在投递耗尽后兜底，不要把 `MaxDeliver` 当作业务重试上限；
- 监控 `nats.jetstream.terminal.total{reason="max_deliver"}`、consumer 的重投计数与 `Projector.Stats().FencedAdmissionRejected`：
  屏障拒绝持续增长同时出现 `max_deliver` 终止，说明投影积压已超过投递预算。

**过期命令直接确认（U-0281）。** 命令过了 `DeadlineAt`，协调器已按超时自行重试或补偿；两种步骤消费者都不再开始业务。
有回执时让结果送达（Mongo 路径重发 completion，原生路径的 completion 随投影 effect 送达），没有回执时直接 ack 并累加
`saga.step.expired_unexecuted_total`，不 nak、不占 `MaxAckPending`。原生步骤的结果经 WAL → 投影 → completion effect 送达，
不依赖这条消息，所以 ack 不会丢掉已提交、还没投影的尝试。读回执出错时仍按退避重投。

**进程被强杀时遗留的 Mongo 事务。** Mongo 步骤的 handler、协调器的状态推进都在 Mongo 事务里；进程被 kill -9 时服务端不会立刻知道，
这笔事务保持打开、持锁，直到 `transactionLifetimeLimitSeconds`（服务端参数，默认 60s）才被回收。期间其他进程对同一文档的事务写一直得到
`WriteConflict`，同一操作的尝试反复失败（真实 NATS + Mongo 两进程强杀实测恢复约 1～1.5 分钟，N06 S5 review）。默认步骤预算（5s × 5 次加退避，约 30s）
短于它，受影响的操作可能用尽重试进入补偿。要让强杀后的步骤自己恢复，二选一（O-S5-3，saga 方向 ②方案）：

- **推荐：把服务端 `transactionLifetimeLimitSeconds` 调到 20**（`db.adminCommand({setParameter: 1, transactionLifetimeLimitSeconds: 20})`，或启动参数
  `--setParameter transactionLifetimeLimitSeconds=20`）。框架自己的事务由 `mongo.transaction_timeout`（默认 30s）约束整个重试过程，单次事务远短于 20s；
  被服务端按寿命中止的事务以 TransientTransactionError 重跑。业务里若有单次超过 20s 的 Mongo 事务，按它调大，同时调大受影响步骤的预算。
- 或按步骤调大 Mongo 步骤的预算（`saga.steps.<type>.<step>.max_attempts`），让 `Timeout × MaxAttempts` 加退避长于该参数。默认步骤预算不改：它约束所有 saga 的失败检测时间。

saga 方向 ②之后被杀进程手里的尝试**不会在锁释放后补提交**（它的事务已随进程消失，服务端回收时中止），接替它的尝试照常执行一次；锁只影响恢复时间。

生产集群应使用 MongoDB replica set（事务所需）和 JetStream file storage；关键区服
通常配置 3 replicas。`AckWait` 必须大于步骤处理的高分位延迟，receipt/tombstone TTL
必须长于 stream 最大保留时间。上线门禁需要在目标 Mongo/NATS 拓扑上验证持续吞吐、
P99、积压恢复以及 coordinator/step worker/Mongo primary/NATS leader 故障切换；本地
内存 benchmark 不能代替该容量结论。
