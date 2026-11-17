# CellWorld 业务适配进模块（D21 后半，2026-11-17）

维护者 2026-11-17：“CellWorld 可以做了”。目标：关闭 KNOWN-LIMITS 二“CellWorld 业务适配进模块（D21 后半）”。

## 一、划分（动手之前写定）

原则（compose.md 四条、“demo 是参考实现，不改成生成器输出”）：demo 里属于 CellWorld **通用**适配的部分做成 cellworld 模块的能力，demo 改为装模块 + 只写模块没覆盖的部分；demo 自己的玩法规则、协议与它自己的实体留在 demo。只对新生成 / 新装模块的工程负责。

### 搬进模块（`modules/cellworld`）

| 块 | demo 里原来的位置 | 模块里的位置 | 说明 |
| --- | --- | --- | --- |
| A1 分区状态的持久嵌套类型 | `db/def/monster.go`（CellHit / CellHandoff / CellOwnership / CellRecovery / CellEffect / CellCooldown / CellDeath） | `files/db/def/cellworld.go.tmpl` | cellworld.ActorState / BaseState 里的嵌套值；任何承载 Actor 状态的 DAO 共用 |
| A2 SceneActor 实体与 DAO | `game/entities/scene_actor/entity.go`、`db/def/scene_actor.go`（demo 的 `add entity` / `add dao` 两步） | 模块自带实体 `entities: [SceneActor]`：`files/game/entities/scene_actor/entity.go.tmpl`、`files/db/def/scene_actor.go.tmpl` | 玩家在格上的那一份，整份是 cellworld 状态，没有业务字段；实体号改由安装器分配 |
| A3 状态映射与本进程 Node 安装点 | `game/partition/partition.go`（Install / Node / SceneID / CellMode、CellDao + ReadActor / WriteActor、ReadBase / WriteBase 收 `*db.PlayerDao`） | `files/game/modules/cellworldhost/state.go.tmpl` | ReadBase / WriteBase 改收接口 `BaseDao`（方法集与生成的 PlayerDao 读写器同名），Player 的字段仍由工程声明 |
| A5 Player 上的恢复快照与死亡回执（实施中加入） | `db/def/player.go` 的 `CellRecovery` / `CellDeathReceipts`、`cell_base.go` 的回执容量检查 | 模块状态 `PlayerDao.CellworldBase`（`CellworldBaseState`，`cellworld_base`，持久、不同步）、`DeathReceipt` / `RecordDeathReceipt` | 见第二节“为什么加 A5” |
| A4 格心跳登记 | `game/entities/scene_actor/entity.go`、`game/components/monsteractor/cell.go`、`game/components/mapview/cell_base.go` 各写一份 `Register("cell.actor"/"cell.base", …)` | `files/game/modules/cellworldhost/state.go.tmpl` 的 `RegisterActorBeat` / `RegisterBaseBeat` | 心跳名、周期取 Node 配置、Node 卸下后什么都不做，三处一份 |

模块场景（`roost module verify cellworld`，`^TestCellworld`）10 → 17：A2 / A3 的映射完整、读出是副本、生成 setter 回滚、Base 快照位置只在回写时写、重新装载的玩家取保存位置、死亡回执表满拒绝新目标、Player 的 CellworldBase 回滚与持久编码、没装 Node 时 SceneActor 拒绝建出与 Node 安装点。先红后绿与变异见第三节。

### 留在 demo（原因）

| 块 | 位置 | 为什么留 |
| --- | --- | --- |
| Player 的分区字段（恢复快照与死亡回执以外）与 BasePlayer 实现 | `db/def/player.go` 的 Cell* / Saved* 标量字段，`game/entities/player/cell_base.go` | 字段是 Player 主 DAO 的扁平字段；其中 CellHP / CellMaxHP / CellAttack / CellPower 在 player 命名空间的同步编码里（Base 的 mirror 借 Player 的编码，客户端、drill、loadtest 按它解码），模块 `state:` 只能追加嵌套结构体，改过去同步编码就变——要维护者定（见第五节 Q1）。面板取 demo 的 attribute 组件，死亡回执与 SettleDeath 的钱包是 demo 的规则。映射本身已经进模块（A3），这里只剩转调 |
| Monster 的 Actor 实现 | `game/entities/monster/cell.go`、`game/components/monsteractor/cell.go` | Monster 是 demo 自己的实体（不是 monster 模块的：monster 模块的 Monster 不持久，且模块 README 写明不能与 cellworld 对同一 Actor 装两套调度）。映射与心跳登记已经是模块的（A3 / A4），这里只剩转调 |
| mapview 的 Base 心跳宿主 | `game/components/mapview/cell_base.go` | Base 心跳挂在 demo 的 Map 组件的心跳宿主上；登记本身改调模块的 `RegisterBaseBeat` |
| 服务里的分区装配 | `internal/service/game/partition.go` | mirror 打包用 demo 的 Player / Monster 同步编码、攻击结论推 demo 的 `AttackResultPush` 协议、奖励规则（每只 NPC 1 金币）、gm 统计、出生点来自 demo 的 `scene.partition` 配置 |
| 分区配置 | `game/settings/partition.go` | `scene.partition.*` 带 demo 的 MonsterTemplate / PlayerHP / AttackDamage；改成模块 config 要把键改成 `cellworld.*`，脚本与 drill 的配置一起改——放到以后 |
| 队伍视图 | `game/entities/player/cell_team.go`、Player 的 TeamVer / TeamMembers 字段 | 监听 team 模块的 `event.TeamChanged`、实现 cellworld.GroupPlayer 都要 Player 的字段；同 Q1 |
| 地形分区与转发 | `internal/service/game/terrain_partition.go`、`terrain_relay.go`、`game/terrainmap/relay.go` | demo 的可编辑地形（`game/terrainmap`、TerrainChunk / TerrainCellEditor）不是 terrain 模块那一套；分区转发建在 demo 地形上，要先定“两套地形合一”，不属于 CellWorld 适配本身 |
| 端点 | `game/controllers/player_partition.go`、`player_move_intent.go`、`player_attack_target.go` | demo 的协议（MoveIntent / AttackTarget / AttackResult） |

### MMORPG 夹具里的队伍、场景、怪物、战斗、副本

MMORPG 夹具（`scripts/testdata/mmorpg`）是单进程玩法夹具，不用 CellWorld。本轮定：**都不随这次改动走，继续手写**，理由逐项（R9 2B 的核对，本轮没有新的模块能力能填上）：

- 队伍：team 是 Remote 实体，REAL 档要多一套 Redis / NATS；夹具是两个 Player 同事务。CellWorld 这轮没有改 team。
- 场景：scene 模块的 Scene 不落库（`nocoll`），夹具的 Scene 持久人数与切图版本；与模块 Scene 撞名。
- 怪物：monster 模块不持久、无掉落归属；夹具要 Boss 血量与掉落过 WAL。CellWorld 的 Actor 映射（A3）只管分区状态，不提供持久怪物。
- 战斗：combat 只限两名玩家；夹具是 PvE、N 对 1、单调输入序号。
- 副本：dungeon 要独立 Dungeon 实体与关卡程序，夹具的“副本 = Scene + Boss 已死”是另一种模型。

这些缺口不是 CellWorld 适配，保留在 R9 2B 的“对 CellWorld 进模块那一轮的提示”表里，作为各模块自己的扩展项。

## 二、实施记录

一个提交 `dff25552`（模块、demo、生成步骤、文档一起：demo 依赖模块的新类型，拆开中间态编不过）；本证据另一个提交。

### 模块（`modules/cellworld`）

- `module.yaml`：`entities: [SceneActor]`、`entity: Player` + `state: CellworldBase`，`verify.pass` 10 → 17，`next` 改写接线步骤。
- 新文件：`files/db/def/cellworld.go.tmpl`（嵌套类型 + `CellworldBaseState`）、`files/db/def/scene_actor.go.tmpl`、`files/game/entities/scene_actor/entity.go.tmpl`、`files/game/modules/cellworldhost/state.go.tmpl`（安装点、心跳登记、CellDao / BaseDao 映射、死亡回执）、`state_test.go.tmpl`（7 个场景，外部测试包，`BaseDao` 用内存替身、CellworldBase 用真实 PlayerDao）。
- SceneActor 没有组件（没有业务规则）；`SetCellPeriod` 调心跳登记的 `SetPeriod`，在模块实体守卫（`TestModuleEntityPackagesHoldOnlyComponentWiring`）里按“框架接口实现”登记例外，原因与 demo 原来的同一条一致。
- 行为与 demo 原实现逐行一致（映射函数整段搬入），两处有意的改动：`ReadBase` / `WriteBase` 收接口；SceneActor 的心跳宿主从 `Install` 给的驱动取（原来直接用 demo 的 `actor.Heartbeat`）。

### 为什么加 A5（Player 的恢复快照与死亡回执改成模块状态）

第一次生成 demo 失败（原文）：`template game-demo: add saga GiftItem (…): refresh generated artifacts: generator dao: parse definitions: dao MonsterDao field CellDeath (*CellDeath): a pointer to CellDeath, which is not a nested struct of these definitions`。嵌套类型改由模块带来后，demo 的 MonsterDao / PlayerDao 在模块安装前就引用它们，而 GiftItem saga 那一步要重刷全部生成物。

把 cellworld 单独提前装（Monster 实体之后、写 MonsterDao 之前）解决了 MonsterDao；但测试夹具的工程只带 `configdata mongo nats dataengine nest`，安装器先代办 `roost add mod manager` / `redis`，那一步同样重刷，失败在 PlayerDao（原文）：`module cellworld: roost add mod manager -service game: refresh generated artifacts: generator dao: parse definitions: dao PlayerDao field CellRecovery (*CellRecovery)…`（`TestDemoTeamModuleIntegration` 等 5 条红）。PlayerDao 在第 508 步就写好了，早于任何可行的安装点。

所以 Player 上用到模块类型的那两个字段改成模块自己追加的状态（与“模块状态 = 主 DAO 下的嵌套结构”一致）：`PlayerDao.CellworldBase{Recovery, DeathReceipts}`，demo 的 player.go 不再引用模块类型。存储格式变化：`cell_recovery` / `cell_death_receipts` → `cellworld_base.1` / `.2`，demo 的 PlayerDao `schema=5 → 6`（未上线，不兼容旧数据）。

### demo

- 删除：`game/entities/scene_actor/entity.go`、`db/def/scene_actor.go`、`game/partition/partition.go`（及 demo.go 里 `add entity SceneActor`、`add dao SceneActor` 两步与对应写步骤）；`db/def/monster.go` 去掉嵌套类型，`db/def/player.go` 去掉两个字段。
- 转调模块：`game/entities/monster/cell.go`、`game/entities/player/cell_base.go`（映射、死亡回执）、`cell_team.go`、`game/entities/scene/entity.go`、`game/components/monsteractor/cell.go`、`game/components/mapview/cell_base.go`（心跳登记）、`internal/service/game/partition.go`（`cellworldhost.Install(node, scene, actor.Heartbeat)`、`CellMode`）。
- 测试：`game/partition/partition_test.go` 移为 `game/entities/player/cell_base_test.go`（只留 demo 的 PlayerDao 接上模块映射的 4 条：快照位置、保存位置、面板回滚、文件 WAL 恢复），新增 `game/entities/monster/cell_test.go`（MonsterDao 满足 CellDao、整份状态与回滚）。SceneActorDao 的映射测试移进模块场景。
- 生成步骤：`demoScaffoldSteps` 在 Monster 实体之后加 `{modules: ["cellworld"]}`，主模块步骤去掉 cellworld；`TestDemoStepsLeaveModuleComponentsToTheModules` 的“全部模块一步装完”改为允许 cellworld 单独先装、且必须在 GiftItem saga 之前（变异：放回主步骤 → 红，见 [mutation-demo-step.txt](mutation-demo-step.txt)）。
- 实体号：SceneActor 9 → 5（安装时的最小空号），team 5 → 6、alliance 6 → 7、alliance_member 7 → 8、room 8 → 9。只有 seamless drill 写死了 SceneActor 的号（`scripts/testdata/seamless/drill.go.txt` 已改为 5）。

代码量：demo 模板 −611 行（删 4 个文件）、+76 / −256（改 14 个）；模块 +1274 行（含 7 个场景约 430 行与 BaseDao 替身）。搬家为主，没有声称“更简单”。

### 文档

`modules/cellworld/README.md`（能力表、Player 字段表、复用步骤、验证范围 17 条）、`modules/README.md` 索引行、`gameplay/cellworld/README.md` 正式接入表（标出模块 / demo）、`demo/README.md` 分区一段、`docs/agent-skills/roost-genre/SKILL.md` 与 `references/compose.md` 的 cellworld 接线说明。

## 三、验证

| 项 | 命令 | 结果 |
| --- | --- | --- |
| 先红 | 装好模块后删掉 `game/modules/cellworldhost/state.go` 再跑 `^TestCellworld` | 编译失败 `undefined: cellworldhost.IsCellMode / Heartbeat / RegisterActorBeat / ReadActor / WriteActor`（[red-before.txt](red-before.txt)）；新场景测的是新能力，红即“没有这个能力” |
| 模块验收 | 新 `-template game` 工程 + `roost module add cellworld` + `roost module verify cellworld` | 17/17、0 skip，build / vet / id check / generate --check 通过（[module-verify.txt](module-verify.txt)）；安装后 `gofmt -l` 干净，SceneActor 号 3 |
| 变异 | 在安装好的工程里改 9 处，各跑 `^TestCellworld` 再恢复 | 9 处全红、恢复后绿（[mutation-state.txt](mutation-state.txt)）：快照位置每次都写、他场景的保存位置被用、去重表不替换、回执容量不查、Uninstall 卸掉别人的 Node、FracX 不写、不是 CellMode 也建出、普通进程 Base 心跳报错、恢复快照只比版本 |
| codegen | `go test -run 'TestDemo' ./codegen/internal/roost`（13 条，含 demo 生成 + 编译 + team / alliance / actor 集成、改服务名） | 全部 PASS（[testdemo.txt](testdemo.txt)） |
| codegen | `go test -run 'TestModuleCatalog\|TestModuleView\|TestModuleEntity\|TestModuleSpec\|TestModuleNext\|TestModuleOwnedEntity\|TestModuleManagers\|TestModulesInstallOntoAGeneratedProjectAndVerify'`；`go vet ./codegen/internal/roost` | ok |
| demo 生成工程 | `project new -template game-demo` → `go build ./...`、`go vet ./...` | 通过；生成期没有 WARN |
| demo 包 | `go test -race` `./game/entities/... ./game/modules/cellworldhost ./game/managers ./game/components/... ./internal/service/game ./game/controllers ./game/settings ./cmd/loadtest` | 除 `TestDungeonClearSubmitsTheSeasonCountAsASnapshot` 外全绿（[demo-packages.txt](demo-packages.txt)）；该条在未改动的 HEAD 生成工程上同样失败（`submitted to board total:global:0:1, want clears:global:0:1`），与本改动无关，见第四节 |
| MMORPG | `scripts/test-mmorpg-generated.sh`（缺省档）、`scripts/test-mmorpg-scenes.sh`（game-demo 多地图 TCP） | 14 PASS / 0 SKIP，module verify 5 项 n/n；TwoScenes PASS，build / vet 通过（[mmorpg.txt](mmorpg.txt)）。REAL 档没跑：夹具不用 CellWorld，本改动不碰它 |
| 文档 | `go test -run 'Link\|Anchor\|Doc' .` | ok |

没跑全量（brief 规定由协调者统一跑）。

## 四、尚未确认 / 没验证

- **真实多进程无缝演练没跑**：`scripts/test-seamless-demo.sh` 用固定端口 7000 / 9100～9103 等，本机有别人的 demo 进程（account / mail / game / gate，sid 1000，已运行约 7 小时）占着，按脚本规则不跑。离线测试里服务层的分区用例用替身实体，**真实 SceneActor 的建出、心跳与 Player 的 CellworldBase 回写只由这条演练覆盖**——合入前须跑一次（含 `ROOST_SEAMLESS_TERRAIN=1` 那一档）；drill 的 SceneActor 号已改为 5。
- `TestDungeonClearSubmitsTheSeasonCountAsASnapshot`（game-demo 的 `game/controllers`）在 HEAD 上就红：读到的榜名是缺省的 `total` 而不是 demo 配置的 `clears`，像是 R5（`7256ab66` 模块配置）之后测试没装载 demo 的 leaderboard 配置。**尚未确认**根因，不在本任务范围。
- 模块场景里 `BaseDao` 的映射用内存替身（模块的测试工程里 Player 没有分区字段），生成 setter 的回滚只在 demo 的 Player 用例里覆盖。

## 五、要维护者定的

> 维护者 2026-11-18 按推荐定（凡写了推荐的点都按推荐）。

- **Q1 Player 的 Base 标量字段要不要也进模块状态**：现在 30 多个 Cell* / Saved* 字段仍由工程在 PlayerDao 里按 README 的表声明，模块经 `BaseDao` 接口用。改成模块 `state:` 的嵌套结构可以省掉手写，代价：CellHP / CellMaxHP / CellAttack / CellPower 在 player 命名空间的同步编码里（Base 的 mirror 借它给客户端看 SceneActor），变成嵌套同步字段后客户端、drill、loadtest 的解码一起改；PosX / PosY / Saved* 与普通场景的 Map 共用，要拆。推荐 A：保持现状（字段表已写进模块 README）；B：做嵌套状态并改客户端解码（M～L）。
- **Q2 队伍视图（`cell_team.go`、TeamVer / TeamMembers）进模块的 `with/team` 联动**：依赖 Q1（GroupPlayer 要 Player 的字段）。推荐随 Q1 一起定。
- **Q3 `scene.partition` 改成模块 config（R5 写法，键改 `cellworld.*`）**：要同步改 seamless 脚本、drill 与 demo 配置。推荐以后单独做。
- **Q4 demo 的分区地形转发**：建在 demo 自己的可编辑地形上，不是 terrain 模块；要不要先定“两套地形合一”。

## 六、交给协调者写的文字

CHANGELOG `[Unreleased]`：

- 变更（cellworld 模块，D21 后半）：模块带来玩家在格上的 SceneActor 实体（`game/entities/scene_actor`、`db/def/scene_actor.go`）、分区状态的嵌套类型（`db/def/cellworld.go`）、Player 上的模块状态 `CellworldBase`（恢复快照与死亡回执，`cellworld_base`）与 `game/modules/cellworldhost` 里的 Node 安装点、格心跳登记、DAO 与 Actor / Base 状态的映射（`ReadActor` / `WriteActor` / `ReadBase` / `WriteBase`）、死亡回执；模块场景 10 → 17。game-demo 改为在 Monster 实体之后单独先装 cellworld，删掉自己的 `game/partition`、SceneActor 实体与 DAO，Player / 怪物 / 服务只写模块没覆盖的部分。
- 升级：game-demo 的 PlayerDao `schema` 5 → 6（`cell_recovery` / `cell_death_receipts` 移进 `cellworld_base`），实体号 SceneActor 9 → 5、Team 5 → 6、Alliance 6 → 7、AllianceMember 7 → 8、Room 8 → 9；未上线不兼容，旧 demo 数据清空、先停旧再起新。只对新生成 / 新装模块的工程负责。

KNOWN-LIMITS：

- 二“CellWorld 业务适配进模块（D21 后半）”改为：已做的部分（SceneActor、映射、心跳登记、安装点、Player 的恢复快照 / 死亡回执）进了模块；留在 demo 的是 Player 的 Base 标量字段与面板 / 队伍副本（Q1、Q2）、mirror 编码与攻击结论推送、奖励规则、`scene.partition` 配置（Q3）、分区地形转发（Q4）。MMORPG 夹具的队伍、场景、怪物、战斗、副本不随 CellWorld 走，留作各模块的扩展项（理由见本证据第一节）。待 Q1～Q4 定后关闭或改写。
- 若 seamless 演练合入前未跑：加一条“cellworld 模块化之后真实多进程演练未复跑”。
