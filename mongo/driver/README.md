# mongo/driver 驱动行为契约

`mongo/driver` 在 mongo-driver v2.6.0 之上实现 `fmongo.IMongo`（`mongo/`），kit 的 MongoMod（`kit/mongo/mongo_mod.go`）用的就是它。框架的约定是：**结果未知时交给调用方处理**。本页说明驱动在哪里自动重试、哪些错误表示“确定没提交”或“结果未知”、在哪里替换了 ctx、默认超时是多少。新增调用点时先对照本页（A2 决定，2026-10-05；Redis 侧见 [redis/driver/README.md](../../redis/driver/README.md)，方案见 [A2 方案](../../docs/feature/A2-DRIVER-REPLAY-CONTRACT-2026-10-05.md)）。

升级驱动时，要对照驱动 `mongo.Session.WithTransaction` 的规则重新核对 `session.go` 里自己实现的重试循环（RR-20261005-NC-101）。

## 1. 哪些操作会被重试

| 操作 | 行为 |
| --- | --- |
| 事务外的单条读 / 写 | 驱动的 retryable reads / retryable writes（`SetRetryReads(true)`、`SetRetryWrites(true)`）：遇到可重试错误（网络错误、主节点切换等）时重试一次。可重试写带 txnNumber，由服务端去重，所以重试后同一笔写至多生效一次。多文档写（`UpdateMany` / `DeleteMany`）不在可重试写的范围内，出错时结果未知 |
| 事务内的单条操作 | 驱动不单独重试 |
| `ISession.WithTransaction` | 用自己实现的循环代替驱动的便捷 API，重试规则相同：回调返回 TransientTransactionError 时整个回调重跑；提交返回 UnknownTransactionCommitResult（MaxTimeMSExpired 除外）时只重试提交；提交返回 TransientTransactionError 时整个回调重跑。重跑之间退避 5ms 起、每次 ×1.5、封顶 500ms，带全抖动。整个过程（包括提交）受 `TransactionTimeout` 限制 |

## 2. 事务错误分类

| 类别 | 错误 | 调用方怎么做 |
| --- | --- | --- |
| **结果未知**（可能已提交） | 提交命令发出之后才失败：网络中断、截止时间落在提交中途、MaxTimeMS 到期。错误包着 **`fmongo.ErrCommitResultUnknown`**，原错误链和标签都保留；截止导致的仍满足 `errors.Is(err, context.DeadlineExceeded)` | 不能当成“没提交”去重做。按持久身份或回执裁决：回读回执、按 ID 幂等重放 |
| **确定没提交** | 回调返回的错误（会先尽力 abort）；截止在提交之前到期（此时不发提交，返回 ctx 错误）；回调或提交返回 TransientTransactionError 之后，窗口在重跑前的退避里关闭（返回 `errors.Join(ctx.Err(), 最后一次错误)`）；`StartTransaction` 失败 | 可以当成“没提交”处理。最后一次错误的链会保留下来，例如 `ErrDuplicateKey`，可以按它分流 |

不要再靠 `UnknownTransactionCommitResult` 标签或 `DeadlineExceeded` 判断是否已提交：标签可能被驱动内部重试的错误覆盖，而 `DeadlineExceeded` 也会出现在“提交前就到期”这种确定没提交的情况里（NC-101 复审观察）。只有 `ErrCommitResultUnknown` 是可靠的判据。现有正式调用方（DataEngine 投影与加载、Remote committer、saga、effect inbox）对 `WithTransaction` 返回的任何错误都按持久回执裁决，加上这个哨兵不改变它们的行为。

## 3. 驱动在哪里替换了 ctx

| 位置 | 用的 ctx |
| --- | --- |
| 回调与提交 | 调用方的 ctx 外面套一层 `TransactionTimeout` 截止。驱动便捷 API 用 background ctx 提交，这里不用 |
| 回调失败后的 abort | `context.WithoutCancel(ctx)`，另加 5s 上限（`transactionAbortTimeout`）。属于尽力而为：没送达的事务由服务端在 `transactionLifetimeLimitSeconds`（缺省 60s）后自行中止 |
| `EndSession` | 同上，`WithoutCancel` 加 5s。没有进行中的事务时不发命令。提交因截止失败后，驱动会在这里补发 abort |
| 其他读写 | 调用方的 ctx。客户端没有设置 CSOT（`SetTimeout`），调用方不给截止时间就没有操作级超时 |

## 4. 默认值

| 项 | 默认值 | 来源 / 配置键 |
| --- | --- | --- |
| ConnectTimeout，也作为 ServerSelectionTimeout | 10s | `fmongo.DefaultConfig`，`mongo.connect_timeout` |
| TransactionTimeout | 30s；`<= 0` 时取驱动的 120s | `fmongo.DefaultConfig`，`mongo.transaction_timeout` |
| abort / EndSession 上限 | 5s | `session.go` |
| 写关注 / 读关注 | majority 加 journal / majority；事务内用 snapshot | `client.go` |
| 连接池 | Max 100 / Min 10 / 空闲 5m | `fmongo.DefaultConfig` |

## 5. 新增调用点核对清单

1. `WithTransaction` 返回 `ErrCommitResultUnknown` 时，按持久回执裁决，不要重做业务。
2. 事务外的多文档写出错后结果未知，与 Redis 写命令一样处理。
3. 需要在调用方 ctx 之外收尾的操作（abort、释放），参照 §3：用 `WithoutCancel` 并加上限，不要直接用 `context.Background()` 无限等待。
