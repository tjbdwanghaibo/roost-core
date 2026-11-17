# 开房间 SDK 三件改动：WebGL 单线程开关（A1）、离开的“房间已不在”标志、deflate 复用（2026-11-18）

维护者 2026-11-18 定的三件事（KNOWN-LIMITS“开房间 SDK”一节的两条“待实施”，加上
[压缩测量](../perf-compressed-20261118/README.md)里“复用 Writer”的建议）。基线 main `616c5a90`（v1.54.0 已发布）。
背景：[C1 WebGL 实测与 A1 / A2 / B 三种做法](../c1-csharp-websocket-20261117/README.md)、[最后一人离开](../leave-last-member-20261117/README.md)。

## 结论

1. **WebGL A1 已实施，并在真实 WebGL 构建里通过**：`RelayClientOptions.PumpDriven`（缺省 false）。Unity 6000.7.0b7 WebGL 构建在浏览器里两次
   `SMOKE PASS`：建房、加入、收发、停 45s / 60s 不断线、离开。同一个构建关掉开关（`pump=0`）照旧卡在第一个 lobby 请求。
   .NET 上新加一组单线程用例：关着开关红，开着绿，变异 6 处都红。
2. **高层离开多返回“房间已不在”标志**：三份 SDK 形状一致。
   - Go：`Leave(ctx) (LeaveResult, error)`，`LeaveResult.RoomGone`；
   - C#：`LeaveAsync()` 返回 `RelayLeaveResult`，`.RoomGone`；
   - TS：`leave()` 返回 `{ roomGone }`；`WebHost.leave()` 同。

   lobby 答 `room_not_found` 时为 true，正常让出席位时为 false。三份用例都断言了标志，变异红。
3. **Go deflate 复用压缩器与解压器**：`sync.Pool` 加 `Reset`。压出的字节与每次新建的 `flate.Writer` 逐字节相同，金样
   `relay-packets.json` 没动。1KiB 快照：
   - 压缩每次分配 1.08 MB → 836 B，耗时 68 µs → 5.9 µs；
   - 全部 24 个子项 B/op 几何平均 −92%，ns/op 几何平均 −61%。

   C# 与 TS 没有同类大分配、也没有便宜的复用办法，**没改**，原因见 §3。

## 1. WebGL A1：`PumpDriven`

### 代码（`client/dotnet/Roost.Client.Relay`）

- `RelayClientOptions.PumpDriven`（缺省 false，其他平台行为不变）。打开时：
  - **读循环**：`Start` 不用 `Task.Run`，在调用 `ConnectAsync` / `RejoinAsync` 的线程上直接起。在锁外起：第一段可能同步跑到收帧、分发。
  - **等待**：所有 `await` 改成 `ConfigureAwait(pumpDriven)`，回到调用者的上下文，机械替换。`RelayCodec.ReadAsync(IRelayTransport, …)`
    多一个可选参数 `continueOnCapturedContext`，内部按包头拼帧的两次等待也跟着回上下文；缺省 false，与以前相同。
  - **心跳**：原来 `HeartbeatAsync` 一轮的判断抽成 `Tick(session, now)`，包括读超时、按角色的 Ping 间隔、定时快照。后台心跳任务与
    `Pump` 共用这个函数。`PumpDriven` 时不起心跳任务，`Pump` 开头对当前连接做一轮 `Tick`，不计入 `budget`。
  - **连接超时与重连退避**：改成 `Pump` 按时钟触发的计时（`After` / `DelayAsync`），不用 `CancelAfter` / `Task.Delay`。
    到期的计时投到调用者的 `SynchronizationContext` 再触发，不在 `Pump` 里面同步跑。原因：连接超时之后，游戏自己 `await ConnectAsync`
    的那段代码会同步跑下去，`Pump` 不可重入。
  - 不用 `SemaphoreSlim.WaitAsync(超时)`：只有后台心跳任务用它，`PumpDriven` 时不起这个任务。
- `LobbyClient` 的 HTTP 抽成接口：
  - `ILobbyHttp.SendAsync(method, url, bearer, jsonBody, ct)` 返回 `LobbyHttpResponse(Status, Body)`；
  - 缺省实现 `HttpClientLobbyHttp`，原来的构造函数不变，内部包一层；
  - 新构造函数 `LobbyClient(baseUrl, gameToken, ILobbyHttp, pumpDriven = false)`；
  - `pumpDriven` 时 `LobbyClient` 的等待也回上下文。
  - `RelayClient` 构造时检查：`PumpDriven` 而 lobby 不是 `pumpDriven`，抛 `ArgumentException`。否则重连时的 lobby 续行会排进线程池。
- 冒烟工程 `client/dotnet/unity-webgl-smoke`：
  - `UnityWebRequestHandler`（HttpMessageHandler）改成 `UnityWebRequestLobbyHttp`（实现 `ILobbyHttp`）：主线程上直接发，别的线程上投到主线程；
  - WebGL 缺省开 `PumpDriven`，页面参数 `pump=0` 关掉；
  - 编辑器用 `ROOST_SMOKE_PUMP_DRIVEN=1`，桌面播放器用 `-roostPumpDriven 1`；
  - 测试用 gt2 改经 `UnityWebRequest` 取，不再经 `HttpClient`；
  - 离开改成断言 `RoomGone`。
- 文档：
  - C# SDK README 的“Unity WebGL”一节重写：三样要换（调度、HTTP、传输）、示例代码、不 `Pump` 的后果；
  - `client/README.md` 的 C# 一行与 WebGL 一条；
  - 冒烟 README 的用法与实测结果。

### 单线程用例（`Roost.Client.Relay.Tests/SingleThread.cs`，先红后绿）

**环境 `StrictLoop`**：像 Unity WebGL 主线程。

- 一个专用线程，加一个只在这个线程上执行的 `SynchronizationContext`。
- 每一轮先执行投进来的续行，再轮询连接与 HTTP，再对每个客户端 `Pump`。

.NET 上没法让线程池与计时器真的不运行，改成“用了就记一笔违规”：

- 从别的线程往这个上下文投续行：`Task.Delay`、`CancelAfter` 的计时器，IO 线程完成的任务都会这样。
- SDK 在别的线程上调传输或 lobby HTTP：`Task.Run`，以及 `ConfigureAwait(false)` 排进线程池的续行。
  - 有同步上下文时，`ConfigureAwait(false)` 的续行不内联，排进线程池。这正是 WebGL 上不完成的原因。

**测试替身**：只在轮询里完成任务，像浏览器桥接与 `UnityWebRequest`。

- `PolledTcpTransport`：非阻塞看 socket。
- `PolledLobbyHttp`：请求由线程池上的 `HttpClient` 发，那一侧扮“浏览器”。

**对端**：另起一个，`ROOST_RELAYPEER_IDLE_MS=2000`（`peer_test.go` 新增的环境变量），relay 的 `idle_timeout` 改成 2s。

**两个用例**：

- `[single-thread] the detector catches the threaded scheduling`：
  - `PumpDriven` 关着建房、连上，必须记到违规（探测器自检）；
  - 另核对 `PumpDriven` 配非 `pumpDriven` 的 `LobbyClient` 抛 `ArgumentException`。
- `[single-thread] pump-driven: …`：`PumpDriven` 开着跑关键子集。每一步之后核对零违规：
  - 建房、加入、游戏 / 房间通道收发；
  - 停 2.5 个 idle（5s），两边不断线、`Rtt` 非 0；
  - 断网自动重连：退避由 `Pump` 完成，经 lobby 要一次令牌；
  - 成员停 `Pump`（像加载场景）：
    1. relay 判空闲断开，房主收到 `MemberLeft`；
    2. 停着时不派发任何通知；
    3. 恢复 `Pump` 后先收到 `Disconnected{IdleTimeout, Reconnecting}`，再重连回原槽位。
  - 转交房主：`YouAreHost` → `HostReady` → 两边 `HostChanged`；新房主再停 2.5s，靠 `Pump` 里的 600ms 心跳不被 host_idle 断开；
  - 连接超时：对端接受连接却不发 Welcome，300ms 后由 `Pump` 取消，得到 `TimeoutException`。之后的续行能调 `Pump`，说明不在 `Pump` 里面；
  - 离开：旧房主 `RoomGone` 为 false，最后一人不抛。

**红**：`ROOST_RELAY_SINGLE_THREAD_OFF=1` 时第二个用例关着开关跑。关着开关就是修前的调度：`Task.Run` 读循环、后台心跳、
`ConfigureAwait(false)`、`Task.Delay`、`CancelAfter`，这部分代码没改。修前的 SDK 没有 `PumpDriven` / `ILobbyHttp`，用例编不过，
所以用“关开关”代替。命令与输出：

```
ROOST_REDIS_TEST_ADDR=127.0.0.1:17379 ROOST_RELAY_SINGLE_THREAD_OFF=1 ROOST_RELAY_TESTS_ONLY="[single-thread] pump-driven" \
  dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$PWD"
FAILED: System.Exception: Check failed: create and connect: everything ran on the loop thread; violations: a continuation posted
to the loop on thread 9 (thread pool) | transport ReceiveAsync on thread 9 (thread pool) | a continuation posted to the loop on
thread 9 (thread pool) | transport ReceiveAsync on thread 9 (thread pool)
```

最终代码上又跑了一次，同样红在 `create and connect`，线程号不同。

**绿**：同一命令去掉 `ROOST_RELAY_SINGLE_THREAD_OFF`。

- 只跑单线程组：连跑 3 次都是两项 ok。
- 全量：rc 0，`all 784 checks passed`，最后一次在全部改动之后。
  - 改动前是 722 项（见最后一人离开的证据）；
  - 新增的是单线程组与 `RoomGone` 断言。

### 变异（每次撤一处，只跑单线程组，跑完从备份恢复并逐字节核对）

| 变异 | 结果 |
|---|---|
| M1 读循环里 `RelayCodec.ReadAsync` 的 `continueOnCapturedContext` 传 false（内部等待不回上下文） | 红：`join and frames: … transport ReceiveAsync on thread 9 (thread pool) \| a continuation posted to the loop on thread 9 (thread pool) …` |
| M2 `DelayAsync` 在 `PumpDriven` 下仍用 `Task.Delay`（续行回上下文） | 红：`reconnect: … a continuation posted to the loop on thread 8 (thread pool)`（最终代码上重跑同样红） |
| M3 连接超时在 `PumpDriven` 下仍用 `CancelAfter` | 红：`connect timeout: … transport Dispose on thread 12 (thread pool) \| unregister a polled item … \| a continuation posted to the loop …`（最终代码上重跑同样红） |
| M4 `Pump` 里不做心跳 `Tick` | 红：`Check failed: no disconnect while holding past the relay idle timeout` |
| M5 `LobbyClient` 等 HTTP 时 `ConfigureAwait(false)` | 红：`create and connect: … a continuation posted to the loop on thread 9 (thread pool)` |
| M6 到期计时就地在 `Pump` 里触发（不投到上下文） | 先跑没红（用例当时没覆盖“续行跑在 Pump 里”）；补上“超时之后的续行里调 `Pump`”一句后红：`System.InvalidOperationException: Recursive Pump is not supported.` |

### WebGL 实测（Unity 6000.7.0b7，浏览器）

**构建**：

- `client/dotnet/unity-webgl-smoke/prepare.sh` 把工程拷到 scratchpad；
- Unity 批处理 `-buildTarget WebGL -executeMethod RelaySmokeEditor.BuildWebGL`，结果 `Build Finished, Result: Success.`。
  第一次因为冒烟脚本里一处变量先用后声明而编译失败，改正后重建。

**页面**：

- `serve.mjs <Build/WebGL> 18791`：测试对端 relay + lobby，房间目录用 `~/.roost-it` 的 Redis。端口 18791，没有碰 7000 / 9100～9103。
- 用 Claude 桌面应用内置浏览器面板打开。面板处于隐藏状态，页面按后台页运行。

**结果**（摘要见 [webgl-smoke.txt](webgl-smoke.txt)）：

| 次 | 页面 | 结果 |
|---|---|---|
| 1 | `?hold=45`（`PumpDriven` 开） | `SMOKE PASS`：建房、加入、5 + 1 + 1 帧，`held 45s: still connected, rtt host 8.5ms member 8.7ms`，`left: ok (member room gone False, last one room gone True)`，没有任何断线 |
| 2 | `?hold=45&pump=0`（关，修前调度） | `SMOKE FAIL stuck at step 'end to end: lobby (LobbyClient + UnityWebRequestLobbyHttp)' for 65s` |
| 3 | `?hold=60`（开） | `SMOKE PASS`，`held 60s`，rtt 约 1000ms（见下），没有断线 |

- 三次的探针都与 C1 相同：6 个原语不完成，只有回到 Unity 上下文的 `await` 完成。平台限制没变，`PumpDriven` 不依赖这些原语。
- 第 3 次 rtt 约 1s，推断是隐藏面板里的后台页把帧率限到约 1 帧/秒：Pong 要等下一帧的 `PollAll` 才读到。即使这样，房主的心跳仍在
  `host_idle_timeout` 2s 之内，没有断线。帧率被限的原因**尚未确认**。
- 收尾：对端与 serve.mjs（PID 37529 / 37535）已停；工程与构建在 scratchpad，没有进仓库。

## 2. 高层离开的 `RoomGone`

**代码**：

- Go `client/relay/client.go`：新类型 `LeaveResult{RoomGone bool}`，`Leave` 返回 `(LeaveResult, error)`；
- C# `RelayClient.cs`：新类型 `RelayLeaveResult`，`LeaveAsync` 返回 `Task<RelayLeaveResult>`；
- TS `client.ts`：新类型 `LeaveResult { roomGone }`，在 `index.ts` 导出；`RelayClient.leave()` 与 `WebHost.leave()` 都返回它。

**文档**：三份 README、`client/README.md` 的 lobby 一条、LOBBY.md §2.1“离开”。

**用例**：三份同形。

- 两人房里成员先离开：标志为 false（新增断言）。
- 房间 b（定下先后：最后一人先只在 relay 上发 Leave，等 lobby 确认房间已关，再高层离开）：标志为 true。
- 房间 a（自然先后）：
  - Go / C# 两种答复都可能，只要求不报错；
  - TS 的 `leave()` 只经 lobby，答复时房间还在，所以断言 false。
- C# 的 `RoomLifecycle` 与冒烟里的成员离开也断言 false。

**先红后绿**：签名是新的，修前编不过。用“撤掉置标志的一行”代替修前，也就是变异：

| SDK | 撤掉的一行 | 结果 |
|---|---|---|
| Go | `result.RoomGone, err = true, nil` → `false, nil` | 红：`client_test.go:378: leave a room that already closed: {RoomGone:false} <nil> (want RoomGone)` |
| C# | `return new RelayLeaveResult(true)` → `(false)` | 红：`Check failed: leaving a room that already closed reports RoomGone` |
| TS | `return { roomGone: true }` → `false` | 红：`tests 61 / pass 60 / fail 1`，红在 `the last member's leave succeeds after the room closes` |

恢复后全绿：Go `-race -count=20` 也过，见下“验证”。

## 3. deflate 复用

### Go（`client/relay/host.go`）

- 压缩：`sync.Pool` 里放 `*flate.Writer`，写进一个转发器 `deflateSink`；每次 `Reset` 到这一次的输出缓冲区。
- 解压：池子里放 `flate.NewReader`，源是一个 `*bytes.Reader`。它实现 `io.ByteReader`，flate 不再包一层 bufio；每次 `src.Reset(data)`，
  再对解压器做 `Resetter.Reset`。
- 放回池子之前断开对调用方缓冲区的引用，池子里不留住快照字节。

**用例 `TestDeflateReuse`**（`golden_test.go`）：

- 7 种大小（0 字节到 256KiB）的状态轮 3 遍。复用的压缩器压出的字节必须与每次新建的 `flate.Writer` 逐字节相同；
- 解坏数据、超过上限之后，下一份照常解；
- 8 个 goroutine 并发往返（`-race`）；
- 2KiB 状态压 200 次，平均分配不到 512 KiB。新建一个 Writer 约 1 MB。`-race` 下 `sync.Pool` 随机丢四分之一的 Put，平均仍在这条线以下。

**红绿与变异**：

- 红（修前的 `host.go`，从 `git show HEAD:client/relay/host.go` 换回来跑，用例不变）：
  `golden_test.go:468: compressing 2048 bytes allocates 1075973 bytes per call on average; the compressor is not reused`（`-race` 下同样红，1076052）。
- 绿：`TestDeflateReuse`、`TestCompressors`、`TestStateCarriesTheCompressionMarker`、两条金样用例都过；`-race -count=3` 过。
- 变异：
  - 池子里的 Writer 用 `flate.BestSpeed`：红，`round 0 state 1 (300 bytes): reused compressor wrote 71 bytes, a fresh flate.Writer 70`。
    逐字节相同的核对抓得到级别或状态的偏差。
  - 解压时不 `src.Reset(data)`：红，`TestCompressors … unexpected EOF`。
- 线上字节：`client/spec/relay-packets.json` 没动（`git status` 无改动），`TestSDKFramesMatchRelayGolden` / `TestSDKHostFramesMatchRelayGolden` 过。

**测量**：同机交替，before / after 各 3 轮、每轮 `-count 2`，各 6 个样本。

- 两个测试二进制：`go test -c`，before 用修前的 `host.go`。
- 原始数据：`deflate-before.txt`、`deflate-after.txt`；benchstat：`deflate-benchstat.txt`；负载：`deflate-host.txt`。
- 不是空闲机：负载 6.6～11，swap 用了 7.6 GiB。

| 子项 | ns/op 前 → 后 | B/op 前 → 后 |
|---|---|---|
| json 1KiB compress | 68.0 µs → 5.9 µs | 1.08 MB → 836 B |
| json 1KiB decompress | 7.2 µs → 3.7 µs | 41.7 KiB → 2.1 KiB |
| json 16KiB compress | 139 µs → 42 µs | 1058 KiB → 7.9 KiB |
| json 256KiB compress | 1.14 ms → 0.80 ms | 1178 KiB → 129 KiB |
| json 256KiB decompress | 0.63 ms → 0.57 ms | 655 KiB → 616 KiB |
| packed 256KiB compress | 2.03 ms → 1.54 ms | 1306 KiB → 257 KiB |
| random 256KiB compress | 119 µs → 51 µs | 1555 KiB → 511 KiB |
| 几何平均（24 个子项） | −60.8% | −91.9%（allocs/op −54%） |

- 压缩率逐项相同：`ratio` 前后一致。
- 大状态解压剩下的分配是结果本身，`io.ReadAll` 的增长缓冲。
- 这台机器不空闲，ns/op 的离散度 5%～47%，只说明方向；B/op 是确定的。

### C# 与 TS：没改

**C#**：`RelayCompressors.Deflate` 用 `DeflateStream`，zlib 状态在原生内存里，托管堆上没有 1 MB 那一块。用 scratchpad 里的小程序量了托管分配
（`GC.GetAllocatedBytesForCurrentThread`，.NET 10 Release，json 状态，每档 300 次）：

| json 状态 | 压缩：托管分配 / 耗时 | 解压：托管分配 / 耗时 |
|---|---|---|
| 1KiB | 872 B，7.1 µs | 19 KB，4.4 µs |
| 16KiB | 6.8 KB，69 µs | 53 KB，25 µs |
| 256KiB | 169 KB，2.2 ms | 833 KB，0.38 ms |

- 解压多出来的是：每次 16KiB 的读缓冲、输入的 `ToArray` 副本、`MemoryStream` 的增长与 `ToArray`。
- 解压只在当上房主时发生一次，压缩每秒至多一次，不值得为它改。
- `netstandard2.1` 的 `DeflateStream` 没有 Reset，原生 zlib 状态（deflate 约 256 KB）每次新建，没有便宜的复用办法。

**TS**：

- 浏览器用 `CompressionStream('deflate-raw')`，按规范是一次性的流，不能复用；
- node 用 `zlib.deflateRawSync` / `inflateRawSync`，也没有复用状态的接口。
- 没测 TS 的分配。

## 验证（全部在本 worktree，2026-11-18）

**Go**：

- `GOWORK=off go build ./client/... ./service/lobby/... ./wiring/lobby/...`：通过；
- `GOWORK=off go vet ./client/relay/ ./service/lobby/ ./wiring/lobby/`：通过；
- `ROOST_REDIS_TEST_ADDR=127.0.0.1:17379 GOWORK=off go test -race -count=1 ./client/relay/ ./service/lobby/ ./wiring/lobby/`：三包 ok，
  用时 client/relay 10.7s、service/lobby 8.2s、wiring/lobby 1.7s；
- `-race -count=20 -run 'TestLeaveByTheLastMember|TestDeflateReuse' ./client/relay/`：ok。

**C#**：`ROOST_REDIS_TEST_ADDR=127.0.0.1:17379 dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- "$PWD"`：rc 0，`all 784 checks passed`。
包括 `[single-thread]` 两项与 `[tcp]` / `[ws]` 各 15 项。跑完 `dotnet build-server shutdown`，删掉各工程的 bin/obj。

**TS**：`scripts/test-client-ts.sh`：tests 61 / pass 61 / fail 0。

- `node_modules` 软链到主检出，跑完已删；
- 签名改动没有加新用例，在原用例上加断言。

**根包**：`GOWORK=off go test -count=1 -run 'Link|Anchor|Doc' .`：ok（2.6s）。

## 尚未确认

- 内置浏览器面板一直是隐藏状态，页面按后台页运行；第 3 次 rtt 约 1s 推断是帧率被限。面板可见时没有重跑。
  Safari / Firefox / 移动浏览器没有测，切后台没有测：内置面板测不了真实后台，KNOWN-LIMITS 已有一条。
- WebGL 上“不 Pump 被断开、恢复后重连回来”只在 .NET 单线程用例里验证（relay idle 2s）。WebGL 里没有专门停 Pump 的场景；
  房主 `host_idle_timeout` 2s 在 WebGL 上的实际表现（加载场景卡帧）没有实测。
- 编辑器与桌面播放器在这一轮没有重跑冒烟。`PumpDriven` 关着时代码路径与修前相同，由 .NET 全量用例覆盖。
- `PumpDriven` 下 SDK 自带的 `TcpRelayTransport` / `WebSocketRelayTransport` 仍可用，IO 完成在 IO 线程、续行回上下文，但没有专门测；
  WebGL 上本来就用不了它们。
- 没有跑全量 `go test ./...`、pretag（按批次规则由协调者统一跑）。

## 要维护者定的点

无新增产品选择。下面几处是在维护者定的形状里自定的，如要改请指出：

- 标志的形状：Go 返回结构体 `LeaveResult{RoomGone}`，没有用 `(gone bool, err error)`。这样三份都叫 RoomGone / roomGone，以后加字段不再改签名。
- C# `LobbyClient` 的 `pumpDriven` 是构造参数，与 `RelayClientOptions.PumpDriven` 要一起开，不一致时 `RelayClient` 构造抛异常。
  另一种做法是 `RelayClient` 自动沿用 lobby 的设置，没有采用：lobby 在 `RelayClient` 之前就要用（建房），必须在构造 lobby 时决定。
- 到期的计时投到 `SynchronizationContext` 再触发：在 Unity 上，连接超时与重连退避比到期时刻晚一帧左右。

## 建议写进 CHANGELOG 的文字（[Unreleased]，协调者统一写）

- **C# 开房间 SDK 支持 Unity WebGL（维护者 2026-11-18 定 A1）**：
  - `RelayClientOptions.PumpDriven`（缺省 false，其他平台不变）打开时不用线程池与计时器：读循环在主线程上起，所有等待回到调用者的上下文；
    心跳、读超时、连接超时、重连退避由 `Pump` 按时钟检查。
  - `LobbyClient` 的 HTTP 抽成 `ILobbyHttp`：缺省 `HttpClientLobbyHttp`，WebGL 用 `UnityWebRequest` 实现，参考
    `unity-webgl-smoke/Assets/RelaySmoke/UnityWebRequestLobbyHttp.cs`，替换原来的 `UnityWebRequestHandler`。新构造函数
    `LobbyClient(url, token, ILobbyHttp, pumpDriven)`；`PumpDriven` 的 `RelayClient` 要求 lobby 也是 `pumpDriven`。
  - `RelayCodec.ReadAsync(IRelayTransport, …)` 多一个可选参数 `continueOnCapturedContext`。
  - 实测：Unity 6000.7.0b7 WebGL 构建在浏览器里建房、加入、收发、停 45s / 60s、离开，两次通过；同一构建关掉开关仍卡在第一个 lobby 请求。
    .NET 新增单线程用例组：关开关红、开开关绿，变异 6 处红。
  - 不 `Pump` 时：成员超过 relay `idle_timeout`（30s）、房主超过 `host_idle_timeout`（2s）被断开，恢复 `Pump` 后按 `ReconnectAttempts` 重连。
  - 测试对端新增 `ROOST_RELAYPEER_IDLE_MS`。
  - 证据：docs/maintenance/evidence/sdk-webgl-a1-20261118/README.md
- **开房间 SDK 的高层离开多返回“房间已不在”标志（维护者 2026-11-18）**：签名改动，未上线不兼容。
  - Go `Client.Leave` 返回 `(LeaveResult, error)`；
  - C# `LeaveAsync` 返回 `RelayLeaveResult`；
  - TS `leave()` / `WebHost.leave()` 返回 `{ roomGone }`。

  lobby 答 `room_not_found` 时 `RoomGone` 为 true，正常离开为 false。
- **Go 开房间 SDK 的 deflate 复用压缩器与解压器**（`sync.Pool` + `Reset`）：
  - 压出的字节不变；
  - 1KiB 快照压缩每次分配 1.08 MB → 836 B、68 µs → 5.9 µs；24 个子项 B/op 几何平均 −92%。

  C# / TS 没有同类大分配，不改。

## 建议写进 KNOWN-LIMITS 的文字（协调者统一写）

“开房间 SDK”一节：

- 删掉“高层离开分不出‘房间早就不在’与‘正常离开’”一条（已实施）。
- 把“Unity WebGL 上 C# 开房间 SDK 跑不起来（…待实施）”换成：
  - **Unity WebGL 上 C# 开房间 SDK 要开 `PumpDriven`**（维护者 2026-11-18 定 A1，已实施）。WebGL 上线程池、计时器与
    `ConfigureAwait(false)` 之后的续行不运行（2026-11-17 实测）。
  - 打开 `PumpDriven` 后 SDK 只靠 `Pump`：游戏不调 `Pump`（加载场景卡住主线程、切后台浏览器停了循环）就不发心跳、不判超时、不重连。
    成员超过 relay `idle_timeout`（30s）、房主超过 `host_idle_timeout`（2s）会被断开：房主进宽限期，宽限期 5s 内重连回来仍是房主，
    否则换 host；恢复 `Pump` 后按 `ReconnectAttempts` 重连。
  - 2026-11-18 在 Unity 6000.7.0b7 WebGL 构建、Claude 内置浏览器（隐藏面板）里两次通过。
  - 没测：Safari / Firefox / 移动浏览器、真实切后台、WebGL 里长时间卡帧的房主。
  - 证据：sdk-webgl-a1-20261118。
