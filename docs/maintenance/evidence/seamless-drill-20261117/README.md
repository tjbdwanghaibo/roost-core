# seamless 真实多进程演练：端口偏移 + CellWorld 模块化 / facebook 之后的复跑（2026-11-17）

目的：`scripts/test-seamless-demo.sh` 写死了一组本机端口，本机 7000、8081、9100～9103 被维护者另一个项目的 dev 进程占着（sid 1000 的 account / mail / game / gate），按脚本规则拒绝跑，所以 CellWorld 业务适配进模块（D21 后半第一块，`dff25552`，[证据](../cellworld-module-d21-20261117/README.md) 第四节“真实多进程无缝演练没跑”）合入后一直没复跑。本轮给脚本加整体端口偏移，在本机私有依赖上跑缺省档和地形档，验收 `dff25552` 与 facebook 模块（`59c98bac`）。

结论：**两档全部通过**，没有发现 CellWorld 模块化或 facebook 引起的回归；没有改生成器、模块或 demo 夹具。

## 一、偏移怎么加（`scripts/test-seamless-demo.sh`）

- 新变量 `ROOST_SEAMLESS_PORT_OFFSET=N`（缺省 0；非负整数，否则退出 2）。`p <缺省端口>` 给出本次实际端口。
- 生成配置的改法：在把依赖地址换成私有实例**之前**，对 `configs/service/config.*.yaml`（不含 `*.prod.example.yaml`）把 `127.0.0.1` / `0.0.0.0` 上的 `7xxx`、`80xx`、`90xx`、`91xx` 端口加 N（perl 一条替换；偏移 0 时不执行）。这样覆盖：玩家 TCP 7000、全部服务的 ops 9100～9110、etcd 注册地址 9000 / 9002 / 9006 / 9010、account 的 web 8081 与 portal 目录里 sid 1000 的 7000。依赖的缺省端口 27017 / 4222 / 6379 / 2379 不在这些段里，私有依赖地址那时还没换进来，不会被误加。
- 派生 sid 1001～1004：`derive` 匹配加偏移后的 9100 / 7000 / 9000，换成 `p 9201..9204`、`p 7001..7004`、`p 8901..8904`。
- 其余：端口占用检查列表、`start_game` 与两个重启脚本轮询的 readyz、drill 的 `-endpoints` / `-ops`、sid 1003 的 loadtest `-endpoint`（并显式给 `-game-ops`，值等于 loadtest 缺省 9100 加偏移）、Robot 段的 `-endpoint` / `-game-ops` 与快照的 ops（快照文件名仍用缺省端口，Python 校验不变）都加偏移。
- etcd：缺省端口 `58379 + N` / `58380 + N`；显式给 `ROOST_SEAMLESS_ETCD_PORT` 时照给的用、不再加偏移。任一端口越过 65535 时退出 2 并说明（所以示例里的 20000 不可用：etcd 会到 78379）。
- 起进程前新增核对（`check_port`）：每个 game 配置（sid 1000～1004，地形档不含 1003）的 ops、玩家 TCP、注册地址就是 endpoints / -ops 用的端口；其余十个服务的 ops、account 的 web 与 portal 目录都已加偏移。不一致就退出 1。
- 偏移 0 时与原来的差别只有两处，都不改变通过时的行为：占用检查多查 8081（account web 本来就要监听它，被占时 account 起不来）；起进程前多一次核对（缺省配置一定通过）。

验证（不起进程，把脚本的派生与核对段原样 `sed` 出来在生成工程的配置副本上执行，脚本见本轮会话的 scratchpad `sim.sh`）：

```
offset 0 terrain 0: config checks ok      （game100x：7001～7004、9201～9204、8901～8904）
offset 0 terrain 1: config checks ok
offset 3000 terrain 0: config checks ok   （10001～10004、12201～12204、11901～11904）
offset 3000 terrain 1: config checks ok
offset 0: base configs unchanged          （diff -r 生成配置与偏移 0 处理后的配置：只多出派生的 game100x）
```

变异：偏移时跳过 `config.account.yaml` → 核对红：`seamless: configs/service/config.account.yaml has no 'addr: …:12101' after the port offset`，退出 1。

`bash -n` 通过。Robot 段（`ROOST_SEAMLESS_ROBOTS>0`）本轮没有跑（见“尚未确认”）。

## 二、环境

- 依赖：`scripts/mirror-local.sh up`，私有根在会话 scratchpad、偏移 31000（Mongo 副本集 58117～58119、NATS JetStream 45222～45224、Redis 47379）。没有用共享隔离环境 `~/.roost-it`：脚本收尾按 `^cwdemo(_|$)` 删全部库、NATS 流与 Redis 键也都是 `cwdemo` 前缀，放在共享环境里会和别人同前缀的运行互相清掉；脚本头也写明依赖须是调用方给的私有实例。跑完 `mirror-local.sh clean`。
- 演练端口偏移 3000（玩家 TCP 10000～10004、account web 11081、ops 12100～12110 / 12201～12204、注册地址 12000～12010 / 11901～11904、etcd 61379 / 61380），跑前逐个 `nc -z` / `lsof` 确认空闲。
- 代码：worktree 分支 rebase 到 main `376820d8`（含 `dff25552` CellWorld 模块化与 `59c98bac` facebook）。生成的 game-demo 实体号：Player 1、World 2、Scene 3、Monster 4、**SceneActor 5**、Facebook 6、Team 7……drill 写死的 `kindSceneActor = 5`、`kindMonster = 4` 仍然对，**不用改**。

## 三、命令与结果

```
source <mirror-local 私有根>/env.sh
ROOST_SEAMLESS_PORT_OFFSET=3000 ROOST_SEAMLESS_OUT=<abs> scripts/test-seamless-demo.sh
ROOST_SEAMLESS_TERRAIN=1 ROOST_SEAMLESS_PORT_OFFSET=3000 ROOST_SEAMLESS_OUT=<abs> scripts/test-seamless-demo.sh
```

### 缺省档：exit 0，15 步全部 `ok`，普通场景机器人通过

`default-drill.log`：enter、own_state、straight、jitter、diagonal、attack_across_0、death_reward_respawn、attack_across_1、panel_level_up、monster_cross、cell_crash、base_restart、team_panel、team_roster_cross_base、ordinary_scene 全部 `STEP … ok`，`seamlessdrill: all steps passed`；脚本 `seamless: all steps passed`。sid 1003 的 loadtest（2 个机器人）p95 5.41s（阈值 16s，未越线，`default-loadtest-1003.json`）。

与本轮验收对象直接相关的几步：

| 步骤 | 覆盖的改动 | 结果摘要 |
| --- | --- | --- |
| enter / own_state / straight | 模块自带的 SceneActor（kind 5）在真实格进程建出、客户端按 kind 5 认出自己的 SceneActor | p1 进场 (2900,1500)、跨格 owner 变化 2 次 |
| cell_crash | A5：Player 上的恢复快照（`CellworldBase`）+ A4 心跳；kill -9 sid 1002 后按快照重建 | p2 HP 72 → 72，epoch 2 → 3，重启 6.9s，Base 重进 7 次 |
| base_restart | Base 快照位置只在回写时写、重新装载取保存位置（A3 / A5） | 保存 (1176,1500)，重启后进场 (1176,1500)，不是出生点 |
| death_reward_respawn | A5：死亡回执（`RecordDeathReceipt`） | 金币 0 → 1，复活 HP 100 |
| team_panel / team_roster_cross_base | facebook：队伍邀请跨 game 投递（p1 在 sid 1000 邀请连在 sid 1004 的 p6） | 面板 50ms 到；kill 1004 后 3068ms 移除、重启后恢复；离队 67ms 移除；解散后 team 清空 |

### 地形档：exit 0，7 步全部 `ok`

`terrain-drill.log`：terrain_start、terrain_ghost_edit、terrain_player_step、terrain_monster_step、terrain_cell_restart、terrain_consistent、terrain_building_view 全部 `ok`；脚本 `seamless: all terrain steps passed`。例如 building_view 观察者 53ms 收到 create，1002 / 1004 上的玩家没有持有。

### 日志（`log-summary.txt`）

- 两档都没有 ERROR、panic、fatal；实际监听地址全部是加偏移后的端口（日志里没有 7000 / 8081 / 91xx）。
- 缺省档 26 条 WARN：16 条 saga 预算提示（配置提示）、chat 未配 prune 通道 1 条、alliance `handlerAllianceFound` 慢派发 1 次（222ms）、`scene: subscribe … subject is not registered` 3 条与“subscription refused repeatedly” 2 条（都在 team_panel 那一步 kill -9 / 重启 sid 1004 的窗口里，订阅对象的 SceneActor 那时不在）、sid 1002 重启后第 6 / 17 帧 `cellworld_input` 的 `entity_not_found` 2 条（格进程刚重启、SceneActor 按快照重建之前到的输入）。后两类在 2026-11-06 的 `feature-review-20261106/cell-lifecycle.log` 里已有同样的行，是演练的故障窗口里的预期瞬态，本轮不按缺陷处理。
- 地形档 11 条 WARN：saga 预算提示 10、chat prune 1。
- `script-tail` 里的 `Killed: 9` 是 drill 自己 kill -9 sid 1002 / 1004 的进程（演练步骤），不是异常。

## 四、发现的问题

没有发现回归。CellWorld 模块化（A2 SceneActor、A3 状态映射与 Node 安装点、A4 心跳登记、A5 恢复快照与死亡回执）与 facebook（邀请跨 game 投递）在真实多进程下按演练断言工作。

## 五、尚未确认 / 没覆盖

- 偏移 0 没有端到端跑（本机 7000 / 8081 / 9100～9103 被占，脚本照规则拒绝）；只用上面的配置模拟确认偏移 0 不改生成配置、核对通过。
- Robot 段（`ROOST_SEAMLESS_ROBOTS>0`、`ROOST_SEAMLESS_CAPACITY=1`）只改了端口，没有跑。
- 每档各跑一次，没有重复多轮。

## 六、建议写进 CHANGELOG / KNOWN-LIMITS 的文字（协调者统一写）

CHANGELOG `[Unreleased]`：

> - **测试：`scripts/test-seamless-demo.sh` 支持整体端口偏移**（`ROOST_SEAMLESS_PORT_OFFSET=N`，缺省 0 行为不变）：生成配置里 demo 的全部端口（玩家 TCP、ops、注册地址、account web / portal 目录）、派生的 sid 1001～1004、drill / loadtest 的地址、缺省 etcd 端口一起加 N，起进程前核对配置与 endpoints 一致；端口占用检查多查 account 的 8081。CellWorld 业务适配进模块（D21 后半第一块）与 facebook 模块之后，在本机私有依赖（mirror-local）上以偏移 3000 复跑：缺省档 15 步 + 普通场景机器人、`ROOST_SEAMLESS_TERRAIN=1` 档 7 步全部通过（[证据](docs/maintenance/evidence/seamless-drill-20261117/README.md)）。

KNOWN-LIMITS：如果有“cellworld 模块化之后真实多进程演练未复跑”一条（D21 证据第六节建议过），可以删；本轮不新增条目。
