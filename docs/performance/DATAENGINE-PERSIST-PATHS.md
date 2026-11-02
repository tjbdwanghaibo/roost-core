# DataEngine 递归持久化准备成本

日期：2026-11-02。基线为 v1.38.0 `c8e65071`，对比 v1.39.0 的递归持久化实现。Apple M5 / darwin arm64 / Go 1.27.3 / 默认 GOMAXPROCS=10；先停止本轮独立 Mongo，测试期间不并行执行构建、索引或其他压测。电源及宿主后台负载未额外控制，绝对容量不可外推。

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

### 正式 Nest 多实体三策略新旧对照

基线 v1.38.0 `c8e65071`，新版 v1.39.0 `19535956`。两边都使用当前已修正初始版本期望的同一 pressure_test.go（哈希一致），旧版运行时与生成器不变。每种策略按旧→新顺序运行一组，每组 10 万请求、1000 请求/s 持续约 100 秒；合计 6 组、60 万请求、240 万次 DAO 更新。初始化及最终核对不计入 TPS；每组总耗时约 140 秒。此表替换发布时只有新版的单边样本，旧样本保存在 v1.39.0 的 Git 历史中。

硬件与配置沿用前文：Apple M5，Go 1.27.3，默认 GOMAXPROCS（10），启动时为 AC 供电；独立 MongoDB 8.0.32 单成员副本集和文件 WAL。本轮没有并行运行其他压测、构建或索引，未采集连续的全机资源数据，不能确定宿主机后台负载的影响。

| 策略 | TPS 旧 → 新 | 请求 P99 旧 → 新 | 记录至 Mongo P99 旧 → 新 | 每请求总分配字节变化 |
| --- | ---: | ---: | ---: | ---: |
| async | 1000.00 → 999.95 | 12.67 → 15.42 ms | 59.94 → 60.54 ms | +7.9% |
| strict | 999.91 → 999.93 | 26.33 → 26.90 ms | 66.44 → 63.15 ms | +7.9% |
| pipelined | 999.89 → 999.82 | 16.53 → 16.52 ms | 64.68 → 64.52 ms | +5.6% |

六组最终内存/Mongo 的每份 DAO 数值、修改次数和版本全部匹配；每组包含 2000 笔初始化的 Committed/Projected 均为 102000，WALUnacked、AdmissionRejected、ProjectionFailures、FatalProjectionConflicts 全为 0。独立 Mongo 清理后只剩 admin/config/local，并正常关闭。

这里固定输入 1000/s，因此 TPS 持平只能说明两边都接得住，不能证明吞吐上限相同。普通标量多 DAO 业务没有集合载荷缩减，未观察到总体延迟改善；strict/pipelined 接近，async 新版请求 P99 增加约 21.7%。每档只有一组配对样本，不能把该尾延迟差异当成稳定回归率或归因到某一函数。

新版 async 还出现 5 次 handler 持锁超过 100ms 的警告，发生器最大迟到 112.85ms（旧版 22.68ms）。[原始警告](benchmarks/persist-paths/nest-compare-warnings.txt)全部保留；它们只反映该次运行的长尾，既不能证明是宿主机停顿，也不能证明所有请求的最大延迟或超 50ms 数量。其他各档的发生器迟到、采样积压峰值与排空时间保留在 JSON 中。

原始结果：旧版 [async](benchmarks/persist-paths/nest-before-async.json) / [strict](benchmarks/persist-paths/nest-before-strict.json) / [pipelined](benchmarks/persist-paths/nest-before-pipelined.json)，新版 [async](benchmarks/persist-paths/nest-async.json) / [strict](benchmarks/persist-paths/nest-strict.json) / [pipelined](benchmarks/persist-paths/nest-pipelined.json)。[环境与夹具哈希](benchmarks/persist-paths/nest-compare-environment.txt)、[运行顺序](benchmarks/persist-paths/nest-compare-runs.txt)。每请求分配包括框架与异步投影过程，不等于 setter 自身分配。

### 小字段准备成本：十轮交替微基准

为排除 WAL/Mongo 等待，补充 `BenchmarkPersistScalarDAOs`：四份生成 DAO，每份修改 Level、Exp 两个标量，经过隔离事务、BSON 冻结与准入验证，以内存提交器结束，并清理 Sync 变化。前后使用同一 benchmark，每侧 10 轮、每轮 300ms，先编译完两侧再交替执行，期间 Mongo 已关闭。

| 指标（每事务） | v1.38.0 | v1.39.0 | 变化 |
| --- | ---: | ---: | ---: |
| 准备耗时 | 7.049 µs | 9.213 µs | +30.72%（+2.164 µs） |
| 分配字节 | 11.61 KiB | 14.68 KiB | +26.44% |
| 分配次数 | 221 | 261 | +18.10% |
| BSON Set 载荷 | 116 B | 116 B | 不变 |

[before](benchmarks/persist-paths/scalar-before.txt)、[after](benchmarks/persist-paths/scalar-after.txt)、[benchstat](benchmarks/persist-paths/scalar-benchstat.txt)。复跑时使用上文 prepare.sh 和当前 benchmark 文件构建两侧，将过滤器改为 `-test.bench '^BenchmarkPersistScalarDAOs$' -test.benchtime=300ms`，交替各运行十次。

这个结果证明小字段准备路径存在额外成本；对照包含新的持久路径跟踪和 Patch 准入校验，尚未拆分两者各自占比。不能把微基准的 +30.7% 说成完整业务请求慢 +30.7%。

### 结论与后续方向

- 集合/嵌套修改显著受益：两轮平均 Mongo 积压追平速度分别为稀疏 8017→40747 条/s（5.08 倍）、嵌套 1298→40492 条/s（31.20 倍）、密集 970→4584 条/s（4.73 倍）。这些是积压追平，不是普通业务 TPS。
- 小字段多 DAO 业务没有减少 BSON 载荷，新增准备成本明确存在；当前 1000 TPS 档都能承受，不能表述为全场景都提升。
- 上述 v1.38.0/v1.39.0 对照只补测试和报告；随后已实施完整字段轻量标记，结果另列于下节，旧失败和退化样本保留。

本机单成员副本集、每档约 100 秒样本不是吞吐上限、长稳、Linux 或生产跨机容量证明；Mongo 落库延迟也不是客户端 Sync 收到延迟。

## 完整字段轻量标记与短键（main，未发布）

对照基线为干净导出的 v1.39.0 `19535956`，当前侧使用本次生成物与 core；仍为同机 Go 1.27.3、默认 GOMAXPROCS=10。构建、race 和 Mongo 验证完成后关闭隔离 Mongo，再交替运行两侧各十轮，每例 200ms。没有同时执行构建或索引。宿主后台负载未控制（验证阶段观察到 Spotlight 活动），绝对耗时不外推。

| 每事务指标 | v1.39.0 | 轻量标记 | 变化 |
| --- | ---: | ---: | ---: |
| 四 DAO / 每 DAO 两个标量：准备耗时 | 9.117 µs | 7.969 µs | -12.59% |
| 同上：分配字节 | 14.68 KiB | 11.74 KiB | -20.05% |
| 同上：分配次数 | 261 | 221 | -15.33% |
| 同上：BSON Set 载荷 | 116 B | 116 B | 不变 |
| 1000 元素稀疏修改 | 4.651 µs | 4.369 µs | 无显著差异（p=0.105） |
| 1000 元素嵌套修改 | 6.187 µs | 6.185 µs | 无显著差异（p=0.565） |
| 1000 元素密集修改 | 1.369 ms | 1.371 ms | 无显著差异（p=0.579） |

集合载荷保持 38B / 51B / 21811B；分配字节分别增加约 0.20% / 0.12% / 0.20%，不隐藏这部分代价。完整字段用位掩码避免路径树分配，递归路径仍保留。准入冲突检查没有撤掉；本轮不声称耗时完全回到 v1.38.0，也不把不同批次的绝对耗时直接比较。

短键独立测量：`BenchmarkStorageKeyEncoding` 使用字段和值相同的两个生成 DAO（一个标量、三个 map 元素、两个 list 元素，每个嵌套对象两个 int32），只改变 BSON 键。各十轮，每例 300ms，仅测完整 BSON 编码：

| 指标 | 可读键 | 数字短键 |
| --- | ---: | ---: |
| BSON 完整文档 | 267 B | 172 B（-35.58%） |
| 编码耗时中位数 | 1.125 µs | 1.129 µs |
| 分配字节 | 945 B | 833 B |
| 分配次数 | 17 | 17 |

短键降低载荷与输出缓冲分配，没有观察到编码耗时收益。收益取决于字段名占比，不是所有数据都减少 35.6%；未测压缩后的 Mongo 磁盘占用或真实网络带宽。PB 同步已经使用字段号，不会因 BSON 短键再次变小。

原始记录：[前](benchmarks/persist-paths/lightweight-before.txt)、[后](benchmarks/persist-paths/lightweight-after.txt)、[benchstat](benchmarks/persist-paths/lightweight-benchstat.txt)、[短键](benchmarks/persist-paths/storage-keys.txt)、[短键统计](benchmarks/persist-paths/storage-keys-benchstat.txt)、[环境/源码摘要](benchmarks/persist-paths/lightweight-environment.txt)。沿用 prepare.sh 构建干净 v1.39.0 与当前两个目录；两侧使用当前 persist_paths_bench_test.go，交替运行：

```sh
# 两个独立二进制交替各十轮；禁止在测量期间构建/运行 race。
/path/to/bench -test.run '^$' -test.bench 'BenchmarkPersistScalarDAOs|BenchmarkPersistPaths/n1000/' -test.benchtime=200ms -test.count=1
# 仅当前侧包含短键夹具。
/path/to/current/bench -test.run '^$' -test.bench '^BenchmarkStorageKeyEncoding$' -test.benchtime=300ms -test.count=10
```

本轮没有重新运行三种持久策略的 1000 TPS 端到端负载；前文 Nest/Mongo P99 属于 v1.38/v1.39 对照，不代表轻量标记版本的新结果。没有 Linux、跨机或生产长稳结论。

后续声明约束已统一为必填 `store` tag；上述短键样本采集于最初使用 `bson` 声明的阶段。本次迁移保留所有物理键与持久编码，不重编号；没有以这些旧样本冒充 tag 校验/告警的性能测试。

## store 约束完成后的复测（2026-11-02）

重新构建 v1.39.0 `19535956` 与当前未提交工作树的两个独立消费工程，使用同一 `persist_paths_bench_test.go`。Apple M5 / macOS arm64 / Go 1.27.3 / AC 供电，GOMAXPROCS 不设定（运行输出为 10）。两侧各十轮、每例 300ms，奇数轮旧→新，偶数轮新→旧；每轮另测当前长键/短键。隔离 Mongo 已关闭，没有并行执行构建、race 或索引。宿主后台活动没有控制。

| 每事务场景 | v1.39.0 | 当前 store / 轻量标记 | 结论 |
| --- | ---: | ---: | --- |
| 四 DAO，各两个标量 | 9.133 µs | 7.964 µs | 耗时 -12.79% |
| 同上分配字节 | 14.68 KiB | 11.74 KiB | -20.05% |
| 同上分配次数 | 261 | 221 | -15.33% |
| 1000 元素稀疏修改 | 4.416 µs | 4.559 µs | 无显著差异，p=0.271 |
| 1000 元素嵌套修改 | 6.169 µs | 6.225 µs | 无显著差异，p=0.271 |
| 1000 元素密集修改 | 1.360 ms | 1.358 ms | 无显著差异，p=0.971 |

稀疏/嵌套分配字节分别增加 0.20% / 0.12%，密集分配无显著差异；集合分配次数没有显著变化。各场景物理键保持不变，BSON 载荷仍分别为标量 116B、稀疏 38B、嵌套 51B、密集 21811B。标量收益来自完整字段轻量标记，不是 `store` 校验本身。

当前生成物的长键/数字短键完整编码对照：

| 指标 | 长键 | 短键 |
| --- | ---: | ---: |
| 完整 BSON | 267 B | 172 B（-35.58%） |
| 编码耗时中位数 | 1.135 µs | 1.115 µs |
| 分配字节 | 945 B | 833 B（-11.85%） |
| 分配次数 | 17 | 17 |

编码时间接近，不把约 0.02µs 的样本差异描述为显著性能提升；短键收益主要是减少载荷和输出缓冲分配。此项是完整编码，不是增量 Patch、Mongo 网络带宽或磁盘占用测量。

所有微基准通过。这轮仅测正式 setter → 隔离事务 → BSON 冻结 → 准入检查（内存提交器），没有 WAL/Mongo I/O，也没有重新测 Nest 端到端 TPS/P99 或生产容量。`store` 缺失校验与变化告警属于生成阶段，不在运行期每次写入执行。

原始结果：[旧版](benchmarks/persist-paths/store-recheck-before.txt)、[当前](benchmarks/persist-paths/store-recheck-after.txt)、[对照统计](benchmarks/persist-paths/store-recheck-benchstat.txt)、[短键](benchmarks/persist-paths/store-recheck-keys.txt)、[短键统计](benchmarks/persist-paths/store-recheck-keys-benchstat.txt)、[环境与源码摘要](benchmarks/persist-paths/store-recheck-environment.json)。复跑沿用前节 prepare.sh 和相同过滤器，benchtime 改为 300ms，按上述顺序交替十轮；基线导出 v1.39.0，当前侧使用本工作树。历史样本保留，不与本次混合计算。

## 轻量标脏与 store 短键：正式 Nest / WAL / Mongo TPS 对照

2026-11-02，约 13:51–14:36。对照三组：v1.39.0 `19535956`（before）、当前未提交工作树保持原长键（after）、当前工作树只把 Wallet/Inventory 业务存储键改为数字短键（short）。三组使用同一正式 pressure 测量器；短键组只适配定义 tag 与 Mongo 校验结构的 BSON tag，不改变 handler、值类型、更新字段、事务或校验门限。短键映射和生成步骤固定在[复跑脚本](../../scripts/perf/compare-dataengine-keys.py)中。

环境为本机 Apple M5 / macOS arm64 / Go 1.27.3，GOMAXPROCS 不设定（实际 10），AC 供电；独立 MongoDB 8.0.32 单成员副本集和本机文件 WAL。构建与压测顺序执行，无 race/profile/索引并跑；没有采集连续宿主资源指标，不能确定后台活动对波动的影响。不是 Linux、跨机 Mongo、客户端网络或 Saga 性能结论。

固定 1000 Entity、32 调用方、每请求 2 Entity × 2 DAO；业务 worker 8 / queue 4096，I/O worker 128 / queue 256。每份 DAO 修改两个 int64 标量，不含额外大 payload。输入 1000/3000/6000 请求/s，各计划 15 秒：高压时调用方等待回复再继续，实际完成时间会延长，发生器迟到单独计量。每档初始化与最终核对不计 TPS。每档结束均 Flush，落库 TPS 包含排空；不是把暂存 WAL 当作持久投影吞吐。

共 39 组：27 组阶梯 + 3 组反序 pipelined 高压复测 + 9 组 30 秒固定负载。合计 216 万次业务事务、864 万次 DAO 更新，另有每组 2000 笔初始化事务。所有内存/Mongo 值、修改次数、版本逐份核对通过；每组 Committed=Projected=requests+2000，最终 WALUnacked、AdmissionRejected、ProjectionFailures、FatalProjectionConflicts 均为 0。测试数据库全部清理，专用 Mongo 正常关闭。

### 30 秒固定负载对照

顺序为短键→当前长键→旧版。下表每项都是“旧版 / 当前长键 / 当前短键”，不得把这一轮的均值匹配输入解读为所有时间窗口都稳定。

| 策略 / 输入 | 完成 TPS | 请求 P99（ms） | 记录至 Mongo P99（ms） | 发生器迟到 P99（ms） |
| --- | --- | --- | --- | --- |
| async / 2000 | 1999.49 / 1999.60 / 1999.86 | 22.49 / 20.44 / 22.99 | 52.78 / 51.47 / 60.17 | 30.15 / 10.98 / 20.95 |
| strict / 1000 | 999.82 / 999.70 / 999.34 | 31.44 / 48.03 / 32.05 | 66.09 / 87.93 / 69.42 | 48.41 / **1097.03** / 35.82 |
| pipelined / 3000 | 2999.06 / 2998.73 / 2999.33 | 15.49 / 15.67 / 15.42 | 68.43 / 67.03 / 66.64 | 5.80 / 5.46 / 5.49 |

各档排空 17.13–48.27ms，计入排空后的 TPS 分别约 1998 / 999 / 2997。30 秒样本支持 async 2000 与 pipelined 3000 的短时承载判断；strict 1000 当前长键发生约 1.10 秒的发生器 P99 迟到，不能仅凭平均 TPS 跟上就声称稳定。请求 P99 不包含发生器等待，完整计划时刻至回复 P99 见原始 JSON；记录至 Mongo 延迟不是客户端 Sync 延迟。

每请求总分配（包含异步投影）旧版→当前长键→短键：async 94082→90237→82684B，strict 127937→115717→119177B，pipelined 160650→146132→141798B。与此前微基准的统计范围不同，不把差值全部归因于 setter 或键长度；首轮 async 1000 档当前长键分配反而高于旧版，原始样本保留。

### 饱和吞吐，不等于可持续输入上限

| 策略 / 输入 | 旧版完成 TPS | 当前长键完成 TPS | 当前短键完成 TPS |
| --- | ---: | ---: | ---: |
| async / 3000 | 2347.00 | 2405.24 | 2433.02 |
| async / 6000 | 2522.67 | 2543.46 | 2580.64 |
| strict / 3000 | 1157.73 | 1200.91 | 1215.38 |
| strict / 6000 | 1173.34 | 1174.79 | 1141.32 |
| pipelined / 6000 首轮 | 4274.71 | 4555.60 | 3822.48 |
| pipelined / 6000 反序复测 | 5060.71 | 5064.12 | 4914.97 |

表中部分值四舍五入，精确值以 JSON 为准。这套 32 调用方配置下，观察到的饱和完成区间约为 async 2350–2580、strict 1140–1220、pipelined 3820–5060 TPS；高压输入没有被及时消化，不能宣称支持持续输入 6000 TPS。调用方并发上限、持久策略、WAL/Mongo 与本机环境都会限制结果，这不是全框架绝对上限。

### 退化与未确认问题

- 当前长键 strict 的 30 秒样本发生器 P99 迟到约 1.10s，请求 P99 48.03ms。同一秒出现多条约 132–134ms 的持锁警告；根因未确认，不能直接归为桌面停顿、GC 或磁盘。首轮 strict 1000 的 P99 也为旧版 17.65ms、当前长键 40.70ms、短键 31.99ms，未丢弃。
- pipelined 首轮短键 3822.5 TPS，比当前长键低约 16.1%；反序复测短键升到 4915.0，长键 5064.1，旧版也升到 5060.7。整体有显著轮次波动，不能只挑一轮证明提升或回归。
- 39 组共保留 43 条 WARN 日志；测试通过只说明最终契约校验通过，不说明没有延迟异常。这里没有修复或掩盖这些长尾，也没有改变门限。

**结论：本轮没有证明稳定的端到端 TPS 提升。** 标量准备微基准的改善和短键载荷缩减成立，但真实链路收益更小、受持久等待与环境波动影响。短键优先按存储体积需求采用；不能据此承诺 Mongo 带宽/磁盘同比下降或小事务 TPS 明显增长。30 秒档不是生产长稳验证，strict 长尾仍需单独定位。

### 原始记录与复跑

- [39 组完整结果](benchmarks/persist-paths/store-tps-results.json)：请求/落库 TPS、P95/P99、排空、积压采样、发生器迟到、分配、最终统计。
- [环境和三侧生成源码摘要](benchmarks/persist-paths/store-tps-environment.json)、[完整 WARN 摘录](benchmarks/persist-paths/store-tps-warnings.txt)。本机大日志/二进制保留于被忽略的 artifacts/perf/store-tps，不依赖这些临时文件才能复跑。

```sh
# baseline 使用干净导出的 v1.39.0；out 必须不存在，脚本拒绝覆盖样本。
export ROOST_DATAENGINE_IT_MONGO_URI='<isolated replica-set URI>'
python3 scripts/perf/compare-dataengine-keys.py --baseline /path/to/v1.39.0 \
  --out /tmp/store-tps-ladder --seconds 15
python3 scripts/perf/compare-dataengine-keys.py --baseline /path/to/v1.39.0 \
  --out /tmp/store-tps-pipeline-repeat --seconds 15 --rates 6000 --policies pipelined --reverse-order
# 三个固定档分别运行：async/2000、strict/1000、pipelined/3000。
python3 scripts/perf/compare-dataengine-keys.py --baseline /path/to/v1.39.0 \
  --out /tmp/store-tps-async --seconds 30 --rates 2000 --policies async --reverse-order
```
