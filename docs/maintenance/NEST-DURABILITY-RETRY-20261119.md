# Nest 持久写与幂等重试：提前确认的边界

**决定（2026-11-19）：记录行为和业务选型，不改 Nest，不回退到全量 `strict`。** 此前审查中的「P1」是疑点编号，不是已证实的购买丢奖事故。验证到了重复请求提前回成功，没有验证到掉电后的永久丢单。复核基线：`5cbde701`。

## 已验证的时序

前提：同一实体的写 handler 声明 `durability=pipelined completion=async`；首次请求 A 修改业务数据和幂等标记，第二次独立调用 B 命中标记，不再写数据。B 是调用方对同一业务操作发起的新请求，不是 Nest 因刷盘慢而重跑 A。

| 顺序 | 请求 A | 请求 B |
| --- | --- | --- |
| 1 | 持锁写入数据和幂等标记，WAL 接受记录并返回未完成的票据。 | — |
| 2 | 放锁；在正常交接路径，派发 worker 释放，完成池等待票据。尚未回成功。 | 取得同一实体锁，读到新标记，判定「已处理」。不写第二笔。 |
| 3 | 首笔票据仍未完成。 | 空记录没有本次票据，当前路径未等待前一笔完成，返回成功。 |
| 4 | WAL `fsync` 成功后，A 才能按成功路径收尾。 | B 如果已经触发外部 ACK，该动作不会因 A 的后续结果自动撤销。 |

验证方法：用真实 Nest 调度和可控的 `CommitTicket`，不放行 A 的票据，再向同一实体提交 B；观察到 B 返回 `1`、A 仍未回复。这是完成顺序的验证，不是掉电实验。实现入口见 [空记录不入队](../../framework/nest/rollback.go)（`pipelinedEnqueue`）及[等待票据与前序完成](../../framework/nest/execution.go)（`commitPipelined`）。

## 什么情况下可以恢复？

判断标准不是「玩家还会不会再登录」，而是**登录时能否从独立、持久的权威来源再次找到同一笔业务操作和所需的发放内容**：

- 能找到：即使掉电使最近更新丢失，仍可重新发起；幂等标记与业务修改须在同一事务中。接受提前确认，是接受可恢复的回退窗口。
- 找不到：如果成功后已不可逆地删除唯一待发记录或确认邮件已领，本地写入又未持久，则重新登录本身无法补发。这是条件性风险，不是已复现的线上结果。

当前 [购买补领](../../modules/purchase/files/game/controllers/purchase.go.tmpl) 从 `Pending` 读记录，`Sync_PurchaseGrant` 成功后才 `Ack`；[邮件领取](../../modules/mailbox/files/game/controllers/mailbox.go.tmpl) 在发奖成功后才 `CommitClaim`。相应的 [购买发货](../../modules/purchase/files/game/handler/purchase_grant.go.tmpl) 和 [邮件发奖](../../modules/mailbox/files/game/handler/mailbox_claim_reward.go.tmpl) 都是**普通 `pipelined`**，未声明 `completion=async`；上述复现不能直接套用在这两个模板上。

## 业务 handler 选型

1. **普通持久写用 `durability=async`。** 有独立权威来源能重放的业务允许提前确认。回复说明本次记录写进了 WAL 文件，不等于已完成 `fsync`。
2. **仅奖励或购买相关、成功后会删除唯一补偿凭据的写，用 `durability=pipelined`，不启用 `completion=async`。** 保持先等本地 WAL 持久，再 ACK、删待发或确认领取的调用顺序。仅有订单号或幂等键，不等于有可重放的权威来源。
3. 此决定针对**持久写**。纯查询仍可走 `memory`；没有要求修改 Saga 等框架内部协议，也不把后续查询到的内存值当作持久确认。

### 成功路径时间线（本地、非空写）

业务函数体先返回，随后才有事务收尾。WAL 入队、文件写入、`fsync`、Mongo 投影是不同的时点。

| 声明 | 何时可以放实体锁 | 派发 worker 何时释放 | 成功回复的保证 |
| --- | --- | --- | --- |
| `strict` | 本笔 `fsync` 后 | 持久等待及正常派发收尾后 | 本笔 WAL 已持久 |
| `async` | WAL 文件写入后，不必等 `fsync` | 写入及正常派发收尾后 | 本笔写入 WAL，不承诺整机掉电后仍在 |
| 普通 `pipelined` | 锁内准入成功后，不等磁盘 I/O | 原 worker 等票据及收尾后 | 本笔 WAL 已持久 |
| `pipelined completion=async` | 同普通 `pipelined` | 正常交接后即可释放；完成队列满时可退回原 worker 等待 | 有本笔记录时仍等本笔持久；无写入后继见上文 |

这些保证都不是「Mongo 已能查到」。如果将来要对删除唯一补偿凭据的业务开启异步完成，先重新验收两条：A 未持久，B 不能成功；A 持久结果不确定，B 也不能成功。不能靠再写一笔奖励或取消幂等检查来满足这个要求。
