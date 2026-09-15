# Room：分层准入、dirty 重试和连接剔除

09-15 第六轮补充（Core b58e280）：SetDownstream 只换引用，没有迁移构造时注册的慢消费者回调。首次订阅前更换真实 sink 也会出现新 sink 剔除后 room 仍保留订阅，新增 RR-20260915-05。回调注册与 unregister 句柄属于 downstream 生命周期所有权，切换时需处理注册失败回滚、同实例幂等和旧事件隔离。[本轮证据](REVIEW-2026-09-15-06.md)。

两 room × 两 session 的 scoped release/ResetRoom/ReleaseSession/Close 四场景通过；阻塞回调的 Close 超时后可重试通过。ReleaseRoomSubscriber、ReleaseSession 与在途准入交错两个对照通过：等准入完成再删除基线。上述不覆盖跨 room 并发剔除及数值 session ID 重用后的旧回调。

审查 Core 4537a6d；[运行证据](REVIEW-2026-09-15-05.md)。以下区分当前机制和修复建议。

RoomBroadcaster.flushDirty 取出 dirty IDs，flushStateBatch 按稳定分片顺序锁定并 Prepare。已经准备成功的集合交给 SubscriptionCoordinator.DistributeBatch；结束后任何 PendingDirty 主体重新入队。准备失败的主体单独记错与重试，其余准备成功的主体仍可交付，所以调用方不能把一次 tick 当跨所有实体的数据库事务。

RoomEnvelopeSink.AdmitEnvelopes 按 room/subscriber 聚合信封，计算候选 Frame/SessionSequence，调用下游后才保存计数。一次拒绝不产生序号缺口。RoomTransportSink 解析 session、按 room 分片加锁，对新增/离开类帧复制 object refs 为临时 plans；编码成功后交给 AtomicBatchTransport，成功才保存 plans。delta 使用已有 refs，无基线则拒绝。ReleaseSession 清理所有 room 的该连接身份，数值 ID 重用前必须调用。

AsyncTransport.AdmitBatch 先验证并复制全部输入，再按 SessionID 顺序锁住相关队列；检查连接状态及每 session 累计可靠队列容量，全部通过后才统一入队。Reliable 是顺序队列，datagram 以流为单位保留最新帧；准入成功只代表传输层承担交付责任，不代表对端已经收到或应用。

默认慢消费者策略遇到带 session 的可靠背压后，先将 session 标死、删除基线、调用 RemoveSession，再筛除其帧重试其他连接。room 的订阅清理由异步通知调用 handleSlowConsumer → Unsubscribe 完成。这两段分别持有传输状态和 membership，必须可靠交接通知。

RR-20260915-04：剔除后剩余批次失败，helper 返回 nil events，调用方错误出口也不派发。后续重试跳过 deadSessions，room 的 dirty 可成功清空，但原订阅仍在。因此不能用“下次 tick 会重试”解释所有状态最终一致。

建议保留已发生副作用对应的 events，在释放 room locks 后，无论剩余批次成功或失败都交给已有 pendingCallbacks/worker；仍只在准入成功时提交 plans。不要持 room lock 同步调用 Unsubscribe，否则会引入重入锁风险。此方案未实施，Close/重复通知/跨房间共享 session 还需专门验证。

本轮对照验证失败后 dirty 重排队、计数无缺口、退役失败重试、拒绝不建立基线、队列拒绝不部分入队。未覆盖真实网络消费恢复、全部生命周期交错或生产性能。每层锁有不同分片粒度；大批次会持多个锁，需用真实负载衡量，不能由锁数量推断吞吐。
