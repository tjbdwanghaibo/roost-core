# 实现学习：恢复、保留与重放的生命周期

Core `f3eaad9b38f87b2873e6f35b517b4ad993aed680`，Kit `c4e7cef1029fb6326fa88b84c622f059bc62c0c4`。[验证记录](REVIEW-2026-09-10-03.md)；均为有界场景，未连接真实资产/存储服务。

## 远程事务：通知与结果需要共同存活

transaction_manager.go 用 txMu 保护 txs，tracker 保存 done、终态、关闭时间。完成路径写 status 后关闭 done；容量和 TTL 清理可删除已关闭 tracker。FlushRemoteTransaction 和严格提交等待路径进入 waitRemoteTransaction，先保存 done，再等通知。

当前唤醒后通过 map 重新找结果，形成 RR-20260910-03：通知仍可达，结果却已被清掉。缓存有界不是问题本身，问题是缓存条目还承担着活动调用的完成结果。设计上应明确历史查询保留时间与活动等待者生命周期；后者不能只依赖前者。

本轮审读 finalizer 停止、重试和容量管理，但没有完成真实后端停止竞态验证。不要将等待者复现的确定性推广为所有 finalizer 故障都已覆盖。

## 技能快照：排序稳定不等于恢复语义相同

runtime_checkpoint.go 对版本、校验、宿主 revision/authority、程序身份及恢复状态作校验；序列化把 map 转为排序序列，使输出可重复。恢复过程重新构建 Cast、process、frame、调度任务等引用关系。

runtime_retention.go 的 completedCastOrder 保存实际完成顺序，达到 CompletedCastLimit 后扫描可淘汰条目。当前 checkpoint 只按 CastID 输出 Casts，Restore 从这个序列重建完成顺序，导致 RR-20260910-04。即使所有 Cast 字段完整，顺序本身也是影响后续历史保留的状态。复制两个独立 Host 的同一前史再施加同一后续输入，可以检查这种遗漏；仅比较恢复当下字段不足够。

本轮差异仅证明 InspectCast 可见历史不同。RootEvent 和 ProcLedger 也有保留机制，但未验证它们是否存在同类行为；不能直接据此推断战斗、随机或奖励出现分歧。

## 取消失败：保留可重试资源

runtime_cast_window.go 的 Cancel 先取消 phase tasks、改变 token，然后执行 cancel 回调和 stopProcesses；停止失败时返回，未完成最终 Cast 状态迁移。process.go 在 Host.StopProcess 返回错误时保留运行状态，允许重试。成功后才推进停止状态与最终释放。

临时测试在带 tick 回调的 spawn process 上令首次 Host.StopProcess 失败；再次 Cancel 成功，Host 无活动 process，CastFinished 且可 checkpoint。另一个 wait 技能在 Cancel 后 checkpoint/restore，推进到原到期 tick 以后没有复活 frame/task。这里只证明对应失败点与 wait 分支；回调自身失败、宿主重入、重复外部副作用仍需单独检查。

## 邮件：发送回执不代替领取去重

Send 的账本持有 envelope 与投递进度，重放可补做未完成投递；Deliver 对仍存在的 mailbox entry 保持已有状态。领取链的 token 与 claimed 状态属于玩家邮件条目，和发送回执不同。

本轮两项实际验证：投递成功但 stamp 更新失败后，同 RequestID 重试修复 stamp，已领取条目的 token/Unread 不变；广播先送达玩家 1 后返回错误，玩家 1 领取，再重试补到玩家 2，玩家 1 不会重新变成可领取，成功后的再次 Send 不再 fanout。

两项都以 entry 仍存在为前提，不能关闭上一轮 RR-20260910-02（淘汰后重投）。而 Deliverer 契约允许接受异步投递任务后返回，DeliveredAtUnix 不能一概解释成所有玩家邮箱已可见。生产发奖还需要资产侧幂等键及其保留期与邮件重放窗口配合；本轮未验证真实资产账本。
