# 基础房间 room

`roost module add room` 安装一个持久化的 Room 实体及其 DAO、lifecycle、组件、Nest handler、PB 端点和场景测试。它负责 1～64 个座位、进出、房主交接与连接代际；匹配、回合规则、超时、房间广播和跨房玩家唯一归属由上层玩法组合。

```sh
roost module plan room
roost module add room
roost module verify room
```

安装器分配实体号、组件号、消息号和错误码，补玩家 TCP 骨架与服务 owner 接线。默认 `-template game` 可直接生成和离线验收；真实客户端仍需工程实现鉴权并启用 TCP 配置，这不是房间模块代办的事。安装后的源码归工程；重复安装不覆盖修改。

## 服务端入口

工程的 `game/room` 提供：

```go
// uniqueID 由服务端 ID 分配器分配；不能由客户端任选，也不能复用已关闭/删除的 ID。
err := room.Open(ctx, registry, uniqueID, 4)
```

`Open` 先通过生成的 `RoomLifecycle.GetOrCreate` 加载或创建，再通过 `RoomOpenSender` 初始化。相同容量的重复调用不重置房间；存储错误原样返回，不能当作“不存在”；首次初始化失败可用相同 ID 重试。这里只创建空房，第一位成功加入者成为房主。

`RoomComp()` 的方法只在正式 Nest 持锁事务里调用；事务外使用生成的 `game/handler/syncsender`。开房与席位写入都是 strict 提交：提交失败回滚，不提前确认。普通 Entity 的落库仍遵守框架现有 WAL / checkpoint 契约，模块没有新增持久化通路。

## 客户端协议与重连

| 协议 | 请求字段 | 行为 |
| --- | --- | --- |
| RoomJoin | RoomID | 加入一个已开房间；同玩家、同物理连接重放不占第二席，也不换代际 |
| RoomView | RoomID | 只有该玩家的当前连接可以查看成员快照 |
| RoomReconnect | RoomID、Epoch | 新连接携带上次确认的代际做 CAS；成功分配新代际，同一连接重放返回原结果 |
| RoomLeave | RoomID、Epoch | 退出自己的当前席位；房主退出后由最小座位号的成员接任，最后一人离开即关闭 |
| RoomClose | RoomID、Epoch | 只有当前房主的当前连接可关闭并清空成员 |

Join / Reconnect / View 返回 `Info{RoomID, Capacity, HostID, Epoch, Seats}`；Epoch 只属于请求者，各席位只公开玩家号、座位号与连接事件状态。所有响应都有 `Code / Reason`。PB 中没有操作者 ID 或连接 nonce：操作者从认证 Context 取，nonce 从 Session 的 `ConnectionID()` 取。直连 TCP 与 Gate 代理均支持；自定义接入适配器必须提供每条物理连接唯一、不能复用的可信 ID，不能以账号 SessionID 代替。

客户端保存 RoomID / Epoch。连接改变后调用 Reconnect，不能用重复 Join 接管旧席位。不同新连接并发用相同 Epoch 重连，至多一条成功；回包丢失而物理连接未变时可重放同一请求。丢失本地 Epoch 的账号恢复、邀请或私房权限应由业务另加受信入口，不能去掉 CAS 或暴露其他成员代际。

房间的 EpochSeed 持久化且只增不减，退出后重入也不复用。考虑 BSON 的整数范围，到 `MaxInt64` 拒绝再分配。连接 nonce 与代际一起检查：旧请求即使猜到新 Epoch，也不能操作新连接。

## 断线、恢复与容量边界

接入层记录每条连接成功 Join / Reconnect 的 `(roomID, playerID, epoch)`，关闭事件使用当时的值调用：

```go
changed, err := room.Disconnect(ctx, nestClient, roomID, playerID, epoch)
```

迟到或重复的旧关闭事件返回 `false`，不影响新连接；提交错误仍须按业务的重试策略处理。断线只改状态，保留座位和房主。模块暂不自动订阅网络关闭事件；接入方需处理“关闭先于 Join 回包”的登记竞态，不能只在回包后无条件放进一个连接表。

`Connected` 是最后已处理的连接事件事实，不是跨进程重启的在线权威。恢复后接入层需按当前连接重建状态，或让客户端显式重连；掉线保留时长、踢人、重启时的房间清理由玩法决定。单个房间有唯一 owner，不提供跨节点接管。

基础准入允许已认证玩家加入任意已开的未满房间；需要邀请码、匹配名单等限制时在同一 Nest 事务内加业务检查。没有全服房间目录，也没有“一名玩家只能处于一个房间”的跨实体约束。快照通过 PB 按请求返回，不自动广播、不安装 EntitySync 订阅；玩法广播必须安排在提交成功后。

## 验证口径

22 条离线场景覆盖容量、幂等、座位复用、房主交接、关闭终态、CAS 竞争、旧连接请求/关闭、PB 编解码、成员权限、BSON 恢复、代际上限，以及 Join / Reconnect / Leave / Close 提交失败的完整回滚。生命周期端点用正式 ManagerAccess、Nest、生成 Sender 和替身提交器；加载失败不能创建空实体。

安装器另测实体号避让、已有文件冲突、生成失败回滚、归属接线、重复安装与业务文件保留。Core 的 TCP → Gate → Game 用例验证连接 nonce 能随可信会话传到业务层；它不等于房间 PB 在真实网络、Mongo、Redis 下的联合验收。
