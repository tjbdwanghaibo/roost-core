# 大厅 / relay 第 3 批：Go 与 C# SDK 接上换 host（sdkhost 组，2026-11-17）

方案 [FEATURE-2026-11-16-lobby-relay](../../FEATURE-2026-11-16-lobby-relay.md) §4.6；契约 [RELAY](../../../framework/RELAY.md) §2.3、§4A，金样 `client/spec/relay-packets.json`。基于 host 组 23ce9a61 的 relay，并对上协调者 302b3fb6（房主能踢离线座位）与 251ab476（开局后掉线的席位保留、回原座）。

## 交付

| 项 | Go `client/relay` | C# `Roost.Client.Relay` |
|---|---|---|
| 解 64～69 | `host.go` 的 `hostControlHandlers`（init 并进 `controlHandlers`） | `RelayClient.BuiltinControl` 加 6 项；`RelayCodec` 加载荷编解码 |
| 回调（强类型） | `OnYouAreHost(YouAreHost)`、`OnYouAreMember(epoch)`、`OnHostChanging(epoch)`、`OnHostChanged(HostEvent)`、`OnHostResumed(HostEvent)`、`OnRejected(Rejected)` | 同名方法，参数 `RelayYouAreHost` / `ulong` / `RelayHostEvent` / `RelayRejected`，经 `Pump` 调用 |
| 发送 | `HostReady(epoch)`、`Transfer(slot, handoff)`、`SetSuccessors(slots)`、`Snapshot(state)`；`Leave(ctx)` 先发 relay 的 Leave 帧再经 lobby 离开 | `HostReady`、`Transfer`、`SetSuccessors`、`Snapshot`（返回 `Sent` / `TooSoon` / `NotConnected`）；`LeaveAsync` 同 Go |
| 角色 | `Role()`：Slot、Host、Candidate、HostSlot、Epoch | `Role`：同 |
| 选项 | `HostPingInterval`（600ms）、`AutoHostReady`（false）、`SnapshotState`、`SnapshotInterval`（3s）、`SnapshotMinInterval`（1s）、`SnapshotMaxBytes`（256 KiB）、`Compressor`（Deflate） | 同名属性 |
| 压缩 | `Compressor` 接口，`Deflate`（compress/flate）、`NoCompression` | `IRelayCompressor`，`RelayCompressors.Deflate`（DeflateStream）、`None`；两边互通（C# 解 Go 压的字节有用例） |
| 踢人 / 验权 | `ErrKicked`（lobby 403 `kicked`）；重连遇 `lobby_rejected` 停、`lobby_unavailable` 退避再试 | `LobbyErrorCode.Kicked`；`RelayReason.LobbyRejected` / `LobbyUnavailable`；重连同 Go |
| 收帧上限 | `max(MaxPayloadBytes, SnapshotMaxBytes + 8 + 16 KiB)` | 同 |

测试对端 `client/relay/peer_test.go`：`relay.NewServer` 接上 admission（直接调 `lobby.HandleRelayVerifyData`，答复按 `wiring/relay` 的 natsAdmission 同样解释）；WebSocket 单条消息上限改成 `Config.MaxFrameBytes()`（快照帧可大于 `max_payload_bytes`）；新增 `POST <lobby_url>/harness/admission {"mode":"ok|reject|down"}`。命令行接口（`ROOST_RELAYPEER_SERVE=1`、`ROOST_RELAYPEER_WS`、stdout 一行地址 JSON、stdin 关闭即退出、`POST /harness/game-token`）不变。

## 用例

Go（`host_test.go`、`client_test.go`、`golden_test.go`）与 C#（`Roost.Client.Relay.Tests/Program.cs`）各覆盖：

- 主动转交：新房主拿到解压后的交接数据，旧房主 `YouAreMember{1}`，迁移期间成员发两帧只收到一条 `HostChanging{1}`，`HostReady` 后三人都收到 `HostChanged{1, 3, 账号}`，三方 `Role` 正确，转发恢复到新房主；新房主的快照不被 `stale_seq` 拒（旧房主推过 seq 1，转交的 `YouAreHost` 不带 seq）。
- 房主断线：宽限期内成员收到 `HostChanging{0}`；期满后按 `SetSuccessors([3])` 把快照（seq 1、原状态、年龄合理、旧房主离线）交给槽位 3 而不是 2，候选 `AutoHostReady` 接手。
- 宽限期内房主（自动重连）回来：成员 `HostResumed{0, 1}`，房主再收到在线成员的 `MemberJoined`，没有换 host。
- 房主 `Leave`：不等宽限期，候选顺序 `[4, 2]` 生效，来源 None。
- 快照节奏：SDK 拒发太密的（Go `ErrSnapshotTooSoon` / C# `TooSoon`）；SDK 最小间隔 1ms、relay 1s 时 relay 回 `Rejected{snapshot, too_soon, seq 2}`；转交给不在线的槽位 `Rejected{transfer, bad_target}`；成员不能发；压缩后超限不发。
- 定时快照：当房主时按间隔推，候选拿到的状态与 seq 对得上；候选回 `HostReady` 之前不向游戏要状态，之后才推。
- 房主心跳：relay `host_idle_timeout` 用缺省 2s，房主不发游戏帧，5.5s 内没有任何换 host 通知、房主没断，之后游戏帧照常到房主。
- 踢人新语义：被踢账号再 join → lobby 403 `kicked`（Go `ErrKicked` / C# `LobbyException.Code == Kicked`）；被踢账号的旧令牌直连 relay → `Closed{kicked}`；席位已让出的成员拿旧令牌直连 relay → `Closed{lobby_rejected}`。
- 重连与验权：对端注入 `down` 时按退避至少试 3 次不放弃，恢复后连回原槽位；注入 `reject` 时只试 1 次，最后一条 Disconnect 是 `lobby_rejected`。
- 开局后（协调者 251ab476 / 302b3fb6）：成员掉线自动重连回原槽位；关掉自动重连的成员自己 Rejoin 回原座；房主踢离线座位收到这个槽位的第二条 `MemberLeft{kicked}`，之后它的 join 答 `kicked`；新来的人 `room_started`。
- 候选不回 `HostReady`：超时后 `YouAreMember{1}`，没有别的候选就关房，成员 `Closed{room_closed}` 不重连（Go 用 relay 配置缩短，C# 用缺省 10s）。
- 金样：13 条新增条目逐字节一致。Go 经 SDK 的公开方法（`Transfer`、`SetSuccessors`、`Snapshot`（seq 从 6 接到 7）、`HostReady`、`Leave`）写到 `net.Pipe`、与金样比；收的 8 条经 `dispatch` 解出结构并逐帧核对 `Role`。C# 对同一份金样用 `RelayCodec` 编码比对与解码字段核对，另用本地 TcpListener 扮 relay：先让客户端当上房主（`YouAreHost{epoch 0, seq 6}` → `HostChanged` 自己），经公开方法发出的 5 帧与金样相同；再逐帧发 8 条收的金样，每帧后核对回调与 `Role`，最后 `Closed{lobby_rejected}` 结束连接。
- 单元：`TestSnapshotSeqContinuesAcrossHosts`（从 YouAreHost 的大 seq 接；转交时从 `epoch<<32` 接）、压缩往返与解压上限、C# 解 Go deflate 的输出。

## 命令与结果

主检出（HEAD 302b3fb6 + 本组未提交改动），真实 Redis（`~/.roost-it`，对端 `room directory redis`）：

```sh
source ~/.roost-it/roost-dataengine-it/env.sh
GOWORK=off go test -race -count=1 -v ./client/relay/     # go-test.txt：18 个用例全过，约 10s
GOWORK=off go test -race -count=3 ./client/relay/        # go-race-count3.txt：ok 27s；另跑过 -count=5 一次 ok
GOWORK=off go vet ./client/relay/                         # ok
(cd client/dotnet && dotnet run --project Roost.Client.Relay.Tests -- <root>)   # cs-test.txt：345 checks passed，约 30s
GOWORK=off go test -count=1 -run 'Link|Anchor|Doc' .     # ok（README 链接）
dotnet build-server shutdown
```

`Roost.Client.Relay` 以 `TreatWarningsAsErrors` 编译 0 警告。

## 变异（撤掉关键的一行，用例变红，再恢复）

Go 9 处（go-mutations.txt），C# 8 处（cs-mutations.txt），全部变红：

| 变异 | Go | C# |
|---|---|---|
| 快照 seq 不再从 YouAreHost / `epoch<<32` 往上接 | G1：seq 接续单元、转交用例（新房主快照被 stale_seq 拒）红 | C1：假 relay 金样（seq 1 ≠ 7）红 |
| HostChanged 不把自己记成房主（角色更新） | G2 红 | C2 红 |
| 房主心跳用成员的 10s 间隔 | G3：房主心跳用例红 | C3：全量时先在定时快照用例红；`ROOST_RELAY_TESTS_ONLY='host heartbeat'` 单跑房主心跳用例红（“no host change for 5.5s”） |
| YouAreHost 状态字节不解压（交接数据解析） | G4 红 | C4 红 |
| `lobby_rejected` 当成可重试（被拒后停止重连） | G5 红 | C5 红（gave up after 50 attempts） |
| `lobby_unavailable` 不重试 | G6 红 | C6 红 |
| lobby 错误名 `kicked` 不映射 | G7 红 | C7 红 |
| AutoHostReady 不回 | G8 红 | — |
| 候选也定时推快照 | G9 红（第一次 GREEN，见下） | C8 红 |

G9 第一次没变红：用例里候选与房主共用一个计数、在 `Leave` 返回之后才读基线，候选在 `Leave` 返回前就已推过一次，被算进了基线。改成候选单独计数、并断言回 `HostReady` 之后才推，重跑变红。两份文件都保留了这段经过。

## 拿不准的点

1. **快照 seq 跨房主**：relay 要求 seq 在房间内递增（跨房主），可 `YouAreHost` 只在来源是快照时带 seq，主动转交时为 0，而 relay 仍存着旧房主更大的 seq，新房主照“从 YouAreHost 的 seq 往上接”会被 `stale_seq` 拒。SDK 的做法：seq 取 `max(本地, YouAreHost.SnapshotSeq, epoch<<32)` 再加 1。epoch 每次换 host 加 1，所以不需要 relay 改协议。TS 组要用同一规则，否则混合房间里 TS 新房主在转交后会被拒。另一种做法是 relay 在交接数据来源时也带上存着的 seq（协议改动，需协调者定）。
2. **压缩**：方案写 LZ4 或 zstd；为了不加第三方依赖，缺省用各语言自带的 RFC 1951 原始 deflate（Go `compress/flate`、C# `DeflateStream`、浏览器可用 `CompressionStream('deflate-raw')`），留 `Compressor` / `IRelayCompressor` 接口给游戏换 LZ4 / zstd。SDK 对快照与交接数据都做透明压缩、在 `YouAreHost` 里透明解压（游戏只看原始状态字节），所以房间里每个客户端必须用同一种；TS 组需对齐 deflate-raw。解压上限 16 MiB（常量，没做成选项）。
3. **`Leave`**：现有 `Leave(ctx)` / `LeaveAsync` 已是“经 lobby 离开”。没有另加一个同名方法，而是让它先在 relay 上发 `Leave` 帧（房主不等宽限期、立即换 host；lobby 不通也能离开），再经 lobby 让出席位。lobby 的 leave 本身也会让 relay 立即换 host，两步重叠无害。
4. **角色在 Welcome 时的 epoch**：Welcome 不带 epoch，连上后还没见过换 host 时 `Role.Epoch` 是 0，不一定是房间的真实 epoch；只影响查询，不影响 `HostReady`（用 YouAreHost 带来的 epoch）。
5. **候选的心跳**：候选（收到 YouAreHost、还没 HostChanged）也按房主的 600ms 发 Ping；relay 在收到 HostReady 时才把它的空闲上限改成 2s，提前加快无害。
6. **Go 的定时快照回调线程**：`SnapshotState` 在心跳 goroutine 上调用、与其他回调并发（文档写明由游戏保证并发安全）；C# 在 `Pump` 里调用。
7. **踢人与旧令牌**：任务里写“拿没过期的旧令牌连 relay，被拒并收到 lobby_rejected”。实测被踢账号的旧令牌在同一个 relay 上先被 relay 本地的被踢记录挡下，收到的是 `Closed{kicked}`（service/relay `checkWithLobby` 先查 `isKicked`；relay_test.go:433 同样断言 kicked）。`host-frames.md` 的说法与代码不符；RELAY.md §3 第 7 条“本地记录只挡报告到达之前那一小段时间”也与代码不符（本地记录在房间存续期间一直生效）。SDK 用例按代码断言：被踢账号旧令牌 → `kicked`；席位已让出的成员旧令牌 → `lobby_rejected`。两种都不重连。请协调者定以哪边为准，改文档或改 relay。
8. **离线座位被踢**（302b3fb6）：SDK 不维护离线座位表，只把第二条 `MemberLeft{kicked}` 原样交给游戏，文档写明“游戏自己维护的话收到 kicked 就删座位”。
9. 没有在 Unity Editor / IL2CPP 上跑过 C#；Go 与 C# 都只连 TCP。

## 建议写进 CHANGELOG / KNOWN-LIMITS 的文字（由协调者统一写）

- CHANGELOG：Go `client/relay` 与 C# `Roost.Client.Relay` 接上换 host——解 relay 的 YouAreHost / YouAreMember / HostChanging / HostChanged / HostResumed / Rejected 并以强类型回调交给游戏，SDK 记当前角色（槽位、房主 / 候选、房主槽位、epoch）；房主 `Transfer`、`SetSuccessors`、`Snapshot`，新房主 `HostReady`（可选自动回）；当房主时可定时推快照（缺省 3s，最小间隔 1s），快照与交接数据缺省用原始 deflate 压缩、可换实现；房主心跳 600ms；收帧上限放大到装得下 YouAreHost；`Leave` 先在 relay 上发 Leave 再经 lobby 离开；被踢记在 lobby（Go `ErrKicked`、C# `LobbyErrorCode.Kicked`），自动重连遇 `lobby_rejected` 停、遇 `lobby_unavailable` 退避重试。测试对端接上 relay→lobby 验权，并新增 `/harness/admission` 故障注入。原先预留的 `OnYouAreHost` 等回调参数从原始帧换成解好的结构（未发布过，不做兼容）。
- KNOWN-LIMITS（框架限制）：快照 seq 跨房主靠 SDK 的 `epoch<<32` 下限保证（主动转交的 YouAreHost 不带 seq），换用别的客户端实现时必须遵守同一规则；房间内所有客户端必须用同一种快照压缩；C# SDK 未在 Unity 里运行过。
