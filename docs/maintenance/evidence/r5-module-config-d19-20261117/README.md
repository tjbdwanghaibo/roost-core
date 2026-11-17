# R5（D19 B）模块的配置声明 —— 证据

日期：2026-11-17。分支：`worktree-agent-ac148c8f254c0ab00`（基于 `265adb58`，未推送、未合并）。

## 做了什么

维护者决定（D19 B，不改）：模块清单可以声明配置结构；安装器接入 Mod 配置声明；`roost config check` / `--check-config`
能查到模块的配置。本轮实现了这条链路，并迁了五处规则。

| 层 | 改动 |
| --- | --- |
| 运行期 `wiring/modulehost` | `Module.Config`（`ConfigOf[T](use)`）；`Mod` 实现 `app.ModConfigSchema`（合并全部模块的键）与 `app.ModConfigCrossChecker`（按每个模块的结构体解码一次，执行它的 `ValidateConfig`）；`Init` 按声明装载一次并经 `use` 交给模块；`Value[T]` 给模块代码读（未装载时返回声明的缺省值）。owner 为空的 Mod 只带配置、不调入口。 |
| 清单 | `module.yaml` 新字段 `config: {package, type, use}`：指向模块包里一个带 `config` tag 的结构体（与 Mod 的 A4① 声明写法相同）。装载时检查：包必须是模块自带的包；`services/<fw>/` 下的包只能配 `runtime.framework: <fw>`，反之亦然。 |
| 安装器 | 从模块源码的语法树读出键（与 `app.SchemaOf` 的反射结果逐键交叉校验），把 `StarterYAML` 写成宿主服务配置的一段（dev、prod 示例、k8s secret 示例三份）；plan 输出一行 `config:`；`InstalledModule` 记下 `config`。 |
| bootstrap | 托管服务上：`kitmodulehost.Module{..., Config: kitmodulehost.ConfigOf[pkg.Config](pkg.UseConfig)}`；业务服务上：另加 `kitmodulehost.NewMod("<服务>", "", ...)` 只带配置。因此多一个未声明停机预算的 Mod，`shutdown.total_timeout` 多 3s（未改动过的停机块由安装器顺带刷新）。 |
| 迁的规则 | profile `profile.exp_per_level`；stamina `stamina.{max,hard_cap,regen_interval,refill_daily_limit,refill_price_step}`；leaderboard `leaderboard.{board_id,season,min_score,max_score,default_top,max_top}`（原 `leaderboard_board.go` 的数）；mailbox `mailbox.ledger_prune_grace`（原 600 秒常量，现缺省 10m）；portal `portal.servers`（原入口里手写的 `config` 结构体改为 `Config` 并在清单声明，入口仍在 Start 里自己读）。缺省值与原常量相同。 |
| demo | 排行榜换成 `clears` 榜改为写配置（`demoLeaderboardConfig` 改三份配置的 `leaderboard:` 段），不再改 Go 常量；`ProfileExpPerLevel` 引用改为 `ExpPerLevel()`。 |
| 文档 | `modules/README.md`（新节“模块的配置：config”）、`codegen/docs/CODEGEN_REFERENCE.zh-CN.md`、`docs/framework/impl/13-wiring.md`、`docs/maintenance/FEATURE-2026-11-10-business-modules.md` §11.6、五个模块 README、`demo/README.md`、`demo/WALKTHROUGH.md`、两个 genre 夹具注释。 |

只对新生成 / 新安装的工程生效，不迁移已有工程（线上未部署，不做兼容）。

## 先红后绿

| 承诺 | 红（修前） | 绿（修后） |
| --- | --- | --- |
| modulehost 能声明、检查、装载模块配置 | `red-modulehost.txt`：新 API 编译失败（`undefined: ConfigOf`、`Mod.ConfigSchema undefined`） | `GOWORK=off go test -race -count=1 ./wiring/modulehost/` ok |
| 安装器把清单声明的配置写进工程配置 | `red-e2e-base.txt`：在 `265adb58` 的干净 worktree 上装 profile stamina leaderboard mailbox 后 `config.game.yaml` 没有 `profile:` 段 | `green-e2e-2.txt`：`TestModuleConfigIsWrittenCheckedAndLoaded` ok |
| `roost config check` 查得出模块配置里写错的值 | `red-check-base.txt`：六个错值（`exp_per_level: 0`、`ledger_prune_grace: 30s`、`hard_cap: 50`、`board_id: "no:colons"`、`max_top: 5`、`portal.servers` 键 `"007"`）全部 `exit 0 <nil>`；用例原文见 `base-red-test.go.txt` | 同一用例在修后按键名报错、exit 3 |
| 模块运行期读配置而不是常量 | 见下面模板变异：把读配置的一行换回常量，四个运行时场景全部变红 | 生成工程里 4 个 `TestR5Config*` 全部通过 |

说明：base 上的红用例当时期望 exit 2；实现后模块的键与业务服务声明的键一样由进程的 `--check-config` 拒绝，退出码是 3（见“要维护者定的点”）。红落在“exit 0、没有报错”上，与退出码无关。

## 变异（关键行撤掉必须变红，脚本在本目录，路径是当时的 scratchpad）

- `mutations-modulehost.txt`（`mut_modulehost.sh`）：Init 不装载、跨键检查不跑、声明不并入 schema、不调 use —— 4/4 红。
- `mutations-codegen.txt`（`mut_codegen.sh`）：语法树读键的 `_` 前缀拼接、不读 example、安装器不写配置段、bootstrap 不给业务服务加只带配置的 Mod —— 4/4 红。
- `mutations-templates.txt`（`mut_templates.py`）：profile / stamina / leaderboard / mailbox 读配置的那一行换回常量 —— 4 个 `TestR5Config*` 全红。

## 回归（都是 `GOWORK=off`，`./codegen/internal/roost/` 用 `-run` + `-timeout 60m`）

| 命令 | 结果 |
| --- | --- |
| `go test -race -count=1 ./wiring/modulehost/` | ok |
| `go vet ./wiring/modulehost/ ./codegen/internal/roost/`；`gofmt -l` | 无输出 |
| `-run '^TestModuleConfig'`（含语法树 vs 反射交叉校验、清单校验、目录声明、端到端） | ok（`green-e2e-2.txt`） |
| 模块套件 20 个用例（profile/mail、rules、task、portal、hosted、dungeon、activity、gm、naming、scene、config、catalog、spec、install/verify、rollback） | ok 596s（`modules-run-1.txt`）；场景数不变：profile 21、mailbox 33、stamina 26、leaderboard 21、portal 15（断言在这些用例与 `scripts/test-modules-generated.sh` 的 `all_counts`） |
| `-run '^TestDemo|^TestConfigCheckRunsCrossModRulesThroughTheProcess$'` | 首轮只有 `TestDemoTemplateGeneratesABuildableWritePath` 红（`demo-run.txt`：断言仍找 Go 常量 `BoardID = "clears"`，demo 已改为写配置）；断言改成查配置后重跑 ok 53s（`demo-run-2.txt`） |
| `-run '^TestRecipesCreateBuildableCompositions$'`（casual 配方含 stamina、mailbox；turn-pvp 含 profile） | ok 231s（`recipes-run.txt`） |
| `scripts/test-modules-generated.sh`（全部模块离线验收：新工程、增量、重复安装无变化、换顺序一次装完） | `modules: ok`，11 次 verify 的计数与脚本 `all_counts` 一致（`modules-script.txt`） |
| `python3 scripts/test-genres-generated.py --genres card-rpg merge-casual`（用 stamina 的两个 genre） | 两个都 passed（`genres-run.txt`、`genres-report.json`：stamina 26、stamina+rules 2，其余计数不变） |

digest 变化：模块模板内容变了，安装记录里的文件摘要随之变化，属预期。

## 尚未确认

- 真实进程用改过的配置启动（不是 `--check-config`）只经生成工程里的 `TestR5Config*` 与 modulehost 单测覆盖装载路径，没有起完整的业务进程连真实依赖演练。
- 托管服务（portal 在 account 上）只声明不装载（`use` 为空），入口仍在 Start 里 `app.LoadConfig` 自读；键检查与跨键检查已覆盖（`portal.servers` 键 `"007"` 被拒）。

## 要维护者定的点

> 维护者 2026-11-18 按推荐定（凡写了推荐的点都按推荐）。

1. **模块配置写错时的退出码**：现在 `roost config check` 对模块键的错值退出 3（进程 `--check-config` 拒绝），不是 2（roost 自己按键检查）。原因：`roost config check --keys-only` 与 roost 侧的键检查只认 roost.yaml 推得出的 Mod 声明，不认模块源码里的结构体。A（推荐）：保持 3，与业务服务自己声明的键一致；B：让 roost 侧也读已安装模块的声明，错值退出 2、`--keys-only` 也能查到——代价是 roost 要在不编译工程的前提下解析模块包源码（已有语法树读键可复用），两处声明来源要保持一致。
2. **业务服务多一个只带配置的 Mod，停机总时长多 3s**：A（推荐）：保持（规则统一，doctor 能检查）；B：让只带配置的 Mod 声明停机预算 0，免掉这 3s——要改 App 对“未声明预算的 Mod”一律 3s 的规则。
3. **mailbox 宽限下限 2m 是本轮定的**（理由：App 允许启动时比高水位慢 1 分钟，宽限要比时钟差大出余量）。请确认或给出别的下限。
4. **清单指向 Go 结构体而不是在 YAML 里列键**：按 A4①“规则声明一次（结构体 + tag）”做的；安装器从源码读键、与反射结果交叉校验。如果希望模块作者完全不写 Go 也能加配置，需要另定。
5. **还留在代码里的规则**：leaderboard 的榜范围（Scope / ScopeID）与按日历换周期仍是改函数体；stamina 随玩家变化的规则（接配置表、按等级成长）仍是改函数体；dungeon 自己的 `LedgerPruneGraceSeconds` 常量不在本轮范围。

## 建议的 CHANGELOG / KNOWN-LIMITS 文字（本轮按约定不改这两个文件）

CHANGELOG `[Unreleased]`：

> - 模块清单新增 `config:`（D19 B）：指向模块包里带 `config` tag 的结构体；安装器把它写成业务服务配置的一段，生成的 bootstrap 把它接进 App 的配置声明，`--check-config` / `roost config check` / `--print-config-schema` 都看得见，进程启动时装载一次。profile（`profile.exp_per_level`）、stamina（`stamina.*`）、leaderboard（`leaderboard.*`，原 `leaderboard_board.go` 的数）、mailbox（`mailbox.ledger_prune_grace`，原 600 秒常量）、portal（`portal.servers`）改为配置，缺省值不变。`wiring/modulehost` 新增 `ConfigOf`、`Value[T]`，`Module.Start` 可为空。
> - 行为变化：装了带配置模块的业务服务多一个只带配置的 modulehost Mod，生成的 `shutdown.total_timeout` 多 3s；模块配置写错时 `roost config check` 退出 3。
> - 升级：只对新生成 / 新安装的工程生效；已有工程不迁移（线上未部署）。

KNOWN-LIMITS 二：删除“模块的规则大多还是 Go 写的业务文件”这一条（已按 D19 B 收口）；如维护者在第 1 点选 A，可在框架限制里加一句“`roost config check --keys-only` 不认模块声明的键，模块键的错值由进程 `--check-config` 报，退出 3”。
