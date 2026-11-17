# R9 批 2C：SLG 装模块（wallet）——证据（2026-11-17）

提交：`17952d07`（夹具、脚本、手册、skill reference 与本目录）。

依据：[方案](../../PLAN-2026-11-17-r9-genre-modules.md) §3.12（映射）、§5 表 2C 行（文件范围）、§9（批 0 定型写法）；维护者 Q4 A（联盟活动教学消费者保留）。手册：[SLG §3.5](../../../framework/SLG-INTEGRATION.md#35-模块化之后场景语义的变化r92026-11-17)。

## 改法

- **模块清单**：`genre_modules=(wallet)`，紧跟 `project new` + `go mod edit -replace`，经 `genre-common/modules.sh add` 装；删掉 `add dao Player --entity Player`（game 模板已有）；`module verify` 放在 `generate --check` 之后、联盟纯内存用例与 REAL 段之前。顺序：装模块 → `add timer --entity Player` → add Tile / Remote / AllianceAuthority → 追加字段（`PlayerDao` 末尾，wallet 字段之后）→ 拷夹具 → Robot → generate。
- **金币 → wallet**：Player 删 `Gold`、`Receipts` 两个字段。建造 / 研究 / 抢地扣费经 `slgPay`（余额不足仍答玩法的 `errSLGRule`，否则 `Wallet().Add(-cost)`），取消退款 `Wallet().Add(BuildCost)`，离线收益定时器回调 `Wallet().Add(n)`——都不留回执，“这一次做过没有”仍由目标等级、待完成队列号、地块归属、定时器触发保证（§9.3 写法）。handler 参数 `ISLGEntity` 改为嵌生成的 `view.PlayerEntity` + `TimerComp()`。
- **原服奖励到账**（§3.12“扩展”）：`handlerAllianceCredit` 改为 `Wallet().Apply(allianceRewardKey(key), amount)`。业务键 = `alliance-reward:` + SHA-256(`activity.Key.String()`) 前 16 字节的十六进制（`Key.String()` 带 `/`、可超 64 字节，wallet 只收 64 字节内的 `[A-Za-z0-9_:-]`）。原服义务账本（`game/modules/alliance` 的 `rewards`，Redis versionstore）**不变、仍手写**；Player 侧的 128 条永久回执删掉，由 wallet 回执窗口去重。内部核对 handler `AllianceWallet` 多一个 `key` 参数，返回 `{Home, Coins, Receipts(窗口条数), Receipt{Delta, Balance}}`。
- **联盟教学消费者**（协调器、Redis CAS 联盟、AllianceAuthority Remote 托管、视野、RPC 预算）一行未改（Q4 A）。

## 文件

- `scripts/test-slg-generated.sh`
- `scripts/testdata/slg/tasks.go.txt`、`timer_handlers.go.txt`、`alliance_rewards.go.txt`、`tasks_test.go.txt`、`robot/network_test.go.txt`、`alliance/player_delivery_test.go`
- `docs/framework/SLG-INTEGRATION.md`（开头、§3.1、§3.2、新 §3.5、§4、§7.2、§7.3、§7.6 注释、§11、§12.1）
- `docs/agent-skills/roost-genre/references/slg.md`（§2、§3、§3.1 命令块、§4、§6、§7）
- 本目录

## 六条验收

| # | 命令 | 结果 |
| --- | --- | --- |
| 1 | `bash scripts/test-slg-generated.sh` | exit 0；`TestSLG` 7 PASS、联盟纯内存 2 PASS、0 SKIP；build / vet / `id check` / `generate --check` 通过；`module verify: wallet 17/17 skipped=0`（[default.txt](default.txt)） |
| 2 | `python3 scripts/test-genres-generated.py --out <scratchpad>/matrix-slg --genres slg` | `status passed`，`custom_module_verify {'wallet': 17}`，passed 7 / custom 9（7 + 联盟纯内存 2，矩阵按 `*.log` 计）/ skipped 0（[matrix.txt](matrix.txt)） |
| 3 | `source ~/.roost-it/roost-dataengine-it/env.sh` + 持 `remote-acceptance.lock` + `ROOST_SLG_REAL=1 bash scripts/test-slg-generated.sh` | 四条 REAL 全过、0 SKIP：`TestSLGAllianceActivity`、`…PlayerDelivery`、`…RemoteDelivery`、`…RPCReceiverBudget`（[real.txt](real.txt)） |
| 4 | 变异 | 5 个，全部红 → 恢复绿（[mutation.txt](mutation.txt)，脚本 [mutate.py.txt](mutate.py.txt)） |
| 5 | `git status` 无本组残留；`GOWORK=off go test -count=1 -run 'Link\|Anchor\|Doc' .` | 提交后本组路径 `git status` 无残留；文档用例里本组链接无错，当时的失败只来自别组尚未提交的证据目录（r9-1a / r9-1d / r9-2b，见交接报告） |
| 6 | 手册 + skill reference | 已改；命令块按 SKILL.md §3.2（自定 module `example.com/myslg` / `myslgadv`）原样抽出实跑：§3 7 PASS + wallet 17/17，§3.1 进阶 7 + 2 PASS + wallet 17/17，§3.4 收尾全过、doctor 无 FAIL；手册 §3.3 / §7.6 示例编译 + vet 通过（[skill-block.txt](skill-block.txt)） |

场景数：缺省 7 + 2、REAL 4，**均不变**。

## 旧断言 → 新断言

见 [SLG §3.5](../../../framework/SLG-INTEGRATION.md#35-模块化之后场景语义的变化r92026-11-17)。摘要：七条 `TestSLG` 的 `Player.Gold` → `Wallet.Coins`（数值不变）；`TestSLGAlliancePlayerDelivery` 冷加载钱包从 `{Gold 10, Receipts{窗口:10}}` 改为 `{Coins 10, wallet 回执窗口 1 条，该键 Delta 10 / Balance 10}`，改金额被拒从夹具规则错误变成 `wallet_conflict`（仍断言 `e != nil`）。

## 变异

| # | 改动（保留的生成工程里） | 场景 | 红 | 恢复 |
| --- | --- | --- | --- | --- |
| M1 | 取消不退款（删 `Wallet().Add(BuildCost)`） | `CancelRefundAndRefusal` | `refund duplicated or canceled construction fired` | 绿 |
| M2 | `slgPay` 只查余额不扣 | `ConstructionMarchAndReplay`、`ResearchAndConstruction…`、`TwoPlayersCompete…`、`RobotBusiness` | 四条 FAIL（`gold=50`、`payment was duplicated or lost` 等） | 绿 |
| M3 | 离线收益定时器不调 `Wallet().Add` | `OfflineIncomeIsBusinessBounded` | `offline income gold=50, want 60` | 绿 |
| M4（REAL） | 到账 `Wallet().Apply(业务键)` 换成不带键的 `Wallet().Add` | `TestSLGAlliancePlayerDelivery` | recover 子进程 `cold wallet receipt or asset missing {1000 10 0 {0 0}}` | 绿 |
| M5 | 生成工程 `WalletState.SetCoins` 的 undo 不还原 | `FailureRollsBackResourcesAndDeadline` | `rollback left a charge or timer` | 绿 |

M4 第一次恢复跑红过一次：复用了变异那次留下的库与 WAL（冷数据里没有回执），不是代码问题；改成每次新库 + 新 WAL 后重跑，变异红、恢复绿，证据里只留重跑结果。变异失败留下的 6 个 `roost_slg_it_m4_*` / `roost_slg_it_mut_*` 库已手动删除。

## 模块缺口

无新缺口（与 §3.12“模块缺口：无”一致），不改 §9.5。两点按 wallet README 现行边界处理、记在手册里：

- wallet 业务键只收 64 字节内 `[A-Za-z0-9_:-]`，结构化的活动身份要先压成键（夹具用 SHA-256 前 16 字节）。
- wallet 回执窗口不是永久账本：原夹具 Player 侧“128 条永久回执、满了拒新身份”换成“最近 128 次带键加减、淘汰最旧”。“只发一次”现在 = 原服义务账本记 Delivered 之后不再重投 + 记之前的重投由 wallet 窗口去重；补投必须在这名玩家又发生 128 次带键加减之前完成。

## 拿不准的点

- 方案“义务账本仍然手写”我理解为原服的 `rewards`（Redis）账本；Player 上的 `Receipts` 永久回执视为被 `Apply` 取代并删除。若维护者要两者都留（Player 永久回执 + wallet `Add`），改法是恢复 `Receipts` 字段并把 `Apply` 换回 `Add`，与 §9.3“有自己事实的用 Add”更一致，但与 §3.12 的“`Apply(业务键)`”字面不符。
- `slgPay` 在余额不足时返回玩法的 `errSLGRule` 而不是 wallet 的 `wallet_short`（保持原夹具语义：场景只断言被拒）。
- 夹具的 `game/modules/alliance/` 与 alliance 模块同路径（方案风险 8）：本轮不装 alliance，只在手册 §3.5 写明“以后要装先改名”，没有改包名（改名会动 REAL 四条的包路径与脚本，超出本批）。
- skill reference §3.1 的 `dbscope` 命令错误（R9 之前就有）顺手改了；同类问题其它 reference 未查。

## 交给协调者写进 CHANGELOG / KNOWN-LIMITS 的话

- CHANGELOG `[Unreleased]`：SLG 夹具改为装 wallet 模块（R9 批 2C）：建造 / 研究 / 抢地扣费、取消退款、离线收益、原服奖励到账都经 `Wallet()`；到账用 `Apply(活动身份的哈希键)`，Player 侧手写的永久到账回执删除，原服义务账本与联盟教学消费者保留；场景名与条数（7 + 2 + REAL 4）不变，旧断言 → 新断言见 SLG §3.5；脚本加跑 `module verify`（wallet 17）。
- KNOWN-LIMITS：N1“模块化顺序”里 SLG 标为已模块化（wallet）；联盟活动按 Q4 A 留手写。无新增遗留问题。

## 维护者决定后的修正（2026-11-17）

**问题**：上面的改法把原服奖励到账换成 `Wallet().Apply(业务键)`、删掉了 Player 上的永久到账回执，判重只剩 wallet 最近 128 次带键加减的窗口。原服还没记 Delivered 就重投、而这期间这名玩家又有超过 128 笔带键入账时，这次活动的回执已被淘汰，金币会重复发。

**维护者决定**：恢复 Player 上的永久到账回执，入账改回 `Wallet().Add`，跨服金币严格只发一次（与 §9.3“有自己事实的用 Add”、wallet README“长期只发一次由调用方记事实并改用 Add”一致；上文“拿不准的点”第一条即此）。

### 改法

- `alliance_rewards.go.txt`：照 `17952d07^` 的写法恢复永久回执——字段按 §9.2 加玩法前缀，`PlayerDao.SLGReceipts string store:"slg_receipts" dao:"nosync"`（JSON：活动身份 `Key.String()` → 金额）；建档写 `{}`；到账先查回执：同身份同金额返回当前余额、不加钱，同身份改金额拒绝，满 128 条拒新身份、不淘汰旧身份；没有才 `Wallet().Add(amount)` 再写回执（溢出由 wallet 答 `wallet_overflow`，原样返回、整笔回滚）。删掉 `allianceRewardKey`（SHA-256 业务键）与 `Apply`。
- 内部核对 handler `AllianceWallet` 去掉 `key` 参数，返回 `{Home, Coins, Receipts}`（`Receipts` 是永久回执 JSON）；`player_delivery_test.go` 冷加载断言改回“永久回执恰好一条、该身份金额 10”，`Coins` 仍读 wallet。
- `test-slg-generated.sh`：Player 字段加 `SLGReceipts`；拷新回归文件；`TestSLG` 断言 7 → 8。
- 新回归 `scripts/testdata/slg/alliance_rewards_test.go.txt` → `game/handler/alliance_rewards_test.go`：`TestSLGAllianceRewardOnceBeyondWalletWindow`（缺省档，替身提交器，复用 `newSLGFixture`）。单独成文件是因为 skill reference §3 基础版不拷联盟 handler，放进 `tasks_test.go.txt` 会让基础版编译不过。

### 场景数

缺省档 `TestSLG` **7 → 8**，多的就是上面这条回归；联盟纯内存 2、REAL 4 不变（矩阵 passed 8 / custom 10）。同步：脚本断言、[SLG §3.4](../../../framework/SLG-INTEGRATION.md#34-八个场景各学什么)（标题“七个”→“八个”并加一行）、§3.5、§11；skill reference §3.1 期望（8 + 2 = 10）与 §7；[方案](../../PLAN-2026-11-17-r9-genre-modules.md) §1 表 SLG 行。skill reference §3 基础版仍是 7（不含联盟 handler）。

### 旧断言 → 新断言

| 场景 | 2C 第一版断言 | 修正后断言 |
| --- | --- | --- |
| `TestSLGAlliancePlayerDelivery`（REAL） | 冷加载 `{Home, Coins 10, wallet 回执窗口 1 条，该键 Delta 10 / Balance 10}`；改金额由 wallet 答 `wallet_conflict` | `{Home, Coins 10, Receipts{窗口: 10}}`：永久回执恰好一条、金额 10；改金额由回执检查拒绝（仍断言 `e != nil`） |
| `TestSLGAllianceRewardOnceBeyondWalletWindow`（新增） | — | 到账 10（余额 60）→ `WalletGrant` 129 笔不同业务键（余额 189）→ 同身份重投余额仍 189 → 同身份改金额被拒、余额仍 189 |

### 修前红 / 修后绿 / 变异

| 步骤 | 命令 | 结果 |
| --- | --- | --- |
| 修前红 | 只加回归，夹具仍是 2C 第一版：`bash scripts/test-slg-generated.sh` | exit 1；`redelivery after 129 keyed credits paid again: coins=199, want 189`，其余 7 条 PASS（[receipt-red.txt](receipt-red.txt)） |
| 修后缺省档 | `bash scripts/test-slg-generated.sh` | exit 0；`TestSLG` 8 PASS、联盟纯内存 2 PASS、0 SKIP；build / vet / `id check` / `generate --check`；`module verify: wallet 17/17 skipped=0`（[receipt-default.txt](receipt-default.txt)） |
| 矩阵 | `python3 scripts/test-genres-generated.py --out <scratchpad>/matrix-slg --genres slg` | `status passed`，`custom_module_verify {'wallet': 17}`，passed 8 / custom 10 / skipped 0（[receipt-matrix.txt](receipt-matrix.txt)） |
| REAL | `source ~/.roost-it/roost-dataengine-it/env.sh` + 持 `remote-acceptance.lock` + `ROOST_SLG_REAL=1 bash scripts/test-slg-generated.sh` | exit 0；四条 REAL 全过、0 SKIP（[receipt-real.txt](receipt-real.txt)） |
| M6 变异 | 保留工程里删掉 `handlerAllianceCredit` 的永久回执检查（`if old, ok := receipts[...]` 整段） | 回归红 `coins=199, want 189`；恢复后绿（[receipt-mutation.txt](receipt-mutation.txt)） |
| 命令块 | SKILL.md §3.2 + reference §3 / §3.1 原样抽出执行（自定 module `myslg` / `myslgadv`） | §3：7 PASS + wallet 17/17；§3.1：8 + 2 PASS + wallet 17/17；0 SKIP（[receipt-skill-block.txt](receipt-skill-block.txt)） |

上文变异表的 M4（`Apply` 换成不带键的 `Add`）针对的是第一版写法，修正后不再适用；修正后的对应变异是 M6。

### 交给协调者写进 CHANGELOG / KNOWN-LIMITS 的话（替换上面那两条里与到账有关的部分）

- CHANGELOG `[Unreleased]`：SLG 原服奖励到账改回 Player 永久到账回执（`SLGReceipts`）+ `Wallet().Add`（维护者 2026-11-17）：2C 第一版用 `Wallet().Apply` 判重只有 128 条窗口，原服记 Delivered 之前重投、期间玩家又有超过 128 笔带键入账时会重复发金币；新增缺省档回归 `TestSLGAllianceRewardOnceBeyondWalletWindow`，`TestSLG` 7 → 8；内部核对 handler `AllianceWallet` 去掉 `key` 参数。
- KNOWN-LIMITS：“游戏接入验收”表 SLG 行“脚本断言 7 条”改为 8 条（多一条原服奖励越过 wallet 窗口仍只发一次的回归）。无新增遗留问题。
