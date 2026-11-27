# Entity 伴生：按类型直取，共用一个本地锁域

适用例子是一个 `Player` 对一个 `ActivityPlayer`。这不是把两个实体合并：二者仍有各自的完整 ID、业务类型、DAO、提交与回滚参与资格。**合上**只表示同一进程当前可以安全地从主实体访问伴生，或反向访问主实体；**脱开**撤销这条直取路径，不删除实体，不改变它们的 ID。普通本地实体即可使用，基础能力不需要安装 Remote 服务。

## 定义与创建

**生成工程通常只要一行。** 在子实体原有 `//roost:entity` 标记上增加 `companionOf=Player`，运行 `roost generate`。生成器在总 registry 的 `post` 阶段登记两端类型，并为子实体生成 `CreateActivityPlayerForPrincipal(access, principal.ID())`；子包不需要为了注册再反向导入主包。见 [entity 标记与创建示例](../../codegen/docs/TAGS.zh-CN.md)。

没有使用生成器时，在两种 kind 及其工厂登记之后、创建实例之前，在应用装配处登记关系。`P`、`C` 是具体实体指针类型，两个 kind 不能互换：

```go
if err := entity.RegisterCompanionKind[*player.Player, *activity.ActivityPlayer](playerKind, activityKind); err != nil {
    return err
}
// principal 是已装入同一个 EntityManager 的普通本地主实体。
v, err := manager.Create(&entity.EntityCreateParam{
    IsCreate: true, Kind: activityKind, CompanionOf: principal.ID(),
})
```

`CompanionOf` 是主实体的**完整 ID**，不是玩家裸号。创建路径从中取 `UniqueID`，按目标 kind 调 `BuildEntityID`；不分配新的唯一号，也不从主实体复制类型位或 Remote 位。若显式 `Id` / `UniqueID` 与推导结果冲突就拒绝。一主允许多个不同 kind 的伴生，一个 child kind 只对应一个主 kind、一个唯一号只对应其中一份。`entity.CompanionID(principal.ID(), activityKind)` 与 `entity.PrincipalID(v.ID())` 是纯身份转换：即使能算出 ID，也不能用它证明曾创建、已合上或有写权。同号独立实体不会自动变成伴生。

## 日常代码只取实体，不取锁

```go
// 只向 Player 投递的普通 Nest handler，不需要给它额外传 ActivityPlayer。
a, err := entity.GetCompanion[*activity.ActivityPlayer]()
if errors.Is(err, entity.ErrCompanionDetached) {
    // 已脱开；此处没有可写的 a。需要独立写时用原有的显式消息途径。
    return nil, err
}
if err != nil { return nil, err }
// a 在本次 handler 的事务边界中，可像原来一样调用业务/生成的 DAO setter。
```

只向伴生投递时，用 `entity.GetPrincipal[*player.Player]()`。getter 不自动入组、装载冷数据或转成跨服 RPC。错误时返回类型的零值（上述指针即 `nil`）。没有合法 handler 作用域、要取的关系/方向不正确、实例不存在或没有受信声明，都不能把它们伪装成一次成功的 getter。`multi` 可以显式传双方，但若有多个不同的业务目标，并不存在唯一的“当前主体”，此时应使用 handler 收到的显式参数而非无参 getter。

**不要跨 handler、goroutine、Await 或独立事务保存返回指针当作写许可。** Go 不会在解绑时把调用方曾保存的指针变成 nil；作用域外重新入场就重新取。合锁不是事务语义的替代品：仍按 handler 的 rollback/durability 策略使用正常 DAO；裸 Go 字段不会自动变成持久记录。

## 脱开、重合与锁的边界

- 在主或伴生的 handler 里调用 `entity.DetachCompanion(childID)` 或 `entity.AttachCompanion(childID)`。`nil` 表示**本次执行受理了变更**，不是立刻交出一把锁。当前 handler 的视图保持稳定，不能把它当作“前半段独立、后半段重新合上”的切换开关。
- 对可回滚的事务，业务报错或提交明确拒绝不发布关系变化，成功才在旧/新域被保护的边界发布。正在排队的取锁者必须验证绑定代际和确切实例；读了旧视图的不能在新代际直接进入。不要在持锁 handler 内等待另一个操作把自己的锁拿走。
- `multi(P,A,C)` 和 `multi(C,A,P)` 都按实际物理域顺序获取、验证；**逻辑参与者不会因锁去重而消失**，三个目标仍可见，两个实体的修改仍是两份记录。原有显式传统组锁的限制不因伴生而放开。普通无伴生 kind 不进入伴生 handler 分支。
- 本地运维/装配要在**任何 Guard 之外**做立即切换，可用 `manager.AttachCompanion(childID)` / `manager.DetachCompanion(childID)`，不能拿 manager 入口绕过在途 handler 的提交边界。在生命周期变更期间遇锁冲突应遵守返回的错误，不应本地持锁自旋。
- 关系变更不是独立的业务 WAL 或跨服迁移凭据。冷恢复时不能仅凭可推导 ID 恢复写权；有权威关系记录的上层可在装配时显式声明已脱开的从属关系，再在确认为本服、当前实例有效时重新合上。进程间、实体重新创建与故障续做仍由原来的持久生命周期策略负责。
- 主实体离开内存时，仍在内存的已脱开伴生是否保留声明，只看持久数据是否真的删除，不看 destroy reason：持久主实体 `Destroy(..., deleteFromDB=false)`（业务离线卸载用 `DestroyReasonCommon` 或领域别名、框架 `MemoryUnload` 都一样）只是卸载，主实体从库重新载入后可以直接 `AttachCompanion`；删档（`deleteFromDB=true`，含 DataEngine 事务内删档准入后的内存收尾）结束主实体身份，清掉这些声明，同号新建的主实体须重新显式声明。不落库的主实体 kind 没有可重新载入的数据，业务的 Destroy 即结束身份，只有框架 `MemoryUnload` 保留声明。

## 和 Remote 怎么配合

上面的基础协议不要求 Remote。**涉及 Remote 时本地锁不能代替它的写批次。** 任一端为托管 Remote 时，创建使用 `CompanionOf` 加 `CompanionDetached: true`：先声明成对的身份，但不直接授权联写。以本地 Player + Remote ActivityPlayer 为例：

1. 在本服取得 ActivityPlayer 的独占所有权。向已脱开的 ActivityPlayer 投递一个单目标、可回滚、非 pipelined 的关系变更 handler，只调用 `entity.AttachCompanion(childID)`。该条消息必须带覆盖这个确切实例的独占写批次；不得把本机对象字段当作权威证明。关系从成功提交边界起生效。
2. 此后只投递到 Player 的活动 handler 也能用 getter 写 ActivityPlayer；框架在取业务锁前补齐必要的 Remote 写批次，并在域锁内重验实例及租约。已合上时直接切共享或转所有权会被拒绝。
3. 先用单独的关系变更 handler `DetachCompanion(childID)` 并等成功，再切为共享。共享期间 B 用原 `Castle + ActivityPlayer` 显式 multi 写；A 不得凭伴生身份直写。回 A 并退出共享**不等于已经合上**，须重新取得独占写许可，再显式 Attach。

V1 不在 pipelined handler 中受理合分（该提交模型会在结果确定前交出锁），不会暗改 durability；涉及 Remote 合分必须能明确确认这是一笔**没有 Remote 数据变更**的批次，不能把修改 Remote 数据与切关系塞进同一个 handler。普通业务的 Remote 数据写仍按原事务语义提交。生来 `ownership=shared` 的 kind 不满足独占要求，不能借伴生访问。不把这些特定限制误解成所有普通本地实体都需要写许可或 Remote 注册。

## 正确性验收

[独立上层用例](../../internal/companioncontract/)使用普通本地主从、真实 `EntityManager`、`Request` / `RequestMulti` 和可控制明确接受/拒绝的测试提交器。它不手填 Guard，不把隐藏实体偷偷补进业务消息，还在业务返回后、提交器给答复前由另一个真实 Nest 引擎竞争相同物理锁，使用注入的测试探针确认竞争已发生。业务侧直接 `GetMutex().TryLock()` 总是被拒绝，不作为持锁证据。应同时满足：失败恢复双方数据和原关系，成功才脱开；合上与否都不改变成员 ID；反向 multi 保留全部参与者并可完成。

```sh
go test -race ./internal/companioncontract ./framework/entity ./framework/nest ./framework/remoteentity
```

[SLG 主堡跨服方案](../maintenance/FEATURE-2026-11-19-activity-player-cross-server.md)是潜在使用者。本次没有把已有 `PlayerDao.Activity` 自动拆库，没有安装新的活动玩法，也没有将生来共享的 Facebook 变成本地独占实体。
