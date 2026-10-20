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
| D14 | AOI 观察者改为 playerId、视野半径按观察者配置 | 维护者 2026-10-20 决定 A1 + B1（六项均按推荐），待实施（排在 2026-10-20 夜间性能补测之后） |

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

**状态：维护者 2026-10-20 决定 A1 + B1，待实施（排在当晚性能补测之后）。** 登记：2026-10-20；源码基线 `4eccd3b1`。来源：维护者 review `AOI.evaluatePair` 的 `observer.id == subject` 时指出，observer 名义上是实体 ID、实际上只有玩家；视野半径也不该只有全局一个。本条只登记方案，不改代码。

### 维护者决定（2026-10-20，六项均按推荐）

1. observer 编号：**A1**，observer = playerId，观察者带 `self`（玩家自身实体 ID，0 表示没有）。A2（视野 ID）不做，将来需要多视野时在 A1 上加。
2. 入口形状：`Enter` / `Move` / `Leave` 直接改成两个 ID 参数，不保留旧入口（未上线，不做兼容）。
3. 事务内入口换算：`InterestConfig` 新增 `ObserverOf func(entityID int64) (playerID int64, ok bool)`，缺省用 `GetUniqueIDFromEntityID`，业务可覆盖（玩家控制英雄 / 载具时视点实体不是 Player）。
4. `InterestConfig.Session`：删除（A1 下 observer 就是会话 ID，多对一隐患随之消失）。
5. 半径：**B1**，每观察者 `EnterRadius` / `LeaveRadius`，`AOIConfig` 同名字段作缺省、新增 `MaxLeaveRadius` 作启动期预算上限；`Bands`、`MaxVisible` 暂保持全局。
6. 运行期修改视野参数：提供排队版（事务内走提交门，视距变化多来自 Buff），同时保留政策阶段外的直接调用。

**实施时一并补的两点（评审补充）**：①新增“只看、不被看”的观察者入口（观战 / GM 视角，`self = 0`），A1 下与 `Enter`、`Show` 并列；②属于 sync 三大块的公开契约调整，按维护手册对 `scripts/perf/sync-aoi` 与 `policy` 包 AOI 基准做前后 benchstat。实施顺序按 roost-coding 纯重构流程：先补现有行为的回归，再改签名，最后补新行为的红绿用例。

### 现状（已核对当前源码）

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
