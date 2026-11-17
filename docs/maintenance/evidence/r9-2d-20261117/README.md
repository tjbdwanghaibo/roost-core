# R9 批 2D：MOBA 帧同步装模块（wallet）——证据（2026-11-17）

依据：[方案](../../PLAN-2026-11-17-r9-genre-modules.md) §3.13（映射）、§5 表 2D 行（文件范围）、§9（批 0 定型写法）；D5（Lockstep 房间与夹具不迁 `Stepper`）。手册：[MOBA §4.6](../../../framework/MOBA-INTEGRATION.md#46-模块化之后场景语义的变化r92026-11-17)。

## 改法

- **模块清单**：`genre_modules=(wallet)`。`go mod edit -replace` 提前到 `cd "$project_root"` 之后（装模块要按本检出解析），紧接 `modules.sh add`；删掉 `add dao Player --entity Player`；`module verify` 放在 `generate --check` 之后、REAL 段之前。DAO 字段改为追加在各 DAO 末尾（PlayerDao 在 wallet 字段之后），store 键不变（字段名小写）。
- **金币 → wallet**：Player 删 `Gold`。`handlerMOBASettle` 胜者 `winner.Wallet().Add(10)`、`handlerMOBAAward` 胜方 `player.Wallet().Add(10)`、`handlerMOBAProfile` 读 `Wallet().Balance()`。都用 `Add` 不用 `Apply`（§9.3）：两处都有自己的永久事实——双英雄的 MatchResult 终态、5v5 的 `MobaRewards` 64 局回执——wallet 的 128 次窗口不能代替它们。原夹具的金币上限 `Gold > 1000000-10` 换成 wallet 的溢出检查（照塔防）；`Wins` / `Losses` 上限与 `MobaRewards` 上限不变。`IMobaPlayerEntity` 改为嵌生成的 `view.PlayerEntity`。
- **其余不动**（§3.13 / D5）：Wins / Losses、`MobaRewards` 原服奖励账本、lockstep 对局、参考模拟、宿主与恢复、对局目录、结算记录、`service/match` 的 `CommitAcross`。

## 文件

- `scripts/test-moba-generated.sh`
- `scripts/testdata/moba/settlement.go.txt`、`round_handlers.go.txt`、`settlement_test.go.txt`、`round_handlers_test.go.txt`、`persistence/persistence_test.go`、`active/routing_test.go`
- `docs/framework/MOBA-INTEGRATION.md`（开头、§4.1、新 §4.6、§4.3 末段、§5.4、§5.8、§10）
- `docs/agent-skills/roost-genre/references/moba-lockstep.md`（§2、§3 命令块、§6、§7）
- 本目录

## 六条验收

| # | 命令 | 结果 |
| --- | --- | --- |
| 1 | `bash scripts/test-moba-generated.sh` | exit 0；`TestMOBA` 28 PASS、0 SKIP；build / vet / `id check` / `generate --check` 通过；`module verify: wallet 17/17 skipped=0`（[default.txt](default.txt)） |
| 2 | `python3 scripts/test-genres-generated.py --out <scratchpad>/matrix-moba --genres moba` | `status passed`，passed 28 / custom 28 / skipped 0，`custom_module_verify {'wallet': 17}`（[matrix.txt](matrix.txt)） |
| 3 | `source ~/.roost-it/roost-dataengine-it/env.sh` 后 `ROOST_MOBA_REAL=1 ROOST_MOBA_ROUTING=1 bash scripts/test-moba-generated.sh` | 第 1 节列的用例全过、0 SKIP：`TestMOBARealSettlementAcrossProcesses`（write / recover）、`…ActiveRoundCrashRecovery`、`…ActiveRoundSnapshotRecoveryAfterSIGKILL`、`…SnapshotDuringHostHandoff`、`…CrossSIDHostRouting`（[real.txt](real.txt)） |
| 4 | 变异 | 7 个（2 个在真实 Mongo / NATS 上），全部红 → 恢复绿（[mutation.txt](mutation.txt)，脚本 [mutate.py.txt](mutate.py.txt)） |
| 5 | `git status` 无本组残留；`GOWORK=off go test -count=1 -run 'Link\|Anchor\|Doc' .` | 本组链接无错；该用例当时的失败只来自别组尚未提交的证据目录（见交接报告） |
| 6 | 手册 + skill reference | 已改；§3 命令块按 SKILL.md §3.2（自定 module `example.com/mymoba`）原样抽出实跑：28 PASS、0 SKIP、wallet 17/17，§3.4 收尾全过、doctor 无 FAIL；手册三段 Go 示例编译 + vet 通过（[skill-block.txt](skill-block.txt)） |

场景数：缺省 28、REAL 3（结算 write / recover 两段算一条）+ ROUTING 2，**均不变**。

## 旧断言 → 新断言

见 [MOBA §4.6](../../../framework/MOBA-INTEGRATION.md#46-模块化之后场景语义的变化r92026-11-17)：结算 / 回滚 / 无胜者 / 5v5 奖励 / 两条 REAL 的 `Player.Gold` → `Wallet.Coins`（数值不变）；KCP Robot 场景的客户端 PB `Gold` 改由 `Wallet().Balance()` 提供（断言不变）。

## 变异

| # | 改动（保留的生成工程里） | 场景 | 红 | 恢复 |
| --- | --- | --- | --- | --- |
| N1 | 结算删 `winner.Wallet().Add(10)` | `SettlementConcurrentReplayAndIdentity`、`ServerReferenceSettlesTheFinishedRound` | `one result paid / counted more than once`、`reference result did not pay exactly once` | 绿 |
| N2 | `Add(10)` 挪到 `!aborted` 之外 | `NoWinnerSettlementNeverPays` | `cancelled / unresumable match paid win rewards` | 绿 |
| N3 | 生成工程 `WalletState.SetCoins` 的 undo 不还原 | `SettlementRollbackAndRetry` | `partial settlement survived rejection` | 绿 |
| N4 | 原服奖励删 `player.Wallet().Add(10)` | `FiveVFiveAwardPartialDeliveryReplayAndIdentity` | `winner duplicate / missing assets` | 绿 |
| N5 | `MOBAProfile` 的 `Gold` 写 0 | `KCPRobotMatchSettlementAndClientResult` | 客户端收到 `Gold:0` | 绿 |
| N6（REAL） | 结算删 `Add(10)` | `TestMOBARealSettlementAcrossProcesses` | `winner gold=0` | 绿（write + recover） |
| N7（ROUTING） | 原服奖励删 `Add(10)` | `TestMOBARealCrossSIDHostRouting` | `winner paid incorrectly` | 绿 |

变异失败留下的 4 个 `roost_moba_it_mut_*` 库已手动删除。

## 模块缺口

无（与 §3.13 一致），不改 §9.5。

## 拿不准的点

- 方案只说“Gold → wallet”，没说用 `Add` 还是 `Apply`；按 §9.3（自己有永久事实的用 `Add`）选了 `Add`。
- 去掉了原夹具 100 万金币上限（照塔防的先例，换成 wallet 溢出）：其他模块也会往同一个钱包加钱，玩法自己再设上限会让 MOBA 奖励因别处的余额被拒。没有场景测这个上限，条数不受影响。

## 交给协调者写进 CHANGELOG / KNOWN-LIMITS 的话

- CHANGELOG `[Unreleased]`：MOBA 夹具改为装 wallet 模块（R9 批 2D）：结算与 5v5 原服奖励的胜者金币经 `Wallet().Add`（判重仍是 MatchResult 终态与 `MobaRewards` 回执），其余（战绩、奖励账本、lockstep、宿主与恢复、跨分区成局）不变（D5）；场景名与条数（28 + REAL 3 + ROUTING 2）不变，旧断言 → 新断言见 MOBA §4.6；脚本加跑 `module verify`（wallet 17）。
- KNOWN-LIMITS：N1“模块化顺序”里 MOBA 标为已模块化（wallet）；无新增遗留问题。
