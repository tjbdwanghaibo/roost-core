# 当前已知限制与验证边界

## RV-20261014-01：Demo 计数器前缀与退役键冲突

**P2，已修复（`23c7a997`），随 v1.26.1 发布。** 引入提交 `1ffb644f`，审查基线 `4370e6b3`，影响 game-demo 账号 allocator 模板，默认生成前缀不受影响。修复后 BindRegistry 在任何 Redis 调用前把计数键等于退役键的前缀作为配置错误拒绝，分配时的退役键报错不再引导删除；拒绝旧兼容的决定不变。回归见模板测试 `TestPlayerIDCounterContinuesAcrossRestartAndASecondReplica` 与 `TestPlayerIDAllocatorRefusesAPrefixWhoseCounterIsTheRetiredKey`，修前在真实生成的 game-demo 上以下文首次失败原文变红。已经用 `roost:demo` 前缀运行的部署仍须先停写、把整个前缀（计数器在内）迁到新前缀。以下为发现时的原始描述。

`account.key_prefix=roost:demo` 可以通过当前配置与 BindRegistry 校验，但生成的计数键恰好是被拒绝的 `roost:demo:player_id`。空 Redis 上第一次 Allocate 看不到旧键，将 checked 置为 true，随后 INCR 创建这个键；同进程继续成功，重启或新副本重新绑定后却把自己的新计数器当旧数据拒绝。当前错误还建议删除同一个键，照做可能让后续分配从头开始，重发已有玩家 ID。

证据位置：[collaborators 模板](../../demo/internal/service/account/collaborators.go.tmpl) 的 refuseRetiredCounter / BindRegistry / Allocate；[配置读取](../../wiring/account/account_mod.go)与[前缀检查](../../wiring/mods/service_servicemods.go)没有保留键冲突校验。隔离探针当前失败，`1ffb644f^` 对应模板通过；[复跑步骤与审查清单](COMMIT-REVIEW.md)。首次失败原文：

```text
fresh deployment restart cannot allocate: demo player id allocator: retired counter key roost:demo:player_id is still present and no longer read: set roost:demo:player_id to its value if that key does not exist yet, then delete roost:demo:player_id (or clear this deployment's account data)
```

审查建议（已按此修复）：在 BindRegistry、任何写入前校验新计数键不能等于退役键，冲突时报告配置错误；保留拒绝旧兼容的既定决定。已有冲突部署先停写并迁移完整前缀与计数器，不能直接删除当前计数器。修复应同时更新模板、示例说明和重启/多副本回归。

## 新用户试用第三轮：待修问题（2026-10-17 登记，尚未修复）

基线 `e85addd9`（origin/main）。用 main 构建的 CLI 从空目录按 beginner 流程（`roost project next` 逐步）建工程、打开 `self_sync`、在共享隔离环境起真实进程并用 TCP 客户端联调时确认。修复由维护者在 main 上进行。

### 1. 自动补齐的 mongo / nats / dataengine 在服务配置里没有配置段

**现象**：`roost add access player --service game` 给 game 补齐 Nest 及其持久化依赖，进程实际启动 `mongo`、`nats`、`dataengine` 三个 Mod（启动日志 `"mod":"mongo"` 等），但 `roost.yaml` 的 `services.game.mods` 只列 `configdata`、`nest`，`configs/service/config.game.yaml` 里三段一段都没有；进程按声明默认值连接（`--print-config` 显示 `mongo.uri: mongodb://127.0.0.1:27017/?replicaSet=rs0`）。

**复现**：`roost project new r3 -module example.com/r3 -mods configdata` → `roost add access player --service game` → `grep -c '^mongo:\|^nats:\|^dataengine:' configs/service/config.game.yaml` 得 0；`<bin> game --print-config | grep -A1 '^mongo:'`。

**影响**：新手不知道要配这三段，第一次启动在开发机上连到任意一个 27017 上的 Mongo（第一轮试用时连到本机非副本集 mongod，报 `production mode requires a replica set`）；要隔离环境时只能靠 `--print-config` 自己摸出键名。与 `framework/app/config_schema.go` 注释所说的“生成器按同一份声明写配置段”不一致：Mod 由依赖补齐时，配置段没有跟着补。

**建议修法**：依赖补齐的 Mod 与显式列出的 Mod 同样写入服务配置段（`StarterYAML`），并在 `roost.yaml` 或注释里写明它们是被 nest 带入的；已有工程由 `project sync` 只补缺失的段、不改已有值。

### 2. `roost project next` 的修复提示是固定示例名

**现象**：`doctor.go` 的 first-business 检查表（`checks` 里 `nest-handler` / `protocol` / `endpoint` 三行）把修复命令写死为 `RenamePlayer` 与 `--handler player`。工程里已经有 `BuyItem`、`GrantGold` 两个 handler 时，`project next` 仍提示 `next: roost add protocol RenamePlayer --group game --handler player`；登录步骤同样固定提示 `--handler player`，业务已有 `shop` controller 域时照做会多出一个 `player` 域。

**复现**：上面的工程里 `roost add handler BuyItem --entity Player --component Shop` 后运行 `roost project next`（progress 10/15）。

**影响**：新手照抄会生成一个与自己业务无关的 RenamePlayer 协议，或在两个 controller 域之间困惑。

**建议修法**：提示按工程现状填名字——已有 handler 而缺协议时用第一个缺协议的 handler 名，已有 controller 域时沿用它；工程里什么都没有时再给示例名，并标明“示例”。

### 3. self_sync 的 packer 要业务整段照抄

**现象**：打开 `self_sync` 后就绪检查要求写 `NewPlayerSyncPacker`，[PLAYER-ACCESS §7](../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync) 给出“可以直接照抄”的整段代码；照抄后的文件 45 行（视图集、构造、Snapshot / Delta / pack 三个方法），内容只依赖生成 DAO 的 `PlayerDaoSyncFields` / `MarshalSync` 与固定的 `self` 视图。

**影响**：每个打开 self_sync 的工程都手抄同一段样板，抄错（视图漏 `self`、mask 处理）只在运行期 `ErrSyncViewUnknown` 才暴露；与 D6 决定的“容易写错的放 core、生成器只写可见接线”方向相反的一小块。

**建议修法**：生成器在 `self_sync` 打开且 Entity 没有自定义 packer 时生成 `player_sync_packer_gen.go`（全部同步字段给 `self` 视图），marker 的 `subjectPacker=` 缺省指向它；需要裁剪字段或多视图的业务再写自己的 packer 覆盖。

## 能力与验证边界

架构后续事项：[Session 运行服务可选化、玩家接入层与 Sync 命名](ACCESS-SYNC-FOLLOWUPS.md)。Session 运行服务保持现状、不改为可选（维护者 2026-10-14 决定）；玩家接入目录已改名为 `game/playeraccess`（dd362116）；Sync 命名已随 v1.26.0 发布，源码 API 需要随升级迁移。

当前版本v1.27.0。v1.25.0 时维护者接受重档失败先发布；v1.26.0 已修复确认的放大因素（慢诊断全进程抓栈），v1.26.0 发布后同参数 15 分钟重档复测通过（业务 P99 12.24ms、Sync 提交→客户端 P99 14.24ms、>50ms 0.0098%、最大 86.341ms），长尾收窄但未唯一归因；v1.27.0 发布后同参数延长到 30 分钟仍通过（业务 P99 12.49ms、Sync P99 14.71ms、>50ms 0.0038%，堆不随时间增长，业务最大 107.307ms），分别见 [v1.26.0 发布验收](../release/v1.26.0-VALIDATION.md#perf15)、[15 分钟复测](../performance/V1260-HEAVY-20261013.md)与 [30 分钟复测](../performance/V1270-HEAVY-30M-20261014.md)。2026-10-15 带采集的同参数重档定位长尾（[2026-10-15 性能四项](../performance/PERF-ROUND-20261015.md) §1）：约 60% 的长尾事件集中在每个墙钟分钟第 2 秒，与宿主 MDM / CorpLink 周期任务同秒，表现为调度延迟而非进程内 STW / GC / 锁，未归因到 roost 代码；另有持续数秒、不在整分钟的簇尚未确认。文档没有重新证明全部历史缺陷已关闭。已合入修复的旧日期状态不再当当前待办；不确定的能力不写成已经支持。

2026-10-12 在 `6c36e0df` 上复验快8/慢8/GOMAXPROCS=0的相同重档，约6分钟出现 Gate 通道 `context deadline exceeded` 并关闭绑定，15分钟目标未通过。同参数诊断复跑约12分25秒失败，轨迹确认Nest慢请求自动全进程抓栈触发330.160ms STW，跨过Gate的250ms期限并放大故障；自动抓栈现已移除并通过Nest及race回归，同参数15分钟复验通过，Sync提交→客户端P99为18.66ms，零消息/HB拒绝、零outbound失败；仍有0.1813%的已订阅delta超过50ms、最大336.387ms，初始延迟未唯一归因。2026-10-10 的通过样本不能作为当前新增提交已稳定的证明。详见[本轮性能报告](../performance/REVIEW-20261012.md)。

| 项 | 当前结论 | 后续验收入口 |
| --- | --- | --- |
| Sync偶发长尾 | on_change按P99≤50ms验收；最大250.699ms样本根因未唯一确定。2026-10-15 采集：本机约60%长尾与宿主每分钟第2秒的周期任务同秒（调度延迟、进程内无额外工作），GC STW/标记辅助/锁/进程内定时器已排除；持续数秒的非整分钟簇尚未确认。宿主留 CPU 余量对照（`GOMAXPROCS=8`，方案 B）同日已做，长尾反而更多（>50ms 为基线 3～10 倍），无效 | 维护者 2026-10-15 决定先接受现状；以后有独占服务器 / Linux 实机时再复测（方案 A）；见[2026-10-15 性能四项](../performance/PERF-ROUND-20261015.md) §1、§5 |
| Remote容量 | 已测80TPS完整1h（v1.24.0）；v1.27 代码 80TPS×30m 在共享隔离环境复测通过（零错误、核验通过、p99 278ms），非跨机上限 | 跨机RTT/故障/24h；见[2026-10-15 性能四项](../performance/PERF-ROUND-20261015.md) §3 |
| Saga补偿容量 | 正向20/s、全补偿10/s已测；20/s全补偿饱和。v1.27 代码各 10 分钟复测通过，全补偿10/s 首测 P99 1.47s / max 2.01s 高于历史 1 分钟样本（0.6s）；夹具加分钟分段后同日复测 P99 0.64s、十个分钟段无增长趋势，首测高值未复现、原因尚未确认 | 真实步骤、混合负载与生产存储另测；见[2026-10-15 性能四项](../performance/PERF-ROUND-20261015.md) §3、§5 |
| 跨机HA与Linux物理故障 | 本机集群/loopback不证明跨机器分区、掉电、真实网卡 | EXTERNAL-VERIFICATION E01～E28具名清单 |
| 客户端引擎 | 有Go/C#及Unity适配源码与测试，未声明真实Unity/Godot/Unreal全矩阵 | 各引擎实机、线程、断线与跨平台确定性 |
| 原生C++接入 | 本版无完整C++/Unreal/GDExtension SDK承诺 | 独立设计和协议一致性验收 |
| 目录升级 | 分类与 Service/Wiring 拆分已实施；旧 kit 与领域 alias 不保留 | [方案与升级要求](../framework/PACKAGE-REORGANIZATION.md)，重新生成并编译消费者；随v1.25.0交付 |
| 嵌入 TCP 出站 | 2026-10-16 起每连接有界出站队列 + 唯一写协程，停读客户端被关闭、不再拖住场景 Sync Flush / Lockstep Tick 的其他会话（修前一个停读连接让每次 Flush 卡满 `write_timeout`，默认 5s）。上限固定为 1024 帧与 `max(2 MiB, 2 × (max_payload_bytes + 16))` 字节，按“排队 + 写协程已取走未写完”计（2026-10-16 修正：原先帧数只数排队、不含在途整批，停读连接到第 2049 帧才拒绝），不开放配置键；帧数上限维护者 2026-10-16 决定保持 1024，理由：场景 Sync / Lockstep 每次每连接只推几帧，上千帧突发不现实，需要更大突发时再评估；没有 Gate 那样的全进程驻留预算，最坏 `max_connections × 字节上限`（默认约 20 GiB，只有全部连接同时停读才会达到）；推送入队多一次 payload 复制；每连接 goroutine 由 1 变 2（独立 Gate 模式下同样多一个通常空闲的写协程）。只有包内基准（往返延迟、分配与修前无显著差异）与生成 demo 场景用例，未做重档压测 | 重档 Sync/AOI 压测核对推送路径 CPU 与内存；部署需要不同队列上限或全局驻留预算时再加 `player_access.tcp` 键；见 [GATEWAY §4](../framework/GATEWAY.md#嵌入-tcp-的出站) |
| 独立 Gate | 已实施 Gate/Game、广播与 Sync/Lockstep；本机功能通过，较重 AOI 负载失败；旧GOMAXPROCS4重档802.919秒先Gate准入耗尽、后Nest满；2026-10-10快8/慢8/默认并行度（实际10）重档15分钟通过；2026-10-15 v1.27 代码按旧4并行度配置重档30分钟通过（业务P99 18.64ms、>50ms 0.236%，拒绝0），多项改动叠加、单样本，旧失败根因未关闭 | [设计与分阶段验收](../framework/GATEWAY.md)；详见[Gate/AOI性能报告](../performance/GATE-AOI.md)；随v1.25.0发布但保留重档失败，不用本机多实例代替多进程/跨机矩阵 |
| schema升级 | DAO自动迁移已撤销，运行时只接受当前schema；离线迁移按[schema变更操作手册](../framework/SCHEMA-MIGRATION.md)（停写→备份→转换→校验→切换→回退），game-demo有执行器与`cmd/schemamigrate`，2026-10-15真实Mongo演练一次 | 业务明确离线转换和保留数据范围；在线双读与生成器骨架（`roost add migration`）未实施，上线后重新决定 |
| Player跨机 | 以已落库数据为准，不要求本地WAL热迁移；搬的是sid，按[参考流程](../framework/PLAYER-MIGRATION.md)停写→排空WAL→释放单实例锁→切路由→新进程空WAL接管，game-demo有检查工具`cmd/sidmove`；2026-10-15同机两进程演练通过（含崩溃与未排空场景） | 真实跨机网络、卷迁移、客户端重连路径未演练；单个玩家改绑sid无框架能力；框架级迁移命令上线后评估 |
| Lockstep历史 | 默认自动保留3600帧（60Hz 60秒 / 30Hz 120秒），越界追帧明确失败并通知客户端终止、不自动重同步（D1，维护者2026-10-15决定C，`a6376934`）；本地108000帧逻辑长局与默认窗口长局有界已验证 | 真实网络断线超过窗口的端到端演练、按业务帧率与断线时长核对窗口；全量快照恢复（D1方案B）待具体游戏需求；见[Sync现状与接入安排](../framework/SYNC-OPTIMIZATION.md) |
| Lockstep TCP 追帧限速 | 已处理（`640e9fac`；维护者 2026-10-16 决定方向 A）：`lockstep.TCPSender` 两条通道分开，实时帧 `SendDatagram` 仍经 `PushLockstepSession` 只准入，追帧页 `SendReliable` 改经新的 `TCPRuntime.PushLockstepSessionWritten` 入队并等写出——追帧随链路速度放慢，属于追帧的出站积压只有在途一页，`CatchupSendTimeout` 对 TCP 发送器重新生效（超时的页还在排队就撤回），超时计入 `CatchupMaxFailures`。修前（`b1a06152`）真实回环 TCP 上约 8 MiB 历史、客户端先停读再慢读，第一批页就把连接推过出站上限被关闭；停读连接的追帧以“session not found”放弃，从未超时。等写出只在 Match 的 `launchSend` goroutine 上，不在 Tick 协程或业务 worker（`fctx.BlockingError` 守卫保留）；一个停读追帧会话最多让发起那一页的 Tick 晚 `CatchupSendWait`（默认 5ms）。回归见 [lockstep/tcp_test.go](../../framework/sync/lockstep/tcp_test.go)。触发阈值未在 toxiproxy 限速的真实链路上测 | 用 toxiproxy 限速链路做一次追帧演练（追帧耗时与链路速度对应、连接不断）；说明见 [guide/04-sync §6](../framework/guide/04-sync.md#经嵌入-tcp-追帧随链路放慢) |
| Lockstep 追帧链路背压（独立 Gate / demo） | 未处理、影响未实测。独立 Gate 的 `wiring/gate.Transport` 两条通道都经 `GameIngress.PushBound`，返回 nil 只表示 Game 本地准入，追帧不随链路放慢：Game 侧队列满时返回 `ErrReliableBackpressure`（计一次追帧失败，每 Tick 一次，连续 `CatchupMaxFailures` 次放弃），Gate 侧队列满或积压超过 `outbound.max_age`（默认 250ms）时 Gate 关闭连接。生成 demo 的 `battleSender` 两条通道都走 `PushPlayer`（只准入），不经 `TCPSender`，同样没有追帧背压 | 独立 Gate 需要 Gate 在 socket 写出后回执（协议改动），需维护者决定是否做；demo 可改用 `TCPSender` 或给可靠通道接等写出版本，需先定 demo 是否演示嵌入 TCP 追帧 |
| 自身视图（self_sync） | 2026-10-16 起 `access.player.self_sync: true` 让玩家经 Entity Sync 收到自己的 Player（DESIGN-TODO D6，`policy.SelfView` + `wiring/selfsync`）。限制：①同一个 `SubjectSyncState` 只能登记在一个 Manager（脏通知器只有一个），工程里另有自建 Manager 也登记 Player（game-demo 的场景）时与 self_sync 互相覆盖，当前没有守卫，文档要求不要同时开；②一帧上限默认取 gateway 默认的 `max_payload_bytes`（1 MiB），部署调小 `player_access.tcp.max_payload_bytes` 而不改 `selfsync.Config.MaxFrameBytes` 时，超限帧被接入层拒收、会话被丢、重开至多 8 次后停到下次登录；③Manager 丢掉会话（推送全部失败）后在仍活着的连接上重开是新 lifetime、epoch 从 1 开始，若该连接的接收端已收过更大 epoch 会把新全量当旧包丢掉——正常情况下丢会话意味着所有连接都已断开，只有“连接还在却拒收”（如②）会落到这里；④不提供 C# 的 subject 载荷解码样例；⑤生成物需要包含 `wiring/selfsync` 的 core（当前主线之后的下一个发布），`minimumVersions.Core` 发版时再定；⑥只做了本机共享隔离环境的一次真实进程演练与包内 / 生成工程回归，未做重档压测 | ①需要时给 `SubjectSyncState` 加“已被另一个 Manager 登记”的拒绝；②生成接线从 `player_access.tcp.max_payload_bytes` 取帧上限（需要接入层在 Nest Init 前读到配置）；③可让 Manager 的新 lifetime 从进程内单调计数取 epoch；见 [PLAYER-ACCESS §7](../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync) |
| SyncBus投递 | 有限次数、保留期限及容量；并非绝不丢。定位为默认可丢广播（维护者2026-10-15），兜底写法与可运行模板见[guide/04-sync §5](../framework/guide/04-sync.md#syncbus-的定位与丢失兜底) | 需要最终一致的业务自建快照/对账闭环，不能仅测重试成功；框架级对账为下一阶段候选 |
| Block AOI | 已实施手写组件、多块覆盖、独立事实入口和并行observer；15分钟常规及快8/慢8/默认并行度重档通过；旧4并行度失败保留，2026-10-15 同配置30分钟复测通过，未作单变量收益归因 | [Block AOI](../framework/INTEREST-BLOCK-AOI.md)及[性能报告](../performance/GATE-AOI.md)；首错定位、独立客户端、覆盖扇出及集中布局复验 |
| policy Drain | 等待retry和已承担事实；永久拒绝仍需修关系或ctx超时，Drain不放弃条目；超时返回`*DrainStalledError`列出卡住条目、原因与等待时长并计数（D3，维护者2026-10-15决定A，`b86a1286`） | 拒绝、关闭和排空组合；“放弃出口”（D3方案B）需先梳理永久拒绝的判定来源 |
| Skill Runtime | 不参与Nest回滚；Host副作用和确定性由业务契约保证 | Host能力、重入、阻塞、恢复和回放测试 |
| 观测接线 | ExportMetrics/admin元数据不是自动外部监控或审批系统 | 部署实际接线与鉴权审计 |
| Remote WAL级Rejected处理 | 已处理（`38b710a2`，随 v1.27.0 发布，维护者 2026-10-14 选“直接精简”）：Durability 1/2/3 的正式链路不产生 Remote Rejected（投影器写权威冲突是 `ErrProjectionConflict` fatal）；三个测试注入函数 `MongoCommitter`/`Backend.RejectRemoteCommitsInTransaction` 与 `Manager.RejectRemoteTransaction` 已删除，strict/pipelined 的 `Commit` 失败一律带 `ErrRemotePersistenceIndeterminate`。tracker 的 Rejected 收尾（回滚、隔离、仅内存卸载、重载、交付 false）只剩 Durability 0 来源，保留 | 守卫 `TestWALRemoteAuthorityConflictNeverProducesRejected` 只覆盖正式 MongoStore/MongoCommitter 投影链路；自定义投影存储若产生 Rejected，strict/pipelined 仍按结果未知回复、由 finalizer 按 tracker 结论收尾（`TestRemoteWALCommitFailureNeverRepliesRemotePartRejected`） |
| Remote请求struct与Sender | handler 参数用带 `remote:"..."` 字段的请求 struct 时，struct 必须留在 handler 包（访问器生成在这里），sender / syncsender 子包又看不见 handler 包类型；`roost generate` 总生成 sender，所以这种 handler 生成失败（修前修后都不可用，2026-10-16 起改为专门报错、不再建议移走类型）。维护者 2026-10-16 决定先写清限制，不改生成结构 | 需要时再设计：让 sender 携带这种 struct（改生成结构），或允许单个 handler 不生成 sender |
| 新用户接入缺口 | 2026-10-16 新用户按 beginner 流程实测的四项均已由维护者同日决定并实施：玩家看自己状态（D6 折中方案：`access.player.self_sync`，剩余限制见下文“自身视图（self_sync）”）；未登录玩家得到 `CodeInternal`、first-business 不含首登 GetOrCreate（D7 方案 C：`errcode.CodeEntityNotFound` 与 `roost add endpoint --lifecycle`）；生成的服务包名大写（D8 方案 C：新工程小写、已有工程跟随目录）；DAO 库名是代码常量（D9 方案 B：骨架 `db=<project>` 与 `roost id entity`）。库名取运行配置（D9 方案 A）保留待评估。2026-10-17 第三轮试用新登记三项待修：见上文“新用户试用第三轮：待修问题” | [DESIGN-TODO D6～D9](DESIGN-TODO.md#d6-玩家看自己状态entity-sync-自身视图的接入骨架) |
| 删档后同 ID 重建 | GetOrCreate 路径已修复（维护者 2026-10-17 决定 [D10](DESIGN-TODO.md#d10-删档后用同一-id-重建) 方案 B，范围只守 GetOrCreate，`572d87c2`）：冷加载对删过档返回独立哨兵 `entity.ErrAuthorityEntityDeleted`（不满足 not found，Remote 托管实体查元数据墓碑），生成 GetOrCreate 不重建、发往该 ID 的 Nest 请求回复 `entity_deleted = 3`，不进 WAL、不 fence。**剩余限制（约束）**：直接 Create（handler 内或 lifecycle `Create`）不读存储，必须使用 id-gen 发的新 ID，不要复用删过档的确定性 ID（也不要对存了但没加载的 ID 直接 Create）；违反时首个提交 `Put expected=0` 撞上墓碑 / 已有文档，投影 `ErrProjectionConflict` fatal、进程 fence、冷重启起不来，处置见下一行。`EntityManager` 的 ID 生成器同样必须永不重复。“谁、为什么删档”与 Remote 实体的删前数据不在墓碑里，需要时由删档入口自己记业务审计（D10 审计结论） | 回归 `TestGeneratedDataEngineRecreateAfterDeleteSameID`（真实 Mongo，已去掉 KNOWN_RED 跳过）、`TestGeneratedRemoteRecreateAfterDeleteIsRefused`、`TestColdTargetDeletedInAuthorityAnswersEntityDeletedCode`；直接 Create 的保护按维护者决定不做（范围收窄前的实现已撤回，见 D10）|
| 删档后同 ID 重建：违反约束或修复前写下的冲突记录 | 修复前已进入 WAL 的“删档后重建”记录，以及违反上一行约束（直接 Create 复用删过档 / 已存储的 ID）写下的记录（首个 `Put expected=0` 撞墓碑或已有文档），不会被跳过或放宽：启动投影恢复仍报 `startup projection recovery: ... fatal projection version conflict: Put duplicate key <db>/<coll>/<id> expected=0 next=1`，进程起不来。框架不自动删记录、不跳过 | 人工处置（只在确认是这类记录时）：①停进程，保留 WAL 目录与 Mongo 备份；②启动错误已给出事务 ID 与 `<库>/<集合>/<_id> expected=0 next=1`：在 Mongo 确认该 `_id` 是墓碑（`_deleted: true`），`roost id entity <_id>` 换算出玩家 ID，`cmd/walinspect -dir <WAL目录> -snapshot <新目录>` 做一份完整只读副本留证（它不推进 checkpoint、不跳过记录）；③线上尚未部署（维护者 2026-10-06）：直接停旧版本、清空该 sid 的 WAL 目录与测试库后起新版本；上线后若遇到，由维护者决定是否接受丢弃该事务（被丢弃的是删档后被错误重建的那条写入，客户端当时收到过成功），并修正复用 ID 的业务代码，框架不提供“跳过一条记录”的入口。见[维护手册 §4](README.md#4-故障处理) |
| Windows | 非框架保证平台，只保证编译与 CLI 兼容（维护者 2026-10-14）。CI `windows-compatibility` 只跑 build、vet、`go test -run '^$'`（编译全部测试不执行）和 roost CLI 生成工程冒烟；完整 `go test` 的既有失败来自平台差异：打开中的文件不能改名/删除（nestwal 损坏记录检查、日志轮转、statslog），checkout 的 CRLF 使 gofmt 门禁与 kitconfig schema 比对失真（run 38500440362 的 6 个失败） | 本机测试结果不改变macOS/Linux支持范围；跨平台也成立的缺陷不归入此行 |

需要维护者做设计选择的事项（Lockstep 历史、SyncBus 投递、policy Drain、schema 迁移、Player 跨机）汇总在 [DESIGN-TODO](DESIGN-TODO.md)。[外部验证细则](EXTERNAL-VERIFICATION.md)保留具体环境、步骤和判据。[性能基线](../performance/STABLE-v1.24.0.md)保留失败样本、统计和参数。本次核对是静态文档整理和明确契约复核，不宣称全仓所有组合已审计、零bug或100%行为覆盖。
