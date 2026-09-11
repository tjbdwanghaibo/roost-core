# 实现学习：远程写入预分派与收尾

本轮 Core 基线 `76664a51be77bdeb79a62cf344d5cee0ecc15daa`。源码阅读与测试边界见[运行记录](REVIEW-2026-09-10-02.md)。

## 预声明与批量准入

Nest 的 `nest_dispatch.go` 从 Tid/Tids/GroupTIds 收集 managed remote ID，调用 Manager.PrepareRemoteWriteBatch。Broadcast 被拒绝，因为一次广播会拆成多个 handler 事务，与单个批量远程事务不一致。没有配置写管理器或返回 nil batch 时也拒绝进入。

`remoteentity/batch.go` 先校验/排序去重 ID、检查最大批量与后端，再预留 finalizer 容量。逐项 beginWrite 取得本地 writeGate 和 ownership 读锁，检查/取得所有权；共享模式还取得带 fence 的分布式锁、重新核对 marker，加载并校验实体，建立版本向量与写租约。

如果后续实体准备失败，已加入 batch 的项经过 Abort/Close 回收，当前失败项释放自己的引用。本轮独立构造第一个实体存在、第二个缺失的批次；失败后用同一上下文再次准备第一个实体成功，验证该场景没有遗留 gate。锁/所有权/存储采用既有测试替身，不是 Redis/Mongo 故障注入。

## Finalize 与落库不是同一阶段

FinalizeLocked 读取事务本地变更及显式删除意图，调用实体参与者生成 commit，补入 transaction ID、base/next version、marker/lock fence/route epoch，验证后保存克隆。后续项失败会回滚先前已 finalized 项。Nest 再把这些提交纳入同一 WAL 记录的 remote mutation。

`nest/msg.go` 收尾区分：未 finalized 或 handler 失败走 Abort；正常走 Commit；已经标记不确定时不再当成明确失败 Abort，而通过 Close 将持有资源交给状态驱动的 finalizer。这个区分避免在 WAL 可能已接受时做错误回滚。

Batch 的 Commit 按 Durability 分支处理：直接 Apply、异步返回推定 receipt、或等待事务状态。异步 receipt 不能独立证明持久化已经完成。Close 对 indeterminate 或异步未完成提交尝试转交资源；普通路径释放 entries 与准入名额。

本轮相关包测试包含异步提交在 WAL Apply 前保留 gate、Apply 后可再次写入，以及 Nest 不确定结果不 Abort。只记录这些被验证场景，不将它们外推为网络分区、真实数据库 fence 或进程重启恢复的完整证明。

## 快照读取是另一条链

`nest/remote_access.go` 在 handler 前收集 RemoteAccess 声明，校验 alias、引用、模式和一致性，解析快照后核对实体/种类/scope/route epoch，再校验版本/时效。Required 失败中止，optional 失败跳过；成功快照放入当前上下文，Remote[T] 按 alias 与类型读取。

只读快照和 managed remote 写批次解决不同需求。调用方不能把取得快照当作取得写租约，也不能把应用层函数返回理解为所有持久化级别都已完成。下轮继续检查 finalizer 队列耗尽/停止、真实锁租约超时和快照发布重试。

## 第三轮补充：完成等待者与缓存保留

Core f3eaad9b38f87b2873e6f35b517b4ad993aed680 已独立复现完成 tracker 被淘汰后等待者 panic（RR-20260910-03）。上文描述的是第二轮历史基线，不代表此边界已安全。通知和结果生命周期的分析见[恢复与重放](IMPLEMENTATION-CHECKPOINT-AND-REPLAY.md)，验证见[第三轮记录](REVIEW-2026-09-10-03.md)。

## 2026-09-11 等待生命周期验收

Core e22e815934293d3bc25a84f37338f9576f9d5c4d 的 U-0168 已通过原容量淘汰复现与新增 TTL 清理复现。等待者持有 tracker 引用，map 清理不再丢掉活动等待结果；[当前机制](IMPLEMENTATION-CHECKPOINT-AND-REPLAY.md)和[验收记录](REVIEW-2026-09-11.md)补充这一点。finalizer 停机与晚到 batch Close 的资源交接仍待独立验证。

2026-09-11 专项续审：正常链路源码未变；晚到 Close 与停止交接已复现问题，见[生命周期与性能补充](IMPLEMENTATION-REMOTE-NEST-LIFECYCLE-AND-PERFORMANCE.md)。
