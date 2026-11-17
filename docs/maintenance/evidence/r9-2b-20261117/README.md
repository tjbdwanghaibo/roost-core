# R9 批 2（agent 2B）：MMORPG 装模块（2026-11-17）

方案：[PLAN-2026-11-17-r9-genre-modules](../../PLAN-2026-11-17-r9-genre-modules.md) 3.11（映射）、第 5 节表 2B（文件范围）、第 9 节（批 0 定型的写法）；维护者 11-17 答复 Q1～Q8 全按推荐（Q5 A：本轮只换 wallet、bag、task）。`scene_runtime*`、`crosssid` 与 `modules/**`、`demo/**` 没有动。同一检出里另有 R3 与其它 R9 agent 在改别的文件，本批生成工程按当时的工作区编译，全部通过。

## 改法

模块清单 `wallet bag task`（不装 enter：夹具没有建档 handler，fixture 的种子 handler 直接经组件发初始资产，所以没碰到 1C 的 `bag_item_invalid`）：

| 原夹具的块 | 现在 | 说明 |
| --- | --- | --- |
| `Player.Gold`（转账、托管、任务奖励） | **wallet** | handler 参数 `IMMOEntity` 改为嵌 `view.PlayerEntity`，调 `Wallet().Add`；“做过没有”在玩法事实里（`TradeSeq`、Trade `Status`），不用 `Apply`。1,000,000 持有上限仍是玩法规则，读 `Balance()` |
| `Player.Items` | **bag** 道具 3001 | 工程 `item.csv` 追加 `3001,MMO 材料,1000`；托管 `Bag().Remove`，成交 / 退还 / 拾取 `Bag().Add`（拾取满了答 `bag_stack_full`） |
| `QuestKills` / `QuestClaimed` | **task** 永久任务 201 | 工程 `task.csv` 追加 `201,mmo.kill,permanent,1,0,coins,0,5,false`；击杀那一笔 `Task().Report("mmo.kill")`；`MMORPGQuest(target, nowUnixMilli)` 调 `Task().Claim(201)`，`task_claimed` 当作重放成功 |
| 队伍、Scene、Monster、战斗 / 冷却、Buff / 复活、InstanceComplete | 手写 | Q5 A，见下文“对 CellWorld 进模块那一轮的提示” |
| Trade 托管 | 借鉴 friends | 两 Player + Trade 同一笔事务，先检查全部余额 / 容量，再经各自组件写 |

配置：bag 与 task 都读配置表，fixture 与 REAL 的每个进程（含 SIGKILL 子进程、第三个核对进程）先 `loadMMOTables` 装 `../../configs/data`。

## 文件

| 文件 | 改法 |
| --- | --- |
| `scripts/test-mmorpg-generated.sh` | `go mod edit -replace` 提前；装 `wallet bag task`；删 `add dao Player`；Player 去掉 4 个资产字段，DAO 字段改为追加在 struct 末尾；往 `item.csv` / `task.csv` 追加两行；缺省档加 `go build` / `go vet ./...` 与 `modules.sh verify` |
| `scripts/testdata/mmorpg/tasks.go.txt` | `IMMOEntity` 嵌 `view.PlayerEntity`；转账改调两边 `Wallet().Add` |
| `scripts/testdata/mmorpg/gameplay.go.txt` | 常量 `mmoItem` / `mmoKillTask` / `mmoKillEvent`；Offer / Resolve 经 bag / wallet；Strike 上报任务；Loot 经 bag；Quest 经 task |
| `scripts/testdata/mmorpg/tasks_test.go.txt` | `loadMMOTables` 与读模块状态的 `mmoGold` / `mmoItems` / `mmoQuestKills` / `mmoQuestClaimed`；种子经组件；断言改读模块状态 |
| `scripts/testdata/mmorpg/gameplay_test.go.txt`、`robot/network_test.go.txt` | 断言改读模块状态；Combat 另加“领奖事实已记” |
| `scripts/testdata/mmorpg/persistence/{persistence,recovery}_test.go` | 每个进程装表；种子经组件；视图读模块状态；崩溃屏障另断言领奖事实已记；未投影 WAL 仍 16 条 |
| `docs/framework/MMORPG-INTEGRATION.md` | §2、§3 表格三行、§4.1 第 2～3 步、新增 §4.5（模块清单、手写块、旧断言 → 新断言）、§10 |
| `docs/agent-skills/roost-genre/references/mmorpg.md` | §2 模块表；§3.2 改为装模块的命令块；§4、§7 |
| `docs/maintenance/PLAN-2026-11-17-r9-genre-modules.md` | 只在 §9.5 追加一行（2B） |

场景数：`TestMMORPG` 仍是 14 条（13 条业务 + 1 条 Robot），场景名不变；REAL `TestMMORPGRealPersistenceAcrossProcesses`（write / recover）与 `TestMMORPGRealCrashRecovery` 不变，未投影 WAL 仍 16 条。新增的模块场景（`module verify`）：bag 20、task 29、task+bag 3、task+wallet 6、wallet 17。

## 验收（方案第 5 节六条 + §9.4）

| # | 命令 | 结果 | 摘要 |
| --- | --- | --- | --- |
| 1 | `bash scripts/test-mmorpg-generated.sh` | exit 0。14 PASS、0 SKIP；build、vet、`id check`、`generate --check` 通过；`module verify` 5 项都是 n/n、skipped=0 | [default.txt](default.txt) |
| 2 | `python3 scripts/test-genres-generated.py --out <scratchpad>/matrix1 --genres mmorpg` | `status passed`；自定义 module 名下 14 条再过；`custom_module_verify` 5 项齐全 | [matrix.txt](matrix.txt) |
| 3 | `source ~/.roost-it/roost-dataengine-it/env.sh; ROOST_MMORPG_REAL=1 bash scripts/test-mmorpg-generated.sh` | exit 0。缺省 14 条加 `TestMMORPGRealPersistenceAcrossProcesses`（write、recover）与 `TestMMORPGRealCrashRecovery`（16 条未投影记录 SIGKILL 后逐字节恢复、补触发、第三进程重放不重复）；0 SKIP，库已删 | [real.txt](real.txt) |
| 4 | 变异：保留的生成工程里改 handler，跑对应场景，再恢复 | 11 个离线变异 + 2 个 REAL 变异全部变红，恢复后变绿 | [mutation.txt](mutation.txt)，驱动 [mutate.py.txt](mutate.py.txt) |
| 5 | `git status`（本批文件）；`GOWORK=off go test -count=1 -run 'Link\|Anchor\|Doc' .` | 本批文件提交后无残留；文档测试通过 | — |
| 6 | 手册与 skill reference 同步；skill 命令块真跑（SKILL.md §3.2 前置，自定 module `example.com/mymmo`）；手册 §4.3 示例在生成工程里 build + vet | 14 PASS，`module verify` 5 项 n/n，§3.4 收尾全过，doctor 无 FAIL；示例编译通过 | [skill-block.txt](skill-block.txt) |

改过断言的场景与变异（第 4 条；旧断言 → 新断言的表在 [MMORPG-INTEGRATION §4.5](../../../framework/MMORPG-INTEGRATION.md#45-装了哪些模块哪些留手写r92026-11-17)）：

| 场景 | 旧断言 → 新断言 | 变异 |
| --- | --- | --- |
| `TradeConcurrentReplay` | `Player.Gold` 40 / 60 → `Wallet.Coins` 40 / 60 | M1 删收款方 `Wallet().Add` |
| `TradeRollback` | `Player.Gold` 50 / 50 → `Wallet.Coins` 50 / 50 | M11 拒绝路径吞掉错误 |
| `RobotBusiness` | `Gold` 43 / 57 / 48、`Items` 4、`QuestKills` 1 → wallet / bag 3001 / task 201 进度 | M2 转账改 `Apply` 固定键 |
| `TradeBothConfirmAndReplay` | `Items` / `Gold` → bag 3001 / wallet | M3 删卖家 `Bag().Remove`；M4 删买家 `Bag().Add` |
| `TradeCancelAndExpiryRefundEscrow` | 同上 | M5 删退款 `Wallet().Add` |
| `TradeConfirmRacesWithCancel` | 资产守恒 → wallet + bag 守恒 | M10 删挂单扣买家金币 |
| `CombatCooldownReplayLootAndQuest` | `QuestKills == 1`、`Items == 4`、`Gold == 55` → task 进度 1、bag 4、wallet 55，另加 `Claimed[201]` | M6 删 `Task().Report`；M7 不容忍 `task_claimed`；M8 吞掉 Claim 的所有错误；M9 删拾取 `Bag().Add` |
| `TestMMORPGRealCrashRecovery` | 种子写 DAO → 经组件；屏障另断言领奖事实已记 | R1 进程不装配置表 |
| `TestMMORPGRealPersistenceAcrossProcesses` | 视图读 DAO 字段 → 读模块状态 | R2 同 M7（recover 阶段重放领奖变红） |

## 模块缺口（按 Q1 只记录，没有改 modules/**）

- 没有要改模块的缺口；没装 enter，没碰到 1C 那条。
- 两点边界：task 的 `Claim` 对已领答 `task_claimed`（不是幂等重放），要“重试拿同样结果”的玩法 handler 自己把它当成功；wallet 不设持有上限，玩法上限要读 `Balance()` 手写。
- 方案 3.11 已列的“monster 持久与掉落、combat 多人”不在 R9，见下节。

## 对 CellWorld 进模块那一轮的提示

| 夹具里的手写块 | 将来可改装的模块 | 现在缺什么 |
| --- | --- | --- |
| Scene（`Occupants` 人数、`SceneVersion` 切图版本、持久） | scene | 1）撞名：scene 模块自带 `Scene` 实体，`module plan scene` 报 owned entity 冲突，夹具要删自己的 Scene 实体与 DAO、改用模块的；2）语义：夹具的 Scene 持久（人数事实），scene 模块的 `SceneDao` 整个不落库（`nocoll`，场景从开图配置重建、成员不从库恢复；demo 的地形 Scene 同样是 `noPersist=true lifetime=runtime_rebuild`）——要么模块加“持久的场景归属事实”（人数 + 归属版本，和 Player 同事务改），要么夹具把人数改成从 Player 归属派生；3）切图的 `version` 旧请求拒绝要在模块里有对应入口；4）与 `scripts/test-mmorpg-scenes.sh`（game-demo 的多地图 TCP）关系要重理，两套 Scene 合一 |
| Monster 血量、`LootOwner` / `LootTaken`、Boss 存活 | monster（依赖 scene） | monster 模块不持久、无掉落（README）。要装：1）持久选项（至少 Boss / 副本怪的血量与掉落归属要过 WAL，崩溃后不能重发掉落）；2）掉落归属与“已拾取”事实，拾取经 bag 与 Player 同一笔事务；3）击杀的实体事件（如 `event.MonsterKilled{Killer}`），task 用 `with/monster` 联动上报，代替夹具在 Strike 里直接 `Report` |
| 多人战斗（`AttackSeq` 单调输入、`CooldownUntil`、同图校验、伤害 + Buff） | combat | combat 只限两名玩家（compose 0D 第 2 档）。要装：1）玩家对怪物（PvE）与 N 人对同一目标；2）单调输入序号 + 冷却的幂等重放；3）与 Monster 实体同事务（多实体锁）；4）Buff 用 Player 定时器（现在夹具 `add timer --entity Player` + 回调），模块要么自带定时器类型号、要么留扩展点 |
| `InstanceComplete`（Boss 死亡才完成副本） | dungeon | dungeon 要独立 Dungeon 实体与关卡程序、每局固定经验、要 profile；夹具的“副本 = Scene + Boss 已死”是另一种模型。要装：1）副本完成事实可挂在已有实体上（Scene / Room）；2）完成条件由 monster 的击杀事件驱动；3）领奖仍是有界账本 |
| 两人队伍 `PartyID` | team | team 是 Remote 实体，REAL 档要多 Redis / NATS；夹具是两个 Player 同事务。CellWorld 那轮若定 Q5 B，MMORPG REAL 依赖要跟着扩，排在 R3 之后 |
| Trade 托管 | 无（借鉴 friends） | 若以后做交易模块：Trade 实体 + 两 Player 同事务托管 + 期限到期清理，可从本夹具抽；物品实例（bag 只有数量）是前置 |

## 拿不准的点

1. **Quest 的重放语义**：handler 把 `task_claimed` 当成功，保留原“领奖重放幂等”的场景语义；如果维护者希望直接用 task 的 `TaskClaim` 协议（重放答 `task_claimed`），`MMORPGQuest` 可以删掉，但 Combat 与两个 REAL 用例的“重放不报错”要改成“重放答 task_claimed”。
2. **金币上限**：保留了夹具 1,000,000 的持有上限（读 `Balance()`），wallet 本身没有；也可以删掉改由 `wallet_overflow` 兜底，但那样托管时“卖家收得下”的预留就没了。
3. **道具上限两处写**：`item.csv` 的 `max_stack` 1000 与 handler 的 `mmoItemLimit` 1000 必须一致（托管时预留买家容量要用这个数）；改表忘改常量会让托管预留与 bag 的实际上限不一致。
4. **task 的缺省每日任务**：wallet 每笔扣款都给缺省的每日任务 2（`wallet.spent`）记 1 次，转账与托管会推进它。这是模块联动的正常行为，夹具不断言它。

## 交给协调者写的内容（本批没改 CHANGELOG / KNOWN-LIMITS）

CHANGELOG `[Unreleased]` 建议加：

- 变更：MMORPG 玩法夹具改为“装模块 + 手写玩法核心”（R9 批 2B）。装 `wallet bag task`：金币走 wallet，交易与掉落的材料是 bag 道具 3001，击杀任务是 task 的永久任务 201（击杀时 `Report`，领奖 `Claim` 经 wallet）；队伍、场景、怪物、战斗、副本完成按 Q5 A 留手写，交易托管借鉴 friends。场景名与条数（14）不变，旧断言 → 新断言见 MMORPG-INTEGRATION §4.5；REAL 未投影 WAL 仍 16 条。

KNOWN-LIMITS：

- “模块化顺序（N1）”一条：MMORPG 已按 R9 改造，模块清单 `wallet bag task`；队伍 / 场景 / 怪物 / 战斗 / 副本留到 CellWorld 进模块那一轮。
- “游戏接入验收”MMORPG 一行：缺省档另跑 `module verify` 5 项；REAL 2026-11-17 复跑通过。
