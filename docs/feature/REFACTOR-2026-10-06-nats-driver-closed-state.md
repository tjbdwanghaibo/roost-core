# 重构：nats/driver 自己持有唯一的“已关闭”状态

日期 2026-10-06，分支 `natsstate`，基线 main `90c92caf`。重构登记，不占 RR。未发版。

## 1. 维护者决定

维护者 2026-10-06 方向调整，按推荐 A：**nats/driver 自己维护唯一的“已关闭”状态，不再依赖 nats.go 的连接状态**。Close 一开始就置位；所有公开方法入口先查它，已关闭直接返回 `fnats.ErrClosed`；`Connected()` 等状态查询在已关闭时直接返回 false；nats.go 的状态只在未关闭时作辅助信息；删掉为翻译 nats.go 状态加的分支。与 Close 并发的调用要么在 Close 之前完成，要么返回 `fnats.ErrClosed`。

## 2. 为什么：同一处修了三次

| 编号 | 提交 | 症状 | 当时的修法 |
| --- | --- | --- | --- |
| [RR-20261004-08](../bugfix/RR-20261004-08.md) | `e0591f79` | 排空超预算硬关后，NatsMod 保留 Assembly，重试只能拿到 nats.go 的 `ErrConnectionClosed` | 硬关后返回终态 `ErrClosedUndrained` |
| [RR-20261006-10](../bugfix/RR-20261006-10.md) | `d05a04a1` | 重复 Close 每次都去排空已关闭的连接、每次报错；`Publish` 关闭后的错误 `errors.Is` 不到 `fnats.ErrClosed` | Assembly 加 `closed bool`；`Publish` 按 nats.go 的错误映射 |
| [RR-20261006-24](../bugfix/RR-20261006-24.md) | `0db819b0` | Subscribe / QueueSubscribe / JetStream 三方法 / CallAsync 关闭后原样返回 nats.go 的错误 | 新增 `closedError` + `connectionClosedError`，逐个调用点包一层 |
| [RR-20261006-26](../bugfix/RR-20261006-26.md) | `0db819b0` | 硬关打断排空后 nats.go 把状态翻回 `DRAINING_PUBS`，`Connected()` 约 5s 内为 true | `Connected()` 另看驱动的 `closing` 标记 |

根因都一样：驱动把 nats.go 的状态和错误直接透传，每发现一个出口就补一次翻译。每次修复都在加分支（新的映射函数、新的标记、新的特判），而不是减少；同一不变量（“Close 之后都是已关闭”）被打破了第三次。这正是 roost-coding“反复出问题要上报方向判断”的信号，维护者据此改方向。

本次在旧实现上跑新守卫，又找出同一类的三个漏口（见 §5 红绿）：`Client.Drain`、`Client.DrainWithContext`、订阅句柄的 `Unsubscribe` 关闭后仍原样返回 nats.go 的 `ErrConnectionClosed`；与 Close 并发的 `Unsubscribe` 出现第三种结果。

## 3. 新判据

`Client.state.closed`（`atomic.Bool`）是唯一的已关闭判据，Client 持有，JetStream、RPC、订阅句柄经自己的 `client` 引用读它：

- **置位**：`Client.Close` 一开始就置位，再关 nats.go 连接；`DrainWithContext` 排空正常结束时置位（`CompareAndSwap`，排空期间被别人 Close 的返回 `fnats.ErrClosed`）。
- **入口检查** `Client.admit()`：已关闭（含 nil Client、没有连接）返回 `fnats.ErrClosed`，不碰 nats.go。Publish / Request / Subscribe / QueueSubscribe / Drain / DrainWithContext、JetStream 三方法、RPC 的 Call / CallWithTimeout / CallAsync / Reply、订阅的 Unsubscribe 都先经它。
- **过了入口之后的失败** `Client.wrapError`：先看驱动状态，这期间 Close 了一律 `fnats.ErrClosed`，不管 nats.go 返回什么（包括 ctx 也恰好结束时 nats.go 先返回的 `ctx.Err()`）。未关闭时才按 nats.go 的错误辅助分类：超时、无响应者、连接已关闭 / 正在排空（排空进行中，或 nats.go 放弃重连后自己关闭）。
- **状态查询**：`Client.Connected()` / `Assembly.Connected()` / 订阅的 `IsValid()` 已关闭时直接返回 false，未关闭才看 nats.go。
- **关闭类方法**：`Client.Close`、`Assembly.Close`、`RPCClient.Stop` / `StopWithContext` 幂等返回 nil。`Assembly.Close` 不再有自己的 `closed` 字段：停 RPC（幂等）之后看 Client 的判据，已关闭返回 nil，否则排空；`ErrClosedUndrained` 只报给关掉连接的那一次。
- **RPC**：RPC 客户端的“已停止”仍是它自己的 `stopped`（回调池的生命周期，停止流程不变）。停止后的结果统一为 `errRPCStopped`（同时 `errors.Is` 到 `fnats.ErrCancelled` 与 `fnats.ErrClosed`），停止时仍在途的调用也用它（原来只 `ErrCancelled`）。只关连接、没停 RPC 时，在途 CallAsync 到 5s 超时按 `fnats.ErrClosed` 终结（`expirePending`）。

并发：入口检查与 `wrapError` 都读同一个原子标记，Close 先置位再关连接，所以一个调用要么在 nats.go 里先于连接关闭完成（在 Close 之前完成），要么失败时看到已置位（`fnats.ErrClosed`），要么入口就被挡下。

## 4. 删掉了什么

| 删除 | 原因 |
| --- | --- |
| `closedError` 与 `connectionClosedError`（RR-24） | Close 之后由入口检查直接返回；并发窗口里由 `wrapError` 按驱动状态判定。nats.go 的关闭 / 排空错误只在未关闭时由 `wrapError` 的辅助分类映射 |
| `natsLifecycleState.closing` 与 `Connected()` 的双重条件（RR-26） | 换成唯一的 `closed`；`Connected` 只写 `!c.closed() && conn.IsConnected()` |
| `Assembly.closed`（RR-10） | 与连接状态重复；改看 Client 的判据 |
| `DrainWithContext` 的 nil 特判（nil Client 返回 nil） | 并入入口检查（返回 `fnats.ErrClosed`） |
| `Publish` 里按 nats.go 错误判断“重试不会成功”的分支 | 改为按 `wrapError` 的结果（`fnats.ErrClosed`）判断 |

代码量：五个源文件非注释非空行 861 → 894。多出来的是每个公开方法统一的入口检查（11 处，每处 3 行）、`JetStreamClient.admit`、`RPCClient.expirePending` 与测试缝 `testAfterAdmit`；删掉的是逐出口的翻译与重复状态。判据从“每个出口各自翻译 nats.go 的错误 + 两个标记”变成“一个标记、入口一处检查、失败一处判定”。

## 5. 验证

**守卫**（`nats/driver/closed_state_guard_promises_test.go`）：

- `TestEveryExportedDriverMethodHasAClosedStateCheck`：解析本包非测试源码，列出每个导出方法（含未导出类型上的导出方法），要求 `closedStateChecks` 里都有一行；免检的（JetStream 消费句柄的 Stop / Drain / Closed、worker 池协议 `rpcTask.OnRelease`）必须写理由。以后新加导出方法不登记就红。
- `TestEveryExportedDriverMethodReportsErrClosedAfterClose`：分别在 `Assembly.Close` 与只 `Client.Close` 之后逐个调用，断言 `fnats.ErrClosed` / nil / false，每个 500ms 内返回。
- `TestACallAdmittedBeforeCloseFinishingAfterItReportsErrClosed`（屏障）：经测试缝在“已过入口、还没碰 nats.go”时插入 Close，结果必须 `fnats.ErrClosed`；“ctx ended”一组再让调用的 ctx 结束（nats.go 此时先返回 `ctx.Err()`）。
- `TestInFlightCallAsyncExpiresAsErrClosedAfterClientClose`、`TestCallsRacingCloseEitherCompleteOrReportErrClosed`（20 轮、6 协程与 Close 竞争，`-race`）。
- 真实 NATS（`closed_state_guard_real_promises_test.go`，`-tags integration`）：排空被预算截断、硬关后，等到 nats.go 翻回 `DRAINING_PUBS`（`IsConnected` 为 true），在这段窗口里把整张表跑完（非关闭类方法约 12ms 答完，答完时 nats.go 仍报 IsConnected = true），关闭类方法放最后。故障矩阵 `dataengine-env.sh` 加 `./nats/driver`（根包门禁要求）。

**旧实现上的红**（守卫只用公开 API，把五个源文件换回基线 `0db819b0` 跑）：

```
--- FAIL: TestEveryExportedDriverMethodReportsErrClosedAfterClose/Assembly.Close
    Client.Drain after Assembly.Close = nats: connection closed, want an error that errors.Is fnats.ErrClosed
    Client.DrainWithContext after Assembly.Close = nats: connection closed, want ...
    subscription.Unsubscribe after Assembly.Close = nats: connection closed, want ...
--- FAIL: TestEveryExportedDriverMethodReportsErrClosedAfterClose/Client.Close
    Assembly.Close after Client.Close = nats: connection closed before drain finished: nats: connection closed, want nil / false
    （Drain / DrainWithContext / Unsubscribe 同上）
--- FAIL: TestCallsRacingCloseEitherCompleteOrReportErrClosed
    round 1: third outcome while racing Close: nats: connection closed
```

**变异**（临时，不提交）：

| 变异 | 结果 |
| --- | --- |
| M1 `Unsubscribe` 跳过入口检查与 `wrapError` | 红：守卫两种关闭方式、屏障“returned <nil> without passing the admission check”、并发“third outcome” |
| M2 新加导出方法 `Client.Flush` 不登记 | 红：`exported method Client.Flush has no entry in closedStateChecks` |
| M3 `JetStreamClient.EnsureStream` 跳过入口检查与 `wrapError` | 红：守卫两种关闭方式、屏障 |
| M4 `wrapError` / `requestWithContext` 不看驱动状态（只按 nats.go 错误分类） | 红：屏障“ctx ended”组 JetStream 三方法 `context canceled`、RPC.Call `nats: request cancelled` |
| M5 `expirePending` 不看驱动状态 | 红：`in-flight CallAsync expiring after Client.Close = nats: request timeout` |
| M6 `Connected()` 不看驱动状态（RR-26 回退） | 真实 NATS 红：守卫 `Assembly.Connected` / `Client.Connected` = true；kit `TestRealNatsModUndrainedCloseIsReportedOnce` 红 |

M4 起初没红：不可达地址上 nats.go 关闭后的错误本身就能被辅助分类映射，于是补了“ctx ended”组，证明驱动状态优先这一条不是摆设。

**既有用例原样通过**（不改断言）：`nats/driver` 全部单元（含 `close_contract_promises_test.go`、`client_promises_test.go` 的翻译表、RPC 停止 / 预算 / 重入用例），`kit/nats` 全部单元与 integration（含 `close_contract_real_promises_test.go` 的 11 个调用、`TestRealNatsModUndrainedCloseIsReportedOnce`）。

验证命令（`GOWORK=off`）：`gofmt -l` 为空；`go vet ./nats/... ./kit/nats/`（含 `-tags integration`）；`go test -race -count=3 ./nats/... ./kit/nats/`；`scripts/mirror-local.sh up`（私有目录、端口偏移 23500，用完 clean）后 `go test -tags integration -race -count=3 -p 1 ./nats/... ./kit/nats/`；`go test -count=1 ./bus/... ./kit/... ./saga/...`；根包 `go test -count=1 .`；`go build ./... && go vet ./...`。

## 6. 兼容

契约（RR-20261006-10 四条）不变，按 `errors.Is(err, fnats.ErrClosed)` / `errors.Is(err, fnats.ErrCancelled)` 判断的调用方不受影响。可观察的差别都在契约之内：

- Close 之后的错误是 `fnats.ErrClosed` 本身（或带调用前缀的包装），不再同时 `errors.Is` 到 nats.go 的 `ErrConnectionClosed`（RR-24 曾保留）；文本不变（两者都是 `nats: connection closed`）。仓内没有按 nats.go 原错误判断的调用方（`rg ErrConnectionClosed` 只剩注释）。
- `Drain` / `DrainWithContext` / 订阅 `Unsubscribe` 关闭后现在 `errors.Is(fnats.ErrClosed)`（原来只是 nats.go 的错误，文本相同）。
- RPC 停止时仍在途的 CallAsync 回 `errRPCStopped`（文本 `nats: request cancelled: nats: connection closed`，原来 `nats: request cancelled`），仍 `errors.Is(fnats.ErrCancelled)`。
- 直接调过 `Client.Close` 再调 `Assembly.Close` 返回 nil（原来 `ErrClosedUndrained`）；按“重复 Close 返回 nil”处理。kit `NatsMod` 只经 `Assembly.Close` 关闭，不受影响；nats.go 自己关闭的连接（放弃重连）不置驱动标记，`Assembly.Close` 照旧报 `ErrClosedUndrained`（`TestNatsModStopReleasesAssemblyWhoseConnectionIsAlreadyClosed`）。
- 只关连接、没停 RPC 时，在途 CallAsync 的 5s 超时结果由 `ErrTimeout` 改为 `fnats.ErrClosed`。

没有改 API、配置、持久格式或生成物。`kit/nats` 不需要改。

## 7. 后续边界

- nats.go 放弃重连后自己关闭的连接，驱动不置位（不注册 ClosedHandler 置位），由 `wrapError` 的辅助分类映射为 `fnats.ErrClosed`；`Assembly.Close` 对它照旧排空失败、报一次 `ErrClosedUndrained`。
- JetStream 消费句柄（`jetStreamSubscription`）的生命周期是 nats.go 的 ConsumeContext，不在本判据内（守卫里写明免检理由）。
- 受影响的 v1.23.0 发版文档条目（本次不改 `docs/release/v1.23.0/*`）：**DRV-5**（驱动与 Mod 的 Close 统一口径）——`impl-saga-drv-dao-rem.md` DRV-5 的源码表两行已过时（`assembly.go` 的 `closed` 字段已删；`Client.Publish` 关闭后不再同时 `errors.Is` 到 gonats 原错误），`guide` / `_summary` 的 DRV-5 行为描述不变。RR-20261006-24 / -26 在 v1.23.0 发版文档里还没有条目，补写时按本记录的判据写，不按 `closedError` / `closing` 写。
