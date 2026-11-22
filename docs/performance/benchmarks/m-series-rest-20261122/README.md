# M 系列剩余 12 项（压缩口径，2026-11-22）

计划与原始命令见 [remaining §3](../../../maintenance/evidence/known-limits-remaining-20261114/remaining.md#3-m空闲机器上的运行序列)。维护者 11-22 要求把未运行的 M-3、M-4、M-6、M-7、M-8、M-10～M-14、M-16、M-17 全部跑完，并定了压缩口径：每轮（含环境起停）约 10 分钟、每档 1 个样本、缩短时长、全部串行。**下面每项写的是实际用的参数，不是计划口径**；单样本数字只作同机同时段对照，不作容量保证。

## 环境与前提

- 源码：main `50896601`（v1.60.0 之后一个文档提交），在 scratchpad 的干净 detached worktree 里跑（主检出不动）；M-8 另开一个只改 `SyncApplyMaxExtraViews` 为 16 的诊断 worktree（diff 原文 `m8/diag-build.diff`，不入库，用完已删）。
- Go 1.27.3；macOS 26 arm64，Apple M5 10 核，32 GiB。统一 `GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0`（进程内 10）；nest-game 用 `ROOST_PERF_CPU=0`。
- 全部串行：同一时刻只跑一个测量，不并行编译。同机常驻 CorpLink、Meshy.app 的 agent 进程、`~/.roost-it` 共享隔离环境（Mongo / NATS / Redis / toxiproxy）、brew 的 mongod / nats / redis / etcd 服务；本轮未动它们。
- **swap 全程已用 7.7～8.0 / 9.0 GiB**（开跑前就是这样，`memory_pressure` 空闲 78～82%），记作环境事实。`df -h /` 剩 297～299 GiB；go build cache 107 GiB（未清）。
- 每段开始前的负载（1 / 5 / 15 分钟）见下表；记录时刻紧接上一段结束，1 分钟值含上一段尾巴。原文 `preflight.txt`。

| 段 | 时刻 | loadavg | swap 已用 |
| --- | --- | --- | --- |
| M-3 batch / dispatch | 07:40 / 07:43 | 2.38 2.61 2.40 / 4.41 3.85 3.00 | 7966 M |
| M-4 periodic / on_change | 07:47 / 07:48 | 3.91 4.32 3.41 / 3.42 4.03 3.40 | 7950～7966 M |
| M-8 sync / async | 07:50 / 07:51 | 4.26 4.56 3.72 / 7.71 5.38 4.03 | 7950 M |
| M-14 demo / game | 07:51 / 07:53 | 4.31 4.80 3.89 / 10.03 6.32 4.53 | 7942～7950 M |
| M-7（120s 作废档 / 80s 有效档） | 07:54～08:00 | 3.97～9.45（1 分钟） | 7942 M |
| M-17 四档 | 08:02～08:08 | 5.47～8.85（1 分钟） | 7758～7942 M |
| M-6 | 08:11 | 3.59 5.14 5.36 | 7750 M |
| M-16（1000 人两档 / 400 人两档） | 08:16 / 08:21～08:25 | 5.54～5.89 / 2.27～4.55 | 7726～7734 M |
| M-10 | 08:30 | 3.37 4.66 4.87 | 7726 M |
| M-13 单机 / Cluster | 08:31 / 08:33 | 2.58 4.33 4.74 / 2.83 3.86 4.49 | 7718～7726 M |
| M-11 w16 / w1 | 08:34 / — | 3.37 3.76 4.39 | 7718 M |
| M-12 100 / 200（脚本在编译后另记 `host-before.txt`） | 08:39 / 08:42 | 7.12 7.55 6.07 / 2.96 5.71 5.58（编译后：4.65 / 1.91） | 7718 M |

原始输出都在检出目录 `artifacts/perf/m-series-rest-20261122/<项>/`（不入库），下文路径都相对这个目录。

## 总览

| 项 | 状态 | 一句话 |
| --- | --- | --- |
| M-3 | 已测（各 1 次 3 min） | batch / dispatch 投递业务延迟同量级；dispatch 分配多 18%、短池作业数 6.7 倍 |
| M-4 | 已测（各 1 次） | periodic / on_change 五个场景差异都在单样本噪声内 |
| M-6 | 已测（1 次 4 min） | 独立 Gate + self_sync 1000 人通过，计数 / 值核对一致 |
| M-7 | 已测（80s，计划 120s 撞空闲超时作废） | 健康 1000 + 停读 50：驻留峰值 35 / 101 MiB，预算拒绝 0，停读 50 条全被写期限关掉 |
| M-8 | 已测（各 1 次 200 tick） | ApplySync 16 视野：200 tick 中 46 tick 有日志积压（p99 110 条），政策阶段 p95 31.7ms 超过半周期 |
| M-10 | 已测（1 次） | 256 kB/s 追平并回到实时；64 kB/s 被 Gate 以 `send_budget` 期限断开，`player_tcp_write_deadline_total{deadline="send_budget"}`=1 |
| M-11 | 已测（各 1 轮） | effect workers 16 / 1 吞吐差 ≤4%，retryable / unknown 都是 0 |
| M-12 | 已测（同秒 100 / 200 各 1 次，不做爬坡） | 100 人 96 / 100、200 人 182 / 200；World 热点 `nest: sync timeout` 0，失败全在 saga 状态轮询 |
| M-13 | 已测（count 3） | 账本满 1000 条：发布 / 撤回每次约 1.1～1.3ms（私有单机 / Cluster），重载 0.44～0.46ms；计划的 publish 2000x 撞夹具上限 |
| M-14 | 已测（各 1 次 60s） | demo 尺度达标；game 尺度每格 region beat p50 96～116ms（20Hz 周期 50ms），ghost 延迟 p99 约 150ms |
| M-16 | **未完成** | 1000 人集中布局经 Gate 两档开场即失败（客户端读期限）；改 400 人：c0 通过，c4 收尾不收敛（期望 376 > 会话上限 100） |
| M-17 | 已测（各 120s） | strict 50 连接约 890 次/s；pipelined 同一实体 104 次/s、每次一条 fsync |

## M-3（No.9）nest-game 心跳投递 batch / dispatch

命令（每种 1 次，计划 5min×4 交替改为 3min×2）：

```sh
ROOST_PERF_LABEL=m3-hbdel-$d ROOST_PERF_COUNT=1 ROOST_PERF_CPU=0 ROOST_NEST_GAME_HEARTBEAT_DRIVER=heartbeat \
  ROOST_NEST_GAME_HEARTBEAT_DELIVERY=$d ROOST_NEST_GAME_DIRTY=5 ROOST_NEST_GAME_DURATION=3m ROOST_NEST_GAME_ENTITIES=10000 \
  ROOST_NEST_GAME_EMBEDDED=1 ROOST_NEST_GAME_HZ=10 ROOST_NEST_GAME_IO_WORKERS=8 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 \
  ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_SHORT_QUEUE=65536 ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_SYNC=1 \
  bash scripts/perf/nest-game.sh        # d = batch，然后 dispatch
```

| 指标 | batch | dispatch |
| --- | --- | --- |
| 业务消息完成 / 计划 | 1,800,000 / 1,800,000，拒绝 0 | 同左 |
| 业务完成 p50 / p99 / max（ms） | 5.67 / 12.09 / 44.7 | 5.50 / 11.83 / 19.5 |
| 服务端 p99 / max（ms） | 2.13 / 31.0 | 1.18 / 9.1 |
| 心跳完成 / 计划，每 Unit 拍数 | 17,998,033，1799～1800（应 1800），跳拍 0 | 17,995,048，1799～1800，跳拍 0 |
| 心跳改值完成 p99 / max（ms） | 0.14 / 8.1 | 0.08 / 8.0 |
| 分配 / 次数 / GC 次数 / GC 停顿 | 75.6 GB / 0.98e9 / 473 / 109ms | 89.6 GB / 1.36e9 / 557 / 127ms |
| 短池开始的作业数 / 峰值等待 | 2,969,763 / 206 | 19,795,048 / 1261 |
| 嵌入 TCP 回路 p99 | 2.61ms | 1.51ms |

结论：两种投递都零跳拍、零拒绝，业务 p99 同量级（12.1 对 11.8ms）。dispatch 每拍一个作业，短池作业数是 batch 的 6.7 倍，分配多 18%、分配次数多 38%；batch 这一次的服务端尾部（max 31ms）高于 dispatch（9.1ms），但两次开跑负载不同（2.4 对 4.4）、单样本，**尾部差异不下结论**。本项没有 CPU 占用数据（报告里没有）。Linux 与历史 0.2s 停顿仍是 C-L1。原始：`m3/m3-hbdel-{batch,dispatch}/sample-1.{json,log}`、`m3/m3-summary.txt`。

## M-4（No.21）Sync 类型调度 periodic / on_change

命令（每种 1 次，计划 2 次交替改 1 次；每次约 50s，夹具默认 200 tick、20Hz）：`ROOST_PERF_LABEL=m4-sched-$s ROOST_PERF_COUNT=1 ROOST_SYNC_SCHEDULE=$s bash scripts/perf/sync-business.sh`。

| 场景 | 调度 | 计划→客户端 p50 / p99 / max（ms） | 服务端完成 p99 | flush p99 | tick 工作 p99 | 帧数 |
| --- | --- | --- | --- | --- | --- | --- |
| solo | periodic / on_change | 2.31/2.90/4.10 ／ 2.07/2.78/3.11 | 1.05 ／ 0.99 | 0.27 ／ 0.20 | 1.03 ／ 0.96 | 200 |
| hotspot32_sparse | periodic / on_change | 2.96/4.42/5.25 ／ 3.09/4.28/5.12 | 2.92 ／ 2.47 | 1.60 ／ 1.74 | 2.97 ／ 2.70 | 6400 |
| hotspot96_sparse | periodic / on_change | 6.34/9.92/12.53 ／ 6.38/10.52/12.57 | 9.01 ／ 8.80 | 8.31 ／ 7.81 | 9.66 ／ 9.46 | 19200 |
| hotspot96_busy | periodic / on_change | 12.57/16.70/19.98 ／ 12.79/17.00/20.76 | 14.63 ／ 15.11 | 7.15 ／ 7.24 | 14.86 ／ 15.68 | 19200 |
| spread128_busy | periodic / on_change | 12.07/14.75/17.27 ／ 12.56/15.07/16.41 | 12.81 ／ 13.36 | 4.20 ／ 3.64 | 13.23 ／ 13.70 | 25600 |

两种调度帧数相同、`deadline_misses` 都是 0、同步字节相同。结论：生产 on_change 接线与历史 periodic 在五个场景里差异都在单样本噪声内（最大 p99 差 0.6ms），**本次没有看到退步**；只有 1 个样本，不能据此判定更细的差异。原始：`m4/m4-sched-{periodic,on_change}/sample-1/*.json`、`m4/m4-summary.txt`。

## M-6（No.23、98）独立 Gate + self_sync 重档

命令（1 次，计划 15min 改 4min）：`ROOST_PERF_LABEL=m6-gate-selfsync ROOST_PERF_COUNT=1 ROOST_PERF_CPU=0 ROOST_NEST_GAME_GATE=1 ROOST_NEST_GAME_SELF_SYNC=1 ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_IO_WORKERS=8 ROOST_NEST_GAME_SHORT_QUEUE=65536 ROOST_NEST_GAME_DURATION=4m bash scripts/perf/nest-game.sh`，NATS 用 `~/.roost-it` 共享隔离环境（夹具每次用唯一 Gate 命名空间 `roost.gate.load.n<纳秒>`）。

```
heartbeat_driver=ticker messages=9999.9/s heartbeat=9999.9/s changes=200.0/s p99=20.060/10.690ms server_p99=11.800/0.830ms counts=true values=true latency=true
--- PASS: TestNestGameLoad (240.42s)
```

- 业务 2,400,000 / 2,400,000，拒绝 0；业务完成 p50 / p99 / max 6.69 / 20.06 / 433.8ms，服务端 p99 11.8ms；Gate 往返（采样 150,000）p50 / p99 / max 1.91 / 16.95 / 279.7ms。
- self_sync（`self:on_change`）：帧 27,400，值核对通过；计划→客户端 p50 / p99 / max 8.01 / 27.92 / 388.5ms，提交→客户端 p99 7.8ms；在线 1000。
- 短池峰值等待 657、最长等 worker 65.4ms；GC 1559 次、停顿合计 269ms。

结论：独立 Gate 叠 self_sync 在 1000 人、1 万实体、每人 10 条/秒下 4 分钟内计数与值一致、无拒绝；尾部（max 0.3～0.4s）偶发，原因没有拆。只跑了 4 分钟一次，不代表 15 分钟稳态。原始：`m6/m6-gate-selfsync/sample-1.{json,log}`。

## M-7（No.24）出站驻留预算：健康 1000 + 停读 50

命令（rate 20 与 100 各 1 次）：`ROOST_TCP_BUDGET_DRILL=1 DRILL_HEALTHY=1000 DRILL_STALLED=50 DRILL_FRAME_BYTES=4096 DRILL_RATE=$r DRILL_SECONDS=80 DRILL_OUT=... go test -run TestOutboundBudgetDrill -count=1 -timeout 10m -v ./infra/network/gateway/`。

**计划的 `DRILL_SECONDS=120` 两档都失败，作废**：`healthy player 1 lost its connection during the drill`。原因是夹具：健康客户端只读不发，120 秒超过 TCP 缺省 `IdleTimeout` 90s，被服务端按空闲关掉（健康帧数 1,799,676 ≈ 1000×20×90，正好停在 90 秒）。不是预算问题；改用 80s 重跑（原文 `m7/m7-rate*-120s.out`、`m7/m7-drill-120s-idle.jsonl`）。夹具若要跑满 120s 需要客户端发心跳，本轮未改代码。

80s 有效档（原文 `m7/m7-drill.jsonl`）：

| rate（轮/秒） | 健康帧收到 / 应收 | 驻留峰值 | 预算拒绝 | 溢出关闭 | 写期限关闭 | 推给已关 / 不存在 |
| --- | --- | --- | --- | --- | --- | --- |
| 20 | 1,599,091 / 1,600,000 | 36,954,544 B（35.2 MiB，预算 256 MiB 的 14%） | 0 | 0 | 50（`write_timeout` 5s） | 63,450 |
| 100 | 7,999,547 / 8,000,000 | 105,415,232 B（100.5 MiB，39%） | 0 | 0 | 50 | 366,837 |

结论：每帧 4 KiB、50 条停读连接时，缺省 256 MiB 预算在 20 / 100 轮每秒下都没被占满，停读连接全部在 5s 写期限到期后关闭，健康连接收帧缺口 < 0.06%（推测是结束时在途，未核对）。100 轮/秒时峰值已到预算的 39%；更大帧、更多停读连接或更长写期限下是否够用没有测。

## M-8（No.25）Interest ApplySync 16 视野诊断构建

命令（诊断构建：只把 `framework/sync/entitysync/policy/interest.go` 的 `SyncApplyMaxExtraViews` 从 8 改为 16；sync / async 各 1 次，用同一份诊断源码）：`ROOST_PERF_LABEL=m8-v16-$apply ROOST_PERF_COUNT=1 bash scripts/perf/sync-aoi.sh -players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50 -views-per-player=16 -apply-mode=$apply`（200 测量 tick，约 10s）。两次都以 `50ms latency gate failed; reports retained` 退出 1——这是夹具的 50ms 门槛，16 视野本就超（历史 P99 126ms），报告照常保留。

| 指标 | ApplySync | ApplyAsync |
| --- | --- | --- |
| 帧数（满额 199,000） | 196,386 | 199,000 |
| 改值→客户端 p50 / p99 / max（ms） | 51.8 / 120.7 / 150.4 | 52.8 / 116.5 / 156.2 |
| 计划→客户端 p99（ms） | 124.4 | 123.5 |
| `log_backlog_per_tick`（server.json） | 200 个样本中 46 个非零；p50 0、p95 71、p99 110、max 120 条，均值 10.3 | 不适用（只有同步模式有这项） |
| 政策阶段 `policy_duration` p50 / p95 / p99（ms） | 19.5 / 31.7 / 59.4 | 7.0 / 15.5 / 23.7 |
| `budget_stop.total` | 44 | — |
| `settle_delay` p99（ms） | 24.3 | 79.9 |
| 服务端每 tick 工作 p99 | 1.78ms | 2.21ms |

`server.json` 原文：

```
log_backlog_per_tick {"entries": {"max": 120, "mean": 10.275, "min": 0, "p50": 0, "p95": 71, "p99": 110}, "nonzero_ticks": 46, "samples": 200}
```

结论：16 视野 ApplySync 下运行中确有日志积压（约四分之一的 tick 非零，最多 120 条），运行末为 0；政策阶段 p95 31.7ms 已超过 20Hz 半周期 25ms，帧数少约 1.3%（196,386 / 199,000）。ApplyAsync 帧数满额、政策阶段短，但事实到结算的延迟（settle_delay p99 79.9ms）更长。与 2026-10-22 的 16 视野 ApplySync（P99 126～128ms、政策阶段 p95 29.7～38.1ms、帧 194,991～197,000）同一量级。这一档仍超周期，缺省上限 8 维持有依据。原始：`m8/m8-v16-{sync,async}/sample-1/{server,client,config}.json`、`m8/m8-summary.txt`。

## M-10（No.30、99）独立 Gate 追帧慢链路

命令（1 次）：`LS_DRILL_FRAMES=3000 LS_DRILL_RATES=256,64 go test -tags integration -run TestRealGateLockstepCatchupOverASlowLinkDrill -count=1 -timeout 20m -v ./wiring/gate/`，NATS / toxiproxy 用共享隔离环境（用例自建、自删代理）。单位：十进制 kB/s。

```
rate 256 KB/s: backlog 3000 frames, client read 1386849 B over 3308 frames (419 B/frame); server catch-up done 3.235s (client had 791122 B); client at head 5.268s; expected backlog/(rate-live) 5.167s; ratio 1.02; Tick errors 0 (last <nil>); closed by Gate false; live after head true; client next 3309 head 3308
metric player_tcp_write_deadline_total: [] -> []
rate 64 KB/s: backlog 3000 frames, client read 160805 B over 3069 frames (52 B/frame); server catch-up done 2.302s (client had 133983 B); client at head 0s; expected backlog/(rate-live) 2.518s; ratio 0.00; Tick errors 0 (last <nil>); closed by Gate true; live after head false; client next 385 head 3069
metric player_tcp_write_deadline_total: [] -> [map[deadline:send_budget]=1]
catch-up over the Gate did not return to live: closed true, at head after 0s, live afterwards false
--- PASS: TestRealGateLockstepCatchupOverASlowLinkDrill/rate256KBps (10.80s)
--- FAIL: TestRealGateLockstepCatchupOverASlowLinkDrill/rate64KBps (2.82s)
```

结论：256 kB/s 下 3000 帧积压 5.27s 追到最新帧（预期 5.17s，比 1.02）并回到实时，没有写期限关闭；64 kB/s 下服务端约 2.3s 就把追帧写完（客户端那时只收到 134 KB），Gate 写 socket 时**单帧发送预算**（`send_budget`，缺省 ≤ `outbound.send_timeout` 250ms）到期断开，客户端停在第 385 帧。这补上了 guide/04-sync“演练里的实测分布待补”：断开计在 `deadline="send_budget"`，不是 `write_timeout`。与既有限制一致（独立 Gate 没有写出回执、不随链路放慢），不是新问题；64 kB/s 档的失败是测量结果，用例本身是演练、不进回归。原始：`m10/m10.out`。

## M-11（No.15.2）effect 消费者对联盟 Join RPC 的影响

在私有 mirror-local（偏移 30000，用完 clean）上跑，每种 1 轮（计划 3 轮×2 改 1 轮×2）：`ALLIANCE_E2E_PERF=1 ALLIANCE_E2E_RETRY=1 ALLIANCE_E2E_EFFECT_WORKERS=$w ALLIANCE_E2E_RUN='^TestAllianceE2EJoinRetryableSources$' bash scripts/test-alliance-e2e.sh`。每次脚本内部 3 段、每段约 15s，256 并发新玩家 Join，写额度 512，有限重试。

```
PERF retry-sources remote_entity.max_concurrent_writes=512 effect_consumer.workers=16 retry=true
PERF retry-sources round=1 conc=256 seconds=15.4 ok=11625 (754.8/s) rejected=0 retryable=0 unknown=0
PERF retry-sources round=2 conc=256 seconds=15.5 ok=9013 (581.3/s) rejected=0 retryable=0 unknown=0
PERF retry-sources round=3 conc=256 seconds=15.6 ok=7526 (483.5/s) rejected=0 retryable=0 unknown=0
PERF retry-sources remote_entity.max_concurrent_writes=512 effect_consumer.workers=1 retry=true
PERF retry-sources round=1 conc=256 seconds=15.2 ok=11057 (725.7/s) rejected=0 retryable=0 unknown=0
PERF retry-sources round=2 conc=256 seconds=15.6 ok=8989 (577.7/s) rejected=0 retryable=0 unknown=0
PERF retry-sources round=3 conc=256 seconds=15.5 ok=7432 (479.3/s) rejected=0 retryable=0 unknown=0
```

每段 `logical` 与 ok 相等、`retry_exhausted=0`。结论：effect workers 16 与 1 的 Join 吞吐差 1～4%（755 / 726、581 / 578、484 / 479），在单样本噪声内；两种设置都没有 retryable / unknown，与既有“额度 512 三轮为 0”一致。**本机这个负载下没有看到 effect 消费者抢 RPC 容量**；目标硬件 C-Q2 仍未验。两种设置下吞吐都随段递减（约 −36%），原因没有拆（推测与联盟成员 / 文档随加入增长有关，**尚未确认**）。原始：`m11/m11-w{16,1}.out`、`m11/m11-logs-w*/`。

## M-12（No.57、113）game-demo 100 / 200 人同秒

按维护者压缩口径只做同秒两档、不做爬坡；每档全新私有 mirror（`demo-robot.sh` 自起自清）与全新生成工程（唯一工程前缀 `dmr<6 位十六进制>`），单 IP 上限 512：

```sh
DEMO_ROBOT_MIRROR_OFFSET=21000 DEMO_ROBOT_COUNT=$n DEMO_ROBOT_MODE=burst DEMO_ROBOT_OUT=<新目录> bash scripts/perf/demo-robot.sh   # n = 100，然后 200
```

起跑失败（未计入结果，原文 `m12/m12-failed-port/`）：第一次用缺省偏移 37000，私有 Mongo 端口 64119 正被共享环境 mongod 副本间连接的本地临时端口占着（`port 64119 unavailable`），两档都没起来；改偏移 21000 后 100 人档正常，200 人档紧接着起时 48118（上一档私有 Mongo 的端口）还没释放，隔 30 秒重跑正常。

| 档 | 成功 / 人数 | 整场景 p50 / p95 | 除 saga 组 p95（门槛 16s） | saga 组 p95 | 失败步骤 | 日志：gift / alliance saga still，saga 仍未知 / 等待 | `nest: sync timeout`（含 World 热点三类） | 起跑前负载 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 100 同秒 | **96 / 100** | 21.5s / 25.3s | 6.48s（通过） | 21.3s | 4 个，都在第 20 步 `gift_status` 轮询 | 4 / 0，8 | **0** | 4.65 |
| 200 同秒 | **182 / 200** | 39.0s / 44.5s | 8.85s（通过） | 38.8s | 18 个：第 20 步 `gift_status` 14、第 34 步 `alliance_op_status` 4 | 14 / 4，22 | **0** | 1.91 |

`summary.json` 原文：

```
100: final_error "threshold violated: error_rate = 0.04 > max 0 (100 samples)"  stats success 96 failure 4  logged_occurrences {"gift_saga_still": 4, "alliance_saga_still": 0, "saga_unknown_or_waiting": 8, "nest_sync_timeout": 0, ...}
200: final_error "threshold violated: error_rate = 0.09 > max 0 (200 samples)"  stats success 182 failure 18  logged_occurrences {"gift_saga_still": 14, "alliance_saga_still": 4, "saga_unknown_or_waiting": 22, "nest_sync_timeout": 0, "world_stats_sync_timeout": 0, "world_record_enter_sync_timeout": 0, "player_world_add_exp_sync_timeout": 0}
```

200 人档平均最慢的几步（ms）：第 30 步 alliance_op_status 平均 9066 / p95 12913；第 34 步 8584 / 11847（4 个失败）；第 20 步 gift_status 7315 / 14075（14 个失败）；第 15 步 gift_status 3461 / 7700；之后才是 mailbox_list 1813、battle 1339。

结论：

- 11-20 把 World 上的写改成 `pipelined completion=async` 之后，**200 人同秒档首次重测**：World 热点上的 `nest: sync timeout` 为 0（11-18 基线 105 次），成功从 86 / 200 升到 182 / 200。与 11-18 诊断对照的预测一致：失败整体移到 saga 状态轮询（赠礼 14、联盟 4），没有出现在 add_exp。
- 100 人档 96 / 100，4 个失败都是赠礼 saga 状态轮询用尽次数；11-18 同口径是 100 / 100，本次单样本，**差异原因尚未确认**。
- 两档的门槛失败都只来自错误率（saga 组失败），除去 saga 组的场景 p95 都在 16s 门槛内；saga 组 p95 21s / 39s。200 人同秒档能否通过仍取决于 saga 并发的决定（KNOWN-LIMITS 已记）。日志次数是出现次数，不是未完成 saga 数（见 `scripts/testdata/demo-robot/README.md`）。失败的赠礼 / 联盟 saga 最终状态没有核对（没开 `DEMO_ROBOT_TRACE_GIFTS`）。
- 每档新环境下没有“上一轮 saga 积压被继承”的条件，本轮不能验证那条推断。原始：`m12/m12-burst{100,200}/{summary.json,robot.json,diagnostic-matches.json,logs/}`。

## M-13（No.60）全服邮件账本在上界附近的成本

命令：`REDIS_ADDR=<addr> go test -tags integration ./service/mail -run '^$' -bench BroadcastLedgerAtCapacity -benchtime 2000x -count 3`。三处 Redis：共享隔离环境单机 17379、私有 mirror-local（偏移 30000）单机 46379、同一 mirror 的 3 主 3 从 Cluster 47400～47405（AOF 开）。

**计划的 publish 子项在 2000x 下失败**：`mail: broadcast ledger is full: 1000 unexpired broadcasts, limit 1000`。原因是基准本身：预填第 k 封在 base+k+1 过期，第 i 次发布的新邮件在 now+1001 过期，第 1000 次起回收不出位置，所以 publish 子项最多只能跑 1000 次（与 1x 功能探针通过不矛盾）。本轮 publish 改用 `-benchtime 1000x`，其他子项 2000x；基准代码没改。这是夹具缺陷，不是邮件服务缺陷。

| 子项（ns/op，3 次） | 共享单机 17379 | 私有单机 46379 | 私有 Cluster |
| --- | --- | --- | --- |
| publish（1000x） | 2.73M / 2.75M / 2.80M | 1.08M / 1.08M / 1.09M | 1.25M / 1.26M / 1.22M |
| revoke | 2.96M / 3.23M / 3.37M | 1.17M / 1.19M / 1.19M | 1.31M / 1.35M / 1.31M |
| snapshot-hinted | 30.4k / 22.6k / 23.5k | 18.7k / 16.8k / 17.0k | 26.6k / 22.1k / 21.1k |
| snapshot-reload | 733k / 720k / 749k | 438k / 436k / 438k | 475k / 453k / 462k |

每次发布 / 撤回约 1.4～1.6 MB 分配、2100 次分配（整份 JSON 读写）；重载约 320 KB、2035 次。结论：账本满 1000 条时，同一套私有环境里 Cluster 比单机慢约 6～15%（发布 1.24 对 1.08ms），读路径常见情况（版本没变）每次约 17～27µs；共享单机 17379 慢 2～3 倍，该实例同时承载其他数据、没有单独查原因，**不作单机与 Cluster 的比较依据**。单个 mail 进程上发布 / 撤回的串行上界按此约 800～900 次/秒（推算，没有并发测）。原始：`m13/m13-{single,single-publish1000,private-single,cluster}.out`。

## M-14（No.72）CellWorld demo / game 尺度

命令（每种 1 次，计划 2 次交替改 1 次）：`ROOST_CELLWORLD_PERF=1 ROOST_CELLWORLD_PERF_SCALE=$scale ROOST_CELLWORLD_PERF_COUNT=1000 ROOST_CELLWORLD_PERF_NPCS=2250 ROOST_CELLWORLD_PERF_ACTIVE=5 ROOST_CELLWORLD_PERF_SECONDS=60 go test ./gameplay/cellworld -run '^TestCellWorldMeasure$' -count=1 -timeout 5m -v`。4 Cell、每格约 2500 Real，1000 玩家，20Hz。两次都 PASS（用例不设延迟门槛）。

| 指标 | demo（cell 7500 / ghost 900 / view 800 / 主视野 64） | game（cell 12800 / ghost 7000 / view 6800 / 主视野 128） |
| --- | --- | --- |
| witness 可见数 p50 / p99 | 64 / 64 | 128 / 128 |
| Base CPU | 122.7% 一核 | 104.3% |
| witness 延迟 p50 / p99 / max | 1.45 / 8.13 / 18.4ms | 4.50 / 19.67 / 98.6ms |
| 每格 Ghost 数 | 601～679 | 3435～3606 |
| 每格 CPU | 108～113% | 158～163% |
| 每格 region beat p50 / p99 | 3.9～5.6 / 6.4～12.0ms | **95.9～115.8 / 126～140ms** |
| ghost 延迟 p50 / p99 | 0.16～0.18 / 4.0～6.7ms | **61.8～70.5 / 149～164ms** |
| handoff p50 / p99 | 1.8～3.1 / 9.3～16.7ms | **192.6～215.2 / 332～384ms** |
| 重传 / outbox 丢弃 / 冲突 | 0 / 0 / 0 | 204～236 / 0 / 0 |
| 合并（coalesced） | 0 | 76k～118k |
| 出站每格 | 约 4000 msg/s、5.6～6.6 MB/s | 628～886 msg/s、15.3～18.0 MB/s |

game 尺度原文（一格）：

```
cell (1,0): reals 2545 ghosts 3510 | CPU 160.2% of one core | region beat p50 115.82ms p99 140.42ms max 227.16ms | out 628 msg/s 18012 KB/s, in 1243 msg/s 2727 KB/s | ghost latency p50 70.45ms p99 158.99ms (n=39591) | handoffs 199 (p50 193.65ms p99 331.57ms), rejected 0 aborted 0 retransmits 226 | outbox drops 0 dispatch errors 0 conflicts 0
```

结论：demo 尺度下各项都远低于 50ms 周期。game 尺度（ghost_band 7000、view 6800）每格 Ghost 约是 demo 的 5.5 倍，**每格 region beat 中位数 96～116ms，是 20Hz 周期 50ms 的两倍多**，ghost / handoff 延迟随之上到 0.15～0.38s、出现 200 多次重传与大量状态合并；没有丢弃和冲突，计数仍正确。也就是说本机这个负载（每格约 2500 Real + 3500 Ghost、5% 移动）下，游戏尺度 4 Cell 跟不上 20Hz。是 region beat 的哪一段随 Ghost 数变慢没有拆（没开 profile），**根因尚未确认**；game 尺度开跑时 1 分钟负载 10.0（上一段 demo 尾巴 + 本身），也会放大。原始：`m14/m14-{demo,game}.out`。

## M-16（No.97）Block AOI 集中布局 observed 0 / 4——未完成

计划命令（1000 人、集中布局、独立 Gate + 场景 Sync，c0 / c4，duration 计划 10min 改 4min）两档都在开场约 8 秒失败：

```
first gate client failure at 2026-11-22T08:16:09.331174+08:00: read tcp 127.0.0.1:49321->127.0.0.1:49320: i/o timeout
load_test.go:613: Sync mismatch: client=0 want count=50 got=0 missing=[2824(subscribed=true) 3848(subscribed=true) ...]
load_test.go:613: Sync diagnostic frames=0 updates=0 first-client=map[]
--- FAIL: TestNestGameLoad (7.91s)
```

排查（约 10 分钟，未改代码）：

- 同参数改 spread 布局 30s（`m16-diag-spread-1000.out`）通过；集中布局改 400 人 30s（`m16-diag-concentrated-400.out`）通过。所以失败只在“1000 人 + 集中布局 + Gate”。
- 夹具 `tcp_clients_test.go` 在握手时给连接设 5 秒期限（`SetDeadline`），之后只在发请求时刷新；读协程沿用这个期限。第一个客户端的失败正好在监听启动后约 5 秒（08:16:04 → 08:16:09.33），之前一帧都没收到。推断：1000 人集中布局下开场（全部连上 + 首轮全量）超过 5 秒，第一个连接的读期限先到，读协程退出——**夹具期限问题的可能性大，尚未确认**（没有拆开场各段耗时）。

改用 400 人重跑两档（其余参数同计划，4min，`m16-p400-c{0,4}.out`）：

| 档 | 结果 | 关键数字 |
| --- | --- | --- |
| c0（observed 0） | PASS | 业务 960,000 / 960,000，业务 p99 12.08ms、服务端 p99 2.00ms；Gate 往返 p99 3.56ms；Sync 帧 428,330，VisibleMax 50，值核对通过；改值→客户端 p99 23.5ms |
| c4（observed 4） | FAIL（负载跑满 4 分钟后收尾核对失败） | `Sync mismatch: client=0 want count=376 got=100`，缺的对象都是 `subscribed=true`；运行期间每约 10 秒一条 `entitysync: a session is over its object limit ... limit=100 held=100 deferred=1`（共 21 条）；Sync 帧 1,024,788 |

c4 的原文：

```
first gate client failure at 2026-11-22T08:29:52.94208+08:00: read tcp 127.0.0.1:53558->127.0.0.1:53550: i/o timeout
sync_load_test.go:421: Sync mismatch: client=0 want count=376 got=100 missing=[43784(subscribed=true) 44808(subscribed=true) ...]
WARN entitysync: a session is over its object limit; the extra objects are not synced to it until there is room (the session stays open). ... session=71 namespace=* limit=100 held=100 deferred=1 evicted=0 pinned_blocked=0
```

读法：集中布局 + 每次改值额外覆盖 4 个块时，夹具的期望集合取 `Interest.Seen`（距离 + 块覆盖 + self 全部来源）为 376 个，超过会话对象上限 100；按框架既定行为，超出的对象暂缓、会话不关，所以客户端最多 100 个，收尾核对按 376 个比必然不等。块覆盖来源不占 `MaxVisible`，主视野收紧到 0 也腾不出 276 个位置。另外负载结束约 5 秒后客户端读期限到期（同上一条夹具期限），收尾核对期间读协程已退出。

结论：**M-16 未完成**。observed 4 的“值不收敛”（`fa936c0d` 修的那种缺 1 个对象）这次没法判定——期望集合本身超出会话上限，核对口径与配置不匹配。需要维护者定：(a) 夹具把期望集合截到会话上限（或调大会话上限 / 减少覆盖块）后重跑；还是 (b) 认为块覆盖不受会话上限约束本身是 Block AOI 的设计缺口。本轮不判，**尚未确认**。1000 人档要先解决夹具 5 秒读期限。原始：`m16/`（含两次诊断与 400 人两档；`m16-diag-mistaken-default-env.out` 是一次环境变量没传进去、按缺省参数跑的误操作，作废）。

### 2026-11-22 夹具修复后重跑（维护者定 A）

夹具改了三处（`codegen/internal/entity/testdata/nestgame`，只影响压测，不改框架）：

1. **读写期限分开**（`tcp_clients_test.go`）：握手后清掉整体期限；读协程每次读之前自己续 60s（`loadReadIdle`），写只在发请求时设 5s。以前读沿用写侧刷新的期限，1000 条连接逐个建好超过 5s，第一条在负载开始前就读超时——上面“开场约 8 秒失败”的原因，修后确认。
2. **终值核对按会话上限**（`sync_load_test.go` 的 `clientMismatch`）：应见集合不超过会话对象上限（`frame.DefaultLimits().MaxObjects`=100）时仍要求两边集合与值完全一致；超过时要求客户端恰好持有 100 个、每个都在应见集合里、值与 DAO 一致。暂缓哪几个由框架决定，不核对。
3. **收敛上限可配**：`ROOST_NEST_GAME_SETTLE`（缺省 10s，原硬编码值），报告新增 `SyncReport.SettleMS`。

重跑结果（负载 1～3.8，每档 1 次）：

| 档 | 时长 | 结果 | 关键数字 |
| --- | --- | --- | --- |
| 1000 人 c0 | 30s | 功能通过；只因延迟门槛判失败 | 业务 300,000 / 300,000、拒绝 0；Gate 往返 p99 9.48ms；VisibleMax 50；提交→客户端 p99 51.83ms（门槛 50ms） |
| 400 人 c4 | 60s | 终值核对通过；只因延迟门槛判失败 | 业务 240,000 / 240,000、拒绝 0；每个客户端恰好 100 个对象，收敛 0.32s；提交→客户端 p99 93.07ms |
| 1000 人 c4 | 30s，收敛上限 10s～300s | **负载结束后收敛极慢，被客户端 60s 读静默中止** | 见下面“追查” |

结论：夹具的两个问题已修，1000 人 c0 与 400 人 c4 都能跑完并核对通过；1000 人 c4 的原因见下面的追查（**先前写的“阻塞在 PushBound / 独立 Gate 出站卡住”是误判，已更正**：那条报错是收尾 Flush 跑到一半 ctx 到期，不是传输阻塞）。

#### 追查 1000 人 c4（2026-11-22）

- **goroutine 栈**（`-test.timeout` 触发 panic，卡住时抓）：没有任何 goroutine 在 `PushBound`；Manager 的 flush 在 `deferUnselectedSnapshotsLocked` 上 runnable（在跑 CPU），测试主协程在核对循环里逐个算 `Interest.Seen`。不是死锁。
- **收敛进度**（夹具新增每 10s 一行 `Sync settling`：Manager 健康计数、累计帧数、未收敛客户端数与缺口）：

| 负载结束后 | 未收敛客户端 | 缺对象总数 | 累计帧 | 订阅总数 | 超额暂缓（over_limit_waiting） | pending |
| --- | --- | --- | --- | --- | --- | --- |
| 10s | 398 | 7553 | 18,428 | 963,030 | 168,195 | 1257 |
| 34s | 280 | 2154 | 18,527 | 962,886 | 279,210 | 1252 |
| 52s | 151 | 1145 | 18,740 | 961,649 | 360,255 | 1043 |
| ~60s（某客户端 60s 没收到包，读期限到期中止） | 56 | 192 | 18,804 | 961,649 | 422,078 | 1043 |

- **读法**：在收敛，只是极慢——1000 个会话合计每秒只出约 6 帧。集中布局下 4 个覆盖块盖住几乎整个对象群，每个会话应见约 940 个、会话上限 100，约 96 万个订阅里大部分转为超额暂缓；块覆盖没有距离，Interest 不收紧它（`interest_over_limit.go` 的既定行为：无距离来源本身超过会话上限时剩下的留在 Manager 暂缓里）。暂缓的订阅不参与捕获，但每次 flush 仍按 subject 逐个扫描全部订阅者（`deferUnselectedSnapshotsLocked` 等），flush 成本随暂缓数增长、帧率塌下去（这一归因来自栈采样与代码，没开 CPU profile 量化各段占比），轮不到的会话一分钟收不到包。
- **定性**：不是正确性问题（没有死锁、没有错值，最终会补齐），是**无距离来源超过会话上限时的成本问题**：配置本身违反了 Warn 里“政策上限要在会话上限之内”的要求，框架按设计降级，但降级的代价随暂缓订阅数线性增长、没有上界。是否要让 flush 跳过暂缓订阅的扫描（例如按会话索引而不是按 subject 逐个扫），待维护者定。

原始：`artifacts/perf/nest/m16diag-c4-*`、`m16fix-*`；goroutine 栈与 300s 那次的日志在 scratchpad（已摘录）。

## M-17（No.103、112）生成工程持久写入四档

命令（每档 1 次，计划 5min 改 120s；50 连接；Mongo 用共享隔离环境，每档唯一库 `roost_generated_tcp_*`，脚本结束已删）：

```sh
ROOST_GENERATED_TCP_SECONDS=120 ROOST_GENERATED_TCP_OUTPUT=<新目录> DURABILITY=strict SELF_SYNC=0 bash scripts/perf/generated-tcp.sh
#   strict + SELF_SYNC=1；DURABILITY=pipelined OPEN_LOOP_RATE=1500；DURABILITY=pipelined ENTITY_MODE=shared
```

| 档 | 确认 / 提供 | 吞吐 | AddGold 往返 p50 / p95 / p99 / max（ms） | WAL 记录 / fsync（每次 fsync 的记录数） | 其他 |
| --- | --- | --- | --- | --- | --- |
| strict，50 个玩家 | 106,944 / 106,944 | 890.8 次/s | 61.7 / 73.4 / 80.0 / 115.2 | 106,944 / 16,622（6.4） | 冷读 Gold 一致，WAL 未确认 0 |
| strict + self_sync | 107,491 / 107,491 | 895.3 次/s | 61.7 / 73.4 / 80.0 / 101.1 | 107,491 / 17,248（6.2） | self_sync 包 107,541 |
| pipelined 开环 1500/s | 116,914 / 180,000（丢 63,086，35%） | 973.2 次/s | 52.0 / 67.3 / 73.4 / 98.5 | 116,914 / 15,612（7.5） | 到达→答复 p50 142.9ms、p99 202.4ms；客户端队列 p50 94.9ms |
| pipelined，同一玩家 50 会话 | 12,540 / 12,540 | 104.1 次/s | 488.5 / 532.4 / 580.3 / 595.0 | 12,540 / 12,540（1.0） | — |

四档都 PASS：错误 0、冷读一致、投影追平 30～55ms。结论：

- 本机（macOS `F_FULLFSYNC`）strict 50 个玩家约 890 次/s，每次 fsync 合并约 6 条，往返 p50 约 62ms；self_sync 不改变吞吐和延迟。
- pipelined 开环 1500/s 超过本机这条链路的处理能力：只接下约 973 次/s，35% 的到达因队列满 / 错过时隙被丢（夹具明确计数，不重试），到达→答复 p99 0.2s。
- 同一实体上 pipelined 每次 fsync 只有 1 条，吞吐 104 次/s、往返 p50 0.49s：同一实体的后继要等前一条（含持久）结束，与 M-15 记录的“pipelined 第一阶段仍按实体排队”是同一机制，不是新问题。
- 单进程夹具（客户端与服务模块同进程），不代表独立 Gate、跨机或目标部署容量。原始：`m17/<档>/{report.json,test.log,env.txt}`、`m17/m17-summary.txt`。

## 清理

- worktree：测量 worktree 与 M-8 诊断 worktree 在拷出 artifacts 后已删除。
- 私有依赖：mirror-local 偏移 30000（M-13 Cluster、M-11）已 `clean`（日志 “no residual processes”）；M-12 的私有 mirror 由 `demo-robot.sh` 自起自清（见 M-12 节）。
- 共享隔离环境：只用了各用例自建的唯一库 / 前缀 / 代理，结束后核对共享 Mongo 无 `roost_generated_tcp_*` 库、共享 Redis 无 `mailbench:*` 键。
- 结束后 `ps` 无本轮留下的测试二进制、mongod / nats-server / redis-server / toxiproxy / etcd 进程（共享环境与 brew 服务、Meshy.app 进程未动）。
