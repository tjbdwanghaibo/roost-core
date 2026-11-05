# 核心：DataEngine 持久化：设计与使用

当前 main 普通 DAO 支持 setter 路径驱动的嵌套 struct/map/list 增量持久化，使用 BSON `$set`/`$unset`；完整替换边界、事务独立性与验证见[递归增量持久化](../DATAENGINE-INCREMENTAL-PERSISTENCE.md)。Remote 权威信封不在此轮范围内。

适用版本：当前 main 与最新发布版本（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/03-dataengine.md) · [模块总目录](../README.md)

## 先理解这一块

进程突然退出后，已确认的游戏数据如何找回来？DataEngine负责把业务修改变成可恢复的持久记录。

先理解WAL确认和数据库落库不是同一时刻，再选择持久策略。不了解fsync、DAO等词时先看入门术语表。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 唯一正式持久链路

DAO 变更 → Nest CommitRecord → nestwal.WAL → dataengine/engine.Projector → Store/Mongo → outbox。dataengine 定义契约，engine 实现 Assembly、Runtime、Repository 和投影；wiring/dataengine 负责依赖、配置与生命周期装配。旧独立 nestwal.Committer/Runtime 不作为现行入口。

一条 CommitRecord 原子准入包含本事务 mutation、effect 与 receipt。Tracker 的同步脏状态与本事务持久变更不是同一份数据。Mutation 表达 Put/Patch/Delete，并携带版本和事务身份；多 DAO 是正式批量契约，不能假定一实体只有一文档。

## 2. WAL 格式与恢复

仅支持 codec 7。旧 codec 明确报 ErrUnsupportedRecordVersion，不能消费后推进 checkpoint，不能自动删除数据“修复”启动。目录由一个持有者独占；尾部截断、校验失败与完整坏记录分开处理。Ack/checkpoint 只推进连续可确认前缀，在前进前满足 WAL 同步要求。

strict 是 WAL fsync，非 Mongo 提交。async 允许先投影，但 Ack 前仍须保证对应 WAL 同步；pipelined 用 LSN 票据确认 fsync。持久结果未知是终止安全写入的原因，不能以一次重试成功覆盖不确定结论。

## 3. 投影与幂等

持实体锁时记录处于 held，投影器不得越过这道边界修改权威。解锁后按依赖推进；同实体版本不能倒序。Mongo 通过条件版本、事务身份、receipt/fence 等判定重复与冲突；“数据库版本更高”不是无条件成功。

**运行期不得直接改权威集合**：加载着的实体以内存为权威，投影按 `_id` + `_version == expected` 条件写（`MongoStore`，`framework/dataengine/engine/mongo_store.go`）。在进程运行时直接改 Mongo 文档：只改字段、不动 `_version` 时版本仍匹配，下一次投影按内存状态覆盖改动（静默丢失）；改了 `_version`（如恢复旧备份）时投影 `ErrProjectionConflict` fatal、进程 fence，重启时启动投影恢复仍冲突、起不来。在线改档写成业务的 GM handler（一条普通 Nest 消息，走正式提交链路）；整档恢复先停该 sid 的进程并确认 WAL 已排空（game-demo `cmd/sidmove wal-drained`），再写回文档，启动后冷加载以库里的版本继续。冲突后的识别与处置见[维护手册 §4](../../maintenance/README.md#4-故障处理)。框架只提供与业务字段无关的两条运维命令 `entity.kick` / `entity.unload`（[§6 单实体运维命令](#单实体运维命令d16)），改档与恢复不进框架（[DESIGN-TODO D16](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d16-单个实体--玩家的运维入口)，维护者 2026-10-20 决定 C）。

**投影的写法**：Put（新建实体的首个提交、整文档写回）是整文档替换，Patch 只 `$set` / `$unset` 自己的键，删档写墓碑。Put 的单条路径用管道更新 `$replaceWith`，文档包在 `$literal` 里按原样写入；批量路径用 `ReplaceOne`。两条路径存下的文档必须相同，等于 WAL 里的那份；业务字符串以 `$` 开头（玩家名、宠物名）也照原样保存，不被当成字段路径或变量。v1.12.2～v1.31.1 的单条路径没有包 `$literal`，这类值会被改写或让投影卡住（检测方法见 CHANGELOG）。实现、不变量与回归见[实现篇 §2 第 10 条](../impl/03-dataengine.md#2-必须保持的契约)。

**投影并行**（2026-10-27 起）：投影器按冲突键把一轮回放的记录分成互不相干的组，分给至多 `dataengine.projection.workers`（缺省 32，2026-11-05 由 8 调大；1 = 串行）条通道同时写 Mongo，通道内照常合批。写同一实体（同一文档 `_id`）、同一回执 / 协调文档、同一 effect 的记录之间仍按 WAL 顺序投影（带 effect 的记录也一样：缺省**按实体有序**，不相干实体的带 effect 记录并行投影，进 outbox 的先后可能与 WAL 不同；需要“全部带 effect 的记录按 WAL 顺序暂存”的进程设 `dataengine.projection.effect_order: wal`，维护者 2026-11-05，仓内消费者核对见 [impl/03](../impl/03-dataengine.md#effect-顺序范围)）；checkpoint 只确认连续成功的前缀，前缀之后已写入的记录在下一轮或重启后靠事务标记幂等重放。它提高的是“同一时刻在途的 Mongo 调用数”，对 RTT 大的远端 Mongo 收益最大；`effect_order: wal` 时全部带 effect 的记录仍是一条串行通道。一轮最多读 `batch_records × workers` 条，每条通道仍是满批。设计、取舍与实测见 [FEATURE-2026-10-27-projector-entity-parallel](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/FEATURE-2026-10-27-projector-entity-parallel.md) 与 [性能报告](../../performance/PROJECTOR-ENTITY-PARALLEL-20261027.md)。

投影器可重放同一已提交记录，外部副作用必须通过稳定身份去重。WAL checkpoint 只决定崩溃后从哪里重放：投影器按条数 / 间隔（`dataengine.projection.checkpoint_records` / `checkpoint_interval`，缺省 256 条 / 20 ms）合并确认，带回执 / effect 的记录（saga 原生步骤）与普通记录一样合并（2026-10-18 起）。这类记录逐条投影约 16 ms，缺省 20 ms 间隔下积压时合并系数约 2；维护者 2026-10-19 决定缺省保持 20 ms，需要更高吞吐的部署可调大 `checkpoint_interval`，代价是崩溃后重放窗口与未确认额度变长（重放仍幂等）。崩溃后会多重放这段已投影的记录，由事务标记判定为已应用，lease fence、回执与 effect 都保持首投的结果。只有单 mutation 快路记录仍逐条确认。effect 暂存进 outbox 后投影器立即唤醒 outbox 发布（2026-10-18 起），`dataengine.outbox.poll_interval`（缺省 100ms）只是兜底轮询，不再决定 effect 的发布延迟。WAL backlog、磁盘/年龄健康阈值、outbox 硬容量分别处理，不能统称任一超限都直接 fail-stop。启用前核对容量与保留期，receipt/事务标记不能在仍有重放可能时提前过期。

**消费 effect（事务性 outbox 的消费端）**：业务用 `wiring/dataengine` 的 `SubscribeEffects(ctx, registry, EffectConsumer{Durable, Topic}, handler)`（D12 方向 A，维护者 2026-10-18 决定）。效果流、主题前缀与回执库按 DataEngine Mod 的声明（`dataengine.effects.*`、`dataengine.database`）取，回执集合缺省每个 durable 一个（`_effect_inbox_<durable>`）。语义：至少一次投递、按 EffectID 至多一次生效——handler 在 Mongo 事务里运行，经传入 ctx 的 Mongo 写与回执原子提交，再到达读到回执就跳过；Mongo 驱动可能重跑事务回调，事务外的副作用（调用别的服务）按 EffectID 幂等（demo 升级邮件把它当 mail 的 RequestID）。handler 返回普通错误 → 不写回执、按退避 nak 重投（缺省 250ms..30s，MaxDeliver 25000）；返回 `PoisonEffect(err)`、信封解不开或同一 EffectID 内容不同 → 直接 Term 不重投、ERROR 日志并计 `dataengine.effect_consumer.handled_total{result="poison"}`（非零要人核对）；handler 的 ctx 以 AckWait（缺省 30s）为截止。回执 TTL（缺省 30 天）不长于 `dataengine.effects.max_age` 时拒绝订阅。

**effect 消费者的并发与顺序**（维护者 2026-11-05）：一个进程里同时跑至多 `EffectConsumer.Workers` 个 handler（不写取 `dataengine.effect_consumer.workers`，缺省 16；1 = 在回调里逐条处理，此前的行为）。信封 `Key`（生产方 `nest.Effect.Key`，生成的 `Emit<Topic>(key, …)` 的 key）相同的 effect 在本进程按到达顺序逐条处理，不同 Key 并发，没有 Key 的不与任何 effect 排序——所以 Key 应当是“需要保持顺序的那个实体”（玩家 ID、联盟 ID）。同一进程里不同实体的处理先后不再跟流顺序一致；共享 durable 的多个实例之间本来就没有顺序。已接纳未结算的 effect 不超过消费者的 `MaxAckPending`（缺省 256），满了回调等一条结算（背压），不确认也不 nak；排队期间自动发 in-progress，broker 不会把排队中的 effect 重投给别的实例。契约与仓内消费者核对见 [impl/03 effect 消费者进程内并发](../impl/03-dataengine.md#effect-消费者进程内并发)。

**effect outbox 发布并发**：`dataengine.outbox.workers` 缺省 8（2026-11-05 由 2 调大，**是给最终压测的试用值，压测后再定**）：每个 worker 一批认领、发布、确认，本机 2 个在约 330 次/秒的写入下开始积压，8 个能跟上，但多占 Mongo（[benchmarks/alliance-join-1000-20261105](../../performance/benchmarks/alliance-join-1000-20261105/README.md)）。

**逻辑库名与物理库名**（[DESIGN-TODO D9](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d9-dao-库名不随配置_id-是编码后的-entityid) 修订版 A，维护者 2026-10-20 决定）：DAO 标记的 `db=` 是**逻辑库名**（小写字母开头，小写字母、数字、下划线；骨架写 `db=main`），`DocumentKey.Database`、WAL 记录与 Remote 提交里的库名都是它。Mongo 里的物理库名只由部署决定，启动时合成一张冻结的表（`dataengine.DatabaseMap`），之后只查表、不再算：

- **两个入口**：配置 `dataengine.databases`（逻辑名 → 物理名，走配置声明、`--check-config` 与 doctor 的同一份规则）；代码回调 `kitdataengine.WithDatabaseResolver(func(logical string) (string, error))`，DataEngine Mod Init 时对代码声明的每个逻辑库各调用一次。**同一逻辑库只能有一个来源**（维护者 2026-10-20 决定）：配置没写的逻辑库取回调返回的非空物理名；配置写了的，回调返回空串（沿用配置）或相同的值都允许，返回不同的非空值拒绝启动并点名逻辑库与两个值。二者都没有时是逻辑名本身。回调只在 Init 调用，所以这条冲突检查也只在 Init 发生，`--check-config` 与 doctor 发现不了。
- **声明**：生成的 `db/gen_databases.go` 每个逻辑库一个常量（`DatabaseMain = "main"`，各 DAO 的 `<Dao>DBName` 引用它），`init` 里 `dataengine.DeclareDatabases` 登记（是否有 DAO 按 sid 分库也一起登记）。
- **没有映射**：表已合成、但声明了却没有配置也没有回调结果的逻辑库，物理名就是逻辑名本身（启动日志告警），与引入映射之前的行为一致；这条只针对表里某个逻辑库没配物理名，**根本没有表**（DataEngine 投影存储的 `MongoStoreConfig.Databases` 为 nil，或 Remote 存储没绑定）时两边都直接报错（见下面“唯一解析点”）；生成配置总是写 `main: <工程名派生的库>`，新工程开箱不会落到名为 `main` 的库。
- **一次校验**（Init 时，失败拒绝启动并点名）：配置里出现代码未声明的逻辑名（拼写错误；二进制一个逻辑库都没声明时无从判断，只告警）；物理名为空、含 Mongo 禁止的字符、是系统库（admin / local / config）、按 sid 分库时加上 `_<sid>` 超过 63 字节；两个逻辑库映射到同一个物理库（大小写不同也算，不提供允许开关）；回调与配置冲突；回调返回错误。
- **唯一解析点**：投影（单条与批量）、冷加载、删档墓碑经 `MongoStore` 的 `physicalDatabase`；Remote 写 DAO 文档经 `MongoCommitter.dataDB`。**两边同一条规则、同一个哨兵**（维护者 2026-10-21 决定）：没有表时读写带库名的 DAO 文档一律返回 `dataengine.ErrDatabaseMapNotBound`，不按逻辑名访问名为 `main` 之类的库；只碰控制库 / 默认库（事务标记、回执、outbox、Remote 元数据与快照）的操作不需要表。**表只给 DataEngine 一次**：wiring 由 DataEngine Mod 冻结后放进 `MongoStoreConfig.Databases`；自建装配用 `dataengine.NewDatabaseMap` 合成（不配映射时 `DatabaseMapConfig{Declared: dataengine.DeclaredDatabases()}`，逻辑名即物理名）。`engine.Assemble` / `MongoStore.SetRemoteProjection` 接 Remote 投影时，若 Remote 存储实现 `dataengine.DatabaseMapBinder`（`MongoCommitter`、`Backend`），自动把同一张表绑过去；Remote 存储已绑了另一张表时接线报错、不覆盖。Remote 若要在接上投影之前就写 DAO 文档（例如先启动、恢复上一轮未完成的提交），仍可提前调 `Backend.BindDatabaseMap` 绑同一张，之后接线再绑同一张是无操作。**最早的拒绝点**是 `NewMongoStore`：进程链接了声明逻辑库的 DAO（生成的 `gen_databases.go`）却没给表时构造即失败，不等第一次投影（那时记录已进 WAL，只能卡在投影上）；没有声明的进程（手写 DAO、只用控制库的工具）由访问时拒绝兜底；业务与工具经 `kitdataengine.DAOCollection` / `Databases(registry)`（能力 `dataengine.databases`）/ `LoadDatabaseMapFile`。根包 `TestNoMongoDatabaseOpenedWithALogicalDAOName` 守住产品代码与模板里没有把 `DbName()` / `<Dao>DBName` 直接交给 `.Database(`。
- **改映射等于搬数据**：WAL 只记逻辑名、投影时才换算；上次启动的映射记在 WAL 目录的 `databases.json`，还有未投影记录而映射变了（含没有该文件的旧 WAL 配非 identity 映射）的进程以 `ErrDatabaseMappingChanged` 拒绝启动。顺序是停写 → 排空 WAL → 离线迁移数据 → 改映射启动（[PLAYER-MIGRATION §5](../PLAYER-MIGRATION.md#5-改-dao-库名映射)）。同一部署里所有会写同一批文档的进程（含跨进程写 Remote 实体的）映射必须一致。

## 4. 冷加载与 schema

EntityRepository 读取聚合一致性视图，先验证所有持久 DAO 的完整性与当前 schema，再水合并发布 Entity。旧/未来 schema 都拒绝；生成 RestorePersisted 在解码前校验。自动 schema 迁移已撤销，不能把仍存在的通用 migration 包解释成 Repository 自动转换旧 DAO 的能力。改了持久结构时按[业务 schema 变更操作手册](../SCHEMA-MIGRATION.md)停写后离线转换。

**新建实体本身就落库并写齐每个 DAO；冷加载把缺的 DAO 补空**（维护者 2026-10-30 两条决定）。冷加载把一个实体的持久 DAO 文档分四类：全部缺失是从未创建（not found）；没有活文档、只有墓碑（其余缺失）是删过档（deleted——删档之后才给 kind 加的 DAO 本来就没有墓碑，不会让它复活）；活文档与墓碑混在一起是损坏（`ErrEntityAggregateCorrupt`）；**至少一个活文档、其余缺失时把缺的 DAO 按初值（builder 给出的零值 / 初值）补上**：上线后给已有 kind 新增 DAO 是常态，存量实体没有新 DAO 的文档。补上的 DAO 处于“版本 0、库里没有文档”，这个实体的**下一笔持久提交**把它以整文档写出，读路径本身不写库；每次补空打 WARN（kind、实体 ID、集合名，不带业务数据）并计 `dataengine.aggregate.dao_filled.total{kind,collection}`：新增 DAO 之后每个存量实体第一次加载各一次是正常的，别的时候出现说明丢写、误删或投影缺口，要人核对。Remote 托管实体部分缺失时还查 Remote 元数据墓碑，删过档的不补、按损坏拒绝。另一头，**新建的持久实体本身就落库**：创建它的持久事务提交时为它的每个 DAO 写整文档，与这笔事务改没改字段无关（新建后一个字段都不改也会落库，重启后能加载）。由谁提交按入口分：Nest handler 内 `Create` / `GetOrCreate`（含 handler 里的 `RunDetachedTransaction` / `RunIsolatedTransaction`）由该事务捕获，事务回滚则不落库、撤销发布；Nest 之外直接 `Create` / `GetOrCreate`（登录时建号、服务 Init 里建 World、handler 之外的 `RunDetachedTransaction` 里）由 DataEngine 注册的新建准入门补一笔 strict 事务，提交被接受后才返回（会等 WAL，业务 worker 上调用直接报 `fctx.ErrBlockingInBusinessWorker`）；memory 快路径 handler（rollback=none、durability=memory）没有提交点，新建持久实体返回 `nest.ErrMemoryTransactionPersistentWrite`，有 RollbackTx 的 memory 事务按现有规则在提交点整条拒绝。两条规则在 Nest 是同一处：事务第一次登记某个实体的 DAO 时，把它版本 0（还没有文档）的 DAO 一并以整文档登记，新建实体的每个 DAO、冷加载补上的 DAO 都是版本 0；每个 DAO 都有文档之后只写改过的 DAO。边界：Remote 托管 kind 在 Nest 之外新建不经准入门（Remote 持久写只能走消息声明的 Remote 批次），照旧在第一笔 Remote 批次提交时写齐；没有 DataEngine 的进程（纯内存、单元测试）没有库可落，事务外新建只在内存。实现与回归见[实现篇 §2 第 12 条](../impl/03-dataengine.md#2-必须保持的契约)。

恢复 gate 未就绪时不能对外发布“空的已恢复实体”。并发加载合并 flight；发布后回调重新连接 Sync 等协作者。缓存只是加速，缺失/过期不得被解释成权威不存在。

**删档后该 ID 不可再用**（维护者 2026-10-17 决定，[DESIGN-TODO D10](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/DESIGN-TODO.md#d10-删档后用同一-id-重建)）。`Destroy(deletePersisted=true)` 留下墓碑后，冷加载得到“删过档”（`entity.ErrAuthorityEntityDeleted` / `ErrEntityAggregateDeleted`，客户端码 `entity_deleted = 3`），它与“从未创建”（`ErrAuthorityEntityNotFound`，码 2）是两个哨兵，**不满足** not found：“不存在就创建”的代码不会去重建。范围只守 GetOrCreate（维护者同日决定）：生成 lifecycle 的 GetOrCreate 不重建、发往该 ID 的 Nest 请求回复码 3。**直接 Create（handler 内或 lifecycle `Create`）不读存储，必须使用 id-gen 发的新 ID**，不要复用删过档的确定性 ID：新建实体的首个提交是“版本 0 的整文档 Put”，WAL 准入只看内存版本链、不能 I/O，撞上墓碑或已有文档的冲突要到投影才发现，那时记录已持久、回复已发出，只能 fail-stop（处置见 [KNOWN-LIMITS](../../maintenance/KNOWN-LIMITS.md)“删档后同 ID 重建：违反约束或修复前写下的冲突记录”）。Remote 托管实体删档物理删除数据文档，冷加载查 Remote 元数据墓碑区分删过档。删档审计：墓碑保留删除前字段、`_version`、`_deleted_at`（投影时间）与 `_last_tx`，“谁、为什么删”由删档入口自己记。

## 5. 外部驱动与结果未知

Redis/Mongo 写出后断线不意味着未执行。驱动只在证明命令未执行时重发，结果未知交给调用方。versionstore 用版本、写令牌、墓碑等表达 CAS 和重复；调用方仍须使用业务幂等键并保留未知结论。不要对未知写无限自动重放。

## 6. 维护与升级

要等 Mongo 落库，用明确 Flush/投影等待契约，不能只等 strict handler 成功。静态 Player 跨机迁移只以已落库数据为准，WAL 负责本机恢复与落库，不提供跨机本地 WAL 热迁移；按 sid 搬机的步骤与检查点见[参考流程](../PLAYER-MIGRATION.md)。旧版本升级先停写、排空、用旧程序完成需保留数据落库，保留备份；新程序用新 WAL 目录。框架不自动清库或迁移 schema。

只读故障分析使用 cmd/walinspect 与 nestwal 的检查/完整副本入口；不要用正在运行的权威 WAL 目录做破坏性实验。

### DataEngine 对 MongoDB 的最低要求

框架启动时只检查拓扑与会话（`ValidateDeployment`，见第二条），不检查服务端版本与参数（维护者 2026-10-30 决定不在启动时强制版本检查，改为提供只读检查工具，见本节末“部署前检查”）；下面三条是部署要求，每条写明代码里依赖它的地方。

- **MongoDB 5.0 及以上**。代码用到的特性最晚到 4.4：Go 驱动 v2.6.0 本身要求 4.2（驱动 `topology.MinSupportedMongoDBVersion`）；Put 单条投影是带 `$replaceWith` 的管道更新（4.2 起）；新建实体的首个提交通常同时 Put 多个 DAO 集合，走事务，在事务里第一次写某个集合时隐式建集合（4.4 起允许）；启动检查发 `hello` 命令（[driver/client.go](../../../infra/storage/mongo/driver/client.go) `ValidateDeployment`，5.0 引入，4.4.2 等补丁版回补）。在此之上部署下限定为 5.0（4.4 已于 2024-02 结束官方支持）。仓库的真实依赖验收用 mongod 8.0，7.0 另有一次实测（见下）。只在 MongoDB 上验证过，兼容 MongoDB API 的托管服务（如 AWS DocumentDB）不在验证范围内。
- **副本集**，单节点副本集即可。多 mutation 记录、批量投影、带回执 / effect / Remote 写的记录都在 Mongo 事务里写（`MongoStore.Project` / `ProjectBatch` → `StartSession` + `WithTransaction`，事务读关注 snapshot、写关注 majority + journal，[driver/client.go](../../../infra/storage/mongo/driver/client.go) `StartSession`），事务只在副本集或分片集群上可用。`mongo.require_replica_set`（缺省 `true`，[wiring/mongo/mongo_mod.go](../../../wiring/mongo/mongo_mod.go)）为 `true` 时 Mongo Mod 启动经 `ValidateDeployment` 拒绝单机（`hello` 没有 `setName` 且不是 mongos）；无论取值都要求服务端支持会话（`logicalSessionTimeoutMinutes`）。装配 DataEngine 的进程保持 `true`：DataEngine 不另外检查这个配置，写成 `false` 又连上单机时，事务投影在运行期失败并一直重试。
- **`transactionLifetimeLimitSeconds=20`**，写进 mongod.conf（`setParameter:` 下 `transactionLifetimeLimitSeconds: 20`）或启动参数 `--setParameter transactionLifetimeLimitSeconds=20`。被 kill -9 的进程留下的投影事务持锁到这个时限（服务端缺省 60s），服务端最晚在 1.5 倍时回收，其间重启排空写同一文档一直写冲突；`dataengine.startup_timeout` 缺省 60s 就是按 20s（最晚 30s 回收）加 30s 余量算的（[runtime.go](../../../framework/dataengine/engine/runtime.go) `drainAtStartup`）。DataEngine 不探测这个参数；saga 参与方启动时只读探测，超过步骤预算窗口时告警（[wiring/saga/steps.go](../../../wiring/saga/steps.go) `probeTransactionLifetime`），也不拒绝启动。完整说明见 [SAGA.md](../SAGA.md)“进程被强杀时遗留的 Mongo 事务”。

**MongoDB 7.0 实测（2026-10-30，macOS arm64）**：官方 tarball 7.0.45（FCV 7.0）与 brew 8.0.32（FCV 8.0）各起一个私有单节点副本集（`--setParameter transactionLifetimeLimitSeconds=20`，WiredTiger），配私有三节点 NATS、Redis 与 toxiproxy，跑同一组真实依赖用例（`-tags integration -race`）：`framework/dataengine/...`、`infra/storage/mongo/...`、`wiring/mongo`（含 R10 回归 `TestRealMongoPutStoresDollarStringsVerbatim`、遗留事务排空 `TestRealMongoStartupDrainOutlastsALeftoverTransaction`、单机拒绝 `TestRealMongoValidateDeploymentRefusesAStandalone`——7.0 一轮由 7.0 的 mongod 起这个单机——以及经 toxiproxy 的提交超时 / EndSession 用例），`framework/saga`（冷副本、步骤收件箱、参与方 kill -9 等），`wiring/dataengine`、`wiring/sidmerge`、`gameplay/movement`、`framework/remoteentity`，以及 `scripts/test-dataengine-generated.sh`（正式生成工程、三次独立进程）。两个版本结果相同（含子用例 1376 项通过、48 项跳过、0 失败，逐项一致）、R10 回归去掉 `$literal` 后两边同样变红，**没有发现 7.0 不支持的特性**。两边跳过的用例相同：要显式打开的性能探针与大积压恢复、子进程角色、需要 Redis Cluster 或 `mirror-local.sh` 的用例；停共享环境 Mongo 主节点 / NATS 的三个故障用例（`wiring/dataengine/failover_integration_test.go`）没有跑。命令与数字见 [v1.31.2 验收](../../release/v1.31.2-VALIDATION.md)。5.0、6.0 没有实测，4.4 只按代码与官方文档判断。

**部署前检查（`cmd/mongocheck`）**：只读核对一个部署是否满足上面的要求，给运维在生产 / staging 上跑。连接串只从环境变量读（缺省 `ROOST_MONGO_URI`，`-uri-env NAME` 改名），不接受命令行参数；输出与错误都不含连接串、主机名或账号（驱动错误只输出服务端错误名与错误码，其余按类别描述）。只发 `hello`、`buildInfo`、`getParameter`、`serverStatus` 四个只读命令；后两个需要 `clusterMonitor` 角色，权限不够时对应项是 WARN 并说明要什么权限。不需要 mongosh。

```bash
# 有 Go 时（本仓库，或任意引用了 roost-core 的工程里）：
ROOST_MONGO_URI='mongodb://<账号>:<口令>@<主机>:27017/?replicaSet=rs0&authSource=admin' \
  go run github.com/tjbdwanghaibo/roost-core/cmd/mongocheck
# 目标机器没有 Go：在本仓库交叉编译后拷过去（静态二进制，零运行期依赖）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mongocheck ./cmd/mongocheck
```

口令放进环境变量时注意 shell 历史（例如从密钥文件 `export ROOST_MONGO_URI="$(cat …)"`）。

服务进程的 Mongo Mod 同样可以从环境变量读连接串与账号（`mongo.uri_env` / `username_env` / `password_env`，加 `mongo.auth_source`，见 [Wiring §2.1](13-wiring.md#21-连接与凭据的环境变量_env)）。`mongocheck` 只读一个连接串变量：账号单独放在变量里的部署（例如 `MONGO_USERNAME` / `MONGO_PASSWORD`）跑它时，把账号拼进给它的连接串（`mongodb://<账号>:<口令>@…&authSource=admin`）。

| 检查项 | PASS | WARN | FAIL |
| --- | --- | --- | --- |
| `server-version`（`buildInfo`） | ≥ 5.0 | 4.4.x（满足代码硬底，已停止官方支持）、版本串无法解析 | < 4.4，或 `buildInfo` 失败 |
| `feature-compatibility-version`（`getParameter`） | ≥ 5.0 且与服务端主次版本一致 | 4.4、与服务端版本不一致（升级没做完）、读不到（权限不够 / 经 mongos） | < 4.4（事务里不能隐式建集合） |
| `topology`（`hello`，与 `ValidateDeployment` 同一规则） | 副本集（有 `setName`） | mongos：框架启动检查接受，但仓库没在分片集群上验证过 | 单机（输出改单节点副本集的步骤） |
| `logical-sessions`（`hello.logicalSessionTimeoutMinutes`） | 有 | — | 没有（`ValidateDeployment` 拒绝启动） |
| `transaction-lifetime-limit`（`getParameter`，只读到所连成员） | = 20 | < 20（不是验证过的取值）、读不到 | > 20（服务端缺省 60 时遗留事务最晚 90s 回收，超过 `dataengine.startup_timeout` 缺省 60s 所按的 30s） |
| `storage-engine`（`serverStatus.storageEngine`） | `wiredTiger` | 其他引擎、读不到、mongos 不报告 | `persistent: false`（DataEngine 的写关注带 `j: true`） |

退出码：0 没有 FAIL（可以有 WARN），1 至少一项 FAIL，2 用法错误或连不上（没有完成检查）。写关注 / 读关注的集群缺省值不检查：DataEngine 在客户端显式设定（majority + journal；读 majority、事务内 snapshot，[driver/client.go](../../../infra/storage/mongo/driver/client.go) `NewClient` / `StartSession`），不依赖服务端缺省。判定规则在 [cmd/mongocheck/check.go](../../../cmd/mongocheck/check.go) `Evaluate`，每条注释写明依据。

**单机改单节点副本集要点**（已有数据原地保留）：mongod.conf 加 `replication.replSetName: rs0`（同时按第三条写 `setParameter.transactionLifetimeLimitSeconds: 20`），重启 mongod；用 mongosh 执行一次 `rs.initiate({_id: "rs0", members: [{_id: 0, host: "<应用可达的主机名>:27017"}]})`（`host` 要写应用连得到的名字：驱动按它发现主节点，写 `localhost` 时别的机器连不上）；应用连接串加 `replicaSet=rs0`。开了认证（`security.authorization: enabled`）时副本集还要求成员间认证，单节点也要配 `security.keyFile`。改完再跑一次 `cmd/mongocheck`。

### 单实体运维命令（D16）

每个装配了 DataEngine 的进程自带 `entity.unload`，每个有玩家接入的进程自带 `entity.kick`，经 ops 的 `/admin/execute` 调用（`ops.admin_enabled` 与 admin token 照常生效，审计是 admin 注册表的 `admin command started / finished` 日志与 `admin.execute.total{command,outcome}`，不另设审批；维护者 2026-10-20 决定）。业务不用手写。两条都只作用于收到命令的那个进程：按 sid 绑定的玩家，发给持有它的 sid。

| 命令 | 参数 | 做什么 / 回答 |
| --- | --- | --- |
| `entity.kick` | `player_id`、`reason?` | 关闭该玩家在本进程的全部会话（嵌入 TCP 关连接；独立 Gate 部署下 Game Ingress 关绑定、Gate 随即断开客户端），关闭走与网络断开相同的路径（场景、聊天在线、自身视图照常清理）。回答 `closed`（本次关闭的会话数），没有会话时 `closed=0`。不改实体状态、不卸载、不封禁，玩家可以立刻重新登录 |
| `entity.unload` | `entity_id`（完整 Entity ID）或 `player_id` + `kind?`；`timeout?`（Go 时长，缺省 5s）、`reason?` | 先保证内存里每笔已提交的修改都已落库，再把实体从本进程内存卸载（不删数据），下一次访问按正常冷加载读回。回答 `entity_id`、`kind`、`resident`（命令开始时是否常驻）、`unloaded`、`drained`（等到投影完成的在途事务数）。没有常驻时成功、`resident=false`，什么也不做。只给 `player_id` 时在本进程已加载的实体里找唯一号相同的那一个，多个 kind 都加载着时要求写 `kind` |

`entity.unload` 的过程（`Runtime.UnloadEntity`，`framework/dataengine/engine/entity_unload.go`）：

1. 在业务池取得实体锁——正在执行、持有该实体的事务先照常结束（不被拒绝）——然后在 WAL 准入处登记卸载屏障。之后写到该实体的事务在准入处以可重试的 `dataengine.ErrEntityUnloading` 拒绝（同时满足 `nest.ErrCommitRejected`），Nest 在 Guard 内回滚；其他实体照常。
2. 等屏障之前已准入、写到该实体的记录投影完成（冷加载用的同一个屏障）。
3. 在业务池卸载（`ManagerAccess.Unload`，`DestroyReasonMemoryUnload`，与 Remote 持久拒绝后的卸载同一入口）。
4. 撤销屏障（任何结果都撤销）。

失败不强行卸载、实体不受影响：持有它的事务在 `timeout` 内没结束、已有另一次卸载在进行、Remote 托管实体的所有权处于 fenced / recovering / quarantined 或迁移中（由 Remote 运行时负责收尾）时返回“忙”（`ErrEntityUnloadBusy`）；投影在 `timeout` 内没完成（Mongo 慢或中断）返回 `ErrEntityUnloadTimeout`，记录留在 WAL 里照常投影；不是自动持久化的实体（库里没有它）拒绝。这些都是确定的“没有卸载”，ops 回 500 与原因，而不是 504 的“结果未知”。所有权稳定（local_owned / shared）的 Remote 托管实体与本地实体同一流程。

玩家在线、仍有 Sync 订阅时，卸载后框架会立刻为订阅者从库重载（`nest.unload_resync`）；要让实体真正离开内存，先 `entity.kick`。用途是让一个实体从库重建内存（例如怀疑非持久的派生状态有误）或释放内存；它**不是**改档入口：卸载之后，任何一次访问（玩家请求、实体定时器、saga 步骤、订阅者重载）都会立刻把实体重新加载，没有“卸载着、可以改库”的窗口；运行期仍不得直接改权威集合（§3）。

```bash
curl -s -H 'X-Admin-Token: <token>' -X POST http://127.0.0.1:9100/admin/execute \
  -d '{"name":"entity.kick","payload":{"player_id":10001,"reason":"ticket 42"}}'
curl -s -H 'X-Admin-Token: <token>' -X POST http://127.0.0.1:9100/admin/execute \
  -d '{"name":"entity.unload","payload":{"player_id":10001,"timeout":"10s","reason":"ticket 42"}}'
```

改档与整档恢复不进框架：在线改档写成业务的 GM handler（一条普通 Nest 消息，strict 写入，操作者与原因写进业务记录，参考 `service/account` 的 `ResolvePendingCreation`）；整档恢复走 §3 的离线流程。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](https://github.com/tjbdwanghaibo/roost-core/blob/132ed79f689df01dc8df0ea3a44e9a768ee126d9/docs/maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
