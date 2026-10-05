# 权威快照回填：入口身份与最终读取承诺

2026-10-05；[本轮](REVIEW-2026-10-05-noncore-24.md)，[NC-35](../bugfix/RR-20261005-NC-35.md)、[NC-36](../bugfix/RR-20261005-NC-36.md)、[旧过期项补修](../bugfix/RR-20260913-08.md)。

Get 的 Linearizable 直接走 LoadAuthoritative，Monotonic miss/版本不足经过 loadMonotonic 合并相同 key/minVersion 请求，再调用同一权威入口。Remote SnapshotReplicaStore 收到 delta 时，gap/epoch/schema 不匹配也用权威全量修复。Manager 外层 Monotonic 有有界版本等待和一次再加载；不能因为某个入口已有校验，就假设其他出口也经过它。

| 阶段 | 必须检查 | 原来漏掉什么 |
| --- | --- | --- |
| loader 原始结果 | 完整 key 等于请求、原始版本足够、数据未过期 | 只验版本，另一 scope/tenant/entity/policy 可写进其他键 |
| Publish 准入 | 保持 epoch、同版本内容冲突、删除水位与已有 L2 错误契约 | 正常“不覆盖”不是保证请求最低版本已满足 |
| 实际 L1 返回 | 以当前时间检查有效期，再检查最终最低版本 | 复用发布前时间，或假定最终 L1 就是原始回答 |
| 错误之后 | 原视图保留、无串键副作用、合法加载与重投递可恢复 | 仅检查第一次 error 会漏掉副作用或永久不可恢复 |

“权威原始结果可信”和“最终返回值正确”是两次证明。Publish 是准入操作，可能保留较新 epoch，可能等待 L2 并跨过绝对截止时间；它返回 nil 不表示指定版本已经落到该 L1。既有容器 TTL 控制保留时长，ExpiresAt 控制数据有效期，两者不能互相替代。

这轮旧 RR-08 为何还会留下残余：之前补了原始结果与最终 L1 两处 Expired，却复用了前一处的时间。原测试只覆盖调用前已过期和同版本延长截止时间，没有让 Publish 中的真实调用阶段跨越截止时间。改进是沿函数内部每个有界 I/O/锁等待重新核对时间敏感承诺，用信封自己的截止时间作为测试事件，而不是任意 sleep；本次通过实际 L2.Set 钩子等待截止时间，再断言公开读取不得 found=true。

Assembly 的启动资源顺序是 snapshot Subscribe→interest Subscribe→seal→storage初始化→outbox恢复→finalizer启动。第二个订阅失败释放第一个，重试复用同一组 Replicator；重复 Start 幂等，Stop 最终两个订阅都释放。停止超时保留发布依赖是为了已接纳的 finalizer 工作，必须验后续 Stop 最终收口，既有回归已覆盖这一点。

Replicator.Stop 的能力仍是取消订阅，没有等待旧 handler 完成或给回调加代际门。已经在途/被 transport 保存的旧回调是否继续调用由 transport 生命周期管理；本轮未新增 drain 承诺，也未把取消订阅当成不再有任何缓存副作用的证明。另一个观察是修复 loader 返回 miss/过期时 SnapshotReplicaStore 只传 loadErr，nil 不证明目标视图存在；未确定真实 broker 业务 ACK 语义前不登记新 bug，也不改变重试协议。

性能上修复只增加完整键比较、一个最终版本比较与一次当前时间读取，没有新缓存、goroutine、锁、等待或 loader 重试。现有 Publish 对 L2 的有界等待和按 publish shard 串行的代价保持；本轮没跑延迟基准、长期容量或真实 broker/HA。
