# 房间里最后一个人 Leave 时 lobby 答 room_not_found（2026-11-17）

## 现象

C1（C# SDK 传输层抽象）的 agent 观察到：Go `Client.Leave`、C# `LeaveAsync` 都是先在 relay 上发 `Leave` 控制帧、再经 lobby
`POST /rooms/{id}/leave`。房间里只剩最后一个人时，relay 收到 `Leave` 随即关房并报 `room.closed`；lobby 先处理了这条报告，SDK 的
leave 再到时房间已经关了，lobby 照 LOBBY §3“过期”答 `room_not_found`，SDK 的离开于是报错。先后取决于 relay 的报告和 SDK 的 HTTP
请求谁先到 lobby：Go 用例修前 5 次里 3 次在自然先后上就红。

核对三份 SDK 时发现 TS 与描述不同：TS 的 `leave()` 只经 lobby（lobby 再经 NATS 让 relay 断开他），自然先后下不会撞上；但先用
`sendLeave()` 在 relay 上离开、或房间因别的原因（房主走了选不出新房主、过了寿命）已经关了再 `leave()`，一样答 `room_not_found`。

## 选择：方案 1（SDK 侧把 room_not_found 当离开成功）

三个候选：

1. SDK 侧：高层离开（Go `Client.Leave`、C# `LeaveAsync`、TS `leave()`）遇到 lobby 答 `room_not_found` 算成功。
2. 调换顺序：先 lobby leave，再关自己的连接。
3. lobby 侧：`LeaveRoom` 对已关闭的房间返回成功。

选 1，理由：

- **覆盖面最全**：`room_not_found` 的所有来源（relay 关房的竞态、房主走了选不出新房主而关房、relay 崩溃后过了寿命、记录已经过期删除）
  对“我要离开”来说都是目的已达到。方案 2 只消掉“自己发 Leave 帧引起的竞态”，房间因别的原因已关时照样报错；方案 3 只覆盖
  “记录还在、标了 Closed”，记录过了 PX 被删掉后 lobby 仍答 `room_not_found`，要一并改成成功就得把“从没有过的房间号”也答成功，
  打错房间号也悄悄成功。
- **不动 lobby 的 HTTP 契约**：LOBBY §3 明写“join、leave、改状态答 `room_not_found`”，保持不变；不碰 `gone()`、不写记录，
  不会复活或续命房间（“只有 relay 的报告续命”、“重启即丢房”都不受影响）。只调 lobby 接口的客户端仍拿到准确的答复。
- **不改离开的先后**：Go / C# 先发 `Leave` 帧是为了房主离开时 relay 不等宽限期、不依赖 lobby 可达（RELAY §4A 表）；方案 2 会让
  lobby 不可达时房主的离开退化成等 lobby 的 NATS 命令。
- **三份 SDK 一致、改动最小**：每份一处判断（Go 一个 `errors.Is`，C# 一个 `catch … when`，TS 一个 `instanceof` 判断），
  其余 lobby 错误照常返回 / 抛出；直接调 `Lobby.LeaveRoom` / `LeaveRoomAsync` / `leaveRoom` 的行为不变（用例里核对了）。

## 改动

- `client/relay/client.go` `Client.Leave`：`errors.Is(err, ErrRoomNotFound)` 时返回 nil。
- `client/dotnet/Roost.Client.Relay/RelayClient.cs` `LeaveAsync`：`catch (LobbyException error) when (error.Code == LobbyErrorCode.RoomNotFound) { }`。
- `client/ts/src/client.ts` `leave()`：`LobbyError` 且 `code === "room_not_found"` 时不抛（`WebHost.leave()` 经它，一并生效）。
- 文档：LOBBY.md §2.1“离开”加一条；client/README.md（lobby 一条加离开语义；换 host 一条更正“TS 的 `leave()` 只经 lobby”）；
  Go / C# / TS 三份 SDK README 的离开说明与用例清单。

## 用例（先红后绿）

三份都是对真实 relay + lobby 的对端用例，形状相同：两人房（房主 + 一个成员），成员先 Leave，然后

- 房间 a（自然先后）：最后一人（房主）直接 Leave，不报错；之后 lobby 里房间已关（房主改状态答 `room_not_found`）。
- 房间 b（定下先后）：最后一人先只在 relay 上发 `Leave` 帧（Go `SendControl(RelayOpLeave)`、C# `SendControl(RelayOps.Leave)`、
  TS `sendLeave()`），等到断开通知、等 lobby 确认房间已关，再调高层离开，不报错。
- 末尾核对只放过这一种：直接调 lobby 客户端离开一个从没有过的房间号，仍是 `room_not_found`。

位置：Go `client/relay/client_test.go` 的 `TestLeaveByTheLastMemberSucceedsAfterTheRoomCloses`；C# `Roost.Client.Relay.Tests/Program.cs`
的 `LastMemberLeave`（名字 “the last member's leave succeeds after the room closes”，TCP 与 WebSocket 各一遍）；TS
`client/ts/test/relay.test.ts` 的同名用例。

### 修前（红）

Go（`ROOST_REDIS_TEST_ADDR=127.0.0.1:17379 GOWORK=off go test -race -count=5 -v -run TestLeaveByTheLastMember ./client/relay/`，房间目录用共享隔离环境的 Redis）：5 次全红，3 次红在自然先后、2 次红在定下先后：

    client_test.go:362: last member leave: lobby: room_not_found (HTTP 404): lobby: room not found
    client_test.go:376: leave a room that already closed: lobby: room_not_found (HTTP 404): lobby: room not found
    FAIL	github.com/tjbdwanghaibo/roost-core/client/relay	0.392s

C#（`ROOST_RELAY_TESTS_ONLY="last member" dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$PWD"`）：4 次全红在房间 a 的
`LeaveAsync`（Program.cs 第 907 行）；临时把房间 a 的异常吞掉只看房间 b，也红（改动已撤回）：

    FAILED: Roost.Client.Relay.LobbyException: lobby: room_not_found (HTTP 404): lobby: room not found
       at Program.LastMemberLeave(Peer peer) in .../Program.cs:line 907

TS（`scripts/test-client-ts.sh`）：61 个里 1 个红；红在房间 b 的 `leave()`（自然先后的房间 a 本来就过，原因见“现象”）：

    ✖ the last member's leave succeeds after the room closes (8.25025ms)
      Error [LobbyError]: lobby: room_not_found (HTTP 404): lobby: room not found
          at async RelayClient.leave (.../dist-test/src/client.js:535:17)
    ℹ tests 61 / pass 60 / fail 1

### 修后（绿）

- Go：`GOWORK=off go build ./client/... ./service/lobby/... ./wiring/lobby/...`、`go vet ./client/relay/ ./service/lobby/ ./wiring/lobby/` 通过；
  `ROOST_REDIS_TEST_ADDR=127.0.0.1:17379 GOWORK=off go test -race -count=1 ./client/relay/ ./service/lobby/ ./wiring/lobby/`：三个包 ok
  （client/relay 10.3s、service/lobby 7.8s、wiring/lobby 1.5s）；新用例 `-race -count=20` 全过。
- C#：`dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$PWD"` 全部用例 ok，“all 722 checks passed”；只跑新用例连跑 3 次都过。
  跑完 `dotnet build-server shutdown`。
- TS：`scripts/test-client-ts.sh`（node_modules 软链到主检出，跑完已删）：tests 61 / pass 61 / fail 0。

### 变异（撤掉关键一行变红）

把判断里的 room_not_found 换成 room_full（等于不放过 room_not_found），跑新用例，改动已撤回：

- Go `errors.Is(err, ErrRoomFull)`：3 次全红（`client_test.go:376 leave a room that already closed …`、`client_test.go:362 last member leave …`）。
- C# `when (error.Code == LobbyErrorCode.RoomFull)`：红，`FAILED: Roost.Client.Relay.LobbyException: lobby: room_not_found (HTTP 404)`，栈在 `LobbyClient.LeaveRoomAsync`。
- TS `error.code === "room_full"`：tests 61 / pass 60 / fail 1，红在新用例。

## 尚未确认

- 没有跑全量 `go test ./...`、根包与 pretag（按批次规则由协调者最后统一跑）。改动只在 client/ 三份 SDK 与文档，Go 侧不涉及别的包。
- Unity / WebGL 上的 C# `LeaveAsync` 没有在真机或浏览器里跑；改动只是 `catch … when`，与传输无关。
- 高层离开现在吞掉 `room_not_found` 后，调用方无法再从返回值区分“房间早就不在”和“正常离开”。要区分的业务可以直接调 lobby 客户端的
  leave；SDK 没有为此再加返回值（维护者若要区分，可以改成返回一个标志，属于产品选择）。

## 建议写进 CHANGELOG 的文字（由协调者写）

`[Unreleased]` 修复：

- client（Go / C# / TS 开房间 SDK）：房间里最后一个人离开时不再报错。Go `Client.Leave`、C# `LeaveAsync`、TS `leave()` 遇到 lobby 答
  `room_not_found`（房间已关或过了寿命，最常见的是 relay 收到最后一人的 `Leave` 帧随即关房、lobby 的 leave 晚到一步）算离开成功；
  其余 lobby 错误照常返回。lobby 的 HTTP 契约不变，直接调 lobby 客户端的 leave 仍拿到 `room_not_found`。
  证据见 docs/maintenance/evidence/leave-last-member-20261117/README.md。

## 建议写进 KNOWN-LIMITS 的文字

无。问题已修复；TS `leave()` 只经 lobby、与 Go / C# 先发 `Leave` 帧不同，是现有设计（client/README 已更正说明），不是遗留问题。
