# 修复验收：从原失败到新的错误契约

2026-09-13，Core `66f65e258bc5655d929e54300dc9a18377ff7e3b`。[本轮实测](REVIEW-2026-09-13-08.md)解释以下机制，建议与未验证边界明确区分。

## 缓存拒绝必须覆盖实际写入

[remote_snapshot.go](../../entity/remote_snapshot.go) 将墓碑检查交给 StoreConfig.Superseded，在 L1 分片锁内对回填与发布一并判定；ReadThrough 被拒后返回现值或 miss，避免缓存没写进去却把已删除的旧值交给业务。FatalRemoteError 决定实际 L2.Set 返回的错误能否降级，预检查不再被当成并发屏障。对实现 RemoteSnapshotVersionedDeleter 的 L2，删除也有版本比较；未实现该接口的适配器仍需单独验收。

Superseded 的资源锁序为缓存分片锁到 tombMu；应避免逆向持锁。此次证明的是本机墓碑和版本化删除能力，不意味着其他节点不能重新写入旧值，也不意味着 TTL 自然覆盖所有 broker 重放。

## WAL：先等记录，再同步文件

[wal.go](../../nestwal/wal.go) 的 Sync 先将 barrier 放入写队列，收到 writer 回答后才 syncActive。屏障结束 BatchDelay 收集，使已准入记录不能留在内存而 Sync 返回成功。Flush/Shutdown 在 [committer.go](../../nestwal/committer.go) 使用可取消的 semaphore 等待；超时是停止等待，不保证用户 applier 已退出。原真实文件和截止复现均通过，磁盘卡死与并发 Close 仍需另验。

## 完成回调：事务已提交与后处理失败分开报告

[rollback.go](../../nest/rollback.go) 逐项恢复回调 panic 并返回 ErrAfterCommitFailed，使后续释放回调仍有机会执行；[pipelined_completion.go](../../nest/pipelined_completion.go) 把错误送回等待者。业务不能把该错误等同“事务未提交”并盲目重试写操作。独立验收必须检查回复、释放和进程存活，不能只检查 recover 日志。

旧测试要求固定成功值，会在新的显式错误契约下失败。正确适配是检查明确的错误类型、保留正常分支和退出断言；不能只删掉 err 检查让测试变绿。

## 所有权未知结果：查询权威再决定

[ownership.go](../../remoteentity/ownership.go) 在 Transfer 错误后清本地 marker，使用独立有界 context 重查：未变才恢复；已转移则 fence 并返回已确认结果；无法查询则 Recovering 阻止准入。本轮真实 Redis 已执行 CAS 后丢回复的场景证明旧 owner 不再写入；发送前失败对照证明正常恢复仍存在。该结论不外推到 EnterShared/LeaveShared 或跨机故障恢复。
