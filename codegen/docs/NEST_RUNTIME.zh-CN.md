# Nest 业务执行模型与生成器

## Handler 定义

新项目使用 `//roost:nest`。`target`/`targets` 明确指出函数开头有多少个
Entity capability 参数，因此 capability 无需以 `Entity` 命名。

```go
type BagOwner interface {
	Bag() BagComponent
}

//roost:nest target=player
func handlerUseItem(owner BagOwner, cmd UseItemCommand) (UseItemResult, error) {
	return owner.Bag().Use(cmd)
}

//roost:nest targets=player,player rollback=undo durability=strict
func handlerTransfer(from BagOwner, to BagOwner, cmd TransferCommand) error {
	return transfer(from.Bag(), to.Bag(), cmd)
}
```

`targets` 以逗号分隔且不包含空格。Entity 参数必须位于普通参数之前。切片
capability（例如 `[]UnitOwner`）生成 MultiGroup 调用。`sync` 可为没有返回值的
handler 强制生成等待完成的同步 Sender。

需要配置、时钟或只读查询依赖时使用指针方法 Handler：

```go
type BagHandler struct {
	configs ConfigReader
	clock   clock.Clock
}

//roost:nest target=player
func (h *BagHandler) handlerUseItem(owner BagOwner, cmd UseItemCommand) error {
	return owner.Bag().Use(h.configs, h.clock, cmd)
}

handler := &BagHandler{configs: configs, clock: clock}
baghandler.RegisterHandlerBagNestHandlers(handler)
```

同一源文件不能混合包函数和不同 receiver 的方法 Handler。方法 Handler 需要实例，
因此不会进入无参 bootstrap；启动层必须显式构造并注册，注册时传入 nil 会立即失败。

`//roost:nest` 是现行标记；旧 `rollback=dirty` 不属于生产协议，升级项目需改成 `state` 或 `undo` 并重新生成。

## 注入式 Sender

假设源文件为 `handler_bag.go`，生成器在 sender 包生成 `BagSender`：

```go
asyncSender := bagsender.NewBagSender(nestClient)
syncSender := bagsyncsender.NewBagSender(nestClient)

if err := asyncSender.Send_UseItem(ctx, playerID, cmd); err != nil {
	// queue full、engine stopped、context canceled 必须在接入层处理
}

result, err := syncSender.Sync_UseItem(ctx, playerID, cmd)
```

Sender 只依赖 `nest.Client`，不会读取全局 `nest.Nest`。因此单元测试可以注入
fake Client，生产环境从 `app.Registry` 取得 kit `nest.Mod` 提供的 Client。

## 锁与跨 Entity 规则

- Handler 中所有会被修改的 Entity 必须全部出现在 Entity 参数列表。
- Multi/MultiGroup 由 core 统一排序并加锁；业务不得自行访问 EntityManager。
- Handler 内禁止再次 Dispatch/Request，避免锁顺序反转和死锁。
- 外部 I/O、跨服务 RPC 和消息发布放在锁外，通过 Saga/Outbox 编排。
- Handler 参数和异步命令在成功入队后视为不可变；需要修改的 DTO 应在接入层复制。

## Data Engine 持久化规则

生成 DAO 的持久化变化只记录在当前 Nest transaction 中，不再维护第二套 DAO 级异步快照
dirty。带持久化属性的 setter、map Set/Del 只能在 Nest handler 或 system transaction
内调用；事务外修改会 fail fast。`durability=memory` 的 handler 不能修改 persistent
字段，低隔离但仍需落地的操作应使用 `durability=async`。这条是运行期强制的：`rollback=none`
（memory 快路径）没有事务，持久 setter 直接 panic；`rollback=state|undo` 的 memory 事务在提交点
发现持久字段改动（或 `AddMutation` / receipt）时整笔失败，按回滚策略撤销内存修改，错误满足
`errors.Is(err, nest.ErrMemoryTransactionPersistentWrite)` 与 `ErrCommitRejected`，并点名
`collection/id` 与字段；带 Remote 批次的 memory handler 改本地持久字段同样被拒、批次 Abort
（RR-20261006-41，v1.23.1 起；之前改动静默丢失）。只改 nopersist 字段的 memory 事务不受影响。

手写 `nest.HandlerMeta`（不经 `//roost:nest`，例如动态注册或测试）时，`Rollback` 不为
none 就必须显式写 `Durability`：`DurabilityMemory` / `DurabilityAsync` / `DurabilityStrict` /
`DurabilityPipelined` 选一个。没写的在注册时失败（`RegisterHandlerWithMeta` 与实例级注册都是），
错误满足 `errors.Is(err, nest.ErrDurabilityUnset)`，点名 handler 并列出可选值（RR-20261006-60，
v1.23.1 起；之前 `Durability` 零值即 memory，而标记缺省是 async，忘写的要到运行期第一次改持久字段才被上一段的规则拒绝）。
`HandlerMeta{}`（rollback=none）照旧是 memory 快路径；显式写 `DurabilityMemory` 的照常可用；
生成的 `HandlerMeta` 总是写明 durability，不受影响。

同一事务对同一 DAO 的改动合并成一个 mutation：新建、migration/replace 和全字段写为
Put；已存在文档的普通修改为字段级 Patch；删除为带 version 的 Delete/tombstone。Patch
不携带 full fallback。Entity release 只处理生命周期和 sync，不编码 BSON 或触发落库。

```go
//roost:nest target=player rollback=undo durability=async
func handlerRename(owner PlayerOwner, name string) error {
	owner.PlayerDAO().SetName(name) // transaction-local Patch
	return nil
}
```

需要启动 Saga 时在 handler 内调用 `saga.EmitStart`；Native Entity step 使用 Data Engine
inbox，把 mutation、step receipt 和 completion effect 放进一个 CommitRecord。Raw Mongo
step 继续使用 `MongoCommandInbox`，不能在其 Mongo transaction 中混写 Nest Entity。

从历史快照数据迁移时，先升级所有 WAL reader，再启用 writer v2，最后重新生成 DAO 并完成
一次性数据验证。运行时只有 `persistence.engine=dataengine`，不存在双写或回切旧引擎；完整
导入步骤见 core `docs/DATA_ENGINE_MIGRATION.md`。

## 升级步骤

1. 把宽 `IPlayerEntity` 拆成 handler 所需的窄 capability。
2. 把 marker 改为 `//roost:nest target=player`。
3. 重新运行 `roost generate` 或 `cmd/nest`。
4. 在 endpoint 构造函数中注入生成的 Sender。
5. 将包级 `Send_*`/`Sync_*` 调用改为 Sender 方法；生成器不会保留旧函数。
6. 删除业务 Runtime、Controller 透传层和全局 Access 获取 Nest 的代码。
7. 确认目标环境 WAL reader 已兼容 v2、writer 已设为 v2，再重新生成持久化 DAO。
8. 用严格/异步/pipelined handler 测试 Put、Patch、Delete 和 rollback，并验证只有 Data Engine 一条写路径。

`durability=pipelined` 自 v1.23.1 起可由标记生成；生成代码不绕过 Nest 的 committer 能力检查和 handler allowlist。
