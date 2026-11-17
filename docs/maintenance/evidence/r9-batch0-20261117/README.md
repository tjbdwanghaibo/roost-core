# R9 批 0（agent 0A）：公共装模块脚手架 + 塔防试点（2026-11-17）

方案：[PLAN-2026-11-17-r9-genre-modules](../../PLAN-2026-11-17-r9-genre-modules.md) 第 5 节“批 0”、3.7；定型写法见同一文档[第 9 节](../../PLAN-2026-11-17-r9-genre-modules.md#9-批-0-定型的写法批-1--批-2-照做)。基线 main `ec625187`（同一检出里另有 room / Gate / demo / DAO 生成器的 agent 在改别的文件，本批的生成工程按当时的工作区编译、全部通过）。

## 改了什么

| 文件 | 改法 |
| --- | --- |
| `scripts/testdata/genre-common/modules.sh`（新） | `add <roost> <模块…>`：`roost module add` + 用 `module list --json` 核对都已登记；`verify <roost> <repo> <json> [<模块…>]`：`roost module verify --json`，逐个核对通过数 == 本检出 `modules/<a>/module.yaml` 的 `verify.pass`（联动 `<a>+<b>` 取 `with.<b>.verify.pass`）、0 skip、四项检查通过、清单里的模块都在结果里。不依赖 PyYAML |
| `scripts/pretag.sh` | `env -u` 补 `ROOST_TURN_PVP_REAL`、`ROOST_COOP_ROGUELIKE_REAL`、`ROOST_SOCIAL_SIM_REAL` |
| `scripts/test-genres-generated.py` | 输出目录里有 `module-verify.json`（即脚本跑过 `modules.sh verify`）时，在自定义 module 名下用 `modules.sh verify` 再核一遍，报告字段 `custom_module_verify`；没迁的玩法行为不变 |
| `scripts/test-tower-defense-generated.sh` | 装 `room wallet enter`；删 `add dao Player` / `add entity TowerRoom` / `add dao TowerRoom` / 改锁档 / Player 加字段；`add timer --entity Room`；`RoomDao` 末尾追加 `TowerState` / `TowerWakeTimer`；末尾跑 `modules.sh verify` |
| `scripts/testdata/tower-defense/**` | TowerRoom 并入 room 的 `Room`：席位代际、连接、在线交给 room（删 `Seat.Epoch/Connected` 与 `Connect(ticket)`）；`TowerInit` 里 `Room().Open(4)` 与名单同一笔事务；新增 `room_roster.go.txt`（Room 上的 `RoomJoining` 名单监听，普通房放行）；命令 / 视图 / 回执带服务端连接标识，先过 `Room().Member` + 连接核对；领奖 `Wallet().Add` + `Claimed`；建档改 enter 的 `EnterInit`、领奖查 `Enter().Built()`；Robot dispatch 加 `reconnect`（转 `RoomReconnect`）、连接取 `room.ConnectionKey`；REAL 用例改 Room / wallet / enter，WAL 计数 19 → 23，加“冷加载后席位在、全员断线”断言 |
| `docs/framework/TOWER-DEFENSE-INTEGRATION.md` | §2 归属表、§3.1 步骤、§3.2 阅读顺序、§3.3 可编译示例（已在生成工程里 `go vet` 通过）、§3.4 场景、新增 §3.5“旧断言 → 新断言”、§5～§8、§10～§12 |
| `docs/agent-skills/roost-genre/references/room-coop-pve.md` | 塔防拆出 §3.1 装模块命令块（统一命令块只留撤离 / 自走棋，等批 1D）；§2、§4、§6、§7 塔防部分 |
| `docs/maintenance/PLAN-2026-11-17-r9-genre-modules.md` | 状态行写批 0 完成；第 5 节全体规则改为主检出 + pathspec；新增第 9 节“批 0 定型的写法” |

场景数：`TestTowerDefense` 11 条（4 规则 + 6 Nest + 1 Robot）不变；REAL `TestTowerDefenseRealCrashRecovery` 1 条不变；新增模块场景（`module verify`）room 33、wallet 17、enter 29。

## 验收（方案第 5 节六条）

| # | 命令 | 结果 | 摘要 |
| --- | --- | --- | --- |
| 1 | `bash scripts/test-tower-defense-generated.sh` | exit 0：11 PASS、0 SKIP；build / vet / `id check` / `generate --check` 通过；`module verify` enter 29/29、room 33/33、wallet 17/17、0 skip | [default.txt](default.txt) |
| 2 | `python3 scripts/test-genres-generated.py --out <scratchpad>/matrix1 --genres tower-defense` | `status passed`，自定义 module 名下 11 条再过，`custom_module_verify` enter 29 / room 33 / wallet 17 | [matrix.txt](matrix.txt) |
| 3 | `source ~/.roost-it/roost-dataengine-it/env.sh; ROOST_TOWER_DEFENSE_REAL=1 bash scripts/test-tower-defense-generated.sh` | exit 0：缺省 11 条 + `TestTowerDefenseRealCrashRecovery` PASS（23 条未投影记录，SIGKILL 后恢复、补触发一次、17 次投递打完三波、第三进程重试不重复发奖），0 SKIP，库已删 | [real.txt](real.txt) |
| 4 | 变异（保留的生成工程里改 handler / 监听，跑对应场景，再恢复） | 五个变异全部变红，恢复后 11 条全绿 | [mutation.txt](mutation.txt) |
| 5 | `git status -- <本批文件>`；`GOWORK=off go test -count=1 -run 'Link\|Anchor\|Doc' .` | 本批文件在提交 `73c0706a` 之后无残留；文档测试 ok（同时跑过 `TrackedGoFilesAreGofmtClean`、`GeneratedScriptsAreInPretag`、`GeneratedProjectRealDependency…`，ok） | — |
| 6 | 手册 + skill reference 同步；skill 命令块真跑（接 SKILL.md §3.2 前置，自定 module `example.com/acme/mytd`） | 11 PASS、`module verify` 三个模块 n/n | [skill-block.txt](skill-block.txt) |

变异清单（第 4 条）：

| 变异 | 改法 | 变红的场景 |
| --- | --- | --- |
| A 去掉连接核对 | `towerMember` 不比 `seat.GetConnection()` | `ReadyDisconnectAndOldConnection`：旧连接拿新代际查到了回执 |
| B 去掉 `Claimed` 事实 | 领奖不写 `Claimed`（只调 `Wallet().Add`） | `CooperativeVictoryAndAtomicRewards`（duplicate reward）、`RobotBusiness`（team payout duplicated） |
| B2 去掉 wallet 发放 | 不调 `Wallet().Add` | 同上两条（余额不是 70） |
| C 名单监听放行所有人 | `Index < -1` | `DefeatNoRewardAndViewCopy`：局外人入座得到 `room_full` 而不是 `room_not_member` |
| D 不查 room 代际 | `Member` 换成只查席位 | `ReadyDisconnectAndOldConnection`（旧代际命令被接受）、`RobotBusiness`（重连后旧代际重放被接受） |

## 发现的模块缺口（按 Q1 只记录，没有改 modules/**）

- **room 缺一个“代际 + 连接 + 在线”一并核对的公开入口**：turn 组件的私有 `authorize` 与塔防的 `towerMember` 是同一段代码；批 1 的撤离、自走棋、合作、回合 PvP 都要。建议给 room 组件加 `Authorize(playerID, epoch, connection)`，turn 改调它（Q1 的 B 条件：两类以上都要、简单工程不必多做事）。R9 内先各自手写，等维护者定。

其它核对：方案风险 7（`test-genres-generated.py` 只改写 `.go/.yaml/.json/.proto/go.mod`）——模块模板里的 `.csv`（9 个）不含 `{{MODULE}}`，`.json`（5 个）只含 `{{PROJECT}}`（工程名，不是 module 路径），没有漏改。

## 拿不准的点

1. **玩法 handler 的注册方式**：塔防 handler 参数是手写接口（`ITowerRoomEntity`），生成器认不出实体与服务，仍生成全局 `RegisterTowerDefenseNestHandlers()`；room 自己的 handler 是 `service=game` 的按引擎注册（`…On`）。与改造前（TowerRoom 也是全局）一致，没有扩大问题，但正式工程里全局注册的 handler 由谁登记到 game 服务，没有在本批验证。
2. **Robot 的重连语义变了**：原来“TCP 重连后原样重放第一条命令得到原回执”，现在 TCP 重连要先 `RoomReconnect` 拿新代际，旧代际的重放被拒（客户端改用 `TowerReceipt` 查）。这是 room 模型下的正确语义，手册 §3.5 已写，但它让 Robot 场景的“重放”含义与其他未迁的玩法不同。
3. **原夹具的金币上限 100 万**换成了 wallet 的 int64 溢出检查（玩法上限样例消失）；判断是样例数值、不是契约，手册 §3.5 写明。
4. **以后同一个 Room 再装 turn**：塔防已 `add timer --entity Room` 并占了类型号 1；turn 的 `TurnDeadline` 怎样在已有定时器的 Room 上分号，本批没有试（塔防不装 turn）。批 1A（回合 PvP 用 turn）会第一次走到。
5. `roost add dao Player --entity Player` 在 game 模板上是空操作（模板已有 Player DAO），已从塔防脚本删除；其它玩法脚本还在调，无害。

## 交给协调者写的内容（本批没改 CHANGELOG / KNOWN-LIMITS）

CHANGELOG `[Unreleased]` 建议加：

- 变更：合作塔防玩法夹具改为“装模块 + 手写玩法核心”（R9 批 0）：装 `room wallet enter`，TowerRoom 并入 room 模块的 `Room`（塔防状态是 `RoomDao` 的 `TowerState` / `TowerWakeTimer`，名单准入是 `RoomJoining` 监听，命令先过 room 的代际与连接围栏），金币走 wallet、建档走 enter；场景名与条数（11）不变，旧断言 → 新断言见 TOWER-DEFENSE-INTEGRATION §3.5；REAL 用例的未投影 WAL 计数 19 → 23。
- 新增：`scripts/testdata/genre-common/modules.sh`（玩法脚本公用的装模块与 `module verify` 条数核对）；`test-genres-generated.py` 对装了模块的玩法在自定义 module 名下再跑 `module verify`。
- 修复：`scripts/pretag.sh` 清 REAL 开关时漏了 `ROOST_TURN_PVP_REAL`、`ROOST_COOP_ROGUELIKE_REAL`、`ROOST_SOCIAL_SIM_REAL`（外层 shell 带着它们跑 pretag 会去连真实存储）。

KNOWN-LIMITS：

- “模块化顺序（N1）”一条（收尾改写时）：塔防已按 R9 改造，模块清单 `room wallet enter`。
- 新的“尚未确认”候选（维护者定）：room 缺“代际 + 连接 + 在线”一并核对的公开入口（见上“模块缺口”）。
- “游戏接入验收”塔防一行：缺省档另有 `module verify` room 33 / wallet 17 / enter 29；REAL 2026-11-17 复跑通过。
