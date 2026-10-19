# 塔防／合作 PvE：共享建造、路径与持久波次

本篇用“四人合作、5×5 地图、三波敌人”学习 core 的房间权威与持久时间接入。入口 0、基地 24，队伍共同使用局内金币，每人拥有自己建造的塔。规则是教学假设，不是公共框架默认玩法。先读[入门手册](../GETTING-STARTED.md)，再运行[正式消费者](../../scripts/test-tower-defense-generated.sh)。

## 学习：业务场景与框架分工

World 类 `TowerRoom` 拥有队伍、地格、塔、路径、怪物位置／血量、基地、金币、波次、时钟和回执。四名玩家提交的命令都在 Room 的 Nest 锁内裁决。Player 保持静态 sid，永久奖励通过本地同库的多实体事务领取；Room 内金币与 Player 金币是两个不同经济域。

| 业务场景 | core 接法 | 必须维护的契约 |
| --- | --- | --- |
| 玩家建档与成局 | `Sync_TowerPlayerInit`、`Sync_TowerInit`，生成 Player／TowerRoom DAO | Create 内存实体不等于持久建档；名单固定，已有房间不因初始化重试重新开局 |
| 配置固定 | `configdata.Store` → `CaptureRules` → Room State | 波数、怪物血量、基地血量、建造费用与计时间隔按值冻结；热更只影响新房间 |
| 同格建造竞争 | `Sync_TowerCommand` → 同一 Room 锁 → 局部状态计算 → `SetState` | 同格只能一人成功；共同金币只扣一次，重复 Seq 返回原回执 |
| 阻路验证 | 业务确定性 BFS，与扣费／建造同事务 | 入口到基地必须可达，堵路整体拒绝；不能先扣款再异步计算路线 |
| 升级与出售 | Owner、期望 Level、Gold 同锁校验 | 只允许所有者操作；升级最多三级；退款为总投入的一半向下取整，不能重复返还 |
| 准备与托管 | 每席 Ready，房间阶段期限 | 已准备玩家不能改塔；全员准备立即开波，未准备或掉线则在期限到达后用现有防御开波 |
| 服务端战斗 | 纯业务步骤＋生成实体 timer | 塔按格序攻击，优先最靠近基地的敌人；射程内扣血后怪物推进一格，逃逸扣基地血量 |
| 波次与胜负 | Room 持久 Tick、Enemies、BaseHP、Result | 波次结束发局内补给；基地归零立即失败；完成全部波次且基地存活获胜 |
| 断线重连 | 持久 Epoch、`Sync_TowerConnect/Receipt/View` | 新会话票由可信入口签发，旧连接不能写或踢新连接；新连接查询旧回执 |
| 团队奖励 | `MultiSync_TowerClaim(PlayerID, RoomID)` | 团队成功每人 70，失败为 0；Player Gold 与 Room Claimed 同事务 |
| 活动波次恢复 | strict WAL、Mongo、DAO 水合、TimerDriver 循环 | 恢复每只怪物的血量／位置及期限；只补一步，不跳过整场战斗 |

源码阅读顺序：[规则](../../scripts/testdata/tower-defense/rules.go.txt) → [状态与 BFS](../../scripts/testdata/tower-defense/state.go.txt) → [命令与波次](../../scripts/testdata/tower-defense/match.go.txt) → [handlers](../../scripts/testdata/tower-defense/handlers.go.txt) → [timer](../../scripts/testdata/tower-defense/timer_handlers.go.txt)。所有 `.go.txt` 都会复制进独立生成工程后编译、运行。

### 如何使用

CLI 创建 Player DAO、TowerRoom Entity／DAO／timer；Room 分类设为 World，使 `MultiSync` 领奖与 Other 类 Player 按框架锁序执行。业务只改定义文件，生成 Sender、DAO setter 和 timer 组件由 CLI 生成。

```go
api := syncsender.NewTowerDefenseSender(engine)
v, err := api.Sync_TowerView(ctx, roomID, authenticatedPlayerID)
if err != nil { return err }
_, err = api.Sync_TowerCommand(ctx, roomID, authenticatedPlayerID, towerdefense.Command{
    Seq: v.Self.NextSeq, Epoch: v.Self.Epoch, Round: v.Round,
    Kind: "build", A: 1, // A 是地格，build 的 B 必须为 0。
}, serverNow.UnixMilli())
return err
```

`upgrade`／`sell` 的 A 是地格，B 是期望塔等级；`ready` 的 A、B 均为 0。客户端只提交操作，不提供费用、伤害、怪物坐标、基地血量、时间、胜负和奖励。PlayerID 来自鉴权会话。命令 Seq 每席永久递增；同身份重试必须逐字段相同，不得因为超时换新 Seq 再建塔。

共享房间的所有建造、扣费、BFS、命令回执与 timer 变动均在 `rollback=undo durability=strict` 的 Nest handler 内。规则在局部解码副本上执行，失败不会写回；注入提交前业务拒绝时，Nest undo 恢复已写 DAO 和 timer。账本每席最多 128 条，满后拒绝新玩家操作，保留旧回执；后台波次和领奖仍可继续。

战斗阶段禁止建造／拆塔，路线不会在怪物行进时变化。BFS 的邻接次序固定，同地图产生相同路径；怪物三只同波生成，塔按地格升序攻击，目标按路径进度、再按敌人索引确定。无地图随机数、无客户端命中裁决。最多 25 个逻辑步可结束一波，地图与数值都设上限，禁止无限追赶离线 tick。

准备期限到了只开波，战斗期限到了只算一步，下一次期限从实际服务端 now 起算。进程停机一小时不会补算一小时伤害；这是示例明确选择的暂停／恢复策略。若产品要求真实墙钟离线损失，需要另行设计有界补算，不应直接循环丢失的所有 tick。

驱动装配顺序：注册 Entity／handlers → 加载前设置时钟 → 启动 Nest → `TimerDriver.Bind`／`Start`。回调检查持久 timer ID、参数、期限，状态与下次 timer 同事务。停机先 Stop 驱动，再排空 Nest 和 Runtime。基础测试用 `DeliverDue` 精确推进；真实恢复启动驱动自己的循环。生成实体计时器接法也可参照[实体定时器源码](../../framework/nest/entitytimer)。

## 验收与复跑

```bash
bash scripts/test-tower-defense-generated.sh
ROOST_TOWER_DEFENSE_REAL=1 \
ROOST_DATAENGINE_IT_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0' \
bash scripts/test-tower-defense-generated.sh
```

Mongo 必须是隔离测试副本集；脚本给每次运行生成唯一数据库和 WAL，成功清理本次库，失败保留证据。`ROOST_TOWER_DEFENSE_KEEP=1` 保留成功消费者。默认提交器替身仅验证调度与业务，不提供真实持久化证据。

| 实际测试 | 证明的行为 |
| --- | --- |
| `TestTowerDefenseConcurrentCellReservationAndRetry` | 四人竞争同格，只有一个所有者和一次扣费，重试不重复扣款 |
| `TestTowerDefenseBlockingRouteAndForeignSaleRollback` | 阻路、入口建造、他人出售、旧等级拒绝；升级／出售退款只一次 |
| `TestTowerDefenseReadyDisconnectAndOldConnection` | 准备冻结、掉线托管、旧连接隔离、当前会话查旧回执、迟到输入拒绝 |
| `TestTowerDefenseUndoRestoresSharedGoldRouteAndTimer` | 真实 Nest undo 恢复塔、共同金币、路线与期限 |
| `TestTowerDefenseCooperativeVictoryAndAtomicRewards` | 四人建塔、三波九只怪物全部击杀、基地不掉血；双实体领奖 undo、四人并发重复领奖 |
| `TestTowerDefenseDefeatNoRewardAndViewCopy` | 无防御导致六只逃逸、基地归零、失败不奖励；局外人拒绝、视图不引用权威路径切片 |
| 四项规则测试 | 单格绕路与入口封堵、确定性复演、一小时离线只补一步、非法路径／Owner／大状态拒绝、账本上限、配置热更 |
| `TestTowerDefenseRealCrashRecovery` | 四玩家建档及第一战斗步共 14 条 strict 未投影记录后 SIGKILL；完整恢复怪物／塔／路径／资产／回执／timer；驱动从第二步续战三波；第三进程冷加载后重试所有命令及四人奖励 |

[真实恢复用例](../../scripts/testdata/tower-defense/persistence/recovery_test.go)先比较完整冷数据，再投递计时；不能靠重复初始化“修复”丢失状态。strict 成功是 WAL 持久，Mongo 可滞后；测试只终止自己创建的子进程。脚本要求 10 项基础测试及显式启用的恢复测试无跳过通过，同时完成 race、消费者 build/vet、ID 与生成漂移检查。

## 业务边界与框架结论

已验证现有 Entity／DAO／Nest／configdata／entitytimer／DataEngine 能支撑这条合作塔防后端链路，本轮未确认新增框架缺陷。寻路、塔的所有权、共享金币权限、攻击顺序、伤害、波次、离线暂停和分奖策略均属业务。

尚未接入正式客户端和公网鉴权、匹配与中途补位、断线超时票据服务、共享资金反恶意消耗、投票拆塔、完整技能／状态效果 Host、多路导航／动态怪物改道、Boss 内容、战斗广播、自动派奖、房间长期归档、跨 sid 奖励和权威 HA。四个逻辑玩家不是四台客户端或生产容量验收；500ms 教学步长不构成延迟／吞吐保证。跨 sid 继续采用 Player 静态归属与 Remote／Saga 原服协作；本地多实体领奖不升级为分布式原子性。SIGKILL 也不代表磁盘损坏、断电或未知回复故障注入已经完成。
