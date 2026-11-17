# 修复：离开房间被在途的 relay 报告撤销（交接 A1，2026-11-17）

排查与复现见 [client-relay-flake-20261117](../client-relay-flake-20261117/README.md)（按它推荐的修法 A 实施）。本目录只记修复和验证。

## 问题

成员经 lobby 离开（`POST /rooms/leave`）之后，lobby 如果再处理到 relay 在他离开之前发出的一条报告（`seq` 更大、`online` 里还有他），`applyOnline` 会把他当成“目录里没有、relay 说在线”的人补回目录，并记成 `connected`。在 relay 的下一条报告纠正之前：

- 他再 join 走“已在房间里”的幂等分支，不查密码、人数和“已开始”；
- 已开始的房间里，他被当成掉线成员留座。

被踢的人不受影响（有被踢名单）。

## 修法

房间记录加一个名单 `leaving`（`Room.Leaving`），记“经 lobby 离开、relay 还没确认走了”的账号：

- **记**：`LeaveRoom` 时记下（成员离开并发了 `leave` 命令；房主离开）。
- **作用**：`applyOnline` 不把名单里的人补回目录，也不把留在目录里的房主重新记成 `connected`。
- **补发**：处理一条新的报告时，名单里的人仍在 `online` 里，就在写入成功后再向 relay 发一次 `leave` 命令（第一次可能丢了，或者 relay 处理在这条报告之后）。
- **清**：relay 报 `room.member_left`（任何原因，只认 `seq` 更大的）；或这个账号重新 join（新座位或回原座）。名单随房间记录过期。

改动：`service/lobby/lobby.go`（字段与三个小方法）、`rooms.go`（join 清、leave 记）、`events.go`（报告处理：清、补发、`applyOnline` 两处判断）。

## 测试（先红后绿）

`service/lobby/lobby_test.go`，内存与 Redis 两种目录都跑：

1. `TestAStaleOnlineReportAfterALeaveDoesNotBringTheLeaverBack`（来自 A1 证据的确定性用例）
   - `leave_then_start`：没开局时离开、处理到在途的旧名单，房主开局，他再 join 必须答 `room_started`。
   - `leave_a_started_room`：已开始的房间里离开、处理到在途的旧名单，他不在目录里，再 join 答 `room_started`。
   - 修前：两个子用例在内存、Redis 上都红（他被补回，join 成功）。修后绿。
2. `TestLeavingRecordIsResentAndCleared`：旧名单里还有他时 `leave` 命令再发一次（共两次）；relay 报 `member_left` 后名单清空；离开后重新 join 名单清空，之后的报告照常把他记成 `connected`。

变异（逐个撤掉，再跑上面两个用例，都红；记录见协调者的私有副本）：

| 变异 | 结果 |
| --- | --- |
| M1 `applyOnline` 补回时不查 `leaving` | 两个用例都红 |
| M2 不补发 `leave` | 用例 2 红 |
| M3 join 不清名单 | 用例 2 红 |
| M4 `LeaveRoom` 不记名单 | 两个用例都红 |
| M5 `member_left` 不清名单 | 用例 2 红 |

## 回归

- `go test -race -count=3 ./service/lobby/ ./wiring/lobby/ ./service/relay/... ./wiring/relay/ ./client/relay/`：ok（含 Redis）。
- `go test -tags integration -race -run TestRealRelay ./wiring/relay/`：ok。
- TS SDK `scripts/test-client-ts.sh`：51 pass；C# relay SDK：345 项通过。

## 尚未确认

- 2026-11-17 sdk 组那一次 `./client/relay` FAIL 是不是这个问题，原因行已丢，仍不能确定（A1 证据里写过）。修复后按 A1 的做法没有再跑 `-count=50` 的长跑。
- 经 lobby 离开、但 relay 上本来就没连着的人，relay 不会再报他的 `member_left`，名单里的这一条留到他重新 join 或房间过期。它只挡住“迟到的在线名单把他补回”，不影响别人，没有另外清理。
