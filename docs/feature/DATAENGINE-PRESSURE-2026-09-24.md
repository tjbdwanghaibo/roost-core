# DataEngine 正式业务压测与收尾评估

> 本文保留优化前的压力基线。后续三个优先项已实施，36 样本复测与最新收口判断见 [批量优化实施记录](DATAENGINE-BATCH-2026-09-24.md)。以下“运行时代码未改动”和待优化结论属于本次历史测量。

日期：2026-09-24。基线 `967bc69` 加当前工作树；Go 1.27.0 / darwin arm64 / Apple M5。按用户范围，本轮只压测本地 Entity，多 DAO 场景为重点，Remote 后续单独验证。运行时代码未改动。

## 1. 负载与测量边界

入口 [scripts/perf/dataengine.sh](../../scripts/perf/dataengine.sh)，复用 [正式生成链路](../../scripts/test-dataengine-generated.sh)。每个样本新生成一个工程，使用独立测试数据库和文件 WAL，独立 OS 进程执行，结束后清理本轮测试库。性能测试不带 race；测量器另做小规模 race 校验。

- GOMAXPROCS=4；32 个并发请求方，每方同时等待一笔请求；单实体用 Request，跨实体用 RequestMulti；Nest 为 8 个业务 worker、1 个心跳 worker、队列容量 256，pipelined 启用 2 个异步完成 worker / 128 队列。
- Mongo 三节点副本集和业务进程运行在同一台机器，`majority + journal=true`，事务 snapshot read concern。WAL v2，默认 10ms group commit / 500µs batch delay；没有关闭 fsync 或降低写关注来换数字。
- 默认 128 个本地 Entity，每个有真实生成的钱包与背包 DAO；热点场景缩至 2 个 Entity。初始化通过 Nest 写入并 Flush，排除在计时之外。
- `single`：单实体，只修改钱包 2 个 int64 字段，1 个 mutation；`dual`：单实体同时修改钱包与背包，2 个 mutation；`pair`：对 64 对实体轮流发放奖励，双方钱包与背包共 4 个 mutation；`hot`：所有请求争用同一对实体，仍为 4 个 mutation。
- 每份 DAO 最终都核对内存与 Mongo 的字段值、交易计数和版本；single 额外检查未修改背包仍为初始化版本。成功计数必须与 Committed/Projected 增量相等，WAL Replay 必须为空。JSON 的 final_stats 为含初始化的累计值，吞吐和延迟不含初始化。
- 本轮为小字段 Patch，不包含大背包/Blob、Remote、receipt 或 effect。Outbox 工作线程正常装配，publisher 对意外 effect 报错，未据此测量 Broker 吞吐。

度量口径：

| 指标 | 含义 |
| --- | --- |
| Request 完成 TPS / p95 / p99 | 业务请求发出至返回；不同 durability 的成功承诺按正式 API 保持，不等于 Mongo 已写入 |
| 最终落库 TPS | 请求数 ÷（开始发送至最后 Flush 完成的总时长），包含积压排空 |
| record→Mongo p95 / p99 | CommitRecord.CreatedAt 至真实 MongoStore 成功返回；包含 WAL/投影排队，不含记录构建前的 Nest 排队与加锁 |
| WAL 积压 | 全部 Request 返回时未 ack 数量，以及每次请求返回后采样的峰值；后者不是连续采样的精确最大值 |
| Mongo / ack 累计耗时 | 包装器透传真实 Store（保留 ProjectBatch）及 WAL.Ack，累加调用墙钟耗时；不是 CPU 时间 |
| B/request / GC | 业务压力加最终排空期间，进程 TotalAlloc 增量及 GC 次数，包含后台线程与测量器，非 DAO setter 单独成本 |

固定速率模式按预定时刻发起请求，额外报告预定发送至返回的 p99，避免把发送器迟到隐藏在 Request 延迟之外。仍受 32 个在途请求上限约束，达不到设定速率时不会声称是无限开环负载。

## 2. 压测结果

下表为每项 3 轮独立进程的中位数；p99 是各轮 p99 的中位数，不是合并样本分位数。single 每轮 4,096 笔；dual/pair/hot 每轮 512 笔。共计 **50,688 笔计时业务事务**，不含初始化、持续负载和诊断小样本。

| 负载 | 策略 | 最终落库 事务/s | Request p99 ms | record→Mongo p99 ms | 请求结束时未 ack |
| --- | --- | ---: | ---: | ---: | ---: |
| single | async | 1238.2 | 131.7 | 58.0 | 6 |
| single | strict | 685.3 | 210.1 | 57.4 | 5 |
| single | pipelined | 4996.4 | 13.0 | 75.2 | 234 |
| dual | async | 49.6 | 155.2 | 9620.9 | 488 |
| dual | strict | 48.2 | 199.3 | 9683.1 | 480 |
| dual | pipelined | 49.1 | 17.4 | 10156.6 | 507 |
| pair | async | 50.2 | 136.8 | 9511.1 | 487 |
| pair | strict | 48.3 | 216.5 | 9525.9 | 475 |
| pair | pipelined | 48.6 | 21.2 | 10232.2 | 506 |
| hot | async | 48.3 | 195.1 | 7774.0 | 402 |
| hot | strict | 46.1 | 315.0 | 6791.1 | 354 |
| hot | pipelined | 49.2 | 25.0 | 10003.3 | 504 |

所有表内样本逐文档校验通过、ProjectionFailures=0、最终 WAL pending=0。跨实体与热点多 DAO 的吞吐接近，说明在此环境中逐笔持久化是主要限制；不能据此推断任意业务下锁竞争都不重要。

取 pipelined 展示阶段成本（其他策略原始 JSON 同样保留）：

| 负载 | Mongo 累计 s | ack 累计 s | ack 次数 | B/request |
| --- | ---: | ---: | ---: | ---: |
| single / pipelined | 0.433 | 0.301 | 22 | 33663 |
| dual / pipelined | 5.554 | 4.814 | 512 | 78558 |
| pair / pipelined | 5.712 | 4.798 | 512 | 156557 |
| hot / pipelined | 5.445 | 4.844 | 512 | 157176 |

多 DAO 512 笔各执行 512 次 checkpoint，Mongo 与 ack 共同占据主要墙钟时间。9～10 秒的投影 p99 是这批突发请求形成的排队延迟，并非低负载单笔 Mongo 事务本身需要 9～10 秒。

固定速率补充：同一 pair/strict 场景、128 Entity、4 DAO/事务。

| 目标输入 | 笔数 / 发送时长 | 实际 Request 完成/s | Request p99 | record→Mongo p99 | 请求结束未 ack | 额外排空 |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| 20/s | 1,200 / 59.956s | 20.015 | 6.735ms | 20.064ms | 1 | 0.019s |
| 100/s（带 CPU profile 的诊断运行） | 600 / 5.996s | 100.061 | 13.269ms | 7,355.093ms | 352 | 7.469s |

20/s 持续约一分钟，采样未 ack 峰值仅 1，未出现持续积压。100/s 输入仍能及时回复，但实际落库含排空仅 44.56/s，形成 352 笔积压；两次最终 WAL 都为 0，字段与版本全部正确。100/s 运行开启 CPU profile，有采样开销，不能把 44.56 与上表 48～50 的差异解释为退化。该诊断足以说明此输入超出当次处理能力，但不作为独立的最大容量估计。

预定发送至回复的 p99 分别 6.931ms、13.461ms，与实际 Request 延迟接近，没有把大量发送器迟到隐藏掉。CPU profile 13.54s 窗口内采样 1.99s；它只覆盖业务进程，不含三个 Mongo 进程。结合 Mongo/ack 墙钟计时，不支持“主要瓶颈是业务 CPU 算法”的判断。

## 3. 为什么多 DAO 会慢

源码与阶段计时共同定位到两段串行成本：

1. `isBatchProjectionRecord` 只把单 mutation、无 effect/receipt/Remote 的普通记录划入批量路径。多 DAO 记录逐笔调用 MongoStore.Project，每笔启动真实 Mongo 事务并写事务 marker。
2. ReplayPass 每个投影单元后调用 WAL.Ack。多 DAO 单元只有一条记录，因此每笔业务都有 checkpoint 写临时文件、file.Sync、替换与目录同步。单 DAO 批量路径能把这项成本分摊到多条记录。

因此，减少 DAO 数可能切换路径；改变 async/strict/pipelined 主要改变前段等待，无法自动提高后端多文档投影吞吐。不能用增加 goroutine 或把更多数据排入 WAL 来宣称性能改善。

## 4. 后续性能工作建议

这些是有测量依据的优化候选，不是本轮发现的数据正确性 bug；本轮保留原提交、WAL 和 Mongo 语义。

| 优先级 | 候选变更 | 约束与验收 |
| --- | --- | --- |
| 1 | 在现有 projector_replay.go 内，对有界的连续成功前缀合并 checkpoint ack | 不越过失败或 held 记录；Mongo 已成功但 ack 失败后仍可安全重放；补中段失败、取消、跨段、末条失败及三种策略的真实重启验收。不增加包，不降低 WAL durable 承诺 |
| 2 | 评估多 DAO 的批量投影，或更高层分片后的独立投影队列 | 前者必须保留逐业务事务身份、同文档版本顺序和失败降级；后者需要明确跨分片 Entity 事务归属，不能直接并行当前全局 WAL。先单独设计，不在压测中偷偷更改 |
| 3 | 按实际落库目标配置准入与积压告警 | WAL 已有磁盘/年龄保护；还需把可接受投影延迟转成业务负载预算。压力样本的有界请求数不代表生产输入已自动限速 |

仅消除 ack 开销仍保留 Mongo 事务成本，不能承诺因此达到上千笔多文档事务/s。优化收益须同机、同 durable/Mongo 配置复跑上述矩阵确认。

## 5. 收尾边界

**功能正确性与已批准的结构/bugfix 批次可以收口；多 DAO 高吞吐不建议标为完整优化结束。**

| 范围 | 当前判断 |
| --- | --- |
| D1 包结构与可读性 | 已完成，核心仍为两个包，中文契约与主线职责已整理 |
| D2 已确认生命周期与历史缺陷 | RR-09～13 已修复并有独立复现/回归，历史 ID 复用修复保留；本轮未发现新的数据正确性 bug |
| D3 性能 | 已有微基准、profile 和真实链路数据；多 DAO 每笔 Mongo 事务与 checkpoint 是已确认的性能瓶颈，若业务需要持续百笔或更多多 DAO 事务/s，当前本机配置不能验收通过 |
| D4 本地实体正确性与恢复 | 多实体多 DAO、错误/panic 回滚、晚冲突、丢 ack 重放、三进程恢复、100k 积压及既有依赖故障矩阵已有证据，见恢复记录 |
| Remote | 按用户要求留后续；本轮不运行、不宣称扩展验收完成 |
| 部署扩展 | Linux 目标硬件、长时峰谷负载、真实断电/磁盘 I/O 故障，以及历史坏 WAL 运维处置工具仍分别跟踪，未冒充已覆盖 |

20 笔多 DAO 事务/s 是这次已实际跑通的持续负载，不是框架配置上限或推荐生产配额。1000 玩家、10000 可见实体、20Hz、1%/5% 是 Sync 的负载描述，无法直接推导出每秒持久化事务数量；若把每次位置变化都写成多 DAO 事务，压力模型会完全不同。业务持久化频率、负载混合、payload 与允许投影延迟未定时，不能宣称整个 MMO 容量已经验收。此前 Sync 的 50ms 目标也不能自动当作 Mongo 落库 SLA。

已有提交承诺、事务原子性、持久化水位与故障恢复保持不变；本轮没有为了跑分改变 ack、批量事务或 fsync 策略。

## 6. 复跑与证据

```sh
source /tmp/roost-dataengine-it/env.sh
export GOCACHE=/tmp/roost-nest-go-cache
ROOST_PERF_REQUESTS=512 ROOST_PERF_COUNT=3 \
  ROOST_PERF_LABEL=my-dataengine-matrix bash scripts/perf/dataengine.sh

# 独立持续负载；按需要设置数量和速率，不能与上面的矩阵并行跑。
ROOST_PERF_SHAPES=pair ROOST_PERF_POLICIES=strict \
  ROOST_PERF_REQUESTS=1200 ROOST_PERF_RATE=20 ROOST_PERF_COUNT=1 \
  ROOST_PERF_LABEL=my-dataengine-paced bash scripts/perf/dataengine.sh
```

表中 single 来自 `artifacts/perf/dataengine/single-final-20260924/`（4,096 笔×三策略×三轮），dual 来自 `dual-final-20260924/`；pair/hot 来自 `matrix-20260924/` 的各策略第 1～3 样本。持续负载分别在 `paced20-20260924/`、`paced100-20260924/`。每份样本保留 JSON 和生成/校验/清理日志，环境文件保存 Go、CPU 配置与工作树状态。本机性能产物已加入 .gitignore，本文保存关键统计与复跑入口。

探索记录说明：初轮单实体场景用了 RequestMulti，已由正常 Request 入口重新运行并替换表内数据；初轮总控脚本运行中被更新，结束阶段发生 shell 重读语法错误。用于上表的 18 份 pair/hot 子进程及测试库清理均逐份确认 PASS；额外产生的第 4 份样本未纳入。最终脚本保持不变后顺序执行 single/dual、固定速率及 race 样本。没有把总控非零退出记为整轮通过。

诊断 profile 和汇总日志在 `/tmp/roost-dataengine-pressure/`；`paced100.cpu` / `pprof-top.txt` 对应带采样的 100/s 运行。复跑该诊断可在单样本命令增加 `ROOST_PERF_CPU_PROFILE=/tmp/dataengine.cpu`，不要让多样本共用一个 profile 路径以免覆盖。

图谱 Verify：`Users-whb-roost-roost-core` / `2026-09-24T10:14:59Z`。ReplayPass 双向一跳与分段条件已核对；图谱部分 heuristic 边误指向同名函数，以当前源文件修正。相关运行路径 coverage 为 metadata_match/no_recorded_issue；testdata/scripts/docs 被规则排除，以直接读取、生成编译和执行补证。没有作全仓无遗漏审计或修改忽略规则。

最终验证：

- 4 种压力形态的 pipelined/race 小样本（每项 64 笔、目标 100/s）通过，包含正常 Request 与 RequestMulti、固定速率调度和测量器并发访问；初始 pair/async 的 64 笔 race 也通过。
- `scripts/test-dataengine-generated.sh` 默认三进程验收重新通过，原单 DAO 与多实体/多 DAO 正确性用例保留；压力测试在普通验收中明确跳过，不把计时混入 race 数据。
- `go test -race ./nest ./nestwal ./dataengine/... ./entity ./kit/dataengine ./kit/nest ./codegen/internal/dao ./codegen/internal/entity -count=1 -timeout=120s`：9 包通过；同范围 `go vet` 通过。
- 脚本语法、文档链接目标和 diff 空白检查通过。上述验证日志在 `/tmp/roost-dataengine-pressure/`，不代表发布或全仓审计。
