# D20 选项 C：Nest 批量心跳作业样本（2026-10-21）

报告：[NEST-BATCH-HEARTBEAT-20261021](../../NEST-BATCH-HEARTBEAT-20261021.md)。夹具：[gameplay/heartbeat/internal/bench](../../../../gameplay/heartbeat/internal/bench/)（`HB_MODE=c`）。

| 文件 | 内容 |
| --- | --- |
| `mutations.txt` | 23 条变异核对在最终代码上一次重跑的失败文本（`scripts/mutate.py` + `scripts/muts-final.json`；分批时的 `muts-all.json` / `muts-more.json` / `muts-switch.json` / `muts-freeze.json` 保留。`mutate.py` 里的仓库路径按本机写死，复跑时改成自己的检出） |
| `paused-round1.jsonl` / `paused-round1-runs.log` / `paused-round1-summary.md` | 压测暂停前采到的第 1 轮（每组 1 次，不作结论）与运行记录、`scripts/agg.py` 汇总 |
| `scripts/` | 门控 `foreign.sh`、正式对比 `run-formal.sh`、普通 Nest 微基准 `run-micro.sh`、profile `run-prof.sh`、串联 `chain.sh`、汇总 `agg.py`；复跑步骤见报告 §4 |

| `rerun-20261022/` | 2026-10-22 补测（main `b86c4fa4`）：正式对比 `formal-raw.jsonl`（全部行）/ `formal.jsonl`（剔除与本 agent 编译重叠的 C 100% r1、加补跑 r4）/ `formal-summary.md` / `formal-runs.log`；同时段补充轮 `supp*`（`run-supp.sh`）；批大小 / 时间片对照 `sweep*`（`run-sweep.sh`）；普通 Nest 微基准 `micro-base.txt` / `micro-final.txt` / `micro-benchstat.txt`；profile 文本 `prof/`（`pprof -top` 前 60 行）；补跑脚本 `redo-c100.sh`。时间片 100 / 400µs、组大小 4 / 16 的二进制由只改 `tryBatchSlice` / `tryBatchGroup` 常量的源码编出，未入库 |

原始输出、二进制留在 scratchpad 的 `hb/run/`（2026-10-22 补测在 `perf/hbrun/`），未入库。
