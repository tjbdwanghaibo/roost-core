# v1.54.0 审查三项修复（RV-20261118-02 / 03 / 04）

审查原文与复跑证据在分支 `origin/fix/core-game-server-review` 的 `docs/maintenance/V154-REVIEW-20261118.md`、
`docs/maintenance/evidence/v154-review-20261118/`（本轮只读，未合并、未 cherry-pick）。修复基于 main `7c9cf93a`，
行号按该提交核对（main 在 v1.54.0 之后改过 SDK：离开返回 RoomGone、C# PumpDriven、Go deflate 复用、wsconn 期限）。
本机 macOS（Darwin 25.5，arm64），Go 1.27，node v26.7.0，dotnet 10.0.302；Linux 用本机已有的 Colima VM
（Ubuntu 6.8.0-117，aarch64，4 核，未下载任何镜像，用完已停回 Stopped），在 macOS 上交叉编 `linux/arm64` 测试二进制进 VM 跑。

| 编号 | 结论 | 提交 |
|---|---|---|
| RV-20261118-03 | 已修复、已验证（内存与 Redis 目录） | `b7bbe9f9` |
| RV-20261118-02 | 已修复、已验证（TS / Go / C# 三份 SDK 一致） | `fe9385bc` |
| RV-20261118-04 | 测试缺陷已修（产品代码未改），macOS 与 Linux（Colima）实跑；gateway 同类用例一并修 | `49faa89b` |

## RV-20261118-03 · 已开局房间报告乱序，主动离开的玩家仍可回座

**根因**：`service/lobby/events.go:126`（7c9cf93a）——`member_left` 原因 `left` 只在 `event.Seq > room.RelaySeq` 时处理
（第 130 行清 `Connected`）。relay 的报告允许并发发出（`service/relay/report.go` 的 `Reporter`：同一房间的报告可能从不同
goroutine 并发调用），只剩房主在线的心跳 seq 3 先到时全房水位越过 left 的 seq 2，这条 left 被当成迟到报告丢掉；
已开局房间里 `Connected` 仍为 true，`holdsSeat` 保留离线席位，JoinRoom 把他当老成员放回。

**修法**：每个席位记 `presence_seq`（`Member.PresenceSeq`，`Connected` 依据的那条报告的序号）。新增 `Room.notePresence`：
不论报告比水位新还是旧，“报告里在线”记 `Connected=true`（`leaving` 名单里的人除外），“member_left 原因 left”记
`Connected=false`，都只认比该成员已记下的 `presence_seq` 更新的；left 之后已不占席位（`holdsSeat`）就立即让出。
`applyOnline` 只管最新名单的在线标记、预留与补人，补进来的人 `presence_seq` 记为那条报告的序号。
与 `leaving`、`alive_until_unix_ms`、“relay 重启即丢房”的语义无交集：迟到报告照旧续命，`Leaving` 的清除仍只认序号更大的
member_left（迟到的旧 left 可能早于 lobby 的 leave，不能用来清）。Redis 记录多一个 JSON 字段，旧记录缺省为 0（线上未部署，
不涉及兼容）。

**红**（`lobby_test.go` 新增 `TestAnExplicitLeaveReleasesAStartedSeatInAnyArrivalOrder`，修前，内存与 Redis 两种目录）：

```
--- FAIL: TestAnExplicitLeaveReleasesAStartedSeatInAnyArrivalOrder/heartbeat_first/memory
    lobby_test.go:967: seats after alice left through the relay = "acc-host:online acc-alice:offline", want only the host
--- FAIL: TestAnExplicitLeaveReleasesAStartedSeatInAnyArrivalOrder/heartbeat_first/redis
    lobby_test.go:967: seats after alice left through the relay = "acc-host:online acc-alice:offline", want only the host
--- PASS: .../left_first/memory, .../left_first/redis
```

修后断言还包括：再 join 得 `ErrRoomStarted`，relay 验权得 `not_member`。另加守卫
`TestALateOnlineReportStillRecordsAReconnectAfterALeave`（修前本来就绿）：left（seq 2）先到、回座重连的 member_joined（seq 4）
迟到，她仍是“连上过”的人，预留过期后掉线照样留座——防止按成员序号收敛后反向丢掉重连事实。原有 carol 段
（`TestStartedRoomsReleaseSeatsOnKickLeaveAndClose`：旧 left 晚于同一成员真实重连）保留不动。

**变异**（`scratchpad` 脚本逐处撤销，恢复后 `cmp` 一致）：

| 变异 | 结果 |
|---|---|
| notePresence 只在 `event.Seq > room.RelaySeq` 时调用（相当于原逻辑） | 新用例 heartbeat_first 内存 / Redis 都红 |
| 迟到报告里的在线事实不记（加 `event.Seq <= r.RelaySeq` 跳过） | `TestALateOnline…` 内存 / Redis 都红（`seats … = "acc-host:online", want her seat kept`） |
| left 不比较成员自己的 `presence_seq` | 原有 carol 段内存 / Redis 都红（`seats after a late left and a drop = "acc-host:online", want carol's kept`） |

**文档**：`docs/framework/LOBBY.md` §“lobby 的处理”改成按成员事实序号收敛的现行规则。

## RV-20261118-02 · 房主的旧快照覆盖新任期状态（TS / Go / C#）

**根因**：

- TS `client/ts/src/client.ts:681`（7c9cf93a）：`pushSnapshot` 取状态（`snapshot.take()`）与压缩（`packState`）都是异步，
  结束后只检查“有连接且是房主”，第 684 行按当前计数分配 seq。A→B→A（epoch 0→2）后旧状态拿到 epoch 2 的 seq，
  relay `service/relay/room.go:435` `storeSnapshot` 只按 seq 递增判断新旧，存下它。
- Go 同类：`client/relay/host.go:352` 压缩在锁外，第 366 行在锁内只查 `role.Host` 就分配 seq；定时快照
  `host.go:421` 在心跳 goroutine 上先调 `SnapshotState()`（游戏回调，可能慢）再 `Snapshot`，任期在要完状态之后才看。
  发送用 `SendControl` 取的是此刻的连接。
- C# 同类：`client/dotnet/Roost.Client.Relay/RelayClient.cs:582` 压缩在锁外（`Snapshot` 可从任意线程调，网络线程照常改
  角色），第 589 行只查 `role.IsHost` 就分配 seq；`AutoSnapshot`（第 943 行）在 Pump 里先 `SnapshotState` 后取任期。
  C# 只有游戏从别的线程调 `Snapshot` 时窗口才打开（Pump 线程上的 AutoHostReady 要等 Pump），但这是文档允许的用法。

**修法（三份一致）**：准备（取状态、压缩）之前记下当前连接与 `epoch`（Go `hostTerm`、C# `HostTerm`、TS 局部变量），
分配 seq 之前在同一把锁里核对；变了就丢弃：不发、不占 seq、不更新最小间隔计时（Go 新增 `ErrSnapshotStale`，C# 新增
`RelaySnapshotResult.Stale`，TS 返回 `undefined` 且不触发 `onSnapshotSent`）。定时快照在向游戏要状态之前记任期。
Go / C# 在记下的连接上写出（不再取“此刻的连接”）。核对通过、seq 已分配后才换任期的那一份，由 relay 按 seq 递增拒收
（新任期 seq 不低于 `epoch<<32`）。不是房主时的旧行为不变（Go `ErrNotRoomHost`、C# 抛 `InvalidOperationException`）；
没有连接时 Go 返回 `ErrNotConnected`、C# 返回 `NotConnected`，都不再先占一个 seq。

**红**（修前；TS 用 `scripts/test-client-ts.sh` 同一套编译产物，对端是 `go test -c ./client/relay` 的真实 Go lobby + relay）：

```
TS 真实对端 relay.test.ts「a snapshot started in an earlier host term is dropped: the next host recovers the newest state」
  AssertionError [ERR_ASSERTION]: the epoch-0 snapshot is dropped
  + 8589934594n
  - undefined
TS 假 relay host.test.ts：take / compress 两处、任期变了的两条：
  AssertionError [ERR_ASSERTION]: the stale snapshot is not sent   + 8589934593n  - undefined
  （任期不变的两条对照照常发出一帧：绿）
TS 假 relay「a snapshot taken on one connection is not sent on the next one」：
  AssertionError [ERR_ASSERTION]: the snapshot from the old connection is dropped   1n !== undefined
Go host_test.go TestASnapshotPreparedInAnEarlierHostTermIsDropped（真实对端；修前先只加 ErrSnapshotStale 哨兵、不改行为）：
  /Snapshot:      host_test.go:347: the epoch-0 snapshot returned <nil>, want ErrSnapshotStale
                  host_test.go:359: B recovered seq 8589934594 state "epoch-zero-state" (source 2), want seq 8589934593 epoch-two-state
  /SnapshotState: host_test.go:359: B recovered seq 8589934594 state "epoch-zero-state" (source 2), want seq 8589934593 epoch-two-state
C# Roost.Client.Relay.Tests「a snapshot prepared in an earlier host term or connection is dropped」（修前先只加 Stale 枚举值）：
  FAILED: System.Exception: Check failed: the epoch-0 snapshot is dropped, got Sent
```

红与审查实测一致（B 收到 seq `8589934594` 的 `epoch-zero-state`）。回归同时断言：过期一份没占 seq（TS `snapshotSeq`
仍是 `2<<32`、下一份是 `(2<<32)+1`；Go / C# 下一任恢复的 seq 是 `(2<<32)+1`），没发送也没被 relay 保存（`onSnapshotSent`
没触发、A 的 Rejected 为空），下一任房主恢复的实际内容是 `epoch-two-state`。Go 的 SnapshotState 路径把卡点放在游戏回调里
（不是压缩里），才真正覆盖“定时快照先要状态”的窗口。

**变异**：

| 变异 | 结果 |
|---|---|
| TS 去掉 epoch 核对 | take/compress × 换任期两条红，真实对端用例红；连接用例绿 |
| TS 去掉连接核对 | 连接用例红，其余绿 |
| Go 去掉 epoch 核对 | 两条路径都红（同上红文本） |
| Go 去掉连接核对 | 连接用例红（`returned relay: write: … use of closed network connection, want ErrSnapshotStale`） |
| Go 定时快照改回“要完状态再取任期”（`_ = c.Snapshot(state)`） | SnapshotState 路径红（B recovered epoch-zero-state） |
| C# 去掉 epoch 核对 | `the epoch-0 snapshot is dropped, got Sent` |
| C# 去掉连接核对 | `the snapshot from the old connection is dropped, got NotConnected` |

**文档**：`docs/framework/RELAY.md` §4A“快照”加 SDK 绑定任期的契约；`client/relay/README.md`、`client/ts/README.md`、
`client/dotnet/Roost.Client.Relay/README.md` 同步。

## RV-20261118-04 · Linux 上 wsconn 写期限回归被对端读超时干扰

**根因（测试）**：`infra/network/wsconn/wsconn_test.go:157`（7c9cf93a）用 `pair(t, 4<<10)` 把客户端 SO_RCVBUF 设成 4 KiB 让写
卡住，第 175 行让客户端 5 秒内读完 4 MiB。Linux 在建连时按设的大小钳住接收窗口，小于一个 MSS（回环 MSS 约 64 KiB），按
糊涂窗口规避一小段一小段地放，每秒只有几十 KB；5 秒读超时，coder/websocket 关闭连接，服务端写报 broken pipe，用例把它
归到旧期限上。生产旧写期限故障没有复现，产品代码不改。

**Linux 复现（Colima，修前用例 + 修复版产品代码）**：

```
wsconn_test.go:187: a write under a fresh one-minute deadline failed: failed to write msg: failed to write frame:
  failed to write frame payload: write tcp 127.0.0.1:34981->127.0.0.1:52692: write: broken pipe (the earlier deadline's timer closed the connection)
--- FAIL: TestAStaleWriteDeadlineDoesNotCloseALaterWrite (5.44s)     （3/3）
```

**排除的修法**：先试“观察到写卡住后再把 SO_RCVBUF 调大”——Linux 上无效（窗口钳位在建连时定下，自动调节因 SOCK_RCVBUF_LOCK
关闭），30 秒只读到约 122 万字节，3 轮全红（`client read 1224438 bytes, failed to read: context deadline exceeded`），已撤回。

**修法**：客户端用缺省接收缓冲，开始读之前不读任何东西，靠 32 MiB 的消息塞满两端 socket 缓冲；“写已卡住”作为前提显式
检查：客户端 goroutine 轮询写方向的 `busy`（测试文件里给 `deadline` 加的只读方法 `inProgress`，产品代码未动），GOMAXPROCS(1)
下它能跑就说明测试 goroutine 卡在写里；写不经阻塞就写完，用例以 “precondition …” 失败而不是假绿。观察到后先让出一次处理器
（让排在前面的旧期限回调跑），再在 30 秒预算内读完；读完再写一条 `after` 确认连接仍可用；失败时打印写错误与对端读结果。
保留“旧期限到期、刷新期限后的写成功、连接可用”的断言与 GOMAXPROCS(1) 调度固定。gateway 的同类用例
`TestAnEarlierFramesDeadlineDoesNotBreakTheNextWebSocketWrite`（4 KiB + 1 MiB + 5 秒）在 Linux 上同样 3/3 红
（`client read a 257270-byte message, failed to read: context deadline exceeded`），按同样办法改（会话外包一层记“正在写”，
`stalledWebSocketSession` 加 readBuffer 参数，其余两条只塞满不读的用例仍用 4 KiB）。

**变异（撤掉产品修复）**：M1 去掉 `deadline.do` 读写之后的 `d.apply(time.Time{})`；M2 整个撤回（`set` 立即交给库，读写时不设不清）。

| 用例 / 平台 | 修复版 | M1 | M2 |
|---|---|---|---|
| wsconn，macOS 不带 -race | 20/20 绿 | 20/20 红 | 20/20 红 |
| wsconn，macOS -race | 20/20 绿 | 5/20 红 | 5/20 红 |
| wsconn，Linux（Colima） | 20/20 绿；整包 ×3 绿 | 69/70 红（20 轮 19 红 + 50 轮 50 红） | 未跑 |
| gateway，macOS 不带 -race | 20/20 绿（-race ×10 绿） | 20/20 红 | 未跑 |
| gateway，Linux（Colima） | 20/20 绿；WebSocket 用例 ×2 绿 | 20/20 红 | 未跑 |

红文本：`a write under a fresh one-minute deadline failed: failed to write msg: failed to write frame: context canceled …;
peer read: client read 327202 bytes, failed to read: failed to read frame payload: unexpected EOF; want 33554432`。
对照：修前的原用例在 macOS M1 下也是不带 -race 20/20、-race 5/20 红——`-race` 下运行时随机化 runnext，GOMAXPROCS(1) 的
调度次序不再固定，这是原方法的固有限制，不是本次引入；变红的证明看不带 -race 的轮次。Linux 上 gateway M1 的失败信息里
peer read 一项有时是 “precondition …”（写被回调打断后客户端还没观察到写开始），失败落在写错误本身上。

**文档**：`docs/framework/GATEWAY.md` 回归段补“前提显式检查、缺省缓冲、不能用 4 KiB、-race 限制”。

## 验证（修复后，全部 GOWORK=off）

- `go vet` 与 `go test -race -count=1 ./service/lobby/ ./service/relay/... ./wiring/lobby/ ./wiring/relay/ ./client/relay/ ./infra/network/wsconn/ ./infra/network/gateway/`：
  全部 ok（`source ~/.roost-it/roost-dataengine-it/env.sh`，`ROOST_REDIS_TEST_ADDR` 取私有环境 Redis，lobby 的 redis 子用例实际运行、0 SKIP）。
- `go test -race -count=1 -tags integration -v ./wiring/relay/`：ok，`TestRealRelay*` 三条实际 PASS（私有 NATS + 用例自起 etcd）。
- `go test -count=1 .`（根包）：ok。
- TS `scripts/test-client-ts.sh`（node_modules 软链主检出，跑完已删软链，未 npm install）：67 tests，67 pass，0 skip。
- C# `dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$PWD"`：all 846 checks passed（TCP 与 WebSocket）；
  之后 `dotnet build-server shutdown`、清 bin/obj。
- 未跑全量 `go test ./...` 与 pretag（按批次规则由协调者统一跑）。

## 尚未确认 / 限制

- RV-04 的 Linux 只在 Colima（arm64 Ubuntu 6.8 内核，4 核）上实跑，审查用的是 Linux amd64；amd64 实机尚未确认。Linux 上没有
  跑 -race（交叉编译 -race 需要 cgo）。
- RV-02 的同任期并发推送（不是本次问题，记下供判断）：同一任期内两次 `pushSnapshot` 并发、先发起的那次取状态更慢且慢过
  最小间隔时，它仍会拿更大的 seq 发出、覆盖后发起的新状态。三份 SDK 都如此。Go / C# 的定时推送在同一 goroutine / Pump 上串行，
  不会自己重叠；TS 的定时推送每次开始时重设 `setInterval`，不等上一份完成，`take` 慢于 `intervalMs` 时会重叠。是否也要按
  “发起顺序”丢弃落后的一份，要维护者定（见下）。
- 本机 Colima 启动时 docker 出现名为 `colima` 的 context（当前 context 仍是 default，未切换）；它是否原本就存在未确认。

## 要维护者定的点

1. RV-02 同任期并发推送：A（推荐）在 SDK 里按发起顺序编号，已发出更新的一份后，落后的那份同样丢弃（三份 SDK 一致，
   改动小，代价是游戏自己并发调用时较早那份返回 Stale / undefined）；B 维持现状，只在 README 写明“不要并发推快照，
   take 要快于 intervalMs”。
2. Go `ErrSnapshotStale` / C# `RelaySnapshotResult.Stale` 是新增公开 API（线上未部署，不做兼容）；生成物与 demo 未用到，
   不涉及最低 core 版本。

## 建议写进 CHANGELOG `[Unreleased]` 的文字

- **lobby：已开局房间里经 relay 主动离开的人，left 报告晚于后续心跳到达也不再留座**（RV-20261118-03）。relay 的报告可以
  并发发出、乱序到达；只剩房主在线的心跳先到时，原来的 lobby 把迟到的 `member_left{left}` 当成旧报告丢掉，主动离开的人被
  当成掉线、还能再 join 回原座。现在每个席位记 `presence_seq`，“连上过 / 主动离开”按成员自己的事实序号收敛，新旧报告都看；
  迟到的旧 left 仍不清掉回座又连上的人。见 [LOBBY](../../../framework/LOBBY.md)。
- **开房间 SDK（Go / TS / C#）：快照绑定准备时的连接与房主任期**（RV-20261118-02）。取状态或压缩期间房主换了一圈
  （A→B→A）或断线重连时，旧状态原来会拿新任期的 seq 发出，relay 当成最新一份存下，下一任房主恢复成旧状态。现在这一份
  丢弃、不发、不占 seq：Go 返回新增的 `ErrSnapshotStale`，C# 返回新增的 `RelaySnapshotResult.Stale`，TS `pushSnapshot`
  返回 `undefined`；定时快照在向游戏要状态之前记任期。见 [RELAY §4A](../../../framework/RELAY.md)。
- **测试：WebSocket 旧写期限回归在 Linux 上不再误报**（RV-20261118-04）。wsconn 与 gateway 的两条用例原来把客户端接收缓冲设成
  4 KiB 再要求 5 秒内读完，Linux 建连时把窗口钳在这个大小、读不完而误报；现在显式检查“写已卡住”，客户端用缺省缓冲读完。
  产品代码未改。

## 建议写进 KNOWN-LIMITS 的文字

- RV-20261118-02 / 03 / 04：改为“已修复（`fe9385bc` / `b7bbe9f9` / `49faa89b`），证据 evidence/v154-review-fixes-20261118”。
- 遗留（待维护者定，见上“要维护者定的点”1）：开房间 SDK 同一任期内并发推快照，较早发起、较晚取完状态的那份仍会以更大 seq
  覆盖较新的一份（TS 定时推送在 `take` 慢于 `intervalMs` 时也会重叠）。
- 限制（尚未确认）：RV-20261118-04 的 Linux 验证只在本机 Colima（arm64）上做过，amd64 实机未跑；`-race` 下 GOMAXPROCS(1)
  的调度固定不成立（runnext 随机化），旧写期限回归撤掉修复时只部分轮次变红。
