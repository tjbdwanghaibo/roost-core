# 卡牌 / 回合制 RPG

手册：`docs/framework/CARD-RPG-INTEGRATION.md`（下称 CARD §N）。路径相对 roost-core 根目录，以检出或 module cache 中的 `docs/` 为准。

> **版本要求：v1.33.1 起可用（2026-10-31 实测 16 条 PASS），最低版本 v1.33.1。** v1.33.0 上夹具 `card_rpg_test.go` 仍以 3 参数调用生成的 `Sync_TickCardRunTimer(ctx, id, nowMs)`（D22-1 后签名为 `(ctx, id)`），编译失败。目标版本低于 v1.33.1 时告诉用户升级并停下，不要自行改夹具绕过。
>
> v1.33.2 起夹具 handler 的业务时间改为约定参数 `nowUnixMilli`（分发包装从 fctx 填），**生成的 Sender 不带时间参数**，测试用 `clock.SetSource` 拨时钟；v1.33.1 上的旧夹具 / 旧手册 Sender 示例还带 `now`（SKILL.md §0）。二档 REAL 恢复用例需 ≥ v1.33.2（SKILL.md §7）。

> 本篇命令按当前检出的脚本写（v1.40.0 起 DAO 字段写 `store` 键，v1.35.0 起在最终 `roost generate` 前安装公共 Robot；**R9（2026-11-17）起改为“装模块 + 只写模块没覆盖的部分”**，命令块与场景语义都变了，见 §3 与 CARD §3.5）；目标是更早的 tag 时读该 tag 检出里的本文件（SKILL.md §0）。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 抽卡 / 保底、养成 / 编队、体力、三人编队回合制 PvE、首通 / 扫荡 / 任务；回合超时托管 | 实时 PvP 对战（无纵切；可借 lockstep 或 Group 同步自己组合）；充值、邮件、活动、跨服 PvP（夹具未实现，game-demo 里的历史通过不等于卡牌场景已验收，CARD §8 末句） |

## 2. 用到的框架模块（CARD §2）

| 业务 | 模块 |
| --- | --- |
| 建档：初始卡组与初始资产 | **enter**（`EnterInit`；初始卡组、100 金币 / 100 钻石 / 20 尘写在 Player 上的 `event.EnterFirst` 监听） |
| 金币 | **wallet**（养成 / 领奖 `Wallet().Add`） |
| 钻石、尘 | **bag** 道具 2001 / 2002（Q2 B：次要货币做道具） |
| 体力 | **stamina**（开局 / 扫荡 `Stamina().Spend`；规则用模块缺省：上限 100、每 5 分钟恢复 1 点） |
| 抽卡、保底、十连、请求回执 | **gacha**（卡池 2，扣金币；卡牌 handler 调 `Gacha().Draw`，同一笔事务把英雄道具换成卡牌实例、重复卡转碎片） |
| 首胜任务 | **task**（领奖 `Report("card.won")`，客户端 `TaskClaim` 领 10 钻石） |
| 卡牌实例、碎片、编队、养成 / 开局账本、首通 | Player DAO（`State` 业务 JSON，手写），`rollback=undo durability=strict` |
| 一局战斗 | CardRun 实体（World 锁档）+ `roost add timer --entity CardRun`（回合期限） |
| 战斗属性 | `roost add attribute HeroStats`（生成属性 ID、派生公式、snapshot） |
| 开局 / 领奖 | Player + CardRun 多实体 Sender |
| 卡表与热更 | `configdata.MustRegisterObject`，入口按值捕获 `Rules` |
| 伤害 | `combat.ResolveDamage`；**不用** Skill Runtime（不在 Nest 回滚域，CARD §6） |

## 3. 生成最小 demo（R9 起装模块）

接在 SKILL.md §3.2 公共前置命令之后。对应 `scripts/test-card-rpg-generated.sh`。模块清单 `enter wallet bag stamina gacha task`（stamina 连带装 rules；Q6 只装 enter，不装 login / portal）；不再 `add dao Player`（game 模板已有），不给 Player 加资产字段。

```bash
F="$CORE/scripts/testdata/card-rpg"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" enter wallet bag stamina gacha task   # = roost module add + 核对已登记
"$ROOST" add entity CardRun
"$ROOST" add dao CardRun --entity CardRun
"$ROOST" add timer --entity CardRun
"$ROOST" add attribute HeroStats
dao_add Player db/def/player.go "State string state"
dao_add CardRun db/def/card_run.go "State string state" "TurnTimer int64 turn_timer"
world game/entities/card_run/entity.go
# 卡牌的道具、卡池与首胜任务追加在模块自带的表后面（CARD §3.1 第 3 步）
printf '%s\n' 2001,钻石,0 2002,尘,0 2101,英雄1,0 2102,英雄2,0 2103,英雄3,0 2104,英雄4,0 2105,英雄5,0 2106,英雄6,0 >> configs/table/item.csv
printf '%s\n' 2,2,5,2106,false >> configs/table/gacha_pool.csv
printf '%s\n' 201,2,2101,27 202,2,2102,27 203,2,2103,26 204,2,2104,7 205,2,2105,7 206,2,2106,6 >> configs/table/gacha_drop.csv
printf '%s\n' 101,card.won,permanent,1,0,item,2001,10,false >> configs/table/task.csv
# 钻石与尘建档时就发，写进 bag 的规则入口，不依赖道具表是否已装载（enter 的模块场景不装工程的表；CARD §3.1）
perl -0pi -e 's/(func ItemRule\(itemID int64\) \(maxStack int64, ok bool\) \{\n)/$1\tif itemID == 2001 || itemID == 2002 {\n\t\treturn 0, true\n\t}\n/ or die "no ItemRule\n"' game/components/bag/items.go
mkdir -p game/modules/cardrpg
for n in catalog profile battle catalog_test; do cp "$F/$n.go.txt" "game/modules/cardrpg/$n.go"; done
cp "$F/attributes.go.txt"     game/modules/gameplay/attribute/hero_stats.go
cp "$F/timer_handlers.go.txt" game/entities/card_run/timer_handlers.go
cp "$F/enter_first.go.txt"    game/entities/player/card_rpg.go          # EnterFirst 监听：初始卡组与初始资产
cp "$F/handlers.go.txt"       game/handler/card_rpg.go
cp "$F/gameplay_test.go.txt"  game/handler/card_rpg_test.go
fix_mod card-rpg-practice game/modules/cardrpg/*.go game/modules/gameplay/attribute/hero_stats.go \
  game/entities/card_run/timer_handlers.go game/entities/player/card_rpg.go game/handler/card_rpg*.go
gofmt -w db/def/*.go game/modules/cardrpg/*.go game/modules/gameplay/attribute/hero_stats.go \
  game/entities/card_run/timer_handlers.go game/entities/player/card_rpg.go game/components/bag/items.go game/handler/card_rpg*.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" card-rpg   # 公共 Robot：业务 PB、TCP → Sender 接线、cmd/robot 与网络用例
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestCardRPG' ./game/modules/cardrpg ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" enter wallet bag stamina gacha task
```

期望：`--- PASS: TestCardRPG` 恰好 **17** 条（16 条业务 + `TestCardRPGRobotBusiness`）、0 skip；`modules.sh verify` 打印 `bag 20/20`、`enter 29/29`、`gacha 29/29`、`rules 10/10`、`stamina 26/26`、`stamina+rules 2/2`、`task 29/29`、`task+bag 3/3`、`task+stamina 3/3`、`task+wallet 6/6`、`wallet 17/17`（每个模块通过数 == 检出 `modules/<名>/module.yaml` 的 `verify.pass`、0 skip）；然后按 SKILL.md §3.4 收尾。

## 4. demo 的最小玩法范围（CARD §3.4，16 条业务 + 1 条 Robot）

配置原子热更与冻结；生成属性派生与快照独立；公开视图不含私钥；畸形状态与篡改冻结战斗 fail-closed；抽卡并发重放 / 保底 / 卡池热更不重算旧回执（gacha）；抽卡被拒与 undo 资金不变、成功结果由回执固定（gacha 失败的尝试会重新抽样）；养成与编队快照分离；并发开局只一局一扣体力；回合权威序号与重放不重挂期限；护盾 / 法力 / 毒 / 治疗战斗；以及领奖、重连视图等。最小可讲的玩法：建档（enter）→ 抽卡（gacha，道具换卡牌）→ 开局（扣 stamina 体力，Player + Run 同事务）（Player + Run 同事务）→ 出手若干回合（或超时托管）→ 胜利发奖一次。

## 5. 业务限制清单

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 扣款、抽样、道具换卡牌与回执同事务（gacha + 卡牌 handler）、undo 回滚、多实体开局与领奖、回合期限持久化并随事务回滚、配置原子发布 | CARD §11“证明” |
| 框架不能做 | 离线追回合：实体不在内存期间到期的节点加载后只触发一次（本例托管一回合再从实际触发时间排下一回合）；Skill Runtime 账本回滚；服务端随机密钥的分配（建档的 EnterFirst 监听用 `crypto/rand`） | CARD §5、§6；KNOWN-LIMITS“按实体的持久定时器（D11）” |
| 模块边界（R9） | gacha 随机不可验证、失败的尝试重新抽样、只留最近 64 次回执（窗口外旧键当新请求再扣一次）、只扣金币、不做卡牌实例；stamina 规则对所有玩家立刻生效、时钟回拨不恢复也不拒绝；wallet 只有一种货币（钻石、尘做 bag 道具） | CARD §3.5、§5；`modules/gacha/README.md` |
| 一致性边界 | 抽卡同键同次数返回原回执、不重掷不重扣，换次数 `gacha_key_conflict`；养成账本至多 64 条，满了拒绝新写、不淘汰旧回执（正式服要归档 / 分片）；配置按值捕获，开局那一代用到底 | CARD §4、§5 |
| 验证边界 | 缺省模式无 WAL / Mongo、定时器手动投递、实体常驻内存；SIGKILL 恢复要 `ROOST_CARD_RPG_REAL=1`；提交在途崩溃、断电、长时计时、Redeliver、多进程与容量未测 | CARD §11；KNOWN-LIMITS“游戏接入验收”卡牌 RPG 行 |
| 业务要自己补 | 卡池与保底参数（gacha 表）、卡牌实例与碎片、养成数值、回合战斗规则、首通 / 扫荡、充值到账、邮件补偿、活动排行 | CARD §2、§8 |

## 6. 常见坑（CARD §9）

- 锁接口命名为 `ICardPlayer` / `ICardRun`（不含 `Entity`）：现在 `generate` 直接报 `//roost:nest found no entity parameter`；改为 `ICardPlayerEntity` / `ICardRunEntity` 或写 `targets=`。
- 定时器回调里改 Player：回调只改本 Run，需要 Player 一起变的交给多实体 handler。
- handler 里发同步 Nest 请求：`ErrSyncInHandler`。
- 为“再试一次”换新序号 / 新请求键：会重复扣款；抽卡重发同一个 gacha 请求键，养成重发同一序号和内容（CARD §5）。
- 卡牌规则另开一个 configdata Store 装载：装载会发布成进程缺省配置，覆盖 gacha / bag / task 读的表；`cardrpg.OpenCatalog` 把 `card_rules` 与 `generated.RegisterGeneratedConfigData` 登记在同一个 Registry（CARD §4）。
- EnterFirst 监听里发 bag 道具：enter 的模块场景不装工程道具表，道具号须在 bag 规则入口 `items.go` 里认下，否则 `module verify` 的 enter 场景答 `bag_item_invalid`（CARD §3.1）。
- 公网请求携带 `Rules`：配置只由服务端入口捕获（CARD §4）。

## 7. 验证

- 一档（R9 起）：17 条 PASS（含 Robot）+ 已装模块 `module verify` 全部等于 `verify.pass` + SKILL.md §3.4；2026-11-17 在 main（R9 批 1）按 SKILL.md §3.2（自定 module `example.com/mycard`）+ 本篇 §3 命令块实跑，见 CARD §3.5 证据链接。以下为 R9 之前的记录。
- 一档（R9 之前）：16 条 PASS + SKILL.md §3.4。2026-10-31 在 v1.33.1 检出、自定 module `example.com/mycard` 按 SKILL.md §3.2 + 本节命令 + §3.4 连跑：16 PASS（`game/modules/cardrpg` 4 + `game/handler` 12）、0 SKIP，build / vet / `id check` / `generate --check` 通过，`project doctor` 仅 4 个 collaborators FAIL。同日在 v1.33.0 上执行，唯一错误是 `card_rpg_test.go` `Sync_TickCardRunTimer` 多传参数。2026-10-31 在 v1.33.2 检出（`e4ec6260`）复测：16 PASS（4 + 12）、0 SKIP，§3.4 全过，doctor 仅 4 项 FAIL。
- 仓库脚本另把 `scripts/testdata/card-rpg/persistence/*_test.go` 复制为 `game/handler/card_rpg_*_test.go` 后 `go vet ./game/handler`（真实模式夹具只编译）；只做一档 demo 时可省。
- 二档：`ROOST_CARD_RPG_REAL=1`（隔离 Mongo 副本集）：strict 提交后、WAL 未投影时 SIGKILL，新进程冷加载、投递一次过期回合、续战领奖，第三个进程核对（CARD §12）。

## 8. 手册指针

- `docs/framework/CARD-RPG-INTEGRATION.md`：§2 状态归谁、§3 生成与阅读顺序、§4～§7 设计要点、§8 更多路线、§9 接入坑、§11～§13 验收
- `docs/framework/guide/07-config.md`（业务表发布、请求内一致性）、`docs/framework/guide/08-skill.md`、`docs/framework/guide/10-time.md` §4
