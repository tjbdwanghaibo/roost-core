# 第三类：未归因偶发 / 疑点的复现与否证，第二批（2026-11-18）

范围：维护者指定的两项——机器人 200 人同秒起跑时 AddExp 在 World 热点上的 `nest: sync timeout`，以及 Remote
128 / 256 并发约 3s 的全体超时簇。第一批见 [category3-anomalies-20261118](../category3-anomalies-20261118/README.md)。

机器：macOS arm64（10 核），**不空闲**：桌面应用常驻（CorpLink、Meshy 等），测量期间 1 分钟负载约 3.4～7（历史探针那 8 轮被探针自己推到 22），swap 已用
约 6.66G / 8G。所有测量串行，计时测量期间不编译；每次测量前后记 `uptime` / `vm.loadavg` / `vm.swapusage`（各目录的
`host-*.txt`）。codebase-memory-mcp 本次连不上，代码定位按当前源码与 grep。基线为 main `4652985a`。

## 1. 200 人同秒起跑：AddExp 的 `nest: sync timeout`

### 做了什么

- **临时诊断**（没提交，原文 [diag-dispatch-trace.diff](diag-dispatch-trace.diff)）：`framework/nest` 的派发队列在
  环境变量 `ROOST_DIAG_DISPATCH_DIR` 非空时，每个外部作业结束记一行：handler、全部目标（kind:id）、准入 / 就绪 / 开始 /
  结束时刻、是否等过同 ID 前驱，以及**最后一个放行它的前驱与它共享的目标 ID 和那个前驱的 handler**。多目标作业
  （AddExp 锁 World + Player）据此能分清卡在哪一个实体上。没设变量时只多一次 `sync.Once` 检查。
- 入口不变：`DEMO_ROBOT_MIRROR_OFFSET=20000 DEMO_ROBOT_COUNT=200 DEMO_ROBOT_MODE=burst bash scripts/perf/demo-robot.sh`，
  每次全新私有依赖和全新工程（脚本的 `replace` 指回本 worktree，所以诊断代码进了生成工程）。
- 两次运行：
  - **A 基线**（只有诊断）：[item1-200burst-baseline/](item1-200burst-baseline/)。负载 4.66 → 3.42。
  - **B 诊断对照**（另把 demo 模板里 `handlerAddExp` 与 `handlerRecordEnter` 的标记从
    `rollback=undo durability=strict` 改成 `rollback=undo durability=pipelined completion=async`，只在本 worktree、
    跑完即还原、没提交）：[item1-200burst-async-diag/](item1-200burst-async-diag/)。负载 6.16 → 6.50。
- 汇总脚本 [analyze_dispatch.py](analyze_dispatch.py)、[window.py](window.py)，结果在各目录 `analysis.txt`；
  派发记录原文 `game-dispatch.csv.gz`（只取 game 进程）。

### 原始数据

| 运行 | 成功 / 200 | add_exp 步 | 日志 `add_exp failed` | 失败落在 |
|---|---|---|---|---|
| A 基线 | 86 | ok 86 / failed 114，mean 2.79s，p95 3.55s | 114（`nest: sync timeout` 105，`nest: sync canceled` + `context deadline exceeded` 9） | 全部在 add_exp |
| B AddExp / RecordEnter 改异步完成 | 32 | **ok 200 / failed 0**，mean 0.20s，p95 0.37s | 0 | saga 状态轮询：`alliance_saga_still` 153、`gift_saga_still` 14 |

A 与 11-14（72 / 200）、11-18 压缩测量（80 / 200，108 次）形态相同。

**等在哪把锁上（A）**：AddExp 作业 286 个（同秒的 200 个加上之后通过的玩家后续调用）：

- 243 个最后由**与它共享 World ID 的前驱**放行，43 个准入时没有前驱，**由 Player 放行的 0 个**；
- 只有 2 个 AddExp 的 Player 在它等待期间有别的作业在跑；
- 依赖等待（准入 → 就绪）mean 1.83s、p95 3.03s、max 3.03s；就绪 → 开始 mean 0.1ms（worker 不缺）。

**World 链在那 3 秒里做了什么（A）**：以第一个 AddExp 准入为 t0：

- t0 时 World 上还排着 **136 个 `handlerRecordEnter`**：约 0.6s 前 enter_game 用 `Send_RecordEnter` 投递，调用方不等，
  但它们是 strict，仍逐个占 World 链，余下占用 1.34s；
- [t0, t0+3s] 内 World 忙 2.89s（96%），完成 RecordEnter 136 个、AddExp 82 个；
- World 上 strict 作业的执行时间（含锁内等 WAL fsync）：RecordEnter mean 9.7ms，同秒那批 AddExp 中位 19.8ms；
  同时段 `nestwal_fsync_duration` 平均 10.4ms（16.14s / 1546 次）、最大 27.8ms；
- 同秒 200 个 AddExp 只有 86 个在准入后 3s 内开始；其余 109 个在准入后 ≥3.0s 才轮到，执行都 <3ms（远小于一次 fsync），
  调用方此时已按 3s 超时返回。

**请求期限是 3s，不是 5s**：生成的 `configs/service/config.game.yaml` 有 `nest.request_timeout: 3s`，`nest.NestSyncTimeout`
（5s）只是包级缺省。

**B 对照**：AddExp 的依赖等待 p95 333ms，World 链上 AddExp / RecordEnter 的作业时间降到约 0（作业在 Enqueue 后结束，
回复仍在持久之后）；`nest: sync timeout` 0。200 人全部过了 add_exp，于是同秒压到后面的 saga：联盟 / 礼物 saga 的状态轮询
大量等不到终态（步骤 30 failed 124 / 186、步骤 20 failed 14 / 200），整体 32 / 200。这一段就是
[robot-step-timing-20261118](../robot-step-timing-20261118/README.md) 第 5～6 节的 saga 启动 / 步骤并发排队。

### 结论

- **(a) 等的是 World：确认。** 归因数据见上；Player 侧没有竞争。
- **(b) 原因：确认是 World 链上 strict 提交“每次 fsync 一条”的吞吐上限，加上排在前面的 RecordEnter 积压。**
  同一实体的下一条作业要等上一条做完，strict 作业要在锁内等到 WAL fsync 才结束，所以 World 每秒最多约一百来笔
  （本机这轮约 10ms 一次 fsync）。同秒 200 个 AddExp，前面还排着 136 个只投递不等待的 RecordEnter，总共约 3.4s 的 World
  时间，超过 3s 请求期限，末尾的 114 个超时。fsync 本身没有异常变慢（均值 10.4ms）。这就是 KNOWN-LIMITS“热点单例实体的
  写吞吐”第一条的同一机制，不是新缺陷。对照 B 把这两个 handler 改成异步完成后 add_exp 零超时，因果成立。
- **(c) 框架限制 + demo 写法，不是框架 bug。** strict 在单个实体上每次 fsync 一条是已写明的契约代价；框架已经提供
  `durability=pipelined completion=async` 给热点写。demo 的选择是：AddExp 做成 World + Player 的 strict 两实体事务，
  每次发经验都写全服 `ExpGranted` 计数（WALKTHROUGH“实体锁档与跨实体事务”把它当两实体事务的教学例子）；登录计数
  RecordEnter 只投递但仍是 strict，它的注释说“不占登录的请求预算”，这对登录成立，但它占着 World 链，吃掉的是排在
  后面的 World 同步请求（这里是 AddExp）的预算。World 上另有 strict 的 ClaimDungeon（B 里成为 World 上最大的占用，
  372 次、平均 12.8ms）和 RecordMatch。
- **只改 demo 不能让 200 人同秒档通过**：B 里 add_exp 全过之后，失败整体移到 saga 排队，那是框架 saga 启动 / 步骤并发
  的问题，按 robot-step-timing 第 6 节仍待定。

### 建议的 demo 改法（没改，等维护者定）

- **A（推荐）**：World 上的写都声明异步完成：`handlerAddExp`、`handlerRecordEnter`、`handlerClaimDungeon`、
  `handlerRecordMatch` 改 `durability=pipelined completion=async`。保留两实体原子性和“回复在持久之后”，World 链在
  Enqueue 后即放下一条。代价：这些 handler 的 AfterCommit 改在完成池执行（无请求上下文、不持锁、不能同步等同一实体的
  Nest 请求），要逐个核对；需要重新生成并跑 demo 测试（`TestDemo`、add_exp / claim_dungeon 用例）；RecordEnter 的注释要改。
  B 只验证了前两个，后两个没测。
- **B**：去掉每次发经验对 World 的同步写（全服经验累计改成周期汇总或分片计数），AddExp 回到单实体。代价：WALKTHROUGH
  失去“两实体事务”的现成例子，要另找一个不在热点上的例子。
- 两者都不解决 saga 段，200 人同秒档的通过还取决于 saga 并发的决定。

### 尚未确认

- 准入后 ≥3s 才轮到、执行 <3ms 的那 109 个 AddExp 最终有没有生效（调用方已经拿到超时）。按契约“超时只结束等待，
  不撤销已准入的业务”，<3ms 的执行时间又说明它们没走完一次 strict 提交；本轮没核对 World `ExpGranted` 与玩家经验。
- 只在 macOS（`F_FULLFSYNC`）上测；Linux 上 fsync 更快时上限会升高，但同秒突发在 strict 单实体链上的机制不变。
- 同秒 AddExp 中位 19.8ms、约两倍于 RecordEnter：推断是别的实体的 strict 负载让每笔多等一次进行中的 fsync，没拆。

## 2. Remote 128 / 256 并发约 3s 全体超时簇

### 先核对入口与口径

- **夹具的开关不是配置缺省**：`TestAllianceE2ERPCConcurrency` 里 `background := os.Getenv("ALLIANCE_E2E_BACKGROUND_CHECKPOINT") == "1"`，
  再把它显式写进 alliance 配置。所以**不设这个变量时夹具跑的是关**，不是 2026-11-14 起的配置缺省（开）。本轮“缺省开”一律
  显式 `ALLIANCE_E2E_BACKGROUND_CHECKPOINT=1`。
- **历史的 20 簇不是这个夹具测出来的**：[alliance-join-1000-20261105“单实例全体同时超时”](../../../performance/benchmarks/alliance-join-1000-20261105/README.md#单实例全体同时超时2026-11-06)
  的 8 轮用的是探针 `TestJoinScaleRetry`（单个 alliance 实例，级别 128 / 256 / 256，每段 12s，新玩家 Join → Leave →
  已有守卫 Join → Leave，可重试错误带同一 RequestID 退避重试；停顿监视器在 ≥32 个尝试已超过 2s 时抓栈）。探针不在仓库，
  在维护者本机 `roost-artifacts/alltimeout-20261106/probe/`。正式入口 `TestAllianceE2ERPCConcurrency` 的 Join 段每 worker
  只 8 次（`join-spread16` 共 1024 / 2048 次），负载形状轻得多；M-19 用的也是它。所以本轮两个都跑：
  - **A 正式入口**：`TestAllianceE2ERPCConcurrency`，128 / 256 交替各 3 轮；
  - **B 历史探针**：`TestJoinScaleRetry` 在当前树上复跑 8 轮，同一口径数簇。
- **簇的口径**：沿用历史探针的“1 秒内 ≥32 个尝试以 ≥2.5s 结束”。历史探针有 `LONG` 行（按 100ms 桶记 ≥2.5s 的结束数），
  按秒合并即可；[probe_clusters.py](probe_clusters.py) 对 11-06 的 `c-after-1/probe.log` 数出 13 簇，与当时表格一致，口径
  可复现。正式入口只打印 ≥3s 的慢样本，[clusters.py](clusters.py) 按“同一秒内结束的 ≥3s 慢样本 ≥32 个”数（比历史略严）。

### A：正式入口，缺省开，128 / 256 各 3 轮

命令（[item2.sh](item2.sh)，私有 mirror-local 偏移 21000，一套环境跑完 6 次后 clean）：

```sh
ALLIANCE_E2E_PERF=1 ALLIANCE_E2E_BACKGROUND_CHECKPOINT=1 ALLIANCE_E2E_STACKS=1 \
ALLIANCE_E2E_RPC_CONCURRENCY=128|256 ALLIANCE_E2E_RUN='^TestAllianceE2ERPCConcurrency$' \
ALLIANCE_E2E_LOGS=<新目录> bash scripts/test-alliance-e2e.sh
```

6 次全部 exit 0、PASS。原始 PERF 行（只留 PERF 与结果行，进程日志含连接串没留）在
[item2-rpc-concurrency-bg1/](item2-rpc-concurrency-bg1/)，汇总 [clusters.txt](item2-rpc-concurrency-bg1/clusters.txt)：

| 轮 | 并发 | 负载（1 分钟，前 → 后） | 失败 | ≥3s 样本（全在 join-same） | 一秒内结束的 ≥3s 样本最多 | 簇 | 栈采集触发 |
|---|---|---|---|---|---|---|---|
| r1 | 128 | 4.95 → 6.61 | 0 | 185 | 25 | 0 | 0 |
| r1 | 256 | 6.61 → 5.13 | 0 | 190 | 23 | 0 | 0 |
| r2 | 128 | 5.13 → 7.02 | 0 | 190 | 23 | 0 | 0 |
| r2 | 256 | 7.02 → 6.02 | 0 | 191 | 23 | 0 | 0 |
| r3 | 128 | 6.02 → 6.85 | 0 | 192 | 23 | 0 | 0 |
| r3 | 256 | 6.85 → 6.21 | 0 | 192 | 22 | 0 | 0 |

swap 全程 6.66G / 8G。≥3s 样本全部是单联盟热点入会 `join-same`（约 21～24 次 / 秒串行排队，错误全为 nil），与 M-19 相同，
不是簇。`join-spread16` 128 并发 p99 约 1.1～1.2s，256 并发约 2.3～2.4s，零失败。

### B：历史探针在当前树上，缺省开

命令（[probe-run.sh](probe-run.sh)）：探针原文复制自 `roost-artifacts/alltimeout-20261106/probe/`
（`joinretry_test.go.txt`、`joinprobe_test.go.txt`、`pprof_probe.go.txt`，没提交），只改一行：`JOINSCALE_KEYS` 的
`section.key=value` 原来按第一个点切，改成按最后一个点切，好让 `dataengine.projection.background_checkpoint=true`
落到嵌套节（harness 的 `setKey` 找不到节会 `t.Fatalf`，用例 PASS 说明写进去了；进程日志里没有回显这个键，没另行核对）。
参数与 11-06 相同：`JOINSCALE_LEVELS=128,256,256`、`JOINSCALE_SECONDS=12`。编译在计时之前做完（`go test -c`），
私有 mirror-local 偏移 21000。原始 PERF / LONG / STALL 行在 [item2-joinscale-probe/](item2-joinscale-probe/)。

8 轮（前 3 轮一套环境、后 5 轮另一套新环境）全部 exit 0、PASS，汇总 [clusters.txt](item2-joinscale-probe/clusters.txt)：

| 轮 | 负载（1 分钟，前 → 后） | 新玩家 Join 128 成功次/秒 · p99 | 256 并发失败合计（3 段 × 4 阶段） | ≥2.5s 的单次尝试 | STALL 抓栈 | 簇 |
|---|---|---|---|---|---|---|
| r1 | 5.41 → 20.19 | 419 · 512ms | 5 | 0 | 0 | 0 |
| r2 | 20.19 → 16.76 | 438 · 500ms | 1 | 0 | 0 | 0 |
| r3 | 15.65 → 7.87 | 383 · 565ms | 0 | 0 | 0 | 0 |
| r4 | 8.11 → 22.18 | 421 · 499ms | 2 | 0 | 0 | 0 |
| r5 | 22.18 → 15.47 | 407 · 559ms | 0 | 0 | 0 | 0 |
| r6 | 15.47 → 13.48 | 437 · 529ms | 2 | 0 | 0 | 0 |
| r7 | 13.48 → 16.62 | 376 · 693ms | 0 | 0 | 0 | 0 |
| r8 | 16.62 → 16.12 | 381 · 593ms | 0 | 0 | 0 | 0 |

- 负载升到 15～22 主要是探针自己（256 并发 + 三节点 Mongo / NATS 同机）；swap 全程 6.66G / 8G。
- 全部阶段里最长的一次调用（含重试）2.61s；单次尝试没有一个到 2.5s，所以停顿监视器一次也没触发。
- 256 并发的失败合计 10 次：6 次是 50 次重试后仍“`remote.4: not executed; retry with the same request id`”（可重试的
  写额度 / 守卫答复），3 次是前一步没入会导致的 `alliance or member not found`。与簇无关。
- 对照 11-06 同一探针：新玩家 Join 128 修后 8 轮 81～353 次 / 秒、p99 0.67～4.1s，256 并发失败每轮 72～1003，簇 20 个；
  本轮 376～438 次 / 秒、p99 0.50～0.69s，失败每轮 0～5，簇 0。两次负载与 swap 不同（当时 load 11～57、swap 16～17G /
  17～18G 且有 Unity 编辑器），代码也已不同（之后合入了 nestwal fsync 与写入解耦、B-3 后台 checkpoint 等），所以这只说明
  “当前树在本轮条件下没有簇”，不能把差别归到哪一项改动。

### 结论

- **未复现**（条件：macOS 本机，正式入口 6 轮负载 5～7、历史探针 8 轮负载 5～22，swap 6.66G / 8G，三节点 Mongo / NATS
  与服务同机，`background_checkpoint` 开）。正式入口 0 簇、栈采集 0 次；历史探针 0 簇、≥2.5s 尝试 0 个、STALL 0 次。
- 没有簇，所以没做“开 / 关”对照，也没有停顿栈可归因（I/O、Mongo majority、WAL fsync、checkpoint 都无从区分）。
- **能不能据此关闭**：不能证明根因已消失。11-06 的 20 簇出现在 load 11～57、swap 接近用满、mongod 主从都有 ≥1s 慢操作的
  条件下，当时的停顿栈都停在投影 Mongo 事务（多数派）、outbox Mongo 标记、WAL fsync、checkpoint 落盘这些 I/O 上；本机
  这轮造不出同等的 I/O 停顿。按 11-06 的结论 2，这类簇的机制是“strict 写要等持久与发布，I/O 停顿超过请求期限时全部在途写
  一起超时”，属于 strict 契约在存储停顿下的表现，而不是一个待修的代码缺陷。建议把它从“未归因遗留”改成“框架限制 + 外部
  环境验收（C-Q1）”：限制写明机制与触发条件，C-Q1 在目标硬件上保留停顿栈再看是否还有别的来源。由维护者定。
- 附带确认：`TestAllianceE2ERPCConcurrency` 不设 `ALLIANCE_E2E_BACKGROUND_CHECKPOINT` 时测的是“关”，与配置缺省相反。
  M-19 是显式 0 / 1 交替，不受影响；以后单跑这个夹具要显式写 1。是否改夹具让“不设”跟随配置缺省，由维护者定（没改）。

## 3. 收尾与清理

- 诊断补丁、demo 模板改动都已还原，工作树只多本目录。
- 本轮起的私有依赖（mirror-local 偏移 20000 两次、21000 三次）、etcd、服务进程都由脚本在结束时停掉并删除根目录
  （`mirror-clean.log` / `cleanup.log` 显示 “no residual processes”）；结束后 `ps` 没有本轮残留进程。
- [item2.sh](item2.sh)、[probe-run.sh](probe-run.sh) 里写死了本次会话的 scratchpad 与 worktree 路径，只作记录；复跑要改路径，
  探针源码取自 `roost-artifacts/alltimeout-20261106/probe/`。

## 4. 建议写进 KNOWN-LIMITS / CHANGELOG 的文字（由协调者统一写）

**KNOWN-LIMITS“机器人负载：100 / 200 人同秒起跑不通过”末尾追加：**

> **2026-11-18 200 人同秒归因**（证据：`evidence/category3-batch2-20261118/README.md` 第 1 节，链接按 KNOWN-LIMITS 的位置写作 `evidence/category3-batch2-20261118/README.md#1-200-人同秒起跑addexp-的-nest-sync-timeout`）：AddExp 等的是 World，不是 Player（派发诊断：286 个 AddExp 中 243 个最后由共享 World 的前驱放行、0 个由 Player 放行）。原因是 World 链上 strict 提交每次 fsync 一条（本机约 10ms 一次），同秒 200 个 AddExp 前面还排着 136 个只投递不等待、但仍是 strict 的 `RecordEnter`，约 3.4s 的 World 时间超过 `nest.request_timeout` 3s，末尾 114 个超时。这是“热点单例实体的写吞吐”的同一机制，加上 demo 的写法（AddExp 每次写全服 `ExpGranted`、World 上的写都是 strict），不是框架缺陷。诊断对照把 AddExp / RecordEnter 改成 `durability=pipelined completion=async` 后 add_exp 200 / 200 零超时，但失败整体移到 saga 状态轮询（32 / 200），所以 200 人同秒档还取决于 saga 并发的决定。demo 改法（World 写全部异步完成，或去掉每次发经验的 World 同步写）待维护者定。超时的那批 AddExp 是否最终生效，尚未确认。

**KNOWN-LIMITS“128 / 256 并发约 3s 全体超时簇仍未归因”替换为（建议，待维护者定）：**

> **strict 写在存储 I/O 停顿超过请求期限时会一起超时**（单实例 128 / 256 并发写 Remote 时“约 3s 全体超时簇”的机制）：strict 写要等持久与发布，投影 Mongo 事务（多数派）、outbox 标记、WAL fsync 或 checkpoint 落盘出现超过请求期限的停顿时，在途写全部超时（2026-11-06 证据：load 11～57、swap 接近用满时 8 轮 20 簇，停顿栈全在这些 I/O 上）。2026-11-18 在当前树上复跑：正式入口 `TestAllianceE2ERPCConcurrency` 128 / 256 各 3 轮（load 5～7）与 11-06 的同一探针 `TestJoinScaleRetry` 8 轮（load 5～22，swap 6.66G / 8G），`background_checkpoint` 开，**0 簇**、无 ≥2.5s 的单次尝试（证据：`evidence/category3-batch2-20261118/README.md` 第 2 节，链接写作 `evidence/category3-batch2-20261118/README.md#2-remote-128--256-并发约-3s-全体超时簇`）。本机造不出当时的 I/O 停顿，未复现不等于不会发生；目标硬件上的验收仍按 C-Q1（保留停顿栈、fsync / outbox 指标、主从慢日志）。

（若维护者仍要把它留作“尚未确认”，条件写：未复现，正式入口 6 轮 + 历史探针 8 轮，load 5～22，swap 6.66G / 8G，macOS 同机三节点。）

**CHANGELOG**：本批只有证据与诊断，没有代码 / 行为变化，不需要条目。
