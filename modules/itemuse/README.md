# itemuse：按服务端配置使用自己的物品

`ItemUse` 把**扣背包 → 执行用法 → 记成交回执**放在同一个 Player 的 Nest 事务。道具号决定用法；客户端不能指定玩家、效果、奖励组、概率、经验或产物。丢弃仍走 `BagDiscard`；抽卡、任务领奖以及独立 `reward.Roll` 的协议和行为不变。

## 安装与配表

| 命令 | 可执行哪些配置 |
|---|---|
| `roost module add itemuse` | 自动安装 `bag`，可执行工程注册的本地效果。没有注册的效果拒绝，不会仅扣掉物品。 |
| `roost module add itemuse reward` | 另启用 `reward.group`：每个箱子抽一次并实际入包。 |
| `roost module add itemuse profile` | 另启用 `profile.exp`：调用当前玩家 `Profile().AddExp`；`enter` 是 profile 本身的依赖。 |

两座桥可同时安装，也可后装。无需 `quota`、`wallet` 或全量模块组。模块声明玩家 TCP 接入与本人同步；运行时使用工程已有的认证和持久化设施。

在 `configs/table/item.csv` 定义输入和奖励物品。在 `configs/table/item_use.csv` 为**可以使用**的输入加一行，再执行 `roost generate`：

```csv
id,effect,arg,max_batch
1003,reward.group,1,4
1001,profile.exp,25,32
```

- 第一行让 `1003` 打开奖励组 `1`。`arg` 是 `reward_group.csv` 的组号，条目定义在 `reward_entry.csv`。例如组行 `1,0,false`，条目行 `1,1,1002,2,0,0,fixed`，表示每箱给 `1002 × 2`。开两箱扣两个 `1003`，入包四个 `1002`，结果清单只含一项。组号不存在或条目无效是失败，不是无奖。
- 第二行让每本 `1001` 给 25 经验，用两本得 50；`Applied=50`。装备、金币等没有用法的物品不因背包中有数量就可使用。删除用法或从 `item.csv` 下架物品会阻止新的使用，不擦除已经成功的回执。
- 以上 ID 仅是可替换的联装示例。运行时不依赖它们；两座桥都未装时，这两条示例用途都不生效。每次请求读固定的配置快照。

## 客户端和服务端 API

客户端消息只有 `pb.ItemUseRequest{Sequence: 1, ItemID: 1003, Count: 2}`；应使用工程生成的 **ItemUse 消息号**发送，玩家身份来自认证会话。控制器 `HandleItemUse(ctx, req)` 经过生成的 Nest Sender。成功时 `Code=0`，`Sequence/ItemID/Count/Left` 是成交时的序号、输入、消费量和剩余量，`Effect/Applied/Items` 是当时的效果名、效果数值及实际入包清单。`Items` 按物品号升序，每项是 `{ItemID, Count}`；开箱 `Applied=0`，经验书 `Items` 为空。余额及档案的**当前值**以自身同步视图为准。

```go
// 工程外层 Nest 调用方（不在该 Player 的另一个 handler 中）：
// import sender "<工程 module>/game/handler/syncsender"，pb "<工程 module>/protocol/pb"
r, err := sender.NewItemUseSender(engine).Sync_ItemUse(ctx, playerEntityID,
    &pb.ItemUseRequest{Sequence: seq, ItemID: itemID, Count: count})
// 这里的 playerEntityID 是完整 entity ID；只有事务成功返回时才能确认 r。
```

已经在该 Player 的 `rollback=undo` 持久 Nest handler 内，用 `r, err := p.ItemUse().Use(seq, itemID, count)`（返回 `view.ItemUseResult, error`），不要再同步派一条新消息。**向外返回错误**，否则调用方吞错等同于自己选择提交不完整的事务。入口先检查当前事务和该 Player **实例**的 guard，不自行取锁；无事务、拿着另一实例的锁、同一次效果/监听内递归使用都拒绝。服务端若代替客户端下单，同样使用这一个玩家序号流，不能另起无关计数器。

## 序号、重放与失败

- 每个 Player、所有使用道具共用一个 `int64` 高水位。新请求必须是 **本人同步字段 `Itemuse.Sequence + 1`**（初值 0，第一笔发 1）。服务端读取 `p.Dao().GetItemuse().GetSequence()`。已有系统的旁观/其他玩家视图不要白名单此字段。完整 `ItemuseReceipt` 是 `nosync`，不广播给客户端。
- 一次只发起一笔新操作。超时、断线或结果不明，先重发**原序号、原物品、原数量**。等于最后成功序号时只读回执，不查配置、不查库存、不抽取、不触发监听、不再发货；相同序号换参数报 `itemuse_conflict`。小于高水位、跳号、非正数或序号耗尽时报 `itemuse_sequence`；窗口外的请求**绝不当新单执行**。多端发生冲突时重新读取本人状态，不要盲目给旧操作换新号。
- 业务明确拒绝/持久提交器明确拒绝：库存、效果、回执与高水位一起回滚。**失败尝试没有承诺任何奖励清单**，可以在修正配置/腾出空间后用原序号重试，可能得到不同的随机结果。合法随机组全未命中是正常成功：仍消耗箱子、记录空清单并前移序号，不能借空奖免费重抽。需要在空间不足时也锁定一次预览结果的玩法应先创建持久订单；本模块不做开奖预览或跨事务待领。
- 非法物品号/数量或超过行批量：`itemuse_request`。缺行、缺桥、奖励组错误或不可用：`itemuse_rule`。超效果预算：`itemuse_limit`。非法事务/重入：`itemuse_guard`。`bag_short`、`bag_stack_full`、`bag_kinds_full`、`bag_item_invalid`、Profile 和监听器拒绝向外传播；控制器按工程 `errcode` 约定返回 `Code/Reason`，未编码的内部错误不把原因透给客户端。使用 `errors.Is` 或生成业务码，不硬编码安装分配的数值。
- 普通请求采用框架默认 `async` 持久级别，与 Bag 一致：回复成功表示提交器已接受，不等于机器突然断电也已 fsync。遇到传输中断或提交结论未知，不能宣称一定回滚；重连后仍用原序号和参数对账。

## 扩展可信效果

在会被工程链接的服务端包（最简单是在 `game/components/itemuse/` 增加一个文件）一次登记。下例是**工程接入示例，不是随基础模块自动启用的效果**：

```go
package itemuse

import "<工程 module>/game/view"

func init() {
    RegisterEffect("craft.token", func(_ view.PlayerEntity, outputID, count int64, out *Output) (int64, error) {
        // CSV: 8200,craft.token,8300,8；每消耗一件 8200，提议入包一件 8300。
        // 8300 要列在 item.csv，不能被设计成能反向兑换自己的凭证。
        return 0, out.Grant(outputID, count)
    })
}
```

`RegisterEffect(name, Effect)` 不允许重复注册或覆盖。`arg` 与 `count` 已为正；扩展负责自身参数含义、乘法上界、准入和经济闭环。所有效果都在同一 Player 上同步执行：用该玩家的 DAO/组件修改可回滚状态，物品通过 `out.Grant` 提议，业务错误必须返回。禁止保留传入指针、直接外发通知、操作其他玩家、另开事务、吞错、反向复兑或用监听器补发同一笔。自定义 Go 实现是**受信代码，不是沙箱**；有外部副作用的业务需单独设计事务型投递。

| 约束 | 行为 |
|---|---|
| 批量 | 每行 `max_batch` 取 `1..32`，请求最多 32 件；开箱即使误配更大也硬限 **4 箱**。一次不会按物品件数展开结果。 |
| 开箱代价 | 最多调用 4 次 `reward.Roll`；每次按 reward 自身规则最多 256 条、64 次加权抽取。合并后最多 **32 种，整单合计 999,999 件**。先用剩余额度比较再累加。禁止直接返还本次输入，也不允许开箱组产出另一个当前有 `reward.group` 用法的箱子，避免循环开箱。 |
| 经验/效果数值 | 每次 `Applied` 在 `0..1,000,000`。经验桥先按总额上限除以批量再乘；真实 `Profile.AddExp` 继续检查已有经验和等级溢出，并执行真实升级监听。 |
| 存储 | 一条永不倒退的持久高水位与一份有界私有回执。没有每日次数、限时配额、概率保底、跨服交易、永久回执历史或已过期业务的自动重试。 |

## 如何验证

`roost module verify` 运行当前已安装模块与联动的清单场景。只重跑道具消费并查竞态：`go test -race -count=1 ./game/handler -run '^(TestItemUse|TestWithItemuse)'`。基础 8 个场景，reward 桥 4 个，profile 桥 3 个。测试使用临时配置、真实 Player/Bag/Sender、进程内 Nest 与替身提交器，覆盖存档重建、热更后重放、参数冲突、窗口和批量边界、库存不足、缺桥、并发、真实奖励入包、真实升级监听与失败后重试。它们不需要外部数据库；不代表已通过真实 WAL、网络客户端或多进程恢复测试。
