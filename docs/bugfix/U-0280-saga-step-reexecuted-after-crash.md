# U-0280：kill -9 / 失锁后，原生 saga 步骤以新的尝试再执行一次（道具复制或丢失）

**仓库 / 位置**：roost-core `saga/dataengine_step_inbox.go`（claim / 回执按 CommandID）、`saga/engine.go`（命令截止时间、`Complete`、`closedOperation`）、
`saga/mongo_store.go` `CompletionRecorded`。
**来源**：真实进程演练 drill6（2026-10-05，代码 `47a9132c`，生成 game-demo，赠礼 saga）。维护者授权按先红后绿修；10-05 追加指示：
**修法若要改设计、公开语义或协调器状态机，写完方案就停下交回**。
**状态**：**已定位、已确定性复现；方案待维护者决定，未实施**（理由见「为什么停在方案」）。**定位文档**：TROUBLESHOOTING T-226。
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
