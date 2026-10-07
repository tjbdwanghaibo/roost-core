# Roost 客户端协议与 C# SDK

Roost 的自定义协议负责帧、路由编号与请求关联，业务载荷由包头选择 PB / Sync / Lockstep。本批提供公共 Go wire、纯 C# `netstandard2.1` TCP 运行库和 Unity 主线程适配。Lockstep 只预留编号，暂不接线。

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
| 2 | Lockstep | 预留，收发均拒绝；后续接线 |
| 3 | 保留 | 拒绝 |

bits3..7必须为0。PB请求/响应flags=0，PB推送=1，Sync推送=3。类型选择解码器，message_id选择业务路由，两者分别判断，不能只看seq或编号猜类型。ID=0的控制帧flags必须为0：首包payload是票据原始UTF-8字节，成功ACK使用相同seq且payload为空，它不经过PB解码。

正式生成的player TCP要求客户端首包和后续包seq非零、单连接按线上发送顺序递增（允许uint32回绕）。C#在发送锁内分配seq；推送seq属于服务器独立空间，即使与请求相同也不能匹配待应答请求。正式TCP当前只接收PB业务请求，客户端Sync请求在Dispatch前拒绝。原始Sync推送复用既有frame v1 / SubjectUpdate v2 / 应用packer；内部版本未随外层改变。

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

// 在引擎主线程每帧有界消费；此示例只展示类型分流。
if (session.TryDequeuePush(out var push))
{
    if (push.Kind == Roost.Client.PayloadKind.Sync)
    {
        var frame = Roost.Client.SyncFrame.Decode(push.Payload.Span);
        // Component.Data由对应namespace/profile/encoding的业务packer解码。
    }
    else
    {
        // 根据push.MessageId调用该消息的生成PB Parser。
    }
}
```

默认单包1MiB、最多64个在途请求（包括等待发送锁者）、256个待消费推送；按业务容量调小/调大，队列计数乘单包上限仍会占用内存。超时必须为正且不超过`int.MaxValue`毫秒。等待锁时取消不关闭健康连接；写期间取消或失败可能留下半包，会关闭连接；发送成功后的超时仅停止等待，服务器业务可能已执行，不自动重放。晚应答丢弃，未注册的推送留原始字节。当前SDK提供请求/响应API，独立无应答Notify发送API留后续。

推送队列满时关闭连接、失败所有等待者并清空不能继续使用的增量，不能静默丢一个delta后继续应用。`Completion`表示接收循环结束，关闭原因读取`CloseReason`。连接关闭后创建新TcpSession实例；重连票据获取、重新订阅及full baseline恢复由业务驱动，本库不声称已提供完整复制状态机。断线检测基于socket I/O和请求预算，未实现独立心跳/空闲超时。

`SyncFrame.Decode`和`SubjectUpdate.Decode`只解码已有Go格式，校验长度、操作和身份；`SubjectId`用`long`，RoomId/版本/掩码用`ulong`，不要转float/double。尚未实现应用packer对应的客户端组件模型、epoch/generation基线应用、可靠恢复或预测回滚。

## Unity接入与其他引擎

先构建Release版`Roost.Client.dll`并放到Unity工程`Assets/Plugins`，再把`client/unity`作为本地UPM包加入（`package.json`）。包不附带已编译DLL，须先提供运行库；Unity项目选择支持.NET Standard 2.1的兼容级别。C#业务PB类型及Google.Protobuf依赖由游戏工程管理。

`Roost.Unity.RoostConnection`在Unity主线程Attach会话，`Update`每帧最多拉取配置数量的推送，然后触发`PacketReceived` / `Disconnected`；网络线程不访问Unity对象。Detach/销毁关闭自己的会话，替换时清掉旧连接队列。PB/Sync解码及游戏对象修改由事件处理者实施。该适配源码尚未在Unity编辑器/IL2CPP平台实测。

后续Godot .NET复用纯C#库并补主线程适配；Godot GDExtension与Unreal优先使用共享C++实现及同一金样。当前没有C++运行库、Godot/Unreal插件或浏览器传输，不把本机.NET通过等同于引擎验收。Lockstep未来沿类型字段接入既有框架能力，另做输入/冗余/追帧的客户端契约。

## 本地验证与金样

```sh
go test -race ./client/wire ./robot/... ./codegen/internal/protocol -count=1
dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json
go test ./codegen/internal/roost -run '^TestClientSDKAgainstGeneratedPlayerTCP$' -count=1 -v
```

正式生成工程测试需要.NET 10 SDK（测试程序target net10.0，运行库仍netstandard2.1）；缺SDK时明确skip，不能当C#验收。测试在私有生成工程执行真实TCP鉴权→生成PB编解码→并发请求→raw Sync推送，并运行现有TCP/Scene race场景。

`client/spec/packets.json`由Go正式wire、frame和entitysync编码器生成，C#消费并验证64位身份、frame/SubjectUpdate和重新封包。只有协议有意变化时才执行`go test ./client/wire -run TestClientProtocolGolden -update`并检查差异，普通回归不能自动更新期望。[设计与当前验证边界](../docs/feature/REFACTOR-2026-10-07-CLIENT-PROTOCOL.md)。
