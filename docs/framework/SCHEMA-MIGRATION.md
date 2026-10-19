# 业务 schema 变更操作手册

本篇是业务改动持久结构（DAO 字段改形状、拆分合并字段）时的离线迁移流程，维护者 2026-10-15 决定按此执行（[DESIGN-TODO D4](../maintenance/DESIGN-TODO.md#d4-业务-schema-离线迁移的模板--流程)）。完整可运行的示例在 game-demo 模板里：[db/migrations](../../demo/db/migrations/player.go.tmpl)、[离线执行器](../../demo/db/migrations/offline.go.tmpl)、[cmd/schemamigrate](../../demo/cmd/schemamigrate/main.go.tmpl)，测试随生成工程运行。

## 1. 现行规则：运行时只认当前 schema

- DAO 标记 `//roost:dao coll=player db=planet schema=2`（`db=` 默认是工程名）声明当前 schema，生成常量 `PlayerDaoSchemaVersion`；投影器写文档时带 `_schema`（[mongo_projection.go](../../framework/dataengine/engine/mongo_projection.go)）。
- 冷加载先校验聚合里**全部** DAO 的 `_schema` 等于当前版本，再水合与发布实体（[entity_repository.go](../../framework/dataengine/engine/entity_repository.go) 的 `checkAggregateSchemas`）；生成的 `RestorePersisted` 在解码前再校验一次。旧版本、未来版本都以 `dataengine.ErrSchemaMismatch` 拒绝，**不自动转换、不写回**。
- 通用工具 [`infra/storage/migration`](../../infra/storage/migration/migration.go) 只负责“步骤登记与按版本链执行”（`Registry.Register` / `RunFrom`），不接入加载路径。
- 墓碑（`_deleted: true`）在冷加载时读作“删过档”（`ErrEntityAggregateDeleted`，D10），不水合、不检查 schema。

- **持久键是 Go 字段名的 snake_case**（生成器 `codegen/internal/dao/gen.go` 的 `bsonKey`），字段上的 `bson:"..."` 标签**不参与**。改持久字段的 Go 名就是改它的存储键：老文档里的旧键在水合时被静默忽略、读成零值，之后的整文档写入会删掉它。`roost generate` 对 `bson` 名与持久键不同的字段打印 WARN；规则是否改为“显式名优先 / 水合时拒绝未知键”待维护者决定（[DESIGN-TODO D13](../maintenance/DESIGN-TODO.md#d13-dao-持久键的来源与水合时的键集合校验)）。在那之前：改名、删字段、`persist` 改 `nopersist` 都按“改形状”走下面的迁移流程。

所以 schema 变更是一次**运维操作**：停写之后把库里的旧文档一次性转成当前 schema，再启动只认当前 schema 的新程序。加字段不需要迁移（BSON 解码给零值），但仍要按需提高 `schema=`；改形状（例如扁平字段变成子文档）必须迁移。

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

测试覆盖（生成工程的 `db/migrations/player_test.go`）：步骤对各种数字拼写与无武器文档的处理；plan 不写、apply 备份原样并推进版本、结果能被生成 DAO 按当前 schema 加载、重复 apply 不再改写、墓碑不动；任一份校验失败或遇到未知 schema / Remote 信封时一份都不写；有写入者没停时 `ErrNotStopped` 且不覆盖对方的写；回退只恢复迁移写的版本、之后被写过的文档列为 refused。`TestPlayerMigrationRealMongo` 在真实副本集上跑同一流程，由 [scripts/test-demo-generated.sh](../../scripts/test-demo-generated.sh) 选中。改错条件写、去掉写前 plan、去掉回退条件三种变体都会让对应用例变红（2026-10-15 在生成工程上实测）。

### 2026-10-15 真实 Mongo 演练

环境：本机私有隔离环境（`dataengine-env.sh`，端口偏移 12000，Mongo 三副本），当前分支生成的 game-demo，game 与九个支撑服务真实进程。

1. 种入 3 份 schema 1 的 Player 文档（两份带 `weapon_id`）。新程序启动后 `gm.player.add_item` 被拒绝：`dataengine: persisted schema does not match runtime schema: resource=player entity=2049028 stored_schema=1 target_schema=2`。
2. 停 game：进程退出，单实例锁键不存在，WAL 全部确认。
3. `plan`：pending 3、converted 3、written 0；`apply`：written 3、pending 0、at_target 3；`verify` 退出码 0。
4. 新程序启动，两个玩家的 `add_item` 成功（count 4）；Mongo 中玩家 2001 为 `_schema 2`、`_version 3`、`equipment.slots.1.item_id 7001`、无 `weapon_id`；未被写过的玩家 2003 仍是 `_version 2`、`_last_tx schema-migration:player_weapon_to_equipment`。
5. 停 game 后 `rollback`：rolled_back 1（玩家 2003），refused 2（玩家 2001、2002 已被新程序写到 `_version 3`），退出码 1；再次 `apply` 只改写玩家 2003（written 1），`verify` 通过。

演练中一次进程控制脚本把子 shell 的 PID 当成 game 的 PID，“停写”实际没有发生；`sidmove wal-drained` 以“a process still holds this WAL”拒绝，才发现旧进程仍在。修正脚本后整段重跑，上面是重跑的结果。这说明第 1 步的检查点不能省：只看停机命令返回不足以证明停写。
