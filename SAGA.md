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
- 重复、迟到的 command/result 不会重复推进状态；协调器放弃一个步骤之后才到达的成功只告警（计数
  `saga.completion.late_after_abandon_total`、ERROR 日志），不重开终态（U-0280）。

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
投影在同一事务里对 claim 做一次条件写（owner、lease token、digest、`pending`、
`lease_until > now` 都匹配才写 `updated_at`）；不匹配就只写入幂等的 skipped transaction
marker，不应用业务 mutation/effect，并让 WAL 安全 ACK。条件写让投影与另一 worker 的
过期接管（`$inc lease_token`）写同一文档，二者只能有一个提交：租约到期附近不会出现
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
重投，同一 CommandID 的 claim 仍在租约内，重投得到 `Duplicate` 并等待 completion 直到步骤
deadline，之后按 `Timeout` / `MaxAttempts` 重试、最终补偿，而不是立即得到业务拒绝。框架目前
没有让原生步骤与 Remote 写原子提交的入口：Remote 实体的修改应放在 Saga 之外、以
`IdempotencyKey` 幂等的 Remote 事务里，原生步骤只修改本地 Entity。

handler 返回后不得直接 publish NATS；Mongo 投影负责原子保存 mutation、receipt 和
outbox，独立 publisher 再投递 effect。重复 CommandID 由短租约 claim 协调，最终以
receipt 为权威；同 ID 不同 digest 返回 `ErrIdentityConflict`。不同 CommandID、相同 IdempotencyKey 是同一操作实例的
不同尝试，由下面的契约保证最多一次生效，业务 step 不需要再按 IdempotencyKey 自己做幂等。
Reserve 读到 receipt 时顺手把 claim 标成 completed；这一步失败不改变 Reserve 的结论（receipt 仍是权威，
写错误会让 Mongo 中止事务并由驱动重跑），但每次失败记 Warn（带 `command_id` 与原因）并累加
`saga.step_inbox.mark_completed_error_total`。该计数持续增长、同一步骤反复 Duplicate 直到 deadline 时，
检查 claim 集合的写入（权限、索引、文档校验）（RR-20260927-16）。

raw Mongo step 继续使用 `MongoCommandInbox`，其 handler 运行在 Mongo transaction 中；
不要在这类 handler 中混用 Nest Entity 修改。两种 inbox 分开是为了保持各自的原子边界，
不是让 Saga coordinator 改走 Entity WAL。Coordinator 的 state/outbox/completion receipt
仍由 Saga Store 的 Mongo transaction 负责。

`Completion.Data` 会成为下一步的 `Command.Payload`，用于传递流程状态。不要放入
大对象；业务实体仍应存放在其权威服务中。

### 原生步骤执行契约（U-0280，维护者 2026-10-05 决定）

1. **最多生效一次的单位是操作实例**：saga + 步骤 + 方向（`Command.IdempotencyKey`，即 `<saga>:<phase>:<step>`）。
   一次操作可以有多次尝试（`MaxAttempts`，配置），每次尝试有自己的 `CommandID`；同一操作实例的所有尝试里**至多一次**
   让业务写、回执与 completion 落库。跨 Resume 的代际也算同一操作实例：新一生遇到旧一生已生效的成功会回放它。
2. **每次尝试的生效窗口包含在协调器等它的窗口内**：claim 租约 `lease_until = min(now + LeaseDuration, Command.DeadlineAt)`。
   协调器只在 `DeadlineAt` 之后才判这次尝试超时、放弃或发出下一次尝试；之后才投影的记录（kill -9 重启后的 WAL 重放、投影积压）
   lease fence 不再匹配，被跳过（受影响实体按 RR-20260926-30 驱逐重载），不留回执、不发 completion。
3. **新尝试先看同一操作实例的其他尝试**（`DataEngineStepInbox.Reserve`，在一个 Mongo 事务里，同一操作实例的并发 Reserve 由
   守卫文档 `saga-step-op/<IdempotencyKey>` 串行化）：
   - 已有**成功**回执（任何一生）或**本生的拒绝**（`Success=false, Retryable=false`）→ 不执行，把那次的 completion 经 saga 结果流
     重发给协调器（它的 completion effect 可能在退避期间被丢弃）；
   - 只有**可重试失败** → 那次尝试已有结论且没有生效，新尝试照常执行；
   - 仍 pending 且租约有效 → 不执行，返回可重试错误（nak 后重投），等它有结论；
   - pending 且租约已过期 → **接替**（`status=superseded`、`lease_token+1`），它在 WAL 里未投影的记录随后被 fence 跳过；
     被接替尝试的迟到投递直接 ack、不执行。
   - 过了自己截止的投递（U-0281 的过期 ack、Reserve 的 `ErrCommandExpired`）不执行，但 ack 前同样把同一操作实例已生效的
     **成功**经 saga 结果流重发：它可能是最后一次尝试，较早尝试的成功又在退避期间被丢弃，不重发就没人再送达（审查 2026-10-05）。
   - **协调器用同一张表接收结果**（B1，维护者决定 2026-10-05）：completion 的代际从 `CommandID` 解析（第 0 代 `<key>:<attempt>`，
     第 N 代 `<key>:rN:<attempt>`，与铸造 ID 的 `commandID` 同一处），与记录当前代际（`Record.Incarnation`）比较。
     旧一生的拒绝 / 失败**不接收**，只计 `Stats().StaleIncarnation` 与 `saga.completion.stale_incarnation_total{saga_type,phase}`
     （新一生照常执行，收件箱也不回放它）；旧一生的成功在记录正停在这个操作上（在等它，或 Resume 之后还没派发、新一生的尝试在退避）时
     **接收为该操作的结果**，不再派发这一步；同一生的结果按原规则接收。不在这个操作上的旧一生成功按第 4 条处理。
4. **放弃之后迟到的成功只告警**：协调器在重试用尽、saga 截止、人工 `Compensate` 或定义缺失时关闭操作，tombstone 记为“放弃关闭”；
   只有接收了**成功**才关闭的记为“带结果关闭”，以失败关闭（可重试失败用尽、拒绝）同样记为放弃关闭——协调器在等最后一次尝试时
   可能接收较早尝试晚到的可重试失败而用尽重试，正在执行的最后一次尝试仍会生效（审查 2026-10-05）。放弃关闭之后才到的成功说明那一步已生效、却不在 `CompletedSteps` 里、不会被补偿：
   记 ERROR、`Stats().LateAfterAbandon` 与 `saga.completion.late_after_abandon_total{saga_type,phase}`，**不重开终态、不自动补偿**。
   同一个成功会多次送达（effect 重投、过期投递的回放、JetStream 重投），告警**按（操作，代际）只记一次**：tombstone 上记
   `late_alarms.r<代际>`（`LateSuccessAlarmStore`，B1），之后的送达计 `Duplicates`。
   运维按 TROUBLESHOOTING T-226 核对：saga 停在 Failed（这一步前面没有已完成步骤）时可以 `Resume`，新一生的同一步骤
   （原生收件箱）回放这次成功、继续往后走，而不是再执行；成功若在 Resume 之后、新一生派发之前才到达，协调器直接把它接收为
   这一步的结果，不告警（第 3 条）。已进入补偿或 Compensated 的，补偿不含这一步，要按业务手工撤销它。
   B 之后这只剩“截止前已投影、completion 在放弃后才送达”（effect 发布延迟）、“较早尝试晚到的失败让协调器在最后一次尝试执行中放弃”
   与时钟偏差三种来源。
5. **分工**：框架兑现跨尝试幂等（收件箱 + 租约封顶 + 协调器告警），原生步骤模板不再需要自己按 `IdempotencyKey` 做业务幂等。
   Mongo 步骤（`MongoCommandInbox`）不在本契约内：它仍是“同一命令最多一次”，跨尝试按 `IdempotencyKey` 做业务幂等
   （如 mail 的 `RequestID`），因为它的提交点在 handler 的 Mongo 事务里、与命令截止时间没有绑定。

**代价与运维要点**：
- **投影积压超过步骤 `Timeout` 时步骤停住而不是重复执行**：每次尝试都在截止后才投影、被跳过，步骤要等积压消退后的那次尝试才能成功；
  积压持续到重试用尽则补偿或 Failed。现象是 `dataengine.fence.skipped.total{resource="_dataengine_inbox_claims"}` 与
  `saga.step_inbox.superseded_total` 增长、步骤超时。调大 `Timeout` 或解决 Mongo 变慢，不要调大 `LeaseDuration`（它已被截止时间封顶）。
- `LeaseDuration` 现在只是上限，实际租约不超过命令截止；`LeaseDuration > AckWait` 的校验保留。
- 依赖协调器、步骤进程与投影进程的时钟偏差远小于 `Timeout`。
- 每次新建 / 接管 claim 多一次守卫 upsert 与一次按 `operation_key` 的索引查询（新索引 `by_operation`）。
- **持久格式增量**：claim 多 `operation_key`、`incarnation`、`superseded_by` 字段与 `superseded` 状态，claims 集合多守卫文档（`namespace=saga-step-op`）；
  tombstone（`_saga_operations`）多 `closure` 字段，B1 再多 `late_alarms` 子文档（`r<代际>: 首次告警时间`）。旧数据缺字段：旧 claim 不参与跨尝试判断，
  没有 `closure` 的 tombstone 不告警，没有 `late_alarms` 的 tombstone 第一次迟到成功照常告警并补上标记。
- **混跑**：契约只在所有步骤进程与协调器都升级后成立。旧进程写的 claim 没有 `operation_key`、租约不封顶；旧协调器不写 `closure`；
  B1 之前的协调器按 `IdempotencyKey` 接收任一代际的结果、每次送达都告警、人工 `Compensate` 不换代——它处理的 completion 与运维操作不受 B1 约束，
  记录本身仍按版本号 fence，新旧协调器不会互相覆盖。
  已生成工程不提供迁移（维护者决定）；仓库内模板与生成物已同步。

## 失败语义

- `Success=true`：进入下一步；
- `Success=false, Retryable=true`：指数退避后重试；
- `Success=false, Retryable=false`：立即开始补偿；
- 补偿持续失败：进入 `ManualRequired`；
- `Resume(ResumeRequest)`：故障修复后继续失败或补偿流程；原 deadline 已过期时必须
  显式提供新的未来 deadline，或设置 `ClearDeadline`；
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

生产集群应使用 MongoDB replica set（事务所需）和 JetStream file storage；关键区服
通常配置 3 replicas。`AckWait` 必须大于步骤处理的高分位延迟，receipt/tombstone TTL
必须长于 stream 最大保留时间。上线门禁需要在目标 Mongo/NATS 拓扑上验证持续吞吐、
P99、积压恢复以及 coordinator/step worker/Mongo primary/NATS leader 故障切换；本地
内存 benchmark 不能代替该容量结论。
