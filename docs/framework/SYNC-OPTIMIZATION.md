# Sync 局部优化与玩家 Controller 生成

基线：a0ecc74f，2026-10-11。原包目录保持不变，本次不更改线协议、持久化边界或 on_change/periodic 调度语义。

## 实施方案

1. Flush：修订号快照只在当前 subject 的捕获与结算准备间使用，同轮复用一张临时表，减少每个 subject 单独分配。不共享 Prepared 内容，不复用 Transport 已接纳的字节。
2. AOI：串行阶段直接汇集事件；并行计算仍写 observer 私有缓冲，等待整批完成后由协调者归集。本轮完成后 Flush 只取事件，不扫描全部观察者。保持同一 pair 的事件顺序和移除后重建的生命周期。
3. Relation：同一锁下维护 subject 到 observers 的反向索引，Forget 只遍历真正持有关系的观察者；Set/Clear 同步维护正反索引。代价是每条关系额外的索引内存。
4. Lockstep：成员变化时作废广播名单缓存，Tick 复用确定顺序；退出、重绑、旁观者与 Close 都要处理。保持单所有者模型和逐接收者准入错误，不改帧序列。

Controller 已按用户确认接入 player access 的协议生成流程：骨架参数为 player_agent.Context 和对应 PB 请求，业务代码自行维护，重复生成不覆盖业务。手动响应继续走当前连接的 Session.Reply，不能把发往 PlayerID 所有连接的 Push 当请求响应。实际用法见[玩家协议教程](PLAYER-ACCESS.md)。

## Controller 如何生成和维护

协议定义写 `handler=player` 后，`go generate ./...` 会创建缺少的 `game/controllers/player/controller.go` 与对应业务方法文件。Controller 持有 Nest Client，但不会自动执行任何 Entity 操作。每个新方法只有参数、返回类型、中文说明和明确的未实现错误，剩下的逻辑由业务编写。

生成器先解析同包所有非测试 Go 文件，按 Controller 方法名查重；方法移动到其他文件后也不会补出重复方法。类型和 FromRegistry 已存在时复用，只有一半存在则报错，避免猜测业务依赖。文件名占用但没有所需方法时报错。删除协议不删除业务，修改签名后由开发者调整方法并编译验证。

事务生成在暂存目录内工作，新增骨架作为首次创建的业务文件提交；已存在的骨架不覆盖、不删除，也仍参与业务输入并发修改检查。`--check` 单独把骨架纳入内容快照，能发现缺失，但不将其视作允许覆盖的生成文件。直接调用底层 `protocol` 工具时可显式传 `-controllers`；正式业务推荐使用 `go generate` 的事务入口。

## 顺带修复：手动响应后的 typed nil

原实现中，类型为 `*pb.Response` 的 Controller 返回 nil，经泛型注册器包装成 any 后仍有动态类型。Dispatch 仅检查 `response == nil`，结果误入自动编码。业务已经调用 Session.Reply 时，可能再回一条空响应，或被编码错误中断连接。

真实生成工程中的负对照记录为：`response=&{2 7 []} err=<nil> encoded=1 replies=1`，测试因额外的自动响应失败。修复在 Dispatch 编码前识别 nil 指针及其他可为 nil 的值，将其恢复为“不自动回复”；非 nil 响应与业务错误仍走原路径。回归要求返回响应为空、编码次数为 0、手动回复次数为 1。该修复并不提供通用重复请求防护，也不禁止业务主动调用两次 Reply。

## 验证与回退

同机、同工具链、同参数先后运行 Manager Flush、AOI 空闲/稀疏事件、Relation 更新/退出和 Match Tick 基准；保存全部样本和分配量。性能运行不并发执行 build/race。对新状态增加生命周期/并行与重生成保护回归，再执行受影响包、生成工程和 Go/C# 接入验证。

没有收益或引入明显代价的优化应撤回或明确限制，不改变测试断言和负载通过验收。不以微基准推断端到端 P99 或跨机容量；不增加默认延迟合并窗口，不把 Flush 改成并行交付，不新增无锁队列。

## 同机基准结果

2026-10-11，Windows/amd64、Go 1.27.0、AMD Ryzen 7 5800H、GOMAXPROCS=4。基线 a0ecc74f 加入相同 benchmark 后先跑 before，再运行本次实现；两次计时期间均未并发执行本任务的 build/test/race。每组 `200ms × 3`，下表为耗时中位数，分配取该样本值。原始三次样本完整保留：[before](../performance/benchmarks/sync-local-20261011/before.txt)、[after](../performance/benchmarks/sync-local-20261011/after.txt)、[汇总](../performance/benchmarks/sync-local-20261011/medians.csv)、[工具链](../performance/benchmarks/sync-local-20261011/environment.txt)。

| 场景 | 优化前 ns/op | 优化后 ns/op | B/op 前 → 后 | allocs/op 前 → 后 |
| --- | ---: | ---: | ---: | ---: |
| Flush 1 会话、32 实体、1 profile | 66,781 | 54,911 | 50,136 → 50,136 | 245 → 245 |
| Flush 64 会话、32 实体、1 profile | 1,883,592 | 1,573,727 | 1,087,762 → 1,017,203 | 850 → 763 |
| Flush 64 会话、32 实体、4 profiles | 1,977,582 | 1,638,951 | 1,185,375 → 1,114,768 | 948 → 861 |
| Flush 256 会话、32 实体、1 profile | 7,811,413 | 5,535,216 | 4,250,910 → 3,965,370 | 2,389 → 2,305 |
| Flush 256 会话、32 实体、4 profiles | 6,915,964 | 5,858,103 | 4,348,582 → 4,063,040 | 2,488 → 2,405 |
| AOI 1,000 观察者、空闲 | 29,981 | 11.17 | 0 → 0 | 0 → 0 |
| AOI 1,000 观察者、单实体来回移动 | 36,348 | 485.3 | 104 → 72 | 4 → 3 |
| AOI 10,000 观察者、空闲 | 435,472 | 10.5 | 0 → 0 | 0 → 0 |
| AOI 10,000 观察者、单实体来回移动 | 422,628 | 501.6 | 104 → 72 | 4 → 3 |
| Relation 10 观察者、重复 Set | 279.1 | 271.3 | 192 → 192 | 2 → 2 |
| Relation 10 观察者、Forget 后重加 | 669.7 | 648.5 | 288 → 480 | 4 → 6 |
| Relation 10,000 观察者、重复 Set | 297.6 | 270.1 | 192 → 192 | 2 → 2 |
| Relation 10,000 观察者、Forget 后重加 | 269,422 | 659.3 | 288 → 480 | 4 → 6 |
| Lockstep 10 玩家 Tick | 4,576 | 3,656 | 2,600 → 2,394 | 20 → 18 |

如何理解这些数字：

- Flush 的帧数、打包次数与传输字节在前后样本中一致。多会话档分配下降符合复用修订表的目标；单会话分配不变，短测耗时变化不单独归因为该改动。
- AOI 空闲值只包含两个事件来源的 Flush，不包含空间更新、网络发送或实际业务。原来每次遍历全部观察者，现在空事件直接返回。移动档是观察者相距很远、一个实体只影响附近一个观察者的稀疏场景，不代表全员集中移动、并行批量计算或大扇出容量。
- Relation 大表中的退出原本扫描所有观察者，现按反向索引只处理真正订阅者。小表几乎没有耗时收益，Forget/重加却额外增加 192 B 与 2 次分配；静态存量也多持有一份反向关系。这是为大表退出可扩展性保留的明确代价，不是所有关系操作都更省内存。极密集关系需要另做存量容量验证。
- Lockstep 本样本耗时下降约 20%，每 Tick 少 2 次分配；历史仍按原逻辑增长，宿主仍须管理 Trim。没有重测长局容量、网络 P99、15 分钟重档或跨机 HA，不能据此宣布已解决 v1.25 的重档失败。

复跑命令（PowerShell；先切换到需要对比的源码版本，并确保有相同的 benchmark 文件）：

```powershell
$env:GOWORK='off'
$env:GOMAXPROCS='4'
go test ./framework/sync/entitysync ./framework/sync/entitysync/policy ./framework/sync/lockstep -run '^$' -bench 'Benchmark(ManagerFlush|AOIEventCollection|RelationUpdates|MatchTickTenPlayers)$' -benchmem -benchtime=200ms -count=3 -timeout=5m
```

## 行为验证

本地已通过：Sync 全包与 protocol 包测试；policy/lockstep 全包 race；新增 AOI 串行与 4 worker 事件一致性、观察者移除重建顺序（race 下重复 3 次）；Relation 替换/清空/退出/重加；Lockstep 重绑、旧断线、旁观者退出与 Close；根模块 `go build ./...`；Sync 与 codegen 相关包 vet。

生成工程验证覆盖自动创建、已有业务逐字节保留、方法移到其他文件后不重复创建、文件冲突拒绝、事务生成落盘、缺失骨架 `--check`、生成工程实际编译，以及 typed nil 的修前失败/修后通过。既有显式 endpoint 工作流和完整 demo 模板也通过。Go/C# TCP 消费测试覆盖真实生成服务器的协议回包与生成工程 build/vet；不等待 GitHub CI，也没有发布新版本。

结构定位使用 codebase-memory 后以当前源码补证：图谱基线仍为 2026-10-10T16:08:17Z，相关路径报告 metadata_changed，新文件不在该索引中，不能把旧图谱当作本次源码的完整索引。

## 第二批：实际重算与编码工作区

基线 57a1eada。保持原包、公开 API、协议与调度语义，实施三项内部优化：

1. 为串行阶段和每个 AOI worker 提供独占工作区，复用可见 ID、块 ID、查询结果、候选切片和去重表。单容器保留上限 4096 项，最多缓存 32 个并行工作区；超额仍正常计算，只在批次结束释放临时容量。工作区不持有对外事件，不引入常驻 worker。
2. 将 observer 的 dirty 原因分成距离和额外块覆盖。主体物理移动只标记距离；额外覆盖修改只标记覆盖；观察者移动始终重算距离、订阅块集合改变才标记覆盖；首次添加两者都标记。同批原因按位合并，移除并重建使用新实例。
3. 先采样 Flush 分配，再决定复用边界。样本中 frame.Encode 自身占分配字节约 77.76%，session.encode 自身约 15.85%。最终帧已经按精确长度分配，Transport 可能继续持有，不池化这部分；只复用串行 Flush 内会话编码的 ObjectDelta/ComponentDelta 临时切片，归还前清除引用、超过 256 项不保留。每个 Manager 仅一份，逐帧状态与发送字节仍独立。

本批基准覆盖 512 subjects、128 observers、稀疏/集中布局、每批 32 次物理移动或额外覆盖切换、1/4 workers。混合行为测试对照强制全量重算，覆盖同批多原因、主体和观察者退出/重建、距离分档与 MaxVisible。本批不宣称端到端重档通过。

### 第二批测量结果

环境仍为 Windows/amd64、Go 1.27.0、Ryzen 7 5800H、GOMAXPROCS=4。以下是每档 200ms × 3 的耗时中位数与对应样本分配值；计时与 build/race/profile 分开。稀疏布局间距 100，集中布局间距 10；块边长 100、进入/离开半径 120/150、MaxVisible=64。空间档每批前 32 个主体来回移动 80；覆盖档切换两个额外块与空集合。两者均经 beginBatch/endBatch 后收集空间和覆盖事件，不包含网络发送。

| AOI 场景 | worker | 前 → 后 µs/批 | 前 → 后 B/批 | 前 → 后分配次数/批 |
| --- | ---: | ---: | ---: | ---: |
| 稀疏、物理移动 | 1 | 136.94 → 82.74 | 23,963 → 2,960 | 260 → 70 |
| 稀疏、物理移动 | 4 | 128.88 → 95.27 | 25,451 → 4,453 | 284 → 94 |
| 稀疏、覆盖切换 | 1 | 135.29 → 55.64 | 29,526 → 11,522 | 222 → 63 |
| 稀疏、覆盖切换 | 4 | 142.93 → 89.82 | 39,559 → 21,557 | 319 → 160 |
| 集中、物理移动 | 1 | 58,108.85 → 51,212.40 | 6,727,622 → 303,278 | 4,590 → 400 |
| 集中、物理移动 | 4 | 26,942.96 → 21,226.84 | 6,773,013 → 373,604 | 4,777 → 607 |
| 集中、覆盖切换 | 1 | 57,167.90 → 1,558.29 | 7,307,360 → 885,585 | 5,045 → 974 |
| 集中、覆盖切换 | 4 | 26,079.85 → 1,502.70 | 7,479,410 → 1,059,686 | 5,812 → 1,746 |

覆盖切换收益很大，是因为不再触发距离候选排序与 MaxVisible 淘汰；不代表所有移动都能提升同样比例。集中物理移动仍很昂贵，单 worker 一批约 51ms，下一步应分析候选排序与满额淘汰扫描。稀疏档 4 worker 比 1 worker 慢，说明 goroutine 调度开销可能超过小批次的并行收益；本次没有改默认 worker 配置。

| Flush 场景（32 实体，每实体载荷 256 B） | 前 → 后 µs/轮 | 前 → 后 B/轮 | 前 → 后分配次数/轮 |
| --- | ---: | ---: | ---: |
| 1 会话，1 profile | 68.46 → 62.43 | 50,136 → 47,832 | 245 → 243 |
| 64 会话，1 profile | 1,619.02 → 1,489.83 | 1,017,202 → 869,744 | 763 → 635 |
| 64 会话，4 profiles | 1,566.17 → 1,690.50 | 1,114,775 → 967,312 | 861 → 733 |
| 256 会话，1 profile | 5,417.15 → 5,188.20 | 3,965,370 → 3,375,547 | 2,305 → 1,793 |
| 256 会话，4 profiles | 5,621.57 → 5,503.61 | 4,063,034 → 3,473,192 | 2,405 → 1,893 |

Flush 每个会话稳定减少两次临时切片分配，256 会话档每轮节省约 590 KB。帧数、打包次数、wire-B/op 前后一致。首轮 64 会话/4 profiles 的耗时中位数反而上升 7.9%，原始样本完整保留，不能将分配收益表述为所有场景吞吐都提高。

针对该档另做 1s × 5 的串行前后对照，中位数从 1,632.88µs 到 1,597.64µs（约 -2.2%），样本区间明显重叠；861 → 733 次分配、约 147 KB/轮的节省保持一致。因此保留这项优化的依据是稳定减少临时分配，不声称该档已有稳定耗时提升。复测原文：[before](../performance/benchmarks/sync-batch-20261011/flush-repeat-before.txt)、[after](../performance/benchmarks/sync-batch-20261011/flush-repeat-after.txt)。

原始证据：[AOI before](../performance/benchmarks/sync-batch-20261011/before.txt)、[AOI after](../performance/benchmarks/sync-batch-20261011/after.txt)、[AOI 汇总](../performance/benchmarks/sync-batch-20261011/medians.csv)、[Flush before](../performance/benchmarks/sync-batch-20261011/flush-before.txt)、[Flush after](../performance/benchmarks/sync-batch-20261011/flush-after.txt)、[Flush 汇总](../performance/benchmarks/sync-batch-20261011/flush-medians.csv)、[分配字节 profile](../performance/benchmarks/sync-batch-20261011/flush-alloc-space.txt)、[分配数量 profile](../performance/benchmarks/sync-batch-20261011/flush-alloc-objects.txt)。profile 是采样估计，包含测试初始化；样本只去除行尾空格和机器本地 Build ID，不删除性能样本。

复跑 AOI：使用上一节相同环境变量，运行 `go test ./framework/sync/entitysync/policy -run '^$' -bench '^BenchmarkAOIBatchRecompute$' -benchmem -benchtime=200ms -count=3`。Flush 使用已有 BenchmarkManagerFlush。做基线对照时将本批新 benchmark 文件带回 57a1eada；不要把生产优化文件一并复制回基线。

### 回归与测试误报处理

- Sync 全包回归通过。AOI 分类重算在 1/4 worker、80 个混合批次中，与强制全量重算比较空间事件、覆盖事件、可见集合；既有独立距离模型、AOICluster、提交事实门与策略回归均在 policy 整包执行。
- 新增传输保留原始切片的测试：多会话、多帧、连续 Flush 后，每一帧仍与当时副本逐字节相等且可解码；工作区清除组件与载荷引用。entitysync/policy 两包 race 重复两次通过。
- 根模块 `go build ./...`、Sync 全包 vet、真实生成工程的 Go/C# TCP 消费测试通过；后者同时 build/vet 生成工程并运行接入层与 Game 的实际测试。没有改变协议字节格式或要求业务增加配置。
- 首次 race 中 `TestSnapshotIndexAndStatsFollowLifecycle` 失败，前后计数相同，但 OldestSnapshotWait 从 0 增至约 1ms。未修改的 main 基线按 `-race -count=100` 同样复现。旧断言错误地要求两次独立采样的“是否为零”一致；现在验证年龄不倒退，且增量不超过包住两次读取的真实耗时，不加 sleep、不设任意容差、不改运行时统计。修后定向 race 100 次通过。

后续仍可做候选过滤/选择算法、Relation 增量 API、可配置 OnChange 合批与长局 Lockstep 测量；这些不在本批实现中。真实 Gate/Nest/Sync 的重档、P99、跨机及长期容量需要独立验收，不由本批微基准替代。

## 集中布局的候选计算优化（2026-10-11）

本批基线为 `1560fc2d`。目标是减少一个观察者附近实体很多、可见名额已经用满时的重复计算，不增加缓存或改变对外 API。

### 实现与行为边界

原实现把订阅块中的所有未可见实体排序，再逐个尝试进入；满额后，每次尝试都扫描当前可见集来寻找最远成员。现在先排除自身和进入半径外的实体，再按原有距离、ID 顺序排序。如果最近的剩余候选都不能替换最远旧成员，后面的候选也不可能成功，可以立即结束。成功替换时仍产生原来的 Leave/Enter，等距离保留旧成员；最远成员同距时仍淘汰 ID 较小者。

距离过滤继续使用防溢出的 `WithinDistance`，排序和淘汰沿用 `DistanceSquared` 的饱和值语义，没有通过普通乘法简化极大坐标判断。空间事实、覆盖订阅、批次屏障、并行所有权及默认 worker 配置均不变。

同时修复满额淘汰的负数 ID 缺陷：公开 AddSubject 接受任何非零 int64，但旧代码用 `farthest < 0` 判断未找到成员。如果最远成员 ID 为 -2，即使新实体更近也无法替换。现在用独立的 found 标志。修前回归实际得到 `[-2]`，预期 `[3]`，见[失败原文](../performance/benchmarks/sync-candidates-20261011/negative-id-before.txt)。正数 ID 行为不变；负数 ID 恢复应有的淘汰规则。

### 同机测量

Windows/amd64，Go 1.27.0，Ryzen 7 5800H，GOWORK=off，GOMAXPROCS=4。使用现有 BenchmarkAOIBatchRecompute，512 实体、128 观察者、每批移动 32 实体或切换额外覆盖，MaxVisible=64；初始化不计时。前后均为 300ms × 3，测试、race 和编译未与基准并跑。下表为每批耗时中位数，非业务消息耗时。

| 场景 | workers | 优化前 µs | 优化后 µs |
| --- | ---: | ---: | ---: |
| 稀疏、物理移动 | 1 | 80.46 | 67.92 |
| 稀疏、物理移动 | 4 | 95.36 | 91.97 |
| 集中、物理移动 | 1 | 46,674.94 | 8,365.97 |
| 集中、物理移动 | 4 | 19,995.03 | 4,185.68 |
| 稀疏、覆盖切换 | 1 | 52.36 | 50.73 |
| 稀疏、覆盖切换 | 4 | 87.69 | 80.48 |
| 集中、覆盖切换 | 1 | 1,477.76 | 1,473.16 |
| 集中、覆盖切换 | 4 | 1,466.84 | 1,407.92 |

集中物理移动耗时分别减少约 82.1% 和 79.1%。覆盖切换没有执行本批修改的空间路径，其差异视为测量波动。分配不是本批优化目标：集中物理移动单 worker 从约 313 KB/批到 323 KB/批，4 worker 从约 369 KB 到 365 KB；没有宣称所有指标同时改善。

[完整 before](../performance/benchmarks/sync-candidates-20261011/before.txt)、[完整 after](../performance/benchmarks/sync-candidates-20261011/after.txt)、[包含分配的汇总](../performance/benchmarks/sync-candidates-20261011/medians.csv)。复跑命令：`go test ./framework/sync/entitysync/policy -run '^$' -bench '^BenchmarkAOIBatchRecompute$' -benchmem -benchtime=300ms -count=3`，分别在本批基线和修后版本运行。

### 验证方式与上线前事项

对照测试保留优化前的完整候选遍历算法，单对状态机共用修正后的负数 ID 处理。容量设为不限、1、12、64，worker 为 1/4；每组 80 个批次，每批 12 次混合移动、覆盖变更、观察者和实体删除重建，比较空间事件、覆盖事件与可见集。另验证负数 ID、等距离保留旧成员和同距最远成员的淘汰顺序。此对照验证候选算法等价性，不把共享状态机当成独立业务模型证明。

准备正式上线时，需要在实际部署的 macOS/Linux 环境、真实业务 handler 与客户端网络上复跑 Gate/Nest/Sync：分别测常规布局、集中热点、批量入场重连及慢客户端，记录成功完成量、P99/max、拒绝、队列高水位和内存趋势。窗口目标、在线人数及单实体载荷使用业务配置，不直接照搬微基准。至少复跑已有 15 分钟重档，再按计划运行时长验证容量；跨机故障按维护目录的外部验收清单实施。本批 Windows 微基准不扩大支持平台，也不关闭旧重档首错、跨机 HA 或长期容量事项。

本批实际通过：`go test ./framework/sync/... -count=1 -timeout=3m`、`go test -race ./framework/sync/entitysync/policy -count=2 -timeout=4m`、最终候选/等距/负数 ID 定向 race 两次、`go build ./...`、`go vet ./framework/sync/...`、根包文档相对链接检查及 `git diff --check`。本批没有修改协议或生成器；上一批真实生成工程 Go/C# TCP 验证结果见上一节，本批未重复该链路。没有执行上述生产环境容量与故障测试。

图谱项目 roost-core 仍为 2026-10-10T16:08:17Z 代际，相关路径显示 metadata_changed、新测试未索引；本批结论已用工作树当前源码与实际测试补证，不把旧索引当成新实现证据。
