# 待维护者决定的设计事项（TODO）

登记日期：2026-10-15；源码基线：`a91f5f72`（v1.27.0 之后只多文档）。本篇只列**需要维护者做产品 / 设计选择**的事项；每项给出现状、问题、可选方向与代价、建议和需要拍板的问题。工程缺口、性能与外部验收分别见 [KNOWN-LIMITS](KNOWN-LIMITS.md)、[EXTERNAL-VERIFICATION](EXTERNAL-VERIFICATION.md) 与 [performance](../performance/README.md)。决定后在对应条目写明“维护者 YYYY-MM-DD 决定 …”并转为实施任务或关闭；不把本篇条目当作已承诺的功能。

| # | 事项 | 状态 |
| --- | --- | --- |
| D1 | Lockstep 历史的保留与过期策略 | 维护者 2026-10-15 决定 C，已实施 |
| D2 | SyncBus 有限投递的兜底 | 维护者 2026-10-15 决定 A，已实施 |
| D3 | Entity Sync `Drain` 遇到永久拒绝的出口 | 维护者 2026-10-15 决定 A，已实施 |
| D4 | 业务 schema 离线迁移的模板 / 流程 | 维护者 2026-10-15 决定 A，已实施 |
| D5 | 静态 Player 跨机迁移的参考流程 | 维护者 2026-10-15 决定 A，已实施 |
| D6 | 玩家看自己状态（Entity Sync 自身视图）的接入骨架 | 维护者 2026-10-16 决定折中方案，已实施（提交见本节） |
| D7 | 实体不存在时给客户端的稳定错误码与首登步骤 | 维护者 2026-10-16 决定 C，已实施（`e7e59298`） |
| D8 | 生成的服务包名大写（`package Game`） | 维护者 2026-10-16 决定 C，已实施（`bac32cd2`） |
| D9 | DAO 库名不随配置、`_id` 是编码后的 EntityID | 维护者 2026-10-16 决定 B，已实施（`a068be37`）；方案 A 保留待评估 |
| D10 | 删档后用同一 ID 重建（投影版本冲突、进程 fail-stop） | 维护者 2026-10-17 决定 B，范围只守 GetOrCreate，已实施（`572d87c2`） |

已决定、不在本篇：Session 运行服务保持现状、不改为可选（2026-10-14）；玩家接入目录 `game/playeraccess` 改名已随 v1.27.0 发布。

## D1 Lockstep 历史的保留与过期策略

**状态：维护者 2026-10-15 决定 C（按推荐），已实施（`a6376934`）。** 方案 B（全量快照恢复）不做，等有具体游戏需求再定。

**实施结果**：
- `MatchConfig.HistoryRetainFrames` 默认 `DefaultHistoryRetainFrames` = **3600 帧**（60Hz 下 60 秒、30Hz 下 120 秒；依据：重连窗口按 60 秒、常见逻辑帧率上限 60Hz 换算，10 席 × 8 字节输入约 1.5MB / 局）。`RetainFullHistory`（-1）显式关闭，退回宿主手动 `TrimHistory` / `TrimHashReports`；正值下限 `MinHistoryRetainFrames` = `ReplayHorizon` = 64（Sequencer 视为仍在线的落后量，同时覆盖一整页追帧与最大冗余深度），不满足时 `NewMatch` 返回 `ErrMatchConfigInvalid`。
- `Tick` 在发出本次追帧页之后裁历史；hash 报告按 64 帧步长跟随同一窗口。宿主手动裁剪 API 保留。
- 追帧越界：`StartCatchup` / `SpectatorCatchup` / `HandleCommand(OpCatchup)` 立即返回 `*CatchupUnservableError`（`errors.Is` `ErrCatchupUnservable`，带 `From` / `Start`）；追帧途中被窗口越过时下一次 `Tick` 返回同一错误。两种情况都经可靠通道给该会话发 C9 v1 追帧失败通知，计 `lockstep.catchup.unservable.total`。
- 客户端表现统一为“追帧失败、交由业务决定踢出或重开”，不自动重同步：Go `FrameAssembler.Ingest` 返回 `*CatchupUnservableError`、`robot.LockstepBot` 终止；C# `LockstepClient` 抛 `LockstepCatchupUnservableException` 并终止实例。
- 一并修复：起点 0 原先绕过越界检查，裁剪后从 0 追帧会得到一段有洞的帧流且无错误。

说明与证据见 [Sync 使用指南](../framework/guide/04-sync.md#追帧越界明确失败不自动重同步)、[SYNC-OPTIMIZATION 末节](../framework/SYNC-OPTIMIZATION.md#lockstep-自动保留与追帧失败通知2026-10-15)。仍待：真实网络下断线超过窗口的端到端演练（见 [KNOWN-LIMITS](KNOWN-LIMITS.md)“Lockstep历史”）。

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
- 生成器：`roost.yaml` 的 `access.player.self_sync: true`（默认关，需要 tcp 传输与 entity feature）；开启时 `internal/bootstrap/generated.go` 改用 `NewModWithEntitySync` 并生成 `PlayerSelfSync`，`server_gen.go` 生成 `WithSelfSync`，缺失时创建一次业务文件 `protocol/def/self_sync.go`（推送编号）；Player 的标记缺省按 `sync=true`；2026-10-17 起没写 `subjectPacker=` 时 entity 生成器写默认 packer `game/entities/player/player_sync_packer_gen.go`（维护者决定，KNOWN-LIMITS 第三轮试用登记项 3），`project sync` / `generate` / `doctor` 对没有 DAO、多个 DAO、显式 `sync=false` 给出改法，对 DAO 没有 sync 字段给警告。关闭时生成物不变。
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

**状态：维护者 2026-10-16 决定 B，已实施（`a068be37`）；方案 A（库名取运行配置）不做，保留为后续评估。**

**实施结果**：`roost add dao`（含 `--entity`）骨架写 `db=<project>`：`daoDatabaseName` 取 `project.name`，系统库 `admin` / `local` / `config` 加 `_data`，超过 52 字节截断并去掉末尾 `_`（Mongo 库名 63 字节上限，留出 `dbscope=sid` 的 `_<sid>`），骨架注释写明库名与 `_id` 换算；game-demo 的 Player / Guild / World DAO 改用同一规则（工程 planet → `planet` 库），demo 文档同步。`roost id entity` 双向换算完整 EntityID 与唯一号 / kind / category / remote（`--entity` 读工程 `//roost:entity` 标记）。已有工程 DAO 定义归业务、不改写，旧库数据不迁移（未上线不做兼容，升级说明见 CHANGELOG）。

登记：2026-10-16 新用户试用实测（与 2026-10-05 演练预演的发现相同，第二次出现）。

**登记时现状**：`roost add dao` 骨架写死 `//roost:dao coll=player db=game`，库名是编译期常量 `PlayerDaoDBName`；服务配置的 `dataengine.database` 只决定 Data Engine 自己的集合（receipts / transactions / outbox）。同一 Mongo 上的两个工程即使改了 `dataengine.database`，业务集合仍都写进 `game.player`。（引擎侧 `DocumentKey.Database` 为空时已回退到 `DefaultDatabase`，见 `framework/dataengine/engine/mongo_store.go` 的 `collection`；但 DAO 解析要求写 `db=`（`codegen/internal/dao/parse.go`），生成的 DAO 走不到这条回退。2026-10-16 审查补注。）另外文档 `_id` 是编码了 kind 的 EntityID（玩家 9001 存成 9217029），运维按玩家 ID 查不到。

**可选方向**：A DAO 库名默认取运行配置（`db=` 省略时用 `dataengine.database` 或新键 `dataengine.business_database`），显式 `db=` 仍是常量；代价是生成 DAO 需要运行期注入库名，迁移已有数据。B 骨架按工程名生成 `db=<project>`，并在文档写明 `_id` 编码与换算命令（`roost id` 加一个换算子命令）；代价小，但多工程仍靠约定隔离。

**建议**：先做 B（低成本、立即消除误用），A 另行评估。需要拍板：库名是否应当是部署配置而不是代码常量。

