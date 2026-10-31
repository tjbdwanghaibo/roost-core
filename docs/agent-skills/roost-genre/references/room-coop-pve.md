# 房间合作 PvE：塔防 / 生存撤离 / 自走棋

三条纵切同形：一局一个房间实体（World 锁档）+ Player；房间实体挂一个持久定时器推进阶段 / 波次 / 期限；入场、领奖、兑付是 Player + 房间的多实体事务。手册：

| 变体 | 手册（下称 TD / SE / AC §N） | 房间实体 |
| --- | --- | --- |
| 合作塔防 | `docs/framework/TOWER-DEFENSE-INTEGRATION.md` | TowerRoom |
| 合作生存撤离 | `docs/framework/SURVIVAL-EXTRACTION-INTEGRATION.md` | SurvivalRaid |
| 自走棋（八人） | `docs/framework/AUTO-CHESS-INTEGRATION.md` | ChessMatch |

路径相对 roost-core 根目录，以检出或 module cache 中的 `docs/` 为准。

> **版本**：三个变体 v1.33.0 起可用。v1.33.2 起 handler 的业务时间改为约定参数 `nowUnixMilli`（分发包装从 fctx 填），**生成的 Sender 不带时间参数**，测试用 `clock.SetSource` 拨时钟；v1.33.1 及更早的手册 Sender 示例还带 `now`（SKILL.md §0）。二档 REAL 恢复用例需 ≥ v1.33.2（SKILL.md §7）。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 回合 / 阶段 / 波次由服务器期限推进的房间玩法：共享资源（金币、卡池、资源点）、按序号的命令、断线托管、按排名或团队领奖 | 实时移动 / 射击（纵切里没有，SE 的实时射击不在范围）；依赖 >20Hz 实体心跳的房间（该调度器最短 50ms，不是 Sync 的发送上限，guide/10-time.md §4A）；物理闯关（另见 PHYSICS-ROOM-SERVICE.md，需 PhysX native）；在线推进靠心跳（≤20Hz）、局内记分板推给全体成员的短局派对玩法看 [party-room.md](party-room.md) |

## 2. 用到的框架模块

| 业务 | 模块 | 出处 |
| --- | --- | --- |
| 房间权威状态（地图、波次、卡池、物资账） | 房间实体 DAO（`State` 业务 JSON + `WakeTimer`），`EntityCategoryWorld` | TD / SE / AC §2、§3.1 |
| 阶段 / 波次 / 撤离读条期限 | `roost add timer --entity <Room>`，一个节点取最早期限，每次推进替换 | TD §6、SE §6、AC §5 |
| 入场扣费、团队 / 排名领奖、兑付 | Player + 房间多实体 Sender，`rollback=undo durability=strict` | TD §7、SE §5、AC §6 |
| 配置冻结 | `configdata.MustRegisterObject`，开局按值捕获 `Rules` 写进房间 | TD §4、SE §4、AC §4 |
| 重连视图 | 锁内只读 Sender 取裁剪后的视图；推送按 self_sync 写业务视图 | TD §7、AC §7 |

## 3. 生成最小 demo

接在 SKILL.md §3.2 公共前置命令之后。先按变体设下面 6 个变量（对应 `scripts/test-<变体>-generated.sh`），再执行统一命令块。

| 变量 | 塔防 | 生存撤离 | 自走棋 |
| --- | --- | --- | --- |
| `E`（实体） | `TowerRoom` | `SurvivalRaid` | `ChessMatch` |
| `E_FILE`（snake） | `tower_room` | `survival_raid` | `chess_match` |
| `PKG`（业务包目录） | `towerdefense` | `survival` | `autochess` |
| `FIX`（夹具目录） | `tower-defense` | `survival-extraction` | `auto-chess` |
| `H`（handler 文件名） | `tower_defense` | `survival_extraction` | `auto_chess` |
| `TP`（测试前缀） | `TestTowerDefense` | `TestSurvivalExtraction` | `TestAutoChess` |
| 期望 PASS 数 | 10 | 12 | 12 |

```bash
E=TowerRoom E_FILE=tower_room PKG=towerdefense FIX=tower-defense H=tower_defense TP=TestTowerDefense   # 换变体改这一行
"$ROOST" add dao Player --entity Player
"$ROOST" add entity "$E"
"$ROOST" add dao "$E" --entity "$E"
"$ROOST" add timer --entity "$E"
dao_add Player db/def/player.go "Gold int64 gold" "Initialized bool initialized"
dao_add "$E" "db/def/$E_FILE.go" "State string state" "WakeTimer int64 wake_timer"
world "game/entities/$E_FILE/entity.go"
F="$CORE/scripts/testdata/$FIX"
mkdir -p "game/$PKG"
for n in rules state match rules_test; do cp "$F/$n.go.txt" "game/$PKG/$n.go"; done
cp "$F/timer_handlers.go.txt" "game/entities/$E_FILE/timer_handlers.go"
cp "$F/handlers.go.txt"       "game/handler/$H.go"
cp "$F/gameplay_test.go.txt"  "game/handler/${H}_test.go"
fix_mod "$FIX-practice" game/$PKG/*.go "game/entities/$E_FILE/timer_handlers.go" game/handler/$H*.go
gofmt -w db/def/*.go game/$PKG/*.go game/entities/$E_FILE/*.go game/handler/$H*.go
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run "^$TP" "./game/$PKG" ./game/handler 2>&1 | tee "$WORK/test.log"
```

期望：`--- PASS: $TP` 条数等于上表、0 skip；然后按 SKILL.md §3.4 收尾。三个变体的 Player 与房间 DAO 字段相同（`Gold` / `Initialized`、`State` / `WakeTimer`）。

## 4. demo 的最小玩法范围

| 变体 | 夹具里的玩法（数值是样例） | 场景要点 |
| --- | --- | --- |
| 塔防（10 = 4 规则 + 6 Nest） | 5×5 地图与阻路 BFS、共享金币建 / 升 / 卖塔、准备与托管、三波战斗、基地胜负、团队奖励 | 并发抢格、阻路回滚、旧连接被拒、undo 恢复金币 / 路线 / 定时器、团队奖励原子（TD §3.4） |
| 生存撤离（12 = 4 + 8） | 付费入场、共享采集、制作预扣、环境伤害、死亡掉落、撤离读条、永久兑付 | 入场原子幂等、资源点竞争、并发拾取、物资守恒、过期不能重进（SE §3.4） |
| 自走棋（12） | 八席共享卡池与商店、经济与升级、合星、准备 / 战斗阶段、确定性战斗、淘汰排名、断线接管、按排名领奖 | 卡池守恒、并发买卡、准备竞争只冻结一场、期限后输入被拒、账本满保留原回执（AC §3.4） |

## 5. 业务限制清单

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 一局一锁的多人命令串行、期限与状态同事务、离线 / 重启后补触发一次、多实体领奖不重复、配置原子发布与按值冻结 | TD / SE / AC §11（AC §10）“证明” |
| 框架不能做 | 离线追帧（加载后只触发一次，不会一次打完整局，补算由业务决定）；寻路 / 阻路、怪物 AI、战斗模拟、卡池随机；接管票与会话票的分配（要可信入口持久分配） | TD §6、AC §5、SE §6 |
| 时序边界 | 最早期限已到、Tick 还没提交时新命令一律被拒，等定时器提交后重发原命令；掉线席位不阻塞阶段推进 | TD §8、SE §6、AC §5 |
| 容量 / 验证边界 | 四台 / 八个真实网络客户端、多进程与容量均未测；生存撤离“恢复路径上的探险结束处理”未覆盖；自走棋跨 sid 发奖未测 | KNOWN-LIMITS“游戏接入验收”三行 |
| 一致性边界 | 每席命令序号单调、同序号同内容返回原回执；账本 128 条（塔防 / 生存撤离每席）满了拒绝新命令、不淘汰旧回执，正式服要归档 / 分片 | TD §5、SE §5、AC §6 |
| 业务要自己补 | 阻路 / 寻路、怪物波次、塔数值；采集 / 制作 / 撤离规则；卡池 / 商店 / 战斗模拟、随机种子冻结；房间生命周期、掉线政策、开局匹配接线（match / session） | 各手册 §2、§8 |

## 6. 常见坑

- 每张卡 / 每个塔一个实体：买卡变成跨实体事务，锁序和死锁要自己处理；推荐一局一锁（AC §8）。
- 房间实体忘了改 World 锁档：领奖同时锁 Player 与房间时锁序不固定（TD §8）。
- 锁接口名不含 `Entity`：`generate` 报 `found no entity parameter`（TD / SE / AC §8～§9）。
- 先扣款再异步算路线：建塔要在局部副本上算完 BFS，入口到基地不可达就整体拒绝（TD §5）。
- 把实体持久创建与业务初始化混为一谈：启用 DataEngine 后普通持久实体事务外 `Create` 会补 strict 提交，Nest 内随事务提交；无 DataEngine 的离线夹具只建内存。初始资源与首次入场仍需自己的事务和幂等事实，Remote 例外见 guide/03-dataengine（SE §9）。
- 离线时长按次补扣环境伤害 / 打完整局：本例都只推进一次，结束时刻永不延长（SE §6、AC §5）。

## 7. 验证

- 一档：§3 命令 + SKILL.md §3.4。2026-10-31 在 v1.33.0 检出、自定 module 实测：塔防 10 / 生存撤离 12 / 自走棋 12 条全部 PASS，build / vet / `id check` / `generate --check` 通过。2026-10-31 在 v1.33.2 检出（`e4ec6260`）按本节统一命令块换变体行复测：10 / 12 / 12 PASS、0 SKIP，§3.4 全过，doctor 仅 4 项 collaborators FAIL。
- 二档：`ROOST_TOWER_DEFENSE_REAL=1` / `ROOST_SURVIVAL_EXTRACTION_REAL=1` / `ROOST_AUTO_CHESS_REAL=1`（隔离 Mongo 副本集）：子进程在 strict 已确认、WAL 未投影时被 SIGKILL，新进程冷加载、驱动投递循环补触发一次、续局领奖，第三个进程核对并重试（各手册“真实 WAL / Mongo 崩溃恢复”节）。
- 缺省模式不证明：无 WAL / Mongo；定时器经 `DeliverDue` 投递、实体一直在内存，没有经过加载后补触发与投递循环计时；多名“玩家”是同进程并发调用。

## 8. 手册指针

- 三篇手册的 §2 状态归谁、§3 生成与阅读顺序、§5～§6 命令身份与期限、§8～§9 接入坑、第二部分验收
- `docs/framework/guide/10-time.md` §4（实体定时器）、§4A（心跳，实时房间才需要，用法见 [party-room.md](party-room.md)）
- `docs/framework/guide/09-services.md`（match / session 成局与运行会话，夹具里没有接线）
- `docs/maintenance/KNOWN-LIMITS.md`“游戏接入验收”塔防 / 生存撤离 / 自走棋三行
