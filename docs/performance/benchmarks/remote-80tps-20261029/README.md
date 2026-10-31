# Remote 负载回归：80TPS × 15 分钟（2026-10-29，v1.31.0 发布后）

main `7838ffe5`（Remote 链路与 v1.31.0 相同），共享隔离环境，`scripts/perf/remote.sh`，参数与 [NIGHT-20261022 §8](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/NIGHT-20261022.md#8-第-9-项remote-80tps--15-分钟) 相同：`ROOST_REMOTE_CPU=4 RATE=80 POLICY=strict SESSIONS=1000 ENTITIES=10000 WORKERS=8 FAST_QUEUE=4096 IO_WORKERS=1024 SLOW_QUEUE=16 WRITE_LIMIT=256 WAL_LIMIT=512 PROJECTION_WORKERS=8 STAGE_METRICS=1 DURATION=15m TIMEOUT=1h`，`GOFLAGS=-p=1`，干净 worktree，单样本。整轮 20:52～21:08（约 16 分钟）。

目的：补上故障矩阵没压到的部分——v1.31.0 的投影器按实体并行（D5）、删除 lease fence（D6）、Nest 按 handler 异步完成与同实体提交序（N1 / N2）在 1 万实体负载下的表现。

| 指标 | 本次（`7838ffe5`） | 10-22 发版候选 | 10-21 HEAD `af904c76` |
| --- | --- | --- | --- |
| 成功 / 实际 TPS | 72000 / 79.993 | 72000 / 79.976 | 72000 / 79.987 |
| Errors / Dropped / 未应用 / 不确定 | 0 / 0 / 0 / 0 | 0 / 0 / 0 / 0 | 0 |
| 全量核验 | 通过（`result.json.verified`） | 通过 | 通过 |
| 混合 p50 / p95 / p99 / max ms | 118 / 169 / 196 / 462 | 164 / 237 / 287 / 1293 | 160 / 231 / 263 / 1280 |
| 服务端 p50 / p95 / p99 / max ms | 117 / 168 / 195 / 461 | 163 / 236 / 286 / 1292 | 159 / 230 / 262 / 1277 |
| 压测端 AdmitLag / ClientQueue p99 ms | 2 / 0 | 2 / 0 | 2 / 0 |

结论：正确性通过，没有回退；延迟各分位都低于 10-22（p99 196 vs 287ms）。单样本、跨日期，宿主与共享 Mongo 状态不同，不把差值归因到某个提交，只说明 v1.31.0 的 Remote 相关改动在这一负载下没有带来可见的退化。

样本：`result.json`（含 68 项指标终值）、`env-head.txt`。完整日志与 `run.log.gz` 在本机 `artifacts/perf/remote/remote-80tps-20261029/`，不随仓。
