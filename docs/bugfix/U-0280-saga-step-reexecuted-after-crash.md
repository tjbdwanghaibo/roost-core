# U-0280：kill -9 / 失锁后，原生 saga 步骤以新的尝试再执行一次（道具复制或丢失）

**仓库 / 位置**：roost-core `saga/dataengine_step_inbox.go`（claim / 回执按 CommandID）、`saga/engine.go`（命令截止时间、`Complete`、`closedOperation`）、
`saga/mongo_store.go` `CompletionRecorded`。
**来源**：真实进程演练 drill6（2026-10-05，代码 `47a9132c`，生成 game-demo，赠礼 saga）。维护者授权按先红后绿修；10-05 追加指示：
**修法若要改设计、公开语义或协调器状态机，写完方案就停下交回**。
**状态**：**已修复，未发版**（维护者 2026-10-05 决定按推荐 A + B + C' 实施，并要求重试次数做成配置；见文末「实施」）。
修前的方案讨论保留在下面，原文不改。**定位文档**：TROUBLESHOOTING T-226。
同一演练的另一缺陷 [U-0281](U-0281-saga-expired-command-nak-forever.md) 已修复。

## 现象（演练证据）

`evidence/U-0280/drill6-duplicate-steps-summary.txt`（按 `_dataengine_receipts` 里每个 saga 的步骤回执数统计；演练库已按清单删除）：

| 窗口 | saga 数 | 状态（4=completed 5=compensated 6=failed） | debit 回执 >1 | 退款回执 >1 | Failed 却有 debit 回执 |
| --- | --- | --- | --- | --- | --- |
| 0～6c（SIGSTOP / SIGTERM 接管） | 124 | 64 / 60 / 0 | 0 | 2 | 0 |
| 6b-A（kill -9，立即重启） | 65 | 35 / 30 / 0 | 0 | 0 | 0 |
| 6b-A2（kill -9，立即重启） | 73 | 18 / 11 / 44 | 1 | 0 | 4 |
| 6b-A3（kill -9，立即重启） | 20 | 10 / 10 / 0 | 3 | 2 | 0 |
| 6b-B（kill -9，60s 后重启） | 20 | 10 / 9 / 1 | 3 | 2 | 1 |

同一步骤同一方向出现两个尝试号的回执，背包数对得上：重复扣款（saga completed）、重复退款（compensated，道具复制）、
已扣款却 Failed（道具丢失）。saga 状态照常终结，协调器只把迟到的结果计为 `duplicates`，没有告警。

## 根因（核实，指到行）

演练 agent 的分析成立。三处事实组合：

1. **回执与 claim 按 CommandID**：`saga/dataengine_step_inbox.go:282` `readReceipt` 查 `saga-step/<CommandID>`；`reserveInTransaction:190` claim 也是
   `saga-step/<CommandID>`。CommandID = `operationKey:attempt`（`engine.go:790` `commandID`），每次尝试都不同，所以尝试 k+1 看不到尝试 k 的 claim / 回执。
2. **命令截止时间只有一次尝试的 Timeout**：`engine.go:635` `DeadlineAt: after.NextRunAt`（`NextRunAt = now + step.Timeout`，模板 5s）；
   而 claim 租约是 `LeaseDuration`（模板 2 分钟，`reserveInTransaction:196`），投影的 fence 只看 claim 租约（`dataengine/lease_fence.go:75` `Predicate`），
   **不看命令截止时间**。所以已写进 WAL 的尝试 k 在 5s 截止后、2 分钟租约内重放时仍会投影生效。
3. **协调器对迟到结果的处理**：`engine.go:392` `Complete` 只在 `StatusWaiting && OperationKey == IdempotencyKey` 时接收（同一步骤同一方向的任一尝试都算）；
   否则 `CompletionRecorded`：退避期间（pending）没有 tombstone → `ErrNotWaiting`，`nest_completion_consumer.go:137` 作为 Permanent 丢弃；
   操作已关闭（进入下一步、补偿或 Failed，`closedOperation:805`）→ `mongo_store.go:162` 查到 tombstone 就当作已记录 → 计 `duplicates`，静默吞掉。

kill -9 时序：发送方进程 A 把尝试 k 的 WAL 记录（扣款 + 回执 + completion effect）提交后被杀；新进程 B 要等单实例锁（~15s）；
协调器 5s 后判 k 超时，发出 k+1、k+2……（投到别的 sid 被 `Admit` 拒绝，过期）；B 启动重放 WAL，k 的 claim 租约仍有效，投影生效。之后：

- (a) k 的 completion 在协调器等 k+j 时到达 → 被接收，saga 前进；k+j 的命令随后在 B 上 Reserve 到新 claim → **再扣一次**，它的 completion 撞 tombstone 被吞；
- (b) k 的 completion 在退避期间到达 → `ErrNotWaiting` 丢弃；k+j 执行 → **再扣一次**；
- (c) k 的 completion 在协调器放弃（重试用尽或 saga 截止）之后到达 → tombstone 吞掉；**已扣款、saga Failed**，什么也不补偿。

演练日志里的 `saga: step is not waiting for a result`（P10 / Q1 / Q3 / P7）就是 (b)。

不只是崩溃：**投影积压超过步骤 Timeout** 时同一进程上也成立——k 已准入未投影，k+1 被实体屏障（`ErrFencedEntityPending`）挡到 k 投影完成，
随后 k+1 拿到自己的新 claim 再执行一次。

### 确定性复现（修前红，未提交）

`evidence/U-0280/u0280_repro_test.go.txt`（放进 `saga/` 改名 `_test.go` 即可跑）：内存 Store + 手动 `processClaimed` 推进协调器时钟，
`mongotest` 上的真实 `DataEngineStepInbox`，“WAL 重放投影”用直接写回执模拟（claim 租约内投影成功是现有承诺）。基线 `12726715`：

```text
--- FAIL: TestU0280Repro/a:_late_result_while_k+1_waits
    attempt 2 of gift-a:1:0 reserved a fresh lease (token=1) although attempt 1 already committed: the debit runs twice
--- FAIL: TestU0280Repro/b:_late_result_dropped_during_backoff
    completion of attempt 1 during backoff: saga: step is not waiting for a result
    attempt 2 reserved a fresh lease although attempt 1 committed and its result was dropped: the debit runs twice
--- FAIL: TestU0280Repro/c:_late_result_after_the_coordinator_gave_up
    step gift-c:1:0 took effect (receipt gift-c:1:0:1) but the saga stays failed with nothing to compensate; Complete returned no error and counted it a duplicate (duplicates=1)
```

## 要回答的三个问题

### 1. v1.19.2 上是否同样存在？是不是 15 次预算引入的？

**存在，不是 15 次预算引入的。** `git diff --stat v1.19.2 HEAD -- saga/dataengine_step_inbox.go saga/command_consumer.go` 为空；
`v1.19.2:saga/engine.go:598` 同样是 `DeadlineAt: after.NextRunAt`，`:355` 同样按 `OperationKey` 接收结果。截止时间来自 `5a9849e3`（08-25 V4.0），按 CommandID 的原生收件箱来自 `c5816f55`（08-31），
09-08 `b97e7ef5` 原样并入 core。MaxAttempts 15 是 `ac5acfbe`（10-05，v1.20.0）对模板的调整，v1.19.2 模板是 5 次。

“每次崩溃每步最多多执行一次”：**在 (a)(b) 这两种交错下成立**。崩溃时同一操作最多只有一条已提交未投影的尝试（同一进程上后一次尝试被实体屏障挡住），
B 启动后先重放它、再执行当前尝试，中间那些尝试只会过期、不会执行，所以多一次。但有两点补充：投影积压时不需要崩溃也会多执行一次；
(c) 不是“多执行”而是“执行了却没补偿”。重试预算的影响只在 (c)：次数越少，崩溃期间越容易用尽而走到 (c)；15 次反而降低了 (c) 的概率，对 (a)(b) 没有影响。

**旧的按玩家租约（v1.19.x PlayerOwner 租约状态机）**：步骤收件箱、协调器代码相同，窗口同形；失锁转移时旧进程的 WAL 只在原目录重启时重放，
claim 租约 2 分钟内重放仍会投影。按源码判断，未在旧版本上实跑。

### 2. 原生步骤原本承诺了什么？

**框架只承诺了“同一命令最多一次”，没有承诺“同一 saga 的同一步骤、同一方向最多生效一次”。** 依据：

- `SAGA.md`「一致性模型」：“同一 attempt 的消息重投共享 `CommandID`，跨 attempt 共享稳定 `IdempotencyKey`”；“以 `Command.IdempotencyKey` 作为业务唯一键”；
- `SAGA.md`「Native Entity step」：“不同 CommandID 即使共享 IdempotencyKey 仍表示新的 Saga attempt，业务 step 继续按 IdempotencyKey 保证语义幂等。”
- `saga/command_consumer.go` `SubscribeDataEngineStep` 注释、`DataEngineStepInbox` 的 Reserve / Replay 都只以 CommandID 去重。
- `SAGA.md` 还写着“被跳过的记录……步骤按 Timeout / 重投再次执行”“重复、迟到的 command/result 不会重复推进状态”——说的是**状态**不重复推进，不是业务不重复执行。

但**生成模板把它当成了步骤级恰好一次**：game-demo 的 `game/handler/gift_debit.go` 注释“That last part is what makes the step exactly-once”，
`gift.NativeStep` 只用框架的回执，没有按 `IdempotencyKey` 做业务幂等。所以缺陷落在框架与模板的分工缝上：框架把跨尝试幂等交给业务，
正式模板没有做，而原生路径的“提交点在投影”又让业务自己做幂等很难做对（见候选 E）。

### 3. 修法候选与取舍

| 候选 | 做法 | 解决 | 代价 / 风险 |
| --- | --- | --- | --- |
| **A. 收件箱按操作实例互斥并回放** | Reserve 在同一事务里：先查自己的回执；再写一个操作实例守卫文档（`saga-step-op/<operationKey[:rN]>`，同一操作的并发 Reserve 在它上面写冲突、串行化）；扫描同一操作其他尝试的 claim（`_id` 前缀 `saga-step/<operationKey[:rN]>:<n>`）：有回执且不是“可重试失败”→ 返回那次的 completion（消费者经 `transport` 重发给协调器，再 ack）；pending 且租约有效 → Duplicate 等待；租约过期 → 把它标为 superseded 并 `$inc lease_token`（其未投影记录投影时 fence 失败被跳过），再走自己的 claim | (a)(b)，以及投影积压下的重复执行 | 持久格式增量（守卫文档、claim 新状态）；“可重试失败允许新尝试执行”是新规则；原生消费者第一次在 saga result 流上发布；混跑期间旧进程看不到新守卫（只在全部升级后成立）；操作实例键要从 CommandID 去掉 `:attempt` 推出，或给 Command 加字段（改 digest / wire）。不解决 (c) |
| **B. claim 租约封顶到命令截止时间** | `lease_until = min(now+LeaseDuration, Command.DeadlineAt)`；截止后 WAL 重放的记录 fence 失败被跳过 | (c) 的主体：尝试只可能在截止前生效，而协调器只在截止后放弃 | 改变 `LeaseDuration` 的含义；**投影积压超过步骤 Timeout 时所有原生尝试都被跳过**，积压消退前步骤无法成功（修前是重复执行）；依赖协调器与投影进程的时钟偏差远小于 Timeout；仍剩“截止前已投影、completion 在放弃后才送达”的窗口 |
| **C. 协调器处理“放弃后迟到的成功”** | tombstone 记录关闭方式（带结果关闭 / 放弃关闭）；放弃后到达的正向成功不再计 duplicate，而是把 saga 重新带入补偿、只补偿该步 | (c) 的全部，含 B 剩下的送达窗口，也覆盖 Mongo 步骤 | **协调器状态机改变**：终态 Failed / Compensated 可能被重开；需要“只补偿第 s 步”的新状态；Store 接口 / 持久格式（operationDoc 加字段）变化；影响所有 saga 使用方 |
| C'. 只告警 | 同 C 的区分，放弃后到达的成功记 ERROR + 计数，不自动处理 | 让 (c) 可见 | 仍要 tombstone 加字段才能不误报；业务损失靠运维处理 |
| D. 放宽截止时间覆盖崩溃重启 | 模板步骤 Timeout ≥ 重启时长（~90s） | 降低 (a)(b)(c) 概率 | 不消除；投影积压仍触发；正常失败要等更久才重试 |
| E. 模板做业务幂等 | Player 实体记已处理的 `IdempotencyKey`（实体字段，受实体屏障与投影约束），新尝试见到就回复同结果 | (a)(b)，符合现有框架承诺，框架不改 | 每个原生步骤使用方都要自己做；实体要存键集合并清理；处理键必须是实体写（只写回执的“空”提交不受屏障约束，会在 k 被跳过时报假成功）；不解决 (c) |
| F. 协调器只接受当前尝试的结果 | `Complete` 比较 CommandID | 无 | 更糟：k 的真实结果被丢弃，k+1 照样执行 |

与 U-0281 的一致性：A～E 都不依赖过期消息留在 durable 里，与 U-0281 的 ack 不矛盾。B 之后，过期命令的 claim 也必然已失效，U-0281 的 ack 更是无条件安全。
Remote entity：原生步骤不能修改 Remote 实体（`ErrRemoteLeaseFenceUnsupported`，RR-20260926-19），`LeaseFence` 只有 dataengine 与 saga 使用（`git grep LeaseFence`），
A / B 不影响 remote entity；C / C' 影响所有 saga 使用方（含 Mongo 步骤）。codegen：`add saga` 生成的原生步骤与 game-demo 模板受 A / B 的行为变化影响，接口不变。

**推荐**：**A + B 作为框架修复**（框架补上“同一操作实例最多生效一次”，并让“尝试只在截止前生效”与协调器的超时一致），
**C' 先上告警**覆盖剩余的送达窗口，C（自动补偿迟到成功）另行决定。理由：

- 承诺应当在框架里兑现：原生路径的提交点是异步、带条件的投影，业务层（E）要做对必须理解屏障与跳过，模板已经证明这一点很容易做错；
- A 解决演练里出现最多的 (a)(b)，且在投影积压下也正确（等前一次尝试有结论再决定）；
- B 用一个简单不变量（“尝试的生效窗口 ⊆ 协调器等它的窗口”）把 (c) 收窄到“已投影、未送达”这一段，代价是 Mongo 严重变慢时原生步骤不前进而不是重复执行——对道具类业务这是更安全的失败方式，但它改变了 `LeaseDuration` 的含义，需要维护者确认；
- C 改协调器状态机、影响所有使用方，单独评审。

若维护者在下面「方向判断」里选择“超时不换 CommandID”的简化方向，它替代 A（B / C' 仍需要）。

### 为什么停在方案（维护者 10-05 指示）

A 改了框架对原生步骤的公开承诺（SAGA.md 明写“不同 CommandID 是新的尝试，业务按 IdempotencyKey 幂等”），引入新的持久状态与“可重试失败才允许新尝试”的规则，
并让原生消费者开始发布 completion；B 改变 `LeaseDuration` 语义与投影积压下的行为；C 改协调器状态机。都不属于“在现有承诺内补齐、改动局部”。
另外 A 与 E 是框架 / 模板两种分工，需要维护者选。**本轮不实施，未提交红测试**（红测试放在 evidence，避免主线留一个失败用例）。

需要维护者决定：

1. 跨尝试幂等由框架兑现（A）还是模板 / 业务兑现（E）；
2. 是否接受 B（尝试只在截止前生效；投影积压超过 Timeout 时原生步骤停止前进）；
3. “放弃后迟到的成功”先告警（C'）还是自动补偿（C）；C 是否允许重开 Failed / Compensated。

选定后的实施与验证计划：A / B 的确定性红测试就是上面的 (a)(b)(c)（(c) 在 B 下改为断言“截止后重放的记录被跳过、无回执”）；
再在生成工程赠礼路径上做一次最小 kill -9 复现（单 sid、一个机器人连续赠礼、在 debit WAL 提交后 kill -9、立即重启），断言每个 saga 每个方向最多一个回执、
背包数守恒。

## 方向判断：saga 近期缺陷是实现细节还是设计层面

`git log --oneline --since=2026-09-25 -- saga/ kit/saga/`（8 笔）与 docs/bug、docs/bugfix 索引里的 saga 条目：

| 问题 | 日期 | 提交 | 性质 |
| --- | --- | --- | --- |
| RR-20260926-30：原生步骤 lease fence 过期被跳过后，内存已改、后续投影 fatal | 09-26 | `f0d1a1e4` `570c4073` | **设计**：提交点在投影、内存在准入时已改；需维护者拍板（屏障 + 驱逐重载） |
| RR-20260926-19：原生步骤 + Remote 写无法原子提交 | 09-26 | `2af4c66d` | **设计**：只能明确拒绝 |
| RR-20260926-63：屏障期间重投消耗 MaxDeliver | 09-27 | `10f94600` | 设计后果（重投预算与屏障时长耦合），只补了运维说明 |
| RR-20260927-16：markCompleted 失败静默 | 09-27 | `881f086d` | 实现细节 |
| U-0225（更早）：补偿版本加两次，Mongo 拒收 | 09-17 | `50d89aa8` | 实现细节 |
| RR-20260917-07（更早）：Assembly 没订阅原生完成 | 09-18 | `05dea712` | 实现 / 装配遗漏 |
| NC-37：健康检查漏第三个消费者 | 10-05 | `47fca740` | 实现细节（观测链未随功能扩展） |
| NC-38：Resume 代际未持久化 | 10-05 | `47fca740` | 实现细节（BSON 映射漏字段） |
| NC-39：启动幂等比较可变状态 | 10-05 | `12726715` | **设计**：启动身份与运行状态没有分开 |
| NC-40：完成信封路由未绑定 | 10-05 | `12726715` | 实现细节（校验缺口） |
| U-0281：过期无回执命令无限 nak | 10-05 | 本批 | 实现细节（错误分类） |
| U-0280：跨尝试重复执行 / 放弃后迟到生效 | 10-05 | 未实施 | **设计**：执行身份与时间模型 |
| 模板：赠礼 debit 重试预算 5→15 次以覆盖一次崩溃重启 | 10-05 | `ac5acfbe` | 预算假设（用次数 × Timeout 去覆盖重启时长） |

判断：**单个缺陷多数是实现细节，但最严重的几条（RR-20260926-30、NC-39、U-0280）有同一个设计根源**——saga 有三套没有对齐的“身份”和“时间”：

- **身份**：协调器按操作（`IdempotencyKey`）接收结果，收件箱按尝试（`CommandID`）去重，启动按业务键 + 意图（NC-39 才分开）；同一件事在不同层用不同粒度判重；
- **时间**：命令截止（Timeout，秒级）、claim 租约（`LeaseDuration`，分钟级）、投影完成（无界）、JetStream `AckWait` / `MaxDeliver`（天级）、saga 截止，彼此只有零散的不等式约束（如 `LeaseDuration > AckWait`），没有“尝试在哪个窗口内可能生效”的统一定义；
- **结论的最终性**：协调器的放弃（超时 / 截止）与步骤的提交点（投影）不协调，放弃后的真实结果没有归宿。

规则里的几个信号都出现了：状态机交错类问题反复（RR-20260926-30、U-0280）；修复越来越依赖时间 / 预算假设（`LeaseDuration > AckWait`、`MaxDeliver × NakBackoffMax` 覆盖投影积压、重试次数 × Timeout 覆盖崩溃重启）；修复多在增加状态与分支（屏障、驱逐、Admit、转交）。

这些点不靠逐条红绿能收敛：每修一处，新的交错（崩溃、积压、退避、放弃、混跑）又会在另一处露出来。实现细节类（NC-37/38/40、U-0281）则是边修边补、可以继续按现流程处理。

候选方向与代价（供维护者选）：

- **保持现有结构，补契约 + A/B/C**：改动集中在收件箱与协调器，兼容现有 wire；代价是又多一层守卫状态，复杂度继续上升。
- **简化：超时不换 CommandID**。协调器对“超时”只重发同一条命令（同一 CommandID，只更新截止），只有收到“可重试失败”才换新 ID；
  这样“同一命令最多一次”自然就是“同一操作实例最多一次”，A 的守卫文档与扫描都不需要。代价：`commandDigest` 要排除截止时间与
  `Attempt`（否则重发撞 `ErrIdentityConflict`），是 wire / 身份格式变化；`Attempt` 不再等于派发次数，`MaxAttempts` 的含义要重新定义；
  原生消费者重投时命中回执要经 `transport` 重发 completion（否则退避期间被丢弃的结果永远到不了协调器，与 A 相同）；
  B / C 的问题（放弃后迟到生效）仍要单独回答。
- **收紧适用面**：原生步骤只用于“可重复执行无害”或自带业务幂等的步骤（E 的分工写进模板与 SAGA.md），框架不承诺跨尝试；
  代价是道具类业务都要自己做幂等，模板要示范正确做法。

**建议调整方向**（推荐第一条或第二条，倾向第二条：它减少状态而不是增加）：在继续修 saga 之前，先写一页“步骤执行契约”并由维护者确认，再按它补实现和测试：

1. 至多一次的单位是**操作实例**（saga、方向、步骤、代际），不是尝试；尝试只是协调器的重试计数；
2. 定义每个尝试的**生效窗口**并与协调器的等待窗口对齐（B 的不变量），列出所有时间参数之间必须满足的关系，在 `NewEngine` / 消费者订阅时校验；
3. 定义协调器放弃后迟到结果的归宿（C / C'），以及哪些终态可以被重开；
4. 明确框架与模板的分工（A 或 E），模板注释与 SAGA.md 同步；
5. 测试从“每个缺陷一条红绿”改为**状态机交错测试**：协调器（内存 Store + 可控时钟）× 原生收件箱（mongotest）× 投影（可控的准入 / 投影 / 跳过 / 崩溃点），对所有交错断言“每个操作实例最多一个生效回执、生效的正向步骤要么在完成的 saga 里要么被补偿”。本记录的复现用例可作为它的第一批场景。

## 验证

本轮只做了复现，没有修改 U-0280 相关代码。U-0281 的修复先行提交（`96720a05`），验证见 U-0281 记录。复现用例在 C01 结束后又以 `-count=20` 跑过，三个子用例每次都红（确定性）。

## 实施（2026-10-05，维护者决定按推荐处理）

维护者决定：A + B + C'，并补充“**框架重试的次数可以是一个配置，一次操作可以有多次尝试**”；已生成工程不迁移，仓库内模板与生成物同步。
授权直接实现。代码 `054fdd66`，文档与证据见同分支随后的提交；CHANGELOG `[Unreleased]` Fixed / Changed。

### 原生步骤执行契约（正文在 SAGA.md「原生步骤执行契约」）

1. 最多生效一次的单位是**操作实例**（saga + 步骤 + 方向，即 `Command.IdempotencyKey`）；一次操作可以有 `MaxAttempts` 次尝试（配置），
   跨 Resume 的代际也是同一操作实例。
2. 每次尝试的生效窗口包含在协调器等它的窗口内：claim 租约 `lease_until = min(now + LeaseDuration, Command.DeadlineAt)`，截止后才投影的记录被跳过。
3. 新尝试先看同一操作实例的其他尝试：成功（任何一生）或本生的拒绝 → 回放那次的 completion；可重试失败 → 执行；
   pending 且租约有效 → nak 等待；租约过期 → 接替（`superseded` + `lease_token+1`），它未投影的记录 fence 失败被跳过。
4. 协调器放弃之后才到的成功只告警（ERROR + `saga.completion.late_after_abandon_total{saga_type,phase}` + `Stats().LateAfterAbandon`），
   不重开终态、不自动补偿；tombstone 区分“带结果关闭 / 放弃关闭”避免误报。
5. 分工：框架兑现原生步骤的跨尝试幂等，模板不再需要自己按 `IdempotencyKey` 做业务幂等；Mongo 步骤不在本契约内（见「与推荐的差异」）。

### 选定方案：A，而不是“超时不换 CommandID”

评估了记录里的简化方向（协调器对超时只重发同一 CommandID，只有可重试失败才换新 ID）。它能把“同一命令最多一次”直接变成“同一操作最多一次”，
状态更少，但在本仓库里代价更大，所以没有选：

- **身份格式变化**：`commandDigest` 是整条命令的 JSON 摘要（含 `DeadlineAt`、`Attempt`、`CreatedAt`），同时用在 Mongo 收件箱回执、原生 claim、
  Nest 步骤回执（`BindCommand`）与 lease fence 上。同一 ID 重发而截止时间不同，要么改摘要定义（所有在途命令与回执的身份在升级瞬间对不上，
  要么得做新旧双摘要比较），要么撞 `ErrIdentityConflict`。A 不碰 wire 与摘要。
- **“一次操作多次尝试”的配置要重新定义**：`Attempt` 不再等于派发次数，`MaxAttempts` 变成“超时重发次数 + 可重试失败次数”的混合，
  演练里看的“尝试号”也失去意义。A 下 `MaxAttempts` 仍是“这次操作最多派发几次尝试”，与维护者的说法一致。
- **Resume 仍会重做**：Resume 进入新代际、换新 ID，上一生里已生效（放弃后迟到）的步骤会被再执行一次；A 按操作实例跨代际回放成功，
  反而给 C' 的告警提供了一条安全的处置路径（`Resume` 回放而不是重做）。
- 两者都需要“命中旧结果时经 saga 结果流重发 completion”与 B；A 多出的状态是守卫文档与 claim 的三个字段，局限在收件箱里。

### 配置设计

- core：`saga.StepBudget{Timeout, MaxAttempts, BackoffMin, BackoffMax}`、`saga.StepBudgets{Defaults, Overrides map[StepKey]StepBudget}`，
  `Options.StepBudgets`（`DefaultOptions` 带框架默认 5s / 5 次 / 100ms..5s）。`Engine.Register` 用 `StepBudgets.Resolve` 补齐每个字段：
  按步骤覆盖 > 定义里写的值 > 配置默认 > 框架默认；`NewEngine` 校验默认值与覆盖（负数、超过 1000 次、退避上限小于下限）。
- kit：saga Mod 读 `saga.step_defaults.{timeout,max_attempts,backoff_min,backoff_max}` 与 `saga.steps.<type>.<step>.<字段>`
  （`kit/saga/step_budgets.go`）。`saga.steps` 下的类型、步骤必须对应注册的定义，字段必须是这四个，否则 `Init` 失败。
  `StepBudgetsFromConfig(cfg, definitions...)` 导出，生成工程的测试用它得到与运行时相同的预算。
- codegen：`add saga` 生成的步骤只写名字与 topic（定义的注释说明预算来自配置），saga Mod 的生成配置带 `step_defaults` 与 `steps: {}`。
- demo：`ac5acfbe` 的“生成后把 definition.go 里 debit 的 MaxAttempts 改成 15”换成 `demoGiftRefundBudget` 改 game 服务的
  dev 配置、prod example 与 k8s Secret example，写入 `saga.steps.gift_item.debit.max_attempts: 15`（带理由注释）。
  `gift_saga_budget_test.go` 保留原承诺（退款重试窗口 ≥ startup_wait + ttl + 45s），改为经 `kitsaga.StepBudgetsFromConfig` 读两份配置再核对；
  把配置改回 5 次时它照原文红（`a refund gets at least 25.75s of retries (max_attempts 5 x timeout 5s ...) ... = 1m30s`）。

### 与推荐的差异 / 补充

- **C' 的覆盖面**：推荐只说“tombstone 区分带结果关闭 / 放弃关闭”。实施中发现**退避期间被 saga 截止或人工 `Compensate` 结束的步骤根本没有 tombstone**
  （`closedOperation` 只关闭 Waiting 记录的 OperationKey），迟到成功会以 `ErrNotWaiting` 被 Nest 完成消费者当永久错误丢弃，告警看不到它。
  所以这两条路径在当前步骤 `Attempt > 0` 时也写放弃关闭的 tombstone（既有 `CloseOperation` 机制，不加状态、不改状态转移）。
  Resume 之后新一生带结果关闭同一操作时，tombstone 改记为“带结果”，旧尝试迟到的结果按重复处理。
- **回放规则区分代际**：成功跨代际回放（Resume 不重做已生效的步骤）；业务拒绝只在同一生里回放（Resume 的目的就是修复原因后重新执行）。
- **顺带修复**：`SubscribeDataEngineStep` 校验原生 handler 的返回值，生成模板返回零值 `Completion`，于是每次成功执行后都多一次 nak 与重投
  （重投读到回执才 ack）。返回值本来就不使用，现在不再校验（`TestNativeStepConsumerHandlesOperationOutcomes/zero completion`）。
- **Mongo 步骤不在本契约内**：`MongoCommandInbox` 的提交点在 handler 的 Mongo 事务里，与命令截止时间没有绑定，跨尝试仍按 `IdempotencyKey`
  做业务幂等（模板的 deliver 用 mail `RequestID`）。若要 Mongo 步骤也按操作实例最多一次，需要另一份方案（守卫 + 截止时间约束提交），需维护者决定。

### 先红后绿

正式用例 `saga/step_operation_promises_test.go`（evidence 的复现移入并改成走真实 `SubscribeDataEngineStep` 与真实
`dataengine/engine.MongoStore.Project`：业务写往 `debits` 插一份文档，文档数就是生效次数）。基线 `50e9a4e8` 的红
（全文 [formal-red-before.txt](evidence/U-0280/formal-red-before.txt)）：

```text
a: operation gift-1:1:0 took effect 2 time(s) with 2 success receipt(s), want 1: debits by [gift-1:1:0:1 gift-1:1:0:2] (handler ran 2 times)
b: operation gift-2:1:0 took effect 2 time(s) with 2 success receipt(s), want 1: debits by [gift-2:1:0:1 gift-2:1:0:2] (handler ran 2 times)
c: attempt gift-3:1:0:1 was replayed 55s after its deadline and still took effect: the claim lease outlived the command deadline, ...
c': success of gift-4:1:0:1 arrived after the coordinator abandoned the step; saga.completion.late_after_abandon_total grew by 0, want 1 (duplicates grew by 1)
d: attempt gift-5:1:0:1 was projected after attempt gift-5:1:0:2 superseded it: the step took effect twice
Resume: operation gift-6:1:0 took effect 2 time(s) ... debits by [gift-6:1:0:1 gift-6:1:0:r1:1]
saga 截止: late success after the saga deadline grew the alarm by 0, want 1
同生拒绝: a refused step ran 2 times in one life, want 1
```

“可重试失败不挡新尝试”“TTL 之后的迟到成功不告警不生效”是守卫用例，修前即绿。B 之后 (c) 断言“截止后重放的记录被跳过、没有回执、没有业务写”，
(c') 断言告警计数 +1 且 saga 仍 Failed。另加：`TestNativeStepLeaseNeverOutlivesTheCommandDeadline`（新建 / 接管都封顶、过期命令 `ErrCommandExpired`）、
`TestNativeStepConsumerHandlesOperationOutcomes`（被接替尝试 ack 不执行、他人租约有效时 `errOperationAttemptInFlight`、零值 Completion 不再 nak）、
人工 `Compensate` 放弃退避中步骤后的迟到成功告警、`TestMongoStoreTombstoneTellsAbandonedFromResolved`（MongoStore 上 abandoned / result / 旧 tombstone /
Resume 升级）、kit `TestStepBudgetsComeFromConfigWithPerStepOverrides` 与 `TestStepBudgetConfigRejectsTyposAndImpossibleValues`。
既有用例按新契约改写两处：`TestDataEngineStepInboxReservesCommandIdentityAndAllowsNewAttempt`（第一次尝试租约有效时新尝试得到
`errOperationAttemptInFlight`，截止后接替）、`TestDataEngineStepInboxUsesAbsoluteClaimExpiry`（索引 3 → 4）。

真实 Mongo（`-tags integration`，隔离副本集，库 `roost_u0280_<pid>_<ns>` 用后删除）`saga/step_operation_real_mongo_integration_test.go`：
- 6 个尝试并发 Reserve 同一操作 × 20 轮，每轮恰好 1 个拿到新租约。**去掉守卫的负对照**（临时让 `guardOperation` 直接返回，验证后还原）：
  `round 1: 6 attempts of one operation reserved a live lease at the same time, want exactly 1 (results=[<nil> <nil> <nil> <nil> <nil> <nil>])`
  （[real-mongo-guard-disabled-red.txt](evidence/U-0280/real-mongo-guard-disabled-red.txt)）——mongotest 按集合检测写冲突，证明不了守卫，必须在真实服务端看。
- “接替 vs 投影”并发 40 轮：每轮要么 k 生效、k+1 回放 k，要么 k 被跳过、k+1 执行，从未两者都生效（本次 39 / 1）。

### kill -9 复现（生成 game-demo，修前 / 修后）

工具在 [evidence/U-0280/kill9/harness](evidence/U-0280/kill9/harness)：从给定的 roost-core 树生成 game-demo（replace 指向该树），把开发配置隔离到
本次前缀（库 `<tag>_game / _saga / _remote`、DAO 编译期库名常量、流 `<TAG>_SAGA / _EFFECTS / _SYNC`、NATS 前缀、Redis 独立 db 与键前缀），
单 sid 1000、全部服务按 `deploy/dev/run.sh` 启动；一个机器人 `add_item` 30 个药水后每 60ms 向从未进过游戏的玩家 1 赠 1 个（debit → deliver 拒绝 → 退款，
每个 saga 以 compensated 结束，背包守恒 = 每个机器人 30）。每轮 `u0280watch` 等到一个本轮新认领、还没有回执的 debit claim，再等 WAL 增长
（那次 Nest 事务已写进 WAL），立即 `kill -9` game 并马上重启（重启等单实例锁约 15s）；三轮后等全部 saga 终结，统计回执与背包。

| 运行 | 代码 | saga | 状态 | debit 回执 >1 | 退款回执 >1 | 背包（每个机器人应为 30） | not waiting 丢弃 | 放弃后迟到告警 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| [u0280a](evidence/U-0280/kill9/u0280a/analysis.txt)（修前，第一版 watcher） | origin/main `7922e428` | 154 | 154 compensated | 6 | 0 | 28 / 27 / 29（少 6） | 12 | 0 |
| [u0280c](evidence/U-0280/kill9/u0280c/analysis.txt)（修前，与修后同一 watcher） | origin/main `7922e428` | 154 | 154 compensated | 7 | 3 | 30 / 27 / 29（少 4 = −7 + 3） | 20 | 0 |
| [u0280b](evidence/U-0280/kill9/u0280b/analysis.txt)（修后） | 本分支 | 164 | 164 compensated | **0** | **0** | **30 / 30 / 30** | 2 | 0 |

u0280a 的第一版 watcher 没有按认领时间过滤，第 2、3 轮是被第 1 轮留下的旧 pending claim 触发的（kill 时机仍在赠礼中、WAL 增长之后），所以修前又用
修后同一版 watcher 跑了 u0280c。修后仍有 2 次 `not waiting` 丢弃（(b) 交错照常发生），但下一次尝试回放了结果，没有重复执行；三轮里没有出现
“截止前已投影、放弃后才送达”，告警为 0。演练窗口的 oplog 只写了 `u0280a_* / u0280b_* / u0280c_*`，共享 `game / saga / remote_entity` 为 0
（[oplog-check.txt](evidence/U-0280/kill9/oplog-check.txt)）；结束后按清单删除了 9 个库、9 条流（`^U0280[ABC]_`，删前列出、全部只匹配本次前缀）、
Redis db 9 / 10 / 11 里 58 个 `u0280[abc]*` 键（删前核对没有其他前缀的键）。

### 组合契约复核（fix-contract-review）

1. **新增错误追到调用方**：`Reserve` 新增 `ErrCommandExpired`、`errAttemptSuperseded`（消费者 ack，计 `saga.step.expired_unexecuted_total`）与
   `errOperationAttemptInFlight`（消费者返回、nak 重投，受旧尝试截止约束）。其他调用方：codegen 夹具 `fenced_step_test.go`（截止 1 小时，行为不变，
   `test-dataengine-generated.sh` 通过）、benchmark（不同操作，通过）。`Engine.Complete` 新分支返回 `record, nil`，两个完成消费者都 ack。
2. **取消与关闭**：`releaseLease`（屏障时交还租约）之后的重投走“自己的 claim 已过期 → 先看其他尝试 → 接管”，被接替的旧尝试不会挡住它；
   U-0281 的过期 ack 分支不变（过期命令不进 `Reserve`），B 之后过期命令的 claim 必然已失效，ack 无条件安全。
3. **有效期**：claim 租约现在 ≤ 命令截止；`LeaseDuration > AckWait` 校验保留（截止后的重投被过期分支 ack，租约短于 AckWait 不会让第二个进程执行同一命令）。
   守卫文档与 claim 一样有 `expires_at`（ReceiptTTL）。claim / 回执 TTL 后同一操作的新尝试看不到旧尝试——与回执 TTL 的既有约束相同（TTL 远长于 saga 生命周期）。
4. **已知退化不降格**：投影积压超过步骤 Timeout 时原生步骤停住（修前是重复执行）——维护者已接受（B），写进 SAGA.md、USER_GUIDE、T-226；
   每次新建 / 接管 claim 多一次守卫 upsert 与一次索引查询，mongotest 上 `BenchmarkDataEngineStepReservation/new_command` 0.52 → 1.33 ms/op
   （替身按集合快照，集合越大越慢，不代表真实 Mongo；`duplicate_active_claim` 4.8 → 5.5 µs/op），真实 Mongo 上的开销未测。
5. **混跑**：契约只在全部步骤进程与协调器升级后成立（旧进程的 claim 没有 `operation_key`、租约不封顶；旧协调器不写 `closure`，新协调器把缺字段的
   tombstone 按“未知”处理、不告警）。维护者决定已生成工程不迁移。

### 验证（`GOWORK=off`，分支基于 `855c2a38`；全套在 `e81d81bc` 上跑过，之后上游只改 skill / combatcomponent，rebase 后重跑 build、vet、saga 与根包）

- `gofmt -l` 空；`go vet ./saga/... ./kit/saga/... ./codegen/...`；`go build ./... && go vet ./...`；`go generate ./...` 后 porcelain 不变；
  `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` 通过。
- `go test -race -count=3 ./saga/... ./kit/saga/...` 通过；新用例 `-race -count=200 -run 'TestNativeStep|TestDataEngineStepInbox|TestStepBudget'` 通过；
  `go test -count=1 ./codegen/...` 通过；根包 `go test -count=1 .` 通过。
- 真实 Mongo：`go test -tags integration -run 'TestRealMongo(ConcurrentAttempts|SupersedeAndProjection)' ./saga/` 通过。
- `bash scripts/test-dataengine-generated.sh`（隔离 Mongo）通过。
- 生成 game-demo（replace 指向本 worktree）`go build ./... && go vet ./... && go test ./...` 通过，`TestARefundOutlastsACrashRestartOfTheSendersSid` 读配置通过。
- kill -9 复现见上表。

### 兼容

- 公开 API 增量：`saga.StepBudget` / `StepBudgets` / `StepKey` / `DefaultStepBudget`、`Options.StepBudgets`、`Stats.LateAfterAbandon`、
  `OperationClosure` / `CompletionHistory` / `CompletionHistoryStore`（可选接口，`Store` 不变）、`ErrCommandExpired`、`MongoStore.CompletionHistory`；
  kit `StepBudgetsFromConfig`。行为变化：零值预算字段由 `Register` 补齐（以前是 `ErrInvalidDefinition`）；租约封顶；原生 handler 返回值不再校验。
- 持久格式增量：见 SAGA.md「原生步骤执行契约」代价一节；没有删除或改名字段，wire（`WireVersion=1`）与摘要不变。
- 指标：新增 `saga.completion.late_after_abandon_total{saga_type,phase}`、`saga.step_inbox.superseded_total`、`saga.step.attempt_replayed_total`；
  kit saga 健康消息多 `late_after_abandon=`。
- 已生成工程不迁移：旧 `definition.go` 里写死的预算照旧生效，配置的按步骤覆盖优先于它。

### 未验证项 / 风险

- 混跑（新旧步骤进程 / 协调器并存）没有实跑，只按源码说明了语义。
- “截止前已投影、放弃后才送达”的告警路径只在单元测试里构造；三轮 kill -9 里没有自然出现。
- 时钟偏差（协调器、步骤进程、投影进程）对 B 的影响只做了说明，没有注入偏差实测。
- 真实进程下的投影积压超过 Timeout（步骤停住）没有实跑，只有单元用例 (d)。
- 真实 Mongo 上 Reserve 新增读写的延迟开销没有测；S5 的其余项（coordinator 租约过期接管后晚 Apply、发布成功但 Ack 未知、真实 broker 跨进程恢复）
  与 U-0280 不重叠，本轮未覆盖。
- Mongo 步骤的跨尝试幂等仍靠业务（见「与推荐的差异」），是否纳入框架需维护者另行决定。

## 复核（2026-10-05，独立审查）

两处在当前实现上能写出红用例的缺口，均落在 C'（“已生效、未计入的成功必须可见”）上，各一笔提交，用例在
`saga/step_operation_review_test.go`：

1. **以失败关闭的 operation 被记为“带结果关闭”**（`fbc3da77`）：协调器在等最后一次尝试时接收较早尝试晚到的可重试失败、
   重试用尽；`MongoStore.Apply` 只要带 Receipt 就记 `closure=result`，最后一次尝试照常执行并生效，它的成功按重复静默确认。
   改为只有接收成功才记 `result`，以失败关闭（可重试失败用尽、拒绝）记 `abandoned`。
   红：`attempt gift-1:1:0:2 took effect after the coordinator closed the operation on a stale retryable failure; saga.completion.late_after_abandon_total grew by 0, want 1`。
2. **过期投递 ack 时不回放同一操作已生效的成功**（`fe39c095`）：情形 (b) 的成功在退避期间被丢弃后，最后一次尝试的投递
   过了自己截止（U-0281 过期分支或 Reserve 的 `ErrCommandExpired`），只查自己的回执就 ack，成功再无人送达、协调器放弃且不告警。
   改为两条不执行的分支在 ack 前只读查找同一操作已生效的成功并经 saga 结果流重发。
   红：`... the expired delivery of gift-N:1:0:2 was acknowledged without replaying it: the saga ended failed with CompletedSteps=0 and no late_after_abandon alarm`。
