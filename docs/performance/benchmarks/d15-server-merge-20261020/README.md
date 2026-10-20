# D15 别名合服：两服真实进程演练（2026-10-20）

功能验收，不是性能测量。对应 [PLAYER-MIGRATION §6](../../../framework/PLAYER-MIGRATION.md#6-合服把服-a-并入服-b) 与 [设计 §8](../../../maintenance/FEATURE-2026-10-20-server-merge-alias.md#8-测试与验收)。

## 环境

- macOS（本机），当前分支 `feat/d15-server-merge`（基于 main `057513bb` 之后的 rebase）生成的 game-demo，工程名 `mergedrill`，`replace` 指回工作树。
- 依赖：`scripts/mirror-local.sh up`（私有根目录、端口偏移 26000：Mongo 副本集 53117–53119、NATS JetStream 40222–40224、Redis 42379），另起私有 etcd 46379；不碰共享隔离环境。
- 进程：account / activity / chat / global / mail / match / platform / rank / session 九个支撑服务 + game sid 1000（`config.game.yaml`）+ game sid 1001（`config.game1001.yaml`）。两份 game 配置的 D9 映射不同：`main` 两侧都映射到 `mergedrill`（共享），验收加的 `dbscope=sid` 逻辑库 `zone`（DAO `ZoneStateDao`，`//roost:dao coll=zone_state db=zone dbscope=sid`）在 1000 映射到 `md_zone_b`、在 1001 映射到 `md_zone_a`，物理库分别是 `md_zone_b_1000` 与 `md_zone_a_1001`。
- 客户端：[mergedrill.go.txt](mergedrill.go.txt)（真实 account 总线登录 / 建角 / 选角，按会话里的 `ServingServerID` 连对应 sid 的嵌入 TCP 端点，`EnterGame` + `AddItem`）。脚本 [lib.sh.txt](lib.sh.txt)、[phase1](phase1.sh.txt)、[phase2](phase2.sh.txt)、[phase3](phase3.sh.txt)；路径是本次会话的临时目录。

## 过程与结果

| 段 | 做了什么 | 结果 |
| --- | --- | --- |
| 1 建数据（[log](phase1.log)） | 两服各注册进 account；u1 在 1000、u2 在 1001、u3 两服各一个角色，每个角色 `AddItem` 10 个；直接写 `md_zone_a_1001.zone_state`：`_id 91001`（`_version 2`）与常量 `_id "season"`（`_version 4`），`md_zone_b_1000.zone_state`：常量 `_id "season"`（`_version 1`，内容不同）；两个 game 正常停机 | 5 个角色背包都是 10 |
| 2 合服（[log](phase2.log)） | `sidmove wal-drained` 两侧通过；`sidmerge migrate -dry-run` | 拒绝：`conflicts` fail，`{"_id":"season"}` 两侧摘要不同；其余检查 ok（两侧 WAL 已排空、`co-location:main` 同一位置、`co-location:zone` 基础库没有集合、`outbox:1001` 共用控制库、saga 全部终态），`bus` 为 confirm（syncbus 走 JetStream），见 [dryrun-conflict.json](dryrun-conflict.json) |
| | 业务处置：B 的常量文档改名 `season-b`；`migrate -manifest` | `copied: 2`（[migrate.json](migrate.json)、[manifest.json](manifest.json)）；重跑 `already_present: 2`、`copied: 0` |
| | `accountctl merge-server -from 1001 -into 1000`；`show-server -sid 1001` | `merged_into: 1000`，`closed` |
| | 误启动 game 1001 | 拒绝启动并退出：`server 1001 was merged into 1000 (D15 drill) and its roles are served there`（[log](game1001-refused.log)）；Mod 启动顺序到 `service.account.serving_guard` 为止，`dataengine` 没有 Start |
| | 启动 game 1000；四个角色登录并写 | u1@1000 背包 11；u2（原服 1001）会话 `serving=1000`，连 1000 的端点，背包 10 → 15；u3 的两个角色都由 1000 承载，先后登录，11 / 12；新账号在 1001 `CreateRole` 被拒（560105 `server is not open`），在 1000 成功 |
| 3 回退（[log](phase3.log)） | 停 game 1000，`wal-drained` 通过；模拟 B 写过一份迁入文档（`91001` 改为 `_version 3`、`round 8`）并新建 `92000`；`sidmerge revert` | `reverted: 2`，`new_in_target {"zone/zone_state": 2}`（`season-b`、`92000` 留在 B）（[revert.json](revert.json)）；A 的 `91001` 是 B 的当前版本（`_version 3`、`_last_tx tx-b`） |
| | `unmerge-server`、`upsert-server -sid 1001 -status open`；1001 的旧 WAL 目录移走、用新目录启动两服 | u2 会话 `serving=1001`，连回 1001，背包 15 → 16（合服期间在 1000 上写的 5 个保留）；u3@1001 → 13，u3@1000 → 12，u1 → 12 |

## 边界

- 嵌入 TCP 走了全程；独立 Gate 没有真实进程演练（单元级见 `wiring/gate` 的 `TestAccountAuthenticatorRoutesAMergedServersRoleToItsServingServer`）。
- `zone` 的文档是直接写进 Mongo 的，game-demo 没有用这个 DAO 的实体，B 的进程没有冷加载它们；玩家数据在共享库 `main`，经真实进程冷加载与写入。
- 第 2 段第一次运行时 u1 那一步用了 `-add 0`，被游戏以 `item count must be positive` 拒绝（客户端参数错误，与合服无关），改为 `-add 1` 后从该步重跑。
- 同一台机器上的两个进程，没有验证真实跨机、DNS / 负载均衡切换。
