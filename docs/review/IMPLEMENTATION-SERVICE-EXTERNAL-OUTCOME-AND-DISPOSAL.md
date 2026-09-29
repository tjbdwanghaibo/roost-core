# Service 外部结果、删除终态与恢复入口

源码事实时点：Core `bdbb61bc`，2026-09-29 第四轮 service review。本文解释当前实现；下方“建议”尚未实施。[问题](../bug/REVIEW-2026-09-29-services-04.md) · [运行和覆盖](REVIEW-2026-09-29-services-04.md)。

## Platform：订单身份不是发奖结果

调用链：HandleCallback 验签并固化订单 → AttemptDelivery 在 Orders.Update 中递增 Attempts/AttemptSequence、追加 PendingAttempts → 外部 Deliverer → 成功 CAS 或 recordFailure → Redis 原子 pending index 随订单移动。Server.retryPendingOnce 从索引取 due 订单驱动同一入口。Mod 默认将 RedisOrders 同时作为 Orders 和 Pending，没有新建旁路持久层。

Attempts 是当前预算，Reopen 可重置；AttemptSequence 是不复用的身份；PendingAttempts 是外部结果待归档的证明。backoff 仅决定何时重试，不能证明商品未发出。当前 Due 不检查 PendingAttempts，recordFailure 则对任意错误移除本 attempt，因此“请求已返回”被当成“已知没有发货”。RR-23 的两个反例展示了这个区别。

SettleOutOfBand/Reopen 在 CAS 内先检查 pending，能保护已登记的在途证明。ResolvePendingAttempts 是人工**已经停止全部外部请求并完成对账**的断言，不是服务自动查到了退款事实；部署必须提供证据。清空 pending 和终态围栏能够拒绝迟到状态回包，不能撤销已经在业务资产端完成的发货。

生成的 platform collaborator 将 purchase grant 按 OrderID 写到 Redis hash，game 再消费。这个 OrderID 幂等写帮助防止重试新增不同债务，但 HSet 错误仍可能是写后丢回复；订单 CAS 与这次外部写不是一个事务。本轮只读 collaborator，未声称正式购买链完整验收。

**建议：** 优先复用现有订单字段和对账入口，增加明确的“未生效拒绝 / 结果未知 / 已生效收据”分类；默认未知保留证明。幂等重试需要业务持久 OrderID 去重且同 key 返回权威结果；人工退款仍须单独确认发奖事实。一次超时不能同时触发自动再发货和无证明退款。迁移时旧 pending 零值不可当成“确定无发货”。

## Mail：删除、领取、结算是不同事实

调用链：Send 持久 Intent/Envelope → direct 或 broadcast Deliver → 单个 Mailbox CAS；Reserve 从 Envelope 验证地址/期限，在 Entry 保存稳定 Token 和递增 Attempts；Cancel 只解开当前 Token+Attempts 的 lease；Commit 按稳定 Token 结算，容许迟到发奖回执。

Delete 仅变为 StatusDeleted，保留既有 Token。展示条目超容量时，evict 对 ClaimToken 非空的 Entry 保存 SettledClaims；无 Token 的 deleted 被完全忘记。随后 Deliver 会重建 unread Entry。expireEntries 保护未结算 Token，不把过期视作外部发奖未发生；这条保护是上一轮明确的设计边界，第四轮没有改写它。

当前 SettledClaims 同时承担“不许再领取”和“可将 Commit 作为成功重放”的职责。有 token 的 deleted 被淘汰后也落入这个集合，而 Entry 尚在时 Commit 拒绝 deleted。这解释了第四轮观察中同一 Commit 的返回由 missing 变为 claimed；它不是外部发奖的权威查询。

**建议：** 在已有 mailbox 内区分 display-deleted、grant-settled、grant-pending 的事实，保留有效删除身份。用户删除表示放弃未领取附件，但不能撤销已经开始的外部发奖；若允许删除在途邮件，合法迟到收据仍需明确结算入口，或对在途 Delete 明确拒绝。不要把容量淘汰当成发奖成功事件。

## 返回值所有权：错误结果同样是公开结果

MemoryStore.Get 返回 struct 副本，不复制内部 slice/map。服务不能依赖 Redis 解码带来的偶然隔离。Platform.Order/AttemptDelivery 已用 Order.clone，但 HandleCallback 重复回调的错误返回遗漏了复制，RR-24 可以不增加 Version 而改写 pending，继而使成功回包失去归档资格。

**建议：** 沿用各领域现有 clone；同时审查成功、错误、幂等重放与 collaborator 输入。失败回包通常最容易漏复制，也最容易被业务拿去诊断、排序或重试。copy 的目标是使外部可变数据与持久 CAS 状态隔离，不是让通用 Store 承担不可知的深拷贝。

## Match：提交不确定时从 ticket 找回已提交 match

一个 queueState 包含 Waiting/Tickets/Matches/SubjectTickets/Requests。Commit 在一个 CAS 中同时消费 ticket 并创建 match，失败没有部分状态。NewID 在 CAS 外产生，写后丢回包会失去此次 Match 返回值；再次 Commit 原 tickets 则因已经 matched 而 conflict，并非幂等返回原 Match。

第四轮验证：写前拒绝时原 tickets 仍 waiting，直接重试成功；写后丢回包时两个 ticket 的 MatchID 相同且非空，通过 Ticket（校验 subject）→ Match 可以恢复原 match，第二次 Commit 未创建第二个 match。Memory 与正式 Redis versionstore 两后端均通过。跨进程重建及资源分配交接尚未执行。

接入时要保留 queue、ticketIDs 和各 subject 的权威身份；收到未知错误先读 ticket，而非重新 Enqueue 或分配新房间。所有 ticket 指向同一 match 后再按稳定 Match.ID 幂等分配资源。Match 本轮没有发现新的确定功能 bug，不代表历史容量积累/公平性/正式房间消费链审完。

## Session 与性能边界

Session 先在 run CAS 中落终态，再逐个 Release，成功后 markReleased，最后删除 owner claim。外部释放与本地标记不是一个原子事务：两个 Finish 或一次释放丢回复均可能重复调用 Releaser，故资源端需并发幂等并校验 run/资源的实例身份。demo collaborator 只打日志，不包含真实 allocator；以前的并发观察仍不能推出实际 double-free。

性能方面，本轮只有源码判断和功能复现：Mail 克隆有界 mailbox、淘汰排序；Match 每次 queueState clone/编码含累计 Tickets/Matches/Requests，读 Candidates 的 limit 约束输出，不等于 Redis 只读该数量的 ticket；Platform 并行度和 pending 积累依赖外部延迟及 retry budget。没有执行负载，不能从测试耗时推 TPS/p99，也不能借清理未知证明降低存储成本。
