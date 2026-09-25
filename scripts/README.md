# scripts/

Remote 容量/长稳：`scripts/perf/remote.sh`（默认 30m，`ROOST_REMOTE_DURATION=24h` 可复跑 24 小时）；故障矩阵：`scripts/test-remote-matrix.sh`。两者独占隔离环境，原始日志和数据保存于 `artifacts/perf/remote/<label>/`；[负载、判据与结果](../docs/feature/REMOTE-ACCEPTANCE-2026-09-24.md)。

Remote 正式业务验收：加载隔离环境的 `ROOST_DATAENGINE_IT_MONGO_URI`、`ROOST_DATAENGINE_IT_REDIS_ADDR`、`ROOST_DATAENGINE_IT_NATS_URL` 后运行 `./scripts/test-remote-generated.sh`。正式 DAO/Entity 生成器构造两实体各两 DAO，经 Nest 与三种持久化策略提交，校验 Mongo、业务拒绝/panic 回滚和真实 NATS 快照接收；不依赖 demo。脚本只清理自身临时工程，测试仅清理专属数据库和 Redis key。[完整验收及独立多进程故障命令](../docs/feature/REFACTOR-2026-09-24-remote.md#第三轮进程故障与正式业务链路)。

仓库级的工具，**每个只有一份**。合仓之前 core / kit / codegen 各带一套，
其中 `gapmap.sh` 三份逐字节相同、`pretag.sh` 三份各不相同却只有一个还能用
（另外两个仓已归档，发不了版）。整体检查时收敛成这里的一份（2026-09-21）。

| 路径 | 做什么 | 谁在用 |
| --- | --- | --- |
| `pretag.sh <version>` | 发版前的门禁：tag 与 module 主版本一致、tag 未存在、无 replace、工作树干净、`GOWORK=off` 下 build/vet/tidy/test 全过、发布清单声明的版本等于要打的 tag（U-0270） | 人工在打 tag 之前跑；`.github/workflows/release.yml` 在 tag 之后再校验一次清单 |
| `gapmap.sh [--max N] [pkg...]` | 覆盖率采样：按包回退断言看测试是否真的会红，产出 `gapmap-report.md` | `.github/workflows/nightly-gapmap.yml` |
| `gapmap/` | 上面那个脚本的 Python 部件（`classscan.py`、`revertsample.py`） | `gapmap.sh` |
| `perf/` | `nest-msg.sh` 跑 [Nest 消息吞吐](../docs/feature/NEST-MSG-THROUGHPUT-2026-09-24.md)；`nest.sh` 跑 [Nest 调度与诊断微基准](../docs/feature/NEST-COMPLETION-2026-09-24.md)；`sync.sh` 跑微基准；`sync-business.sh` 跑[隔离 demo](../docs/feature/SYNC-BUSINESS-LOAD-2026-09-23.md)；`sync-aoi.sh` 跑[1000/10000 AOI 双进程负载](../docs/feature/SYNC-AOI-1000-10000-2026-09-23.md) | 人工 |
| `consolidation/` | 合仓期间的搬迁辅助（`merge_kit_pkg.py`、`split_driver.py`） | 已完成的迁移，保留备查 |

另外两处 `scripts/` 是**层内**的，不重复：

- `kit/scripts/integration/` — kit 服务的集成环境（brew 起的 Redis / Mongo 副本集 / NATS，见 `dataengine-env.sh`）。
- `codegen/scripts/` — 生成器自己的运行时校验（`*-runtime.sh` 把生成物编译进真运行时跑一遍）、
  `source-head-check.sh`（本地版 framework-compat）、`install-windows.ps1`。

Sync AOI 工具新增全量预算和集中恢复参数，见[六项实施与复跑命令](../docs/feature/REFACTOR-2026-09-24-sync-six-items.md)。

正式双模式：`./scripts/test-sync-modes-generated.sh` 验证 DAO/Entity 生成工程与 Nest→Sync；`./scripts/perf/sync-aoi.sh -mode=periodic` 或 `-mode=on_change` 默认走正式 Nest 内存提交和 Interest 事实队列，`-nest=false` 仅用于兼容原来的独立 Sync 基线。业务输入固定每 50ms 一批，详见[双模式实施记录](../docs/feature/IMPLEMENTATION-2026-09-24-sync-modes.md)。

DataEngine 正式持久化链路：配置隔离 `ROOST_DATAENGINE_IT_MONGO_URI` 后运行 `./scripts/test-dataengine-generated.sh`。脚本用正式 DAO/Entity 生成器构造最小测试工程，三次独立进程使用同一测试库和 WAL，覆盖三种持久化策略、共享实体的四文档交易、错误/panic 回滚、ack 丢失重放与末文档冲突原子回滚。只清理本次生成的 `roost_generated_it_*` 库；本入口不运行 Remote 场景。[恢复验收](../docs/feature/DATAENGINE-RECOVERY-2026-09-24.md)。

DataEngine 真实 Mongo 压测：`bash scripts/perf/dataengine.sh` 复用正式生成工程，默认非 race、GOMAXPROCS=4、32 并发、128 实体、每样本 1000 笔，四种负载 × 三种策略 × 三轮。结果存入 `artifacts/perf/dataengine/<label>/`；每轮逐文档验证并排空 WAL。可通过 `ROOST_PERF_SHAPES="single dual pair hot"`、`ROOST_PERF_POLICIES="async strict pipelined"`、`ROOST_PERF_REQUESTS`、`ROOST_PERF_WRITERS`、`ROOST_PERF_ENTITIES`、`ROOST_PERF_COUNT`、`ROOST_PERF_CPU`、`ROOST_PERF_LABEL` 调整。`ROOST_PERF_RATE=20` 按每秒 20 笔目标节奏发送，未设置时为闭环突发压力；超载时同时观察排空时间和 WAL 积压，不能只看 Request 返回吞吐。


DataEngine 多 DAO 批量优化后的验收与性能对比见
[实施记录](../docs/feature/DATAENGINE-BATCH-2026-09-24.md)。正式生成链路现覆盖三种策略下
满额准入拒绝、四 DAO 回滚、排空后再准入。大积压专项增加
`ROOST_DATAENGINE_IT_BACKLOG_MULTI=1`（同时设置 `ROOST_DATAENGINE_IT_BACKLOG=1`），
把 100k WAL 恢复扩展为 10k Entity × 双 DAO；默认仍保留原单 DAO 用例。

Remote 的持久权限、最新故障矩阵和容量复跑见 [2026-09-25 实施报告](../docs/feature/REMOTE-AUTHORITY-2026-09-25.md)。旧部署必须先停写、排空并迁移；脚本记录当前 Remote 源码摘要，不能拿旧协议长稳结果替代新协议验收。


Remote 诊断可设置 `ROOST_REMOTE_CPU_PROFILE=/绝对路径/cpu.pprof` 和 `ROOST_REMOTE_TRACE=/绝对路径/trace.out`；`scripts/test-remote-generated.sh` 把它们传给 Go test。跟踪会覆盖初始化、预热和计时阶段，诊断结果应与不带 profile 的正式负载分开。长稳仍使用 `scripts/perf/remote.sh` 的 `ROOST_REMOTE_DURATION=30m` / `24h`；当前版最新实跑范围见 `docs/feature/REMOTE-UNIFIED-2026-09-25.md`。

Remote 阶段定位可设置 `ROOST_REMOTE_STAGE_METRICS=1`，结果包含 Nest 阶段 metrics 和最多 32 个请求错误样本。`ROOST_REMOTE_PROJECTION_WORKERS=1` 提供串行对照，默认使用正式 Projector 的 8 路独立 Entity 投影；`ROOST_REMOTE_RATE=60` 可验证 50 TPS 以上目标。脚本源码摘要包含 DataEngine。最新容量/长稳结果见 [吞吐与超时报告](../docs/feature/REFACTOR-2026-09-25-remote-throughput.md)。

Remote 容量阶梯入口：`ROOST_REMOTE_CAPACITY_LABEL=<唯一标签> ROOST_REMOTE_RATES='80 120 160' ROOST_REMOTE_DURATION=120s bash scripts/perf/remote-capacity.sh`。先加载隔离环境 env.sh，沿用 1000 会话、10000 Entity、2 Entity × 2 DAO、strict。每档单独初始化、预热并全量核验，首次失败停止，保留退出码；之后用更细档位缩小区间，最高通过档应延长验证，不能把短测单点当通用上限。

`ROOST_REMOTE_WORKERS` 控制本轮 Nest 逻辑并发（负载默认 64）；`ROOST_REMOTE_IO_WORKERS` 单独控制慢池并发（默认跟随前者）；`ROOST_REMOTE_PROJECTION_WORKERS` 控制 DataEngine 独立 Remote 投影并发（默认 8）。容量对比必须同时披露这三项、CPU 和持久化策略。

### Nest 快慢双池验收

启动 option：`NestOptionWithWorkerPools(fast, slow)`；每池 QueueCap 为整池等待容量。
`ROOST_REMOTE_WORKERS` 配快池，`ROOST_REMOTE_IO_WORKERS` 配共享慢池，新增 `ROOST_REMOTE_FAST_QUEUE`（默认 4096）与 `ROOST_REMOTE_SLOW_QUEUE`（默认 64）。Remote 模式仍走正式业务/存储链路。

调度混合基准：`ROOST_PERF_BENCH=BenchmarkMixedFastSlow bash scripts/perf/nest.sh`。1%/5% 是慢 I/O 请求比例，模拟等待 500μs，不能当作 Entity 变化频率或真实数据库 TPS。

慢池并发/队列对比：加载隔离环境后运行 `ROOST_REMOTE_WORKER_LABEL=<唯一标签> bash scripts/perf/remote-workers.sh`。默认固定快池8、投影8、每档120秒，比较慢池64/128/256/1024与等待位16/64；`ROOST_REMOTE_WORKER_CASES='64:64:120 1024:16:120'` 可指定慢worker:等待位:输入TPS。过载结果保留并继续下一档，环境/预热无结果则停止；以零错误、零丢弃和全量校验通过判定通过，不能只看成功TPS或队列长度。

Remote 资源预算验证新增 `ROOST_REMOTE_WRITE_LIMIT`（正整数，未设使用正式默认 128）、`ROOST_REMOTE_WAL_LIMIT`（正整数，未设保持 WAL 不设记录数上限）。它们独立于快/慢 worker 数；历史 4096 预算对照可显式设 `ROOST_REMOTE_WRITE_LIMIT=4096`，前提是 `AsyncFinalizeCapacity` 足够。最新[预算与集中恢复验收](../docs/feature/REFACTOR-2026-09-25-resource-budgets-and-session-recovery.md)披露拒绝和成功 TPS。
