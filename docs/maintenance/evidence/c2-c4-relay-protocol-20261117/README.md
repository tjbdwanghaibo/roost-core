# 交接 C2、C4：Welcome 带 epoch、前台上报、状态字节的压缩方式标识（2026-11-17）

这一轮按维护者 2026-11-17 的决定（“按推荐”）实现以下内容：

- C2 一：Welcome 带 epoch。
- C2 二：前台上报，选项 1。选项 3 不做。
- C4 A：缺省仍用 deflate，SDK 不带 zstd，给出接入示例。
- C4 D：状态字节前加 1 字节压缩方式标识。

现行契约见 [RELAY §2.2、§2.3、§7](../../../framework/RELAY.md)，各 SDK 的用法见各自的 README。本目录只放证据：

- 先红后绿的原文。
- 变异的原文。
- 最终运行的结果。

## 做了什么

| 项 | relay | Go SDK | C# SDK | TS SDK |
|---|---|---|---|---|
| Welcome 固定 13 字节 `[角色][房主槽位 4B][epoch 8B]` | 建房与 `welcomeLocked` 都写 `r.epoch`；选人阶段进房的连接拿到房主槽位 0、epoch 是新的一轮 | `wire.RelayWelcome.Epoch`，解码只接受 13 字节；`Role().Epoch` 与 `Welcome().Epoch` 在 Welcome 时（含重连）更新 | `RelayWelcome.Epoch`，只接受 13 字节；`Role.Epoch` 同 | `Welcome.epoch`（bigint），只接受 13 字节；`client.epoch` 同 |
| `Presence = 85`（成员 → relay，Seq 0，`[1/0]`） | 只核长度与取值，不对就按协议错误断开；转成 `MemberPresence = 70`（Seq 为成员槽位）只发给已就绪的房主；选人阶段、宽限期、房主自己发的都丢弃；计入帧率限额 | `SetForeground(bool)`、`OnMemberPresence`；收到 HostChanged、每次连上（Connect、Rejoin、自动重连）后自动重发 | `SetForeground(bool)`（无连接时返回 false）、`OnMemberPresence`；重发规则同 Go | `setForeground`、`onMemberPresence`、`successorsDeclared`；`WebHost` 按 `visibilitychange` 上报；游戏没显式 `setSuccessors` 时，房主自动声明“前台在前、槽位升序”；页面隐藏让出房主时也按这个顺序 |
| 状态字节 `[标识 1B][压缩后]`（空状态不带标识） | 不解析 | `Compressor.ID()`；`ErrUnknownCompression` | `IRelayCompressor.Id`；`RelayUnknownCompressionException` | `Compressor.id`；`UnknownCompressionError`；`packState` / `unpackState` |
| 各 SDK README 的“接入自己的 zstd / LZ4”示例 | — | `client/relay/host_test.go` 末尾，编译 | `Roost.Client.Relay.Tests/Program.cs` 末尾，编译并用桩走一遍 | `client/ts/test/compress.test.ts` 末尾，编译并用桩走一遍 |

压缩方式标识的规则如下：

- 0 表示不压缩，1 表示 RFC 1951 原始 deflate。这两种内置方式谁都解得开。
- 2～255 归游戏自带的压缩方式。示例里 zstd 用 2，LZ4 用 3。
- 收到状态时先认自己配置的 compressor，再认内置的 0 / 1。
- 标识不认识时，报“不认识的压缩方式”错误。
- 标识认识但内容坏时，报解压错误。
- `snapshot_max_bytes` 把标识这 1 个字节算在内。

金样 `client/spec/relay-packets.json` 有 31 条，变化如下：

- `relay_welcome` 改为 epoch 5。
- 快照、交接数据各条的状态字节前加上 `0x00`。
- 新增 `relay_presence`、`relay_member_presence`、`relay_you_are_host_deflate`（一个 stored deflate 块，内容 "hi"）、`relay_you_are_host_unknown_compression`（标识 0x7F）。

## 先红后绿

顺序是：先改金样和测试，再加 API 桩，这样拿到的是行为上的红，不是编译错误；最后实现。各文件是原样输出：

- [red-go-golden-step1.txt](red-go-golden-step1.txt)、[red-ts-golden-step1.txt](red-ts-golden-step1.txt)、[red-cs-golden-step1.txt](red-cs-golden-step1.txt)：第一步只改金样。三份 SDK 都红在 Welcome、标识与新帧上。
  - Go：`relay_transfer: read read pipe: i/o timeout`，SDK 发出的交接数据没带标识，与金样不符，假 relay 读不到预期的帧。
  - C#：`Check failed: C# encodes relay_welcome like Go`。
  - TS：`relay_welcome` 期望 29 字节帧；`14 base frames + 13 host-change frames` 期望 27。
- [red-relay.txt](red-relay.txt)：relay 侧用例。
  - `welcome after two host changes {Host:false HostSlot:2 Epoch:0} slot 5, want host slot 2 epoch 2`。
  - presence 没转发，收到的是 Pong 而不是 `msg 70 seq 2`。
- [red-go-sdk.txt](red-go-sdk.txt)：Go SDK 加了 API 桩之后的红。
  - golden：`welcome {... Epoch:0}`。
  - 标识：`markers: deflate 2a, none 6162`，没有标识字节。
  - 晚进房：`late welcome {Slot:4 ... HostSlot:3 Epoch:0}`。
  - 重发：`timed out waiting for member presence at the host`。
- [red-ts.txt](red-ts.txt)：TS 加桩后，15 条失败、44 条通过。失败的包括 Welcome epoch、setForeground 与重发、前台优先、标识，以及金样相关的换 host 用例。
- [red-cs-1.txt](red-cs-1.txt)：C# 加桩后，`Check failed: presence sent`（`HostFramesAgainstFakeRelay`）。
- 中间绿：[green-ts-1.txt](green-ts-1.txt)、[green-cs-1.txt](green-cs-1.txt)。

## 变异（逐条改回一处、跑对应测试、恢复）

原文见 [mutations.txt](mutations.txt)，由 [run_mutations.sh](run_mutations.sh) 加 [mutate.py](mutate.py) 一次跑出，两个脚本都在本目录；脚本里的路径是当时的 worktree 与临时目录。每条都红在承诺本身上：

| 编号 | 改回什么 | 红在哪 |
|---|---|---|
| r1 | relay：`welcomeLocked` 不写 epoch | `TestWelcomeCarriesTheRoomEpoch`：`welcome after two host changes {... Epoch:0} slot 5, want host slot 2 epoch 2` |
| r2 | relay：`Presence` 转给所有在线成员而不只是房主 | `TestPresenceReachesOnlyTheHost`：`slot 2 expected silence, got ... msg 70 seq 2` |
| m3 | Go：Welcome 时保留旧 epoch | 金样 `welcome {... Epoch:0}`、`after relay_welcome role ... Epoch:0, want ... Epoch:5`；真实对端 `epoch-late role ... Epoch:0, want ... Epoch:1` |
| m4 | Go：HostChanged 不重发前台状态 | `TestPresenceIsResentAfterHostChangedAndReconnect`：`timed out waiting for presence re-sent to the new host` |
| m5 | Go：自动重连成功后不重发 | 同一用例：`timed out waiting for presence after b reconnected` |
| m6 | Go：不认识的标识当成自己的压缩方式解 | 金样 `unknown-marked offer: state "hi" err <nil>`；`TestStateCarriesTheCompressionMarker` 的混用用例拿到的是解压错误而不是“不认识” |
| t1 | TS：Welcome 不更新 epoch | 2 条：Welcome epoch（真实对端）、`client.epoch follows it on connect and on every reconnect` |
| t2 | TS：HostChanged 不重发 | 2 条：真实对端 presence、`setForeground ... re-sent after HostChanged` |
| t3 | TS：每次连上（attach）不重发 | 3 条：上面两条加 `a member page reports visibilitychange as Presence` |
| t4 | TS：`foregroundFirst` 只按槽位排 | `foreground first: a hidden host hands off to the member in the foreground ...` |
| t5 | TS：不认识的标识当成自己的压缩方式解 | 3 条：金样标识、README zstd 示例、`the compression marker: an unknown method is UnknownCompressionError ...` |
| t6 | TS：游戏显式声明过候选顺序也自动覆盖 | 同 t4 那条（断言游戏声明的顺序不被覆盖） |
| cs1 | C#：Welcome 时保留旧 epoch | `after relay_welcome: role ... epoch 0 == ... epoch 5` |
| cs2 | C#：HostChanged 不重发 | `Timed out waiting for re-sent 1`（`Presence` 真实对端用例） |
| cs3 | C#：自动重连成功后不重发 | `Timed out waiting for after b reconnected` |
| cs4 | C#：不认识的标识当成自己的压缩方式解 | 金样 `Check failed: unknown marker accepted` |

（TS 的变异用 `node --test` 直接跑 dist-test，失败数见 `ℹ fail`；管道末尾是 `sort -u`，所以这几条的 `exit` 是 0，红以 `ℹ fail` 为准。各条改动的原文在 `mutations/<编号>.old` / `.new`。变异时 Redis 地址同最终运行；Go 的 `WARN lobby: ... no relay instance is available for transport ws` 是已有用例故意请求没有 ws relay 的房间，与本轮无关。）

## 最终运行（全部在本 worktree 上，Redis 用 `~/.roost-it` 的 17379）

| 命令 | 结果 |
|---|---|
| `ROOST_DATAENGINE_IT_REDIS_ADDR=127.0.0.1:17379 GOWORK=off go test -count=1 -race ./client/relay/ ./client/wire/ ./service/relay/... ./wiring/relay/` | 5 个包 ok（client/relay 10.3s、client/wire 2.1s、service/relay 11.4s、service/relay/token 3.5s、wiring/relay 4.4s） |
| `GOWORK=off go vet` 同上各包；`gofmt -l client service wiring` | 无输出 |
| `scripts/test-client-ts.sh`（node_modules 已在，没有执行 npm ci） | build 通过，60 条通过、0 条失败，见 [final-ts.txt](final-ts.txt) |
| `dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$PWD"`，跑完 `dotnet build-server shutdown` | `all 409 checks passed`，见 [final-cs.txt](final-cs.txt) |

## 尚未确认

- **deflate 与 zstd 的实际差距**：两者在真实状态上的耗时与大小没有测过。C4 A 按“先不带 zstd”定，README 里写的是“先量自己的状态再决定”。
- **Unity 下的表现**：C# 的改动只在 .NET 上跑过，没有在 Unity Editor 或 IL2CPP 下跑。
- **下游对 `Role().Epoch` 的依赖**：以前 Welcome 后 epoch 是 0，现在是房间的真实值。仓库里没找到依赖“进房 epoch 为 0”的代码，Go、TS 和 C# 都查过；但游戏侧怎么用 epoch 不在仓库里。
- **C# 晚进房 epoch 用例的独立红**：C# 的测试程序先跑假 relay 的用例，变异 cs1 在那里就已经红了（Welcome 之后的角色检查），所以 `welcome epoch for a late joiner and a rejoin` 这条真实对端用例自己变红的情形没有单独展示。它覆盖的承诺与 Go、TS 的同名用例相同，那两份的独立红都已展示。
- **页面真的切到后台时 `visibilitychange` 的上报**：只在 node 里用假的 `document` 验证过，没有在浏览器里实测。

## 要维护者定的点

> 维护者 2026-11-18 按推荐定（凡写了推荐的点都按推荐）。

1. **标识的解码顺序**：方案只说“未知标识明确报错”。这次实现成：先认自己配置的 compressor，再认内置的 0 / 1，所以配了 zstd 的客户端也能解开 deflate 的状态。
   - A（已实现，推荐）：内置两种总能解。
   - B：只认自己配置的 compressor。B 的代价是房间里混用缺省客户端时，换 host 会丢状态。
2. **从没连上时就调 `SetForeground`**：调用时没有连接，SDK 先记下状态，连上后自动发出。
   - A（已实现，推荐）：连上后自动发。
   - B：只在有连接时发。
   - Go 与 TS 在没有连接时返回 `ErrNotConnected` / false，C# 返回 false，三者都仍会记下状态。
3. **收到 `HostResumed` 时要不要也重发前台状态**：宽限期内房主回来，房主还是同一个客户端，它之前收到的上报仍在。这次没有重发。
   - A（推荐）：不重发。
   - B：重发。好处是房主刷新页面后能重新拿到全部成员的状态，因为 TS 的 `WebHost` 刷新后 presence 表是空的。代价是多一轮小帧。
4. **`docs/release/v1.54.0-NOTES.md`**：其中“方案已写、等维护者定（交接 C2～C6）”里的“Welcome 帧不带 epoch”“快照不用 zstd”两条现在已经定了，也已经做了。这份文档不在这次的修改范围内，留给协调者改。

## 给 CHANGELOG 与 KNOWN-LIMITS 的建议文字（本轮不改这两个文件）

CHANGELOG `[Unreleased]` 建议加：

> - **relay 协议：Welcome 带 epoch、前台上报、状态字节带压缩方式标识（维护者 2026-11-17，交接 C2、C4，选 C2 一 + 二的选项 1、C4 A + D）**：
>   - `Welcome` 固定 13 字节 `[角色][房主槽位 4B][epoch 8B]`。选人阶段进房的连接拿到房主槽位 0 和新一轮的 epoch。三份 SDK 在 Welcome 时（含重连）更新 `Role` 的 epoch。
>   - 新控制帧 `Presence = 85`（成员 → relay，`[1/0]`）和 `MemberPresence = 70`（relay → 当前房主，Seq 为成员槽位）。relay 只核格式，没有房主时丢弃。
>   - SDK 新增 `SetForeground`、`OnMemberPresence`，收到 HostChanged 与每次连上后自动重发。
>   - TS `WebHost` 按页面可见性上报；游戏没有显式声明候选顺序时，房主自动把前台成员排在前面。
>   - 快照与交接数据非空时是 `[压缩方式 1B][压缩后]`，0 表示不压缩，1 表示 deflate，2～255 归游戏。标识不认识时报明确的错误。
>   - 缺省仍用 deflate，SDK 不带 zstd，三份 README 给了接入示例。
>   - 金样 `relay-packets.json` 变为 31 条：`relay_welcome` 以及快照、交接各条变了，新增 4 条。
>   - **升级**：线上未部署，不兼容。relay 与三份 SDK 必须一起升级：旧客户端收到 13 字节的 Welcome 会按协议错误断开，新客户端也不接受旧 relay 的 5 字节 Welcome。旧客户端发的状态字节没有标识，新客户端会把首字节当标识读。
>   - 证据：`docs/maintenance/evidence/c2-c4-relay-protocol-20261117`。

KNOWN-LIMITS 建议：

- **第 574 行“Welcome 不带 epoch”**：删除，已解决。
- **第 577 行“快照压缩用 deflate，可替换，同一房间必须用同一种”**：改成下面这句。
  > **快照压缩缺省用 deflate，SDK 不带 zstd / LZ4**：要用就由游戏自带库实现压缩接口，示例见各 SDK README。状态字节带 1 字节压缩方式标识，内置的不压缩与 deflate 谁都解得开；游戏自带的压缩方式，房间里每个客户端都要配上，否则新房主拿到“不认识的压缩方式”错误、按没有状态处理。deflate 与 zstd 在真实状态上的差距没有测过。解压上限 16 MiB 是常量。
- **第 742 行“我在前台没有专门的控制帧”**：改成下面这句。
  > **relay 选人不看前台状态**（交接 C2 选项 3 不做）：前台优先只经房主声明的 `Successors` 生效，包括 TS `WebHost` 的自动声明。房主还没收到上报、或来不及声明就掉线时，兜底仍是槽位最小的在线成员。成员可以谎报前台状态，后果只是影响候选顺序。
