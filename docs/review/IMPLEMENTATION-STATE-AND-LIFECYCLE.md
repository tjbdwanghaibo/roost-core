# 实现学习：锁作用域与可重试生命周期

证据基线见[进度表](PROGRESS.md)，本文对应第四轮 Core/Kit SHA。下面描述当前源码；建议不等于已实现。实际测试与限制见[运行记录](REVIEW-2026-09-09-04.md)。

## Entity Guard 与 Nest Cast

入口是 Core `nest/cast.go` 的 CastMulti，依赖 `entity/entity_guard.go` 的锁作用域。阅读到的顺序是：规范化目标 → 检查 Guard/dispatch/getter → 在加载前检查组锁顺序与远程声明 → GetMany → 校验已加载目标 → 排序并 Touch/RequireEntity → 返回实体。

Guard 允许重用已持有组；新增组要高于当前已持有最大组，避免跨组动态取锁破坏统一顺序。最新 maxLockedGroup/mayLock 抽取保留这一约束。这里的重点不是排序代码本身，而是加载前、加载后都不能绕过持锁规则。

远程 managed Entity 必须由消息预声明并在预分派阶段处理。本轮 refuseUndeclaredRemoteTargets 会接受已在 Guard 中的目标，拒绝未声明的 managed remote 目标；Cast 不是任意动态远程加锁入口。旧 RemoteReleases 管道移除后，不应继续按旧图谱函数名理解当前资源所有权。

失败路径只释放本次新获得的锁，不能释放调用方之前已经持有的保护。业务 handler 应把远程目标集合纳入消息声明，不能指望一次 Cast 动态扩展分布式事务范围。entity/nest 包 race 测试通过，但这不证明真实多服锁故障恢复已验证。

## Assembly 与 Runtime 停机

Core `dataengine/engine/assembly.go` 持有 Runtime；`runtime.go` 管理组件关闭。原问题是在 Shutdown 尚未成功时丢弃 Runtime，后一次调用因为没有对象而返回成功，形成“未完成却不可重试”。

当前 Assembly 在错误时保留 Runtime；Runtime 使用互斥与组件完成标记协调分阶段 Shutdown，重试能够跳过已完成阶段、继续失败阶段。关键不变量是：返回完成之前，未完成工作仍有可达的所有者；完成标记只能表达实际完成。

本轮复用原 overlay 复现并运行 engine race 测试，均通过。真实后端、完整进程重启和所有组件失败组合仍未覆盖。调用方仍应处理 Shutdown 错误并按生命周期协议重试，不能把超时当作成功。

## Session Claim 清理次序

Kit `service/session/service.go` 的冲突清理涉及幂等 claim 与会话丢弃。原 RR-20260909-02 中，先丢弃会话使其他调用能重新认领，旧清理随后删除的可能已经是新 claim（ABA）。

当前调整为先释放旧 claim，再丢弃冲突会话，使会话丢弃触发的重入不再落入旧的删除窗口。原独立复现与 session race 测试通过；这里验证的是该重入交错，不是所有真实后端原子性。

游戏登录链中，“清理失败尝试”也是并发协议的一部分。读到相同 key 不能证明仍是同一代所有权；评估清理逻辑时，应同时画出资源释放后谁能立刻重新进入，而不是只看正常成功流程。
