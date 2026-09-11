# Nest completion：慢确认、回调异常与停机

基线 Core `dd1f270c1f044022df37a887f6510e8e26b90dab`。本轮续读异步完成机制；已有锁等待性能数据见[历史文档](IMPLEMENTATION-REMOTE-NEST-LIFECYCLE-AND-PERFORMANCE.md)，本轮未重测性能。

## 完成链中的不同责任

invokeWithTransaction 在实体锁内 Enqueue，记录 LSN，将 committer 的 TransactionReleased 追加到 tx 的 AfterCommit 列表，再把 ticket、事务和回复通道交给 completionPump。prepareCompletion 捕获独立回复通道，避免后续使用已回收 Msg。

pump 等 ticket，随后将完成函数派给按主实体分片的 worker；队列满时直接在 pump 执行。complete 先等待同实体前驱，再执行 tx.Commit，最后回复。排序节点由 defer release，异常展开仍能释放此节点。

但三个“完成”不能混为一谈：

| 责任 | 含义 | 当前异常路径 |
| --- | --- | --- |
| ticket 完成 | 持久化结果已知 | 本轮成功，不可因后续回调 panic 回滚 |
| order.release | 允许后续同主实体 completion 执行 | 有 defer，panic 展开仍运行 |
| TransactionReleased + 回复 | 通知后端解除 held，并结束请求等待 | 位于可 panic 的业务回调之后，可能不运行 |

RollbackTx.Commit 先将状态标为 committed，随后逐个执行回调。worker.SafeFunc 在外层 recover 只记录日志，不会跳回中断的循环继续执行，也不会跳到回复语句。因此“panic 被捕获”不等于“事务收尾已完成”。[本轮实测](../bug/REVIEW-2026-09-11-04.md)正好区分了这三件事。

## 慢完成与 Shutdown

NestMgr.Shutdown 第一次调用设置 stopped 并启动后台清理，后台先停止 dispatcher，再排空 completionPump。调用者只在 waitStopped 等待共享 stopDone；context 到期不会取消后台清理。后续 Shutdown 仍等待同一个 stopDone。

本轮分别以未确认 ticket 和阻塞 AfterCommit 作为屏障，连续两次短超时 Shutdown 均返回 DeadlineExceeded；解除屏障后再次 Shutdown 成功，原请求回复与释放各正常完成。测试表明这两类慢完成可以继续收尾，不需要重复启动另一套停止流程。它不证明无限阻塞回调可以被强制终止。

异常回调则不同：worker 看起来已经完成任务，Shutdown 也能结束，但具体事务的释放/回复被跳过。正常排空只是必要条件，不能替代每个 completion 的终态保证。

## 建议与业务接入

建议把框架必需通知从业务回调队列中分离，并在 completion 层统一处理 worker 与 pump 回退的异常。明确定义已持久化而副作用失败的回复语义；继续执行其他业务回调还是中断并隔离，需要契约决定，但不能默默漏掉释放和请求终态。

业务应让 AfterCommit 有界，外部效果使用可重试、幂等的持久化记录。调用方超时不代表事务未持久化；不要把超时后的盲目重试当成回滚替代品。本轮只验证现有接口与替身 committer，真实 WAL/projector 的 held 清理、进程崩溃恢复与饱和回退 panic 仍待实验。
