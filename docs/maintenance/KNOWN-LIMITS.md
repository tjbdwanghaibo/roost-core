# 当前已知限制与验证边界

## RV-20261014-01：Demo 计数器前缀与退役键冲突

**P2，已修复（`23c7a997`），未发版。** 引入提交 `1ffb644f`，审查基线 `4370e6b3`，影响 game-demo 账号 allocator 模板，默认生成前缀不受影响。修复后 BindRegistry 在任何 Redis 调用前把计数键等于退役键的前缀作为配置错误拒绝，分配时的退役键报错不再引导删除；拒绝旧兼容的决定不变。回归见模板测试 `TestPlayerIDCounterContinuesAcrossRestartAndASecondReplica` 与 `TestPlayerIDAllocatorRefusesAPrefixWhoseCounterIsTheRetiredKey`，修前在真实生成的 game-demo 上以下文首次失败原文变红。已经用 `roost:demo` 前缀运行的部署仍须先停写、把整个前缀（计数器在内）迁到新前缀。以下为发现时的原始描述。

`account.key_prefix=roost:demo` 可以通过当前配置与 BindRegistry 校验，但生成的计数键恰好是被拒绝的 `roost:demo:player_id`。空 Redis 上第一次 Allocate 看不到旧键，将 checked 置为 true，随后 INCR 创建这个键；同进程继续成功，重启或新副本重新绑定后却把自己的新计数器当旧数据拒绝。当前错误还建议删除同一个键，照做可能让后续分配从头开始，重发已有玩家 ID。

证据位置：[collaborators 模板](../../demo/internal/service/account/collaborators.go.tmpl) 的 refuseRetiredCounter / BindRegistry / Allocate；[配置读取](../../wiring/account/account_mod.go)与[前缀检查](../../wiring/mods/service_servicemods.go)没有保留键冲突校验。隔离探针当前失败，`1ffb644f^` 对应模板通过；[复跑步骤与审查清单](COMMIT-REVIEW.md)。首次失败原文：

```text
fresh deployment restart cannot allocate: demo player id allocator: retired counter key roost:demo:player_id is still present and no longer read: set roost:demo:player_id to its value if that key does not exist yet, then delete roost:demo:player_id (or clear this deployment's account data)
```

审查建议（已按此修复）：在 BindRegistry、任何写入前校验新计数键不能等于退役键，冲突时报告配置错误；保留拒绝旧兼容的既定决定。已有冲突部署先停写并迁移完整前缀与计数器，不能直接删除当前计数器。修复应同时更新模板、示例说明和重启/多副本回归。

## 能力与验证边界

架构后续事项：[Session 运行服务可选化、玩家接入层与 Sync 命名](ACCESS-SYNC-FOLLOWUPS.md)。Session 可选化继续暂缓（维护者 2026-10-14）；玩家接入目录已改名为 `game/playeraccess`（dd362116）；Sync 命名已随 v1.26.0 发布，源码 API 需要随升级迁移。

当前版本v1.26.0。v1.25.0 时维护者接受重档失败先发布；本版已修复确认的放大因素（慢诊断全进程抓栈），v1.26.0 发布后同参数 15 分钟重档复测通过（业务 P99 12.24ms、Sync 提交→客户端 P99 14.24ms、>50ms 0.0098%、最大 86.341ms），长尾收窄但未唯一归因，见[发布验收](../release/v1.26.0-VALIDATION.md#perf15)与[复测报告](../performance/V1260-HEAVY-20261013.md)。文档没有重新证明全部历史缺陷已关闭。已合入修复的旧日期状态不再当当前待办；不确定的能力不写成已经支持。

2026-10-12 在 `6c36e0df` 上复验快8/慢8/GOMAXPROCS=0的相同重档，约6分钟出现 Gate 通道 `context deadline exceeded` 并关闭绑定，15分钟目标未通过。同参数诊断复跑约12分25秒失败，轨迹确认Nest慢请求自动全进程抓栈触发330.160ms STW，跨过Gate的250ms期限并放大故障；自动抓栈现已移除并通过Nest及race回归，同参数15分钟复验通过，Sync提交→客户端P99为18.66ms，零消息/HB拒绝、零outbound失败；仍有0.1813%的已订阅delta超过50ms、最大336.387ms，初始延迟未唯一归因。2026-10-10 的通过样本不能作为当前新增提交已稳定的证明。详见[本轮性能报告](../performance/REVIEW-20261012.md)。

| 项 | 当前结论 | 后续验收入口 |
| --- | --- | --- |
| Sync偶发长尾 | on_change按P99≤50ms验收；最大250.699ms样本根因未唯一确定 | 性能报告保留原严格门禁失败、参数和profile |
| Remote容量 | 已测80TPS完整1h，非跨机上限 | 同负载复跑，另做跨机RTT/故障/24h |
| Saga补偿容量 | 正向20/s、全补偿10/s已测；20/s全补偿饱和 | 真实步骤、混合负载与生产存储另测 |
| 跨机HA与Linux物理故障 | 本机集群/loopback不证明跨机器分区、掉电、真实网卡 | EXTERNAL-VERIFICATION E01～E28具名清单 |
| 客户端引擎 | 有Go/C#及Unity适配源码与测试，未声明真实Unity/Godot/Unreal全矩阵 | 各引擎实机、线程、断线与跨平台确定性 |
| 原生C++接入 | 本版无完整C++/Unreal/GDExtension SDK承诺 | 独立设计和协议一致性验收 |
| 目录升级 | 分类与 Service/Wiring 拆分已实施；旧 kit 与领域 alias 不保留 | [方案与升级要求](../framework/PACKAGE-REORGANIZATION.md)，重新生成并编译消费者；随v1.25.0交付 |
| 独立 Gate | 已实施 Gate/Game、广播与 Sync/Lockstep；本机功能通过，较重 AOI 负载失败；旧GOMAXPROCS4重档802.919秒先Gate准入耗尽、后Nest满；2026-10-10快8/慢8/默认并行度（实际10）重档15分钟通过，旧失败根因未关闭 | [设计与分阶段验收](../framework/GATEWAY.md)；详见[Gate/AOI性能报告](../performance/GATE-AOI.md)；随v1.25.0发布但保留重档失败，不用本机多实例代替多进程/跨机矩阵 |
| schema升级 | DAO自动迁移已撤销；显式通用migration工具仍在 | 业务明确离线转换和保留数据范围 |
| Player跨机 | 以已落库数据为准，不要求本地WAL热迁移 | 应用的停写/排空/切换方案 |
| Lockstep历史 | 滚动裁剪与本地108000帧逻辑长局验证已完成；宿主仍须主动裁剪历史和hash | 确定保留/过期恢复策略，验收真实网络长局；见[Sync现状与接入安排](../framework/SYNC-OPTIMIZATION.md) |
| SyncBus投递 | 有限次数、保留期限及容量；并非绝不丢 | 快照/业务对账闭环，不能仅测重试成功 |
| Block AOI | 已实施手写组件、多块覆盖、独立事实入口和并行observer；15分钟常规及快8/慢8/默认并行度重档通过；旧4并行度失败保留，未作单变量收益归因 | [Block AOI](../framework/INTEREST-BLOCK-AOI.md)及[性能报告](../performance/GATE-AOI.md)；首错定位、独立客户端、覆盖扇出及集中布局复验 |
| policy Drain | 等待retry和已承担事实；永久拒绝需修关系或ctx超时 | 拒绝、关闭和排空组合 |
| Skill Runtime | 不参与Nest回滚；Host副作用和确定性由业务契约保证 | Host能力、重入、阻塞、恢复和回放测试 |
| 观测接线 | ExportMetrics/admin元数据不是自动外部监控或审批系统 | 部署实际接线与鉴权审计 |
| Remote WAL级Rejected处理 | 已处理（`b3b2b52c`，未发版，维护者 2026-10-14 选“直接精简”）：Durability 1/2/3 的正式链路不产生 Remote Rejected（投影器写权威冲突是 `ErrProjectionConflict` fatal）；三个测试注入函数 `MongoCommitter`/`Backend.RejectRemoteCommitsInTransaction` 与 `Manager.RejectRemoteTransaction` 已删除，strict/pipelined 的 `Commit` 失败一律带 `ErrRemotePersistenceIndeterminate`。tracker 的 Rejected 收尾（回滚、隔离、仅内存卸载、重载、交付 false）只剩 Durability 0 来源，保留 | 守卫 `TestWALRemoteAuthorityConflictNeverProducesRejected` 只覆盖正式 MongoStore/MongoCommitter 投影链路；自定义投影存储若产生 Rejected，strict/pipelined 仍按结果未知回复、由 finalizer 按 tracker 结论收尾（`TestRemoteWALCommitFailureNeverRepliesRemotePartRejected`） |
| Windows | 非框架保证平台，只保证编译与 CLI 兼容（维护者 2026-10-14）。CI `windows-compatibility` 只跑 build、vet、`go test -run '^$'`（编译全部测试不执行）和 roost CLI 生成工程冒烟；完整 `go test` 的既有失败来自平台差异：打开中的文件不能改名/删除（nestwal 损坏记录检查、日志轮转、statslog），checkout 的 CRLF 使 gofmt 门禁与 kitconfig schema 比对失真（run 38500440362 的 6 个失败） | 本机测试结果不改变macOS/Linux支持范围；跨平台也成立的缺陷不归入此行 |

[外部验证细则](EXTERNAL-VERIFICATION.md)保留具体环境、步骤和判据。[性能基线](../performance/STABLE-v1.24.0.md)保留失败样本、统计和参数。本次核对是静态文档整理和明确契约复核，不宣称全仓所有组合已审计、零bug或100%行为覆盖。
