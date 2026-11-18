# 开房间 SDK 同一任期内并发快照以发起顺序为准（2026-11-18，维护者定 A）

接 `docs/maintenance/evidence/v154-review-fixes-20261118/README.md`“要维护者定的点”1：维护者选 A——在 SDK 里按发起顺序编号，
已发出更晚发起的一份后，落后的那份丢弃，三份 SDK 一致。修复基于 `4894a456`（含 `fe9385bc` 的任期绑定）。
本机 macOS（Darwin 25.5，arm64），Go 1.27，node v26.7.0，dotnet 10.0.302。

| 项 | 结论 | 提交 |
|---|---|---|
| 同任期并发快照乱序覆盖（Go / TS / C#） | 已修复、已验证（三份 SDK 红→绿、变异变红） | `ac1dcf43` |

## 根因

`fe9385bc` 只在分配 seq 前核对“连接与房主任期没变”。同一任期内两份快照并发时，seq 在“准备完（取状态、压缩）”那一刻
按当前计数分配，与发起顺序无关：先发起、准备得更慢的那份拿到更大的 seq；relay `service/relay/room.go` `storeSnapshot` 只按
seq 递增判断新旧，把它当成最新一份存下，盖掉后发起的较新状态，下一任房主恢复成旧状态。

- Go `client/relay/host.go:405`（`4894a456`）：`snapshotIn` 在锁内只查任期与最小间隔就 `c.host.snapSeq++`；压缩在锁外，
  定时快照的 `SnapshotState` 也在锁外（心跳 goroutine），与游戏从别的 goroutine 调的 `Snapshot` 可以重叠。
- TS `client/ts/src/client.ts:691`（`4894a456`）：`pushSnapshot` 在 `take` / `packState` 两个 await 之后按 `this.seq + 1n`
  取 seq；`syncSnapshotTimer`（第 859 行）的 `setInterval` 不等上一份完成，`take` 慢于 `intervalMs` 时定时推送自己就会重叠。
- C# `client/dotnet/Roost.Client.Relay/RelayClient.cs:624`（`4894a456`）：`SnapshotIn` 压缩在锁外、锁内 `++snapshotSeq`；
  `Snapshot` 可从任意线程调，与 Pump 里的定时快照或别的线程的 `Snapshot` 重叠。

## 修法（三份一致）

每份快照在开始准备时（与记下连接 / epoch 同一处，Go `hostTerm()`、C# `CurrentHostTerm()` 在锁内，TS 在第一个 await 之前）
取一个只增不减的发起号（Go `hostState.snapStarted` → `hostTerm.ticket`、C# `snapshotStarted` → `HostTerm.Ticket`、TS
`snapshotTicket`）。分配 seq 时在同一把锁里（TS 是同一段同步代码）核对：号小于“已分配 seq 的最大号”（Go `snapSent`、C#
`snapshotSent`、TS `snapshotSentTicket`）就丢弃——不发、不占 seq、不更新最小间隔计时、不触发 snapshotSent。分配成功时把
“已分配的最大号”记成自己的号（核对保证它单调）。晚发起的一份因最小间隔（TooSoon）没分配 seq 时不算，较早那份照常可发。
号跨任期也只增不减：不同任期的旧份先被连接 / epoch 核对挡下，号只在同一任期内起作用。

**报告值沿用换任期的那一个**（Go `ErrSnapshotStale`、C# `RelaySnapshotResult.Stale`、TS 返回 `undefined`）：两种情况对调用方
的含义与处理相同——“这一份的状态已经过时，没发，也没占 seq，不要重试同一份；更新的状态已经（或将会）由别的推送带上去”；
单独再加一个值只增加公开 API，调用方没有不同的处理可做。Go 错误文本与三份的注释改成同时覆盖两种情况。

**文档**：`docs/framework/RELAY.md` §4A“快照”加一条“同一任期可以并发推，以发起顺序为准、后发起的胜出”；
`client/relay/README.md`、`client/ts/README.md`、`client/dotnet/Roost.Client.Relay/README.md` 同步。

## 红（修前，`4894a456` 的产品代码 + 新用例）

Go `client/relay/host_test.go` 新增 `TestAnEarlierStartedSnapshotDoesNotOverwriteALaterOne`（真实 Go lobby + relay 对端；
Snapshot 路径把 older-state 卡在压缩里，SnapshotState 路径把定时快照卡在向游戏要状态里；newer-state 先发出后再放行）：

```
--- FAIL: TestAnEarlierStartedSnapshotDoesNotOverwriteALaterOne/Snapshot (0.04s)
    host_test.go:455: the earlier-started snapshot returned <nil>, want ErrSnapshotStale
    host_test.go:467: B recovered seq 2 state "older-state" (source 2), want seq 1 newer-state
--- FAIL: TestAnEarlierStartedSnapshotDoesNotOverwriteALaterOne/SnapshotState (0.04s)
    host_test.go:467: B recovered seq 2 state "older-state" (source 2), want seq 1 newer-state
```

TS（`client/ts/test/host.test.ts` 假 relay 三条，`client/ts/test/relay.test.ts` 真实对端一条；编译产物同 `scripts/test-client-ts.sh`）：

```
✖ an earlier-started snapshot whose take finishes after a later one is sent is dropped
  AssertionError [ERR_ASSERTION]: the earlier-started snapshot is dropped
  2n !== undefined
✖ an earlier-started snapshot whose compress finishes after a later one is sent is dropped
  AssertionError [ERR_ASSERTION]: the earlier-started snapshot is dropped
  2n !== undefined
✖ timer pushes that overlap because take is slower than intervalMs never send an older state after a newer one
  AssertionError [ERR_ASSERTION]: relay got the older state after a newer one: newer-state, older-state, newer-state, newer-state, newer-state, newer-state
✖ an earlier-started snapshot that finishes last is dropped: the next host recovers the later-started state   （真实对端）
  AssertionError [ERR_ASSERTION]: the earlier-started snapshot is dropped
  2n !== undefined
```

C# `client/dotnet/Roost.Client.Relay.Tests/Program.cs` 新增「an earlier-started snapshot is dropped once a later one is sent」
（真实对端，older-state 从工作线程调 `Snapshot` 卡在压缩里；`ROOST_RELAY_TESTS_ONLY="earlier-started snapshot"`）：

```
[tcp] peer: lobby http://127.0.0.1:50489, relay 127.0.0.1:50487, room directory memory
FAILED: System.Exception: Check failed: the earlier-started snapshot is dropped, got Sent
```

红落在承诺本身：Go / TS 真实对端断言下一任房主恢复的 seq 与内容（修前 B 恢复 seq 2 的 older-state）；TS 假 relay 断言 relay
收到的帧只有 newer-state、seq 连续、`snapshotSeq` 没被占、`onSnapshotSent` 只有 1 次；定时推送那条直接看到 older-state 在
newer-state 之后发出。C# 的红停在第一条断言（返回 `Sent`），其后的“B 恢复 seq 1 newer-state”在修后通过，修前没有单独记录。

确定性：较早的一份由闸门（Go / C# `ManualResetEvent` / channel，TS Promise）卡住，测试等到“已进入卡点”再推较新的一份、确认
发出（Go / C# 返回成功，TS 拿到 seq 1；定时推送那条等到 relay 收到第一帧），再越过两边的最小间隔（Go 30ms，TS / C# 真实对端
1200ms，relay 缺省 1s）才放行。Go 两条用例 `-race -count=5` 连跑全绿。

## 绿（修后，`ac1dcf43`）

| 命令 | 结果 |
|---|---|
| `GOWORK=off go vet ./client/relay/` | 通过 |
| `GOWORK=off go test -race -count=1 ./client/relay/` | ok（10.6s） |
| `GOWORK=off go test -race -count=5 -run 'TestAnEarlierStartedSnapshot\|TestASnapshotPrepared' ./client/relay/` | ok |
| `GOWORK=off scripts/test-client-ts.sh`（node_modules 软链主检出，跑完已删软链、dist / dist-test，未 npm install） | 71 tests，71 pass，0 fail，0 skipped（原 67 + 新 4） |
| `GOWORK=off dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$PWD"` | all 866 checks passed（TCP 与 WebSocket；原 846） |

之后 `dotnet build-server shutdown`、清 `client/dotnet/Roost.Client.Relay{,.Tests}/{bin,obj}`；没有残留的对端进程。

## 变异（撤掉发起号核对那一行，用例必须红；恢复后 `cmp` 与修后文件一致）

| 变异 | 结果 |
|---|---|
| Go 删 `case term.ticket < c.host.snapSent:` 分支 | 两条路径都红（同上红文本：`returned <nil>, want ErrSnapshotStale`；`B recovered seq 2 state "older-state"`） |
| TS 把核对改成 `if (ticket < 0 && ticket < this.snapshotSentTicket)`（直接删会因 `noUnusedLocals` 编译失败） | 4 条都红（3 条 `the earlier-started snapshot is dropped  2n !== undefined`，定时那条 `relay got the older state after a newer one`） |
| C# 删 `if (term.Ticket < snapshotSent) return RelaySnapshotResult.Stale;` | `Check failed: the earlier-started snapshot is dropped, got Sent` |

## 未做 / 尚未确认

- 未跑全量 `go test ./...`、pretag、`scripts/test-client-dotnet.sh` 的其余部分（Roost.Client.Tests / Terrain / 生成工程，
  不涉及 `RelayClient.cs`）；按批次规则由协调者统一跑。
- C# 没有加“定时快照（Pump 里 `SnapshotState`）与 `Snapshot` 重叠”的用例：Pump 由测试线程手动驱动，把 Pump 卡在
  `SnapshotState` 里会同时卡住测试；这条路径走的是同一个 `CurrentHostTerm` / `SnapshotIn`，与 Go 的 SnapshotState 路径
  （已覆盖）同构，未单独验证。
- 只在 macOS 上跑，Linux 未跑（纯 SDK 逻辑，与平台无关的推断尚未实测）。

## 建议写进 CHANGELOG `[Unreleased]` 的文字

- **开房间 SDK（Go / TS / C#）：同一任期内并发推快照以发起顺序为准**（接 RV-20261118-02，维护者 2026-11-18 定）。同一任期里
  两份快照并发（游戏从几处同时调，或手动推与定时推重叠；TS 定时推送在 `take` 慢于 `intervalMs` 时也会重叠）时，原来先发起、
  取状态或压缩更慢的那份会拿到更大的 seq，盖掉 relay 已存的较新状态，下一任房主恢复成旧状态。现在每份在开始准备时取发起号，
  比它晚发起的一份已经发出时这一份丢弃、不发、不占 seq，报告方式与换任期相同：Go `ErrSnapshotStale`（错误文本改为同时说明
  两种情况）、C# `RelaySnapshotResult.Stale`、TS `pushSnapshot` 返回 `undefined`。见 [RELAY §4A](docs/framework/RELAY.md)。

## 建议写进 KNOWN-LIMITS 的文字

- 删去 v154-review-fixes-20261118 建议里的遗留条目“开房间 SDK 同一任期内并发推快照，较早发起、较晚取完状态的那份仍会以更大
  seq 覆盖较新的一份”，改为“已修复（`ac1dcf43`），证据 evidence/snapshot-start-order-20261118”。
