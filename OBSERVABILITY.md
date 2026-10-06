# roost 可观测性规范与指标清单

所有指标经 `roost-core/metrics` 注册表（Counter / Gauge / Duration / Histogram 四类），`metrics.Snapshot()` 导出快照、`metrics.PrometheusText(metrics.Snapshot())` 输出 Prometheus 文本格式。宿主服务暴露一个抓取端点即可：

```go
http.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
    _, _ = w.Write(metrics.PrometheusText(metrics.Snapshot()))
})
```

## 命名规范

- **新指标一律点号分层**：`<子系统>.<对象>.<动作>`，计数器以 `_total` 结尾（Prometheus 导出时点号自动转下划线）。示例：`nest.handler.lock_hold.slow.total`、`nestwal.reject.total`。
- **存量下划线命名（`bus_*`、`failurelog_*`、`entitysync_*`）保持不变**——改名会打断既有采集，规范只约束新增。
- **label 基数必须有界**：handler 名、result 枚举、reason 枚举可以；实体 ID、玩家 ID、技能 ID 一律禁止（`metrics.Registry` 有 series 上限兜底，但打到上限本身就是事故）。
- Duration 一律用 `metrics.ObserveDuration`，不要把毫秒塞进 Counter。
- **需要分位数的时长用 `metrics.ObserveHistogram`**：17 个固定指数桶（1ms 起逐桶翻倍到 ~65s，`metrics.HistogramBounds()` 可查），`metrics.HistogramQuantile(name, labels, q)` 桶内线性插值取分位数；Prometheus 导出为标准累积 `_bucket{le}` + `_sum_nanos` + `_count`，可直接喂 `histogram_quantile()`。桶固定意味着无采样窗口——它是进程生命期累积分布，压测类"单场分布"要在场景开始前 `metrics.Reset()` 或用 label 区分场次。
- skillv2/combat 是零依赖包，**不直接接 obs**：技能侧观测由宿主适配（Runtime 的 `StateDeltas`/基准数据经宿主转发），这是设计边界不是遗漏。

## 指标清单（按面板分组）

### 调度与事务（core/nest）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `nest.dispatch.total` / `nest.dispatch.remote.total` | Counter | 分发量（labels: handler/result） |
| `nest.dispatch.cost` | Duration | 单次分发耗时 |
| `nest.dispatch.slow_trace_suppressed` | Counter | 被进程级采样窗口抑制的重复全堆栈诊断，无实体 ID 标签 |
| `nest.dispatch.queue_len` / `worker_num` / `delayed_messages` | Gauge | 队列水位 |
| `nest.dispatch.requeue.total` | Counter | 锁冲突重排队 |
| `nest.handler.lock_hold` | Duration | 从进入事务执行到调用 release 前或事务返回；不含等锁及完整 release hook 成本，保留旧口径 |
| `nest.handler.lock_hold.slow.total` | Counter | 超阈值持锁（默认 100ms，`NestOptionWithSlowLockThreshold`） |
| `nest.pipelined.durable_wait` | Duration | 内联时为 ticket 等待；异步时从建立完成任务至解锁/前序完成后，不能当成纯 fsync 耗时 |
| `nest.pipelined.async_total` | Counter | Phase 2 完成结果（labels.result: ok/degraded/completion_failed/indeterminate——**indeterminate 非零即事故**） |
| `nest.entity_group.transition.total` | Counter | 锁组迁移 |

Nest 200ms 慢请求继续逐请求记录日志和耗时；全 goroutine 堆栈按进程每 5 秒至多采样一次。下一次堆栈日志中的 `suppressed_since_last_trace` 表示跳过的重复诊断数，避免依赖变慢时日志和运行时抓栈放大压力。

`NestOptionWithStageMetrics(true)` 开启 `nest.stage.duration{handler,stage}`（Duration，默认关闭）。

| stage | 计时边界 |
| --- | --- |
| queue | 本次消息进入 worker 准入前 → 开始 dispatch；主动延迟不计入，重排队重新起算 |
| load / lock | 普通路由和广播的实体加载 / 初始实体及组锁获取；动态 Cast 成本包含在 handler 中 |
| capture / handler | 事务初始实体捕获 / 业务调用（包含内部动态 Cast） |
| prepare / enqueue / durable_commit | 构建提交记录 / pipelined Enqueue / strict Commit 调用；不是磁盘层 fsync 分解 |
| admission | 成功准入回调与 Sync 锁内冻结；不重复统计后续 Commit 的幂等准入 |
| durable_wait / commit_queue | 当前执行者等待 ticket / ticket 进入异步等待队列后至被取出 |
| release / cleanup | 路由实体释放（含 hook）/ 外层 Guard 剩余清理与 after-unlock 回调 |
| completion_queue / completion_unlock / completion_order | 已完成 ticket 等完成 worker / 等本事务解锁 / 等同主实体前序完成 |
| completion / rollback / remote_confirm | 异步完成回调与回复 / 失败回滚 / 已存在远端写批次的最终收尾 |

阶段只在实际经过的路径上产生；广播按实体采样。并发阶段可能重叠，嵌套事务、动态实体提前释放等成本也可能包含在外层阶段，不能直接相加视为端到端延迟。Duration 用于次数、总耗时和平均值诊断，不提供 p99；客户端延迟分位数仍由业务压测统计。详细成本和复跑命令见 [Nest 收尾验收](docs/feature/NEST-COMPLETION-2026-09-24.md)。

### Durability 管线（kit/dataengine + kit/nestwal）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `nestwal.batch.total` / `nestwal.append.total` | Counter | 组提交批数 / 记录数（比值 = 合批放大率） |
| `nestwal.bytes.total` | Counter | 写入字节 |
| `nestwal.fsync.duration` | Duration | 每批 fsync 时延（strict 锁内成本的直接来源） |
| `nestwal.pending.tickets` | Gauge | 未 durable 的 pipelined ticket 数（durable lag 的实体侧读数） |
| `nestwal.disk.bytes` | Gauge | 段文件占用（回收压力） |
| `nestwal.reject.total` | Counter | 容量拒绝（labels.reason: queue_full/disk_cap） |
| `entitysync_durability_gate_deferred_total` | Counter | 被 durable watermark 推迟的 subject 捕获次数（整个 subject 本 tick 不发） |
| `entitysync_frames_admitted_total` | Counter | 传输层接受的会话帧数 |
| `entitysync_sessions_lost_total` | Counter | 因推送失败被 Manager 关闭的会话数（`SessionLost` 回调政策） |

### 缓存与总线（core）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| ~~`cache.refhmap.write_degraded_total`~~ | — | **v1.19.0 起已移除**（RR-20261004-NC-21）：RefHMap Eval 失败不再降级为非原子写，而是保留原始错误返回调用方；按业务判别结果未知（USER_GUIDE §4）。旧面板 / 告警请删除这一项 |
| `bus_dispatch_total` / `bus_dispatch_drop_total` / `bus_dispatch_duration` | C/C/D | 总线吞吐与丢弃 |
| `bus_dead_letter_total` / `_requeue_total` / `_purge_total` | Counter | 死信生命周期 |
| `bus_duplicate_total`、`bus_rpc_*` | Counter/Gauge | 去重与 RPC 水位 |
| `failurelog_*_total` | Counter | 失败日志生命周期（append/delete/purge/trim 均带 namespace label） |
| `failurelog_degraded_total{namespace,op}` | Counter | 适配器没有 Lua（Eval 返回空结果）时走了非原子回退；生产驱动不应出现，非零说明接了不支持脚本的 IRedis。脚本报错（结果未知）不降级，计入 `failurelog_append_total{result="error"}` 等并返回错误（RR-20261005-NC-160） |
| `obs.series.dropped{metric}` | Counter | 指标基数打满后被丢弃的写入数（**非零即告警**：该 metric 的新 label 组合已静默失效） |
| `log.rotate_failures` | Counter | 日志轮转打不开新分片的次数（EMFILE / ENOSPC / 权限）。失败期间继续写上一分片、1s 后重试（RR-20261005-NC-263）；**非零即查磁盘与日志目录** |
| `log.write_errors{sink}` | Counter | 日志写失败的行数，`sink` 为 `console` / `file`；一个 sink 出错不影响另一个（NC-263）。slog 吞掉写错误，这是唯一能看到日志在丢的地方 |

### 跨服实体（kit/remote_entity）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `remote_entity.remote.{read,prepare,apply}_total` / `_latency` | Counter/Duration | 远程实体三段操作 |
| `remote_entity.write_admission_rejected_total` | Counter | Remote 完整写生命周期预算耗尽；Stats 同时公开 WritesInFlight / WriteLimit / WriteRejected |
| `remote_entity.remote.write_gate_wait` | Duration | 写闸门等待 |
| `remote_entity.remote.interest_rejected_total` | Counter | interest 拒绝 |
| `remote_entity.finalize_retry_total` / `release_failure_total` / `quarantine_error_total` / `remote_entity_transaction_tracker_drop_total` | Counter | 收尾/隔离异常（均应为零基线） |
| `remote_entity.deferred_outcome_not_run_total{outcome}` | Counter | Remote 结果未知的事务拿到持久结论时 Nest 已停机 / 已 fence，提交后工作（AfterCommit、Sync 放行）未执行（RR-20260926-61）；停机窗口外应为零 |

### 帧同步（kit/lockstep）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `lockstep.frame.total` | Counter | 切帧数（速率 ≈ 房间数 × 逻辑帧率，掉速 = tick 驱动异常） |
| `lockstep.input.late.total` | Counter | 迟到输入折入后续帧的次数（客户端上行 RTT 健康度；占比高应上调 `SubmitWindow` 或降逻辑帧率） |
| `lockstep.input.rejected.total{reason}` | Counter | 被拒输入/哈希上报（unknown_player/too_early/payload_too_big/hash_*——识别恶意或错版客户端的第一现场） |
| `lockstep.catchup.frames.total` | Counter | 追帧下发的历史帧数（重连/中途加入压力） |
| `lockstep.desync.total` | Counter | 关键帧哈希裁决识别的离群玩家数（**非零即事故**：作弊或确定性 bug） |

### 机器人 / 压测（core/robot）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `robot.session.call{msg,result}` | Histogram | 每次请求/响应调用的时延分布；result 枚举 ok/timeout/closed/error/encode_error/send_error/mismatch/decode_error——非 ok 占比是被测服务的第一告警面 |
| `robot.session.late_response{msg}` | Counter | Call 已超时 / 放弃之后才到的应答（seq 非 0 且没有等待者），直接丢弃、不当推送分发（RR-20261005-NC-262）；持续增长说明被测服务的应答慢于剧本超时 |
| `robot.runner.scenario.cost{profile,run,scenario,result}` | Histogram | 一次场景执行的全程耗时（ok/error/canceled）；loadtest 阈值裁决从它取 p50–p99。`run` label 每场压测唯一——同 profile 连跑两场分布互不污染（series 随场次增长，靠 series 上限兜底，长驻进程注意场次频率） |
| `robot.runner.scenario.total{profile,run,scenario,result}` | Counter | 场景执行结果计数（error_rate 的分母/分子） |
| `robot.runner.target` / `robot.runner.online{profile,run,scenario}` | Gauge | 目标并发 vs 实际在线机器人（Stages 升降是否按预期跟随） |
| `robot.loadtest.active{profile}` | Gauge | 该 profile 是否有活跃 run（单活跃约束的可视化） |

### 实体定时器（core/timer）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `timer.unhandled_dropped_total{kind}` | Counter | 到期时因类型（`kind` = 类型号）没有注册 handler 而被删除的定时器节点（维护者决定 D-L2）。**基线应为零**；非零说明存储里还有某种已下线 / 漏注册类型的节点，它们在到期时被丢弃。每次删除另有 Warn `timer: dropped a due timer with no handler registered for its type`；宿主加载时对这类存量类型每种告警一次（`timer: stored timers have no handler registered for their type`）。宿主事务回滚后重试同一次 Tick 会再计一次 |

### 服务事件（kit/service 与 core/service，维护者决定 C6）

生成工程的每个托管服务默认用 `servicemetrics.NewMetricsReporter("<服务名>")`（`internal/service/<svc>/collaborators.go` 的 `Metrics()`），事件写进进程的 metrics 注册表、经 ops `/metrics` 导出。关闭：配置 `service_metrics.enabled: false`（所有 kit 服务 Mod 在 Init 时不再把 Reporter 交给服务），或在 collaborators 里返回 nil。[方案](docs/feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md)

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `service.accepted.total{service,op}` | Counter | 完成并改了状态的操作 |
| `service.refused.total{service,op,reason}` | Counter | 业务原因拒绝（限额、权限、过期令牌），不是基础设施故障 |
| `service.replayed.total{service,op}` | Counter | 由幂等记录直接回答、没有再执行的请求；上升说明传输在重投 |
| `service.dropped.total{service,op}` | Counter | 按保留期 / 上限丢弃、清扫掉的条数（按条数累加）。session 的 `op="run.swept"` 是每次清扫处理的 run 数（以前是 gauge `session.swept`，只剩最后一次的读数） |
| `service.conflict.total{service,op}` | Counter | CAS 重试耗尽；同一逻辑键争用的信号 |
| `service.depth{service,name[,key]}` | Gauge | 当前大小。match 的队列长度是 `name="queue",key="<Mode:GroupSize:Partition>"`，rank 的看板大小是 `name="board",key="<Board.ID>"`（以前队列 key、看板 ID 拼在名字里） |

基数：`service` / `op` / `reason` / `name` 都是代码里的常量或枚举。`key` 是调用方给的对象标识：**队列的 Partition 与看板 ID 必须是有限枚举**（区服、模式、看板种类），不能把玩家 / 公会 / 场次 ID 拼进去——默认 Reporter 不会替你删序列（注册表从第十二轮起有 `metrics.DeleteSeries(name, labels)`，由对象的拥有者在销毁时调用，loadtest 的 `run` 序列就是这样随运行记录删除的），`service.depth` 到每指标 2048 条后新组合被丢弃并计 `obs.series.dropped{metric="service.depth"}`。看板的 scope / season 不进 key，同一种看板的不同 scope 共用一条序列（读到的是最近一次 Page 的那个）。默认 Reporter 不加按运行、按请求、按实体变化的标签。项目自写的 Reporter 不实现 `servicemetrics.KeyedReporter` 时，深度事件仍以旧形状 `Depth("queue.<key>")` 到达，由它自己决定怎么拆。

### 持久化与 fence（kit/dataengine）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `dataengine.load.skipped.total{resource}` | Counter | 非 strict 载入模板无法解码而跳过的行数。**基线应为零**；持续增长意味着字段改名/编解码变更正在让整表静默加载不全（strict 模板会直接失败，非 strict 只跳过，所以这条曲线是它唯一的信号） |
| `dataengine.fence.skipped.total{resource}` | Counter | 被 lease fence 拦下、整笔标记为 skipped 的事务数。**陈旧 saga worker 偶发是正常的**；但"所有被 fence 的事务同时开始跳过"意味着 fence 谓词已不可满足（claim schema 漂移），而这两种情况从进程内部无法区分——只能靠曲线形状判断：稳定低速率 = 正常，阶跃到与事务量同阶 = 事故 |

## 健康与就绪（`/healthz`、`/readyz`）

kit `ops` Mod 提供两个探针端点，checker 经 `health.Registry` 注册（`app.ModHealth`），每项结果是 `ok` / `degraded` / `fail`：

| 端点 | 200 的条件 | 503 的条件 | 探针 |
| --- | --- | --- | --- |
| `/healthz` | 进程的 HTTP 在答就 200 | — | startup / liveness |
| `/readyz` | 就绪位为真（`service.started` 之后、`service.stopping` 之前），且没有 checker 为 `fail` | 就绪位为假，或任一 checker 为 `fail`（未知状态按 `fail` 算） | readiness、compose healthcheck、shell `healthcheck.sh`、`make dev-run` |

**每个 checker 有期限**（维护者第十二轮决定）：checker 并发调用，各自最多等 1.5s（`health.DefaultCheckTimeout`），到期未返回记 `fail`（`message: check timed out`，`error` 写明期限与已跑时长），不再拖住整个 `/readyz`；同一个 checker 同一时刻只有一次调用，卡住的不会每次探针多一个。

**Degraded 算就绪**（维护者决定 D1，2026-10-06）：有 `degraded` 时 `/readyz` 仍返回 200、`ok: true`，响应体 `degraded: true`，`degraded_dependencies` 列出每个降级项的 `name` / `status` / `message` / `error`；`dependencies` 照样列出全部 checker。之前 Degraded 与 Fail 一样返回 503。现有的 Degraded 来源都是“还能服务、需要关注”：单实例锁续期结果未知（`singleton`，≤ `singleton.renew_interval` 的窗口）、entitysync 主体 / 会话 ≥ 80% 容量、remoteentity 写许可用满、DataEngine 投影积压告警。`fail` 是“不能再安全工作”：fenced、Projector / Outbox 不健康、容量用尽、已关闭、失锁或未持有单实例锁。

部署侧的探针都只看 HTTP 状态码，Degraded 不再让 k8s 摘掉 endpoint，也不会让 compose / shell 部署判为未就绪；要对降级告警，抓 `/readyz` 响应体的 `degraded`，或看各来源自己的指标（`entitysync_*`、`remote_entity.*`、`dataengine_*`）。

## 告警基线建议

1. `nest.pipelined.async_total{result="indeterminate"} > 0` —— 立即告警（fence 事故）。
2. ~~`cache.refhmap.write_degraded_total` 增长~~ —— v1.19.0 起该指标已移除（RR-20261004-NC-21，Eval 失败直接返回错误，不再有非原子降级窗口）。
3. `nestwal.reject.total` 增长 —— 容量预算不足。
4. `nestwal.pending.tickets` 持续爬升 —— durable 落后于提交，检查磁盘。
5. `nest.handler.lock_hold.slow.total` 新增 handler label —— 该 handler 是下一个 pipelined 灰度对象（见 NEST_PIPELINED_COMMIT.md §12）。
6. `remote_entity.release_failure_total` / `quarantine_error_total` 非零 —— 所有权收尾异常。
7. `lockstep.desync.total` 非零 —— 立即告警（确定性被破坏：作弊或模拟 bug，两者都必须查）。
8. `dataengine_fence_skipped_total` 速率阶跃到与被 fence 事务量同阶 —— 立即告警（claim schema 漂移：事务正在静默变成 no-op，既无错误也无失败测试）。
9. `obs_series_dropped_total` 非零 —— 某 metric 的 label 基数打满，新组合的观测在静默丢失；排查 label 来源或上调 `WithMaxSeriesPerMetric`。
10. `/readyz` 响应体 `degraded: true` 持续数分钟 —— 关注（不会摘流量，见上一节）：按 `degraded_dependencies[].name` 查对应来源。

Grafana 总览面板见 [observability/grafana-roost-overview.json](observability/grafana-roost-overview.json)（按上述四组布局，导入后选择 Prometheus 数据源即可）。
