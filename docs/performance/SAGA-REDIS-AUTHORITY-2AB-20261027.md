# Saga 协调器存储迁到 Redis 权威：第 2 步 2a / 2b 同机对照（2026-10-27）

[设计 v2](../maintenance/DESIGN-2026-10-26-redis-authority-v2.md) §8 要求的压测：第 2 步 2a（协调器存储换成单主权威 Redis）与 2b（Store Service 把 Redis 日志投影进 Mongo 冷副本）完成后，与当前 main 在同一台机器上交替对照，按 §6 的门槛逐项判定。维护者 2026-10-26 指示：“直接从 V2 开始吧，之后跑一次 saga 的压测”。

样本、逐跳时间线、fsync 微基准与 Redis 延迟采样放在 [benchmarks/saga-redis-authority-2ab-20261027](benchmarks/saga-redis-authority-2ab-20261027/)；完整原始结果在本机 `artifacts/perf/saga-redis-2ab-20261027/`（不随仓）。

## 0. 结论

1. **按 v2 §8 的正式口径（权威 Redis 的 AOF 与 Mongo、NATS 在同一块盘上），2a 的三项性能门槛都没有达标，新版本比 main 慢。** Mongo 步骤饱和吞吐：worker 2 时约 17.5 TPS（main 约 43，0.40～0.45 倍），worker 16 时约 22 TPS（main 约 45，0.47～0.52 倍，门槛 ≥ 1.5 倍）；c1 P50 195 / 192ms（门槛 ≤ 0.85 倍）；原生步骤饱和吞吐 0.78～0.87 倍（门槛 ≥ 0.95 倍）。正确性方面，c1～c64 全部档位的终态与账户核对两边都通过，冷热副本 0 差异。
2. **慢的原因是本机上每次 Redis 写都要等一次很贵的落盘，而 Redis 主线程在落盘期间不处理任何命令。**
   - Redis 在 macOS 上把 `appendfsync always` 实现为 `fcntl(F_FULLFSYNC)`（Redis 源码 `src/config.h` 的 `redis_fsync`；Linux 上是 `fdatasync`）。本机这块盘空闲时一次 F_FULLFSYNC 约 4ms（[微基准](benchmarks/saga-redis-authority-2ab-20261027/fsync-microbench.txt)），压测中 Redis 自己记的 `aof-fsync-always` 典型 16～18ms、最大 103ms（Mongo 三节点的 journal 和 NATS 在同一块盘上刷盘）。
   - 落盘在 Redis 主线程上同步做：每轮事件循环 = 执行这一轮收到的命令 + 写 AOF + F_FULLFSYNC，之后才回复。一个脚本的往返至少等一轮，通常一轮半（到达时正赶上上一轮在落盘）；只读的 HMGET 也排在落盘后面。Mongo 在 macOS 上同样用 F_FULLFSYNC 刷 journal（WiredTiger `__posix_sync`），但它多线程、按组提交，读和别的会话不被一次刷盘挡住。
   - 每个串行工作池的吞吐 ≈ worker 数 ÷（每项串行脚本数 × 每个脚本的往返），压测中每个脚本 50～165ms（§3）。逐跳时间线（§3）确认：worker 2 时饱和的是 2 个结果消费者（`result_transport` 656ms，是在 JetStream 里排队），worker 16 时饱和的是 4 个发布器（`outbox_wait` 1061ms）。main 上同一位置是 2～3 次 Mongo 往返，单个约 17～35ms，而且不互相挡。
3. **同一套代码把权威 Redis 的 AOF 放到内存盘上（诊断，不是部署口径），2a 的三项门槛都过了**：Mongo 步骤 worker 16 饱和约 143～148 TPS（main 的 3.1～3.3 倍）；c1 P50 118 / 115ms（0.61 / 0.64 倍）；原生步骤正向 / 补偿饱和 1.00～1.35 倍。协调器各跳从 50～160ms 降到 1.5～2.3ms。设计本身的往返收益成立；门槛不达标落在“每次写都 F_FULLFSYNC、与 Mongo 共用一块盘”这一条部署条件上。
4. **每笔操作数符合设计**：Mongo 步骤两步每笔前台 Mongo 命令 50 → 19.0（预期约 17），事务提交 8 → 4，Store Service 的批量投影另计每笔约 7～9 条命令（不在关键路径上）；协调器每笔 11 个脚本 + 2 次只读 HMGET，与 v2 §3.2 一致（`TestRedisRoundTripsPerTwoStepSaga` 精确计数）。
5. **2b 门槛**：投影吞吐 2.05 万～2.74 万条/s（门槛 5000）；停止加压后积压 3～8ms 归零（门槛 30s）；冷副本 0 差异；驱逐后的迟到成功装回并补偿（集成用例）。积压峰值按“条数 ÷ 在途笔数”字面口径，Mongo 步骤 c1 / c4 档是 3 / 1、10 / 4，略超 2 倍（日志条数与在途笔数单位不同，见 §5）。
6. **需要维护者决定**（§6）：v2 §6 规定 2a 门槛全部满足才进下一步。推荐 A：2a / 2b 合入 main，2a 的性能门槛改在 Linux、权威 Redis 独占一块盘的条件下复测判定，同时开始 2c（每步脚本 5 → 2，直接减少落盘次数）。

## 1. 环境与方法

- **机器**：本机 macOS（Apple Silicon，darwin/arm64），go1.27.3，GOMAXPROCS=4，`saga-scan.sh` 开跑前 loadavg 门控；swap 已用约 2.4 GB。宿主长期有后台负载（CorpLink、fseventsd），各档 loadavg 记录在 `runs/*.txt`。
- **Mongo / NATS**：`~/.roost-it/roost-dataengine-it` 隔离环境（Mongo 副本集 3 节点、JetStream 3 节点），每档持 `remote-acceptance.lock`，库名 / 流名带本轮唯一前缀，结束后核对残留为 0。
- **权威 Redis**：`scripts/it-redis-aof.sh` 起的私有单主 redis-server 8.10.1（端口 26391，`appendonly yes`、`appendfsync always`、`no-appendfsync-on-rewrite no`、`maxmemory 2gb`、`noeviction`、无 RDB、无副本）。正式口径的数据目录在 `~/.roost-it/redis-aof/`（与 Mongo、NATS 同一 APFS 卷）；诊断口径放在 2 GB 内存盘 `/Volumes/redisram`（`ROOST_IT_REDIS_AOF_ROOT`）。每轮结束删键并核对，残留 0。
- **对照**：
  - 基线：origin/main `c74376c7` 的干净 worktree，只 cherry-pick 测量提交 `891d93c0`（计数、终态复读、脚本），不带产品代码。
  - 新版本：分支 `feat/redis-authority-2ab`（2a `6709eb90` + 2b `a3edef04` + 压测夹具连接池取生产缺省 `2aad947c`），夹具在进程内跑 Store Service。
  - Mongo 步骤夹具的权威 Redis 连接池最初是 32（沿用跨进程辅助函数的值），比生产缺省 64 小；原生步骤夹具走 wiring 装配，本来就是 64。用 32 跑的第一轮（`runs/saga2ab-new-s0-w2-pool32.txt`）Mongo 步骤 w2 c16～c128 饱和 16.7～17.8 TPS，改 64 后 17.2～17.8，没有可分辨差异；这一轮不计入表，样本保留。
- **档位**：`native-forward`、`native-compensate`、`mongo-forward`，并发 1 / 4 / 16 / 32 / 64 / 128，每档 60s，稳态窗口 [10s, 60s)；Mongo 步骤 worker 2（三个场景）与 16（只 mongo-forward）。顺序：基线 s1 w2 → 新 s1 w2 → 基线 s1 w16 → 新 s1 w16 → 基线 s2 w2 → 新 s2 w2 → 基线 s2 w16 → 新 s2 w16，最后内存盘一轮（w2、w16）。
- **计数**：Mongo 命令取每档前后 `serverStatus.metrics.commands` 增量（不含 `getMore`，扣掉夹具自己的读写），Store Service 的投影写按 `store_service.mongo_ops_total` 单列；Redis 取 `INFO commandstats` 增量。Redis 计数包含协调 / 发布循环被唤醒却没领到东西的空领取、Store Service 的租约与推进脚本；HMGET 主要来自夹具按 ID 每约 50ms 轮询终态（新版本的终态在 Redis 上），不是协调器自己的读。协调器的精确脚本数用单元用例计（§2.2）。
- **延迟**：夹具现有口径（服务端 P50 / P99，含最多约 50ms 观察间隔）。新版本的终态轮询读 Redis，也要排在落盘后面，比基线读 Mongo 多约一个落盘周期，这部分计入新版本的延迟。

## 2. 结果

### 2.1 共享盘（正式口径）：吞吐与延迟

TPS 是两轮各自的稳态成功 TPS；比值、P50 / P99 是两轮均值。c128 的原生步骤两边都过载（步骤超时、出现 Failed / ManualRequired），见 §2.5。

| 场景 | 并发 | 基线 TPS | 新版本 TPS | 比值 | 基线 P50 / P99 ms | 新版本 P50 / P99 ms | 核对 基线 / 新 |
| --- | ---: | --- | --- | ---: | --- | --- | --- |
| native-forward | 1 | 3.20 / 3.24 | 3.02 / 3.20 | 0.97 | 286 / 376 | 302 / 428 | 通过 / 通过 |
| native-forward | 4 | 7.68 / 6.92 | 4.94 / 5.66 | 0.73 | 530 / 717 | 636 / 893 | 通过 / 通过 |
| native-forward | 16 | 9.92 / 10.06 | 7.62 / 7.94 | 0.78 | 1535 / 2166 | 1711 / 2405 | 通过 / 通过 |
| native-forward | 32 | 10.14 / 9.36 | 7.98 / 8.24 | 0.83 | 3240 / 3820 | 3352 / 5384 | 通过 / 通过 |
| native-forward | 64 | 10.48 / 9.96 | 7.40 / 8.98 | 0.80 | 6164 / 7844 | 7146 / 15434 | 通过 / 通过 |
| native-forward | 128 | 6.08 / 6.00 | 2.92 / 2.34 | 0.44 | 12650 / 36095 | 25610 / 38884 | **未通过** / **未通过** |
| native-compensate | 1 | 2.16 / 2.34 | 2.30 / 2.04 | 0.96 | 415 / 581 | 444 / 609 | 通过 / 通过 |
| native-compensate | 4 | 4.96 / 4.94 | 3.72 / 3.42 | 0.72 | 776 / 1119 | 979 / 1312 | 通过 / 通过 |
| native-compensate | 16 | 5.52 / 6.38 | 5.08 / 4.88 | 0.84 | 2654 / 3504 | 2725 / 3720 | 通过 / 通过 |
| native-compensate | 32 | 5.76 / 6.28 | 5.12 / 5.40 | 0.87 | 5298 / 7832 | 5507 / 7321 | 通过 / 通过 |
| native-compensate | 64 | 6.44 / 6.36 | 5.40 / 5.68 | 0.87 | 9882 / 16685 | 10426 / 22257 | 通过 / 通过 |
| native-compensate | 128 | 2.76 / 3.02 | 0.34 / 1.52 | 0.32 | 24730 / 30738 | 27494 / 51409 | **未通过** / **未通过** |
| mongo-forward w2 | 1 | 4.98 / 5.36 | 5.06 / 5.08 | 0.98 | 192 / 244 | 195 / 265 | 通过 / 通过 |
| mongo-forward w2 | 4 | 12.18 / 12.36 | 8.92 / 9.22 | 0.74 | 323 / 421 | 441 / 571 | 通过 / 通过 |
| mongo-forward w2 | 16 | 41.14 / 35.88 | 17.46 / 16.94 | 0.45 | 408 / 619 | 930 / 1105 | 通过 / 通过 |
| mongo-forward w2 | 32 | 43.36 / 43.74 | 17.62 / 17.24 | 0.40 | 728 / 925 | 1829 / 2115 | 通过 / 通过 |
| mongo-forward w2 | 64 | 43.40 / 43.84 | 17.24 / 16.44 | 0.39 | 1456 / 1865 | 3802 / 4187 | 通过 / 通过 |
| mongo-forward w2 | 128 | 42.84 / 42.40 | 17.82 / 17.82 | 0.42 | 2968 / 3969 | 7268 / 8125 | 通过 / 通过 |
| mongo-forward w16 | 1 | 5.54 / 5.42 | 4.82 / 5.24 | 0.92 | 179 / 241 | 196 / 272 | 通过 / 通过 |
| mongo-forward w16 | 4 | 13.58 / 13.66 | 9.90 / 8.78 | 0.69 | 295 / 397 | 426 / 571 | 通过 / 通过 |
| mongo-forward w16 | 16 | 43.86 / 42.46 | 22.46 / 22.44 | 0.52 | 365 / 512 | 703 / 1036 | 通过 / 通过 |
| mongo-forward w16 | 32 | 46.94 / 45.80 | 22.26 / 21.18 | 0.47 | 682 / 837 | 1466 / 1861 | 通过 / 通过 |
| mongo-forward w16 | 64 | 44.54 / 46.92 | 20.44 / 22.46 | 0.47 | 1386 / 1718 | 2973 / 3335 | 通过 / 通过 |
| mongo-forward w16 | 128 | 43.58 / 46.76 | 22.00 / 23.84 | 0.51 | 2821 / 3289 | 5601 / 6238 | 通过 / 通过 |

### 2.2 每笔操作数

| 场景 | 并发 | 基线每笔 Mongo 命令 | 新：前台 Mongo | 新：Store Service Mongo | 基线每笔 commitTransaction | 新每笔 commitTransaction | 新：每笔 EVALSHA+EVAL（含空领取与 Store Service） | 新：每笔 HMGET |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| native-forward | 1 | 105.4 | 50.1 | 9.0 | 10.0 | 6.0 | 50.9 | 39.5 |
| native-forward | 4 | 89.9 | 49.3 | 8.8 | 9.2 | 5.6 | 35.1 | 27.0 |
| native-forward | 16 | 127.1 | 83.8 | 8.4 | 8.8 | 5.1 | 28.6 | 24.2 |
| native-forward | 32 | 189.4 | 145.7 | 8.3 | 8.8 | 5.0 | 27.8 | 25.8 |
| native-forward | 64 | 295.2 | 276.4 | 8.3 | 8.8 | 5.1 | 27.9 | 29.9 |
| native-forward | 128 | 272.7 | 301.2 | 12.6 | 13.3 | 11.8 | 54.7 | 112.2 |
| native-compensate | 1 | 152.2 | 71.3 | 13.0 | 14.0 | 8.0 | 72.7 | 56.6 |
| native-compensate | 4 | 132.9 | 72.5 | 12.6 | 13.7 | 8.0 | 50.7 | 38.2 |
| native-compensate | 16 | 202.2 | 133.2 | 12.3 | 13.4 | 7.6 | 42.1 | 34.5 |
| native-compensate | 32 | 302.4 | 235.9 | 12.0 | 13.4 | 7.5 | 40.8 | 36.4 |
| native-compensate | 64 | 467.0 | 420.8 | 11.9 | 13.4 | 7.7 | 40.2 | 40.4 |
| native-compensate | 128 | 316.4 | 469.3 | 13.3 | 17.6 | 14.4 | 61.6 | 79.3 |
| mongo-forward w2 | 1 | 65.8 | 19.0 | 8.8 | 8.0 | 4.0 | 37.3 | 23.7 |
| mongo-forward w2 | 4 | 57.2 | 19.0 | 8.1 | 8.0 | 4.0 | 27.0 | 21.4 |
| mongo-forward w2 | 16 | 51.5 | 19.0 | 7.5 | 8.0 | 4.0 | 21.7 | 19.3 |
| mongo-forward w2 | 32 | 50.3 | 19.0 | 7.5 | 8.0 | 4.0 | 21.6 | 19.6 |
| mongo-forward w2 | 64 | 50.3 | 19.0 | 7.5 | 8.0 | 4.0 | 21.6 | 19.9 |
| mongo-forward w2 | 128 | 50.5 | 19.0 | 7.5 | 8.0 | 4.0 | 21.2 | 20.1 |
| mongo-forward w16 | 1 | 64.9 | 19.0 | 8.7 | 8.0 | 4.0 | 37.3 | 23.6 |
| mongo-forward w16 | 4 | 56.6 | 19.0 | 8.2 | 8.0 | 4.0 | 27.0 | 21.5 |
| mongo-forward w16 | 16 | 50.9 | 19.0 | 6.9 | 8.0 | 4.0 | 18.9 | 18.7 |
| mongo-forward w16 | 32 | 50.1 | 19.0 | 7.0 | 8.0 | 4.0 | 18.9 | 19.0 |
| mongo-forward w16 | 64 | 50.2 | 19.0 | 7.1 | 8.0 | 4.0 | 18.7 | 19.1 |
| mongo-forward w16 | 128 | 50.2 | 19.0 | 7.1 | 8.0 | 4.0 | 18.5 | 19.3 |

- **Mongo 步骤**：前台每笔 19.0 次与并发无关，是步骤侧（两步各一次预留事务、一次回执事务、业务写）加启动时的业务键守卫读 1 次；协调器的启动、派发、完成事务全部不再走 Mongo（每笔 `commitTransaction` 8 → 4，少的 4 个就是协调器事务）。v2 §8.6 预期约 17。
- **原生步骤**：前台每笔在低并发时减半（正向 105 → 50，补偿 152 → 71）；高并发下两边都被步骤侧等回执的轮询放大（c64 正向 295 → 276），这部分在步骤侧（DataEngine），第 2 步不动。
- **协调器脚本数**：`TestRedisRoundTripsPerTwoStepSaga`（`framework/saga/redis_store_test.go`，20 笔两步正向）计到每笔正好 11 个脚本（启动 1 + 每步领取 1、派发 1、outbox 领取 1、确认 1、接收结果 1）和 2 次 HMGET，与 v2 §3.2 第 2 步 2a / 2b 的 11 一致（[记录](benchmarks/saga-redis-authority-2ab-20261027/round-trips-test.txt)）；空领取另计（这 20 笔里 `claim_due` 60 次、`claim_outbox` 41 次）。压测里 `INFO commandstats` 的每笔 18.5～21.6（饱和档）包含空领取和 Store Service 脚本，低并发档空领取占比更高（c1 约 37）。
- **领取与积压无关**：`BenchmarkRedisClaimDue` 在积压 1000 / 1 万 / 10 万条时每次领取都是约 4.1ms（[记录](benchmarks/saga-redis-authority-2ab-20261027/bench-claimdue.txt)），即一次空闲盘上的 F_FULLFSYNC；ZSet 范围领取本身不随积压变慢。

### 2.3 Store Service 与冷热核对（新版本，共享盘两轮）

| 场景 | 并发 | 稳态终态笔数 | 积压峰值（条） | 停止加压后归零耗时 s | 冷热版本不一致 | 冷副本终态数 |
| --- | ---: | --- | --- | --- | --- | --- |
| native-forward | 1 | 180 / 193 | 2 | 0.004 | 0 / 0 | 180 / 193 |
| native-forward | 4 | 300 / 340 | 6 | 0.005 | 0 / 0 | 300 / 340 |
| native-forward | 16 | 464 / 487 | 9 | 0.005 | 0 / 0 | 464 / 487 |
| native-forward | 32 | 499 / 510 | 10 | 0.004 | 0 / 0 | 499 / 510 |
| native-forward | 64 | 474 / 564 | 14 | 0.004 | 0 / 0 | 474 / 564 |
| native-forward | 128 | 374 / 381 | 20 | 0.005 | 0 / 0 | 229 / 240 |
| native-compensate | 1 | 138 / 123 | 2 | 0.004 | 0 / 0 | 138 / 123 |
| native-compensate | 4 | 224 / 207 | 6 | 0.004 | 0 / 0 | 224 / 207 |
| native-compensate | 16 | 318 / 308 | 10 | 0.006 | 0 / 0 | 318 / 308 |
| native-compensate | 32 | 324 / 337 | 13 | 0.003 | 0 / 0 | 324 / 337 |
| native-compensate | 64 | 359 / 373 | 14 | 0.004 | 0 / 0 | 359 / 373 |
| native-compensate | 128 | 285 / 321 | 19 | 0.003 | 0 / 0 | 92 / 170 |
| mongo-forward w2 | 1 | 305 / 303 | 3 | 0.005 | 0 / 0 | 305 / 303 |
| mongo-forward w2 | 4 | 536 / 556 | 10 | 0.004 | 0 / 0 | 536 / 556 |
| mongo-forward w2 | 16 | 1055 / 1018 | 20 | 0.004 | 0 / 0 | 1055 / 1018 |
| mongo-forward w2 | 32 | 1075 / 1054 | 22 | 0.008 | 0 / 0 | 1075 / 1054 |
| mongo-forward w2 | 64 | 1058 / 1043 | 23 | 0.003 | 0 / 0 | 1058 / 1043 |
| mongo-forward w2 | 128 | 1149 / 1152 | 20 | 0.004 | 0 / 0 | 1149 / 1152 |
| mongo-forward w16 | 1 | 294 / 313 | 2 | 0.004 | 0 / 0 | 294 / 313 |
| mongo-forward w16 | 4 | 606 / 525 | 8 | 0.005 | 0 / 0 | 606 / 525 |
| mongo-forward w16 | 16 | 1356 / 1360 | 24 | 0.007 | 0 / 0 | 1356 / 1360 |
| mongo-forward w16 | 32 | 1338 / 1294 | 25 | 0.003 | 0 / 0 | 1338 / 1294 |
| mongo-forward w16 | 64 | 1278 / 1391 | 23 | 0.003 | 0 / 0 | 1278 / 1391 |
| mongo-forward w16 | 128 | 1408 / 1536 | 28 | 0.006 | 0 / 0 | 1408 / 1536 |

“积压峰值”是采样到的未投影日志条数峰值；“归零耗时”是停止加压后积压降到 0 的耗时；“冷热版本不一致”是积压归零后逐笔比较 Redis 驻留记录与 Mongo 冷副本版本的差异笔数；“冷副本终态数”是冷副本里达到期望终态的笔数，与 Redis 终态复读一致。

### 2.4 诊断：权威 Redis 的 AOF 放内存盘（一轮）

只换权威 Redis 的数据目录（Mongo、NATS 不变），用来回答“落盘代价去掉后设计的收益是多少”。内存盘断电即丢，**不是部署口径**，结果不能用于判定门槛。

| 场景 | 并发 | 基线 TPS 均值 | 内存盘 TPS | 比值 | 基线 P50 / P99 ms | 内存盘 P50 / P99 ms | 内存盘终态与核对 |
| --- | ---: | ---: | ---: | ---: | --- | --- | --- |
| native-forward | 1 | 3.22 | 4.60 | 1.43 | 286 / 376 | 210 / 295 | Completed 276 |
| native-forward | 4 | 7.30 | 8.96 | 1.23 | 530 / 717 | 438 / 697 | Completed 532 |
| native-forward | 16 | 9.99 | 10.02 | 1.00 | 1535 / 2166 | 1589 / 2257 | Completed 598 |
| native-forward | 32 | 9.75 | 10.28 | 1.05 | 3240 / 3820 | 3106 / 3967 | Completed 635 |
| native-forward | 64 | 10.22 | 12.82 | 1.25 | 6164 / 7844 | 4901 / 5755 | Completed 818 |
| native-forward | 128 | 6.04 | 9.28 | 1.54 | 12650 / 36095 | 6450 / 26655 | Completed 674, Failed 80 |
| native-compensate | 1 | 2.25 | 2.84 | 1.26 | 415 / 581 | 341 / 459 | Compensated 172 |
| native-compensate | 4 | 4.95 | 5.82 | 1.18 | 776 / 1119 | 684 / 939 | Compensated 344 |
| native-compensate | 16 | 5.95 | 6.14 | 1.03 | 2654 / 3504 | 2577 / 3340 | Compensated 376 |
| native-compensate | 32 | 6.02 | 6.76 | 1.12 | 5298 / 7832 | 4727 / 6154 | Compensated 422 |
| native-compensate | 64 | 6.40 | 8.62 | 1.35 | 9882 / 16685 | 7310 / 13214 | Compensated 548 |
| native-compensate | 128 | 2.89 | 5.04 | 1.74 | 24730 / 30738 | 18433 / 30912 | Compensated 421, Failed 78 |
| mongo-forward w2 | 1 | 5.17 | 8.38 | 1.62 | 192 / 244 | 118 / 158 | Completed 506 |
| mongo-forward w2 | 4 | 12.27 | 21.98 | 1.79 | 323 / 421 | 181 / 269 | Completed 1301 |
| mongo-forward w2 | 16 | 38.51 | 42.90 | 1.11 | 408 / 619 | 365 / 491 | Completed 2602 |
| mongo-forward w2 | 32 | 43.55 | 39.58 | 0.91 | 728 / 925 | 800 / 1029 | Completed 2394 |
| mongo-forward w2 | 64 | 43.62 | 42.48 | 0.97 | 1456 / 1865 | 1496 / 1938 | Completed 2562 |
| mongo-forward w2 | 128 | 42.62 | 45.28 | 1.06 | 2968 / 3969 | 2852 / 3149 | Completed 2804 |
| mongo-forward w16 | 1 | 5.48 | 8.56 | 1.56 | 179 / 241 | 115 / 156 | Completed 516 |
| mongo-forward w16 | 4 | 13.62 | 24.32 | 1.79 | 295 / 397 | 161 / 230 | Completed 1463 |
| mongo-forward w16 | 16 | 43.16 | 79.08 | 1.83 | 365 / 512 | 193 / 312 | Completed 4843 |
| mongo-forward w16 | 32 | 46.37 | 143.18 | 3.09 | 682 / 837 | 213 / 406 | Completed 8556 |
| mongo-forward w16 | 64 | 45.73 | 129.92 | 2.84 | 1386 / 1718 | 392 / 926 | Completed 8221 |
| mongo-forward w16 | 128 | 45.17 | 147.88 | 3.27 | 2821 / 3289 | 793 / 1355 | Completed 9149 |

### 2.5 过载档（c128）终态

| 场景 | 基线 | 新版本 | 内存盘 |
| --- | --- | --- | --- |
| native-forward | Completed 462, Compensated 10, Failed 93, Manual 1（核对未通过）；Completed 470, Compensated 8, Failed 101 | Completed 229, Compensated 56, Failed 87, Manual 2（核对未通过）；Completed 240, Compensated 41, Failed 100 | Completed 674, Failed 80 |
| native-compensate | Compensated 247, Failed 183, Manual 3（核对未通过）；Compensated 265, Failed 174 | Compensated 92, Failed 155, Manual 38（核对未通过）；Compensated 170, Failed 151 | Compensated 421, Failed 78 |
| mongo-forward w2 | Completed 2627；Completed 2622 | Completed 1149；Completed 1152 | Completed 2804 |
| mongo-forward w16 | Completed 2688；Completed 2850 | Completed 1408；Completed 1536 | Completed 9149 |

原生步骤 c128 两边都过载（P99 30s 以上，步骤超时后出现 Failed，ManualRequired 让账户核对不通过）。新版本在共享盘上的过载更重（补偿场景 ManualRequired 38 / 0，基线 3 / 0），与它在共享盘上饱和点更低一致；内存盘上没有 ManualRequired。Mongo 步骤 c128 两边都没有出错。

## 3. 定位：时间花在哪里

逐跳时间线（`ROOST_SCAN_HOPS=1`，每档 30s，只用于定位）：

| 跳（均值 ms） | 基线 w2 c16 | 新·共享盘 w2 c16 | 新·内存盘 w2 c16 | 新·共享盘 w16 c32 |
| --- | ---: | ---: | ---: | ---: |
| 稳态 TPS | 40.84 | 11.88 | 42.52 | 18.24 |
| 端到端 P50 | 382 | 1322 | 377 | 1689 |
| `start_commit`（启动） | 17.3 | 51.6 | 1.7 | 52.2 |
| `coordinator_wait_and_claim` | 81.6 | 73.1 | 1.8 | 108.4 |
| `dispatch_commit`（派发） | 35.2 | 99.4 | 1.5 | 99.5 |
| `outbox_wait`（发布器排队） | 34.2 | 43.2 | 1.9 | **1061.3** |
| `command_transport` | 49.6 | 61.3 | 206.3 | 57.7 |
| `step_reserve` + `receipt_and_commit`（Mongo 步骤侧） | 66.7 | 77.8 | 68.4 | 80.2 |
| `result_transport`（结果在 JetStream 排队） | 44.5 | **656.4** | 48.9 | 61.1 |
| `result_consumer_slot` | 5.5 | 82.3 | 0.4 | 0.1 |
| `completion_apply`（接收结果） | 34.5 | 165.0 | 2.3 | 155.8 |

（[时间线文件](benchmarks/saga-redis-authority-2ab-20261027/timeline/)；c4 档同样方向：新·共享盘 P50 460ms、`completion_apply` 99ms，内存盘 P50 188ms，基线 327ms。）

- 新版本共享盘上，每个经过 Redis 脚本的跳是 50～165ms，是一次脚本往返等 1～数个落盘周期（`completion_apply` 还含一次 HMGET）；内存盘上同样的跳是 1.5～2.3ms。
- worker 2：结果在 JetStream 里排队 656ms、`result_consumer_slot` 82ms，饱和的是结果消费者（每个结果 HMGET + Apply 两次往返，`completion_apply` 165ms）。基线同一位置约 35ms，worker 2 时饱和点在步骤消费者。
- worker 16：结果消费者不再是瓶颈，4 个发布器每条命令“outbox 领取 → 发布 → 确认”两次脚本往返，命令在 outbox 里排队 1061ms。基线 worker 16 的饱和点也是发布器（每条 3 次 Mongo 串行，约 43 TPS）。
- 内存盘上协调器各跳 1.5～2.3ms，瓶颈回到 Mongo 步骤消费者（`command_transport` 206ms 是命令在步骤消费者前排队），所以 w2 饱和与基线持平（v2 §8.6 预期“基本不变”）；w16 时饱和约 143～148 TPS。

**Redis 自身的延迟事件**（压测期间每 20s 读 `LATENCY LATEST`，阈值 5ms，[采样](benchmarks/saga-redis-authority-2ab-20261027/redis-latency-samples.txt)）：

| 轮次 | `aof-fsync-always` 中位 / 最大 ms | `aof-write` 中位 / 最大 ms |
| --- | --- | --- |
| 新 s1 w2 | 16 / 37 | 10 / 32 |
| 新 s1 w16 | 17 / 41 | 9 / 20 |
| 新 s2 w2 | 17 / 103 | 10 / 38 |
| 新 s2 w16 | 18 / 57 | 11 / 48 |
| 内存盘 s1 w2 / w16 | 只有一次 13ms 事件，其余都低于 5ms | 无 |

**F_FULLFSYNC 微基准**（空闲时，同一 APFS 卷，256 字节追加 + 刷盘 300 次，[源码](benchmarks/saga-redis-authority-2ab-20261027/fsync-microbench.go.txt)）：`~/.roost-it` 卷 P50 4.0ms、P99 6～8ms；内存盘 P50 0.1～0.15ms。Go 的 `File.Sync` 在 darwin 上本身就是 F_FULLFSYNC，所以两行数字相同。

## 4. 门槛逐项（v2 §6）

### 2a

| 门槛 | 结果 | 判定 |
| --- | --- | --- |
| 单元 + 集成（真实 Redis AOF + Mongo）全绿含 `-race`；`kill -9` Redis 用例通过 | 见 v2 §13.5 | **达标** |
| 协调段 Mongo 操作 = 0（启动的业务键守卫读除外） | Mongo 步骤前台每笔 19.0 = 步骤侧 + 启动读 1；`commitTransaction` 8 → 4；`RedisStore` 只在 `Create` 守卫与冷记录装回时读 Mongo | **达标** |
| Redis 脚本数与 §3.2 一致（±10%） | 单元用例计数每笔正好 11 + 2 次 HMGET | **达标** |
| Mongo 步骤 w16 饱和吞吐 ≥ 1.5 × 基线 | 共享盘约 22 vs 45（0.47～0.52 倍）；内存盘诊断 143～148（3.1～3.3 倍） | **不达标**（正式口径） |
| Mongo 步骤 c1 P50 ≤ 0.85 × 基线 | 共享盘 w2 195 vs 192（1.02 倍）、w16 196 vs 179（1.09 倍）；内存盘 0.61 / 0.64 倍 | **不达标**（正式口径） |
| 原生正向 / 补偿饱和吞吐 ≥ 0.95 × 基线 | 共享盘 c16～c64：正向 0.78～0.83，补偿 0.84～0.87；内存盘 1.00～1.35 | **不达标**（正式口径） |
| 非过载档终态与账户核对全部通过 | c1～c64 两边全部通过，终态复读一致 | **达标** |

### 2b

| 门槛 | 结果 | 判定 |
| --- | --- | --- |
| 投影吞吐本机 ≥ 5000 条/s（P=16、批 256） | `BenchmarkSagaProjection` 20528 / 21850 / 27393 条/s（GOMAXPROCS 10），22724 / 22943（GOMAXPROCS 4） | **达标** |
| 压测期间积压不超过 2 × 在途 | 积压峰值 2～28 条；c16 起不超过并发数的 1.6 倍，c128 约 0.2 倍；c1、c4 的 Mongo 步骤档 3 / 1、10 / 4（2.5 倍） | **达标**（按笔计，维护者 2026-10-27 确认，见 §5） |
| 停止加压后 30s 内归零 | 全部档位 3～8ms | **达标** |
| 冷副本核对 0 差异 | 全部档位 0；冷副本终态数 = Redis 终态复读 | **达标** |
| 迟到成功在驱逐后能装回并补偿 | `TestRedisLateSuccessAfterEvictionReloadsAndCompensates`、`TestRedisLateAlarmReloadsAnEvictedRecord` 通过 | **达标** |

## 5. 解释与限制

- **积压的单位**：一笔两步 saga 在生命周期里产生约 5 条日志（启动 1、派发 2、完成 2），积压按条数采样，在途按笔数，两者单位不同。c1 时 1 笔在途、积压 3 条，是同一笔的几次状态变化还没投影完（批间隔 100ms 的 `XREAD BLOCK`）。维护者 2026-10-27 确认门槛按笔计：未投影日志涉及的 saga 笔数 ≤ 2 × 在途笔数，没有按笔指标时用“条数 ÷ 每笔条数”折算（与“条数 ≤ 2 × 在途 × 每笔条数”等价）。按此全部档位 ≤ 0.6 倍，达标；原先按字面“条数 ≤ 2 × 笔数”c1 / c4 略超，见 v2 §6、§13.6。
- **只在 macOS 上测**：Linux 的 `appendfsync always` 是 `fdatasync`，代价通常比 F_FULLFSYNC 低，但本轮没有 Linux 机器，**没有验证**，不能把内存盘的数字当成 Linux 的预期。
- **权威 Redis 和 Mongo 同盘**：正式口径刻意沿用 v2 §8.1 的环境（同机同盘），落盘相互干扰是其中一部分（空闲 4ms → 压测中 16～18ms）。v2 §11 第 1 项推荐的独立实例在本机没有独立物理盘可测。
- **单样本的内存盘轮**：只有一轮，作为方向判断；两轮的正式口径之间，原生步骤 c64 两轮差 10～20%（宿主后台负载），比值按两轮均值算。
- **过载档**：c128 原生步骤两边都过载，不参与门槛判定；新版本在共享盘上的 ManualRequired 更多，是饱和点更低的结果，没有发现账户或终态不一致（核对失败只来自 ManualRequired 计数）。

## 6. 方向判断与待决定

2a 的设计目标（减往返、去掉协调器的 Mongo 事务）在计数上全部兑现，但“每个脚本 = 一次 F_FULLFSYNC，且 Redis 单线程在落盘期间不处理任何命令”让每次往返在本机从 Mongo 的 17～35ms 变成 30～165ms。2c 把每步脚本从 5 个降到 2 个，正好对着这一项；第 4 步的原生步骤收益同样依赖每次写的落盘代价。

| 选项 | 内容 | 代价 |
| --- | --- | --- |
| **A（推荐）** | 2a / 2b 合入 main；2a 的性能门槛改在 Linux、权威 Redis 独占一块盘的条件下复测判定；同时开始 2c | 合入后在 macOS 开发机上 Saga 吞吐低于现在（Mongo 步骤约 0.4～0.5 倍，原生约 0.8 倍）；需要准备一台 Linux 压测机 |
| B | 不合入，2c 在本分支上继续，2c 完成后按 2c 门槛在同样条件下一起测 | 分支继续悬挂、rebase 成本；如果本机口径下 2c 仍达不到 2 倍，问题只是推后 |
| C | 放宽持久性（`appendfsync everysec` 或去掉 WAITAOF） | 违背维护者“AOF always + 同连接 WAITAOF”的决定，确认的写可能在崩溃时丢，**不推荐**，只为完整列出 |

不论选哪项，v2 §8.1 的环境都应写明权威 Redis 的数据盘（是否与 Mongo 同盘、是否 macOS），否则门槛数字不可比。
