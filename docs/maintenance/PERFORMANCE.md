# v1.24.0 稳定版性能验收

阅读顺序：先看本页“最终结果”，再查具体模块的负载和统计。TPS表示每秒事务数，P99不等于最大延迟；术语解释见[入门手册](../GETTING-STARTED.md)。本页是带环境与参数的测量记录，不是所有游戏的容量保证。

状态：v1.24.0已发布，tag指向`2fa1c787`；性能满足本轮明确的业务指标，pretag、故障矩阵与真实tag消费者验收通过。维护者本轮要求：“开始性能验证，如果性能通过直接新版本发布，这个版本会作为短期内的一个稳定版本”。此前暂停负载的要求由本轮授权取代。性能基线为 `eac89694`，实施分支 `codex/stable-performance`已合入main；本次不部署。

## 范围与判据

- Remote：正式生成 DAO/Entity → Nest → 文件 WAL → Mongo → Remote → NATS 快照，1000 会话、10000 实体，每事务 2 实体 × 2 DAO，strict，输入 80 TPS，连续 1 小时。成功回复吞吐、请求错误、丢弃和最终数据一致性分别判定；要求错误/丢弃为零、全量核验通过，资源及积压保持有界。
- Sync：正式 Nest 与可靠异步发送，1000 会话、10000 实体、每人约 50 个可见实体，20Hz、10 批次、1%/5% 变化率，periodic/on_change 两模式。初测脚本沿用逐条严格 50ms 门禁及最终可见集合核验；维护者查看本轮长尾数量和最大值后，明确实际 on_change 业务按 P99≤50ms 验收，并保留、继续跟踪偶发长尾。原始严格门禁失败不改写，具体决定见下文。
- 维护者本轮补充：普通游戏业务才是主负载，Saga 占比不到 0.5%。确认按 1000 玩家 × 10 条消息/秒、10000 Entity 全挂 heartbeat 组件，1Hz 常规和 10Hz 较重档，1%/5% 修改率验证。Remote 专项不含 Saga，但不能代表此主负载。
- Nest 主业务新增 `scripts/perf/nest-game.sh`：正式生成 Entity/DAO/Component，正式 Nest ticker 投递与快池 Guard 执行；回滚用 undo，临时业务状态用 nopersist 字段。分别报告普通消息/heartbeat 完成率、真实 Entity 修改率、准入后排队、handler 时间、计划到 Guard 释放的 p50/p95/p99/max、全量 >50ms 数、队列/内存/GC 和逐实体结果。稳定档要求无拒绝、完整计数/数据一致且各类完成 p99≤50ms；最大值和超标数不隐藏。Sync 网络的严格门禁独立保留。
- 该主业务夹具只测本地临时状态和组件执行，不声称包含 AI/寻路复杂度、数据库或网络。心跳到期相位均匀分散；变化率是各类调用中实际修改实体的比例，不能与 Sync 专项“每 20Hz 窗口的变化比例”混用。低变化回调仍检查 DAO 状态；发生修改时经两个生成 setter 更新两个字段。
- 按维护者追加要求，稳定档之后单独逐档提高 Entity 修改频率和 heartbeat 实体数，找“零错误且延迟达标”的短测上限及首次饱和边界。饱和样本完整保留，不以拒绝后的成功吞吐冒充稳定容量，不修改生产队列或门禁迎合压测。
- 发布前另外执行 pretag、21 格真实资源故障矩阵；发布后下载真实 tag，生成并验证正式 game-demo。CPU profile 与正常延迟测量分开，性能测试期间不跑全仓编译、故障注入或索引重建。
- 1 小时是本轮长稳范围，不宣称 24 小时、物理跨机网络或线上容量上限已经验收。Windows 不在支持范围；macOS 实测和 Linux 编译/已有运行证据分开记录。

## 环境与复跑

本机 macOS/arm64、Go 1.27.0，有常驻后台负载。私有资源根 `/tmp/roost-stable-it-20261008`，端口偏移 26000；Mongo 三节点副本集、NATS 三节点 JetStream、Redis 与 toxiproxy 均由 `scripts/mirror-local.sh` 管理。env 含凭据，只 source、不打印、不提交。启动依赖、测试和停止在同一持续执行会话内完成。

```sh
export ROOST_MIRROR_LOCAL_HOME=/tmp/roost-stable-it-20261008
export ROOST_MIRROR_LOCAL_OFFSET=26000 ROOST_IT_MONGO_CACHE_GB=0.5
export GOWORK=off GOMAXPROCS=4 GOFLAGS=-p=1
bash scripts/mirror-local.sh up
source "$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it/env.sh"
export ROOST_REMOTE_CPU=4 ROOST_REMOTE_RATE=80 ROOST_REMOTE_POLICY=strict
export ROOST_REMOTE_SESSIONS=1000 ROOST_REMOTE_ENTITIES=10000
export ROOST_REMOTE_WORKERS=8 ROOST_REMOTE_FAST_QUEUE=4096
export ROOST_REMOTE_IO_WORKERS=1024 ROOST_REMOTE_SLOW_QUEUE=16
export ROOST_REMOTE_WRITE_LIMIT=256 ROOST_REMOTE_WAL_LIMIT=512
export ROOST_REMOTE_PROJECTION_WORKERS=8 ROOST_REMOTE_STAGE_METRICS=1
export ROOST_REMOTE_LABEL=<未使用的新标签>
export ROOST_REMOTE_DURATION=1h ROOST_REMOTE_TIMEOUT=2h
export ROOST_REMOTE_HEAP_PROFILE_MINUTES=10,30,60
bash scripts/perf/remote.sh
bash scripts/mirror-local.sh down
```

先以 2m、无 heap profile 完成预检，再启动新的完整 1h 样本；不能拼接历史中断数据。1h 在 10/30/60 分钟做 GC 后 heap 采样，统计包含这项稳定性诊断开销。

原始证据持久根：`artifacts/perf/stable-v1.24.0-20261008/`。保留各轮 `env.txt`（Git/源码摘要/参数）、压缩运行日志、进度 JSONL、最终 JSON、`.verified` 与 profile；不提交二进制、大日志和凭据。工作树清理前移动证据至主检出并核对。

## 普通业务与 heartbeat 复跑

```sh
# 每一档使用新 label；HZ=1/10、DIRTY=1/5 共四组，每组默认三个独立进程。
GOWORK=off GOFLAGS=-p=1 ROOST_PERF_CPU=4 ROOST_PERF_COUNT=3 \
ROOST_PERF_LABEL=<新标签> ROOST_NEST_GAME_DURATION=1m \
ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_PLAYERS=1000 \
ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 ROOST_NEST_GAME_HZ=10 \
ROOST_NEST_GAME_DIRTY=5 bash scripts/perf/nest-game.sh
```

每条消息锁内检查状态，按变化比例经两个正式生成 setter 修改 DAO。heartbeat 通过正式 ComponentManager 挂载；组件没有独立业务状态，计数/X/HP 都在生成 DAO 中。总完成数必须与 Nest ProcessedMessages、固定到期计划和逐 Entity 的最终字段值一致。普通消息默认落在 1000 玩家 ID，心跳落在全部 Entity；同 ID 顺序竞争真实存在。

普通/心跳全量完成计数，延迟 p50/p95/p99 按 1/16 序号抽样，实际修改路径单独全采样；最大值和 >50ms 次数覆盖全部完成事件。直方图分辨率为 10us，100ms 桶包含向上取整后触顶及溢出样本，同时输出该桶数量和未截断最大值；饱和档不得把该桶显示成精确 p99=100ms。

上限使用新 label：Entity 档设置 `ROOST_NEST_GAME_HZ=0`、`ROOST_NEST_GAME_DIRTY=100`、`ROOST_NEST_GAME_MESSAGE_TARGETS=10000`，逐档提高每玩家消息率；heartbeat 档保持普通消息 10000/s 和 10Hz、5% 变化，逐档增加实体数。每档 30s、首次不达标保留结果并停止上探；边界附近再细化，最后达标档延长确认。设置 `ROOST_NEST_GAME_PROFILE=1` 或 `ROOST_NEST_GAME_RACE=1` 只用于独立诊断/正确性样本，不混入容量结果。

## Remote 预检

`stable-smoke-80-eac89694`：120.095 秒，9600 笔成功，79.937 TPS，Errors=0、Dropped=0；p50/p95/p99/max=113/160/198/354ms，最终全量核验通过。它只证明预检通过，不代替 1h 结果。

## Remote 一小时结果

`stable-1h-80-eac89694`：3600.228 秒，288000 笔成功，79.994943 TPS，Errors=0、Dropped=0，未应用/结果不确定错误均为 0；p50/p95/p99/max=117/181/716/1643ms。独立接收及所有实体最终持久数据核验通过，`.verified` 已生成。

362 个进度样本中 heap 为 82.09～218.72MiB，末尾 122.89MiB；goroutine 为 1104～2298，末尾 1104。WAL 未确认峰值 62，结束归零；Remote 同时事务峰值 62、写入峰值 94，写拒绝 0，投影错误 0。完成后的 outbox/projection pending 为 0。事务记录周期回收，观测上限 65485，未随着累计 288000 次请求线性增长。10/30/60 分钟与结束 heap profile 均保留，GC后采样堆（pprof口径）在10/30/60分钟分别为117.54/116.04/110.24MB，结束101.74MB；主要是有界事务记录、兴趣注册表、快照缓存与WAL缓冲。本轮未观察到随请求总量持续增长，不能扩张为24h泄漏已经排除。

去掉预热，只统计 `remote_update` 的阶段计时：Remote prepare 平均 37.230ms、confirm 72.226ms、durable commit 19.995ms；普通 handler 平均 0.0026ms。`logic_queue` 每次请求两次续投、单次平均 0.173ms，不可与每事务时延直接相加。该场景的主要成本仍是持久/远端完成链路，不能作为本地业务消息吞吐指标。最大值是各自样本最大值，不把它们相加构造不存在的请求。

## Saga 复跑与判据

维护者确认 Remote Entity 80 TPS 已够用，新增独立 Saga 性能测试，不继续搜索 Remote 极限。Saga 以普通业务 10000 条/秒的 0.2% 作为代表输入，即每秒启动 20 个 Saga，分别连续 1 分钟运行两个步骤正向成功、第二步拒绝后补偿第一步，每类三个独立样本。

`scripts/perf/saga.sh` 需要 source 上文私有环境后运行：

```sh
ROOST_PERF_LABEL=<新标签> ROOST_SAGA_PERF_RATE=20 \
ROOST_SAGA_PERF_DURATION=1m GOFLAGS=-p=1 bash scripts/perf/saga.sh
```

真实 Mongo 副本集、三副本 JetStream，正式 `Assemble`、默认 Engine worker/批次配置、`SubscribeMongoStep` 及事务 inbox。每步修改 Mongo 业务字段并写入幂等效果记录；最终逐文档检查正向值为 2、补偿值为 0、每个成功步骤仅生效一次，outbox/lease 排空，存储/发布/worker 故障及错误终态为零。输入采用固定计划时间，积压不降低计划数量。完成延迟从计划启动到可读取持久终态，包含约 50ms 的观察轮询间隔。

`ROOST_SAGA_PERF_MODE=forward|compensate` 可只跑指定路径。20/s 全补偿首轮虽通过所有数据断言，实际完成仅 15.388 TPS、P99 19.497s，已判为性能饱和，不能据 `go test PASS` 宣称可稳定承担 20/s 全补偿；第二轮 17.287 TPS、P99 10.683s 同样有积压。原始两轮不删除，另补 10/s 全补偿对照，最终汇总以全部样本为准。

这是协调器及步骤提交链路，不含 Nest 发起意图的 WAL/Outbox 前段，也不是与 Nest/Sync 同进程竞争资源的全混合压测。Saga 是异步业务，不把 Sync 的 50ms 门禁套在事务完成上；报告真实完成率和 p50/p95/p99/max，要求所有计划事务正确完成并排空。生产复杂步骤、物理跨机 RTT、重度 Saga 占比需另做容量评估。

## Nest 普通业务与 heartbeat 结果

四组各 3×60s，共完成 720 万条普通消息、3960 万次 heartbeat，拒绝 0；每组固定输入、实际完成及逐 Entity 的 DAO 字段核验全部通过。4 个快 worker、GOMAXPROCS=4、队列 4096，未因某一档负载而调整生产配置。

| HB / 修改比例 | 普通消息完成/s | HB 完成/s | 修改 Entity 次数/s | 普通 / HB 最差 P99 ms | 普通 / HB 全量 >50ms 次数 |
| --- | ---: | ---: | ---: | --- | --- |
| 1Hz / 1% | 9999.7 | 9999.7 | 200.0 | 10.42 / 10.46 | 3 / 1 |
| 1Hz / 5% | 9999.5 | 9999.5 | 1000.0 | 10.40 / 10.44 | 0 / 0 |
| 10Hz / 1% | 9999.5 | 99994.9 | 1099.9 | 10.27 / 10.26 | 0 / 0 |
| 10Hz / 5% | 9999.1 | 99991.0 | 5499.5 | 11.35 / 11.33 | 0 / 0 |

实际修改路径单独全采样的最差 P99 为 11.40ms（消息）/11.34ms（HB）。1Hz/1% 第二轮有 3 条普通消息、1 次 HB 超过 50ms，最大 66.674ms /51.686ms；所有其他普通/HB 样本均未超过 50ms。该结果按预先声明的 Nest P99 门槛通过，不等同于全部请求严格小于 50ms。Sync 的最终业务验收采用 on_change P99≤50ms；逐条严格门禁的失败样本仍保留。

小规模 100 Entity、100 玩家、10Hz、100% 变化、2s 的功能冒烟和 `-race` 各通过一轮（各消息/HB 2000 次）；race 数据不参与性能统计。原始样本保留在 `nest/stable-nest-h{1,10}-d{1,5}/`。

## Saga 三轮 20 TPS 结果

| 路径 / 样本 | 完成数 | 实际完成 TPS | P50 ms | P95 ms | P99 ms | 最大 ms |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| forward / 1 | 1200 | 19.927 | 350.3 | 471.4 | 541.9 | 587.7 |
| forward / 2 | 1200 | 19.930 | 330.0 | 405.7 | 429.6 | 479.6 |
| forward / 3 | 1200 | 19.932 | 332.9 | 415.8 | 465.1 | 520.1 |
| compensate / 1 | 1200 | 15.388 | 8580.3 | 19316.4 | 19497.0 | 19969.9 |
| compensate / 2 | 1200 | 17.287 | 5864.8 | 10555.6 | 10683.3 | 10730.0 |
| compensate / 3 | 1200 | 14.810 | 7363.4 | 24172.5 | 26498.1 | 27154.7 |

六轮共 7200 个 Saga 均到达正确终态；StartErrors/WrongTerminal/PublishFailures/StoreFailures/WorkerFailures 均为 0，outbox 和租约均排空，业务字段与每步幂等效果逐文档核验通过。正向每笔 2 个 command，全补偿每笔 3 个 command（第二步业务拒绝也是一次正式消费/完成处理）。

**容量结论：正向 20/s 无持续积压；100% 补偿的 20/s 输入已饱和，不能列为稳定容量。** 第三轮全补偿只有 14.810 TPS、P99 26.498s，虽最终数据正确，仍是性能饱和样本。10/s 全补偿对照已完成，见下。没有修改生产 worker、批次、持久要求或超时以迎合该结果。

### Saga 10 TPS 全补偿对照

- 第 1 轮：600 个正确补偿，9.954 TPS，P99 601.195ms，最大 779.599ms。
- 第 2 轮：600 个正确补偿，9.954 TPS，P99 618.717ms，最大 660.566ms。

两轮各持续输入 60s，600/600 正确完成，StartErrors/WrongTerminal/PublishFailures/StoreFailures/WorkerFailures=0，outbox/lease=0，逐文档业务与幂等核验均通过。相对 20/s 全补偿积压样本，本档没有秒级累积尾延迟。复跑加 `ROOST_SAGA_PERF_MODE=compensate ROOST_SAGA_PERF_RATE=10`。这是已测可用档位，不宣称 Saga 精确极限就是 10 TPS，也不宣称正向 20 TPS 是最大值。

## Sync 验收口径复核

### handler 提交边界与接收计时

维护者本轮再次明确：`on_change` 指一次 Nest handler 完成后的全部 change 统一同步，setter 只标脏，不逐次发送。当前正式链路由 `SyncMutation.Admit` 在成功准入、Entity 锁释放前收集全部目标的脏字段并冻结视图；全部锁释放且完成提交确认后唤醒 Manager。网络发送在锁外。唤醒可以合并，因此业务提交批次不等于网络帧边界，不承诺每个 handler 恰好一帧；没有可交付变化不会因此制造状态更新。

Sync 夹具使用正式 Nest handler 与 `NestOptionWithEntitySync`。配置中的 20Hz 有两个不同含义：负载按每 50ms 窗口选择 1%/5% Entity 修改，并分成 10 批、约每 5ms 投递；Manager 的 50ms ticker 在 periodic 模式负责周期 Flush，在 on_change 模式只是兜底，收到提交唤醒就可以 Flush，无需等待下一周期。当前夹具每次 handler 修改一个 Entity 的多个字段，不是 setter 调用数对应发送数。

`scripts/perf/sync-aoi/nest.go` 在 handler 锁内修改字段后、标脏前记录 `Changed`，收集变化时记录 `Committed`；后者是夹具的收集时间，不等于通用 WAL/Remote 持久确认时间。`client.go` 的独立接收进程为每个连接读完整帧，完成帧和 Subject 解码、版本链及字段校验，更新客户端对象记录后才记录 `Received`。因此主指标 `Received−Changed` 包含 handler 剩余执行、提交/释放、Sync 调度/编码/排队、loopback TCP 和客户端该对象解码校验；`Received−Planned` 另外包含计划投递迟到。没有包含客户端渲染或公网 RTT。

固定周期可能将累积变化集中为编码、发送队列及带宽峰值；按 handler 完成时机触发有机会分散这些工作，但 Nest/HB 本身集中完成时仍会形成峰值。更频繁交付也可能减少跨 handler 的状态合并，使包数和总字节增加。当前没有细粒度带宽峰值或链路饱和证据，不能把 periodic 的较高延迟或 on_change 的偶发长尾直接归因为带宽。periodic 的周期等待本身已经占用最多约 50ms；两模式的 P99 差异不能作为“网络瓶颈已定位”的证明。

### 两模式的验收区别

初始方案把 periodic/on_change 两模式都列入“每条接收延迟严格≤50ms”的同一门禁。实际 periodic 1% 三轮严格失败，实际变化到接收 P99 分别为 55.959/56.168/57.351ms；该失败记录保留，**没有修改脚本阈值、删除样本或改变周期使门禁变绿**。

源码 `sync/entitysync/manager.go` 的 `Manager.run` 按 `Interval` ticker 串行 Flush；本档 interval=50ms。变化若错过本次捕获，就先等下一个周期，之后仍需捕获/编码/排队/接收。因此周期等待本身可耗尽 50ms 预算；首轮接收准入后 P99 只有 4.799ms，主要时延处于变化到准入阶段。CBM 查询和 coverage 已覆盖 manager/flush，脚本在索引排除范围，按当前源码直接补证；图中 `NewTicker` 的启发式误连不采信，以源码 `time.NewTicker` 为准。

维护者此前已将当前周期链路定位为“对信息同步延迟不敏感的类型”，并要求保留两模式、实时链路通过 Guard 提交后同步。periodic 报告真实的周期等待加处理时延，不承诺每条≤50ms；on_change 承担实时同步验证。没有修改生产周期，也不把 periodic 的原始严格失败称为通过。

2026-10-08 维护者在获知本轮 on_change 初测两轮分别有237/4446个实际变化样本超50ms、最大250.699/94.062ms，以及随后六轮零超标后，明确选择：**“P99≤50ms，保留并继续跟踪偶发长尾”。** 据此，本轮 on_change 的1%/5%全部普通初测与复测均满足实际业务验收（实际变化P99为2.047～5.375ms，计划到接收P99也低于50ms）。这个业务决定允许在其余门禁通过后发布，不表示最大延迟≤50ms或长尾已修复。脚本仍输出原有严格门禁，失败退出码、数量和原始数据全部保留；没有改测量实现来制造通过。

## Entity / Heartbeat 容量边界

以下上限属于本机、4 个快 worker、GOMAXPROCS=4、快队列4096、10ms固定计划投递、当前轻量业务与采样开销下的结果。不是 CPU 绝对极限，也不是任意 AI/寻路业务的承诺。所有拒绝和退化样本保留。

| 样本 | 秒 | 普通完成/s | HB完成/s | 拒绝数 | 消息/HB P99 ms | 判定 |
| --- | ---: | ---: | ---: | ---: | --- | --- |
| entity-rate-100 | 30 | 99985.5 | 0.0 | 0 | 10.33/0.00 | 通过 |
| entity-rate-200 | 30 | 199969.7 | 0.0 | 0 | 10.25/0.00 | 通过 |
| entity-refine-300 | 30 | 299952.1 | 0.0 | 1 | 10.79/0.00 | 未通过 |
| entity-rate-400 | 30 | 399667.7 | 0.0 | 7588 | 12.61/0.00 | 未通过 |
| entity-confirm-200 | 60 | 199972.8 | 0.0 | 731 | 10.46/0.00 | 未通过 |
| entity-long-100 | 120 | 99995.6 | 0.0 | 0 | 10.21/0.00 | 通过 |
| heartbeat-entities-10000 | 30 | 9998.8 | 99988.2 | 0 | 10.17/10.16 | 通过 |
| heartbeat-entities-20000 | 30 | 9998.1 | 199962.4 | 0 | 10.22/10.24 | 通过 |
| heartbeat-refine-30000 | 30 | 9995.9 | 299877.3 | 2163 | 10.76/10.76 | 未通过 |
| heartbeat-entities-40000 | 30 | 9980.2 | 399239.6 | 20875 | 23.68/26.15 | 未通过 |
| heartbeat-confirm-20000 | 60 | 9999.0 | 199982.5 | 221 | 10.34/10.35 | 未通过 |
| heartbeat-long-15000 | 120 | 9999.7 | 149994.9 | 0 | 10.23/10.25 | 通过 |

已验证较长档：每秒 100000 次 Entity 修改（每次两个正式 setter），以及 15000 Entity ×10Hz 加普通消息10000/s（两个流各5%修改）；两档各持续120s，零拒绝，完成计数及逐 Entity 字段核验通过。它们是可复现入口对应的已测档位，不能由有限样本推断长期绝对零拒绝。

200000 次修改/s、20000 Entity×10Hz 的30s样本曾通过，延长到60s分别出现731和221次队列拒绝，因此不列为稳定容量。其快队列峰值均达到4096；在200000/s输入下，队列只相当于约20ms的缓冲，一次较长调度/执行停顿就会耗尽。门禁同时检查拒绝、计数、数据和时延，不能只看仍约10ms的P99就宣布通过。没有通过放大队列、缩短样本或丢弃失败记录获得上限。

## Sync 首轮三次重复结果

下表保持原始严格 50ms 门禁。P99/max 均为毫秒；“计划”包含业务投递迟到，“变化”从实际锁内修改开始计算。

| 模式 / 变化 | 样本 | 变化接收样本数 | 计划 P99 / max | 变化 P99 / max | >50ms 计划 / 变化 |
| --- | --- | ---: | --- | --- | --- |
| periodic / 1% | 1 | 581274 | 60.307 / 78.261 | 55.959 / 73.540 | 111495 / 111309 |
| periodic / 1% | 2 | 581322 | 60.338 / 74.083 | 56.168 / 69.413 | 111045 / 110464 |
| periodic / 1% | 3 | 581401 | 60.604 / 74.509 | 57.351 / 70.119 | 110485 / 106184 |
| periodic / 5% | 1 | 2857897 | 67.185 / 124.549 | 63.042 / 116.490 | 861963 / 859182 |
| periodic / 5% | 2 | 2858017 | 67.773 / 144.270 | 63.369 / 136.777 | 854502 / 848825 |
| periodic / 5% | 3 | 2857994 | 67.425 / 131.443 | 63.006 / 123.240 | 847583 / 839939 |
| on-change / 1% | 1 | 644693 | 6.930 / 31.303 | 2.345 / 24.282 | 0 / 0 |
| on-change / 1% | 2 | 644143 | 8.994 / 335.638 | 3.532 / 250.699 | 3240 / 237 |
| on-change / 1% | 3 | 644701 | 5.917 / 45.056 | 2.047 / 30.639 | 0 / 0 |
| on-change / 5% | 1 | 3401505 | 8.439 / 134.941 | 5.375 / 94.062 | 10408 / 4446 |
| on-change / 5% | 2 | 3404183 | 6.395 / 32.737 | 3.135 / 25.841 | 0 / 0 |
| on-change / 5% | 3 | 3404551 | 5.982 / 21.044 | 3.086 / 18.080 | 0 / 0 |

12个样本均无运行期 Flush 失败、异步发送错误或会话丢失，发送/接收帧数与字节一致；独立TCP接收进程完成解码和最终可见集合逐项核验。停机取消可能使累计 FlushFailures/LastError 留下一次 context canceled，按已有夹具单列，不混入运行期失败计数。

on_change 首轮不是全部通过：1%第二轮在tick939出现340.207ms业务投递停顿，237个实际变化样本超50ms，最大250.699ms；该轮总GC STW仅4.570ms，单靠STW不能解释停顿，但仍不能据此把原因直接归为宿主机。5%第一轮有4446个实际变化样本超50ms，最大94.062ms。两档各另外两轮严格通过。失败样本不重写为通过；独立Go执行轨迹和重复验证另列。

复测前宿主机快照：10物理/逻辑CPU，swap已用4149MiB；同时存在桌面、IDE与其他常驻进程。该快照不覆盖尖峰发生时刻，不能作为“已经证明是环境原因”的证据。

## 独立复测与执行轨迹

初测结束后，串行运行 Go 执行轨迹诊断，再运行不带 trace/profile 的 on_change 1%/5%各3×60s独立样本；期间没有其他模块编译或CBM查询。六个普通复测样本都通过脚本原有严格50ms门禁、发送/接收及最终可见集合核验。1%实际变化P99=2.490/2.336/2.590ms，5%实际变化P99=4.052/3.167/3.076ms；复测不撤销初测失败，也不是根因已修复的证明。

新增 `scripts/perf/sync-aoi.sh -runtime-trace=true` 只作用于性能工具，默认关闭，不改变生产逻辑或门禁。它记录服务器 Go 执行轨迹和墙钟锚点，用于与客户端 outlier 对齐；与 `-profile=true` 一样，只用于独立诊断，不能混入普通性能统计。

诊断样本 `stable-sync-runtime-trace`（5%、60s）也捕获到超标：3403511个变化接收样本，实际变化P99=3.573ms、最大64.523ms，901个>50ms；计划时延超标1718个。原始轨迹116MiB及离线窗口分析均保存于产物目录。18:47:41附近，Go轨迹显示39.910ms并发GC标记区间，与AOI evaluate/evict及请求等待重叠；该窗口STW为0.294/0.089ms，整个轨迹最大STW约0.294ms。Producer曾等待Nest结果30.573ms，某发送协程Runnable等待16.117ms；Manager在AOI调用栈中有28.714ms的Running状态区间。Running区间包含线程被宿主调度的可能，不能当成CPU独占时间；并发GC区间也不能当成全进程停顿。

这些证据支持“GC并发工作、AOI处理和调度积压叠加”的诊断，不能唯一解释没有轨迹的初始250.699ms峰值。没有确认到数据正确性缺陷，也没有把宿主机归因为已证明事实。后续如继续优化，应对AOI工作量/分配与队列调度做同配置前后对照，不扩大队列、调整采样或改50ms阈值掩盖尾延迟。

## 独立 CPU / 分配 profile

`stable-nest-profile`（10Hz、5%、30s）与 `stable-sync-profile`（on_change、5%、10s）均独立运行并保留 CPU/alloc profile，未混入普通样本。macOS 的 CPU 栈里系统等待/唤醒占比很高，不能直接把 `usleep` 当业务 CPU 瓶颈。分配热点更明确：Nest 为 NewRollbackTx、fctx.Context.Set、调度准入和夹具自己的计时闭包；Sync 为可靠发送数据复制、帧编码、fixture异步包装、map clone与订阅生命周期。alloc_space 是进程累计分配，包含初始化，不作为纯稳态每秒分配率。

这些是后续优化候选，不是本轮已修改的生产逻辑：优先确认低变化回调是否可以减少每次事务/上下文分配，以及AOI/发送缓冲生命周期是否能安全复用；任何改动须保持Guard回滚、视图冻结与交付所有权，做同配置前后对照。当前不为得到绿色门禁加特殊分支。

Remote独立CPU诊断`stable-cpu-80-eac89694`完成4800笔，测量60.087秒、79.885 TPS，错误/丢弃0，P99=183ms、最大256ms，最终全量核验通过。CPU profile覆盖初始化和测试全过程121.75秒，不能用其总采样时间除以60秒当成稳态CPU利用率；主要栈是系统调用、网络事件和线程唤醒，另有Manager包装器回收、map迭代与GC。正常1h吞吐/时延以无CPU profile的长稳样本为准。

## 最终结果

Remote、Nest、Entity/HB容量、Saga以及Sync初测/复测已有具名数据。on_change 满足维护者本轮明确的P99≤50ms业务指标；逐条严格门禁初测失败仍保留，独立复测不覆盖或撤销它们。Saga正向20/s和全补偿10/s是已测档位，全补偿20/s饱和不列为稳定容量。旧240秒约60.1 TPS、4520错误的失败样本继续保留于[历史记录](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/OUTBOX-PARTIAL-LOAD-2026-10-08.md)，不改写为当前结果。

候选`e6728981`的pretag已通过（生成无漂移、build/vet/tidy、全仓测试），根包`go test -count=1 .`通过，Linux/arm64交叉构建通过。首轮pretag发现4处指向已删除DAO迁移源码的历史文档链接，已补能力撤销说明并改为历史路径记载；未修改门禁或增加豁免。首轮失败完整日志和修后日志均保留。

私有环境故障矩阵`stable-matrix-v1-24-0`为21/21通过，无跳过或空匹配：四种Mongo/NATS故障×三种提交策略12格，以及进程租约、Redis集群/未复制fence、持久权威、所有权计数、Mongo/WAL恢复、broker故障/网络及最终健康9格。执行后私有依赖已停止，结果和profile已转存持久证据目录。该矩阵是本机多进程真实资源故障验证，不冒充物理跨机运行。

v1.24.0 annotated tag已推送，指向`2fa1c7877b14c77b52e062bedcb3455cef8db0fb`。从远端真实tag生成game-demo，`GOWORK=off GOPROXY=direct`、无replace，模块解析为v1.24.0；66个包build/vet/test通过，其中19个包执行测试，其余无测试文件。生成、模块版本及build/vet/test日志完整保存。未等待GitHub CI，未部署。

CBM已在main刷新，generation `2026-10-08T11:31:15Z`，31930节点/254670边、skipped=0，5个历史模板部分解析。新增Saga性能测试与版本配置coverage无记录缺口；5个本轮Go路径位于脚本/testdata排除范围，已经当前源码补证。后续只回填文档，不改变已索引的业务代码。偶发长尾继续保留为后续profile/AOI与调度优化的观测入口，不把本轮业务验收通过写成根因关闭。
