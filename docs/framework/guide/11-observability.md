# 11 可观测与运维（说明）

> 配套实现文档：[impl/11-observability.md](../impl/11-observability.md)（文件地图、主流程、不变量与守卫测试、并发、review 检查点、源码疑点）。
> 源码基准：tag `v1.23.0`（`28912cd6`），文中 `path:line` 都按这个 tag。codebase-memory 图谱的代际是 2026-09-30：`metrics/metrics.go`、`health/health.go`、`kit/ops/ops_mod.go`、`log/log.go`、`failurelog/failurelog.go`、`kit/statslog/statslog.go` 等是 `metadata_changed`，`servicemetrics/metrics_reporter.go`、`app/business_time.go`、`nest/dispatcher_series.go` 是 `not_tracked`。本篇结论全部按 tag 源码直接读取，指标总表由脚本扫描全仓 `IncCounter / SetGauge / AddGauge / ObserveDuration / ObserveHistogram` 调用点生成后人工核对。

## 速览

- **这一块是什么**：每个进程一个指标注册表（`metrics.Registry`，四种类型：Counter、Gauge、Duration、Histogram）、一个健康检查注册表（`health.Registry`）、一个 admin 命令注册表（`admin.Registry`），由 App 在 `NewRegistry` 时建好（`app/registry.go:34-39`）；kit 的 ops Mod 把它们放到一个 HTTP 端口上：`/healthz`、`/readyz`、`/metrics`、`/statsz`、`/admin/commands`、`/admin/execute`。另有结构化日志（`log`）、Redis 失败列表（`failurelog`，现只给总线死信用）、安全原语（`security`）和服务事件上报（`servicemetrics`）。
- **最重要的保证**：① 指标基数有上界：每个指标名最多 2048 条序列（`metrics.max_series_per_metric` 可调），超出的新组合被丢弃并计入 `obs_series_dropped_total{metric}`。② `/readyz` 不会被卡住的 checker 拖死：checker 并发跑，每个最多 1.5s，到期报 Fail。③ Degraded 算就绪（D1）：只有 Fail 或就绪位为假才 503。④ `/admin/*` 只认 `X-Admin-Token` 或 `Authorization: Bearer <token>`，常数时间比较；命令在 `ops.admin_timeout`（缺省 10s）内执行，超时回 504 并写明“结果未知”。
- **最容易踩的坑**：① 指标名不受任何守卫：`OBSERVABILITY.md`、手写总览仪表盘、生成的 demo 仪表盘都可能引用不存在的名字或标签（已知 lockstep 丢前缀 F04-5、总览仪表盘 `entitysync_flush_gate_deferred_total`、demo 仪表盘 `nats_jetstream_terminal_total{subject}`，见 [impl §11](../impl/11-observability.md#11-源码疑点与文档不一致)）。② 标签只能放有限枚举；总线异步消息的 `module,msg` 目前取自对端，没有上界（F05-5）。③ 生产环境 ops 端口绑公网必须同时写 `ops.allow_public_addr: true`，生成的生产示例绑的是 `0.0.0.0:9100`。④ `runtime_*`、`entity_count*` 这些 gauge 只在 stats_log 采集或有人访问 `/statsz` 时刷新；`stats_log.enabled` 缺省是 false。⑤ entitysync 的 `CheckHealth` 没有任何生产注册方，它不在 `/readyz` 里（F04-3）。

### 本篇覆盖的包

| 包 / 文件 | 职责 |
| --- | --- |
| `metrics` | 指标注册表、序列上限、`DeleteSeries`、直方图分位数、Prometheus 文本导出 |
| `health` | checker 注册表、并发快照、每 checker 期限、Degraded 聚合 |
| `admin` | admin 命令注册表与执行（panic 恢复、结果归一）、命令元数据注册表 |
| `kit/ops` | ops Mod：配置 `ops.*`、就绪位、HTTP 端点、Bearer 鉴权、admin 期限 |
| `kit/statslog` | stats_log Mod：周期采集运行时 / 实体 / Nest 数据写 JSONL，同步发布 gauge，给 `/statsz` 供数 |
| `log` | slog 封装：上下文属性、按时间轮转、多 sink 扇出与写失败计数、`ELog` |
| `failurelog` | Redis 列表式失败记录（追加 + 裁剪 + TTL，原子脚本），现唯一用户是 bus 死信队列 |
| `security` | 会话 token（HMAC）、请求体签名、按 key 的令牌桶限流 |
| `servicemetrics`（及 `kit/service/servicemetrics` 别名） | 服务事件契约 `Reporter`、nil 安全的 `Sink`、默认生产实现 `MetricsReporter`、测试用 `Recorder` |
| `app/registry.go`、`app/config_schema.go`（`metrics.*`、`log.*`） | 三个注册表的创建；日志与序列上限配置 |
| `OBSERVABILITY.md`、`observability/grafana-roost-overview.json` | 规范与指标清单（手写）、总览仪表盘（手写） |
| 生成工程 `demo/deploy/dev/observability/**` | 开发机 Prometheus + Grafana、生成的 demo 仪表盘 |

跨分区说明：
- ops Mod 的就绪位挂在哪两个生命周期 hook、`/readyz` 在启动与停机序列里的位置、单实例锁 checker，见 [01 app 说明 §6.3](01-app-lifecycle.md#63-readiness)。
- 各来源自己的指标含义（Nest 派发、WAL、entitysync、Remote、saga、kit 服务）在各自分区：[02](02-nest-entity.md)、[03](03-dataengine.md)、[04](04-sync.md)、[05](05-remote-mirror.md)、[06](06-saga.md)、[09](09-kit-services.md)。本篇给全仓总表和统一规则。
- 业务时间高水位推进失败计数的语义见 [10 时间](10-time.md)。
- 生成器怎么渲染 ops 配置、仪表盘与 dev-run，见 [12 代码生成](12-codegen.md)。

---

## 1. 定位与边界

**负责**

- 进程内的指标、健康、admin 三个注册表，以及把它们暴露出去的 ops 端点。
- 指标命名与基数规则、序列生命周期（`DeleteSeries`）、Prometheus 文本格式。
- 结构化日志的格式、轮转、写失败可见性。
- 失败记录列表（死信）的原子追加 / 删除 / 清空。
- 会话 token、请求签名、限流这几个安全原语。
- 服务事件上报的统一契约。
- 仪表盘与 `OBSERVABILITY.md` 的口径。

**不负责**

- 分布式追踪（没有 OpenTelemetry / trace span；`fctx` 里的 `Trace` 只进日志属性）。
- 指标的远端存储与告警引擎（Prometheus、Alertmanager 由部署方提供；框架只给文本端点）。
- 日志收集（只写 stdout 与本地文件；轮转之后的清理、收集由部署方做）。
- 各模块“出了什么事”的业务含义：只在本篇总表里列名字，含义见各分区。
- 玩家接入层的限流接线（`gateway.RateLimit` 与 player_agent 的兼容问题见 [04 F04-13](../impl/04-sync.md#102-本篇写作时发现的源码疑点待闭环)）。

→ [实现文档](../impl/11-observability.md)对应：§1 包与文件地图。

## 2. 核心概念与术语

| 术语 | 含义 | 位置 |
| --- | --- | --- |
| 指标注册表 | `metrics.Registry`：按 `名字 + 排序后的标签` 建序列，每种类型一张表 | `metrics/metrics.go:170` |
| 默认注册表 | 包级函数（`metrics.IncCounter` 等）写入的那个；App 的 `NewRegistry` 每次把自己新建的设为默认 | `metrics/metrics.go:184`、`app/registry.go:36` |
| 序列 / 序列上限 | 一个名字 + 一组标签值是一条序列；每个名字最多 `maxSeriesPerMetric`（缺省 2048）条，打满后新组合被丢弃 | `metrics/metrics.go:21`、`:638` |
| `obs.series.dropped` | 被上限丢弃的写入次数，按指标名一条，`Snapshot` 时合成 | `metrics/metrics.go:466` |
| `DeleteSeries` | 按名字（可空）+ 标签子集删序列并归还名额；空 match 不删 | `metrics/metrics.go:511` |
| Duration（timer） | 只记次数、总和、最大、最近，没有分位数 | `metrics/metrics.go:72`、`:389` |
| Histogram | 17 个固定指数桶（1ms 起翻倍到约 65.5s），桶内插值并钳在 [min,max] 内估分位数 | `metrics/metrics.go:28`、`:133` |
| checker | `health.Checker`，返回 ok / degraded / fail | `health/health.go:53` |
| 就绪位 | ops Mod 的 `ready`：`service.started` 置真、`service.stopping` 置假 | `kit/ops/ops_mod.go:154-173` |
| Degraded | “还能服务、需要关注”；不影响 `Snapshot.OK` | `health/health.go:29-40` |
| admin 命令 | `admin.CommandDef{Name, Description, Handler}`，经 `/admin/execute` 执行 | `admin/admin.go:44` |
| Reporter | 服务事件契约：Accepted / Refused / Replayed / Dropped / Conflict / Depth | `servicemetrics/servicemetrics.go:15` |
| failurelog | Redis 列表：RPUSH + LTRIM + PEXPIRE 一条脚本完成 | `failurelog/failurelog.go:17` |
| stats_log 记录 | 一次采集：运行时、实体计数、Nest 池、自定义 provider，写成一行 JSON | `kit/statslog/statslog.go:51`、`:398` |

→ [实现文档](../impl/11-observability.md)对应：§2 关键类型与数据结构。

## 3. 设计原因

| 取舍 | 为什么 | 出处 |
| --- | --- | --- |
| 自带注册表，不用 prometheus/client_golang | 零依赖、热路径是原子加，标签用 map 方便；代价是没有 `# TYPE` / `# HELP` 行，Duration 导出成四个独立序列 | `metrics/prometheus.go:13` |
| 每指标序列上限 + 丢弃计数，而不是无限增长 | 攻击者或 bug 控制的标签会把内存撑爆；打满的指标用 `obs_series_dropped_total{metric}` 自己报警 | `metrics/metrics.go:638-662` |
| `DeleteSeries` 由“对象的拥有者”在销毁时调用 | 带动态标签（一次压测运行、一个派发器）的序列在对象消失后停在最后值、占名额；谁拥有对象谁删 | [R12 §2](../../feature/DECISIONS-R12-KIT-2026-10-06.md#2-metrics-按标签删除)、`metrics/metrics.go:503-510` |
| 直方图分位数钳在观测到的 [min,max] 内 | 桶翻倍，桶上界插值能把 9.6s 报成 16.384s，压测门限误判 | [RR-20261006-27](../../bugfix/RR-20261006-27.md) |
| checker 并发 + 每个 1.5s 期限 + 同一 checker 只有一次在途调用 | 串行时一个卡住的 checker 让 `/readyz` 永远挂着，k8s 每次探针多留一个 goroutine | [R12 §3](../../feature/DECISIONS-R12-KIT-2026-10-06.md#3-readyz-每个-checker-的期限) |
| Degraded 算就绪 | 单副本服务被摘掉唯一 endpoint；entitysync 在 80% 容量边界来回翻转 | [D1](../../feature/D1-READYZ-DEGRADED-IS-READY-2026-10-06.md) |
| `Authorization` 必须带 `Bearer ` | 裸头或别的 scheme 不能被当成 admin token | [R12 §4](../../feature/DECISIONS-R12-KIT-2026-10-06.md#4-ops-必须带-bearer) |
| admin 命令有期限、写超时比期限多 5s | 长命令回复写不出去时客户端只见 EOF，不知道执行了没有 | [OPS-ADMIN-TIMEOUT](../../feature/OPS-ADMIN-TIMEOUT-2026-10-06.md) |
| ops 在 `Start` 里同步 bind | 端口被占时进程不能“没有探针地”跑下去，探针会探到别的进程 | [RR-20261005-NC-230](../../bugfix/RR-20261005-NC-230.md) |
| 生产环境 ops 只能绑回环，绑公网要显式声明 | `/metrics`、`/statsz`、`/readyz` 不鉴权；打开 admin 后可执行全部命令 | `kit/ops/ops_mod.go:78-97` |
| 服务事件按事件命名、不按指标名 | 拼错的指标名没人看得见；加事件是所有实现的编译错误 | `servicemetrics/servicemetrics.go:6-14`、[C6](../../feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md) |
| 日志多 sink 各自写、写失败计数 | `io.MultiWriter` 在第一个失败处停；slog 吞掉写错误，丢行无痕 | [RR-20261005-NC-263](../../bugfix/RR-20261005-NC-263.md) |
| failurelog 脚本报错不降级 | 连接断开可能发生在脚本执行之后，降级就是第二次执行 | [RR-20261005-NC-160](../../bugfix/RR-20261005-NC-160.md) |

→ [实现文档](../impl/11-observability.md)对应：§9 历史与重要修复。

## 4. 怎么用

### 4.1 打开 ops 端点

生成工程的共享 Mod 缺省就是 `lock`、`ops`、`statslog`（`codegen/internal/roost/manifest.go:192`），装配由生成的 bootstrap 完成。只需在服务配置里打开：

```yaml
ops:
  enabled: true
  addr: 127.0.0.1:9100          # 生产绑 0.0.0.0 时要同时写 allow_public_addr: true
  admin_enabled: false          # 打开时 admin_token 必填
  admin_token: ""
  admin_timeout: 10s
stats_log:
  enabled: true                 # 缺省 false；关着时 runtime_* / entity_count* gauge 不刷新
```

验证：`curl -s 127.0.0.1:9100/readyz | jq .`、`curl -s 127.0.0.1:9100/metrics | head`。

### 4.2 写指标

```go
metrics.IncCounter("mymod.job.done.total", metrics.Labels{"result": "ok"}, 1)
metrics.ObserveDuration("mymod.job.cost", nil, time.Since(start))      // 平均值诊断
metrics.ObserveHistogram("mymod.call.latency", nil, time.Since(start)) // 需要分位数时
metrics.SetGauge("mymod.queue.len", metrics.Labels{"queue": "main"}, int64(n))
```

规则（`OBSERVABILITY.md:11-18` 加本篇核对）：

1. **新名字点号分层**：`<子系统>.<对象>.<动作>`，计数器以 `.total` 或 `_total` 结尾都行（导出时点号变下划线，计数器缺 `_total` 时自动补，`metrics/prometheus.go:54-62`）。存量下划线名（`bus_*`、`failurelog_*`、`entitysync_*`、`player_tcp_*`）不改。
2. **标签只放有限枚举**：handler 名、result、reason、注册过的方法名。玩家 / 实体 / 公会 / 场次 ID、错误文本一律不行。对端传来的名字要先对照本进程注册表，未注册的归并成一个固定值（参照 `bus/jetstream_rpc.go:664-689` 的 `_unregistered` / `_other`）。
3. **同一个名字固定一组标签键**。混用“有标签”和“无标签”会让 `sum by (reason)` 出现空串分组（现存例子见总表 `player_tcp_*` 备注）。
4. **Gauge 不以 `_total` 结尾**（现存反例 `bus_rpc_pending_total`）。
5. **带动态标签的序列由拥有者删除**：真实例子 `nest/dispatcher_series.go:57-72`（派发器排空后按名字计数删除）、`robot/loadtest/manager.go:620-632`（运行挤出历史时删 `run` 序列）。
6. **加一个指标后同步改 `OBSERVABILITY.md` 与用到它的仪表盘**。目前没有守卫替你检查（[impl §11 N3](../impl/11-observability.md#11-源码疑点与文档不一致)）。

压测类“单场分布”：直方图是进程生命期累积的，要么给场次一个标签并在结束后 `DeleteSeries`，要么在场景开始前 `Registry.Reset()`。判断分位数前先看 `metrics.HistogramCount`，0 表示没有数据（可能被上限丢弃了），不是 0ms（[RR-20261005-NC-161](../../bugfix/RR-20261005-NC-161.md)）。

### 4.3 注册健康检查

在 Mod 的 `Provide` 里从 Registry 取 `*health.Registry` 并登记。真实例子 `kit/redis/redis_mod.go:117-133`：

```go
healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
healthReg.Register("redis", health.CheckerFunc(func(ctx context.Context) health.Result {
    if err := asm.Ping(ctx); err != nil {
        return health.Result{Status: health.StatusFail, Message: "ping failed", Err: err}
    }
    return health.Result{Status: health.StatusOK, Message: "connected"}
}))
```

要点：

- **配合 ctx**：快照给每个 checker 1.5s（`health/health.go:71`）。不配合 ctx 的 checker 到期被记 Fail，那次调用继续在后台跑，同一 checker 同一时刻只有一次在途。
- **Status 只用三个值**。空串当 OK，其他未知值当 Fail（`health/health.go:249`、`:177-183`）。
- **Degraded 用于“还能服务”**：容量接近上限、结果未知窗口、积压告警。不能再安全工作才是 Fail。
- **名字唯一**：同名再登记会静默替换前一个（`health/health.go:114-121`）。
- 业务对象（例如 entitysync `Manager`、demo 的 `Scene`）有 `CheckHealth` 不代表它在 `/readyz` 里，必须有人 `Register`。entitysync 目前没有（F04-3）。

### 4.4 admin 命令

登记到 App 的 admin 注册表（不是包级默认注册表，包级的那份装配路径从不读，`hotcode/admin.go:24-27`）：

```go
commands, _ := app.Lookup[*admin.Registry](registry, mods.ModAdmin)
_ = commands.Register(admin.CommandDef{
    Name: "gm.player.add_item", Description: "...",
    Handler: func(ctx context.Context, cmd admin.Command) (admin.Result, error) {
        p, err := admin.DecodePayload[addItemPayload](cmd)
        ...
        return admin.Result{Data: map[string]any{"ok": true}}, nil
    },
})
```

真实例子：框架自带 `hotcode.list / revert / load_plugin`（`hotcode/admin.go:28-80`）、`bus.dlq.list / requeue / purge`（`bus/admin.go:24-80`，kit nats Mod 自动登记 `kit/nats/nats_mod.go:170`）、`robot.loadtest.*`（`robot/loadtest/admin.go:26`）；game-demo 的 14 个 GM 命令（`demo/internal/service/game/gm.go.tmpl:43-58`、`:179`、`:502-509`）。

调用：

```bash
curl -s -H 'Authorization: Bearer <token>' 127.0.0.1:9100/admin/commands
curl -s -X POST -H 'X-Admin-Token: <token>' 127.0.0.1:9100/admin/execute \
  -d '{"name":"gm.player.add_item","trace_id":"t-1","operator":"alice","payload":{"player_id":1,"item_id":2,"count":1}}'
```

要点：handler 必须配合 ctx（超过 `ops.admin_timeout` 回 504、结果未知）；同名 `Register` 静默替换（`admin/admin.go:117-131`，`TestRegistryRegisterReplacesExistingCommand`）；可能重复执行的命令自己用 `trace_id` 做幂等。框架**不记审计日志**（[impl §11 N5](../impl/11-observability.md#11-源码疑点与文档不一致)），需要审计的命令在 handler 里自己记。

### 4.5 服务事件（servicemetrics）

服务保存 `servicemetrics.Wrap(reporter)` 的结果，所有调用点无条件调用（nil 安全，`servicemetrics/servicemetrics.go:51-110`）。生成工程每个托管服务的 `collaborators.go` 返回 `servicemetrics.NewMetricsReporter("<服务名>")`（例如 `demo/internal/service/account/collaborators.go.tmpl:184`；其余服务由 `codegen/internal/roost/framework_services.go:360` 生成）。关闭：配置 `service_metrics.enabled: false`（每个 kit 服务 Mod 共用 `kit/mods/service_servicemods.go:66-77` 的声明），或在 collaborators 里返回 nil。

`DepthOf(name, key, v)` 的 `key` 必须是有限集合（配置里的队列、看板种类），默认 Reporter 不替你删序列。测试里用 `servicemetrics.NewRecorder()` 断言事件（`servicemetrics/recorder.go`）。

### 4.6 日志

App 在 Mod Init 之前按 `log.*` 初始化（`app/app.go:190-202`），业务直接用 `log/slog` 或本包的 `log.Info / Warn / Error`、`ELog`。每行自动带：

| 属性 | 来源 |
| --- | --- |
| `goId` | 当前 goroutine id（`DisableGoID` 可关） |
| `frame` | 请求上下文的帧号，没有时取 `nest.CurTick`（`app/app.go:201`） |
| `server_time_ms` | **请求上下文里是业务时间**（`fctx` 钉住的那个），上下文外是系统时间（`log/log.go:273-277`；见 [impl §11 N11](../impl/11-observability.md#11-源码疑点与文档不一致)） |
| `source` / `handler` / `player` / `msg` / `seq` | `fctx` 的 `RequestMeta`，有值才写 |
| `caller` / `caller_func` | `log.caller: true` 时 |

文本格式里这些属性排在 `level` 与 `msg` 之间（`log/ordered_text_handler.go`，`TestRuntimeAttrsAreWrittenBetweenLevelAndMessage`）。`log.json: true` 改用 slog 的 JSON handler。

### 4.7 failurelog

当前唯一用户是 bus 的死信队列：`bus/reliable.go:104-114` 以 `Namespace: "bus_dlq"`、`TTL = nats.reliable.dlq_ttl`（缺省 7 天）、`MaxEntries` 缺省 10000 创建，键是 `<prefix>:dlq:<module>:<msg>`（`bus/reliable.go:221-233`）。运维通过 admin 命令 `bus.dlq.list / requeue / purge` 操作。死信语义见 [05 说明](05-remote-mirror.md)。

两条要知道的行为：TTL 在每次追加时整表刷新（`failurelog/failurelog.go:23-26`），所以列表在最后一次追加 7 天后整体过期，单条不会单独过期；超过 `MaxEntries` 时最老的条目被裁掉，生产路径上**没有计数**（[impl §11 N4](../impl/11-observability.md#11-源码疑点与文档不一致)）。

### 4.8 安全原语

| 原语 | 用法 | 用户 |
| --- | --- | --- |
| `SignSessionToken(playerID, secret, ttl, now)` / `VerifySessionToken(token, secret, expectPlayerID, now)` | token = `base64url("<playerID>:<expiresAtMs>:<nonce>") + "." + base64url(HMAC-SHA256)`；`ttl<=0` 取 1h；`now` 为零取 `time.Now` | `kit/service/account/service.go:293`、`:317`；`kit/service/platform/service.go:243`、`:256` |
| `SignPayload` / `VerifyPayloadSignature` | hex(HMAC-SHA256)，校验接受 `sha256=` 前缀，空签名或空 secret 一律拒绝 | `httpclient/client.go:178`；`kit/service/platform/service.go:318` |
| `NewRateLimiter(cfg)` / `AllowN(key, n)` | `{OwnerID, Action}` 令牌桶；缺省容量 20、每秒补满、全表 10 万 key、每主体 256 key、闲置 10 分钟回收 | `gateway/middleware.go:33`（`gateway.RateLimit`） |

secret 的生产校验在配置层：带 `secret:"true"` 的键在生产环境不许为空或以 `dev-` 开头（`internal/configschema/check.go:236-240`）。`ops.admin_token` **没有**这个标记，生产环境的 `dev-` token 只靠 `ops.allow_dev_token` 和 `roost config check --production` 的整文件扫描拦（`codegen/internal/roost/doctor.go:822-830`）。

### 4.9 stats_log 与 `/statsz`

stats_log Mod 每 `stats_log.interval` 采集一次：goroutine、堆、GC、按 category / kind 的实体数、Nest 两个池的队列与本窗口处理量、业务登记的 provider（`RegisterProvider(name, fn)`），写一行 JSON 到 `<dir>/<server_type>-<sid>.stats.log`，同时把运行时与实体计数发布成 gauge（`kit/statslog/statslog.go:189-205`）。`/statsz` 现场采一次同样的记录返回（`kit/ops/ops_mod.go:328-339`）；没有装 stats_log Mod 的进程回 404。

注意：`/statsz` 会把 Nest 窗口的起点移到这次调用（[impl §11 N6](../impl/11-observability.md#11-源码疑点与文档不一致)），把 JSONL 里各行的 `processed_messages` 相加会少算。

→ [实现文档](../impl/11-observability.md)对应：§3 主流程。

## 5. 配置

| 键 | 类型 / 缺省 | 校验 | 说明 |
| --- | --- | --- | --- |
| `ops.enabled` | bool / false | — | 打开 ops 端点 |
| `ops.addr` | string / `127.0.0.1:9100` | 生产 + enabled + 非回环 + 未声明 `allow_public_addr` → 拒绝启动（`kit/ops/ops_mod.go:92-95`） | 写端口 0 由系统分配 |
| `ops.allow_public_addr` | bool / false | — | 端点在鉴权代理之后才写 true |
| `ops.admin_enabled` | bool / false | 打开时 `admin_token` 非空（`:84-87`） | 关着时 `/admin/*` 回 404 |
| `ops.admin_token` | string / 空 | `dev-` 前缀要 `allow_dev_token`（`:88-90`） | 不是 `secret` 键 |
| `ops.allow_dev_token` | bool / false | — | 生产不看它（见 §4.8） |
| `ops.admin_timeout` | duration / 10s | `min:"1ns"` | 命令期限；写超时 = max(15s, 它 + 5s) |
| `metrics.max_series_per_metric` | int / 0（取 2048） | `min:"0"` | `app/config_schema.go:229`；`NewRegistry` 时读一次 |
| `log.level` | string / info | debug / info / warn(ing) / error 或 slog 的数字写法（`log/log.go:169-190`） | |
| `log.json` | bool / false | — | |
| `log.stdout` / `log.file` | bool / true / true | — | 两个都 false 时退回 stdout（`log/log.go:73-88`） |
| `log.dir` | string / `log` | — | 文件名 `<server_type>-<sid>.log` |
| `log.caller` | bool / false | — | |
| `log.rotate_interval` | duration / 24h | 无下限；≤0 不轮转 | 24h 对齐本地零点，其余对齐 Unix 纪元（`log/rotation.go:152-162`） |
| `log.rotate_time_format` | string / 按间隔推断 | — | `20060102` / `2006010215` / … |
| `stats_log.enabled` | bool / false | — | |
| `stats_log.dir` / `stats_log.filename` | `log` / `<server_type>-<sid>.stats.log` | — | 相对工作目录；部署要给可写挂载（T-177） |
| `stats_log.interval` | duration / 1m | `min:"1ns"` | |
| `service_metrics.enabled` | bool / true | — | false 时本进程全部 kit 服务不上报 |
| `nats.reliable.dlq_ttl` | duration / 7 天 | `min:"1ns"` | 死信列表 TTL，见 [05](05-remote-mirror.md)；条数上限 10000 没有配置键（`bus/reliable.go:21`、`:46-48`） |

健康检查期限（1.5s）不是配置键，只能代码里 `health.Registry.SetCheckTimeout` 改。

→ [实现文档](../impl/11-observability.md)对应：§2、§7 持久化 / 协议格式。

## 6. 运行与运维

### 6.1 端点

| 端点 | 鉴权 | 200 | 非 200 |
| --- | --- | --- | --- |
| `GET /healthz` | 无 | HTTP 在答就 200，体 `{ok, service, sid}` | — |
| `GET /readyz` | 无 | 就绪位为真且没有 Fail | 503；体见 §6.2 |
| `GET /metrics` | 无 | Prometheus 文本 0.0.4（无 `# TYPE` 行） | — |
| `GET /statsz` | 无 | 一次 stats 采集的 JSON | 404（没装 stats_log）/ 503（没有 Registry） |
| `GET /admin/commands` | token | `{ok, commands:[名字]}` | 404 admin 关 / 401 |
| `POST /admin/execute` | token | `admin.Result` | 404 / 401 / 400（解码失败、未知命令、handler 返回错误）/ 504（期限到、结果未知） |

请求体上限 1 MiB（`kit/ops/ops_mod.go:29`）。

### 6.2 `/readyz` 与健康来源

响应体字段：`ok`、`degraded`、`degraded_dependencies[]`、`service`、`sid`、`message`（starting / service started / service stopping）、`server_time_ms`（业务时间）、`metrics`（当前序列条数）、`dependencies[]`（每项 `name / status / message / error / checked_at_ms`）。

| checker 名 | 登记处 | OK | Degraded | Fail |
| --- | --- | --- | --- | --- |
| `singleton` | `app/singleton.go:639` | 持锁 | 续期结果未知（窗口内） | 失锁 / 未持有 |
| `nest` | `kit/nest/nest_mod.go:224` | 引擎在跑 | — | 引擎未跑 |
| `dataengine` | `kit/dataengine/mod.go:315` | 正常 | 投影积压告警 `BacklogWarning` | 未就绪、fenced、Projector / Outbox 不健康 |
| `saga` | `kit/saga/mod.go:163` | 正常（消息里带各类计数） | — | 未运行、消费者停、worker 停、Mongo ping 失败 |
| `remote_entity` | `kit/remoteentity/remote_entity_mod.go:146` | 正常 | 写许可用满 | fatal、兴趣表或事务表满 |
| `remote_mirror` | `kit/remoteentity/remote_mirror_mod.go:150` | 正常 | 本地兴趣表满 | 未初始化、停机中 |
| `redis` / `mongo` / `etcd` | `kit/redis/redis_mod.go:121`、`kit/mongo/mongo_mod.go:94`、`kit/etcd/etcd_mod.go:97` | ping 通 | — | 未初始化 / ping 失败（自带 2s，被 1.5s 截短） |
| `nats` | `kit/nats/nats_mod.go:125` | 已连接 | — | 未初始化 / 断开 |
| `sync` | `kit/syncbus/mod.go:191` | bus 非空即 OK（不探传输） | — | bus 未初始化 |
| entitysync `Manager.CheckHealth` | **无生产注册方** | — | ≥80% 容量 | 满 / 已关闭（F04-3，`sync/entitysync/manager.go:1018`） |

`OBSERVABILITY.md:154` 与 T-267 列的 Degraded 来源含 entitysync、不含 `remote_mirror`，按上表为准。

### 6.3 全仓指标总表

167 个指标名（含 `obs.series.dropped`），按写入位置所在的包分组。“OBS”列表示 `OBSERVABILITY.md` 是否收录（按名字或通配写法匹配）：收录 74 个，未收录 88 个，名字不符 5 个（lockstep）。导出名规则：点号与其他非法字符变 `_`；Counter 不以 `_total` 结尾时补 `_total`；Duration 导出 `_count / _sum_nanos / _max_nanos / _last_nanos` 四个序列；Histogram 导出 `_bucket{le}`（le 以秒计）+ `_sum_nanos` + `_count`。标签后带 `?` 的表示只在部分写入点出现。

不在表里的两组：`syncstream`（`syncstream_*`、`roost_sync_*`）与 `skillsync`（`skillsync_*`）用自己的 `MetricSink` 接口（float64）导出，**仓内没有任何生产调用方、也没有接到 `metrics.Registry` 的适配器**（`syncstream/observability.go:31`、`syncstream/observability_impl.go:31`、`skill/skillsync/observability.go:39`），这些名字不会出现在 `/metrics` 里。

<!-- 下表由扫描脚本生成后人工核对；行号按 v1.23.0 -->
#### app

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `app.business_time.advance_failed.total` | `app_business_time_advance_failed_total` | Counter | — | `app/business_time.go:219` | 否 | OBSERVABILITY 未收录（F10 T6） |

#### bus

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `bus_dead_letter_purge_total` | `bus_dead_letter_purge_total` | Counter | `module,msg` | `bus/bus.go:212` | 是 |  |
| `bus_dead_letter_requeue_total` | `bus_dead_letter_requeue_total` | Counter | `module,msg` | `bus/bus.go:198` | 是 |  |
| `bus_dead_letter_total` | `bus_dead_letter_total` | Counter | `module,msg` | `bus/bus.go:1045` | 是 | `module,msg` 取自对端未注册名，无上界（F05-5） |
| `bus_dispatch_drop_total` | `bus_dispatch_drop_total` | Counter | `module,msg,reason` | `bus/bus.go:801` | 是 | 同上（F05-5）；`reason` 只有两种 worker 错误 |
| `bus_dispatch_duration` | `bus_dispatch_duration_{count,sum_nanos,max_nanos,last_nanos}` | Duration | `module,msg` | `bus/bus.go:859` | 是 |  |
| `bus_dispatch_total` | `bus_dispatch_total` | Counter | `module,msg` | `bus/bus.go:894` | 是 |  |
| `bus_duplicate_total` | `bus_duplicate_total` | Counter | `module,msg` | `bus/bus.go:1010` | 是 |  |
| `bus_rpc_call_total` | `bus_rpc_call_total` | Counter | `transport,method,result,reason?` | `bus/jetstream_rpc.go:740` | 是 | `method` 有界（≤256 + `_other`，RR-20261006-19） |
| `bus_rpc_consumer_delivery` | `bus_rpc_consumer_delivery` | Gauge | `method,transport` | `bus/jetstream_rpc.go:466` | 是 |  |
| `bus_rpc_pending` | `bus_rpc_pending` | Gauge | `method,transport` | `bus/jetstream_rpc.go:658` | 是 |  |
| `bus_rpc_pending_total` | `bus_rpc_pending_total` | Gauge | `transport` | `bus/jetstream_rpc.go:655` | 是 | Gauge 却以 `_total` 结尾 |
| `bus_rpc_request_total` | `bus_rpc_request_total` | Counter | `method,result,transport` | `bus/jetstream_rpc.go:744` | 是 | `method` 有界（已注册 + `_unregistered`） |

#### cache

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `cache.layered.backfill_failed.total` | `cache_layered_backfill_failed_total` | Counter | — | `cache/layered.go:63、:75、:91` | 否 |  |

#### dataengine

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `dataengine.load.skipped.total` | `dataengine_load_skipped_total` | Counter | `resource` | `dataengine/load.go:166` | 是 |  |

#### dataengine/engine

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `dataengine.fence.evictions.failed.total` | `dataengine_fence_evictions_failed_total` | Counter | — | `dataengine/engine/fenced_step.go:171` | 否 |  |
| `dataengine.fence.evictions.started.total` | `dataengine_fence_evictions_started_total` | Counter | — | `dataengine/engine/fenced_step.go:117` | 否 |  |
| `dataengine.fence.skipped.total` | `dataengine_fence_skipped_total` | Counter | `resource` | `dataengine/engine/mongo_store.go:333` | 是 |  |

#### entity

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `entity.unload_resync.backlog` | `entity_unload_resync_backlog` | Gauge | — | `entity/unload_resync.go:190` | 否 |  |
| `remote_entity.snapshot_bootstrap_overflow_total` | `remote_entity_snapshot_bootstrap_overflow_total` | Counter | — | `entity/remote_snapshot.go:557` | 否 |  |
| `remote_entity.snapshot_bootstrap_replay_failed_total` | `remote_entity_snapshot_bootstrap_replay_failed_total` | Counter | — | `entity/remote_snapshot.go:573` | 否 |  |

#### failurelog

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `failurelog_append_total` | `failurelog_append_total` | Counter | `namespace,result` | `failurelog/failurelog.go:110、:114、:118、:122、:128` | 是 |  |
| `failurelog_degraded_total` | `failurelog_degraded_total` | Counter | `namespace,op` | `failurelog/failurelog.go:252` | 是 |  |
| `failurelog_delete_total` | `failurelog_delete_total` | Counter | `namespace,result` | `failurelog/failurelog.go:200、:204、:209、:212` | 是 |  |
| `failurelog_purge_total` | `failurelog_purge_total` | Counter | `namespace,result` | `failurelog/failurelog.go:157、:160` | 是 |  |
| `failurelog_trim_total` | `failurelog_trim_total` | Counter | `namespace` | `failurelog/failurelog.go:418` | 是 | 只在无 Lua 的降级路径计数（§6.6 N4） |

#### kit/configdata

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `configdata.reload.total` | `configdata_reload_total` | Counter | `result` | `kit/configdata/configdata.go:80、:82` | 否 |  |
| `configdata.rollback.total` | `configdata_rollback_total` | Counter | `trigger` | `kit/configdata/configdata.go:75、:84` | 否 |  |
| `configdata.version` | `configdata_version` | Gauge | — | `kit/configdata/configdata.go:90` | 否 |  |

#### kit/statslog

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `entity.count` | `entity_count` | Gauge | — | `kit/statslog/statslog.go:204` | 否 | 同上 |
| `entity.count_by_category` | `entity_count_by_category` | Gauge | `category` | `kit/statslog/statslog.go:221、:225` | 否 | 同上；缺席的键写回 0 |
| `entity.count_by_kind` | `entity_count_by_kind` | Gauge | `kind` | `kit/statslog/statslog.go:221、:225` | 否 | 同上；缺席的键写回 0 |
| `runtime.goroutines` | `runtime_goroutines` | Gauge | — | `kit/statslog/statslog.go:199` | 否 | 只在 stats_log 采集或 `/statsz` 时刷新 |
| `runtime.heap_alloc_bytes` | `runtime_heap_alloc_bytes` | Gauge | — | `kit/statslog/statslog.go:200` | 否 | 同上 |
| `runtime.heap_sys_bytes` | `runtime_heap_sys_bytes` | Gauge | — | `kit/statslog/statslog.go:201` | 否 | 同上 |
| `runtime.num_gc` | `runtime_num_gc` | Gauge | — | `kit/statslog/statslog.go:203` | 否 | 同上（累计值，用 `rate`） |
| `runtime.sys_bytes` | `runtime_sys_bytes` | Gauge | — | `kit/statslog/statslog.go:202` | 否 | 同上 |
| `stats_log.write_failures` | `stats_log_write_failures_total` | Counter | — | `kit/statslog/statslog.go:290` | 否 |  |

#### log

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `log.rotate_failures` | `log_rotate_failures_total` | Counter | — | `log/rotation.go:104` | 是 |  |
| `log.write_errors` | `log_write_errors_total` | Counter | `sink` | `log/log.go:356` | 是 |  |

#### manager

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `manager.start.duration` | `manager_start_duration_{bucket,sum_nanos,count}` | Histogram | `manager` | `manager/engine.go:176` | 否 |  |
| `manager.started` | `manager_started` | Gauge | — | `manager/engine.go:196、:270` | 否 |  |

#### metrics（注册表自身）

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `obs.series.dropped` | `obs_series_dropped_total` | Counter | `metric` | `metrics/metrics.go:466-476`（`Snapshot` 时合成） | 是 | 每个打满的指标名一条；**非零即告警** |

#### mongo

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `mongo.ensure_index.election_retries.total` | `mongo_ensure_index_election_retries_total` | Counter | — | `mongo/driver/collection.go:305` | 否 |  |

#### nats

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `nats.jetstream.settle_failures.total` | `nats_jetstream_settle_failures_total` | Counter | `op` | `nats/driver/jetstream.go:126` | 否 |  |
| `nats.jetstream.terminal.total` | `nats_jetstream_terminal_total` | Counter | `reason` | `nats/driver/jetstream.go:115` | 否 | `reason`=permanent / max_deliver；demo 仪表盘按 `subject` 聚合 |
| `nats.rpc.callback.latency` | `nats_rpc_callback_latency_{bucket,sum_nanos,count}` | Histogram | — | `nats/driver/rpc.go:361` | 否 |  |
| `nats.rpc.completed.total` | `nats_rpc_completed_total` | Counter | — | `nats/driver/rpc.go:359` | 否 |  |
| `nats.rpc.pending` | `nats_rpc_pending` | Gauge | — | `nats/driver/rpc.go:238、:358` | 否 |  |
| `nats.rpc.queue_rejected.total` | `nats_rpc_queue_rejected_total` | Counter | — | `nats/driver/rpc.go:374` | 否 |  |
| `nats.rpc.started.total` | `nats_rpc_started_total` | Counter | — | `nats/driver/rpc.go:237` | 否 |  |
| `nats.subscription.handler_panic.total` | `nats_subscription_handler_panic_total` | Counter | — | `nats/driver/client.go:302` | 否 |  |

#### nest

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `nest.dispatch.cost` | `nest_dispatch_cost_{count,sum_nanos,max_nanos,last_nanos}` | Duration | `handler,type,result` | `nest/trace.go:36` | 是 |  |
| `nest.dispatch.delayed_messages` | `nest_dispatch_delayed_messages` | Gauge | `dispatcher` | `nest/dispatcher.go:321` | 是 | 同上 |
| `nest.dispatch.fast_continuations` | `nest_dispatch_fast_continuations` | Gauge | `dispatcher` | `nest/dispatcher.go:320` | 否 | 同上 |
| `nest.dispatch.queue_len` | `nest_dispatch_queue_len` | Gauge | `dispatcher,pool` | `nest/dispatcher.go:330` | 是 | 派发器排空停止后 `DeleteSeries`（RR-20261006-18） |
| `nest.dispatch.remote.total` | `nest_dispatch_remote_total` | Counter | `handler,type,result` | `nest/trace.go:38` | 是 |  |
| `nest.dispatch.requeue.total` | `nest_dispatch_requeue_total` | Counter | `reason,type` | `nest/group_transition.go:395` | 是 |  |
| `nest.dispatch.slow_reroute.total` | `nest_dispatch_slow_reroute_total` | Counter | `dispatcher` | `nest/dispatch_queue.go:305` | 否 | 同上 |
| `nest.dispatch.slow_trace_suppressed` | `nest_dispatch_slow_trace_suppressed_total` | Counter | — | `nest/trace.go:125` | 是 |  |
| `nest.dispatch.total` | `nest_dispatch_total` | Counter | `handler,type,result` | `nest/trace.go:35` | 是 |  |
| `nest.dispatch.worker_num` | `nest_dispatch_worker_num` | Gauge | `dispatcher,pool` | `nest/dispatcher.go:331` | 是 | 同上 |
| `nest.entity_group.transition.total` | `nest_entity_group_transition_total` | Counter | `result,state` | `nest/group_transition.go:423` | 是 |  |
| `nest.handler.lock_hold` | `nest_handler_lock_hold_{count,sum_nanos,max_nanos,last_nanos}` | Duration | `handler` | `nest/trace.go:421` | 是 |  |
| `nest.handler.lock_hold.slow.total` | `nest_handler_lock_hold_slow_total` | Counter | `handler` | `nest/trace.go:423` | 是 |  |
| `nest.pipelined.async_total` | `nest_pipelined_async_total` | Counter | `result` | `nest/pipelined_completion.go:239、:254、:262、:274` | 是 |  |
| `nest.pipelined.durable_wait` | `nest_pipelined_durable_wait_{count,sum_nanos,max_nanos,last_nanos}` | Duration | `handler` | `nest/execution.go:385、:230` | 是 |  |
| `nest.remote.deferred_after_commit_error_total` | `nest_remote_deferred_after_commit_error_total` | Counter | `handler` | `nest/msg.go:158` | 否 |  |
| `nest.remote.post_commit_without_outcome_total` | `nest_remote_post_commit_without_outcome_total` | Counter | `handler` | `nest/msg.go:141、:166` | 否 |  |
| `nest.stage.duration` | `nest_stage_duration_{count,sum_nanos,max_nanos,last_nanos}` | Duration | `handler,stage` | `nest/trace.go:164` | 是 | `NestOptionWithStageMetrics(true)` 才写 |
| `nest.trace.cost` | `nest_trace_cost_{count,sum_nanos,max_nanos,last_nanos}` | Duration | `handler,type,event,result` | `nest/trace.go:369` | 否 |  |
| `nest.trace.events.total` | `nest_trace_events_total` | Counter | `handler,type,event,result` | `nest/trace.go:367` | 否 |  |

#### nestwal

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `nestwal.append.total` | `nestwal_append_total` | Counter | — | `nestwal/wal.go:1000` | 是 |  |
| `nestwal.batch.total` | `nestwal_batch_total` | Counter | — | `nestwal/wal.go:999` | 是 |  |
| `nestwal.bytes.total` | `nestwal_bytes_total` | Counter | — | `nestwal/wal.go:1001` | 是 |  |
| `nestwal.disk.bytes` | `nestwal_disk_bytes` | Gauge | — | `nestwal/wal.go:1002` | 是 |  |
| `nestwal.fsync.duration` | `nestwal_fsync_duration_{count,sum_nanos,max_nanos,last_nanos}` | Duration | — | `nestwal/wal.go:975` | 是 |  |
| `nestwal.pending.tickets` | `nestwal_pending_tickets` | Gauge | — | `nestwal/wal.go:471` | 是 |  |
| `nestwal.recovery.tail_truncated.bytes` | `nestwal_recovery_tail_truncated_bytes_total` | Counter | `reason` | `nestwal/wal.go:1126` | 否 |  |
| `nestwal.recovery.tail_truncated.total` | `nestwal_recovery_tail_truncated_total` | Counter | `reason` | `nestwal/wal.go:1125` | 否 |  |
| `nestwal.reject.total` | `nestwal_reject_total` | Counter | `reason` | `nestwal/wal.go:426、:912` | 是 |  |

#### remoteentity

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `remote_entity.deferred_outcome_error_total` | `remote_entity_deferred_outcome_error_total` | Counter | — | `remoteentity/transaction_manager.go:363` | 否 |  |
| `remote_entity.deferred_outcome_not_run_total` | `remote_entity_deferred_outcome_not_run_total` | Counter | `outcome` | `remoteentity/transaction_manager.go:357` | 是 |  |
| `remote_entity.finalize_retry_total` | `remote_entity_finalize_retry_total` | Counter | — | `remoteentity/transaction_manager.go:370` | 是 |  |
| `remote_entity.finalize_status_read_total` | `remote_entity_finalize_status_read_total` | Counter | — | `remoteentity/transaction_manager.go:270` | 否 |  |
| `remote_entity.lock_takeover_total` | `remote_entity_lock_takeover_total` | Counter | — | `remoteentity/versioned_lock.go:598` | 否 |  |
| `remote_entity.quarantine_error_total` | `remote_entity_quarantine_error_total` | Counter | — | `remoteentity/transaction_manager.go:238、:310` | 是 |  |
| `remote_entity.rejected_unload_error_total` | `remote_entity_rejected_unload_error_total` | Counter | — | `remoteentity/transaction_manager.go:334` | 否 |  |
| `remote_entity.rejected_unload_total` | `remote_entity_rejected_unload_total` | Counter | — | `remoteentity/local_runtime.go:89` | 否 |  |
| `remote_entity.rejected_unload_unsupported_total` | `remote_entity_rejected_unload_unsupported_total` | Counter | — | `remoteentity/transaction_manager.go:330` | 否 |  |
| `remote_entity.release_failure_total` | `remote_entity_release_failure_total` | Counter | — | `remoteentity/manager.go:69` | 是 |  |
| `remote_entity.remote.apply_latency` | `remote_entity_remote_apply_latency_{count,sum_nanos,max_nanos,last_nanos}` | Duration | `result` | `remoteentity/transaction_manager.go:610` | 是 |  |
| `remote_entity.remote.apply_total` | `remote_entity_remote_apply_total` | Counter | `result` | `remoteentity/transaction_manager.go:609` | 是 |  |
| `remote_entity.remote.interest_refresh_renewed_total` | `remote_entity_remote_interest_refresh_renewed_total` | Counter | — | `remoteentity/interest_refresh.go:210` | 否 |  |
| `remote_entity.remote.interest_refresh_requests_total` | `remote_entity_remote_interest_refresh_requests_total` | Counter | `result` | `remoteentity/interest_refresh.go:111` | 否 |  |
| `remote_entity.remote.interest_refresh_sent_total` | `remote_entity_remote_interest_refresh_sent_total` | Counter | `result` | `remoteentity/interest_refresh.go:73` | 否 |  |
| `remote_entity.remote.interest_rejected_total` | `remote_entity_remote_interest_rejected_total` | Counter | `reason` | `remoteentity/interest.go:218` | 是 |  |
| `remote_entity.remote.interest_renew_refused_total` | `remote_entity_remote_interest_renew_refused_total` | Counter | `reason` | `remoteentity/snapshot_client.go:330` | 否 |  |
| `remote_entity.remote.prepare_latency` | `remote_entity_remote_prepare_latency_{count,sum_nanos,max_nanos,last_nanos}` | Duration | `result,batch` | `remoteentity/batch.go:62` | 是 |  |
| `remote_entity.remote.prepare_total` | `remote_entity_remote_prepare_total` | Counter | `result,batch` | `remoteentity/batch.go:61` | 是 |  |
| `remote_entity.remote.read_latency` | `remote_entity_remote_read_latency_{count,sum_nanos,max_nanos,last_nanos}` | Duration | `result,consistency` | `remoteentity/snapshot_client.go:178` | 是 |  |
| `remote_entity.remote.read_total` | `remote_entity_remote_read_total` | Counter | `result,consistency` | `remoteentity/snapshot_client.go:177` | 是 |  |
| `remote_entity.remote.write_gate_wait` | `remote_entity_remote_write_gate_wait_{count,sum_nanos,max_nanos,last_nanos}` | Duration | — | `remoteentity/batch.go:126` | 是 |  |
| `remote_entity.snapshot_l2_tombstone_wait_total` | `remote_entity_snapshot_l2_tombstone_wait_total` | Counter | `result` | `remoteentity/snapshot_l2.go:381` | 否 |  |
| `remote_entity.snapshot_push_enabled` | `remote_entity_snapshot_push_enabled` | Gauge | `sid` | `remoteentity/snapshot_client.go:495` | 否 | `sid` 每进程一个 |
| `remote_entity.snapshot_replica_historic_dropped_total` | `remote_entity_snapshot_replica_historic_dropped_total` | Counter | — | `remoteentity/syncer.go:132` | 否 |  |
| `remote_entity.unresolved_reject_error_total` | `remote_entity_unresolved_reject_error_total` | Counter | — | `remoteentity/transaction_manager.go:460` | 否 |  |
| `remote_entity.unresolved_resolved_total` | `remote_entity_unresolved_resolved_total` | Counter | `state` | `remoteentity/transaction_manager.go:469` | 否 |  |
| `remote_entity.write_admission_rejected_total` | `remote_entity_write_admission_rejected_total` | Counter | — | `remoteentity/transaction_manager.go:136` | 是 |  |
| `remote_entity_transaction_final_overwrite_ignored_total` | `remote_entity_transaction_final_overwrite_ignored_total` | Counter | — | `remoteentity/transaction_tracking.go:111` | 否 |  |
| `remote_entity_transaction_tracker_drop_total` | `remote_entity_transaction_tracker_drop_total` | Counter | — | `remoteentity/transaction_tracking.go:103` | 是 |  |
| `remote_entity_write_gate_timeout_total` | `remote_entity_write_gate_timeout_total` | Counter | — | `remoteentity/batch.go:128` | 否 |  |

#### robot/loadtest

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `robot.loadtest.active` | `robot_loadtest_active` | Gauge | `profile` | `robot/loadtest/manager.go:466、:519` | 是 |  |
| `robot.loadtest.run.duration` | `robot_loadtest_run_duration_{count,sum_nanos,max_nanos,last_nanos}` | Duration | `profile,result` | `robot/loadtest/manager.go:521` | 否 |  |
| `robot.loadtest.run.total` | `robot_loadtest_run_total` | Counter | `profile,result` | `robot/loadtest/manager.go:520` | 否 |  |

#### robot/runner

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `robot.runner.online` | `robot_runner_online` | Gauge | `profile,run,scenario` | `robot/runner/runner.go:377、:380` | 是 | 同上 |
| `robot.runner.scenario.cost` | `robot_runner_scenario_cost_{bucket,sum_nanos,count}` | Histogram | `profile,run,scenario,result` | `robot/runner/runner.go:433、:438` | 是 | `run` 序列随运行记录挤出历史时 `DeleteSeries` |
| `robot.runner.scenario.total` | `robot_runner_scenario_total` | Counter | `profile,run,scenario,result` | `robot/runner/runner.go:429、:432、:437` | 是 | 同上 |
| `robot.runner.target` | `robot_runner_target` | Gauge | `profile,run,scenario` | `robot/runner/runner.go:251、:328` | 是 | 同上 |

#### robot/session

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `robot.session.call` | `robot_session_call_{bucket,sum_nanos,count}` | Histogram | `msg,result` | `robot/session/session.go:118` | 是 |  |
| `robot.session.late_response` | `robot_session_late_response_total` | Counter | `msg` | `robot/session/session.go:280` | 是 |  |

#### saga

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `saga.completion.late_after_abandon_total` | `saga_completion_late_after_abandon_total` | Counter | `phase,saga_type` | `saga/engine.go:638、:680` | 否 |  |
| `saga.completion.stale_attempt_total` | `saga_completion_stale_attempt_total` | Counter | `phase,saga_type` | `saga/engine.go:648` | 否 |  |
| `saga.completion.stale_incarnation_total` | `saga_completion_stale_incarnation_total` | Counter | `phase,saga_type` | `saga/engine.go:658` | 否 |  |
| `saga.reopened_total` | `saga_reopened_total` | Counter | `from_status,reason,saga_type` | `saga/engine.go:598` | 否 |  |
| `saga.step.attempt_replayed_total` | `saga_step_attempt_replayed_total` | Counter | — | `saga/command_consumer.go:481、:514` | 否 |  |
| `saga.step.expired_unexecuted_total` | `saga_step_expired_unexecuted_total` | Counter | — | `saga/command_consumer.go:360、:437、:461、:468` | 否 |  |
| `saga.step_inbox.mark_completed_error_total` | `saga_step_inbox_mark_completed_error_total` | Counter | — | `saga/step_operation_inbox.go:185` | 否 |  |
| `saga.step_inbox.superseded_total` | `saga_step_inbox_superseded_total` | Counter | — | `saga/step_operation_inbox.go:316` | 否 |  |

#### servicemetrics

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `service.accepted.total` | `service_accepted_total` | Counter | `op,service` | `servicemetrics/metrics_reporter.go:53` | 是 |  |
| `service.conflict.total` | `service_conflict_total` | Counter | `op,service` | `servicemetrics/metrics_reporter.go:76` | 是 |  |
| `service.depth` | `service_depth` | Gauge | `service,name[,key]` | `servicemetrics/metrics_reporter.go:81、:88` | 是 | `key` 必须是有限枚举，默认 Reporter 不删序列 |
| `service.dropped.total` | `service_dropped_total` | Counter | `op,service` | `servicemetrics/metrics_reporter.go:70` | 是 |  |
| `service.refused.total` | `service_refused_total` | Counter | `op,reason,service` | `servicemetrics/metrics_reporter.go:58` | 是 |  |
| `service.replayed.total` | `service_replayed_total` | Counter | `op,service` | `servicemetrics/metrics_reporter.go:63` | 是 |  |

#### skill

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `skill.spawn.abandoned.total` | `skill_spawn_abandoned_total` | Counter | — | `skill/runtime_spawn_stop.go:140` | 否 |  |
| `skill.spawn.abandoned_pruned.total` | `skill_spawn_abandoned_pruned_total` | Counter | — | `skill/runtime_spawn_stop.go:167` | 否 |  |
| `skill.spawn.stop_retry_exhausted.total` | `skill_spawn_stop_retry_exhausted_total` | Counter | — | `skill/runtime_spawn_stop.go:203` | 否 |  |

#### sync/entitysync

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `entitysync_durability_gate_deferred_total` | `entitysync_durability_gate_deferred_total` | Counter | — | `sync/entitysync/flush.go:209` | 是 | 总览仪表盘写成不存在的 `entitysync_flush_gate_deferred_total` |
| `entitysync_flush_duration` | `entitysync_flush_duration_{bucket,sum_nanos,count}` | Histogram | — | `sync/entitysync/flush.go:91` | 否 |  |
| `entitysync_frames_admitted_total` | `entitysync_frames_admitted_total` | Counter | — | `sync/entitysync/flush.go:346` | 是 |  |
| `entitysync_full_captures_total` | `entitysync_full_captures_total` | Counter | `reason` | `sync/entitysync/flush.go:488` | 否 |  |
| `entitysync_sessions_lost_total` | `entitysync_sessions_lost_total` | Counter | — | `sync/entitysync/subscriptions.go:229` | 是 |  |

#### sync/lockstep

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `catchup.frames.total` | `catchup_frames_total` | Counter | — | `sync/lockstep/room.go:435` | 名字不符 | 同上（F04-5） |
| `desync.total` | `desync_total` | Counter | — | `sync/lockstep/room.go:486` | 名字不符 | 同上；`OBSERVABILITY.md` 告警名永不触发（F04-5） |
| `frame.total` | `frame_total` | Counter | — | `sync/lockstep/room.go:301` | 名字不符 | 缺 `lockstep.` 前缀（F04-5） |
| `input.late.total` | `input_late_total` | Counter | — | `sync/lockstep/room.go:270` | 名字不符 | 同上（F04-5） |
| `input.rejected.total` | `input_rejected_total` | Counter | `reason` | `sync/lockstep/room.go:266、:457、:461` | 名字不符 | 同上（F04-5） |

#### timer

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `timer.unhandled_dropped_total` | `timer_unhandled_dropped_total` | Counter | `kind` | `timer/scheduler.go:305` | 是 | `kind` 来自存储里的类型号 |

#### versionstore

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `versionstore.cas.total` | `versionstore_cas_total` | Counter | `result,store` | `versionstore/versionstore.go:178` | 否 |  |
| `versionstore.conflict.total` | `versionstore_conflict_total` | Counter | `store` | `versionstore/versionstore.go:183` | 否 |  |
| `versionstore.unknown_outcome.total` | `versionstore_unknown_outcome_total` | Counter | `result,store` | `versionstore/write_token.go:63` | 否 |  |

#### 生成代码：game-demo

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `scene_session_reopen_failed_total` | `scene_session_reopen_failed_total` | Counter | `reason` | `demo/internal/service/game/scene.go.tmpl:828` | 否 |  |

#### 生成代码：玩家 TCP（codegen/internal/roost/render_player_tcp.go）

| 指标（源码名） | Prometheus 名 | 类型 | 标签 | 写入位置 | OBS | 备注 |
| --- | --- | --- | --- | --- | --- | --- |
| `player_tcp_auth_failure_total` | `player_tcp_auth_failure_total` | Counter | `reason?（handshake_capacity 有，其余无）` | `codegen/internal/roost/render_player_tcp.go:812、:819、:826` | 否 | 同上 |
| `player_tcp_connection_rejected_total` | `player_tcp_connection_rejected_total` | Counter | `reason?（per_ip 有，max_connections 无）` | `codegen/internal/roost/render_player_tcp.go:765、:775` | 否 | 同名有标签与无标签两种序列混用 |
| `player_tcp_connections` | `player_tcp_connections` | Gauge | — | `codegen/internal/roost/render_player_tcp.go:699、:784、:785` | 否 |  |
| `player_tcp_dispatch_duration` | `player_tcp_dispatch_duration_{count,sum_nanos,max_nanos,last_nanos}` | Duration | — | `codegen/internal/roost/render_player_tcp.go:856` | 否 |  |
| `player_tcp_dispatch_error_total` | `player_tcp_dispatch_error_total` | Counter | — | `codegen/internal/roost/render_player_tcp.go:860` | 否 |  |
| `player_tcp_dispatch_timeout_total` | `player_tcp_dispatch_timeout_total` | Counter | — | `codegen/internal/roost/render_player_tcp.go:858` | 否 |  |
| `player_tcp_frame_error_total` | `player_tcp_frame_error_total` | Counter | — | `codegen/internal/roost/render_player_tcp.go:817、:838、:840` | 否 |  |
| `player_tcp_push_closed_total` | `player_tcp_push_closed_total` | Counter | — | `codegen/internal/roost/render_player_tcp.go:944` | 否 |  |
| `player_tcp_push_error_total` | `player_tcp_push_error_total` | Counter | — | `codegen/internal/roost/render_player_tcp.go:989、:1002` | 否 |  |
| `player_tcp_push_no_session_total` | `player_tcp_push_no_session_total` | Counter | — | `codegen/internal/roost/render_player_tcp.go:971、:999` | 否 |  |
| `player_tcp_push_total` | `player_tcp_push_total` | Counter | — | `codegen/internal/roost/render_player_tcp.go:987、:1001` | 否 |  |
| `player_tcp_session_closed_callback_panics_total` | `player_tcp_session_closed_callback_panics_total` | Counter | — | `codegen/internal/roost/render_player_tcp.go:547` | 否 |  |
| `player_tcp_session_closed_dropped_total` | `player_tcp_session_closed_dropped_total` | Counter | — | `codegen/internal/roost/render_player_tcp.go:564` | 否 |  |
| `player_tcp_write_error_total` | `player_tcp_write_error_total` | Counter | — | `codegen/internal/roost/render_player_tcp.go:1113` | 否 |  |

### 6.4 仪表盘

| 仪表盘 | 来源 | 内容 | 已知不一致 |
| --- | --- | --- | --- |
| Roost Overview（uid `roost-overview`） | 手写 `observability/grafana-roost-overview.json` | 四行：Nest 调度与事务、Durability 管线、缓存 / 总线 / 跨服实体、进程运行时（statslog gauge） | `:261` 查 `entitysync_flush_gate_deferred_total`，实际名是 `entitysync_durability_gate_deferred_total`，该线永远为空；“进程运行时”一行依赖 `stats_log.enabled` |
| Roost game-demo（uid `roost-demo`） | 生成：`demo/deploy/dev/observability/grafana/dashboards/roost-demo.json.tmpl`，由 `codegen/internal/roost/demo.go:625-629` 写进工程 | 七行：玩家 TCP、Nest、WAL、事件链与配置、场景复制会话、bus RPC、机器人压测 | `:663` 按 `subject` 聚合 `nats_jetstream_terminal_total`，该指标只有 `reason` 标签；配套 README（`README.md.tmpl:21`、`:24`）同样写 `{subject}`，并把 `bus_rpc_*` 的开关写成 `nats.reliable.enabled`（实际是 `nats.rpc.transport: jetstream`，`kit/nats/nats_mod.go:72-75`） |

开发机起 Prometheus + Grafana：`docker compose -f deploy/dev/observability/docker-compose.yaml up -d`，抓取目标是各服务 ops 端口（game 9100、托管服务 9101 起按名字排序）和压测 `-metrics-addr 127.0.0.1:9300`（`demo/deploy/dev/observability/prometheus.yml.tmpl`）。

生成仪表盘的守卫只查 JSON 合法、每个面板有查询、占位符替换完、查询数 ≥20，以及两个指定指标在面板里（`codegen/internal/roost/demo_test.go:503-573`）；不查其他查询里的名字和标签是否存在。总览仪表盘与 `OBSERVABILITY.md` 没有任何守卫。

### 6.5 告警基线（按源码校正）

| 条件 | 级别 | 说明 |
| --- | --- | --- |
| `nest_pipelined_async_total{result="indeterminate"} > 0` | 立即 | fence 事故 |
| `obs_series_dropped_total > 0` | 立即 | 某指标标签基数打满，新组合静默失效；调 `metrics.max_series_per_metric` 前先查标签来源 |
| `nestwal_reject_total` 增长、`nestwal_pending_tickets` 持续爬升 | 高 | 容量 / 磁盘 |
| `desync_total > 0`（**不是** `lockstep_desync_total`，F04-5 修复前） | 立即 | 确定性被破坏 |
| `dataengine_fence_skipped_total` 速率阶跃到与事务量同阶 | 立即 | claim schema 漂移 |
| `remote_entity_release_failure_total`、`remote_entity_quarantine_error_total` 非零 | 高 | 所有权收尾异常 |
| `log_write_errors_total`、`log_rotate_failures_total`、`stats_log_write_failures_total` 非零 | 中 | 日志 / 统计在丢 |
| `app_business_time_advance_failed_total` 持续增长 | 中 | 非生产环境的高水位推进失败（T-284） |
| `nats_jetstream_terminal_total` 增长 | 高 | 事件消费最终放弃（`reason` 区分 permanent / max_deliver） |
| `timer_unhandled_dropped_total` 非零 | 中 | 存量定时器类型无 handler 被丢弃 |
| `/readyz` 体 `degraded: true` 持续数分钟 | 关注 | 按 `degraded_dependencies[].name` 查来源 |

### 6.6 常见故障 → TROUBLESHOOTING

| 现象 | 行 |
| --- | --- |
| `/readyz` 503，某项 `message: check timed out` | [T-280](../../TROUBLESHOOTING.md) |
| `/readyz` 200 但 `degraded: true` | [T-267](../../TROUBLESHOOTING.md)（注意 entitysync 实际不在其中） |
| 升级后 `/admin/*` 401，token 没变 | [T-279](../../TROUBLESHOOTING.md)（必须 `Bearer `） |
| `/admin/execute` 504 | [T-263](../../TROUBLESHOOTING.md) |
| `/admin/execute` 200 但体为空或 `internal server error` | [T-242](../../TROUBLESHOOTING.md) |
| 启动失败 `ops: listen on ops.addr … address already in use` | [T-262](../../TROUBLESHOOTING.md) |
| 日志缺新分片 / 一个 sink 出错 | [T-266](../../TROUBLESHOOTING.md) |
| stats_log 文件不落盘 | [T-177](../../TROUBLESHOOTING.md) |
| 死信写失败 `failurelog: append script: EOF` | [T-250](../../TROUBLESHOOTING.md) |
| `app_business_time_advance_failed_total` 增长 | [T-284](../../TROUBLESHOOTING.md) |
| `timer_unhandled_dropped_total` 非零 | [T-268](../../TROUBLESHOOTING.md) |

→ [实现文档](../impl/11-observability.md)对应：§6 失败处理、§7 协议格式、§11 源码疑点。

## 7. 保证与不保证

**保证**

- 每个指标名的序列数不超过上限；打满后旧序列照常更新，新组合丢弃并计数、按名字告警一次（`metrics/metrics.go:638-662`）。
- `DeleteSeries` 删除后同名同标签再写是从零开始的新序列；空 match 不删任何东西（`metrics/metrics.go:511-541`）。
- 直方图分位数不超出观测到的最小 / 最大值（RR-20261006-27）。
- 标签值按文本格式转义，非法 UTF-8 换成 U+FFFD（`metrics/prometheus.go:111-119`）。
- `/readyz` 在 1.5s 加少量开销内答完；卡住的 checker 不累积 goroutine（每个至多一个在途）。
- `/admin/*` token 常数时间比较；没配 token 时什么都不放行（`kit/ops/ops_mod.go:425-433`）。
- `ops.Start` 返回 nil 即已监听；端口被占启动失败。
- 日志一个 sink 失败不影响另一个；`log.Close` 之后的日志改写到控制台（只配文件时改写 stderr）。
- failurelog 脚本结果未知时不重放、原样报错。

**不保证**

- 指标是进程内的，进程重启清零；Counter 没有持久化。
- `/metrics` 不带 `# TYPE` / `# HELP`；Duration 没有分位数。
- 默认注册表是进程全局的：同一进程里建多个 App Registry 时，包级 `metrics.*` 写进最后建的那个（`app/registry.go:36`）。
- `/readyz` 只反映登记过的 checker；有 `CheckHealth` 但没登记的对象（entitysync）不可见。
- admin 命令没有框架级审计、没有权限分级（`CommandMeta.Risk / ApprovalRequired` 没有执行方，[impl §11 N13](../impl/11-observability.md#11-源码疑点与文档不一致)）。
- 死信列表超过上限时最老条目被静默裁掉（生产路径不计数）。
- `OBSERVABILITY.md` 与仪表盘里的名字没有守卫，以本篇总表和源码为准。
- 需要外部验证：Prometheus 对无 `# TYPE` 文本的解析（按 untyped 处理）、Grafana 版本兼容（生成 compose 固定 `grafana/grafana:11.2.0`、`prom/prometheus:v2.54.1`）。

→ [实现文档](../impl/11-observability.md)对应：§4 不变量清单、§11 源码疑点。

## 8. 相关文档

- 实现：[impl/11-observability.md](../impl/11-observability.md)
- 规范与清单（手写，部分过时）：[OBSERVABILITY.md](../../../OBSERVABILITY.md)；总览仪表盘 [grafana-roost-overview.json](../../../observability/grafana-roost-overview.json)
- 方案：[D1 Degraded 算就绪](../../feature/D1-READYZ-DEGRADED-IS-READY-2026-10-06.md)、[R12 kit 批（DeleteSeries、checker 期限、Bearer、CAS 计数、推进失败计数）](../../feature/DECISIONS-R12-KIT-2026-10-06.md)、[C6 默认服务指标](../../feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md)、[Ops admin 期限](../../feature/OPS-ADMIN-TIMEOUT-2026-10-06.md)
- 发版记录：[v1.23.0 app / ops 分册](../../release/v1.23.0/impl-app-own-clk-ops-tool.md)（APP-10、APP-11、OPS-1～OPS-6）
- 快速参考：[USER_GUIDE](../../USER_GUIDE.md)、[TROUBLESHOOTING](../../TROUBLESHOOTING.md)
- 其他分区：[01 app](01-app-lifecycle.md)、[02 nest](02-nest-entity.md)、[03 dataengine](03-dataengine.md)、[04 sync](04-sync.md)、[05 remote / mirror](05-remote-mirror.md)、[06 saga](06-saga.md)、[07 配置](07-config.md)、[09 kit 服务](09-kit-services.md)、[10 时间](10-time.md)；[12 代码生成](12-codegen.md)

[↑ 速览](#速览) · [实现文档](../impl/11-observability.md)
