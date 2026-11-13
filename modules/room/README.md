# room：有限座位的持久房间（进出、房主交接、重连代际）

一个自带 `Room` 实体的基础房间：服务端开房，玩家加入、查看、重连、离开，房主可以关闭。它只管“谁坐在哪个座位、谁是房主、每个座位现在认哪条连接”，匹配、回合规则、广播都由上层模块组合。

## 能力

**客户端协议**（`protocol/def/room.go`；操作者一律取自认证会话，协议里没有玩家号与连接标识）

| 协议 | 请求字段 | 做什么 |
| --- | --- | --- |
| `RoomJoin` | `RoomID` | 加入一间已开的房。同一玩家、同一条物理连接重发不占第二席，也不换代际 |
| `RoomView` | `RoomID` | 成员用自己当前的连接读成员快照 |
| `RoomReconnect` | `RoomID`、`Epoch` | 换了连接之后，带着上次确认的代际做 CAS 接管自己的席位；成功分配新代际，同一条连接重发返回原结果 |
| `RoomLeave` | `RoomID`、`Epoch` | 退出自己的席位。房主退出后由座位号最小的成员接任；最后一人离开，房间关闭 |
| `RoomClose` | `RoomID`、`Epoch` | 只有当前房主用当前连接可以关闭房间并清空成员 |

Join / Reconnect / View 的应答带 `RoomInfo{RoomID, Capacity, HostID, Epoch, Seats}`：`Epoch` 只是请求者自己的代际，别人的不给；每个席位只公开 `PlayerID`、`Slot`、`Connected`。所有应答都有 `Code` / `Reason`。

**服务端入口**（`game/room/room.go`）

- `room.Open(ctx, registry, uniqueID, capacity) error`：开一间空房。`uniqueID` 由服务端分配、不能复用；先经生成的 lifecycle `GetOrCreate`，再经 `RoomOpen` 初始化。同样容量重复调用不重置房间；初始化失败可以用同一个号重试。第一位成功加入的人成为房主。
- `room.Disconnect(ctx, nestClient, roomID, playerID, epoch) (bool, error)`：接入层在连接关闭时调，把那个席位标成断线。`epoch` 必须是这条连接最后一次成功 Join / Reconnect 拿到的值；迟到或重复的旧关闭事件返回 `false`，不影响新连接。
- `room.EntityID(roomID) (int64, error)`：把房间唯一号换成 Room 的完整 Entity ID。
- `room.ConnectionKey(session) (string, error)`：从服务端 Session 的 `ConnectionID()` 取连接标识。

**Nest handler**（`game/handler/room_*.go`，写入的都是 `rollback=undo durability=strict`）：`RoomOpen`、`RoomJoin`、`RoomView`、`RoomReconnect`、`RoomDisconnect`、`RoomLeave`、`RoomClose`。事务之外用生成的 `syncsender`（例如 `syncsender.NewRoomJoinSender(client).Sync_RoomJoin(ctx, id, playerID, connection)`）；`RoomComp()` 的方法（`Open`、`Join`、`Reconnect`、`Disconnect`、`Leave`、`Close`、`View`）只能在持有 Room 锁的事务里调用。

**登记点**：`room.OnRoomJoining(name, func(room.Owner, playerID int64) error)`（`game/components/room/join_checks.go`，`room.Owner` 即 `view.RoomEntity`）。在 `Join` 的事务里、检查房间已开之后、占座之前执行；返回错误即拒绝入房。不能做 I/O。只在 `init` 里登记，空名、nil、重名直接 panic，按名字顺序执行。`matchroom` 用它校验名单。

**状态**：模块自带实体 `Room`（`game/entities/room/entity.go`，类别是 World）与 `RoomDao`（`db/def/room.go`，集合 `room`）：`Capacity`、`Closed`、`HostID`、`EpochSeed`、`Seats`（玩家号 → `RoomSeat{Slot, Epoch, Connected, Connection}`）。只落库，不同步；成员经 `RoomView` 读 PB 快照。连接标识不出现在任何对外的快照里。

**配置**：无。容量上限是代码常量 `MaxSeats = 64`（`game/components/room/room.go`）。

**错误码**（`internal/errors/room.go`）：`room_invalid`（参数或操作不合法）、`room_unopened`（还没初始化）、`room_closed`、`room_full`、`room_not_member`、`room_stale_epoch`（代际或连接对不上）、`room_not_host`。

**日志**：玩家加入、离开、重连、关闭、断线确认记 Info；服务端开房记 Debug。加入返回房主；离开不返回新房主，不为日志追加查询。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/room/room_close.go`, `game/controllers/room/room_join.go`, `game/controllers/room/room_leave.go`, `game/controllers/room/room_reconnect.go`, `game/room/room.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **不做的事**：不做匹配与邀请码；没有全服房间目录；不约束“一名玩家只能在一间房里”；不广播、不装 EntitySync 订阅（成员变化要别人知道，由玩法在提交成功之后自己推）；不订阅网络关闭事件（要接入层自己调 `room.Disconnect`）；不踢人、不设掉线保留时长、不做重启后的房间清理；不分配房号。
- **客户端不能开房**：开房只有服务端入口。缺省准入是“任何已认证的玩家都能加入任何已开、未满的房”，要限制就登记 `OnRoomJoining`。
- **上限**：容量 1 到 64；连接标识至多 128 字节；一间房只有一个房主。`EpochSeed` 只增不减，到 `MaxInt64` 之后拒绝再分配。
- **一致性**：开房与每一次席位变更都是 Room 上的一笔严格事务，提交失败时座位、房主与代际一起回滚，不提前确认。同一个代际的并发重连至多一条成功。已关闭的房号永不再开。
- **`Connected` 不是在线权威**：它是“最后处理过的连接事件”。进程重启之后不会自动重建，接入层要按当前连接重建，或让客户端显式重连。接入层还要处理“关闭事件先于 Join 回包”的登记竞态。
- **连接标识必须可信**：来自服务端 Session（直连 TCP 与 Gate 代理都提供）。自定义接入适配器要给每条物理连接一个唯一、不复用的标识，不能用账号的 SessionID 代替，否则旧连接能操作新席位。
- **客户端要保存 `RoomID` 与 `Epoch`**：换了连接之后只能 `RoomReconnect`，不能靠重复 `RoomJoin` 接管旧席位。客户端丢了本地 `Epoch` 的恢复途径要业务另加受信入口。
- **前提**：业务服务要有 `nest` Mod；有协议所以要玩家 TCP 接入（安装时代办骨架）。鉴权与启用监听仍归工程。一间房只属于一个 game 进程，不提供跨节点接管。
- **已知限制**：[KNOWN-LIMITS“room 的边界”](../../docs/maintenance/KNOWN-LIMITS.md)。

## 复用方式

- **需求正好是它**：`roost module add room`。安装器分配实体号、组件号、消息号与错误码，实体包的 `lifecycle_gen.go` 由 roost generate 产出，补玩家 TCP 骨架。没有依赖。
- **需求多一点**：
  - 入房条件（邀请码、名单、等级）：登记 `OnRoomJoining`。
  - 房内玩法状态：给 `Room` 加自己的组件与状态（像 `matchroom`、`turn` 那样声明 `entity: Room`），在自己的 handler 里与席位一起提交。
  - 断线处理：在接入层的连接关闭回调里调 `room.Disconnect`；踢人与超时用自己的定时器加 `RoomLeave` 的服务端等价逻辑。
  - 房号：自己写分配器，或者装 `matchroom` 用它的 `_room_id_sequence`。
  - 不要改 `game/components/room/room.go` 里代际与连接的 CAS 检查。
- **别的代码要用它**：开房 `room.Open(ctx, registry, uniqueID, capacity)`；读成员快照用 `syncsender.NewRoomViewSender(client).Sync_RoomView(ctx, id, playerID, connection)`（要那名成员当前的连接标识）；在自己的 Room handler 里直接用 `target.RoomComp()`。
- **只适合借鉴的情形**：大规模场景（用 scene 与视野模块）、需要观战或不占座旁观、需要跨进程迁移的房间。值得借鉴的是“席位代际 + 连接标识”的重连 CAS，以及断线事件带代际、迟到的旧事件伤不到新连接。

## 联动

本模块没有 `with/` 联动。依赖它的模块：`matchroom`（登记 `OnRoomJoining` 做名单准入）、`turn`（回合玩法挂在 Room 上）、`matchmaking`（经 `matchroom` 开房，清匹配结果前核对房间已关）。

## 验证范围

- `roost module verify room`：22 条场景（`game/handler` 与 `game/controllers/room`）。
- 离线：真实的 Nest 引擎、生成的 Sender、正式的 ManagerAccess 与 lifecycle，提交器是替身（可以注入提交失败）；没有 Mongo、Redis 与真实网络。
- 覆盖：容量、幂等、座位复用、房主交接、关闭终态、CAS 竞争、旧连接的请求与关闭事件、PB 编解码、成员权限、BSON 往返恢复、代际上限，以及 Join / Reconnect / Leave / Close 提交失败后的完整回滚；加载失败不会建出空实体。
- 安装器另测实体号避让、已有文件冲突、生成失败回滚、重复安装与业务文件保留。
- 没有验证：房间协议在真实 Mongo / WAL 下的联合验收。连接标识随可信会话经 TCP → Gate → Game 传到业务层，在 core 的用例里验证过；配方的双人房间端点跑过真实 TCP，Account 与持久提交仍是替身。

只读声明：RoomView 成员快照 使用 `//roost:nest durability=memory`，仍在正式 Nest 实体锁内执行，不取持久状态回滚快照、不写 WAL。裸 `//roost:nest` 的默认值是 state / async，不能与 memory 混同；带状态推进、回执写入或引用校正的入口保持写事务声明。
