# v1.31.0 发版前整体压测样本（2026-10-22 晚）

清单与逐项状态：[V1310-PRERELEASE-20261022](../../V1310-PRERELEASE-20261022.md)。被测：发版候选 main `3d727c2f`（worktree `roost-core-wt-perf1022`）。二进制、pprof、server/client 原始 JSON 与 game 日志在会话 scratchpad 的 `perf1022/`，未入库。

| 目录 / 文件 | 内容 |
| --- | --- |
| `aoi-3pt/` | 第 5 项：policy 三点（P0 `49897751` / P1 `08432d4e` / P2 发版候选）交替 10 轮原始输出、`benchstat-3pt.txt`、`policy-runs.log`（含 loadavg）；sync-aoi 单视野三点与共享视野 `sync-aoi-*` |
| `hb-load/` | 第 6 项（含第 7 项 sync-aoi 200ms 一组）：sync-aoi 全局循环 / 心跳驱动与多视野 `sync-aoi-*`；nest-game ticker / heartbeat 15 分钟 JSON 与 env；`driver.txt` / `driver-benchstat.txt`；`openloop.jsonl`；`host-load.txt`（每 15 秒 loadavg 与 CPU 前 6） |
| `demo-loadtest/` | 第 3 项：100 / 200 人机器人报告与 game 日志 WARN / ERROR 分类 |
| `run-list.sh` / `agg-runs.py` / `run-policy.sh` / `chain-b.sh` | 门控运行器（每次前等 10～30 秒没有外来进程，运行中采到就作废重跑，门控脚本是 `nest-batch-heartbeat-20261021/cdefault-20261022/foreign-ext.sh`）与汇总脚本；`<scratch>` / `<worktree>` 是占位 |

第 1 项样本在 `nest-batch-heartbeat-20261021/cdefault-20261022/night-*`，第 2 项在 `interest-applysync-budget-20261022/`。
