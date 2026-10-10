# Gate、Nest 与 Block AOI 性能验证

最新结果（2026-10-10）：快8、慢8、GOMAXPROCS=0（本机实际10）的10Hz/5%重档完整15分钟通过；此前GOMAXPROCS=4的失败仍保留，根因未关闭。各节按日期和参数区分，不以新配置通过覆盖旧失败。功能验证见[发布验收范围](../release/v1.25.0-VALIDATION.md)。

## 2026-10-09：Block AOI 改前样本

### 改前性能原始证据

全部为 Apple M5、macOS arm64、Go1.27.0、GOMAXPROCS=4；Gate/Game/测试客户端共享同一 Go 进程，broker 在本机独立进程。不是服务器单独 CPU、Linux 实机或跨机容量证明。客户端真实解码 Sync 并比较 AOI 最终可见集合及 DAO 值。1000 玩家、10000 Entity、每玩家10 msg/s、全部 Entity 挂 heartbeat；AOI 最大49加自己，可见集合实际约26～50。每轮 `(实体序号+轮次)%100` 选变化实体；玩家在地图前部，每次变化同时移动 AOI。

| 具名负载 | 实际结果 |
| --- | --- |
| gate-nest-onehour-20261009 | 最初无 AOI/Sync 的 1h Gate/Nest 负载，约200s后连接关闭/超时；确认已失败后定向停止，原日志与 Go stack 保留，无最终 JSON，不能当完整1h通过 |
| gate-nest-diagnostic-20261009 | 4m，240万消息+240万 heartbeat，计数/DAO值一致；Gate往返P99=7.6ms、max=168.253ms、>50ms=3889；当时有并发功能检查，是诊断，不是干净正式性能 |
| gate-nest-sync-refmap-20261009 | 60s，1Hz/1%；60万消息+60万heartbeat，计数/最终AOI值一致；消息完成P99=97.09ms，Sync提交→客户端P99=15.34ms、max=106.856ms、>50ms=591；整体门槛失败。此前误将 ObjectRef 当 EntityID 的测试客户端已修复，原失败日志保留 |
| gate-sync-profile-20261009 | 15s，1Hz/1%；15万消息+15万heartbeat，约9998/s各；消息完成P99=14.37ms、heartbeat=10.75ms，Sync提交→客户端P99=7.46ms；计数/最终值/该轮门槛通过，有CPU/heap profile，不是长稳 |
| gate-sync-heavy-20261009 | 60s，10Hz/5%；先出现 Nest queue full，随后绑定退出/EOF，最终校验失败，无有效最终完整 JSON；保留日志，不掩盖拒绝 |
| gate-sync-heavy-profile-20261009 | 15s，10Hz/5%；重现连接退出和最终校验失败，CPU/heap profile 保留 |
| gate-sync-lock-profile-20261009 | 15s，同样10Hz/5%；15万消息+150万heartbeat，约9996 msg/s、99962 heartbeat/s、5498变化/s；消息完成P99=36.07ms、heartbeat=20.31ms，Sync提交→客户端P99=22.11ms，计划输入→客户端P99=41.06ms/max=67.72ms/>50ms=1791；计数/最终值/该轮门槛通过，含CPU/heap/mutex/block采样，不当正式长稳或性能前后对比 |

Sync交付时延只采当时已有订阅的 delta；新进入AOI的全量可能携带很久以前的状态，不能把历史状态年龄冒充该次变化的交付延迟。全量/删除仍参与最终可见集合和数值校验。不同起点的长尾全部保留，不把 handler 已开始后的指标替代计划输入指标。

原始数据位于忽略的 `artifacts/perf/nest/<label>/`，功能日志位于 `/private/tmp/roost-gate-*.log`。重跑入口为 `scripts/perf/nest-game.sh`，使用 `ROOST_NEST_GAME_GATE=1`、`ROOST_NEST_GAME_SYNC=1`、`ROOST_NEST_GAME_DURATION`、`ROOST_NEST_GAME_HZ`、`ROOST_NEST_GAME_DIRTY`；此前以 env.txt 为准，不从标签猜实际时长/参数。开启 ROOST_NEST_GAME_PROFILE 会采CPU/heap/mutex/block，诊断采样本身影响调度。

### Block AOI 改前未关闭项（历史快照）

1. 首次约200s后的连接退出根因未唯一确定。已修复控制容量、阻塞Close、启动故障覆盖等独立问题，但不能据此认定原长测问题已经解决；常规档已完整运行15分钟且计数/最终值通过；其计划时延口径存在缺陷，不认定旧失败根因已经闭合。
2. 10Hz/5%/持续AOI移动的承载不稳定：较长样本队列拒绝/连接退出，15s采样又通过。需要首个失败原因、排队/持锁/资源曲线与独立客户端隔离，不能只加队列/期限或换场景强过门槛。
3. 共享 NATS 连接的 publish 和出站路径存在锁竞争，mutex profile 可见；这是性能候选，尚不能宣称是上述断线唯一根因，也不直接改成第二套队列。后续比较合批、连接分工或并发份额的实际收益。
4. Gate独立多进程的正式Account RPC+Mongo持久链路、Linux实机/跨机、Hash/旁观者/慢追帧的新通道矩阵、每个资源上限/不同Game故障隔离矩阵尚未完整验收。
5. P5常规档已有完整15分钟计数/最终值证据，但修正计时后的完整长测及重档仍未通过；另缺少paired embedded/Gate、独立吞吐上限、1000连接广播突发叠加业务/Sync，以及含少量Saga的正式混合负载；不能把历史Remote/Saga基线代替新Gate结果。
6. Stop 重试等待者问题已修复：每阶段复用唯一完成信号，不改变准入/排空/依赖释放顺序。synctest 修前20次取消留下20个 waiter，修后只有1个；三轮定向及真实 NATS dispatcher 阻塞→首次Stop超时→解除阻塞→Stop重试的race回归通过。
7. 最新Gate实现/现行文档尚未提交及合并；CBM已在停止性能测试后刷新，generation `2026-10-09T11:35:46Z`。

### 恢复验证结果

- `gate-sync-resume-heavy-15m-20261009`：沙箱不允许 loopback，连接阶段失败，未形成性能样本。
- `gate-sync-resume-heavy-15m-native-20261009`：宿主权限、10Hz/5%、目标15分钟；19:08:32启动，19:08:55首次 Nest queue full，随后 Gate forward Unknown/dispatch failed 关闭绑定。确认失败后对已核对路径的测试进程发送 SIGQUIT 保留栈并停止；没有完整15分钟通过或最终JSON。
- `gate-sync-resume-regular-15m-20261009`：1Hz/1%，完整900秒；900万业务+900万heartbeat全部完成、零拒绝、数据一致，200实体变化/s。原夹具报告Sync提交→客户端P99=7.61ms/max=197.227ms/>50ms=581、Gate往返P99=9.23ms/max=302.747ms/>50ms=14259。但计划输入相关P50/P95出现0，UnixNano跨TCP后丢失单调时间分量，负数原来被夹成零且未计数；因此只确认本轮完成数/最终值/存活时长，延迟验收等待修正口径重跑，原始JSON不改。
- 私有 NATS 监控保存于重档目录 `nats-monitor.jsonl`，每5秒采 varz/connz；采集晚于重档首错且跨后续常规档，分析必须按时间分段，不能当首错时刻精确证据。

Stop 收尾修复（已实施）：原有 WaitGroup 的准入/完成责任不变，仅在本包用一个排空完成信号包装每个停止阶段；在关闭该阶段全部准入后首次等待时建立唯一 waiter，后续 Stop 超时重试复用。每次调用的 context 只控制本次等待，不取消业务、不提前释放依赖。以 synctest 让20次取消重试的所有协程确定进入等待，验证同一阶段只有一个 waiter；另复跑真实 NATS 的 dispatcher 阻塞/解除/Stop 重试链路。压测期间只准备测试与文档，停止负载后运行红绿/race回归。

测量修复：同进程客户端/Game/Sync测量统一使用进程单调原点的偏移，经真实TCP和NATS传输后还原；不修改生产Budget的跨机墙钟协议。负延迟显式计数并使验收失败，附录记录末尾墙钟相对单调钟的漂移。此方法仅限同进程夹具，跨进程必须独立校准时钟或采用客户端往返指标。

- `gate-sync-heavy-firstfailure-20261009`：单调计时修正后10Hz/5%完整60秒通过，60万业务+600万heartbeat，约5499.6次变化/s；业务P99=20.06ms，HB=15.17ms，Sync提交→客户端P99=17.33ms/max=27.632ms；本轮负延迟及Sync>50ms均0，末尾墙钟相对单调钟漂移+0.799417ms。只是短预检，不能关闭重档不稳定项。
- `gate-sync-heavy-monotonic-15m-20261009`：同参数目标15分钟，35.432秒首次HB准入拒绝，失败；确认后停止自建负载。现场快池WaitingForWorker=4096、BlockedOnPredecessor=0、OldestWaiting=44.505ms、MaxWorkerWait=78.666ms；计划输入滞后40.672ms，due与已计划HB相差约4042。即时栈中四个快worker分别在指标标签处理、两个Nest current-dispatch sync.Map路径、Interest.queueFact锁路径；栈只是瞬间位置，不能证明某一把锁独占造成整个停顿。原始现场为 `sample-1.json.failure-1.stack`，不以停止时的栈替代首次失败。

### Block AOI 改前结论与复跑入口（历史快照）

- 常规1Hz/1%：900万业务+900万HB、零拒绝、最终DAO/AOI正确，堆采样峰值180.1MB，goroutine采样始终5454；原始计划时延可能被负数夹零影响，修正后尚无完整15分钟常规档结果。
- 重档10Hz/5%：修正后60秒通过，但两次目标15分钟的样本分别在约23秒/35.432秒失败；不能宣布重档稳定或据此发版。Stop修复不被描述成吞吐优化，计时修复也不改变负载或生产时钟协议。
- 重档首错前25.015秒NATS采样约73479条/s、24.25MB/s消息数据，采样连接pending=0、slow consumer=0；这不包括全部TCP/IP开销，5秒采样不能排除间隙峰值。此前mutex竞争仍是真实候选，但没有broker带宽饱和证据。
- 后续先把负载发生器/模拟客户端与Game执行隔离，保留原输入计划与1%/5%变化口径，定向采Nest上下文/指标/Interest临界区等待；再决定减少热路径公共状态操作或缩短AOI锁范围。不盲加队列、worker或超时，也不以短样本通过关闭长测失败。
- 复跑：启动私有loopback NATS后，设置 `ROOST_DATAENGINE_IT_NATS_URL`、`ROOST_PERF_LABEL`（新标签）、`ROOST_PERF_COUNT=1`、`ROOST_NEST_GAME_GATE=1`、`ROOST_NEST_GAME_SYNC=1`、`ROOST_NEST_GAME_DURATION=15m`、`ROOST_NEST_GAME_HZ=1` / `10`、`ROOST_NEST_GAME_DIRTY=1` / `5`，运行 `bash scripts/perf/nest-game.sh`。需允许loopback，沙箱拒绝不算性能失败。诊断profile另跑，不与正式延迟测量混用。
- Stop回归：`GOWORK=off go test ./infra/network/gateway -run '^TestGateStopRetriesShareOneDrainWaiter$' -count=3`；真实资源与race：设置私有NATS URL后 `GOWORK=off go test -race -tags=integration ./infra/network/gateway ./wiring/gate`，已通过。修前失败证据 `/private/tmp/roost-gate-stop-red-native.log`，修后三轮定向 `/private/tmp/roost-gate-stop-green-race.log`，完整包 `/private/tmp/roost-gate-resume-race.log`。没有执行新的跨机/Linux物理故障验收。

当时收尾记录：根包 `GOWORK=off go test -count=1 .`、Gate/Wiring定向vet通过；负载已全部停止，私有14222 NATS及本轮监控采集器已退出，没有操作用户共享NATS。未提交、推送、合并或发布。

### 2026-10-09：Block AOI 改后15分钟实测

21:06～21:37串行运行，未并跑race、profile或索引。Apple M5 / macOS arm64 / Go1.27.0，GOMAXPROCS=4，Nest快worker=4、共享等待容量65536。1000玩家、10000实体、10000业务消息/s，普通AOI可见26～50（含自己），on_change。Gate/Game/模拟客户端仍在同一Go进程，私有NATS 2.14.5独立进程，真实loopback TCP。此次没有把客户端独立进程化，也没有验证额外observedBlocks扇出；不能当跨机、独占服务器上限或手写SpatialComponent专属压力证据。正式组件接入由上一节功能测试覆盖，本负载继续通过QueueMove进入相同Interest事实管线。

| 指标 | 常规1Hz / 1% | 重档10Hz / 5% |
| --- | --- | --- |
| 目标负载时长 / 测试总耗时 | 900s / 902.88s | 900s / 902.84s |
| 业务完成 | 9000000，9999.98/s | 首错时8020156；无最终完成统计 |
| HB完成 | 9000000，9999.98/s | 首错时80284275；无最终完成统计 |
| Entity修改 | 180000，200/s | 目标5500/s，失败后不能作为达成吞吐 |
| 业务计划→完成P99 | 17.50ms | 无最终JSON，不报告全程P99 |
| HB计划→完成P99 | 12.18ms | 同上 |
| 修改实体业务 / HB完成P99 | 17.17 / 12.19ms | 同上 |
| Sync提交→客户端P99 / max / >50ms | 20.06 / 148.643ms / 4096条 | 同上 |
| Sync计划输入→客户端P99 / max / >50ms | 35.59 / 219.778ms / 24985条 | 同上 |
| Gate往返P99 / max / >50ms | 13.78 / 167.119ms / 23046条 | 同上 |
| Nest快队列峰值 / 拒绝 | 845 / 0 | 第二次首错达65536，并出现拒绝 |
| 计数 / 最终DAO与AOI / 延迟 | 全部通过，负延迟0 | 失败，EOF及客户端最终值不一致 |
| 进程堆采样峰值 / goroutine范围 | 195.66MB / 5454～5458 | 无最终采样报告 |

常规完整JSON：`artifacts/perf/nest/block-aoi-regular-15m-20261009/sample-1.json`。业务/HB/Gate P99每16次采样，>50ms和max统计全部完成事件；不能拿Over50MS除以直方图Samples计算业务/Gate比例。Sync交付延迟只计已有订阅的delta，本轮4237853条；全量和删除另参与最终值/可见集合校验。常规累计分配736.50GB（含客户端/Gate/Game整个进程）、8725次GC，不是常驻内存；分配成本仍有分析价值，不因P99通过忽略。

重档失败顺序（原样保留首错，未中途终止、未改参数重跑覆盖失败）：

1. **802.9190305s**：业务入口 `gateway: admission capacity exhausted`。Nest QueueLen=7050、WaitingForWorker=6920、BlockedOnPredecessor=130、Running=2、历史PeakWaiting=12665、Rejected=0；OldestWaiting=101.344ms，MaxWorkerWait=128.939ms。此刻Nest尚未满，不能归结为65536容量不足。
2. **805.464563875s**：heartbeat首次 `nest: dispatch queue full`，WaitingForWorker=9996、BlockedOnPredecessor=55540、Running=4，合计65536；最老等待684.944ms，最大worker等待280.306ms。随后出现业务worker拒绝、连接退出和订阅重试。
3. 15分钟结束时EOF及最终值不一致（示例client=0、subject=2824：期望X/HP=404/50，收到404/4）。`awaitValues`在写最终JSON之前Fatal，因此本次没有最终JSON，不能用首错计数推算全程吞吐或编造P99。

首错现场在 `artifacts/perf/nest/block-aoi-heavy-15m-20261009/sample-1.json.failure-0.stack`、`failure-1.stack`；完整失败日志为同目录 `sample-1.log`。保留env.txt、测试二进制和fixture摘要。两个目录各有nats-monitor.jsonl，每5秒采样；共用同一个私有broker，所以重档计数是累加值，计算速率必须作区间差。常规/重档监控最大pending分别7636/32308字节，slow consumer均0；重档首错前后采样没有持续broker积压。这不能排除5秒间隙峰值或客户端NATS连接锁竞争。

**2026-10-09该配置结论**：常规在修正单调计时后首次完整15分钟通过；重档仍失败，维护者接受此限制先发布v1.25.0；不能称重档瓶颈已关闭。首错从旧样本约35秒推迟至约803秒不等于吞吐提升的因果证明；本次同时改了AOI与队列容量，也没有同容量旧代码对照。下一步应围绕首错前Game/Gate准入占用、Nest调度停顿与分配/GC做定向定位，隔离负载发生器和客户端；失败路径应在最终对账前保存统计，避免Fatal丢失完整性能JSON。保留正式准入上限，不再通过扩队列或放宽期限强过门槛。

复跑使用前述脚本，固定 `ROOST_PERF_COUNT=1 ROOST_PERF_CPU=4 ROOST_NEST_GAME_WORKERS=4 ROOST_NEST_GAME_QUEUE=65536 ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 ROOST_NEST_GAME_GATE=1 ROOST_NEST_GAME_SYNC=1 ROOST_NEST_GAME_DURATION=15m ROOST_NEST_GAME_RACE=0 ROOST_NEST_GAME_PROFILE=0`，HZ/DIRTY分别1/1与10/5；为每轮指定全新ROOST_PERF_LABEL和私有ROOST_DATAENGINE_IT_NATS_URL。

两轮负载与本轮私有NATS/监控均已退出；没有操作共享服务。本次只补性能证据和现行说明，未改生产逻辑、提交、推送、合并或发布。

发布状态（2026-10-09）：维护者接受重档失败，v1.25.0已发布，tag提交487052dd；pretag与远端真实tag的game-demo消费工程build/vet/test全部通过。main已包含本轮实现。按维护者最新要求，本轮不继续分析重档原因。发布验收见[记录](../release/v1.25.0-IMPLEMENTATION.md)；上文未提交/未发布为当时历史状态。


### 2026-10-10：快8 / 慢8 / 默认CPU并行度重档

维护者将本轮参数最终指定为快worker=8、慢worker=8、GOMAXPROCS=0。Go1.27在本机Apple M5上实际采用GOMAXPROCS=10。1000玩家、10000实体、每玩家10业务消息/s、所有实体10Hz heartbeat、5%修改、on_change、20Hz兜底、AOI约49加自己、快等待65536、慢等待64均保留。15分钟输入窗口，测试总耗时902.90s，完成计时900.003845s。无race/profile/索引重建并跑。

| 指标 | 实际结果 |
| --- | --- |
| 业务消息 | 9000000全部完成，9999.96/s，拒绝0 |
| Heartbeat | 90000000全部完成，99999.57/s，拒绝0 |
| Entity修改 | 4950000次，5499.98/s |
| 业务计划→完成P99 / max | 18.63 / 205.483ms |
| HB计划→完成P99 / max | 12.92 / 109.673ms |
| 修改实体业务 / HB完成P99 | 18.66 / 12.98ms |
| Sync提交→客户端P99 / max / >50ms | 22.73 / 157.542ms / 43407条 |
| Sync计划输入→客户端P99 / max / >50ms | 33.53 / 248.197ms / 163253条 |
| Gate往返P99 / max / >50ms | 13.37 / 164.529ms / 16183条 |
| 快池等待峰值 / 最大worker等待 | 6332 / 60.279ms |
| 最终计数、DAO值、AOI集合、延迟门槛 | 全部通过，负延迟0；末尾可见28～50 |
| 堆采样峰值 / goroutine范围 | 216.38MB / 5434～5444 |

Sync已有订阅delta延迟样本40233643条，提交→客户端>50ms占约0.108%；保留长尾，按P99≤50ms验收。业务/HB/Gate分位数每16次采样，而>50ms统计覆盖全部完成事件，不能拿其Over50MS除以Samples算比例。Slow Started=0，此场景没有Remote慢准备，不能当慢池吞吐验收。累计分配约1.515TB是整个客户端/Gate/Game进程15分钟累计值，不是常驻内存。

源码基线f25527db，加本轮未提交的job紧凑布局/单调偏移调整；夹具增加ROOST_NEST_GAME_SLOW_WORKERS配置并写入结果（默认32保留旧测试默认值）。相比旧失败样本，同时改变快worker、CPU并行度及对象布局，因此只能确认此配置本轮通过，不能把收益单独归于worker或宣称此前Gate首错根因已关闭。Gate/Game/客户端仍同进程，broker独立，未覆盖跨机、额外observedBlocks高扇出或Remote/Saga混合。

原始产物：artifacts/perf/nest/heavy-fast8-slow8-auto-15m-20261010/（完整sample-1.json、日志、env.txt、源码差异source.patch、二进制与5秒NATS采样）。[随仓摘要](benchmarks/nest-heavy-fast8-slow8-auto-20261010.json)保留全部最终指标与内存峰值，省略每秒Samples数组。上一轮fast18/slow8/GOMAXPROCS4在维护者纠正参数后中断，目录heavy-fast18-slow8-15m-20261010内有INTERRUPTED.txt；不是完整样本，不混入通过/失败统计。

复跑设置ROOST_PERF_CPU=0、ROOST_NEST_GAME_WORKERS=8、ROOST_NEST_GAME_SLOW_WORKERS=8，其余沿用重档脚本参数并使用新标签。压测与自建NATS/采集器已全部退出，没有停止共享服务。此次不提交、不发布，不追加根因分析。
