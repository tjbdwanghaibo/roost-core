# U-0225：saga 步骤拒绝 / 重试用尽后，补偿在 Mongo 存储上落不下去

- 仓库：roost-core `saga`
- 缺陷类：C2（状态机 / 版本）
- 来源：game-demo 第十批实施（送礼 saga）实跑发现，无 RR 编号
- 相关：T-119；测试 `saga/compensation_version_promises_test.go`

## 问题

deliver 步骤对"收件人从未进过游戏"返回 `Completion{Success:false, Retryable:false, Error:…}`，
协调器的 completion 消费者对每一次投递都记 `saga: consumer processing failed … err="saga: invalid record"`，
记录停在 `waiting`，`last_error` 变成 `step result timeout`、`attempt` 递增；步骤按超时被反复重发，
Mongo inbox 每次回放同一个拒绝，协调器每次都拒收。saga 永远进不了补偿。

## 根因

`saga/engine.go`：

- `applyCompletion` 先 `after.Version++`，对不可重试的失败再调 `beginCompensation(after, …)`；
- `beginCompensation` 里 `after.Version = max(after.Version, record.Version+1)`——传进来的 `record` 已经 +1，于是再 +1；
- 同形状：`applyCompletion` 的可重试分支调 `retryOrCompensate(after, …)`，后者也做一次 `max(…, record.Version+1)`；
  重试用尽时再进 `beginCompensation`，第三次。
- `MongoStore.Apply` 第一行：`request.After.Version != request.ExpectedVersion+1` → `ErrInvalidRecord`。

于是 Mongo 存储上，**任何**由步骤结果触发的补偿（拒绝、重试用尽、成功后超 deadline）都被拒绝；
只有显式 `Compensate` API 与 claim 循环的 deadline 路径传的是未加版的记录，一直是对的。

为什么单测没看见：`engine_test.go` 的 `memoryStore.Apply` 只比对 `current.Version != ExpectedVersion`（CAS 当前版本），
不检查 `After.Version` 的步长；`TestEngineCompensatesInReverseOrder` 一直绿。mongotest 替身不支持
`$in []saga.Status`，claim 循环跑不起来，所以此前也没有 Mongo 存储版的引擎测试。

## 方案

- 采用：把"加一次版本"与"填补偿状态"拆开——`beginCompensation(record)` = `record.Version+1` + `compensationState(after)`，
  `retryOrCompensate(record)` = `record.Version+1` + `retryState(after)`；已加过版本的调用方（`applyCompletion`）直接调
  `compensationState` / `retryState`。五个调用点各自的语义不变。
- 没采用：让 `MongoStore.Apply` 放宽为 `After.Version > ExpectedVersion`。存储的严格步长是它的不变量（每次 Apply 恰一步，
  版本可作为变更计数），放宽是把错误藏起来。
- 没采用：给 `memoryStore` 加步长校验就算完。加了（见下）但那只是让单测变红，不是修复。

## 改动

- `saga/engine.go`：`beginCompensation` / `compensationState`、`retryOrCompensate` / `retryState`，`applyCompletion` 改调后两者。
- `saga/compensation_version_promises_test.go`：三条承诺。

## 证明

修前：

```
--- FAIL: TestStepRefusalMovesSagaIntoCompensationOnMongoStore
    Complete rejected a valid step refusal: saga: invalid record
--- FAIL: TestRetryOrCompensateAdvancesVersionByOne
    exhausted: version 7 → 9, want 8 (status compensating)
--- FAIL: TestApplyCompletionAdvancesVersionByOne
    refusal: version 7 → 9, want 8
    retryable-final: version 7 → 9, want 8
```

修后：`go test ./saga -race -count=1` 全绿。实跑（game-demo 送礼 saga，go.work 指向 core 源码）：6 个机器人全过，
saga 记录 6 completed + 6 compensated（收件人 1 从未进游戏 → deliver 拒绝 → debit 补偿退回道具）。

## 未做 / 边界

- `memoryStore` 的 Apply 仍不校验步长；引擎的 Mongo 版测试只覆盖了"存储里已有 waiting 记录 → Complete"这一段，
  claim 循环在 mongotest 上跑不了（`$in` 不支持），deadline 路径（`Run` 里 `beginCompensation(record)`）靠代码审读。
- 与本修复无关但实跑时一起看到的：原生 Nest 步骤（`SubscribeDataEngineStep` + `EmitCompletion`）的完成效果落在
  `ROOST_EFFECTS` 流的 `roost.effect.saga.result.<id>`，协调器只订阅 `ROOST_SAGA` 的 `roost.saga.result.>`——没有人消费。
  已登记 W-2026-09-17-04，交 review 判定。
