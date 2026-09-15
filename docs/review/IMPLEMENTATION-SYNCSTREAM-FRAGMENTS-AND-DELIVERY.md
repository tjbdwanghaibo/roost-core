# SyncStream：分片、重组和交付语义

Core b58e280；[第六轮证据](REVIEW-2026-09-15-06.md)。范围为 syncstream/publisher.go、syncbus/sync.go 和 room/jetstream_syncbus.go 的订阅包装；History/journal 尚未深入。

Publisher 检查 epoch、sequence 上界、期望 observer 和 payload 大小，将 Packet 序列化为 JSON，可选 gzip，然后计算原始 JSON 的 SHA-256。按 MaxFrameBytes 切片，每片携带独立 delivery ID、part/parts、checksum 和业务 topic/key/sequence。RequireConfirmation 只接受支持 PublishConfirmed 的 bus；它证明 broker 接受，不证明业务 handler 成功。

SubscribeWithOptions 按 topic/key/version/from/parts/encoding/checksum 聚合分片。重复的相同分片不增加 received；相同槽位不同内容令 assembly 作废。每个 assembly 有 bytes 限制，解压后还有 decoded bytes 限制；TTL 在后续多片 accept 时清理。收齐后先删除 assembly，再解码、验证信封/observer/payload，最后把克隆 Packet 交给 handler。

本轮通过 identity、gzip 逆序、重复/冲突、checksum、chunks/envelope/payload 和 encoding 守卫，以及确定时间推进的 TTL 测试。没有测量并发 assembly 数量上界或长期空闲内存；MaxAssemblyBytes 是单个 assembly 的限制，不是所有待拼装数据的全局内存预算。生产接入需按来源流量估计内存和过期扫描成本，不能把逐包限额理解为全局有界。

syncbus.Handler 文档明确错误不重试。JetStream sync 包装在 JSON 错误、自消息或 handler 错误时返回 nil；handler 错误会记录日志。这是当前设计契约，本轮没有启动真实 broker 验证 ACK 时序，不能将传输重投递能力当成业务重试保证。需要可靠业务处理时，应明确应用 ACK/History 重放或业务幂等恢复流程，下一轮继续查框架现有工具。

BufferedPublisher.Publish 是同步并按 MaxAttempts 重试；TryEnqueue 是显式异步有界队列并克隆 payload，Close 关闭队列并等待 worker。当前 Close 只等待队列 worker，源码未将同步 Publish 的在途调用纳入 WaitGroup；本轮没有验证这一交错，暂列后续。OnError 也是业务回调，超时与 panic 行为不能由队列容量保证。

本轮重试成功、耗尽及关闭后拒绝均通过；尚未做重组进程重启、真实消息重复投递、journal 恢复和全部并发交错，不标记 syncstream 完成。
