# SyncBus：Patch 应用与 Delivery 身份

Core b4bf09e；[09-16 实测](REVIEW-2026-09-16.md)。本轮覆盖 PatchSyncer 和 DeliveryIDs，不声称真实 broker 端到端已验证。

PatchSyncer.Start 验证 bus/topic/KeyOf/Apply，互斥保护一次订阅；Stop 执行并清除 unsubscribe，可再次 Start。订阅函数和 unsubscribe 都在自身 mutex 内调用，宿主不应假定任意回调重入 Start/Stop 安全，本轮未测重入或阻塞 unsubscribe。

Publish 跳过无数据 patch，验证 key，JSON 编码后分配独立 MessageID。支持 IContextPublisher 时传递 context，否则发布前检查取消，再调用普通 Publish。接收路径跳过 nil/零 key/空 data 和同 LocalSid 消息，解码并验证/补齐 key，再执行 Apply(context.Background(), patch)。上游传输取消不会自动成为 Apply 的 context；需要宿主定义停止与超时边界。

配置注释明确这是 transient patch，没有 store/delete/stale 语义。实测相同 delivery 或更旧业务版本仍调用 Apply；Apply 的 error 向 Handler 返回，不能推导 broker 将重试，前轮已记录 syncbus Handler 的错误不重试契约。业务若不是天然幂等，需要自己关联业务操作 ID、状态版本或重放机制。

DeliveryIDs 每实例随机前缀加原子计数，同业务版本每次 Publish 仍有新身份。它避免不同发布实例的 broker 去重键碰撞，但无法自动判断“这次重试是否同一业务操作”。本轮多 minter 512 次并发和同版本连续 Publish 对照通过，没有把样本测试当绝对唯一性证明。

性能：JSON 编码/消息分配发生在调用线程；容量、持久化确认、网络重连是 bus 具体实现的责任。下一轮继续具体传输实现；本轮没有生产吞吐、尾延迟或真实 broker 去重窗口测试。
