# JetStream 独立、Mongo混合与资源对照（2026-10-23）

结果支持：此前 Saga 中30余毫秒的消息交付不是 JetStream 独立运行的基础延迟。相同本机三副本服务，独立运行平均约1.4ms；只加入同机 Mongo 持久事务，独立进程/连接/流的探针也升到约43ms，停止事务后恢复。已经排除“只有Saga自身调度/共享连接/回调阻塞才会导致退化”的解释。尚未分离磁盘同步等待、内核调度、电源策略的各自贡献。

## 参数与边界

基于 `0ff5014c` 加本轮性能夹具；macOS、电池供电、10逻辑CPU、32GiB内存。NATS server 2.14.5 / nats.go 1.53.1，三个NATS和三个Mongo节点同机、同一内置SSD。进程GOMAXPROCS=4沿用Saga诊断口径，不代表Nest业务池配置。Mongo使用正式驱动majority+journal，未削弱可靠性；NATS仍是现有三副本文件流，未修改服务器配置。

探针走正式 `ndriver.JetStreamClient.Publish/Subscribe`，每次发布等待PubAck，消费立即返回由驱动ACK。MaxAckPending=256、AckWait=30s、MaxDeliver=25000，流容量512MiB。1KiB固定载荷包含唯一编号，不是Saga真实JSON内容；探针移除了业务处理，目的是隔离总线。不是跨机、故障或长稳验收。现有服务器监控报告sync_interval=120s；本报告的PubAck不额外宣称“每条消息物理fsync”，也没有改动该配置。

所有样本逐编号核对接收，重复/错误/遗漏均为0；等待服务端consumer NumPending和NumAckPending归零。单进程单调时钟记录发布→回调与PubAck；不会把两者相加。不同进程的混合窗口用墙钟对齐。编译、race与容量采样分开。

## 独立与混合延迟

低负载每轮120消息/s × 30秒，共3600条；4个同步发布worker。表内均值和P99均为消息级。

| 场景 | 交付均值 ms | 交付P99 ms | PubAck均值 ms |
| --- | ---: | ---: | ---: |
| 独立，混合前 | 1.35 | 3.66 | 0.84 |
| 同机Saga 30笔/s | 42.71 | 92.77 | 16.96 |
| Saga停止后 | 1.52 | 7.85 | 1.16 |
| 仅Mongo 240事务/s | 42.84 | 91.56 | 18.06 |
| Mongo停止后 | 1.41 | 3.33 | 0.88 |

Saga背景负载1800/1800成功，业务核验通过，服务端P99约550.3ms。Mongo-only是独立进程、8worker、每事务插入2条独立文档，不运行Saga、不建立NATS连接；14400笔提交成功、28800条文档核对一致，实测239.94事务/s。240事务/s对应两步Saga每笔8次事务提交的量级，**不是完整重放Saga的查询/写入模式**。

## 短时吞吐

以下是15秒有界并发闭环（每worker等PubAck再发下一条），不设输入速率，接收与ACK排空计入耗时。没有把闭环数据当开环排队P99。

| 同步发布worker | 成功消息数 | 完成消息/s | 交付均值 ms | 交付P99 ms |
| --- | ---: | ---: | ---: | ---: |
| 4 | 182514 | 12166 | 0.46 | 0.78 |
| 32 | 601966 | 40111 | 1.13 | 5.03 |
| 128 | 971961 | 64738 | 2.67 | 11.60 |

三档均逐条收到且ACK排空，未测到吞吐平台，所以64738是本夹具已测能力，**不是上限**。高负载下均值比低负载小可能受批处理/调度影响，本轮没有拆因。不能将消息/s换算成Saga事务/s；无需据此提高Saga worker默认值。

## 混合期间 CPU / 网络 / 内存 / 磁盘

另做110秒连续120消息/s探针，中间60秒加入Mongo-only 240事务/s，采集115秒、每秒一次。13200条全部收到，14400笔Mongo事务和28800条文档核验成功。取稳定窗口：开始后5秒至Mongo启动前2秒；Mongo启动后5秒至结束前5秒；结束后5秒至探针结束前2秒。按发布秒分桶计算加权交付均值；资源采样是同窗口秒级均值。不能把“各秒P99最大值”当整体P99。

| 指标 | 负载前 | Mongo混合 | 负载后 |
| --- | ---: | ---: | ---: |
| 消息交付均值 | 1.68ms | 53.77ms | 1.55ms |
| PubAck均值 | 1.04ms | 20.43ms | 1.07ms |
| 整机CPU忙碌 | 31.3% | 41.8%（秒峰49%） | 31.5% |
| 三个Mongo CPU合计，100%=1核 | 8.2% | 126.5%（约1.26核） | 7.8% |
| 三个NATS CPU合计，100%=1核 | 63.7% | 36.4% | 65.3% |
| 回环接收流量 | 1.49MiB/s | 4.01MiB/s | 1.49MiB/s |
| SSD写吞吐 | 0.70MiB/s | 36.35MiB/s（秒峰65.48） | 2.43MiB/s |
| SSD写IOPS | 109 | 6512 | 177 |
| 驱动层每次写IO平均耗时¹ | 0.184ms | 0.021ms | 0.101ms |
| Swap-out增量 | 0 | 0 | 0 |
| Mongo RSS合计 | 1179.6MiB | 1189.3MiB | 1191.4MiB |
| NATS RSS合计 | 2176.4MiB | 2176.4MiB | 2176.4MiB |

¹ 来自IORegistry累计Total Time (Write)与Operations (Write)的相邻差值，先算每秒平均再汇总。**不是fsync延迟、不是磁盘利用率，也不包含所有文件系统/持久确认等待**。负载变成大量小写后该均值反而降低，不能只凭IOPS上升宣称磁盘已饱和。

判断：

- **CPU**：没有整机饱和证据；三个Mongo各自平均约58%/34%/34%单核。基线已有桌面及监控活动，且电池模式下低CPU占用不保证没有调度或降频影响。
- **网络**：业务通信主要是本机回环；混合期约4MiB/s、无链路错误增量。en0收/发平均约0.004/0.012MiB/s，未见带宽耗尽。该结果不验证跨机网卡。
- **内存**：无新增swap-out，混合期swap-in也为0；压缩内存约4680MiB且稳定，进程RSS基本稳定。没有本轮内存抖动引发退化的证据，不把现存swap占用当作正在换页。
- **持久I/O路径**：Mongo负载开关与写IOPS、消息延迟同步变化，是当前优先排查方向。磁盘带宽与驱动级均值却未显示耗尽；需进一步看journal/fsync/flush等待、文件系统和调度，才能确定具体瓶颈。没有证据支持改用TCP或降低持久保障。

资源采样本身及桌面活动会产生扰动：本轮混合53.77ms高于未采样约42.84ms，不能把二者差值全部归因于采样。始终用同轮前/中/后比较。原始采样含其它进程路径，仅保存在本地忽略目录；公开摘要只保留数据库、NATS与系统指标。

## 复跑与验证

加载私有 `env.sh` 后，运行：

```sh
# 独立、闭环吞吐与 Mongo 事务混合对照（一次跑完）
ROOST_PERF_LABEL=<唯一名称> bash scripts/perf/jetstream-probe.sh
# macOS资源采样：110秒连续探针，中间60秒Mongo背景负载
ROOST_PERF_LABEL=<另一个唯一名称> bash scripts/perf/jetstream-resources.sh
```

**2026-10-29 起脚本只剩 Mongo 背景**：上表“同机Saga 30笔/s”一行（`saga-mixed.json`、`mixed.json`）用的 Saga 混合背景负载跑的是旧协调器的压测夹具 `TestRealSagaPerformance`，它随 saga JetStream 切换第 4 批（`298e4396`）删除；之后该分支匹配不到用例、静默空跑，已从脚本删除。这一行与对应 JSON 只作历史样本，不能用当前代码复跑。现在脚本依次跑独立（`alone-before`）、闭环三档（`closed-*`）、Mongo 混合（`mongo-mixed` + `mongo.json`）与停止后（`alone-after`），并核对每段用例确实运行。saga 正式实现的压测入口是 `scripts/perf/saga-js-scan.sh`（见 [性能报告目录](../../README.md)）。

入口独占 `remote-acceptance.lock`、编译后再采样，测试只创建并清理唯一流/数据库，不改原服务配置。第一轮/第二轮及资源采样分别在两份worktree的 `artifacts/perf/saga/jetstream-20261023`、`jetstream-mongo-20261023`、`jetstream-resources-20261023`。原始日志、每秒计数、源码摘要均保留，同目录JSON为可携带结果。CBM仍为10-14旧代际，依覆盖提示读当前源码。

已执行：正式驱动探针真实三副本测试；Mongo/Saga背景负载校验；探针短时race；integration标签Saga vet；根包门禁。资源采样115行无错误，iostat首行开机累计均值已剔除。只新增诊断夹具/脚本和文档，生产默认配置未改。

## 持久化等待进一步定位（同日11:15重跑）

继续使用110秒JetStream探针、中间60秒Mongo 240事务/s，保持三副本和持久化设置。每秒读取三个Mongo的WiredTiger log计数，负载前与期间分别用macOS `sample <pid> 1 5`采样六个服务；NATS剥离符号的地址用现有二进制的Go pclntab离线解析。采样只用于定位，不以这轮时延替代未采样容量成绩。

有效重跑13200条消息全部收到并ACK排空，14400笔事务成功、28800条文档核对一致。115个状态样本连续，最大间隔约1.002秒。阶段稳定窗口由实际启动/结束时间划定，详见 `wait-summary.json`。

| 观测项 | 负载前 | Mongo混合 | 停止后 |
| --- | ---: | ---: | ---: |
| JetStream交付均值 | 1.84ms | 46.74ms | 1.42ms |
| PubAck均值 | 1.44ms | 20.83ms | 1.07ms |
| Mongo每节点journal sync次数/s | 0.2～0.8 | 94.8 / 96.5 / 105.5 | 0.13～0.23 |
| Mongo每次journal sync平均耗时 | 基线次数太少，不作容量比较 | 10.07 / 9.98 / 9.18ms | 次数太少 |
| 各节点累计sync耗时/观察墙钟 | <0.6% | 95.5% / 96.4% / 96.9% | <0.2% |
| JournalFlusher线程样本位于日志同步fcntl路径 | 0～0.6% | 94.7%～95.8% | 未采栈 |
| 每个NATS平均驻留pwrite的线程数¹ | 0.006～0.053 | 2.17 / 2.30 / 2.37 | 未采栈 |

¹ 将调用树内pwrite叶子样本数相加，再除以采样轮数，可大于1；不是CPU利用率、单请求耗时或全进程线程占比。不使用sample末尾“仅列出≥5次”的摘要，否则会把基线的小样本误记为零。分别为382/176、404/176、413/174；基线1/171、8/172、9/169。

Mongo完整调用链在原始样本中为：`JournalFlusher::run → WiredTigerRecoveryUnit::waitUntilDurable → WiredTigerSessionCache::waitUntilDurable → __session_log_flush → __wt_log_force_sync → __log_fsync_file → __posix_file_sync → fcntl → __fcntl`。WiredTiger同步累计计数与线程驻留样本相互印证：journal线程大部分时间花在同步路径，**不是消耗了95%的CPU**。各节点flowControl等待增量为0。

NATS混合期多条OS线程停在内核`pwrite`；基线仅偶尔出现。Go线程采样不能完整恢复所有goroutine调用链，尚未分清具体stream文件与Raft文件。结合前轮CPU/网络/内存未饱和、Mongo独立负载开关会使NATS延迟升高/恢复，证据将重点缩小到**同机持久化同步和文件写入相互影响**。目前没有证据支持把延迟归因于Saga业务计算或NATS网络吞吐不足。

仍不能仅凭这些栈断言SSD带宽耗尽、APFS某个锁或某种设备flush策略是唯一根因；sample也不能提供每次系统调用的精确时长。下一步有价值的对照是保持相同持久语义，把Mongo与NATS放到不同物理存储/主机，或在可追踪环境测单次sync与pwrite等待。当前未做部署调整，未改journal、复制副本、同步策略或Saga worker默认值。

### 睡眠样本保留与诊断保护

第一次调用栈诊断的11:11:00～11:11:20发生macOS Idle Sleep，电源日志已确认；状态采样出现17.987秒间隔。它影响墙钟阶段与Go单调计时的对齐，因此该轮阶段汇总明确标为无效，原始数据保留在 `jetstream-waits-20261023`，旧汇总改名 `summary-unreliable-clock.json`，**没有删除或混入有效结果**。

有效重跑是 `jetstream-waits-awake-20261023`。入口仅在测试生命周期持有 `caffeinate -i` 空闲睡眠断言，退出自动释放，不修改系统电源设置；机器仍使用电池。两个资源/等待分析器现在拒绝倒退或超过3秒的采样间隔，不静默跨过缺口。已用无效原始数据验证拒绝，用连续重跑验证汇总成功。采样间隔正常不等于绝对排除了所有调度或电源影响。

复跑：source私有env后，`ROOST_PERF_LABEL=<唯一名> bash scripts/perf/jetstream-waits.sh`。要求macOS的sample/caffeinate、mongosh及当前无鉴权本地私有副本集；脚本不适用于带鉴权或远程生产集群。摘要工具为 `scripts/perf/jetstream-wait-summary.py`；离线NATS符号工具为 `scripts/perf/symbolize-nats-sample.py`，不需要重启服务或开启pprof。原始计数、样本、源码摘要保留在两份worktree的上述artifacts目录。

## 当前决定

维护者决定保留Mongo事务与 `majority + j:true`，不改变生产默认，不再追加本地压测。迁往实际Mongo服务后再验收总业务负载和延迟；本报告的同机数据不作为该服务的容量上限。当前已完成诊断和复跑工具交付，具体存储层机制与跨机复测保留为后续条件具备时的验证项。
