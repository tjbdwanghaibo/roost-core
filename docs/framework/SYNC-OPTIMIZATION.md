# Sync 优化现状与接入安排

更新日期：2026-10-15（Lockstep 自动保留见[末节](#lockstep-自动保留与追帧失败通知2026-10-15)）；此前代码核对基线：`9d97a211`。此前确定的 Sync 局部优化已完成代码实施和本地验证，新增能力随 v1.26.0 发布（[发布说明](../release/v1.26.0-NOTES.md#n7)）。业务接入、目标环境验收和可选的新优化分别列在下面。

## 当前完成范围

| 模块 | 已完成内容 | 使用时需要注意 |
| --- | --- | --- |
| Flush | 修订表与编码工作区复用 | 在途发送字节仍独立，未池化 |
| AOI | 事件归集、分类重算、有界工作区、候选过滤与提前结束 | 小批量不保证并行更快，集中热点仍需目标环境验证 |
| Relation | 反向索引、重复 Set 工作区、QueueRelationDelta 增量入口 | 业务需显式使用增量入口；退出重加存在已记录的耗时代价 |
| OnChange | 可配置固定合批窗口 | 默认 0；开启后增加等待，周期检查可提前触发 |
| Lockstep | 广播名单缓存、滚动历史裁剪与容量回收；默认自动保留窗口（3600 帧）与追帧失败通知（2026-10-15）；全量快照恢复（2026-10-20） | 窗口按允许断线时长 × 帧率核对；没配快照来源时越界追帧明确失败，配了时越界改发业务快照 |
| 玩家 Controller | 自动补齐缺少的方法、保留已有业务、修复 typed nil 响应 | 业务编写方法内容，生成器不会代办 Entity 操作 |

这里的完成范围不等于全框架零缺陷，也不等于生产验收完成。配置和代码示例见 [Sync 使用指南](guide/04-sync.md)，玩家协议接入见 [Controller 教程](PLAYER-ACCESS.md)。后文保留各次改动的基线、测量和失败样本，数字只适用于对应场景，不作为当前待办列表。

## 业务接入：上线前需要确定的选择

| 项目 | 业务需要做什么 | 完成判据 |
| --- | --- | --- |
| OnChange 合批 | 根据延迟要求决定是否设置 on_change_batch_window；0 保持即时唤醒 | 部署配置与预期一致，功能验证中最终状态和版本连续正确 |
| Relation 增量 | 已知新增/移除成员时使用 QueueRelationDelta；完整列表仍可使用 QueueRelation | 通过正式提交入口，回滚不发布，退出重建不被旧事实影响 |
| Lockstep 保留 | 核对默认 3600 帧（60Hz 60 秒 / 30Hz 120 秒）是否覆盖允许的断线时长，不够就按“秒 × 帧率”配置 HistoryRetainFrames；需要整局回放时独立复制并持久化，或配置 RetainFullHistory 自行裁剪 | 正常追帧范围仍可服务；客户端收到恢复失败后有业务处理（踢出、回大厅或开新局）；`lockstep.catchup.unservable.total` 符合预期 |
| Lockstep 快照恢复 | 断线可能超过窗口时提供 `SnapshotProvider`（服务器参考模拟发布检查点），Recovery 零值即 auto；按快照大小与最低带宽核对 `MaxBytes`、每 Tick 预算（独立 Gate 下尤其要按[实测建议](guide/04-sync.md#经嵌入-tcp-追帧随链路放慢)配置）；客户端接 restore 并核对状态 hash | 越界重连经快照回到头部、状态 hash 一致；`lockstep.snapshot.failed.total` 符合预期 |
| 版本升级 | 发布时按维护流程执行；业务固定新版本并编译消费者 | 新配置/API 随正式版本交付，不能只升级文档而仍使用旧 tag |

Lockstep 默认保留 3600 帧，依据见[使用指南](guide/04-sync.md#长局如何控制内存历史保留窗口)；它不是适合所有游戏的唯一值。没配快照来源（catchup 模式）时，过期追帧返回 ErrCatchupUnservable 并给客户端发恢复失败通知，业务必须明确选择处理方式（重新入场或退出对局并给出原因）；配了 `SnapshotProvider` 时越界改发业务快照（[快照恢复](#lockstep-快照恢复2026-10-20)）。框架不会凭输入历史自动生成权威快照，快照内容来自业务的服务器参考模拟。hash 报告默认跟随同一窗口；判定窗口更短时宿主可再调用 TrimHashReports。

## 尚待目标环境验收

这些事项含正确性与恢复验证，不只是吞吐测试。已有本地回归继续有效，但不能替代真实客户端与部署环境。

| 类别 | 场景 | 需要留下的结果 |
| --- | --- | --- |
| 恢复正确性 | 断线重连、裁剪边界内外追帧、慢客户端、连接替换 | 无版本/帧序号缺口、旧连接不污染新连接、失败有明确终态 |
| 故障稳定性 | 跨机断连、丢包、进程退出及恢复 | 在既定超时内退出或恢复，资源与队列最终收敛 |
| 性能与容量 | 集中热点、批量入场、真实业务、长时间运行 | 成功完成量、P99/max、拒绝、队列高水位与进程内存趋势 |
| 客户端兼容 | 实际使用的 Unity/Godot/Unreal 与目标平台 | 协议处理、线程使用、重连与确定性符合业务要求 |

详细环境与判据见 [外部验证清单](../maintenance/EXTERNAL-VERIFICATION.md)和[已知限制](../maintenance/KNOWN-LIMITS.md)。本地 108000 帧测试是逻辑长局，不是已经跑过真实网络一小时；旧重档失败的根因也没有因此关闭。

## 可选的新优化

以下均为后续候选，尚未实施，不计入已经完成的原定优化范围。

| 方向 | 解决什么问题 | 开始前需要的依据 |
| --- | --- | --- |
| AOI 候选选择算法 | 进一步减少密集场景的排序与扫描 | 当前负载 profile 仍显示这些步骤是主要开销，并能保持距离/同距顺序语义 |
| AOI 小批量串并行选择 | 避免小任务的并行调度成本高于计算收益 | 同环境不同批量对照，明确切换规则；不能硬编码某次微基准的阈值 |

建议顺序：先完成 Lockstep 保留窗口与过期恢复的真实故障验证（正常追帧、慢追帧、过期失败与客户端终止、回放所有权），再根据实际负载选择算法优化。

## 已实施优化的设计与证据

以下记录从基线 `a0ecc74f` 开始。原包目录、线协议和持久化边界保持不变；OnChange 后续增加了显式可选的合批窗口，零值保持原行为。

### 首批内部优化

1. Flush：修订号快照只在当前 subject 的捕获与结算准备间使用，同轮复用一张临时表，减少每个 subject 单独分配。不共享 Prepared 内容，不复用 Transport 已接纳的字节。
2. AOI：串行阶段直接汇集事件；并行计算仍写 observer 私有缓冲，等待整批完成后由协调者归集。本轮完成后 Flush 只取事件，不扫描全部观察者。保持同一 pair 的事件顺序和移除后重建的生命周期。
3. Relation：同一锁下维护 subject 到 observers 的反向索引，Forget 只遍历真正持有关系的观察者；Set/Clear 同步维护正反索引。代价是每条关系额外的索引内存。
4. Lockstep：成员变化时作废广播名单缓存，Tick 复用确定顺序；退出、重绑、旁观者与 Close 都要处理。保持单所有者模型和逐接收者准入错误，不改帧序列。

Controller 已按用户确认接入 player access 的协议生成流程：骨架参数为 playeraccess.Context 和对应 PB 请求，业务代码自行维护，重复生成不覆盖业务。手动响应继续走当前连接的 Session.Reply，不能把发往 PlayerID 所有连接的 Push 当请求响应。实际用法见[玩家协议教程](PLAYER-ACCESS.md)。

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

覆盖切换收益很大，是因为不再触发距离候选排序与 MaxVisible 淘汰；不代表所有移动都能提升同样比例。该阶段集中物理移动单 worker 一批约 51ms；下文候选计算优化已进一步降低开销，这里的数值仅代表该阶段样本。稀疏档 4 worker 比 1 worker 慢，说明 goroutine 调度开销可能超过小批次的并行收益；本次没有改默认 worker 配置。

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

候选过滤与提前结束、Relation 增量 API、OnChange 合批及本地逻辑长局验证均已在后续章节所述版本实施。真实 Gate/Nest/Sync 的重档、P99、跨机及长期容量需要独立验收，不由本批微基准替代。

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

## Relation 重复更新实现（2026-10-12）

基线 9e064702。此前三批局部优化已交付，但不代表全部可选优化或上线验收完成。本轮针对重复 Set 每次构建新关系表的分配：在 RelationSource 锁保护下复用一份有上限的候选集合，仅在成员确实变化时转交为正式集合；不变时清空后复用。候选表不得与存量成员表同时共享，不保留超大输入的临时容量。保留零 ID 过滤、自关系、重复 ID 去重、负数 ID、正反索引和同 pair 事件顺序。

复用会把候选构表放入锁内，因此除单成员外，必须测量 32/1024/8192 成员与并发 Set；如锁竞争或大表成本明显退化则调整或撤回。不会修改 QueueRelation 的提交门和生命周期，也不通过新增直接更新接口绕开它。

### 实测结果与取舍

最终采用 1024 个输入成员的工作区上限，大集合仍锁外构表。输入含重复成员时也按输入长度选择路径，避免大量重复 ID 延长锁占用。工作区只有一份，属于 RelationSource，不属于每个 observer；成员发生变化时工作区转交正式集合，下次按需重新分配。因此收益集中在关系不变的重复推送，不能理解成所有关系更新都零分配。Set 仍是 O(n) 全量差分；只处理变化成员时使用下文已实施的 QueueRelationDelta。

环境：Windows/amd64，Go 1.27.0，Ryzen 7 5800H，GOMAXPROCS=4、GOWORK=off；基线 9e064702，新增规模基准同样在修前运行。RelationUpdates 为 500ms × 3，SetSize 为 300ms × 3。表中为三次中位数，单位 ns/op；parallel 是同一个 source 的并发调用吞吐摊销，不是请求 P99。

| 场景 | 修前 → 修后 ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: |
| 10 观察者，单成员重复 Set | 280.0 → 166.5 | 192 → 0 | 2 → 0 |
| 10,000 观察者，单成员重复 Set | 261.7 → 145.1 | 192 → 0 | 2 → 0 |
| 32 成员，串行重复 Set | 2,741 → 2,052 | 1,240 → 0 | 4 → 0 |
| 32 成员，并发重复 Set | 3,067 → 2,819 | 1,240 → 0 | 4 → 0 |
| 1024 成员，串行重复 Set | 75,891 → 67,375 | 36,992 → 6 | 6 → 0 |
| 1024 成员，并发重复 Set | 78,226 → 66,821 | 36,992 → 8 | 6 → 0 |
| 8192 成员，串行重复 Set | 718,757 → 693,220 | 295,600 → 295,600 | 34 → 34 |
| 8192 成员，并发重复 Set | 624,855 → 594,969 | 295,601 → 295,601 | 34 → 34 |
| 10 观察者，退出重加 | 624.7 → 668.6 | 480 → 480 | 6 → 6 |
| 10,000 观察者，退出重加 | 618.3 → 637.7 | 480 → 480 | 6 → 6 |

1024 成员的少量摊销字节来自首次构表，不代表逐次持续分配。8192 档没有工作区复用，不把短测差异归因于大表优化。

退出重加首轮约慢 7%/3.1%，因此对 10 观察者档单独做 1s × 5，修前中位数 622.5ns，修后 653.0ns，仍约慢 4.9%（约 31ns/次），分配保持 480 B/6 次。这是分支和所有权处理增加后的明确代价；保留本优化的依据是重复 Set 明显减少分配和耗时，而非所有操作都变快。高频退出重建业务不能套用重复 Set 的收益，需要按自己的变化率测量。

完整证据：[原始基线](../performance/benchmarks/sync-relation-20261012/before.txt)、[修后及额外500ms样本](../performance/benchmarks/sync-relation-20261012/after.txt)、[规模基线](../performance/benchmarks/sync-relation-20261012/size-before.txt)、[同口径规模结果](../performance/benchmarks/sync-relation-20261012/size-after.txt)、[汇总](../performance/benchmarks/sync-relation-20261012/medians.csv)、[退出重加长测前](../performance/benchmarks/sync-relation-20261012/repeat-before.txt)、[长测后](../performance/benchmarks/sync-relation-20261012/repeat-after.txt)。另有一次长测启动时构建进程尚未确认退出，该[重叠样本](../performance/benchmarks/sync-relation-20261012/repeat-before-overlap-excluded.txt)完整保留并排除，不参与任何前后结论；正式长测在构建结束后串行重跑。

复跑：`go test ./framework/sync/entitysync/policy -run '^$' -bench '^BenchmarkRelationUpdates$' -benchmem -benchtime=500ms -count=3`；规模组替换为 `-bench '^BenchmarkRelationSetSize$' -benchtime=300ms`。旧版本对照只复制新增 benchmark，不复制实现。

### Relation 重复更新的验证

已通过 Sync 全包测试、policy 全包 race 两次、根模块 `go build ./...`、Sync 全包 vet。独立集合模型连续执行 1500 次操作，核对成员、正反索引和批次事件；并发回归覆盖 Set/Forget/Clear/Flush，另覆盖大表换小表、重复 ID、零/负数/自关系、输入切片修改。协议、生成器和提交事实队列未修改，不要求业务更换 API。图谱仍为 2026-10-10T16:08:17Z，相关路径标记过期，已按当前源码补证。

## Relation 增量更新（2026-10-12，基线 9a34d1d2）

本批实施 QueueRelationDelta(entity, source, added, removed)，只复制变化的 ID，经现有事实队列的确认、回滚、顺序、预算和发布屏障后应用。底层 RelationSource.ApplyDelta 只访问变化成员并同时维护正反索引；普通业务 handler 使用 QueueRelationDelta，不能绕过提交门直接修改 source。

语义为 next = (previous ∪ added) − removed，同一批删除优先，零 ID 忽略，重复 ID 幂等，自关系和负数 ID 合法。多次调用仍按队列顺序生效；Set 是全量替换，不与增量跨调用合并。Leave 清除观察者的旧事实；Hide/Leave 将退出 ID 从待应用的新增和移除集合中剔除，防止旧新增在同 ID 重建后复活。不增加消息格式、持久化格式或默认延迟。

### 全量与增量的等价负载测量

Windows/amd64、Go 1.27.0、Ryzen 7 5800H，GOWORK=off、GOMAXPROCS=4，500ms × 3，中位数。两条路径在同一构建中运行，已有 Set 实现未修改。每轮从固定规模的存量集合中移除一人、加入一人，Flush 必须恰好产生两个事件，最后核对完整成员表；初始化不计时。计时期间未并跑本任务的测试、race、构建或索引重建。

| 存量成员 | 全量 Set ns/op | 增量 ApplyDelta ns/op | B/op 全量 → 增量 | 分配次数 全量 → 增量 |
| --- | ---: | ---: | ---: | ---: |
| 1 | 597.7 | 480.9 | 480 → 288 | 6 → 4 |
| 32 | 2,760 | 450.8 | 1,528 → 288 | 8 → 4 |
| 1024 | 72,695 | 455.8 | 37,280 → 288 | 10 → 4 |
| 8192 | 652,090 | 476.0 | 295,889 → 288 | 38 → 4 |

增量避免随存量大小扫描、构建全表，处理成本主要随变化量增长。该测量仅含关系源及其事件收集，不含 Queue 的切片复制、政策聚合、订阅调整、DAO、网络和业务 handler；不能把表中比例当整服 TPS 提升。新 API 是显式接入，不会自动优化仍调用全量 QueueRelation 的业务。此次没有改变上一批 Set 的行为或退出重加代价。

[全部原始样本](../performance/benchmarks/sync-delta-20261012/benchmark.txt)、[中位数汇总](../performance/benchmarks/sync-delta-20261012/medians.csv)。复跑：`go test ./framework/sync/entitysync/policy -run '^$' -bench '^BenchmarkRelationMemberChange$' -benchmem -benchtime=500ms -count=3`。

### 正式链路与验证边界

新增增量与 Set 共用 queueFact，继续检查 entity/source、队列关闭和容量；输入切片复制，不保存业务切片引用。applyQueued 的 ready/discarded 判断、同 ID 未确认前缀和同提交整体应用规则不变。发布屏障统一在应用、丢弃或关闭时释放；没有新增异步任务或持久化格式。

回归覆盖：1200 次随机增量与全量集合模型比较事件和正反索引；零/负数/自关系、重复与新增移除重叠；Set/Delta 混合、未确认提交、回滚、同 ID 顺序、批次额度小于同提交事实数；其他来源保留；Leave/Hide、退出再进入、待删除 ID 重建；非法参数、预算拒绝、Close、发布屏障及并发 Set/Delta/Forget/Clear/Flush。

业务示例见[使用指南](guide/04-sync.md)。Relation 增量已实施。OnChange 合批及 Lockstep 本地长局验证见下节；生产环境真实业务、跨机和长期容量仍需验证。图谱仍为旧代际 2026-10-10T16:08:17Z，调用关系存在漏报及误连，以相关当前源码与实际回归补证。

本批实际通过：`go test ./framework/sync/... -count=1 -timeout=3m`、`go test -race ./framework/sync/entitysync/policy -count=2 -timeout=4m`、最终增量/并发生命周期定向 race 三次、`go build ./...`、`go vet ./framework/sync/...`、根包文档链接检查和 diff 检查。未运行新增真实网络、跨机或长期容量负载。

内存取舍：增量原地修改活动成员表，删除部分成员不保证 Go map 立即释放历史峰值容量；删除最后一个成员时移除整张表。事实结构新增增量元数据，队列仍按原事实条数限额，本基准没有测量队列常驻内存变化。上线前需将关系峰值和变化率纳入长时间容量测试。

## OnChange 合批与 Lockstep 长局实现（2026-10-12）

基线 b1ec31a3。OnChange 新增 OnChangeBatchWindow，零值保持立即处理唤醒；正值从首次唤醒起计时，后续唤醒不续期，周期 tick 可提前触发。显式 Flush、Drain 与 Stop 的最终 Flush 不等待窗口。只延迟同步循环的唤醒处理，不改变冻结、提交水位、可靠增量或快照预算。接入 sync.entity.on_change_batch_window，拒绝负值及 periodic 模式下非零配置。

Lockstep History.TrimBefore 原来每次分配并复制剩余帧；现已改为清除已丢弃引用、推进连续视图，大幅缩容时才复制。空历史释放数组，持续裁剪保持容量与保留窗口同阶。ReadRange 仍返回连续只读页，跨 Append/Trim 不允许继续持有旧视图；发送追帧继续使用编码后的独立载荷。（当时默认不自动丢弃历史；2026-10-15 起默认按保留窗口自动裁剪，见末节。）长局测试覆盖持续推进、裁剪、页读取、序号和慢追帧隔离；模拟长局不冒充真实多小时网络验收。

### 使用方式与实现取舍

正式配置与业务保留窗口示例见[Sync 使用指南](guide/04-sync.md)。合批采用首次通知起算的固定窗口，持续更新不会让截止时间不断后移；它减少的是 Flush/发送批次，不减少提交时捕获变化的工作，也不保证网络延迟。默认窗口为零，现有应用不主动开启就保持即时唤醒。周期 tick 可以提前处理积压，显式 Flush、Drain、Stop 不等待窗口。

History 不再每裁掉一帧就复制剩余历史。通常只清除丢弃部分的引用并移动切片起点；需要缩容时才复制剩余帧。额外记录完整底层数组容量，因为仅看移动后的切片 cap 会漏掉仍被保留的旧前缀。逐步缩小和一次大幅缩小均有回归；清空历史释放数组。返回页是只读临时视图，下一次 Append/TrimBefore 后不可继续持有；长期保存回放必须复制需要保留的数据。追帧先编码为独立字节再异步发送，沿用原有发送所有权。

### 本地结果

基线为 b1ec31a3，Go 1.27.0，Windows/amd64，Ryzen 7 5800H，GOMAXPROCS=4。基准每组 300ms、三次，表内为中位数；测试、race、编译与性能测量串行执行。微基准只含追加、裁掉一帧、读取 32 帧页，不含业务、网络和载荷分配。

| 保留窗口 | 原耗时 ns/op | 最终耗时 ns/op | 原 B/op | 最终 B/op |
| --- | ---: | ---: | ---: | ---: |
| 300 帧 | 7086 | 40.37 | 28159 | 67 |
| 18000 帧 | 346829 | 74.49 | 1309985 | 152 |

最终输出的 0 allocs/op 是摊销后取整，仍存在偶发扩容/缩容分配。保留[原始基准](../performance/benchmarks/sync-batching-20261012/history-before.txt)、[最终基准](../performance/benchmarks/sync-batching-20261012/history-after.txt)以及[容量修正前样本](../performance/benchmarks/sync-batching-20261012/history-after-initial.txt)，不把该微基准改善倍数当成整服吞吐提升。

OnChange 使用确定性虚拟时间：每 1ms 提交一次，共 20 次，零/5ms/10ms 窗口分别发送 20/4/2 个更新帧；检查版本、增量基线衔接、最终值为 20、冻结内存排空。覆盖固定截止时间、周期提前处理、显式 Flush、Stop、取消后重启、零值及装配覆盖规则。这是行为测试，不是端到端延迟测量。

Lockstep 推进 108000 次 Tick，10 位玩家每帧提交 8 字节输入，相当于 30Hz 一小时的逻辑帧量。保留 1800 帧时底层帧数组峰值为 2560 个槽位；每 30 帧报告 hash，裁剪后最多保留 60 个报告帧。随后继续推进与裁剪，29 页追帧接回直播，核对序号无缺口及输入载荷；过期历史明确返回 ErrCatchupUnservable。这里的峰值是帧数组容量，不是整个进程的 RSS。已有慢追帧不阻塞 Tick 的回归也随 Lockstep 整包通过。

### 边界缺陷及修复证据

同一轮复现并修复两处 History 边界问题：ReadRange 从第二帧读取且 limit=math.MaxInt 时，旧 offset+limit 溢出导致 panic；现在先按剩余长度限制 limit。TrimBefore 在历史接近 uint32 最大帧号时，旧 first+len 溢出导致错误裁空；现在按差值限制裁剪数量。分别保存[修前读取失败](../performance/benchmarks/sync-batching-20261012/history-boundary-before.txt)与[修前裁剪失败](../performance/benchmarks/sync-batching-20261012/history-trim-before.txt)，最终整包回归包含这两个边界用例。该修复不代表 Match 支持帧号回绕。

最终实际通过：Sync 全部子包与 wiring/nest 测试；entitysync、lockstep、wiring/nest 的 race 两次；长局/渐进缩容定向测试；go build ./...；Sync 与 wiring/nest 的 go vet。正式生成工程的 TestClientSDKAgainstGeneratedPlayerTCP 也通过，包含 Go/C# TCP 消费链路。日志见[普通回归](../performance/benchmarks/sync-batching-20261012/tests-final.txt)、[race](../performance/benchmarks/sync-batching-20261012/race-final.txt)、[长局](../performance/benchmarks/sync-batching-20261012/long-final.txt)、[合批场景](../performance/benchmarks/sync-batching-20261012/final-scenarios.txt)与[生成消费者](../performance/benchmarks/sync-batching-20261012/generated-client.txt)。

复跑入口：`go test ./framework/sync/... ./wiring/nest -count=1`；`go test -race ./framework/sync/entitysync ./framework/sync/lockstep ./wiring/nest -count=2`；`go test ./framework/sync/lockstep -run '^$' -bench '^BenchmarkHistoryRollingWindow$' -benchmem -benchtime=300ms -count=3`。基准对照需要把当前 history_long_test.go 放回基线 worktree，仅运行 benchmark，不能用不同测试负载比较。

尚未验收真实一小时网络运行、跨机丢包/断线、生产负载与进程长期内存曲线。（当时宿主须主动调用 TrimHistory 和 TrimHashReports；2026-10-15 起默认自动保留，见末节。）Windows 本地结果不扩大仓库正式平台支持范围，也不替代上线前在目标环境的容量验收。图谱仍是旧代际，当前修改和新增测试均已读取源码补证。

## Lockstep 自动保留与追帧失败通知（2026-10-15）

基线 `45897a0c`；维护者 2026-10-15 决定 [DESIGN-TODO D1](../maintenance/DESIGN-TODO.md#d1-lockstep-历史的保留与过期策略) 按推荐方案 C 实施，提交 `a6376934`。

**做了什么**：`MatchConfig.HistoryRetainFrames` 默认 3600 帧自动保留（`RetainFullHistory` 关闭，正值下限 `MinHistoryRetainFrames` = `ReplayHorizon` = 64）；`Tick` 在发出本次追帧页之后裁历史，hash 报告按 64 帧步长跟随窗口。越界追帧在 `StartCatchup` 时即返回 `*CatchupUnservableError`，下一次 `Tick` 经可靠通道发 C9 v1 追帧失败通知；Go `FrameAssembler` / `robot.LockstepBot` 与 C# `LockstepAssembler` / `LockstepClient` 收到后终止本场，不再请求追帧。使用说明见[使用指南](guide/04-sync.md#越界的处理按恢复方式区分)。2026-10-20 起 C9 v1 已被 v2（带失败原因）取代，越界在 auto / snapshot 模式下改走快照（下一节）。

**裁剪放在追帧之后的原因**：若在切帧后立即裁剪，从当前最早帧开始的追帧会在本次 `Tick` 的追帧阶段就被判定越界，窗口实际比配置少一帧且“从最早帧追”永远失败（实现时 `TestMatchAutoRetentionOvertakesStalledCatchup` 首版即因此失败）。页面在发出前已编码成独立字节，之后裁剪不影响在途页。

**一并修复**：起点 0 原先跳过越界检查，`History.ReadRange` 把 0 钳到保留的第一帧，裁剪后从 0 追帧会得到一段有洞的帧流而没有任何错误；现在 0 按帧 1 核对。`History` 裁空后用内部起点判定，不再把“裁空”当成“全部可服务”。

**修前失败**（`45897a0c`，同一断言的临时红测试，Go 1.27 / darwin）：

```text
--- FAIL: TestRedDefaultRetentionBoundsLongMatch
    history len 10900 after 10900 ticks, want 3600
--- FAIL: TestRedCatchupBeyondRetentionFailsAtRequest
    StartCatchup from trimmed frame = <nil>, want ErrCatchupUnservable
--- FAIL: TestRedCatchupFromZeroAfterTrim
    catch-up from 0 after trim: tick err <nil>, paged 10 frames, first paged frame 20 (frames before it silently skipped)
```

**回归**：`history_long_test.go` 的 `TestMatchDefaultRetentionBoundsLongMatch`（10900 帧后历史恰为 3600 帧、底层数组 ≤ 4 倍窗口、hash 报告 ≤ (窗口 + 步长)/30 + 1）、`TestMatchAutoRetentionOvertakesStalledCatchup`（发送失败使游标被窗口越过 → 下一次 Tick 明确失败、只收到通知、随后回到实时广播）、`TestMatchRetainFullHistoryKeepsEveryFrame`（关闭后历史与报告保留整局、从帧 1 追帧无缺口）、`TestMatchHistoryRetainFramesValidation`；`match_test.go` 的 `TestMatchCatchupAbandonedWhenHistoryTrimmed`（请求即失败、可靠通道只有一条通知、客户端解出 From/Start、起点 0）与指标钉住；`robot` 的 `TestLockstepBotCatchupUnservableNoticeIsTerminal` / `TestLockstepBotUnservableCatchupRequestIsTerminal`；C# `TestLockstep`（通知字段、坏通知拒绝、组装器不变）与 `LockstepGapRetry`（客户端终止、不再模拟）；`client/spec/packets.json` 新增 `catchup_unservable` 金样。

**行为变化**：`StartCatchup` 对已裁起点由“接受、下一 Tick 放弃”改为立即返回错误（关闭自动裁剪、宿主手动裁剪时同样如此）；`CatchingUp` 在通知发出前仍为真。线协议新增 C9 v1 下行通知，旧客户端会把它当坏包终止（未上线，不做兼容）。

**未验证**：真实网络下断线超过窗口的端到端演练、Unity/IL2CPP 上的 C# 行为、生成工程（demo 战斗只有 45 帧，不会触发裁剪）。


## Lockstep 快照恢复（2026-10-20）

维护者 2026-10-20 决定把 [DESIGN-TODO D1](../maintenance/DESIGN-TODO.md#d1-lockstep-历史的保留与过期策略) 的方案 B 立项，设计（`fe9453dd`）同日批准、§12 十三个决定点全部按推荐，分五期实施：P1 服务端核心（`f33e1b01`、`a02c3999`）、P2 Go 客户端（`69fa9e79`）、P3 C# SDK（`f03a6602`）、P4 示例与验收（`8781b32a`）、P5 现行文档。设计、各期实施记录与偏差见 [FEATURE-2026-10-20-lockstep-snapshot-recovery](../maintenance/FEATURE-2026-10-20-lockstep-snapshot-recovery.md)；用法见[使用指南](guide/04-sync.md#落后的会话怎样回来追帧快照或自动)，实现契约见 [impl/04-sync §2D](impl/04-sync.md#2d-lockstep-快照恢复的实现契约)。

**做了什么**：`MatchConfig.Recovery`（catchup / snapshot / auto，零值在配了 `Snapshots.Provider` 时为 auto，否则为 catchup）与 `SnapshotConfig`；业务快照来源 `SnapshotProvider` 在 Match 自己的 goroutine 上调用、同一局至多一次在途、多会话共享，`Tick` 只非阻塞收结果；快照以 `CA 01` 分块经可靠通道下发（首块带 SHA-256 摘要与可选状态 hash），每会话一批在途，随链路放慢的通道按 `BytesPerTick`、只准入的通道（独立 Gate）按 `UnpacedBytesPerTick` 节流（`nettransport.ReliablePacer`）；快照之后从快照帧 +1 续发历史页；恢复期间历史钉住，最多多留 `PinFrames` 帧；恢复失败通知 C9 升 v2（带原因）。Go `FrameAssembler.Receive` / `ResumeAfterSnapshot`、`robot.LockstepBot.Restore`，C# `LockstepClient` 的 `restore`。P5 按维护者追加决定，恢复进行中的会话被 Detach 计 `lockstep.snapshot.failed.total{reason="detached"}`（`TestMatchSnapshotRecoveryDetachIsCounted`；修前同一用例报 `... = 0 after detaching a session waiting, want 1`），独立 Gate 下“断开—重连—从头重传”的循环因此在失败指标上可见。

**为什么不改缺省窗口**：快照让窗口不再决定“断线多久还能回来”，但 3600 帧在 10 席下只有约 1.5MB / 局，缩短窗口省下的内存少，却会把更多短断线推到更贵的快照，并依赖业务的检查点间隔（设计 §10，§12 第 9 条）。缺省仍是 3600 帧，等有游戏按真实断线分布与快照大小测过再定。

**热路径**：没有恢复中会话时 `Tick` 只多一次空 map 判断；P1 前后 `BenchmarkHistoryRollingWindow` 与 Match Tick 的 benchstat 无差异（[样本](../performance/benchmarks/lockstep-snapshot-p1-20261020/benchstat.txt)）。

**修前现行行为**（新功能没有修前缺陷，取同一场景在旧代码或去掉快照来源时的表现，原文）：

```text
TestMatchAutoRecoveryServesOutOfWindowRequestWithSnapshot：out-of-window request with a snapshot provider (auto): lockstep match: catch-up range trimmed from history: from 5, history starts at 20
Example_snapshotRecovery（去掉 Provider）：panic: lockstep match: catch-up range trimmed from history: from 21, history starts at 137
MOBA（去掉 Provider）：player 2 3 from frame 41: lockstep match: catch-up range trimmed from history: from 41, history starts at 232
```

**验收**：包内判定表、Provider 线程、错误语义、分块节奏与重发、钉住、长局与两个 fuzz（`snapshot_test.go`）；真实回环 TCP 4 MiB 快照与 bot 断线 4 个窗口后恢复（`tcp_test.go`）；`Example_snapshotRecovery`；MOBA 十个 KCP 客户端中两人断线超过 4 个窗口后经 Arena 检查点恢复、局末同摘要（直接驱动 Match 一条，经持久对局 `Room` 与已提交门一条）；Room 下一帧提交卡住 / 结果未知时可靠通道只发到已提交帧（`TestMOBARoomCatchupNeverSendsUncommittedFrames`）；生成工程 C# 客户端经真实 TCP 断线 200 帧后重新登录、经快照恢复；toxiproxy 限速演练（[LOCKSTEP-SNAPSHOT-DRILL-20261020](../performance/LOCKSTEP-SNAPSHOT-DRILL-20261020.md)）：嵌入 TCP 在 256 KB/s、64 KB/s 下 1 MiB 与 4 MiB 都恢复、连接不断，传输耗时与“大小 / 速率”之比 1.00～1.04；独立 Gate 在 64 KB/s（1 MiB）与 256 KB/s（4 MiB）下被 Gate 写 socket 超时断开、重连从头再传，未恢复。

**行为变化与升级**：配了 Provider 的 Match 越界不再返回 `*CatchupUnservableError`；C9 v1 删除、新增 CA 01 下行消息，旧客户端 SDK 把它们当坏包终止，升级需同时更新服务端与 Go / C# 客户端（项目未上线，不做兼容）。

**已提交门（2026-10-20）**：“先切帧、后提交、提交后才广播”的宿主（MOBA `Room`）配 `MatchConfig.CommittedFrame`，追帧页与取快照的上界停在已提交帧，空闲的恢复会话落进冗余包覆盖范围即切回实时；nil 时行为不变。没有恢复中会话的 Tick 不经过门，热路径前后对照无差异（[样本](../performance/benchmarks/lockstep-room-gate-20261020/benchstat.txt)：`MatchTickTenPlayers` 1.147µs → 1.079µs，p=0.616，n=10；分配不变）。设计见 [FEATURE §15](../maintenance/FEATURE-2026-10-20-lockstep-snapshot-recovery.md#15-room-接入已提交门2026-10-20)。

**未验证**：真实移动网络 / 跨机下的快照恢复；Unity 编辑器与 IL2CPP 上的 C# SHA-256；独立 Gate 下断开是哪一个写期限先到期；持久对局 `Room` 在替身权威下有 KCP / Nest 回归，真实 strict WAL / Mongo 下有 SIGKILL 冷加载后经已提交检查点快照恢复的演练（下行是记录器、不经 KCP），不含真实 WAL 的提交延迟分布、宿主交接与快照恢复同时发生。剩余边界见 [KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md#lockstep)。
