# 放置模拟 / 合成休闲

两条纵切都是“单 Player + 一个唤醒 / 期限定时器”的单人联网玩法。手册：`docs/framework/IDLE-SIM-INTEGRATION.md`（下称 IDLE §N）、`docs/framework/MERGE-CASUAL-INTEGRATION.md`（下称 MERGE §N）。路径相对 roost-core 根目录，以检出或 module cache 中的 `docs/` 为准。

> **版本要求**：放置模拟 v1.33.0 起可用；**合成休闲 v1.33.1 起可用（2026-10-31 实测 15 条 PASS），最低版本 v1.33.1**（v1.33.0 上 `merge_casual_test.go` 仍以 3 参数调用生成的 `Sync_TickPlayerTimer`，编译失败）。合成目标版本低于 v1.33.1 时告诉用户升级并停下，不要自行改夹具绕过。
>
> v1.33.2 起夹具 handler 的业务时间改为约定参数 `nowUnixMilli`（分发包装从 fctx 填），**生成的 Sender 不带时间参数**，测试用 `clock.SetSource` 拨时钟；v1.33.1 上的旧夹具 / 旧手册 Sender 示例还带 `now`（SKILL.md §0）。二档 REAL 恢复用例需 ≥ v1.33.2（SKILL.md §7）。

## 1. 适用与不适用

| 适用 | 不适用 / 要另想办法 |
| --- | --- |
| 放置：生产队列、原料预扣与取消退款、离线收益窗口与倍率积分、仓库、订单、每日奖励。合成：棋盘合成、生产器、惰性体力、离线命令批次、设备代际接管、限时挑战、广告奖励账本 | 接受客户端整份存档覆盖服务器（MERGE §9 第一条，只接受命令）；跨服市场（IDLE 未做）；真实广告平台 / 支付 / 登录票分配（夹具是模拟） |

## 2. 用到的框架模块

| 业务 | 模块 | 出处 |
| --- | --- | --- |
| 全部存档（业务 JSON） | Player DAO `State` + `WakeTimer`，`rollback=undo durability=strict` | IDLE §2、MERGE §2 |
| 准点唤醒 / 挑战期限 | `roost add timer --entity Player`，每次写存档替换节点 | IDLE §5、MERGE §6 |
| 离线补算 | 业务按区间积分一次算完（不是框架追帧） | IDLE §4、§5 |
| 配置冻结 | `configdata`；放置写进存档冻结，合成首登冻结 | IDLE §2、MERGE §4 |
| 每日边界 | `time.business_timezone` / `business_day_start`（夹具用演示 UTC 日界） | IDLE §5；guide/10-time.md §2A |

## 3. 生成最小 demo

接在 SKILL.md §3.2 公共前置命令之后。两者只差夹具目录与文件。

### 3.1 放置模拟（`scripts/test-idle-sim-generated.sh`）

```bash
"$ROOST" add dao Player --entity Player
"$ROOST" add timer --entity Player
dao_add Player db/def/player.go "State string state" "WakeTimer int64 wake_timer"
F="$CORE/scripts/testdata/idle-sim"
mkdir -p game/idlesim
for n in rules state production rules_test; do cp "$F/$n.go.txt" "game/idlesim/$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$F/handlers.go.txt"       game/handler/idle_sim.go
cp "$F/gameplay_test.go.txt"  game/handler/idle_sim_test.go
fix_mod idle-sim-practice game/idlesim/*.go game/entities/player/timer_handlers.go game/handler/idle_sim*.go
gofmt -w db/def/*.go game/idlesim/*.go game/entities/player/timer_handlers.go game/handler/idle_sim*.go
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestIdleSim' ./game/idlesim ./game/handler 2>&1 | tee "$WORK/test.log"
```

期望 `--- PASS: TestIdleSim` 恰好 **12** 条、0 skip。

### 3.2 合成休闲（`scripts/test-merge-casual-generated.sh`，v1.33.1 起可用）

```bash
"$ROOST" add dao Player --entity Player
"$ROOST" add timer --entity Player
dao_add Player db/def/player.go "State string state" "WakeTimer int64 wake_timer"
F="$CORE/scripts/testdata/merge-casual"
mkdir -p game/mergecasual
for n in rules state operations reward rules_test; do cp "$F/$n.go.txt" "game/mergecasual/$n.go"; done
cp "$F/timer_handlers.go.txt" game/entities/player/timer_handlers.go
cp "$F/handlers.go.txt"       game/handler/merge_casual.go
cp "$F/gameplay_test.go.txt"  game/handler/merge_casual_test.go
fix_mod merge-casual-practice game/mergecasual/*.go game/entities/player/timer_handlers.go game/handler/merge_casual*.go
gofmt -w db/def/*.go game/mergecasual/*.go game/entities/player/timer_handlers.go game/handler/merge_casual*.go
"$ROOST" generate
go test -mod=mod -race -count=1 -timeout 2m -v -run '^TestMergeCasual' ./game/mergecasual ./game/handler 2>&1 | tee "$WORK/test.log"
```

期望 `--- PASS: TestMergeCasual` 恰好 **15** 条、0 skip。两者都以 SKILL.md §3.4 收尾。

## 4. demo 的最小玩法范围

| 玩法 | 场景要点（数值是样例） |
| --- | --- |
| 放置（IDLE §3.4，12 条） | 分段积分与一次积分结果相同；三工坊并行队列独立完成；只退未开工任务且只退一次；仓库容量与订单原子；升级前先按旧倍率结算；驱动补触发不能延长离线窗口；跨日与缺席不放大每日奖励；undo 恢复队列 / 经济 / 唤醒期限 |
| 合成（MERGE §3.4，15 条） | 棋盘权威与合成规则；生产器重放不重掷不重扣；出售撤销不复制物品；离线批次原子、拒绝云端冲突；设备接管 fence 旧写入；挑战只由服务器判胜、并发领奖一次；持久期限只判负一次 |

## 5. 业务限制清单

| 类别 | 内容 | 出处 |
| --- | --- | --- |
| 框架能做 | 存档与唤醒期限同事务、离线后加载只补触发一次、undo 回滚、配置原子发布 | IDLE §10、MERGE §11“证明” |
| 框架不能做 | 离线追帧（离线一年也只投递一次，补多少由业务一次算完）；离线实体准点唤醒；“读视图推进时间” | IDLE §5、MERGE §6；KNOWN-LIMITS“按实体的持久定时器（D11）” |
| 时间边界 | 驱动索引在提交之后（AfterCommit）更新，测试读到新状态后仍要有界等待索引；每日边界正式服按业务时区算 | IDLE §5 |
| 一致性边界 | 每玩家一个有序命令流，同序号同内容返回原回执；放置账本 128 条、合成命令 / 广告账本各 128 条、单批 16 条，满了拒绝不淘汰；改 `bson` 名或删字段会丢旧键（加载只打 WARN），要离线迁移 | IDLE §6、MERGE §5、§7 |
| 验证边界 | 缺省模式无 WAL / Mongo、实体常驻内存；SIGKILL 恢复要 `ROOST_IDLE_SIM_REAL=1` / `ROOST_MERGE_CASUAL_REAL=1`；提交在途崩溃、多进程与容量未测 | KNOWN-LIMITS“游戏接入验收”两行 |
| 业务要自己补 | 生产公式、离线上限、加速道具；合成规则、挑战、广告平台验签对接、登录票分配与认证、客户端预测与冲突提示；内购、邮件、活动 | IDLE §2、MERGE §8 |

## 6. 常见坑

- 按离线秒数循环追 tick：只在加载后那一次触发里按区间积分（IDLE §8）。
- 让唤醒或读视图续期离线窗口：玩家不上线也能一直累积；窗口只由有效新命令开启（IDLE §4、§8）。
- 把客户端存档当真相：只接受命令，服务器重放（MERGE §9）。
- 用客户端离线时间戳当收益依据：批次里统一用本次请求的服务器时间（MERGE §5）。
- 换序号重试结果未知的请求：用原 Epoch / Seq 查回执（`Sync_IdleReceipt` / `Sync_MergeReceipt`）。

## 7. 验证

- 放置：§3.1 12 条 PASS + SKILL.md §3.4；2026-10-31 在 v1.33.0 检出、自定 module 实测通过。2026-10-31 在 v1.33.2 检出（`e4ec6260`）复测：12 PASS、0 SKIP，§3.4 全过，doctor 仅 4 项 FAIL。
- 合成：§3.2 15 条 PASS。2026-10-31 在 v1.33.1 检出、自定 module `example.com/mymerge` 按 SKILL.md §3.2 + 本节命令 + §3.4 连跑：15 PASS（`game/mergecasual` 3 + `game/handler` 12）、0 SKIP，build / vet / `id check` / `generate --check` 通过，`project doctor` 仅 4 个 collaborators FAIL。同日在 v1.33.0 上执行，唯一错误是 `merge_casual_test.go` 三处 `Sync_TickPlayerTimer` 多传参数。2026-10-31 在 v1.33.2 检出（`e4ec6260`）复测：15 PASS、0 SKIP，§3.4 全过，doctor 仅 4 项 FAIL。仓库脚本另把 `scripts/testdata/merge-casual/persistence/*_test.go` 复制进 `game/handler` 后 `go vet`（只编译），一档 demo 可省。
- 二档：各自 `*_REAL=1`（隔离 Mongo 副本集）SIGKILL 后冷加载、补算一次、下一期限按时投递，第三个进程核对（IDLE §11、MERGE §12）。

## 8. 手册指针

- `docs/framework/IDLE-SIM-INTEGRATION.md`：§4 时间结算、§5 唤醒定时器、§6 幂等、§8 接入坑、§10～§12 验收
- `docs/framework/MERGE-CASUAL-INTEGRATION.md`：§5 离线批次与设备代际、§6 时间、§7 广告与存储格式、§9 接入坑、§11～§13 验收
- `docs/framework/guide/10-time.md` §2A（业务日）、§4（实体定时器）；`docs/framework/SCHEMA-MIGRATION.md`

## v1.35.0 起的公共 Robot 接线

手工生成 idle-sim、merge-casual、auto-chess 时，在最终 `roost generate` 前执行（`genre` 取本篇对应脚本名）：

```sh
python3 "$repo_root/scripts/testdata/robot-common/install.py" "$repo_root" "$project_root" "$genre"
```

该步骤安装业务 PB、已鉴权 TCP → Sender 接线、`loadtest.Commands`、`cmd/robot` 与网络验收；之后仍须正式 generate。自定义 module 由安装器读取 go.mod。最稳妥的完整顺序直接使用同检出的玩法脚本。网络用例现已覆盖该玩法的对局或经营闭环、结算重试与 TCP 重连；可信时间/敌方推进仍在服务器侧。准确步骤见 Robot 手册，不代表完整业务重登或真实存储恢复。

验收数量与限制统一见 [Robot 手册](../../../framework/ROBOT.md)，不要沿用历史版本的通过数。
