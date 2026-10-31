# MMORPG

手册：`docs/framework/MMORPG-INTEGRATION.md`（下称 MMO §N）。路径相对 roost-core 根目录，以检出或 module cache 中的 `docs/` 为准。

> **版本要求**：§3.2 的业务纵切夹具 **v1.33.1 起可用（2026-10-31 实测 13 条 PASS），最低版本 v1.33.1**。v1.33.0 上 `mmo_gameplay_test.go` 仍以 3 参数调用生成的 `Sync_TickPlayerTimer`，编译失败。目标版本是 v1.33.0 时只做 §3.1（game-demo 参考形态）并告诉用户纵切要升级，不要自行改夹具绕过。
>
> v1.33.2 起夹具 handler 的业务时间改为约定参数 `nowUnixMilli`（分发包装从 fctx 填），**生成的 Sender 不带时间参数**，测试用 `clock.SetSource` 拨时钟；v1.33.1 上的旧夹具 / 旧手册 Sender 示例还带 `now`（SKILL.md §0）。二档 REAL 恢复用例需 ≥ v1.33.2（SKILL.md §7）。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 同进程多地图的服务器权威状态同步 MMO：登录建角、移动与 AOI、组队、面对面交易、权威战斗与冷却、Buff / 死亡复活、掉落任务副本、背包装备、公会（Remote）、跨 sid 赠礼（Saga）、邮件 / 支付 / 活动 / 排行 | 无缝大世界多进程分区、Player 动态跨 sid 迁移（MMO §6，需框架先补）；跨服世界 Boss + 副本权限视野 + 活动结算的联合消费者尚未验收（MMO §6.1）；竞技副本要帧同步时单独走 moba-lockstep |

## 2. 用到的框架模块（MMO §2、§3）

| 业务 | 模块 |
| --- | --- |
| 角色资产、血量、任务、地图归属 | Player DAO + 组件；Nest 串行 |
| 交易托管、怪物归属、副本事实 | 独立 Trade / Monster / Scene 实体（World 锁档）+ 多实体 Sender |
| Buff 到期、复活 | `roost add timer --entity Player` |
| 谁能看到谁 | 每张地图一个 `policy.Interest`（Block AOI、关系来源、Profile 白名单），一个连接一个 `entitysync.Manager` |
| 怪物 / 回复 / 场景帧 | `roost add heartbeat` / `add behavior --ai --state` / `add timedomain`（guide/10-time.md §4A～§4B；纵切夹具未用） |
| 技能 | `gameplay/skill`（Runtime 不进 Nest 回滚域，guide/08-skill.md） |
| 公会 / 跨服 | Remote Entity + Mirror；Saga 原生步骤 / effect |

## 3. 生成最小 demo

### 3.1 参考形态：game-demo 模板 + 多地图 TCP（v1.33.0 起可用）

`-template game-demo` 生成完整参考应用（登录、背包、邮件、匹配、聊天、副本、赠礼 saga、地图与 AOI、lockstep 战斗、GM、loadtest；`demo/README.md`）。把 SKILL.md §3.2 的 `project new` 那行换成 `-template game-demo` 再执行，之后：

```bash
F="$CORE/scripts/testdata/mmorpg"
for n in scene_runtime.go scene_runtime_test.go; do
  sed "s|MMO_MODULE|$MOD|g" "$F/$n.txt" > "internal/service/game/mmo_$n"
done
gofmt -w internal/service/game/mmo_*.go
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 3m -v -run '^TestMMORPGTwoScenesTCPTransferRetryAndReconnect$' ./internal/service/game 2>&1 | tee "$WORK/test.log"
```

期望 1 条 PASS（两张地图、真实 TCP 客户端、切图重试与重连，MMO §12）。这两份夹具用 `MMO_MODULE` 占位，不需要 `fix_mod`。对应 `scripts/test-mmorpg-scenes.sh`（该脚本不跑 `generate`，模板已生成齐；多跑一次只做核对）。

### 3.2 业务纵切：Player / Scene / Trade / Monster（v1.33.1 起可用）

接在 SKILL.md §3.2（`-template game`）之后。对应 `scripts/test-mmorpg-generated.sh`。

```bash
"$ROOST" add dao Player --entity Player
"$ROOST" add timer --entity Player
for e in Scene Trade Monster; do
  "$ROOST" add entity "$e"
  "$ROOST" add dao "$e" --entity "$e"
done
# dao_i64 <实体> <文件> 字段...：全部 int64、bson 键取字段名小写（与脚本一致）
dao_i64() { local t=$1 f=$2 x a=(); shift 2; for x in "$@"; do a+=("$x int64 $(printf %s "$x" | tr A-Z a-z)"); done; dao_add "$t" "$f" "${a[@]}"; }
dao_i64 Player db/def/player.go Gold PartyID SceneID SceneVersion TradeSeq TradeBuyer TradeAmount HP Items \
  AttackSeq LastAttackTarget CooldownUntil BuffValue BuffTimer ReviveTimer QuestKills QuestClaimed
dao_i64 Scene db/def/scene.go Occupants InstanceComplete
dao_i64 Trade db/def/trade.go Status SellerID BuyerID Price Count Deadline SellerConfirmed BuyerConfirmed
dao_i64 Monster db/def/monster.go HP SceneID LootOwner LootTaken
world game/entities/scene/entity.go game/entities/trade/entity.go game/entities/monster/entity.go
F="$CORE/scripts/testdata/mmorpg"
for n in tasks tasks_test gameplay gameplay_test; do cp "$F/$n.go.txt" "game/handler/mmo_$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/player/timer_handlers.go
fix_mod mmorpg-practice game/handler/mmo_*.go game/entities/player/timer_handlers.go
gofmt -w db/def/*.go game/handler/mmo_*.go game/entities/player/timer_handlers.go
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMMORPG(Party|Trade|Combat|Buff|Transfer|Death)' ./game/handler 2>&1 | tee "$WORK/test.log"
```

期望：`--- PASS: TestMMORPG` 恰好 **13** 条、0 skip；然后按 SKILL.md §3.4 收尾。

## 4. demo 的最小玩法范围（MMO §4.4，7 组 13 条）

两人组队 / 解散；并发转账重放与拒绝重试；Trade 托管交易（确认、取消、到期、并发竞争）；战斗 → 掉落 → 任务 → 副本完成；Buff 刷新与回滚；死亡与复活期限；切图、返回与旧请求。最小可讲的玩法：进图 → 打怪 → 拾取 → Buff 到期，再加一次面对面交易。

## 5. 业务限制清单

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 多实体同事务（交易三实体）、持久期限、同进程切图（Nest 提交 + Interest Reconcile 两阶段）、Hold / Ready 与高 epoch 全量、Profile 字段白名单、Remote 公会、Saga 跨 sid 赠礼 | MMO §3、§5、§6 |
| 框架不能做 | Player 动态跨 sid 迁移、副本 / 战场自动 HA、跨服撤权传播、已下发信息收回；寻路 / 巡逻组件（movement 只有 MapBasic / Move / MoveTo / Follow）；Skill Runtime 随 Nest 回滚 | MMO §6、§6.1、§7；guide/10-time.md §4B；guide/08-skill.md |
| AOI 容量 | 2026-10-20 sync-aoi（1000 玩家、1 万实体、20Hz、本机 loopback）：单视野 Nest 改值→客户端 P99 约 64ms，+4 视野 78ms（修复后 65ms），+16 视野修复后 97～141ms（ApplyAsync）；缺省 ApplySync 每玩家 ≤ 9 视野；`MaxSpatialViews` 缺省 8192；`MaxObjects` 缺省 100，超出关整个会话 | guide/04-sync.md §2“已测规模与边界”；KNOWN-LIMITS“广播与 AOI” |
| 吞吐 / 心跳 | 生成工程 strict 50 连接约 1253 TPS、同实体约 108 TPS（SKILL.md §5 #3）；心跳最短 50ms，移动建议 200ms | KNOWN-LIMITS“能力与验证边界”；guide/10-time.md §4A |
| 一致性边界 | strict = WAL fsync，Mongo 可见要用 Flush / 投影等待；连接关闭或 ctx 超时不等于撤销；AOI 可见不等于攻击 / 交易授权；Saga 不是跨服瞬时原子交易；Saga 业务键（如跨 sid 赠礼）只在运行期间 + 终态保留期内唯一，保留期后可复用、前后两轮靠协调器生成的生命周期身份区分（维护者 2026-10-31 定案 A），要“同一礼物永远只送一次”在业务数据里记已用过的键 | MMO §3、§7；SAGA.md“定义”业务键与查询；guide/06-saga.md 幂等键 |
| 业务要自己补 | 战斗公式、射程、视线、反作弊；组队 / 交易 / 掉落规则；技能 Host 副作用与持久边界；寻路；副本准入与容量；公会命名与权限；切图失败的重试驱动 | MMO §3“业务负责什么”列 |

## 6. 常见坑（MMO §7）

- 改了 SceneID 就当切图完成：持久事实、兴趣订阅、客户端基线都要更新。
- 两个独立 Manager 的流拼到同一个客户端 Receiver；应用 Full 时不清旧对象（MMO §5）。
- 收到一次确认就交换资产：两个确认绑定同一报价身份。
- 照搬 game-demo 的 Scene 注解：demo 地形 Scene 是 `noPersist=true lifetime=runtime_rebuild`，纵切的 Scene 带人数事实要持久（MMO §2）。
- 框架有 Lockstep 就以为 MMO 必须帧同步：开放世界用服务器权威状态同步。
- 真实 TCP 场景通过就说容量合格：功能与容量分开验收。

## 7. 验证

- §3.1：1 条 PASS + SKILL.md §3.4。2026-10-31 在 v1.33.0 检出、自定 module 实测通过（含 `generate`、build / vet / `id check` / `generate --check`）。2026-10-31 在 v1.33.2 检出（`e4ec6260`）复测：1 PASS、0 SKIP，§3.4 全过；game-demo 工程 `project doctor` 退出 0、无 FAIL。
- §3.2：13 条 PASS。2026-10-31 在 v1.33.1 检出、自定 module `example.com/mymmo` 按 SKILL.md §3.2 + 本节命令 + §3.4 连跑：13 PASS、0 SKIP，build / vet / `id check` / `generate --check` 通过，`project doctor` 仅 4 个 collaborators FAIL。同日在 v1.33.0 上执行，唯一错误是 `mmo_gameplay_test.go` 两处 `Sync_TickPlayerTimer` 多传参数。2026-10-31 在 v1.33.2 检出（`e4ec6260`）复测：13 PASS、0 SKIP，§3.4 全过，doctor 仅 4 项 FAIL。仓库脚本另把 `scripts/testdata/mmorpg/persistence/*_test.go` 复制为 `game/handler/mmo_*_test.go` 后 `go vet`（真实模式夹具只编译），一档 demo 可省。
- 二档：`ROOST_MMORPG_REAL=1`（隔离 Mongo 副本集）正常停机与 SIGKILL 后经投递循环恢复（MMO §11）；`scripts/test-mmorpg-cross-sid.sh` 双 sid 赠礼（需 Mongo + NATS，MMO §13）；game-demo `make dev-up && make dev-run && make dev-smoke`。

## 8. 手册指针

- `docs/framework/MMORPG-INTEGRATION.md`：§3 业务与框架对照（最全的模块映射）、§5 切图与重连、§6 跨服协作、§7 易混边界、§9～§14 验收
- `docs/framework/guide/04-sync.md` §2（AOI 选型、上限、已测规模）、`docs/framework/INTEREST-BLOCK-AOI.md`
- `docs/framework/guide/10-time.md` §4～§4E（定时器、心跳、移动、行为、技能推进）
- `demo/README.md` 与 `demo/` 模板源码（game-demo 是完整参考）
