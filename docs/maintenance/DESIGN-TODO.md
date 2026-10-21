# 待维护者决定的设计事项（TODO）

登记日期：2026-10-15；源码基线：`a91f5f72`（v1.27.0 之后只多文档）。本篇只列**需要维护者做产品 / 设计选择**的事项；每项给出现状、问题、可选方向与代价、建议和需要拍板的问题。工程缺口、性能与外部验收分别见 [KNOWN-LIMITS](KNOWN-LIMITS.md)、[EXTERNAL-VERIFICATION](EXTERNAL-VERIFICATION.md) 与 [performance](../performance/README.md)。决定后在对应条目写明“维护者 YYYY-MM-DD 决定 …”并转为实施任务或关闭；不把本篇条目当作已承诺的功能。

| # | 事项 | 状态 |
| --- | --- | --- |
| D1 | Lockstep 历史的保留与过期策略 | 维护者 2026-10-15 决定 C，已实施；2026-10-20 方案 B（全量快照恢复）立项并批准（§12 全部按推荐），已实施（P1～P5） |
| D2 | SyncBus 有限投递的兜底 | 维护者 2026-10-15 决定 A，已实施 |
| D3 | Entity Sync `Drain` 遇到永久拒绝的出口 | 维护者 2026-10-15 决定 A，已实施 |
| D4 | 业务 schema 离线迁移的模板 / 流程 | 维护者 2026-10-15 决定 A，已实施 |
| D5 | 静态 Player 跨机迁移的参考流程 | 维护者 2026-10-15 决定 A，已实施 |
| D6 | 玩家看自己状态（Entity Sync 自身视图）的接入骨架 | 维护者 2026-10-16 决定折中方案，已实施（提交见本节） |
| D7 | 实体不存在时给客户端的稳定错误码与首登步骤 | 维护者 2026-10-16 决定 C，已实施（`e7e59298`） |
| D8 | 生成的服务包名大写（`package Game`） | 维护者 2026-10-16 决定 C，已实施（`bac32cd2`） |
| D9 | DAO 库名不随配置、`_id` 是编码后的 EntityID | 维护者 2026-10-16 决定 B，已实施（`a068be37`）；2026-10-20 决定方案 A 修订版（代码写逻辑名、部署映射物理库），已实施 |
| D10 | 删档后用同一 ID 重建（投影版本冲突、进程 fail-stop） | 维护者 2026-10-17 决定 B，范围只守 GetOrCreate，已实施（`572d87c2`） |
| D11 | 按实体的持久定时器接入（Buff 到期、建造完成等到点要做的事） | 维护者 2026-10-17 决定 B 与离线语义，已实施（`a2b4f4d8`、`5581deeb`） |
| D12 | Saga 原生实体步骤与事务性 effect 消费者的生成支持 | 维护者 2026-10-18 决定 A，已实施；四个未验证项已验证（修两处缺陷）；同日批准 B（生成器），已实施；原生步骤单进程容量按 A 接受并文档化 |
| D13 | DAO 持久键的来源与水合时的键集合校验（改 Go 字段名静默丢老数据） | 维护者 2026-10-19 决定并已实施：定义里的 `bson` 名就是持久键（缺省 snake_case），生成持久文档结构体；改字段生成期 + 加载期 WARN、不拒绝；旧键不加载、整文档写回即删除，业务层约束写进文档（同日撤销方案 A 与清理入口） |
| D14 | AOI 观察者改为 playerId、视野半径按观察者配置 | 维护者 2026-10-20 决定 A1 + B1（客户端请求 LOD 层级 + 视野大小，层级对应 Bands、视野大小服务端夹取量化），已实施（2026-10-20；前后 benchstat 由维护者夜间统一跑） |
| D15 | 合服（把一个服并入另一个服） | 维护者 2026-10-20 决定别名合服，设计 §10 八项均按推荐，已实施（P1～P4） |
| D16 | 单个实体 / 玩家的运维入口（踢下线、卸载、改档与恢复） | 维护者 2026-10-20 决定 C（按推荐），已实施（2026-10-20：`entity.kick` / `entity.unload`） |
| D17 | 全服邮件的内置投递策略 | 维护者 2026-10-20 决定 A（全局邮件一个 ID，个人邮箱拉取合并），已实施（`mail.NewLazyBroadcast`） |
| D18 | 业务时区与“业务日”边界 | 维护者 2026-10-20 决定 A（按推荐，缺省 UTC），已实施 |
| D19 | 视野（View）：逻辑 ID、空间 / 关系两类、玩家与视野多对多 | 维护者 2026-10-20 决定实施（int64 视野 ID、同步层级取高或并集、MaxVisible 按视野、不进 Nest、删除旧 RelationSource），已实施（2026-10-20）；多视野修复后排队事实分两种应用模式（ApplySync 缺省 / ApplyAsync，维护者 2026-10-21 决定，已实施）；三项小决定按推荐保留 |
| D20 | 移动由实体自己的心跳驱动：MapBasic / Move 组件 + actionflow 控制 | 维护者 2026-10-21 定方案（①～⑮ 与压测后决定）；**P1 心跳框架已实施**（`gameplay/heartbeat` + `wiring/heartbeat`，见下文“P1 实施结果”）；P1 后决定 ④（Remote 共享模式不允许挂载心跳）已实施；**P2 组件与 actionflow 适配已实施**（`gameplay/movement` + `actionflow.BeatRunner`，见下文“P2 实施结果”）；真实 Remote 共享模式切换下的心跳、移动、任务与 AI 已验证并修了两处恢复缺陷（见下文“真实 Remote 共享模式切换验证”）；**P4 压测工具心跳驱动代码已实施、未测量**（见下文“P4 实施结果”，测量待 v1.31 发版前压测）；**P5 技能与 Buff 的快池 tick 迁移已实施**（`combatcomponent.AttachBeat` / `NewRuntimeBeat` + `skill.TickClock`，见下文“P5 实施结果”；spawner 一项归 P3）；P3 未做；Nest 批量心跳作业（选项 C）已实施，维护者 2026-10-21 决定先合入、缺省关闭（`heartbeat.delivery: batch` 开启），合批优化压测暂停待补（[设计](FEATURE-2026-10-21-nest-batch-heartbeat.md)） |
| D21 | 吸收 ssr 的优秀组件进 `gameplay` | 维护者 2026-10-21 登记为后续任务；调研一（action / action_list / mission / ai / state 对比与引入方案）已写；维护者 2026-10-21 决定调研一 13 条全部按推荐（A），**已实施**（D21-1～D21-5，已合入 main，见本节“调研一 · 实施”；同日 `actionflow.Agent` 更名为 `Behavior`）；实施中的 4 项小决定维护者 2026-10-21 确认按推荐；其余 ssr 候选未调研 |
| D22 | 业务时间取自 ctx、支持变速 | 维护者 2026-10-21 提出（D21 调研中补充、单列）；同日决定范围按 ssr 收窄：全局不做变速（D22-2 取消），只做 D22-1 与场景时间域；**D22-1 与场景时间域已实施**（见下文“实施结果”；D21 合入后补了 `Behavior` 缺省时间与 actionflow / ai 的 `BeatConfig.TimeDomain`） |

已决定、不在本篇：Session 运行服务保持现状、不改为可选（2026-10-14）；玩家接入目录 `game/playeraccess` 改名已随 v1.27.0 发布。

## D1 Lockstep 历史的保留与过期策略

**状态：维护者 2026-10-15 决定 C（按推荐），已实施（`a6376934`）。** 方案 B（全量快照恢复）当时不做；**维护者 2026-10-20 决定立项**（恢复方式做成 Match 选项 catchup / snapshot / auto，快照来源由业务经接口提供），设计见 [FEATURE-2026-10-20-lockstep-snapshot-recovery.md](FEATURE-2026-10-20-lockstep-snapshot-recovery.md)（维护者 2026-10-20 批准，§12 十三个决定点全部按推荐）。**方案 B 已实施（P1～P5）**，结果见下文“方案 B 实施结果”。

**实施结果**：
- `MatchConfig.HistoryRetainFrames` 默认 `DefaultHistoryRetainFrames` = **3600 帧**（60Hz 下 60 秒、30Hz 下 120 秒；依据：重连窗口按 60 秒、常见逻辑帧率上限 60Hz 换算，10 席 × 8 字节输入约 1.5MB / 局）。`RetainFullHistory`（-1）显式关闭，退回宿主手动 `TrimHistory` / `TrimHashReports`；正值下限 `MinHistoryRetainFrames` = `ReplayHorizon` = 64（Sequencer 视为仍在线的落后量，同时覆盖一整页追帧与最大冗余深度），不满足时 `NewMatch` 返回 `ErrMatchConfigInvalid`。
- `Tick` 在发出本次追帧页之后裁历史；hash 报告按 64 帧步长跟随同一窗口。宿主手动裁剪 API 保留。
- 追帧越界：`StartCatchup` / `SpectatorCatchup` / `HandleCommand(OpCatchup)` 立即返回 `*CatchupUnservableError`（`errors.Is` `ErrCatchupUnservable`，带 `From` / `Start`）；追帧途中被窗口越过时下一次 `Tick` 返回同一错误。两种情况都经可靠通道给该会话发 C9 v1 追帧失败通知，计 `lockstep.catchup.unservable.total`。
- 客户端表现统一为“追帧失败、交由业务决定踢出或重开”，不自动重同步：Go `FrameAssembler.Ingest` 返回 `*CatchupUnservableError`、`robot.LockstepBot` 终止；C# `LockstepClient` 抛 `LockstepCatchupUnservableException` 并终止实例。
- 一并修复：起点 0 原先绕过越界检查，裁剪后从 0 追帧会得到一段有洞的帧流且无错误。

说明与证据见 [Sync 使用指南](../framework/guide/04-sync.md#越界的处理按恢复方式区分)、[SYNC-OPTIMIZATION](../framework/SYNC-OPTIMIZATION.md#lockstep-自动保留与追帧失败通知2026-10-15)。上面的“越界即失败”现在只是 catchup 模式（没配快照来源时的缺省）的行为；C9 已升 v2。

**方案 B 实施结果**（2026-10-20，P1 `f33e1b01` / `a02c3999`、P2 `69fa9e79`、P3 `f03a6602`、P4 `8781b32a`、P5 文档与 detached 计数）：
- `MatchConfig.Recovery`（catchup / snapshot / auto；零值在配了 `Snapshots.Provider` 时为 auto，没配仍是 catchup，现有工程行为不变）与 `SnapshotConfig`（阈值缺省 0、上限 4 MiB / 硬上限 64 MiB、分块 16 KiB、每 Tick 64 KiB / 只准入 16 KiB、取快照 2s、钉住缺省一个窗口）。
- 业务快照来源 `SnapshotProvider`：Match 自己的 goroutine 调用、`Tick` 不等、每局至多一次在途、多会话共享；快照以 CA 01 分块经可靠通道下发（SHA-256 摘要 + 可选状态 hash），之后续发历史页；恢复期间钉住历史；失败通知 C9 v2 带原因（history_trimmed / snapshot_unavailable / snapshot_too_large / recovery_expired）。
- Go `FrameAssembler.Receive` / `ResumeAfterSnapshot`、`robot.LockstepBot.Restore`；C# `LockstepClient` 的 `restore`；校验失败一律终止、不自动重试。
- 指标 `lockstep.snapshot.total{reason}` / `bytes.total` / `failed.total{reason}`（含维护者 2026-10-20 追加的 `detached`）。
- 验收：包内与真实回环 TCP、`Example_snapshotRecovery`、MOBA 十客户端 KCP 长断线恢复、生成工程 C# 端到端、toxiproxy 限速演练（嵌入 TCP 两档恢复；独立 Gate 慢链路上大快照被断开循环）。
- 已做（2026-10-20）：MOBA 持久对局 `Room` 接快照来源（已提交门 `MatchConfig.CommittedFrame`，设计 §15）。
- 未做 / 后续：独立 Gate 写出回执；客户端上传快照；Unity / IL2CPP 实测。剩余边界见 [KNOWN-LIMITS](KNOWN-LIMITS.md#lockstep)，现行说明见 [guide/04-sync §6](../framework/guide/04-sync.md#落后的会话怎样回来追帧快照或自动)。

**原登记（2026-10-15）**：宿主不裁剪则内存随对局长度线性增长，裁得太激进则断线较久的玩家无法追帧，且“追不上时怎么办”没有框架级约定；可选 A 维持现状 + 文档、B 自动裁剪 + 全量重同步（需状态快照来源）、C 只加自动裁剪 + 明确错误。

## D2 SyncBus 有限投递的兜底

**决定与状态**：维护者 2026-10-15 决定 A（按推荐），已实施（`f385f8c5`）：SyncBus 定位为默认可丢广播，需要最终一致的状态由业务做“丢失检测（序号缺口 / 版本落后 / 投递耗尽）→ 拉快照对账”闭环；写法见 [guide/04-sync §5“SyncBus 的定位与丢失兜底”](../framework/guide/04-sync.md#syncbus-的定位与丢失兜底)，可运行模板为 [syncbus/example_test.go](../../framework/sync/syncbus/example_test.go) 的 `Example_reconcileAfterLoss`。B（框架级对账）记为下一阶段候选，未实施；C 不采用。以下为决定前的原始描述。

**现状**：JetStream SyncBus 默认有限投递次数（5）、有保留期限与容量，**不保证绝不丢**（[guide/04-sync](../framework/guide/04-sync.md)）；KNOWN-LIMITS 要求业务做快照 / 对账闭环，不能只测重试成功。

**问题**：消息耗尽重试或过期后，订阅方状态与权威可能长期不一致；目前框架只计数 / 记日志，没有自动修复路径，各业务需要自己设计对账。

**可选方向**：
- A 维持现状 + 文档模板：给出“丢失检测（序号缺口 / 版本落后）→ 拉取快照”的推荐写法与示例。
- B 框架级对账：SyncBus 订阅端检测到缺口或投递耗尽时，自动向发布端请求快照（复用 Remote 快照 / Entity Sync 全量机制），并提供指标与告警。代价：要定义快照请求协议、限流和发布端负载上限。
- C 提高下限：默认无限重试 + 死信队列，由运维处理死信。代价：积压时内存 / 存储压力转移到 broker，仍不能消除对账需求。

**建议**：A 立即可做；B 作为下一阶段候选，先确认哪些业务用 SyncBus 承载需要强一致的状态（若只用于可丢的广播，A 足够）。

**需要拍板**：SyncBus 的定位（可丢广播 vs 需要最终一致的状态通道）；若是后者，是否接受 B 的协议与负载成本。

## D3 Entity Sync `Drain` 遇到永久拒绝的出口

**状态：维护者 2026-10-15 决定 A（按推荐），已实施（`b86a1286`）。** Drain 行为不变、不放弃任何条目；方案 B（明确放弃出口）需先梳理 policy 侧哪些拒绝是永久的，未做。

**实施结果**：
- ctx 超时 / 取消时 `Manager.Drain` 返回 `*entitysync.DrainStalledError`：`errors.Is(err, context.DeadlineExceeded / Canceled)` 不变；`Stalls` 列前 `DrainStallLimit` = 8 项，`Total` 为全部条目数。每项有来源（subject / snapshot / policy / interest）、subject 与会话、最后一次暂缓或拒绝的原因（`commit_pending`、`durability_wait`、`prepare_failed`、`snapshot_budget`、`subscribe_refused`、`policy_pending` 等）与说明、连续等待时长、最后错误。
- 原因来源：Flush 在暂缓 subject 时记下原因与首次时间（只在暂缓期间存在，随下一次处理更新或删除）；快照预算等待沿用已有起点；政策通过 `RegisterPolicy(PolicyHooks{Apply, Pending, Stalls})` 报告，Interest 报告未确认提交的排队事实与上界内被拒的订阅（首次拒绝时间、最后错误）。
- 指标：`entitysync_drain_timeout_total`（Counter）、`entitysync_drain_remaining_entries`（Gauge，成功排空为 0），均无标签。

说明见 [Sync 使用指南 §4](../framework/guide/04-sync.md#停机排空drain卡住时看什么)。

**原登记（2026-10-15）**：停机时若某个订阅 / 关系被永久拒绝，Drain 一直等到 ctx 超时，调用方只拿到 `context.DeadlineExceeded`，看不出是哪个条目卡住、是否可以安全放弃；可选 A 诊断增强、B 明确放弃出口、C 只写文档。

## D4 业务 schema 离线迁移的模板 / 流程

**决定与状态**：维护者 2026-10-15 决定 A（按推荐），已实施（`12aa1a03`）：[业务 schema 变更操作手册](../framework/SCHEMA-MIGRATION.md)（停写 → 备份 → 转换 → 校验 → 切换版本 → 回退，与 DAO 只接受当前 schema、水合前校验的规则对齐）；game-demo 模板新增 `db/migrations` 离线执行器与 `cmd/schemamigrate`（plan / apply / verify / rollback），单元测试随生成工程运行，`TestPlayerMigrationRealMongo` 由 `scripts/test-demo-generated.sh` 在真实副本集上运行；2026-10-15 在本机私有环境用真实 game 进程演练一次（记录在手册 §4）。迁移仍要求离线停写；上线后是否允许在线双读期需重新决定。B（`roost add migration` 生成器支持）记为候选，未实施。以下为决定前的原始描述。

**现状**：DAO 自动迁移已撤销（DAO 只接受当前 schema，水合前校验，不一致即拒绝）；保留一个通用的显式迁移工具 [`infra/storage/migration`](../../infra/storage/migration/migration.go)（步骤登记与执行），业务需要自己写离线转换。升级要求见 [maintenance README §1–§2](README.md)。

**问题**：业务第一次改 schema 时没有现成的端到端流程（停写 → 备份 → 转换 → 校验 → 切换版本 → 回退方案），容易各写一套；生成器也不产出迁移骨架。

**可选方向**：
- A 文档 + 示例：在 docs/framework 写一篇“schema 变更操作手册”，demo 里加一个完整的迁移示例（含校验与回退）。
- B 生成器支持：`roost add migration <name>` 生成迁移骨架（步骤、幂等标记、校验函数、dry-run），接入 `migration` 工具与 doctor 检查。
- C 维持现状。

**建议**：A 先做；B 视业务使用频率再定。

**需要拍板**：是否需要生成器层面的支持（B）；迁移是否必须离线（停写）还是允许在线双读期（与“未上线不做兼容”的原则相关，上线后需要重新决定）。

## D5 静态 Player 跨机迁移的参考流程

**决定与状态**：维护者 2026-10-15 决定 A（按推荐），已实施（`ec96b74f`）：[静态 Player 跨机迁移参考流程](../framework/PLAYER-MIGRATION.md)按 sid 搬机，每一步指到现有 API 与检查点；game-demo 模板新增检查工具 `cmd/sidmove`（WAL 已排空、单实例锁持有者、玩家文档未变）。2026-10-15 本机演练：同 sid 两个 game 进程在持续写入下迁移、崩溃回退与“投影未排空”场景，确认计数无缺口无重复（记录在该篇 §4；同机两进程，未覆盖真实跨机网络、卷迁移与客户端重连）。框架没有把单个玩家改绑到另一个 sid 的能力（`Role.ServerID` 建角后不变），“单个玩家换服 / 合服”是新的产品功能，未登记为本项范围。B（框架级迁移命令）记为上线后评估，未实施。以下为决定前的原始描述。

**现状**：静态 Player 跨机迁移只以已落库数据为准，WAL 只负责保证落库，不做跨机 WAL 热迁移（[guide/03-dataengine](../framework/guide/03-dataengine.md)、[maintenance README §2](README.md)）；停写、排空、切换方案由应用负责。

**问题**：扩缩容 / 迁服时每个业务都要自己设计“停写 → 等投影落库 → 切路由 → 新节点加载”，顺序错误会导致丢写或双写；框架已有的单实例锁、投影等待、Directory / 路由能力没有被组织成一条可直接用的流程。

**可选方向**：
- A 参考流程文档 + 运维脚本示例：明确每一步用哪个现有能力（单实例锁释放、`WaitProjection`、Directory 切换、拒绝新准入）与检查点。
- B 框架级“迁移 Player”命令：在 ops 管理面提供 drain-and-move 操作，自动按步骤执行并校验。代价：要处理失败回滚与部分完成状态，属于较大的新功能。
- C 维持现状。

**建议**：A 先做，并用 demo 在本机演练一次；B 等上线后有真实迁服需求再评估。

**需要拍板**：是否需要框架级迁移命令（B）；上线前是否要求完成一次端到端迁服演练。

## D6 玩家看自己状态（Entity Sync 自身视图）的接入骨架

**状态：维护者 2026-10-16 决定折中方案，已实施**（提交 `fc0a8315` core、`9aa7be92` 生成器）。登记：2026-10-16 新用户按 beginner 流程写商店（金币 / 道具 DAO 字段已标 `sync`）后，想让客户端收到自己的金币与道具变化，找不到可走的路。

**登记时的现状**：生成的 bootstrap 固定写 `kitnest.NewMod(EntityAccess)`（`codegen/internal/roost/render.go` 的 `nest` 分支），roost.yaml 没有开关能改用 `wiring/nest.NewModWithEntitySync`；生成器与 demo 都不调用它。demo 的场景同步自己建 `entitysync.Manager` + Interest 政策（`demo/internal/service/game/scene.go.tmpl`），并手写卸载重同步、加载后重绑、held / ready 会话与客户端 `scene_ready`。最简单的“只看自己”需要业务自己拼：Manager、`policy.Direct`、把 `TCPRuntime.PushSyncPlayer` 包成 Transport、登录时 Register / OpenSession / Bind、断线 CloseSession、卸载与重载、客户端解原始 Sync 帧。DAO 的 `sync` 标记对新工程没有可见效果。

**登记时的可选方向**：A 在 `access player` 上加 `self_sync: true`，由生成器生成全部装配与生命周期；B 只补文档与 demo 中抽出的最小示例；C 维持现状。

**维护者决定（2026-10-16，折中）**：容易写错的并发生命周期放在 core，一个小而清楚的公开 API；生成器只在生成器所有的 `*_gen.go` 里写可见接线；Go 客户端补解码示例，C# 本轮不做；要有真实进程回归。

**实施结果**：

- core：`framework/sync/entitysync/policy.SelfView`（生命周期：回调只记事实，Flush 的政策阶段按净事实对齐会话 / 登记 / `Direct` 绑定，profile 固定 `self`）；`wiring/selfsync`（`PushSyncPlayer` → `entitysync.Transport`、嵌入 TCP 握手钩子与 Game Ingress OnActive / OnClosed、`EntitySyncSetup`）；`wiring/nest.EntitySyncSetup` 新增 `Attach` / `EntityLoaded` 两个可选钩子。包位置取舍：生命周期属于 Sync 块（只依赖 entitysync 与 entity，用 entitysync 的 harness 测各种事件顺序），放在已有 Direct / Interest 旁边；gateway 适配与 Nest Mod 装配属于接线，放 wiring，避免 sync 块依赖网络层。
- 生成器：`roost.yaml` 的 `access.player.self_sync: true`（默认关，需要 tcp 传输与 entity feature）；开启时 `internal/bootstrap/generated.go` 改用 `NewModWithEntitySync` 并生成 `PlayerSelfSync`，`server_gen.go` 生成 `WithSelfSync`，缺失时创建一次业务文件 `protocol/def/self_sync.go`（推送编号）；Player 的标记缺省按 `sync=true`；2026-10-17 起没写 `subjectPacker=` 时 entity 生成器写默认 packer `game/entities/player/player_sync_packer_gen.go`（维护者决定，代价见 KNOWN-LIMITS“self_sync 默认 packer 的代价”），`project sync` / `generate` / `doctor` 对没有 DAO、多个 DAO、显式 `sync=false` 给出改法，对 DAO 没有 sync 字段给警告。关闭时生成物不变。
- 客户端：Go 解码示例 `wiring/selfsync/example_client_test.go`（随测试运行）；C# 本轮不做（`SyncReceiver` 能收帧，subject 载荷解码需业务自写）。
- 回归：`policy/self_view_test.go`（断线重连、顶号、登录与断线并发、关闭与 Flush 并发、丢会话后等连接关闭（2026-10-17 起不再重开，见 impl/04-sync §2C）、卸载撤回重提交、新建回滚重绑）、`wiring/selfsync/selfsync_test.go`（真实 gateway TCP）、`codegen/internal/roost/self_sync_test.go`（按新手流程生成工程、打开开关、经生成接线跑真实 TCP）；另在共享隔离环境（真实 Mongo / NATS / Redis）跑了一次生成工程真实进程：登录 → EnterGame → 全量 gold=0 → AddGold → 增量 5、12 → 断线重连 → 全量 12 → 增量 13；进程重启后冷加载 → 全量 13。

用法见 [PLAYER-ACCESS §7](../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)，实现与不变量见 [impl/04-sync §2C](../framework/impl/04-sync.md#2c-自身视图selfview的生命周期)。剩余限制在 KNOWN-LIMITS“自身视图（self_sync）”一行。

## D7 实体不存在时给客户端的稳定错误码与首登步骤

**状态：维护者 2026-10-16 决定 C（两者都做），已实施（`e7e59298`）。** 登记：2026-10-16 新用户试用实测。

**实施结果**：
- 框架码：`infra/base/errcode` 新增框架段 `1–999`（只由 errcode 包分配，包注释登记全部码段归属）与 `CodeEntityNotFound = 2` / `ErrEntityNotFound`（`entity_not_found`）。依赖方向：errcode 在 infra、不认识 framework；判定在 Nest：`framework/nest/io_load.go` 的 `targetLoadError` 遇到 `entity.ErrAuthorityEntityNotFound`（DataEngine `ErrEntityAggregateNotFound` 满足它）时 `errcode.Wrap(errcode.ErrEntityNotFound, err)`，原错误保留。生成 endpoint 只给 `CodeInternal` 记 ERROR，因此这种预期情况不再记 ERROR。`roost.yaml ids.errcode` 下界落进框架段时 manifest 校验拒绝。
- 已删档：实施时登记为与“从未创建”同一哨兵、同一个码（只在错误文本 `state=absent|deleted` 区分）；删档后同 ID 重建 2026-10-16 实测会让投影版本冲突、进程 fail-stop，转为 [D10](#d10-删档后用同一-id-重建)。**2026-10-17 起由 D10 取代**：删过档是独立哨兵与客户端码 `entity_deleted = 3`；而且原先在真实 Mongo 上墓碑被读取过滤，`state=deleted` 实际从未出现（见 D10）。
- 首登步骤：`roost add endpoint EnterGame --handler player --lifecycle Player` 生成登录 endpoint（`<Entity>FromRegistry(controller.Registry()).GetOrCreate(ctx, context.PlayerID)`）；两处 controller 骨架都带 `Registry()`；first-business 新增 `workflow:login` 检查，`roost project next` 给出上面的命令；workbook、CODEGEN_REFERENCE、README 同步。
- 端到端验证（2026-10-16，已验证）：当前主线（`e346fe3f`）生成新手工程走完 first-business（含 `roost add endpoint EnterGame --handler player --lifecycle Player`）与 `roost add transport tcp`，game 服务连本机共享隔离环境的真实 Mongo 副本集 / NATS（DataEngine 真实落库，私有库名与流名），真实 TCP 客户端：从未登录的玩家直接发 RenamePlayer 收到 `code=2 reason="entity not found"`，服务端日志无 ERROR / WARN；同一连接发 EnterGame（`code=0`）后再发 RenamePlayer 成功（`code=0`），Mongo 里 `name` 落库、`_version=1`；重启进程后该玩家直接发 RenamePlayer 冷加载成功、`_version=2`，另一个新玩家仍先得到 `code=2`。已删档的玩家直接发业务请求同样得到 `code=2`。只测了嵌入 TCP：新手工程没有独立 Gate 传输，game-demo 的 Gate 模式要另起 Gate 进程、Redis 单实例身份与账号票据，本轮未起；`code=2` 由 controller 写进 Response，Gate 只转发回包字节，与传输无关。

**现状**：一个从未登录（Entity 不存在）的玩家直接发业务请求时，Nest 冷加载返回 `dataengine repository: entity aggregate not found`，`errcode.ClientError` 把它归为 `CodeInternal`（客户端收到 `code=1 server error`），服务端记 ERROR。beginner 的 first-business 流程（`roost project next` / `doctor --workflow first-business`）全绿后不包含“登录时 `lifecycle.GetOrCreate`”这一步，新手第一次真实请求就会碰到。

**可选方向**：A 框架定义一个稳定错误码（例如 `errcode` 里的 `entity_not_found`），Nest 冷加载不存在时包装它，客户端可提示重新登录；代价是 nest / dataengine 错误与 errcode 产生依赖，要定码段归属。B 不加框架码，在 first-business 流程里加一步 `roost add endpoint EnterGame`（调用 lifecycle.GetOrCreate）并让 doctor 检查；代价是只解决新工程，冷加载不存在仍是 Internal。C 两者都做。

**建议**：C。需要拍板：码值与名字放在框架还是生成工程；“不存在”是否要区分已删档。

## D10 删档后用同一 ID 重建

**状态：维护者 2026-10-17 决定 B，范围只守 GetOrCreate，已实施（`572d87c2`）。** 维护者原话要点：删档后该 ID 不可再用；重建只需要“创建失败”（给调用方一个明确错误）。现在 ID 都由 id-gen 生成、不重复，Player 不会删除，实际很难遇到。同日收窄范围：只守 GetOrCreate（登录 / 创角边界），直接 Create 维持原行为与原代价，靠“必须用 id-gen 新 ID”的约束。登记：2026-10-16。

**实施结果**：

- **删过档是独立状态**。`entity.ErrAuthorityEntityDeleted`（与 `ErrAuthorityEntityNotFound` 同层）/ DataEngine `ErrEntityAggregateDeleted`：冷加载读到全部 DAO 都是墓碑时返回它。它**刻意不满足** `errors.Is(err, ErrAuthorityEntityNotFound)`，理由是失败方向安全：所有“不存在就创建”的代码（生成 lifecycle 的 `GetOrCreate`，以及业务自写、或生成后归业务所有而没有重新生成的同类逻辑）遇到删过档的 ID 都不会去创建，而是把错误交给调用方；反过来若让它满足 not found，每一处这种代码都得记得先排除删档，漏一处就回到 fence。需要“删档与不存在同样处理”的框架调用方逐一改为两者都判：卸载后重载（`unload_resync.go`）对两者都立即退回 remove。
- **修正一个错误前提**：原登记与 D7 写“冷加载报 `state=deleted`、数据层能区分”，实际在真实 Mongo 上 `MongoStore.Load` 默认过滤墓碑（`_deleted != true`），删过档一直读作 `state=absent`，只有测试替身会交出墓碑。现在 `LoadSpec.IncludeDeleted` 让聚合仓库的单实体一致性读连墓碑一起读（`entity_repository.go` `readAggregate`），其他读取照旧只读活文档。
- **Remote 托管实体**：Remote 删档**物理删除**数据文档，墓碑只在 Remote 元数据（`_remote_entity_meta._deleted`）里。`readAggregate` 对 Remote 托管 kind 在全部缺失时，于同一次一致性读里经 Store 可选的 `RemoteEntityDeleted`（`MongoStore` → `remoteentity.Backend` → `MongoCommitter`）查它，删过档同样返回 `ErrEntityAggregateDeleted`。修前实测（真实 Mongo / Redis / NATS）：冷加载 `state=absent`，同 ID 经 GetOrCreate 的判断会在内存里重建，之后每次写都以 `remote entity: state version conflict: entity … loaded version 0, authority 2` 失败（不 fence，但实体看似建成却永远写不进去）。
- **客户端码**：新增框架码 `errcode.CodeEntityDeleted = 3`（`entity_deleted`），框架段分段测试同步。取舍：沿用 `entity_not_found = 2` 会误导客户端——2 的约定是“先 EnterGame 再重试”，而删过档的玩家 EnterGame 同样失败，客户端会陷入“登录 → 失败 → 再登录”；单独的码让客户端直接提示“角色已删除”，走产品的新建角色流程（换新 ID）。Nest 在声明目标的加载结果处包装（`io_load.go` `entityStateError`，发往删过档 ID 的业务请求回复 3），生成 lifecycle 在 `Get` 处包装（`deleted<Entity>Error`，`GetOrCreate` 经它返回 3）。
- **不放宽任何持久契约**：没有跳过冲突 WAL、没有放宽版本比较、没有在投影阶段伪装成功；墓碑不被覆盖。

| 创建入口 | 准入前是否读存储 | 现在 |
| --- | --- | --- |
| 生成 lifecycle `GetOrCreate`（EnterGame、game 模板 `EnsureWorld`） | 读（冷加载） | 冷加载返回删过档，不再 Create；错误带 `entity_deleted`，客户端码 3。**受保护** |
| 发往删过档 ID 的 Nest 业务请求 | 读（I/O 阶段冷加载声明目标） | 回复码 3，handler 不执行。**受保护** |
| Remote 托管实体经 GetOrCreate | 读（冷加载 + 元数据墓碑） | 同上。**受保护** |
| 生成 lifecycle `Create`、Nest 之外 `ManagerAccess.Create` / `EntityManager.Create`（显式 ID） | 不读 | **不受保护**，与 `bdc9003a` 相同：零额外读 |
| Nest handler 内 `Create` / `CreateInScope`（显式 ID） | 不读（业务池不能 I/O） | **不受保护**，与 `bdc9003a` 相同：零回滚重排 |
| ID 由 `EntityManager` 的 ID 生成器分配 | 不读 | 生成器契约永不重复 |

- **约束（直接 Create）**：handler 内或 lifecycle `Create` 必须使用 id-gen 发的新 ID，不要复用删过档的确定性 ID（也不要对“存了但没加载”的 ID 直接 Create）。违反时新实体的首个提交是版本 0 的整文档 Put，投影撞上已有文档 / 墓碑，`ErrProjectionConflict` fatal、进程 fence，冷重启的启动恢复同样失败；处置见 [KNOWN-LIMITS](KNOWN-LIMITS.md)“删档后同 ID 重建：违反约束或修复前写下的冲突记录”与[维护手册 §4](README.md#4-故障处理)。ID 可能已有记录时一律用 `GetOrCreate`。
- **范围收窄前曾实现、已撤回的部分**：显式 ID 新建前的“占位 + 等在途投影 + 读权威”检查（含 handler 内新建的回滚 + 重排到 I/O 阶段复查、memory 快路径直接拒绝、“已在内存的 ID 绝不发布未检查实例”的发布守卫）。它们只服务于直接 Create 的保护，没有独立修复既有缺陷，按维护者决定全部撤回；正常创建路径恢复 `bdc9003a` 的行为与代价。其中“对存了但没加载的 ID 直接 Create 同样会投影冲突 fence”是同一类问题，撤回后照旧，由上面的约束覆盖。
- **性能代价**：创建路径零变化；冷加载路径不变（`IncludeDeleted` 只放宽了同一次 `_id` 查询的过滤条件；Remote 托管 kind 只在全部缺失时多读一次元数据文档）。
- **启动恢复与修复前写下的冲突记录**：修复前已经进入 WAL 的“删档后重建”记录（首个 `Put expected=0` 撞墓碑），以及今后违反上述约束写下的记录，都会让启动投影恢复 `fatal projection version conflict`，进程起不来。框架不自动删记录、不跳过、不放宽版本比较；人工处置步骤见 KNOWN-LIMITS。线上尚未部署，按“未上线不兼容”的约定，正常做法是停进程、清空该 sid 的 WAL 目录与测试库后再起新版本。
- **审计结论（墓碑保留了什么、删档审计是否需要另存）**：方案 B 下经 GetOrCreate 的路径不会覆盖墓碑（遵守约束时也没有别的路径会覆盖）。墓碑是对删除前文档的 `$set`，保留：删除前的全部业务字段（最后状态）、`_version`（删除前 + 1）、`_schema`、`_deleted: true`、`_deleted_at`（**投影时**的墙钟，UTC；重放幂等不改写）、`_last_tx`（删除事务 ID）。不保留：删除原因（`DestroyReason`）、操作者 / 请求 ID、准入时刻；`_last_tx` 指向的 `_dataengine_transactions` 标记按 TTL（默认 30 天）过期，WAL 在 checkpoint 后截断。Remote 托管实体的数据文档被物理删除，只剩元数据墓碑（`_deleted`、`_ver`、fence / epoch），不保留删前业务字段。结论：“这个 ID 是否删过、何时删、（本地实体）删前是什么数据”墓碑够用，框架不需要另存；“谁、为什么删”以及 Remote 实体的删前数据属于业务审计，由删档入口（GM / 运营工具）自己记录（统计日志或业务审计表），框架不提供。运维若手工清除某 ID 的全部墓碑，这个 ID 会重新变成“不存在”，GetOrCreate 可以再创建。
- **回归（先红后绿）**：
  - 真实 Mongo、正式生成 Entity / DAO：`TestGeneratedDataEngineRecreateAfterDeleteSameID`（`codegen/internal/entity/testdata/dataengine/reload_projection_test.go`，去掉 KNOWN_RED 跳过，strict / async / pipelined）。修前（`bdc9003a` 上的原用例，`ROOST_GENERATED_KNOWN_RED=1`，它按 GetOrCreate 的判断在 not found 后 Create 同一 ID）重建后 Flush 得到 `recreate: projector fenced: transaction adbf420cf4ca13bf0000000000000004: dataengine mongo: mutation 0: dataengine mongo: fatal projection version conflict: Put duplicate key roost_generated_it_d10_basered_75359/trade_inventories/33823492 expected=0 next=1`（async 同样）。修后断言：冷加载只满足删过档；GetOrCreate 不重建；发往该 ID 的 Nest 请求回复码 3；WAL 准入计数不变、墓碑不变、引擎不 fence；其他实体照常写、新 ID 经 GetOrCreate 照常创建；冷重启后启动恢复无冲突，删过档的 ID 仍不可用，其他实体加载与写入正常。直接 Create 路径不再断言安全（用例注释写明约束）。实施中途还有一次红：只加哨兵、未让聚合读包含墓碑时，真实 Mongo 上冷加载仍是 `entity aggregate not found: entity=33823492 state=absent`，由此发现上面的错误前提。
  - Remote：`TestGeneratedRemoteRecreateAfterDeleteIsRefused`（`codegen/internal/entity/testdata/remoteflow/outcome_test.go`，真实 Mongo / Redis / NATS）。修前（只加本地墓碑读取时）红：`load deleted Remote entity: dataengine repository: entity aggregate not found: entity=4611686018437321649 state=absent, want ErrAuthorityEntityDeleted only`；修后冷加载与 GetOrCreate 的查找都只满足删过档、不发布、其他 Remote 实体照常写、无 fatal。
  - 框架单测：`framework/nest/cold_target_test.go` 的 `TestColdTargetDeletedInAuthorityAnswersEntityDeletedCode`（单 / 多目标回 `CodeEntityDeleted`、不满足 not found、handler 不执行）；`framework/dataengine/engine` 的 `TestEntityRepositorySeparatesAbsentFromDeleted`、`TestEntityRepositoryReadsRemoteTombstoneForAbsentRemoteAggregate`、`TestRepositoryReloadDoesNotResurrectPendingTombstone`（期望由 not found 改为 deleted）、`TestMongoStoreLoadExcludesTombstonesAndHonoursCallerFilter`（`IncludeDeleted`）；`infra/base/errcode` 框架段分段表；生成器 `TestExplicitFirstBusinessWorkflowGeneratesAccessLifecycleAndEndpoint` 钉住 lifecycle 的 `deletedPlayerError` 包装。
  - 生成工程真实进程（新手流程 + `roost add endpoint EnterGame --lifecycle Player` + 嵌入 TCP + 真实 Mongo 副本集 / NATS / Redis，私有库名、流名与键前缀，用后清理；临时加一个调 `lifecycle.Destroy(..., true)` 的删档 endpoint）：玩家 9001 `EnterGame code=0` → `RenamePlayer code=0` → 删档 `code=0` → `RenamePlayer code=3 entity deleted` → `EnterGame code=3 entity deleted`；玩家 9002 `EnterGame`、`RenamePlayer` 都是 `code=0`；从未登录的 9003 `RenamePlayer code=2 entity not found`；进程存活、日志无 ERROR。SIGTERM 正常退出后重启：9001 仍是 `code=3`，9002 改名 `code=0`，9003 `EnterGame` 新建后改名 `code=0`，日志无 ERROR。修前同一流程在真实进程上 fence 退出、重启起不来（见下方原登记）。

**原登记（2026-10-16，决定前）**：

**现象（实测）**：
- 框架层：`codegen/internal/entity/testdata/dataengine` 的 `TestGeneratedDataEngineRecreateAfterDeleteSameID`（正式生成 Entity / DAO、Runtime 删档准入、文件 WAL、真实 Mongo；strict 与 async 各一次）。创建并落库（`_version=2`）→ `Destroy(deletePersisted=true)` 落库成墓碑（`_deleted: true`、`_version=3`）→ 冷加载 `ErrEntityAggregateNotFound` → 同 ID `Create` + 写入 → `Flush` 得到 `fatal projection version conflict: Put duplicate key .../trade_inventories/<id> expected=0 next=1`，投影器 fence。用 `ROOST_GENERATED_KNOWN_RED=1` 运行复现（默认跳过）。
- 冷重启：同一 WAL 目录新起 Runtime，`startup projection recovery` 重放同一条记录再次 `fatal projection version conflict`，Runtime 启动失败。
- 生成工程真实进程（新手工程 + 嵌入 TCP + 真实 Mongo / NATS，临时加一个调 `lifecycle.Destroy(..., true)` 的删档 endpoint）：删档成功 → 同一玩家 RenamePlayer 得到 `code=2` → EnterGame `code=0`（GetOrCreate 新建）→ RenamePlayer 回复 `code=0`，随后服务端 `dataengine: fatal storage outcome; process is fenced`（`d7verify_nova_e2e/player/<id> expected=0 next=1 stored=2`），进程退出，其他玩家一起断线；再启动时 `mod dataengine start: dataengine runtime: startup projection recovery: ... fatal projection version conflict`，进程起不来。

**根因**：
1. 冷加载把全部墓碑读作不存在时丢掉了墓碑版本（`framework/dataengine/engine/entity_repository.go` `readAggregate`：`doc.Deleted` 只计数，`state=deleted` 只进错误文本）。
2. 生成 lifecycle `GetOrCreate`（`codegen/internal/roost/add_workflow.go`）在确切 not found 后 `Create(IsCreate: true)`，新实体各 DAO tracker 版本为 0；生成 DAO 的 `PrepareMutation`（`codegen/internal/dao/template_dao.go`）在版本 0 时生成 `Put expected=0 next=1`。
3. `MongoStore` 的版本条件对 `expected=0` 要求 `_version` 不存在（`framework/dataengine/engine/mongo_store.go` `mutationFilter`，批量路径 `mongo_projection.go` `batchMutationModel` 相同），墓碑带 `_version`，upsert 撞主键后 `classifyNoMatch` 判为 `ErrProjectionConflict`（fatal）。墓碑本来就只接受 `expected=墓碑版本` 的 Put 复活（`TestMongoStoreNewerPutRevivesTombstoneAtHigherVersion`）。
WAL 准入只看内存版本链（`ValidateMutation`：`next = expected + 1`），准入时不能做 I/O，所以冲突只能在投影时发现；此时回复已经发出、记录已经持久，按契约只能 fail-stop。

**可选方向**（都不跳过冲突 WAL、不放宽版本比较、不伪造持久确认）：
- **A 允许重建，版本接续墓碑**：冷加载读到全部墓碑时，错误带每个 DAO 的墓碑版本（仍满足 `errors.Is(err, ErrEntityAggregateNotFound)`，客户端码不变）；生成 lifecycle `GetOrCreate` 把它交给 `Create`（`EntityCreateParam` 新字段），新实体各 DAO tracker 从墓碑版本起步，且首个提交强制整文档 Put（墓碑保留了删除前的旧字段，Patch 会把新旧字段混在一起）；投影走已有的“严格高于墓碑的 Put 复活”路径。代价：`EntityCreateParam`、生成 DAO 模板（tracker 起始版本 + 首次整文档）、生成 lifecycle 各改一处，已生成工程的 lifecycle 归业务、要手改或重新生成；Nest handler 里直接 `Create` 一个删过档的 ID（没有冷读）仍会冲突，需文档约束或在 I/O 阶段先读；复活后墓碑的 `_deleted_at` 不再保留，删档审计要另存。Remote 实体已经是这种语义（删除后当前 fence 写入新版本 = 显式重建，见 wiring CHANGELOG“不变量 ③”）。
- **B 删档后该 ID 不再可用**：`state=deleted` 改为独立哨兵（例如 `ErrEntityAggregateDeleted`，不满足 not found），`GetOrCreate` 不再创建并返回它，客户端得到新码（例如 `entity_deleted`）或登录失败。代价：PlayerID 来自账号时，删角色等于该账号永远不能再建同 ID 角色，产品要另发 ID；直接 `Create` 删过档的 ID 仍会 fence，需要同样的约束。
- **C 重建前清墓碑**：`GetOrCreate` 发现 `state=deleted` 时，先在一个 Mongo 事务里按“`_deleted: true` 且版本等于读到的墓碑版本”删除全部 DAO 文档，再按新实体从版本 0 创建。代价：清理是 WAL 之外的直接存储写，偏离 Nest → WAL → 投影的唯一持久链路；版本回到 1，失去单调版本；删档审计同样丢失。

**建议**：A。D7 已定“已删档”与“从未创建”对客户端同义（先登录 / 创角），A 让这句话成立且只用现有的复活规则；B、C 分别改变产品语义与持久链路。

**需要拍板**：删档后是否允许同 ID 重建（A / C 允许，B 不允许）；选 A 时，handler 内直接新建删过档 ID 的处理（只写文档约束，还是 I/O 阶段先读墓碑）；删档审计（删除时间）是否需要另存。

## D8 生成的服务包名大写（`package Game`）

**状态：维护者 2026-10-16 决定 C（新工程小写、已有工程按目录现有包名），已实施（`bac32cd2`）。** 登记：2026-10-16。

**实施结果**：`codegen/internal/roost/service_package.go` 的 `withServicePackages(root)` 在 render 前读出每个 `internal/service/<name>/` 现有包名（优先业务文件），`renderService` / `renderServiceManagers` / `renderFrameworkCollaborators` / `renderFrameworkClients` / game 模板 Service 与 demo `{{GAME_SERVICE_PKG}}` 都用它；目录不存在时 `defaultServicePackage`（去 `_` / `-`、小写，关键字加 `service`）。game-demo 模板里的 `package Account` / `Chat` / `Platform` / `Session` 改成小写。业务文件不改写；老工程 sync 后仍是单一大写包名，可编译（用改动前的生成器造工程、再用新生成器 sync + generate + build/vet/test 实测）。

**现状**：`internal/service/<name>/` 下生成的文件用 `safeIdent(name)` 作包名（`render.go` 的 `renderService` / `renderServiceManagers`、`framework_services.go`），game-demo 模板同样是 `package Account`、`{{GAME_SERVICE_PKG}}`。合法但不合 Go 包名惯例（小写），新手读到 `package Game` 会以为写错。

**问题**：不能只改生成器：`service.go`、`managers.go` 归业务、生成一次后不再改写，已有工程里它们仍是大写包名，而生成器持有的 `framework_clients_gen.go` 等重写成小写后，同一目录出现两个包名，`roost project sync` 之后编译失败。

**可选方向**：A 改成小写，并让 `roost project sync` 对这几个已知业务文件只改 package 子句（或 doctor 给出一条命令）；B 维持大写，在 CODEGEN_REFERENCE 写明是约定；C 新工程小写、已有工程保持（按目录现有包名生成）。

**建议**：C（不改写业务文件，也不制造双包名）。需要拍板：是否值得为风格做这次迁移。

## D9 DAO 库名不随配置、`_id` 是编码后的 EntityID

**状态：维护者 2026-10-16 决定 B，已实施（`a068be37`）；2026-10-20 决定实施方案 A 的修订版（代码里写逻辑库名，部署时映射成物理库名），已实施；同日对映射规则再定两条：回调与配置冲突拒绝启动（选 B），没有映射回退到逻辑名并告警（选 A，保持），已实施。** 下文先写修订版 A 的结果，再保留方案 B 的实施记录与原始登记。

**修订版 A 实施结果（2026-10-20）**：

- **常量文件**：DAO 生成器扫描全部持久 DAO 的 `db=`（逻辑名，解析时校验：小写字母开头，小写字母、数字、下划线，不是系统库名；规则与运行期 `dataengine.ValidLogicalDatabaseName` 相同，因生成器不得 import 运行时而写了一份，不一致时生成物 `init` 里的 `DeclareDatabases` 会 panic），在 DAO 包（`db/`）生成 `gen_databases.go`：每个逻辑库一个常量（`DatabaseMain = "main"`），`init` 里 `dataengine.DeclareDatabases` 登记（含是否有 DAO 按 sid 分库）；`<Dao>DBName` 引用它。文件名沿用 DAO 生成器的 `gen_*` 命名（孤儿清理按这个前缀与生成头识别），没有持久 DAO 时按孤儿删除。
- **骨架**：`roost add dao` 写 `db=main`，game-demo 同样；生成配置写 `dataengine.databases: {main: <project_db>}`（`daoDatabaseName` 规则不变），新工程开箱的物理库与方案 B 相同。
- **映射入口与冻结**：配置 `dataengine.databases`（Mod 配置声明，`--check-config` / doctor 同源）与 `kitdataengine.WithDatabaseResolver`。DataEngine Mod **Init** 时合成并冻结（`dataengine.NewDatabaseMap`）：回调对每个声明的逻辑库调用一次。**同一逻辑库只能有一个来源**（维护者 2026-10-20 选 B）：配置没写的逻辑库取回调返回的非空物理名；配置写了的，回调返回空串（沿用配置）或与配置相同的值都允许，返回不同的非空值拒绝启动，报错点名逻辑库与两个值。先前“回调覆盖配置、启动日志告警”的行为已删除：两处给出不同的名字时静默选一个，等于让数据落到运维以为的库之外。冲突只能在 Init 判断——回调是代码、只在 Init 调用，`--check-config` / doctor 拿不到它，只校验配置本身。运行中只查表。表以能力 `dataengine.databases` 发布，并在 Provide 时绑定给 Remote 的 Mongo 存储（Remote 存储先于 DataEngine Provide、写入只在 Start 之后，所以第一次写之前已绑定；wiring 装配下未绑定拒绝写）。
- **没有映射时**（维护者 2026-10-20 选 A，保持现状）：物理名就是逻辑名本身（启动日志告警）。理由：现有 `DefaultDatabase` 回退只针对空库名（引擎侧兜底到 DataEngine 自己的库，生成 DAO 走不到），框架对“没有部署信息”的取舍是给出确定、可预期的名字而不是拒绝；identity 与引入映射前的行为一致（已有工程 `db=<工程名>` 的数据位置不变），测试夹具与工具不配映射照常工作；新工程总有生成配置里的映射，不会落到 `main`。代价：手写配置删掉映射会落到名为 `main` 的库（只有告警），记入 KNOWN-LIMITS。未采用“声明的逻辑库必须映射，否则拒绝启动”：所有测试、夹具、工具都要配映射，且对已有工程是一次破坏性升级。
- **一次校验**（失败拒绝启动并点名）：配置里未声明的逻辑名；物理名为空、含 Mongo 禁止字符、是系统库、按 sid 分库时加 `_<sid>` 超过 63 字节（按本进程 sid 计算）；两个逻辑库映射到同一物理库（大小写不同也算）；回调与配置对同一逻辑库给出不同的物理名（只在 Init）；回调错误。**不提供“允许共用物理库”的开关**：两个逻辑库里同名集合的文档会混在一起，改其中一个映射时也无法单独搬走；现在没有需要它的场景，以后加开关是放宽、不破坏兼容，反过来则不然。二进制一个逻辑库都没声明时（工程还没有 DAO）无从判断拼写，只告警。
- **唯一解析点**：`MongoStore.physicalDatabase`（单条 / 批量投影、冷加载、墓碑）、`MongoCommitter.dataDB`（Remote 写 DAO 文档）、`kitdataengine.DAOCollection` / `Databases` / `LoadDatabaseMapFile`（业务与工具：game-demo 公会号序列、送礼收件人检查、`cmd/sidmove players`、`cmd/schemamigrate`）。控制库（`dataengine.database`、`remote_entity.mongo.database`、saga 库、lease fence 的库）是配置给出的物理名，不经映射。守卫：根包 `TestNoMongoDatabaseOpenedWithALogicalDAOName`（产品代码与模板里 `.Database(` / `.DatabaseForSid(` 的参数不得直接是 `DbName()` / `<Dao>DBName` / `db.Database*` / `Key.Database`，除非经 `.Physical(`；只看同一行）。
- **WAL**：只记逻辑名（DocumentKey 与 Remote 提交原样写入；Remote 提交的 digest 覆盖库名，投影前改写会让发布路径的重放校验失败，所以只在写 Mongo 的那一刻换算）。可行性评估结论：可行。DataEngine 启动在打开 WAL（持有 `writer.lock`）之后、构造 Projector（构造即回放）之前读 WAL 目录的 `databases.json`，用 `WAL.HasUnacknowledgedRecords`（读到第一条未确认记录即停）判断有无未投影记录；有且上次映射里任一逻辑库的换算结果变了（文件不存在按 identity 比较）就以 `ErrDatabaseMappingChanged` 拒绝启动、不改文件，否则原子写入本次映射。`walinspect` 与 `sidmove wal-drained` 的报告带上它。改映射的步骤写进 [PLAYER-MIGRATION §5](../framework/PLAYER-MIGRATION.md#5-改-dao-库名映射)（停写 → 排空 WAL → 迁移 → 改映射），SCHEMA-MIGRATION 指向它。
- **未采用的方案**：①原登记的方案 A（`db=` 省略时取 `dataengine.database` 或新键 `dataengine.business_database`）：仍是“一个工程一个业务库”，多个逻辑库无法分别部署，且把 DAO 数据和 DataEngine 系统集合的库绑在一起；②运行中按需调用回调（懒解析）：投影热路径上做不可控的业务调用，同一逻辑库在进程生命周期内可能得到不同结果；③在 Nest 提交时把逻辑名换成物理名写进 WAL：WAL 与部署绑定，改映射后旧记录无法按新部署解释，且破坏 Remote 提交的 digest；④给 Mongo 客户端整体套一层改名包装：DataEngine / Remote / saga 自己的控制库也会被误映射；⑤允许共用物理库的开关：见上。
- **验证**：先红后绿与回归见 CHANGELOG `[Unreleased]`“DAO 逻辑库名”；真实依赖：`TestSagaNativeStepsAndEffectRealDependencies`（逻辑库 main 经 `dataengine.databases` 映射到唯一库，核对文档不在名为 main 的库）与游戏夹具（经映射表写唯一库）。

**方案 B 实施结果（2026-10-16）**：`roost add dao`（含 `--entity`）骨架写 `db=<project>`：`daoDatabaseName` 取 `project.name`，系统库 `admin` / `local` / `config` 加 `_data`，超过 52 字节截断并去掉末尾 `_`（Mongo 库名 63 字节上限，留出 `dbscope=sid` 的 `_<sid>`），骨架注释写明库名与 `_id` 换算；game-demo 的 Player / Guild / World DAO 改用同一规则（工程 planet → `planet` 库），demo 文档同步。`roost id entity` 双向换算完整 EntityID 与唯一号 / kind / category / remote（`--entity` 读工程 `//roost:entity` 标记）。已有工程 DAO 定义归业务、不改写，旧库数据不迁移（未上线不做兼容，升级说明见 CHANGELOG）。

登记：2026-10-16 新用户试用实测（与 2026-10-05 演练预演的发现相同，第二次出现）。

**登记时现状**：`roost add dao` 骨架写死 `//roost:dao coll=player db=game`，库名是编译期常量 `PlayerDaoDBName`；服务配置的 `dataengine.database` 只决定 Data Engine 自己的集合（receipts / transactions / outbox）。同一 Mongo 上的两个工程即使改了 `dataengine.database`，业务集合仍都写进 `game.player`。（引擎侧 `DocumentKey.Database` 为空时已回退到 `DefaultDatabase`，见 `framework/dataengine/engine/mongo_store.go` 的 `collection`；但 DAO 解析要求写 `db=`（`codegen/internal/dao/parse.go`），生成的 DAO 走不到这条回退。2026-10-16 审查补注。）另外文档 `_id` 是编码了 kind 的 EntityID（玩家 9001 存成 9217029），运维按玩家 ID 查不到。

**可选方向**：A DAO 库名默认取运行配置（`db=` 省略时用 `dataengine.database` 或新键 `dataengine.business_database`），显式 `db=` 仍是常量；代价是生成 DAO 需要运行期注入库名，迁移已有数据。B 骨架按工程名生成 `db=<project>`，并在文档写明 `_id` 编码与换算命令（`roost id` 加一个换算子命令）；代价小，但多工程仍靠约定隔离。

**建议**：先做 B（低成本、立即消除误用），A 另行评估。需要拍板：库名是否应当是部署配置而不是代码常量。

## D11 按实体的持久定时器接入

**状态：维护者 2026-10-17 决定方案 B（`roost add timer --entity X` 生成器），并定了离线语义：框架不做追帧；离线期间到期的定时器在实体 load 起来后只触发一次，追帧（补算错过的周期等）由业务在回调里根据传入的参数自己处理。已实施（`a2b4f4d8` core 驱动与宿主、`5581deeb` 生成器与 game-demo World 迁入）。game-demo World 已恢复手写（维护者 2026-10-18 规则：game-demo 是手写的参考实现，不迁到生成器产物上；生成器只生成框架内容，不生成 demo），见下文“game-demo”一条。** 新用户试用第四轮登记项 3，剩余限制见 [KNOWN-LIMITS](KNOWN-LIMITS.md#按实体的持久定时器d11)。

**后续决定（维护者 2026-10-18，均按推荐）**：①卸载与已入队 Tick 的窗口接受，不扩大 Nest 契约；②不做“离线也准点”的全局驱动——需要准点的由业务发送 Nest 消息，不需要的在回调里自己追帧；③去掉 `timers` 条目后生成部分不自动回收；④多一个 Mod 带来的 3s 停机保底接受，不改 app 规则。

**实施结果**：

- **生成器**（[CODEGEN_REFERENCE“roost add timer”](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#roost-add-timer按实体的持久定时器)、[impl/12-codegen §2C](../framework/impl/12-codegen.md#2c-roost-add-timer)）：`roost.yaml timers.<entity>` 声明组件名、组件类型号、服务、节点所在 DAO。add 时在实体 DAO 定义里加节点 map 与种子两个持久字段、实体加组件字段（Go 结构体不能跨文件，这两处之后归业务）；project sync 重写生成部分：`<component>_component_gen.go`（组件、DAO 适配、驱动变量 `<Component>Driver`）、`db/def/<entity>_<component>_node_gen.go`（节点类型）、`game/handler/tick_<entity>_<component>_gen.go`（`rollback=undo durability=strict` 的 Tick handler，另有名字对齐测试）与 bootstrap 里的 `kitentitytimer.NewMod`；业务回调 `<component>_handlers.go` 只在缺失时创建。Remote 托管（`remote=managed`）与 `noPersist` 实体拒绝并说明原因；无 DAO、组件重名、重复 add 同样在改文件前拒绝。
- **驱动放在 core**（[impl/10-time §2A](../framework/impl/10-time.md#2a-按实体的持久定时器驱动)）：`framework/nest/entitytimer`（`Driver` 每种实体一个、`Host` 每个实例一个），`wiring/entitytimer` 只做 App 装配（依赖 Nest，Provide 绑 Nest 客户端、只读实体查找与 `app.BusinessClock`）。取舍：放 Nest 块下，因为它只用 Nest 的公开 API（`Client.Dispatch`、`AfterCommit`）投递和排定索引，不改 Nest 调度契约；放 core 而不是生成物，是为了索引、投递、重投、离线语义只有一份实现，各工程只生成 DAO 与 handler 的适配。索引是“已加载实例的最早期限”小堆：加载时报告存量最早期限，事务提交后（`AfterCommit`）报告新的，回滚不改；报告带单调序号，丢弃晚到的旧报告；`OnDestroy`（卸载 / 删档 / 撤销新建）后永久离开索引。只有最早期限到了才给那一个实体投递一次 Tick，投递前核对实例（已移除或被取代则丢弃，不唤醒不在内存的实体），没有报告按 10s 重投。快池上的更新只持一把短锁、不做 I/O。
- **离线语义**：加载时记下 `loadedAt`；期限早于它的节点在加载后由驱动投递一次 Tick，只触发一次（Scheduler 对过期节点本就只触发一次、按返回值从 now 续期，框架不计算错过次数、不循环补触发）。回调参数 `entitytimer.Fire`：`Deadline`（原定期限）、`Now`（实际触发时刻）、`LoadedAt`、`Overdue`（加载后补触发）、`Interval`（周期）；返回正值为下次间隔（从 `Now` 起算），与 Scheduler 一致。写进了生成物注释、[guide/10-time §4](../framework/guide/10-time.md#4-按实体的持久定时器) 与新手手册。
- **持久与恢复**：节点只在 DAO 里，走 DataEngine 正式链路；种子随 upsert 推进并持久，重建不复用 ID（依赖 `4e0d257c` 的“每个分配过的 ID 至少一次 upsert”）。
- **game-demo**：`5581deeb` 曾把 World 迁到同一生成器（`timers.world`）。**维护者 2026-10-18 决定撤回：game-demo 是手写的参考实现，不迁到生成器产物上；生成器只生成框架内容，不能生成 demo。** World 现为手写：`game/entities/world/timer_component.go` 手写组件（组件类型号 2006 不变）、`TimerNode` 节点类型与 `WorldDao.Timers` / `TimerSeed` 的 Store 适配、`TimerDriver` 与活动阶段回调，底下直接用 core 的 `entitytimer.Host`；`game/handler/tick_world_timers.go` 手写 Tick handler（`handlerTickWorldTimers`，名字与驱动的对齐由 demo 的 `tick_world_timers_test.go` 守）；驱动由 game 服务 `activity.go` 的 `startWorldTimers` 在 `Service.Init` 加载 World 之前手工 `SetClock`（业务时钟）/ `Bind`（Nest 客户端、只读实体查找）/ `Start`，`Shutdown` 里停止（App 先停 Service 再停 Mod，Nest 仍在）。取舍：不恢复 D11 之前的每秒 `Sync_TickWorldTimers` 循环与 nopersist `TimerNextDue`，因为驱动是框架能力，demo 手写接入框架能力是允许的，也正好展示不用生成器时怎样接；代价是 demo 与生成物各有一份组件适配（demo 模板 257 行，其中非注释代码约 120 行；形状相同，由各自的测试守）。清单里不再有 `timers.world`、bootstrap 不装 `wiring/entitytimer` Mod，game 服务少一个 Mod、`shutdown.total_timeout` 少 3s（缺省 Mod 集的 game-demo 由 D11 时的 26 个 / 120s 回到 25 个 / 117s；测试夹具 `-mods configdata,mongo,nats,dataengine,nest` 由 25 个 / 117s 回到 24 个 / 114s——D11 记录的“114s 变为 117s”是夹具的数字）。World 的 10 个定时器用例全部保留：最早期限读驱动的公开 `TimerDriver.DueOf`，优先级用例经组件内的 host（组件只暴露游戏用到的操作）。新手工程场景（第四轮“限时 Buff 到期”）仍由生成工程端到端用例覆盖，并写进生成工程新手手册 §3.6。
- **测试**：core（`driver_test.go` / `host_test.go` / `nest_test.go`，`-race`）与生成器（`add_timer_test.go`，含 `TestEntityTimerGeneratedProjectEndToEnd`：生成工程经真实 Nest 引擎、wiring/entitytimer 与生成的 Tick handler，可控业务时钟、显式 `DeliverDue`，覆盖在线到期一次、周期续期、离线错过十个周期加载后只触发一次且参数正确、卸载后不投递不加载、重载后 ID 不复用、多定时器最早期限、删档不触发）。修前红：`a56d5965` 上 `roost add timer --entity Player` 报 `invalid timer name ""`（`TestAddTimerGeneratesOwnedPartsAndBusinessCallbacks` 同样失败）。变异核对（各自让对应用例变红）：提交前立即报告 → `TestHostReportsThroughNestTransactions`（`after a rolled-back arm: nodes 0, pending 1`）；接受旧报告 → `TestDriverDropsStaleReports`；不核对实例 → `TestDriverValidatesTheOwnerBeforeDelivering`（`delivered 3, want only the owner that is still loading`）；关闭后仍接受报告 → `TestDriverNeverDeliversToAClosedOwner`；`Overdue` 恒为假 → `TestHostFiresAnOverdueTimerOnceAfterLoad`；加载时不报告 → 生成工程端到端（`after load the index holds 0001-01-01 00:00:00 +0000 UTC false, want the missed deadline`）。

以下为决定前的原始登记。

**现状（基线 `300d728a`，已核对源码）**：

- `infra/base/timer.Scheduler`（`scheduler.go`）只是一个不加锁的最小堆：按期限、优先级、登记顺序三键排序，typed 节点的增删改经 `ChangeFunc` 交给宿主写存储，闭包节点纯内存；重建（`NewScheduler(owner, seed, saved, onChange)`）、谁来调 `Tick`、在什么锁 / 事务里调，全部由宿主决定（[guide/10-time §3](../framework/guide/10-time.md#3-scheduler)）。进行中的 Scheduler 重构（[REFACTOR-2026-10-17](REFACTOR-2026-10-17-timer-scheduler.md)）不改变“宿主驱动”这个前提。
- 唯一的现成接入是 game-demo World 的 `TimerComponent`（`demo/game/entities/world/timer_component.go.tmpl`）：期限存 `WorldDao.Timers`（`TimerNode`）与 `TimerSeed`，最早期限存 nopersist 字段 `timer_next_due`；每次调用从 DAO 现建一个 Scheduler、钩子写回 DAO、用完丢弃，回滚就是 DAO 的回滚；handler 只记 effect、不做 I/O。驱动是服务进程里的 ticker 每秒发一次 `Sync_TickWorldTimers`（`demo/internal/service/game/activity.go.tmpl` 的 `tickWorld`）——只有一个 World，所以“每秒给它发一条”就够。它与活动阶段 handler 写在一起，生成器没有对应命令。
- `Delay_X` 不持久（[NEST_RUNTIME “延时 Sender 不持久”](../../codegen/docs/NEST_RUNTIME.zh-CN.md)）：停机丢弃、崩溃丢失，不能承载“必须发生”的到期动作。
- 结果：按玩家的到期动作（Buff 到期推送、建造完成、行军到达）要把组件、节点 DAO、tick handler、驱动整套复制到 Player，并自己解决“上万在线玩家怎么驱动”；新手多半退回“存到期时刻、读取时判断”，到点服务器什么也不做。

**可选方向**：

- **A 文档化参考实现**：把 World `TimerComponent` 明确标为参考实现，在 NEST_RUNTIME / guide/10-time 写“给 Player 加持久定时器”的步骤（节点 DAO 字段、组件的重建 / 持久化 / settle、tick handler、驱动），以及“到期时实体不在内存怎么办”（加载时在 `OnInitFinish` 之后补触发已过期节点）。代价：最小，只写文档；但每个工程仍复制约 250 行，Player 的驱动（按最早期限投递还是每秒全量）仍要自己写，容易写成每秒给每个在线玩家发一条消息。
- **B 生成器 `roost add timer --entity <Entity>`**：生成节点 DAO 字段（`Timers` / `TimerSeed` / nopersist `TimerNextDue`）、`TimerComponent` 骨架（重建、持久化钩子、`settle`、`RegisterHandler` 的位置、加载后补触发）、`Tick<Entity>Timers` handler，以及服务里的驱动：进程内按 entityID 记最早期限的小堆（组件每次 settle 后更新，实体卸载时移除），到期才给那一个实体发 Tick 消息，不按在线人数轮询。生成物归业务（与 add component 同一所有权规则），game-demo World 迁到同一骨架。代价：一条新生成命令、模板与回归测试；“最早期限索引”是进程内派生状态，离线（未加载）实体到期不会被唤醒，只在下次加载时补触发——需要“离线也准点”的事仍要别的机制。
- **C 框架提供可复用组件与驱动**：把 TimerComponent 提成框架包（经一个存储适配接口接 DAO），由 Nest 为声明了定时器的已加载实体维护最早期限、到期投递 Tick（引擎内置的到期轮）。代价最大：动到 Nest 调度（三大块之一，review 最高优先级）、Entity 生命周期与 Remote 托管实体（跨服迁移时定时器随实体走）的语义；好处是一次做好、所有业务继承，符合“统一保证放在框架一处”的原则。

**建议**：先做 B——到期规则与持久化留在业务自己的 DAO 与事务里（与 World 参考实现同一形状，不扩大 Nest 契约），生成器消除复制和驱动写错的风险；B 在 game-demo 与新手工程里稳定、确认“已加载实体按最早期限投递”的驱动形状之后，再评估是否把驱动下沉到 Nest（C）。A 可作为 B 落地前的过渡立刻写。须等进行中的 Scheduler 重构合入后再动。

**需要拍板**：1）选 A / B / C；2）离线实体的到期语义——下次加载时补触发是否可接受，还是需要“离线也准点”（那要一个按期限扫描存储的全局驱动，超出本条范围）；3）第一版驱动是否允许每秒轮询全部在线实体（实现最简单，代价是每秒 N 条 Nest 消息）。

## D12 Saga 原生实体步骤与事务性 effect 消费者的生成支持

**状态：维护者 2026-10-18 决定方向 A（框架补齐胶水，demo 作为验收），已实施**（见 CHANGELOG `[Unreleased]`“Saga 原生步骤与 effect 消费者的胶水进框架”）。**B（生成器）维护者 2026-10-18 批准开工，已实施**（见下文“B 实施结果”与 CHANGELOG `[Unreleased]`“roost add saga -native 与 roost add effect”）；C 不采用。拍板项 2：`NativeStep` 放 `framework/saga`（Nest handler 签名直接引用 `saga.NativeStep`，生成器已支持外部包参数类型）。拍板项 3（`roost add effect`）随 B 同批交付。**原生步骤单进程容量：维护者 2026-10-18 决定按 A 处理——接受现状、写进文档与生成骨架、量大按 sid 分片多进程，不做消费者并发或投影合批**（KNOWN-LIMITS“Saga 原生步骤容量（单进程）”）。

**B 实施结果（2026-10-18）**：

- **`roost add saga <name> -steps a,b,c -native a,b [-entity E] -component C`**（`codegen/internal/roost/add_saga.go`）：`-native` 与 `-steps` 一样保序解析，必须是 `-steps` 的子集、不能重复（`-native step "x" is not in -steps (…)`）；只给 `-entity` / `-component` 不给 `-native` 报错；原生步骤的锁目标复用 `add handler` 的解析（抽成 `resolveNestHandlerTarget`），跑步骤的服务要有 nest Mod；与已有 Nest handler 同名（按函数名扫描 `game/handler`）或任何目标文件已存在都在写盘前拒绝，错误输入不改工程。生成物（业务文件，只建一次）：`definition.go`（去掉 `Subscribe<Step>` / `Subscribe<Step>Compensation`，未上线不做兼容）、`steps.go`（`StepsGroup = "<service>-<name>"`、`StepDeps`、`State` + `EncodeState` / `DecodeState`、每步正向与补偿步骤体、`RegisterSteps(steps, deps)`、`StartSteps(ctx, registry, deps)`）、每个原生步骤 `game/handler/<name>_<step>.go` 与 `_compensation.go`（参数 `saga.NativeStep`，事务内 `step.Complete`）。`add saga` 的同步跑 generate，新 handler 的 Sender 随之生成，生成工程直接能编译。与评估稿的差异：`Register(steps, deps)` 改名 `RegisterSteps`（`definition.go` 已有 `Register(engine)`，同包不能重名），另加 `StartSteps` 让服务 Init 只需一行；骨架默认拒绝（`… is not implemented`），写完业务前 saga 不报告成功。
- **`roost add effect <topic> -consumer <svc>`**（`add_effect.go`）：点分主题（`saga.*` 保留），`game/effects/<topic>.go`（载荷、`Topic<Topic>`、`Emit<Topic>(key, event)`）与 `internal/service/<svc>/<topic>_consumer.go`（`start<Topic>EffectConsumer`：`kitdataengine.SubscribeEffects`，解不开 `PoisonEffect`，durable `<svc>-<topic>`，骨架默认返回错误、重投不丢）；消费服务要有 dataengine Mod；effect 文件已存在时只建新消费者（同一 effect 多个消费服务）。
- **归属与提示**：服务 Init / Shutdown 不改写；`add saga` / `add effect` 的 next 提示与 `roost help saga` / `roost help effect` 给出要加的两行（`StartSteps` / `Drain`；`start<Topic>EffectConsumer` / `Drain`）。doctor 新增只读检查（低成本、文本判据，`saga_effect_doctor.go`）：`saga-steps:<name>`（steps.go 有 `StartSteps` / `RegisterSteps` 却没有服务导入并调用 → WARN；调用了但所在包没有 `Steps.Drain(ctx)` → WARN）与 `effect-consumer:<service>/<func>`（消费者函数在服务包里没有调用 → WARN）。只证明“写了”，不证明运行时一定走到；注释里的用法不算调用。
- **容量写进骨架**（数字已按 2026-10-18 性能优化后的实测更新，见 [SAGA-OPT](../performance/SAGA-OPT-20261018.md)；下面是实施时的原文）：`steps.go` 注释、`add saga` next 提示与 `roost help saga` 写明单进程每个 durable 约 14～18 条/s、原生步骤 saga 正向约 10/s、全补偿约 5/s 开始饱和、超过后重试放大、量大按 sid 分片并加 `WithAdmit`，链接性能报告；effect 消费者骨架写明逐条处理、未单独测量。
- **game-demo**：保留手写接线（`gift_saga.go` 的按 sid 准入与转交依赖服务 Init 里才有的玩家驻留表与总线，迁移改动不可控），生成的 `saga/gift_item/steps.go` 由 demo 模板换成指向 `gift_saga.go` 的说明（doctor 不对它报 WARN）；升级邮件 `level_up_mail.go` 已是 `SubscribeEffects` 写法，未改名为生成器的函数名。
- **验证**：codegen 单测（生成物形状与登记顺序、`-native` 子集 / 重复 / 缺组件 / 服务无 nest、同名 handler、重复 add、错误输入工程逐字节不变、无 `-native` 等价 Mongo 登记、effect 生成与第二消费者、doctor 两项）、生成工程编译（骨架未填业务与接线后 build / vet / doctor OK），以及真实依赖端到端 `TestGeneratedSagaNativeStepsAndEffectConsumer`（新手工程 add saga -native + add effect → 填最小业务 → 真实 Mongo / NATS / DataEngine / Nest 上一笔正向、一笔补偿与一条 effect 消费恰好一次，挂在 `scripts/test-demo-generated.sh`）。


**实施结果**：

- `framework/saga`：`NativeStep`（只由框架构造；`Command()`、`Complete(success, reason)` = 回执与 lease fence 绑定 + completion，都在调用方的 Nest 事务里）、`NativeStepBody`、`ErrNoNativeStep`；`DataEngineStepInbox.StepHandler(body)` 把步骤体接到 `SubscribeDataEngineStep`；`DataEngineStepInbox.RunHandedOver(ctx, command, body)` 是转交接收方（自己 Reserve，与消费者互斥，不等投影）。契约（幂等键、结果未知、补偿语义）写在 `native_step.go` 文件注释与 [guide/06-saga §2.1](../framework/guide/06-saga.md#21-业务怎样写步骤步骤运行时design-todo-d12-方向-a)。
- `wiring/saga`：`Steps`（`NewSteps(registry, group)`、`Native` / `Mongo(name, topic, …, WithAdmit)`、`Start`、`Drain(ctx)` / `Stop`、`RunHandedOver(ctx, name, command, accept)`）：按 saga Mod 与 DataEngine Mod 的声明组装发布器、两种收件箱（原生租约 2 分钟）、每步一个 durable（`<group>-<name>`），指标 `saga.step.run_total` / `run_duration` / `admit_refused_total`。没有新配置键。不是 App Mod：步骤体要用服务 Init 里才发布的能力。
- `wiring/dataengine`：`SubscribeEffects(ctx, registry, EffectConsumer{Durable, Topic}, handler)` 与 `PoisonEffect`：按 `dataengine.effects.*` / `dataengine.database` 组装 Mongo effect inbox 与 durable；至少一次投递、按 EffectID 至多一次生效，普通错误退避重投，毒消息 Term（`dataengine.effect_consumer.handled_total{result="poison"}`），回执 TTL 不长于效果流保留期时拒绝订阅。
- 按 sid 准入与转交（约 300 行）留在 demo：框架提供 `WithAdmit`（取执行权之前问归属）与 `RunHandedOver`（接收方先核对步骤名与命令主题，再由 `accept` 复核归属，然后取执行权）两个扩展点。
- demo：`gift_saga.go` 660 → 526 行（`startGiftSaga` 123 → 49 行，其中框架组装约 12 行：`NewSteps`、四行登记、`Start`；其余是业务依赖查询与转交接线），`gift.NativeStep` 删除（`gift.go` 151 → 100 行），handler 参数改为 `saga.NativeStep`；`level_up_mail.go` 107 → 87 行，改用 `SubscribeEffects`。活动阶段消费者（`activity.go`）本轮未改（与并行的活动改动避免冲突），仍用 `effectSettings` + `nestwal.SubscribeJetStreamEffects`。
- 顺带：saga 的完成回执保留期检查（`checkEffectRetention`）从 Init 搬进 `CheckConfigAcrossMods`，`--check-config` 与 doctor `config-check` 也报。

**验证结果（2026-10-18，A 的四个未验证项）**：

1. **原生步骤在真实 Nest 调度下**：已验证。`wiring/saga` 的 `TestRealStepsRunNativeStepsUnderTheRealNestScheduler`（`-tags integration`，随 `dataengine-env.sh test` 的 `./wiring/saga` 运行）：正式 Mongo / NATS（三副本流）/ DataEngine / Nest（`wiring/nest` Mod，真实 short / io 池与冷加载）/ saga Mod，手写余额实体经 DataEngine 仓储从 Mongo 冷加载，步骤体经 Nest Client 请求 handler，在事务里 `step.Complete`。场景与结论：正向完成；业务拒绝只提交回执与失败 completion、收款方版本不变、协调器补偿；执行中再投一份同一命令（不带 MsgID 绕过流去重）与完成后再投一份都只回放、handler 只执行一次；Nest 提交后丢回复（结果未知）重投等到回执 ack、不再执行；调用 Nest 前失败的尝试等到截止、协调器第二次尝试执行；投影器挡在 checkpoint 时另一个 durable 撞上实体屏障（`ErrFencedEntityPending`）交还租约、在**第一次尝试**内生效；投影停到命令截止之后整笔跳过、实体驱逐、第二次尝试在重载的实体上生效。余额（Mongo 与内存）恰好一次。断言有效性：去掉消费者在 `ErrFencedEntityPending` 时交还租约，用例报生效尝试 [3]、屏障解除后 10.4s 才完成。验证中看到的性质：一个 durable 在一个进程里逐条处理，屏障场景只能由**另一个 durable**（或另一个进程）触发；投影停顿时在等回执的命令挡住同 durable 后续命令直到截止（见项 3）。
2. **跨进程赠礼演练**：生成 game-demo（`--skip-deps` + replace 到本分支），account / chat / mail / match 等服务 + 两个 game（sid 1000、1001），真实 Mongo / NATS（共享隔离环境，工程名 `d12vdrill` 派生的库与流，用后删除）、私有 Redis 与 etcd；演练驱动 `cmd/giftdrill`（源码与日志存 [benchmarks/saga-d12-20261018/drill](../performance/benchmarks/saga-d12-20261018/drill/)）建 p1、p4@1000 与 p2、p3@1001，双向赠礼。**第一轮发现缺陷**：转交（`RunHandedOver`）与消费者在同一命令 ID 上互相 `saga: idempotency identity conflict`——命令经 bus（MessagePack）转交回来，时间字段换成本地时区，而命令摘要是 JSON 文本的 sha256；转交方先认领时，持有原消息的消费者读不到自己的回执，nak 到 MaxDeliver（第一轮结束 5 分钟后 `game-gift-debit` 11 条、`debit-undo` 2 条仍在重投，delivered 123 → 200，ack floor 不动）。业务值正确，坏在投递收不了尾，只在主机时区不是 UTC 时出现。已按先红后绿修复（摘要统一 UTC，`BindCommand` 共用同一摘要；见 CHANGELOG）。修后重跑三个阶段：
   - 正常：p1→p2、p3→p4 各 10 笔 completed，送给从未进游戏的玩家 1 各 2 笔 compensated；debit 落到错的 sid 时准入拒绝并转交（两边各 7 次），转交执行成功 2 / 4 次，其余转交撞上同一发送方的实体屏障（`ErrFencedEntityPending`，交还租约，由重投收尾）；收礼方各领 10 封，背包 p1 89 / p3 89、p2 10 / p4 10，恰好一次；四个步骤 durable 全部 ack 完。
   - 正常停机：双向各 30 笔连发后 2s 对 game2（sid 1001）SIGTERM，此时 debit durable 有 34 条在途；`Drain` 期间处理掉缓冲里的投递（14 条转交给 sid 1000、执行了 10 条转交过来的），服务停机约 2.1s，进程退出 2.69s。game2 下线期间 p3 的 debit 只能等它（别的进程准入拒绝、转交失败、按退避重投，debit durable 投递数 146 → 475）；重启 1s 就绪后全部完成（最长 28.7s）。60 笔全部 completed，领信 30 / 30，背包 59 / 59 / 40 / 40。
   - kill -9：双向各 20 笔 + 补偿各 2 笔连发后对 game（sid 1000）kill -9（debit 在途 13～26 条）；重启时等单实例锁过期（kill 后约 16s 取到锁），从 kill 到就绪 18s；44 笔全部到终态（40 completed、4 compensated，最长 20.5s），领信 20 / 20，背包 39 / 39 / 60 / 60，恰好一次，durable 全部 ack 完。
3. **saga 性能同机前后对照**：见 [性能报告](../performance/SAGA-D12-20261018.md)。`scripts/perf/saga.sh` / `TestRealSagaPerformance` 只走 framework/saga 的 Mongo 步骤入口，D12 没改它（代码相同），对照只能证明旧路径不受影响：正向 20/s、全补偿 10/s 两版本都通过（新版本一个样本因 Mongo 主节点超时失败，补跑通过，原因尚未确认）。另做经过新 API 的负载（`TestRealNativeStepsPerformance`，真实 Nest + DataEngine，基线用 D12 之前的手写组装）：正向 5/s、全补偿 3/s 各两轮，手写与 `Steps` 无可测差异；**正向 20/s 与全补偿 10/s 在原生路径上两种写法都饱和崩溃**，正向 10/s、全补偿 5/s 已崩——单进程原生步骤容量是新发现（不是 D12 引入），登记 KNOWN-LIMITS。
4. **已存在 durable 的参数变更**：已验证（nats-server 2.14.5）。`Subscribe` 用 `CreateOrUpdateConsumer`，D12 改的 `game-level-up-mail` MaxDeliver 20 → 25000、MaxAckPending（服务器缺省 1000）→ 256 原地生效、投递进度保留，不会静默沿用旧参数；DeliverPolicy 等不可变字段不同则服务器拒绝（10012 `deliver policy can not be updated`），原报错不说是哪个流 / durable。已修：建立失败都带上流与 durable，不可变冲突时点名“停掉使用它的进程、`nats consumer rm <流> <durable>` 后再启动，投递进度丢失”（`infra/network/nats/driver/real_nats_integration_test.go` 的 `TestRealJetStreamSubscribeUpdatesAnExistingDurableInPlace`）。收件箱集合改名的升级说明维持 CHANGELOG 原文。

**B 评估（2026-10-18，基于上面的验证；当时建议再等等，同日维护者批准开工并已实施，见上文“B 实施结果”）**：

- **A 的 API 是否稳定到可以写生成器**：验证没有发现需要改形状的 API。真实 Nest、两进程转交、正常停机与 kill -9 下 `NativeStep.Complete`、`Steps.Native` / `Mongo` / `WithAdmit` / `Start` / `Drain` / `RunHandedOver`、`SubscribeEffects` / `PoisonEffect` 的契约都按文档成立；发现的缺陷（命令摘要的时区）修在 framework 内部，不改签名。可能**新增**而不改已有签名的地方：原生步骤容量若要提高，最可能的落点是步骤消费者的并发度（一个 durable 在一个进程内并行处理多条，`Steps` 多一个步骤选项）或投影器对带回执 / effect 记录的合批；二者都不要求迁移生成物，但会改变生成骨架里该写的容量说明。
- **生成物与归属**（沿用 `add rpc` / `add timer` 的“业务文件只建一次”，这里没有需要重生成的 `*_gen.go`）：
  - `roost add saga <Name> -service <svc> -steps a,b,c -native a,b`：`saga/<name>/definition.go`（已有，业务文件；删掉现在的 `Subscribe<Step>` Mongo 辅助函数——它们早于 `Steps`，A 之后是第二套写法）；新增 `saga/<name>/steps.go`（业务文件）：每步一个步骤体（原生步骤体解载荷后调生成的 sender，Mongo 步骤体留 TODO 与“事务外副作用按 IdempotencyKey 幂等”的说明）与 `Register(steps *kitsaga.Steps, deps)`（按定义里的主题常量登记，原生 `Native`、其余 `Mongo`，`WithAdmit` 空着并注释多进程分片要补）；原生步骤的 Nest handler 骨架 `game/handler/<name>_<step>.go` 与补偿各一个（业务文件，参数带 `saga.NativeStep`，注释写 `step.Complete` 与业务拒绝也提交），复用 `add handler` 的实体 / 组件解析（`-entity` / `-component`）。
  - 服务 Init / Shutdown 归业务、不改写：像 `add skill` 一样在 next 提示里给两行（Init：`NewSteps` + `Register` + `Start`；Shutdown：`Drain(ctx)`）。
  - `roost add effect <topic> -consumer <svc>`：`game/effects/<topic>.go`（载荷类型、topic 常量、`Emit<Topic>` 辅助，业务文件）与 `internal/service/<svc>/<topic>_consumer.go`（`SubscribeEffects` 骨架，载荷解不开返回 `PoisonEffect`，事务外调用按 EffectID 幂等，业务文件）；同样只给 Init / Shutdown 提示。拍板项 3 建议同批交付：形状相同，effect 部分更小。
- **与现有 `roost add saga` 合并**：保留第六轮修的 `-steps` 保序与重名检查；`-native` 必须是 `-steps` 的子集、同样保序解析；不给 `-native` 时只生成 Mongo 登记，与今天等价（少了 `Subscribe*` 辅助）；`Register` 的登记顺序跟 `-steps`，与 `Definition.Steps` 一致。
- **工作量**：生成器约 300～400 行（两个命令、`-native` 解析、handler 骨架复用 `add handler`），codegen 单测（文件清单、保序、子集校验、生成后格式化与编译）、生成工程端到端（`project new` → `add saga -native` / `add effect` → build / vet / test / doctor），help / CODEGEN_REFERENCE / guide 06；约 2 天。
- **风险**：中低，主要不在 API 而在**默认引导**：生成器把“改实体的步骤走原生路径”变成一条命令，而单进程原生步骤持续吞吐只有个位数 saga/s（正向两步 5/s 通过、10/s 崩；全补偿 3/s 通过、5/s 崩），过载时重试放大、终态变 failed / compensated。骨架注释与 help 必须写明容量与分片 / 准入要业务补，否则按生成物上线会在不高的量上踩到。
- **推荐：再等等**，等维护者对“原生步骤单进程容量”的决定（KNOWN-LIMITS 同名条目）：接受现状并写进文档，则 B 可以立即开工；决定先做消费者并发或投影合批，则 B 放在它之后（`Steps` 可能多一个选项，生成骨架的容量说明随之改，避免生成物与 help 改两轮）。effect 部分与容量无关，维护者愿意的话可以先单独做。

**原登记（2026-10-18，决定前）**：

**现状（已核对当前源码）**：

- `roost add saga` 生成 `saga/<name>/definition.go`：定义、`Register`、`EmitStart`、步骤 topic 常量，以及每一步的 `Subscribe<Step>` / `Subscribe<Step>Compensation`，**只走 Mongo 步骤**（`saga.SubscribeMongoStep`）。注释点名“业务是 Nest 事务的步骤用 `saga.SubscribeDataEngineStep`”，但不生成。
- 原生步骤要业务自己拼的东西（框架 API 都在，缺的是组装）：从 Registry 取 JetStream / Mongo / Nest 客户端；按 saga Mod 的声明取流与前缀（`kitsaga.StreamSettings`）和 DataEngine 库名（`kitdataengine.EffectSettings`）；建 `saga.NewJetStreamPublisher`、`saga.NewDataEngineStepInbox`（Owner、LeaseDuration 要长过 AckWait）；每个 topic 一个 durable 调 `SubscribeDataEngineStep`；StepHandler 从 ctx 取 `saga.ReservationFromContext`，把 Command + Reservation 带进 Nest handler，在事务里 `inbox.Bind` 再 `saga.EmitCompletion`（demo 的 `gift.NativeStep.Complete`，约 50 行，内容与业务无关）。
- game-demo 的送礼接线 `internal/service/game/gift_saga.go` 共 661 行（其中注释约 280 行）：组装与订阅循环约 125 行，**按 sid 静态绑定玩家带来的准入与转交**（`admitOwned`、`ownerroute` 转交、`runHandoff`）约 300 行，四个步骤体约 140 行。转交那部分是 demo 的玩家分片拓扑决定的，生成器不该替业务猜；组装、`NativeStep` 与订阅循环是每个工程都一样的胶水。
- 事务性 outbox（`nest.Emit` 的 effect + `nestwal.NewMongoEffectInbox` + `nestwal.SubscribeJetStreamEffects`）只有 demo 的升级发邮件（`level_up_mail.go`，107 行）一个样例，生成器没有对应命令；effect 的类型与 topic 常量在 `game/effects` 里手写。
- 结果：跨服务副作用的两条正路都要照 demo 大段手写；更省事的写法是“事务后尽力调用 + 幂等 ID”，崩溃时少一笔（demo 的排行提交就这样写，并在注释里说明了代价）。

**可选方向**：

- **A 框架补齐胶水（不改生成器）**：把 `NativeStep`（Bind + EmitCompletion）提到 `framework/saga`（例如 `saga.NativeStep` 由 StepHandler 从 ctx 构造、Nest handler 里 `step.Complete(ok, reason)`）；`wiring/saga` 提供按 Mod 声明组装的步骤运行时（例如 `kitsaga.Steps(registry, owner)` 返回已建好的 publisher / 两种 inbox / 流设置，`SubscribeNative(topic, handler, admit)`、`SubscribeMongo(...)`，`Drain` 统一停止）；`wiring/dataengine` 提供 `SubscribeEffects(registry, topic, durable, handler)`（内含 Mongo effect inbox 与流设置）。demo 改用它们作为验收。代价：几个公开 API 与对应测试；业务仍手写 Nest handler、订阅调用与（如有）准入 / 转交；生成物不变。预计 demo 组装部分从约 125 行降到约 20 行，`gift.NativeStep` 删除。
- **B 在 A 之上加生成器**：`roost add saga ... -native debit,refund`（或按步骤标注）为原生步骤生成 `Subscribe<Step>Native` 与带 `saga.NativeStep` 参数的 Nest handler 骨架（业务文件，只建一次）及服务 Init 里的订阅接线；`roost add effect <topic> --consumer <service>` 生成 effect 载荷类型与 topic 常量、`Emit` 辅助与消费者骨架。代价：新的 CLI 形状、模板、fixture 与生成工程端到端测试要长期维护；多进程分片时的准入（`Admit`）仍要业务补，骨架只能留明确的 TODO 与说明；依赖 A 的 API，A 不先稳定就会生成一批随后要迁移的代码。
- **C 只写文档**：在 guide/06-saga 与生成工程文档里加“跨服务副作用的两条正路”手册，逐段对照 demo 文件。代价最低，不减少手写量，demo 改动时文档容易过时。

**建议**：先做 **A**（胶水进框架、demo 作为第一个调用方验证），A 落地后再决定是否做 B。理由：缺的主要是与业务无关的组装和 `NativeStep`，放进框架后手写量降到“handler + 一次订阅调用”，生成器即使以后要做也只是薄薄一层；直接做 B 会把今天 demo 的胶水形状固化进模板。准入 / 转交属于玩家分片拓扑，A、B 都不替业务决定。

**需要拍板**：1）选 A / B / C（或 A 后接 B）；2）若做 A，`NativeStep` 放 `framework/saga` 还是生成到业务包（影响 Nest handler 签名能否引用框架类型）；3）若做 B，effect 命令是否与 saga 命令同批交付。

## D13 DAO 持久键的来源与水合时的键集合校验

**状态：维护者 2026-10-19 决定，已实施。** 决定（维护者原话要点，经三次更正）：

1. **DAO 定义里可以写显式持久名**：字段的 `bson:"<键>"` 生效，作为持久键；没写时仍是 snake_case(Go 字段名)。显式名还写在**生成的工具 struct** 上——以前生成器另生成 marshal / unmarshal 用的结构体，现在每个 DAO 生成持久文档结构体，读写、回滚、补丁都以它为键的来源。
2. **改字段（改名、删字段）打印 WARN，不拒绝加载**：生成期与加载期各一处。
3. ~~方案 A：整文档写回时保留库里不认识的持久键，不删除；提供显式清理入口~~——**同日撤销**（维护者 2026-10-19：“旧字段就不 load 了，给一个业务层的约束或者通知就可以了”）：库里有、DAO 未声明的键不读入、不在内存里携带，整文档写回自然去掉它们（与 D13 之前的写入语义一致）；`dataengine.UnknownStoredKeys` 携带机制与 `engine.CleanupStoredKeys` 清理入口删除（322a7c40 实现过、未发版）。业务层约束写进 SCHEMA-MIGRATION §1：改名 / 删字段前要么保留原 `bson` 名，要么先离线迁移再提升 schema。

**实施结果**：

- **键规则**（`codegen/internal/dao/parse.go` 的 `applyBSONTag` / `validateFieldKeys`）：`FieldDef.Key` 是唯一的取键方式（持久键、同步 WireName、回滚快照键是同一个）。显式名须字母开头、只含字母数字下划线；任何键不得以 `_` 开头（留给 `_id`、`_schema`、`_version`、`_last_tx`、`_deleted` 等元数据）；同一 DAO / 嵌套类型内所有字段的键不得重复（持久、只同步、只事务字段共用回滚快照与同步文档）；`bson` 选项只认 `omitempty`（照抄进生成的标签），其余选项与 `bson:"-"` 生成期报错。9baf02a1 的“bson 被忽略”告警已删除。
- **持久文档结构体**（`template_dao.go`）：`<dao>PersistDoc`（`ID` `bson:"_id"`、`SchemaVersion` `bson:"_schema"`、每个持久字段一行显式 `bson` 标签）。`marshalCommitState` / `MarshalPersist` / `Unmarshal` / `RestorePersisted` 都经 `persistDoc()` / `setPersistDoc()`；回滚快照 `<dao>RollbackDoc` 把它作为 `_persisted` 子文档、再加非持久字段；增量补丁、`PersistFieldNames`、`SyncFields`、`MarshalSync` / `ApplySync` 的键字面量都取同一个 `FieldDef.Key`（`TestEveryKeyTheGeneratedDaoSpellsIsATagOfItsDocumentStructs` 守住：生成物里每个键字面量都是文档结构体的某个标签）。整文档写入从 `bson.M` 改为结构体编码，顶层键按声明顺序写出。嵌套类型沿用 `<type>BSONDoc`。回滚快照保留 `<dao>RollbackDoc` 的形状（持久文档作 `_persisted` 子文档）：它让快照与持久写入共用同一份键来源，撤销方案 A 后仍有价值。
- **生成期 WARN**（`codegen/internal/dao/baseline.go`）：生成前读输出目录里上一次的生成物（`<dao>PersistDoc`；更早的生成物里是 `Unmarshal` 的局部 `rawDoc`；嵌套类型是 `<type>BSONDoc`）作为基线，持久键消失、新增、类型变化各打一条 WARN，写明后果、迁移手册与 `schema=` 是否提高；不阻断生成。保留原 `bson` 名改 Go 名时键不变，不告警。
- **加载期 WARN**（`framework/dataengine/stored_keys.go`）：`dataengine.NewStoredKeySet[<dao>PersistDoc]` 从结构体标签推出键集合（嵌套按解码层级，map 值 / 切片元素路径写作 `*`）；生成的 `RestorePersisted` 在 schema 校验之后、解码之前调用 `WarnUnknown`：同一进程同一 DAO 同一键只打一次 `slog.Warn`（带 `dao`、`key`、`first_id`），每次出现计数 `dataengine.dao.unknown_stored_key.total{dao}`，照常加载但不读入该键（WARN 写明“该键不再读取，下一次整文档写回会删除它；要保留数据请给字段写原 bson 名，或先离线迁移”）。顶层 `_` 开头的元数据键与缺失键不报。Remote 托管实体经同一个 `RestorePersisted`（payload 是信封的 data）。
- **旧键不读入（撤销方案 A 后）**：持久文档结构体与嵌套 wire 文档不再有收未知键的 inline 字段，解码器忽略库里多出的键；整文档写回（`$replaceWith` / ReplaceOne、Remote data 信封替换）就把它们去掉，单字段补丁只 `$set` 自己的键、旧键留在库里但不再被读取（需要清掉时用一次性离线迁移）。存储写法、版本条件、冲突分类、墓碑语义都没有改。回归：生成 DAO 运行时 `TestUnknownStoredKeysWarnOncePerKeyAndAreNotLoaded`，真实 Mongo `TestOldStoredKeysAreNotLoadedAndTheNextFullWriteDropsThemRealMongo` / `TestRemoteEnvelopePayloadDropsOldStoredKeysRealMongo`（对 322a7c40 的方案 A 实现红：`after a full write the old keys [old_gold pos.old_z equips.1.old_color] are still stored, want them dropped`、`remote payload carries old keys [...]`）。
- **剩余风险**：WARN 不阻断；忽略告警时旧值不再加载，下一次整文档写回即丢失——这由业务约束（保留原 `bson` 名或先离线迁移）承担，框架只通知。生成期基线只在工作树里，删掉生成物或换机器重新生成看不到上一次的形状，此时只剩加载期 WARN。见 [KNOWN-LIMITS](KNOWN-LIMITS.md#dao-持久键d13)。

**原登记（2026-10-19）**：

**问题（实测）**：把 ``Gold int64 `bson:"gold" dao:"persist,sync"` `` 的 Go 名改成 `Coins`、保留 `bson:"gold"` 想让库里的键不变，`roost generate`、`go build`、`roost project doctor` 全部通过、启动无告警，登录后金币 40 → 0（Mongo 里 `gold: 40` 还在）；之后的写入以 `coins` 继续。数据安全问题：没有任何一层报错。

**现状（已核对当前源码）**：

- **持久键 = Go 字段名的 snake_case**：`codegen/internal/dao/gen.go` 的 `bsonKey`（模板函数，`toSnake(name)`）是唯一来源，水合（`Unmarshal` 的 `rawDoc` 标签）、整文档写入（`Marshal`）、增量写入（`$set` 路径、`MapPatchPath`）、Sync 名都用它。
- **`bson` 标签不参与**：`parse.go` 的 `parseDaoTag` 只读 `dao:"..."`；`roost add dao` 骨架注释、FIRST_BUSINESS / CODEGEN_REFERENCE 示例与 `roost add timer` 生成的字段都写了 `bson:"..."`（恰好与 snake_case 相同，所以看起来生效）。
- **水合不看键集合**：生成的 `RestorePersisted` 先比 `_schema` 整数，再 `bson.Unmarshal` 进 `rawDoc` 结构体——库里有、定义里没有的键被解码器静默丢弃；定义里有、库里没有的键得零值（这正是“加字段不需要迁移”依赖的行为）。
- **写入**：创建 / `MarkPersistFull` 是 `ReplaceOne` 整文档（旧键随之删除，老值彻底丢失）；增量是 `$set`（旧键残留但再也不读）。
- **D4 为什么没拦住**：D4 的校验只比业务手写的 `schema=` 整数（`checkAggregateSchemas` 与生成的 `RestorePersisted`），不比字段集合；生成器没有持久字段的基线；未知键被忽略是“加字段兼容”的副作用。同样静默的还有：删字段、`persist` 改 `nopersist`、`dao:"-"`。

**可选方向**（未上线，均不做旧数据兼容：版本号加 1、CHANGELOG 写“升级需清空”）：

- **A 显式持久名**：生成器把字段的显式名当持久键，缺省仍是 snake_case。写法二选一：尊重已有的 `bson:"name"`（用户与文档已经在写，最少意外），或新增 `dao:"persist,key=name"`（与 bson 库的语义分开，`bson` 标签继续告警或拒绝）。效果：改 Go 名、保留显式名 → 键不变，不需要迁移。代价：模板改用 `FieldDef` 的持久键（约 20 处 `bsonKey` 调用）、golden 与 doctor 文本更新；显式名本身成为存储契约（改它 = 改键）；**挡不住**“改 Go 名且没写显式名”。已生成工程：没写或写得与 snake_case 相同的字段生成物不变，写了不同名字的字段键会变。
- **B 水合时校验键集合**：文档里出现定义没有的持久键（排除 `_id`、`_version`、`_schema`、`_last_tx`、`_deleted`、`_deleted_at` 等元数据）即拒绝加载，错误与 `ErrSchemaMismatch` 同类、点名多出的键并指向迁移手册；缺失的键仍允许（加字段兼容不变）。强制点与现有 schema 校验合流（生成的 `RestorePersisted` 解码前遍历 `bson.Raw` 顶层元素），“规则声明一次、运行期加载时强制一次”。代价：每次冷加载多一次顶层键扫描（不分配，微小）；删字段 / 改 `nopersist` 也要先迁移（`$unset`），把现在的静默残留变成显式运维动作；Remote 托管实体的加载路径要一并接上；嵌套子文档是否递归校验需要定（递归更全、代码与开销更大）。已生成工程：重新 generate 即得；库里已有残留键的数据启动即拒绝（未上线：清库）。
- **C 两者都做**：B 是保证（无论怎样改名都不会静默丢数据），A 让“只改 Go 名”不需要迁移。
- **D（可与上面组合）生成期基线**：提交一份持久键清单（类似 `protocol/protocol_manifest.json`），generate / doctor 比对：键消失或改名而 `schema=` 未提高时报错。代价：多一个受控文件要随代码提交；只在生成期，挡不住已部署数据与代码不一致。

**建议：C，显式名用已有的 `bson:"..."`**（B 先做，它单独就消除静默丢失；A 随后）。理由：B 把保证放在唯一的运行期加载点，与 D4 同一处；A 用户已经在写、文档示例已经是这个形状，实现后示例变成真的。D 暂不做。

**需要拍板**：1）是否采用 B，包括“删字段 / 改 `nopersist` 也必须先迁移”；2）A 的显式名用 `bson` 标签还是 `dao:"key=..."`；3）B 是否递归校验嵌套子文档（建议先只校验顶层与嵌套类型的直接字段）。

## D14 AOI 观察者改为 playerId、视野半径按观察者配置

**状态：维护者 2026-10-20 决定 A1 + B1，已实施（2026-10-20，见下文“实施结果”）。** 登记：2026-10-20；源码基线 `4eccd3b1`。来源：维护者 review `AOI.evaluatePair` 的 `observer.id == subject` 时指出，observer 名义上是实体 ID、实际上只有玩家；视野半径也不该只有全局一个。本条只登记方案，不改代码。

### 维护者决定（2026-10-20，六项均按推荐）

1. observer 编号：**A1**，observer = playerId，观察者带 `self`（玩家自身实体 ID，0 表示没有）。A2（视野 ID）不做，将来需要多视野时在 A1 上加。
2. 入口形状：`Enter` / `Move` / `Leave` 直接改成两个 ID 参数，不保留旧入口（未上线，不做兼容）。
3. 事务内入口换算：`InterestConfig` 新增 `ObserverOf func(entityID int64) (playerID int64, ok bool)`，缺省用 `GetUniqueIDFromEntityID`，业务可覆盖（玩家控制英雄 / 载具时视点实体不是 Player）。
4. `InterestConfig.Session`：删除（A1 下 observer 就是会话 ID，多对一隐患随之消失）。
5. 半径与距离分档：**B1，LOD 层级与视野大小分开配置**（2026-10-20 维护者补充：视野变化一般来自客户端的 LOD 请求，请求带 **LOD 层级**和**视野大小**两项）。`AOIConfig` 新增 LOD 层级表，每一层是一组 `Bands`（例如 low / medium / high 各自的距离分档）；视野大小单独配置允许范围 `[MinViewRadius, MaxLeaveRadius]` 与量化步长，`EnterRadius` 取请求的视野大小、`LeaveRadius` = 视野大小 + 配置的滞回量（客户端只报一个数，进出半径由服务端推出）。启动期按 `MaxLeaveRadius` 算 `worstObserverBlocks` 预算，超过即拒绝；每层的 `Bands` 在启动期校验（递增、不超过 `MaxLeaveRadius`）。观察者记自己当前的 LOD 层级与视野大小，`resubscribe`、`evaluatePair`（进入 / 保持 / 分档）、`evaluateObserver`、`AOICluster` 镜像方框都读观察者的值。`MaxVisible` 暂保持全局。
6. 运行期切换：**客户端 AOI / LOD 请求带 `(lodLevel, viewRadius)`，直接生效**（2026-10-20 维护者补充后修订，取代“提供排队版”）。①LOD 层级只能选配置里的层；视野大小由服务端夹到允许范围并按步长量化（防止改过的客户端拉大视野多看单位、放大订阅与带宽，量化也减少细碎请求引起的重订阅）；②由接入层收到请求后调用 `Interest.SetObserverView(playerID, ObserverView{LOD, Radius})`，与移动请求同样串行处理，不排队、不走提交门（不改业务状态，没有回滚问题）；③同一玩家请求限速（缺省如每秒至多一次，可配置），超频拒绝并计数；量化后与当前值相同的请求不触发重评；④视野变大时新入范围的单位按进入下发全量，变小时超出的按离开退订；LOD 层级变化只按新 `Bands` 重新分档、重发视图，不改订阅块；两者都复用移动时的 `resubscribe` + 完整重评。排队版暂不做：将来有“Buff 改视野”的玩法时，业务在提交后发一次请求，或再加排队版。

**实施时一并补的两点（评审补充）**：①新增“只看、不被看”的观察者入口（观战 / GM 视角，`self = 0`），A1 下与 `Enter`、`Show` 并列；②属于 sync 三大块的公开契约调整，按维护手册对 `scripts/perf/sync-aoi` 与 `policy` 包 AOI 基准做前后 benchstat。实施顺序按 roost-coding 纯重构流程：先补现有行为的回归，再改签名，最后补新行为的红绿用例。

### 实施结果（2026-10-20）

按 roost-coding 纯重构流程三步提交：先在旧代码上补观察者编号回归（`TestObserverAndSubjectIDSpacesStayApart` 只对 Manager 的订阅表断言，D14 前后断言一字未改；`TestSessionManyToOneDropsASharedSubscription` 在旧代码上证实多对一隐患属实，随 `Session` 删除），再改签名与数据结构、全部调用方跟随，最后补新行为用例并逐项变异验证。提交见 CHANGELOG `[Unreleased]` 本条。

| 决定 | 落点 |
| --- | --- |
| 1 A1 | `AOI` / `AOICluster` 的观察者键是玩家 ID，`interestObserver.self` 是自身实体；自排除三处（`evaluatePair`、`evaluateObserver` 候选、`aoi_block.go` `evaluateObserved`——草案的依赖点清单漏了覆盖块这一处）都比 `self`；`self` 改变时连同覆盖可见集重评 |
| 2 入口 | `Enter(playerID, entityID, at, view)`、`Move(playerID, entityID, to)`、`Leave(playerID, entityID)`，旧入口删除 |
| 3 ObserverOf | `InterestConfig.ObserverOf`，缺省 `DefaultObserverOf`（`GetUniqueIDFromEntityID`，0 视为不是观察者）；入队时换算一次存进事实 |
| 4 Session | 删除；会话 ID = 观察者 ID |
| 5 B1 | `AOIConfig.LODs`（每层一组 Bands，与 `Bands` 二选一）、`MinViewRadius` / `MaxLeaveRadius` / `ViewRadiusStep`；滞回量取缺省视野的 `LeaveRadius − EnterRadius`；`worstObserverBlocks` 按 `MaxLeaveRadius`；每层分档启动期校验；`MaxVisible` 保持全局 |
| 6 运行期 | `Interest.SetObserverView(playerID, ObserverView{LOD, Radius})` 直接生效、不排队不走提交门；`ViewChangeInterval` 缺省 1 秒，超频 `ErrObserverViewRate`，未配置层级 `ErrObserverView`，均计 `entitysync_interest_view_rejected_total{reason}`；同值不重评、不占限速；返回生效的视野 |
| 评审 ① | `Watch` / `MoveView` / `Unwatch`（`self = 0`） |
| 评审 ② | benchstat 由维护者在夜间性能测试统一跑（基准名与命令见实施汇报） |

与草案的偏差：① 滞回量没有单独的配置项，取缺省视野的 `LeaveRadius − EnterRadius`（规则只声明一次，零值配置行为不变）；② 视野大小零值 = 缺省视野，范围零值 = 只允许缺省视野，`EnterRadius` 必须落在量化网格上；③ `Bands` 现在也检查不超过 `MaxLeaveRadius`；④ 新增 `MoveView`（只移动视点）与 `Interest.ObserverView` 读取当前视野，demo 重连时沿用玩家上次请求的视野。

接入层：game-demo 新增 `scene_view`（消息 10025）→ `HandleSceneView` → `Scene.SetView` → `SetObserverView` + `Apply`，错误码 100022 / 100023；框架接入层（`game/playeraccess`、生成器）不提供这个消息：LOD 表、视野范围与错误码都是业务的，与 `move` 一样写在业务端点里。

### 现状（实施前，已核对当时源码）

**observer 与 subject 共用实体 ID，session 在最后一步换算。**

- `InterestConfig.Session` 的注释写明：观察者、目标、关系都按实体 ID 编号，只在交给 Manager 时经 `Session` 换成会话 ID 一次；不设 `Session` 时会话 ID 就是观察者 ID。
- `Interest.Enter(id, at)` 用同一个 `id` 先 `aoi.AddSubject`、再 `aoi.AddObserver`，并 `self.Set(id, []int64{id})`。`Interest.Move` 用同一个 `id` 先 `MoveSubject` 再 `MoveObserver`。
- demo 场景（`demo/internal/service/game/scene.go.tmpl`）：`Join` 用 `subject.ID()`（完整 EntityID）调 `Enter`，`sessionFor` 用 `GetUniqueIDFromEntityID` 换成 playerId 作会话；`SetTeam` 把成员 playerId 转成实体 ID 后 `team.Set(observer, peers)`。
- 事务内入口的观察者都取 `e.ID()`：`QueueMove(e, at, observer=true)`、`QueueSpatial`、`QueueRelation` / `QueueRelationDelta`（`interest_queue.go` 应用时 `fact.id`）。

**“observer 可以是任意实体”只是名义抽象。** `Interest` 只有两种登记入口：`Enter`（既看又被看）和 `Show`（只被看），没有“只看”的入口；仓库里所有 `Enter` 调用方登记的都是有会话的玩家：demo 场景是玩家实体（会话 = playerId）；`scripts/perf/sync-aoi` 与 `codegen/internal/entity/testdata` 的 nestgame / dataengine 夹具是模拟玩家，先 `OpenSession(SessionID(实体 ID))` 再 `Enter`，不设 `Session`（会话 ID 直接等于实体 ID），其余单位一律 `Show`。会被订阅下发的 observer 必须对应一个会话，而会话按玩家开。

**依赖“observer 与 subject 同一 ID 空间”的 4 处：**

1. `aoi.go` `evaluatePair`：`if observer.id == subject { return }`（空间来源不订阅自己）。
2. `aoi.go` `evaluateObserver`：入场候选筛选 `subject != observer.id`。
3. `interest.go` `Enter`：`self.Set(id, []int64{id})`。
4. `interest.go` `Leave`：`relation.Clear(id)`（作为观察者）与 `relation.Forget(id)`（作为目标）用同一个 `id`。

`AOI` 与 `AOICluster` 的注释没有写这条约定（`AOICluster` 只写“ids are cluster-global”）。绕过 `Interest` 直接调用 `AOI.AddObserver` / `AOICluster.AddObserver`、把会话 ID 或 playerId 当 observer 时，第 1、2 处比的是两种 ID：自己不被排除，实体 ID 数值恰好等于该 playerId 的目标会被误当成自己而看不见。仓库里目前没有这样的调用方。

**订阅合并键与多对一的隐患。** `Interest.held` 按 `(observer, subject)` 记来源；`Manager` 侧一个来源对一个 `(session, subject)` 只记一份订阅（`SubscriptionSource.Unsubscribe`：“释放本来源对 (session, subject) 的订阅”），而整个 `Interest` 是一个来源。所以两个 observer 经 `Session` 映射到同一会话、都看见同一目标时，其中一个看不见了就会 `Unsubscribe`，把另一个也需要的订阅一起退掉。`Session` 的注释没有禁止多对一，现在没有这样的调用方。

**半径全局一份。** `AOIConfig.EnterRadius`、`LeaveRadius`、`Bands`、`MaxVisible` 对所有观察者相同。使用点：`aoi.go` `resubscribe`（以 `LeaveRadius` 为半边长的方框决定订阅哪些块）、`evaluatePair`（进入用 `EnterRadius`、保持用 `LeaveRadius`、分档用 `Bands`、容量用 `MaxVisible`）、`evaluateObserver`（候选按 `EnterRadius` 筛选）、`aoi_cluster.go` `placeObserverLocked`（边界镜像的方框按 `LeaveRadius`）、配置期 `AOIConfig.validate` 的 `worstObserverBlocks`（按全局 `LeaveRadius` 算单个观察者最多订阅的块数，超过 `MaxObserverBlocks` 启动即拒绝——注释强调“启动期拒绝，不在生产里第一次遇到”）。

### 问题

1. **概念错位**：observer 实际是“某个玩家的视野”，却以实体 ID 编号；自排除、self 关系与退场清理都靠“同一个号既是实体又是视野”成立，约定只写在 `Interest` 和 demo 里，`AOI` / `AOICluster` 单独使用时容易用错。
2. **视野距离只能全局一个**：不同玩家的视距设置、设备档位、玩法（侦察类 buff、观战、GM 视角）无法各自不同。

### 方向一：observer 用什么编号

- **A1 observer = playerId（建议）**：观察者表按 playerId 记，观察者带上“自己的实体”（`self`，玩家自身实体 ID；为 0 表示没有，例如观战）。
  - 自排除改为 `subject == observer.self`（上面第 1、2 处）。
  - `Session` 变成恒等：observer 就是会话 ID，`held` 的键自然是 `(session, subject)`，上面的多对一隐患随之消失；demo 的 `sessionFor` 去掉。
  - 关系源的 observer 改为 playerId、subject 仍是实体 ID，语义是“玩家关心哪些实体”。demo 的 `SetTeam` 变简单：observer 直接用成员 playerId，只有 subject 要 `BuildEntityID`。
  - 视点不再绑死在玩家实体上：玩家控制英雄、载具时，observer 的位置取被控制实体的位置即可。
  - 代价：observer 只能是有会话的玩家；“非玩家也要看”（NPC 感知）不走这套 Interest（它本来就是给同步下发用的）。
- **A2 视野 ID（摄像机抽象）**：observer 是独立的视野，带 `session` 与可选的跟随实体。最通用（一个玩家多个视野、观战、小地图远景），但要多做两件事：合并表按 `(session, subject)` 记、来源里区分各视野的空间来源；同一目标在两个视野落在不同距离档时，要在视野之间选视图。现在没有多视野需求；将来需要时可以在 A1 上加（一个 playerId 下挂多个视野）。
- **A0 保持现状**：只在 `AOI` / `AOICluster` 注释里写明“observer 与 subject 同为实体 ID”，并可加构造期检查。

### 方向二：半径放在哪里

- **B1 每个观察者自己的视野参数，配置给缺省值与上限（建议）**：
  - 观察者新增 `EnterRadius` / `LeaveRadius`；`AOIConfig` 的同名字段作缺省值，新增 `MaxLeaveRadius` 作上限。
  - 预算检查保持在启动期：`worstObserverBlocks` 改按 `MaxLeaveRadius` 计算；单个观察者登记或修改时校验 `0 < Enter ≤ Leave ≤ MaxLeaveRadius`，不合法返回 `ErrInterestConfig`，不在生产里第一次超预算。
  - 所有使用点改读观察者自己的值：`resubscribe` 方框、`evaluatePair`、`evaluateObserver` 候选筛选、`AOICluster.placeObserverLocked` 镜像方框。
  - 正确性不变：目标移动时只评估登记在旧、新块上的观察者（`observersAt`），而每个观察者按自己的半径订阅块，大视野的观察者自然登记在更多块上。订阅本来就是单向的，A 看得见 B 不要求 B 看得见 A。
  - 运行期改半径：新增“设置视野参数”的操作，效果同 `MoveObserver` 带覆盖变化（`resubscribe` + 完整重评）。
  - 代价：每个观察者多两个 int64；视野大的观察者订阅更多块（由上限约束）；`AOICluster.crossesCapacityLocked` 不涉及半径，不受影响。
- **B0 保持全局**：需要不同视距时按视距分多个 `Interest` / `AOI`。代价是同一张地图的实体要登记多份。

### 改动点（A1 + B1）

| 位置 | 改动 |
| --- | --- |
| `framework/sync/entitysync/policy/aoi.go` | `interestObserver` 加 `self`、`enter`、`leave`；`AddObserver` / `addObserverUnbounded` 接收它们；`resubscribe`、`evaluatePair`、`evaluateObserver` 改读观察者的值，自排除改比 `self`；`AOIConfig` 加 `MaxLeaveRadius`，`validate` 按它算预算；类型注释写明 observer 键是 playerId |
| `framework/sync/entitysync/policy/aoi_cluster.go` | `AddObserver` / `MoveObserver` 同步签名；镜像方框按观察者自己的 `LeaveRadius`；注释写明键约定 |
| `framework/sync/entitysync/policy/interest.go` | `Enter` / `Move` / `Leave` 区分 playerId 与实体 ID；`self` 关系改为 `Set(playerID, [entityID])`；`Leave` 用 playerId 做 `Clear`、实体 ID 做 `Forget`；`Session` 按拍板项处理；`Resubscribe` / `Visible` 参数改为 playerId |
| `framework/sync/entitysync/policy/interest_queue.go`、`spatial_component.go` | `QueueMove(observer=true)`、`QueueSpatial`、`QueueRelation` / `QueueRelationDelta` 的观察者由 `e.ID()` 换成 playerId（换算方式按拍板项）；新增排队版的视野参数修改 |
| `demo/internal/service/game/scene.go.tmpl` | `Join` / `Move` / `Leave` 传两个 ID；去掉 `sessionFor`；`SetTeam` 的 observer 改用 playerId |
| `scripts/perf/sync-aoi`、`codegen/internal/entity/testdata`（nestgame、dataengine 夹具）、`policy` 包测试 | 跟随新签名；自排除、关系、`Leave` 清理、按观察者半径的进出与预算拒绝补回归 |
| 文档 | `docs/framework/guide/04-sync.md`、`docs/framework/INTEREST-BLOCK-AOI.md`、`docs/framework/impl/04-sync.md`、CHANGELOG（公开 API 变化，未上线不做兼容） |

### API 变化（A1 + B1，草案）

| 现在 | 改后（草案） |
| --- | --- |
| `Interest.Enter(id, at)` | `Interest.Enter(playerID, entityID int64, at spatial.Point, view ObserverView)`；`view` 为零值时取 `AOIConfig` 缺省 |
| `Interest.Move(id, to)` | `Interest.Move(playerID, entityID int64, to spatial.Point)`（同时移动目标与视点） |
| `Interest.Leave(id)` | `Interest.Leave(playerID, entityID int64)` |
| `Interest.Resubscribe(observer)` / `Visible(observer)` | 参数改为 playerId |
| `QueueMove(e, at, observer)` / `QueueSpatial(...)` / `QueueRelation(e, ...)` / `QueueRelationDelta(e, ...)` | 签名不变，观察者由 `e` 换算成 playerId |
| `RelationSource.Set(observer, subjects)` 等 | 签名不变，`observer` 语义改为 playerId，`subjects` 仍为实体 ID |
| `AOI.AddObserver(id, at)` / `AOICluster.AddObserver(id, at)` | `AddObserver(playerID, self int64, at spatial.Point, view ObserverView)` |
| — | 新增 `ObserverView{EnterRadius, LeaveRadius int64}`、`AOIConfig.MaxLeaveRadius`、`Interest.SetObserverView(playerID, view)` 与排队版 |
| `InterestConfig.Session` | 按拍板项删除或保留 |

### 建议

A1 + B1，一次做完：两部分改的是同一批函数（观察者登记、`resubscribe`、`evaluatePair`），分两次会把这些签名改两遍。`Bands` 与 `MaxVisible` 先保持全局，等有具体玩法需要时再按观察者配置。属于 sync 三大块的公开契约调整，按 roost-coding 纯重构流程实施：先补现有行为的回归，再改签名，最后补新行为的红绿用例。

### 需要拍板

1. observer 编号：A1（playerId）、A2（视野 ID）还是 A0（保持现状、只补注释）。
2. `Enter` / `Move` / `Leave` 的新形状：直接改成两个 ID 参数（上表），还是新增 `EnterPlayer` 等并删除旧入口。
3. 事务内入口从实体换算 playerId 的方式：`InterestConfig` 新增 `ObserverOf func(entityID int64) (playerID int64, ok bool)`（业务可控，非玩家返回 false），还是固定用 `GetUniqueIDFromEntityID` 并只接受玩家这一种实体类型。
4. `InterestConfig.Session`：A1 下删除（observer 就是会话 ID），还是保留作 playerId → 会话的换算（为将来会话 ID 不等于 playerId 留口）。
5. 半径：是否采用 B1（每观察者可配，`MaxLeaveRadius` 作启动期预算上限）；`Bands`、`MaxVisible` 是否也按观察者配置（建议先不）。
6. 运行期修改视野参数是否需要排队版（事务内调用走提交门），还是只提供政策阶段外的直接调用。

## D15 合服（把一个服并入另一个服）

**状态：维护者 2026-10-20 决定别名合服，设计 §10 八项均按推荐，已实施（P1～P4，实施记录见设计 §12）。** 现行说明：[PLAYER-MIGRATION §6](../framework/PLAYER-MIGRATION.md#6-合服把服-a-并入服-b)（runbook）、[guide/09-services §4](../framework/guide/09-services.md#4-关键业务边界)（account）；剩余边界见 [KNOWN-LIMITS](KNOWN-LIMITS.md#合服d15)。以下为决定前的登记。 登记：2026-10-20 新用户试用第八轮（游戏服务器专家视角）；源码基线 `8ca14004`。设计全文：[FEATURE-2026-10-20-server-merge-alias.md](FEATURE-2026-10-20-server-merge-alias.md)。

**现状**：迁服（[PLAYER-MIGRATION](../framework/PLAYER-MIGRATION.md)、game-demo `cmd/sidmove`）搬的是同一个 sid，玩家绑定不变，只做 WAL 排空 / 单实例锁 / 玩家文档比对三项检查；玩家与服的绑定只在 account 的 `Role.ServerID` 与会话 sid，Gate 与 game 按会话 sid 静态判断。阻碍：slot 键 `(accountID, serverID)` insert-only，同账号两服各有角色时改绑必然冲突；没有改绑接口；一个进程只认一个 sid（会话比对、`dbscope=sid` 库名带本进程 sid、单实例锁、World UniqueID）。

**可选方向**：
- **别名合服（推荐）**：离线（A、B 停服排空），按 sid 独占的数据由工具搬进 B 的库，account 记录“A 并入 B”，会话的承载服解析为 B；角色身份与 slot 不变，game 进程与三大块不改，可回退。改动在 account 与两处读会话 sid 的消费者，外加 `accountctl` 子命令与 `cmd/sidmerge`。
- 一个进程服务多个 sid：改动落在接入、单实例锁、按 sid 分库、多 World 共存，成本高。
- 物理合服（改绑 `ServerID`）：先要决定双角色怎么处理，不可回退。

**审查对原提案的更正与补充**（设计 §1.4、§6.3）：
- D9 之后物理库来自每个服务配置的 `dataengine.databases`：“玩家文档不用搬”只在 A、B 把每个非按 sid 逻辑库映射到同一 Mongo 上的同一物理库时成立；建议不同则拒绝合服。`sidmerge` 要分别用 A、B 的配置换算源库与目标库。D9 的映射不能代替搬迁（映射按逻辑库、加本进程 sid，两个逻辑库不得共用物理库），合服也不改 B 的映射、不触发 `ErrDatabaseMappingChanged`。
- `wal-drained` 不覆盖 outbox：停机时已暂存的 effect 留在 Mongo，认领不按 sid 过滤；A、B 不共用控制库时 A 剩下的 effect 没人发。前置条件要加“A 的 outbox 为空或共用控制库”。
- 持久化的 sid（如赠礼 saga 的 `FromSID`）使“全部 saga 终态”成为必需条件；发往 A 的持久总线消息需要清单（哪些主题持久尚未逐个核对）。
- 行号、`_id` 冲突举例、审计方式、错误码的更正见设计 §1.4。

**需要拍板**：设计 §10 的八项（字段命名、多次合服拍平、误启动自检、源库处置、工具划分、原服标识显示，以及审查补充的“非按 sid 逻辑库位置不同时拒绝”与“outbox 检查放在哪里”）。

**实施后维护者决定（2026-10-21，均按推荐，保持现实现）**：①启动自检在 account 不可达时重试 30 秒后拒绝启动（fail-closed）；②合服前 saga 终态检查覆盖整个 saga 存储（严格）；③demo 合服角色登录拉全服邮件时带原服范围 `ServerScope(A)`。

## D16 单个实体 / 玩家的运维入口

**状态：维护者 2026-10-20 决定 C（按推荐），已实施（2026-10-20，结果见下文“实施结果”）。** 决定：框架只做 `entity.kick` 与 `entity.unload`，命令走现有 admin 鉴权与审计接线（不另设审批）；改档 / 恢复按 B 的业务 GM handler 与离线流程。登记：2026-10-20 新用户试用第八轮第 1 项；源码基线 `8ca14004`。

**现状（已核对）**：框架的 admin 命令只有 `configdata.*`、`hotcode.*`、`bus.dlq.*`，没有针对单个实体 / 玩家的命令；代码层只有 `ManagerAccess.Unload`。投影按版本条件写：`Put` / `Patch` 的过滤条件是 `_id` + `_version == expected`（`framework/dataengine/engine/mongo_store.go:222-224`），成功后写 `_version = next`、`_schema`、`_last_tx`。所以运行期直接改 Mongo 的结果是确定的、按设计的：
- 只改字段、不动 `_version`：版本仍匹配，下一次投影按内存状态覆盖改动（改动静默丢失）。
- 恢复旧备份（`_version` 回退）：版本不匹配，`ErrProjectionConflict` fatal，进程 fence；重启时启动投影恢复再次冲突、起不来，直到人工处理。

这不是投影的缺陷：内存是权威，第三个写者就是 fail-stop 要拦的情形。缺的是**安全的运维入口**与文档：回档、补偿改档在运营里是常规操作。本次审查已把“运行期不得直接改权威集合”与冲突的识别 / 处置补进 [guide/03 §2](../framework/guide/03-dataengine.md) 与[维护手册 §4](README.md#4-故障处理)。

**可选方向**：
- **A 框架提供实体运维命令**：`entity.kick`（关该玩家全部会话）、`entity.unload`（排空该实体投影后卸载、期间拒绝写入）、`entity.inspect`、`entity.restore <id> <doc>`（经 Nest + DataEngine 以一次正常事务整文档写回：进 WAL、版本 +1、可审计）。代价：admin 命令要按实体类型找 Manager 与 DAO、处理 Remote 托管与按 sid 分库，`restore` 要定义 schema 校验与哪些字段可写。
- **B 只给写法与 runbook**：在线改档写成业务的 GM handler（一条普通 Nest 消息，strict 写入，就是今天的正式链路）；整档恢复走离线流程（停该 sid 进程 → `sidmove wal-drained` → 写回备份文档，`_version` 任意 → 启动后冷加载以库里的版本继续）。代价：每个项目自己写 GM handler；离线恢复要停整个 sid。
- **C 折中**：框架只做 `entity.kick` 与 `entity.unload`（通用、与业务字段无关），改档 / 恢复按 B。

**建议**：C。踢下线与卸载与业务字段无关，框架做一次各项目继承；改档的字段语义、校验与审计属于业务（参考 `service/account` 的 `ResolvePendingCreation` 把操作者备注写进记录的做法）。

**需要拍板**：1）选 A / B / C；2）选 A 或 C 时命令走现有 admin 鉴权与审计接线，还是要求单独的审批（部署方）。

### 实施结果（2026-10-20）

**命令的最终形状**（使用说明在 [guide/03 §6](../framework/guide/03-dataengine.md#单实体运维命令d16)，实现契约在 [impl/03 §2 第 9 条](../framework/impl/03-dataengine.md#2-必须保持的契约)）：

| 命令 | 注册位置 | 参数 | 回答 |
| --- | --- | --- | --- |
| `entity.kick` | `infra/network/gateway/admin.go` `RegisterAdminCommands`；经 `wiring/gate.RegisterPlayerAdminCommands` 由 `GameIngressMod.Provide`（独立 Gate）与生成的嵌入 TCP Mod `Provide`（`internal/access/player/tcp/server_gen.go`）调用 | `player_id`、`reason?` | `player_id`、`closed` |
| `entity.unload` | `framework/dataengine/engine/admin.go` `RegisterAdminCommands`；`wiring/dataengine` Mod 在 Provide 时注册 | `entity_id`（完整 ID）或 `player_id` + `kind?`（EntityKind 数值）；`timeout?`（缺省 5s）、`reason?` | `entity_id`、`kind`、`resident`、`unloaded`、`drained` |

两条都经 ops `/admin/execute`，鉴权与审计用现有接线，各另记一条带 reason / trace_id / operator 的 Info 日志；不另设审批。

**`entity.unload` 的语义**（`Runtime.UnloadEntity`，`framework/dataengine/engine/entity_unload.go`）：①业务池取实体锁（持有实体的在途事务先照常结束、不被拒绝）后，在 Projector 准入临界区登记卸载屏障；②命令 goroutine 等该实体已准入的记录投影完（`WaitEntityProjection`）；③业务池 `ManagerAccess.Unload`；④撤销屏障。屏障期间写到该实体的事务在 WAL 前以可重试的 `dataengine.ErrEntityUnloading` 拒绝、Nest 回滚。下一次访问正常冷加载。

| 情形 | 结果 |
| --- | --- |
| 未加载 | 成功，`resident=false`，什么也不做（不登记屏障） |
| 在途事务持有实体超过 timeout | `ErrEntityUnloadBusy`，没有登记屏障，那笔事务照常提交 |
| 已准入记录在 timeout 内没投影完 | `ErrEntityUnloadTimeout`，实体留在内存、屏障撤销，记录照常投影 |
| 另一次卸载进行中 | `ErrEntityUnloadBusy` |
| Remote 托管、所有权 local_owned / shared | 与本地实体同一流程（Remote 写同样经准入与投影；屏障在 WAL 前挡住 Remote 记录） |
| Remote 托管、所有权 fenced / recovering / quarantined / sharing / draining / unknown | `ErrEntityUnloadBusy`（由 Remote 运行时收尾） |
| `AutoPersist=false` | `ErrEntityUnloadNotPersistent`（库里没有它，卸载等于丢弃） |
| 在业务 worker 上调用 | `fctx.ErrBlockingInBusinessWorker`，不做任何事 |

失败都是确定的“没有卸载”，错误不包裹 ctx 错误（admin 审计 outcome 记 `error`，ops 回 500 + 原因，不是 504 的“结果未知”）。指标 `dataengine.entity_unload.total{result}`、`dataengine.entity_unload.admission_rejected.total`。

**与草案的偏差**：
1. `entity.kick` 的注册：独立 Gate 由框架 Mod（`GameIngressMod`）注册；嵌入 TCP 的 TCPRuntime 由生成的 player TCP Mod 创建，框架 Mod 拿不到它，所以由生成的 `Provide` 调一行框架函数 `gatewiring.RegisterPlayerAdminCommands`（命令实现与注册函数都在框架里）。
2. “在途事务”落到两处：命令开始时持有实体的事务**先等它结束**、不拒绝（取锁受 timeout 约束，超时才 busy）；屏障之后才开始的写入被拒绝。这样一次运维操作不会把恰好在执行的玩家请求打回。
3. `player_id` 不写 `kind` 时在本进程已注册的 kind 里找唯一号相同、已加载的实体：唯一一个就是它，没有就是“未加载”，多个则要求写 `kind`。`kind` 用 EntityKind 数值（框架里没有 kind 名注册表）。
4. 卸载后若玩家在线、仍有 Sync 订阅，框架会立即为订阅者从库重载（既有 `nest.unload_resync`）；要实体真正离开内存先 `entity.kick`。不为 unload 关掉重载。
5. 踢下线不封禁、不给客户端发原因（协议里没有踢下线消息）；reason 只进日志。

**证据**：
- 修前红：`TestModRegistersEntityUnloadAdminCommand`（`wiring/dataengine`）`admin commands after Provide = [], want "entity.unload"`；生成工程 `TestAdminEntityKickClosesThePlayersConnections`（`TestGeneratedPlayerTCPAdminKick` 运行）`server_gen_test.go:53: admin commands after Provide = [], want entity.kick`。
- 行为回归（`framework/dataengine/engine/entity_unload_test.go`，正式 Nest + 文件 WAL + Mongo 替身）：排空后卸载、卸载期间写入被拒并回滚、冷加载与卸载前一致；投影超时；在途事务超时 busy / 期限内等它完成；未加载 / 非持久 / 并发 / 业务 worker；Remote 托管实体。变异：去掉 `reserve` 里的屏障检查 → `write during unload = <nil>`；去掉排空等待 → `unloading entities = 0, want 1` 与超时用例 `Unloaded:true, <nil>`；去掉第①步取锁 → `the transaction that held the entity when the unload began was refused: nest: transaction commit rejected`。
- 真实依赖：生成工程 `TestGeneratedDataEngineAdminUnload`（strict / async / pipelined，真实 Mongo + 文件 WAL，`scripts/test-dataengine-generated.sh` 三阶段全通过）；`TestRealGateAdminKickClosesTheClientConnection`（独立 Gate + 真实 NATS，客户端连接被断开）；嵌入 TCP 见生成工程用例（真实 TCP 连接被关、可重新登录）。

**实施后维护者决定（2026-10-20，均按推荐）**：①`entity.kick` 不做短时禁止重登（封禁属于账号 / 接入层的产品状态，另做）；②`entity.unload` 缺省 timeout 保持 5s（受 `ops.admin_timeout` 约束），不加配置键；③`kind` 只接受数值，不加 kind 名注册表（只给 `player_id` 时自动定位）；④命令开始时在途的事务按“等它结束、超过 timeout 报 busy”处理，不改为立即失败。

## D17 全服邮件的内置投递策略

**状态：维护者 2026-10-20 决定 A，已实施（2026-10-20）。** 维护者原意：全局邮件是一个 ID，个人邮箱每次从全局拉取合并到个人邮箱。细则（按推荐）：①登录时拉一次，`List` / `Summary` 前再检查；②每玩家一个水位（已合并到的最大全局邮件 ID）；③合并生成带全局邮件 ID 的个人邮件，已读 / 已领 / 删除记在个人这份，按全局 ID 去重、附件只领一次；④全局邮件有有效期，过期后不再合并，已合并的按个人邮件有效期；⑤目标条件用业务谓词，框架内置“注册时间早于发送时间”；⑥可撤回，未合并的不再合并，已合并的默认不动；⑦范围与 `AudienceBroadcast` 一致（全服 / 单服 / 公会）。登记：2026-10-20 新用户试用第八轮第 3 项；源码基线 `8ca14004`。

**现状（已核对）**：`AudienceBroadcast` 的范围是一个服 / 公会 / 全体（`service/mail/types.go:242-246`），投递只经业务提供的 `mail.Deliverer`（`service/mail/store.go:62-77`：没有缺省，“任务队列、首次读取时懒回填、夜间扫描”由部署选择；`Deliver` 按（邮箱、邮件）幂等）；`Broadcast` 为 nil 时拒绝广播（`service.go:167`）。`Service.Deliver`（`service.go:361`）导出给 Deliverer 使用，但 mail 没有“按 scope 列出广播信封”的入口，业务要做懒合并就得自己维护广播索引与每玩家水位。生成骨架原注释“投给每个在线玩家”与契约不符，会引向漏发离线玩家的实现，已修正（CHANGELOG `[Unreleased]`，`TestMailBroadcastSkeletonDescribesTheWholeAudience`）。

**可选方向**：
- **A 框架提供可选的“存一份 + 懒合并”策略**（例如 `mail.NewLazyBroadcast`）：广播信封按 scope 建索引（有效期、目标条件如注册时间 / 等级由业务谓词给出），每个玩家一个水位，`List` / `Summary` 前或登录时合并到邮箱；骨架缺省仍为 nil。代价：mail 的读路径要多一步合并与一份按 scope 的存储，十万级 scope 的索引大小与过期回收要定。
- **B 不进框架，给参考实现**：game-demo 或文档给一份懒合并的 Deliverer + 登录钩子写法。代价：每个项目复制一份。
- **C 维持现状**（只靠修正后的注释）。

**建议**：A，作为可选策略、不改缺省。全服补偿 / 公告是几乎所有长线游戏都有的需求，懒合并要碰 mail 的读路径，业务在包外做不干净；`store.go` 现有的“库不替部署选策略”的取舍保留为缺省 nil。

**需要拍板**：1）选 A / B / C；2）选 A 时目标条件用业务谓词还是固定字段（注册时间、等级）。

**实施结果（2026-10-20）**：
- 形态：`service/mail/broadcast.go` 的 `LazyBroadcast`（`NewLazyBroadcast(LazyBroadcastConfig{Index, Eligible, MaxActive})`）实现 `Deliverer`，`Service.New` 按类型识别；骨架缺省仍为 nil，接入一行，`wiring/mail` 的 Mod 在策略没有自带账本时给它 `<key_prefix>:bcast:` 下的 Redis 账本。存储有 Redis（`NewRedisBroadcastIndex`）与内存（`NewMemoryBroadcastIndex`）两版，共用一份实现：账本是一个 versionstore 文档，旁边一个只增的版本提示键。
- ①登录拉一次：新 RPC `Mail.PullBroadcasts(ctx, playerID, BroadcastProfile)`，接点是登录端点（game-demo `enter_game`，与付费补发同一位置；框架的实体加载钩子 `OnEntityLoaded` 不带画像、且可能在快池上，不适合阻塞 RPC）；`List` / `Summary` 前再检查，账本头不高于水位时只读一次版本键（O(1)），账本按版本在进程内缓存。
- ②水位：`Mailbox.BroadcastWatermark`；全局邮件 ID 在账本文档的同一次 CAS 里分配并登记（不另用 id-gen：分开的计数器与记录会让读者先看到 8 再看到 7，水位越过 7）。
- ③合并：条目键是广播信封 id，`Entry.GlobalID` 记全局邮件 ID，已读 / 已领 / 删除记在这条上；水位与条目同一次 CAS，删除后不重现。合并不另存个人信封（“存一份”），条目指向那一份信封。
- ④有效期：过期的不再合并；已合并的按信封自身 `ExpiresAtUnix`（与全局邮件相同）消失。回收：过期记录在下一次发布 / 撤回时移除，未过期至多 `MaxActive`（缺省 1000），超过以 `ErrBroadcastFull`（590116）拒绝。
- ⑤条件：内置“注册时间早于发送时间”，注册时间由登录端点放进画像（`RegisteredAtUnix`，取 account `Role.CreatedAtUnix`；game-demo 经认证器的 `svcaccount.RoleCreatedAtClaim`）；业务谓词 `Eligible(profile, mail)`，输入放 `Attributes`。
- ⑥撤回：新 RPC `Mail.RevokeBroadcast`，未合并的不再合并，已合并的不动。
- ⑦范围：`""` / `mail.ServerScope` / `mail.GuildScope`；公会成员关系由业务在画像里提供，变化后再拉一次。
- 与细则的偏差：已合并副本的有效期没有单独配置（与全局邮件同一过期时刻）；“注册时间早于发送时间”取严格早于、按秒。
- 证据与剩余边界：CHANGELOG `[Unreleased]` 与 [KNOWN-LIMITS](KNOWN-LIMITS.md) 的“全服邮件 `mail.LazyBroadcast`”。

**实施后维护者决定（2026-10-20）**：合并进个人邮箱的那份**随全局邮件一起过期**（选 A，不另设个人有效期）——取代状态行细则④里“已合并的按个人邮件有效期”的说法；`RoleCreatedAtClaim` 放在 `service/account`、撤回只在 Mail RPC 上提供（均按推荐）。

## D18 业务时区与“业务日”边界

**状态：维护者 2026-10-20 决定 A（按推荐），已实施。** 决定：`time.business_timezone` 缺省 UTC（启动日志打印，生产示例写明），`clock` 提供业务日辅助，生成的 `main` 内嵌 `time/tzdata`。登记：2026-10-20 新用户试用第八轮第 4 项；源码基线 `8ca14004`。

**现状（已核对）**：`infra/base/clock` 的业务时钟只有偏移（`time.logic_offset`），包注释把“日 / 周重置”列为业务时钟的用途，但没有业务时区配置，也没有业务日边界工具；文档没有每日重置的推荐写法。业务只能用 `time.Local` 或自己 `LoadLocation`。生成的 Dockerfile 运行层是 `gcr.io/distroless/static-debian12:nonroot`（`codegen/internal/roost/render.go:1364`），生成的 k8s / compose 清单与 `main` 都不设 `TZ`、不嵌入 `time/tzdata`：容器内 `time.Local` 是 UTC，开发机通常是本地时区，同一份代码跨天时刻不同。distroless 镜像是否自带 zoneinfo 本次未在容器内实测（尚未确认），嵌入 `time/tzdata` 可消除这个依赖。

**可选方向**：
- **A 配置 + 工具**：`time.business_timezone`（缺省 UTC，启动日志打印；生产配置示例写明），`clock` 提供 `BusinessDay(t)` / `NextDayStart(t)`（可带每日重置时刻），生成的 `main` 导入 `time/tzdata`；文档补“存上次重置的业务日，登录 / 首次操作时惰性重置，需要准点推送时用实体定时器”的写法；`glsvet` 对业务包里的 `time.Local` 给提示。代价：多一个配置键与少量 API。
- **B 只给文档与部署**：文档写推荐写法，生成部署写出 `TZ`。代价：时区仍是进程环境，开发与线上不一致的风险留给部署。

**建议**：A。规则声明一次（配置 schema），运行期加载时强制一次，与业务时钟的单一来源一致；时区不应取决于容器环境变量。

**需要拍板**：1）选 A / B；2）选 A 时缺省 UTC 还是必填。

**实施结果**（2026-10-20）：

- 配置：App 的声明 `timeConfig`（`framework/app/config_schema.go`）新增 `time.business_timezone`（缺省 `UTC`）与 `time.business_day_start`（每日重置时刻，`HH:MM`，缺省 `00:00`；草案的“可带每日重置时刻”做成第二个键）。两个键都带 `example`（生成器写进每个服务的开发配置与生产示例的 `time:` 段，`renderBusinessTimeConfig`）与 `shared`（`roost config check --all` / doctor `config-across-services` 要求各服务相同）。`timeConfig.ValidateConfig` 加载时拒绝拼错 / 非 IANA 的名字、`Local`、空名、非两位 `HH:MM`，各自点名键；启动前检查、`--check-config`、doctor `config-check` 同一处。启动日志 `starting server` 打印 `business_timezone` 与 `business_day_start`。
- API（`infra/base/clock/calendar.go`）：`Calendar`（`Location`、`DayStartOffset`、`BusinessDay(t) Day`、`DayStart(t)`、`NextDayStart(t)`）与 `Day`（十进制 `YYYYMMDD`，可存 DAO、可比较）；`ParseCalendar` / `LoadTimezone` / `ParseDayStart` / `NewCalendar`；进程级 `SetCalendar` / `ProcessCalendar` 与包级 `BusinessDay` / `DayStart` / `NextDayStart`。业务时钟带日历：`clock.NewBusinessIn(offset, calendar)`、`clock.CalendarOf(business)`，`app.BusinessCalendar(registry)` = `CalendarOf(app.BusinessClock(registry))`。重置时刻按墙上时间，夏令时那天 23 / 25 小时。
- tzdata：生成的 `main.go`（`renderMain`）导入 `time/tzdata`；game-demo 没有自己的 main 模板，同一个 `renderMain`。“distroless 是否带 zoneinfo（尚未确认）”不再相关：编译出的 game-demo 进程在看不到系统 zoneinfo 的环境里（macOS `sandbox-exec` 拒读 `/usr/share/zoneinfo`、`/var/db/timezone`、`GOROOT/lib/time`，`TZ` 清空、`ZONEINFO` 指向空目录）接受 `Asia/Shanghai`；镜像本身仍未在容器内实测。
- glsvet：业务包（`game` 目录）里的 `time.Local`、`time.LoadLocation` 打印 hint（同一个 `//glsvet:system-clock` 豁免）。
- 文档：[guide/10-time §2A](../framework/guide/10-time.md#2a-业务时区与业务日)（推荐写法：存上次重置的业务日、登录 / 首次操作惰性重置、准点用实体定时器取 `NextDayStart`）、[impl/10-time §1A](../framework/impl/10-time.md#1a-业务日历design-todo-d18)、CODEGEN_REFERENCE §14、PROJECT_GENERATOR §13 / §14 / §17、生成工程的 IMPLEMENTATION 文档；idle-sim / merge-casual 手册的 UTC 日界夹具不改，补一句正式业务按 `time.business_timezone`。game-demo 没有日界逻辑（活动窗口按 Unix 纪元对齐，与时区无关），只在 `game/activity` 的 `WindowStart` 注释写明按日历的窗口用业务日历。
- 与草案的偏差：① 只设 `ZONEINFO` 为空目录模拟不了“没有 zoneinfo”（Go 在 `ZONEINFO` 里找不到时回落到系统目录，实测仍加载成功），回归改用 macOS `sandbox-exec`，并先用不内嵌 tzdata 的对照程序确认环境确实看不到 zoneinfo，看不到才断言，否则 Skip（自带 zoneinfo 的 Linux 主机）。② `Day` 不做日期运算，周重置由业务按 `Day` 推。
- 修前红：`infra/base/clock` 编译失败 `undefined: Calendar` / `undefined: ParseCalendar` 等（没有业务日函数）；`TestBusinessTimezoneIsDeclaredCheckedAndCarriedByTheBusinessClock` 报 `App does not declare time.business_timezone`；`TestRunSetsTheProcessCalendarAndLogsIt` 报 `process calendar after start = UTC 00:00`；`TestGeneratedConfigsWriteTheBusinessTimeZone` 对每个服务配置报 `does not write time.business_timezone: UTC and time.business_day_start: 00:00`；`TestGeneratedMainEmbedsTheTimeZoneDatabase` 报 `with the generated main's imports [] and no zoneinfo on the host, loading Asia/Shanghai: exit status 1: operation not permitted`；`TestDoctorChecksCrossModConfigRulesThroughTheProcess`（main 不导入 tzdata 时）报编译出的 game 进程 `config: time.business_timezone: business time zone "Asia/Shanghai" is not a time zone this process can load (...): operation not permitted`。变异核对：业务日改成“减去重置时刻取日期、起点 + 24h”，夏令时用例与整年不变量用例变红。
- 剩余限制在 KNOWN-LIMITS“业务时区与每日重置”一行。

## D19 视野（View）：逻辑 ID、空间 / 关系两类、玩家与视野多对多

**状态：维护者 2026-10-20 决定实施，已实施（2026-10-20，见下文“实施结果”）；多视野修复后的后续三项与“两种应用模式”维护者 2026-10-21 已决定并实施（见下文“两种应用模式”）；“实施结果”末尾的三项小决定维护者 2026-10-21 均按推荐保留现实现。** 决定：①按本节模型实施；②`ViewID` 用 **int64**，由业务层分配与解释；③**同步层级**：视野给出同步层级（数据分级，例如联盟视野只同步标记为联盟层级的字段），同一目标被多个视野看见时同步级别高的（数据多的）；层级之间不是包含关系时，各层级的数据都同步（取并集）；④`MaxVisible` **按每个视野**计；⑤视野操作**不进 Nest、不做排队版**：视野的增删改、成员变更、挂接都是直接调用，由视野自身的锁保护；⑥上限缺省按建议（`MaxViewsPerPlayer` 64——2026-10-21 起只是 ApplyAsync 的缺省，ApplySync 缺省与上限是 9——、`MaxPlayersPerView` 4096，启动期可配）；⑦按最干净的方式处理、不留历史包袱：`RelationSource` 与按来源名的旧关系 API 删除，全部改为关系视野。登记：2026-10-20；源码基线 `057513bb`（D14 已实施）。来源：维护者确认“一个玩家确实会有多个视野”；随后补充：联盟数据同步这类 relation 需求要能归到同一模型（例如 alliance view），`viewId` 应是**逻辑 ID**、不能写死为实体 ID（否则绑死一个实体），例如副本的阵营视野。

### 实施结果（2026-10-20）

按 roost-coding 纯重构流程分步提交：先在旧 API 上补关系与多来源合并的回归基线（关系进出与 Hide、关系先撤距离仍持有、分档换视图、self 与 Leave、队伍互见与离场双向清理；断言只看 Manager 订阅表与 packer 收到的视图），再提交同步层级并集（entity + Manager），再改 policy 的结构与 API 并迁移全部调用方（基线用例只改准备步骤），最后补新行为用例并逐项变异验证。提交见 CHANGELOG `[Unreleased]` 本条。

| 决定 | 落点 |
| --- | --- |
| ① 模型 | 空间视野：`DefineSpatialView(id, at, sight, follow)` / `MoveSpatialView` / `SetSpatialView` / `SetViewFollow`；关系视野：`DefineRelationView(id, layer)` / `SetViewMembers` / `AddViewMembers` / `RemoveViewMembers`；多对多：`AttachView` / `DetachView`，`RemoveView` 自动从所有玩家解除；合并：`hold.layers` 的来源键从来源名改为视野内部编号（空间视野区分距离与块覆盖，0 为 self），任一挂接视野持有就保持订阅，最后一个释放才退订 |
| ② ViewID | `policy.ViewID int64`，业务分配与解释；`MainView = 0` 保留给主视野（见下“主视野”），业务视野不能用 0。内部另有不复用的 `handle` 作 AOI 观察者编号与来源键，业务 ID 只用于登记表查找 |
| ③ 同步层级 | 层级 = `SyncProfile`，字段沿用 `NamedSyncView` 按 DAO 字段名列出；`entity.UnionSyncProfiles` / `SyncProfile.Layers` 定义并集身份（Key 为各层级身份排序后用 `+` 连接），`SyncViewSet.Lookup` 解析为字段并集、`Covers` 判断包含；Interest `selectedProfile`：被包含的去掉、字段相同按优先级留一个、其余取并集，包含关系按 `ViewSets` 每个类型的字段判断；Manager `profilePriority` 对并集取各层级最优，与 `Lookup` 一致——Manager 的 profile 结构不用改，并集仍是一个 `SyncProfile` |
| ④ MaxVisible | 每个空间视野是 AOI 的一个观察者，`MaxVisible` / `MaxObserverBlocks` 自然按视野计 |
| ⑤ 不进 Nest | 视野操作只取视野登记表的锁（`viewMu`），改事实并记一条变更，由下一次 `Apply` / 政策阶段按调用顺序应用，不等待空间计算；不提供排队版。排队入口的处理见下 |
| ⑥ 上限 | `MaxViewsPerPlayer`（缺省 64，含主视野）、`MaxPlayersPerView`（缺省 4096），超出 `ErrViewLimit` 并计 `entitysync_interest_view_limit_rejected_total{reason}`；启动期 `worstObserverBlocks × MaxViewsPerPlayer ≤ MaxPlayerBlocks`（新配置项，缺省 1024 × 64） |
| ⑦ 不留包袱 | 删除 `RelationSource`、`Source`、`SourceSpatial/Self/Block`、`InterestConfig.Relations / SourceProfiles / Profile / ObserverOf`、`Interest.Relation`、`QueueRelation` / `QueueRelationDelta`、`MoveShown`、`DefaultObserverOf`；层级配置改为 `BandLayer` / `BlockLayer` / `SelfLayer` |

**排队入口的处理**：位置是 DAO 驱动的事实，必须与提交一致——回滚的移动不能让视野先看见新位置——所以 `QueueMove` / `QueueSpatial` / `QueueObservedBlocks` 保留提交门。改的是“谁跟着动”：空间视野可以跟随实体，事实应用时移动目标并带上跟随它的全部视野（主视野跟随玩家实体），于是 `observer` 参数与 `ObserverOf`（从实体换算观察者）都不再需要，删除；关系事实（`QueueRelation*`）属于视野配置，按决定⑤改为直接调用，删除。只有一套：位置走提交门，视野配置直接调用。Leave / Hide 对排队事实的改写保持“与先应用、再 Leave / Hide 逐项相同”：目标那一半作废，跟随者那一半保留。

**主视野 ID 方案**：主视野是玩家 `Enter` / `Watch` 时自带的空间视野，没有全局 ViewID，只在按玩家的入口里出现（D14 的 `Enter` / `Watch` / `MoveView` / `SetObserverView` / `ObserverView` / `Visible` / `Unwatch` / `Leave` 都作用于它），文档与代码里用 `policy.MainView`（0）指代；所以 0 对业务视野保留。它只挂给玩家自己、计入 `MaxViewsPerPlayer`，不能被别人挂接（观战用 `Watch`，或业务另定义一个跟随目标的空间视野）。

与草案的偏差：① ViewID 用 int64（决定②），不是字符串。② `SetSpatialViewRadius` 改为 `SetSpatialView(id, ObserverView)`，LOD 与视野大小一起改、归一规则同主视野；另加 `SetViewFollow`。③ 跟随实体不只用于自排除，视点也随实体移动（因此删掉 `observer` 参数与 `ObserverOf`）；`SetViewFollow(id, 0)` 后视点留在当前位置。④ `Move(entityID, to)` 合并了 `Move(player, entity, to)` 与 `MoveShown`。⑤ 草案“分档取最近档”“MaxVisible 按玩家并集”“提供排队版”“保留 RelationSource 薄封装”按决定③④⑤⑦改。⑥ 草案 7 的“分档表随玩家的 LOD 层级”改为每个空间视野有自己的 `ObserverView`（LOD 与半径），客户端 LOD 请求只改主视野。⑦ 块预算新增 `MaxPlayerBlocks`，没有把 `MaxObserverBlocks` 改成按玩家（见下“需要决定”）。⑧ 记过业务视野变更之后 Manager 的政策阶段也结算与重试（与排过事实同一规则），Drain 计入未应用的视野变更。⑨ 新增 `Seen(playerID)`（玩家经任一视野持有的全部目标，诊断与 sync-aoi 核对用）、`ViewPlayers` / `ViewMembers` / `ViewVisible`、`AOI.FlushBlocks`。

调用方：game-demo 队伍改为每队一个关系视野（`Scene.SetTeam`，空了就 `RemoveView`）；SLG 夹具联盟改为共享关系视野（联盟同步示例）、侦察为私有关系视野；MMORPG 夹具 `BandLayer`；codegen nestgame / dataengine 夹具与 `scripts/perf/sync-aoi` 去掉 `observer` / `ObserverOf`，sync-aoi 新增 `-views-per-player`（每个玩家额外的跟随视野）与 `-alliance-size`（共享关系视野）场景开关，两项为 0 时负载不变。

**需要维护者决定**（都不阻塞使用）：

1. 跨政策来源的层级：同一 pair 同时被 Interest 与 Group / Direct / SelfView 持有时，Manager 仍按优先级取一个视图（未改）。A 保持（推荐：不同政策一般不在同一 Manager 上同看一个目标，SelfView 的 `self` 通常已是全字段）；B 在 Manager 的 `bestProfile` 里也取并集——Manager 不知道字段，只能一律并集身份，会改变现有 SelfView / Group 的视图身份。
2. 块预算形式：A 现状（推荐），`MaxObserverBlocks` 仍按单个视野，另加 `MaxPlayerBlocks` 核算 `worst × MaxViewsPerPlayer`，缺省下只有单视野已超预算才触发；B 把 `MaxObserverBlocks` 改成按玩家，缺省 1024 ÷ 64 = 每视野 16 块，game-demo（最大视野 300、块 100，每视野 64 块）等现有配置启动即被拒，须各自调大。
3. 关系视野是否排除挂接玩家自己的实体：A 不排除（推荐，现状），自己经 self 与联盟两个层级看见自己，按包含取高或取并集；B 排除，需要知道每个玩家的自身实体，挂接、成员变化时多一层过滤。

### 排队事实异步应用（2026-10-21，维护者 2026-10-21 给出方向）

**问题**（[夜间补测 §14'.2](../performance/NIGHT-20261020.md#142-sync-aoi服务端--客户端两个进程)“根因与修复”）：排队模式下 `QueueMove` / `QueueSpatial` / `QueueObservedBlocks` 只入队，全部事实在 Manager `Flush` 的政策阶段由 `applyQueued` 一次性应用：AOI 的写阶段、并行重评与订阅结算都在 Flush 的关键路径上，Flush = AOI + 打包下发；直接调用模式下 AOI 在调用方线程里分散完成，Flush 只剩打包下发。16 个额外空间视野时 Flush 超过同步周期，tick 被跳过。

**方向**：排队事实的应用改为分批、由独立的应用线程持续消费，不占 Flush 关键路径，也不占 Nest 线程；Flush 只结算已经应用好的结果并下发。

**必须保持的语义**（与改动前逐项相同）：

1. 只有提交后的事实生效：应用线程取事实时读同一个框架提交门（`SyncConditionFor`），未确认的留在队首，回滚 / 拒绝的丢弃并释放发布屏障；回滚的移动永远不被看见。
2. 同一实体按顺序生效：取批规则不变（未确认前缀阻塞同 ID 后续事实，其他 ID 可越过；`BatchSize` 不切开同一提交）。
3. Leave / Hide 与排队事实的改写规则不变：改写只作用于还在队列里的事实；应用线程取批与应用在同一次 `Interest.mu` 持有内完成，Leave / Hide 也持 `mu`，所以不会插进一批的中间。已应用、未结算的事实等同于“先应用、再 Leave / Hide”。
4. 内容与可见性同一帧出现：事实持有的发布屏障（`HoldSyncPublication`）在**结算之后**才释放，结算（订阅 / 退订说给 Manager）在 Flush 的政策阶段，同一 Flush 随后才捕获内容；所以一个移动的新位置与它带来的进出视野仍在同一帧。
5. Drain 等到在途事实全部应用并结算：`queuedPending` 计入队列、应用中与已应用未结算的事实；应用线程的错误（事实无法应用）由下一次结算返回，只让一次 Flush 失败（同改动前）。
6. 关闭顺序：`Interest.Close` 先注销政策钩子，再停应用线程并等它退出（不持 `mu` 等待），最后在 `mu` 内释放队列中与已应用未结算事实的发布屏障、退订全部 pair。

**线程模型**：先做**单个应用线程**（每个 `Interest` 一个 goroutine，第一条排队事实时启动）。理由：`Interest.mu` 是一把锁，保护 AOI、`held` 与订阅结算；AOI 的写阶段本来就是串行短写入，读阶段已经按观察者并行（`InterestConfig.Workers`），单线程消费已能把 AOI 移出 Flush。按 `AOICluster` 分区或按块分片做多线程需要把 `mu` 拆成 AOI 分片锁与结算锁、跨分片的视野与跟随要协调，改动面大，等单线程版的数据证明不够再做。

**唤醒**：`queueFact` 非阻塞通知应用线程；视野登记表的变更同样通知。事实的提交确认在 periodic 模式下没有通知（`Manager.WakeSync` 只在 on_change 生效，不改 Manager 契约），所以队里有未确认事实时应用线程按 1ms 起、最长 8ms 的退避轮询提交门；有事实应用完时调 `Manager.WakeSync`（on_change 下让 Manager 尽快结算）。**合批**：两步之间至少隔 `applyGather`——periodic 取同步周期的 1/10（20 Hz 时 5 ms），on_change 取 `OnChangeBatchWindow`（缺省 0，不合批）。结算只在 Flush 里发生，周期内拆得更细只会让同一观察者被几个事实分别波及、多次重评（不合批的早期版本在 16 视野负载下每步约 6.5 个事实，10 秒里应用线程的并行重评累计 4.6 s；S1 每个 Flush 一批时累计约 2.6 s）。

**Flush 与应用线程的交接**：一致点是批次边界。应用线程每一步在 `mu` 内取一批（≤ `BatchSize`）、写阶段、并行重评，AOI 事件留在 AOI 的事件缓冲里，应用完的事实记入“已应用未结算”；Flush 的政策钩子在 `mu` 内结算：应用登记表剩余变更、把 AOI 事件归并进 `held` 并说给 Manager、重试被拒的 pair，最后释放已应用事实的发布屏障。任何结算入口（政策钩子、业务直接 `Apply`）都在结算后释放屏障，不会出现订阅先到、内容被屏障挡住的半态。屏障就是 `mu` 本身：Flush 若撞上应用线程正在算的一批，最多等这一批算完（不等后面的批）。Flush 期间不执行 AOI 应用。

**背压**：队列总量仍由 `MaxQueuedFacts`（缺省 65536）限制，计入排队、应用中与已应用未结算的事实；应用线程落后、或 Flush 停摆时队列满，`QueueMove` 等返回 `ErrInterestQueueFull`（不变），由 handler 返回失败。新增指标：`entitysync_interest_fact_backlog`（gauge，排队 + 应用中 + 已应用未结算）、`entitysync_interest_apply_duration`（histogram，应用线程每一步的耗时）、`entitysync_interest_settle_delay`（histogram，一批从应用完成到被结算的等待）。

**实施结果（2026-10-21）**：按上述最小一步实施（单应用线程 + 合批），没有改 Manager 与 Nest 的契约（Manager 只多了 `ManagerCounters.PolicyDuration` 计数）。原场景 16 视野 Nest：Flush 41 → 30 ms（政策阶段 19 → 7 ms），P99 约 130 → 97～141 ms（单机噪声大，3 样本），帧数 98～100%；direct 57～70 ms。剩下约一个同步周期的差距是结构性的：Manager 按自己的周期结算，截止前几毫秒才应用完的事实要等下一周期。可选方向（都要改交接，待维护者决定）：A Flush 开始前等应用线程追上已确认的事实（AOI 重新部分回到 Flush 关键路径，上限是一批）；B 应用线程按 Manager 周期对齐，截止前一个合批窗口强制走一步；C 维持现状，延迟敏感的场景用 on_change。证据见 [夜间补测 §14'.2a](../performance/NIGHT-20261020.md#142a-根因与修复2026-10-21)。**维护者 2026-10-21 选 C**，并把它当作模式问题处理：本节的应用线程成为 `ApplyAsync`，缺省改为 `ApplySync`（见下“两种应用模式”）。

**未采用**：Flush 用 `TryLock` 撞上就跳过结算（Flush 完全不等，但这批内容要多等一个周期）；多应用线程（见上）；把结算也放进应用线程（结算会与 Flush 的捕获交错，一批的进出视野可能分在两帧，屏障释放早于捕获时还要另加交接）。


### 两种应用模式（2026-10-21，维护者决定）

**决定**（维护者 2026-10-21，对上一节“排队事实异步应用”留下的三项）：

1. **Nest 比 direct 多约一个同步周期：选 C**，接受，不加“Flush 前等应用线程”，也不做周期对齐。
2. **修夹具口径**：sync-aoi 的“新订阅全量”延迟只按本 tick 改的值计时，不把全量更新里本会话没见过的旧改值按改值时刻计时。先用确定的小场景证明旧口径会把旧值年龄算进去，再修（见下“夹具口径”）。
3. **接受两项可见变化**：提交后立即 Flush 看不到结果、要改用 Drain；`ManagerCounters` 新增 `PolicyDuration` 字段。写进 CHANGELOG 与升级说明。
4. **第 1 项本质是模式问题**（维护者：每个玩家额外视野 8 个以内和 8 个以上分成两种模式，由业务层选择，对应不同游戏类型）：同步应用模式适合每玩家不超过 8 个额外视野（MMO、MOBA、卡牌），排队事实在 Flush 的政策阶段同步应用、保留“只评估移动过的目标”，提交后 Flush 就能看到结果，不需要应用线程；异步应用模式适合 8 个以上（RTS、大量召唤物 / 眼位），即上一节的应用线程，Flush 只结算，Flush 不会因 AOI 超时，代价是尾延迟多约一个周期。缺省同步（维护者倾向：大多数游戏视野不多）。

**实施**（[interest.go](../../framework/sync/entitysync/policy/interest.go) `ApplyMode` / `viewsPerPlayerFor`，[interest_queue.go](../../framework/sync/entitysync/policy/interest_queue.go) `applyBatchLocked` / `settleQueued`）：

| | `ApplySync`（零值，缺省） | `ApplyAsync` |
| --- | --- | --- |
| 应用在哪里 | `settleQueued`（Flush 政策阶段）在 `Interest.mu` 内先应用一批（不超过 `BatchSize`），随即结算 | 每个 Interest 一个应用线程（第一条排队事实时启动）分批应用，`settleQueued` 只结算 |
| 线程 | 没有额外线程；AOI 计算在 Manager 的 Flush 线程上 | 多一个常驻 goroutine；Flush 撞上正在算的一批时最多等这一批 |
| 结果何时可见 | 提交后的下一次 Flush（`BatchSize` 以内；剩余的下一次政策阶段继续，on_change 下 `WakeSync` 尽快再来） | 应用线程走到之后的那次 Flush；提交后立即 Flush 不保证可见，要全部可见用 `Drain` |
| 视野上限 | `MaxViewsPerPlayer` 零值取 `SyncApplyMaxViewsPerPlayer`（9），显式超过拒绝构造 | 零值 64，不受此限 |

两种模式共用取批与应用步骤 `applyBatchLocked`（提交门、同 ID 不越过、`BatchSize` 不切开同一提交）和结算入口（`applyLocked` → `releaseAppliedLocked`，结算之后才释放发布屏障），所以语义逐项相同：只有提交后的事实生效、同一实体按入队顺序生效、Leave / Hide 对排队事实的改写、内容与它带来的进出视野在同一帧、Drain 等到在途事实全部应用并结算、Close 释放全部屏障。只有“结果何时可见”和“线程模型”不同。直接调用（`Move` / `Apply`）不经队列，不受模式影响，但视野上限照样按模式检查。

**阈值**：`SyncApplyMaxExtraViews = 8`、`SyncApplyMaxViewsPerPlayer = 1 + 8`（主视野 + 8），写在 `interest.go` 并注明来源（维护者决定 + 下表实测）。`MaxViewsPerPlayer` 本来就含主视野与关系视野，所以队伍、公会、联盟等关系视野也计入这 9 个。构造时 `ApplySync` 且 `MaxViewsPerPlayer > 9` 返回 `ErrInterestConfig`，报错原文：“ApplySync allows at most 9 views per player (main view + 8 extra), MaxViewsPerPlayer is N; games with more views per player (RTS units, many summons / wards) should set ApplyMode: policy.ApplyAsync”。未知模式同样拒绝构造。Interest 配置由业务代码构造，不经 wiring 或生成配置暴露，所以没有加配置键；game-demo 模板显式写 `ApplyMode: policy.ApplySync` 并说明怎么选。

**适用游戏类型与选择指引**：

| 游戏类型 | 每玩家视野（主视野之外） | 模式 |
| --- | --- | --- |
| MMO / MMORPG | 队伍、公会、联盟关系视野，少量召唤物 | ApplySync |
| MOBA | 队伍（阵营）视野，少量眼位 / 召唤物 | ApplySync；眼位可能超过 8 个时 ApplyAsync |
| 卡牌、休闲、放置、塔防、自走棋 | 很少或没有 | ApplySync |
| SLG | 联盟关系视野 + 行军队列 / 侦察单位 | 行军与侦察单位加起来不超过 8 用 ApplySync，否则 ApplyAsync |
| RTS | 每个单位一个视野 | ApplyAsync |
| 大量召唤物 / 眼位 / 侦察单位的玩法 | 十几个以上 | ApplyAsync |

数法：主视野 1 + 关系视野个数 + 空间视野（召唤物、眼位、单位）个数，不超过 9 用缺省；可能超过就选 ApplyAsync，并按自己的密度压测。

**实测**（2026-10-21，本机 M5、GOMAXPROCS 4 / 进程，sync-aoi 每次约 20 秒、10 秒测量、200 个 tick，两轮交替顺序，只做对照不算正式测量；夹具用修好口径的版本；被测 `b6ef5573`（测量时是变基前代码相同的提交）；ApplySync 16 视野用“只把 `SyncApplyMaxExtraViews` 改成 16”的诊断构建，未入库）。原场景 `-players=1000 -entities=10000 -hz=20 -dirty=1 -visible=50`，改值→客户端（`state_change_to_client`）：

| 额外视野 | 模式 | P50 ms | P99 ms | 帧数（应发 ≤200000） | Flush 均值 ms = 政策 + 打包 | 政策阶段每 tick P95 ms | 新订阅全量 P99 ms |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 4 | ApplySync | 38.0 / 38.3 | 65.1 / 65.2 | 190113 / 190153 | 14.8 = 5.9 + 8.9 / 15.3 = 6.1 + 9.2 | 7.5 / 8.4 | 64.9 / 64.1 |
| 4 | ApplyAsync | 33.1 / 33.2 | 59.3 / 58.9 | 190327 / 190237 | 10.0 = 0.6 + 9.4 / 10.0 = 0.6 + 9.4 | 1.2 / 1.3 | 60.6 / 58.8 |
| 4 | direct | 31.6 / 32.1 | 49.4 / 51.5 | 190716 ×2 | 8.1 / 8.8（AOI 在压测线程，每 tick P50 6.8 ms） | 0 | 49.4 / 51.0 |
| 8 | ApplySync | 45.9 / 47.5 | 79.8 / 88.2 | 200000 / 199995 | 24.6 = 10.3 + 14.3 / 26.5 = 11.4 + 15.1 | 14.9 / 15.1 | 73.1 / 77.3 |
| 8 | ApplyAsync | 37.1 / 37.0 | 82.0 / 72.6 | 199598 / 199758 | 17.2 = 2.2 + 14.9 / 17.1 = 2.2 + 14.9 | 5.5 / 5.6 | 68.6 / 62.7 |
| 8 | direct | 34.8 / 35.2 | 60.9 / 56.1 | 200000 ×2 | 14.1 / 14.2（压测线程每 tick P50 13 ms） | 0 | 60.2 / 54.8 |
| 16 | ApplySync（诊断构建） | 65.3 / 63.9 | 150.1 / 144.3 | 190000 / 195170 | 47.3 = 21.9 + 25.4 / 45.7 = 21.3 + 24.4 | 38.8 / 39.0 | 79.7 / 73.4 |
| 16 | ApplyAsync | 49.6 / 51.1 | 129.6 / 137.6 | 195000 / 191998 | 35.1 = 9.0 + 26.1 / 35.6 = 9.1 + 26.5 | 29.0 / 31.3 | 62.8 / 47.2 |
| 16 | direct | 35.0 / 32.4 | 68.5 / 90.0 | 200000 ×2 | 22.6 / 24.0（压测线程每 tick P50 23～25 ms） | 0 | 68.9 / 89.2 |

新场景（`-world=4000 -block=20 -enter-radius=30 -leave-radius=36 -move-amplitude=10 -entities=50000`）：

| 额外视野 | 模式 | P99 ms | 帧数 | Flush 均值 ms = 政策 + 打包 | 新订阅全量 P99 ms |
| --- | --- | --- | --- | --- | --- |
| 4 | ApplySync | 53.9 / 53.8 | 80204 / 80218 | 4.3 = 1.0 + 3.2 / 4.1 = 1.0 + 3.1 | 53.0 / 52.6 |
| 4 | ApplyAsync | 52.7 / 53.1 | 80215 / 80340 | 3.1 = 0.0 + 3.1 / 3.4 = 0.0 + 3.3 | 52.1 / 51.5 |
| 4 | direct | 50.0 / 50.1 | 80240 ×2 | 3.0 / 2.9 | 50.1 / 50.1 |
| 8 | ApplySync | 55.9 / 55.1 | 119577 / 119576 | 6.7 = 1.7 + 5.0 / 5.8 = 1.4 + 4.4 | 53.0 / 54.2 |
| 8 | ApplyAsync | 54.2 / 53.7 | 119694 / 119809 | 5.4 = 0.1 + 5.3 / 4.4 = 0.1 + 4.3 | 54.4 / 52.3 |
| 8 | direct | 49.9 / 50.0 | 119597 ×2 | 4.6 / 4.3 | 49.8 / 49.9 |

Flush 均值 = `ManagerCounters.FlushDuration / FlushCalls`（含启动与预热的少量 Flush），政策 = `PolicyDuration / FlushCalls`，打包 = 两者之差；“政策阶段每 tick P95”只算测量窗口。periodic 下周期等待本身最多 50 ms，所以 P99 在 50 ms 上下是结构性的，比较看差异。异步 16 视野第 2 个样本运行时另一个 agent 的 worktree 里有一条短的编辑命令（不是基准），数字与第 1 个样本同量级。原始产物在会话 scratchpad 的 `applymode/`，摘要在 [benchmarks/d19-multiview-20261021/applymode-runs.jsonl](../performance/benchmarks/d19-multiview-20261021/applymode-runs.jsonl)。

**8 这个阈值**：原场景 ApplySync 的 Flush 均值按视野数近似线性增长（4 / 8 / 16 视野 15 / 25.5 / 46.5 ms，每个视野约 2.6 ms），8 视野时 Flush 约为周期（50 ms）的一半、政策阶段每 tick P95 15 ms，帧数满，有余量；16 视野时 Flush 46～47 ms 贴着周期，Flush 次数 203～208（异步 205～208、8 视野 213），开始丢 tick。所以在这台机器、这个密度（每个视野可见约 50、1000 玩家）下，8 落在“Flush 不超过半个周期”的位置，数据支持 8；按 Flush 不超过周期算的上限约 17。阈值按每个玩家的视野数定，实际 Flush 还与玩家数、可见数、移动比例成正比，玩家更多或更密时同样 8 个视野也可能更紧（没有测）。新场景（小块小视野）8 视野 ApplySync 的 Flush 只有 6～7 ms，远在周期内。

**与预期不同的一点（如实记录，没有改实现）**：本夹具里 ApplySync 的延迟没有比 ApplyAsync 低。原场景 4 视野 ApplySync P50 / P99 比 ApplyAsync 高约 5 / 6 ms，8 视野 P50 高约 9 ms、P99 两者交错（79.8 / 88.2 对 82.0 / 72.6），16 视野 ApplySync 全面更差。原因：本夹具的改值均匀分布在周期里（每 tick 10 批），ApplyAsync 的应用线程在周期内就把大部分事实算完，Flush 只结算，Flush 更短；只有截止前几毫秒到达的事实要多等一个周期，只占尾部很少的比例。ApplySync 把 AOI 放回 Flush，所有改值都要等这次 Flush 的政策阶段算完才捕获，Flush 越长整条分布越往后移。ApplySync 的好处是确定性——提交后的下一次 Flush 必定可见、没有额外线程、没有“截止前到达多一个周期”的尾巴，不是测得的延迟更低。改值集中在周期末尾到达的负载（D20 心跳打散前的全局循环恰好不是这种）在 ApplyAsync 下尾部会更差，没有测。

**夹具口径**（决定 2）：[client.go](../../scripts/perf/sync-aoi/client.go) 修前对 `ObjectUpdate` 一律计时，全量重建带上的是目标最后一次改的值，本会话没计过时就按那次改值时刻计时。确定场景 `TestFullUpdateTimesOnlyThisTicksChange`（手工编帧：tick 5 新建 Step 3、改值时刻两秒前；tick 6 同一对象全量重建仍是 Step 3；tick 7 全量 Step 7；tick 9 增量 Step 8）修前全量样本 [2000.2, 0.2] ms（P99 即 2000.2），修后 [0.0]，增量两次都是 1 个样本。修后规则：增量照常计时（晚几个 tick 到也计）；全量（新建或重建）只计 Step 等于帧 tick 的改值。代价是跨 tick 才送到的全量不计入，全量 P99 偏乐观。本轮 30 次运行的全量 P99 都在 47～90 ms。

**未采用**：按视野数自动切换模式（模式是业务对游戏类型的选择，运行中切换会改变“何时可见”的契约）；阈值按负载动态算（同一份配置在不同机器上行为不同）；只数空间视野、不数关系视野（`MaxViewsPerPlayer` 本来就含关系视野，另起一个计数要多一项配置；关系视野的应用成本不在 AOI 批次里；是否改为只数空间视野交维护者决定）。

### 现状（已核对源码）

- D14（A1）之后观察者 = playerId，空间视点每个玩家**一个**：`Interest.Enter(playerID, entityID, at, view)` / `Watch`、`MoveView(playerID, to)`、`SetObserverView`。
- 关系源 `RelationSource.Set(playerID, entityIDs)`（`policy/source.go`）按**玩家**存成员集合，没有距离分档（band 0）。联盟“成员彼此同步”要对每个成员各 `Set` 一份相同集合：N 个成员 × M 个目标，成员变化要改 N 份。
- `Interest` 已经有按 `(player, subject)` 跨来源合并的机制：`held map[pair]*hold`，`hold.bands` 按**来源名**记各来源给的档（`interest.go:116-140`），任一来源持有就保持订阅，全部释放才退订。缺的是：来源内部不能区分“同一玩家的多个视点”，也不能让一份来源被多个玩家共享。
- D14 之前靠 `Session` 多对一表达多视野，因为订阅合并不计数会互相退订（D14 第 1 步回归复现），不能作为方案。

### 方案（建议）

1. **视野 = 逻辑 ID 的可见性来源**：`ViewID` 是业务给的逻辑键（建议字符串，例如 `"alliance:123"`、`"camp:dungeon42:red"`、`"ward:777"`、`"unit:9001"`），**不绑定实体**；空间视野可以选择“跟随某个实体”，但跟随关系可改、可为空。
2. **两类视野**：
   - **空间视野**：一个点 + `ObserverView`（LOD / 半径）+ 可选 `self`（跟随实体，用于自排除）。玩家主视野（镜头）就是一个空间视野，D14 的 `Enter` / `Watch` / `MoveView` / `SetObserverView` 作用于它（保留现有 API）。
   - **关系视野**：一个显式的实体集合（联盟成员、队伍、好友、阵营全体单位），可配一个固定档（缺省 band 0）。现有 `RelationSource` 保留为“每个玩家私有的关系视野”的写法，或改为在其上的薄封装。
3. **玩家与视野多对多（Attach / Detach）**：一个玩家可挂多个视野，一个视野可被多个玩家共享。
   - 联盟：`alliance:123` 关系视野挂给全体成员；成员加入 / 退出只改这一份集合和挂接关系。
   - 副本阵营视野：`camp:…:red` 下挂该阵营所有单位 / 眼位的空间视野（或阵营本身是一组空间视野），挂给阵营里每个玩家——任何一个单位看得见，全阵营都看得见。
   - 玩家个人的多单位视野（RTS、召唤物）：每个单位一个空间视野，只挂给自己。
4. **合并沿用现有 `held`**：把 `hold` 的来源键从“来源名”扩成“视野 ID”；`(player, subject)` 只要有任一挂接的视野持有就订阅，最后一个释放才退订（不会互相退订）；分档取各视野里**最近**（细节最多）的一档，关系视野按其固定档参与比较。
5. **API（草案）**：`DefineSpatialView(viewID, at, view, self)` / `MoveSpatialView` / `SetSpatialViewRadius`；`DefineRelationView(viewID, band)` / `SetViewMembers(viewID, subjects)` / `AddViewMembers` / `RemoveViewMembers`；`AttachView(playerID, viewID)` / `DetachView`；`RemoveView(viewID)`（自动从所有玩家解除）。事务内入口提供排队版（放眼、召唤、入盟多在事务里，走提交门）。
6. **预算与上限**：`MaxViewsPerPlayer`（挂接数）、`MaxPlayersPerView`（共享扇出）、空间视野半径仍夹到 `[MinViewRadius, MaxLeaveRadius]`；启动期按 `worstObserverBlocks × MaxViewsPerPlayer` 核算块预算；`MaxVisible` 按玩家并集计。
7. **LOD 与半径**：客户端 LOD 请求只改玩家主视野；其他空间视野的半径由服务端设；分档表随玩家的 LOD 层级。
8. **自排除**：空间视野排除自己跟随的实体；玩家自己的其他单位照常按距离或由关系视野（例如 `self`）保证可见。

### 代价与风险

- 共享视野的变化（关系成员增减、阵营单位移动）要扇出到挂接的所有玩家，成本 = 变化量 × 挂接人数，由 `MaxPlayersPerView` 约束；大联盟（上千人）挂接时要测。
- 空间视野多的玩家 / 阵营（RTS 上百单位），目标移动时要评估相关块上的所有视野，评估量线性增长；需要 benchstat 与 sync-aoi 加多视野、共享视野场景。
- 分档取最近档会增加切档次数；`AOICluster` 镜像要按视野登记。
- 这是 D14 刚改的同一批代码（policy 包），公开 API 再改一次；建议在发版前一起完成，v1.30.0 只出现一次 AOI 接口变化。

### 未采用

- `viewID` 用实体 ID：绑死一个实体，阵营 / 联盟 / 眼位这类不是“一个实体”的视野表达不了（维护者 2026-10-20）。
- 回到 `Session` 多对一：订阅合并不计数，互相退订。
- 每个视野一个独立会话：客户端要处理多路订阅与去重，带宽翻倍。
- 关系需求继续每成员各 `Set` 一份：N × M 存储与 N 倍更新。

### 需要拍板

1. 是否按“视野 = 逻辑 ID、空间 / 关系两类、玩家与视野多对多、合并沿用 held 计数”实施（建议是）。
2. `ViewID` 类型：字符串（建议，可读、天然带命名空间）还是 int64。
3. 多视野分档取最近档（建议）。
4. `MaxVisible` 按玩家并集计（建议）。
5. 视野增删改、成员变更、挂接是否提供排队版（建议提供）。
6. 缺省上限：`MaxViewsPerPlayer` 64、`MaxPlayersPerView` 4096（建议，启动期可配，预算按它们算）。
7. 现有 `RelationSource` 去留：保留为“玩家私有关系视野”的薄封装（建议，现有调用方不改），还是全部迁到关系视野 API。

**实施后维护者决定（2026-10-21，均按推荐，保持现实现）**：①同一 pair 同时被 Interest 与 Group / Direct / SelfView 持有时，Manager 仍按优先级取一个视图，不在 Manager 里取并集；②块预算新增 `MaxPlayerBlocks`，`MaxObserverBlocks` 仍按单个视野计；③关系视野不排除挂接玩家自己的实体（按包含取高或取并集）。

**两种应用模式的后续决定（维护者 2026-10-21）**：①缺省 `ApplySync`（保持）；②同步模式上限 9 计入关系视野（`MaxViewsPerPlayer` 含关系视野，保持现实现）；③“提交后立即 Flush 看不到排队事实、要用 Drain”只在 `ApplyAsync` 下出现，CHANGELOG 按此写，确认。

## D20 移动由实体自己的心跳驱动：MapBasic / Move 组件 + actionflow 控制

**状态：维护者 2026-10-21 提出方向；同日定了四点：移动组件放 `gameplay`；心跳处理本质也是 Nest，“hb 就在 nest 的快线程”；心跳框架写进 `gameplay`；以后跑在 Nest 快池（short）里的业务周期逻辑都以心跳为底层。要求先给心跳（HB）方案（下文“心跳（HB）方案”），确认后再试。2026-10-21 方案确认，P1 心跳框架已实施（下文“P1 实施结果”），P2 组件与 actionflow 适配已实施（下文“P2 实施结果”），P4 压测工具心跳驱动的代码已实施、测量待补（下文“P4 实施结果”），P5 的技能与 Buff 两项已实施（下文“P5 实施结果”），生成器接入已实施（下文“生成器接入实施结果”），P3 game-demo 接入已实施（下文“P3 实施结果”，连同 P5 的 game-demo 刷怪）。** 来源：分析 D19 多视野“Nest 模式失守”时，维护者指出压测里的移动由一个全局循环驱动、不像真实游戏；移动应在**实体自己的心跳（HB）**里做，心跳天然打散；加 `MapBasic` 与 `Move` 两个组件，在 game-demo 统一使用，由 `gameplay/actionflow` 控制移动，逻辑统一且性能高。设计参考 ssr 的 heartbeat 模块（自动心跳 autotick 与按实体的心跳组件），下文 §6 说明哪些思路照搬、哪些不适合 roost。

**P1 实施后维护者决定（2026-10-21）**：①`Elapsed` 上限取 `max(1s, 档位)`（选 A）；②Remote 共享模式（Shared）下框架不跳，由业务层自己处理（选 A）；③卡住兜底按推荐：10s 起、同一登记连续卡住翻倍、80s 封顶、执行后复位（维护者 2026-10-21 确认）；④Remote 共享模式下心跳不能执行，因此**不允许挂载心跳**：登记时拒绝并给出明确错误（维护者 2026-10-21；**已实施**，共享是运行期状态，运行期进入共享的由下一拍停掉，见下文“决定 ④ 实施结果”）。

**压测后维护者决定（2026-10-21）**：调度结构按档分环、10ms 分辨率；驱动 1 个；投递方式 P1 先用单目标 `Dispatch`（X），驱动接口按“一格到期的一批心跳一次交出”设计，内部先逐个 Dispatch；同时做方案 D（Nest 批量入队、逐个执行：一次取队列锁入队一批独立的单目标作业、每个 worker 只唤醒一次并连续取作业，作业语义与 X 相同）的原型与压测，达标（CPU 接近 Y、业务 p99 不差于 X、语义不变）则心跳切到 D，否则保留 X。E（搭便车）、F（按 worker 固定分片）不做。

**维护者决定（2026-10-21，对方案 §需要拍板逐条）**：①包放 `gameplay`（`gameplay/heartbeat`、`gameplay/movement`，装配 `wiring/heartbeat`）；②投递方式由压测决定：单目标 `Dispatch` 与 ssr 式“每条消息带一批实体、worker 逐个 TryLock”对比（维护者经验 ssr 方式更好）；③**不合并**：同一实体的多个心跳登记各自投递；④**跳拍**：上一拍在途时跳过，不累积不补发，**跳拍打 Warn 日志**（限频）并计数；⑤快池很快、理论上不会满：去掉水位整格让路与每格上限，只保留“每实体至多一拍在途”与 `ErrQueueFull` 跳拍兜底；⑥频率采用 ssr 式**固定档位**（如 50ms / 100ms / 200ms / 500ms / 1s / 2s / 5s / 10s / 30s / 60s），不支持任意周期；⑦驱动 goroutine 数参考 ssr；⑧HB 本身不持久化，心跳里改持久字段由业务按自己的持久级别处理；⑨卸载窗口接受（实例核对 + 令牌）；⑩**所有实体都支持**：心跳投到实体所在的池（快池 / 慢池）；Remote 托管实体投到当前拥有方，所有权迁移时旧拥有方停、新拥有方重新登记；⑪调度结构由压测决定：ssr 式固定档位环、时间轮、最小堆对比（维护者经验 ssr 方式更好）；⑫entitytimer 的驱动不改；⑬实体的 tick 都走 HB（房间 / 场景本身是实体，用自己的 HB）；⑭⑮按推荐：game-demo 移动改为客户端发目标、服务端心跳推进；位置不每拍落库（nopersist，按周期或离开场景落库）。回调只做内存计算与入队、不阻塞。

### P1 实施结果（2026-10-21）

**状态：P1 已实施**（分支 `feat/d20-heartbeat-p1`，基于 `be5a76f5`；本地提交，由维护者推送）。框架 `gameplay/heartbeat`，装配 `wiring/heartbeat`（Mod 名 `heartbeat`）。用法 [guide/10-time §4A](../framework/guide/10-time.md#4a-实体心跳周期推进)，契约 [impl/10-time §2B](../framework/impl/10-time.md#2b-实体心跳)，指标 [OBSERVABILITY“实体心跳”](../framework/OBSERVABILITY.md#实体心跳gameplayheartbeat)，剩余边界 [KNOWN-LIMITS“实体心跳”](KNOWN-LIMITS.md#实体心跳d20-第-1-期)。读代码时 codebase-memory 图的根是主检出、不是本 worktree，结构结论以 worktree 源码核对为准。

**API 最终形状**：

```go
var Periods [10]time.Duration       // 50ms 100ms 200ms 500ms 1s 2s 5s 10s 30s 60s
const Resolution = 10 * time.Millisecond

func NewDriver(Config{Name, StallAfter, WarnInterval, Logger}) *Driver
func (d *Driver) SetClock(func() time.Time) / Bind(nest.Client, Lookup) error / Start() error / Stop(ctx) error
func (d *Driver) NewHost(entity.IThreadSafeEntity) *Host
func (h *Host) Register(name string, period time.Duration, fn Func, opts ...RegOption) (*Reg, error)
func (h *Host) OnDestroy()
func (r *Reg) Start() / Stop() / SetPeriod(time.Duration) error / Active() bool / Period() / Name()
type Beat struct { Now time.Time; Elapsed, Period time.Duration; Skipped int; First bool }
type Func func(Beat) error           // 返回错误 = 本拍事务回滚
func WithDurability(nest.DurabilityPolicy) RegOption   // 缺省 async，回滚策略固定 undo
func wiringheartbeat.NewMod(*entity.ManagerAccess, *Driver) *Mod
```

**按决定逐条落实**：位置 ①；按档分环 10ms、1 个驱动 goroutine、相位 = hash(实体 ID)（⑦⑪）；每拍每个到期登记一条单目标 `Dispatch`、不合并（②③），驱动内部投递接口 `deliverer.deliverBatch(ctx, []beat, rejected)` 一次交出一格的一批，P1 实现逐个 Dispatch，方案 D 落地后只换这一层；回调在实体自己的 worker、实体锁与事务内（handler 包级注册、回滚 undo），快池 / 慢池由 Nest 按种类分流（⑩）；每个登记至多一拍在途，在途跳拍、`ErrQueueFull` 跳拍，计数 + 限频 Warn，无水位让路、无每格上限（④⑤）；HB 不持久、`OnInitFinish` 登记、`OnDestroy` 撤销，持久级别按登记选（⑧）；不追帧，`Beat{Now, Elapsed, Skipped, First}`；事务内启停走 AfterCommit、回滚作废；卸载窗口用实例核对 + 令牌（⑨）；Remote 托管实体只在拥有方跳（⑩）；entitytimer 不改（⑫）。

**与方案的偏差（如实记录，需要时请维护者改）**：

1. **`Elapsed` 上限是 `max(1s, 档位)`，不是一律 1 秒**：2s 及以上的档若夹到 1 秒，每拍都少算（5s 档的回复只算 1 秒）。只对 ≥ 2s 的档有差别。
2. **保留了“卡住兜底”，缺省 10s 并按次翻倍（最多 80s）**：方案 §3 写的是 2s。没有它，Nest 在执行 handler 之前放弃的消息（Remote 写准入被拒、准入后卸载且重载失败）会让登记永远“在途”、再也不跳。实测 2s 不翻倍时有放大：-race 下 5 万实体 1s 档（每秒 5 万拍，超过 -race 吞吐约 2700 拍/秒），排队超过 2s 后每 2s 重投一份，`Stalled:66451`、`SkippedInflight:100446`，执行仍追不上；改成 10s 起翻倍后放大降为对数级，-race 冒烟降到 2000 实体。非 -race 的 5 万实体 1s 档：`Delivered:100000 Executed:100000`，跳拍、卡住、丢弃全为 0。
3. **Remote 共享模式（Shared）P1 不跳**：方案只说“投到当前拥有方”。共享模式下各写者都经分布式锁写、实例都是 `Shared`，本地看不出谁是“拥有方”；P1 只在 `LocalOwned` 执行，避免两边同时跳。维护者决定 ④ 后改为不允许挂载（见下文“决定 ④ 实施结果”）。
4. **心跳 handler 按持久级别注册 4 个**（`heartbeat.beat.memory|async|strict|pipelined`），由登记选，缺省 async：落实 ⑧“由业务按自己的持久级别处理”，方案 §9 第 8 条原只写一个。
5. **生成器未接入**：P1 没有消费方，`roost` 不装这个 Mod；`go test ./codegen/...` 本期不涉及。

**Remote 所有权迁移设计**：不另建心跳侧的所有权状态，复用 Remote 已有的两道闸：

- Nest：发给 Remote 托管 ID 的消息自动经 I/O 阶段 `PrepareRemoteWriteBatch` 取写许可（权威拥有方不是本进程 → fenced；实例 `Draining` / `Fenced` / `Quarantined` → 拒绝；`Unknown` / `Recovering` → 转 `LocalOwned`）。`TransferRemoteOwnership` 先排空已准入的本地写、把实例转 `Draining`，再改权威，所以旧拥有方的最后一拍先于新拥有方的第一拍。
- 心跳：驱动投递前看实例所有权状态，`LocalOwned` / `Unknown` / `Recovering` 才投（`mayOwn`）；handler 执行前再要求 `LocalOwned`。作用是非拥有方与共享模式不投不跳，且非拥有期间被准入拒绝的拍不会卡住成为拥有方之后的心跳（变异核对 M6）。
- 新拥有方“重新登记”就是正常加载：实例被路由来的第一条消息加载，组件 `OnInitFinish` 登记并按 DAO 状态启动；已有旧实例（`Fenced`）的进程在实例回到 `LocalOwned` 后恢复。
- 测试 `TestRemoteHeartbeatFollowsOwnershipWithoutDoubleBeats`：两个真实 Nest 引擎 + 按 `beginWrite` / `TransferRemoteOwnership` 写门与状态顺序模拟的 Remote Manager，驱动在转移期间并发推进；断言旧拥有方在转移标记之后执行 0 拍、新拥有方在标记之前执行 0 拍，各自的拍都经过 Remote 写准入，共享模式不投。没有真实 Mongo / Redis 下的迁移演练。

**红绿证据**：实现与测试同轮写成，红用变异核对给出（每个强制点改掉一处，对应用例在当前代码上变红，下表是失败原文）：

| 变异 | 变红的用例与原文 |
| --- | --- |
| M1 相位不按 hash（全进第 0 格） | `TestTiersFireOnTheirPhaseOncePerPeriod`：`100ms cell 0 holds 10000 of 10000 entities (want about 1000): [10000 0 0 0 0 0 0 0 0 0]` |
| M2 不看在途、到期就投 | `TestInflightBeatSkipsCountsAndWarnsAtALimitedRate`：`delivered 5 beats while the first was still in flight, want 1` |
| M3 Warn 不限频 | 同上：`warns inside the interval = 2, want still 1` |
| M4 事务内启停立即生效 | `TestStartStopFollowTheTransaction`：`rolled-back start took effect: active true registered 1` |
| M5 执行时不核对实例 | `TestBeatOfAnUnloadedInstanceDoesNotRunOnItsReplacement`：`old 1 new 0 stats {Delivered:1 Executed:1 ... DroppedStale:0 ...}` |
| M6 Remote 不按所有权过滤 | `TestRemoteHeartbeatFollowsOwnershipWithoutDoubleBeats`：`owner beats: A 10 before the transfer, B 55 after (want ≥ 9 and ≥ 200)`（不双跳由 Remote 写准入保证；去掉过滤后新拥有方被非拥有期间的在途拍卡住 10s） |
| M7 换档不移格 | `TestSetPeriodMovesToTheNewTier`：`1s beats in 3s = [40 50 60 ... 330]`（仍按 100ms 跳） |
| M8 停止时批里剩下的不清在途 | `TestStopWaitsForTheBatchAndDispatchesNothingAfter`：`Stop during a batch: 1 dispatched, 50 left in flight; want the one in progress only` |
| M9 卡住阈值不翻倍 | `TestStalledBeatIsRedeliveredAndRunsOnce`：`deliveries of a beat that never runs at seconds [1 11 21 31 41 51 61 71], want [1 11 31 71]` |
| M10 心跳 handler 不开事务 | `TestBeatRunsUnderTheEntityLockInsideATransaction`：`beat ran in tx=false locked=true shortWorker=true` |
| M11 回调侧记录不随回滚恢复 | `TestStartStopFollowTheTransaction`：第三拍 `Elapsed:100ms`（应为 200ms，覆盖回滚的那一拍） |

“慢池实体投到慢池”与“多登记不合并”没有对应的强制点可变异（前者由 Nest 按种类分流，后者驱动本来就逐登记投递），用例是契约检查。

**需要维护者决定**：

1. `Elapsed` 上限：A 维持 `max(1s, 档位)`（推荐，长档不少算）；B 按原话一律 1 秒。
2. Remote 共享模式由谁跳：A P1 不跳（现状，安全）；B 共享模式下由权威记录的 OwnerSid 进程跳（需要把所有权记录的 OwnerSid 暴露到实例上，改 Remote，另行设计）。
3. 卡住兜底：A 10s 起翻倍（现状，推荐）；B 去掉兜底（Remote 准入被拒等情况下登记永久停跳）；C 回到 2s。

### 决定 ④ 实施结果：Remote 共享模式不允许挂载心跳（2026-10-21）

**状态：已实施**（分支 `fix/heartbeat-remote-shared`，基于 `c299c4cc`；本地提交，由维护者推送）。

**共享是运行期状态，不是静态声明**（已核对源码）：`entity.RemotePolicy` 只有 None / Managed / Mirror（`framework/entity/policy.go`），codegen 也只有 `remote=managed`；共享模式由 `remoteentity.Manager.EnterRemoteSharedMode` / `LeaveRemoteSharedMode`（`framework/remoteentity/ownership.go`）按实体 ID 在运行期切换（LocalOwned → Sharing → Shared；Shared → Draining → LocalOwned）。实例加载时是 `Unknown`（`LoadRemoteEntity` 不设所有权状态），共享权威下第一次写准入经 `newWriteLease` 的 marked 分支转 `Shared`（`framework/remoteentity/batch.go`）。所以组件在 `OnInitFinish` 登记时，第二个写者看到的是 `Unknown`，登记时判定不出来。

**方案（选 a）**：已在共享模式时 `Register` / `Start` 返回 `ErrHeartbeatRemoteShared`；登记后才进入共享的，由下一拍在实体锁内停掉登记（回调不执行），计 `heartbeat.stopped_total{reason="remote_shared"}` 并打 Warn；回到独占后不自动重启，业务需要时再 `Start`。不选 b（“种类可能进入共享就在登记时拒绝”）：没有种类级的共享声明，任何 `remote=managed` 种类都可能被 `EnterRemoteSharedMode`，b 等于拒绝全部 Remote 托管实体，与决定 ⑩（Remote 托管实体投到当前拥有方）冲突；要做 b 得先给 Remote 加种类级声明，属于 Remote 的改动、不在本项。停掉放在实体锁内（handler）而不是驱动 goroutine：业务视角的 `Active` 只在锁内改，与启停的事务语义（AfterCommit / 回滚）一致；代价是每个登记在共享模式下多投一拍、走一次共享模式写准入（分布式锁）。维护者 2026-10-21 确认按 a（运行时切换检测），并要求 Warn 限频（按 `WarnInterval`，与跳拍 Warn 相同）。**维护者决定（2026-10-21）：从 Shared 回到 LocalOwned 不自动恢复**，被停掉的登记保持停止，由业务在回到独占时重新 `Start`（理由：避免意外重启——共享期间状态可能已被其他写者改过；自动恢复还要框架额外记住“被谁停的”，与业务自己的 Stop 区分）。由 `TestLeavingSharedModeDoesNotResumeTheHeartbeat` 固定。

检测点：Remote 没有所有权状态切换的回调（`entity.RemoteEntityBase.TransitionRemoteOwnership` 只做 CAS），心跳也不应依赖 `framework/remoteentity` 的内部实现，所以在心跳自己的两处读实例的所有权状态（`IThreadSafeRemoteEntity.RemoteOwnershipState`，只依赖 `framework/entity` 接口）：驱动到期时（原子读）与 handler 执行时（实体锁内）。没有新增 Remote 侧的钩子：钩子会在 Remote 的 goroutine 里、不持实体锁时触发，仍然要投一条消息进实体锁才能改 `Active`，并不比到期时检测更可靠，还要改 `framework/entity`。保证与限制：共享模式下任何一拍都不执行回调；共享期间到期的已启动登记都被停掉；共享窗口短于周期、期间没有到期的拍时这个登记不被停（期间也没有执行）。

**强制点**：`host.go` `sharedErr`（Register / Start）、`driver.go` `mayOwn`（Sharing / Shared 照常投）、`host.go` `runBeat`（共享模式 → `change(false)` + `stoppedShared`）、`driver.go` `flush`（Warn）。API 变化：`Reg.Start()` 返回 `error`；`Stats.StoppedShared`；指标 `heartbeat.stopped_total{reason}`。

**红绿**（`remote_test.go`，修前在 `c299c4cc` 上）：

| 用例 | 修前失败原文 |
| --- | --- |
| `TestRegisterOnASharedRemoteEntityIsRejected` | `Register on a Remote entity in shared succeeded (registration true), want a refusal` |
| `TestEnteringRemoteSharedModeStopsTheHeartbeat` | `after entering shared mode: registration active=true, callbacks run 0 (want stopped, 0); stats {Delivered:4 Executed:4 ... DroppedNotOwner:20 DroppedDuplicate:0 Registered:1}`（静默停跳：登记仍算在跳） |
| `TestFirstAdmissionIntoSharedModeStopsTheHeartbeat` | `after the first admission into shared mode: registration active=true, callbacks run 0 (want stopped, 0); stats {Delivered:1 Executed:0 ... DroppedNotOwner:20 DroppedDuplicate:0 Registered:1}` |

另 `TestSharedModeStopsEveryRegistrationAndWarnsAtALimitedRate`（两个实体、每个两个不同档位的登记，先后进入共享：全部停掉、`StoppedShared` 4、Warn 间隔内只一条）在 Warn 不限频的版本（`96724b6a`）上失败：`warns inside the interval = 2, want still 1`。

`TestLeavingSharedModeDoesNotResumeTheHeartbeat`（回到独占后业务 Start 之前不跳、Start 后照常跳）在 `c299c4cc` 上失败（P1 是共享期间暂停、回到独占自动继续）：`back to local ownership without Start: active=true, callbacks run 10 (want stopped, 0); stats {Delivered:14 Executed:14 ... DroppedNotOwner:4 DroppedDuplicate:0 Registered:1}`。

修后五条通过；变异核对：去掉 Register 核对 → 第一条红；去掉 Start 核对 → 第二条 `Start in shared mode: err <nil> active true`；共享不投 → 第二条红（`StoppedShared:0 Registered:1`）；handler 不停 → 第二、三条红（`StoppedShared:20 Registered:1`）。原 `TestRemoteHeartbeatFollowsOwnershipWithoutDoubleBeats` 末尾“共享模式不投”的断言删除（语义已改为投一拍并停掉，由上面三条覆盖）。

**未做**（本节实施时）：真实 Redis 分布式锁 / Mongo 下的共享模式演练（测试用模拟写准入的假 Manager）。2026-10-21 已补，见下一节。

### 真实 Remote 共享模式切换验证（2026-10-21）

**状态：已验证**（分支 `test/remote-heartbeat`，本地提交，由维护者推送）。补的是决定 ④、P2 第 6 条、D21 A6 / C4 留下的未验证项：之前“框架停掉心跳”只在单元用例里直接 `Stop` 登记或用模拟写准入的假 Manager。

**怎么验**：`gameplay/movement/shared_test.go` 的三个场景经真实的 `remoteentity.Manager` 做所有权流转（第一次写认领、`EnterRemoteSharedMode` / `LeaveRemoteSharedMode` / `TransferRemoteOwnership`、写准入与共享模式的分布式锁、写门排空），每个进程是真实 Nest 引擎 + 实体管理器 + 启动的心跳驱动 + 自己的 Manager；实体 `remoteNPC` 装了 `MapBasic`（周期落库）、`Move`、`Behavior`（移动组 / 工作组心跳、任务心跳）与 AI 心跳，共 5 个登记。进程内用例的 Remote 权威（所有权记录与带 fence 的版本锁）是替身 `memRemoteAuthority`，状态机、准入与排空都是 Manager 自己的；`shared_integration_test.go`（`-tags integration`）用正式装配 `remoteentity.Assemble`（真实 Redis 版本锁与快照 L2、Mongo 持久写权威、NATS 复制）跑同一组场景，隔离库名 / 键前缀 / 主题前缀每次唯一、用完删除。每个场景在两种心跳投递方式下各跑一遍（`heartbeat.delivery` 的 `dispatch` 缺省 / `batch`，D20 选项 C）：驱动绑定的客户端包了一层记录，核对 Remote 实体的每一拍都走单目标 `Dispatch`、没有一拍进 `DispatchTryBatch`，同进程里一个本地对照实体在 `batch` 下走批量作业、在 `dispatch` 下走单目标（证明投递方式确实生效）；所有权检查（共享模式停掉、非拥有方不跳、迁移无双推进）两种方式结论相同。变异核对：让 Remote 的拍进批量作业 → `batch` 子测试红（Nest 不接受 Remote 目标、拍交回，`first start: A1 is not running every heartbeat … ai ticks 0, work ticks 0`），`dispatch` 子测试不受影响。

| 场景 | 断言 |
| --- | --- |
| 独占 → 共享 → 独占（两轮） | 进入共享后 5 个登记全部停掉，`StoppedShared` 与 `heartbeat.stopped_total{reason="remote_shared"}` 各 +5（每登记 +1）；之后 500ms 窗口里没有任何周期推进、位置不变；共享期间 `Sync` 返回 `ErrHeartbeatRemoteShared`、动作 / 任务 / 策略都留着；回到独占不自动恢复；业务恢复后 5 个登记全部接着跳。第二轮共享期间业务发起新移动：旧移动（任务的那一步）被取代、任务失败结束，新移动挂不上心跳、以失败结束（原因带共享模式），通知顺序固定为 `mission action-end 取消` → `observer mission-end` → `observer action-end 取消` → `observer action-end 失败`，两次结束通知时 `Move.Moving()` 都已为假（`Cancel` 在通知之前） |
| 所有权迁移（共享模式下转移，与生成工程的 Remote 流程相同） | A 进入共享（A1 停 5 个）→ 转给 B（A1 为 Fenced）→ B 第一条消息加载 B1（加载时恢复场景与 AI，`Unknown` 下登记合法），写准入转 `Shared`，第一拍停掉 B1 的 2 个 → B 回到独占不自动恢复，业务恢复后接着跳；A1 在进入共享之后、B1 在回到独占之前都没有任何周期推进（无双推进）；A 之后的写被 `ErrRemoteFenced` 拒绝 |
| 卸载与重新加载 | 独占时卸载：唯一的快池 worker 被另一实体占住、A1 的拍经写准入后排队，卸载后放开，排队的拍不在旧实例也不在重载实例上执行（`DroppedStale` 3），A1 之后零推进、A2 的推进全在 A1 之后；共享时卸载：A2 停 5 个后卸载，下一次写加载 A3（`Unknown` 登记合法），转 `Shared` 后第一拍停 2 个，回到独占不自动恢复，恢复后接着跳 |

**发现并修复的缺陷**（先红后绿，红在“回到独占后业务调用 Sync / 重新设置策略能接着走”这条承诺上）：

1. **任务心跳没有恢复入口**：文档让业务回到独占后调用 `BeatRunner.Sync()`，但它只管动作组；`Behavior` 的 `"<Name>/mission"` 只在任务推进时对齐，被框架停掉后任务一直不被 tick（步骤时限不再检查），业务只能直接碰 `MissionReg()`（文档说启停由 Behavior 管理）。修法：`BeatRunner` 多一个由 `NewBehavior` 装上的 `syncExtra`，`Sync` 时调 `Behavior.resyncMissionBeat`：当前任务在运行、需要周期 tick 而心跳没在跳就按它的周期 `Start`，挂不上只返回错误、不结束任务（与组的 `Sync` 相同）。
2. **重新设置当前策略是静默空操作**：KNOWN-LIMITS 让业务回到独占后“重新 `SetStrategy` / `Recover`”，但 `SetStrategy(同一个策略)` 走 `sameStrategy` 直接返回 nil，不对齐心跳；策略拒绝被替换（`CanStopByNext` 为假）时还返回 `ErrStrategyRejected`。修法：`SetStrategy` 先判 `sameStrategy`——设成当前策略不是替换，不问 `CanStopByNext`、不 Init / Stop，只 `syncBeat`。

修前失败原文（进程内场景一，`53cc3f97` 上加入用例，两处都未修）：

```
shared_test.go:865: resumed after the first shared window: A1 is not running every heartbeat: active [actionflow/1 actionflow/2 map.save] (want [actionflow/1 actionflow/2 actionflow/mission ai map.save]), moved true, ai ticks 0, work ticks 600; driver {Delivered:949 Executed:944 ... StoppedShared:5 Registered:3}
```

只修 1 之后：`active [actionflow/1 actionflow/2 actionflow/mission map.save] … ai ticks 0`；两处都修后通过。变异核对：去掉 `syncExtra` 的装配 → 场景一 `active [actionflow/1 actionflow/2 ai map.save]`、`TestBeatSyncResumesAStoppedMissionHeartbeat` `after Sync: mission beat false group beat true registered 1`；还原 `SetStrategy` 的判断顺序 → `TestSettingTheCurrentStrategyAgainResumesAStoppedBeat` `setting the current strategy again: ai: current strategy rejects replacement`。用例里还修了一处测试自身的数据竞争（排队的旧拍经写准入触发加载时读 `onLoad`，改为在 `loadMu` 下读写）。真实依赖的卸载场景（worker 被占住、拍排队超过 200ms）在 `-race` 下还报出过 Nest 慢派发追踪 `%#v` 反射读 `*heartbeat.Reg` 的数据竞争，与 main `18c2fdbe`（`Reg.GoString`）修的是同一处，rebase 后不再出现。

**没有改的（待维护者决定）**：周期落库 `"map.save"` 被框架停掉后，`MapBasic` 没有单独的恢复入口，`Enter` 在场景里返回 `ErrAlreadyInScene`，业务只能 `Leave` 再 `Enter`（同一事务里，AOI 提交后收到一次离开再进入）。A（推荐）：给 `MapBasic` 加 `Sync() error`（在场景里且配了 `SaveEvery` 时重新 `Start` 周期落库），与 `BeatRunner.Sync` 对齐；B：维持现状，文档写明 `Leave` + `Enter`。代价：A 是 movement 的一个小 API，B 让 AOI 多一次进出。另：任务因动作结束而结束时，观察者先收到任务结束、再收到这个动作结束（`Behavior` 先交任务、任务在处理里结束），这个顺序由用例固定、写进 KNOWN-LIMITS，没有改。

**复跑**：进程内 `GOWORK=off go test -race -count=1 -run '^TestRemote' ./gameplay/movement`；真实依赖 `source ~/.roost-it/roost-dataengine-it/env.sh` 并持 `$ROOST_DATAENGINE_IT_ROOT/remote-acceptance.lock`（`wiring/scripts/integration/lib/common.sh` 的 `acquire_acceptance_lock`）后 `GOWORK=off go test -race -tags=integration -run '^TestRealRemote' -count=1 ./gameplay/movement`，整套随 `wiring/scripts/integration/dataengine-env.sh test`（包已列入）。本轮结果（rebase 到 `6576b7fb` 之后，两种投递方式）：进程内 `-race -count=2` 通过；真实依赖 `-race` 通过（每个子测试约 4～6s），跑后隔离库与键为 0。没有覆盖：独占模式下不经共享的直接迁移（仍只有 `heartbeat/remote_test.go` 的模拟写准入用例）、真实依赖下的故障注入（Redis / Mongo 断开时的切换）。

### P2 实施结果（2026-10-21）

**状态：P2 已实施**（分支 `feat/d20-movement-p2`，本地提交，由维护者推送）。runner 适配在 `gameplay/actionflow`（`heartbeat.go`），组件在 `gameplay/movement`。用法 [guide/10-time §4B](../framework/guide/10-time.md#4b-移动与动作心跳驱动) 与包内 [Example](../../gameplay/movement/example_test.go)，契约 [impl/10-time §2C](../framework/impl/10-time.md#2c-移动与动作的心跳驱动)，剩余边界 [KNOWN-LIMITS“移动与动作”](KNOWN-LIMITS.md#移动与动作d20-第-2-期)。没有新指标（沿用 `heartbeat.*`）。codebase-memory 图的根是主检出、不是本 worktree，结构结论以 worktree 源码为准。

**P2 中维护者决定（2026-10-21）**：

1. **移动参考 ssr，由 action 驱动**：action 挂心跳，在心跳里调用 move 的接口。`Move` 是被动组件（设定、`Advance`、`Stop`、查询），自己不挂心跳；`MoveTo` / `Follow` 由实体的 ActionRunner 驱动，runner 挂心跳，每拍 runner tick 在途动作、动作调 `Move.Advance`。移动中的实体每拍只有一条心跳消息，不需要 Waker 绕路收尾。
2. **所有控制实体行为的都走 action**：移动、技能、AI 决策等统一经 ActionRunner 下发与收尾，不允许绕过动作直接驱动组件。
3. **runner 不随事务回滚：由业务层保证，框架不处理**（同日修正上一条“框架实现提交后生效”的要求）：handler 先做完全部可能失败的校验和修改，最后一步才调用动作的 Start / Enqueue 等；之后还可能失败的，回滚后 runner 里会残留这次的变更。心跳那一拍里动作对组件的修改随这一拍的事务提交或回滚，保持现状。

**对照 ssr（只读 `kingdom/modules/action`、`action_list`、`move`、`map_basic`）**：`action_list.setCurAction` 在组的当前动作切换时给这个组登记心跳（周期 = 动作的 `LoopInterval`，同周期不换登记），`OnTick(group)` 调当前动作的 `Loop`；`ActionMove.DoLoop` 自己算 `elapsed = 本拍时刻 - 上次时刻`，调 `MovePathInfo.StepMove(elapsed)`，路径走完返回 `ErrReachEnd`，action_list 据此结束动作；move 模块写 map_basic 的位置。roost 照搬“组的当前动作挂组的心跳、动作在心跳里调 move、move 返回到达”的结构；不同点：`elapsed` 用心跳给的 `Beat.Elapsed`（夹取、随回滚恢复），move 的结论是返回值 `Result` 而不是错误，每次切换都重新启停（不比较周期）。与 Nest 事务模型没有冲突：心跳回调本来就在实体锁与事务内，动作对组件的修改随这一拍的事务；ssr 没有事务，“runner 不回滚”按决定 3 由业务保证。改动没有超出 actionflow 与 movement。

**API 最终形状**：

```go
// gameplay/actionflow
func NewBeatRunner(ActionRunnerConfig, BeatConfig{Host, Name, Period, Options}) (*BeatRunner, error)
func (b *BeatRunner) Runner() *ActionRunner / Reg(group) *heartbeat.Reg / Ticking() bool / Sync() error
type TickIntervaler interface{ TickInterval() time.Duration }   // 动作可选；0 = 不需要周期 tick
// ActionContext 新增 Elapsed time.Duration（本组这一拍的 Beat.Elapsed）

// gameplay/movement
type Fields interface { SceneID() int64; SetSceneID(int64); Pos() spatial.Point; SetPos(spatial.Point); Facing() Facing; SetFacing(Facing) }
type Space interface { Enter(e, at) error; QueueMove(e, at) error; Leave(e) error }
type InterestSpace struct { Interest *policy.Interest; Player func(e) (playerID int64, view policy.ObserverView, ok bool) }
func NewMapBasic(owner, host *heartbeat.Host, MapConfig{Fields, Save, SaveOnLeave, SaveEvery, SaveOptions, OnSpaceError}) (*MapBasic, error)
func (m *MapBasic) Enter(sceneID, space, at) error / Leave() error / SetPos(at) error / SetFacing(Facing) / Save() error
func (m *MapBasic) SceneID() / Pos() / Facing() / InScene() / Space() / Published() Snapshot / OnDestroy()
func NewMove(mb *MapBasic, MoveConfig{Terrain, Locate}) (*Move, error)
func (m *Move) MoveTo(path []spatial.Point, speed float64) (token uint64, err error)
func (m *Move) Follow(target int64, speed float64, keep int64, stopWhenReached bool) (uint64, error)
func (m *Move) Advance(token uint64, elapsed time.Duration) Result   // Reason 0 = 还在移动；Arrived / Replaced / Blocked / LeftScene / TargetLost / TargetLeft / Refused
func (m *Move) Stop(token uint64) bool / Moving() bool / Token() uint64
type Locator func(id int64) (Snapshot, bool); func LocateIn(get func(int64) entity.IThreadSafeEntity) Locator
func RegisterActions(registry *actionflow.Registry, moveTo, follow actionflow.ActionKind) error   // MoveToParam{Path, Speed, Period} / FollowParam{Target, Speed, Keep, StopWhenReached, Period}
type MapOwner interface{ MapBasic() *MapBasic }; type MoveOwner interface{ MoveComponent() *Move }; type MemFields struct{ OnChange func() }
```

**按要求逐条落实**：位置 `gameplay/movement`；`MapBasic` 有场景 ID、位置、朝向，位置 nopersist 只同步、`SaveOnLeave` / `SaveEvery` 由业务选落库，AOI 接线经 `Space` 接口注入：进场景 `Enter`、位置变化 `QueueMove`（事务内入队、走提交门）、离开 `Leave`。`Move` 有速度、路径 / 跟随目标、状态，被动；`MoveTo` / `Follow` 动作的组由 runner 挂心跳（缺省 100ms），每拍 `Advance(Beat.Elapsed)`、写 `MapBasic`、触发 `QueueMove`，不追帧；到达、打断、被阻挡、目标消失由 actionflow 收尾；组空闲时停心跳。`BeatRunner` 是通用的 runner 宿主，AI、技能以后用同一个 runner。回调只做内存计算与入队。

**与方案的偏差（如实记录）**：

1. **每组一个心跳登记**（参考 ssr），不是方案 §5.4 的“一个 `"actionflow"` 登记、周期取各组最小值”：一个登记按最短周期跳会让慢的动作被按快的节拍 tick、`Elapsed` 也对不上。多个组同时有需要 tick 的动作时各自投递（决定 ③ 不合并）。
2. **状态存放在业务 DAO，`MapBasic` 不自带字段**：同步内容由生成 DAO 的字段打包，所以 `MapBasic` 经 `Fields` 读写业务 DAO（与 game-demo `MapComponent` 一致），`MemFields` 只给测试 / 示例 / 不持久的 NPC。
3. **进出场景在提交之后**：`QueueMove` 有提交门，`Interest.Enter` / `Show` / `Leave` / `Hide` 没有；`MapBasic` 把它们放进 `nest.AfterCommit`（实体锁外），`InterestSpace` 进出后调一次 `Interest.Apply`。
4. **AOI 排队满不结束移动**：`policy.ErrInterestQueueFull` 是背压，整拍作废、下一拍接着走；其他写位置错误以 `Refused` 结束。
5. **整数格坐标**：`Move` 用亚格位置累积零头；朝向 `Facing` 取 1/100 度。
6. **共享模式**（适配 main `96724b6a` / `d287031f`）：组的心跳挂不上时动作以失败结束；运行中被停掉后回到独占由业务 `BeatRunner.Sync` 重新挂上；带 `SaveEvery` 的 `Enter` 在共享模式下返回错误。
7. **AI 没有接线**；`BeatConfig` 没有 MissionRunner（任务由动作结束推进，需要周期 tick 的任务另登记心跳）。
8. **PACKAGES 里 actionflow 的测试文件数**由 12 改成实际的 4。

**过程记录**：第一版（`93794491`、`345616ad`）是 Move 自己挂 `"move"` 心跳、移动动作经 Waker 由事件收尾；维护者决定 1 之后改为本节的结构（`382eb10c`），Waker、`Move.Resume`、Done 回调删除。

**偶发失败（复核发现，已修，`1008ba58`）**：`TestMoveToActionRunsOnTheMoveHeartbeatAndIsReplacedByTheNextOne` 在 `-race -count=15` 下 2/15 失败，原文 `nest_test.go:300: ended {id:1 kind:1 status:2 reason:arrived} at {Present:true SceneID:1 Pos:{X:3 Y:1} Facing:1843}`（另一种：`nest_test.go:321: at {Present:true SceneID:1 Pos:{X:3 Y:5} Facing:9000} registered 1`）。判断：**测试时序问题，不是产品缺陷**。status:2 是成功、reason 是 arrived——第一条移动确实到了终点 (3,3)，但测试读到的 `Published` 还是上一拍的 (3,1)：动作结束在拍的事务里记下，`Published` 与心跳登记的停止都在提交后、实体锁释放之后才执行（契约就是“最近一次提交”），测试看到结束就读提交后状态。证据：在 `afterCommit` 前临时插 30ms 睡眠放大窗口，修前测试 2/2 失败（`... at {... Pos:{X:2 Y:0} ...}`）、修后 2/2 通过；Nest 同实体消息等前驱作业整个结束（`dispatchNest` → `runNestLogic` 的 defer 先释放 Guard 作用域、执行提交后回调，`finishJobLocked` 才放行后继）。修法：`nestFixture.settle(e)` 经同一实体发一条空消息再断言提交后状态（不 sleep、不放宽断言）。同包排查：凡是“从拍的事务里观察到事件之后断言 `Published` / `Registered`”的地方都加了 `settle`（runner tick 用例、MoveTo 用例两处、Follow 收尾、冒烟的终点断言）；经 `Request` 返回之后断言的用例不受影响（回复在提交后回调之后）。负载下的修前 / 修后失败率见下文“验证”。

**红绿证据**：新代码，实现与测试同轮写成，红用变异核对给出（每个强制点改掉一处，对应用例在当前代码上变红，原文）：

| 变异 | 变红的用例与原文 |
| --- | --- |
| M1 推进不按 `elapsed`（固定 100ms） | `TestSkippedBeatsAreCoveredByOneElapsedAndFractionsAccumulate`：`a 300ms beat queued [move 0,1], want one move to 0,3` |
| M2 组的动作离开时不停心跳 | `TestEachGroupBeatsAtItsCurrentActionsInterval`：`idle groups: true 100ms / true 500ms registered 2`；`TestRunnerIsTickedByTheHeartbeatAndStopsWhenIdle`：`idle runner keeps its heartbeat: active true registered 1` |
| M3 每拍从格重新起算（丢零头） | `TestSkippedBeatsAreCoveredByOneElapsedAndFractionsAccumulate`：`positions at 0.3 cells per beat: [0 0 0 0 0], want [0 1 1 1 2]` |
| M4 `Advance` 不核对编号 | `TestANewMoveReplacesTheOldOne`：`advancing the replaced move: arrived 2,3, pos {X:2 Y:3}` |
| M5 事务里的进场景立即执行 | `TestEnterAndLeaveFollowTheTransaction`：`rolled-back enter: ops [enter 1,1] in scene false published {Present:true SceneID:0 ...}` |
| M6 `SetPos` 不 `QueueMove` | `TestPositionChangesReachTheAOIOnlyWhenCommitted`：`a committed move far away did not take the entity out of the watcher's view` |
| M7 MapBasic 不恢复回滚 | `TestEnterAndLeaveFollowTheTransaction`：`rolled-back enter: ops [] in scene true published {...}` |
| M8 Move 不恢复回滚 | `TestRolledBackMoveToLeavesNothingRunning`：`rolled-back move: moving true registered 0` |
| M9 跟随不判目标消失 | `TestFollowTracksTheTargetUntilItIsGone`：`target destroyed: target left the scene 13,0` |
| M10 一拍 tick 全部组 | `TestAGroupsBeatTicksOnlyThatGroupWithItsElapsed`：`ticks [first tick 1 elapsed 300ms second tick 1 elapsed 300ms ...], want [second tick 1 elapsed 300ms first tick 1 elapsed 100ms ...]` |
| M11 不填 `Elapsed` | 同上：`ticks [second tick 1 elapsed 0s ...]`；`TestRunnerIsTickedByTheHeartbeatAndStopsWhenIdle`：`tick outside a heartbeat beat: [{elapsed:0 inTx:true} ...]` |
| M12 移动动作不要周期 tick | `TestMoveToActionIsDrivenByTheRunnerHeartbeatAndReplacedByTheNextOne`：`moving entity: move group heartbeat active false, registrations 0 (want exactly the runner's one)` |
| M13 AOI 排队满按拒绝处理 | `TestRefusedPositionHoldsOnBackpressureAndEndsOtherwise`：`backpressure: position refused 0,0 pos {X:0 Y:0} moving false` |
| M14 同一事务进场景后又移动也入队 | `TestEnterAndLeaveFollowTheTransaction`：`space saw [move 2,2 enter 2,2 move 3,3 leave], want [enter 2,2 move 3,3 leave]` |
| M15 位置没变也落库 | `TestEnterAndLeaveTheScene`：`saved [3@4,4 3@4,4 3@5,4], want one periodic save and one on leave` |
| M16 共享模式下仍进场景 | `TestSharedRemoteEntityRefusesHeartbeatsAndResumesAfterLeavingSharedMode`：`enter in shared mode: <nil> in scene true` |
| M17 挂不上心跳的动作不结束 | 同上：`move started in shared mode: ended [{id:1 kind:1 status:4 reason:replaced by next action}] moving true` |
| M18 `Sync` 吞掉错误 | 同上：`runner sync in shared mode: <nil> active false` |
| M19 动作取消不停移动 | 同上：`move started in shared mode: ended [... {id:2 kind:1 status:3 reason:heartbeat: Remote entity is in shared mode ...}] moving true` |

端到端冒烟（非性能测量）：`TestSmokeThousandsOfEntitiesMoveOnTheirOwnHeartbeats`，真实 Nest + 心跳驱动 + 同步 Manager + Interest（ApplySync），3000 实体各自进场景、随机 `MoveTo`（距离 ≤ 150 格的方框、150 格/秒），64 个观察者铺满 1280×1280 地图：`3000 entities, 3000 moved, 64 watchers, 53233 position payloads pushed; heartbeat {Delivered:25437 Executed:25437 ... StoppedShared:0 Registered:0}`（与第一版 Move 自挂心跳时的 25414 拍同一量级：每个移动实体每拍一条消息）；全部动作成功、位置等于终点、每个实体至少一个邻近观察者最后收到的就是终点位置，结束后驱动上没有登记。-race 下 300 实体。冒烟里每会话对象数超过同步帧缺省上限 100，测试把 `frame.Limits.MaxObjects` 调到 4096（缺省值未改）。

**验证**：偶发失败的修前 / 修后失败率（`TestMoveToActionRunsOnTheMoveHeartbeatAndIsReplacedByTheNextOne` → 改名后的 `TestMoveToActionIsDrivenByTheRunnerHeartbeatAndReplacedByTheNextOne`）：不加负载 `-race -count=100 -run <三个经心跳等事件的用例>`，修前（`345616ad`）24/100、修后 0/100；加负载（后台循环 `go test -race -count=5 ./framework/sync/entitysync/policy/`）`-race -count=50 ./gameplay/movement/ ./gameplay/actionflow/`，修前 1/50（该用例，2850 次顶层用例里 1 次失败），修后 0/50（2851 次全部通过，退出码 0）。负载复现时 nestbatch 的正式对比在跑，它的脚本在检测到外来 go 进程时作废并重跑那一轮，所以不影响它的数据、只会推迟。gofmt、`GOWORK=off go vet ./...`、`go test -race -count=1 ./gameplay/... ./framework/sync/... ./wiring/...`、根包 `go test -count=1 .`、全量 `go test -count=1 ./...` 的结果见本轮交付报告（全部退出码 0 才提交）。

### P3 实施结果（2026-10-21）

**状态：P3 已实施**（分支 `feat/d20-p3-demo`，基于 main `53cc3f97`，本地提交、未推送）。改动只在 `demo/`（手写参考实现，不迁到生成器）、`codegen/internal/roost/demo.go`（demo 的写文件步骤表，新模板必须登记）与 C# 客户端 `client/dotnet`；**没有改框架包**。说明见 [WALKTHROUGH“移动”“怪物”“场景时间”“刷新”](../../demo/WALKTHROUGH.md#移动客户端发目标服务端在实体自己的心跳里走d20) 与 [guide/10-time §4B“参考接法”](../framework/guide/10-time.md#4b-移动与动作心跳驱动)，剩余边界 [KNOWN-LIMITS“game-demo 的移动、怪物与场景时间”](KNOWN-LIMITS.md#game-demo-的移动怪物与场景时间d20-第-3-期)。codebase-memory 图的根是主检出、generation 早于 D21 / D22，demo 是 `.tmpl` 不进图，结构结论以 worktree 源码与生成工程为准。

**按要求逐条落实**：

1. **心跳接线**：`game/actor.Heartbeat`（包级驱动）在 `internal/service/<game>/heartbeat.go` 手写 `Bind`（Nest 客户端 + 只读实体查找）并 `Start`，`Service.Init` 里早于 World 与场景，`Shutdown` 一开始就停（Nest 仍在）；与 World 定时器驱动同一种写法，不装 `wiring/heartbeat` Mod。
2. **玩家移动（决定 ⑭）**：新协议 `MoveTarget`（msg 10026，`{X, Y, Follow}` → `{Code, Reason, ActionID, X, Y}`），handler `handlerMoveTarget`（场景 + 玩家）：找一次路，最后一步 `Behavior.CreateAction(MoveTo / Follow)`；每拍由玩家自己的移动组心跳 `Move.Advance(Elapsed)` → `MapBasic.SetPos` → DAO + `QueueMove`，观察者经 Entity Sync 看到每一步。寻路预算（4096 节点）用完时走直线，`Move` 按 `actor.Ground` 每格检查、被挡停下。原按点移动（msg 10017）保留为瞬移：`MapComponent.Teleport` 校验地形、`SetPos`、最后结束正在走的路；端点不再调 `Scene.Moved`（删除）。
3. **位置不每拍落库（决定 ⑮）**：`PlayerDao.PosX/PosY/SceneID/Facing` 改 `nopersist,sync`，新增持久的 `SavedSceneID/SavedPosX/SavedPosY`，由 `MapBasic` 的 `SaveOnLeave` 与 `SaveEvery: 10s`（`"map.save"`）写，进场景以它为首选点。未上线、不兼容：旧文档的 `pos_x/pos_y/scene_id` 不再读。
4. **怪物**：`ActorComponent`（新组件 2101）挂 `MapBasic` / `Move` / `Behavior` / `ai.NewBeatController`（策略 `actor.Patrol`：移动组空闲时走向下一个角）/ `state.Set` + `state.Gate`（`Rooted` → 移动组 `FreezePause`）/ `CombatComponent.BindStates`（`root` 标签派生 `Rooted`）；`Rooted` 经状态观察者镜像进 `MonsterDao.Rooted`（`nopersist,sync`）给客户端；GM `gm.scene.root`（`seconds` 给了是限时定身）。Buff 用 P5 的 `combatcomponent.AttachBeat` 挂心跳（`"combat"`，1Hz，`actor.SkillClock` 100ms 一 tick，跟随场景时间域），限时定身到点由怪物自己的心跳移除（协调方 2026-10-21 要求：有 Buff / 技能就用 `AttachBeat` / `NewRuntimeBeat`；demo 实体上没有技能 Runtime，所以没有 `NewRuntimeBeat`）。没做追击 / 回巢（可选项，未做）。
5. **刷怪（P5 可选项）**：见上表“已做”一行。先后顺序写在 `spawner.go` 类型注释与 WALKTHROUGH“刷新”：① `lifecycle.Create`（AI 无策略、不跑）→ ② 放身体（可见前唯一一次写）→ ③ `ShowSubject`（先 Register 再 Show）→ ④ `handlerSpawnMonster`（`MapBasic.Enter` + 开始巡逻，提交后心跳才开始，所以第一步在可见之后）→ ⑤ `Refresh.Spawned`；任何一步失败逆序撤销、下一拍重来。一次只建一批（`busy`），否则还没 `Spawned` 的请求会被下一拍再交一次。
6. **场景时间域**：场景实体新增 `SceneDao`（`nocoll`，三个字段 `nopersist,sync`）与 `timedomain.Domain`（倍率 [0, 4]）；玩家、怪物的 `Behavior` 与 AI 以 `BeatConfig.TimeDomain: actor.WorldTime()` 构造。GM `gm.scene.time_rate` → `handlerSetSceneTimeRate`（场景锁内 `SetRate`）。场景自己是复制主体（`sync=true`），复制桥把它放进关系视野 `sceneView`，每个 `Join` 挂上；`MaxPlayersPerView` 调到 65536。C# 客户端新增 `Roost.Client.SceneTimeDomain`（`Update` / `At` / `Now`，与服务端 `State.At` 同一公式、单调托底）与自测。
7. **loadtest**：新动作 `move_target`、`move_arrive`（等自己的复制位置到达目标），场景脚本在 `move_out_of_bounds` 之后走 30 格。

**与要求的偏差（如实记录）**：

1. **“副本场景”是 demo 唯一的世界场景**：demo 没有副本场景实体（副本是 session 服务的 run），时间域挂在世界场景上；`actor.WorldTime` / `actor.Ground` 是进程级的，成员构造时绑定（多场景的写法见 KNOWN-LIMITS）。
2. **场景时间下发走关系视野**：场景不是空间主体，没有 `Show` 进 AOI；每个成员挂场景视野。代价是一个视野扇出到全场景（上限 65536）。
3. **`actor.InterestMoves` 的 `Enter` / `Leave` 是空操作**：进出场景（复制主体、会话、Show / Hide）仍由复制桥负责（它处理重连代际、退役中的主体），`MapBasic` 只报告移动；对兴趣政策还不认识的实体（`Join` 之前）忽略 `ErrInterestUnknown`。
4. **demo 的玩家不调 `LeaveScene`**：断线 / 卸载不经 Nest 离开场景，`SaveOnLeave` 不触发，存档位置靠 10 秒周期落库（KNOWN-LIMITS）。
5. **改了 `codegen/internal/roost/demo.go`**：只是 demo 的写文件步骤表（新模板必须有一步写它，`TestDemoTemplateStepsAndShippedFilesAgree` 钉住），另加 `Scene` 的 DAO 与怪物 `Actor` 组件两个 `add` 步骤；不涉及生成器心跳装配。
6. **测试的组件状态在实体锁里读**：第一版在测试 goroutine 里直接读 `Move.Moving()` 等，`-race -count=20` 报 4 处 data race（测试读法问题，产品代码在锁内）；改为经一个测试 handler 在实体锁里读（`locked`）后 `-race -count=20` 全过。

**红绿证据**：新功能，实现与测试同轮写成，红用变异核对给出（每个强制点改掉一处，在生成工程里重新生成后跑对应用例，原文）。端到端用例在 `internal/service/<game>/scene_movement_test.go`：真实 Nest 引擎 + 生成的 handler + 运行中的心跳驱动 + 复制桥（Manager + Interest），业务时钟换成手动推进的（`clock.SetSource`）——驱动仍按真实时间醒来，时钟不动时每拍推进 0，推进一步时恰好由下一拍走完，所以“走了几格”是确定的数、“不动”可以不靠 sleep 断言；提交后的状态先 `settle`（同实体一条空消息）、组件状态在实体锁里读。

| 变异 | 变红的用例与原文 |
| --- | --- |
| M1 `InterestMoves.QueueMove` 不排进兴趣政策 | `TestAPlayerWalksToItsGoalOnItsOwnHeartbeatAndTheWatcherSeesTheSteps`：`the walk brought the walker within 100 cells of player 9703, which still does not see it at {600 500}: {}`（第一版用例只看近处观察者，这个变异没变红——近处观察者本来就订着，位置靠同步增量到达；补了“走进远处玩家视野”这一段之后变红） |
| M2 瞬移不结束正在走的路 | 同上：`after a teleport: at {450 450} walking true heartbeat true, want {450 450} and no walk` |
| M3 玩家 `Behavior` 不挂场景时间域 | `TestATwiceAsFastSceneWalksTwiceAsFarAndTellsItsMembers`：`the 2x step: never happened; last saw {300 360}`（1 倍速只走了 30 格） |
| M4 门控不加 `Rooted` → 移动组规则 | `TestARootedMonsterStopsWhereItIsAndWalksOnWhenFreed`：`rooted: move group frozen false, heartbeat active true; want frozen and not beating` |
| M5 场景的刷怪心跳不启动 | `TestTheScenesBeatRefillsThePopulationAndEachMonsterPatrols`：`the scene's beat fills the table's population: never happened; last saw 0` |
| M6 `Spawn` 不进场景的移动（`MapBasic.Enter`） | 同上：`monster 2323435195258110993 is counted but not on the map` |
| M7 玩家的 `Move` 不带地形 | `TestAPlayerWalks…`：`the straight walk stops at the wall: never happened; last saw {492 492}`（穿墙继续走） |
| M8 寻路预算用完不改走直线 | 同上：`scene_position: invalid scene position [reason=no route to_x=700 to_y=700]: spatial: path search budget exhausted` |
| M9 `Join` 不挂场景视野 | `TestATwiceAsFast…`：`the member never received the scene's own subject; got subjects [9944068]` |
| M10 巡逻不看移动组是否有当前动作 | `TestPatrolStartsTheNextCornerOnlyWhenTheMoveGroupIsIdle`：`a walk in progress was replaced: next 2` |
| M11 刷怪心跳用墙钟而不是 `Beat.Now` 判重生 | `TestTheScenes…`：`the slot is refilled after the delay: never happened; last saw 2` |
| M12 限时定身不带时长（按永久施加） | `TestARootedMonster…`：`a timed buff is on but the monster's buff heartbeat is not running` |
| M13 施加定身用 tick 0 而不是 `CurrentTick()` | 同上：`two buff beats: never happened; last saw 1`（buff 第一拍就过期、心跳随之停） |
| C1 C# `SceneTimeDomain.At` 不乘倍率 | `scripts/test-client-dotnet.sh`（`ROOST_DOTNET_GOLDEN_ONLY=1`）：`Unhandled exception. System.Exception: 2x doubles the business time after the anchor` |

没变红、如实记录的两个：① 怪物 AI 心跳不挂时间域（`ai.BeatConfig.TimeDomain` 去掉）——没有用例变红：巡逻策略只看“移动组是否空闲”，不读时间，demo 里域时间只影响 AI 的 `FreezeFor` 等到期判断（demo 不用）；怪物走路的加速由 `Behavior` 的域保证（与 M3 同一路径）。② 巡逻里的 `Frozen` 判断是兜底：冻结的组本来就拒绝 `CreateAction`，去掉它行为不变。刷怪“提交后才交出”（`AfterCommit`）与“一次只建一批”（`busy`）没有确定的观察点（拍不会回滚、两批相隔 1 秒），没有变异用例，靠代码与注释。

**验证**（rebase 到 main `6576b7fb` 之后重跑；再 rebase 到 `ddeeffa3`（P5 技能 / Buff 心跳、Remote 共享模式验证）并给怪物接上 `AttachBeat` 之后，生成工程的 build / vet / `-race ./...` / 心跳用例 `-race -count=20` 与 codegen demo 用例重跑，退出码均为 0，仓库全量未再重跑；生成工程 = `go run ./codegen/cmd/roost project new planet -module example.com/planet -template game-demo --skip-deps` + `replace` 指回 worktree，`GOWORK=off GOFLAGS=-mod=mod`；退出码直接读）：

- 生成工程：`gofmt -l .` 无输出；`go build ./...`、`go vet ./...` 0；`go test -race -count=1 ./...` 0（26 个包）；心跳相关用例（4 个端到端 + 巡逻单测）`-race -count=20` 0。另把 `actor.Heartbeat` 临时改成 `Delivery: heartbeat.DeliveryBatch`（选项 C 的批量作业，不提交）跑 4 个端到端用例 `-race -count=3`，0。
- 仓库：`GOWORK=off go vet ./...` 0；根包 `go test -count=1 .` 0；`scripts/test-sync-modes-generated.sh` 0；`ROOST_DOTNET_GOLDEN_ONLY=1 scripts/test-client-dotnet.sh` 0；`go test -count=1 -run 'TestDemo|TestGameDemo' ./codegen/internal/roost/` 0。
- 仓库 `go test -race -count=1 ./gameplay/...` 与全量 `go test -count=1 ./...` 各 1 个失败、都是同一个用例：`gameplay/heartbeat/internal/bench` 的 `TestBusyShortPoolSkipsBeatsAndResumes`（main 上 P4 新增，`cf2bd7d2`），原文 `driver_test.go:436: entity 2048010029 ran 1 beats after the pool was released, want it to resume`。本分支没有改 `gameplay/`，该包代码与 main 相同；单独 `-count=10` 全过，失败时宿主 load 12～13（全量并行）。判断：该用例在宿主负载下的时序敏感，**尚未确认**是否只是口径问题，留给 P4 一方，本轮不改。
- 基线（rebase 之前、改动之前的 main `53cc3f97` 生成工程）`go test ./...` 有一次 `TestAStalledOldConnectionDoesNotResetTheLiveClient` 失败（`scene_connections_test.go:279: a push the live connection took was reported as failed: … outbound queue full`），之后各次运行都通过；与本轮改动无关，**尚未确认**，记在这里。

**冒烟（待补测，2026-10-21 因宿主资源占用推迟）**：维护者 2026-10-21 要求本轮不跑 loadtest / robot 冒烟与任何压测，loadtest 只要求编译与单元测试通过（已通过）。复跑命令（生成工程目录里，依赖用 `scripts/mirror-local.sh` 起的私有环境或 README 的隔离环境，把配置改指向它）：

```bash
make dev-run                       # 十个进程 + accountctl 登记 sid 1000
make dev-smoke                     # 两个机器人走完整条链，含 move_target → move_arrive
make loadtest LOADTEST_COUNT=20    # 冒烟规模，不是压测
make dev-stop
```

### P4 实施结果（2026-10-21）

**状态：代码已实施，没有测量**（分支 `feat/d20-p4-perf`，基于 `53cc3f97`；本地提交，由维护者推送）。维护者 2026-10-21 要求先暂停压测、把代码收尾；同日宿主上有别的耗资源程序，冒烟也不跑，只做单元测试与编译检查。开关、口径、已做验证与复跑命令见 [HB-DRIVEN-LOAD-20261021](../performance/HB-DRIVEN-LOAD-20261021.md)，剩余边界见 [KNOWN-LIMITS“压测工具的心跳驱动”](KNOWN-LIMITS.md#压测工具的心跳驱动d20-第-4-期)。

**落实**：

1. **sync-aoi**（`3c5e41e3`）：`-move-driver=loop|heartbeat`（另 `-move-period` 缺省 100ms、`-move-speed` 缺省 30 格/秒、`-movers` 缺省按改值速率折算）。heartbeat 模式每个实体装 `heartbeat.Host` + `movement.MapBasic` / `Move` + `actionflow.BeatRunner`（memory 级别），移动者在原往返带上 `MoveTo` 随机目标巡逻、到达后在 `OnEnded` 里接下一段（经 runner 延后队列，handler 最后一步才调 runner），位置经 `MapBasic.SetPos` → `QueueMove` 进 AOI；压测端循环只设帧信封、`Apply` 与等 tick。移动者数 = `ceil(每 tick 改值数 × 20/s × 档位)`，缺省参数 200 个、每秒 2000 次改值，与全局循环相同。输出区分：`move_driver` 与 `server.json` 的 `heartbeat` 一节（驱动计数、心跳间隔、巡逻段数、动作失败）；心跳模式额外门槛是零跳拍、零动作失败。
2. **nest-game**（`22b01614`）：`ROOST_NEST_GAME_HEARTBEAT_DRIVER=ticker|heartbeat`；heartbeat 下每个生成 Unit 一个 `1s / HZ` 档位的登记，回调执行同一个 `HeartbeatComponent.Tick`（计数与 Sync 接线同 ticker），帧回调只投普通业务；核对改为投递 = 执行、零跳拍、每 Unit 拍数 `时长 / 档位 ± 2`、DAO 等于按拍数算出的改值次数。`nest-game.sh --check-heartbeat` 跑接线短测。
3. **§7 微基准**（`cf2bd7d2`）：`BenchmarkWheelAdvance`、时间轮 / 最小堆、`Dispatch` / `DispatchBroadcast` 三项在 HB-STRUCTURES 时已有（`BenchmarkSched`、`TestDeliveryRun`），不重复；新增正式驱动的 `BenchmarkDriverDispatch`（假客户端）、`TestDriverOpenLoop`（真实 Nest + 空回调开环，`HB_OPENLOOP=1`，缺省跑 0.3 秒小档断言）、`TestBusyShortPoolSkipsBeatsAndResumes`（快池占满时在途 / 队列满跳拍、恢复后报告跳过数，常跑）。
4. **D19 多视野 Nest 模式复测**：命令已写进报告 §4，未跑。
5. **投递层开关**：选项 C 的 `heartbeat.delivery` 在本分支 rebase 时还没有合入 main，工具没有透传；合入后在 sync-aoi / nest-game 加一个对应开关即可。

**缺省的选择（需要维护者决定）**：两种工具缺省都保留原全局循环——A 保持到 v1.31 发版前的正式对比做完再定（推荐：已有报告的复跑命令不加开关时得到原负载，心跳模式的数字还没有对照）；B 现在就改为心跳驱动（符合“替代全局循环”的原话，代价是所有旧复跑命令都要补 `-move-driver=loop` / `ROOST_NEST_GAME_HEARTBEAT_DRIVER=ticker`）。

**红绿**：新代码，红用变异核对（原文）：sync-aoi `loadSpace.QueueMove` 不入队 → `TestHeartbeatPatrolReachesClientsAndTheAOI`：`player 1784 at {X:222 Y:200} sees [2808 5880], want the entities within 40 at their final positions [2808 5880 6904]`；写位置不标同步脏 → 同一用例 `client latency samples 2, of which deltas 0: heartbeat changes did not reach visible clients as updates`（原断言只要求有样本时这个变异是绿的，因为进出视野的全量也带样本，已改为同时要求增量样本）；客户端心跳模式改回按 Step 逐项核对 → `TestClientVerifiesThePatrolBandPerMoveDriver`：`heartbeat at {X:397 Y:200} step 3: accepted false, want true`。nest-game 回调不调 `Tick` → `TestHeartbeatDriverTicksGeneratedUnits`：`unit 0: 10 beats should change 10 times, DAO heartbeats 0 hp 0`；登记不启动 → `delivered 0 executed 0`。`TestBusyShortPoolSkipsBeatsAndResumes` 是对 P1 已实施规则（决定 ④⑤）的契约检查，P1 的 M2 已给出“不看在途”的红。

**过程中修的夹具问题**：sync-aoi 的压测 handler 原为包级 `MustRegisterMemoryHandler`（在 `newLoadNest` 里），同一进程起第二个引擎（`go test -count>1`）会 `duplicate handler` panic，改为引擎实例级注册（meta 相同）；端到端用例里客户端 goroutine 读结果 map 的数据竞争（测试自身）已修。根包 `TestGeneratedProjectRealDependencyTestsHaveAnEntry` 要求生成工程夹具里的新测试有运行入口，修前原文 `TestHeartbeatDriverTicksGeneratedUnits (codegen/internal/entity/testdata/nestgame) needs real dependencies but no entry runs it`，已在 `generatedRealDependencyElsewhere` 登记为 `nest-game.sh --check-heartbeat`（`57b0665b`，根目录 `integration_coverage_test.go`，超出本任务的改动范围，只加一行登记）。

**未做**：任何冒烟与正式测量（报告 §4）；`TestNestGameLoad` 心跳路径的运行；`heartbeat.delivery` 透传。

**合入后补修（2026-10-21，协调方）**：`TestDriverOpenLoop` 的小档接线断言在 main 上偶发失败（`-race`、宿主 load 12～15 下 `-count=20` 失败 5 次），两处是用例口径错，不是产品缺陷：① “投递数 ≥ 执行数”用的是测量窗口内的差值，窗口起点取在拍子在途时，窗口前投递、窗口内执行的拍只进 Executed（失败样本 `Delivered:586 Executed:601`）——改为比较驱动的累计计数（结果新增 `driver_totals`）；② 在途跳拍与墙钟间隔 p50 量的是宿主调度（跳拍 100～184 次、p50 63～100ms，驱动本身被拖晚、不记为跳拍），不属于接线断言——跳拍只记日志（由 `TestBusyShortPoolSkipsBeatsAndResumes` 覆盖），周期改为断言每拍自带的 `Beat.Period` 等于登记周期（确定性；结果新增 `wrong_period_beats`，变异“比较 2×周期”变红：`beats carried a period other than the registered 50ms`）。墙钟间隔仍记在结果里，零跳拍与间隔分布是正式开环测量（`HB_OPENLOOP=1`）的结论。修后同负载 `-race -count=60` 通过。

### P5 实施结果（2026-10-21）

**状态：P5 的技能与 Buff 两项已实施**（分支 `feat/d20-p5-tick`，代码 `a7fe9328`；本地提交，由维护者推送）。spawner 一项归 P3，不在这里。用法 [guide/10-time §4E](../framework/guide/10-time.md#4e-技能与-buff心跳驱动d20-第-5-期)，契约与变异核对 [impl/10-time §2G](../framework/impl/10-time.md#2g-技能与-buff-的心跳)，剩余边界 [KNOWN-LIMITS“技能与 Buff 的心跳”](KNOWN-LIMITS.md#技能与-buff-的心跳d20-第-5-期)。没有新指标。codebase-memory 图的根是主检出、不是本 worktree，结构结论以 worktree 源码为准。

**API 最终形状**：

```go
// gameplay/skill（纯运行时，不依赖 Nest）
type TickClock struct{ Length time.Duration; Origin time.Time }    // tick = floor((t − Origin) / Length)；Origin 零值 = Unix 纪元
func (c TickClock) TickAt(time.Time) Tick / StartOf(Tick) time.Time / Ticks(time.Duration) Tick / Validate() error
func (r *Runtime) CurrentTick() Tick / Pending() bool
// gameplay/skill/combat
func (c *BuffContainer) NextDue() (int64, bool)
// gameplay/skill/combatcomponent
func (c *CombatComponent) AttachBeat(BeatConfig{Host, Clock, Name, Granularity, Options, TimeDomain, Periodic, OnBeat, OnError}) error
func (c *CombatComponent) BeatReg() *heartbeat.Reg / CurrentTick() int64 / SyncBeat() error
type BuffBeat struct{ heartbeat.Beat; Tick int64; Expired []combat.BuffInstance }
func NewRuntimeBeat(*skill.Runtime, RuntimeBeatConfig{Host, Clock, Name, Period, MaxCatchUp, Options, TimeDomain, OnError}) (*RuntimeBeat, error)
func (b *RuntimeBeat) Start(program, input) / Input(cast, port, input) / Do(func(*skill.Runtime) error) / CatchUp() / Sync() / Tick() / Runtime() / Reg() / Stats()
```

**换算规则**（impl §2G 第 1、4 条）：tick 长度由业务的 `TickClock.Length` 定（[1ms, 1s]），向下取整（tick k 覆盖 `[Origin + k·L, Origin + (k+1)·L)`），`Origin` 缺省 Unix 纪元（tick 绝对、可存进 DAO）。Runtime 忙时**逐 tick** 推进（每个 tick 调一次 `Advance(t)`），推进中一旦空闲就停在空闲点、停心跳；空闲时拍不推进，handler 的输入一次跳到输入的 tick。一拍最多逐 tick 推进 `MaxCatchUp`（缺省 `max(1s, 周期)`，挂域时乘倍率）换算出的 tick 数，超过的剩余一次 `Advance(target)`，计 `Stats().Jumps` 并限频 Warn。时刻倒退不动。挂时间域时 `Beat.Now` 与 handler 的时刻都是域时间：时停 tick 不前进、2 倍速走两倍。

**与回放、确定性的关系**：`MemoryHost.Advance` 每次推进都发 `tick_advanced`、`revision++`、按 tick 过期状态，“一次推到 N”与“逐个推到 N”结果不同，所以忙时必须逐 tick；空闲点由 Runtime 状态决定（而不是拍的时刻），空闲之后由输入跳。由此在没有 `Jumps` 的区间，**同一输入序列（每个输入落在同一 tick）在任何心跳抖动下逐 tick 相同**——`TestRuntimeBeatResultsDoNotDependOnHeartbeatJitter` 用每 tick 一拍、每两 tick 一拍、10～400ms 随机（含重复与晚到的拍）、600～990ms 长间隔四种抖动各 5 个种子，比较 Host 每次被推进时的记录（tick、revision、目标生命、施法者法力）与每次输入的结果，全部相同。回放记录输入的 tick（`RecordingHost` 带 tick），不记拍；发生过 `Jumps` 的区间回放要在同样的 tick 跳。

**按要求逐条落实**：

1. **Buff 心跳**：`AttachBeat` 登记一个停止状态的 `"combat"` 心跳；有限时 buff（`DueTick != 0`）或业务的 `Periodic()` 为真时自动启动，周期 `heartbeat.PeriodAtMost(Granularity)`（缺省 1s = 1Hz）；每拍 `TickBuffs(TickAt(Beat.Now))` 后调 `OnBeat`，全部清空且 `Periodic` 为假时停。启停随事务（施加限时 buff 的 handler 回滚，心跳不启动）。`OnInitFinish` 按加载出的 buff 启动，**停服期间到期的记录在加载后的第一拍清掉**（`OnInitFinish` 不在事务里，不能在那里改 DAO）。可选 `TimeDomain`，buff 时长按域时间算。周期效果、冷却、回复不在组件里：组件只持有 buff，业务的周期工作经 `Periodic` / `OnBeat` 挂在同一个心跳上（`SyncBeat` 重新对齐）。
2. **保留 `TickBuffs`**：项目没上线、不做兼容，但它是“推进一步”的原语（与 `Runtime.Advance` 对称）——buff 心跳的拍就是调它；不在 Nest 快池上的驱动（Lockstep 房间的定步长循环）与 `status_bridge_test` / `state_bridge_test`（测原语本身与派生状态的通知）需要它。删掉它只能换成一个同样的未导出函数加测试钩子，不更简单。改了一处：没有到期 buff 时直接返回、不登记逆操作（每拍不复制容器）。
3. **Skill 心跳**：`NewRuntimeBeat` 登记一个停止状态的 `"skill"` 心跳（周期缺省 `PeriodAtMost(Length)`），`Runtime.Pending()`（活动施法、排程任务、逐 tick 推进的衍生物、未用尽重试的待停止衍生物）时启动，空闲时停；每拍按上面的规则推进。handler 改 Runtime 一律经 `Start` / `Input` / `Do`（先推进到输入的 tick，再执行，最后对齐心跳）。拍里的 `Advance` 错误交给 `OnError`、不让本拍回滚（Runtime 不在回滚域里）。
4. **夹具和使用方**：仓库里没有快池上手写周期消息调 `TickBuffs` / `Runtime.Advance` 的调用方——game-demo、生成器、`scripts/testdata` 都没有（`rg` 核对）。直接调用的只有：`combatcomponent` 的 `status_bridge_test` / `state_bridge_test`（原语测试，保留）、`skillsync` 测试与 `gameplay/skill/examples`（纯 Runtime，不在 Nest 上，保留）；`scripts/testdata` 玩法夹具的 `s.Advance(f.Now.UnixMilli())` 是对局按期限推进（entitytimer 驱动），按 P5 表保持原样。`demo/` 没有用到本轮改的 API（只用 `skill.DefaultCompileEnvironment`）；生成器的 `render_workflow_docs.go` 第 613 行“运行时边界”只提到 HostAdapter，没有提心跳入口，归生成器 agent。
5. **放在 `combatcomponent`**：`gameplay/skill` 是不依赖 Nest 的纯 DSL 运行时（只 import 标准库、`combat` 与 metrics），`examples` 与 `skillsync` 测试在 Nest 之外用它；`combatcomponent` 本来就是“技能 / 战斗接进 roost 实体”的那一层，已依赖 nest / entity。`TickClock` 是纯换算，放在 `skill`。

**与方案的偏差（如实记录）**：

1. **Buff 粒度固定，不按“下一个到期”自适应**：周期只按 `Granularity` 取档，短 buff 多时由业务调小；没做“离到期越近跳得越快”的自适应换档（每次施加 / 每拍都可能 `SetPeriod`，收益是短 buff 的精度）。
2. **追帧超过上限时跳，不无限补**：方案只写“追帧上限”；超过上限的部分一次 `Advance(target)`（到期任务仍按 tick 顺序执行，只合并 Host 的中间 tick），而不是留到下一拍继续逐 tick 补（那样确定性完全保住，但过载时落后无界增长）。
3. **忙时每个 tick 都推进 Host**：没有“只在到期点推进”的稀疏推进（Host 的 tick 序列会随拍变化，见上）；长时排程任务（30 秒后的弹药回充）期间每 tick 一次空推进。
4. **`Pending` 的“活动施法”一项没有强制点证据**：变异 P1（去掉这一项）没有让用例变红——测试程序在施法活动期间总有排程任务。保留是保守的，写进 KNOWN-LIMITS。
5. **`gameplay/skill` 的 `TickClock` 测试与 `Pending` 测试放在 `scheduler_test.go`**（推进主题），combatcomponent 的两组新测试是新主题、新文件（`beat_test.go`、`runtime_beat_test.go`）。

**红绿证据**：新代码，实现与测试同轮写成，红用变异核对给出（17 项各改一处强制点，对应用例在当前代码上变红；原文全文在 impl §2G 的表里）。摘要：R1 拍直接一次推到目标 → `every other tick seed 1: per-tick results differ`；R2 拍推进空闲的 Runtime → `a beat on an idle runtime moved it to 60 (<nil>)`；R3 推进不停在空闲点 → `after the cast: active false tick 40, want stopped at the idle point 8 (windup 3 + recovery 2 from tick 3)`；R5 没有追帧上限 → `100 ticks behind: {Steps:110 Idle:0 Jumps:0 JumpedTicks:0} at tick 110, want 20 more steps, one jump over 80 ticks, at tick 110`；B1 改 buff 不对齐心跳 → `a timed buff did not start the beat`；B3 拍的 tick 早一个 → `a beat 1ns before tick 1005 expired the buff`；B6 buff 心跳不挂域 → `paused scene: 1 expired, 0 active, want the buff to stay`；T1 换算向零截断 → `TickAt(origin-1ns) = 0, want -1`。实现过程中第一版有一个真缺陷被抖动测试抓到：handler 的追帧推进到空闲点就停（与拍同一规则），输入落在空闲点而不是输入的 tick——长间隔抖动下 `tick 31 applied at 18: cast 0 err skill: skill is on cooldown`；改为输入在空闲点之后照常一次跳到输入的 tick。

**两种投递方式**：rebase 到选项 C（`heartbeat.delivery`）之后，走运行中驱动的 7 个用例（`TestTimedBuffsBeatUntilTheyExpire`、`TestARolledBackBuffDoesNotStartTheBeat`、`TestLoadedExpiredBuffsAreClearedOnTheFirstBeat`、`TestPeriodicWorkKeepsTheBuffBeat`、`TestBuffsStopExpiringWhileTheSceneIsPaused`、`TestRuntimeBeatDrivesTheRuntimeOnTheRealHeartbeat`、`TestSetBuffDueTickAlignsTheBeat`）经 `forEachBeatDelivery` 在 `dispatch` 与 `batch` 下各跑一遍；用例只断言契约（启停、到期、空闲后停），不断言批量作业特有的计数，没有单独核对 batch 方式确实走了批量路径（驱动 `Stats` 没有批量计数，按配置选择）。

**断言方式**：真实 Nest 用例经同一实体的一条空消息等前一拍收尾（`settle`）再读提交后状态，登记的 `Active` 与 buff 列表在实体锁内读，不 sleep 断言；等待拍里的事件用带期限的轮询（只观察，不作断言）。

**验证**：gofmt；`GOWORK=off go vet ./gameplay/... ./wiring/...`；`GOWORK=off go test -race -count=30 ./gameplay/skill/combatcomponent/`（心跳相关用例，35s，退出码 0）；`GOWORK=off go test -race -count=1 ./gameplay/... ./wiring/...`；根包 `GOWORK=off go test -count=1 .`；全量 `GOWORK=off go test -count=1 ./...`；`go run ./cmd/glsvet ./gameplay/skill/ ./gameplay/skill/combatcomponent/` 无提示。退出码见本轮交付报告（全部为 0 才提交文档）。没有改生成工程用到的东西，没跑 `scripts/test-*-generated.sh`。

**待拍板**（先按推荐做了）：① 技能心跳的位置：A 放 `combatcomponent`（现状，推荐：`skill` 保持不依赖 Nest）；B 新子包 `gameplay/skill/skillbeat`。② Buff 粒度：A 固定 `Granularity`（现状，推荐：简单、可预期）；B 按下一个到期自适应换档。③ 超过追帧上限：A 剩余一次跳过、计数 + Warn（现状，推荐：落后有界）；B 留到下一拍继续逐 tick 补（确定性完全保住，过载时落后无界）。④ `TickBuffs` 保留为导出的原语（现状，推荐）；B 改为未导出、测试经钩子调用。

### 生成器接入实施结果（2026-10-21）

**状态：已实施**（分支 `feat/codegen-heartbeat`，开工基于 `53cc3f97`、交回前 rebase 到 `ddeeffa3`（含 P4、选项 C、P5、Remote 验证）；提交 `237d9cd2` 命令与装配、`33ffecb4` 配置段、`ce7bae9e` 端到端两种投递方式、`c7064b47` 技能 / Buff 心跳入口的文档；本地提交，由维护者推送）。维护者 2026-10-21 要求生成工程开箱就能用 D20～D22 的能力、尽早收尾。用法 [CODEGEN_REFERENCE“roost add heartbeat / behavior / timedomain”](../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#roost-add-heartbeat--behavior--timedomain实体心跳行为与时间域)，实现 [impl/12-codegen §2E](../framework/impl/12-codegen.md#2e-roost-add-heartbeat--behavior--timedomain)，边界 [KNOWN-LIMITS“生成器接入心跳、行为与时间域”](KNOWN-LIMITS.md#生成器接入心跳行为与时间域)。只改了 `codegen/`、`scripts/`（新脚本与夹具）和文档，没有改框架包。

**命令与生成结构**（生成部分 project sync 重写，业务文件只在缺失时创建）：

| 命令 | 生成部分 | 业务文件 |
| --- | --- | --- |
| `roost add heartbeat --entity E` | `game/heartbeat/driver_gen.go`（工程唯一的 `Driver`）、`E/heartbeat_component_gen.go`（宿主、`Register` / `Beat`）、bootstrap 的 `kitheartbeat.NewMod` | `heartbeat_beats.go`（`onHeartbeatInit`，示例登记停止状态） |
| `roost add behavior --entity E [--ai] [--state]` | `E/behavior_component_gen.go`（`Behavior` 接宿主；`--ai` 时 `ai.NewBeatController` 作 Observer） | `behavior_actions.go`（种类 / 组 / 注册表 / 冲突 / `timeDomain()` / `onBehaviorReady` / 示例 `ExampleAction`）、`--ai` 的 `behavior_strategy.go`、`--state` 的 `states.go`（Catalog）、`game/handler/E_behavior.go`（示例 handler） |
| `roost add timedomain --entity S` | `S/timedomain_component_gen.go`（`timedomain.New` + DAO 适配） | `timedomain_config.go`（倍率范围）；DAO 定义 add 时加三个 `nopersist,sync` 字段 |

**装配决定：按需装配**（与 entitytimer 相同）。依据“开箱可用、不用的实体零开销”：实体侧本来就零开销（没有登记、登记都停着的实例不占调度；生成的示例登记是停止状态，AI 不设策略不跳），所以“开箱可用”由 add 命令保证——它把 `heartbeat` 加进实体所在服务的 `mods`，bootstrap 随之装上，不需要手工接线。不缺省装进新工程，因为进程侧有固定开销：驱动 goroutine 空转也每 10ms 走一格，多一个 Mod 让 `shutdown.total_timeout` 多 3s 保底（实测新工程 game 服务 74s → 77s、k8s 宽限期 79 → 82s）。`heartbeat` 做成 catalog Mod（依赖 nest），配置段、doctor 的配置检查、停机预算与 `roost add mod heartbeat` 都走 Mod 的现成路径。

**配置**：生成配置的 `heartbeat:` 段来自 `kitconfig_gen.go` 里 Mod 的配置声明（`modConfigSection`）。rebase 到 `6576b7fb`（选项 C 已合入）后在 `wiring/internal/configschemagen` 加了 `heartbeat` 组，生成配置（开发、生产示例、k8s Secret 示例）写 `heartbeat:\n  delivery: dispatch`；add 命令给已有服务配置补这一段（`appendModConfigSections`），重复 add 不重复写。红：快照里去掉 `heartbeat` 组时 `TestAddBehaviorGeneratesOwnedPartsAndBusinessFiles` 变红（返回的路径里没有三份配置文件）。

**红绿证据**：修前（`53cc3f97` 的 `roost`，新工程加 Monster 实体后）原文：`roost add heartbeat --entity Monster` → `unexpected arguments ["Monster"]`；`roost add behavior --entity Monster --ai` → `unexpected arguments ["Monster" "--ai"]`；`roost add timedomain --entity Monster` → `unexpected arguments ["Monster"]`；`roost add mod heartbeat -service game` → `unknown wiring mod "heartbeat"`。修后 `add_heartbeat_test.go` 六条用例通过，生成工程端到端通过（`TestGeneratedHeartbeatBehaviorEndToEnd`：`example action: last progress "running ticks=5 waited=470 started=0", executed beats 6`）。变异核对：把生成的 `BehaviorConfig.Beat` 改成 nil（行为组件不挂心跳），端到端变红，原文 `timed out waiting for the heartbeat to tick the example action (driver {Delivered:0 Executed:0 ... Registered:0})`。

**与任务书的偏差**：①时间域是单独的命令 `roost add timedomain`，不是 behavior 的选项——挂域的是场景实体，用域的是成员实体，两者通常不是同一个；②`--state` 只生成 Catalog 声明，不接 `Set` / `Gate` / `StateDao`（持久状态要登记进 DaoManager，接法因实体而异）；③生成了一个业务 Nest handler 文件（`game/handler/<entity>_behavior.go`），用来发起示例动作、设定示例策略，端到端经它验证“示例动作被心跳推进”；④端到端用例放在脚本夹具（`scripts/testdata/heartbeat`），不在 `go test ./codegen/...` 里（避免再加一个要编译整个生成工程的用例）。

**需要维护者决定**：

1. heartbeat Mod 的装配方式：A 按需（现状，推荐：进程侧固定开销只在用到时付）；B 新工程里装了 nest 的服务缺省装（新手不必跑 add 也能直接用 `game/heartbeat.Driver`；代价是每个 game 进程多一个空转 goroutine 与 3s 停机预算）。
2. 生成工程端到端脚本是否进 pretag：A 不进（现状，与其他 `test-*-generated.sh` 相同，手工运行）；B 进 pretag（发版前多约 3 分钟）。

**与并行任务的衔接**：选项 C（`heartbeat.delivery`）已在 rebase 时接入（见上文“配置”）；P3（game-demo）不用这个生成器（demo 是手写参考实现，维护者 2026-10-18）。

**验证**（rebase 到 `ddeeffa3` 之后的 `c7064b47` 的树，退出码直接读；rebase 前在 `cc24027f` 上同样跑过一遍、结果相同）：gofmt 无输出；`GOWORK=off go build ./...`、`go vet ./...` 通过；`GOWORK=off go test -count=1 ./...` 通过（153 个包 ok，其中 `codegen/internal/roost` 276s、根包 12s）；`bash scripts/test-heartbeat-generated.sh` 通过（生成工程 build / vet / `test -race ./...` 全过，端到端 dispatch 一遍 `0 Nest batch jobs`、batch 一遍 `21 Nest batch jobs`，`roost id check`、`roost generate --check` 通过）；`bash scripts/test-sync-modes-generated.sh` 通过；`go generate ./codegen/... ./wiring/...` 后 porcelain 干净。生成工程放在会话 scratchpad，脚本结束时删除。

**未验证**：真实 DataEngine / Mongo 下的生成工程心跳（卸载重载后重新登记）、Remote 托管实体上的生成组件、`roost project doctor` 只在 rebase 前（`53cc3f97` 基线）的生成工程上跑过一次（退出码 0），rebase 后没有重跑。

### 现状（已核对源码，基线 `6e65ba12`）

- `scripts/perf/sync-aoi`：压测端一个循环每 tick 挑 1% 实体、分 10 批改值并移动（direct 模式直接 `interest.Move`；Nest 模式对每个实体发 `engine.Request` 到 load handler，handler 里 `QueueMove`，见 [nest.go](../../scripts/perf/sync-aoi/nest.go) `newLoadNest`）。移动由全局控制器驱动，不是实体自己的心跳。
- **框架里没有按实体的心跳**（维护者问“现在框架里没有 hb 吗”）。全仓查 heartbeat / HB / tick / 延时 Sender / entitytimer / AutoTick 的结论，逐项见下文 §1。
- `gameplay/actionflow`（ActionRunner / Mission / PlanMission）与 `gameplay/ai`（Controller / 行为树）已有 `Tick` 接口，但仓库里**没有任何生产调用方**：除各自包内的测试外没有人调用 `ActionRunner.Tick` 或 `ai.Controller.Tick`。
- game-demo 场景的移动是客户端发一个点、端点经 `MultiSync_MovePlayer`（玩家 + 场景实体两个目标）在事务里校验并写位置，提交后端点调 `Scene.Moved` → `interest.Move`（[move.go](../../demo/game/controllers/player/move.go.tmpl)、[scene.go](../../demo/internal/service/game/scene.go.tmpl) `Moved`）。服务器不做连续移动；怪物（`entities/monster`）只有位置与血量，不移动。

### 方向（组件部分；心跳部分已由下文方案取代）

1. **实体心跳**：见下文“心跳（HB）方案”。
2. **`MapBasic` 组件**：所在地图 / 场景、位置、朝向，以及与 AOI 的接线（进入场景 `Enter`、位置变化 `QueueMove`、离开 `Leave`）。
3. **`Move` 组件**：速度、目标点或路径、状态；在心跳里按经过的时间推进位置，写 `MapBasic`，触发 `QueueMove`；到达 / 打断 / 被阻挡由 actionflow 收尾。
4. **actionflow 控制**：移动是一个 Action（MoveTo / Follow / Patrol 等），由实体的 ActionRunner 驱动；心跳就是 runner 的 tick（接法见方案 §5.4）。技能、AI 等其他动作共用同一个 runner 与心跳。
5. **压测**：sync-aoi（及 nest-game）的移动改为实体心跳驱动，替代全局循环；保留旧负载开关以便对比。
6. **与“排队事实异步应用”的关系**：心跳在 Nest 快池 worker 上只做位置推进与入队（marshal + `QueueMove`），AOI 计算由 sync 侧做：ApplySync 在 Flush 的政策阶段、ApplyAsync 在应用线程（见 D19“两种应用模式”，2026-10-21 已实施）。两者互补。

### 心跳（HB）方案（2026-10-21，待确认）

#### 1. 结论：roost 现在没有可直接用的心跳

全仓用 codebase-memory 图（BM25 “heartbeat tick periodic entity”，名字正则 `heartbeat|autotick|hb` 在函数层 0 命中）与 `rg -i heartbeat|autotick` 核对，与“周期驱动实体”相关的只有下面几样，**没有一样是“按实体、非持久、在实体快池 worker 上执行的周期消息”**：

| 现有机制 | 是什么 | 为什么不是心跳 |
| --- | --- | --- |
| Nest 帧 Ticker（[ticker.go](../../framework/nest/ticker.go) `RegisterTickCallback` / `doTick`，`NewEngine` 构造、`nest.tick_duration` 缺省 100ms） | 引擎一个 goroutine，按间隔递增帧号，依次调用**包级**注册的回调 | 回调在 Ticker 自己的 goroutine 上顺序执行，不经实体锁、不在事务里；注册表是包级的（所有引擎共用）。生产代码里没有调用方，帧号只用于日志（`app.LogFrame`）。唯一用户是 nest-game 压测夹具：它在帧回调里按 `HeartbeatHz` 给每个实体 `Dispatch` 一条消息（[load_test.go](../../codegen/internal/entity/testdata/nestgame/load_test.go) “唯一正式 ticker 只负责投递”），这正是下文推荐的形状，但只存在于测试夹具里 |
| 延时 Sender（`Delay_X`、`nest.SendOptionWithDelay`，[dispatcher.go](../../framework/nest/dispatcher.go) `TryDelaySendMsg`） | 进程内按到期时间的最小堆，到期再走正常准入；不持久，容量 `nest.delayed_capacity` | 不能在 handler 里自我续期：handler 内调 `Dispatch` 返回 `ErrAsyncInHandler`（[client.go](../../framework/nest/client.go) `validateClientDispatch`）。即使从外面续期，也没有取消句柄（停不了，只能靠标志空跑）、没有去重（重复启动会出现两条链）、没有相位控制；到期时实体若已卸载，冷目标会被 I/O 阶段重新加载，唤醒不在内存的实体 |
| 按实体持久定时器 `framework/nest/entitytimer`（D11） | 期限进 DAO、已加载实例的最早期限小堆、到期投一次 Tick、10s 重投、离线后加载补触发一次 | 为稀疏、持久、准点的到期动作设计：每次续期都写 DAO 节点（走 WAL），每秒十次的移动会把位置推进变成持久写；驱动按“最早期限”而不是“周期 + 相位”组织 |
| `gameplay/actionflow`、`gameplay/ai` | `Action.Tick` / `Mission.Tick` / `ActionRunner.Tick(group, now)` / `Controller.Tick(now)` | 只有接口，没有驱动；“谁、在哪个锁里、多久调一次”目前没有答案 |
| `gameplay/skill/combatcomponent.TickBuffs` | 让到期 buff 过期 | 注释写明“由业务的 Nest 心跳/定时 handler 在实体锁内调用”，但仓库里没有这样的心跳 handler |
| 其余同名项 | Gateway TCP 连接保活（`answerHeartbeat`）、nest-game 夹具的 `HeartbeatComponent` 与 tailtrace 的 `heartbeat` 流名 | 与实体周期逻辑无关，或只是压测夹具 |

缺的是四件事：①按实体登记 / 停止 / 改频率的入口，随实体加载 / 卸载自动登记和解除；②一个只负责“按周期与相位把消息投进快池”的驱动；③一个框架注册的心跳 handler，在实体锁内按登记表依次调用各组件的回调；④与快池准入一致的合并、跳拍与背压规则。

#### 2. 模型：按实体的非持久周期 Nest 消息

推荐：**心跳是一条投递到该实体快池 worker 的单目标 Nest 消息**，由框架注册的心跳 handler 在实体锁内执行，与该实体的其他消息串行（同 ID 顺序由 Nest 准入统一建立，心跳不另建顺序）。驱动只负责“按时投递”，自己不执行任何业务。

| 方案 | 做法 | 结论 |
| --- | --- | --- |
| **A 按实体的周期 Nest 消息（推荐）** | 驱动到点给实体 `Dispatch` 一条心跳消息；handler 在实体锁内调回调 | 符合“心跳处理本质也是 Nest”：锁、事务、回滚、Sync 冻结、`QueueMove` 提交门全部沿用正式链路；回调里能做的事与普通快池 handler 完全相同 |
| B 驱动批量回调 | 驱动 goroutine 自己取实体锁（或 TryLock）逐个调回调（ssr 的执行方式接近这种：广播消息到 worker 后 TryLock，失败重投） | 绕过 Nest 的准入、同 ID 顺序与事务收尾；回调里改 DAO 不在事务里，Sync 冻结与提交门都不成立。不采用 |

投递粒度：**每个实体每拍一条单目标消息**（`Client.Dispatch`）。`DispatchBroadcast` 能把至多 32 个目标（`MaxBroadcastIdNum`）合成一次准入、在同一个 worker 上逐个加锁执行，准入次数降到 1/32，但一批要等批内每个目标的前驱都完成才能开始（批内任一实体忙，整批等），批内一个慢回调拖后面 31 个；广播路径也不支持 Remote。先用单目标，P4 用微基准判断准入开销是否值得换成批量（见 §7、§9 第 2 条）。

handler 元信息：单目标，参数是“实例令牌”（见 §5.1），`rollback=undo`；持久级别见 §9 第 8 条（推荐 `async`：本拍只改 nopersist 字段时提交记录为空，[rollback.go](../../framework/nest/rollback.go) `durableCommit` 在 `record.Empty()` 处直接返回，不写 WAL）。实体种类必须走快池：`BusinessPoolLong` 的种类在建 Host 时拒绝（心跳是快池业务）；Remote 托管实体的权威不在本进程，P1 同样拒绝（与 D11 生成器拒绝 `remote=managed` 一致）。

#### 3. 调度结构

**驱动放在哪里**：`gameplay/heartbeat.Driver`，进程一个（服务进程里每个 Nest 引擎一个），由 `wiring/heartbeat` Mod 装配：Provide 时绑定 Nest 客户端、只读实体查找（`ManagerAccess.Manager().Get`，不加载、不等待）、`app.BusinessClock` 与 Nest 统计；Nest 启动之后 `Start`，App 先停 Service、再停本 Mod、最后停 Nest（与 `wiring/entitytimer` 同一顺序）。不复用 Nest 帧 Ticker 的包级回调：那里的回调串行跑在一个共享 goroutine 上，一拍投几千条消息会拖慢其他回调，注册表也是包级的（测试之间互相影响）。

**goroutine 数**：缺省 **1 个**驱动 goroutine；配置 `heartbeat.shards`（缺省 1）可按实体 ID 哈希分成 N 个分片，每片一个时间轮、一把锁、一个 goroutine。只有 P4 微基准证明单 goroutine 在目标速率下占用超过约 1/4 核时才调大。

**时间轮（推荐）而不是最小堆或 ssr 的固定档位环**：

- 单层时间轮：分辨率 `heartbeat.resolution` 缺省 **10ms**，周期范围 `[MinPeriod, MaxPeriod]` 缺省 **[50ms, 60s]**（最高 20Hz；更长的周期是“定时”而不是“心跳”，用 entitytimer），轮长 = `MaxPeriod / resolution` = 6000 格。每格是一个切片，条目记自己的格号与下标，删除 O(1)（与末尾交换）。
- 每 10ms 驱动醒一次，取当前格的全部条目逐个处理，处理完放回 `当前格 + 周期格数`。周期按分辨率向上量化，不限于几个固定档。
- 不用最小堆：5 万条目、每秒几十万次出入堆，每次 O(log n) 且同一把锁下比较时间；时间轮每次出入 O(1)。不用 ssr 的“每档一个环、实体插入所有命中格”：那要求周期取固定档、一个条目占多格，换频率要删多格。
- **相位打散**：首拍时刻 = 下一个“周期网格点 + 相位”，相位 = `hash(实体ID) mod 周期格数`。确定性（同一实体重载后相位不变、测试可复现），同周期的实体均匀落在周期内的各格上，每 10ms 一格的投递量 ≈ 总拍率 × 10ms，没有整点尖峰。同一实体上多个登记共用这个相位（见 §4 合并），所以 100ms 与 1s 的登记在同一格上相遇。

**每拍的上界与背压（与快池准入规则一致）**：

- **一个实体至多一条在途心跳**：条目有 `inflight` 标志；到点时若上一拍还没执行（还在快池排队或执行中），本拍**跳过**、计入 `Skipped`，不再入队、不累积。所以排队中的心跳消息数 ≤ 登记了心跳的实体数，心跳自己永远不会把快池队列灌满。handler 一开始执行就清掉 `inflight`（defer，panic 也清）。
- **准入失败就跳拍**：`Dispatch` 返回 `ErrQueueFull`（快池等待数达到 `nest.short.queue_capacity`，见 [dispatch_queue.go](../../framework/nest/dispatch_queue.go) 准入）或引擎停止中时，清 `inflight`、计 `heartbeat.skipped_total{reason="queue_full"}`，下一拍自然重试；不在本拍内重试、不进延时队列。
- **水位让路**：驱动每 10ms 读一次 `NestMgr.Stats()`；快池等待数超过 `heartbeat.high_water`（缺省队列容量的 50%）时，这一格的条目全部跳拍（`reason="high_water"`）、照常放回轮上。保证业务消息始终至少有一半队列可用；心跳是“晚一拍就按经过时间多算一点”的工作，比玩家请求更适合让路。
- **每格上界**：`heartbeat.max_per_slot`（缺省 16384）；超出的条目本拍跳过，下一格从被跳过处轮转开始，避免总是同一批实体挨饿。正常相位分布下远达不到这个数（§7）。
- **卡住兜底**：`inflight` 超过 `heartbeat.stall_after`（缺省 2s）没有清除（消息被 Nest 丢弃而 handler 没执行，例如目标在准入后被卸载），驱动视为丢失、重新投递并计 `heartbeat.stalled_total`。

**投递前核对实例**（沿用 entitytimer `DeliverDue` 的做法）：条目对应的实例已 `IsRemoved()`，或查到的是同 ID 的另一个实例，就丢弃条目、不投递——驱动不唤醒不在内存的实体。

#### 4. API 草案（`gameplay/heartbeat`）

```go
// Beat 是交给回调的一拍。
type Beat struct {
	Now     time.Time     // 业务时间（app.BusinessClock），本拍执行时读取
	Elapsed time.Duration // 距本登记上次执行的业务时间，夹到 [0, MaxElapsed]；首拍等于 Period
	Period  time.Duration // 本登记当前周期（量化后）
	Skipped int           // 上次执行以来因在途 / 背压被跳过的拍数；只供观测，不补投
	First   bool          // 本登记启动后（含实例加载后重新启动）的第一拍
}

type Func func(Beat)

// Host 每个实体实例一个，由组件在构造时取得；只在实体锁内（或加载发布之前）调用。
func (d *Driver) NewHost(owner entity.IThreadSafeEntity) (*Host, error) // 长池种类、Remote 托管实体返回错误
func (h *Host) Register(name string, period time.Duration, fn Func) *Reg // 登记即停止状态；同名重复登记报错
func (h *Host) OnDestroy()                                              // 组件 OnDestroy 调用：永久离开驱动

func (r *Reg) Start()                     // 开始；已在跑时无操作
func (r *Reg) Stop()                      // 停止；全部登记都停时实体离开时间轮（空闲不占调度）
func (r *Reg) SetPeriod(p time.Duration)  // 改频率：下一拍按新周期，从本登记上次执行起算
func (r *Reg) Active() bool

// Owner 由实体实现，框架心跳 handler 据此找到 Host。
type Owner interface{ HeartbeatHost() *Host }
```

- **声明**：组件在构造或 `OnInitFinish` 里 `Register`（例如 Move 组件登记 `"move"`、100ms）；需要时 `Start`，做完 `Stop`。周期、回调、启停都由组件持有，框架只看登记表。
- **加载 / 卸载 / 删除**：心跳不持久。实例加载后所有登记都是停止状态；组件在 `OnInitFinish` 里按自己的 DAO 状态决定是否 `Start`（例如持久的“行军中”状态加载后重新启动位置推进）。`OnDestroy`（卸载、删档、撤销新建）让实例永久离开驱动，之后的启停一律忽略。不用 finalizer 兜底。
- **事务内的启停**：在 Nest 事务里调用的 `Start` / `Stop` / `SetPeriod` 先记在 Host 的待办里，经 `nest.AfterCommit` 生效；事务回滚就丢弃（与 entitytimer 的报告同一规则）。不在事务里（加载发布前）立即生效。心跳回调里调用的同样延后到本拍提交后（不在遍历登记表时改它）。
- **频率可变、空闲停**：`SetPeriod` 改频率；实体的投递周期 = 正在跑的登记里最短的周期，登记变化后重算并移到新格。没有正在跑的登记时条目移出时间轮，零成本。
- **回调参数**：`Elapsed` 按“本登记上次执行”算，不按“实体上次心跳”算，所以同一实体上 100ms 与 1s 的两个登记各自得到正确的经过时间；掉拍时 `Elapsed` 自然变大（只算一次），`Skipped` 告诉回调掉过几拍。`MaxElapsed` 缺省 1s，可按登记覆盖：业务时钟偏移在运行中被调整、或进程长时间停顿时，不让一拍走出几公里。

#### 5. 语义

1. **非持久**：登记、相位、在途状态都只在进程内存；停机即丢，加载后由组件重新登记。消息参数带实例令牌（Host 创建时分配的进程内序号）：handler 发现令牌与实体当前 Host 不一致（旧实例的迟到消息碰上重载后的新实例）就什么都不做、计 `heartbeat.stale_total`。投递前核对之后、执行之前实例被卸载时，这条心跳可能让 Nest 把实体重新加载一次（准入时判为冷目标、改道 I/O 阶段），也可能因取不到实体而被丢弃（由 `stall_after` 兜底）——这与 D11 已接受的“卸载与已入队 Tick 的窗口”同一类（维护者 2026-10-18 决定①）；空闲实体（最可能被卸载的）已经停了心跳，窗口实际很小。是否另加“只投已加载目标”的 Nest 发送选项见 §9 第 9 条。
2. **不追帧**：框架从不为错过的拍补投消息；掉拍后下一拍 `Elapsed` 覆盖整段时间，回调按经过时间算一次（移动 = 速度 × Elapsed，沿路径推进，越过终点即到达）。需要逐拍确定性的逻辑（技能 Runtime 按整数 tick 推进）在回调里把“推进到 Now 对应的 tick”交给它自己的推进函数，它内部按序处理到期工作，这不算框架追帧。
3. **与 D11 持久定时器的分工**：持久、准点、离线也要算的事走 entitytimer（Buff 到期写档、建造完成、行军到达、活动阶段、离线产出）；在线周期推进走心跳（移动、actionflow / AI、周期性伤害与回复、冷却推进、实体自己的逻辑帧）。同一件事可以两者都用：行军到达是 entitytimer 期限，行军途中给 AOI 的位置是 1～2Hz 心跳。**entitytimer 的驱动不改成基于心跳**（§9 第 12 条）：它按“最早期限”只在到期时唤醒一次，期限可以是几小时之后，还要处理“加载后补触发”与重投；放进心跳要么让时间轮支持任意长周期（多层轮），要么每个有定时器的实体都挂一个低频心跳轮询，两者都比现在贵、语义也不同。
4. **与 actionflow 的接法（心跳即 runner tick）**：`gameplay/actionflow` 增加一个适配（P2），持有实体的 ActionRunner / MissionRunner 和一个心跳登记 `"actionflow"`：有当前动作或任务时 `Start`，周期取当前各组动作周期的最小值（动作可选实现 `TickInterval() time.Duration`，不改 `Action` 接口，缺省 100ms），全部结束时 `Stop`。每拍对每个有当前动作的组调 `ActionRunner.Tick(group, beat.Now)`，再调 `MissionRunner.Tick`；runner 自己的延后队列照常在最外层返回前执行。`ai.Controller.Tick` 作为另一个登记（例如 `"ai"`、200ms）或在 actionflow 登记的回调里顺序调用，由接入方选择。
5. **与 AOI 的接法**：心跳回调里只推进位置、写 `MapBasic` 的位置字段（nopersist、参与同步）并 `interest.QueueMove(e, pos)`；`QueueMove` 在实体锁内只记事实、走提交门（[interest_queue.go](../../framework/sync/entitysync/policy/interest_queue.go) `QueueMove`），AOI 计算由 sync 侧在提交后应用（D19 后续的“排队事实异步应用”进一步把它移出提交路径）。注意：D19 记录的“每玩家 16 个额外空间视野时 Nest 模式失守”恰好在这条提交路径上，原因尚未确认（KNOWN-LIMITS“AOI 视野”④）；心跳不改变它，P4 压测须同时覆盖这一点。
6. **快池不阻塞**：回调只做内存计算和入队（改 DAO、记 effect、`QueueMove`、记待办），不能阻塞、不能做 I/O，不能在回调里 `Dispatch` / `Request`（Nest 本来就拒绝）。需要等待或 I/O 的，按现有规则处理：持久后续动作记 effect；需要查询的走 Await（I/O 池，首版只支持无回滚 memory handler）或长池消息，由进程侧组件在提交后投递。寻路这类重计算在接到移动命令的那条消息里做（或预计算），不在每拍里做。
7. **业务时钟**：`Beat.Now` 与 `Elapsed` 都按 `app.BusinessClock`（测试注入可控时钟，与 entitytimer 一致）；时间轮本身按系统单调时间醒来，只决定“何时投递”，不进入业务计算。

#### 6. 与 ssr heartbeat 模块的差异和取舍

ssr 的模型（参考 ssr 的 heartbeat 模块、定时器模块与行动 / 任务模块的用法整理）：

- **登记**：每个实体带一个自动心跳模块；组件调用 `Register(周期档, 回调)` 得到 ID、`Remove(ID)` 撤销；另有“每拍都调”和“每秒最多一次”两类回调；回调为空的登记只用来把实体心跳提速。
- **周期与相位**：周期只能取 10 个固定档（50ms 基础帧的 1、2、4、10、20、40、100、200、600、1200 倍）；实体的心跳频率 = 已登记的最短档。相位 seed = `hash(实体ID) mod 1200`，所有档共用一个 seed，按 `帧号 mod 档 == seed mod 档` 触发，各档天然对齐。
- **结构**：实体内用“10 档定长桶 + 最短档游标”存登记，O(1) 求最短档；最小堆与排序数组是同一接口的另外两种实现，只在测试里对照，生产用桶。全局调度器是 4 个环（跨度 2 / 20 / 200 / 1200 帧），每格一个 ID 集合，实体按（最短档, seed）插入命中的所有格。
- **驱动**：进程一个 50ms 全局帧 goroutine，在一把全局锁下取出本帧格里的 ID，按每条消息至多 64 个目标广播进实体消息队列。
- **执行**：消息队列 worker 逐个目标 TryLock 实体，在实体锁内遍历登记、按档过滤后调回调，参数是（距该登记上次执行的时间, 帧差, now）；TryLock 失败把消息原样重投，不合并。回调期间的登记 / 撤销延后到本拍结束。
- **卸载、频率、空闲**：`Release` 清空并退出全局环（另有 finalizer 兜底），`Frozen` / `Recover` 暂停与恢复，`UseTicker` 可换成场景自己的帧；换频率 = 撤销 + 重新登记，行动列表与任务按当前 Action / Mission 的 `LoopInterval` 登记、切换时换档；没有登记时退出全局环。
- **与 timer 的关系**：ssr 的定时器挂在心跳上（每秒心跳检查到期，临近到期时登记一个空回调把心跳提到 1s / 2s / 10s… 档），定时器精度受心跳档约束，实体不在内存就不触发。
- **性能取舍**：固定档 + 预先算好的格 → 每帧收集 O(本帧 ID 数)、广播 64 目标一包减少入队；代价是周期只有 10 档、全局锁、消息队列没有上界和背压、TryLock 失败重投会放大排队，心跳与业务消息同一队列没有让路规则。

roost 的取舍：

| 项 | 照搬 | 不照搬 / 改法 | 理由 |
| --- | --- | --- | --- |
| 按实体 ID 哈希定相位、多登记共用相位 | 照搬 | — | 确定、无尖峰，多登记在同一拍相遇才能合并投递 |
| 实体频率 = 最短登记周期，按登记各自算经过时间 | 照搬 | — | 一实体一条消息，回调看到自己的周期 |
| 回调期间的登记变化延后 | 照搬 | 改为随事务 `AfterCommit` 生效，回滚丢弃 | roost 的回调在事务里，回滚的启停不能生效 |
| 没有登记时退出调度 | 照搬 | — | 空闲零成本 |
| 驱动广播 + worker TryLock、失败重投 | 不照搬 | 单目标 `Dispatch` 走 Nest 准入，至多一条在途，跳拍不重投 | Nest 已有同 ID 顺序与前驱等待，不需要 TryLock；重投在快池满时会放大排队 |
| 固定 10 档、多环、每格 ID 集合 | 不照搬 | 单层时间轮、周期按 10ms 量化 | roost 不需要档位来对齐，也避免一个条目占多格 |
| 定时器挂在心跳上 | 不照搬 | 持久定时器仍走 entitytimer | roost 的定时器持久、离线后加载补触发，精度不受心跳档约束 |
| finalizer 兜底、`UseTicker` 换驱动 | 不照搬 | `OnDestroy` 显式离开；场景帧用场景实体自己的心跳 | roost 实体生命周期有明确的销毁回调；一套驱动更容易保证背压规则 |
| 心跳与业务同一队列、无让路 | 不照搬 | 水位让路 + 准入失败跳拍 | 快池准入有容量上限，心跳不能挤掉玩家请求 |

#### 7. 性能估算（推算，未实测）

已测的参照：v1.29.0 重档（Apple M5，GOMAXPROCS=10）1 万实体全部 10Hz 心跳 + 1000 玩家各 10 条/秒业务，即每秒 10 万条心跳 + 1 万条业务，short 8 worker / 队列 65536，30 分钟心跳与业务拒绝都是 0，被测进程（nest-game 夹具进程，含 Gate 与 Sync）约 4.2～4.6 核（见 [V1290-HEAVY-30M](../performance/V1290-HEAVY-30M-20261019.md)）。那里的心跳由夹具的帧回调逐个 `Dispatch`，形状与本方案相同，所以“每秒 10 万条单目标心跳”在这台机器上已有证据；更高的速率没有。

**只算移动（5 万实体、10Hz）**：

| 移动中占比 | 每秒心跳消息 | 每 10ms 一格 | 排队心跳上界（=移动实体数） |
| ---: | ---: | ---: | ---: |
| 10% | 5 万 | 500 | 5000 |
| 20% | 10 万（与已测重档同量级） | 1000 | 1 万 |
| 50% | 25 万 | 2500 | 2.5 万 |
| 100% | 50 万 | 5000 | 5 万 |

**所有快池业务 tick 都走心跳之后**（示例分布，供拍板时代入自己的数字）：5 万实体 = 1 万玩家 + 4 万怪物 / NPC。

| 群体 | 数量 | 登记 | 不合并（每个登记一条） | 按实体合并（取最短周期） |
| --- | ---: | --- | ---: | ---: |
| 移动中的玩家 | 2000 | 移动 10Hz + Buff / 回复 1Hz | 2.2 万/s | 2 万/s |
| 静止、有 Buff 的玩家 | 4000 | Buff / 回复 1Hz | 4000/s | 4000/s |
| 活跃且移动的怪物 | 6000 | 移动 10Hz + AI 5Hz + 回复 1Hz | 9.6 万/s | 6 万/s |
| 活跃未移动的怪物 | 4000 | AI 5Hz + 回复 1Hz | 2.4 万/s | 2 万/s |
| 其余（空闲） | 3.4 万 | 无 | 0 | 0 |
| 合计 | 5 万（32% 挂心跳） | | **14.6 万/s** | **10.4 万/s（省约 29%）** |

- 合并省掉的是“同一实体上同频或倍频的登记”（移动 + AI + actionflow 常常同时在跑）；单一用途的实体省不了。跨实体的批量准入（`DispatchBroadcast` 32 个一批）另外能把准入次数降到约 1/32（10.4 万/s → 约 3300 次准入/s），但执行条数不变，代价见 §2。
- **驱动开销**：每拍一个条目 = 出格 + 两次内存核对 + 一次 `Dispatch` 准入（取消息对象、短锁登记同 ID 尾）。按每条 1～3µs 粗估，10 万/s 约 0.1～0.3 核、25 万/s 约 0.25～0.75 核，单 goroutine 在 25 万/s 附近可能到顶，那时开分片或批量准入。驱动每秒只醒 100 次；时间轮本身的出入格与 5 万条目的内存（每条约 100 字节量级，约 5MB）可以忽略。
- **快池队列压力**：心跳在队列里的数量 ≤ 挂心跳的实体数（示例 1.6 万，占 65536 的 24%），加上 50% 水位让路，业务始终至少有一半队列；每 10ms 一格约 1000 条的突发远小于队列容量。真正的约束是 worker 的执行时间：10.4 万/s × 每拍回调耗时（移动 + `QueueMove` 估 5～20µs）≈ 0.5～2 核 worker 时间，叠加 D19 记录的“提交后 Interest 事实应用随视野数线性增长”。

**后续微基准（P1 / P4 做，本轮不测）**：`BenchmarkWheelAdvance`（5 万条目、混合周期，只测时间轮出入，不投递）；`BenchmarkDriverDispatch`（假客户端，测驱动单 goroutine 每秒能处理的条目数）；真实 Nest + 空 handler 的开环投递 10 / 25 / 50 万 每秒，记录完成数、跳拍数、快池等待峰值、P99；同负载 `Dispatch` 与 `DispatchBroadcast` 对照；时间轮与最小堆在 5 万条目下对照（验证 §3 的选择）；快池被业务占满时的跳拍与让路行为。同机交替、benchstat、保存全部样本。

#### 8. 测试计划与实施分期

**测试（P1，`-race`，先红后绿）**：可控业务时钟 + 假 Nest 客户端（显式推进时间轮，事件顺序由测试决定）：相位按 ID 哈希确定、同周期实体均匀分布；周期量化与上下限；多登记取最短周期、各自的 `Elapsed`；启停与改频率后移格、全部停止后离开时间轮；在途时跳拍且 `Skipped` 正确、不累积；`ErrQueueFull` 与水位让路跳拍；每格上界轮转；卡住兜底重投；`MaxElapsed` 夹取；`OnDestroy` 之后不再投递；旧实例令牌在新实例上不执行；事务回滚丢弃启停、提交后生效；长池种类与 Remote 托管实体拒绝。真实 Nest 引擎：心跳 handler 在实体锁内执行，与同一实体的普通消息严格串行（用 barrier 证明不并发），实例卸载后不再投递。每个强制点配一个变异核对（例如“跳拍改为累积”“回滚不丢弃启停”“不核对实例”各自让对应用例变红），修前失败文本原样记录。

| 分期 | 内容 | 风险 |
| --- | --- | --- |
| P1 心跳框架 | `gameplay/heartbeat`（Driver / Host / 心跳 handler）、`wiring/heartbeat` Mod 与配置 schema、指标（`heartbeat.delivered_total`、`skipped_total{reason}`、`stalled_total`、`stale_total`、`registered`）、guide / impl 文档与 OBSERVABILITY | 中：新 Mod 增加停机预算一项；指标名进门禁 |
| P2 组件与 actionflow | `gameplay` 下 `MapBasic` / `Move` 组件（MoveTo / Follow，路径推进、到达 / 打断）；actionflow 心跳适配；`ai.Controller` 接法 | 中：actionflow / ai 第一次有生产驱动，延后队列与回调重入要按现有用例再核一遍 |
| P3 game-demo 接入 | 玩家移动改为“客户端发目标 → MoveTo 动作 → 心跳推进 → `QueueMove`”；怪物巡逻；保留现有按点移动作为对照或瞬移 | 中：改变 demo 的客户端协议与 loadtest 场景；demo 是手写参考实现，不迁到生成器 |
| P4 压测改为心跳驱动 | sync-aoi 与 nest-game 的移动 / 心跳负载改用 `gameplay/heartbeat` 驱动，保留旧全局循环开关对照；补 §7 微基准；同时复测 D19 多视野 Nest 模式 | 中：压测数字口径变化，前后要同机对照 |
| P5 现有快池 tick 迁移 | 见下表 | 按项 |

**现有周期写法按“是否跑在快池里”筛选后的去留**：

| 现有写法 | 是否快池业务 | 以后 |
| --- | --- | --- |
| game-demo World 的 `Sync_TickWorldTimers` 每秒 ticker | 曾是（服务 goroutine 每秒给 World 发一条快池消息） | 已在 D11 去掉，World 现在用 entitytimer 驱动（持久期限）。不再迁移；以后再出现“业务自己每 N 秒给实体发周期消息”的写法，一律改用心跳 |
| `scripts/perf/sync-aoi` Nest 模式的全局移动循环 | 是（每个实体一条 `Request` 到快池） | **P4 代码已实施（`3c5e41e3`）**：`-move-driver=heartbeat` 为心跳 + movement 巡逻，`loop` 保留且仍是缺省；测量待 v1.31 发版前压测 |
| nest-game 夹具帧回调逐个投递心跳 | 是 | **P4 代码已实施（`22b01614`）**：`ROOST_NEST_GAME_HEARTBEAT_DRIVER=heartbeat` 用 `gameplay/heartbeat`，`ticker`（`HeartbeatHz` 旧路径）保留且仍是缺省；测量待 v1.31 发版前压测 |
| `gameplay/actionflow`、`gameplay/ai` 的 `Tick` | 是（应当在实体锁内调用） | P2 由心跳驱动 |
| `combatcomponent.TickBuffs` 与周期性 Buff 效果、冷却、回复 | 是 | **已实施（P5，`AttachBeat`，见“P5 实施结果”）**。原计划：有到期 buff 或周期效果时挂 1Hz（或按 buff 粒度）登记，全部清空时停；加载后的第一拍顺带清理过期记录（注释里“重启后首次业务 tick”）。需要离线也准点、且结果要落档的长时 buff 仍用 entitytimer。风险中（战斗数值的时间粒度要与技能 tick 对齐） |
| Skill `Runtime.Advance(tick)` | 是（挂在实体组件上时） | **已实施（P5，`NewRuntimeBeat`，见“P5 实施结果”）**。原计划：有在途施法 / 衍生物时挂登记，每拍把 Runtime 推进到 `beat.Now` 对应的 tick，空闲停。风险中（技能确定性依赖整数 tick 与回放，换算规则要测） |
| game-demo 怪物刷新（`spawner` 每秒 ticker） | 部分（服务 goroutine 在 Nest 外决定、再建实体和登记同步） | **已做（随 P3，2026-10-21）**：场景实体的 1Hz 心跳 `"scene.spawn"` 在场景锁里决定缺额，提交后（`AfterCommit`）交给进程侧按“建实体 → 放身体 → 登记同步并进 AOI → Nest 事务进场景并启动 AI → 计数”的顺序建，ticker 删除；见“P3 实施结果” |
| SLG 行军、建造、研究、收入（夹具） | 到期部分不是周期 | 继续用 entitytimer；行军途中如果大地图 AOI 需要服务端位置，加 1～2Hz 心跳插值位置 + `QueueMove`。风险低 |
| tower-defense / auto-chess / survival / idle 夹具的 `Advance(now)` | 不是周期（按下一个期限推进） | 继续用 entitytimer；某个玩法需要连续模拟（例如塔防里怪物连续移动）时，房间实体挂心跳 |
| 活动阶段（World 期限 + 服务 `runActivity` 5s 循环） | 期限是持久定时器；循环是服务后台工作（跨服务协调、经总线） | 不迁：期限留在 entitytimer，服务循环不在范围内 |

**不在范围内（不是快池业务，只写边界、不纳入心跳）**：Lockstep `Match` / 房间的 `Tick` 协程（game-demo battle 30ms、MOBA 夹具 room，定步长、单 owner goroutine）；Sync Manager 的 Flush 与兜底 ticker；DataEngine 投影器与 outbox worker；Nest WAL group commit；entitytimer 驱动自身的调度循环；各 service 的后台 sweep（session / match / chat / activity / platform）、Gateway 续约、statslog、业务时间高水位、Remote 版本锁续约；game-demo 的 playerowner 卸载扫描、匹配器循环、场景重试；robot / loadtest 压测端；I/O 池与长池上的工作。

**房间 / 场景级 tick**：房间、场景本身就是实体（game-demo 的场景实体被移动事务锁住、World 实体、MOBA 的对局实体），房间逻辑帧就是该实体自己的心跳登记；不提供“非实体心跳”。限制：心跳是单目标消息，房间心跳只能改房间自己的状态；需要每拍同时改多个成员的逻辑，要么拆成成员各自的心跳读房间下发的事实，要么由业务消息（`DispatchMulti`）承担——是否给心跳加“按组投递”留到 P5 之后视需要再定。

#### 9. 需要维护者拍板

1. **包名与位置**：`gameplay/heartbeat`（Driver / Host / 心跳 handler）+ `wiring/heartbeat`（Mod 装配）；移动组件放 `gameplay/movement`（`MapBasic` / `Move`）。推荐这样：心跳框架在 gameplay（维护者要求），装配与 entitytimer 一样放 wiring。
2. **投递粒度**：A 每实体每拍一条单目标 `Dispatch`（推荐，已有 10 万/s 证据、无批内互等）；B `DispatchBroadcast` 32 个一批。推荐 A，P4 微基准证明准入开销成为瓶颈再评估 B。
   - **压测结论（2026-10-21，[HB-STRUCTURES-20261021](../performance/HB-STRUCTURES-20261021.md) §4～§6，维护者决定 ②）**：5 万实体、100ms、10ms 分辨率、真实 Nest 引擎、回调只做内存计算，同时 5000 条/s 业务消息。100% 移动（50 万次/s）进程 CPU：X 单目标 `Dispatch` 4.25 核、Z `DispatchBroadcast` 1.90 核、Y ssr 式批量（64 个一批、承载 handler 逐个 `TryLock`）0.77 核；20% 移动（10 万次/s）1.37 / 0.64 / 0.30 核。业务消息 P99：无心跳 0.10ms，X 0.50～0.67、Y 0.12～0.25、Z 0.88～1.24ms（50ms 分辨率下 Z 8.3ms：批要等全部前驱，排在后面的业务消息等整批）。48 次运行跳拍 0、准入失败 0，回调间隔 P99 100.5～102.9ms。X 的 CPU 约六成是每条消息停放 / 唤醒 worker 的调度器自旋与派发队列锁。**Y 在 roost 里绕过目标实体的 Nest 事务与 Sync 捕获**：回调跑在承载实体的事务里，`QueueMove` 的提交门（`CurrentSyncMutation`）挂到承载事务、目标的位置变化不随之捕获，移动不能用；只作成本参照。与维护者经验对照：ssr 式批量在成本上确实最好，在 roost 语义上不成立。
   - **推荐**：A（X）作为 P1；B（Z）不推荐（业务延迟变差、不支持 Remote）；CPU 成为瓶颈时评估 C——在 Nest 里加“批量心跳作业”（不登记同 ID 顺序、逐目标确认无在途作业且 `TryRequireEntity` 成功后在该目标自己的 Guard + 事务里执行，失败交回驱动重投），改 Nest 契约，需另行设计。
   - **方案 D 原型（2026-10-21，[实验记录](../performance/NEST-DISPATCH-BATCH-20261021.md)，分支 `perf/nest-dispatch-batch`）不达标**：CPU 只比 X 少 10～12%；维护者 2026-10-21 按推荐 A 决定分支不合入，只保留 worker“结束后同一次持锁取下一项”，随选项 C 合入。
   - **维护者 2026-10-21 决定：选项 C 先合入、缺省关闭，合批优化压测通过后再改为缺省开启**（开关 `heartbeat.delivery: dispatch | batch`，缺省 `dispatch`；Nest 侧的分片登记表、同锁取下一项、`DispatchTryBatch` 入口无条件生效）。
   - **选项 C 已实施（2026-10-21，[设计](FEATURE-2026-10-21-nest-batch-heartbeat.md)，分支 `perf/nest-batch-heartbeat`，未合入 main）**：维护者 2026-10-21 批准按推荐做（C + 两张 goroutine 登记表改分片表 + 只保留 D 的同锁取下一项），并追加同 ID 顺序硬约束：tails 有登记（含已准入未执行）的目标当场转单目标作业排在冲突消息之后（不跳拍，对应 ssr 的 MoveCopy 但只投一次），执行期间认领为该 ID 的尾作业。`heartbeat.delivery: batch` 时心跳按 64 个一批走 `nest.NestMgr.DispatchTryBatch`（Remote 托管实体仍单目标 `Dispatch`）。23 条变异核对 22 红 1 绿（绿的一条是防御性包装，见设计 §16）。**性能结论：压测暂停，待补**（维护者 2026-10-21 18:55 要求先完成代码；复跑步骤见 [NEST-BATCH-HEARTBEAT-20261021 §4](../performance/NEST-BATCH-HEARTBEAT-20261021.md#4-待补测复跑步骤)）。待拍板 4 项见设计 §15。
3. **同一实体多个组件登记**：A 合并为一条投递，实体周期取最短、各登记按自己的周期与经过时间触发（推荐，示例分布省约 29%）；B 每个登记各自投递。
4. **合并 / 跳拍**：至多一条在途，上一拍没执行完就跳过、只计 `Skipped`，不累积、不补投（推荐）；备选“累积到下一拍一起执行”会让慢实体越积越多，不推荐。
5. **背压**：准入失败跳拍 + 快池等待超过 50% 容量时整格跳拍 + 每格上界 16384（推荐）。水位比例是否接受 50%。
6. **时间参数**：分辨率 10ms、周期范围 [50ms, 60s]、`MaxElapsed` 1s；缺省频率建议移动 10Hz、AI 5Hz、Buff / 回复 / 冷却 1Hz（只作文档建议，由组件登记时给出）。推荐这一组。
7. **驱动 goroutine**：缺省 1 个，`heartbeat.shards` 可调（推荐）。
   - **压测结论（2026-10-21，同上 §2、§5，维护者决定 ⑦）**：ssr 的心跳收集与投递都在 1 个全局 50ms 帧 goroutine 上（另有 1 个 10ms 的消息队列节拍唤醒 worker），没有独立的心跳驱动。roost 1 个驱动在 50 万次/s 时占 0.70 核（X）；4 个分片时 X 变差（业务 P99 0.50 → 3.6ms、快池等待峰值 1403 → 6393、驱动合计 0.70 → 1.87 核，多驱动同时争派发队列锁），Y / Z 无收益。推荐 1 个、不提供 `heartbeat.shards`。
8. **心跳 handler 的持久级别**：A `async`（推荐：只改 nopersist 时记录为空、不写 WAL；偶尔需要落档的结果，例如动作完成发奖，与普通快池 handler 一样提交）；B `memory`（回调里一切持久写被拒绝，持久结果只能走 entitytimer 或别的业务消息，最严格也最便宜）。
9. **卸载窗口**：A 接受“准入后被卸载的实体可能被冷加载一次”，靠投递前核对实例 + 实例令牌保证不在新实例上误执行（推荐，与 D11 决定①一致）；B 给 Nest 加“只投已加载目标、冷目标直接丢弃”的发送选项（扩大 Nest 契约，三大块之一）。
10. **种类限制**：长池种类与 Remote 托管实体在 P1 拒绝建 Host（推荐）。
11. **调度结构**：单层时间轮（推荐），不用最小堆、不用 ssr 的固定档多环；不复用 Nest 帧 Ticker 的包级回调（推荐）。Nest 帧 Ticker 的包级回调目前没有生产调用方，是否另行清理不在本条范围。
    - **压测结论（2026-10-21，[HB-STRUCTURES-20261021](../performance/HB-STRUCTURES-20261021.md) §3，维护者决定 ⑪；上面“单层时间轮（推荐）”由此改为按档分环）**：5 万 / 50 万登记、D20 §7 分布与全部 100ms 两种，benchstat 10 遍。ssr 的思路（固定档 + 相位 = hash(实体 ID) 分桶）最好，但 ssr 的写法（4 个跨度环、每格 map、一个登记插多格）不是：改写成“每档一个环、登记只进一格、格是切片 + 下标”后，50 万全部 100ms 每秒收集 2.8ms（50ms 帧）/ 3.9ms（10ms），ssr 写法 27.4ms、单层时间轮 14.4ms、最小堆 367ms；撤销 / 改档 11～17ns（ssr 写法 52～141ns、堆 46～74ns）；常驻约 50 B/登记（ssr 写法 62～91 B，ssr 生产“每格预分配 10000”另有固定约 420 MB；时间轮混合档位约 80 B）。相位打散每格最大 / 平均 ≤ 1.04。分辨率决定突发大小：50ms 帧一次投 2.5 万（5 万个 100ms 登记），10ms 每格 5000，对应 X 的迟到 P50 21.5 → 7.4ms、Z 的业务 P99 8.3 → 1.2ms。与维护者经验一致。
    - **推荐**：按档分环，10ms 分辨率（相位 = hash mod 6000，再对本档格数取模），1 个驱动；不用最小堆与单层时间轮。原型见 `gameplay/heartbeat/internal/bench`（`TierRing`）。
12. **entitytimer 驱动是否改成基于心跳**：不改（推荐，理由见 §5.3）。
13. **房间 / 场景级 tick**：房间、场景都是实体，用自己的心跳；不提供非实体心跳（推荐）。
14. **P3 范围**：game-demo 玩家移动是否改成“发目标点、服务器按心跳推进”（推荐，才能展示 MoveTo / actionflow；按点移动保留为对照或瞬移），连同原“需要确认”第 5 条（本期只做 MoveTo / Follow，还是连巡逻、寻路一起）一起定。

### 需要确认（原登记）

1. ~~组件放在哪里~~：维护者 2026-10-21 定为框架 `gameplay`（包名见上文 §9 第 1 条）。
2. ~~心跳机制放在哪里~~：维护者 2026-10-21 定为写进 `gameplay`、在 Nest 快池上执行（方案见上文）。
3. 位置是否每次心跳持久化：建议位置为 nopersist（只同步），按周期或离开场景时落库一次；还是每次心跳 strict 写入。
4. ~~心跳频率~~：由上文方案给出（按登记、移动中才高频、空闲停），缺省值见 §9 第 6 条。
5. 范围：本期只做移动（MoveTo / Follow），还是连巡逻、寻路一起（与 §9 第 14 条一起定）。

## D21 吸收 ssr 的优秀组件进 `gameplay`

**状态：维护者 2026-10-21 登记为后续任务；同日要求先做第一项调研——ssr 控制实体行为的一套（`action`、`action_list`、`mission`、`ai`）与 roost `gameplay/actionflow` / `gameplay/ai` 的能力对比，以及 `state` 能否直接加进 `gameplay`。调研结果与方案见下文“调研一”。维护者 2026-10-21 决定：调研一 13 条拍板项全部按推荐（A）实施，按 D21-1 → D21-5 分期进行，已实施（结果见“调研一 · 实施”）。** 来源：D20 心跳与移动参考了 ssr 的 heartbeat / action / move / map_basic，维护者希望把 ssr 里好的组件（component / 模块）也加进框架的 `gameplay`，业务逻辑可以参考 ssr 的处理。

**原则**：按 roost 的模型重写，不复制 ssr 代码；只收“与具体游戏玩法无关、多数游戏都会用”的通用部件，玩法规则留给业务（demo 是参考，生成器只生成框架内容）；所有控制实体行为的逻辑走 actionflow（D20 维护者决定），周期推进走心跳（D20）；每个组件先写方案（与现有 roost 包的关系、事务 / 回滚 / 同步语义、持久化），维护者确认后再实施。

**ssr 候选（按可能的通用性初排；`gameplay` 现有 actionflow / ai / attribute / heartbeat / skill，D20 P2 新增 movement）**：

- 行为与调度：`action`、`action_list`、`mission`、`ai` / `ai_engine` / `bt_engine`、`state` **已调研（下文“调研一”）**；`trigger_engine`、`eca_man`（事件-条件-动作）、`cooldown`、`timer` 待调研。
- 空间与移动：`map_basic`、`move`、`follow_agent`、`rvo`（避让）、`dynamic_obstacle`、`hexagon` / `hexagon_map`、`search_entity`、`observe` / `radar`、`mist`（迷雾）。
- 战斗与效果：`effect` / `effectid`、`damage`、`aggro`（仇恨）、`fight`、`skill_pool`、`power`。
- 经济与道具：`bag` / `bag_light`、`currency`、`prop`、`drop`、`store`、`gacha`、`limit`（次数 / 频率限制）、`refresh`。
- 社交与组织（多数已有 `service/*` 对应，主要是比对补缺）：`team`、`rank` / `top_list_*`、`task` / `taskcore`、`reddot`、`uniqname`。

**下一步**：调研一待拍板；其余候选逐项调研 ssr 实现与 roost 现状，产出“收 / 不收 / 收哪部分”的清单与优先级，交维护者确认后分批实施。

### 调研一：行为控制（action / action_list / mission / ai / state）

**读了什么**：ssr 只读 `ssr/kingdom/modules/` 下 `action`、`action_list`、`mission`、`ai`、`ai_engine`、`bt_engine`、`state`，以及它们用到的 `heartbeat`、`move`、`map_basic` 接口和 `kingdom/view` 里的模块接口（下文 ssr 出处一律相对 `ssr/kingdom/modules/`，`view/…` 相对 `ssr/kingdom/`）。roost 读 main（`3e7b4011`）的 `gameplay/actionflow`、`gameplay/ai`、`gameplay/heartbeat`、`gameplay/skill/combat` / `combatcomponent`，以及 D20 P2 分支 `feat/d20-movement-p2`（未合入 main）的 `gameplay/actionflow/heartbeat.go`（BeatRunner）与 `gameplay/movement`。codebase-memory 图的根是主检出、generation 停在 2026-10-14，P2 文件不在图里，结构结论以源码为准。

#### 1. ssr 的模型

**总体**：一个实体的行为由三层组成，互相用回调连起来，全在实体所在的逻辑线程上串行执行（ssr 没有事务）：

- **action_list**（实体模块）：按“组”放当前动作，组的当前动作挂自己的心跳，负责动作的开始、替换、打断、结束；同时内嵌 `AbilityManager`（按玩法列出的 `CreateMoveAction` / `SetMissionChaseAndAttack` 等门面）和 `MissionManager`（`view/mod_action_list.go:18-46`、`action_list/internal/action_list.go:53-62`、`action_list/internal/ability_manager.go:25-143`）。
- **mission**（action_list 内的单任务槽）：用手写 Go 代码编排多个动作（追击后攻击、徘徊、回出生点……），有自己的心跳；动作结束时被回调决定下一步（`mission/mission.go:13-51`）。
- **ai**（实体模块 AICtrl）：按优先级排列的若干 director（选策略层、策略层、喊话、避让）接收心跳与游戏事件，决定调用哪个 mission / action（`ai/internal/ai_ctrl.go:66-86`、`ai/internal/director_priority.go:3-16`）。
- **state**（实体模块）：按状态类型存“是否生效 + 参数”的状态位，变化时通知观察者；action / mission / ai / 战斗都读写它（`state/internal/state.go:26-34`）。

**action（单个动作）**

| 方面 | ssr 的做法 | 出处 |
| --- | --- | --- |
| 接口 | `TryInit` → `Start(curTime) (wait bool, err)` → 每拍 `Loop(curTime) error` → `CanCancelByNext(next)` / `CanCancelByForce()` / `TryCancel(next, force)` → `OnEnd(reason)`；`Update(param)` 改在途参数；`LoopInterval()` 决定心跳档位；`FrozenAction` / `RecoverAction` | `view/mod_action_list.go:91-118` |
| 结束方式 | `Loop` 返回错误即结束（正常结束也用哨兵错误，如 `ErrReachEnd`、`ErrRVOStop`）；`Start` 返回 `wait=false` 表示瞬时动作（例如 JumpTo），不进当前位、直接释放 | `action_list/internal/action_list.go:434-436`、`action/internal/action_rvo.go:134-142`、`action/internal/action_move.go:194-200` |
| 取代否决 | 新动作要取代当前动作时先问当前动作 `CanCancelByNext(next)`，再 `TryCancel(next)`；返回 `ErrNoNeedCancel` 时**保留当前动作、丢弃新动作且对调用方返回 nil**（例如同目的地的移动、手动移动不被 AI 移动取代） | `action_list/internal/action_list.go:255-287`、`557-583`；`action/internal/action_move.go:1003-1022` |
| 归属任务 | 动作创建时若当前任务正在自己的回调里（`HasOnProcessTag`），记下任务 ID；任务结束时按任务 ID 清掉它的动作 | `action/internal/base.go:9-22`、`mission/mission_manager/manager.go:119` |
| 实现库 | 7 种动作（Move、Chase、Attack、EndAttack、CastSkill、Rvo、Interaction），全是玩法相关；`sync.Pool` 复用对象，每 10 秒打印各种类在途数量 | `action/action_mod/mod.go:14-38`、`action_list/action_list_mod/mod.go:17-19` |
| 和组件的接法 | 动作在 `Loop` 里调组件接口：`ActionMove.DoLoop` 自算 `elapsed`，调 `MovePathInfo.StepMove(elapsed)`；动作在 Start / OnEnd 里置位 / 清除状态（Manual、BackToCity、BackToOrigin） | `action/internal/action_move.go:302-346`、`246-274`、`1024-1052` |

**action_list（组、替换、排队、心跳）**

- **组**：固定 3 组 Live / Fight / Move，动作种类到组的映射是硬编码函数 `GetActionGroup`（`view/mod_action_list.go:81-89`、`175-184`）。
- **同组**：`HasActionList` 对三个组都返回 false，所以**排队实际关闭**：新动作总是尝试取代当前动作（`view/mod_action_list.go:166-173`、`action_list/internal/action_list.go:199-202`）。取代走“try → commit → afterCommit → cancel”的 `doTcc`：新动作 `TryInit` 成功、旧动作同意被取消才换上，`Start` 失败或 `wait=false` 时撤回（`action_list/internal/action_list.go:28-51`、`236-287`）。
- **跨组**：新动作与**其他组**的当前动作按硬编码矩阵 `NeedCancelCurAction(cur, next)` 判定，命中就拒绝新动作（`ErrOtherActionBlock`）；启动后再按同一矩阵取消其他组的动作——按代码，第二段只有在 Start 期间其他组又换了动作时才可能命中（`action_list/internal/action_list.go:204-213`、`292-302`；矩阵 `view/mod_action_list.go:136-164`）。
- **冻结**：`Frozen(group)` 置标志并**强制取消**当前动作；冻结期间创建动作返回 `ErrActionListFrozen`；`Recover` 只解冻，不重启任何东西（`action_list/internal/action_list.go:75-97`、`159-170`）。由状态驱动：定身（Immobile）冻结 Move 组、缴械（Disarm）冻结 Fight 组（`battle/internal/fight_state.go:78-93`）。
- **结束**：`EndCurAction(group, force)`、`EndAllCurAction`、`EndAction(id)`（当前或排队的都可以按 ID 结束）、`ClearAllActionWithOption(force, filter...)`（按任务 ID 等过滤）（`action_list/internal/action_list.go:449-478`、`512-554`）。结束时依次调动作 `OnEnd` → 撤心跳 → 通知任务 `OnActionEnd` → 通知 AI `OnActionEnd` → 放回对象池（`action_list/internal/action_list.go:480-493`）。
- **重入**：组在变更中（`changing`）时新的变更进 `deferF` 列表，变更结束后依次执行；超过 10 条或 10 轮直接**丢弃并打 Error 日志**（`action_list/internal/action_list.go:215-235`、`309-313`）。
- **心跳**：组的当前动作切换时按动作的 `LoopInterval()` 给该组登记心跳（新旧周期相同则沿用旧登记）；动作 `Loop` 里核对心跳 ID 与当前动作是否一致，不一致就撤掉过期登记（`action_list/internal/action_list.go:375-401`、`action/internal/base.go:91-102`）。周期按场景选（副本 100ms、大世界 500ms）（`action/internal/action_move.go:145-152`）。时钟：大世界用墙钟，副本用场景时间（可暂停 / 变速）（`action_list/internal/action_list.go:428-433`）。

**mission（任务）**

- **一个实体同时只有一个任务**；`SetMission(type, param)` 结束旧任务再启动新任务；同类型且 `CanCancelBySameTypeMission` 为假时**静默不换**（返回 nil）；正在切换（`changing`）时再调 `SetMission` / `EndCurMission` **直接返回 nil、什么都不做**（`mission/mission_manager/manager.go:72-79`、`129-136`、`175-181`）。
- **生命周期**：`Start(cur, param)`（返回 `ErrFinish` 表示启动即完成，照样通知 AI）→ 自己的心跳 `Loop`（100ms / 500ms，`mission/internal/base.go:60-67`、`mission/mission_manager/manager.go:144-173`）→ 动作结束回调 `OnActionEnd` → `End(reason)`；结束时清掉属于它的动作、通知 AI `OnMissionEnd`（`mission/mission_manager/manager.go:109-127`、`193-205`）。任务回调里的结束操作先进 `deferFList`，回调返回后执行，同样 10 条上限后丢弃（`mission/internal/base.go:77-149`）。
- **内容**：9 种任务全是手写玩法流程（追击并攻击、追击并交互、移动并交互、徘徊、追击并放技能、选技能追击放技能、回出生点、避让），步骤、分支、失败处理都写在 Go 代码里。例如 ChaseAndAttack：Start 时建追击动作，已在范围内就直接攻击；每拍检查目标是否还可攻击、超距重新追；追击动作因不可达结束超过阈值次数则任务失败（`mission/internal/chase_and_attack.go:106-266`）。**没有数据驱动的计划**。
- **入口**：移动请求按移动类型分流成“直接建移动动作”或“设一个任务”（`move/internal/move.go:349-374`）。

**ai（决策）**

- **AICtrl**：持有选策略层（`selectStrategy`）与策略层（`strategy`）两个策略，再加喊话、避让两个 director；director 按固定优先级排成数组，事件按优先级依次分发，分发期间对更高优先级位置的增删延后（`ai/internal/ai_ctrl.go:66-86`、`ai/internal/director.go:190-249`）。
- **事件**：约 25 种具体事件（被攻击、死亡、动作结束、任务结束、状态变化、心跳、每秒心跳、Buff 生效、被嘲讽、施法、采集……）都是接口方法（`view/mod_ai.go:17-46`）。冻结、手动操作状态（Manual / ManualForceMove）、副本未开战时屏蔽决策（`ai/internal/ai_ctrl.go:439-456`、`486-547`）。
- **策略替换**：`SetStrategy(next)`：旧策略 `CanStopByNextStrategy` 同意 → 新策略 `Init` → 卸旧装新；卸下的策略 `OnClear` 结束任务并清掉所有动作（`ai/internal/ai_ctrl.go:369-399`、`ai/internal/strategy_base.go:30-39`）。
- **冻结**：`Frozen(opts)` 可带“多久后自动恢复”（`RecoverDuration`，在 AI 心跳里检查）和“是否打断在途动作”两个选项（`ai/internal/ai_ctrl.go:177-190`、`492-499`）。
- **心跳**：选策略层与策略层各要一个周期，AICtrl 只登记两者中最短的一个心跳；另有一个每秒心跳（`ai/internal/ai_ctrl.go:51-59`、`284-329`）。
- **策略的两种写法**：① 手写策略（SimpleNpc、SimpleServant、Trusteeship 等 8 种，玩法相关，例如 SimpleNpc 每拍按状态选“追击 / 徘徊 / 回出生点 / 避让”任务，`ai/internal/strategy_simple_npc.go:243-340`）；② **AIEditor**：用 `ai_engine` 跑编辑器导出的 JSON。
- **ai_engine**（AIEditor 用）：不是经典行为树，而是**按阶段（stage）组织的事件触发图**：每个阶段一棵根，根下挂 Event 节点（监听某类事件、可一次性或重复）、Condition / Selector / Sequence 组合与 Action 叶子；Action 叶子是**瞬时函数**（可配延迟执行和最多执行次数），通常调用 `SetMission*` / `CreateMoveAction`；`SetNextStage` 切阶段并重开该阶段的事件；黑板是 int / string 两种值的键值表，分全局与每拍两级（`ai_engine/ai_engine.go:26-106`、`186-202`、`ai_engine/internal/engine.go:112-255`、`ai_engine/internal/action.go:63-104`、`ai/internal/editor_action.go:176-178`）。配置按版本热更（`ai/ai_mod/mod.go:27-44`），但运行中的引擎的版本检查回调是空函数，已在跑的实例不换树（`ai_engine/internal/engine.go:90-94`）。运行日志可留在内存、经调试命令查看（`ai/internal/strategy_ai_editor.go:169-208`、`ai/internal/debug_ai.go`）。
- **bt_engine**：一套带 Running 状态、Parallel / Inverter / Repeater / Succeeder 的行为树引擎，但**ssr 里没有任何包导入它**，`InitRunData` 是 TODO 空实现（`bt_engine/bt_engine.go:61-80`、`bt_engine/internal/engine.go:83-86`）。

**state（状态位）**

- **不是状态机**：没有“当前状态 + 迁移规则”，也没有互斥组；是 **约 140 个状态类型各自独立的“是否生效 + 参数”**，状态类型按编号区间分成 4 种模式（`state/state.go:16-158`、`194-204`）：
  - **Cover**（约 50 个，例如 Moving、Dying、BackToCity、InProtection）：最后一次 Set / Unset 为准，可重复设置、重复取消（`state/internal/config/config_cover.go:12-15`、`state/internal/node/node_cover.go:17-53`）；
  - **RefCover**（例如 Immobile 定身、Disarm 缴械、Invincible、Manual）：匿名引用计数，Set 加一、Unset 减一，参数取最后一次 Set（`state/internal/node/node_ref_cover.go:16-49`）；
  - **RefAcc**（例如 BeTaunted 嘲讽）：每次 Set 返回一个序号，Unset 必须带序号，生效参数取最早的一次（`state/internal/config/config_ref_acc.go:10-15`、`state/internal/node/node_ref_acc.go:22-78`）；
  - **Once**（例如 EnterScene、LeaveScene、BeAttacked、Jump）：Set 时通知观察者、随即复位——实际是**按状态类型分发的事件**（`state/internal/state.go:123-126`）。
- **观察者**：按状态类型、按变化类型（Set / UnSet / Update）注册，带优先级排序；`Agent` 把一组注册打包、一次全部注销（`state/internal/state.go:229-270`、`state/internal/node/node_base.go:20-40`、`state/internal/agent.go:20-49`）。SetState 的回调有 recover，ClrState 的没有（`state/internal/state.go:119-121`、`204-206`）。
- **时长**：没有内建计时。`SetStateWithTimeout` 发一条延迟消息再清除（`state/internal/state.go:175-182`、`state/internal/handlers_matrix.go:79-90`）；`ExpireTimeFunc` 声明过期时间，但模块没有心跳，要实体自己在每秒心跳里调 `CheckExpireState`（源码注释“状态模块没有心跳机制，先暂时由上层调用吧”，`state/internal/state.go:295-316`、`entities/castle_brief/internal/castle_brief.go:261-263`）。
- **同步与持久化**：状态节点只在内存；每个状态可配 `SyncWdsFunc`，把变化投影到实体的同步组件 `MapEntityState` 的字段上（该组件的字段带 bson 标签，随实体落库、同步给客户端）（`state/internal/config.go:10-31`、`wds/kingdomMap.pb.go:231-252`）。**加载后不自动恢复**：由实体在加载时读组件字段重新 Set（例如城堡的保护期，`entities/castle_brief/internal/castle_brief.go:199-207`）。
- **操作检查**：`CheckOperation(op)` 按“必须全部生效 / 必须全部不生效”的状态列表判断能否做某个操作（只登记了 2 种操作）（`state/internal/checker.go:15-45`）。
- **和行为的接法**：Immobile / Disarm → 冻结动作组（上文）；移动动作置 / 清 Manual 等状态；AI 监听 Dying / EnterScene / LeaveScene、读 Manual 状态屏蔽决策（`ai/internal/ai_ctrl.go:120-142`）；技能效果经战斗对象 `ModifyState` 改状态（`fight/object.go:107-111`）。

**持久化与重连（整体）**：action / mission / ai 的运行状态全部只在内存（对象池），不落库、不随断线重连恢复；实体重新加载后由业务（AI 心跳、实体加载逻辑）重新决策。玩家断线重连对服务器侧行为没有影响（动作照常跑），客户端看到的是同步组件的结果（位置、状态位）。

**测试**：action / action_list / mission / ai / state 都没有测试文件；ai_engine 有一个导出测试。

#### 2. 能力对比

| 维度 | ssr | roost 现状 | 差距 | 是否引入 |
| --- | --- | --- | --- | --- |
| 动作类型 | 7 种玩法动作（移动、追击、攻击、施法、避让、交互…），`sync.Pool` 复用 | 只有契约（`Action{Kind, Start, Tick, Cancel}`，`gameplay/actionflow/runtime.go:17-23`）与实例级注册表（`registry.go:23-135`）；P2 分支提供 MoveTo / Follow（`gameplay/movement/action.go`） | 缺“瞬时动作”（Start 即完成）：roost 的 Start 只能返回错误，要完成得等第一拍 Tick；ssr 的瞬时动作不通知任务（`wait=false` 走撤回分支，不调 `OnActionEnd`），是缺陷不是能力 | 框架只引入“Start 即完成”；具体玩法动作不引入（追击 / 交互可做成 movement 的组合，另行调研 follow_agent / rvo 时再定）；对象池不引入（无证据，需要时先压测） |
| 动作组 / 互斥 / 优先级 | 3 个固定组；同组取代需当前动作同意（`CanCancelByNext` / `TryCancel`）；跨组硬编码矩阵（阻止或取消）；无数值优先级 | 组由业务的 `GroupForKind` 决定，数量不限（`action_runner.go:51-61`）；同组 `Start` 一律取代（旧动作被取消，`action_runner.go:526-542`）；无跨组规则 | 缺①取代否决 / 合并相同请求，②跨组冲突规则 | ①② 都引入，做成可选接口 / 配置函数，不硬编码矩阵（见 §3 A2、A3） |
| 队列与取代策略 | 有链表队列但三组都关闭排队；`EndAction(id)` 可结束排队项；按过滤条件清除 | 每组 FIFO 队列（`Enqueue`），队首在组空闲时启动；`ClearQueue`、`ClearMission`；丢弃的排队项各发一次取消的 OnEnded（`action_runner.go:222-233`、`599-631`） | roost 更完整；缺按 ID 结束任意动作 | 引入 `EndAction(id)`（§3 A4） |
| 打断与恢复 | `Frozen` 取消当前动作并拒绝新动作，`Recover` 只解冻；动作有 `FrozenAction` / `RecoverAction` 回调 | `Freeze` 只暂停（当前动作不 tick、拒绝 Start、队列不推进），`Recover` 继续当前动作并推进队列（`action_runner.go:331-342`、`544-547`） | 语义不同：ssr 冻结=打断，roost 冻结=暂停。定身打断移动是常见需求 | 引入冻结模式（暂停 / 打断两种，§3 A5） |
| 任务（多步计划、分支、失败处理） | 单任务槽；9 种手写任务，步骤与失败处理写在 Go 里；同类型参数相同时静默不换；有自己的心跳 | 单任务槽 `MissionRunner`（`CanReplaceBy` 否决、`ClearActions` 钩子）；通用 `PlanMission`：数据描述的步骤，每步 `OnSuccess` / `OnFail` 跳转到任意步或结束（`plan.go:14-219`）；任务没有心跳驱动（P2 `BeatConfig` 不含 MissionRunner） | roost 的计划更通用；缺：任务周期 tick 的驱动、步骤超时与重试、“相同请求不换”的明确返回 | 引入任务心跳（§3 A6）、PlanMission 步骤超时 / 重试（§3 A8）；手写玩法任务不引入 |
| AI 决策与动作的衔接 | 多 director 按优先级收事件；手写策略 + 编辑器事件触发图（ai_engine，分阶段、事件节点、瞬时 Action 叶子）；bt_engine 未使用；策略经 `SetMission*` / `Create*Action` 下发 | `Controller` 单策略、事务式替换、回调期间切换延后（`ai/controller.go:28-109`）；`BehaviorStrategy` 行为树（组合、并行、重复、守卫、冷却、限时、随机选择，`ai/nodes.go`）+ `TaskflowAction` 叶子按动作 ID 等结束（`ai/behavior_strategy.go:131-172`）；严格 JSON 树解析（`ai/tree_parser.go`） | roost 的行为树与动作衔接更完整（叶子能等动作结果，ssr 的编辑器 Action 是瞬时的）；缺：通用事件投递、阶段（boss 多阶段）、冻结定时恢复、心跳驱动（P2 未接 AI） | 引入事件投递、分阶段策略、`FreezeFor`、AI 心跳（§3 C1～C4）；ai_engine 编辑器格式、bt_engine、喊话 / 避让 director 不引入 |
| 状态（state）机制 | 约 140 个独立状态位，4 种模式（覆盖 / 匿名计数 / 带序号计数 / 一次性事件），观察者 + 优先级，投影到同步组件 | 无。战斗容器的 buff 有标签（`gameplay/skill/combat/buffs.go:15-16`、`HasTag` 274），技能免疫检查靠业务注入的 `HasGameplayTag`（`combatcomponent/status_bridge.go:36-37`） | 缺一个“实体当前有哪些行为相关的状态”的统一入口，以及状态对动作组的门控 | 新包 `gameplay/state`（§4） |
| 事件与回调 | 动作结束 → 任务 → AI 由 action_list 直接串好；状态变化 → 观察者；AI 事件接口约 25 个具体方法 | 回调都在：ActionRunner 的 `OnQueued` / `OnTransition` / `OnEnded` / `OnError`（`action_runner.go:40-49`），MissionRunner 的 `OnEnded` 等（`mission_runner.go:17-26`），Controller 的 `OnActionEnd` / `OnMissionEnd`；**但框架没有把三者接起来的实现**，`ActionList` 接口只有测试里的实现，业务要自己写胶水 | 缺默认装配（ssr 的 action_list 模块那一层） | 引入默认装配 `actionflow.Behavior`（§3 A1） |
| 心跳驱动方式 | 每组当前动作一个登记、任务一个登记、AI 一个登记（取层间最短）+ 每秒一个；周期按场景选；按心跳 ID 核对丢弃过期拍 | P2：`BeatRunner` 每组一个登记，周期取动作的 `TickInterval`，组空闲停；`Elapsed` 来自心跳；Remote 共享模式挂不上时动作失败结束；任务、AI 未接 | 缺任务与 AI 的心跳接线 | 引入（§3 A6、C4） |
| 持久化 / 重载后恢复 | 不持久；状态投影到同步组件字段随实体落库，加载时由实体逐个重设 | runner 纯内存、不随事务回滚（D20 决定 3，由业务保证）；不持久；buff 在 `CombatDao` 里持久、随事务回滚（`combatcomponent/component.go:40-74`） | 一致：都不持久行为状态。state 需要定：存哪里、回不回滚、持久不持久 | 行为状态保持不持久；state 见 §4 |
| 配置化 | 动作 / 任务 / 策略在 Go 里注册；AI 编辑器 JSON（阶段、事件、条件、动作、延迟、次数），按版本热更（运行中实例不换） | 动作 / 任务 Go 注册、注册表可封存；`PlanMission` 计划是数据；行为树 JSON（严格解析、带路径诊断）；无热更，换配置需重新 `SetStrategy` | roost 的配置更严格；缺阶段与事件节点 | 阶段 / 事件引入为行为树能力（§3 C2）；不引入 ssr 编辑器格式与 stub 版本热更（配置热更走 configdata 的现有机制，另议） |
| 可观测性 | 每 10 秒打印各类动作 / 任务在途数；AI 编辑器运行日志留内存、可经调试命令查看；按实体开关的动作日志 | 只有钩子（OnTransition / OnEnded / OnError），无指标 | 缺在途数 / 结束原因计数、AI 决策轨迹 | 引入可选指标与行为树轨迹钩子（§3 A9、C5） |
| 测试性 | 无单元测试；行为依赖全局时钟、全局工厂与对象池 | ActionRunner / MissionRunner / Controller / 行为树有大量单测（延后队列、重入、panic 恢复、有界）；P2 有真实 Nest 与 `-race` 用例 | roost 明显更好 | 新能力按现有方式先红后绿 |

#### 3. 引入方案

**和 roost 约束的关系（对全部条目成立）**：runner、任务、策略、state 的方法都只在实体锁内调用（Nest handler、心跳回调、加载 / 新建初始化），不加锁；回调只做内存计算与入队，不阻塞、不 I/O，符合快池（short）不阻塞；周期推进只经 `gameplay/heartbeat` 登记；ActionRunner / MissionRunner / Controller 不随事务回滚（D20 决定 3：handler 最后一步才调用它们，框架不处理）；行为状态不持久。

**A. 扩展 `gameplay/actionflow`**

- **A1 默认装配 `Behavior`（ssr action_list 模块那一层）**。现在 `ActionList` 接口（`runtime.go:25-51`）没有框架实现，业务要自己把 ActionRunner、MissionRunner、AI 接起来，而接线顺序有坑（动作结束先送任务再送 AI；任务结束先清它的动作；清场先 `EndCurMission` 再 `EndAllAction`；任务回调里建的动作要带任务 ID）。草案：

  ```go
  // gameplay/actionflow/behavior.go
  type Observer interface { // ai.Controller 实现它；actionflow 不导入 ai
      OnActionEnd(id int64, kind ActionKind, reason ActionReason)
      OnMissionEnd(m Mission, reason ActionReason)
  }
  type BehaviorConfig struct {
      Registry     *Registry
      GroupForKind func(ActionKind) (ActionGroup, bool)
      Conflict     func(running, incoming ActionSnapshot) Conflict // A3，可空
      Beat         *BeatConfig                                    // 为空则不挂心跳（测试 / 手动 tick）
      Observer     Observer
      OnError      func(error)
  }
  func NewBehavior(owner entity.IThreadSafeEntity, cfg BehaviorConfig) (*Behavior, error) // 实现 ActionList
  func (a *Behavior) Runner() *ActionRunner
  func (a *Behavior) Missions() *MissionRunner
  func (a *Behavior) Clear(reason ActionReason) // EndCurMission 后 EndAllAction 的固定顺序
  ```

  影响：新增，不改现有类型；测试里的 `planActionList` 可改用它。工作量约 1 天（含按现有延后队列用例补“动作结束 → 任务 → 观察者”的顺序与重入测试）。

- **A2 取代否决与合并（ssr `CanCancelByNext` / `TryCancel` 的 `ErrNoNeedCancel`）**。可选接口，不实现时行为不变（一律取代）：

  ```go
  type ReplaceDecision uint8
  const ( Replace ReplaceDecision = iota; Keep /* 保留当前，新动作以取消结束 */; Reject /* 保留当前，Start 返回 ErrReplaceRejected */ )
  type Replacer interface { ReplaceBy(ctx *ActionContext, next ActionSnapshot) ReplaceDecision }
  ```

  与 ssr 的区别：ssr 的“保留当前”对调用方返回 nil 且新动作无声消失；roost 的 `Keep` 照样给新动作一个 ID 并发一次取消状态的 OnEnded（与“拿到 ID 的动作一定有结论”一致，`action_runner.go:140-143`），`Reject` 返回明确错误。`End(group, force=true)` 不问 `Replacer`（ssr 的 `CanCancelByForce` 不引入：强制结束必须成功）。工作量 0.5 天。

- **A3 跨组冲突规则（ssr 硬编码矩阵 → 配置函数）**：

  ```go
  type Conflict uint8
  const ( NoConflict Conflict = iota; BlockIncoming /* 新动作返回 ErrBlockedByGroup */; PreemptRunning /* 取消对方组的当前动作再启动 */ )
  // ActionRunnerConfig.Conflict func(running, incoming ActionSnapshot) Conflict
  ```

  在 `applyStart` 里先按其他组的当前动作逐个判定（组按编号排序，结果确定）；`PreemptRunning` 取消对方时不推进对方队列。默认 nil = 无跨组规则（现状）。工作量 0.5 天。

- **A4 `EndAction(id, force, reason)`**：按 ID 结束当前或排队的动作（ssr `EndAction(id)`）。`ActionList` 接口加同名方法（不兼容：测试实现要补）。工作量 0.3 天。

- **A5 冻结模式**：`Freeze(group, mode FreezeMode)`，`FreezePause`（现状：暂停当前、拒绝新动作）与 `FreezeCancel`（ssr：取消当前、拒绝新动作）；冻结是**计数的**（多个来源同时冻结同一组，全部解冻才恢复），由 §4 的 state 门控驱动时天然需要计数。`Freeze(group)` 改签名（不兼容，现有调用只在测试）。工作量 0.5 天。

- **A6 任务心跳**：`BeatConfig` 增加任务登记（`"<Name>/mission"`），任务可选实现 `TickIntervaler`（复用 P2 的接口），有任务且需要周期 tick 时挂、任务结束时停；Remote 共享模式挂不上时任务以失败结束（与动作一致）。只在 `Behavior` 里接（BeatRunner 只管动作组不变）。工作量 0.5 天。

- **A7 Start 即完成（瞬时动作）**：两种做法（拍板 3）：A 改 `Action.Start` 为 `Start(ctx) (done bool, result ActionResult, err error)`；B 可选接口 `Instant interface{ StartResult() (ActionResult, bool) }`，Start 返回后 runner 查询。两者都让 runner 在 Start 后立即走 `finish`、发 OnEnded、通知任务（修掉 ssr 瞬时动作不通知任务的问题）。A 影响 P2 的 `movement.moveAction` 与测试里的全部动作实现（约 20 处）；B 无影响。工作量 0.3 天（B）/ 0.5 天（A）。

- **A8 PlanMission 步骤超时与重试**：`MissionStep.Timeout time.Duration`（依赖 A6 的任务 tick：超时以 `ActionStatusExpired` 结束当前步的动作，走 `OnFail`）、`MissionStep.Retry int`（失败后原地重试次数，用完才走 `OnFail`）。更复杂的分支（按条件、按结果原因）留给行为树，不在计划里加表达式。工作量 0.5 天。

- **A9 可观测（可选）**：`Behavior` 提供指标钩子：`actionflow.actions_started_total{kind}`、`actionflow.actions_ended_total{kind,status}`、在途数 gauge；名字按 `metric_names_gate` 规则登记进 [OBSERVABILITY](../framework/OBSERVABILITY.md)。是否默认开启待定（每个动作两次计数器递增，量级同心跳）。工作量 0.5 天。

**B. `gameplay/state`（新包，详见 §4）**：状态声明表 + 每实体状态集合 + 观察者 + 对动作组的门控 + 与 Buff 的桥。

**C. 扩展 `gameplay/ai`**

- **C1 事件投递**：ssr 的约 25 个具体事件是玩法词表，不照搬；给 `Controller` 加 `OnEvent(ev Event)`，`Event{Kind uint16; Data any}`，策略可选实现 `EventStrategy{ OnEvent(ctx *Context, ev Event) }`；`BehaviorStrategy` 像动作结束一样把事件缓冲到下一拍，新增条件叶子 `EventReceived(kind)` 与 `TakeEvent`。冻结期间事件丢弃（ssr 同样屏蔽），动作 / 任务结束照常送达（现有 `notifiable` 规则不变）。工作量 0.5 天。
- **C2 分阶段策略（ssr ai_engine 的 stage）**：`StagedStrategy[C]`：若干命名的树、当前阶段、`SetStage(name)`（在回调里调用时延后到本拍结束，切换时 Reset 旧树、触发配了 `OnInterrupt` 的叶子收尾）；JSON 增加 `stages` 顶层与 `set_stage` 内置动作叶子。阶段切换常见于 boss 多阶段，与具体玩法无关。工作量 1 天。
- **C3 `FreezeFor(d)`**：冻结并在 `d` 后的第一次 Tick 自动恢复（ssr `RecoverDuration`）；可选是否同时结束在途动作（ssr `NoInterruptAction`）。工作量 0.2 天。
- **C4 AI 心跳**：`ai.BeatController`（或 `Behavior` 的选项）给 Controller 登记 `"ai"` 心跳，周期由策略可选实现 `TickInterval()` 决定，无策略或冻结且无定时恢复时停。用独立登记而不是并进 actionflow 的组心跳（拍板 7），理由同 P2 决定“每组一个心跳”：AI 决策周期（常见 200ms～1s）与动作推进周期不同。工作量 0.5 天。
- **C5 决策轨迹钩子（可选）**：`BehaviorStrategyOptions.Trace func(path string, status Status)`，供调试命令或日志按实体采样；不内建内存日志环（ssr AIEditor 的 `logs` 无上限地追加，`ai/internal/strategy_ai_editor.go:190-193`）。工作量 0.3 天。

**不引入（及理由）**

| ssr 部件 | 理由 |
| --- | --- |
| 具体动作（攻击、追击、施法、交互、避让）与具体任务（追击攻击、徘徊、回出生点等）、`AbilityManager` 门面 | 玩法相关；业务按自己的动作 / 任务种类注册。追击 / 跟随类可在调研 follow_agent / rvo 时作为 movement 的组合再评估 |
| `bt_engine` | ssr 自己没有使用、未完成；roost 行为树已覆盖并更完整 |
| `ai_engine` 编辑器 JSON 格式、stub 版本热更 | 是 ssr 编辑器工具链的格式；roost 只取“阶段 + 事件”两个概念（C1、C2），格式沿用现有严格 JSON 树 |
| 喊话、避让 director 与 director 优先级数组 | 玩法；roost 用单策略 + 事件，策略内部可组合多个子行为 |
| 场景时间 / 墙钟按“大世界”切换 | 时钟由业务经 `PopulateContext` / `Now` 钩子注入，框架不认场景类型 |
| 对象池 `sync.Pool` | 没有 roost 侧分配压力的证据；需要时先基准再做 |
| 静默丢弃（`deferF` 超 10 条丢弃、切换中 `SetMission` 返回 nil、取代失败返回 nil） | 与 roost 的“有界 + 明确错误 / OnError 报告”冲突，后者已实现 |
| state 的 Once 模式、匿名计数（RefCover） | 见 §4 |

**对现有调用方的影响（项目未上线，可不兼容，逐项列出）**：仓库里 actionflow / ai 除各自测试外没有生产调用方（D20 现状核对），P2 分支的 `gameplay/movement` 是第一个。不兼容项：A4 `ActionList` 加 `EndAction`（测试实现 `planActionList` 等补方法）；A5 `Freeze` 改签名并改为计数（测试里的冻结用例改写）；A7 若选 A，`Action.Start` 改签名（movement 两个动作 + 测试动作）。其余为新增或可选接口，默认行为不变。

**分期与工作量（前提：D20 P2 先合入 main，全部建立在 BeatRunner 上）**

| 期 | 内容 | 估计 |
| --- | --- | --- |
| D21-1 | A1 `Behavior`、A2 取代否决、A3 跨组冲突、A4 `EndAction`、A5 冻结模式、A7 瞬时动作 | 3 天 |
| D21-2 | `gameplay/state`（§4 S1～S4）与 actionflow 门控（S5） | 2.5 天 |
| D21-3 | Buff → state 桥（`combatcomponent`，S6） | 1 天 |
| D21-4 | A6 任务心跳、A8 计划超时 / 重试、C4 AI 心跳、C3 `FreezeFor` | 2 天 |
| D21-5 | C1 事件、C2 分阶段策略、C5 / A9 可观测 | 2 天 |

每期按 maintenance §7 先红后绿、`-race`、文档（guide / impl 的 actionflow 与 ai 章节、PACKAGES、KNOWN-LIMITS）同步；game-demo 接入随 D20 P3 一起，不单列。

#### 4. state 单独一节

**ssr 的 state 是什么**：一组**独立的状态位**（不是状态机，没有互斥组），每位带可选参数；4 种写入语义（覆盖、匿名计数、带序号计数、一次性事件）；变化通知按优先级送给观察者；时长靠延迟消息或实体心跳轮询；值只在内存，靠 `SyncWdsFunc` 投影到同步组件、随实体落库，加载时由实体逐个重设（§1）。用途三类：① **控制状态**（定身、缴械、无敌、嘲讽）——由技能 / Buff 设置，门控动作组与 AI；② **行为标记**（手动操作、回城、回出生点、移动中）——由动作设置，供 AI 与其他模块判断；③ **生命周期事件**（进 / 出场景、死亡、被攻击）——Once 模式，其实是事件总线。

**roost 里谁已经覆盖了一部分**：Buff 的持续时间、叠层、驱散、免疫与标签在 `combat.BuffContainer`（持久、随事务回滚，`gameplay/skill/combat/buffs.go:31-49`、`combatcomponent/component.go:40-74`）；技能的免疫判断经业务注入的 `HasGameplayTag`（`combatcomponent/status_bridge.go:36-37`）；生命周期有实体的 `OnInitFinish` / `OnDestroy` 与业务事件。缺的是：①“这个实体此刻是否被定身”这类**跨来源汇总的布尔事实**的统一入口，②这些事实对**动作组的门控**，③行为标记的存放处。

**方案：`gameplay/state`**

- **S1 声明（启动时一次，之后封存）**：

  ```go
  package state
  type ID uint16
  type Def struct {
      ID      ID
      Name    string
      Sync    bool // 进实体同步（客户端可见，例如“被定身”）
      Persist bool // 落库；只用于业务直接设置的长期标记（例如保护期），缺省 false
  }
  type Catalog struct{ /* ... */ }
  func NewCatalog(defs ...Def) (*Catalog, error) // 重复 ID / 名字报错；封存后只读
  ```

- **S2 两种写入语义**（取代 ssr 的 4 种）：
  - **按来源（Sourced）**：`Add(id, src Source) bool` / `Remove(id, src) bool`，`Source{Kind uint8; ID int64}`（例如 Buff 实例、某个动作、GM）；有任一来源即生效。取代 ssr 的匿名计数与带序号计数：来源可识别，重复 Add 同一来源不叠加，Remove 不存在的来源无效——避免 ssr RefCover“多减一次就错位”的问题。
  - **派生（Provider）**：`Provide(id, func() bool)`，查询时现算，不存储（例如“被定身” = Buff 容器里有 `stun` 标签）。拥有数据的组件在数据变化后调 `Changed(id)` 触发通知。派生状态天然随数据回滚、加载后自动正确，不需要持久与恢复。
  - **不引入 Once**：生命周期事件走实体回调或 C1 的 AI 事件，不混进状态表；**不引入匿名计数**。**参数**：不提供通用参数槽（ssr 的 `StateParam` 是一个装所有状态参数的大结构）；需要参数的（嘲讽目标）由来源自带（`Source.ID`）或由业务组件自己存。
- **S3 存储、回滚、同步、持久**：按来源的状态存在实体的一个 DAO 组件（`StateDao`）里：随 Nest 事务回滚（undo），`Sync` 位经现有实体同步下发，`Persist` 位落库、其余 nopersist。派生状态不存储。与 actionflow 不同，state 的值**随事务回滚**——它是数据，不是执行器；这样 handler 里“先加状态、后续步骤失败”回滚后状态也撤销，不需要业务额外保证。
- **S4 查询与观察**：`Has(id)`、`Any(ids...)`、`Satisfies(Requirement{All, None []ID})`（ssr `CheckOperation`）；`Watch(id, func(Change)) Handle` / `Unwatch`、`Watcher` 打包注销（ssr `Behavior`）；不做观察者优先级（顺序 = 注册顺序，确定性即可；ssr 的优先级只用于“AI 先于别人收到死亡”这类排序，可由注册顺序表达）。观察者在实体锁内同步调用；观察者里改状态延后到外层调用返回后执行、有界（与 actionflow 的延后队列同一规则）。**观察者的副作用不随回滚**：观察者若调 actionflow（例如取消在途动作），回滚后状态撤销而动作已被取消——与 D20 决定 3 相同，由业务保证（改状态放在 handler 最后一步）；能用 S5 门控表达的不要用观察者。
- **S5 与 actionflow 的门控（声明式，推荐）**：`state.Gate(set, rules...)` 生成 actionflow 用的冻结来源：规则 `Rule{State: Rooted, Group: Move, Mode: actionflow.FreezeCancel}`。状态生效时对组计数冻结（A5），失效时解冻；`FreezeCancel` 在生效那一刻取消在途动作。门控只依赖 state 与 actionflow 的公开接口，放在 `gameplay/state`（state 导入 actionflow，反向不导入）。
- **S6 与技能 / Buff 的关系与边界**：
  - **Buff（`combatcomponent`）**：负责数值、持续时间、叠层、驱散、免疫，是“控制效果”的唯一持有者；state 不重复存时长。桥接：`combatcomponent` 提供 `StateProvider(tag combat.Tag) func() bool` 与“容器变化后调用 `Changed`”的钩子（`ApplyBuff` / `RemoveBuff` / `DispelBuffs` / `TickBuffs` 之后），业务声明 `stun` 标签 → `Stunned` 状态。Buff 到期由 `TickBuffs`（D20 P5 挂 1Hz 心跳）驱动，state 跟着变。
  - **技能（`gameplay/skill`）**：施法前的“能否施法”查 state（`Satisfies`）；`StatusBridge.HasGameplayTag` 可直接接到 `state.Has`（标签与状态的映射由业务声明）；技能运行时自身的状态（施法阶段、衍生物）不进 state。
  - **actionflow**：动作可以 `Add` 行为标记（`Source{Kind: action, ID: 动作 ID}`），在 `Cancel` / 结束时 `Remove`（ssr 移动动作置 / 清 Manual 的做法）；门控走 S5。
  - **AI**：读 `Has` 做条件叶子；状态变化可经 C1 作为事件送给策略。
- **S7 时长**：不内建（推荐）。有时长的控制效果走 Buff；离线也要准点的长期标记（保护期）走 entitytimer（D11）到期时 `Remove`；短时标记由设置它的动作或 Buff 负责撤销。不做 ssr 的“实体心跳里轮询过期”。
- **工作量**：S1～S5 约 2.5 天，S6 约 1 天（见分期 D21-2、D21-3）。

#### 5. 调研一 · 需要拍板

1. **框架提供默认装配 `actionflow.Behavior`**：A 提供（推荐：ssr 的 action_list 就是这一层，接线顺序有坑，业务各写一份容易错，且 AI / 任务心跳要有统一挂载点）；B 维持现状，业务自己实现 `ActionList`。
2. **同组取代与跨组冲突**：A 可选接口 `Replacer` + 配置函数 `Conflict`，默认行为不变（推荐）；B 照搬 ssr 的固定组与硬编码矩阵（玩法耦合）；C 不做。
3. **Start 即完成**：A 改 `Action.Start` 签名（推荐：接口一处表达完整，项目未上线可不兼容，影响 movement 两个动作与测试）；B 可选接口（零影响，但多一个隐式约定）。
4. **冻结语义**：A 暂停 / 打断两种模式 + 计数冻结（推荐：state 门控需要计数，定身打断移动需要打断模式）；B 只保留现在的暂停。
5. **任务心跳与计划超时 / 重试**：A 在 `Behavior` 里给任务挂心跳，PlanMission 加 `Timeout` / `Retry`（推荐）；B 任务不 tick，超时由业务另挂心跳。
6. **AI 的扩展范围**：A 事件投递 + 分阶段策略 + `FreezeFor` + 独立 `"ai"` 心跳（推荐）；B 只接心跳；C 另做事件触发引擎（与候选 `trigger_engine` / `eca_man` 一起调研）。
7. **AI 心跳**：A 独立登记 `"ai"`，周期由策略决定（推荐）；B 并进 actionflow 的组心跳。
8. **新增 `gameplay/state`**：A 新包（推荐）；B 不做，控制状态全由 Buff 标签直接查。
9. **state 的写入语义**：A 按来源 + 派生两种，不要 Once、不要匿名计数（推荐）；B 照搬 ssr 四种。
10. **state 的存储**：A DAO 组件、随事务回滚、按声明同步 / 持久（推荐：状态是数据，回滚一致，不需要加载后重设）；B 纯内存、与 runner 同规则不回滚（实现最少，但同步要另投影、加载要重设，与 ssr 一样）。
11. **state 的时长**：A 不内建，时长归 Buff / entitytimer（推荐）；B 内建 `AddFor(d)`（需要 state 自己挂心跳或 entitytimer）。
12. **Buff → state**：A 派生（查询时读 Buff 容器标签，推荐）；B 复制（Buff 施加 / 移除时写 state，两份数据要保持一致）。
13. **分期顺序**：按上表 D21-1 → D21-5，前提 D20 P2 先合入（推荐）；或先做 state（D21-2/3）再做 actionflow 扩展。

**维护者决定（2026-10-21）**：以上 13 条全部按推荐（A）：1 框架提供 `actionflow.Behavior`；2 可选 `Replacer` + 配置函数 `Conflict`，默认行为不变；3 改 `Action.Start` 签名支持 Start 即完成；4 暂停 / 打断两种冻结模式 + 计数冻结；5 `Behavior` 给任务挂心跳、`PlanMission` 加 `Timeout` / `Retry`；6 AI 事件投递 + 分阶段策略 + `FreezeFor` + 独立 `"ai"` 心跳；7 AI 心跳独立登记、周期由策略决定；8 新增 `gameplay/state`；9 按来源 + 派生两种写入语义；10 DAO 组件、随事务回滚、同步 / 持久按声明（缺省不持久）；11 不内建时长；12 Buff → state 用派生；13 按 D21-1 → D21-5 的顺序（D20 P2 已合入 main `382eb10c`）。

#### 6. 调研一 · 实施

**维护者 2026-10-21 决定：Agent 更名为 Behavior**（`actionflow.Agent` → `actionflow.Behavior`，`NewAgent` → `NewBehavior`，`AgentConfig` → `BehaviorConfig`）：Agent 容易和 AI agent 混淆；它是 action_list + mission + 接 AI 的融合，是实体的“行为组件”（运行时组件，不带 DAO）。`ActionList` 接口名与 `ai.Controller` 不变，Behavior 是 `ActionList` 的默认实现。改名在 D21-5 之后单独提交（refactor），下文与上文方案里的名字已统一改成 Behavior（ssr state 模块自己的 `Agent` 除外）。

分期结果按完成顺序写在下面，最后是“调研一 · 收尾”。

##### D21-1 实施结果（2026-10-21）

**状态：已实施**（分支 `feat/d21-gameplay`，本地提交 `86c6dc1c`，由维护者推送）。用法 [guide/10-time §4C](../framework/guide/10-time.md#4c-行为控制behavior取代冲突与冻结)，契约 [impl/10-time §2D](../framework/impl/10-time.md#2d-行为控制behavior取代冲突冻结与状态)，边界 [KNOWN-LIMITS“行为控制”](KNOWN-LIMITS.md#行为控制d21)。codebase-memory 图的 generation 早于本轮，D21 新文件不在图里，结构结论以 worktree 源码为准。

**API 最终形状**：

```go
// gameplay/actionflow
type Action interface { Kind() ActionKind; Start(*ActionContext) (done bool, result ActionResult, err error); Tick(*ActionContext) (bool, ActionResult); Cancel(*ActionContext, string) }
type Replacer interface{ ReplaceBy(ctx *ActionContext, next ActionSnapshot) ReplaceDecision } // ReplaceAllow / ReplaceKeep / ReplaceReject
type Conflict uint8                                                                            // NoConflict / BlockIncoming / PreemptRunning
ActionRunnerConfig.Conflict func(running, incoming ActionSnapshot) Conflict
ActionRunnerHooks.OnFreeze func(group ActionGroup, frozen bool)
func (r *ActionRunner) EndAction(id int64, force bool, reason ActionReason) error
func (r *ActionRunner) Freeze(group ActionGroup, mode FreezeMode) error // FreezePause / FreezeCancel，计数
func (r *ActionRunner) Recover(group ActionGroup, now time.Time) error  // 计数归零才解冻
func (r *ActionRunner) Frozen(group) bool / FreezeCount(group) int
var ErrActionGroupNotFrozen, ErrActionNotFound, ErrReplaceRejected, ErrBlockedByGroup
type Observer interface{ OnActionEnd(id int64, kind ActionKind, reason ActionReason); OnMissionEnd(Mission, ActionReason) }
func NewBehavior(owner entity.IThreadSafeEntity, BehaviorConfig{Registry, GroupForKind, Conflict, DefaultMission, Beat *BeatConfig, Observer, Now, OnError, MaxDeferredCommands, MaxDeferredSteps}) (*Behavior, error)
func (a *Behavior) Runner() / Beat() / Missions() / Clear(reason) + ActionList 全部方法
type ActionList interface { …; EndAction(id, force, reason) error; Freeze(group, mode) error; Recover(group) error; Frozen(group) bool; … } // 去掉 Frozen(group) / Recover(group) / GetFrozen
```

**实施中的决定（方案没写死的细节，按“不引入”清单与 roost-coding 定）**：

1. **被拒绝的新动作是否发 OnEnded**：与现有的“冻结”一致——最外层直接调用被拒（`ErrReplaceRejected` / `ErrBlockedByGroup`）时返回错误、不发 OnEnded（调用方没拿到 ID）；回调里延后的（ID 已交出）发一次失败状态的 OnEnded。`ReplaceKeep` 总是给 ID 并发一次取消状态的 OnEnded，`Start` 返回 nil（方案原文）。
2. **跨组冲突也约束排队项**：排队项成为当前动作时同样过 `Conflict`，被挡以失败结束、报告、接着试下一个——否则一个被挡的 Start 可以经 Enqueue 绕过规则。此时同组空闲，不问 `Replacer`。
3. **`FreezeCancel` 只打断当前动作，排队项保留**（ssr 的三组都关了排队，没有先例）：打断是“停下正在做的事”，排队的计划是否作废由业务决定（`ClearQueue` / `EndAction`）。列为待拍板 ①。
4. **冻结的组停心跳**：新增 `ActionRunnerHooks.OnFreeze`（只在计数 0↔1 时调用），`BeatRunner` 用它停 / 重挂组的心跳；暂停期间不占调度，解冻后第一拍的 `Elapsed` 从重新启动算起（暂停的时间不计入移动）。
5. **`EndAction` 的延后执行找不到时不报告**：回调里按 ID 结束一个随后在同一轮里已经结束的动作是正常时序，不算错误；最外层直接调用找不到返回 `ErrActionNotFound`。
6. **`movement` 的 Start 即完成用 `Advance(token, 0)`**：不在动作里另写一套“是否已到达”的判断；为此 `walkPath` 让与当前位置重合的途经点在 0 时长下也越过（原来 `budget > 0` 才进循环，有剩余距离时行为不变）。
7. **Behavior 的“先任务后观察者”在任务回调里有一处例外**（任务回调期间的送达延后、观察者当次收到），不为它在 Behavior 里另加一层延后队列（方案“不引入”的复杂调度层），写进 KNOWN-LIMITS。
8. **测试里的 `planActionList` 保留**：它测的是“业务自己接线”的底层路径（`ActionRunner` + `MissionRunner` 直接接），`Behavior` 的同类行为在 `behavior_test.go` 另测。

**红绿证据**：新功能，实现与测试同轮写成；红用变异核对给出（每个强制点改掉一处，对应用例在当前代码上变红，原文）：

| 变异 | 变红的用例与原文 |
| --- | --- |
| D1 `start` 忽略 `done` | `TestAnActionThatCompletesInStartEndsAtOnceAndTheQueueMoves`：`after an instant action: current &{instant ...} (want the queued one), instant starts 1 cancels 0, queued starts 0`；`TestAMoveThatIsAlreadySettledEndsInStartWithoutAHeartbeat`：`ended inside the handler after each start: [0 1 2]; registered 1` |
| D2 不问 `Replacer` | `TestTheCurrentActionCanKeepItselfOrRejectAReplacement`：`keep: Start = 2, <nil>; current kept false; new starts 1; current cancels 1` |
| D3 不认 `BlockIncoming` | `TestConflictRulesBlockOrPreemptOtherGroups`：`blocked start: <nil> starts 1` |
| D4 抢占时推进对方队列 | 同上：`preempt: <nil> current3 true current1 &{later walk ...} walk cancels 1 later starts 1 queue 0` |
| D5 `EndAction` 不找排队项 | `TestEndActionEndsTheCurrentOrAQueuedActionByID`：`actionflow: action not found: 3` |
| D6 冻结不计数（Recover 清零） | `TestFreezingIsCountedAndCancelModeInterruptsTheCurrentAction`：`one of two sources recovered: <nil> frozen false` |
| D7 `FreezeCancel` 不打断 | 同上：`cancel freeze: current &{moving ...} cancels 0 queue 1 next starts 0`；`TestCancelFreezeFromACallbackInterruptsAfterTheCallback`：`after the callback: current &{moving ...} cancels 0` |
| D8 冻结不停组心跳 | `TestAFrozenGroupStopsBeatingAndResumesOnRecover`：`paused group: active true registered 1`；`TestFreezingTheMoveGroupPausesOrInterruptsTheMove`：`paused move: registered 1 ended []` |
| D9 先观察者后任务 | `TestBehaviorDeliversActionEndsToTheMissionBeforeTheObserver`：`observer saw [action 1 2 step 0 action 2 2 step 1 mission 1 2], want [action 1 2 step 1 mission 1 2 action 2 2 step -1]` |
| D10 不打任务标签 | `TestBehaviorEndsAMissionsActionsWithTheMissionOnly`：`mission tags: step 0 (mission 1) ai 0` |
| D11 `Clear` 先动作后任务 | `TestBehaviorClearLeavesNothingRunning`：`after Clear: group 1 <nil> group 2 <nil> second starts 1 mission <nil>` |
| D12 0 时长不越过重合的途经点 | `TestAMoveThatIsAlreadySettledEndsInStartWithoutAHeartbeat`：`ended inside the handler after each start: [0 2 3]; registered 0` |

**验证**：gofmt；`GOWORK=off go vet ./...`；`GOWORK=off go test -race -count=1 ./gameplay/... ./wiring/...`；涉及心跳的用例（actionflow 的心跳用例、movement 的 nest 用例）`-race -count=30` 全部通过；根包 `GOWORK=off go test -count=1 .` 通过（退出码均为 0）。

**待拍板（先按推荐做了）**：① `FreezeCancel` 是否连排队项一起丢弃——现按“只打断当前、保留排队”（推荐：排队计划的去留由业务决定）；备选 B：打断时丢弃本组排队项（各发一次取消的 OnEnded）。

##### D21-2 实施结果（2026-10-21）

**状态：已实施**（本地提交 `849031fa`）。新包 `gameplay/state`，用法 [guide/10-time §4C](../framework/guide/10-time.md#4c-行为控制behavior取代冲突与冻结) 的“状态位”，契约 [impl/10-time §2D](../framework/impl/10-time.md#2d-行为控制behavior取代冲突冻结与状态) 第 6～8 条，边界 [KNOWN-LIMITS“行为控制”](KNOWN-LIMITS.md#行为控制d21)。

**API 最终形状**：

```go
// gameplay/state
type ID uint16; type Source struct{ Kind uint8; ID int64 }
type Def struct{ ID ID; Name string; Sync, Persist bool }
func NewCatalog(defs ...Def) (*Catalog, error); (*Catalog).Def / Lookup / IDs
func NewStateDao(id int64, dbName string, catalog *Catalog) *StateDao // DaoInterface + MutationParticipant + PersistedDaoLoader + 回滚快照；FieldStates、CollectionName = "entity_state"
func NewSet(dao *StateDao, SetConfig{OnError}) *Set
func (s *Set) Add / Remove(id, src) error; RemoveSource(src) ([]ID, error); Provide(id, func() bool) error; Changed(ids...); Refresh()
func (s *Set) Has(id) bool; Any(ids...) bool; Satisfies(Requirement{All, None}) bool; Sources(id) []Source; Active() []ID; Synced() []ID
func (s *Set) Watch(id, func(Change)) (Handle, error); Unwatch(Handle); NewWatcher() *Watcher // Watcher.Watch / Close
type Freezer interface{ Freeze(actionflow.ActionGroup, actionflow.FreezeMode) error; Recover(actionflow.ActionGroup) error } // actionflow.Behavior 实现
func Gate(set *Set, target Freezer, onError func(error), rules ...Rule{State, Group, Mode}) (*Gated, error); (*Gated).Close()
var ErrCatalogInvalid, ErrUnknownState, ErrNotInTransaction, ErrProviderInvalid, ErrNotifyRunaway
```

**实施中的决定 / 与方案的偏差**：

1. **观察者里改状态：改动立即生效，通知排到下一轮**（方案 S4 写的是“改状态延后到外层调用返回后执行”）。延后改动会让观察者里的 `Add` 拿不到结果、`Has` 读到旧值；改为立即改、通知按轮次补发，同样做到“回调运行期间不嵌套通知、有界（64 轮）”，观察者看到的总是当前值。
2. **通知按“观察者上次看到的值”算差、且每次比较全部被观察状态**：这是处理“状态随回滚、观察者副作用不随回滚”的办法——回滚期间不能调观察者（回滚中再改别的组件会被拒），回滚后观察者看到的旧值与实际不同，下一次任何改动或 `Refresh` 就按差补发。观察者因此总是收到交替的值，不会连收两次同一个值。
3. **门控每条规则至多持有一次冻结**（不是“状态每生效一次就 Freeze 一次”）：结合第 2 条，回滚造成的多余冻结在下一次通知时撤销，计数不会累积、组不会永久冻结。方案没写回滚时门控怎么办；按 D20 决定 3 的口径（runner 不回滚、由业务保证），框架保证“最终一致、不累积”，立即一致由业务在回滚后调 `Refresh`。列为待拍板 ②。
4. **持久状态只能在事务里改**：事务外改 `Persist` 状态返回 `ErrNotInTransaction`（无法落库，拒绝而不是静默丢失）；不持久的允许在加载 / 新建初始化阶段直接改（NPC 出生时的标记）。`CombatDao` 是一律 panic，这里改成返回错误，因为状态写入是常规业务调用。
5. **持久文档的键是状态名**，`RestorePersisted` 跳过不认识的名字与不再声明 `Persist` 的状态（与 D13“旧键不加载、整文档写回即删除”一致，不报警）；只有一个字段位 `FieldStates`，持久写整个 `states` 子文档。
6. **同一状态可以同时有来源与派生函数**（任一为真即生效）：例如“被定身”既来自 Buff 标签（派生），也可以由剧情直接加来源。
7. **`Set.Synced` 给同步打包用**：框架的同步内容由业务的 packer 打包，`state` 不自带 packer；变化时标 `StateDao` 的同步位 `FieldStates`（与 `CombatDao` 相同的做法）。派生状态在 `Changed` 时标同步位。
8. **`Gate` 的签名多一个 `onError`**（方案草案是 `Gate(set, rules...)`）：`Freeze` / `Recover` 的错误（例如 `FreezeCancel` 打断时动作 `Cancel` panic）需要有去处。

**红绿证据**（新包，实现与测试同轮写成；变异核对，原文）：

| 变异 | 变红的用例与原文 |
| --- | --- |
| S1 通知不按差（每次改动都通知） | `TestADerivedStateIsComputedAndNotifiedOnlyWhenItChanges`：`watchers saw [first false second false first false second false …]`（同一值反复直到截停，原文过长，此处截取开头），`want [first true second true first false second false]` |
| S2 同一来源重复加会叠加 | `TestASourcedStateHoldsWhileAnySourceRemains`：`rooted true sources [{1 1} {1 1}], want only buff1 left` |
| S3 不登记逆操作 | `TestStatesFollowTheTransaction`：`state_test.undo: states survived the rollback: [1 3 5]` |
| S4 不持久的状态也标持久 | 同上：`nest: transaction commit rejected`（memory 持久级别下改不持久的状态被拒） |
| S5 事务外允许改持久状态 | `TestAPersistentStateCannotChangeOutsideATransaction`：`persistent add outside a transaction: <nil> has true` |
| S6 不标同步脏 | `TestStatesFollowTheTransaction`：`a synced state change left no sync dirty bit` |
| S7 持久文档含全部状态 | `TestTheDaoPersistsOnlyPersistentStatesByName`：`restore under a changed catalog: <nil> map[1:[{1 1}]]` |
| S8 门控不按规则持有（每次通知都冻结） | `TestGateFreezesActionGroupsWhileStatesHold`：`stunned + silenced: freeze count 3 registered 0 ended [1 4 action group frozen]` |
| S9 `Refresh` 什么都不做 | `TestGateRealignsAfterARolledBackStateChange`：`Refresh did not realign the gate` |
| S10 `Close` 不撤销冻结 | `TestGateAlignsWhenInstalledAndReleasesOnClose`：`Close kept the freeze` |
| S11 观察者里的改动嵌套通知 | `TestWatcherChangesAreNotifiedInTheNextRoundAndRunawayIsBounded`：`saw [rooted true manual false manual true manual set inside: true], want [rooted true manual false manual set inside: true manual true]` |
| S12 装门控时不对齐 | `TestGateAlignsWhenInstalledAndReleasesOnClose`：`a gate installed while rooted did not freeze` |

**验证**：gofmt；`GOWORK=off go vet ./...`；`GOWORK=off go test -race -count=1 ./gameplay/... ./wiring/...`；`gameplay/state` 全部用例（含真实 Nest + 心跳的门控用例）`-race -count=30`；根包 `GOWORK=off go test -count=1 .`；`go run ./cmd/glsvet ./gameplay/state/` 无提示。退出码均为 0。

**待拍板（先按推荐做了）**：② 门控在 handler 回滚后的一致性——现按“最终一致”：冻结保留到下一次状态通知（或业务调 `Refresh`），每条规则至多一次、不累积（推荐：与 runner 不回滚的口径一致，框架不在回滚路径上改别的组件）；备选 B：框架在回滚后自动补发（需要 Nest 提供“回滚完成后、实体锁释放前”的钩子，当前没有）。

##### D21-3 实施结果（2026-10-21）

**状态：已实施**（本地提交 `575f216a`）。位置按方案 S6 放在 `gameplay/skill/combatcomponent`（`state_bridge.go`）：`combatcomponent` import `state`，`state` 不认识 combat。用法 [guide/10-time §4C](../framework/guide/10-time.md#4c-行为控制behavior取代冲突与冻结) 的“Buff → 状态”，契约 [impl/10-time §2D](../framework/impl/10-time.md#2d-行为控制behavior取代冲突冻结与状态) 第 9 条。

**API**：`type StateBinding struct{ State state.ID; Tags []combat.Tag }`；`func (c *CombatComponent) BindStates(set *state.Set, bindings ...StateBinding) error`（构造时一次；每个状态一个提供函数，持久状态、空标签被拒）；`func (c *CombatComponent) HasBuffTag(tag combat.Tag) bool`。

**实施中的决定**：① 一个状态绑多个标签（任一即生效），方案草案是“一个标签一个提供函数”（`StateProvider(tag)`），合成一个绑定避免同一状态登记多个提供函数；② 通知点是“改 buff 集合”的 mutator 加 `OnInitFinish`（加载出的 buff 在这里让已经装好的门控跟上），`SetBuffDueTick` 不改集合、不通知；③ 回滚后派生状态的 `Has` 当场就对（读的是回滚后的容器），观察者在下一次通知时跟上（与 D21-2 第 2 条同一规则）。

**红绿证据**（变异核对，原文）：

| 变异 | 变红的用例与原文 |
| --- | --- |
| B1 `TickBuffs` 不通知 | `TestBuffTagsDriveDerivedStates`：`watchers saw [1 true 2 true 2 false 1 false], want [1 true 2 true 1 false 2 false 1 true 1 false]` |
| B2 `DispelBuffs` 不通知 | 同上：`watchers saw [1 true 2 true 1 false 2 false 1 true], want [1 true 2 true 1 false 2 false 1 true 1 false]` |
| B3 提供函数缓存容器（等于复制，回滚后不跟） | 同上：`a rolled-back root buff still roots` |
| B4 `ApplyBuff` 不通知 | 同上：`watchers saw [], want [1 true 2 true 1 false 2 false 1 true 1 false]` |
| B5 `OnInitFinish` 不通知 | `TestLoadedBuffsReachBoundStatesOnInitFinish`：`watcher saw [] after OnInitFinish, want [true]` |

**验证**：gofmt；`GOWORK=off go vet ./...`；`GOWORK=off go test -race -count=1 ./gameplay/... ./wiring/...`；根包 `GOWORK=off go test -count=1 .`；`go run ./cmd/glsvet ./gameplay/skill/combatcomponent/` 无提示。退出码均为 0。


##### D21-4 实施结果（2026-10-21）

**状态：已实施**（本地提交 `79138636`）。用法 [guide/10-time §4C](../framework/guide/10-time.md#4c-行为控制behavior取代冲突与冻结) 的“任务心跳”“计划步骤的时限与重试”“AI 心跳与定时冻结”，契约 [impl/10-time §2D](../framework/impl/10-time.md#2d-行为控制behavior取代冲突冻结与状态) 第 10～12 条。

**API 最终形状**：

```go
// gameplay/actionflow
func (a *Behavior) MissionReg() *heartbeat.Reg                                // "<Beat.Name>/mission"
type MissionStep struct{ …; Timeout time.Duration; Retry int }             // 每次尝试的时限；失败 / 过期原地重试次数
type MissionPlan struct{ …; Period time.Duration }; const DefaultMissionPeriod = 100 * time.Millisecond
func (m *PlanMission) TickInterval() time.Duration                         // 只在当前步有时限时非 0
// gameplay/ai
func NewBeatController(hooks ControllerHooks, beat BeatConfig{Host, Name, Period, Options}) (*Controller, error) // DefaultBeatName = "ai"，DefaultBeatPeriod = 500ms
func (c *Controller) BeatReg() *heartbeat.Reg
func (c *Controller) FreezeFor(d time.Duration, interrupt bool); FrozenUntil() time.Time
// gameplay/heartbeat
func PeriodAtMost(want time.Duration) time.Duration                        // 动作组、任务、AI 共用的取档
```

**实施中的决定 / 与方案的偏差**：

1. **任务不实现 `TickIntervaler` 就不挂心跳**（动作不实现时按缺省周期）：任务历来由动作结束推进，缺省不 tick 才不会让每个有任务的实体多一条心跳。
2. **对齐点是 `MissionRunner` 的 `OnState` + `OnChanged`**：任务的周期常取决于 Start 之后的内部状态（`PlanMission` 要看当前步），`OnState(true)` 发生在 Start 之前，所以每次任务推进（`OnChanged`）都重新对齐；对齐只比较登记状态，没有变化时不产生启停。
3. **超时以 `EndAction(动作, force, 过期)` 结束这一步的动作**（方案“以 ActionStatusExpired 结束当前步的动作”）：动作照常 `Cancel`，结论经 `OnActionEnd` 回来，与其他失败同一条路径；每次重试重新计时。
4. **取消不重试**：方案只写“失败后原地重试”；`Canceled` 意味着有人有意结束了它（被别的动作取代、业务结束、冻结打断），重试会和那一方抢，所以直接走 `OnFail`。列为待拍板 ③。
5. **AI 心跳不经 `Behavior`**：方案写“`ai.BeatController`（或 `Behavior` 的选项）”；放在 `ai` 包（`NewBeatController`），因为 actionflow 不导入 ai，而 AI 的周期取决于策略、冻结——都是 `Controller` 自己的状态。
6. **每拍之后重新对齐 AI 周期**：策略可以随自身状态改 `TickInterval`，不需要额外的通知入口。
7. **`FreezeFor(d, interrupt)` 覆盖之前的冻结，`Freeze` / `Recover` 清定时**；定时冻结期间心跳照常跳（用来检查到期），无定时冻结时停。
8. **取档规则提到 `heartbeat.PeriodAtMost`**：`BeatRunner`、任务心跳、AI 心跳原来各写一份同样的循环。

**红绿证据**（变异核对，原文；`&{…}` 处原文是带指针地址的结构体，省略）：

| 变异 | 变红的用例与原文 |
| --- | --- |
| H1 任务心跳从不挂 | `TestAMissionStepTimesOutRetriesAndFailsOnTheMissionHeartbeat`：`mission with a timed step: reg <nil> registered 1 (want the mission's and the action group's)` |
| H2 `Tick` 不看时限 | 同上：`first timeout: cancels 0 starts 1 mission &{…}` |
| H3 取消也重试 | `TestTheMissionHeartbeatFollowsTheCurrentStep`：`a canceled step was retried: starts 2 mission &{…} log [action 1 2 step 1 action 2 4 step 1]` |
| H4 重试次数不按步清零 | `TestPlanRetriesAreCountedPerStep`：`mission {Status:3 CurrentStep:-1 CurrentAction:0 LastResult:{Status:3 Reason:flaky}}, starts 2 / 1 (want each step retried once, then success)` |
| H5 任务结束 / 无时限的步不停心跳 | `TestAMissionStepTimesOutRetriesAndFailsOnTheMissionHeartbeat`：`after the mission failed: active true registered 1 info {Status:5 …}`；`TestTheMissionHeartbeatFollowsTheCurrentStep`：`untimed second step: mission beat active true current &{untimed …}` |
| H6 重试不重新计时 | `TestAMissionStepTimesOutRetriesAndFailsOnTheMissionHeartbeat`：`observer saw [action 1 5 step 0], want [action 1 5 step 0 action 2 5 step 0 mission 1 5]` |
| H7 冻结时 AI 照跳 | `TestTheAIBeatsAtTheStrategysIntervalAndStopsWhenIdleOrFrozen`：`frozen: true 200ms registered 1`；`TestFreezeForRecoversOnTheFirstBeatAfterItEnds`：`Freeze after FreezeFor: frozen true beating true ended 1` |
| H8 定时冻结不到期 | `TestFreezeForRecoversOnTheFirstBeatAfterItEnds`：`at the end: ticks 0 frozen true until 2027-01-15 16:00:01 +0800 CST` |
| H9 周期不看策略 | `TestTheAIBeatsAtTheStrategysIntervalAndStopsWhenIdleOrFrozen`：`300ms strategy: true 500ms (want the 200ms tier)` |
| H10 每拍后不重新对齐 | 同上：`strategy slowed down: true 200ms ticks 1` |
| H11 `FreezeFor` 不结束在途动作 | `TestFreezeForRecoversOnTheFirstBeatAfterItEnds`：`FreezeFor: frozen true until 2027-01-15 16:00:01 +0800 CST beating true ended 0` |
| H12 定时冻结也停心跳 | 同上：`FreezeFor: frozen true until 2027-01-15 16:00:01 +0800 CST beating false ended 1` |

**验证**：gofmt；`GOWORK=off go vet ./...`；`GOWORK=off go test -race -count=1 ./gameplay/... ./wiring/...`；涉及心跳调度的用例（`TestPlanStepTimeoutsRunOnTheRealMissionHeartbeat`、`TestTheAIDecidesOnItsOwnHeartbeat`，连同前几期的 `TestAFrozenGroupStops…`、`TestGate…`、`TestStatesFollow…`、`TestFreezingTheMoveGroup…`、`TestAMoveThatIsAlreadySettled…`）`-race -count=30` 全部通过；根包 `GOWORK=off go test -count=1 .`。退出码均为 0。

**待拍板（先按推荐做了）**：③ 被取消的步骤是否重试——现按“不重试、直接 OnFail”（推荐：取消是有意的结束，重试会与发起取消的一方抢）；备选 B：取消也算失败，按 `Retry` 重试。

##### D21-5 实施结果（2026-10-21）

**状态：已实施**（本地提交 `e95916f7`）。用法 [guide/10-time §4C](../framework/guide/10-time.md#4c-行为控制behavior取代冲突与冻结) 的“AI 事件、分阶段与轨迹”“动作指标”，契约 [impl/10-time §2D](../framework/impl/10-time.md#2d-行为控制behavior取代冲突冻结与状态) 第 13～15 条，指标 [OBSERVABILITY“动作”](../framework/OBSERVABILITY.md#动作gameplayactionflow可选)。

**API 最终形状**：

```go
// gameplay/ai
type Event struct{ Kind uint16; Data any }; type EventStrategy interface{ OnEvent(*Context, Event) }
func (c *Controller) OnEvent(ev Event) bool
func (b *BehaviorContext[C]) HasEvent(kind uint16) bool / TakeEvent(kind) (Event, bool) / Events() []Event / SetStage(name) error
type EventReceived[C] struct{ Kind uint16; Take bool }; type SetStage[C] struct{ Stage string }; type Traced[C] struct{ Path string; Child Node[C] }
func NewStagedStrategy[C](stages map[string]Node[BehaviorContext[C]], start string, options BehaviorStrategyOptions[C]) (*BehaviorStrategy[C], error)
func (s *BehaviorStrategy[C]) SetStage(name string) error / Stage() string; var ErrStageUnknown; const DefaultMaxEvents = 64
BehaviorStrategyOptions[C]{…, MaxEvents int, OnStage func(from, to string), Trace func(path string, status Status)}
func ParseStages[C](data []byte, registry *Registry[C]) (Stages[C]{Roots, Start}, error); func (r *Registry[C]) TraceNodes()
// JSON：{"node": "event", "kind": n, "take": bool}、{"node": "set_stage", "stage": "名字"}、文档顶层 {"start", "stages"}
// gameplay/actionflow
BehaviorConfig.Metrics bool; MetricActionsStarted / MetricActionsEnded / MetricActionsRunning; func (ActionStatus) String() string
```

**实施中的决定 / 与方案的偏差**：

1. **`StagedStrategy` 不是单独的类型**：方案写 `StagedStrategy[C]`；实现上 `BehaviorStrategy` 本身支持阶段，`NewStagedStrategy` 返回 `*BehaviorStrategy[C]`，`NewBehaviorStrategy(root, …)` 是只有一个阶段（名字为空串）的特例——两者共用缓冲、Reset、结束通知的全部逻辑，不重复一份。
2. **策略外的 `SetStage` 立即生效**（方案只写了“在回调里调用时延后到本拍结束”）：业务在事件回调或 handler 里切阶段时，没有“本拍”可等。
3. **事件条件叶子默认不取走**：`EventReceived{Take: false}` 无状态、可以进守卫谓词（JSON 的 `event` 在守卫里只允许不取走）；要“一个事件只触发一次”用 `Take: true`。没有单独做方案里的 `TakeEvent` 叶子，取走是同一个叶子的选项。
4. **事件缓冲满时丢最早的**、不报告：事件是“唤起决策”的提示，最新的更有用；可靠的事实应在 DAO / state 里。
5. **轨迹按节点、由解析器包装**（方案 `Trace func(path, status)`）：路径用 JSON 路径，只有开了 `Registry.TraceNodes` 的解析才包，Go 写的树用 `Traced` 自己包；不开时零开销（不包）。
6. **动作指标缺省关闭**（方案“是否默认开启待定”）：列为待拍板 ④。指标名按命名规范用点号分层（`actionflow.actions.*`）。

**红绿证据**（变异核对，原文）：

| 变异 | 变红的用例与原文 |
| --- | --- |
| E1 冻结时仍投递事件 | `TestEventsReachTheTreeOnTheNextTickAndAreDroppedWhileFrozen`：`an event was delivered to a frozen controller` |
| E2 事件缓冲不设上限 | 同上：`buffer kept [{7 <nil>} {8 <nil>} {6 <nil>}], want the newest two` |
| E3 树里的切阶段立即生效 | `TestStagedStrategySwitchesAfterTheTickAndInterruptsTheOldTree`：`log [interrupt 101 calm -> enraged still calm after asking interrupt 202 enraged -> calm calm -> ] stage "", want [interrupt 101 still calm after asking calm -> enraged interrupt 202 enraged -> calm]` |
| E4 切换不 Reset 旧树 | 同上：`log [interrupt 101 still calm after asking calm -> enraged enraged -> calm] stage "calm", want [interrupt 101 still calm after asking calm -> enraged interrupt 202 enraged -> calm]` |
| E5 不核对 `set_stage` 目标 | `TestParseStagesBuildsStagedTreesWithEventAndSetStageLeaves`：`unknown target: <nil>` |
| E6 取走事件的 `event` 可做守卫谓词 | 同上：`taking guard: <nil>` |
| E7 `Traced` 不报告 | `TestTraceReportsEveryNodeWhenTheRegistryTracesNodes`：`trace [], want [$.root.children[0]=2 $.root.children[1]=3 $.root=3]` |
| E8 `running` 不减 | `TestBehaviorMetricsCountStartsEndsAndRunningActions`：`metrics map[… actionflow.actions.running:3 …], want map[… actionflow.actions.running:1 …]` |

另：E3 第一次的变异（只改策略外 `SetStage` 的入口）没有让任何用例变红——树里的切阶段走 `requestStage`、不经那个入口，变异没有打到强制点；改在 `requestStage` 上之后变红（上表）。

**验证**：gofmt；`GOWORK=off go vet ./...`；`GOWORK=off go test -race -count=1 ./gameplay/... ./wiring/...`；根包 `GOWORK=off go test -count=1 .`（含指标名门禁）；全量 `GOWORK=off go test -count=1 ./...`（见下文“调研一 · 收尾”）。退出码均为 0。

**待拍板（先按推荐做了）**：④ 动作指标是否缺省开启——现按“缺省关闭、`BehaviorConfig.Metrics` 打开”（推荐：每个动作两次注册表写入，全服开销没有测量）；备选 B：缺省开启，按需关闭。

##### 调研一 · 收尾（2026-10-21）

- **提交**（分支 `feat/d21-gameplay`，本地，未推送）：决定记录 `78350b9e`；D21-1 `86c6dc1c` + `bc2bc568`；D21-2 `849031fa` + `f31b2cde`；D21-3 `575f216a` + `24403bcd`；D21-4 `79138636` + `e42e4841`；D21-5 `e95916f7` + `87139cb1`；更名 `08c734b1`。
- **全量验证**：gofmt；`GOWORK=off go vet ./...`；`GOWORK=off go test -race -count=1 ./gameplay/... ./wiring/...`；涉及心跳调度的用例 `-race -count=30`；根包 `GOWORK=off go test -count=1 .`；全量 `GOWORK=off go test -count=1 ./...`。退出码均为 0。
- **待拍板（都已先按推荐实施）**：① `FreezeCancel` 是否连排队项一起丢弃（现：保留）；② 门控在 handler 回滚后的一致性（现：下一次状态通知时纠正、不累积）；③ 被取消的计划步骤是否重试（现：不重试）；④ 动作指标是否缺省开启（现：关闭）。
- **维护者 2026-10-21 决定：上述 4 项全部按推荐（即现状）**：① `FreezeCancel` 只打断当前动作、排队项保留，去留由业务决定；② 门控在 handler 回滚后不由框架补发，下一次状态通知或业务 `Refresh` 时纠正、每条规则至多持有一次冻结；③ 被取消的计划步骤不重试，直接走 OnFail；④ 动作指标缺省关闭，由 `BehaviorConfig.Metrics` 打开。
- **没做 / 不在本轮**：game-demo 与生成器接入 `Behavior`、`state`、AI 心跳（随 D20 P3）；`TickBuffs` 挂心跳（D20 P5）；其余 ssr 候选（`trigger_engine`、`eca_man`、`cooldown`、`timer`、空间、战斗、经济、社交）的调研。

## D22 业务时间取自 ctx、支持变速

**状态：维护者 2026-10-21 提出（在 D21 调研中补充，单列，不并进 D21）：ssr 有个很好的设计——业务里的时间一律从 ctx 取，业务时间因此可以加速、减速。同日维护者决定范围（见下方“维护者决定”），实施中。**

**维护者决定（2026-10-21）**：范围按 ssr 收窄——**全局不做变速，只做场景级**。

- **D22-2（部署级变速）取消**：全局时钟仍然只有偏移，`time.logic_offset` 保持现状，不加协调存储里的时间计划，不加 `time.speed_control`。
- 实施 **D22-1**（业务时间一律从 ctx 取）与**场景时间域**（原 D22-3）。
- §5 的 9 条修订为：
  1. A：业务时间唯一入口是 `fctx.Now()`，每条消息 / 每一拍冻结一次。
  2. A：endpoint 的 `nowUnix` / `nowUnixMilli` 参数保留，改为执行时由生成的分发包装从 `fctx` 填。
  3. 只对挂了时间域的心跳登记生效：调度仍按真实时间；`Beat.Now` / `Elapsed` 用域时间，时停时 `Elapsed = 0`；`Elapsed` 上限乘倍率。
  4. 取消：场景在一个进程内，不需要跨进程协调。
  5. 时间域倍率范围 [0, 100]，允许 0（时停）。
  6. `logic_offset` 保持现状。
  7. A：entitytimer 不接时间域，域内的定时走心跳。
  8. 取消：只有场景时间域，生产可用。
  9. 先做 D22-1，再做时间域。

下文 §1～§5 是决定前的调研与方案原文，其中 3.2、3.3 的部署级变速与 §5 第 4、6、8 条已被上面的决定取代。

### 实施结果（2026-10-21）

现行说明见 [guide/10-time §1A](../framework/guide/10-time.md#1a-业务时间一律取自-ctxd22-1)、[§4D](../framework/guide/10-time.md#4d-场景时间域变速)，契约与变异核对见 [impl/10-time §2E](../framework/impl/10-time.md#2e-业务时间取自-ctxd22-1)、[§2F](../framework/impl/10-time.md#2f-场景时间域)，限制见 [KNOWN-LIMITS](KNOWN-LIMITS.md#业务时间取自-ctx-与场景时间域d22)，变化见 CHANGELOG。

**D22-1 业务时间一律取自 ctx**（决定 ①A、②A）：

- 迁移的调用点（都在 Nest 消息里执行）：心跳驱动缺省时钟（`Beat.Now`、启动时刻）；entitytimer 驱动缺省时钟（`Host` 的增删改、`OnInitFinish`、Tick 的触发时刻——Tick 消息不再带投递时刻，`Host.Tick()` 无参数、新增 `TickAt`）；`actionflow.ActionRunner` / `MissionRunner`、`ai.Controller`、`timer.Scheduler` 没给时间时的缺省；`wiring/heartbeat`、`wiring/entitytimer` 去掉 `SetClock(app.BusinessClock(r).Now)`；Nest 生成器的业务时间参数（`nowUnix` / `nowUnixMilli`）改为分发包装执行时从 `fctx` 填、Sender 不再带；endpoint 骨架不再取业务钟；`roost add timer` 的 Tick handler 与组件 `Tick()`；game-demo 的调用方（领邮件奖励、副本领奖、建 / 入公会、GM 入会、活动开窗 / 结算）、`executionNowUnix` 与 `Controller.BusinessNow` 删除、World 定时器。
- 不在 Nest 消息里的路径照旧读 `app.BusinessClock(registry)`：服务 goroutine（活动协调器、匹配、spawner、mail / rank / chat / account / session 服务经 `Config.Now` 注入）、admin / GM 命令的服务侧、业务时间高水位、启动阶段。边界写在 guide §1A。
- 新增 `fctx.Frozen`（一次执行冻结的派生值）与 `clock.SetSource`（测试换真实时间来源）。
- glsvet 提示（做了）：业务目录里 Nest handler 本身读 `app.BusinessClock` / `clock.Now` / `clock.UnixMilli` 打 `hint:`。理由：最常见的写法就是 handler 里直接读，提示成本低、误报可豁免（`//glsvet:live-business-clock`）；做成失败不对——Nest 之外读 `app.BusinessClock` 是正确的，handler 里量耗时也合法。不跟进 helper，约束本身靠文档。
- 不兼容：生成的 Sender 签名、Tick handler 签名、`entitytimer.Host.Tick`；升级步骤写在 CHANGELOG。

**场景时间域**（决定 ③⑤⑦⑨）：

- 新包 `gameplay/timedomain`：`Domain`（锚点 + 倍率的分段线性映射，`SetRate` 以生效时刻为新锚点、连续；倍率 [0, 100]，0 时停；参数 `atomic.Pointer` 发布、按序号只接受更新的）。参数的权威在场景实体的 DAO 字段（`Fields`），随事务回滚、标 sync 下发客户端；提交后（`AfterCommit`）发布给成员。一条消息里第一次读取冻结（`fctx.Frozen`）。成员读数取“已交出的最大域时间”托底，保证单调。
- 持久与否：由业务选，推荐随进程结束的副本用 `nopersist,sync`；要持久的场景用持久字段，重载后按存下的锚点与倍率接着走（停服期间同样按倍率前进）。理由：副本场景随进程结束，不需要持久；框架不替业务决定，`Fields` 就是 DAO 字段，持久只是字段标记。
- `heartbeat.WithTimeDomain(d)`：调度不变，`Beat.Now` / `Elapsed` 用域时间，时停 `Elapsed = 0`，上限乘倍率。对 heartbeat 的改动只限 `Beat.Now` 的来源、上限与启动时刻，投递层不动。
- actionflow / ai（D21 合入后补，推荐并实施：改动只是透传 `WithTimeDomain` 与缺省时间取域时间，不需要另行拍板）：`actionflow.BeatConfig.TimeDomain`、`ai.BeatConfig.TimeDomain`。动作组心跳、`Behavior` 的任务心跳、AI 心跳挂上域；心跳之外调用 runner 时 `ActionContext.Now` 也是域时间；`Behavior` 与 `ai.Controller` 没配 `Now` 时取域时间，所以计划步骤 `Timeout`、`FreezeFor` 与各自心跳的到期判断同一把尺子。端到端用例经真实 Nest、运行中的心跳驱动与 `BeatRunner`。
- D21 新增的取时间处逐个核对（D22-1）：`Behavior.now` 原缺省 `clock.Now`，改为 `fctx.Now`（先红：`Behavior action context Now = …002, want … frozen …001`）；任务心跳、AI 心跳用 `Beat.Now`（驱动缺省 `fctx.Now`）；`ai.NewBeatController` 与 `FreezeFor` 经 `Controller.now`（D22-1 已改 `fctx.Now`）；计划步骤 `Timeout` 的起点是 `MissionContext.Now`（经 `Behavior.now`），到期判断是任务心跳的 `Beat.Now`。
- 技能 / Buff：本轮没有自动接线；**D20 P5 已接上**（`combatcomponent.BeatConfig.TimeDomain`、`RuntimeBeatConfig.TimeDomain`，见 D20“P5 实施结果”）。
- entitytimer 不接时间域（决定 ⑦）。

**证据**：每项先在当前代码上变红（单步时钟让“第二次现读”一定不同）再改：心跳 `Beat.Now = …00.004, but the beat's message froze …00.003`；entitytimer `Fire.Now = …00.005, but the Tick message froze …00.007`；生成的分发包装 `handler got business time 1800000000001, but its message froze 1800000000003`；actionflow / AI `ActionContext.Now = …00.002, want … frozen …00.001`；glsvet `hints = 0, want 3`。时间域 9 个变异核对各让对应用例变红（失败文本在 impl §2F）。验证命令与结果见本轮汇报与 CHANGELOG。

**未做 / 待定**：game-demo 的场景没有挂时间域、客户端（C#）没有外推域时间的接口；`scripts/testdata` 的持久化恢复消费夹具（需要真实 Mongo）本轮没有在真实依赖上跑。

**读了什么**：ssr 只读 `ssr/kingdom/framework/kctx`、`framework/dispatcher`、`view/scene_time.go`、`entities/scene/internal/timer.go`、`modules/level`（变速入口）、`modules/heartbeat`、GM 改时间的入口，以及 ssr 依赖的 `watcher/misc/chrono`（下文 ssr 出处相对 `ssr/kingdom/`，chrono 相对 `watcher/misc/chrono/`）。roost 读 main（`3e7b4011`）的 `infra/base/clock`、`infra/base/fctx`、`framework/app`（业务时钟、高水位）、`framework/nest`（消息上下文）、`framework/nest/entitytimer`、`gameplay/heartbeat`、`gameplay/actionflow` / `gameplay/ai`、`infra/base/timer`、codegen 的 endpoint 业务时间参数，以及 [guide/10-time](../framework/guide/10-time.md)。

### 1. ssr 的做法

ssr 有**两层**时间，变速只在第二层：

**第一层：进程业务时钟（chrono，只有偏移）**

- chrono 是一个缓存的时钟：后台 goroutine 每 5ms 一拍，把 `real = time.Now()` 与 `shifted = real + shift` 存进两个原子量；`chrono.Now()` 读 `shifted`，`chrono.RealNow()` 读 `real`（`chrono.go:123-133`、`325-327`、`355-357`；拍间隔 `manager/boot_release_man/internal/system/chrono.go:14-16`）。所以 `chrono.Now()` 的精度是 5ms，同一拍内读到同一个值。
- **只有偏移，没有倍率**：`UpdateTimeShift(shift)` 改偏移，入口是 GM / 调试命令（`http/httpcmd/http_post/timeShift.go:31-48`、`http/httpcmd/http_post/now.go:45-60`、`manager/system/system.go:87-91`、`modules/refresh/internal/refresh_debug.go:20-35`，最后一个只在 debug 模式可用）。偏移只在内存，不持久；可以往回调，没有单调保证。`System.SetNow` 把已偏移的 `chrono.Now()` 当真实时间算新偏移，第二次设置会把旧偏移叠错（`manager/system/system.go:87-91`）。
- **ctx 时间**：每条消息开始时 `ctx.Init` 取一次 `ctx.Now = chrono.Now()`（`framework/dispatcher/prepare/prepare.go:48`、`framework/kctx/context.go:226-230`），同一条消息里读 `ctx.Now` 都是同一个值。心跳回调拿到的 `cur` 就是这条心跳消息的 `ctx.Now`（`modules/heartbeat/internal/auto_heartbeat.go:23-28`）。但业务里直接读 `chrono.Now()` 的地方与读 `ctx.Now` 的数量相当（modules / entities 下约 300 处对 350 处），“一律从 ctx 取”并没有被强制。

**第二层：场景时间（变速在这里）**

- **范围**：只有 SE 副本场景可以变速（`entities/scene/internal/limit.go:119-126`）；其他场景的场景时间就是 `chrono.Now()`（`entities/scene/internal/timer.go:149-152`）。
- **实现**：每个场景一个 timer，挂在 chrono 上每 5ms 积分一次：`now += (real_now - lastBeat) × rate`（`entities/scene/internal/timer.go:172-191`），起点是场景创建时的 `chrono.Now()`（`timer.go:50-63`，源码注释说用时间戳起点是为了技能 CD）。倍率是若干“通道”的乘积（节奏 Rhythm、时停 SpeedBreaker），`ChangeTimeFlowRate(rate, Fast|Slow)` 设某个通道为 `rate` 或 `1/rate`，`FrozenTimeFlowRate` 设为 0（时间停止），每个通道可带“多久后自动恢复”（真实时间）（`timer.go:38-44`、`69-146`；接口 `view/scene_time.go:32-45`）。积分方式保证**倍率改变时连续不跳变**，倍率 ≥ 0 时单调不减。倍率改变后同步给客户端（场景组件 `SetTimeRate`、关卡 `OnTimeRateChange`，`timer.go:65-67`、`84-85`）。
- **ctx 快照**：场景时间在一条消息里第一次读取时存进 `ctx.SceneNowNano`，之后同一条消息里都读这个值，消息结束清掉（`entities/scene/internal/timer.go:148-160`、`framework/kctx/context.go:66-79`、`framework/dispatcher/dispatcher.go:118`、`129`、`150`）。业务经 `view.CurTimeNano(entity)` 读实体所在场景的时间（约 190 处，`view/scene_time.go:79-129`）。
- **用途（生产玩法）**：玩家请求战斗加速（FightFast）、出牌时慢放（ShowCardSlow）、捕宠瞄准时慢放并在上限时长后自动恢复（`modules/level/internal/level_handler.go:239-317`）；关卡脚本“除某人外全部时停”（`modules/level/internal/level_export.go:1735-1765`）与触发器改流速（`modules/level/internal/trigger/level_trigger_action.go:1283`）。倍率有上下限检查（`timeChangeRateLimit`）。
- **对各子系统的影响**：
  - **心跳**：调度按真实节拍（chrono 的拍、实体心跳的档位不缩放）；动作在 `Loop` 里用场景时间算 `elapsed`（`action_list/internal/action_list.go:428-433`），所以加速时每拍走得更远、时停时 `elapsed = 0`，原地不动。
  - **技能冷却 / 战斗计时**：读场景时间（`CurTimeNano`），随倍率；`TransDuration` 只按当前倍率换算一段时长（接口注释“只能获取当前流速的转变”，`view/scene_time.go:41-42`），仓库里没有调用方。
  - **延迟消息**（`g.Matrix.Delay`，如 `SetStateWithTimeout`）按真实时间，**不随倍率**（`modules/state/internal/state.go:175-182`）。
  - **持久化的到期时间**：存 chrono 时间（例如城堡保护期，`entities/castle_brief/internal/castle_brief.go:199-207`），不受场景倍率影响；只受第一层偏移影响。
  - **仍用真实时间**：消息发送时间戳（`framework/matrix/message.go:151`）、延迟队列、chrono 自身的拍。
- **问题（不照搬的理由）**：倍率通道数组在 RPC / 关卡线程里写、在 chrono goroutine 的 `Beat` 里读（`timer.go:80-84` 对 `172-190`），`RecoverTimeFlowRate` 还会从 `Beat` 里被调用，数组不是原子的，是数据竞争；场景时间精度 5ms；第一层偏移可以往回走、不持久、`SetNow` 有叠加错误。

**用在哪些场景**：第一层（偏移）——调试、测试、GM 把服务器时间拨到活动时间预演、跨日 / 跨周重置验证；第二层（场景倍率）——生产玩法：副本战斗 2 倍速、演出 / 瞄准慢放、技能时停。

### 2. roost 现状

- **业务时钟只有偏移**：`clock.Business` = 真实时间 + `time.logic_offset`（`infra/base/clock/clock.go:28-70`）；进程级时钟 `clock.Now()` 读原子偏移（`clock.go:95-155`），App 启动时 `SetOffset` 一次、运行期不改（`framework/app/app.go:218-220`）；生产强制为 0（[guide/10-time §2](../framework/guide/10-time.md#2-业务高水位)）。`app.BusinessClock(r)` 取 Registry 里的业务时钟，退回进程级（`framework/app/business_clock.go:15-26`）。业务时间有部署级高水位，只许前进，存在协调存储，启动时检查、运行中每 10 秒推进（`framework/app/business_time.go:15-50`）。
- **D18 业务日历**：`time.business_timezone` / `time.business_day_start`，`BusinessDay(t)` 按业务时间算“哪一天”（`infra/base/clock/calendar.go`、`framework/app/business_clock.go:28-38`）。
- **ctx 里已经有冻结的业务时间，但没人用**：每条 Nest 消息建一个 `fctx.Context`（`framework/nest/nest_dispatch.go:296-303`），`init` 时固定 `Now = clock.Now()`（`infra/base/fctx/context.go:118-123`），`fctx.Now()` / `fctx.NowMilli()` 读它，不在 ctx 里时现读（`context.go:132-144`）；快照交接不传 Now，每次执行重新固定（`context.go:27-29`）。但 framework / gameplay / service / demo / codegen 里**没有业务代码调用 `fctx.Now()`**：
  - 服务与 demo 读 `app.BusinessClock(r).Now()`（现读，例如 `demo/internal/service/game/activity.go.tmpl:151`、`spawner.go.tmpl:64-78`）；
  - endpoint 的 handler 参数 `nowUnix` / `nowUnixMilli` 由 endpoint 骨架在**收到请求时**用 `app.BusinessClock(controller.Registry()).Now()` 填（`codegen/internal/roost/add_workflow.go:593-600`），不是 handler 执行时；World 定时器 Tick 用同一参数（`demo/game/handler/tick_world_timers.go.tmpl:26-27`）。
- **心跳**：驱动按系统单调时间调度（`gameplay/heartbeat/driver.go:239-260`），`Beat.Now` / `Elapsed` 在 handler 里**现读**驱动的业务时钟（`host.go:337-361`；进程装配 `SetClock(app.BusinessClock(r).Now)`，`wiring/heartbeat/heartbeat.go:56`），`Elapsed` 夹到 `max(1s, 档位)`（`host.go:29-39`、`346-347`）。
- **entitytimer**：期限是业务时间、毫秒精度、存 DAO（`framework/nest/entitytimer/driver.go:1-17`、`128-135`）；驱动睡眠时长 = 期限 − 业务现在，按**系统时间**睡，上限 `MaxWait = 1s`（`driver.go:40-42`、`203-215`）；离线到期的节点加载后补触发一次（`driver.go:11-16`）。
- **actionflow / ai / Scheduler**：没传时间时缺省 `clock.Now()`（进程业务时钟现读，`gameplay/actionflow/action_runner.go:718-721`、`mission_runner.go:449`、`gameplay/ai/controller.go:249`、`infra/base/timer/scheduler.go:170-185`）；P2 的 movement 用 `Beat.Elapsed`。
- **系统时间**：租约、超时、重试、WAL、日志、指标、Lockstep 的帧推进与发送超时（`framework/sync/lockstep/match.go:819`、`snapshot.go:406`）直接用 `time` 包；glsvet 在业务包里对 `time.Now` 打提示（`cmd/glsvet/clockhints.go:19-27`）。
- **测试的可控时钟**：`clock.Set` / `SetOffset` / `Reset`（只给测试，`clock.go:114-132`）、`clock.BusinessFunc`、心跳 `Driver.SetClock` 与不启动循环直接 `step`、entitytimer `SetClock` + `DeliverDue`。

**差距**：①“一律从 ctx 取”没有落地——ctx 里有冻结时间，但业务现读时钟，同一事务里可能读到不同时刻（心跳的 `Beat.Now` 与 handler 里读的时钟就是两次读取）；endpoint 的业务时间是收到请求的时刻，不是执行时刻（排队延迟算进了“之前”）。②没有倍率，偏移也不能运行期改。③没有“场景 / 房间”级的时间域。

### 3. 方案

**3.1 业务时间放进执行上下文（一律从 ctx 取）**

- 规则：Nest handler、心跳回调、entitytimer 回调、actionflow / ai / skill 的回调里，业务时间只从执行上下文取；每条消息 / 每一拍开始时冻结一次，同一事务里读到同一个值。
- API：沿用已有的 `fctx.Now()` / `fctx.NowMilli()`（不新增入口）。增补：
  - `fctx.Now()` 在 Nest 执行上下文之外（服务 goroutine、启动阶段）读进程业务时钟现值，行为不变；
  - 心跳 `Beat.Now` 改为本条心跳消息的 `fctx.Now()`，`Elapsed` 用它算（不再现读第二次）；entitytimer `Fire` 的触发时刻同理；
  - actionflow / ai / Scheduler 的缺省时间从 `clock.Now()` 改为 `fctx.Now()`；
  - endpoint 的 `nowUnix` / `nowUnixMilli` 参数：改为在 handler **执行时**由生成的 Nest 分发包装从 `fctx` 填（参数仍在 handler 签名里，测试照样可以直接传值），请求里不带它（拍板 2）。
- 与 `app.BusinessClock(r).Now()` 的关系：两者同源（进程业务时钟与 Registry 业务时钟都读同一份 `time` 配置）。`BusinessClock` 留给 Nest 之外的服务 goroutine（活动协调器、匹配、spawner 的决策循环）；在 Nest handler 包里调用它（或 `clock.Now()`）由 glsvet 提示改为 `fctx.Now()`，与现有的 `time.Now` 提示同一机制。
- 迁移：框架内的调用点（心跳、entitytimer、actionflow、ai、Scheduler 缺省）一次改完；生成模板与 demo 的 Nest handler 改读 `fctx`；服务层不动。项目未上线，不保留旧行为开关。

**3.2 变速：业务时钟加倍率（部署级，测试 / 预演环境）**

- 时钟模型改为分段线性：`B(r) = anchorB + (r − anchorR) × rate`，`r` 是真实时间（墙钟），`anchorR` / `anchorB` / `rate` 合称“时间计划”。现在的偏移就是 `rate = 1` 的特例（`anchorB − anchorR = offset`）。
- **单调、连续**：改倍率时以生效时刻 `T` 为新锚点（`anchorB' = B(T)`），之前之后在 `T` 处相等，不跳变；`rate` 取 `[0.1, 100]`（拍板 5，部署级不允许 0），因此单调递增。往前拨（跳到某天）仍只许前进，受现有高水位约束。
- **运行期调整**：admin 命令 `time.rate set <rate> [--at <业务时间或 +延迟>]`、`time.advance <duration>`（一次性前拨）、`time.show`。计划存在协调存储里（与业务时间高水位同一个键空间），带版本号；各进程每秒（或订阅）读取，新计划带一个**未来的生效真实时刻**（缺省“现在 + 2 秒”），保证所有进程在同一真实时刻切换（拍板 4）。不同主机的墙钟偏差 `δ` 换算成业务时间偏差 `δ × rate`。
- **重启连续**：计划持久，重启后按计划算出的业务时间接着走，不会因为“偏移是配置里的固定值”而回退被高水位拒绝；`time.logic_offset` 退化为“部署首次启动时的初始计划”，之后以协调存储里的计划为准（拍板 6）。进程停着的期间业务时间照计划前进（预演“离线一周”）。
- **安全边界**：只在非生产环境、且配置 `time.speed_control = true` 时允许计划里 `rate ≠ 1` 或运行期前拨；生产环境启动检查拒绝（与 `logic_offset` 生产强制为 0 同一处）；每次修改写审计日志、导出 gauge `app.business_time.rate`；`time.show` 同时打印业务时间、真实时间、倍率与计划版本。

**3.3 变速对各部分的影响与处理**

| 部分 | 处理 |
| --- | --- |
| 心跳 | 驱动仍按真实时间调度，档位周期不缩放（CPU 不随倍率涨）；`Beat.Now` / `Elapsed` 是业务时间，倍率自动体现在 `Elapsed` 上（加速时每拍走得更远）。`Elapsed` 上限从 `max(1s, 档位)` 改为 `max(1s, 档位) × rate`，否则 10 倍速时 100ms 档每拍就被夹住。代价：高倍率下每拍步长变大，移动 / 碰撞的精度下降，预演场景可接受（拍板 3） |
| entitytimer | 期限仍是业务时间、存 DAO。驱动的睡眠时长改为 `(期限 − 业务现在) / rate`（真实时间），计划切换时唤醒驱动重算；`MaxWait` 不变 |
| 离线补触发 | 不变：加载后补一次，`Fire` 带原定期限与触发时刻（都是业务时间），错过多少周期由业务按业务时间算；进程停着的期间业务时间照计划前进，补算量按倍率放大，这正是预演要的 |
| 持久化的到期时间 | 一律存业务时间（绝对时刻），与现在一致；租约、存储 TTL、锁等系统时间不受影响（[guide/10-time §1](../framework/guide/10-time.md#1-双时钟)）。不存“剩余时长”，倍率变化不需要改写存量数据 |
| D18 业务日界 | 不变：`BusinessDay(业务时间)`，倍率 > 1 时“一天”在真实时间里变短，跨日重置随之提前到来；日历配置不变 |
| 多进程 / 多服 | 同一部署（同一个协调存储、同一 `singleton.key_prefix`）读同一份计划，同一真实时刻切换；跨部署（合服前的两个服）各自一份。活动协调器、匹配等跨进程比较业务时间的服务读的是同一份计划，与现在“所有进程读同一个 `logic_offset`”同一约束（`framework/app/business_clock.go:11-13`） |
| Lockstep / Sync | 不受影响：Lockstep 按帧号推进、帧间隔与输入超时是系统时间；实体同步的帧与刷新是系统时间。下发给客户端的业务时间戳（冷却结束、活动截止）仍一致；客户端要做本地插值时需要知道倍率，经 3.4 的时间域下发 |
| 系统时间 | 不变：租约、超时、重试退避、WAL、日志、指标、Nest Await 截止、心跳调度、entitytimer 重投间隔都用 `time` 包 |

**3.4 时间域（场景 / 房间级变速，ssr 第二层，生产玩法）**

- ssr 真正在生产里用的变速是**场景时间**：副本 2 倍速、慢放、时停。roost 对应一个可选的**时间域**：场景 / 房间实体拥有一个 `timedomain.Domain`（建议放 `gameplay/timedomain`），域时间 `D = anchorD + (B − anchorB) × rate`，`B` 是本条消息的 `fctx.Now()`；倍率可为 0（时停）；倍率由若干命名通道相乘、每个通道可带自动恢复期限（照 ssr 的语义，但期限用域外的业务时间）。
- **读法**：`timedomain.Now(d)` 在一条消息里第一次读取时算出并存进 fctx，之后同一消息读到同一值（照 ssr 的 `SceneNowNano`）。
- **写法与事务**：倍率只在域所属实体（场景）的 Nest handler 里改；参数存在场景实体的 DAO（随事务回滚），提交后经 `nest.AfterCommit` 发布到域的原子快照，成员实体（在各自的锁里）读快照，不读场景实体的 DAO（不跨实体加锁）。不持久（副本随进程结束），需要时可选持久。
- **接法**：心跳登记可选 `heartbeat.WithTimeDomain(d)`：调度仍是真实时间，`Beat.Now` / `Elapsed` 换成域时间（时停时 `Elapsed = 0`）；actionflow 的 `ActionContext.Now` / `Elapsed` 随之；技能 Runtime 的 tick 由业务从域时间换算；Buff 时长按技能 tick 计，随之变速。entitytimer 不接时间域（持久、离线语义只对业务时间成立，拍板 7）。倍率变化写进场景的同步组件下发给客户端（ssr 同样下发）。
- **安全边界**：时间域是玩法能力，生产可用；只在声明了时间域的实体上有效，倍率上下限由域的配置声明（ssr 只允许 SE 副本）。

### 4. 分期与工作量

| 期 | 内容 | 估计 |
| --- | --- | --- |
| D22-1 | 一律从 ctx 取：心跳 / entitytimer / actionflow / ai / Scheduler 的缺省改读 `fctx`；endpoint 业务时间参数改为执行时填；glsvet 提示 Nest handler 包里的 `BusinessClock` / `clock.Now`；demo 与生成模板跟进。行为上只是“同一消息内取同一时刻” | 1.5 天 |
| D22-2 | 部署级变速：时间计划（锚点 + 倍率）、协调存储持久与版本、定时切换、`time.speed_control` 与生产拒绝、admin 命令、心跳 `Elapsed` 上限按倍率、entitytimer 睡眠按倍率、指标与审计 | 2.5 天 |
| D22-3 | 时间域：`gameplay/timedomain`、fctx 快照、`heartbeat.WithTimeDomain`、actionflow 接线、同步下发倍率 | 2.5 天 |

每期先红后绿（可控时钟；倍率切换前后连续性、单调性、多进程同一时刻切换用假协调存储与两个时钟），D22-2 跑全量与根包门禁。D22-3 依赖 D20 P2（BeatRunner）合入。

### 5. 需要拍板

1. **业务时间的唯一入口**：A 沿用 `fctx.Now()`（每条消息 / 每一拍冻结一次，已有机制，推荐）；B 给 Nest handler 加显式 ctx 参数（生成器与全部 handler 签名都要改）。
2. **endpoint 的 `nowUnix` / `nowUnixMilli`**：A 参数保留，改为执行时由生成的分发包装从 `fctx` 填（推荐：执行时刻才是事务的时刻，handler 仍可单测）；B 维持收到请求时填；C 删掉参数，handler 内读 `fctx.NowMilli()`。
3. **心跳在变速下**：A 调度按真实时间、`Elapsed` 用业务时间、上限乘倍率（推荐：CPU 不随倍率涨，与 ssr 一致）；B 档位周期按倍率缩放（精度不变，CPU 随倍率线性涨）。
4. **多进程一致**：A 计划存协调存储、带未来生效时刻同时切换（推荐）；B 每个进程各自配置倍率（同一部署内业务时间会分叉，活动 / 匹配会错）。
5. **倍率范围**：A 部署级 `[0.1, 100]`、不允许 0，时间域允许 0（时停）（推荐）；B 部署级也允许暂停（全服 entitytimer / 活动停摆，测试价值低）。
6. **`time.logic_offset` 的地位**：A 退化为首次启动的初始计划，之后以协调存储里的计划为准（推荐：变速后重启必须连续，否则高水位拒绝启动）；B 保留为启动时强制的偏移，变速只在进程生命期内有效（重启即回退，高水位会拒绝）。
7. **entitytimer 与时间域**：A entitytimer 只用部署业务时间，域内定时走心跳（推荐）；B entitytimer 也支持域时间（离线补触发对“随进程结束的副本”没有意义）。
8. **生产环境**：A 部署级变速与运行期前拨只在非生产且 `time.speed_control = true` 时可用，时间域生产可用（推荐）；B 部署级也允许生产（风险：活动、邮件、赛季全部提前）。
9. **分期顺序**：D22-1 → D22-2 → D22-3（推荐：D22-1 没有行为风险，是后两期的前提）；或先做 D22-3（玩法价值高，但依赖 D22-1 的 fctx 快照）。
