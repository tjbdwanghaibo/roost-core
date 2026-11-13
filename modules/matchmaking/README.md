# matchmaking：本服的单人 / 整队匹配，成局后移交给匹配房

玩家（或队长带着整支队伍）在自己所在的 game 服排队，模块经托管的 match 服务配对，成局后用 `matchroom` 开一间固定名单的房间，把房号写回每名成员的档。排队意图是持久的：断线、回包丢失、进程重启之后，用查询协议就能把同一次排队接着做完。

## 能力

**客户端协议**（`protocol/def/matchmaking.go`，四个请求共用应答 `MatchmakingResponse{Code, Reason, Sequence, Key, Phase, RoomID, MatchID, Members}`）

- `MatchEnqueue{Sequence, Team}`：开始排队。`Sequence` 必须是 `MatchStatus` 返回的当前值加一；重发同一次排队仍用原 `Sequence`。`Team=true` 表示队长带整队排队。
- `MatchCancel{Key}`：取消当前这次排队。取消与成局谁先谁后由 match 服务的原子 Cancel / Commit 决定，应答的 `Phase` 可能是 `matched`。
- `MatchStatus{}`：查询并推进。它不只是读：每次调用都会把停在半路的步骤接着做（补发 Enqueue、补绑票、补开房、补写结果）。
- `MatchClear{Key}`：清掉已经结束的那次排队（`cancelled` / `expired` / `matched`），之后才能排下一次。`matched` 要等房间关闭之后才清得掉。

`Phase` 的取值在 `game/entities/player/matchmaking_component.go`：`preparing`、`dispatching`、`queued`、`cancelling`、`matched`、`cancelled`、`expired`；空串表示没有排队。`Key` 是 `"<队长玩家号>:<Sequence>"`。

**服务端入口**（`game/matchmaking/runtime.go`，都在 Nest 事务之外调用，会等待 match 服务与 Nest）

- `matchmaking.FromRegistry(registry) (*Runtime, error)`：取模块启动时发布的运行时（能力名 `matchmaking.runtime`）。
- `(*Runtime).Enqueue(ctx, playerID, sequence, asTeam)`、`Cancel(ctx, playerID, key)`、`Status(ctx, playerID)`、`Clear(ctx, playerID, key)`：四个协议各自对应的入口，返回 `player.MatchSnapshot`。
- `(*Runtime).State(ctx, playerID)`：只读这名玩家当前的匹配快照，不推进。
- `(*Runtime).Step(ctx)`：做一轮配对。模块的 `Start` 每 500 毫秒调一次，单轮上限 5 秒。
- `NewRuntime(client, match, rooms, sid, resident)`：显式装配与离线验收用。

**Nest handler**（`game/handler/match_*.go`，由 `Runtime` 经生成的 Sender 调用，不对客户端开放）：`MatchBegin`（单人记意图）、`MatchTeamBegin`（队伍与全部成员同一笔事务记意图并冻结名单）、`MatchUpdate`（全部成员同一笔事务推进阶段）、`MatchClear`、`MatchRelease`（解除队伍名单冻结）、`MatchState`（只读）、`MatchRoomClosed`（只读：这一局的匹配房关了没有）。写入的 handler 都是 `rollback=undo durability=strict`；两个只读 handler 声明 `durability=memory`（不开事务、不取回滚快照、不写 WAL，在里面改持久字段会 panic）——`MatchState` 是客户端轮询每次都走的路。

**登记点**

- `player.OnMatchmakingStarting(name, func(*Player) error)`（`matchmaking_component.go`）：开始排队前的检查。在记排队意图的那笔 Player 事务里、对组里每名成员各执行一次；返回错误即拒绝，整组什么都不写。只在 `init` 里登记，空名、nil、重名直接 panic，按名字顺序执行。
- 模块自己登记了 team 的 `OnTeamJoining("matchmaking", …)`：排队中（`Phase` 非空）的玩家不能建队或入队。

**状态**：`PlayerDao.Matchmaking`（`db/def/matchmaking.go` 的 `MatchmakingState`），只落库、不下发给客户端。每人只保留当前意图与最后结果（`Sequence`、`Key`、`Leader`、`TeamID`、`Members`、`SID`、`Phase`、`TicketID`、`MatchID`、`RoomID`、`Deadline`）；`Sequence` 只增不清零。队伍一侧的名单冻结记在 team 模块的 `Team.MatchKey`。

**配置**：没有配置表与配置键。匹配策略写在 `game/matchmaking/matcher.go` 与 `runtime.go` 的 `queue`（队列模式 `module-duel`、`GroupSize: 2`、分区 `sid-<sid>-size-<人数>`），归工程所有。

**错误码**（`internal/errors/matchmaking.go`）

- `matchmaking_invalid`：请求或成局结果不成样子（序号、sid、名单不对）。
- `matchmaking_busy`：玩家或队伍已有匹配 / 换服意图；队员不在本服或不在线；匹配房还开着所以清不掉。
- `matchmaking_stale`：`Sequence` 或 `Key` 不是当前的，或阶段已经变了。
- `matchmaking_unknown`：票已经绑定，但 match 服务里查不到它或它的成局记录，需要人工核对。
- 整队排队还会原样带回 team 模块的码（没有队伍、不是队长）。

**日志**：持久意图受理及阶段变化确认，记录玩家、请求键、阶段、MatchID / RoomID；unknown 记 Warn，内部错误补 player_id。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/matchmaking/controller.go`, `game/matchmaking/runtime.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：没有 MMR、没有按等级 / 分数配对、没有跨池扩圈、没有跨服房间路由、没有补位；不推送匹配结果（客户端靠 `MatchStatus` 轮询，建议间隔 500 毫秒左右）；不决定胜负、不发奖励；成局后只开房，不替玩家入房（客户端自己发 `RoomJoin`）。
- **匹配规则是固定的**：一张 match 票代表一支完整队伍（单排是一人队伍），同一个 sid、同样人数的两支队伍成一局。2 人队不会被拆开，也不会与 1 人队拼局。
- **上限**：队伍至多 8 人（`BeginMatching` 与 team 的 `MaxMembers`），所以一间匹配房至多 16 人；每个池每轮至多读 64 张票（`matcher.go` 的 `Candidates(ctx, q, 64)`）；人数从 1 到 8 共 8 个池，每 500 毫秒各扫一遍。没有吞吐承诺。
- **一致性**：
  - 队伍的名单冻结与全部成员的排队意图在 `[Team, []Player]` 一笔严格事务里提交或回滚；之后每一步阶段推进也是全体成员一笔事务。
  - 与 match 服务、开房之间不是一笔事务，靠持久阶段加重试对齐：`preparing → dispatching` 的转换只选定一次 Enqueue；Enqueue 回包丢了只查候选、不重发；Commit 成功而开房或写结果失败时，下一次 `MatchStatus` 用同一个 `MatchID` 补齐，不会开第二间房。
  - 从未绑定过票的意图在 60 秒后（`Deadline`）可以安全过期（`abandon`）。这个期限不适用于已经绑定票的排队。
  - **票已绑定而 match 的历史已经没了时**，答 `matchmaking_unknown`，意图与名单冻结都保留，不自动解除。match 已结束的历史缺省只保留 10 分钟，查询与恢复要在保留期内完成。
- **前提**：依赖 `team`、`matchroom`（连带 `room`）；业务服务要 `uses: [match]`（托管的 match 服务）并有 Nest Mod 与玩家 TCP 接入；进程要有正数的 `sid`。排队的全部成员必须驻留在本进程（实体在本进程的 EntityManager 里）且队伍记录的 sid 都是本服，否则答 `matchmaking_busy`。模块只在单个 game 进程内配对与开房。
- **启动窗口**：协议在 Mod 的 Provide 阶段注册，运行时在模块启动时才发布；这之间到达的请求明确失败，不排队。
- **已知限制**：见 [KNOWN-LIMITS“R7 匹配模块的当前边界”](../../docs/maintenance/KNOWN-LIMITS.md)——历史消失时保留屏障、默认每池窗口 64、不跨服、匹配房的持久映射不自动清理、离线验收不能替代真实 Redis / Mongo / NATS 联合验收。

## 复用方式

- **需求正好是它**：`roost module add matchmaking`，连带装上 `team`、`matchroom`、`room`，并接入托管 match 的客户端与模块的后台循环。客户端先完成 `EnterGame` 再排队。
- **需求多一点**：
  - 改配对规则（按人数以外的条件分池、一局几支队伍）：改 `game/matchmaking/matcher.go` 的 `Step` / `form` / `eligible` 与 `runtime.go` 的 `queue`。注意 match 服务的 `GroupSize` 在这里数的是队伍数，不是玩家数。
  - 加“什么时候不许排队”：登记 `player.OnMatchmakingStarting`。
  - 成局后要做别的事（通知、初始化玩法）：改 `matcher.go` 的 `accept`，在结果写进成员之后做；它可能被重复调用，要按 `MatchID` 幂等。
  - 不要改 `matchmaking_component.go` 里 `UpdateMatching` 的阶段转换与 `handler/match_*.go` 的事务边界，恢复逻辑依赖它们。
- **别的代码要用它**：
  - 读一名玩家的匹配状态：`matchmaking.FromRegistry(registry)` 之后 `runtime.State(ctx, playerID)`；已经持有 Player 锁时直接 `p.Dao().GetMatchmaking().GetPhase()`（空串即没有排队）。
  - 服务端替玩家发起 / 取消：`runtime.Enqueue(ctx, playerID, sequence, asTeam)`、`runtime.Cancel(ctx, playerID, key)`。
- **只适合借鉴的情形**：跨服匹配、带分数的匹配、一局多于两支队伍且人数不等。值得借鉴的是“持久意图 + 每步幂等 + 查询即恢复”的做法（`runtime.go` 的 `resume`），以及用 `Sequence` 挡住旧请求重放。

## 联动

- `matchmaking` + `rehome`（`with/rehome/game/components/matchmaking/matchmaking_rehome.go`）：有匹配状态（`Phase` 非空）时拒绝换服；档里有换服意图时拒绝排队。两条检查都在各自的 Player 事务里。
- 队伍相关的限制由 `team` + `rehome` 的联动负责，不在这里。
- 本模块是 `matchroom` 的调用方、`room` 的 `OnRoomJoining` 的间接使用者（经 `matchroom`）。

## 验证范围

- `roost module verify matchmaking`：17 条场景（`game/handler` 13 条、`game/matchmaking` 2 条、`game/controllers/matchmaking` 2 条）；与 `rehome` 的联动另有 2 条。
- 场景是离线的：真实的 Nest 引擎与 match 的 `MemoryStore`，Remote / WAL 提交是替身，开房用测试夹具里的单间房；没有经过真实的 Redis、Mongo、NATS 与 TCP。
- 覆盖：单排与两队成局、取消与 Commit 的两种先后、掉线后补查并重连、重复结果、Enqueue 回包丢失、未绑定意图到期、历史到期不解除屏障、队伍冻结与整组回滚、旧序号、活房不能清、端点可在模块启动前注册、两个只读 handler 的事务声明是 memory 快路径（核对生成的注册代码）。
- turn-pvp 配方另有一条经 TCP / Robot 的“排队到回合”链路，房号映射用 mongotest；真实依赖的联合验收没有做。

## 客户端流程

1. `MatchStatus` 拿到自己的 `Sequence`；新一轮发 `MatchEnqueue{Sequence: 当前+1}`，重发仍用原值。
2. 组队先走 team 的 `TeamCreate`、`TeamInvite`、`TeamAccept`，再由队长发 `MatchEnqueue{Team: true}`。
3. 用 `MatchStatus` 轮询。`matched` 之后应答带 `RoomID` / `MatchID`，客户端用 room 的 `RoomJoin` 入房；断线重连仍用 room 的 `RoomReconnect`（连接标识与 `Epoch`）。
4. 取消带着当前 `Key` 发 `MatchCancel`。不能把“发出了取消”当成“取消成功”，以应答的 `Phase` 为准。
5. `cancelled` / `expired` 可以直接 `MatchClear`；`matched` 要等 `RoomClose` 或最后一名成员离开使房间关闭之后才能清。每名成员各清各的。清掉之后 `Sequence` 保留，旧请求不能重新排队。

断线不等于退出队伍、撤销排队或丢弃已成局的结果。

## 未知结果与恢复

- 只有绑定到全部成员的票才会被选中成局。外部的孤票即使晚到也不会被本模块选中，最终由 match 的票据过期清理。
- 已绑定的票用 match 的票据 / 成局查询恢复。重复提交不重复开房；房间即使已经关闭也能确认原来的移交身份，不会重新打开。
- `matchmaking_unknown` 不是取消也不是超时：要运营按存储与业务记录核对之后处理，不能删账本强行重排。
- Player 不积累请求历史。匹配房的映射是独立的持久文档，清理要与业务的留存策略一起定。
