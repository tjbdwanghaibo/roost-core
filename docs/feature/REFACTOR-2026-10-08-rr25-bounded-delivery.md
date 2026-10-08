# RR-25 新方案：保留投递上限，修复消费者生命周期

状态：已按本方案实施，目标race与真实NATS验证通过；最终阶段发布验收见docs/review/STAGE-v1.23.1-CLOSURE-2026-10-08.md。维护者明确要求“RR-25不能取消投递次数上限”。本方案替代此前无限重投候选；不直接恢复旧 candidate.patch。

## 目标与证据

保留 `syncbus.max_deliver`，默认 5，表示同一 durable 对一条消息的**总投递次数**，不是首次之外再重试 5 次。0 继续取默认值，不能映射成无限；不增加次数、不换 durable 名、不通过重建消费者重置计数。

核对基线 main `36fb32ab`。CBM generation 为 2026-09-30，图中 Stop 行号已漂移；相关文件已做 coverage 检查并以当前源码补证：

- `sync/syncbus/driver/jetstream.go:259`：fanout 为空或全部 Subscription 已退订，仍返回 nil，底层因此 ACK。
- 同文件 `:279`：最后退订先删除 topic，再 Stop；新 Subscribe 可在旧 consumer 关闭之前进入。
- 同文件 `:247`：总线关闭准入后返回停止错误，未设置 NAK 退避。
- `nats/driver/jetstream.go:106`：错误达到上限 Term，否则按 NakBackoffMin/Max 退避；未设置时立即 Nak。已有能力足够，不必改 RPC/Remote 的公共结算策略。
- `sync/syncbus/subscription.go:92`：退订先关闭本地准入，release 必须非阻塞，支持 handler 用其投递 ctx 退订自己。此契约必须保持。

## 1. 一个 topic 同时只允许一个消费实例

每个 fanout 使用 `creating → active → retiring → closed` 生命周期。Subscribe 与 SubscribeLive 保持各自的身份，不合并两个 durable。

- 首次订阅先登记本地接收者和 creating 状态，再在全局锁外创建 consumer；其他 topic 不被网络创建阻塞。创建中的回调、失败回滚及并发 Stop 必须有明确归属，不能凭“Subscribe 返回后才回调”的假设实现。
- 最后一个订阅离开时，先在锁内将 topic 标记 retiring，保留该代对象，再在锁外请求 consumer.Stop。没有接收者时不继续发起新的消费。
- retiring 期间的新 Subscribe 返回可判别的可重试错误，不能创建第二个 Consume，也不在 callback 内等待旧 Closed。调用方通过现有生命周期重试；实施时复核 Mirror 等调用方，不能吞掉失败并宣称启动成功。
- 旧 consumer.Closed 与该代在途回调均结束后，才移除旧代、允许下一代创建。清理核对对象身份，防止旧回调误删新代。
- release 只发起停止；不能等 Closed，否则 handler 内退订自己会死锁。等待与最终回收属于 transport 生命周期管理。

## 2. ACK 按“是否实际交付”决定

| 实际情况 | 处理 |
| --- | --- |
| 合法远端消息，至少一个本地 handler 获得投递准入 | 完成既有 fanout 后 ACK；每个接收者仍拥有独立消息副本 |
| fanout 为空，或快照中的全部 Subscription 都在准入时拒绝 | 返回明确的生命周期重投错误，不 ACK |
| handler 已执行但返回业务错误或 panic | 保持既有记录错误并继续其他 handler、最终 ACK 的约定，不顺带修改业务失败语义 |
| 坏信封、自进程回环消息 | 保持原丢弃并 ACK 约定 |

实现应分别表达“准入成功”和“handler 执行错误”。不能仅凭 errors.Is(err, ErrUnsubscribed) 猜测准入结果，因为业务 handler 也可能返回同名错误；也不能仅检查快照长度。可为 Subscription 增加供传输使用的准入结果入口，并让原 Deliver 委托同一实现；不新增消息队列和独立计数器。panic 仍需归还准入，真实执行过不能误判为无人接收。

退订先于准入的订阅不再收到新消息；其他仍存活的订阅正常接收。已经交付过的消息可能因 ACK 失败重投，不能承诺 exactly-once。

## 3. 有限重投加退避，停止消费优先

只在 SyncBus 创建 consumer 时使用已有 NakBackoffMin/Max：建议起始 1 秒，上限 `max(1秒, AckWait)`。AckWait 默认 10 秒时，前四次失败后的请求延迟为 1、2、4、8 秒；第 5 次失败按现有规则终止。该参数用于 NakWithDelay，不改 broker 的 BackOff 数组，也不改正常 ACK 延迟。

退避是窗口内在飞消息的保护。主要修复仍是及时停止旧 consumer、无接收者时不启动消费；不允许把一个空 fanout 留在后台靠退避反复拉取。库与网络中已经在飞的消息仍可能到达，不能宣称 Stop 一调用就撤销了服务端的全部投递。

停机顺序：关闭新订阅/创建准入，标记已有 topic 退役并取消创建，请求停止消费，等待创建收尾、consumer.Closed 及总线/该代在途回调结束，最后允许释放连接。StopWithContext 超时保留资源与旧代状态，后续重试等待同一批；不会因为超时就创建新 consumer 或释放仍被 handler 使用的资源。

普通退订不能改为 Drain 后继续调用已退订 handler；这会破坏现有准入和资源释放契约。正常 Stop 也不能依靠持续 InProgress 保活无人接收的消息。

## 4. 达到上限是明确失败，不绕过上限

有限投递意味着连续崩溃、反复退订或 ACK 失败仍可能耗尽预算，包含“最后一次机会恰逢停机窗口”。本方案不再承诺这些情况下无限次恢复投递。

- 保持终止日志与 `nats.jetstream.terminal.total{reason="max_deliver"}`。日志记录 stream、consumer、stream sequence、次数和生命周期原因，不打印业务 payload。
- 区分本地 Term 与 broker 因 ACK 超时耗尽：后者可能没有进入本进程的错误处理，运维验收同时观察 JetStream MAX_DELIVERIES / MSG_TERMINATED advisory。普通 NATS advisory 不是持久失败台账，不能把“订阅了告警”当成崩溃期间也绝不漏报。
- 给出恢复 runbook：状态型订阅按其正式快照/重同步接口恢复；事件型订阅按具体业务的持久日志或人工对账恢复。通用 SyncBus 不知道业务事实，不能自动拿最新状态代替必须逐条执行的事件。
- 不自动重新 Publish 同一消息、不新建补投流、不换 durable 或重置 cursor；这些都会变相绕过上限。原消息在 stream 中是否仍可读取受保留策略和期限约束，不能当作永久备份。

## 5. 验收场景

先固定行为失败，再实施；旧无限重投测试中的配置断言不能沿用为新方案的成功条件。

1. 空 fanout、快照后全部退订均不 ACK；存在活跃接收者则正常交付。覆盖 handler 返回 ErrUnsubscribed、业务错误和 panic，避免混淆准入结果。
2. 卡住旧 Closed 后重订：不出现第二个 Consume；释放后可重试成功；live/all 身份仍独立。
3. handler 自退订不死锁；在同一回调内立即重订返回可重试错误，不等待自己；无订阅时没有持续拉取。
4. 创建期间 Stop、创建失败、其他 topic 并发创建，以及 Stop 超时后再次 Stop，确认资源最终收敛。
5. **真实私有 NATS**，MaxDeliver 分别为 1、2、5：核对 consumer 配置和服务端 NumDelivered；退避生效、上限仍生效、同 durable 重订不重置次数、到限无第 N+1 次成功投递。MaxDeliver=1 明确没有重试机会。
6. 预算未耗尽时短暂退订/重订最终收到消息；耗尽时明确终止且可观测，不能只检查“最终收到”而漏掉次数越界。另验证 ACK 丢失、进程退出前未确认的恢复边界。
7. 定向 race 覆盖 SyncBus/Subscription/NATS driver/kit 装配；全仓 build/vet/test 与根包门禁。共享 RPC/Remote 原结算策略回归保持通过。

主要改动集中于 sync/syncbus/driver/jetstream.go、Subscription 准入结果入口及对应回归；配置 help、OBSERVABILITY、bug/bugfix 与交接随实施更新。不新拆包，不引入第二套持久投递系统。已实施以上业务代码与回归。

## 参考

- [NATS ACK、NAK、MaxDeliver 与终止通知](https://docs.nats.io/learn/jetstream/acknowledgment)：NAK 立即重投；显式延迟使用 NakWithDelay；达到上限不等于业务成功。
- [Go ConsumeContext 与 Msg 接口](https://pkg.go.dev/github.com/nats-io/nats.go/jetstream)：Stop 与 Drain 的缓冲处理不同；当前仓库固定 v1.53.1，实施以该版本本地源码为准。
- [原缺陷和修前失败](../bug/RR-20261008-25.md)、[旧候选历史](../bugfix/RR-20261008-25.md)。
