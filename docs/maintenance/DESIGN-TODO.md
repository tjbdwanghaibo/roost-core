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
| D7 | 实体不存在时给客户端的稳定错误码与首登步骤 | 待决定（2026-10-16 新用户试用登记） |
| D8 | 生成的服务包名大写（`package Game`） | 待决定（2026-10-16 新用户试用登记） |
| D9 | DAO 库名不随配置、`_id` 是编码后的 EntityID | 待决定（2026-10-16 新用户试用登记） |

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
- 生成器：`roost.yaml` 的 `access.player.self_sync: true`（默认关，需要 tcp 传输与 entity feature）；开启时 `internal/bootstrap/generated.go` 改用 `NewModWithEntitySync` 并生成 `PlayerSelfSync`，`server_gen.go` 生成 `WithSelfSync`，缺失时创建一次业务文件 `protocol/def/self_sync.go`（推送编号）；Player 未声明 `sync=true` / `subjectPacker` 时 `project sync` / `generate` / `doctor` 逐条给出改法。关闭时生成物不变。
- 客户端：Go 解码示例 `wiring/selfsync/example_client_test.go`（随测试运行）；C# 本轮不做（`SyncReceiver` 能收帧，subject 载荷解码需业务自写）。
- 回归：`policy/self_view_test.go`（断线重连、顶号、登录与断线并发、关闭与 Flush 并发、丢会话重开、卸载撤回重提交、新建回滚重绑）、`wiring/selfsync/selfsync_test.go`（真实 gateway TCP）、`codegen/internal/roost/self_sync_test.go`（按新手流程生成工程、打开开关、经生成接线跑真实 TCP）；另在共享隔离环境（真实 Mongo / NATS / Redis）跑了一次生成工程真实进程：登录 → EnterGame → 全量 gold=0 → AddGold → 增量 5、12 → 断线重连 → 全量 12 → 增量 13；进程重启后冷加载 → 全量 13。

用法见 [PLAYER-ACCESS §7](../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)，实现与不变量见 [impl/04-sync §2C](../framework/impl/04-sync.md#2c-自身视图selfview的生命周期)。剩余限制在 KNOWN-LIMITS“自身视图（self_sync）”一行。

## D7 实体不存在时给客户端的稳定错误码与首登步骤

**状态：待决定。** 登记：2026-10-16 新用户试用实测。

**现状**：一个从未登录（Entity 不存在）的玩家直接发业务请求时，Nest 冷加载返回 `dataengine repository: entity aggregate not found`，`errcode.ClientError` 把它归为 `CodeInternal`（客户端收到 `code=1 server error`），服务端记 ERROR。beginner 的 first-business 流程（`roost project next` / `doctor --workflow first-business`）全绿后不包含“登录时 `lifecycle.GetOrCreate`”这一步，新手第一次真实请求就会碰到。

**可选方向**：A 框架定义一个稳定错误码（例如 `errcode` 里的 `entity_not_found`），Nest 冷加载不存在时包装它，客户端可提示重新登录；代价是 nest / dataengine 错误与 errcode 产生依赖，要定码段归属。B 不加框架码，在 first-business 流程里加一步 `roost add endpoint EnterGame`（调用 lifecycle.GetOrCreate）并让 doctor 检查；代价是只解决新工程，冷加载不存在仍是 Internal。C 两者都做。

**建议**：C。需要拍板：码值与名字放在框架还是生成工程；“不存在”是否要区分已删档。

## D8 生成的服务包名大写（`package Game`）

**状态：待决定。** 登记：2026-10-16。

**现状**：`internal/service/<name>/` 下生成的文件用 `safeIdent(name)` 作包名（`render.go` 的 `renderService` / `renderServiceManagers`、`framework_services.go`），game-demo 模板同样是 `package Account`、`{{GAME_SERVICE_PKG}}`。合法但不合 Go 包名惯例（小写），新手读到 `package Game` 会以为写错。

**问题**：不能只改生成器：`service.go`、`managers.go` 归业务、生成一次后不再改写，已有工程里它们仍是大写包名，而生成器持有的 `framework_clients_gen.go` 等重写成小写后，同一目录出现两个包名，`roost project sync` 之后编译失败。

**可选方向**：A 改成小写，并让 `roost project sync` 对这几个已知业务文件只改 package 子句（或 doctor 给出一条命令）；B 维持大写，在 CODEGEN_REFERENCE 写明是约定；C 新工程小写、已有工程保持（按目录现有包名生成）。

**建议**：C（不改写业务文件，也不制造双包名）。需要拍板：是否值得为风格做这次迁移。

## D9 DAO 库名不随配置、`_id` 是编码后的 EntityID

**状态：待决定。** 登记：2026-10-16 新用户试用实测（与 2026-10-05 演练预演的发现相同，第二次出现）。

**现状**：`roost add dao` 骨架写死 `//roost:dao coll=player db=game`，库名是编译期常量 `PlayerDaoDBName`；服务配置的 `dataengine.database` 只决定 Data Engine 自己的集合（receipts / transactions / outbox）。同一 Mongo 上的两个工程即使改了 `dataengine.database`，业务集合仍都写进 `game.player`。（引擎侧 `DocumentKey.Database` 为空时已回退到 `DefaultDatabase`，见 `framework/dataengine/engine/mongo_store.go` 的 `collection`；但 DAO 解析要求写 `db=`（`codegen/internal/dao/parse.go`），生成的 DAO 走不到这条回退。2026-10-16 审查补注。）另外文档 `_id` 是编码了 kind 的 EntityID（玩家 9001 存成 9217029），运维按玩家 ID 查不到。

**可选方向**：A DAO 库名默认取运行配置（`db=` 省略时用 `dataengine.database` 或新键 `dataengine.business_database`），显式 `db=` 仍是常量；代价是生成 DAO 需要运行期注入库名，迁移已有数据。B 骨架按工程名生成 `db=<project>`，并在文档写明 `_id` 编码与换算命令（`roost id` 加一个换算子命令）；代价小，但多工程仍靠约定隔离。

**建议**：先做 B（低成本、立即消除误用），A 另行评估。需要拍板：库名是否应当是部署配置而不是代码常量。

