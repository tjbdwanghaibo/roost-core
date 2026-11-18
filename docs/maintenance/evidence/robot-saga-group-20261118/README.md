# game-demo 机器人：saga 步骤单独一组统计，门槛只卡除去 saga 的场景时间（2026-11-18）

维护者 2026-11-18 定：saga 操作“有能力就行”（能完成、结果正确），不按延迟门槛卡；框架本来就说明 saga 的吞吐不高。所以把场景里的 saga 操作单独作为一项统计，和其他步骤分开判门槛。

基线 main `8940ca73`。背景见 [robot-step-timing-20261118](../robot-step-timing-20261118/README.md)：100 人同秒档 5 个 saga 状态轮询合计约 17s，占整场景 72%～73%。

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

- `demo/loadtest/scenarios/demo.yaml.tmpl`：5 个 saga 轮询步骤加 `group: saga`，场景逻辑不变。
- `demo/cmd/loadtest/main.go.tmpl`：新 flag `-p95-exclude-group`（缺省 `saga`）。非空时 `-max-p95`（缺省仍 16s）作用在 `p95_excluding:<组>` 上；给空串恢复整场景 `p95`。其他场景（cellworld、storm、无缝演练）没有 saga 组，门槛等价于整场景 p95，`-max-p95` 的用法不变。
- `scripts/testdata/demo-robot/capacity_test.go.txt` 显式传 `-p95-exclude-group saga`，`loadtest-args.json` 里看得到；`report.py` 的 `summary.json` 增加 `groups`。
