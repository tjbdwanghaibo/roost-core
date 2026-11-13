# 交接：修业务模块里已知的问题（2026-11-12）

维护者 2026-11-12：“有问题的地方你直接都清理了”，随后“全部分出吧”——这批修复整体交给接手 agent。本文是任务书。问题是当天给 42 个模块写说明时逐模块读代码发现的，都没有修。

## 0. 从哪里开始

- **分支**：远端 `feat/business-modules-fixes`。它在主线 `feat/business-modules` 的 `ec9cb5a3` 之上多两个提交：一份**未验证的半成品**（§3）和本文。在自己的 worktree 里做，推回这个分支，**不要合进别的分支**。
- 做完之后的事由原会话接：并回主线 → 全套测试 → 合入 `main` → 用新的组合 skill 试生成一个 demo → 发版。所以这批修复是发版前的最后一块，交付时要能直接合。
- `GOWORK=off`。模块模板是编进 CLI 的：改模板后重新 `go build -o <scratch>/roost ./codegen/cmd/roost` 再生成工程验。新建工程后 `go mod edit -replace github.com/tjbdwanghaibo/roost-core=<你的 worktree>`。

## 1. 规矩

- 这批修复由你一个人负责，所以共享文件也归你改：`scripts/test-modules-generated.sh` 的场景数与联动名单、`codegen/internal/roost/` 里钉着场景数与联动的测试、`modules/README.md`、`docs/framework/CAPABILITIES.md`、`docs/maintenance/KNOWN-LIMITS.md`、`CHANGELOG.md`、game-demo 里跟着要改的地方。
- 不改 gateway / Nest / DataEngine / Sync 与生成器的行为。问题必须改它们才能修时不要改：写清现象、最小复现、建议的做法，并把这条限制写进该模块 README 的“能力边界”。
- 遵循模块的写法（`modules/README.md` 的“必须遵守的约定”）与组合 skill 的规则（`docs/agent-skills/roost-genre/references/compose.md` §0、§0A、§4C）：状态在 DAO、改状态只在 Nest handler、业务拒绝用错误码、号不写死（`{{ID:...}}`）、场景测试不用子测试、成功日志打在提交之后的端点层。
- **先红后绿**：每处修复先写会失败的场景（或给现有场景加会失败的断言），看到它红，再修到绿。行为规则的修复定稿后做一次变异自检（把修复改回去，确认有用例变红）。
- 场景数变了同步改 `module.yaml` 的 `verify.pass`（联动的在 `with.<另一个模块>.verify.pass`）、模块 README 的“验证范围”，以及上面那些钉着数字的共享文件。
- 改了行为同步改模块 README 的“能力”“能力边界”“复用方式”。新增的玩家操作按日志规则加日志（`docs/maintenance/HANDOFF-2026-11-12-module-logging.md` §1–§2）。

修到什么程度：

- **缺陷**：修掉，带测试。
- **功能缺口**（缺客户端入口、缺通知、缺一条路）：补一个最小而完整的实现——协议、端点、handler、错误码、场景、README，风格照同模块已有的。不做超出这个缺口的东西。
- **取舍类**（要定规则的）：按“对玩家最不意外、对数据最安全”的方向定，在报告里写明规则与理由。
- 拿不准是不是问题的：先写一个场景复现。复现不了就在报告里写“未复现”，不改代码。

## 2. 问题清单

### 2.1 场景与世界：scene、actor、monster、terrain

这四个模块互相依赖（scene 依赖 actor，monster 依赖 actor 与 scene，terrain 依赖 scene），combat 也依赖 actor。改 actor / scene 的错误返回方式后，这些模块与 scene 的各组联动（`modules/scene/with/*`、别的模块的 `with/scene`）都要仍然通过。

1. **scene：业务拒绝会断开玩家连接。** `HandleSceneChange` / `HandleSceneInteract`（`modules/scene/files/game/controllers/scene/controller.go.tmpl`、`scene_interact.go.tmpl`）把场景已满、目标未开放、`FromID` 过期、对象太远、序号跳号都作为 Go error 直接返回，`Code` / `Reason` 从不填，模块没有声明任何 errcode。按 `infra/network/gateway/tcp.go` 的 `dispatchRequest` 与生成端点模板的注释，端点返回 error 会结束连接。做成：正常拒绝各有业务错误码、填进应答的 `Code` / `Reason`、连接不断；只有真正的内部错误才作为 error 返回。场景断言“拒绝时应答带码、controller 不返回 error”。
2. **terrain：没有业务错误码。** `ErrDenied` / `ErrSequence` / `ErrOccupied` / `ErrFull` / `ErrBase` 都是 `errors.New`，到客户端全是 `CodeInternal`；笔刷端点的入参预检硬编码 `Code: 1`。做成每种正常拒绝一个业务错误码，客户端分得出“序号错了该重查”和“这里不能建”。
3. **monster：没有登记任何业务码。** `MonsterInspect` 的正常拒绝（不同场景、怪已死、ID 不是怪）到客户端都是内部错误。给它们业务错误码。清单 `verify.packages` 列了 `./game/handler`，而场景全在 `./game/monster/service`，核对后改对。
4. **actor：没有错误码。** `ErrCannotMove`（被定身、还没被放进空间）、路径超长等玩家可触发的拒绝，经生成的端点变成内部码并每次记一条 `slog.Error`。给它们业务错误码，正常拒绝不再记 Error。

game-demo 用了 actor / scene / terrain 的一部分（`demo/` 下自己的场景与地形代码）；改了被 demo 引用的导出符号或错误值时 demo 要跟着改。

### 2.2 联盟与活动：alliance、activity、activity_alliance、activity_world

1. **activity + enter 联动疑似传错实体号。** `modules/activity/with/enter/game/controllers/activity/activity_enter.go.tmpl` 把 `entered.PlayerID`（原始玩家号）直接传给 `Sync_ActivityEvent` 和 `Sync_ActivityComplete`；别处（rules、alliance 的 `RefreshMembership`、purchase、cdkey、生成的端点转交）都先 `entity.BuildEntityID(playerID, player.EntityKindPlayer)`。现有联动场景直接调 `EnterComp().Init`，没走这个回调。先写一个真正经过进入回调的场景复现，再修。
2. **alliance：一条用例不在 verify 范围内。** `internal/service/alliance/admin_test.go.tmpl` 的 `TestAllianceStopFoundingSendsNewAlliancesToTheOtherInstances` 所在包不在 `verify.packages` 里，模块共 50 个 `TestAlliance*`，清单写 49。纳入验收并把数目改对。
3. **alliance：模板里残留 game-demo 的注释，与模块缺省值对不上。** `protocol/def/alliance.go.tmpl` 写建盟费是“item 9001”“outside this demo”，模块缺省是 `rules.go` 的 `CoinItemID = 1003`；`game/alliance/alliance.go.tmpl`、`game/handler/alliance_debit.go.tmpl`、`start_alliance_op.go.tmpl` 引用 “gift saga”“game/handler/gift_debit.go”“StartGift”；`player_alliance.go.tmpl` 写 “like WorldStats”；`settings.go.tmpl` 的 help 写 “the demo's item table”。改成对只装模块的工程成立的说法。
4. **alliance：客户端入口只覆盖一部分。** 申请 / 批准 / 拒绝、踢人、转让、宣战在联盟服务里是完整的 RPC，但没有客户端协议和 game 端点。只装模块时：不开放加入的联盟玩家进不去；盟主无法转让，所以永远退不出；也没有解散联盟、结束战争。补齐让这些流程走得通的最小客户端入口：申请、批准 / 拒绝、踢人、转让盟主，以及“盟主是最后一人时退出即解散”或一个解散入口（按服务侧已有的能力二选一）。宣战 / 结束战争服务侧完整就一并给入口，不完整就不做并写进能力边界。权限判断留在联盟服务（单写者）一侧，game 端点只转交。game-demo 装了 alliance 并覆盖了它的规则文件，要跟着改。
5. **activity_alliance 的 `created` 触发不会自动生效。** `Alliance` 没实现 `ActivityCreatedAt()`，恢复扫描传 0，`Trigger=created` 的联盟活动只有调用方显式传创建时间才激活；`AllianceDao.CreatedAtUnix` 是秒，活动引擎用毫秒。修到“联盟创建触发的活动在创建之后自动激活”，单位换算正确，带场景。

### 2.3 社交与战斗：friends、equipment、team、combat、dungeon

1. **friends：六种动作都要求对方在线。** 端点对所有 `Action` 都检查双方的 `ActiveSessions`，取消拉黑、拒绝申请、删除好友在对方离线时也做不了。定规则并修：只改自己一侧、或对方不需要当场知情的动作（取消拉黑、拒绝申请、删除好友、拉黑）在对方离线时也能做；发申请能否对离线玩家发，按“两个 Player 同一笔 Nest 事务能不能装载离线一方”的实际情况定。多实体事务装载离线玩家有框架层面的障碍时不改框架：做得到的做了，做不到的写进能力边界。
2. **equipment：同一道具换等级再 `Equip` 需要背包里另有一件。** `equipment_component.go` 的 `Equip` 在“已穿同一道具、等级不同”时仍先 `BagComp().Remove(itemID, 1)` 再 `Add(previous, 1)`，净变化为零，但背包里没有第二件时被背包拒绝。修成同一槽位上同一道具只换等级时不动背包。
3. **team：被邀请的人无从得知邀请。** `TeamAccept` 需要完整的 `TeamID`，但模块没有推送，也没有“我收到的邀请”查询。补最小实现：被邀请人能查到自己收到的、还有效的邀请（至少一个查询协议）；邀请发出时被邀请人在线就推一条通知（推送失败不影响邀请本身）。上限与过期沿用现有规则。team 被 matchmaking、game-demo 使用，改导出符号时它们要跟着改。
4. **combat：没有“死亡之后”的路。** 死亡不结束会话，之后该玩家再 `CombatReady` 永远答 `combat_dead`，没有回血或复活入口，也没有死亡相关的场景。定一个最小而合理的规则并实现（例如“会话结束之后生命回到满值，可以再次准备”），带场景。`combat/with/skillbook` 这条联动在 `module.yaml` 里只有 summary、没有验证场景：补上至少一条场景与 `verify`。
5. **dungeon：敌人种类写死在两处。** `run.go` 的 `Defeated` 只认 `"minion"` / `"boss"`，而关卡定义在 `rules.go` 的 `Program()`；只改规则文件加新敌人会被 `dungeon_invalid` 拒绝。改成敌人种类只有一个来源（规则文件）。
6. **只读 handler 的声明。** `handlerDungeonCounts`、`handlerAttributeGet` 是只读的，却标了 `rollback=undo durability=strict`；同类的只读 handler（enter 的 `handlerEnterBuilt`、friends 的 `handlerFriendsList`、alliance 的 `handlerPlayerAlliance`）用不带选项的 `//roost:nest`。读 `docs/framework/guide/02-nest-entity.md` 与生成器对这两个选项的处理，确认只读 handler 该怎么标，按结论把全部模块里同样的情况统一。
7. **清单缺 `access:` 段。** `equipment`、`dungeon`、`idle`、`gacha` 的清单没有 `access:` 段（friends 有 `access: transport: tcp`）。单装（不装 enter）时不会打开玩家 TCP 与自身视图同步，而 idle 的客户端必须靠同步视图里的 `Idle.Sequence` 才知道下一个序号。对照有客户端协议、要靠自身视图的其它模块（wallet、checkin、stamina）的清单，判断该不该声明、声明什么，按结论补上并验证单装时确实打开了；不该声明就写明理由。

### 2.4 零散项：notice、gm、wallet、task、servertime、leaderboard、naming、matchmaking

1. **notice：两条命令名是 `gm.notice.*`，却不经 gm 模块的登记**，直接登记在框架 admin 命令表上，gm 模块对操作者与备注的必填检查、审计对它们不生效。`Start` 要求进程里有 `app.ModAdmin`，清单没有声明这个前提（只有 `requires: [chat]`）。修成：装了 gm 模块时这两条命令经 gm 的登记（照别的模块 `with/gm` 联动的做法，把命令搬进 `modules/notice/with/gm/`），没装 gm 时 notice 只提供可信的服务端入口、不登记运维命令；清单里的前提与实际一致。带场景确认执行前 / 执行后日志与必填检查生效。
2. **wallet 的错误文案过窄。** `ErrWalletInvalid` 的文案是 "empty key or zero amount"，没覆盖“键含非法字符 / 超长”“金额为负”。文案改到与实际拒绝一致（只改文案，不改码）；别的模块里断言旧文案的用例跟着改。
3. **task 模板里的过期注释与命名。** `TaskDailyBattle = 3`，而 3 号任务是 `dungeon.claimed`；`handlerTaskAdvance` / `handlerTaskClaim` 的注释仍写“当日进度”“今天的奖励”，现在已有每周 / 永久；`TaskClaimResponse` 的注释只列了 `wallet_overflow`，没提道具奖励的拒绝。改到与代码一致。
4. **servertime 有两个同名包。** `game/servertime`（`At` / `Now`）与 `game/controllers/servertime`（`PushNow`）包名都是 `servertime`。先看这是不是模块的普遍写法（别的模块 `game/<name>` 与 `game/controllers/<name>` 的包名）：是就不改、在报告里说明；不是就给出最小的改法。
5. **leaderboard：场景里 rank 一侧是手写替身**（`leaderboardRecordingRank`、`leaderboardFakeRank`），不是真实的 rank `Service` 加内存存储。把“只升不降”“同分先到在前”这两条规则的场景改成（或另加）用真实的 rank `Service` 加内存存储跑，替身只留给需要注入失败的场景。
6. **naming / matchmaking 的错误日志。** 核对 naming 的 `HandleNamingRename`、matchmaking 的四个端点答内部错误时是否都带 `player_id` 记了 `slog.Error`；没有就补上。

## 3. 半成品提交

分支上的第一个提交是四个并行 agent 做到一半被停下时的工作区，涉及 20 个模块、约 2700 行。**没有跑过任何验收，可能编译不过；场景数、清单、共享文件都没有同步。** 各组停下时的位置：

| 组 | 停在哪里 |
| --- | --- |
| 场景与世界 | scene、actor、monster 已改到一部分；terrain 的 controller 与测试正要写 |
| 联盟与活动 | activity、activity_alliance、alliance 各动了几个文件 |
| 社交与战斗 | friends、equipment、dungeon 有改动；team 的场景（handler 层与端点层）正要写 |
| 零散项 | notice、gm、task、wallet、idle、gacha、attribute、matchmaking 有改动；leaderboard 的测试正要改写 |

把它当草稿：`git show --stat HEAD~1` 看动了哪些文件，逐个模块读过、判断对不对再用，不对就重写。每个模块都要重新走一遍“先红后绿”——草稿里的修复没有留下“红”的证据，至少要用变异自检确认新场景拦得住。

## 4. 验证与交付

- 每修完一组模块：新建 `-template game` 工程装这组模块及其依赖，`go build ./...`、`go vet ./...`、`roost module verify`、工程自己的 `go test ./...`。
- 全部修完，跑一次即可：
  - `bash scripts/test-modules-generated.sh`（全部模块正序、反序同装）；
  - `go test -count=1 -timeout 60m -run 'TestDemo|TestModule|TestRecipe' ./codegen/internal/roost`；
  - 新生成的 game-demo：build / vet / `roost id check` / `roost generate --check` / `go test ./...` / `roost module verify`；
  - 全仓 `go test ./...`、根包 `go test -count=1 .`、`python3 scripts/check-doc-anchors.py`。
- 对改了客户端可见行为的至少两处（建议 scene 的拒绝不断连接、alliance 新增的客户端入口）起真实进程走一遍，记下结果；做不到就写明只有离线验证。
- 文档：改到的模块的 README；`modules/README.md` 与 `CAPABILITIES.md` 里对应的行；`KNOWN-LIMITS.md` 里已经不成立的条目删掉或改写、新出现的限制补上；`CHANGELOG.md` 的 `[Unreleased]` 加一条；`docs/maintenance/HANDOFF-2026-11-12-module-logging.md` 最后一节（“读代码时已经发现、不在本任务里修的问题”）里修掉的划掉或删掉。
- 报告：每个问题一行（修了 / 没修及原因 / 未复现、改了哪些文件、新增或改动的场景名）；场景数与联动的变化；新协议与错误码；你定的取舍；变异自检的结果；跑了哪些检查及原样的结果行；没验证的；过程中又发现的新问题（不要自己扩大范围去修）。

## 5. 接手结果

本轮由 `feat/business-modules-fixes` 承接，逐项结果、行为取舍、场景数、变异与真实进程验收见[修复报告](evidence/module-fixes-20261112/README.md)。保留的离线双边好友、离线队伍邀请、停战入口均在报告中明确列出；后续追加的 Scene / Terrain 启动接线问题已修复并补真实 TCP 验收。仅推回本分支，不合并、不发版。
