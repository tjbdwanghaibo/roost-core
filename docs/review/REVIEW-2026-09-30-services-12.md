# Service 第十二轮：强杀恢复、Redis HA、积压轮转与 Match 历史容量

**结论：本轮列明场景未确认新的生产逻辑 bug。**Session 的回收可在子进程真实退出后由新实例接续；隔离 Redis Cluster 的一个 master 被强杀、副本提升后，新进程可读取和结束同一 run。两者都依赖外部 Release 按资源身份做持久幂等，不能据此宣称端到端 exactly-once。Match 热队列的终态历史造成显著的时延与内存分配增长，属于此前已记录的归档设计风险，本轮给出更大的本机样本，尚无生产 SLO 或新的独立错误反例。Toxiproxy 三项仍全部跳过。

基线为 Core `88c6528675b527a7c30a662ed2e370d964cb13f4`、Kit `f0e5b67aa43473c2bcabd2ae180c69559fd6e20e`、Codegen `1e028fa4b2927ffb5d7772b90440e7447dc665cf9`。三仓先 fetch/快进并核对无未提交修改；本轮只在 Core 文档目录增加 review 和 Go overlay 探针，没有更改生产代码。Service 原[阶段矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md)的 10/10 域主链有界整理及逐批 bug 台账不重算为分支覆盖率；本轮只关闭下面已经实测的专项子项。[可复跑源、命令、原始日志校验值](evidence/service-review-20260930-01/README.md)和[机制学习](IMPLEMENTATION-SERVICE-CRASH-HA-AND-CAPACITY.md)分别记录环境与设计解释。

| 专项 | 本机实测 | 不能外推 |
| --- | --- | --- |
| Session 回收跨进程强杀 | standalone、Cluster 各 2 次，`-race -count=2`；子进程在 Redis `SETNX` 模拟资源生效后 `os.Exit(73)`，记录仍是 terminal/pending=1；新 Service 再次回调，最终 pending=0、claim 删除、同 RequestID 重放不变。回调共 2 次、模拟效果 1 次。 | `SETNX` 是测试中的幂等外部资源替身，不是实际 allocator、scene、资产服务；无真实服务回执/对账。 |
| Redis Cluster 单 master 故障 | 3 主 3 从，AOF everysec；负责 slot 11881 的 master `:16533` 写入后 `WAIT 1=1`，强杀其独占 PID 28480；`:16535` 约 5 个 500ms 轮询后成为 master，16384 槽恢复。独立 verify 进程把 run 结束为 succeeded，pending=0、Release 调用一次、claim 清空、请求重放稳定。 | 一台机器上的一次节点退出；`WAIT 1` 是本样本的复制确认，不证明普通已确认写在 failover 中永久不丢；无网络分区、多个 owner、故障组合或生产拓扑。 |
| Session 积压与局部故障 | 250 个持久 run，提供可轮转的 OwnerSource，每次 100 owner；首轮注入一个 `Claim.Get` 失败，四轮处理 `99,100,50,1`，最后 250 个 run expired/pending=0、claim 均清，回调各 1 次。 | OwnerSource 轮转由 fixture 提供；不证明任何部署自动发现所有 owner。故障是包装器注入，非物理 Redis 读断线。 |
| Match 容量 | Memory/Redis 各 64、256、1024、4096、8192、16384 条旧终态记录，16 次串行 Enqueue/Cancel 均通过。Redis 64→16384 的 16 对耗时 `48.039→5234.860 ms`，Go 分配 `8.38→2742.88 MB`，单 key JSON `23,223→4,968,081 B`。另在 8192 条历史上串行 128 对耗时 23.418 s、分配约 10.96 GB，进程内 p50/p95/p99 为 175.725/241.997/334.625 ms。 | 短时、串行、本机、单热点的样本；不是生产 TPS/长稳基准，不能与业务 SLO 直接比较，也未实施归档。 |
| 网络故障 | `TestToxicRedis*` 3 项 test skip、0 pass。 | 缺 Toxiproxy 代理与环境变量；物理丢回复/超时验收仍待办。 |

HA 首轮 `1m` TTL fixture 因构建/测试间隔超时，两个 verify 断言先后错误：`Get` 只把过期状态体现在读视图，`Finish` 落下 expired 并返回 `ErrRunExpired`，即使资源释放成功。保留两份失败日志 hash 于[结果摘要](evidence/service-review-20260930-01/RESULTS.json)；按真实契约修正断言、改 `5m` TTL 后第二轮通过。这个失败是测试设计问题，不新增 RR 编号。[Session `finish`/`releasePending`/`Get`](../../service/session/service.go) 的实现顺序和返回契约与该观察相符。

源码路径以知识图谱 Tier 2 定位并核对相关调用/片段，`service/session/service.go`、`sweep_source.go`、`redis_store.go`，`service/match/queue_store.go`、`redis_store.go`，`redis/driver/cluster_recovery.go`、`lock_toxic_integration_test.go` 和 `versionstore/redis_store.go` 均执行路径覆盖查询。图谱 full generation `2026-09-29T14:50:03Z`，这些路径无记录的 parse/skipped 缺口但返回 Windows `metadata_changed`；据实际 checkout 源码复读和本轮运行结果下结论，不能把“无记录缺口”当完整性证明。`Session.Get` 的图谱检索不足，直接读实现补证。关于 Kit/Codegen，本轮只同步 HEAD，未作新的行为结论。

**下一验收入口：**实际 allocator/scene/资产 Release 幂等与回执对账；Toxiproxy 物理丢回复、双向隔离及高延迟；至少跨机器、跨 owner、含写入未知结果的 HA；Match 归档方案落地后的兼容迁移与目标 SLO 下并发/长稳基准。旧问题台账不因本轮绿测扩大验收范围，当前[归档方案](IMPLEMENTATION-SERVICE-FINAL-SPECIALTIES.md)仍是设计交接而非已实施功能。
