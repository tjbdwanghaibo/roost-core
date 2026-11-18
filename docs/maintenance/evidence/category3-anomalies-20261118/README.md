# 第三类：未归因偶发 / 疑点的复现与否证（2026-11-18）

范围：v1.55.0 之后维护者指定的三项——E-1 AOI workers=4 微基准差异、E-3 Gate 100/s 输入丢拍、UGC 物理长稳后段交付频率低 0.9%。
计划见 [remaining §E / §3](../known-limits-remaining-20261114/remaining.md)。

机器：macOS 26 arm64（Apple M5，10 核），**不空闲**：桌面应用常驻，测量期间 1 分钟负载 4.1～7.0、swap 已用约 6.7G / 8G。
所有测量串行进行，计时测量期间不编译；每次测量前后记 `uptime`（各节 `load.log` / `runs.log` / `run20.load`）。
codebase-memory-mcp 本次连不上，代码定位按当前源码与 grep。提交号只引用 hash，基线为 `b0ff526b`（v1.55.0 之后的 main）。

## 1. E-1：AOI workers=4 慢 7～11%

### 做了什么

三个测试二进制（`GOWORK=off go test -c ./framework/sync/entitysync/policy`），交替运行
`-test.run '^$' -test.bench 'AOIBatchRecompute/dense=false/coverage=false/workers=(1|4)$' -test.count 1 -test.benchmem`，每轮三者顺序轮换，`/Users/whb/go/bin/benchstat` 比较：

| 二进制 | 来源 |
| --- | --- |
| base | `b4699758`（临时 `git worktree add`，用完已删除） |
| head | `b0ff526b` |
| nobr | head 去掉移动路径上的 `observer.limited` 分支：`admitNearest`、`evaluateMoved`、`evaluatePair` 三处 `m.visibleLimit(observer)` 改回 base 的 `m.config.MaxVisible` 写法（[e1/nobr.diff](e1/nobr.diff)）；结构体字段与 `evaluateObserver` 的收紧逻辑保持 head 原样 |

11-08 证据说的“只多两次分支判断”指 `evaluatePair` 与 `evaluateMoved` 里经 `visibleLimit` 多出的 `observer.limited` 判断；`admitNearest` 在同一条路径上也改成了 `visibleLimit`（提到循环外），一并去掉。`evaluateObserver` 新增的收紧循环不在 `coverage=false` 的移动路径上，未动。

- 按计划 10 轮：[e1/r10](e1/r10/)（`run.sh`，负载 5.62～6.95）。
- 机器有负载，10 轮的单次离散度（CV 4～25%）与要找的差异同量级，**另跑 30 轮**：[e1/r30](e1/r30/)（同脚本 `seq 1 30`，负载 5.35～6.62）。
- 离散度 [e1/spread.txt](e1/spread.txt)，同轮配对比较 [e1/paired.txt](e1/paired.txt)。

### 结果（sec/op 中位数）

| 档 | base | head | nobr | head vs base | nobr vs base | head vs nobr |
| --- | ---: | ---: | ---: | --- | --- | --- |
| 10 轮 workers=1 | 5.484µs ±17% | 5.610µs ±5% | 5.668µs ±5% | ~ p=0.353 | ~ p=0.853 | ~ |
| 10 轮 workers=4 | 10.19µs ±10% | 10.91µs ±20% | 10.08µs ±10% | **+7.07% p=0.029** | ~ p=0.315 | **−7.58% p=0.011** |
| 30 轮 workers=1 | 5.754µs ±3% | 5.756µs ±4% | 5.750µs ±3% | ~ p=0.843 | ~ p=1.000 | ~ p=0.779 |
| 30 轮 workers=4 | 10.52µs ±2% | 11.18µs ±2% | 10.64µs ±4% | **+6.28% p=0.000** | ~ p=0.559 | **−4.79% p=0.003** |

同轮配对（workers=4，30 轮）：head 比 base 慢 25/30 轮（中位比 1.074），head 比 nobr 慢 26/30（1.054），nobr 比 base 慢 14/30（0.995）。workers=1 三对都在 13～15/30、中位比 0.99～1.00。B/op、allocs/op 三者完全相同。

离散度：单次运行 CV 5～11%（workers=4），10 轮里有离群值（base workers=1 一次 10.4µs）；30 轮把中位数置信区间压到 ±2～4%，配对符号检验也一致，**差异在这台负载 5～7 的机器上可分辨，不属于“负载下不可判定”**。

### 结论：确认存在；去掉分支后消失；归因为代码形态 / 布局，不是分支本身的执行成本

1. **差异真实**：head 比 base 慢约 6～7%（约 0.66µs / 批），只在 workers=4；10 轮与 30 轮一致。
2. **与这三处改动相关**：nobr 与 base 无显著差异、比 head 快 4.8%。按计划的判定（“显著且第三个不显著”）记为“代码布局”。
3. **不是分支执行成本**：serial（workers=1）路径执行完全相同的判断、次数相同，却没有任何差异（n=30，可分辨约 ±3%）。若 0.54µs 是 4 个 worker 关键路径上的判断开销，串行总量应约是它的 4 倍（约 +2µs，+38%），实际为 0。差异只出现在并行路径，与“每个判断多几条指令”不符。
4. **布局证据**：`go tool nm -size` 显示 head 相对 nobr：`admitNearest` 400→448 字节、`evaluateMoved` 1584→1600、`evaluatePair` 1840→1856，其后 `evictFarther`、`endBatch` 等符号地址整体后移 0x30～0x50。新唤醒的 worker 核上指令缓存 / 分支预测是冷的，代码形态变化在并行路径上更敏感——**这一机理是推断，没有用硬件计数器逐项证实**（尚未确认的只是机理，不影响“非分支执行成本”的结论）。
5. 量级参照：同一稀疏配置下 4 worker 本身就比串行慢 1.83 倍（10.5 vs 5.75µs，主要是每批起 goroutine 的开销），差异落在并行调度开销之内。

条件：macOS arm64 M5，load 5.4～7.0，Go 1.27.3，`dense=false/coverage=false` 一档；其他七个组合 11-08 已无显著差异，本次未复测。Linux 未测。

需要维护者定（产品选择，不自行改代码）：
- **A（推荐）**：接受，写进框架限制“一 / 广播与 AOI”。理由：只在稀疏、小批量的 4 worker 微基准出现，量级在并行起 goroutine 的开销之内，串行路径不受影响。
- B：试着把 `visibleLimit` 的结果在 `evaluateMoved` 入口算一次、传给 `evaluatePair`，再按本方法 30 轮复测；代价是改热路径签名，而且布局敏感的差异可能换个位置重现，不保证收回。

## 2. E-3：Gate 100/s 输入丢拍

### 做了什么

1. **给丢拍计数打原因标签**（`codegen/internal/roost/testdata/gate/load_test.go.txt`，即 `TestGateGeneratedBusinessConsumer` 生成工程里的 Gate 负载用例；`scripts/perf/gate-mixed.sh` 跑的就是它）。这个夹具是**一条请求在途的闭环**：`send()` 等 PB 应答和对应 Sync Delta 后才返回，请求被拒或超时会直接让测试失败。所以未发槽位只有两种来源：本拍先等过定时器、醒来已晚 → 压测端；没等定时器、是上一笔往返把节拍拖过了 10ms → 服务端往返。每个未发槽位记一个原因，合计等于 `Dropped`：
   - `client_beat_lag`：压测端定时器 / 调度醒晚；
   - 上一笔往返拖过间隔时，按它在途期间的服务端信号依次：`deadline_expired`（Robot 请求超时增量）、`admission_reject`（Nest 三池准入拒绝增量）、`heartbeat_skipped_beat`（heartbeat 跳拍增量）、都没有则 `slow_round_trip`；
   - 另记 `drop_detail`（往返里应答段 / Sync 段哪段更长、在途期间 heartbeat 是否修改过 World）、`drop_events`（前 256 次现场）、`load_timers`（负载区间内进程计时指标的次数与均值：Gate 分发、Nest 锁持有、WAL fsync）。
   - 测试新增断言“原因合计 = Dropped”；脚本的 python 校验同步改为合计。**变异**：去掉 `dropReasons[reason] += missed` 后用 1000/s × 3s 跑脚本，红在 `drop reasons do not balance: dropped=2722 labelled=0 …`（[e3/mutant-red.txt](e3/mutant-red.txt)），还原后下面七轮全部通过。
   - 现行文档同步：[ROBOT.md](../../../framework/ROBOT.md) gate-mixed 一段、[gate-pacing 计数口径](../../../performance/benchmarks/gate-pacing-20261114/README.md)。
2. **正式五轮**：私有依赖 `source ~/.roost-it/roost-dataengine-it/env.sh`，`ROOST_PERF_LABEL=gate-drop-<n> ROOST_GATE_LOAD_RATE=100 ROOST_GATE_LOAD_SECONDS=30 bash scripts/perf/gate-mixed.sh`，n=1..5 串行（[e3/run.sh](e3/run.sh)，[e3/runs.log](e3/runs.log) 含每轮前后负载与 swap）。
3. **两轮诊断**（n=6、7，同参数）：加 `load_timers` 后再跑，拆往返组成。
4. **独立 fsync 探针**（[e3/fsyncprobe.go.txt](e3/fsyncprobe.go.txt)）：同一数据卷上“追加 300 字节 + `File.Sync`”（darwin 走 `F_FULLFSYNC`）2000 次 × 2，无其他负载进程。

原始 `result.json` / `environment.txt` 在 `e3/gate-drop-<n>/`，汇总 [e3/summary.txt](e3/summary.txt)（`summarize.py` 生成）。

### 结果

| 轮 | 负载（前→后） | Scheduled | 丢拍 | 原因 | PB p50 / p99 / max（ms） | 丢拍前一笔应答中位（ms） |
| --- | --- | ---: | ---: | --- | --- | ---: |
| 1 | 6.54→5.67 | 3000 | 48 | slow_round_trip 48 | 7.80 / 18.48 / 25.15 | 15.31 |
| 2 | 6.41→5.30 | 3000 | 58 | slow_round_trip 58 | 8.51 / 18.48 / 29.83 | 17.65 |
| 3 | 5.30→6.48 | 3000 | 49 | slow_round_trip 49 | 7.80 / 18.48 / 23.95 | 15.72 |
| 4 | 6.48→6.10 | 3000 | 73 | slow_round_trip 73 | 8.51 / 18.48 / 63.99 | 17.53 |
| 5 | 6.10→5.84 | 3000 | 88 | slow_round_trip 88 | 8.51 / 18.48 / 21.22 | 16.61 |
| 6（诊断） | 4.69→5.36 | 2999 | 283 | slow_round_trip 283 | 8.51 / 21.95 / 30.41 | 17.48 |
| 7（诊断） | 5.36→4.11 | 3000 | 297 | slow_round_trip 297 | 9.27 / 21.95 / 28.44 | 17.75 |

- 七轮 896 个未发槽位**全部是 `slow_round_trip`**；`client_beat_lag`、`admission_reject`、`deadline_expired`、`heartbeat_skipped_beat` 都是 0；heartbeat 300 拍全部投递执行、无跳拍；Nest 准入拒绝、请求拒绝 / 超时 / 错误都是 0。
- 全部是应答段占大头（Sync 段最长 13.2ms 一次，其余 ≤ 7ms）；上一笔在途期间 heartbeat 修改过 World 的只有 1～9 次 / 轮，不是主因。
- 往返组成（诊断轮 6 / 7，负载区间均值）：Gate 分发 `player_tcp_dispatch_duration` 9.98 / 10.25ms ⊃ `handlerAddExp` 锁持有 8.94 / 9.39ms ⊃ WAL `nestwal.fsync.duration` 5.66 / 5.82ms（max 18.2 / 18.8ms），fsync 次数 2731 / 2718 ≈ 提交数（每笔 strict 提交一次 fsync，单条在途没有组提交可合并）。
- 独立探针：`F_FULLFSYNC` p50 3.74ms、p99 4.6～4.8ms、max 9.7～11.0ms，2000 次里超过 10ms 的 0～1 次。负载中的 WAL fsync 均值是它的约 1.5 倍、尾部到 18ms。

### 结论：否证“客户端节拍落后”；丢拍来自服务端 strict 提交往返，属于框架固有代价，不是可修的 bug

- **确认**：在 macOS 本机、私有依赖、单账号单在途闭环 100/s 下，丢拍全部由上一笔 PB 往返超过 10ms 间隔造成；往返的主体是 `handlerAddExp` 在锁内等 strict WAL 的 `F_FULLFSYNC`（均值约 5.7ms，约占锁持有的 60%），其余是 Gate / NATS / Nest 与 Sync。往返均值（约 8～10ms）贴着 10ms 间隔，尾部（p99 18～22ms）每次越过都丢一拍，所以丢拍数对环境很敏感（同参数 48～297）。
- **否证**：不是压测端定时器（`client_beat_lag` 0 / 896），也不是准入拒绝、请求超时或 heartbeat 跳拍。
- **不是可修的 bug**：strict 的契约就是锁内等持久（[NEST_PIPELINED_COMMIT §12.3](../../../framework/NEST_PIPELINED_COMMIT.md)：“单实体每次 fsync 一条是 strict 的固有代价，不改”）。夹具是单在途闭环，目标间隔与往返均值相当，本身就不能维持 100/s。没有发现比这更小、可以先红后绿修掉的缺陷。
- **尚未确认**：负载中 fsync 比独立探针慢 1.5 倍、尾部到 18ms 的来源（同盘 Mongo 副本集 journal、投影写入或其他进程争用）没有逐项拆分；诊断轮 6 / 7 丢拍是前五轮的 3～6 倍，PB p50 / p99 也偏高，原因（环境里同时有别的磁盘活动）没有核对。Linux（fsync 一般远快于 `F_FULLFSYNC`）未测，仍归 C-L8。

## 3. UGC 物理长稳：后段交付频率低约 0.9%

### 原始证据与夹具

- 原样本：[physicsroom-soak-20261029](../../../performance/benchmarks/physicsroom-soak-20261029/README.md) 的 `soak20-go.tsv`（`067d4606`，10-29 21:18 起 20 分钟），结论写在 [PHYSICS-ROOM-SERVICE 长稳一节](../../../framework/PHYSICS-ROOM-SERVICE.md#长稳rss-与原生内存2026-10-29macos-arm64)。
- 夹具：`wiring/physicsroom/load_native_test.go` 的 `TestPhysicsRoomSoak`（`physx && cgo`），机关场景 8 房间 × 20 逻辑客户端。每个客户端一圈是 `Submit` → `Poll` → `timer.Reset(50ms)` → 等定时器；空 Poll 为 0 时，**交付频率就是客户端循环频率 = 1 /（50ms + Submit + Poll + 定时器晚醒）**，与服务端物理频率无关，只有 Submit / Poll 变慢时产品才会让它变低。
- 交付频率按窗口计（每客户端每 15 秒的帧数 / 15），分辨率 1 帧 = 0.067Hz（0.34%）。

### 先看原样本：是一次台阶，不是随时间的漂移

`segments.py`（排除 vmmap 挂起窗口）对原样本：

| 段 | 交付 Hz | 物理 Hz | tick 间隔 p99 | Step p99 | Poll p99 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 15～600s | 19.718 | 49.998 | 21.61ms | 1.244ms | 0.460ms |
| 615～1200s | 19.545 | 49.995 | 22.10ms | 1.230ms | 0.426ms |

600s 之前的非 vmmap 窗口都在 19.67～19.73Hz，615s 起全部在 19.47～19.60Hz，此后不再回升：变化发生在 600～615s 这一个窗口里，之后是平台，不是逐渐下滑。同一时刻物理房间 tick 间隔 p99 也整体上移约 0.5～0.6ms（房间 tick 同样靠定时器），GC 次数（约 70 次 / 15s）与分配速率（约 458MB / 15s）前后不变，Poll p99 反而略低。这个形态指向“进程外的环境在某一刻变了、所有定时器一起醒得更晚”，而不是产品随运行时间累积的退化。

### 受控复现：给夹具加一圈的分段，再跑 20 分钟

夹具改动（只在显式启用的长稳测量里生效）：每个窗口多记 `loop_mean`（整圈）、`submit_mean`、`poll_mean`、`timer_late_mean`（醒来时刻减 Reset+50ms）与 `loadavg1`（`uptime`）。30 秒冒烟验证分解闭合：整圈 51.039 ≈ 50 + 0.184 + 0.069 + 0.775（[ugc/smoke.tsv](ugc/smoke.tsv)）。

运行：本机 PhysX 4.1.2 静态库（`/Users/whb/roost/.physics-deps/build`，10-25 按 `scripts/physics/build-native.sh` 构建）与当前 `bridge.cpp` 在工作树 `infra/physics/physx/lib/`（gitignore）链出 `libroost_physx.dylib`（主检出里那份是旧 ABI，缺 `rp_joint_*`，链不上）；`caffeinate -i`，不跑 `sample.py`（避免 vmmap 挂起窗口），`ROOST_PHYSICS_SOAK_MINUTES=20 ROOST_PHYSICS_SOAK_INTERVAL=15`（[ugc/run20.sh](ugc/run20.sh)）。前后负载 5.20→3.74（[ugc/run20.load](ugc/run20.load)），本机 `displaysleep 3`。运行结束后从 `pmset -g log` 取这段时间的显示器开 / 关事件（[ugc/display-events-soak20.txt](ugc/display-events-soak20.txt)），用 [ugc/align.py](ugc/align.py) 与每个窗口对齐（[ugc/align-soak20.txt](ugc/align-soak20.txt)，原始 [ugc/soak20.tsv](ugc/soak20.tsv)）。

| 显示器 | 窗口数 | 交付 Hz | 整圈 | Submit | Poll | 晚醒 | tick 间隔 p99 | Step p99 | 负载 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 开 | 66 | 19.843 | 50.400ms | 0.086 | 0.051 | 0.262 | 21.21ms | 0.94ms | 4.51 |
| 关 | 11 | 19.708 | 50.740ms | 0.143 | 0.072 | 0.524 | 22.22ms | 1.64ms | 4.65 |

- 12:17:03 显示器关（无人操作 3 分钟）→ 紧接着的窗口（375s）交付由 19.87 掉到 19.67～19.73，整圈 +0.34ms（晚醒 +0.26、Submit +0.06、Poll +0.02），tick 间隔 p99 +1.0ms，Step p99 +0.7ms；12:20:16 显示器开 → 555s 窗口之后回到 19.86～19.87。两次切换都在同一窗口里发生，**跟随显示器状态，不跟随运行时间**；负载在开 / 关两段几乎相同（4.5 vs 4.6）。
- 按时间对半（0～600 / 600～1200s）这一次后段反而高 0.25%（19.797 → 19.847），因为关屏落在前半段。
- 物理均频全程 49.99～50.00Hz，空 Poll 0，goroutine 206，测试 PASS。

**第二次运行（显示器保持常亮）**：同一二进制，`caffeinate -d -i`、6 分钟（[ugc/run6.sh](ugc/run6.sh)，负载 3.73→5.35，[ugc/soak6-display-held.tsv](ugc/soak6-display-held.tsv)；`pmset` 在 12:25 之后没有显示器事件，[ugc/align-soak6.txt](ugc/align-soak6.txt)）。前 135 秒 19.73Hz、晚醒 0.43～0.47ms；150s 窗口起台阶到 19.53～19.60Hz、晚醒 0.86～0.96ms，Submit / Poll 不变（0.14～0.16 / 0.06ms），之后保持到结束。对半：15～180s 19.682 → 180～360s 19.558（−0.63%），差异全部在晚醒（+0.31ms，整圈 +0.31ms）。负载由约 4.5 升到约 5.0，没有其他可见事件。这一次的开局本来就比第一次显示器开的状态慢（晚醒 0.45 vs 0.26ms、Submit 0.145 vs 0.086ms）。

### 结论：否证“产品随运行时间退化”；确认差异在夹具的客户端循环计时，随本机状态成台阶变化

- **确认（本机条件）**：两次运行里交付频率的每一次变化都能由“整圈 = 50ms + Submit + Poll + 定时器晚醒”的分段完全解释，变化落在定时器晚醒（两次都有）和 Submit / Poll 的 CPU 时间（只在关屏那次，变慢约 66% / 41%）上。物理均频 49.99～50.00Hz、空 Poll 0、goroutine 206 始终不变，没有产品侧的退化。夹具“响应后再等 50ms”的节奏把定时器晚醒原样换算成交付频率（晚醒每多 0.5ms，交付约低 1%）。
- **否证“后段”**：台阶出现在 150s、375s、600～615s（原样本）等任意时刻，关屏那次还在显示器重新打开后完全回升；按运行时间对半时“后段”可高可低（第一次 +0.25%，第二次 −0.63%，原样本 −0.89%）。“后段更低”是台阶恰好落在中点附近的结果，与运行时长无关。
- **已确认的一个触发**：显示器关闭（12:17:03 关、12:20:16 开，交付与晚醒在同一窗口里跟着切换，负载不变）。**不是唯一触发**：第二次显示器常亮，150s 仍出现一次只在晚醒上的台阶，伴随负载约 +0.5，具体来源（其他进程、调度器状态或 Go 运行时）**尚未确认**。
- **原样本**：形态（一次台阶后保持、tick 间隔 p99 同时上移约 0.6ms、GC 与分配速率不变）与这两次的台阶一致；10-29 的 `pmset` 日志已滚掉（本机只保留到 11-11），那一刻的显示器 / 机器状态无法核对，原样本的具体触发保留为推断。
- 条件：macOS 26 arm64（M5），8 房间 × 20 客户端，负载 3.6～6.3；Linux 未测（C-L7 仍在）。以后做长稳或频率对照：测量期间 `caffeinate -d -i` 保持显示器常亮，并看 `timer_late_mean` 列，交付频率的变化先扣掉晚醒再判断产品。

### 夹具改动

`wiring/physicsroom/load_native_test.go`：`loadWindow` 加整圈 / Submit / Poll / 晚醒累计，TSV 末尾追加 `loop_mean`、`submit_mean`、`poll_mean`、`timer_late_mean`、`loadavg1` 五列（旧列不变，`summ.py` 按列名读，不受影响）。只在显式启用的 `TestPhysicsRoomSoak` / `TestMultiRoomReplicationLoad` 中记录，`go vet -tags physx ./wiring/physicsroom` 通过；不改断言，不是修复，不做红绿。

## 收尾

- 临时 worktree（`b4699758`、nobr）已 `git worktree remove`；三个基准二进制、soak 测试二进制、fsync 探针在会话 scratchpad，不入库。
- 工作树 `infra/physics/physx/lib/libroost_physx.dylib`（gitignore）是本次链出的私有库，用完删除；要复跑按上文命令重新链接（或 `scripts/physics/build-native.sh`）。
- 没有起私有依赖进程（共享 `~/.roost-it` 已在运行，只 source 不打印），没有残留测试进程。

## 建议写进 KNOWN-LIMITS / CHANGELOG 的文字

### KNOWN-LIMITS

**删除**“二 / 同步：尚未确认的偶发与疑点”的这一条：

> AOI workers=4 的 7～11% 微基准差异：……

**加到**“一 / Sync、玩家接入与 Lockstep / 广播与 AOI”（若维护者选 A）：

> - **AOI 并行重评的代码布局敏感（E-1，2026-11-18）**：`BenchmarkAOIBatchRecompute/dense=false/coverage=false/workers=4` 在 `SetObserverLimit`（每观察者上限）合入后慢约 6～7%（约 0.66µs / 批，macOS M5、负载 5～7、30 轮交替 p<0.001）；去掉移动路径上 `observer.limited` 的三处判断即消失，但串行 `workers=1` 执行同样的判断却无差异，所以不是判断本身的开销，而是这几个函数的代码形态 / 布局在并行 worker 冷启动时的敏感（机理是推断，未用硬件计数器证实）。这一稀疏小批量配置下 4 worker 本来就比串行慢约 1.8 倍；不改。见[证据](README.md) §1。

**改写**“二 / 验收与运维 / Robot 推广验收（2026-11-01）”那一段，移到“一 / 验收与运维 / Robot 推广验收的范围（2026-11-01）”末尾：

> - **gate-mixed 100/s 的输入丢拍来自服务端 strict 往返（E-3，2026-11-18）**：夹具是单账号、一条在途的闭环，丢拍按原因分开计数（`drop_reasons`）。macOS 本机私有依赖 100/s × 30s 五轮丢 48～88 拍、两轮诊断 283 / 297 拍，全部是 `slow_round_trip`：上一笔 PB 往返超过 10ms 间隔；压测端定时器落后、Nest 准入拒绝、请求超时、heartbeat 跳拍都是 0。往返主体是 `handlerAddExp` 锁内等 strict WAL 的 `F_FULLFSYNC`（负载中均值约 5.7ms、最长约 18ms，独立探针 p50 3.7ms），往返均值 8～10ms 贴着间隔，所以夹具在 macOS 上维持不了 100/s，丢拍数随环境在 48～297 间变化。这是 strict 单实体每笔 fsync 的固有代价（NEST_PIPELINED_COMMIT §12.3），不是压测端问题，也没有可修的缺陷。负载中 fsync 比独立探针慢约 1.5 倍的来源（同盘 Mongo journal / 投影 / 其他进程）未拆分；Linux 未测，仍归 C-L8。见[证据](README.md) §2。

**改写**“二 / 玩法组件与物理房间 / 玩法联合链”一条，去掉交付频率半句：

> - **玩法联合链**：PhysX 动作执行器与 LevelComponent 的真实联合玩法尚未验收；RSS 页来源未逐区域核对。

并把“一 / 玩法组件与物理房间”里“多房间与内存的已测口径”中“交付频率后 8 分钟比前 8 分钟低 0.9%，原因尚未确认（倾向客户端定时器）”改为：

> 交付频率后 8 分钟比前 8 分钟低 0.9%：2026-11-18 复现确认是夹具客户端循环的计时（定时器晚醒与 Submit / Poll 的 CPU 时间）随本机状态成台阶变化，一个已确认的触发是显示器关闭，与运行时长无关，物理频率与空 Poll 不变（[证据](README.md) §3）；长稳测量期间应 `caffeinate -d -i` 并看 `timer_late_mean`。

（显示器常亮时仍出现过一次只在晚醒上的台阶，来源尚未确认；它只影响夹具的交付频率读数，不再作为产品遗留。）

### CHANGELOG `[Unreleased]`

> - 测试：`scripts/perf/gate-mixed.sh` 的 Gate 负载夹具把丢拍按原因分开（`drop_reasons`：`client_beat_lag` / `deadline_expired` / `admission_reject` / `heartbeat_skipped_beat` / `slow_round_trip`，合计等于 Dropped），并输出 `drop_detail`、`drop_events` 与负载区间计时 `load_timers`；`result.json` 不再有 `drop_reasons.client_pacing`，读它的脚本改为按合计校验。
> - 测试：物理房间长稳 TSV 追加 `loop_mean` / `submit_mean` / `poll_mean` / `timer_late_mean` / `loadavg1` 五列，用于把交付频率拆成客户端循环的各段。
