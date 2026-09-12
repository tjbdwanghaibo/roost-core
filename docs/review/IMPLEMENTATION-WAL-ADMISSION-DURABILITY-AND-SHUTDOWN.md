# WAL：准入、持久化、投影和停止的边界

基线 Core `c9e853e08c91d498b65d6f1d6e4dd35d39726a51`。本轮读取 nestwal 实现并用真实临时文件验证，外部投影和发布仍是替身。相关问题见[09-12 报告](../bug/REVIEW-2026-09-12.md)。

## 不能混淆的四个阶段

| 阶段 | 当前机制 | 能说明什么 |
| --- | --- | --- |
| 接受 | Enqueue 校验、预留容量、排序 LSN 并入 appendCh，返回 ticket | 已承诺处理；不代表字节已写入 |
| 持久化 | writer 收集批次、写文件并 fsync，推进 DurableLSN、完成 ticket | 记录持久化；不代表业务投影已发生 |
| 释放与投影 | Committer 的 held 防止提前 replay；TransactionReleased 清除 held 并唤醒 | 已允许应用 mutations/effects；仍需成功执行 |
| Ack | replayPass 应用后写 Ack；关闭重开从 Ack 后继续 | 已确认前缀，不需再次重放；后端仍应幂等 |

collectBatch 在 BatchDelay、条数、字节数或关闭信号上结束，然后才 processBatch。Sync 当前绕过了这条队列，直接同步 active 文件，故不能凭 Sync=nil 推断第一阶段已经整体进入第二阶段。修复应选择明确的准入边界，而不是把队列当前长度为零当作完成：writer 可能已经取走记录，仍在收集批次。

## held 队首的设计代价

Committer.Enqueue 先 hold 事务，准入失败释放；成功后等待 Nest 的 TransactionReleased。replayPass 按日志顺序遇到 held 就返回 errTransactionHeld，因此一个未释放队首能挡住其他实体的后续已释放记录。

本轮真实文件测试：A、B 都持久化，仅释放 B 时应用次数为 0；释放 A 后应用次数为 2；Ack 保存后关闭重开不再返回这些记录。另一个实验不释放记录就关闭并重开，新的 committer 能恢复两条。这说明 held 是进程内安全门槛，持久化日志仍能在重开后恢复，不能把投影暂时停顿等同于日志丢失。

代价是 release 通知必须可靠。业务 AfterCommit panic 跳过该通知时，问题可能影响队首之后的其他实体。真实业务 panic→真实 WAL 整条集成链尚未搭建，本轮分别验证了 Nest 通知丢失与 WAL held 阻塞两个环节。

## 停止预算为什么会失效

Committer.Shutdown 先 Flush 再 Close。后台 replay 使用 committer.ctx，并在整个读取、应用、发布、Ack 期间持有 replayMu。Flush 也要取得 replayMu，且自身还有 flushMu。普通 mutex.Lock 不接收 context，调用者在这里等待时，传给 Shutdown 的截止时间不能中断等待。

后台 applier 即使正确响应自己的 context 也不能解决：它得到 committer.ctx，而取消动作在尚未执行的 Close 内。本轮将这个因果链固定在真实后台投影中，确认 20ms 截止后再等待 50ms 仍不返回。

建议将“停止继续准入”“后台排空”“调用者等待”分别建模。取消等待不必等同于丢弃已接收数据；应保留后续可以检查、等待或恢复的状态。设计 context 可取消的协调机制时，也要保护正在使用的文件和单消费者 replay，不要为按时返回制造并发关闭或重复投影。

## completion 的两层容量与故障隔离

completionPump 输入 channel 与 worker 的 MPSC 队列不是同一个容量。MPSC 最小为 16，即使给 pump 的配置为 1，不能用“一条排队任务”推断 worker 已满。测试应通过实际准入拒绝证明背压条件。

pool 拒绝后 pump 直接执行 complete，既转移了回调延迟，也改变了异常恢复边界。普通 worker 有 SafeFunc，pump.run 直接 goroutine 没有；本轮隔离子进程确认 panic 可越过该边界退出进程。需要在 completion 层保证统一的释放、回复和异常语义，而不是依赖调用位置恰好有 recover。

## 后续验证顺序

先修正 Sync 的准入屏障和 Shutdown 的可取消等待，再用当前复现验收；同时处理 AfterCommit 的必需通知与饱和恢复。之后增加真实 Nest/WAL 组合、Ack 写失败后重试、Linux 存储尾延迟与容量压力。当前实验没有检验断电持久性、真实数据库事务或生产吞吐，不作生产就绪证明。
