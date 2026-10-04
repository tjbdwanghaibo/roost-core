# Redis 锁续租与订阅生命周期

**最终状态更新（2026-10-04）：RR-20261004-01已由上游3bb901fb/5d386146修复，本轮独立验收通过，未发版。** 本轮原4场景在真实Redis全部转绿；13条取锁/释放未知正式回归race与Remote vet通过，另3条真实Redis集成通过。下方本轮“新wanted未修”保留发现时点，以本条及独立验收为准；三资源生成消费者、authority故障矩阵与长稳未在本机验收。 [验收证据](../review/evidence/noncore-review-20261004-18/README.md#独立验收上游修复)。

提交前新增wanted已审：versioned TryLock执行后丢回复丢失token，登记[RR-20261004-01](../bug/RR-20261004-01.md) P2未修；[追加4场景/方案边界](../review/REVIEW-2026-10-04-noncore-18.md#提交前新增wanted)。普通锁/订阅14项通过的结论仅限其原范围，不包含此新反例。

2026-10-04，源码基线 `3d3b22c9`。[运行/14 场景](REVIEW-2026-10-04-noncore-18.md) · [原始证据](evidence/noncore-review-20261004-18/README.md) · [进度](PROGRESS.md)。本页解释现有实现，本轮未修改 Redis 产品代码。

## 持有者身份与结果未知

[lock.go](../../redis/driver/lock.go)由工厂装配 `IDistLock`。每次 Acquire 产生随机 owner value，SetNX 设置 TTL；正常失败不持有，成功进入 acquired。网络错误不能证明 SetNX 没执行，因此保留 token 并进入 uncertain，Acquire 拒绝重用。Release Lua 先比较 value 再 DEL，Extend 先比较再 PEXPIRE；传输错误保留 token，后续校验清理只针对自己。

这防止旧对象误删新持有者，**不**阻止旧业务代码在 TTL 到期后继续写存储。接口已明确 best-effort、无 fencing；正确性级实体/存储互斥应走已有 versioned lock 或 fenced election，而不是给本锁再包一次重试。测试在真实 Redis 执行后注入回复错误，核验三种操作的不确定状态、token reconciliation 和重用；不是网络代理故障测试。

## 自动续租拥有独立生命周期

`NewAutoExtendLock` 要求 inner 初始 TTL 与 wrapper 一致；默认 interval=ttl/3，非法 nil/TTL/interval 在进入前拒绝。Acquire 成功后 Background watch 独立于 Acquire 请求 ctx，避免短请求结束立刻停止长任务的续租。opMu 串行化 Acquire/Release/手动 Extend，mu 保护 cancel/done/lostErr。

watch 每个 interval 建立有预算的 Extend ctx。成功刷新本地 lastRenewed；服务端明确不持有马上记录 ErrLockNotHeld 并退出；传输错误到本地 TTL 窗口耗尽才记录丢锁，保留原始原因。Release 取消并等待 watch，再调用 inner token 校验清理；重新 Acquire 清除前轮 lostErr。用户需持有 wrapper 并在长临界区对外发布前检查 Err，同时理解没有 fencing 仍可能双执行。

本轮已验跨原 TTL 的存活、Acquire ctx 取消后仍续租、瞬时错误恢复、持续失败暴露 cause、服务端删除后暴露丢锁，以及 Release 后可重新获取。Err 是已观察到的丢锁信号，不是 downstream 写权限证明。inner 不协作取消时 Release 仍可卡在等待，opMu 的排队也不看 caller ctx；未实现有界停止的新协议。服务端执行时间与回复时间差、GC/长暂停、手动改 TTL 的组合和弱网恢复仍需验证。

## 订阅资源由接入方关闭

[Client.Subscribe](../../redis/driver/client.go)把 go-redis PubSub 交给[newPubSub](../../redis/driver/pubsub.go)，SDK channel 与框架 msgCh 之间有转换 goroutine。外层 select 同时读 SDK 和 done，内层发送 select 同时读 done；因此业务不消费导致 msgCh 满时仍可退出。Close 用 once 关闭 done 并关闭 SDK PubSub；goroutine 退出关闭 msgCh，channel 关闭可能晚于 Close 返回。

本轮等待 server NUMSUB 确认后验证消息字段和多 channel 路由；不凭 Subscribe 返回推断已经 ack。明确观察满队列后关闭，再排空 channel 至 closed，16 并发 Close 也能结束。接入方保存并显式关闭每个 subscription；Client.Close 仅委托 SDK，本轮没有证明它自动收尾所有独立订阅，也没有承诺断线消息可追回。Pub/Sub 不持久化，可靠业务事件应使用框架已有可靠消息链，不能拿本次正常投递测试替代交付协议。

## 成本、限制与下一入口

每个活动 auto lock 至少一个 watch goroutine/ticker；每次续租一次 Lua 往返。每个订阅有转换 goroutine、两个有界消息 channel 和 SDK 独立资源；慢消费者可能形成背压，持有者必须管理 Close。没有 benchmark 或长期连接/goroutine 水位测量，不给吞吐收益和容量上限。

本轮 14/14 场景通过、未确认新 RR；真实弱网/重连、Client 与订阅整体关闭、Cluster/HA、租期时钟和长期容量继续留在[CARRYOVER](../bug/CARRYOVER.md)。下一轮优先 RefHMap schema/Patch 与全量写并发及未知结果恢复，随后正式迁移消费者，优先使用已有脚本、错误原因、权威读回和版本能力。
