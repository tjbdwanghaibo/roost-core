# 当前已知限制与验证边界

当前版本v1.25.0，维护者接受重档失败先发布；本次文档整理没有新增行为修复，也没有重新证明全部历史缺陷已关闭。已合入修复的旧日期状态不再当当前待办；不确定的能力不写成已经支持。

| 项 | 当前结论 | 后续验收入口 |
| --- | --- | --- |
| Sync偶发长尾 | on_change按P99≤50ms验收；最大250.699ms样本根因未唯一确定 | 性能报告保留原严格门禁失败、参数和profile |
| Remote容量 | 已测80TPS完整1h，非跨机上限 | 同负载复跑，另做跨机RTT/故障/24h |
| Saga补偿容量 | 正向20/s、全补偿10/s已测；20/s全补偿饱和 | 真实步骤、混合负载与生产存储另测 |
| 跨机HA与Linux物理故障 | 本机集群/loopback不证明跨机器分区、掉电、真实网卡 | EXTERNAL-VERIFICATION E01～E28具名清单 |
| 客户端引擎 | 有Go/C#及Unity适配源码与测试，未声明真实Unity/Godot/Unreal全矩阵 | 各引擎实机、线程、断线与跨平台确定性 |
| 原生C++接入 | 本版无完整C++/Unreal/GDExtension SDK承诺 | 独立设计和协议一致性验收 |
| 目录升级 | 分类与 Service/Wiring 拆分已实施；旧 kit 与领域 alias 不保留 | [方案与升级要求](../framework/PACKAGE-REORGANIZATION.md)，重新生成并编译消费者；随v1.25.0交付 |
| 独立 Gate | 工作分支已实施 Gate/Game、广播与 Sync/Lockstep；本机功能通过，较重 AOI 负载失败；Block AOI改后15分钟常规通过，重档802.919秒先Gate准入耗尽、后Nest满，仍未通过 | [设计与分阶段验收](../framework/GATEWAY.md)；详见 DIRECTORY-GATE-VALIDATION；随v1.25.0发布但保留重档失败，不用本机多实例代替多进程/跨机矩阵 |
| schema升级 | DAO自动迁移已撤销；显式通用migration工具仍在 | 业务明确离线转换和保留数据范围 |
| Player跨机 | 以已落库数据为准，不要求本地WAL热迁移 | 应用的停写/排空/切换方案 |
| Lockstep历史 | 宿主调用TrimBefore/TrimHistory管理保留 | 长局、追帧范围和内存预算测试 |
| SyncBus投递 | 有限次数、保留期限及容量；并非绝不丢 | 快照/业务对账闭环，不能仅测重试成功 |
| Block AOI | 工作分支已实施手写组件、多块覆盖、独立事实入口和并行observer；15分钟常规通过、重档失败，未证明性能提升 | INTEREST-BLOCK-AOI及DIRECTORY-GATE-VALIDATION；首错定位、独立客户端、覆盖扇出及集中布局复验 |
| policy Drain | 等待retry和已承担事实；永久拒绝需修关系或ctx超时 | 拒绝、关闭和排空组合 |
| Skill Runtime | 不参与Nest回滚；Host副作用和确定性由业务契约保证 | Host能力、重入、阻塞、恢复和回放测试 |
| 观测接线 | ExportMetrics/admin元数据不是自动外部监控或审批系统 | 部署实际接线与鉴权审计 |
| Windows | 非框架保证平台；本次6个既有测试失败已在稳定基线复现，详见发布验收 | 本机测试结果不改变macOS/Linux支持范围 |

[外部验证细则](EXTERNAL-VERIFICATION.md)保留具体环境、步骤和判据。[性能基线](PERFORMANCE.md)保留失败样本、统计和参数。本次核对是静态文档整理和明确契约复核，不宣称全仓所有组合已审计、零bug或100%行为覆盖。
