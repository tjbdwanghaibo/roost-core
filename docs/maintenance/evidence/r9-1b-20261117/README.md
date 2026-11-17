# R9 批 1（agent 1B）：放置模拟、合成休闲改为装模块（2026-11-17）

方案：[PLAN-2026-11-17-r9-genre-modules](../../PLAN-2026-11-17-r9-genre-modules.md) §3.4、§3.5、§5 的 1B 行，写法照[第 9 节](../../PLAN-2026-11-17-r9-genre-modules.md#9-批-0-定型的写法批-1--批-2-照做)；维护者 Q1～Q8 按推荐（Q1 A：合成的体力跟 stamina、棋盘规则仍冻结；Q2 B：放置的宝石做 bag 道具；Q6 只装 enter；Q8 不建配方）。在主检出 main 上工作，同一检出里另有 1A / 1C / 1D、R3（terrain / rehome / mailbox / demo / DAO 生成器）的 agent 在改别的文件；本批生成工程按当时工作区编译、全部通过。

## 改了什么

| 文件 | 改法 |
| --- | --- |
| `scripts/test-idle-sim-generated.sh` | 装 `enter wallet bag checkin`（`genre-common/modules.sh add`）；删 `add dao Player`；`add timer --entity Player`；`PlayerDao` 末尾追加 `IdleState` / `IdleWakeTimer`（原 `State` / `WakeTimer`，加玩法前缀）；`configs/table/item.csv` 追加宝石 `1101,宝石,0`；拷 `first_enter.go.txt`；末尾跑 `modules.sh verify` |
| `scripts/testdata/idle-sim/**` | `State` 去掉 `Gold` / `Gems` / `DailyDay`，`Receipt` 去掉金币快照；`Apply(c, now, Purse)` 返回 `Effect{Gold, Gems, Daily}`，handler 在同一笔里调 `Wallet().Add`、`Bag().Remove`、`Checkin().Claim`；放置建档要求 `Enter().Built()`，同一笔经 bag 发初始 10 颗宝石；新增 `first_enter.go.txt`（`event.EnterFirst` 的 Player 监听：wallet 发 100 金币）；测试改读模块状态、`refuse` 另核对模块资产不变、fixture 先在建档前调一次 `IdleInit` 必须被拒；REAL 用例加 enter 建档与宝石道具表，WAL 计数 6 → 7、最终金币 129 → 134 |
| `scripts/test-merge-casual-generated.sh` | 装 `enter wallet stamina`（stamina 连带 rules）；删 `add dao Player`；`PlayerDao` 末尾追加 `MergeState` / `MergeWakeTimer`；拷 `first_enter.go.txt`；末尾跑 `modules.sh verify` |
| `scripts/testdata/merge-casual/**` | `Rules` 去掉 `EnergyCap` / `EnergyMillis`（体力规则不再冻结），`State` 去掉 `Gold` / `Energy` / `EnergyAt`，`Receipt` 去掉金币与体力快照；`Apply(c, now, Purse)` 返回 `Effect{Gold, Energy}`，handler 逐条调 `Wallet().Add`、`Stamina().Spend` / `Grant`（`Granted` 不足整笔拒绝）；广告每日次数改按 `clock.BusinessDay`（借鉴 checkin）、订单号格式借鉴 wallet 业务键；`handlerMergeView` 加 `nowUnixMilli` 算当前体力；登录建棋盘要求 `Enter().Built()`；新增 `first_enter.go.txt`（wallet 发 30 金币）；测试、REAL 用例与 Robot 改读模块状态，WAL 计数 6 → 7 |
| `docs/framework/IDLE-SIM-INTEGRATION.md`、`MERGE-CASUAL-INTEGRATION.md` | 开头、§2 归属表、§3.1 步骤、§3.2 阅读顺序、§3.3 示例前提、§3.4 场景、新增 §3.5“旧断言 → 新断言”、时间 / 幂等 / 存储格式节、验收节（`module verify` 条数、REAL 计数与金币）；§3.3 的可编译示例在生成工程里 `go vet` 通过 |
| `docs/agent-skills/roost-genre/references/idle-merge-casual.md` | §2 模块清单与归属、§3.1 / §3.2 装模块命令块、§4、§5 模块边界、§7 验证 |
| `docs/maintenance/PLAN-2026-11-17-r9-genre-modules.md` | 只在 §9.5 追加 1B 一行 |

场景数：`TestIdleSim` 13 条（3 规则 + 9 Nest + 1 Robot）、`TestMergeCasual` 16 条（3 规则 + 12 Nest + 1 Robot）不变；REAL 各 1 条不变。新增模块场景（`module verify`）：放置 bag 20、checkin 8、enter 29、wallet 17；合成 enter 29、rules 10、stamina 26、stamina+rules 2、wallet 17。

## 验收（方案第 5 节六条）

| # | 命令 | 结果 | 摘要 |
| --- | --- | --- | --- |
| 1 | `bash scripts/test-idle-sim-generated.sh`；`bash scripts/test-merge-casual-generated.sh` | 两者 exit 0：13 / 16 PASS、0 SKIP；build / vet / `id check` / `generate --check` 通过；`module verify` 每个模块 n/n、0 skip | [default.txt](default.txt) |
| 2 | `python3 scripts/test-genres-generated.py --out <scratchpad>/b1117-r9-1b/matrix --genres idle-sim merge-casual` | 两类 `status passed`，自定义 module 名下 13 / 16 条再过，`custom_module_verify` 与第 1 条相同 | [matrix.txt](matrix.txt) |
| 3 | `source ~/.roost-it/roost-dataengine-it/env.sh; ROOST_IDLE_SIM_REAL=1 …`、`ROOST_MERGE_CASUAL_REAL=1 …` | 两者 exit 0：缺省全部 + `Test*RealCrashRecovery` PASS（各 7 条未投影记录，SIGKILL 后恢复、补触发一次、第三进程重放不重复），0 SKIP，库已删 | [real.txt](real.txt) |
| 4 | 变异（保留的生成工程里改 handler / 监听 / 业务包，跑对应场景，再恢复） | 放置 10 个、合成 9 个、REAL 各 1 个，全部变红，恢复后全绿 | [mutation.txt](mutation.txt) |
| 5 | `git status -- <本批文件>`；`GOWORK=off go test -count=1 -run 'Link\|Anchor\|Doc' .` | 本批文件提交后无残留；文档测试见下“未过的根包测试” | — |
| 6 | 手册 + skill reference 同步；skill 命令块真跑（接 SKILL.md §3.2 前置与 §3.4 收尾，自定 module `example.com/acme/myidle`、`example.com/acme/mymerge`） | 13 / 16 PASS、0 SKIP，`module verify` 全部 n/n，`project doctor` 无 FAIL | [skill-block.txt](skill-block.txt) |

变异清单（第 4 条；“空转检查”一项是把测试自己的拒绝 handler 改成放行，证明新加的模块状态断言不是恒真）：

| 变异 | 改法 | 变红的场景 |
| --- | --- | --- |
| I1 去掉 wallet | `Effect.Gold` 不交给 `Wallet().Add` | `WarehouseCapacity…`、`UpgradeAndBoost…`、`ConcurrentAdmission…` |
| I2 去掉 bag 扣宝石 | 不调 `Bag().Remove` | `UpgradeAndBoost…` |
| I3 每日奖励不经 checkin | `Checkin().Claim` 换成 `Wallet().Add(5)`（旧夹具写法） | `ConcurrentAdmission…`、`DailyBoundary…` |
| I4 吞掉 checkin 的拒绝 | `checkin_done` 时照样提交 | `DailyBoundary…`（同日第二次被接受） |
| I5 去掉初始金币 | `OnEnterFirst` 不调 wallet | `WarehouseCapacity…`、`UpgradeAndBoost…`、`ConcurrentAdmission…`、`DailyBoundary…` |
| I6 去掉初始宝石 | 放置建档不调 `Bag().Add` | `UpgradeAndBoost…`、`UndoRestores…` |
| I7 不要求已建档 | 去掉 `Enter().Built()` | 全部 10 条 Nest / Robot 场景（fixture 的建档前 `IdleInit` 被接受） |
| I8 空转检查 | 撤销用例的拒绝 handler 改为放行 | `UndoRestores…` |
| I9 严格解码 | 去掉 `Raw` 下界（替代原 `Gold:-1` 样本） | `InvalidSnapshots…` |
| I10 业务上限 | 领取不按 wallet 余额判上限 | `InvalidSnapshots…` |
| M1 去掉 wallet | `Effect.Gold` 不交给 `Wallet().Add` | `WarehouseAndUnlock…`、`SellUndo…`、`Orders…`、`DeviceTakeover…`、`ChallengeServerWin…`、`RobotBusiness` |
| M2 去掉 stamina 扣体力 | 不调 `Stamina().Spend` | `ProducerReplay…`、`ChallengeServerWin…`、`OfflineRecovery…`、`AdProof…` |
| M3 去掉 stamina 发体力 | 广告不调 `Stamina().Grant` | `AdProof…` |
| M4 去掉初始金币 | `OnEnterFirst` 不调 wallet | `WarehouseAndUnlock…`、`SellUndo…`、`Orders…`、`DeviceTakeover…`、`ChallengeServerWin…`、`PersistentDeadline…` |
| M5 不要求已建档 | 去掉 `Enter().Built()` | 全部 13 条 Nest / Robot 场景 |
| M6 空转检查 | 撤销用例的拒绝 handler 改为放行 | `UndoRestores…` |
| M7 广告次数 | 去掉每业务日 3 次上限 | `AdProof…` |
| M8 严格解码 | 去掉 `Charges` 下界（替代原 `Gold:-1` 样本） | `MalformedState…` |
| M9 业务上限 | 出售不按 wallet 余额判上限 | `BoundedLedger…` |
| I-REAL / M-REAL | 每日奖励换回 `Wallet().Add(5)` / 去掉 wallet | `TestIdleSimRealCrashRecovery`（recovery changed economy）/ `TestMergeCasualRealCrashRecovery`（recovered claim …） |

旧断言 → 新断言的逐条表在手册 [IDLE §3.5](../../../framework/IDLE-SIM-INTEGRATION.md#35-模块化之后场景语义的变化r92026-11-17)、[MERGE §3.5](../../../framework/MERGE-CASUAL-INTEGRATION.md#35-模块化之后场景语义的变化r92026-11-17)。

## 未过的根包测试（不是本批文件引起的）

`GOWORK=off go test -count=1 -run 'Link|Anchor|Doc' .`：`TestTrackedMarkdownRelativeLinksResolve` 在提交前报本批两份手册指向本目录 README 的链接（未跟踪），提交后消失；同一测试另报 1C 的两处链接（`r9-1c-20261117`，1C 未提交）。`TestGeneratedProjectRealDependencyTestsHaveAnEntry` 报 `demo/internal/service/game/terrain_test.go.tmpl` 的 `TestTerrainMapOwnershipRealMongo` 没有入口——R3 agent 的未提交文件，与本批无关。

## 发现的模块缺口（按 Q1 只记录，没有改 modules/**；方案 §9.5 已追加一行）

1. **enter 的场景不装道具表，`EnterFirst` 监听里发 bag 道具会让 enter 的 13 条场景答 `bag_item_invalid`**：放置第一版把初始宝石写在监听里，`module verify` 的 enter 只过 16/29。改为在建放置存档那一笔里 `Bag().Add`。1C 卡牌碰到同一件事（改道具规则绕过），两类玩法都要，可能满足 Q1 的 B 条件，等维护者定。
2. **`event.EnterFirst` 只带 `NowUnixMilli`**：放置的冻结规则（服务端捕获的配置快照）、合成的棋盘（服务端密钥 + 规则）建不进监听，方案 §3.4 / §3.5 写的“初始存档 / 初始棋盘写在 `EnterFirst`”做不到；按 compose 规则 4 提出：初始金币进监听，玩法存档仍由玩法自己的建档 handler 建、要求 `Enter().Built()`。
3. **stamina**：规则不按存档冻结（方案已定不补）；`Grant` 到硬顶静默截断，合成要求 `Granted` 等于要发的点数、否则整笔拒绝；`rules.go` 是“留给工程改”的文件，但 stamina 自带 26 条场景按缺省数（100 / 999 / 5 分钟）断言，工程一改数 `module verify` 就红——合成因此用缺省数，体力的样例数值全变（上限 10 → 100、3 秒 → 5 分钟）。
4. **checkin**：奖励规则写死在它自带的两条场景里（README 让工程同时改那两条），放置不改模块文件、改用 checkin 的连续天数加成（10×min(n,7)），没有按方案“配平”成原来的平 5。

## 拿不准的点

1. **方案 §3.4 / §3.5 的“EnterFirst 写初始存档 / 棋盘”没有照做**（见缺口 2）。现在是 enter 建档 + 玩法建档两步，两步都 strict；建档之后、玩法建档之前崩溃时，下次玩法建档照常补上（玩法存档为空就建），不会重复发金币（金币在 enter 那一笔）。
2. **checkin 的数额**：方案说“连续加成在 checkin.go 配平”，我理解为“要么改模块文件，要么跟模块语义”；改模块文件会让 checkin 自带场景红，所以选了跟模块语义（Q1 A）。如果维护者要平 5，需要同时改工程里 checkin 的两条自带场景（README 允许），`module verify` 仍 8/8。
3. **合成的体力数值**：跟 stamina 缺省规则后，合成的体力相关断言全部换了数（手册 §3.5 有表）；“离线恢复保留零头”改为在 stamina 的周期（5 分钟）上验证。
4. **玩法字段改名** `State` → `IdleState` / `MergeState`、`WakeTimer` → `IdleWakeTimer` / `MergeWakeTimer`（store 键同改）：按 §9.2“字段名带玩法前缀”的意思推到 Player 上，避免与模块字段混读；按“未部署不做兼容”，旧工程的库不迁移。
5. **合成广告的每日次数**从“UTC 毫秒整除”改为 `clock.BusinessDay`（YYYYMMDD），缺省配置下同一天；`AdReceipt.Day` 的取值含义随之变化（未部署，不迁移）。

## 交给协调者写的内容（本批没改 CHANGELOG / KNOWN-LIMITS）

CHANGELOG `[Unreleased]` 建议加：

- 变更：放置模拟、合成休闲玩法夹具改为“装模块 + 手写玩法核心”（R9 批 1B）。放置装 `enter wallet bag checkin`：金币走 wallet、宝石是 bag 道具 1101（Q2 B）、每日奖励走 checkin（连续天数加成）、建档走 enter；生产队列与收益积分手写（借鉴 idle 模块）。合成装 `enter wallet stamina`：金币走 wallet、体力跟 stamina（Q1 A，不再随棋盘冻结，缺省上限 100、每 5 分钟 1 点）；棋盘、生产器、挑战、广告手写，广告每日次数按业务日。两类场景名与条数（13 / 16）不变，旧断言 → 新断言见 IDLE / MERGE 手册 §3.5；REAL 用例未投影 WAL 计数 6 → 7；玩法 DAO 字段改名 `IdleState` / `MergeState`（store 键同改）。

KNOWN-LIMITS：

- “模块化顺序（N1）”一条（收尾改写时）：放置（`enter wallet bag checkin`）、合成（`enter wallet stamina`）已按 R9 改造。
- “尚未确认”候选（维护者定）：enter 的场景不装道具表，`EnterFirst` 里发 bag 道具会让它们失败（1B、1C 两类都碰到）；`EnterFirst` 不带配置快照，要冻结配置的玩法存档建不进监听。
- “游戏接入验收”放置 / 合成两行：缺省档另有 `module verify`（放置 bag 20 / checkin 8 / enter 29 / wallet 17；合成 enter 29 / rules 10 / stamina 26 / stamina+rules 2 / wallet 17）；REAL 2026-11-17 复跑通过。
