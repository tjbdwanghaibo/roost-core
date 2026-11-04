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
- **边界**：守卫只管本进程，不校验别的进程的配置；归属服务本身仍按首写认领与持久 fence 决定哪个实例是 owner（多实例路由与改投见 [guide/09“路由 RPC 与改投”](guide/09-services.md#2a-路由-rpc-与改投)）。回归：`ownership_test.go` 的 `TestForeignKindIsRejectedBeforeOwnershipClaim`、`wiring/remoteentity` 的 `TestRemoteEntityModForeignKinds`。

## 路由登记

实体服务有多个实例时（[联盟服务方案 2.7](../maintenance/FEATURE-2026-11-04-alliance-service.md#27-分片接管与迁移决定-2)），owner 在 Redis 登记一份带过期的“实体 ID → owner sid + 所有权代际”，调用方的路由 RPC 按它找 owner（[guide/09“路由 RPC 与改投”](guide/09-services.md#2a-路由-rpc-与改投)）。它只是路由提示：写准入永远只看所有权记录与 fence，不读登记。实现：`framework/remoteentity` 的 `route_store.go`（键、值、三个脚本、查找）与 `route.go`（Manager 上的登记、续期、前置检查与预加载）。

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

RemoteEntityMod 缺少以下任一能力必须启动失败：Redis versioned lock（竞争协调；所有权的权威是 Mongo 的 ownership / grant / fence，正式装配不用 Redis marker，见[Remote实现篇](impl/05-remote-mirror.md)）、跨服 SyncBus、`IRemoteAtomicBatchCommitter`、`IRemoteSnapshotLoader`、`IRemoteCommitOutbox`。依赖在 Start 后封存，禁止运行期替换造成 data race。

Broadcast 和匿名 callback 不允许写 remote-managed entity，因为它们不具备一个明确的原子 transaction/rollback 边界。跨实体写使用声明目标的 Multi handler。
