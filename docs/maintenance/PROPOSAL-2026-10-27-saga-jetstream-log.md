# Saga 协调器以 JetStream 为权威日志：设计稿与原型（2026-10-27）

**状态：原型（第二轮：维护者 2026-10-27 决定按推荐，补了投影打开的实现与压测，见 §12、§17）。** 代码在 `scripts/perf/saga-jslog-proto/`（实验包，不进生产路径，不改 `framework/saga` 的 Store / 引擎 / wiring），
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
5. **第二轮（投影打开，维护者决定 1A / 2A / 3A 后）**：参与方接现行 DataEngine Mongo 投影、协调器事件写 saga 冷副本，缺省 4 条流。
   积压按笔折算为在途的 0.98～2.06 倍（只有一档 2.06，同档另一样本 1.22）、停止加压后 44～88ms 归零、冷副本与 Mongo 账户 0 差异；
   kill -9 后冷副本最终一致（冷副本投影第一版 AckAll 丢版本，已改逐条 ack，§12）。同盘口径下吞吐明显下降（正向 c128 约 2660 → 600～810 TPS，
   c1 P50 约 11 → 45ms），诊断确认是 Mongo 刷盘与 NATS / WAL 同盘争用，不是投影进了关键路径（报告第二轮）。
6. **第三轮（缺省配置：参与方投影开、冷副本关；云 Mongo）**：本机同盘正式口径正向 c128 约 1650～1790 TPS、c1 P50 约 30ms，余额 / 复读 / Mongo 账户全部通过，
   但积压按笔门槛有 6 档短时峰值超过 2 倍（追平只要 40～100ms）。云 Mongo（本轮 RTT 约 8ms）上关键路径回到不投影时的量级（c1 P50 11ms、c128 约 1960～2690 TPS），
   而参与方投影器持续上限约 500～590 条/s（≈ 250～290 笔/s），c16 起积压持续增长；同时段现行基线 main / 2c 原生正向约 5.2～5.3 TPS。云上第一次 c128 的余额核对与积压读数各有一处
   对不上，尚未确认（报告第三轮 T0 第 5 条）。
7. **第四轮（按投影积压准入，§12a）**：参与方经 KV 发布投影积压，协调器带滞回判定、只拒新 saga 的启动（终态 Rejected / 发起方 ErrBackpressure，可重试），
   信号过期也关闭准入，参与方 MaxUnackedRecords 作硬上限；准入、过期、硬上限三个用例与 noadmit 变异通过 / 变红。硬上限用例顺带找出原型 handler 回执逆操作被去重丢掉的缺陷，已修。只做功能与短测。
8. **第五轮（并行投影器 + 积压准入联合压测，§12a 推荐参数）**：云上（RTT 64ms、8 通道）准入开时积压有界（峰值约 2850～3050 笔、2.8～3.5s 追平），
   有效持续吞吐约 690～735 笔/s（= 投影器速率），准入关时积压 6～13 万条、追平 53～107s；本机投影器跟得上关键路径，准入从不关闭，持续吞吐由关键路径决定（本轮约 1150～1210 笔/s）。
9. **建议（第一轮）**：方向可行，值得进入实施评审；先补两项再决定是否替换 Redis 权威路线——异步 Mongo 投影在压测中同时运行（量持续吞吐与积压），以及 Linux 跨机三节点复测（§16）。

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

- saga ID → FNV-1a → 分区 `k ∈ [0, P)`。分区按 `k % G` 分到 G 条流 `SAGAJS_<PREFIX>_G<g>`（**缺省 G = 4**，维护者 2026-10-27 决定 3A；
  分区不足 4 个时取分区数；`StreamGroups = 1` 是单流 `SAGAJS_<PREFIX>`，保留为可选），每条流的 subject 是它那几个分区的 `<prefix>.<k>.>`，
  R3、文件存储、`AllowAtomicPublish`、去重窗口 2 分钟。单写者检查的过滤与 subject 不随流数变化；参与方、终态观察者、冷副本投影在每条流上各建一个消费者。
- subject：

| subject | 写者 | 读者 |
| --- | --- | --- |
| `<prefix>.<k>.ev.acct` | 分区协调器（事件 + 发给 acct 参与方的命令） | 参与方 durable 消费者（过滤 `<prefix>.*.ev.acct`）；重放 |
| `<prefix>.<k>.ev.done` | 分区协调器（终态事件） | 发起方 / 业务的有序消费者（过滤 `<prefix>.*.ev.done`）——终态通知天然存在；重放 |
| `<prefix>.<k>.ev.none` | 分区协调器（无命令的状态变化，如进入退避） | 重放 |
| `<prefix>.<k>.in.start` | 发起方（MsgID = `start.<id>.<Client 实例随机标识>.<实例内序号>`，全局唯一，§12b） | 分区协调器（过滤 `<prefix>.<k>.in.>`） |
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
| 投影打开时协调器 kill -9 | 冷副本投影在协调器进程里一起被杀；B 进程用同一 durable 接着投影，旧实例在途未 ack 的事件 AckWait 后重投 | `TestCoordinatorKill9TakeoverProjected`：冷副本在最后一个终态后 5.6s 与 400 笔终态逐笔一致，Mongo 账户与内存一致（AckAll 版本红，见 §12） |
| 投影打开时参与方 kill -9 | 重启先把 WAL 尾巴投影进 Mongo，再从 Mongo 加载；未 ack 的命令重投回放回执。被杀进程正在做的批量投影事务留在服务端，到 `transactionLifetimeLimitSeconds`（缺省 60s）才回收，期间重启的排空会超时——第四轮 `-race` 全套里撞到一次：`drain WAL into mongo: dataengine projector: segment … records=21: context deadline exceeded`，进程起不来、用例 3 分钟超时。改为排空失败就重试（上限 3 分钟，先恢复再服务），之后 3 次连跑通过；这是 SAGA.md 已记的“强杀遗留 Mongo 事务”在原型里的同一现象 | `TestParticipantKill9RecoveryProjected`：重启恢复 30ms，Mongo 账户与预期余额逐账户一致，冷副本 0 差异 |
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

## 12. Mongo 投影（第二轮已实现，`sagaproj.go`、`participant.go`）

两边都不在关键路径上。**缺省配置（维护者 2026-10-27 第三轮决定 A）：参与方投影开（它是实体的正式持久化路径）；saga 冷副本关、可选开启。**
开启冷副本时建议：只投影终态（`SagaProjector.TerminalOnly`，过滤 `<p>.*.ev.done`，写入量按笔一次，重开后再到终态再一次；在途 saga 的查询由分区所有者内存回答）、
更大的批、或放在单独进程里，避免和协调器 / 参与方抢同一块盘与同一个进程（第二轮实测同盘时冷副本是退化的主要来源）。
冷副本关闭时清理守卫不等它（`TestParticipantProjectionWithoutColdCopy`）。

- **协调器 → saga 冷副本**（`SagaProjector`，可选）：每条流一个 durable 消费者（过滤 `<p>.*.ev.>`），一批至多 256 条、最多等 50ms，同一 saga 只留版本最大的一条，
  一次无序 BulkWrite 写进 `<库>.sagas`；每条写是“版本更大才整文档替换”的管道更新（upsert），重投、乱序、多实例同时投影都幂等。写成功后**逐条 ack**。
  清理守卫带上它的 durable 名，保证“先投影后清理”；终态 saga 过了内存保留期由冷副本回答查询与业务键唯一（查询接口原型没做）。
  - **实测**：第一版用 AckAll（只 ack 本批最后一条）。协调器进程 kill -9 后，新实例 ack 本批最后一条把旧实例取走、没 ack、正在等 AckWait 重投的事件一并确认掉，
    冷副本缺版本：`TestCoordinatorKill9TakeoverProjected` 报 `cold copy differs from the observed terminal states (22)`，例如 `k-121: final=k-121 v3 completed … cold={ID:k-121 Version:2 Status:running}`、
    `k-131: … cold={ID: Version:0 Status:}`。改逐条 ack 后通过（冷副本在最后一个终态之后 5.6s 追平，主要是 AckWait 10s 内的重投等待）。
  - **实测**：消费者状态改为 R1、内存存储（进度丢了只是从流里还在的事件重投一遍，幂等）：每条 ack 不再是一次 R3 共识写。代价：服务端重启后从头重投；
    清理守卫找不到它时不清理。批等待 20ms 时 Mongo 提交太碎（参与方 WAL 与 JetStream 发布都更慢），100ms 时 c16 积压到在途的 2.6 倍，取 50ms。
- **参与方 → 实体文档**：现行 DataEngine 路径（`engine.NewMongoStore` + `engine.DefaultProjectorOptions()` 的 Projector）异步投影，ack 后 WAL 可裁剪。
  重启按现行顺序：先 `Flush` 把 WAL 里未投影的尾巴写进 Mongo，再从 Mongo 加载实体（版本取 `_version`）。差异：不经代码生成的 Repository / 加载器，
  直接 `MongoStore.Load` 后按手写 DAO 解码；没有 DataEngine 的 Runtime / outbox（原型没有 effect）。
- 积压按“笔”判定（维护者 2026-10-27 确认的口径）：参与方未确认记录 ÷ 每笔记录数（正向 2、补偿 3）、冷副本未投影事件 ÷ 每笔事件数（正向 3、补偿 4），
  取大者与在途笔数（闭环并发）比较，门槛 ≤ 2 倍；停止加压后 30s 内归零；冷副本与终态、Mongo 账户与内存逐笔 0 差异。结果见压测报告第二轮。

## 12a. 按投影积压准入（第四轮已实现，`admission.go`）

维护者 2026-10-27 选了第三轮的 A：参与方投影在关键路径之外，关键路径吞吐可以远高于投影器（云上实测约 500～590 条/s），所以要按投影积压对**新 saga**做准入。
投影器按实体分片并行是生产 DataEngine 改动，另一分支在做；本节只是原型上的准入。

- **信号来源**：参与方 DataEngine 投影器的 `ProjectorStats.WALUnacked`（已准入、尚未投影确认的记录数）。协调器按“记录数 ÷ 每笔记录数”（两步正向 2，可配置）折算笔数，
  取全部参与方的最大值。没有用 Mongo 侧的任何读数：积压只有参与方自己知道。
- **怎么让协调器知道**：参与方每 `LoadTick`（缺省 200ms）把 `{records, at}` 写进 KV 桶 `<STREAM>_LOAD` 的 `part.<参与方名>`（桶 MaxAge 30s，进程死掉后条目最终消失）。
  协调器 watch 整个桶。**过期**按写入时刻判断（不等 MaxAge）：任何一个见过的参与方超过 `StaleAfter`（缺省 2s）没更新 → 准入关闭，原因 `backpressure_stale_signal`；
  条目被删除 / 过期清除按“下线”处理，不再计入。**初始化 fail-closed**（RV-20261028-03，2026-10-28 修）：协调器 watch 的初始快照（桶里已有的全部信号）读完之前准入关闭，
  原因 `backpressure_not_ready`——这时空的信号表只说明“还没读到”，不说明“没有积压”；初始快照读完且确实没有任何参与方信号时才打开（没有要保护的投影），
  读完时已有超过高水位的积压则直接按积压关闭。单独一个原因而不沿用 stale：stale 是某个参与方停更、要排查的故障，not_ready 是启动 / 接管期的正常短暂状态；
  两者都以 `backpressure` 开头、同样可重试。接管（`Takeover`）不等初始快照，在途 saga 的重放与推进照常进行，只有新 saga 的启动在这段时间被拒。信号停更可能是参与方卡死，也可能只是发布失败，两种都按关闭处理：宁可少接新单，
  不让积压在看不见的地方增长（这是一个产品取舍，推荐如此）。
- **阈值与滞回**：笔数 ≥ `HighSagas` 关闭，关闭后降到 ≤ `LowSagas`（缺省 High 的一半）才重新打开。信号恢复新鲜但积压仍在滞回区间时按积压判定。
- **只作用于新 saga 的启动**：关闭时协调器把启动意图写成终态 `Rejected`（`Reason` = `backpressure` / `backpressure_stale_signal` / `backpressure_not_ready`），不派发任何步骤；
  在途 saga 的推进、重试、补偿一律不受影响——它们是消化积压的唯一途径。被拒的 ID 可以再次发起（版本接着往上走；启动尝试身份全局唯一，换了 Client 也不会被流的去重窗口吞掉，§12b）。
- **对业务返回什么**：协调器把准入状态写进同桶的 `admit` 键（变化时立即写、否则每秒刷新）；发起方 `Client.Start` 看到 3s 内的“关闭”直接返回
  **`ErrBackpressure`（可重试，不写启动意图）**；它只是提示，协调器处理启动意图时再判一次才是权威（终态 `Rejected`、原因以 `backpressure` 开头，同样按可重试处理）。
  对应现行 Redis 权威的 `ErrBackpressure`（`max_projection_backlog`）。
- **最后一道硬上限**：参与方 `ProjectorOptions.MaxUnackedRecords`（`ParticipantOptions.HardLimit`）——积压到这里 DataEngine 在 WAL 准入前拒绝（`ErrProjectionBackpressure`），
  Nest 整体回滚、命令 Nak 重投，协调器按步骤预算重试。它会拖慢在途 saga，所以：硬上限 ≫ `HighSagas × 每笔记录数 + 在途笔数 × 每笔记录数`；步骤预算（`Timeout × MaxAttempts`
  加退避）要覆盖积压从硬上限回落的时间，否则在途 saga 用尽重试走补偿（补偿同样要写参与方，会一起被拒，最后停在 ManualRequired）。
- **验证**（本机隔离 Mongo，参与方投影经“写入闸门”包装，闸门关上时投影阻塞、积压确定地上涨）：
  - `TestAdmissionClosesOnProjectionBacklogAndReopens`：积压到约 46～48 笔（阈值 40）准入关闭；发起方 `Start` 返回 `ErrBackpressure`；绕过提示写的意图终态 `Rejected`/`backpressure`；
    准入关闭时仍在途的 8 笔（每步结果晚 2s）全部 `Completed`；放开投影后积压回落、准入重新打开，被拒的 ID 再发起全部完成；余额、Mongo 账户、事件链核对通过。3 次连跑通过。
  - `TestAdmissionClosesWhenTheBacklogSignalGoesStale`：参与方停更信号 → 关闭（`backpressure_stale_signal`）、启动被拒、在途照常完成；恢复后重新打开。
  - `TestHardLimitRejectsStepsUntilProjectionCatchesUp`：不开准入、硬上限 20 条，闸门关 1s：约 235～251 次步骤被拒后重试成功，30 笔全部完成、余额与 Mongo 一致。
  - `TestAdmissionStaysClosedUntilTheInitialSnapshotIsRead`（单元）与 `TestCoordinatorRejectsStartsUntilAdmissionIsReady`（协调器经 `AdmissionReadyGate` 受控地拖住初始快照）：
    快照读完之前启动被拒（`backpressure_not_ready`、不派发步骤），放开后快照为空、同一 ID 再发起被放行。修前在 `4170bcad` 上的红：
    `before the initial snapshot: open=true reason="", want closed/backpressure_not_ready`、`a new saga was admitted (1 starts) before the initial backlog snapshot was read`。
  - **变异** `SAGAJS_MUTATE=noadmit`（协调器不查准入）：两个准入用例都失败——`start of rej-0 while backlogged: rej-0 v3 completed … want rejected/backpressure`、
    `start of stale-0 with a stale signal: stale-0 v3 completed …`。
  - **硬上限用例找出原型自己的缺陷（先红后绿）**：参与方 handler 用同一个 field 给余额和回执登记逆操作，`RecordUndo` 按（owner, field）去重，回执那条被丢掉；
    事务被硬上限拒绝回滚后余额复原、回执留在内存，重投回放“成功”而余额没改：`balances do not match the terminal states (sum 15999979, want 16000000)`（修后把登记改回旧写法再跑一次复现：
    `sum 15999987`）。回执改用 `RecordUndoToken`（单独 field、按 opKey 作 token）后 3 次连跑通过。前三轮没有触发回滚的路径，所以没暴露。
- **推荐参数（第五轮联合压测，维护者 2026-10-28：并行投影器在 main `e11506a4`，参与方缺省 8 通道；同意“信号过期即关闭准入”的缺省）**：

  | 参数 | 推荐值 | 依据 |
  | --- | --- | --- |
  | `HighSagas` | **R × T ÷ 每笔记录数**；云上（RTT 约 64ms、8 通道）取 R = 1000 条/s（实测 1310～1450 条/s 取保守值）、T = 5s → **2500 笔** | 高水位 = “投影器持续速率 × 允许的追平时间”。实测积压峰值 2844～3053 笔、停止加压后 2.8～3.5s 追平，都在 T 内 |
  | `LowSagas` | **High ÷ 2（1250 笔）** | 关闭后要追平一半才重开：云上 c64 / c128 准入关闭时间占 55% / 72%，有效吞吐稳定在投影速率附近，没有抖到频繁开关 |
  | 信号发布间隔 `LoadTick` | **200ms** | 越过高水位后的超调 ≈ 输入速率 × (发布间隔 + 判定间隔 100ms + 发起方看到 admit 的延迟)；实测超调约 340～550 笔（高水位的 14～22%），硬上限里留了余量 |
  | 过期时长 `StaleAfter` | **2s（= 10 × 发布间隔）** | 允许连续丢 9 次发布不误关；参与方真停更时 2s 内关准入 |
  | 硬上限 `MaxUnackedRecords` | **4 × High × 每笔记录数 = 20000 条** | 积压到它要约 20s 才能投完（按 R = 1000）；步骤预算 5s × 5 次加退避约 30s 覆盖得住。实测峰值 6106 条，没碰到 |

  阈值随部署变：R 用“投影器持续速率”的保守值（同网络、同通道数下实测），T 是业务能接受的“Mongo 落后于内存”的最长时间。本机（Mongo 同机、R 远高于关键路径）按这组参数准入从不关闭。
- 原始输出：[round4-admission](../performance/benchmarks/saga-jetstream-log-proto-20261027/round4-admission/)（`-race` 全套、noadmit 变异、两处先红后绿）。
- **没做**：准入状态的指标与告警、按参与方 / 按 saga 类型分别准入、在 Engine 层面的 API 形状；信号桶的跨进程发布节奏在正式压测里再定。

## 12b. 发起方：启动尝试身份与等待者（2026-10-28 修，`client.go`）

分支 `fix/core-game-server-review` 上的审查登记了三项（RV-20261028-01/02/03），维护者 2026-10-28 让在 main 上修；03 是准入初始化，见 §12a。

- **启动尝试身份必须全局唯一（RV-20261028-01；正式实施沿用这条设计约束）**。启动意图的 MsgID = `start.<saga ID>.<Client 实例随机标识>.<实例内序号>`，
  实例标识是 `NewClient` 时生成的 128 位随机串（`crypto/rand.Text`）。原来只有 `start.<id>.<实例内序号>`：流按 MsgID 在 2 分钟去重窗口内去重，
  Client 重建（发起方进程重启）或另一个 Client 的序号都从 1 开始，被拒（`Rejected`）之后用同一个 saga ID 重试时 MsgID 和旧尝试相同，意图被流吞掉、
  PubAck 照样成功（`Duplicate` 没检查），等待者永远等不到。
  - 同一次 `StartIntent` 里结果未知的网络重发沿用同一个身份，流去重保证只追加一条（重发拿到 `Duplicate` 是正常结果）。
  - **第一次发布就拿到 `Duplicate`** 说明身份撞了、意图没入流：返回 `ErrStartIdentityReused`，撤掉等待者，不当成功返回。之前有过结果未知的发送时不做这个判断。
    正常运行中不会出现；它是身份生成坏掉时的显式失败，代替“等待者永远挂起”。
  - 同一 saga ID 是否已经在跑由协调器判定（`DupStarts`），不靠 MsgID；MsgID 只负责“一次尝试只进流一次”。
- **等待者（RV-20261028-02）**：原来 `wait[id] = ch` 覆盖同 ID 在途调用的通道，终态只关闭最后一个，前面的调用者永远等不到。现在同一个 ID 的每次 `StartIntent`
  都登记自己的通道，下一个被观察到的终态唤醒该 ID 的全部等待者。观察表只接受版本更大的终态（`old.Version >= ev.Version` 丢弃），被拒之后**不再删掉**观察表里的旧
  `Rejected`，所以旧一轮的拒绝回执（版本不大于已观察到的）不会结束新一轮。`ctx` 只管发布：发布失败（返回错误）时撤掉本次登记的等待者；返回通道之后调用方不等了
  不撤，终态到达时统一关闭并清理。已经到过非 `Rejected` 终态的 ID 直接返回已关闭的通道（原样）。
- **剩余局限（尚未确认，没有用例复现）**：新 Client 回放完历史终态之前对同一个 ID 发起，回放中的旧终态可能提前唤醒它（调用方看到旧的 `Rejected`，再发起一次即可；
  不丢意图、不多建 saga）。原型的 `NewClient` 不等回放追平；正式实施的发起方要在“回放追平”之后才接受发起，或者让终态带上触发它的启动尝试身份。
- **验证**（私有三节点集群，`-race`）：`TestRejectedStartRetriedByAnotherClientIsAppended`（重建 / 另一个 Client 同 ID 重试入流、得到新终态 v2 / v3）、
  `TestStartResendAfterUnknownOutcomeIsNotAppendedTwice`（两次回复丢失后重发，只追加 1 条）、`TestStartIdentityCollisionIsAnError`、
  `TestStartIntentWaiters`（先后两次、16 路同时、终态与发起竞争 20 轮、被拒后新一轮且旧回执重放一次）。修前在 `4170bcad` 上的红：
  - `start of same acknowledged but no terminal state within 10s; last start-intent seq 1 before the call, 1 after (unchanged = the intent was deduplicated, never appended)`（重建与另一个 Client 两种）；
  - `first publish acknowledged as a duplicate: err=<nil>, want ErrStartIdentityReused`；
  - `first caller stranded: the second StartIntent replaced its waiter`、`caller 0 of 16 stranded`、`round 0: caller 0 stranded`、
    `new round ended by the previous round's rejection receipt (v1)`。
  - 结果未知重发只追加一条的用例修前修后都通过（它守的是修法不能把同一次调用的重发也换身份）。
- **对已有压测数据**：没有影响，依据见压测报告第五轮之后的“2026-10-28 修复对压测数据的影响”。

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
- **投影与关键路径同盘**：投影打开后吞吐明显下降（报告第二轮），定位到 Mongo 三节点 journal 与参与方 WAL、JetStream 在同一块盘上互相拖慢
  （参与方 Nest 调用、JetStream 发布的平均耗时都成倍上升），不是投影阻塞了关键路径。生产要把 Mongo 与 NATS / 参与方 WAL 分盘或分机，本机无法验证。
- **参与方投影器的持续吞吐**：投影不在关键路径上，所以关键路径吞吐可以远高于投影器（云上约 500～590 条/s），积压会无限增长、追平前从 Mongo 读不到最新实体。
  原型已加按积压准入（§12a，功能验证，未压测）；投影器并行通道在另一分支做。
- **发起方回放追平前的提前唤醒**（§12b，尚未确认）：新 Client 回放完历史终态之前发起，旧终态可能提前唤醒等待者；只影响等待语义，不丢不重。
- 没有：Resume / 代际、人工补偿、定义版本、业务键唯一（跨保留期）、查询接口、多参与方服务与按 sid 路由、App 级 fail-stop 接线、指标。

## 15. 位置与运行

```bash
export SAGAJS_ROOT=<私有目录>  # 数据与日志
scripts/perf/saga-jslog-proto/nats-cluster.sh start     # 私有三节点（缺省 25622-25624）
export SAGAJS_URL=nats://127.0.0.1:25622,nats://127.0.0.1:25623,nats://127.0.0.1:25624
GOWORK=off go test -race -count=1 -v ./scripts/perf/saga-jslog-proto/...      # 语义 + 正确性 + kill -9（投影用例要 SAGAJS_MONGO_URI，否则跳过）
# 投影用例 / 压测：持共享隔离环境的 remote-acceptance.lock，把 env.sh 里的 Mongo 地址转给 SAGAJS_MONGO_URI（不打印），只用 sagajs_<前缀> 库、用完删除。
# kill -9 用例的子进程就是测试二进制本身（TestMain 见 SAGAJS_ROLE_ARGS 走进程入口，roles_test.go）。
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

## 17. 维护者决定（2026-10-27）

维护者对下面四项“全部按推荐”：

1. **1A**：Redis 2c 现状不动；原型先补“异步 Mongo 投影同时运行”的压测（本轮完成，见 §12 与报告第二轮），Linux 跨机复测等有机器再做。
2. **2A**：参与方回执放在实体内（实体锁串行化同一操作的尝试，一条 WAL 记录，不需要操作状态文档与 lease fence）。
3. **3A**：按分区组分 4～8 条流；原型缺省改为 4 条流，单流保留为可选（§3）。
4. **终态通知**：`ev.done` 作为可选能力，格式先不承诺稳定。

第三轮（同日晚）：

5. **A**：协调器侧 saga 冷副本投影改为缺省关闭、可选开启（开启时建议只写终态、批更大或单独进程，§12）；参与方侧 DataEngine Mongo 投影保持开启。
   缺省配置、压测脚本（`run-perf.sh matrix-part`，`SAGAJS_PERF_PROJECTION=part`）与本稿同步；冷副本用例保留，在“开启”配置下跑。
6. **B / C**：本机正式口径补一轮“参与方投影开、冷副本关”；云 Mongo 压测一次（只把参与方投影的库换成云库），与 2026-10-26 的云结果及同时段现行基线对比。结果见压测报告第三轮。

第四轮（同日夜）：

7. **按投影积压准入**（选第三轮 §6 的 A 的原型部分）：投影器“按实体分片并行”是生产 DataEngine 改动，另一个分支做；原型分支只做“按投影积压准入”，
   只做功能与短测、不做正式压测（正式的“并行投影器 + 准入”联合压测等那个分支好了再安排）。实现与验证见 §12a。

第五轮（2026-10-28）：

8. 投影器按实体分片并行已进 main（`e11506a4`，`ProjectorOptions.ProjectionWorkers` 缺省 8）；原型分支 rebase 到它，参与方用缺省 8 通道（`SAGAJS_PERF_PROJ_WORKERS=1` 留作对照开关）。
   维护者同意准入“信号过期即关闭”的缺省。用联合压测定准入参数（§12a）。
9. 第五轮交回后维护者决定原型分支并入 main（“目前的数据已经足够支持了”）：设计稿、五轮报告与样本、`scripts/perf/saga-jslog-proto/` 实验代码一起进 main，不碰生产路径；
   正式实施仍按 1A 等 Linux 跨机复测成立后再出函数级设计。

2026-10-28：

10. 分支 `fix/core-game-server-review` 上的审查对原型登记了三项（RV-20261028-01 启动 MsgID 跨 Client 重复、02 同 ID 等待者被覆盖、03 准入初始快照前放行），
    维护者核对后让在 main 上修（原型实验包，先红后绿）。修法与证据见 §12a、§12b；“启动尝试身份必须全局唯一”作为正式实施的设计约束沿用。

以下是第一轮交回时列出的选项原文，保留备查。


1. **是否进入实施评审**。A（推荐）：保留 Redis 2c 现状不动，先在原型上补“异步 Mongo 投影同时运行”与 Linux 跨机复测，数字仍成立再出函数级实施设计；
   B：直接按本稿实施并替换 Redis 权威路线（代价：2a / 2b / 2c 的工作作废、saga 回归测试大面积迁移）；C：不采用，原型只作参考。
2. **参与方回执放在哪里**。A（推荐）：放在实体里（实体锁串行化同一操作的尝试，一条 WAL 记录，不需要操作状态文档与 lease fence）；B：保留现行操作状态文档（Mongo 事务在关键路径上）。
3. **流的划分**。A（推荐）：按分区组分 4～8 条流（选主停顿与吞吐按流隔离）；B：单流（简单，故障域是全部分区）。
4. **终态通知**：`ev.done` 事件可以直接作为对业务的终态通知（现行没有终态推送），是否作为公开接口。推荐作为可选能力，先不承诺格式稳定。
