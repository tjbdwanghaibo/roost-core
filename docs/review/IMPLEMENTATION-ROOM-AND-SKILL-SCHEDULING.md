# 实现学习：Room 生命周期与 Skill 延迟任务

本次阅读基线：Core `8ea815930b4b32bf59cb4a894e50782687046e0d`。仅说明已读机制，不代表全包完整审计。证据见[运行记录](REVIEW-2026-09-10.md)。

## RoomManager 的资源所有权

实现位于 Core `room/room_manager.go`，不在 kit/service/room。Create 在管理器锁内检查 running/stopped、房间 ID 重复与容量，创建 Broadcaster，注册共享预算与活动回调，启动后纳入 rooms。Get 拒绝返回正在关闭或已停止的房间，成功访问更新活动时间。

Close 先令管理器停止接收新房间，再停止清扫线程，按 ID 顺序关闭已有房间。取消/超时会保留房间条目供后续重试；成功结束的条目移除。Remove 同样对取消/超时保留引用。这避免“操作未结束却忘记资源”的假成功。

Broadcaster 的 Close（room_broadcast.go）先等待 stop；取消/超时直接返回，不立即释放仍被使用的对象。结束后取消回调、重置同步 sink、关闭 coordinator、归还共享 subject/subscriber 预算并清空状态。已有 RoomManager 超时重试和预算回收测试随本轮 race 包测试通过；没有测试真实下游永久阻塞或所有生命周期竞态。

空闲清扫同时要求最后活动时间达阈值、无活动 subject/subscriber、无待处理 subject/retirement。它不是仅按创建时间删房。Get 也算活动，因此观测房间过期应使用 Stats，反复 Get 会改变被测条件。默认值推导仍需满足定时器约束，本轮 RR-20260910-01 表明“原始参数为正”不能保证“派生周期为正”。

## Skill：任务身份、快照和取消

阅读 Core `skill/scheduler.go`、`runtime_cast_window.go`。调度堆按 DueTick 再 Sequence 排序；相同 tick 的顺序由序号决定。挂起工作保存 FrameID，retainLocalFrame 克隆局部变量，恢复时 takeLocalFrame 删除该帧并再次克隆，避免不同挂起分支共享可变 locals。

普通 cast 任务携带 CastID/PhaseToken。执行时取得并消耗局部帧、减少 pendingTasks，然后检查 cast 是否存在、阶段是否匹配、是否结束/失败；旧阶段任务不继续执行。系统任务（如弹药恢复）另有路径，不能把普通任务的检查推广到所有任务类型。

Cancel 在 Runtime 互斥锁内取消当前阶段排队任务、清理它们的 frames、推进 phaseToken，再处理取消操作/进程停止与策略槽释放。releasePolicySlot 比较当前槽的 cast ID，避免删掉另一个 cast 后来取得的槽。

本轮独立测试从真实 wait DSL 编译并激活，确认存在挂起帧；Cancel 后检查 frames 与 pendingTasks 清零，Advance 越过原触发时间，目标生命值未减少。它验证成功取消的清理链；取消回调失败、进程停止错误、宿主重入和 checkpoint 恢复仍需另审。

## 接入时的含义

Room 的关闭超时仍需要重试；Skill 的逻辑 tick 推进和取消操作应通过 Runtime 入口处理。资源“已标记停止”和“已完成回收”、任务“排队过”和“仍有资格执行”是不同状态。测试应分别断言状态、资源数量和最终业务效果，而不只检查返回 nil。
