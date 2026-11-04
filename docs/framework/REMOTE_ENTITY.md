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

Remote 实体 handler 里 `nest.Emit` / `saga.EmitStart`：Emit 把 memory 升到 strict，effect 与 Remote 写进同一条 WAL 提交记录，投影时一起进 Mongo（DataEngine 投影的同一个事务），outbox 在投影之后各发布一次；handler 失败时两者都不提交。2026-11-04 起有真实依赖端到端（`codegen/internal/entity/testdata/remoteflow/emit_test.go` 的 `TestGeneratedRemoteHandlerEmitsEffectsWithTheRemoteWrite`，经 `scripts/test-remote-generated.sh`，strict 与声明 memory 两种 handler）：投影被拦住时 Mongo 与 outbox 都看不到这笔，记录里正好两个 effect；放行后版本 +1、两个 effect 各发布一次；失败的 handler 不留 Remote 写也不发 effect。原生 saga 步骤仍不能写 Remote（[SAGA.md 第 7 条](SAGA.md#原生步骤执行契约u-0280维护者-2026-10-05-决定2026-10-28-决定-2a-改为回执在实体里)），这里覆盖的是“Remote handler 启动 saga / 发 effect”。

多实体写要求 `IRemoteAtomicBatchCommitter`，不支持时 fail-fast，绝不退化成逐实体提交。删除 commit 必须带完整 `RemoteDataDelete` 集合，codegen 自动生成。

框架只接受完整的 `IRemoteEntityBackend`，并通过 `IRemoteEntityManager.SetBackend` 一次注入，缺少 load、原子 batch commit、snapshot loader 或 outbox 任一能力都会在编译期暴露。组合方式是 `remoteentity.NewBackend(loader, remoteentity.NewMongoCommitter(...))`：业务只实现 `LoadRemoteEntity`，Wiring中由 `WithMongoStorage(loader)` 装配（没有 `NewMongoBackend`）。DAO 库名映射表只给 DataEngine 一次（`engine.MongoStoreConfig.Databases`：wiring 由 DataEngine Mod 冻结后传入，自建装配用 `dataengine.NewDatabaseMap` 合成），`engine.Assemble` / `MongoStore.SetRemoteProjection` 接 Remote 投影时自动把同一张表绑给 `Backend` / `MongoCommitter`（二者实现 `dataengine.DatabaseMapBinder`）；Remote 存储已绑另一张表时接线报错、不覆盖。没有表时两边都拒绝读写带库名的 DAO 文档（`dataengine.ErrDatabaseMapNotBound`，维护者 2026-10-21）。Remote 要在接上投影之前写 DAO 文档时（例如先启动、恢复上一轮未完成的提交），先调 `Backend.BindDatabaseMap` 绑同一张。

## WAL 与 outbox

Nest WAL当前codec7 持久化完整 RemoteCommit（mutation、delete、snapshot、invalidation）。Async handler 在 WAL admission 后返回，write gate 由 finalizer 持有，直到 backend status 确认。坏事务采用单次检查后退避重排队，不独占 worker。

Mongo transaction首先标记`Applied`；唯一的`RecoverOutbox`协调者负责快照/复制发布，成功后标记`Committed`。投影只持久提交并唤醒outbox，finalizer只确认和释放，不自行发布；`ApplyRemoteCommits`成功只代表持久回执，需要发布确认的调用者等待`FlushRemoteTransaction`。memory/strict/pipelined的回复与写额度释放语义保持不变，不能提前到Applied。

### 持久拒绝（Rejected）的来源与收尾

事务的 Rejected 只有 Durability 0（memory）一种生产来源：`Commit` 直接写权威时被明确拒绝（写前校验失败、fenced、版本冲突），在业务池同步回滚、隔离，`Close` 释放后仅内存卸载；结果未知时由 finalizer 回源，读不到结论就用 `RejectUnresolvedRemoteCommits` 以同一事务 `_id` 写下持久拒绝（撞键则按已有记录的真实状态收尾），拿到 Rejected 后回滚、隔离、释放 gate、仅内存卸载，再把 false 交给提交后工作。WAL 准入前的 `Abort` 只在本进程 tracker 记 Rejected，不写持久记录。

Durability 1/2/3（async / strict / 随 strict 路径提交的 pipelined）的 WAL 投影不产生 Rejected：投影器写权威遇到 fence / 版本冲突是 `ErrProjectionConflict` fatal（进程级 fail-stop，记录不 ack）。strict / pipelined 的 `Commit` 失败因此一律带 `entity.ErrRemotePersistenceIndeterminate`（回复判别表第 2 行），框架自带 remoteentity 只在 memory 下回复 `nest.ErrRemotePartRejected`；finalizer 对 WAL 事务读不到结论时只隔离重试，不写拒绝。维护者 2026-10-14 据此删除了只供测试注入 WAL 级拒绝的 `MongoCommitter` / `Backend.RejectRemoteCommitsInTransaction` 与 `Manager.RejectRemoteTransaction`；守卫用例 `TestWALRemoteAuthorityConflictNeverProducesRejected`、`TestRemoteWALCommitFailureNeverRepliesRemotePartRejected`。

协调者共用启动恢复、显式恢复、重试和停止屏障，页内按完整事务有界并行；涉及任意同一Entity的事务按扫描顺序尝试，失败仍保留Applied。正式配置`remote_entity.outbox_publish_workers`默认8（0取8），范围1～64；设1用于串行诊断，不随Nest io 池 worker数放大。带游标的正式Backend可越过失败页继续扫描，不删除失败记录。页间先排空；停止必须等已派发发布结束。

未发布记录不设置TTL，只有完成发布的记录才进入过期回收。发布与ACK保持幂等。实体侧的`AcknowledgeRemoteCommit`必须并发安全：outbox和Committed回执的本地确认可能交错，生成实体经`Tracker.AdvanceVersion`原子CAS满足。阶段耗时见[指标](OBSERVABILITY.md)，功能验收不能当作新TPS结论。

## 多写者：本地副本与权威

共享模式下多个进程都能写同一实体，每个写者进程内存里各有一份实例；独占模式下所有权转走又转回、或退出共享回到独占，也会让本进程的实例错过别的进程的提交。内存实例只在本进程写入时随确认推进，别的进程的提交不会改它。规则（2026-11-04 修复，见 CHANGELOG）：

- **写准入对齐权威**：取得写许可（独占的持久许可，或共享模式的分布式锁 + 许可）后，本地实例版本落后于权威版本、或停在 `Fenced`（转出时置入，状态表里不会自己恢复）时，先经 `Backend.UnloadRemoteEntity`（正式 loader 是 `ManagerAccess.Unload`，`DestroyReasonMemoryUnload`，不删持久数据）把它仅内存卸载，再加载：内存未命中才会经 loader 从权威重建。卸载经 ctx 的本地执行入口（Nest I/O 准备阶段注入的业务池续行）执行，等实体锁受 OpTimeout 约束；卸载没完成或重载撞上卸载收尾窗口返回可重试的 `entity.ErrRemoteEntityReloading`（包裹 `ErrRemoteFenced`）。Sync 与持久拒绝后的卸载同一规则：旧同步状态关闭、订阅保持，重载后全量。持久拒绝等待框架卸载的实例仍走拒绝收尾那条路径。
- **outbox 确认只落到写者实例**：每个进程的 outbox 协调者扫描共享 outbox 里全部未发布事务，会代发别的进程的提交。确认（推进版本向量、生成实体的 `AcknowledgeRemoteCommit`）只在本地实例就是这笔提交的写者时执行：它的版本向量在确认前是 `(BaseVersion, LockFence)`、重复确认时是 `(NextVersion, LockFence)`。否则不动这份实例（计 `remote_entity.foreign_ack_skipped_total`），由下一次写准入从权威重载；发布与版本通知照常。修复前代发会把没有那笔修改的实例推进到新版本，下一笔整文档写覆盖掉别的进程的修改。
- **边界**：loader 不支持卸载（自定义 loader 没实现 `IRemoteEntityUnloader`）时换不掉实例，落后时照旧报 `ErrRemoteVersionConflict`、`Fenced` 时报 `ErrRemoteFenced`；事务外独立删档（`Destroy(deletePersisted=true)` 不在 handler 内）在 `EntityManager.Destroy` 持实体锁时准备批次，带 `entity.WithRemoteWriteTargetHeld`，实例落后时不卸载、按版本冲突拒绝，下一次 Nest 写准入换掉它之后再删。非写入路径读本进程内存里的 Remote 实例（不经写准入）可能是旧的，跨进程读取用快照（下一节）。

## 实体归属守卫

独占模式下没有所有权记录的实体由**第一个写它的进程**认领（写准入里的 `ClaimOwnership`）。实体归某个服务写（例如联盟服务独占全部 Alliance，见[联盟服务方案](../maintenance/FEATURE-2026-11-04-alliance-service.md#210-生成器最小增量决定-3)）时，别的服务的进程一旦先写到一个新实体就会把它认领走。守卫（2026-11-04）：

- **声明**：`coreremote.Config.ForeignKinds []entity.EntityKind` 列出“声明归属于别的服务”的 kind；wiring 经 `wiring/remoteentity.WithForeignKinds(kinds...)` 传入（生成工程由实体注解 `service=<name>` 推出，阶段 2），Mod Init 时校验一次：每个 kind 都必须是已注册的 Remote 托管 kind。不写 YAML 键，不在两处声明。
- **拒绝点**：`PrepareRemoteWriteBatch` 在写额度、wrapper 与任何所有权读写之前整批检查，批里有一个 foreign kind 整批拒绝；`ClaimRemoteOwnership`、`EnterRemoteSharedMode`、`LeaveRemoteSharedMode`、`TransferRemoteOwnership` 在 `beginOwnershipTransition` 里同样先拒绝。返回 `entity.ErrRemoteKindNotOwned`（包裹 `ErrRemoteWriteCapabilityDisabled`，不可重试），计 `remote_entity.foreign_kind_rejected_total`。权威里不会留下这个进程的所有权记录。
- **不受影响**：`GetRemoteOwnership` 与快照读照常（RPC 服务端“改投”要读 owner）；未列入的 kind（`ForeignKinds` 为空时即全部 kind）行为与没有守卫时逐字相同——Player 的静态 sid 归属、共享模式、SLG 联盟夹具都不变。
- **边界**：守卫只管本进程，不校验别的进程的配置；归属服务本身仍按首写认领与持久 fence 决定哪个实例是 owner（分片与改投见 [guide/09“按槽位路由与改投”](guide/09-services.md#2a-按槽位路由与改投)）。回归：`ownership_test.go` 的 `TestForeignKindIsRejectedBeforeOwnershipClaim`、`wiring/remoteentity` 的 `TestRemoteEntityModForeignKinds`。

## 读取

业务读取 `RemoteSnapshotEnvelope`，不读取远端 live Entity。L1/L2 以 tenant/entity/kind/scope/policy 为完整 key；payload 是 `FrozenRemoteSnapshotPayload`，L1 命中不复制 backing bytes，解码时显式取 copy。相同 epoch/version 的不同 checksum 被拒绝。checksum（CRC64）在 Mongo 中只存 8 字节 Binary 一种形态；旧版本写下的 BSON 数字形态与 null / undefined 不再读取，解码时报 `remote checksum has unexpected BSON type`。

- Cached：允许当前缓存版本。
- Monotonic：要求不低于调用方 minVersion；缓存不满足时不等待推送，按 `(key, After)` 合并回源一次（Mirror 第 2 步起）。
- Linearizable：直接权威存储读取。

Interest 使用 TTL 软状态，本机在半 TTL 前不重复发布 renewal，避免每次 L1 read 产生网络消息。

## 启动约束

RemoteEntityMod 缺少以下任一能力必须启动失败：Redis versioned lock（竞争协调；所有权的权威是 Mongo 的 ownership / grant / fence，正式装配不用 Redis marker，见[Remote实现篇](impl/05-remote-mirror.md)）、跨服 SyncBus、`IRemoteAtomicBatchCommitter`、`IRemoteSnapshotLoader`、`IRemoteCommitOutbox`。依赖在 Start 后封存，禁止运行期替换造成 data race。

Broadcast 和匿名 callback 不允许写 remote-managed entity，因为它们不具备一个明确的原子 transaction/rollback 边界。跨实体写使用声明目标的 Multi handler。
