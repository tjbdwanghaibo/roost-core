# Roost 客户端协议与 C# SDK

Roost 的自定义协议负责帧、路由编号与请求关联，业务载荷由包头选择 PB / Sync / Lockstep。本批提供公共 Go wire、纯 C# `netstandard2.1` TCP 运行库和 Unity 主线程适配。Lockstep 已接线：输入/Hash/Catchup命令、既有C7广播与C#有序消费。

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

bits3..7必须为0。PB请求/响应flags=0，PB推送=1，Sync推送=3；Lockstep上行Notify=4，下行push=5。类型选择解码器，message_id选择业务路由，两者分别判断，不能只看seq或编号猜类型。ID=0的控制帧flags必须为0：首包payload是票据原始UTF-8字节，成功ACK使用相同seq且payload为空，它不经过PB解码。

正式生成的player TCP要求客户端首包和后续包seq非零、单连接按线上发送顺序递增（允许uint32回绕）。C#在发送锁内分配seq；推送seq属于服务器独立空间，即使与请求相同也不能匹配待应答请求。正式TCP接收PB业务请求和显式注册的Lockstep Notify，客户端Sync请求在Dispatch前拒绝。类型不符在decoder/handler前拒绝，各类共用同一个message_id注册表。原始Sync推送复用既有frame v1 / SubjectUpdate v2 / 应用packer；内部版本未随外层改变。

Go封包实现是`client/wire`，robot TCP/WebSocket与正式生成的player TCP/探针/loadtest共用它。现有`sync/nettransport`的UDP/KCP/QUIC channel管理不在本次改造范围；没有据此宣称所有传输或公网TLS已统一。

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

推送队列满时关闭连接、失败所有等待者并清空不能继续使用的增量，不能静默丢一个delta后继续应用。`Completion`表示接收循环结束，关闭原因读取`CloseReason`。连接关闭后创建新TcpSession实例；重连票据获取、重新订阅及full baseline恢复由业务驱动，本库不声称已提供完整复制状态机。断线检测基于socket I/O和请求预算，未实现独立心跳/空闲超时。

`SyncFrame.Decode`和`SubjectUpdate.Decode`只解码已有Go格式，校验长度、操作和身份；`SubjectId`用`long`，RoomId/版本/掩码用`ulong`，不要转float/double。

新增`SyncReceiver`（本工作分支待运行验证）按单连接单流核对epoch、tick、baseTick与schema：首帧必须Full，重复/旧代不再应用，缺口或业务回调失败后拒绝增量，成功应用才推进基线。其内存状态固定，不缓存历史帧或业务实体。Go对应`sync/frame.Receiver`。同一连接串行消费，业务组件的generation、版本、解码及状态应用仍由业务维护；这里不提供预测回滚或所有packer的客户端模型。

接入方捕获缺口/应用错误后关闭旧TcpSession，清理业务状态、经正常票据鉴权重连并恢复订阅；新连接重新创建或Reset接收器，不能把旧连接排队的包送给新基线。已有鉴权控制链的业务也可调用服务端HoldSession/ReadySession得到新epoch；本库不新增客户端任意请求全量的公开协议。Full可能只是恢复的第一包，后续连续Delta/create补齐其余对象，不能等“一个Full含全部可见对象”。Unity的`RoostConnection`已接入缺口关闭与Disconnected通知，Attach前必须安装PacketReceived；Sync回调异常不再仅打印后继续增量。真实Unity运行验证仍未执行。

## Lockstep接入

服务端保留原Sequencer/Room/History/RedundantEncoder/DesyncDetector。TCP是可靠回退通道，有队头阻塞，Tick的同步广播可能等待socket写预算；没有将TCP包装成UDP性能保证。NewTCPSender的maxPayload应等于生成TCP/C#单包上限；按玩家数、MaxInputBytes、RedundancyDepth和CatchupBatchFrames计算每包预算，历史页也不能超过此上限。

生成access/player.NewMod可接收ProtocolRegistrar；在已有PB注册之后、Seal之前执行，注册冲突或失败会中止启动。业务在自己的app装配中使用player.NewMod(registerMatch)，原无参调用保持PB行为。注册函数通过RegisterPayloadNotify(...wire.PayloadLockstep, lockstep.DecodeCommand, handler)接线，handler只将认证PlayerID/Session.Principal().SessionID和Command交给业务Nest Sender，不能从网络handler并发操作Room。示意：

```go
func registerMatch(protocols *player_agent.ProtocolRegistry, registry *app.Registry) error {
    return player_agent.RegisterPayloadNotify(protocols, commandMessageID,
        wire.PayloadLockstep, lockstep.DecodeCommand,
        func(ctx *player_agent.Context, command lockstep.Command) error {
            // matchSender使用既有Nest handler；按会话当前match归属找到比赛。
            return matchSender.Submit(ctx.Context(), ctx.PlayerID, ctx.Session.Principal().SessionID, command)
        })
}
// 初始化时：player.NewMod(registerMatch)。matchSender由业务启动装配注入。
// 比赛单所有者中：room.HandleCommand(authenticatedNumericSessionID, command)。
// RoomConfig.Datagrams和Reliable均可使用：
// lockstep.NewTCPSender(frameMessageID,maxPayload,concurrentSafeResolve,tcpRuntime.PushLockstepSession)
```

座位由服务器Attach赋予，Command不传player。会话映射须使用唯一连接生命周期ID，旧连接重绑后从Room移除；旧close事件不得Detach新连接，业务应核对绑定。旁观者只允许Catchup；Hash必须对应已切帧。Room所有Attach/Detach/HandleCommand/Tick/ReportHash/Close在比赛单所有者内串行执行；可靠页的resolver须并发安全，不能访问无锁Room map。每Tick应检查返回错误，发送错误不会撤回已经切出的权威帧。

Command格式为C8 01 + operation[1] + frame(uvarint32非零)：Input=1再带length+bytes（最多1024，Room通常配置更小）；Hash=2再带uvarint64；Catchup=3无尾部。拒绝未知操作、零帧、溢出、截断、尾字节和多余字段。Notify不返回服务器成功ACK；如业务需要可观察的拒绝结果，应另外定义PB业务响应。禁止SDK自动重传输入，原Sequencer只在有界ReplayHorizon内去重。

C#广播与历史页同用C7 v1，最多64帧、每帧256输入、每输入1024字节，严格递增并复制载荷。LockstepAssembler对每个包事务式组装：乱序最多默认256帧，坏包/溢出不提交部分释放结果；这与Go原Assembler可能返回已释放帧+error的契约不同。LockstepClient将坏包、容量溢出和模拟回调失败视为终止，应用新建比赛实例并恢复；不重复可能已部分执行的模拟。

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

同连接混合推送先按Kind+MessageId分流；其他类型不能丢弃。缺口从Next自动请求历史，连续缺口默认64个包重试；若没有新包，调用方按自己的有界计时器调用RequestCatchupAsync((uint)match.Next)。无包计时重试/比赛超时不内置，避免伪造主动恢复保证。已有追帧允许向后修正起点；历史被裁剪时须业务恢复，不能跳过缺帧继续模拟。

实例从帧1开始，一实例一场比赛。原C7不含match/epoch；应用须在握手/订阅/路由生命周期隔离比赛，跨比赛复用同一路由且仍有旧包时不能仅新建Assembler。重连不提供游戏快照或自动跳帧；业务恢复/重放准备后再消费。游戏定点计算、随机种子、配置一致性和作弊裁决仍由业务实现，FrameHasher不证明游戏状态一致。[本轮方案与验收](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/IMPLEMENTATION-2026-10-08-CLIENT-LOCKSTEP.md)。

## Unity接入与其他引擎

先构建Release版`Roost.Client.dll`并放到Unity工程`Assets/Plugins`，再把`client/unity`作为本地UPM包加入（`package.json`）。包不附带已编译DLL，须先提供运行库；Unity项目选择支持.NET Standard 2.1的兼容级别。C#业务PB类型及Google.Protobuf依赖由游戏工程管理。

`Roost.Unity.RoostConnection`在Unity主线程Attach会话，`Update`每帧最多拉取配置数量的推送，然后触发`PacketReceived` / `Disconnected`；网络线程不访问Unity对象。Detach/销毁关闭自己的会话，替换时清掉旧连接队列。PB/Sync/Lockstep解码及游戏对象修改由主线程事件处理者实施；Lockstep异步消费必须串行await，不能直接挂并发async void事件。该正式适配源码已在.NET测试中直接链接，并通过真实TCP的缺口/业务异常关闭回归；测试只替代Unity宿主类型，尚未在Unity编辑器/IL2CPP平台实测。

后续Godot .NET复用纯C#库并补主线程适配；Godot GDExtension与Unreal优先使用共享C++实现及同一金样。当前没有C++运行库、Godot/Unreal插件或浏览器传输，不把本机.NET通过等同于引擎验收。Lockstep复用纯C#库；Unreal/GDExtension仍需C++实现，不将.NET验证当C++验收。

## 本地验证与金样

```sh
go test -race ./client/wire ./sync/frame ./robot/... ./codegen/internal/protocol -count=1
dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json
go test ./codegen/internal/roost -run '^TestClientSDKAgainstGeneratedPlayerTCP$' -count=1 -v
```

正式生成工程测试需要.NET 10 SDK（测试程序target net10.0，运行库仍netstandard2.1）；缺SDK时明确skip，不能当C#验收。测试在私有生成工程执行真实TCP鉴权→生成PB编解码→并发请求→raw Sync推送，以及Room→TCPSender→C#输入/Hash/主动制造缺口→可靠追帧，并运行现有TCP/Scene race场景，重生成托管目录必须字节不变。

`client/spec/packets.json`由Go正式wire、frame、entitysync和lockstep编码器生成，C#消费并验证64位身份、frame/SubjectUpdate、Command/C7、与robot一致的输入链hash和重新封包。只有协议有意变化时才执行`go test ./client/wire -run TestClientProtocolGolden -update`并检查差异，普通回归不能自动更新期望。[设计与当前验证边界](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-10-07-CLIENT-PROTOCOL.md)。

TCP发送会检查fctx当前执行位置，快worker在resolver/socket前返回ErrBlockingInFastWorker。Room.Tick含同步网络I/O，必须由Entity锁外的比赛单所有者驱动；不能放进Nest快handler等待socket，也不能临时换goroutine绕过标记却并发访问Room。Nest边界只转交命令/调用已配置的比赛单所有者；本轮真实TCP夹具用独立串行owner channel，证明协议/Room消费，未声称具体业务比赛Nest实体及持久化装配已验收。
