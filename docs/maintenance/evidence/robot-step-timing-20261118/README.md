# game-demo 机器人 100 人同秒档：整场景时间花在哪一步（2026-11-18）

维护者 2026-11-18 定：先加逐步计时跑一轮，再决定改场景、调门槛还是查框架。背景是 [perf-compressed-20261118](../perf-compressed-20261118/README.md) 第 3 节：100 人同秒 100 / 100 成功、每类请求零超时，但整场景 p50 20.7s / p95 24.8s，超过 16s 门槛。

基线 main `616c5a90`。本轮只加计时，不改门槛、不改 demo 业务逻辑、不改场景。

## 结论

- **时间主要花在等 saga 完成上**：5 个 saga 状态轮询步骤（礼物 2 个、联盟 3 个）合计约 17s，占整场景的 72%～73%（两轮正式测量），两轮的分布几乎一样。
- **其余的大头是场景本身的固定等待**：一场帧同步对战约 1.34s、服务端寻路 0.62s、聊天推送等待 0.51s、匹配等待约 0.66s、邮件轮询约 0.85s。还有 `add_exp` 1.3～1.4s（p95 约 2.5s），是 100 人同秒排队等 World 热点。
- **saga 慢在“启动链路”，属于框架排队，不是 saga 步骤本身慢**。诊断轮（第 5 轮，补丁没提交）把联盟 saga 拆成三段，以发起 found 为例：
  - 从发起请求应答（Nest 事务已提交）到协调器建出 saga 记录：平均 4.1s，p95 4.7s。低负载时（最后一个）只要 0.12s。
  - 协调器里从建出到终态：平均 1.4s。
  - 机器人轮询看到终态的滞后：0.2s。
- **把启动中继并发从 4 调到 16 后（第 7 轮，诊断）**：启动链路降到 0.4～0.5s，但排队转移到 Mongo 步骤（缺省串行）。有 5 个礼物 saga 的投递步骤超过 5s 步骤超时被重派，机器人等 10s 后判失败，6 / 100 失败。只调 Mongo 步骤并发（第 3 轮）没有效果，因为前面的启动中继先卡住了。
- 所以这是“多段限流串在一起”：saga 启动中继（`saga.start_effect_workers` 缺省 4）→ Mongo 步骤消费者（每条流串行，`MongoWorkers` 缺省 1，wiring 没有开放配置）→ 协调器。同秒 100 人在这几段上排队。**每段各自的容量上限、PubAck 延迟占多少，尚未确认**。

## 1. 计时怎么加的

都在正式链路上，没有新建包：

- **`robot/scenario`**：
  - 新增 `Step(name, node)` 组合子。每跑一次，记下耗时、成败、子树内 `Retry` 实际执行子节点的次数，以及 `Wait` 节点睡掉的时间。
  - 嵌套 Step 各记一份，内层的 wait / retry 同时计入外层。计数挂在 ctx 上，用原子量，`Parallel` 分支可以安全并发。
  - 机器人没有 Recorder 时照常执行、不计时。
  - `ParseSpec` 给 YAML 场景的每个顶层步骤自动套 Step；根不是 sequence 时，整棵树算一步。
  - 步骤名是两位序号加子树摘要，例如 `30 retry(wait 250ms,alliance_op_status)`。**demo 模板不用改**。
- **`robot/session`**：
  - `Recorder.FinishStep` / `StepSnapshot` 汇总出 `StepReport`：samples、ok / failed、mean / p50 / p95 / max、wait_mean_ms、retry_attempts_avg / max。
  - 和消息统计共用同一个指数桶直方图（从原来的 callSeries 抽出 `latencyBuckets`，行为不变）。
  - 最多 256 个步骤名，超出的样本并进 `(overflow)`，不丢。
- **`robot/runner`**：新增 `Runner.StepReport()`。
- **`robot/loadtest`**：`RunSnapshot.steps`（JSON `steps`）。快照深拷贝。
- **`scripts/testdata/demo-robot/report.py`**：`summary.json` 增加 `steps_by_mean_ms`，按平均耗时从大到小排。
- **文档**：`docs/framework/ROBOT.md` 的报告解释和 `scripts/testdata/demo-robot/README.md` 已同步。

步骤耗时 = 请求往返 + 场景等待 + 机器人本地处理，不是服务端排队时间。

## 2. 红绿与变异

先写桩：API 存在，但不记录。新用例在桩上全部红，原文见 `red.txt`：

```
--- FAIL: TestManagerIncludesRequestAndTrafficReports   loadtest_test.go:348: step report missing: []
--- FAIL: TestRunnerReportsNamedStepsOfEveryRobot       runner_test.go:94: steps = []
--- FAIL: TestSpecTimesEachTopLevelStep                 scenario_test.go:250: step names = []
--- FAIL: TestNestedStepsAccumulateIntoEveryEnclosingStep scenario_test.go:291: steps = []
--- FAIL: TestStepReportAggregatesTimeRetriesAndWaits   report_test.go:87: steps = []
```

实现后全绿。每次变异只撤一行，跑完都已恢复：

| 撤掉的行 | 变红的用例 |
|---|---|
| `Retry` 里的 `noteRetryAttempt(ctx)` | `TestSpecTimesEachTopLevelStep`（retry 0，应为 3）、`TestNestedSteps…`、`TestRunnerReportsNamedSteps…` |
| `noteWait` 沿 parent 向外累计，改成只记最内层 | `TestNestedStepsAccumulateIntoEveryEnclosingStep`（外层 wait 2.5ms，应 ≥5ms） |
| Step 记录 `OK: err == nil` 改成恒为 true | `TestSpecTimesEachTopLevelStep`（失败步记成 ok） |
| `buildTimedRoot` 不给顶层元素套 Step | `TestSpecTimesEachTopLevelStep`（steps 为空） |
| `FinishStep` 不累计 Waited | `TestStepReportAggregatesTimeRetriesAndWaits`（wait_mean 0） |
| loadtest 快照不取 `StepReport` | `TestManagerIncludesRequestAndTrafficReports` |
| `clone` 不深拷贝 `Steps` | `TestManagerIncludesRequestAndTrafficReports`（返回的报告改写了保留证据） |

验证命令与结果：

- `GOWORK=off go test -count=1 -race ./robot/...`：全部 ok。
- `GOWORK=off go vet ./robot/...`：无输出。
- `GOWORK=off go test -count=1 -run 'Link|Anchor|Doc' .`：ok。
- demo 模板没改，所以没跑 `TestDemo`。

## 3. 跑了几轮

入口都是：

```sh
DEMO_ROBOT_MIRROR_OFFSET=20000 DEMO_ROBOT_COUNT=100 DEMO_ROBOT_MODE=burst bash scripts/perf/demo-robot.sh
```

每轮用新目录和全新私有依赖，源码为分支提交 `8fdd3082`。

- **机器不空闲**：同时有另一个 agent 在编译 C# / Go；swap 用了约 7.8 GiB（共 9 GiB）。
- **只看分布，不当容量数字**：下表的整场景分位数受机器负载影响，结论只看时间落在哪些步骤。

| 轮 | 改动 | 负载（开跑 → 结束，1 分钟） | 成功 | 整场景 p50 / p95 | saga 轮询合计 |
|---|---|---|---|---|---|
| run1 | 无（正式） | 5.1 → 7.3 | 100 / 100 | 22.3s / 26.5s | 16.9s（72.3%） |
| run2 | 无（同档对照） | 5.3 → 5.5 | 100 / 100 | 21.9s / 26.2s | 17.5s（73.1%） |
| run3 | 诊断：Mongo 步骤并发 16 | 5.4 → 4.8 | 100 / 100 | 23.1s / 26.2s | 17.7s（73.5%） |
| run4 | 诊断：记录步骤命令的等待时长 | 8.4 → 7.9 | 100 / 100 | 25.0s / 28.9s | 19.7s（74.8%） |
| run5 | 诊断：记 run4 的指标 + 联盟 saga 三段时间戳 | 9.1 → 9.5 | 100 / 100 | 25.3s / 29.6s | 20.3s（75.4%） |
| run6 | 想测启动中继 16，但补丁没生效（生成配置仍是 4），当作重负载下的重复 | 12.7 → 19.4 | 100 / 100 | 24.1s / 29.0s | 18.7s（70.1%） |
| run7 | 诊断：启动中继并发 16（改生成配置） | 8.6 → 5.3 | **94 / 100** | 21.5s / 27.4s | 16.9s（71.5%） |

几点说明：

- 诊断补丁都**没提交**，原文在 `diag-patch-run4-run5.diff`、`diag-patch-run7.diff`，各轮的 `environment.txt` 也记着 ` M` 的文件。
- run7 第一次起跑时，端口 47118 还被上一轮占着，没起服就退出了，没有负载数据；换新目录重跑。
- 每轮的原始小文件在 `runN*/`：`summary.json`、`robot.json`、`host-*.txt`、`test.log`、`diagnostic-matches.json`、`environment.txt`、`loadtest-args.json`、`game-metrics-after.txt`。
- 整理结果：
  - `steps.md`：逐步表；
  - `timeline.txt`：服务端确认日志按秒分桶；
  - `saga-lifetime.txt`：联盟 saga 三段拆分。
- 整理脚本：`table.py`、`groups.py`、`timeline.py`、`lifetime.py`。

## 4. 逐步耗时（run1；run2 几乎相同，见 `run2/steps.md`）

完整 57 步见 `run1/steps.md`。下表是平均耗时 ≥ 0.1s 的步骤，按场景顺序列出：

| 步骤 | 平均 ms | p50 | p95 | max | 占比 | wait 平均 ms | retry 平均 / 最大 | 在等什么 |
|---|---|---|---|---|---|---|---|---|
| `02 enter_game` | 275 | 291 | 357 | 357 | 1.2% | 0 | 0 | 登录、建 Player、记入 World |
| `07 selector(wait_push\|wait 10ms)` | 510 | 512 | 512 | 512 | 2.2% | 10 | 0 | 等聊天推送，500ms 超时几乎每次都等满（场景写死） |
| `10 add_exp` | 1397 | 1374 | 2490 | 2490 | 6.0% | 0 | 0 | Player + World 事务，100 人同秒排 World 热点 |
| `11 retry(wait 250ms,mailbox_list)` | 597 | 532 | 819 | 1004 | 2.6% | 596 | 2.38 / 4 | 升级奖励邮件的效果链 |
| `15 retry(wait 250ms,gift_status)` | 1613 | 1156 | 3864 | 4594 | 6.9% | 1405 | 5.61 / 13 | 自赠礼物 saga |
| `17 retry(wait 250ms,mailbox_list)` | 251 | 253 | 253 | 253 | 1.1% | 250 | 1 / 1 | 礼物邮件（saga 完成时已到，只付一次 250ms） |
| `20 retry(wait 250ms,gift_status)` | 3512 | 3252 | 8193 | 8193 | 15.0% | 2861 | 11.42 / 20 | 赠给不存在的玩家，saga 补偿 |
| `30 retry(wait 250ms,alliance_op_status)` | 4586 | 4591 | 8393 | 8749 | 19.6% | 3787 | 15.12 / 24 | 建联盟 saga |
| `32 retry(wait 100ms,alliance_info)` | 101 | 103 | 103 | 103 | 0.4% | 100 | 1 / 1 | Remote 镜像（第一次就读到） |
| `34 retry(wait 250ms,alliance_op_status)` | 4061 | 4212 | 6481 | 6830 | 17.4% | 3591 | 14.34 / 20 | 捐献 saga |
| `37 retry(wait 250ms,alliance_op_status)` | 3129 | 3545 | 5004 | 6552 | 13.4% | 2789 | 11.14 / 19 | 领奖 saga |
| `44 move_arrive` | 621 | 633 | 708 | 708 | 2.7% | 0 | 0 | 服务端走 30 格（60 格/s 约 0.5s） |
| `51 selector(wait_push\|retry(…poll_match))` | 387 | 411 | 633 | 1126 | 1.7% | 0 | 0 | 等决斗配对推送 |
| `53 battle` | 1341 | 1349 | 1349 | 1349 | 5.7% | 0 | 0 | 一整场帧同步对战，固定时长 |
| `55 selector(wait_push\|retry(…poll_match))` | 278 | 245 | 532 | 612 | 1.2% | 0 | 0 | 等排位配对推送 |
| 其余 42 步合计 | 约 830 | | | | 3.5% | | | 普通请求，各自几十 ms 以内 |

各步平均之和是 23.4s（run1），与整场景 p50 22.3s 相符。

**轮询本身的额外开销**：retry 块都是先 `wait 250ms` 再查，所以就算一次就成，也要先付 250ms；平均多付约 125ms 的量化误差。5 个 saga 轮询加 2 个邮件轮询，至少约 1.75s。这部分是场景写法的固定成本，与负载无关。

## 5. 归因：在等什么、哪里在排队

### saga（约 17s，72%）

服务端指标（`run1/game-metrics-after.txt`）里，步骤体本身都很快（`saga_step_run_duration` 平均值）：

- 礼物：debit 26ms、deliver 14ms、debit-undo 25ms。
- 联盟：found-debit 26ms、found 228ms、donate 132ms、claim 123ms。

一个联盟 saga 的步骤体加起来约 0.25s，机器人却要等 3～4.6s。拆解：

- **步骤命令的排队**（run4，诊断指标“协调器发出命令 → 步骤开始执行”的平均值）：
  - found 717ms、donate 356ms、claim 524ms；
  - found-debit 142ms、donate-debit 118ms、reward-grant 60ms；
  - gift deliver 276ms、gift debit 102ms、debit-undo 93ms。

  Mongo 步骤（found / donate / claim / deliver）排得最长，与它们每条流串行一致。但这部分只解释了约 1s。
- **启动链路**（run5，`run5-saga-lifetime/saga-lifetime.txt`）：

  | 操作 | 发起应答 → 建出记录 | 建出 → 终态 | 终态 → 机器人看到 | 合计 |
  |---|---|---|---|---|
  | found | 平均 4085 / p95 4686 ms | 1438 / 4805 | 221 / 580 | 5744 / 9489 |
  | donate | 3746 / 4575 | 702 / 1715 | 190 / 471 | 4639 / 6247 |
  | reward | 2724 / 4117 | 634 / 1637 | 142 / 305 | 3500 / 5647 |

  启动延迟随负载变化：最早一个 2.9s，中段升到 4.6s，排空后最后一个 0.12s。这是积压的形态，不是固定定时器。
- **启动中继并发调到 16**（run7，`run7-startworkers16/saga-lifetime.txt`）：
  - 启动链路降到平均 0.46～0.53s；
  - 协调器内的时间升到 1.6～2.0s：排队移到了下一段；
  - 联盟 saga 合计从 3.5～5.7s 降到 2.4～2.9s；
  - 赠给不存在玩家的礼物 saga（步骤 20）有 5 个超过机器人 10s 的轮询上限，判失败。日志有 5 条 `saga: partition skipped an invalid input … step is not waiting for a result`：投递步骤超过 5s 步骤超时被重派，迟到的结果被拒。最后 94 / 100。
- **只调 Mongo 步骤并发**（run3，诊断补丁里 `MongoWorkers: 16`）：saga 轮询合计 17.7s，没有变化。因为启动链路先限了流。

结论：saga 这段是**框架的排队**，不是场景写法问题。多段限流串在一起：

1. 启动中继：`saga.start_effect_workers` 缺省 4，每条 StartSaga 要等分区一轮发布的 PubAck。
2. Mongo 步骤消费者：每条流串行，`StepConsumerConfig.MongoWorkers` 缺省 1，`wiring/saga.Steps` 没有开放这个配置；demo 的联盟 found / donate / claim 和礼物 deliver 都是 Mongo 步骤。
3. 协调器分区：一轮一个发布在途。

100 人同秒时，大约 500 个 saga 在 20s 内先后进入，前两段先饱和。

### add_exp（1.3～1.4s，6%）

- AddExp 是 Player + World 的 Multi 事务。`nest_handler_lock_hold` 平均约 19ms，WAL fsync 平均 11ms、最大 29～31ms。
- 100 人同秒把 World 排成一队，约 100 × 19ms ≈ 1.9s，与 p95 2.5s 相符。
- 这是已登记的 World 热点：200 人档里的 `nest: sync timeout` 就是它。
- 单个请求不慢，慢在同一实体上的串行，以及锁里等落盘。

### 场景固定成本（约 4.5s）

- 对战 1.34s、寻路 0.62s、聊天推送等待 0.51s、两次配对约 0.66s、邮件轮询约 0.85s，再加轮询的量化开销。
- 这些与负载基本无关。

### 是否像框架排队的其他指标

- `nest_dispatch_queue_len` 三个池结束时都是 0。
- `dataengine_outbox_pending` 结束时是 0，`nest: sync timeout` 0 次。
- 单请求 p95 都在 2.5s 以内（AddExp 最慢）。
- 没有看到 Nest 池排队。

## 6. 建议（只给建议，没有改门槛和 demo 逻辑）

1. **saga 启动 / 步骤并发，属于“查框架”**（推荐先做）：
   - 依据：只调启动中继，启动延迟从 4s 降到 0.5s，说明缺省并发是这一档的主瓶颈。但调完后排队移到 Mongo 步骤，并引出步骤超时重派、6 个机器人失败。
   - **不能简单把数字调大了事**，要维护者定方向：
     - **A（推荐）**：给 saga 启动链路和步骤消费者加分段指标，再定缺省值与开放的配置，用 100 人同秒复测：
       - 启动 effect 年龄（effect 提交 → StartSaga 开始）、StartSaga 耗时；
       - 步骤命令年龄（本轮诊断补丁的 `saga.step.diag_command_age`）；
       - 结果 → 下一条命令。

       要定的配置：`start_effect_workers` 缺省值；`wiring/saga.Steps` 开放 `MongoWorkers` 并让模块按步骤声明能否并发。

       代价：动 saga 框架（三大块之外，但是高频路径），要补 Mongo 步骤并发的契约测试。
     - **B**：只把 `start_effect_workers` 和 Mongo 步骤并发调大并复测。代价：run7 已经看到排队转移和步骤超时，没有分段指标就不知道下一段的上限，可能反复调。
2. **World 热点（add_exp）**：已登记的限制，100 人档只占约 1.4s，不是这次超门槛的主因，这轮不建议动。
3. **场景与门槛，属于“改场景 / 调门槛”，等 1 的结果再定**：
   - 轮询先 wait 再查，改成先查再 wait，可以省约 7 × 125～250ms。
   - 聊天推送的 500ms 选择器几乎总是等满。

   这两项合计约 1.5～2s，就算全省掉，也达不到 16s 门槛（还差 6s 以上）。16s 门槛对“同秒 100 人各跑 5 个串行 saga”是否合理，要等 saga 排队解决后，看剩下的固定成本再定：粗估空闲时整场景约 7～8s，**没测，尚未确认**。

## 7. 尚未确认

- 启动链路的 4s 里，outbox 投递（`dataengine.outbox.workers: 8`）、JetStream 发布 / 投递、启动中继（4 并发 × 每条 StartSaga 等分区一轮 PubAck）各占多少，没有拆开。
- run7 说明启动中继是第一道限流，但没测它的单条耗时。
- Mongo 步骤 found 每次 228ms 的构成（Mongo 事务 + 到联盟服务的 RPC + 联盟服务 Nest / WAL）没有拆。
- 两项都调大（启动中继 16 + Mongo 步骤并发 16）的组合没测；超时重派会不会更多也不知道。
- 礼物补偿 saga（步骤 20）比自赠（步骤 15）慢一倍左右，原因没有单独分析。
- 空闲机上整场景的底数没测。本轮各轮机器负载 5～19，run4～run7 时负载明显升高，整场景分位数不能横向比较。只有 run1 / run2 是同条件对照。

## 8. 建议写进 CHANGELOG / KNOWN-LIMITS 的文字（协调者统一写）

链接按目标文件所在目录写，所以放在代码块里，原样粘贴。

**CHANGELOG `[Unreleased]` · 新增**：

```markdown
- **Robot 逐步计时**：YAML 场景的每个顶层步骤自动计时（名字为两位序号加子树摘要，如 `30 retry(wait 250ms,alliance_op_status)`），代码搭的场景用 `scenario.Step(name, node)`。`RunSnapshot` / loadtest JSON 新增 `steps`：每步样本数、ok / failed、mean / p50 / p95 / max、`wait_mean_ms`（场景 wait 节点睡掉的时间）、`retry_attempts_avg / max`。`Runner.StepReport()` 可直接读取。`scripts/perf/demo-robot.sh` 的 `summary.json` 增加 `steps_by_mean_ms`。见 [Robot 报告解释](docs/framework/ROBOT.md#按运行的请求与流量)。
```

**KNOWN-LIMITS “机器人负载：100 / 200 人同秒起跑不通过”条目末尾追加**：

```markdown
**2026-11-18 逐步计时**（[证据](evidence/robot-step-timing-20261118/README.md)，忙机负载 5～19，两轮正式加五轮诊断）：100 人同秒整场景时间的 72%～73% 在 5 个 saga 状态轮询上（约 17s）。其余是 `add_exp` 的 World 排队约 1.4s，和场景固定等待约 4.5s（对战 1.34s、寻路、推送等待、轮询量化）。

saga 步骤体本身很快（联盟最慢的 found 228ms），时间花在启动链路排队：Nest 提交到协调器建出记录，负载下平均 3.7～4.1s，排空后 0.12s。诊断轮把 `saga.start_effect_workers` 从 4 调到 16 后，启动降到约 0.5s，但排队转移到串行的 Mongo 步骤消费者（`MongoWorkers` 缺省 1，wiring 未开放），出现投递步骤超时重派、6 / 100 失败。只调 Mongo 步骤并发没有效果。

判断是框架 saga 启动 / 步骤并发的排队，不是场景写法；改轮询写法最多省约 2s，达不到 16s 门槛。各段占比（outbox、JetStream、StartSaga、Mongo 步骤构成）**尚未确认**，待 saga 分段指标后复测再定缺省并发与门槛。
```
