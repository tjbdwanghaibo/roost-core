# D20 选项 C：Nest 批量心跳作业样本（2026-10-21）

报告：[NEST-BATCH-HEARTBEAT-20261021](../../NEST-BATCH-HEARTBEAT-20261021.md)。夹具：[gameplay/heartbeat/internal/bench](../../../../gameplay/heartbeat/internal/bench/)（`HB_MODE=c`）。

| 文件 | 内容 |
| --- | --- |
| `mutations.txt` | 22 条变异核对的失败文本（`scripts/mutate.py`，变异定义在 `scripts/muts-all.json` / `muts-more.json`；`mutate.py` 里的仓库路径按本机写死，复跑时改成自己的检出） |
| `paused-round1.jsonl` / `paused-round1-runs.log` / `paused-round1-summary.md` | 压测暂停前采到的第 1 轮（每组 1 次，不作结论）与运行记录、`scripts/agg.py` 汇总 |
| `scripts/` | 门控 `foreign.sh`、正式对比 `run-formal.sh`、普通 Nest 微基准 `run-micro.sh`、profile `run-prof.sh`、串联 `chain.sh`、汇总 `agg.py`；复跑步骤见报告 §4 |

原始输出、二进制留在 scratchpad 的 `hb/run/`，未入库。
