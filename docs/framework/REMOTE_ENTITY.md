# Remote Entity 生产协议

Remote Entity 只保留一条写链路：Nest 声明目标 → 全局顺序获取 write gate/ownership read lock/分布式 lock → Entity mutex 内冻结 commit → WAL 或同步 Mongo transaction → snapshot/outbox 发布 → ACK dirty → 释放 fence。

## 一致性向量

每次提交同时校验四个互不混用的维度：`StateVersion`、`MarkerEpoch`、`LockFence`、`RouteEpoch`。StateVersion 必须严格 `base+1`；ownership/route 代际只能前进；shared write 必须带非零 LockFence。TransactionID 是幂等键。LockFence 只在共享模式参与权威校验（每次写许可分配一个）；独占写不取写许可，提交里的 LockFence 是实例版本向量沿用的值，投影不校验它（[独占写准入](#独占写准入)）。

## 存储事务

`framework/remoteentity` 的 `MongoCommitter`（接线入口是 `wiring/remoteentity` 的 `WithMongoStorage`）在同一 Mongo transaction 中完成：

1. entity meta 的 CAS：`_ver == BaseVersion` 与所有权代际（`_owner_epoch`、`_owner_route`）；共享模式另校验 `_grant_fence` 与 `_grant_token`（[独占写准入](#独占写准入)）；
2. 全部 DAO full document mutation 或 delete target；
3. 权威 snapshot；
4. transaction receipt 与待发布 outbox。

Remote 实体 handler 里 `nest.Emit` / `saga.EmitStart`：Emit 把 memory 升到 strict，effect 与 Remote 写进同一条 WAL 提交记录，投影时一起进 Mongo（DataEngine 投影的同一个事务），outbox 在投影之后各发布一次；handler 失败时两者都不提交。2026-11-04 起有真实依赖端到端（`codegen/internal/entity/testdata/remoteflow/emit_test.go` 的 `TestGeneratedRemoteHandlerEmitsEffectsWithTheRemoteWrite`，经 `scripts/test-remote-generated.sh`，strict 与声明 memory 两种 handler）：投影被拦住时 Mongo 与 outbox 都看不到这笔，记录里正好两个 effect；放行后版本 +1、两个 effect 各发布一次；失败的 handler 不留 Remote 写也不发 effect。原生 saga 步骤仍不能写 Remote（[SAGA.md 第 7 条](SAGA.md#原生步骤执行契约u-0280维护者-2026-10-05-决定2026-10-28-决定-2a-改为回执在实体里)），这里覆盖的是“Remote handler 启动 saga / 发 effect”。

多实体写要求 `IRemoteAtomicBatchCommitter`，不支持时 fail-fast，绝不退化成逐实体提交。删除 commit 必须带完整 `RemoteDataDelete` 集合，codegen 自动生成。

框架只接受完整的 `IRemoteEntityBackend`，并通过 `IRemoteEntityManager.SetBackend` 一次注入，缺少 load、原子 batch commit、snapshot loader 或 outbox 任一能力都会在编译期暴露。组合方式是 `remoteentity.NewBackend(loader, remoteentity.NewMongoCommitter(...))`：业务只实现 `LoadRemoteEntity`，Wiring中由 `WithMongoStorage(loader)` 装配（没有 `NewMongoBackend`）。DAO 库名映射表只给 DataEngine 一次（`engine.MongoStoreConfig.Databases`：wiring 由 DataEngine Mod 冻结后传入，自建装配用 `dataengine.NewDatabaseMap` 合成），`engine.Assemble` / `MongoStore.SetRemoteProjection` 接 Remote 投影时自动把同一张表绑给 `Backend` / `MongoCommitter`（二者实现 `dataengine.DatabaseMapBinder`）；Remote 存储已绑另一张表时接线报错、不覆盖。没有表时两边都拒绝读写带库名的 DAO 文档（`dataengine.ErrDatabaseMapNotBound`，维护者 2026-10-21）。Remote 要在接上投影之前写 DAO 文档时（例如先启动、恢复上一轮未完成的提交），先调 `Backend.BindDatabaseMap` 绑同一张。

## WAL 与 outbox

Nest WAL当前codec7 持久化完整 RemoteCommit（mutation、delete、snapshot、invalidation）。Async handler 在 WAL admission 后返回，write gate 由 finalizer 持有，直到 backend status 确认。坏事务采用单次检查后退避重排队，不独占 worker。

Mongo transaction首先标记`Applied`；唯一的`RecoverOutbox`协调者负责快照/复制发布，成功后标记`Committed`。投影只持久提交并唤醒outbox，finalizer只确认和释放，不自行发布；`ApplyRemoteCommits`成功只代表持久回执，需要发布确认的调用者等待`FlushRemoteTransaction`。memory/strict/pipelined的回复与写额度释放语义保持不变，不能提前到Applied。

### outbox 发布归属

每个进程的协调者都扫描共享 outbox 里全部 Applied 提交，但等发布结论的 tracker（strict 写的确认、延迟收尾的等待）只在写者进程里。别的进程代发后写者没人通知：修复前（2026-11-06）写者的 strict 写等到 Nest 请求期限（3 s）超时、转延迟收尾，收尾持写门等到 `finalize_projection_timeout`（30 s）才回源释放，同一实体其间的写全部在写门上超时。规则：

- **写者自己发**：事务记录带写者 sid（`_remote_entity_transactions.writer_sid`，`MongoCommitter` 的 sid）。扫描立即发布本进程写的提交；没有写者（自定义 outbox 不填 `RemoteCommitStatus.WriterSID`）或没有写入时刻的，同样立即发布（修复前的行为）。
- **别的进程过宽限期才代发**：别的进程写的提交写入（`created_at`，写者的时钟）后超过 `remote_entity.outbox_takeover_grace` 仍是 Applied，视为写者已经不在，由扫到它的进程代发（`remote_entity.outbox.takeover_total{reason="grace"}`）。宽限期零值与下限都是 `finalize_projection_timeout`（wiring 对更小的配置值报错）：写者活着但发布慢、被代发时，它的 tracker 仍没人通知，要等延迟收尾到期回源才结束；宽限期不短于收尾期限，代发发生时写者的收尾期限也已基本到期（收尾期限从 strict 等待超时后才开始算），写者不会因代发再多挂一个收尾期限。写者自己标记时发现记录已是 Committed（读页之后被代发），`MarkRemoteCommitPublished` 照旧当作成功，tracker 正常结束。
- **同实体后继立即代发**：一页里同一实体上排在某个留存提交后面、本轮要发布的提交已经存在时，留存提交立即代发并先发（`reason="successor"`）。同一实体的提交在写门与写锁下串行，写者发布前一直持有它们，后面能有别的提交说明前面的写者已经失去这个实体（崩溃后被接管）；这样接管者的写不用陪着等宽限期，同实体发布顺序也保持。只在页内判断，跨页时后面的提交照常先发（与发布失败时相同，同实体顺序只在一页内尽力保持）。
- **崩溃接管**：写者崩溃后可能再没有本地写去唤醒别的协调者，所以每次扫描为留存提交里最早的到期时刻安排一次唤醒（`armOutboxTakeover`），到期后扫到它的进程代发。发布按事务 `_id` 幂等：`MarkRemoteCommitPublished` 只把 Applied 改成 Committed，重复发布同一快照由版本 / 水位去重；两个进程同时到期代发，第二个标记时读到 Committed 视为成功。代价是写者崩溃留下的提交晚一个宽限期才发布（订阅方在这段时间读不到推送，回源仍是新版本）。前提是崩溃之后还有别的进程扫描过一次 outbox：扫描由本进程的写（投影后唤醒）、启动恢复与上述到期唤醒触发，没有周期轮询；别的进程都不写时，留存提交等到同 sid 重启或下一次别处的写（那时通常已过宽限期，立即代发）。
- **同 sid 重启**：同 sid 的新进程把旧进程留下的提交当作自己的，启动恢复（`Assembly.Start` → `RecoverOutbox`）立即发布，不等宽限期；单实例锁保证此时旧进程已经不在。崩溃重启的短暂重叠里两边都可能发布，按上一条幂等。
- **不回源**：写者的 tracker 停在 Applied 时不另外点读权威（KNOWN-LIMITS 原条目的方案 B），只靠自己的发布推进 tracker、靠延迟收尾的期限兜底；宽限期下限保证代发之后写者最多再等一次 strict 等待的剩余时间（strict 的收尾期限从等待超时后才开始算），或一次收尾重试间隔（async 的收尾期限早于代发到期，之后按 `finalize_retry_interval` 退避回源，最长 5 s）。延迟收尾因期限到期才回源的次数计 `remote_entity.finalize_projection_timeout_total`，正常应为 0。
- **时钟同步要求**：宽限期按写者写入的 `created_at`（写者的时钟）与代发方的本地时钟比较，实际起作用的宽限期是 `宽限期 − Δ`（Δ 为代发方时钟快于写者的量，慢时为负）。两个方向都只影响时延与同一实体的可用性，不影响正确性：每笔提交仍只由 Applied 改成 Committed 一次，重复投递按版本去重，写者 tracker 仍在上一条的上界内结束（2026-11-06 真实进程演练，见下）。
  - **代发方快 Δ**：活着的写者的保护窗口缩小 Δ。只要 `宽限期 − Δ` 仍大于写者发布的正常耗时（含协调者排队；作为量级参考，两实例 64 并发跨实例 `Join` 整次写的 p99 约 0.6 s）就没有影响；Δ 接近或超过宽限期时退化为修复前：别的进程在写者的协调者读到提交之前代发，写者的 strict 写超时答“结果未知”，延迟收尾持写门到收尾期限，同一实体其间的写在写门上超时（`finalize_projection_timeout_total`、`remote_entity_write_gate_timeout_total` 增长）。写者崩溃后的接管提前 Δ。
  - **代发方慢 Δ**：崩溃接管晚 Δ（`宽限期 + Δ`），活着但卡住的写者被代发也晚 Δ；写者自己的延迟收尾在收尾期限后按退避回源，并唤醒自己的协调者，不依赖代发。
  - **写者之间的偏差**：outbox 按 `created_at` 排序，同一实体上先后两笔提交的写者时钟差超过两笔的间隔时，前一笔排到后一笔后面，“同实体后继立即代发”认不出它，它照常等宽限期、晚于后一笔发布（被版本比较丢弃，同跨页情形）。
  - **要求**：各进程用 NTP / chrony 校时，偏差保持在 1 s 以内并对偏差告警；缺省宽限期 30 s 时这留有远大于写者发布耗时的余量。不要用调小宽限期去换更快的崩溃接管：宽限期越小，同样的偏差越容易吃掉活着的写者的保护窗口。演练（宽限期与收尾期限都按 1/10 缩为 3 s、写者发布慢 300 ms）：代发方快 0 / 1 / 2 s 时写者自己发布、没有代发；快 2.7 s 时代发抢在写者标记之前，但写者已读到页，tracker 照常确认；快 3 s / 6 s 时写者的 strict 写超时、写门持有约 4 s（strict 1 s + 收尾期限 3 s）。写者崩溃后的接管时延实测为 `宽限期 − Δ`（Δ = −6 / −3 / −1 / 0 / +1 / +2.5 s 时 9.0 / 6.0 / 4.0 / 3.1 / 2.0 / 0.54 s），Δ ≥ 宽限期时下一次扫描即代发（0.08～0.12 s），每笔只发布一次。

回归：[outbox_test.go](../../framework/remoteentity/outbox_test.go) 的 `TestOutboxWriterPublishesItsOwnCommitBeforeOtherCoordinators`（两个协调者共享 outbox，非写者先扫，写者的 strict 写在期限内确认；修复前停在 Applied 等到超时）、`TestOutboxForeignCommitIsTakenOverOnlyAfterTheGrace`、`TestOutboxCrashedWritersCommitIsPublishedAfterTheGraceWithoutAnotherWake`、`TestOutboxRestartedWriterWithTheSameSidPublishesLeftoverCommitsAtOnce`、`TestOutboxForeignPredecessorOfAPublishedCommitIsTakenOverFirst`、`TestOutboxWriterFindingItsCommitAlreadyPublishedCompletesItsTracker`、`TestOutboxWriterOvertakenBeforeItsScanIsReleasedAtTheProjectionDeadline`、`TestOutboxSuccessorRuleUnderWriterClockSkewFallsBackToTheGrace`。

真实进程演练（2026-11-06，[outbox_takeover_integration_test.go](../../framework/remoteentity/outbox_takeover_integration_test.go)，`ROOST_OUTBOX_DRILL=1`，`scripts/test-remote-matrix.sh` 的 `outbox-takeover` 格）：两个子进程各是完整的 Remote 写者（正式 Assembly、真实 Mongo / Redis / JetStream、真实 WAL 与投影器），写者到 NATS 的下行链路经 toxiproxy 加延迟（发布确认变慢），接管方持续写自己的实体并对写者的实体续约快照兴趣；时钟偏差经 `outboxNow` 只注入接管方的发布归属判定。时长按 1/10 缩放（宽限期 = 收尾期限 = 3 s，strict 等待 1 s，收尾重试间隔 500 ms）。写者发布卡 3.5 s（超过宽限期）时：接管方在写入后 3.01～3.02 s 代发两笔，每笔只由 Applied 改成 Committed 一次（写者迟到的发布标记时读到 Committed，当作成功）；被抢先的那笔 strict 写 1.0 s 答“结果未知”，写门在代发后 0.95 s 释放（上界：一次 strict 等待的剩余 1 s），期间同实体的写在写门上超时 1 次，释放后同实体的写 46 ms 确认；宽限期 = 2 × 收尾期限时代发后 0.96 s 释放（上界：一次收尾重试间隔）。写者指标 `finalize_projection_timeout_total` 1 / 2、`remote_entity_write_gate_timeout_total` 1，接管方 `outbox.takeover_total{reason="grace"}` 2、自己的写 0 失败。加上“慢发布转后台”（[outbox 发布并发](#outbox-发布并发)）之后，写者的协调者不再被前一笔的慢发布挡在页屏障上，也会去发被抢先的那笔，但它的确认同样被拖慢，只在代发之后标记、读到已是 Committed，tracker 由此直接结束：同机复跑（各 3 轮 + `-race`）宽限期 = 收尾期限时写门在代发后约 0.51 s 释放，宽限期 = 2 × 收尾期限时约 0.96 s；演练的场景前提相应改为“写者对这笔的标记只能发生在代发之后”。

### 持久拒绝（Rejected）的来源与收尾

事务的 Rejected 只有 Durability 0（memory）一种生产来源：`Commit` 直接写权威时被明确拒绝（写前校验失败、fenced、版本冲突），在业务池同步回滚、隔离，`Close` 释放后仅内存卸载；结果未知时由 finalizer 回源，读不到结论就用 `RejectUnresolvedRemoteCommits` 以同一事务 `_id` 写下持久拒绝（撞键则按已有记录的真实状态收尾），拿到 Rejected 后回滚、隔离、释放 gate、仅内存卸载，再把 false 交给提交后工作。WAL 准入前的 `Abort` 只在本进程 tracker 记 Rejected，不写持久记录。

Durability 1/2/3（async / strict / 随 strict 路径提交的 pipelined）的 WAL 投影不产生 Rejected：投影器写权威遇到 fence / 版本冲突是 `ErrProjectionConflict` fatal（进程级 fail-stop，记录不 ack）。strict / pipelined 的 `Commit` 失败因此一律带 `entity.ErrRemotePersistenceIndeterminate`（回复判别表第 2 行），框架自带 remoteentity 只在 memory 下回复 `nest.ErrRemotePartRejected`；finalizer 对 WAL 事务读不到结论时只隔离重试，不写拒绝。维护者 2026-10-14 据此删除了只供测试注入 WAL 级拒绝的 `MongoCommitter` / `Backend.RejectRemoteCommitsInTransaction` 与 `Manager.RejectRemoteTransaction`；守卫用例 `TestWALRemoteAuthorityConflictNeverProducesRejected`、`TestRemoteWALCommitFailureNeverRepliesRemotePartRejected`。

### outbox 发布并发

协调者共用启动恢复、显式恢复、重试和停止屏障，页内按完整事务有界并行；涉及任意同一Entity的事务按扫描顺序尝试，失败仍保留Applied。正式配置`remote_entity.outbox_publish_workers`默认32（0取32；2026-11-05 由8调大，strict 写等发布完成才答复，8 个并发在联盟 Join 约 330 次/秒时已占满约 86%），范围1～64；设1用于串行诊断，不随Nest io 池 worker数放大。带游标的正式Backend可越过失败页继续扫描，不删除失败记录。停止必须等已派发发布结束（含转到后台的）。

**慢发布不挡后面的提交**（2026-11-06）：后台发布循环一页里已没有可派发的事务、在途的发布 100 ms 内没有任何一项结束时，在途的转到后台继续，循环去读下一页；修复前一次发一页、等整页结束，一笔慢发布（一次几秒的 Mongo 标记、一次慢的快照写）挡住之后全部提交，单实例 128 并发时全部写额度（`max_concurrent_writes`）停在等确认、到 Nest 请求期限同时超时。转到后台的仍占并发额度（`outbox_publish_workers` 由全部在途发布共用），下一页跳过它们，涉及同一实体的后继等它结束（结束时唤醒发布循环），同实体发布顺序不变；失败照常把 tracker 置为 Indeterminate，发布循环按失败退避后重试；停机等它们结束。显式 `RecoverOutbox`（启动恢复）仍等本次派发的全部结束。转后台的发布数计 `remote_entity.outbox.detached_total`，正常负载下应很少；持续增长说明 Mongo 标记或快照发布在变慢。回归 `TestOutboxSlowPublicationDoesNotHoldUpLaterCommits`（修前 strict 写 `commit timed out`、tracker 停在 Applied）、`TestOutboxDetachedPublicationKeepsSameEntityOrder`、`TestOutboxDetachedPublicationIsDrainedBeforeStop`、`TestOutboxDetachedPublicationFailureIsRetried`。

**快照 publish 分片**（2026-11-06）：`RemoteSnapshotCache` 的 64 把 publish 分片锁（同一 key 的 L1 写与它的 L2 调用串行）改按整个 key 打散（`entity.EntityIDStripe`）。实体 ID 低 10 位是 kind 与 category，修复前分片只取低 6 位，同一 kind 的全部快照发布挤在一把锁上、按 L2 往返逐个串行，outbox 的并发发布退化成 1。快照客户端的兴趣条带锁同样改用它。回归 `TestRemoteSnapshotPublishShardSpreadsOneKind`（修前 64 个同 kind 实体落在 1 个分片）。

未发布记录不设置TTL，只有完成发布的记录才进入过期回收。发布与ACK保持幂等。实体侧的`AcknowledgeRemoteCommit`必须并发安全：outbox和Committed回执的本地确认可能交错，生成实体经`Tracker.AdvanceVersion`原子CAS满足。阶段耗时见[指标](OBSERVABILITY.md)，功能验收不能当作新TPS结论。

## 独占写准入

前提（维护者 2026-11-06）：**一个实体同一时刻只有一个 owner**。由 App 层统一的单实例锁与 fail-stop 保证（[App 生命周期](impl/01-app-lifecycle.md)）：失去归属的进程先停，新 owner 才接手；不考虑旧进程与新进程同时以 owner 身份写（脑裂）。所有权记录里是单个 owner sid、非共享的实体（独占）据此不再每次请求向 Mongo 确认归属；共享模式不变。

- **稳定持有不碰权威**：所有权缓存是本 sid 独占、当前托管的实例就是在这份租约下对齐过的那份（`heldExclusively`）时，写准入（Nest 每个以它为声明目标的请求，读写都一样）不做任何 Mongo 读写：不取写许可（修改前每次 `GrantWrite`，`_remote_entity_meta` 上一次多数派 `findAndModify`），也不按 `marker_cache_ttl` 重读所有权记录。本进程是唯一写者，内存就是最新。缓存在这里不按 TTL 过期：独占实体的所有权只由 owner 进程自己改（认领、转移、进出共享都要求 `expected.OwnerSid == localSid`，持写门与所有权写锁串行），本进程的改动同步写进缓存，结果未知时置为未知。
- **对齐时刻**：本地可能落后的入口只有三类，各在之后的第一次准入（或预加载 / 路由前置检查时）向权威对齐一次版本（`ReadAuthorityState`，`_remote_entity_meta` 一次 `FindOne`，只读；首写认领直接用认领带回的状态，不另读，见第 2 条）：
  1. **加载**：冷加载、仅内存卸载后再访问（淘汰、持久拒绝后的重载、落后副本换代）——实例是新指针，对齐记录失配；
  2. **取得归属**：首写认领、转入（别的 owner 转来，`MarkerEpoch` 前进）、崩溃接管（同 sid 重启，新进程里没有对齐记录）、起服预加载（`PreloadOwnedEntities`）与路由前置检查（`ServeRoutedRequest`）的冷加载——后两者读到的权威版本与加载的实例一致、状态可写时直接记下对齐，之后第一次请求不再读。首写认领用 `WriteAuthority.ClaimAuthorityState`（2026-11-06）：准入只在所有权缓存是“没有记录”时认领（缓存过期时这次准入刚读过），直接插入第一代记录（版本 0），插入成功时这份记录就是对齐所需的权威状态；插入失败（撞上已有记录：并发认领赢了，或记录早已存在；或结果未知）读回一次，owner 是别人照旧按 fence 拒绝 / 改投，是本 sid 就用读到的版本对齐——比本地实例新时同样先卸载重载再执行。认领之后不再读权威，认领前也不再先读一次；`ClaimOwnership` 是它只返回所有权的形式（Redis marker 等非持久装配只有它，不做版本对齐）；
  3. **从共享退回独占**：`LeaveRemoteSharedMode` 推进 `MarkerEpoch`，对齐记录失配。
  所有权的任何变更都推进 `MarkerEpoch`，所以“租约变了”与“实例换了”两条就覆盖了全部入口。对齐时实例版本落后于权威或停在 `Fenced`，按下一节先卸载再从权威加载；一致就记下对齐（`markAligned`），此后回到稳定持有。
- **只读请求**：handler 不改持久字段时，生成的 `HasRemoteCommitLocked` 答 false，批次没有 Remote 提交：不写 WAL、不投影、不发 Mongo 命令。它仍走写准入：占一个 `max_concurrent_writes` 名额、过实体写门，时长是 handler 本身（稳定持有时准入不做 I/O）。**没有新增只读声明**：写门让读请求排在同一实体上结论未定的写之后，读不到可能被回滚 / 结果未知的修改；绕开写门的只读声明会改变这条语义，并要同时改 codegen、Nest 与 Remote 三层。代价：同一实体上的读要等在途写拿到持久结论，写额度被在途写占满时读同样答 overloaded。
- **兜底**：独占提交在投影元数据 CAS 上只校验 `_ver == BaseVersion`、`_owner_epoch`、`_owner_route`（`authorityCommitFilter`；共享提交另校验 `_grant_fence` 与非空 `_grant_token`。同一个 epoch 只对应一种模式，过滤条件按权威记录的 `_owner_shared` 选），与提交同一个 Mongo 事务，不增加往返。前提被打破（例如同 sid 两个进程重叠、都以为自己是 owner）时，后到的那笔因版本或代际不符被拒，权威不被覆盖；`MongoCommitter` 记 ERROR 与 `remote_entity.commit_conflict_total`。之后：
  - **Durability 0**（memory）：明确拒绝（`ErrRemoteVersionConflict`），按[持久拒绝](#持久拒绝rejected的来源与收尾)回滚、隔离、仅内存卸载；下一次访问重新对齐（读到别的 owner 时答 `ErrRemoteFenced`）。
  - **Durability 1/2/3**（WAL）：投影器 `ErrProjectionConflict` fatal，进程 fail-stop（DataEngine `OnFatal` → App 的 RuntimeFailure，与修改前写许可被取代时相同）。不另加实体级恢复：WAL 里这笔已持久、不能丢，也不能改写到新版本上；进程停下由运维与 App 单实例裁决谁是 owner，这正是前提要求的“失去归属的进程先停”。
- **确认认写者**：独占写不再带每次递增的 fence，同一实体上各次独占写的 LockFence 相同，别的进程的提交可能与一份空闲副本的 `(BaseVersion, LockFence)` 恰好一致。`acknowledgeRemoteCommit` 因此另要求实例的 wrapper 此刻正由这笔事务持有写门（定稿时记下、写门释放时清除）；写门释放后的重复确认只在向量已经等于这笔的结果时放行（幂等）。否则不推进（`remote_entity.foreign_ack_skipped_total`）。
- **共享模式不变**：分布式锁、每次 `GrantWrite`（现在只接受共享实体，去掉了 owner 参数）、`_grant_fence` / `_grant_token` 校验照旧。
- **指标**：`remote_entity.exclusive_admission_total{path=held|aligned}`（稳定持有 / 对齐了一次的独占准入，正常时几乎全是 `held`）、`remote_entity.commit_conflict_total`（应为 0）。
- **回归**：[write_admission_test.go](../../framework/remoteentity/write_admission_test.go) 的 `TestExclusiveOwnerWritesWithoutAWriteGrant`（5 次稳定写：修前 5 次 `findAndModify` 与 5 次所有权读，修后 0）、`TestExclusiveReadOnlyRequestTouchesNoMongo`（经 Nest 的 5 次只读请求：修前 5 次 `findAndModify`，修后没有任何 Mongo 写、不读元数据与数据文档）、`TestExclusiveOwnerAlignsBeforeServingAfterItMayHaveFallenBehind`（转走又转回、退出共享、同 sid 重启与淘汰后再访问：先对齐再答复 / 写入）、`TestRoutedColdLoadAlignsSoTheFirstRequestReadsNoAuthority`、`TestExclusiveFirstWriteClaimNeedsNoAlignmentRead`（新实体第一次写：修前认领前读一次、认领后再读一次对齐，修后 `_remote_entity_meta` 上只剩“没有记录”那一次读与认领的一次插入）、`TestExclusiveClaimOfAnExistingNewerRecordReloadsBeforeWriting`（认领撞上本 sid 已有的更新记录：先重载再写）、`TestExclusiveWriteFromABrokenPremiseIsRejectedAtProjection`（同 sid 两个进程、所有权在 owner 背后被改）、`TestPublishingAnotherIncarnationsCommitDoesNotAdvanceAnIdleCopy`；[mongo_authority_test.go](../../framework/remoteentity/mongo_authority_test.go) 的 `TestExclusiveCommitNeedsNoGrantButChecksVersionAndEpoch`。

## 多写者：本地副本与权威

共享模式下多个进程都能写同一实体，每个写者进程内存里各有一份实例；独占模式下所有权转走又转回、或退出共享回到独占，也会让本进程的实例错过别的进程的提交。内存实例只在本进程写入时随确认推进，别的进程的提交不会改它。规则（2026-11-04 修复，见 CHANGELOG）：

- **写准入对齐权威**：共享模式取得分布式锁与写许可后、独占模式在[对齐时刻](#独占写准入)读到权威版本后，本地实例版本落后于权威版本、或停在 `Fenced`（转出时置入，状态表里不会自己恢复）时，先经 `Backend.UnloadRemoteEntity`（正式 loader 是 `ManagerAccess.Unload`，`DestroyReasonMemoryUnload`，不删持久数据）把它仅内存卸载，再加载：内存未命中才会经 loader 从权威重建。卸载经 ctx 的本地执行入口（Nest I/O 准备阶段注入的业务池续行）执行，等实体锁受 OpTimeout 约束；卸载没完成或重载撞上卸载收尾窗口返回可重试的 `entity.ErrRemoteEntityReloading`（包裹 `ErrRemoteFenced`）。Sync 与持久拒绝后的卸载同一规则：旧同步状态关闭、订阅保持，重载后全量。持久拒绝等待框架卸载的实例仍走拒绝收尾那条路径。
- **outbox 确认只落到写者实例**：每个进程的 outbox 协调者扫描共享 outbox 里全部未发布事务，过了宽限期会代发别的进程的提交（[outbox 发布归属](#outbox-发布归属)）。确认（推进版本向量、生成实体的 `AcknowledgeRemoteCommit`）只在本地实例就是这笔提交的写者时执行：它的版本向量在确认前是 `(BaseVersion, LockFence)`、重复确认时是 `(NextVersion, LockFence)`，并且它的 wrapper 正由这笔事务持有写门（写门释放后只放行已等于这笔结果的重复确认，见[独占写准入](#独占写准入)）。否则不动这份实例（计 `remote_entity.foreign_ack_skipped_total`），由下一次写准入从权威重载；发布与版本通知照常。修复前代发会把没有那笔修改的实例推进到新版本，下一笔整文档写覆盖掉别的进程的修改。
- **边界**：loader 不支持卸载（自定义 loader 没实现 `IRemoteEntityUnloader`）时换不掉实例，落后时照旧报 `ErrRemoteVersionConflict`、`Fenced` 时报 `ErrRemoteFenced`；事务外独立删档（`Destroy(deletePersisted=true)` 不在 handler 内）在 `EntityManager.Destroy` 持实体锁时准备批次，带 `entity.WithRemoteWriteTargetHeld`，实例落后时不卸载、按版本冲突拒绝，下一次 Nest 写准入换掉它之后再删。非写入路径读本进程内存里的 Remote 实例（不经写准入）可能是旧的，跨进程读取用快照（下一节）。独占稳定持有期间不再向权威确认（[独占写准入](#独占写准入)）：前提被打破时，后到的进程在下一次写之前读到的是它自己的内存。

## 实体归属守卫

独占模式下没有所有权记录的实体由**第一个写它的进程**认领（写准入里的 `ClaimAuthorityState`）。实体归某个服务写（例如联盟服务独占全部 Alliance，见[联盟服务方案](../maintenance/FEATURE-2026-11-04-alliance-service.md#210-生成器最小增量决定-3)）时，别的服务的进程一旦先写到一个新实体就会把它认领走。守卫（2026-11-04）：

- **声明**：`coreremote.Config.ForeignKinds []entity.EntityKind` 列出“声明归属于别的服务”的 kind；wiring 经 `wiring/remoteentity.WithForeignKinds(kinds...)` 传入（生成工程由实体注解 `service=<name>` 推出，阶段 2），Mod Init 时校验一次：每个 kind 都必须是已注册的 Remote 托管 kind。不写 YAML 键，不在两处声明。
- **拒绝点**：`PrepareRemoteWriteBatch` 在写额度、wrapper 与任何所有权读写之前整批检查，批里有一个 foreign kind 整批拒绝；`ClaimRemoteOwnership`、`EnterRemoteSharedMode`、`LeaveRemoteSharedMode`、`TransferRemoteOwnership` 在 `beginOwnershipTransition` 里同样先拒绝。返回 `entity.ErrRemoteKindNotOwned`（包裹 `ErrRemoteWriteCapabilityDisabled`，不可重试），计 `remote_entity.foreign_kind_rejected_total`。权威里不会留下这个进程的所有权记录。
- **不受影响**：`GetRemoteOwnership` 与快照读照常（RPC 服务端“改投”要读 owner）；未列入的 kind（`ForeignKinds` 为空时即全部 kind）行为与没有守卫时逐字相同——Player 的静态 sid 归属、共享模式、SLG 联盟夹具都不变。
- **边界**：守卫只管本进程，不校验别的进程的配置；归属服务本身仍按首写认领与持久 fence 决定哪个实例是 owner（多实例路由与改投见 [guide/09“路由 RPC 与改投”](guide/09-services.md#2a-路由-rpc-与改投)）。回归：`ownership_test.go` 的 `TestForeignKindIsRejectedBeforeOwnershipClaim`、`wiring/remoteentity` 的 `TestRemoteEntityModForeignKinds`。

## 路由登记

实体服务有多个实例时（[联盟服务方案 2.7](../maintenance/FEATURE-2026-11-04-alliance-service.md#27-分片接管与迁移决定-2)），owner 在 Redis 登记一份带过期的“实体 ID → owner sid + 所有权代际”，调用方的路由 RPC 按它找 owner（[guide/09“路由 RPC 与改投”](guide/09-services.md#2a-路由-rpc-与改投)）。它只是路由提示：写准入永远只看所有权记录（独占稳定持有时看本进程对它的缓存，见[独占写准入](#独占写准入)）与版本 / 代际，不读登记。实现：`framework/remoteentity` 的 `route_store.go`（键、值、三个脚本、查找）与 `route.go`（Manager 上的登记、续期、前置检查与预加载）。

- **哪些 kind**：`coreremote.Config.RouteKinds`，wiring 经 `wiring/remoteentity.WithRouteKinds(kinds...)` 传入（生成工程是声明了 `service=<本服务>` 的 Remote 托管 kind），Mod Init 校验一次：已注册的 Remote 托管 kind、不在 `WithForeignKinds` 里。不写 YAML 键；不传时没有登记，行为与没有这项功能时逐字相同。
- **键与值**：键 `<remote_entity.snapshot_l2_key_prefix>:remote_entity:route:<完整 Entity ID>`（前缀为空时没有前缀与冒号，与 L2 快照键同一规则，不带 hash tag，Redis Cluster 下按键分散）；值 `<owner sid>:<MarkerEpoch>`。MarkerEpoch 每次所有权变更加一，同 sid 崩溃重启时不变。
- **三个按值比较的脚本**：登记——键不存在或已存代际不比本次新才 `SET … PX`（代际按十进制字符串比较，不经 Lua number），迟到的旧 owner 盖不掉新 owner；续期——值完全相同才 `PEXPIRE`，否则答“不同”或“不存在”，不重建键；删除——值相同才 `DEL`。
- **登记时机**：独占写准入成功（冷加载后的第一次写、首写认领）、`ClaimRemoteOwnership`、路由 RPC 前置检查冷加载之后（`Manager.ServeRoutedRequest`）、起服预加载之后。同一个值只写一次 Redis，之后由续期维持。登记失败只记指标与日志、不影响这次写，实体仍记在本进程的登记表里，续期得到“不存在”时重登。
- **删除时机**：`TransferRemoteOwnership` 进入 Draining 之后（转移被证明没发生时不补登，下一次写准入或前置检查重登）；写准入发现所有权已是别人（fence）；正常停机（`Assembly.Stop` 先停续期循环再按值批量删除，删除失败只记日志、按 TTL 过期）。kill -9 不删，靠 TTL。Remote 实体没有空闲卸载；仅内存卸载后重载（持久拒绝、落后副本）不改所有权，登记保留。
- **续期**：`RouteRenewInterval`（10s）一轮，对本进程登记表里的实体按 `RouteRenewBatch`（1000）一批 pipeline 执行续期脚本，TTL `RouteTTL`（30s）。“不同”不再续；“不存在”先在实体写门下向权威确认所有权仍是本 sid 且独占，再登记。整批失败（Redis 不可用）只计数，下一轮再试。续期循环挂在 Manager 生命周期上（`Assembly.Start` 启动、`Assembly.Stop` 停）。
- **串行化**：所有登记与删除都在实体的写门下做（续期“不存在”分支也先取写门），与所有权转换串行：转移持有写门直到结束，不会出现“转移刚删掉登记、并发的登记把旧代际写回、续期再续下去”。
- **查找**：`coreremote.RouteLookup.LookupOwner`：Redis 有登记就用；没有、读不了或值坏了就读持久所有权记录；都没有答“不存在”。Redis 不可用时每次查找多一次 Mongo 点读，写准入不受影响。owner 进程经写 Manager 的 `GetRemoteOwnership` 读，只读进程（Mirror Mod）经 `coreremote.MongoOwnershipReader` 读同一个 `_remote_entity_meta`。
- **前置检查**：`Manager.ServeRoutedRequest(ctx, id)`：本进程持有登记的实体直接返回本 sid（不做 I/O）；否则在写门下读所有权记录——没有记录答不存在（**不认领**），owner 是别人返回它，owner 是本 sid（独占）就冷加载、登记；owner 是本 sid 而冷加载得到权威确认的“不存在”（**空认领**，见下条）返回本 sid、不登记，删过档返回满足 `ErrAuthorityEntityDeleted` 的错误。wiring 的 `RPCRouteCheck` 把它映射成 RPC 答复（删过档答 `errcode.ErrEntityDeleted`）。
- **空认领**（2026-11-04 联盟服务阶段 5 发现、按维护者推荐 A 修复）：首写认领发生在写准入、事务提交之前；认领它的事务被业务拒绝回滚、或提交前崩溃时，认领不撤销，留下“所有权记录是本 sid、权威里没有聚合”的实体（game-demo：建盟被拒时发出的联盟号）。框架的统一语义是**本实例拥有、业务上还不存在的实体**：前置检查答“由本实例服务”、不加载不登记，请求交给业务——创建类请求（`GetOrCreate`）在写准入里照常首写（同 ID 再建，所有权本来就是本 sid），其余请求由 Nest 冷加载答 `entity_not_found`（Remote 声明目标在写准入里的冷加载与本地声明目标同一条 `entityStateError` 规则）；起服预加载计入 `PreloadReport.Empty`，不算失败、不记 ERROR、不登记。删过档（D10，`_deleted` 墓碑）不是空认领：那个 ID 不可再用，计入 `PreloadReport.Deleted`。没有采用“回滚时撤销认领”：提交前崩溃同样留下空认领，撤销是新的所有权 CAS、结果未知时照样留下，语义统一之后它只是可选的清理。空认领不会被自动清除；它占一条 `_remote_entity_meta`，会被按 owner 枚举列出、随 `TransferRemoteOwnership` 转走（转过去仍是空认领）。回归：`route_test.go` 的 `TestEmptyClaimIsAnOwnedEntityThatDoesNotExist`、nest `remote_commit_test.go` 的 `TestRemoteTargetLoadStateBecomesAClientCode`、`scripts/test-alliance-e2e.sh` 的 `TestAllianceE2ERejectedFoundingAfterRestart`。
- **起服预加载**：`Manager.PreloadOwnedEntities(ctx, kinds)` 经 `{_owner_sid: 1}` 索引（`EnsureRemoteStorage` 建）按 owner 枚举 `_remote_entity_meta`（`MongoCommitter.ListOwnedRemoteEntities`，kind 用 `_id` 对 1024 取模过滤，不改元数据文档格式），只取本 sid 独占的；每个实体在写门下向权威复核后只加载、不写，再登记；并发 16。枚举失败返回错误（启动失败）；单个实体加载失败计数并跳过，留给按需冷加载；空认领与删过档的是确定状态，分别计入 `Empty` / `Deleted`，不算失败。wiring 的 `RemotePreloadMod` 在 `remote_entity`、`nest`（与可选的 `dataengine`）之后同步跑完它，`WithPreloadKinds` 声明 kind，声明了却没装这个 Mod 时配置检查报错。
- **指标**：`remote_entity.route_register_total{result=ok|stale|error}`、`remote_entity.route_renew_total{result=renewed|replaced|missing|error}`、`remote_entity.route_delete_total{result}`、`remote_entity.route_lookup_total{source=redis|authority|not_found|error}`、`remote_entity.route_lookup_fallback_total{reason=redis_missing|redis_error}`、`remote_entity.preload_total{result=ok|skipped|empty|deleted|error}`。
- **按 owner 枚举**：`Manager.ListOwnedRemoteEntities(ctx, kinds, after, limit)`（接口 `entity.RemoteOwnedEntityLister`）与预加载同一个查询，按 ID 升序分页列出本 sid 独占的实体，一页至多 `OwnedListPageLimit`（1000）；结果是查询时刻的快照，逐个实体仍经 `TransferRemoteOwnership` 等入口裁决。运维迁移“本实例持有的 N 个”用它（game-demo `alliance.transfer {count}`），代价与本实例持有数成正比，不随集群实体总数增长。要 I/O，只能在 I/O 段调用。
- **边界**：登记表只增不减（实体不空闲卸载），续期量是“已加载的登记 kind 实体数 / 10s”；死掉的 owner 不拉起同 sid 时登记过期，调用方回退读到的仍是它，请求一直是“可重试”。回归：`route_test.go`（替身存储上的 Manager 行为）、`route_redis_integration_test.go`（真实 Redis 单机与 Cluster 上的三个脚本、TTL 过期、真实 Mongo 上的按 owner 枚举、两个 Manager 之间的转移与 kill -9 端到端）。

## 读取

业务读取 `RemoteSnapshotEnvelope`，不读取远端 live Entity。L1/L2 以 tenant/entity/kind/scope/policy 为完整 key；payload 是 `FrozenRemoteSnapshotPayload`，L1 命中不复制 backing bytes，解码时显式取 copy。相同 epoch/version 的不同 checksum 被拒绝。checksum（CRC64）在 Mongo 中只存 8 字节 Binary 一种形态；旧版本写下的 BSON 数字形态与 null / undefined 不再读取，解码时报 `remote checksum has unexpected BSON type`。

- Cached：允许当前缓存版本。
- Monotonic：要求不低于调用方 minVersion；缓存不满足时不等待推送，按 `(key, After)` 合并回源一次（Mirror 第 2 步起）。
- Linearizable：直接权威存储读取。

Interest 使用 TTL 软状态，本机在半 TTL 前不重复发布 renewal，避免每次 L1 read 产生网络消息。

## 启动约束

RemoteEntityMod 缺少以下任一能力必须启动失败：Redis versioned lock（共享模式的竞争协调；所有权的权威是 Mongo 的 ownership 记录、共享模式的 grant / fence，正式装配不用 Redis marker，见[Remote实现篇](impl/05-remote-mirror.md)）、跨服 SyncBus、`IRemoteAtomicBatchCommitter`、`IRemoteSnapshotLoader`、`IRemoteCommitOutbox`。依赖在 Start 后封存，禁止运行期替换造成 data race。

Broadcast 和匿名 callback 不允许写 remote-managed entity，因为它们不具备一个明确的原子 transaction/rollback 边界。跨实体写使用声明目标的 Multi handler。
