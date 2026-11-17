# `./client/relay` 那次没复现的 FAIL：复现与根因（交接 A1，2026-11-17）

对应 [KNOWN-LIMITS 二“客户端：遗留”](../../KNOWN-LIMITS.md#客户端遗留) 里“`./client/relay` 出现过一次 FAIL，尚未确认”一条，以及 [sdk 组证据](../lobby-relay-sdk-20261117/README.md)。

只读排查，没有改代码。带日志的旧版代码和确定性用例都放在仓库之外的副本里跑（`git archive` 导出）；确定性用例的源码附在本文末尾。源码行号按 main 的 `742ccafc`（到 `7db0c1ca` 为止，这里引用的文件没有变化）。

## 结论

1. **复现了一个真实的不确定失败**，在 sdk 组的提交 `19ce7b31` 上（出事 7 分钟后提交的版本，是能拿到的离出事时最近的源码；出事时的工作区是未提交的中间状态）：`TestRoomLifecycleAgainstRealRelayAndLobby` 报 `client_test.go:279: join a started room after leaving: <nil>`。完整套件单跑 80 次失败 1 次（第 15 次）；加了调试日志只跑这一个用例，200 次失败 1 次（第 82 次）。
2. **复现出的这个失败，根因在 `service/lobby`，不在 SDK，也不是别的组的半截改动**（它是在一份干净导出的提交上复现的）：成员经 lobby 离开之后，lobby 才处理到 relay 在他离开之前发出的一条在线名单，把他又加回了房间；他再 join 时走“已经在房间里”的那条路，不查“已开始”。
3. **lobby 的这个行为在当前代码（`742ccafc`）上仍然存在**：在 HEAD 的副本上按这个顺序写的确定性用例，内存与 Redis 两种房间目录都变红（用例按固定顺序直接调用 lobby，不依赖时序；保存的记录是跑一遍的结果）。经现在的 SDK 端到端没有复现过（两轮 `-count=50` 全过）；现在的 SDK 和用例与旧版有三处差别，都会缩小这个窗口，哪一处起决定作用没有做实验（第四节）。
4. 它是不是当初那一次，**不能确定**：当初的输出只剩三行，失败的用例名和原因行都没有了。能对上的只有：同一个包、相近的代码、都是不改源码重跑就通过。

修复没有做，要等推送之后；修法和要维护者定的点见后文。

## 一、当初那一次的原始记录

从 sdk 组的会话记录里找到了那条命令（之前的说法是“原因被 `tail` 截掉”，实际连包名那一行也截掉了）：

- 时间：2026-11-17 15:57:10～15:57:14（本地时间），耗时 3.38 秒。
- 命令：往 `client/relay/golden_test.go` 追加一个用例之后，`go test -race -count=1 ./client/relay/... 2>&1 | tail -3`。
- 输出只有三行：两行 `[no test files]`（当时对端还在 `internal/relaypeer` 里）和一行 `FAIL`。
- 6 秒后不带 `/...` 重跑通过；随后 `-count=8`、单跑 6 次、`-count=20` 都通过。

两点和交接文档里的说法不同：

- **“当时 relay 组正在改 `service/relay`”在出事的那一刻没有记录支持。** 同一个会话的主记录和 125 份子任务记录里，与那 3.38 秒有重叠的工具调用只有 sdk 组自己这一条；从它开始到重跑通过结束（15:57:10.8～15:57:20.6），也只有这两条命令。（只覆盖有记录的会话；记录之外有没有别的进程在动仓库，尚未确认。）
- sdk 组自己在失败前 9 秒（15:57:00～15:57:01）用脚本改过 `client/relay/client.go`，失败的那条命令本身又往 `golden_test.go` 追加了一个用例。失败那次和随后通过的那次之间没有改动，用的是同一份源码。
- 所以它是“同一份源码，一次失败、随后三十多遍通过”。

同一个用例更早还失败过一次，原因行是完整的，但和这次复现出来的不是同一个现象：15:42:02，`client_test.go:273: kicked member rejoin: lobby: room_started (HTTP 409)`，17 秒后重跑通过。那时的源码是未提交的中间状态，已经拿不到，**没有分析**。

## 二、复现

环境：macOS darwin/arm64，10 核，Go 1.27.3；`source ~/.roost-it/roost-dataengine-it/env.sh`，房间目录用隔离环境的 Redis。

| 轮次 | 代码 | 命令 | 机器负载（1 分钟） | 结果 |
| --- | --- | --- | --- | --- |
| 1 | `9dcd5a2f` | `GOWORK=off go test -race -count=50 -timeout 60m ./client/relay/` | 没有记录到（未额外加压） | 18 个用例 × 50 遍全过，437s |
| 2 | `742ccafc` | 同上，另起 10 个占满 CPU 的进程（`yes > /dev/null`） | 12～13 | 18 个用例 × 50 遍全过，438s |
| 3 | `19ce7b31`（6 个用例） | `GOWORK=off go test -race -count=1 -v ./client/relay/` 循环单跑，每次新进程 | 结束时 5.1，过程中没有记录 | 80 次里失败 1 次（第 15 次） |
| 4 | `19ce7b31` 加 lobby 调试日志 | 只跑 `TestRoomLifecycleAgainstRealRelayAndLobby`，循环单跑 | 没有记录 | 200 次里失败 1 次（第 82 次） |

不算数的两轮（写出来是为了不让人误读）：一轮加压跑了约 3 分钟被中止，没有结果；一轮跑了 5 秒收到 SIGTERM（`exit=143`，来源没有查），不是用例失败。

第 2 轮说明：这个强度的 CPU 压力对这些用例基本没有影响，耗时和不加压时一样，因为用例大部分时间在等待。第 1、2 轮跑的是现在的代码，现在的用例不会触发这个问题（原因见第四节），所以这两轮全过并不说明问题不存在。

## 三、根因：失败那次的实际顺序

第 4 轮失败那次的 lobby 日志（`EVENT` 是 lobby 处理 relay 的报告。同一条出现两次，是 Redis 目录写入冲突后把更新函数重新执行了一遍：`infra/storage/versionstore/redis_store.go:364` 的重试循环）：

```
EVENT room.member_left seq=4 account="member-2" online=[host-1 member-1]   members [host-1 member-1 member-2] -> [host-1 member-1]    （这一次没提交成功）
LEAVE member-1: removed from members, relaySeq=3
EVENT room.member_left seq=4 account="member-2" online=[host-1 member-1]   members [host-1 member-2] -> [host-1 member-2 member-1]    （重试：member-1 被加回来）
JOIN  member-1: already a member (rejoin path) state=started relaySeq=4
EVENT room.member_left seq=5 account="member-1" online=[host-1]            members [host-1 member-2 member-1] -> [host-1 member-2]
client_test.go:279: join a started room after leaving: <nil>
```

1. relay 因为踢掉 member-2 发出一条报告（seq=4），里面的在线名单是当时的快照，还有 member-1。lobby 第一次处理它时写入冲突，没有提交。
2. member-1 经 lobby 离开：lobby 把他从名单里删掉并提交，这时 lobby 记的 relay 序号还是 3。
3. lobby 重试 seq=4：序号比 3 大，所以应用；`applyOnline` 按“relay 报告在线、目录里却没有的人补上”把 member-1 加了回去。
4. 房主开局。member-1 再 join：已经在名单里，走幂等的那条路，不查“已开始”，拿到了令牌。
5. relay 处理完 leave 之后的报告（seq=5）才把他移走。

通过的那次，seq=4 在离开之前就提交了。

涉及的三段逻辑（行号按 `742ccafc`，旧版里是同样的逻辑）：

- 离开：成员直接从名单里删掉，然后通知 relay（`service/lobby/rooms.go`，`LeaveRoom`）。
- relay 报告：序号更大就应用；`applyOnline` 把报告里在线、目录里没有的人补上，只排除被踢的（`service/lobby/events.go:147-173`，第 168 行的条件是 `!r.kicked(account)`）。
- 加入：已经在名单里的人不查密码、人数和“已开始”（`service/lobby/rooms.go`，`JoinRoom` 里 `memberIndex(account) >= 0` 的分支）。

被踢的人已经有保护（踢出“与序号无关，迟到也照记”，`events.go` 里处理 `ReasonKicked` 的那一段），主动离开的没有。

## 四、当前代码仍受影响

### 确定性用例（HEAD `742ccafc` 的副本上，不靠运气）

在 `service/lobby` 现有的测试 harness 上写的用例，顺序是：建房 → 房主连上（seq 1）→ bob 加入并连上（seq 2）→ bob 经 lobby 离开 → 一条 seq 3 的心跳，在线名单里还有 bob → 房主开局 → bob 再 join。

```
GOWORK=off ROOST_REDIS_TEST_ADDR=127.0.0.1:17379 go test -race -count=1 -v -run TestA1StaleOnlineReport ./service/lobby/
```

内存与 Redis 两种房间目录结果相同：

```
seats right after bob's leave:            "acc-host:online"
seats after the in-flight report (seq 3):  "acc-host:online acc-bob:online"
bob joins the started room:                err = <nil>
seats after relay's member_left (seq 4):   "acc-host:online acc-bob:offline"
the in-flight report undid the leave: seats = "acc-host:online acc-bob:online", want only the host
bob after leaving, room started: err = <nil>, want ErrRoomStarted
```

第二种情形（房间已经开始，连上过的成员主动离开）同样变红：离开之后那条在途的报告把他加回来，他再 join 成功；现有用例“bob after leaving”（`service/lobby/lobby_test.go:853-858`）写的期望是“席位让出，之后再 join 是新人”。这种情形下，relay 后面的 `member_left{left}` 报告加上预留过期之后，席位会被清掉。

同一份副本里 `service/lobby` 原有的用例全部通过（`ok 5.811s`），说明副本本身没有问题。

### 为什么现在的 SDK 用例不红

现在的 SDK 和用例与 `19ce7b31` 相比有三处差别，每一处都会缩小这个窗口：

1. 用例在踢人之后、离开之前多了一步 `waitFor(t, "lobby records the kick", …)`（`client/relay/client_test.go:291`），等 lobby 处理完踢人的那条报告才继续。失败那次把 member-1 加回来的，正是踢人的这条报告。
2. SDK 的 `Leave` 现在先在 relay 连接上发 `RelayOpLeave`，再经 lobby 离开（`client/relay/client.go:403-416`）；旧版只经 lobby 离开。relay 更早知道他走了，之后的报告里就没有他。
3. 用例在离开之后、开局之前多了一步“拿旧令牌连 relay，期望被 lobby 拒绝”（`client/relay/client_test.go:311-314`），多了一次往返的时间。

**这三处是读代码看到的差别，没有逐个去掉做实验**，所以不能说是哪一处让它不再出现，也不能说现在的端到端用例一定不会再出现。能确定的只有 lobby 这一层的行为（上面的确定性用例）。

不经 SDK、直接调 lobby 的 leave 接口的客户端不受第 2 条保护。

### 实际影响（按证据能说到的程度）

- 房间目录在几毫秒到一条报告的间隔内，把已经离开的人显示为在线成员（占一个席位、计入人数）。
- 这段时间内他再 join，不查密码、人数和“已开始”。要触发需要离开之后立刻 join。
- 窗口多大取决于 relay 报告到 lobby 的延迟。测试对端是进程内投递，生产是 NATS，窗口只会更大；**生产环境里的实际概率没有测过**。
- 没有看到数据损坏或崩溃；后续的报告会把名单纠正回来。

## 修法（等推送之后做，方向要维护者定）

问题的本质：lobby 的“离开”是立刻生效的，而 relay 的在线名单是带延迟的快照，lobby 又把它当成“谁连着”的权威。被踢已经用一份名单解决了同样的冲突，离开没有。

| | 做法 | 代价 |
| --- | --- | --- |
| A（推荐） | 离开时记一笔“正在离开”（类似 `Kicked`）；`applyOnline` 不把这份名单里的人补回来；relay 报告他的 `member_left`、或他重新 join 成功时清掉 | `Room` 多一个字段；要定“relay 一直不报告”时怎么办（建议：再收到把他列为在线的报告时，重发一次 leave 命令，方向上是自愈的） |
| B | 离开时不立刻删，只打标记，等 relay 的 `member_left` 再删；加入、列房、人数都把带标记的人当成不在 | 和 A 等价，但改动点更多（所有读名单的地方都要认这个标记） |
| C | 不改服务端，只在文档里写明“离开后短时间内目录可能把人加回来” | 不解决 join 绕过检查的问题 |

先红后绿：把上面的确定性用例放进 `service/lobby/lobby_test.go`（两种情形、两种目录），先确认变红，再改。变异：去掉 `applyOnline` 里新加的条件，用例变红。SDK 一侧不用改。

## 要维护者定的点

1. 是否按缺陷处理，修法取 A 还是 B。
2. “正在离开”的记录在 relay 一直不报告时保留多久、要不要重发 leave 命令。
3. KNOWN-LIMITS 里这一条挪到哪一节（它现在记在“客户端：遗留”，实际是 lobby 的问题）。

## 尚未确认

- 它是不是当初那一次（原因行已丢，见结论第 4 条）。
- 现在的端到端用例为什么不再出现：三处差别没有逐个做实验。
- 15:42:02 那次更早的失败（`kicked member rejoin: room_started`）没有分析，源码拿不到。
- lobby 发给 relay 的 leave 命令如果丢了会怎样，没有查。
- 生产环境（NATS 投递、多个 lobby 实例）下的实际窗口和概率。
- 当初那 3.38 秒里有没有记录之外的进程在动仓库。

## 建议写进 CHANGELOG / KNOWN-LIMITS 的文字（由协调者统一写）

现在（修复之前）：

- KNOWN-LIMITS 二“客户端：遗留”，删掉“`./client/relay` 出现过一次 FAIL，尚未确认”一条。
- KNOWN-LIMITS 二“大厅 lobby 与转发 relay：遗留”，加一条：
  - **离开房间可能被在途的 relay 报告撤销（已复现，未修）**：成员经 lobby 离开之后，lobby 若再处理到 relay 在他离开之前发出的在线名单（序号更大、名单里还有他），会把他加回房间目录；在 relay 的下一条报告纠正之前，他再 join 不查密码、人数和“已开始”。被踢的人不受影响（有被踢名单）。2026-11-17 sdk 组开发中 `./client/relay` 的那次 FAIL 可能就是它（原因行已丢，不能确定）：在 sdk 组随后提交的版本上单跑 80 次复现 1 次，当前代码在 lobby 一层用确定性用例稳定复现；经现在的 SDK 端到端两轮 `-count=50` 没有出现（证据链接写 `evidence/client-relay-flake-20261117/README.md`）。
- CHANGELOG：这次没有行为变化，不用写。

修复之后：

- CHANGELOG `[Unreleased]`：**lobby：离开房间不再被在途的 relay 报告撤销**（写明修法与新字段）。
- KNOWN-LIMITS：删掉上面那一条。

## 附：确定性用例的源码

放进 `service/lobby/` 即可运行（依赖该包现有的测试 harness）。修复时把它并入 `lobby_test.go`，先确认变红。

```go
package lobby

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A1 的确定性复现（只放在私有副本里，不在仓库中）。
//
// 顺序：成员经 lobby 离开之后，才处理到 relay 在他离开之前发出的一条在线名单（序号更大、名单里还有他）。
// 期望（LeaveRoom 的文档与现有用例“bob after leaving”）：离开的人席位让出，之后再 join 是新人；房间已开始时
// 新人被拒（ErrRoomStarted）。
func TestA1StaleOnlineReportAfterLeaveBringsTheLeaverBack(t *testing.T) {
	// 情形一：房间还没开始时离开，随后房主开局（client/relay 的 TestRoomLifecycle 里出问题的顺序）。
	t.Run("leave_then_start", func(t *testing.T) {
		backends(t, time.Hour, func(t *testing.T, h *harness) {
			ctx := context.Background()
			id := h.create(t, "gt-host", CreateRequest{}).Room.ID
			relay := h.room(t, id).Relay.Instance
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 1, Host: "acc-host", Online: []string{"acc-host"}})
			if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
				t.Fatal(err)
			}
			h.report(t, RoomEvent{Type: EventMemberJoined, Room: id, Relay: relay, Seq: 2, Host: "acc-host", Account: "acc-bob",
				Online: []string{"acc-host", "acc-bob"}})

			if _, err := h.svc.LeaveRoom(ctx, "gt-bob", id, LeaveRequest{Game: "party"}); err != nil {
				t.Fatal(err)
			}
			t.Logf("seats right after bob's leave:            %q", seats(h.room(t, id)))

			// relay 在处理 leave 命令之前发出的报告（例如一条心跳），在 lobby 这边排在 LeaveRoom 之后。
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 3, Host: "acc-host", Online: []string{"acc-host", "acc-bob"}})
			afterStale := seats(h.room(t, id))
			t.Logf("seats after the in-flight report (seq 3):  %q", afterStale)

			if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateStarted}); err != nil {
				t.Fatal(err)
			}
			_, err := h.join("gt-bob", id, JoinRequest{})
			t.Logf("bob joins the started room:                err = %v", err)

			// relay 处理完 leave 之后的报告。
			h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: relay, Seq: 4, Host: "acc-host", Account: "acc-bob", Reason: ReasonLeft,
				Online: []string{"acc-host"}})
			t.Logf("seats after relay's member_left (seq 4):   %q", seats(h.room(t, id)))

			if afterStale != "acc-host:online" {
				t.Errorf("the in-flight report undid the leave: seats = %q, want only the host", afterStale)
			}
			if !errors.Is(err, ErrRoomStarted) {
				t.Errorf("bob after leaving, room started: err = %v, want ErrRoomStarted", err)
			}
		})
	})

	// 情形二：房间已经开始，连上过的成员主动离开。
	t.Run("leave_a_started_room", func(t *testing.T) {
		backends(t, time.Hour, func(t *testing.T, h *harness) {
			ctx := context.Background()
			id := h.create(t, "gt-host", CreateRequest{}).Room.ID
			relay := h.room(t, id).Relay.Instance
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 1, Host: "acc-host", Online: []string{"acc-host"}})
			if _, err := h.join("gt-bob", id, JoinRequest{}); err != nil {
				t.Fatal(err)
			}
			h.report(t, RoomEvent{Type: EventMemberJoined, Room: id, Relay: relay, Seq: 2, Host: "acc-host", Account: "acc-bob",
				Online: []string{"acc-host", "acc-bob"}})
			if _, err := h.svc.SetRoomState(ctx, "gt-host", id, StateRequest{Game: "party", State: StateStarted}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.LeaveRoom(ctx, "gt-bob", id, LeaveRequest{Game: "party"}); err != nil {
				t.Fatal(err)
			}
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 3, Host: "acc-host", Online: []string{"acc-host", "acc-bob"}})
			afterStale := seats(h.room(t, id))
			t.Logf("seats after the in-flight report (seq 3):  %q", afterStale)
			_, err := h.join("gt-bob", id, JoinRequest{})
			t.Logf("bob joins again:                           err = %v", err)
			h.report(t, RoomEvent{Type: EventMemberLeft, Room: id, Relay: relay, Seq: 4, Host: "acc-host", Account: "acc-bob", Reason: ReasonLeft,
				Online: []string{"acc-host"}})
			h.lapse()
			h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: relay, Seq: 5, Host: "acc-host", Online: []string{"acc-host"}})
			t.Logf("seats after member_left, lapse, heartbeat: %q", seats(h.room(t, id)))

			if afterStale != "acc-host:online" {
				t.Errorf("the in-flight report undid the leave: seats = %q, want only the host", afterStale)
			}
			if !errors.Is(err, ErrRoomStarted) {
				t.Errorf("bob after leaving a started room: err = %v, want ErrRoomStarted", err)
			}
		})
	})
}
```
