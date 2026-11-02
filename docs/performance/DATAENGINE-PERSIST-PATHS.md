# DataEngine 递归持久化准备成本

日期：2026-11-02。基线为 v1.38.0 `c8e65071`，对比本次 main 工作树的递归持久化实现。Apple M5 / darwin arm64 / Go 1.27.3 / 默认 GOMAXPROCS=10；先停止本轮独立 Mongo，测试期间不并行执行构建、索引或其他压测。电源及宿主后台负载未额外控制，绝对容量不可外推。

## 测量口径与结果

同一 benchmark 源码，生成两个独立消费工程，各自引用对应 core 与 golden 生成物。前后交替 10 轮，每例每轮 200ms。覆盖 setter、事务/撤销记账、PrepareMutation BSON 冻结、ValidateCommitRecord 准入校验与 Sync 确认清理，不包含真实 WAL I/O、fsync、Mongo、网络或 Nest 排队。

| 集合规模 / 修改 | 准备耗时：之前 → 之后 | BSON Set 载荷：之前 → 之后 |
| --- | --- | --- |
| 100 / map key + list index | 5.917 → 4.253 µs | 1222 → 38 B |
| 100 / 两处嵌套叶子 | 40.153 → 6.381 µs | 7689 → 51 B |
| 100 / map 与 list 全改 | 111.19 → 98.16 µs | 2.538 → 1.964 KiB |
| 1000 / map key + list index | 33.132 → 4.774 µs | 12922 → 38 B |
| 1000 / 两处嵌套叶子 | 385.555 → 6.598 µs | 76089 → 51 B |
| 1000 / map 与 list 全改 | 1.617 → 1.463 ms | 27.15 → 21.30 KiB |

表中耗时是 benchstat 中位数；BSON 仅计 `Patch.SetBSON`，不含 Mutation 元数据、Unset、WAL 信封或网络头。密集变化的载荷减少来自 map key 补丁回退为整体字段，减少重复路径前缀；不表示 BSON 压缩算法变化。

1000 元素稀疏场景分配字节由 73.093 KiB 降至 7.709 KiB，嵌套场景由 699.10 KiB 降至 12.92 KiB。**稀疏场景分配次数由 80 增至 101**：路径树有元数据分配成本，当前选择减少完整对象编码与总分配字节。密集场景仍需遍历被修改元素，不是常数时间。不能用这些数值推导 Mongo TPS、磁盘写入量或生产 P99。

原始样本：[before](benchmarks/persist-paths/before.txt)、[after](benchmarks/persist-paths/after.txt)、[benchstat](benchmarks/persist-paths/benchstat.txt)。基准源码为 `codegen/internal/dao/testdata/runtime/persist_paths_bench_test.go`；两侧使用同一份源码。

## 复跑

运行 [prepare.sh](benchmarks/persist-paths/prepare.sh) 时传入源码目录、全新临时工程目录以及当前 benchmark 文件。旧版源码可用 `git archive c8e65071` 导出，不要在带未提交修改的工作树切换版本。

```sh
bash docs/performance/benchmarks/persist-paths/prepare.sh "$PWD" /tmp/persist-after "$PWD/codegen/internal/dao/testdata/runtime/persist_paths_bench_test.go"
/tmp/persist-after/bench -test.run '^$' -test.bench '^BenchmarkPersistPaths$' -test.benchtime=200ms -test.count=1
# 对基线导出的源码重复构建，然后交替运行两侧各 10 次，分别保存输出。
benchstat before.txt after.txt
```

这份基线只回答普通 DAO 更新准备与载荷问题。真实 Mongo 与 WAL 重开验证见[实现说明](../framework/DATAENGINE-INCREMENTAL-PERSISTENCE.md)，不冒充存储容量测试。

## 真实存储压力复跑

`TestPersistPathsPressure` 使用 128 个 DAO、每个集合 1000 元素，每档 10000 次事务；先经 Nest 隔离事务生成最终 Mutation、验证并 async 写入文件 WAL，统一 Sync 后以 8 个投影 worker 排空。核对全部文档内容、版本、投影失败数及 WAL 剩余。它测积压追平，不包含 Nest 消息队列，prepare/append 延迟包含单写者等待，不能与上面的 CPU 微基准直接比较。

```sh
# 使用上节 prepare.sh 构建的当前消费测试二进制。
ROOST_PERSIST_PRESSURE=1 ROOST_DATAENGINE_IT_MONGO_URI='<isolated replica-set URI>' \
  /tmp/persist-after/bench -test.run '^TestPersistPathsPressure$' -test.v -test.timeout=8m
```

旧版对照需同时复制当前 `persist_pressure_test.go` 与 `persist_paths_test.go` 到临时消费工程（去掉 daoruntime build tag），后者提供共同捕获/校验函数；只运行压力用例，不运行要求新行为的回归。两侧始终使用各自版本的生成 DAO 与 core。

正式 Nest 多实体链路另使用 `scripts/test-dataengine-generated.sh`：1000 个 Entity、32 个调用方、每事务 2 个 Entity × 2 个 DAO、short worker 8 / 等待容量 4096、I/O worker 128 / 等待容量 256，async/strict/pipelined 分档；不覆盖客户端网络或 Saga。

```sh
# 每档独立库/WAL，输出文件须选尚未存在的位置；不固定 GOMAXPROCS。
ROOST_DATAENGINE_IT_MONGO_URI='<isolated replica-set URI>' \
ROOST_DATAENGINE_PERF=1 ROOST_PERF_SHAPE=pair ROOST_PERF_ENTITIES=1000 \
ROOST_PERF_WRITERS=32 ROOST_PERF_REQUESTS=100000 ROOST_PERF_RATE=1000 \
ROOST_PERF_POLICY=async ROOST_PERF_OUTPUT=/tmp/persist-nest-async.json \
  bash scripts/test-dataengine-generated.sh
# 将 policy/output 分别改为 strict、pipelined 再运行。
```

### 两轮交替的真实 Mongo 结果

独立 MongoDB 8.0 单成员副本集与 WAL 均在本机 SSD，默认 GOMAXPROCS（10），不使用 race/profile。每侧每轮 3 档，共 12 档、12 万条更新；每档逐一核对 128 份文档的完整内容和最终版本。全部零投影失败、WAL 剩余 0。原始数值见 [pressure.json](benchmarks/persist-paths/pressure.json)，环境与源码摘要见 [pressure-environment.txt](benchmarks/persist-paths/pressure-environment.txt)。

| 形状 | v1.38.0 追平速度，两轮（条/s） | 本次追平速度，两轮（条/s） | 准备＋追加总时间，旧 → 新（秒） |
| --- | --- | --- | --- |
| 稀疏 map key + list index | 7957 / 8078 | 40828 / 40665 | 14.76 / 14.73 → 12.67 / 12.60 |
| 两处嵌套叶子 | 1308 / 1287 | 41160 / 39824 | 27.88 / 28.45 → 12.68 / 12.86 |
| map 与 list 全改 | 953 / 987 | 4586 / 4582 | 32.32 / 34.49 → 33.35 / 33.93 |

追平提升来自更小更新载荷及密集 map 更新改为完整字段赋值；密集修改的准备＋WAL 追加没有稳定改善。准备阶段单调用方逐条等待 WAL 写入，包含默认 500µs 批等待和宿主调度，不是 setter 的纯 CPU 耗时。Mongo 服务端仍处理实际文档，不能按网络字节比例推算磁盘收益。单成员本地副本集结果不能代替多节点/远端服务性能。

### 保留的失败样本

首次正式 Nest async / 1000 Entity / 10 万请求在最终内存版本检查失败。最小复核在 v1.38.0 和当前源码均得到 `coins=11000 transfers=1000 version=1002; want ... version=1001`，确认压测仍沿用初始版本 1，遗漏创建本身的持久提交。修复只把 wallet、inventory、Mongo 三处期望统一为已有 `seededVersion=2`，不变更业务执行或降低校验。详见[RV-20261102-02](../maintenance/CORE-REVIEW.md#rv-20261102-02正式-dataengine-压测使用过时的初始版本)。失败轮没有生成完整结果，不纳入通过样本或 TPS 对比。最小复核输出：[旧版](benchmarks/persist-paths/nest-baseline-red.txt)、[当前](benchmarks/persist-paths/nest-current-red.txt)；末尾的 PASS 来自脚本清理阶段，压力用例与脚本整体均退出失败。

### 正式 Nest 多实体三策略验收

三档均以 1000 请求/s 持续输入约 100 秒，每档 10 万请求（每请求 4 DAO 更新），合计 30 万请求 / 120 万 DAO 更新，不含初始化。每档完整执行约 140 秒，额外时间为逐实体初始化、最终核对及清理。每档使用 1000 Entity、32 调用方、上文 worker 配置和独立库/WAL。

| 策略 | 完成 TPS | 请求 P99 | 计划时刻至回复 P99 | 记录至 Mongo P99 | 停止输入后排空 | 采样积压峰值 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| async | 999.97 | 13.60 ms | 14.42 ms | 60.96 ms | 34.93 ms | 196 |
| strict | 999.87 | 28.95 ms | 39.76 ms | 65.98 ms | 26.53 ms | 167 |
| pipelined | 999.92 | 16.76 ms | 17.19 ms | 69.78 ms | 29.22 ms | 231 |

三档最终每份 DAO 的内存/Mongo 数值、修改次数、版本均匹配；每档包含 2000 笔初始化的 Committed/Projected 均为 102000，WALUnacked、AdmissionRejected、ProjectionFailures、FatalProjectionConflicts 全为 0。结束核对隔离 Mongo 只剩 admin/config/local，未遗留测试库。

原始结果：[async](benchmarks/persist-paths/nest-async.json)、[strict](benchmarks/persist-paths/nest-strict.json)、[pipelined](benchmarks/persist-paths/nest-pipelined.json)。积压峰值为请求完成时采样，不是连续精确峰值；调用方迟到最大约 42～71ms，已保留在 harness 字段。请求 P99 不等于所有样本都小于 50ms；记录至 Mongo 的延迟也不是客户端 Sync 收到延迟。

这组验证了本机 1000 TPS 档位，不是 TPS 上限、15 分钟长稳、生产多节点 Mongo 或 Linux 容量保证。集合增量前后对照与本节正式 Nest 标量多 DAO 场景分别回答不同问题，不能把两张表相乘推算吞吐。
