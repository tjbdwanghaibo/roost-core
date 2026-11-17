# game-demo 通关榜用例读到缺省榜名 —— 证据

日期：2026-11-17。基于 `3ed5772d`，分支 `worktree-agent-aee0bddb4b1489668`（未推送、未合并）。

## 结论

- 现象：game-demo 生成工程 `game/controllers` 的 `TestDungeonClearSubmitsTheSeasonCountAsASnapshot` 在未改动的 HEAD 上就红，
  端点提交到 `total:global:0:1`，用例要 `clears:global:0:1`（`red-before.txt`）。
- 根因（已确认）：**测试夹具缺装载，运行时代码没有问题。** R5（`7256ab66`）把 leaderboard 的榜名等从 Go 常量改为模块配置
  `leaderboard.*`（`leaderboard_config.go` 的 `Value[Config]`），demo 的 `clears` 改为由 `demoLeaderboardConfig` 写进 game 服务三份配置的
  `leaderboard:` 段。真实进程里，生成的 bootstrap 给 game 服务加了只带配置的 `kitmodulehost.NewMod("game", "", …{Name: "leaderboard",
  Config: ConfigOf[leaderboardConfig.Config](leaderboardConfig.UseConfig)}…)`，Init 时按声明装载并 `UseConfig`，端点读到 `clears`。
  单测不经 bootstrap，`Value.Get()` 在没装载过时按设计返回声明的缺省值（`board_id` 缺省 `total`）。R5 给 demo 的
  `game/modules/leaderboard/leaderboard_board_test.go` 加了 `useDemoBoardConfig`（读开发配置的 `leaderboard:` 段），但漏了同样钉住字面
  `clears` 的 `game/controllers/player_finish_dungeon_test.go`。
- 为什么 R5 没发现：codegen 的 `TestDemo*` 只生成、编译、vet demo，不跑生成工程的 `game/controllers` 用例；R5 的证据里没有在生成的 demo 上
  跑这个包。

## 修法

`demo/game/controllers/player_finish_dungeon_test.go.tmpl`：新增 `useDemoLeaderboardConfig(t)`，与 `leaderboard_board_test.go` 的
`useDemoBoardConfig` 同一读法（`app.LoadConfigFile` 读 `configs/service/config.{{GAME_SERVICE}}.yaml` → `app.LoadConfig` 解码成
`gameleaderboard.Config` → `UseConfig`，`t.Cleanup` 换回旧值）。在 `newFinishDungeonHarness` 开头调用，所以文件里经正式端点的三条用例
（快照、真实 Redis、活动贡献）都在 demo 的真实配置上跑。断言没动（仍是字面 `clears`）。`demo/WALKTHROUGH.md` 对应段补一句。
controllers 包没有 `t.Parallel` 用例，换包级配置不会和别的用例并发。

## 红 / 绿 / 变异

| 步骤 | 命令（在生成的 game-demo 里，`GOWORK=off GOFLAGS=-mod=mod`，core replace 到本检出） | 结果 |
| --- | --- | --- |
| 红（HEAD 模板） | `go test -count=1 -run TestDungeonClearSubmitsTheSeasonCountAsASnapshot ./game/controllers/` | FAIL：`first clear: submitted to board total:global:0:1, want clears:global:0:1`（`red-before.txt`） |
| 绿（用修后模板重新生成的工程） | `ROOST_REVIEW_REDIS=<共享隔离 Redis> go test -race -count=1 -v -run 'TestDungeonClear|TestAReplay' ./game/controllers/` | 3 条 PASS，含真实 Redis 那条（`green-targeted.txt`） |
| 变异 M1 | 撤掉 harness 里的 `useDemoLeaderboardConfig(t)` | 红，同一失败文本（`mutations.txt`） |
| 变异 M2 | 开发配置 `board_id: clears` 改回 `total` | 红（`mutations.txt`）：用例现在钉住的是“demo 写进配置的榜到了端点” |

生成方式：`go build ./codegen/cmd/roost` 后 `roost project new demo -module example.com/dclears -mods configdata,mongo,nats,dataengine,nest
-template game-demo`（与 `projectFixtureOptions["game-demo"]` 同参数），再 `go mod edit -replace` 指向本检出。修后重新生成的工程里
`gofmt -l game/controllers game/modules/leaderboard` 无输出。

## 回归

| 命令 | 结果 |
| --- | --- |
| 生成工程 `go vet ./game/controllers/ ./game/modules/leaderboard/` | ok |
| 生成工程 `go test -race -count=1 ./game/controllers/... ./game/modules/leaderboard/... ./game/modules/mailbox/... ./game/components/profile/... ./game/handler/... ./game/entities/player/... ./services/account/portal/...` | 全部 ok（`green-packages.txt`；profile 包没有测试文件） |
| 仓库 `GOWORK=off go test -count=1 -run 'TestDemo|TestModuleLeaderboard|ModuleConfig' -timeout 60m ./codegen/internal/roost/` | ok 167.9s（`codegen-run.txt`）。注意：仓库里没有名为 `TestModuleLeaderboard*` 的用例，这段模式没匹配到东西；匹配到的是 18 条 `TestDemo*` / `TestGameDemo*` / `TestGeneratedGameDemo*` 与 4 条 `TestModuleConfig*` |
| `GOWORK=off go vet ./codegen/internal/roost/` | ok |
| `GOWORK=off go test -count=1 -run 'Link|Anchor|Doc' .` | ok |

## 同类排查（读模块配置的 demo / 模块测试）

game-demo 里模块配置的实际值与声明缺省值对照：profile `exp_per_level: 100`（缺省 100）、mailbox `ledger_prune_grace: 10m`（缺省 10m）、
portal `servers`（use 为 nil，入口在 Start 里自读，测试各自给配置）、stamina 未装；只有 leaderboard 不同（`board_id`、`max_score`、
`default_top`、`max_top`）。在生成的 demo 里搜所有 `_test.go` 对这些规则的引用：

- 用字面值钉 demo 配置的只有两处：`game/modules/leaderboard/leaderboard_board_test.go`（R5 已装载）与本次修的 `player_finish_dungeon_test.go`。
- 其余（`game/handler/add_exp_test.go`、`game/entities/player/attribute_component_test.go` 的 `ExpPerLevel()`，模块的
  `leaderboard_test.go`、`controllers/leaderboard_test.go` 的 `CurrentConfig()` / `CurrentBoard()`）都按名字引用，读缺省值时自洽，
  不是“碰巧通过”的错断言；但它们测的是缺省值，不是 demo 写进配置的值（leaderboard 模块的这两份按 R5 设计就在缺省上跑）。
- 模块自带的 `leaderboard_board_test.go`（钉 `total` / 一万亿 / 10 与 50）、card-rpg genre 夹具的 `cardRegenMillis`（stamina 缺省 5m）
  按注释是“钉住声明的缺省值、不装载工程配置”。工程或 genre 只改 yaml 不改代码时，这些用例仍绿、也不会提醒——这是 R5 的设计取舍，
  不是本次的缺陷；列在下面供协调者判断要不要登记。

没有发现第二个读到缺省值而断言碰巧通过的用例。

## 尚未确认

- 真实进程装载 `clears`：没有起 game 进程连真实依赖演练；装载路径由 R5 的 modulehost 单测与变异（`mutations-modulehost.txt`）覆盖，
  本次只核对了生成的 `internal/bootstrap/generated.go` 第 160 行确实把 leaderboard 的 `ConfigOf(UseConfig)` 挂在 game 服务上、
  `configs/service/config.game.yaml` 里是 `board_id: clears`。
- 门禁缺口：codegen 的 `TestDemo*` 不跑生成 demo 的包测试；pretag 的 `go test ./...` 跑 codegen 也不会跑到。CI 的 demo 生成工程测试
  （`scripts/test-demo-generated.sh` 注释里提到的 framework-compat / demo-publish）应当会跑到，但按规则不等 CI，未核对它当时是否红过。

## 建议写进 CHANGELOG / KNOWN-LIMITS 的文字（本轮按约定不改这两个文件）

CHANGELOG `[Unreleased]`（修复）：

> - game-demo：`game/controllers` 的 `TestDungeonClearSubmitsTheSeasonCountAsASnapshot` 在 R5（模块配置）之后读到 leaderboard 声明的缺省榜
>   `total` 而失败——单测不经 bootstrap，没有装载 demo 写进 game 服务配置的 `leaderboard.board_id: clears`。通关榜的端点用例改为先按进程的
>   读法装载开发配置的 `leaderboard:` 段；运行时代码不变。

KNOWN-LIMITS（框架限制，可选，由协调者 / 维护者定）：

> - 模块的场景测试与 `roost module verify` 在模块声明的缺省配置上跑（`modulehost.Value` 未装载时返回缺省），不读工程 yaml：工程只改配置不改代码时，
>   模块自带的“钉住缺省值”的用例照样通过，也不会提醒配置与之不同；工程自己要钉配置值时，按 demo 的 `leaderboard_board_test.go` 那样在用例里装载配置段。
>   生成 demo 的包测试只由 CI 的生成工程测试覆盖，codegen 的 `TestDemo*` 只生成、编译、vet。
