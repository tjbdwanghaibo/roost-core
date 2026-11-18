# 房间合作 PvE：塔防 / 生存撤离 / 自走棋

三条纵切同形：一局一个房间（room 模块的 `Room` 实体，已是 World 锁档）+ Player；房间挂一个持久定时器推进阶段 / 波次 / 期限；入场、领奖、兑付是 Player + 房间的多实体事务。席位、代际与连接、在线、断线接管交给 room 模块，金币走 wallet、建档走 enter，生存撤离带出的药剂按件进 bag；玩法核心（地图 / 物资 / 卡池、阶段推进、战斗）手写。

| 变体 | 手册（下称 TD / SE / AC §N） | 对应脚本 | 房间上的玩法字段 |
| --- | --- | --- | --- |
| 合作塔防 | `docs/framework/TOWER-DEFENSE-INTEGRATION.md` | `scripts/test-tower-defense-generated.sh` | `RoomDao` 末尾的 `TowerState` / `TowerWakeTimer` |
| 合作生存撤离 | `docs/framework/SURVIVAL-EXTRACTION-INTEGRATION.md` | `scripts/test-survival-extraction-generated.sh` | `SurvivalState` / `SurvivalWakeTimer` |
| 自走棋（八人） | `docs/framework/AUTO-CHESS-INTEGRATION.md` | `scripts/test-auto-chess-generated.sh` | `ChessState` / `ChessWakeTimer`，容量 8（`Room().Open(8)`） |

命令对应当前版本的脚本；目标 roost-core 版本更早时读那个 tag 里的本文件。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 回合 / 阶段 / 波次由服务器期限推进的房间玩法：共享资源（金币、卡池、资源点）、按序号的命令、断线托管、按排名或团队领奖 | 实时移动 / 射击（纵切里没有）；依赖高于 20 次 / 秒心跳的房间；物理闯关（另见 `PHYSICS-ROOM-SERVICE.md`）；在线推进靠心跳、局内记分板推给全体成员的短局派对玩法看 [party-room.md](party-room.md) |

## 2. 命令

接在 SKILL.md §3.2 公共前置命令之后，三个变体各一段。共同点：先 `modules.sh add` 装模块（Player / Room 的主 DAO 与玩家 TCP 接入由安装代办），再 `add timer --entity Room`，往 `RoomDao` 追加玩法字段，拷夹具（名单准入是 Room 上的 `event.RoomJoining` 监听），装公共 Robot，`roost generate`，跑场景，`modules.sh verify`。新命令过 `Room().Authorize(玩家, 代际, 连接)`；入场扣费、领奖与兑付是 Player + 房间多实体 Sender，“扣过 / 领过没有”记在房间的 `Joined` / `Claimed`。

### 2.1 塔防

```bash
F="$CORE/scripts/testdata/tower-defense"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" room wallet enter
"$ROOST" add timer --entity Room                                                    # 波次期限
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

### 2.2 生存撤离

药剂按件进 bag（bag 自带道具表的 1001 药水），初始 100 金币是 `event.EnterFirst` 监听。

```bash
F="$CORE/scripts/testdata/survival-extraction"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" room wallet bag enter
"$ROOST" add timer --entity Room                                                    # 探险期限
dao_add Room db/def/room.go "SurvivalState string survival_state" "SurvivalWakeTimer int64 survival_wake_timer"
mkdir -p game/modules/survival
for n in rules state match rules_test; do cp "$F/$n.go.txt" "game/modules/survival/$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/room/timer_handlers.go
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

### 2.3 自走棋

```bash
F="$CORE/scripts/testdata/auto-chess"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" room wallet enter
"$ROOST" add timer --entity Room                                                    # 阶段期限
dao_add Room db/def/room.go "ChessState string chess_state" "ChessWakeTimer int64 chess_wake_timer"
mkdir -p game/modules/autochess
for n in rules state match rules_test; do cp "$F/$n.go.txt" "game/modules/autochess/$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/room/timer_handlers.go
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

三段都按 SKILL.md §3.4 收尾。

## 3. 期望场景数

| 变体 | 期望（0 SKIP） | 场景要点（数值是样例） |
| --- | --- | --- |
| 塔防 | `TestTowerDefense` **11** 条（4 规则 + 6 Nest + 1 Robot）；`room 34/34`、`wallet 17/17`、`enter 29/29` | 并发抢格、阻路回滚、断线与旧代际 / 旧连接被拒、名单外入座被拒、回滚恢复金币 / 路线 / 定时器、团队奖励原子 |
| 生存撤离 | `TestSurvivalExtraction` **13** 条（4 规则 + 8 Nest + 1 Robot）；`bag 20/20`、`enter 29/29`、`room 34/34`、`wallet 17/17` | 入场原子幂等、资源点竞争、并发拾取、物资守恒、断线与旧代际被拒、兑付金币 + 药剂原子、过期不能重进 |
| 自走棋 | `TestAutoChess` **13** 条（12 + 1 Robot）；`enter 29/29`、`room 34/34`、`wallet 17/17` | 卡池守恒、并发买卡、准备竞争只冻结一场、断线不拖住阶段、排名奖励原子、账本满保留原回执 |

## 4. 限制

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 一局一锁的多人命令串行、期限与状态同事务、离线 / 重启后补触发一次、多实体领奖不重复、配置原子发布与按值冻结 | TD / SE / AC 的验收章节 |
| 框架不能做 | 离线追帧（加载后只触发一次，补算由业务决定）；寻路 / 阻路、怪物 AI、战斗模拟、卡池随机 | TD §6、AC §5、SE §6 |
| 时序 | 最早期限已到、Tick 还没提交时新命令一律被拒，等定时器提交后重发原命令；掉线席位不阻塞阶段推进 | TD §8、SE §6、AC §5 |
| 一致性 | 每席命令序号单调、同序号同内容返回原回执；账本 128 条（塔防 / 生存撤离每席）满了拒绝新命令、不淘汰旧回执，正式服要归档 / 分片 | TD §5、SE §5、AC §6 |
| 缺省模式不证明 | 无 WAL / Mongo；定时器经 `DeliverDue` 投递、实体一直在内存；多名“玩家”是同进程并发调用；四台 / 八个真实网络客户端、多进程与容量未测。真实依赖档 `ROOST_TOWER_DEFENSE_REAL=1` / `ROOST_SURVIVAL_EXTRACTION_REAL=1` / `ROOST_AUTO_CHESS_REAL=1`（隔离 Mongo）：SIGKILL 后冷加载、补触发一次、续局领奖 | KNOWN-LIMITS“游戏接入验收” |
| 业务要自己补 | 阻路 / 寻路、怪物波次、塔数值；采集 / 制作 / 撤离规则；卡池 / 商店 / 战斗模拟、随机种子冻结；房间生命周期、掉线政策、开局匹配接线（match / session） | 各手册 §2、§8 |

**常见坑**：

- 每张卡 / 每个塔一个实体：买卡变成跨实体事务；推荐一局一锁（AC §8）。
- 自己另建房间实体而不用 room 的 `Room`：要自己改锁档、自己做席位代际与连接围栏。
- 名单监听对普通房（玩法状态字段为空）要放行：room 自带的场景用的就是普通房，否则 `module verify` 失败。
- 连接标识从客户端 PB 里取或用玩家号代替：旧连接就能操作新席位；必须 `room.ConnectionKey(session)`。
- 重启之后直接下新命令：room 的在线状态不落库，新进程里全员断线，新命令答 `room_stale_epoch`；成员先 `RoomReconnect` 拿新代际。已有回执的重放、查回执不要求在线。
- 兑付药剂进 bag 要道具表：进程没装载 `configs/data`（或规则表另开了一个 `configdata.Store` 把它顶掉）时 `Bag().Add` 答 `bag_item_invalid`，整笔兑付回滚。
- 锁接口名不含 `Entity`：`generate` 报 `found no entity parameter`。
- 先扣款再异步算路线：建塔要在局部副本上算完 BFS，不可达就整体拒绝。
- 把实体持久创建与业务初始化混为一谈：初始资源与首次入场仍需自己的事务和幂等事实（guide/03-dataengine）。
- 离线时长按次补扣环境伤害 / 打完整局：本例都只推进一次。
