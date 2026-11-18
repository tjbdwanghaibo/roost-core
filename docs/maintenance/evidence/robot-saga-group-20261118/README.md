# game-demo 机器人：saga 步骤单独一组统计，门槛只卡除去 saga 的场景时间（2026-11-18）

维护者 2026-11-18 定：saga 操作“有能力就行”（能完成、结果正确），不按延迟门槛卡；框架本来就说明 saga 的吞吐不高。所以把场景里的 saga 操作单独作为一项统计，和其他步骤分开判门槛。

基线 main `8940ca73`，实现提交 `f076d4fe`（原分支 `worktree-agent-ac80204413d57b5c0`）。背景见 [robot-step-timing-20261118](../robot-step-timing-20261118/README.md)：100 人同秒档 5 个 saga 状态轮询合计约 17s，占整场景 72%～73%。

## 1. demo 里有哪些 saga 操作

场景 `demo/loadtest/scenarios/demo.yaml.tmpl` 里的 5 个 saga 都是“机器人发起 → 服务端 Nest 事务接纳并发出启动 effect → saga 协调器推进步骤 → 机器人轮询状态到终态”。计时只落在轮询步骤上（发起请求本身是普通 Nest 请求，不算 saga 组）：

| 步骤 | 操作 | 第 1 个事务域 | 第 2 个事务域 | 期望终态 |
|---|---|---|---|---|
| `15 retry(wait 250ms,gift_status)` | 赠给自己（`send_gift_self`） | game：Player 的 Bag 扣物品（原生步骤，按 sid 路由 + 准入） | mail 服务：把物品作为附件寄出（Mongo 步骤，`deliver`） | completed |
| `20 retry(wait 250ms,gift_status)` | 赠给不存在的玩家 1（`send_gift`） | 同上，扣 Bag | mail 服务拒绝投递 → 协调器补偿扣款（原生步骤 `debit-undo`，物品回 Bag） | compensated |
| `30 retry(wait 250ms,alliance_op_status)` | 建联盟（`found_alliance`） | game：Player 扣金币 9001（原生 `found-debit`） | alliance 服务：建联盟（Mongo 步骤 `found`，经 alliance RPC） | completed |
| `34 retry(wait 250ms,alliance_op_status)` | 捐献（`donate_alliance`） | game：Player 扣金币（原生 `donate-debit`） | alliance 服务：记捐献（Mongo 步骤 `donate`） | completed |
| `37 retry(wait 250ms,alliance_op_status)` | 领联盟奖励（`claim_alliance_reward`） | alliance 服务：登记领取（Mongo 步骤 `claim`） | game：Player 加物品（原生 `reward-grant`） | completed |

步骤定义：礼物在 `demo/internal/service/game/gift_saga.go.tmpl`（说明见 `demo/saga/gift_item/steps.go.tmpl`），联盟在 `modules/alliance/files/game/modules/alliance/runtime/saga.go.tmpl`。

框架文档对 saga 的定位（本轮引用，没有改）：

- [SAGA.md “跨服交易：使用约束”](../../../framework/SAGA.md#跨服交易使用约束)：一次请求的**接纳与整笔交易完成是两个时刻**；客户端收到交易号后显示“处理中”，通过状态查询确认结果；Nest handler 不应持锁等整笔 saga 完成。
- 同节容量与 [KNOWN-LIMITS “Saga”](../../KNOWN-LIMITS.md#saga) 的性能口径：正式实现是框架层正式路径的压测（macOS 单机原生正向 c128 约 1050 saga/s），`wiring/saga` 的 `Steps` 胶水**不在压测路径上**；容量“应按全链路测”（[guide/06-saga §5](../../../framework/guide/06-saga.md#5-接入与运维)）。
- [逐步计时证据](../robot-step-timing-20261118/README.md#5-归因在等什么哪里在排队)：demo 全链路上，saga 启动中继（缺省 4 并发）与 Mongo 步骤消费者（每条流串行）是限流段。

框架文档里**没有找到“saga TPS 不高”这句原话**；以上是相关的现行说明（异步接纳、处理中、容量要全链路测），以及本机全链路实测的排队形态。

## 2. 设计

原则：saga 组只看成败（`error_rate` 仍覆盖全部场景，saga 失败照样算失败）；延迟只报告不卡；原来的整场景 `p95` 指标保留。只动 `robot/` 现有包，不新建包。

### 2.1 标组

- YAML：顶层步骤可选 `group: saga`。只允许在顶层步骤上（根是 sequence 时它的元素；根不是 sequence 时根本身），其他位置写 `group` 报错。根是 sequence 时根上写 `group` 也报错（组属于步骤，不属于整个场景）。步骤名不变（仍是两位序号加子树摘要），和前一轮证据可直接对照。
- 代码：`scenario.StepInGroup(group, name, node)`；`Step(name, node)` 等于不带组。组名去首尾空白、转小写。

### 2.2 每次场景在组上花了多少时间

- 组步骤结束时，把它的耗时累计到本次场景运行的 `robot.Context`（每次场景运行新建一个 Context，looping 也是）：`Context.AddGroupTime(group, d)` / `GroupTimes()`，带锁（Parallel 分支并发）。
- 同组嵌套只算最外层（内层 Step 发现外层 frame 同组就不再累计），不会重复计。
- **限制**：Parallel 里并行的两个同组步骤各记各的，组时间可能超过墙钟时间；“除去组”的时间按 0 截底。demo 场景没有这种写法。
- Runner 只在场景**成功**时调用 `Recorder.FinishScenario(elapsed, rb.GroupTimes())`，elapsed 与整场景耗时直方图用的是同一个值。所以组与“除去组”的分位数和整场景 `quantiles_ms` 一样，只覆盖成功场景。

### 2.3 Recorder 汇总

- `Recorder` 新增成功场景的耗时直方图（与步骤统计同一种指数桶，约 9% 精度，分位数取桶上界并裁到实际 max），以及每个组两张直方图：组内时间、场景时间减组内时间。
- 一个组第一次出现时（不论来自成功还是失败场景的步骤），用“此前所有成功场景”补齐：组内时间补同样多个 0，“除去组”复制此前整场景直方图。因为此前的场景都没跑过这个组，这样补出来是精确的，以后的每个成功场景对每个已知组都记一个样本（没跑这个组记 0）。
- 组最多 32 个，超出的并入 `(overflow)`。
- `StepSample` / `StepReport` 加 `group`；组报告另计该组步骤的执行次数与失败次数（含失败场景）。
- `GroupSnapshot(include ...string)` 返回各组报告；`include` 里点名但从未出现的组也返回：组内时间全 0、“除去组” = 整场景。这样门槛点名的组在没有该组步骤的场景（例如 cellworld）上等价于整场景 p95，不会因为“没有样本”误判。

### 2.4 报告（RunSnapshot JSON）

新增 `groups`，键是组名：

```jsonc
"groups": {
  "saga": {
    "scenarios": 100,            // 成功场景数（分位数的样本数）
    "steps": 500, "steps_failed": 0,   // 该组步骤执行 / 失败次数（含失败场景）
    "group_ms":     {"mean_ms":…, "p50_ms":…, "p90_ms":…, "p95_ms":…, "p99_ms":…, "max_ms":…},  // 每个成功场景在该组步骤上的时间合计
    "excluding_ms": {"mean_ms":…, "p50_ms":…, "p90_ms":…, "p95_ms":…, "p99_ms":…, "max_ms":…}   // 场景时间减去该组时间
  }
}
```

原有 `quantiles_ms`（整场景）、`steps`（逐步，现在带 `group`）不变。

### 2.5 门槛

- 新指标 `p50_excluding:<组>` / `p90_excluding:<组>` / `p95_excluding:<组>` / `p99_excluding:<组>`，单位秒，取 `groups.<组>.excluding_ms`，样本数是 `groups.<组>.scenarios`；样本 0 照旧判违反（`no_samples`），组名为空判 `unknown_metric`。一次只除去一个组。
- 原 `p50/p90/p95/p99`（整场景，obs 直方图）不变。两者桶不同：整场景用 obs 的翻倍桶并收在最快 / 最慢之间，“除去组”用 Recorder 的约 9% 指数桶取上界，同一组数据两者可以差一个桶。
- Runner 没有实现组报告（自定义 Runner）时，除去组的门槛判 `no_samples`，不按 0 通过。

### 2.6 demo 接入

- `demo/loadtest/scenarios/demo.yaml.tmpl`：demo 场景的 5 个 saga 轮询步骤加 `group: saga`，场景逻辑不变；文件头注释说明 saga 只看成败。
- `demo/cmd/loadtest/main.go.tmpl`：新 flag `-p95-exclude-group`（缺省 `saga`）。非空时 `-max-p95`（缺省仍 16s）作用在 `p95_excluding:<组>` 上；给空串恢复整场景 `p95`。其他场景（cellworld、storm、无缝演练）没有 saga 组，门槛等价于整场景 p95，`-max-p95` 的用法不变。
- `scripts/testdata/demo-robot/capacity_test.go.txt` 显式传 `-p95-exclude-group saga`，`loadtest-args.json` 里看得到；`report.py` 的 `summary.json` 增加 `groups`。
- 同一文件里另外两个场景 `alliance_offline_join`、`alliance_cross_join` 也有建联盟的 saga 轮询（共 3 个），**一并标了 `group: saga`**，规则一致：凡是 saga 状态轮询都在组里、组里只有它们。这是本轮自己的判断（任务原文只点了 demo 场景的 5 个），维护者若不要，撤掉这 3 处即可，见第 6 节。
- 生成工程新增 `cmd/loadtest/scenarios_test.go`：用命令自己的 `loadScenarios` 加载随工程发的场景（证明当前 `robot/scenario` 认 `group`），并检查上面的分组规则。codegen 侧 `TestDemoScenarioGroupsEverySagaPollAndNothingElse` 按 YAML 结构查同一规则（生成器不能 import 运行时，根包 `TestCoreDependencyBoundary` 拦着，所以那边不调 `ParseSpec`）。

## 3. 红绿

先写桩：API 都在（`StepInGroup`、`group` 字段、`FinishScenario`、`GroupSnapshot`、`Runner.GroupReport`、`RunSnapshot.Groups`），但什么都不记、门槛不认新指标。新用例在桩上全红，原文 `red.txt`：

```
--- FAIL: TestManagerIncludesRequestAndTrafficReports   loadtest_test.go:354: group report missing: map[]
--- FAIL: TestExcludingThresholdJudgesTheScenarioWithoutTheGroup   threshold_test.go:231: … p95_excluding:saga ≤ 20ms judged failed/threshold: threshold violated: "p95_excluding:saga": unknown metric …
--- FAIL: TestRunnerReportsGroupTimeOfSuccessfulScenarios   runner_test.go:117: saga group = {Scenarios:0 Steps:0 …}
--- FAIL: TestSpecGroupsTopLevelSteps   scenario_test.go:349: steps = ["01 connect=" "02 retry(wait 4ms,poll)=" "03 wait 3ms=" "04 wait 2ms="]
--- FAIL: TestNestedSameGroupStepsCountOnce   scenario_test.go:394: group times = map[] (run took 15.305417ms)
--- FAIL: TestGroupReportSplitsGroupTimeFromTheRestOfTheScenario   report_test.go:142: groups = map[]
```

模板侧：两条分组检查在基线模板（`8940ca73` 的 `demo.yaml.tmpl` / `main.go.tmpl`）上红，原文 `red-codegen.txt`、`red-generated.txt`：demo 场景第 15 / 20 / 30 / 34 / 37 步“saga poll true but group \"\"”，`alliance_offline_join` 第 5 步、`alliance_cross_join` 第 5 / 20 步同样，codegen 侧另报 “cmd/loadtest no longer judges -max-p95 on the scenario without the saga group by default”。

实现后全绿。

### 变异

脚本每次只改一处，跑受影响的包，跑完恢复原文（`mutations.txt`）：

| # | 改动 | 变红的用例 |
|---|---|---|
| 1 | runner 成功场景不调 `FinishScenario` | `TestRunnerReportsGroupTime…`、`TestExcludingThreshold…` |
| 2 | runner 失败场景也调 `FinishScenario` | `TestRunnerReportsGroupTime…`（成功 2 个，记成 4 个） |
| 3 | 同组嵌套也累计（去掉外层同组判断） | `TestNestedSameGroupStepsCountOnce` |
| 4 | `StepInGroup` 不累计组时间 | `TestSpecGroups…`、`TestNestedSameGroup…`、`TestRunnerReportsGroupTime…`、`TestExcludingThreshold…` |
| 5 | 新组不用此前场景补齐 | `TestGroupReportSplits…`、`TestExcludingThreshold…` |
| 6 | 除去组不按 0 截底 | `TestGroupReportSplits…` |
| 7 | 只给本次跑过的组记样本（没跑的不记 0） | `TestGroupReportSplits…` |
| 8 | `GroupSnapshot` 不返回点名但未出现的组 | `TestGroupReportSplits…`、`TestExcludingThreshold…` |
| 9 | `FinishStep` 不计组步骤次数 | `TestGroupReportSplits…`、`TestRunnerReportsGroupTime…` |
| 10 | 门槛取组内时间而不是除去组 | `TestExcludingThreshold…` |
| 11 | YAML 顶层步骤的 group 不交给 `StepInGroup` | `TestSpecGroupsTopLevelSteps` |
| 12 | 嵌套位置的 group 不报错 | `TestSpecGroupsTopLevelSteps` |
| 13 | 快照不深拷贝 `Groups` | `TestManagerIncludesRequestAndTrafficReports` |

### 验证命令

- `GOWORK=off go test -count=1 -race ./robot/...`：全部 ok；`GOWORK=off go vet ./robot/... ./codegen/internal/roost/`：无输出。
- `GOWORK=off go build ./...`：ok。
- `GOWORK=off go test -count=1 -timeout 60m -run 'TestDemo' ./codegen/internal/roost/`：ok（150s；加生成工程测试与改写 codegen 用例后复跑 162s，ok）。
- 根包 `GOWORK=off go test -count=1 .`：第一次红在 `TestCoreDependencyBoundary`（codegen 测试 import 了 `robot/scenario`，生成器不得依赖运行时），改为按 YAML 结构查、真正的加载检查放进生成工程后 ok。
- 生成工程（当前生成器 `project new planet -template game-demo --skip-deps`，`replace` 指回本 worktree）：`go build ./...` ok、`go vet ./cmd/loadtest ./loadtest/...` 无输出、`go test -race -count=1 ./cmd/loadtest ./loadtest/...` ok，新用例 `TestShippedScenariosLoadAndGroupEverySagaPoll` PASS。
- 没跑：全量 `go test ./...`、pretag（按批次规则留给协调者）；生成工程的 `go generate ./...` porcelain（本轮没改 DAO / 协议 / 生成器输入，只改了模板里的场景、loadtest 命令和一个测试文件）。

## 4. 验证轮（100 人同秒）

```sh
DEMO_ROBOT_MIRROR_OFFSET=20000 DEMO_ROBOT_COUNT=100 DEMO_ROBOT_MODE=burst \
  DEMO_ROBOT_OUT=<scratchpad>/sagagroup/run1 bash scripts/perf/demo-robot.sh
```

- 源码：分支上与 main `f076d4fe` 同内容的提交（`environment.txt`，工作树干净）；端口：私有依赖偏移 20000、`port_shift` 31000（endpoint 38000、ops 40100）、etcd 35479。
- **机器不空闲**：起跑前 1 分钟负载 7.17（5 / 15 分钟 8.37 / 7.02），结束 6.40；swap 用了约 7.4 GiB（共 9 GiB）。同时有别的进程（桌面应用、系统扫描、共享隔离环境的 mongod）在占 CPU。
- 原始小文件在 `run1/`：`summary.json`、`robot.json`、`host-{before,after}.txt`、`environment.txt`、`loadtest-args.json`（含 `-p95-exclude-group saga`）、`test.log`、`diagnostic-matches.json`。

结果：**通过**，退出码 0，`TestDemoRobotCapacity` 1 个 PASS。

| 项 | 值 |
|---|---|
| 成功 / 失败 / 取消 / drop | 100 / 0 / 0 / 0 |
| 门槛 `error_rate` ≤ 0 | 0，通过 |
| 门槛 `drop_rate` ≤ 0 | 0，通过 |
| 门槛 `p95_excluding:saga` ≤ 16s | **8.32s，通过**（100 样本） |
| 整场景（只报告）p50 / p90 / p95 / p99 | 25.3 / 28.5 / 28.9 / 29.2 s |
| saga 组时间 mean / p50 / p95 / max | 20.0 / 21.7 / 22.6 / 22.6 s |
| 除去 saga 组 mean / p50 / p95 / max | 7.08 / 7.70 / 8.32 / 8.32 s |
| saga 组步骤 | 500 次，失败 0 |
| 日志 saga still / unknown、`nest: sync timeout` | 均为 0 |

说明：

- 整场景 p95 28.9s 仍远超 16s；按原门槛这轮会判失败。新门槛下除去 saga 组的 p95 8.32s，约为门槛的一半。
- saga 组的 p90 / p95 / p99 都等于 max：组时间集中在 20～22.6s，约 9% 的指数桶里最后一桶的上界裁到了实际 max。
- 和前一轮（[逐步计时](../robot-step-timing-20261118/README.md) run1，负载 5.1）比，saga 轮询合计 17s → 20s、整场景 p50 22.3s → 25.3s；本轮负载更高（7.2），单轮，不作容量对比。除去 saga 组的 7～8s 与前一轮“空闲时整场景约 7～8s”的粗估量级一致，但那个粗估本身没测过。
- 各 saga 步骤平均：建联盟 5.56s、捐献 4.52s、补偿礼物 3.96s、领奖 3.67s、自赠礼物 2.27s。

## 5. 限制

- 一个门槛只能除去一个组；要同时除去多个组得另加实现（Recorder 只按单组保存“除去组”直方图）。
- `Parallel` 里并行的同组步骤各记各的，组时间可能超过墙钟时间，“除去组”按 0 截底；demo 没有这种写法。
- “除去组”的分位数用 Recorder 的约 9% 指数桶取上界，整场景 `quantiles_ms` 用 obs 直方图（翻倍桶收在最快 / 最慢之间），两者口径不同，不能逐毫秒相减比较。
- 组报告只覆盖成功场景（与整场景门槛一致）；失败场景里的组步骤只计 `steps` / `steps_failed`。
- 生成工程的场景用了 `group:`：旧版 core 的 `ParseSpec` 开着 `KnownFields`，读到 `group` 直接报错。新生成的 game-demo 需要包含本改动的 core；`minimumVersions` 按惯例在发版打 tag 之后单独提交提升，本轮没动。

## 6. 尚未确认 / 要维护者定的

- **另外 3 个 saga 轮询也标了组**（`alliance_offline_join` 1 个、`alliance_cross_join` 2 个）。推荐保留（规则统一：saga 只看成败）；不要的话撤这 3 处 `group: saga`，并把两条分组检查改成只看 demo 场景。
- 框架文档里没有“saga TPS 不高”的原话，第 1 节引用的是“接纳与完成是两个时刻、客户端显示处理中”和“容量按全链路测、`Steps` 胶水不在压测路径上”。要不要在 SAGA.md 补一句“同步请求路径上不要按 saga 完成时间设延迟门槛”，**尚未确认**，本轮没改 SAGA.md。
- 验证只跑了一轮、忙机。“除去 saga 组”在 100 人同秒下 8.3s 的余量是否稳定，没有多轮对照，**尚未确认**。
- saga 排队本身（启动中继 4 并发、Mongo 步骤串行）没动，见逐步计时证据第 6 节；本轮只改统计与门槛口径，不解决 saga 慢。

> 维护者 2026-11-18：两个联盟入会场景里的 3 个 saga 轮询也归 saga 组，保留。

## 7. 建议写进 CHANGELOG / KNOWN-LIMITS 的文字（协调者统一写）

链接按目标文件所在目录写，原样粘贴。

**CHANGELOG `[Unreleased]` · 新增**：

```markdown
- **Robot 步骤组与“除去组”门槛**：YAML 场景的顶层步骤可写 `group: <组名>`（其他位置报错），代码搭的场景用 `scenario.StepInGroup(group, name, node)`。`RunSnapshot` / loadtest JSON 新增 `groups`：每个组的 `scenarios`（成功场景数）、`steps / steps_failed`、`group_ms`（每个成功场景在组内步骤上的时间）与 `excluding_ms`（场景时间减去组内时间），各含 mean / p50 / p90 / p95 / p99 / max；`steps` 每步带 `group`。门槛新增 `p50/p90/p95/p99_excluding:<组>`，原整场景 `p95` 等不变。`Runner.GroupReport(include...)` 可直接读取。见 [Robot 报告解释](docs/framework/ROBOT.md#按运行的请求与流量)。
```

**CHANGELOG `[Unreleased]` · 行为变化**：

```markdown
- **game-demo 机器人的 p95 门槛不再卡 saga 等待时间**（维护者 2026-11-18：saga 有能力就行）：场景里的 saga 状态轮询（demo 的 5 个、两个联盟入会场景的 3 个）标为 `saga` 组；`cmd/loadtest` 新增 `-p95-exclude-group`（缺省 `saga`），`-max-p95`（缺省 16s）判 `p95_excluding:saga`，给空串回到整场景 `p95`。saga 仍要到期望终态，失败照样计入错误率；saga 组的时间在 `groups.saga` 里报告。没有 saga 组的场景（cellworld 等）门槛不变。`scripts/perf/demo-robot.sh` 显式传 `-p95-exclude-group saga`，`summary.json` 带 `groups`。新生成的 game-demo 需要包含此改动的 core（旧 core 不认场景里的 `group`）。100 人同秒忙机一轮：100 / 100，除去 saga 组 p95 8.32s（整场景 28.9s，saga 组 p95 22.6s），[证据](docs/maintenance/evidence/robot-saga-group-20261118/README.md)。
```

**KNOWN-LIMITS “机器人负载：100 / 200 人同秒起跑不通过”条目末尾追加**：

```markdown
**2026-11-18 saga 单独成组**（[证据](evidence/robot-saga-group-20261118/README.md)）：维护者定 saga 只看成败、不卡延迟，demo 的 p95 16s 门槛改判除去 saga 组之后的场景时间。忙机一轮（负载 7.2）100 人同秒 100 / 100，除去 saga 组 p95 8.32s、通过；saga 组 p95 22.6s、整场景 28.9s 只报告。100 人同秒档按新门槛通过（单轮，余量是否稳定尚未确认）；200 人档与爬坡档没按新门槛复跑。saga 启动 / 步骤并发的排队本身没有改变，仍按逐步计时证据第 6 节待定。
```

**KNOWN-LIMITS Robot 相关处（若有 Robot 报告限制小节）补一条限制**：

```markdown
- **“除去组”门槛一次只除去一个组**：`p95_excluding:<组>` 按单组保存直方图；`Parallel` 里并行的同组步骤会让组时间超过墙钟时间（除去组按 0 截底）；“除去组”的分位数用约 9% 指数桶，整场景 `quantiles_ms` 用 obs 直方图，两者不能逐毫秒相减。
```
