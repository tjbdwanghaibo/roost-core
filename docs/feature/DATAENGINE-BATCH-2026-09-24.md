# DataEngine 剩余优先项：实施与验收

状态：本地 DataEngine 本轮剩余优先项已完成并验证，可转入维护，未发布。

用户已授权实施。范围：本地 Entity 的多 DAO 批量投影、成功前缀 checkpoint 合并、积压准入与告警；Remote 扩展验证继续留后续。基线与原性能见 [压力报告](DATAENGINE-PRESSURE-2026-09-24.md)。

## 结构与兼容性

保持 dataengine / dataengine/engine 两个核心包，不增加包或并行投影队列。

| 文件 | 实施内容 |
| --- | --- |
| engine/projection_plan.go | 复用记录数/逻辑字节上限，Store 显式声明多 mutation 批量能力后才合并本地多 DAO 记录 |
| engine/mongo_projection.go | 每个业务保持原 TransactionID/digest；单个 Mongo 事务中按 WAL 顺序构建各集合 ordered bulk，最后一并写入业务事务 markers |
| engine/projector_replay.go | 有界合并连续成功前缀 ack；held/失败/取消前只确认已成功记录，Remote/receipt/effect/migration 前后仍独立确认 |
| engine/projector.go / health.go | 原有准入锁内原子预留容量；可配置未 ack 条数上限、预警阈值、拒绝统计和健康消息 |
| kit/dataengine/mod.go | 暴露相应启动配置并拒绝非法阈值；旧配置默认不启用新的业务限流 |

原 BatchProjectionStore 仍只承诺单 mutation；新增可选能力声明，未声明的外部 Store 不会收到多 mutation 批次。MongoStore 实现新能力。WAL/BSON 格式、公开提交成功承诺与每个业务事务的 ID 不变。

## 行为边界

- 普通本地单/多 DAO 可以共享一次 Mongo transaction；所有被合并业务一并可见，这是更强的批次原子性。每业务仍有独立 marker，重启可单独识别，不能把多个业务当成一个 ID。
- 相同文档在不同业务中的 mutation 保持 WAL 先后顺序；一笔业务内部重复文档键继续由原验证拒绝。
- 不合并 Remote、receipt、effect、migration。真实冲突时整个 Mongo 批次必须回滚，然后降级逐笔投影，确认正确前缀、在真实失败处停止；不能吞掉永久冲突，也不能让后续事务越过它。
- 仅具备持久事务 marker 的本地路径按有界数量/时间阈值合并 checkpoint；单 DAO 的单笔快路及未知 Store 仍立即确认。不会等待凑批。设数量为 1 可恢复逐单元确认。合并不改变 WAL 写入 fsync 和 durable ticket，只扩大“Mongo 成功但 checkpoint 未确认”的可重放窗口。失败/held/批次结束均尝试确认成功前缀，ack 失败时保留 WAL，依靠幂等重放。
- 积压上限拒绝发生于写 WAL 前，属于可重试的背压，不触发 fatal。Nest 必须回滚本事务所有 DAO 与版本，投影继续运行并释放容量。System 提交同样遵守限制，避免遗留等待票据。默认阈值为 0，不替业务设定任意吞吐配额。

## 实施与验收

1. 改分段与 Mongo 批量事务，覆盖多集合、同文档连续版本、事务身份、晚冲突整批回滚、已提交丢 ack 重放，以及旧 Store 兼容。
2. 改 checkpoint 聚合，覆盖跨段、数量/字节边界、held、取消、批量降级、中段失败、ack 失败；保留逐单元模式的旧承诺测试。
3. 接准入限制、Kit 配置和健康预警，覆盖并发预约、拒绝不写 WAL、失败归还额度、恢复后再准入、三种 Nest 策略下四 DAO 回滚。
4. 相关包 race/vet、真实 Mongo 故障与三进程生成链路验证；按上轮同机同参数复跑 36 样本，再验证 20/s、100/s。测最终落库而不是仅看 Request 返回。

单项可分别退回旧能力声明、checkpoint_records=1 或阈值=0；无需迁移旧 WAL 或数据库。性能失败时继续定位，不降低持久化承诺。任意点断电、目标 Linux 硬件长期容量与坏 WAL 运维工具仍属于独立部署/运维工作，不冒充本轮性能实施内容。


## 实际实现与正确性验收

三个优先项均已实施，仍保持两个核心包。MongoStore 显式声明多 mutation 批量能力；普通集合按稳定顺序执行 ordered bulk，同一文档保持 WAL 版本顺序，每笔业务独立持久 marker。字节/记录上限复用既有分段，不新增后台并行队列。

checkpoint 默认 256 条 / 20ms，在存储单元结束处检查并在本次重放末尾确认，不等待凑批。一次 Mongo 原子批量可以跨过数量阈值，阈值不能拆开已提交单元；窗口总量仍受 ReplayBatchRecords/ReplayBatchBytes 约束。单 mutation 的 Project 快路没有完整历史 marker，仍立即 ack，避免后继版本覆盖 `_last_tx` 后无法重放旧记录；未知 Store 保留旧确认节奏。特殊事务前后各自确认。

新增 `MaxUnackedRecords` / `WarnUnackedRecords`，默认 0；准入在现有锁内原子预留，满额同步返回 `ErrProjectionBackpressure`。同步拒绝归还预约、清理 System ticket；未确定的 durable 结果仍保留恢复责任。ACK 成功后归还容量。Kit 读取配置并拒绝非法阈值，Stats / health 暴露积压预警与拒绝次数。具体配置见 [Kit 指南](../../kit/README.md)。

覆盖已知边界：

- 旧 Store 能力兼容、多 mutation 中任意 Remote 排除，receipt/effect/migration 分界；数量/字节边界、checkpoint 数量/时间阈值、逐单元模式、单 DAO 快路安全边界。
- 成功前缀后遇失败、held、取消或 ack 失败；整批回滚后逐笔定位冲突。并发 64 次准入、上限 8，只有 8 次写 WAL；System/Enqueue 满额拒绝、失败清理与 drain 后再次准入。
- 真实 Mongo 的双实体/四 DAO 连续版本、每笔事务 marker、已提交丢 checkpoint 重开、不同 digest 拒绝；第三笔最后一个 DAO 冲突时，整批先回滚，再只确认前两笔，所有 DAO 停在版本 2，后续 WAL 保留。
- 正式生成 DAO → Nest RequestMulti → 文件 WAL → 真实 Mongo 的三进程验收通过，涵盖三种 durability。新增每种策略满额拒绝后四 DAO 的值、版本通过内存与最终落库校验，释放容量后能继续提交。原错误/panic、丢 ack 与晚冲突验证保留；夹具现在透传实际多 DAO Batch 能力。

本轮实际复现并修复两个历史 bug：[RR-14](../bugfix/RR-20260924-14.md) 事务内 Put 重复键错误地查询已中止 session；[RR-15](../bugfix/RR-20260924-15.md) 同批重复 ID 覆盖 digest。各自保留修前证据和修复记录。

9 包 race 回归、vet 通过；完整 `TestReal*` 真实集成通过（50.533s），包括 Mongo 主切换、NATS 中断、事务/receipt/effect、混合重放。100k 积压用例需要独立开启，不计入这次全套通过数量。日志：`/tmp/dataengine-batch-race.log`、`/tmp/dataengine-batch-last-unit.log`、`/tmp/dataengine-batch-vet.log`、`/tmp/dataengine-batch-real-green.log`、`/tmp/dataengine-batch-generated-final.log`。

## 同机同参数性能复测

复用上轮相同正式生成入口、128 Entity / 32 请求方 / GOMAXPROCS=4。single 每样本 4,096 笔，其余 512 笔；三策略各三轮，共 36 样本、50,688 笔计时业务。所有样本逐 DAO 校验通过，最终 WAL=0、ProjectionFailures=0、测试库清理通过。WAL fsync、Mongo majority+journal、事务 read concern 和客户端 API 均保持原值。

以下为每组三轮的中位数，p99 也是各轮 p99 的中位数；吞吐包含最终排空，不能解释为线上容量保证。ack 次数为本轮业务阶段，不含初始化。

| 负载 | 策略 | 之前最终落库/s | 现在最终落库/s | 比值 | Request p99 ms | record→Mongo p99 ms | ack 次数 |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| single | async | 1238.2 | 1210.4 | 0.98× | 144.4 | 56.0 | 111 |
| single | strict | 685.3 | 685.9 | 1.00× | 208.0 | 57.8 | 174 |
| single | pipelined | 4996.4 | 4973.6 | 1.00× | 12.2 | 70.9 | 21 |
| dual | async | 49.6 | 1177.7 | 23.76× | 112.9 | 65.9 | 13 |
| dual | strict | 48.2 | 700.4 | 14.54× | 159.9 | 64.0 | 21 |
| dual | pipelined | 49.1 | 3524.8 | 71.85× | 12.7 | 72.5 | 4 |
| pair | async | 50.2 | 1294.4 | 25.81× | 94.5 | 61.0 | 14 |
| pair | strict | 48.3 | 622.8 | 12.89× | 188.6 | 61.2 | 23 |
| pair | pipelined | 48.6 | 2845.6 | 58.50× | 15.7 | 101.1 | 4 |
| hot | async | 48.3 | 198.4 | 4.11× | 231.9 | 56.0 | 98 |
| hot | strict | 46.1 | 124.6 | 2.70× | 322.0 | 59.2 | 120 |
| hot | pipelined | 49.2 | 2241.3 | 45.59× | 21.5 | 101.1 | 5 |

多 DAO 批量显著降低 Mongo 与 checkpoint 次数。pair/pipelined 从每 512 笔 512 次 ack 降为中位数 4 次，最终落库吞吐约 58.5 倍；单 DAO 基本持平。hot 的 async/strict 只有两个 Entity，前段锁与每笔 durable 等待仍限制吞吐，不能把它当作分散负载。突发多 DAO 的 record→Mongo p99 约 56～101ms，并非所有场景都达到 50ms 落库。

原始结果：`artifacts/perf/dataengine/batch-single-20260924/`、`batch-multi-20260924/`，每份 JSON 与生成/验证/清理日志保留；父脚本汇总日志在 `/tmp/dataengine-batch-perf-*.log`。本轮未在运行中修改性能脚本或压力夹具。以前的压力报告保留为前后对比基线。


固定输入补充，同一 pair/strict / 128 Entity / 四 DAO：

| 输入 | 笔数/发送时长 | 最终落库/s | Request p99 | 预定发送→回复 p99 | record→Mongo p99 | 未 ack 采样峰值/结束值 | 末尾排空 |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 20/s | 1200 / 59.955s | 20.01 | 7.123ms | 7.573ms | 20.611ms | 2 / 1 | 18.18ms |
| 100/s | 600 / 5.995s | 99.60 | 16.002ms | 16.127ms | 53.838ms | 10 / 7 | 29.36ms |

20/s 一分钟负载保持原有低延迟；相同 600 笔的 100/s 输入已能跟上，结束积压从上轮 352 降至 7，排空从约 7.5 秒降至 29ms。上轮 100/s 诊断带 CPU profile，本轮不带，因此不把这个延迟比值当作严格无扰动的加速比；主要吞吐改善以同参数 36 样本矩阵为依据。这仍是短时验收，不等于生产长期负载保证。记录位于 `batch-paced20-20260924/`、`batch-paced100-20260924/`。

复跑矩阵：

```sh
source /tmp/roost-dataengine-it/env.sh
export GOCACHE=/tmp/roost-nest-go-cache GOWORK=off
# 标签须取新名字；两组顺序运行，避免相互争用本机副本集。
ROOST_PERF_SHAPES=single ROOST_PERF_REQUESTS=4096 ROOST_PERF_COUNT=3 \
  ROOST_PERF_LABEL=local-single-new bash scripts/perf/dataengine.sh
ROOST_PERF_SHAPES='dual pair hot' ROOST_PERF_REQUESTS=512 ROOST_PERF_COUNT=3 \
  ROOST_PERF_LABEL=local-multi-new bash scripts/perf/dataengine.sh
ROOST_PERF_SHAPES=pair ROOST_PERF_POLICIES=strict ROOST_PERF_REQUESTS=600 \
  ROOST_PERF_RATE=100 ROOST_PERF_COUNT=1 ROOST_PERF_LABEL=local-paced-new \
  bash scripts/perf/dataengine.sh
```


## 大积压补验与收口

最后独立执行 `ROOST_DATAENGINE_IT_BACKLOG=1 ROOST_DATAENGINE_IT_BACKLOG_MULTI=1` 的真实 Mongo/race 验收：100,000 条事务、10,000 Entity、每 Entity 两个 DAO，跨 32 个 WAL 文件段。慢 Store 取消后所有记录保留，恢复投影 23.350s（4,283 事务/s，391 次 checkpoint），两集合共 20,000 份文档版本全部为 10，抽查字段值正确，再次重开 WAL pending=0。该数字带 race，属于恢复诊断，不与无 race 的业务压力矩阵混作容量对比。日志 `/tmp/dataengine-batch-backlog.log`。

```sh
source /tmp/roost-dataengine-it/env.sh
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off \
ROOST_DATAENGINE_IT_BACKLOG=1 ROOST_DATAENGINE_IT_BACKLOG_MULTI=1 \
  go test -race -tags=integration ./kit/dataengine \
  -run '^TestRealDataEngineLargeBacklogRecovery$' -count=1 -v -timeout=300s
```

最终九包 race 再次全部通过，包含 Nest、WAL、DataEngine、Entity、Kit 和生成器；含 integration 编译的 DataEngine/Kit vet 通过。日志 `/tmp/dataengine-batch-final-race.log`、`/tmp/dataengine-batch-final-vet.log`。真实 `TestReal*` 共 14 个顶层测试通过，原先跳过的大积压已由上述双 DAO 专项补验。正式三进程链路和 36+2 个性能/固定输入样本的通过及测试库清理逐份检查完成。

索引刷新至 `2026-09-24T13:46:18Z`，18,041 节点 / 142,481 边；本轮 15 个运行/测试文件 coverage 为 metadata_match/no_recorded_issue，engine scope 没有已记录缺口及剩余分页。testdata/docs/scripts 按规则排除，使用直接源读取、实际生成与运行补证；不据此作全仓无遗漏声明。

**本轮三个剩余优先项与新发现的两个历史缺陷均可收口。** 本地链路维持现有提交承诺和恢复语义，不再保留“多 DAO 只能约 50 事务/s”的旧性能阻塞项。Remote 扩展验收继续按用户要求后置；生产 Linux 硬件的长期容量与延迟、真实断电/磁盘故障、历史坏 WAL 运维工具是独立部署工作。Sync 的 50ms 要求不自动等于 Mongo 的落库 SLA，实际业务持久化比例仍需由部署负载确定。
