# D20 方案 D：Nest 批量入队原型（2026-10-21）样本

报告：[NEST-DISPATCH-BATCH-20261021](../../NEST-DISPATCH-BATCH-20261021.md)。夹具：[gameplay/heartbeat/internal/bench](../../../../gameplay/heartbeat/internal/bench/)（`HB_MODE=d`）。

| 文件 | 内容 |
| --- | --- |
| `delivery-set1.jsonl` / `delivery-set1-runs.log` | 第一组：第一版代码（X / Y / Z / D；与最终代码只差 worker 的上下文选项按条构造、每条消息多 2 次分配，rebase 前的中间提交，未入库）、改前 `be5a76f5`（X0）、实验补丁（D+shard / X+shard），45 次干净运行的原始结果与每次运行的时刻、loadavg、作废记录 |
| `delivery-set2.jsonl` / `delivery-set2-runs.log` | 第二组：最终代码 （分支 `perf/nest-dispatch-batch`）（修掉每条消息多 2 次分配之后）重测 X / Y / D 与两个实验组，33 次 |
| `delivery-summary.md` | 两组按（组, 移动比例）汇总的中位数 [最小–最大]，由 `scripts/agg.py` 生成 |
| `pprof-cpu-*-cum.txt`、`pprof-mutex-*.txt` | 100% 移动的 CPU（按累计）与 mutex profile 摘要（X0 / X / D / Y / D-shard），与正式测量分开运行 |
| `profile-runs.jsonl` / `profile-runs.log` | profile 与执行追踪运行的计数；同一标签有多行时，最后一行是采用的那次，前面的是门控作废后重跑留下的 |
| `trace-block-events.txt` | 执行追踪里 goroutine 进入阻塞的次数与前 5 个调用点（`scripts/parse_block.py` 解析 `go tool trace -d=parsed`） |
| `micro-base.txt` / `micro-final.txt` / `micro-benchstat.txt` / `micro-runs.log` | 纯 Nest 微基准：改前 `be5a76f5` 与最终代码交替 6 遍的原始输出与 benchstat |
| `mutations.txt` | 13 条变异核对在最终代码上的失败文本（`scripts/mutate3.py`） |
| `exp-sharded-registry.patch` | 定位实验用的未提交补丁：Guard 作用域与当前 Nest 消息两个 `sync.Map` 换成 64 片读写锁 map |
| `scripts/` | 门控（`foreign.sh`：别的 go 构建 / 测试 / 压测进程，CPU>5% 或 compile/link；排除本轮自己的二进制）、正式运行（`run-formal.sh` / `run-formal2.sh`）、profile（`run-prof.sh`）、微基准（`run-micro.sh` / `run-micro2.sh`）、汇总与变异脚本 |

二进制由 `go test -c ./gameplay/heartbeat/internal/bench` 与 `go test -c ./framework/nest` 在对应提交上生成；原始 profile、trace 文件与二进制留在 scratchpad 的 `nestbatch/`，未入库。
