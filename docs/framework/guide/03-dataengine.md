# 核心：DataEngine 持久化：设计与使用

适用版本：v1.29.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/03-dataengine.md) · [模块总目录](../README.md)

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

**运行期不得直接改权威集合**：加载着的实体以内存为权威，投影按 `_id` + `_version == expected` 条件写（`MongoStore`，`framework/dataengine/engine/mongo_store.go`）。在进程运行时直接改 Mongo 文档：只改字段、不动 `_version` 时版本仍匹配，下一次投影按内存状态覆盖改动（静默丢失）；改了 `_version`（如恢复旧备份）时投影 `ErrProjectionConflict` fatal、进程 fence，重启时启动投影恢复仍冲突、起不来。在线改档写成业务的 GM handler（一条普通 Nest 消息，走正式提交链路）；整档恢复先停该 sid 的进程并确认 WAL 已排空（game-demo `cmd/sidmove wal-drained`），再写回文档，启动后冷加载以库里的版本继续。冲突后的识别与处置见[维护手册 §4](../../maintenance/README.md#4-故障处理)。框架只提供与业务字段无关的两条运维命令 `entity.kick` / `entity.unload`（[§6 单实体运维命令](#单实体运维命令d16)），改档与恢复不进框架（[DESIGN-TODO D16](../../maintenance/DESIGN-TODO.md#d16-单个实体--玩家的运维入口)，维护者 2026-10-20 决定 C）。

投影器可重放同一已提交记录，外部副作用必须通过稳定身份去重。WAL checkpoint 只决定崩溃后从哪里重放：投影器按条数 / 间隔（`dataengine.projection.checkpoint_records` / `checkpoint_interval`，缺省 256 条 / 20 ms）合并确认，带回执 / effect 的记录（saga 原生步骤）与普通记录一样合并（2026-10-18 起）。这类记录逐条投影约 16 ms，缺省 20 ms 间隔下积压时合并系数约 2；维护者 2026-10-19 决定缺省保持 20 ms，需要更高吞吐的部署可调大 `checkpoint_interval`，代价是崩溃后重放窗口与未确认额度变长（重放仍幂等）。崩溃后会多重放这段已投影的记录，由事务标记判定为已应用，lease fence、回执与 effect 都保持首投的结果。只有单 mutation 快路记录仍逐条确认。effect 暂存进 outbox 后投影器立即唤醒 outbox 发布（2026-10-18 起），`dataengine.outbox.poll_interval`（缺省 100ms）只是兜底轮询，不再决定 effect 的发布延迟。WAL backlog、磁盘/年龄健康阈值、outbox 硬容量分别处理，不能统称任一超限都直接 fail-stop。启用前核对容量与保留期，receipt/事务标记不能在仍有重放可能时提前过期。

**消费 effect（事务性 outbox 的消费端）**：业务用 `wiring/dataengine` 的 `SubscribeEffects(ctx, registry, EffectConsumer{Durable, Topic}, handler)`（D12 方向 A，维护者 2026-10-18 决定）。效果流、主题前缀与回执库按 DataEngine Mod 的声明（`dataengine.effects.*`、`dataengine.database`）取，回执集合缺省每个 durable 一个（`_effect_inbox_<durable>`）。语义：至少一次投递、按 EffectID 至多一次生效——handler 在 Mongo 事务里运行，经传入 ctx 的 Mongo 写与回执原子提交，再到达读到回执就跳过；Mongo 驱动可能重跑事务回调，事务外的副作用（调用别的服务）按 EffectID 幂等（demo 升级邮件把它当 mail 的 RequestID）。handler 返回普通错误 → 不写回执、按退避 nak 重投（缺省 250ms..30s，MaxDeliver 25000）；返回 `PoisonEffect(err)`、信封解不开或同一 EffectID 内容不同 → 直接 Term 不重投、ERROR 日志并计 `dataengine.effect_consumer.handled_total{result="poison"}`（非零要人核对）；handler 的 ctx 以 AckWait（缺省 30s）为截止。回执 TTL（缺省 30 天）不长于 `dataengine.effects.max_age` 时拒绝订阅。

**逻辑库名与物理库名**（[DESIGN-TODO D9](../../maintenance/DESIGN-TODO.md#d9-dao-库名不随配置_id-是编码后的-entityid) 修订版 A，维护者 2026-10-20 决定）：DAO 标记的 `db=` 是**逻辑库名**（小写字母开头，小写字母、数字、下划线；骨架写 `db=main`），`DocumentKey.Database`、WAL 记录与 Remote 提交里的库名都是它。Mongo 里的物理库名只由部署决定，启动时合成一张冻结的表（`dataengine.DatabaseMap`），之后只查表、不再算：

- **两个入口**：配置 `dataengine.databases`（逻辑名 → 物理名，走配置声明、`--check-config` 与 doctor 的同一份规则）；代码回调 `kitdataengine.WithDatabaseResolver(func(logical string) (string, error))`，DataEngine Mod Init 时对代码声明的每个逻辑库各调用一次。**同一逻辑库只能有一个来源**（维护者 2026-10-20 决定）：配置没写的逻辑库取回调返回的非空物理名；配置写了的，回调返回空串（沿用配置）或相同的值都允许，返回不同的非空值拒绝启动并点名逻辑库与两个值。二者都没有时是逻辑名本身。回调只在 Init 调用，所以这条冲突检查也只在 Init 发生，`--check-config` 与 doctor 发现不了。
- **声明**：生成的 `db/gen_databases.go` 每个逻辑库一个常量（`DatabaseMain = "main"`，各 DAO 的 `<Dao>DBName` 引用它），`init` 里 `dataengine.DeclareDatabases` 登记（是否有 DAO 按 sid 分库也一起登记）。
- **没有映射**：表已合成、但声明了却没有配置也没有回调结果的逻辑库，物理名就是逻辑名本身（启动日志告警），与引入映射之前的行为一致；这条只针对表里某个逻辑库没配物理名，Remote 存储根本没绑定表时直接报错（见下面“唯一解析点”）；生成配置总是写 `main: <工程名派生的库>`，新工程开箱不会落到名为 `main` 的库。
- **一次校验**（Init 时，失败拒绝启动并点名）：配置里出现代码未声明的逻辑名（拼写错误；二进制一个逻辑库都没声明时无从判断，只告警）；物理名为空、含 Mongo 禁止的字符、是系统库（admin / local / config）、按 sid 分库时加上 `_<sid>` 超过 63 字节；两个逻辑库映射到同一个物理库（大小写不同也算，不提供允许开关）；回调与配置冲突；回调返回错误。
- **唯一解析点**：投影（单条与批量）、冷加载、删档墓碑经 `MongoStore` 的 `physicalDatabase`；Remote 写 DAO 文档经 `MongoCommitter.dataDB`（DataEngine Mod 在 Provide 时把同一张表绑定给 Remote 存储；**无论 wiring 装配还是直接 `NewMongoCommitter` 自建装配，没绑定映射表就写带库名的 DAO 文档一律返回 `remoteentity.ErrDatabaseMapNotBound`**，不按逻辑名写进名为 `main` 之类的库，维护者 2026-10-21 决定。自建装配在第一次提交前用投影所用的同一张表调 `Backend.BindDatabaseMap` 或 `MongoCommitter.BindDatabaseMap`，测试可用 `dataengine.NewDatabaseMap(dataengine.DatabaseMapConfig{})` 合成一张不配映射的表）；业务与工具经 `kitdataengine.DAOCollection` / `Databases(registry)`（能力 `dataengine.databases`）/ `LoadDatabaseMapFile`。根包 `TestNoMongoDatabaseOpenedWithALogicalDAOName` 守住产品代码与模板里没有把 `DbName()` / `<Dao>DBName` 直接交给 `.Database(`。
- **改映射等于搬数据**：WAL 只记逻辑名、投影时才换算；上次启动的映射记在 WAL 目录的 `databases.json`，还有未投影记录而映射变了（含没有该文件的旧 WAL 配非 identity 映射）的进程以 `ErrDatabaseMappingChanged` 拒绝启动。顺序是停写 → 排空 WAL → 离线迁移数据 → 改映射启动（[PLAYER-MIGRATION §5](../PLAYER-MIGRATION.md#5-改-dao-库名映射)）。同一部署里所有会写同一批文档的进程（含跨进程写 Remote 实体的）映射必须一致。

## 4. 冷加载与 schema

EntityRepository 读取聚合一致性视图，先验证所有持久 DAO 的完整性与当前 schema，再水合并发布 Entity。旧/未来 schema 都拒绝；生成 RestorePersisted 在解码前校验。自动 schema 迁移已撤销，不能把仍存在的通用 migration 包解释成 Repository 自动转换旧 DAO 的能力。改了持久结构时按[业务 schema 变更操作手册](../SCHEMA-MIGRATION.md)停写后离线转换。

恢复 gate 未就绪时不能对外发布“空的已恢复实体”。并发加载合并 flight；发布后回调重新连接 Sync 等协作者。缓存只是加速，缺失/过期不得被解释成权威不存在。

**删档后该 ID 不可再用**（维护者 2026-10-17 决定，[DESIGN-TODO D10](../../maintenance/DESIGN-TODO.md#d10-删档后用同一-id-重建)）。`Destroy(deletePersisted=true)` 留下墓碑后，冷加载得到“删过档”（`entity.ErrAuthorityEntityDeleted` / `ErrEntityAggregateDeleted`，客户端码 `entity_deleted = 3`），它与“从未创建”（`ErrAuthorityEntityNotFound`，码 2）是两个哨兵，**不满足** not found：“不存在就创建”的代码不会去重建。范围只守 GetOrCreate（维护者同日决定）：生成 lifecycle 的 GetOrCreate 不重建、发往该 ID 的 Nest 请求回复码 3。**直接 Create（handler 内或 lifecycle `Create`）不读存储，必须使用 id-gen 发的新 ID**，不要复用删过档的确定性 ID：新建实体的首个提交是“版本 0 的整文档 Put”，WAL 准入只看内存版本链、不能 I/O，撞上墓碑或已有文档的冲突要到投影才发现，那时记录已持久、回复已发出，只能 fail-stop（处置见 [KNOWN-LIMITS](../../maintenance/KNOWN-LIMITS.md)“删档后同 ID 重建：违反约束或修复前写下的冲突记录”）。Remote 托管实体删档物理删除数据文档，冷加载查 Remote 元数据墓碑区分删过档。删档审计：墓碑保留删除前字段、`_version`、`_deleted_at`（投影时间）与 `_last_tx`，“谁、为什么删”由删档入口自己记。

## 5. 外部驱动与结果未知

Redis/Mongo 写出后断线不意味着未执行。驱动只在证明命令未执行时重发，结果未知交给调用方。versionstore 用版本、写令牌、墓碑等表达 CAS 和重复；调用方仍须使用业务幂等键并保留未知结论。不要对未知写无限自动重放。

## 6. 维护与升级

要等 Mongo 落库，用明确 Flush/投影等待契约，不能只等 strict handler 成功。静态 Player 跨机迁移只以已落库数据为准，WAL 负责本机恢复与落库，不提供跨机本地 WAL 热迁移；按 sid 搬机的步骤与检查点见[参考流程](../PLAYER-MIGRATION.md)。旧版本升级先停写、排空、用旧程序完成需保留数据落库，保留备份；新程序用新 WAL 目录。框架不自动清库或迁移 schema。

只读故障分析使用 cmd/walinspect 与 nestwal 的检查/完整副本入口；不要用正在运行的权威 WAL 目录做破坏性实验。

### 单实体运维命令（D16）

每个装配了 DataEngine 的进程自带 `entity.unload`，每个有玩家接入的进程自带 `entity.kick`，经 ops 的 `/admin/execute` 调用（`ops.admin_enabled` 与 admin token 照常生效，审计是 admin 注册表的 `admin command started / finished` 日志与 `admin.execute.total{command,outcome}`，不另设审批；维护者 2026-10-20 决定）。业务不用手写。两条都只作用于收到命令的那个进程：按 sid 绑定的玩家，发给持有它的 sid。

| 命令 | 参数 | 做什么 / 回答 |
| --- | --- | --- |
| `entity.kick` | `player_id`、`reason?` | 关闭该玩家在本进程的全部会话（嵌入 TCP 关连接；独立 Gate 部署下 Game Ingress 关绑定、Gate 随即断开客户端），关闭走与网络断开相同的路径（场景、聊天在线、自身视图照常清理）。回答 `closed`（本次关闭的会话数），没有会话时 `closed=0`。不改实体状态、不卸载、不封禁，玩家可以立刻重新登录 |
| `entity.unload` | `entity_id`（完整 Entity ID）或 `player_id` + `kind?`；`timeout?`（Go 时长，缺省 5s）、`reason?` | 先保证内存里每笔已提交的修改都已落库，再把实体从本进程内存卸载（不删数据），下一次访问按正常冷加载读回。回答 `entity_id`、`kind`、`resident`（命令开始时是否常驻）、`unloaded`、`drained`（等到投影完成的在途事务数）。没有常驻时成功、`resident=false`，什么也不做。只给 `player_id` 时在本进程已加载的实体里找唯一号相同的那一个，多个 kind 都加载着时要求写 `kind` |

`entity.unload` 的过程（`Runtime.UnloadEntity`，`framework/dataengine/engine/entity_unload.go`）：

1. 在业务池取得实体锁——正在执行、持有该实体的事务先照常结束（不被拒绝）——然后在 WAL 准入处登记卸载屏障。之后写到该实体的事务在准入处以可重试的 `dataengine.ErrEntityUnloading` 拒绝（同时满足 `nest.ErrCommitRejected`），Nest 在 Guard 内回滚；其他实体照常。
2. 等屏障之前已准入、写到该实体的记录投影完成（冷加载用的同一个屏障）。
3. 在业务池卸载（`ManagerAccess.Unload`，`DestroyReasonMemoryUnload`，与驱逐同一入口）。
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

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
