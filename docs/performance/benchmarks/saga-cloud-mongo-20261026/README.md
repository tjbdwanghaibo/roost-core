# Saga 云 Mongo / 本机 Mongo 并发扫描样本（2026-10-26）

报告与口径见 [SAGA-CLOUD-MONGO-20261026](../../SAGA-CLOUD-MONGO-20261026.md)。

- `local/` 与 `cloud/`：每档的结果 JSON（`<场景>-c<并发>.json`）、Mongo 步骤的逐跳计数（`.json.hops.json`）与逐笔时间线汇总（`.timeline-summary.json`，过载档有重试时没有）、前后 serverStatus 计数（`.server-{before,after}.json`）、`runs.tsv`（每档前后 loadavg 与耗时）、`env.txt`、`summary.json`（`saga-scan-summary.py` 输出）以及残留核对。
  - `local/local-1`、`cloud/cloud-1`：主扫描，Mongo 步骤 worker 2。
  - `local/local-w16`、`cloud/cloud-w16`：Mongo 步骤 worker 16。
  - `local/local-repeat`：本机 Mongo 步骤 c32 重跑。
  - `local/local-nohops`：本机原生正向 c32 关闭追踪。
- `ceiling-{local,cloud}.json`：原始往返与单文档事务的闭环并发天花板。
- `claim-explain-{local,cloud}.json`：领取查询的 explain 与实测领取耗时。
- `redis-aof-always-local.txt`：本机私有 Redis（AOF always）单脚本提交的延迟。
- `log-event-classes.txt`：过载档日志事件分类（已脱敏）。

原始日志、库快照与测试二进制只在本地 `artifacts/perf/saga/`。云库样本经过脱敏，不含连接串、主机名和账号。
