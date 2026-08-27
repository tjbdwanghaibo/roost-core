# Roost 通用游戏服务器框架评估

> 评估日期：2026-08-27  
> 评估范围：`roost-core`（Go 模块仍为 `cube-core`）、`roost-kit`（Go 模块仍为 `cube-kit`）、`roost-skill`。  
> 运行目标：Linux；Windows 特有的文件句柄、临时目录和 PowerShell 限制不计入产品缺陷。  
> 对照对象：Skynet 与 Microsoft Orleans。三者的抽象层次并不相同，因此本文比较“解决同一游戏服务器问题时，框架与业务团队各自需要承担什么”，而不是只比较 API 数量。

## 1. 结论摘要

Roost 已经不是一个仅能演示 actor/entity 调度的原型。它在以下几条游戏服务器关键链路上形成了相互配合的实现：

- Entity 串行执行、多实体确定性加锁、事务回滚；
- WAL、checkpoint、主动 Flush、崩溃恢复与不确定提交后的 fail-stop；
- Remote Entity 的所有权、路由、版本与 fencing；
- 跨服务 Saga、事务 outbox、幂等收据与补偿；
- 状态同步、syncstream、LOD/AOI、UDP/KCP/QUIC、20 Hz 房间同步和 lockstep；
- 确定性 2D 技能 Runtime、战斗组件、checkpoint/replay 与客户端同步。

从架构和代码设计看，Roost 的优势并不是“比 Skynet 更轻”或“比 Orleans 更自动”，而是把游戏业务最容易反复出错的状态一致性、跨服写入和实时同步做成了一套 Go 原生、相对强约束的基础设施。这条路线合理，而且具有实际差异化。

发行整改后，已发布的 `cube-core v1.8.0`、`cube-kit v1.8.0`、`roost-skill v1.7.0` 已在一个全新项目中以 `GOWORK=off`、无 `replace` 的方式完成 download/verify/tidy/test/vet；skill v1.7.0 tag 也单独通过全量 test/vet。库发行线已经从条件性 No-Go 提升为可用。剩余发行阻断集中在 codegen：已经发布的 v1.6.0 tag 仍带旧的 `v0.0.0 + replace` 项目模板；当前主线已修复，但不可回写已发布 tag，必须发布 v1.6.1 或更高版本才能让外部用户获得修复。

综合判断：

| 维度 | 源码与设计 | 当前发行状态 | 评价 |
| --- | ---: | ---: | --- |
| 鲁棒性 | 8.2/10 | 7.8/10 | 核心一致性设计较强；缺真实基础设施故障注入和长期运行证据 |
| 工程性 | 8.2/10 | 7.4/10 | 无 replace 依赖、版本默认值和 CI 门禁已修；等待 codegen 新 patch tag 发布 |
| 实用性 | 8.1/10 | 7.8/10 | 三个运行库可直接解析；对 SLG、房间制和 2D 权威 ARPG 很有价值 |
| 综合 | **8.2/10** | **7.7/10，库可用 / codegen 待 patch** | codegen v1.6.1 发布并通过 smoke 后可升至约 8.2/10 |

这里的分数表示当前证据强度，不表示代码作者能力，也不表示相同硬件上的绝对性能。当前没有三框架同模型、同存储、同负载的可复现实测，因此本文不编造吞吐或延迟排名。

## 2. 评估基线和验证结果

### 2.1 实际版本状态

- core：稳定发行 `v1.8.0`；当前 HEAD `16f057d` 是 tag 后的开发主线，README 已统一显示稳定版本和 v1.8.0 安装命令。
- kit：稳定发行 `v1.8.0` tag 本身没有 replace；当前 HEAD 新增 robot，依赖 core HEAD 才有的 API，因此使用可由 Go proxy 解析的精确 pseudo-version，不再使用本地路径。tag CI 会拒绝 pseudo-version，下一次 kit tag 前必须先发布对应 core tag。
- skill：以正式 `roost-skill v1.7.0` tag 为发行基线；它的 module path 为 `github.com/tjbdwanghaibo/roost-skill`，无生产 replace，并已独立通过测试。本地旧 checkout 不再作为发行状态证据。
- codegen：稳定 tag 为 v1.6.0；当前主线已把默认组合改为 core v1.8.0、kit v1.8.0、skill v1.7.0、codegen v1.6.0，并删除生成项目中的全部 replace。由于 v1.6.0 tag 已经不可变，这部分需要新的 patch tag 才算对外完成。

本文把“稳定 tag”与“tag 后开发主线”分开描述，不再用本地 checkout 位置判断用户能否安装。

### 2.2 可重复验证

- core 有约 320 个 Go 文件、96 个测试文件；Linux CI 执行全量测试、`go vet`、自定义 `glsvet`、关键包 race test 和性能基准门禁。
- kit 有约 157 个 Go 文件、57 个测试文件；CI 同时验证已发布 core 与本地 core 源码，并执行关键包 race test 和 100×100 房间同步基准。
- `roost-skill origin/main` 临时副本约 257 个 Go 文件、77 个测试文件；CI 包含全量 race、两个 fuzz target 和 benchmark。
- 当前 kit 开发主线在 `GOWORK=off` 下通过 module download/verify、全量 test 和 vet；依赖的是 core HEAD 的精确 pseudo-version，没有 replace。
- 从当前 codegen 主线生成的全新项目直接 require core v1.8.0、kit v1.8.0、skill v1.7.0；`go mod tidy` 后版本仍保留，无 replace，并通过全量 test/vet。
- `roost-skill v1.7.0` tag 在无 replace、无 go.work 环境下，`combat`、`combatcomponent`、`skillcompose`、`skillsync`、`skillv2` 全部通过 test/vet。
- 本机 Windows 沙箱中的 core/syncstream 个别失败来自打开中的 WAL 文件无法在 `TempDir` 清理时删除，以及受限目录解析权限。按本次范围约定，不将其计为 Linux 生产缺陷；但本次也没有以 Windows 结果冒充 Linux 全绿证明。

本次审查没有连接真实 MongoDB replica set、Redis AOF、NATS/JetStream 或 etcd 集群，也没有执行 kill -9、网络分区、磁盘写满、时钟漂移和滚动升级。因此，对于分布式恢复能力，结论是“实现和单元规格较完整”，不是“已经用故障实验完成生产证明”。

## 3. 架构定位

Roost 的三个层次划分总体合理：

1. **core 是协议和正确性内核。** 它定义 Entity、Nest、事务提交、checkpoint、remote protocol、Saga、同步协议、复制模型、lockstep、可观测性等稳定契约；尽量不直接绑定 MongoDB、Redis、NATS。
2. **kit 是基础设施适配层。** 它把 MongoDB、Redis、NATS/JetStream、etcd、本地 WAL、UDP/KCP/QUIC 等实现装配为 core capability 和 `app.Mod`。
3. **skill 是玩法域基础设施。** 最新主分支提供严格 JSON 解析、静态编译、不可变 Program、定点数 Runtime、战斗属性/Buff/伤害、Nest 可回滚组件以及 skill sync。

这个分层比把全部能力放入一个“大框架包”更容易测试和替换；问题主要在边界管理仍不够彻底，例如 module/repository 名称混用、kit 的可选依赖依靠装配顺序、skill 发布线发生过 module major 切换。

## 4. 鲁棒性评价

### 4.1 做得好的部分

#### Entity 与 Nest 并发模型

业务 handler 进入前由底层持有 Entity mutex，业务层不需要自行管理锁；多 Entity 操作按稳定顺序加锁，避免常见的 AB/BA 死锁。框架还限制 handler 内的同步跨 Entity 调用，防止“已持有 A，再等待 B，而 B 又等待 A”的隐式环路。

这比普通的“提供一个 mutex，由业务自觉使用”更稳，也比把所有状态都塞入一个单线程全局 loop 更能横向利用多核。与 Orleans grain 的单 activation 串行调度、Skynet service mailbox 的目标相近，但 Roost 额外显式处理了多 Entity 原子操作。

#### 回滚、WAL 与不确定提交

Nest 支持 State/Undo 回滚以及 Memory/Async/Strict/Pipelined durability。Pipelined 模式把 append 留在锁内，把 fsync 和完成判定移出 Entity 锁，能够降低慢盘造成的热点 Entity 锁占用；同时使用 durable watermark 控制外化。若 fsync 结果不确定，代码不盲目回滚，而是 fencing/fail-stop，避免“磁盘其实已提交、内存却回滚并继续接单”的双历史问题。

这种处理是成熟事务系统才会主动考虑的边界。它比“写失败就恢复旧对象”更正确，但代价是部署必须真正响应进程被 fence 的告警并完成恢复；如果运维层把 fail-stop 当成普通错误忽略，正确性设计仍会失效。

#### Save/Load 与 checkpoint

checkpoint 路径提供有界 journal、Flush/FlushAll、版本和 tombstone；Redis WAL 使用同连接 Lua + `WAITAOF`，避免在不同 Redis 连接上等待到与本次写无关的持久化位置。远程所有权 fence 会透传到保存路径，旧 owner 的延迟写不会仅凭“请求更晚到达”覆盖新 owner。

这里的思路正确：缓存只解决读性能，version/fence 才解决旧写拒绝。L1/L2 snapshot 同样有价值，但 L2 Redis 不能被误认为权威数据库。

#### Remote Entity 与 Saga

Remote Entity 同时校验 state version、marker epoch、lock fence、route epoch，并用 fenced lock/ownership CAS 防止租约过期后的旧持有者继续写。写模式的分布式锁并不能单独解决该问题，因为暂停进程可能在锁过期后恢复；必须由存储端拒绝旧 fence，当前设计认识到了这一点。

Saga 包含 definition version、lease fencing、事务 outbox、幂等 command receipt、tombstone 和补偿。它适合跨服但不要求全局 ACID 的业务，例如跨服转移、公会操作、奖励发放流程。它不能把所有步骤神奇变成 exactly-once；正确语义仍是“至少一次投递 + 幂等效果 + 可补偿状态机”。Roost 的实现方向与这一现实相符。

#### 实时同步与技能确定性

Replication/syncstream 有 snapshot、delta、ACK、重发/重同步、LOD、兴趣集、带宽与队列上限；房间层针对 20 Hz、每房不超过 100 个对象/订阅者给出专门基准。UDP、KCP、QUIC 和 lockstep 覆盖状态帧与输入帧两种玩法，而不是强迫所有游戏使用一种同步模型。

最新 skill 主分支采用 `int64` 定点数、HMAC 派生随机、严格 parser/compiler、不可变 Program、checkpoint/replay 和有界 Runtime 配置。它能覆盖 Dota 类技能中的阶段、消耗、冷却、状态、Buff、伤害和表现事件，同时保持服务端权威与可复算性。这是 Roost 相对通用 actor 框架最明显的游戏域优势。

### 4.2 鲁棒性不足和风险

#### 已解决：本地 replace 泄漏

kit 根模块的相对路径 replace 已删除；core、kit、codegen 的 release-hygiene 都会拒绝根 `go.mod` 出现 replace。codegen 生成项目也直接 require 真实 module path，并由回归测试和 generated-project release smoke 约束。当前剩余工作不是继续修改旧 tag，而是发布包含新模板的 codegen patch 版本。

#### 分布式正确性缺少真实故障矩阵

大量单元测试覆盖了 fence、重投、indeterminate、恢复和容量边界，这是强项；但 Redis、MongoDB、NATS 和 etcd 的真实故障语义不会被 fake 完整模拟。例如：主从切换期间的 WAITAOF 结果、Mongo transaction unknown commit result、JetStream redelivery、etcd compaction 后 watch 恢复、磁盘写满时 WAL 的部分写，都需要容器化故障实验。

#### goroutine identity 与可重入锁

Entity 默认使用可重入 mutex，并通过 `modern-go/gls`/goroutine identity 支持上下文识别；core 还用自定义 `glsvet` 限制误用。这缓解了工程风险，但没有消除对非标准 goroutine identity 技巧的依赖。Go runtime 变化、间接异步调用和未被 vet 覆盖的边界仍需要长期维护。更长期的方向是让 Nest transaction token 显式沿调用链传播，把“是否当前持锁”从 goroutine 身份推断迁移到显式执行上下文。

#### 可靠消息的语义需要更醒目

通用 bus 的消费去重如果在 handler 前用 SetNX 占位，进程在占位后、业务完成前崩溃，可能得到 at-most-once 的业务效果；只有把业务写与 receipt/outbox 放入同一事务，或提供可恢复状态机，才能获得更强保证。Saga/Remote Entity 已做得更完整，但通用 bus API 和文档必须阻止业务误把“不会重复”理解为“绝不丢效果”。

#### Skill Runtime 锁粒度

Skill Runtime 用单 mutex 保护内部时间线，并可能在该边界内调用 Host。对“每个房间或战斗实例一个 Runtime”的部署方式，这有利于确定性和简化竞态；如果把大量房间共用一个 Runtime，它会成为全局串行瓶颈。因此必须将 Runtime 分片模型写入生产文档，并增加整房间 100 单位、20 Hz、连续技能/状态组合的端到端基准，而不只测局部函数。

## 5. 工程性评价

### 5.1 优点

- **测试意识较强。** core、kit、skill 均有大量回归测试，关键并发包进入 race CI；skill 还有 parser/checkpoint fuzz。
- **正确性文档不是空泛介绍。** [NEST_TRANSACTION_WAL.md](NEST_TRANSACTION_WAL.md)、[NEST_PIPELINED_COMMIT.md](NEST_PIPELINED_COMMIT.md)、[REMOTE_ENTITY.md](REMOTE_ENTITY.md)、[SAGA.md](SAGA.md)、[OBSERVABILITY.md](OBSERVABILITY.md) 记录了不变量、失败语义和运维指标。
- **容量默认值和 fail-closed 较普遍。** 队列、journal、outbox、checkpoint、room、skill Runtime 都有上限，减少失控内存增长。
- **core/kit 边界大体正确。** 接口与协议放 core，基础设施适配放 kit，具体技能域放 skill；这比让 core 直接导入所有数据库 SDK 更可维护。
- **可观测性覆盖关键状态。** Nest fence、pipelined verdict、remote write gate、Saga lease、同步丢弃和 lockstep desync 均有指标入口。

### 5.2 问题

#### 版本与命名治理不统一

仓库目录称 `roost-core/roost-kit`，模块仍称 `cube-core/cube-kit`；commit 写 `v9.0`，正式 tag 却为 v1.8.0；core README 仍为 v1.7.0；skill 本地与远端又存在 `/v2` 和无 major suffix 两条历史。技术上 module 名称可以与品牌不同，但必须有唯一、机器可校验的发布规则。

#### Mod 依赖没有完全进入依赖图

kit 文档自己承认只有六个 Mod 实现 `DependsOn()`。`nats.reliable.enabled=true` 时 NATS 在 Provide 阶段查 Redis capability，Nest 对 Remote Entity 的可选接入也受注册顺序影响。装配顺序一旦决定功能是否生效，它就不是“可选风格”，而是正确性依赖，应进入拓扑图或以显式 option 注入。

#### CI 尚未形成统一发行证据链

目前每仓 CI 很丰富，但缺一个以候选 tag 为输入的三仓发行矩阵：

- 不允许工作区和本地 replace；
- kit 必须只依赖即将发布/已发布的 core tag；
- skill 必须只依赖对应 core/kit tag；
- 示例、codegen 产物和一个最小服务必须从空 module 安装并启动；
- README 版本、go.mod、tag 和 changelog 必须机器一致。

`roost-skill origin/main` 的 E2E workflow 仍 checkout core v1.3.0、kit v1.1.0，虽然测试 module 实际要求 v1.8.0 且可能从代理下载，因此这些旧 checkout 至少是误导且无效的测试步骤，应清理。

#### 安全与供应链门禁不足

现有 CI 没有形成稳定的 `govulncheck`、`staticcheck`、依赖许可证/SBOM、secret scan、签名 tag/制品、容器镜像扫描门禁。游戏服务暴露公网协议且依赖数据库、消息系统和压缩/加密路径，这部分不能只靠 `go vet`。

#### API 面积增长过快

core 同时包含 entity、sync、replication、lockstep、AI、taskflow、robot、gateway、security、observability 等大量包。能力丰富，但也扩大了兼容、审计和维护表面。应定义“稳定内核 API”“实验性游戏设施”“可选工具”三层稳定性等级，避免所有 package 都被用户理解为长期兼容承诺。

## 6. 实用性评价

### 6.1 对业务开发的实际帮助

Roost 最实用的地方，是把业务代码约束到 handler、component、entity/DAO 和生成的 sender/handler，而把锁、回滚、WAL、Flush、远程 fencing 和同步流放到底层。对于游戏团队，这比提供一组孤立工具更能减少重复工作。

适合的场景：

- 玩家、角色、公会、队伍等强身份 Entity；
- SLG 的跨服转移、联盟操作、奖励流程和配置热更新；
- 房间制 ARPG/MOBA 的 20 Hz 权威状态同步；
- 需要快照+增量、LOD/AOI、断线恢复或 lockstep 的对局；
- 需要可审计、可回放的 2D 技能与战斗数值。

使用成本也很明确：

- 完整能力通常依赖 MongoDB、Redis、NATS/JetStream、etcd，部署和故障模型明显重于 Skynet 的小内核；
- 强一致路径要求业务理解 handler durability、幂等 key、fence、outbox 和补偿边界，不能把所有调用都当普通 RPC；
- 目前缺一个权威 production starter、Helm/Kubernetes 参考部署、容量规划表和升级/回滚 runbook。

### 6.2 SLG 适配度

**适配度：8.5/10（发行阻断修复后）。**

Entity/Nest、Remote Entity、Saga、checkpoint、config hot reload 和后台 taskflow 对 SLG 很契合。玩家、公会、城池等对象可以按 Entity 分片，跨区操作用 Saga，读多写少数据使用 L1/L2 snapshot。

仍应由业务或后续公共包提供的能力包括：世界格/地图的跨进程 ownership 与无缝迁移、排行榜、邮件/社交、经济账本、赛季归档。这些不全是 core 必须内置的功能，但大世界 shard handoff 的 fencing 与恢复协议值得沉淀为框架能力。

### 6.3 房间制 2D ARPG/MOBA 适配度

**适配度：8.0/10（以 skill origin/main 为准）。**

状态同步、LOD/AOI、多 transport、lockstep、确定性技能与 combat component 的组合具有较完整的服务端基础。100 单位、20 Hz 的目标规模也与当前设计相符。

明确缺少且不应被文档暗示为已有的能力：3D 轴、navmesh/寻路、权威物理、客户端预测与 rollback netcode、延迟补偿、反作弊、大世界无缝分片迁移。skill README 已把前三项列为非目标，这是诚实且合理的边界；但如果宣传“通用 ARPG 后端”，必须把宿主需要补的清单放在选型首页。

### 6.4 其他游戏类型

卡牌、回合制、棋牌、放置类会受益于 Entity、Saga 和技能/效果编译，但可能不需要完整实时 transport；此时 Roost 仍可按模块使用。对极简小游戏，完整 Mongo+Redis+NATS+etcd 栈可能得不偿失，Skynet 小内核或普通 Go 服务反而更直接。

## 7. 与 Skynet、Orleans 的公平比较

### 7.1 三者不是同一层产品

- [Skynet](https://github.com/cloudwu/skynet) 是 C 核心、修改版 Lua VM 的轻量 actor/message 调度与网络框架。官方入门文档明确说它不是开箱即用引擎，而是一组工具，具体游戏服务器架构由团队决定；service 通常把业务状态常驻内存，数据库更多承担备份角色。[Skynet GettingStarted](https://github.com/cloudwu/skynet/wiki/GettingStarted)
- [Orleans](https://learn.microsoft.com/en-us/dotnet/orleans/overview) 是通用云原生 virtual actor 平台。grain 具有稳定身份，运行时负责按需 activation、定位、回收和集群故障恢复，并提供 persistence provider、stream、transaction、timer/reminder 等设施。
- Roost 是更具游戏领域观点的 Go 平台：它不像 Orleans 那样隐藏 placement/activation，也不像 Skynet 那样把数据一致性和玩法设施大多留给项目，而是显式提供 Entity 事务、WAL、Remote Entity、Saga、实时复制和技能域。

### 7.2 能力矩阵

| 能力 | Roost | Skynet | Orleans |
| --- | --- | --- | --- |
| 单对象串行 | Entity 锁 + Nest 调度；可做多 Entity 有序锁 | service mailbox，模型简单且成熟 | grain activation 单线程 scheduler，任务逐个执行 |
| 对象生命周期 | 应用显式 load/unload、route/owner | 应用创建/销毁 service | virtual actor 自动 activation/deactivation/location |
| 多对象事务 | Nest 同进程事务；Remote/Saga 处理跨服 | 通常由应用协议实现 | 支持 transactional state/ACID，但需配置相应存储 |
| 普通持久化 | checkpoint/WAL/Flush，策略明确 | 框架不规定，常由项目备份内存状态 | provider 模型；状态写入需业务显式 `WriteStateAsync` |
| 消息保证 | 按 bus、outbox、syncstream 分层；强语义需事务组合 | 集群 send/call 的断线语义由应用处理 | 默认 at-most-once；启用重试后是 at-least-once，并非 exactly-once |
| 集群 placement | ownerroute/etcd/remote entity，仍需应用编排 | 提供 cluster 基础能力，不是完整集群平台 | 运行时内建 placement、directory、membership 与多种负载策略 |
| 实时客户端网络 | UDP/KCP/QUIC、LOD/AOI、snapshot/delta、lockstep | TCP/UDP 与 gateway 原语，玩法协议由项目写 | RPC/stream 强，游戏 datagram/帧同步通常自建 |
| 技能/战斗 | 确定性 2D skill/combat 是现成领域层 | 无内建，生态/项目实现丰富 | 无内建，作为 grain/服务实现 |
| 可观测与部署 | 有指标/健康检查/robot，部署模板仍不足 | debug console 和社区实践，项目化程度取决于团队 | .NET logging、Metrics/OpenTelemetry、Kubernetes 官方集成成熟 |
| 成熟度与生态 | 新、快速演进，独立生产证据少 | 游戏行业长期使用，核心小且久经验证 | Microsoft/.NET 生态、文档和 provider 完整 |

Orleans 的单 grain 调度并不代表所有消息天然 exactly-once；官方文档说明默认消息是 at-most-once，配置重试后转为 at-least-once。[Orleans delivery guarantees](https://learn.microsoft.com/en-us/dotnet/orleans/implementation/messaging-delivery-guarantees) 它的普通 grain state 也不是每次字段变化自动落盘，业务仍要决定何时调用 `WriteStateAsync`。[Orleans persistence](https://learn.microsoft.com/en-us/dotnet/orleans/grains/grain-persistence/) 因此不能简单得出“Orleans 自动保证一切一致性”的结论。

反过来，Orleans 的 virtual actor 生命周期、集群 placement 和资源优化能力明显领先 Roost。它能在 grain 未激活时自动选择 silo，并提供资源感知 placement；较新的 activation repartition/rebalance 还能优化 locality 或资源分布，部分能力仍标为 experimental。[Orleans placement](https://learn.microsoft.com/en-us/dotnet/orleans/grains/grain-placement) Roost 当前的 ownerroute/remote entity 更显式、更贴近游戏数据正确性，但还不是同等级的透明集群调度平台。

Skynet 的优势是小、快、模型直接、长期经过真实游戏项目使用。官方也明确把它定位为调度成千上万 Lua VM/service 的轻量框架，并让服务常驻状态、用消息协作。[Skynet repository](https://github.com/cloudwu/skynet) 其弱项不是“不能做分布式游戏”，而是很多生产能力属于项目架构而非框架契约：数据库一致性、跨节点幂等、Saga、状态复制协议、技能系统都需要团队自行选择或复用生态方案。对经验丰富的 Skynet 团队，这是自由度；对希望统一正确性基线的团队，这是重复工作和审计成本。

### 7.3 性能比较应如何理解

- **Skynet** 的 C 调度内核、Lua service 隔离和固定 worker 模型极轻，成熟项目有大量经验；Lua 业务的对象分配、序列化和项目级存储协议仍会决定真实性能。
- **Orleans** 为 transparent activation、directory、serialization、cluster membership 和 provider 生态付出更多运行时开销，但能降低分布式调度的业务复杂度。单 grain 仍是串行瓶颈，需要正确选择 grain 粒度。[Orleans scheduling](https://learn.microsoft.com/en-us/dotnet/orleans/implementation/scheduler)
- **Roost** 的 Go 编译代码、按 Entity 并行、Pipelined WAL、L1 snapshot 和专用实时协议具备获得低延迟的设计条件；但复杂一致性路径包含锁、序列化、WAL、Mongo/Redis/NATS 往返。当前局部 benchmark 不能证明整链路一定快于前两者。

公平结论是：Roost 对 20 Hz、100 单位房间的目标在设计上可行，但必须用指定硬件和真实依赖测出 p50/p95/p99、锁等待、WAL durable latency、GC pause、网络 bytes/tick 和故障恢复时间后，才能称为性能达标。没有这些数据时，“高性能”是合理目标，不是已完成证明。

## 8. 生产优先级

### P0：剩余发布动作

1. 将当前 codegen 修复发布为 v1.6.1 或更高版本；发布前把脚手架自己的 `versions.codegen` 默认值同步到新 tag，并运行 generated-project release smoke。不能覆盖或复用已经发布的 v1.6.0 tag。
2. 下一次 kit tag 前先发布包含 robot API 的 core 新 tag，再把 kit 的 pseudo-version 切换为该正式 tag；tag gate 已禁止带 pseudo-version 发布。
3. 从全新 Linux 环境启动真实依赖，执行 Entity 写入、Flush、重启恢复、Remote Entity 转移、Saga 重投和 skill checkpoint 恢复。纯编译 smoke 已完成，这一项验证运行时基础设施。

### P1：进入大规模生产前完成

1. 为 Mongo unknown commit、Redis failover/WAITAOF、NATS redelivery、etcd compaction、WAL 部分写、kill -9、磁盘满建立自动故障注入矩阵。
2. 把 NATS→Redis、Nest→Remote Entity 等真实依赖全部声明为 `DependsOn` 或显式构造依赖，删除“顺序正确才生效”的隐含装配。
3. 增加全链路 soak/benchmark：Nest+WAL+Mongo、Remote Entity 热读与 ownership 迁移、100×100 20 Hz 同步、每房 100 单位技能战斗、海量房间分片。
4. 为关键链路接入统一 trace/context propagation，并发布 dashboard、告警阈值、runbook、容量模型和降级策略。
5. 增加 `govulncheck`、`staticcheck`、SBOM、依赖/许可证审查、secret scan、签名 tag/制品和容器扫描。
6. 明确 schema、WAL、checkpoint、skill Program 和同步 wire 的兼容/迁移策略，至少覆盖 N-1 滚动升级和可回滚发布。

### P2：提升通用性和竞争力

1. 提供 Kubernetes/Helm 参考部署、开发环境 compose、生产配置模板和一键故障演练。
2. 将 API 标为 stable/experimental/internal，收缩 core 的长期兼容面。
3. 提供大世界 shard ownership/handoff 公共协议；空间索引仍可在 kit，但跨进程迁移的 fence、快照和恢复应在 core 定义。
4. 提供客户端参考 SDK/协议测试向量，确保 ACK、delta、resync、LOD 和 skill applier 跨语言一致。
5. 对 Skill Runtime 明确“一房间一 Runtime/固定 shard”的部署模型；若 Host 回调可能阻塞，定义禁止项或拆分 prepare/apply 阶段。

## 9. 最终评价

### 从鲁棒性看

Roost 的核心设计已经超过多数内部游戏框架的常见水平，尤其是 indeterminate commit fail-stop、fencing、版本化 tombstone、transaction outbox 和有界同步队列。主要短板是外部依赖故障尚未形成同等强度的自动化证明，以及少数底层技巧和消息语义需要更明确的使用边界。

### 从工程性看

代码组织、测试和设计文档是明显优点。此次删除本地 replace、修正生成模板、统一稳定版本并加入 CI 门禁后，工程性源码评分从 7.6 提升到约 8.2；已发布生态评分从 5.8 提升到约 7.4。差值来自 codegen v1.6.0 仍是不可变旧模板，发布 v1.6.1 并通过 release smoke 后，发行评分预计可达到约 8.2。

### 从实用性看

对希望使用 Go、又不想让业务团队重复实现 Entity 锁、WAL、跨服 fencing、Saga、实时同步和技能执行的团队，Roost 有很强吸引力。它比 Skynet 更有观点、比 Orleans 更贴近实时游戏数据面；代价是生态和生产验证远少于两者，集群自动 placement 与部署体系也明显不如 Orleans。

最终建议不是“重写成 Skynet/Orleans”，而是保持当前差异化：

- 学 Skynet 控制内核复杂度、保持消息/调度路径可审计；
- 学 Orleans 把 activation、placement、provider、可观测和部署形成完整产品体验；
- 保留 Roost 在游戏一致性、实时同步和确定性技能上的独有优势。

core v1.8.0、kit v1.8.0、skill v1.7.0 的无 replace 组合已经达到可安装、可编译的生产候选基线。更准确的当前表述是：**运行库发行可用，codegen 的发行修复等待新 patch tag；真实基础设施故障矩阵仍是从生产候选走向无条件生产标准的主要证据缺口。**

## 10. 外部参考

- [Skynet repository](https://github.com/cloudwu/skynet)
- [Skynet GettingStarted](https://github.com/cloudwu/skynet/wiki/GettingStarted)
- [Skynet Cluster](https://github.com/cloudwu/skynet/wiki/Cluster)
- [Orleans overview](https://learn.microsoft.com/en-us/dotnet/orleans/overview)
- [Orleans scheduling](https://learn.microsoft.com/en-us/dotnet/orleans/implementation/scheduler)
- [Orleans messaging delivery guarantees](https://learn.microsoft.com/en-us/dotnet/orleans/implementation/messaging-delivery-guarantees)
- [Orleans grain persistence](https://learn.microsoft.com/en-us/dotnet/orleans/grains/grain-persistence/)
- [Orleans transactions](https://learn.microsoft.com/en-us/dotnet/orleans/grains/transactions)
- [Orleans placement](https://learn.microsoft.com/en-us/dotnet/orleans/grains/grain-placement)
- [Orleans observability](https://learn.microsoft.com/en-us/dotnet/orleans/host/monitoring/)
- [Orleans Kubernetes hosting](https://learn.microsoft.com/en-us/dotnet/orleans/deployment/kubernetes)
