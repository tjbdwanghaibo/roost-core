# Remote Entity 与 Nest：生命周期和性能学习

最新补充（09-11 第三轮）：Core 4e8f5ec 的 U-0172/0173 已独立验收。外部 Close 现在在 retryMu 下检查 stopping 并加入 retryWG；Nest 停机回收延迟消息前回复 ErrNestStopped。真实 batch 的停止后/停止中关闭、Nest 内部重排停机与停止后重排均通过，见[验收记录](REVIEW-2026-09-11-03.md)。以下缺陷描述与微基准保留为旧基线历史，本轮未重新测性能。

源码基线 Core `31ffe275645ae04f5376c748feb31aa0422b6e6d`。本文区分现有源码机制、已测行为与建议；不代表建议已经实现。正常 prepare/finalize 过程见[已有学习文档](IMPLEMENTATION-REMOTE-PREPARE-AND-FINALIZE.md)。

## Remote 的资源交接

Prepare 得到的 batch 持有 entries 和 finalizer 名额。entry 的 writeGate 约束本进程同实体写入，ownership 读锁保护所有权使用，wrapper 引用维持对象存活；获取过的分布式锁也需要释放。

Close 对普通终态同步释放；不确定事务及符合条件的异步提交转交 deferredRemoteClose。finalizer 查询/协调终态，必要时重试或隔离，再释放 entries 与 slot。重试等待让出 worker，但不提前让出实体写入资源，以免不确定结果下继续写。默认 4096 名额、16 workers，对积压有界；重试 goroutine 数受到已取得名额约束，不能称为无限创建。

这个选择有明确代价：一个实体的慢后端或未决事务会拖住同实体后续写入。提高 worker 数只能帮助独立实体，不能消除热点实体的串行依赖。默认单次 OpTimeout 30s；真实锁释放与重试耗时尚未测量。

StopFinalizer 等待 workers 与 retryWG 后最后排空队列。关键不变量应是“停止完成后不再有人成功交接新资源”。当前只等待内部 retry 发送者，遗漏外部 batch.Close，导致 [RR-20260911-03](../bug/REVIEW-2026-09-11-02.md)。建议将准入关闭、发送者退出与最后 drain 纳入统一协议，保证失败交接由调用者清理，成功交接由消费者清理，恰好一次。

## Nest 的接收、完成和停止

Request 建立回复通道，立即或延迟准入，再等待结果、context 或同步超时。延迟调度使用集中堆与循环；停止时需同时处理 worker 中的任务和尚未到期的消息。内部 group 重排会克隆消息并保留 RetChan，因而队列元素可能有等待中的同步调用者。

当前延迟队列停止分支只回收 Msg，未结束等待者，形成 [RR-20260911-04](../bug/REVIEW-2026-09-11-02.md)。业务取消不能替代框架对已接受请求的终态责任。回归应覆盖消息到期与停止竞争、显式 delay、内部 requeue，并确保只有一个回复。

Pipelined 完成机制将持久化 ticket 与回调/回复交给 completionPump。提交满队列时回退到当前执行路径，仍维护完成顺序；ticket 失败进入 abandon/fence 路径，成功才进行后续事务完成与回复。完成链按 PRIMARY entity 关联；共享次实体、主实体不同的多实体事务不具有同一条完成顺序保证，这是源码明确的契约边界。业务若依赖跨聚合回调顺序，不能直接假设存在全局串行顺序。

Shutdown 在后台停止 dispatcher，再停止 completionPump；调用者 context 限制等待时间。completionPump 在排空前关闭准入，使已接受 completion 有收尾路径。仍需在真实慢 ticket、慢回调下量化停机预算；本轮没有测出这些分布。

## 实测性能及适用解释

同机、单次、无 race 的微基准，命令见[附录](../bug/REPRO-2026-09-11-02.md)。

| Nest 模式，模拟提交 5ms，20 次 | 总耗时 ms/op | 实体锁等待 ms |
| --- | ---: | ---: |
| Strict | 5.593 | 5.595 |
| Pipelined | 5.495 | 0.04260 |

Pipelined 在这个场景显著缩短实体锁被持久化等待占用的时间，最终确认耗时仍接近模拟提交延迟。收益主要是让同实体后续工作更早进入可执行阶段，不能据此声称端到端请求延迟降低同样比例。实际收益还受热点、回调、队列饱和和存储尾延迟影响。

| Remote tracker 已满的保留条目数，100 次 | 新事务 track + complete μs/op |
| ---: | ---: |
| 64 | 1.488 |
| 1024 | 33.988 |
| 8192 | 215.458 |
| 65536（默认上限） | 5491.790 |

每档均为 224B/op、2 allocs/op。transaction_manager 的清理与最老终态选择遍历 map，并持有全局 txMu；满容量、终态未到 TTL 的工作负载会放大扫描成本。此结果不是 U-0168 等待者 bug 的复发，也没有证明实际业务已经达到此开销。

建议先测真实 pending/terminal 比例与容量占用，再考虑终态有序索引、过期堆/时间桶或分摊清理，保留等待者生命周期语义。关注 txMu 等待、finalizer 队列/名额占用、未决事务年龄、同实体 gate 等待及 completion 回调延迟；队列变大只能缓冲压力，不能替代服务能力。

## 评价与下一步

这两个模块已具备实体一致性、容量约束、异步持久化确认和故障隔离的设计基础，适合继续打磨成通用框架。当前最优先的是补齐停机时的资源与回复所有权，随后再以真实游戏热点和后端故障测试验证成本。尚未完成 Linux/真实后端压测，不能把本轮有限证据作为生产就绪证明。

09-11 第四轮补充：[completion 异常与停机](IMPLEMENTATION-COMPLETION-FAILURE-AND-SHUTDOWN.md)验证慢 ticket/回调下重复 Shutdown，新增 RR-20260911-06；捕获业务 panic 并不保证框架回复与释放完成。
