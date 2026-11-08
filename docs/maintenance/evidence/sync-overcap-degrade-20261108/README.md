# 超额降级（D17）：证据（2026-11-08）

对应 [方案 §18 / D17](../../FEATURE-2026-11-08-editable-terrain.md#18-超额降级减少同步对象不断开d172026-11-08)、[ENTITY_SYNC“超额降级”](../../../framework/ENTITY_SYNC.md#超额降级会话持有对象超过上限) 与 [impl/04-sync §2G](../../../framework/impl/04-sync.md#2g-超额降级会话持有对象超过上限d17)。

## 先红后绿

**修前红（b4699758）**：把两条被改写的断言换成新契约、只用 b4699758 已有的 API，在 b4699758 的副本上跑：[red-before.txt](red-before.txt)。

- `TestOneFramePerSessionPerTickSplitOnlyAtTheObjectLimit`（`MaxObjects: 2` 订阅第三个对象）：`a session over its object budget was closed: {… Sessions:0 … SessionsLost:1 …} cause=entitysync: session lost (object_limit): frame: object limit exceeded`；
- `TestALostSessionDisconnectsThePlayerUnlessThePushWasRefused` 第 2a 步：`an object over MaxObjects: lost [entitysync: session lost (object_limit): frame: object limit exceeded], closed [7], frames 0; want it deferred with the connection untouched`。

红落在承诺本身（会话没关、连接没断），不是错误文本。新增的用例（`over_limit_test.go`、`interest_over_limit_test.go`、生成工程的三个）用到新 API（`ManagerStats.OverLimit*`、`SubscribePinned`、`Deferred`），在 b4699758 上编译不过，不拿编译失败当红；它们的保护力由下面的变异证明。

**变异（本实现上改坏一处，对应用例变红）**：[mutations.txt](mutations.txt)。

| 变异 | 变红的用例 |
| --- | --- |
| M1 `admitWithinLimits` 什么都不做（回到编码时超限） | `TestOneFramePerSessionPerTickSplitOnlyAtTheObjectLimit`、`TestNamespaceObjectLimitsAreCountedPerClass`、`TestOverLimitDefers…`、`…PinnedSubscriptionEvicts…`、`…PinningAWaitingSubscriptionAdmitsIt`、`…HoldReadyStartsOverFromZero` 等（会话以 `encode_failed` 被关；有的用例因此等不到收敛，跑到 go test 的 10 分钟超时） |
| M2 Interest 收到通知不收紧 | `TestInterestShrinksAFullSessionToTheNearest`（两种模式：客户端留着 ID 最小、最远的 `[1 2 3 4]`）、`…MaxVisibleAboveTheSessionLimit…`、`…RelationPairMakesTheMainViewGiveWay` |
| M3 不再周期再通知 | `TestOverLimitRenotifiesWhileStillWaiting` |
| M4 钉住的订阅不挤已持有的 | `TestOverLimitPinnedSubscriptionEvictsTheLowestPriority`、`TestOverLimitChurnKeepsTablesConsistent`（churn 里从没发生挤让，用例拒绝“什么都没证明”的运行） |

**未超额逐字节不变**：`TestUnderTheLimitFramesAreByteIdentical` 跑一段从不超出上限的交付（多会话、两类上限含满额时同 tick 的 remove + create 替换、快照预算、增量、Hold/Ready、held 会话、注销、换 profile），全部帧的 SHA-256 是 `e59a0815…3efd4`，与把同一场景放到 b4699758 上跑出的值相同。

**修后绿**：本分支的 entitysync / policy / wiring 测试（含 `-race`）与生成工程全量测试，见方案 §18 的验证清单。生成工程改写 / 新增的用例与两个相邻的不超额用例输出：[demo-green.txt](demo-green.txt)（地形视野 80 > 64：825 次 Flush 后稳定在 64 块，累计暂缓 9 块，用例耗时约 2 秒（含每秒一次的再通知等待）；视野满 64 块的用例没有任何暂缓）。

## 基准对照（同机交替）

- 基线 b4699758，新：本分支。两边各编译 entitysync 与 policy 的测试二进制，`base` / `new` 交替各跑 6 轮（`-test.count 1 -test.benchmem`，两个包的全部基准）。Apple M5，机器有后台负载、swap 已用约 8GB（耗时方差大，见 ± 列）。
- 终版：[base-es.txt](base-es.txt) / [new-es.txt](new-es.txt) → [benchstat-entitysync.txt](benchstat-entitysync.txt)；[base-pol.txt](base-pol.txt) / [new-pol.txt](new-pol.txt) → [benchstat-policy.txt](benchstat-policy.txt)；AOI 观察者结构收紧后单独复核 `BenchmarkAOIBatchRecompute`：[aoi-base.txt](aoi-base.txt) / [aoi-new.txt](aoi-new.txt) → [benchstat-aoi-recheck.txt](benchstat-aoi-recheck.txt)。

| 指标 | 结论 |
| --- | --- |
| frames / packs / wire-B | 全部相同（未超额时交付逐字节不变） |
| allocs/op | 全部相同 |
| sec/op（entitysync） | `ExhaustedSnapshotWindow` +1.4%（p=0.048，230→234ns），其余无显著差异 |
| B/op（entitysync） | `SessionRecovery` +1.3%：`sessionLifetime` 多一个暂缓索引指针，48→56 字节，分配档 48→64，每次 Hold 每会话多 16 字节；其余相同 |
| sec/op（policy） | `AOIBatchRecompute/dense=false/coverage=false/workers=4` 三次分别 +8.4% / +11.1% / +7.1%（约 9µs 档，±3～8%）；同一路径 workers=1 与其余七个组合无显著差异。原因**尚未确认**（改动在这条路径上只多两次分支判断；观察者结构收紧回原大小后仍在） |
| B/op（policy） | 无显著差异 |

**第一轮发现并改掉的两处**（[bench-round1](bench-round1/benchstat-entitysync.txt)）：

1. `ExhaustedSnapshotWindow` +10.3%（235→259ns）：每次 Flush 无条件 `time.Now()` 给再通知用。改为先看有没有暂缓的订阅、有才取时钟，回到 +1.4%。
2. `subscription` 结构 96→136 字节（分配档 96→144，每个订阅多 48 字节）：`overLimit` 挪进 `inFlight` 的填充字节，通知时刻从 `time.Time` 改为单调时钟纳秒 `int64`，现为 112 字节（分配档 112，每个订阅多 16 字节；`pinnedBy` 指针与通知时刻各 8 字节，没再压）。AOI 观察者结构的 `limited` / `limit` 挪进两个 bool 所在的字长（`limit` 用 `int32`），观察者大小与 b4699758 相同。

## 环境

本机 macOS / Apple M5，`GOWORK=off`，Go 版本见各文件头；不依赖外部服务（这批用例都是内存传输与真实 TCP 环回）。
