# matchroom：把一次成局幂等地移交给一间固定名单的房间

匹配逻辑得出“这一局是这些人”之后调它：它按成局身份（MatchID）持久地保留一个房号，开出一间容量等于人数的 `room`，并把名单写进房间。此后只有名单里的人进得了这间房。同一个 MatchID 不论移交多少次，得到的都是同一间房。

## 能力

**客户端协议**：无。玩家只通过 `room` 模块的 `RoomJoin` 等协议操作自己的席位。

**服务端入口**（`game/matchroom/matchroom.go`，在 Nest 事务之外调用，会等待 Mongo 与 Nest）

```go
rooms, err := matchroom.FromRegistry(registry)                  // 取 *matchroom.Rooms
roomID, err := rooms.Open(ctx, matchID, []int64{playerA, playerB}) // 返回房间唯一号
```

- `Open(ctx, matchID, members) (int64, error)`：`matchID` 是可信的成局身份，`members` 是核实过的玩家唯一号。返回成功时名单与容量已经提交到 Room。失败可以用同样的参数重试。
- `Binding`：持久映射的一行（`MatchID`、`RoomID`、`SID`、`Members`）。

**Nest handler**（`game/handler/matchroom_open.go`）：`MatchroomOpen(matchID, members)`，`rollback=undo durability=strict`，由 `Open` 经生成的 Sender 调用。组件方法 `MatchroomComp().Open(matchID, members)` 与 `MatchroomComp().Closed(matchID)`（这一局的房关了没有，给可信流程核对用）只能在持有 Room 锁时调用。

**登记点**：模块自己不提供登记点。它登记了 room 的 `OnRoomJoining("matchroom", …)`（`game/entities/room/matchroom_component.go`）：房间有 MatchID 时，名单之外的玩家入房被拒，答 room 的 `room_not_member`。检查在 Room 的入房事务里。

**状态**

- `RoomDao.Matchroom`（`db/def/matchroom.go` 的 `MatchroomState`：`MatchID`、`Members`），只落库、不同步。名单不可变。
- Mongo 里与 Room 同一个物理库的两个集合：`_match_rooms`（以 MatchID 为 `_id`，存 RoomID、SID、排好序的完整名单）与 `_room_id_sequence`（房号计数器，文档 `_id: "room"`）。

**配置**：无。

**错误码**：模块没有自己的错误码。组件拒绝时用 room 的 `room_invalid`、`room_not_member`、`room_closed`；`Open` 入口的拒绝（参数不对、映射冲突）是不带业务码的普通错误。

## 能力边界

- **不做的事**：不做匹配；不替成员入房（成员自己发 `RoomJoin`）；不通知成员；不改名单、不补位、成员离开不把资格让给别人；不提供跨节点接管——同一个 MatchID 在另一个 sid 上或带着不同名单再来，被拒绝（`matchroom: conflicting handoff`）。
- **上限**：名单 2 到 64 人（room 的 `MaxSeats`），不能有重复或非正数的玩家号；`matchID` 非空且至多 128 字节。
- **幂等与恢复**：
  - 先读已有映射；没有才从 `_room_id_sequence` 原子取号，再插入映射。并发插入的输家读赢家的那一行。房号允许跳号，不复用。
  - “保留了房号、Room 还没初始化”是可以恢复的中间态：再调一次 `Open` 会经 lifecycle 的 `GetOrCreate` 与 `MatchroomOpen` 补齐。计数器或映射写成功不等于房已经开好，以 `Open` 返回成功为准。
  - 同一局重复移交不重置席位与代际；房间已经关闭时只确认原身份，不重开。
  - 已经开过的普通房（容量非 0 而没有 MatchID）不能改造成匹配房。
- **映射不自动清理**：`_match_rooms` 没有 TTL。清理前要确认没有还能重放的业务引用。
- **房号空间**：工程里别的代码若直接调 `room.Open` 自己分配房号，必须用同一个 `_room_id_sequence`，或用经过论证互不重叠的号段，否则会撞号。
- **断线与在线状态**：断线保留席位、重连用 room 的 `Epoch` 与连接校验——这些都是 room 的行为，本模块没有另做。自动断线订阅、掉线保留多久、重启后的在线状态重建、房内广播都没有。
- **前提**：依赖 `room`；业务服务要有 `nest` 与 `mongo` 两个 Mod；进程要有正数的 `sid`。只在拥有这间 Room 的那个 game 进程里有效。
- **已知限制**：[KNOWN-LIMITS“R7 匹配模块的当前边界”](../../docs/maintenance/KNOWN-LIMITS.md)（映射不自动清理、不跨服）与“room 的边界”。

## 复用方式

- **需求正好是它**：`roost module add matchroom`，连带装上 `room`。要现成的匹配就装 `matchmaking`（它依赖本模块）。
- **需求多一点**：
  - 自己写匹配、邀请或 GM 开房：拿到可信的成局身份后调 `rooms.Open(ctx, matchID, members)`，把返回的房号发给成员。
  - 要在入房时加别的限制：登记 room 的 `OnRoomJoining`，不要改 `matchroom_component.go` 的名单检查。
  - 房间关了之后才允许做某事（清匹配状态、结算）：在 Room 的 handler 里用 `MatchroomComp().Closed(matchID)`。
- **别的代码要用它**：`matchroom.FromRegistry(registry)` 取 `*Rooms`；`rooms.Open(ctx, matchID, members) (roomID int64, err error)`。
- **只适合借鉴的情形**：名单会变的房间（中途加人、观战席）、要跨进程迁移的房间。值得借鉴的是“外部身份 → 本地资源号”的持久映射写法：唯一键插入定胜负，取号与初始化分开、各自可重试。

## 联动

本模块没有 `with/` 联动。`matchmaking` 依赖它：成局后调 `Open`，清匹配结果前用 handler `MatchRoomClosed`（在 matchmaking 模块里）经 `Closed` 核对房间已关。

## 验证范围

- `roost module verify matchroom`：12 条场景（`game/handler` 6 条、`game/matchroom` 6 条）。
- 离线：`game/handler` 的场景用真实 Nest 引擎加替身提交器；`game/matchroom` 的场景里映射与开房多数是函数替身，持久映射的并发重试一条用 mongotest（内存里的 Mongo 替身）。
- 覆盖：名单外的人入房被拒且不占座、重复移交不动席位与代际、掉线成员仍可入房与重连、改名单与普通房改造被拒、提交失败整体回滚、已关闭的房只确认身份、开房失败后用同一个房号重试、冲突映射不开房、非法名单不取号、取号失败不开房、缺依赖时启动失败。
- 没有验证：真实 Mongo 的故障恢复、跨进程。turn-pvp 配方的 TCP 场景经过正式的计数器与映射代码，数据库同样是 mongotest。
