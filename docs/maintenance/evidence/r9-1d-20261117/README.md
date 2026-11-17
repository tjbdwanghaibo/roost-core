# R9 批 1D：生存撤离与自走棋改为“装模块 + 只写模块没覆盖的部分”（2026-11-17）

提交：`3d9b8933`（实现、手册、证据）。依据：[R9 方案](../../PLAN-2026-11-17-r9-genre-modules.md) §3.8、§3.9，写法照 §9（批 0 塔防样板），房间类玩法新命令改调 room 的 `Room().Authorize`（1b38c2b8）。场景名与条数不变：撤离 13、自走棋 13（各含 1 条业务 Robot），0 SKIP。

## 改法

| 玩法 | 模块清单 | 并进模块的 | 仍手写的（样板） |
| --- | --- | --- | --- |
| 生存撤离 | `room wallet bag enter` | SurvivalRaid 并入 room 的 `Room`（容量 4，`RoomDao` 末尾追加 `SurvivalState` / `SurvivalWakeTimer`）；席位代际与连接、在线、断线接管归 room；入场费 `Wallet().Add(-5)`、材料兑金币 `Wallet().Add`；封存清单里的药剂按件 `Bag().Add(1001 药水, n)`；建档 enter，初始 100 金币是 `event.EnterFirst` 监听 | 采集、制作预扣、环境伤害、死亡掉落、撤离读条、物资守恒、Room 上的探险定时器、`Joined` / `Claimed` 事实、名单准入（`RoomJoining` 监听，借鉴 matchroom） |
| 自走棋 | `room wallet enter` | ChessMatch 并入 room 的 `Room`（容量 8，`ChessState` / `ChessWakeTimer`）；断线接管 = `RoomReconnect`；排名奖励 `Wallet().Add`；建档 enter | 共享卡池与商店、经济、合星、编队、阶段推进、确定性战斗与配对、排名、`Claimed`、名单准入 |

身份围栏：新命令（撤离含首次付费入场）经 `Room().Authorize`；已有回执的重放、已入场的重试与查回执经 `Member` + 连接核对（不要求在线），写法同塔防 `towerMember`。领奖 / 兑付不过 room 围栏（玩家身份取自会话，与塔防相同）。

撤离 `Rules.Prices` 从四项改为三项（药剂不再兑金币，改为按件入包）；`MedkitItem = 1001` 对应 bag 自带道具表的“药水”。

## 文件

- 脚本：`scripts/test-survival-extraction-generated.sh`、`scripts/test-auto-chess-generated.sh`（`genre_modules` 数组 + `modules.sh add` / `verify`，删去 `add dao Player`、`add entity`、改锁档与 Player 字段）。
- 夹具：`scripts/testdata/survival-extraction/**`（新增 `room_roster.go.txt`、`first_enter.go.txt`）、`scripts/testdata/auto-chess/**`（新增 `room_roster.go.txt`）。
- 手册：`docs/framework/SURVIVAL-EXTRACTION-INTEGRATION.md`、`docs/framework/AUTO-CHESS-INTEGRATION.md`（§2、§3.1～§3.3、新增 §3.5 旧断言 → 新断言、身份 / 兑付 / 领奖 / 验收各节）。
- skill reference：`docs/agent-skills/roost-genre/references/room-coop-pve.md`（删统一命令块，新增 §3.2 撤离、§3.3 自走棋装模块命令块；§2、§4～§7）。
- 方案：§9.5 追加 1D 一行。

## 六条验收

| # | 命令 | 结果 |
| --- | --- | --- |
| 1 | `bash scripts/test-survival-extraction-generated.sh`、`bash scripts/test-auto-chess-generated.sh` | 各 13 PASS、0 SKIP；build / vet / `id check` / `generate --check` 过；`module verify`：撤离 bag 20/20、enter 29/29、room 34/34、wallet 17/17，自走棋 enter 29/29、room 34/34、wallet 17/17，全 0 skip（[default.txt](default.txt)） |
| 2 | `python3 scripts/test-genres-generated.py --out <scratchpad>/matrix --genres survival-extraction auto-chess` | 两类 `passed`，自定 module 名下 13 / 13，`custom_module_verify` 与上同（[matrix.txt](matrix.txt)） |
| 3 | `source ~/.roost-it/roost-dataengine-it/env.sh` 后 `ROOST_SURVIVAL_EXTRACTION_REAL=1 …`、`ROOST_AUTO_CHESS_REAL=1 …` | `TestSurvivalExtractionRealCrashRecovery`（24 条未投影）、`TestAutoChessRealCrashRecovery`（50 条未投影）PASS，0 SKIP（[real.txt](real.txt)） |
| 4 | 变异 | 撤离 8 个、自走棋 7 个，见下（[mutation.txt](mutation.txt)） |
| 5 | `git status`；`GOWORK=off go test -count=1 -run 'Link\|Anchor\|Doc' .` | 本组文件全部提交（3d9b8933），主检出里本组路径无残留；提交后复跑文档测试：`TestDocumentedCommitsAreReachableFromHEAD`、`TestCoreContractsDoNotLinkDrivers`、`TestObservabilityDocNamesExistingMetrics` 过，`TestTrackedMarkdownRelativeLinksResolve` 只剩 2 处、都是 2B（MMORPG）尚未提交的 `r9-2b-20261117` 证据链接，与本组无关；本组的 6 处链接在提交后都能解析 |
| 6 | 手册与 reference | 两份手册 §3.5 写旧断言 → 新断言；reference §3.2 / §3.3 命令块接 SKILL.md §3.2、自定 module 名实跑：13 / 13 PASS、module verify 全过（[skill-block.txt](skill-block.txt)）；两份手册 §3.3 的 Go 示例放进生成工程 `go vet` 通过 |

## 变异（红 → 恢复绿）

撤离（`se1` 工程）：S1 重放分支不核对连接 → `Reconnect…` 红（旧连接用新代际查回执被放行）；S2 入场不过 room 围栏 → `Admission…` 红（旧连接替席位付费）；S3 兑付不调 `Wallet().Add` → `Channel…` 红；S4 兑付不调 `Bag().Add` → `Channel…` 红（药水 0 件）；S5 去掉 `Claimed` 检查 → `Channel…` 红（重复到账）；S6 入场不扣费 → `Admission…` 红；S7 名单监听放行所有人 → `Reconnect…` 红（局外人入座）；S8 `EnterFirst` 不发初始金币 → `Admission…`、`Channel…` 红（`wallet_short`）。S1 / S2 / S7 第一次写的变异删掉了被用的变量、是编译失败，不算红，换成能编译的写法（条件后加 `&& false` / 改比较值）重跑后是真红。

自走棋（`ac1` 工程）：C1 重放分支不核对连接 → `Reconnect…` 红；C2 领奖不调 `Wallet().Add` → `Terminal…`、`Robot…` 红；C3 去掉 `Claimed` 检查 → `Terminal…` 红（金币 320）；C4 名单监听放行所有人 → `Reconnect…` 红；C5 `Member` 改成用席位当前代际（不看请求代际）→ `Reconnect…`、`Robot…` 红；C6 新命令也走重放分支（不经 `Authorize`）→ **仍绿**：`Apply` 还收 room 报告的在线状态，断线席位的新命令被它再拒一次（两道围栏，设计如此）；C7 两道一起去掉 → `Deadline…DisconnectCannotStall` 红。

REAL 档改过的断言（撤离最终金币 95 / 99 / 98 / 98 + 药水 1 / 0 / 0 / 0、重启后断线新命令被拒、全员 `RoomReconnect` 后续局、第三进程旧代际命令改查回执；自走棋 50 条未投影、冷加载后席位在而全员断线）没有单独做 REAL 变异：对应的模块调用（wallet / bag / 围栏）已由缺省档变异覆盖，REAL 每轮数分钟，按批次规则只做一次全绿。

## 场景数

不变：撤离 13（4 规则 + 8 Nest + 1 Robot）、自走棋 13（4 规则 + 8 Nest + 1 Robot）。改过断言的场景逐条见两份手册 §3.5。

## 模块缺口

没有新的（§3.8 / §3.9 结论不变）。记一条接入注意（已写进方案 §9.5、撤离手册 §4 / §9、reference §6）：bag 的 `Add` 读进程当前配置；夹具的 `survival.OpenCatalog` 另开一个只含玩法规则的 `configdata.Store`，装载就会顶掉工程的道具表，兑付答 `bag_item_invalid`。夹具改为“先捕获规则、再装载 `configs/data`”（REAL 的写进程不兑付，不受影响）；正式工程应把玩法规则登记进工程同一个 Registry。

## 拿不准的点

1. **撤离的药剂改成进 bag（语义变化）**：方案 §3.8 写“永久兑付的物资 → bag”，原夹具只兑金币。我取的是“材料兑金币（wallet）、药剂按件入包（bag）”，因此 `Prices` 去掉药剂一项，REAL 最终金币玩家 0 从 100 变 95（多 1 件药水）。若维护者的意思是“全部物资都入包”或“金币之外再入包”，要再改一次（改动只在 `state.go.txt` 的 `Reward` / `Medkits` 与 handler 两行）。
2. **入场也过 room 围栏**：撤离的 `SurvivalEnter` 多了 `epoch, connection` 参数，没入场时经 `Authorize`、已入场的重试走 `Member` + 连接（重启后全员断线时重试仍成功）。原夹具入场不核对身份；这是按“新命令一律调 Authorize”推出来的，没有更早的定论。
3. **REAL 第二进程让四人都 `RoomReconnect`**：原用例冷加载后直接下新命令；room 在线不落库，现在必须先重连。于是第三进程里换连接之前的旧代际命令按代际围栏被拒、改按当前代际查回执——这和原有“旧代际命令被拒”的契约一致，但“跨进程原样重放全部命令”这句在撤离里变成“同代际的原样重放 + 旧代际查回执”。
4. **`SetClock` 串扰**：两类夹具都调了包级的 `room.TimerDriver.SetClock`，没有撤销入口；脚本与 `module verify` 分开跑，本组没碰到。只在手册 §8 / §9 记了，没改 `modules/room`。
5. 同一检出里别的 agent（1A / 1B / 1C / 2B）的未提交改动不影响本组生成工程（两类脚本全绿）；本组只提交上面列的文件。

## 给协调者：CHANGELOG / KNOWN-LIMITS 建议文字

- CHANGELOG `[Unreleased]`：`R9 批 1D：生存撤离、自走棋改为装模块——房间并入 room 的 Room（席位代际 / 连接 / 在线 / 断线接管，新命令经 Authorize），金币走 wallet、建档走 enter，撤离带出的药剂按件进 bag（材料仍兑金币）；场景 13 / 13 不变，脚本加跑 module verify，REAL 全绿（撤离 24、自走棋 50 条未投影记录）。`
- KNOWN-LIMITS“游戏接入验收”撤离 / 自走棋两行：房间实体改为 room 的 `Room`；“鉴权与会话票 / 接管票是合成身份”改为“鉴权合成、连接标识是固定串、断线由用例直接调 `RoomDisconnect`（未经真实 TCP / Gate 的断线订阅）”；撤离未验收项“局外逐件仓库”改为“只有药剂按件入包，其余材料仍兑金币”。
