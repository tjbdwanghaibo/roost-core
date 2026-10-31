# 回合 PvP、合作 Roguelike 与异步社交经营

这三条纵切沿用 `game` 模板，业务夹具放在 `scripts/testdata/`，不新增框架玩法包或另一套完整 demo。它们是可运行的入门规则，不是成品游戏。完整应用仍参考 `demo/`。

## 生成与运行

在目标版本 roost-core 根目录执行；CLI 与 runtime 都使用这个检出：

```bash
ROOST_GENRE_KEEP=1 ROOST_GENRE_MODULE=example.com/myduel bash scripts/test-turn-pvp-generated.sh
ROOST_GENRE_KEEP=1 ROOST_GENRE_MODULE=example.com/mycoop bash scripts/test-coop-roguelike-generated.sh
ROOST_GENRE_KEEP=1 ROOST_GENRE_MODULE=example.com/myfarm bash scripts/test-social-sim-generated.sh
```

每条脚本打印临时目录，生成物位于其 `project/`。不指定 `KEEP` 时成功后删除；失败保留工程和日志。`ROOST_GENRE_MODULE` 可替换模块名，无需手工替换导入。脚本执行正式 CLI 创建工程、Player 与 GameRoom DAO、实体与 Nest sender 生成，然后运行 **9 个场景、零 SKIP**，最后 build、vet、ID 与生成漂移检查。三个脚本均加入 pretag。

公共生成机械步骤在 `scripts/testdata/genre-common/run.sh`，业务规则与专属断言在各玩法目录。生成工程主要文件：

- `game/rules/rules.go`：纯玩法状态机。
- `game/handler/scenario.go`：房间初始化、鉴权身份对应的 Player 与 Room 多实体命令。
- `db/def/player.go`、`db/def/game_room.go`：金币与房间状态的正式 DAO 定义。
- `game/handler/*_test.go`：可直接运行的 Sender 调用与断言。
- `protocol/def/scenario.go`、`loadtest/protocol/registry_gen.go`：业务 PB 定义及自动生成的公共 Robot 编解码注册。
- `game/scenarionet/dispatch.go`：已鉴权 TCP 请求到生成 Sender 的业务边界。
- `loadtest/scenario.go`、`cmd/robot/main.go`：公共 Robot 的请求、幂等重试与重连断言；不是另一套网络实现。

## 共同契约

房间属于 World 锁档，玩家属于 Player 原有锁档；命令由 `MultiSync_ScenarioCommand` 声明两实体，所有状态修改经 DAO setter，提交拒绝一起回滚。Roguelike 领奖与经营收获会在同一事务中更新 Room 领取事实和 Player 金币。

`ScenarioOpen` 是宿主入口，名单来自已鉴权的成局或邀请结果。客户端不能直接提交 `members` 或选择任意 `player` 实体；接入层必须从会话解析身份，并校验目标房间路由。`game/scenarionet` 已提供 PB 命令入口，但宿主仍负责真实票据、房间分配与路由，不能直接公开测试鉴权器。

每名成员只有最后一条命令回执，包含命令全内容与结果。相同 Seq、相同内容返回原结果且不重复支付；相同 Seq 改内容、跳序、过旧 Seq、旧 Revision 拒绝。这是有界去重，不是永久回执账本；旧请求结果未知时查询当前权威状态，不换新 Seq 盲重试。房间标识不可用于另一局，终局保留或删除由宿主决定。

`nowUnixMilli` 由生成的分发包装取服务端业务时间，不接受客户端时间。状态完整存进 DAO 的 JSON 字段以便阅读；大规模生产需按访问与同步需求拆字段，不将 JSON 整体公开给所有玩家。离线重载用例验证状态解码；新增 Robot 用例另外经过真实 TCP 断开重连，仍不代表数据库恢复。

## 各玩法范围与场景

| 玩法 | 规则 | 8 个场景 |
| --- | --- | --- |
| `turn-pvp` | 双人轮流攻击；每回合 30 秒；超时只消耗当前回合、扣当前行动方生命，再开启新回合，不追跑所有错过的回合 | 行动顺序、超时单拍、终局拒绝行动、重连状态继续；共同的相同重试/冲突、提交拒绝回滚、过期版本、非成员拒绝 |
| `coop-roguelike` | 双人投票选路线，双方齐备推进；分歧由固定种子决策；三阶段后分别领奖 | 队伍阶段屏障、奖励只到账一次、Room 与 Player 一起回滚、重载不重抽；四个共同场景 |
| `social-sim` | 主人种植，受邀访客帮助一次缩短 10 秒，成熟后主人收获；长时间离线只收本次种植，不凭空累积无限作物 | 访客权限、离线收获去重、帮助期限只缩短一次、收获双实体回滚；四个共同场景 |

回合超时由宿主通过受控命令触发，本纵切没有挂自动定时驱动；实际游戏可参考 card-rpg 的持久定时器，但必须保持预期 Revision，旧定时事件不能消耗下一回合。Roguelike 是离散房间流程，不含实时动作战斗、技能或物理；种子常量用于可复现样例，生产由服务器创建时注入并持久保存。社交经营只演示本进程两实体事务，不包含跨服访问者授权、Remote 或 Saga。

每类另有第 9 条 `TestScenarioRobotTCPRetryAndReconnect`：真实 loopback TCP → 生成 PB → 正式 Nest/DAO，执行首个动作、同连接重复、重连重复，最终 Revision 只能为 1。生成工程可使用真实票据运行：

```sh
# ROOST_ROBOT_TICKET 由自己的登录流程注入；不要提交票据。
go run ./cmd/robot -endpoint 127.0.0.1:7000 -room <已分配的新房间ID>
```

`loadtest.Smoke` 接受自己的 AuthProvider；嵌入 TCP 和 Gate 使用同一个 `player-tcp` transport。当前自动用例实跑嵌入 TCP，Gate 路由另验；固定重试是幂等功能验证，不能用它报告正常写入 TPS。压力场景必须推进新的 Seq/Revision，并按玩法完成结算与数据断言。

## 验证边界

提交器为可注入拒绝的内存替身，DAO / undo / Nest / 生成 Sender 为正式实现。默认模式不连接 Mongo、Redis、NATS，不提供 REAL 模式；不能据此声称 strict 已真正 fsync、进程崩溃恢复或跨进程原子性通过。完整 Gate、EntitySync、自动期限驱动、客户端展示、多人容量和跨机恢复是独立验收项。

## 后续补齐顺序

接下来优先完成已有能力的端到端验收，不继续横向堆玩法。完整参考沿用 `game-demo`：Gate/Game 启动选择和标准 C# 客户端的业务 PB、Nest 修改 DAO、PB EntitySync 全量/增量/删除与重连已接成[同机可运行链路](GATEWAY.md#生成工程与标准-c-客户端联合链路)。随后补真实账号票据、Mongo 持久化与崩溃恢复。客户端必须展示变化类型，不能以本地改值代替服务器同步。三类玩法分别补自动期限驱动、Gate/真实账号与同步接入，再做 Unity Editor/IL2CPP 实跑；各项未完成前仍适用上面的验证边界。

## 合作 Roguelike 整局 Robot

当前合作脚本为 **10 条**，回合 PvP/异步经营仍各 9 条。新增 `CoopView` PB、成员授权的 Nest 视图查询、`scenarionet.CoopDispatch` 与 `loadtest.Coop`；`cmd/robot` 默认运行双客户端三关与领奖闭环，另需 `ROOST_ROBOT_PEER_TICKET`。详细流程、报告口径和未覆盖项统一见 [Robot 完整业务样板](ROBOT.md#完整业务样板)。这是客户端 PB 权威视图查询，不是 EntitySync；仍使用替身鉴权/提交器，宿主负责分配新房间和真实票据。
