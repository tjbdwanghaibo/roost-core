# 对等实体组：任一成员的 Nest handler 都能改同组成员

对等组只是当前 `EntityManager` 里的一个非零 `GroupID`。没有组长、组实体或组 DAO；每个成员保留原来的 EntityID、持久化文档和同步身份。适合人数有界、经常需要一起写的本地实体。成员关系只在内存有效；删除、卸载或服务重启后，由业务根据自己的可信信息重建，不由可推导的 ID 恢复写权。

## 业务用法

```go
// 下列三个片段分别运行在已经注册、由 Nest 调度的 handler 里。
// 1. 单目标消息投给 A：把本次显式目标 A 加入 groupID。
return nil, nest.JoinEntityGroup(groupID)

// 2. A、B 都完成加入后，只给 A 发消息即可修改 B；反过来也一样。
b, err := nest.GetGroupMember[*actors.Worker](bID) // 完整 EntityID，同组允许多名同类型成员
if err != nil { return nil, err }
b.AddEnergy(delta) // 使用业务原有的数据修改/标脏方法；不需要锁，不再投一条消息
return nil, nil

// 3. 单目标消息投给 B：提交成功后 B 才离开。
return nil, nest.LeaveEntityGroup()
```

- 先用**各自的单目标 handler**加入；第一名成员不成为组长。Join 不是从另一个组抢过来的 Move。明确成功的事务边界才发布新关系；Leave 返回到当前 handler 并不意味着原组已解锁。当前 handler 的视图保持到结束，下一次从 A 访问已离组的 B 才收到 `nil, nest.ErrGroupMemberUnavailable`。失败不发布；提交结果不确定时不发布并沿用 Nest 的 fence，不把不确定解释为成功。提交已确定（含 `RollbackNone` + memory 的已准入修改）之后、发布前的复核才失败（例如引擎恰被另一个 worker fence）时，handler 的修改照常作为已提交保留、同步照常放行、消息不重排，只是关系不发布，回复带 `nest.ErrAfterCommitFailed`。
- `GetGroupMember[T](id)` 只在当前 handler 的一个无歧义组视图内有效：类型不匹配、非成员、过期实例、子 goroutine、独立嵌套事务等不返回成员。不要把返回的实体指针缓存到下一条消息、回调或其他 goroutine；Go 的业务字段不是可自动撤销的引用。
- 现有 `CurrentEntityLockGroup().Get/Range`、`GroupEntityAs` 也必须通过相同视图检查。显式目标数组仍保持调用方的顺序和重复项；组内的隐式成员不会追加到业务参数里。跨多个组的显式 multi 可以执行，但无组参数的 `GetGroupMember` 不选择默认组；业务应使用已显式声明的目标。
- 框架用去重后的组锁按固定顺序 `Try`，在锁内收集实例并固定生命周期，再按实体锁序尝试加锁、校验成员代际；拿不到就释放已得资源，不持一组阻塞等另一组。真正可访问的成员分别进入事务和同步：A/B 是两个逻辑参与者，仍用各自已有的持久化与回滚协议。组锁不是单文档提交，也不替数据模型生成缺失的持久化适配器。大组会扩大每条消息的准备和快照成本，不宜把整张地图塞进一个组。
- 安全的首版支持本地成员。Remote 或已合上的伴生锁域不作为隐式组成员，不能借组访问绕过 Remote 的所有权或写批次。变更成员关系不接受 `pipelined` 或带 Remote 写批次的 handler；冲突时返回可判别的错误，业务不要在持锁的 handler 里循环等待。已有引擎外的 `RequestJoinEntityLockGroup/RequestLeaveEntityLockGroup/RequestMoveEntityLockGroup` 是独立异步操作，准入并不等于已经修改；事务内应使用上面的 Join/Leave。
- 默认 ManagerAccess 的组锁表随 EntityManager 身份共享，而不是随 Nest 实例分裂。一个实体实例只允许发布给一个 Manager；不同 Manager 的同号实例仍相互独立。公共广播按目标独立准入与重试；try-batch 的组成员回到完整的单目标事务。组竞争不会重放一批里已经成功的广播目标。

## 锁的归属：上锁是框架的事

从构造起，**实体的 `GetMutex()` 不是业务取锁入口，即使在 Nest handler 内也不是**。`Lock()` 和 `Unlock()` 立即 panic `entity.ErrEntityLockScope`；`TryLock()` 和 `LockWithTimeout()` 立即返回 false；直接调用 `GetMutex().LockContext(ctx)` 返回 `ErrEntityLockScope`，`lock.LockContext` 适配器在有效 context 下也返回同一错误。`LockId()` 仅供读身份；没有给 handler 或实体打开一段时间的公共布尔标记。缓存返回值、同一 goroutine 重入或加入对等组都不会获得一张业务锁票。

在业务层用 Nest handler、组 getter 和 Join/Leave；底层封装内部完成取放锁。同步抓快照、卸载、Remote ownership 等有各自的系统入口，内部拿锁不把能力转给 packer、hook 或业务回调。`GetEntityGuard()` 只查当前作用域，没有时返回 `nil`。创建 Guard、手动 Require/Release/ReleaseAll、安装伴生作用域、替换事务捕获器，以及接收任意回调的 `WithLocked` 都是框架维护入口。业务可以调用由框架完整取放锁的具体操作，但不能借 Guard 提前释放 Nest 的锁。第三方自己创建、自己保留的原始 `lock.Mutex` 无法靠 Go 包装器撤销；不要把自定义锁提供者或框架内部代码当作不受信的业务插件隔离边界。

[伴生实体（主从有方向）](ENTITY-COMPANIONS.md) 与本协议不同，不要为对等组派生主从 ID。
