# MMORPG

手册：`docs/framework/MMORPG-INTEGRATION.md`（下称 MMO §N）。命令对应当前版本的 `scripts/test-mmorpg-generated.sh` 与 `scripts/test-mmorpg-scenes.sh`；目标 roost-core 版本更早时读那个 tag 里的本文件。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 同进程多地图的服务器权威状态同步 MMO：登录建角、移动与视野、组队、面对面交易、权威战斗与冷却、Buff / 死亡复活、掉落任务副本、背包装备、公会（Remote）、跨服赠礼（Saga）、邮件 / 支付 / 活动 / 排行 | 固定分区大世界（另见 SKILL.md §6 与 cellworld 模块）；Player 跨 sid 无缝迁移（rehome 只做断线改绑）；跨服世界 Boss + 副本权限视野 + 活动结算的联合链没有验收（MMO §6.1）；要公平的竞技副本单独走 moba-lockstep |

## 2. 命令

### 2.1 参考形态：game-demo 模板 + 多地图 TCP

`-template game-demo` 生成完整参考应用（登录、背包、邮件、匹配、聊天、副本、赠礼 saga、地图与视野、lockstep 战斗、GM、loadtest；`demo/README.md`）。把 SKILL.md §3.2 的 `project new` 那行换成 `-template game-demo` 再执行，之后：

```bash
F="$CORE/scripts/testdata/mmorpg"
for n in scene_runtime.go scene_runtime_test.go; do
  sed "s|MMO_MODULE|$MOD|g" "$F/$n.txt" > "internal/service/game/mmo_$n"
done
gofmt -w internal/service/game/mmo_*.go
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMMORPGTwoScenesTCPTransferRetryAndReconnect$' ./internal/service/game 2>&1 | tee "$WORK/test.log"
```

这两份夹具用 `MMO_MODULE` 占位，不需要 `fix_mod`。

### 2.2 业务纵切：Player / Scene / Trade / Monster

接在 SKILL.md §3.2（`-template game`）之后。模块：`wallet`（金币）、`bag`（材料道具 3001）、`task`（击杀任务 201，`Task().Report("mmo.kill")`，领奖 `Task().Claim`），handler 收 `view.PlayerEntity` 在同一笔事务里调它们。队伍、场景、怪物、战斗、副本、交易托管手写（交易借鉴 friends 的两 Player 同事务写法，MMO §4.5）。

```bash
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" wallet bag task
"$ROOST" add timer --entity Player
for e in Scene Trade Monster; do
  "$ROOST" add entity "$e"
  "$ROOST" add dao "$e" --entity "$e"
done
# dao_i64 <实体> <文件> 字段...：全部 int64、store 键取字段名小写（与脚本一致）
dao_i64() { local t=$1 f=$2 x a=(); shift 2; for x in "$@"; do a+=("$x int64 $(printf %s "$x" | tr A-Z a-z)"); done; dao_add "$t" "$f" "${a[@]}"; }
dao_i64 Player db/def/player.go PartyID SceneID SceneVersion TradeSeq TradeBuyer TradeAmount HP \
  AttackSeq LastAttackTarget CooldownUntil BuffValue BuffTimer ReviveTimer
dao_i64 Scene db/def/scene.go Occupants InstanceComplete
dao_i64 Trade db/def/trade.go Status SellerID BuyerID Price Count Deadline SellerConfirmed BuyerConfirmed
dao_i64 Monster db/def/monster.go HP SceneID LootOwner LootTaken
world game/entities/scene/entity.go game/entities/trade/entity.go game/entities/monster/entity.go
# 材料道具 3001（单种上限 1000）与击杀任务 201（事件 mmo.kill，奖 5 金币）追加在模块自带的表后面
printf '%s\n' '3001,MMO 材料,1000' >> configs/table/item.csv
printf '%s\n' 201,mmo.kill,permanent,1,0,coins,0,5,false >> configs/table/task.csv
F="$CORE/scripts/testdata/mmorpg"
for n in tasks tasks_test gameplay gameplay_test; do cp "$F/$n.go.txt" "game/handler/mmo_$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/player/timer_handlers.go
fix_mod mmorpg-practice game/handler/mmo_*.go game/entities/player/timer_handlers.go
gofmt -w db/def/*.go game/handler/mmo_*.go game/entities/player/timer_handlers.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" mmorpg
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMMORPG(Party|Trade|Combat|Buff|Transfer|Death|Robot)' ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" wallet bag task
```

两种形态都按 SKILL.md §3.4 收尾。

## 3. 期望场景数

| 形态 | 期望 |
| --- | --- |
| 2.1 参考形态 | 1 条 PASS（两张地图、真实 TCP 客户端、切图重试与重连）；game-demo 工程 `project doctor` 没有 FAIL |
| 2.2 业务纵切 | `--- PASS: TestMMORPG` 恰好 **14** 条（13 条业务 + `TestMMORPGRobotBusiness`）、0 SKIP；`modules.sh verify` 打印 `bag 20/20`、`task 29/29`、`task+bag 3/3`、`task+wallet 6/6`、`wallet 17/17` |

业务场景覆盖：两人组队 / 解散；并发转账重放与拒绝重试；Trade 托管交易（确认、取消、到期、并发竞争）；战斗 → 掉落 → 任务 → 副本完成；Buff 刷新与回滚；死亡与复活期限；切图、返回与旧请求。

## 4. 限制

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 多实体同事务（交易三实体）、持久期限、同进程切图（Nest 提交 + Interest 两阶段）、Hold / Ready 与高 epoch 全量、Profile 字段白名单、Remote 公会、Saga 跨服赠礼 | MMO §3、§5、§6 |
| 框架不能做 | Player 跨 sid 无缝迁移、副本 / 战场自动高可用、跨服撤权传播、已下发信息收回；寻路 / 巡逻组件（movement 只有 MapBasic / Move / MoveTo / Follow）；Skill Runtime 随 Nest 回滚 | MMO §6、§7；guide/10-time.md §4B；guide/08-skill.md |
| 视野容量 | 本机 loopback 1000 玩家、1 万实体、20Hz：单视野改值到客户端 P99 约 64ms，+16 视野 97～141ms（ApplyAsync）；缺省 ApplySync 每玩家 ≤ 9 视野；`MaxObjects` 缺省 100，超出时超额降级 | guide/04-sync.md §2“已测规模与边界”；KNOWN-LIMITS“广播与 AOI” |
| 心跳 | 最短 50ms，移动建议 200ms | guide/10-time.md §4A |
| 一致性 | 提交确认不等于 Mongo 已可见（要用 Flush / 投影等待）；连接关闭或超时不等于撤销；视野可见不等于攻击 / 交易授权；Saga 不是跨服瞬时原子交易，业务键只在运行期间 + 终态保留期内唯一，“同一礼物永远只送一次”要在业务数据里记已用过的键 | MMO §3、§7；guide/06-saga.md |
| 业务要自己补 | 战斗公式、射程、视线、反作弊；组队 / 交易 / 掉落规则；技能 Host 副作用与持久边界；寻路；副本准入与容量；公会命名与权限；切图失败的重试驱动 | MMO §3 |
| 缺省模式不证明 | 无 WAL / Mongo。真实依赖档 `ROOST_MMORPG_REAL=1`（隔离 Mongo）：正常停机与 SIGKILL 后经投递循环恢复；`scripts/test-mmorpg-cross-sid.sh` 双服赠礼（Mongo + NATS）；game-demo `make dev-up && make dev-run && make dev-smoke` | MMO §11、§13 |

玩家读取公会加成、资料这类允许快照语义的功能，用 [Remote 指南 §6](../../../framework/guide/05-remote-mirror.md#6-普通-entity-写入--remote-view-读取) 的“普通 Player + Remote View”链路；视图是预取时的快照，不能授权扣公会资源、审批成员，handler 要校验当前归属与引用一致。

**常见坑**（MMO §7）：

- 改了 SceneID 就当切图完成：持久事实、兴趣订阅、客户端基线都要更新。
- 两个独立 Manager 的流拼到同一个客户端 Receiver；应用 Full 时不清旧对象。
- 收到一次确认就交换资产：两个确认要绑定同一报价身份。
- 照搬 game-demo 的 Scene 注解：demo 地形 Scene 是 `noPersist=true lifetime=runtime_rebuild`，纵切的 Scene 带人数事实要持久。
- 有 Lockstep 就以为 MMO 必须帧同步：开放世界用服务器权威状态同步。
- 真实 TCP 场景通过就说容量合格：功能与容量分开验收。
