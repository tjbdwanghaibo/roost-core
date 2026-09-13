# Mirror 生命周期：解绑与工作完成

Core `d1b14b99a9fcf9ed4029966ac55372407b5a52a4`，2026-09-13。实测见[运行记录](REVIEW-2026-09-13-06.md)。

[envelope.go](../../mirror/envelope.go) 的 Start（48 行）在 mutex 内订阅并设置 started，失败允许重试。handler 直接调用 Store.ApplyReplica，使用 fctx.BaseContext；没有检查 started 或实例 generation。Stop（106 行）在 mutex 内调用 unsubscribe，再清除标志；没有在途计数或接收 context 取消。

[sync.go](../../syncbus/sync.go) 的 ISubscriber（35 行）不要求 unsubscribe 等待回调退出。传输层可能已经取出 handler，此后解绑只能阻止后续查找。本轮屏障测试证明 Apply 能在 Stop 后完成；保存 handler 的测试证明重启标志不隔离旧回调。替身不能证明所有真实 bus 都这样调度。

建议新 snapshot client 自己拥有接收准入：handler 携带启动 generation，在同一锁边界检查 generation/closing 并增加在途计数，完成时归还。Shutdown 先关闭准入并取消 client context，再解绑、等待工作、释放依赖。不能无锁 Add 的同时 Wait 并释放对象。generation 拒绝尚未准入的旧回调；已经进入的工作仍需要等待，或在提交点再次检查。

不要持有准入锁等待 unsubscribe/drain：适配器可能等待回调完成，而回调也需要该锁。超时不等于后台工作停止，应保留可再次等待的 stopping 状态。loader 不响应取消时，不提前销毁仍被使用的资源。

性能上只在准入/完成处使用短临界区，避免把网络调用和完整 Apply 放进全局锁而阻塞不同 key。按 key 串行提交与全局生命周期准入分开处理；本轮无性能测量。这些是[实施方案](PLAN-REMOTE-POLICY-MIRROR.md)第 3–4 步的待实施要求，不是已有能力，也不是新增确认 bug。
