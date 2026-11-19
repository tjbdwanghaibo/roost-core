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
