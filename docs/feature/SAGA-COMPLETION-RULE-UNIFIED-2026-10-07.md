# saga 完成判定收敛为一条统一规则（2026-10-07）

**来由**：维护者 2026-10-07 选 A（[FRAMEWORK-DOCS-FINDINGS 第 90 行](../review/FRAMEWORK-DOCS-FINDINGS-2026-10-07.md)）：“saga 完成判定收敛为统一规则，并补 S7 跨进程上限为共享配置”。
`saga/engine.go` `Complete` 的接收规则两天里补了五次（U-0280 → B1 → 方向 ③ → 方向 ④ → RR-20261006-42），由 2 条变成 5 条，每次都是对一种记录状态再加一个分支。
本方案把它们换成一条规则、一个判定函数。

**基线**：`origin/main` `a7285da6`，分支 `sagarule`。codebase-memory 共享 generation 停在 09-30（早于基线），以下行号按 worktree 当前源码。

## 1. 统一规则

> **成功是操作的结论，只要这个操作还没有带结果关闭，就接收。**
> 拒绝是本生这个操作的结论，可重试失败是一次尝试的结论，二者只在协调器正等着它们时接收。

completion 的代际 `inc` 从 `CommandID` 解析（`commandIDIncarnation`，不变），记录 `r`，操作键 `k = completion.IdempotencyKey`。
判定只有一个入口 `judgeCompletion(r, completion, inc)`，按序：

| # | 条件 | 判定 | 去向 |
| --- | --- | --- | --- |
| 0 | `inc > r.Incarnation` | 协调器不可能产生的代际 | `stale_incarnation`，计数、不改记录 |
| S | **成功**，协调器开过 `k`（本生：`r` 开着 `k`，即 `openOperation(r)=k`——在等或在重试退避；旧一生：`inc < r.Incarnation`，那一生派发过它）且 `r` 停在 `k` 上（`positionedAt`：在等、退避，或下一步就派发 `k`） | 接收为 `k` 的结果 | `applyCompletion` + `stepTransition(causeResult, 回执)`，带结果关闭 `k` |
| S' | **成功**，其余（`r` 已离开 `k`，或本生根本没开过 `k`） | 按 `k` 的关闭方式（tombstone）：未带结果关闭（`abandoned`）→ 仍接收，落点是“补偿这一步”；已带结果关闭 → 重复；从没开过 → 不认识 | `completeNotWaiting`：方向 ④ `compensateLateStep` / 补偿方向告警 / `Duplicates++` / `ErrNotWaiting` |
| R/F-old | 拒绝或可重试失败，`inc < r.Incarnation` | 上一生的结论，新一生的尝试照常执行 | `stale_incarnation` |
| R/F-gone | 拒绝或可重试失败，本生，`r` 不在等 `k`（`Status≠Waiting` 或 `OperationKey≠k`） | 协调器没在等 | `completeNotWaiting`：有回执 → 重复；否则 `ErrNotWaiting` |
| F-stale | 可重试失败，本生，`r` 在等 `k`，`CommandID ≠ r.CommandID` | 较早一次尝试的结论 | `stale_attempt` |
| R / F | 其余（拒绝来自本生任何一次尝试；可重试失败就是正在等的那次） | 接收 | `applyCompletion` |

“带结果关闭”与“放弃关闭”的区分来自协调器的 tombstone（`_saga_operations`，`closure = result | abandoned`，U-0280）；收件箱的
每操作状态文档（[SAGA-OPERATION-STATE-DOC](SAGA-OPERATION-STATE-DOC-2026-10-06.md)）保证同一操作至多一次生效、之后的尝试只回放，
所以“已带结果关闭”就是“这个成功已经计过”。

**S 为什么不用查 tombstone**：记录停在 `k` 上时 `k` 不可能已带结果关闭。带结果关闭只发生在接收成功时（`applyCompletion` 让记录
离开 `k`：正向 `Step++`，补偿 `nextCompensation`）或 `compensateLateStep`（关闭的是正向操作，记录此时在补偿方向 / 终态）。
离开之后回到同一个操作键只有两条路：`Resume` 与补偿方向的人工 `Compensate`，二者都开新一生；而 `Resume` 有已完成步骤时
走补偿（`nextCompensation`），不会回到已带结果关闭的正向步骤；补偿操作带结果关闭后 `CompletedSteps--`，`nextCompensation` 不再选它。
所以 S 只读记录，不在热路径上多读一次 tombstone。

## 2. 与已有方向的兼容

| 方向 | 要求 | 统一规则里怎样成立 |
| --- | --- | --- |
| 方向 ③（只接收正在等的那次尝试的可重试失败） | 较早尝试的可重试失败不能用后一次的尝试计数判用尽、放弃正在执行的尝试（O-S5-7） | F-stale：可重试失败是尝试的结论，只认 `r.CommandID`；成功与拒绝是操作的结论，不看尝试 |
| 方向 ④（放弃后迟到的正向成功补偿这一步） | 操作已放弃关闭，成功仍到达 → 补偿那一步、可重开终态 | S'：放弃关闭 ≠ 带结果关闭，成功仍“接收”，只是记录已不在 `k` 上，落点换成 `compensateLateStep`（补偿方向的只告警，按（操作，代际）去重） |
| B1（按代际，旧一生的拒绝 / 失败不接收） | 新一生的尝试照常执行，旧一生的拒绝不能把新一生打回补偿 | R/F-old；成功不看代际（S / S'），与 B1 的“旧一生成功在记录停在该操作上时接收”一致 |
| RR-20261006-42（退避中送达的成功） | 退避中送达的成功不能 Term | S：`positionedAt` 覆盖退避（`Pending` / `Compensating` 且 `Attempt>0`） |
| U-0280（操作为至多一次单位、tombstone、补偿方向迟到成功告警） | 已带结果关闭的成功是重复；放弃后迟到的补偿成功告警一次 | S' 的历史分支不变（`completeNotWaiting` 原样保留） |

## 3. 现有 5 条规则的对照

| 原规则（`Complete` 文档注释编号） | 来源 | 统一规则里的位置 |
| --- | --- | --- |
| 1. 旧一生（或比记录新）的拒绝 / 失败不接收，计 `stale_incarnation` | B1 | 0（比记录新，成功也算）+ R/F-old |
| 2. 旧一生的成功：记录停在这个操作上就接收 | B1 | S（成功不分代际） |
| 3. 同一生、在等这个操作：成功与拒绝从任何尝试接收，可重试失败只接收正在等的那次 | 方向 ③ | S（成功）+ R / F（拒绝、当前尝试的失败）+ F-stale |
| 4. 同一生、操作在重试退避：成功接收 | RR-20261006-42 | S（`positionedAt` 包含退避） |
| 5. 其余按回执与 tombstone：重复 / 放弃后迟到的成功（补偿或告警）/ `ErrNotWaiting` | U-0280、方向 ④ | S' + R/F-gone（`completeNotWaiting` 不变） |

推导下来 5 条都被覆盖，没有需要上报的例外，行为不变。推导中的一处细化：初稿把 S 写成“`r` 停在 `k` 上就接收”，跑原有用例时
`TestCompletionConsumersTermTheSameTerminalErrors/step_is_not_waiting_and_has_no_history` 红了——刚启动、还没派发（`Pending`、`Attempt=0`）
的记录收到本生一份从没派发过的尝试的成功，原规则 5 给 `ErrNotWaiting`（Term）。“还没有带结果关闭”的前提是“开过”：本生没开过
的操作，它的成功不是协调器派出的尝试产生的，不认识。S 因此要求“协调器开过 `k`”，与原规则 2～4 逐一对齐。

## 4. 删掉的分支

- `Complete` 里的四分支 `switch` 与 `accept` 变量（`saga/engine.go:450-466`）：“`inc≠r` 且失败或更新”、“`inc≠r` → `positionedAt`”、
  “`Waiting` 且同操作 → 接收，可重试且非当前尝试 → `stale_attempt`”、“default：成功且 `openOperation==k`（RR-42）”。
  换成 `switch judgeCompletion(...)` 的四个去向，规则只在 `judgeCompletion` 里。
- `Complete` 的 5 条编号注释换成统一规则一段。
- `Complete` 不再引用 `openOperation`（它只留在 `stepTransition` 决定关闭哪个操作与 `compensateLateStep`）。

守卫（`saga/completion_rule_guard_test.go`）：
1. 判定表：`judgeCompletion` 在（成功 / 拒绝 / 当前尝试失败 / 较早尝试失败）×（旧 / 同 / 新代际）×（在等 / 退避 / 待派发 / 已离开）的
   全部 48 种组合上等于第 1 节的表；
2. 结构：`Complete` 恰好调用一次 `judgeCompletion`，且不直接读 `Status`、`OperationKey`、`CommandID`、`Incarnation`、`Retryable`、`Success`，
   不调用 `positionedAt` / `openOperation`；`positionedAt` 只在 `judgeCompletion` 里调用。再加一条记录状态的分支就会被这个守卫报出。

## 5. S7 跨进程：`saga.max_payload_bytes` 成为发起方与协调器共用的配置键（RR-20261006-66）

**现状**（RR-20261006-43 之后）：`EmitStart` 的上限来自 `Engine.Register` 写进的按类型进程表；协调器在别的进程时只按 4 MiB 校验，
超限的意图随 Nest 事务提交，协调器拒绝后由启动消费者 Term + ERROR 兜底，业务事务已提交。

**改法**：
- `kit/mods.SagaPayloadConfig`：`saga.max_payload_bytes`（缺省 65536，`min 1`、`max 4194304`）一份声明，`kit/saga` 与 `kit/nest` 两个 Mod 的配置都嵌入它，
  A4 ① 的 `Merge` 要求同一键的声明完全相同，共用一个结构体就是这样。协调器（`kit/saga`）照旧把它作为 `Options.MaxPayloadBytes`；
  发起方（每个调 `EmitStart` 的进程都有 Nest，即 `kit/nest`）在 `Init` 里 `coresaga.SetStartDataLimit(值)`。
- core：`startDataLimit(type)` = min(本进程协调器为该类型注册的上限, 配置的进程上限, 线上硬上限 4 MiB)。没经过 kit 装配、也没有协调器的进程仍只按 4 MiB。
- 不再写进生成的配置段（去掉 `example`）：没有 saga 的 Nest 工程不会多出一个 `saga:` 段；要改上限就在发起方与协调器的配置里写同一个值（SAGA.md 写明）。
- 先红后绿：`kit/nest` 用例模拟“本进程没有协调器、配置 `saga.max_payload_bytes: 1024`”，Nest 事务里 `EmitStart` 2048 字节 → 修前提交成功，修后 `ErrInvalidRecord`、事务不提交。

## 6. 兼容

线上未部署，不做兼容。完成判定除第 3 节说明的不可能情形外行为不变；S7 收紧：发起方按配置值拒绝（之前按 4 MiB）。
配置声明里 `saga.max_payload_bytes` 加了 `max 4194304`（之前超过时由 `saga.New` 报错，现在在配置检查时报错）。

## 7. 验证

`GOWORK=off`：`gofmt -l`；saga、kit/saga、kit/nest 的 vet（含 `-tags integration`）与 `-race -count=3`；私有副本集 `-run '^TestRealMongo'` 与跨进程强杀；
`go generate` 后 codegen 测试、game-demo 重新生成；push 前全量 `go test ./...`。结果见 `docs/bugfix/RR-20261006-66.md` 与本文第 8 节。

## 8. 实施状态

已实施（`133a4e88`，分支 `sagarule`，未发版）。

**代码量（如实）**：`Complete` 从 69 行减到 59 行，文档注释从 18 行（5 条编号规则）减到 3 行；新增 `completionVerdict` 类型与四个常量、
`judgeCompletion`（连注释 50 行）。`saga/engine.go` 合计 1208 → 1232 行（非注释非空行 1028 → 1047，+19）：规则没有变少的代码量，
变化在于判定集中到一个纯函数（不读 Store、可穷举测试），`Complete` 只剩按结论分派。守卫 `saga/completion_rule_guard_test.go` 124 行。
S7：`saga/nest.go` +16 行，`kit/mods/saga_payload.go` 12 行，`kit/nest` / `kit/saga` 各 +3～4 行。

**守卫先红**：把 `engine.go` 换回基线（只补一个空的 `judgeCompletion` 让包能编译），结构守卫报出：

```text
--- FAIL: TestCompleteJudgesOnlyThroughJudgeCompletion (0.00s)
    completion_rule_guard_test.go:115: Complete reads record.Incarnation: the acceptance rule lives only in judgeCompletion
    completion_rule_guard_test.go:108: Complete calls positionedAt: the acceptance rule lives only in judgeCompletion
    completion_rule_guard_test.go:115: Complete reads record.Status: the acceptance rule lives only in judgeCompletion
    completion_rule_guard_test.go:115: Complete reads record.OperationKey: the acceptance rule lives only in judgeCompletion
    completion_rule_guard_test.go:115: Complete reads record.CommandID: the acceptance rule lives only in judgeCompletion
    completion_rule_guard_test.go:108: Complete calls openOperation: the acceptance rule lives only in judgeCompletion
    completion_rule_guard_test.go:122: Complete calls judgeCompletion 0 times, want exactly one decision entry
```

**原有用例**：`saga` 包全部用例原样通过（未改任何既有测试文件），含 U-0280（`step_operation_*`）、B1（`step_operation_incarnation_promises_test.go`、
`mongo_resume_incarnation_promises_test.go`）、方向 ①②③④（`step_transition_guard_test.go`、`saga_direction_3_4_promises_test.go`、`saga_reopen_observability_promises_test.go`）、
单状态文档（`step_operation_state_promises_test.go`）、RR-42～47（`step_success_in_backoff_promises_test.go`、`start_rejection_promises_test.go` 等）；
私有副本集 `^TestRealMongo` 与跨进程强杀 `TestRealSagaCrossProcessKillRecovers` 通过。验证明细见 [RR-20261006-66 修复记录](../bugfix/RR-20261006-66.md)。
