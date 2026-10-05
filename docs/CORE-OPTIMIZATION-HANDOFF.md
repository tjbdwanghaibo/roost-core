# Roost 核心优化汇总与 agent 交接

**10-05 第十三批/N05镜像路由接入**：NC-33/34两个P2已修、声明场景验证，未发版；缓存与interest在写前绑定第三层payload，10副作用反例红→绿，13新增正式叶子。709最终扩大race叶子/8skip、根包14及build/vet/glsvet通过。N05仍部分完成，下一重订阅/回调交错/权威回填组合；Mirror DTO未实施。[范围/证据](review/REVIEW-2026-10-05-noncore-23.md)。

**10-05 第十二批/N04具名本机缺口收口**：[NC-32](bugfix/RR-20261005-NC-32.md)已修、声明场景验证，未发版；深层更新通知不再绑临时父副本，应用须重生成关联代码。9正式/6消费红转绿，完整金样53、消费28通过，其中11新消费覆盖嵌套迁移与持续CAS预算/恢复。N04全域仍部分完成，下一N05路由/mirror增量；真实Mongo/Cluster/HA/容量等保留。[范围/证据](review/REVIEW-2026-10-05-noncore-22.md)。

**10-05 N04接续**：[NC-31已修](bugfix/RR-20261004-NC-31.md)，未发版；提交前校验目标BSON/装载/身份，12新正式、17生成消费通过。多DAO/CAS/真实文件WAL子进程强杀恢复六项增量无新确认缺陷；五包race421pass/1helper skip、根包14、build/vet/glsvet通过，Kit integration仅编译。N04仍部分完成，下一嵌套/类型变化与持续竞争消费→具名收口→N05路由/mirror；真实Mongo/HA/长容量不冒认。[运行/进度](review/REVIEW-2026-10-05-noncore-21.md)。下方NC-31未修为10-04原发现时点。

**10-04 N04正式迁移写回接续**：[本轮](review/REVIEW-2026-10-04-noncore-20.md)确认[NC-31 P2未修](bug/RR-20261004-NC-31.md)：目标解码晚于CommitSystem，坏迁移进入WAL或写成不可加载的新schema。生成消费11叶子3fail/8控制，真实文件WAL/Projector+MongoStore、后端mongotest；build/34既有定向race/根包14/vet通过，未宣称真实Mongo/HA。3文件仅补中文注释；review与共同skill已明确必要中文说明和本地验收收尾，不等待GitHub CI。N04仍部分完成，下一多DAO/CAS消费与具名缺口收口；行为修复另走bugfix。

**10-04 用户要求的复审核对与改进**：[独立复盘](review/REVIEW-2026-10-04-fix-audit-retrospective.md)确认RR-02～07成立且六修复已包含；d3四项历史28红/4控制、定向36绿，外部限制分列。规范/本机skill接入组合契约复核，修正README残留降级说明。收尾另一线Wanted分流RR-08已修并整合，45普通NATS叶子race/vet及最终根包独立通过；本机未冒认真实broker。无产品行为修改、未增加域完成数。

**最终接手状态（b9625f4f）**：NC-30已修、未发版；上游RR-20261004-02～07均已实施。合并后241相关叶子、12 NC-30正式、16 review、11生成消费、根包12及mongotest115叶子通过；真实etcd/Mongo对照不冒认本机验收。新W-2026-10-04-02连接drain超时重试候选留待真实NATS复现，优先于迁移接入。[最终同步记录](review/REVIEW-2026-10-04-noncore-19.md#最后增量同步)。下方旧“未修”及RR-07待修为接手时点。

**Service第九批/第十一轮阶段完成（2026-09-29）**：[RR-34 Pipeline修复/验证](bugfix/RR-20260929-34.md)，[10域主链完成/34项新编号台账/具名剩余事项](review/REVIEW-2026-09-29-services-11.md)，本轮无新确认缺陷。原3/3绿、正式58叶子执行/整体19包951pass叶子、3Toxiproxy skip分列；100Service源blob未变，Core/consumer编译、vet、12RPC check通过。[机制](review/IMPLEMENTATION-SERVICE-PIPELINE-AND-REVIEW-CLOSURE.md)。后续是归档/fulfilled与外部/HA/长稳具名设计验收，不再泛化重开其余service。未发布/迁移，不升级其他核心性能专项；下方为历史时点。

**Service第八批/第十轮（2026-09-29）**：[RR-33 Mail Cluster批读修复/验证](bugfix/RR-20260929-33.md)，正式Mod跨3owner分页/claim；[新RR-34 P2 Pipeline首缺失掩盖写错误](bug/REVIEW-2026-09-29-services-10.md)只交接未改driver。[运行/停点](review/REVIEW-2026-09-29-services-10.md) · [机制/兼容](review/IMPLEMENTATION-SERVICE-MAIL-BATCH-AND-PIPELINE-ERRORS.md)。10域主链有界整理完成，965pass事件/3Toxiproxy skip分列；无key迁移、未发布/部署，不改其他核心性能专项验收。下方保留历史。


**Service第七批与第九轮（2026-09-29）**：[RR-31/32两项2/2修复验证](bugfix/SERVICE-BUGFIX-2026-09-29-07.md)，原8/8、新正式33叶子、最终integration+race17包/909事件通过；[10域主链/具名本机专项收口](review/REVIEW-2026-09-29-services-09.md)，新[RR-33 P2 Mail Cluster页](bug/REVIEW-2026-09-29-services-09.md)仅记录未修。[Pipeline候选、Match历史成本、购买fulfilled归档设计](review/IMPLEMENTATION-SERVICE-FINAL-SPECIALTIES.md)。不改变其他核心专项验收，已有业务需合并升级；HA/真实资源/迁移/长稳未全验，未发布。下方为历史时点。

**Service 第六批实施与第八轮 review（2026-09-29）**：[RR-28/29/30 3/3 修复验证](bugfix/SERVICE-BUGFIX-2026-09-29-06.md)，原15/15、新正式43叶子、完整17包/826事件通过；继续确认[Activity Cluster dispatch准入、demo grant catalog重试覆盖两个新P2](bug/REVIEW-2026-09-29-services-08.md)，仅交接未实施。[运行/停点](review/REVIEW-2026-09-29-services-08.md)、[实际机制](review/IMPLEMENTATION-SERVICE-INDEX-MAINTENANCE-AND-CLUSTER.md#第六批修复后的实现与第八轮学习)。不改下方性能专项验收；未发布、未迁移旧数据、HA/真实资金/长稳仍有限制。下方“未修”是历史时点。

**Service 第七轮 review（2026-09-29）**：[RR-28 P1 支付待办清理竞争、RR-29/30 P2 Cluster 配置准入](bug/REVIEW-2026-09-29-services-07.md)，均未修；[运行](review/REVIEW-2026-09-29-services-07.md)、[索引/Cluster 学习](review/IMPLEMENTATION-SERVICE-INDEX-MAINTENANCE-AND-CLUSTER.md)。真实 Redis/三 master Cluster 6 反例失败、9 控制通过，四包268既有事件通过。原第五批4/4保持关闭；本轮仅文档，不改下方性能专项验收，不代表HA/生产资金/容量已收敛。

**Service 第五批实施与第六轮审查（2026-09-29）**：[RR-25/26/27、旧 Activity Opening 残余 4/4 修复](bugfix/SERVICE-BUGFIX-2026-09-29-05.md)，源码 4b0837d7；[邻接审查](review/REVIEW-2026-09-29-services-06.md)无新增确认缺陷，[10 域最新矩阵](review/SERVICE-REVIEW-COMPLETION-2026-09-29.md)。16 包/830 事件回归、最终两模式各 27 叶子通过；Activity/Chat owner 升级、legacy 恢复与外部资源/HA/强杀/容量专项仍有边界。未发版，不改变下方性能专项验收；旧“未修”保留历史时点。

**Service 第四轮 review（2026-09-29）**：[问题与交接](bug/REVIEW-2026-09-29-services-04.md)：RR-23 外部发货未知错误清除证明（P1）、RR-24 Platform 回包切片共享（Memory/P3），旧 Mail 删除残余另关联 RR-20260910-02。未修生产代码；[学习及恢复入口](review/IMPLEMENTATION-SERVICE-EXTERNAL-OUTCOME-AND-DISPOSAL.md)、[运行/测试](review/REVIEW-2026-09-29-services-04.md)。本批不改变下方性能任务验收。

**Service 第三轮实施（2026-09-29）**：[RR-20260929-19～22 修复与兼容](bugfix/SERVICE-BUGFIX-2026-09-29-03.md)已完成：建角持久恢复计划、Mail 取消代次/生成消费者、Directory 原子身份删除、Profile 输出隔离；16 包、794 测试事件通过，未发版。此批不改变下方性能任务验收；Mail API/自定义 store 与旧数据对账按新交接执行。

**Service 实施交接（2026-09-29）**：[两轮 18 项新问题及旧正常 Finish ABA 修复](bugfix/SERVICE-BUGFIX-2026-09-29.md)已完成定向回归，未发版；新增 [roost-bugfix](agent-skills/roost-bugfix/SKILL.md)。这是 service 状态/恢复的独立批次，不改下方既有性能任务的验收结论。

**当前状态（2026-09-27）**：RR-20260926-01～85 全部已修复并已发布——RR-01～29、31、32 随 **v1.17.0**（2026-09-26，tag `8811c02`），RR-30、33～85 随 **v1.17.1**（2026-09-27，tag `ffcf902`）；各轮修复均已提交并推送 main。v1.17.1 之后仍未完成 / 未验证的条目统一在 [OPEN-ITEMS-2026-09-27](review/OPEN-ITEMS-2026-09-27.md)，进度以其 §H 为准。下文 §5 各条中的“未提交 / 未修复 / 未发布”是当时的记录，已在条目内注明现状。

**仍然有效的边界**：RR-10～24 的修复见[新增复审修复](review/REVIEW-2026-09-26-release-fixes.md)（已随 v1.17.0 发布）。§4 的 Remote 两行原为直接注入 MongoCommitter 的装配；2026-09-29 已在正式 kit Backend 装配复测（B30，写许可 256，见[复测记录](review/REVIEW-2026-09-29-b30.md)），结论已并入 §4；Remote + lease-fence 混合准入明确拒绝；同 SessionID 重连遇到旧队列未退出需重试。下文原批次结论需结合此更新阅读。

更新日期：2026-09-27（顶部状态与 §5；其余各节内容为 2026-09-26）。范围是本轮对话已实施的 Nest、Sync、DataEngine、Remote 优化与必要调用链，并回链更早的结构调整；不是全仓逐函数审计或永久性能保证。

**当前结论：约定的优化批次已实施，用户已接受本轮 Sync 少量 50ms 尾延迟超标，允许提交。** 原严格门禁结果仍保留为失败，不修改代码阈值，不自动推广到其他负载。24小时长稳、真实生产网络等未验证项单列。

## 1. 接手顺序与基线

1. 阅读仓库 [AGENTS.md](../AGENTS.md) 和 [roost 写代码 skill](agent-skills/roost-coding/SKILL.md)。`roost-optimize` 是同一规范的优化入口；本机安装副本不能取代仓库规则源。
2. 查看当前 `git status`、分支、`git log` 和实际 Go 工具链。本次实施基础为 main `8ce21a5`；其前 `646ce63` 汇合此前核心优化，`8ce21a5` 包含快照调度与恢复诊断。本次代码包含公平调度、九项优化及两项 RR 修复。提交身份以包含本文件的 Git 提交为准，不在文档内自引用尚未生成的 hash；提交不代表 push/部署。
3. 以下按最终形态汇总。历史文档中的“未提交”“暂缓”“待实现”只反映当时状态；若与后续已实施记录冲突，以后续记录和当前源码为准。保留历史失败与撤回实验，不批量改写旧证据。
4. 大部分运行时使用一个 Go module，基线 Go 1.27.0；框架在根目录，装配在 kit，生成器在 codegen。不要按历史 roost-kit/roost-codegen 独立仓路径改代码。

这是依据既有实施记录和最近一次实际验收整理的接手文档，本次文档整理没有重跑长稳、故障矩阵或全仓审计。

## 2. 业务目标与正式链路

目标背景：1000玩家、10000 Entity、Interest/AOI 每人约50可见（49他人加self），低变化率1%/5%，以20Hz窗口组织负载。不是1000人同room全量互播。Remote持久事务频率独立；多实体/多DAO是正式验收场景。

```text
外部请求 → 统一准入与显式目标ID依赖
  ├─ 普通已加载目标 → 快池
  └─ Slow/Remote → 慢池准备I/O/远端权限 → 内部续行进入快池
快池：Guard/本地锁 → handler/setter标脏 → 事务准入
  → on_change锁内冻结/periodic登记 → 全部本地锁释放
  → 沿原durability完成确认；Remote后置确认/释放在慢池
DataEngine：WAL → 有界回放/投影 → 持久确认及必要发布
Sync：提交条件满足 → Interest事实 → 单一Flush → 版本/预算/组帧 → Transport
```

图表示责任与依赖，durability/Remote 不同路径的确认可能与解锁并行；不能据此把所有模式改成同一串行时序。**业务执行与 Entity local 锁归快池；慢池无本地业务锁；setter不直接发送。** periodic 默认、on_change可配置；后者锁内冻结、锁外且提交条件满足才交付，50ms周期兜底。正式装配和行为细节见[双模式实施](feature/IMPLEMENTATION-2026-09-24-sync-modes.md)及[双池契约](feature/REFACTOR-2026-09-25-nest-fast-slow.md)。

目录保持 `nest/`、`dataengine/` + `dataengine/engine/`、`remoteentity/`；Sync 在 `sync/` 下按 entitysync/policy、frame、nettransport、lockstep、syncbus 分工。基建仍有独立边界，不继续为“优化”增加 manager/service 子层。

## 3. 已实施的优化全景

### Sync

| 批次 | 最终行为与收益 | 原始记录 |
| --- | --- | --- |
| 结构收敛 | EntitySync Manager统一客户端复制；Group/Interest/Direct负责组织，旧Replicator退场；同步块收进sync，传输与帧职责分离 | [结构/可读性](feature/REFACTOR-2026-09-23-sync-readability.md)、[历史收尾](feature/SYNC-COMPLETION-2026-09-23.md)；更早M-13～M-18见Git历史 |
| R1～R3与正确性 | 同包分文件、标准库API/命名整理；引用删除、替换次序、逐帧成功前缀、在途revision/lifetime、停止deadline修复 | 同上；RR-20260923-01～07 |
| 多来源与共享内容 | 各政策来源独立增删订阅；共享同Profile不可变编码，取消来源不误退役其他订阅实体 | [历史收尾](feature/SYNC-COMPLETION-2026-09-23.md) |
| A～E / Profile | AOI最远对象O(V)扫描；观察者候选去重/缓冲复用；Flush聚合工作项；会话引用表写时复制；字段白名单、优先级及重复打包治理 | [A～E最终实现](feature/REFACTOR-2026-09-23-sync-next-steps.md)、[Profile契约](feature/SYNC-PROFILES.md) |
| 六项后续 | 完整实体包组帧、Profile装配校验、快照对象/字节/会话预算、Flush/编码/AOI缓冲复用、可靠队列全局字节/年龄治理与观测 | [六项实施](feature/REFACTOR-2026-09-24-sync-six-items.md) |
| 正式双模式 | Nest option、kit配置/生命周期、生成DAO变化收集、Interest提交事实接通；on_change与periodic共用协议和提交边界 | [双模式](feature/IMPLEMENTATION-2026-09-24-sync-modes.md) |
| 生命周期与四项调度 | 会话实际订阅反向索引；有界阶段诊断；预算耗尽停止重复候选工作；Profile需求缓存；冷创建与已存在对象全量更新分开；恢复观测与窗口基准修复 | [资源预算](feature/REFACTOR-2026-09-25-resource-budgets-and-session-recovery.md)、[四项最终结果](feature/REFACTOR-2026-09-25-sync-snapshot-scheduling.md) |
| 公平调度 | 新可见/恢复分类轮转，共用原总预算及会话额度，空闲额度可借用；数量预选与字节准入同序；阶段错误留存 | [公平调度](feature/REFACTOR-2026-09-25-sync-recovery-fairness.md)、RR-20260926-01 |
| 最新S1～S3 | 待快照请求增量索引，避免遍历全部订阅；轻量Stats与显式AuditStats；同预算恢复与容量梯度验证 | [九项实施与实测](feature/REFACTOR-2026-09-26-core-nine-items.md) |

### Nest

| 批次 | 最终行为与收益 | 原始记录 |
| --- | --- | --- |
| 09-24 整体验收 N24-1～N24-4（原记录内编号 N1～N4） | 注册/dispatch/事务/回滚/诊断同包分文件；Single/Multi/MultiGroup统一收尾；准入、释放与回复责任显式化；可选阶段指标；profile驱动Timer/临时集合优化 | [Nest整体验收](feature/NEST-COMPLETION-2026-09-24.md)、[消息吞吐](feature/NEST-MSG-THROUGHPUT-2026-09-24.md) |
| 事务正确性 | pipelined锁内准入、Cast事务保护、组锁/panic/广播清理、Ticker启停、已准入后的持久完成责任、真实释放后回调 | [早期主线](feature/REFACTOR-2026-09-24-nest-and-immediate-sync.md)、RR-20260924-01～08 |
| Remote分段 | 慢准备→快业务→慢确认/释放，避免远端I/O长期占逻辑worker；内部续行保留收尾能力 | [阶段隔离](feature/REFACTOR-2026-09-25-nest-remote-stages.md) |
| 快慢双池 | main/hb/remote等执行资源归为双池；全部显式ID统一依赖顺序；无关ID共享worker；有界等待、内部续行公平与停机排空 | [双池](feature/REFACTOR-2026-09-25-nest-fast-slow.md)、RR-20260925-07～09 |
| 09-26 九项 N26-1～N26-3（九项记录内编号 N1～N3） | 快阶段Getter禁止冷加载；Slow准备声明目标；队列区分前驱与worker等待、峰值/年龄/拒绝和续行；指标key和单ID减分配 | [九项实施](feature/REFACTOR-2026-09-26-core-nine-items.md)、RR-20260926-02 |

### DataEngine 与 Remote

| 批次 | 最终行为与收益 | 原始记录 |
| --- | --- | --- |
| DataEngine职责/生命周期 | 保留两包，拆准入/回放和Mongo事务编排；操作等待可取消；Outbox、Assembly/Runtime统一启停和失败清理；空闲/held回放减少预分配 | [DataEngine分批实现](feature/REFACTOR-2026-09-24-dataengine.md) |
| 历史WAL冲突 | 确认公会ID复用，正式生成路径使用持久化发号；确定冲突进入fatal；没有跳过或改写冲突WAL | [W-02修复](bugfix/W-2026-09-22-02.md)、RR-20260924-12 |
| 多DAO批量与背压 | Store能力声明、ordered bulk及独立事务marker；checkpoint数量/时间合并但不跨越失败；原子未确认WAL限额、告警与拒绝 | [批量实施](feature/DATAENGINE-BATCH-2026-09-24.md) |
| 恢复与正式生成 | 100k积压、真实依赖故障；生成DAO/Entity→Nest→WAL/Mongo，三次独立进程及多实体多DAO、丢ack/冲突/回滚验证 | [恢复验收](feature/DATAENGINE-RECOVERY-2026-09-24.md)、[本地压测](feature/DATAENGINE-PRESSURE-2026-09-24.md) |
| Remote正确性/可读性 | 完成状态/事务缓存及收尾；启停、锁token代际、未知结果恢复；正式多实体DAO和进程故障验收 | [Remote分批记录](feature/REFACTOR-2026-09-24-remote.md)、[历史故障与长稳](feature/REMOTE-ACCEPTANCE-2026-09-24.md) |
| 持久权威与协议统一 | Mongo ownership/grant/fence约束写入；Redis仅协调；多DAO/metadata批量CAS；统一正式协议，删除未发布弱兼容/迁移入口；持久回执快读避免重复事务 | [持久权威](feature/REFACTOR-2026-09-25-remote-authority.md)、[统一与失败证据](feature/REMOTE-UNIFIED-2026-09-25.md) |
| 超时与吞吐 | 增加错误/阶段证据；独立Remote有界并行投影，保留冲突屏障及连续成功前缀ack；慢堆栈采样限频 | [超时定位与60TPS长稳](feature/REFACTOR-2026-09-25-remote-throughput.md) |
| 独立资源预算 | Remote写许可覆盖真实事务生命周期，与1024等慢worker并发独立；过载直接拒绝，不另建无界等待 | [资源预算](feature/REFACTOR-2026-09-25-resource-budgets-and-session-recovery.md) |
| 最新D1～D3 | ReplayReadBytes独立限制读取保留量；Remote有界窗口滑动补位；大记录/冷/热点/突发/混合正式链路；80TPS×30分钟及容量阶梯 | [九项实施与实测](feature/REFACTOR-2026-09-26-core-nine-items.md) |

## 4. 最新验收与用户接受的边界

**10-05 N06 S1/S2/S3/S6 复核（revn06）**：[NC-50/51/52 与 RR-20261001-06 残余](review/REVIEW-2026-10-05-n06-revn06.md)已修复、声明场景验证，未发版；versionstore 退避后重读（所有 Redis 服务的伪冲突），隔离真实 Redis 集成 891 pass/23 环境 skip。S4/S5 由其他 agent 接续，N06 仍部分完成；默认生成工程无指标落点（观察 1）待功能决定。

**10-05 N07第一批**：[NC-60～63](review/REVIEW-2026-10-05-noncore-n07.md)已修复、声明场景验证，未发版。attribute快照锁、game-demo属性层随事务回滚（已污染存量不自动修正）、属性生成器与errcode扫描的生成期拒绝；8相关包race、根包、build/vet、codegen、全新生成game-demo消费通过。event零接线与configdata发布后回调可见性记为观察，N07未整体完成。

**10-05 N06第三批**：[NC-41/42与RR-09残余](review/REVIEW-2026-10-05-noncore-27.md)已修复、声明场景验证，未发版；13新增正式回归、7行为红/overlay红绿，race420/2Cluster skip、根包14/build/vet及两生成消费通过。坏持久计划保留不自动迁移，N06未整体完成。交给另一agent的[全部后续review清单](review/REMAINING-REVIEW-HANDOFF-2026-10-05.md)从global RPC/Mod/App.Live增量接续，外部专项单列。

**10-05 N06第二批**：[NC-39/40](review/REVIEW-2026-10-05-noncore-26.md)已修复、声明场景验证，未发版。启动摘要/原生完成路由11反例红→绿，含兼容和取消恢复26新正式叶子；本地矩阵见证据。旧已推进缺摘要记录启动重投明确冲突，不自动迁移；N06仍部分完成，Service增量/跨协调器/真实Mongo/NATS/HA待补。不改变下表历史性能边界。

**10-05 N06第一批**：[NC-37/38](review/REVIEW-2026-10-05-noncore-25.md)已修复、声明场景验证，未发版；Saga三消费者健康、持久代际/两次恢复与相邻关停共20新正式叶子，相关race498/1skip、根包14、build/vet/glsvet及生成双模式通过。N05在途退订契约本机补证后接续N06；两域仍部分完成，真实Mongo/NATS/HA/容量、旧writer混跑与历史waiting处置未验收。

以下均为本机 Go1.27.0、Apple M5 的限定负载，不是生产跨机SLA。参数、命令和原始产物路径见[九项报告](feature/REFACTOR-2026-09-26-core-nine-items.md)。可随仓库携带的脱敏汇总、完整28条Sync outlier及结果文件SHA256见[证据JSON](feature/CORE-OPTIMIZATION-2026-09-26-evidence.json)；本地大日志/profile不入Git，也不保证其他checkout存在。

| 验收 | 结果及限制 |
| --- | --- |
| 功能 | 相关Entity/Nest/NestWAL/DataEngine/Sync/Kit/Metrics race、vet、glsvet通过；正式生成三进程持久化、Sync双模式、Remote三策略通过。首次NATS环境失败保留，恢复环境后通过 |
| Sync正常AOI | 1%变化p99 2.350/2.401ms，5% 4.110/5.570ms；4次测试两种超50ms计数都为0 |
| Sync集中恢复 | 8次中2次原门禁失败：1000预算/5%第1次，442050样本，计划14条/实际变化1条超标，实际max50.058ms；2000预算/1%第2次，102441样本，计划14条/实际5条，实际max50.523ms |
| Sync用户决定 | **2026-09-26用户明确表示“这个指标目前可以接受”，允许本轮交付。** 6条实际超标包含在28条计划超标里，不能相加。第一组全为delta，第二组全为恢复create；没有省略outlier。原门禁继续失败，不将接受外推到未来退化或所有对象首次可见 |
| 混合正式链路 | 同Nest、真实WAL/Mongo，持久80TPS+AOI1000/10000/50；1%/5%已有对象变化到进程内解码p99 6.142/14.715ms，最终DAO/版本/WAL/同步/AOI验证通过。不是TCP延迟 |
| Remote30分钟 | **正式 kit Backend 装配（2026-09-29，`a6931c5`，`remote_entity.max_concurrent_writes=256`）**：144000成功，79.996TPS，0错误/丢弃，p50 / p95 / p99 / max 131 / 213 / 551 / 1480ms，数据核验通过（`artifacts/perf/remote/b30-w256-sustained-80-a6931c5`）。写许可默认 128 时同负载在本机 Mongo 停顿 ≥1.6s 时被占满而失败（v1.17.1 同样），属余量不足、非回归；按定容规则 80 TPS 用 256。原直接注入装配结果：144000成功、p99 582ms |
| Remote短时容量 | **正式 kit Backend 装配（2026-09-29，写许可 256，每档 120s）**：80 档 0 错误（p99 555ms）、120 档 0 错误（119.818TPS，p99 1045ms）均核验通过；160 档 17 次写许可拒绝（p99 1544ms），阶梯停止。**最后通过档 120、首个过载档 160**（本机单机三副本 Mongo，限定负载，不是容量保证）。原直接注入装配：160 通过、240 过载 |
| 本地持久负载 | 双实体双DAO每DAO32KiB：939.01TPS；50%初始冷：645.76TPS；两个热点实体：138.39TPS；突发目标500TPS：522.66TPS（短样本整批投放，不是容量上限） |
| Nest profile | 单实体51–55→41–45 alloc，多实体61→48–52 alloc；完整请求耗时增加约2–4%。分配下降不等于吞吐提高；mutex证据不支持拆全局调度锁 |

计划延迟从计划事件时间起算；实际变化延迟从Entity修改起算，都到客户端完成该数据解码。样本是“会话×实体的一次变化交付”，不是独立实体数或帧数。冷对象旧状态不冒充新变化，首次可见/全量恢复另测。集中恢复的冷对象可能等待数秒，不能以已接受的50.523ms推断全部重连基线在50ms内到齐。

## 5. 明确没有实施、没有证明或已撤回的事项

- **已撤回**：独立10ms快照预算窗口试验无稳定收益，最终无 `SnapshotBudget.Interval` / `-snapshot-window` API；不要据旧方案补回。也没有按负载ID特判、跳过冻结或拼接opaque delta。
- **独立后续需求**：共享帧协议+网关多播需要网关/客户端配套；并行Flush、空间分片、每Profile独立版本链当前没有收益证据，不当作漏掉的收尾。
- **2026-10-05 静态绑定 + App 单实例锁已实施（main，未发版）**：[App 单实例锁方案](feature/APP-SINGLETON-LOCK-2026-10-05.md) 第 1～5 笔与[静态绑定方案](feature/PLAYEROWNER-STATIC-BINDING-2026-10-05.md)落地——core `app` 在任何 Mod Init 之前拿 `<key_prefix>:<server_type>:<sid>` 的 Redis 锁、失锁 fail-stop（`RuntimeFailure.OnFail` 先围栏 Nest）、全部 Mod 停完才释放，`app.ModSingleton` 提供只读 `Live`；game-demo 玩家只在 account 绑定的 sid 上服务（驻留表 + 闲置卸载），activity 用 `Live`，赠礼按 `FromSID` 路由；`game/playerroute`、activity 全局租约、kit `service/global` 租约 API、`redis.lock` / `etcd.election` capability 删除。第 5 笔在隔离环境做了真实进程演练（SIGSTOP / SIGCONT / kill -9 / SIGTERM、两个 sid + 跨服赠礼，时间线见方案 §13）。§7 里玩家租约相关的 RR（RR-20260920-03 / 04 / 09～12、RR-20260921-03 / 04、RR-20260926-31、RR-20260929-13 / 17、RR-20260930-23 / 24、RR-20261001-07、RR-20261004-10 / 11 / 14）所在代码已被取代，索引行已标注。
- **2026-10-05 v1.20.0 已发布（tag → `999dc672`）**：App 单实例锁（`app.Singleton` / `OnFail` / `Live`）、game-demo 静态绑定、删除 kit global 租约 API 与 election/lock capability（破坏性）、RR-20261004-10～14、nocoll、B 线 NC-31～36；本地 pretag（干净 worktree）与故障矩阵 21/21（`matrix-v1200-999dc672`）通过后打 tag，tag 后生成 game-demo（GOPROXY=direct）编译 / vet 通过。
- **2026-10-04 v1.19.2 已发布（tag → `4ee44f34`）**：v1.19.1 回归 RR-20261004-09、RR-20261004-08、历史遗留 RR-20260921-03 / 04 / 05；本地 pretag（干净 worktree）与故障矩阵 21/21（`matrix-v1192-4ee44f34`）通过后打 tag（维护者 10-04：验收以本地为准，不等 GitHub CI），tag 后生成 game-demo 编译 / vet 通过。
- **2026-10-04 v1.19.1 已发布（tag → `d3e69336`）**：NC 修复复审确认的 RR-20261004-02～07 与 NC-30；pretag、故障矩阵 21/21（`matrix-v1191-d3e69336`）、CI 全绿后打 tag，tag 后生成 game-demo 编译 / vet 通过。
- **2026-10-04 v1.19.0 已发布（tag → `74e1ba39`）**：发版前 pretag（干净 worktree）通过、Remote 故障矩阵 21/21（`artifacts/perf/remote/matrix-v1190-74e1ba39`，本地）、main CI 全绿（此前 ci 自 10-01 起红：RR-20261001-01 补修与 NC-07 补修，见 CHANGELOG）；生成的 game-demo 对 v1.18.0 与 v1.19.0 均可编译，生成器 Core 下限不变。下表 §7 中 10-01 之后各行“未发布”= 已随 v1.19.0 发布。
- **长稳（C01）已通过**（2026-10-04）：第 5 次 C01（**通过**，`5d386146`，2026-10-04 10:00Z～11:01Z，1 小时看趋势——维护者 10-04 决定，label `c01-1h-80-w256-5d386146`）：288000 笔、80.00 TPS、**Errors 0**、Dropped 0；p50 / p95 / p99 / max 142 / 265 / 945 / 1645 ms。**全量核验通过**（`result.json.verified`：Mongo / NATS 快照全部一致，回滚与 outbox 检查通过，区间核验 10000 实体 widened=0）。堆每 10 分钟最低值 109 / 112 / 114 / 110 / 115 / 119 MB（10～60 分钟平台，无单调增长），最高 214～219 MB；pprof 10m → 60m inuse 净增 4.6 MB（101.9 MB 总量，增量在 harness `scopedRedis.key` 与在途事务，不在泄漏点）；goroutine 2138～2322 平稳；tracker 在 65536 上限下持平；WAL 未确认峰值 123；采样无空洞。环境：mongod 已带 WiredTiger 缓存上限（各约 0.5～0.6 GB）。与第 3、4 次两次 24 小时的内存平台一致（RR-20260930-03 修复成立），本次在无环境停顿下补齐了错误判据与全量核验。 此前：第 4 次 C01（`746567ff`，2026-10-03 07:59Z～10-04 08:00Z，重启宿主机后，label `c01-soak-24h-80-w256-746567ff-r4`）：6911265 笔、79.99 TPS、Dropped 0、投影失败 0；p50 / p95 / p99 / max 139 / 344 / 1078 / 2547 ms（max 从 16 s 降到 2.5 s）。堆每小时最低值 107～122 MB、tracker 在 65536 饱和持平、goroutine 2271～2400，内存判据第二次通过。**Errors 735（0.011%）全部出在 19.59h 的一次约 10 s 提交停顿**（WAL 最老未确认 9.9 s，10 s 内只完成 21 笔，采样没有空洞——进程没被冻住），479 次写许可拒绝 + 256 次结果未知；同一时段 Mongo 慢查询最长 523 ms。宿主机 swap 从 0 涨到 2.7 GB：**三个 mongod 各占约 3.1 GB**——环境脚本没给 WiredTiger 缓存设上限，缺省是物理内存的一半，三节点合计约 46 GB > 32 GB 内存，长跑中逐步填满后换页。已修：`ROOST_IT_MONGO_CACHE_GB` 缺省 1（重启后每节点约 100 MB）。全量核验仍因 Errors > 0 未执行；harness 正在改为“有错误也做区间一致性核验”（成功 ≤ 实际 ≤ 成功 + 结果未知），之后跑第 5 次。 第 3 次：第 3 次 C01（`cf8b9e46`，2026-10-01 05:19Z～10-02 05:21Z，label `c01-soak-24h-80-w256-cf8b9e46`，写许可 256，heap 采样 60/360/720/1080/1380 分钟）：24 小时 6910424 笔、79.98 TPS、Dropped 0、投影失败 0、致命冲突 0；p50 / p95 / p99 / max 138 / 366 / 1134 / 16011 ms。**内存有界**：堆每小时最低值第 1 小时后稳定在 108～122 MB、最高 215～240 MB（第 1 次是 81 → 2529 MB 单调上升，RR-20260930-03 修复成立）；pprof 60m → 1380m inuse 只增 7 MB（108 → 116 MB 总量，增量在负载 harness `runRemoteLoad` / `scopedRedis.key`，不在框架）；goroutine 2264～2489 平稳；tracker 在配置上限 65536 处饱和并持平（有界，非泄漏）。**Errors 1576（0.023%）**，集中在两次宿主机停顿：8.71h（22:02 CST）进程停顿约 2.5 s（WALUnacked 190、在途 194、最长派发 7.7 s）→ 413 次写许可拒绝 + 194 次 `nest: sync timeout`（5 s）；17.70h（07:01 CST）采样空洞 14.9 s、吞吐归零 → 969 次写许可拒绝。两次停顿期间 Mongo 主节点慢查询最长 544 / 696 ms，不是 Mongo；宿主机 `fseventsd` 已 100% CPU 运行 71 天、RSS 17.6 GB，swap 9.9 / 11 GB 已用，pageouts 1064 万——是宿主机换页停顿。**全量一致性核验因 Errors > 0 未执行**（harness 先失败，无 `result.json.verified`）。结论：C01 的内存 / goroutine / tracker 判据通过；错误判据未通过但归因宿主机而非框架（与 B30、第 1 次同类）；核验缺失待宿主机修复（重启 fseventsd / 释放 swap）后复跑补齐。 更早两次：第 1 次 C01（`f46db3e`）跑满 24 小时，0 丢弃、0 投影失败，但堆每小时最低值从 81MB 单调升到 2529MB，根因为 [RR-20260930-03](bug/RR-20260930-03.md)（已修复，`42804572`）；另有 2031 次写许可拒绝，与 B30 的 Mongo 停顿同源。第 2 次（`5fedc652`）按维护者要求在约 46 分钟时中止，不作为验收。改由 fable 按 [C01-RUNBOOK](review/C01-RUNBOOK-2026-09-30.md) 重跑。30分钟堆86.94–307.58MB且后半程基线较高，事务保留接近65536上限；不能宣称排除了泄漏。缺少24小时结果不撤销用户本次接受，也不能写成长期已验证。
- **故障矩阵**：~~历史Remote版本有21/21通过，最新九项版本未完整重跑；其正式生成与相关race通过不等同新的完整集群矩阵。~~ **2026-09-27 关闭**：`f1d7591`（v1.17.1 发版前）上 `scripts/test-remote-matrix.sh` 21/21 PASS（见下方“v1.17.1 发版前验证”条；OPEN-ITEMS E 节）。仍然成立的只有：真实跨机/弱网、生产客户端与业务schema需部署环境验证（OPEN-ITEMS C02，保留）。
- **历史FlushFailures根因未证明**：RR-20260926-01修复阶段错误留存；本轮没有复现历史偶发FlushFailures，不将观测修复说成历史根因修复。
- **2026-09-26 复审核实并修复**：RR-03～06 四条成立；另确认 RR-07（WAL回放漏计）、RR-08（续行占用导致准入/指标分叉）、RR-09（Remote失败缺事务ID）。代码与防回归规则已补齐，race、静态检查及正式生成三链路通过（**现状：已提交并随 v1.17.0 发布**）；[逐项判断、负对照和边界](review/REVIEW-2026-09-26-followup.md)。Projected是成功尝试数；Close/Open不等同Hold恢复；periodic仅字节预算的预捕获成本仍存在。此前性能数字未重测。
- **2026-09-26 上线前复审登记**（登记时未修复；**现状：RR-10～24 全部已修复并随 v1.17.0 发布，2026-09-27 关闭本条的“未修复”与发版手续两项，见 OPEN-ITEMS E 节**）（基线 `aaada47`，[复审记录](review/REVIEW-2026-09-26-release.md)）：P1 级 [RR-20260926-10](bug/RR-20260926-10.md)（卸载后投影前重载 → fence 且重启不起来）、[RR-20260926-11](bug/RR-20260926-11.md)（Remote 重放被新 fence 拒绝，投影卡死）、[RR-20260926-12](bug/RR-20260926-12.md)（生成工程 syncbus 配置失效）；另 P2 十二条（RR-13～24）。**§4 的 Remote 30分钟80TPS、120/160TPS 容量与 RR-20260925-05 的 59.997TPS 由直接接 MongoCommitter 的测试装配测得，正式 kit 装配下并行投影未开启（[RR-20260926-21](bug/RR-20260926-21.md)），在正式装配复测前不作为上线依据。**（RR-21 已修复，正式装配已开启并行投影；性能口径的正式装配复测仍未做，保留为 OPEN-ITEMS B30。）发版手续（清单 v1.16.1、生成器下限、CHANGELOG）与 CI 状态见复审记录——已由后续发版完成，`8973a78` 把版本清单与生成器下限升到 v1.17.1。
- **2026-09-26 修复复验（基线 `985d5ba`）**：RR-03～24 修复方向大多成立，但引入回归 RR-25～29（其中 RR-25 为 P1：冷实体业务固定失败），另有既有缺陷 RR-30～32 与 11 条复核残留；故障矩阵 20/21 PASS（唯一失败为 RR-29 夹具）。见[复验记录](review/REVIEW-2026-09-26-fix-verification.md)。
- **2026-09-26 复验问题修复**（登记时未发布；现状：已随 v1.17.0 发布，RR-30 随 v1.17.1）：RR-25～29、31、32 已修复，RR-04/05/07/10/11/12/15/17/19/22/24 复核补修完成；RR-20260926-30 当时未修复（需维护者拍板契约），v1.17.0 之后按维护者决定（实体屏障 + 跳过后驱逐重载）修复，随 v1.17.1 发布。RR-28 复核中发现并堵上“持久拒绝后解冻会把被拒绝的内存修改写出”（含修复前 Durability 1 首轮拒绝的同类泄漏）。已随 **v1.17.0**（2026-09-26）发布；RR-30 作为已知风险写入发布说明。
- **2026-09-26 v1.17.0 疑点核实**：登记 RR-20260926-33～47（8 条 P2、7 条 P3），修复方向经维护者批准并写入各 RR；见[核实记录](review/REVIEW-2026-09-26-v1170-triage.md)。
- **2026-09-26 修复合并后独立审计（`2330d57`）**：RR-30、33～47 红绿成立；新登记 RR-20260926-48～63（2 条 P2、13 条 P3、1 条 P4），修复方向经维护者批准；见[审计记录](review/REVIEW-2026-09-26-audit.md)。
- **2026-10-01 B29 性能对照**（`1836ebce`）：6 组 RR 修复修前 / 修后 benchstat（n=10）与 E / F 端到端，无新退化；已记录代价：RR-16 合并提交 async 投影准入每笔 +5 allocs、p99 +7～14%（writers 1 / 8）；RR-20260928-11 pipelined 回退 strict 端到端 WAL fsync +22%、延迟无退化；RR-74 可回滚事务 +1 alloc；RR-25 准入冷判定 +26～87 ns/请求。证据 [REVIEW-2026-10-01-b29](review/REVIEW-2026-10-01-b29.md)，结果 `artifacts/perf/{b29-micro,remote/b29e-*,sync/b29f-*}`（本地）。C03 的 macOS 侧 D / E 组数字由此更新，Linux 对照仍不做。
- **2026-09-30 v1.18.0 已发布（tag → `4b277176`）**：A 线 RR-20260928-15、RR-20260930-03/11 与 B 线 RR-20260929-01～34、RR-20260930-01/02/04～10；含源码不兼容的 API 变化（`Mail.CancelClaim`、`session.ClaimStore`、`platform.Admin`），故为次版本，见 [CHANGELOG](../CHANGELOG.md)。发版前故障矩阵 21/21 PASS（`artifacts/perf/remote/matrix-v1180-30085d62`）。
- **2026-09-30 问题存档**：两条工作线全部编号的状态、仍开放事项与证据索引见 [ARCHIVE-2026-09-30](review/ARCHIVE-2026-09-30.md)；C01 执行手册见 [C01-RUNBOOK](review/C01-RUNBOOK-2026-09-30.md)。
- **2026-09-28 v1.17.2 已发布（tag → `924cc5d`）**：之后仍未完成 / 未验证 / 保留的条目见 [REMAINING-2026-09-28](review/REMAINING-2026-09-28.md)，以它为准（[OPEN-ITEMS-2026-09-27](review/OPEN-ITEMS-2026-09-27.md) 作为过程记录保留）。
- **2026-09-28 v1.17.2 发版前验证（`51fc7ee`）**：残留清单批次 1～22 与第五～八轮独立审计的修复合并后，干净工作树完整门禁通过（build / vet / glsvet / gofmt 0、整仓 race 120 包、重复运行、sync-modes、新生成 game-demo 17 包 / doctor / diff 0）；推送后 CI 除 framework-compat（待本版发布）外全绿，windows-compatibility 转绿；故障矩阵 `scripts/test-remote-matrix.sh` **21/21 PASS**（`artifacts/perf/remote/matrix-v1172-51fc7ee`）。生成器下限升到 v1.17.2（v1.17.1 上生成的 syncbus 测试编译失败，已实测）。
- **2026-09-27 残留总清单**：v1.17.1 之后所有“有缺陷 / 未验证 / 未完成”条目去重汇总在 [OPEN-ITEMS-2026-09-27](review/OPEN-ITEMS-2026-09-27.md)，按批次逐条处理，进度以该文件 §H 为准。
- **2026-09-27 v1.17.1 发版前验证（`f1d7591`）**：RR-83～85 与 RR-73/80 复核补修合并后，干净工作树 build / vet / glsvet、整仓 `-race`（120 包）、sync-modes 通过；故障矩阵 `scripts/test-remote-matrix.sh` **21/21 PASS**（`artifacts/perf/remote/matrix-v1171-f1d7591`，本地隔离环境，未提交产物）。RR-83～85 未经独立审计。
- **2026-09-27 第四轮修复后独立审计（`8d06d80`）**：RR-73～80 红绿成立（75、79 部分成立）；新登记 RR-20260926-84、85，复核残留追加在原 RR；RR-81/82 另由修复方修复、RR-83 登记；见[审计记录](review/REVIEW-2026-09-27-audit4.md)。
- **2026-09-27 第三轮修复后独立审计（`736bdb2`）**：RR-64～72 红绿成立；新登记 RR-20260926-73～80，RR-74/75 经维护者拍板“拒绝”；见[审计记录](review/REVIEW-2026-09-27-audit3.md)。
- **2026-09-27 第二轮修复后独立审计（`7706419`）**：RR-48～63 红绿成立；新登记 RR-20260926-64～72，方向经维护者批准；见[审计记录](review/REVIEW-2026-09-27-audit2.md)。
- **图谱尚未刷新**：本文原写最后 generation `2026-09-25T11:41:37Z`；第三、四轮审计（audit3 / audit4）实查为 **2026-09-26 08:36**，以后者为准（2026-09-27 `index_status` 显示 ready，但不给出 generation 时间，之后未刷新）。刷新曾被“pre-coordination or unverified CBM generation is active”阻止；新代码以源码/实际测试补证。环境恢复后重建并检查coverage；不要清未知锁或中断其他实例（OPEN-ITEMS C33）。

## 6. 复跑与交付约定

从模块根目录执行，选择任务影响范围；命令不意味着本次已经再次执行。GOWORK=off用于独立模块验证；本地隔离依赖按 [DataEngine恢复指南](feature/DATAENGINE-RECOVERY-2026-09-24.md)准备，不将当前机器env.sh或凭据提交。性能期间不要同时跑race、故障注入或索引。

```sh
GOWORK=off go test -race ./entity ./nest ./nestwal ./dataengine/... ./kit/dataengine ./sync/... ./kit/nest ./metrics
GOWORK=off go vet ./entity ./nest ./nestwal ./dataengine/... ./kit/dataengine ./sync/... ./kit/nest ./metrics
GOWORK=off go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync
bash scripts/test-sync-modes-generated.sh
# 已配置隔离Mongo/Redis/NATS的环境后：
bash scripts/test-dataengine-generated.sh
bash scripts/test-remote-generated.sh
# 按需独立运行；此脚本会注入故障，不与其他验收共用正在运行的环境：
bash scripts/test-remote-matrix.sh
```

性能入口：`scripts/perf/nest.sh`（微基准）、`nest-msg.sh`（消息）、`dataengine.sh`（生成持久链路）、`sync-aoi.sh` / `sync-recovery.sh`（AOI/恢复）、`remote.sh` / `remote-capacity.sh`（持续/阶梯）。每次用新的label目录，实际参数见九项报告“复跑入口”，其中包含24小时命令。脚本原50ms失败退出仍保留，读者需结合本节用户接受的具体样本判定，不能对其他失败一律忽略退出码。

提交前检查 diff、生成物、链接和适用回归；不要将 artifacts 的源码备份/二进制纳入 `go test ./...` 后把发现的重复包当成框架错误。功能变化必须有可追踪的issue或方案，当前待办与接受决定更新此文；不要强行刷新旧测试数字。后续agent汇报要区分当前实测与引用历史。

## 7. 缺陷记录索引
10-05 N09 skill 第一批（revn09）：[NC-110](bug/RR-20261005-NC-110.md) / [NC-111](bug/RR-20261005-NC-111.md) / [NC-112](bug/RR-20261005-NC-112.md)（P2）与 [NC-113](bug/RR-20261005-NC-113.md)（P3）**已复现，未修复**；[本轮](review/REVIEW-2026-10-05-n09-batch1.md)。施法终止路径收尾不一致（方向判断见本轮 §7）；combat 状态随 Nest 回滚恢复。
10-05 N03 revn03：[NC-90](bug/RR-20261005-NC-90.md) / [NC-91](bug/RR-20261005-NC-91.md) / [NC-92](bug/RR-20261005-NC-92.md) 三个 P2 与 [NC-93](bug/RR-20261005-NC-93.md) P3 **已复现，未修复**——JetStream 停止不排空在途 handler、轻量 RPC 被派发拒绝不回包、轻量调用被 JetStream 请求流截获、选主 Resign 不受预算约束；[本轮](review/REVIEW-2026-10-05-noncore-n03.md)（含方向判断：Bus 停止 / 排空与 etcd 选主各自连续多轮出缺陷）。

10-05 N06 revn06：[NC-50](bug/RR-20261005-NC-50.md) / [NC-51](bug/RR-20261005-NC-51.md) / [NC-52](bug/RR-20261005-NC-52.md) 与 [RR-20261001-06 残余](bugfix/RR-20261001-06.md#复核后的补修2026-10-05) **已修复、声明场景验证，未发版**；[本轮](review/REVIEW-2026-10-05-n06-revn06.md)、[证据](bugfix/evidence/noncore-bugfix-20261005-revn06/README.md)。T-229/230，T-182 追加。无格式/API/wire 变化，NC-52 只在输掉 CAS 后多一次 GET。

10-05 N01 / N06-S4 审查：[RR-20261005-01](bug/RR-20261005-01.md) P2 **已修复，未发版**（[修复](bugfix/RR-20261005-01.md)）——game-demo activity 的 `activity.game_sids` 含重复 sid 或候选超过 `app.SingletonLiveMaxSIDs` 时修复前启动成功、此后每个窗口被协调器 / `Live` 拒绝，现在启动时按键名拒绝。单实例锁本体无新确认缺陷，观察 9 条见[运行记录](review/REVIEW-2026-10-05-n01s4.md)。

10-05 N07第一批：[NC-60](bug/RR-20261005-NC-60.md) / [NC-61](bug/RR-20261005-NC-61.md) / [NC-62](bug/RR-20261005-NC-62.md) / [NC-63](bug/RR-20261005-NC-63.md) **已修复、声明场景验证，未发版**；[证据](bugfix/evidence/noncore-bugfix-20261005-n07/README.md)。模板改动需 `roost project sync`；用常量编号的 errcode / float 属性声明在生成期失败；T-227/228。

10-05 drill6 saga：[U-0281](bugfix/U-0281-saga-expired-command-nak-forever.md) **已修复，未发版**——原生步骤消费者对过期且无回执的命令改为 ack（修前返回 `context.DeadlineExceeded` 无限 nak，占满共享 durable 的 MaxAckPending），T-225。[U-0280](bugfix/U-0280-saga-step-reexecuted-after-crash.md) **已定位、已确定性复现，方案待维护者决定，未实施**——收件箱按 CommandID 去重、命令截止只有一次 Timeout 而 claim 租约 2 分钟、协调器丢弃 / 吞掉迟到结果，崩溃或投影积压后同一步骤再执行一次、或放弃后迟到生效；v1.19.2 同样存在；记录含候选 A～F、推荐与“saga 方向判断”（身份 / 时间 / 最终性三处设计未对齐，建议先定步骤执行契约再修），T-226。

10-05第十七批：[NC-41](bug/RR-20261005-NC-41.md) / [NC-42](bug/RR-20261005-NC-42.md) **已修复、声明场景验证，未发版**；[outbox修复](bugfix/RR-20261005-NC-41.md)、[opening修复](bugfix/RR-20261005-NC-42.md)。[RR-20261001-09残余](bugfix/RR-20261001-09.md#复核后的补修2026-10-05)追加、未发版，T-181修订，本轮T-223/224；无格式/API/自动迁移。

10-05 U-0279：[Nest 暂时性冲突重排加抖动](bugfix/U-0279-nest-requeue-jitter.md) **已修复，未发版**。v1.20.0 整体验证 `TestGeneratedDataEngineCrossCreateResolvesOnRealWAL` 耗尽 400 次上限，正常负载下 v1.20.0 / v1.19.2 失败率 35%～53%（既有问题，满载时反而罕见）；固定 5ms 重排 + 单定时器延迟队列让对称交叉创建每轮重演（活锁），改为 5ms + [0, 5ms) 抖动（OPEN-ITEMS C09 预案），上限与最短窗口不变。T-220。

10-05第十六批：[NC-39](bug/RR-20261005-NC-39.md) / [NC-40](bug/RR-20261005-NC-40.md) **已修复、声明场景验证，未发版**。[启动修复/兼容](bugfix/RR-20261005-NC-39.md)、[路由修复](bugfix/RR-20261005-NC-40.md)、[证据](bugfix/evidence/noncore-bugfix-20261005-16/README.md)、[机制](review/IMPLEMENTATION-SAGA-START-IDENTITY-AND-CANCELLATION.md)。T-221/222；optional持久字段要求统一升级writer，不自动补旧已推进记录，拒绝消息不自动改路由。

10-05第十五批：[NC-37](bug/RR-20261005-NC-37.md) / [NC-38](bug/RR-20261005-NC-38.md) **已修复、声明场景验证，未发版**。三个消费者health与incarnation持久映射；[健康修复](bugfix/RR-20261005-NC-37.md)、[代际修复/兼容](bugfix/RR-20261005-NC-38.md)、[证据](bugfix/evidence/noncore-bugfix-20261005-15/README.md)、[机制/复盘](review/IMPLEMENTATION-SAGA-CONSUMER-HEALTH-AND-DURABLE-RESUME.md)。T-218/219。新BSON字段不需要重生成，但旧writer完整Replace会丢字段；不自动修历史回执或生产记录。

10-05第十四批：[NC-35](bug/RR-20261005-NC-35.md) / [NC-36](bug/RR-20261005-NC-36.md) 与 [RR-20260913-08残余](bugfix/RR-20260913-08.md) **已修复、声明场景验证，未发版**。权威加载绑定完整请求键，最终L1重新验最低版本与当前有效期；T-216/217，旧T-69追加。[红绿/复跑](bugfix/evidence/noncore-bugfix-20261005-14/README.md)、[本轮进度](review/REVIEW-2026-10-05-noncore-24.md)、[读取与生命周期机制](review/IMPLEMENTATION-AUTHORITATIVE-SNAPSHOT-POSTCONDITIONS.md)。同一源码19新正式叶子，真实broker/HA/跨节点水位/长容量仍未验，不扩写另一线核心专项或新App feature完整验收。

10-05第十三批：[NC-33](bug/RR-20261005-NC-33.md) / [NC-34](bug/RR-20261005-NC-34.md) **已修复、声明场景验证，未发版**；[缓存修法](bugfix/RR-20261005-NC-33.md)、[兴趣修法](bugfix/RR-20261005-NC-34.md)、[红绿/复跑](bugfix/evidence/noncore-bugfix-20261005-13/README.md)。T-214/215。格式不变、错误身份写前拒绝；不新增认证、版本化Delete或DTO Mirror。

10-05第十二批：[NC-32](bug/RR-20261005-NC-32.md) **已修复、声明场景验证，未发版**；[决策/重新生成要求](bugfix/RR-20261005-NC-32.md)、[红绿](bugfix/evidence/noncore-bugfix-20261005-12/README.md)。T-211。唯一父归属保护保留，无BSON/WAL格式或公开签名变更，不补历史漏写数据。

10-05第十一批：[NC-31](bug/RR-20261004-NC-31.md) **已修复、声明场景验证，未发版**；[决策/兼容性](bugfix/RR-20261004-NC-31.md)、[红绿](bugfix/evidence/noncore-bugfix-20261004-11/README.md)、[多DAO/CAS/进程恢复](review/REVIEW-2026-10-05-noncore-21.md)。T-210。缺Loader/Id的手写候选现在unsupported；不改在线对象，不自动修坏WAL/生产数据，不重开另一线核心完整专项。

10-04第十批修复/非三大核心第十一批：[NC-30 P2](bug/RR-20261004-NC-30.md)已确认并修复，registry在Lua副作用前裁决；12正式、六包race/vet221叶子、16新review、11正式生成消费者通过。[红绿](bugfix/evidence/noncore-bugfix-20261004-10/README.md) · [迁移/schema学习](review/IMPLEMENTATION-DAO-MIGRATION-HYDRATION-AND-REFHMAP-SCHEMA.md)。无新增待修RR，T-208，未发版。N04仍41候选累计源文已读、场景部分完成；下一Repository持久迁移/重载消费（复用核心另一线证据）→N04收口→N05增量。类型迁移/Mongo/Cluster/HA/弱网/长容量留项；NC-26～29只补索引状态遗漏。

最终整合更新：RR-20261004-01已由上游3bb901fb/5d386146修复，本轮4真实Redis探针+13正式race/vet+3真实Redis集成独立通过。[验收](review/evidence/noncore-review-20261004-18/README.md#独立验收上游修复)。下方本轮新wanted未修为发现时点；原双方证据保留，未发版，未替代authority/三资源/HA/长稳验收。

提交前fetch到cfe878fe并整合四个远端提交；新W-2026-10-04-01已登记[RR-20261004-01](bug/RR-20261004-01.md) P2未修：实际取锁后丢回复遗失token。4叶子2fail/2控制，产品未改；连续未知/authority/代际验证留项。原N0414项无新RR的结论不包括此追加wanted，远端harness/长稳仅接手未本机验收。

2026-10-04 B线第九批修复与N04第五批（起点`3d3b22c9`，未发版）：

| 范围 | 当前状态 | 证据 |
| --- | --- | --- |
| NC-26～29 | 四P3已修、声明场景验证；BSON路径/unique/bulk预检/私有事务 | [运行](review/REVIEW-2026-10-04-noncore-17.md)，28正式回归/红重放；消费者夹具2初始失败已适配，674pass/17skip |
| Redis锁/AutoExtend/pubsub | 14新增场景全通过，本批未确认新RR，域仍部分完成 | [审查](review/REVIEW-2026-10-04-noncore-18.md) · [学习](review/IMPLEMENTATION-REDIS-LOCK-RENEWAL-AND-PUBSUB-LIFETIME.md) |

N04新增事务实现文件后当前源文41/41累计已读；真实弱网/租期时钟/订阅总体关闭/Cluster/HA/长稳留项。下一schema/并发/未知恢复→正式迁移；下方历史未修不作当前状态。三大核心生产实现未改，只调整必要消费者测试夹具。

2026-10-04 B线第八批修复与N04第四批接续（起点`ce90e90d`，未发版）：

| 问题 | 当前状态 | 证据 |
| --- | --- | --- |
| NC-21～25 | 五项已修、声明场景验证；Eval不重放，替身复制/成员/精度/post-image正确 | [修复](review/REVIEW-2026-10-04-noncore-15.md)，40新增正式叶子/十包race-vet/两个生成DAO真实消费者；扩展消费17skip留项 |
| NC-26～29 | 四个P3已确认未修，仅mongotest；D路径、unique建立、bulk预检、并发restore | [审查/13叶子](review/REVIEW-2026-10-04-noncore-16.md) · [问题](bug/REVIEW-2026-10-04-noncore-16.md) · [方案](review/IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md) |

N04源文40/40累计复用已读、场景部分完成；另一线三大核心只跑受影响回归，不重审或改实现。下方旧未修保留当时时点，真实Mongo/Cluster/HA/长稳及性能不由本次宣布完成。

以下索引覆盖此次核心优化阶段2026-09-23～26的RR记录；保留独立问题与修复文件，修复记录内有原始复现、验证和限制，不把索引等同于本轮逐项复验。更早记录继续查 [bug索引](bug/README.md)、[bugfix索引](bugfix/README.md) 和 [review索引](review/README.md)。

| 问题 | 主题 | 修复记录 |
| --- | --- | --- |
| [RR-20261004-NC-21～25](bug/REVIEW-2026-10-04-noncore-14.md) | 未知Lua重放一P2、mongotest复制/候选/精度/返回身份四P3；已确认、未修 | [运行/40源文范围](review/REVIEW-2026-10-04-noncore-14.md)、[机制](review/IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md)、[反例](review/evidence/noncore-review-20261004-14/README.md)；场景部分完成 |
| [RR-20261004-NC-16～20](bug/REVIEW-2026-10-04-noncore-12.md) | RefHMap根形状、文本codec、nil父Patch与名称碰撞及mongotest分页；5/5已修、声明场景验证，未发版 | [16](bugfix/RR-20261004-NC-16.md)、[17](bugfix/RR-20261004-NC-17.md)、[18](bugfix/RR-20261004-NC-18.md)、[19](bugfix/RR-20261004-NC-19.md)、[20](bugfix/RR-20261004-NC-20.md)、[运行](review/REVIEW-2026-10-04-noncore-13.md)；41正式项、十包race/vet、生成ref-hmap真实消费，T-202～205 |
| [RR-20261004-NC-13～15](bug/REVIEW-2026-10-04-noncore-10.md) | ReadThrough fatal裁决、Layered一致性回填、四Store旧写错误；3/3已修、声明场景验证，未发版 | [13](bugfix/RR-20261004-NC-13.md)、[14](bugfix/RR-20261004-NC-14.md)、[15](bugfix/RR-20261004-NC-15.md)、[运行](review/REVIEW-2026-10-04-noncore-11.md)；41正式项、真实Redis、两个正式生成消费者，T-199～201 |
| [RR-20261004-NC-11/12](bug/REVIEW-2026-10-04-noncore-08.md) | Campaign setup预算/第三方watcher关闭；2/2已修、声明场景验证，未发版 | [11](bugfix/RR-20261004-NC-11.md)、[12](bugfix/RR-20261004-NC-12.md)、[运行/限制](review/REVIEW-2026-10-04-noncore-09.md)；15正式项、T-197/198，正常Resign预算/真实服务端回收仍开放 |
| [RR-20261004-NC-08～10](bug/REVIEW-2026-10-04-noncore-06.md) | JS无handler回包协议、Assembly callback停止预算、ServiceRPC发现预算；3/3已修、声明场景验证，未发版 | [08](bugfix/RR-20261004-NC-08.md)、[09](bugfix/RR-20261004-NC-09.md)、[10](bugfix/RR-20261004-NC-10.md)、[运行/限制](review/REVIEW-2026-10-04-noncore-07.md) |
| [RR-20261004-NC-05～07](bug/REVIEW-2026-10-04-noncore-04.md) | 限流拒绝占 key、Recover report panic、非法生成路由注册 panic；3/3已修、声明场景验证，未发版 | [05](bugfix/RR-20261004-NC-05.md)、[06](bugfix/RR-20261004-NC-06.md)、[07](bugfix/RR-20261004-NC-07.md)，[运行/环境边界](review/REVIEW-2026-10-04-noncore-05.md) |
| [RR-20261003-NC-01～04](bug/REVIEW-2026-10-03-noncore-01.md) | 10-04 单 Mod 校验、Clone timeout、错误 HTTP 状态与完整 JSON 绑定；四项已修、具名场景验证，未发版 | [01](bugfix/RR-20261003-NC-01.md)、[02](bugfix/RR-20261003-NC-02.md)、[03](bugfix/RR-20261003-NC-03.md)、[04](bugfix/RR-20261003-NC-04.md) |
| [RR-20261004-NC-01～04](bug/REVIEW-2026-10-04-noncore-02.md) | Manager Stop panic / 重复 Start、Admin schema 引用与 Ops 取消关闭；4/4 已修、声明场景验证，未发版，Health 空 Status 仍为观察 | [01](bugfix/RR-20261004-NC-01.md)、[02](bugfix/RR-20261004-NC-02.md)、[03](bugfix/RR-20261004-NC-03.md)、[04](bugfix/RR-20261004-NC-04.md)，[运行](review/REVIEW-2026-10-04-noncore-03.md) |
| [RR-20260923-01](bug/RR-20260923-01.md) | 等待新快照的订阅被当作从未交付，退订后残留客户端对象 | [修复](bugfix/RR-20260923-01.md) |
| [RR-20260923-02](bug/RR-20260923-02.md) | tick 部分交付后重试，内容基线和帧时钟与客户端分叉 | [修复](bugfix/RR-20260923-02.md) |
| [RR-20260923-03](bug/RR-20260923-03.md) | 满容量会话合法替换对象，因 subject ID 排序被关闭 | [修复](bugfix/RR-20260923-03.md) |
| [RR-20260923-04](bug/RR-20260923-04.md) | 在途交付覆盖新的会话或订阅意图 | [修复](bugfix/RR-20260923-04.md) |
| [RR-20260923-05](bug/RR-20260923-05.md) | Stop/Close 等待 Flush 忽略 deadline，并允许旧循环未退出就重启 | [修复](bugfix/RR-20260923-05.md) |
| [RR-20260923-06](bug/RR-20260923-06.md) | Group.AddSubject 部分失败后残留成员状态且无法重试 | [修复](bugfix/RR-20260923-06.md) |
| [RR-20260923-07](bug/RR-20260923-07.md) | 不同政策相互撤销订阅，Group 关闭误退役共享实体 | [修复](bugfix/RR-20260923-07.md) |
| [RR-20260924-01](bug/RR-20260924-01.md) | pipelined 的准入回调在 Entity 解锁后运行 | [修复](bugfix/RR-20260924-01.md) |
| [RR-20260924-02](bug/RR-20260924-02.md) | release hook panic 泄漏 Nest 组锁和 scope | [修复](bugfix/RR-20260924-02.md) |
| [RR-20260924-03](bug/RR-20260924-03.md) | 动态 Cast 的事务保护错误依赖 Sync 接线 | [修复](bugfix/RR-20260924-03.md) |
| [RR-20260924-04](bug/RR-20260924-04.md) | 广播释放异常漏归还引用并中断后续实体 | [修复](bugfix/RR-20260924-04.md) |
| [RR-20260924-05](bug/RR-20260924-05.md) | 异步提交完成可能早于 Entity 解锁 | [修复](bugfix/RR-20260924-05.md) |
| [RR-20260924-06](bug/RR-20260924-06.md) | Ticker 并发 Start/Stop 可能残留运行 | [修复](bugfix/RR-20260924-06.md) |
| [RR-20260924-07](bug/RR-20260924-07.md) | pipelined 释放异常跳过完成或丢失回复 | [修复](bugfix/RR-20260924-07.md) |
| [RR-20260924-08](bug/RR-20260924-08.md) | pipelined 内联完成吞掉 AfterCommit 错误 | [修复](bugfix/RR-20260924-08.md) |
| [RR-20260924-09](bug/RR-20260924-09.md) | DataEngine 等待投影与停机所有权时忽略截止时间 | [修复](bugfix/RR-20260924-09.md) |
| [RR-20260924-10](bug/RR-20260924-10.md) | Outbox 启停分离导致关闭后启动与重复关闭通道 | [修复](bugfix/RR-20260924-10.md) |
| [RR-20260924-11](bug/RR-20260924-11.md) | DataEngine 启动/关闭与失败清理缺少统一所有权 | [修复](bugfix/RR-20260924-11.md) |
| [RR-20260924-12](bug/RR-20260924-12.md) | Remote 永久版本冲突未触发 DataEngine fencing | [修复](bugfix/RR-20260924-12.md) |
| [RR-20260924-13](bug/RR-20260924-13.md) | 慢 Outbox 发布耗尽失败后的重试窗口 | [修复](bugfix/RR-20260924-13.md) |
| [RR-20260924-14](bug/RR-20260924-14.md) | 事务内 Put 重复键被误处理为可重试超时 | [修复](bugfix/RR-20260924-14.md) |
| [RR-20260924-15](bug/RR-20260924-15.md) | 同批重复事务 ID 覆盖 digest 校验 | [修复](bugfix/RR-20260924-15.md) |
| [RR-20260924-16](bug/RR-20260924-16.md) | FlushRemoteAll 丢失被淘汰的完成结果 | [修复](bugfix/RR-20260924-16.md) |
| [RR-20260924-17](bug/RR-20260924-17.md) | 无效 Remote 提交消耗 pending 容量 | [修复](bugfix/RR-20260924-17.md) |
| [RR-20260924-18](bug/RR-20260924-18.md) | RemoteChecksum 不能写入真实 Redis | [修复](bugfix/RR-20260924-18.md) |
| [RR-20260924-19](bug/RR-20260924-19.md) | Remote 装配启停缺少生命周期所有权 | [修复](bugfix/RR-20260924-19.md) |
| [RR-20260924-20](bug/RR-20260924-20.md) | 旧解锁回复清除新一代本地状态 | [修复](bugfix/RR-20260924-20.md) |
| [RR-20260924-21](bug/RR-20260924-21.md) | Redis 锁版本和 fence 的 Lua 返回丢精度 | [修复](bugfix/RR-20260924-21.md) |
| [RR-20260924-22](bug/RR-20260924-22.md) | fence 分配失败遗留无 TTL 的 owner | [修复](bugfix/RR-20260924-22.md) |
| [RR-20260924-23](bug/RR-20260924-23.md) | Remote 冷准入脱离调用方取消，多实体重复计算预算 | [修复](bugfix/RR-20260924-23.md) |
| [RR-20260924-24](bug/RR-20260924-24.md) | Redis Cluster 已选主，客户端仍访问旧主 | [修复](bugfix/RR-20260924-24.md) |
| [RR-20260924-25](bug/RR-20260924-25.md) | Remote Cluster 锁缺少同槽配置校验 | [修复](bugfix/RR-20260924-25.md) |
| [RR-20260924-26](bug/RR-20260924-26.md) | 未复制 Redis 锁写丢失后，已确认 fence 被复用 | [修复](bugfix/RR-20260924-26.md) |
| [RR-20260925-01](bug/RR-20260925-01.md) | 重复故障测试遗留 JetStream 流，耗尽预留容量 | [修复](bugfix/RR-20260925-01.md) |
| [RR-20260925-02](bug/RR-20260925-02.md) | 直接构造 MongoCommitter 可绕过持久许可 | [修复](bugfix/RR-20260925-02.md) |
| [RR-20260925-03](bug/RR-20260925-03.md) | 慢请求全堆栈诊断放大 Remote 压力 | [修复](bugfix/RR-20260925-03.md) |
| [RR-20260925-04](bug/RR-20260925-04.md) | Remote 原子投影后又开启只读 Mongo 事务 | [修复](bugfix/RR-20260925-04.md) |
| [RR-20260925-05](bug/RR-20260925-05.md) | Remote 串行投影放大 Nest 请求排队和确认超时 | [修复](bugfix/RR-20260925-05.md) |
| [RR-20260925-06](bug/RR-20260925-06.md) | Remote 慢操作占用 Nest 业务 worker | [修复](bugfix/RR-20260925-06.md) |
| [RR-20260925-07](bug/RR-20260925-07.md) | Nest 跨池顺序与慢阶段本地执行边界 | [修复](bugfix/RR-20260925-07.md) |
| [RR-20260925-08](bug/RR-20260925-08.md) | 小等待队列提前拒绝可并发请求 | [修复](bugfix/RR-20260925-08.md) |
| [RR-20260925-09](bug/RR-20260925-09.md) | Remote 回滚 panic 遗留 Entity 本地锁 | [修复](bugfix/RR-20260925-09.md) |
| [RR-20260925-10](bug/RR-20260925-10.md) | 同 ID 重开会话可能继承旧订阅 | [修复](bugfix/RR-20260925-10.md) |
| [RR-20260925-11](bug/RR-20260925-11.md) | 通用故障矩阵遗漏 Remote 集成套件 | [修复](bugfix/RR-20260925-11.md) |
| [RR-20260925-12](bug/RR-20260925-12.md) | 现有对象全量刷新被冷恢复预算阻塞 | [修复](bugfix/RR-20260925-12.md) |
| [RR-20260925-13](bug/RR-20260925-13.md) | 冷快照窗口边界随晚醒漂移 | [修复](bugfix/RR-20260925-13.md) |
| [RR-20260926-01](bug/RR-20260926-01.md) | Sync policy 失败丢失 LastError | [修复](bugfix/RR-20260926-01.md) |
| [RR-20260926-02](bug/RR-20260926-02.md) | Nest 快阶段允许冷加载占用逻辑 worker | [修复](bugfix/RR-20260926-02.md) |
| [RR-20260926-03](bug/RR-20260926-03.md) | Slow 快阶段继承慢阶段执行器，RunLocal 在快池内自等，快池饥饿死锁 | [修复](bugfix/RR-20260926-03.md) |
| [RR-20260926-04](bug/RR-20260926-04.md) | 字节软预算下大对象被持续插队而饿死 | [修复](bugfix/RR-20260926-04.md) |
| [RR-20260926-05](bug/RR-20260926-05.md) | 快照额度在 Push 前预扣，RetryLater 后窗口额度被未尝试会话占满 | [修复](bugfix/RR-20260926-05.md) |
| [RR-20260926-06](bug/RR-20260926-06.md) | 快池内框架阻塞等待入口没有 fail-fast（应 panic） | [修复](bugfix/RR-20260926-06.md) |
| [RR-20260926-07](bug/RR-20260926-07.md) | WAL回放边界漏计 | [修复](bugfix/RR-20260926-07.md) |
| [RR-20260926-08](bug/RR-20260926-08.md) | 续行占用时队列容量和观测分叉 | [修复](bugfix/RR-20260926-08.md) |
| [RR-20260926-09](bug/RR-20260926-09.md) | Remote并行投影缺少失败事务ID | [修复](bugfix/RR-20260926-09.md) |
| [RR-20260926-10](bug/RR-20260926-10.md) | 卸载后投影前重载读旧版本，进程 fence 且重启无法恢复（P1） | [修复](bugfix/RR-20260926-10.md) |
| [RR-20260926-11](bug/RR-20260926-11.md) | Remote 重放被新 fence 拒绝，WAL 投影永久卡住（P1） | [修复](bugfix/RR-20260926-11.md) |
| [RR-20260926-12](bug/RR-20260926-12.md) | 生成工程 syncbus 配置段被忽略，JetStream 静默退回 NATS（P1 建议） | [修复](bugfix/RR-20260926-12.md) |
| [RR-20260926-13](bug/RR-20260926-13.md) | 释放失败跳过 postRemoteCommit，Sync 冻结 | [修复](bugfix/RR-20260926-13.md) |
| [RR-20260926-14](bug/RR-20260926-14.md) | 持久提交后仍 Abort，远端锁外回滚 | [修复](bugfix/RR-20260926-14.md) |
| [RR-20260926-15](bug/RR-20260926-15.md) | 同 SessionID 重开被传输层丢弃 | [修复](bugfix/RR-20260926-15.md) |
| [RR-20260926-16](bug/RR-20260926-16.md) | async checkpoint 先于段 fsync | [修复](bugfix/RR-20260926-16.md) |
| [RR-20260926-17](bug/RR-20260926-17.md) | 停机不等外部 Flush，checkpoint 回退 | [修复](bugfix/RR-20260926-17.md) |
| [RR-20260926-18](bug/RR-20260926-18.md) | OpenRuntime Shutdown 不释放 WAL | [修复](bugfix/RR-20260926-18.md) |
| [RR-20260926-19](bug/RR-20260926-19.md) | 租约跳过后 Remote 事务悬挂 | [修复](bugfix/RR-20260926-19.md) |
| [RR-20260926-20](bug/RR-20260926-20.md) | Memory 级结果未知被回滚 | [修复](bugfix/RR-20260926-20.md) |
| [RR-20260926-21](bug/RR-20260926-21.md) | 正式装配未开并行投影，性能口径待复测 | [修复](bugfix/RR-20260926-21.md) |
| [RR-20260926-22](bug/RR-20260926-22.md) | demo 公会发号 upsert 竞态 | [修复](bugfix/RR-20260926-22.md) |
| [RR-20260926-23](bug/RR-20260926-23.md) | FatalSuffix 测试竞态与 Windows 时钟断言 | [修复](bugfix/RR-20260926-23.md) |
| [RR-20260926-24](bug/RR-20260926-24.md) | 迁移表缺 room 映射 | [修复](bugfix/RR-20260926-24.md) |
| [RR-20260926-25](bug/RR-20260926-25.md) | 生成 sender/demo 不走 Slow，冷实体业务固定失败（P1） | [修复](bugfix/RR-20260926-25.md) |
| [RR-20260926-26](bug/RR-20260926-26.md) | 快阶段冷缺失被改为 panic | [修复](bugfix/RR-20260926-26.md) |
| [RR-20260926-27](bug/RR-20260926-27.md) | 快池删除准入 panic 致进程 fence | [修复](bugfix/RR-20260926-27.md) |
| [RR-20260926-28](bug/RR-20260926-28.md) | Durability 0 未提交结果永无结论 | [修复](bugfix/RR-20260926-28.md) |
| [RR-20260926-29](bug/RR-20260926-29.md) | kit/dataengine 集成测试夹具失效 | [修复](bugfix/RR-20260926-29.md) |
| [RR-20260926-30](bug/RR-20260926-30.md) | 本地 lease fence 跳过后投影 fatal | [修复](bugfix/RR-20260926-30.md)（v1.17.1） |
| [RR-20260926-31](bug/RR-20260926-31.md) | 闲置交还未等投影即释放租约 | [修复](bugfix/RR-20260926-31.md) （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| [RR-20260926-32](bug/RR-20260926-32.md) | 提交后 release hook panic 仍 Abort | [修复](bugfix/RR-20260926-32.md) |
| [RR-20260926-33](bug/RR-20260926-33.md) | WAL fsync 失败后仍信任后续 fsync，checkpoint 越过未落盘数据 | [修复](bugfix/RR-20260926-33.md)（v1.17.1） |
| [RR-20260926-34](bug/RR-20260926-34.md) | 投影在 Mongo 事务内撞键后继续读，真实 Mongo 下投影卡死 | [修复](bugfix/RR-20260926-34.md)（v1.17.1） |
| [RR-20260926-35](bug/RR-20260926-35.md) | handler 内 CreateInScope 不在提交边界内 | [修复](bugfix/RR-20260926-35.md)（v1.17.1） |
| [RR-20260926-36](bug/RR-20260926-36.md) | 冷登录等待投影无超时 | [修复](bugfix/RR-20260926-36.md)（v1.17.1） |
| [RR-20260926-37](bug/RR-20260926-37.md) | Remote strict 确认超时后 Sync 冻结、AfterCommit 丢失 | [修复](bugfix/RR-20260926-37.md)（v1.17.1） |
| [RR-20260926-38](bug/RR-20260926-38.md) | finalizer 与投影器并发发布同一 Remote 事务 | [修复](bugfix/RR-20260926-38.md)（v1.17.1） |
| [RR-20260926-39](bug/RR-20260926-39.md) | 被拒绝隔离的 Remote 实体无重载入口 | [修复](bugfix/RR-20260926-39.md)（v1.17.1） |
| [RR-20260926-40](bug/RR-20260926-40.md) | demo 重连时移出新连接玩家 | [修复](bugfix/RR-20260926-40.md)（v1.17.1） |
| [RR-20260926-41](bug/RR-20260926-41.md) | 零填充尾部拒绝打开 | [修复](bugfix/RR-20260926-41.md)（v1.17.1） |
| [RR-20260926-42](bug/RR-20260926-42.md) | dataengine.shutdown_timeout 不生效 | [修复](bugfix/RR-20260926-42.md)（v1.17.1） |
| [RR-20260926-43](bug/RR-20260926-43.md) | versionedLock 续期重启窗口 | [修复](bugfix/RR-20260926-43.md)（v1.17.1） |
| [RR-20260926-44](bug/RR-20260926-44.md) | Prepare 失败 Abort 多占快池续行 | [修复](bugfix/RR-20260926-44.md)（v1.17.1） |
| [RR-20260926-45](bug/RR-20260926-45.md) | Remote 实体允许 sid 作用域 DAO | [修复](bugfix/RR-20260926-45.md)（v1.17.1） |
| [RR-20260926-46](bug/RR-20260926-46.md) | 已提交但释放失败无已提交哨兵 | [修复](bugfix/RR-20260926-46.md)（v1.17.1） |
| [RR-20260926-47](bug/RR-20260926-47.md) | 准入判定依赖自定义 Getter 契约 | [修复](bugfix/RR-20260926-47.md)（v1.17.1） |
| [RR-20260926-48](bug/RR-20260926-48.md) | P2 事务内新建实体交叉创建永久死锁；memory 模式 Create 不加锁 | [修复](bugfix/RR-20260926-48.md)（v1.17.1） |
| [RR-20260926-49](bug/RR-20260926-49.md) | P2（潜在） 已提交事务错误链含锁超时被重新准入 | [修复](bugfix/RR-20260926-49.md)（v1.17.1） |
| [RR-20260926-50](bug/RR-20260926-50.md) | P3 WAL terminal 后被跳过步骤重复排入驱逐 | [修复](bugfix/RR-20260926-50.md)（v1.17.1） |
| [RR-20260926-51](bug/RR-20260926-51.md) | P3 停机预算缩放反例，默认总时长过短 | [修复](bugfix/RR-20260926-51.md)（v1.17.1） |
| [RR-20260926-52](bug/RR-20260926-52.md) | P3 旧连接持续写失败的重同步循环 | [修复](bugfix/RR-20260926-52.md)（v1.17.1） |
| [RR-20260926-53](bug/RR-20260926-53.md) | P3 本地 strict 已提交后错误无哨兵 | [修复](bugfix/RR-20260926-53.md)（v1.17.1） |
| [RR-20260926-54](bug/RR-20260926-54.md) | P3 共享加载被领头 ctx 截断 | [修复](bugfix/RR-20260926-54.md)（v1.17.1） |
| [RR-20260926-55](bug/RR-20260926-55.md) | P3 快速重连 ErrSubjectRetiring | [修复](bugfix/RR-20260926-55.md)（v1.17.1） |
| [RR-20260926-56](bug/RR-20260926-56.md) | P3 syncbus 流名不随 prefix 隔离 | [修复](bugfix/RR-20260926-56.md)（v1.17.1） |
| [RR-20260926-57](bug/RR-20260926-57.md) | P3 GetOrCreate 瞬时 ErrEntityRemoved；flush 死分支 | [修复](bugfix/RR-20260926-57.md)（v1.17.1） |
| [RR-20260926-58](bug/RR-20260926-58.md) | P3 混合事务 Remote 拒绝丢弃本地实体兴趣事实 | [修复](bugfix/RR-20260926-58.md)（v1.17.1） |
| [RR-20260926-59](bug/RR-20260926-59.md) | P3 拒绝 / 跳过卸载后订阅者停在错误内容 | [修复](bugfix/RR-20260926-59.md)（v1.17.1） |
| [RR-20260926-60](bug/RR-20260926-60.md) | P4 RR-45 装配校验绕过 | [修复](bugfix/RR-20260926-60.md)（v1.17.1） |
| [RR-20260926-61](bug/RR-20260926-61.md) | P3 RR-37 快池拒绝时就地执行业务回调 | [修复](bugfix/RR-20260926-61.md)（v1.17.1） |
| [RR-20260926-62](bug/RR-20260926-62.md) | P3 ErrRemoteFenced 无法区分重载窗口 | [修复](bugfix/RR-20260926-62.md)（v1.17.1） |
| [RR-20260926-63](bug/RR-20260926-63.md) | P3 测试与契约收尾（RR-43 抖动、ack 幂等契约、规范） | [修复](bugfix/RR-20260926-63.md)（v1.17.1） |
| [RR-20260926-64](bug/RR-20260926-64.md) | P2（潜在） memory handler 新建实体冲突不回滚却重排 | [修复](bugfix/RR-20260926-64.md)（v1.17.1） |
| [RR-20260926-65](bug/RR-20260926-65.md) | P3 嵌套独立事务已提交后外层重排 | [修复](bugfix/RR-20260926-65.md)（v1.17.1） |
| [RR-20260926-66](bug/RR-20260926-66.md) | P3 默认停机总时长不足以覆盖真实 game 服务 | [修复](bugfix/RR-20260926-66.md)（v1.17.1） |
| [RR-20260926-67](bug/RR-20260926-67.md) | P3 handler 内先 Destroy 再新建同 ID 未加锁发布 | [修复](bugfix/RR-20260926-67.md)（v1.17.1） |
| [RR-20260926-68](bug/RR-20260926-68.md) | P3 ctx 临近到期的推送关闭健康连接 | [修复](bugfix/RR-20260926-68.md)（v1.17.1） |
| [RR-20260926-69](bug/RR-20260926-69.md) | P3 forget 清除重新登记 subject 的通知器 | [修复](bugfix/RR-20260926-69.md)（v1.17.1） |
| [RR-20260926-70](bug/RR-20260926-70.md) | P3 兜底 remove 后政策不重新订阅 | [修复](bugfix/RR-20260926-70.md)（v1.17.1） |
| [RR-20260926-71](bug/RR-20260926-71.md) | P3 按 builder.RemotePolicy 判定托管的另外两处 | [修复](bugfix/RR-20260926-71.md)（v1.17.1） |
| [RR-20260926-72](bug/RR-20260926-72.md) | P4 卸载退役中 RegisterAfterRetirement 返回值 | [修复](bugfix/RR-20260926-72.md)（v1.17.1） |
| [RR-20260926-73](bug/RR-20260926-73.md) | P3 memory handler Cast 目标摘除后锁超时重排重复修改 | [修复](bugfix/RR-20260926-73.md)（v1.17.1） |
| [RR-20260926-74](bug/RR-20260926-74.md) | P3（潜在） 外层回滚覆盖嵌套独立事务已提交结果 | [修复](bugfix/RR-20260926-74.md)（v1.17.1） |
| [RR-20260926-75](bug/RR-20260926-75.md) | P3（潜在） Remote 消息内嵌套独立事务提交外层批次 | [修复](bugfix/RR-20260926-75.md)（v1.17.1） |
| [RR-20260926-76](bug/RR-20260926-76.md) | P3 嵌套独立事务结果未知不 fence | [修复](bugfix/RR-20260926-76.md)（v1.17.1） |
| [RR-20260926-77](bug/RR-20260926-77.md) | P4 “是否已提交”判别指引与停机文档不准确 | [修复](bugfix/RR-20260926-77.md)（v1.17.1） |
| [RR-20260926-78](bug/RR-20260926-78.md) | P4 二次撤销后 Group / Direct 订阅永久丢失 | [修复](bugfix/RR-20260926-78.md)（v1.17.1） |
| [RR-20260926-79](bug/RR-20260926-79.md) | P4 Direct 绑定表只在 Unbind 时清理 | [修复](bugfix/RR-20260926-79.md)（v1.17.1） |
| [RR-20260926-80](bug/RR-20260926-80.md) | P4 doctor WARN 只看一份配置、减 Mod 后 sync 不一次收敛 | [修复](bugfix/RR-20260926-80.md)（v1.17.1） |
| [RR-20260926-81](bug/RR-20260926-81.md) | P3 新建冲突回滚后同 ID 新建拿到 ErrEntityRemoved | [修复](bugfix/RR-20260926-81.md)（v1.17.1） |
| [RR-20260926-82](bug/RR-20260926-82.md) | P4 dataengine/engine 包测试不能重复运行 | [修复](bugfix/RR-20260926-82.md)（v1.17.1） |
| [RR-20260926-83](bug/RR-20260926-83.md) | P4 entity 包测试不能重复运行 | [修复](bugfix/RR-20260926-83.md)（v1.17.1） |
| [RR-20260926-84](bug/RR-20260926-84.md) | P3（潜在） 收尾阶段 RunIsolatedTransaction 被当成消息自己的事务 | [修复](bugfix/RR-20260926-84.md)（v1.17.1） |
| [RR-20260926-85](bug/RR-20260926-85.md) | P4 RR-79 释放通知删掉重开后被撤销的 Direct 绑定 | [修复](bugfix/RR-20260926-85.md)（v1.17.1） |
| [RR-20260927-01](bug/RR-20260927-01.md) | P4 Windows 上 RR-80 写失败用例注入不生效（CI 红） | [修复](bugfix/RR-20260927-01.md)（v1.17.2） |
| [RR-20260927-02](bug/RR-20260927-02.md) | P4 生成的 TCP `CloseSessions` 恒为 0 | [修复](bugfix/RR-20260927-02.md)（v1.17.2） |
| [RR-20260927-03](bug/RR-20260927-03.md) | P4 demo run.sh 不传 redis.db / password，玩家 id 计数键不带前缀 | [修复](bugfix/RR-20260927-03.md)（v1.17.2） |
| [RR-20260927-04](bug/RR-20260927-04.md) | P4 doctor 非正时长判定与运行时不一致、建议值口径 | [修复](bugfix/RR-20260927-04.md)（v1.17.2） |
| [RR-20260927-05](bug/RR-20260927-05.md) | P3 player TCP Mod 不声明停机预算 | [修复](bugfix/RR-20260927-05.md)（v1.17.2） |
| [RR-20260927-06](bug/RR-20260927-06.md) | P3 fence 之后外层自己的事务仍交给 committer | [修复](bugfix/RR-20260927-06.md)（v1.17.2） |
| [RR-20260927-07](bug/RR-20260927-07.md) | P3 嵌套独立事务裸 mutation 绕过 RR-74 | [修复](bugfix/RR-20260927-07.md)（v1.17.2） |
| [RR-20260927-09](bug/RR-20260927-09.md) | P3 Remote 托管 + sid 作用域 DAO 提交路径不拒绝 | [修复](bugfix/RR-20260927-09.md)（v1.17.2） |
| [RR-20260927-10](bug/RR-20260927-10.md) | P4 `RegisterEntityKindDefs` 出错留半批 | [修复](bugfix/RR-20260927-10.md)（v1.17.2） |
| [RR-20260927-11](bug/RR-20260927-11.md) | P3 `CreateInScope` 捕获失败被吞掉仍提交 | [修复](bugfix/RR-20260927-11.md)（v1.17.2） |
| [RR-20260927-12](bug/RR-20260927-12.md) | P4 撤销新建实体用 `DestroyReasonCommon` | [修复](bugfix/RR-20260927-12.md)（v1.17.2） |
| [RR-20260927-13](bug/RR-20260927-13.md) | P4 卸载后重载 / 共享加载超时参数没接 kit 配置 | [修复](bugfix/RR-20260927-13.md)（v1.17.2） |
| [RR-20260927-14](bug/RR-20260927-14.md) | P4 卸载后重载最坏延迟文档不实 | [修复](bugfix/RR-20260927-14.md)（v1.17.2） |
| [RR-20260927-15](bug/RR-20260927-15.md) | P3 同 fence 下 Remote 版本向量可回退 | [修复](bugfix/RR-20260927-15.md)（v1.17.2） |
| [RR-20260927-16](bug/RR-20260927-16.md) | P4 saga 收件箱 `markCompleted` 失败静默 | [修复](bugfix/RR-20260927-16.md)（v1.17.2） |
| [RR-20260927-17](bug/RR-20260927-17.md) | P3 Remote L2 快照键不带部署前缀 | [修复](bugfix/RR-20260927-17.md)（v1.17.2） |
| [RR-20260927-18](bug/RR-20260927-18.md) | P3 demo 场景 Manager 未接卸载后重载 | [修复](bugfix/RR-20260927-18.md)（v1.17.2） |
| [RR-20260927-19](bug/RR-20260927-19.md) | P4 场景重开复制会话失败无计数 | [修复](bugfix/RR-20260927-19.md)（v1.17.2） |
| [RR-20260927-20](bug/RR-20260927-20.md) | P4 nest 单用例 `-count>1` panic duplicate handler | [修复](bugfix/RR-20260927-20.md)（v1.17.2） |
| [RR-20260927-21](bug/RR-20260927-21.md) | P4 同 Guard 自我撤销后再建同 ID 空转重排 | [修复](bugfix/RR-20260927-21.md)（v1.17.2） |
| [RR-20260927-22](bug/RR-20260927-22.md) | P4 `Unregister` 作用在已 forget 的旧 subject 上 | [修复](bugfix/RR-20260927-22.md)（v1.17.2） |
| [RR-20260927-23](bug/RR-20260927-23.md) | P4 demo 场景未接 `OnEntityLoaded → Rebind` | [修复](bugfix/RR-20260927-23.md)（v1.17.2） |
| [RR-20260927-24](bug/RR-20260927-24.md) | P4 strict 截止回复缺 `ErrRemotePersistenceIndeterminate` | [修复](bugfix/RR-20260927-24.md)（v1.17.2） |
| [RR-20260927-25](bug/RR-20260927-25.md) | P4 不可比较的自定义 Mutex 比较 panic | [修复](bugfix/RR-20260927-25.md)（v1.17.2） |
| [RR-20260927-26](bug/RR-20260927-26.md) | P3 `ReleaseCast(旧实例)` 放掉同 ID 新实例的锁 | [修复](bugfix/RR-20260927-26.md)（v1.17.2） |
| [RR-20260927-27](bug/RR-20260927-27.md) | P4 非 Nest 持锁领头方冷加载死锁 | [修复](bugfix/RR-20260927-27.md)（v1.17.2） |
| [RR-20260927-28](bug/RR-20260927-28.md) | P4 `RetractSyncSubject` 锁外比对后误注销新登记 | [修复](bugfix/RR-20260927-28.md)（v1.17.2） |
| [RR-20260927-29](bug/RR-20260927-29.md) | P4 领头方正常完成时不关 `leaderAway`，迟到 `RunLocal` 永久阻塞 | [修复](bugfix/RR-20260927-29.md)（v1.17.2） |
| [RR-20260927-30](bug/RR-20260927-30.md) | P4 含装着 func 的接口字段的 Mutex 仍 panic | [修复](bugfix/RR-20260927-30.md)（v1.17.2） |
| [RR-20260927-31](bug/RR-20260927-31.md) | P3 Cast 捕获失败被吞后事务照常提交 | [修复](bugfix/RR-20260927-31.md)（v1.17.2） |
| [RR-20260927-32](bug/RR-20260927-32.md) | P4 提交前拒绝不带 `ErrCommitRejected`，判别表表述 | [修复](bugfix/RR-20260927-32.md)（v1.17.2） |
| [RR-20260927-33](bug/RR-20260927-33.md) | P2 生成 compose 的 `tmpfs` 被逗号拆开，服务起不来 | [修复](bugfix/RR-20260927-33.md)（v1.17.2） |
| [RR-20260927-34](bug/RR-20260927-34.md) | P2 生产镜像缺 `configs/data`，容器启动即失败 | [修复](bugfix/RR-20260927-34.md)（v1.17.2） |
| [RR-20260927-35](bug/RR-20260927-35.md) | P4 demo 模板测试期望流名未含 `roost.room` 兼容映射 | [修复](bugfix/RR-20260927-35.md)（v1.17.2） |
| [RR-20260928-01](bug/RR-20260928-01.md) | P4 多 ManagerAccess 下积压 gauge 互相覆盖 | [修复](bugfix/RR-20260928-01.md)（v1.17.2） |
| [RR-20260928-02](bug/RR-20260928-02.md) | P4 Guard 跨 Manager 同 ID 时 RR-21 误判 | [修复](bugfix/RR-20260928-02.md)（v1.17.2） |
| [RR-20260928-03](bug/RR-20260928-03.md) | P3 本地已提交、Remote 明确拒绝时回复无哨兵 | [修复](bugfix/RR-20260928-03.md)（v1.17.2） |
| [RR-20260928-04](bug/RR-20260928-04.md) | P3 stats_log 在容器 / systemd 里不落盘也不告警 | [修复](bugfix/RR-20260928-04.md)（v1.17.2） |
| [RR-20260928-05](bug/RR-20260928-05.md) | P2 shell / systemd 安装缺 `configs/data` | [修复](bugfix/RR-20260928-05.md)（v1.17.2） |
| [RR-20260928-06](bug/RR-20260928-06.md) | P3 game-demo 生产示例缺 `game_route` / `activity` / `platform` | [修复](bugfix/RR-20260928-06.md)（v1.17.2） |
| [RR-20260928-07](bug/RR-20260928-07.md) | P3 k8s Secret 示例缺 `saga` / `player_access` | [修复](bugfix/RR-20260928-07.md)（v1.17.2） |
| [RR-20260928-08](bug/RR-20260928-08.md) | P4 strict 下 tracker 淘汰后 Overloaded 被误标 `ErrRemotePartRejected` | [修复](bugfix/RR-20260928-08.md)（v1.17.2） |
| [RR-20260928-09](bug/RR-20260928-09.md) | P2 pipelined + Remote 明确拒绝不回滚、不卸载、Sync 门冻结 | [修复](bugfix/RR-20260928-09.md)（v1.17.2） |
| [RR-20260928-10](bug/RR-20260928-10.md) | P3 install.sh 升级失败自动回滚 / rollback.sh 回不到旧 release | [修复](bugfix/RR-20260928-10.md)（v1.17.2） |
| [RR-20260928-11](bug/RR-20260928-11.md) | P2 pipelined 回退 strict 路径时 `WAL.Append` 不等 fsync | [修复](bugfix/RR-20260928-11.md)（v1.17.2） |
| [RR-20260928-12](bug/RR-20260928-12.md) | P4 回滚时新进程按旧 unit 的 TimeoutStopSec 被停 | [修复](bugfix/RR-20260928-12.md)（v1.17.2） |
| [RR-20260928-13](bug/RR-20260928-13.md) | P4 CRLF Secret 示例被静默跳过 | [修复](bugfix/RR-20260928-13.md)（v1.17.2） |
| [RR-20260928-14](bug/RR-20260928-14.md) | P3 `codegen/internal/roost` 在 Windows CI 超时 | [修复](bugfix/RR-20260928-14.md)（v1.17.2） |
| [RR-20260928-15](bug/RR-20260928-15.md) | P4 生成 TCP / scene 测试未等会话登记 | [修复](bugfix/RR-20260928-15.md)（v1.18.0） |
| [RR-20260930-03](bug/RR-20260930-03.md) | P2 `AtomicLocalStore` 覆盖写无限追加时钟记录（C01 堆增长） | [修复](bugfix/RR-20260930-03.md)（v1.18.0） |
| [RR-20260930-12](bug/RR-20260930-12.md) | P2 fence 后无 effect 的 memory handler 仍以 Durability 0 直写 Remote（N21） | [修复](bugfix/RR-20260930-12.md)（未发布；行为收紧） |
| [RR-20260930-13](bug/RR-20260930-13.md) | P3 `SetEntityVersion` 同 fence 可回退 StateVersion（N26） | [修复](bugfix/RR-20260930-13.md)（未发布；接口签名改为返回 error） |
| [RR-20260930-14](bug/RR-20260930-14.md) | P3 广播路径按 ID 释放、一把锁跨后续目标（N28） | [修复](bugfix/RR-20260930-14.md)（未发布；每目标自己的 Guard 作用域） |
| [RR-20260930-15](bug/RR-20260930-15.md) | P3 值类型实体实现在 `holding` 比较时 panic（N29） | [修复](bugfix/RR-20260930-15.md)（未发布；契约 + 入口校验） |
| [RR-20261004-06](bug/RR-20261004-06.md) | P2 etcd Campaign 在 session 建好后的失败 / 取消分支不再撤…（NC 复审） | [修复](bugfix/RR-20261004-06.md)（未发布；NC-11 回退修复；T-208） |
| [RR-20260921-05](bug/RR-20260921-05.md) | P2 本仓 go:generate 产物与 codegen 运行期守卫无 CI 校验（09-21 登记、10-04 核实） | [修复](bugfix/RR-20260921-05.md)（未发布） |
| [RR-20260921-03](bug/RR-20260921-03.md) | P1 playerowner 归还中被重新 Claim，`mine=true` 而共享表无主（09-21 登记、10-04 核实） | [修复](bugfix/RR-20260921-03.md)（未发布） （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| [RR-20260921-04](bug/RR-20260921-04.md) | P2 归还批次占住刷新循环 45s > Lease（09-21 登记、10-04 核实） | [修复](bugfix/RR-20260921-04.md)（未发布） （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| [RR-20261004-10](bug/RR-20261004-10.md) | P2 playerowner 重新认领无回合预算、续租确认排在等待之后（W-03） | [修复](bugfix/RR-20261004-10.md)（未发布；已生成工程手工合并） （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| [RR-20261004-11](bug/RR-20261004-11.md) | P3 撤离进行中 Admit 照常放行（W-04） | [修复](bugfix/RR-20261004-11.md)（未发布；已生成工程手工合并） （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| [RR-20261004-14](bug/RR-20261004-14.md) | P3 playerowner 窗口起算时刻与认领丢回复（W-07） | [修复](bugfix/RR-20261004-14.md)（未发布；已生成工程手工合并） （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| [RR-20261004-13](bug/RR-20261004-13.md) | P3 go 子命令超时不杀孙进程 / Wait 被管道拖住（W-06） | [修复](bugfix/RR-20261004-13.md)（未发布；Windows 未实跑） |
| [RR-20261004-12](bug/RR-20261004-12.md) | P3 生成器 chdir 整个进程（W-2026-10-04-05） | [修复](bugfix/RR-20261004-12.md)（未发布；生成物不变） |
| [RR-20261004-09](bug/RR-20261004-09.md) | P2 RefHMap 注册表 guard 误报冲突（NC-30 回归，v1.19.1） | [修复](bugfix/RR-20261004-09.md)（未发布；v1.19.1 回归修复） |
| [RR-20261004-08](bug/RR-20261004-08.md) | P3 NatsMod 连接 drain 失败后重试永不收敛（W-2026-10-04-02） | [修复](bugfix/RR-20261004-08.md)（未发布；连接层“ctx 错误保留、终态释放”，延伸自 RR-07） |
| [RR-20261004-07](bug/RR-20261004-07.md) | P3 NatsMod 停止时 Bus 超过预算后保留 bus / asm 以便重试，但…（NC 复审） | [修复](bugfix/RR-20261004-07.md)（未发布；含复审 S1 / S2 / S4） |
| [RR-20261004-02](bug/RR-20261004-02.md) | P2 `LayeredStore` 的 L1 过期后（或 ttl≤0 时）仍永久否决权威值；远端写已生效却报 `ErrStaleWrite`（NC 复审） | [修复](bugfix/RR-20261004-02.md)（未发布；Layered 窗口外交付权威值） |
| [RR-20261004-03](bug/RR-20261004-03.md) | P2 RefHMap Patch 只续期根到叶路径上的 hash，兄弟 hash 过期后 `Get` 返回部分记录且 `ok=true`（NC 复审） | [修复](bugfix/RR-20261004-03.md)（未发布；Patch KEYS 变为整条记录；Get 部分记录改报 miss） |
| [RR-20261004-04](bug/RR-20261004-04.md) | P3 `ReadThroughStore` 的 loader 回填被 L1 以 stale 拒绝时，`Get` 返回 `ErrStaleWrite`（读取因写被拒而失败）（NC 复审） | [修复](bugfix/RR-20261004-04.md)（未发布） |
| [RR-20261004-05](bug/RR-20261004-05.md) | P3 mongotest 忽略 `IndexModel.Sparse`，非 sparse 唯一索引把缺字段跳过，而真实 Mongo 当作 null（替身比真实宽松）（NC 复审） | [修复](bugfix/RR-20261004-05.md)（未发布；替身行为收紧） |
| [RR-20261004-01](bug/RR-20261004-01.md) | P2 TryLock 取锁结果未知不记 token，实体卡到 LockTTL | [修复](bugfix/RR-20261004-01.md)（未发布；token 格式 `<base32>.<seq>`，TryLock 脚本 ARGV 3 → 4；T-207） |
| [RR-20261001-06](bug/RR-20261001-06.md) | P2 account pending slot 无释放入口（W-01 拍板） | [修复](bugfix/RR-20261001-06.md)（未发布；新码 560115 / 560116、`Admin.ResolvePendingCreation`、committed 自动释放、T-182）；10-05 复核残余补修：换名也释放死计划，[记录](bugfix/RR-20261001-06.md#复核后的补修2026-10-05)，未发版 |
| [RR-20261001-07](bug/RR-20261001-07.md) | P3 playerowner 取回后 stale 副本被 Refresh 续租（W-02 拍板） | [修复](bugfix/RR-20261001-07.md)（未发布；已生成工程手工合并） （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| [RR-20261001-08](bug/RR-20261001-08.md) | P3 chat 最新页 `Gap=true` 误报（W-03 拍板） | [修复](bugfix/RR-20261001-08.md)（未发布；wire 语义收紧，无 API 变化） |
| [RR-20261001-09](bug/RR-20261001-09.md) | P3 activity legacy Opening / 坏 Intent（W-04 拍板） | [修复](bugfix/RR-20261001-09.md)（未发布；T-181） |
| [RR-20261001-05](bug/RR-20261001-05.md) | P2 activity pending 名额在 ledger TTL 后永不回收（复审 B 线） | [修复](bugfix/RR-20261001-05.md)（未发布；新码 620119、`Admin.ReconcileProgress`、T-180） |
| [RR-20261001-02](bug/RR-20261001-02.md) | P3 mail `sameSendIntent` nil / 空切片判不同（复审 B 线） | [修复](bugfix/RR-20261001-02.md)（未发布） |
| [RR-20261001-03](bug/RR-20261001-03.md) | P2 空 CSV 目录也跑 tablegen，新工程生成链路失败（复审 B 线，回归） | [修复](bugfix/RR-20261001-03.md)（未发布） |
| [RR-20261001-04](bug/RR-20261001-04.md) | P3 v1 manifest + 手写 JSON 无迁移说明（复审 B 线） | [修复](bugfix/RR-20261001-04.md)（未发布；错误文本 + 文档） |
| [RR-20261001-01](bug/RR-20261001-01.md) | P2 ci Redis job 漏设四个门变量，service Redis 变体静默不跑 | [修复](bugfix/RR-20261001-01.md)（未发布） |
| [RR-20260930-22](bug/RR-20260930-22.md) | P2 game-demo gift 收件人检查读错库（B27 第 3 批暴露） | [修复](bugfix/RR-20260930-22.md)（未发布） |
| [RR-20260930-23](bug/RR-20260930-23.md) | P2 续租中断重取后在线玩家被 Destroy、脱离场景（B27 第 3 批暴露） | [修复](bugfix/RR-20260930-23.md)（未发布；T-179，已生成工程手工合并） （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| [RR-20260930-24](bug/RR-20260930-24.md) | P3 活动租约丢失后永不重取（B27 第 3 批暴露） | [修复](bugfix/RR-20260930-24.md)（未发布） （2026-10-05 所在代码已被静态绑定 / App 单实例锁取代，见 [APP-SINGLETON-LOCK](feature/APP-SINGLETON-LOCK-2026-10-05.md)） |
| [RR-20260930-20](bug/RR-20260930-20.md) | P2 回滚后 release hook panic 吞掉业务错误（B27 第 2 批暴露） | [修复](bugfix/RR-20260930-20.md)（未发布；`nest/nest_dispatch.go` `joinRecoveredError`） |
| [RR-20260930-21](bug/RR-20260930-21.md) | P2 解锁失败后本地 `acquired` 不清，实体本进程内永久不可写（B27 第 2 批暴露） | [修复](bugfix/RR-20260930-21.md)（未发布；释放失败 → 持有状态未知，`TryLock` 以 Redis 为准；T-178） |
| [RR-20260930-16](bug/RR-20260930-16.md) | P4 CRLF 配置追加 Mod 段用 LF（N23） | [修复](bugfix/RR-20260930-16.md)（未发布） |
| [RR-20260930-17](bug/RR-20260930-17.md) | P3 生成工程 CI 抓不到 compose 语义错误（N24） | [修复](bugfix/RR-20260930-17.md)（未发布；生成工程自带结构检查） |
| [RR-20260930-18](bug/RR-20260930-18.md) | P3 game-demo `Scene.Close` 不受停机时限约束（N31） | [修复](bugfix/RR-20260930-18.md)（未发布） |
| [RR-20260930-19](bug/RR-20260930-19.md) | P3 Remote 标记键无部署前缀、锁键隔离要求未写（N25） | [修复](bugfix/RR-20260930-19.md)（未发布；缺省键逐字不变、kit 配置面不变） |
| [RR-20260930-11](bug/RR-20260930-11.md) | P3 `cache` 包 `TestReadThroughStoreCoalescesMisses` 偶发（测试只钉住领头者） | [修复](bugfix/RR-20260930-11.md)（只改测试，v1.18.0） |
| [RR-20260930-CG-12](bug/REVIEW-2026-09-30-codegen-05.md#rr-20260930-12) | P2 cfggen false 索引生成无用导入 | [修复/真实消费回归](bugfix/RR-20260930-CG-12.md)（未发版） |
| [RR-20260930-CG-13](bug/REVIEW-2026-09-30-codegen-05.md#rr-20260930-13) | P2 cfggen bean 与函数/import 名冲突 | [修复/写入前拒绝](bugfix/RR-20260930-CG-13.md)（未发版） |
| [RR-20260930-CG-14](bug/REVIEW-2026-09-30-codegen-05.md#rr-20260930-14) | P2 deps 事务遗漏合仓迁移文件回写 | [修复/隔离与正式 CLI 消费](bugfix/RR-20260930-CG-14.md)（未发版） |
