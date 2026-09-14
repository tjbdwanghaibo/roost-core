# Activity：聚合窗口与派发任务的生命周期

09-14 第三轮 Kit `7030c5f`：U-0191 将 opening 与确认条目分开，U-0192 增加独立 Delivering 列表。上一轮原始 overlay 的开活动交错、三条派发路径及对照全通过，包 race 通过。原 RR-02/03 触发已验收；作者明确的 reopen 调度、发送适配器与超 OpeningGrace 容量边界仍未完成。见[第三轮](REVIEW-2026-09-14-03.md)。以下是原基线分析。

审查基线：Kit `ac1a8801604bde4b7ec5ed60104502c542602ed0`，配合 Core `84b2a4a`；2026-09-14 第二轮。状态：部分源码阅读与场景验证，未做真实 Redis、网络投递及性能基准。

## 当前数据与调用链

`service/global/activity/redis_store.go` 为 Activities、Participants、Ledger、Audits、Dispatches、Windows 建立六套 versionstore 存储。每条 Update 有 CAS，但不同存储的操作没有共同事务。Ledger 配 TTL；其余存储此处不配置 TTL，不能把单条记录上限理解为累计磁盘空间有界。

`OpenActivity → admitToWindow → Activities.Create`。Windows 是每组的有限 key 列表，承担发现任务的责任。`NotifyPhase` 将 pending 变成 collecting 并设置宽限期；收齐所有 game 时变成 complete。`AdvanceExpired` 读取 Windows、按期限排序，推进到期活动；已完成记录走 heal；不存在记录走 prune。

`settleCompletion/AdvanceExpired → ensureDispatches → pruneWindow`。ensureDispatches 按 (activity, game) insert-only 创建 Dispatch，重复执行可补齐部分失败并保持原 token。Dispatch 初始 pending，AttemptDispatch 用 CAS 增加 Attempts、计算退避；AckDispatch 用 token 确认并进入 acked；预算耗尽转 exhausted。管理端 ReopenDispatch 保留 token，重置预算并回到 pending。

## 为什么 CAS 仍会丢任务

窗口是发现活动的入口，活动记录是业务事实。先创建窗口可防止一类“活动已存在但从未索引”的失败，但 sweep 可将尚未完成创建的窗口当成垃圾删除。单独 CAS 每条记录无法区分“创建中”与“永久缺失”。[RR-20260914-02](../bug/REVIEW-2026-09-14-02.md) 在 Create 前插入 sweep，稳定得到成功开活动但最终 collecting 且 window=[]。

派发任务比聚合过程活得更久。当前 Server.sweepGroup 只消费 AdvanceExpired 返回的本次新完成活动；聚合窗口删除后，派发的 pending 状态虽然持久存在，却没有再次被内置循环找到。[RR-20260914-03](../bug/REVIEW-2026-09-14-02.md) 的 grace/notify/heal 三条路径均复现，显式调用派发原语的对照通过。

## 修复设计建议（未实现）

优先复用现有 versionstore：为创建/清理设计带代际的可恢复意图，为派发设置独立待处理索引。清理不能只凭旧读得到的 key 无条件删除当前索引；失败恢复必须能重建索引与事实记录的一致关系。单纯调换两次写入或追加一次写入，会把失败窗口移到另一个位置。

派发索引与聚合窗口分别设置容量、退避和保留规则，完成、补建、reopen 都需要交接路径；确认或耗尽才退出派发调度。端到端还需投递接口、接收方 token 幂等、ACK 失败重试。当前 sweep 丢弃 AttemptDispatch 返回值，因此本轮 Attempts 断言仅证明调度次数，不证明网络发送。

## 性能与业务接入评价

现有窗口容量和批量上限限制单次处理量，是合理的保护；但 AdvanceExpired 会逐条读取窗口活动，heal 又可能按每个预期 game 读取/创建派发，单次 Redis 往返数量随窗口规模与 fanout 增长。DueDispatches 也逐 game 读取。这里是源码成本分析，没有吞吐数字；优化前先修复任务可发现性，再评估分批读、有限并发或有序到期索引。

聚合 complete 表示形成结果；Dispatch pending 表示仍须交付；Attempts 表示领取过投递预算；只有接收方真实 ACK 才能证明其声明已处理。业务监控应分别展示 collecting 超龄、最老 due 派发、失败/耗尽和 ACK 延迟，不能以聚合完成数替代结算交付成功数。

## 验证与后续

本轮 Activity 包 race 通过；四个独立故障场景失败、正常开活动与显式重试/ACK 对照通过。使用 MemoryStore 和注入时钟，未模拟 Redis 回复丢失、跨进程崩溃或真实发送。后续先验收两项 RR，再查 ApplyProgress 预留记录与 participant 去重窗口的失败恢复、派发索引与 reopen/分页公平性。见[运行记录](REVIEW-2026-09-14-02.md)。
