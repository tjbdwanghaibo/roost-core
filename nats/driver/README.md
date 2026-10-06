# nats/driver 驱动行为契约

nats-io/nats.go v1.53.1 之上的驱动：核心连接 `Client`、同连接上的 `JetStreamClient`、请求 / 应答 `RPCClient`，由 `Assemble` 装成 `Assembly`（kit `NatsMod` 用它）。本文目前只写 Close 契约；节号与 [redis/driver](../../redis/driver/README.md)、[mongo/driver](../../mongo/driver/README.md) 的契约表对齐为 §5，§1～§4（重试、错误分类、ctx、默认值）尚未整理成文，以源码注释为准。

## 5. Close：重复调用与出错后再调用

口径与其他驱动相同（[RR-20261006-10](../../docs/bug/RR-20261006-10.md)）：**重复 Close 幂等返回 nil，第一次的错误只报一次；并发 Close 的后到者等第一个做完；Close 之后的其他调用返回已关闭错误**。nats 的已关闭错误是 `fnats.ErrClosed`（`github.com/tjbdwanghaibo/roost-core/nats`），唯一例外是 `Assembly.Close` 排空失败时的终态 `ErrClosedUndrained`，只报一次。

**判据（[REFACTOR-2026-10-06-nats-driver-closed-state](../../docs/feature/REFACTOR-2026-10-06-nats-driver-closed-state.md)）**：驱动自己持有唯一的“已关闭”状态（`Client` 上的原子标记），`Close` 一开始就置位、排空正常结束时置位。每个公开方法入口先查它，已关闭直接返回 `fnats.ErrClosed`、不碰 nats.go；调用过了入口、在 nats.go 里失败时同样先看它。所以与 Close 并发的调用**要么在 Close 之前完成，要么返回 `fnats.ErrClosed`**。nats.go 的连接状态只在未关闭时作辅助（排空进行中、nats.go 放弃重连后自己关闭）——硬关打断排空后 nats.go 会把状态翻回 `DRAINING_PUBS`、约 5s 内 `IsConnected` 为 true（[RR-20261006-26](../../docs/bug/RR-20261006-26.md)），驱动不看它。

| 对象 | 第二次 Close | 第一次 Close 出错后再调用 | Close 之后的操作 | 并发 Close |
| --- | --- | --- | --- | --- |
| `Assembly.Close(ctx)` | nil（RPC 停止幂等，连接已关闭就不再排空） | 等 RPC 回调超预算：返回 ctx 错误、保留所有权，再调继续等同一次排空；排空失败或超预算：硬关连接、返回 `ErrClosedUndrained`（包着排空错误，仍可 `errors.Is` 到 ctx 错误），之后 nil | `Connected()` 为 false；经它发布的 Client / JetStream / RPC 同下 | 串行（`operation.Serial`）：后到者在自己的 ctx 内等第一个做完，之后 nil；`ErrClosedUndrained` 只报给一个 |
| `Client.Close()` | 无返回值，幂等 | — | Publish / Request / Subscribe / QueueSubscribe / Drain / DrainWithContext 返回 `fnats.ErrClosed`；`Connected()` 为 false；之前拿到的订阅 `Unsubscribe` 返回 `fnats.ErrClosed`、`IsValid` 为 false | 原子置位，无等待 |
| `JetStreamClient` | — | — | EnsureStream / Publish / Subscribe 返回 `fnats.ErrClosed`（连接关闭后） | — |
| `RPCClient.Stop` / `StopWithContext` | nil（只停一次，后到者等同一次回调排空，受自己的 ctx 约束） | ctx 先结束返回 ctx 错误，再调继续等 | 停止后的 CallAsync、停止时在途的 CallAsync 回调都收到同时 `errors.Is` 到 `fnats.ErrCancelled` 与 `fnats.ErrClosed` 的错误；连接关闭后 Call / CallWithTimeout / Reply 返回 `fnats.ErrClosed`；只关连接、没停 RPC 时在途 CallAsync 到 5s 超时收到 `fnats.ErrClosed` | 同左 |
| kit `NatsMod.StopWithContext` | nil | `ErrClosedUndrained` 时报告并释放 Assembly（RR-20261004-08），之后 nil；Bus / RPC 回调超预算时保留，可以再调 | 经 Mod 发布的 IClient / IRpc / IJetStream / IBus 调用返回可 `errors.Is` 到 `fnats.ErrClosed` 的错误（真实 NATS 实测，`kit/nats/close_contract_real_promises_test.go`） | 串行：后到者等第一个做完 |

调用方据此：判断“已经关了”用 `errors.Is(err, fnats.ErrClosed)`，不要按 nats.go 的 `ErrConnectionClosed` 判断（Close 之后的错误不再包着它）。

**守卫**：`closed_state_guard_promises_test.go` 解析本包源码列出每个导出方法，要求表里登记它 Close 之后的结果；新加导出方法不登记就红。新增方法的入口先调 `admit()`，nats.go 的失败经 `wrapError` 返回。
