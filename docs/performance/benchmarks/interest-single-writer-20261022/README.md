# Interest 单写者重构前后对比样本（2026-10-22）

报告：[INTEREST-SINGLE-WRITER-20261022](../../INTEREST-SINGLE-WRITER-20261022.md)。改前 `494134ae`，改后 `11988df8`；两边的 sync-aoi 都带 `6cffbc03` 的夹具修正，ApplySync 16 视野两边都用只把 `SyncApplyMaxExtraViews` 改成 16 的诊断构建（未入库）。

| 文件 | 内容 |
| --- | --- |
| `aoi-o-runs.jsonl` / `aoi-o-summary.md` / `aoi-o-runs.log` | sync-aoi 原场景，每次运行一行（字段同 `d19-multiview-20261021/applymode-runs.jsonl`，另加 `server_cpu_s` / `client_cpu_s` = `ps` 采样的累计 CPU 秒、`flush_failures_before_stop`、`manager_last_error`）、逐轮汇总、运行记录（含 loadavg）。运行名 `<场景>-v<视野>-<sync/async>-<nest/direct>-<base/after>-r<轮>` |
| `aoi-n-*` | 小块场景，同上 |
| `aoi-o-harness-apply-*` | 第一遍对照（作废）：sync-aoi 的 Nest 模式每 tick 仍调 `Apply`，改后的数被压测线程做的 AOI 扭曲；保留为夹具问题的证据 |
| `policy-base.txt` / `policy-after.txt` / `policy-benchstat.txt` / `policy-runs.log` | policy 包基准交替 10 遍与 benchstat |
| `extra_bench_test.go.txt` | 方案 §6 第 1 项要求的两条新基准（压测专用，改名 `.txt` 免得进构建；复跑时拷进 `framework/sync/entitysync/policy/` 两边各编一次测试二进制） |
| `run-aoi.sh` / `run-policy.sh` / `agg-aoi.py` | 门控运行与汇总脚本（`run-aoi.sh <目录> <输出> <轮数> o|n`，二进制放 `<目录>/bin/aoi-{base,after}[-d16]`；门控脚本是 `nest-batch-heartbeat-20261021/scripts/foreign.sh`） |

原始 server.json / client.json、日志与二进制在会话 scratchpad 的 `perf/interest/`，未入库。
