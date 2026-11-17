# R9 批 1（agent 1C）：卡牌 RPG 装模块（2026-11-17）

方案：[PLAN-2026-11-17-r9-genre-modules](../../PLAN-2026-11-17-r9-genre-modules.md) 3.6（映射）、第 5 节表 1C（文件范围）、第 9 节（批 0 定型的写法）；维护者 11-17 答复 Q1～Q8 全按推荐。同一检出里另有 R3 与其它 R9 agent 在改别的文件，本批生成工程按当时的工作区编译，全部通过。

## 改法

模块清单 `enter wallet bag stamina gacha task`，stamina 连带装上 rules（Q6 只装 enter，Q8 不建配方）：

| 原夹具的块 | 现在 | 说明 |
| --- | --- | --- |
| 建档 `handlerCardInit(key)` | **enter** + `event.EnterFirst` 监听 | 新文件 `enter_first.go.txt`，装到 `game/entities/player/card_rpg.go`：写初始卡组，发 100 金币（wallet）以及 100 钻石、20 尘（bag）。战斗密钥用 `crypto/rand` 生成（`DrawKey` 改名 `BattleKey`，只给战斗随机用） |
| Gold | **wallet** | 养成扣费、领奖、扫荡走 `Wallet().Add`；“领过没有”仍是 Run 与开局账本里的 `Claimed` |
| Gems、Dust | **bag** 道具 2001 / 2002（Q2 B） | 两个道具号写进 bag 的规则入口 `items.go`（见“模块缺口”） |
| Energy 与开局扣体力 | **stamina** | 开局、扫荡在同一事务里调 `Stamina().Spend`；视图取 `Current(now)`；规则用模块缺省（上限 100、每 5 分钟 1 点） |
| 抽卡、保底、十连、请求回执 | **gacha**（Q1 A，语义跟 gacha） | `handlerCardDraw(player, key, count)` 调 `Gacha().Draw(key, 卡池 2, count)`，在同一笔事务里把英雄道具从背包取出、换成卡牌实例，重复卡转成碎片并发 1 尘；`Replayed` 时不再转换。卡池 2：单抽 2 金币，`pity_after` 5，保底道具 2106，权重 27/27/26/7/7/6。扣费货币按 Q2 推荐：gacha 只扣 wallet，所以从扣钻石改为扣金币 |
| 任务“首胜 10 钻石”（原 `Manage{quest}`） | **task** | 领奖时 `Task().Report("card.won")`；任务表追加 101 号永久任务，奖励道具 2001 × 10，客户端经 `TaskClaim` 领取 |
| 卡牌实例 / 碎片、养成、编队、HeroStats、CardRun 回合 PvE 与定时器、首通 / 扫荡 | 手写 | `Profile.Manage` 只判卡牌条件、改卡牌状态，并算出 `Effects{Gold, Dust, Energy}`，由 handler 调模块执行；回合与定时器借鉴 turn，首通借鉴 dungeon |

配置：`cardrpg.OpenCatalog` 先 `generated.RegisterGeneratedConfigData`，再登记 `card_rules`，两者在同一个 Store 里装载。configdata 的 Load 会发布成进程的缺省配置，卡牌规则若另开一个 Store 装载，会把 gacha / bag / task 读的表换掉。`Rules` 里删掉了 `DrawCost` / `Pity`，这两项改在 gacha 表里配。

## 文件

| 文件 | 改法 |
| --- | --- |
| `scripts/test-card-rpg-generated.sh` | 装模块；删 `add dao Player`；DAO 字段改为追加在 struct 末尾；往四张模块表追加卡牌行；给 bag `items.go` 打补丁；拷 `enter_first.go.txt`；末尾跑 `modules.sh verify` |
| `scripts/testdata/card-rpg/catalog.go.txt` | 资产 / 道具 / 卡池常量与 `HeroTemplate`；`Rules` 删 `DrawCost` / `Pity`；`OpenCatalog` 同时登记生成的表 |
| `scripts/testdata/card-rpg/profile.go.txt` | 存档去掉资产、`Pity`、`Draws`、`QuestClaimed`、`EnergyAt`；删 `Draw` / `Regenerate`；新增 `Collect`、`Effects`、`NewBattleKey` |
| `scripts/testdata/card-rpg/handlers.go.txt` | Player 参数改用 `view.PlayerEntity`；draw / manage / start / claim / profile 改调模块组件；新增 `cardView` 与 `applyEffects` |
| `scripts/testdata/card-rpg/enter_first.go.txt`（新） | EnterFirst 监听 |
| `scripts/testdata/card-rpg/gameplay_test.go.txt`、`catalog_test.go.txt` | 夹具改走 enter 建档；catalog 由工程表加 `cards.json` 组成；新增 `setCardDrawPrice` 热更卡池；新增 `cardAssets`（整份比对卡牌存档与模块状态）；改过的断言见手册 §3.5 |
| `scripts/testdata/card-rpg/robot/*.txt` | dispatch 的抽卡请求键取 `draw-<Seq>`；断言改读 wallet / gacha |
| `scripts/testdata/card-rpg/persistence/recovery_test.go` | 每个进程各装一份 catalog；建档改 `EnterInit`；屏障记模块状态与抽卡回执；重启后热更卡池涨价，再重放 |
| `docs/framework/CARD-RPG-INTEGRATION.md` | §2、§3.1～§3.4、新增 §3.5“旧断言 → 新断言”、§4～§7、§11～§13 |
| `docs/agent-skills/roost-genre/references/card-turn-based.md` | §2 模块表；§3 改为装模块的命令块；§4～§7 |
| `docs/maintenance/PLAN-2026-11-17-r9-genre-modules.md` | 只在 §9.5 追加一行（1C 的模块缺口） |

场景数：`TestCardRPG` 仍是 17 条（`game/modules/cardrpg` 4 条 + `game/handler` 12 条 + 1 条 Robot），场景名不变；REAL `TestCardRPGRealCrashRecovery` 1 条不变，未投影 WAL 仍 7 条。新增的模块场景（`module verify`）：bag 20、enter 29、gacha 29、rules 10、stamina 26、stamina+rules 2、task 29、task+bag 3、task+stamina 3、task+wallet 6、wallet 17。

## 验收（方案第 5 节六条 + §9.4）

| # | 命令 | 结果 | 摘要 |
| --- | --- | --- | --- |
| 1 | `bash scripts/test-card-rpg-generated.sh` | exit 0。17 PASS、0 SKIP；build、vet、`id check`、`generate --check` 通过；`module verify` 11 项都是 n/n、skipped=0 | [default.txt](default.txt) |
| 2 | `python3 scripts/test-genres-generated.py --out <scratchpad>/matrix1 --genres card-rpg` | `status passed`；自定义 module 名下 17 条再过；`custom_module_verify` 11 项齐全 | [matrix.txt](matrix.txt) |
| 3 | `source ~/.roost-it/roost-dataengine-it/env.sh; ROOST_CARD_RPG_REAL=1 bash scripts/test-card-rpg-generated.sh` | exit 0。缺省 17 条加 `TestCardRPGRealCrashRecovery` 通过：7 条未投影记录，SIGKILL 后恢复，补触发一次，第三个进程重试不重复；0 SKIP，库已删 | [real.txt](real.txt) |
| 4 | 变异：保留的生成工程里改 handler / 监听，跑对应场景，再恢复 | 9 个变异全部变红，恢复后变绿 | [mutation.txt](mutation.txt)，驱动 [mutate.py.txt](mutate.py.txt) |
| 5 | `git status`（本批文件）；`GOWORK=off go test -count=1 -run 'Link\|Anchor\|Doc' .` | 本批文件提交后无残留；文档测试见报告 | — |
| 6 | 手册与 skill reference 同步；skill 命令块真跑（SKILL.md §3.2 前置，自定 module `example.com/mycard`）；手册 §3.3 示例在生成工程里 build + vet | 17 PASS，`module verify` 11 项 n/n，§3.4 收尾全过，doctor 无 FAIL；示例编译通过 | [skill-block.txt](skill-block.txt) |

变异清单（第 4 条）：

| 变异 | 改法 | 变红的场景 |
| --- | --- | --- |
| M1 | 删掉 `Replayed` 早退，重放时也转换 | `DrawConcurrentReplayPityAndConfigIdentity`（`bag_short`，重放失败） |
| M2 | 删掉 `Bag().Remove(英雄道具)` | 同上（hero item left in bag） |
| M3 | `Gacha().Draw` 换成 `DrawFree`，不扣金币 | `DrawRefusalAndUndoKeepFundsAndRandomStream` |
| M4 | 删掉开局的 `Stamina().Spend` | `ConcurrentAdmissionHasOneRunAndOneEnergyCharge` |
| M5 | 删掉领奖的 `Task().Report` | `WinClaimSweepQuestAndConcurrentRewardReplay`（`task_incomplete`） |
| M6 | 领奖金币置 0，不经 wallet | `RetreatHasNoRewardAndFirstClearOnlyPaysOnce` |
| M7 | `applyEffects` 只做加法，不扣金币 | `GrowthRejectsMissingCardsOldRequestsAndInvalidTeams` |
| M8 | 视图体力读存值，不经 `Current(now)` | `LedgerBoundsAndOfflineEnergyKeepReplayEvidence` |
| M9 | 建档不发初始金币 | `RobotBusiness` |

## 模块缺口（按 Q1 只记录，没有改 modules/**）

- **enter 的 `module verify` 不装工程配置表**：EnterFirst 监听里发 bag 道具时，enter 的 13 条场景会答 `bag_item_invalid`（第一次跑时 enter 只有 16/29）。卡牌在 bag 的规则入口 `game/components/bag/items.go` 里认下 2001 / 2002 绕过：这是工程文件、正式扩展点，英雄道具仍走表。Q2 B 把次要货币做成 bag 道具后，“建档就发”很常见，放置的 Gems 也可能碰到，可能满足 B 条件，请维护者定。
- **gacha 只扣 wallet 金币**：卡牌抽卡因此从扣钻石改为扣金币，钻石只剩初始发放与首胜奖励。“gacha 换扣的货币”与 Q2 A（wallet 多币种）是同一件事，本轮不动。
- 方案 3.6 已列的“gacha 确定性随机流、卡牌实例”不补，与 gacha README 一致。

## 拿不准的点

1. **钻石失去用途**：按 Q2 B 加 gacha 只扣金币，钻石只是初始与任务奖励的“摆设”。如果维护者希望抽卡仍扣钻石，要么改 gacha 的 `Draw`（会让 gacha 自己的 29 条场景失败），要么等 wallet 多币种。
2. **bag `items.go` 打补丁**：用的是 bag README 写明的工程扩展点，但它是为 enter 的模块场景让路，不是业务需要；如果以后 enter 场景改为装工程的表，这个补丁可以删掉。
3. **体力数值跟随 stamina 缺省**（上限 10 → 100，3 秒 1 点 → 5 分钟 1 点）：夹具没改 `stamina/rules.go`，因为改了 stamina 自己的场景会断言失败。所以 `Energy` 断言从 8 / 4 变成 98 / 94，时钟回拨也从“报错”变成“不恢复不倒扣”。
4. **抽卡回执窗口从“满 64 拒绝”变成“淘汰最旧”**：`LedgerBoundsAndOfflineEnergyKeepReplayEvidence` 的场景名里还有 “KeepReplayEvidence”，但窗口外旧键现在会当新请求再扣一次；这是 gacha 的语义，手册 §3.5 写明。场景名按规则不改。
5. **任务奖励**：方案写“奖励经 wallet”，夹具用的是 task 的 `item` 奖励（bag 联动登记）发 10 钻石，以保留原经济数值。

## 交给协调者写的内容（本批没改 CHANGELOG / KNOWN-LIMITS）

CHANGELOG `[Unreleased]` 建议加：

- 变更：卡牌 RPG 玩法夹具改为“装模块 + 手写玩法核心”（R9 批 1C）。装 `enter wallet bag stamina gacha task`：建档走 enter 的 EnterFirst 监听，金币走 wallet，钻石与尘是 bag 道具，体力走 stamina；抽卡由卡牌 handler 调 `Gacha().Draw`，在同一笔事务里把道具换成卡牌实例、重复卡转碎片，扣金币，失败重抽，成功结果由回执固定；首胜改为 task 任务。场景名与条数（17）不变，旧断言 → 新断言见 CARD-RPG-INTEGRATION §3.5；REAL 未投影 WAL 仍 7 条。

KNOWN-LIMITS：

- “模块化顺序（N1）”一条：卡牌已按 R9 改造，模块清单 `enter wallet bag stamina gacha task`。
- 新的“尚未确认”候选（维护者定）：enter 的模块场景不装工程配置表，EnterFirst 里发 bag 道具会让它变红（见“模块缺口”）。
- “游戏接入验收”卡牌一行：缺省档另跑 `module verify` 11 项；REAL 2026-11-17 复跑通过。
