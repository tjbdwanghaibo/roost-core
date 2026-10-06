# Mongo 步骤延迟分析：时间花在哪里，能不能降（2026-10-06）

**来由**：维护者第十二轮决定，原话是“mongo 的延迟可以分析下”（[DECISIONS-PENDING](../review/DECISIONS-PENDING-2026-10-05.md) 末表“Mongo 步骤延迟”）。
saga 方向 ②（`9669d181`，[方案与实施](SAGA-DIRECTION-STEP-TRANSITION-AND-MONGO-INBOX-2026-10-06.md)）把 Mongo 步骤纳入操作实例收件箱后，
`MongoCommandInbox.Handle` 从一个事务变成两个：Reserve 事务和执行事务。实施时在真实副本集上测过单协程延迟，从 9.0 ms/op 涨到 17.4 ms/op。
**基线**：分支 `mongolat`，起点 `origin/main` `78e26853`；修前 = `9669d181^`（`a95cf4dc`）。代码图谱共享 generation 停在 09-30，`saga/command_consumer.go`、
`saga/dataengine_step_inbox.go` 为 `metadata_changed`，`saga/step_operation_inbox.go` 未入索引，本文以当前源码为准。

## 结论

1. **延迟几乎全花在多出的那一次持久提交上。** 修后每次尝试有 2 个事务，各有 1 次 `w:majority, j:true` 提交。数据命令从 4 条增加到 8 条，每条在客户端只要 0.1～0.3 ms，在服务端不到 0.6 ms。
   提交在服务端约 8.4 ms，其中约 8 ms 花在等写关注（`waitForWriteConcernDurationMillis`），而这段等待主要是日志刷盘：在本机上，`w:1, j:false` 提交只要 0.11 ms，`w:1, j:true` 就要 7～8 ms。
   修前每次 9.0 ms，其中提交 8.1 ms；修后 18.3 ms，其中提交 15.9 ms、数据命令 2.2 ms。
2. **吞吐同样减半，因为瓶颈是每秒能做多少次提交。** 1、8、32 个协程时，修后的 ops/s 分别是修前的 49%、50%、57%。
   折算成每秒提交数，修前和修后在 8 协程时都是约 600 次/s，32 协程时是 1.8k 对 2.0k 次/s。副本集每秒能做的提交数没有变，修后每次尝试要用两次。
3. **在不放松契约、不改回归断言的前提下，没有找到能减少提交次数的做法。** 契约要求同时满足两条：在途尝试租约有效时别人要等（第 2 条）；截止过后，即使旧尝试的事务还开着，下一次尝试也能接替它执行（第 3 条）。
   要让在途状态被别人看见，就得在业务事务之外先提交一次，因为 Mongo 事务未提交的写只能通过写冲突被感知，而写冲突又会挡住接替。
   合成一个事务的原型（spike，没有提交）把延迟和吞吐都拉回了修前水平（9.2 ms/op），但 ② 的契约用例在真实副本集上变红（见候选 1），这说明契约测试能抓到这种改法。
   降低 Reserve 的写关注可以把这次提交降到 0.1 ms，但有反例会回放一份最终没有生效的结果（见候选 4）。
4. **没有实施生产代码的改动。** 只新增了一个可复跑的分析基准文件 `saga/mongo_step_latency_real_mongo_integration_test.go`，以及本文和证据。
   是接受这份代价，还是改动契约换取单事务，**需要维护者决定**（见文末“需要维护者决定”）。

## 环境与方法

- **副本集**：私有三节点副本集（mongod 8.0.28，三个节点在同一台机器上，WiredTiger 缓存每节点 0.5 GB，端口偏移 24000），用 `kit/scripts/integration/lib` 的 `mongo_up` 起，不碰共享隔离环境。
  机器是 Apple M5 10 核，macOS，Go 1.27.0。测量期间别的 agent 在编译、跑测试，load average 在 3.6～6.7 之间，每轮前都记录了 `uptime`。
- **计时装饰器**：`BenchmarkRealMongoStepLatencyBreakdown`。它包住 `fmongo.IMongo`，按“所在事务 / 命令 / 集合”统计次数和客户端耗时，提交耗时取事务总耗时减去回调耗时。
  这相当于在 roost 的驱动接口层做 command monitoring；`mongo/driver` 没有暴露驱动的 monitor 选项，而改 `mongo/` 不在本次范围内。
- **服务端**：在主节点上把 `slowms` 设为 0（profile level 0），让所有命令写进诊断日志，再用 jq 按命令汇总 `durationMillis` 和 `waitForWriteConcernDurationMillis`。
  `commitTransaction` 是在 admin 库上执行的，库级 profiler 看不到它，所以用诊断日志。测完把 `slowms` 恢复成 100。
- **吞吐**：`BenchmarkRealMongoStepThroughput/g=N`。g 个协程从一个计数器领取下一次操作，每次都是新的操作实例，handler 在事务里插入一份业务文档。报告 ops/s 和单次 Handle 延迟的 p50、p99。
- **写关注**：`BenchmarkRealMongoCommitWriteConcern` 用裸驱动开单文档事务，读关注、读偏好和 `mongo/driver` 相同，只换写关注。
- **对照方式**：修前和修后各编译一个测试二进制，同机交替跑 6 轮，每轮一个进程，`-benchtime 1000x`，用 benchstat 比较。基准文件只依赖 `Handle` 的公开签名，原样复制到修前的源码树上就能编译。

## 时间分解

### 每次尝试做了什么（[breakdown.txt](evidence/mongolat/breakdown.txt)、[server-slowlog.txt](evidence/mongolat/server-slowlog.txt)）

| | 修前（一个事务） | 修后 Reserve 事务 | 修后执行事务 |
| --- | --- | --- | --- |
| 读回执 | `findOne steps` | `findOne steps` | — |
| 读自己的 claim | — | `findOne steps_claims` | — |
| 守卫 upsert | — | `findAndModify steps_claims` | — |
| 按操作查 claim | — | `find steps_claims` | — |
| 写 claim | — | `insert steps_claims` | `update steps_claims`（settleOwnClaim，条件写） |
| 业务写 | `insert business` | — | `insert business` |
| 回执 | `insert steps`（占位）+ `update steps` | — | `insert steps` |
| 提交（`w:majority, j:true`） | 1 | 1 | 1 |
| **合计** | 4 条命令 + 1 次提交 | 5 条命令 + 1 次提交 | 3 条命令 + 1 次提交 |

所有命令都只走一次网络往返（事务开始是附带在第一条命令上的，`EndSession` 只是把会话放回池里，不发命令）。
修前每次尝试 5 次往返，修后 10 次。守卫 upsert 每次 1 次；按操作查询每次 1 次，过期分支的 `operationSuccess` 另算。

### 各自耗时（n=6 的 benchstat，[throughput-benchstat.txt](evidence/mongolat/throughput-benchstat.txt)）

| 指标 | 修前 | 修后 | 变化 |
| --- | --- | --- | --- |
| 单次 Handle（顺序单协程） | 8.97 ms ±3% | 18.32 ms ±12% | +104% |
| 事务 / 次 | 1 | 2 | |
| 提交耗时 / 次（客户端） | 8.06 ms ±5% | 15.90 ms ±14% | +97% |
| 数据命令 / 次 | 4 | 8 | |
| 数据命令耗时 / 次（客户端） | 0.91 ms ±15% | 2.22 ms ±13% | +143% |
| 分配 | 598 allocs、53 KiB | 1337 allocs、114 KiB | +123% |

服务端口径（141 次 Handle，诊断日志）：`commitTransaction` 修前平均 8.34 ms，其中等写关注 7.67 ms；修后平均 8.44 ms，其中等写关注 8.04 ms，次数是修前的 2 倍。
数据命令在服务端都不超过 0.6 ms（大多 0.0x ms），客户端多出的那部分是往返和编解码。
另外，在负载更高时（load 6.6）单独采样的 [breakdown.txt](evidence/mongolat/breakdown.txt) 里，提交涨到 11～14 ms/次，构成不变。

### 一次提交为什么要 8 ms（[write-concern.txt](evidence/mongolat/write-concern.txt)）

| 事务写关注 | 提交耗时 |
| --- | --- |
| `w:majority, j:true`（roost 的设置） | 9.5～9.6 ms |
| `w:majority, j:false` | 8.2～9.2 ms（`writeConcernMajorityJournalDefault=true`，多数派确认仍要等日志落盘） |
| `w:1, j:true` | 7.4～8.2 ms |
| `w:1, j:false` | 0.11 ms |

只要要求日志落盘，提交就要 7～10 ms；不要求日志落盘时只要 0.1 ms。在同一块盘上，单次 `F_FULLFSYNC` 约 3.5 ms，普通 `fsync` 0.07 ms，`F_BARRIERFSYNC` 0.39 ms。
一次落盘提交大约相当于两三次 `F_FULLFSYNC`，WiredTiger 具体怎么调用没有逐一核对。三个节点在同一台机器上，所以复制本身几乎不花时间。
**这台机器放大了提交的绝对耗时。** 生产环境多半是 Linux 加 NVMe，再跨主机复制，一次落盘提交通常在 1～5 ms 量级（这里没有测）。
但修后每次尝试多一次落盘提交这个结构不变；只要提交耗时占大头，“单次延迟约翻倍、吞吐受每秒提交数限制”的结论在生产环境同样成立。

## 原生步骤为什么“没有”这笔代价

原生步骤其实也付这笔代价。`DataEngineStepInbox.Reserve` 调用的是同一个 `stepOperationInbox.reserve`，它就是上表的“修后 Reserve 事务”：5 条命令加 1 次落盘提交，在本机约 9～10 ms。
原生步骤之后的执行路径是：Nest 事务、本地 WAL 准入、投影，然后 `waitReplay` 以 25 ms 为间隔轮询回执，最后 ack。
它的生效点是投影事务对 claim 的条件写，而投影事务是批量的，一次提交由同一批的许多记录分摊。

| | 原生步骤 | Mongo 步骤（修后） | Mongo 步骤（修前） |
| --- | --- | --- | --- |
| 预约（在途可见） | Reserve 事务，1 次落盘提交 | Reserve 事务，1 次落盘提交 | 没有（回执占位在业务事务里，别人看不见） |
| 生效点 | 投影批量事务（分摊） | 业务事务（每次 1 次落盘提交） | 业务事务（每次 1 次） |
| 同一操作的跨尝试最多一次 | 有 | 有 | 没有（只保证同一命令最多一次） |

所以原生步骤并没有省掉预约这次提交，它省掉的是“每次尝试独占一次生效提交”。Mongo 步骤的业务写就在 handler 自己的事务里，这次提交没法和别的尝试分摊，而修前本来也要付。
② 新增的正好是原生步骤一直在付的那次预约提交。这次提交为什么是必要的，见下一节的论证。

## 必要性：在途可见和接替为什么需要两次提交

② 的契约（[SAGA.md](../../SAGA.md)「原生步骤执行契约」）与回归 `in-flight attempt past its deadline is taken over and cannot commit` 同时要求：

- (i) 尝试 k 的业务事务还开着、租约有效时，k+1 能看出 k 在途，因此不执行（返回在途错误并 nak）；
- (ii) 过了 k 的截止，即使 k 的事务还开着（handler 卡住，或者进程被杀、事务留在服务端），k+1 也能接替并执行，k 之后提交时被 fence 掉。

要满足 (i)，k 在途这件事必须在 k 提交之前就对 k+1 可见。Mongo 事务里未提交的写，别的事务读不到，只能通过写冲突感知。
所以只有两种做法：一是在业务事务之外先提交一次，也就是 Reserve；二是让 k 在业务事务里先写一份 k+1 也要写的文档，比如守卫。
如果用第二种，只要 k 的事务还开着，k+1 对这份文档的写就一直冲突，(ii) 做不到。
反过来，要 fence 掉迟到的 k，k+1 必须和 k 的生效写写同一份文档（现在是 supersede 与 settleOwnClaim 写同一个 claim），这样两者只能有一个提交。
于是 (i) 加 (ii) 就要求 k 在开业务事务之前，已经提交过一份“我在途”的记录。② 的方案里已经写过这个判断，这次用 spike 实测证实了。

## 并发吞吐（[throughput-benchstat.txt](evidence/mongolat/throughput-benchstat.txt)，n=6）

| 协程 | ops/s 修前 → 修后 | p50 修前 → 修后 | p99 修前 → 修后 |
| --- | --- | --- | --- |
| 1 | 111 ±5% → 54.5 ±3%（−51%） | 9.0 → 18.1 ms | 14.1 → 28.1 ms |
| 8 | 597 ±3% → 296 ±2%（−50%） | 12.9 → 26.2 ms | 22.7 → 39.1 ms |
| 32 | 1792 ±24% → 1022 ±7%（−43%） | 15.7 → 29.0 ms | 46.3 → 65.1 ms |

单协程延迟翻倍，并发时吞吐基本也减半。按每次尝试的提交数折算：修前 8 协程 597 次提交/s，修后 592 次/s；32 协程分别是 1.79k 和 2.04k 次/s。
这说明副本集在给定并发下每秒能做的落盘提交数是瓶颈。Mongo 的日志组提交在 32 协程时开始分摊（差距从 −50% 收窄到 −43%），但没有把差距抹平。更高并发（64 以上，单个 durable 的 `MaxAckPending` 缺省是 256）没有测。

**容量含义**：同一个副本集、同样的提交预算下，修后的 Mongo 步骤尝试吞吐约为修前的一半。在本机，8 协程约 300 次/s，32 协程约 1000 次/s。
saga 步骤是业务流程（发礼物、扣款），频率远低于 20Hz 的状态同步，交接文档没有为它设吞吐目标。如果某个业务的 Mongo 步骤量接近副本集提交能力，需要按业务量重新评估。

## 优化候选

### 1. 把 Reserve 合进执行事务（一个事务内先 claim 再执行业务写）——不采用，作为选项 B 交维护者

**原型**（spike，未提交，补丁见 [spike-merged-tx-contract-red.txt](evidence/mongolat/spike-merged-tx-contract-red.txt)）：`Handle` 只开一个事务，里面先跑 `reserveInTransaction`（守卫写在最前），再跑 handler、`settleOwnClaim`，最后写回执。
守卫写冲突时（错误码 112）映射成在途错误，用来 nak，而不是在 `WithTransaction` 里一直重跑到 `transaction_timeout`。

**收益（实测，3 轮）**：单次 9.20 ms/op（修前 8.97，修后 18.32）；吞吐在 1、8、32 协程时分别是 108、624、1431 ops/s，与修前统计上没有显著差别（[spike-merged-tx.txt](evidence/mongolat/spike-merged-tx.txt)）。

**正确性**：
- 最多一次、只在截止前生效：两条都成立。同一操作的尝试都写守卫，靠写冲突串行化；`settleOwnClaim` 在提交前仍检查租约，而租约封顶到截止。
- 接替：做不到。k 的事务开着时，k+1 的守卫写一直冲突。② 的契约用例在真实副本集上变红：
  ```text
  in-flight_attempt_past_its_deadline_is_taken_over_and_cannot_commit:
    attempt gift-1:1:1:2 after gift-1:1:1:1's deadline: {...} duplicate=false err=saga: another attempt of this step operation holds a live lease, want it to take over and execute
  ```
  这就是负对照：契约测试能抓到合并事务后“接替”丢失。mongotest 在写入时看不到别的未提交事务（快照加提交时检查），所以失败在前一步：
  `attempt gift-1:1:1:2 ran while attempt gift-1:1:1:1 was still in flight with a live lease`。
- 对运行的影响：进程还活着时，k 的事务会被命令截止（`processCtx`）中止，接下来的尝试只是晚一点开始。handler 卡在不看 ctx 的调用上时，后续尝试要一直等到它返回。
  进程被杀时，服务端要到 `transactionLifetimeLimitSeconds`（缺省 60s，O-S5-3 建议 20s）才中止遗留事务，这段时间里后续尝试都过期，可能用尽 `MaxAttempts`，进入补偿或 ManualRequired。
  现在的设计在截止时就能接替。不过如果 k+1 的业务写碰到 k 遗留事务锁住的同一批文档，现在一样要等（N06 S5 和 ② 的强杀实跑里，恢复时间都被遗留锁拉到 60s 以上）；只有业务写不重叠时（按尝试插入不同的文档），两种设计才有差别。
- 附带的简化：handler 失败时整笔回滚，claim 不会留下，就不需要 `releaseLease`。

### 2. 首次尝试快路径（同一操作还没有 claim 时省掉守卫 upsert 或 Reserve 往返）——不采用

要省掉的不是往返，而是那次提交，所以快路径实际就是“首次尝试用单事务”，只剩三种写法：
- 不写守卫：k 与 k+1 写不同的 claim 文档，没有写冲突。两边各自读到“没有别的尝试”的快照后都提交，就是写偏斜，同一操作生效两次，**违反最多一次**。
- 守卫写在最前：就是候选 1。卡住或被杀的往往正是首次尝试，所以快路径恰好会落在用例 2 覆盖的那个场景，接替同样丢失。
- 守卫写在最后（乐观并发，见下面的选项 C）：最多一次和接替都成立，但在途等待 (i) 丢了。
任何一种都不能在不改断言的前提下通过 ② 的用例 2。另外，“有没有 claim”本身就要读一次，省不掉。

**选项 C：乐观单事务（推断，未实测）**。事务开头插回执占位，让同一命令的并发重投在 handler 之前就冲突（这是修前的做法）；然后读同一操作的 claim，有结论就回放；
接着跑 handler；最后写守卫、检查截止、写 claim 和回执。同一操作的两个尝试谁先提交守卫谁赢，输的一方在重跑时读到对方的结论。
最多一次、截止、接替都成立。代价是 k 在途、租约有效时 k+1 会照样执行 handler：事务内的写只有一份生效，事务外的调用（gift deliver 发邮件）可能多发一次。
按用例 2 的断言，它会在“租约有效时不执行”这一步失败；这是推断，没有实跑。
正常运行中，只有 k 的 handler 拖过截止、或者协调器和步骤进程之间有时钟偏差时，才会出现这种重叠。延迟预计与候选 1 同量级（一次提交），没有测。

### 3. 合并守卫写和 claim 写，减少文档数或索引写——不采用

- 守卫是每个操作一份（`saga-step-op/<IdempotencyKey>`），claim 是每条命令一份（`saga-step/<CommandID>`）。
  原生投影的 lease fence 是按 `DocumentID = saga-step/<CommandID>` 去找 claim 的（`coredata.LeaseFence`），两种收件箱共用这套持久格式。
  要合成“每个操作一份”，得改原生投影的 fence 格式，还要做迁移。
- 收益最多是少一条数据命令：客户端 0.1～0.3 ms，服务端约 0 ms，不到修后单次延迟的 2%，比测量噪声（±3～12%）还小，而且不减少提交次数。
- 同理，把“读自己的 claim”并进“按操作查询”（`$or`）也能省一次往返（约 0.1～0.2 ms），但它会动原生步骤共用的判定路径。
  旧版本写的 claim 没有 `operation_key`，那时还得兜底单独读一次。收益在噪声以内，不做。

### 4. 调整 Reserve 的写关注——不采用（契约会被打破）

把 Reserve 的提交降到 `w:1, j:false`，这次提交就从约 9.5 ms 降到 0.11 ms（实测），单次延迟几乎能回到修前水平。逐项看后果：
- claim、守卫、supersede 写丢失（切主回滚）本身不危险。执行事务以多数派提交时，按 oplog 前缀，同一主节点上更早的 Reserve 也已经到了多数派；
  claim 丢了，`settleOwnClaim` 就匹配不上，事务被 fence；supersede 丢了，旧尝试仍被它自己的租约时间挡住，因为 supersede 只会在租约过期后发生。
- **反例在读那一侧。** 事务的 snapshot 读关注只有在以多数派提交时，才保证读到的是多数派已提交的数据。设想 k 的执行事务已在主节点本地提交，但还没复制到多数派，这时同一命令的重投（或 k+1）进来；
  它的 Reserve 以 `w:1` 读到 k 的回执或 completed claim，于是回放 k 的成功，发给协调器。随后主节点故障，k 的提交被回滚。
  结果是协调器接收了一份最终没有生效的成功，违反“回放的结果一定已生效”。现在 Reserve 以多数派提交，这种情况下提交会失败或结果未知，于是 nak，不会回放。
- 这还需要 `mongo` 接口支持按事务指定写关注，这不在本次范围内。
- 结论：保持 `w:majority, j:true`。顺带一提，多数派下 `j:true` 和 `j:false` 几乎没有差别，因为 `writeConcernMajorityJournalDefault` 缺省是 true。

### 5. 批量或流水线——不采用

- **单次 Handle 内部做流水线**：执行事务必须在快照里看到已经提交的 claim，`settleOwnClaim` 才能匹配。所以执行事务不能在 Reserve 提交确认之前开始，两次提交只能串行。
- **跨投递把 Reserve 攒成一批**：一个事务预约多条命令，负载高时每次尝试的提交数可以从 2 降到 1+1/B。实测吞吐确实受每秒提交数限制，所以有效果。
  但这要在消费者里加一层攒批和调度，低负载时要等批次凑齐，单次延迟反而变长。一批里任何一条冲突或校验失败，整批都要拆开重试。这违反 roost-coding 里“不引入无证据的复杂调度层”。
  目前没有业务量证据表明 Mongo 步骤吞吐是瓶颈。如果以后有了，这是首选方向，需要单独写方案。Mongo 自带的日志组提交在 32 协程时已经开始分摊（−43%）。

## 实施内容

- **没有改生产代码**，原因见上面的候选。
- 新增 `saga/mongo_step_latency_real_mongo_integration_test.go`（`integration` tag，只有带 `-bench` 才会运行，不会被 `-run '^TestRealMongo'` 选中）：
  `BenchmarkRealMongoStepLatencyBreakdown`（命令构成与耗时分解）、`BenchmarkRealMongoStepThroughput/g=1,8,32`、`BenchmarkRealMongoCommitWriteConcern`。
  库名用 `roost_mongolat_<pid>_<ns>`，跑完删除。文件只依赖 `Handle` 的公开签名，可以复制到修前的源码树上做对照。
- 证据在 [evidence/mongolat](evidence/mongolat)。spike 和 fsync 探针都在 scratch 里，没有提交，补丁原文见证据文件。

## 需要维护者决定

| 选项 | 每次尝试的提交 | 单次延迟（本机） | 吞吐（本机，32 协程） | 契约与行为 |
| --- | --- | --- | --- | --- |
| **A. 接受现状**（推荐） | 2 | 18.3 ms | 1.0k ops/s | ② 的契约完整：在途等待、截止时接替、最多一次、截止前生效 |
| B. 合并事务（守卫在前） | 1 | 9.2 ms（实测） | 1.4k ops/s（实测，3 轮） | 最多一次、截止前生效保留；**截止时接替丢失**，卡住或被杀的尝试会挡住后续尝试，直到它的事务结束（被杀进程最长 `transactionLifetimeLimitSeconds`）；② 的用例 2 要改断言 |
| C. 乐观单事务（守卫在后） | 1 | 预计与 B 同量级（未测） | 未测 | 最多一次、截止前生效、接替保留；**在途等待丢失**，与在途尝试重叠的新尝试会执行 handler，事务外调用可能重复；② 的用例 2 要改断言 |

推荐 A 的理由：
- 多出的代价是一次落盘提交，在生产的 Linux 环境大约 1～5 ms（推断），而 saga 步骤是低频的业务流程。
- B 和 C 各自去掉的，正是 ② 为了修 O-S5-4（尝试 k 拖过截止后提交，造成两份业务写）、减少重复外部调用而新加的性质。
- 如果以后 Mongo 步骤吞吐成了瓶颈，先考虑候选 5 的跨投递批量预约，它不放松契约。

## 复跑

```sh
# 私有副本集（不碰共享环境）：source kit/scripts/integration/lib/{common,mongo}.sh 后 mongo_up，
# ROOST_IT_HOME=<私有目录> ROOST_IT_PORT_OFFSET=<非 0 / 1000>；或共享隔离环境 source ~/.roost-it/roost-dataengine-it/env.sh（不打印）。
export ROOST_DATAENGINE_IT_MONGO_URI=<副本集 URI>
GOWORK=off go test -tags integration -run '^$' -bench 'BenchmarkRealMongoStep(Throughput|LatencyBreakdown)$' -benchtime 1000x -count 6 -benchmem ./saga/
GOWORK=off go test -tags integration -run '^$' -bench 'BenchmarkRealMongoCommitWriteConcern' -benchtime 300x -count 3 ./saga/
# 修前对照：git worktree add --detach <dir> 9669d181^，把上面的基准文件复制到 <dir>/saga/，两边交替跑，benchstat 比较。
```

## 实施状态与验证

**分析已完成；没有改生产代码，未发版**（提交号见 DECISIONS-PENDING 第十二轮“Mongo 步骤延迟”一行）。验证时 rebase 到 `origin/main` `6bf15516`；
`78e26853` 之后上游对 saga 只改了测试和 `kit/saga` 的步骤预算，收件箱代码没有变化，测量结论仍然适用。验证均在 `GOWORK=off` 下进行：

- `gofmt -l saga kit/saga` 为空；`go vet ./saga/`、`go vet -tags integration ./saga/`、`go build ./... && go vet ./...` 都通过；
- `go test -race -count=3 ./saga/` 通过；根包 `go test -count=1 .` 通过；
- 私有副本集上 `go test -tags integration -count=1 -run '^TestRealMongo' ./saga/` 全部通过（当前代码，包括 ② 的 `TestRealMongoStepAttemptsOfOneOperationTakeEffectOnce`）；
- spike（合并单事务）上同一个用例在真实副本集和 mongotest 上都变红，失败文本见候选 1。spike 的工作树在收尾时删除。
- 没有改 nest / entity / dataengine / sync，所以没有跑 glsvet；没有改生成形状，所以没有跑 codegen 和 game-demo。

**未完成**：64 个以上协程的吞吐没有测；选项 C 只做了推断，没有实测；生产形态（Linux、跨主机副本集）下的提交耗时没有测，文中的 1～5 ms 是推断。

## 维护者决定（2026-10-06）

选 **A：接受现状**。原话：“按照A，目前真正走saga的实际业务场景不多，55tps足够了”。每次尝试两次落盘提交、契约不放松；B / C 不实施。影响面只在 saga 的 Mongo 步骤（`MongoCommandInbox`），原生步骤、普通 DAO / Entity 写入与 remote entity 不经过这条路径。对延迟敏感的流程可改用原生步骤或不走 saga。
