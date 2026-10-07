# C6：服务指标默认落到 metrics 注册表（Prometheus 导出）（2026-10-06）

维护者决定（[DECISIONS-PENDING 第二轮](../review/DECISIONS-PENDING-2026-10-05.md) C6）：提供默认 Prometheus adapter，名字带 ID 的指标拆成固定名 + 标签。

## 1. 现状与问题

- kit 各服务（account、activity、chat、directory、global、mail、match、platform、rank、session）都接受一个 `servicemetrics.Reporter`，
  生成工程的 `internal/service/<svc>/collaborators.go` 里 `Metrics()` 一律返回 nil；仓内唯一实现是测试用的 `Recorder`。
  服务的接受 / 拒绝 / 重放 / 丢弃 / CAS 冲突 / 深度事件在生产里无处落地（N06 观察 1、N12 O1）。
- 现有 Depth 名字里嵌了标识：`"queue."+queue.Key()`（`service/match/queue_store.go`）、`"board."+board.ID`（`kit/service/rank/redis_store.go`）。
  原样当指标名，每个队列 / 看板就是一个新指标名。
- `session.swept`（`service/session/service.go`）把一次清扫处理的条数当 gauge 报：只剩“最近一次扫了几条”，两次清扫各 3 条看起来和一次 3 条一样。
- `metrics.Registry` 没有删除序列的入口（N12 O2 / O3）：带“每次运行”“每个对象”这类标签的序列只增不减，触到每指标 2048 条上限后新组合被丢弃。

## 2. 方案

### 2.1 默认 Reporter（core `servicemetrics`）

`servicemetrics.NewMetricsReporter(service string) *MetricsReporter`，每次上报写进**当时的**进程默认注册表 `metrics.DefaultRegistry()`
（App 在启动时建注册表并设为默认，kit ops 的 `/metrics` 读的就是它；collaborators 在 App 建注册表之前就被调用，所以不在构造时捕获注册表）。

| 事件 | 指标（Prometheus 名） | 类型 | 标签 |
| --- | --- | --- | --- |
| `Accepted(op)` | `service.accepted.total`（`service_accepted_total`） | Counter | `service`、`op` |
| `Refused(op, reason)` | `service.refused.total` | Counter | `service`、`op`、`reason` |
| `Replayed(op)` | `service.replayed.total` | Counter | `service`、`op` |
| `Dropped(op, n)` | `service.dropped.total`（加 n） | Counter | `service`、`op` |
| `Conflict(op)` | `service.conflict.total` | Counter | `service`、`op` |
| `Depth(name, v)` | `service.depth` | Gauge | `service`、`name` |
| `DepthOf(name, key, v)` | `service.depth` | Gauge | `service`、`name`、`key` |

指标名固定为这 6 个，服务、操作、原因、对象都在标签里。

### 2.2 名字里的 ID 拆成标签

- 新增可选接口 `servicemetrics.KeyedReporter{ DepthOf(name, key string, value int64) }` 与 `Sink.DepthOf`：Reporter 实现了它就按
  （固定名, key）上报；没实现的（项目自写的 Reporter）退回旧形状 `Depth(name+"."+key)`，所以 `Reporter` 接口不变、已有实现照常编译。
- 调用方改为 `DepthOf("queue", queue.Key(), n)`、`DepthOf("board", board.ID, n)`。测试用 `Recorder` 实现 `DepthOf`，事件名 `depth:queue{<key>}`。
- **基数**：`key` 是队列标识（`Mode:GroupSize:Partition`）与看板 ID（`Board.ID`，如 `arena`；不含 scope / season）。两者都是业务配置里列出来的有限集合，
  通常几个到几十个；但它们由调用方传入，框架不能保证有界。一个把玩家 ID、公会 ID 拼进 `Partition` 或看板 ID 的业务会让 `service.depth`
  的序列持续增长，注册表在每指标 2048 条处丢弃新组合并计 `obs.series.dropped{metric="service.depth"}`（非零即告警），且序列删不掉。
  这一点写进 OBSERVABILITY.md 与 USER_GUIDE：队列分区与看板 ID 必须是有限枚举。
- `op` / `reason` 是各服务代码里的常量（chat 的 `publish.<kind>` / `message.evicted.<kind>` 由频道种类枚举组成），有界。
- 默认 Reporter 不加按运行、按请求、按实体的标签（N12 O2：注册表删不掉序列）。

### 2.3 `session.swept` 改成计数

`Sweep` 改报 `Dropped("run.swept", n)`（计数，累计；n = 0 不报），不再 `Depth("session.swept", n)`。Dropped 的定义本来就包括“为过期清扫掉的条目”。
已过期而被改成 expired 的另有 `Dropped("run.expired", 1)`，`run.swept` 还包括已终态、本次补做释放的 run。

### 2.4 默认装上，可以关

- 生成器写的 `collaborators.go` 与 game-demo 自带的四份 collaborators：`func Metrics() servicemetrics.Reporter { return servicemetrics.NewMetricsReporter("<svc>") }`。
- 关闭有两种：collaborators 里返回 nil（代码，原有契约）；或配置 `service_metrics.enabled: false`（默认 true，严格布尔，登记进 `frameworkBoolKeys`）。
  后者由 kit 各服务 Mod 在 Init 里经 `ServiceMetricsConfig.ApplyServiceMetrics` 统一处理：关闭时不把 Reporter 交给服务。
- 已生成的工程不改（collaborators 只创建一次）；要用默认 Reporter 时把 `Metrics()` 改成上面那一行。

## 3. 改动面

`servicemetrics/`（新 `metrics_reporter.go`、`Sink.DepthOf`、`KeyedReporter`、`Recorder.DepthOf`）、`kit/service/servicemetrics`（别名转发）、
`service/match`、`kit/service/rank`、`service/session` 的三个调用点、`kit/mods`（`ServiceMetrics`）与十个服务 Mod 的 Init、`app/config_validation.go`
（登记键）、codegen `renderFrameworkCollaborators`、demo 四份 collaborators 模板、OBSERVABILITY.md、USER_GUIDE、CHANGELOG。

## 4. 兼容

- `Reporter` 接口不变；项目自写的 Reporter 不实现 `DepthOf` 时，深度事件形状与以前完全相同。
- `Recorder` 的深度事件名对 queue / board 变了（`depth:queue.<key>` → `depth:queue{<key>}`），session 的 `depth:session.swept` 变成 `dropped:run.swept`。断言旧名字的项目测试要跟着改。
- 生成代码用到新 API（`servicemetrics.NewMetricsReporter`），新生成的工程需要 core ≥ v1.20.2：发版时 `minimumVersions.Core` 与 framework-compat 的 `minimum` 一起升（C4 的 `LoadGroupsFile` 同样要求）。

## 5. 验证

- 先红：match / rank 的深度名含队列 / 看板标识，session 报 `depth:session.swept`；生成的 collaborators 的 `Metrics()` 返回 nil。
- 绿：上面四条转绿；`MetricsReporter` 单测（真实注册表 → `PrometheusText`：六类指标的名字与标签、名字里不含 key、Dropped 累加、Depth 替换、懒解析默认注册表）；
  `mods.ServiceMetrics` 关闭开关；生成工程起真实进程（隔离 Redis / NATS），经总线调一次服务后 `/metrics` 上有 `service_*`，名字里没有 ID。

## 6. 实施状态

已实施（分支 `c4c6`，提交号见 DECISIONS-PENDING C6 行）。

- 先红（修前代码）：
  ```
  --- FAIL: TestQueueDepthDropsAndReplaysAreReported
      match_test.go:779: queue depth reported 0 under the fixed name queue keyed by the queue, want 2; accepted:enqueue=2, depth:queue.ranked:2:eu=2
  --- FAIL: TestSubmitAndPageReportWhatTheyDid
      store_test.go:536: board depth reported 0 under the fixed name board keyed by the board, want 1; accepted:submit=1, depth:board.arena=1, replayed:submit=1
  --- FAIL: TestSweepResolvesExpiredRunsAndFreesTheirClaims
      session_test.go:508: the sweep counted 0 swept runs, want 3; accepted:attach=3, accepted:enter=3, accepted:release=3, depth:session.swept=3, dropped:run.expired=3
  --- FAIL: TestGeneratedServicesReportIntoTheMetricsRegistryByDefault
      service_metrics_promises_test.go:43: internal/service/account/collaborators.go: Metrics() returns nil, want servicemetrics.NewMetricsReporter("account"): a nil reporter leaves the service's events with nowhere to go
  ```
- 真实进程（隔离环境 Redis / NATS，前缀 `c6e2e<标签>`，用完删除自己的 2 个 Redis 键）：生成工程起 `rank` 服务，临时探针进程经总线 Submit 三次、重放一次、Page 一次，再抓 `/metrics`。
  - 修前生成的工程（`Metrics()` 返回 nil）：`/metrics` HTTP 200，0 行，没有任何 `service_*`。
  - 修后生成的工程：
    ```
    service_accepted_total{op="submit",service="rank"} 3
    service_depth{key="arena",name="board",service="rank"} 3
    service_replayed_total{op="submit",service="rank"} 1
    ```
- 绿：上面四条用例；`servicemetrics` 新增 `metrics_reporter_promises_test.go`（真实注册表 → `PrometheusText`，六个固定名、标签、Dropped 累加与 0 不建序列、gauge 替换、先建 Reporter 后换注册表、无 KeyedReporter 时退回旧形状）；`kit/mods` 开关四种取值与 `off` 报错；rank Mod 关闭后不交出 Reporter。`go test -race -count=3` 覆盖 servicemetrics、kit/mods、kit/service/...、service/match、service/session、app；`go test ./codegen/...`、生成工程 build / vet / test 通过。
- 未做：注册表按标签删除序列（N12 O2 / O3，属 metrics 包的独立改动）；看板 scope / season 不进标签（会让基数随公会 / 赛季增长）。
