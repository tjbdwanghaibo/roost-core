# 核心：DataEngine 持久化：实现与维护

当前 main 普通 DAO 支持 setter 路径驱动的嵌套 struct/map/list 增量持久化，使用 BSON `$set`/`$unset`；完整替换边界、事务独立性与验证见[递归增量持久化](../DATAENGINE-INCREMENTAL-PERSISTENCE.md)。Remote 权威信封不在此轮范围内。

适用版本：当前 main 与最新发布版本（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）；源码目录最初按 v1.24.0 提取、之后随改动维护，链接由根包文档门禁保证存在，缺漏以源码为准。[设计与使用](../guide/03-dataengine.md)


致命投影冲突是粘滞终态：每次 `ReplayPass` 在取得回放锁后复查 fatal，覆盖排队期间发生的冲突，Flush 与直接回放共用此边界。fatal 当轮仍等待已启动的并行通道返回并确认连续成功前缀；后续调用返回原始 fatal，不再调用存储。普通临时错误仍可重试，取消不升级为 fatal。

## 如何阅读

进程突然退出后，已确认的游戏数据如何找回来？DataEngine负责把业务修改变成可恢复的持久记录。

第一次接入先读本页顶部的“设计与使用”。准备修改代码时，先看下面的约束与流程，再展开源码目录；测试清单用于找到已有用例，不要求从头读完。

## 1. 实现边界

`framework/dataengine`、`framework/nestwal`、`infra/storage/versionstore`、`framework/cache`、`infra/storage/mongo`、`infra/storage/redis`。下面从同一工作树的源码与测试声明提取，排除 testdata；是可复核的定位索引，不把出现一个名字视为行为已经测试通过。

## 2. 必须保持的契约

1. codec 7 单路径，拒绝旧格式不推进 checkpoint。
2. 所有 DAO schema 校验早于水合发布。
3. 投影依赖、held 与版本 CAS 保持；高版本不是幂等证明。
4. 提交结果未知 fence；成功 WAL 回复不等于 Mongo 已落库。
5. 删过档（全部 DAO 只剩墓碑；Remote 托管 kind 看元数据墓碑）是 `ErrEntityAggregateDeleted`，不满足 not found（D10，只守 GetOrCreate）。直接 Create 不读存储，靠“必须用 id-gen 新 ID”的约束；不能为让重建成功而放宽版本条件、在投影时跳过冲突或覆盖墓碑。

6. outbox 由两样东西驱动：投影器在一个单元把 effect 暂存进 outbox（事务已提交）后同步调用 `ProjectorOptions.OnEffectsStaged`，`Assembly.Start` 把它接到 `OutboxWorker.Kick`（容量 1 的唤醒通道：多次合并、空闲循环先醒、`RunOnce` 期间到达的唤醒留到下一轮，不丢）；`PollInterval` 轮询保留为兜底（重试退避到期、别的进程的 effect）。唤醒只是提示，不改投影 / checkpoint 的顺序与持久语义；outbox 先于 Projector 构造，唤醒在回放开始前接好。回归：`TestProjectedEffectWakesTheOutboxWithoutWaitingForThePoll`（正式装配、轮询 1 小时）、`TestOutboxKickWakesALoopCoalescesAndStopsCleanly`。

6A. **一批 effect 的认领与确认各是常数次往返**（2026-11-05 联盟 Join 加压时发现：原先每个候选一次 `FindOneAndUpdate`、每条一次 `DeleteOne`，多个认领循环在同一批候选上逐条撞车，2 个与 8 个 worker 的发布速率都约 40 条/s，远低于暂存速率）。`MongoOutboxStore.Claim` 固定三次往返：按 `available_at, _id` 查候选 → 一次条件 `UpdateMany`（条件仍是“到期且无有效租约”，服务端逐文档原子判定）写租约、推进 `lease_token` 并打上本次认领的随机标记 `lease_claim` → 按候选 `_id` 与标记读回；两次查询之间被别人认领、被 Nack 推迟或已删除的候选不被改写，读回只认本次标记，租约语义与逐条 CAS 相同。`OutboxWorker.RunOnce` 一批内仍按认领顺序逐条同步投递（等 PubAck）；存储实现可选的 `OutboxBatchAcker` 时，投递成功的那些在批尾一次 `AckMany`（一次 `BulkWrite`，每条仍按 `_id` + owner + token 删除，租约已不属于自己的那条不删并报 `ErrOutboxLeaseConflict`），否则逐条 `Ack`。推迟确认只拉长“已投递、未删除”的窗口（远短于租约），其间崩溃的重投由代理按 effect ID 去重，与逐条确认之间崩溃的结果相同。同机 game-demo 联盟 Join：outbox 发布约 30～70 → 220～310 条/s（[benchmarks/alliance-join-1000-20261105](../../performance/benchmarks/alliance-join-1000-20261105/README.md)）。回归：`TestMongoOutboxStoreClaimsABatchInConstantRoundTrips`（修前 62 条 63 次往返）、`TestMongoOutboxStoreAckManyDeletesByLeaseInOneRoundTrip`、`TestOutboxWorkerAcknowledgesABatchInOneRoundTrip`（修前 16 次 `DeleteOne`）。

7. checkpoint 只决定崩溃后从哪里重放（`WAL.Ack` 写 checkpoint 槽并清理已确认段），进程内外没有读者依赖它：投影完成通知、票据、实体屏障在投影成功时发出，saga 等回执读 Mongo，`Flush` / `Shutdown` 每轮 `ReplayPass` 退出前确认已成功前缀。所以 `ReplayPass` 对批次、多 mutation 记录和带 effect / 回执 / Remote 写的特殊记录一律按 `CheckpointRecords`（缺省 256）/ `CheckpointInterval`（缺省 20 ms，维护者 2026-10-19 决定保持；特殊记录逐条投影约 16 ms，积压时合并系数约 2，调大可提高系数，代价是重放窗口与未确认额度变长）/ 轮末合并确认（2026-10-18 起特殊记录也合并，[方案](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/CONTRACT-2026-10-18-projector-checkpoint-merge.md)）。前提是 Store 实现 `BatchProjectionStore` 并保证这些记录在后续版本写入后仍能重放：`MongoStore.Project` 在事务里**先**查事务标记，标记与业务写、回执、outbox、Remote 提交同事务，重放返回首投结论（已应用），不重复暂存。例外只有两种，仍逐单元立即确认：单 mutation 快路记录（`projectsOnFastPath`，不写标记，只靠 `_last_tx`，与 `MongoStore.Project` 选快路用同一判定）和只实现 `ProjectionStore` 的 Store。崩溃后重放的已投影记录 ≤ `CheckpointRecords` 条且只来自约一个间隔加一个单元的窗口；不得为了少重放而把确认提前到投影成功之前，也不得跳过 ack 失败后的后续段。回归：`TestSpecialRecordsShareCheckpointThresholds`、`TestFastPathRecordStillConfirmsPrecedingSpecialRecords`、`TestSpecialRecordCheckpointLossReplaysTheWholeUnconfirmedPrefix`、真实 Mongo 的 `TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint`（[projector_replay_integration_test.go](../../../framework/dataengine/engine/projector_replay_integration_test.go)：带回执与 effect 的特殊记录已在 Mongo 提交、进程在 checkpoint 落盘前退出，重启后重放返回首投结论，业务值、回执、事务标记恰好一次，effect 不重新暂存）；saga 级的崩溃恢复由 `framework/saga` 的 `TestRealJetStreamParticipantKill9`（[step_participant_real_integration_test.go](../../../framework/saga/step_participant_real_integration_test.go)）覆盖：参与方（真实 Nest + nestwal WAL + DataEngine 投影）在 Nest 事务、未发出的结果与未投影的 WAL 都在途时被 kill -9，同一 WAL 重启后全部 saga 到终态，Mongo 逐账户余额等于按终态算出的预期（任何操作生效两次或丢失都对不上）。它验证的是端到端恰好一次，不单独构造“checkpoint 丢失”的时刻。

8. DAO 库名映射（D9 修订版 A）：`DocumentKey.Database` / `RemoteDataMutation.Database` 是逻辑库名，WAL 只记逻辑名；物理库名只经启动时冻结的 [DatabaseMap](../../../framework/dataengine/databases.go)（`NewDatabaseMap`：每个逻辑库只有一个来源——配置或回调非空结果，都没有时 identity；回调对配置已写的逻辑库只能返回空串或同一个值，不同即拒绝启动，不得改回“回调覆盖配置”；校验未声明逻辑名、物理名合法性 / 系统库 / `_<sid>` 后 63 字节、物理库唯一、回调与配置冲突、回调错误；冲突只在 Init 判断，`ValidateDatabaseConfig`（`--check-config` / doctor）不调用回调）。解析点：[mongo_store.go](../../../framework/dataengine/engine/mongo_store.go) `physicalDatabase`（单条投影、批量分组、冷加载与墓碑都经 `collection` 走到它，是 DataEngine 侧的唯一强制点：带库名而 `MongoStoreConfig.Databases` 为 nil 时返回 `dataengine.ErrDatabaseMapNotBound`，在访问任何 DAO 集合前拒绝；空库名取默认库、不需要表）、[mongo_committer.go](../../../framework/remoteentity/mongo_committer.go) `dataDB`（`BindDatabaseMap` 一次绑定；`boundDatabaseMap` 是 Remote 侧的唯一强制点：任何构造方式下，提交里有带库名的 DAO 写入或删除而表未绑定时返回包着同一个哨兵的错误，在写任何 DAO 文档前拒绝、整个 Mongo 事务回滚；只写控制库的提交不需要表）。两边规则相同（维护者 2026-10-21 决定收掉“DataEngine nil 表按 identity 投影”的不对称）：不得恢复任何一侧“没有表时按逻辑名访问库”的回退；有表、表里没有某个逻辑库时仍按 `DatabaseMap.Physical` 的 identity 规则。哨兵与绑定接口 `DatabaseMapBinder` 定义在 [framework/dataengine/databases.go](../../../framework/dataengine/databases.go)。强制时机：`NewMongoStore` 在 `Databases` 为 nil 而 `DeclaredDatabases()` 非空时构造即拒绝（规则在装配时强制一次；记录进 WAL 后才在投影上失败只能卡住投影器），访问时的检查兜底没有声明的进程。绑定只有一个入口：`MongoStore.SetRemoteProjection`（`Assemble` 调用）在 Remote 存储实现 `DatabaseMapBinder` 且本存储有表时先绑表，失败（已绑另一张）则不接上投影并返回错误；本存储没有表时不绑，两边都拒绝。wiring Mod 不再单独绑定。[wiring/dataengine/databases.go](../../../wiring/dataengine/databases.go)（Mod Init 合成、交给 `Assemble` 并发布 `dataengine.databases` 能力、工具入口 `LoadDatabaseMapFile` / `DAOCollection`）。控制库（DataEngine 自己的 `dataengine.database`、Remote 的 `remote_entity.mongo.database`、saga 库）是配置给出的物理名，不经映射。[Assembly.Start](../../../framework/dataengine/engine/assembly.go) 在打开 WAL 之后、构造 Projector（构造即回放）之前调 [reconcileDatabaseMapping](../../../framework/dataengine/engine/databases.go)：读 WAL 目录的 `databases.json`（[nestwal/databases.go](../../../framework/nestwal/databases.go)），`HasUnacknowledgedRecords` 为真且上次映射里任一逻辑库换算结果变了（文件不存在按 identity 比较）返回 `ErrDatabaseMappingChanged`，否则原子写入本次映射。不得在投影前把逻辑名换成物理名写进 WAL（Remote 提交的 digest 覆盖库名，改写会让发布路径的重放校验失败），也不得在运行中调用回调。回归：`TestDatabaseMapPriorityAndResolverCalledOncePerLogicalDatabase`、`TestDatabaseMapRefusesResolverConflictingWithConfig`、`TestDatabaseMapRefusesEachInvalidMapping`、`TestMongoStoreResolvesLogicalDatabasesOnEveryDocumentPath`、`TestMongoStoreWithoutDatabaseMapRefusesLogicalDatabases`、`TestReconcileDatabaseMapping*`、`TestMongoCommitterWritesDAODocumentsIntoTheMappedDatabase`、`TestSetRemoteProjectionBindsTheDataEngineDatabaseMap`、`TestInspectReportsTheRecordedDatabaseMapping`、wiring 的 `TestDataEngineModProjectsLogicalDatabaseIntoMappedPhysicalDatabase` / `TestDataEngineModFreezesTheMappingAtInitAndRefusesInvalidConfig` / `TestDataEngineModBindsTheFrozenMapToRemoteStorageAndPublishesIt` / `TestSelfAssembledMongoStoreRequiresAMapWhenDatabasesAreDeclared` / `TestToolsReadTheSameDatabaseMapping`、根包 `TestNoMongoDatabaseOpenedWithALogicalDAOName`、真实依赖 `TestSagaNativeStepsAndEffectRealDependencies`（逻辑库 main 映射到唯一物理库）。

9. 运维卸载单个实体（`entity.unload`，D16，[entity_unload.go](../../../framework/dataengine/engine/entity_unload.go)）的顺序不能调换：①在业务池经 `ManagerAccess.WithLocked` 取实体锁并确认仍是当前托管实例（等持有它的事务结束，受 ctx 约束；它的准入在锁内，结束时要么已登记在途投影、要么已回滚；DataEngine 不直接 import `infra/base/lock`，三大块依赖方向见根包 `TestCorePillarDependencyDirection`），在 `heldMu` 下登记屏障（`Projector.beginEntityUnload`，锁序与锁内准入相同：实体锁 → heldMu）；②命令 goroutine 上 `WaitEntityProjection`；③业务池 `ManagerAccess.Unload`；④defer 撤销屏障。屏障在 `reserve` 的同一临界区检查（`checkUnloadingEntitiesLocked`），所以登记之后写到该实体的记录一定在 WAL 之前以 `coredata.ErrEntityUnloading` 拒绝、Nest 回滚——这是“②之后内存 == 库”一直成立的唯一原因；不能改成先登记屏障再等锁（持有实体的在途事务会被拒绝），也不能省掉①的锁或②的等待（回归分别在 `TestEntityUnloadWaitsForOrRefusesATransactionHoldingTheEntity` 与 `TestEntityUnloadDrainsProjectionsRefusesWritesThenColdLoadsTheSameState` 里变红，变异记录见 DESIGN-TODO D16 实施结果）。失败（取锁超时 / 另一次卸载 / Remote 所有权不稳定 → `ErrEntityUnloadBusy`；投影超时 → `ErrEntityUnloadTimeout`；`AutoPersist=false` → `ErrEntityUnloadNotPersistent`）都不卸载、撤销屏障，错误不包裹 ctx 错误（admin 审计会把它记成 unknown，而结论是确定的）。`UnloadEntity` 在业务 worker 上直接返回 `fctx.ErrBlockingInBusinessWorker`。admin 命令在 [admin.go](../../../framework/dataengine/engine/admin.go)（`RegisterAdminCommands`，wiring/dataengine Mod 在 Provide 时注册）；`entity.kick` 在 [gateway/admin.go](../../../infra/network/gateway/admin.go)（`PlayerRuntime.CloseSessions`，wiring/gate `RegisterPlayerAdminCommands`：GameIngressMod 与生成的嵌入 TCP Mod 在 Provide 时调用）。

10. 投影写法（[mongo_store.go](../../../framework/dataengine/engine/mongo_store.go) `applyPut`、[mongo_projection.go](../../../framework/dataengine/engine/mongo_projection.go) `batchMutationModel`）：Put 是整文档替换。单条路径（快路与事务内）是 `FindOneAndUpdate(版本条件, [{$replaceWith: {$literal: doc}}], upsert, 返回新文档)`，批量路径是 `ReplaceOne` upsert 模型；同一条 mutation 经两条路径存下的文档必须相同，都等于 WAL 里的 `Data` 加上 `_version` / `_schema` / `_last_tx`。`$replaceWith` 的参数是聚合表达式，**必须**包 `$literal`：不包时以 `$` 开头的业务字符串被当成字段路径或变量求值——`"$name"` 让字段消失或取到旧文档的值，`"$$ROOT"` 变成整份旧文档，单独的 `"$"`、未定义变量 `"$$foo"` 让投影报错并一直重试（v1.12.2～v1.31.1 曾如此，见 CHANGELOG）。Patch 与删档墓碑是普通 `$set` / `$unset` 更新文档，值按字面量写入，不受影响；saga 冷副本的门控替换同样包 `$literal`（[cold_copy.go](../../../framework/saga/cold_copy.go) `coldCopyUpsert`）。今后在管道更新或聚合表达式里放业务数据，同样要包 `$literal`。没有改用 `FindOneAndReplace`：版本过滤、upsert、返回新文档与 `classifyNoMatch` 都能保持，但 `ICollection.FindOneAndReplace` 没有 upsert 选项，要扩接口，收益只是不依赖管道更新（4.2 起支持，下限见设计篇“DataEngine 对 MongoDB 的最低要求”）。替身 [mongotest.go](../../../infra/storage/mongo/mongotest/mongotest.go) `applyPipeline` 按服务器规则对 `$replaceWith` 的参数求值（字段路径、`$$ROOT` / `$$CURRENT`、`$literal`，其余运算符报 `ErrUnsupported`），不得退回“参数当字面量存”，否则单元测试看不见这类缺陷。回归：`TestMongoStorePutStoresDollarStringsVerbatim`（替身）、`TestRealMongoPutStoresDollarStringsVerbatim`（[mongo_store_integration_test.go](../../../framework/dataengine/engine/mongo_store_integration_test.go)，真实副本集，`-tags integration`）、`TestReplaceWithEvaluatesExpressionsLikeTheServer`（替身自身）、saga `TestRealMongoColdCopyVersionGate`（`last_error` 以 `$` 开头）。

11. Mongo 部署前提分两处检查（维护者 2026-10-30 决定不在启动时强制版本检查）：Mongo Mod 启动只经 [driver/client.go](../../../infra/storage/mongo/driver/client.go) `ValidateDeployment` 拒绝单机（`mongo.require_replica_set` 为 `true` 时）与不支持会话的服务端；版本、FCV、`transactionLifetimeLimitSeconds`、存储引擎由运维工具 [cmd/mongocheck](../../../cmd/mongocheck/check.go) 只读检查（`Evaluate`，判定与[设计篇“最低要求”](../guide/03-dataengine.md#dataengine-对-mongodb-的最低要求)一一对应，拓扑判定与 `ValidateDeployment` 同一规则）。工具的连接串只从环境变量读，驱动错误经 `describeError` 只输出服务端错误名 / 错误码或类别，不原样输出（驱动错误文本可能含主机与连接串片段）；单测 `cmd/mongocheck/check_test.go`（判定边界、错误脱敏、连接串不从命令行读）。改判定规则时同步设计篇的表。

12. **新建实体本身就落库并写齐每个 DAO；冷加载把缺的 DAO 补空，由下一笔持久提交写出**（R15 2026-10-30 下游端到端冒烟发现：Player 有 5 个 DAO，建号事务只改了 4 个，重启后冷加载报 `entity aggregate is incomplete or corrupt: live=4 missing=1`；同日维护者两条决定：缺文档的 DAO 补空——上线后给已有 kind 新增 DAO 是常态；没改持久字段的新建实体也要落库）。**一条 Nest 规则**：[persist_change.go](../../../framework/nest/persist_change.go) `includeUnpersistedDAOs`——事务第一次登记某个 DAO 时，把同一实体里版本 0（还没有持久文档）、尚未登记的 DAO 以 `AllFields` 登记，版本 0 的 DAO 准备时生成整文档 Put，Remote 托管实体经同一份登记由 `RemotePersistChangeFor` 认领（整份 payload，数据文档是 upsert 的 ReplaceOne）。聚合由 [entity_factory.go](../../../framework/entity/entity_factory.go) `BuildEntity` 在 builder 之前（组件 Init / `OnInitFinish` 里的 setter 就可能是第一笔登记）经 `bindDaoAggregate` 绑定：新建与冷加载都绑定，同一个 `entity.DaoAggregate`（按集合名排序）交给每个 DAO 的 tracker（`DaoInterface.Dirty()` 实现 `entity.AggregateMember`；生成 DAO 是 [dataengine.Tracker](../../../framework/dataengine/tracker.go)）。确认每个 DAO 都有文档后 `MarkAllPersisted`，此后 `Tracker.Aggregate()` 为 nil，热路径只多一次原子读（版本只在提交被接受时前进、不会退回 0，标记只置一次）。版本 0 的 DAO 有两种来源：①**新建**：[rollback.go](../../../framework/nest/rollback.go) `CaptureCreatedEntity` 捕获新实体时调 `includeCreatedEntity`，创建它的事务没改任何字段也写齐全部 DAO。**失败的新建不留下 DAO 写**：builder 之前绑定聚合，组件 Init / `OnInitFinish` 里的 setter 已在当前事务登记；新建在被捕获之前失败（构建、取锁、TryAdd 发布）时 [manager_factory.go](../../../framework/entity/manager_factory.go) `abandonCreated` 经 `CreatedEntityCapturer.BeginCreatedEntityBuild` 取得的入口，由 `RollbackTx.abandonCreatedEntityBuild` 撤销构建期间以这些 DAO 登记的修改，业务吞掉错误时事务照常提交其余修改、库里没有失败实体；构建还留下了自身 DAO 以外的持久登记时不能逐项撤销，带 `nest.ErrFailedCreateLeftEffects` 整条回滚（准入门路径本来就把错误上抛、整条回滚）。谁捕获按入口分（[manager_factory.go](../../../framework/entity/manager_factory.go) `Create`）：Guard 上绑定了 `CreatedEntityCapturer`（Nest handler 内，含 handler 里的 detached / isolated 事务）→ 该事务，回滚撤销发布、不落库；没有捕获者、kind 落库且不是 Remote 托管 → `EntityManager` 注册的 `CreateAdmitter`（[entity_create.go](../../../framework/dataengine/engine/entity_create.go) `entityCreateAdmitter`，`Runtime.Start` 与加载器、删除门一起注册，停机时注销）：开 Guard 作用域、`RunIsolatedTransaction`（strict）把事务绑成捕获者再 `CreateInScope`，提交被接受后才返回、才放锁；会等 WAL，业务 worker 上 `fctx.BlockingError` 拒绝，结果未知 fail-stop（同删除准入）。装了准入门时在没有捕获者的作用域上直接 `CreateInScope` 持久 kind 返回 `entity.ErrPersistentCreateOutsideTransaction`，不会悄悄只发布到内存。memory 快路径 handler（`memoryHandlerCreates`）新建持久实体返回 `ErrMemoryTransactionPersistentWrite` 并撤销发布；有 RollbackTx 的 memory 事务照 `refuseMemoryPersistentWrite` 在提交点整条拒绝。②**冷加载补空**：[entity_repository.go](../../../framework/dataengine/engine/entity_repository.go) `readAggregate` 在一次一致性读里分类——全部缺失 not found（Remote 先查元数据墓碑）；没有活文档、有墓碑 deleted（删档后才加的 DAO 没有墓碑，不能把删过档的实体当部分存在复活）；活文档与墓碑混在一起 corrupt；至少一个活文档、其余缺失 → 缺的 DAO 用 builder 给出的初值（`filled`，版本 0，不校验 schema、不水合），Remote 托管实体此时还查元数据墓碑，删过档按 corrupt 拒绝。`reportFilledDAOs` 每个补上的 DAO 计 `dataengine.aggregate.dao_filled.total{kind,collection}` 并打一条 WARN（kind、实体 ID、集合名）。读路径不写库：补上的 DAO 随这个实体的下一笔持久提交写出（没有下一笔就一直没有文档，下次加载再补、再计数）。整文档登记的掩码是 `AllFields`（最高位为 1），Remote 事务记录按位另存掩码（[mongo_committer.go](../../../framework/remoteentity/mongo_committer.go) `storeRemoteCommits` / `loadRemoteCommits`，回归 `TestMongoCommitterStoresEveryMutationMaskBitForBit`）：原样存 uint64 时 BSON 报 `overflows int64`，记录已持久而投影永远失败。**取舍**：补空选“下次持久提交时一起写”而不是加载后立即补写——读路径产生写要在 I/O 阶段起事务、与卸载 / 重载 / 投影屏障交错，而补上的初值与“没有文档”在下次加载时等价，延后写不丢信息；丢写 / 误删被静默补空的风险由 WARN 与计数器暴露（新增 DAO 时每个存量实体一次，其余时候应为零）。事务外新建选“框架补一笔持久事务”而不是“要求必须在事务内创建、事务外报错”——生成的登录建号与服务 Init 的 `EnsureWorld` 都在 Nest 之外 `GetOrCreate`，报错会把这两条正式路径变成编译通过、运行失败；代价是这类调用多一次 strict WAL 等待，每次新建一条独立 WAL 记录（之后的修改是第二笔），业务 worker 上不能调。**边界**：handler 之外的 `RunDetachedTransaction` 里新建，新建那笔事务独立提交，外层随后回滚不撤销新实体（外层没有能把新实体锁持到提交的 Guard 作用域）；Remote 托管 kind 在 Nest 之外新建不经准入门（Remote 持久写只能走消息声明的批次），照旧在第一笔 Remote 批次提交时写齐；handler 内新建的 Remote 托管实体经 `CaptureEntities` 的 `remoteBatchCovers` 判断：不在本消息批次里时返回 `ErrDurableRemoteWriteUnsupported`，捕获失败令事务整条回滚、撤销发布（回归 `TestCreateInHandlerRefusesARemoteEntityTheMessageDidNotDeclare`）；没有 DataEngine（没注册准入门）时事务外新建只在内存；tracker 不实现 `AggregateMember` 的手写 DAO（自己用 `AddMutation` 落库）不参与，由写它的代码保证；`noPersist` 实体不绑定。回归：`TestNewEntityIsStoredWithEveryDAOByItsCreatingTransaction`、`TestColdLoadFillsADAOAddedAfterTheEntityWasStored`（[entity_repository_test.go](../../../framework/dataengine/engine/entity_repository_test.go)：正式 Nest + 文件 WAL + Mongo 替身，handler 内改 / 不改、handler 外 detached、事务外、回滚、memory handler、初始化写了 DAO 后失败（handler 处理错误、事务外经准入门）各一例，旧版本 kind 写入后升级重启补空并由下一笔提交写出、删过档不复活，用全新的仓库冷加载）、`TestEntityRepositoryFillsMissingDAOsOfAStoredAggregate`（四分类与 Remote 元数据墓碑）、`TestNewEntityFirstPersistentCommitWritesEveryDAO` / `TestColdLoadedEntityNextCommitWritesTheFilledDAO` / `TestCapturedNewEntityWritesEveryDAOWithoutAnyChange` / `TestMemoryHandlerRefusesToCreateAPersistentEntity` / `TestNewEntityAggregateRegistrationRollsBackWithTheTransaction` / `TestNewRemoteEntityFirstCommitClaimsEveryDAO`（[persist_change_test.go](../../../framework/nest/persist_change_test.go)）、`TestBuildEntityBindsTheDAOAggregateForPersistentEntities` / `TestEntityManagerCreateOutsideATransactionGoesThroughTheCreateAdmitter`（[entity_factory_test.go](../../../framework/entity/entity_factory_test.go)）、真实 Mongo 的生成工程用例 `TestGeneratedDataEngineCreatedEntityWritesEveryDAOAndReloads`（handler 内只改 wallet / 不改、事务外新建，async / strict / pipelined，重启后冷加载）与 `TestGeneratedDataEngineColdLoadFillsADAOAddedLater`。

13. 交出实体（`Runtime.HandOffEntity`，[entity_handoff.go](../../../framework/dataengine/engine/entity_handoff.go)；用法见[设计篇“交出实体”](../guide/03-dataengine.md#交出实体handoffentity)）建立在第 9 条的卸载屏障上，只加入口，不改 `UnloadEntity` 与投影路径。顺序不能调换：①进行中屏障——与运维卸载同一张表（`beginEntityUnload`），常驻时经 `raiseUnloadBarrier` 持实体锁登记，不常驻时直接登记；这一段的写入拿到可重试的 `coredata.ErrEntityUnloading`，交出、`Release`、运维卸载在同一实体上靠它互斥（后来者 `ErrEntityUnloadBusy`）；②`WaitEntityProjection` 排空；③`waitProjectedRecordsConfirmed` 等 checkpoint；④业务池卸载此刻常驻的实例；⑤`holdEntityHandOff` 在 `heldMu` 下登记交出屏障（`Projector.handedOffEntities`），然后才撤进行中屏障——两张表有重叠，准入处没有空档。`reserve` 在同一临界区先查卸载屏障、再查交出屏障（`checkHandedOffEntitiesLocked`，没有交出时只是一次空表判断），所以进行中的实体先拿到可重试错误，成立之后才是 `ErrEntityHandedOff`。**为什么有第③步**：第 7 条说 checkpoint 之外没有读者依赖它，这在“只有本进程写这份文档”时成立；交出之后有了第二个写者，快路记录“已投影、checkpoint 未确认”时被它推进文档，重放就是 fatal（`classifyNoMatch` 比较 `_version` 与 `_last_tx`）。刻画用例 `TestUnconfirmedProjectionReplayedAfterAnotherWriterAdvancedTheDocument`（替身）与 `TestRealMongoUnconfirmedProjectionReplayedAfterAnotherWriterAdvancedTheDocument`（真实副本集）：同一进程的下一轮回放、重启后的启动排空都 fatal；没有别的写者时重放幂等；写事务标记的记录重放安全。并行窗口里别的通道失败时，快路记录可以长时间停在“已投影、未确认”，不只是崩溃窗口。**第③步怎么等**：投影完成后投影器不再记录事务属于哪个实体，所以取“此刻已投影、未确认”的全部事务（`admitted` 里有、`pendingTransactions` 里没有；两张表都在 `heldMu` 下，后者是前者的子集），轮询（2 ms）到它们都离开 `admitted`（`acknowledge` 只在 `WAL.Ack` 成功后移出）。调用方持有屏障并已排空，所以该实体的记录此刻要么已确认、要么在这个集合里；集合以一轮回放读取的记录数为限（`replayReadRecords`），与积压无关；checkpoint 按 WAL 顺序前进，多等的只是本来就排在前面或同一轮里的记录。只读登记，不驱动回放、不改 checkpoint 时机、不在 checkpoint 路径上加通知。轮询里只查 fatal / WAL terminal / 关闭（`confirmationStopped`），不查 `WAL.Healthy`（读目录）。**`Release` 不只是撤屏障**：交出不拦加载（`unload_resync`、任何访问都能把实体读回来），期间的实例只读且可能落后于库；`Release` 在进行中屏障下等该实体在途的冷加载落定（`EntityRepository.awaitLoadFlight`）、在业务池卸载常驻实例（`unloadResident`，卸载不掉返回 `ErrEntityUnloadBusy`、屏障保持），再按令牌撤交出屏障（旧返回值的令牌对不上，不会解除之后的另一次交出）。重复交出走同一流程：再卸载一次，`holdEntityHandOff` 返回原令牌。屏障只在内存里：重启后的归属由调用方的权威回答。Remote 托管实体拒绝（`ErrEntityHandOffUnsupported`）：保持中的屏障会挡住 Remote 自己的收尾记录。回归 [entity_handoff_test.go](../../../framework/dataengine/engine/entity_handoff_test.go)：去掉第③步 `TestEntityHandOffWaitsForTheCheckpointToCoverItsRecords`、`TestEntityHandOffAcrossShutdown` 变红；`Release` 不卸载 `TestEntityHandOffKeepsRefusingWritesUntilReleased`、`TestEntityHandOffWithSubscribersReloadsButKeepsRefusingWrites` 变红；去掉第④步五条变红；不比较令牌 `TestEntityHandOffRepeatedAndReleasedTwice` 变红。
14. 常驻实体（`lifetime=resident`，维护者 2026-11-13；规则在 [framework/entity/resident.go](../../../framework/entity/resident.go)）：把实例移出内存的唯一入口是 `EntityManager.Destroy`（事务内新建被撤销的 `revokeCreated` 除外，那时实体从未提交），`refuseResidentUnload` 在取锁之前对 `deleteFromDB=false` 且实例 `Lifetime()` 为 resident、ctx 没有 `entity.WithResidentRelease` 许可的调用返回 `entity.ErrEntityResident`（WARN + `entity.resident_unload_refused_total{kind}`）。带许可的只有三处，不得增加：`unloadResident`（交出与 `Release` 的卸载）、`deferEntityDelete` 的准入后收尾、`unloadEntity(allowResident=true)`（`RefreshEntity`）。`UnloadEntity` 在立屏障之前就拒绝常驻实例（不挡写入、不排空）；`RefreshEntity` 卸载后经 `reloadEntity`（`access.Get`，与按需加载同一共享冷加载）载回；常驻 kind 此刻不在内存时直接载回（已交出时返回 `ErrEntityHandedOff`），`Release` 在撤屏障之后对常驻 kind 同样载回，失败记 ERROR 与 `dataengine.resident_reload_total{result=error}`、不让调用失败（Release）或返回错误（Refresh）。admin `entity.unload` 的 `force` 只对常驻 kind 生效（走 `RefreshEntity`）。注册期 `ValidateEntityRegistry` 的 `residentDaoScopeProblems` 要求常驻 kind 的每个 DAO 是 `dbscope=sid`（`entity.ErrResidentDaoScope`）；与 nopersist / Remote 的冲突由 `ValidateEntityPolicy` 拒绝。回归：entity `TestResidentEntityStaysInMemory`、`TestValidateEntityRegistryRejectsAResidentKindWithAGlobalDao`；engine `TestResidentEntityUnloadIsRefusedAndForceRefreshesIt`、`TestResidentEntityIsHandedOffAndReloadedOnRelease`、`TestDataEngineDeleteFinalizesAResidentEntity`；真实副本集 `TestRealMongoResidentEntityRefreshAndReloadAfterAbortedHandOff`（[entity_handoff_integration_test.go](../../../framework/dataengine/engine/entity_handoff_integration_test.go)，`-tags integration`：运维卸载被拒、人工修库后 `RefreshEntity` 载入库里的状态、交出后另一个写者推进文档、`Release` 立刻载回那个版本并接着写；变异“Release 不载回”“Refresh 不载回”均变红）。

## 2A. 聚合恢复与投影实现走读

[EntityRepository](../../../framework/dataengine/engine/entity_repository.go)的Load合并同ID并发flight，等待该实体必要投影后进入loadAggregate。loadAggregate读取builder与完整一致性视图，先遍历全部DAO的Descriptor/SchemaVersion，再进行RestorePersisted与身份校验；只有所有DAO成功后，才RunLocal创建并发布实体及loaded hooks。前一DAO已解码、后一DAO失败时不能提前发布半实体。

readAggregate的读取回调可能因数据库事务重试再次执行，累积结果必须每次重置；flight遇panic也必须结束并唤醒等待者。这些恢复责任不应由调用方临时补sleep实现。

readAggregate用`LoadSpec.IncludeDeleted`读到墓碑（MongoStore默认过滤`_deleted: true`，其他读取路径不变），按“全部缺失→ErrEntityAggregateNotFound、全部墓碑→ErrEntityAggregateDeleted、混合→ErrEntityAggregateCorrupt”分类。Remote托管kind删档时数据文档被物理删除、墓碑只在Remote元数据里，全部缺失时在同一次一致性读里经Store可选的`RemoteEntityDeleted`（MongoStore→remoteentity.Backend→MongoCommitter读`_remote_entity_meta._deleted`）区分删过档。D10。

[codec.go](../../../framework/nestwal/codec.go)的encodeRecord与decodeRecord固定codecVersion=7；unsupported version在解释record body前拒绝。格式校验不等于尾部截断安全，WAL的文件恢复、fsync与checkpoint分别有守卫。

**按实体分片并行投影**（2026-10-27，[设计](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/FEATURE-2026-10-27-projector-entity-parallel.md)）：[projector_parallel.go](../../../framework/dataengine/engine/projector_parallel.go)。`ProjectionWorkers`（`dataengine.projection.workers`，缺省 32，1 = 串行；2026-11-05 由 8 调大：带回执 / effect / Remote 写的记录每条一个 Mongo 事务，吞吐约为“通道数 ÷ 单条事务时延”，8 条通道在本机联盟 Join 约 150 次/秒时封顶，见 [benchmarks/alliance-join-20261105](../../performance/benchmarks/alliance-join-20261105/README.md)）> 1 且 Store 声明 `ParallelProjectionStore` 时，一轮回放最多读 `ReplayBatchRecords × ProjectionWorkers` 条（字节仍受 `ReplayReadBytes`），`planReplayWindows` 按冲突键（文档 `_id`；回执身份 Namespace/ID；effect ID；事务 ID；带 effect 的记录在 `effect_order: wal` 时共享一个顺序类，缺省不共享，见下面“effect 顺序范围”；Store 未声明 Remote 适配器并发安全时 Remote 记录共享一个顺序类）做并查集，分量数 ≥ 2 的段按首条位置把整分量分给记录数最少的通道（至多 `ProjectionWorkers` 条），通道内按 WAL 顺序用 `planProjectionSegments` 合批（普通记录 `ProjectBatch`，特殊记录单条 `Project`）；`Key.ID == 0` 的记录是屏障，独占串行段；整段互相冲突时走原串行路径（快路与立即确认不变）。不变量与强制点：①有交集的记录只在同一通道、按 WAL 顺序（`planParallelWindow`）；②单 mutation 快路不写标记，只给“本窗口里它的全部键都不再出现”的独占单条记录（`fastPath`），其余进 `ProjectBatch` 写标记；逐条退回让一条非末条快路记录成功时该通道就此停下（`truncated`），本轮只确认到前缀；③每条通道第一个单元总会发出，之后任一通道失败即停止补位，已发出的单元等它返回，每个失败单独 `isFatalProjection`；④窗口结束只 ack 连续成功前缀，前缀之后成功的记录靠事务标记在下一轮 / 重启后幂等重放；⑤成功单元的收尾（完成通知 / `OnEffectsStaged` / 计数）与串行路径共用 `finishProjectedUnit`，`OnEffectsStaged` 可能被多个通道并发调用。通道是静态分配（不做逐条补位），一个慢单元只拖住它所在的通道与本窗口的确认。effect 暂存顺序缺省按实体有序（维护者 2026-11-05，见下面“effect 顺序范围”）。回归：[projector_parallel_test.go](../../../framework/dataengine/engine/projector_parallel_test.go)（同实体 / 多实体 / effect 顺序、失败前缀与重启幂等、fatal 停止全部通道、快路条件、逐条退回停通道、workers=1 串行、规划器表驱动）、[projector_remote_test.go](../../../framework/dataengine/engine/projector_remote_test.go)（Remote 记录走同一调度器）、真实副本集 `TestRealMongoParallelProjectionReplaysSuccessfulSuffixAfterRestart`（`-tags integration`）。

**持久键与未知键（DESIGN-TODO D13）**：生成 DAO 的持久文档结构体 `<dao>PersistDoc` 是键的唯一声明（取自定义里必填的 `store` 键）。`RestorePersisted` 在 schema 校验之后、解码之前调用 [StoredKeySet.WarnUnknown](../../../framework/dataengine/stored_keys.go)：逐个读文档元素（不解码值），库里有、结构体没有的键按 DAO + 路径每进程一次 `slog.Warn`，计 `dataengine.dao.unknown_stored_key.total{dao}`，不拒绝加载；顶层 `_` 开头的元数据键与缺失键不报。这些键**不读入**（维护者 2026-10-19 撤销“整文档写回保留旧键”的方案 A）：持久文档结构体没有收未知键的字段，解码器忽略它们，DAO 内存里没有，整文档写回（存储层的 Put：`$replaceWith` / ReplaceOne，以及 Remote data 信封替换）就把它们从文档里去掉；单字段补丁只 `$set` 自己的键，旧键留在库里但不再被读取。版本条件、冲突分类、墓碑语义不变。框架不提供清理入口：改名 / 删字段前保留原 `store` 键或先离线迁移（[SCHEMA-MIGRATION](../SCHEMA-MIGRATION.md) §1），这是业务约束，框架只通知。

### effect 顺序范围

`ProjectorOptions.EffectOrder`（配置 `dataengine.projection.effect_order`，`entity` | `wal`，缺省 `entity`，`enum` 声明在加载配置时拒绝其它取值，`NewProjector` 对空值取 `entity`、对未知值报错）只决定**带 effect 的记录之间**的并行投影冲突键（`projectionConflictKeys` 的 `conflictScope.effectsInWALOrder`）：

- **`entity`（缺省，维护者 2026-11-05）**：带 effect 的记录只靠它本来就有的实体键（文档 `_id`）、回执键、effect ID 键、事务键串行。写同一实体、共享回执、同一 effect ID 的记录仍按 WAL 顺序投影与暂存；写不相干实体的带 effect 记录分进不同通道并行投影，它们进 outbox 的先后（`available_at` 取暂存时刻）可能与 WAL 不同。一个窗口内的乱序范围 ≤ 一轮读到的记录数（`batch_records × workers`）。
- **`wal`（可选，缺省关闭）**：全部带 effect 的记录共享 `effectOrderKey`，彼此按 WAL 顺序逐条投影，effect 暂存顺序与 WAL 相同；与 2026-11-05 之前的唯一行为逐字相同（变异核对 M5 守着它）。只给依赖“不同实体的 effect 按 WAL 顺序暂存”的进程打开。
- 不变：checkpoint 只确认连续成功前缀；前缀之后已投影的记录靠事务标记幂等重放（`ProjectFenced` 先查标记，不重复暂存 effect）；`OnEffectsStaged` 只是唤醒提示。`workers: 1` 时全部串行，两种取值没有区别。

改缺省之前核对过的 outbox 路径（2026-11-05，当前源码；codebase-memory 图谱本轮连不上，结论来自直接读源码与 `rg`）：

| 路径 | 是否假设全局单调 | 放宽后 |
| --- | --- | --- |
| DataEngine effect outbox（[outbox_store.go](../../../framework/dataengine/engine/outbox_store.go)） | 否。没有序号与游标：每次 `Claim` 从头按 `available_at, _id` 查到期且无有效租约的文档（2026-11-05 起一批三次往返，见第 6A 条），`Ack` / `AckMany` 删除文档，`Nack` 改 `available_at` | 晚暂存、`available_at` 较早的文档下一次认领就会被查到，不漏；`_id` = effect ID，重放靠事务标记不重复暂存，发布侧以 effect ID 作 JetStream MsgID 去重 |
| Remote 提交 outbox 恢复扫描（`MongoCommitter.PendingRemoteCommitPage`，游标 `created_at, _id`） | 游标只在一次扫描内用（`IRemoteCommitOutboxPager` 注释），下一次扫描从头开始；`created_at` 是写入时的墙钟，不是 WAL 位置 | 不受 effect 顺序影响：不带 effect 的 Remote 记录早已按实体并行投影；一次扫描里错过的提交在下一次扫描（补发循环的 dirty 重扫、启动恢复）里被看到 |
| outbox 积压采样（`Backlog`：计数 + 最老 `created_at`） | 否 | 只影响 `OldestAge` 的读数先后，不影响正确性 |

核对过的 effect 消费者（仓内全部：framework / wiring / service 内置、game-demo、genre 夹具与生成器骨架）——没有一个依赖“不同实体之间的 effect 按 WAL 顺序”，因此没有任何生成配置打开 `wal`：

| 消费者 | 依据 |
| --- | --- |
| saga 启动中继 `SubscribeNestStarts`（`saga.start`，`framework/saga/nest_start_consumer.go`） | `Workers` 可并发、注释写明不保证回调顺序；`StartSaga` 按 saga 身份幂等、写按版本 CAS |
| 通用订阅 `wiring/dataengine.SubscribeEffects` 与生成器骨架（`roost add effect`） | 共享 durable、`MaxAckPending` 缺省 256 并发在途、失败重投；Mongo inbox 按 EffectID 去重；框架从未承诺 effect 全局顺序（[NEST_TRANSACTION_WAL](../NEST_TRANSACTION_WAL.md) 只承诺至少一次与 `Effect.ID` 去重） |
| mailbox/profile 模块 `mailbox.profile_reward` → 升级邮件 | 每条 effect 独立发一封邮件、按 EffectID 幂等；同一玩家的 effect 同实体，仍按 WAL 顺序 |
| game-demo `activity.phase_due` → `NotifyPhaseDue`（`activity.go`） | 只由 World 实体发出（同实体有序）；inbox 去重，`NotifyPhase` 按 game sid 幂等，迟到 / 重投答“已报告” |
| game-demo alliance 服务 | 2026-11-05 晚起不产生、不消费任何 effect（[联盟服务方案 §2.5B](../../maintenance/FEATURE-2026-11-04-alliance-service.md#25b-联盟不用-effect2026-11-05-晚)）：成员通知、预约期限、跨实例宣战的对方一侧都由服务层在写答复之后直接做 |
| game 侧收联盟通知（`internal/service/game/alliance_notice.go`） | 走总线消息，不是 effect；尽力而为、容忍乱序 |
| genre 夹具（`scripts/testdata/*`）、`service/`、`gameplay/` | 没有 `nest.Emit` / effect 消费者 |

### effect 消费者进程内并发

`wiring/dataengine.SubscribeEffects`（[effects.go](../../../wiring/dataengine/effects.go)）在一个进程里按信封 `Key` 并发处理（维护者 2026-11-05 决定 A；此前 nats.go 单 goroutine 回调里逐条跑完、每条一个 Mongo inbox 事务，本机约 50 条/秒，[benchmarks/alliance-join-1000-20261105](../../performance/benchmarks/alliance-join-1000-20261105/README.md)）。契约：

- **并发度**：`EffectConsumer.Workers`，不写取 `dataengine.effect_consumer.workers`（缺省 16，`min:1 max:1024`，加载配置时校验一次；`Workers` 负数拒绝订阅）。`1` 时在回调里同步处理、不推迟结算，与此前逐条处理逐字相同；传输不支持推迟结算（`DeferSettle == nil`，测试替身）时同样退回逐条。
- **顺序**：同一 `Key` 的 effect 在本进程按到达顺序逐条处理（前一条 handler 返回、结算之后才开始下一条），不同 `Key` 并发，同时在跑的 handler ≤ Workers。`Key` 为空的 effect 各自处理、不与任何 effect 排序——没有实体可排，串成一条会让所有无 Key 的 effect 互相挡住；仓内生产方都给了 Key（下表）。同一进程内不同实体的处理先后不再与流顺序相同；多实例共享 durable 时本来就不成立，nak 退避重投也本来就可能让同一实体的 effect 晚于它后面那条生效。
- **派发**：回调解开信封（解不开照旧 Term）→ 取一个在途名额 → `JetStreamMsg.DeferSettle` 推迟结算 → 交给 [worker.KeyedDispatcher](../../../infra/base/worker/keyed.go)（与总线 RPC 的按顺序键派发同一份实现，[rpc_dispatch.go](../../../infra/network/bus/rpc_dispatch.go)）后返回，nats.go 接着投下一条；派发的任务等一个 worker 名额、跑 inbox 事务、按结果 `settle`。按 Key 排队而不是哈希到固定 worker：handler 会同步调别的服务（当时的联盟成员变化 effect 调守卫所在实例），固定 worker 让一个慢实体挡住同 worker 上的其他实体。
- **准入上界**：已接纳未结算（排队与在跑）的 effect ≤ 消费者的 `MaxAckPending`（缺省 256，与 broker 侧在途上限同一个数）。满了回调停在准入处等一条结算（背压）：这条不确认、不 nak，nats.go 不投后面的；订阅 Stop（handler ctx 取消）时按 ctx 错误 nak 交还 broker。不 nak 而是等，是因为 nak 的那条会在退避后才重投，同一实体后面的 effect 会先被接纳，顺序就乱了。
- **AckWait**：排队期间每 AckWait/2 发一次 in-progress（[fnats.KeepInProgress](../../../infra/network/nats/jetstream.go)，与总线 JetStream RPC 共用）；排队超过 AckWait/10 的开始处理前再发一次，broker 侧的重投期限不早于 handler 期限（AckWait，从开始处理算）减 AckWait/10。handler 开始后不再发心跳：卡住的 handler 照旧在 AckWait 后被 broker 重投，不会被心跳无限续命。
- **不变**：inbox 事务、按 EffectID 去重、至少一次、ok / duplicate / retry / poison 计数与 Term 规则、App 停机 Drain 等全部推迟的结算完成（订阅的 `Closed`）。handler panic 只结束这一条（nak 重投），同 Key 后面的照常处理。
- 回归（[effects_test.go](../../../wiring/dataengine/effects_test.go)，替身按 nats.go 的方式逐条投递并支持 `DeferSettle`）：不同实体并发、同实体按到达顺序、在途上界背压、Workers 上限、Workers=1 逐条（配置与字段两种写法）、Stop 时排队的 nak、panic、排队心跳；修前前四条全红（回调被钉住的 handler 挡住，后面的 effect 5 s 内不结算）。

核对（2026-11-05，当前源码；codebase-memory 图谱连不上，直接读源码与 `rg`）：仓内经 `SubscribeEffects` 的消费者在“同 Key 有序、不同 Key 并发”下都成立，Key 都是实体：

| 消费者 | Key | 依据 |
| --- | --- | --- |
| mailbox/profile 模块 `mailbox.profile_reward` → 升级邮件 | 玩家 ID | 每条独立发一封邮件、`RequestID` = EffectID 幂等；同一玩家仍按到达顺序 |
| `roost add effect` 生成的骨架 | 业务传给 `Emit<Topic>(key, …)` 的 key | 骨架注释与 `roost help effect` 写明“同 Key 有序、不同 Key 并发、无 Key 不排序、跨副本不保证” |

不经 `SubscribeEffects`、不受影响的：saga 启动中继 `SubscribeNestStarts`（自己的 `Workers`，本来就不保证顺序）；game-demo `activity.phase_due`（`nestwal.SubscribeJetStreamEffects` 直接订阅，仍在回调里逐条）。没有发现依赖跨实体顺序的消费者。

### effect 持久性（reliable / best_effort）与消费顺序（entity / none）

维护者 2026-11-05 方案一：联盟 Join 每次约 22 条 Mongo 命令里约 5 条花在一条只用来发尽力而为通知的可靠 effect 上（outbox 查重 + 插入、认领 / 删除、消费方 inbox 事务）。effect 因此分两个维度：

**持久性由生产方声明**，在 `nest.Effect.Delivery`（= `dataengine.Effect.Delivery`，`EffectReliable` 是零值 / 缺省，`EffectBestEffort`）。放在 Effect 上而不是消费者配置里：`nest.Emit` 是唯一生产入口，回滚、提交被拒、结果未知都在事务里判定，只有生产方知道一次写什么时候“不会再丢”；生成器把它声明成 effect 文件里的 `Delivery<Topic>` 常量，`Emit<Topic>` 带上它，消费者骨架引用同一常量，两端只有一处声明。

- **reliable**：逐字不变——effect 进 CommitRecord、投影事务暂存 outbox、发布到效果流、消费方 Mongo inbox 按 EffectID 去重，至少一次。
- **best_effort**（[rollback.go](../../../framework/nest/rollback.go) `Emit` / `scheduleBestEffort`，[execution.go](../../../framework/nest/execution.go) `commitDurable` / `commitPipelined`）：
  - 不进 CommitRecord（`ValidateCommitRecord` 拒绝记录里的非 reliable effect），没有 outbox、inbox、回执，零额外 Mongo 命令。
  - **发送点 = 这次写的持久点**，与 `AfterCommit` 同一时刻、同一路径：strict 在 WAL fsync 成功、Guard 解锁之后；带 Remote 批次的事务在 Remote 持久结论之后（`remoteWriteBatch.Commit` 等到投影器的权威确认，与答复调用方同一时刻；不挂在本地 WAL fsync 上，因为被 fence 的旧 owner 的 WAL 写可能永远到不了权威）；pipelined 在票据持久（durable LSN 越过）之后。`async` 事务一旦 Emit best_effort 就升为 strict（async 的提交点早于 fsync，按它发出的通知可能对应一条丢掉的写）；`memory` 事务没有持久变更，Commit 成功即发。原则：绝不通知一个可能丢掉的改动。
  - 回滚、提交被拒（`ErrCommitRejected`）、结果未知（indeterminate）都不发：回调只在 Commit 成功的路径上登记。提交器没有发送能力（不实现 `nest.BestEffortEffectPublisher`）时这次提交以 `ErrBestEffortPublisherRequired` 被拒，不静默丢。
  - 发送：[wiring/dataengine](../../../wiring/dataengine/effects_best_effort.go) 的 Mod 实现 `PublishBestEffort`，**只入队**（容量 `dataengine.effects.best_effort_queue`，缺省 8192；回调可能在快池上，不能等），一个后台 goroutine 经 core NATS 发到 `<dataengine.effects.subject_prefix>_best_effort.<topic>`（必须在效果流的 `<prefix>.>` 之外，否则会被 JetStream 存下）。
  - **至多一次**：持久之后、发出之前进程崩溃；停机开始后才入队的（`stopped`；停机前已入队的在停机 ctx 内发完，ctx 先到时剩下的随 NATS 连接关闭而失败）；队列满（`dropped`）；连接断开或发送失败（`failed`）；进程没装 NATS（`unavailable`，每次入队 WARN，不让事务失败）；此刻没有订阅者——都会丢；计数 `dataengine.effects.best_effort_total{result}`。只用于“丢了有别处补”的通知。
- **消费**：`EffectConsumer.Delivery` 与生产方一致。best_effort 订阅是 core NATS 队列订阅（队列组 = `Durable`），没有 inbox、没有重投：handler 出错或 panic 只记日志与 `dataengine.effect_consumer.handled_total{consumer,result="failed"}`，信封解不开计 `poison`；生命周期与 `SubscribeEffects` 相同（登记成 App 输入、`Drain` 等在跑的结束）。`Stop` / `Drain` 先关接纳、再等全部已接纳的回调返回才关闭 `Closed`，`Workers` 为 1（回调在 nats.go 投递 goroutine 上就地处理）时也一样；`Stop` 立即关接纳，之后到达的消息计 `stopped`、不调 handler；`Drain` 排空 nats.go 已缓冲的消息之后才关接纳（RV-20261107-02）。
- **消费顺序 `EffectConsumer.Order`**（两种持久性都适用）：`entity`（缺省）即上一节“同 Key 有序、不同 Key 并发”；`none` 不按 Key 排队（`KeyedDispatcher` 的空键），只受 Workers 上限。全局顺序仍只有生产端 `dataengine.projection.effect_order: wal`，不在消费者选。
- 回归：[transaction_test.go](../../../framework/nest/transaction_test.go) `TestBestEffortEffectIsPublishedOnlyAfterTheCommitIsDurable`（async 升 strict、记录里没有 best_effort）、`TestBestEffortEffectIsNotPublishedWithoutACommit`（handler 失败 / 提交被拒 / 结果未知 / 没有发送能力）；[pipelined_commit_test.go](../../../framework/nest/pipelined_commit_test.go) `TestBestEffortEffectWaitsForThePipelinedTicket`；[effects_test.go](../../../wiring/dataengine/effects_test.go) `TestSubscribeEffectsWithOrderNoneDoesNotQueueByEntity`、`TestSubscribeEffectsBestEffortHasNoInboxAndNoRedelivery`、`TestBestEffortPublisherSendsAfterEnqueueAndDropsWhenFull`、`TestBestEffortSubscriptionClosesAfterAcceptedHandlers`（Stop / Drain × Workers 1 / 2）；[create_in_handler_test.go](../../../framework/nest/create_in_handler_test.go) `TestAFailedCreateDoesNotPublishItsEffects`（失败创建不发 best_effort）。变异核对（发送挪到持久提交之前）：strict / pipelined / 被拒 / 结果未知四条全红。

仓内目前没有 best_effort 生产方：引入它的 game-demo 联盟的 `alliance.member_changed` / `alliance.reservation_due` 已于 2026-11-05 晚随“联盟全部不用 effect”删除（[联盟服务方案 §2.5B](../../maintenance/FEATURE-2026-11-04-alliance-service.md#25b-联盟不用-effect2026-11-05-晚)）。两种模式与上面的契约保留给其他服务；框架回归（`transaction_test.go`、`pipelined_commit_test.go`、`effects_test.go` 与 NATS 集成用例）不依赖 demo。

## 3. 并发、失败与恢复的修改检查

修改前沿正式调用入口确认拥有者、锁/事务作用域、准入点和释放点。明确拒绝、已经接纳、结果未知、持久确认、投影完成分别给出错误与收尾责任。改变公开类型、配置或线格式时，同时更新生成器、调用方与使用篇，避免同一 tag 的实现和文档产生两套契约。

若修复并发问题，用可控 barrier/时钟构造修前失败，不能用 sleep 概率通过代替因果证据。停机测试要检查在途回调和依赖释放；持久测试要检查恢复后数据与重复输入。外部系统的真实故障证据单列。

## 4. 文件、类型与职责定位

<details>
<summary>需要定位代码时，展开源码文件与类型目录</summary>

### cache

12 个实现文件、8 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [atomic_local.go](../../../framework/cache/atomic_local.go) | `ExpiringStore`、`AtomicLocalConfig`、`AtomicLocalStats`、`AtomicLocalStore` |
| [grouped_local.go](../../../framework/cache/grouped_local.go) | `GroupedLocalStore` |
| [layered.go](../../../framework/cache/layered.go) | `LayeredOptions`、`LayeredStore` |
| [local.go](../../../framework/cache/local.go) | `LocalStore`、`LocalStoreOption` |
| [mirror.go](../../../framework/cache/mirror.go) | `ReplicaConfig`、`ReplicaSyncer` |
| [read_through.go](../../../framework/cache/read_through.go) | `Loader`、`ReadThroughOptions`、`ReadThroughStats`、`ReadThroughStore` |
| [redis_hash.go](../../../framework/cache/redis_hash.go) | `RedisHashKey`、`RedisHashKeyFunc`、`RedisJSONHashStore` |
| [redis_json.go](../../../framework/cache/redis_json.go) | `RedisKeyFunc`、`RedisJSONStore` |
| [redis_raw.go](../../../framework/cache/redis_raw.go) | `RedisRawJSONStore`、`RedisRawSortedSetStore` |
| [ref_hmap.go](../../../framework/cache/ref_hmap.go) | `RefHMapKeyStringFunc`、`RefHMapPatcher`、`RefHMapConfig`、`RedisRefHMapStore` |
| [replica_local.go](../../../framework/cache/replica_local.go) | `ReplicaStore`、`ReplicaLocalStore` |
| [store.go](../../../framework/cache/store.go) | `Store`、`KeyFunc`、`StaleFunc`、`ValidateKeyFunc`、`ValidateValueFunc`、`StoreConfig` |

### dataengine

11 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [doc.go](../../../framework/dataengine/doc.go) | 函数/方法或内部实现；见源码 |
| [hook.go](../../../framework/dataengine/hook.go) | `DirtyHook` |
| [load.go](../../../framework/dataengine/load.go) | `RawDocument`、`LoadSpec`、`Store`、`EntityExister`、`LoadTemplate`、`Loader` |
| [mutation_types.go](../../../framework/dataengine/mutation_types.go) | `TransactionID`、`Durability`、`MutationKind`、`DatabaseScope`、`DocumentKey`、`FieldPatch`、`Mutation`、`Effect`、`Receipt`、`CommitRecord`、`SyncFieldMeta` |
| [projection.go](../../../framework/dataengine/projection.go) | `Descriptor`、`ProjectionTicket`、`SystemCommitter` |
| [scope.go](../../../framework/dataengine/scope.go) | 函数/方法或内部实现；见源码 |
| [stored_keys.go](../../../framework/dataengine/stored_keys.go) | `StoredKeySet` |
| [sync_view.go](../../../framework/dataengine/sync_view.go) | 函数/方法或内部实现；见源码 |
| [tracker.go](../../../framework/dataengine/tracker.go) | `Tracker`、`TrackerSnapshot` |
| [validate.go](../../../framework/dataengine/validate.go) | 函数/方法或内部实现；见源码 |

### dataengine/engine

22 个实现文件、21 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [admin.go](../../../framework/dataengine/engine/admin.go) | `UnloadCommand`；`RegisterAdminCommands`（`entity.unload`）、`DefaultEntityUnloadTimeout` |
| [assembly.go](../../../framework/dataengine/engine/assembly.go) | `AssemblyConfig`、`AssemblyDeps`、`Assembly` |
| [doc.go](../../../framework/dataengine/engine/doc.go) | 函数/方法或内部实现；见源码 |
| [entity_delete.go](../../../framework/dataengine/engine/entity_delete.go) | 函数/方法或内部实现；见源码 |
| [entity_handoff.go](../../../framework/dataengine/engine/entity_handoff.go) | `EntityHandOff`（`Release`）、`Runtime.HandOffEntity`、`ErrEntityHandedOff` / `ErrEntityHandOffUnsupported`；交出屏障 `holdEntityHandOff` / `checkHandedOffEntitiesLocked`、等 checkpoint 的 `waitProjectedRecordsConfirmed` |
| [entity_projection.go](../../../framework/dataengine/engine/entity_projection.go) | 函数/方法或内部实现；见源码 |
| [entity_repository.go](../../../framework/dataengine/engine/entity_repository.go) | `RecoveryGate`、`EntityRepository` |
| [entity_unload.go](../../../framework/dataengine/engine/entity_unload.go) | `EntityUnloadResult`、`Runtime.UnloadEntity`、`ErrEntityUnloadBusy` / `ErrEntityUnloadTimeout` / `ErrEntityUnloadNotPersistent`；卸载屏障 `beginEntityUnload` / `checkUnloadingEntitiesLocked` |
| [health.go](../../../framework/dataengine/engine/health.go) | 函数/方法或内部实现；见源码 |
| [mongo_load.go](../../../framework/dataengine/engine/mongo_load.go) | 函数/方法或内部实现；见源码 |
| [mongo_projection.go](../../../framework/dataengine/engine/mongo_projection.go) | 函数/方法或内部实现；见源码 |
| [mongo_store.go](../../../framework/dataengine/engine/mongo_store.go) | `MongoStoreConfig`、`MongoStore`、`RemoteProjectionStore` |
| [operation_gate.go](../../../framework/dataengine/engine/operation_gate.go) | 函数/方法或内部实现；见源码 |
| [outbox_store.go](../../../framework/dataengine/engine/outbox_store.go) | `OutboxLease`、`OutboxItem`、`OutboxBacklog`、`OutboxStore`、`OutboxAck`、`OutboxBatchAcker`、`MongoOutboxStore` |
| [outbox_worker.go](../../../framework/dataengine/engine/outbox_worker.go) | `OutboxPublisher`、`OutboxWorkerOptions`、`OutboxWorkerStats`、`OutboxWorker` |
| [projection_plan.go](../../../framework/dataengine/engine/projection_plan.go) | 函数/方法或内部实现；见源码 |
| [projector.go](../../../framework/dataengine/engine/projector.go) | `ProjectionStore`、`BatchProjectionStore`、`RemoteParallelProjectionStore`、`ProjectorOptions`、`ProjectorStats`、`Projector` |
| [projector_parallel.go](../../../framework/dataengine/engine/projector_parallel.go) | 函数/方法或内部实现；见源码 |
| [projector_replay.go](../../../framework/dataengine/engine/projector_replay.go) | 函数/方法或内部实现；见源码 |
| [runtime.go](../../../framework/dataengine/engine/runtime.go) | `Runtime`、`PipelinedRuntimeConfig` |

### mongo

8 个实现文件、0 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [bulk.go](../../../infra/storage/mongo/bulk.go) | `WriteModel`、`WriteModelType` |
| [client.go](../../../infra/storage/mongo/client.go) | `IMongo` |
| [collection.go](../../../infra/storage/mongo/collection.go) | `ICollection`、`IStreamingCollection`、`FindOption`、`FindOneAndUpdateOption`、`UpdateResult`、`BulkWriteResult` |
| [config.go](../../../infra/storage/mongo/config.go) | `Config` |
| [database.go](../../../infra/storage/mongo/database.go) | `IDatabase` |
| [errors.go](../../../infra/storage/mongo/errors.go) | 函数/方法或内部实现；见源码 |
| [index.go](../../../infra/storage/mongo/index.go) | `IndexConflictPolicy`、`IndexModel` |
| [session.go](../../../infra/storage/mongo/session.go) | `ISession` |

### mongo/driver

4 个实现文件、5 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [client.go](../../../infra/storage/mongo/driver/client.go) | `Client` |
| [collection.go](../../../infra/storage/mongo/driver/collection.go) | `IndexMigrationPolicy` |
| [database.go](../../../infra/storage/mongo/driver/database.go) | 函数/方法或内部实现；见源码 |
| [session.go](../../../infra/storage/mongo/driver/session.go) | 函数/方法或内部实现；见源码 |

### mongo/mongotest

2 个实现文件、4 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [mongotest.go](../../../infra/storage/mongo/mongotest/mongotest.go) | `Client`、`Database`、`Collection` |
| [transaction.go](../../../infra/storage/mongo/mongotest/transaction.go) | 函数/方法或内部实现；见源码 |

### nestwal

11 个实现文件、11 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [checkpoint.go](../../../framework/nestwal/checkpoint.go) | 函数/方法或内部实现；见源码 |
| [codec.go](../../../framework/nestwal/codec.go) | 函数/方法或内部实现；见源码 |
| [dirsync_unix.go](../../../framework/nestwal/dirsync_unix.go) | 函数/方法或内部实现；见源码 |
| [dirsync_windows.go](../../../framework/nestwal/dirsync_windows.go) | 函数/方法或内部实现；见源码 |
| [effect_inbox.go](../../../framework/nestwal/effect_inbox.go) | `EffectHandler`、`MongoEffectInbox`、`EffectInboxOptions`、`JetStreamEffectConsumerConfig` |
| [inspect.go](../../../framework/nestwal/inspect.go) | `InspectOptions`、`CheckpointInspection`、`SegmentInspection`、`Inspection` |
| [inspect_snapshot.go](../../../framework/nestwal/inspect_snapshot.go) | 函数/方法或内部实现；见源码 |
| [effect_envelope.go](../../../framework/nestwal/effect_envelope.go) | `EffectEnvelope` |
| [lock_unix.go](../../../framework/nestwal/lock_unix.go) | 函数/方法或内部实现；见源码 |
| [lock_windows.go](../../../framework/nestwal/lock_windows.go) | 函数/方法或内部实现；见源码 |
| [wal.go](../../../framework/nestwal/wal.go) | `Options`、`Stats`、`WAL` |

### redis

7 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [cas.go](../../../infra/storage/redis/cas.go) | `ScriptRunner`、`CompareAndSetCommand`、`CompareAndSetIndex`、`CompareAndSetResult` |
| [client.go](../../../infra/storage/redis/client.go) | `IRedis`、`ReplicatedEvaler`、`ReplicatedEvalResult`、`ListTrimmer`、`ListRemover`、`Z` |
| [config.go](../../../infra/storage/redis/config.go) | `Config` |
| [errors.go](../../../infra/storage/redis/errors.go) | 函数/方法或内部实现；见源码 |
| [pipeline.go](../../../infra/storage/redis/pipeline.go) | `IPipeline`、`FutureBytes`、`FutureStringMap`、`FutureInt64` |
| [pubsub.go](../../../infra/storage/redis/pubsub.go) | `IPubSub`、`PubSubMessage` |
| [versioned_lock.go](../../../infra/storage/redis/versioned_lock.go) | `IVersionedLock`、`IFencedVersionedLock`、`VersionedLockOptions`、`IVersionedLockFactory` |

### redis/driver

7 个实现文件、6 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assembly.go](../../../infra/storage/redis/driver/assembly.go) | `Assembly` |
| [client.go](../../../infra/storage/redis/driver/client.go) | `Client` |
| [cluster_recovery.go](../../../infra/storage/redis/driver/cluster_recovery.go) | 函数/方法或内部实现；见源码 |
| [pipeline.go](../../../infra/storage/redis/driver/pipeline.go) | 函数/方法或内部实现；见源码 |
| [pubsub.go](../../../infra/storage/redis/driver/pubsub.go) | 函数/方法或内部实现；见源码 |
| [replay.go](../../../infra/storage/redis/driver/replay.go) | 函数/方法或内部实现；见源码 |
| [replicated.go](../../../infra/storage/redis/driver/replicated.go) | 函数/方法或内部实现；见源码 |

### redis/redistest

1 个实现文件、1 个测试文件。测试辅助，不进运行时：写真实 Redis 的用例用它按本测试唯一前缀注册清理，单机与 Cluster（逐主节点 SCAN）都只删键名含该前缀的键，不 FLUSH。CI `service-redis` job 末尾要求 Redis 为空。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [redistest.go](../../../infra/storage/redis/redistest/redistest.go) | `Prefix`、`Cleanup`、`DeleteKeys` |

### versionstore

6 个实现文件、7 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [codec.go](../../../infra/storage/versionstore/codec.go) | `JSONCodec` |
| [memory_store.go](../../../infra/storage/versionstore/memory_store.go) | `MemoryStore` |
| [redis_store.go](../../../infra/storage/versionstore/redis_store.go) | `RedisClient`、`RedisConfig`、`RedisIndex`、`RedisStore` |
| [update_all.go](../../../infra/storage/versionstore/update_all.go) | `MutateAll`、`MultiUpdater`（多键原子更新，service/match 跨分区成局用） |
| [versionstore.go](../../../infra/storage/versionstore/versionstore.go) | `Versioned`、`ConditionalDeleter`、`Mutate`、`Store`、`Codec`、`KeyFunc` |
| [write_token.go](../../../infra/storage/versionstore/write_token.go) | `UnknownOutcomeError` |

</details>

## 5. 回归入口

下列名字由当前测试源码提取，仅证明存在对应回归入口。执行时以 go test 的实际 PASS/FAIL/SKIP 为准；未启用的真实资源测试不能算通过。常用筛选方向：`Test.*Schema`、`Test.*Codec`、`Test.*Ack`、`Test.*Projection`。

<details>
<summary>准备验证改动时，展开测试目录</summary>

### cache

- [admission_test.go](../../../framework/cache/admission_test.go)：`TestCacheAdmissionFatalRemoteSetIsNotDegraded`、`TestCacheAdmissionStaleWriteAcrossStores`、`TestCacheAdmissionBackfillAdmission`、`TestCacheAdmissionRemoteErrorPolicyOnGetAndDelete`、`TestCacheAdmissionLayeredRejectedReadFailures`
- [atomic_local_test.go](../../../framework/cache/atomic_local_test.go)：`TestAtomicLocalStoreVersionTTLAndBounds`、`TestReadThroughStoreCoalescesMisses`、`TestAtomicLocalOrderBoundedUnderOverwrite`、`TestAtomicLocalOrderBoundedUnderDeleteChurn`、`TestAtomicLocalOrderBoundedUnderExpiryChurn`；其余 2 项见文件
- [cache_test.go](../../../framework/cache/cache_test.go)：`TestLocalStoreRejectsStaleVersion`、`TestLocalStoreEvictsLeastRecentlyUsedEntry`、`TestLayeredStoreReadsThroughAfterTTL`、`TestReplicaSyncerAppliesUpdate`、`TestRedisRawJSONStoreUsesRedisString`；其余 5 项见文件
- [layered_test.go](../../../framework/cache/layered_test.go)：`TestLayeredColdKeysHaveBoundedMetadata`、`TestLayeredExpiryEvictionRevalidatesAuthority`、`TestLayeredExpiryChurnAndColdExpiration`、`TestLayeredExpiredLocalCopyDoesNotVetoAuthority`、`TestLayeredExpiredLocalCopyDoesNotVetoAuthorityRealRedis`；其余 5 项见文件
- [read_through_contract_test.go](../../../framework/cache/read_through_contract_test.go)：`TestReadThroughLoaderFillRefusalIsNotReadFailure`、`TestReadThroughRemoteFailurePolicyIsHonouredOnGetAndSet`、`TestReadThroughReturnsACanceledWaitersSlot`
- [ref_hmap_contract_test.go](../../../framework/cache/ref_hmap_contract_test.go)：`TestRefHMapPatchPathRefusesEachShapeItCannotAddress`、`TestRefHMapLayoutRefusesTypesDeeperThanTheLimit`、`TestRedisJSONStoreRefusesStaleWritesAndMissingKeyFunc`、`TestRefHMapPatchKeepsTheWholeRecordAliveRealRedis`、`TestRefHMapGetTreatsAMissingReferencedHashAsMiss`；其余 6 项见文件
- [ref_hmap_test.go](../../../framework/cache/ref_hmap_test.go)：`TestRedisRefHMapStoreStoresNestedStructsAsSameSlotKeyRefs`、`TestRedisRefHMapStoreSetDeletesPreviouslyRegisteredKeys`、`TestRedisRefHMapStoreDeleteUsesRegisteredKeys`、`TestRedisRefHMapStorePatchUpdatesOnlyTargetScalar`、`TestRedisRefHMapStoreTreatsTimeAsScalar`；其余 5 项见文件
- [replica_contract_test.go](../../../framework/cache/replica_contract_test.go)：`TestReplicaDeleteAndUpsertShareVersionOrder`、`TestReplicaRejectsMissingIdentityExtractors`、`TestReplicaLocalStoreRetainsDeleteWatermarkAtCapacity`、`TestReplicaLocalStoreConcurrentDeletesAndWrites`、`TestReplicaPayloadIdentityBeforeStoreMutation`；其余 2 项见文件

### dataengine

- [load_test.go](../../../framework/dataengine/load_test.go)：`TestLoaderRespectsDependenciesAndSkipsLoadedOrDeletedDocuments`、`TestLoaderRejectsUnknownCircularAndStrictCallbackFailures`、`TestLoaderNonStrictSkipIsCountedAndStrictStillFails`
- [model_test.go](../../../framework/dataengine/model_test.go)：`TestCommitRecordEmptyIncludesReceipts`、`TestCloneEffectPreservesAndDetachesHeaders`、`TestResolveDatabaseScopeDefaultsAndUsesTypedScope`、`TestMapPatchPathBuildsOnlySafeNestedPaths`、`TestWaitProjectionHonorsTicketAndContext`；其余 1 项见文件
- [tracker_test.go](../../../framework/dataengine/tracker_test.go)：`TestTrackerAcceptVersionIsConsecutiveAndCompareAndSwap`、`TestTrackerAdvanceVersionAllowsRemoteEntityJumpsAndIsIdempotent`、`TestTrackerPersistenceAcceptDoesNotConsumeSyncDirty`、`TestTrackerSnapshotRestoreCoversVersionAndSyncState`；其余 2 项见文件
- [stored_keys_test.go](../../../framework/dataengine/stored_keys_test.go)：`TestStoredKeySetReportsKeysTheDocumentStructDoesNotDeclare`、`TestStoredKeySetWarnsOncePerKey`；生成 DAO 上的加载期 WARN 与“不读入”在 `codegen/internal/dao/testdata/runtime/roundtrip_test.go`（`TestUnknownStoredKeysWarnOncePerKeyAndAreNotLoaded`），真实 Mongo（补丁不碰旧键、整文档写回去掉、重放幂等、Remote 载荷）在 `stored_keys_mongo_test.go`，由 `codegen/scripts/dao-golden-runtime.sh` 运行
- [validate_test.go](../../../framework/dataengine/validate_test.go)：`TestValidateMutationRefusesEachMalformedShape`、`TestValidateMutationCrossChecksRemoteCommitAgainstHeader`、`TestValidateCommitRecordRefusesEachMalformedPart`、`TestLoaderAndTrackerGuards`

### dataengine/engine

- [admission_reserve_benchmark_test.go](../../../framework/dataengine/engine/admission_reserve_benchmark_test.go)
- [assembly_test.go](../../../framework/dataengine/engine/assembly_test.go)：`TestAssembleRefusesEachMissingDependency`、`TestJetStreamOutboxPublisherRefusesIncompleteEffects`、`TestAssemblyRetriedShutdownHandsOverWALWithoutCheckpointRegression`、`TestAssemblyKeepsTheRuntimeUntilShutdownCompletes`、`TestRuntimeRepeatedStartPreservesReadyAndOutbox`；其余 6 项见文件
- [batch_limits_test.go](../../../framework/dataengine/engine/batch_limits_test.go)：`TestMultiBatchCapabilityAndSpecialBoundaries`、`TestMultiCheckpointThresholdsAndFailurePrefix`、`TestMultiCheckpointLossKeepsEntireSuccessfulPrefix`、`TestMultiCheckpointFlushesBeforeHeldAndOnCancel`、`TestMarkerlessSingleRecordStillCheckpointsImmediately`；其余 4 项见文件
- [entity_delete_test.go](../../../framework/dataengine/engine/entity_delete_test.go)：`TestDataEngineDeleteDefersMemoryRemovalUntilTransactionAdmission`、`TestDataEngineDeleteRollbackLeavesEntityLive`、`TestDataEngineDeleteAdmissionPanicFencesAndStopsServingEntity`、`TestShortWorkerRemoteDeleteIsDefiniteRejection`、`TestDeleteAdmissionOtherPanicStaysIndeterminate`；其余 4 项见文件
- [entity_handoff_test.go](../../../framework/dataengine/engine/entity_handoff_test.go)：`TestUnconfirmedProjectionReplayedAfterAnotherWriterAdvancedTheDocument`（现状刻画）、`TestEntityHandOffKeepsRefusingWritesUntilReleased`、`TestEntityHandOffBarsAnEntityThatIsNotLoaded`、`TestEntityHandOffWaitsForTheCheckpointToCoverItsRecords`、`TestEntityHandOffRepeatedAndReleasedTwice`、`TestEntityHandOffRefusals`、`TestEntityHandOffAndOrdinaryUnload`、`TestEntityHandOffUnderConcurrentWrites`、`TestEntityHandOffAcrossShutdown`、`TestEntityHandOffWithSubscribersReloadsButKeepsRefusingWrites`（正式 Nest + 文件 WAL + Mongo 替身）
- [entity_handoff_integration_test.go](../../../framework/dataengine/engine/entity_handoff_integration_test.go)：`TestRealMongoUnconfirmedProjectionReplayedAfterAnotherWriterAdvancedTheDocument`、`TestRealMongoEntityHandOffThenAnotherWriterThenRelease`（`-tags integration`，真实副本集、后台投影循环）
- [entity_repository_test.go](../../../framework/dataengine/engine/entity_repository_test.go)：`TestEntityRepositorySingleFlightsCompleteAggregate`、`TestEntityRepositoryRecoveryBarrierAndIncompleteAggregate`、`TestEntityRepositoryRejectsTombstone`、`TestEntityRepositorySeparatesAbsentFromDeleted`、`TestEntityRepositoryReadsRemoteTombstoneForAbsentRemoteAggregate`（全部缺失 / 全部墓碑 / Remote 元数据墓碑，D10）、`TestEntityRepositoryPreservesRemoteVersionVector`、`TestEntityRepositoryFillsMissingDAOsOfAStoredAggregate`、`TestNewEntityIsStoredWithEveryDAOByItsCreatingTransaction`、`TestColdLoadFillsADAOAddedAfterTheEntityWasStored`（部分缺失补空、新建实体本身落库，§2 第 12 条）；其余 11 项见文件
- [entity_unload_test.go](../../../framework/dataengine/engine/entity_unload_test.go)：`TestEntityUnloadDrainsProjectionsRefusesWritesThenColdLoadsTheSameState`、`TestEntityUnloadTimesOutWithoutTouchingTheEntity`、`TestEntityUnloadWaitsForOrRefusesATransactionHoldingTheEntity`、`TestEntityUnloadRefusals`、`TestEntityUnloadRemoteManagedEntity`（正式 Nest + 文件 WAL + Mongo 替身）；真实 Mongo 的同一流程是生成工程 `TestGeneratedDataEngineAdminUnload`（`scripts/test-dataengine-generated.sh`），命令注册见 wiring 的 `TestModRegistersEntityUnloadAdminCommand`
- [failure_integration_test.go](../../../framework/dataengine/engine/failure_integration_test.go)：`TestNATSOutageDoesNotBlockProjectionAndBacklogRecoversByEffectID`
- [projector_replay_integration_test.go](../../../framework/dataengine/engine/projector_replay_integration_test.go)：`TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint`（`-tags integration`，真实副本集）
- [mongo_store_integration_test.go](../../../framework/dataengine/engine/mongo_store_integration_test.go)：`TestRealMongoPutStoresDollarStringsVerbatim`（`-tags integration`，真实副本集）
- [mongo_benchmark_test.go](../../../framework/dataengine/engine/mongo_benchmark_test.go)
- [mongo_store_test.go](../../../framework/dataengine/engine/mongo_store_test.go)：`TestMongoStoreUsesAbsoluteExpiryForReceipts`、`TestMongoStorePatchExactVersionAndReplay`、`TestMongoStorePatchRejectsWrongOrMissingBase`、`TestMongoStorePutUpsertsWhenNoBaseVersionExpected`、`TestMongoStoreProjectBatchUsesOneTransactionAndDurableMarkers`、`TestMongoStorePutStoresDollarStringsVerbatim`；其余 23 项见文件
- [outbox_test.go](../../../framework/dataengine/engine/outbox_test.go)：`TestOutboxCloseBeforeStartIsTerminal`、`TestOutboxConcurrentStartClose`、`TestOutboxRetryDelayStartsAfterFailedPublish`、`TestMongoOutboxStoreClaimTakesLeaseByTokenCAS`、`TestMongoOutboxStoreSkipsEffectsNotYetAvailable`、`TestMongoOutboxStoreClaimsABatchInConstantRoundTrips`、`TestMongoOutboxStoreAckManyDeletesByLeaseInOneRoundTrip`、`TestOutboxWorkerAcknowledgesABatchInOneRoundTrip`；其余 14 项见文件（含投影后唤醒的两项）
- [projection_test.go](../../../framework/dataengine/engine/projection_test.go)：`TestPlanProjectionSegmentsPreservesMixedOrder`、`TestBatchProjectionEligibilityMatchesMongoContract`、`TestPlanProjectionSegmentsBoundsOrdinaryBatches`、`TestPlanProjectionSegmentsHonorsMaxRecords`、`TestPlanProjectionSegmentsPreservesEveryRecordAndFence`；其余 17 项见文件
- [projector_benchmark_test.go](../../../framework/dataengine/engine/projector_benchmark_test.go)
- [projector_parallel_test.go](../../../framework/dataengine/engine/projector_parallel_test.go)：`TestEntityParallelIndependentEntityCompletesWhileFirstIsBlocked`、`TestEntityParallelSameEntityRecordsKeepWALOrder`、`TestEntityParallelMultiEntityRecordOrdersOverlappingRecords`、`TestEntityParallelFailedPrefixStopsAckAndRestartReplaysSuffixIdempotently`、`TestEntityParallelFatalConflictFailStopsEveryLane`；其余 7 项见文件
- [projector_remote_test.go](../../../framework/dataengine/engine/projector_remote_test.go)：`TestRemoteProjectionIndependentEntityCompletesWhileFirstBlocked`、`TestRemoteProjectionFailureOnlyAcknowledgesPrefixAndReplaysSuffix`、`TestRemoteProjectionCheckpointFailureKeepsWindow`、`TestRemoteProjectionCancellationJoinsWorkers`、`TestRemoteProjectionFatalSuffixIsNotHiddenByEarlierTransientFailure`；其余 2 项见文件
- [projector_test.go](../../../framework/dataengine/engine/projector_test.go)：`TestProjectorAckNotBlockedByPublisherFailure`、`TestProjectorFatalConflictInvokesFence`、`TestProjectorCommitSystemTicketCompletesAfterProjection`、`TestProjectorUsesAtomicBatchStoreForBacklog`、`TestProjectorAcknowledgesSuccessfulPrefixBeforeLaterSegmentFailure`；其余 16 项见文件

### mongo/driver

- [client_test.go](../../../infra/storage/mongo/driver/client_test.go)：`TestClientConcurrentCloseWaitsForTheFirstDisconnect`、`TestClientCloseRetriesAfterAFailedDisconnect`、`TestIndexCreationRetriesThroughAnElectionWithinBounds`、`TestWithTransactionKeepsTheLastCallbackErrorWhenTheWindowClosesInBackoff`、`TestWithTransactionFailuresBeforeTheCommitAreNotResultUnknown`
- [collection_test.go](../../../infra/storage/mongo/driver/collection_test.go)：`TestIsIndexDefinitionConflict`、`TestIsIndexNotFound`、`TestIndexConflictPolicyRequiresExplicitAutoRecreate`、`TestMongoIndexModelDistinguishesRelativeAndAbsoluteExpiry`、`TestStringifyIDDoesNotExposeNilSentinel`；其余 3 项见文件
- [real_mongo_promises_test.go](../../../infra/storage/mongo/driver/real_mongo_promises_test.go)：`TestRealMongoValidateDeploymentRefusesAStandalone`、`TestRealMongoIndexConflictIsSurfacedUnlessRecreationIsAllowed`
- [real_mongo_transaction_test.go](../../../infra/storage/mongo/driver/real_mongo_transaction_test.go)：`TestRealMongoWriteErrorAbortsTransactionAndKeepsDriverChain`
- [transaction_deadline_integration_test.go](../../../infra/storage/mongo/driver/transaction_deadline_integration_test.go)：`TestRealMongoCommitIsBoundedByTransactionTimeout`、`TestRealMongoEndSessionAfterCommitTimeoutIsBounded`

### mongo/mongotest

- [identity_test.go](../../../infra/storage/mongo/mongotest/identity_test.go)：`TestMongoIdentityPromises`、`TestMongoIdentityBoundaries`
- [index_test.go](../../../infra/storage/mongo/mongotest/index_test.go)：`TestMongoBoundaryPromises`、`TestIndexAndBulkBoundaryPromises`、`TestUniqueIndexOverAnArrayIsRefusedNotGuessed`、`TestUniqueIndexMissingFieldPromises`
- [mongotest_test.go](../../../infra/storage/mongo/mongotest/mongotest_test.go)：`TestInsertRejectsDuplicateIDAndUniqueIndex`、`TestVersionCASSemantics`、`TestUpsertSeedsFromEqualityFieldsOnly`、`TestReplaceWithEvaluatesExpressionsLikeTheServer`、`TestLeaseFilterEvaluatesOwnerTokenStatusAndExpiry`、`TestClaimStealUsesTokenCAS`；其余 16 项见文件
- [transaction_test.go](../../../infra/storage/mongo/mongotest/transaction_test.go)：`TestTransactionWriteErrorAbortsLikeTheServer`、`TestTransactionCommitAfterSwallowedWriteErrorIsRetried`、`TestTransactionNotFoundDoesNotAbortAndPlainErrorsAreNotRetried`、`TestTransactionSnapshotPromises`

### nestwal

- [codec_test.go](../../../framework/nestwal/codec_test.go)：`TestEncodeRejectsUnboundedUnsetPathsAndHeaders`、`TestDecodeRejectsEveryTruncationPoint`、`TestCodecPatchRoundTrip`、`TestCodecDeleteCarriesNoPayload`、`TestCodecRejectsUnknownRecordVersion`；其余 5 项见文件
- [codec_reference_test.go](../../../framework/nestwal/codec_reference_test.go)：`TestEncodeMatchesBinaryWriteReference`、`TestEncodeRejectsSameInputsAsBinaryWriteReference`（手写定长编码与改造前 `binary.Write` 参照实现逐字节对照）
- [corruption_test.go](../../../framework/nestwal/corruption_test.go)：`TestOpenRefusesSegmentGapsAndUnacknowledgedTruncation`、`TestOpenRefusesEachCorruptFrameHeaderField`、`TestOpenTruncatesZeroFilledTailOfLastSegment`、`TestOpenTruncatesZeroFilledTailStartingAtCheckpointFence`、`TestOpenRefusesTailThatIsNotAllZero`；其余 4 项见文件
- [crash_test.go](../../../framework/nestwal/crash_test.go)：`TestNestWALCrashChildProcess`、`TestNestWALCrashKeepsDurablePrefix`
- [effect_inbox_test.go](../../../framework/nestwal/effect_inbox_test.go)：`TestMongoEffectInboxDeduplicatesAndRejectsIdentityConflict`、`TestMongoEffectInboxHandlerFailureLeavesNoReceipt`、`TestMongoEffectInboxSurvivesTransactionRetry`
- [export_test.go](../../../framework/nestwal/export_test.go)
- [inspect_test.go](../../../framework/nestwal/inspect_test.go)：`TestInspectAcceptsDurableCheckpointBoundary`、`TestInspectRefusesRunningWriterWithoutCreatingSnapshot`、`TestInspectPreservesCorruptRecordAndSuffix`、`TestInspectValidWALCancellationAndUnsafeDestinations`、`TestInspectNeverRepairsTornTailOrCheckpoint`
- [pipelined_strict_fallback_bench_test.go](../../../framework/nestwal/pipelined_strict_fallback_bench_test.go)
- [pipelined_strict_fallback_test.go](../../../framework/nestwal/pipelined_strict_fallback_test.go)：`TestAppendWaitsForFsyncOnStrictPathForStrictAndPipelined`、`TestBroadcastPipelinedIsInvisibleUntilFsync`、`TestPipelinedFastPathStillReleasesLockBeforeFsync`、`TestWALTerminalWakesEntityProjectionWaiters`
- [terminal_test.go](../../../framework/nestwal/terminal_test.go)：`TestAckRefusesTerminalWALAndKeepsCheckpoint`、`TestInflightAckFailsAfterFailedFinalSync`、`TestInflightAckAfterCleanCloseStillSucceeds`、`TestAckFirstFsyncFailureIsIndeterminateForTickets`、`TestSyncAfterTerminalNeverFsyncsOrAdvancesDurableLSN`；其余 4 项见文件
- [wal_benchmark_test.go](../../../framework/nestwal/wal_benchmark_test.go)
- [wal_test.go](../../../framework/nestwal/wal_test.go)：`TestWALReplayAckAndReopen`、`TestWALReplayStartsAtAckFenceAcrossRotation`、`TestWALConcurrentAppendAndRotation`、`TestWALRecoversTornTail`、`TestWALRejectsChecksumCorruption`；其余 15 项见文件

### redis

- [cas_test.go](../../../infra/storage/redis/cas_test.go)：`TestCompareAndSetAppliesWhenExpectedValueMatches`、`TestCompareAndSetReturnsCurrentWhenExpectedValueDiffers`、`TestCompareAndSetCanRequireMissingKey`、`TestCompareAndSetRejectsIncompleteCommandsBeforeIO`、`TestCompareAndSetRejectsMalformedScriptReply`；其余 5 项见文件

### redis/driver

- [client_test.go](../../../infra/storage/redis/driver/client_test.go)：`TestMissingKeysSurfaceAsContractErrNil`、`TestNewClientRequiresAnAddress`、`TestRedisClientsHonourContextDeadlinesOnTheWire`、`TestClusterRecoveryPreservesErrorsWithoutReplayingCommands`、`TestAssembleBuildsTheClientAndRefusesNoAddress`；其余 3 项见文件
- [close_test.go](../../../infra/storage/redis/driver/close_test.go)：`TestClientRepeatedCloseReturnsNilOnEveryDeployment`、`TestClientCloseErrorIsReportedOnce`、`TestClientConcurrentCloseAllReturnNil`、`TestAssemblyRepeatedCloseReturnsNil`
- [mget_integration_test.go](../../../infra/storage/redis/driver/mget_integration_test.go)：`TestMGetReturnsOneElementPerKeyIncludingMisses`、`TestMGetWithNoKeysMakesNoRoundTrip`、`TestMGetPreservesBinaryValues`
- [no_replay_test.go](../../../infra/storage/redis/driver/no_replay_test.go)：`TestAWriteWhoseReplyIsLostIsNotReplayedByTheDriver`、`TestNotExecutedErrorsAreStillResent`、`TestResendsStopAtTheConfiguredBudget`、`TestAPipelineWithAnExecutedCommandIsNotResent`、`TestReadsKeepTheDriverRetry`；其余 8 项见文件
- [pipeline_errors_integration_test.go](../../../infra/storage/redis/driver/pipeline_errors_integration_test.go)：`TestIntegrationPipelineDoesNotHideWriteErrors`、`TestIntegrationPipelineFuturesAndLifecycle`、`TestIntegrationPipelinePreservesPostWriteUnknownError`
- [write_lost_reply_integration_test.go](../../../infra/storage/redis/driver/write_lost_reply_integration_test.go)：`TestRealRedisAWriteWhoseReplyIsLostRunsOnce`

### redis/redistest

- [redistest_test.go](../../../infra/storage/redis/redistest/redistest_test.go)：`TestEscapeGlobMatchesPrefixLiterally`、`TestDeleteKeysRefusesAnEmptyPrefix`、`TestDeleteKeysRemovesOnlyKeysContainingThePrefix`、`TestDeleteKeysRemovesOnlyKeysContainingThePrefixOnCluster`

### versionstore

- [fake_redis_test.go](../../../infra/storage/versionstore/fake_redis_test.go)
- [index_test.go](../../../infra/storage/versionstore/index_test.go)：`TestAnIndexedCreateEntersTheIndex`、`TestAnIndexedUpdateRetiresTheEntry`、`TestTheIndexIsReadByScoreAndBounded`、`TestAStoreWithoutAnIndexSaysSo`、`TestAPerOwnerIndexKeepsOwnersApart`；其余 4 项见文件
- [lost_reply_integration_test.go](../../../infra/storage/versionstore/lost_reply_integration_test.go)：`TestRealRedisUpdateWhoseReplyIsLostNeverWritesTwice`
- [update_all_test.go](../../../infra/storage/versionstore/update_all_test.go)：`TestUpdateAllWritesEveryKeyTogether`、`TestUpdateAllRetriesAsAWholeWhenAnyKeyMoved`、`TestUpdateAllRefusesBadArguments`、`TestUpdateAllSettlesALostReplyByItsTokens`、`TestUpdateAllReportsConflictWhenTheBudgetIsExhausted`；其余 1 项见文件
- [versionstore_test.go](../../../infra/storage/versionstore/versionstore_test.go)：`TestStoredVersionIsNeverZeroAndIncrementsPerWrite`、`TestUpdateDecliningToSaveChangesNothing`、`TestUpdatePropagatesMutateErrorWithoutWriting`、`TestCreateRefusesAnExistingKey`、`TestDeleteRequiresTheCallersVersion`；其余 12 项见文件
- [write_token_integration_test.go](../../../infra/storage/versionstore/write_token_integration_test.go)：`TestRealRedisAWriteWhoseReplyIsLostIsRecognisedByItsToken`、`TestRealRedisClusterAWriteWhoseReplyIsLostIsRecognisedByItsToken`、`TestRealRedisAnIndexedWriteWithANaNScoreChangesNothing`
- [write_token_test.go](../../../infra/storage/versionstore/write_token_test.go)：`TestAnUpdateWhoseReplyIsLostIsNotAppliedTwiceWhenTheCallerRetries`、`TestACreateWhoseReplyIsLostIsNotReportedAsTakenWhenTheCallerRetries`、`TestAnUpdateWhoseReplyIsLostStaysAppliedOnceAfterSomeoneElseWritesOnTop`、`TestAnUpdateThatNeverRanIsResentOnce`、`TestResumeReturnsTheEarlierWriteInsteadOfWritingAgain`；其余 8 项见文件

</details>

## 6. 验收与运维

改动后运行受影响包测试；跨包行为变更跑全仓测试，并发相关补 race，生成器相关验证重生成无漂移及正式消费工程。命令与发布记录见 [维护手册](../../maintenance/README.md)。没有新测量不能改容量承诺，性能结论以 [性能报告](../../performance/README.md) 为准。
