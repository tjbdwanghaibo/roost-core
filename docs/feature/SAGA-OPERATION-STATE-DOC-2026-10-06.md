# saga 步骤收件箱：每个操作一份状态文档（2026-10-06）

**来由**：维护者决定，原话“我希望直接改成一份状态文档”，本轮直接做，不留到下个大版本。兼容范围随后补充决定：
“不考虑旧进程，完成按照新的处理，线上还没有旧的进程跑”（见第 6 节）。

**基线**：`origin/main` `76885daa`，分支 `sagadoc`。代码图谱共享 generation 停在 09-30（`saga/step_operation_inbox.go` 未入索引，
`dataengine_step_inbox.go`、`command_consumer.go` 为 `metadata_changed`），本方案以当前源码为准。

## 1. 为什么改

`stepOperationInbox`（原生 `DataEngineStepInbox` 与 Mongo `MongoCommandInbox` 共用）每次尝试写一份 claim（`saga-step/<CommandID>`），
另写守卫 `saga-step-op/<IdempotencyKey>` 串行化；每次判定都要按 `operation_key` 把这个操作的 claim 查出来筛。由此连续出现：

| 编号 | 问题 | 补法 |
| --- | --- | --- |
| RR-20261006-14 | `stepTransition` 守卫盲区 | 结构收拢 + `go/types` 守卫（与收件箱形状无关，本次不动） |
| RR-20261006-15 | 跨 Resume 累积的 claim 超过 4096，这一步永远 Reserve 不了；查询耗时随 claim 数线性增长 | `outcome` 字段 + 只取有影响的 claim + 索引 `by_operation_decision` |
| RR-20261006-16 | 升级前写的 4097 份旧 claim 撞上限 | 旧 claim 单独计数（`maxLegacyOperationClaims`） |

三次补法都在“按操作查一组 claim”之上加状态与上限。判定真正需要的只是：当前谁持有租约、这个操作有没有成功、本生有没有拒绝。
把它们收进每个操作一份文档，判定只读这一份，尝试次数、Resume 次数、历史 claim 数都不再进入判定，上限与查询代价随之消失。

## 2. 文档形状与索引

集合：原生 `_dataengine_step_operations`（替代 `_dataengine_inbox_claims`），Mongo 步骤 `<收件箱集合>_operations`（替代 `<收件箱集合>_claims`，
缺省 `_saga_step_inbox_operations`）。回执不变：原生 `_dataengine_receipts` 的 `saga-step/<CommandID>`，Mongo 收件箱集合的 `_id = CommandID`。

```text
_id            操作实例 = Command.IdempotencyKey（<saga>:<phase>:<step>）
version        收件箱每次写入加一；Reserve 的写以它做 CAS
-- 当前尝试（最近一次取得租约的尝试）；投影的 lease fence 直接匹配这几个顶层字段 --
command_id     当前尝试的 CommandID
incarnation    它的代际（从 CommandID 解析，与协调器同一函数）
digest         它的命令摘要（含 CommandID，所以也标识尝试）
owner          持有租约的收件箱实例
lease_token    租约代号，整个操作单调递增：每次授予租约（新尝试、接替、同一命令重新取得）加一
lease_until    租约到期 = min(取得时 now + LeaseDuration, 命令截止)；结论写入或交还后改为过去的时间
deadline_at    当前尝试的命令截止（被接替时记进 superseded）
status         pending（结论未知）| settled（结论已写）
result         settled 时：success | refused | retryable
completion     settled 时：结论的 completion effect 载荷
-- 历史，都有界 --
refusals       {"r<代际>": {command_id, completion}}，最多保留代际最大的两生（见 4.5）
refusals_dropped_through  被剪掉的拒绝里最大的代际（没剪过就没有这个字段）
superseded     [{command_id, by, deadline_at}]：最近被接替的尝试，最多 16 条，按接替先后剪掉最早的（见 4.4）
created_at / updated_at / expires_at
```

索引只有 `ttl_expires_at`（`expires_at`，`ExpireAt`）。所有读写都按 `_id`；投影 fence 的条件也以 `_id` 开头。
`expires_at` 每次写入刷新为 `now + receiptTTL`（缺省 30 天），与回执同一保留期。

**为什么当前尝试放顶层**：DataEngine 投影对 fence 文档做的条件写是固定的（`coredata.LeaseFence.Predicate`：`_id`、`owner`、`lease_token`、
`digest`、`status=pending`、`lease_until > now`，确认写 `updated_at`）。当前尝试的字段名与它一致，原生投影一行不改，fence 指向
`(_dataengine_step_operations, <IdempotencyKey>)` 即可。

## 3. 状态转移与 CAS 条件

记号：`c` 是这次投递的命令，`s` 是读到的状态文档（可能不存在），`cur` 是 `s` 的当前尝试，`now` 是收件箱时钟。

### 3.1 Reserve（一个 Mongo 事务；原生与 Mongo 步骤共用）

按顺序判定，命中即停：

| # | 条件 | 结论 | 写入（全部在 Reserve 事务里） |
| --- | --- | --- | --- |
| 1 | `c` 自己的回执存在 | 回放（Duplicate + 那份 completion） | 若 `cur = c` 且 pending：结算（3.3，失败只记 Warn 与计数，RR-20260927-16） |
| 2 | `now ≥ c.DeadlineAt` | `ErrCommandExpired` | 无 |
| 3 | `s` 不存在 | 执行（新租约，token 1） | `insert`；并发插入撞唯一键由外层重试一次 |
| 4 | `c ∈ s.superseded` | `errAttemptSuperseded` | 无 |
| 5 | `cur = c`，摘要不同 | `ErrIdentityConflict` | 无 |
| 6 | `cur = c`，settled | 回放 `s.completion` | 无 |
| 7 | `cur = c`，pending，租约有效 | Duplicate（同一命令另一次投递在途，等它的回执） | 无 |
| 8 | `cur = c`，pending，租约过期 | 执行（重新取得自己的租约） | 取租约（3.2），不记 superseded |
| 9 | `cur ≠ c`，pending，`cur` 的回执存在（原生：已投影、未结算） | 先结算 `cur`（3.3），按 10～15 继续 | 结算写 |
| 10 | `cur` settled 且 success | 回放成功（任何一生） | 无 |
| 11 | `s.refusals` 有 `c` 这一生的拒绝 | 回放那份拒绝 | 无 |
| 12 | `c` 的代际 ≤ `refusals_dropped_through` | `errAttemptSuperseded`（见 4.5） | 无 |
| 13 | `cur` pending，租约有效 | `errOperationAttemptInFlight` | 无 |
| 14 | `cur` pending，租约过期 | 执行（接替 `cur`） | 取租约，`cur` 追加进 superseded（超过 16 条剪掉最早的），计 `saga.step_inbox.superseded_total` |
| 15 | 其余（`cur` 可重试失败，或别的生的拒绝） | 执行 | 取租约 |

**3.2 取租约**：`UpdateOne({_id, version: s.version}, $set{当前尝试 = c, owner = 本实例, lease_token = s.lease_token+1, lease_until, deadline_at,
status = pending, result/completion 清空, superseded（接替时追加）, refusals 剪枝后的表, version+1, updated_at, expires_at})`。
在事务里与任何并发写同一文档（另一 Reserve、投影的 fence 确认、结算、交还）写冲突，输家整笔重跑、重新判定；`MatchedCount = 0` 返回 `ErrConflict`。

**3.3 结算（写结论）**：

| 谁 | 条件（filter） | 写入 |
| --- | --- | --- |
| Mongo 步骤生效点（执行事务内，handler 之后） | `_id, command_id = c, digest, owner, lease_token, status = pending, lease_until > now` | `status = settled, result, completion, lease_until = 0, version+1`，拒绝时 `refusals.r<代际>`；不匹配 → `errAttemptFenced`，事务中止 |
| 原生（读到权威回执之后：Reserve 第 1 / 9 步、`Replay`、过期分支） | `_id, command_id = c, status = pending`（回执已是权威，不看租约） | 同上；不匹配不报错（已结算或已不是当前尝试） |

**3.4 投影确认（原生生效点，投影批量事务内）**：`LeaseFence.Predicate` 匹配状态文档顶层字段，`$set updated_at`。不匹配则整笔记录跳过（不变）。

**3.5 交还租约**（原生 handler 以 `ErrFencedEntityPending` 失败、Mongo 执行事务失败）：
`{_id, command_id = c, digest, owner, lease_token, status = pending} → lease_until = now, version+1`。

## 4. 契约逐条论证

1. **同一操作最多生效一次**。生效只有两条路：原生投影的 fence 确认、Mongo 的结算写，两者的条件都要求“`c` 是当前尝试、token 相同、pending、租约未到期”。
   当前尝试同一时刻只有一个；token 每次授予加一，旧尝试的 fence（WAL 里的旧 token）与旧 Reserve 里的 token 都不再匹配。成功一旦结算（第 10 步）就不再授予
   租约；成功已投影但未结算时，第 9 步先读到回执、结算后回放，不会接替。接替（第 14 步）与投影确认、结算写同一文档，在 Mongo 里只能有一个提交，
   投影先提交时接替的事务重跑后走第 9 步（RR-20260926-30 §6 的写冲突串行化不变）。
2. **在途时别的尝试要等**：第 13 步。只读的结论不写文档；并发 Reserve 中想写的那些在同一文档上写冲突串行化，作用等于原来的守卫文档。
3. **截止后能接替，旧执行者醒来后提交被 fence**：租约封顶到命令截止，截止后第 14 步接替、token 加一；原生旧记录投影时 fence 不匹配被跳过，
   Mongo 旧执行事务的结算条件不匹配，`errAttemptFenced` 中止整笔事务（业务写回滚）。
4. **尝试只在截止前生效，超时有晚成功告警**：`lease_until ≤ DeadlineAt`，fence 与结算都要求 `lease_until > now`；协调器侧的告警（`late_after_abandon`、
   B1 的按代际只告警一次）不在收件箱，未改。
4.4 **被接替的尝试不再执行**（第 4 步）：原实现靠被接替 claim 的 `superseded` 状态在保留期内永久记住。现在状态文档记最近 16 个被接替的尝试。
   真正需要这份记录的只有截止未到的被接替尝试：截止过后它的投递在第 2 步就 `ErrCommandExpired`。截止未到就被接替，只出现在租约被提前交还
   （`releaseLease`）或 `LeaseDuration` 短于步骤 `Timeout` 时，而协调器同一时刻只等一次尝试，正常运行里至多一两条。按接替先后剪掉最早的，
   被剪掉的那一条要在之后还有 16 次接替、自己的截止却仍未到才会被当作新尝试判定——即使出现，它仍受第 1 条约束，至多一次不受影响。
   不按截止时间剪：做接替的进程与投递旧尝试的进程时钟可能不同（`mongo_step_consumer_promises_test.go` 的“被接替的尝试 ack 不执行”就是
   接替方时钟在前的情形），按接替方时钟剪掉的记录在投递方看来仍可能截止未到。
5. **拒绝按代际回放（B1 incarnation）**：第 11 步只回放 `c` 这一生的拒绝；别的生的拒绝走第 15 步执行。
4.5 **为什么只保留两生的拒绝**：拒绝只对同一生里、截止未到的其他尝试有用。一生里协调器在收到拒绝后不再派发这个操作的尝试，同一生里截止未到的
   尝试只能是收到拒绝之前派发的那一次（等最后一次尝试时收到较早尝试晚到的拒绝）。它的截止通常早已过去；要让它在两次更新的“被拒绝的一生”之后还
   截止未到，运维必须在一个步骤 `Timeout` 内连续两次 Resume、两生都被拒绝。所以保留代际最大的两生；更老的一生被剪掉时记 `refusals_dropped_through`，
   这样的一生的投递不能证明“这一生没有拒绝”，按第 12 步不执行（ack），而不是冒险执行——协调器已经前进了至少两生，不再接收它的结果（B1）。
6. **`stepTransition` 是唯一出口**：协调器（`engine.go`、`step_transition.go`）不改，RR-14 的 `go/types` 守卫照常有效，不需要调整。
7. **原生步骤的生效点仍在投影的批量事务里**：fence 仍由 `Bind` 绑进 Nest 事务，只是 `Resource` / `DocumentID` 指向状态文档；`dataengine` 不改。
8. **Mongo 步骤仍是 Reserve 事务 + 执行事务**（维护者已选 A）：Reserve 事务的命令从 5 条（读回执、读 claim、守卫 upsert、按操作查询、写 claim）
   降为 3 条（读回执、读状态、写状态），执行事务的结算仍是一次条件更新；提交次数不变，延迟不应变差（第 8 节实测）。

## 5. 两种收件箱怎么接

| | 原生 `DataEngineStepInbox` | Mongo `MongoCommandInbox` |
| --- | --- | --- |
| 状态文档集合 | `_dataengine_step_operations` | `<collection>_operations` |
| Reserve | `stepOperationInbox.reserve`（共用，3.1） | 同左 |
| 生效点 | 投影事务的 fence 确认（`Bind` 的 `LeaseFence{Resource: _dataengine_step_operations, DocumentID: IdempotencyKey}`） | 执行事务里 handler 之后的结算写（3.3） |
| 结论写入 | 读到回执后结算（Reserve、`Replay`、过期分支） | 结算与回执在执行事务里一起提交 |
| 交还租约 | handler 以 `ErrFencedEntityPending` 失败 | 执行事务失败 |

`Reservation` 增加未导出的 `operationKey`（交还、结算、`Bind` 校验用），导出字段不变。消费者（`SubscribeDataEngineStep`、`SubscribeMongoStep`）
对各种错误的处理不变；`operationSuccess`（不执行的投递 ack 前重发成功）改为读状态文档。

## 6. 迁移与兼容：不兼容

**不兼容**。理由：维护者 2026-10-06 决定（“不考虑旧进程，完成按照新的处理，线上还没有旧的进程跑”），线上未部署，没有旧数据。

- 新代码不读旧 claim 集合（`_dataengine_inbox_claims`、`<收件箱集合>_claims`），不写旧格式，不支持与 v1.22.0 及以前（以及 main 上 RR-15/16 之后）的步骤进程混跑。
- 存储形状改变，**不兼容 v1.22.0 及以前写下的收件箱数据**：升级时先停掉全部旧步骤进程，清空或丢弃旧 claims 集合（`db.<name>.drop()`），
  再起新进程。旧集合不会被新代码读取或删除。WAL 里旧进程留下、尚未投影的原生步骤记录的 fence 指向旧 claim 文档，升级前要让旧进程排空 WAL。
- 回执集合、协调器记录、tombstone、wire、摘要都不变；协调器不需要同时升级。

## 7. 删掉的东西

- claim 文档、守卫文档、claims 集合及其索引（`claim_expired`、`uniq_command`、`by_operation`、`by_operation_decision`、claims 的 `ttl_expires_at`）；
- claim 的 `outcome` 字段、`claimOutcome*`；
- 上限 `maxDecisiveOperationClaims`、`maxLegacyOperationClaims`（它们的前身 `maxOperationAttempts`）与超限的 `ErrConflict`；
- 按操作查询 `operationClaims` / `operationClaimsFilter`、`resolveOtherAttempts`、`guardOperation`、旧 claim 的读取与计数路径；
- 混跑用例 `saga/mongo_step_mixed_version_real_mongo_integration_test.go` 与 `ROOST_SAGA_MIXED_OLD_BINARY`；
- RR-15/16 的旧格式用例：`step_operation_legacy_claims_promises_test.go`、`step_operation_claims_cost_real_mongo_integration_test.go`、
  `TestOperationClaimsFilterSelectsOnlyDecisiveClaims` 及真实 Mongo 版本。

## 8. 测试计划

- **不改断言照常通过**：`saga/*_promises_test.go`（U-0280、U-0281、B1、方向 ②、O-S5-1）、`mongo_step_operation_promises_test.go`、`TestRealMongo*`、
  跨进程强杀 `TestRealSagaCrossProcessKillRecovers`（每个操作恰好一次提交）。只读写 claim 内部结构的断言改为读状态文档的同义断言（如“第一次尝试被接替、
  token 为 2、接替者是第二次尝试”改为看 `superseded` 与当前尝试），行为断言不动。
- **上限用例改写**：`TestOperationAttemptsAccumulatedOverResumesDoNotBlockANewLife` 改为真实 `Handle` 走 5 生 × 820 次尝试（超过原 4096），断言新一生
  照常执行、之后回放，且这个操作只有一份文档；真实 Mongo 版本同形（次数少一些）。
- **新增**：状态文档满足投影 fence 谓词且每个字段都起作用（替代原 claim 版本）；拒绝只保留两生、更老一生的投递不执行；被接替尝试的记录在截止后剪掉；
  多进程用例（原混跑用例里验证契约本身的三个场景改为同一版本的两个进程：在 handler 里被 SIGKILL 后接替、停在事务里的提交被 fence、两进程并发至多一次）。
- **基准**：私有三节点副本集（`scripts/mirror-local.sh`），`BenchmarkRealMongoStepThroughput/g=1,8,32`（Mongo 步骤）与新增的原生 Reserve 吞吐基准，
  当前 main 与本分支各编一个测试二进制交替跑，benchstat n ≥ 6。
- 常规：`gofmt`、`go vet`（含 `-tags integration`）、`-race -count=3 ./saga/... ./kit/saga/...`、glsvet、codegen、根包、`go build ./... && go vet ./...`。

## 实施状态

**已实施，未发版**（`a013f9ff`，分支 `sagadoc`）。证据在 [evidence/sagadoc](evidence/sagadoc)。

### 改动

- `saga/step_operation_inbox.go`：重写为状态文档 `stepOperation` 与 3.1 的判定表（`reserveInTransaction` 的注释按表中行号写）；`grantLease`（CAS 取租约、
  接替时记 superseded、剪枝拒绝）、`settleOwnAttempt`（Mongo 生效点）、`markCompleted` / `settleFromReceipt`（原生读到回执后结算）、`releaseLease`、
  `operationSuccess` 都只读写这一份文档。删除 claim、守卫、`operationClaims` / `operationClaimsFilter` / `resolveOtherAttempts` / `guardOperation` /
  `supersede` / `attemptResult`、`outcome`、两个上限。
- `saga/dataengine_step_inbox.go`：集合 `_dataengine_step_operations`；`Bind` 的 fence 指向 `(_dataengine_step_operations, IdempotencyKey)`；`Reservation`
  多未导出的 `operationKey`，`ReservationFromContext` / `Bind` 一并校验。
- `saga/command_consumer.go`：集合 `<collection>_operations`；执行事务的生效点改调 `settleOwnAttempt`；注释更新。消费者分支不变。
- `dataengine` 不改（fence 谓词原样匹配状态文档的顶层字段）；协调器（`engine.go`、`step_transition.go`）与 RR-14 守卫不改。
- 测试：只读写 claim 内部结构的断言改读状态文档（同义：被接替、token、lease_until 封顶、结算、fence 谓词逐字段起作用、索引形状）；
  `step_operation_attempt_cap_promises_test.go` 改写（5 生 × 820 次真实 `Handle`，断言新一生执行、之后回放、只有一份文档；真实 Mongo 版 5 × 220）；
  新增 `step_operation_state_promises_test.go`（拒绝只留两生、被剪掉的一生不执行；被接替的尝试截止前不执行、截止后过期、只记最近 16 条）、
  `mongo_step_multiprocess_real_mongo_integration_test.go`（原混跑用例的三个契约场景，两边同一版本）、`step_operation_benchmark_real_mongo_integration_test.go`
  （原生 Reserve 吞吐基准）、`TestRealMongoTakeoverFencesTheEarlierAttemptsProjection`（接替先提交时旧尝试的投影被 fence，见下）。删除混跑、RR-16 旧 claim、RR-15 查询耗时 / 过滤器用例。`dataengine/engine/lease_fence_integration_test.go` 与 codegen 夹具
  `testdata/dataengine/fenced_step_test.go` 改为把状态文档的租约改到过去。

### 实施中的一处修正（4.4）

初版按截止时间剪 `superseded`、并且不记截止已过的被接替尝试。`-race -count=3` 时 `TestMongoStepConsumerFollowsTheOperationInbox/a superseded attempt is acknowledged without running`
变红（`delivery of the superseded attempt = saga: another attempt of this step operation holds a live lease, want nil (ack)`）：接替方的时钟在前，按它判断
截止已过的尝试，在投递方看来截止未到。改为按接替先后只留最近 16 条、不按截止剪（4.4 已改写），该用例与全包转绿。

### 验证（`GOWORK=off`）

- `gofmt -l` 空；`go vet ./saga/... ./kit/saga/...` 与 `-tags integration`（含 `./dataengine/...`）；`go build ./... && go vet ./...`；
  `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` 通过。
- `go test -race -count=3 ./saga/... ./kit/saga/...` 通过：U-0280、U-0281、B1、方向 ②、O-S5-1、RR-14 守卫（`TestEveryCoordinatorWriteGoesThroughStepTransition`、
  `TestStepTransitionAloneDecidesTheIncarnation`）与全部 `*_promises_test.go` 不改断言。
- 私有三节点副本集（`ROOST_MIRROR_LOCAL_HOME=<scratchpad>/sagadoc-env ROOST_MIRROR_LOCAL_OFFSET=26300 scripts/mirror-local.sh up`，用完 `clean`）：
  `go test -tags integration -count=1 ./saga/`（全部 integration 用例，含 `^TestRealMongo`、多进程、真实 NATS）通过；跨进程强杀
  `TestRealSagaCrossProcessKillRecovers`：60/60 完成、120 个操作、120 次提交、**0 个操作被多次提交**，恢复 1m1～1m5s（仍由遗留事务锁主导，
  [cross-process-kill.txt](evidence/sagadoc/cross-process-kill.txt)）；`TestRealMongoFencedProjectionSerializesWithLeaseTakeover`（dataengine，投影与接替写同一状态文档）
  `-count=3` 通过；`scripts/test-dataengine-generated.sh`（含 codegen 夹具 `TestGeneratedDataEngineFencedStepSkipDoesNotFenceTheEntity`）通过。
  混跑用例已按维护者决定删除，不再有 `ROOST_SAGA_MIXED_OLD_BINARY`。
- `go test -count=1 ./codegen/...`、根包 `go test -count=1 .` 通过。生成形状没变（模板与生成器未改），没有重新生成 game-demo。

### 基准（[bench.txt](evidence/sagadoc/bench.txt)）

同机（Apple M5，load average 3.7～5.4，其他 agent 同时在跑），私有三节点副本集（mongod 8.0.28），base = `origin/main` `76885daa`、after = 本分支，
各编一个测试二进制交替 6 轮，`-benchtime 1000x`，benchstat 中位数：

| 基准 | 协程 | ops/s base → after | p50 ms | p99 ms | allocs/op |
| --- | --- | --- | --- | --- | --- |
| Mongo 步骤 `Handle`（`BenchmarkRealMongoStepThroughput`） | 1 | 54.2 → 55.0（~，p=0.29） | 18.7 → 18.1 | 26.0 → 26.0 | 1343 → 980（−27%） |
| | 8 | 287 → 278（~，p=0.49） | 27.0 → 28.0 | 41.7 → 44.2 | 1352 → 987 |
| | 32 | 972 → 978（~，p=1.00） | 30.6 → 29.9 | 70.5 → 76.6 | 1350 → 987 |
| 原生 `Reserve`（`BenchmarkRealMongoNativeReserveThroughput`，新增） | 1 | 109 → 110（~，p=0.85） | 9.11 → 9.07 | 14.2 → 14.5 | 834 → 460（−45%） |
| | 8 | 582 → 552（~，p=0.09） | 13.2 → 14.0 | 23.3 → 24.9 | 843 → 464 |
| | 32 | 1734 → 1671（~，p=0.39） | 16.1 → 17.6 | 42.1 → 46.1 | 843 → 467 |

延迟与吞吐没有统计显著的变化（全部 p > 0.05）；原生 g=8 的 −5% 接近显著，单独交替复测 8 轮（`-benchtime 2000x`）为 619 → 607 ops/s（p=0.28），
仍在噪声内。分配下降 27～45%。和预期一致：每次尝试的落盘提交次数没变（Reserve 1 次 + 生效点 1 次），Reserve 少了两条数据命令（守卫 upsert、按操作查询），
而单条数据命令只有 0.1～0.3 ms，被 8～9 ms 的提交淹没（[延迟分析](SAGA-MONGO-STEP-LATENCY-2026-10-06.md)）。好处在于不随历史变慢：同一操作累积再多尝试，
Reserve 读写的仍是一份文档。

### 未完成 / 风险

- 代码图谱共享 generation 停在 09-30，本次没有刷新索引（其他会话在用共享 daemon）。
- `TestRealMongoSupersedeAndProjectionOfTheSameAttemptSerialize` 两次实跑都是 40/40 轮“k 投影、k+1 回放”，没有出现“接替先赢”的一边（修前记录 39 / 1）；
  用例断言的是“从不两者都生效”，照常通过（哪一边先赢由时序决定；dataengine 的 `TestRealMongoFencedProjectionSerializesWithLeaseTakeover` 实跑同样是投影先赢）。
  接替先赢的一边新增确定性用例 `TestRealMongoTakeoverFencesTheEarlierAttemptsProjection`：k 写进 WAL 后 k+1 接替，再投影 k 的记录（投影器时钟仍在 k 的租约内），
  k 因 token 不匹配被跳过、没有回执（真实副本集 `-count=3` 通过）；mongotest 上的同类场景是 `step_operation_promises_test.go` 的 (d)。
- 生产形态（Linux、跨主机副本集）没有测。
