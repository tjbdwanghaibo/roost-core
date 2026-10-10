# Remote Entity 生产协议

Remote Entity 只保留一条写链路：Nest 声明目标 → 全局顺序获取 write gate/ownership read lock/分布式 lock → Entity mutex 内冻结 commit → WAL 或同步 Mongo transaction → snapshot/outbox 发布 → ACK dirty → 释放 fence。

## 一致性向量

每次提交同时校验四个互不混用的维度：`StateVersion`、`MarkerEpoch`、`LockFence`、`RouteEpoch`。StateVersion 必须严格 `base+1`；ownership/route 代际只能前进；shared write 必须带非零 LockFence。TransactionID 是幂等键。

## 存储事务

`framework/remoteentity` 的 `MongoCommitter`（接线入口是 `wiring/remoteentity` 的 `WithMongoStorage`）在同一 Mongo transaction 中完成：

1. entity meta 的 StateVersion/fence CAS；
2. 全部 DAO full document mutation 或 delete target；
3. 权威 snapshot；
4. transaction receipt 与待发布 outbox。

多实体写要求 `IRemoteAtomicBatchCommitter`，不支持时 fail-fast，绝不退化成逐实体提交。删除 commit 必须带完整 `RemoteDataDelete` 集合，codegen 自动生成。

框架只接受完整的 `IRemoteEntityBackend`，并通过 `IRemoteEntityManager.SetBackend` 一次注入，缺少 load、原子 batch commit、snapshot loader 或 outbox 任一能力都会在编译期暴露。组合方式是 `remoteentity.NewBackend(loader, remoteentity.NewMongoCommitter(...))`：业务只实现 `LoadRemoteEntity`，Wiring中由 `WithMongoStorage(loader)` 装配（没有 `NewMongoBackend`）。

## WAL 与 outbox

Nest WAL当前codec7 持久化完整 RemoteCommit（mutation、delete、snapshot、invalidation）。Async handler 在 WAL admission 后返回，write gate 由 finalizer 持有，直到 backend status 确认。坏事务采用单次检查后退避重排队，不独占 worker。

Mongo transaction首先标记`Applied`；唯一的`RecoverOutbox`协调者负责快照/复制发布，成功后标记`Committed`。投影只持久提交并唤醒outbox，finalizer只确认和释放，不自行发布；`ApplyRemoteCommits`成功只代表持久回执，需要发布确认的调用者等待`FlushRemoteTransaction`。memory/strict/pipelined的回复与写额度释放语义保持不变，不能提前到Applied。

协调者共用启动恢复、显式恢复、重试和停止屏障，页内按完整事务有界并行；涉及任意同一Entity的事务按扫描顺序尝试，失败仍保留Applied。正式配置`remote_entity.outbox_publish_workers`默认8（0取8），范围1～64；设1用于串行诊断，不随Nest慢worker数放大。带游标的正式Backend可越过失败页继续扫描，不删除失败记录。页间先排空；停止必须等已派发发布结束。

未发布记录不设置TTL，只有完成发布的记录才进入过期回收。发布与ACK保持幂等。实体侧的`AcknowledgeRemoteCommit`必须并发安全：outbox和Committed回执的本地确认可能交错，生成实体经`Tracker.AdvanceVersion`原子CAS满足。阶段耗时见[指标](OBSERVABILITY.md)，功能验收不能当作新TPS结论。

## 读取

业务读取 `RemoteSnapshotEnvelope`，不读取远端 live Entity。L1/L2 以 tenant/entity/kind/scope/policy 为完整 key；payload 是 `FrozenRemoteSnapshotPayload`，L1 命中不复制 backing bytes，解码时显式取 copy。相同 epoch/version 的不同 checksum 被拒绝。

- Cached：允许当前缓存版本。
- Monotonic：要求不低于调用方 minVersion；缓存不满足时不等待推送，按 `(key, After)` 合并回源一次（Mirror 第 2 步起）。
- Linearizable：直接权威存储读取。

Interest 使用 TTL 软状态，本机在半 TTL 前不重复发布 renewal，避免每次 L1 read 产生网络消息。

## 启动约束

RemoteEntityMod 缺少以下任一能力必须启动失败：Redis versioned lock（竞争协调；所有权的权威是 Mongo 的 ownership / grant / fence，正式装配不用 Redis marker，见[Remote实现篇](impl/05-remote-mirror.md)）、跨服 SyncBus、`IRemoteAtomicBatchCommitter`、`IRemoteSnapshotLoader`、`IRemoteCommitOutbox`。依赖在 Start 后封存，禁止运行期替换造成 data race。

Broadcast 和匿名 callback 不允许写 remote-managed entity，因为它们不具备一个明确的原子 transaction/rollback 边界。跨实体写使用声明目标的 Multi handler。
