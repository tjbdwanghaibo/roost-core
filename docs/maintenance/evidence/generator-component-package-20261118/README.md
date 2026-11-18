# 生成器对齐“组件包 + view + 实体事件”与缺省 async（2026-11-18）

基于 main `f229585e`（v1.56.0）。本机 macOS（Darwin 25.5，arm64），Go 1.27，`GOWORK=off`。规则来源：
[compose 附录 A2](../../../agent-skills/roost-genre/references/compose.md#a2-组件的写法组件包--gameview--实体事件)；
维护者 2026-11-18 的决定：组件写在 `game/components/<功能>/`，标准命令生成这种形态；生成器写出的骨架、示例与文档不用
`durability=strict`（缺省 async；`strict` 仍是运行时支持的取值）；`roost add entity` 骨架不再在实体包声明 `I<X>Entity`。

## 1. 守卫

`TestGeneratedComponentFollowsTheComponentPackageRule`（[add_test.go](../../../../codegen/internal/roost/add_test.go)）：新建
`-template game` 工程，依次 `add entity Guild`、`add dao Guild --entity Guild`、`add component Roster --entity Guild`、
`add handler JoinGuild --entity Guild --component Roster`、`add component DailyQuest --entity Player`、
`add handler ClaimDailyQuest …`、`add timer --entity Guild`、`add handler ArmGuildTimer --entity Guild --component Timer`、
`add behavior --entity Guild`，再 `roost generate`，断言：

- 组件包 `game/components/roster/roster.go`（`//roost:component`、`CompTypeRoster`、`RegisterComponentFactory(CompTypeRoster, NewComponent)`、
  `Owner`、`Component`、`NewComponent`）与 `game/view/component_roster.go`（`Roster` / `RosterHolder`）存在，DailyQuest 同理；
- 实体结构体挂 `comp:"roster.CompTypeRoster"` / `comp:"dailyquest.CompTypeDailyQuest"`；
- 实体包（guild / player / world）里没有 `<x>_component.go`，`entity.go` 不声明组件结构体与组件方法，手写文件不声明 `I<X>Entity`；
- handler 收 view 接口：`view.IRosterEntity`、`view.IDailyQuestEntity`；定时器的 Tick handler、`add handler --component Timer`
  与行为示例 handler 收 `view.GuildEntity`；`game/handler` 下没有 `guild.` / `player.` / `world.` 的 `I…Entity`；
- 工程里没有任何 `.go` 文件写 `durability=strict`；
- 生成工程 `go build ./...`、`go vet ./...` 通过（`-short` 时跳过编译）。

## 2. 先红

在 `f229585e` 的生成器上（只加守卫）：`go test -count=1 -run '^TestGeneratedComponentFollowsTheComponentPackageRule$' ./codegen/internal/roost/`
失败，原文见 [red-main.txt](red-main.txt)（临时目录改写成 `<tmp>`）：组件包与 view 文件不存在、实体挂的是 `*RosterComponent`
`comp:"CompTypeRoster"`、实体包里有 `roster_component.go` / `daily_quest_component.go`、三个 `entity.go` 声明 `I<E>Entity`、
定时器 Tick handler 与行为 handler 收 `guild.ITimerEntity` / `guild.IBehaviorEntity`、四个 handler 写 `durability=strict`。

## 3. 后绿与变异

改动后同一命令通过（含生成工程 build / vet）。变异（在独立检出上改回旧行为，守卫必须变红）：

| 变异 | 结果 |
|---|---|
| M1：`add_entity.go` 换回 `f229585e`（组件写进实体包） | 红，[mutation-1.txt](mutation-1.txt)：组件包缺失、实体包出现 `<x>_component.go`、挂载标签不对 |
| M2：`add handler` 骨架加回 `durability=strict`；定时器 Tick handler 换回 `guild.ITimerEntity` | 红，[mutation-2.txt](mutation-2.txt)：三个 handler 写 strict、Tick handler 与它的生成接线收实体包接口 |
| M3：`roost add entity` 骨架加回 `I<E>Entity` | 红，[mutation-3.txt](mutation-3.txt)：guild / player / world 的 `entity.go` 声明 `I<E>Entity` |

## 4. 顺带修掉的问题（改动前就存在，改动后才暴露）

`ensureImport`（`add_entity.go`）给只有一行 `import "…"` 的实体文件加第二条 import 时，把新 import 的位置设成文件开头
（`Lparen = token.Pos(1)`、新节点不带位置），`go/printer` 于是把紧跟在 import 后面的注释——实体标记上方的说明与
`//roost:entity`——挪进了 import 块，实体标记丢失，之后 `roost generate` 报 `//roost:entity marker is not followed by a
struct declaration`。旧的 `add entity` 骨架在 import 与注释之间正好隔着一条 `type I<E>Entity …` 声明，所以没暴露；去掉它之后
`add entity` + `add component`（或 `add dao`）就复现。改为把新 import 放在原有最后一条之后、单行 import 补括号。暴露它的是
现有用例（`TestEntityOwnerScopesHandlersLifecycleAndDoctor`、`TestAddSagaNative*`、`TestAddEffect*` 等 15 个），修后通过。

## 5. 验证（本机，`GOWORK=off`）

| 命令 | 结果 |
|---|---|
| `go build ./...`、`go vet ./codegen/...` | 通过 |
| `go test -count=1 <codegen 下除 internal/roost 外的全部包>` | 通过 |
| `go test -count=1 -timeout 0 ./codegen/internal/roost/`（整包、含生成工程 build / vet / demo） | 4 个失败：1 个是本轮漏改的断言（`TestModulesInstallOntoAProjectWithBusinessCodeAndRollBackOnFailure` 找实体里的 `ProfileComponent`，改成认 `comp:"profile.CompTypeProfile"` 后单跑通过）；另 3 个（`TestModulePortalInstallsOnALobbyProjectWithoutGame`、`TestModulePortalInstallsOnTheHostedAccountService`、`TestModuleAddDeclaresTheFrameworkServicesItUses`，都是“装模块后 project sync 又改了工程”）在 `f229585e` 原样检出上同样失败，与本轮无关 |
| `go test -count=1 .`（根包） | 通过 |
| `scripts/test-heartbeat-generated.sh`（夹具 `scripts/testdata/heartbeat/handlers.go.txt` 改收 `view.<E>Entity` + `<X>Of`） | 通过：build / vet / `-race` 全量，端到端用例 dispatch 与 batch 两种投递各 PASS 一次，`roost id check` 通过 |

没跑：pretag、模块矩阵（`scripts/test-modules-generated.sh`、`roost module verify` 全量）、其余玩法脚本。

## 6. 范围与没做的

- 老工程实体包里的旧组件（`<x>_component.go`）不迁移；同名旧组件存在时 `add component` 拒绝，提示挪进组件包或换名。
- 定时器组件文件保留兼容别名 `type I<Component>Entity = view.<Entity>Entity`：模块 activity 的测试模板
  （`modules/activity/files/game/handler/activity_test.go.tmpl`）还写 `es[0].(player.I{{TIMER_COMPONENT}}Entity)`，本轮不改 modules/。
  模块改成 `view.PlayerEntity` 后删别名。
- 模块 rules 自己在实体包声明 `IRulesEntity`（`modules/rules/files/game/entities/player/rules.go.tmpl`），不是生成器输出，未改。
- game-demo 与 `codegen/internal/roost/testdata`、各用例里手写的 handler 仍有 `durability=strict`：它们不是生成器写出的骨架，
  是否改由维护者决定。
