# Outbox与历史收口实现（未发版）

对应[说明OC1～OC6](OUTBOX-CLOSURE-2026-10-08-NOTES.md)。基线88fc852c，第一批36ce227c/55f2cc20/42eb95e8，后续由包含本文的提交承接；行号为本批冻结源码。测试命令、实际结果和原始证据位置统一见[验收](../review/OUTBOX-HISTORICAL-CLOSURE-2026-10-08.md)，本文不把测试文件存在当作通过。

| 编号 | 入口 | 不变量与review检查点 |
| --- | --- | --- |
| OC1 | `remoteentity/transaction_manager.go:685`；`remoteentity/outbox_publish.go:27`；`kit/remoteentity/config.go:109` | RecoverOutbox共用Serial与retryWG；完整事务的全部Entity建立依赖；页内最多64worker，页间排空；取消停止新派发并排空在途；失败保留Applied，不能偷改strict成功边界 |
| OC2 | `entity/remote_protocol.go`、`remoteentity/backend.go`、`remoteentity/mongo_committer.go`、`remoteentity/snapshot_client.go`、`cache/layered.go` | 游标前进；坏记录不删除；兴趣队列有界且Stop等worker；元数据容量不能靠TTL无限增长 |
| OC3 | `entity/entity_manager.go:195`；`lock/lock.go:11` | 取消发生在初次ctx检查后、锁仍被别人持有时也能返回；Touch和本次锁层级回收；删除已准入或未知后生命周期完成 |
| OC4 | `nestwal/inspect.go:48`；`nestwal/inspect_snapshot.go:37`；`cmd/walinspect/main.go` | 只读锁定原目录；坏帧停止不猜边界；完整复制后缀和checkpoint；哈希+报告+无INCOMPLETE共同判断完成；不调用Open修尾 |
| OC5 | `sync/frame/receiver.go:40`；`client/dotnet/Roost.Client/SyncReceiver.cs:26`；`client/unity/Runtime/RoostConnection.cs:26` | 应用成功才推进基线；重复/旧代不应用；失败拒绝后续Delta；Reset隔离旧回调；新连接不能被旧回调关闭 |
| OC6 | `remoteentity/outbox_crash_integration_test.go`；`etcd/driver/cluster_failover_integration_test.go` | 精确持久Applied后SIGKILL；同sid真实恢复；实际Raft leader强杀；本地进程与跨主机网络分开报告 |

红绿证据：RR-46修前memory/durable各超时，RR-47修前无关事务被阻塞；见[RR-46](../bug/RR-20261008-46.md)、[RR-47](../bug/RR-20261008-47.md)及对应bugfix。并发守卫含第二个Entity共享、worker上限、取消不误标Committed、停止屏障；不能只测首Entity相同。

新增计时无entity/transaction高基数标签。confirm_wait覆盖调用方等待，可能与outbox阶段重叠，不能直接相加为请求耗时。新增Histogram仅记录完整发布尝试。性能复跑须独立新label，不能拼接中断长稳。
