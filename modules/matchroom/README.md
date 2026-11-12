# matchroom

`roost module add matchroom` 安装 room 和匹配房组件。可信匹配逻辑在 Nest 之外调用：

```go
rooms, err := matchroom.FromRegistry(registry)
if err != nil { return err }
roomID, err := rooms.Open(ctx, matchID, []int64{playerA, playerB})
```

MatchID 必须是可信成局身份，成员是已核实的玩家唯一号。客户端没有此入口，只能通过 RoomJoin 操作自己的席位。

## 持久移交

`_match_rooms` 使用 MatchID 作为唯一键，保存 RoomID、SID 和排序后的完整名单。先读已有映射；没有时从与 Room 相同物理库的 `_room_id_sequence` 原子分配房号，再只插入映射。并发插入的输家读取赢家；未知写结果留给下次查同一个键恢复。允许跳号，不复用。

工程中其他直接调用 room.Open 的分配器必须使用同一 `_room_id_sequence`，或使用经过论证的互不重叠号码空间，不能随手从 1 开始另一套计数器。此模块不提供跨节点房间接管，相同 MatchID 的不同 sid / 名单被拒绝。

保留房号后经正式生命周期 GetOrCreate、MatchroomOpen Sender 初始化 Room。名单、容量、匹配身份同事务；失败可重试，不能把计数器/映射写成功当作 Room 已初始化。已存在的普通房不能改成匹配房。同一匹配重复通知不重置席位，已经关闭的匹配房只确认原身份，不重开。

## 入房与断线

room 的 OnRoomJoining 是事务内准入登记点，matchroom 是实际使用者。匹配名单之外的人不能入房，尚未连接的成员也保有资格；断线保留现有席位，重连继续用 Room 的 Epoch/连接校验。名单不可修改，成员离开不向旁人转让资格。

自动断线订阅、掉线保留时间、重启在线状态重建与广播沿用 room 的既有后续需求，本模块没有另造一套实现。

## 验证边界

定向场景覆盖名单、重复移交、断线、非法输入、提交回滚与开房失败重试；生成工程跑 build/vet/ID/生成漂移与 race。turn-pvp TCP 场景经过正式计数器/映射代码，数据库用 mongotest；不等于真实 Mongo 故障恢复验收。历史映射不做自动 TTL 删除，清理前必须确认没有仍能重放的业务引用。
