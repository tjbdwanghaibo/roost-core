# EntitySync：订阅、批次准入与持久化

审查 Core faf4631，仅描述当前机制和建议。

SubscriptionCoordinator 把 membership 独立于实体内容保存。Subscribe 按 subject 分片锁串行操作，先登记 pending，再 CaptureSnapshot 和 sink 准入，成功后 active；失败恢复旧订阅或删除新记录。profile 改变也走快照路径。Unsubscribe 先 closing，Leave 准入成功后移除，失败恢复 active。

DistributeBatch 收集更新并按分片编号排序加锁，读取订阅 profile，构造每接收者的信封，ReservePreparedSubjectSyncBatch 之后一次准入所有信封，成功后提交实体内容并推进订阅 ContentVersion。ReliableEnvelopeSink 必须整批接受或失败，具体 session 序号、历史和运输属于 sink 的职责。

FlushSubject 先以 LastCommitLSN 对比 durableWatermark；未持久化则跳过并保留 dirty。当前 Subscribe 与直接 Distribute 路径没有相同门槛，RR-20260915-03 已在三入口复现。持久化是内容版本的性质，不能只附着于一个便利方法。

资源/性能：64 个 subject 分片锁避免同主体操作交错，但不同主体可能落在相同分片；锁覆盖 sink 准入，慢 sink 会扩大阻塞范围。批量锁排序有助于统一获取顺序；同 profile 的预备 payload 可复用给多个订阅者。当前没有压测或字节开销测量，不把这些源码成本分析当性能缺陷。

下一步检查失败重试、profile 交接、取消和 sink 生命周期，以及带实体锁的 LSN 捕获一致性。[本轮证据](REVIEW-2026-09-15-03.md)。
