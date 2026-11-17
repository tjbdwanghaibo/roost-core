# 放置模拟 / 合成休闲

两条纵切都是“单 Player + 一个唤醒 / 期限定时器”的单人联网玩法。手册：`docs/framework/IDLE-SIM-INTEGRATION.md`（下称 IDLE §N）、`docs/framework/MERGE-CASUAL-INTEGRATION.md`（下称 MERGE §N）。路径相对 roost-core 根目录，以检出或 module cache 中的 `docs/` 为准。

> **版本要求**：放置模拟 v1.33.0 起可用；**合成休闲 v1.33.1 起可用（2026-10-31 实测 15 条 PASS），最低版本 v1.33.1**（v1.33.0 上 `merge_casual_test.go` 仍以 3 参数调用生成的 `Sync_TickPlayerTimer`，编译失败）。合成目标版本低于 v1.33.1 时告诉用户升级并停下，不要自行改夹具绕过。
>
> v1.33.2 起夹具 handler 的业务时间改为约定参数 `nowUnixMilli`（分发包装从 fctx 填），**生成的 Sender 不带时间参数**，测试用 `clock.SetSource` 拨时钟；v1.33.1 上的旧夹具 / 旧手册 Sender 示例还带 `now`（SKILL.md §0）。二档 REAL 恢复用例需 ≥ v1.33.2（SKILL.md §7）。

> 本篇命令按当前检出的脚本写（R9 起两类都先 `roost module add` 再只写模块没覆盖的部分；v1.40.0 起 DAO 字段写 `store` 键，v1.35.0 起在最终 `roost generate` 前安装公共 Robot）；目标是更早的 tag 时读该 tag 检出里的本文件（SKILL.md §0）。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 放置：生产队列、原料预扣与取消退款、离线收益窗口与倍率积分、仓库、订单、每日奖励（checkin）。合成：棋盘合成、生产器、惰性体力（stamina）、离线命令批次、设备代际接管、限时挑战、广告奖励账本 | 接受客户端整份存档覆盖服务器（MERGE §9 第一条，只接受命令）；跨服市场（IDLE 未做）；真实广告平台 / 支付 / 登录票分配（夹具是模拟） |

## 2. 用到的框架与业务模块

模块清单（R9，2026-11-17；Q6 只装 enter，Q8 不建配方）：放置 `enter wallet bag checkin`，合成 `enter wallet stamina`（stamina 连带 rules）。

| 业务 | 归谁 | 出处 |
| --- | --- | --- |
| 建档、初始资产 | **enter**：建档 `EnterInit`；初始金币（放置还有初始宝石）写在 `event.EnterFirst` 的 Player 监听（夹具 `first_enter.go.txt`）；玩法存档建在建档之后、要求 `Enter().Built()` | IDLE §2、MERGE §2 |
| 金币 | **wallet**：新命令的金币变化经 `Wallet().Add` 与玩法存档同一笔；“只做一次”由玩法自己的 `Seq` 回执保证 | IDLE §2、§6，MERGE §2、§5 |
| 放置的宝石 | **bag** 道具 `1101`（Q2 B）：初始 10 颗在 `EnterFirst` 监听里 `Bag().Add`，加速 `Bag().Remove`；道具表加一行 | IDLE §2 |
| 放置的每日奖励 | **checkin**：`daily` 命令同一笔 `Checkin().Claim(now)`，按业务日、连续天数加成 | IDLE §5 |
| 合成的体力 | **stamina**（Q1 A）：`Spend` / `Grant` / `Current`，规则在 `game/components/stamina/rules.go`，不随棋盘冻结 | MERGE §2、§4、§6 |
| 生产队列 / 收益积分、棋盘 / 生产器 / 订单 / 挑战 / 广告 | 手写；Player DAO 末尾的 `IdleState` / `MergeState` + `IdleWakeTimer` / `MergeWakeTimer`，`rollback=undo durability=strict`；放置借鉴 idle 模块的“游标只前移整周期”，合成的广告借鉴 checkin 的业务日与 wallet 的业务键 | IDLE §2、MERGE §2 |
| 准点唤醒 / 挑战期限 | `roost add timer --entity Player`，每次写存档替换节点 | IDLE §5、MERGE §6 |
| 离线补算 | 业务按区间积分一次算完（不是框架追帧） | IDLE §4、§5 |
| 配置冻结 | `configdata`；放置写进存档冻结，合成首登冻结（体力规则除外） | IDLE §2、MERGE §4 |
| 每日边界 | `clock.BusinessDay`（`time.business_timezone` / `business_day_start`，缺省 UTC 零点）：放置经 checkin，合成广告次数照 checkin | IDLE §5、MERGE §7；guide/10-time.md §2A |

## 3. 生成最小 demo

接在 SKILL.md §3.2 公共前置命令之后。两者都是“装模块 → `add timer --entity Player` → 追加玩法字段 → 拷夹具 → Robot → generate”；不再 `add dao Player`（game 模板已有），不给 Player 加 `Gold` 等字段（模块的状态由安装器追加）。

### 3.1 放置模拟（`scripts/test-idle-sim-generated.sh`）

```bash
F="$CORE/scripts/testdata/idle-sim"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" enter wallet bag checkin   # = roost module add + 核对已登记
"$ROOST" add timer --entity Player                                                          # 生产与收益的唤醒
dao_add Player db/def/player.go "IdleState string idle_state" "IdleWakeTimer int64 idle_wake_timer"
printf '1101,宝石,0\n' >> configs/table/item.csv                                            # 宝石是 bag 道具（idlesim.GemItem）
mkdir -p game/modules/idlesim
for n in rules state production rules_test; do cp "$F/$n.go.txt" "game/modules/idlesim/$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$F/first_enter.go.txt"    game/entities/player/idle_sim.go      # event.EnterFirst 监听：初始金币与宝石
cp "$F/handlers.go.txt"       game/handler/idle_sim.go
cp "$F/gameplay_test.go.txt"  game/handler/idle_sim_test.go
fix_mod idle-sim-practice game/modules/idlesim/*.go game/entities/player/{timer_handlers,idle_sim}.go game/handler/idle_sim*.go
gofmt -w db/def/player.go game/modules/idlesim/*.go game/entities/player/*.go game/handler/idle_sim*.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" idle-sim   # 公共 Robot：业务 PB、TCP → Sender 接线、cmd/robot 与网络用例
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestIdleSim' ./game/modules/idlesim ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" enter wallet bag checkin
```

期望 `--- PASS: TestIdleSim` 恰好 **13** 条（12 条业务 + `TestIdleSimRobotBusiness`）、0 skip；`modules.sh verify` 打印 `bag 20/20`、`checkin 8/8`、`enter 29/29`、`wallet 17/17`（每个模块通过数 == 检出 `modules/<名>/module.yaml` 的 `verify.pass`、0 skip，并带 build / vet / `id check` / `generate --check`）。

### 3.2 合成休闲（`scripts/test-merge-casual-generated.sh`，v1.33.1 起可用）

```bash
F="$CORE/scripts/testdata/merge-casual"
bash "$CORE/scripts/testdata/genre-common/modules.sh" add "$ROOST" enter wallet stamina   # stamina 连带 rules
"$ROOST" add timer --entity Player                                                      # 挑战期限
dao_add Player db/def/player.go "MergeState string merge_state" "MergeWakeTimer int64 merge_wake_timer"
mkdir -p game/modules/mergecasual
for n in rules state operations reward rules_test; do cp "$F/$n.go.txt" "game/modules/mergecasual/$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$F/first_enter.go.txt"    game/entities/player/merge_casual.go  # event.EnterFirst 监听：初始金币
cp "$F/handlers.go.txt"       game/handler/merge_casual.go
cp "$F/gameplay_test.go.txt"  game/handler/merge_casual_test.go
fix_mod merge-casual-practice game/modules/mergecasual/*.go game/entities/player/{timer_handlers,merge_casual}.go game/handler/merge_casual*.go
gofmt -w db/def/player.go game/modules/mergecasual/*.go game/entities/player/*.go game/handler/merge_casual*.go
python3 "$CORE/scripts/testdata/robot-common/install.py" "$CORE" "$DIR" merge-casual   # 公共 Robot：业务 PB、TCP → Sender 接线、cmd/robot 与网络用例
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMergeCasual' ./game/modules/mergecasual ./game/handler 2>&1 | tee "$WORK/test.log"
bash "$CORE/scripts/testdata/genre-common/modules.sh" verify "$ROOST" "$CORE" "$WORK/module-verify.json" enter wallet stamina
```

期望 `--- PASS: TestMergeCasual` 恰好 **16** 条（15 条业务 + `TestMergeCasualRobotBusiness`）、0 skip；`modules.sh verify` 打印 `enter 29/29`、`rules 10/10`、`stamina 26/26`、`stamina+rules 2/2`、`wallet 17/17`。体力规则用 stamina 自带的 `game/components/stamina/rules.go`，不要改（stamina 自带场景按它的缺省数断言）。两者都以 SKILL.md §3.4 收尾。

## 4. demo 的最小玩法范围

| 玩法 | 场景要点（数值是样例） |
| --- | --- |
| 放置（IDLE §3.4，12 条业务 + 1 条 Robot） | 分段积分与一次积分结果相同；三工坊并行队列独立完成；只退未开工任务且只退一次；仓库容量与订单原子；升级前先按旧倍率结算；驱动补触发不能延长离线窗口；跨日与缺席不放大每日奖励（checkin：连续第 n 天 10×min(n,7)、断签重计）；undo 恢复队列 / 宝石 / 唤醒期限 |
| 合成（MERGE §3.4，15 条业务 + 1 条 Robot） | 棋盘权威与合成规则；生产器重放不重掷不重扣（stamina 只扣一次）；出售撤销不复制物品；离线批次原子、拒绝云端冲突；设备接管 fence 旧写入；挑战只由服务器判胜、并发领奖一次；持久期限只判负一次 |

R9 之后场景名与条数不变，改过断言的场景（旧断言 → 新断言）见 IDLE §3.5、MERGE §3.5。

## 5. 业务限制清单

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 存档、唤醒期限与同一 Player 上的模块状态（wallet / bag / checkin / stamina）同事务、离线后加载只补触发一次、undo 回滚、配置原子发布 | IDLE §10、MERGE §11“证明” |
| 模块边界 | wallet 只有一种货币（放置的宝石做 bag 道具）、去重窗口 128 次（长期“只做一次”靠玩法回执，用 `Add` 不用 `Apply`）；stamina 规则不随存档冻结、`Grant` 到硬顶静默截断（合成要求发足）；checkin 奖励按连续天数加成；`EnterFirst` 里发 bag 道具只要道具号在工程道具表里（模块场景先按正式启动的方式装载 `configs/data`，维护者 2026-11-17） | IDLE §2、§3.5，MERGE §2、§4；各模块 README |
| 框架不能做 | 离线追帧（离线一年也只投递一次，补多少由业务一次算完）；离线实体准点唤醒；“读视图推进时间” | IDLE §5、MERGE §6；KNOWN-LIMITS“按实体的持久定时器（D11）” |
| 时间边界 | 驱动索引在提交之后（AfterCommit）更新，测试读到新状态后仍要有界等待索引；每日边界正式服按业务时区算 | IDLE §5 |
| 一致性边界 | 每玩家一个有序命令流，同序号同内容返回原回执；放置账本 128 条、合成命令 / 广告账本各 128 条、单批 16 条，满了拒绝不淘汰；改 `bson` 名或删字段会丢旧键（加载只打 WARN），要离线迁移 | IDLE §6、MERGE §5、§7 |
| 验证边界 | 缺省模式无 WAL / Mongo、实体常驻内存；SIGKILL 恢复要 `ROOST_IDLE_SIM_REAL=1` / `ROOST_MERGE_CASUAL_REAL=1`；提交在途崩溃、多进程与容量未测 | KNOWN-LIMITS“游戏接入验收”两行 |
| 业务要自己补 | 生产公式、多产线队列、离线上限、加速道具（idle 模块只有一条金币线）；合成规则、挑战、广告平台验签对接、登录票分配与认证、客户端预测与冲突提示；内购、邮件、活动 | IDLE §2、MERGE §8 |

## 6. 常见坑

- 按离线秒数循环追 tick：只在加载后那一次触发里按区间积分（IDLE §8）。
- 让唤醒或读视图续期离线窗口：玩家不上线也能一直累积；窗口只由有效新命令开启（IDLE §4、§8）。
- 把客户端存档当真相：只接受命令，服务器重放（MERGE §9）。
- 用客户端离线时间戳当收益依据：批次里统一用本次请求的服务器时间（MERGE §5）。
- 换序号重试结果未知的请求：用原 Epoch / Seq 查回执（`Sync_IdleReceipt` / `Sync_MergeReceipt`）。

## 7. 验证

- 2026-11-17（R9 批 1B）：§3.1 / §3.2 命令块在当前检出、自定 module 名下实测，13 / 16 PASS、0 SKIP，`modules.sh verify` 全部等于 `verify.pass`（证据 `docs/maintenance/evidence/r9-1b-20261117/`）。
- 放置：§3.1 12 条 PASS + SKILL.md §3.4；2026-10-31 在 v1.33.0 检出、自定 module 实测通过。2026-10-31 在 v1.33.2 检出（`e4ec6260`）复测：12 PASS、0 SKIP，§3.4 全过，doctor 仅 4 项 FAIL。
- 合成：§3.2 15 条 PASS。2026-10-31 在 v1.33.1 检出、自定 module `example.com/mymerge` 按 SKILL.md §3.2 + 本节命令 + §3.4 连跑：15 PASS（`game/modules/mergecasual` 3 + `game/handler` 12）、0 SKIP，build / vet / `id check` / `generate --check` 通过，`project doctor` 仅 4 个 collaborators FAIL。同日在 v1.33.0 上执行，唯一错误是 `merge_casual_test.go` 三处 `Sync_TickPlayerTimer` 多传参数。2026-10-31 在 v1.33.2 检出（`e4ec6260`）复测：15 PASS、0 SKIP，§3.4 全过，doctor 仅 4 项 FAIL。仓库脚本另把 `scripts/testdata/merge-casual/persistence/*_test.go` 复制进 `game/handler` 后 `go vet`（只编译），一档 demo 可省。
- 二档：各自 `*_REAL=1`（隔离 Mongo 副本集）SIGKILL 后冷加载、补算一次、下一期限按时投递，第三个进程核对（IDLE §11、MERGE §12）。

## 8. 手册指针

- `docs/framework/IDLE-SIM-INTEGRATION.md`：§4 时间结算、§5 唤醒定时器、§6 幂等、§8 接入坑、§10～§12 验收
- `docs/framework/MERGE-CASUAL-INTEGRATION.md`：§5 离线批次与设备代际、§6 时间、§7 广告与存储格式、§9 接入坑、§11～§13 验收
- `docs/framework/guide/10-time.md` §2A（业务日）、§4（实体定时器）；`docs/framework/SCHEMA-MIGRATION.md`
