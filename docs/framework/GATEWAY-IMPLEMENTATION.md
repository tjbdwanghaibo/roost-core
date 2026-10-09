# 独立 Gate：设计评审与实施方案

状态：**目录重构已提交，Gate 实施中**。日期：2026-10-09；起始基准 `ef640e6e`，目录提交 `1b09cfb0`。TCP 共用运行实现、MessagePack、原始 NATS 能力已落码；完整独立 Gate 尚未交付。分阶段实际结果见 [实施验收](../maintenance/DIRECTORY-GATE-VALIDATION.md)。本轮先目录后 Gate，具体分类见 [目录方案](PACKAGE-REORGANIZATION.md)。

## 1. 评审结论

原设计方向合理，可以作为开发基础：连接和鉴权放在 Gate；业务协议解码、Nest 调度、Entity、DataEngine 留在 Game；固定 ServerID 路由；沿用 RS v2、Entity Sync 和 Lockstep 格式；断线、取消和写入结果分开处理。暂不建设全局在线目录、动态迁移或 WAL 热迁移，符合当前需求。

原文已经提示以下风险，但还没有固定实现契约。它适合作为架构说明，不能直接当成接口规格开工。建议按本方案补齐后实施，不需要重写核心三大模块。

| 必须细化的地方 | 当前源码事实 | 实施决定 |
| --- | --- | --- |
| 转发并发 | Bus RPC 按方法名散列到 worker，handler 在该 worker 内执行 | 玩家数据通道不直接包装成一个阻塞的 Bus `Forward` 方法；使用现有 NATS 连接上的专用、有界字节通道 |
| 预算传递 | servicerpc 保留调用方 deadline；Bus 接收端 context 来自本进程生命周期，未恢复调用方期限 | 新信封显式携带期限和剩余预算，Game 入队时开始计时，不能出队后重新获得完整预算 |
| 封装和大小 | Bus 原默认 JSON；本轮已迁为 MessagePack，正文和信封保留分层 | Gate 内部信封与 Bus RPC 统一采用 MessagePack；启动检查实际 NATS 载荷上限，计算完整包大小 |
| 绑定代次 | Principal 有 PlayerID/SessionID/Claims；进程身份可复用 App SingletonIncarnation | Game 签发随机 BindID，完整匹配 Gate/Game incarnation；不使用客户端递增值获得接管权 |
| 发送承诺 | Entity Sync 的 Push 成功表示已准入；AsyncTransport 后续发送失败使队列失败 | Game 本地准入、Gate 准入、socket 写出分别计量；后续失败关闭对应绑定与 Sync lifetime，不能继续发增量 |
| 阶段顺序 | 原 P4 才集中处理资源限制 | P2 就提供最小的条目、字节、在途限额和发送期限；P4 做故障矩阵与调优，不能先上线无界原型 |

这些是新能力的实施约束，不是现有嵌入式 TCP 已存在上述业务缺陷的结论。默认 NATS RPC 重试次数是 1；不能把“驱动支持配置重试”误写为“当前默认会重复执行”。

## 2. 正式链路与目录

```text
客户端 RS v2
  → gateway TCP：连接、包头校验、鉴权、心跳、限流
  → Gate Binding：可信 PlayerID / 固定 ServerID
  → NATS 定向字节请求
  → Game Ingress：来源、代次、预算、载荷类别检查
  → 现有 ProtocolRegistry.DispatchPayload
  → 现有生成 Nest Sender → Nest handler → Guard / DataEngine
  → Game 每绑定有序出站 → Gate 每连接有序出站 → 客户端
```

Gate 不注册游戏 controller，不持 Entity，不运行业务 Nest。Game 接入适配器实现现有 `gateway.Session`，因此 ProtocolRegistry 可以继续使用当前分发接口。所有业务仍进入本地正式 Sender；不能让 Gate、NATS callback 或接入工作协程直接拿 Entity local 锁。

Nest 快慢池保持现有契约：快池执行 handler；慢池只做允许的前置 I/O、Remote 获取/释放与等待。转发等待不能占用业务快池。Gate 的网络执行器属于接入基建，不是第三种 Nest 业务池。

| 位置 | 拟定改动 | 边界 |
| --- | --- | --- |
| `infra/network/gateway/` | 按 TCP、session、binding、packet、ingress、outbound、NATS adapter 分文件，放运行逻辑和内部信封 | 同一个基础设施包；包含绑定、续期、收发、广播和排空；不依赖业务生成物、Nest、Entity、Wiring |
| `wiring/gate/` | Gate Mod、Game ingress Mod、Account/发现/App/dispatch/Sync/Lockstep 接线 | 便捷接入两端；配置、依赖注入与生命周期委托；不持运行队列、不实现收发/续期循环 |
| `infra/network/nats/`、其 `driver/`、`wiring/nats/` | 增加最小可选能力：带 context 的原始请求、实际载荷上限及接收缓冲限制 | driver/runtime 实现能力，Wiring 负责接上现有连接及启停；不建立第二套总线或持久消息系统 |
| `infra/network/bus/`、`infra/network/servicerpc/`、RPC 生成接线 | MessagePack Codec、默认装配、请求/响应信封及真实 RPC 验收 | 普通结构体直接编解码；RPC 路由、业务错误与持久传输契约继续保持 |
| `codegen/internal/roost/` | 服务目录、配置声明、Gate/Game 装配和 TCP 生成接线 | 生成器保留业务注册、编码器和配置映射，通用 TCP 只保留一份运行实现 |
| `framework/sync/` | 接线既有 Transport、SessionLifecycle、错误回调及帧大小上限 | 原帧、profile、AOI、on_change 和 Lockstep 恢复协议继续复用 |

表中为当前源码位置，目录分类已实施。已有 gateway 搬迁与新 Gate 均纳入本轮，先验收目录/领域/Wiring 重构，再实施 Gate 新行为；目录完成不代替本文 P1～P5 的 Gate 验收。

Wiring 延续 Wiring 的便捷接入初衷，业务声明 Mod/option 后由 App 接好初始化与关闭；模块自己拥有运行状态。Gate runtime 的认证、发现、incarnation 核对和业务分发依赖显式注入，不能为了取得 Registry 能力反向 import framework/app 或 wiring。此轮不另建 service/gate 领域包；连接会话仍归 gateway，service/session 管理的是副本/试炼等运行记录。

先完成 TCP 提取，保留嵌入 Game 与独立 Gate 两种正式部署方式；它们共用同一 TCP 核心，分别接本地 dispatcher 和远端 forwarder。这是部署选择，不是旧实现兼容分支。新的可配置部署枚举拟为 `embedded` / `gate`，默认保持当前 embedded；Game 的 ingress 单独显式声明。具体配置位置沿用现有 player access 声明，生成器拒绝冲突组合。

通用 TCP 的 dispatcher 输入和输出均使用原始 payload、MsgID、Seq、PayloadKind。业务 PB 编码和 Response 类型保留在生成接线中；Response 通过 TCPReply 显式交付已编码字节；不能为了提取 TCP，把生成的 ProtocolRegistry 反向引用进 gateway。

## 3. 绑定：拥有者、身份和生命周期

### 3.1 认证和定向路由

Gate 用真实 Account.ValidateSession 或应用鉴权器得到 PlayerID、SessionID、固定 ServerID；在装配边界转成显式可信 Route，避免每包重新解释 Claims。原始票据只用于绑定认证，不进入日志、指标或后续数据包。

发现仅查找指定 Game ServerID 的可用实例，绝不轮询选择其他区服。生产两端启用现有 App singleton；Bind 校验当前持锁进程身份和 ingress readiness。现有 SingletonLiveness 只有 sid 活性，不能把它当成远端 Token 校验；实施时补最小的 incarnation 匹配能力，由 App 继续拥有锁存储。发现只携带地址/代次候选，不授予实体写权。

Game 在 Bind 阶段校验真实票据及固定 ServerID，并确认自身已具备安全业务准入条件。普通业务包只引用已确认的绑定身份，不再每包访问 Account。App 失锁沿现有 fail-stop 规则停止准入；Gate 不把请求自动切换到另一个 Game。

### 3.2 身份与状态

绑定身份包含：`PlayerID`、`SessionID`、`GateID`、`GateIncarnation`、`ConnectionNonce`、`GameID`、`GameIncarnation`、`BindID`。字符串/令牌长度必须有限制。ConnectionNonce 在 Gate 接受连接时随机生成；BindID 由目标 Game 在确认绑定时随机生成。外部 Seq 只关联该连接的请求响应，不用于业务去重。

Gate 持有 socket 和本地连接表；Game 持有绑定表与已认证 principal；Game BindID 是两端连接关系的权威标识。Sync 的数值 SessionID 在 Game 按绑定分配，旧生命周期退出前不得复用。保留不同 SessionID 属于同一玩家的能力，不引入全局单点登录。

```text
Gate：accepted → authenticating → binding → active → draining → closed
Game：binding → active → closing → closed
```

同一 ConnectionNonce 的 Bind 重试只能返回同一结果；Unbind 后不能用迟到 Bind 复活。终态记录按控制请求有效期、续期窗口和时钟余量保留并有总量上限；过期请求直接拒绝，不为防重放无限保存历史。相同 SessionID 的替换只能在 Game 原子更新绑定时完成，并在外部放行业务前结束；旧 Unbind/Kick/发送错误只关闭自己持有的 BindID。

先 Bind 成功，再发外部鉴权 ACK。初始绑定可以存在，但 Sync session 保持 held；Gate 的 ACK 写出并准备接收业务后发送 Activate，才允许 Game 主动推送。Activate 与 ACK 丢失、重复、超时都需幂等；未激活绑定到期清理。需要 EnterGame 才建立订阅的业务继续走原正式 handler，不在 Bind 里偷偷执行登录业务。

### 3.3 续期和断线

Bind/Renew/Unbind/Close 都完整匹配代次。Gate 与 Game 用有限租期续期，控制消息有独立小额容量，不能被大量业务包挤掉。租期参数在 P1 配置规格中固定，并通过暂停/断网测试验证；本方案不把任意 TTL 当成已测故障恢复时间。

断线通知用于加快释放；Game 定时核对绑定租期及进程身份，补偿通知丢失。会话退订通过正式 Sync lifecycle 与必要 Nest 调度完成，不能在通知协程修改 Entity。租期失效阻止新的业务准入，不撤销此前已经接纳的事务。新进程/新连接重新认证、Bind 并建立新 Sync epoch；Lockstep 通过现有追帧入口恢复。静态 Player 的搬机仍以落库数据为准。

## 4. 内部通道、预算与执行结果

### 4.1 传输选择

首期采用 **Core NATS 的非持久 request/reply**。复用当前 NATS Assembly；Gate 通道固定单次传输尝试，不受普通 servicerpc 的 JetStream 配置切换影响。客户端写请求不经 JetStream 自动重投，不用共享 service queue 随机选 Game。

维护者已确认首期使用 NATS；性能验证包含实际请求/响应、Sync、Lockstep 和广播，不以空载 Publish 吞吐替代端到端结果。

数据通道增加一份显式版本的 MessagePack 二进制信封，原业务字节放在 `[]byte` 字段；不经 JSON/Base64 双层编码。只接受当前内部版本，未知版本在业务前拒绝，不新增旧格式解码分支。

维护者已选择 MessagePack，并要求内部 Bus RPC 使用相同编码。首期使用 `vmihailenco/msgpack/v5` 对普通 Go 结构体直接编解码，无需 proto 文件、生成方法或必填 tag。默认采用结构体 map 形式，使用导出字段名；现有 JSON tag 不作为 MessagePack 的隐式重命名规则。字段改名属于内部协议变化，配套更新两端和版本。数组模式虽然可省去字段名，但将声明顺序变成协议，本轮不默认启用。

统一的 Codec 负责紧凑整数策略、字节所有权和解码边界，不能让不同调用方各自设置 Encoder 参数。Encoder/Decoder 不在多个 goroutine 间共享可变状态；复用时在 Reset 后重新应用一致策略。反射编解码不代表每次都需要完整扫描字段，但也不能据此宣称性能必然超过 CBOR/Protobuf；以实际类型的 ns/op、B/op、allocs/op 和编码字节数确认。

Bus 的请求体、NatsMsg 外层信封、成功响应体和成功/失败信封全部经过同一个 MessagePack Codec，`[]byte` 使用二进制值，去掉 Base64 开销；servicerpc 和生成 Server/ClientMod 使用相同默认装配。先保留有意义的体/信封分层，不能为减少一次 Marshal 弱化业务错误和传输错误隔离。Bus 共用 Codec，因此调整默认值还会影响普通异步 Bus 消息，验证必须覆盖该路径，不能只改 Gate 或 RPC 客户端。

现有 JetStream RPC 也使用 Bus Codec，需配套验证；改变编码不改变其重投、inbox、TTL 和准入规则。NATS/JetStream 自身定义的 JSON 控制消息仍按官方格式解析，例如 PubAck 识别，不是要删除的旧业务 JSON 兼容路径。Sync/Lockstep 及客户端 PB 格式保持原样，只改变内部信封。

这是一项内部线格式升级：请求和响应都必须有可校验的新格式/版本，两端、生成物与消费工程配套更新。按维护规则先排空需要保留的旧进程/旧持久队列数据，不新增 JSON/MessagePack 双读和自动猜测格式，不自行清空共享 stream 或业务数据。验收覆盖 int64 ID、枚举、nil/empty、嵌套类型、map、原始字节、时间和自定义编解码类型，避免把 JSON 行为直接当成 MessagePack 行为。

接收 callback 只检查总大小、合法来源和信封基本字段，再做有界准入；不能在 NATS callback 等 Nest。Game 每绑定最多一个业务请求在执行，绑定之间可以并行；并发数及总绑定数均有硬上限。等待任务只有取得条目、字节和在途额度后才能启动，完成后归还；不能按唯一 `Forward` 方法名把全部玩家塞进一个顺序 worker。

生产通道必须启用服务认证和 subject 权限。拟定 subject 包含目标 sid/incarnation 与来源 Gate 或 Game 的命名空间，部署给每个服务身份限制其可发布的来源前缀。接收方校验 subject 与信封一致；公开连接没有内部 publish 权限。NATS 当前 Msg 不暴露已认证发布者身份，不能仅在 payload 填 GateID 就声称已鉴权；实际 ACL 配置与伪造测试是交付内容。回信主题限制在约定 inbox 前缀，禁止请求者指定任意业务发布主题。

### 4.2 消息和结果

| 消息 | 关键字段/返回 | 责任 |
| --- | --- | --- |
| Bind / Activate | 进程身份、ConnectionNonce、可信认证证明、目标 Game；返回 BindID | 确认路由与绑定，控制重复和激活窗口 |
| Forward | 完整绑定、RequestID、MsgID、外部 Seq、PayloadKind、期限、剩余预算、原字节 | Game 校验后走现有 ProtocolRegistry 和 Sender |
| Outbound | 完整绑定、每绑定 OutSeq、response/push 类别、MsgID/Seq、PayloadKind、原字节 | Game 统一出站，Gate 完整包入队并返回准入 ACK |
| Broadcast | 来源进程/权限、BroadcastID、目标 Gate 代次、显式受众、MsgID、PB 字节和期限 | 每 Gate 一份广播，再按本地活动连接有界扇出；返回准入统计 |
| Renew / Unbind / Close | 完整绑定、控制请求期限、明确原因 | 幂等清理、到期恢复，不影响新绑定 |

内部结果显式区分 `NotAdmitted`、`Completed`、`Unknown`。Completed 只能表示业务按自身持久策略完成且必要响应已进入发送链路，不承诺客户端收到或 Mongo 已投影。业务错误使用现有业务响应协议；不能把所有错误都伪装成 NotAdmitted。

Forward 返回内部执行状态；客户端业务响应作为 Outbound 的 response 项进入同一条出站顺序，避免 RPC 响应与主动推送走不同通道而互相越序。无响应 Notify 只返回内部完成状态。外部 RS v2 暂不增加通用错误包：业务前失败/传输未知按现有关闭与重连行为处理；需要结果查询的写操作使用原业务幂等 ID 和查询接口。内部 Unknown 进入日志/指标，不能冒充客户端已识别这个状态。

本通道不承诺所有业务 exactly-once，不为非幂等 handler 自动补持久去重。默认不重发未知 Forward；重复输入验证用具备真实持久幂等能力的业务检查实际写入次数。

### 4.3 总预算

Gate 在完成帧读取并准备业务准入时记录本地单调计时与总 deadline；Gate 排队、发现、传输、Game 排队和 Nest 等待均消耗同一预算。信封携带绝对期限与发送时剩余预算，Game 收到后立即设置缩短后的 context，检查期限后才能进入执行。

跨机不能共享 Go 的单调时钟。两端需要有界的时钟偏差配置/监控，Game 对绝对期限扣除保守余量，并与剩余预算、接入最大预算取最小值；后续出队还要扣除本地等待。P5 记录实际时钟偏差；超过允许范围拒绝新绑定/请求，不能仅传一个 remaining_ms 使网络停顿获得额外预算。socket 读半包的超时由 TCP 读期限单独约束。

Game 能证明未进入 Nest 时才返回 NotAdmitted；Nest 已接纳后超时或回信丢失归入 Unknown，已接纳事务沿原契约提交/恢复。连接取消停止等待，不能证明远端未执行。绑定换代也不能撤销旧代已接纳的扣款操作。

## 5. 统一发送与 Sync 保证

Game 每绑定使用一个有界有序出站口，所有 PB 响应/主动推送、Sync、Lockstep 进入这里。Game 分配 OutSeq 与队列准入同在一个临界区；Gate 只接受匹配 BindID 且顺序连续的包。重复 OutSeq 不重复写，缺口或超时关闭该绑定，不猜测可以跳过哪个包。不同生产者的入队先后是传输顺序；不额外承诺“业务响应永远早于该 handler 引起的 Sync”。

Gate 每连接只有一个 writer，鉴权 ACK、心跳 ACK 和 Game 出站包共用它。Game 返回的 OutSeq 不作为 RS 外部序列：PB 响应保留请求 Seq，推送序列由 Gate writer 在写出顺序中分配。任何一包写出部分字节后失败都关闭 socket，不按字节截断 Sync entity 包继续发。

复用 `nettransport.AsyncTransport` 的可靠队列、字节预算、最大年龄和失败回调。Wiring 通过显式队列接口注入现有实现并转换 SessionID；gateway 不反向 import Framework，适配器不另持队列或复制容量/生命周期状态机。Game 本地队列成功准入后，Sync 可以按现有规则推进帧状态；后台发送必须得到 Gate 有界队列准入 ACK。NATS Publish 返回 nil 只说明本地发布调用成功，不能直接作为 Sync Push 的可靠交付结论。

Game→Gate ACK 丢失可能已入队：不自动重放写 socket 的包，关闭旧绑定并通过新 epoch 恢复。Gate 入队后 writer 失败、队列过龄、进程退出时通知 Game 关闭该 lifetime；通知失败由租期/核对兜底。旧失败回调不能关闭新 lifetime。Manager 的错误和 SessionLifecycle 接线必须实测，不只增加一个日志回调。

队列满不能采用“丢一个 Sync delta、继续发送后续 delta”。可靠 PB、状态帧、Lockstep 帧均按完整包准入；拒绝/后续失败进入相应关闭、全量或追帧恢复。临时基础设施不可用可在未准入时用原 ErrRetryLater 保持 pending；一旦旧链路连续性未知，就必须终止该 lifetime。

on_change 保持现有正式边界：setter 标脏 → handler 汇总变更 → Guard 内冻结 → 释放全部锁并满足提交水位 → 唤醒 Sync → 出站。20Hz 仍是兜底，不因增加 Gate 改为每个 setter 发包。任何 NATS/socket 写等待都在 Guard 和 Nest 快 worker 外发生。

### 5.1 面向客户端的广播

**广播属于首期交付范围。** Gate 提供发送能力；业务决定广播内容、权限和受众，不在 Gate 执行公告规则、维护游戏房间或计算 AOI。

拟定业务入口为 `Broadcast(ctx, audience, messageID, value)`，生成接线负责把业务 PB 编码一次，再向目标 Gate 发送冻结字节。低层入口接收有大小限制的 immutable payload；这些均为拟定 API，不是当前已存在接口。

| 受众 | 首期语义 | 路由 |
| --- | --- | --- |
| AllOnline | 当前应用/部署命名空间内全部已激活连接 | 发送到本次发现的全部就绪 Gate |
| GameOnline(ServerID) | 固定区服的已激活连接，可分布在多个 Gate | 首期向就绪 Gate 扇出，各 Gate 按可信绑定 ServerID 筛选 |
| Players(PlayerIDs) | 指定玩家的全部已激活会话 | 去重玩家列表、限制目标数并分批；首期由各 Gate 查本地连接表 |

首期不建设全局在线目录；Players 按全部 Gate 查找允许多花少量控制投递，避免为广播引入每消息中央查询。房间/队伍/公会广播由业务提供明确玩家集合；Game 可按已持有的绑定位置合并到 Gate。需要严格绑定代次或会话范围的调用使用显式 binding 目标，不用 PlayerID 列表猜测旧连接。

每个 Gate 接收一次编码后的广播，快照本地匹配的 active binding，然后按有界批次准入各连接的统一发送队列。快照后新连接不补发本次广播；快照中的旧 binding 关闭/替换时跳过，不能把其投递转到新连接。不同 Gate 的受众快照时刻可能不同；发现后新加入的 Gate 不在这次目标集合内，不承诺集群原子在线快照。

发送到具名 GateID/incarnation 的 NATS subject，逐 Gate 收取准入结果；不能用 queue group 将广播只分配给一个 Gate。发送侧并发 Gate 数、待发广播数/字节、Gate 扇出任务数、快照目标数及去重记录都有硬上限。一个慢连接失败不阻塞或撤销其他连接已经成功的准入；拒绝时报告失败数，持续写阻塞按统一策略关闭该绑定。不为每个目标创建无界 goroutine，也不对每个客户端重新编码 PB。

Gate 返回每次本地快照的 `matched/accepted/refused/stale` 计数；发送侧还记录目标 Gate 的 `acknowledged/rejected/unknown`。已准入不等于 socket 已写出，更不等于客户端已处理；部分成功必须显式返回，响应丢失不能把未知当成零投递。只统计 confirmed Gate 的计数，不能把所有 Gate 汇总成精确的全局在线玩家数；同玩家多会话按连接计数。

BroadcastID 由来源进程代次和唯一请求身份组成，同 ID 的重试内容/受众必须一致。Gate 在有界有效期内保存首次准入结果，重复请求返回原结果，不再次投递成功或失败目标；换 ID 表示新的广播。请求过期直接拒绝，去重容量满时先拒绝新任务，不能淘汰仍有效的记录来冒充幂等。Gate 重启不持久保存这些记录，因而不承诺跨重启 exactly-once，也不自动重试未知广播。

来源权限与受众一起检查：Game 默认只允许广播自己的绑定区服/玩家；跨区服 AllOnline 由明确授权的系统服务发起，并限制在当前应用命名空间。客户端不能通过普通 Forward 调用内部 Broadcast，也不能靠填写 ServerID 获得其他区服的广播权限。

通用 Broadcast 首期只接受 **PB push**，与其他输出共用 socket writer。它在 Gate 准入时确定与其他包的先后，不参与 Game 的 OutSeq 计数，也不承诺不同广播来源之间的全局顺序；有业务顺序要求的消息由 Game 通过原每绑定有序出站口提交。

Sync 的 profile/AOI/基线和每绑定 epoch 不同，不能把同一份 delta 广播给任意客户端。Lockstep 的房间帧可复用同一份 C7 payload，并按房间当前 binding 合并 Gate 批次投递，但必须保留每接收者的绑定身份、连续 OutSeq、准入反馈和追帧责任。该优化属于 P3 的 Lockstep 出站接线，不走通用 PB Broadcast；Gate 无须持有 Room 成员真相。

### 5.2 Lockstep 的具体接入位置

Lockstep 与 PB/Entity Sync 共用 Gate、Core NATS 和 MessagePack 信封，不另建 Lockstep Gate 服务。仍需在 P3 单独完成协议入口、会话映射、发送和恢复适配，不能仅因通用 Forward/Outbound 跑通就声明 Lockstep 已接通。

| 当前位置 | 当前责任 | 独立 Gate 的接线 |
| --- | --- | --- |
| [command.go](../../framework/sync/lockstep/command.go) | 原始 Command 编解码；HandleCommand 根据认证 session 处理 Input/Hash/Catchup | Gate 原样转发 flags=4 的字节；Game 校验 PayloadKind 后解码，完整 BindID 映射到当前数值 session，再进入 Room 的串行入口 |
| [room.go](../../framework/sync/lockstep/room.go) | 席位/旁观者绑定、输入收集、C7 广播、历史追帧 | Room 和成员真相保留在 Game；连接换代通过 Attach/旁观者入口更新，追帧复用当前 History/Catchup |
| [tcp.go](../../framework/sync/lockstep/tcp.go) | TCPSender 将实时和可靠通道接到 PushLockstepSession | 复用 sender 的 resolver/包大小/禁止快 worker 阻塞契约，将 push 接到 Game 的有序 Gate 出站；复用共同逻辑，不复制一份 TCP sender |
| [生成 TCP](../../codegen/internal/roost/render_player_tcp.go)、[ProtocolRegistry](../../codegen/internal/roost/render_access.go) | flags=4 上行和 flags=5 推送；显式注册 Lockstep Notify | Gate 保留 RS 头与类型校验；Game 保留路由注册；Gate writer 写出原始 C7/追帧字节 |

MessagePack 只编码内部 envelope，不重新编码 Command、C7 或追帧页。实时帧和追帧页首期最终均走 Gate TCP，有界发送且不采用只保留最新帧；不能因为 Room 的实时接口名叫 SendDatagram，就宣称 Gate 首期提供 UDP/KCP。

输入、Attach/Detach、Tick 按 Room 的现有串行拥有者调度；仅每玩家/每连接串行不足以保护多人共享 Room。Tick 驱动保持在快 worker/Entity 锁之外，出站对本地有界队列准入，后台任务承担 NATS/socket 等待；单个追帧者或慢客户端不能卡住全 Room 的帧推进。

断线处理先在同一个 Room 串行作用域内核对当前 session 是否仍对应待关闭的 BindID，再执行 Detach；现有 Detach 接受 player，直接用迟到旧断线调用会错误解绑重连后的新 session。重连后的输入、hash 和追帧也必须用新绑定身份，旁观者仍只具有追帧权限。

新增真实客户端 → Gate → NATS → Game Room 的集成测试，覆盖两 Gate/两 Game、实时帧、Input/Hash/Catchup、旁观者、慢追帧、重连与旧事件、发送故障后的缺口恢复。现有 [e2e_gate_test.go](../../framework/sync/lockstep/e2e_gate_test.go) 是 KCP loopback 的端到端验收测试；文件名中的 gate 不代表独立 Gate service，不能代替这次通路验收。

## 6. 资源、配置和停机

P1 固定以下配置的合法范围、默认值和内存计算；P2 实现保护，P4 用实际测量调参，不先填一个未经测量的“最佳 worker 数”。

| 资源 | 必须限制的维度 | 拒绝/收尾 |
| --- | --- | --- |
| TCP | 现有连接/IP/握手/包大小/速率及读写期限 | 超限在业务前拒绝，释放连接与字节 |
| 绑定 | Game/Gate 总绑定数、未激活绑定数、控制包大小、终态记录数 | 绑定准入失败或过期释放 |
| 上行 | 每连接在途=1、排队条目/字节；每 Gate 总额；每 Game 分额 | 有界拒绝；一个 Game 故障不能占满其他 Game 的所有额度 |
| 下行 | 每绑定及进程的条目/驻留字节/在途字节/最大年龄 | 未准入明确返回；已准入后失败终止绑定，不静默丢包 |
| 广播 | 发送侧与 Gate 的任务/字节/并发上限、目标快照数、有效去重记录数 | 部分成功和未知显式返回；单慢连接隔离，不能重复放大广播或无限驻留 payload |
| NATS | server MaxPayload、客户端 pending 条目/字节、重连缓冲、inbox 数 | 启动校验；slow-consumer/订阅丢包触发该通道失败和恢复 |
| 控制面 | Bind/Renew/Close 专用小额预算和超时 | 不与数据面无限争抢；耗尽同样有界失败 |

外部允许的最大完整业务包必须能装进内部信封；限制计算包含最长合法身份字段、字段名和 MessagePack 开销，并对实际编码长度复核。NATS 上限不足时启动失败，配置或运行日志给出需调整的值；不能暗缩对外包上限。Sync 的 MaxFrameBytes 适配器必须传播有效上限；按完整 entity sync 包分帧，不按裸字节切开。

停止顺序：标记 ingress 不再接纳 → Gate 停新握手/新业务 → 在共同预算内等待已接纳操作和可发送结果 → 关闭绑定/socket → 退订通道 → 等实际在途任务退出 → 释放 NATS/其他依赖。排空超时不能先销毁仍被事务或发送任务使用的依赖。readiness 与 singleton 活性分别使用，不能因为仍持锁就判断可以接纳新玩家。

指标按阶段区分 Gate 排队/转发、Game 接入/Nest 等待、Game 下行准入、Gate 入队/socket 写出；记录字节水位、最老队列年龄、拒绝/未知/旧代拒绝/续期失败/发送失败/恢复次数。PlayerID、SessionID、BindID 不作为 metrics label，个体关联放受控日志/trace。

## 7. 分阶段实施和验收

| 阶段 | 实施内容 | 必须完成的证据 |
| --- | --- | --- |
| P1 契约与 TCP 提取 | 本方案落接口、MessagePack 结构体信封与 Bus RPC Codec、配置和广播受众/准入结果；通用 TCP 提取；明确 ACL/生命周期 | 编解码语义与格式拒绝测试、Core/JetStream RPC 错误及异步消息回归；当前 TCP 生成工程回归；依赖无环；没有两份 TCP runtime |
| P2 正式 PB 链路 | Gate/Game Mod、鉴权 Bind/Activate、单 Gate/Game 双向 PB、本地 PB 广播、最小有界发送与限额 | 独立进程真实登录、读和持久写；拒绝不写、提交后超时不盲重放；全在线/区服/指定玩家广播正确，慢连接不阻塞另一连接；embedded 回归 |
| P3 会话与同步 | 两 Gate/两 Game、跨 Gate 广播、续期和身份核对、完整 Sync/Lockstep 接线 | 固定路由、旧响应/Kick/Unbind/异步错误隔离；广播无跨区误送、同 ID 窗口内不重复；重启和通知丢失后收口；全量/追帧恢复；混合类型顺序 |
| P4 故障与排空 | NATS 缓冲/订阅故障、ACK 丢失、慢 Game/连接、广播部分失败和未知、排空、监控 | 每项上限实际触发且资源释放；不同 Game 隔离；部分写失败不续流；广播与旧绑定/重连竞争正确；事务结果与网络失败分开核验 |
| P5 性能与跨机 | 正式生成消费工程、macOS/Linux、loopback 与实际跨机分开验证；MessagePack 真实包和 RPC 基准；混合负载叠加广播突发 | 记录编码耗时/分配/字节数与端到端完成率、硬件、RTT/时钟、CPU/内存/带宽、延迟长尾及最终状态；广播不能使正常业务无界积压 |

性能沿用实际业务基线：1000 玩家、10000 Entity、每玩家约 50 个可见实体、每玩家每秒 10 条普通消息；全部 Entity 挂 heartbeat，分别验证 1Hz/10Hz，变化率 1%/5%，on_change + 20Hz 兜底。Saga 代表占比约 0.2%，Remote 80TPS 作为独立已够用负载，不用 Remote/Saga 的 TPS 替代普通消息吞吐。

先做各档短预检，再完成一档具名代表负载 1h 稳定性；同时跑具名故障矩阵和独立吞吐上限测试。比较同环境 embedded 与 Gate 链路的新增成本，不把以前的 80TPS/50ms 结果直接当 Gate 结果。

广播额外验证 1000 连接的全在线突发、跨两个 Gate 的指定区服/玩家、多会话、断线替换、同 ID 重复、ACK 丢失和一部分客户端不读。基准记录受众规模、PB 大小、广播频率、实际 accepted/received 数及广播期间普通消息/Sync P99；具体频率和大小作为具名测试参数，不把一轮全服公告吞吐当成持续广播容量。

Sync 的 P99≤50ms 从业务计划输入/handler 完成等起点分别记录，到客户端实际解码并应用完整更新结束；明确包含 Gate、排队与网络。计划输入指标包含调度积压，不能只统计开始执行后的快速样本。保留最大值和 >50ms 数量，继续跟踪用户已接受的偶发长尾；HB 调度延迟、普通消息完成率、实体修改完成率、持久正确性单独统计。

新增行为按受影响包执行 build/vet/test/race；生成器变更重生成并编译、实际运行消费工程。接入与故障测试使用隔离资源，不向共享业务 Mongo/Redis 做注入。跨机未跑只能标记待验收，不用本机 loopback 代替。

## 8. 本次核查依据与限制

| 依据 | 核实内容 |
| --- | --- |
| [gateway.go](../../infra/network/gateway/gateway.go) | 可信 principal 与 transport-neutral Session；Endpoint 后必须接 Sender |
| [render_access.go](../../codegen/internal/roost/render_access.go) | DispatchPayload 接受 gateway.Session；类型检查先于解码；输出已编码 Response |
| [TCP](../../infra/network/gateway/tcp.go)、[会话](../../infra/network/gateway/tcp_session.go)、[Runtime](../../infra/network/gateway/tcp_runtime.go)、[生成接线](../../codegen/internal/roost/render_player_tcp.go) | 共用运行实现；读取/dispatch 每连接串行；同步写锁与期限；生成物仅配置、协议和 Mod 接线 |
| [Bus](../../infra/network/bus/bus.go)、[servicerpc](../../infra/network/servicerpc/client.go) | 方法名散列、同步 handler、接收 context、定向与轻量/JetStream 选择 |
| [MessagePack Codec](../../infra/network/bus/msgpack_codec.go)、[RPC 信封](../../infra/network/bus/rpc_error.go)、[NATS Mod](../../wiring/nats/nats_mod.go) | 当前默认 MessagePack，RM v1 / RPC v2；拒绝旧 JSON 及未知格式；PubAck 的 JSON 是 NATS 官方控制协议 |
| [NATS RPC](../../infra/network/nats/driver/rpc.go)、[重试策略](../../infra/network/nats/rpc.go)、[Assembly](../../infra/network/nats/driver/assembly.go) | 默认单次尝试、复用连接、关闭所有权；不是自动提供 Gate 身份校验 |
| [服务发现](../../infra/network/etcd/discovery.go)、[App singleton](../../framework/app/singleton.go) | Metadata、sid 活性与本进程 incarnation 的能力边界 |
| [Entity Sync Transport](../../framework/sync/entitysync/transport.go)、[Flush](../../framework/sync/entitysync/flush.go)、[异步发送](../../framework/sync/nettransport/channel.go) | 成功准入推进帧状态、失败恢复、队列和字节上限 |
| [Lockstep 输入](../../framework/sync/lockstep/command.go)、[Room](../../framework/sync/lockstep/room.go)、[TCP sender](../../framework/sync/lockstep/tcp.go)、[既有端到端测试](../../framework/sync/lockstep/e2e_gate_test.go) | 现有串行 Room、认证 session、嵌入式发送与 KCP 测试；跨 NATS Gate 接线仍待实施 |
| [当前设计](GATEWAY.md)、[性能基线](../maintenance/PERFORMANCE.md) | 当前已交付边界与真实业务规模 |

CBM 当前工作树项目为 `roost-core-gate-design-plan`，Tier 2，当前 generation `2026-10-09T08:17:42Z`，已刷新 Gate 新增文件；绑定、TCP、原始 NATS 与 Codec 材料路径 coverage 为 metadata_match，无记录缺口。图谱不是完整性证明，生成模板缺口仍按源码补证。docs 被策略排除，直接阅读。macOS 本机真实 NATS 和生成客户端证据见实施验收，不冒充独立 Gate 集群、Linux 实机或跨机性能验收。

MessagePack 选型依据：[库说明](https://github.com/vmihailenco/msgpack)、[编码选项](https://github.com/vmihailenco/msgpack/blob/v5/encode.go)。支持普通结构体、可选 tag、数组模式及紧凑整数配置；本文未引用第三方性能数字作为 roost 容量承诺。
