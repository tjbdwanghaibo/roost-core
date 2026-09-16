# JetStream SyncBus：命名空间、确认与停止

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
