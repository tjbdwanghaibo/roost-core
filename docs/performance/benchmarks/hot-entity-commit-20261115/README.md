# 热点单例实体的写吞吐、M-15 分散到达与登录潮遗留（2026-11-15）

对应 KNOWN-LIMITS 2026-11-15 新增的三条：“热点单例实体的写吞吐”、“游戏尺度与千人”的 M-15 分散失败、“login.notices
溢出丢弃没有指标”与“登录潮里 30 次 account CreateRole 超时”。设计与契约见
[NEST_PIPELINED_COMMIT §12](../../../framework/NEST_PIPELINED_COMMIT.md#12-热点单例实体的写吞吐2026-11-15)。

本机：macOS arm64（10 核，APFS）。同一时段机器上还有其他构建 / 测试在跑，每轮都记了 `sysctl vm.loadavg`（一分钟值），
负载在下面各表里列出；单轮结果不作容量结论。M-15 原始输出（机器人报告、各进程日志、前后指标）不入库，本目录只放摘要。

## 一、微基准：一个实体上的同步请求

`framework/nestwal/hot_entity_bench_test.go` 的 `BenchmarkHotEntityRequest`：真实 Nest（8 个 short worker）+ 正式
`engine.Projector` + 真实 WAL（默认组提交参数、真实 fsync），32 个闭环客户端同时 `Request` 同一个实体；
`async_under_strict_load` 另有 4 个客户端在别的实体上持续做 strict 提交。

前后对照只针对 B（nestwal 的 fsync 与写入解耦）：两侧各 `go test -c`，before 只把 `framework/nestwal` 的 `wal.go`
（与依赖它内部字段的两个测试文件）换回基线，两侧都已有 A 的声明；6 轮交替运行，`benchstat`：
[hot-entity-before.txt](hot-entity-before.txt)、[hot-entity-after.txt](hot-entity-after.txt)、
[hot-entity-benchstat.txt](hot-entity-benchstat.txt)，负载 [hot-entity-load.txt](hot-entity-load.txt)（3.5～4.3）。

| 变体 | before ops/s | after ops/s | 变化 | after p50 / p99 |
| --- | --- | --- | --- | --- |
| strict | 120.7 | 118.5 | ~（p=0.24） | 270ms / 288ms |
| pipelined | 119.2 | 118.2 | ~（p=0.70） | 271ms / 285ms |
| pipelined + `completion=async`（A） | 3.11k | 2.92k | ~（p=0.31） | 11.6ms / 18.1ms |
| async | 221.7 | 323.9 | +46%（p=0.002） | 97ms / 122ms |
| async，另有 strict 负载 | 121.2 | 284.4 | +135%（p=0.002） | 113ms / 129ms |

读法：

- A 的效果是 pipelined 一行对 pipelined + `completion=async` 一行：同一实体约 120 → 约 3000 次 / 秒（约 25 倍），每条
  回复仍在持久之后。业务 handler 在 A 之前不能选用异步完成，只能是 pipelined 一行。
- strict 与不声明的 pipelined 不变：同 ID 准入链要等回复，每次 fsync 一条，是契约本身的代价。
- B 让 async 记录不再排在 strict 的 fsync 后面；macOS 上 `F_FULLFSYNC` 进行中同文件的 `write` 仍在内核里等，所以 async
  仍比没有 fsync 时慢（见 §12 平台说明）。Linux 未在本机测量。

先红后绿与变异（原文）：

- [red-declared-async.txt](red-declared-async.txt)：`TestDeclaredAsyncCompletionLetsTheNextRequestOnTheEntityStart` 在 A 之前
  两条子测试都超时（第一条未持久时第二条没开始）；[mutations-a.txt](mutations-a.txt)：去掉声明解析（M1）、实例注册
  不建完成泵（M2）各变红。
- [red-nestwal.txt](red-nestwal.txt)：B 之前 `TestAsyncAppendDoesNotWaitForAnOngoingSegmentSync`、
  `TestSyncOnlyCoversWritesFinishedBeforeItStarted` 变红；[mutations-b.txt](mutations-b.txt)：按 fsync 结束时的写入位置
  发布（MB1）、strict 写完即回复（MB2）各变红。

## 二、M-15 登录潮：Demo 临时改回 `Sync_RecordEnter`

条件：`s11-cellworld-robot`（1000 人、爬坡每 100ms 20 人、进入点分散 1400），用
`ROOST_SEAMLESS_PROJECT_HOOK` 在生成工程里把 EnterGame 的 World 登录计数改回同步等待（`Sync_RecordEnter`，修复前的
写法），handler 分别保持 strict（对照）与改成 `durability=pipelined` + `AsyncCompletion`。只改生成的工程，不提交。

| 指标 | strict A | strict B | 异步完成 A | 异步完成 B |
| --- | --- | --- | --- | --- |
| 负载（开始 → 结束） | 12.5 → 19.5 | 19.5 → 21.2 | 12.4 → 17.4 | 17.4 → 21.3 |
| World `handlerRecordEnter` 平均锁持有 | 13.3ms（1276 次，含重试） | 12.2ms（1143 次） | 0.021ms（1000 次） | 0.029ms（1000 次） |
| 进入路径有失败的玩家（`enter-timing.py`） | 653 | 540 | 0 | 0 |
| Base 请求超时（`player_tcp_dispatch_timeout_total`） | 1555 | 1032 | 0 | 0 |
| 握手 → 进入完成（成功者）p50 / p99 | 2.05s / 3.14s | 2.12s / 3.01s | 0.14s / 0.92s | 0.19s / 0.87s |
| 机器人失败 | 40 | 29 | 0 | 7 |

读法：

- 同一份 demo、同一个同步等待写法，只差 handler 声明：strict 时 World 每次 fsync 记一人，排队把后来的玩家推过 3 秒期限
  （与[登录潮记录](../login-burst-20261115/README.md) §一 的基线同一特征）；声明 `completion=async` 后锁持有降到几十微秒，
  进入路径不再有失败，计数在回复前仍已持久。
- strict 两轮的机器人失败数不大，是因为第二轮决定的 `login_timeout` 退避重试把大部分玩家补进来了；失败里多数是“预热期
  没看见自己”（重试进来太晚）。异步完成 B 的 7 个失败是分散到达来回摆（这两轮跑在 §三 的到达控制修改之前）。
- 脚本：钩子 [hook-sync-strict.txt](hook-sync-strict.txt) / [hook-sync-async.txt](hook-sync-async.txt)（作为
  `ROOST_SEAMLESS_PROJECT_HOOK` 传入），摘要 [m15-summarize.py](m15-summarize.py)。
- demo 本身仍用 `Send_RecordEnter`（登录不看计数结果，不必等）；要等持久结果的热点写才需要 `completion=async`。

## 三、M-15 分散失败：机器人到达控制在回传延迟下来回摆

条件：`s11-cellworld-robot` 默认参数（进入点分散 1400、预热 30 秒、判定框 ±20、阈值与爬坡不变）。KNOWN-LIMITS 记的此前三轮
是 240 / 21 / 417。

**根因**：机器人每 100ms 按回传的位置连续调速度（速度 = 2/秒 × 位移）。回传位置在负载下延迟 0.6 秒以上（同步帧间隔、
Base 转发），意图生效还有抖动；这个比例控制在这样的延迟下不稳定，会在目标附近持续来回摆，预热结束那一刻正好在框外就
判失败。证据（先加了失败诊断 `spreadTrace`，判定不变）：

- 未到达的机器人里，能看见自己的几乎全部“进过框又出去”：spread-b 25 个失败的进框次数 1～11 次，异步完成 B 的 7 个是
  5～8 次；离散仿真（回传延迟 d 拍、意图再过 1 拍生效）里增益 2/秒在约 0.6～0.7 秒延迟时开始失稳。
- spread-c（负载 18 → 28）850 个失败里 477 个从没进过框：同步帧停更约 2.7 秒、能看见自己的拍数约七成，是整机过载下
  Base 同步跟不上（日志积压 33.8 万），不是到达控制的问题；这一轮不作容量结论。

**修复**（demo `cmd/loadtest/cellworld.go` 的 `spreadSteer`，判定、阈值、爬坡都不变）：改成“走一段 → 停下 → 等回传位置
停稳 → 按停稳的位置走下一段”；一段按墙钟计时，近处放慢（至少走 1 秒），停稳要求回传位置落在这一段预期终点附近、或已离开
出发点并 1 秒不变。回归 `TestSpreadSteerSettlesUnderPositionEchoDelay`（按 10ms 积分模拟回传延迟 100～1000ms、意图生效
抖动 0～600ms、拍子迟到 0～200ms，要求最后 5 秒回传位置一直在框里）；旧控制、只按拍数计的第一版与四个变异中的三个变红，
原文 [mutations-spread2.txt](mutations-spread2.txt)。

| 轮次 | 到达控制 | 负载（开始 → 结束） | 机器人失败 |
| --- | --- | --- | --- |
| spread-a / b / c | 旧（比例控制） | 2.6→16.6 / 16.6→18.1 / 18.1→28.4 | 0 / 25 / 850 |
| steer-a / b / c | 分段（按拍计时） | 21.2→22.9 / 22.9→19.1 / 19.1→24.8 | 0 / 0 / 13 |
| steer2-a / b / c | 分段（按墙钟计时，最终版） | 3.8→19.7 / 18.1→14.8 / 14.8→16.1 | 0 / 0 / 0 |

steer-c 剩下的 13 个：负载下机器人拍子丢约一成，按拍数计的一段实际走得更久、越过目标上百，停下后又要等 5 秒超时才重走；
按墙钟计时的最终版针对的就是这一点。最终版三轮全部通过（负载不低，但三轮中位数也不作容量结论）。

**同一出生点**（`ROOST_SEAMLESS_SPAWN_SPREAD=0`，最终版到达控制）：

| 轮次 | 负载 | 机器人失败 | Base 兴趣积压（结束时） | 预算停下 | 同步会话丢失 |
| --- | --- | --- | --- | --- | --- |
| same-a | 15.5 → 16.3 | 0 | 0 | 149 | 23 |
| same-b | 15.1 → 19.5 | 0 | 0 | 151 | 13 |

此前同一出生点的 5 轮里有 3 轮（`lb-fix3`、`lb-fix4`、`lb-r2-nospread`）在预热期出现 Base 兴趣饱和（积压 37～50 万、会话
丢失 222～755），2 轮没有。这两轮都没有出现，但 2 轮不足以说明饱和与旧到达控制有关，触发条件仍**尚未确认**；同点千人的
Base 兴趣 / 同步容量按维护者决定另起一批。

全部 17 轮的逐轮摘要（负载、进入时序、失败分类、Base 指标）：[m15-runs.txt](m15-runs.txt)。其中 steer2 首次排队时与机器上
另一个 agent 的演练撞了端口（account 绑定失败、全员 no responders），作废重跑，不计入。

## 四、login.notices 溢出计数

登录模块 `login_active.go` 在会话激活告知溢出（超过 256 条在途）与停止后丢弃时记 `login.notice_dropped_total`
（`reason=busy` / `stopping`）。修前回归变红原文：[red-login-notice-dropped.txt](red-login-notice-dropped.txt)。

## 五、30 次 account CreateRole 超时（lb-fix2）

lb-fix2 那一轮有 30 个机器人在 account CreateRole（经 NATS，3 秒期限）超时，只出现在 chat 写锁修复之前。要确认的推测是
“chat 世界频道抢 compare-and-set 拖慢共用的 Redis”。

**1. 单独探针**（[redis-storm-probe.go.txt](redis-storm-probe.go.txt)，复制进 `service/chat/` 对私有 redis-server 跑；
结果 [redis-storm-probe.txt](redis-storm-probe.txt)）：约 70KB 的世界频道状态，同一时刻到达的公告各用一个 Store 抢写
（修复前）时，同一 Redis 上无关的小 GET p50 24ms、p99 46ms、最大 57ms，Redis 约占满一个核；排队写（修复后）时 p99
0.6ms。按 5ms 间隔到达时两种写法都没有可见影响。

**2. M-15 复现**（[run-repro.sh.txt](run-repro.sh.txt) + [hook-announce.py](hook-announce.py)，路径是当时的临时目录：临时去掉 `service/chat/store.go` 的进程内写锁、每次登录同步发一条公告，跑完还原；
`redis-sampler.py` 每 50ms PING、每秒 INFO）：

| 轮次 | 负载 | 公告失败 | CreateRole 超时 | Redis PING（风暴期间） | Redis CPU |
| --- | --- | --- | --- | --- | --- |
| repro-a | 24.8 → 19.0 | 765 | 0 | p50 24～27ms，最大 145ms | ≤ 0.27 核 |
| repro-b | 10.4 → 21.9 | 669 | 53（机器人 821～914，同一秒内） | 同上量级，最大约 180ms | ≤ 0.30 核 |

- 现象能在“抢写风暴”条件下复现（两轮中一轮），超时的都是最后到达的约 100 个机器人；没有风暴的各轮（本目录 10 轮、登录潮
  记录里 lb-fix2 之外的 11 轮）都没有出现。所以它与风暴相关，而风暴的条件已被两处修复去掉（chat 进程内写锁、公告每秒至多一条）。
- 但“Redis 被拖慢到 3 秒”不成立：风暴期间 Redis 自身 CPU 不到三成个核，PING 最长约 0.18 秒；同一机器人在前一步
  Login（同样经 NATS、读写 Redis）没有超时。机器当时负载约 20（10 核），PING 的延迟里有多少是调度延迟分不清。
- 还没排除的去处：account 的 Redis 连接池（`redis.pool_size` 未配置时为 10，CreateRole 一次约十几次 Redis 往返，单次往返
  慢到几十毫秒时吞吐会降到登录到达率以下）、NATS 在风暴里的排队、整机 CPU 争用。account 在这套演练里没有采指标的
  端点，这一步需要给 account 加采样（RPC 处理时长、连接池等待）再复现，**尚未确认**。
