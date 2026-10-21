# D20 心跳调度结构与投递方式（2026-10-21）样本

报告：[HB-STRUCTURES-20261021](../../HB-STRUCTURES-20261021.md)。原型：[gameplay/heartbeat/internal/bench](../../../../gameplay/heartbeat/internal/bench/)。

| 文件 | 内容 |
| --- | --- |
| `sched-benchstat.txt` | 对比一：10 遍干净样本合并后的 benchstat（ns/op、B/op、allocs/op、每格 p50 / p99 / max、liveB/reg、ssr map 预分配） |
| `sched-passes.log` | 对比一每遍的起止、loadavg 与干净 / 作废判定（采到外来 go 构建 / 测试进程即作废） |
| `phase-distribution.md` | 每格到期数量分布（`HB_DIST_OUT`） |
| `delivery.jsonl` | 对比二：48 次运行的原始结果（每行一次，含配置） |
| `delivery-summary.md` | 按组汇总（中位数 [最小–最大]），由 `scripts/agg-delivery.py` 生成 |
| `delivery-runs.log` | 对比二每次运行的时刻、参数与 loadavg |
| `pprof-cpu-{x,y,z}-cum.txt`、`pprof-mutex-{x,y,z}.txt` | 100% 移动、10ms、1 个驱动的 CPU（按累计排序）与 mutex profile 摘要；与正式测量分开运行，计数见 `profile-runs.jsonl` |
| `scripts/` | 门控（等机器安静、运行中采样外来进程）、运行与汇总脚本；运行时放在 scratchpad 的 `hbbench/scripts/`，`bench.test` 由 `go test -c ./gameplay/heartbeat/internal/bench` 生成 |

第一批测量（11:34～12:10）与其他 agent 的 sync-aoi 对照和 `go test ./...` 重叠，整批作废，未入库。
