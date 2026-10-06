# C4：活动组由一个配置文件定义，每组上限 64（2026-10-06）

维护者决定（[DECISIONS-PENDING 第三轮](../review/DECISIONS-PENDING-2026-10-05.md) C4）：“game 组应该是一个配置文件，上限暂定是 64 个”。

## 1. 现状与问题

- game-demo 每个 game 服务配置各写一份 `activity.game_sids`，活动组的 id 是编译期常量 `gameactivity.GroupID = "<project>"`。
  候选集 = 本服 + `game_sids`，启动时只检查重复、int32、不超过一次 `Live` 的上限 200（RR-20261005-01）。
- 协调器 `kit/service/global/activity` 对一个窗口的 expected 集合上限是 `MaxExpectedGames = 64`。候选 65～200 个、同时活着超过 64 个时，
  每个窗口都被协调器拒绝，只有每 5 秒一条 Warn（N01/S4 O4）。
- 协调器的后台 sweep 只扫 `activity.sweep_groups` 列出的组，生成的配置写的是 `[]`：game-demo 的组从来没有进程兜底宽限窗口（T-46 的情形）。
  组 id 写在 game 代码里、sweep 组写在协调器配置里，两处可以不一致。

## 2. 目标

- 活动组（参与同一个全服活动的 game sid 集合）由一个文件定义：`configs/activity_groups.yaml`。
  ```yaml
  groups:
    - id: gd0
      game_sids: [1000, 1001, 1003]
  ```
- 解析与校验只有一份：`kit/service/global/activity.LoadGroupsFile` / `ParseGroups`。game 进程和协调器都用它读同一个文件。
  - 每组成员数 ≤ `MaxExpectedGames`（64，就是协调器单窗口的上限，同一个常量）；
  - sid 必须是正的 int32；组内不重复；一个 sid 只能属于一个组（game 按自己的 sid 找组）；
  - 组 id 非空、不含 `/`（与 `Key.Validate` 同规则）、不重复；至少一个组；未知字段拒绝（`game_sid` 之类的拼写错误不会被当成空）。
  - 错误点名文件、组和 sid。
- game（game-demo 模板）：读 `activity.groups_file`，按本进程 sid 找到所属组，组 id 就是协调器 Key 的 GroupID，组成员就是 `Live` 查询的候选集。
  本进程 sid 不在任何组里、文件不合格，都在启动时报错（在任何远端调用之前）。
- 协调器：`activity.groups_file`（可选）。设置后启动时按同一规则校验文件；`activity.sweep_groups` 为空时，后台 sweep 扫文件里的全部组
  （显式写了 `sweep_groups` 仍以它为准，用于多副本分担）。这是 kit activity 包里唯一的行为改动，`Service` 逻辑不变。

## 3. 改动面

| 位置 | 改动 |
| --- | --- |
| `kit/service/global/activity/groups.go`（新） | `Group` / `Groups`、`ParseGroups`、`LoadGroupsFile`、`Groups.Of(sid)` / `IDs()` |
| `kit/service/global/activity/activity_mod.go` | 读 `activity.groups_file`，空 `sweep_groups` 时取文件里的组 |
| `demo/game/activity/activity.go.tmpl` | 删 `GroupID` 常量，`Key(groupID, activityID)` |
| `demo/internal/service/game/activity.go.tmpl` | 删 `candidateSIDs` 与 `activity.game_sids`；按文件定组与候选；开窗拆出 `openWindow` 便于测试 |
| `codegen/internal/roost` | 有 activity 框架服务的工程生成 `configs/activity_groups.yaml`（只创建一次，之后归项目）；activity 服务配置写 `groups_file`；Dockerfile 把文件拷进镜像；shell 安装把文件拷进 release；game-demo 的 game 配置写 `groups_file`、删 `game_sids`；`second-game.sh` 在 game 读组文件时检查自己的 sid 在文件里 |
| 文档 | USER_GUIDE、GAME_DEMO_TEMPLATE、CHANGELOG |

文件默认内容：组 id 为工程名（与原来的 `GroupID` 常量相同，Redis 里已有的窗口 / 派发键不变）；成员是生成的各部署方式给第一个业务服务的 sid
（本机与 k8s / shell 的 1000、`second-game.sh` 的 1001、生产 compose 按服务序号给的 1000+N），`Live` 只会把实际在跑的算进 expected。

部署：文件是随代码版本发布的内容（和 `configs/data` 一样进镜像 / release），compose 与 k8s 直接用镜像里的 `/app/configs/activity_groups.yaml`；
某个环境要不同的分组时，把文件挂到这个路径，或者把 `activity.groups_file` 指到挂载的位置。

## 4. 兼容

- 维护者决定**不迁移已生成的工程**：旧工程的 game 代码仍读 `activity.game_sids`，不受影响；新生成的 game-demo 不再有这个键。
- kit：`activity.groups_file` 是可选新键，未设置时协调器行为与之前完全相同。
- 已生成工程执行 `roost sync` / `upgrade` 时，Dockerfile（生成器所有）会多一行 COPY，同时 `configs/activity_groups.yaml` 不存在就会被创建，所以镜像构建不会缺文件。

## 5. 验证

- 先红：旧模板上 `activity.game_sids` 给 64 个其他 sid（加本服 65 个候选）启动不被拒绝。
- 绿：kit `groups` 单测（65 拒绝、64 接受、各类错误点名）、Mod 读文件得到 sweep 组；生成工程里 65 个成员的组启动被拒，正常组经真实协调器
  （内存存储）开窗，Key 的组 id 来自文件、expected 是 `Live` 的结果，64 个全活也能开窗；codegen 测试；生成工程 build / vet / test。

## 6. 实施状态

已实施（分支 `c4c6`，提交号见 DECISIONS-PENDING C4 行）。

- 先红（旧模板生成的工程，`TestActivityRefusesACandidateListNoWindowCouldOpenWith` 加一格：本服 + 64 个其他 sid）：
  ```
  --- FAIL: TestActivityRefusesACandidateListNoWindowCouldOpenWith/more-candidates-than-the-coordinator-takes (0.00s)
      activity_test.go:238: startActivity with activity.game_sids=[2000 2001 2002 2003]: error activity: game: capability "service.global.activity" not found; is the activity process running and reachable over the bus? does not refuse the list by name; every window it would try to open would fail
  ```
  （65 个候选通过了候选检查，一直走到查协调器能力才失败。）
- 绿：
  - kit：`groups_promises_test.go`——65 个成员加载即拒（点名文件、组、65、64）；64 个接受，且全活时真实协调器开窗；11 类不合格文件点名拒绝；`Of` / `IDs`；Mod 读文件得到 sweep 组、显式 sweep_groups 优先、不合格 / 缺失文件 Init 失败。`go test -race -count=3` 通过。
  - 生成工程（game-demo，replace 到 worktree）：`TestActivityRefusesAGroupNoWindowCouldOpenWith`（65 个成员、重复、两组、超 int32、本服不在组里、未配文件，均按名拒绝；64 个通过组检查）、`TestAWindowOpensForTheGroupTheFilePutsThisServerIn`（真实协调器内存存储：Key 的组 id 来自文件、expected 是 `Live` 的结果、另一组没有窗口、重复开窗正常；64 个全活能开窗）。`go build ./... && go vet ./... && go test ./...` 全绿。
  - codegen：`activity_groups_promises_test.go`（文件覆盖 compose / k8s / run.sh / second-game.sh 的 sid；协调器与 game 配置写 `groups_file`、不再有 `sweep_groups` / `game_sids`；Dockerfile 与 install.sh 带上文件；没有协调器的工程不生成）；`go test ./codegen/...` 全绿。
  - `second-game.sh`：`SECOND_SID=1005` 启动前退出并点名文件；1001 / 1003 通过检查。
- ~~未做：协调器的 `OpenActivity` 不核对 expected 集合是否属于 Key 的组（会改 `Service` 逻辑，留待需要时再做）~~ **已实施**（2026-10-06，分支 `oa`，
  [RR-20261006-17](../bug/RR-20261006-17.md) / [修复](../bugfix/RR-20261006-17.md)）：Mod 把已加载的组经 `Config.Groups` 交给 Service，`OpenActivity` 写入前要求 Key 的组在文件里、
  expected ⊆ 组成员，否则 `ErrInvalid` 点名；未设置 `groups_file` 的协调器不变。这改了 §2 写的“`Service` 逻辑不变”。
- 没有在真实依赖上起进程演练（启动拒绝发生在任何远端调用之前，单测与生成工程测试已覆盖）。
