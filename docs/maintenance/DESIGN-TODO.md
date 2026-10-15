# 待维护者决定的设计事项（TODO）

登记日期：2026-10-15；源码基线：`a91f5f72`（v1.27.0 之后只多文档）。本篇只列**需要维护者做产品 / 设计选择**的事项；每项给出现状、问题、可选方向与代价、建议和需要拍板的问题。工程缺口、性能与外部验收分别见 [KNOWN-LIMITS](KNOWN-LIMITS.md)、[EXTERNAL-VERIFICATION](EXTERNAL-VERIFICATION.md) 与 [performance](../performance/README.md)。决定后在对应条目写明“维护者 YYYY-MM-DD 决定 …”并转为实施任务或关闭；不把本篇条目当作已承诺的功能。

| # | 事项 | 状态 |
| --- | --- | --- |
| D1 | Lockstep 历史的保留与过期策略 | 已决定 C，已实施 |
| D2 | SyncBus 有限投递的兜底 | 待决定 |
| D3 | Entity Sync `Drain` 遇到永久拒绝的出口 | 已决定 A，已实施 |
| D4 | 业务 schema 离线迁移的模板 / 流程 | 待决定 |
| D5 | 静态 Player 跨机迁移的参考流程 | 待决定 |

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

**现状**：DAO 自动迁移已撤销（DAO 只接受当前 schema，水合前校验，不一致即拒绝）；保留一个通用的显式迁移工具 [`infra/storage/migration`](../../infra/storage/migration/migration.go)（步骤登记与执行），业务需要自己写离线转换。升级要求见 [maintenance README §1–§2](README.md)。

**问题**：业务第一次改 schema 时没有现成的端到端流程（停写 → 备份 → 转换 → 校验 → 切换版本 → 回退方案），容易各写一套；生成器也不产出迁移骨架。

**可选方向**：
- A 文档 + 示例：在 docs/framework 写一篇“schema 变更操作手册”，demo 里加一个完整的迁移示例（含校验与回退）。
- B 生成器支持：`roost add migration <name>` 生成迁移骨架（步骤、幂等标记、校验函数、dry-run），接入 `migration` 工具与 doctor 检查。
- C 维持现状。

**建议**：A 先做；B 视业务使用频率再定。

**需要拍板**：是否需要生成器层面的支持（B）；迁移是否必须离线（停写）还是允许在线双读期（与“未上线不做兼容”的原则相关，上线后需要重新决定）。

## D5 静态 Player 跨机迁移的参考流程

**现状**：静态 Player 跨机迁移只以已落库数据为准，WAL 只负责保证落库，不做跨机 WAL 热迁移（[guide/03-dataengine](../framework/guide/03-dataengine.md)、[maintenance README §2](README.md)）；停写、排空、切换方案由应用负责。

**问题**：扩缩容 / 迁服时每个业务都要自己设计“停写 → 等投影落库 → 切路由 → 新节点加载”，顺序错误会导致丢写或双写；框架已有的单实例锁、投影等待、Directory / 路由能力没有被组织成一条可直接用的流程。

**可选方向**：
- A 参考流程文档 + 运维脚本示例：明确每一步用哪个现有能力（单实例锁释放、`WaitProjection`、Directory 切换、拒绝新准入）与检查点。
- B 框架级“迁移 Player”命令：在 ops 管理面提供 drain-and-move 操作，自动按步骤执行并校验。代价：要处理失败回滚与部分完成状态，属于较大的新功能。
- C 维持现状。

**建议**：A 先做，并用 demo 在本机演练一次；B 等上线后有真实迁服需求再评估。

**需要拍板**：是否需要框架级迁移命令（B）；上线前是否要求完成一次端到端迁服演练。
