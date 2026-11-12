# friends：同一个 game 进程内、双方在线时的好友、申请与黑名单

好友关系是两名玩家各自档里的名单。任何一次修改都同时锁住两个 Player、在一笔 Nest 事务里写两边，所以两边不会不一致；代价是只有双方都在同一个 game 进程里在线时才能改。

## 能力

### 客户端协议（`protocol/def/friends_change.go`、`friends_list.go`）

- `FriendsChange`（请求 / 应答）：`FriendsChangeRequest{PeerID, Action}`，应答只有 `Code` / `Reason`。操作者取自已认证的会话，不能伪造。`Action` 的取值（常量在 `game/entities/player/friends_component.go`）：

  | 值 | 常量 | 做什么 |
  | --- | --- | --- |
  | 1 | `FriendsApply` | 申请：写进对方的 `Incoming`。重发不写、不占第二格；任一方把对方拉黑就拒绝 |
  | 2 | `FriendsAccept` | 同意：要求自己收到过对方的申请；同时写两边的好友，并清掉双方之间的申请 |
  | 3 | `FriendsRemove` | 删除：同时移除两边的好友关系与双方之间的申请 |
  | 4 | `FriendsBlock` | 拉黑：在操作者的黑名单里记对方，并做一次删除 |
  | 5 | `FriendsUnblock` | 取消拉黑：只删黑名单，不恢复旧好友或旧申请 |
  | 6 | `FriendsReject` | 拒绝：只删自己收到的这条申请 |

- `FriendsList`（请求 / 应答）：请求体为空，返回自己的 `Peers`（好友）、`Incoming`（收到的申请）、`Blocked`（黑名单），都是玩家号、升序。

### 服务端入口

- `syncsender.NewFriendsChangeSender(nestClient).MultiSync_FriendsChange(ctx, actorEntityID, peerEntityID, action)`：多实体 Nest 调用，参数是两个完整的 Player EntityID。调用者必须自己确认两名玩家都属于本进程。
- `syncsender.NewFriendsListSender(nestClient).Sync_FriendsList(ctx, entityID)`：返回 `player.FriendsSnapshot{Peers, Incoming, Blocked}`（排序后的值拷贝）。
- 组件 `FriendsComp()`：`Change(peer *FriendsComponent, action int32) error` 只能在同时锁住双方的事务里调；`Snapshot()` 只能在本人的实体锁内读。
- Nest handler（`game/handler/friends_change.go`、`friends_list.go`）：`handlerFriendsChange`（两个 Player，`rollback=undo durability=strict`）、`handlerFriendsList`（只读）。
- 端点 `HandleFriendsChange` / `HandleFriendsList` 在 `game/controllers/friends`。

### 登记点

无。关系变化没有钩子，也没有推送。

### 状态

`PlayerDao.Friends`（类型 `FriendsState`，`db/def/friends.go`）：`Peers`、`Incoming`、`Blocked` 三个名单，键是对方的完整 EntityID。只落库、不同步；客户端只能经 `FriendsList` 看到自己的名单。

### 配置

没有配置表与配置键。名单上限是 `friends_component.go` 里的常量 `FriendsLimit`（64）。

### 错误码（`internal/errors/friends.go`）

`friends_invalid`（操作不合法、对象是自己、未知的 `Action`，或对方不在本进程在线）、`friends_full`（名单已满）、`friends_blocked`（任一方拉黑了对方）、`friends_application_missing`（同意时没有对应的申请）。

**日志**：关系操作确认，记录 action 与对方玩家号；内部错误补 player_id。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/friends/friends_change.go`, `game/controllers/friends/friends_list.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **只在同一个 game 进程内、双方都有已认证 TCP 会话时能改。** 端点用本进程玩家传输的 `ActiveSessions` 核对操作者与对方；任何一方没有本地会话就答 `friends_invalid`，六种 `Action` 都一样——包括取消拉黑、拒绝申请、删除好友，对方不在线时也做不了。读自己的名单不受这个限制。
- **没有跨服好友、离线申请。** 不为远端玩家建本地镜像，没有 Saga 或邮件降级的路径。会话检查不是分布式锁：检查之后遇到实体迁移 / 交出，由 Nest 的实体归属拒绝并返回失败。
- **不带展示资料与通知。** 名单里只有玩家号，没有名字、等级、在线状态；收到申请、被同意、被删除都没有推送，对方要自己再拉 `FriendsList`。不含好友聊天、赠送、推荐、备注、分组。
- **上限：** 三个名单各 64 人。申请时检查双方的好友数与对方的 `Incoming` 数，同意时检查双方的好友数，拉黑时检查操作者的黑名单数。
- **一致性与幂等：** 两个 Player 在同一笔 `durability=strict` 的事务里；任何检查失败都不写，提交失败两边一起回滚。重复的申请、对已是好友的再次同意都是零写入。存储只有当前名单，没有关系历史、申请的过期时间。
- **前提：** nest Mod 与玩家 TCP 接入（模块安装时声明 `access.transport: tcp`）。没有别的业务模块依赖。实体的归属必须是工程的正式接线，不能让两个进程写同一个实体。
- **已知限制：** `docs/maintenance/KNOWN-LIMITS.md` 的“场景模块的边界”一条写明 friends 要求双方本进程在线。

## 复用方式

- **需求正好是它**（单服、小规模、在线互加）：`roost module add friends`，不连带别的模块。
- **需求比它多一点：**
  - 改名单上限：改 `game/entities/player/friends_component.go` 的 `FriendsLimit`。
  - 名单里要带名字、等级：`FriendsList` 只给玩家号，客户端按玩家号另取；或者在端点里对每个玩家号查自己的展示数据再拼进应答。
  - 关系变化要通知对方：在 `HandleFriendsChange` 事务成功之后自己推送（模块没有推送协议）。
  - 允许对离线玩家做“只改自己一侧”的动作（取消拉黑、拒绝申请）：要改端点里的在线检查并为这两个动作另写只锁一个实体的 handler；现有的 `Change` 假定两边都锁住。
- **别的业务代码要判断两人是不是好友：** 在锁住其中一名玩家的 Nest handler 里读 `target.FriendsComp().Snapshot()`，或者直接调 `Sync_FriendsList`。要以服务端身份改关系：调 `MultiSync_FriendsChange`，并自己保证两人都在本进程。
- **只适合借鉴的情形：** 跨服好友、离线也能加好友、好友数很大的社交系统不适合直接用——这些需要独立的关系服务或异步的申请队列。值得借鉴的是“双方名单在一笔两实体事务里写，先检查全部容量与权限、再写两边”。

## 联动

无。本模块没有 `with/`，也没有别的模块带 `with/friends`。

## 验证范围

`roost module verify` 跑 12 条场景（`^TestFriends`，`game/handler` 10 条、`game/controllers/friends` 2 条），没有子测试。覆盖申请幂等、同意与交叉申请的清理、没有申请时拒绝同意、对称删除、拉黑后双向拒绝、取消拉黑不恢复好友、拒绝只消费收到的申请、三类名单的上限、两实体提交失败一起回滚、非法或不存在的对象零写入，以及端点拒绝不在本进程的对方、操作者取自会话、只列自己的名单。

场景是离线的：用真实的 Nest、生成的 DAO / Sender / 协议 / controller，提交器与“在线会话”是替身。没有经过真实 TCP、真实 Mongo / WAL、跨进程与长时间运行的验证。仓库一侧另有安装与可编译变异的专项测试（`codegen/internal/roost/module_naming_friends_test.go`），它不属于 `roost module verify`。
