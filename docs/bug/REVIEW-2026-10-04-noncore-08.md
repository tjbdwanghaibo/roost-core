# N03 etcd 接续：session 创建与 callback 关闭预算

2026-10-04，etcd源文等于远端main`e62729aca28f8db8e9613145c3b3cf5e1bc1701e`，同一工作树先实施NC-08～10修复；本批没有修改etcd或KitEtcd产品代码。Tier2当前源码/实际反例验证，两个新P2 **已确认、未修**。没有把上一轮已修三项重报。

| RR | 触发与影响 | 证据 |
| --- | --- | --- |
| RR-20261004-NC-11 | Campaign已取消，但session的LeaseGrant仍等client生命周期，选主等待/退出超出caller预算 | 正式clientv3→NewSession→本机gRPC LeaseGrant，确认入场后取消，100ms内不返回；关闭client才退出 |
| RR-20261004-NC-12 | WatchCallback.CloseWithContext在检查ctx之前同步关闭底层watcher，第三方watcher关闭阻塞会拖住caller | watcher.Close确认入场，已取消ctx仍等100ms，放行watcher后才返回context.Canceled |

8个新增叶子/独立项=2行为失败/6控制，归为2根因；两测试包race/45 test pass事件、0测试fail/skip，KitEtcd仅编译/vet且无测试。选择真实etcd集成项1条因PATH无etcd而skip，未计通过。[运行/范围](../review/REVIEW-2026-10-04-noncore-08.md) · [复跑与原始日志](../review/evidence/noncore-review-20261004-08/README.md) · [机制与修复方向](../review/IMPLEMENTATION-ETCD-SNAPSHOT-WATCH-AND-LIFETIME.md)。

## RR-20261004-NC-11

**P2：Campaign取消没有覆盖创建session的真实RPC。** `etcd/driver/election.go:66–72`的createWithEtcd直接concurrency.NewSession(e.cli)，其内部默认sessionOptions.ctx=client.Ctx()，Grant使用该ctx。`:74–103`的Campaign调用create没有传caller ctx，直到创建完成才将ctx交给elect.Campaign。网络不可达/租约grant卡住时，取消Campaign不会中断这一步。

反例使用实际clientv3和一个本机gRPC LeaseServer，LeaseGrant方法确认收到请求后协作等待**实际RPC ctx**取消。取消Campaign ctx，100ms观察窗无返回；关闭专属client后LeaseGrant及Campaign退出，错误为context canceled。没有hook替代NewSession、没有编译失败，listener/client/server均在测试结束关闭；这不是完整etcd server/HA验证。

建议将session setup和长期session生命周期分开：caller预算覆盖Grant/建立session/竞选等待，成功领导权继续由client/session维持，不能直接WithContext(campaignCtx)后在成功时defer cancel，否则原“Campaign成功后取消等待ctx不丢领导权”的契约会退化。优先复用clientv3租约、NewSession选项、可解除的context取消连接和已有状态锁，明确竞选成功/取消竞争时谁持有session及何时清理。先红后绿验证Grant取消、成功后caller取消、失败清理、session loss/Resign及重复竞选；不要用每次调用一个不可回收后台goroutine掩盖阻塞。Session.Close在失败/Resign时自身可能还有TTL级回收等待，属于同链后续预算验证点，不能仅修Grant就宣布全部选主有界。

## RR-20261004-NC-12

**P2：关闭预算位于同步依赖之后。** `etcd/watch_callback.go:67–85` CloseWithContext先requestClose，后select ctx；requestClose的sync.Once内部调用watcher.Close，没有ctx。第三方IWatcher可合法在Close等待自己的在途资源，包装层因此无法按caller取消返回。另一个并发CloseWithContext会先等同一Once，仍绕过预算。

反例经过正式WatchCallback及其返回的IWatchSubscription，底层watcher以门闩保留Close的实际收尾责任。已取消ctx下100ms无返回，释放门闩后返回context.Canceled，最终Close等到Done。另正常callback错误/显式关闭控制通过。**不声称默认core watcher在本次也永久阻塞**：其loop在发送队列处可选ctx退出，另有满队列关闭控制通过；这是公开包装对受支持第三方依赖的边界缺陷。

建议requestClose只负责幂等发起cancel/关闭，实际底层Close由每subscription唯一、可再次等待的生命周期收尾任务执行；Done应在handler和底层watcher真实退出后完成，多个caller只按各自ctx等同一个完成状态。关闭错误需要具名处理，不能提前close(Done)、每次重试派新goroutine、丢弃handler或允许callback内无期限等待自己。复用sync.Once/context/IWatchSubscription，并补并发Close、取消后再次排空、handler阻塞和底层关闭失败。

## 观察而非新增 RR

Discovery.Deregister等待lifecycleMu/loopDone未带ctx，选主Resign的session.Close还有独立TTL等待，后续应验证调用和取消责任，尚未新增稳定反例。Mirror Snapshot/JSON Clone是O(n)或与值大小相关的复制成本；每订阅有界队列不会限制订阅总数。Snapshot+revision+1、陈旧事件门禁、CAS与克隆机制不能替代真实集群恢复或事务业务幂等。本批没有benchmark或生产容量结论。
