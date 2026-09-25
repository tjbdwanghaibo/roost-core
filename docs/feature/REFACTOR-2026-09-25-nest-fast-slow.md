# Nest 快慢双池与跨池同 ID 顺序

状态：已实施，七包 race、vet、边界负对照、真实负载与 21/21 故障矩阵验收完成；未部署。100 TPS 短测通过，120 TPS 过载；本轮未跑长稳。沿用 roost-optimize；不新增包。

## 现状与结论

当前是 main / heartbeat / cost / remote 四个业务执行池，此外 pipelined 的完成泵属于事务基建。各池独立哈希路由：同 ID 的普通、广播、Cost、RemoteAccess 请求可以跨池超车；同一哈希槽的无关 ID 又可能被慢请求阻塞。RR-20260925-06 已分离 Remote 前后置 I/O，但还没有统一排序和不同队列预算。

快池少量 worker、较大有界队列；慢池较多 worker、较小有界队列，是合理方向。Go worker 是 goroutine，并非独占 OS 线程。增加慢并发仍受依赖吞吐约束，必须实测，不能承诺仅改池数即可提升 TPS。

## 目标结构与职责

保持 `nest/`：`dispatcher.go` 负责外部准入、延迟消息、Fence 与停机；新增同包 `dispatch_queue.go` 负责两条就绪队列、ID 依赖与执行资源；`remote_dispatch.go` 保留慢→快→慢交接；`nest_dispatch.go` 保留 Guard 与事务执行。main/hb/cost 合并到两种执行资源，不增加 manager/service 子包。

- 默认普通业务、心跳/广播进入快池。
- 显式阻塞调用使用 `SendOptionSlow`（原 `SendOptionIsCost` 兼容）；包括业务已知的 player 加载，不按 player 类型或 handler 名猜测所有调用都慢。
- 所有 handler（包括 Cost/Slow）均在快池执行。慢池禁止 Entity 本地锁/Guard；需要本地加锁的异常回滚也必须回到快池。任意 handler 内阻塞 RPC 不会自动搬到慢池，调用方必须拆成前置 I/O 与业务两段。
- Remote 写与 RemoteAccess 自动进入慢池。Remote 获取/确认/释放仍在慢池，业务阶段通过原有内部信封进入快池；不把 Entity 锁跨 goroutine 转移。
- 不改 WAL 持久级别、Sync 确认时机和默认 5 秒请求等待；锁内 WAL 准入保持原契约。

## 顺序与背压

统一准入时对消息声明的 Entity ID 建立先后依赖。单实体、Multi/MultiGroup、广播的全部显式目标均参与，重复 ID 去重。每个 ID 的新消息只依赖之前已准入的尾消息，依赖图天然按准入顺序向前，不形成调度环；声明不相交的消息可以使用任何空闲 worker，消除无关 ID 的 hash 槽队头阻塞。

同 ID 的顺序在进入任一执行池前决定，慢消息占有该顺序位置直到前置、业务和后置完成。等待前驱的快消息只占有界等待预算，不占快 worker。多实体锁、组锁及动态 Cast 的互斥仍由 Guard 负责；未在消息目标中声明的动态实体不承诺调度 FIFO。

广播可以保留现有分批，但整批显式 ID 都进入排序；每个目标的业务事务仍独立。并发调用的先后由准入锁决定。延迟消息在到期准入时排序；因组迁移或锁忙而重排的请求按重新准入顺序执行，这是既有重试语义。Pipelined 的独立完成泵与提前释放协议保留：保证业务执行顺序，不新增跨异步完成的全事务完成顺序承诺。

快、慢等待预算分别计数，包含等待 ID 前驱的消息，不包含正在执行的 worker。慢池默认并发为快池的 4 倍、至少 32；等待容量默认 64。快池默认 worker 跟随 GOMAXPROCS，等待容量默认 10000。所有值可独立配置。新容量为整池总等待预算，旧配置原本按每 worker 容量，需要明确迁移。

Remote 的内部快阶段已有外部准入资格，不重复进入 ID 排序，否则会等待自己的慢阶段。为其保留最多慢 worker 数量的内部信封位置，避免外部快队列满导致释放通路无法前进；内部与外部快就绪队列轮流服务。没有每条消息额外创建 goroutine。

停机关闭外部准入并排空统一队列；两种 worker 均保持运行直到已接受工作及其内部延续全部结束，随后一起退出。调用者取消只限制等待，不丢弃排空责任。

## API、观测与兼容

新增 `WorkerPoolConfig` 与 `NestOptionWithWorkerPools(fast, slow)`，Kit 提供 `nest.fast.workers/queue_capacity`、`nest.slow.workers/queue_capacity`。旧 worker_num/queue_capacity、RemoteWorkers option 映射到相应新配置，heartbeat_worker_num 不再创建独立池；旧导出 Stats 字段保留迁移兼容，新观测仅使用 Fast/Slow 和内部续行数，避免看起来仍有四个池。

## 实施和验收

1. 统一双池执行与 ID 依赖准入，改 Remote 内部投递、停机、观测和配置。
2. 确定性验证跨快慢同 ID 顺序、广播/多目标顺序、无关 ID 进展、满队列、取消/Fence、内部延续、重排和停机；race 回归。
3. 保留本地 Nest 微基准修前结果；补混合快慢负载基准。真实 Remote 仍用 1000 会话、10000 Entity、2 Entity × 2 DAO、strict，披露并发与容量，比较成功 TPS/尾延迟/积压，不预设优化一定更快。
4. 写问题与修复记录、更新索引及文档。旧源备份只用于负对照/回退，不恢复整个脏工作树。

## 已实施边界与初步验证

`dispatch_queue.go` 已建立双池和全显式目标 ID 准入依赖；`slow_load.go` 负责声明目标的前置加载及缓存。`entity/local_executor.go` 只提供同步本地交接能力，不新增包：DataEngine EntityRepository 将初始化/发布交回快池；Remote 内存持久模式提交失败的本地回滚也回到快池。自定义 Loader 的 I/O 与本地业务回调需要按同一契约拆分，不能自动识别任意自定义代码中的锁/RPC。

六包 race、vet 通过；定向并发测试 race ×20 通过。新增 `BenchmarkMixedFastSlow/slow_{1,5}_percent` 用 500μs 等待模拟 I/O，只测调度，不等同 Mongo/NATS 端到端性能。

本地微基准：Apple M5、Go 1.27.0、GOMAXPROCS=4，每项 1 秒 ×3 次，中位数。不是统计显著性结论。

| 项目 | 修前 ns/op | 修后 ns/op | 修前/修后 B/op | 修前/修后 allocs/op |
| --- | ---: | ---: | ---: | ---: |
| 单实体 Request | 3373 | 3472 | 2172 / 2252 | 51 / 55 |
| 多实体 Request | 4033 | 3810 | 2429 / 2557 | 57 / 61 |
| StageMetrics 关闭 | 3532 | 3422 | 1996 / 2076 | 55 / 59 |
| StageMetrics 开启 | 7543 | 5611 | 3070 / 3150 | 83 / 87 |

统一排序不是免费优化：单实体用时约增加 2.9%，每请求增加 4 次分配。主要收益是跨池顺序、共享慢 worker 和有界等待，不能把多实体或指标组的单次波动解释为整体加速。

混合调度微基准：4 快 worker、32 慢 worker、128 个并发提交者、1000 个轮转 ID；每条慢请求模拟等待 500μs，再交回快池。1% 慢请求中位数 720.0ns/op、255B/op、6 allocs/op；5% 为 817.4ns/op、276B/op、6 allocs/op。ns/op 是所有并发操作的摊销墙钟时间，不是单条慢请求延迟。此测试不执行 Entity 事务或数据库，不拿它作为业务 TPS。

## 真实 Remote 验收（双池版）

Apple M5、Go 1.27.0、GOMAXPROCS=4；快池 8 worker / 等待 4096，慢池 64 worker / 等待 64，Remote 投影 8。1000 会话、10000 Entity，每事务 2 Entity × 2 DAO，strict；每档另有 5000 笔预热，不计入计时 TPS。

| 输入 TPS / 时长 | 成功 / 错误 / 丢弃 | 实际完成 TPS | p50 / p95 / p99 / max ms | 全量校验 |
| --- | --- | ---: | --- | --- |
| 100 / 120 秒 | 12000 / 0 / 0 | 99.638 | 663 / 737 / 860 / 945 | 通过，最终 WAL/快慢队列均清零 |
| 120 / 120 秒 | 12653 / 1747 / 0 | 104.596 | 1187 / 1266 / 1352 / 1450 | 未通过；有错误后按脚本规则未执行全量校验 |

原始证据：`artifacts/perf/remote/two-pool-100-20260925/{result.json,result.json.verified,analysis.json,env.txt,run.log.gz}`；脚本退出 0。平均外部排队 31.22ms、最大 277.45ms；快延续排队平均 0.0052ms、最大 0.612ms；Remote 确认平均 589.84ms。快 worker 并非当前主要耗时来源。

旧四池 100 TPS ×120 秒为 11986 成功/14 次超时，旧快逻辑 64、Remote 64；新测快 8、慢 64，已明确同时变更调度及快 worker 配置，不能将全部差异归因于单一因素。旧 80 TPS ×10 分钟通过与本轮 100 TPS ×2 分钟通过不是同等长稳证据；本轮未重跑 30 分钟/24 小时。

120 TPS 档记录的前 32 条错误均为 `nest: dispatch queue full`，慢池等待采样最大 64（已达上限），快池最大 0；最终 WAL 未确认数回到 0。总计 1747 个错误不是 fixture 的 dropped（后者为 0）；不能把约 104.6 的成功回复 TPS 当作零错误容量。原始目录 `artifacts/perf/remote/two-pool-120-20260925`，脚本退出 1，保留失败证据且没有 `.verified`。当前配置的短测通过点为 100 TPS，120 TPS 已过载；没有声称找到了精确极限。

新增加载初始化、Remote 失败回滚测试以及 Sync 协作回归后，七包 `go test -race` 再次通过。使用临时 Go overlay 只恢复两处旧就地执行行为，`TestMemoryCommitFailureDelegatesLocalRollback` 与 `TestEntityRepositoryPublishesThroughLocalExecutor` 均失败；恢复当前源码后通过。未改写工作树制造负对照。日志位于 `artifacts/perf/nest/two-pool-verification-20260925/`。

图谱已更新至 `2026-09-25T08:49:04Z`，18272 节点、145692 边；本轮 26 个源码/测试证据路径 metadata_match、无记录缺口。3 个旧模板解析范围与本轮无关；docs、脚本和生成测试夹具按规则不入图，已直接读取。覆盖信息不等于完整性证明。

本轮故障矩阵 21/21 PASS，脚本退出 0；目录 `artifacts/perf/remote/two-pool-matrix-20260925`。包含 12 个 Mongo/NATS × async/strict/pipelined 业务组合，以及租约进程退出、Redis Cluster/未复制 fence、持久进程故障、ownership 计数器、Mongo WAL 恢复、broker 故障/网络与最终健康检查。它是既定矩阵覆盖，不能等同所有生产故障已穷尽。

两档负载 env 中记录的 61 个非测试 Go 源文件与最终版本哈希一致，见各目录 `source-check.json`；新增回归测试、文档不改变业务执行源。所有原始日志保留；没有重置既有工作树或执行部署。

## 同日追加复核

本轮之后修复 RR-08 准入执行额度与 RR-09 回滚异常解锁，见[再次复核](../review/NEST-FAST-SLOW-2026-09-25.md)。1024/16 通过调度正确性回归；不重复真实性能压测。上文 source-check 一致性指当时的 RR-07 版本，不能套用于追加修复后的源码。
