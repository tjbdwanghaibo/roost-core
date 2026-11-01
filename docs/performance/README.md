# 性能基线与复跑

本目录只保留各类负载的最新有效基线及必要原始样本。“最新”是同一负载、同一实现路线里的最近证据，不是全框架刚刚测过。表中历史基线均不能当成当前版本的容量保证；本轮新增 Robot/Gate 本机短测，其余历史基线没有因此变成当前版本保证。

## 按问题选择基线

| 要了解什么 | 基线与适用范围 |
| --- | --- |
| DAO map/list 同步 | [集合同步微基准](DAO-COLLECTION-SYNC.md)：稀疏/密集变化的 PB 字节、CPU 与分配开销，不是网络容量 |
| Robot / 真实账号 Gate 持久热点 | [当前短测](ROBOT-GATE-20261101.md)：20/s 通过；100/s 夹具批量能力修复后两轮排空通过，保留修前失败，包含 Sync 三类延迟与复跑入口 |
| 客户端解码 / Unity / 弱网 | [当前验证](CLIENT-20261101.md)：native 与 Mono/IL2CPP 的可见集容量、TCP 故障，非 WAN/设备容量 |
| Nest 调度开销 | [dispatch](NEST-DISPATCH-BENCHMARK.md)（handler 10–500µs 对照）；[最新完成顺序基准](benchmarks/nest-completion-order-all-20261029/README.md) |
| 心跳、普通消息与 Entity 混合负载 | [心跳驱动](HB-DRIVEN-LOAD-20261021.md)、[批量投递](NEST-BATCH-HEARTBEAT-20261021.md)；[调度结构取舍](HB-STRUCTURES-20261021.md)保留为现行参数依据 |
| Gate + AOI 同步延迟 | [完整链路](GATE-AOI.md)、[Interest 单写应用](INTEREST-SINGLE-WRITER-20261022.md)；periodic、on_change、ApplySync、ApplyAsync 分开比较 |
| DataEngine 投影能力 | [按实体并行投影](PROJECTOR-ENTITY-PARALLEL-20261027.md)，区分普通记录、回执、effect 与本机/云 Mongo |
| Saga 正式 JetStream 实现 | [本机验收](SAGA-JETSTREAM-AUTHORITY-20261029.md)：原生正向 c128 约 1047–1053 笔/s；[云 Mongo 复核](SAGA-CLOUD-C128-REPRO-20261029.md)：吞吐与投影追平必须同时看，不能只报接纳 TPS |
| Remote Entity | [80 TPS × 15 分钟](benchmarks/remote-80tps-20261029/README.md)；[故障矩阵](benchmarks/remote-matrix-20261029/README.md)是正确性证据，不算吞吐 |
| 物理房间与 Chunk | [长稳](benchmarks/physicsroom-soak-20261029/README.md)、[分配](benchmarks/physicsroom-alloc-20261029/README.md)、[3D 同步对照](benchmarks/chunk-physx-3d-20261025/README.md)；Linux 实跑未验收 |

## 复跑入口

- Nest 混合负载：[`scripts/perf/nest-game.sh`](../../scripts/perf/nest-game.sh)。参数按对应报告选择，不把不同消息频率、实体变化率、worker 数或持久策略混作同一档。
- Saga：[`scripts/perf/saga-js-scan.sh`](../../scripts/perf/saga-js-scan.sh)。区分 strict/pipelined、准入、并发和参与方投影，统计终态、积压与追平时间。
- Remote、物理、AOI 与微基准：使用表中报告记录的命令和环境。代码版本必须匹配；无法复现时重新建立基线，不沿用旧结论。
- 真实资源使用隔离库、命名空间与环境锁；不对生产或其他任务的共享资源注入故障。

## 指标与证据规则

- 业务消息、Entity 修改、Heartbeat、Remote 事务与 Saga 各自统计；不能用 Saga TPS 代替普通游戏服务器吞吐。
- Sync 按 P99 ≤ 50ms 评估，同时记录最大值与超 50ms 的数量。on_change 是 handler 提交后统一处理变化，setter 只标脏；20Hz 周期兜底不是 on_change 的硬上限。
- 延迟起点必须写清：请求提交、handler 改值、持久提交或计划时间；终点是 socket 写出、客户端收到还是完成解码。压测发生器迟到、客户端排队和解码不可混作服务器执行时间。
- 同机同参数前后交替测试才能归因。保留失败、尾延迟、宿主噪声、未跑/中断/仅编译状态；不同主机与旧实现数字只作背景。
- 当前选定基线的精简样本在 `benchmarks/`；完整日志、profile 与二进制在忽略的 `artifacts/perf/`。全新克隆不包含本地 artifacts。

## 历史报告

被新基线替代的版本压测、旧 Mongo/Redis Saga 路线、原型试验和已结束诊断不再放在当前目录。其作用是解释过去的设计取舍和回归，按需从[整理前快照](https://github.com/tjbdwanghaibo/roost-core/blob/505a14edb9cffcf60d601785f824abcbc0ab02e9/docs/performance/)读取；既有文档引用已固定到原快照，不丢失证据，也不让旧报告挤占当前阅读入口。

以后每次产生同口径新基线，更新本页并将被替代正文/样本移入 Git 历史；只有仍用于解释现行取舍或未解决问题的对照需要继续随仓。
