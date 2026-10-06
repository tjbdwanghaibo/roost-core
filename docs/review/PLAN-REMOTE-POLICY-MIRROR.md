# RemotePolicy Mirror 实施交接

09-14 未收敛状态：真实 Redis 已确认另一 cache 实例仍能重写已删旧版本；本机墓碑有效不能替代全局水位。WAL Close 中 Sync 新缺陷列 RR-20260914-01，作为可靠交付依赖继续处理。模式切换六个真实 Redis 故障场景通过。[工作表](OPEN-QUESTIONS.md) · [证据](REVIEW-2026-09-14.md)。

收尾独立验收：合并作者修复 `885ff4f585508b7f157b89419f83d05bf0a7b5d8`（U-0189，Enter/Leave 共用未知结果恢复），解决文档索引冲突时保留两个 RR 与作者说明。原触发按新契约适配：权威确认切换成功允许返回 nil，发送前失败仍必须报错；核心 live 模式和写准入断言保留。四个模式场景及 Leave 连续三次写准入均 PASS（overlay 1.656s）。RR-12/13 现均已独立验收，旧失败证据保留；未验证真实 Redis/跨进程。最终统计：24 轮、16 篇机制文档、36 RR，索引 36 已修复、0 未修复；不是全仓审完。

第十轮补充：[RR-13 LeaveShared 重试无法收敛](../bug/REVIEW-2026-09-13-10.md) 与 RR-12 并列为动态 owner 模式依赖。读取设计本身不替代 owner 的合法恢复路径；进度统计不将 Mirror 方案算作已实现。

第九轮新增依赖风险：[RR-20260913-12](../bug/REVIEW-2026-09-13-09.md)，owner EnterShared 已执行但回复丢失后仍可独占写准入。使用动态共享模式的 owner 上线前需验收此路径；不推翻此前 Transfer 修复。

第八轮验收更新：第七轮 L2 残余、同进程代际、Transfer、WAL、回调与 Mail 原故障已通过，见[运行记录](REVIEW-2026-09-13-08.md)。跨节点删除水位、TTL 与重放窗口、跨进程时钟回拨仍需协议证明；Mirror 方案尚未实施。

第七轮最新验收：RR-08、RR-06 通过；RR-01/05 有 L2 残余；兴趣跨重启代际待复核。第 2–4 步须增加在途回填越过墓碑、冷 L1 删除新 L2、预检查后 CAS 冲突验收。[证据](../bug/REVIEW-2026-09-13-07.md)。

收尾同步：首次推送因远端前进被拒，已正常 merge `06fdd2dc319856c6645ddfb79eba6c4171f681c0`。该提交新增 U-0180/0181/0183/0184 和 U-0175 补充修复，涉及 RR-20260913-02/05/06/08、RR-20260911-06。以上测试仍对应原审查 SHA；新修复待下一轮独立验收，旧状态描述是历史快照。保留远端源码和 bugfix 说明，未将其当作本轮已验证。

后续验收细化：[生命周期实测](IMPLEMENTATION-MIRROR-LIFECYCLE.md)，第 3–4 步需覆盖在途工作和旧回调隔离。

2026-09-13，基线 Core `d7832e8249a4b8dca123fa7e28268fa05e78118a`，Kit `3855c71f5aaaf5ca4b7091ae17bf8cb0e6943b79`，Codegen `cacd627b70991c5d0e38545866610db78e695b53`。**本文是待实施方案；没有新增运行时 API，也没有修复代码。** 现有能力与源码依据见[机制说明](IMPLEMENTATION-REMOTE-POLICY-MIRROR.md)，本轮范围见[运行记录](REVIEW-2026-09-13-05.md)。

## 推荐第一版

以“一个 owner 发布公会摘要，另一个服务读取摘要”为纵向样例。使用完整快照、L1、独立只读 DTO reader，复用 `RemoteSnapshotCache` 和 `mirror.Replicator`。第一版不要求 L2、delta 或 entitysync 协议桥接；持续订阅仍必须具备续租、代际、重同步与删除屏障。默认缓存读必须配置最大陈旧时间；需要权威裁决的业务向 owner 发命令。

只读服务不装配写 Manager 的事务 backend、写锁和 finalizer。现有 Manager 改为组合共享快照组件，保留外观兼容，避免复制读路径。旧 `remote=mirror` 生成路径不能继续被当成已完成的只读能力：新生成结果仅提供 DTO/reader；旧可写 Entity 用法明确报迁移错误。同一进程同 kind 只注册一次身份，用独立 reader 表达消费能力。

## 先处理的已知约束

以下状态来自[第四轮独立验收](REVIEW-2026-09-13-04.md)，本轮未重跑历史复现。不要仅按 bugfix 中“已修”关闭 RR-08。

| 问题 | 第一版要求 | 可延期边界 |
| --- | --- | --- |
| RR-20260913-01 删除无屏障 | 在统一 apply 中比较版本，保留有界水位并建立淘汰后 bootstrap 屏障 | 不可延期；L1 也会复活旧值 |
| RR-20260913-02 旧 release 撤销新兴趣 | 引入订阅 session/generation，release 只撤销对应 generation | 使用兴趣过滤时不可延期 |
| RR-20260913-08 过期部分修复 | Cached、Monotonic、Linearizable、直接权威加载和同版本刷新均统一校验 | 不可直接复用目前所有返回路径 |
| RR-20260913-05/06 L2 冲突与回填 | 启用 L2 前统一语义错误分类及原子准入 | L1-only 样例可延期；不是所有缓存风险都随之消失 |
| RR-20260913-09 owner 转移未知结果 | 转移结果不确定时停止写准入，独立有界查询权威结果 | 单 owner 样例不验迁移；生产允许迁移前必须解决 |
| RR-20260911-06、RR-20260912-01/02 | 复核完成回调和 WAL 停机/持久化承诺 | 演示可披露边界；可靠生产交付不可宣称已通过 |

RR-03 payload 身份、RR-04 等待名额、RR-07 schema/codec、RR-10 大版本、RR-11 大 epoch 已有独立通过证据，抽取时保留原回归。问题原文见[索引](../bug/README.md)。

## 组件与改动落点

下列新组件名、文件名均为建议，实施时按项目命名收敛；现有文件的职责依据机制说明中的链接。

| 层 | 现有入口 | 建议交付 |
| --- | --- | --- |
| core/entity | remote_snapshot.go 与快照类型 | 新 reader 契约；观察 token；独立 DTO/字节副本；所有加载/回填/推送共享准入规则 |
| core/remoteentity | transaction_manager.go 的 ReadRemoteSnapshot；assembly.go 的 BindSync；syncer.go | 提取 snapshot_client.go：读取、兴趣、按 key apply、回填、状态与生命周期；Manager 委托它，禁止两份协议 |
| core/mirror | Replicator 与 Envelope | 复用传输封装；由 Remote 协议层补完整身份、删除水位、session 校验，不把业务策略硬塞给通用 Store |
| kit | 现有装配约定 | 新只读装配，注入 loader/bus/时钟/限额，Start/Shutdown/health；不要求原子写 backend |
| codegen/internal/entity | parse/gen 与 registry 消费者 | Mirror DTO 和 reader 接入声明；不生成持久化/提交参与能力；真实消费者编译验收 |

首个抽取点需要特别注意：`ReadRemoteSnapshot` 的 Linearizable 分支直接调用 `LoadAuthoritative`，最后的 fallback 同样直接调用它；只在 `Get` 中添加校验无法覆盖它们。`BindSync` 返回的是未启动 replicator，调用方负责启动和回收。新只读装配须显式接管这项所有权。

## 必须先固定的协议

### 身份和观察 token

完整 key 保留 Tenant/Kind/EntityID/Scope/Policy 等现有身份维度，使用实际类型定义，不按 EntityID 单独缓存。key 的 Policy 是视图维度，不是 RemotePolicy 枚举。版本比较使用整数，不经浮点。观察 token 包含权威 epoch 与内容 version；仅当 epoch 来自可比较的权威分配时才按 epoch/version 排序。无法证明新 epoch 的权威性时，停止接收并重新加载，不以“数字更大”替代来源认证。

同一 epoch/version 的 payload、schema、codec、操作类型必须一致；冲突返回一致性错误，不能按网络降级吞掉。删除分配新版本，权威 loader 对删除也返回带版本的墓碑；无版本的 `found=false` 不能建立防复活水位。

### 订阅代际与首载

建议采用 consumer 启动 session ID + session 内递增 generation。session ID 只做相等判断，不比较随机 ID 大小；重启建立新 session，旧 session 只能靠租约清理，不能释放新 session 的兴趣。owner 按 consumer/session/key 保存 generation 与租约，并将多个 session 的有效兴趣聚合。限制 session 数量，避免无限遗留。

首载采用显式订阅确认与缓冲协议：owner 确认订阅已进入发布路径后，consumer 加载带水位的权威全快照，并缓冲确认后到达的消息。按 key 串行安装全快照，再应用高于其水位的缓冲更新。加载期间缓冲溢出则整次 bootstrap 失败并重试，不能标 Ready。只有 bus/owner 能证明确认后的发布不被静默丢弃，才提供此保证；普通 Subscribe 返回成功本身不是证明。

若实际 bus 不能提供这项确认/可靠性，第一阶段只交付明确的按需缓存读，持续推送模式保持禁用，直到补齐适配。这样不会把“可能恰好收到”写成订阅一致性契约。

release 携带原 session/generation，只撤销精确匹配的订阅。续租和 release 对同 generation 的顺序还需单调 operation sequence；更高 generation 的续租不得被旧 release 或旧 renew 覆盖。租约时间仅负责过期，不能兼任操作版本。

### 有界墓碑与恢复

第一版推荐“不用固定墓碑 TTL 猜安全窗口”：水位与当前 key 的已验证订阅代际绑定。内存压力要淘汰水位时，同时让 key 退出 Ready，关闭旧代际准入；下一次读取先完成新代际 bootstrap。消息接收入口必须拒绝未经准入的 key，旧消息不能自己创建新 Ready 条目。旧 fetch 回调同样携带本地 generation，失效后不得写入。

如果共享广播消息没有订阅代际，应在接收调度中绑定可验证的流边界，并用权威快照水位过滤历史消息；若做不到，需增加协议字段或持久化高水位。仅解绑本地回调再绑定不构成网络历史隔离。可重放窗口无上限时，TTL 墓碑方案不成立。

### 读取和关闭

拟议 Read 输入包括完整 key、读模式、最低观察 token、MaxStaleness；返回快照/token/新鲜度或明确错误。Cached 也不返回超过业务容忍期或绝对 ExpiresAt 的值。Monotonic 达不到下限时等待或加载，超时返回错误；Linearizable 仅在 loader 明确提供保证时开放。内容版本未变的权威刷新需要显式刷新有效期：要区分可信刷新元数据与内容冲突，不能命中“同版本无需更新”便返回旧过期条目。

Shutdown 停止准入，取消续租和加载，解绑订阅，等待已准入工作，最后释放依赖。等待尊重 context；截止后保留可再次等待的停止状态，不提前释放仍被调用的资源。loader 不响应取消时应返回超时并报告未退出任务，不能声称已完全关闭。

## 建议按六个提交实施

| 顺序 | 工作及依赖 | 提交验收 |
| --- | --- | --- |
| 1 | 定义只读 contract、token、错误、默认配置与迁移策略 | DTO 修改不污染缓存；无写参与能力；owner/consumer 同身份、同进程无冲突注册 |
| 2 | 统一快照准入与所有读出口；依赖 1 | 过期 Cached/Monotonic/Linearizable/直接 Load、同版本刷新；保留等待名额/身份/schema 回归 |
| 3 | 提取共享 snapshot client，Manager 委托；依赖 2 | 原 Manager 行为回归；客户端不要求写 backend；启动失败逐步回收，无重复订阅 |
| 4 | full snapshot 订阅、代际、墓碑、恢复；依赖 3 | 旧删除/旧 upsert/重建；renew-release 全交错；启动缓冲、溢出、重连、淘汰与旧 fetch 回调 |
| 5 | kit 装配、codegen 只读产物、公会摘要样例；依赖 1–4 | 生成后真实消费者编译；两进程读写分离；跨租户/profile 拒绝；停止取消与重复关闭 |
| 6 | Linux 真实 bus/存储故障与性能报告；依赖 5 | 强杀重启、重投、静默断线、owner 切换与 outbox 补发；明确尚未完成的已知问题 |

每项验收是待新增或待复用的测试要求，**不代表本轮已运行**。旧复现入口：[接收/兴趣](../bug/REPRO-2026-09-13.md)、[L1/L2](../bug/REPRO-2026-09-13-02.md)、[真实 Redis](../bug/REPRO-2026-09-13-03.md)、[过期残余](../bug/REPRO-2026-09-13-04.md)。实施时把稳定回归纳入正式测试，不依赖本机临时目录仍然存在。

## 性能与完成标准

缓存命中不获取 Nest 写锁或 Redis owner 锁；DTO 解码/复制成本单独计量。用相同 payload 大小、key 数、热点比例、订阅扇出和更新频率比较 owner 直读、Mirror L1 命中、冷加载；报告吞吐、p95/p99、allocs/op、RSS、队列深度、合并加载比例和回填放大。增加慢消费者与断线恢复负载，检查有界队列和公平性，不只跑热 key 微基准。

配置至少包括 key/字节上限、单次 payload、加载并发/等待者、缓冲深度、加载超时、最大陈旧时间、兴趣租期/续租间隔与 Shutdown 超时；非法组合启动即拒绝。具体数值按样例测量确定，本文不编造吞吐目标。

完成标准是第 1–5 项有代码与对应验收，第 6 项给出可复跑环境和限制。L2、delta、entitysync/syncstream 桥接另列扩展，不阻塞 L1 full-snapshot 样例；上线前仍须解决所用 owner/持久化路径的未关闭正确性问题。

## 2026-10-05 现有接入校验补修

[NC-33缓存](../bugfix/RR-20261005-NC-33.md)与[NC-34兴趣](../bugfix/RR-20261005-NC-34.md)已修，10正式副作用反例红转绿，13新叶子含兼容/恢复通过；[机制](IMPLEMENTATION-MIRROR-PAYLOAD-IDENTITY-AND-ROUTING.md)。本轮没有实施本交接的独立DTO reader、重连或全局水位；历史墓碑限制与外部验证继续保留。[范围与下一入口](REVIEW-2026-10-05-noncore-23.md)。

## 2026-10-05 revn05：方案与实现状态

方案本身未改。实现仍未实施（无只读 DTO reader / snapshot client、无订阅确认与首载缓冲、无 MaxStaleness）。本轮修的是现有接入：L2 CAS 落败不再把旧快照装进 L1（[NC-130](../bugfix/RR-20261005-NC-130.md)），版本化删除在共享 L2 留墓碑（[RR-20260913-01 残余](../bugfix/RR-20260913-01.md)）——后者对应本方案“有界墓碑与恢复”里的共享层水位，不等于第 4 步完成。重放 / 首载问题见[审查 O5](REVIEW-2026-10-05-n05-revn05.md)。

## 2026-10-06 B2：方案与实现状态

维护者选了 B2 方向 (a)（[实施记录](../feature/B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md)）：共享 L2 为水位权威、L1 只是有界副本，`cached_max_staleness` 落地为配置与契约（对应本方案“读取和关闭”里的 MaxStaleness），DeliverAll 重放的过老快照按发布时刻丢弃。本方案的只读 DTO reader、观察 token、订阅代际与首载缓冲仍未实施。

## 2026-10-06 第 1～3 步实施

维护者第四轮决定补齐本方案，本批做六步表的 1～3，[实施记录](../feature/MIRROR-STEPS-1-3-2026-10-06.md)。方案本身未改。

- **第 1 步（只读契约）**：`entity.RemoteSnapshotReadOnly`（只读方唯一能力）、观察 token `entity.RemoteObservation`（排序与 L1 / L2 准入同一规则，混合 epoch 返回 `ErrRemoteObservationIncomparable`）、DTO reader `entity.RemoteMirrorReader[T]`（解码独立副本、读侧校验身份与 schema / codec、不注册 kind 或全局解码器，同进程 owner / consumer 不冲突）、`ErrRemoteReadUnsupported`；迁移策略见实施记录 §3。
- **第 2 步（统一读出口）**：B2 已覆盖陈旧上限、过期与同版本刷新；本批补齐 Manager 外层回退（一次 Monotonic 未命中回源两次，先红后绿）、Cached 交出低于最低版本的值（先红后绿）、带 epoch 的 token、Linearizable 能力门。全部读出口经 `RemoteSnapshotCache.Read` 与 `Covers`。
- **第 3 步（共享 snapshot client）**：`remoteentity.SnapshotClient` 拥有读、兴趣、按 key apply、回填、状态与生命周期；Manager 组合并委托，`Assembly` 的复制启停交给它；客户端不要求写 backend；启动失败逐步回收、无重复订阅；`Stop` 套 A3 三步停机骨架。缓存写入仍只经 `admitLocked`。
- 真实 Redis + 自建 Cluster 的 `TestRealB2WatermarkMatrix*`、真实 JetStream 重放、生成工程 12 条 `TestGeneratedRemote*` 通过。
- **第 4～6 步未开始**，入口与前置条件见实施记录 §7（核心前置：总线能否证明确认后的发布不被静默丢弃；不满足时第 4 步只交付按需读）。

## 2026-10-06 第 4 步实施

维护者第九轮决定按推荐做第 4 步（推送订阅依赖 JetStream，按主题 DeliverNew；没开 JetStream 显式退化为按需读取），[实施记录](../feature/MIRROR-STEP-4-AND-O4-2026-10-06.md)。方案本身未改；B2 之后与原文的差别见实施记录 §2（L2 水位与陈旧上限已挡住旧删除 / 旧 upsert / 淘汰后的旧消息，不再需要“Ready 门”）。

- 可确认订阅：`fsyncbus.ILiveSubscriber` / JetStream `SubscribeLive`（DeliverNew durable，与 DeliverAll 分开）/ `mirror.NewLive`；`SnapshotClient.Start` 只在它上面开推送，普通 NATS 退化并记 Warn。
- 首载缓冲：快照缓存 `ApplyReplica`，权威加载在途时缓冲、装入后经 `admitLocked` 重放、溢出丢弃并再回源一次。
- 兴趣代际：锁内分配；release 撤销水位，renew / release 全交错收敛到最后一次操作。
- O4 兴趣容量按 consumer 计。
- 真实 JetStream + Redis：确认订阅、不重放历史、退订后重订续投；推送到达只读方；B2 组合矩阵与生成工程 12 条照样通过。
- **第 5、6 步未开始**，入口见实施记录 §6.8。

## 2026-10-06 第 5 步实施

[实施记录](../feature/MIRROR-STEP-5-2026-10-06.md)。方案本身未改；样例放在生成工程 `testdata/remoteflow`（比 game-demo 新增一个服务更简单，验收用真实依赖跑两进程），game-demo 只更新了 `guild_info` 的说明。

- **kit 装配**：`kit/remoteentity.RemoteMirrorMod`（`NewSnapshotClient` + `Start(bus)` / `Stop(ctx)`；只读快照段，新键 `remote_entity.mirror.shutdown_timeout`；不要求原子 backend；健康 `snapshot_push` / `interest_refused`；StopBudget），只读 Mongo loader `remoteentity.NewMongoSnapshotLoader`，`MirrorSource(registry)`；owner 进程的 `RemoteEntityMod` 登记同一能力。
- **codegen 只读产物**：`//roost:mirror` DTO → spec / 解码 / reader；`remote=mirror` 迁移诊断。
- **公会摘要样例**：owner（Managed Guild，Nest → WAL → Remote 提交 → 发布）+ 只读子进程（kit SyncBusMod + RemoteMirrorMod + 生成 reader）；真实 JetStream + Redis + Mongo 上推送读到 v2，普通 NATS 上 3s 陈旧上限之后按需读到；跨租户 / profile 读不到、错配源被拒；只读进程没有写能力；停止后读返回 `ErrSnapshotClientStopped`、重复关闭 nil。
- **版本下限**：生成的只读产物用到 v1.20.2 没有的 `entity.RemoteMirrorReader` / `RemoteSnapshotReadOnly` / `RemoteMirrorSpec`（第 1～3 步新增），kit 用到 `remoteentity.NewSnapshotClient`（第 3 步）与本步新增 API；发版准备时统一上调生成工程的 `minimumVersions.Core`，本步未改。
- **第 6 步未开始**：外部条件见实施记录 §6。

## 2026-10-06 第 6 步（本机替代）

维护者指示“看能否在本机用别的方式替代”，[实施记录](../feature/MIRROR-STEP-6-LOCAL-2026-10-06.md)。方案本身未改；Linux 真实环境的那部分改为外部验证清单（记录 §5）。

- **环境**：`scripts/mirror-local.sh` 自起私有依赖进程（不碰共享隔离环境），`test` / `bench` 结束清理并核对无残留进程。
- **故障（两进程，生成工程 `testdata/remoteflow`）**：owner 强杀 + WAL 重放补发、NATS 节点强杀与 SIGSTOP 静默断线、toxiproxy 延迟 / 分区 / 丢数据（DeliverNew durable 续投）、Redis 单机与 Cluster 切主（墓碑不回退、不复活）、Mongo stepDown、owner 转移、只读服务强杀重启；只读方自己核对不回退 / 不复活 / 有界收敛，0 违例。
- **缺陷**：[RR-20261006-01](../bug/RR-20261006-01.md) 删除 Remote 实体时确认交给已清空的实例、删除不发布，已修复（先红后绿）。
- **性能**：同机交替 n=6 对照 v1.20.2，读延迟（L1 / L2 / 权威）、回源次数、推送扇出（1 / 10 / 100 读者 × 10 key）无显著差别。
- **未完成**：Linux 内核网络、跨主机真实分区、长时间容量与长稳、目标负载下的扇出、Remote outbox（Mongo 已提交、发布前崩溃）的精确注入；观察 O-M6-1 / O-M6-3 已按维护者第十轮决定实施（[记录](../feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md)）。
