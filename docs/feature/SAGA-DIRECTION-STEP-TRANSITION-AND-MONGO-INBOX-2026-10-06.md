# saga 方向 ①②：离开步骤收成一个转移，Mongo 步骤纳入操作实例收件箱（2026-10-06）

**来由**：维护者第六轮决定“saga 方向：按推荐 ①②，③④ 暂不做”（[DECISIONS-PENDING](../review/DECISIONS-PENDING-2026-10-05.md) 末表）。
方向判断见 [N06 S5 review](../review/REVIEW-2026-10-06-n06s5.md)“方向判断”；同一不变量“每个操作实例的结果恰好被计入一次，不被计入的有人知道”的修复链：
U-0280（`054fdd66`）→ 复核两处（`23b97942`、`877bb66c`）→ B1（`3fabe34d`）→ NC-250。
**基线**：`origin/main` `b3538251`。代码图谱共享 generation 停在 09-30，`saga/engine.go`、`command_consumer.go`、`dataengine_step_inbox.go`、`mongo_store.go`、
`nest_completion_consumer.go`、`codegen/internal/roost/add.go` 均 `metadata_changed`，本方案以当前源码为准。

## ① 离开当前步骤：一个转移函数（纯重构）

### 现状

协调器写记录的出口有 8 个，各自拼 `ApplyRequest`：

| 出口 | 位置 | 关闭哪个操作 | 代际 |
| --- | --- | --- | --- |
| 派发（不离开步骤） | `processClaimed` 末尾 | — | — |
| 等结果超时（重试 / 用尽） | `processClaimed` Waiting 分支 | `closedOperation(record, after)` | — |
| saga 截止 | `processClaimed` | `abandonedOperation(record)`（NC-250） | — |
| 定义缺失 | `processClaimed` | `abandonedOperation(record)` | — |
| 步骤下标越界 | `processClaimed` | 不关闭 | — |
| 接收结果（成功 / 拒绝 / 可重试失败） | `Complete` | `closedOperation`，成功时再显式给 `IdempotencyKey` | — |
| 人工 Compensate | `Compensate` | `abandonedOperation(record)` | 补偿方向时 `Incarnation++` |
| Resume | `Resume` | 不关闭 | `Incarnation++` |

“放弃关闭”这条子规则在截止、人工 Compensate、定义缺失三个出口各漏写过一次（U-0280 补前两个，NC-250 补第三个）；`closedOperation` 与
`abandonedOperation` 是同一个问题（“离开前在哪个操作上”）的两份回答。

### 目标

`saga/engine.go` 新增唯一的转移构造 `stepTransition(before, after Record, cause transitionCause, receipt *Completion, outbox *OutboxRecord) ApplyRequest`，
所有 `store.Apply` 的请求都由它产生，它在一处决定：

1. **开着的操作**（`openOperation(record)`）：`Waiting` 时是 `OperationKey`；`Pending` / `Compensating` 且 `Attempt > 0`（已派发、在重试退避）时是当前方向 + 步骤的操作；
   其余（还没派发、终态、`Failed` / `ManualRequired`——进入它们时已经关闭过）为空。
2. **关闭**：`before` 开着的操作在 `after` 里不再开着（方向、步骤、状态变了）就关闭它；接收了成功结果时关闭结果所属的操作（Resume 之后还没派发、
   记录停在该操作上时 `before` 没有开着的操作，B1）。tombstone 的“带结果 / 放弃”仍由 Store 按 `Receipt.Success` 记（`ApplyRequest` 文档已写明），
   排队命令随关闭在同一事务删除，新尝试的 outbox 替换旧排队命令（不变）。
3. **开新代际**：`Resume` 总是；人工 `Compensate` 在补偿方向停下的记录上（B1）。由 `cause` 决定，调用方不再自己改 `Incarnation`。
4. 协调循环的出口带租约（`ExpectedLease`），`Complete` / `Compensate` / `Resume` 不带（只按版本 fence，不变）。

删除 `abandonedOperation`、`closedOperation`。`Complete` 里“成功时显式关闭 `IdempotencyKey`”并入规则 2。

### 行为对照（逐出口）

- 截止、定义缺失：`before` ∈ {Pending, Waiting, Compensating}，`openOperation` 与 `abandonedOperation` 相同；`after` 是补偿 / Failed / ManualRequired，`Attempt=0` 或终态，不再开着。相同。
- 超时：重试时 `after` 同步骤 `Pending/Compensating` 且 `Attempt` 不变（≥1），仍开着同一操作 → 不关闭；用尽 → 关闭。与 `closedOperation` 相同。
- 接收结果：成功后 `Attempt=0`、步骤推进 → 关闭；可重试失败未用尽 → 仍开着；拒绝 / 用尽 → 关闭；Resume 后未派发时接收旧一生成功 → 规则 2 的结果分支。相同。
- 人工 Compensate：Pending 退避 → 关闭（同 NC-250）；`ManualRequired` 上 `abandonedOperation` 会在 `Attempt > 0` 时再关闭一次已关闭的操作——MongoStore 与内存 Store
  对已存在的 tombstone 不改关闭方式，排队命令在第一次关闭时已删，新规则不再重复关闭，可观察结果相同。
- Resume：`before` 是 Failed / ManualRequired，不关闭。相同。
- 步骤下标越界：`Attempt > 0` 时新规则会以放弃关闭这个操作，旧代码不关闭。正常运行不可达（退避中的记录步骤必然有效，`retryState` 对无效步骤直接 ManualRequired），
  只在同类型同版本的定义被换成步骤更少的定义时出现；那时这个操作确实被放弃，按契约第 4 条记放弃是更正确的结果。列为唯一的行为差异。

### 防止新出口绕过

守卫测试 `saga/step_transition_guard_test.go`：解析 `saga` 包全部非测试源码，断言 `ApplyRequest{...}` 复合字面量只出现在 `stepTransition` 里、
`Engine` 方法里对 `Incarnation` 的赋值 / 自增只出现在 `stepTransition` 里、`store.Apply(` 的参数都是 `stepTransition(...)` 的调用或其结果变量。
新增出口若手拼请求，测试报出文件与行号。选测试而不是 glsvet：规则只针对一个包、一个函数，放在包内最近处；glsvet 是跨包的执行契约检查。
（发版前复审补强：原守卫看不到不写字面量的两种绕过——改 `stepTransition` 返回的请求字段（如 `request.CloseOperation = ""`）、`var request ApplyRequest` 逐字段拼请求再经 `store := e.store` 的别名写入。现在这两种以及 `new(ApplyRequest)`、对结果取地址都报出，负对照固定在 `saga/testdata/stepguard`，由 `TestStepTransitionGuardSeesBypassesWithoutALiteral` 每次运行。）

## ② Mongo 步骤纳入操作实例收件箱

### 现状与问题

`MongoCommandInbox.Handle` 在一个 Mongo 事务里先插入以 `CommandID` 为 `_id` 的回执（占位）、再跑 handler、再写入 completion，只保证“同一命令最多一次”。
跨尝试靠业务按 `IdempotencyKey` 幂等。提交点（事务提交）与命令截止没有绑定：N06 S5 实跑里尝试 k 在 WriteConflict 重试里拖过截止才提交，协调器已发出 k+1，
k+1 也提交，同一操作两份业务写（O-S5-4），也是“放弃后迟到成功”的主要来源。

### 目标契约（与原生步骤同一张表）

1. 新尝试先看同一操作实例的其他尝试：有成功（任何一生）或本生的拒绝 → 回放，不执行；
2. 有在途尝试且租约有效 → 等待（可重试错误，nak 后重投再判断）；
3. 租约过期 → 接替（`superseded` + `lease_token+1`）；
4. 尝试只在截止前生效：租约 `lease_until = min(now + LeaseDuration, Command.DeadlineAt)`，handler 事务提交前对自己的 claim 做条件写
   （owner、token、`pending`、`lease_until > now`），不匹配就中止事务，业务写随之回滚。

### 实现：复用原生收件箱的守卫、claim 与判定

把 `DataEngineStepInbox` 里与“操作实例”有关的代码（`reserveInTransaction`、`guardOperation`、`resolveOtherAttempts`、`operationSuccess`、`attemptResult`、
`supersede`、`markCompleted`、`releaseLease`）原样移到 `saga/step_operation_inbox.go` 的 `stepOperationInbox`，两个收件箱各嵌入一份，差别只在：

| | 原生 `DataEngineStepInbox` | Mongo `MongoCommandInbox` |
| --- | --- | --- |
| claim / 守卫集合 | `_dataengine_inbox_claims`（不变） | `<inbox 集合>_claims`（新集合，默认 `_saga_step_inbox_claims`），文档格式与原生相同 |
| 回执 | `_dataengine_receipts`，投影写 | `<inbox 集合>`，`_id = CommandID`（格式不变），handler 事务写 |
| 生效点（fence） | 投影事务对 claim 的条件写 | handler 事务里对 claim 的条件写（同一事务写回执、claim 标 completed） |

`Handle` 分两步：

1. **Reserve 事务**（共用代码）：本命令回执 → 回放；自己的 claim 有效 → 同一命令在途（返回可重试错误）；截止已过 → `ErrCommandExpired`；
   写守卫 `saga-step-op/<IdempotencyKey>`，按 `operation_key` 查同一操作的 claim 做判定；新建或接管自己的 claim。
2. **执行事务**：handler（业务写只经事务 ctx）→ 对自己的 claim 条件写（标 completed、存 completion）→ 插入回执（旧格式）。条件写不匹配 → `errAttemptFenced`，事务中止。
   接替写的是同一个 claim 文档，与这里的条件写只能有一个提交。

handler 事务失败（handler 错误、取消、fence）后交还租约（`lease_until = now`，只对仍 pending、本 owner / token 的 claim），重投能立刻重新 Reserve，与改动前“失败后重投立即重跑”一致。
提交结果未知（提交时超时）时交还条件不匹配（已提交的 claim 是 completed），重投读到回执回放。

**提交点**：执行事务的提交。业务写必须经 handler 拿到的事务 ctx 写进同一个 Mongo 事务，才在契约内；对另一个服务的调用（gift deliver 发邮件）不在事务里，
被 fence 中止的尝试可能已经发出调用，**这类步骤仍需要业务按 `IdempotencyKey` 幂等**。业务写在事务内的步骤，业务幂等从“必需”降为可选的纵深防御。

消费者 `SubscribeMongoStep`：`ErrCommandExpired` / `errAttemptFenced` → 重发同一操作已生效的成功（复用 `replayOperationSuccess`）后 ack；
`errAttemptSuperseded` → ack；`errOperationAttemptInFlight` → nak；回放其他尝试的 completion 原样发布（`CommandID` 是那次尝试的，协调器按它去重与核对代际）。
过期分支在没有自己的回执时同样先重发同一操作已生效的成功（与原生 U-0280 复核 2 一致）。

### 时间与选项

`CommandInboxOptions` 增 `Owner`（缺省 `saga-mongo-inbox-<随机>`，每个收件箱实例一个）与 `LeaseDuration`（缺省 1 分钟，再封顶到命令截止）。
不加 `LeaseDuration > AckWait` 校验：租约总被截止封顶，截止后的投递走过期分支。依赖步骤进程与协调器的时钟偏差远小于 `Timeout`（与原生相同）。

### 兼容

- 持久格式只增：新集合 `<inbox 集合>_claims`（claim / 守卫，索引 `uniq_command`、`by_operation`、`claim_expired`、`ttl_expires_at`）；回执集合格式不变；
  claim 的 `completion` 字段是 completion effect 载荷（与原生相同）。
- 混跑：旧步骤进程不写 claim，新进程看不到它处理的尝试（只按回执看到同一命令）；同一命令最多一次在混跑中仍成立（双方都写同一个回执 `_id`）。
  跨尝试最多一次只在全部 Mongo 步骤进程升级后成立；之前仍靠业务幂等。新协调器与旧步骤进程、旧协调器与新步骤进程都按原协议工作。
- 已生成工程不迁移（维护者决定）；仓库内模板（gift deliver 注释）与 `roost add saga` 生成物同步注释。`Handle` 签名不变；新增的不执行分支错误是包内哨兵，
  直接调用 `Handle` 的代码会多看到 `ErrCommandExpired`（导出）与可重试的“在途”错误。

### 代价

每次执行多一个 Reserve 事务（读回执、读 claim、守卫 upsert、按操作查询、写 claim），执行事务多一次条件更新。mongotest 与真实 Mongo 各测一次（见实施状态）。

## O-S5-1：两个结果消费者的终态分类

②之后普通结果流上的 completion 都来自带操作实例回放的收件箱（Mongo 步骤、原生步骤的回放），“退避中到达的成功”不再需要靠普通流 nak 等协调器回到等待：
下一次尝试会回放它，过期投递也会重发它。所以两个消费者共用 `isTerminalCompletionError`，并把 `ErrIdentityConflict` 加入（同一 CommandID 不同内容，重投不会变）。
普通流从此对 `ErrNotWaiting` / `ErrNotFound` / `ErrIdentityConflict` / `ErrDefinitionMissing` / `ErrInvalidRecord` Term，不再 nak 到 `MaxDeliver`。
（发版前审查更正：`ErrDefinitionMissing` 移出终态，两条流一起改为 nak 退避——滚动发布时结果可能先到还没升级的协调器，定义会随新进程上线；
定义一直不来时由协调器自己的定义缺失 fence（NC-250）收尾。共用 `isTerminalCompletionError` 不变。回归
`saga/completion_definition_rollout_promises_test.go`，记录见 [发版前审查观察收尾](../bugfix/PRERELEASE-AUDIT-FOLLOWUP-2026-10-06.md)。）
混跑代价：仍有旧 Mongo 步骤进程时，“退避中到达的成功 + 最后一次尝试过期”这一角落会少一次被接收的机会（旧进程过期分支不重发），按契约第 4 条落到告警。

## O-S5-3：被杀进程遗留的 Mongo 事务锁

不改默认步骤预算（它约束所有 saga 的失败检测时间，且要到 ~12 次才盖住默认 60s）。写进 SAGA.md 与 USER_GUIDE：生产建议把服务端 `transactionLifetimeLimitSeconds` 调到 20s
（框架自己的事务都由 `mongo.transaction_timeout`（30s）约束整个重试过程，单次事务远短于 20s；被服务端中止的事务按 TransientTransactionError 重跑），
或让 Mongo 步骤的 `Timeout × MaxAttempts` 加退避长于该参数。

## 验证计划

- ①：U-0280、U-0281、B1、NC-250 与全部 `step_operation_*` 用例不改断言通过；守卫测试；对一个手拼请求的出口做负对照（临时加一处，测试变红）。
- ②先红后绿（真实 Mongo，`-tags integration`）：尝试 k 已提交、结果未送达，协调器发出 k+1：修前业务写两次，修后一次并回放 k 的结果；
  k 在执行事务里卡过截止、k+1 接替：修前两次，修后一次（k 被 fence 中止）。mongotest 上同样的确定性用例。
- O-S5-1：普通结果消费者对五种终态错误 Term 的用例。
- 跨进程强杀：`TestRealSagaCrossProcessKillRecovers` 断言每个操作恰好一次业务提交。
- 性能：`BenchmarkMongoCommandInboxHandle` 在 mongotest 与真实 Mongo 上前后对照。
- 常规：`gofmt`、`go vet`、`go test -race -count=3 ./saga/... ./kit/saga/...`、根包、`go build ./... && go vet ./...`、codegen 测试、`go generate` porcelain、生成 game-demo build / vet / test。

## 实施状态

**已实施，未发版**。① `a95cf4dc`（纯重构）；② 与 O-S5-1、文档 `9669d181`。证据里的基线 `a5e7b070` 是 ① 在 rebase 前的提交（内容同 `a95cf4dc`）。证据在 [evidence/sagadir](evidence/sagadir)。

### ①

- `saga/step_transition.go`：`stepTransition` / `openOperation` / `transitionCause`；`engine.go` 8 个出口全部改调它，删除 `abandonedOperation`、`closedOperation`。
- 守卫 `saga/step_transition_guard_test.go`；负对照（临时加一个手拼请求、`Incarnation++` 的 Engine 出口）三处全部报出：[guard-negative.txt](evidence/sagadir/guard-negative.txt)。
- U-0280、U-0281、B1、NC-250 与 `step_operation_*` 全部既有用例**不改断言**通过（`-race -count=3` 全包；关键用例 `-race -count=50`）。

### ②

- `saga/step_operation_inbox.go`：原生收件箱的 claim / 守卫 / 判定原样移入 `stepOperationInbox`，另加 Mongo 步骤的生效点 `settleOwnClaim` 与 `errAttemptFenced`。
  `DataEngineStepInbox` 只剩原生特有部分（回执读法、`Bind`、`Replay`、`waitReplay`），`dataEngineClaim` 是 `stepClaim` 的别名（测试与持久格式不变）。
- `saga/command_consumer.go`：`MongoCommandInbox` 嵌入同一核心，`Handle` 改为 Reserve 事务 + 执行事务；`CommandInboxOptions` 增 `Owner`、`LeaseDuration`；
  `SubscribeMongoStep` 处理回放 / 在途 / 过期 / fence / 接替，过期分支补上“重发同一操作已生效的成功”。
- **先红后绿**（[mongo-step-red-green.txt](evidence/sagadir/mongo-step-red-green.txt)），`saga/mongo_step_operation_promises_test.go` 同一份用例跑 mongotest 与真实副本集：
  ```text
  committed attempt is replayed by the next attempt:
    operation gift-1:1:1 took effect 2 time(s), want 1: the business write ran for [gift-1:1:1:1 gift-1:1:1:2] although attempt gift-1:1:1:1 had already committed
  in-flight attempt past its deadline is taken over and cannot commit:
    attempt gift-1:1:1:2 ran while attempt gift-1:1:1:1 was still in flight with a live lease
    operation gift-1:1:1 took effect 2 time(s), want 1: attempt gift-1:1:1:1 committed after gift-1:1:1:2 took it over (k returned <nil>, ...)
  ```
  修后两边都绿：k+1 回放 k 的结果（CommandID 是 k 的）；在途时 k+1 不执行，k 截止后 k+1 接替执行，k 的事务以 `errAttemptFenced` 中止、不留回执。
- 消费者 `saga/mongo_step_consumer_promises_test.go`：回放发布、在途 nak、过期重发同一操作的成功、被接替 ack。
- **跨进程强杀实跑**（[cross-process-kill.txt](evidence/sagadir/cross-process-kill.txt)）：`TestRealSagaCrossProcessKillRecovers` 现在断言每个操作实例业务事务恰好提交一次。
  修后：60/60 完成，120 个操作、120 次提交、**0 个操作被多次提交**（N06 S5 第 2、3 次实跑各 1 个）；恢复 1m22.7s（仍由遗留事务锁主导）；
  存活进程 232 次“过了截止不执行”，期间出现 MaxTimeMSExpired、连接中断导致的提交结果未知，仍然恰好一次。

### O-S5-1

普通结果消费者与原生结果消费者共用 `isTerminalCompletionError`，并加入 `ErrIdentityConflict`。修前红（[o-s5-1-red.txt](evidence/sagadir/o-s5-1-red.txt)）：
`plain result stream = saga: not found (permanent=false) ...`、`... step is not waiting for a result (permanent=false) ...`、`... idempotency identity conflict (permanent=false) ...`；修后两条流都 Term。

### O-S5-3

按方案写进文档，不改默认步骤预算：SAGA.md「进程被强杀时遗留的 Mongo 事务」与 USER_GUIDE §7 建议 `transactionLifetimeLimitSeconds=20`，或按步骤调大 Mongo 步骤预算。
没有改生成的 compose 模板（开发环境不涉及强杀恢复时长；改它会动全部生成工程的 compose 快照）。

### 性能（[bench.txt](evidence/sagadir/bench.txt)，同机 Apple M5，顺序单协程，每次新命令）

| 环境 | 修前 | 修后 | 说明 |
| --- | --- | --- | --- |
| mongotest（2000 次 × 5） | 0.44 ms/op，8.2k allocs | 3.6 ms/op，63k allocs | 替身按集合快照、按操作查询是扫描，集合越大越慢，只作同口径对照 |
| 真实三节点副本集（500 次 × 6，交替） | 9.0 ms/op（8.8～9.2） | 17.4 ms/op（16.1～18.7） | 每次多一次事务提交（Reserve 事务），延迟约翻倍；吞吐随并发步骤扩展，未做并发压测 |

延迟代价来自多出的一次事务提交（多数派写），而不是守卫 upsert 与查询本身。（后续分析：时间分解、1 / 8 / 32 协程吞吐对照（吞吐同样约减半，受每秒提交数限制）与各优化候选见 [SAGA-MONGO-STEP-LATENCY-2026-10-06.md](SAGA-MONGO-STEP-LATENCY-2026-10-06.md)。）没有把 Reserve 与执行合成一个事务：那样“在途尝试”在别人看来不可见，
只能靠写冲突重试等待，无法接替，被杀进程遗留的事务还会连守卫一起锁住（见“实现”一节）。

### 验证（`GOWORK=off`）

- `gofmt -l` 空；`go build ./... && go vet ./...`；`go vet -tags integration ./saga/`；`go test -race -count=3 ./saga/... ./kit/saga/...`；
  `-race -count=20` 跑新增与 `TestNativeStep*`、`TestDataEngineStepInbox*`、`TestExpiredStepCommand*`、`TestMongoCommandInbox*`；根包 `go test -count=1 .`。
- `go test -count=1 ./codegen/...`；`go generate ./...` 后 porcelain 只有本次改动；生成 game-demo（replace 到 worktree）`go build ./... && go vet ./... && go test ./...`（18 个包通过）。
- 真实依赖（`~/.roost-it/roost-dataengine-it`，验收锁空闲，库 `roost_sagadir_*` / `roost_revn06s5_*`、流 `REVN06S5_*` 用后删除）：`-run '^TestRealMongo'` 全部 saga 真实 Mongo 用例、
  `TestRealSagaCrossProcessKillRecovers`、`scripts/test-dataengine-generated.sh` 通过。
- 未改 nest / entity / dataengine / sync：没有跑 glsvet。

### 兼容与未完成

- 持久格式只增（新集合 `<收件箱集合>_claims`）；wire、摘要、回执格式不变；公开 API 只增（`CommandInboxOptions.Owner` / `LeaseDuration`）。
- 行为变化：Mongo 步骤跨尝试回放 / 等待 / 接替；在途时同一命令的并发重投改为 nak（修前等对方提交后回放）；普通结果流对五种终态错误 Term。
- 混跑没有实跑（语义见 SAGA.md「Mongo 步骤」）。
- 真实 Mongo 上的并发吞吐对照没有做，只测了单协程延迟。
