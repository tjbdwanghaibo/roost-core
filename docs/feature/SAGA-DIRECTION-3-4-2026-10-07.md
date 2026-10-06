# saga 方向 ③④：只接收正在等的那次尝试的可重试失败；放弃后迟到生效的正向步骤自动补偿（2026-10-07）

**来由**：第六轮维护者决定“saga 方向：按推荐 ①②，③④ 暂不做”（[DECISIONS-PENDING](../review/DECISIONS-PENDING-2026-10-05.md) 约 149 行）；
第十三轮维护者：“还有留到下个版本的几项在本机能完成吗？希望本次能完成了”（同表“原‘下个大版本’项”行），saga 方向 ③④ 本版完成。
**基线**：`origin/main` `66d72a33`，分支 `saga34`。代码图谱共享 generation 停在 09-30，`saga/` 在它之后改过多次（`a95cf4dc`、`5ca32611`、`a013f9ff` 等），本方案以当前源码为准。

## 1. ③④ 的原文与出处

[N06 S5 review](../review/REVIEW-2026-10-06-n06s5.md)“方向判断”（第 119～133 行）列了四条，第六轮按编号决定：

> 3. **只接收正在等的那次尝试的失败**（O-S5-7）：去掉“较早尝试晚到的失败让协调器在最后一次尝试执行中放弃”这一告警来源；改协调器接收规则。
> 4. 方向 C（只补偿迟到生效的那一步）仍未做；若 2、3 落地，告警来源只剩“effect 发布延迟”与时钟偏差，C 的必要性下降。

- ③ 对应观察 O-S5-7（同文第 60、117 行）：“同一生较早尝试晚到的失败被接收，可能放弃仍在执行的最后一次尝试（告警兜底）”。
  方向 ② 的实施报告也写过“如果之后又在这里出问题，先考虑 ③”。
- ④ 的“方向 C”定义在 [U-0280 记录](../bugfix/U-0280-saga-step-reexecuted-after-crash.md) 候选表：
  > **C. 协调器处理“放弃后迟到的成功”**：tombstone 记录关闭方式（带结果关闭 / 放弃关闭）；放弃后到达的正向成功不再计 duplicate，而是把 saga 重新带入补偿、只补偿该步。
  > 代价：协调器状态机改变：终态 Failed / Compensated 可能被重开；需要“只补偿第 s 步”的新状态；Store 接口 / 持久格式（operationDoc 加字段）变化；影响所有 saga 使用方。

  当时维护者选了 C'（只告警，`054fdd66`），C “另行决定”；U-0280 记录给维护者的第 3 问是“C 是否允许重开 Failed / Compensated”。
  [B1 方案](B1-SAGA-COMPLETION-INCARNATION-2026-10-06.md) 第 8 节方向判断再次点名 C。

## 2. 对照当前代码：是否仍成立

**③ 仍成立，未被后续改动覆盖。** `Engine.Complete`（`saga/engine.go`）同一生、记录在等这个操作时接收“同一生较早尝试的成功、拒绝、可重试失败”。
单状态文档（`a013f9ff`）只改了收件箱的存储形状，协调器接收规则没变；SAGA.md 契约第 4 条仍把它列为告警来源，
`TestNativeStepSuccessAfterAFailureClosedOperationIsAlarmed`（`saga/step_operation_review_test.go`）把现状钉成“接收、Failed、告警”。

时序（2 次尝试）：k 在截止前投影了一次可重试失败，它的 completion effect 还在路上；k 超时 → 退避 → 派发最后一次尝试 k+1；k 的失败此时到达，
协调器按“在等这个操作”接收，`retryState` 用的是记录上的 `Attempt`（= k+1）→ 重试用尽 → 补偿 / Failed；k+1 在收件箱里看到 k 只是可重试失败，照常执行并生效。

**④ 仍成立。** 契约第 4 条“放弃之后迟到的成功只告警，不重开终态、不自动补偿”。在当前代码上核对剩余来源：

- 协调器只在 `NextRunAt`（= 这次尝试的命令截止）之后才认领 Waiting 记录（`MongoStore.ClaimDue` 的 `next_run_at <= now`），截止 / 超时用尽 / 定义缺失放弃
  一个在等的操作都发生在它的截止之后；租约封顶到命令截止（方向 ② 后 Mongo 步骤同样），所以放弃之后不会再有尝试**生效**；
- 剩下的是**送达**晚于放弃：最后一次尝试在截止前生效，completion 在协调器放弃之后才到（outbox 发布 / JetStream 积压、过期投递的回放）；③ 落地前还有较早尝试晚到的失败；以及时钟偏差。
- 送达晚到时，第 0 步放弃 → `Failed`（终态），后面的步骤放弃 → 补偿前缀 → `Compensated`（终态）。所以 C 不能只处理非终态，“重开 Failed / Compensated”是 C 的必要部分。

## 3. ③：可重试失败只接收正在等的那次尝试的

**规则**：同一生、记录在等这个操作（Waiting 且 `OperationKey` 相同）时：

| completion | 来自正在等的尝试（`CommandID == Record.CommandID`） | 来自同一生较早的尝试 |
| --- | --- | --- |
| 成功 | 接收 | 接收（不变：成功是操作的结论，收件箱也回放它） |
| 拒绝（`Retryable=false`） | 接收 | **接收**（见下） |
| 可重试失败 | 接收 | **不接收**：计 `Stats().StaleAttempt` 与 `saga.completion.stale_attempt_total{saga_type,phase}`，记 WARN，返回 nil（消费者 ack） |

原文写“只接收正在等的那次尝试的失败”。拒绝不能按字面一并拒收：收件箱把同一生的拒绝当作操作的结论（契约第 3 条“本生的拒绝 → 回放，不执行”），
下一次尝试的投递会把**原来那次的** completion（`CommandID` 是被拒的那次）经结果流重发（`command_consumer.go` 的 `reservation.Completion.CommandID != command.ID` 分支）。
按字面拒收，回放永远不被接收，协调器每次尝试都等到超时，用尽 `MaxAttempts` 才补偿，拒绝原因也丢了。按收件箱的表分开才对得上：

- **操作的结论**（成功、本生的拒绝）：收件箱不让之后的尝试执行，协调器从任何一次尝试接收都安全；
- **尝试的结论**（可重试失败）：只说明那一次没生效，之后的尝试照常执行，只有正在等的那一次的才能推进记录。

这与 B1“协调器与收件箱用同一张表”一致，协调器不加状态。

**论证（接收拒绝不会放弃正在执行的尝试）**：k 的拒绝写进状态文档必须在 k 的租约内（生效点条件写），租约封顶到 k 的截止；
协调器在 k 截止之后才派发 k+1；k+1 Reserve 时 k 要么已结算（拒绝在 `refusals` 里 → 回放、不执行），要么 pending 且租约过期 → 接替（k 的生效点从此不匹配，拒绝不会落库）。
两种情况下“k 的拒绝已送达”与“k+1 在执行”不会同时成立（时钟偏差除外，与契约第 2 条同一前提）。

**影响**：失败关闭仍记为“放弃关闭”。③ 之后以失败关闭时不再有别的尝试能生效（时钟偏差除外），保留放弃关闭让这种异常仍能被发现（④ 会补偿它）。

## 4. ④：放弃后迟到生效的正向步骤只补偿这一步（方向 C）

### 4.1 规则

协调器收到一份“放弃关闭之后才到的成功”（tombstone 是放弃关闭、这个 `CommandID` 没有回执，判定与现在相同）时：

- **补偿方向的操作**：维持现状，只告警（ERROR + `late_after_abandon_total{phase="compensate"}`）。补偿方向的放弃都落在 `ManualRequired`，
  运维 `Resume` 后新一生派发同一补偿，收件箱回放这次成功（任何一生的成功都回放），不需要协调器另做。
- **正向操作（第 s 步）**：步骤已经生效、却不在 `CompletedSteps` 里。协调器在**同一个 Store 事务**里：记下这份 completion 的回执、把 tombstone 改为带结果关闭
  （都是 `stepTransition` 收到成功回执时已有的行为）、在记录上写 `LateStep = s+1` 与 `LateData = completion.Data`，并按记录所处位置：

  | 记录位置 | 处理 |
  | --- | --- |
  | `Failed`（只可能是第 0 步被放弃）、`Compensated`、`Compensating` 且还没派发下一个补偿（`Attempt == 0`） | **立即进入补偿第 s 步**：`Status=Compensating`、`Phase=Compensate`、`Step=s`、`NextRunAt=now` |
  | 某个补偿正在等结果（Waiting）或在重试退避（`Compensating`、`Attempt > 0`） | 只记 `LateStep`；那个补偿接收结果后，下一个补偿先补第 s 步 |
  | `ManualRequired`（补偿失败，或正向定义缺失） | 只记 `LateStep`；人工 `Resume` / `Compensate` 时先补第 s 步（运维的记录不自动跑） |

  补偿第 s 步的命令 `Payload` 是 `LateData`（这一步正向成功的 Data，与正常流程里“补偿最后完成的一步”拿到的 Data 相同）；
  它的成功结果**不进入** `Data` 链（之后补偿第 j<s 步拿到的仍是原链上的 Data），成功后清掉 `LateStep` / `LateData`，回到原来的倒序：
  `CompletedSteps > 0` 就补偿 `CompletedSteps-1`，否则 `Compensated`。补偿第 s 步重试用尽或被拒 → `ManualRequired`（与其他补偿相同），`LateStep` 保留，`Resume` 再补它。
- 同一个成功之后的送达：回执已在 → 计 `Duplicates`。`late_after_abandon_total` 仍按（操作，代际）计一次（由回执保证），标签 `phase="forward"` 表示已自动补偿，日志降为 WARN。
- 防御：记录在正向非终态、`LateStep` 已是另一步、或 s < `CompletedSteps`（都不应发生，见 4.3）时退回只告警（ERROR）。

“重开 Failed / Compensated”是 C 的定义里写明的代价（U-0280 候选表），维护者选 ④ 即选 C；第 2 节说明只处理非终态的 C 覆盖不到第 0 步与补偿已结束的情形。
重开只发生在“有一步真实生效却没被补偿”时，结果仍回到 `Compensated`。`Failed` 被重开后最终是 `Compensated`（它确实有一步生效、又被撤销了）。

### 4.2 状态与持久格式

`Record` 增两个字段（`MongoStore` 的 `late_step`、`late_data`，`omitempty`，正常记录不多写字节）：

- `LateStep int`：放弃之后才生效、还没补偿的正向步骤号 + 1；0 表示没有；
- `LateData []byte`：那一步正向成功的 `Data`，补偿它的命令载荷。

`Record.Validate` 新增：`LateStep >= 0`；`LateStep == 0` 时 `LateData` 为空；`LateStep > 0` 时 `LateStep-1 >= CompletedSteps` 且（`Phase == Compensate` 或 `ManualRequired`）；
`Compensating` 要求 `CompletedSteps > 0` 或 `LateStep > 0`；`Compensated` / `Failed` / `Completed` 要求 `LateStep == 0`。
没有新状态：“正在补偿迟到那一步”就是 `Phase == Compensate && Step == LateStep-1`。

选下一个补偿只有一处（`nextCompensation`）：`LateStep > 0` → 补它；否则 `CompletedSteps > 0` → 补 `CompletedSteps-1`；否则 `Compensated`。
开始补偿（`compensationState`，截止 / 用尽 / 拒绝 / 人工 Compensate）、补偿成功（`applyCompletion`）、`Resume`、迟到成功到达都调它；
`Compensate` 在 `CompletedSteps == 0 && LateStep == 0` 时仍拒绝。新转移原因 `causeLateSuccess` 仍经 `stepTransition`（守卫测试不变）。

Store 接口不变（`ApplyRequest` 不加字段）：回执、tombstone 改带结果关闭、记录字段在 `Apply` 的同一事务里，自定义 Store 只需持久化两个新字段。

### 4.3 契约论证

- **至多一次计入**：迟到成功只在“没有回执、tombstone 是放弃关闭”时处理，处理的事务同时写回执与带结果关闭；并发送达同一份成功时 `Apply` 按回执去重（`ApplyDuplicate`），
  只有一次能写 `LateStep`，第 s 步的补偿只派发一次。
- **只有一个迟到步骤**：正向一次只有一个操作开着；放弃它就离开正向（补偿 / Failed / ManualRequired）。回到正向只有 `Resume` 一条路（`Failed` 或正向 `ManualRequired`、`CompletedSteps == 0`），
  那时记录停在同一个操作上，旧一生的成功按 B1 规则 2 直接接收为结果，不是迟到。所以 `LateStep` 一个字段够用，`s >= CompletedSteps` 恒成立。
- **补偿命令身份**：补偿第 s 步的 `IdempotencyKey` 是 `<saga>:2:s`，正常倒序里第 s 步不在 `CompletedSteps` 内，同一生里没派发过这个操作，`CommandID` 不会与旧回执相撞；
  `ManualRequired` 之后的 `Resume` 换代，`CommandID` 带 `:rN:`。
- **顺序**：第 s 步在“下一个边界”补偿。迟到成功到达时若第 j 步的补偿已在途，j 先结束，s 在 j 之后补——这偏离了严格倒序，但在途的补偿不能放弃（它可能生效）；
  正常业务按 `IdempotencyKey` 设计的资源状态（`reserved -> released`）不依赖跨步骤的补偿顺序。写进 SAGA.md。
- **与收件箱的关系**：收件箱不变。补偿第 s 步是一个普通的补偿操作（同一张表、同一套租约）。

## 5. 延迟与吞吐

- ③：`Complete` 多一次字符串比较，无新 I/O。
- ④：正常路径只多两个 `omitempty` 字段与 `Validate` 里几次整数比较，`Apply` / `ClaimDue` / 收件箱 / 投影都不变；迟到路径是一次 `Apply` 事务（与接收一次结果相同）加一次正常补偿。
- 不在热路径上，不做 benchstat（共同要求“验证按影响面来”）。

## 6. 兼容

线上未部署，不做兼容（维护者 2026-10-06）。公开 API 只增：`Record.LateStep` / `LateData`、`Stats.StaleAttempt`、指标 `saga.completion.stale_attempt_total`。
行为变化：同一生较早尝试晚到的可重试失败不再推进记录；放弃后迟到的正向成功不再只告警，而是补偿那一步，`Failed` / `Compensated` 可能被重开回 `Compensating`。
`saga.completion.late_after_abandon_total` 含义不变（放弃之后才到的成功，按操作与代际计一次），`phase="forward"` 的已自动补偿。
生成模板、game-demo 不涉及（模板不引用这些规则；GM 查询按状态过滤不受影响）。

## 7. 测试计划

先红后绿（确定性，`nativeWorld` 真实原生收件箱 + 投影 + 手动时钟，或内存 Store 引擎），修前跑红并抄失败文本：

- ③ `TestNativeStepStaleAttemptRetryableFailureDoesNotAbandonTheLastAttempt`：上节时序，断言 k 的可重试失败不改变记录、k+1 的成功被接收、saga 前进、无迟到告警、`StaleAttempt == 1`。
  它替换 `TestNativeStepSuccessAfterAFailureClosedOperationIsAlarmed`（那条把被 ③ 取消的行为钉成承诺，按决定改写）。
- ③ 守卫：较早尝试的拒绝经下一次尝试回放（`CommandID` 是较早那次）仍被接收、立即补偿（不等超时）。
- ④ `Failed` 重开：第 0 步最后一次尝试截止前生效、completion 在 Failed 之后到达 → 补偿第 0 步（载荷是正向成功的 Data）→ `Compensated`；重复送达只计重复。
- ④ `Compensated` 重开只补迟到那一步：第 1 步放弃 → 补偿第 0 步 → `Compensated` → 第 1 步迟到成功 → 只派发第 1 步补偿 → `Compensated`。
- ④ 补偿在途：第 0 步补偿 Waiting 时第 1 步迟到成功 → 只记 `LateStep`；第 0 步补偿成功后补第 1 步；第 0 步的 Data 链不被第 1 步补偿结果覆盖。
- ④ `ManualRequired` 保持：补偿被拒后迟到成功只记录；`Resume` 先补迟到那一步再补前缀。
- ④ 补偿方向的迟到成功仍只告警。
- 原有契约测试原样通过：U-0280、U-0281、B1、方向 ①②、单状态文档、`TestEveryCoordinatorWriteGoesThroughStepTransition`、跨进程强杀、`TestRealMongo*`。
  断言被 ③④ 推翻的既有用例单列改动与理由（实施状态）。
- MongoStore：两个新字段往返（mongotest）；私有三节点副本集上 `-tags integration -run '^TestRealMongo'` 与跨进程强杀。

## 实施状态

**已实施，未发版**（分支 `saga34`，`a6a902cd`）。无新 RR：③④ 是方向变更，实施中没有发现缺陷。

### 改动

- `saga/engine.go`：`Complete` 同一生分支加 ③ 的判断（`reportStaleAttempt`，`Stats.StaleAttempt`）；`completeNotWaiting` 的迟到正向成功交给
  `compensateLateStep`（`lateForwardStep` 判定范围），写记录冲突时由 `Complete` 重读重试；`nextCompensation` 是选下一个补偿的唯一一处，
  `compensationState`、`applyCompletion` 的补偿成功分支、`Resume`、迟到成功到达都调它；`processClaimed` 补偿迟到那一步时载荷取 `LateData`；
  `Compensate` 在 `LateStep > 0` 时也允许；`parseOperationKey` 是 `operationKey` 的逆。
- `saga/step_transition.go`：新原因 `causeLateSuccess`（关闭规则不变：成功回执关闭它所属的操作，tombstone 由放弃改带结果）。
- `saga/record.go`：`Record.LateStep` / `LateData`、`Clone`、`Validate`（`lateStepValid`）。`saga/mongo_store.go`：`late_step` / `late_data`。
- `saga/store.go` 注释；文档 SAGA.md 契约第 3、4 条与持久格式、USER_GUIDE、kit/README、TROUBLESHOOTING T-226、CHANGELOG、交接 §7。

### 先红后绿

基线 `66d72a33` 上新用例（`saga/saga_direction_3_4_promises_test.go`）的失败文本全文在 [red-before.txt](evidence/saga34/red-before.txt)，摘要：

- ③ `TestNativeStepStaleAttemptRetryableFailureDoesNotAbandonTheLastAttempt`：`a retryable failure of earlier attempt gift-1:1:0:1 moved the record while the coordinator waits for gift-1:1:0:2: status=failed`；
- ④ `...ReopensAFailedSaga...`：`took effect after the saga failed, but the saga was not brought back to compensate it: status=failed`；
  `...AfterCompensatedCompensatesOnlyThatStep`：`the compensated saga was not reopened`；`...DuringAnInFlightCompensation...`：`status=compensated step=0 completed=0`（第 1 步从未补偿）；
  `...OnManualRequiredIsCompensatedOnResume`：Resume 后派发的是第 0 步的补偿；
- 守卫（修前修后都绿）：`TestNativeStepReplayedRefusalOfAnEarlierAttemptIsAccepted`（③ 不能把回放的拒绝当成旧尝试丢掉）、`TestNativeStepLateCompensationSuccessIsOnlyAlarmed`（④ 只管正向）。
- 新 API 用例（修后加入）：`TestMongoStoreIgnoresARetryableFailureOfAnEarlierAttempt`、`TestMongoStoreLateStepCompensationRoundTrip`（mongotest，`late_step` / `late_data` 往返、回执与转移同一事务、补偿命令载荷）。

### 断言被 ③④ 推翻的既有用例（按决定改写，其余断言不动）

| 用例 | 原断言 | 改为 |
| --- | --- | --- |
| `TestNativeStepSuccessAfterAFailureClosedOperationIsAlarmed`（U-0280 复核） | 较早尝试晚到的可重试失败被接收、saga Failed、迟到成功告警 | 删除，同一时序改写成 ③ 的红绿用例（文件里留了指向说明） |
| `TestNativeStepTakesEffectAtMostOncePerOperation/c'`（U-0280） | 迟到成功后 saga 仍 `Failed` | `Compensating`、只补第 0 步；告警计数 1、生效 1 次不变 |
| `TestNativeStepOperationInterleavingsWithCoordinatorDecisions`：Resume 回放 / saga 截止两个子用例（S5） | 迟到成功后仍 `Failed`，运维 Resume 让新一生回放、继续向前 | 协调器自己补偿这一步（`Compensating` 不能 Resume），最终 `Compensated`，handler 只执行 1 次；截止子用例告警计数 1 不变 |
| `TestCoordinatorChecksTheIncarnationOfACompletion/E2`（B1） | 三次送达后 saga 仍 `Failed` | `Compensating`、`LateStep=1`；“三次送达只计一次”不变 |

其余 U-0280、U-0281、B1、方向 ①②（含 `TestEveryCoordinatorWriteGoesThroughStepTransition`）、单状态文档、O-S5-1、NC-250 用例原样通过。

- 新增真实 Mongo 用例 `TestRealMongoLateSuccessReopensACompensatedSagaOnce`（`saga/late_step_real_mongo_integration_test.go`）：同一迟到成功 8 路并发送达 × 10 轮，
  每轮恰好一次把 `Compensated` 带回补偿第 1 步（版本只加一、回执与 tombstone 带结果关闭同一事务、`late_step` / `late_data` 经服务端往返）。

### 验证（`GOWORK=off`）

- `gofmt -l` 空；`go vet ./saga/... ./kit/saga/...` 与 `go vet -tags integration ./saga/... ./kit/saga/...`；`go build ./... && go vet ./...` 通过。
- `go test -race -count=3 ./saga/... ./kit/saga/...` 通过；新增与改写的用例（及 B1 / U-0280 交错用例）`-race -count=50` 通过。
- 私有三节点副本集（`ROOST_MIRROR_LOCAL_HOME=<scratchpad>/saga34-env ROOST_MIRROR_LOCAL_OFFSET=26900 scripts/mirror-local.sh up`，用完 `clean`，未碰共享环境）：
  `go test -tags integration -count=1 ./saga/` 全部通过（10 个 `TestRealMongo*`、真实 NATS 退避、协调器租约接管、多进程）；跨进程强杀
  `TestRealSagaCrossProcessKillRecovers`：60/60 完成、120 个操作、120 次提交、**0 个操作被多次提交**、恢复 1m10s（仍由遗留事务锁主导）；
  新增 `TestRealMongoLateSuccessReopensACompensatedSagaOnce` `-count=3` 通过。摘要 [integration.txt](evidence/saga34/integration.txt)。
- `scripts/test-dataengine-generated.sh`（同一私有环境）通过；根包 `go test -count=1 .` 通过。
- 未改 nest / entity / dataengine / sync 与生成模板：没有跑 glsvet、codegen 测试、没有重新生成 game-demo（按影响面）。
- 不在热路径（第 5 节），没有做 benchstat。

### 未完成 / 风险

- 顺序：迟到的那一步可能排在一个已在途的补偿之后补偿（第 4.3 节），依赖业务补偿不要求跨步骤严格倒序；SAGA.md 已写明。
- “重开 Failed / Compensated”按 C 的定义实施（第 4.1 节）；若维护者要求终态不可重开，可把第 4.1 表第一行改为“只记 `LateStep`、`Resume` 时补偿”，其余不变。
- 时钟偏差仍是契约前提（与契约第 2 条相同），不在本次范围。
