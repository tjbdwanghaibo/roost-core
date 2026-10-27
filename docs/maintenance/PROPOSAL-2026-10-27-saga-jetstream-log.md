# Saga 协调器以 JetStream 为权威日志：设计稿与原型（2026-10-27）

**状态：原型。** 代码在 `scripts/perf/saga-jslog-proto/`（实验包，不进生产路径，不改 `framework/saga` 的 Store / 引擎 / wiring），
分支 `proto/saga-jetstream-log` 只在本地提交。压测报告见 [SAGA-JETSTREAM-LOG-PROTO-20261027](../performance/SAGA-JETSTREAM-LOG-PROTO-20261027.md)。
本文先写前提与实测语义，再写设计；凡是实测改变了设计的地方都标“实测”。

## 0. 结论

1. **语义成立**（§2，原始错误文本在表里）：通配期望序号能做分区单写者（并发写每轮恰好一个成功，失败码 10071 / 10164）；原子批量在
   2.14.5 + nats.go 1.53.1 上手写头可用，首条带期望序号、旧序号整批拒绝；MsgID 去重**先于**期望序号检查，结果未知可以“同一 MsgID、
   同一期望序号重发”判定；R3 kill -9 leader 不丢已 PubAck 的写，去重表随 leader 切换保留。**因实测调整的设计**：用一条流 + 通配过滤而不是流级序号
   （结果与启动意图同流会打断流级检查）；批的结果未知不能靠 PubAck 重放（同 MsgID 的批返回 10201），改为“10201 或最后一条是自己的 = 已提交”；
   核心订阅能收到被拒的发布，事件只能经 JetStream 消费者读；选主停顿 5～7s 期间整条流不可写。
2. **原型完整跑通**：分区协调器（内存状态机、CAS 发布、原子批量、时间轮、KV 快照 + 清理、重放、TTL 租约只管活性）+ 参与方（真实 Nest 实体 + nestwal strict / pipelined，
   一个事务里改余额并写操作回执，WAL 落盘后直接发结果）。替身：参与方不投影 Mongo、WAL 不 ack（重启从整段 WAL 重建）、实体手写；没做 Resume / 代际、
   人工补偿、定义版本、业务键查询、指标（§14）。
3. **正确性**：正向 / 补偿、重复命令与结果、迟到成功重开补偿、旧写者被隔离、快照清理后重放一致、发布结果未知、多流、pipelined、协调器与参与方 kill -9，
   `-race` 全绿；变异去掉期望序号 → 事件链分叉，去掉回执检查 → 余额不符（§10、报告 §5）。原型自身的时间轮缺陷由迟到成功用例发现并修正（§9）。
4. **量级**（压缩矩阵，口径与现状不同，不算倍数）：c1 端到端 P50 约 11ms，c128 约 2500～2900 TPS 仍未饱和；协调器关键路径 0 次本地 fsync，
   每步 3 条 JetStream 消息，c128 每次发布合 8～9 条事件；参与方每步约 0.04 次 fsync（组提交）。同机现状原生步骤 c1 P50 约 260～330ms、饱和约 7～9 TPS，
   差距主要来自去掉步骤侧 Mongo 投影 / 收件箱与不跑 Mongo（报告 §1、§4）。
5. **建议**：方向可行，值得进入实施评审；先补两项再决定是否替换 Redis 权威路线——异步 Mongo 投影在压测中同时运行（量持续吞吐与积压），以及 Linux 跨机三节点复测（§16）。

## 1. 前提（维护者 2026-10-27 确认，原文）

1. JetStream 3 副本（R3、文件存储），收到 PubAck 即视为不丢（多数派持久）；整集群同时断电丢最近数据的风险由前提承担，框架不处理。
2. 生产 NATS ≥ 2.12，与服务同网段。本机 nats-server 是 v2.14.5，`go.mod` 里 nats.go v1.53.1。
3. etcd 只做服务发现 / 少量锁，不放事务数据。
4. Mongo 不在关键路径，只作可选异步投影（原型里可以不做投影，只需说明接法）。

补充（不是新前提，是前提 1 的含义）：JetStream 文件存储缺省不逐条 fsync（按 `sync_interval` 周期刷盘，NATS 文档；原型未改），
PubAck 表示多数派已写入。所以协调器关键路径上**没有本地 fsync**；参与方的 Nest WAL 仍按 strict / pipelined 逐组 fsync。

## 2. JetStream 语义实测（2.14.5 + nats.go 1.53.1，私有三节点、R3、文件存储）

用例在 `semantics_test.go`、`failover_test.go`，`-v` 打出原始回复。

| 项 | 结论 | 原始证据 |
| --- | --- | --- |
| 单 subject 期望序号 `Nats-Expected-Last-Subject-Sequence` | 只看该 subject 的最后序号，别的 subject 写入不影响 | 旧序号：`code=400 err_code=10071 description=wrong last sequence: 3` |
| 通配 `…-Subject-Sequence-Subject: <p>.0.ev.>` | 序号按“匹配过滤的最后一条”比较；过滤里任一 subject 的写入都会推进它；发布 subject 不必落在过滤里（同样检查） | `wrong last sequence: 4`；只带 `-Subject` 不带序号：`err_code=10193 missing sequence for expected last sequence per subject` |
| 流级 `Nats-Expected-Last-Sequence` | 看整条流；分区结果也写在同一条流时，任何结果都会让协调器的检查失败 | `wrong last sequence: 2` |
| 多写者并发同一期望序号 | 40 轮 × 16 写者，每轮恰好 1 个成功 | 失败者错误码：并发提案时 `err_code=10164 description=wrong last sequence`（R>1 的“常量”版本，无序号），顺序到达时 10071；两者同义 |
| 原子批量（`AllowAtomicPublish`） | nats.go 1.53.1 没有高层 API，手写 `Nats-Batch-Id` / `Nats-Batch-Sequence`（1 起）/ 末条 `Nats-Batch-Commit: 1`；非末条用普通 publish，末条用 request，回复 `{"seq":<末条序号>,"batch":…,"count":N}` | `{"stream":"SEMB…","seq":3,"batch":"b…","count":3}` |
| 批 + 期望序号 | 首条带通配期望序号：旧序号 → 整批拒绝、一条不写 | `{"error":{"code":400,"err_code":10071,"description":"wrong last sequence: 3"},…,"seq":0}`，存储增量 0 |
| 批内后续消息带期望序号 | 按**批前**状态比较（第二条带批前的最后序号被接受）；后续消息带流级 `Expected-Last-Sequence` 不符 → 整批拒绝 | 接受：`{"seq":5,…,"count":2}`；拒绝：`wrong last sequence: 6`、存储 0 |
| 批 + MsgID | 批内允许 `Nats-Msg-Id`；同一 MsgID 的批重发**不返回原 PubAck**，整批拒绝 | `err_code=10201 atomic publish batch contains duplicate message id`，存储 0 |
| 批缺号 / 未提交 | 缺号整批拒绝；没有 Commit 的半批不落库 | `err_code=10176 atomic publish batch is incomplete`；未提交半批 0.5s 后存储 0 |
| 首条用 request | 服务端对非提交消息回空回复（流控 ack） | `reply=""` |
| MsgID 去重与期望序号的先后 | **去重在期望序号检查之前**：同一 MsgID 重发，即使期望序号已过时，也返回原序号、`Duplicate=true` | `ack={Sequence:1 Duplicate:true}`（别的写者已推进过滤之后同样如此）；新 MsgID + 旧序号 → 10071 |
| PubAck 丢失（已落库） | 不带 reply 发出 CAS 写后用同一 MsgID、同一期望序号重试 → `Duplicate` 原序号 | `ack={Sequence:3 Duplicate:true}` |
| R3 kill -9 流 leader | 600 次单写者 CAS 写，第 200 次时杀 leader：已 PubAck 的 600 条全部在、恰好一次、序号与 PubAck 一致；1 次首发超时（`context deadline exceeded`），用同一 MsgID + 期望序号重试判为“新写入” | 写入停顿最长 5.0～7.2s（选主）；新 leader 接管 |
| 去重表随 leader 切换保留 | 已落库但没收到 PubAck 的写，立刻 kill -9 新 leader，重试判 `Duplicate` 原序号 | `ack-lost write landed at 601, leader n3 killed, retry after 5.10s: ack={Sequence:601 Duplicate:true}` |

**对设计的影响（实测）**：

1. 单写者用**一条流 + 通配过滤 `<p>.<k>.ev.>`** 的期望序号，不用流级序号：结果与启动意图写在同一条流里，流级检查会被它们打断。
   每分区一条流时流级检查可用，但结果也要换到别的流（多一个流、参与方结果不能和分区事件同序），原型没有采用。
2. 结果未知的处理：**同一 MsgID、同一期望序号重发**。单条：成功或 `Duplicate` 都表示已提交；10071/10164 表示别人写了（读过滤上最后一条，
   MsgID 是自己的就是已提交，否则被隔离）。批：成功 = 这次提交；10201 = 之前已提交；序号不符同单条。去重窗口（原型 2 分钟）必须长于重试时长。
3. 期望序号只放在批的首条（按批前状态比较，后续条带也不会更严）。
4. 选主停顿 5～7s 期间整条流不可写：所有分区的协调都停。分区数多时应把分区分到几条流（不同 leader），见 §11。
5. **核心订阅能收到被拒的发布**（发布就是往 subject 发消息，流拒绝它不影响核心订阅者）。所以参与方和终态观察者都必须用 JetStream 消费者读，
   不能用核心订阅读事件 subject。

## 3. 布局

- saga ID → FNV-1a → 分区 `k ∈ [0, P)`。一条流 `SAGAJS_<PREFIX>`，subject `<prefix>.>`，R3、文件存储、`AllowAtomicPublish`、去重窗口 2 分钟。
- subject：

| subject | 写者 | 读者 |
| --- | --- | --- |
| `<prefix>.<k>.ev.acct` | 分区协调器（事件 + 发给 acct 参与方的命令） | 参与方 durable 消费者（过滤 `<prefix>.*.ev.acct`）；重放 |
| `<prefix>.<k>.ev.done` | 分区协调器（终态事件） | 发起方 / 业务的有序消费者（过滤 `<prefix>.*.ev.done`）——终态通知天然存在；重放 |
| `<prefix>.<k>.ev.none` | 分区协调器（无命令的状态变化，如进入退避） | 重放 |
| `<prefix>.<k>.in.start` | 发起方（MsgID = `start.<id>`） | 分区协调器（过滤 `<prefix>.<k>.in.>`） |
| `<prefix>.<k>.in.res` | 参与方（MsgID = `res.<CmdID>`） | 分区协调器 |

- KV：`<STREAM>_SNAP`（键 `p<k>`，分区快照）、`<STREAM>_OWN`（键 `p<k>`，所有权租约，桶 TTL）。
- 生产里命令目标要细到持有实体的进程：`ev.<service>.<sid>`，每个 sid 一个 durable 消费者；原型只有一个参与方进程。

## 4. 事件格式（`model.go`，`WireVersion=1`）

`Event{v, part, writer, in, at, kind, saga, cmd?}`：`saga` 是这笔 saga 的**完整新状态**（状态、当前操作与尝试、截止 / 退避时刻、
已生效步骤、操作 tombstone、版本），`cmd` 是要派发的一次尝试（`op` = 现行 IdempotencyKey，`cmd_id` = `op:attempt`，截止时刻）。
状态和命令在同一条消息里，没有 outbox。`in` 是写这批事件时已处理的最大输入序号。结果 `Completion{saga_id, op, cmd_id, phase, step, success, retryable, reason}`。

## 5. 单写者与隔离

- 分区内存有 `lastEv`（过滤 `<p>.<k>.ev.>` 上最后一条事件的序号）。每轮发布的首条带 `Expected-Last-Subject-Sequence-Subject: <p>.<k>.ev.>`
  与 `Expected-Last-Subject-Sequence: lastEv`。**PubAck 之后才改内存**，`lastEv` 取 PubAck 序号（批取末条）。
- 被拒（且最后一条不是自己的）→ 分区立即停（fail-stop，`ErrFenced`），不重试、不读回继续：别的写者已推进，本实例的内存过时。
- 所有权租约（KV TTL，原型 3s，每 TTL/3 续一次）只管**活性**：谁来接管。续不上 → 分区停。接管方 `Create` 键（过期后才成功）或测试 / 运维
  用 `Put` 强占。**安全不依赖租约**：旧实例还活着、租约也还认为是自己的，它的写入仍被期望序号拒掉（`TestFenceRejectsStaleWriter`）。
- 快照也按修订号 CAS，并且只向前（比较 `LastEv`）：旧实例写不进比新实例旧的快照（§8）。

## 6. 批量

一轮循环取至多 `MaxBatch`（256）条输入加到期定时器，在内存副本上算完全部转换，产生的事件用**一次**发布提交：1 条用普通发布，多条用原子批量
（首条带期望序号）。一个分区同时只有一次发布在途（下一次的期望序号依赖这次的 PubAck）；分区之间并行。压测里每次发布平均携带的事件数见报告。

## 7. 参与方（`participant.go`）

- durable pull 消费者，过滤 `<p>.*.ev.acct`，AckExplicit，AckWait 30s。每条命令一次 Nest `Request` 到目标实体：
  handler 先查实体里的操作回执（`opKey → {cmd_id, success, reason}`），有就**回放**，不执行；没有且已过命令截止 → 不执行、ack；
  否则执行业务（改余额）并写回执，**同一事务、同一条 WAL 记录**（补丁 `$set bal`、`$set r.<opKey>`，过期回执顺手 `$unset`）。
- Nest 返回（strict：组提交 fsync 之后；pipelined：外化闸门之后）→ 发 `Completion` 到 `<p>.<k>.in.res`（MsgID `res.<本次 CmdID>`）→ PubAck → ack 命令。
  发结果之前崩溃：命令没 ack，重投后回放回执、再发结果。
- 同一操作的多次尝试由**实体锁**串行化：后到的尝试读到先到的回执。不需要现行的操作状态文档、Reserve 事务、lease fence 与实体屏障。
- 回执保留期（原型 10 分钟）必须长于“命令可能被重投的最长时间”；过期后同一操作的迟到投递按过期处理。

## 8. 重放、快照与清理

- **重放**（接管 / 启动）：读快照 `{seq=InHi, last_ev, in_hi, sagas}`，再用有序消费者读 `<p>.<k>.>` 从 `seq+1` 到当前末尾：事件按版本只前进地覆盖 saga、
  更新 `lastEv`、`InHi = max(InHi, ev.in)`；输入先缓冲，读完后只保留序号 > `InHi` 的作为待处理输入。读完再核对快照修订号没变（变了就重做）。
  然后按每笔的截止 / 退避时刻重建时间轮，live 消费者从末尾之后开始。
- **快照**：每 `SnapshotEvery` 条事件（压测 2000），把内存状态（非终态 + 保留期内的终态）写进 KV，按修订号 CAS；冲突时读回比较 `LastEv`，更新的不是自己 → `ErrFenced`。
  快照的 `seq` 取**输入水位** `InHi`（不是 `LastEv`）：`InHi` 之后可能有已进内存队列、尚未处理的输入，重放必须从这里读起（原型第一版取了 max，评审时改掉）。
- **清理**：`purge(filter=<p>.<k>.>, seq < min(InHi+1, LastEv, 参与方消费者 ack floor+1))`。保留最后一条事件：期望序号按过滤上的最后一条比较，
  删光了会变回 0，单写者检查就失效了。参与方还没 ack 的命令不删。
- **限制**：KV 值上限（原型桶 8 MiB）限制快照大小。终态 saga 在内存与快照里只留保留期，之后的查询与业务键唯一性要靠投影（§12）；
  非终态 saga 很多时要分块快照（对象存储 + KV 里放清单），原型没做。

## 9. 定时器

每分区一个哈希时间轮（5ms 槽、1024 槽），在分区循环里推进（无锁）。条目只是“到时看一眼”：状态机按 saga 当前的截止 / 退避时刻判定，陈旧条目不改状态。
到期产生的转换（超时 → 退避 / 放弃，退避到期 → 下一次尝试）与输入一样经事件提交。原型第一版的轮把“当前 tick 里还没到期”的条目推迟了一整圈（5.1s），
迟到成功用例 8 次红 3 次（原始输出见报告 §5），修正后 8/8 绿，并加了单元断言。

## 10. 故障表

| 故障 | 行为 | 验证 |
| --- | --- | --- |
| 协调器进程 kill -9 | 已 PubAck 的事件在流里；内存丢失。新实例等租约过期（≤ TTL）后接管、重放；崩溃前已处理但没提交的输入在 `InHi` 之后，重新处理 | `TestCoordinatorKill9Takeover`：400 笔、第 150 笔后杀，B 接管（含 ≤3s 租约等待）后 7.4s / 3.4s（两次运行）内全部到终态，状态 / 余额 / 事件链核对通过 |
| 旧协调器还活着（停顿、分区） | 它的下一次发布被期望序号拒绝（10071），分区 fail-stop | `TestFenceRejectsStaleWriter`；变异去掉期望序号后事件链分叉（见下） |
| 参与方 kill -9 | strict 已确认的提交在 WAL；重启从 WAL 重建；未 ack 的命令重投 → 回放回执；协调器超时后的新尝试同样回放 | `TestParticipantKill9Recovery`：300 笔、第 100 笔后杀，重启后全部到终态，余额逐账户一致 |
| 重复命令（重投） | 回执回放，不重复执行 | `TestEndToEndDuplicateCommandsAndResults`（30% Nak）；变异去掉回执检查后余额不符 |
| 重复 / 迟到结果 | 操作已带结果关闭 → 重复；放弃之后的正向成功 → 补偿该步（终态重开）；补偿方向只告警 | 同上（30% 重复结果）；`TestEndToEndLateSuccessReopensAndCompensates` |
| 发布结果未知（PubAck 超时、断连） | 同一 MsgID、同一期望序号重发，按 §2 判定；30s 仍未知 → 分区停 | 语义用例（ack 丢失、leader 切换后判 Duplicate）；`TestPublishOutcomeUnknownIsResolved`：三分之一的发布“发出但丢 PubAck”，单条判 `Duplicate`、批判 10201 / 最后一条是自己的，两条分支都命中，状态 / 余额 / 事件链一致 |
| 期望序号冲突 | 读过滤上最后一条：MsgID 是自己的 → 已提交；否则 `ErrFenced` | `TestFenceRejectsStaleWriter` |
| JetStream leader 切换 | 已 PubAck 不丢；切换期间（实测 5～7s）发布超时，按“结果未知”重试 | `TestLeaderKillKeepsAckedWritesAndResolvesUnknowns` |
| 参与方的结果发布失败 | 重试到成功才 ack 命令；进程退出则命令重投、回放 | 代码路径；未单独注入 |
| 快照写入冲突 / 清理 | 快照 CAS + 只向前；清理不越过快照水位、最后一条事件与参与方 ack floor | `TestSnapshotPurgeReplayMatches`：清理后 B 的重建逐笔等于 A 的内存，B 继续服务 |
| 整集群同时断电 | 可能丢最近的已确认消息（前提 1 承担） | 不处理 |

## 11. 与现行 saga 语义的对应

| 现行（`docs/framework/SAGA.md`） | 本设计 |
| --- | --- |
| 状态与待发布命令在一个存储事务 / durable outbox | 一条事件同时是新状态和命令；没有 outbox、没有发布器 |
| `version + lease token` 阻止过期 worker | 分区单写者 + 期望序号；saga 内版本只用于重放时“只前进” |
| completion 回执（按 CommandID）与推进同事务 | 结果是输入消息；接收结果的转换在同一事件里写 tombstone（`closed[op]`）；重复结果由 tombstone + 流内 MsgID 去重 |
| 操作 tombstone、迟到成功补偿（方向 ④）、补偿方向只告警 | 相同规则（`machine.complete`，对应 `judgeCompletion`），单元 + 端到端用例 |
| 跨尝试至多一次：操作状态文档 + Reserve + lease fence + 实体屏障 | 实体里的操作回执 + 实体锁串行；执行前查命令截止。**差别**：现行的 lease fence 让截止后才投影的记录被跳过；这里截止检查在事务开始时，WAL 落盘可能晚于截止 → 只能靠迟到成功补偿（规则相同，频率取决于截止相对 fsync 的余量） |
| 退避中送达的成功接收、拒绝 / 较早尝试的可重试失败不接收 | 相同 |
| 步骤预算、`ManualRequired` | 相同预算模型；`Resume` / 代际（`Incarnation`）、人工 `Compensate`、定义版本、业务键查询**原型没做** |
| 启动意图与 Nest 事务原子（`EmitStart`） | 原型由发起方直接写 `in.start`；生产应由 Nest effect 发出（或 Nest 事务后发布 + MsgID 去重） |
| `Engine.Get` / `List` / `GetByBusinessKey` | 分区所有者内存可答；跨实例与终态历史要靠投影（§12） |

## 12. Mongo 投影接法（未实现）

一个独立的投影消费者（durable，过滤 `<p>.*.ev.>`）按分区顺序把事件里的 saga 状态 upsert 进 `_sagas`（版本只升不降，与 v2 Store Service 相同的单调写），
批量 BulkWrite、进度就是消费者 ack floor。它不在关键路径上；落后只影响查询新鲜度。清理守卫里加上这个消费者的 ack floor，就保证“先投影后清理”，
终态 saga 从内存 / 快照里过保留期后由 Mongo 回答查询与业务键唯一。

## 13. 对 Remote 的可推广性

Remote 实体的“单写者 + 版本 fence”与这里同构：每个 Remote 实体（或实体分片）一个 subject，写入带该 subject 的期望序号即是版本 CAS，
多实体原子写用原子批量（每条带各自的期望序号，批内按批前状态比较——§2 实测）。难点不在 JetStream：Remote 读需要“最新值”（KV / 直接读
最后一条），回放式重建对热点实体的读延迟、以及与 Nest 本地 WAL 的两阶段边界都要单独设计。本轮只证明了原语可用，没有原型。

## 14. 已知风险与没做的

- **单流的上限**：所有分区共用一条流 = 一个 Raft 组、一个 leader；吞吐与选主停顿都按流计。原型支持把分区按 `k % G` 分到 G 条流
  （`Layout.StreamGroups`，subject 与单写者检查不变；参与方与终态观察者在每条流上各建一个消费者），`TestEndToEndAcrossStreamGroups` 验证功能，压测对照见报告。
- **快照大小**受 KV 值上限约束（§8）；分块快照没做。
- **事件携带完整 saga 状态**：`Data` 大时日志膨胀；可改为增量，但重放要能从快照 + 增量还原。
- **macOS 测量**：参与方 WAL 的 fsync 在 macOS 上是 F_FULLFSYNC；JetStream 在本机三节点同盘。Linux 未测。
- 原型省掉的段与对数字的影响见报告 §1。
- 没有：Resume / 代际、人工补偿、定义版本、业务键唯一（跨保留期）、Mongo 投影、多参与方服务与按 sid 路由、App 级 fail-stop 接线、指标。

## 15. 位置与运行

```bash
export SAGAJS_ROOT=<私有目录>  # 数据与日志
scripts/perf/saga-jslog-proto/nats-cluster.sh start     # 私有三节点（缺省 25622-25624）
export SAGAJS_URL=nats://127.0.0.1:25622,nats://127.0.0.1:25623,nats://127.0.0.1:25624
GOWORK=off go test -race -count=1 -v ./scripts/perf/saga-jslog-proto/...      # 语义 + 正确性 + kill -9
SAGAJS_MUTATE=nofence   GOWORK=off go test -count=1 -run TestFenceRejectsStaleWriter ./scripts/perf/saga-jslog-proto/        # 必须失败
SAGAJS_MUTATE=noreceipt GOWORK=off go test -count=1 -run TestEndToEndDuplicateCommandsAndResults ./scripts/perf/saga-jslog-proto/  # 必须失败
scripts/perf/saga-jslog-proto/nats-cluster.sh clean
```

## 16. 全面实施的工作量与风险（估计）

| 块 | 内容 | 估计 |
| --- | --- | --- |
| 协调器 | `framework/saga` 新的分区运行器（取代领取 / outbox / 发布器）；状态机直接复用 `judgeCompletion`；Resume / 代际 / 人工补偿 / 定义版本 / `ManualRequired` 接入；快照分块（对象存储 + KV 清单） | 大（约 1.5～2 周等效） |
| 所有权与 App | 分区租约挂到 App 级单实例 / fail-stop（`RuntimeFailure`），多实例分摊分区 | 中（3～5 天） |
| 参与方 | 原生步骤：实体内回执需要生成器支持（DAO 的回执字段与补丁路径、保留期清理），替换 `_dataengine_step_operations` + lease fence + 实体屏障；按 sid 路由（`ev.<service>.<sid>` 与每 sid 消费者）；Mongo 步骤收件箱保留 | 大（约 1.5 周） |
| 投影与查询 | 事件 → Mongo `_sagas` 的批量单调投影、清理守卫加投影消费者、`Get` / `List` / `GetByBusinessKey` | 中（约 1 周） |
| 装配 / 配置 / 生成物 / 文档 | wiring、配置 schema、`roost add saga` 模板、game-demo、SAGA.md / impl 篇、指标与仪表 | 中（约 1 周） |
| 验收 | Linux 跨机三节点、leader 切换与网络分区、长稳、投影积压、与现行版本同口径对照 | 中（约 1 周） |

主要风险：单流 Raft 组的吞吐与选主停顿（用多流缓解，跨机未测）；前提 1 的断电窗口（服务端不逐条 fsync）；快照大小与 `max_payload`；
事件携带完整状态导致日志膨胀；清理受最慢的参与方消费者牵制（卡住的消费者会让日志一直长，需要告警与上限策略）；截止检查代替 lease fence 后迟到成功的频率；
saga 是已知反复出问题的模块，换掉整条协调路径会让现有的回归测试大面积失效，需要按规则逐条迁移。

## 17. 需要维护者决定

1. **是否进入实施评审**。A（推荐）：保留 Redis 2c 现状不动，先在原型上补“异步 Mongo 投影同时运行”与 Linux 跨机复测，数字仍成立再出函数级实施设计；
   B：直接按本稿实施并替换 Redis 权威路线（代价：2a / 2b / 2c 的工作作废、saga 回归测试大面积迁移）；C：不采用，原型只作参考。
2. **参与方回执放在哪里**。A（推荐）：放在实体里（实体锁串行化同一操作的尝试，一条 WAL 记录，不需要操作状态文档与 lease fence）；B：保留现行操作状态文档（Mongo 事务在关键路径上）。
3. **流的划分**。A（推荐）：按分区组分 4～8 条流（选主停顿与吞吐按流隔离）；B：单流（简单，故障域是全部分区）。
4. **终态通知**：`ev.done` 事件可以直接作为对业务的终态通知（现行没有终态推送），是否作为公开接口。推荐作为可选能力，先不承诺格式稳定。
