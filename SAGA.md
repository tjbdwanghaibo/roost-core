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
- 同一 attempt 的消息重投共享 `CommandID`，跨 attempt 共享稳定 `IdempotencyKey`；
- `version + lease token` 阻止过期 worker 推进状态；
- 按 `CommandID` 保存的 completion receipt 与状态推进在一个存储事务中提交；
- operation 关闭时写入有 TTL 的 tombstone，并在同一事务清理尚未发布的旧 attempt；
- 新 attempt 会在同一事务替换该 operation 仍排队的旧 outbox，避免重试扇出；
- 重复、迟到的 command/result 不会重复推进状态。

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
            Timeout: 5 * time.Second,
            MaxAttempts: 5,
            BackoffMin: 100 * time.Millisecond,
            BackoffMax: 5 * time.Second,
        },
    },
}
```

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

副作用仍然只发生一次：被跳过的记录不落库、不发 completion；步骤按 Timeout/重投再次执行。
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
receipt 为权威；同 ID 不同 digest 返回 `ErrIdentityConflict`，不同 CommandID 即使共享
IdempotencyKey 仍表示新的 Saga attempt，业务 step 继续按 IdempotencyKey 保证语义幂等。
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

## 失败语义

- `Success=true`：进入下一步；
- `Success=false, Retryable=true`：指数退避后重试；
- `Success=false, Retryable=false`：立即开始补偿；
- 补偿持续失败：进入 `ManualRequired`；
- `Resume(ResumeRequest)`：故障修复后继续失败或补偿流程；原 deadline 已过期时必须
  显式提供新的未来 deadline，或设置 `ClearDeadline`；
- `Compensate`：仅在没有 in-flight step 时允许人工发起补偿。

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

原生步骤的 `LeaseDuration` 仍应覆盖投影的高分位延迟：租约只在 Reserve 时设置、不随投影续期，
投影积压超过租约期时记录会被跳过（上面的屏障与驱逐保证不会 fence，但该步骤要重投再执行一次，
期间同一实体的写入会被可重试地拒绝）。监控 `StaleEvictions` 的增长可以发现租约偏短。

**屏障期间的重投会消耗 JetStream `MaxDeliver`（RR-20260926-63）。** 实体屏障（`ErrFencedEntityPending`）让 step 消费者
返回可重试错误（租约内的 `Duplicate` 则等待 completion，等不到时同样返回错误），消息被 Nak 并按 `NakBackoffMin` 起倍增、
封顶 `NakBackoffMax` 的退避重投，每次都计入该 consumer 的 `MaxDeliver`。屏障时长与投影积压同量级：Mongo 中断或变慢时它可能远长于一次投影。投递次数达到
`MaxDeliver` 后适配层对消息 `Term`（计数 `nats.jetstream.terminal.total{reason="max_deliver"}`），JetStream 不再投递，
这条命令不会因屏障解除而自动执行——只能等 coordinator 的步骤 `Timeout` / `MaxAttempts` 发起新 attempt（新 CommandID）重试，
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

**已知缺陷（U-0280，未修复）。** 收件箱只按 `CommandID` 去重，命令截止时间只有一次尝试的 `Timeout`，而 claim 租约是
`LeaseDuration`：已写进 WAL 的尝试 k 若在截止之后、租约之内才投影（进程崩溃后重放、投影积压超过 `Timeout`），协调器可能已经
发出 k+1 并让它再执行一次，或已放弃该步骤而不补偿它。原生步骤目前只保证“同一命令最多一次”；需要步骤级至多一次的业务按
`IdempotencyKey` 自行幂等。方案与取舍见 [U-0280](docs/bugfix/U-0280-saga-step-reexecuted-after-crash.md)。

生产集群应使用 MongoDB replica set（事务所需）和 JetStream file storage；关键区服
通常配置 3 replicas。`AckWait` 必须大于步骤处理的高分位延迟，receipt/tombstone TTL
必须长于 stream 最大保留时间。上线门禁需要在目标 Mongo/NATS 拓扑上验证持续吞吐、
P99、积压恢复以及 coordinator/step worker/Mongo primary/NATS leader 故障切换；本地
内存 benchmark 不能代替该容量结论。
