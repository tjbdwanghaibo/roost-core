# CellWorld 千人容量排查证据（2026-11-14）

基线 `067c14f7`（feat/v147），分支 `tmp/cellworld-perf`。macOS arm64 Apple M5（4P + 6E，10 核）/ 32GiB，Go 1.27.3。宿主机常驻负载 WindowServer、sysmond、CorpLink 合计约 1～1.5 核，swap 已用 6.5GiB / 8GiB。结论见 [CELLWORLD.md §千人排查](../../CELLWORLD.md#千人排查2026-11-14)。

- `core-runs.log`：`TestCellWorldMeasure`（1000 玩家 + 9000 NPC，4 Cell，30s 窗口）12 轮的原始输出与前后 `uptime`。轮名：`r1`～`r4` 由 zsh 后台启动（BG_NICE，nice 5），其余 nice 0；`before` = 067c14f7，`fix` = 本分支两处修复；`g4` = GOMAXPROCS=4，`g10` = 缺省；`nofull` = 实验开关（观察者成员全量刷新时不强制成员状态全量），**未提交**。运行时带了临时分段探针（Cell 排队 / 编码 / 发布，Base 回调入口滞后 / 解码 / 投递 / 回调空隙 / Nest 排队），探针与实验开关只在排查用的工作树里，见 `base probes` / `cell … probes` 行。
- `region-witnesses-*.txt`：`BenchmarkRegionWitnesses` 修前修后同机交替 6 轮与 benchstat。
- `mirror-renew-view.txt`：`BenchmarkMirrorRenewView` 逐个 Touch 与 TouchMany 对照（6 轮）。
- `robot-rerun.json` / `robot-rerun-summary.txt` / `robot-rerun-nats-monitor.txt`：`scripts/test-seamless-demo.sh` 容量档 1000 Robot（20s 测量）复跑，私有 mirror-local 环境（偏移 26000），每 0.5s 采 nats-server `/varz`、`/connz?sort=pending` 与 Redis `--latency-history`。代码是本分支修复后的版本。

- `core-runs-batching-repair.log` / `batching-repair-summary.md`：witness 按区块合批、对齐刷新相位与按缺口修复（`62f88d7a`）前后同机交替各 2 轮（1% / 5%），`before` = e056d3c0，`after` = 62f88d7a；两边都只加了 Base 接收回调占用探针。测量期间宿主机另有压 CPU 的任务，load average 10～30，见每轮的 uptime。
- `batching-repair-red-green.txt`：三条新回归在 e056d3c0 上的失败输出与修后通过输出。

pprof（CPU / allocs，Cell 与 Base 各进程）与完整日志没有入库，留在排查会话的临时目录。
