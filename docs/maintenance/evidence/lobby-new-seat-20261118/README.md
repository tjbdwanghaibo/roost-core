# RV-20261118-06：新预留被上一个席位的旧在线报告记成“连上过”

基线：main `8a2cf3a9`。审查来源：`88d55951:docs/maintenance/MAIN-REVIEW-20261118.md` 第 06 条，探针 `review_new_seat_test.go.txt`。

## 根因

- [rooms.go](../../../../service/lobby/rooms.go) `JoinRoom` 给新加入的人建席位时 `PresenceSeq` 为 0。同一账号上一个席位已删除（relay 报 seq 3 主动离开），他在开局前再 join 只拿到预留；上一个席位的 seq 2 在线报告晚到，[events.go](../../../../service/lobby/events.go) `notePresence` 只比较 `event.Seq > Member.PresenceSeq`（2 > 0），把新预留记成 `Connected`。开局、预留过期后 `holdsSeat` 因 `State == started && Connected` 一直留座，他还能按重连回座。
- 同类路径检查时确认的第二处：房主经 lobby 离开（席位不动、记 `leaving`、`Connected=false`，但这个离开没有序号），leave 命令到达 relay 之前他自己先掉线，relay 的确认是 `member_left` 原因 `disconnected`（seq 3）。seq 3 清掉 `leaving` 后，离开前那条连接的 seq 2 心跳晚到，`2 > PresenceSeq(1)` 又把他记成 `Connected`；换完 host 后已开始的房间仍给他留座、可再 join。

## 修复

- `JoinRoom` 新建席位时 `PresenceSeq = room.RelaySeq`（建席时的房间水位）。新连接在签出令牌之后才发生，relay 在房间锁内给快照占序号（`service/relay/room.go` `reportLocked`），含新连接的报告 seq 必然大于这个水位，所以下界不吞真连接。
- `HandleRelayEvent` 在 `member_left`（seq 更大）清 `leaving` 之前：账号仍在 `leaving` 且还占席位时，把它记成这个 seq 上的主动离开（`Connected=false`、`PresenceSeq=event.Seq`）。
- 契约同步到 [LOBBY.md](../../../framework/LOBBY.md) §“lobby 的处理”。

同类路径的结论：

- 被踢后重进：被踢是永久名单，`JoinRoom` 与验权都拒绝，不会建新席位，无此问题。
- 房间重建：`CreateRoom` 每次是新房间号、`RelaySeq=0`；relay 已报告过的房间不许在新进程上重建（验权 `room_not_found`），无此问题。
- 房主席位补回（`HandleRelayEvent` 末尾 `room.memberIndex(room.Host) < 0`）：只在“经 lobby 离开的人又被 relay 选成房主”时出现，此前的事实都被 `leaving` 挡住；`leaving` 由 relay 确认清掉时走上面第二处修复。只有他随后又 join（清 `leaving`）这一支，旧报告是否应算“连上过”属于语义选择（他的旧连接在被选成房主时确实连着），未改，保留现状。

## 红（未修改的 rooms.go / events.go / lobby.go，只加新用例）

```
source ~/.roost-it/roost-dataengine-it/env.sh; export ROOST_REDIS_TEST_ADDR=$ROOST_DATAENGINE_IT_REDIS_ADDR GOWORK=off
go test -count=1 -v -run 'TestANewSeatIgnoresLateReportsAboutThePreviousSeat|TestALobbyLeaveConfirmedByADisconnectStaysALeave' ./service/lobby
```

```
    lobby_test.go:1076: seats after the new reservation expired = "acc-host:online acc-alice:offline", want only the host
    lobby_test.go:1079: alice joining the started room she never connected to: err = <nil>, want ErrRoomStarted
--- FAIL: TestANewSeatIgnoresLateReportsAboutThePreviousSeat (0.01s)
    --- FAIL: TestANewSeatIgnoresLateReportsAboutThePreviousSeat/left_through_relay (0.00s)
        --- FAIL: .../left_through_relay/memory (0.00s)
        --- FAIL: .../left_through_relay/redis (0.00s)
    --- FAIL: TestANewSeatIgnoresLateReportsAboutThePreviousSeat/left_through_lobby (0.00s)
        --- FAIL: .../left_through_lobby/memory (0.00s)
        --- FAIL: .../left_through_lobby/redis (0.00s)
    --- PASS: TestANewSeatIgnoresLateReportsAboutThePreviousSeat/reconnected (0.00s)
    lobby_test.go:1110: seats after the old host left through the lobby = "acc-host:offline acc-alice:online", want only the new host
    lobby_test.go:1113: old host joining the started room he left: err = <nil>, want ErrRoomStarted
--- FAIL: TestALobbyLeaveConfirmedByADisconnectStaysALeave (0.00s)
    --- FAIL: TestALobbyLeaveConfirmedByADisconnectStaysALeave/memory (0.00s)
    --- FAIL: TestALobbyLeaveConfirmedByADisconnectStaysALeave/redis (0.00s)
FAIL	github.com/tjbdwanghaibo/roost-core/service/lobby	0.556s
```

`reconnected` 是对照（新预留之后真的连上过再掉线，应留座），修前修后都应通过。审查探针 `TestReviewPriorSeatReport` 在基线上同样红（memory 与 redis 两个后端：`after reservation expired: acc-host:online acc-alice:offline`、`rejoin = <nil>, want ErrRoomStarted`），修后通过；探针未提交。

## 绿

```
go test -count=1 -v -run '<同上>' ./service/lobby      # 两条用例全部子测试 PASS（memory + redis）
go test -race -count=3 ./service/lobby                 # ok  20.363s（含 Redis 后端）
go vet ./service/lobby
```

## 变异（修复后逐一改坏，跑两条新用例）

| 变异 | 改动 | 结果 |
| --- | --- | --- |
| M1 | `JoinRoom` 新席位去掉 `PresenceSeq: room.RelaySeq` | 红：`left_through_relay`、`left_through_lobby`（memory + redis） |
| M2 | 确认分支条件加 `&& false`（不再把经 lobby 离开记成主动离开） | 红：`TestALobbyLeaveConfirmedByADisconnectStaysALeave` |
| M3 | 确认分支只清 `Connected`、不记 `PresenceSeq` | 红：`TestALobbyLeaveConfirmedByADisconnectStaysALeave` |
| M4 | 新席位下界改成 `RelaySeq + 1`（过高） | 红：对照 `reconnected`（`seats ... = "acc-host:online", want her seat kept`） |

M4 说明下界取“水位本身”而不是更高：水位之后的第一条报告就可能是新连接。
