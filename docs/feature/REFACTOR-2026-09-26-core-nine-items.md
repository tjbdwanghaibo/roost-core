# 三大模块九项优化实施方案

日期：2026-09-26。用户已授权全部实施。基线 main `8ce21a5` 加上一轮未提交的 Sync 公平调度；保留现有改动与失败证据。

交付更新：用户随后明确接受本文两组50ms尾延迟偏差，并授权整理规范后提交。代码已实施；原严格门禁仍失败，未更改阈值/样本。最新跨模块汇总、接受范围与agent规范见[交接文档](../CORE-OPTIMIZATION-HANDOFF.md)。下文“未commit/push”保留当时测试结束的状态，提交状态以后续Git历史为准。

## 范围与结构

保持 `nest/`、`dataengine/engine/`、`sync/entitysync/`，通用 Entity 访问契约放在现有 `entity/`。
不迁移包、不创建新的执行池或业务层包装。Nest → Guard 锁内冻结/事务准入 → 解锁 →
持久确认/投影 → Sync 交付的边界保持；每个实体同步包完整编码，不按任意字节切断。

| 编号 | 当前依据 | 目标与实施位置 | 验收 |
| --- | --- | --- | --- |
| S1 | 冷恢复约 5 万条，固定 1000/窗口使恢复天然跨窗口 | 与 S2 共用有界冷创建调度，减少实际恢复工作；同预算验证后做容量梯度，保留实时更新不扣冷预算 | 1000/10000/50，1%/5%，正常及集中恢复，报告首次可见、恢复与变化延迟，原 50ms 门禁不变 |
| S2 | 每轮从 subject 的全部订阅重建候选 | 在订阅状态转换时维护冷请求索引，计划只访问待快照请求，过期条目受 revision/lifetime 校验 | Hold/Ready/Close/重开、退订、Profile、部分成功重试、并发 race；CPU/分配对照 |
| S3 | Stats 全表加锁；历史错误证据不足 | 订阅/快照数随状态增量维护，保留显式完整核对入口；保持阶段错误信息并补运行观测 | 增量与完整核对一致，生命周期/并发测试，正式负载失败原因留存 |
| D1 | ReplayBatchBytes 只限制后续投影分段 | 回放读取同时受记录数和逻辑字节限制，首条超大记录独占以保证进度；不改变 WAL 格式 | 大记录/边界/取消/重放/跨段，无误 ack，真实 WAL 验证 |
| D2 | Remote 一批全部结束才补下一批 | 有界滑动 Remote 投影；补位只允许尚无 Entity/事务冲突的相邻记录，特殊内容保持屏障 | 慢首笔不挡独立后续、冲突顺序、失败连续前缀 ack、取消和关闭等完在途 |
| D3 | 后来的调度与预算版本缺持续实测 | 当前源码真实 Remote 持续容量、大记录及本地冷热/突发负载；统一保留配置与代码摘要 | 30 分钟持续验证及 24 小时入口；全量数据/版本/积压/超时核验 |
| N1 | preparedGetter 的缓存未命中会退回可能冷加载的 Getter | 正式快阶段采用明确的已加载访问契约；慢阶段预加载声明目标；动态缺失拒绝且给出可行动错误 | 快 worker 不因未声明冷加载等待；普通/慢/Remote/动态 Cast/生成链路回归 |
| N2 | QueueLen 混合同 ID 前驱与 worker 等待 | 暴露运行、就绪、前驱阻塞和内部续行；前驱/worker累计等待、合并等待年龄/峰值与准入拒绝 | 确定性控制队列验证各计数、清零与现有 FIFO 契约 |
| N3 | 每请求 ID 切片/依赖及统一锁可能有成本 | 先采集 allocation/mutex 基线，去除有证据的冗余分配；不凭猜测拆锁/引入池 | 同基准前后对照与并发顺序回归；无收益候选不进入生产代码 |

## 顺序、兼容与风险

先完成 D1/D2 和 N2/N3，再落实 N1 的 Entity/Kit/生成调用方契约，最后 S1–S3；所有代码固定后串行执行性能与真实依赖验收。
读取字节上限可能使一次 ReplayPass 返回更少记录，调用方必须按进度继续；不能把一轮误当全部完成。
Remote 补位不得跨过未知、混合、effect/receipt/迁移屏障，失败后停止补位并等已经开始的工作结束。
快阶段冷加载限制属于显式行为收紧，须提供错误与迁移说明；不在持锁中自动跳转慢池或重试已执行业务。
Sync 新索引只存请求身份/元数据，不缓存可变业务对象内容；统一状态转换、退订和清理，防止第二份真相失步。

各阶段跑必要 race/静态检查与正式生成工程；性能不能与 CPU 测试并行。保留所有失败运行和旧基线。
同进程混合验收同时驱动 Nest、持久化和 AOI Sync，落库频率独立于 20Hz 同步频率。
50ms 变化延迟与冷恢复完成时长分别披露，不修改现有总门禁，也不将短测或候选配置当成生产 SLA。
确认为 bug 的项目另登记问题与修复；尚未复现的历史 FlushFailures 保留未定位状态。

## 实现与验证进度

- D1：新增独立 `ReplayReadBytes`（默认 4MiB）及 Kit `projection.read_bytes`。边界前/边界上、首条超大记录、后缀保留及后续收敛测试通过。逻辑字节数不是精确堆占用，WAL 解码可能短暂保留一条 lookahead。
- D2：Remote 窗口上限从 worker 数改为回放记录/字节预算，仍要求窗口内全部 Entity 与事务身份互斥；2 个 worker 下首条阻塞时，第 3 条独立记录可前进。首次观察失败后停止补位、等待在途、按连续前缀 ack。回放、取消、冲突与失败测试及 race 通过。
- N1：仅包装快阶段的 Getter 调用 context，不改 handler 原始 context 身份。ManagerAccess 冷目标返回明确错误；动态 Cast 不会转移半个 handler。登记 [RR-20260926-02](../bugfix/RR-20260926-02.md)。自定义 Getter 必须履行同一契约。
- N2：准入时维护等待链表，启动时 O(1) 移除；Stats 区分前驱、worker 与续行占用。只有真正存在前驱的请求记 DependencyWait。确定性阻塞测试验证峰值、拒绝、年龄、耗时与排空归零。
- N3：allocation profile 指向指标 key 的中间分配；改为直接构建最终字符串，保持排序和转义。单 ID 复用 job 内嵌存储。mutex profile 未证明统一调度锁是瓶颈，因此保留它。
- S2：新增同包 `snapshot_requests.go`，在 kind 转换/增删订阅时维护待快照请求，按来源/会话缓存排序，预算用尽即停止选择。不再按 subject 的全部订阅重建候选。普通内容捕获仍受 subject 锁与 revision 保护；索引不保存实体内容。
- S3：Stats 使用订阅计数、请求索引数量、held 计数、pending 交集计数与按等待时间排序的链表，复杂度不随实体/订阅量增长。`AuditStats` 保留完整遍历。静止生命周期核对和“占住 subject 锁也能规划/Stats”的回归通过。
- D3：正式生成压测增加 `cold`、`ROOST_PERF_PAYLOAD_BYTES`、`ROOST_PERF_BURST`；可选 `ROOST_PERF_MIXED=1` 在同一个 Nest 中驱动真实 WAL/Mongo 与 Interest AOI、20Hz 内存变化及 Sync。完整解码验证时钟/版本/引用/最终 AOI 与 DAO 值；混合接收端在进程内，不当作网络延迟 SLA。

Entity、Nest、NestWAL、DataEngine、Kit DataEngine/Nest、Sync 全子包及 Metrics 的 race 通过；
vet、glsvet 通过。正式生成三进程持久化、双 Sync 模式、Remote async/strict/pipelined 验收通过。
隔离环境 NATS 未启动造成的首次 Remote 连接失败已保留，heal 后重跑成功。
100 Entity / 100 次持久请求的混合 race smoke 与 128 Entity / 32KiB 每 DAO 的冷加载 race smoke 通过。

### Nest profile 对照

相同 Go 1.27/M5、CPU=4、1s×3、mem/mutex profile：Single 原 3473–3541ns、2220–2252B、51–55 alloc；
现 3561–3589ns、1931–1971B、41–45 alloc。Multi 原 3826–3865ns、2557–2558B、61 alloc；
现 3959–3989ns、2317–2357B、48–52 alloc。
分配减少，但完整九项改动包含阶段约束和队列观测，吞吐没有提升，耗时小幅增加约 2–4%；
不能以分配下降宣称消息吞吐提高。mutex 累积约 6.5ms/8.8s，未支持拆分统一锁。
原始对照 `artifacts/perf/nest/nine-items/{before,after}*`。

## 复跑入口

先 `source /tmp/roost-dataengine-it/env.sh` 使用已存在的隔离环境。测试不修改生产部署。
每个 label 必须是新目录；输出记录 HEAD、工作树状态、关键源码 SHA256、配置和原始数据。

```sh
# Sync 同预算对照和容量梯度；trace/profile 默认关闭，仍以原 50ms 门禁退出。
ROOST_PERF_LABEL=<新标签> ROOST_PERF_COUNT=2 \
  ROOST_RECOVERY_BUDGETS='1000 2000 5000' bash scripts/perf/sync-recovery.sh

# 大记录、50% 初始冷实体和突发请求可分别/组合运行。
ROOST_PERF_LABEL=<新标签> ROOST_PERF_SHAPES=cold ROOST_PERF_POLICIES=strict \
  ROOST_PERF_ENTITIES=1000 ROOST_PERF_REQUESTS=2000 ROOST_PERF_PAYLOAD_BYTES=32768 \
  ROOST_PERF_RATE=500 ROOST_PERF_BURST=100 bash scripts/perf/dataengine.sh

# 同进程：持久交易 80TPS，AOI 1000 人/10000 Entity/约50可见，20Hz×1% 内存变化。
# 5% 改 ROOST_PERF_MIXED_DIRTY=5；持久写入频率不随 Sync 频率提高。
ROOST_PERF_LABEL=<新标签> ROOST_PERF_SHAPES=pair ROOST_PERF_POLICIES=strict \
  ROOST_PERF_ENTITIES=10000 ROOST_PERF_REQUESTS=1600 ROOST_PERF_RATE=80 \
  ROOST_PERF_MIXED=1 ROOST_PERF_MIXED_DIRTY=1 bash scripts/perf/dataengine.sh

# Remote 24小时入口（本轮只执行30分钟，24小时未运行）。
ROOST_REMOTE_LABEL=<新标签> ROOST_REMOTE_DURATION=24h ROOST_REMOTE_TIMEOUT=25h \
  ROOST_REMOTE_RATE=80 ROOST_REMOTE_SESSIONS=1000 ROOST_REMOTE_ENTITIES=10000 \
  ROOST_REMOTE_WORKERS=8 ROOST_REMOTE_FAST_QUEUE=4096 \
  ROOST_REMOTE_IO_WORKERS=1024 ROOST_REMOTE_SLOW_QUEUE=16 \
  ROOST_REMOTE_WRITE_LIMIT=128 ROOST_REMOTE_WAL_LIMIT=512 \
  ROOST_REMOTE_PROJECTION_WORKERS=8 bash scripts/perf/remote.sh
```

`mixed.StateAgeAtDecodeP99MS` 包括新可见对象带来的既有状态年龄；
`ExistingObjectChangeToDecodeP99MS` 单独统计已经持有对象的状态变化。两者均非 TCP 完成延迟；
网络 SLO 由单独的 sync-aoi 服务端/客户端测量。混合测试同时检查最终 AOI、全量 DAO 值/版本、
投影数量、WAL 排空、同步版本链与零丢会话，不能仅看两个 TPS 数字。

## Sync 与本地持久化实测

正式 AOI 每次 200 tick，服务端/独立 TCP 客户端各 CPU=4，1%/5% 状态变化，
1000/10000/50、20Hz、10 批事件。下表 `capped` 与上一轮历史参数一致：
每会话 10 个对象、tick 50 同时恢复 1000 个会话，trace/profile 关闭。

| 场景 | 变化 p99 ms（两次） | 超50ms数：计划/变化（两次） | 原门禁 |
| --- | --- | --- | --- |
| 正常1% | 2.35 / 2.401 | 0/0、0/0 | 通过、通过 |
| 正常5% | 4.11 / 5.57 | 0/0、0/0 | 通过、通过 |
| 恢复1%/1000 | 25.688 / 20.928 | 0/0、0/0 | 通过、通过 |
| 恢复5%/1000 | 14.443 / 10.473 | 14/1、0/0 | 失败、通过 |
| 恢复1%/2000 | 15.393 / 20.944 | 0/0、14/5 | 通过、失败 |
| 恢复5%/2000 | 14.71 / 17.879 | 0/0、0/0 | 通过、通过 |

**集中恢复的严格总门禁仍未全绿**，不能宣称全部对象都在50ms内可见。
1000 预算/5% 的失败样本包含已有基线 delta，不能统称为冷创建：14 次计划超标中，
1 次变化超标。2000 预算/1% 第二次包含冷 create 超标。保留全部 outlier，未省略。

同预算5%恢复服务端总分配约1.974GB，上一轮约2.16GB，减少约9%；
1% p99从上一轮32.36–34.945ms降至20.928–25.688ms，5% p99样本区间有重叠。
这些是本机短测，不能外推线上CPU或网络的保证。所有这些正式样本 FlushFailures、SessionsLost、SendErrors 均为0。

另保留 `nine-recovery-*` 的无每会话限额、tick40恢复梯度：1000/2000/5000总预算，
大约2.65–4.40s / 1.25–1.60s / 0.50–0.55s达到服务端当时兴趣集合的准入检查点。
5000档的1%两次均有计划超标，额度增加有瞬时CPU代价，不能只按恢复秒数选配置。
该探索脚本执行中调整过后续参数，末尾因脚本读取偏移退出2；12个样本都完成且配置落盘，
仅作探索证据，不充当与历史相同参数的正式对照。固定脚本另跑出的正式结果为上表 `nine-capped-*`。

`nine-visibility` 是独立诊断：trace 262144、完整证据、无覆盖/遗漏/未匹配 create。
新可见 p99=831.625ms，恢复基线 p99=3683.952ms；原公平调度分别869.002/3687.029ms。
1000个恢复客户端全部按当时AOI校验，检查点上界5199.406ms。trace运行不计入性能门禁。

| 本地真实 WAL/Mongo 负载 | 请求完成 TPS | 请求 p99 ms | 最终 WAL / 投影失败 |
| --- | ---: | ---: | --- |
| 双实体×双DAO，每DAO32KiB，strict | 939.01 | 52.251 | 0 / 0 |
| 1000实体，50%初始冷，strict | 645.76 | 104.876 | 0 / 0 |
| 两个热点实体×双DAO，strict | 138.39 | 249.020 | 0 / 0 |
| 每批100、目标500TPS，pipelined | 522.66 | 19.117 | 0 / 0 |

最后一组按整批投放，最后一批提前完成会使短样本平均值略高于目标500；不能视为容量上限。
每组都检查全部内存/Mongo的DAO值和版本，大记录还核对完整Payload；测试全部通过。
热点组1000笔请求集中到同两个ID，体现同ID串行约束，不能与大量独立实体的吞吐直接比较。

### 正式生成混合链路

同一个进程/同一个 Nest、真实文件WAL/Mongo；10000 Entity、1000接收者、AOI最多50（含自身），
每组20秒、1600笔双实体×双DAO严格持久事务，内存状态变化独立20Hz×1%/5%。

| 变化率 | 持久TPS | 请求p99 ms | 状态变化数 | 已有对象变化→解码p99 ms | 最终检查 |
| --- | ---: | ---: | ---: | ---: | --- |
| 1% | 80.026 | 15.537 | 40000 | 6.142 | DAO/版本/WAL/解码/AOI均通过 |
| 5% | 80.017 | 15.234 | 200000 | 14.715 | DAO/版本/WAL/解码/AOI均通过 |

两组 final_stats.WALUnacked/ProjectionFailures、Sync.Pending/PendingSnapshots/FlushFailures/
SessionsLost、Nest.Queue.Fast/Slow.Rejected 均为0。每组 committed/projected=11600（含10000笔初始化），
额外40000/200000次内存变化没有被当成持久TPS。接收端为进程内协议解码，不是生产网络SLO。

原 `nine-mixed-d1/d5` 的 ChangeToDecodeP99MS 混入新可见对象携带的旧状态年龄（1%约4.95秒），
没有丢弃旧结果；正式最终工具保留 StateAgeAtDecodeP99MS，并新增 ExistingObjectChangeToDecodeP99MS。
修正的是观测口径，原同步门禁完全未改。最终口径通过独立混合race smoke后，两组均重新完整运行。

### Remote 当前版本容量

1000业务会话、10000 Entity、每事务2实体×2 DAO、strict，CPU=4；快池8/4096、慢池1024/16，
Remote写许可128、WAL未确认512、投影worker8。短测各60秒，启用阶段指标。

| 目标TPS | 完成数 | 完成TPS | 事务p99 ms | 错误/丢弃 | 全量核验 |
| ---: | ---: | ---: | ---: | --- | --- |
| 80 | 4800 | 79.886 | 263 | 0/0 | Mongo与NATS全部通过 |
| 100 | 6000 | 99.795 | 346 | 0/0 | Mongo与NATS全部通过 |
| 120 | 7200 | 119.811 | 278 | 0/0 | Mongo与NATS全部通过 |
| 160 | 9600 | 159.581 | 399 | 0/0 | Mongo与NATS全部通过 |
| 240 | 13561 | 224.476 | 677 | 839/0 | 门禁失败，无verified标记 |

当前配置最高已验证零错误输入档位为160TPS，240TPS已过载；阶梯遇失败即停止，320未运行。
这只把本机短测容量边界限定在160到240输入TPS之间，不能视为精确上限或长期保证。
240档14400次请求中839次返回 `remote entity: capacity exceeded`，与 `WriteRejected=839` 一致；
Nest快慢队列Rejected、DataEngine AdmissionRejected及ProjectionFailures均为0，最终WAL排空。
成功请求的remote_confirm阶段平均479.894ms，durable_commit平均15.607ms，logic_queue平均0.061ms；
直接拒绝点是128个Remote写许可，确认等待占主要时间，证据不支持通过继续增加慢worker解决。
没有为提升数字放大许可或放松验收，也不将带拒绝的224.476TPS当成可用容量。
失败样本保留在 `artifacts/perf/remote/nine-upper-240/`，120/160成功样本在对应目录。
30分钟用80TPS验证持续性；
Remote事务确认延迟不能与客户端Sync的50ms要求混为同一指标。

### Remote 30分钟实际结果

`artifacts/perf/remote/nine-soak-80/`，持续1800.093秒，144000笔全部成功，
**79.996 TPS**；错误/丢弃均0。事务p50/p95/p99=133/223/582ms，max=1528ms，未出现SyncTimeout。
最终committed/projected=149000（含5000笔预热），WALUnacked/ProjectionFailures/FatalProjectionConflicts=0。
`.verified` 已产生：全部10000 Entity×2DAO的Mongo值/版本、NATS完整快照、拒绝/panic回滚及Remote outbox排空通过。

10秒采样的WAL峰值69（上限512）；堆86.94–307.58MB，goroutine1093–2228。
事务状态保留上限65536、默认TTL10分钟，活跃事务与保留的完成记录分别计数。
堆有明显GC波动且后半段高于初期，因此不据此宣称排除了内存泄漏；24小时复跑仍未执行。

| 运行分钟 | 堆min/median/max MB（10秒采样） | 保留事务最大数 | WAL在途最大数 |
| --- | --- | ---: | ---: |
| 0–5 | 86.94/144.27/189.49 | 28199 | 19 |
| 5–10 | 119.72/185.90/234.70 | 52199 | 13 |
| 10–15 | 128.09/203.31/251.91 | 64997 | 16 |
| 15–20 | 138.33/201.50/273.57 | 65142 | 19 |
| 20–25 | 161.63/244.11/290.45 | 65210 | 69 |
| 25–30 | 170.76/254.04/307.58 | 65281 | 26 |

## 本轮交付边界与索引状态

九项计划涉及的代码、回归测试、压测入口和说明均已实施；功能/race、vet、glsvet及上述正式生成验收通过。
性能验收不能统一标为通过：集中恢复的原始50ms严格门禁仍有失败样本；历史FlushFailures本轮未复现，
错误记录缺失已修复，但不能将其等同于找到了历史失败的根因。24小时长稳和完整集群故障矩阵本轮未执行。
本轮尚未commit/push。

性能运行结束后尝试刷新codebase-memory索引，worker被运行实例协调检查阻止：
`CBM index worker could not start: a pre-coordination or unverified CBM generation is active`。
未终止其他运行实例，也未删除索引锁。保留的generation仍是 `2026-09-25T11:41:37Z`，
不能作为本轮新实现的结构证据；本轮修改路径的coverage显示metadata_changed/not_tracked，
docs/scripts/testdata按规则excluded，均以直接源码核验和实际测试补足。
刷新失败与coverage原始响应保存在 `artifacts/perf/core-nine-validation/index-*.json`。

## 2026-09-26 外部复审后的更正

[逐项核实和修复](../review/REVIEW-2026-09-26-followup.md)确认 RR-03～06，并修复 WAL 回放计数、续行容量及 Remote 错误定位。
D1 原轮没有跨段/取消专项回归；本轮增加真实跨文件段、取消和边界计数测试。D2 增加确定性停止补位断言，消除既有 fatal-suffix 测试对完成顺序的依赖。
N1 原轮 mock Getter 不能证明直接 ManagerAccess/Repository 不可绕过，本轮增加真实访问入口与执行位置保护。
带 loader 时快阶段无法区分冷对象与持久不存在的对象，返回的冷加载错误不能当作 EntityNotFound；Multi 的可选冷缺失也须走 Slow 预加载，未配置 loader 的 nil 占位保持。
本报告此前的性能样本保留为对应版本证据，本轮未重跑性能或长稳。
