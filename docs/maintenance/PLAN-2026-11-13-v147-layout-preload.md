# v1.47.0 计划：生成工程目录重整 + 本地实体起服预加载（2026-11-13）

维护者 2026-11-13 的要求与决定汇总；实施分两条线并行（目录重整、本地预加载），由主会话合并后统一跑全套。
基线分支 `feat/v147` = main（v1.46.0 发版前的 `3a090307`）+ bag / wallet / attribute 共用组件（`tmp/shared-comp`）。

## 1. 目录目标

```
services/<svc>/         非 game 服务的手写代码，只有用到的服务才有目录
  account/              ← internal/service/account（collaborators、login_channels/dev/player_id）
    portal/             ← game/portal（HTTP 登录入口，跑在 account 进程）
  chat/ ...             ← internal/service/<svc>/collaborators.go
internal/service/game   game 服务的生成接线，保持不动
game/login/             ← game/login（票据校验）+ game/enter（进入/新建登记点、EnterCreate）
                          + game/profile/enter_profile.go（新建时的角色名校验）
                          + Player 进入时的初始化（enter_component / enter_first / enter_hooks）
game/components/<x>/    组件逻辑（Owner 宿主接口），按功能分包
  profile/              ← profile_component/hooks/levelup + game/profile/rename.go
  bag/ wallet/ attribute/  第一批（已在基线，文件名还是 component.go，要按决定 1 改成 <x>.go）
  其余模块的组件（checkin task gacha wheel naming ……）第二批，全部拆出 Player
game/entities/<e>/      只剩 entity.go、_gen 文件（含 lifecycle_gen.go）与每个组件的薄接线文件
game/managers/          managers.go：Managers() 登记点（新建工程是空列表）
game/servertime/        不动
game/controllers、game/handler  不变
```

同步改：compose §4D 的路径 game/<功能>/ → game/components/<功能>/；生成工程的目录说明（AGENTS.md 骨架/DEMO 文档）。

## 2. 维护者 2026-11-13 的决定
1. components 里文件名：bag/bag.go（包名同名文件），不用 component.go
2. 进入时的 Player 初始化（enter_first/enter_hooks/enter_component）放进 game/login
3. portal → services/account/portal/
4. servertime 不动（game/servertime）
5. lifecycle / playeraccess（主会话提议，维护者未反对，随本批实施）：playeraccess 整个是固定模板、无工程内容 → 移进 roost-core 成框架包；
   lifecycle 是 `roost add lifecycle` 一次性脚手架、只随实体名变 → core 出泛型实现 + 每个实体包生成 lifecycle_gen.go；
   world_singleton.go 的 WorldUniqueID 进 game/entities/world/。
6. 已定（维护者 11-13 选“并进接入层”）：game/protocol_handlers/<功能>/ + game/protocol_bootstrap/ 合并为 internal/access/player/protocols_gen.go（全是生成、唯一引用方是 access/player/mod_gen.go，
   没有包外调用 *With 函数）；每个功能的 controller 接口断言保留在这一份文件里。protocol/player_bind 不并（controller 测试会 import 它，并进来会成环）。
7. 维护者 11-13：生成工程带 game/managers/（可以没有业务代码）。提议：把 internal/service/game/managers.go 的 Managers() 移到
   game/managers/managers.go（空列表 + 注释说明 manager 是什么、何时用、DependsOn、Start 时 World 还不存在），Service 改调 managers.Managers()。
   -template game 与 game-demo 都带。
8. 起服全量加载的现状：`//roost:entity preload=startup` 只允许 remote=managed + service= 的实体（按所有权记录枚举，RemotePreloadMod 在 Nest 之后、
   Service.Init 之前跑；单个失败留给按需加载，枚举失败让启动失败）。普通本地实体的支持见第 3 节。

## 3. 本地 Nest 实体起服预加载（维护者 11-13：有需求，普通本地实体必须支持，含 Player 这类）
- 标记：实体类型上 `//roost:entity ... preload=startup`，放开到本地实体；可选 `preload_order=<N>`（越小越先，同序并发，前一批完成再下一批），
  落到现在没人读的 `EntityBuilderParam.LoadPriority`（做实它）。
- 哪些属于本进程（维护者 11-13 定：“local 就是 dbscope=sid”）：生成工程里本地 Nest 实体（Player、World、`add entity` 出来的）的 DAO
  缺省由 `dbscope=global` 改为 `dbscope=sid`（物理库 `<库>_<sid>`，每个服一个库）。库本身就是按 sid 筛的，不另加字段与索引；
  按 ID 冷加载也只会在本服库里找到。`preload=startup` 对 sid 的实体：枚举本服库里该 kind 主 DAO 集合的未删除文档。
  `dbscope=global` 只留给 Remote 托管等跨服共用的实体（Remote 已要求 global，不变）；本地实体写 global + preload 时生成期拒绝。
  连带影响要处理：rehome（换服）现在靠 global 库 + HandOffEntity，改成 sid 库后要把文档搬到目标服的库（参考 sidmerge 的搬库做法），
  rehome 模块与它的场景要跟着改；合服 sidmerge 已按 sid 库设计，核对即可；game-demo 的 terrain_cell_editor 已是 sid。
- 时机：新 Mod（本地预加载），DataEngine 恢复与 Nest 启动之后、Service.Init 之前；Remote 的 RemotePreloadMod 不变。
- 失败：枚举失败 → 启动失败；单个实体失败 → 记错误与指标，留给按需加载。分页 + 并发上限 + 报告与指标，照 PreloadOwnedEntities。
- 待查：加载进来的本地实体会不会被换出（PersistedHotCold、unload_resync），预加载是否连带 Resident；World 仍用 EnsureWorld（没有就建），不改。
- 测试：两个 order 先后；坏文档不挡启动；枚举失败让启动失败；两个 sid 共用一个 Mongo（local 与 sid 两种）各自只加载本服的；local 按 ID 加载别的服的实体被拒；
  换服之后旧服不再预加载、新服预加载；生成期拒绝 remote 实体上的 preload_order 等非法组合。
- 文档：framework guide 02、impl 12-codegen、compose §4E 改写“实体没有启动顺序”、CHANGELOG。

## 4. 范围与回归（维护者 11-13）
- 不考虑已用 core 生成的旧工程（不迁移、不兼容）；只要新生成的工程对。
- v1.47 这一批（目录重整、共用组件、managers 目录、协议注册并入接入层、lifecycle/playeraccess、本地预加载）都要核对并同步
  core 自己的 demo/（game-demo 参考源码）与测试：codegen 测试（TestDemo*、TestModule*、TestRecipe*、路径断言、变异目标路径）、
  scripts/testdata 各玩法夹具、scripts/test-*-generated.sh、模块门禁计数、demo/WALKTHROUGH 与 README、skill 文档里的路径。

## 5. 实施记录：目录重整线未完成项（2026-11-13，tmp/v147-layout）

已完成：§1 的 services/、game/login（票据校验 + 原 game/enter + enter×profile 联动）、game/managers、协议登记并入接入层、
playeraccess 进 core（`infra/network/playeraccess`）、lifecycle 生成（core `wiring/entitylifecycle` + 每个实体包 `lifecycle_gen.go`）、
world / scene 单例进实体包；组件拆分完成 enter（`game/components/enter`）与基线的 bag / wallet / attribute（文件已改名 `<x>.go`）。

未完成，需要维护者定方向（1、2 已按 §6 的 game/view 完成，见 §6“实施记录”）：

1. **其余模块组件（profile checkin task gacha naming mailbox equipment shop ……）仍在 Player 包。** 拆不动的原因：
   - 钩子签名带具体类型：`OnProfileLevelUp(func(c *ProfileComponent, from, to int32))`、`OnEquipmentChanged` 等，登记方（attribute、activity、
     mailbox、task、game-demo）在回调里调 `c.Owner().XComp()`，拿的是 `*Player` 上的别的组件；组件进共用包后 Owner 是接口，回调拿不到别的组件。
   - 联动文件（`with/`）与 game-demo 在 Player 包里给组件类型加方法（例如 activity 的 timers、demo 的 dungeon ledger、profile 测试改写
     `onLevelUp`）；组件类型变成外部包类型的别名后，这些方法不能再声明在 Player 包里。
   - 工程自有文件 `profile_levelup.go` 是 `func (component *ProfileComponent) onLevelUp`，同样受上一条限制。
   - 可选方向：A（推荐）钩子改为收宿主 `*Player`（或宿主接口）而不是组件，联动方法改成 Player 包里的函数，组件逐个按 bag 的样板拆；
     代价是改所有登记方与 demo，模块场景数不变但联动文件都要重写。B 保留组件类型的钩子，在共用包里用 Owner 的可选接口做类型断言拿别的组件；
     改动小，但每个跨组件调用都多一层断言，读起来绕。
2. **`enter_first.go` / `enter_hooks.go` 留在 Player 包**（§2 决定 2 要求进 game/login）：`OnFirstEnter` / `OnCreate` 的钩子收 `*Player`，
   登记方在 Player 包里；放进 game/login 后 Player 包要 import game/login，而 game/login 已 import Player，成环。现状是 enter 的组件逻辑在
   `game/components/enter`、进入流程在 `game/login`，Player 包只留首次发放与新建钩子的登记点。若按 1 的 A 把钩子改成收宿主接口，可以一并搬走。
3. **`game/profile/rename.go`（名字规则登记点）、`game/chat`、`game/equipment`、`game/leaderboard`、`game/mailbox` 未动**；非框架的业务服务
   （alliance、activity_world 等）的手写文件仍在 `internal/service/<服务>/`（§1 只要求托管框架服务进 services/）。
4. **跑在非归属服务的 lifecycle**：原 `roost add lifecycle --service` 在生成期拒绝；现在每个实体包都有 lifecycle_gen.go，改由 doctor 的
   entity-owner 检查别的服务的用法（按 import + 函数名的源码文本查，共用代码 game/ 下看不出由谁调用，不在检查内）。

## 6. 维护者 11-13：用 game/view 打断循环引用
剩余组件拆不出 Player（钩子用具体组件类型、跨组件 `Owner().XComp()`、联动在组件类型上声明方法）与 enter_first / enter_hooks 进不了 game/login
（login → player 的反向引用）——维护者定：game 下新建 `game/view/`，接口定义都放这里。组件、login、钩子与联动只依赖 view；实体实现 view。
钩子签名改为 view 类型，跨组件调用经返回 view 接口的访问器；全部模块组件拆进 game/components，enter_first / enter_hooks 进 game/login。
文件组织（维护者 11-13 补充）：entity 与 component 的接口都在 `game/view/` 一个包里，按前缀分文件——实体 `view/entity_<实体>.go`（如 `entity_player.go`），
组件 `view/component_<功能>.go`（如 `component_profile.go`）；模块模板同名（`files/game/view/component_<功能>.go.tmpl`，自带实体的再加 `entity_<实体>.go.tmpl`）。
组合（维护者 11-13 补充）：view 内部组合——`component_<功能>.go` 定义组件接口与“带有这个组件”的小接口（只含返回组件接口的访问器），
`entity_<实体>.go` 的实体接口嵌入 `entity.IThreadSafeEntity` 与它挂的各组件小接口；组件、钩子、联动只依赖用得到的最小接口。

实施记录（tmp/v147-layout，2026-11-13）：
- 全部 Player 组件拆进 `game/components/<功能>/`（25 个模块 + enter），每个模块带 `game/view/component_<功能>.go`（组件接口 + `<X>Holder`）；
  Player 包只剩实体本体、`_gen` 文件与每个组件一个薄接线（组件号、类型别名、`I<X>Entity`、工厂登记、Owner 方法）。
- 实体接口 `game/view/entity_<实体>.go` 与实体上的 view 访问器（`func (e *Player) Profile() view.Profile`）由**实体生成器**写，不由安装器追加：
  生成器本来就按实体的 `//roost:component` 知道挂了哪些组件，按 view 里现有的 Holder 生成，`generate --check` 会发现漂移，
  卸 / 装模块后重新生成即可，安装器不必维护一份“哪些 Holder 嵌进哪个实体”的状态。冷组件不生成访问器（字段可能为 nil）。
- 钩子签名改收 `view.PlayerEntity`（或更小的 Holder），登记点在发出钩子的组件包；`enter_first.go` / `enter_hooks.go` 进 `game/login`。
  联动文件（`with/`）进登记方的组件包或 `game/login`；在组件类型上声明方法的联动改成函数。
- 留在 Player 包、没有拆的：activity 的 `activity_timers.go`（方法挂在工程的计时器组件上）；game-demo 的 `map_component.go` / `cell_base.go`
  （与 cell 路由耦合，包名 `map` 不能用）、`saga_receipts.go`、`sync_packer.go`（实体宿主本身的职责）。
