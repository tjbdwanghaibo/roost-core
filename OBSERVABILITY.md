# roost 可观测性规范与指标清单

所有指标经 `cube-core/obs` 注册表（Counter / Gauge / Duration 三类），`obs.Snapshot()` 导出快照、`obs.PrometheusText(obs.Snapshot())` 输出 Prometheus 文本格式。宿主服务暴露一个抓取端点即可：

```go
http.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
    _, _ = w.Write(obs.PrometheusText(obs.Snapshot()))
})
```

## 命名规范

- **新指标一律点号分层**：`<子系统>.<对象>.<动作>`，计数器以 `_total` 结尾（Prometheus 导出时点号自动转下划线）。示例：`nest.handler.lock_hold.slow.total`、`nestwal.reject.total`。
- **存量下划线命名（`bus_*`、`checkpoint_*`、`failurelog_*`、`entitysync_*`）保持不变**——改名会打断既有采集，规范只约束新增。
- **label 基数必须有界**：handler 名、result 枚举、reason 枚举可以；实体 ID、玩家 ID、技能 ID 一律禁止（`obs.Registry` 有 series 上限兜底，但打到上限本身就是事故）。
- Duration 一律用 `obs.ObserveDuration`，不要把毫秒塞进 Counter。
- skillv2/combat 是零依赖包，**不直接接 obs**：技能侧观测由宿主适配（Runtime 的 `StateDeltas`/基准数据经宿主转发），这是设计边界不是遗漏。

## 指标清单（按面板分组）

### 调度与事务（core/nest）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `nest.dispatch.total` / `nest.dispatch.remote.total` | Counter | 分发量（labels: handler/result） |
| `nest.dispatch.cost` | Duration | 单次分发耗时 |
| `nest.dispatch.queue_len` / `worker_num` / `delayed_messages` | Gauge | 队列水位 |
| `nest.dispatch.requeue.total` | Counter | 锁冲突重排队 |
| `nest.handler.lock_hold` | Duration | 每 handler 实体锁持有时长（pipelined 灰度选型依据） |
| `nest.handler.lock_hold.slow.total` | Counter | 超阈值持锁（默认 100ms，`NestOptionWithSlowLockThreshold`） |
| `nest.pipelined.durable_wait` | Duration | Phase 1 提交等待 |
| `nest.pipelined.async_total` | Counter | Phase 2 完成结果（labels.result: ok/degraded/indeterminate——**indeterminate 非零即事故**） |
| `nest.entity_group.transition.total` | Counter | 锁组迁移 |

### Durability 管线（kit/nestwal + core/checkpoint）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `nestwal.batch.total` / `nestwal.append.total` | Counter | 组提交批数 / 记录数（比值 = 合批放大率） |
| `nestwal.bytes.total` | Counter | 写入字节 |
| `nestwal.fsync.duration` | Duration | 每批 fsync 时延（strict 锁内成本的直接来源） |
| `nestwal.pending.tickets` | Gauge | 未 durable 的 pipelined ticket 数（durable lag 的实体侧读数） |
| `nestwal.disk.bytes` | Gauge | 段文件占用（回收压力） |
| `nestwal.reject.total` | Counter | 容量拒绝（labels.reason: queue_full/disk_cap） |
| `checkpoint_journal_submit_timeout_total` | Counter | journal 背压超时 |
| `checkpoint_redis_wal_{submit,write,ack,replay,clean}_total` | Counter | Redis WAL 生命周期 |
| `checkpoint_release_gate_deferred_total`（kit） / `entitysync_flush_gate_deferred_total` | Counter | 外化闸门推迟次数（durable watermark 落后信号） |

### 缓存与总线（core）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `cache.refhmap.write_degraded_total` | Counter | Redis Lua 失败降级为非原子写（**非零需告警**：存在读到中间态的窗口） |
| `bus_dispatch_total` / `bus_dispatch_drop_total` / `bus_dispatch_duration` | C/C/D | 总线吞吐与丢弃 |
| `bus_dead_letter_total` / `_requeue_total` / `_purge_total` | Counter | 死信生命周期 |
| `bus_duplicate_total`、`bus_rpc_*` | Counter/Gauge | 去重与 RPC 水位 |
| `failurelog_*_total` | Counter | 失败日志生命周期（append/delete/purge/trim 均带 namespace label） |
| `failurelog_degraded_total{namespace,op}` | Counter | 原子 Lua 脚本失败降级为非原子回退（增长需关注：优先确认 Redis 允许 EVAL） |
| `obs.series.dropped{metric}` | Counter | 指标基数打满后被丢弃的写入数（**非零即告警**：该 metric 的新 label 组合已静默失效） |

### 跨服实体（kit/remote_entity）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `remote_entity.remote.{read,prepare,apply}_total` / `_latency` | Counter/Duration | 远程实体三段操作 |
| `remote_entity.remote.write_gate_wait` | Duration | 写闸门等待 |
| `remote_entity.remote.interest_rejected_total` | Counter | interest 拒绝 |
| `remote_entity.finalize_retry_total` / `release_failure_total` / `quarantine_error_total` / `remote_entity_transaction_tracker_drop_total` | Counter | 收尾/隔离异常（均应为零基线） |

### 帧同步（kit/lockstep）

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `lockstep.frame.total` | Counter | 切帧数（速率 ≈ 房间数 × 逻辑帧率，掉速 = tick 驱动异常） |
| `lockstep.input.late.total` | Counter | 迟到输入折入后续帧的次数（客户端上行 RTT 健康度；占比高应上调 `SubmitWindow` 或降逻辑帧率） |
| `lockstep.catchup.frames.total` | Counter | 追帧下发的历史帧数（重连/中途加入压力） |
| `lockstep.desync.total` | Counter | 关键帧哈希裁决识别的离群玩家数（**非零即事故**：作弊或确定性 bug） |

## 告警基线建议

1. `nest.pipelined.async_total{result="indeterminate"} > 0` —— 立即告警（fence 事故）。
2. `cache.refhmap.write_degraded_total` 增长 —— 告警（非原子窗口开启）。
3. `nestwal.reject.total` 增长 —— 容量预算不足。
4. `nestwal.pending.tickets` 持续爬升 —— durable 落后于提交，检查磁盘。
5. `nest.handler.lock_hold.slow.total` 新增 handler label —— 该 handler 是下一个 pipelined 灰度对象（见 NEST_PIPELINED_COMMIT.md §12）。
6. `remote_entity.release_failure_total` / `quarantine_error_total` 非零 —— 所有权收尾异常。
7. `lockstep.desync.total` 非零 —— 立即告警（确定性被破坏：作弊或模拟 bug，两者都必须查）。
8. `obs_series_dropped_total` 非零 —— 某 metric 的 label 基数打满，新组合的观测在静默丢失；排查 label 来源或上调 `WithMaxSeriesPerMetric`。

Grafana 总览面板见 [observability/grafana-roost-overview.json](observability/grafana-roost-overview.json)（按上述四组布局，导入后选择 Prometheus 数据源即可）。
