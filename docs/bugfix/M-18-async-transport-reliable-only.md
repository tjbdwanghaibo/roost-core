# M-18：`AsyncTransport` 只留 reliable——datagram lane、分片头、批准入契约退场

- 单元：M-18（结构整理，不占 U 编号，不计缺陷）· 关闭 [W-2026-09-23-01](../bug/WANTED.md)，承接 [ARCH-12](ARCH-12-sync-package-layout.md)
- 仓库：roost-core `sync/nettransport`、`sync/lockstep`（注释）、文档
- 分支：`feature/arch-10-sync-policy`
- 破坏性：`AsyncTransport` 的 datagram API 与批准入 API 删除；`NewAsyncTransport` 的下游参数从 `Transport` 收窄为 `ReliableSender`

## 为什么

datagram lane 是老 Replicator 的低延迟状态帧通道：latest-only 折叠、整帧分片批准入、失败不致命。它的前提是"帧是自包含的最新状态"。RR-20260920-02 / U-0260 证明 roost 的 delta 不满足这个前提（被替换的 delta 是再也不会产生的变化），普通 delta 改走 reliable；ARCH-10 的 `entitysync.Manager` 直接以 reliable lane 为唯一出口；M-15 删掉了客户端侧的 `Reassembler`。到 M-17 为止这条 lane 只剩测试在调，却仍占 `channel.go` 一半、维护着一个没人说的 42 字节分片协议。lockstep 从来绕开它直接走裸 `DatagramSender`。维护者拍板整条删。

## 删了什么

| 位置 | 内容 |
| --- | --- |
| `channel.go` | datagram worker、latest-only 折叠（`latest / latestOrder`）、`SendDatagram / SendDatagramBatch`、`AdmitBatch` 与多会话锁序、`AdmissionError`、`Channel / ChannelDatagram / ChannelReliable`、`SendError.Channel`、配置 `MaxDatagramsPerFrame / MaxDatagramBytes / AllowOpaqueDatagrams`、统计 `Datagram*` 六项与 `PendingDatagramFrames / DatagramSendsInFlight` |
| `datagram.go`（整文件） | 42 字节分片头 `DatagramHeader / DatagramMeta`、`FragmentDatagrams / InspectDatagram`、`DefaultMaxFragments`、三个 datagram 错误 |
| `sender.go` | `OutboundFrame`、`AtomicBatchTransport`、`ErrInvalidDatagramBatch`；`DefaultMaxDatagram` 搬到这里（KCP / QUIC 配置默认值仍用） |
| 测试 | `atomic_admission_test.go`（批原子性、按 room 保留最新）整文件；`channel_test.go` 的 latest-only 与残缺批两条；`admission_promises_test.go` 的 `AdmitBatch` 畸形帧用例改为 `SendReliable` 版本 |

保留：UDP / KCP / QUIC 三个协议传输的 `SendDatagram / SendDatagramBatch`（lockstep 的冗余广播走它们）、`CompositeTransport`、`Transport / DatagramSender / DatagramBatchSender / ReliableSender` 契约。`Transport` 的注释改为"协议传输实现的双 lane 契约；`AsyncTransport` 不是 Transport，它是 reliable 前面的队列"。

## 之后的 `AsyncTransport`

每会话一个有界队列 + 一个 worker。`SendReliable` 校验（会话 id、非空、`MaxReliableBytes`）、拷贝、在会话锁下入队；满则 `ErrReliableBackpressure`。下游出错是会话终态：队列作废并计 `ReliableAbandoned`，`ErrorHandler` 拿到原因，worker 退出、调用下游 `RemoveSession`、从表里摘除，id 随即可复用。错误串前缀 `replication transport:` → `nettransport:`。

`NewAsyncTransport(downstream ReliableSender, …)`：codegen 生成的 `NewQUIC / NewKCP / NewUDP` 传入的协议传输或 `CompositeTransport` 都满足，生成代码不变。

## 行为变化（对照旧 promise）

- 失败会话不再以"failed 但仍注册"的状态停留（那是双 worker 时代 datagram worker 卡住才出现的形态）：现在 worker 退出即注销，随后的 `SendReliable` 报 `ErrSessionNotRegistered`，原因只经 `ErrorHandler` 送出。`ErrSessionFailed` 只在 fail 与 worker 退出之间的窗口出现。U-0131 的对应断言改写为这个形状。

## 验证

- `go test ./sync/... ./robot/... ./codegen/internal/roost/`（含 `-race -count=3` 的 nettransport）全绿
- `go build ./...`；lockstep、robot、codegen 生成代码零改动即编译
