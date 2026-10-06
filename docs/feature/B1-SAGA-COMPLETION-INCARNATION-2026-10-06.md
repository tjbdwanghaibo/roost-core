# B1：saga 协调器接收 completion 时核对代际（2026-10-06）

> **状态（2026-10-06 核对）**：已实施（`3fabe34d`），已随 v1.20.2 发布。下文“未发版”是实施当时的状态。

**来由**：维护者决定 B1（[DECISIONS-PENDING](../review/DECISIONS-PENDING-2026-10-05.md) 第二轮“协调器接收 completion 时核对代际：做”）。
U-0280 的执行契约见 [SAGA.md「原生步骤执行契约」](../../SAGA.md#原生步骤执行契约u-0280维护者-2026-10-05-决定)，
实施与复核见 [U-0280](../bugfix/U-0280-saga-step-reexecuted-after-crash.md)。
Mongo 步骤（`MongoCommandInbox`）的跨尝试幂等不在本方案内（DECISIONS-PENDING 已注明另写方案）。

**基线**：`origin/main` `3a71321c`。代码图谱的共享 generation 停在 09-30（早于 U-0280 的 saga 改动），本方案以当前源码为准。

## 1. 问题（独立审查的四个边角）

`Engine.Complete` 只要记录在 `Waiting` 且 `OperationKey == completion.IdempotencyKey` 就接收，同一操作任何一次尝试、任何一生的结果都算（`saga/engine.go` `Complete`）。
不核对代际，四个边角：

| # | 边角 | 修前后果 |
| --- | --- | --- |
| E1 | Resume 之后，上一生晚到的**拒绝**在新一生等待时到达 | 被接收，新一生直接失败 / 补偿；新一生的尝试照常执行并生效（收件箱不回放旧一生的拒绝），成功变成“放弃后迟到”——扣款不在 `CompletedSteps` 里 |
| E2 | 放弃后迟到的成功告警路径不写任何持久标记 | 同一个已生效的成功每送达一次（effect 重投、过期投递的回放、JetStream 重投）就告警、计数一次 |
| E3 | Resume 之后、新一生派发之前到达的旧一生成功 | 记录在 `Pending`，按 tombstone 告警一次；新一生派发后收件箱回放它又被接收、计入——告警是假的 |
| E4 | 补偿方向 `ManualRequired` 上调用人工 `Compensate` | 代际不变、`Attempt` 归零，重新派发的第一次尝试复用上一轮补偿第一次尝试的 `CommandID`；原生收件箱按摘要报 `ErrIdentityConflict`（截止时间不同）一直 nak，Mongo 收件箱回放旧结果，协调器按回执去重——补偿永远不会真正重新执行 |

## 2. 接收规则（协调器状态机）

协调器对每个操作（saga + 方向 + 步骤，即 `IdempotencyKey`）已经记着当前代际（`Record.Incarnation`）与当前等待的尝试（`Record.Attempt` / `CommandID`），
不新增记录字段。completion 的代际从 `CommandID` 解析：

- **选解析 `CommandID`，不加字段**：`CommandID` 由协调器铸造（`commandID(operationKey, incarnation, attempt)`：第 0 代 `key:attempt`，第 N 代 `key:rN:attempt`），
  每份 completion 都带着它（原生步骤在事务里 emit、Mongo 步骤回执、收件箱回放都原样携带）。新增 `Completion.Incarnation` 字段要求每个 handler 填写，
  已生成工程的 handler 不会填，缺省 0 会把新一生的结果误判为旧代际；还要改 wire / `completionDigest`。解析与铸造放在同一处（`engine.go` 的
  `commandID` / `commandIDIncarnation`），原生收件箱的 `commandIncarnation` 改为调用它（一行，保证两边对同一个 ID 得出同一个代际）。
  不是这个格式的 `CommandID`（测试或手工命令）按第 0 代处理，与收件箱一致。

`Complete` 的判定顺序：

1. **completion 的代际 ≠ 记录的代际**：
   - 失败（拒绝或可重试失败），或代际比记录还新（不可能由协调器产生）→ **不接收，只计数**：`Stats().StaleIncarnation`、
     `saga.completion.stale_incarnation_total{saga_type,phase}`，返回 `record, nil`（消费者 ack）。不写回执、不动 tombstone。
   - 成功，且记录**正停在这个操作上**（`Waiting` 且 `OperationKey` 相同；或 `Pending` / `Compensating` 且当前方向 + 步骤就是它，
     即 Resume 之后还没派发、或新一生的尝试在退避）→ 按契约第 3 条“成功在任何一生里都不重做”**接收为该操作的结果**：
     与同一生的成功走同一个 `applyCompletion`，写回执，`CloseOperation` 记“带结果关闭”（tombstone 由放弃升级为带结果），删掉这个操作仍排队的命令。
   - 成功但记录不在这个操作上 → 走第 3 步（重复 / 放弃后迟到）。
2. **同代际、记录在等这个操作**（`Waiting` 且 `OperationKey` 相同）：按现有规则接收（同一生较早尝试的成功、拒绝、可重试失败都接收，与收件箱“本生的拒绝回放”一致）。
3. **其余**：查回执与 tombstone（不变）。放弃关闭之后到达的成功告警，但**按（操作，completion 的代际）只告警一次**（第 3 节）。
   同代际的成功在本生退避期间到达仍是 `ErrNotWaiting`（下一次尝试由收件箱回放，U-0280 (b)，不变）。

逐条对照 U-0280 契约：

| 契约 | 与新规则的关系 |
| --- | --- |
| 1 至多一次的单位是操作实例，跨 Resume 同一实例 | 协调器把旧一生的成功当作这个实例的结果接收，与收件箱跨代际回放成功一致；不会因此多执行（新一生的尝试在收件箱里看到它就回放） |
| 2 尝试的生效窗口 ⊆ 协调器等它的窗口 | 不受影响：被接收的旧一生成功本身是在它那次尝试的窗口内投影的（否则被 fence 跳过，不会有 completion） |
| 3 收件箱：成功任何一生回放、拒绝只在本生回放、可重试失败放行 | **修前矛盾**：收件箱不回放旧一生的拒绝，协调器却接收它（E1）。修后两边同一张表：旧一生的拒绝 / 失败两边都不认，成功两边都认 |
| 4 放弃后迟到的成功只告警，不重开终态 | 不变；补“只告警一次”（E2）。记录已因 Resume 重新停在这个操作上时，旧一生的成功不再是“未计入”，直接接收（E3），所以第 4 条的“运维 Resume 后新一生回放”路径现在有两种到达顺序，结论相同 |
| 5 分工（Mongo 步骤不在契约内） | 不变。Mongo 步骤的旧一生成功同样被接收为结果（它的业务按 `IdempotencyKey` 幂等），旧一生的失败同样不被接收 |
| U-0280 复核 1（以失败关闭记为放弃） | 不变：只对同代际适用；旧代际的失败根本不被接收，也就不会以它关闭操作 |
| U-0280 复核 2（过期投递回放成功） | 不变：回放的成功若属于旧一生，按第 1 步接收或告警 |

没有发现规则之间的矛盾；唯一需要明确优先级的是“旧一生成功 × 放弃关闭的 tombstone”：记录停在这个操作上时以接收为准，不告警。

## 3. 告警去重

在 operation tombstone 上记已告警的代际：`late_alarms.r<代际>`（时间戳）。新增可选接口 `LateSuccessAlarmStore.MarkLateSuccessAlarm(ctx, completion, incarnation) (first bool, err error)`：
`MongoStore` 用一次条件更新 `{_id, saga_id, "late_alarms.r<N>": {$exists: false}}` + `$set`，`MatchedCount == 1` 才是第一次（单文档原子，真实 Mongo 并发下只有一个成功）。
第一次才记 ERROR 与计数，之后按重复计 `Duplicates`。没实现这个接口的 Store 照旧每次告警（不静默丢告警）。
不另写轻量回执：tombstone 已经是“这个操作被放弃”的持久事实、同一 TTL，标记放在它上面不引入新的集合与清理。

## 4. 人工 Compensate

补偿方向的 `ManualRequired`（补偿步骤拒绝或重试用尽）上调用 `Compensate` 时**进入新代际**（`Incarnation++`），与 `Resume` 一样：
新一生的 `CommandID` 与上一生不相交，收件箱不回放上一生的拒绝，补偿真正重新执行；上一生补偿若其实已生效（成功迟到），按契约第 3 条回放而不重做。
只在记录已经处于补偿方向时递增：从正向（Pending / 退避中）发起的补偿在这一生里还没派发过补偿命令，不会复用 ID，保持现有 `CommandID` 不变。

**正确做法写进 SAGA.md**：补偿方向 `ManualRequired` 修复原因后用 `Resume`（推荐，它同时处理截止时间）；`Compensate` 在这种状态下与 `Resume` 等价。
`Compensate` 的本意是“中止正向、开始补偿”。

## 5. 持久格式与混跑

- **只增不改**：`_saga_operations` tombstone 多一个子文档字段 `late_alarms`（`r<代际>: 时间`）。记录、回执、命令、wire、摘要都不变；
  `Record.Incarnation` 已持久（NC-38）。
- **旧数据缺字段**：旧 tombstone 没有 `late_alarms`，第一次迟到成功照常告警并补上标记；U-0280 之前的 tombstone 没有 `closure`，仍按“未知”不告警（不变）。
- **混跑**：
  - 旧协调器仍按 `IdempotencyKey` 接收任一代际的结果（E1 / E3 在旧进程处理的 completion 上仍会发生），不读写 `late_alarms`（每次告警）。
    新协调器的接收规则只在它处理的 completion 上成立；全部协调器升级后才是不变量。新旧都按版本号 fence 记录，不会互相覆盖。
  - 旧协调器上的人工 `Compensate` 仍复用 `CommandID`；新协调器递增代际后旧进程读到的记录照常工作（代际是已有字段）。
  - 收件箱、消费者行为不变（只把代际解析换成共用函数）。

## 6. 改动面

- `saga/engine.go`：`Complete` 判定顺序、`Compensate` 递增代际、`commandIDIncarnation`、stale 计数与告警去重。
- `saga/store.go`：`LateSuccessAlarmStore` 可选接口。
- `saga/mongo_store.go`：`MarkLateSuccessAlarm`、`operationDoc.LateAlarms`。
- `saga/dataengine_step_inbox.go`：`commandIncarnation` 改调 `commandIDIncarnation`（理由见第 2 节，行为不变）。（更正注，2026-10-06：`9669d181` 之后
  `commandIncarnation` 在 `saga/step_operation_inbox.go`，main `37338490` 时 `:492`；两种收件箱共用。）
- `command_consumer.go`、kit/saga 不改。
- 文档：SAGA.md 契约第 3 / 4 条与“失败语义”、本方案、U-0280 记录追加“B1 实施”、TROUBLESHOOTING T-226、CHANGELOG、DECISIONS-PENDING。

## 7. 验证

- 先红后绿：四个边角各一条确定性用例（`saga/step_operation_incarnation_promises_test.go`），跑在 `nativeWorld`（真实 `SubscribeDataEngineStep`
  消费者 + mongotest 上真实 `DataEngineStepInbox` + 真实 dataengine `MongoStore.Project` 投影器），手动推进时钟，不用 sleep。
- MongoStore 上的标记（mongotest）；真实 Mongo 上并发标记只有一个第一次（`-tags integration -run TestRealMongoLateSuccessAlarm`）。
- U-0280、U-0281、`saga/step_operation_*_test.go` 全部既有用例；`-race -count=200` 跑新增与相关用例；saga / kit/saga `-race -count=3`；根包、build、vet。

## 8. 实施状态

**已实施，未发版**（`3fabe34d`）。改动面与第 6 节一致；`command_consumer.go`、kit/saga 未改，原生收件箱只把代际解析换成共用函数。

**先红后绿**（基线 `3a71321c`，全文 [red-before.txt](../bugfix/evidence/B1/red-before.txt)）：E1 拒绝被新一生接收、saga Failed；E2 一个迟到成功告警 3 次；
E3 Resume 后到达的旧成功告警 1 次且 saga 停在第 0 步；E4 人工 Compensate 复用 `gift-4:2:0:1`，投递报 `idempotency identity conflict`。修后全绿。
另加：`TestMongoStoreMarksALateSuccessAlarmOncePerLife`（MongoStore 上 3 次送达 = 1 次告警 + 2 次重复；按代际标记、旧 tombstone 补标记、他人 saga 不标记；
旧一生拒绝在 MongoStore 上不接收、旧一生成功接收）、`TestCommandIDIncarnationInvertsCommandID`、真实 Mongo `TestRealMongoLateSuccessAlarmIsMarkedOnce`
（20 轮 × 8 个并发送达，每轮恰好 1 个 first）。

**验证**（`GOWORK=off`）：
- `gofmt -l` 空；`go build ./... && go vet ./...`；`go vet -tags integration ./saga/`；根包 `go test -count=1 .` 通过。
- `go test -race -count=3 ./saga/... ./kit/saga/...` 通过（含 U-0280、U-0281 与 `saga/step_operation_*_test.go` 全部既有用例，未改动断言）。
- `-race -count=200`：新增用例与 `TestNativeStep*`、`TestMongoStoreTombstone*`、`TestDataEngineStepInbox*`、`TestExpiredStepCommand*`（U-0281）、
  `TestMongoResume*` / `TestMongoIncarnation*` 通过。
- 真实 Mongo（`~/.roost-it/roost-dataengine-it`，验收锁空闲，库 `roost_u0280_<pid>_<ns>` 用后删除）：
  `go test -tags integration -run 'TestRealMongo(LateSuccessAlarmIsMarkedOnce|ConcurrentAttempts|SupersedeAndProjection)' ./saga/` 通过。
- 未改 nest / entity / dataengine / sync、未改生成形状：没有跑 glsvet、codegen、生成工程（按影响面）。

**兼容**：公开 API 只增（`Stats.StaleIncarnation`、`LateSuccessAlarmStore`、`MongoStore.MarkLateSuccessAlarm`）；新指标 `saga.completion.stale_incarnation_total{saga_type,phase}`。
行为变化：旧一生的拒绝 / 失败不再改变新一生（以前会让它失败或消耗一次重试）；旧一生成功在 Resume 后可直接推进 saga；同一迟到成功只告警一次，其余计 `Duplicates`；
补偿方向 `ManualRequired` 上的 `Compensate` 递增 `Incarnation`、新的 `CommandID` 带 `:rN:`。

**未验证 / 风险**：
- 混跑（新旧协调器并存）没有实跑，语义按第 5 节说明。
- 没有做真实 Mongo 上去掉 `$exists` 条件的负对照（条件更新的原子性是服务端单文档语义，mongotest 用例已覆盖逻辑）。
- Mongo 步骤的跨尝试幂等仍不在框架契约内（DECISIONS-PENDING B1 另写方案）。

**方向判断**（roost-coding“反复出问题要上报”）：saga 协调器 / 收件箱近 10 天的修复链是 U-0280（A + B + C'，`054fdd66`）→ U-0280 复核两处（`23b97942`、`877bb66c`）
→ 本次 B1 四处，都落在同一个不变量“每个操作实例的结果恰好被计入一次、不被计入的有人知道”上，属于同一不变量被反复打破的信号。
根因是 U-0280 方案记录里点出的“身份 / 时间 / 最终性三套没对齐”：收件箱已按操作实例 + 代际判断，协调器还按操作判断。B1 把协调器对齐到同一张表，
没有增加记录状态（只用已有的 `Incarnation` 与一个 tombstone 子字段），属于“减少分叉”而不是“再加分支”。剩下没对齐的是 Mongo 步骤（仍按命令）与
“放弃后迟到成功”只告警不自动补偿（C 未做）；若之后再在这两处出缺陷，建议按 U-0280 记录的候选方向把 Mongo 步骤纳入同一收件箱契约，或做 C（只补偿迟到生效的那一步），
而不是继续在协调器上加特判。
