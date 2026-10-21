# 核心：DataEngine 持久化：实现与维护

适用版本：v1.29.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）；源码目录最初按 v1.24.0 提取、之后随改动维护，链接由根包文档门禁保证存在，缺漏以源码为准。[设计与使用](../guide/03-dataengine.md)

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

6. outbox 由两样东西驱动：投影器在一个单元把 effect 暂存进 outbox（事务已提交、未被 fence 跳过）后同步调用 `ProjectorOptions.OnEffectsStaged`，`Assembly.Start` 把它接到 `OutboxWorker.Kick`（容量 1 的唤醒通道：多次合并、空闲循环先醒、`RunOnce` 期间到达的唤醒留到下一轮，不丢）；`PollInterval` 轮询保留为兜底（重试退避到期、别的进程的 effect）。唤醒只是提示，不改投影 / checkpoint 的顺序与持久语义；outbox 先于 Projector 构造，唤醒在回放开始前接好。回归：`TestProjectedEffectWakesTheOutboxWithoutWaitingForThePoll`（正式装配、轮询 1 小时）、`TestOutboxKickWakesALoopCoalescesAndStopsCleanly`。

7. checkpoint 只决定崩溃后从哪里重放（`WAL.Ack` 写 checkpoint 槽并清理已确认段），进程内外没有读者依赖它：投影完成通知、票据、实体屏障在投影成功时发出，saga 等回执读 Mongo，`Flush` / `Shutdown` 每轮 `ReplayPass` 退出前确认已成功前缀。所以 `ReplayPass` 对批次、多 mutation 记录和带 effect / 回执 / Remote 写的特殊记录一律按 `CheckpointRecords`（缺省 256）/ `CheckpointInterval`（缺省 20 ms，维护者 2026-10-19 决定保持；特殊记录逐条投影约 16 ms，积压时合并系数约 2，调大可提高系数，代价是重放窗口与未确认额度变长）/ 轮末合并确认（2026-10-18 起特殊记录也合并，[方案](../../maintenance/CONTRACT-2026-10-18-projector-checkpoint-merge.md)）。前提是 Store 实现 `BatchProjectionStore` 并保证这些记录在后续版本写入后仍能重放：`MongoStore.ProjectFenced` 在事务里**先**查事务标记再判 lease fence，标记与业务写、回执、outbox、Remote 提交同事务，重放返回首投结论（applied / skipped），不重判 fence、不重复暂存。例外只有两种，仍逐单元立即确认：单 mutation 快路记录（`projectsOnFastPath`，不写标记，只靠 `_last_tx`，与 `ProjectFenced` 选快路用同一判定）和只实现 `ProjectionStore` 的 Store。崩溃后重放的已投影记录 ≤ `CheckpointRecords` 条且只来自约一个间隔加一个单元的窗口；不得为了少重放而把确认提前到投影成功之前，也不得跳过 ack 失败后的后续段。回归：`TestSpecialRecordsShareCheckpointThresholds`、`TestFastPathRecordStillConfirmsPrecedingSpecialRecords`、`TestSpecialRecordCheckpointLossReplaysTheWholeUnconfirmedPrefix`、真实 Mongo 的 `TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint` 与 saga 级重启 `TestRealStepsRestartReplaysUnconfirmedProjectionsExactlyOnce`。

8. DAO 库名映射（D9 修订版 A）：`DocumentKey.Database` / `RemoteDataMutation.Database` 是逻辑库名，WAL 只记逻辑名；物理库名只经启动时冻结的 [DatabaseMap](../../../framework/dataengine/databases.go)（`NewDatabaseMap`：每个逻辑库只有一个来源——配置或回调非空结果，都没有时 identity；回调对配置已写的逻辑库只能返回空串或同一个值，不同即拒绝启动，不得改回“回调覆盖配置”；校验未声明逻辑名、物理名合法性 / 系统库 / `_<sid>` 后 63 字节、物理库唯一、回调与配置冲突、回调错误；冲突只在 Init 判断，`ValidateDatabaseConfig`（`--check-config` / doctor）不调用回调）。解析点：[mongo_store.go](../../../framework/dataengine/engine/mongo_store.go) `physicalDatabase`（单条投影、批量分组、冷加载与墓碑都经 `collection` 走到它，是 DataEngine 侧的唯一强制点：带库名而 `MongoStoreConfig.Databases` 为 nil 时返回 `dataengine.ErrDatabaseMapNotBound`，在访问任何 DAO 集合前拒绝；空库名取默认库、不需要表）、[mongo_committer.go](../../../framework/remoteentity/mongo_committer.go) `dataDB`（`BindDatabaseMap` 一次绑定；`boundDatabaseMap` 是 Remote 侧的唯一强制点：任何构造方式下，提交里有带库名的 DAO 写入或删除而表未绑定时返回包着同一个哨兵的错误，在写任何 DAO 文档前拒绝、整个 Mongo 事务回滚；只写控制库的提交不需要表）。两边规则相同（维护者 2026-10-21 决定收掉“DataEngine nil 表按 identity 投影”的不对称）：不得恢复任何一侧“没有表时按逻辑名访问库”的回退；有表、表里没有某个逻辑库时仍按 `DatabaseMap.Physical` 的 identity 规则。哨兵与绑定接口 `DatabaseMapBinder` 定义在 [framework/dataengine/databases.go](../../../framework/dataengine/databases.go)。强制时机：`NewMongoStore` 在 `Databases` 为 nil 而 `DeclaredDatabases()` 非空时构造即拒绝（规则在装配时强制一次；记录进 WAL 后才在投影上失败只能卡住投影器），访问时的检查兜底没有声明的进程。绑定只有一个入口：`MongoStore.SetRemoteProjection`（`Assemble` 调用）在 Remote 存储实现 `DatabaseMapBinder` 且本存储有表时先绑表，失败（已绑另一张）则不接上投影并返回错误；本存储没有表时不绑，两边都拒绝。wiring Mod 不再单独绑定。[wiring/dataengine/databases.go](../../../wiring/dataengine/databases.go)（Mod Init 合成、交给 `Assemble` 并发布 `dataengine.databases` 能力、工具入口 `LoadDatabaseMapFile` / `DAOCollection`）。控制库（DataEngine 自己的 `dataengine.database`、Remote 的 `remote_entity.mongo.database`、saga 库、lease fence 的库）是配置给出的物理名，不经映射。[Assembly.Start](../../../framework/dataengine/engine/assembly.go) 在打开 WAL 之后、构造 Projector（构造即回放）之前调 [reconcileDatabaseMapping](../../../framework/dataengine/engine/databases.go)：读 WAL 目录的 `databases.json`（[nestwal/databases.go](../../../framework/nestwal/databases.go)），`HasUnacknowledgedRecords` 为真且上次映射里任一逻辑库换算结果变了（文件不存在按 identity 比较）返回 `ErrDatabaseMappingChanged`，否则原子写入本次映射。不得在投影前把逻辑名换成物理名写进 WAL（Remote 提交的 digest 覆盖库名，改写会让发布路径的重放校验失败），也不得在运行中调用回调。回归：`TestDatabaseMapPriorityAndResolverCalledOncePerLogicalDatabase`、`TestDatabaseMapRefusesResolverConflictingWithConfig`、`TestDatabaseMapRefusesEachInvalidMapping`、`TestMongoStoreResolvesLogicalDatabasesOnEveryDocumentPath`、`TestMongoStoreWithoutDatabaseMapRefusesLogicalDatabases`、`TestReconcileDatabaseMapping*`、`TestMongoCommitterWritesDAODocumentsIntoTheMappedDatabase`、`TestSetRemoteProjectionBindsTheDataEngineDatabaseMap`、`TestInspectReportsTheRecordedDatabaseMapping`、wiring 的 `TestDataEngineModProjectsLogicalDatabaseIntoMappedPhysicalDatabase` / `TestDataEngineModFreezesTheMappingAtInitAndRefusesInvalidConfig` / `TestDataEngineModBindsTheFrozenMapToRemoteStorageAndPublishesIt` / `TestSelfAssembledMongoStoreRequiresAMapWhenDatabasesAreDeclared` / `TestToolsReadTheSameDatabaseMapping`、根包 `TestNoMongoDatabaseOpenedWithALogicalDAOName`、真实依赖 `TestSagaNativeStepsAndEffectRealDependencies`（逻辑库 main 映射到唯一物理库）。

9. 运维卸载单个实体（`entity.unload`，D16，[entity_unload.go](../../../framework/dataengine/engine/entity_unload.go)）的顺序不能调换：①在业务池经 `ManagerAccess.WithLocked` 取实体锁并确认仍是当前托管实例（等持有它的事务结束，受 ctx 约束；它的准入在锁内，结束时要么已登记在途投影、要么已回滚；DataEngine 不直接 import `infra/base/lock`，三大块依赖方向见根包 `TestCorePillarDependencyDirection`），在 `heldMu` 下登记屏障（`Projector.beginEntityUnload`，锁序与锁内准入相同：实体锁 → heldMu）；②命令 goroutine 上 `WaitEntityProjection`；③业务池 `ManagerAccess.Unload`；④defer 撤销屏障。屏障在 `reserve` 的同一临界区检查（`checkUnloadingEntitiesLocked`，紧跟原生步骤屏障），所以登记之后写到该实体的记录一定在 WAL 之前以 `coredata.ErrEntityUnloading` 拒绝、Nest 回滚——这是“②之后内存 == 库”一直成立的唯一原因；不能改成先登记屏障再等锁（持有实体的在途事务会被拒绝），也不能省掉①的锁或②的等待（回归分别在 `TestEntityUnloadWaitsForOrRefusesATransactionHoldingTheEntity` 与 `TestEntityUnloadDrainsProjectionsRefusesWritesThenColdLoadsTheSameState` 里变红，变异记录见 DESIGN-TODO D16 实施结果）。失败（取锁超时 / 另一次卸载 / Remote 所有权不稳定 → `ErrEntityUnloadBusy`；投影超时 → `ErrEntityUnloadTimeout`；`AutoPersist=false` → `ErrEntityUnloadNotPersistent`）都不卸载、撤销屏障，错误不包裹 ctx 错误（admin 审计会把它记成 unknown，而结论是确定的）。`UnloadEntity` 在业务 worker 上直接返回 `fctx.ErrBlockingInBusinessWorker`。admin 命令在 [admin.go](../../../framework/dataengine/engine/admin.go)（`RegisterAdminCommands`，wiring/dataengine Mod 在 Provide 时注册）；`entity.kick` 在 [gateway/admin.go](../../../infra/network/gateway/admin.go)（`PlayerRuntime.CloseSessions`，wiring/gate `RegisterPlayerAdminCommands`：GameIngressMod 与生成的嵌入 TCP Mod 在 Provide 时调用）。

## 2A. 聚合恢复与投影实现走读

[EntityRepository](../../../framework/dataengine/engine/entity_repository.go)的Load合并同ID并发flight，等待该实体必要投影后进入loadAggregate。loadAggregate读取builder与完整一致性视图，先遍历全部DAO的Descriptor/SchemaVersion，再进行RestorePersisted与身份校验；只有所有DAO成功后，才RunLocal创建并发布实体及loaded hooks。前一DAO已解码、后一DAO失败时不能提前发布半实体。

readAggregate的读取回调可能因数据库事务重试再次执行，累积结果必须每次重置；flight遇panic也必须结束并唤醒等待者。这些恢复责任不应由调用方临时补sleep实现。

readAggregate用`LoadSpec.IncludeDeleted`读到墓碑（MongoStore默认过滤`_deleted: true`，其他读取路径不变），按“全部缺失→ErrEntityAggregateNotFound、全部墓碑→ErrEntityAggregateDeleted、混合→ErrEntityAggregateCorrupt”分类。Remote托管kind删档时数据文档被物理删除、墓碑只在Remote元数据里，全部缺失时在同一次一致性读里经Store可选的`RemoteEntityDeleted`（MongoStore→remoteentity.Backend→MongoCommitter读`_remote_entity_meta._deleted`）区分删过档。D10。

[codec.go](../../../framework/nestwal/codec.go)的encodeRecord与decodeRecord固定codecVersion=7；unsupported version在解释record body前拒绝。格式校验不等于尾部截断安全，WAL的文件恢复、fsync与checkpoint分别有守卫。

[projector_remote.go](../../../framework/dataengine/engine/projector_remote.go)按相邻、无共享Entity的纯Remote事务构造有界窗口：特殊effect/receipt、重复事务身份、记录/字节预算构成边界。执行窗口首次失败后停止补位、等待已派发工作，并只确认连续成功前缀。并行不能越过同实体因果依赖。

**持久键与未知键（DESIGN-TODO D13）**：生成 DAO 的持久文档结构体 `<dao>PersistDoc` 是键的唯一声明（定义里的 `bson` 名，缺省 snake_case）。`RestorePersisted` 在 schema 校验之后、解码之前调用 [StoredKeySet.WarnUnknown](../../../framework/dataengine/stored_keys.go)：逐个读文档元素（不解码值），库里有、结构体没有的键按 DAO + 路径每进程一次 `slog.Warn`，计 `dataengine.dao.unknown_stored_key.total{dao}`，不拒绝加载；顶层 `_` 开头的元数据键与缺失键不报。这些键**不读入**（维护者 2026-10-19 撤销“整文档写回保留旧键”的方案 A）：持久文档结构体没有收未知键的字段，解码器忽略它们，DAO 内存里没有，整文档写回（存储层的 Put：`$replaceWith` / ReplaceOne，以及 Remote data 信封替换）就把它们从文档里去掉；单字段补丁只 `$set` 自己的键，旧键留在库里但不再被读取。版本条件、冲突分类、墓碑语义不变。框架不提供清理入口：改名 / 删字段前保留原 `bson` 名或先离线迁移（[SCHEMA-MIGRATION](../SCHEMA-MIGRATION.md) §1），这是业务约束，框架只通知。

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
| [lease_fence.go](../../../framework/dataengine/lease_fence.go) | `LeaseFence` |
| [load.go](../../../framework/dataengine/load.go) | `RawDocument`、`LoadSpec`、`Store`、`EntityExister`、`LoadTemplate`、`Loader` |
| [mutation_types.go](../../../framework/dataengine/mutation_types.go) | `TransactionID`、`Durability`、`MutationKind`、`DatabaseScope`、`DocumentKey`、`FieldPatch`、`Mutation`、`Effect`、`Receipt`、`CommitRecord`、`SyncFieldMeta` |
| [projection.go](../../../framework/dataengine/projection.go) | `Descriptor`、`ProjectionTicket`、`SystemCommitter` |
| [scope.go](../../../framework/dataengine/scope.go) | 函数/方法或内部实现；见源码 |
| [stored_keys.go](../../../framework/dataengine/stored_keys.go) | `StoredKeySet` |
| [sync_view.go](../../../framework/dataengine/sync_view.go) | 函数/方法或内部实现；见源码 |
| [tracker.go](../../../framework/dataengine/tracker.go) | `Tracker`、`TrackerSnapshot` |
| [validate.go](../../../framework/dataengine/validate.go) | 函数/方法或内部实现；见源码 |

### dataengine/engine

21 个实现文件、19 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [admin.go](../../../framework/dataengine/engine/admin.go) | `UnloadCommand`；`RegisterAdminCommands`（`entity.unload`）、`DefaultEntityUnloadTimeout` |
| [assembly.go](../../../framework/dataengine/engine/assembly.go) | `AssemblyConfig`、`AssemblyDeps`、`Assembly` |
| [doc.go](../../../framework/dataengine/engine/doc.go) | 函数/方法或内部实现；见源码 |
| [entity_delete.go](../../../framework/dataengine/engine/entity_delete.go) | 函数/方法或内部实现；见源码 |
| [entity_projection.go](../../../framework/dataengine/engine/entity_projection.go) | 函数/方法或内部实现；见源码 |
| [entity_repository.go](../../../framework/dataengine/engine/entity_repository.go) | `RecoveryGate`、`EntityRepository` |
| [entity_unload.go](../../../framework/dataengine/engine/entity_unload.go) | `EntityUnloadResult`、`Runtime.UnloadEntity`、`ErrEntityUnloadBusy` / `ErrEntityUnloadTimeout` / `ErrEntityUnloadNotPersistent`；卸载屏障 `beginEntityUnload` / `checkUnloadingEntitiesLocked` |
| [fenced_step.go](../../../framework/dataengine/engine/fenced_step.go) | `FenceOutcomeProjectionStore` |
| [health.go](../../../framework/dataengine/engine/health.go) | 函数/方法或内部实现；见源码 |
| [mongo_load.go](../../../framework/dataengine/engine/mongo_load.go) | 函数/方法或内部实现；见源码 |
| [mongo_projection.go](../../../framework/dataengine/engine/mongo_projection.go) | 函数/方法或内部实现；见源码 |
| [mongo_store.go](../../../framework/dataengine/engine/mongo_store.go) | `MongoStoreConfig`、`MongoStore`、`RemoteProjectionStore` |
| [operation_gate.go](../../../framework/dataengine/engine/operation_gate.go) | 函数/方法或内部实现；见源码 |
| [outbox_store.go](../../../framework/dataengine/engine/outbox_store.go) | `OutboxLease`、`OutboxItem`、`OutboxBacklog`、`OutboxStore`、`MongoOutboxStore` |
| [outbox_worker.go](../../../framework/dataengine/engine/outbox_worker.go) | `OutboxPublisher`、`OutboxWorkerOptions`、`OutboxWorkerStats`、`OutboxWorker` |
| [projection_plan.go](../../../framework/dataengine/engine/projection_plan.go) | 函数/方法或内部实现；见源码 |
| [projector.go](../../../framework/dataengine/engine/projector.go) | `ProjectionStore`、`BatchProjectionStore`、`RemoteParallelProjectionStore`、`ProjectorOptions`、`ProjectorStats`、`Projector` |
| [projector_remote.go](../../../framework/dataengine/engine/projector_remote.go) | 函数/方法或内部实现；见源码 |
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

9 个实现文件、1 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [cas.go](../../../infra/storage/redis/cas.go) | `ScriptRunner`、`CompareAndSetCommand`、`CompareAndSetIndex`、`CompareAndSetResult` |
| [client.go](../../../infra/storage/redis/client.go) | `IRedis`、`EvalCall`、`DurableEvaler`、`DurableBatchEvaler`、`ReplicatedEvaler`、`ReplicatedEvalResult`、`ListTrimmer`、`ListRemover`、`Z` |
| [config.go](../../../infra/storage/redis/config.go) | `Config` |
| [errors.go](../../../infra/storage/redis/errors.go) | 函数/方法或内部实现；见源码 |
| [lock.go](../../../infra/storage/redis/lock.go) | `IDistLock`、`IDistLockFactory` |
| [pipeline.go](../../../infra/storage/redis/pipeline.go) | `IPipeline`、`FutureBytes`、`FutureStringMap`、`FutureInt64` |
| [pubsub.go](../../../infra/storage/redis/pubsub.go) | `IPubSub`、`PubSubMessage` |
| [script.go](../../../infra/storage/redis/script.go) | `IScript` |
| [versioned_lock.go](../../../infra/storage/redis/versioned_lock.go) | `IVersionedLock`、`IFencedVersionedLock`、`VersionedLockOptions`、`IVersionedLockFactory` |

### redis/driver

8 个实现文件、9 个测试文件。

| 源码 | 导出类型（定位用） |
| --- | --- |
| [assembly.go](../../../infra/storage/redis/driver/assembly.go) | `Assembly` |
| [client.go](../../../infra/storage/redis/driver/client.go) | `Client` |
| [cluster_recovery.go](../../../infra/storage/redis/driver/cluster_recovery.go) | 函数/方法或内部实现；见源码 |
| [lock.go](../../../infra/storage/redis/driver/lock.go) | `DistLockFactory`、`AutoExtendLock` |
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

- [lease_fence_test.go](../../../framework/dataengine/lease_fence_test.go)：`TestLeaseFenceReceiptRejectsIdentityAndPayloadTampering`
- [load_test.go](../../../framework/dataengine/load_test.go)：`TestLoaderRespectsDependenciesAndSkipsLoadedOrDeletedDocuments`、`TestLoaderRejectsUnknownCircularAndStrictCallbackFailures`、`TestLoaderNonStrictSkipIsCountedAndStrictStillFails`
- [model_test.go](../../../framework/dataengine/model_test.go)：`TestCommitRecordEmptyIncludesReceipts`、`TestCloneEffectPreservesAndDetachesHeaders`、`TestResolveDatabaseScopeDefaultsAndUsesTypedScope`、`TestMapPatchPathBuildsOnlySafeNestedPaths`、`TestWaitProjectionHonorsTicketAndContext`；其余 1 项见文件
- [tracker_test.go](../../../framework/dataengine/tracker_test.go)：`TestTrackerAcceptVersionIsConsecutiveAndCompareAndSwap`、`TestTrackerAdvanceVersionAllowsRemoteEntityJumpsAndIsIdempotent`、`TestTrackerPersistenceAcceptDoesNotConsumeSyncDirty`、`TestTrackerSnapshotRestoreCoversVersionAndSyncState`；其余 2 项见文件
- [stored_keys_test.go](../../../framework/dataengine/stored_keys_test.go)：`TestStoredKeySetReportsKeysTheDocumentStructDoesNotDeclare`、`TestStoredKeySetWarnsOncePerKey`；生成 DAO 上的加载期 WARN 与“不读入”在 `codegen/internal/dao/testdata/runtime/roundtrip_test.go`（`TestUnknownStoredKeysWarnOncePerKeyAndAreNotLoaded`），真实 Mongo（补丁不碰旧键、整文档写回去掉、重放幂等、Remote 载荷）在 `stored_keys_mongo_test.go`，由 `codegen/scripts/dao-golden-runtime.sh` 运行
- [validate_test.go](../../../framework/dataengine/validate_test.go)：`TestValidateMutationRefusesEachMalformedShape`、`TestValidateMutationCrossChecksRemoteCommitAgainstHeader`、`TestValidateCommitRecordRefusesEachMalformedPart`、`TestLeaseFenceValidateRefusesEachIncompleteFence`、`TestLeaseFenceLoaderAndTrackerGuards`

### dataengine/engine

- [admission_reserve_benchmark_test.go](../../../framework/dataengine/engine/admission_reserve_benchmark_test.go)
- [assembly_test.go](../../../framework/dataengine/engine/assembly_test.go)：`TestAssembleRefusesEachMissingDependency`、`TestJetStreamOutboxPublisherRefusesIncompleteEffects`、`TestAssemblyRetriedShutdownHandsOverWALWithoutCheckpointRegression`、`TestAssemblyKeepsTheRuntimeUntilShutdownCompletes`、`TestRuntimeRepeatedStartPreservesReadyAndOutbox`；其余 6 项见文件
- [batch_limits_test.go](../../../framework/dataengine/engine/batch_limits_test.go)：`TestMultiBatchCapabilityAndSpecialBoundaries`、`TestMultiCheckpointThresholdsAndFailurePrefix`、`TestMultiCheckpointLossKeepsEntireSuccessfulPrefix`、`TestMultiCheckpointFlushesBeforeHeldAndOnCancel`、`TestMarkerlessSingleRecordStillCheckpointsImmediately`；其余 4 项见文件
- [entity_delete_test.go](../../../framework/dataengine/engine/entity_delete_test.go)：`TestDataEngineDeleteDefersMemoryRemovalUntilTransactionAdmission`、`TestDataEngineDeleteRollbackLeavesEntityLive`、`TestDataEngineDeleteAdmissionPanicFencesAndStopsServingEntity`、`TestShortWorkerRemoteDeleteIsDefiniteRejection`、`TestDeleteAdmissionOtherPanicStaysIndeterminate`；其余 4 项见文件
- [entity_repository_test.go](../../../framework/dataengine/engine/entity_repository_test.go)：`TestEntityRepositorySingleFlightsCompleteAggregate`、`TestEntityRepositoryRecoveryBarrierAndIncompleteAggregate`、`TestEntityRepositoryRejectsTombstone`、`TestEntityRepositorySeparatesAbsentFromDeleted`、`TestEntityRepositoryReadsRemoteTombstoneForAbsentRemoteAggregate`（全部缺失 / 全部墓碑 / Remote 元数据墓碑，D10）、`TestEntityRepositoryPreservesRemoteVersionVector`；其余 11 项见文件
- [entity_unload_test.go](../../../framework/dataengine/engine/entity_unload_test.go)：`TestEntityUnloadDrainsProjectionsRefusesWritesThenColdLoadsTheSameState`、`TestEntityUnloadTimesOutWithoutTouchingTheEntity`、`TestEntityUnloadWaitsForOrRefusesATransactionHoldingTheEntity`、`TestEntityUnloadRefusals`、`TestEntityUnloadRemoteManagedEntity`（正式 Nest + 文件 WAL + Mongo 替身）；真实 Mongo 的同一流程是生成工程 `TestGeneratedDataEngineAdminUnload`（`scripts/test-dataengine-generated.sh`），命令注册见 wiring 的 `TestModRegistersEntityUnloadAdminCommand`
- [failure_integration_test.go](../../../framework/dataengine/engine/failure_integration_test.go)：`TestNATSOutageDoesNotBlockProjectionAndBacklogRecoversByEffectID`
- [fenced_eviction_once_test.go](../../../framework/dataengine/engine/fenced_eviction_once_test.go)：`TestSkippedFencedStepIsQueuedForEvictionOnce`、`TestTerminalAckWakesWaitersOfAPendingEviction`
- [fenced_step_resync_test.go](../../../framework/dataengine/engine/fenced_step_resync_test.go)：`TestSkippedFencedStepResyncsSubscribersFromMongo`、`TestSkippedFencedStepWithoutAuthorityRetractsSubscribers`
- [lease_fence_integration_test.go](../../../framework/dataengine/engine/lease_fence_integration_test.go)：`TestRealMongoFencedProjectionSerializesWithLeaseTakeover`
- [lease_fence_skip_promises_test.go](../../../framework/dataengine/engine/lease_fence_skip_promises_test.go)：`TestFencedLocalStepSkippedAfterLeaseExpiryDoesNotFenceNextTransaction`、`TestFencedLocalStepAppliedReleasesBarrierWithoutEviction`、`TestFencedStepRedeliveryWaitsOnlyUntilThePreviousRecordSettles`、`TestFencedStepBarrierIsReleasedOnEveryExit`、`TestFencedStepSkippedDuringStartupRecoveryNeedsNoEviction`；其余 1 项见文件
- [mongo_benchmark_test.go](../../../framework/dataengine/engine/mongo_benchmark_test.go)
- [mongo_store_test.go](../../../framework/dataengine/engine/mongo_store_test.go)：`TestMongoStoreUsesAbsoluteExpiryForReceipts`、`TestMongoStoreLeaseFenceSkipsStaleSagaTransaction`、`TestMongoStoreLeaseFenceRejectsExpiredLease`、`TestMongoStoreRefusesARetiredRemoteLeaseFenceRecordWithoutWriting`、`TestMongoStoreLeaseFenceAppliesOnlyMatchingOwnerAndToken`；其余 27 项见文件
- [outbox_test.go](../../../framework/dataengine/engine/outbox_test.go)：`TestOutboxCloseBeforeStartIsTerminal`、`TestOutboxConcurrentStartClose`、`TestOutboxRetryDelayStartsAfterFailedPublish`、`TestMongoOutboxStoreClaimTakesLeaseByTokenCAS`、`TestMongoOutboxStoreSkipsEffectsNotYetAvailable`；其余 14 项见文件（含投影后唤醒的两项）
- [projection_test.go](../../../framework/dataengine/engine/projection_test.go)：`TestPlanProjectionSegmentsPreservesMixedOrder`、`TestBatchProjectionEligibilityMatchesMongoContract`、`TestPlanProjectionSegmentsBoundsOrdinaryBatches`、`TestPlanProjectionSegmentsHonorsMaxRecords`、`TestPlanProjectionSegmentsPreservesEveryRecordAndFence`；其余 17 项见文件
- [projector_benchmark_test.go](../../../framework/dataengine/engine/projector_benchmark_test.go)
- [projector_remote_test.go](../../../framework/dataengine/engine/projector_remote_test.go)：`TestRemoteProjectionIndependentEntityCompletesWhileFirstBlocked`、`TestRemoteProjectionFailureOnlyAcknowledgesPrefixAndReplaysSuffix`、`TestRemoteProjectionWindowBoundaries`、`TestRemoteProjectionCheckpointFailureKeepsWindow`、`TestRemoteProjectionRefillsWhileFirstRecordIsBlocked`；其余 4 项见文件
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
- [mongotest_test.go](../../../infra/storage/mongo/mongotest/mongotest_test.go)：`TestInsertRejectsDuplicateIDAndUniqueIndex`、`TestVersionCASSemantics`、`TestUpsertSeedsFromEqualityFieldsOnly`、`TestLeaseFilterEvaluatesOwnerTokenStatusAndExpiry`、`TestClaimStealUsesTokenCAS`；其余 15 项见文件
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

- [client_test.go](../../../infra/storage/redis/driver/client_test.go)：`TestRedisIntegerParsesWAITAOFReplyValues`、`TestRedisIntegerRejectsInvalidWAITAOFReplyValue`、`TestEvalDurableRejectsClusterTopologyBeforeIO`、`TestMissingKeysSurfaceAsContractErrNil`、`TestEvalDurableRejectsMalformedWAITAOFReply`；其余 8 项见文件
- [close_test.go](../../../infra/storage/redis/driver/close_test.go)：`TestClientRepeatedCloseReturnsNilOnEveryDeployment`、`TestClientCloseErrorIsReportedOnce`、`TestClientConcurrentCloseAllReturnNil`、`TestAssemblyRepeatedCloseReturnsNil`、`TestDistLockAfterClientCloseKeepsReportingErrClosed`
- [lock_integration_test.go](../../../infra/storage/redis/driver/lock_integration_test.go)：`TestDistLockExpiryAndReacquireIntegration`
- [lock_test.go](../../../infra/storage/redis/driver/lock_test.go)：`TestAutoExtendLockKeepsLeaseAliveUntilRelease`、`TestAutoExtendLockSurvivesTransientExtendErrors`、`TestAutoExtendLockSurfacesLostLease`、`TestAutoExtendLockRejectsInvalidConfiguration`、`TestAutoExtendLockRejectsConcurrentAcquire`；其余 8 项见文件
- [lock_toxic_integration_test.go](../../../infra/storage/redis/driver/lock_toxic_integration_test.go)：`TestToxicRedisDroppedReleaseReplyLeavesTheLockUncertainUntilReconciled`、`TestToxicRedisDroppedAcquireReplyIsReconciledNotRetried`、`TestToxicRedisLatencyKeepsAcquireWithinItsDeadline`
- [mget_integration_test.go](../../../infra/storage/redis/driver/mget_integration_test.go)：`TestMGetReturnsOneElementPerKeyIncludingMisses`、`TestMGetWithNoKeysMakesNoRoundTrip`、`TestMGetPreservesBinaryValues`
- [no_replay_test.go](../../../infra/storage/redis/driver/no_replay_test.go)：`TestAWriteWhoseReplyIsLostIsNotReplayedByTheDriver`、`TestNotExecutedErrorsAreStillResent`、`TestResendsStopAtTheConfiguredBudget`、`TestAPipelineWithAnExecutedCommandIsNotResent`、`TestReadsKeepTheDriverRetry`；其余 6 项见文件
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
