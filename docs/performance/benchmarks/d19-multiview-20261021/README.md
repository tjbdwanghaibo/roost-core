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

## 两种应用模式对照与夹具口径修正（2026-10-21，维护者决定之后）

被测 `b6ef5573`（测量时是变基前代码相同的提交）（ApplySync 16 视野用只把 `SyncApplyMaxExtraViews` 改成 16 的诊断构建），夹具用修好口径的版本（新订阅全量只计本 tick 改的值）。原场景 4 / 8 / 16 个额外视野、新场景 +4 / +8，ApplySync / ApplyAsync / direct 各两轮（交替顺序），每次约 20 秒，只做对照。结论与表格见 [夜间补测 §14'.2a 末尾](../../NIGHT-20261020.md#142a-根因与修复2026-10-21) 与 [DESIGN-TODO D19“两种应用模式”](../../../maintenance/DESIGN-TODO.md#两种应用模式2026-10-21维护者决定)；原始产物在会话 scratchpad 的 `applymode/`，未入库。

| 文件 | 内容 |
| --- | --- |
| `applymode-runs.jsonl` | 每次运行一行：运行名 `<场景 o/n>-v<视野>-<sync/async/direct>-<轮>`；改值→客户端 P50 / P95 / P99 / max（`chg_*`）、增量 / 全量 P99、帧数、可见均值、Flush 次数与均值、政策 / 打包两段、政策阶段每 tick P50 / P95、压测线程每 tick P50、Flush 失败数、`hb_overlap_bytes`（运行期间每 2 秒记录的 `pgrep -f roost-core-wt-hbbench` 输出字节数；`o-v16-async-2` 非零，是另一个 agent 的一条短编辑命令，不是基准） |
| `red-fixture-full-timing.txt` | `TestFullUpdateTimesOnlyThisTicksChange` 在修前夹具上的失败原文（全量样本 [2000.2 0.2] ms） |
| `red-apply-modes.txt` | `TestSyncApplyModeCapsViewsPerPlayer`、`TestApplyModeDecidesWhenACommittedMoveIsVisible` 在只加字段、未实现模式时的失败原文（sync/flush 子用例 50 次运行 50 次红） |
