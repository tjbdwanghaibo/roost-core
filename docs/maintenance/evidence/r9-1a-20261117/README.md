# R9 批 1 · 1A：回合 PvP、合作 Roguelike、异步社交经营 + genre-common（2026-11-17）

方案：[PLAN-2026-11-17-r9-genre-modules](../../PLAN-2026-11-17-r9-genre-modules.md) §3.1～3.3、§4、§9。手册：[SESSION-GENRES](../../../framework/SESSION-GENRES.md)（“模块接走了什么，手写了什么”“模块化之后场景语义的变化”两节）；skill reference：[session-genres.md](../../../agent-skills/roost-genre/references/session-genres.md)。

提交：`aa6a3a28`（本证据补记另一提交）。

## 改法

| 玩法 | `genre_modules`（脚本里的数组） | 并入 / 去掉 | 手写（夹具） |
| --- | --- | --- | --- |
| 回合 PvP | `turn wallet`（连带 room） | GameRoom → room 的 `Room`（Q3 A）；GameRoom 的 `add timer` 删掉，期限是 turn 的 `TurnDeadline`；`Player.Coins` → wallet | `RoomDao.PvpState`（生命、胜负、带内容的攻击回执）；`RoomJoining` 名单监听；`game/components/turn/pvp.go` + `run.sh` 在 `turn.go` 插两行（turn README“超时要做别的事改 OnDeadline”：超时扣行动方生命、分出胜负即结束）；攻击按 turn 的 `CheckEnd → 改玩法 → End` 三段式 |
| 合作 Roguelike | `room wallet` | GameRoom → `Room`；`Coins` → wallet | `RoomDao.CoopState`（投票、种子、三关、`Claimed`、回执；借鉴 turn 的“每人连续命令号 + 最后一条可重放”）；名单监听；身份围栏照塔防 `towerMember` 分两支（新命令 `Authorize`，最后回执重放 `Member` + 连接、不要求在线） |
| 社交经营 | `friends wallet` | 不再有房间：农场是主人 `PlayerDao.SocialFarm`；访客资格 = 主人好友名单 | 种植 / 帮助 / 收获；主人操作只锁主人，帮助锁 `[主人, 访客]`（借鉴 friends） |

genre-common（§4 的去向，按方案落地）：

- `run.sh`：留。读 `GENRE_MODULES`（各玩法脚本的 `genre_modules` 数组）→ `modules.sh add` → 往模块主 DAO 末尾追加玩法字段 → 拷夹具 → `generate` → 场景（9 / 10 / 10 不变）→ build / vet / id check / generate --check → `modules.sh verify`（`module-verify.json`）→ REAL。删掉 `add entity GameRoom` / `add dao` / `add timer` / 插 `Coins`。拷贝时断言不覆盖模块或生成文件（只许覆盖模板自带的 `cmd/robot/main.go`、`loadtest/README.md`）。
- **§9.1 末“genre-common 三类的模块清单怎么给”的决定**：清单写在各玩法脚本的 `genre_modules` 数组里（与塔防脚本同一处、同一写法），经环境变量 `GENRE_MODULES` 交给 `run.sh`；`run.sh` 缺这个变量直接失败。没有另放清单文件，免得清单与脚本两处漂移。
- `handlers.go.txt`（GameRoom 版）：删，拆到各玩法自己的 `handlers.go.txt`。
- `protocol.go.txt` / `network.go.txt`：留公共 PB 外壳与编解码；请求加 `Epoch`、应答加 reconnect 的新 `Epoch`；`RoomID` 改为唯一号，接入层换成 Entity ID（房间 `room.EntityID`）。转成哪条 Sender 由玩法的 `command.go` 决定（房间玩法共用 `genre-common/room_command.go.txt`，经营自带）。
- `gameplay_test.go.txt`：留 4 条公共场景，改为调用各玩法的小接口（`newScenarioTarget` / `registerScenario` / `open` / `state` / `sendAs` / `targetFacts` / `closeScenario` / `scenarioSmoke`，写在玩法的 `fixture_test.go.txt`）；房间玩法共用部分在新增的 `genre-common/room_test.go.txt`（入座、接管、重连、冷加载后读回连接）。
- `network_test.go.txt`、`robot.go.txt`、`robot_README.md`：留（TCP Robot 外壳）；`loadtest.Smoke` 与从未生效的 `robot_main.go.txt`（三类都被覆盖）删掉，首动作验证并入 `Commands` 的步骤；新增 `KindReconnect` 与“Epoch 为 0 沿用最近一次接管的代际”。
- `persistence/runtime_test.go.txt`：留真实 Runtime 接线，实体集合改由玩法钩子给（合作：Room + 两名 Player；经营：两名 Player），冷加载后先 `adopt`（读回持久席位的连接与代际）、重放、再 `rejoin`。
- 夹具状态包 `game/modules/rules` 改名 `game/modules/scenariorules`（§2 说的与 rules 模块同名易混）。

## 文件

`scripts/test-{turn-pvp,coop-roguelike,social-sim}-generated.sh`；`scripts/testdata/genre-common/**`（除 `modules.sh`）；`scripts/testdata/{turn-pvp,turn-pvp-real,coop-roguelike,social-sim}/**`；`docs/framework/SESSION-GENRES.md`；`docs/agent-skills/roost-genre/references/session-genres.md`；方案 §9.5 追加 1A 一行；本证据目录。`turn-pvp-real/recipe_test.go.txt` 与 `scripts/test-turn-pvp-real.sh` 未改。

## 六条验收

1. **脚本缺省档**（`bash scripts/test-<玩法>-generated.sh`）：回合 PvP 10/10、合作 10/10、经营 9/9，0 SKIP；build / vet / id check / generate --check 通过；`module verify`：回合 PvP room 34/34、turn 25/25、wallet 17/17；合作 room 34/34、wallet 17/17；经营 friends 18/18、wallet 17/17，全部 skipped=0。见 `default-*.txt`、`final-doc-runs.txt`。
2. **矩阵**（`python3 scripts/test-genres-generated.py --out <新目录> --genres turn-pvp coop-roguelike social-sim`）：最终一轮（最后一次夹具改动之后）三类 `status passed`，`custom_module_verify` 与上面同数，passed 10 / 10 / 9、skipped 0（`matrix.json` 的 `final_run`；`earlier_runs` 是之前两次）。第一次跑回合 PvP 失败过：turn 的 `module verify` 按 `^TestTurn` 选场景，把脚本拷进工程的 `TestTurnPVPRealTimerRecovery`（缺省 SKIP）算进 turn，`skipped=1`；改为给该夹具加构建标签 `turnreal`（缺省档带标签编译 / vet，REAL 带标签运行）后通过。见 `matrix.json`。
3. **REAL**（`source ~/.roost-it/roost-dataengine-it/env.sh` 后）：
   - `ROOST_TURN_PVP_REAL=1`：`TestTurnPVPRealTimerRecovery` seed / recover 各 PASS（race、0 SKIP），随后 `test-turn-pvp-real.sh` 配方演练 `TestTurnRecipeRealLifecycle` seed / recover PASS。
   - `ROOST_COOP_ROGUELIKE_REAL=1`：`TestCoopRoguelikeRealCrashRecovery`（partial_vote 9 条 strict 记录、first_reward 13 条；R9 前 7 / 11）PASS。
   - `ROOST_SOCIAL_SIM_REAL=1`：`TestSocialSimRealCrashRecovery`（growing 6 条、harvested 7 条）PASS。
   见 `real-*.txt`。
4. **变异**：见下节与 `mutation.txt`，每条改过断言的场景至少一个，全部先红后绿。
5. 提交 `aa6a3a28` 后主检出 `git status` 只剩别组的改动（本组文件无残留）；`GOWORK=off go test -count=1 -run 'Link|Anchor|Doc' .` 在该提交的干净克隆里通过；主检出里同一命令被别组（2A）未提交的两处断链拖红，与本组无关（`doc-tests.txt`）。
6. 手册与 skill reference 已同步（模块清单、手写部分与样板、旧断言 → 新断言、条数）；SESSION-GENRES 的三条命令块（自定 module 名 myduel / mycoop / myfarm）与两条 REAL 命令块真跑通过（`final-doc-runs.txt`）。

## 场景数

不变：回合 PvP 10、合作 10、经营 9（`TestScenario*`）。REAL 用例数不变（回合 1 条两阶段 + 配方 1 条两阶段；合作 / 经营各 1 条两个子场景）。

## 改过断言的场景与变异（旧断言 → 新断言见 SESSION-GENRES 表）

在最后一次夹具改动之后保留的三份生成工程里改 handler / 组件 / 接入（`mutations.sh.txt` 列出全部 21 条，逐条调 `mutate.py.txt`：改一处 → 跑对应场景 → 恢复 → 再跑），日志 `mutation.txt`（20 条红→绿；C6 缺省档仍绿，如下说明）：

| 变异 | 改了什么 | 变红的场景 |
| --- | --- | --- |
| P1 | 回合 PvP 去掉命中奖励的 `Wallet().Add` | TurnOrder |
| P2 | 回合 PvP 重放不比对攻击内容 | RetryAndConflict |
| P3 | 回合 PvP 用当前回合号代替客户端期望的回合号 | StaleRevision |
| P4 | 回合 PvP 把请求代际换成席位当前代际再交给 turn | ReconnectReadsAuthoritativeTurn |
| P5 | 回合 PvP 非成员分支漏返回错误 | Membership |
| P6 | `turn.go` 去掉 `pvpTimeout`（超时不扣生命） | TimeoutConsumesOneTurn、TurnTimesOutByTheRoomTimer |
| P7 | `turn.go` 去掉 `pvpOver`（分出胜负 turn 不结束） | TerminalCannotAttack、TurnTimesOutByTheRoomTimer |
| P8 | 公共 dispatch 应答漏填 reconnect 的新代际 | RobotTCPRetryAndReconnect |
| P9 | 回合 PvP 进程内结果缓存（不随事务回滚） | CommitRejectionRollsBack |
| P10 | 公共接入把客户端 `RoomID` 直接当 Entity ID（不经 `room.EntityID`） | RobotTCPRetryAndReconnect |
| C1 | 合作去掉领奖的 `Wallet().Add` | RewardExactlyOnce、RobotCompleteCoopAndClientViews |
| C2 | 合作进程内结果缓存 | RewardRollsBackBothEntities、CommitRejectionRollsBack |
| C3 | （只在生成工程里改模块，验证场景依赖它）room 的 `Reconnect` 不更新席位连接 | SeedSurvivesReload |
| C4 | 合作非成员分支漏返回错误 | Membership |
| C5 | 公共 dispatch 应答漏填新代际 | RobotCompleteCoopAndClientViews、RobotTCPRetryAndReconnect |
| C6 | 合作最后回执重放也要求在线（`Authorize`） | `TestCoopRoguelikeRealCrashRecovery`（REAL：冷加载后原连接重放被拒）；缺省档没有离线重放的场景，同一变异缺省档仍绿——这一支只由 REAL 档守 |
| S1 | 经营不认好友（`friend := false`） | VisitorPermissions、HelpChangesDeadlineOnlyOnce、RobotTCPRetryAndReconnect |
| S2 | 经营去掉收获的 `Wallet().Add` | OfflineHarvestOnce、RobotTCPRetryAndReconnect |
| S3 | 经营进程内结果缓存 | HarvestRollback、CommitRejectionRollsBack |
| S4 | 经营非好友分支漏返回错误 | Membership |

试过、没有变红、换掉的：`durability=strict` → `async`（同步 Sender 仍等提交结果、提交被拒照样报错，所以 CommitRejection 不红）；为此把公共 `CommitRejectionRollsBack` 加强为“放开后同一命令真正生效一次”，改用 P9 / C2 / S3 的进程内缓存变异。

## 模块缺口（§9.5 已追加一行）

没有要改模块的缺口（§3.1～3.3 结论不变）。两点边界：

1. turn 没有“超时代价 / 对局提前结束”的钩子：回合 PvP 照 turn README 改工程里的 `game/components/turn/turn.go`（`run.sh` 插两行，调夹具的 `pvp.go`；`PvpState` 为空时不做事，turn 自带 25 条照过）。只有回合 PvP 一类要，不满足 B。
2. `roost module verify` 按测试名前缀选场景，工程里与模块同前缀的测试会被算进模块条数（上面验收 2 的失败）。夹具用构建标签绕开；是否让 verify 只数模块自带文件里的用例，留给维护者。

## 拿不准的点

- **回合 PvP 加了命中奖励**：旧纵切的奖励恒为 0（`Player.Coins` 从不变化），但方案 §3.1 要装 wallet。这里改为“每次命中 1 金币，经 `Wallet().Add`，与回合事实同一笔事务”，让公共回滚场景真的覆盖 wallet + Room。属于玩法规则改动，手册已写明；若维护者不要奖励，删 `HitReward` 与 wallet 即可，场景数不变。
- **回合 PvP 扩展了模块装进来的 `turn.go`**：按 README 是正当的扩展点，但 `run.sh` 用文本锚点插行，turn 模板改动时这里会断言失败（失败是显式的）。
- **社交经营的农场用 JSON 字符串字段**（`SocialFarm string`），没有按 compose 0A 做成嵌套结构体：三类共用“严格 JSON 状态机 + 公共夹具”，与塔防的 `TowerState` 同一写法。
- **社交经营的帮助锁了访客但不写访客**：访客奖励为 0，锁 `[主人, 访客]` 是照方案“两个 Player 的事务”与 friends 的写法；访客回执记在主人农场里（以好友上限 + 1 为界）。
- **测试串扰**：夹具不调用 `TimerDriver.SetClock`，只拨 `clock.SetSource`（驱动缺省读 fctx 业务时钟），所以 `go test ./game/handler` 整包（turn / room / wallet 自带场景 + 10 条玩法场景，72 条）一起跑通过，不受 room-authorize 证据里那条串扰影响。
- 场景 `RoomJoin` 等席位写入让合作 REAL 的 strict 记录从 7 / 11 变为 9 / 13；经营仍是 6 / 7（GameRoom 开房换成了加好友两步）。

## 交给协调者写进 CHANGELOG / KNOWN-LIMITS / 其他文件的内容

- CHANGELOG `[Unreleased]`：R9 批 1A——回合 PvP（`turn wallet`，GameRoom 并入 room 的 Room，期限改用 turn 的 `TurnDeadline`，纵切与 `-recipe turn-pvp` 同一模型）、合作 Roguelike（`room wallet`）、社交经营（`friends wallet`，不再有房间）改为装模块 + 手写剩余部分；三个脚本加跑 `module verify`；公共 PB 加 `Epoch`，`RoomID` 改为唯一号；删 `loadtest.Smoke`；合作 `cmd/robot` 新增必填 `-epoch` / `-peer-epoch`；回合 PvP 每次命中 1 金币；场景数不变（10 / 10 / 9）。
- KNOWN-LIMITS（N1 改写时）：三类已模块化；`module verify` 的前缀选场景会把工程里同前缀测试算进去（见上“模块缺口”2）。
- 不在本组范围、文字已过时、需协调者改：
  - `integration_coverage_test.go` 第 604 行 `TestTurnPVPRealTimerRecovery` 的说明仍写“GameRoom 纵切”，应改为“Room 上 turn 的 TurnDeadline（R9 起与配方同一模型）”。
  - `docs/framework/CAPABILITIES.md` 第 271～273 行：回合超时“由房间的持久定时器（`roost add timer`）”→ turn 的 `TurnDeadline`；三类说明可补模块清单。
  - `docs/framework/ROBOT.md` 第 96～98 行合作 `cmd/robot` 命令缺 `-epoch` / `-peer-epoch`（两人须已 RoomJoin）；第 106 行“GameRoom 纵切”措辞。
