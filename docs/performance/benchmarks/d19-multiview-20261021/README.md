# D19 多视野 Nest 模式失守：定位与修复证据（2026-10-21）

结论与表格见 [夜间补测 §14'.2a](../../NIGHT-20261020.md#142a-根因与修复2026-10-21)。本目录只放摘要；完整原始产物（每次运行的 server.json / client.json、cpu / mutex / block / allocs profile、日志、诊断构建的源码副本）在会话 scratchpad 的 `mv16/`，未入库。

环境：本机 Apple M5，Go 1.27，GOMAXPROCS 4 / 进程，`GOWORK=off`；sync-aoi 每次约 20 秒（10 秒测量、200 个 tick），只用于定位与前后对照。三点：B = `c1d21fba`；S1 = AOI 批次只评估移动过的目标；S2 = S1 + 排队事实异步应用（最终提交）。运行用的二进制由同一工作树按三点替换 `framework/sync/entitysync/policy` 的源文件编出，夹具与 `PolicyDuration` 计数三点相同。

| 文件 | 内容 |
| --- | --- |
| `sync-aoi-runs.jsonl` | 每次运行一行：改值→客户端 P99、帧数、Flush 次数与均值、政策阶段 / 打包下发两段（`policy_ms` / `pack_ms`，按 `ManagerCounters.PolicyDuration` 拆）、捕获 / 编码 / 准入、同口径每 tick 计时（`pol_tick_*` 政策阶段、`drv_tick_*` 压测线程里 `interest.Move` + `Apply`）。运行名 `f2-<点>-<场景>-<模式>-<样本>` / `fin-…`，`cb` = B、`c1` = S1、`cf` = S2 |
| `diag-hack-breakdown.jsonl` | 带计时的诊断构建（未入库，计时本身让数字偏大）：B / S1 `applyQueued` 每批的持锁、写阶段（`MoveSubject`、跟随视野）、并行重评、结算，事实数、跟随移动数、同实体重复数、因 `BatchSize` 推迟的批数；S2 不合批早期版本的应用线程每步与结算计时 |
| `pprof-B-*.txt` / `pprof-S1-*.txt` / `pprof-S2-*.txt` | `-profile` 运行的 CPU（flat / cum）、mutex、block profile 文本（本机没有 graphviz，不出 svg） |
| `benchstat-B-vs-S1.txt` | policy 包 AOI / Cluster / 视野基准，B 与 S1 同机交替 10 轮 |
| `benchstat-B-vs-S2.txt` | 同上，B 与 S2（最终） |
| `red-*.txt` | 两条回归在修前代码上的失败原文 |

场景参数：原场景 `-players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50 -views-per-player 16`；新场景另加 `-world=4000 -block=20 -enter-radius=30 -leave-radius=36 -move-amplitude=10 -entities=50000`，`-views-per-player 0 / 4 / 16`；direct 模式加 `-nest=false`。
