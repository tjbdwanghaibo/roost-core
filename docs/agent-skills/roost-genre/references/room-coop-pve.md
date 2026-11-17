# 房间合作 PvE：塔防 / 生存撤离 / 自走棋

三条纵切同形：一局一个房间（room 模块的 `Room` 实体，World 锁档）+ Player；房间挂一个持久定时器推进阶段 / 波次 / 期限；入场、领奖、兑付是 Player + 房间的多实体事务。R9（2026-11-17）起三条都按“装模块 + 只写模块没覆盖的部分”改造：房间并入 room 模块的 `Room`（席位、代际与连接、在线、断线接管），金币走 wallet、建档走 enter，生存撤离带出的药剂按件进 bag；玩法核心（地图 / 物资 / 卡池、阶段推进、战斗）仍手写。手册：

| 变体 | 手册（下称 TD / SE / AC §N） | 房间实体 |
| --- | --- | --- |
| 合作塔防 | `docs/framework/TOWER-DEFENSE-INTEGRATION.md` | room 模块的 `Room`（塔防状态是 `RoomDao` 末尾的 `TowerState` / `TowerWakeTimer`） |
| 合作生存撤离 | `docs/framework/SURVIVAL-EXTRACTION-INTEGRATION.md` | room 模块的 `Room`（`SurvivalState` / `SurvivalWakeTimer`） |
| 自走棋（八人） | `docs/framework/AUTO-CHESS-INTEGRATION.md` | room 模块的 `Room`，容量 8（`ChessState` / `ChessWakeTimer`） |

路径相对 roost-core 根目录，以检出或 module cache 中的 `docs/` 为准。

> **版本**：三个变体 v1.33.0 起可用。v1.33.2 起 handler 的业务时间改为约定参数 `nowUnixMilli`（分发包装从 fctx 填），**生成的 Sender 不带时间参数**，测试用 `clock.SetSource` 拨时钟；v1.33.1 及更早的手册 Sender 示例还带 `now`（SKILL.md §0）。二档 REAL 恢复用例需 ≥ v1.33.2（SKILL.md §7）。

> 本篇命令按当前检出的脚本写（v1.40.0 起 DAO 字段写 `store` 键，v1.35.0 起在最终 `roost generate` 前安装公共 Robot）；目标是更早的 tag 时读该 tag 检出里的本文件（SKILL.md §0）。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 回合 / 阶段 / 波次由服务器期限推进的房间玩法：共享资源（金币、卡池、资源点）、按序号的命令、断线托管、按排名或团队领奖 | 实时移动 / 射击（纵切里没有，SE 的实时射击不在范围）；依赖 >20Hz 实体心跳的房间（该调度器最短 50ms，不是 Sync 的发送上限，guide/10-time.md §4A）；物理闯关（另见 PHYSICS-ROOM-SERVICE.md，需 PhysX native）；在线推进靠心跳（≤20Hz）、局内记分板推给全体成员的短局派对玩法看 [party-room.md](party-room.md) |

## 2. 用到的框架模块

| 业务 | 模块 | 出处 |
| --- | --- | --- |
| 房间权威状态（地图、波次、卡池、物资账） | room 模块 `RoomDao` 上追加的玩法字段（业务 JSON + 定时器节点号：`TowerState` / `SurvivalState` / `ChessState` 与对应 `…WakeTimer`），`Room` 已是 `EntityCategoryWorld` | TD / SE / AC §2、§3.1 |
| 席位、代际与连接、在线、断线接管 | **room 模块**：`RoomJoin` / `RoomReconnect` / 断线订阅；名单准入是 Room 上的 `event.RoomJoining` 监听；新命令过 `Room().Authorize(玩家, 代际, 连接)`，已有回执的重放 / 查回执过 `Member` + 连接核对（不要求在线） | TD / SE / AC §2、§5、§7 |
| 玩家金币、建档 | **wallet**（入场费 `Wallet().Add(-费)`、领奖 / 兑付 `Wallet().Add`，“扣过 / 领过没有”记在房间的 `Joined` / `Claimed`）、**enter**（`EnterInit`，入场 / 领奖查 `Enter().Built()`；撤离的初始 100 金币是 `event.EnterFirst` 监听） | TD §7、SE §5、§8、AC §6 |
| 永久道具（生存撤离） | **bag**：封存清单里的药剂按件 `Bag().Add(1001 药水, n)`，与金币、`Claimed` 同一笔；道具表是 bag 自带的 `configs/table/item.csv` | SE §8 |
| 阶段 / 波次 / 撤离读条期限 | `roost add timer --entity <Room>`，一个节点取最早期限，每次推进替换 | TD §6、SE §6、AC §5 |
| 入场扣费、团队 / 排名领奖、兑付 | Player + 房间多实体 Sender，`rollback=undo durability=strict` | TD §7、SE §5、AC §6 |
| 配置冻结 | `configdata.MustRegisterObject`，开局按值捕获 `Rules` 写进房间 | TD §4、SE §4、AC §4 |
| 重连视图 | 锁内只读 Sender 取裁剪后的视图；推送按 self_sync 写业务视图 | TD §7、AC §7 |

## 3. 生成最小 demo

接在 SKILL.md §3.2 公共前置命令之后，三个变体各一段（§3.1～§3.3），对应 `scripts/test-<变体>-generated.sh`。共同点：先 `modules.sh add` 装模块（= `roost module add` + 核对已登记；Player / Room 的主 DAO 与玩家 TCP 接入由安装计划代办），再 `add timer --entity Room`，往 `RoomDao` 追加玩法字段，拷夹具，装公共 Robot，`roost generate`；不再 `add dao Player` / `add entity <房间>`，不给 Player 加 `Gold` / `Initialized`，不改锁档。最后 `modules.sh verify` 核对已装模块自带的场景（每个模块通过数 == 检出 `modules/<名>/module.yaml` 的 `verify.pass`、0 skip，并带 build / vet / `id check` / `generate --check`）。

### 3.1 塔防（装模块，R9 起）

模块清单 `room wallet enter`（Q6 只装 enter，不装 login / portal）；不再 `add entity TowerRoom`，不给 Player 加 `Gold` / `Initialized`，不改锁档（room 的 `Room` 已是 World）。对应 `scripts/test-tower-defense-generated.sh`：

```bash
F="$CORE/scripts/testdata/tower-defense"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" room wallet enter   # = roost module add + 核对已登记
"$ROOST" add timer --entity Room                                                    # 波次期限：Room 上的实体定时器
dao_add Room db/def/room.go "TowerState string tower_state" "TowerWakeTimer int64 tower_wake_timer"
mkdir -p game/modules/towerdefense
for n in rules state match rules_test; do cp "$F/$n.go.txt" "game/modules/towerdefense/$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/room/timer_handlers.go   # 覆盖 add timer 的示例
cp "$F/room_roster.go.txt"    game/entities/room/tower.go            # RoomJoining 名单监听
cp "$F/handlers.go.txt"       game/handler/tower_defense.go
cp "$F/gameplay_test.go.txt"  game/handler/tower_defense_test.go
fix_mod tower-defense-practice game/modules/towerdefense/*.go game/entities/room/{timer_handlers,tower}.go game/handler/tower_defense*.go
gofmt -w db/def/room.go game/modules/towerdefense/*.go game/entities/room/*.go game/handler/tower_defense*.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" tower-defense
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestTowerDefense' ./game/modules/towerdefense ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" room wallet enter
```

期望：`TestTowerDefense` 11 条 PASS、0 skip；`modules.sh verify` 打印 `room 34/34`、`wallet 17/17`、`enter 29/29`（每个模块通过数 == 检出 `modules/<名>/module.yaml` 的 `verify.pass`、0 skip，并带 build / vet / `id check` / `generate --check`）。

### 3.2 生存撤离（装模块，R9 起）

模块清单 `room wallet bag enter`：入场费与材料兑的金币走 wallet，封存清单里的药剂按件进 bag（bag 自带道具表的 1001 药水），初始 100 金币是 `event.EnterFirst` 监听（`first_enter.go.txt`）。对应 `scripts/test-survival-extraction-generated.sh`：

```bash
F="$CORE/scripts/testdata/survival-extraction"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" room wallet bag enter
"$ROOST" add timer --entity Room                                                    # 探险期限：Room 上的实体定时器
dao_add Room db/def/room.go "SurvivalState string survival_state" "SurvivalWakeTimer int64 survival_wake_timer"
mkdir -p game/modules/survival
for n in rules state match rules_test; do cp "$F/$n.go.txt" "game/modules/survival/$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/room/timer_handlers.go   # 覆盖 add timer 的示例
cp "$F/room_roster.go.txt"    game/entities/room/survival.go         # RoomJoining 名单监听
cp "$F/first_enter.go.txt"    game/entities/player/survival.go       # EnterFirst 监听：初始 100 金币
cp "$F/handlers.go.txt"       game/handler/survival_extraction.go
cp "$F/gameplay_test.go.txt"  game/handler/survival_extraction_test.go
fix_mod survival-extraction-practice game/modules/survival/*.go game/entities/room/{timer_handlers,survival}.go game/entities/player/survival.go game/handler/survival_extraction*.go
gofmt -w db/def/room.go game/modules/survival/*.go game/entities/room/*.go game/entities/player/survival.go game/handler/survival_extraction*.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" survival-extraction
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestSurvivalExtraction' ./game/modules/survival ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" room wallet bag enter
```

期望：`TestSurvivalExtraction` 13 条 PASS、0 skip；`modules.sh verify` 打印 `bag 20/20`、`enter 29/29`、`room 34/34`、`wallet 17/17`。

### 3.3 自走棋（装模块，R9 起）

模块清单 `room wallet enter`；对局房容量 8（`Room().Open(8)`）。对应 `scripts/test-auto-chess-generated.sh`：

```bash
F="$CORE/scripts/testdata/auto-chess"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" room wallet enter
"$ROOST" add timer --entity Room                                                    # 阶段期限：Room 上的实体定时器
dao_add Room db/def/room.go "ChessState string chess_state" "ChessWakeTimer int64 chess_wake_timer"
mkdir -p game/modules/autochess
for n in rules state match rules_test; do cp "$F/$n.go.txt" "game/modules/autochess/$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/room/timer_handlers.go   # 覆盖 add timer 的示例
cp "$F/room_roster.go.txt"    game/entities/room/chess.go            # RoomJoining 名单监听
cp "$F/handlers.go.txt"       game/handler/auto_chess.go
cp "$F/gameplay_test.go.txt"  game/handler/auto_chess_test.go
fix_mod auto-chess-practice game/modules/autochess/*.go game/entities/room/{timer_handlers,chess}.go game/handler/auto_chess*.go
gofmt -w db/def/room.go game/modules/autochess/*.go game/entities/room/*.go game/handler/auto_chess*.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" auto-chess
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestAutoChess' ./game/modules/autochess ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" room wallet enter
```

期望：`TestAutoChess` 13 条 PASS、0 skip；`modules.sh verify` 打印 `enter 29/29`、`room 34/34`、`wallet 17/17`。

## 4. demo 的最小玩法范围

| 变体 | 夹具里的玩法（数值是样例） | 场景要点 |
| --- | --- | --- |
| 塔防（11 = 4 规则 + 6 Nest + 1 Robot；另有模块自带 room 34 / wallet 17 / enter 29） | 5×5 地图与阻路 BFS、共享金币建 / 升 / 卖塔、准备与托管、三波战斗、基地胜负、团队奖励（手写）；席位 / 重连（room）、金币（wallet）、建档（enter） | 并发抢格、阻路回滚、room 断线与旧代际 / 旧连接被拒、名单外入座被拒、undo 恢复金币 / 路线 / 定时器、团队奖励经 wallet 原子（TD §3.4；旧断言 → 新断言见 TD §3.5） |
| 生存撤离（13 = 4 规则 + 8 Nest + 1 Robot；另有模块自带 room 34 / wallet 17 / bag 20 / enter 29） | 共享采集、制作预扣、环境伤害、死亡掉落、撤离读条、封存清单（手写）；付费入场与材料兑金币（wallet）、药剂入包（bag）、席位 / 重连（room）、建档与初始金币（enter） | 入场原子幂等且旧连接不能替席位入场、资源点竞争、并发拾取、物资守恒、room 断线与旧代际 / 旧连接被拒、名单外入座被拒、兑付金币 + 药剂经 wallet / bag 原子、过期不能重进（SE §3.4；旧断言 → 新断言见 SE §3.5） |
| 自走棋（13 = 12 + 1 Robot；另有模块自带 room 34 / wallet 17 / enter 29） | 八席共享卡池与商店、经济与升级、合星、准备 / 战斗阶段、确定性战斗、淘汰排名、按排名领奖（手写）；席位 / 断线接管（room）、金币（wallet）、建档（enter） | 卡池守恒、并发买卡、准备竞争只冻结一场、room 断线不拖住阶段、旧代际 / 旧连接被拒、名单外入座被拒、排名奖励经 wallet 原子、账本满保留原回执（AC §3.4；旧断言 → 新断言见 AC §3.5） |

## 5. 业务限制清单

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 一局一锁的多人命令串行、期限与状态同事务、离线 / 重启后补触发一次、多实体领奖不重复、配置原子发布与按值冻结 | TD / SE / AC §11（AC §10）“证明” |
| 框架不能做 | 离线追帧（加载后只触发一次，不会一次打完整局，补算由业务决定）；寻路 / 阻路、怪物 AI、战斗模拟、卡池随机；接管票与会话票的分配（三个变体都改用 room 的席位代际 + 服务端连接标识，不再有业务自己的票） | TD §6、AC §5、SE §6 |
| 时序边界 | 最早期限已到、Tick 还没提交时新命令一律被拒，等定时器提交后重发原命令；掉线席位不阻塞阶段推进 | TD §8、SE §6、AC §5 |
| 容量 / 验证边界 | 四台 / 八个真实网络客户端、多进程与容量均未测；生存撤离“恢复路径上的探险结束处理”未覆盖；自走棋跨 sid 发奖未测 | KNOWN-LIMITS“游戏接入验收”三行 |
| 一致性边界 | 每席命令序号单调、同序号同内容返回原回执；账本 128 条（塔防 / 生存撤离每席）满了拒绝新命令、不淘汰旧回执，正式服要归档 / 分片 | TD §5、SE §5、AC §6 |
| 业务要自己补 | 阻路 / 寻路、怪物波次、塔数值；采集 / 制作 / 撤离规则；卡池 / 商店 / 战斗模拟、随机种子冻结；房间生命周期、掉线政策、开局匹配接线（match / session） | 各手册 §2、§8 |

## 6. 常见坑

- 每张卡 / 每个塔一个实体：买卡变成跨实体事务，锁序和死锁要自己处理；推荐一局一锁（AC §8）。
- 自己另建房间实体而不用 room 的 `Room`：要自己改 World 锁档、自己做席位代际与连接围栏；三个变体都用 room 的 `Room`（已是 World）。
- 名单监听对普通房（玩法状态字段为空）要放行：room 模块自带的 34 条场景用的就是普通房，`module verify` 会失败（TD §8）。
- 连接标识从客户端 PB 里取或用玩家号代替：旧连接就能操作新席位；必须 `room.ConnectionKey(session)`（TD §8）。
- 重启之后直接下新命令：room 的在线状态不落库，新进程里全员断线，新命令（`Authorize`）答 `room_stale_epoch`；成员先换连接 `RoomReconnect` 拿新代际。已有回执的重放、查回执与已入场的重试不要求在线（SE §12 的第二进程就是这样续局）。
- 撤离兑付的药剂进 bag 要道具表：进程没装载 `configs/data`（或规则表另开了一个 `configdata.Store` 把它顶掉）时 `Bag().Add` 答 `bag_item_invalid`，整笔兑付回滚（SE §4、§8）。
- 锁接口名不含 `Entity`：`generate` 报 `found no entity parameter`（TD / SE / AC §8～§9）。
- 先扣款再异步算路线：建塔要在局部副本上算完 BFS，入口到基地不可达就整体拒绝（TD §5）。
- 把实体持久创建与业务初始化混为一谈：启用 DataEngine 后普通持久实体事务外 `Create` 会补 strict 提交，Nest 内随事务提交；无 DataEngine 的离线夹具只建内存。初始资源与首次入场仍需自己的事务和幂等事实，Remote 例外见 guide/03-dataengine（SE §9）。
- 离线时长按次补扣环境伤害 / 打完整局：本例都只推进一次，结束时刻永不延长（SE §6、AC §5）。

## 7. 验证

- 一档：§3 命令 + SKILL.md §3.4。2026-10-31 在 v1.33.0 检出、自定 module 实测：塔防 10 / 生存撤离 12 / 自走棋 12 条全部 PASS，build / vet / `id check` / `generate --check` 通过。2026-10-31 在 v1.33.2 检出（`e4ec6260`）按本节统一命令块换变体行复测：10 / 12 / 12 PASS、0 SKIP，§3.4 全过，doctor 仅 4 项 collaborators FAIL。
- 一档（塔防，R9 后）：2026-11-17 在 main 检出跑 `scripts/test-tower-defense-generated.sh`：11 条 PASS、0 SKIP，`module verify` room 33 / wallet 17 / enter 29；`test-genres-generated.py --genres tower-defense` 自定 module 名 passed（[R9 批 0 证据](../../../maintenance/evidence/r9-batch0-20261117/README.md)）。
- 一档（生存撤离、自走棋，R9 后）：2026-11-17 在 main 检出跑两个脚本：各 13 条 PASS、0 SKIP，`module verify` 撤离 room 34 / wallet 17 / bag 20 / enter 29、自走棋 room 34 / wallet 17 / enter 29；`test-genres-generated.py --genres survival-extraction auto-chess` 自定 module 名 passed；§3.2、§3.3 命令块接 SKILL.md §3.2、自定 module 名实跑通过（[R9 批 1D 证据](../../../maintenance/evidence/r9-1d-20261117/README.md)）。
- 二档：`ROOST_TOWER_DEFENSE_REAL=1` / `ROOST_SURVIVAL_EXTRACTION_REAL=1` / `ROOST_AUTO_CHESS_REAL=1`（隔离 Mongo 副本集）：子进程在 strict 已确认、WAL 未投影时被 SIGKILL，新进程冷加载、驱动投递循环补触发一次、续局领奖，第三个进程核对并重试（各手册“真实 WAL / Mongo 崩溃恢复”节）。
- 缺省模式不证明：无 WAL / Mongo；定时器经 `DeliverDue` 投递、实体一直在内存，没有经过加载后补触发与投递循环计时；多名“玩家”是同进程并发调用。

## 8. 手册指针

- 三篇手册的 §2 状态归谁、§3 生成与阅读顺序、§5～§6 命令身份与期限、§8～§9 接入坑、第二部分验收
- `docs/framework/guide/10-time.md` §4（实体定时器）、§4A（心跳，实时房间才需要，用法见 [party-room.md](party-room.md)）
- `docs/framework/guide/09-services.md`（match / session 成局与运行会话，夹具里没有接线）
- `docs/maintenance/KNOWN-LIMITS.md`“游戏接入验收”塔防 / 生存撤离 / 自走棋三行
