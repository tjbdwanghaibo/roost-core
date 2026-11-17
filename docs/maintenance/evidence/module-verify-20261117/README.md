# module verify 两项行为修正（2026-11-17，R9 §9.5 维护者决定）

来源：[PLAN-2026-11-17-r9-genre-modules §9.5](../../PLAN-2026-11-17-r9-genre-modules.md#95-试点发现的模块缺口按-q1-只记录不在-r9-改模块) 里 1C / 1B / 1A 记录的两条，维护者 2026-11-17 定为要改：

1. **模块场景测试先装工程的配置表**：卡牌（1C）、放置（1B）在 enter 的 `event.EnterFirst` 监听里发工程道具表里的 bag 道具；`roost module verify enter` 跑 enter 自带场景时进程里没有装载工程的 `configs/data`，29 条里 13 条答 `bag_item_invalid`。两类当时各绕了一下：卡牌在 bag 的规则入口 `items.go` 里认下 2001 / 2002，放置把初始宝石挪出监听、改在放置建档那一笔发。
2. **module verify 只统计模块自带文件里的用例**：verify 按清单的 `run` 前缀（turn 是 `^TestTurn`）选场景，工程里同前缀的测试被算进模块条数；回合 PvP（1A）的 `TestTurnPVPRealTimerRecovery` 缺省档 SKIP，让 turn 的校验失败过一次，1A 只好给它加构建标签 `turnreal`。

提交：见文末“提交”。

## 根因

1. 模块测试没有公共夹具：164 个模块测试模板各自起 Nest、各自装需要的测试表（bag、task、shop、activity……用 `configdata.NewRegistry` + `Load`，结束时换回原来的缺省 Store），没有任何一处按正式启动的方式装载工程的 `configs/data`。正式进程是 `internal/registry.RegisterAll`（config 阶段 = `generated.RegisterConfigData`）+ configdata Mod 的 `NewStore(DefaultRegistry(), "configs/data").Load`；测试进程里这两步都没有，`configdata.DefaultStore()` 为 nil，`generated.ItemByID` 查不到任何道具，`EnterFirst` 监听的 `Bag().Add(2001, …)` 答 `bag_item_invalid`，enter 建档整笔回滚。
2. `VerifyModules`（`codegen/internal/roost/module.go`）直接把清单的 `verify.run` 交给 `go test -run`，在 `verify.packages` 里跑：同包里任何名字匹配前缀的测试——不管是不是模块装进来的——都会运行并被 `countModuleScenarios` 计数（通过算进 passed，跳过算进 skipped）。

## 改法

| 位置 | 改动 |
| --- | --- |
| `codegen/internal/roost/module.go` | `VerifyModules` 每一项（模块自己的、联动 `<a>+<b>`）先从 `roost.yaml` 记下的文件（`modules.<a>.files`；联动是 `modules.<a>.with.<b>`）里读出测试：`_test.go`、`go/build` 按当前 GOOS / GOARCH 与构建约束会编译的（mailbox / purchase 带 `integration` 标签的真实依赖用例照旧不在其中）、名字匹配清单 `run` 的顶层 `TestXxx(t *testing.T)`；再拼成精确的 `-run '^(A|B|…)$'`。读不到任何用例、文件被删时这一项失败。条数口径（顶层 PASS、任一层 SKIP）不变 |
| `codegen/internal/roost/module_testconfig.go`（新） | 模块场景的公共夹具 `roost_config_data_test.go` 的源码与写入：包内测试文件，`init` 里 `generated.RegisterGeneratedConfigData(configdata.NewRegistry())` 登记工程全部表（与正式启动同一份登记；不用缺省 Registry，避免与测试里别处的登记重复），再 `configdata.NewStore(registry, <到工程根的相对路径>/configs/data).Load`——装载即发布为进程缺省 Store 与新请求钉住的快照。装载失败直接 panic（正式启动同样会失败） |
| `codegen/internal/roost/module_install.go` | `AddModules` 收集这次写入的模块测试文件与联动测试文件所在的包，包里还没有夹具的写一份（包名取同目录模块测试文件的包名，去掉 `_test`）；夹具与模块文件同一个备份 / 回滚边界；已有的不动，不记在任何模块名下（之后归工程所有） |
| `codegen/internal/roost/module_verify_test.go`（新） | 两条回归用例（下文） |
| 卡牌夹具 `scripts/test-card-rpg-generated.sh` | 删掉给 `game/components/bag/items.go` 打补丁的那段（2001 / 2002 只靠道具表） |
| 放置夹具 `scripts/testdata/idle-sim/**` | 初始宝石搬回 `EnterFirst` 监听（`first_enter.go.txt`：wallet 100 金币 + bag 10 宝石同一笔）；`handlerIdleInit` 只建放置存档、不再 `Bag().Add`；注释同步 |
| 回合 PvP `scripts/testdata/turn-pvp-real/timer_test.go.txt`、`scripts/test-turn-pvp-generated.sh` | 去掉 `//go:build turnreal` 与脚本里的 `-tags turnreal`；缺省档照样编译 / vet，REAL 档照样 `-run '^TestTurnPVPRealTimerRecovery$'` 运行 |
| 文档 | `modules/README.md`（清单示例、规则两条）、`codegen/docs/CODEGEN_REFERENCE.zh-CN.md`（verify 一行、安装第 3 步）、`docs/framework/CARD-RPG-INTEGRATION.md`、`IDLE-SIM-INTEGRATION.md`、`SESSION-GENRES.md`、skill 参考 `docs/agent-skills/roost-genre/references/card-turn-based.md`、`idle-merge-casual.md` |

没有改任何 `modules/*/files/**` 模板：配置装载放在安装器写的包级夹具这一处，不在每个模块的测试里各改一遍。模块自己装测试表的用例照旧（结束时换回的是工程的表）；bag 的“没有装载配置”用例本来就显式 `SetDefaultStore(nil)`。

## 回归用例：修前红 / 修后绿 / 变异

`codegen/internal/roost/module_verify_test.go`：

- `TestModuleVerifyLoadsTheProjectConfigData`：生成工程装 `enter bag`，`item.csv` 加 `2001,钻石,0`，`game/entities/player/gems.go` 写一个 `EnterFirst` 监听 `Bag().Add(2001, 10)`，`roost generate` 后 `VerifyModules` 要求 enter 29、bag 20、0 skip。
- `TestModuleVerifyCountsOnlyTheModulesOwnTests`：装 `enter profile`，在 `game/handler` 写工程自己的 `TestEnterProjectRealDependencies`（缺省 SKIP）与 `TestEnterProjectOwnRule`（PASS），在 `game/modules/login` 写 `TestWithEnterProfileProjectOwn`（SKIP）；要求 enter 29、enter+profile 3、profile 21、profile+enter 4，全部 0 skip。

命令：`GOWORK=off go test -count=1 -run '^TestModuleVerify(LoadsTheProjectConfigData|CountsOnlyTheModulesOwnTests)$' -v ./codegen/internal/roost/`

| 阶段 | 结果 | 文件 |
| --- | --- | --- |
| 修前 | 两条都 FAIL：`enter:[16 0]`（13 条 `bag_item_invalid`）；`enter:[30 1]`、`enter+profile:[3 1]` | [red-codegen.txt](red-codegen.txt)；同一情形在生成工程里直接 `go test` 的输出（13 条 FAIL 全是 `bag_item_invalid: unknown item [item_id=2001]`）：[red-enter-direct.txt](red-enter-direct.txt) |
| 修后 | 两条都 PASS | [green-codegen.txt](green-codegen.txt) |
| 变异 M1 | 去掉 `AddModules` 里 `writeModuleConfigFixtures` 的调用：`TestModuleVerifyLoadsTheProjectConfigData` FAIL，`enter:[16 0]` | [mutation-codegen.txt](mutation-codegen.txt) |
| 变异 M2 | `-run` 换回清单前缀 `scenario.verify.Run`：`TestModuleVerifyCountsOnlyTheModulesOwnTests` FAIL，`enter:[30 1]`、`enter+profile:[3 1]` | 同上 |

两处变异都已撤回。

真实情形的修前 / 修后（回合 PvP）：同一个去掉标签的生成工程上，修前的 roost（main 构建）`module verify turn` 答 `FAIL module turn: 25 passed (want 25), 1 skipped`（工程的 `TestTurnPVPRealTimerRecovery`），修后 `ok module turn: 25 passed, 0 skipped`：[turn-pvp-verify-before-after.txt](turn-pvp-verify-before-after.txt)。

## 条数

- 全部模块：全新 `-template game` 工程一次装 42 个模块（`scripts/test-modules-generated.sh` 的 `all_modules`），`roost module verify --json` 90 项（模块 + 联动）逐项等于 `all_counts`（即各自 `module.yaml` 的 `verify.pass`），全部 0 skip，四项工程级检查通过；安装器在 39 个包里写了公共夹具；同一工程 `go test -mod=mod ./...` 45 个包全过：[full-project-verify.txt](full-project-verify.txt)。条数没有任何变化，`module.yaml`、模块 README 与门禁里的计数都没改。
- 用静态扫描核对过“模块文件里的用例 ∩ run = verify.pass”：mailbox 34 → 33、purchase 24 → 21 差的正好是带 `integration` 标签的真实依赖用例（按构建约束排除）；skillbook 的 `TestSkillCatalogCompiles` 不匹配 `^TestSkillbook`，照旧不计。
- 三类玩法缺省档：卡牌 17、放置 13、回合 PvP 10，0 SKIP；`modules.sh verify` 各模块等于清单：[genres-default.txt](genres-default.txt)。
- 回合 PvP REAL 档：`TestTurnPVPRealTimerRecovery` seed / recover 各 PASS（race、不带构建标签、按原名运行），随后 `test-turn-pvp-real.sh` 配方演练 PASS：[turn-pvp-real.txt](turn-pvp-real.txt)。

## 其他验证

- 模块门禁 `scripts/test-modules-generated.sh`（跑一次）：exit 0、`modules: ok`，脚本与计数未改：[gate-modules.txt](gate-modules.txt)。
- codegen 相关单测 `go test -run 'TestModule|TestAppendDaoFields|TestTenModuleStructs|TestRecipe' ./codegen/internal/roost`：ok（1682s，含两条新回归用例）；`GOWORK=off go test -count=1 -run 'Link|Anchor|Doc' .`：ok：[codegen-tests.txt](codegen-tests.txt)。

## 拿不准的点

1. **夹具落在“放了模块测试文件的包”而不是严格的 `verify.packages`**：两者在现有 42 个模块里重合，另外多了 `game/modules/skills`（skillbook 的 `TestSkillCatalogCompiles` 所在包，不在 verify 里）；账号服务、platform 等非 game 进程的包也会装 game 的 `configs/data`——它们的正式进程未必装载配置表，但装载本身无副作用（同一份数据）。如果维护者要求“只在 game 进程的包里装”，再按服务收窄。
2. **夹具在包级 `init` 里装载、失败即 panic**：`configs/data` 与生成的登记不一致（忘了 `roost generate`）时整个包的测试都起不来，提示里写了 `run roost generate`。这与正式启动一致，但比原来“只有用到表的用例失败”更硬。
3. **装在同一 Registry 之外**：夹具登记在新建的 Registry 上（避免与测试里别的 `RegisterConfigData` 重复），表的集合与正式启动相同；如果工程在 `internal/registry` 的 config 阶段之外还往缺省 Registry 手工登记了别的表（非生成的），夹具看不到它们。现有模板与玩法夹具没有这种写法。
4. **已有工程**：只有安装模块的那一次才写夹具（未上线、不做兼容）；用旧版 roost 装好模块的工程不会补上，要照新工程里的 `roost_config_data_test.go` 手抄一份到各模块测试所在的包。
5. **同名用例跨包**：精确 `-run` 只按名字匹配；工程在 `verify.packages` 的另一个包里写了与模块用例完全同名的测试时会被一起运行、条数变多、verify 失败（可见的失败，不会静默多算）。没有按包分别运行，避免 verify 变慢。
6. **game-demo**：`-template game-demo` 也经 `AddModules` 装 profile / bag 等模块，生成的 demo 工程里同样会出现这些夹具；demo 自己的测试在同一包里会先看到工程配置已装载。按分工没有跑 TestDemo / demo 重新生成，由主线全量覆盖（demo 组同时在改 demo/**）。

## 建议写进 CHANGELOG `[Unreleased]` 的文字

- **修复**（codegen / modules）：**`roost module verify` 的两项行为修正**（维护者 2026-11-17，R9 §9.5）。
  - **模块场景先装工程的配置表**：`roost module add` 在每个放了模块测试文件（含联动的测试文件）的包里写一份公共夹具 `roost_config_data_test.go`（已有不动，之后归工程所有），它的 `init` 按 game 进程正式启动的方式装载工程的 `configs/data`。之前 enter 的场景不装工程的表，工程在 `EnterFirst` 监听里发道具表里的 bag 道具时 13 / 29 条答 `bag_item_invalid`。卡牌删掉给 `items.go` 打的补丁，放置的初始宝石搬回 `EnterFirst` 监听。
  - **只统计模块自带文件里的用例**：用例名从 `roost.yaml` 记下的模块文件（联动是 `with.<另一个模块>`）里读出、与清单的 `run` 取交集，拼成精确的 `-run '^(A|B|…)$'`；工程里同前缀的测试不再被运行或计数。回合 PvP 的 `TestTurnPVPRealTimerRecovery` 去掉构建标签 `turnreal`。
  - 各模块条数不变。证据 `docs/maintenance/evidence/module-verify-20261117/`。

## 建议的 KNOWN-LIMITS 改动

- “三、已修复的历史”加一行：**2026-11-17：`module verify` 两项行为修正（R9 §9.5 维护者决定）**：模块场景先按正式启动的方式装载工程的 `configs/data`（安装器写包级夹具 `roost_config_data_test.go`），enter 的场景不再因工程 `EnterFirst` 发道具表里的道具而答 `bag_item_invalid`；verify 只运行、只统计模块装进工程的测试文件里的用例（精确 `-run`），工程同前缀的测试不算进模块条数，回合 PvP 去掉 `turnreal` 标签。证据链接写 `evidence/module-verify-20261117/README.md`（相对 KNOWN-LIMITS）。
- “一”里 **bag 的边界**（“进程里没装载配置时背包拒绝一切放入……往背包里放道具的测试都要先装道具表”）建议改为：“……进程里没装载配置时背包拒绝一切放入（有意如此）；模块场景所在的包由安装器的公共夹具先装载工程的 `configs/data`，工程自己另起的测试进程（不在这些包里）仍要自己装道具表，否则 `bag_item_invalid`；……”。
- “一”第 411 行附近（玩法脚本核对 `module verify`）可补半句：“verify 只统计模块自带测试文件里的用例，工程的同前缀测试不影响条数”。
