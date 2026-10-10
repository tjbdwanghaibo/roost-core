# Gate 与 Game 接入

v1.25.0已实现嵌入 Game 的 TCP 与独立 Gate 两种正式接入。两者共用 [TCPServer](../../infra/network/gateway/tcp.go)，业务都进入生成的 ProtocolRegistry → Nest Sender → Nest → Entity/DataEngine。新增独立接入随v1.25.0交付；本机性能和跨机验收边界见[功能验收](../release/v1.25.0-VALIDATION.md)与[性能报告](../performance/GATE-AOI.md)。

## 1. 职责与目录

| 位置 | 责任 |
| --- | --- |
| `infra/network/gateway` | TCP、鉴权边界、绑定、续期、有界转发/出站、广播、排空；不依赖 Nest/Entity/Service/Wiring |
| `wiring/gate` | App singleton、Account、发现、正式 dispatcher、Sync/Lockstep 的便捷接线 |
| `codegen/internal/roost` | 生成协议和配置、嵌入 TCP Mod、Game `NewIngressMod`；不生成另一份 TCP 运行循环 |
| `service/session` | 副本/试炼等运行记录，与 socket、Gate Binding、Sync SessionID 无关 |

独立部署的链路是客户端 RS v2 → Gate TCP → Core NATS → Game Ingress → 正式业务 handler。内部信封和默认 Bus/RPC 使用 MessagePack；PB、Sync、Lockstep 的客户端格式保持原样。Gate 不处理 Entity 业务，也不获取 Entity local 锁。

## 2. 正式接入方式

嵌入部署继续声明生成的 player TCP Mod。独立部署中，Gate 声明 `wiring/gate.NewGateMod(options)`；Game 声明生成的 `player/tcp.NewIngressMod(options)`，关闭该 Game 的本地 player TCP listener。两个 Mod 使用同一 capability 名，生成接线拒绝重复提供，不能在同一个 Game 同时装配两种入口。

`DefaultOptions()` 提供资源默认值。Gate 要明确设置 `TCP.Enabled=true`、监听地址及业务 `Ready`；Game 提供业务 `Ready`。Ready 只快速读取已维护的准入状态，不能每包执行 I/O。两端必须启用 App singleton，并依赖同一应用命名空间下的 NATS。

未提供 Authenticator 时，接线使用正式 Accounts.ValidateSession 校验签名 session 票据、PlayerID 与固定 Game SID。自定义 Authenticator 也必须返回可信固定路由；不是依据客户端自报 SID 随机选 Game。绑定阶段 Gate/Game 各自验证，后续数据包不重复查 Account。

使用发现时，两端设置 `RegisterDiscovery=true`，并装配 `etcd.NewEtcdMod(etcd.WithoutServiceRegistration())`。接入监听/订阅启动及 NATS Flush 完成后才登记候选；一个 Discovery 只有一个登记拥有者。Game 候选必须带 `access.incarnation` 与 `access.ingress_ready=true`。Gate 默认用 FixedGameResolver 只查固定 SID；也可以显式注入 ResolveGame。发现只是候选，真正的身份由 SingletonIdentityChecker 核对。

NATS ACL 必须限制每个进程的来源前缀和自己的 reply inbox。应用 payload 自报 GateID 不构成认证。正式权限配置和已测边界见[具体契约](GATEWAY-IMPLEMENTATION.md)；测试用私有 broker 和临时测试用户，不要求修改共享业务 broker。

## 3. 绑定与请求结果

完整绑定含 PlayerID、SessionID、Gate/Game SID 与 incarnation、连接 nonce、Game 签发 BindID。先 Bind，再物理写出 AuthACK，最后 Activate。OnActive/OnClosed 用 Game 分配的不复用数值 receiver 建立和关闭 Sync/Room 生命周期；旧关闭、旧出站失败不会解绑新连接。不同 SessionID 可属于同一玩家，没有框架隐式全局单点登录。

Gate Forward 只有一次传输尝试。预算包含 Gate 等待、NATS、Game 等待与正式 dispatcher；控制请求也有条目/字节上限和固定 worker，排队消耗原预算。

- `NotAdmitted`：确认未交给 dispatcher。
- `Completed`：内部执行及本地出站已准入；不是落库确认或客户端业务处理确认。
- `Unknown`：已交给 dispatcher 后的失败/超时，或者丢失回执；不能自动重放扣款/发奖等写请求。

## 4. Sync、Lockstep 与广播

`wiring/gate.Transport` 把 Entity Sync 和 Lockstep 接到 Game 同一个有序出站口。数值 receiver 发送时冻结完整 binding。Sync 保持原 profile/AOI/epoch/版本与 handler 完成后的 on_change + 20Hz 兜底；setter 只标脏。Push 成功只表示本地准入，异步下游失败关闭对应完整旧 binding 和 Sync lifetime。

Room 的 Attach/Detach/Input/Hash/Tick/Catchup 仍走业务的单一 Room owner。迟到断线使用 `Room.DetachSession(player, receiver)` 条件解绑；不能在 NATS callback 直接修改 Room 或 Entity。首期两种 Lockstep 发送接口最终都经 Gate TCP，不提供 UDP/KCP 承诺。

`gateway.BroadcastSender` 支持指定玩家、指定 Game 在线连接，以及由 system 身份发起 AllOnline。Game 只能广播自己拥有的连接；通用广播仅为 PB push，不把不同客户端的 Sync delta 当通用广播。每 Gate 返回准入统计与 completed/not_admitted/unknown；同 BroadcastID 在有限窗口内返回首次结果，不重复投递，换内容复用 ID 会被拒绝。统计按连接，包含同玩家多会话，不构成精确全局在线玩家数。

PB、Sync、Lockstep 共用每绑定 OutSeq 和有界出站，Gate 检查连续序号并复用唯一 socket writer。按完整协议包发送，不按字节截断 Sync 帧。重复只确认已准入前缀；缺口或持续发送失败关闭连接，重连后恢复全量/追帧。

## 5. 停机、指标与验证

停止先关闭准入，再等已接纳 dispatcher、回调与出站真实排空，随后注销发现和关闭 TCP。超时保留实例及依赖，新 context 可继续等待；不把取消等待当事务已经停止。

`gate.channel.result_total` 与 `gate.channel.failure_total` 使用有限 role/phase/result 标签；Stats 返回当前绑定和驻留预算。不要把 PlayerID、SessionID、incarnation 或票据加入指标标签。

已验证正式生成 PB/Nest/双实体 DAO/文件 WAL/Sync、实际 NATS 双 Gate 双 Game、Lockstep 输入/追帧/重连、旧事件隔离、ACK 丢失、广播去重、控制容量与预算、真实 ACL、Stop 重试。测试使用本机隔离资源，鉴权权威、存储和进程范围按[功能验收记录](../release/v1.25.0-VALIDATION.md)逐项说明；不声称已完成独立多进程、真实 Mongo、Linux 实机或跨机完整矩阵。

完整配置、协议、阶段验收和未验收项见[设计与实施契约](GATEWAY-IMPLEMENTATION.md)。目录升级没有旧 import alias，旧工程必须重新生成并编译；稳定基线历史性能不能替代新增 Gate 的结果。
