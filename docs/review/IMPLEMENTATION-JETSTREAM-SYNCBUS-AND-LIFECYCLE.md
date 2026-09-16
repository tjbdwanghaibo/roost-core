# JetStream SyncBus：命名空间、确认与停止

09-16 第四轮（Core 3060817、Kit 527eecd）：当前一个 topic 共用底层消费者，本地 handlers 在锁内取快照、锁外依次调用，消息副本与 panic 隔离；最后退订者释放底层订阅。原真实广播/分片五场景全部通过。Prefix 进入非默认 durable 身份；仅 Core 默认 roost.sync 保留旧名，Kit 默认 roost.room 会改名，旧 ACK 游标不会自动继承。共享 Stream 的 Subjects/MsgID 所有权仍是声明限制，不能用名称分离证明共享配置安全。[验收和升级方案](../bug/REVIEW-2026-09-16-04.md)。旧基线分析保留在下文。

## 09-16 第三轮补充：真实消费与本地广播

本节基线 Core `21e0a6c`、Kit `7030c5f`；下文此前基线记录保留。[35 场景运行与限制](REVIEW-2026-09-16-03.md)。本轮用独立 NATS server v2.11.9 单节点文件存储验证了发布确认、ID 去重及窗口到期、ACK/NAK/Term、durable 续接、进程重启和连接恢复的有界场景。真实集群、断电及 TCP 半开未验证。

同一 Stream/Durable 的多个 Consume 是竞争者。本地对同一 JetStreamSyncBus/topic 两次 Subscribe 会建立这样的竞争关系，结果为 12 条消息分摊成 6/6；普通 NATS 对照为 12/12。两个独立 syncstream reassembler 随后只收到部分分片，identity 51 片、gzip 18 片全部确认/结算后，两边仍都没有 Packet。[RR-20260916-03](../bug/REVIEW-2026-09-16-03.md) 是本地广播问题；只把 Prefix 加入名称无法解决它。

建议保留稳定 broker 消费者，在 bus 内维护 topic 到底层订阅及本地 handler 注册表的映射，复用现有 IJetStream/订阅对象、mutex 和 once；锁内取得 handler 快照、锁外调用。单个取消移除一个 handler，最后一个取消才释放底层消费者。并发初始化、失败回滚、消息克隆、panic 隔离和 Stop 交错应一起定义，避免广播修复又引入共享可变数据或资源泄漏。此为实施方向，尚未修改实现。

确认的三个层次需要分别判断：发布返回成功说明这次 broker 发布调用成功；consumer ACK 表示这次投递被结算；业务完整 Packet/状态是否应用还取决于重组及 handler。真实重启实验中旧消息再次投递后仍能继续收到新消息，业务须容忍重复。人为在真实发布成功后返回一次错误，BufferedPublisher 重试生成新 delivery ID，接收端收到两次同一 Packet；broker 去重不能替代应用序号/epoch 或业务幂等。

Kit RoomMod 的停止优先选择上下文 stopper，否则调用 Stop；NatsMod 先停止业务 bus 再关闭 Assembly。当前只验证相关包既有测试，尚未验证完整 App 的依赖排序、关闭期间注册和半包恢复。已有 syncstream 历史/ACK/恢复工具可作为业务接入基础，但必须明确应用成功水位及恢复触发，不能因本轮 broker 测试通过就视旧缺口已收敛。

## 此前源码机制记录

基线 Core `1143f61ee79fb22ac54ce0c0d87dc5927c2806f0`；[运行与证据限制](REVIEW-2026-09-16-02.md)。以下区分当前源码行为与建议，不代表修复已实施。

## 数据流与责任边界

| 阶段 | 当前实现 | 接入时需要理解的边界 |
| --- | --- | --- |
| 构造 | `room/jetstream_syncbus.go:88` 规范化配置，EnsureStream 提交 Prefix.> | Ensure 是 CreateOrUpdate，涉及共享远端配置所有权 |
| 发布 | `PublishContext:116` 拷贝消息/Data，补 FromSid，JSON 编码，附带超时和 MsgID | 调用方消息不被改写；取消会透传，但失败返回不证明 broker 未接收 |
| 确认 | `nats/driver/jetstream.go:40` 调用同步 Publish，映射 ACK；room PublishConfirmed 复用 Publish | 确认不表示消费者已应用；普通 NATS 适配没有此确认能力 |
| 创建消费者 | room `Subscribe:147` 生成持久名称、按完整 subject 过滤；driver `Subscribe:62` CreateOrUpdateConsumer 后 Consume | SID/topic 稳定身份可复用游标，不能忽略 Prefix 或实例所有权 |
| 接收 | driver `jetStreamMsg:207` 复制数据/元信息；room 解码、过滤自身、调用 handler | 重复消息仍会交给 handler；包装层对错误只记录并返回 nil |
| 结算 | driver `settleJetStreamDelivery:92` 决定 ACK、NAK 或 Term | 业务是否被重试取决于到达 driver 的错误，不能只看 driver 支持 NAK |

显式 MessageID 优先；缺失时基于 topic/key/version/fromSID/part 生成 fallback，关键字段为零则不生成。分片 Part 必须参与身份，否则 broker 去重可能合并不同分片。去重窗口有界，消息身份不能替代业务幂等或跨进程的应用水位。共享 Stream 下的 Prefix 作用域还需与消息身份一起设计。

## 错误与恢复

driver handler 的 panic 转为 error；永久错误或达到 MaxDeliver 时 Term，其余按配置延迟 NAK 或立即 NAK。ACK/NAK/Term 调用失败会记录指标与日志，没有本地无限重试。是否仍可被服务端重投递，受消费者存在性、保留时间、MaxDeliver 和连接恢复影响，不能由日志文案保证。

room SyncBus 的 handler error 被吞掉，因此这条调用链最终选择 ACK；这是 SyncBus 的通知契约。本轮受控测试覆盖该行为，没有把它误报成新增故障。需要可靠业务应用时，优先评估已有 syncstream 的序号、历史、ACK 与恢复接口，明确何时记录应用成功、何时拉全量；这些工具的旧问题本轮没有验收，不应假定可直接获得端到端保证。

## 生命周期与所有权

driver 每个订阅单独持有 handler context。Stop 先取消 handler，再停 Consume；Drain 请求排空后等待 Closed 再取消。它们与总线 Stop 的含义不同：总线只对当前 subs 快照逐项 Stop，没有等待正在创建的订阅，也没有 terminal 状态。

测试控制底层 Subscribe 的返回时机，确认它可以在总线 Stop 返回之后登记。是否构成功能 bug 取决于上层停止契约；建议先定义终态关闭还是清理当前订阅。如果需要终态屏障，可在创建前后检查停止状态/代数，过期返回立即清理，再考虑等待在途操作。不要持有总线 mutex 等待网络创建，以免阻塞停机。

## 命名空间问题与实施方向

[RR-20260916-02](../bug/REVIEW-2026-09-16-02.md) 验证同 Stream/topic/SID 下不同 Prefix 生成相同 Durable、不同 FilterSubject。源码另显示每个构造器会提交单一 Prefix 的 Stream Subjects。修复应先选择一对一 Stream 所有权或共享 Stream 管理协议，再决定名称、迁移与配置校验；只扩大散列输入不能解决共享远端配置互相覆盖。

性能上，当前复制和 JSON 编码换取清晰的数据所有权，同步确认提供明确的发布结果边界但增加等待。没有测量前不建议为了减少复制直接暴露调用方切片；应先测分配、不同消息大小的确认延迟和背压，再决定是否引入受控批量发送。真实 broker 恢复和性能测试仍是后续工作。
