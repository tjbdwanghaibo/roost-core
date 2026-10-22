# Interest ApplySync 预算计入结算的对照样本（2026-10-22 晚）

报告：[INTEREST-SINGLE-WRITER §7.1](../../INTEREST-SINGLE-WRITER-20261022.md#71-结果2026-10-22-晚v1310-发版前压测第-2-项)。改前 `bb7a734f`、只含预算 `be49684b`、改后发版候选 `3d727c2f`；16 视野三边用 `SyncApplyMaxExtraViews` 16 的诊断构建（未入库）。

| 文件 | 内容 |
| --- | --- |
| `summary.md` / `runs.jsonl` / `runs.log` | 16 / 0 / 8 视野三点交替 2 轮：每次一行（`agg-runs.py` 口径，含 loadavg、`budget_stop`、运行末 `log_backlog`、政策 / 结算直方图 P95）与运行记录 |
| `v8-single-variable/` | 8 视野 `be49684b` / `12669852` / 发版候选三点轮转 3 轮的单变量复核 |
| `item2.list` / `run-list.sh` / `agg-runs.py` | 运行列表（`<scratch>` 是占位）、门控运行器与汇总脚本 |
