# 卡牌 / 回合制 RPG

手册：`docs/framework/CARD-RPG-INTEGRATION.md`（下称 CARD §N）。命令对应当前版本的 `scripts/test-card-rpg-generated.sh`；目标 roost-core 版本更早时读那个 tag 里的本文件。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 抽卡 / 保底、养成 / 编队、体力、三人编队回合制 PvE、首通 / 扫荡 / 任务；回合超时托管 | 实时 PvP 对战（没有纵切；可借 lockstep 或组同步自己组合）；充值、邮件、活动、跨服 PvP（夹具未实现，CARD §8） |

## 2. 命令

接在 SKILL.md §3.2 公共前置命令之后。模块与分工：

| 业务 | 归谁 |
| --- | --- |
| 建档、初始卡组与初始资产 | **enter**；初始卡组、100 金币 / 100 钻石 / 20 尘写在 Player 上的 `event.EnterFirst` 监听 |
| 金币 | **wallet**（养成 / 领奖 `Wallet().Add`） |
| 钻石、尘 | **bag** 道具 2001 / 2002（次要货币做道具） |
| 体力 | **stamina**（开局 / 扫荡 `Stamina().Spend`，用模块缺省规则：上限 100、每 5 分钟恢复 1 点） |
| 抽卡、保底、十连、请求回执 | **gacha**（卡池 2，扣金币；卡牌 handler 调 `Gacha().Draw`，同一笔事务把英雄道具换成卡牌实例、重复卡转碎片） |
| 首胜任务 | **task**（`Report("card.won")`，客户端 `TaskClaim` 领 10 钻石） |
| 卡牌实例、碎片、编队、养成 / 开局账本、首通 | 手写：Player DAO 的 `State`（业务 JSON） |
| 一局战斗 | 手写：CardRun 实体（World 锁档）+ `add timer --entity CardRun`（回合期限）；属性 `roost add attribute HeroStats`；伤害 `combat.ResolveDamage`，**不用** Skill Runtime（不在 Nest 回滚域，CARD §6） |
| 卡表与热更 | `configdata.MustRegisterObject`，入口按值捕获 `Rules` |

```bash
F="$CORE/scripts/testdata/card-rpg"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" enter wallet bag stamina gacha task
"$ROOST" add entity CardRun
"$ROOST" add dao CardRun --entity CardRun
"$ROOST" add timer --entity CardRun
"$ROOST" add attribute HeroStats
dao_add Player db/def/player.go "State string state"
dao_add CardRun db/def/card_run.go "State string state" "TurnTimer int64 turn_timer"
world game/entities/card_run/entity.go
# 卡牌的道具、卡池与首胜任务追加在模块自带的表后面（CARD §3.1）
printf '%s\n' 2001,钻石,0 2002,尘,0 2101,英雄1,0 2102,英雄2,0 2103,英雄3,0 2104,英雄4,0 2105,英雄5,0 2106,英雄6,0 >> configs/table/item.csv
printf '%s\n' 2,2,5,2106,false >> configs/table/gacha_pool.csv
printf '%s\n' 201,2,2101,27 202,2,2102,27 203,2,2103,26 204,2,2104,7 205,2,2105,7 206,2,2106,6 >> configs/table/gacha_drop.csv
printf '%s\n' 101,card.won,permanent,1,0,item,2001,10,false >> configs/table/task.csv
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
  game/entities/card_run/timer_handlers.go game/entities/player/card_rpg.go game/handler/card_rpg*.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" card-rpg
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestCardRPG' ./game/modules/cardrpg ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" enter wallet bag stamina gacha task
```

然后按 SKILL.md §3.4 收尾。

## 3. 期望场景数

`--- PASS: TestCardRPG` 恰好 **17** 条（16 条业务 + `TestCardRPGRobotBusiness`）、0 SKIP；`modules.sh verify` 打印 `bag 20/20`、`enter 29/29`、`gacha 29/29`、`rules 10/10`、`stamina 26/26`、`stamina+rules 2/2`、`task 29/29`、`task+bag 3/3`、`task+stamina 3/3`、`task+wallet 6/6`、`wallet 17/17`。

业务场景覆盖：配置原子热更与冻结；属性派生与快照独立；公开视图不含私钥；畸形状态与篡改冻结战斗 fail-closed；抽卡并发重放 / 保底 / 卡池热更不重算旧回执；抽卡被拒资金不变；养成与编队快照分离；并发开局只一局一扣体力；回合权威序号与重放；护盾 / 法力 / 毒 / 治疗战斗；领奖与重连视图。

## 4. 限制

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 扣款、抽样、道具换卡牌与回执同事务；回滚；多实体开局与领奖；回合期限持久化并随事务回滚；配置原子发布 | CARD §11 |
| 框架不能做 | 离线追回合：不在内存期间到期的节点加载后只触发一次；Skill Runtime 账本回滚；服务端随机密钥的分配（建档监听用 `crypto/rand`） | CARD §5、§6；KNOWN-LIMITS“按实体的持久定时器” |
| 模块边界 | gacha 随机不可验证、失败的尝试重新抽样、只留最近 64 次回执（窗口外旧键当新请求再扣一次）、只扣金币、不做卡牌实例；stamina 规则对所有玩家立刻生效；wallet 只有一种货币 | CARD §5；`modules/gacha/README.md` |
| 一致性 | 抽卡同键同次数返回原回执，换次数 `gacha_key_conflict`；养成账本至多 64 条，满了拒绝新写（正式服要归档 / 分片）；配置按值捕获，开局那一代用到底 | CARD §4、§5 |
| 缺省模式不证明 | 无 WAL / Mongo、定时器手动投递、实体常驻内存；真实依赖档 `ROOST_CARD_RPG_REAL=1`（隔离 Mongo）：SIGKILL 后冷加载、投递一次过期回合、续战领奖；提交在途崩溃、断电、多进程与容量未测 | CARD §11、§12 |
| 业务要自己补 | 卡池与保底参数、卡牌实例与碎片、养成数值、回合战斗规则、首通 / 扫荡、充值到账、邮件补偿、活动排行 | CARD §2、§8 |

**常见坑**（CARD §9）：

- 锁接口命名为 `ICardPlayer`（不含 `Entity`）：`generate` 报 `//roost:nest found no entity parameter`；改成 `ICardPlayerEntity` 或写 `targets=`。
- 定时器回调里改 Player：回调只改本 Run，要 Player 一起变的交给多实体 handler。
- 为“再试一次”换新序号 / 新请求键：会重复扣款；抽卡重发同一个请求键，养成重发同一序号和内容。
- 卡牌规则另开一个 configdata Store 装载：会顶掉 gacha / bag / task 读的表；`cardrpg.OpenCatalog` 把规则与生成的配置登记在同一个 Registry。
- EnterFirst 监听里发 bag 道具：道具号写进工程道具表 `item.csv` 即可，不改 bag 的规则入口。
- 公网请求携带 `Rules`：配置只由服务端入口捕获。
