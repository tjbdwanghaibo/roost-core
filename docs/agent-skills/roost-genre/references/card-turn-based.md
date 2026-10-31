# 卡牌 / 回合制 RPG

手册：`docs/framework/CARD-RPG-INTEGRATION.md`（下称 CARD §N）。路径相对 roost-core 根目录，以检出或 module cache 中的 `docs/` 为准。

> **版本要求：v1.33.1 起可用（2026-10-31 实测 16 条 PASS），最低版本 v1.33.1。** v1.33.0 上夹具 `card_rpg_test.go` 仍以 3 参数调用生成的 `Sync_TickCardRunTimer(ctx, id, nowMs)`（D22-1 后签名为 `(ctx, id)`），编译失败。目标版本低于 v1.33.1 时告诉用户升级并停下，不要自行改夹具绕过。
>
> v1.33.2 起夹具 handler 的业务时间改为约定参数 `nowUnixMilli`（分发包装从 fctx 填），**生成的 Sender 不带时间参数**，测试用 `clock.SetSource` 拨时钟；v1.33.1 上的旧夹具 / 旧手册 Sender 示例还带 `now`（SKILL.md §0）。二档 REAL 恢复用例需 ≥ v1.33.2（SKILL.md §7）。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 抽卡 / 保底、养成 / 编队、体力、三人编队回合制 PvE、首通 / 扫荡 / 任务；回合超时托管 | 实时 PvP 对战（无纵切；可借 lockstep 或 Group 同步自己组合）；充值、邮件、活动、跨服 PvP（夹具未实现，game-demo 里的历史通过不等于卡牌场景已验收，CARD §8 末句） |

## 2. 用到的框架模块（CARD §2）

| 业务 | 模块 |
| --- | --- |
| 钻石、卡牌、碎片、体力、序号账本 | Player DAO（`State` 业务 JSON），`rollback=undo durability=strict` |
| 一局战斗 | CardRun 实体（World 锁档）+ `roost add timer --entity CardRun`（回合期限） |
| 战斗属性 | `roost add attribute HeroStats`（生成属性 ID、派生公式、snapshot） |
| 开局 / 领奖 | Player + CardRun 多实体 Sender |
| 卡表与热更 | `configdata.MustRegisterObject`，入口按值捕获 `Rules` |
| 伤害 | `combat.ResolveDamage`；**不用** Skill Runtime（不在 Nest 回滚域，CARD §6） |

## 3. 生成最小 demo（v1.33.1 起可用）

接在 SKILL.md §3.2 公共前置命令之后。对应 `scripts/test-card-rpg-generated.sh`。

```bash
"$ROOST" add dao Player --entity Player
"$ROOST" add entity CardRun
"$ROOST" add dao CardRun --entity CardRun
"$ROOST" add timer --entity CardRun
"$ROOST" add attribute HeroStats
dao_add Player db/def/player.go "State string state"
dao_add CardRun db/def/card_run.go "State string state" "TurnTimer int64 turn_timer"
world game/entities/card_run/entity.go
F="$CORE/scripts/testdata/card-rpg"
mkdir -p game/cardrpg
for n in catalog profile battle catalog_test; do cp "$F/$n.go.txt" "game/cardrpg/$n.go"; done
cp "$F/attributes.go.txt"     game/gameplay/attribute/hero_stats.go
cp "$F/timer_handlers.go.txt" game/entities/card_run/timer_handlers.go
cp "$F/handlers.go.txt"       game/handler/card_rpg.go
cp "$F/gameplay_test.go.txt"  game/handler/card_rpg_test.go
fix_mod card-rpg-practice game/cardrpg/*.go game/gameplay/attribute/hero_stats.go \
  game/entities/card_run/timer_handlers.go game/handler/card_rpg*.go
gofmt -w db/def/*.go game/cardrpg/*.go game/gameplay/attribute/hero_stats.go \
  game/entities/card_run/timer_handlers.go game/handler/card_rpg*.go
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestCardRPG' ./game/cardrpg ./game/handler 2>&1 | tee "$WORK/test.log"
```

期望：`--- PASS: TestCardRPG` 恰好 **16** 条、0 skip；然后按 SKILL.md §3.4 收尾。

## 4. demo 的最小玩法范围（CARD §3.4，16 条）

配置原子热更与冻结；生成属性派生与快照独立；公开视图不含私钥；畸形状态与篡改冻结战斗 fail-closed；抽卡并发重放 / 保底 / 配置身份；抽卡拒绝与 undo 保留资金与随机流；养成与编队快照分离；并发开局只一局一扣体力；回合权威序号与重放不重挂期限；护盾 / 法力 / 毒 / 治疗战斗；以及领奖、重连视图等。最小可讲的玩法：首登 → 抽卡 → 开局（Player + Run 同事务）→ 出手若干回合（或超时托管）→ 胜利发奖一次。

## 5. 业务限制清单

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 扣款与随机结果与回执同事务、undo 回滚、多实体开局与领奖、回合期限持久化并随事务回滚、配置原子发布 | CARD §11“证明” |
| 框架不能做 | 离线追回合：实体不在内存期间到期的节点加载后只触发一次（本例托管一回合再从实际触发时间排下一回合）；Skill Runtime 账本回滚；服务端随机密钥的分配（要可信首登入口用 `crypto/rand`） | CARD §5、§6；KNOWN-LIMITS“按实体的持久定时器（D11）” |
| 一致性边界 | 同 `seq` 同内容返回原回执、不重掷不重扣；改张数 / 跳序拒绝；抽卡与管理账本各至多 64 条，满了拒绝新写、不淘汰旧回执（正式服要归档 / 分片）；配置按值捕获，开局那一代用到底 | CARD §4、§5 |
| 验证边界 | 缺省模式无 WAL / Mongo、定时器手动投递、实体常驻内存；SIGKILL 恢复要 `ROOST_CARD_RPG_REAL=1`；提交在途崩溃、断电、长时计时、Redeliver、多进程与容量未测 | CARD §11；KNOWN-LIMITS“游戏接入验收”卡牌 RPG 行 |
| 业务要自己补 | 卡池与保底参数、养成数值、体力规则、回合战斗规则、首通 / 扫荡、充值到账、邮件补偿、活动排行 | CARD §2、§8 |

## 6. 常见坑（CARD §9）

- 锁接口命名为 `ICardPlayer` / `ICardRun`（不含 `Entity`）：现在 `generate` 直接报 `//roost:nest found no entity parameter`；改为 `ICardPlayerEntity` / `ICardRunEntity` 或写 `targets=`。
- 定时器回调里改 Player：回调只改本 Run，需要 Player 一起变的交给多实体 handler。
- handler 里发同步 Nest 请求：`ErrSyncInHandler`。
- 为“再试一次”换新序号：会重复扣款；重发同一序号和内容（CARD §5）。
- 公网请求携带 `Rules`：配置只由服务端入口捕获（CARD §4）。

## 7. 验证

- 一档：16 条 PASS + SKILL.md §3.4。2026-10-31 在 v1.33.1 检出、自定 module `example.com/mycard` 按 SKILL.md §3.2 + 本节命令 + §3.4 连跑：16 PASS（`game/cardrpg` 4 + `game/handler` 12）、0 SKIP，build / vet / `id check` / `generate --check` 通过，`project doctor` 仅 4 个 collaborators FAIL。同日在 v1.33.0 上执行，唯一错误是 `card_rpg_test.go` `Sync_TickCardRunTimer` 多传参数。2026-10-31 在 v1.33.2 检出（`e4ec6260`）复测：16 PASS（4 + 12）、0 SKIP，§3.4 全过，doctor 仅 4 项 FAIL。
- 仓库脚本另把 `scripts/testdata/card-rpg/persistence/*_test.go` 复制为 `game/handler/card_rpg_*_test.go` 后 `go vet ./game/handler`（真实模式夹具只编译）；只做一档 demo 时可省。
- 二档：`ROOST_CARD_RPG_REAL=1`（隔离 Mongo 副本集）：strict 提交后、WAL 未投影时 SIGKILL，新进程冷加载、投递一次过期回合、续战领奖，第三个进程核对（CARD §12）。

## 8. 手册指针

- `docs/framework/CARD-RPG-INTEGRATION.md`：§2 状态归谁、§3 生成与阅读顺序、§4～§7 设计要点、§8 更多路线、§9 接入坑、§11～§13 验收
- `docs/framework/guide/07-config.md`（业务表发布、请求内一致性）、`docs/framework/guide/08-skill.md`、`docs/framework/guide/10-time.md` §4

## v1.35.0 起的公共 Robot 接线

手工生成 card-rpg 时，在最终 `roost generate` 前执行（`genre` 取本篇对应脚本名）：

```sh
python3 "$repo_root/scripts/testdata/robot-common/install.py" "$repo_root" "$project_root" "$genre"
```

该步骤安装业务 PB、已鉴权 TCP → Sender 接线、`loadtest.Commands`、`cmd/robot` 与网络验收；之后仍须正式 generate。自定义 module 由安装器读取 go.mod。最稳妥的完整顺序直接使用同检出的玩法脚本。新增测试覆盖两个动作、重试、TCP 重连、非法命令和不存在玩家；不是完整业务恢复或真实存储验收。

验收数量与限制统一见 [Robot 手册](../../../framework/ROBOT.md)，不要沿用历史版本的通过数。
