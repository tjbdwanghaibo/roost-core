# R9 方案：13 类玩法改成“装模块 + 只写模块没覆盖的部分”（2026-11-17）

状态：**13 类玩法已实施**（2026-11-17：批 0、批 1 的 1A～1D、批 2 的 2A～2D 全部合入 main；收尾时维护者又定了三件事：模块场景测试先装工程配置表、`module verify` 只统计模块自带文件里的用例、SLG 恢复永久到账回执，见 §9.5）。**CellWorld 进模块这一波先不做**（维护者 2026-11-17），记在 KNOWN-LIMITS 二。以下是实施时的原文：维护者 2026-11-17 答复第 7 节 Q1～Q8 **都按推荐**，并同意每类玩法脚本加跑 `module verify`；R3 与 CellWorld 进模块也要做（先后见第 8 节）。实施时各 agent 在主检出 main 上按文件分工、用 pathspec 提交（与 v1.52.0 整理轮相同），不另开 worktree。**批 0 已完成**（2026-11-17：公共 `genre-common/modules.sh`、pretag 补 `env -u`、塔防试点；定型写法见[第 9 节](#9-批-0-定型的写法批-1--批-2-照做)，证据 [r9-batch0-20261117](evidence/r9-batch0-20261117/README.md)）。基线 main v1.52.0（`f6699655`，最低 core 提升 `d1a6ab2a`）。来源：维护者 2026-11-14 的 D21 A（[决定原文](evidence/known-limits-remaining-20261114/decisions.md)），交接 [HANDOFF §四](HANDOFF-2026-11-14-v147-leftovers.md#四维护者答复后要实施的2026-11-14) 的 R9，[KNOWN-LIMITS](KNOWN-LIMITS.md)“模块化顺序（N1）”。组合规则以 [compose.md](../agent-skills/roost-genre/references/compose.md) 为准（四条规则：遵循核心思路、能复用就装、不能就借鉴最近的模块、需求与限制冲突必须提出）。

读者：维护者（第 7 节要答的问题）与后续实施 agent（第 3～5 节照做）。

## 0. 结论

- **模块能接走的主要是“局外”**：玩家货币（wallet）、道具（bag）、体力（stamina）、抽卡（gacha）、建档与首次发放（enter）、房间席位与重连代际（room）、顺序回合（turn）、任务（task）、好友（friends）。**玩法核心没有模块**：塔防寻路与波次、自走棋共享卡池、合成棋盘、放置生产队列、帧同步、SLG 队列与抢地、MMO 多人战斗与交易、派对房间的心跳与记分板。这些按 compose 规则 3“借鉴”留手写，夹具里写明样板是哪个模块。
- **第一轮基本不改模块**：第 3 节列出的模块缺口，推荐都“不补、改夹具语义或留手写”；唯一可能动模块的是多货币（第 7 节 Q2，推荐不动）。工作量集中在夹具、生成脚本与玩法文档。
- **做法**：每类脚本在 `project new -template game` 之后执行 `roost module add <清单>`，再拷贝剩下的手写夹具；**场景名与条数不变**，断言改成读模块状态；脚本多跑一次 `roost module verify` 核对已装模块的场景数。
- **分批**：批 0（公共脚手架 + 塔防试点，1 个 agent）→ 批 1（4 个 agent 并行）→ 批 2（4 个 agent 并行）→ 收尾全量。墙钟约 12～14 小时，见第 5 节。
- **开工前置**：维护者答第 7 节 Q1～Q8；同一检出里正在改 `modules/room`、`wiring/gate`、`demo/**` 的三个 agent 合入之后再开（房间类玩法依赖 room 的现行 API）。

## 1. 13 类玩法清单

以 `scripts/test-genres-generated.py` 的 `GENRES` 与 `scripts/pretag.sh` 的 `genre_scripts` 为准。pretag 里另外的 heartbeat、level、sync-modes、mmorpg-scenes（用 `-template game-demo`，归 demo）、modules、gate 不是玩法，不在 R9 范围。

| # | 玩法 | 脚本 | 夹具目录 | 缺省档（前缀 / 条数） | REAL 档（开关 → 依赖 → 用例） |
| --- | --- | --- | --- | --- | --- |
| 1 | SLG | `scripts/test-slg-generated.sh` | `scripts/testdata/slg/` | `TestSLG` 8（含 Robot；第 8 条是维护者 2026-11-17 加的原服奖励越过 wallet 窗口回归）+ 联盟纯内存 2 条 | `ROOST_SLG_REAL=1` → Mongo + Redis + NATS（持 `remote-acceptance.lock`）→ `TestSLGAllianceActivity`、`…PlayerDelivery`、`…RemoteDelivery`、`…RPCReceiverBudget` |
| 2 | MOBA 帧同步 | `scripts/test-moba-generated.sh` | `scripts/testdata/moba/` | `TestMOBA` 28 | `ROOST_MOBA_REAL=1` → Mongo → `TestMOBARealSettlementAcrossProcesses`（write / recover）、`…ActiveRoundCrashRecovery`、`…ActiveRoundSnapshotRecoveryAfterSIGKILL`；再加 `ROOST_MOBA_ROUTING=1` → NATS → `…SnapshotDuringHostHandoff`、`…CrossSIDHostRouting` |
| 3 | MMORPG | `scripts/test-mmorpg-generated.sh` | `scripts/testdata/mmorpg/` | `TestMMORPG(Party\|Trade\|Combat\|Buff\|Transfer\|Death\|Robot)` 14 | `ROOST_MMORPG_REAL=1` → Mongo → `TestMMORPGRealPersistenceAcrossProcesses`（write / recover）、`TestMMORPGRealCrashRecovery` |
| 4 | 卡牌 RPG | `scripts/test-card-rpg-generated.sh` | `scripts/testdata/card-rpg/` | `TestCardRPG` 17 | `ROOST_CARD_RPG_REAL=1` → Mongo → `TestCardRPGRealCrashRecovery` |
| 5 | 合作塔防 | `scripts/test-tower-defense-generated.sh` | `scripts/testdata/tower-defense/` | `TestTowerDefense` 11 | `ROOST_TOWER_DEFENSE_REAL=1` → Mongo → `TestTowerDefenseRealCrashRecovery` |
| 6 | 生存撤离 | `scripts/test-survival-extraction-generated.sh` | `scripts/testdata/survival-extraction/` | `TestSurvivalExtraction` 13 | `ROOST_SURVIVAL_EXTRACTION_REAL=1` → Mongo → `TestSurvivalExtractionRealCrashRecovery` |
| 7 | 合成休闲 | `scripts/test-merge-casual-generated.sh` | `scripts/testdata/merge-casual/` | `TestMergeCasual` 16 | `ROOST_MERGE_CASUAL_REAL=1` → Mongo → `TestMergeCasualRealCrashRecovery` |
| 8 | 放置模拟 | `scripts/test-idle-sim-generated.sh` | `scripts/testdata/idle-sim/` | `TestIdleSim` 13 | `ROOST_IDLE_SIM_REAL=1` → Mongo → `TestIdleSimRealCrashRecovery` |
| 9 | 自走棋 | `scripts/test-auto-chess-generated.sh` | `scripts/testdata/auto-chess/` | `TestAutoChess` 13 | `ROOST_AUTO_CHESS_REAL=1` → Mongo → `TestAutoChessRealCrashRecovery` |
| 10 | 派对房间 | `scripts/test-party-room-generated.sh` | `scripts/testdata/party-room/` | `TestPartyGame` 19 | `ROOST_PARTY_ROOM_REAL=1` → Mongo → `TestPartyGameRealCrashRecovery` |
| 11 | 回合 PvP | `scripts/test-turn-pvp-generated.sh` → `genre-common/run.sh` | `scripts/testdata/turn-pvp/`、`turn-pvp-real/` | `TestScenario` 10 | `ROOST_TURN_PVP_REAL=1` → Mongo → `TestTurnPVPRealTimerRecovery`（seed / recover）；再跑 `scripts/test-turn-pvp-real.sh`（`-recipe turn-pvp`，Mongo + NATS + Redis + 自起 etcd、Gate、SIGKILL） |
| 12 | 合作 Roguelike | `scripts/test-coop-roguelike-generated.sh` → `genre-common/run.sh` | `scripts/testdata/coop-roguelike/` | `TestScenario` 10 | `ROOST_COOP_ROGUELIKE_REAL=1` → Mongo → `TestCoopRoguelikeRealCrashRecovery` |
| 13 | 异步社交经营 | `scripts/test-social-sim-generated.sh` → `genre-common/run.sh` | `scripts/testdata/social-sim/` | `TestScenario` 9 | `ROOST_SOCIAL_SIM_REAL=1` → Mongo → `TestSocialSimRealCrashRecovery` |

共同部分：1～10 类经 `scripts/testdata/robot-common/install.py` 装业务 Robot（`<玩法>/robot/*`）；11～13 类用 `genre-common` 的 TCP Robot。每个脚本都跑 build / vet / `roost id check` / `roost generate --check`；本机 pretag 耗时 13～32 秒一个（`evidence/round-20261116/pretag-run2.txt`）。REAL 环境：`source $HOME/.roost-it/roost-dataengine-it/env.sh`（偏移 1000 的长期环境）。

调研中顺带发现（不在 R9 里修，记在这里）：

- pretag 的 `env -u` 清 REAL 开关时漏了 `ROOST_TURN_PVP_REAL`、`ROOST_COOP_ROGUELIKE_REAL`、`ROOST_SOCIAL_SIM_REAL`：外层 shell 带着这三个开关跑 pretag 会去连真实存储。批 0 顺手补上。
- 玩法参考的条数与脚本对不上：`slg.md` 写 `TestSLG` 6、脚本断言 7；card 16 / 17、放置 12 / 13、合成 15 / 16、塔防 / 撤离 / 自走棋 10 / 12 / 12 对 11 / 13 / 13（都差加进来的业务 Robot）。各批改文档时一并改成脚本的数。

## 2. 模块的装配事实（本轮用到的部分）

- 模块装在名为 `Player` 的实体上（room / turn / matchroom 装在模块自带的 `Room` 实体上），状态是主 DAO 下的嵌套结构体（[KNOWN-LIMITS“模块安装绑定 Player 与工程形状”](KNOWN-LIMITS.md)）。玩法自己的世界实体（TowerRoom、CardRun、Tile……）照常 `roost add entity`。
- **本机实测**（scratchpad，2026-11-17）：`project new -template game` → `add dao Player` → `add entity Scene` / `GameRoom` + DAO → `module add turn`（连带 room）、`module add wallet` → `go build ./...` 通过，说明“玩法实体 + 模块”能共存。`module plan scene` 报 `module scene: owned entity Scene conflicts with existing game/entities/scene/entity.go`，`module plan monster` 同样（monster 依赖 scene）。
- **路径冲突**：MMORPG 夹具的 `Scene` / `Monster` 实体与 scene / monster 模块撞名；SLG 夹具的 `game/modules/alliance/alliance.go` 与 alliance 模块的同名文件撞路径。夹具 genre-common 写 `game/modules/rules/`，与 rules 模块（`game/components/rules/`）不撞路径但同名，读代码易混。
- 协议号：夹具手写 15001～15003、15011；安装器从 10000 起取最小空号并扫已有标记，先装模块或先拷夹具都不撞。
- 模块的前置：enter / rules / idle 等的 `plan` 会代办 `add access player`、`add transport tcp` 与打开 `self_sync`。派对房间脚本里手写的这几步要挪到 `module add` 之后，只追加 `sync_groups`。

## 3. 逐类映射

结论用 compose 第 1 步的词：**装**（直接装）、**扩展**（装了再扩展：改模块留给工程的文件、监听实体事件、调组件入口）、**借鉴**（手写，写明样板）、**手写**。“模块缺口”一栏写要不要给模块补、补在哪。

### 3.1 回合 PvP（turn-pvp）

| 夹具里的块 | 结论 | 模块 / 联动 | 说明 |
| --- | --- | --- | --- |
| GameRoom 成员名单（宿主给名单，`Membership` 场景） | 装 + 扩展 | room | 服务端 `room.Open` 开房，成员 `RoomJoin`；名单校验写 `event.RoomJoining` 监听（借鉴 matchroom 的“固定名单”） |
| 轮到谁、命令序号、最后一条可重放、回合期限与超时推进（`Turn` / `Last` / `Deadline` + GameRoom 定时器） | 装 | turn | 按 turn README“接上游戏动作”：`CheckEnd` → 改玩法状态 → `End`；超时由 turn 的 `TurnDeadline` 推进，GameRoom 的 `add timer` 删掉 |
| 攻击、HP、胜负、终局不能再打 | 扩展 | turn | Room DAO 上加玩法自己的字段（HP、胜者），handler 收 `view.RoomEntity` |
| 期望版本 `Revision`（`StaleRevision` 场景） | 改语义 | turn | 用 turn 的 round + sequence 代替，场景名保留，断言改为“旧 round 被拒” |
| 奖励 `Player.Coins` | 装 | wallet | `Wallet().Apply(业务键, n)`，键 = 房号 + 回合 |
| PB 协议 / Robot | 手写 | — | 请求加 `Epoch`，分发取 `room.ConnectionKey(session)` |

REAL：`turn-pvp-real/timer_test.go.txt` 从 GameRoom 定时器改测 Room 的 `TurnDeadline`（仍 seed / recover 两段）；`test-turn-pvp-real.sh` 的配方演练不变。**纵切与配方从此是同一套 Room/Turn 模型**（Q7）。模块缺口：无。

### 3.2 合作 Roguelike（coop-roguelike）

| 夹具里的块 | 结论 | 模块 | 说明 |
| --- | --- | --- | --- |
| 双人席位、成员校验、重连 | 装 | room | 同 3.1 |
| 同时投票、路线种子、三关推进 | 借鉴（样板 turn） | — | turn README 写明“所有人同时提交的回合”不适合；借鉴它的“每人连续命令号 + 最后一条可重放”，状态放 Room DAO |
| 通关领奖恰好一次（`Claimed` + Coins 双实体回滚） | 手写 + 装 | wallet | `Claimed` 留在 Room，金币走 wallet，同一笔 `[Player, Room]` 事务 |
| 只读成员视图 PB、完整 Robot | 手写 | — | 保留 |

模块缺口：无。

### 3.3 异步社交经营（social-sim）

| 夹具里的块 | 结论 | 模块 | 说明 |
| --- | --- | --- | --- |
| 主人 / 受邀访客的权限 | 装 | friends | 访客须是好友；friends 的申请 / 同意要求双方本进程在线，场景准备阶段满足 |
| 种植、成熟时刻、帮助缩短一次、收获 | 借鉴（样板 friends） | — | 状态从 GameRoom 挪到**主人的 Player DAO 嵌套字段**（compose 0A“挂在已有实体上的状态是 DAO 的嵌套结构体”）；帮助是 `[主人, 访客]` 两个 Player 的事务，照 friends 的写法 |
| 收获奖励 | 装 | wallet | 同上 |

不再需要 GameRoom。模块缺口：无。

### 3.4 放置模拟（idle-sim）

| 夹具里的块 | 结论 | 模块 | 说明 |
| --- | --- | --- | --- |
| 建档 `handlerIdleInit` | 装 + 扩展 | enter | 初始存档写在 `event.EnterFirst` 监听里 |
| Gold | 装 | wallet | 存档 JSON 的回执里不再记 Gold 快照，严格解码的范围校验随之改 |
| Gems | Q2 | bag（推荐） | 作为 bag 道具；“发过没有”记在玩法回执里，与 bag 同一笔事务 |
| 每日奖励（跨日不倍增） | 装 + 扩展 | checkin | 连续加成在 `game/components/checkin/checkin.go` 配平；`DailyBoundaryAndMissedDaysDoNotMultiplyRewards` 改读 checkin 状态 |
| 三工坊生产队列、原料预扣与取消退款、收益窗口与倍率积分、产出缓冲、仓库容量、出售、订单 | 借鉴（样板 idle） | — | idle 模块只有一条金币线，README 写明“没有多产线、生产队列”；借鉴它的“游标只前移整周期 + 单调序号 + 回拨拒绝” |
| 配置冻结写进存档、唤醒定时器 | 手写 | — | 保留 |

模块缺口：idle 多产线——**不补**（玩法核心）；bag 总容量（仓库上限）——不补，调用 `bag.Add` 前手写校验。

### 3.5 合成休闲（merge-casual）

| 夹具里的块 | 结论 | 模块 | 说明 |
| --- | --- | --- | --- |
| 首登建档 | 装 + 扩展 | enter | `EnterFirst` 写初始棋盘；设备代际接管（`DeviceTakeover…`）留手写 |
| Gold | 装 | wallet | 同 3.4 |
| 体力 Energy 惰性恢复 | 装（改语义，Q1） | stamina | stamina 的规则每次读写取当前值，夹具是首登冻结；只把**体力**改成跟 stamina，棋盘规则仍冻结。生产器充能 `Charges` 留手写 |
| 广告回调与每日次数 | 借鉴（样板 checkin + wallet） | — | 业务日判断照 checkin，到账键照 wallet |
| 棋盘、仓库、合成、生产器、出售撤销、订单、离线批次、限时挑战 | 手写 | — | 玩法核心 |

模块缺口：stamina 的“按存档冻结规则”——不补（与 stamina README“改规则立刻对所有人生效”相反，且只此一类要）。

### 3.6 卡牌 RPG（card-rpg）

| 夹具里的块 | 结论 | 模块 | 说明 |
| --- | --- | --- | --- |
| 建档 `handlerCardInit` | 装 + 扩展 | enter | 初始卡组写在 `EnterFirst` |
| Gold | 装 | wallet | — |
| Gems、Dust | Q2 | bag（推荐） | — |
| Energy 恢复与开局扣体力 | 装 | stamina | 开局 handler 在同一事务调 `Stamina().Spend` |
| 抽卡、保底、十连、请求回执 | 装 + 扩展（改语义，Q1） | gacha（连带 wallet、bag） | 卡牌自己的 handler 调 `Gacha().Draw` 拿结果，同一笔事务把道具转成卡牌实例、重复卡转碎片（gacha README“不做卡牌系统”）。gacha 失败的尝试会重新抽样，`DrawRefusalAndUndoKeepFundsAndRandomStream` 改为“资金不变、成功结果由回执固定”；扣费改用 gacha 的金币或钻石（随 Q2） |
| 养成、编队、战斗快照 | 手写 | — | — |
| 每张卡的属性 `HeroStats` | 手写 | — | 保留 `roost add attribute HeroStats`；attribute 模块是玩家级三属性，不覆盖卡牌 |
| CardRun 三人编队回合 PvE + 回合期限 | 借鉴（样板 turn） | — | turn 要求房间里两名以上在座成员，单人 PvE 不适用 |
| 首通、扫荡 | 借鉴（样板 dungeon） | — | dungeon 每局固定 100 经验、无扫荡、要 profile；借鉴它的有界领奖账本 |
| 任务领奖 | 装 | task | 胜利时 `Report`，奖励经 wallet |

模块缺口：gacha 的确定性随机流、卡牌实例——**不补**（gacha README 明确选了“随机不可验证”与“不做卡牌系统”）。

### 3.7 合作塔防（tower-defense，批 0 试点）

| 夹具里的块 | 结论 | 模块 | 说明 |
| --- | --- | --- | --- |
| `Player.Initialized` 建档 + Gold | 装 | enter、wallet | — |
| TowerRoom 席位、准备、断线、旧连接 | 装 + 扩展（Q3） | room | TowerRoom 并入 Room：塔防状态成为 Room DAO 自己的字段；夹具的 `Connect(ticket)` 换成 room 的 `Reconnect(epoch, connection)` |
| 宿主给名单开局 `handlerTowerInit(ids)` | 扩展 | room | `room.Open` + `RoomJoining` 名单监听 |
| 5×5 地图、阻路 BFS、建造 / 升级 / 出售、波次、胜负、配置冻结 | 手写 | — | 玩法核心 |
| 波次期限 | 手写 | — | Room 上的实体定时器加一个类型（turn README：已有定时器时复用） |
| 团队领奖 | 手写 + 装 | wallet | `Claimed` 在 Room，金币走 wallet |

### 3.8 生存撤离（survival-extraction）

同 3.7 的房间做法（SurvivalRaid 并入 Room，Q3）。付费入场与兑付金币 → wallet；永久兑付的物资 → bag；共享采集、制作预扣、环境伤害与治疗、死亡掉落与拾取、撤离读条 → 手写。制作容量上限在调用 `bag.Add` 前手写校验。模块缺口：无。

### 3.9 自走棋（auto-chess）

ChessMatch 并入 Room（容量 8，Q3）；按排名领奖 → wallet；建档 → enter。八席共享卡池与商店、经济与升级、合星、编队、准备 / 战斗阶段、确定性战斗与配对、断线接管 → 手写（shop 模块是“金币买道具”，不是共享卡池；turn 不适合同时准备的阶段）。模块缺口：无。

### 3.10 派对房间（party-room）

| 夹具里的块 | 结论 | 模块 | 说明 |
| --- | --- | --- | --- |
| `Coins`（`persist,sync`，经自身视图下发） | 装 | wallet | wallet 的状态本来就标 sync |
| `Initialized` 建档 | 装 | enter | — |
| PartyRoom：心跳 100ms 推进、SyncGroup 公开记分板、房间码、房主转移、断线宽限、补位 | 借鉴（样板 room，Q3 推荐不换） | — | room 不广播、不装 EntitySync（D24 A 把广播暂缓）；匹配用 `service/match` 弹性人数与补位，matchmaking 模块只做同 sid、同人数整队、无补位。借鉴 room 的代际围栏 |

模块缺口：room 广播、matchmaking 弹性人数与补位——**不在 R9 补**（分别是 D24 A 暂缓项与 R7 边界）。

### 3.11 MMORPG

| 夹具里的块 | 结论 | 模块 | 说明 |
| --- | --- | --- | --- |
| Gold（交易、掉落、扣费） | 装 | wallet | — |
| Items | 装 | bag | — |
| 击杀任务 `QuestKills` / `QuestClaimed` | 装 | task | 击杀时 `Report`，领奖经 wallet |
| 两人队 `PartyID` | 手写（Q5） | — | team 是 Remote 实体，REAL 档要多 Redis / NATS；夹具是两人同事务样例 |
| Scene 实体（人数、切图版本） | 手写（Q5） | — | 与 scene 模块撞名；换 scene 等于重写 Transfer 场景，且与 demo 的 mmorpg-scenes 脚本关系要重理 |
| Monster 血量、掉落归属、Boss 存活 | 手写 | — | monster 模块不持久、无掉落（README） |
| 攻击、冷却、Buff / 复活定时器 | 手写 | — | combat 只限两名玩家（compose 0D 第 2 档：借鉴 MMORPG 参考本身） |
| Trade 托管 | 借鉴（样板 friends） | — | 无模块，两 Player 事务照 friends |
| `InstanceComplete` | 手写 | — | dungeon 要独立 Dungeon 实体与关卡程序，留到 CellWorld 那轮一起看 |

模块缺口：monster 持久与掉落、combat 多人——**不在 R9**（属于 CellWorld / MMO 进模块那轮）。

### 3.12 SLG

| 夹具里的块 | 结论 | 模块 | 说明 |
| --- | --- | --- | --- |
| Gold | 装 | wallet | 建造 / 研究 / 行军扣费改调 wallet |
| 建造、研究、行军队列 + Player 定时器、取消退款 | 手写 | — | 玩法核心 |
| 抢地（Tile 多实体事务） | 手写 | — | — |
| 离线收益（定时器周期发、离线封顶） | 手写 | — | idle 是“领取时惰性结算”，`TestSLGOfflineIncomeIsBusinessBounded` 断言的是定时器补发，语义不同 |
| 联盟活动教学消费者（`service/activity` 协调器、Redis CAS 联盟、AllianceAuthority Remote 托管、原服奖励义务账本） | 手写（Q4 推荐保留） | — | 这是框架原语的教学，REAL 四条都建在它上面；alliance 模块已有自己的真实验收（P7 / P8） |
| 原服奖励到账（`alliance_rewards`） | 扩展 | wallet | 到账改 `Wallet().Add`，Player 上的永久到账回执 `SLGReceipts` 保留，义务账本仍手写（维护者 2026-11-17 收尾时定：恢复永久回执，`34147a1e`；2C 第一版的 `Apply(业务键)` 在 wallet 128 次窗口外会重复发） |

模块缺口：无。

### 3.13 MOBA 帧同步

Gold → wallet；Wins / Losses 战绩、MobaRewards 原服奖励账本 → 手写；lockstep 对局、参考模拟、宿主与恢复、对局目录、结算记录 → 手写（D5 已定 Lockstep 房间与夹具不迁 `Stepper`）；匹配用 `service/match` 的 `CommitAcross` 跨分区成局，matchmaking 模块只做本 sid → 手写。这一类模块化后改动最小。模块缺口：无。

## 4. genre-common 的去向

genre-common 只服务 11～13 类（回合 PvP、合作、经营）。

| 文件 | 去向 |
| --- | --- |
| `run.sh` | **留**（公共生成机制）。加“按玩法装模块”一步（模块清单按玩法给）；删 `add entity GameRoom` / `add dao GameRoom`、turn-pvp 的 `add timer --entity GameRoom`（Room 与定时器来自模块）、往 Player 插 `Coins` 的一段（wallet）。场景数断言 9 / 10 / 10 不变 |
| `handlers.go.txt`（GameRoom 上的 Open / Command） | **拆到各玩法**：turn-pvp 用 turn 的 `CheckEnd` / `End`，合作写 Room 上的投票 handler，经营写 Player handler。`run.sh` 已支持玩法自带 `handlers.go.txt` 覆盖 |
| `protocol.go.txt`、`network.go.txt` | **留**公共 PB 外壳，请求加 `Epoch`，分发取连接标识 |
| `gameplay_test.go.txt`（4 条公共场景） | **留**，改为调用各玩法提供的小接口（构造、发命令、读余额）：重试与冲突、提交被拒整笔回滚（wallet 与 Room 一起）、旧版本被拒、非成员被拒 |
| `network_test.go.txt`、`robot*.go.txt`、`robot_README.md` | **留**（TCP Robot 外壳） |
| `persistence/runtime_test.go.txt` | **留**真实 Runtime 接线，实体集合改为参数（合作：Player + Room；经营：两个 Player） |

**变成模块的**：`Player.Coins` → wallet；GameRoom 的成员与席位 → room；命令序号、回执、期限（只 turn-pvp）→ turn。**不新增模块**。批 0 新增的公共装模块脚本（建议 `scripts/testdata/genre-common/modules.sh`：装清单、跑 `module verify --json`、按各模块 `module.yaml` 的 `verify.pass` 核对条数）13 类共用。

## 5. 实施分批

**全体规则**：在主检出 main 上按下表分工、`git commit -- <paths>` 提交，不开 worktree（维护者 2026-11-17 定；原写的“每个 agent 一个独立 worktree 与分支”作废）。下列文件**只由批 0 或协调者改**：`CHANGELOG.md`、`docs/maintenance/KNOWN-LIMITS.md`、`docs/agent-skills/roost-genre/references/compose.md`、`modules/**`、`scripts/test-modules-generated.sh`、`scripts/testdata/robot-common/**`、`scripts/pretag.sh`、`scripts/test-genres-generated.py`。各 agent 把要写进 CHANGELOG / KNOWN-LIMITS 的话写在交接说明里。不嵌套子 agent；跑全量前看 `df`；跑完清理测试进程与私有环境；同一时刻至多两个 agent 跑 REAL（共用 `~/.roost-it`）。

**每个 agent 的验收**（缺一不算完）：

1. `bash scripts/test-<玩法>-generated.sh`：缺省档条数与第 1 节相同、0 SKIP；build / vet / `id check` / `generate --check` 通过；新增的 `module verify` 每个已装模块条数等于 `module.yaml` 的 `verify.pass`、0 skip。
2. `python3 scripts/test-genres-generated.py --out <scratchpad 下的新目录> --genres <玩法…>`：status passed（自定义 module 名下同一批用例再跑一遍）。
3. REAL：`source $HOME/.roost-it/roost-dataengine-it/env.sh` 后 `ROOST_<玩法>_REAL=1 bash scripts/test-<玩法>-generated.sh`，第 1 节列的用例全过、0 SKIP。
4. 改过断言的场景：把模块调用去掉或改错（例如 `Apply` 换成不带键的 `Add`）证明变红，再恢复变绿，红绿日志留在交接说明。
5. 主检出 `git status` 无残留；`GOWORK=off go test -count=1 -run 'Link|Anchor|Doc' .` 通过。
6. 同步改玩法手册（`docs/framework/<玩法>-INTEGRATION.md` 或 `SESSION-GENRES.md`）与 skill reference（`references/<玩法>.md`）：模块清单、手写部分及样板、场景语义的变化（旧断言 → 新断言）、条数。

| 批 | agent | 玩法 | 独占的文件 | 墙钟（估） |
| --- | --- | --- | --- | --- |
| 0 | 0A | 公共脚手架 + 塔防试点 | `genre-common/modules.sh`（新）、`pretag.sh` 的 `env -u`、`test-genres-generated.py`（如需）、`test-tower-defense-generated.sh`、`testdata/tower-defense/**`、`TOWER-DEFENSE-INTEGRATION.md`、`room-coop-pve.md` 的塔防部分 | 2～3 h（Q2 选 A 再加 3～4 h） |
| 1 | 1A | 回合 PvP、合作、经营 | `genre-common/**`（除 `modules.sh`）、`testdata/{turn-pvp,turn-pvp-real,coop-roguelike,social-sim}/**`、三个脚本、`SESSION-GENRES.md`、`session-genres.md` | 3～4 h |
| 1 | 1B | 放置、合成 | `testdata/{idle-sim,merge-casual}/**`、两个脚本、`IDLE-SIM-` / `MERGE-CASUAL-INTEGRATION.md`、`idle-merge-casual.md` | 3～4 h |
| 1 | 1C | 卡牌 RPG | `testdata/card-rpg/**`、脚本、`CARD-RPG-INTEGRATION.md`、`card-turn-based.md` | 3 h |
| 1 | 1D | 生存撤离、自走棋 | `testdata/{survival-extraction,auto-chess}/**`、两个脚本、两份手册、`room-coop-pve.md`（批 0 之后） | 3～4 h |
| 2 | 2A | 派对房间 | `testdata/party-room/**`、脚本、`PARTY-ROOM-INTEGRATION.md`、`party-room.md` | 1.5～2 h |
| 2 | 2B | MMORPG | `testdata/mmorpg/**`（不动 `scene_runtime*` 与 `crosssid`）、脚本、`MMORPG-INTEGRATION.md`、`mmorpg.md` | 3～4 h |
| 2 | 2C | SLG | `testdata/slg/**`、脚本、`SLG-INTEGRATION.md`、`slg.md` | 2 h |
| 2 | 2D | MOBA | `testdata/moba/**`、脚本、`MOBA-INTEGRATION.md`、`moba-lockstep.md` | 1.5 h |
| 收尾 | 协调者 | 全部 | CHANGELOG `[Unreleased]`、KNOWN-LIMITS N1 改写、compose.md 里“夹具与 demo”一句、合并 | 2～3 h |

批 0 的试点用来定型：模块清单怎么写进脚本、房间类玩法并入 Room 的写法、场景断言改读模块状态的写法、`module verify` 的核对方式。批 1 / 批 2 照试点做。

**收尾全量**（按“慢的套件最后跑一次”）：干净 worktree 里 pretag 的 `genre_scripts` 全部；`python3 scripts/test-genres-generated.py --out <新目录>`（13 类）；13 类 REAL 串行（约 1 小时，SLG 与 turn-pvp 要 Redis / NATS / etcd）；模块有改动时 `bash scripts/test-modules-generated.sh`；文档链接测试。

**墙钟合计**约 12～14 小时（批 0 约 2.5、批 1 约 4、批 2 约 4、收尾约 2.5），agent 时长约 28 小时。估计依据：每个脚本缺省档 15～35 秒、REAL 每条 1～3 分钟，主要时间花在改夹具与场景断言。

## 6. 风险

1. **场景名不变、含义变了**：换成模块后断言读的是模块状态，有几条语义确实变了（卡牌抽卡随机流、合成体力规则冻结、回合 PvP 的版本号）。缓解：每条改过断言的场景在玩法手册里写“旧断言 → 新断言”，并按验收第 4 条先红后绿。
2. **存档 JSON 与模块状态拆开**：放置、合成、卡牌的回执里记着 Gold 快照，严格解码会校验 Gold 范围（`MalformedState…FailsClosed`、`InvalidSnapshots…` 一类场景）。拆出 wallet 后这些校验与回执都要改，是本轮工作量最大的部分。
3. **房间类玩法并入 Room 的连接身份**：room 要求可信的连接标识（`ConnectionKey`），夹具现在用 ticket。离线场景要像 room 模块的测试那样造 Session；改错会让旧连接能操作新席位。
4. **room 正在被改**：同一检出里有 agent 在改 `modules/room`。开工前重读 room README 的现行 API，批 0 之后 room 再变要回到批 0 复核。
5. **脚本变慢**：多了 `module add` 与 `module verify`，每个脚本估计多 30～60 秒；pretag 以 4 路并行，总共多约 3 分钟。
6. **REAL 依赖不要扩大**：推荐的模块清单里没有要 Redis / NATS 的（team、alliance、matchmaking 都没选），REAL 依赖与现在相同。Q5 若选 team，MMORPG 的 REAL 要加 Redis / NATS。
7. **自定义 module 名矩阵**：`test-genres-generated.py` 只改写 `.go/.yaml/.json/.proto/go.mod`；模块装进来的配置表（`.csv`）若带 module 路径会漏改。批 0 核对一遍。
8. **路径冲突的遗留**：SLG 夹具包 `game/modules/alliance` 与 alliance 模块同路径；本轮不装 alliance 不冲突，但工程以后要装 alliance 模块会失败。Q4 选 A 时在 SLG 手册里写明，必要时把夹具包改名（建议 `slgalliance`）。

## 7. 要维护者决定的问题

| # | 问题 | 选项 | 推荐 |
| --- | --- | --- | --- |
| Q1 | 夹具语义与模块语义不同时怎么办（卡牌抽卡失败重抽、合成体力规则冻结、wallet 128 次去重窗口对比夹具的永久回执） | A 改夹具语义跟模块，场景名与条数不变，手册写明；B 给模块补；C 这一块留手写 | **逐块定，缺省 A**；模块 README 明确“不做”的玩法核心用 C；两类以上玩法都要、且简单工程不必多做事的才 B（“模版可以复杂，能兼容简单就可以”）。按此第一轮没有 B |
| Q2 | 多货币（卡牌 Gems / Gold / Dust，放置 Gold / Gems）：wallet 只有一种货币 | A wallet 加多币种（依赖 wallet 的 checkin、task、stamina、shop、gacha、idle、mailbox、purchase 全部要跟，L）；B 次要货币做 bag 道具；C 主币 wallet、其余留手写 | **B**：本轮不动模块；“wallet 多币种”另立为模块增强候选，不阻塞 R9 |
| Q3 | 房间类玩法是否并入 room 模块的 `Room` 实体（TowerRoom、SurvivalRaid、ChessMatch、GameRoom、PartyRoom） | A 并入：席位、重连代际、房主交接用 room，玩法状态作 Room DAO 自己的字段；B 不并入，只借鉴 | **回合 PvP、合作、塔防、撤离、自走棋 A；派对房间 B**（要心跳 + 组同步记分板 + 补位，room 不广播且 D24 A 暂缓），等 room 广播做了再并 |
| Q4 | SLG 联盟活动 | A 保留夹具的教学消费者（框架原语示范），只模块化玩家经济；B 换成 alliance + activity_alliance 模块，REAL 四条重写，运行要 etcd 与联盟服务 | **A** |
| Q5 | MMORPG 的组队、场景、怪物、副本 | A 本轮只换 wallet、bag、task，组队 / 场景 / 怪物 / 副本留手写，到 CellWorld 进模块那轮一起定；B 本轮换 team + scene + monster（删夹具 Scene / Monster，REAL 加 Redis / NATS，要排在 R3 之后） | **A** |
| Q6 | 登录链路 | A 只装 enter（首次发放走 `EnterFirst`），不装 login / portal，Robot 仍用夹具鉴权；B 13 类都装 login + portal + enter | **A**：真实登录链路已由配方与 `test-turn-pvp-real.sh` 覆盖 |
| Q7 | 回合 PvP 纵切并到 room + turn 后，与 `-recipe turn-pvp` 的关系 | A 两者同一模型，纵切 REAL 的定时器用例改测 Room 的 `TurnDeadline`，配方演练保留；B 纵切保留 GameRoom | **A** |
| Q8 | 要不要给每类玩法新建配方（`modules/recipes/catalog.yaml`） | A 不建，脚本里 `module add`；B 建 13 个配方 | **A**：配方要求带登录链路、面向用户；玩法夹具是参考与门禁。哪类以后要给用户直接起步再单独提 |

另需确认（可按推荐直接做）：genre 脚本里加跑 `module verify`（推荐做，见第 5 节验收第 1 条）。

## 8. 与 R3、CellWorld 及其它条目的先后

- **R3（地形 DAO 按地图归属选库，D11 Q17 A + N3 A）**：改 `modules/terrain`、demo 的地形与换服段。R9 按推荐（Q5 A）不碰 terrain / scene / monster，**两者文件不重叠，可以并行**，R3 可占批 2 的一个名额（批 2 仍不超过 4 个 agent）。Q5 若选 B，MMORPG 那批要排在 R3 之后。
- **CellWorld 业务适配进模块（D21 的后半）**：排在 R9 之后（D21 A），具体在批 2 的 MMORPG 合入之后、demo agent 当前的工作合入之后——它要动 `modules/{scene,monster,actor,cellworld}` 与 `demo/game/partition`，MMORPG 的组队 / 场景 / 怪物也在那时一起定。
- **R5（模块清单声明配置，D19 B）**：独立。先做的一方不受影响，后做的 rebase；R5 先合时，stamina 等的规则来自配置，合成的体力语义（Q1）照新的配置方式写。
- **R7 的 room 部分（断线订阅与重启后在线状态，D24，2026-11-15 已实施）**：房间类玩法直接用它的现行行为；正在改 room 的 agent 合入后以 README 为准。
- **R2（换服 / account）**：与 R9 无文件重叠。

## 9. 批 0 定型的写法（批 1 / 批 2 照做）

塔防试点（2026-11-17）落地后的写法。样板文件：[`test-tower-defense-generated.sh`](../../scripts/test-tower-defense-generated.sh)、[`testdata/tower-defense/`](../../scripts/testdata/tower-defense/)、[`genre-common/modules.sh`](../../scripts/testdata/genre-common/modules.sh)；手册改法见 [TOWER-DEFENSE-INTEGRATION §3.1、§3.5](../framework/TOWER-DEFENSE-INTEGRATION.md#35-模块化之后场景语义的变化r92026-11-17)。

### 9.1 模块清单写进脚本

- 清单是脚本里的一个数组，紧跟 `project new` + `go mod edit -replace` 之后装：

  ```bash
  genre_modules=(room wallet enter)
  modules_sh="$repo_root/scripts/testdata/genre-common/modules.sh"
  bash "$modules_sh" add "$review_root/roost" "${genre_modules[@]}" >> "$review_root/generate.log" 2>&1
  ```

  `modules.sh add` = `roost module add <清单>` + 用 `module list --json` 核对清单里的每个都已登记（依赖由安装器连带装）。清单只写玩法直接用的模块，依赖不必列（turn 会带上 room）。
- 原来脚本里由模块代办的步骤要删掉：`add dao Player --entity Player`（game 模板已有）、给 Player 插 `Gold` / `Coins` / `Initialized`、`add entity <房间>` / `add dao <房间>`、把房间实体改 World 锁档（room 的 `Room` 已是 World）。`add access player` / `add transport tcp` / `self_sync` 由 `module plan` 代办，手写的这几步挪到 `module add` 之后只补模块没做的（派对房间的 `sync_groups`）。
- 模块装完再 `add timer --entity Room`（房间玩法自己的期限）、往模块的主 DAO 追加玩法字段、拷手写夹具，最后装公共 Robot、`roost generate`。顺序固定：**装模块 → add timer → 追加字段 → 拷夹具 → Robot → generate**。
- `module verify` 放在原有的 build / vet / `id check` / `generate --check` 之后、REAL 段之前：

  ```bash
  bash "$modules_sh" verify "$review_root/roost" "$repo_root" "$review_root/module-verify.json" "${genre_modules[@]}" | tee "$review_root/module-verify.txt"
  ```

  它跑 `roost module verify --json`，逐个核对：每个已装模块（以及装上的联动 `<a>+<b>`）通过数 == 本检出 `modules/<a>/module.yaml` 的 `verify.pass`（联动取 `with.<b>.verify.pass`）、0 skip，build / vet / id check / generate --check 全过，清单里的模块都在结果里。结果文件名必须是 `module-verify.json`：`test-genres-generated.py` 看到它就在自定义 module 名下用同一脚本再核一遍（报告字段 `custom_module_verify`）；没迁的玩法没有这个文件，矩阵行为不变。不要把模块场景写进 `*.log`（矩阵会把 `*.log` 里的 `--- PASS` 当成玩法场景再跑）。
- genre-common 三类（批 1A）由 `run.sh` 调同一个 `modules.sh`，清单按玩法给（`run.sh` 的参数或玩法目录里的一个小文件，1A 定）。

### 9.2 房间类玩法并入 room 的 Room（Q3 A）

- **状态**：玩法事实追加到 room 模块的 `db/def/room.go` 的 `RoomDao` 末尾，字段名带玩法前缀（`TowerState string store:"tower_state"`、`TowerWakeTimer int64 store:"tower_wake_timer"`，`dao:"nosync"`）。玩法 JSON 里**删掉**与 room 重复的东西：席位的 `Epoch`、`Connected`，以及夹具自己的 `Connect(ticket)`。保留玩法自己的每席事实（准备、领奖、回执）与名单顺序（`Seats[i].PlayerID`）。
- **handler 参数**：手写一个锁接口，嵌 room 的 Holder 与 Room 的定时器：

  ```go
  type ITowerRoomEntity interface {
  	entity.IThreadSafeEntity
  	Dao() *db.RoomDao
  	view.RoomHolder
  	TimerComp() *roomentity.TimerComponent
  }
  ```

- **开局**：开房与名单同一笔事务——玩法的 Init handler 在 Room 上调 `m.Room().Open(<容量>)` 再写玩法状态；同号重试只核对名单；已有人入座的普通房拒绝改造。生产里成局服务先经生成的 lifecycle `GetOrCreate`（与 `room.Open` 相同）再发 Init。之后玩家经 room 的 `RoomJoin` 入座。
- **名单准入**：在 Room 实体上写 `event.RoomJoining` 监听（`game/entities/room/<玩法>.go`，`//roost:listen`），玩法房只放名单里的人、返回 `apperrors.ErrRoomNotMember`；**玩法状态为空的普通房放行**，否则 room 自带的 34 条场景会挂。
- **身份围栏**：每条玩法命令、回执查询都带 `connection`（接入层从服务端 Session 取：`roomlogic.ConnectionKey(session)`），handler 先 `m.Room().Member(playerID, epoch)`，再核对 `seat.GetConnection() == connection`（否则 `ErrRoomStaleEpoch`）；新命令再要求 `m.Room().Connected(playerID)`，已有回执的重放不要求在线（重启后全员断线时重试仍得原回执）。视图用 `m.Room().View(playerID, connection)` 做成员检查并拿本人代际，各席 `Connected` 取 `m.Room().Connected`。
- **定时器**：`roost add timer --entity Room`，用夹具的 `timer_handlers.go.txt` 覆盖生成的示例，类型号常量带玩法名（`TimerTowerPhase = 1`）。
- **注册**：room 与 Room 定时器的 handler 是按引擎实例注册的（`RegisterRoomJoinNestHandlersOn` 等，`service=game`）；离线 fixture 在 `engine.Start()` 前逐个 `register(engine)`；玩法自己的 handler 参数是手写接口，仍是全局 `Register<文件>NestHandlers()`。
- **夹具里的连接**：fixture 给每席一个固定连接串（`tower-conn-<i>`），入座 `Sync_RoomJoin(room, id, conn)`；断线用 `Sync_RoomDisconnect(room, id, epoch)`（生产由断线订阅自动调），换连接用 `Sync_RoomReconnect(room, id, 旧代际, 新连接)`。Robot 只有 Play 一条消息：在 dispatch 里加 `Kind "reconnect"` 转 `RoomReconnect`，每条新 TCP 连接先接管席位（代际 = `EpochSeed + 1`，用 `robotRead` 在实体锁内读 seed）；接管之后 fixture 的连接已旧，读状态改用 `robotDecode(... GetTowerState ...)`，跑完再 `RoomReconnect` 交还 fixture 的连接。
- **REAL**：多出的 `RoomJoin` 每条是一条 strict 记录，未投影 WAL 计数相应增加（塔防 19 → 23）；冷加载后断言席位与代际都在、`Connected` 全为 false；重放用原连接串与原代际。

### 9.3 场景断言改读模块状态

- 金币：`p.Dao().GetGold()` → `p.Dao().GetWallet().GetCoins()`；领奖 handler 里“领过没有”留在玩法状态（`Claimed`），金币调 `p.Wallet().Add(n)`（不留回执；wallet README：长期“只发一次”由调用方的事实保证）。只防短期重发、没有自己事实的地方才用 `Apply(业务键, n)`。奖励为 0 时不调（`Add(0)` 是 `wallet_invalid`）。
- 建档：删夹具的 `TowerPlayerInit` / `Initialized`，fixture 用 enter 的 `Sync_EnterInit(ctx, id)`（全局 `RegisterEnterInitNestHandlers()`），handler 查 `p.Enter().Built()`。初始发放写在 `game/modules/login/enter_first.go` 的 `firstEnter` 或 `event.EnterFirst` 监听。
- 断线 / 重连：断言改成 room 的返回值（`RoomDisconnect` 的 `changed`、`RoomReconnect` 的新 `Epoch`、视图里 room 的 `Connected`），旧代际迟到的断线是 `changed=false`、不是错误。
- 每改一条场景，在玩法手册加一行“旧断言 → 新断言”（TD §3.5 的表格式），场景名与条数不变。
- **变异**：每条改过断言的场景至少一个变异，在保留的生成工程里改 handler / 监听、跑对应场景看红、恢复看绿，日志摘要放证据目录。塔防用了五个：去掉连接核对、去掉 `Claimed`、去掉 `Wallet().Add`、名单监听放行所有人、`Member` 换成只查席位不查代际（[mutation.txt](evidence/r9-batch0-20261117/mutation.txt)）。

### 9.4 每个 agent 交付时自查

- 缺省档：`bash scripts/test-<玩法>-generated.sh`，玩法条数不变、0 SKIP，末尾打印 `module verify: <名> n/n skipped=0`。
- 矩阵：`python3 scripts/test-genres-generated.py --out <新目录> --genres <玩法>`，`status passed` 且有 `custom_module_verify`。
- REAL：`source ~/.roost-it/roost-dataengine-it/env.sh` 后 `ROOST_<玩法>_REAL=1 …`，第 1 节列的用例全过、0 SKIP。
- 手册与 skill reference 的命令块真跑一遍（接 SKILL.md §3.2 前置、自定 module 名）。
- `GOWORK=off go test -count=1 -run 'Link|Anchor|Doc' .`。

### 9.5 试点发现的模块缺口（按 Q1 只记录，不在 R9 改模块）

- **room 没有“代际 + 连接 + 在线”一并核对的公开入口**：**已补，1b38c2b8**（维护者 Q1 B）。room 组件与 `view.Room` 新增 `Authorize(playerID, epoch, connection) (*db.RoomSeat, error)`（先 `Member`，再要求连接是席位认下的那条且在线，否则 `room_stale_epoch`），turn 删掉私有 `authorize` 改调它；塔防 `towerMember` 的新命令经它，已有回执的重放与查回执仍是 `Member` + 连接核对（不要求在线）。批 1 的玩法新命令直接用 `m.Room().Authorize`，有“重放不要求在线”的照塔防分两支。[证据](evidence/room-authorize-20261117/README.md)。

- **（1C 卡牌）enter 的 `module verify` 不装工程配置表，EnterFirst 监听里发 bag 道具会让 enter 的 13 条场景答 `bag_item_invalid`**（Q2 B 把次要货币做成 bag 道具后，建档就要发；放置的 Gems 也可能碰到，可能满足 B 条件）：卡牌在 bag 的规则入口 `items.go` 里认下 2001 / 2002 绕过，只记录、不改模块；另：gacha 只扣 wallet 金币（README“不做付费”），卡牌抽卡因此从扣钻石改为扣金币，钻石只剩初始与首胜任务奖励——“gacha 换扣的货币”与 Q2 A（wallet 多币种）是同一件事。[证据](evidence/r9-1c-20261117/README.md)。

- **（1B 放置 / 合成）同上一条的放置版 + enter / stamina / checkin 的三处边界**：放置的初始宝石放在 `EnterFirst` 监听里时 enter 的 13 条场景同样答 `bag_item_invalid`（1C 的 B 候选多了一类玩法）；放置改为在建放置存档的那一笔里 `Bag().Add`（“发过没有”= 存档已存在），不动道具规则。另记三点、都不改模块：`event.EnterFirst` 只带 `NowUnixMilli`，要服务端捕获配置快照或服务端密钥的玩法存档（放置的冻结规则、合成的棋盘与 HMAC 密钥）建不进监听，玩法自己的建档 handler 要求 `Enter().Built()`；stamina 不按存档冻结规则、`Grant` 到硬顶静默截断（合成要求实际发足，否则整笔拒绝），规则文件不能改数（stamina 自带 26 条按缺省数断言）；checkin 的奖励数额写死在它自带的两条场景里，放置改用它的连续天数加成（10×min(n,7)）而不是原来的平 5。[证据](evidence/r9-1b-20261117/README.md)。

- **（1D 撤离 / 自走棋）没有新的模块缺口**（§3.8、§3.9 结论不变；撤离初始 100 金币只发 wallet，不碰 1B / 1C 那条 bag 建档问题）。接入注意一条、不改模块：bag 的 `Add` 读进程当前配置（道具表），玩法夹具的 `OpenCatalog` 另开一个只含玩法规则的 `configdata.Store`，它一装载就把工程的道具表顶掉，撤离兑付药剂随之答 `bag_item_invalid`——夹具改为先捕获规则、再装载 `configs/data`，手册写明正式工程把玩法规则登记进工程同一个 Registry；凡“玩法自带 Catalog + 装 bag”的类型都会碰到。[证据](evidence/r9-1d-20261117/README.md)。

- **（2B MMORPG）没有要改模块的缺口**（装 `wallet bag task`，没装 enter，所以没碰到 1C 那条）；记两点边界：task 的 `Claim` 对已领答 `task_claimed`、不是幂等重放，要“重试拿同样结果”的玩法 handler 自己把它当成功（MMORPG 的 `MMORPGQuest`）；wallet 不设持有上限，玩法上限读 `Balance()` 手写。队伍 / 场景 / 怪物 / 战斗 / 副本留到 CellWorld 那轮，各块缺什么写在证据的“对 CellWorld 进模块那一轮的提示”。[证据](evidence/r9-2b-20261117/README.md)。

- **（1A 回合 PvP / 合作 / 经营）没有要改模块的缺口**（§3.1～3.3 结论不变）；记两点、都不改模块：turn 没有“超时代价 / 对局提前结束”的钩子，回合 PvP 按 turn README 改工程里的 `game/components/turn/turn.go`（`OnDeadline` 计超时后扣行动方生命、`advance` 结束条件加“已分胜负”，`genre-common/run.sh` 插两行、调 `pvp.go`），玩法状态为空时不做事、turn 自带 25 条照过；`roost module verify` 按测试名前缀选场景（turn 是 `^TestTurn`），工程里同前缀的测试会被算进模块条数——纵切的 `TestTurnPVPRealTimerRecovery` 缺省档 SKIP 让矩阵的 turn 校验失败过一次，夹具改带构建标签 `turnreal`。[证据](evidence/r9-1a-20261117/README.md)。

- **（2A 派对房间）没有新的模块缺口**（装 `wallet enter`；§3.10 已记的 room 广播与 matchmaking 弹性人数 / 补位仍不在 R9 补，PartyRoom 按 Q3 B 留手写）；没碰到 Catalog 顶掉道具表、`SetClock` 串扰、EnterFirst 发 bag 三条（派对不装 bag / room、没有初始资产）。记一条接入注意、不改模块：wallet 的余额是嵌套状态 `Wallet`，自身视图的增量可能只带嵌套补丁，客户端照旧用 `SyncPlayerDao.Values()` 会答 `apply nested patch before Values`，要按生成的 `db.ApplySyncPlayerDao` 合到基线上再读（凡“自身视图读模块嵌套状态”的玩法都一样）。[证据](evidence/r9-2a-20261117/README.md)。
