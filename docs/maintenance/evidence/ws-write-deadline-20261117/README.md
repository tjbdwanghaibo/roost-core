# 2026-11-17：WebSocket 连接被自己的旧写期限关掉（relay 与 Gate）

现象（C1 在 Unity 编辑器里观察到，C# SDK 走 WebSocket 连 relay）：13 次里有 5 次，成员在心跳的整 10s 点（20.5s / 40.5s）被 relay 断开。relay 读这条连接时报 `use of closed network connection`，写循环没有报错。

## 根因（已定位、已修复、已验证；未推送）

`infra/network/wsconn` 原来把 `SetReadDeadline` / `SetWriteDeadline` 原样交给 coder/websocket v1.8.15 的 `websocket.NetConn`。它的写期限实现（模块缓存 `github.com/coder/websocket@v1.8.15/netconn.go` 第 61～72、207～219 行）：

- `SetWriteDeadline` 用 `time.AfterFunc` 的计时器 `Reset(time.Until(t))`；期限已过时按 1ns 后到期。
- 到期回调在自己的 goroutine 里 `writeMu.tryLock()`：拿不到锁（有写在进行）就 `writeCancel()`，写 context 永久取消，`Conn.write` 的 `context.AfterFunc` 随之 `c.close()` 关掉整条连接；拿到锁就置 `writeExpired`，之后的写失败，直到再设期限。
- `Timer.Reset` 拦不住已经触发、但回调 goroutine 还没跑到 `tryLock` 的那一次。

relay 的 `writeLoop`（`service/relay/conn.go`）每批写前设 `now + write_timeout`（10s），写完不清；Gate 的 `tcpSession.writeFrame` 每帧写前设期限（`write_timeout` 或独立 Gate 的单帧发送预算），写完不清。成员每 10s 发一次 Ping，relay 回 Pong 的那次写正好落在上一批 Pong 的期限到期点上：计时器触发，回调还没跑，`writeLoop` 先 `SetWriteDeadline`（`Reset` 无效）再 `Write` 拿到锁，回调随后 `tryLock` 失败，把这次新的写当成“写在期限处未完成”，取消并关掉连接。这一次写本身可能已写完、返回 nil（所以写循环没报错），读循环随即看到 `use of closed network connection`。Go 客户端按 100ms 节奏发帧时，期限每 100ms 被推后一次，从不在两次写之间到期，所以没复现。

同一机制的第二个表现：期限已过时 `NetConn` 不立即拒绝，而是按 1ns 后到期，紧接着的写要么照样写出（回调晚于写完才跑），要么被回调关掉连接；原有用例 `TestAnAlreadyExpiredDeadlineRefusesWithoutClosing` 先睡 20ms 让计时器在没有写时到期，正好绕开了它。独立 Gate 的单帧发送预算快用完时会走到这里。

读期限：同一套计时器，同样的缺陷在机制上存在。relay 不用读期限（空闲改用计时器）；Gate 的读循环几乎一直有读在进行，读期限到期就是 `idle_timeout`，旧期限只在“一帧恰好在空闲期限点到达”时起作用，没有找到实际触发场景。修法对读写一视同仁。

## 红用例（修前，原代码）

在 `GOMAXPROCS(1)` 下固定调度次序：测试 goroutine 占住唯一的处理器跨过旧期限与一个唤醒计时器的两个到期点，再在通道上挂起；调度器同一轮里先触发旧期限（回调进 runnext），再触发唤醒（唤醒 goroutine 进 runnext，回调挤到队尾），唤醒把测试 goroutine 放回 runnext，于是测试 goroutine 先设新期限、开始一次塞满停读对端的 4 MiB 写并挂起，回调此时才跑。对照：同一用例在第一次写后加 `SetWriteDeadline(time.Time{})`（调用方自己清期限），原代码上 30/30 两轮全过，确认是旧期限的计时器关的连接。

- `infra/network/wsconn` `TestAStaleWriteDeadlineDoesNotCloseALaterWrite`：上一次写设 3ms 期限、写完不清，旧期限到期后设一分钟的新期限再写。
- `infra/network/wsconn` `TestAnExpiredWriteDeadlineRefusesTheVeryNextWrite`：设一个已过的期限后立即写（不睡），要求 `os.ErrDeadlineExceeded` 且不包裹 `net.ErrClosed`，之后清掉期限还能写、对端只收到之后那条。
- `infra/network/gateway` `TestAnEarlierFramesDeadlineDoesNotBreakTheNextWebSocketWrite`：同样的次序走 `tcpSession.WritePacket`，上一帧带 3ms 的调用方期限（单帧发送预算），下一帧用 `write_timeout`（一分钟）。

原代码上的失败文本（原样，第一轮的行号）：

```
--- FAIL: TestAStaleWriteDeadlineDoesNotCloseALaterWrite (0.01s)
    wsconn_test.go:185: a write under a fresh one-minute deadline failed: failed to write msg: failed to write frame: context canceled (the earlier deadline's timer closed the connection)
--- FAIL: TestAnExpiredWriteDeadlineRefusesTheVeryNextWrite (0.00s)
    wsconn_test.go:133: write right after setting an expired deadline: i/o timeout: the connection is closed: use of closed network connection: failed to write msg: failed to write frame: context canceled, want os.ErrDeadlineExceeded without net.ErrClosed
--- FAIL: TestAnEarlierFramesDeadlineDoesNotBreakTheNextWebSocketWrite (0.01s)
    tcp_transport_test.go:602: a frame written under write_timeout (a minute) failed: player tcp: connection cannot take the frame: failed to write msg: failed to write frame: context canceled (the previous frame's deadline closed the connection)
```

红的比例（原代码，同一台机器，机器有其他 agent 的负载）：

| 用例 | 不带 -race | 带 -race |
|---|---|---|
| wsconn stale | 5/5、20/20、50/50、49/50 | 5/20 |
| wsconn expired | 5/5、19/20（4 MiB 缓冲当时在设期限之后分配，分配可能让出处理器；挪到前面之后）50/50、50/50 | 20/20 |
| gateway 上一帧期限 | 20/20、49/50 | 6/20 |

-race 下 stale 两条只有约四分之一红：竞态检测改变了调度（推测是插桩带来的让出），不是确定的；不带 -race 时近乎确定。修复后的绿是结构性的（计时器在写完后停掉，旧期限没有计时器可触发），不依赖这个比例。

## 修法

只改共用的 `infra/network/wsconn`（Gate、嵌入 TCP、relay 都经它）：每个方向一个 `deadline`，期限只在一次读写进行期间交给 `NetConn`。

- `SetReadDeadline` / `SetWriteDeadline` 只记下期限；有读写在进行时同时交给 `NetConn`（照 net.Conn 约定，进行中的读写也受新期限约束，`Close` 之外打断阻塞读写的用法不变）。
- `Read` / `Write`：开始时期限已过就立即返回包裹 `os.ErrDeadlineExceeded` 的错误，什么也不做、连接保留；否则把期限交给 `NetConn`、读写、再 `SetXxxDeadline(time.Time{})` 停掉计时器。同一方向的读写一次一个（`NetConn` 本来也逐个执行，没有并发损失）。
- 读写进行中期限到期仍关闭整条连接（库的行为，不变），错误同时包裹 `os.ErrDeadlineExceeded` 与 `net.ErrClosed`，Gate 的 `writeFrame` 照旧当作 `errConnectionBroken`。
- 调用方（relay 的 `writeLoop`、Gate 的 `writeFrame`）不改：TCP 上“换期限即生效”本来成立，现在 WebSocket 上也成立。relay `writeLoop` 设期限处补了一句中文契约注释。

没选“relay / Gate 每批写完 `SetWriteDeadline(time.Time{})`”：要两处调用方各自记得，之后新的调用方还会再踩；而且解决不了“期限已过时紧接着的写被关掉”。

剩下的边界（写进了 `deadline` 的注释与 GATEWAY §6）：一次读写恰好在期限处结束、库的回调已触发而还没跑时，它仍可能关掉紧接着的那次读写，或让它以 `context deadline exceeded` 失败——那次期限确实到了，与“读写在期限处未完成”同样处理。

## 变异

| 变异 | 结果 |
|---|---|
| M1：去掉 `do` 里读写之后的 `d.apply(time.Time{})` | wsconn stale 20/20 红、gateway 上一帧期限 20/20 红；expired 仍绿（开始前的拒绝还在） |
| M2：去掉 `do` 开头“已过期就拒绝”的判断 | wsconn expired 20/20 红；原有 `TestAnAlreadyExpiredDeadlineRefusesWithoutClosing` 也 20/20 红（`write with an expired deadline: <nil>`）；stale 仍绿 |

恢复后与修复版逐字节一致（`cmp`）。

## 验证命令与结果（修复后）

- `GOWORK=off go test -race -count=1 ./infra/network/wsconn/ ./infra/network/gateway/ ./service/relay/... ./wiring/relay/ ./wiring/gate/ ./client/relay/`：全部 ok。
- `GOWORK=off go test -race -count=20` 新用例与原有期限用例（wsconn 五条、gateway 两条）：ok。不带 -race 的新用例 `-count=50` / `-count=20`：ok。
- `GOWORK=off go test -race -count=1 -tags integration -v ./wiring/relay/`（私有环境的 NATS、用例自起的 etcd）：全部 PASS，三条 `TestRealRelay*` 实际运行，无 SKIP。
- `GOWORK=off go test -race -count=1 -tags integration -v ./wiring/gate/`：22 条 PASS，`TestRealGateLockstepCatchupOverASlowLinkDrill` 按设计 SKIP（演练开关）。
- `scripts/test-client-ts.sh`（worktree 里软链主检出的 `client/ts/node_modules`，跑完删除软链，未 npm install）：60 pass、0 fail、0 skipped。
- `GOWORK=off go vet` 上述各包、`gofmt -l`：干净。
- 没跑：全量 `go test ./...`、pretag、根包（按批次规则由协调者统一跑；本次改了 `docs/framework/GATEWAY.md`、`RELAY.md`，根包的链接 / 锚点门禁需要协调者那一轮覆盖）。

## 尚未确认

- Unity 编辑器里 13 次中 5 次的具体调度路径：机制已由确定性用例证明，并且与 C1 的观察（整 10s 点、读报 closed、写无错）一致；但没有在 Unity + 修复后的 relay 上复跑，修复对那条现场的效果尚未确认。需要 C1 用修复后的 relay 再跑一轮 13 次以上。
- 读期限的同一缺陷没有找到实际触发场景（Gate 读循环几乎一直有读在进行；relay 不用读期限），只按同一修法处理，没有单独的红用例。
- 浏览器（TS SDK 的真实浏览器）与 C# SDK 的 WebSocket 心跳节奏下的回归没跑。

## 建议写进 CHANGELOG（`[Unreleased]`，由协调者写）

修复条目：

> - **修复：WebSocket 连接偶发被自己的旧写期限关掉（relay 与 Gate）**（2026-11-17）：`infra/network/wsconn` 原来把读写期限原样交给 coder/websocket 的 `NetConn`，它的期限计时器一直挂着；调用方每次写前设期限、写完不清（relay 的 `writeLoop`、Gate 的会话写），下一次写恰在上一次期限的到期点开始时，到期回调把新的写当成“写在期限处未完成”，关掉整条连接。relay 的 WebSocket 成员因此偶发在心跳的整 10s 点被断开（读报 `use of closed network connection`，写无错）；独立 Gate 按单帧发送预算的节奏推送时同样会撞上。另外期限已过时紧接着的写不是立即拒绝，而是照样写出或关掉连接。现在期限只在一次读写进行期间交给库：开始时已过期立即拒绝、连接保留；读写结束即停掉计时器。红：`TestAStaleWriteDeadlineDoesNotCloseALaterWrite`、`TestAnExpiredWriteDeadlineRefusesTheVeryNextWrite`（wsconn）、`TestAnEarlierFramesDeadlineDoesNotBreakTheNextWebSocketWrite`（gateway），在 GOMAXPROCS(1) 下固定调度次序，修前不带 -race 49～50/50 红。证据 `docs/maintenance/evidence/ws-write-deadline-20261117/README.md`。

原有那句“**与 TCP 的差别**：期限在一次读写进行中到期时关闭整条连接（TCP 只打断这一次）”仍然成立，建议补半句：

> 期限在一次读写进行中到期时关闭整条连接（TCP 只打断这一次）；换期限即生效、已过的期限在读写开始前即拒绝，这两点与 TCP 相同（2026-11-17 修复前不成立）。

## 建议写进 KNOWN-LIMITS（由协调者写）

“WebSocket 接入”一节“期限在读写进行中到期会关闭整条连接”一条末尾补一句：

> 只有读写进行中到期才关闭；上一次读写留下的期限不作用于之后的读写（2026-11-17 修复 relay 成员在心跳整 10s 点被断开）。一次读写恰在期限处结束时，库的到期回调仍可能关掉紧接着的那次读写，等同于那次超时。

“尚未确认”可另列一条：修复后的 relay 在 Unity 编辑器 C# SDK 上的复跑（见本证据“尚未确认”）。
