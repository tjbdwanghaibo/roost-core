# Lockstep 客户端与 RS v2 接线

2026-10-08，基线 `36fb32ab`，独立工作树实施。用户授权“开始实施lockstep”，此前类型10仅预留；不考虑旧包兼容。

## 采用的方案

保留`sync/lockstep`中的Sequencer、Room、History、冗余广播、追帧与DesyncDetector，不另写比赛排序器。RS v2布局/版本不变，启用类型10：上行flags=4，下行push=5；类型11和未知位继续拒绝。PB/Sync不变。

下行直接使用既有`C7 01 + frameCount + frame/input varints`，广播与可靠追帧页同一格式。上行此前是业务自定义，新增同包Command codec：`C8 01 + operation[1] + frame(uvarint)`，Input再带`length(uvarint)+bytes`，Hash再带`hash(uvarint64)`，Catchup没有尾部。帧非零、Input最多1024字节、拒绝溢出/截断/尾字节；不携带客户端自报的player，座位从已鉴权会话映射并由Room当前绑定再校验。Room、match/channel生命周期仍由应用拥有。

正式生成ProtocolRegistry增加载荷类型元数据与类型化Notify注册，所有类型共用原有注册/Seal/middleware/鉴权/Dispatch链路。PB默认类型0；只有显式注册的Lockstep Notify接收类型2，错误类型在decoder/handler前拒绝。Sync上行仍禁止，不能因为头识别Sync就授予客户端写权。正式TCP继续严格序列/请求预算/限流/关闭，控制鉴权和心跳只接受flags=0。

新增同包TCPSender适配已有Room发送接口，调用生成Runtime的PushLockstepSession；这是可靠TCP接入，可能有队头阻塞，不冒认UDP实时性能。Room单所有者：Attach/Command/Tick/Hash仍须在比赛串行handler里执行，controller通过既有业务Sender转交，不能由多个连接直接并发操作Room。SessionResolver供后台追帧调用，应用须提供并发安全的映射。

C#仍在纯netstandard2.1库里按职责分文件：广播/Command codec、带界限的有序去重Assembler、主线程LockstepClient消费与Input/Hash/Catchup发送。Notify与PB Request共用发送锁/序列/容量/取消契约；Notify完成只证明发送成功，不代表业务执行ACK。客户端缺口从Next请求追帧，持续缺口按包计数重试；模拟回调异常将该比赛客户端终止，不能重传后把部分模拟当未发生。客户端实例只用于一场比赛，跨比赛/断线需新实例与业务恢复，未提供游戏快照。

## 验证与升级

Go现有Room/Sequencer/robot回归与race、Command坏包/身份/重绑/旁观者控制；Go正式编码金样供C#解码/重封包/输入链哈希；C#乱序/重复/补洞/容量/模拟失败/通知发送；正式生成game-demo完整build/vet与真实TCP+C#，验证注册类型拒绝、输入→Room.Tick→广播、Hash与历史追帧。按影响跑本仓build/vet/glsvet、根包与全量测试，保留实际skip/环境失败。Unity/Godot/Unreal实机、公网UDP和确定性游戏模拟不由本轮回环证明。

升级需同版本core/生成TCP与C#库；旧首批客户端拒绝Lockstep，不能混用。回退整笔接线与客户端一起回退，恢复类型10拒绝，不单独回退Command或头标记。没有改持久格式，也不自动发版、不等待GitHub CI。

图谱Verify代际仍为2026-09-30，相关源码已变；已按coverage读当前工作树补证，不把图谱结果当当前全覆盖。

TCP发送会检查fctx当前执行位置，快worker在resolver/socket前返回ErrBlockingInFastWorker。Room.Tick含同步网络I/O，必须由Entity锁外的比赛单所有者驱动；不能放进Nest快handler等待socket，也不能临时换goroutine绕过标记却并发访问Room。Nest边界只转交命令/调用已配置的比赛单所有者；本轮真实TCP夹具用独立串行owner channel，证明协议/Room消费，未声称具体业务比赛Nest实体及持久化装配已验收。

## 本轮实施结果

已完成共享头类型2、Go Command/Room当前会话权限、TCPSender、生成注册表/Notify/启动扩展、生成TCP raw推送与C#codec/Assembler/LockstepClient/Notify。业务接入示例、并发边界和限制见[client说明](../../client/README.md)，全仓失败交接见[验收与剩余问题](../review/REVIEW-2026-10-08-client-lockstep-validation.md)。新增代码保留必要中文注释，没有另写Sequencer/History或另建协议路由表。

| 执行 | 最终结果 |
| --- | --- |
| Go 1.27、GOWORK=off build ./...、vet ./...、go run ./cmd/glsvet ./... | 三项exit0 |
| dotnet build/run消费者 | netstandard2.1库零警告/零错误；7个Go金样与会话/Lockstep回归通过 |
| 新Command/Room权限/TCP快worker契约race | exit0，见command-green |
| Lockstep/wire/robot完整race三次 | exit0；保留此前一次既有KCP关停context canceled失败，不将其删除或改阈值 |
| 正式生成SDK与注册扩展定向 | exit0，100.196s；生成完整game-demo build/vet、三包race、实际TCP+C#、go generate托管目录字节不变；空业务工程hook执行/封存/失败不发布通过 |
| 全仓go test -count=1 ./... | exit1：153包，126通过/22无测试/5失败；根包47.067s通过。基线对照与具体失败见验收记录，不能称全仓绿 |

[日志证据（空白规范化）](evidence/client-lockstep-20261008)。没有运行引擎实机、公网UDP、HA/长期容量或真实确定性游戏。本次只是通用协议/消费能力；业务仍须实现比赛单所有者、Nest命令桥接、Tick驱动和真实状态Hash。无回复Notify不证明业务ACK。没有发版、部署或等待GitHub CI。
