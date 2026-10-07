# 客户端协议与 SDK：PB / Sync 分流

2026-10-07，实施基线 `494fe096`。维护者授权按客户端方案实施，并明确不考虑旧包兼容。

## 当前问题与决定

普通 robot 使用12字节小端包头，正式生成的 player TCP 使用16字节大端 RS 包头，demo loadtest 单独翻译。EntitySync 在 demo 还包进 PB 的 bytes 字段，客户端无法直接由协议头选择业务解码。统一为正式 TCP 的16字节布局，不增加包头大小；提升版本为2，旧版本直接拒绝，不猜格式、不双写。

布局：`magic RS[2] | version=2[1] | flags[1] | message_id[4] | sequence[4] | payload_length[4] | payload`，整数为大端。flags bit0=服务器推送，bits1..2为载荷类型字段：00=PB、01=Sync、10=Lockstep（后续实现）、11=保留。当前Lockstep、保留类型及其他保留位均拒绝；消息ID决定业务路由，类型位决定载荷解码，两者不能互相替代。message_id=0只属于鉴权控制握手，payload为应用凭据原始字节，不是假装PB消息。

Sync载荷直接采用已有 frame + SubjectUpdate + 业务packer格式，不再额外套EntitySyncPush PB。初版player TCP客户端请求仍仅注册PB，收到客户端Sync请求时在Dispatch前拒绝；不因此给客户端修改服务器权威状态的能力。Sync推送走显式PushSyncPlayer/PushSyncSession，保留原推送失败/关闭和多连接责任。

## 结构与依赖

新增 `client/wire`（Go，仅标准库，共享头/校验/封包）、`client/dotnet`（纯C# netstandard2.1）、`client/unity`（主线程适配），保留既有robot/transport、生成TCP/鉴权/生命周期包。Go核心不依赖C#工具链。传输只理解头与字节；PB和Sync业务内容留各自解码器。C#库不引用Unity/Godot，不以Go runtime/native插件作为客户端必需品。

现有codegen的Go定义→proto/msgid/manifest仍为单一来源；扩展可选C#消息编号输出，PB类型由同一proto经protoc生成，不新增手写消息表/序列化格式。空定义要退役对应C#生成文件，其他源码不能被覆盖。

## 实施批次

1. 本轮统一Go包头和正式生成TCP/探针/robot/loadtest，接线原始Sync推送；C#实现封包、鉴权、请求匹配、有界待请求、推送类型分流和Sync解码，Unity主线程适配。代码生成增加C#编号与PB导出说明。
2. 后续Godot .NET适配，以及正式业务packer对应的客户端状态应用/恢复与生成器；它不是当前通用frame解码通过即可宣称完成。
3. C++共用同一规范/金样，Unreal与Godot GDExtension适配；平台构建/真实引擎验证分别留证，不由本机C#通过代替。

请求超时仅表示停止等待；不自动重放业务操作。发送中断后关闭连接，避免留下半包继续使用。旧连接的应答不得投递给新连接。主线程回调有界，不在网络线程创建引擎对象。

## 兼容、验证和回退

明确破坏旧wire版本：全部应用重新生成player TCP/探针/loadtest，客户端一并升级；无旧版本兼容、无持久数据迁移。protobuf字段编号、Sync内部版本/epoch/generation不随外层升级而修改。

Go普通PB/Sync与坏头/超限/分包/粘包正式回归；生成工程运行真实TCP鉴权→PB请求/响应→原始Sync推送，跨Go/C#金样及真实连接消费；按影响race/vet、根包与全仓build。Unity IL2CPP、Godot、Unreal、浏览器及真实公网传输需实际环境验证，未执行不得标通过。不等待GitHub CI，不发版。

回退按整笔提交和客户端协议版本一起回退，不能单独把版本改回1而保留新的raw Sync语义。性能暂无前后对照，只声明统一布局/去除额外PB套壳，不声称吞吐或零分配改善。

## 已落地的调用链与组合责任

公共wire → 正式生成TCP readFrameLimit（校验后申请payload） → 既有鉴权/严格序列 → PB ProtocolRegistry/typed endpoint/Dispatch。反向PB Reply/Push与raw Sync PushSyncPlayer/PushSyncSession统一封包。sceneLane直接传entitysync产生的冻结frame，所有连接的部分失败、慢连接关闭、sessionLost及恢复full责任继续由原实现承担，正式Scene测试覆盖这条链。robot先按FlagPush分流，Sync留原始消息、不交PB decoder。

C#先校验输入、限制inFlight（包括排队发包者），发送锁内分配seq与登记等待者，确保并发线上排序；应答按seq、message_id、PB类型匹配。推送单独有界排队。写中取消或失败可能留下半包，关闭流并结束等待者；已发送后的取消只停止等待，服务器可能已执行，晚应答不能变成push。满push队列关闭并清空不能继续使用的增量，业务需重新获取full。Unity只在Update主线程有界消费，销毁/更换会话关闭旧连接。此批仅包含协议、运行库与解码，未实现完整业务复制客户端。

C#编号通过protocol CLI的-csharp显式导出，同一Go定义生成PB proto和编号；沿既有请求/应答同ID约束。支持Git autocrlf重生成；手写输出及非普通目标文件拒绝，且在其他产物写入前拒绝；空定义只退役本生成器拥有的文件。普通roost generate没有自动加C#产物，业务构建按客户端README接入导出步骤。

资源：Windows、Go1.27.0、.NET10 SDK；运行库target netstandard2.1，测试消费者target net10.0。未查询/等待GitHub CI。main尚未发版，新生成server需包含client/wire的core版本或本checkout的replace；不能用已发布旧tag冒充本轮运行。正式发版时同步runtime下限和客户端协议升级说明。

## 具名验证清单

| 触发/消费者 | 预期与实际 | 复跑入口 |
| --- | --- | --- |
| Go PB/Sync粘包、共享头/流读取、坏版本/Lockstep/保留位/超限 | 申请payload前拒绝非法头；解包副本隔离；已有robot场景race通过 | `go test -race ./client/wire ./robot/... ./codegen/internal/protocol -count=1` |
| 正式Go entitysync/frame编码 → C#金样 | PB/auth/raw Sync重新封包一致；subject ID 9007199254740993保持整数；截断及Lockstep拒绝 | `dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json` |
| C#真实回环TCP取消/晚应答/容量 | 服务端读到后续包证明旧包写阶段结束，再取消旧等待；晚应答丢弃；非法预算不占容量；pending满额拒绝；push溢出关闭并失败pending | 同上，LocalSession与Capacity实际执行 |
| 私有正式生成game-demo的TCP/生成PB/C#与现有Scene | 实际鉴权、32并发请求排序/应答关联、raw Sync解码；客户端Sync请求在Dispatch前拒绝；现有TCP/Scene race通过 | `go test ./codegen/internal/roost -run '^TestClientSDKAgainstGeneratedPlayerTCP$' -count=1 -v` |
| C#生成编号/proto/手写保护/CRLF/退役 | 编号与Go同源；手写文件连-force也不覆盖；拒绝不先改proto；空定义保留手写/删除生成；protocol整包race通过 | `go test -race ./codegen/internal/protocol -count=1` |
| 本仓编译/vet | 全仓build与受影响包vet通过 | `go build ./...`；`go vet ./client/wire ./robot/... ./codegen/internal/protocol ./codegen/internal/roost` |
| 根包真实示例 | 首次六个示例编译后因Windows无.exe后缀无法启动；补平台后缀后根包与六示例真实运行通过 | `GOWORK=off go test -count=1 .` |
| 生成器短回归 | 第一次因sh未进PATH、旧header[3]源码断言失败；使用Git sh并更新共享FlagPush断言后整包通过 | PowerShell：`$env:PATH='C:/Program Files/Git/usr/bin;'+$env:PATH; go test ./codegen/internal/roost -short -count=1` |

首次正式生成消费还报scene/loadtest两处未使用import，已清理并整链重跑通过；编译失败不当协议行为红。Shellcheck未安装，没有借短回归通过宣称它已运行。旧`TestGeneratedGameDemoBuildsAndVetsAgainstThisCheckout`在Windows显式skip，不能算验收；本轮SDK正式消费测试新增私有生成工程全包build/vet，最终结果见下方记录。

图谱按Tier2定位/coverage检查：roost-core generation仍为2026-09-30T11:58:14Z，旧路径metadata_changed、新文件not_tracked，均读取当前源码及实际生成消费补证。早期图谱transport closed后源码回退，恢复后核对project/root和覆盖信号。本记录不作全仓穷尽或负面完整性结论。

## 下一阶段实施条件

1. 按真实业务packer同源生成客户端组件模型，实现epoch/generation/版本/full-delta应用与失败后恢复；用跨进程业务及断线场景验收，不能以通用解码通过替代。
2. 补C#无应答Notify、心跳/空闲预算、TLS或明确受保护接入环境；Godot .NET复用C#，做主线程适配与真实引擎消费。
3. C++共享规范/金样，接Unreal/Godot GDExtension；各引擎、IL2CPP和移动平台分别实测。WebGL/浏览器传输另设计。
4. Lockstep仅保留类型10，后续授权时优先接既有lockstep能力，明确输入/冗余/追帧/顺序/去重/恢复契约再启用。

## 本轮最终运行结果

以上具名Go/C#场景全部通过。最后版本的`TestClientSDKAgainstGeneratedPlayerTCP`实际执行生成工程全包`go build -mod=mod ./...`、`go vet -mod=mod ./...`，然后真实TCP/C#与生成TCP/Scene race，整体退出0（89.105s）。本仓`go build ./...`与受影响包vet退出0；C#运行库/测试程序编译无警告无错误；protocol最后一轮race退出0，包括空定义错误扩展名保护。根包修后退出0（19.722s），六个示例确实运行；生成器完整short回归在Git sh环境退出0（159.162s）。本机原始运行日志保留于仓外`D:/whb_s/.tmp/client-*.txt`，本段记录可复跑入口和结论，不提交本机临时文件。

跨语言PB证明范围是C#运行库收发业务PB字节、正式Go生成类型编解码和应答关联；C#业务PB类/protoc/Google.Protobuf组合由应用导出与锁版，本轮未实测该外部工具组合。没有Unity编辑器/IL2CPP、Godot/Unreal、TLS/public网络、业务复制应用/恢复、Lockstep客户端验收。既有整工程门禁的Windows skip与本轮实际执行分列；shellcheck缺失仍为未执行。未发版，不等待GitHub CI。
