# Remote 容量、长稳与集群故障验收

> 2026-09-25 后续：RR-20260924-26 已实施 Mongo 持久权威并通过针对性真实故障回归，见 [修复与升级边界](../bugfix/RR-20260924-26.md)。本文以下数据保持旧协议的历史测试事实，不自动继承为新协议长稳验收。
2026-09-24 开始执行，基线 967bc69 加当前工作树。按用户选择实际运行 30 分钟，并提供 24 小时入口；不会把 30 分钟结果写成 24 小时通过。本报告区分已执行结果、失败准入项与尚未执行的 24 小时验证。

当时结论（旧协议）：strict 20 TPS 的 30 分钟业务/数据校验通过；100 TPS 时 strict/pipelined 大量超时，async 可接受短时突发但需要约 91.4 秒完成后续排空和核对。Redis 未复制写丢失后 fence 重用仍未修复，**生产 HA 验收未通过**。24 小时入口就绪，尚未执行。

## 负载与判据

正式 DAO/Entity 生成器 → Nest.RequestMulti → 文件 WAL v2 → Mongo 原子 Remote projection → Redis L2/锁 → NATS → 接收 Manager。健康/容量场景不配置存储回源以证明实际网络交付；NATS 故障场景配置真实 Mongo backend，并验证最小版本的单调读取与权威回填。每事务修改两个实体、每实体两个 DAO；整个数据集 10000 个实体，共 20000 个 DAO/快照。1000 个业务请求 worker 代表业务会话并发上限，不是 1000 个真实客户端连接，也不是 AOI 可见关系测试。

本夹具每个 DAO 的业务字段为一个 int64，当前 WAL 记录约 1488 字节/事务；没有真实游戏背包或任务列表的大对象载荷。因此“生产规模”仅指所给会话/实体数量和正式调用通路，不等同于真实线上流量回放。

长稳以 strict、20 Remote 事务/s 为本次基线，Remote 持久化频率独立于 20Hz 客户端同步。固定速率输入，有界等待队列，记录完成/错误/丢弃、排队起算的 Request p50/p95/p99/max、WAL 与投影积压、事务缓存、Go heap 和 goroutine。返回延迟在不同持久化模式下代表各自的确认边界，不等于全部 Mongo/NATS 可见延迟。

预热先令所有实体完整提交一次，不计入持续输入吞吐；最终复跑器使用统一 strict 预热，再以所选策略计时，避免 async 的提前返回把预热变成积压测试；Interest 在运行中每 10 秒续订，保持默认 30 秒租期。为承载约 250 秒的数据集轮转，压力夹具的快照缓存 TTL 明确设为 5 分钟，Nest 64 worker、GOMAXPROCS=4；本次 30 分钟和首轮容量的锁租期沿用原业务夹具的 **3 秒**，后续补测与最终入口使用框架默认 **24 小时**；Async runtime 为 2 worker / 32 队列；其余 Remote 默认容量预算保留。短租期会改变过载时的 fencing 边界，不能把该组容量当成默认生产租期结果。成功样本要求 Flush 后逐个检查全部 20000 Mongo 文档、版本/值及 20000 NATS 快照，并要求 Remote outbox 排空；请求错误样本判失败，不声明全量一致性通过。成功标记只在这些检查之后写入。

单机 Apple M5 上同时运行隔离 Mongo 三节点、NATS 三节点、单节点 Redis 和应用进程；健康/压力测试的发送与接收 Manager 位于同一个 Go 测试进程，通过两条真实 NATS 连接交换消息。长稳期间还运行过独立 Redis Cluster 和短时回归，同机资源会有竞争；不是专用静默压测主机。六节点 Redis Cluster 故障使用另一组专属进程/端口，与长稳的单节点 Redis 独立。容量结果不能代表多主机生产网络或生产硬盘性能。

## 30 分钟实际结果（2026-09-25 完成）

目录 `artifacts/perf/remote/soak-30m-strict-20-v3/`。正式 Go 测试 **PASS**，测试主体 2026.76s（含约 227 秒初始化/预热及最终核对），计时输入 1800.005s。

| 指标 | 实测 |
| --- | ---: |
| 计时成功事务 | 35,999 |
| 预热事务 | 5,000 |
| 完成吞吐（含尾部等待） | 19.9994 事务/s |
| Request p50 / p95 / p99 / max | 36 / 45 / 80 / 529 ms |
| 业务错误 / 输入丢弃 | 0 / 0 |
| 采样最大 WAL 未确认 / Remote 活跃事务 | 6 / 6 |
| 最终 WAL 未确认 / Remote 活跃事务 | 0 / 0 |
| Go heap 采样范围 | 83.9～229.0 MiB |
| 运行中 goroutine / worker 排空后 | 1190～1203 / 190 |
| 最终 retained trackers / 配置上限 | 40,999 / 65,536 |
| 最终 WAL 文件字节 / 段数 | 61,006,584 / 1 |
| 最终 Mongo 文档 / NATS 快照校验 | 20,000 / 20,000，全量通过 |

末尾业务拒绝与 panic 回滚、Remote outbox 排空也通过。p99 80ms 是 strict Remote 事务返回时间，不能拿来替代此前 Entity Sync 的 50ms 交付指标。heap 包含未回收分配和事务缓存增长；30 分钟尚未触及 tracker 容量上限，也没有跨 WAL 段轮换，不宣称已证明 24 小时内存/磁盘有界。24 小时复跑重点检查 tracker 触及上限后的回收、反复 WAL 分段与 checkpoint 回收、GC 后 heap 是否持续增长，以及 Interest 多轮续订后的全量快照一致性；这些项目目前未完成验收。

这次正在运行的二进制使用首版 ticker 输入，共收到 35,999 个 tick（理论 36,000），统计按实际完成数计算。后续复跑器改为按计划时间补发并计入队列丢弃，避免 ticker 漏 tick 隐藏过载；没有用更改后的公式回填本次结果。

执行器问题保留：运行期间更新 shell 文件，使两层脚本在 Go 测试结束后恢复读取时出现语法错误，外层退出 2。因此**该次脚本整体退出不算通过**；上表依据独立 Go PASS、完整 result.json 和最终 `.verified` 标记。最终脚本语法已检查，默认租期 async 的完整容量入口与 `runner-smoke-final-20260925` 短测均已退出 0；未删掉这次失败日志。长稳二进制编译早于 Cluster 两项修复，使用的是单节点 Redis；修复的 Cluster/Kit 分支由后续真实集群与 race 独立验证。

## 三种策略的短租期持续输入容量探测

同一 1000 worker / 10000 Entity / 两 Entity × 两 DAO 形状，以 100 事务/s 输入 30 秒，每种策略独立初始化。最终夹具统一 16 并发 strict 预热，计时阶段才使用目标策略；默认 Nest 5 秒回复等待、Remote 容量限制没有放宽。短样本用于发现过载和定位成本，不用作最大稳定 TPS。

结果目录 `artifacts/perf/remote/capacity-<policy>-100-final-20260925/`，退出码见 `capacity-final-20260925.tsv`。各策略实际值见下表（本组串行执行）。

| 策略 | 成功 / 错误 / 丢弃 | 成功返回 TPS | p50 / p95 / p99（ms） | 最大采样 WAL 未确认 | 全量验收 |
| --- | ---: | ---: | ---: | ---: | --- |
| strict | 156 / 2844 / 0 | 5.20 | 2443 / 4834 / 4978 | 987 | 失败：首个错误为 Nest 回复等待超时，后续拒绝用例遇 writer fenced |
| async | 3000 / 0 / 0 | 99.97 | 9 / 16 / 18 | 2349 | 未验收：发现夹具续订生命周期错误后主动终止 |
| pipelined | 158 / 2842 / 0 | 4.51 | 2331 / 4674 / 4974 | 2309 | 失败：回复等待超时，后续值核对受未决提交影响 |

async 虽然以约 100 TPS 返回，但计时窗口仅投影 651 笔（约 21.7 TPS），结束时还有 2349 笔 WAL 未确认；这不是可持续 100 TPS 的落地能力。

strict 的 5.20 是过载时在回复期限内成功返回的速率，**不是存储的最大提交能力**；3000 笔计划输入中仅 156 笔成功返回，其余错误不能忽略。

strict 的 30 秒 CPU profile 中，`slowDispatchTraceWatch.trace` 累计占采样 CPU 的 29.04%，全 goroutine 堆栈采集约 19.76%；慢调用诊断在过载时会增加成本。总 CPU 样本约 10.02 秒/30.13 秒墙钟，不支持“应用 CPU 已打满”的结论。仅此 profile 不能归因 Mongo 磁盘或网络瓶颈，也尚未执行关闭诊断的 A/B 对比；原始 profile 和 `cpu-top.txt` 已保留。

这一轮 async 输入结束后，夹具也停止 Interest 续订，投影却仍在排空；最终 NATS 校验因租期过期而缺少快照，不能算生产模块通过或失败。已将续订延长到最终校验结束，保留原始结果和主动中止记录，再补跑正式默认锁租期。pipelined 短租期样本已包含此夹具修复。

保留两轮非计时失败：`capacity-*-100-20260925/` 是 64 并发冷预热触发超时/容量拒绝，`capacity-*-100-warm16-20260925/` 是 NATS 初始未就绪，在业务开始前失败。最终入口新增环境预检和恢复，没有删掉失败或将它们混入持续吞吐统计。

延迟分位数仅统计成功返回的请求，从计划发送时间起算，包含等待；必须同时查看错误、丢弃和 WAL 积压。若 Request 超时，事务结果可能仍未决或稍后提交；不能把这类超时直接解释为回滚，也不能把“实际值比成功返回计数大”推导为数据损坏。首版 pipelined 失败后的 `rollback lost` 文案只表示实际值与成功回复计数不符，不构成回滚缺陷证据；最终复跑器在负载错误后直接判失败并跳过该错误计数口径的状态核对。只有 `.verified` 存在且 Go 测试退出零，才表示该负载的全量最终验收通过。

## 正式默认锁租期补测

使用 `ROOST_REMOTE_LOCK_TTL=24h`，其余计时负载相同；订阅持续到最终校验，失败负载不再把成功回复计数作为精确状态期望。目录为 `capacity-<policy>-100-default-20260925/`。

首个 strict 样本未进入计时：该轮隔离 NATS 失联，与相邻执行器结束时间重合（未另行证明进程退出根因），预热续订最终报 `outbound buffer limit exceeded`。这是环境/执行器生命周期问题，原样本保留，不能作为 100 TPS 结论；集群恢复后继续后续策略，最后独立重跑 strict；有效目录为 `capacity-strict-100-default-retry-20260925/`。

| 策略 | 成功 / 错误 / 丢弃 | 成功返回 TPS | p50 / p95 / p99（ms） | 最大采样 WAL 未确认 | 全量验收 |
| --- | ---: | ---: | ---: | ---: | --- |
| strict（重跑） | 158 / 2842 / 0 | 4.72 | 2471 / 4774 / 4913 | 2046 | 失败：Nest 回复等待超时，未声明最终一致性通过 |
| async | 3000 / 0 / 0 | 99.97 | 5 / 12 / 16 | 2369 | **通过，Go 与脚本退出均为 0** |
| pipelined | 169 / 2831 / 0 | 4.83 | 2217 / 4768 / 4997 | 2248 | 失败：Nest 回复等待超时，未声明最终一致性通过 |

strict/pipelined 在默认 5 秒回复等待下大量超时，100 TPS 均未通过；延长锁租期没有消除投影积压。现有证据支持本机小载荷 strict 20 TPS 的 30 分钟基线，尚未测出最大可持续吞吐，不能将成功回复的 4.72/4.83 TPS 当成最大提交能力。

async 计时期间投影 631 笔（约 21.0 TPS）。生成负载结果后又经过约 **91.4 秒**才写出全量校验标记，包含排空、业务拒绝/panic 回滚与逐文档/快照核对。20,000 Mongo 文档、20,000 实际 NATS 快照及 outbox 排空全部通过，`authoritative_refill=false loads=0`。这次证明 30 秒突发最终恢复一致，未证明持续 100 TPS；其 p99 16ms 仅为 async Request 回复，不是 Mongo/NATS 最终可见延迟。

## 本轮故障矩阵

| 范围 | 场景与断言 | 执行状态 |
| --- | --- | --- |
| 正式 Remote 业务，三种持久化模式 | Mongo primary 停止后，继续四 DAO 提交与最终快照校验 | 三种模式均通过 |
| 同上 | Mongo 两节点停止，恢复多数派后完成提交，保持四 DAO 一致 | 三种模式均通过 |
| 同上 | NATS 单节点停止，客户端通过发现的集群地址恢复，单调读取/必要时回填，最终快照正确 | 三种模式均通过 |
| 同上 | NATS 全停后恢复，检查业务提交与单调读取/权威回填后的最终快照 | 三种模式均通过 |
| Redis 多进程租约 | 强杀、旧 owner 隔离/恢复后拒绝误解锁、慢网络下准入取消 | 本轮矩阵通过 |
| Redis 3 主 3 从 | 已复制 owner/fence 后强杀主节点；旧 owner 拒绝、新 fence 增长 | 修复 RR-24 后通过，约 2.283s |
| Redis 3 主 3 从 | 已确认但未复制的锁写丢失后再选主 | **失败：fence 1 → 1，RR-26，生产 HA 未通过** |
| Redis 3 主 3 从 | 全节点强杀，有界请求失败；AOF always 重启后 version/fence 保留 | 通过 |
| Redis 计数与 ownership | int64 精度、溢出拒绝、争抢/转移 | 本轮矩阵通过 |
| Mongo/文件 WAL | 多实体原子回滚、发布失败重放、checkpoint 丢失恢复 | 本轮矩阵通过 |
| DataEngine durable effects | Mongo 选主、NATS 全停、JetStream leader 切换 | 本轮矩阵通过 |
| NATS 网络 | 延迟、连接 reset、半开/ACK 丢失 | 本轮矩阵通过；这是 durable effect 路径，不冒充 Remote 快照可靠交付证明 |

原始矩阵 `artifacts/perf/remote/matrix-20260925/results.tsv` 共 20 组，18 通过、2 失败：未复制 Redis fence 回退，以及不带回源的 NATS 全停 async 快照缺失。后者暴露的是接收端配置的交付边界：普通 NATS 丢消息后，仅有本地缓存无法自行补齐。

随后补跑正式 Mongo backend + RemoteReadMonotonic（要求最低版本）的 NATS 单停/全停 × 三种模式，共 6 个组合均通过。全停 async 实际执行 4 次 Mongo 快照读取，四份 DAO 快照最终都达到版本/值 3；不是放宽断言或重发业务。该测试由调用方提供可信最低版本 3；不证明不知道最新版本的纯订阅消费者在重连后会自动补齐，也没有把 Core NATS 升级为持久消息协议。补跑第一格因 NATS 初始未就绪失败，环境恢复后的同格重跑通过，原失败仍保留在 `matrix-refill-20260925/results.tsv`。后续矩阵入口已加入环境预检。

按最终生产读取配置归并，**20 组中 19 组通过，1 组失败（RR-26）**，证据来源见 `artifacts/perf/remote/matrix-final-20260925.tsv`。这是两轮来源的汇总，不是声称原矩阵一次全绿；故障准入入口仍会因 RR-26 返回非零。

本轮“完整矩阵”指上述明确拓扑、故障与三种策略的笛卡尔组合，不指一切可能的分布式故障。跨机网络分区、磁盘损坏/断电、任意事务指令位置 SIGKILL、永久离线消费者都不由这些用例证明。普通 NATS 不提供持久投递；故障后的生产读取必须允许按最低版本回源，不能把仅有缓存和普通 NATS 的接收端当作可靠消息消费者。

## 已发现问题

- [RR-26](../bug/RR-20260924-26.md)：真实未复制写丢失后 fence 复用，当时生产 HA 准入失败；现已实施根因修复，见 [协议修复记录](../bugfix/RR-20260924-26.md)。

- [RR-24](../bugfix/RR-20260924-24.md)：Redis 已选主但 go-redis 写路由仍缓存旧主，默认周期 60 秒；断连后合并异步刷新修复，不额外重放命令。
- [RR-25](../bugfix/RR-20260924-25.md)：Cluster 缺少 hash tag，首笔加锁 CROSSSLOT；Kit Init 现拒绝无效配置。显式固定 tag 的锁仍集中在一个槽位，不宣称分片容量提升。
- 首次 1000 并发冷预热触发 Nest 默认 5 秒等待超时，未进入 30 分钟计时，不计为长稳通过。正式长稳改用 16 并发预热；后续 64 并发冷预热 strict 同样超时，async 提前回复导致未落地事务积压，触发 `remote entity: capacity exceeded`。这些失败不计为持续输入容量结果；最终容量复跑统一用 16 并发 strict 预热，再切换计时策略。突发启动与持续输入是不同容量条件。

核心生产改动的 Redis driver、Kit Remote 和 RemoteEntity race 回归、集成 build-tag vet 均通过；真实故障矩阵使用 race。容量/长稳入口默认不带 race，避免把检测器开销算作业务容量。最终脚本通过语法检查，未将预热、环境失败或跳过的测试记为容量通过。

## 复跑入口

共用测试集群的容量和故障测试必须串行，脚本使用环境目录中的 `remote-acceptance.lock` 防止两个入口同时运行，获得锁后先恢复并检查隔离集群就绪，再开始测试。勿在其他未遵循此锁的集成任务运行期间执行故障矩阵。

```sh
bash kit/scripts/integration/dataengine-env.sh up
source /tmp/roost-dataengine-it/env.sh
export GOCACHE=/tmp/roost-nest-go-cache GOWORK=off

# 默认 1000 业务 worker / 10000 实体，strict / 20 事务每秒 / 30 分钟；正式默认锁租期 24h。
ROOST_REMOTE_LABEL=soak-30m bash scripts/perf/remote.sh
# 复刻本次已完成长稳的 3 秒锁租期：添加 ROOST_REMOTE_LOCK_TTL=3s。

# 24 小时入口：本轮不提前宣称这一项已执行。
ROOST_REMOTE_LABEL=soak-24h ROOST_REMOTE_DURATION=24h \
  ROOST_REMOTE_TIMEOUT=25h bash scripts/perf/remote.sh

# 故障矩阵：12 个正式业务组合 + 租约、Cluster、ownership、WAL 与 Broker 场景。
ROOST_REMOTE_MATRIX_LABEL=fault-matrix bash scripts/test-remote-matrix.sh

# 单点容量或热点复跑，建议独立多轮采样。
ROOST_REMOTE_LABEL=capacity-100 ROOST_REMOTE_DURATION=30s \
  ROOST_REMOTE_RATE=100 ROOST_REMOTE_POLICY=pipelined bash scripts/perf/remote.sh
ROOST_REMOTE_LABEL=hot-20 ROOST_REMOTE_DURATION=30s \
  ROOST_REMOTE_SHAPE=hot ROOST_REMOTE_RATE=20 bash scripts/perf/remote.sh
```

压力结果位于 `artifacts/perf/remote/<label>/`：环境信息、压缩原始日志、result.json、每 10 秒 JSONL 采样及最终 `.verified` 标记。`ROOST_REMOTE_LOCK_TTL` 可显式设置锁租期，未设置时采用框架默认 24h；不延长 Nest 默认 5 秒回复等待。`ROOST_REMOTE_PROFILE=1` 输出 CPU profile；仅短时容量样本建议开启。故障矩阵逐格保留日志与 results.tsv，测试跳过或非零退出均记为失败。最终脚本短测使用 strict、100 Entity、16 worker、20 TPS × 3s，完成 60 笔并通过全量校验；它验证执行入口，不能替代 10000 Entity 的容量或 24 小时验收。

## 图谱与证据范围

本轮采用 Verify，源码定位结合 source fallback；图谱对同名方法的启发式连边不作为真实调用证明。索引已刷新至 `2026-09-24T16:04:30Z`，18,144 nodes / 143,835 edges，本轮生产源码与集成测试 metadata_match、无记录缺口；这仍不是无遗漏保证。testdata、scripts、docs 和性能产物按配置排除，已直接读取、生成、编译或执行。三处历史模板 parse_partial 不在本轮范围。
