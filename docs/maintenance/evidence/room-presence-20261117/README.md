# RV-20261116-01：room 连接登记随提交、按代际更新（2026-11-17）

审查报告：`origin/fix/core-game-server-review` 的 `docs/maintenance/MAIN-REVIEW-20261116.md` RV-20261116-01（P2）。探针按现行布局（`game/controllers` 单包、`view.IRoomEntity`）改写成模块场景。

## 根因

断线订阅的登记表 `Presence` 原来由控制器在收到 Nest 成功回包之后更新：

- `HandleRoomJoin` / `HandleRoomReconnect` 收到成功结果才 `Track`；`Track` 没有代际比较，并先删同玩家在这间房的别的连接；
- `HandleRoomLeave` 成功后 `Forget(roomID, playerID)` 无条件撤掉该玩家的全部登记。

Nest 只串行化房间上的事务，不保证控制器在收到结果之后仍按提交顺序运行；同步等待取消（`ErrNestCanceled`）也不撤销已经准入的事务。所以：

1. 旧 Join（同连接重复 Join）的回包迟到，`Track(epoch=1)` 覆盖了新 Reconnect 的 `epoch=3` 登记；
2. 旧 Leave 的回包迟到，`Forget` 撤掉了同玩家新 Join 的登记；
3. 等待被取消、Join 照样提交，控制器跳过 `Track`，席位在线却没有登记。

`Presence` 的补扫只扫已登记的连接，三种情况下连接关了席位都一直 `Connected=true`。

## 改法

- Join / Reconnect / Leave / Close 的 Nest handler（`modules/room/files/game/handler/room_*.go.tmpl`）在组件成功之后调 `TrackCommitted` / `ForgetCommitted` / `ForgetRoomCommitted`（`game/modules/room/room_presence.go`），它们经 `nest.AfterCommit` 在事务提交之后更新本进程正在运行的 `Presence`（`Start` 设、`Close` 撤；回滚不执行）。控制器不再碰登记。等待被取消不影响：提交后回调在 Nest worker 上执行。
- 提交后回调在解锁之后执行，不保证按提交顺序，所以 `Presence` 按席位代际单调：每个（房间, 玩家）只记一条；`Track` 只认比现有登记（或墓碑）新的代际；`Forget(roomID, playerID, epoch)` 只撤代际不超过 `epoch` 的登记并留墓碑；`ForgetRoom` 留关房标记。墓碑与标记 1 分钟后在补扫时清掉。
- Leave handler 只在确实撤掉了 `epoch` 这一代席位时（事务内 `Member(playerID, epoch)` 成立）才 `ForgetCommitted`，重复退出时客户端给的 `epoch` 不进登记表（否则一个巨大的 epoch 墓碑能挡住之后的入房登记）。
- 没有改框架。

## 先红后绿

生成工程：`roost project new p1 -module example.com/rp -template game --skip-deps`，`go mod edit -replace` 指向本检出，`roost module add room`。三条回归写在 `modules/room/files/game/controllers/room_test.go.tmpl`（真实控制器、生成的 Sender、真实 Nest；连接层与提交器是替身）：

| 场景 | 做法 |
| --- | --- |
| `TestRoomPresenceLateJoinReplyKeepsTheReconnectedSeatTracked` | 扣住旧 Join 的成功回包（Nest 已提交），另一条连接 Reconnect 拿到代际 3，再放行旧回包；两条连接都关，席位必须断线 |
| `TestRoomPresenceLateLeaveReplyKeepsTheRejoinedSeatTracked` | 房里留 102；扣住 101 的 Leave 回包，101 用新连接重新入房（代际 3），再放行；连接都关，席位必须断线 |
| `TestRoomPresenceCanceledWaitStillTracksTheCommittedJoin` | 提交器在 strict 提交里停住，取消请求 ctx（控制器拿到 `ErrNestCanceled`），放行提交；真实 View 确认席位在线，连接关了必须断线 |

- 修前（修复前模板生成的工程，`go test -count=1 -run 'TestRoomPresence(LateJoin|LateLeave|CanceledWait)' ./game/controllers/`，三轮）：三条 3/3 FAIL，均为 “every connection of player 101 closed, but the seat stays online after sweeps; 1 tracked”（唯一剩下的登记是观察者）。见 [red-before-fix.txt](red-before-fix.txt)。修前 `roost module verify room` 29 条通过（[verify-before.txt](verify-before.txt)）。
- 修后（新模板重新生成的工程，`-race`，三轮）：三条与原有 `TestRoomClosedConnectionMarksTheSeatOffline`（含“关闭先于登记”“旧断线不影响新连接”）、新增单测 `TestRoomPresenceKeepsTheNewestEpochWhateverOrderCommitsArrive` 全部 PASS。见 [green-after-fix.txt](green-after-fix.txt)。`roost module verify room`：33 passed（want 33）、0 skipped，build / vet / id check / generate --check 通过（[verify-after.txt](verify-after.txt)）。

## 变异（[mutations.txt](mutations.txt)）

在修后的生成工程里撤掉关键一行再跑同一组用例：

| 变异 | 结果 |
| --- | --- |
| M1：`Track` 去掉代际比较 | `TestRoomPresenceKeepsTheNewestEpochWhateverOrderCommitsArrive` FAIL（0 tracked; want only the new connection） |
| M2：`Forget` 不看现有代际（照撤新代际） | 同上 FAIL |
| M3：Join handler 去掉 `TrackCommitted` | 取消等待、Leave 迟到两条与原有 `TestRoomClosedConnectionMarksTheSeatOffline` FAIL |

M1 / M2 下三条控制器回归仍绿：改到提交后回调之后，正常调度下回调本来就按提交顺序到达，控制器回包迟到已不影响登记；代际单调防的是提交后回调本身的乱序（解锁之后才跑），由单测直接构造乱序覆盖。

## 范围与没做的

- 离线：没有真实 TCP / Gate、真实存储与进程重启；提交后回调乱序只在单测里构造，没有在真实 Nest 上制造。
- 墓碑保留 1 分钟是按“提交后回调之间的乱序远短于它”定的，未做压力下的实测分布。
