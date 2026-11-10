# Roost 客户端协议与 C# SDK

Roost 的自定义协议负责帧、路由编号与请求关联，业务载荷由包头选择 PB / Sync / Lockstep。本批提供公共 Go wire、纯 C# `netstandard2.1` TCP 运行库和 Unity 主线程适配。Lockstep 已接线：输入/Hash/Catchup命令、既有C7广播与C#有序消费、C9 v2恢复失败通知与CA 01快照恢复。

## 标准业务入口

校时、表现插值、输入预测与阶段诊断见[网络体验接入](../docs/framework/NETWORK-EXPERIENCE.md)。这些能力按业务显式启用，EntitySync 仍立即应用权威增量。

```csharp
var clock = new NetworkClock();
// client 是已连接的 GameClient；在创建它的线程调用，并持续 Pump。
client.ProbeClock(sample => clock.Observe(sample), error => Console.WriteLine(error));
// 换服/重连先 clock.Reset()。只在 clock.Ready 后使用 clock.Now(ClockSample.LocalNow)。
client.Entities.Trace = new ClientTrace(1024);
```

新业务推荐 `GameClient`，已有低层消费者可以继续使用 `TcpSession`。前者复用同一 RS 编解码和 TCP 会话，没有另起网络协议。

- `Connection`：票据鉴权、默认 15 秒心跳、请求预算、断线状态；自动重连默认关闭，由业务通过 `ConnectionOptions.AutoReconnect` 启用。有界指数退避、每次重新取票、每次成功/失败都有 `ReconnectResult`，不会自动重发业务请求。
- C# `MaxPendingRequests` 限制普通请求、通知及显式校时探测；监控心跳另有最多一个内部槽位，业务不能借用。心跳仍共享发送锁与线上序列，发送阻塞或真正无应答受 `HeartbeatTimeout` 限制。

RV-20261101-04：v1.37.0 在业务占满容量时，心跳会报 `Pending request capacity exhausted`，监控器关闭健康连接，使业务收到 `ObjectDisposedException`。`HeartbeatCapacityTests.cs` 以真实回环 TCP 保持业务在途，等待并应答心跳才返回业务结果；检查额外业务仍被拒绝、容量释放后能再次请求、后续心跳无应答会断线。修前在 `await first` 复现 `ObjectDisposedException`，修后通过。此验证不代表 Unity Player 或 WAN 压测。

2026-11-01 修后验证：`dotnet run --no-restore --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json` 全部通过；`GOWORK=off go test -count=1 -timeout 10m -run '^TestClientSDKAgainstGeneratedPlayerTCP$' ./codegen/internal/roost` 的真实生成工程 TCP 链路也通过。
- `GameClient`：在创建线程调用 `Subscribe` / `Request` / `Pump`；PB 解析、推送和完成回调都在 Pump 中执行。取消/超时/网络/解析错误走失败回调；业务错误码仍由应答 PB 表示。未消费完成回调也占容量；满时同步拒绝新请求。
- `EntityViews`：Full 清场、Delta/create/update/remove、对象代际、subject 版本、命名空间和 profile 路由。缺口或解析/应用失败关闭连接，恢复经正常鉴权流程，不跳过错误增量。默认容量 10000 对象，仍需按项目实际负载验证。
- `RoostClientDriver`：Unity 主线程 Attach 后每帧 Pump，销毁时 Dispose。它与低层 `RoostConnection` 是不同使用层级，同一连接只选一个消费者。

`Roost.Client` 本体无第三方依赖；可选 `Roost.Client.Codecs` 仅依赖 Google.Protobuf 3.31.1，提供生成 PB 类型的便利重载。默认 DAO 同步已在 main 改成 **PB 字段补丁**，持久化仍是 BSON；见 [DAO 同步协议](../docs/framework/DAO-SYNC-PROTOBUF.md)。v1.34.2 的旧同步客户端须重新生成，不能混用。

```csharp
using Roost.Client;
using Roost.Client.Codecs;
using Roost.Dao.Sync; // 由 db/dao_sync.proto 生成

var client = new GameClient(new ConnectionOptions {
    Host = host, Port = port, AutoReconnect = false
});
client.Error += ShowNetworkError;
client.Reconnected += result => ShowReconnectResult(result);
client.StateChanged += state => ShowConnectionState(state);
client.Entities.RegisterDao("player", SyncPlayerDao.Parser,
    (patch, previous) => patch.Apply(previous));
client.EntitiesChanged += frame => RefreshVisibleEntities(client.Entities.Current);
await client.Connection.ConnectAsync(GetFreshTicketAsync);
// 引擎主线程每帧：client.Pump(); Unity 也可交给 RoostClientDriver.Attach(client)。
```

业务请求用本项目生成的类型与编号，例如 `client.Request(requestId, responseId, request, Reply.Parser, reply => ..., error => ...)`；推送用 `client.Subscribe(pushId, Push.Parser, push => ...)`，返回的 IDisposable 在页面退出时释放。首次连接失败由 ConnectAsync 的异常交给业务。方法组/泛型的无依赖重载也可直接传 `Reply.Parser.ParseFrom`；不要复制服务端 PB 定义或手写第二份类型。

### EntitySync 变化回调

`EntityChanged` 在 `Pump` 线程逐个通知同步组件视图。整帧解码、版本校验和视图更新全部成功后才通知；此时 `Entities.Current` 已是整帧应用后的状态。重复或过期帧不通知，失败帧不发部分通知。

```csharp
client.EntityChanged += change => {
    var identity = change.Identity;
    switch (change.Kind) {
        case EntityChangeKind.Full:
            // 初次出现或重新提供完整视图。Current.Value 是解析后的 PB 状态。
            ShowOrReplace(identity.Update.SubjectId, change.Current!.Value);
            break;
        case EntityChangeKind.Delta:
            // Previous/Current 是前后完整状态；字段补丁在 Current.Update.Payload。
            Refresh(identity.Update.SubjectId, change.Previous!.Value, change.Current!.Value);
            break;
        case EntityChangeKind.Remove:
            Hide(identity.Update.SubjectId, identity.ComponentType);
            break;
    }
};
client.EntitiesReset += () => ClearVisibleEntities();
```

示例中的 ShowOrReplace / Refresh / Hide / ClearVisibleEntities 是业务 UI 方法。身份还包含 Namespace、Profile、ObjectId、Generation 与 ComponentType，多种视图不得只按 SubjectId 混在一起。

- `Full` / `Delta` 指 **SubjectUpdate** 的载荷类型，不是外层 frame 类型：外层 Delta 中也可能新建对象并携带 Full。
- `Remove` 的 Current 为 null，Previous 保留旧视图；RemovalReason 为 Server（对象/组件移除）或 SnapshotReplacement（恢复全量不再包含旧视图/代际）。它表示客户端不再可见，不等同于服务器销毁业务实体。
- `EntitiesReset` 表示连接切换、断线或同步错误清场，仅在有视图被清除时通知；不伪造成服务器 Remove。Dispose 静默清理。
- `EntitiesChanged` 保留整帧通知，在逐视图通知之后执行；空帧也能收到帧通知。业务按需要选一个粒度，避免重复刷新。
- 业务通知抛错交给 Error，不破坏已确认的同步基线，也不阻止其他订阅者；Error 自身抛错被隔离。协议/解码错误仍断开流并清场。
- 回调只读前后视图，不直接修改 PB 对象或在通知期间调用 Entities.Reset/Receive；连接生命周期由 GameClient 管理。

完整可编译用法在 `client/dotnet/Roost.Client.Tests/GameClientTests.cs`，真实 TCP 对端收发官方生成的 `StringValue` PB；DAO 类型与 Apply 方法在跨语言测试中实际编译和执行。纯 C# 网络适配的桥接源文件由 .NET 测试编译。另有 native 网络路径已实际验证 Unity Editor、Mono/IL2CPP Player 与解码容量，见 [Unity 接入](unity/README.md)；两条网络路径不共用容量结论。

## 包头：RS v2

固定16字节，整数大端；`payload_length`仅包含载荷，不包含头。

| 偏移 | 大小 | 字段 | 语义 |
| --- | ---: | --- | --- |
| 0 | 2 | magic | ASCII `RS` |
| 2 | 1 | version | `2`，旧版本直接拒绝 |
| 3 | 1 | flags | bit0=服务器推送，bits1..2=载荷类型 |
| 4 | 4 | message_id | 业务路由编号，0仅用于鉴权控制 |
| 8 | 4 | sequence | 请求关联；请求与服务器推送各有独立序列 |
| 12 | 4 | payload_length | 在分配内存前检查配置上限 |
| 16 | N | payload | 业务载荷或鉴权票据 |

| 类型字段 `(flags >> 1) & 3` | 类型 | 当前行为 |
| ---: | --- | --- |
| 0 | PB | 普通请求/响应、PB推送 |
| 1 | Sync | 原始Sync frame推送，直接解码，无PB bytes外壳 |
| 2 | Lockstep | 显式注册的输入/Hash/Catchup Notify；广播/历史页推送 |
| 3 | 保留 | 拒绝 |

bits3..7必须为0。PB请求/响应flags=0，PB推送=1，Sync推送=3；Lockstep上行Notify=4，下行push=5。类型选择解码器，message_id选择业务路由，两者分别判断，不能只看seq或编号猜类型。ID=0的控制帧flags必须为0：首包payload是票据原始UTF-8字节（或下面的连接代号信封），成功ACK使用相同seq且payload为空，它不经过PB解码。

### 鉴权首包的连接代号

首包payload可以写成带连接代号的信封，让服务端在同一SessionID的两条连接落在不同Gate时认出迟到完成的旧握手（客户端放弃一次握手、立即连到另一个Gate重连的情形；规则见[GATEWAY §3](../docs/framework/GATEWAY.md#3-绑定与请求结果)）：

| 偏移 | 大小 | 字段 | 语义 |
| --- | ---: | --- | --- |
| 0 | 1 | marker | `0x00` |
| 1 | 1 | tag | ASCII `G` |
| 2 | 8 | generation | 连接代号，uint64大端，非0 |
| 10 | N | ticket | 票据原文，非空 |

- 不带代号时payload就是票据原文，行为与以前相同（跨Gate时后完成的握手胜出）。票据原文因此不能以`0x00`开头；以`0x00`开头却不是完整信封（tag不是`G`、代号为0、没有票据）的首包按坏帧拒绝。服务端在鉴权前剥掉信封，业务authenticator只看到票据原文。
- **每次连接（包括重连）取一个新代号**，同一客户端越晚发起的连接代号越大，而且**客户端重启后仍要更大**：推荐`max(上一个 + 1, 当前UTC Unix纳秒)`，上一个只存在内存里。进程内严格递增、不受时钟回拨影响；重启后从当时的时钟起算，自然大于旧进程发过的代号。不要用从1开始的纯内存计数器：重启后新连接会输给旧连接。精度只有毫秒的平台用`Unix毫秒 × 1000000`。
- 时钟在重启前后被往回拨时，新连接的代号可能偏小；后果只是与旧进程仍在途的握手重叠时被拒一次，重连即恢复。服务端只在重叠的握手之间比较代号，一个异常大的代号挡不住之后的正常重连。
- Go实现：`wire.EncodeAuth(ticket, generation)`；robot的`transport.AuthPacket(token)`每次调用自动取`transport.NextConnectionGeneration()`。
- C#运行库（`client/dotnet`，2026-11-06）：`TcpSession.ConnectAsync`每次连接取`AuthPayload.NextConnectionGeneration()`写成信封，`Connection` / `GameClient`的首次连接与自动重连、Unity托管适配`RoostConnection` / `RoostClientDriver`都经它，调用方API不变、没有新配置项。代号在发起连接时取，进程内所有连接共用一个递增序列。`AuthPayload.Encode` / `Decode`公开，供自建工具核对字节。票据上限由8192改为8182字节：服务端缺省的首包上限`MaxHandshakeBytes`是8 KiB，其中10字节是信封。以`0x00`开头的票据在连接前抛`ArgumentException`。
- C++运行库（`client/cpp`，2026-11-06）：`roost_connect`每次连接取进程内新代号写成信封（全部`roost_client`共用一个递增序列）。C++ `roost::client::connect`、`game_client`重连、Unity的`NativeClient` / `NativeGameClient` / `NativeReconnect`，以及经C ABI的Unreal模块都经它。C ABI与`roost_abi_version()==2`不变，托管绑定不用改，替换native库即可。票据为空或以`0x00`开头时原样发出，由服务端拒绝（`CLOSED`），与以前的失败方式相同。这里的连接代号与事件里按`roost_client`计数的连接`generation`不是一回事。
- Unity：正式网络核心是C++（`RoostNativeDriver` + `NativeGameClient`），托管适配走C# `TcpSession`，两条路线都已带代号。重新执行`scripts/client/build-unity.py`打包，新的native插件与`Roost.Client.dll`才生效。Unreal使用同一C++库。
- 跨语言金样：`client/spec/packets.json`的`auth_generation`（票据`ticket`，代号最高位为1，检验按无符号64位大端写出）。Go（`TestClientProtocolGolden*`）、C#（`Roost.Client.Tests`）和C++（`native_client`）都用同一票据与代号编码，并与金样逐字节比对。
- 混用：服务端对没带代号的一方仍按“后完成者胜出”处理，只要同一会话的两次握手有一次没带代号就退回这条规则。不是这三套运行库的自建客户端要自己按上面的布局与生成规则实现；自建接入服务若直接把首包载荷当票据读，需要先拆信封（Go用`wire.DecodeAuth`，C#用`AuthPayload.Decode`）。

正式生成的player TCP要求客户端首包和后续包seq非零、单连接按线上发送顺序递增（允许uint32回绕）。C#在发送锁内分配seq；推送seq属于服务器独立空间，即使与请求相同也不能匹配待应答请求。正式TCP接收PB业务请求和显式注册的Lockstep Notify，客户端Sync请求在Dispatch前拒绝。类型不符在decoder/handler前拒绝，各类共用同一个message_id注册表。原始Sync推送复用既有frame v1 / SubjectUpdate v2 / 应用packer；内部版本未随外层改变。

Go封包实现是`client/wire`，robot TCP/WebSocket与正式生成的player TCP/探针/loadtest共用它。Go客户端解码Entity Sync自身视图（`access.player.self_sync`）的写法见`wiring/selfsync/example_client_test.go`：每连接一个`frame.Receiver`，`DecodeSubjectUpdate`取出载荷交给DAO的`ApplySync`，随测试运行；C#本轮没有对应样例。现有`sync/nettransport`的UDP/KCP/QUIC channel管理不在本次改造范围；没有据此宣称所有传输或公网TLS已统一。

## 服务端与代码生成

已有工程升级core后执行`roost project sync`重生成托管player TCP/探针，demo业务所有文件需同步本次scene/loadtest修改。旧12字节robot帧和RS v1不兼容，全部客户端一起升级；不能只改版本字节。`robot/transport.EncodePackets`现在返回`([]byte, error)`，调用方必须处理非法flags/超限错误。

PB仍走`PushPlayer` / `PushSession`及生成ProtocolRegistry encoder。Sync使用显式`PushSyncPlayer(ctx, playerID, msgID, frameBytes)`或`PushSyncSession`，不走PB encoder。demo的`sceneLane`已改成raw Sync；`EntitySyncPush`结构暂留作协议编号/生成schema产物，正式scene路径不再序列化它。业务自己的PB推送不自动变成Sync。

可选C#消息编号由同一Go定义生成，路径显式指定，现有默认产物不变。在业务工程根目录执行（将`<core-version>`换成选定版本）：

```sh
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/protocol@<core-version> \
  -def ./protocol/def -csharp ./client/generated/MessageIds.cs
protoc -I ./protocol/proto --csharp_out=./client/generated ./protocol/proto/protocol.proto
```

`-csharp`输出`Roost.Generated.MessageIds`，请求、应答编号与Go绑定一致（当前同ID），推送另列。PB C#类型由这份proto及Google.Protobuf运行库生成，protoc/runtime版本需在业务工程锁定；本库自身不依赖Google.Protobuf。生成器拒绝覆盖手写C#文件（包括`-force`），空定义仅退役带生成标记的文件。普通`roost generate`没有自动增加C#输出；业务构建需把上述导出命令加入自己的流程。

## C# TCP使用

构建或打包纯运行库：

```sh
dotnet build client/dotnet/Roost.Client/Roost.Client.csproj -c Release
dotnet pack client/dotnet/Roost.Client/Roost.Client.csproj -c Release
```

运行库只依赖标准库，使用显式票据鉴权、请求超时与有界待请求/推送队列。下面的PB字节对应`int64 field1=42`，业务应用用生成PB类型的`ToByteArray()` / `Parser.ParseFrom(...)`替换：

```csharp
using var session = await Roost.Client.TcpSession.ConnectAsync(
    "127.0.0.1", 7000, shortLivedTicket, TimeSpan.FromSeconds(5));
var response = await session.RequestAsync(10001, 10001,
    new byte[] { 0x08, 0x2a }, TimeSpan.FromSeconds(5));

// 与连接同生命周期保存，不能每个包都new；主线程每帧有界消费。
var syncReceiver = new Roost.Client.SyncReceiver();
if (session.TryDequeuePush(out var push))
{
    if (push.Kind == Roost.Client.PayloadKind.Sync)
    {
        syncReceiver.Receive(push.Payload.Span, frame =>
        {
            // Full先清空旧流的对象/引用，再应用create；后续Delta继续补齐可见对象。
            // Component.Data由对应namespace/profile/encoding的业务packer解码。
            // 必须在回调内完成应用，失败抛异常；不要只入队就返回成功。
        });
    }
    else if (push.Kind == Roost.Client.PayloadKind.Protobuf)
    {
        // 根据push.MessageId调用该消息的生成PB Parser。
    }
}
```

默认单包1MiB、最多64个在途请求（包括等待发送锁者）、256个待消费推送；按业务容量调小/调大，队列计数乘单包上限仍会占用内存。超时必须为正且不超过`int.MaxValue`毫秒。等待锁时取消不关闭健康连接；写期间取消或失败可能留下半包，会关闭连接；发送成功后的超时仅停止等待，服务器业务可能已执行，不自动重放。晚应答丢弃，未注册的推送留原始字节。NotifyAsync提供PB/Lockstep无应答发送，和Request共用容量/锁/序列；完成只证明字节写出，不能推断服务器业务成功。

推送队列满时关闭连接、失败所有等待者并清空不能继续使用的增量，不能静默丢一个delta后继续应用。`Completion`表示接收循环结束，关闭原因读取`CloseReason`。直接使用 TcpSession 时，关闭后创建新实例。上层 Connection 提供心跳、可选自动重连和重新取票；GameClient/EntityViews 管理连接代际与对象视图。业务订阅恢复仍取决于服务端登录和进房流程。

### GameClient 回调与销毁边界

构造、订阅、Request、Pump 和 Dispose 在同一消费线程调用。回调可以 Dispose 客户端：已经进入的回调正常返回，但当前事件剩余订阅者以及本次 Pump 后续业务通知立即停止；已排队完成项被清空，在途结果不再投递。Dispose 不撤销服务端已经执行的业务。业务解析器返回或抛错后也重新检查销毁状态，不再通知成功、失败或 Error 观察者。

所有业务多播通知统一逐订阅者派发，每次调用前检查生命周期；单个观察者抛错交给 Error，不阻止其他观察者，除非客户端已销毁。新增事件应复用 Notify，禁止一次 Invoke 整个多播委托。ResetEntities、业务解析器等可调用外部代码的位置也属于生命周期边界，返回后不能继续处理已销毁客户端。这是维护规则，不是新增线程安全承诺。

RV-20261101-06 的原报告见 [review 1bf08673](https://github.com/tjbdwanghaibo/roost-core/blob/1bf08673/docs/maintenance/COMMIT-REVIEW.md)。修前在 Connecting 状态的首个订阅者 Dispose 后，后续订阅者仍执行；另一个确定性探针在 EntitiesReset 中销毁后仍收到 StateChanged。`DisposalTests.cs` 保留修前失败断言 `state subscriber disposal boundary` / `reset disposal must stop later reset and state callbacks`，修后覆盖普通多订阅者、首个与中间订阅者销毁、实体重置、TCP 推送与请求解析中销毁；重连测试同样覆盖首个与中间订阅者销毁。C# SDK 完整自测通过；未验证 Unity Player/IL2CPP 或生产 WAN，不以普通 C# 回归替代这些验收。

2026-11-01 验证命令均通过：`dotnet run --no-restore --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json`（含成功及失败结果的多播销毁）、`GOWORK=off go test -count=1 .`、`GOWORK=off go test -count=1 -timeout 10m -run '^TestClientSDKAgainstGeneratedPlayerTCP$' ./codegen/internal/roost`。本次只改 C# 消费线程通知，没有重跑 Go race 或容量压测。

`SyncFrame.Decode`和`SubjectUpdate.Decode`只解码已有Go格式，校验长度、操作和身份；`SubjectId`用`long`，StreamId/版本/掩码用`ulong`，不要转float/double。

底层 `SyncReceiver`按单连接单流核对epoch、tick、baseTick与schema：首帧必须Full，重复/旧代不再应用，缺口或业务回调失败后拒绝增量，成功应用才推进基线。其内存状态固定，不缓存历史帧或业务实体。Go对应`sync/frame.Receiver`。同一连接串行消费，直接使用底层 Receiver 时，组件状态由业务维护；上层 EntityViews 校验 generation 与 subject 版本，按注册的 codec 应用数据，不提供预测回滚或任意 packer 的自动解析。

接入方捕获缺口/应用错误后关闭旧TcpSession，清理业务状态、经正常票据鉴权重连并恢复订阅；新连接重新创建或Reset接收器，不能把旧连接排队的包送给新基线。已有鉴权控制链的业务也可调用服务端HoldSession/ReadySession得到新epoch；本库不新增客户端任意请求全量的公开协议。Full可能只是恢复的第一包，后续连续Delta/create补齐其余对象，不能等“一个Full含全部可见对象”。Unity的`RoostConnection`已接入缺口关闭与Disconnected通知，Attach前必须安装PacketReceived；Sync回调异常不再仅打印后继续增量。真实Unity运行验证仍未执行。

## Lockstep接入

服务端保留原Sequencer/Match/History/RedundantEncoder/DesyncDetector。TCP是可靠回退通道，有队头阻塞；没有将TCP包装成UDP性能保证。TCPSender的实时帧只进连接出站队列、不等socket写出（停读连接队满即被关闭，不拖住同一Tick的其他接收者），追帧页、快照分块与恢复失败通知入队并等写出，追帧与快照随链路速度放慢、CatchupSendTimeout生效。NewTCPSender的maxPayload应等于生成TCP/C#单包上限；按玩家数、MaxInputBytes、RedundancyDepth和CatchupBatchFrames计算每包预算，历史页也不能超过此上限。

生成access/player.NewMod可接收ProtocolRegistrar；在已有PB注册之后、Seal之前执行，注册冲突或失败会中止启动。业务在自己的app装配中使用player.NewMod(registerMatch)，原无参调用保持PB行为。注册函数通过RegisterPayloadNotify(...wire.PayloadLockstep, lockstep.DecodeCommand, handler)接线，handler只将认证PlayerID/Session.Principal().SessionID和Command交给业务Nest Sender，不能从网络handler并发操作Match。示意：

```go
func registerMatch(protocols *playeraccess.ProtocolRegistry, registry *app.Registry) error {
    return playeraccess.RegisterPayloadNotify(protocols, commandMessageID,
        wire.PayloadLockstep, lockstep.DecodeCommand,
        func(ctx *playeraccess.Context, command lockstep.Command) error {
            // matchSender使用既有Nest handler；按会话当前match归属找到比赛。
            return matchSender.Submit(ctx.Context(), ctx.PlayerID, ctx.Session.Principal().SessionID, command)
        })
}
// 初始化时：player.NewMod(registerMatch)。matchSender由业务启动装配注入。
// 比赛单所有者中：match.HandleCommand(authenticatedNumericSessionID, command)。
// MatchConfig.Datagrams和Reliable均可使用（实时只准入、追帧等写出，两个push不能接反）：
// lockstep.NewTCPSender(frameMessageID,maxPayload,concurrentSafeResolve,
//     tcpRuntime.PushLockstepSession,tcpRuntime.PushLockstepSessionWritten)
```

座位由服务器Attach赋予，Command不传player。会话映射须使用唯一连接生命周期ID，旧连接重绑后从Match移除；旧close事件不得Detach新连接，业务应核对绑定。旁观者只允许Catchup；Hash必须对应已切帧。Match所有Attach/Detach/HandleCommand/Tick/ReportHash/Close在比赛单所有者内串行执行；可靠页的resolver须并发安全，不能访问无锁Match map。每Tick应检查返回错误，发送错误不会撤回已经切出的权威帧。

Command格式为C8 01 + operation[1] + frame(uvarint32非零)：Input=1再带length+bytes（最多1024，Match通常配置更小）；Hash=2再带uvarint64；Catchup=3无尾部。拒绝未知操作、零帧、溢出、截断、尾字节和多余字段。Notify不返回服务器成功ACK；如业务需要可观察的拒绝结果，应另外定义PB业务响应。禁止SDK自动重传输入，原Sequencer只在有界ReplayHorizon内去重。

C#广播与历史页同用C7 v1（恢复失败通知为C9 v2，快照分块为CA 01），最多64帧、每帧256输入、每输入1024字节，严格递增并复制载荷。LockstepAssembler对每个包事务式组装：乱序最多默认256帧，坏包/溢出不提交部分释放结果；这与Go原Assembler可能返回已释放帧+error的契约不同。LockstepClient将坏包、容量溢出和模拟回调失败视为终止，应用新建比赛实例并恢复；不重复可能已部分执行的模拟。

```csharp
var match = new Roost.Client.LockstepClient(session, commandMessageId, frameMessageId,
    TimeSpan.FromSeconds(5));
await match.SubmitInputAsync(1, encodedInput);
// 在引擎主线程串行消费，上一HandlePushAsync未完成前不要取下一Lockstep包：
if (session.TryDequeuePush(out var packet) && packet.Kind == Roost.Client.PayloadKind.Lockstep)
    await match.HandlePushAsync(packet, frame => deterministicGame.Step(frame));
// 固定关键帧后，报告真实游戏状态摘要；InputHasher仅作输入链诊断。
await match.ReportHashAsync(lastAppliedFrame, deterministicGame.StateHash);
```

同连接混合推送先按Kind+MessageId分流；其他类型不能丢弃。缺口从Next自动请求历史，连续缺口默认64个包重试；若没有新包，调用方按自己的有界计时器调用RequestCatchupAsync((uint)match.Next)。无包计时重试/比赛超时不内置，避免伪造主动恢复保证。已有追帧允许向后修正起点。服务端历史默认只保留最近3600帧；服务端无法完成恢复时经同一路由推送恢复失败通知（`C9 02 reason from start`），`HandlePushAsync`抛`LockstepCatchupUnservableException`（`Reason`：`HistoryTrimmed` 起点已被裁掉 / `SnapshotUnavailable` / `SnapshotTooLarge` / `RecoveryExpired`；`RequestedFrom`/`HistoryStart`）并把它记为`Failure`，实例终止；业务据此踢出、回大厅或开新局，不能跳过缺帧继续模拟，SDK不自动重同步。

### 快照恢复

服务端配置了快照来源（Go `lockstep.SnapshotProvider`）时，缺的帧超出历史窗口（或超过阈值、或对局设为总是快照）会改发全量快照：客户端仍只发`OpCatchup(from)`，服务端经同一路由下发`CA 01`分块（首块带帧号、总长、可选状态hash与SHA-256摘要），之后从快照帧+1续发普通C7历史页并回到实时广播。C#用法：

```csharp
var match = new Roost.Client.LockstepClient(session, commandMessageId, frameMessageId,
    TimeSpan.FromSeconds(5),
    // 在调用方（引擎主线程）同步执行：用快照替换模拟状态，返回恢复后的状态hash（与ReportHash同一算法）。
    restore: snapshot => deterministicGame.Restore(snapshot.Frame, snapshot.Data.Span));
```

- 分块在`HandlePushAsync`里按`transfer`/`offset`重组：新编号且偏移0取代未完成的旧传输；同编号且偏移小于已收字节是重发，丢弃；偏移跳跃、越过总长、首块总长超过`maxSnapshotBytes`（缺省4 MiB，最大64 MiB）是坏包。传输期间收到的C7整包丢弃、不模拟，也不因缺口重发追帧请求（`SnapshotInProgress`）。
- 收齐后先核对SHA-256，再调`restore`；快照带状态hash时与`restore`的返回值比较。成功后实例从快照帧+1继续（`Next`），乱序缓冲清空，`SnapshotsRestored`加1。
- 失败一律终止（记为`Failure`，不自动重试）：分块坏包`InvalidDataException`；摘要不符`LockstepSnapshotCorruptException`；状态hash不符`LockstepSnapshotMismatchException`（`Frame`/`Expected`/`Actual`，说明客户端版本/规则/确定性与服务器参考不一致；业务可在新建实例前自行`ReportHashAsync(Frame, Actual)`留证）；`restore`抛出的异常原样记录；没有传`restore`却收到快照`LockstepSnapshotUnsupportedException`。
- 只用`LockstepAssembler`的调用方：`Receive`返回`LockstepDelivery`（`Frames` + 可选`Snapshot`），拿到快照、恢复后必须调用`ResumeAfterSnapshot(snapshot.Frame)`，在此之前`Receive`抛`InvalidOperationException`；`Ingest`只追帧，遇到快照分块抛`LockstepSnapshotUnsupportedException`。`LockstepInputHasher.Reset(value)`可在恢复后续接输入链诊断。
- Unity适配层不变：`RoostConnection`只转发`Packet`，`restore`与模拟一样在`PacketReceived`处理者所在的主线程执行。
- 重连即新实例：`LockstepClient`绑定一个`TcpSession`，重新登录后要新建实例，组装器从帧1开始。缺的帧仍在服务端窗口内时（auto缺省阈值0）服务端从帧1追帧，业务须先把模拟状态重置到开局再消费；越界才走快照。生成工程的C#端到端（`TestClientSDKAgainstGeneratedPlayerTCP`的`GeneratedLockstepSnapshot`）走的是越界路径：断线200帧后重新登录、新实例经40 KiB多块快照恢复、续帧到头部，链值与服务器参考一致。
- 平台边界：SHA-256用`SHA256.Create().ComputeHash`（netstandard2.1与Unity/Mono都有，不用.NET 5+的`SHA256.HashData`）；只在本机.NET 10测试中运行过，尚未在Unity编辑器/IL2CPP平台实测。

Go客户端（`framework/sync/lockstep`与`robot`）对应接口：`FrameAssembler.Receive(packet)`返回`Delivery{Frames, Snapshot}`，恢复并核对状态hash后调`ResumeAfterSnapshot(snapshot.Frame)`（之前再投包返回`ErrSnapshotNotApplied`），客户端快照上限用`NewFrameAssemblerLimits`；`Ingest`遇快照分块返回`ErrSnapshotUnsupported`。`robot.LockstepBotConfig.Restore`/`MaxSnapshotBytes`/`ReportSnapshotMismatch`，`Stats().SnapshotsRestored`，`FrameHasher.Reset`；没设`Simulate`/`Hash`的纯线协议bot不设`Restore`也能以快照的StateHash续输入链。失败语义与C#相同（坏分块`ErrFrameCorrupt`、摘要`ErrSnapshotCorrupt`、hash不符`*SnapshotMismatchError`，一律终止）。服务端配置、快照来源写法与限速链路实测见[guide/04-sync §6](../docs/framework/guide/04-sync.md#落后的会话怎样回来追帧快照或自动)。

实例从帧1开始，一实例一场比赛。原C7不含match/epoch；应用须在握手/订阅/路由生命周期隔离比赛，跨比赛复用同一路由且仍有旧包时不能仅新建Assembler。没有`restore`时重连不提供游戏快照或自动跳帧；业务恢复/重放准备后再消费。游戏定点计算、随机种子、配置一致性和作弊裁决仍由业务实现，FrameHasher不证明游戏状态一致。[本轮方案与验收](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/IMPLEMENTATION-2026-10-08-CLIENT-LOCKSTEP.md)。

## 地形（可编辑地形客户端）

[可编辑地形方案](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md)的 C# 客户端部分，在 `Roost.Client` 的 `Roost.Client.Terrain` 命名空间（源码 `dotnet/Roost.Client/Terrain/`），随 `Roost.Client.dll` 进 Unity 包，纯 C#、`netstandard2.1`，只用 BCL 的 `DeflateStream` 与 `SHA256`。规则与服务端 [gameplay/terrain](../gameplay/terrain/README.md) 相同，查询结果与 Go 逐位一致。世界单位厘米：块 6400（64m）、采样间隔 100（1m）、每块 64 × 64 点、高度原始值 1 = 10cm。

| 部分 | 入口 |
| --- | --- |
| 原始地形包（格式版本 2：包头之后是索引表与区域水位表，版本 1 的包按 `TerrainPackVersionException` 拒绝） | `TerrainPack.Open(ReadOnlyMemory<byte>)`（Addressables / UnityWebRequest 读到的整份字节，不复制）、`TerrainPack.Open(Stream, leaveOpen)`（可随机读的 FileStream；Android 的 StreamingAssets 在 apk 里，不能用 FileStream，读成字节再打开）；打开时校验包头与索引的整体 SHA-256，`ReadChunk(key)` 每次校验该块的 SHA-256 再解压，`ChunkSha256(key)`、`Meta`、`Dispose` |
| 地形视图 | `new TerrainView(pack)`；写入口 `LoadBase(key)`、`SetOverlay(key, baseRevision, version, heights, materials)`（`ushort` / `byte` span；或直接传生成 DAO 的两个 `RepeatedField<uint>`，高度 > 65535、材质 > 255 时抛 `TerrainInvalidChunkException`，不截断）、`Unload(key)`；`TryGetVersion`、`GetChunk`（只读、不复制）、`Count` |
| 查询 | `Height` / `Material` / `IsWater` / `WaterDepth` / `Probe`（缺块、地图外抛异常）；每帧热路径用 `TryProbe(x, y, out ground, out missing)`，返回 `Ok` / `OutOfMap` / `NotLoaded`，不抛异常。水位线按块取（`TerrainMeta.WaterLevelOf` / `TerrainView.WaterLevelOf`：所在区域水位，否则全局水位；`TerrainGround.WaterLevel` / `Depth`），与服务端判水、游泳同一来源（包格式版本 2 的区域水位，方案 D3 / §25） |
| mesh 钩子 | 事件 `ChunkChanged`（`Loaded` / `Updated` / `Unloaded`，带前后版本与 `MeshesToRebuild`）；`TryBuildHeightGrid(key, Span<int>, out missing)` 与纯函数 `TerrainMesh.TryBuildHeightGrid(meta, key, self, right, top, topRight, destination, out missing)` 输出 65 × 65 顶点的世界高度（厘米） |
| 错误 | `TerrainBadPackException`、`TerrainPackVersionException`、`TerrainCorruptChunkException`（带块坐标，只影响这一块）、`TerrainOutOfMapException`、`TerrainNotLoadedException`（带缺的块）、`TerrainInvalidChunkException`、`TerrainStaleOverlayException`、`TerrainBaseRevisionException`，与 Go 的哨兵错误一一对应；不会用默认数据代替缺失或损坏的块 |

**接 EntitySync。** 改过的块是 `TerrainChunk` 实体，用 entitysync 同步（方案 §7）。客户端用项目生成的 `dao_sync.proto` 与 `dao_sync_apply.cs` 注册 DAO（见上文“EntitySync 变化回调”），收到 Full / Delta 后 `change.Current.Value` 已是生成 Apply 合并好的完整 `SyncTerrainChunkDao`，取四个字段交给视图：

```csharp
// Unity 主线程：启动时打开包、按视距 LoadBase 原始块（读包 + 解压，成批时放到后台线程）。
var pack = TerrainPack.Open(packBytes);
var terrain = new TerrainView(pack);
terrain.ChunkChanged += change => { foreach (var key in change.MeshesToRebuild) dirtyMeshes.Add(key); };

client.EntityChanged += change => {
    if (change.Kind == EntityChangeKind.Remove || !(change.Current!.Value is SyncTerrainChunkDao dao)) return;
    var key = ChunkKeyOfEntity(change.Identity.Update.SubjectId);   // 实体 ID → 块坐标是游戏工程的约定
    long version = dao.Version?.Value ?? 0;
    if (version == 0) return;                                        // 实体已建、还没提交过改动：按原始块看待
    terrain.SetOverlay(key, dao.BaseRevision?.Value ?? 0, version, dao.Heights.Value, dao.Materials.Value);
};

// 每帧：重建脏块的 mesh（缺邻块时等它装入，ChunkChanged 会再把这一块列进来）。
var heights = new int[TerrainMesh.VertexCount];
foreach (var key in dirtyMeshes)
    if (terrain.TryBuildHeightGrid(key, heights, out _)) RebuildUnityMesh(key, heights);  // 业务渲染代码
dirtyMeshes.Clear();
```

- `ChunkKeyOfEntity` 由游戏工程提供：game-demo 的实体唯一号是 `ChunkKey.Id + 1`（`game/entities/terrain_chunk.KeyOf`），实体 ID 的位布局见 `framework/entity`。SDK 只提供与 Go 相同的 `ChunkKey.Id` / `ChunkKey.FromId`。
- 对象上限（方案 D15）：开了地形的场景里，地形块单独计 64 个、玩家 / 怪 / 场景仍是 100，会话的对象引用表合起来 164；建筑与玩家、怪同在 100 那一类、不另计（方案 §27.10）。服务端每帧对象数仍按 100 切，所以 `SyncFrame.Decode` 的 `maxObjects` 保持缺省 100（即服务端的 `Limits.MaxObjects`），**不需要传 164**。`EntityViews` 若显式设容量，不得小于 164（缺省 10000）。玩家 / 怪的帧与不开地形时逐字节相同。
- 建筑（方案 [§27](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#27-建筑实体化与同步2026-11-10)）：普通的同步实体，命名空间 `building`，`change.Current.Value` 是生成 Apply 合并好的 `SyncBuildingDao`。按 `ConfigId` 查游戏的配置表 `building`（预制体 `Model`、占地宽高）实例化，`Params` 是修改参数值（下标是配置表 `building_param` 的槽；比槽少时缺的取缺省值、比槽多时多出的忽略），`Gx` / `Gy` 是锚点节点（占地左下角，世界坐标 = 节点 × 100 厘米），`Facing` 0～3（1、3 时宽高互换），`Foundation` 是整平地基的世界高度（`int.MinValue` 表示没整平，按地面放）；`State` 为 0 时不画（拆除之后、remove 之前的那一帧）。`Remove` 就是建筑离开视野或被拆。建筑与玩家、怪进出同一个场景主视野（按占地中心的距离，方案 §27.10），客户端不需要为它另设视野。占地只由配置宽高与朝向决定，参数不改变占地。夹具见下面“验证”。
- 版本单调：比视图里已有覆盖块旧的版本抛 `TerrainStaleOverlayException`（迟到的旧全量不会把地形改回去），版本相同则替换；同版本同数据（重连、重新进入视野时的重复全量）不发 `ChunkChanged`。`BaseRevision` 与包头不同抛 `TerrainBaseRevisionException`：客户端的原始地形资源过期，应更新资源。
- `Remove`（块离开地形 Interest）时示例保留已装的覆盖块：这一块的最新已知数据比原始块更接近服务端；再次进入视野时服务端重发全量（方案 §7“没有客户端缓存”）。按视距 `Unload` 之后再 `LoadBase` 得到的是原始块，直到这一块再次进入地形 Interest、收到全量为止——显示上会先是原始地形、再跳到改过的样子。需要避免时由业务自己保留“块 → 最后一份覆盖块”的缓存，在 `LoadBase` 之后补 `SetOverlay`。
- 顶点网格：`destination[j·65 + i]` 是世界采样点 (key.X·64 + i, key.Y·64 + j) 的世界高度，最右一列与最上一行取右 / 上邻块的第 0 列 / 行（共享边），地图右 / 上边复制本块最后一列 / 行，邻块没装返回 false。每个顶点等于 `Height` 在该点的值，相邻块的接缝逐个相同；一块变了，左 / 下 / 左下邻块的接缝也变，所以 `MeshesToRebuild` 最多 4 块。网格三角形在 1m 格内是平面而 `Height` 是双线性插值，格内的点可能差几厘米：落地、站立高度等要与服务端一致的判定用查询，不用渲染网格。Unity 是 Y 轴向上：世界 X → Unity x、世界 Y → Unity z、高度 → Unity y，厘米换米除以 100。
- 编辑请求（协议 `TerrainEdit`，msgid 10038；方案 D18 / [§20](../docs/maintenance/FEATURE-2026-11-08-editable-terrain.md#20-编辑请求身份与判重t01t02d182026-11-09)）：身份是（玩家 ID, `Seq`）。`Seq` 由客户端按玩家单调递增地取（新编辑 = 上一次 + 1，可跳号，取值 1～2^63−1），结果未知（超时、断线）时用**同一个** `Seq` 重发，已生效的答 `Duplicate`。会话开始（或重装、换设备，不知道上一次用到哪里）先发 `Seq = 0` 的查询，从答复的 `LastSeq + 1` 开始；不先查就从 1 重新计数，窗口内的旧号会被当成重复、新编辑不执行。答复总带 `LastSeq`。`terrain_seq`（100059）：`Seq ≤ LastSeq` 表示太旧（当时是否生效已无法确认，这次没做——看同步流里的地形，不要换新号把同一笔再发一遍），`Seq > LastSeq` 表示跳得太远（从 `LastSeq + 1` 续号再发）。旧字段 `RequestID` 已改名，C# 侧没有现成的 TerrainEdit 封装，接入时按生成的协议类型写。
- 线程：查询与 `TryBuildHeightGrid` 不取锁，任意线程可并发调用，同一块内不会读到两个版本；写入口由一把锁串行，`LoadBase` 读包与解压在锁外。`ChunkChanged` 在写入口的调用线程、新数据已可查询之后触发，订阅者抛错交给 `Error`。`TerrainPack.ReadChunk` 可并发（Stream 来源在锁内定位 + 读取）。

验证：`dotnet run --project client/dotnet/Roost.Client.Terrain.Tests -- client/spec`（`scripts/test-client-dotnet.sh` 与 pretag 也跑）。它核对 Go 写出的 [terrain-golden.pack](spec/terrain-golden.pack) 的整包与 6 块校验和、区域水位表、[terrain-queries.json](spec/terrain-queries.json) 全部 246 个位置的高度 / 材质 / 水 / 所在块的水位线 / 水深；坏包头、索引、截断、坏块；覆盖块规则与通知；网格顶点与接缝；以及 Go 生成 DAO 写出的 `TerrainChunk` 全量、按下标的高度增量、按下标的材质增量、材质整字段、高度整字段五步经 C# 生成 Apply 合并后与 Go 的合并结果相等、与 Go `View` 的 650 次查询逐点相等（材质在 DAO 里是 `[]uint16` / `repeated uint32`，取值 0～255，方案 D11）；`Building` 的放置全量、只带参数的增量、换成无槽配置、拆除四步经 C# 生成 Apply 合并后与 Go 全量逐字节相等、摆放字段一致（方案 §27）。这份同步夹具在 `dotnet/Roost.Client.Terrain.Tests/Fixtures/`，由 `regen.sh` 用正式 DAO 生成器从 demo 的 `TerrainChunkDao` 与 `BuildingDao` 定义生成（`-update` 覆盖，缺省只核对，`scripts/test-client-dotnet.sh` 的完整模式会核对它与当前生成器一致）。没有在 Unity Editor、IL2CPP 或移动设备上运行过；没有经过真实 entitysync 链路（服务端 `TerrainChunk` 接 entitysync 是 P2 服务端部分）。C++ 客户端没有地形实现。

## Unity接入与其他引擎

按 [Unity 构建入口](unity/README.md) 生成完整 UPM 包，再从输出目录导入 `package.json`；不要直接导入未构建的 `client/unity`，也不要重复放置 `Roost.Client.dll`。Unity 项目选择支持 .NET Standard 2.1 的兼容级别。C# 业务 PB 类型及 Google.Protobuf 依赖由游戏工程管理。包内保留纯 C# 适配，同时提供复用 native 网络线程的 `RoostNativeDriver`；同一连接选择一种路径。

`Roost.Unity.RoostConnection`在Unity主线程Attach会话，`Update`每帧最多拉取配置数量的推送，然后触发`PacketReceived` / `Disconnected`；网络线程不访问Unity对象。Detach/销毁关闭自己的会话，替换时清掉旧连接队列。PB/Sync/Lockstep解码及游戏对象修改由主线程事件处理者实施；Lockstep异步消费必须串行await，不能直接挂并发async void事件。该正式适配源码已在.NET测试中直接链接，并通过真实TCP的缺口/业务异常关闭回归；测试只替代Unity宿主类型，尚未在Unity编辑器/IL2CPP平台实测。

后续Godot .NET复用纯C#库并补主线程适配；Godot GDExtension与Unreal优先使用共享C++实现及同一金样。当前没有C++运行库、Godot/Unreal插件或浏览器传输，不把本机.NET通过等同于引擎验收。Lockstep复用纯C#库；Unreal/GDExtension仍需C++实现，不将.NET验证当C++验收。

## 本地验证与金样

```sh
go test -race ./client/wire ./framework/sync/frame ./robot/... ./codegen/internal/protocol -count=1
dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json
go test ./codegen/internal/roost -run '^TestClientSDKAgainstGeneratedPlayerTCP$' -count=1 -v
# 或者一条命令跑上面两条 C# 验证（缺 dotnet 时打印 SKIP 并以 0 退出）：
./scripts/test-client-dotnet.sh
```

测试程序只接受三种参数：`<packets.json>`；`<packets.json> <host> <port>`；`<packets.json> cross-gate <gateAPort> <gateBPort> <requestId> <responseId>`。第三种只供 gateway 包的跨 Gate 用例驱动，见下文。其他参数打印用法并以 2 退出。`scripts/test-client-dotnet.sh` 在金样与自测之后跑 `infra/network/gateway` 的 `TestAcrossGatesTheCSharpClientThatAbandonsAHandshakeKeepsItsNewerConnection`：同一进程里起两个独立 Gate 和一个 Game（进程内 NATS 替身），用真实 C# 进程依次完成以下步骤，先后用标准输入输出做屏障：

1. 在 Gate A 上发起握手，旧 Bind 停在 Game 的鉴权里；
2. 放弃这次握手，转连 Gate B，完成握手并答完一个请求；
3. 放行旧 Bind 后，断言新绑定仍在、新连接仍能应答。

运行库不带代号时这条用例变红。pretag 跑金样与自测，CI 的 `client-dotnet` job 跑全部（缺 dotnet 即失败）；维护手册 §3 把它列为改协议 / Sync / Lockstep 时的验证。

正式生成工程测试需要.NET 10 SDK（测试程序target net10.0，运行库仍netstandard2.1）；缺SDK时明确skip，不能当C#验收。测试在私有生成工程执行真实TCP鉴权→生成PB编解码→并发请求→raw Sync推送，以及Match→TCPSender→C#输入/Hash/主动制造缺口→可靠追帧，并运行现有TCP/Scene race场景，重生成托管目录必须字节不变。

`client/spec/packets.json`由Go正式wire、frame、entitysync和lockstep编码器生成，C#消费并验证64位身份、frame/SubjectUpdate、Command/C7、与robot一致的输入链hash、C9 v2各原因、CA 01两块重组（SHA-256与最高位为1的uvarint64状态hash）和重新封包。只有协议有意变化时才执行`go test ./client/wire -run TestClientProtocolGolden -update`并检查差异，普通回归不能自动更新期望。[设计与当前验证边界](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-10-07-CLIENT-PROTOCOL.md)。

TCP发送会检查fctx当前执行位置，业务 worker 在resolver/socket前返回ErrBlockingInBusinessWorker。Match.Tick含同步网络I/O，必须由Entity锁外的比赛单所有者驱动；不能放进Nest快handler等待socket，也不能临时换goroutine绕过标记却并发访问Match。Nest边界只转交命令/调用已配置的比赛单所有者；本轮真实TCP夹具用独立串行owner channel，证明协议/Match消费，未声称具体业务比赛Nest实体及持久化装配已验收。

### Gate + 生成 game-demo 联合验证

运行 `bash scripts/test-gate-client.sh`：自动创建独立本机 NATS，生成并启动验证宿主，编译 `Roost.Client.Demo`，验证业务 PB 请求/应答、真实 DAO PB 全量/增量、删除通知与重连。消息 ID 同样来自生成器，C# 不手写编号。详见 [Gate 联合链路](../docs/framework/GATEWAY.md#生成工程与标准-c-客户端联合链路)。该入口是可运行的接入参考和联合测试，存储投影、账号票据和生产部署的边界见手册。

## Native C++ 共享网络核心

[client/cpp](cpp/README.md) 提供 C++17 核心、C ABI、Unity 托管绑定和 UE 外部库接线。当前已补业务 PB、EntitySync Full/Delta/Remove、Lockstep 追帧/快照恢复、场景时间域、通知/取消与可选重连。网络线程独占 socket，消费线程派发业务回调。网络 ABI 2 需要库和绑定一起更新；具体能力、可选 Protobuf 依赖及各平台未验证状态见 native 手册。
