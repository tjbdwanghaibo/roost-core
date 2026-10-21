# 性能验证与复跑

性能报告按版本、场景和参数保留；通过仅对该次实际负载成立。业务功能验收归 [release](../release/v1.26.0-VALIDATION.md)，未关闭问题归 [已知限制](../maintenance/KNOWN-LIMITS.md)。

| 报告 | 范围与结论 |
| --- | --- |
| [Interest 单写者重构前后对比（2026-10-22）](INTEREST-SINGLE-WRITER-20261022.md) | 改前 `494134ae` / 改后 `11988df8` 同机交替：政策阶段在跑时业务线程上的直接调用均值与 p99 降一到两个数量级（`Enter`+`Leave` 23.4µs → 0.47µs），AOI 基准 ±2%；sync-aoi Nest ApplySync 0～8 视野 P50 低 6～11ms、P99 低 4～8ms，ApplyAsync 持平，ApplySync 16 视野仍超周期；修了 sync-aoi Nest 模式每 tick 调 `Apply` 的夹具问题（`9ad54c3c`）；direct 基线语义变化不可直接比；demo 登录风暴未测 |
| [D20 第 4 期：压测工具改为心跳驱动（2026-10-21）](HB-DRIVEN-LOAD-20261021.md) | 只交付代码、没有性能结论：sync-aoi `-move-driver=heartbeat`（实体心跳 + movement 巡逻）、nest-game `ROOST_NEST_GAME_HEARTBEAT_DRIVER=heartbeat`（生成 Unit 挂 `gameplay/heartbeat`），缺省仍是原全局循环；§7 微基准补正式驱动的假客户端基准、真实 Nest 开环投递与快池占满跳拍；冒烟与正式对比 2026-10-21 因宿主资源占用推迟到 v1.31 发版前压测，命令见该页 §4 |
| [D20 选项 C：Nest 批量心跳作业（2026-10-21）](NEST-BATCH-HEARTBEAT-20261021.md) | **2026-10-22 补测：不达标，建议暂不改缺省**：同时段 100% 移动进程 CPU X 3.03 / C 1.76 / Y 0.93 核，业务 p99 X 0.078 / C 0.235ms（由批量作业的时间片决定，100µs 时 0.19ms）；跳拍 / 准入失败 / 快池拒绝 0；普通 Nest 微基准 −9.9%；宿主中途换过一次时段（轻负载组 CPU 翻倍，原因尚未确认）。探索性运行据以改了两处设计：批量与普通作业两条就绪链交替（同一条 FIFO 时业务 p99 2.1ms）、8 个一组认领（逐目标取派发队列锁约占 13% CPU） |
| [D20 方案 D：Nest 批量入队原型（2026-10-21，实验记录）](NEST-DISPATCH-BATCH-20261021.md) | 原型 + 对比，判据未达标、分支不合入：`DispatchBatch` 批量入队、逐个执行；5 万实体 100ms 心跳 100% 移动时进程 CPU X 3.80 / D 3.38 / ssr 式批量 0.71 核；X 的成本主要是逐作业的 Nest 执行开销（含两个按 goroutine 登记的 `sync.Map` 争用）与 macOS 上的调度器空转。只保留 worker“结束后同锁取下一项”（短 handler 几何平均 −7.7%、完成 p99 −13.8%），随选项 C 合入 |
| [D20 心跳调度结构与投递方式（2026-10-21）](HB-STRUCTURES-20261021.md) | 原型 + 基准，不是正式实现：ssr 思路（固定档 + 相位分桶）改写为按档分环后收集 / 撤销 / 内存全面领先（ssr 的 map 多环写法慢 6～10 倍，最小堆慢约 100 倍）；5 万实体 100ms 经真实 Nest，50 万次/s 时单目标 Dispatch 4.25 核、DispatchBroadcast 1.90 核、ssr 式批量 0.77 核，后者绕过目标实体的事务与 Sync 捕获、不能原样采用；推荐按档分环 10ms、单目标 Dispatch、1 个驱动 |
| [生成工程完整链路的 strict 写入吞吐（2026-10-20）](GENERATED-TCP-STRICT-20261020.md) | 定位新用户试用第八轮“约 125 TPS”、不是容量基线：CLI 生成工程 + strict handler + 嵌入 TCP，50 连接各自一个玩家压测端 1253 TPS（WAL 每批约 9.9 条，上限约 short 池 worker 数 / 每笔持锁），同一实体 108 TPS；试用报告的形态与“请求落在同一实体”一致，单样本、非独占宿主 |
| [Lockstep 快照恢复限速链路演练（2026-10-20）](LOCKSTEP-SNAPSHOT-DRILL-20261020.md) | 功能演练、不是性能基线：嵌入 TCP 在 256 KB/s、64 KB/s 下 1 MiB / 4 MiB 快照都恢复、连接不断，传输耗时与“大小 / 速率”之比 1.00～1.04；独立 Gate（只准入，缺省约 492 KB/s）在 64 KB/s（1 MiB）、256 KB/s（4 MiB）下被 Gate 写 socket 超时断开、重连后从头再传，三次都未恢复（设计 §6.3 风险 1 成立） |
| [投影器合并 checkpoint 前后对照（2026-10-18）](SAGA-CHECKPOINT-20261018.md) | 特殊记录（带回执 / effect）与普通记录合并 checkpoint：积压时每条确认 0.56～0.75 次（修前 1 次），投影器约 15 → 20 条/s；正向 8/s 由崩溃变为 P50 517 ms / P99 793 ms，正向 10/s 由崩溃变为全部正确（P50 2.2 s），全补偿 5/s P50 3.9 s → 688 ms；正向 12/s、全补偿 7/s 起仍崩溃；持续档不变；真实 WAL 微基准全特殊 256 条 256 → 1 次 checkpoint；真实 Mongo 崩溃注入（引擎级 + saga 重启）恰好一次 |
| [Saga 原生步骤性能优化（2026-10-18）](SAGA-OPT-20261018.md) | 逐跳打点 + 投影后唤醒 outbox + 去冗余读 + 等回执移出 durable 回调，同机四阶段交替：持续档 Server P50 正向 440 / 399 → 278 / 314 ms、全补偿 597 / 610 → 352 / 424 ms（主要来自 outbox 唤醒）；吞吐上限未到正向 10/s（四阶段都在正向 10/s、全补偿 10/s 崩溃），正向 7～8/s、全补偿 5/s 从崩溃变为全部正确但延迟秒级；新的第一瓶颈是投影器逐条处理带回执 / effect 的记录（checkpoint 负载下 50～55 ms） |
| [Saga D12 前后对照（2026-10-18）](SAGA-D12-20261018.md) | `79d15114` 对 `63b63fa8`：旧路径（Mongo 步骤）代码相同、正向 20/s 与全补偿 10/s 两版本都通过（新版本一次样本因 Mongo 主节点超时失败，补跑通过，原因尚未确认）；原生步骤经真实 Nest + DataEngine，手写胶水与 `Steps` 在正向 5/s、全补偿 3/s 各两轮同档位无可测差异；两者在正向 10/s、全补偿 5/s 起饱和（单进程一个 durable 逐条处理 + 投影器单条处理带回执的记录），过载时仍恰好一次 |
| [性能四项（2026-10-15）](PERF-ROUND-20261015.md) | 长尾约 60% 集中在每分钟第 2 秒，与宿主 MDM / CorpLink 周期任务同秒，未归因到代码；nestwal 编码去反射保留（编码 −9%～−63%）；Remote 80 TPS×30m、Saga 正向 20/s 与全补偿 10/s 各 10m 通过；AOI 4 并行度（GOMAXPROCS=4、short 4）重档 30 分钟通过（旧样本约 803 秒失败，不做单变量归因） |
| [Nest 消息热路径优化](NEST-HOT-PATH-20261013.md) | 六项局部优化、96 组同机矩阵及异常交替复核；微基准不等于游戏混合负载容量 |
| [v1.30.0 候选发版前夜间补测（2026-10-20～21）](NIGHT-20261020.md) | main `69fb668f`，每项 15 分钟、服务端 / 压测端分开：嵌入 TCP 场景 Sync / self_sync 重档通过、出站预算拒绝 0；A-B-A-B 判断 Sync 提交→客户端 P99 +0.6～1.1ms 来自代码、10-19 的长尾差异不复现；Block AOI 4 并行度、Remote 80TPS、Lockstep 纯追帧限速、schema / Player 迁移演练通过；原生步骤 8 / 5 每秒全部正确但 P99 到 3.9s；WAL 放 RAM disk 时 checkpoint 56 → 1ms；Saga Mongo 步骤 30 / 15 每秒饱和；匹配队列每分区“成局人数/s × 保留期”持续上限约 4500 人；D19 每玩家 16 个额外视野时 Nest 模式失守（尚未确认） |
| [v1.29.0 发版后重档 30 分钟与 Saga 复测](V1290-HEAVY-30M-20261019.md) | 2026-10-19 同参数 30 分钟通过：业务 P99 13.09ms、Sync 提交→客户端 P99 15.43ms、Gate 往返 P99 6.78ms（比 v1.27.0 高 0.2～0.7ms，尚未确认），服务端业务 P99 4.37ms；长尾变多，同日 v1.27.0 对照同样变多，按宿主条件处理；Saga 正向 20/s、全补偿 10/s 各 10 分钟通过 |
| [v1.27.0 重档 30 分钟复测](V1270-HEAVY-30M-20261014.md) | 2026-10-14 同参数延长到 30 分钟通过：业务 P99 12.49ms、Sync 提交→客户端 P99 14.71ms、Gate 往返 P99 6.15ms，>50ms delta 0.0038%；堆与 goroutine 不随时间增长 |
| [v1.26.0 重档 15 分钟复测](V1260-HEAVY-20261013.md) | 2026-10-13 发布后同参数重档通过：业务 P99 12.24ms、Sync 提交→客户端 P99 14.24ms、Gate 往返 P99 6.04ms，>50ms delta 0.0098% |
| [2026-10-12 新增提交复验](REVIEW-20261012.md) | 配置快照/基准夹具修复与局部性能；15分钟重档目标在约6分钟出现Gate通道超时，未通过 |
| [v1.24.0 稳定基线](STABLE-v1.24.0.md) | Nest、Sync、DataEngine、Remote/Saga 的历史验收；不代替新增 Gate 验收 |
| [Gate / AOI 混合负载](GATE-AOI.md) | 1000玩家、10000实体、10000消息/s；2026-10-09常规通过、4并行度重档失败；2026-10-10快8/慢8/默认并行度重档15分钟通过；2026-10-15 4并行度重档30分钟通过 |
| [Nest 调度对照](NEST-DISPATCH-BENCHMARK.md) | ID hash / ID tail、10～500μs handler 与 job 布局；微基准不能直接换算真实服务器容量 |

## 证据存放

- [benchmarks](benchmarks/)：随仓的精简原始样本与结果摘要，保留环境、参数及失败边界。
- 根目录 `artifacts/perf/`：本地完整日志、profile、二进制、源码差异和高频采样，由 `.gitignore` 排除；全新克隆不含这些文件。
- 历史报告中的 `/private/tmp/` 路径是当时的本地证据位置，不承诺在其他机器可访问。

不删除失败来留下全绿结果，不把中断、仅编译、沙箱拒绝连接计为性能通过。不同参数、硬件或夹具版本不得直接归因比较。Sync按P99≤50ms验收，保留max与超过50ms数量；on_change指一个Nest handler完成后统一处理变化，20Hz为兜底。

## 复跑入口

混合负载使用 [nest-game.sh](../../scripts/perf/nest-game.sh)。最新重档参数如下；私有NATS连接、独立标签及其他准备步骤见[完整报告](GATE-AOI.md)。不要指向共享生产资源。

```sh
ROOST_PERF_CPU=0 ROOST_PERF_COUNT=1 \
ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_IO_WORKERS=8 \
ROOST_NEST_GAME_SHORT_QUEUE=65536 ROOST_NEST_GAME_PLAYERS=1000 \
ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 \
ROOST_NEST_GAME_GATE=1 ROOST_NEST_GAME_SYNC=1 \
ROOST_NEST_GAME_DURATION=15m ROOST_NEST_GAME_HZ=10 ROOST_NEST_GAME_DIRTY=5 \
ROOST_NEST_GAME_RACE=0 ROOST_NEST_GAME_PROFILE=0 \
bash scripts/perf/nest-game.sh
```

运行前另设唯一 `ROOST_PERF_LABEL` 和私有 `ROOST_DATAENGINE_IT_NATS_URL`。`ROOST_PERF_CPU=0`保留Go自动选择，不代表运行时只有零个逻辑处理器；记录实际GOMAXPROCS。race、profile与索引刷新不要并跑正式延迟测量。其他模块命令及微基准见各报告，本文档整理未重新执行压测。

长尾归因时在上面命令前加 `ROOST_NEST_GAME_TAIL=metrics`（每秒 runtime/metrics 与 rusage，开销小）或 `ROOST_NEST_GAME_TAIL=trace`（另开 Go flight recorder，超过 `ROOST_NEST_GAME_TAIL_THRESHOLD`（默认 50ms）的完成事件后保存前后窗口，`_TAIL_BASELINE_EVERY` 另存对照窗口）；其余变量见夹具 [tailtrace_test.go](../../codegen/internal/entity/testdata/nestgame/tailtrace_test.go) 文件头。产物在样本目录的 `sample-N.tail/`，脚本结束后自动用 [tailtrace](../../scripts/perf/tailtrace/main.go)（独立模块，依赖 `golang.org/x/exp/trace`）生成 `analysis.txt` / `analysis.json`：长尾事件与 GC / STW / 调度延迟 / 互斥等待 / 可运行队列的关联与富集比、长尾簇的墙钟分钟相位，以及 trace 中各分段的 goroutine 状态；单个 trace 的簇内对照用 [burst](../../scripts/perf/tailtrace/burst/main.go)。宿主噪声在本机很明显，见 [2026-10-15 报告 §1](PERF-ROUND-20261015.md#1-长尾延迟定位)。默认关闭；开启后有观测开销，结果只用于归因，不与无采集样本的延迟数字比较。

### 嵌入 TCP 重档

[KNOWN-LIMITS“v1.29 待补测”](../maintenance/KNOWN-LIMITS.md#v129-待补测)原第 1、2 项的入口（夹具 2026-10-20 加入；2026-10-20 夜间两档各 15 分钟已跑，结果见[夜间补测 §2](NIGHT-20261020.md#2-第-12-项嵌入-tcp-重档)）。`ROOST_NEST_GAME_EMBEDDED=1` 让玩家连接直连 Game 进程内的 `gateway.TCPServer` + `TCPRuntime`（接线同生成的 player TCP Mod：`NewTCPServer` → `ConnectRuntime` → `ConnectHandshake` → `Start`），不经独立 Gate 和 NATS，所以**不需要** `ROOST_DATAENGINE_IT_NATS_URL`。TCP 配置取生产缺省（`max_payload_bytes` 1 MiB、`write_timeout` 5s、`outbound.resident_bytes` 256 MiB），只改监听地址、连接上限与限流（与 Gate 模式的 TCPServer 一样关掉每连接限流）。夹具见 [embedded_load_test.go](../../codegen/internal/entity/testdata/nestgame/embedded_load_test.go)。

- **场景 Sync**（`ROOST_NEST_GAME_SYNC=1`）：负载形状与上面的独立 Gate 重档相同（玩家数、每玩家消息速率、全部 Entity 的 HB、修改率、Interest/AOI 上限 49 加自己、on_change 即时处理）。差别只在接入：会话 ID 是玩家 ID，Manager 的 Transport 是 `selfsync.PushPlayerFrame(TCPRuntime)`（与 game-demo 场景的 `sceneLane` 同一条），会话在鉴权 ACK 写出后的 `AfterACK` 以 held 打开、开跑前统一 Ready；回包与 Sync 推送都进每连接出站队列，由写协程写出。
- **自身视图**（`ROOST_NEST_GAME_SELF_SYNC=1`，只在嵌入模式下）：按生产装配 `selfsync.New` → `EntitySyncSetup`（Config → Configure → NewManager → Attach）→ `BindPlayers(TCPRuntime)` → `ConnectHandshake(SelfSync.Handshake())`；玩家 Unit 每次改动（业务消息与 HB 的修改）推给本人一帧，NPC Unit 不启用 Sync。Mode / Interval 与场景 Sync 相同（on_change、50ms）。**不能与 `SYNC=1` 同时打开**：一个 `SubjectSyncState` 只能登记在一个 Manager（`entitysync.ErrSubjectRegisteredElsewhere`），生产同样不支持场景与自身视图同时覆盖玩家实体（[PLAYER-ACCESS §7](../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)），夹具在开跑前拒绝这个组合。所以自身视图档没有 AOI 扇出，推送量约为“玩家改动次数”（1000 玩家 × 每秒约 1 次），远低于场景档，结果只与自身视图自己比较。
- 自身视图不在独立 Gate 下跑（生产支持 Gate + self_sync，经 Game Ingress 的 `OnActive` / `OnClosed`；本夹具没有接这条，需要时另加）。

晚上正式跑的命令（维护者 2026-10-20：每项 15 分钟。除 `ROOST_NEST_GAME_DURATION=15m` 外与 [V1290 复测 §2](V1290-HEAVY-30M-20261019.md#2-命令) 的重档参数逐项相同，只把 `GATE=1` 换成 `EMBEDDED=1`；不需要私有 NATS）：

```sh
# 1. 嵌入 TCP + 场景 Sync，15 分钟
ROOST_PERF_LABEL=perf129-embedded-scene-15m ROOST_PERF_COUNT=1 ROOST_PERF_CPU=0 \
ROOST_NEST_GAME_DIRTY=5 ROOST_NEST_GAME_DURATION=15m ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_EMBEDDED=1 ROOST_NEST_GAME_HZ=10 \
ROOST_NEST_GAME_IO_WORKERS=8 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_PROFILE=0 \
ROOST_NEST_GAME_RACE=0 ROOST_NEST_GAME_SHORT_QUEUE=65536 ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_SYNC=1 \
  bash scripts/perf/nest-game.sh

# 2. 嵌入 TCP + 自身视图，15 分钟（同一组参数，SYNC=1 换成 SELF_SYNC=1）
ROOST_PERF_LABEL=perf129-embedded-self-15m ROOST_PERF_COUNT=1 ROOST_PERF_CPU=0 \
ROOST_NEST_GAME_DIRTY=5 ROOST_NEST_GAME_DURATION=15m ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_EMBEDDED=1 ROOST_NEST_GAME_HZ=10 \
ROOST_NEST_GAME_IO_WORKERS=8 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_PROFILE=0 \
ROOST_NEST_GAME_RACE=0 ROOST_NEST_GAME_SHORT_QUEUE=65536 ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_SELF_SYNC=1 \
  bash scripts/perf/nest-game.sh
```

- 预计时长：每档生成 + 编译约 1～2 分钟，负载 15 分钟，建连、收敛与停机约 1 分钟，合计约 17～18 分钟；两档串行约 35 分钟。要同晚加跑一次独立 Gate 对照（V1290 §2 原命令，`ROOST_NEST_GAME_DURATION` 同样改为 15m，需私有 NATS）再加约 18 分钟，三档合计约 55 分钟。15 分钟样本与 V1290 的 30 分钟样本比较 P50～P99 可以，>50ms 条数与最大值要按时长折算且只作参考。
- 两档串行跑，不与 race、profile、`ROOST_NEST_GAME_TAIL`、索引重建或别的 `go build` / `go test` 并跑；长稳前看 `sysctl vm.swapusage`，按 V1290 §1 记录宿主负载。
- 冒烟（缩小规模、约 1 分钟）只说明夹具能起、报告字段齐全，不是性能结论。

嵌入模式额外输出（JSON 的 `Embedded` 与每秒 `Samples[].Outbound`、每分钟 `Minutes[].Outbound`）：

| 字段 | 含义 |
| --- | --- |
| `Embedded.RoundTrip` | 客户端写出请求 → 读到回包，服务端为主；与 `GateRoundTrip` 同口径，少 Gate 与 NATS 两跳 |
| `Embedded.CountersDelta.budget_exhausted` | `player_tcp_outbound_budget_exhausted_total` 在负载开始到 Sync 收敛之间的增量，正常负载期望 0 |
| `Embedded.CountersDelta.push_refused_retried / _closed` | `player.sync.push_refused.total{outcome="retried"/"closed"}`（`selfsync.PushPlayerFrame`） |
| `overflow` / `push_closed` / `write_errors` / `dispatch_errors` / `dispatch_timeouts` | 每连接上限溢出、推送关连接、写 socket 失败、dispatch 失败 / 超时 |
| `stranded_players` / `connections` | 自身视图滞留玩家（Gauge，分钟内取最大值）、当前连接数（Gauge） |
| `Embedded.GoroutinesAtStart / AtEnd` | 负载前 / 后按栈数出的写协程（`tcpSession.writeLoop`）与读循环（`TCPServer.serveConnection`）；取 goroutine profile 有短暂停顿，只在被测区间外取两次 |
| `Embedded.OutboundClean` | 上面几项拒绝 / 关闭计数都是 0；只报告，不是通过门槛 |
| `Embedded.QueuePeak` | 每连接出站队列峰值与全进程驻留字节**取不到**：在未导出的 `tcpSession` 里，框架也没有驻留 Gauge（KNOWN-LIMITS“嵌入 TCP 出站”） |

所有模式新增 `Minutes`：按负载分钟汇总每秒采样（业务 / HB 每秒、堆最小 / 平均 / 最大、goroutine 范围、short 池平均 worker 等待、200ms 慢 dispatch 增量），在负载结束后离线计算，不改变被测区间。延迟分位仍只有整段（按分钟的分位要开 `ROOST_NEST_GAME_TAIL=metrics`）。

## 读结果：压测端与服务端

维护者 2026-10-15 要求所有负载型压测把延迟拆成“压测端”（发生器节拍、模拟客户端排队与解码、观测轮询）和“服务端”（框架实际处理）两部分。各入口原有的端到端字段与通过门槛**不变**，保留它们只为与历史样本同口径比较；新增的 `Harness.*` / `Server.*`（JSON 小写为 `harness.*` / `server.*`）是同一段时间的拆分。评估框架延迟看 `Server.*`，`Harness.*` 偏大说明发生器或观测本身成了瓶颈。2026-10-15 之前的样本没有这些拆分；同一入口的分段除特别说明外在同一进程的单调时钟上取，各分段的分位分别统计，不能把分位相加。

“已分清”指原字段的区间本来就不含压测端等待；“新拆分”指原字段混有压测端等待、本次新增了分段；微基准（`nest.sh`、`sync.sh`、`nest-dispatch.sh` 等 `go test -bench` 的 ns/op）没有压测端等待的概念，不在此列。

### nest-game.sh（新拆分，2026-10-15 `44896aa4`）

**心跳由谁驱动**（D20 第 4 期，2026-10-21）：`ROOST_NEST_GAME_HEARTBEAT_DRIVER=ticker`（缺省，帧回调逐个投递，原负载）或 `heartbeat`（每个 Unit 挂 `gameplay/heartbeat` 登记，周期 `1s / ROOST_NEST_GAME_HZ`，须为 1、2、5、10、20Hz）；`--check-heartbeat` 跑接线短测。heartbeat 驱动下 `Heartbeats.Completion` 从回调开始算、`Harness.*` 为空，另有 `Beats`，见 [HB-DRIVEN-LOAD-20261021](HB-DRIVEN-LOAD-20261021.md#1-新增开关)。

`Messages` / `Heartbeats` 的 `Completion` 从计划时刻、`QueueWait` 从投递时刻起算，混有压测端等待（P99≤50ms 门槛仍按 `Completion`，偏保守）：

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `Harness.AdmitLag` | 计划时刻 → ticker 投递 | 压测端：发生器只在 Nest 10ms tick 里批量投递，天然 0～10ms；ticker 迟到也记在这里 |
| `Harness.ClientQueue` | 投递 → 模拟客户端写出请求 | 压测端：每个玩家连接一次只有一条在途请求，含等前一条回包；直接投递（heartbeat、无 Gate）恒为 0 |
| `Server.QueueWait` | 请求写出 → handler 开始 | 服务端：Gate、NATS、Game 接入、Nest 排队 |
| `Handler` | handler 本身 | 服务端 |
| `Server.Completion` | 请求写出 → Entity 释放 | 服务端合计 |
| `Completion` / `QueueWait` | 计划 / 投递 → 释放 / 开始 | 混合（历史口径） |

`GateRoundTrip` 是客户端写出到收到回包（服务端为主），嵌入 TCP 模式下同一段是 `Embedded.RoundTrip`。嵌入模式的 `Server.*` 区间是“请求写出 → Game 进程内 TCP 读循环 → Nest”，不含 Gate 与 NATS。Sync 的 `PlannedToClient` 从计划时刻起算、含压测端等待（混合）；`ChangedToClient` / `CommittedToClient` 从 handler 改值 / 提交起算，是服务端到客户端的同步延迟。

### nest-msg.sh（已分清，不改）

`p50_us` / `p95_us` / `p99_us`：dispatch 模式从取得 producer 信用之后、调用 `Dispatch` 之前，到 handler 释放后的 post-release 回调；request 模式从调用 `Request` 到返回。producer 等信用（闭环窗口）发生在计时起点之前，不计入，所以全部是服务端（Nest 排队 + handler + 释放）。闭环负载下这段排队由窗口大小决定，读数时与 `window_per_producer`、`producers` 一起看。

### dataengine.sh（新拆分）

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `harness.admit_lag_p95/p99/max_ms` | 计划时刻 → writer 调用 `Request` / `RequestMulti` | 压测端：计时器迟到，以及 32 个 writer 都在等上一条回复时的排队；只在 `ROOST_PERF_RATE>0` 时有值（`admit_lag_samples` = 请求数），闭环（默认 rate=0）为 0 |
| `request_p95_ms` / `request_p99_ms` | 调用 → 回复 | 服务端：Nest 排队、handler、按持久级别等 WAL / 提交 |
| `record_to_mongo_p95/p99_ms` | 提交记录生成（`CreatedAt`，墙钟）→ Mongo 投影返回 | 服务端（投影链路） |
| `scheduled_to_reply_p99_ms` | 计划时刻 → 回复 | 混合（历史口径），= admit_lag + request 按样本相加 |
| `mixed.StateAgeAtDecodeP99MS` / `ExistingObjectChangeToDecodeP99MS`（`ROOST_PERF_MIXED=1`） | handler 改值 → 进程内 Transport 解码 | 服务端（Transport 同步回调，无客户端排队） |

同一修改顺带修正了投影计时：Projector 对单条段优先走 `ProjectFenced`，夹具只包 `Project` / `ProjectBatch` 时这条路径绕过计时，`ROOST_PERF_RATE>0` 时稳定报 `projection samples=n-1 want=n` 失败（默认闭环全是批量段，所以没暴露）。

### remote.sh / remote-capacity.sh / remote-workers.sh（新拆分）

后两个入口逐档调用 `remote.sh`，字段相同。`result.json` 新增（1ms 桶向下取整，只计成功回复）：

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `Harness.AdmitLag` | 计划时刻 `due` → 发生器把任务放进通道 | 压测端：发生器在 1/rate 间隔的 ticker 上补发，ticker 迟到记在这里 |
| `Harness.ClientQueue` | 放进通道 → 会话 worker 调用 `RequestMulti` | 压测端：会话 worker 都在等上一笔回复时在通道里排队 |
| `Server.Completion` | 调用 `RequestMulti` → 返回 | 服务端：Nest 排队、加载、handler、WAL / Remote 写入与回复 |
| `LatencyP50MS`～`LatencyMaxMS` | `due` → 返回 | 混合（历史口径） |

`ErrorDetails[].ElapsedMS` 从调用起算，属服务端。三份直方图常驻约 1.4MB 堆，长稳 `heap_bytes` 的基线相应抬高，增长趋势不受影响。

### saga.sh（新拆分）

原 `P50MS`～`MaxMS` 从计划启动时刻算到 50ms 轮询读到持久终态，混有发生器排队与轮询间隔。服务端终态时刻改从引擎写出口取：测试把协调器的 Store 包一层 `terminalClock`，记下终态那次 `Store.Apply` 提交成功返回的时刻（`Store.Apply` 是协调器唯一的状态写出口，见 `stepTransition`），不改请求与结果。

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `Harness.AdmitLag` | 计划启动时刻 → 提交协程调用 `StartSaga` | 压测端：计时器迟到、64 格任务通道与 8 个提交协程 |
| `Server.Start` | `StartSaga` 调用本身 | 服务端（建记录并提交） |
| `Server.Completion` | 调用 `StartSaga` → 终态 `Apply` 返回 | 服务端：两步经 JetStream 派发、Mongo 步骤执行、完成回传与终态提交 |
| `Harness.ObserveLag` | 终态 `Apply` 返回 → 轮询读到终态 | 压测端：上界约一个 50ms 轮询间隔加一次 `Find` |
| `Minutes[].ServerP99MS` | 同一分钟分段的 `Server.Completion` P99 | 服务端 |
| `P50MS`～`MaxMS`、`Minutes[].P50MS/P99MS/MaxMS` | 计划 → 轮询读到 | 混合（历史口径） |

`Server.TerminalMissing` 非零说明终态有别的写出口、拆分不完整；各段带 `NegativeDurations`，非零说明时刻先后假设不成立（例如 `Find` 先于 `Apply` 返回读到提交）。

### sync-aoi.sh / sync-recovery.sh（新拆分）

**移动由谁驱动**（D20 第 4 期，2026-10-21）：`-move-driver=loop`（缺省，原负载：压测端循环发改值请求）或 `heartbeat`（只在 `-nest` 下：实体自己的心跳 + `gameplay/movement` 巡逻，另有 `-move-period` / `-move-speed` / `-movers`）。心跳模式 `planned_event_to_client` 的起点是这一拍消息的业务时间、不含驱动迟到，下表 `harness.*` 与 `server.request_to_change_ms` 只在 loop 模式有值；`server.json` 的 `heartbeat` 一节与口径差异见 [HB-DRIVEN-LOAD-20261021](HB-DRIVEN-LOAD-20261021.md#1-新增开关)。不加开关的旧命令得到原负载。

服务端与模拟客户端是两个进程；`planned_event_to_client` / `state_change_to_client` / `commit_to_client` 的终点是客户端解码校验完该对象。`server.json`（服务端进程，只计正式测量 tick，样本是每次状态变化）：

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `harness.admit_lag_ms` | 批次计划时刻 → 批次醒来开始发送 | 压测端：`time.Sleep` 迟到 |
| `harness.client_queue_ms` | 批次开始 → 本次状态变化调用 `Request` | 压测端：一个发生器串行发出整批，后面的等前面的 `Request` 返回 |
| `server.request_to_change_ms` | 调用 `Request` → handler 改值 | 服务端（`-nest=false` 时是直接改值，近似 0） |

`client.json`（样本是每个观察者收到的每次变化；`Changed` / `Committed` 是服务端进程墙钟，同机两进程比较）：

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `server.change_to_receive_ms` | handler 改值 → 客户端读完整帧负载、解码之前 | 服务端：Sync 间隔与 flush、传输（含 async 队列）、回环 TCP；客户端读协程被调度的时间无法分开，按服务端上界计 |
| `server.commit_to_receive_ms` | Nest 提交 → 同上 | 服务端 |
| `harness.client_decode_ms` | 读完整帧 → 解码校验完该对象 | 压测端：含同一帧里排在前面的对象 |
| `planned_event_to_client_ms` | 计划 → 解码完 | 混合（50ms 门槛之一，历史口径） |
| `state_change_to_client_ms` / `commit_to_client_ms` / `delta_*` / `full_*` | 改值 / 提交 → 解码完 | 服务端为主，含 `client_decode`（门槛之一，历史口径） |
| `transport_admission_to_client_ms` | Transport 写出 → 整帧解码完 | 服务端为主，含整帧解码 |

`flush_start_lag_ms` 是驱动循环 `time.Sleep` 迟到（压测端）；`server_active_work_ms` 是驱动线程一个 tick 内的忙碌时间，包括串行发出的请求（服务端为主）。恢复项：`recovery_admission_caught_up_ms` 在每个 tick 末检查一次，含最多一个 tick（50ms）的观察间隔，是服务端值的上界；`recovery_client_verified_ms` 只在诊断模式（`-trace-capacity>0`）有值，且服务端为核验暂停了输入，不作延迟比较。

### sync-business.sh（新拆分）

单进程；样本是每个客户端收到的每一帧，按 tick 对齐：

| 字段 | 区间 | 归属 |
| --- | --- | --- |
| `harness.admit_lag_ms` | tick 计划时刻 → 开始发送（与 `start_lag_ms` 同一段，按帧加权） | 压测端 |
| `harness.capture_ms` | 夹具记下本 tick 期望值 | 压测端 |
| `harness.client_decode_ms` | 读完整帧 → 解码并核对期望值 | 压测端 |
| `server.requests_ms` | 本 tick 的业务请求（AddExp + Moved，发生器串行发出，每条等回复）减去 capture | 服务端 |
| `server.flush_to_receive_ms` | Sync flush 开始 → 客户端读协程读完负载 | 服务端：flush、推送与回环 TCP |
| `server.completion_ms` | `requests` + `flush_to_receive` | 服务端合计 |
| `scheduled_to_client_ms` | tick 计划时刻 → 解码完（墙钟） | 混合（历史口径）≈ 上面五段按帧相加 |

`tick_work_ms` / `flush_ms` / `deadline_misses` 是驱动线程按 tick 的服务端工作量。game-demo 场景接口改为 `PushSyncPlayer(ctx, id, messageID, payload []byte)` 之后（`ba0dd7c2` / `634ae6ce` 一带，未逐提交确认）该夹具无法编译（`*businessTCP does not implement scenePusher`），本次一并改为新接口；之前的 sync-business 历史样本早于该变更。
