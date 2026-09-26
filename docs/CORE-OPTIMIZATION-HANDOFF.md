# Roost 核心优化汇总与 agent 交接

**提交更新（2026-09-26）**：用户已授权提交，RR-03～24 的修复、回归与文档随本次 main 提交保存；未执行 push 或发布。下文“未提交”和索引刷新失败是修复验收时的记录。

**最新修复（2026-09-26）**：RR-10～24 见[新增复审修复](review/REVIEW-2026-09-26-release-fixes.md)。当前工作树未提交；旧性能结论不能代表 Backend 正式装配，新的 Remote + lease-fence 混合准入明确拒绝，同 SessionID 重连遇到旧队列未退出需重试。下文原批次结论需结合此更新阅读。

更新日期：2026-09-26。范围是本轮对话已实施的 Nest、Sync、DataEngine、Remote 优化与必要调用链，并回链更早的结构调整；不是全仓逐函数审计或永久性能保证。

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
| 09-24 整体验收 N1～N4 | 注册/dispatch/事务/回滚/诊断同包分文件；Single/Multi/MultiGroup统一收尾；准入、释放与回复责任显式化；可选阶段指标；profile驱动Timer/临时集合优化 | [Nest整体验收](feature/NEST-COMPLETION-2026-09-24.md)、[消息吞吐](feature/NEST-MSG-THROUGHPUT-2026-09-24.md) |
| 事务正确性 | pipelined锁内准入、Cast事务保护、组锁/panic/广播清理、Ticker启停、已准入后的持久完成责任、真实释放后回调 | [早期主线](feature/REFACTOR-2026-09-24-nest-and-immediate-sync.md)、RR-20260924-01～08 |
| Remote分段 | 慢准备→快业务→慢确认/释放，避免远端I/O长期占逻辑worker；内部续行保留收尾能力 | [阶段隔离](feature/REFACTOR-2026-09-25-nest-remote-stages.md) |
| 快慢双池 | main/hb/remote等执行资源归为双池；全部显式ID统一依赖顺序；无关ID共享worker；有界等待、内部续行公平与停机排空 | [双池](feature/REFACTOR-2026-09-25-nest-fast-slow.md)、RR-20260925-07～09 |
| 09-26 九项 N1～N3 | 快阶段Getter禁止冷加载；Slow准备声明目标；队列区分前驱与worker等待、峰值/年龄/拒绝和续行；指标key和单ID减分配 | [九项实施](feature/REFACTOR-2026-09-26-core-nine-items.md)、RR-20260926-02 |

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

以下均为本机 Go1.27.0、Apple M5 的限定负载，不是生产跨机SLA。参数、命令和原始产物路径见[九项报告](feature/REFACTOR-2026-09-26-core-nine-items.md)。可随仓库携带的脱敏汇总、完整28条Sync outlier及结果文件SHA256见[证据JSON](feature/CORE-OPTIMIZATION-2026-09-26-evidence.json)；本地大日志/profile不入Git，也不保证其他checkout存在。

| 验收 | 结果及限制 |
| --- | --- |
| 功能 | 相关Entity/Nest/NestWAL/DataEngine/Sync/Kit/Metrics race、vet、glsvet通过；正式生成三进程持久化、Sync双模式、Remote三策略通过。首次NATS环境失败保留，恢复环境后通过 |
| Sync正常AOI | 1%变化p99 2.350/2.401ms，5% 4.110/5.570ms；4次测试两种超50ms计数都为0 |
| Sync集中恢复 | 8次中2次原门禁失败：1000预算/5%第1次，442050样本，计划14条/实际变化1条超标，实际max50.058ms；2000预算/1%第2次，102441样本，计划14条/实际5条，实际max50.523ms |
| Sync用户决定 | **2026-09-26用户明确表示“这个指标目前可以接受”，允许本轮交付。** 6条实际超标包含在28条计划超标里，不能相加。第一组全为delta，第二组全为恢复create；没有省略outlier。原门禁继续失败，不将接受外推到未来退化或所有对象首次可见 |
| 混合正式链路 | 同Nest、真实WAL/Mongo，持久80TPS+AOI1000/10000/50；1%/5%已有对象变化到进程内解码p99 6.142/14.715ms，最终DAO/版本/WAL/同步/AOI验证通过。不是TCP延迟 |
| Remote30分钟 | 144000成功，79.996TPS，0错误/丢弃，p99 582ms；最终WAL/投影失败0、Mongo/NATS/outbox全量核验通过，未出现SyncTimeout |
| Remote短时容量 | 120/160输入TPS均通过，160档成功159.581TPS、p99399ms；240档839次写许可拒绝，不能把成功224.476TPS视为可用容量；320未运行。边界在已测160通过与240过载之间，不是精确极限 |
| 本地持久负载 | 双实体双DAO每DAO32KiB：939.01TPS；50%初始冷：645.76TPS；两个热点实体：138.39TPS；突发目标500TPS：522.66TPS（短样本整批投放，不是容量上限） |
| Nest profile | 单实体51–55→41–45 alloc，多实体61→48–52 alloc；完整请求耗时增加约2–4%。分配下降不等于吞吐提高；mutex证据不支持拆全局调度锁 |

计划延迟从计划事件时间起算；实际变化延迟从Entity修改起算，都到客户端完成该数据解码。样本是“会话×实体的一次变化交付”，不是独立实体数或帧数。冷对象旧状态不冒充新变化，首次可见/全量恢复另测。集中恢复的冷对象可能等待数秒，不能以已接受的50.523ms推断全部重连基线在50ms内到齐。

## 5. 明确没有实施、没有证明或已撤回的事项

- **已撤回**：独立10ms快照预算窗口试验无稳定收益，最终无 `SnapshotBudget.Interval` / `-snapshot-window` API；不要据旧方案补回。也没有按负载ID特判、跳过冻结或拼接opaque delta。
- **独立后续需求**：共享帧协议+网关多播需要网关/客户端配套；并行Flush、空间分片、每Profile独立版本链当前没有收益证据，不当作漏掉的收尾。
- **24小时未运行**：保留入口。30分钟堆86.94–307.58MB且后半程基线较高，事务保留接近65536上限；不能宣称排除了泄漏。缺少24小时结果不撤销用户本次接受，也不能写成长期已验证。
- **故障矩阵有版本边界**：历史Remote版本有21/21通过，最新九项版本未完整重跑；其正式生成与相关race通过不等同新的完整集群矩阵。真实跨机/弱网、生产客户端与业务schema仍需部署环境验证。
- **历史FlushFailures根因未证明**：RR-20260926-01修复阶段错误留存；本轮没有复现历史偶发FlushFailures，不将观测修复说成历史根因修复。
- **2026-09-26 复审核实并修复**：RR-03～06 四条成立；另确认 RR-07（WAL回放漏计）、RR-08（续行占用导致准入/指标分叉）、RR-09（Remote失败缺事务ID）。代码与防回归规则已补齐，race、静态检查及正式生成三链路通过，尚未提交/部署；[逐项判断、负对照和边界](review/REVIEW-2026-09-26-followup.md)。Projected是成功尝试数；Close/Open不等同Hold恢复；periodic仅字节预算的预捕获成本仍存在。此前性能数字未重测。
- **2026-09-26 上线前复审登记、未修复**（基线 `aaada47`，[复审记录](review/REVIEW-2026-09-26-release.md)）：P1 级 [RR-20260926-10](bug/RR-20260926-10.md)（卸载后投影前重载 → fence 且重启不起来）、[RR-20260926-11](bug/RR-20260926-11.md)（Remote 重放被新 fence 拒绝，投影卡死）、[RR-20260926-12](bug/RR-20260926-12.md)（生成工程 syncbus 配置失效）；另 P2 十二条（RR-13～24）。**§4 的 Remote 30分钟80TPS、120/160TPS 容量与 RR-20260925-05 的 59.997TPS 由直接接 MongoCommitter 的测试装配测得，正式 kit 装配下并行投影未开启（[RR-20260926-21](bug/RR-20260926-21.md)），在正式装配复测前不作为上线依据。** 发版手续（清单 v1.16.1、生成器下限、CHANGELOG）与 CI 状态见复审记录。
- **图谱尚未刷新**：最后generation `2026-09-25T11:41:37Z`。刷新被“pre-coordination or unverified CBM generation is active”阻止；新代码以源码/实际测试补证。环境恢复后重建并检查coverage；不要清未知锁或中断其他实例。

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

以下索引覆盖此次核心优化阶段2026-09-23～26的RR记录；保留独立问题与修复文件，修复记录内有原始复现、验证和限制，不把索引等同于本轮逐项复验。更早记录继续查 [bug索引](bug/README.md)、[bugfix索引](bugfix/README.md) 和 [review索引](review/README.md)。

| 问题 | 主题 | 修复记录 |
| --- | --- | --- |
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
| [RR-20260926-10](bug/RR-20260926-10.md) | 卸载后投影前重载读旧版本，进程 fence 且重启无法恢复（P1） | 未修复 |
| [RR-20260926-11](bug/RR-20260926-11.md) | Remote 重放被新 fence 拒绝，WAL 投影永久卡住（P1） | 未修复 |
| [RR-20260926-12](bug/RR-20260926-12.md) | 生成工程 syncbus 配置段被忽略，JetStream 静默退回 NATS（P1 建议） | 未修复 |
| [RR-20260926-13](bug/RR-20260926-13.md) | 释放失败跳过 postRemoteCommit，Sync 冻结 | 未修复 |
| [RR-20260926-14](bug/RR-20260926-14.md) | 持久提交后仍 Abort，远端锁外回滚 | 未修复 |
| [RR-20260926-15](bug/RR-20260926-15.md) | 同 SessionID 重开被传输层丢弃 | 未修复 |
| [RR-20260926-16](bug/RR-20260926-16.md) | async checkpoint 先于段 fsync | 未修复 |
| [RR-20260926-17](bug/RR-20260926-17.md) | 停机不等外部 Flush，checkpoint 回退 | 未修复 |
| [RR-20260926-18](bug/RR-20260926-18.md) | OpenRuntime Shutdown 不释放 WAL | 未修复 |
| [RR-20260926-19](bug/RR-20260926-19.md) | 租约跳过后 Remote 事务悬挂 | 未修复 |
| [RR-20260926-20](bug/RR-20260926-20.md) | Memory 级结果未知被回滚 | 未修复 |
| [RR-20260926-21](bug/RR-20260926-21.md) | 正式装配未开并行投影，性能口径待复测 | 未修复 |
| [RR-20260926-22](bug/RR-20260926-22.md) | demo 公会发号 upsert 竞态 | 未修复 |
| [RR-20260926-23](bug/RR-20260926-23.md) | FatalSuffix 测试竞态与 Windows 时钟断言 | 未修复 |
| [RR-20260926-24](bug/RR-20260926-24.md) | 迁移表缺 room 映射 | 未修复 |
