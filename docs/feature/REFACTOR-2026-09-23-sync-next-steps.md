# Sync 后续优化分析与分批方案

09-24 截止更新：本文保留各阶段历史；文中的“事件触发发送暂缓”已由后来授权的[正式双模式接入](IMPLEMENTATION-2026-09-24-sync-modes.md)替代。A～E、六项后续优化及双模式的统一完成状态见[最终截止复核](SYNC-COMPLETION-2026-09-23.md#2026-09-24-最终截止复核)。

日期：2026-09-23。状态：**A～E 已实施并完成回归与目标负载对比，尚未发布。** 第 1～9 节保留实施前方案，当前结果见第 10 节。

实施授权补充：用户随后要求将 A～E 全部实施，并明确 profile 的字段配置、优先级与重复计算三方面都需改善。本轮保留 SyncProfile 线格式，增加显式字段视图配置及可配置优先级，Interest 支持按来源指定视图；规范化和捕获共享集中到内容层。未知字段视图拒绝打包，不自动回退到更宽视图。旧配置继续兼容；事件调度、并行 Flush、分片与网关协议仍是上表独立暂缓项。A/B/D 不改协议；新增配置与工具、回归及实施结果在下文追加。

基线：`967bc69` 加当前工作树的 R1～R3、RR-01～07、M-19/M-20，以及最新低变化率基准。用户认为当前性能已经够用；以 1000 玩家、10000 实体、Interest/AOI 每人约 50 可见、20Hz 和较低变化频率为背景，不继续把 40Hz 或严格 50ms 最大延迟作为本轮改造目标。

## 1. 建议顺序

| 顺序 | 事项 | 主要收益 | 改动风险与建议 |
| --- | --- | --- | --- |
| A | AOI 最远对象选择去掉排序 | 减少 CPU 和临时切片，逻辑也更直接 | 较低；最适合第一小批 |
| B | AOI 候选集合与 Flush 临时数据减分配 | 降低分配和 GC 压力 | 中；逐处验证，不一次引入完整对象池体系 |
| C | 补充运行指标及真实异步传输负载 | 更早发现排队、慢消费者与恢复成本 | 指标较低，接入验证中等；可独立推进 |
| D | 稳定视野下的会话引用表按需复制 | 少量对象变化时避免复制整个会话对象表 | 中高；保护在途隔离和逐帧提交，单独实施 |
| E | 真实业务字段 delta / LOD 调优 | 在业务字段较大时减少字节和序列化 | 依赖真实 schema；先测 payload，不能用当前模拟组件下结论 |
| 暂缓 | 事件触发调度、并行 Flush、空间分片、共享帧/网关多播 | 扩容量或缩短等待 | 当前规模没有迫切需求，复杂度和兼容成本高 |

这里的顺序不是 bug 严重级别；本轮没有建立新的行为缺陷复现，也不声称全模块不存在 bug。

## 2. 性能依据和适用边界

最新[低变化率基准](SYNC-AOI-1000-10000-2026-09-23.md)中，20Hz/1% 的主动工作 p95 为 15.09～15.92ms，服务端分配约 51.15 MB/s；20Hz/5% 为 22.97～29.51ms、约 215.55～215.77 MB/s。当前目标应优先减少重复工作、改善观测与维护成本，无需重写整个 Sync。

已有 **20Hz/10% 压力档** profile 为候选热点提供证据：

- 分配 flat：`observersAt` 15.10%、`evaluateObserver` 14.04%、`Flush` 11.45%、`maps.clone` 10.93%、`sortedVisible` 10.01%、`frame.Encode` 9.98%。
- CPU cumulative：`evictFarther` 16.48%，其中包含 `sortedVisible` 等被调用函数，不能把 cumulative 比例直接相加，也不能等同于可回收收益。
- alloc_space 包含初始化与预热；这不是最新 1% 档位的热点占比。实施前应补同配置 profile，分别保留正式无 profile 的时间样本。
- 本模型设置 MaxVisible=49，加 self 为 50，并且每次状态变化同时移动。容量淘汰因此比较频繁。自然密度约 50、但不设同样硬上限的业务可能收益不同。

原始证据在 `artifacts/perf/sync/aoi-1000-10000-profile/{alloc-top,cpu-cum}.txt`；这些是本地生成文件，报告保留关键数据，不能假定其他 checkout 有原始文件。

## 3. A：AOI 去掉仅为选最大值而进行的排序

源码：[aoi.go](../../sync/entitysync/policy/aoi.go)，`evaluatePair → evictFarther → sortedVisible`。

当前 `evictFarther` 先把可见 subject ID 排序，再扫描最远对象；每次仅需要一个最远对象，却付出一次切片分配和 O(V log V) 排序。在当前 V≈49 时没有必要引入堆或树，单次 O(V) 扫描更容易维护。

具体方案：直接遍历 `observer.visible`，用明确的比较规则选出最远对象。旧实现按 ID 升序遍历，只有距离严格增大才替换，所以**最远距离相同时选择较小 ID**；新实现必须显式保留这条规则。候选对象与最远对象等距时仍不淘汰。保留未知位置跳过、自身排除及原 Leave/Enter 事件顺序。

不删除其他位置的 `sortedVisible`：`evaluateObserver` 和对外查询等位置的确定性顺序有独立意义，不能因为这个调用可去掉排序就全局改成遍历 map。

验收：不同 map 插入顺序、等距对象、边界距离、容量满后的替换，验证相同的可见集合和有序事件流；跑 policy 现有回归和 20Hz/1%、5% 的同配置 A/B，单独记录分配。按该函数单独回退，无 API、协议、配置迁移。

## 4. B：降低 AOI 和 Flush 的临时分配

### AOI：先合并一次查询中的重复操作

`MoveSubject` 先调用 `observersAt(from)`，再调用 `observersAt(to, affected...)`，两次建立去重数据并排序。可合并旧/新格子的观察者，再统一去重、排序一次；若两个格子相同，也只收集一次。**不能因为实体没跨格就跳过 evaluatePair**：格内移动仍可能越过半径、LOD 或最远对象边界。

`evaluateObserver` 还会创建 blocks、candidates 和 seen，并按距离、ID 排序。可在 AOI 的串行调用所有权内复用局部工作切片，设容量保留上限，清理指针；不在 1000 个 observer 上各自永久保留一大块缓冲，也不未经测量就使用全局 sync.Pool。候选近到远、同距按 ID 的次序必须保持，避免产生短暂 Enter+Leave。

不要将多个 Move 直接合成最后一次位置作为“纯性能优化”：这可能改变中途进出事件以及其他观察者的判断，需要独立业务语义设计。

### Flush：先减少容器，后考虑复用

源码：[flush.go](../../sync/entitysync/flush.go)、[subject.go](../../sync/entitysync/subject.go)。每次有 pending 的 Flush 都构造 frames、settlements、sessions、prepared 等；每个 subject 分别收集 snapshot/live profile，又创建 held 集合。

候选步骤：一次遍历订阅收集两类 profile；为常见单 profile 保留简单路径，多 profile 走通用去重；再评估把同一 SessionID 的几份临时数据放入一个私有工作项。以减少查找和分配为目的，不为拆函数新增包。

缓冲复用必须在 `releaseInFlight` 和提交/中止完成后清理，不能让上次的 capturedUpdate、subscription 或 session 指针跨 tick 被覆盖；限制高峰容量保留。`flushGate` 只保证 Flush 串行，不意味着可复用已被传输持有的帧字节。

外层 [frame.Encode](../../sync/frame/codec.go) 已按计算长度一次分配，并使用 `binary.BigEndian.Append*`。其分配高不代表现有写法低效；帧拥有独立字节也是 M-20 共享组件缓存与 Transport 之间的隔离边界。先优化临时描述数据，保留现有帧所有权；更改输出缓冲契约另立方案。

验收：事件顺序、profile 捕获顺序、版本和收发字节不变；异常退出、重试、持久化暂缓、关闭后无残留引用；race 检查及现有 RR-01～07 回归。AOI 和 Flush 分开提交、分开 A/B，不把两者收益混成一份结论。

## 5. C：运行指标和异步传输验证

现有 [Manager.Stats](../../sync/entitysync/manager.go) 提供对象、会话、订阅、pending、准入帧、失败等；[AsyncTransport.Stats](../../sync/nettransport/channel.go) 提供队列数量、发送字节、背压和发送错误。这些可以复用。

建议补充低基数指标：有效/空 Flush 次数和耗时分布、处理的 dirty 数、create/update/remove 数、排队字节、最老消息年龄、全量恢复原因。不要给每个 entity/session 建独立监控标签。Manager.Stats 当前会复制 subject 列表并逐个加锁统计订阅，AsyncTransport.Stats 也逐会话取锁；先用低频采样，别把全量 Stats 接到每个 tick 上。若确实需要高频计数，再以生命周期回归保护增量计数的一致性。

当前 AOI 工具的传输是直接回环 TCP，**没有覆盖 AsyncTransport 的入队、工作协程和发送超时**。下一项更贴近生产的验证是接入真实异步适配器，混入少量暂停读取的客户端，记录正常会话时延、队列字节/年龄、会话退出和重连全量恢复。

具体待测风险：默认 ReliableQueueSize=256 是消息数，MaxReliableBytes=1MiB 是单条上限；有界消息数不等于适合业务的队列字节预算。实际每帧大小和多帧 tick 会影响积压。先测再讨论新增每会话/总字节限额与过期策略；这属于行为/API 扩展，应单独说明默认值和兼容性。

`SendReliable` 目前在检查会话与队列是否可接受之前复制 payload，拒绝路径可能产生无效分配；将复制后移可作为小候选，但会增加锁内复制时间，需要拥塞场景验证。队列现在通过切片头前移出队，可在测到持续分配后再考虑环形队列，当前不为此增加基础设施包。

保留当前“队列满只关闭慢会话”的语义。不能将增量帧换成 latest-only 丢弃队列，也不能把某个客户端背压映射成整个 Manager 的 ErrRetryLater；否则版本链或其他会话都可能受影响。

## 6. D：稳定视野时按需复制会话引用表

源码：[session.go](../../sync/entitysync/session.go)、[flush.go](../../sync/entitysync/flush.go)、[subscriptions.go](../../sync/entitysync/subscriptions.go)。`Flush` 为待发送会话调用 clone，复制 objects、generations、free；分多帧时又需要独立的 next 状态。即使只有已有对象的 update，也复制整张表。

候选方案：会话标量仍独立复制；纯 update 帧只读共享引用表；第一次 allocate/release 前确保可写、复制引用表和 free。Hold/Ready、对象引用重用、逐帧提交必须一起核对。不要直接把 `maps.Clone` 改成普通赋值：后续 create/remove 会改写被旧会话或在途帧共享的 map。

预期受益是低 dirty、少 AOI 进出的稳态更新，比例取决于真实 churn。首次进入、重连全量和高移动档仍可能需要复制，不能承诺消除 profile 中全部 maps.clone 成本。

验收：纯 update、混合 create/remove、满容量替换、多帧前缀成功后失败、ErrRetryLater、Hold/Ready、同 ID 重开与旧 Push 返回，及缓冲改写隔离；race 必须通过。该批比 A/B 风险高，收益不足时保留原实现，单独回退。

## 7. E：字段内容与可读性

框架已有 mask、不同 profile/LOD 和业务 packer 入口。当前 AOI 模拟组件的 snapshot/delta 都编码代表性 BSON，并不证明真实业务已经按字段生成最小 delta。拿到真实 schema 后，先分开统计移动、属性和进入视野全量的字节贡献，再调整业务 packer；减少 1KB payload 比优化几十字节包装可能更有效，但本轮没有对应数据，不能承诺收益。

只改变同步字段值而不改变位置时，业务不必调用 AOI Move；只有位置改变才做空间评估。位移阈值、坐标量化、字段低频发送会影响客户端表现，属于业务取舍；LOD 当前表示视图选择，不能据此声称已有每 LOD 独立发送频率。

可读性后续可补两点：

- `DefaultInterval` 注释当前声称变化在一个 interval 内到达每个会话，应改为“正常调度下的合并检查周期，不含执行、网络和重试耗时”；这与现有实测更一致。
- 在会话引用表、内容版本、订阅意图 revision 的核心注释中维持清楚的三种状态边界。Flush 已按捕获、准入、结算组织，不为压短函数而增加接口、管理器或通用事务抽象。

## 8. 结构、执行与回退

当前与目标包结构相同；全部八包保持原路径：

```text
sync/
├── entitysync/          Flush、会话和订阅；A/B/D 相关私有辅助仍在原包
│   └── policy/          AOI/Interest；A/B 的空间评估优化在此
├── frame/               线格式，保留独立字节所有权
├── nettransport/        异步队列及具体协议；C 的观测和验证
├── lockstep/            输入同步，本轮没有新性能证据支持改造
└── syncbus/
    ├── driver/          NATS/JetStream，本轮未做集群测量
    └── mirror/          服务间副本，本轮不据 AOI 数据推断瓶颈
```

路径映射均为原包 → 原包；`policy → entitysync → frame/nettransport` 与现有 entity/spatial 依赖边界不变。A/B/D 不改变公开 API、导入、存储或线格式，kit/codegen/demo 无需迁移。C 的新队列预算或 E 的业务编码契约若实施，另列配置/客户端迁移说明。

建议先独立落地 A，再选 B 的“旧/新格子只收集排序一次”；之后依据最新低变化率 profile 决定是否继续 Flush 或 D。C 可作为独立观测/验证批次，避免运行基准时与其他耗 CPU 工作并行。

每批：保存改前二进制与参数 → 定向语义回归 → race → 同机串行 1%/5% 重复样本 → 核对数据完整性、CPU/分配、p95/p99 和最大值。保留所有样本，不改场景或延迟门禁掩盖回退。具体收益以 A/B 为准；没有稳定收益就撤回该批，不用维护复杂度换微小提升。

事件触发发送、并行 Flush、空间分片、共享帧协议/网关多播均暂缓。共享组件编码已完成，不应重复列为待办；网关多播仍需网关和客户端配套。当前用户认可容量，不把这些扩展包装成必须完成的收尾。

## 9. 本轮证据范围

使用 codebase-memory Verify，项目 `Users-whb-roost`，代际 `2026-09-23T10:23:29Z`。定位热点、读取精确实现，并跟踪 evictFarther 与 clone 的直接调用关系；相关查询没有未处理的分页。11 个运行时代码证据文件 coverage 为 metadata_match、无记录缺口，仍只代表 best-effort 信号。

图谱把内建 int64 和标准库 maps.Clone 产生了无关的 heuristic 连边；已依据实际源代码排除，没有将其作为依赖证据。工具与 profile 采用已知路径直接读取。前轮 race、容量和性能结论引用既有验收记录；本轮仅写方案和索引文档，没有重新运行生产回归或宣称完成新的性能 A/B。

本轮深入范围是用户实际使用的 Interest/AOI → EntitySync → Frame 与异步传输衔接。lockstep、syncbus/driver/mirror 仅沿用现有包职责和历史收尾范围，并非八包逐函数穷尽审计。

## 10. 实施与验收记录

用户明确授权全部实施，并确认 profile 的字段选择、来源优先级和重复计算都需要改善。本轮按 A～E 落地，未将原先暂缓的事件触发调度、并行 Flush、分片和网关协议混入本次实现。

### 已落地

- **A / AOI**：最远对象直接扫描，保留等距淘汰较小 ID、候选等距不替换；旧/新格子观察者合并后只排序一次，同格仍评估距离。候选与 blocks 缓冲在 AOI 串行所有权内复用，最多保留容量 4096；seen 清空复用，过大集合不保留。未引入全局池。
- **B / Flush**：同会话的身份、entries 和 settlements 聚合为一个私有工作项；held map 按需分配；两类 profile 一次收集，规范化去重集中到 entity。Prepared 捕获直接保留 CommitLSN，避免为查水位复制更新列表；无接收者不再打包默认内容，仍保留水位门槛和脏版本提交。
- **C / 观测与传输**：增加 Flush 次数、空 tick、耗时、捕获数、准入对象数及固定标签的全量原因指标。AsyncTransport 增加排队字节、在途字节、最老消息年龄和可选每会话字节预算；拒绝前不复制 payload，队列复用容量并清理旧引用。新增 `-async` 目标负载路径与慢消费者隔离测试。
- **D / 会话**：clone 先共享只读引用表，首次 allocate/release 才复制；纯 update 只推进独立时钟。多帧提交点、Hold/Ready、同 ID 重开及旧 Push 隔离保持。
- **E / profile**：新增不可变 `SyncViewSet`、`SyncView` / `NamedSyncView`、按字段掩码的 packer 适配器。DataEngine 使用已有生成 DAO 字段词汇表解析名称；不反向引入 Entity→DataEngine 依赖。Manager 支持显式优先级；Interest 先解析来源视图，再用统一规则选择，移除旧 min-band 中间状态。full-dirty 与同 profile 新订阅共享一次 snapshot payload，保留不同版本头。
- **真实字段示例**：Player/Monster demo 的 default 保持全字段，新增可显式订阅的 near/far 白名单与 delta；使用实际生成 DAO 编解码验证字段投影。场景默认选择仍保持旧配置，未声称已经掌握用户生产 schema。接入及兼容说明见 [Sync 视图配置](SYNC-PROFILES.md)。

新字段配置与优先级是可选 API；SyncProfile 和线协议字段未增加。自定义 Interest.Profile 回调若把远档映射成更高优先级视图，应注意本次统一后的规则按实际 profile 选优，不再先折叠为最小 band；默认映射仍保持原结果。Demo 的未知 profile 由过去忽略改为拒绝，需要将自定义视图加入声明。以上行为变化单列，不伪装为纯性能调整。

### 回归与生成工程

- `go test -race ./sync/... ./entity ./dataengine ./scripts/perf/sync-aoi` 通过，覆盖八个 Sync 包。
- 新增字段名称校验、未知 profile 拒绝、配置所有权、字段白名单、full-dirty 共享打包、无人订阅仍守水位、私有字段变化的空 payload 版本推进、来源视图回退、引用表写时复制、AOI 等距规则、队列预算/年龄/字节隔离测试。
- 慢消费者测试使用真实 AsyncTransport 与独立 `net.Pipe`，一端停止读取，另一端逐帧验证时钟、对象引用和内容版本；队列满只关闭慢会话。
- 根依赖边界、robot 与 codegen/internal/roost 回归通过；`go vet` 与 `go build ./...` 通过。
- 最终核心回归再次通过；慢消费者、引用隔离、字段空 delta、优先级和无人订阅水位等新增用例在 race 下连续运行 20 次通过。异步工具另以 30 人/300 实体、2Hz/4 tick 跑 race 烟测，数据校验通过、未报告 race；50ms 门禁按预期失败，该烟测不计入性能结果。
- 临时目录生成完整 game-demo，对真实 Player/Monster 执行 `go test -race`，验证 near/far 的 BSON 字段与单字段 delta；生成工程 `go build ./...` 通过。生成器、目录路径和日志保存在 `artifacts/perf/sync/next-steps-implementation/`。
- 代码索引刷新后代际为 `2026-09-23T13:12:02Z`，15 个主要实现、模板和工具证据路径均为 metadata_match、无记录缺口；新增视图 API、PrepareViews、来源选择和引用表复制可查询。没有据此宣称全仓索引完整。47 个变更 Go 源文件/模板的 gofmt、218 个相关本地链接与 `git diff --check` 通过。

### 同模型 A/B

保存实施前后两个二进制，在同机、各进程 GOMAXPROCS=4 下，20Hz/200 tick，1% 和 5% 各交替运行 before/after 三轮，共 12 个正式样本。测量期间不并行运行测试、profile 或索引。

| 变化比例 | 实施前分配 MB/s | 实施后分配 MB/s | 分配下降（样本中位数） | 实施前主动工作 p95 | 实施后主动工作 p95 |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1% | 51.11～51.14 | 21.43～21.45 | 约 58% | 16.56～17.50ms | 14.42～14.90ms |
| 5% | 215.23～215.50 | 113.12～113.15 | 约 47% | 21.72～50.22ms | 19.63～22.18ms |

5% before 第二轮有明显迟到（计划输入 p99 约 1347ms），完整保留，不将它归因于某个函数，也不只用这一轮计算加速倍数。分配下降在各样本中一致；耗时结论仍受开发机调度影响。

六对 before/after 的帧数、字节数、create/remove/update 数、延迟样本数和最终可见集合一致，12 轮内容校验全部通过。端到端实际变更 p99：1% before 50.69～51.86ms、after 50.66～52.00ms；5% before 49.79～64.70ms、after 50.82～51.11ms。周期等待没有改变，不声称 p99 或最大延迟已达到 50ms，原严格门禁仍返回失败并保留报告。

另用 **1000 人 / 10000 实体 / 约 50 可见 / 20Hz / 1%** 跑真实异步队列三轮：内容、版本和可见集合校验通过；1000 会话存活，SendErrors、ReliableAbandoned、ReliableBackpressure 均为 0，结束时 pending/in-flight 字节为 0。主动工作 p95 10.93～11.58ms、分配约 31.40 MB/s、实际变更 p99 52.17～52.41ms。异步模式增加测试信封分配与入队复制，并把发送移到工作协程，不能用主循环变短声称总 CPU 同比下降。

正式结果：`artifacts/perf/sync/next-{before,after}-20hz-{1,5}pct-{1,2,3}/`，异步结果 `next-async-20hz-1pct-{1,2,3}/`。这些是同机目标模型验证，不替代真实部署、生产字段与客户端网络环境。
