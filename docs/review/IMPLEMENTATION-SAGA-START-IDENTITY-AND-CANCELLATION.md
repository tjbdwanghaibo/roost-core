# Saga 启动身份、完成路由与取消恢复

2026-10-05；起点 `cb11be90`，最终实现以本文件所在提交为准。[本轮](REVIEW-2026-10-05-noncore-26.md) · [进度](PROGRESS.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-16/README.md)。接续[三消费者/持久恢复](IMPLEMENTATION-SAGA-CONSUMER-HEALTH-AND-DURABLE-RESUME.md)，不从头重审核心三域。

## 启动意图不是运行状态

Nest 通过 `NewStartEffect/EmitStart` 把启动意图放入事务 effect；DataEngine outbox 发布 `EffectEnvelope`，`SubscribeNestStarts → handleNestStart → Engine.StartSaga` 以 type/business_key 的持久唯一索引创建记录。首次创建不是步骤业务完成；重投必须回到当前记录，不能重置状态。

Record.Data 是下一步的输入，成功 `Complete → applyCompletion` 会替换它；DeadlineAt 可被 Resume 改写。NC-39 的旧重复判断直接比较这些可变字段，跨生命周期后“同一原请求”和“当前状态”已不相等。新 `StartDigest` 是固定原始 type/business_key/definition_version/data/deadline_at 的 SHA-256，ID 单独判断，Now 不参与。Engine 的各状态转换通过 Clone 保留摘要，正式 BSON 字段必须跨 Create、完整 Replace、重载与 List 保存。

新记录能够跨步骤推进、全部完成、恢复代际和重启保持原始身份。旧已推进记录无法证明原始意图，明确冲突；不能把现有 Data 当成“初始值”回填。自定义 Store/旧 writer 丢字段会失去重投证明，需要统一升级与可信来源的单独迁移，不是删除 Saga/回执重新执行。

## 完成信封要在副作用之前校验

`NewCompletionEffect` 的 Topic 是 `saga.result.<SagaID>`。原生消费者在 EFFECT stream 的宽 subject 上收多 Saga，所以前缀正确只说明消息类型，不证明路由身份。NC-40 增加 payload 解码后精确 Topic/SagaID 比较，错误 Permanent 返回，在 Engine.Complete 前停止。

合法结果沿 `Complete → Store.Apply` 同事务 CAS 更新状态，写 completion receipt/operation closure，删除相同 operation 的 outbox。重复 receipt 不再推进版本；已记录的旧步骤结果按既有幂等规则处理。新校验不改变 command/completion wire、EffectID/Key/Header 或发送 API，也不提供发布鉴权。实际 broker Subject/ACL、终止失败与重连另验。

## 两类取消不能混用

MongoCommandInbox 的 handler 只用传入 txCtx 写 Mongo。命令预约、业务写与完成回执同事务；取消导致失败时不返回业务成功。新控制在业务写后取消，确认替身私有事务无业务/回执可见；换有效 ctx 重试成功，再投递只读回执，不重复 handler。

原生 DataEngineStepInbox 不执行该 Mongo 业务事务，它先持久 Reserve lease，再由正式 Nest handler `Bind` 把 token/digest 的 LeaseFence 与命令 receipt 写进同一 WAL；EmitCompletion 绑定可回放完成 payload。消费者的 handler 返回成功还不够，要等权威 receipt 投影后 `waitReplay` 才完成投递。

普通取消不证明事务未准入，故 lease 保留，其他重投不能马上重跑业务；晚到权威 receipt 后 Reserve/Replay 标记 claim completed。明确 `ErrFencedEntityPending` 表示这次 WAL 前拒绝，才使用不受消息取消影响的上下文交还未用 lease；恢复后 Reserve 得到递增 token。新两控制通过，手工注入权威 receipt 只证明消费契约，没有把它写成实际 WAL/投影或跨进程的验收。

## 事务控制证据的边界

正式 MongoStore 在状态 Replace、completion Insert、operation Insert 后分别触发确定性取消，替身事务未发布任何半写，outbox保留；新的有效 ctx 重试后状态/摘要/回执/outbox一致，重复完成不双结算。额外强制一次 callback retry，最终版本只前进一次。所有取消 hook 都断言实际执行，不用任意 sleep；上下文失败与编译失败不混为产品红。

这些测试使用 mongotest 的私有快照模型。真实 Mongo 客户端/driver 的 CommitTransaction 可能有结果未知、网络超时和自身重试，它们不能由本机替身证明。没有更改业务回滚、DataEngine 的 unknown/fenced 策略或 production session；更不能把取消返回直接当成允许删除 WAL/回执的依据。

## 指标与设计评价

`servicemetrics.Reporter` 是服务事件契约；Sink 转发可选 reporter、非正 Dropped 不上报，Depth 是 gauge。Kit 的同名类型是 core 别名；Recorder 为各服务测试保存计数，内部锁保护读取，Snapshot复制、Events排序。既有计数/并发回归纳入本轮，未发现本次已查源码中的新功能反例；这不证明每个服务的每个错误分支都有上报。Recorder 的排序/复制与内存存储适合测试，不能直接当生产容量/监控后端。

当前 Saga 复用存储唯一键、CAS、outbox、receipt、LeaseFence 的分工合理，无需为两个身份问题另建 manager/cache 或重试层。启动新增 JSON/hash 是有限控制路径开销，未测性能；完成校验是写前字符串比较，不新增 I/O/锁/goroutine。真实吞吐、GC、长容量与 HA 留在外部验证。

## 为什么此前漏检

过去启动测试停在 Create 附近，即使两次请求相同，也没有让 Data/DeadlineAt 正常变化后再重投；原生完成测试验证“有人消费”，没有反向把信封路由和 payload 做合法异键。与上轮 NC-38 的 BSON 代际遗漏不同，本轮缺口是跨阶段身份判断及接入层写前绑定。

后续按“原始身份 → 可变运行状态 → 持久表示 → 实际重投”审查幂等；按“合法异键 → 无副作用拒绝 → 修正后恢复”审查信封；取消一律继续验证新 context/新代际到最终收敛。测试数量只辅助记录，不能换成全仓覆盖率。
