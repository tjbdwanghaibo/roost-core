# RemotePolicy Mirror 实施交接

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
