# 回合 PvP、合作 Roguelike 与异步社交经营

这三条纵切沿用 `game` 模板，**先装业务模块，再只手写模块没覆盖的部分**（R9，2026-11-17）；业务夹具放在 `scripts/testdata/`，不新增框架玩法包或另一套完整 demo。它们是可运行的入门规则，不是成品游戏。完整应用仍参考 `demo/`。

## 生成与运行

在目标版本 roost-core 根目录执行；CLI 与 runtime 都使用这个检出：

```bash
ROOST_GENRE_KEEP=1 ROOST_GENRE_MODULE=example.com/myduel bash scripts/test-turn-pvp-generated.sh
ROOST_GENRE_KEEP=1 ROOST_GENRE_MODULE=example.com/mycoop bash scripts/test-coop-roguelike-generated.sh
ROOST_GENRE_KEEP=1 ROOST_GENRE_MODULE=example.com/myfarm bash scripts/test-social-sim-generated.sh
```

每条脚本打印临时目录，生成物位于其 `project/`。不指定 `KEEP` 时成功后删除；失败保留工程和日志。`ROOST_GENRE_MODULE` 可替换模块名，无需手工替换导入。

公共生成机制在 `scripts/testdata/genre-common/run.sh`，按顺序：`project new -template game` → `roost module add <清单>`（`genre-common/modules.sh add`，依赖由安装器连带装上）→ 往模块的主 DAO 末尾追加玩法字段 → 拷玩法夹具 → `roost generate` → 玩法场景（社交经营 **9 条**，回合 PvP、合作 Roguelike 各 **10 条**，零 SKIP）→ build / vet / `id check` / `generate --check` → `roost module verify`（`genre-common/modules.sh verify`：每个已装模块通过数等于本检出 `modules/<名>/module.yaml` 的 `verify.pass`、0 skip，结果写 `module-verify.json`）。模块清单写在各玩法脚本的 `genre_modules` 数组里：

| 玩法 | `genre_modules` | 连带装上 | `module verify` |
| --- | --- | --- | --- |
| `turn-pvp` | `turn wallet` | room | room 34、turn 25、wallet 17 |
| `coop-roguelike` | `room wallet` | — | room 34、wallet 17 |
| `social-sim` | `friends wallet` | — | friends 18、wallet 17 |

Player 的主 DAO、玩家 TCP 接入由模块的 `plan` 代办；脚本不再 `add entity GameRoom` / `add dao`、不再 `add timer`（回合期限是 turn 的 `TurnDeadline`）、不再往 Player 插 `Coins`（金币是 wallet 的 `PlayerDao.Wallet`）。三个脚本均在 pretag。

## 模块接走了什么，手写了什么

| 玩法 | 装（模块） | 手写（夹具）与样板 |
| --- | --- | --- |
| 回合 PvP | **room**：两人席位、代际、连接与在线（GameRoom 并入 room 的 `Room`，Q3 A）；**turn**：轮到谁、每人连续命令号与最后一条重放、回合期限与超时推进；**wallet**：命中奖励 | `RoomDao.PvpState`：生命、胜负、带内容的攻击回执（turn 只为“无载荷的结束回合”判重）；名单准入（`event.RoomJoining` 监听，借鉴 matchroom 的固定名单）；对 turn 组件的两处扩展（turn README“超时要做别的事改 OnDeadline”）：超时扣行动方 1 点生命、分出胜负即结束 |
| 合作 Roguelike | **room**：同上；**wallet**：通关金币 | `RoomDao.CoopState`：同时投票、路线种子、三关推进、领奖事实 `Claimed`（**借鉴 turn** 的“每人连续命令号 + 最后一条可重放”；turn README 写明不适合所有人同时提交的回合）；名单准入；只读成员视图 PB 与整局 Robot |
| 社交经营 | **friends**：访客须是主人的好友；**wallet**：收获金币 | 主人 `PlayerDao.SocialFarm`：种植、成熟时刻、帮助缩短一次、收获（**借鉴 friends** 的两 Player 事务）；不再需要房间 |

生成工程主要文件：

- `game/modules/scenariorules/rules.go`：纯玩法状态机（原 `game/modules/rules`，改名免得与 rules 模块混淆）。
- `game/handler/scenario.go`：玩法 handler。房间玩法是 `[Room, Player]` 多实体命令，参数是手写的窄接口（嵌 `view.RoomHolder`，回合 PvP 再嵌 `view.TurnHolder`）；经营是主人一人（种植、收获）或 `[主人, 访客]`（帮助）。
- `game/entities/room/scenario.go`：房间玩法的名单准入监听（玩法状态为空的普通房放行，room / turn 自带场景不受影响）。
- `game/components/turn/pvp.go`：回合 PvP 对 turn 组件的扩展；`run.sh` 在同包 `turn.go` 的 `OnDeadline` 与 `advance` 各插一行调用它。
- `db/def/room.go` / `db/def/player.go`：模块的主 DAO，末尾追加玩法字段（`PvpState` / `CoopState` / `SocialFarm`，严格 JSON，`nosync`）。
- `game/handler/*_test.go`：`scenario_test.go`（公共 4 条与夹具外壳）、`scenario_room_test.go`（房间玩法共用的入座 / 接管 / 重连）、`scenario_genre_test.go`（各玩法的夹具差异）、`genre_test.go`（玩法专属场景）。
- `protocol/def/scenario.go`、`loadtest/protocol/registry_gen.go`：业务 PB 与自动生成的公共 Robot 编解码注册；请求带 `Epoch`，应答带 reconnect 的新 `Epoch`。
- `game/modules/scenarionet/dispatch.go` + `command.go`：已鉴权 TCP 请求到生成 Sender 的边界。请求的 `RoomID` 是命令目标的唯一号（房间号 / 农场主人的玩家号），接入层换成 Entity ID（房间经 `room.EntityID`），客户端不能把别的种类的 Entity ID 交给 Sender；房间玩法的连接标识取自服务端 Session（`room.ConnectionKey`）。
- `loadtest/scenario.go`、`cmd/robot/main.go`：公共 Robot 的步骤、幂等重试、换连接接管与断言。

## 共同契约

房间玩法的命令目标是 room 模块的 `Room`（World 锁档），玩家是 Player；命令由生成的 `MultiSync_ScenarioCommand(ctx, roomID, playerID, epoch, connection, cmd)` 声明两实体，所有状态修改经 DAO setter，提交拒绝时玩法字段、模块状态（席位、回合、期限节点）与钱包一起回滚。经营的种植 / 收获只锁主人（农场与钱包在同一个 Player 上），帮助锁 `[主人, 访客]`。

身份围栏全部交给 room（照塔防的 `towerMember`）：**新命令**经 `Room().Authorize(playerID, epoch, connection)`（代际、连接、在线一并核对；回合 PvP 经 turn 的 `CheckEnd`，内部同一入口）；合作的**最后一条回执重放**不要求在线，只核对 `Member` 的代际与席位认下的那条连接——进程重启后全员断线时，原连接、原代际的重试仍得原回执；新命令要先 `RoomReconnect` 换代际。回合 PvP 的重放随 turn 的语义，也要求在线。经营没有席位，访客资格按主人的好友名单在同一把锁内判断。

开房（`ScenarioOpen`）是宿主入口：开房与名单同一笔事务，名单来自已鉴权的成局结果；之后成员经 room 的 `RoomJoin` 入座（名单监听放行），回合 PvP 再由房主经 turn 的 `TurnStart` 开局。客户端不能提交 `members` 或选择任意 `player` 实体；接入层从会话解析身份与连接标识。经营的好友关系由 friends 的申请 / 同意建立（双边动作要求两人本进程在线，夹具以服务端身份调 Sender）。

每名成员只有最后一条命令回执，包含命令全内容与结果（代际不在回执里）。相同 Seq、相同内容返回原结果且不重复支付；相同 Seq 改内容、跳序、过旧 Seq、旧 Revision 拒绝。这是有界去重，不是永久回执账本；旧请求结果未知时查询当前权威状态，不换新 Seq 盲重试。“领过没有”（合作的 `Claimed`）是玩法自己的长期事实，金币经 wallet 的 `Add`（不留回执，只发一次由玩法事实保证）。

`nowUnixMilli` 由生成的分发包装取服务端业务时间，不接受客户端时间。

## 各玩法范围与场景

| 玩法 | 规则 | 场景（玩法专属 + 公共 4 条 + TCP Robot） |
| --- | --- | --- |
| `turn-pvp` | 双人轮流攻击，每次命中 1 金币；每回合 30 秒；超时只消耗当前回合、扣当前行动方 1 点生命，交给对方，不追跑错过的回合；生命归零即终局 | 行动顺序、超时单拍、Room 定时器自动超时、终局拒绝行动、重连读权威回合；公共 4 条；TCP Robot 到终局 |
| `coop-roguelike` | 双人投票选路线，双方齐备推进；分歧由固定种子决策；三阶段后各自领 10 金币 | 队伍阶段屏障、奖励只到账一次、Room 与 Player 一起回滚、重连不重抽；公共 4 条；TCP 首动作 + 整局 Robot |
| `social-sim` | 主人种植，好友访客帮助一次缩短 10 秒，成熟后主人收获 5 金币；长时间离线只收本次种植 | 访客权限、离线收获去重、帮助期限只缩短一次、收获回滚；公共 4 条；TCP Robot 到收获 |

公共 4 条（`genre-common/gameplay_test.go.txt`）：相同重试 / 改内容冲突、提交被拒整笔回滚后同一命令真正生效一次、过期版本被拒、非成员被拒。被拒的断言比较命令目标与两名成员的**完整持久文档**（BSON 解码后比较），不只比玩法 JSON 与金币。

### 模块化之后场景语义的变化（R9，2026-11-17）

场景名与条数不变；下表是改过断言的场景（旧断言 → 新断言）。

| 场景 | 旧断言 | 新断言 |
| --- | --- | --- |
| 公共 `RetryAndConflict` / `StaleRevision` / `CommitRejectionRollsBack` | 被拒后 GameRoom 的 State 字符串与 Player.Coins 不变 | 被拒后 Room / 两名 Player 的完整持久文档（含模块的席位、回合、期限节点与钱包）不变；`CommitRejection` 另要求放开后同一命令真正生效一次（Revision=1、回执就是这条命令） |
| 公共 `Membership` | 纯状态机 `State.Apply(999, …)` 报错 | 存在但不在名单 / 不是好友的玩家经正式 handler 发命令被拒（room 的席位、经营的好友名单），什么都不写 |
| 回合 PvP `StaleRevision` | 旧 Revision 被拒 | 期望版本改用 turn 的回合号（Revision = 已消耗回合数，回合号 = Revision+1）：旧回合被 turn 拒绝 |
| 回合 PvP `TurnOrder` | 生命 2/2 | 生命 2/2，且两人钱包各 1（命中奖励；旧纵切没有奖励，Coins 恒 0） |
| 回合 PvP `TimeoutConsumesOneTurn` | 客户端上报 `timeout` 命令消费当前回合 | 客户端不能上报超时（`timeout` 被拒）；服务端投递一次 Room 定时器 Tick（turn 的 `TurnDeadline`）消费当前回合，行动方扣 1 点生命；再投一次不再消费 |
| 回合 PvP `TerminalCannotAttack` | 终局、胜者 0 | 另断言 Revision=5、turn 的期限为 0（turn 随对局一起结束） |
| 回合 PvP `ReconnectReadsAuthoritativeTurn` | 本地解码状态预演一步与服务端一致 | 行动方断线（`RoomDisconnect`）后行动被拒；换连接 `RoomReconnect` 拿新代际，旧代际的命令被拒，turn 的 `TurnView` 读到权威回合（轮到自己、回合 2），新代际行动成功 |
| 回合 PvP `TurnTimesOutByTheRoomTimer` | GameRoom 定时器（`add timer`）推进 | 同样经真实驱动循环，但期限是 turn 的 `TurnDeadline`：行动换掉期限节点，旧期限不再触发；终局后节点为 0 |
| 公共 PB（三类 Robot 的请求） | `RoomID` 是 GameRoom 的 Entity ID，原样交给 Sender | `RoomID` 是唯一号（房间号 / 农场主人玩家号），接入层换成 Entity ID；新增 `Epoch`（请求）与 reconnect 的新 `Epoch`（应答） |
| 回合 PvP Robot（第 9 条） | 首动作 Revision=1、Reward=0；终局 Revision=5 | 每条新 TCP 连接先接管席位（`reconnect` → `RoomReconnect`），每次命中 Reward=1；终局 Revision=5、两人钱包 3/2；局外人接管与命令都被拒 |
| 合作 `RewardExactlyOnce` / `RewardRollsBackBothEntities` | Player.Coins | wallet 余额（`Claimed` 仍在 Room） |
| 合作 `SeedSurvivesReload` | 本地解码状态预演一步与服务端一致 | 另加一步：队友换连接 `RoomReconnect` 之后，新代际投票与预演一致（种子、阶段不变） |
| 合作 Robot（第 9、10 条） | 直接发命令 | 每条新连接先带席位当前代际接管（`loadtest.Coop` 多一个 `epochs` 参数，`cmd/robot` 多 `-epoch` / `-peer-epoch`）；领奖后换连接重新接管再重投 |
| 经营 `VisitorPermissions` | 房间名单第二人是访客 | 访客是主人的好友（friends）；局外人不是好友 |
| 经营 `OfflineHarvestOnce` / `HarvestRollback` | Player.Coins；收获是 GameRoom + Player 双实体回滚 | wallet 余额；收获的农场与钱包都在主人这一个 Player 上，同一笔事务回滚 |

回合超时由 turn 的持久定时器自动推进：`module add turn` 让安装器给 Room 补一套实体定时器，并在 `timers.room.types` 里为 `TurnDeadline` 分配类型号。TurnStart 登记第一个期限；到期由生成的 `room.TimerDriver` 投递 Tick，turn 的 `OnDeadline` 在 Room 的 Nest 锁与事务内计一次超时、推进一回合并从触发时刻续期；本玩法插入的 `pvpTimeout` 在同一笔事务里扣行动方生命。期限内有人行动时 `End` 换掉期限节点，旧节点不会消费新回合；分出胜负时 `advance` 结束 turn，不再续期。离线语义沿用框架：房间不在内存期间到期的，加载后只补触发一次、只消费一个回合。夹具只拨 `clock.SetSource`（驱动缺省读 fctx 业务时钟），不调用驱动的 `SetClock`，所以与 turn / room 自带场景放在同一次 `go test` 里也不串（2026-11-17 实测整包一起跑通过）。

Roguelike 是离散房间流程，不含实时动作战斗、技能或物理；种子常量用于可复现样例，生产由服务器创建时注入并持久保存。社交经营只演示本进程两 Player 事务，不包含跨服访问者授权、Remote 或 Saga；friends 的双边动作要求两人本进程在线（模块边界）。

### TCP Robot

每类都有 `TestScenarioRobotTCPRetryAndReconnect`：真实 loopback TCP → 生成 PB → 正式 Nest/DAO。房间玩法先接管席位、再发首个动作、同连接重复、换连接重新接管后重复，Revision=1；经营直接发首个动作。回合 PvP 继续至终态（Revision=5），社交经营继续至帮助 / 收获（Revision=3），重试不重复奖励。合作的完整三关另由第 10 条覆盖。生成工程可使用真实票据运行：

```sh
# ROOST_ROBOT_TICKET 由自己的登录流程注入；不要提交票据。
# 回合 PvP / 社交经营：每个身份使用自己的步骤文件，Request 含 RoomID/Seq/Revision/Kind/Epoch。
# 房间玩法新连接的第一步是 {"Request":{"RoomID":<房间>,"Kind":"reconnect","Epoch":<RoomJoin 拿到的代际>}}，
# 之后 Epoch 填 0 即沿用接管得到的新代际。RoomID 都是唯一号：房间号 / 经营的农场主人玩家号。
go run ./cmd/robot -endpoint 127.0.0.1:7000 -steps steps.json
# 合作 Roguelike：完整双客户端流程，两人须已 RoomJoin 入座；另需 ROOST_ROBOT_PEER_TICKET。
go run ./cmd/robot -endpoint 127.0.0.1:7000 -room <已分配的新房间ID> -epoch <本人代际> -peer-epoch <队友代际>
```

`loadtest.Commands` 的 Step 包含 Request、WantCode、Revision、Reward 和 Reconnect；`loadtest.KindReconnect` 是 Robot 换连接后接管席位的请求（服务端转 room 的 `RoomReconnect`；真实客户端直接发 room 模块自带的 `RoomReconnect` 协议）。旧的 `loadtest.Smoke` 已删除（首动作验证并入 `Commands` 的步骤）。嵌入 TCP 和 Gate 使用同一个 `player-tcp` transport。当前自动用例实跑嵌入 TCP，Gate 路由另验；固定重试是幂等功能验证，不能用它报告正常写入 TPS。

## 验证边界

默认场景的提交器为可注入拒绝的内存替身，DAO / undo / Nest / 生成 Sender / 模块组件为正式实现；默认不连接 Mongo、Redis、NATS，不能用这些场景证明 strict fsync 或进程崩溃恢复。三个玩法各有显式 REAL 档，范围见下文。TCP/PB 场景与存储恢复分别计数；只有回合配方的独立 Gate 已在对应真实档运行，EntitySync、客户端展示、多人容量和跨机恢复仍是独立项。

## 后续补齐顺序

接下来优先完成已有能力的端到端验收，不继续横向堆玩法。完整参考沿用 `game-demo`：Gate/Game 启动选择和标准 C# 客户端的业务 PB、Nest 修改 DAO、PB EntitySync 全量/增量/删除与重连已接成[同机可运行链路](GATEWAY.md#生成工程与标准-c-客户端联合链路)。客户端必须展示变化类型，不能以本地改值代替服务器同步。三类玩法分别补 Gate/真实账号与同步接入（room 不广播，D24 A 暂缓），再做 Unity Editor/IL2CPP 实跑；各项未完成前仍适用上面的验证边界。

## 合作 Roguelike 整局 Robot

当前合作与回合 PvP 脚本各 **10 条**，异步经营 9 条。合作另有 `CoopView` PB、成员授权的 Nest 视图查询（room 的 `View` 核对连接，金币取 wallet 余额）、`scenarionet.CoopDispatch` 与 `loadtest.Coop`；`cmd/robot` 默认运行双客户端三关与领奖闭环，另需 `ROOST_ROBOT_PEER_TICKET` 与两人的席位代际。详细流程、报告口径和未覆盖项统一见 [Robot 完整业务样板](ROBOT.md#完整业务样板)。这是客户端 PB 权威视图查询，不是 EntitySync；仍使用替身鉴权/提交器，宿主负责分配新房间和真实票据。

## 回合 PvP 真实档

使用本机私有 mirror-local 的 Mongo、Redis、NATS 环境，运行 `ROOST_TURN_PVP_REAL=1 GOWORK=off GOFLAGS=-p=2 bash scripts/test-turn-pvp-generated.sh`。默认 10 条场景保持不变，真实档再跑 `TestTurnPVPRealTimerRecovery` 的 seed / recover 两阶段（各独立进程、race、零 SKIP），随后调用 `scripts/test-turn-pvp-real.sh`。可用 `ROOST_GENRE_MODULE` 验证自定义 module。这个用例名字以 `TestTurn` 开头，但 turn 的 `module verify` 只运行、只统计 turn 装进工程的测试文件里的用例（维护者 2026-11-17），不会把它缺省档的 SKIP 算进 turn 的条数，所以不带构建标签：缺省档编译 / vet，REAL 档按名字运行。

`TestTurnPVPRealTimerRecovery`（R9 起测 Room 上 turn 的 `TurnDeadline`，Q7 A）：seed 进程经正式 Sender 开房 + 名单、两人 `RoomJoin`、房主 `TurnStart`、玩家 0 攻击一次（turn 回执、命中奖励、新期限节点），刷盘退出；recover 进程冷加载同一真实 Mongo / 文件 WAL，核对 Room 与两名 Player 的完整持久文档及驱动的到期索引。在线状态不落库：原连接、原代际的重放被拒（turn 的重放也要求在线），两人换连接 `RoomReconnect` 后重放得原结果；业务钟跨过两小时、驱动循环只补消费一个回合（行动方生命 3→1），原回执可重放、旧页面被拒，继续至终局后期限节点为 0。

后者生成正式 `-recipe turn-pvp`：真实 portal/account 登录、MatchEnqueue/MatchStatus 成局、RoomJoin、TurnStart/End/View 经独立 Gate → NATS → Game Ingress → Nest/DAO；第 2 回合收到确认后 SIGKILL game，离线跨过多个期限，再用同一 WAL / Mongo 重启。新连接带旧 Epoch 被拒，RoomReconnect 增加 Epoch，持久序号可重放；超时只补一次，之后完成并关闭房间。`TestTurnRecipeRealLifecycle` 的 seed / recover 各实际执行一次。**纵切与配方从 R9 起是同一套 Room / Turn 模型**：配方只有顺序回合，纵切在其上加了伤害、胜负与命中奖励；两者证据仍分别计数。

`ROOST_TURN_PVP_PORT_SHIFT`（默认 35000）、`ROOST_TURN_PVP_ETCD_PORT`（57779）可隔离端口；脚本只清理自己的生成工程、数据及进程，日志在 `ROOST_TURN_PVP_OUT`。这是 macOS 单机功能验收，无跨机接管、断电或容量结论。game-demo 另有 `scripts/test-demo-turn.sh` 实跑 Makefile 的三个 dev-smoke 场景及 Gate 下的 turn Robot，见 [验收记录](../maintenance/evidence/turn-real-20261114/README.md)。

## 合作与经营的 WAL-only 真实档

```sh
# 先 source 私有 mirror-local 的 env.sh；这两档只连接其中的 Mongo 副本集。
GOWORK=off GOFLAGS=-p=2 ROOST_COOP_ROGUELIKE_REAL=1 bash scripts/test-coop-roguelike-generated.sh
GOWORK=off GOFLAGS=-p=2 ROOST_SOCIAL_SIM_REAL=1 bash scripts/test-social-sim-generated.sh
```

合作默认 **10**、经营默认 **9** 条场景保持不变；各自再运行 1 条真实测试、2 个崩溃子场景（race、零 SKIP）。`ROOST_GENRE_MODULE` 支持自定义 module，`ROOST_GENRE_OUTPUT` 指定保留生成工程与完整日志。默认档也把真实恢复夹具装入工程并编译 / vet，pretag 仍仅运行默认档。

共用的 `genre-common/persistence/runtime_test.go.txt` 只负责正式 Runtime / Nest / 生成 DAO / Sender / 文件 WAL / Mongo 装配和崩溃屏障；实体集合、开局、状态与命令都来自玩法的夹具钩子（合作：room 的 Room + 两名 Player；经营：两名 Player，命令目标是主人）。子进程关闭后台投影，所有命令经 strict 确认，确认 WAL 已 fsync、Appended=Admitted、Acknowledged=0，父子均核对本玩法的真实业务集合为空；通过独立管道报告完整持久文档后，由父进程 SIGKILL。恢复读取原 WAL，核对完整持久文档（BSON 解码后比较），先用**原连接、原代际**重放最后回执（不要求在线），再换连接重连（合作）继续业务；正常排空后再开第三个进程核对终态，同样先重放、再重连重试。没有改变持久策略，也没有靠延时猜测未投影窗口。

| 真实测试 | 崩溃点 | 恢复断言 |
| --- | --- | --- |
| `TestCoopRoguelikeRealCrashRecovery` | 第二关只有一人投票（9 条 strict 记录；R9 前 GameRoom 版 7 条）；三关完成后第一人已领奖（13 条；R9 前 11 条） | 固定路线种子、阶段、半票、席位代际及回执不变；冷加载后全员断线，原连接重放得原回执，`RoomReconnect` 后继续到双方各 10 金币；同请求重试只返回原结果、新序号重复领奖拒绝；第三进程继续保持终态 |
| `TestSocialSimRealCrashRecovery` | 加好友、种植、好友帮助后尚未成熟（6 条）；主人收获已确认（7 条） | 好友关系与 ReadyAt 不变，离线两小时加载不自动发收益；仅主人显式收获一次 5 金币，帮助不会再次提前，访客不能领；第三进程保持 Harvests=1、钱包 5/0 |

完整证据和复跑见 [B5 验收记录](../maintenance/evidence/session-real-20261114/README.md) 与 [R9 1A 证据](../maintenance/evidence/r9-1a-20261117/README.md)。这些真实测试直接调用正式 Sender，不包含 TCP/PB、真实登录、Gate、NATS、跨服访问者授权或客户端引擎。各脚本先执行的既有 TCP Robot 使用替身提交器，不能与本段拼称为同一条网络与存储联合链。没有新增社交经营自动 timer，成熟仍按持久 ReadyAt 在 harvest 时判定；不承诺永久回执账本、回复未知、断电、跨机或容量。
