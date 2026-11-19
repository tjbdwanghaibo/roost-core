# Mongo 库名前缀（2026-11-19）

起因：维护者发现同一台机器上的数据库撞名。决定：“在框架层加一个前缀能力，这个前缀统一配置在配置中，对于 skill 生成的模式这个配置变成项目名”；不放在 MongoDB 客户端底层，而是上层封装一个使用能力；前缀关掉时所有库名都只是业务层自定义的名字。

本机核对（改之前）：roost 生成的工程（`lnx_light_between_us_server`，Mongo 27027）库名已带工程名；共用的 brew Mongo（27017）上没前缀的 `account game gm player_profile player_report skill_cache` 来自老工程 `ai-game-blackbox-server` 的手写配置。缺口是库名各处各自派生工程名（`dataengine.database`、`databases.main`、`saga.database`、`remote_entity.mongo.database`），业务新加的库没有统一规则。

## 做法

- `mongo.database_prefix`（`wiring/mods.MongoDatabasePrefix`，Mongo Mod 与 DataEngine 共用一份声明）。
- Mongo Mod 把驱动客户端用 `wiring/mongo.WithDatabasePrefix` 封装后注册：`Database(name)` → `<前缀>_<name>`，`DatabaseForSid(base, sid)` → `<前缀>_<base>_<sid>`，系统库原样。驱动与 `infra/storage/mongo` 不变。
- 合服工具 `wiring/sidmerge` 直接用驱动：`kitmongo.DatabasePrefix(cfg)` + `mods.PrefixedDatabase` 算库名（`side.physical`、控制库）。
- DataEngine：`--check-config` 与 Init 按加前缀之后的名字核对 63 字节。
- 生成器：前缀写工程名（`{project_db}`，截到 40 字节），库名写 `game` / `{main: game}` / `saga` / `remote_entity`。

## 修前红（新用例放在改动前的 HEAD 5cbde701 上跑）

```
$ GOWORK=off go test -count=1 -run TestMongoModHandsOutAClientThatPrefixesEveryBusinessDatabase ./wiring/mongo/
    database_prefix_test.go:39: "mongo:\n  database_prefix: proj\n": database names = ["game" "game_1000" "admin" "local" "config"], want ["proj_game" "proj_game_1000" "admin" "local" "config"]
FAIL

$ GOWORK=off go test -count=1 -run TestDatabaseNamesMustFitWithTheMongoPrefix ./wiring/dataengine/
    databases_test.go:310: 40-byte prefix + 20-byte database + _1000: Init = <nil>; want a refusal naming the logical database and mongo.database_prefix
FAIL
```

## 修后

- `GOWORK=off go test -count=1 ./wiring/... ./framework/dataengine/... ./infra/storage/mongo/...`：全部 ok（含新用例 `TestMongoModHandsOutAClientThatPrefixesEveryBusinessDatabase`、`TestWithDatabasePrefixWrapsAnyClient`、`TestMongoInitRefusesABadDatabasePrefix`、`TestDatabaseNamesMustFitWithTheMongoPrefix`、`TestSidePhysicalAddsTheMongoDatabasePrefix`）。
- `GOWORK=off go test -count=1 -run 'TestDAOSkeletonDatabaseFollowsTheProjectName|TestGeneratedSharedResourceNamesFollowTheProject|TestConfigContract|TestKitConfig|Schema' ./codegen/internal/roost/`：ok（生成配置写 `mongo.database_prefix: <工程名>` 与不带前缀的库名）。
- 根门禁 `GOWORK=off go test -count=1 .`：ok。
- 生成器整包（`./codegen/internal/roost`，约 33 分钟）提交后单独跑，结果写在提交说明之后的汇报里。

## 没验证的

- 真实 Mongo 上的端到端（生成工程起服后库名为 `<工程名>_game` 等）只由生成器整包里的真实依赖用例覆盖；本机隔离环境的手动起服没做。
- 已有工程的数据迁移不提供工具（线上未部署，不做兼容）：照 CHANGELOG 升级说明手工处理。

## 评审 A/B 处理

同日评审（分支 `review/main-server-followup` 上的 `REVIEW-2026-11-19-mongo-database-prefix.md`）指出 b05af487 的两处偏差：前缀只在 Mongo Mod 交出的客户端里加，WAL 映射核对与裸驱动工具仍把不带前缀的映射库名当最终库名。

- **B（修）**：前缀非空时 `DAODatabaseFile` 返回 `game` / `game_1000`，`schemamigrate` 与 `sidmove players` 不传 `-db` 会打开不带前缀的库。缺省路径就触发。修法：`coredata.DatabaseMapConfig` 新增 `Effective`，`DatabaseMap.Effective` / `Effectives` 给出实际库名；`wiring/dataengine.NewDatabaseMap` 注入 `mods.PrefixedDatabase(prefix, ·)`；`DAODatabaseFile` 返回实际库名再接 sid，合服 `side.physical` 用实际库名。显式 `-db` 不再加工。运行期客户端仍只加一次前缀。
- **A（不修，留警告）**：复用同一 WAL 目录、仍有未投影记录、只改 `mongo.database_prefix` 时，`reconcileDatabaseMapping` 只比映射库名（`main → game`），放行；积压记录回放进新前缀的库。维护者：前缀在项目初定下，正常不改，“A 不用修，留一个警告即可”。警告写在 `mongo.database_prefix` 的配置帮助、[13-wiring §2.2](../../../framework/guide/13-wiring.md#22-mongo-库名前缀mongodatabase_prefix)、[03-dataengine](../../../framework/guide/03-dataengine.md) 与 KNOWN-LIMITS（DAO 库名映射 ⑧）。`databases.json` 与停机 WAL 核对保持原样（映射库名）。

### 修前（变异核对：在独立工作树里把 B 的修复退回原样）

```
$ GOWORK=off go test -count=1 -run TestToolsResolveThePrefixedDatabaseLikeTheProcess ./wiring/dataengine/
--- FAIL: TestToolsResolveThePrefixedDatabaseLikeTheProcess (0.00s)
    databases_test.go:333: DAODatabaseFile({global:true}) = "game", <nil>; want "atlas_game" (the database the process opens)
    databases_test.go:333: DAODatabaseFile({global:false}) = "game_1000", <nil>; want "atlas_game_1000" (the database the process opens)
FAIL
```

### 修后

新用例 `TestToolsResolveThePrefixedDatabaseLikeTheProcess`（共享 / 按服 DAO，非空前缀；`LoadDatabaseMapFile` 的 Physical 与 Effective）与改写后的 `TestSidePhysicalAddsTheMongoDatabasePrefix`（空 / 非空前缀的 `physical`，`walCheck` 仍比映射库名）通过；原有无前缀对照 `TestToolsResolveTheDAODatabaseLikeTheProcess` 照常通过。`GOWORK=off go test -count=1 ./framework/dataengine/... ./wiring/... ./infra/storage/mongo/...` 全部 ok，根门禁 `GOWORK=off go test -count=1 .` ok。
