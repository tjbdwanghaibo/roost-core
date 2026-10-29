# Saga 正式实现（JetStream 协调器）性能验收（2026-10-29）

设计 [§12](../maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md#12-性能验收整轮--30-分钟) 的验收轮。入口 [saga-js-scan.sh](../../scripts/perf/saga-js-scan.sh)，用例 `framework/saga` 的 `TestRealSagaJSScanLevel`（`-tags integration`）。样本在 [benchmarks/saga-jetstream-authority-20261029](benchmarks/saga-jetstream-authority-20261029/)，原始日志与 pprof 留在本机 `artifacts/perf/saga-js/saga-js-20261029*`（不随仓）。

## 0. 结论

- **最终轮（main `65cda8c9`，生产推荐配置）全部达标**：原生步骤正向 c128 **1047 / 1053 笔/s**（判据 ≥ 900），c1 端到端 P50 23.6ms（≤ 40），c16 256（≥ 179）；准入档关 6 次、重开 6 次，积压峰值 5868 = 1.17 × 高水位（≤ 1.3）；15 档非期望终态、余额不符、发布结果未知全部 0，停止加压后积压 ≤ 4.8s 归零。整轮约 8 分钟。
- **第一轮没达标的原因不是正式实现退化**：c64～c128 封顶约 500 笔/s。同机交替对照原型 c128 1573 / 1659、正式 492 / 496（c16 两边相同），定位到参与方 Nest short 池：strict 提交在 short worker 上锁内等 group commit 的 fsync（[NEST_TRANSACTION_WAL](../framework/NEST_TRANSACTION_WAL.md) 写明的豁免），吞吐 ≈ worker 数 ÷ fsync 时长（本机约 8ms）。压测夹具只给了 8 个 worker（原型 64），生产缺省（`nest.short.workers=0` = GOMAXPROCS）只有 4 个。
- **维护者决定（2026-10-29）**：只给 saga 原生步骤 handler 用 `durability=pipelined` 并打开 Nest 异步完成，其他 handler 保持原缺省（async），不改 Nest 池缺省、不建 saga 专用池（`65cda8c9`，契约见 [SAGA.md「原生步骤的提交方式」](../framework/SAGA.md)）。同一最终轮里，生产缺省 4 个 short worker：strict 254 笔/s、short 池 100% 占满、别的实体每次多等 worker 约 100ms；pipelined + 异步完成 1047～1053 笔/s、short 池忙 4%、多等约 25µs。

## 1. 口径

本机 macOS（Apple Silicon，交流电），GOMAXPROCS=4；私有三节点 NATS 2.14.5（R3、文件存储）、私有 mongod 8.0 单节点副本集（`transactionLifetimeLimitSeconds=20`，SAGA.md 的部署要求）；协调器 P=16、G=4、R3，冷副本关；参与方是正式路径（Nest + nestwal WAL + DataEngine 投影 8 通道 + 实体内回执 + `SubscribeNativeStep` / `SubscribeMongoStep`），256 个账户随机转账；闭环并发，每档 30s，稳态窗口 [5s, 30s)，单样本为主。正确性是硬门槛：每档非期望终态、余额（Nest 内存与 Mongo 逐账户）、发布结果未知、积压 30s 内归零任一不为 0 则该档失败。机器同时有其他应用，负载（load1）2～7、swap 1.8～3.8 GB，每档都记在 `runs.tsv`。

**跨时段的绝对数不直接比较**：同一配置在不同时段差约 25%（strict 64 worker c128：定位时 1000～1290，最终轮 986；pipelined + 异步 4 worker：实验时 1350～1379，最终轮 1047～1053）。结论只用同一时段交替跑的对照。

## 2. 最终轮（生产推荐配置，`65cda8c9`）

| 档 | 提交方式 | short worker | 笔/s | P50 / P99 ms | short 忙 | 每作业等 worker |
| --- | --- | --- | --- | --- | --- | --- |
| 原生正向 c1 | pipelined + 异步 | 4 | 41.2 | 23.6 / 35.5 | 0 | 3µs |
| 原生正向 c16 | pipelined + 异步 | 4 | 256.2 | 60.5 / 113.0 | 1% | 2µs |
| 原生正向 c64 | pipelined + 异步 | 4 | 697.8 | 90.4 / 142.9 | 2% | 7µs |
| 原生正向 c128 | pipelined + 异步 | 4 | **1047.5** | 118.7 / 208.9 | 4% | 24µs |
| 原生正向 c128（第二样本） | pipelined + 异步 | 4 | **1053.4** | 118.7 / 190.1 | 4% | 28µs |
| 原生全补偿 c16 | pipelined + 异步 | 4 | 174.2 | 89.4 / 149.5 | 1% | 2µs |
| 原生全补偿 c128 | pipelined + 异步 | 4 | 717.3 | 176.5 / 287.9 | 5% | 44µs |
| 原生正向 c128 对照 | strict | 4 | 254.5 | 480.3 / 879.3 | **100%** | **99.5ms** |
| 原生正向 c128 对照 | strict | 64 | 986.2 | 127.4 / 198.2 | 36% | 5µs |
| Mongo 步骤 c16 | — | — | 514.1 | 29.8 / 56.5 | — | — |
| Mongo 步骤 c128 | — | — | 1090.5 | 113.5 / 208.0 | — | — |
| 快照开 c64 | pipelined + 异步 | 4 | 679.3 | 92.6 / 152.8 | 2% | 7µs |
| 准入 c128（慢投影） | pipelined + 异步 | 4 | 413.5 | 61.9 / 4852.8 | 1% | 14µs |
| 重放 1 万 / 5 万条 | — | — | 启动 0.056s / 0.198s | — | — | — |

准入档：投影每条多等 10ms 让积压真正越过高水位 5000，30s 内关闭 6 次、重开 6 次、关闭时间占 66%，拒绝 1098 次启动，积压峰值 5868，关闭期间有效终态 13.4 笔/s（只来自在途 saga）、停止后 4.8s 归零；P99 4.85s 是被准入挡下又重试成功的启动。判据“关闭时有效吞吐不为 0”成立，数值低是准入的设计结果，判据未改。

## 3. 第一轮与定位

第一轮（`47a9e85f`，参与方 strict、8 个 short worker）：原生正向 c1 40.9、c16 253、c64 500、c128 491～505 笔/s，c64 与 c128 相同而延迟翻倍（服务速率恒定约 500/s）；全补偿 c128 325，pipelined c128 483（未开异步完成），Mongo 步骤 c128 1059；准入档积压峰值 112，从未关闭（判据未被检验）。正确性全 0。

同机交替对照（`ab/`）：原型（`c565c81b`，`run-perf.sh one`，参与方投影 8 通道）c16 246、c128 1573 / 1659；正式 c16 269、c128 496 / 492。c16 相同、c128 差 3.2 倍，负载两边都在 4～7，排除宿主负载。

定位（`locate/`，同时段 c128）：

| short worker | 笔/s | WAL 记录 / fsync |
| --- | --- | --- |
| 4 | 251 | 4.00 |
| 8 | 484～490 | 7.94 |
| 16 | 852 | 13.77 |
| 32 | 987 | 15.90 |
| 64 | 979～1010 | 16.1 |
| 原型（64 worker） | 1329～1409 | 约 21 |

fsync 频率始终约 124 次/s，吞吐只取决于一次 fsync 带几条记录，而 strict 下这个数等于在等 fsync 的 worker 数。64 worker 时剖析（`08-*.pprof`，本机）每笔约 134ms：StartSaga 35ms、每步 24.7ms（其中 WAL 等待 11.7ms、结果 PubAck 11.9ms）、其余约 50ms；CPU 只用约 1.8 核，无串行锁热点；协调器每轮每分区一次发布（P=16 时每次 2.76 条事件，P=4 时 9.1 条、1096 笔/s）。与原型剩余约 20～28% 的差距：约 10% 来自 P=16（设计选择 §4.1），其余在三次同步 R3 PubAck 与正式参与方开销，未再细分。压测夹具改为与原型一致（`a314b54a`），准入档加慢投影与“关闭再重开”判据。

## 4. pipelined + 异步完成实验（`f1bfc6ed`）

同时段交替（`pipeasync/`）：strict 4 worker 242 笔/s（short 忙 100%、每作业等 worker 105ms）；pipelined + 异步完成 4 worker 1379 / 1350、8 worker 1379（short 忙 4～8%、等 worker 35～72µs、每次 fsync 22 条）；strict 64 worker 1290。异步完成下 AfterCommit 契约按源码核对并用真实端到端、参与方 kill -9（`-race`）验证，去掉回执的变异变红。据此不需要 saga 专用池：它只能隔离 worker 数，隔离不了同一实体的锁，且要给 Nest 引入按 handler 分通道的新概念。

## 5. 未验证与限制

- 只在 macOS 单机；Linux、跨机、云 Mongo 本轮未测（云 Mongo 一轮按设计 §12 不计入 30 分钟，未跑）。
- 单样本为主，时段间波动约 25%；c128 第二样本与第一个样本差 0.6%。
- 异步完成是 Nest 引擎级开关：saga 进程里用户自己声明 pipelined 的 handler 也会在完成池上跑 AfterCommit（SAGA.md 已写明）。
- `framework/saga` 真实用例的参与方零值仍是 strict，新默认靠 `ROOST_SAGA_SCAN_PIPELINED_ASYNC=1` 打开（压测脚本已打开）。
