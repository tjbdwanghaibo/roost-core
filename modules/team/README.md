# team：可跨 game 进程的队伍——建队、邀请、入队、离队、解散，成员表与玩家引用同一笔事务

队伍是一个 Remote 共享权威实体（Team，实体标记 `remote=managed ownership=shared`，生来共享、不能退出共享，见 [共享 kind](../../docs/framework/REMOTE_ENTITY.md#共享-kindownershipshared)）：成员表只有一份，放在 Team 上，多个 game 进程都能在事务里读写它；每名玩家身上只存“我在哪支队伍”的引用。邀请经被邀请人的 Facebook（依赖的 [facebook](../facebook/README.md) 模块，同样是共享 Remote 实体）投递，被邀请人连在哪个 game 上、在不在线都收得到。

## 能力

- **客户端协议**（`protocol/def/team.go`，七个请求加一个推送；原六个请求的应答是 `TeamResponse{Code, Reason, Info}`，`Info` 是 `TeamView{TeamID, Leader, Revision, Members}`）
  - `TeamCreate`（`TeamCreateRequest{}`）：自己当队长建一支队。已经在队伍里时返回现有队伍，不再建。
  - `TeamInvite`（`TeamInviteRequest{Invitee}`）：队长邀请一名玩家，`Invitee` 是玩家号。
  - `TeamAccept`（`TeamAcceptRequest{TeamID}`）：被邀请的人入队，`TeamID` 是完整实体号。
  - `TeamLeave`（`TeamLeaveRequest{}`）：离队。队长离队时队长交给玩家号最小的成员；最后一人离开后队伍留下一个空壳，号不复用。
  - `TeamDisband`（`TeamDisbandRequest{}`）：队长解散队伍，清空成员与邀请。
  - `TeamInfo`（`TeamInfoRequest{}`）：读自己所在的队伍。
  - `TeamInvites`：查本人 Facebook 收件箱里的有效队伍邀请（收件箱各来源合计最多 64 条，别的 game 投来的也在）；逐条经 Nest 核对共享 Team 权威，过滤已过期、已接受、已解散、已满员、匹配冻结的邀请。一次权威查询失败就返回错误码，不伪装完整列表。
  - `TeamInvited`：邀请提交后经 `facebook.Deliver` 记进被邀请人的 Facebook 收件箱，并告知全部 game 进程；被邀请人连着的那个进程推送 `{TeamID, Inviter, ExpireAtUnixMilli}`。投递失败不撤销权威邀请。
  - 操作人与所在服的 sid 都来自认证会话和本服务配置，客户端不能指定。
- **服务端入口**
  - Player 上的 Team 组件（逻辑、类型号 `team.CompTypeTeam` 与工厂登记在 `game/components/team/team.go`，对外接口 `view.Team`；handler 收生成的 `view.ITeamEntity`，经 `Team()` 取得）：`TeamID() int64`、`Create(target, sid)`、`Accept(target, sid, now)`、`Invite(target, invitee, now)`、`Leave(target, disband)`、`Info(target)`。除 `TeamID()` 外都同时修改 Team 与 Player，调用方的事务必须同时声明两者。
  - Team 实体（`game/entities/team/entity.go`，直接挂 `teamroster.CompTypeTeamRoster`）上的成员表组件 TeamRoster（`game/components/teamroster/teamroster.go`，类型号与工厂登记也在那里；handler 收 `view.TeamEntity`，经 `TeamRoster()` 取得）：`Info() view.TeamSnapshot`（`{TeamID, Leader, Revision, Members, SIDs}`，锁内读出的值拷贝）、`IsMember(id)`、`HoldMatch(key)` / `ReleaseMatch(key)`（匹配期间冻结名单，见“联动”）。
  - Nest handler（`game/handler/`，参数是 `view.TeamEntity` 与 `view.ITeamEntity`）：`handlerPlayerTeam`（只读本人的队伍号：`sender.NewPlayerTeamSender(client).Sync_PlayerTeam(ctx, playerEntityID)`）、`handlerTeamCreate`、`handlerTeamInvite`、`handlerTeamAccept`、`handlerTeamLeave`、`handlerTeamInfo`（后五个都是 `[Team, Player]` 两个实体的事务，`rollback=undo`（持久级别取缺省 async），例如 `sender.NewTeamCreateSender(client).MultiSync_TeamCreate(ctx, teamEntityID, playerEntityID, sid)`）。
  - Controller（`game/controllers/team.go`）：七个 `HandleTeam…` 端点；建队准备（从 Mongo 的 `_team_id_sequence` 集合取号、创建空的 Remote 实体、`ClaimRemoteOwnership` 认领）发生在事务之外。Team 声明为共享 kind，认领的那一条插入就是共享记录，不经过建队进程独占的中间态，也不再切共享；认领答出非共享记录（本进程的登记没有声明共享）时建队报错。
- **实体事件**（`game/event/team.go`，在 Player 上发布；别的模块在自己的组件或 Player 的方法上标 `//roost:listen [priority=N]` 监听，`roost generate` 按优先级排好顺序）
  - 拦截事件 `event.TeamJoining{TeamID}`（`game/components/team/team.go` 的 `checkJoin`）：建队与入队的事务内准入检查，先于任何写入。监听返回错误就拒绝这次入队；只做检查、不改状态、不做 I/O。现在的监听：matchmaking 20、team 的 rehome 联动 10（沿用改成事件之前按名字字母序的顺序）。
  - `event.TeamChanged`（`game/components/team/changed.go`）：队伍关系变化时发布，字段是变化前后两份 `view.TeamSnapshot` 的值拷贝（`TeamID/Leader/Revision/Members/SIDs` 与 `Before…`，`team.ChangedSnapshots(ev)` 还原成两份快照）和 `Refresh`；`Refresh` 为真表示只是当前玩家核对了一次（`TeamInfo` 等），不是名单变了。在同一笔事务里，监听出错整笔回滚；监听里不能做 I/O，要发通知得用提交后的机制。
- **状态**（`db/def/team.go`）
  - `TeamDao`（集合 `team`，主库）：`Leader`、`Members`（玩家号 → 所在服 sid）、`Invites`（玩家号 → 过期时刻）、`MatchKey`、`Revision`。只落库。
  - `PlayerDao.Team`（`TeamState{TeamID}`）：只落库、不同步。客户端查自己的队伍用 `TeamInfo`，查邀请用 `TeamInvites`。收到的邀请记在被邀请人的 Facebook 上（facebook 模块的 `FacebookDao.Invites`），只用于发现，不是接受邀请的权威。
- **配置**：没有配置表。规则是 `game/components/teamroster/teamroster.go` 里的三个常量，归工程所有：`MaxMembers = 8`、`MaxInvites = 64`、`InviteMillis = 60000`（邀请 60 秒有效）。
- **错误码**（`internal/errors/team.go`）：`team_invalid`、`team_missing`（不在这支队伍 / 队伍不存在）、`team_exists`（已经在别的队伍）、`team_not_leader`、`team_not_invited`（没被邀请或邀请已过期）、`team_full`、`team_invites_full`、`team_blocked`（队伍正在匹配中被冻结、与换服互斥等）。

**日志**：建队、邀请、加入、离开 / 解散确认；返回快照时记录队长、版本和成员，错误补 player_id。离队 Sender 不返回新队长，因此不另查快照伪称本次交接结果。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/team.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **通知范围**：邀请提交之后记进被邀请人的 Facebook 收件箱（任何 game 都能写），再广播告知全部 game 进程，被邀请人连着的那个推 `TeamInvited`；离线的被邀请人登录后用 `TeamInvites` 查到。投递在邀请的事务之后，确保收件箱、记录、广播或推送失败只记 Debug，调用方邀请已经成交；重试邀请可再次投递，可能重复推送，客户端按 TeamID 合并。普通 NATS 的广播至多一次，丢了由 `TeamInvites` 补查。没有成员变化推送。
- **不做的事**：没有踢人、主动转让队长、申请入队、拒绝邀请、队伍聊天。查询不预留席位，接受时仍须核对 Team 当前权威。
- **数量上限**：一支队至多 8 人；同时有效的邀请至多 64 份（过期的邀请在下一次邀请时清掉）；邀请 60 秒后过期，恰好到期那一刻就算过期。邀请不占座位，接受时才检查容量。一名玩家同一时刻只能在一支队伍里。
- **一致性**
  - 建队、入队、离队、解散都在一笔 `[Team, Player]` 事务里：成员表的变化与操作者本人的引用一起提交或回滚。
  - 重复请求是安全的：已是成员时再 `TeamAccept`、已建成时再 `TeamCreate`，都不改事实、返回现状。
  - 解散与队长离队**不会**在同一笔事务里改其他成员的档：其他成员身上的引用是惰性清除的，在他们下一次 `TeamInfo` / `TeamLeave` 核对权威成员表时清掉。没核对之前，残留的引用会保守地挡住换服（装了 rehome 时）。
  - 建队的准备阶段在事务之外：取号成功但事务没成、或并发的首次建队，可能留下建到一半的空实体（共享的空认领，不卡任何人）；持久号不复用。
- **前提**：依赖 facebook 模块（安装时一起装上）；需要 nest 与 remote_entity 两个 Mod（安装时自动补上 remote_entity）；运行时需要 Mongo、Redis、NATS；服务配置里的 sid 必须为正。玩家侧入口要求 Player 已经建好（通常先装 enter）。
- **已知限制**（详见 `docs/maintenance/KNOWN-LIMITS.md`）：“team 的边界”一条——当前只验收了离线的 Nest 事务与生成的 Remote 提交格式，真实 Redis / Mongo / NATS、独立 Gate、跨进程的队伍都没有验收。

## 复用方式

- **需求正好是它**：`roost module add team`。新工程通常先装 enter（要鉴权再装 login），通过 `EnterGame` 建好 Player 后再请求这些端点。流程：`TeamCreate` → 队长 `TeamInvite` → 被邀请方接收 `TeamInvited` 或查询 `TeamInvites` → `TeamAccept`；其他成员操作前先 `TeamInfo`。
- **需求多一点**
  - 改人数、邀请数、邀请时长：改 `game/components/teamroster/teamroster.go` 顶部的三个常量。
  - 加入队条件（等级、冷却、正在副本里不能入队）：在自己的组件上监听 `event.TeamJoining`（只读）。
  - 名单变了要更新别的东西（队友视野分组、缓存）：在自己的组件或 Player 上监听 `event.TeamChanged`；通知用提交后的机制发，不在监听里做 I/O。
  - 加踢人 / 转让队长：照 `game/components/teamroster/teamroster.go` 里 `Leave` 的写法加方法（记得推进 `Revision`、检查 `MatchKey`），再加协议与 `[Team, Player]` 的 handler。被踢的人的引用同样只能惰性清除。
  - 不要改 `TeamDao` 的 store 键与集合名、`EntityCategoryRemote` 的实体类别（它保证锁序先于 Player）、实体标记上的 `ownership=shared`（去掉后建队认领出的是建队进程独占的队伍，别的 game 写它被 fence，建队直接报错）、建队时“先取号建空实体并认领、再进事务”的次序。
- **别的模块 / 业务代码要用它**
  - 读某名玩家的队伍号（事务外）：`id, err := sender.NewPlayerTeamSender(client).Sync_PlayerTeam(ctx, playerEntityID)`。
  - 在自己的事务里要整队名单：handler 的目标参数写 `view.TeamEntity`，调 `target.TeamRoster().Info()`，名单与各成员的 sid 都在返回的 `view.TeamSnapshot` 里。
  - 要在一段时间内冻结名单（排队、进副本）：同一笔事务里调 `HoldMatch(key)`，结束后用同一个 `key` 调 `ReleaseMatch(key)`；冻结期间邀请、入队、离队、解散都答 `team_blocked`。
- **不适合直接用的情形**：只在单个进程内存在、不需要落库的临时小队（那用 room 更轻）；上百人的团队或公会（用 alliance）；需要实时名单推送的队伍界面（要自己加通知）。值得借鉴的是“共享权威只有一份成员表，玩家只存引用，引用惰性核对”的做法，以及用准入钩子让两个模块互相挡住对方（见 rehome 联动）。

## 联动

- facebook（team 依赖它，不是 `with/`）：邀请提交之后 `facebook.Deliver` 记进被邀请人的 Facebook 并告知全部 game 进程；`init` 里 `facebook.OnInvite(team.EntityKindTeam, …)` 登记推 `TeamInvited` 的处理；`TeamInvites` 经 `facebook.Invites` 读收件箱。代码在 `game/controllers/team.go`。
- `team/with/rehome`：两个方向的事务内检查——在队伍里时拒绝换服（team 组件监听 `event.RehomeApplying`，priority 10，答 `team_blocked`），换服意图未清除时拒绝建队与入队（team 组件监听 `event.TeamJoining`，priority 10）。2 条场景。
- matchmaking（依赖 team，不是 `with/`）：在 `[Team, []Player]` 事务里用 `HoldMatch` 冻结名单、保存每名成员的排队意图，确定终态后用同一个键 `ReleaseMatch`；另外 matchmaking 组件监听 `event.TeamJoining`（priority 20）：排队中的玩家不能建队或入队。
- 别的模块没有 `with/team` 目录。game-demo 用 `event.TeamChanged` 更新分区场景的队伍视野缓存（那段代码在 demo，不在模块里）。

## 验证范围

- `roost module verify team`：32 条场景（`^TestTeam`，包 `./game/handler` 24 条、`./game/controllers` 8 条）。覆盖：建队与入队同一笔事务、准入失败回滚建队与双方成员、同一条提交记录同时含 Remote 的变化与 Player 的变化、重复接受与重复建队不改事实、必须有邀请、邀请恰好到期即失效、只有队长能邀请与解散、容量在接受时检查、邀请有界且可续期、过期邀请释放名额、队长交给最小号成员、最后一人离开留下不可复用的空壳、解散后其他人的引用在读权威时清除、不能加入第二支队、快照自有集合、并发接受被串行化、离队 / 解散被拒时双方恢复、非法 sid、外人不能读或改名单、投影钩子失败整笔回滚；端点要求已认证玩家、拒绝空请求、拒绝错误类型的实体号与非法被邀请人、Controller 使用实体运行时的 lifecycle、建队认领一步就是共享记录（Team 声明了共享 kind、不调切共享、认领答出独占记录时建队报错）。
- 邀请投递：查询只认收件箱里的队伍邀请并过滤权威已失效的；收件箱记不上时照样告知在线的被邀请人，推送失败不改变邀请，不在本进程的不推、不冷加载；别的 game 广播来的告知走同一个推送。收件箱自身的回滚、过期、容量与迟到告知在 facebook 模块的场景里。
- rehome 联动另有 2 条（`^TestWithTeamRehome`）。
- 上述模块场景用真实 Nest 引擎与生成的 DAO / Sender，提交器、Remote 外部锁与权威回执是内存替身。另有 `scripts/test-social-real.sh` 的 `TestAllianceE2ESocialTeam`：私有 Mongo / Redis / NATS 上两个 game sid 共享同一 Team，经真实玩家 TCP 建队、邀请、跨进程接受；toxiproxy 丢接受回包与 80±20ms 下行延迟下重连重试，核对双方同一成员表 / 版本、队长转移，以及跨 sid 整队匹配拒绝后无意图。本场景用已知 TeamID 接受；跨 game 的邀请投递另由 `TestAllianceE2ESocialTeamInvite` 验（A 在 game 1000 邀请连在 game 1001 的 B：B 收到推送、查得到并接受；离线的 C 再登录后查到并接受）。两条都不验证独立 Gate、容量或持续压力；[#154 归属决定](../../docs/maintenance/KNOWN-LIMITS.md)不因本场景关闭。证据见 [B-7 报告](../../docs/maintenance/evidence/social-real-20261114/README.md)。
- 仓库侧的安装验收是 `codegen/internal/roost` 里的 `TestModuleTeamGenerated`（含缺少 remote_entity 时自动补齐、重复安装不改文件）。
