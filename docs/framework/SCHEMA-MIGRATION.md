# 业务 schema 变更操作手册

本篇是业务改动持久结构（DAO 字段改形状、拆分合并字段）时的离线迁移流程，维护者 2026-10-15 决定按此执行（[DESIGN-TODO D4](../maintenance/DESIGN-TODO.md#d4-业务-schema-离线迁移的模板--流程)）。完整可运行的示例在 game-demo 模板里：[db/migrations](../../demo/db/migrations/player.go.tmpl)、[离线执行器](../../demo/db/migrations/offline.go.tmpl)、[cmd/schemamigrate](../../demo/cmd/schemamigrate/main.go.tmpl)，测试随生成工程运行。

## 1. 现行规则：运行时只认当前 schema

- DAO 标记 `//roost:dao coll=player db=planet schema=2`（`db=` 默认是工程名）声明当前 schema，生成常量 `PlayerDaoSchemaVersion`；投影器写文档时带 `_schema`（[mongo_projection.go](../../framework/dataengine/engine/mongo_projection.go)）。
- 冷加载先校验聚合里**全部** DAO 的 `_schema` 等于当前版本，再水合与发布实体（[entity_repository.go](../../framework/dataengine/engine/entity_repository.go) 的 `checkAggregateSchemas`）；生成的 `RestorePersisted` 在解码前再校验一次。旧版本、未来版本都以 `dataengine.ErrSchemaMismatch` 拒绝，**不自动转换、不写回**。
- 通用工具 [`infra/storage/migration`](../../infra/storage/migration/migration.go) 只负责“步骤登记与按版本链执行”（`Registry.Register` / `RunFrom`），不接入加载路径。
- 墓碑（`_deleted: true`）在冷加载时读作“删过档”（`ErrEntityAggregateDeleted`，D10），不水合、不检查 schema。

- **持久键**（维护者 2026-10-19 决定，[DESIGN-TODO D13](../maintenance/DESIGN-TODO.md#d13-dao-持久键的来源与水合时的键集合校验)）：DAO 定义里字段写了 `bson:"x"` 时 `x` 就是持久键，没写时是 Go 字段名的 snake_case。键要字母开头、只含字母数字下划线，不能以 `_` 开头（留给 `_id`、`_schema`、`_version` 等元数据），同一类型内不能重复；`bson` 选项只认 `omitempty`，`bson:"-"` 报错。违反时 `roost generate` 报错。生成的 `gen_<x>_dao.go` 里有一个持久文档结构体 `<dao>PersistDoc`（嵌套类型是 `<type>BSONDoc`），每个字段的显式 `bson` 标签就是它的存储键；整文档写入、水合、回滚快照、增量补丁都用这些键。
- **要改 Go 字段名又保住数据：保留原来的 `bson` 名**（``Coins int64 `bson:"gold"` ``），键没变，不需要迁移，生成期与加载期都不告警。新字段建议一开始就写 `bson` 名（`roost add dao` 骨架的注释就是这样写的）。
- **改字段会告警，不拒绝**：
  - 生成期：`roost generate` 把工作树里上一次的生成物当基线，持久键消失（改名没留 `bson` 名、删字段、`persist` 改 `nopersist`）、新增、类型变化各打印一条 `WARN: <Dao>: stored key "gold" (Gold int64) is no longer generated ...`，写明后果并提示 `schema=` 是否提高。基线只是工作树里的文件：再生成一次不再告警。
  - 加载期：生成的 `RestorePersisted` 发现库里有、持久文档结构体没有的键时打印 `dataengine: stored document has a key the DAO does not declare`（`dao`、`key`、`first_id`；同一进程同一 DAO 同一键只打一次，嵌套路径里的 map 键与下标写作 `*`），计数 `dataengine.dao.unknown_stored_key.total{dao}`，照常加载但**不读入**这个键的值。缺失的键给零值（加字段的正常情况），不告警。
- **旧键不再加载，整文档写回即删除**（维护者 2026-10-19：“旧字段就不 load 了，给一个业务层的约束或者通知就可以了”；同日撤销先前的“整文档写回保留旧键”方案 A）：库里有、DAO 未声明的键（顶层与嵌套子文档）不读入、不在内存里携带，下一次整文档写回（含 Remote 托管实体的 data 信封替换）就把它从文档里去掉；单字段补丁（`$set`）不碰它，它留在库里但不再被读取，需要清掉时用一次性离线迁移。
- **业务层约束（改名 / 删字段前必读）**：要么保留原 `bson` 名，要么**先离线迁移再提升 `schema=`**（§2）；否则旧数据不再加载，下一次整文档写回即丢失。框架只负责通知（生成期与加载期的 WARN、计数），不替业务保留旧值。

所以 schema 变更是一次**运维操作**：停写之后把库里的旧文档一次性转成当前 schema，再启动只认当前 schema 的新程序。加字段不需要迁移（BSON 解码给零值），但仍要按需提高 `schema=`；改形状（例如扁平字段变成子文档）、改名没保留 `bson` 名、删字段而旧值仍有用，都要在新程序启动**之前**迁移：把旧键的值搬到新键，迁移步骤里一并 `$unset` 旧键（新程序不读旧键，整文档写回也会删掉它）。

## 2. 流程

| 步骤 | 做什么 | 用到的现有能力 | 检查点（不满足不进入下一步） |
| --- | --- | --- | --- |
| 0 准备 | 新程序把 `schema=` 加 1 并 `roost generate`；为每个改形状的 DAO 写一个 `migration.Step`（只读旧版本真的有的字段）与一个 `Check`（用生成 DAO 的 `RestorePersisted` 按新 schema 解码 + 业务不变量） | `infra/storage/migration`；生成 DAO | 步骤与校验的单元测试通过（示例：`db/migrations/player_test.go`） |
| 1 停写 | 所有写这些集合的进程按正常停机流程退出：Service.Shutdown 后各 Mod 逆序停止，DataEngine 停机先 `Projector.Flush`（WAL fsync + 回放到没有可处理记录）再关闭，单实例锁最后释放 | `Runtime.Shutdown`（[runtime.go](../../framework/dataengine/engine/runtime.go)）；App 单实例锁（[singleton.go](../../framework/app/singleton.go)） | 进程已退出；每个 sid 的 WAL 已全部确认（`cmd/sidmove wal-drained`，见 [Player 跨机迁移](PLAYER-MIGRATION.md#3-检查工具)）；单实例锁键不存在（`cmd/sidmove singleton`）。异常退出的进程先在原 WAL 目录上重启恢复、再正常停机，不能直接迁移 |
| 2 备份 | 整库备份（`mongodump` 或存储快照）；执行器另外把每份被改写文档的原样副本写进备份集合 | Mongo 运维；执行器 `Backup` 集合 | 备份可读、可恢复到隔离库 |
| 3 dry-run | `schemamigrate plan`：读全部文档、转换并 `Check`，不写 | 执行器 `Job.Plan` | `pending` 为待迁移数、`converted` 等于它；任何一份失败整次拒绝 |
| 4 转换 | `schemamigrate apply`：先完整 plan，全部通过才逐份“写备份 → 条件改写”。条件是 `_id` + 原 `_version` + 原 `_schema`；改写后 `_schema` 为新版本、`_version` 加 1、`_last_tx` 记为 `schema-migration:<步骤名>` | 执行器 `Job.Apply` | 条件不成立（有人还在写）以 `ErrNotStopped` 中止；中断后重跑从剩余文档继续 |
| 5 校验 | `schemamigrate verify`：没有旧版本活文档，每份新版本文档都能被 `Check` 接受 | 执行器 `Job.Verify` | `pending` 为 0、退出码 0 |
| 6 切换版本 | 启动新程序（新 WAL 目录或已全部确认的原目录）；抽样读写验证 | 冷加载的 schema 校验 | 无 `ErrSchemaMismatch`；首批写入的 `_version` 从迁移后的版本继续 |
| 7 回退 | **新程序写入之前**：`schemamigrate rollback` 用备份集合原样恢复（只恢复仍是迁移写的那一版），旧程序可再启动。**新程序写入之后**：迁移后被写过的文档列为 `refused`、不覆盖；此时只能向前修复，或停写后恢复整库备份并接受丢失备份之后的写入，由业务决定 | 执行器 `Job.Rollback`；整库备份 | `rolled_back` 等于迁移数；有 `refused` 时退出码非零 |

为什么改写时 `_version` 加 1：投影器的条件写只比较 `_id` 与 `_version`（`mutationFilter`），不看 `_schema`。若迁移保持原 `_version`，一个没停干净的旧进程仍能按旧版本条件把旧 schema 写回去；加 1 之后它的条件必然失败、投影以冲突 fatal 暴露，而不是静默覆盖。新程序从加 1 后的版本继续，`_last_tx` 标记让回退和审计能区分“迁移写的版本”与“业务写的版本”。

## 3. 执行器的边界

- 只处理 `From` 与 `To` 两个版本；集合里出现别的 schema、缺 `_version` / `_schema`、带 `_ver` 的 Remote 版本信封，在**写任何东西之前**整次拒绝。Remote 托管实体的版本、所有权与 fence 由 Remote 链路维护，不能用这个执行器改写；需要时另行设计。
- 一个聚合有多个持久 DAO 时，每个改了形状的 DAO 各跑一个 Job；它们都在停写期间完成、全部 verify 通过才启动新程序。
- 不在线双读：迁移期间业务停写。上线后若需要在线迁移（双读期、灰度），需要重新决定（维护者 2026-10-06 起“线上未部署不做兼容”的规则在上线后作废）。
- 生成器层面的支持（`roost add migration <name>` 生成步骤骨架、幂等标记、校验函数与 dry-run，接入 doctor）是候选，尚未实施，视业务使用频率再定。

## 4. 示例与验证

game-demo 的 Player 从 schema 1 到 2：v1 的扁平 `weapon_id` 变成 v2 的 `equipment.slots`（槽位 1）。

```sh
# 1 停写之后（所有 game 进程已正常退出）
go run ./cmd/schemamigrate -mongo "$URI" plan      # dry-run
go run ./cmd/schemamigrate -mongo "$URI" apply     # 备份 + 条件改写
go run ./cmd/schemamigrate -mongo "$URI" verify    # 切换前验收
# 新程序写入之前发现问题：
go run ./cmd/schemamigrate -mongo "$URI" rollback
```

Player 文档所在的物理库按 `-config`（缺省 `configs/service/config.game.yaml`）的 `dataengine.databases` 把 Player DAO 的逻辑库 `db=main` 换算出来，与 game 进程同一条规则（`kitdataengine.LoadDatabaseMapFile`）；`-db` 可直接给物理库名。迁移期间不要改映射：改映射本身是一次数据搬迁，见 [PLAYER-MIGRATION §5](PLAYER-MIGRATION.md#5-改-dao-库名映射)。

测试覆盖（生成工程的 `db/migrations/player_test.go`）：步骤对各种数字拼写与无武器文档的处理；plan 不写、apply 备份原样并推进版本、结果能被生成 DAO 按当前 schema 加载、重复 apply 不再改写、墓碑不动；任一份校验失败或遇到未知 schema / Remote 信封时一份都不写；有写入者没停时 `ErrNotStopped` 且不覆盖对方的写；回退只恢复迁移写的版本、之后被写过的文档列为 refused。`TestPlayerMigrationRealMongo` 在真实副本集上跑同一流程，由 [scripts/test-demo-generated.sh](../../scripts/test-demo-generated.sh) 选中。改错条件写、去掉写前 plan、去掉回退条件三种变体都会让对应用例变红（2026-10-15 在生成工程上实测）。

### 2026-10-15 真实 Mongo 演练

环境：本机私有隔离环境（`dataengine-env.sh`，端口偏移 12000，Mongo 三副本），当前分支生成的 game-demo，game 与九个支撑服务真实进程。

1. 种入 3 份 schema 1 的 Player 文档（两份带 `weapon_id`）。新程序启动后 `gm.player.add_item` 被拒绝：`dataengine: persisted schema does not match runtime schema: resource=player entity=2049028 stored_schema=1 target_schema=2`。
2. 停 game：进程退出，单实例锁键不存在，WAL 全部确认。
3. `plan`：pending 3、converted 3、written 0；`apply`：written 3、pending 0、at_target 3；`verify` 退出码 0。
4. 新程序启动，两个玩家的 `add_item` 成功（count 4）；Mongo 中玩家 2001 为 `_schema 2`、`_version 3`、`equipment.slots.1.item_id 7001`、无 `weapon_id`；未被写过的玩家 2003 仍是 `_version 2`、`_last_tx schema-migration:player_weapon_to_equipment`。
5. 停 game 后 `rollback`：rolled_back 1（玩家 2003），refused 2（玩家 2001、2002 已被新程序写到 `_version 3`），退出码 1；再次 `apply` 只改写玩家 2003（written 1），`verify` 通过。

演练中一次进程控制脚本把子 shell 的 PID 当成 game 的 PID，“停写”实际没有发生；`sidmove wal-drained` 以“a process still holds this WAL”拒绝，才发现旧进程仍在。修正脚本后整段重跑，上面是重跑的结果。这说明第 1 步的检查点不能省：只看停机命令返回不足以证明停写。
