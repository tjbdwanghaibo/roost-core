# 主动转交绑定房主任期（RV-20261118-07）与自动快照用例的同步点（RV-20261118-09）

基线：main `8a2cf3a9`。审查来源：分支 `fix/core-game-server-review` 上的 `docs/maintenance/MAIN-REVIEW-20261118.md`
与探针 `review_transfer_test.go.txt`、`review_auto_test.go.txt`（只用 `git show 88d55951:<路径>` 读取，未合并那条分支）。
环境：macOS，`GOWORK=off`，联机用例用本机传输与内存房间目录。

## RV-20261118-07：旧任期发起的转交在新任期生效

**根因**：Go `Client.Transfer`（[host.go](../../../../client/relay/host.go)）先 `IsHost()`，再锁外压缩交接数据，压缩完用
`SendControl` 在**此刻**的连接上发出；TS `transfer`（[client.ts](../../../../client/ts/src/relay/client.ts)）压缩完只看
`role.host`；C# `Transfer`（[RelayClient.cs](../../../../client/dotnet/Roost.Client.Relay/RelayClient.cs)）`RequireHost()` 后压缩、
发出，不核对。三者都不核对“发起时的连接与任期”。relay（[room.go](../../../../service/relay/room.go) `transfer`）只看
“发送者是不是此刻的房主”，Transfer 帧不带 epoch。A 在 epoch 0 发起、压缩期间 A→B→A（epoch 2），旧的那次照样从房主连接发出，
relay 开启 epoch 3，目标收到旧交接数据。

**修复**（协议改动，线上未部署，不做兼容）：

- 线上格式：`Transfer` 载荷改为 `[epoch 8B][交接数据]`（`wire.RelayTransfer`），新增拒收原因 6 `stale_epoch`；
  [relay-packets.json](../../../../client/spec/relay-packets.json) 的 `relay_transfer` 金样随之改为带 epoch 2。
- relay：`transfer` 在 NotHost 之后核对 `t.Epoch == r.epoch`，不等回 `Rejected{stale_epoch}`、房间不变；转交帧的载荷上限
  与快照相同为 `snapshot_max_bytes + 8`。
- 三份 SDK：发起时记下连接与 epoch，压缩完在锁内（TS 同步段内）核对，变了不发——Go `ErrTransferStale`、C#
  `RelayTransferResult.Stale`（`Transfer` 返回值由 bool 改为 `RelayTransferResult`）、TS `transfer` 返回 `false`；通过后在
  记下的连接上发出带这个 epoch 的帧。SDK 核对之后、帧到达 relay 之前任期仍可能变化，由 relay 的 epoch 核对兜住。

### 红（修前）

审查探针放进 `./client/relay`，在 `8a2cf3a9` 未改代码上：

```
GOWORK=off go test -count=3 -run TestReviewEarlierTransfer ./client/relay
    review_transfer_test.go:37: epoch 3 offered to C with state "old"   （三轮相同）
```

原文见 [red-transfer.txt](red-transfer.txt)。relay 一侧的正式用例 `TestTransferFromAnEarlierHostTermIsRejected`
（[host_test.go](../../../../service/relay/host_test.go)）在只改了 wire、relay 未改时：

```
GOWORK=off go test -count=1 -run TestTransferFromAnEarlierHostTermIsRejected ./service/relay
    host_test.go:201: slot 1 got channel 3 op 64 seq 1 payload "\x00\x00\x00\x00\x00\x00\x00\x03"; want control op 69 seq 0
```

即旧 epoch 的转交被执行（房主收到 YouAreMember{epoch 3}），见 [red-server.txt](red-server.txt)。

### 绿

- Go：`go test -race -count=1 ./client/relay ./client/wire ./service/relay ./wiring/relay` 全过；新用例
  `TestATransferPreparedInAnEarlierHostTermIsDropped`、`TestATransferPreparedOnAnEarlierConnectionIsDropped`
  （[client/relay/host_test.go](../../../../client/relay/host_test.go)）、`TestTransferFromAnEarlierHostTermIsRejected`、
  `TestTransferAtTheSizeLimitIsAccepted` 以 `-race -count=10` 复跑通过。审查探针修后三轮通过（旧的那次返回错误，探针提前返回）。
- TS：`scripts/test-client-ts.sh` 74 项全过（新增假 relay 的任期 / 连接两条与真实对端 A→B→A 一条）。
- C#：`dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- <repo>` `all 924 checks passed`（新增 `StaleTransfer`，
  TCP 与 WebSocket 各一轮，含断线重连；金样里 `DecodeTransfer` 解出 epoch 2）。

### 变异

| 变异 | 结果 |
|---|---|
| Go `Transfer` 去掉连接 / 任期核对（relay 核对保留） | 两条 Go 新用例红：`returned <nil>, want ErrTransferStale`；连接那条拿到旧连接的写错误（[mut-go-client.txt](mut-go-client.txt)） |
| relay `transfer` 去掉 epoch 核对（SDK 核对保留） | `TestTransferFromAnEarlierHostTermIsRejected` 红，Go SDK 用例仍绿（SDK 先挡住）（[mut-server.txt](mut-server.txt)） |
| 两处都去掉 | Go 新用例红；审查探针复现 `epoch 3 offered to C with state "old"`（[mut-both.txt](mut-both.txt)） |
| TS `transfer` 恢复修前（只看 `role.host`，epoch 取发送时的） | 新增 3 条全红，真实对端那条 `the epoch-0 transfer is dropped: true !== false`（[ts-mut.txt](ts-mut.txt)） |
| C# `Transfer` 恢复修前（不核对，连接与 epoch 取发送时的） | `the epoch-0 transfer is dropped, got Sent`（[cs-mut.txt](cs-mut.txt)） |

## RV-20261118-09：自动快照用例的同步点

**根因**：`TestAutoSnapshotIsPushedWhileHost` 等 `pushed >= 3`，而 `pushed` 在 `SnapshotState` 回调里递增，压缩、分配 seq、
写帧都在其后；`waitPong` 的 Ping 由测试 goroutine 发出，可能排在第三份快照前面，离开后候选拿到 seq 2。是夹具问题，SDK 行为无误。

**修复（只改测试）**：`newPlayer` 允许用例自带 `Dial`（[client_test.go](../../../../client/relay/client_test.go)），用例包一层
连接、数写出的 Snapshot 控制帧，等第三份**写到连接上**之后再 Ping/Pong、离开；第三次取状态故意慢 150ms，把窗口固定下来。

- 红（审查探针，第三次取状态卡住）：`offer seq 2 state "auto-2" (pushed 3)` 三轮相同（[red-auto.txt](red-auto.txt)）。
  探针自带旧的等待条件，修后仍会红，它只用于复现夹具问题，未放进仓库。
- 绿：`go test -race -count=10 -run TestAutoSnapshotIsPushedWhileHost ./client/relay` 通过。
- 变异：把等待条件改回 `pushed.Load() >= 3`，三轮全红 `offer seq 2 state "auto-2" (pushed 3, written 2)`
  （[mut-auto.txt](mut-auto.txt)）。

## 未覆盖

- `wiring/relay/real_integration_test.go`（`integration` 标签，要 NATS）只改了转交帧的载荷，`go vet -tags integration` 通过，未运行。
- Unity Player、浏览器真机未测；跨语言字节一致只经金样与真实 Go relay 对端核对。
