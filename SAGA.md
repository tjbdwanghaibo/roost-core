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
投影在同一事务里验证 claim 的 owner、lease token、`pending` 状态和 `lease_until`；任一
不匹配都只写入幂等的 skipped transaction marker，不应用业务 mutation/effect，并让 WAL
安全 ACK。这样新 owner 接管后，旧 worker 的晚到记录不能产生副作用，也不会成为永久
poison WAL。控制 receipt 不进入业务 receipt collection。

handler 返回后不得直接 publish NATS；Mongo 投影负责原子保存 mutation、receipt 和
outbox，独立 publisher 再投递 effect。重复 CommandID 由短租约 claim 协调，最终以
receipt 为权威；同 ID 不同 digest 返回 `ErrIdentityConflict`，不同 CommandID 即使共享
IdempotencyKey 仍表示新的 Saga attempt，业务 step 继续按 IdempotencyKey 保证语义幂等。

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

生产集群应使用 MongoDB replica set（事务所需）和 JetStream file storage；关键区服
通常配置 3 replicas。`AckWait` 必须大于步骤处理的高分位延迟，receipt/tombstone TTL
必须长于 stream 最大保留时间。上线门禁需要在目标 Mongo/NATS 拓扑上验证持续吞吐、
P99、积压恢复以及 coordinator/step worker/Mongo primary/NATS leader 故障切换；本地
内存 benchmark 不能代替该容量结论。
