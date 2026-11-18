# 放置模拟 / 合成休闲

两条纵切都是“单 Player + 一个唤醒 / 期限定时器”的单人联网玩法。手册：`docs/framework/IDLE-SIM-INTEGRATION.md`（下称 IDLE §N）、`docs/framework/MERGE-CASUAL-INTEGRATION.md`（下称 MERGE §N）。命令对应当前版本的 `scripts/test-idle-sim-generated.sh` 与 `scripts/test-merge-casual-generated.sh`；目标 roost-core 版本更早时读那个 tag 里的本文件。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 放置：生产队列、原料预扣与取消退款、离线收益窗口与倍率积分、仓库、订单、每日奖励。合成：棋盘合成、生产器、惰性体力、离线命令批次、设备代际接管、限时挑战、广告奖励账本 | 接受客户端整份存档覆盖服务器（只接受命令，MERGE §9）；跨服市场；真实广告平台 / 支付 / 登录票分配（夹具是模拟） |

## 2. 命令

接在 SKILL.md §3.2 公共前置命令之后。模块与分工：

| 业务 | 归谁 |
| --- | --- |
| 建档、初始资产 | **enter**；初始金币（放置还有初始宝石）写在 `event.EnterFirst` 的 Player 监听；玩法存档建在建档之后、要求 `Enter().Built()` |
| 金币 | **wallet**：金币变化经 `Wallet().Add` 与玩法存档同一笔；“只做一次”由玩法自己的 `Seq` 回执保证 |
| 放置的宝石 | **bag** 道具 1101：初始 10 颗在 `EnterFirst` 监听里 `Bag().Add`，加速 `Bag().Remove` |
| 放置的每日奖励 | **checkin**：`daily` 命令同一笔 `Checkin().Claim(now)` |
| 合成的体力 | **stamina**：`Spend` / `Grant` / `Current`，规则在 `game/components/stamina/rules.go`，不随棋盘冻结 |
| 生产队列、收益积分、棋盘、生产器、订单、挑战、广告 | 手写：Player DAO 末尾的 `IdleState` / `MergeState` + `IdleWakeTimer` / `MergeWakeTimer`；准点唤醒与挑战期限用 `add timer --entity Player`，每次写存档替换节点；离线补算按区间积分一次算完 |

### 2.1 放置模拟

```bash
F="$CORE/scripts/testdata/idle-sim"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" enter wallet bag checkin
"$ROOST" add timer --entity Player                                                          # 生产与收益的唤醒
dao_add Player db/def/player.go "IdleState string idle_state" "IdleWakeTimer int64 idle_wake_timer"
printf '1101,宝石,0\n' >> configs/table/item.csv                                            # 宝石是 bag 道具
mkdir -p game/modules/idlesim
for n in rules state production rules_test; do cp "$F/$n.go.txt" "game/modules/idlesim/$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$F/first_enter.go.txt"    game/entities/player/idle_sim.go      # EnterFirst 监听：初始金币与宝石
cp "$F/handlers.go.txt"       game/handler/idle_sim.go
cp "$F/gameplay_test.go.txt"  game/handler/idle_sim_test.go
fix_mod idle-sim-practice game/modules/idlesim/*.go game/entities/player/{timer_handlers,idle_sim}.go game/handler/idle_sim*.go
gofmt -w db/def/player.go game/modules/idlesim/*.go game/entities/player/*.go game/handler/idle_sim*.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" idle-sim
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestIdleSim' ./game/modules/idlesim ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" enter wallet bag checkin
```

### 2.2 合成休闲

```bash
F="$CORE/scripts/testdata/merge-casual"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" enter wallet stamina   # stamina 连带 rules
"$ROOST" add timer --entity Player                                                      # 挑战期限
dao_add Player db/def/player.go "MergeState string merge_state" "MergeWakeTimer int64 merge_wake_timer"
mkdir -p game/modules/mergecasual
for n in rules state operations reward rules_test; do cp "$F/$n.go.txt" "game/modules/mergecasual/$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$F/first_enter.go.txt"    game/entities/player/merge_casual.go  # EnterFirst 监听：初始金币
cp "$F/handlers.go.txt"       game/handler/merge_casual.go
cp "$F/gameplay_test.go.txt"  game/handler/merge_casual_test.go
fix_mod merge-casual-practice game/modules/mergecasual/*.go game/entities/player/{timer_handlers,merge_casual}.go game/handler/merge_casual*.go
gofmt -w db/def/player.go game/modules/mergecasual/*.go game/entities/player/*.go game/handler/merge_casual*.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" merge-casual
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMergeCasual' ./game/modules/mergecasual ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" enter wallet stamina
```

体力规则用 stamina 自带的 `rules.go`，不要改（stamina 自带场景按它的缺省数断言）。两者都按 SKILL.md §3.4 收尾。

## 3. 期望场景数

| 玩法 | 期望 | 场景要点（数值是样例） |
| --- | --- | --- |
| 放置 | `--- PASS: TestIdleSim` 恰好 **13** 条（12 条业务 + Robot）、0 SKIP；`bag 20/20`、`checkin 8/8`、`enter 29/29`、`wallet 17/17` | 分段积分与一次积分结果相同；三工坊并行队列；只退未开工任务且只退一次；仓库容量与订单原子；升级前按旧倍率结算；驱动补触发不延长离线窗口；跨日与缺席不放大每日奖励；回滚恢复队列 / 宝石 / 唤醒期限 |
| 合成 | `--- PASS: TestMergeCasual` 恰好 **16** 条（15 条业务 + Robot）、0 SKIP；`enter 29/29`、`rules 10/10`、`stamina 26/26`、`stamina+rules 2/2`、`wallet 17/17` | 棋盘权威与合成规则；生产器重放不重掷不重扣；出售撤销不复制物品；离线批次原子、拒绝云端冲突；设备接管挡住旧写入；挑战只由服务器判胜、并发领奖一次；持久期限只判负一次 |

## 4. 限制

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 存档、唤醒期限与同一 Player 上的模块状态同事务；离线后加载只补触发一次；回滚；配置原子发布 | IDLE §10、MERGE §11 |
| 模块边界 | wallet 只有一种货币、去重窗口 128 次（长期“只做一次”靠玩法回执，用 `Add` 不用 `Apply`）；stamina 规则不随存档冻结、`Grant` 到上限静默截断；checkin 奖励按连续天数加成 | 各模块 README |
| 框架不能做 | 离线追帧（离线一年也只投递一次，补多少由业务一次算完）；离线实体准点唤醒；“读视图推进时间” | IDLE §5、MERGE §6；KNOWN-LIMITS“按实体的持久定时器” |
| 时间 | 驱动索引在提交之后更新，测试读到新状态后仍要有界等待；每日边界按业务时区算 | IDLE §5 |
| 一致性 | 每玩家一个有序命令流，同序号同内容返回原回执；放置账本 128 条、合成命令 / 广告账本各 128 条、单批 16 条，满了拒绝不淘汰；改存储键或删字段会丢旧数据，要离线迁移 | IDLE §6、MERGE §5、§7 |
| 缺省模式不证明 | 无 WAL / Mongo、实体常驻内存；真实依赖档 `ROOST_IDLE_SIM_REAL=1` / `ROOST_MERGE_CASUAL_REAL=1`（隔离 Mongo）：SIGKILL 后冷加载、补算一次、下一期限按时投递；提交在途崩溃、多进程与容量未测 | IDLE §11、MERGE §12 |
| 业务要自己补 | 生产公式、多产线队列、离线上限、加速道具；合成规则、挑战、广告平台验签、登录票分配与认证、客户端预测与冲突提示；内购、邮件、活动 | IDLE §2、MERGE §8 |

**常见坑**：

- 按离线秒数循环追 tick：只在加载后那一次触发里按区间积分（IDLE §8）。
- 让唤醒或读视图续期离线窗口：窗口只由有效新命令开启。
- 把客户端存档当真相：只接受命令，服务器重放（MERGE §9）。
- 用客户端离线时间戳当收益依据：批次里统一用本次请求的服务器时间。
- 换序号重试结果未知的请求：用原 Epoch / Seq 查回执（`Sync_IdleReceipt` / `Sync_MergeReceipt`）。
