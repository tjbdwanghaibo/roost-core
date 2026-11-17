# 邀请的跨 game 自动投递：每个玩家一个伴生的 facebook Remote 实体（2026-11-17）

任务来源：KNOWN-LIMITS 二“社交容量与归属”的前一半——“邀请的跨 game 自动投递没有实现”。容量那一半不在本任务。
做法按维护者 2026-11-17 的推荐：给每个 player 加一个伴生的 facebook entity，它是一个 Remote entity。

## 一、设计（先写，再实施）

### 现状（改之前）

- 队伍邀请的权威在共享 Remote 实体 Team 上（`TeamDao.Invites`），任何 game 都能在事务里读写它（D14：Team 归 game 服务、共享模式）。
- “被邀请人收到”这一步靠被邀请人自己的 Player：邀请提交后，发邀请的 game 只在被邀请人**此刻连在本进程**时，往他的
  `PlayerDao.Team.Invited` 记一条收件记录，再推 `TeamInvited`（`game/controllers/team.go` 的 `teamAnnounceInvite`）。
- 只能投本进程的原因：Player 归它的承载 game 独写（静态归属 + App 单实例锁），别的 game 不能写它的档；会话在本进程是
  “他的档归本进程写”的唯一证明。所以连在别的 game 上、或离线的被邀请人，收不到推送，`TeamInvites` 也查不到，
  只能事先知道 TeamID 再接受（`TestAllianceE2ESocialTeam` 就是这样绕过去的）。

### facebook 是什么

| 问题 | 结论 |
| --- | --- |
| 身份 | 每名玩家一个，号由玩家号直接推出：`entity.BuildEntityID(玩家号, EntityKindFacebook)`，与 Player 同一个 unique、不同 kind。不要发号器，一一对应，谁都能算出来。先例：demo 的 `terrain_editor`（“id 是玩家的 unique id，所以只用 GetOrCreate”）。 |
| 类别 | `category=entity.EntityCategoryRemote remote=managed service=<game>`，**共享模式**（和 Team 一样）：任何 game 进程都能在 Nest 事务里写它，写准入取分布式锁与写许可，持久与 fence 走 Remote 正式链路。 |
| 放在哪个服务 | game 服务（与 Team 相同，D14）。不新开服务。 |
| 持有的状态 | `FacebookDao.Invites`：收到的邀请收件箱，键是发出邀请的实体完整号（队伍号），值是 `{From 邀请人玩家号, ExpireAt 过期时刻（业务时间，Unix 毫秒）}`。有界（`MaxInvites = 64`），同一来源的新邀请只延长不缩短，过期的在下一次记录时清掉，恰好到期那一刻算过期。它是**提示**，邀请成立与否仍以来源实体（Team）上的权威为准，接受时照旧核对。 |
| 谁建它 | 第一次要写它的那个 game（目前就是发邀请的那一方），在事务之外“确保”：没有所有权记录 → `GetOrCreate` + 认领 + 切共享；已共享 → 只 `GetOrCreate` 装进本进程内存；由本进程独占、还没切共享 → 切共享；被别的 game 独占（别人正确保到一半）→ 返回可重试错误。和建队“先取号建空实体、再进事务”同一个次序。 |
| 属于哪个模块 | **新模块 `facebook`**（自带实体 Facebook + 组件 Facebook + 运行时），team `requires: [facebook]`。理由：它是“玩家”的伴生社交实体，不是队伍的一部分；好友申请这类以后要跨 game 投递的东西可以复用同一个收件箱与通知，不必各模块各造一份。 |

### 跨 game 投递的路径

```
A（game-1）TeamInvite
  └─ [Team, Player A] strict 事务提交邀请（不变）
  └─ 提交之后（不在任何实体锁里）facebook.Deliver：
       1. 确保 Facebook(B) 存在且是共享模式（上面的“谁建它”）
       2. Nest 事务 [Facebook(B)] strict：收件箱记一条（Remote 共享写，持久后才回）
       3. 告知：本进程有 B 的会话就直接交给登记的处理（team：推 TeamInvited）；
          同时在服务间同步总线（syncbus，主题 facebook）广播一条通知 {To, Source, From, ExpireAt}
B（game-2）
  ├─ 在线：game-2 的 facebook 运行时收到通知 → B 在本进程有会话 → team 的处理推 TeamInvited
  └─ 任何时候：TeamInvites 读 Facebook(B) 的收件箱（Nest 读共享实体，拿锁后读到最新权威），逐条核对 Team 权威后返回
B TeamAccept(TeamID)：[Team, Player B] 事务（不变），接受仍以 Team 上的权威为准
```

- **收件箱是记录，推送是捷径**（与 chat“存储为准、推送为捷径”同一原则）：通知走普通 NATS 时至多一次，丢了由
  `TeamInvites` 补查；JetStream 时用 DeliverNew 的可确认订阅，不回放旧历史，过期的通知收到也丢弃。
- **广播**：通知发给所有 game 进程，谁那里有 B 的会话谁推；没有按“B 现在在哪个 game”路由（路由要一份所有模块都有的
  玩家→承载服映射，现在只有装了 rehome / account 的工程才有）。代价是每次邀请一条广播、game 数个接收者各判一次在线，
  见下“限制”。
- **投递失败不撤销邀请**：确保 / 记录 / 广播任何一步失败都只记 Debug，邀请（Team 上的权威）已经成交，调用方应答不变，
  重发邀请会再投一次——与改之前的“告知不是邀请的一部分”相同。

### 换服（rehome）时它动不动

不动。Facebook 的号只由玩家号推出，与承载 sid 无关；它在共享模式下由任何 game 写，不归某个 sid 独写；通知是广播，
不依赖 B 的承载服。换服搬的是 Player，facebook 不用跟着搬，也不用改 rehome 模块。

### 与 D14（Team 归 game 服务）的关系

一致：facebook 与 Team 同在 game 服务、同用共享模式；game 本来就因 Team 保留 `remote_entity`，facebook 不增加新的服务或 Mod
（syncbus 是 `remote_entity` 的既有依赖）。没有改 Team 的归属与成员表、邀请的权威语义。

### 改动范围

- 新模块 `modules/facebook`：实体、DAO、组件、两个 Nest handler、`game/modules/facebook`（号、确保、投递、查询、通知登记点、
  运行时订阅）、场景测试。
- `modules/team`：清单加 `requires: [facebook]`；收件记录从 `PlayerDao.Team.Invited` 搬到 facebook（删掉 Player 上的
  `Invited`、`NoteInvite` / `Invites` 与两个 handler）；`teamAnnounceInvite` 改为 `facebook.Deliver`，`TeamInvites` 改读
  facebook 收件箱；接受不再在同一笔事务里清收件记录（facebook 不进接受的事务，已接受的邀请由权威核对滤掉、到期清掉）。
- demo：只改 `demo/README.md` 队伍一段的一句（原“被邀请的人从别处得知队伍 ID（此接口不推送邀请）”）；`codegen/internal/roost/demo.go`
  不改（它装 team，facebook 随 `requires` 先装），不动 demo 的 CellWorld / 队伍视图文件。
- 真实依赖：`scripts/testdata/alliance-e2e/social_test.go.txt` 新增 `TestAllianceE2ESocialTeamInvite`。

### 尚未确认（与推荐可能有出入、或拿不准的点）

1. **谁建 facebook**：现在是“第一次要写它的 game 确保”，所以邀请一个从没登录过、甚至不存在的玩家号也会建出一个
   facebook（每个号至多一个，邀请有界、只有队长能发）。另一种做法是“玩家自己的承载 game 在进入游戏时建”，那样邀请不存在的
   玩家可以直接拒绝，但 team 要多依赖 enter、没进过游戏的玩家收不到邀请。本轮按前者实施，尚未确认维护者要哪一种。
2. **投递是否进邀请的同一笔事务**：现在是提交之后尽力投递（与改之前同一语义）。也可以做成 `[Team, Player A, Facebook B]`
   一笔事务（邀请成功 ⇒ 收件箱一定有），代价是邀请多一个共享 Remote 的分布式锁、facebook 建不出来时邀请失败。尚未确认。
3. **通知走广播**：见上，按承载服路由要先有通用的玩家→承载服映射。尚未确认要不要做。
4. **名字**：模块、实体、集合都叫 facebook（维护者的叫法），与外部品牌同名；仓库里没有与之冲突的命名惯例。要不要换个
   中性名字（如 social / inbox）尚未确认。

## 二、做了什么（实施结果）

- 新模块 `modules/facebook`（9 个场景）：
  - `db/def/facebook.go`：`FacebookDao{Invites map[int64]*FacebookInvite}`（集合 `facebook`，`nosync`，只在服务端）。
  - `game/entities/facebook`：实体 Facebook（`remote=managed service=<game>`），`EntityKindFacebook`。
  - `game/components/facebook`：`Note`（校验、清过期、只延长不缩短、`MaxInvites = 64` 有界）、`Invites(now)`（恰好到期不列出，按来源排序）。
  - `game/handler`：`handlerFacebookNote`（`rollback=undo durability=strict`）、`handlerFacebookInvites`（`durability=memory`）。
  - `game/modules/facebook`：`EntityID`、`Ensure`、`Deliver`、`Invites`、`OnInvite`（init 期按来源 kind 登记告知处理）、`Start`（订阅 syncbus 主题 `facebook`，经 `app.AddIngress` 登记，停机退订）。
- `modules/team`（35 → 31 个场景）：`requires: [facebook]`；删 `PlayerDao.Team.Invited` / `TeamInvite`、组件的 `NoteInvite` / `Invites`、
  两个 handler 模板（`team_invite_note`、`team_invites`）及对应 4 个 handler 场景；`teamAnnounceInvite` 改走 `facebook.Deliver`；
  init 里 `facebook.OnInvite(EntityKindTeam, teamNotifyInvite)`（本进程有被邀请人会话才推 `TeamInvited`）；`TeamInvites` 读 facebook
  收件箱，只认队伍来源，逐条照旧核对 Team 权威。控制器场景两条重写（查询核对权威 + 外来 kind 跳过；告知失败不撤销邀请 + 广播告知只推本进程在线者）。
- 真实依赖：`scripts/testdata/alliance-e2e/social_test.go.txt` 新增 `TestAllianceE2ESocialTeamInvite`；`scripts/test-alliance-e2e.sh`
  默认集合加 `SocialTeamInvite`（11 → 12）；`scripts/test-social-real.sh` 4 条；根包 `integration_coverage_test.go` 登记。
- 场景数同步：`scripts/test-modules-generated.sh`（加 facebook=9，team=31）、`codegen/internal/roost/module_{team,matchmaking,alliance}_test.go`
  （team 测试加 3 条关键行变异）、`modules/README.md`、`modules/team/README.md`、`docs/framework/CAPABILITIES.md`、
  `docs/agent-skills/roost-genre/references/compose.md`、`demo/README.md` 一句。

### 实施中发现的问题：读一个还没打开的共享实体会把它独占认领

第一版 `Invites` 直接走 Nest 读 Facebook(B)。真实依赖下 B 在 game-1001 上先查了一次空收件箱，这一读经过写准入，
对“没有所有权记录”的 Facebook(B) 做了**首写认领**，由 game-1001 独占（空认领、没切共享）；之后 game-1000 的 A 邀请 B 时
`Ensure` 看到“被别的 game 独占” → `ErrOpening`，投递失败，B 收不到也查不到（`e2e-mutant-read-claims.log`）。
修法：`Invites` 先看所有权，没有记录或不是共享就直接返回空、不进 Nest（还没打开的收件箱必然是空的）。对应场景
`TestFacebookInvitesNeverClaimsAnUnopenedFacebook` 与变异 M1、生成测试里的 recipe 变异。

框架层面：Nest 只读请求也走写准入并认领“无记录”的 Remote 实体，这是现有行为（首写认领），本轮没改框架。对共享模式的伴生
实体来说这是个坑——任何“先读后由别人建”的用法都会踩。是否要在框架里让只读请求不认领，**尚未确认**，交维护者判断（见四）。

## 三、证据

### 先红（改之前的代码，真实依赖）

`e2e-red.log`：A 在 game-1000 建队并邀请，B 连在 game-1001：

```
social_test.go:180: TEAM_INVITE_DELIVERY: player 100002 on sid 1001 got no TeamInvited for team 4611686018427388949 within 15s
--- FAIL: TestAllianceE2ESocialTeamInvite (20.35s)
```

红落在承诺本身：断言是“B 所在的 game 在 15s 内推来对应队伍的 `TeamInvited`”，改之前只在本进程投递，必然收不到。

### 中间态（没有读守卫的第一版，真实依赖）

`e2e-mutant-read-claims.log`：

```
social_test.go:188: TEAM_INVITE_INBOX: player 100002 on sid 1001 listed &{Code:0 Reason: Invites:[]}; want exactly team 4611686018427388953 from 100001
```

### 后绿（真实 Mongo 副本集 / Redis / NATS，syncbus 走 JetStream，两个 game 进程）

`e2e-green.log`：

```
SOCIAL_INVITE team 4611686018427388953 on game 1000: B (game 1001) pushed &{TeamID:4611686018427388953 Inviter:100001 ExpireAtUnixMilli:1794928745657} and listed it, accepted revision 2; C invited offline, listed after relogin on game 1001, joined revision 3
--- PASS: TestAllianceE2ESocialTeamInvite (6.33s)
alliance actual cases: passed=1, skipped=0
```

用例覆盖：B 在线跨 game 收到推送 → 收件箱列出 → 按推来的 TeamID 接受（修订 2）；C 在 game-1001 登录后断开（离线），
被邀请，重新登录后查到并接受（修订 3）。各进程日志无 ERROR 行。

### 关键行变异（`mutate.py`，在装了 team + facebook 的新工程上逐条改一处，`mutations.txt`）

| 编号 | 关键行 | 结果 |
| --- | --- | --- |
| M1 | `Invites`：`if !found \|\| !lease.Shared {`（未打开不经 Nest） | RED `TestFacebookInvitesNeverClaimsAnUnopenedFacebook` |
| M2 | `Ensure`：`if found && lease.Shared {`（已共享只装内存） | RED `TestFacebookEnsureOpensSharedOnce` |
| M3 | `Deliver` 后本进程直接告知 | RED `TestFacebookDeliverRecordsThenTellsEveryGame`（local notices=[]） |
| M4 | `Deliver` 广播 | RED 同上（broadcast=[]） |
| M5 | 收到告知时 `invite.To != msg.Key` | RED `TestFacebookNoticeFromAnotherGameReachesTheFeature` |
| M6 | 组件 `now < invite.GetExpireAt()` | RED `TestFacebookInviteIsListedUntilItExpires` |
| M7 | 组件 `!renewing && dao.InvitesLen() >= MaxInvites` | RED `TestFacebookInvitesAreBoundedAndPruned` |
| M8 | team 控制器 `if invitation.ExpireAt == 0 {` | RED `TestTeamEndpointInvitationNoticeFailureDoesNotUndoAuthority` |
| M9 | `TeamInvites` 只认队伍来源 | RED `TestTeamEndpointInvitationQueryChecksAuthority` |
| M10 | 告知只推本进程在线者 `ActiveSessions(invite.To) == 0` | RED `TestTeamEndpointInvitationNoticeFailureDoesNotUndoAuthority` |

另有真实依赖层面的变异：去掉 M1 的守卫即上面的中间态，真实 e2e 变红。`codegen/internal/roost/module_team_test.go`
固化了 M1、M7、M8 三条（`recipeMutationMustFail`）。

### 验证命令与结果

| 命令 | 结果 |
| --- | --- |
| 新工程 `roost project new … -template game` + `roost module add team rehome` + `roost module verify`（`module-verify-team-rehome.txt`） | facebook 9/9、rehome 50/50、team 31/31、team+rehome 2/2；build / vet / id check / generate --check 全 ok |
| `scripts/test-alliance-e2e.sh`，`ALLIANCE_E2E_RUN='^TestAllianceE2ESocialTeamInvite$'`（共享隔离环境 env，私有 etcd 端口） | 改前 FAIL（红），改后 PASS（绿），见上 |
| `GOWORK=off go vet . ./codegen/internal/roost` | 干净 |
| `GOWORK=off go test -count=1 .`（根包） | ok（19.5s） |
| `GOWORK=off go test -count=1 -timeout 60m -run '^TestModuleTeamGenerated$' ./codegen/internal/roost` | ok（71.3s） |
| `GOWORK=off go test -count=1 -timeout 60m -run '^(TestDemoTeamModuleIntegration\|TestModuleMatchmakingGenerated\|TestModuleAllianceGenerated)$' ./codegen/internal/roost` | ok（234.1s） |
| `scripts/test-alliance-e2e.sh`，`ALLIANCE_E2E_RUN='^TestAllianceE2ESocial(Team\|TeamInvite\|Matchmaking\|Rehome)$'`，期望 4（即 `test-social-real.sh` 的集合） | 4 条全 PASS，passed=4 skipped=0（`e2e-social4.log`） |

没有跑全量 `go test ./...` 与 pretag（按本批共用规则）。

## 四、限制与遗留

### 框架 / 设计固有的限制（可留在 KNOWN-LIMITS）

- 告知是**广播**给所有 game 进程，每次邀请一条 syncbus 消息、每个 game 判一次在线；game 数很大时有放大。
- 告知是尽力而为：普通 NATS 时至多一次，丢了只能靠 `TeamInvites` 补查；推送不是邀请的一部分。普通 NATS 路径只由替身覆盖，
  真实依赖验证的是 JetStream。
- 投递在邀请事务**之后**：邀请成立但确保 / 记录失败时，收件箱里没有，只能重发邀请（与改前同一语义）。
- facebook 由**第一次要写它的 game** 建：邀请不存在的玩家号也会建出一个空的 facebook（有界、只有队长能发）。
- 某 game 认领了 facebook 但还没切共享就崩了，别的 game 确保时一直得到 `ErrOpening`，直到所有权按 Remote 的既有规则释放 /
  被接管（本轮没加专门的恢复，**尚未确认**实际等待时长）。

### 遗留（尚未确认）

> 维护者 2026-11-18 按推荐定：1～4 按本轮的实施（第一次写的 game 建、事务之后投递、广播告知、名字 facebook）。5（框架只读认领）另议。

1. 谁建 facebook（第一次写的 game vs 承载 game 进入时建）。
2. 投递进不进邀请的同一笔事务。
3. 告知广播 vs 按承载服路由。
4. 名字 facebook 是否换成中性名。
5. 框架：Nest 只读请求是否应认领“无所有权记录”的 Remote 实体（本轮在 facebook 里绕开，没改框架）。
6. 社交容量（KNOWN-LIMITS 二的另一半）不在本任务。

## 五、建议文字（协调者合入时用；本 agent 未改 CHANGELOG / KNOWN-LIMITS）

### CHANGELOG `[Unreleased]`

```
### 新增
- 新模块 facebook：每名玩家一个伴生的共享 Remote 实体 Facebook（号由玩家号推出，game 服务、共享模式），
  记跨 game 的邀请收件箱，并经 syncbus 主题 facebook 广播告知所有 game；业务按来源实体 kind 用 facebook.OnInvite 登记告知处理。9 个场景。

### 变化
- team 依赖 facebook：邀请提交后经 facebook 投递，被邀请人连在任何 game 上都收到 TeamInvited，离线的登录后用 TeamInvites 查到；
  接受仍以 Team 上的权威为准。team 场景 35 → 31（收件箱场景移到 facebook）。新增真实依赖用例 TestAllianceE2ESocialTeamInvite。

### 升级
- 存储变化：PlayerDao.Team.Invited 删除，新集合 facebook。只对新生成工程 / 新装 team 的工程生效，旧工程不兼容、手工迁移或重新生成。
```

### KNOWN-LIMITS 二“社交容量与归属”

把“邀请的跨 game 自动投递没有实现”一句删去，改为：

```
- 邀请的跨 game 投递经 facebook 模块（伴生共享 Remote 实体 + syncbus 广播告知）：告知广播给所有 game、尽力而为（普通 NATS 至多一次，
  靠 TeamInvites 补查）；投递在邀请事务之后，失败不撤销邀请；facebook 由第一次写它的 game 建。谁建 / 是否同事务 / 广播还是路由 /
  命名尚未确认（evidence/social-facebook-entity-20261117）。
- 框架：Nest 只读请求会认领“无所有权记录”的 Remote 实体（首写认领），共享模式的伴生实体须在读前看所有权；是否改框架尚未确认。
```
容量一半保持原文。
