# Roost Review 跨轮进度

## 2026-10-05 N10 第一批（ai / actionflow / featureflag / hotcode）

基线 `197f7bb9`，分支 `revn10`，NC 段 120～129（用 120～123）；图谱 generation 09-30，四包此后无提交，模板与 demo 用法以源码补证。[本轮/矩阵](REVIEW-2026-10-05-noncore-n10.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-n10/README.md)。NC-120 / 121（P2）、NC-122 / 123（P3）已修复、声明场景验证，未发版。

| 子域 | 场景 | 状态 / 下一入口 |
| --- | --- | --- |
| actionflow | A1～A6 替换 / 入队 / 重入 / 丢弃 / 冻结，M1～M4 任务推进 / 取消 / EndAll（最小接线探针） | NC-121、NC-122；O-A1～O-A4 观察；第二批：UpdateAction、Context 池化存储 |
| ai | T1～T7 事务式替换、冻结、Init/Shutdown 收尾、中断传播、SetMission 注释、解析 | NC-120；O-T3 / O-T6 补注释；第二批：nodes 逐节点红线 |
| featureflag | F1～F5；运行期热更复用 N07b H0～H4（不重做） | 无缺陷；O-F1 观察 |
| hotcode | H1～H8 可见性、旧请求生命周期（Nest 每次派发解析一次）、并发替换、插件部分应用 | NC-123；O-H1～O-H4；真实 .so 加载未跑 |

7 条新正式用例修前红 → 修后绿；四包 race×3、`./nest` race、全仓 build/vet、根包、全新生成 game-demo build/vet 与相关包测试通过。ai / actionflow 无生产使用方。N10 第一批完成、场景部分完成，不计 completed/15。方向判断：actionflow 回调重入靠事后比对 `unit.cur`，NC-122 是 U-0100 同机制的漏网分支，建议结构上禁止重入或改延后命令队列（或移出 core 待真实使用方）。不等待 CI，不发版。

## 2026-10-05 N09 skill 第二批（同步/接入 + 执行/状态余项）

基线 `855c2a38`（origin/main），分支 `revn09b`，NC 段 114～119（用 114～117）；图谱 generation 2026-09-30，早于第一批修复，按当前源码逐行补证。[本轮/矩阵](REVIEW-2026-10-05-n09-batch2.md) · [修复](../bugfix/RR-20261005-NC-114.md)。NC-114/115/117（P2）、NC-116（P3）已修复、声明场景验证，未发版。

| 方向 | 本批 | 状态 / 下一入口 |
| --- | --- | --- |
| 同步/接入 | Y1～Y5：Coordinator、outbox、文件 outbox（unix / windows）、Applier、可见性三条下发路径 | NC-114 reset 不过滤、NC-115 快照 / 增量口径不一、NC-116 Applier 卡死；O5 全局超龄停发、O6 tmp 残留、O8～O10、O12 可见性变化无重发入口 |
| 执行/状态余项 | Y6～Y10：增量 mutation / baseline、checkpoint、NC-110 后终止路径与 checkpoint 一致性、owned 进程、回放 | NC-117 提交前失败 cast 无界保留、checkpoint 恢复不了；O7 checkpoint 字节不确定、O11 RecordingHost |
| 数据/属性（107） | 未审（只读 Parse 入口与生成的 CompileAll） | 第三批：Parse + Compile 拒绝路径、skillcompose；process_motion / area / numeric |

8 条新正式用例：7 条修前红 → 修后绿（skill 2、skillsync 5）+ 1 条控制；skill 5 包 race×3、examples / sync-e2e、build/vet、根包通过，不累计作覆盖率。N09 部分完成，不计 completed/15。方向判断：“同一事实多条路径各写一套规则”在两批反复出现（终止路径 → 保留集合；快照 / 增量 / reset 的可见性），建议终态登记与可见性规则各收敛到唯一入口，本批按此做最小收敛。O1～O4 未找到新触发路径。不等待 CI，不发版。

## 2026-10-05 N04 接续（revn04）

起点 `be7bcc18`，独立 worktree 分支 `revn04`；图谱 generation 2026-09-30、以当前源码补证。[本轮](REVIEW-2026-10-05-n04-revn04.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-revn04/README.md)。NC-100 P1、NC-101 P2、NC-102 P3 已修复、声明场景验证，未发版。

| 项 | 本轮 | 状态 / 边界 |
| --- | --- | --- |
| versionstore NC-52 复核 | Memory / Redis 两实现与 rank、activity、RedisDispatches 等 CAS 循环逐一对照 | 一致，无同形态；真实 Redis 丢回复暴露 NC-100（与 NC-52 无因果） |
| Redis | 丢回复（自建 toxiproxy 代理）、RefHMap Patch TTL 多 hash、私有 AOF 实例 kill -9 重启 | NC-100 修复；TTL / 重连 / 持久恢复控制通过；Cluster 未跑 |
| 真实 Mongo | 事务重试、未知提交（upstream / downstream 黑洞）、唯一索引 9 组对照 | NC-101、NC-102 修复；mongos / 主从切换中提交未验 |
| 正式 Repository 链路 | 第 22 轮 28 消费叶子换真实副本集后端（迁移→文件 WAL→投影→新 Manager 重载、强杀子进程重开 WAL） | 28/28 通过两次；DataEngine 本体未改 |

4 组新正式回归（driver RESP 替身 1、versionstore 真实 Redis 1、mongo/driver 真实副本集 1、mongotest 1），修前红原文入证据。真实 Redis 集成 1008 pass / 30 环境 skip / 0 fail，相关 race -count=3、mongotest 消费包、根包、build/vet 通过，计数不累计作覆盖率。N04 仍场景部分完成，不计 completed/15。方向判断：第三方驱动默认重试 / 超时语义与“结果未知交给调用方”契约反复不一致（见本轮记录）。不等待 CI，不发版。

## 2026-10-05 N03 bus / nats / servicerpc / etcd（revn03）

基线 `be7bcc18`，分支 `revn03`，NC 段 90～99（用 90～93）；图谱 generation 2026-09-30 早于 N03 全部 10-04 / 10-05 修复，以当前源码补证。[本轮](REVIEW-2026-10-05-noncore-n03.md) · [审查证据](evidence/noncore-review-20261005-n03/README.md) · [修复证据](../bugfix/evidence/noncore-bugfix-20261005-n03/README.md)。审查 `e81d81bc`，修复 `ae742984` / `64179ad5` / `25646001` / `89a102db`。

| 子域 | 场景（真实依赖） | 状态 / 下一入口 |
| --- | --- | --- |
| JetStream 发布 / ACK / Term / 重投 | 共享隔离 NATS：业务错误单次 ACK、解码失败 5 次后 Term、回包失败重投 5 次、AckWait < handler 重复执行 | 契约成立；O1～O3 观察 |
| 重连 | 自起 nats-server 重启：中断期调用按期限返回、约 1.1s 恢复 | 成立；多节点集群未验 |
| 在途关闭 / 再次 Stop | Stop 不等 JetStream handler、回包随停止取消 | **NC-90 已修**；真实 NATS 停止返回 ctx 错误并保留、重试收敛、ACK 一次 |
| 满队列 fallback | Bus 派发池拒绝 RPC 不回包、进死信；RPC callback 池 fallback 复用 NC-09 回归 | **NC-91 已修**（2.0s 超时 → 0.3ms 拒绝） |
| 传输混用 | JetStream 部署里的轻量调用被请求流截获、报错却执行 | **NC-92 已修** |
| ServiceRPC 发现 + 剩余期限 | 真实 etcd 注册 + 真实 NATS：JetStream handler 看到 796 / 800ms | 成立；轻量路径不传期限（O8） |
| etcd Resign / Deregister 预算 | SIGSTOP 临时 etcd：Resign 阻塞 >20s；Deregister 500ms 返回 | **NC-93 已修**；Deregister 成立 |
| 服务端清理 / lease / watch 恢复 | 正常 Resign lease 撤销、键清零；lease 消失后 1.2s 重注册；断网 + 压缩后 Mirror 恢复 | 成立；O5～O7 观察 |

新增正式回归：bus 7（NC-90 3、NC-91 2、NC-92 2）、etcd/driver 2，integration 4（kit/nats 2、etcd/driver 2），均修前红 → 修后绿。改动包 race×3、integration race、全仓 build/vet、根包、18 个依赖包测试通过。N03 本单元的真实依赖场景收口；多节点 NATS / etcd HA、长时容量仍为外部项。方向判断（Bus 停止 / 排空、etcd 选主）见本轮末节。
## 2026-10-05 N08 codegen（revn08，macOS）

起点 `50e9a4e8`，交付前快进到 `be7bcc18`；独立 worktree 分支 `revn08`。cb11be90 的重发信号 / 进程树补修直接复用未重审；`add saga` 生成部分与赠礼 demo 步骤属 U-0280，未读改。[本轮](REVIEW-2026-10-05-n08-codegen.md) · [证据](evidence/noncore-review-20261005-n08/README.md)。NC-70～74 五个 P3 与 NC-75 P2 已修复、声明场景验证，未发版（NC-75 运行时 required 待决定）。

| 项 | 本轮 | 状态/边界 |
| --- | --- | --- |
| cfggen required/ref/skipempty 与真实 JSON/索引往返 | 当前 core 上 9 叶子 race×3；正式门夹具新增 skipempty/显式索引名/uint64/负 int64/bool/string 前向 ref 形状，对 pin v1.20.0 5/5，负对照红 | 完成本机部分；Luban 外部表未做 |
| 旧工程显式 upgrade / 改名退役 / 失败回滚 / 依赖整理 | v1.18.0 生成工程→当前 CLI upgrade 后 build/vet/test/doctor/check 通过；DAO 改名、退役；生成器失败回滚 manifest、依赖失败半升级后 `project deps` 收敛 | NC-71（预览漏列配置）已修；O2 半升级提示未改 |
| 取消与暂存树清理 | 正式 CLI 五条命令 SIGINT/TERM/HUP | NC-70 已修；生成器/复制/提交窗口残余（O6） |
| Unix 信号/进程树 | macOS 单独 race×3 21/21 | Windows taskkill 路径仍只 vet |
| 10 具名环境 skip、shell 部署/rollback | 9 条实跑通过（含真实 docker compose config、四条 shell）；shellcheck 9 脚本 1 note | 第 10 条为过期常量门（O1），翻转后 2/2 通过但未提交（联网） |
| N07 移交：roost id 错误码 AST 化 | NC-73 已修 | protocol/entity 标记 ID 未改 |
| N07 第二批移交：dev-run 期间 generate、运行时 required | NC-74 已修；NC-75 ref 与 `-check` 已修，tablegen 运行期门入 CI | 运行时 required 存在性待维护者选 A/B |

方向判断：“同级暂存树 + 外部 go 进程 + 信号”近期第四次出缺陷（RR-20261004-12/13、cb11be90、NC-70），建议下次改为 CLI 入口统一接管信号转 ctx 取消（详见本轮记录）。N08 仍**场景部分完成**：真实 systemd/k8s 部署、Windows 进程树、离线代理、强杀/磁盘故障未做。不等待 GitHub CI，不发版。

## 2026-10-05 N07第二批（configdata真实热更与handler快照）

基线3d4fe9f3，分支revn07b，图谱generation 09-30（ActiveSnapshot调用方都在模板里，入边为0），以源码补证。[本轮](REVIEW-2026-10-05-noncore-n07b.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-n07b/README.md)。NC-65 P2、NC-64 P3已修复、声明场景验证，未发版。

| 子域 | 场景 | 状态 / 下一入口 |
| --- | --- | --- |
| GM热更端到端 | 隔离Mongo/NATS/Redis+私有etcd起global+game，H0～H4：成功、四种失败（原子拒绝、scene不读失败代）、探针rollback、CSV+generate | 契约成立；NC-64说明修正；缺required被接受与generate不跳过.dev/移交N08 |
| handler快照 | K1～K7：两次读之间reload/rollback不跨代、下个请求读新代、判别反例、准入早于rollback读旧代 | 无缺陷；新增生成工程控制用例；C-O9观察 |
| 属性×配置 | A8加载后Gear/attr_final、A9热更不重算在线玩家 | NC-65修复；C-O8观察；方向判断见本轮 |
| 第一批观察 | C-O1/C-O2/C-O3/E-O4在真实进程无触发路径 | 维持观察，不改行为 |

新增2条生成工程正式回归（attribute 1、handler config snapshot 1）。本分支全新生成工程build/vet/`go test ./...`与4包race×3、codegen、根包、全仓build/vet通过（详见证据）。N07场景部分完成，只剩维护者决定项；下一N08（带两条移交）。不等待CI，不发版。

## 2026-10-05 N09 skill 第一批（执行/状态 + 事务/结算）

基线 `be7bcc18`（origin/main），分支 `revn09`，NC 段 110～119（用 110～113）；图谱 generation 2026-09-30，skill 自 09-27 无代码提交，以当前源码补证。[本轮/清单/矩阵](REVIEW-2026-10-05-n09-batch1.md) · [修复](../bugfix/RR-20261005-NC-110.md)。NC-110/111/112（P2）、NC-113（P3）已修复、声明场景验证，未发版。

| 方向 | 本批 | 状态 / 下一入口 |
| --- | --- | --- |
| 执行/状态（55） | E1～E4 施法开始 / 打断取消释放 / 结束 / 重复触发：`runtime.go`、`runtime_cast_window.go`、`runtime_cast_policy.go`、`scheduler.go`、`executor.go` 等主干 | NC-110～112（终止路径收尾不一致，统一为 `failCastLocked`）；checkpoint / 回放 / owned 进程 / 选择与输入等未审 |
| 事务/结算（3） | S1～S3、T1～T2：combatcomponent 全部 + combat attributes / buffs / damage；Nest 回滚 4 组合探针、BSON 往返 | NC-61 同形核对不成立（combat 状态随回滚恢复）；NC-113 map 共享；O1 Runtime 状态不在事务里、O2 属性修饰到不了伤害（待维护者） |
| 同步/接入（14） | T3 只核对生成接线：game-demo 只编译 catalog | skillsync / presentation 全部未审，下一批首选 |
| 数据/属性（107） | 未审 | 第三批：Parse + Compile 拒绝路径、skillcompose |

6 个新正式用例（skill 5、combatcomponent 1）修前红 → 修后绿；skill 5 包 race×3、examples / sync-e2e、build/vet、根包通过，不累计作覆盖率。N09 部分完成，不计 completed/15。方向判断：施法终止路径同一不变量（policy 槽位释放）第二次被打破（v1.5.0 修过 Cancel / Interrupt），建议并已按“唯一失败终态入口 + 对外 API 先判终态”收敛，不再按分支补步骤。不等待 CI，不发版。

## 2026-10-05 N06 S1/S2/S3/S6 复核（revn06）

起点 `50e9a4e8`，独立 worktree 分支 `revn06`；S4 global/App 与 S5 Saga 由其他 agent 负责，本轮不读改其生产文件。[本轮](REVIEW-2026-10-05-n06-revn06.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-revn06/README.md)。RR-20261001-06 残余 P2、NC-50 P3、NC-51 P3、NC-52 P2 已修复、声明场景验证，未发版。

| 项 | 本轮 | 状态/边界 |
| --- | --- | --- |
| S1 account | 丢回复 / 并发 Admin / 建角组合推演；换名路径不判死计划（残余补修）、补偿失败无指标（NC-50）；4 新用例 Memory + 隔离真实 Redis 红→绿 | 跨进程同时换名、Cluster、默认 30s ClaimTTL 未实跑 |
| S2 chat | 两副本时钟差 2h 并发 Append + 周期 Prune + AfterSeq 翻页组合控制；真实 Redis 上暴露 versionstore 伪冲突（NC-52），修后 9/9 | chat 自身无新缺陷；热点频道容量未测，Cluster 未跑 |
| S3 activity | 已确认 Keys 与 Opening 共用 `windowKeyProblem`（NC-51）；ledger 预约身份、oversized 轮转、Admin 前置条件复核 | 已混入 Delivering 的跨组键需运维；预约身份余项见观察 3 |
| S6 servicemetrics/Mail | 逐包上报点盘点；默认生成工程 Reporter 为 nil、无生产适配器（观察 1）；Mail 真实信封恢复 4 组合控制通过 | 默认指标落点待功能决定；其余服务只经 versionstore 回归 |

9 个新正式叶子（含 3 子用例）+ 2 组组合控制；7 原红。相关 race、真实 Redis 集成 891 pass/23 环境 skip/0 fail、根包、build/vet 通过，不累计作覆盖率。N06 仍部分完成，不计 completed/15。方向判断：activity 窗口条目验证第四次在相邻循环被打破、account 建角判定只在部分入口生效，建议收敛为单一入口 / 决策表（见本轮记录）。不等待 CI，不发版。

## 2026-10-05 N07第一批（configdata/attribute/event/errcode）

基线50e9a4e8（origin/main），分支revn07，图谱generation 2026-09-30、以源码补证。[本轮/矩阵](REVIEW-2026-10-05-noncore-n07.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-n07/README.md)。NC-60/61/63（P2）、NC-62（P3）已修复、声明场景验证，未发版。

| 子域 | 场景 | 状态 / 下一入口 |
| --- | --- | --- |
| configdata | C1～C11：热更新/回滚/DryRun、失败与panic回滚、跨Store、并发、Kit gauge；3观察 | 无确认缺陷；GM reload端到端与tablegen getter在handler内一致性待第二批 |
| attribute | A1～A7 | NC-60快照锁、NC-61模板回滚、NC-62生成期拒绝；方向判断见本轮 |
| event | E1～E8 | 无框架/模板接线，4观察不登记RR；接入还是移除待维护者 |
| errcode | R1～R5 | NC-63扫描拒绝非字面量/别名并查重名；id扫描AST化留N08 |

新增4组正式回归（attribute 1、codegen attribute 1、codegen errcode 1、game-demo模板 1）；修前红与修后绿原文入证据。8相关包race、根包、build/vet、codegen、attribute-runtime、全新game-demo消费通过。N07场景部分完成，不计completed/15；下一N07第二批→N08。不等待CI，不发版。

## 2026-10-05 N06第三批/第十七批修复

起点12726715，fetch/pull后主分支未变，无新Wanted/skill镜像差异。[本轮](REVIEW-2026-10-05-noncore-27.md) · [机制](IMPLEMENTATION-OUTBOX-CLAIM-AND-OPENING-RECOVERY.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-17/README.md)。NC-41 P2（outbox候选→并发Nack→原子领取）、NC-42 P3（Open持久坏计划）及RR-09非法/跨组sweep残余已修复、声明场景验证，未发版。

13新增正式叶子；原9叶子7红/2控制与旧产品overlay一致。race矩阵420 pass/2Cluster skip/0fail，根包14、build/vet、正式CLI两同步消费通过，计数不累计作覆盖率。account/chat/global/Mail具名增量已读/相关回归分列，N06仍场景部分完成。下一global RPC/Mod/Redis/App.Live实际消费→Saga剩余跨协调器/晚receipt/TTL及发布未知结果→N07/08。

用户要求交给另一agent的[全部后续review清单](REMAINING-REVIEW-HANDOFF-2026-10-05.md)已按N01～N15列出具体待查项，N06 S1～S6给精确停点，核心另一线接口和真实Mongo/Redis/Cluster/NATS/etcd/HA/长容量另列；候选CSV只用于定位。未声明整域/全仓完成，无新完成日期承诺，不等待CI，不发版。

## 2026-10-05 N06第二批/第十六批修复

47fca740干净快进cb11be90；新增codegen信号补修已整合，Unix压力结论未在Windows独立重做，无新Wanted/skill包差异。[本轮](REVIEW-2026-10-05-noncore-26.md) · [机制/漏检复盘](IMPLEMENTATION-SAGA-START-IDENTITY-AND-CANCELLATION.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-16/README.md)。NC-39/40两个P2已修、声明场景验证，未发版。

| 主链 | 本轮增量 | 状态/边界 |
| --- | --- | --- |
| 启动身份 | 两后端四阶段8反例红→绿，8兼容控制 | 原始摘要跨BSON/Replace保存；旧已推进缺摘要明确冲突，不伪造迁移 |
| 完成路由 | 3合法异键/缺目标反例红→绿，拒绝无状态/回执副作用、合法重投恢复 | 已接公开consumer；不是发布鉴权/真实broker Term验收 |
| 事务取消 | 三写阶段取消后新ctx重试、一次callback重跑、普通inbox业务取消 | 5新控制通过，后端mongotest，不证明Mongo未知提交 |
| 原生收件箱 | 普通取消保留lease/晚receipt不重跑、WAL前fenced交还并换token | 2新控制通过，权威receipt注入，不冒认实际WAL/投影 |
| servicemetrics | core/Kit seam和Recorder当前补证、既有计数/gauge/并发回归 | 两处中文职责注释，未声称每域错误路径都有指标 |

合计26新正式叶子；11原始/overlay行为反例，编译夹具修正单列。相关race/根包/build/vet/glsvet与生成消费结果见证据，不累计跨运行测试数作覆盖率。N06仍场景部分完成，不计completed/15；继续Service相对db4b7009的account/chat/activity/global等增量、实际指标落点及Saga跨协调器/receipt余项，再转N07。真实Mongo/NATS/未知提交/HA/容量与N05 Mirror DTO保留；不等待GitHub CI，不发版，无新的完成日期承诺。

交付正常整合 `47a9132c` 的 Nest U-0279；共享文档保留双方记录，排障编号 T-220 属上游，本轮 T-221/222。33材料路径 LF 摘要未变，合并后 Nest/Saga/Kit Saga race534叶子、0fail/0skip，根包14、全仓build、相关vet/glsvet通过；与原586不同范围，不相加。上游生成工程千轮/负载结论未独立复做，详见[最终整合](REVIEW-2026-10-05-noncore-26.md#最终整合与本地验证)。

## 2026-10-05 N05停机补证与N06第一批/第十五批修复

7949da08干净快进af2f67fb，新增仅演练清理文档，无新Wanted或skill包差异。[本轮](REVIEW-2026-10-05-noncore-25.md) · [机制与漏检复盘](IMPLEMENTATION-SAGA-CONSUMER-HEALTH-AND-DURABLE-RESUME.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-15/README.md)。NC-37/38两个P2已修复、声明场景验证，未发版。

| 范围 | 新证据 | 状态 / 下一入口 |
| --- | --- | --- |
| N05在途停止 | 正式JetStreamSyncBus/Replicator退订、重订阅与旧callback交错1叶子 | Stop不承诺drain；底层broker/Store为替身，不声称真实transport验收 |
| N06健康 | 三消费者缺失/退出与实际Kit停止/重新启动，10叶子4红→绿 | NC-37关闭；不是只证明循环活着 |
| N06持久恢复 | 正向/补偿两次恢复、重读/派发/回执，2红→绿；4代际兼容 | NC-38关闭；旧缺字段为0，混跑旧writer未支持 |
| N06生命周期 | 第二/第三订阅失败清理后重试；第三Drain取消后再次Stop | 3控制通过，无新确认关闭bug |
| 本地矩阵 | 合计20新正式叶子；相关race498/1skip、根包14、build/vet/glsvet；生成periodic/on_change2叶子 | 原红与overlay保留；真实Mongo/NATS/HA/容量单列 |

N05本机停机契约补证后转N06，**两域仍场景部分完成，不计completed/15**。接续Saga启动意图重投与运行Data/DeadlineAt变化、完成信封/收件箱和事务取消组合→Service增量/servicemetrics→N07。新方案Mirror DTO仍未实施，真实broker ACK/重连、L2跨节点水位/HA、长期容量保留；不据包测试数重算全仓覆盖率或承诺完成日期。App最新变更仅文档整合，本轮未独立重做其外部演练。不等待GitHub CI。

收尾正常整合3f29a921的v1.20.0发布/生成器下限与pretag记录；旧NC-31～36随上游tag发布，本轮NC-37/38仍Unreleased。33证据源码LF哈希相同，原正式/race矩阵保持；最终补检查与完整本地bugfix skill镜像同步见[交付同步](REVIEW-2026-10-05-noncore-25.md#交付同步v1200发布记录)。合并与验收分列，没有本轮发布动作。

最新编译/根包14/codegen vet通过；codegen/internal/roost普通包复跑280叶子/10环境skip，首跑缺sh的两环境失败保留。不是全codegen树重审，原20新增和498相关race不累加，不关闭外部留项。

## 2026-10-05 N05权威回填与第十四批修复

be4eb0fa干净快进40d89ac6；无新增bugfix/Wanted/skill待验收。[本轮](REVIEW-2026-10-05-noncore-24.md) · [机制](IMPLEMENTATION-AUTHORITATIVE-SNAPSHOT-POSTCONDITIONS.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-14/README.md)。NC-35/36两个P2与旧RR08残余已修复、声明场景验证，未发版。

| 入口 | 本轮新增证据 | 状态 / 下一入口 |
| --- | --- | --- |
| loader身份 | 两模式×四合法异键，L1/L2副作用与恢复 | 8读取+1消费红→绿，NC-35闭合 |
| 最终最低版本 | 较新epoch保留但版本不足、合法同epoch恢复 | 2读取+1消费红→绿，NC-36闭合 |
| I/O跨有效期 | L2.Set等信封截止时间，最终返回miss、新版本恢复 | 2读取红→绿，旧RR08追加，不新编号 |
| 复制/生命周期 | gap/epoch/schema/错误传播六消费；第二订阅失败、重试与重复Start/Stop最终active=0 | 共19新正式叶子；既有Stop超时后再次停止不双计 |
| 最终本地矩阵 | 733相关race叶子/8skip、根包14、build/vet/glsvet；正式生成双模式2叶子；codegen620普通叶子/10环境skip | 具名环境缺口保持，不是全仓覆盖率 |

N05仍**场景部分完成、不计completed/15**。接续真实transport停机/旧handler在途交错的具名契约与剩余场景，再转原计划N06；Mirror DTO未实施，跨节点水位/真实broker ACK/HA/长容量保持留项。最新App方案已记录1/2/2b/3/3b实现，4/5与真实进程演练不因整合算完成；本轮接手其记录并独立运行相关App/Kit、codegen与global API单测，未作全feature独立审计。不等待GitHub CI，未发版，无新的全部完成日期承诺。

末次同步64acd782：另一线赠礼静态sid路由第4笔已实施/整合，前句4/5指首次基线。原15证据源码哈希不变；最新build/根包14/codegen vet通过，正式生成game-demo消费另见[末次记录](REVIEW-2026-10-05-noncore-24.md#末次同步赠礼静态sid路由64acd782)。第5笔真实进程演练不算完成，未发版。

末次生成消费89 race叶子/2Mongo skip通过；原始依赖的完整build触发历史B35 genproto重复包，已留档，只有独占夹具显式整理依赖后的全仓build/vet通过。模板与框架依赖未修改，不将默认依赖流程/公开tag兼容记为已验证。

推送重试整合10e2e0ea：作者App方案第1～5笔现标已实施/有真实演练记录，本机未重跑该外部演练。原15 N05证据源码LF哈希不变；最新core build/根包14/codegen vet/etcd前缀回归通过，新独占生成消费91 race叶子（作者新增两项phase/topic）/2Mongo skip、显式整理依赖后生成build/vet通过。89与91是两次基线矩阵，不累加或改变本批新增19计数。[最新同步](REVIEW-2026-10-05-noncore-24.md#推送重试同步10e2e0ea)。

最后同步fdcd8605的etcd停机与App相邻控制：普通App/etcd-driver race162叶子、核心build/根包14/vet独立通过；真实etcd integration-tag未执行，原N05证据与91生成消费未受模板变化影响。[最终增量](REVIEW-2026-10-05-noncore-24.md#最后etcd停机增量fdcd8605)。未计功能域全部完成/外部验收。

交付时又正常整合f8bb0261的登录claim/赠礼重启预算与卸载测试；原15N05源码不变，最新build/根包14/codegen vet通过。新模板只整合，未本轮逐项独立审查，91生成消费仍对应10e2e0ea，不冒认最终全部模板验收。[交付范围](REVIEW-2026-10-05-noncore-24.md#交付范围冻结与最后整合f8bb0261)。

## 2026-10-05 N05接入与NC-33/34修复

基线b2232db5，fetch/ff-only已最新，无新增修复/Wanted待验收。[本轮](REVIEW-2026-10-05-noncore-23.md) · [机制](IMPLEMENTATION-MIRROR-PAYLOAD-IDENTITY-AND-ROUTING.md) · [证据](../bugfix/evidence/noncore-bugfix-20261005-13/README.md)。两个P2已修、声明场景已验证，未发版。

| 入口 | 本轮增量 | 状态 / 下一入口 |
| --- | --- | --- |
| cache mirror | key/version两项副作用红→绿，无VersionOf与null指针控制 | NC-33闭合；普通Delete不声明版本墓碑 |
| interest mirror | renew/release各scope/SID/expiry/op四项红→绿 | NC-34闭合；generation0/迟到release/空Delete控制保持 |
| 范围 | 11生产文件当前补证，13新正式叶子；592相关race叶子/8skip、根包14、build/vet/glsvet通过 | 复用既有路由/Replicator/Snapshot回归不重复计新增；非全仓覆盖率 |
| 下一轮 | BindSync/Assembly失败重订阅、停止/回调交错、snapshot gap/epoch/schema回填组合 | N05仍场景部分完成；真实broker/HA/水位/容量另列 |

N04本机迁移链沿用上轮，不重验未变化来源。N05不以11/历史24文件计算完成率、不计completed/15。Mirror DTO与静态PlayerOwner未在本轮实施；最后整合另一线App singleton第1笔d4ac9853/c9b934ae及e3810ef1续期预算修正，原22路径哈希不变，最终扩大race709叶子/8skip及根包/build/vet/glsvet通过；第2～5阶段与真实进程/Cluster未独立验收。[同步边界](REVIEW-2026-10-05-noncore-23.md#最后同步app单实例锁第1阶段)。无新的全部review完成日期承诺。不查询/等待GitHub CI。

## 2026-10-05 NC-32修复与N04嵌套迁移 / 持续CAS收口

起点/最新 main c3aa0edd，无新代码增量；新增[NC-32](../bug/RR-20261005-NC-32.md)已修复、声明场景验证，未发版。[运行](REVIEW-2026-10-05-noncore-22.md) · [修复](../bugfix/RR-20261005-NC-32.md) · [证据](evidence/noncore-review-20261005-22/README.md)。

| 范围 | 本轮证据 | 状态 / 下一入口 |
| --- | --- | --- |
| 恢复后的深层业务写 | 正式9红与消费6红，修后完整金样53/消费者28通过 | wire恢复递归绑定修复；应用重生成，不自动补历史漏写 |
| 嵌套 / 类型变化 | 正常两步string→int64、坏目标类型、坏源数值、null四项 | 成功后深层修改实际写回，fresh Manager无迁移器重载；本机具名缺口闭合 |
| 持续CAS竞争 | 两次正式竞争者CAS淘汰、第三次视图预算错误；再Load到最新version成功 | 有界ErrMigrationConflict、未发布、Flush后unacked=0；一项新增通过 |
| 数量与范围 | 28消费 = 上轮17 + 本轮11；53金样 = 既有44 + 新正式9 | 不重复计新增，不将测试数折算全仓覆盖率 |

N04仍历史41/41候选源文累计已读，整个功能域**场景部分完成，不计completed/15**。本机生成迁移具名链已补齐，转N05路由/mirror增量；真实Mongo副本集/未知提交、Redis Cluster/弱网、部署故障矩阵、长期容量与大嵌套恢复性能单列保留，不关闭历史17外部skip。没有新的功能域完成日期承诺。最终本地验证结果与初次环境失败的处理见证据；不等待GitHub CI。

收尾上游0f554aed只改App单实例锁/静态玩家绑定方案文档，已整合；无Go/Wanted增量，不计实现或独立验收。[交付增量](REVIEW-2026-10-05-noncore-22.md#交付前上游增量)。

## 2026-10-05 NC-31修复与N04多DAO / CAS / 进程恢复

起点edf85be9，干净main快进3127d37c；本轮修改/验证身份以[摘要](evidence/noncore-review-20261004-21/source-hashes.csv)及交付提交为准。[运行](REVIEW-2026-10-05-noncore-21.md) · [NC-31修复](../bugfix/RR-20261004-NC-31.md) · [证据](evidence/noncore-review-20261004-21/README.md)。**NC-31已修，声明场景验证，未发版；接续review无新增确认RR。**

| 范围 | 本轮证据 | 状态 / 下一入口 |
| --- | --- | --- |
| 迁移准入 | 可信正式5红/3控制、生成3红/8控制；12新正式及原11消费全绿 | 坏BSON/目标类型/身份不进入CommitSystem；兼容int32 ID/旧payload schema |
| 多DAO | 正常两个提交，后序坏类型/步骤失败不发布；前序保留，正式CAS修正后只接续剩余DAO | 3新增消费通过；逐DAO持久迁移，不承诺全有或全无 |
| 并发CAS | 目标schema竞争者保留score99，旧schema竞争者重读后再迁移；旧WAL结算/unacked=0 | 2新增消费通过；持续竞争达到预算尚待正式生成消费补证 |
| 进程恢复 | strict CommitSystem返回后强杀独占子进程，重开同目录WAL并重放/新Manager加载 | 1新增消费通过；真实文件恢复，mongotest不证明Mongo/HA |
| 本地验证 | 合计17生成叶子、五包race421pass/1helper skip、根包14、build/vet/glsvet通过；Kit integration仅编译 | 不等待/查询GitHub CI；历史17外部skip不关闭 |

N04仍为历史41/41源文累计已读、**业务场景部分完成，不计completed/15**。六项增量场景已补，不双计原11，不往产品文件分母加入核心同行和测试。下一N04嵌套/类型变化及持续CAS竞争生成消费、具名清单收口→N05路由/mirror增量。收尾远端42059469仅文档：W-2026-10-04-08按维护者静态sid绑定新前提归类“简化后不适用”，新方案未实施，不算旧实现修复/独立验收。[增量说明](REVIEW-2026-10-05-noncore-21.md#交付前上游增量)。真实Mongo副本集/未知提交、Cluster/HA、真实部署断电/强杀矩阵、长容量保留；没有按本轮测试数重算全仓百分比或完成日期。

## 2026-10-04 N04 正式迁移写回与重载接入

基线`34137925`，从`04d679b8`干净main快进；[本轮记录](REVIEW-2026-10-04-noncore-20.md)、[证据/复跑](evidence/noncore-review-20261004-20/README.md)、[实现学习](IMPLEMENTATION-DAO-MIGRATION-HYDRATION-AND-REFHMAP-SCHEMA.md)。**新P2 [NC-31](../bug/RR-20261004-NC-31.md)已确认、未修**：迁移结果先提交，坏BSON/ID进WAL，错误目标字段写成不可加载的schema3/version8。

| 范围 | 本轮增量 | 当前状态 / 下一入口 |
| --- | --- | --- |
| 正式生成DAO→Repository→Runner→文件WAL/Projector→MongoStore→重载 | 11叶子3fail/8控制pass；目标解码与提交时序差异确认；后端mongotest | 新缺陷未修，不冒认真实Mongo/HA |
| 成功/取消恢复 | 新Manager无迁移器重载成功；caller取消但晚投影成功后可重新加载；较新schema/步骤失败不提交 | 单DAO/scalar链已验证，多DAO/并发CAS/重启仍待验 |
| 本地验证 | 全仓build0，既有34定向race、根包14、相关vet通过 | 不等待或查询GitHub CI；普通构建条件已满足，新反例不能称绿 |
| 规范/源码说明 | 本机review及共同skill明确中文注释和本地验收；3个Go文件仅补中文契约注释 | 未改行为/接口/正式测试，不新增产品文件分母 |

N04历史候选源文41/41累计保留，**业务场景仍部分完成，不计completed/15**。接续多DAO迁移/并发CAS淘汰和正式重载→N04具名缺口收口→N05路由/mirror增量；用户要求bugfix时先NC-31。真实资源/长期容量等既有留项保持，不据本批计数折算全仓覆盖率或重算日期。

**10-04 复审复盘**：只核对既有RR-02～07与流程，不新增功能域完成计数。六修复提交祖先关系确认；d3历史四项28fail/4控制，定向36pass，vet及根包通过。收尾aa35已将新Wanted分流RR-08并修复，Kit/NatsDriver45普通叶子race/vet及最终根包独立通过；外部验收不冒认。[复盘/改进](REVIEW-2026-10-04-fix-audit-retrospective.md) · [证据](evidence/noncore-audit-followup-20261004/README.md)。下一迁移接入/N04收口，08真实资源/组合留项单列。

**最终接手状态（b9625f4f）**：NC-30已修、未发版；上游RR-20261004-02～07均已实施。合并后241相关叶子、12 NC-30正式、16 review、11生成消费、根包12及mongotest115叶子通过；真实etcd/Mongo对照不冒认本机验收。新W-2026-10-04-02连接drain超时重试候选留待真实NATS复现，优先于迁移接入。[最终同步记录](REVIEW-2026-10-04-noncore-19.md#最后增量同步)。下方旧“未修”及RR-07待修为接手时点。

## 2026-10-04 第十批修复与非三大核心第十一批 N04

main从25ef4c1e快进e7027a65（仅C01文档增量），新[NC-30](../bug/RR-20261004-NC-30.md)确认并修复，无新增待修RR。[运行](REVIEW-2026-10-04-noncore-19.md) · [学习](IMPLEMENTATION-DAO-MIGRATION-HYDRATION-AND-REFHMAP-SCHEMA.md)，未发版。

| 范围 | 本批实际证据 | 当前状态 / 接续 |
| --- | --- | --- |
| registry清理 | 旧树及overlay各2fail/4控制；12正式、六包221叶子race/vet通过 | NC-30已修；不是值CAS，历史孤儿不自动清理 |
| Patch/Set/schema/恢复 | 16新review场景通过，其中3个观察展示混读、旁支过期和Stale竞争 | 类型变更、真实弱网、Cluster/HA/长容量待验 |
| 正式生成消费 | 7 RestorePersisted迁移+4直接/Cached错误透传，11叶子race/vet通过 | Repository持久写回/重载/进程故障未验，不称数据库已迁移 |
| N04源码 | 41候选，40同hash复用第18轮，ref_hmap.go全文/diff；18材料路径coverage/hash | 累计源文41/41，业务场景部分完成，不计completed/15 |

下一 **正式迁移的Repository/持久确认/重载接入（复用另一线核心证据，仅补消费）→N04缺口收口→N05路由/mirror增量**。本机Mongo/Cluster/HA/真实弱网不可用；原17环境skip未被本批0skip关闭。另一线C01已有1h通过记录，本机未独立复跑。跨域50～90有效小时组织估计未重算。[计划](NONCORE-REVIEW-PLAN-2026-10-03.md) · [留项](../bug/CARRYOVER.md)。下方旧未修保留原时点。

**最终状态更新（2026-10-04）：RR-20261004-01已由上游3bb901fb/5d386146修复，本轮独立验收通过，未发版。** 本轮原4场景在真实Redis全部转绿；13条取锁/释放未知正式回归race与Remote vet通过，另3条真实Redis集成通过。下方本轮“新wanted未修”保留发现时点，以本条及独立验收为准；三资源生成消费者、authority故障矩阵与长稳未在本机验收。 [验收证据](../review/evidence/noncore-review-20261004-18/README.md#独立验收上游修复)。

提交前同步补充：整合远端4提交（`cfe878fe`），新wanted已登记[RR-20261004-01](../bug/RR-20261004-01.md) P2未修；[追加结论](REVIEW-2026-10-04-noncore-18.md#提交前新增wanted)。原修复/普通Redis14场景证据保留3d3b22c9基线，未受远端代码影响，不声称远端负载harness/长稳已本机验证。

## 2026-10-04 第九批修复 + 非三大核心第十批 N04

main 基线 `3d3b22c9`，fetch/pull 无 main 增量。[NC-26～29 修复](REVIEW-2026-10-04-noncore-17.md) · [Redis 锁/续租/订阅审查](REVIEW-2026-10-04-noncore-18.md) · [学习](IMPLEMENTATION-REDIS-LOCK-RENEWAL-AND-PUBSUB-LIFETIME.md)。下方旧未修保留历史，未发版。

| 范围 | 本批新增证据 | 状态 / 接续 |
| --- | --- | --- |
| NC-26～29 | 修改前及旧产品 overlay 13 叶子均 7 fail/6 控制；28 正式叶子转绿，十包 race/vet 320 pass、0 fail/skip | 四个替身 P3 已修；D 路径、unique 建立、bulk Type、私有事务隔离/冲突 |
| 受影响消费者 | 首次旧夹具 2 fail；明确错误注入并加并发提交保留断言后，14 测试包674 pass/17 环境 skip，race/vet 通过；两个生成 DAO 消费者通过 | 仅必要测试夹具调整，核心产品未改；17 skip 不计通过 |
| Redis 新审查 | 14 新场景全通过：实际 Lua 后丢回复注入、stale token、续租跨 TTL/丢锁、满队列/并发 Close/channel 映射 | 本批无新 RR；真实弱网、重连、租期时钟、HA/Cluster/长稳未验 |
| N04 源文清单 | 原40/40历史快照；新增事务实现文件后当前41/41累计已读，39同hash复用+旧文件diff/新文件全读 | [inventory/hash/coverage](evidence/noncore-review-20261004-18/README.md)；不是本轮新读41文件或业务100% |

下一优先 **RefHMap schema/Patch 与全量 Set 并发及未知结果恢复 → 正式迁移消费者**。真实 Mongo/Redis Cluster/HA/长期容量另留项，N01～N04 仍不计 completed/15。约50～90有效小时跨域风险预算未重新按余项估算，本轮不按文件/测试比例扣减。[计划](NONCORE-REVIEW-PLAN-2026-10-03.md) · [CARRYOVER](../bug/CARRYOVER.md)。提交前fetch到cfe878fe后新增wanted已分流RR-20261004-01 P2未修：真实Lua4叶子2fail/2控制。新增Remote范围不算N04分母，远端harness/长稳改动未在本机验收。

## 2026-10-04 第八批修复 + 非三大核心第九批 N04

main起点`ce90e90d`，fetch/pull无增量。[NC-21～25修复](REVIEW-2026-10-04-noncore-15.md) · [N04第四批](REVIEW-2026-10-04-noncore-16.md) · [NC-26～29未修](../bug/REVIEW-2026-10-04-noncore-16.md) · [机制/实施交接](IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md)。下方旧未修为历史时点，未发版。

| 范围 | 本批证据 | 状态 / 接续 |
| --- | --- | --- |
| NC-21～25 | 原12叶子6fail/6控制、旧产品overlay复跑相同；40新增正式叶子通过，十包race/vet292叶子pass/0fail/skip | 五项已修，未知Lua错误/复制/唯一成员/精确比较/post-image身份已验 |
| 正式与受影响消费者 | DAO CLI独立module生成、两个真实Redis消费者race/vet通过；DataEngine/Kit/Service14测试包674叶子pass/17skip | 仅已有回归，不重审核心三模块；17环境skip单列，不计通过 |
| N04源文 | 固定清单累计40/40已读；38同hash复用、两项未变范围复用+当前diff补证 | [inventory/hash/来源](evidence/noncore-review-20261004-16/inventory.csv)，不是本轮新读40文件或业务100% |
| 新审查 | 13叶子7fail/6控制：D路径、已有重复的unique建立、非法bulk Type预检、并发全库restore | NC-26～29四P3，仅公开Mongo替身，已确认未修 |

下一新范围 **Redis锁/AutoExtend/pubsub真实故障 → RefHMap schema与未知结果恢复 → 正式迁移消费**，真实Mongo cursor/partial bulk/事务与Cluster/HA继续留项。用户说没有修复则跳过NC-26～29验收；要求bugfix先处理四项。N01～N04仍不计completed/15，约50～90有效小时风险预算保持，未以文件或测试数扣减。[证据](evidence/noncore-review-20261004-16/README.md) · [计划](NONCORE-REVIEW-PLAN-2026-10-03.md) · [CARRYOVER](../bug/CARRYOVER.md)。

## 2026-10-04 第七批修复 + 非三大核心第八批 N04

基线`08d18be9`，fetch/pull无增量。[NC-16～20修复](REVIEW-2026-10-04-noncore-13.md) · [N04第三批](REVIEW-2026-10-04-noncore-14.md) · [五新未修RR](../bug/REVIEW-2026-10-04-noncore-14.md) · [机制学习](IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md)。下方旧未修保留历史，未发版。

| 范围 | 新证据 | 状态 / 接续 |
| --- | --- | --- |
| NC-16～20 | 原16叶子10fail/6控制实际红、旧产品overlay红复跑；最终41新正式叶子通过，十包race/vet252叶子pass/0fail/skip | 5/5已修；nil根/codec、深层Patch/root miss/类型冲突/祖先TTL/registry/非法布局/分页大值具名验证 |
| 正式生成消费 | 当前DAO CLI独立module生成ref-hmap DAO，真实Redis实际nil父Patch和已有路径更新 | 一个消费者race/vet通过，补上前轮仅模板可达的缺口；无模板或格式迁移 |
| N04源文 | 补mongotest595以后与当前前段/RefHMap，固定候选累计40/40全文已读；38项按前轮hash复用 | [清单/blob/来源](evidence/noncore-review-20261004-14/inventory.csv)；源文阅读完成，场景部分完成，不是100%业务覆盖 |
| 新审查场景 | 十二叶子6fail/6控制：真实Lua+执行后丢回复注入2项，公开替身10项 | NC-21一P2、NC-22～25四P3未修；初版BSON类型假设错误已修探针，不计产品失败 |

下一新入口 **BSON.D嵌套路径/unique-index/bulk/并发事务隔离 → Redis锁/续租/pubsub真实故障与Cluster → Mongo真实cursor/partial bulk/事务重试及正式迁移消费**。用户声明未修跳过NC-21～25验收，要求bugfix先修本轮五项。N01～N04仍不计completed/15；约50～90有效小时跨域风险预算保持，未按40/40扣减或承诺日期。[证据](evidence/noncore-review-20261004-14/README.md) · [计划](NONCORE-REVIEW-PLAN-2026-10-03.md) · [留项](../bug/CARRYOVER.md)。

## 2026-10-04 第六批修复 + 非三大核心第七批 N04

基线`1502f973`，fetch/pull无增量。[NC-13～15修复](REVIEW-2026-10-04-noncore-11.md) · [N04第二批审查](REVIEW-2026-10-04-noncore-12.md) · [五个新未修RR](../bug/REVIEW-2026-10-04-noncore-12.md) · [RefHMap机制与实施交接](IMPLEMENTATION-REFHMAP-LAYOUT-PATCH-AND-REDIS-LIFETIME.md)。下方旧未修保留历史时点，未发版。

| 范围 | 实际新增证据 | 状态 / 接续 |
| --- | --- | --- |
| NC-13～15 | 修前21叶子9fail/12控制，旧实现overlay复跑相同；最终41正式准入叶子通过，五包race/vet：170叶子pass、186 pass事件、0fail/7skip | 3/3已修；fatal分类、Layered被拒回填与四store旧写错误契约已验证，Redis比较仍是建议性而非CAS |
| 真实Redis与正式DAO消费 | 原七Redis集成7pass、Raw/Hash/JSON旧/新写6pass；正式DAO CLI在独立module生成，两消费者race/vet通过并复跑 | 七skip另列补证，不覆盖旧日志；不包括Cluster/HA、真实Mongo或正式迁移消费者 |
| N04源文 | 新补19个完整候选，累计39/40已读；两Kit文件此前已同行；mongotest仅1–595行 | [清单/blob/当前hash](evidence/noncore-review-20261004-12/inventory.csv)，39/40不是业务覆盖率；场景仍部分完成 |
| RefHMap与mongotest新场景 | 真实Redis8叶子5fail/3控制，mongotest8叶子5fail/3控制；正常Mongo/migration三测试包race34pass、五包vet通过 | NC-16～19四P2、NC-20一P3未修；Redis独占进程已退出，替身分页失败不冒称真实Mongo失败 |

下一新入口 **mongotest595以后 → RefHMap Eval未知结果/Patch TTL/schema → Redis订阅/续租真实故障与Cluster → Mongo真实cursor/partial bulk/事务重试及正式迁移消费**。用户声明未修则跳过NC-16～20验收，要求bugfix时先修本轮五项。N01～N04均仍不计completed/15；[约50～90有效小时计划](NONCORE-REVIEW-PLAN-2026-10-03.md)是跨域风险预算，本批没有按39/40源文比例扣工时或承诺完成日期。[修复证据](../bugfix/evidence/noncore-bugfix-20261004-06/README.md) · [新审查证据](evidence/noncore-review-20261004-12/README.md) · [外部/容量留项](../bug/CARRYOVER.md)。

## 2026-10-04 第五批修复 + 非三大核心第六批 N04

基线`3560a19b`，pull无增量。[NC-11/12修复](REVIEW-2026-10-04-noncore-09.md) · [N04运行](REVIEW-2026-10-04-noncore-10.md) · [三个新未修RR](../bug/REVIEW-2026-10-04-noncore-10.md) · [机制与实施交接](IMPLEMENTATION-CACHE-ADMISSION-AND-MIGRATION.md)。下方旧未修为历史时点，未发版。

| 范围 | 实际新增证据 | 状态 / 接续 |
| --- | --- | --- |
| NC-11/12 | 修前8项2fail转绿；最终15正式项、两个测试包race61 test pass事件0fail/skip、KitEtcd编译/vet及三包vet；真实SDK Grant取消/超时、双收尾/重试 | 2/2已修、声明场景验证；正常Resign预算/服务端回收与集群恢复仍留项 |
| N04源文 | 20/40完整读取：cache10、migration1、Mongo5、Redis4；KitRedis/KitMongo2同行，RefHMap/mongotest/driver client仅具名范围 | 源文与场景部分完成；[逐文件清单/blob](evidence/noncore-review-20261004-10/inventory.csv)，不是50%业务覆盖 |
| N04普通场景 | overlay26叶子=9行为失败/16控制/1容量观察；五测试包race113 test pass事件0fail/7skip，另三个包无测试；八包vet | NC-13/14 P2、NC-15 P3待修；容量只观察，完整场景分母尚未收口 |
| 真实Redis补测 | 专属Redis8.8.0原7skip逐项实际通过；正式cache/driver实际Redis6叶子2fail/4pass补证NC-15，独占进程已退出 | 原skip日志保留、补测另列；不声称Cluster/HA/长稳或Mongo/etcd真实部署通过 |

下一新入口 **N04 RefHMap完整反射/patch/schema与Lua未知结果 → Redis assembly/锁/pubsub/cluster恢复 → Mongo真实cursor/partial bulk/事务重试与正式DAO/codegen迁移消费**。用户说未修时直接接续新内容，不重验NC-13～15；明确bugfix时先修这些RR。N03仍39/39源文已读、场景部分完成；N01～N04均不计completed/15。[约50～90有效小时计划](NONCORE-REVIEW-PLAN-2026-10-03.md)仍是风险预算，本批未核定剩余风险新工时，不按20/40源文比例扣减。[修复证据](../bugfix/evidence/noncore-bugfix-20261004-05/README.md) · [新审查证据](evidence/noncore-review-20261004-10/README.md) · [外部/预算留项](../bug/CARRYOVER.md)。

## 2026-10-04 第四批修复 + 非三大核心第五批

[修复运行](REVIEW-2026-10-04-noncore-07.md) · [etcd/KitEtcd审查](REVIEW-2026-10-04-noncore-08.md) · [两个新未修P2](../bug/REVIEW-2026-10-04-noncore-08.md) · [机制学习](IMPLEMENTATION-ETCD-SNAPSHOT-WATCH-AND-LIFETIME.md)。源码起点`e62729ac`，下方NC-08～10已修，本批NC-11/12未修，未发版。

| 范围 | 本批新增证据 | 状态 / 接续 |
| --- | --- | --- |
| NC-08～10修复 | 17正式项4fail转绿，最终28正式项、17原overlay通过；六测试包race/vet、126 test pass事件0fail/skip；正式CallReliable进程内消费 | 三项已修/声明场景验证；真实broker/connected Kit/长期容量未验 |
| N03清单源文 | 剩余14个etcd文件完整读取，合并上批为39/39；KitEtcd1同行 | 源文已读，场景仍部分完成，不是业务覆盖100%或completed/15 |
| etcd新场景 | 8项2fail/6控制；真实SDK→本机gRPC LeaseGrant，第三方watcher Close、readiness/队列关闭/镜像copy/CAS/stale | NC-11/12待修；两测试包race45 pass事件，KitEtcd仅编译/vet，无测试 |
| 外部环境 | 真实etcd集成选中1项因PATH缺etcd而skip | 不计通过；真实NATS/etcd、HA、长稳继续待验 |

[修复证据](../bugfix/evidence/noncore-bugfix-20261004-04/README.md) · [新审查证据/清单](evidence/noncore-review-20261004-08/README.md)。下一新范围 **N04 redis/mongo/cache/migration**，复用Service旧专项；N03真实资源/lease恢复、选主清理和callback关闭余项独立保留。用户说“没有修复”时直接进入N04，明确bugfix时先修新RR。约50～90有效小时仍为风险工作量粗估，本批未按39/39比例扣减，[计划](NONCORE-REVIEW-PLAN-2026-10-03.md)维护同一口径。

## 2026-10-04 非三大核心第四批：N03 通信与 RPC

[运行](REVIEW-2026-10-04-noncore-06.md) · [三个新未修 P2](../bug/REVIEW-2026-10-04-noncore-06.md) · [机制与实施方向](IMPLEMENTATION-MESSAGING-RPC-BUDGET-AND-TERMINAL-OWNERSHIP.md) · [反例/源文件清单](evidence/noncore-review-20261004-06/README.md)。产品提交`49796514`，下方NC-05～07已修；新NC-08～10未修，未发版。

N03 39个清单源文件中完整读取25个：Bus8、NATS13、ServiceRPC3、etcd接口1；另读Kit NatsMod1，worker.Pool与etcd.driver.Discover只计具名范围。17个overlay叶子/独立项=4失败/13控制，对应JS无handler协议、callback停止预算、发现预算3根因。六包race/vet通过、93 test pass事件、0fail/skip；没有真实NATS/etcd/Redis、HA、长稳或性能实测。

N01/N02/N03均为场景部分完成，25/39只表示N03清单源文读取，不能当业务覆盖率或完成单元数。下一入口是剩余14个etcd文件（driver/discovery仅查Discover，仍在余项）与KitEtcd：watcher/election/local_mirror/订阅、snapshot/watch readiness、租约恢复与关闭；N03真实JS往返、满队列fallback与RPC剩余预算也保留。[计划](NONCORE-REVIEW-PLAN-2026-10-03.md)仍采用约50～90有效小时风险预算，未从文件数扣小时。

## 2026-10-04 第三批修复：N02 NC-05～07

[修复运行](REVIEW-2026-10-04-noncore-05.md) · [记录](../bugfix/README.md) · [红绿/消费者](../bugfix/evidence/noncore-bugfix-20261004-03/README.md)。起点 `c4aa1e7d`，三个P2已修、声明场景验证，未发版。正式29项14失败转绿，最终30项、原overlay34项通过；正常/退役及旧坏模式生成物13次消费者全绿，三个坏模式生成阶段拒绝。受影响最终20测试包race/754事件、vet通过；9原测试skip和缺sh的具名检查不当作通过。N02仍为场景部分完成，接N03通信域；下方“未修”保留原时点。

## 2026-10-04 非三大核心第三批：N02 请求链与真实生成消费

[本轮运行](REVIEW-2026-10-04-noncore-04.md) · [三个新未修 P2](../bug/REVIEW-2026-10-04-noncore-04.md) · [学习与实施建议](IMPLEMENTATION-REQUEST-ADMISSION-AND-GENERATED-WEBROUTES.md) · [反例/控制/源文件清单](evidence/noncore-review-20261004-04/README.md)。源码 `483350ca`，下方 NC-01～04 已修；本批 NC-05～07 未修。

N02 8/8 生产源文已读（本批 security3/gateway2/webroute1，复用当前未变化 HTTP2），另查四个 Webroute 生成链文件。34 个 overlay 叶子/独立项为 5 失败、28 控制通过、1 契约观察；六包 race/vet 通过、111 test pass 事件。正式 CLI 在四个独立业务 module 生成、编译、注册，实际 HTTP/退役共 10 控制通过，三类非法模式 3 失败；五个阶段选择 skip 不计场景。补齐 RR-20260930-08 的正常 HTTP 消费与退役旧 URL 404 缺口，不把新模式校验 bug 混为旧修复失败。

N01 15/15 与 N02 8/8 仅代表清单源码读取，两域场景均部分完成；不据此给出全仓业务覆盖率或 completed/15。N01 仍缺 Group 阻塞预算、完整 App 故障进程、Ops bind/hijack/权限与 Health 策略；N02 仍缺容量/非协作回调、完整业务鉴权与跨模块矩阵。新 RR 单独修复，下一批 **N03 bus/nats/servicerpc/etcd** 请求关联、取消、关闭与恢复，Kit 同行。[计划](NONCORE-REVIEW-PLAN-2026-10-03.md)按已补证缺口更新为约 50～90 个有效工作小时；仍非实测速率、完成日期或后台任务承诺。

## 2026-10-04 第二批修复：NC-01～04

[修复运行](REVIEW-2026-10-04-noncore-03.md) · [逐项记录](../bugfix/README.md) · [正式红绿/复跑](../bugfix/evidence/noncore-bugfix-20261004-02/README.md)。源码起点 `7e0d6ee2`，四项均已修、声明场景验证，未发版；下方第二批“未修”保留历史。

原14项（11fail/3pass）全转绿，新增正式集合35叶子/独立项通过；11包race、233test pass事件、0fail/skip，vet通过。补齐 Manager 重复/并发/失败重试与最后Start接管、schema nil/空/并发副本、Ops取消/期限/重试/并发关闭。原 Health观察未改。N01源文15/15仍成立，场景依然部分完成：永久阻塞/Group预算、完整App故障进程、Opsbind/hijack及权限链尚缺；继续N02，不由四项关闭推定整域完成。

## 2026-10-04 非三大核心第二批：N01 生命周期与 Ops

[本轮运行](REVIEW-2026-10-04-noncore-02.md) · [四个新 RR](../bug/REVIEW-2026-10-04-noncore-02.md) · [机制与建议](IMPLEMENTATION-RUNTIME-MANAGER-AND-OPS-OWNERSHIP.md) · [复跑/源文件清单](evidence/noncore-review-20261004-02/README.md)。源码 `3529a569`，和下方第一批四项修复分开；新问题三个 P2、一个 P3 **均未修**。

| 范围 | 本批新增证据 | 状态 / 接续 |
| --- | --- | --- |
| N01 生产源文 | 本轮补十文件，与上批 App 五文件合计 15/15；另读两个 Kit adapter | 源码读取完成，场景验证部分完成；不是整个单元收口 |
| Manager 生命周期 | 5 个新叶子/独立项，4 fail/1 pass | Stop panic、重复 Start 两 RR；并发双 Start、失败重试、停止竞争待验 |
| Lifecycle Hook / Admin | Hook 2 pass；metadata 7 项 6 fail/1 pass | schema 所有权 RR；Group 阻塞/预算与完整权限/审计链待验 |
| Ops / Health | 真实取消关闭 1 fail、正常控制 1 pass、空 Status+Err 1 观察 | Ops 所有权 RR；Health 观察尚非 RR；deadline/hijack/并发停机待验 |
| 正常回归 | 关联六包 race / 54 test pass 事件、0 fail/skip，vet 通过 | 不包括未修的新反例，不推定全模块覆盖 |

新增 17 个具名叶子/独立项 = 11 fail + 5 正常 pass + 1 仅观察。Manager/Admin/Ops 源码未改。图谱 09-30 generation、metadata_changed / 新附件 not_tracked 以当前源码补证，没有重启共享索引。

下一入口 **N02 security/gateway/正式 Webroute**，同行补 N01 具名状态/阻塞/故障剩余项；用户说未修时继续新范围。其余 14 单元不因本轮回归标完成，15 单元计划仍为有界源码阶段计划。10-03 的 54～92 有效小时只是初估，需在 N02 补齐后按实际新主链重新校准，不能从 15/15 阅读或 bug 数算总覆盖率。

## 2026-10-04 非三大核心第一批修复

[本轮修复运行](REVIEW-2026-10-04-noncore-01.md) · [四项 bugfix](../bugfix/README.md) · [红/绿与复跑](../bugfix/evidence/noncore-bugfix-20261004-01/README.md)。起点 `183b0bdd`。RR-20261003-NC-01～04 **4/4 已修，声明场景验证，未发版**；下方 10-03 “未修”保留历史。

正式新增 45 个叶子/独立场景通过；原 review 30 场景原文通过；最终 11 个关联包 race、194 个 test pass 事件、0 fail/skip，同包 vet 通过。源码阶段与修复状态分开：N01/N02 仍部分完成，继续 lifecycle/manager/admin/health 与 Kit Ops，再接 security/gateway。没有因为回归通过将整域升级为审完。

## 2026-10-03 非三大核心第一批：App / HTTP 与剩余计划

[本轮运行](REVIEW-2026-10-03-noncore-01.md) · [四项未修 P2](../bug/REVIEW-2026-10-03-noncore-01.md) · [机制学习](IMPLEMENTATION-APP-AND-HTTP-BOUNDARIES.md) · [15 单元完成计划](NONCORE-REVIEW-PLAN-2026-10-03.md) · [复跑/清单](evidence/noncore-review-20261003-01/README.md)。源码基线 `746567ff`。

用户说明 Nest/Sync/DataEngine 已由另一线基本跑过，本线接续其他模块。当前清单 783 个 Go 候选，其中核心与紧邻共享边界 155，其他 628 映射 15 单元；不是未读数或逻辑覆盖率。历史 Service 十域阶段完成证据、10-01 Service/Codegen 独立复审复用；下方 09-30 停点保留为历史，不遗漏更新的文档。

| 范围 | 本批实证 | 当前状态 / 下一入口 |
| --- | --- | --- |
| N01 App 主链 | 5 个生产文件读取；12 单 Mod/helper/真实 Execute 场景，7 fail/5 pass | NC-01 未修；N01 部分完成，接 lifecycle/manager 注册/启动/停止竞争、admin/health |
| N02 HTTP 接入 | 两主文件完整读取；client 11 场景 3 fail/8 pass、server 7 场景 2 fail/5 pass | NC-02～04 未修；N02 部分完成，接 security/gateway/正式 Webroute |
| 关联包回归 | 7 包 race、123 test pass 事件、0 fail/skip；同包 vet 通过 | 仅回归证据，manager/lifecycle/security 等不因此计为源码审查完成 |
| 其他 13 单元 | 旧证据关联、当前 blob 清单和分阶段预算已归档 | 尚未执行本轮新增主链；按计划接续，不自动计算完成百分比 |

30 具名新场景/控制 = 12 失败反例 + 18 通过控制，归为四个根因；七包回归全绿没有覆盖这些边界。图谱 generation 09-30、freshness metadata_changed，当前源码和 overlay 补证；不称整个模块图谱/源码同代闭环。

最新 Wanted 10-01 四条已有 RR-06～09/bugfix 去向，本轮未重验，也未重复登记。剩余源码阶段与本机验证初估 54～92 有效小时，每天 4～6 小时约 9～23 投入日；完整假设、日期窗口和批次见计划。bug 修复、外部 HA/长稳另外验收。下轮从 N01 的 lifecycle/manager 接着读，用户表示未修时直接继续新范围。

## 2026-09-30 Codegen 第六轮：RR-CG-12～14 修复验收

[运行/停点](REVIEW-2026-09-30-codegen-06.md) · [三项修复](../bugfix/README.md) · [原始证据](../bugfix/evidence/codegen-bugfix-20260930-12-14/README.md)。起点 `4784ef82`，源码修复收口 `2f68aa22`。用户本轮明确 bugfix；三项原触发与具名邻接场景已验，未发版，下方第五轮“未修”保留历史。

| 范围 | 本轮实证 | 剩余边界 |
| --- | --- | --- |
| cfggen 索引/namespace | 18 个新增叶子中的 12 项配置消费/拒绝控制；四索引场景、五保留名、三正常 bean；全包 race | required/ref/skipempty 与真实 JSON/索引往返仍未执行 |
| deps 自动合仓/输入所有权 | 六项正式成功/失败/并发场景、既有只提交模块保护；真实 CLI get/tidy 后 Go/manifest/模块对账，生成工程编译 | 不等于所有显式升级版本消费者/强杀/磁盘/rollback 失败验证 |
| 相邻生成消费 | 正式 DAO → Entity → periodic/on_change Sync，native Go 等价脚本链 race | 不是完整业务进程运行或真实外部资源矩阵 |
| 全 Codegen/静态 | 全 Codegen race/vet、glsvet 通过；具名 shell 项跳过 | sh/shellcheck PATH 工具限制；发布 tag 未含新修复 |

RR-CG-12～14 按本表范围关闭；整个 Codegen 仍有下方具名未审项，继续入口是配置运行期、显式 upgrade 消费者、正式 Webroute 启动。新测试 graph coverage not_tracked、旧路径 metadata_changed，当前源码和执行补证，不称图谱全域同代。

编号映射：第五轮原 Codegen RR-12/13/14 → RR-CG-12/13/14，避免与并行 Nest/Entity 记录重号；原报告和证据保留。正常整合远端 `d4ef0a3b` 至 `56d4b805`，重新构建 CLI、迁移新工程与 DAO/Entity 双模式消费者，均通过；生成 compose 的 Docker 运行检查未执行。完整收尾结果见第六轮记录及 final-* 日志。

收尾版本 `05f0dc3b` 又整合了 `9f7d0dc5`。临时 22/23/24 与 game-demo 重号，因此 Codegen 最终完整编号为 RR-20260930-CG-12/13/14，映射见规范 bug 记录。补跑全 cfggen 与 demo/依赖/consolidation 定向 race、静态检查、当前 Core 生成消费者及双模式 Sync 全通过；对应 cg-final-* 日志。game-demo 独立 RR-20260930-22～24 仍按另一工作线的未修状态管理，不计入本轮 3/3。

## 2026-09-30 Codegen 第五轮：cfggen 与依赖自动迁移

[运行/停点](REVIEW-2026-09-30-codegen-05.md) · [三个新 P2](../bug/REVIEW-2026-09-30-codegen-05.md) · [实现学习](IMPLEMENTATION-CFGGEN-NAMESPACE-AND-DEPENDENCY-MIGRATION.md) · [复跑](evidence/codegen-review-20260930-05/README.md)。基线 `30085d62`，仅文档审查。上轮五项修复实现无新变化，Codegen 包回归通过；RR-12～14 已复现、未修。

| 范围 | 新增覆盖/实际执行 | 状态与下一入口 |
| --- | --- | --- |
| cfggen 导出/校验/类型/名字/accessor | 当前主文件源码与图谱定位；false、true、无 index、两个 bean 冲突正式 CLI/独立消费包 | 部分场景已验证；RR-12/13 未修；继续运行期 ref/required/skipempty 与索引值往返 |
| cfggen 分组和复合 bean | server target；嵌套值 bean、切片递归、int/bool/string 索引、global；编译且 client 表/字段省去 | 正常样本通过；不计作真实 JSON 加载/所有 group 组合已验 |
| 依赖 stage 到 root、自动 consolidation | 事务/迁移/CLI 当前源码；注入 resolver 的成功漏回写反例、失败 root 前像控制 | RR-14 未修；真实代理/tag 解析未验；显式 upgrade/dry-run/并发变化下一轮 |
| 正式包/并发回归 | `go test ./codegen/...` 跳过具名 shell 环境项；cfggen/依赖/consolidation 定向 race | 两组通过；不把既有测试绿当作新三个问题关闭 |

四个预期失败反例与四个通过控制对应具名场景，不换算为全域覆盖百分比。已补上一轮“cfggen 复杂 schema/升级分支”的部分缺口；Codegen 整体仍未收敛。图谱刷新超时、freshness metadata_changed，源码与实跑补证；未确认同代图谱全覆盖。仍待正式 Webroute 服务启动、真实配置消费、升级消费者、强杀/磁盘故障与线上兼容。

## 2026-09-30 Codegen 第四轮：五项修复与消费端验证

[本轮运行与剩余边界](REVIEW-2026-09-30-codegen-04.md) · [RR-06～10 修复](../bugfix/README.md) · [现有生成机制更新](IMPLEMENTATION-CODEGEN-STAGING-AND-RETIREMENT.md)。Core 基线 `da56c998`；Codegen 主入口盘点沿用第三轮矩阵。本轮五项原触发 **5/5 修复并在声明场景验证**；未发布，不等于 Codegen 全域完成。

| 范围 | 本轮实证 | 下一入口 |
| --- | --- | --- |
| Attribute/Event/Webroute 退役 | 零输入、部分 receiver、手写保护及失败前保留正式回归；定向 race | 正式 Webroute 服务启动/旧 URL 消失；事件协议窗口 |
| Tablegen JSON 与 roost 暂存 | v2 所有权、改动拒删、v1 不明归属报错；正式暂存/`--check`；外部业务工程两次编译及漂移→修复→转绿 | 旧 v1 项目人工归属判定、实际配置加载/版本迁移 |
| Errcode | 注释、块注释、字符串误提取正式回归、重复码旧回归 | 非字面量定义仍按原契约不入 CSV |
| 之前验证缺口 | 独立模块缓存+本机 file proxy+Go 1.27 解决上轮 `go mod tidy` 缓存写锁；真实 `go test ./...` 通过 | cfggen 组合、upgrade、多故障强杀/磁盘与客户端兼容仍待审 |

图谱起点同 `da56c998`，但候选路径 freshness `metadata_changed`；新修改主要由源码和执行证据证明。下方第三轮“未修/外部编译受阻”均为历史时点。

## 2026-09-30 Codegen 第三轮：主入口盘点，整体未收敛

[运行及各生成器矩阵](REVIEW-2026-09-30-codegen-03.md) · [RR-04/05 修复](../bugfix/RR-20260930-04.md) / [Nest](../bugfix/RR-20260930-05.md) · [新 RR-06～10](../bug/REVIEW-2026-09-30-codegen-03.md) · [复现/限制](evidence/codegen-review-20260930-03/README.md)。Core `5fedc652`、Kit `f0e5b67a`、独立 Codegen `1e028fa4` 拉取核对；实际生成器在 Core `codegen/`。RR-04/05 **2/2 原触发修复并按声明场景验证**；新四项 P2、一项 P3 未修。

| 本轮范围 | 已执行 | 仍待完成 |
| --- | --- | --- |
| Entity/Nest 删除最后标记 | 修前正式用例红；修后零标记、同包首实体、`-sender=false`、手写保护及暂存提交测试通过；定向 race 通过 | 真实外部工程 `go mod tidy` 被本机模块缓存权限挡住，消费者编译/部署另验 |
| Attribute/Event/Webroute/Tablegen 输入退役、Errcode 误提取 | 五个隔离 CLI 反例、当前源码与图谱入口调用链；RR-06～10 分别登记 | 五项尚未实施；Event 部分 receiver、启动路由、配置加载消费者需动态验 |
| Codegen 主入口盘点 | DAO、RPC、Protocol、Entity、Nest、registry、Attribute、Event、Webroute、Tablegen、cfggen、errcode、roost 暂存顺序均归位，详见矩阵 | 不是逐行/分支审计；cfggen 复杂 schema、脚手架/升级、进程强杀/磁盘故障和版本兼容未收敛 |

图谱覆盖 generation 为 `2026-09-29T14:50:03Z`、相关路径 freshness `metadata_changed`；最新源码和 CLI/正式测试补证。下方第二轮“RR-04/05 未修”保留历史时点。

## 2026-09-30 Codegen 第二轮：修复两项并继续退役审查

[运行/停点](REVIEW-2026-09-30-codegen-02.md) · [RR-01 修复](../bugfix/RR-20260930-01.md) · [RR-02 修复](../bugfix/RR-20260930-02.md) · [新 RR-04/05](../bug/REVIEW-2026-09-30-codegen-02.md) · [复跑](evidence/codegen-review-20260930-02/README.md)。Core `f6566d4e` 基线，Kit/独立 Codegen 无新源码；工作在 Core `codegen/`。原两项 **2/2 修复/声明场景验证**，生产发布/部署未做；新两项 Entity/Nest 退役缺陷未修。其他 agent 的 RR-20260930-03 是独立 Cache/长稳问题，本轮未触碰。

| 本轮 Codegen 范围 | 已执行/状态 | 仍待完成 |
| --- | --- | --- |
| servicerpc 零标记、跨包半边 | 修前红；修后 CLI `-check` 1、生成清理 2/2，手写/其他源保留测试，定向 race 绿 | 不同字面路径命令、实际 Kit 消费编译/发布版本迁移 |
| protocol 空定义、handler、上层暂存/检查 | 修前红；修后 CLI 清理 4/4、正式包空/部分/手写/失败回归，`SyncProject` 删除 4/4 与 `-check` 两项漂移，定向 race 绿 | 自定义输出路径在上层规划、强杀/磁盘故障、线上旧消息 ID 兼容 |
| entity → registry | 源码与当前 CLI 生成→删 marker→重跑；旧 wire 留存且仍有注册 marker，RR-20260930-04 未修 | 正式项目 aggregate/消费者与多实体改名、guard test |
| nest wrapper/sender/bootstrap | 源码与当前 CLI 生成→删 marker→重跑；旧四文件留存，RR-20260930-05 未修 | 正式 game bootstrap/消费者、`-sender=false`、源文件改名 |
| 其它生成器 | registry 按扫描 marker 重写 aggregate 源码已读；Codegen 包回归完成 | event、table/config、webroute/errcode 的同类退役与交叉生成 |

图谱基础 generation 在本轮工作树改码前已落后两个纯文档提交，所有物质结论均回读当前源码，修改后的新文件不宣称在旧图中。旧第一轮下方“未修复”为当时结论。

## 2026-09-30 Codegen 第一轮：当前 Core 生成链与输入退役

[本轮审查](REVIEW-2026-09-30-codegen-01.md) · [两项未修复问题](../bug/REVIEW-2026-09-30-codegen-01.md) · [实现学习](IMPLEMENTATION-CODEGEN-STAGING-AND-RETIREMENT.md) · [隔离复现](evidence/codegen-review-20260930-01/README.md)。三仓 fetch 后不变；Core `1b7a2fc5`、Kit `f0e5b67a`、冻结独立 Codegen `1e028fa4`。实际入口为 Core `codegen/`。上一轮 Service 本机场景已留档，外部真实资源/跨机 HA/生产长稳仍另待环境，不把本轮 Codegen 回归冒称 Service 验收。

| Codegen 范围 | 当前证据与状态 | 未覆盖/下轮入口 |
| --- | --- | --- |
| roost 暂存提交链 | 源码读 `GenerateTransactional`、input snapshot、commit planner/guard/rollback；相关既有回归。已验证部分场景，无新增本链确认 bug | 进程强杀、磁盘失败、跨文件提交与旧生成物归属 |
| servicerpc 输入/检查/双半 | 源码、现有测试、隔离生成→删 marker→`-check`；[RR-20260930-01 P2](../bug/REVIEW-2026-09-30-codegen-01.md) 未修复 | 改名、跨包 `-out` 双半孤儿处理、消费者注册/构建 |
| protocol 定义与固定输出 | 源码、隔离生成→删最后定义；[RR-20260930-02 P2](../bug/REVIEW-2026-09-30-codegen-01.md) 未修复 | 单条消息/handler 退役、协议兼容窗口、生成项目消费 |
| dao 退役对照 | 源码和既有孤儿回归；这里只核对差异，未对 DAO 做本轮新故障枚举 | 定义变化/损坏输出/并发故障仍未专项 |
| 其它生成器 | `go test ./codegen/... -skip TestDeployScriptsCarryNoKnownShellcheckFindings` 全部有测试包通过；尚未开展相同深度的源码/动态审查 | entity/nest、event/registry、table/config、webroute/errcode |

图谱 Core HEAD 对齐，但相关路径 coverage 为 `metadata_changed`；已读当前源码补证。以上是本轮有界源码与场景覆盖，不是 Codegen 全量/行覆盖率。具名 shell 静态检查因本机无 `sh` 未通过，单独记录；其余包回归通过，详见[本轮运行记录](REVIEW-2026-09-30-codegen-01.md)。

## 2026-09-30 Service 第十三轮：真实代理故障与持续热点

[审查结论](REVIEW-2026-09-30-services-13.md) · [机制学习](IMPLEMENTATION-SERVICE-FAULT-AND-CONTENTION.md) · [复跑/日志校验](evidence/service-review-20260930-02/README.md)。Core `d91d8a30`、Kit `f0e5b67a`、Codegen `1e028fa4` 三仓 fetch 后 HEAD 未变。**本轮无新增确认生产 bug，生产代码未改。**Service 原 10/10 域主链阶段整理维持；新增的是下列有界场景，不改称生产故障全验收。

| 上轮专项缺口 | 本轮新增实测 | 仍未完成 |
| --- | --- | --- |
| Toxiproxy 3 skip | 真实代理下 3 test × race/count2 = 6 pass/0 skip；Session/Match CAS 写后丢回复各 1 pass，读回/同身份重试稳定 | 完整双向分区、服务间 RPC 与实际资源系统 |
| 单 owner、带 `WAIT` HA | 无 `WAIT` 脚本强杀 owner 1 master，六 owner 跨槽 seed/独立 verify 各 1 pass，副本接管后全部回收 | 不证明写入在强杀时未复制；跨机/分区和丢失已确认写的业务对账 |
| 测试侧内存轮转 250 owner | Redis 持久游标跨三个 Go 进程：99/100/50/1 完成，250 效果各 1，3 pass | 部署用 roster/游标、真实读断线与孤儿 owner 对账 |
| Match 短串行样本 | 4096 旧终态、四客户端 60.371 秒 443 对完成、209 冲突重试、Waiting=0；样本 p99 3.529 秒、Go TotalAlloc 59.46 GB | 归档未实施；目标 SLO、生产并发、跨天长稳未验证 |

测试二进制按 Toxiproxy 官方 Go 模块在工作区临时目录构建；独占 Redis/代理均核对归属后关闭。上轮“Core 下缺数据环境脚本”只指 Core 根路径；`kit/scripts/integration/` 实际提供接线脚本。以下是各批原时点快照。

## 2026-09-30 Service 第十二轮：本机故障与容量专项

[本轮审查](REVIEW-2026-09-30-services-12.md) · [机制学习](IMPLEMENTATION-SERVICE-CRASH-HA-AND-CAPACITY.md) · [探针与结果](evidence/service-review-20260930-01/README.md)。Core `88c65286`、Kit `f0e5b67a`、Codegen `1e028fa4` 拉取并核对；原 Service 10/10 域主链阶段进度保持，不把本轮四类小专项改称全生产验收。**无新增确认生产 bug；未改生产源码。**

| 专项分母 | 本轮已执行 | 尚未验收 |
| --- | --- | --- |
| 跨进程强杀 | standalone/Cluster 各 2 次 `-race` 通过；真实子进程退出、后续新实例完成；模拟资源回调 2 次/持久效果 1 次 | 实际 allocator/scene/资产资源的回执、幂等及对账 |
| Redis HA | 本机 3 主 3 从，1 个负责测试 key 的 master 强杀、`WAIT 1=1`、副本提升、独立进程 verify 通过 | 跨机器/分区、普通未 `WAIT` 写、写入未知结果、多 owner 故障交错 |
| Backlog 与 Match 容量 | 250 owner 轮转及单次读故障，4 页 99/100/50/1 清空；Match 64～16384 旧记录/16 对及 8192 旧记录/128 对短样本通过，测得显著增长 | 持久 owner 来源接线、Match 归档实施、并发/长稳及业务 SLO |
| Toxiproxy 物理网络故障 | 3 test skip / 0 pass，环境尚未接线 | 丢回复、延迟、分区恢复 |

首次 HA fixture 因 1 分钟 TTL 超时和错误断言失败两次，结果、校验值和纠正后通过的第二轮均保留；这些失败不计作框架 bug。七个本轮独占 Redis 端口已经核对目录后关闭。下方各批记录是当时快照。

## 2026-09-29 第九批bugfix / Service第十一轮：阶段完成

**10/10功能域主链及具名本机专项完成有界审查。**[本轮](REVIEW-2026-09-29-services-11.md) · [最新矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md) · [学习](IMPLEMENTATION-SERVICE-PIPELINE-AND-REVIEW-CLOSURE.md)。RR-34 **1/1修复/声明场景通过**；RR-20260929-01～34按各批原验收关闭，旧残余沿证据单列。本轮无新增确认缺陷，不声称全框架或全部生产逻辑无bug。

| 口径 | 最新事实 | 未计为完成 |
| --- | --- | --- |
| 源码范围 | 100生产路径（82非生成/18生成）100blob不变复用；共享Redis改码/维护入口专项复读，12RPC check通过 | 不是逐行/分支或执行覆盖率 |
| 本轮修复 | 原standalone/Cluster反例与生命周期3/3绿；正式两后端race/count2 58次叶子执行通过 | 物理丢包/跨owner全局顺序/HA不由hook证明 |
| 当前回归 | 19测试包1035pass事件/951pass叶子；Core/consumer编译、vet通过，0fail/build-fail | 3Toxiproxytest skip；无测试package skip1另计 |
| 后续 | Match归档、购买fulfilled/drain、真实资源回执、旧数据迁移、HA/强杀/长稳具名交接 | 方案未实施/真实环境未执行，不泛化为service主链未安排 |

下方未实施/FAIL保留历史时点，不重新打开已通过声明场景的旧根因，也不升级其他核心域/性能事项。

## 2026-09-29 第八批 bugfix / Service 第十轮

实现/主体文档已推送28f8fc15，干净主树同步；graph full generation14:09:57Z ready，新正式测试已纳入。137路径仍135 metadata_changed/两个go.txt nottracked，[后补证据](evidence/service-review-20260929-10/COVERAGE-AFTER.json)保存限制和源/执行依据。四个隔离Redis核验目录后关闭，共享MCP保留。

基线68cf87fc，fetch无远端改动；[RR-33 1/1修复/声明场景验证](../bugfix/SERVICE-BUGFIX-2026-09-29-08.md)，新[RR-34 P2 Pipeline误报成功](../bug/REVIEW-2026-09-29-services-10.md)仅交接未改driver。[运行](REVIEW-2026-09-29-services-10.md) · [实现学习](IMPLEMENTATION-SERVICE-MAIL-BATCH-AND-PIPELINE-ERRORS.md)；提交/最终图谱见该轮交付追加。

| 进度分母 | 当前事实 | 不计为完成 / 下一入口 |
| --- | --- | --- |
| Service10域主链 | 10/10本阶段有界整理；100生产路径97blob复用第九轮/3Mail变化复读；共享Redis/cache错误与生命周期补查 | 不是逐行/分支或全故障正确率，不重开全域扫描 |
| 上轮RR-33 | 原2叶子红绿、Mod16封跨3owner两页、读取边界/错误传播已验证 | 自定义客户端Pipeline接线、所有读owner升级；未部署 |
| 本轮新缺陷 | RR-34两真实后端反例fail、复用/Discard控制pass | 下次bugfix在driver检查全部Cmder错误 |
| 本机回归 | Mail count2/race314pass事件280叶子0skip；18包965pass事件888叶子0fail；Core/consumer编译、vet、RPC12/12 | 3Toxiproxy test skip、servicemetrics package skip1；外部渠道資源/HA/強杀/長稳未验 |

Wanted本基线无新活动候选；Match历史、购买fulfilled/归档、资源幂等沿第九轮设计入口，未改变验收。下方旧“RR-33未修”为历史。


## 2026-09-29 第七批 bugfix 与第九轮：service 本阶段专项收口

实现/主体文档已推送 `004c8cf8`，干净主树同步；graph full更新成功，ready/新正式测试命中、generation13:13:03Z。124路径仍有metadata_changed/两个go.txt未纳入的限制，[后补证据](evidence/service-review-20260929-09/COVERAGE-AFTER.json)保留源码与执行依据。四个独占Redis已核验目录后关闭，共享MCP保留。

基线 bcebb805 加本轮实现。**RR-31/32 2/2 已修/声明场景已验；新 RR-33 P2 Mail Cluster 分页未修。**[修复/兼容](../bugfix/SERVICE-BUGFIX-2026-09-29-07.md)、[新问题/实施方向](../bug/REVIEW-2026-09-29-services-09.md)、[运行](REVIEW-2026-09-29-services-09.md)、[机制与归档方案](IMPLEMENTATION-SERVICE-FINAL-SPECIALTIES.md)。下方旧“0/2未修”保持历史时点。

| 完成口径 | 当前状态 | 剩余边界 |
| --- | --- | --- |
| Service主链范围 | 10/10功能域本阶段有界整理；100生产路径核算，95blob不变复用/5变化复读；18生成文件按12check核验 | 不是逐行/分支/全故障正确率，不升级其他核心域 |
| 本轮具名专项 | Activity配置/多game部分完成恢复；购买首内容/未知回复/消费归档边界；Mail跨槽批读/Pipeline候选；Match历史成本；生成装配均有源码/实际证据 | 本机专项审查收口，不是方案已实施或外部验收完成 |
| 第八轮两项缺陷 | RR-31/32 2/2修复，原断言8/8绿、新正式33叶子通过 | 旧prefix迁移、旧业务producer升级、历史被覆盖goods恢复未执行 |
| 新缺陷 | RR-33真实反例1fail、tagged/Pipeline控制通过 | 下一次service bugfix入口；Mail生产未改 |
| 回归/消费 | integration+race17包/909事件/841叶子，test skip0；consumer107事件/100叶子；demo5/5、RPC12/12、全Core/consumer编译、定向vet绿 | 实际渠道/asset/allocator、HA/強杀、长稳/生产迁移未执行 |

不再留“其余service尚未安排”的泛化待办。后续优先RR-33，归档/fulfilled/资源回执按具体设计入口推进；缺少外部环境的不计已验证。[最新矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md)、[路径/样本/限制](evidence/service-review-20260929-09/README.md)。

## 2026-09-29 第六批 bugfix 与 Service 第八轮

代码与主体文档已提交推送 `5f81e5c8`。图谱 full 更新成功，最终 ready/HEAD 对齐、generation11:18:53Z，三个新正式测试已纳入；29证据路径仍metadata_changed，源码补证与partial边界见[刷新结果](evidence/service-review-20260929-08/COVERAGE-AFTER.json)。四个独占Redis实例已关闭，未停止共享MCP。

基线 db642494 加本轮工作树修复，[源文件摘要](evidence/service-review-20260929-08/SOURCE.json)标识具体内容。**RR-28/29/30 三项 3/3 已修复、原场景验证；新 RR-31/32 两项 0/2 实施。**[修复/兼容](../bugfix/SERVICE-BUGFIX-2026-09-29-06.md) · [新问题](../bug/REVIEW-2026-09-29-services-08.md) · [运行](REVIEW-2026-09-29-services-08.md)。

| 增量专项 | 已完成源码/实际场景 | 停点与下轮入口 |
| --- | --- | --- |
| Platform 原子缺失退休 | 新 versionstore Lua/接线；原 overlay15/15，正式屏障/重建；两后端空坏字节和执行前/后错误重试 | RR-28 原触发关闭；旧丢 pending 对账未实施 |
| Rank/Platform Cluster | 首 tag 共享工具、配置拒绝；三 master 合法生命周期 | RR-29/30 原触发关闭；直接构造器/跨 slot 分片与旧 prefix 迁移 |
| Activity Cluster | 4 坏配置 actual complete/dispatch=false/CROSSSLOT；tagged Open→Notify→Owed→Attempt→Ack 控制通过 | RR-31 缺启动校验，新发现未改码 |
| 购买 producer→drain→strict handler/ledger | 模板全读、生成源码核对；旧 seed/control=10，新 catalog 二进制重试覆盖为3 | RR-32 固定首次 grant；fulfilled 归档协议、HGetAll/ledger 容量 |
| Mail 存储边界 | 单 key Mailbox/SendLedger 不能按缺 tag 归 bug | 跨槽 MGet 页未新增实跑，不做无问题结论 |

正式定向43叶子/51事件、完整17包/826事件/764叶子均通过，test skip=0；servicemetrics 无测试包 package skip=1。新 review8 次叶子3 pass/5反例fail/0 skip/build-fail。全仓编译/四包vet/既有生成消费通过。10/10 域主链有界整理沿既有证据保留，本轮不重置进度、不将测试数当覆盖率或全 service 无 bug。29路径图谱检查含metadata_changed/三个新增正式测试missing，源码补证；旧K1/K2/K3与资金/HA/强杀/长期容量状态不变。下方保留此前时点。

## 2026-09-29 Service 第七轮：维护竞争与 Cluster

源码 **1625fee0**；[问题/实施交接](../bug/REVIEW-2026-09-29-services-07.md)、[运行证据](REVIEW-2026-09-29-services-07.md)、[索引与 Cluster 学习](IMPLEMENTATION-SERVICE-INDEX-MAINTENANCE-AND-CLUSTER.md)。继续第六轮专项而非重扫 100 路径。新增 **RR-28 P1、RR-29/30 P2，0/3 修复**；原第五批 4/4 沿已验收证据保持关闭。

| 已查范围 | 源码/实际场景 | 当前停点与下一入口 |
| --- | --- | --- |
| Platform pending→缺失→Retire / malformed→Defer | 真 Redis 两个迟到创建/对象重建反例；ghost、正常 paid、终态不复活控制通过 | RR-28 条件退休原语与接线；历史索引对账未执行 |
| Platform/Rank Mod→driver→Cluster 多 key 脚本 | 三 master、16384 slots；4 个坏前缀反例；2 个有效 tag 生命周期控制通过 | RR-29/30 有效共同 tag 启动校验；复制/failover/完整 app 网络未验 |
| Rank Reset/Remove→在途 Add→同键重放 | 2 个屏障控制通过，旧 base=100 不被带回；实际结果 5 | 不推定封季/fence，复杂长期竞争未穷尽 |
| Match terminal/Requests/Sweep/状态体积 | Memory/Redis 各 64 Cancel、一年后仍保留 23084-byte 状态；重放返回原终态 | 已有容量观察不重复 RR；聚合压力/归档与购买 ledger 为后续纯 review 入口 |

当前 15 叶子场景 9 pass/6 预期 fail/0 skip/build-fail；无 overlay 四包 268 测试及子测试 pass。100 路径/10 域主链有界整理保持完成，不把本轮专项等同全 service 已无 bug。图谱代际08:12:07Z，34 证据/复用路径 metadata_changed，关键源码补证；K1/K2/K3 与 HA/强杀/真实资金/长稳专项状态不变。

## 2026-09-29 Service 第六轮：本批缺陷收敛

源码 **4b0837d7**；[修后邻接 review](REVIEW-2026-09-29-services-06.md)、[最新矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md)、[当前机制](IMPLEMENTATION-SERVICE-BOUNDS-AND-RECOVERY-CLOSURE.md#第五批修复后的实际实现)齐全。本批 RR-25/26/27 与旧 RR-14-02 残余 **4/4 原触发/声明场景通过**，列明邻接范围无新增确认缺陷。10/10 域主链整理已收口；当前 100 生产路径，6 变更源码及回归复查、94 blob 不变复用，不能称行/分支覆盖率。

| 本轮闭环 | 实际执行/结论 | 后续具名边界 |
| --- | --- | --- |
| Match / Rank | Group 非法与空候选、合法 2/64；加法溢出拒绝/同键合法重试/并发重读，全过 | Match 终态历史归档、热点榜/队列容量 |
| Chat | 中间/尾部年龄洞、limit、正反/空页/游标/Gap、Requests、计数；Memory/Redis 全过 | 真实入口限流、所有读取/清理 owner 升级 |
| Activity | 慢 Create/满容量；admit/Create/confirm 写后丢回复；持久计划重建；旧 257 窗口有界轮转跨重建；legacy 同 key 恢复，全过 | legacy 无计划/遗忘 orphan 对账；真实 game/HA/强杀 |
| 基础 / 消费 | 完整 16 包/830 事件 pass，最终两模式各 27 叶子/32 事件全绿；编译/vet/受影响生成检查/已有生成消费通过 | allocator/支付、断网、多进程、长稳性能未验证 |
| 图谱 | full 刷新等待未得到完整返回；后续 coverage 看到 08:12:07Z 新 full generation、新测试已纳入 | 11 路径仍 metadata_changed，源码补证；不据 scope 干净证明完整 |

本阶段可以转入具名专项或其他核心域，不重复 service 第一轮。未操作生产升级/迁移/发布；K1/K2/K3 原专项状态不因服务回归绿自动改变。下方保留第五批实施与此前 review 时点。

## 2026-09-29 Service 第五批 bugfix

基线 cec1dd30；RR-25/26/27 与旧 RR-20260914-02 的本轮残余 **4/4 已实施、原触发和邻接恢复回归通过**。[决策、兼容与执行记录](../bugfix/SERVICE-BUGFIX-2026-09-29-05.md)。16 包/830 测试及子测试事件通过；随后补充两个写后丢回复场景，最终定向两模式各 27 叶子/32 事件全过。全仓编译、四包 vet、Chat/Activity 生成检查与生成消费者通过。Activity/Chat owner 升级、legacy 意图、真实外部系统边界保留；继续修后 service review，不因 4/4 关闭所有专项。

## 2026-09-29 Service 第五轮：本阶段主链收口

source b336ce62；第四批 3 项修复已推送。随后完成[第五轮新内容](REVIEW-2026-09-29-services-05.md)、[10 域矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md)和[机制学习](IMPLEMENTATION-SERVICE-BOUNDS-AND-RECOVERY-CLOSURE.md)。**10/10 域的列明主链/存储/组装契约已整理；100/100 当前生产路径已归类，不是逐行或测试覆盖率。**57 路径复用未变源码证据、25 非生成变更路径复读、18 生成文件当前检查。

新增 RR-25/26/27（2 P2、1 P3），旧 RR-20260914-02 的迟到 Opening 残余 P2，均未实施；[交接](../bug/REVIEW-2026-09-29-services-05.md)。新 21 叶子 × 两模式 = 42 次执行，18 pass / 24 预期 fail / 0 skip，最终无 build-fail/race 警报；12 次 RPC 检查通过。原 16 测试包/801 事件回归基线通过。

| 本轮推进 | 已读 / 已执行 | 状态及具名剩余范围 |
| --- | --- | --- |
| Account / Directory | 发布门禁、失败关闭准入、同计划重建、名字 owner；Account Memory/Redis 新控制通过，Directory 条件删除已有正式证据 | 主链整理完成；legacy 孤儿/真实强杀与自定义后端专项未验证 |
| Global | 旧 Acquire 与迁移/new lease 重叠，Memory PASS | 主链完成；实际业务 fence/双进程/HA 未验证 |
| Activity | grace 回收、确认、满容量、过期扫描，Memory/Redis 反例成立 | 主链完成；RR-14-02 残余待修，超容量旧数据恢复/真实 game 专项 |
| Mail / Platform | 第四批未知效果/回包/删除证明已修；正式回归、生成购买消费工程与 split Attempts 接入 | 主链完成；实际资产/支付对账、在途删除产品契约、旧数据专项 |
| Match / Rank | 公开 Grouping 非法大小、历史 churn；Rank 正负溢出与合法边界真实 Redis | 主链完成；RR-25/26 待修；历史归档、房间分配、压力/公平专项 |
| Session / Chat | 3 页轮转来源清理、并发 Releaser 2 calls/1 fixture effect；Chat 去重及 2 秒偏移 Memory/Redis | 主链完成；RR-27 待修；真实 allocator、Gap/年龄契约、入口限流专项 |

源码整理、场景验证、缺陷修复独立计数：本轮新问题 0/4 修复；真实 allocator/HA/断网/强杀/负载未验证，不计已完成。下一入口为该四项 bugfix 或矩阵的具名专项；不要从头重复 service 主链。K1/K2/K3 原进度保留，本轮服务收口不代替其专项证据。

## 2026-09-29 Service 第四批 bugfix 完成

RR-23/24、旧 RR-10 删除残余 3/3 实现并通过定向回归；16 包/801 测试及子测试 pass、0 测试 fail/skip；生成工程消费链及全仓编译通过。[升级与下一入口](../bugfix/SERVICE-BUGFIX-2026-09-29-04.md)。随后继续 review 新范围，下方保留历史停点。


## 2026-09-29 Service 第四轮停点

源码 `bdbb61bc`，fetch 后 origin/main 相同；[运行](REVIEW-2026-09-29-services-04.md) · [问题](../bug/REVIEW-2026-09-29-services-04.md) · [学习](IMPLEMENTATION-SERVICE-EXTERNAL-OUTCOME-AND-DISPOSAL.md)。新增 **RR-23 P1、RR-24 P3**，旧 RR-20260910-02 追加未领取删除残余 P2，均未实施。上一轮四项当前源码/正式回归复核通过，不扩大为全故障窗口独立验收。

| 域（本轮 SHA bdbb61bc） | 已读/执行与当前状态 | 下轮未完成入口 |
| --- | --- | --- |
| Platform | recordFailure/admin/回调输出/Mod/index/生成发奖；超时退款和重复发货 × 两后端；输出别名 Memory FAIL/Redis PASS；RR-23/24 | 外部结果分类与收据；真实 purchase grant 消费及进程重建 |
| Mail | Delete/Commit/evict/Deliver/Reserve；删除复活 × 两后端；在途 Commit 淘汰前后观察；旧 RR-10 残余 | 有效删除证明及容量；在途删除业务契约、实际奖励回执 |
| Match | 单 queue CAS、ticket/match 读回、分组、expiry；写前/写后错误 × 两后端共 4 叶子 PASS | 正式 matchmaker → 房间资源幂等分配；历史容量与长期公平性 |
| Session | Finish/Release/markReleased/claim 和 demo collaborator 精读；已有回归复跑 | 实际 allocator 缺证据；不能称 double-free 已确认 |
| 证据 | 新选定 16/16 叶子执行，7 安全反例 FAIL/9 对照观察 PASS；现有 16 包/794 事件全过；补 Redis 2 包/15 事件全过 | 未跑真实 Broker/资产/HA/断网/强杀/负载；不是代码审完百分比 |
| 图谱 | 新 generation 05:34:58Z 发布，full RPC 本身超时；search/双向 trace/snippet/coverage 后当前源码补证 | metadata_changed、方法遗漏与错边限制保留；无全 service 完整性声明 |

既有 10/10 服务域主链有记录的范围数不变。本轮向四域邻近路径推进，不回头重复主链；用户声明未修复时跳过旧验收。下方完整保留第三轮修复与历史停点。

## 2026-09-29 Service 第三轮 bugfix 停点

基线 `23f92d73`，本批 RR-20260929-19～22 **4/4 已实施并通过原触发回归**；[逐项实现、兼容与验证](../bugfix/SERVICE-BUGFIX-2026-09-29-03.md)。16 个包通过、794 测试/子测试事件通过、无测试级 skip；全仓编译、定向 vet、Mail/Account 生成检查通过。新框架场景不以该数字计算源码覆盖率。

| 范围 | 当前实际实现与证据 | 下一入口 / 保留边界 |
| --- | --- | --- |
| Account | slot 持久计划、同 ID/同名恢复、pending 准入、Memory/Redis、服务重建、并发 | 旧空 slot/孤儿/committed 名字需对账；新名字被他人提交需业务协调；无后台扫尾 |
| Mail | Token+Attempts 取消，旧 JSON 拒绝、生成调用者/模板、迟到 Commit 仍可结算 | owner-first 升级；真实发奖/跨进程旧取消仍需部署验证；继续 Delete/Commit 与在途发奖 |
| Directory / Profile | 8 个删除重建交错；三公开 Role 输出 Memory/Redis 所有权 | 自定义 backend/wrapper 需 DeleteIf；真实 HA/断网未测 |
| 生成与运维 | codegen 首跑的 sh 环境失败已在 Git shell 补跑该用例通过 | shellcheck 未安装，不称检查通过；未部署/发版/运行生产迁移 |

源码修复不等于整个 service 域审完。用户说没有修复时，旧验收跳过规则保持；默认继续 Account 其他补偿失败、Mail 在途交错、实际 Releaser 并发与 Platform 未知外部结果。下方完整保留第三轮 review 和历史停点。

## 2026-09-29 Service 第三轮停点

最新源码 `83c04243`，fetch 后未新增 main 源码；[运行记录](REVIEW-2026-09-29-services-03.md) · [问题交接](../bug/REVIEW-2026-09-29-services-03.md) · [实现学习](IMPLEMENTATION-SERVICE-COMPENSATION-AND-ATTEMPT-IDENTITY.md)。新增 **RR-20260929-19..22（3 P2、1 P3）**，尚未实施。旧 19 项的现有回归复跑通过，不代表各问题所有故障窗口已验收。

| 范围（最近 SHA 83c04243） | 新增阅读与实际执行 | 状态 / 下轮入口 |
| --- | --- | --- |
| Account 建角 | Slots.Create / Roles.Create / Names.Commit 补偿；三写后错误 × Memory/Redis；对应三写前控制 | RR-19；继续持久 intent、崩溃、读回失败及补偿失败；旧 slot 尾提交修复保留 |
| Mail 领取 | Reserve/Cancel 的 Token、Attempts、Deadline；旧取消 × 两后端；200 条过期 pending 容量及 Delete 后重试 | RR-20；容量为保护证明的观察；下一入口 Delete/Commit 与在途发奖交错 |
| Directory | Cancel/Release → Store.Delete；四个删除重建交错、现有 DeleteIf 语义 | RR-21；优先原子身份删除；同 Owner 换 Token 待执行 |
| 返回所有权 / 资源 | Account 两输出切片 × 两后端；Session 并发 Finish 回调观察 | RR-22 仅 Memory；真实 Releaser 并发幂等仍未测 |
| 回归与证据 | 16 包通过、759 测试/子测试事件通过、0 测试 skip；新选定 25/25 叶子已执行，14 反例失败、11 对照/观察通过 | 不是整包/全框架正确率；真实 Broker、发奖、断网 HA、强杀、负载未测 |
| 图谱 | roost-core 新 coverage generation 03:13:02Z 已发布；路径 metadata_changed，关键源码补证 | full 请求本身超时；不作全 service 穷尽图谱结论 |

此前 10/10 服务域“主链有记录”的范围计数保留，不能解释为全文件或全场景审完。当前用户优先 service；后续按以上邻近未覆盖入口推进。用户说没有修复时跳过旧验收。下方是历史修复/审查时点。

## 2026-09-29 Service 修复停点

基线 `f46db3e7`；RR-20260929-01～18 和旧 RR-20260909-02 正常 Finish ABA 共 **19 项实现完成并通过定向回归**。756 个测试/子测试通过，另补 3 个边界用例通过；全仓编译、定向 vet、12 次生成一致性检查通过。进度是所列问题的实现/验证状态，不是源码覆盖率或全故障交错完成率。

[逐项实现、学习与升级边界](../bugfix/SERVICE-BUGFIX-2026-09-29.md) · [roost-bugfix skill](../agent-skills/roost-bugfix/SKILL.md)。正式回归已进入对应包。下一入口：独立验收本轮实现，mail 在途 claim/正文缺失分类、Account 其他未知写入结果。旧邮箱缺期限、旧 ledger 缺 Intent 和旧身份碰撞仍需对账迁移；本轮未发版。

图谱 `roost-core` generation `2026-09-29T00:39:36Z`；coverage 标记 metadata_changed / 工作树新文件 missing，当前源码补证。下方保留修复前时点的报告。

09-29 第二轮最新停点：[Service 身份、迁移与失败恢复](REVIEW-2026-09-29-services-02.md)。单仓 Core `e10dd3f1`；上一轮 **10/10 服务域主链**保持为范围进度，本轮不换算源码正确率。新增 **RR-20260929-11..18：8 个动态问题（6 P2、2 P3）**；旧 RR-20260909-02 追加正常 Finish 的真实 Redis ABA，不重复编号。

| 本轮新增范围 | 已完成 | 下一入口与限制 |
| --- | --- | --- |
| 身份 / 提交结果 | account 两个已验证身份碰撞、slot 写后响应丢失 | RR-11/12；身份键迁移、名字/角色其他写入点崩溃恢复 |
| 路由 / 对象所有权 | global 顺序迁移后续期、三种输出 map 引用 | RR-13/17；Acquire/迁移并发约束；map 问题仅 MemoryStore |
| 资源 / 终态 | Attach 管理字段、Finish 重试漏清理、正常 Finish ABA | RR-14/18 + 旧 RR 残留；原子身份删除优先，未量化游戏资源后果 |
| 活动 / 邮件 | 正增量回绕、正文写前失败后的同请求恢复 | RR-15/16；完成重叠仅观察；mail claim 在途到期/正文缺失分类待查 |
| 验证 / 文档 | 9 个缺陷顶层 FAIL（8 新 + 1 旧新触发）、1 观察 PASS；7 包原有回归 425 测试/子测试全过，无 skip | 真实 Broker/发奖、断网/HA、多进程强杀和容量性能未测 |
| 图谱 | full refresh 提交一次后超时，查询/coverage 请求未取得可依赖结果；当前源码补证 | 不能确认 generation 更新；未操作共享 MCP 进程 |

复现脚本、源码材料、实施建议及学习文档已落盘。生产代码未修，上轮十项没有重新验收；下一轮若声明未修复，直接从表内新入口继续。下方报告是历史时点，不被本轮结论覆盖。

09-29 最新停点：[Core 全部 service 域](REVIEW-2026-09-29-services.md)。单仓 Core `6b73289c`，范围 `service/` + `kit/service/`；本次 **10/10 服务域主链完成**（account、directory、global routing/lease、activity、mail、match、platform、rank、session、chat），不是全源码审完率。96 个生产 Go 路径有清单，18 个生成文件通过门禁并抽样；其余按当前主链与接线做源码验证，不宣称每行覆盖。

| 本轮状态 | 已完成 | 保留下一轮 |
| --- | --- | --- |
| 缺陷发现 | RR-20260929-01..10：9 个动态反例、1 个源码接线缺口；2 P1、7 P2、1 P3 | 尚未修复/实施；先支付终态与 activity 未确认进度 |
| 服务回归 | 15 测试包 race/integration pass，684 pass 事件，无测试级 skip；4 个 platform 初次 skip 已补跑 | 真实 Broker、业务退款/发奖、多进程重启/强杀 |
| 传输生成 | 9 RPC 服务、12 次 servicerpc -check 全通过，无生成文件写入 | 正式 game 接收/确认链路补测 |
| 图谱证据 | MCP 恢复；96 路径 coverage：20 metadata_changed、76 not_tracked；当前源码补证 | 图谱 generation 仍 09-20，不能据 ready/HEAD 宣称刷新完 |
| 设计/性能 | 状态证明、所有权补偿、键隔离、候选清理和热点对象成本写入机制文档 | match/activity 历史积压容量与热点 p95/p99；session roster 可达性 |

文档交接与复跑脚本已完成；生产源码未改，不关闭旧修复。本轮更新不覆盖下方旧报告的时点结论，也不重算历史三仓覆盖率。用户说“没有修复”则跳过旧 bug 验收，继续新范围。

09-20 最新停点：[运行/验收/同步/所有权](REVIEW-2026-09-20.md)。Core `8c589a6` / Kit `19fb010` / Codegen `9bbac81`；RR-20260919-02/04/07/08/09/10 原触发通过，两个活动 Wanted 分流，新增 RR-20260920-01..05（4 个 P1、1 个 P2）。累计 **54 份运行记录、33 篇机制文档、99 个不同 RR**，不是代码覆盖率或当前未修复数。

| 当前域 | 本轮新增阅读与执行 | 未完成及下一步 |
| --- | --- | --- |
| K1 实体/事务 | nested 唯一父、两层 load flight、Nest nil 验收；player owner token/lease/write admission 逐层审查 | RR-03/04；用 incarnation + Lua compare，租约无法证明时 fail-closed 并 drain/unload |
| K2 数据/恢复 | versionstore 原子二级索引通过；remote BSON uint64/WAL 毒丸、platform poison prefix | RR-01/05；定编码和迁移、pre-WAL 校验、quarantine/repair，补真实 Mongo/Redis 重启 |
| K3 跨服/权威 | remote-managed Wanted 更正根因；SID 与进程实例身份边界收敛 | owner fence 完成后继续迁移、mirror、墓碑与防复活 |
| 实时同步 | room→entitysync→transport 完整调用链；现有阻塞测试证明 latest-only 覆盖 | RR-02；先 reliable 修正确性，再做 ACK baseline/coalesce，重跑 16/32 客户端 |
| 横向 Codegen/Kit | activity owed、订单原子索引验收；playerroute/scene/platform 新模板精读 | 老数据索引回填；真实双实例 owner、支付 poison、网络背压未测 |

本轮 24 个关键路径均经图谱 discovery/trace/coverage 后回读当前源码；`.tmpl` 用源码补证。全局 18.74% 仍是 09-18 历史文档路径触达基线，本轮不换算新的审完百分比。下一轮若用户未修复，直接从 K1 的 handler/in-flight transaction 与 K3 owner fence 后续继续；若已修复，优先验五个新 RR。

09-19 第二轮最新停点：[运行/K1/Activity](REVIEW-2026-09-19-02.md)。Core `a2e8fa0` / Kit `5116f2a` / Codegen `fde74d1`；RR-01/03/06 与 chat presence 原触发通过，RR-05 的分页残余另列 RR-07。K1 新增实体 load、Nest 缺失实体与 worker 串行关闭的有界审查；activity 新 feature 发现交付断链。累计 **53 份运行记录、32 篇机制文档、94 个不同 RR**，不是代码覆盖率或当前未修复数。

| 当前域 | 本轮新增阅读与执行 | 未完成及下一步 |
| --- | --- | --- |
| K1 实体/事务 | ManagerAccess/Repository 双层 singleflight；Nest single/broadcast/multi 对照；worker safeHandle/Close；四包 race 与两个临时红测 | RR-08/09；继续 leader context、loader generation、在途 load 的 Shutdown handoff、删除/重载交错 |
| K2 数据/恢复 | 随实体加载读 repository restore 与 Runtime Shutdown 顺序；未新增真实 store 场景 | K1 收敛后继续提交不确定性、WAL/投影关闭和恢复 |
| K3 跨服/权威 | load 入向追到 remoteentity，未新增 owner/fence 行为测试 | K1/K2 后继续迁移、mirror、墓碑与防复活 |
| 横向 Codegen/Kit | RR-01/03/05/06 与 chat 接线验收；platform 分页残余；activity Delivering/dispatch/ACK 与 game runner | RR-07/10；实现按 game owed 枚举与真实领取后再验多窗口离线/重启/ACK 丢失 |

三个 codebase-memory watcher 在重启后在线并对齐当前 Git；Codegen 已显式 full reindex。关键路径 coverage 显示 checkout 后 metadata_changed，均已回读当前源码。下一轮默认从 K1 的 load 生命周期继续，不把包测试通过外推为整域完成。

09-19 最新停点：[运行/验收/K1/platform](REVIEW-2026-09-19.md)。Core 1947faa / Kit 5116f2a / Codegen 7297f92；U-0245、game-demo 第十四批装备/迁移与第十五批支付正向门通过，RR-06 因 chat presence 未接线改为部分修复。K1 新 11 叶子 8 过 3 失败，生命周期另 1 叶子失败；batch15 又确认订单/索引非原子、坏单阻塞整页、未履约 grant 过期删除，合计 RR-20260919-01..06 六个 P1。W-11 转 ARCH-07。累计 **52 份运行记录、31 篇机制文档、90 个不同 RR**，不是代码覆盖率或当前未修复数。

| 当前域 | 本轮新增阅读与执行 | 未完成及下一步 |
| --- | --- | --- |
| K1 实体/事务 | 顶层 map 同 key 多写三形状、nil、commit拒绝、panic均过；顶层 pointer 4叶子2失败；map alias 1失败 | 先修 pointer 所有权与唯一父约束；再继续实体加载/释放、串行 mailbox、关闭结算和持久化完成交接 |
| K2 数据/恢复 | 三个失败均用真实 CommitRecord，alias 解码 BSON 确认只写 `equips.2` | 无真实 Mongo/WAL/断电新增验证；K1 收敛后继续 schema、提交不确定性与恢复 |
| K3 跨服/权威 | 本轮无新 owner/fence/mirror 行为验证；运行期 ID 修复验收通过 | K1/K2 后继续 owner/fence/epoch、迁移、mirror、墓碑与防复活 |
| 横向 Codegen/Kit | 7297f92 新生成 demo 全包通过；U-0245、装备/迁移正向通过；platform 支付链与 pending 索引逐段审查，新增 3 个支付恢复 P1 | 订单与索引原子化；坏项隔离；付费履约确认后再清理；修 callback/presence；真实 Redis 多进程故障仍未测 |

图谱仍是 09-08 generation，最新 DAO/TCP/platform/purchase 模板未进入图；聚合 coverage 后已逐一回读源码。下一轮用户若没有修复，跳过上述验收并直接进入 K1 下一入口；若有修复，先验六个新 RR 和 RR-06 剩余部分。

09-18 第三轮最新停点：[运行/验收/Wanted](REVIEW-2026-09-18-03.md)。Core 9a97d7e / Kit 399f175 / Codegen c73bc12；RR-03/04 修复原根因独立通过，10 条 Wanted 全部分流，K1 顶层 DAO map 另确认所有权问题。独立 22 叶子 15 通过、7 行为失败；累计 **51 份运行记录、30 篇机制文档、84 个不同 RR**，不是代码覆盖率或未修复数。

| 当前域 | 本轮新增阅读与执行 | 未完成及下一步 |
| --- | --- | --- |
| K1 实体/事务 | 当前 DAO template/golden 重新生成；顶层 map committed old/new、rollback old/new 四场景，2过2失败，RR-10 | 继续同 key 多次 Set/Del、nil/alias child；panic、commit拒绝、关闭结算；实体加载/释放和串行调度仍未整体完成 |
| K2 数据/恢复 | RR-03 修复后 10 个归属/Commit 叶子；顶层游离值真实产生 CommitRecord | 未解码 stale patch；无真实 WAL/Mongo/断电恢复新增验证，K1 后继续 |
| K3 跨服/权威 | scene/entity ID 边界与运行期 monster ID 已读，两个 spawner 碰撞转 RR-09 | owner/fence/epoch、迁移/镜像/墓碑未新增；K1/K2 后继续 |
| 横向 Codegen/Kit | RR-04 三叶子通过；mail、TCP lifecycle、syncTopic、AOI、scene relation/identity 分流 | RR-05..09 待实施；真实多进程/连接风暴/性能未测 |

Wanted 当前无活动待判项。W-02 转 ARCH-06；W-06/07/08/09 分别按完整 entity ID、文档契约、demo source-refcount、全 nopersist DAO 收敛。[Scene 机制](IMPLEMENTATION-SCENE-INTEREST-IDENTITY-AND-LIFECYCLE.md)与[生成契约](IMPLEMENTATION-GENERATED-FEATURE-CONTRACTS.md)已记录事实和方案。图谱仍为 09-08 旧代，关键路径经 coverage 后全部回读当前源码；没有将局部场景外推为整域完成。

09-18 第二轮最新停点：[运行/验收/证据](REVIEW-2026-09-18-02.md)。Core abb7b80 / Kit 399f175 / Codegen ffff2a1；用户已修复，本轮8项旧RR原触发通过，另确认RR-20260918-03/04（新归属/保留期根因）。独立46场景38通过8行为失败；累计**50份运行记录、29篇机制文档、78个不同RR**，不是覆盖率或当前未修复数。

| 当前域 | 新增阅读与执行范围 | 未完成及下一步 |
| --- | --- | --- |
| K1 实体/事务 | nested模板wire、map/slice/pointer替换；Nest undo首次去重/反序撤销/Tracker恢复；DirtyHook；生成DAO后续Commit。局部12归属+2提交场景全部执行，7过7失败 | 只完成该有界范围，整域仍未完成。下一入口顶层DAO Set/Del/Init与nested关系；多次写、panic、提交拒绝、关闭结算；实体加载/锁/串行历史证据继续回填 |
| K2 数据/恢复 | 共享K1的真实prepare→记录型commit，以及修复验收中的Room水位链；不重复计场景 | 本轮无真实WAL/Mongo/恢复/schema新增验证，K1之后继续 |
| K3 跨服/权威 | 本轮无新增范围，保留旧owner/mirror文档 | K1/K2后继续owner/fence/epoch、迁移/镜像/墓碑；未称完成 |

最新功能顺序仍为K1→K2→K3，K7暂缓；本轮K4/K6仅随修复验收延伸。8项旧问题的原触发通过不等于新问题已解决；用户下轮声明未修复时直接推进上述K1下一入口。早先18.74%仅为旧SHA的文档引用基线，本轮不把它换算成代码审完率。

用户最新优先级：先集中推进 K1 实体与事务执行 → K2 数据提交与恢复 → K3 跨服实体与所有权；K7 技能/战斗暂缓，覆盖此前的扩展建议。每域记录范围、场景分母、证据和剩余风险，未修复旧问题按声明跳过验收。规则已写入本机 roost-review skill，见[更新路线](CORE-FUNCTION-CLASSIFICATION.md)。

审查优先级调整：[八个核心功能域与路线](CORE-FUNCTION-CLASSIFICATION.md)。后续以实体执行/持久化/跨服权威为第一主链，鉴权、资产和停机安全随链检查；旧版技能战斗扩展建议已被当前K7暂缓要求替代。用户未修复时不回到旧验收。

09-18 新增[可复算覆盖统计](COVERAGE-2026-09-18.md)：三仓 699 个主要源码文件，历史报告可定位 131 个，文档路径触达率 18.74%。不是整文件审完率；已建立全量文件/引用台账，最近一轮先核准 10 个局部源码入口，历史证据待逐条回填。49 轮/29 篇机制/76 RR 计数不变。

09-18：**49 份运行记录、29 篇机制文档、76 个不同 RR**，仅记录计数，不是覆盖率或当前未修复数。Core 6e09124 / Kit 55f3a35 / Codegen 2e09c16；三仓同步无新提交。用户未修复，本轮跳过全部旧验收。[运行与总体进度表](REVIEW-2026-09-18.md) · [两项新 P2](../bug/REVIEW-2026-09-18.md)。

| 范围 | 已验证/状态 | 限制与后续 |
| --- | --- | --- |
| Codegen sync=true 消费契约 | 三组生成成功；sync=false 编译过，两个 sync=true 编译失败，RR-20260918-01 | 模板与 Core 配置需收敛，自动运行链未通过 |
| 生成实体手动 EnableSync → RoomManager → RoomTransportSink | 快照/增量/batch/profile/退订/退休/关闭/失败重试八场景通过 | 最终传输替身，无真实客户端、并发卸载与生产网络 |
| 房间 pipelined 外发屏障 | 三入口失败；直接 coordinator 三个已配置水位对照通过，RR-20260918-02 | 无真实 WAL；Room 配置传递缺失，非旧问题复核 |
| Wanted-05 | 两个确定缺陷已登记，公开组合路径已证实 | 自动入口/真实恢复尚未完成；Kit Mod 是设计选择 |

本轮新行为 14 叶子 11 过/3 失败，race 无报告；消费编译另计，生成器原包测试过。图谱仍 09-08 旧代，关键范围已源码补证。下一轮未修复时推进 GM 鉴权/参数边界、mail/gift 资产交接或真实同步消费/卸载并发；不要反复验收旧 RR。跨领域进度见本轮运行表，Lockstep/Sync 均未标整体收敛。

09-17 第三轮（09-18 完成文档收尾）：48 份运行记录、29 篇机制文档、74 RR（记录计数，非覆盖率）。Core dc1aab1（后续 1af903e 仅文档）/ Kit 55f3a35 / Codegen 2e09c16。[运行](REVIEW-2026-09-17-03.md) · [五项问题](../bug/REVIEW-2026-09-17-03.md) · [跨层契约机制](IMPLEMENTATION-GENERATED-FEATURE-CONTRACTS.md)。

| 范围 | 已验证/状态 | 限制与下一入口 |
| --- | --- | --- |
| DAO BSON / nested dirty | 原 codec 三项过；通知四叶子一过三失败，RR-05 | struct/slice、解绑、真实事务 patch/rollback 待查 |
| attribute feature | CLI 成功、消费编译缺公共类型，RR-06 | Core runtime 与本地访问器契约待实施 |
| Saga U-0225 / 原生完成 | 原 race 过；订阅三叶子二过一失败，RR-07 | fake JS；原生 inbox/outbox/真实 broker 未端到端 |
| dungeon 奖励 | 生成 Controller + Core Session 五叶子二过三失败，P1 RR-08 | 派发替身；事务内 RunID 去重待实施 |
| battle 宽限期 | 实际 run/Room 两叶子一过一失败，RR-09 | 未连 TCP；开帧状态机待修 |
| Wanted-05 statesync | 公开组合入口已读，broadcaster 自有 coordinator；再观察 | 下一轮优先真实生成实体→room→sink、持久化水位与关闭 |
| cfggen / 新 demo | runtime 四项过；demo generate/build/编译检查过 | GM/聊天/榜单等尚未逐项行为验收；生产故障/性能未测 |

新增行为 14 叶子 6 过/8 失败，attribute 编译失败另计。Wanted-02/03/04 已分流，原文保留。图谱仍 09-08 旧代，相关过期/未跟踪路径已源码补证；没有认定整包完成。前轮 platform RR-04、历史匹配键升级和 ARCH-05 状态不变。

09-17 第二轮：47 份运行记录、28 篇机制文档、69 RR（记录计数，非覆盖率）。实际测试 Core 6fb36e7 / Kit 4830150 / Codegen 242b438；收尾时新远端 feature 另在第三轮审查。[运行](REVIEW-2026-09-17-02.md)。

| 范围 | 已验证/状态 | 限制与下一入口 |
| --- | --- | --- |
| manager / match 五项修复 | 原 6 叶子通过；算术/所有权新增 15 场景通过 | 新格式键通过，历史升级四失败，RR-01 未全面收敛 |
| platform owner 重试 | 真实 30 秒 tick 后仍无投递，两个主动驱动对照通过；新 RR-04 | 待实现有界持久来源；非真实支付/Redis 验证 |
| 六 RPC 服务 + directory | 39 份领域/transport 直接依赖盘点，原七包 race 通过；Wanted 转 ARCH-05 | 私有维护入口、RegistryBound、数据/发布兼容需实施；未迁移 |

新矩阵 23 场景 18 过/5 失败，原 6 复核单计；Core 两包与 Kit 九包原 race 通过。图谱 09-08 旧代，相关 not_tracked/metadata_changed 已源码补证。未认定整包完成。

09-17：46 份运行记录、27 篇机制文档、68 RR（仅记录计数，不是覆盖率）。Core 9f31436、Kit 4830150、Codegen 242b438。用户声明未修复，本轮跳过旧复核，新增 RR-20260917-01/02/03。[运行与证据](REVIEW-2026-09-17.md)。

| 范围 | 已验证不变量与状态 | 下一入口/限制 |
| --- | --- | --- |
| Core service/match Queue → Candidates → Commit | 键碰撞可混池；普通隔离和提交对照通过；已验证部分场景 | RR-01 待实施；真实 Redis 键迁移未验证 |
| ScoreWindowGrouping | 普通分差/窗口对照通过，四种整数溢出触发错误配对或拒绝 | RR-02 待实施；完整等待时间/时钟回拨契约待查 |
| 内存 Store 输入/输出所有权 | 七个切片修改入口污染状态；顺序复现确认 | RR-03 待实施；不推定 Redis 具有相同引用问题 |
| Codegen demo matchmaker | 九种候选/策略/提交分支、三种通知不可用成组场景通过 | 真实 TCP/RPC 查询恢复和通知成功链未验证 |
| 匹配历史保留 | 128 场后推进一年，Sweep 仍保留票/比赛/请求；列容量观察 | 幂等重放/归档合同与真实后端大状态压测待查 |

本轮 34 个新场景，20 通过、14 个行为失败；Core 原 match race 测试通过。恢复后重跑并保存三个最小复现，均确认问题，不重复计入 34 场景；原完整临时矩阵文件已丢失，限制见运行记录。图谱 generation 09-08，迁入路径/模板 not_tracked、versionstore metadata_changed，已源码补证。未将 match 或 demo 标为完成；后续优先 RPC 身份/序列化、匹配重放与查询恢复，既有未审范围保留。

09-16 第五轮：45 份运行记录、26 篇机制文档、65 RR（仅记录计数）。Core 41d49b9、Kit 4830150、Codegen 242b438。复核上次实施交接，新增 RR-20260916-06/07。[运行与逐项验收](REVIEW-2026-09-16-05.md)。

| 范围 | 当前证据与状态 | 下一入口/限制 |
| --- | --- | --- |
| Recover / WAL | 原四个 Recover、两个 journal、20 个 History/WAL 叶子全部通过；Windows 半尾失败已修复 | Linux 本轮未独立运行；宿主恢复策略/高并发重试待查 |
| Core/Kit 服务迁移 | match/mail/session + servicemetrics + RPC 两半归属落实，相关十一包 race 与 Core 边界测试通过 | 其余 Kit 领域未全迁，真实存储/协议端到端未新增验收 |
| manager Engine | 原测试过；三项独立交错一过两失败，最后 Start/Stop 和首项启动期间 Register 新 RR | RR-06/07 显式状态与所有权交接待修 |
| Codegen/demo | 当前 CLI 生成真实 demo，发布与源码模式 build/test 通过，源码 generate --check 通过 | Core 三份 RPC 命令头 stale；shellcheck 未安装；非生产部署验收 |
| durable 升级交接 | 上轮指出的 Kit 默认改名说明仍未补齐 | 旧游标与重放演练仍未完成 |

MCP generation 09-08，迁入文件 not_tracked、旧文件 metadata_changed；已按当前源码补证，不认定整包或全框架 review 完成。

09-16 第四轮：44 份运行记录、25 篇机制文档、63 RR（记录计数，不是覆盖率）。Core 3060817、Kit 527eecd、Codegen 678b2d9。用户明确要求恢复修复验收；七项新 bugfix 定向核验，新增 RR-20260916-04/05。[运行](REVIEW-2026-09-16-04.md) · [统一实施交接](../bug/REVIEW-2026-09-16-04.md)。

| 范围 | 已验证不变量/状态 | 限制与下轮入口 |
| --- | --- | --- |
| History Import/Recover、清理身份 | 原交接 29 场景全过；清理三触发/Rotate 对照过；新 Recover 删除 ABA、同位置 Import 两触发失败 | RR-04 修改代数待实施；宿主捕获契约未穷尽 |
| WAL 尾部与 fail-stop | 原 20 场景 19 过，半尾截断 Windows 拒绝；Sync/Publish 后报错两项停止准入通过 | Linux 半尾续写待验；只读目录对照 Skip；非真实断电 |
| JetStream fanout/Prefix | 五项真实本地广播/分片复现通过；命名隔离通过，Kit 默认 durable 改名已确认 | 原 RR-03 触发验收通过；迁移演练与共享 Stream 所有权仍待处理 |
| Kit match → codegen/demo | 无效 Grouping 注入确认 RR-05；实际为调用方 Candidates→Group→Commit | 契约收敛待实施，完整 demo 尚未审完 |
| Kit 核心职责样本 | session/mail/match、manager 属实现；dataengine/saga 装配作迁移样板 | ARCH-01..04 仅方案；非 Kit 全目录穷尽 |

相关 Core/Kit 原 race 测试结果及失败详见交接文档。MCP generation 09-08，过期片段/新未跟踪文件均以源码补证；不将任何整包标为完成，其他旧 RR 状态不变。

09-16 第三轮：43 份运行记录、25 篇机制文档、61 RR（仅记录计数）。Core 21e0a6c，Kit 7030c5f，Codegen cacd627。用户未修复，跳过旧验收；真实 NATS v2.11.9 单节点新 35 场景，32 通过、3 失败确认 RR-20260916-03。Core 三包、Kit 两包原有 race 测试通过。[运行](REVIEW-2026-09-16-03.md)。

| 新增覆盖 | 已验证不变量/状态 | 限制和下一入口 |
| --- | --- | --- |
| room JetStream Subscribe → driver Consume → syncstream reassembler | 同 bus/topic 双订阅新 RR，消息分摊、两种分片均未重组；普通 NATS/不同 SID/单接收者对照通过 | observer 过滤组合、半包重启与业务快照恢复 |
| NATS driver 发布、结算、重连、Drain | 真实 ACK/去重/进程重启、重投/终止/续接、请求映射/队列/排空等部分通过 | 单节点 Windows，非集群 failover/断电/TCP 半开 |
| syncstream 不确定确认与 Apply | 人为丢弃成功返回后重试，业务 Packet 重复；Apply 错误符合不重试包装契约 | 应用幂等、水位、长期离线恢复未收敛 |
| Kit room/nats mod | 当前装配/停止源码已读，原有两包测试通过 | 完整 App 依赖排序、关闭期间注册待查 |

本轮图谱 generation 09-08、证据路径 metadata_changed，已用当前源码补证。未验收旧 Prefix 问题或重跑旧复现。下一轮优先新 receiver/observer/恢复路径，再轮转 bus/RPC 重放；既有 journal/Lockstep/StateSync 缺口保留。

09-16 第二轮：42 份运行记录、25 篇机制文档、60 RR（记录计数，非覆盖率）。Core 1143f61，Kit 7030c5f，Codegen cacd627。用户未修复，跳过旧核验；JetStream/NATS 新 32 场景，31 通过、1 失败确认 RR-20260916-02。原源码 room/nats driver race 通过。[运行](REVIEW-2026-09-16-02.md)。

| 新范围 | 已验证不变量、状态与限制 | 下一入口 |
| --- | --- | --- |
| room/jetstream_syncbus.go 命名空间与发布 | 消费者 Prefix 冲突新 RR；SID/topic/Stream 对照、所有权/context/ID 等部分通过；非真实 broker | Stream 所有权、真实服务端配置与确认超时 |
| Subscribe/Stop、接收包装 | 幂等/重试/过滤/错误契约受控验证；Stop 越过在途创建为观察项 | 上层关闭顺序、终态契约和真实重连 |
| nats/driver/jetstream.go 结算 | ACK/NAK/Term/延迟/panic/结算错误十场景通过；没有证明重投必达 | 服务端 redelivery、应用 Apply 恢复 |
| room/nats_syncbus.go 与普通 driver 发布入口 | 当前源码已读，普通发布无持久确认；未做网络专项 | 真实连接中断与可恢复传输对比 |

本轮图谱 generation 仍为 09-08，相关路径 metadata_changed，已以当前源码补证；不标任何整包完成，旧 journal/room/statesync 缺口保留。

09-16：41 份运行记录、24 篇机制文档、59 RR（非覆盖率）。Core b4bf09e，Kit 7030c5f，Codegen cacd627。用户未修复，跳过旧核验；journal 故障/syncbus 新 28 场景，26 通过、2 失败确认 RR-20260916-01：写/发布结果不确定后继续使用旧内存状态。原源码两包 race 通过。[运行](REVIEW-2026-09-16.md)。

| 新范围 | 证据与限制 | 下一入口 |
| --- | --- | --- |
| Journal 错误语义 | 八场景六过两失败，临时注入非真实断电 | 生产恢复协议、多实例目录所有权保留 |
| PatchSyncer/DeliveryIDs | 二十场景通过，重复/旧版本交 Apply 符合契约 | 真实 JetStream/NATS 确认、重连与应用恢复 |

09-15 第八轮：40 份运行记录、23 篇机制文档、58 RR（非覆盖率）。Core 6cef240，Kit 7030c5f，Codegen cacd627。用户声明没有修复，跳过旧核验；新 29 场景 23 通过、6 失败确认 RR-20260915-08/09：持久化 Import/Restore 未替换 journal、Recover 提交过期捕获。现有 syncstream race 通过。[运行](REVIEW-2026-09-15-08.md)。

| 新范围 | 状态/限制 | 下一入口 |
| --- | --- | --- |
| Import/Restore + journal | 四失败两 checkpoint 对照，新 RR-08 | 原子替换和同步错误歧义 |
| Recover 快照交接 | 两失败三对照，新 RR-09 | 修改代数与宿主捕获契约 |
| 校验/并发 checkpoint | 十非法、四合法、store 失败、三并发重启通过 | journal 故障与 syncbus 业务恢复 |

09-15 第七轮：39 份运行记录、23 篇机制文档、56 RR（非覆盖率）。Core 4622463，Kit 7030c5f，Codegen cacd627。旧修复跳过；History/ACK/journal 新 20 场景 16 通过、4 失败确认 RR-20260915-06/07：清理后身份复用、半条 WAL 恢复后续写损坏。现有 syncstream race 通过。[运行](REVIEW-2026-09-15-07.md)。

| 新增范围 | 状态与限制 | 下一入口 |
| --- | --- | --- |
| History 清理/ACK/Resync | 三种清理触发同身份问题；Rotate 对照通过 | Recover 并发快照与身份高水位 |
| journal 拒绝与文件恢复 | 六种拒绝原子性通过；半尾续写新 RR | Import/Restore 持久化、Record/Checkpoint 边界 |
| syncbus 应用恢复 | 本轮没有新增实现验证 | 后续去重和 ACK/重放接入 |

09-15 第六轮：38 份运行记录、22 篇机制文档、54 RR（非覆盖率）。Core b58e280，Kit 7030c5f，Codegen cacd627。旧修复跳过；新增 24 场景 22 通过、2 失败确认 RR-20260915-05：SetDownstream 未迁移慢消费者回调。相关三包 race 通过。[运行](REVIEW-2026-09-15-06.md)。

| 新增范围 | 状态与限制 | 下一入口 |
| --- | --- | --- |
| room downstream/共享 session/释放 | 回调迁移新 RR；清理范围、Close 重试与两种在途释放通过 | 旧事件、session ID 重用与跨 room 并发剔除保留 |
| syncstream publisher/reassembler/buffer | 14 场景通过；未做真实 broker/恢复 | History/ACK/epoch/journal |
| syncbus handler 契约 | 明确错误不重试，JetStream 包装源码一致 | 去重与应用 ACK/重放责任 |

09-15 第五轮：37 份运行记录、21 篇机制文档、53 RR（非覆盖率）。Core 4537a6d，Kit 7030c5f，Codegen cacd627。旧修复跳过验收；room/真实 sink/AsyncTransport 新增 21 场景，19 通过、2 失败确认 RR-20260915-04：剔除后剩余批次失败丢通知，重试后旧订阅不清理。相关两包 race 通过。[运行](REVIEW-2026-09-15-05.md)。

| 新增范围 | 状态与证据限制 | 下一入口 |
| --- | --- | --- |
| RoomBroadcaster dirty/退役、RoomEnvelopeSink 序号 | 五个失败/交接场景通过；tick 非全主体事务 | 生命周期与在途 flush |
| RoomTransportSink 基线与剔除 | 八个参数/基线场景通过；剔除三场景一过两失败 | 跨 room 共享 session、释放和通知关闭 |
| AsyncTransport.AdmitBatch | 五个队列原子性场景通过，未测真实网络 | 然后 syncstream/syncbus |

09-15 第四轮：36 份运行记录、20 篇机制文档、52 RR（记录计数，不是覆盖率）。Core b205653，Kit 7030c5f，Codegen cacd627。旧修复跳过验收；entitysync 失败/重试、profile 交接、批次 dirty 与生命周期新增 17 叶子全部通过，相关两包 race 通过；分片取消阻塞列观察项，无新增确定 RR。[运行](REVIEW-2026-09-15-04.md)。

| 新增范围 | 状态与限制 | 下一入口 |
| --- | --- | --- |
| Subscribe/profile/Unsubscribe/Distribute | 拒绝、panic、取消与重试共 12 场景通过 | 真实 sink 的原子准入与重试归属 |
| DistributeBatch/生命周期 | 4 个双主体场景通过；不代表并发穷尽 | room 批量 flush 与背压 |
| 分片并行与取消 | 1 观察通过，同分片取消需等锁释放 | 有界 sink，接着 syncstream/syncbus |


09-15 第三轮：35 份运行记录、20 篇机制文档、52 RR。Core faf4631，Kit 7030c5f，Codegen cacd627。用户要求跳过旧修复；本轮转入 entitysync，新增 RR-20260915-03，持久化水位未覆盖新订阅/profile/直接分发。五叶子三失败两通过，entitysync/entity race 通过。[运行](REVIEW-2026-09-15-03.md)。

| 范围 | 新证据/状态 | 下一入口 |
| --- | --- | --- |
| statesync 宿主消费链 | 本轮查询未定位完整接入，不作不存在结论 | 外部客户端、别名导入与真实运输恢复缺口保留 |
| entitysync Subscribe/Distribute/FlushSubject | 水位门槛三入口绕过，确认新 RR | 带版本的持久化准入、profile/失败重试 |
| entitysync 生命周期 | 源码已读，未专项穷尽 | 取消、sink/关闭和状态交接，之后 syncstream/syncbus |

09-15 第二轮：34 份运行记录、19 篇机制文档、51 RR。Core 9baf2ee，Kit 7030c5f，Codegen cacd627。按用户要求验收 RR-20260914-10..13、RR-20260915-01，原 24 场景全通过；新增 RR-20260915-02：重叠准备已交付视图在 stale 后仍静默分叉。三个新叶子两失败一通过，statesync race 通过。[运行](REVIEW-2026-09-15-02.md)。

| 范围 | 证据/状态 | 下一入口 |
| --- | --- | --- |
| U-0200..0204 原触发 | 五项原复现验收通过 | 保留真实链路等证据限制 |
| 自定义 PrepareLatest/Commit 交付 | 多/少对象分叉确认新 RR；显式全量通过 | 发送前视图固定或歧义恢复，序号交接 |
| 消费者与后续 sync | 本轮仅受控应用序列 | 真实运输/客户端恢复，再 entitysync、syncstream/syncbus |

2026-09-15：33 份运行记录、19 篇机制文档、50 RR。Core f8ee1eb，Kit 7030c5f，Codegen cacd627。新增 RR-20260915-01，满容量对象/组件替换因操作顺序被拒；六叶子两失败四通过，statesync race 通过。本轮不验收旧修复；上游 RR-10..13 修复标记保留、待独立验收。[运行](REVIEW-2026-09-15.md)。

| 新增范围 | 验证与状态 | 下一入口 |
| --- | --- | --- |
| delta 对象/组件生命周期 | 两种较小 ID 替换失败，较大 ID 对照通过；新 RR 未修复 | 最终容量与操作预算 |
| 通用 schema/archetype | 两个字节快照往返对照通过 | 业务生成解码器兼容性 |
| 客户端恢复 | 本轮未做真实消费链 | 优先消费者/恢复，再 entitysync、syncstream/syncbus |

第八轮：32 份运行记录、19 篇机制文档、49 RR（45 标修复、4 未修复）。Core 215fffa，Kit 7030c5f，Codegen cacd627。用户声明未修复，RR-10..12 跳过复核；新增 RR-13：LOD 错相发送使组件冻结。七叶子六通过一失败，statesync race 通过。[运行](REVIEW-2026-09-14-08.md)。

| 新增范围 | 不变量/证据 | 状态与下轮入口 |
| --- | --- | --- |
| statesync lod.go | 限频后的更新活性；奇数发送失败，逐帧/全量对照通过 | RR-13 未修复；继续删除/schema/生命周期 |
| statesync ring/session | 会话历史淘汰后全量回退、旧 ACK 拒绝 | 部分场景通过；全局环与会话历史独立 |
| PreparedFrame | ForceFull/新提交/Abort 后旧提交均拒绝 | 受控先后通过；真实同时发送未验证 |

下一入口：投影生命周期、客户端应用/重组恢复，之后 entitysync → syncstream/syncbus；旧问题未修复时继续新内容。

第七轮：31 份运行记录、19 篇机制文档、48 RR（45 标修复、3 未修复）。Core a123605，Kit 7030c5f，Codegen cacd627。用户声明 RR-10 未修复，本轮跳过复核；新增 RR-11（ForceFull 旧 ACK）、RR-12（单片帧长限制）。九个独立叶子六通过三失败，statesync race 通过。下一入口：LOD/更新频率、历史淘汰和恢复交接，再 entitysync → syncstream/syncbus。[运行](REVIEW-2026-09-14-07.md)。

| 当前范围 | 入口/不变量 | 证据与状态 | 未覆盖/下一步 |
| --- | --- | --- | --- |
| statesync session/control | ForceFull、Acknowledge、HandleControl | 四场景两失败两对照；RR-11 未修复 | 并发 ForceFull/Commit、重连恢复 |
| statesync datagram | push/Expire、单多片一致性、冲突清理 | 五场景四通过一失败；RR-12 未修复 | 真网络、客户端应用链、空片/畸形片 |
| statesync projection/history | 前轮 RR-10 保留 | 用户声明未修复，未再验收 | LOD、历史淘汰 |
| Kit/Codegen | pull main | 无增量，无新增源码审查 | 按既定轮转继续 |

第六轮：30 份运行记录、19 篇机制文档、46 RR（45 标修复、1 未修复），另有用户发现 U-0199。Core 2c5469e，Kit 7030c5f，Codegen cacd627。RR-09 已验收；U-0199 修前/后对照确认校验顺序漏审；statesync 新 RR-10。下一入口：RR-10、ForceFull/旧 ACK、分片重组，再 entitysync → syncstream/syncbus。Lockstep 回绕、业务模拟/快照恢复仍待查。[运行](REVIEW-2026-09-14-06.md) · [漏审复盘](POSTMORTEM-LOCKSTEP-U0199.md)。以下保留历史时点状态。

2026-09-14 第五轮：29 份运行记录、18 篇机制文档、45 RR（44 标修复、1 未修复）。Core `a1245fd`，Kit `7030c5f`，Codegen `cacd627`。RR-08 原上限验证通过；新增 RR-09：LockstepBot 回调错误后继续成功处理却跳帧，三处错误复现。真实 TCP 重投、取消恢复、原 UDP/期限/内存五场景通过；lockstep/robot race 通过。[运行](REVIEW-2026-09-14-05.md) · [问题](../bug/REVIEW-2026-09-14-05.md)。消费者错误恢复已从待查变成确认问题；sync 继续按已确认顺序排在 lockstep 后。

2026-09-14 第四轮：28 份运行记录、18 篇机制文档、44 RR（43 标修复、1 未修复）。Core `30a6b5b`，Kit `7030c5f`，Codegen `cacd627`。lockstep RR-04..07 原十叶子全过；新 RR-08 去重身份表准入上限失败。真实本机 UDP 24 帧冗余恢复通过、超期限重放边界确认、lockstep/robot race 通过，补两项有界微基准。[运行](REVIEW-2026-09-14-04.md) · [覆盖与优先级](LOCKSTEP-AND-SYNC-COVERAGE.md)。

当前六个 lockstep 生产文件已阅读，风险验证未全部完成；robot 消费者已读但错误恢复待专项。用户确认 lockstep 之后按 statesync → entitysync → syncstream/syncbus；sync 只有历史部分覆盖，本轮盘点不计新增正确性验证。下一入口：RR-08、重放期限与消费者恢复，再依清单推进；不以全包 race 通过宣告完成。

2026-09-14 第三轮（lockstep）：累计 27 份运行记录、18 篇机制文档、43 RR（39 标修复、4 未修复）。Core `a1455fb`，Kit `7030c5f`，Codegen `cacd627`。Activity RR-02/03 原五个测试叶子全通过；lockstep 十个叶子中五失败对应四项新 RR、五通过；两个相关包 race 通过。[运行](REVIEW-2026-09-14-03.md) · [问题](../bug/REVIEW-2026-09-14-03.md) · [机制](IMPLEMENTATION-LOCKSTEP-INPUT-AND-CATCHUP.md)。

| 范围 | 新增证据 | 状态/下一入口 |
| --- | --- | --- |
| lockstep sequencer | 按时与首次迟到输入跨帧重传重复入帧；显式覆盖对照通过 | RR-04 未修复；原身份/乱序窗口 |
| lockstep room/history | batch=1 不收敛，batch=2 通过；重绑、裁剪、发送失败、关闭对照通过 | RR-05 未修复；真实链路与快照 |
| lockstep room/wire | -1 会话碰撞、257 输入不能解码 | RR-06/07 未修复；配置协议统一 |
| lockstep assembler/desync | 满缓冲补洞、重复下行、裁剪屏障通过 | 部分场景；未验客户端模拟 |
| Activity | RR-02/03 原 overlay、包 race 通过 | 原触发验收；reopen/发送/容量边界保留 |

本轮无真实网络、压测或客户端确定性证明。以下条目保留历史时点状态。

2026-09-14 第二轮：累计 26 份运行记录、17 篇机制文档、39 RR（37 标修复、2 未修复）。Core `84b2a4a`，Kit `ac1a880`，Codegen `cacd627`。WAL U-0190 原关闭交错及新增取消重试/最终同步失败传播通过；Activity 新增两个 P2（窗口创建交接丢索引、派发跨轮扫描遗漏）。[运行](REVIEW-2026-09-14-02.md) · [问题](../bug/REVIEW-2026-09-14-02.md)。

| 本轮范围 | 入口与不变量 | 证据/状态 | 下一入口 |
| --- | --- | --- | --- |
| Core nestwal/wal.go | awaitWriteBarrier、Sync、Close、syncAndCloseActive | 原触发及独立错误/取消边界通过，包 race 通过；部分场景验证 | fsync 阻塞、真实物理故障及 shutdown 调用链 |
| Kit service/global/activity | OpenActivity/admitToWindow/AdvanceExpired/pruneWindow | RR-20260914-02 可复现；正常到期对照通过 | 带代际创建意图、延迟 prune 与失败恢复 |
| Kit service/global/activity | ensureDispatches/sweepGroup/DueDispatches/AttemptDispatch/AckDispatch | RR-20260914-03 三条完成路径复现；显式重试及 ACK 对照通过；包 race 通过 | 独立派发索引、reopen、公平性和实际发送 |
| Codegen | 同步 main | 本轮无新增源码覆盖 | 消费者与模板轮转 |

机制新增：[Activity 窗口与派发](IMPLEMENTATION-ACTIVITY-WINDOW-AND-DISPATCH.md)。图谱旧代，源码补证；Activity 使用 MemoryStore，未验真实 Redis/网络交付。以下条目保留各轮当时状态。

2026-09-14：累计 25 份运行记录、16 篇机制文档、37 RR（36 标修复、1 未修复），另有[六项未收敛工作](OPEN-QUESTIONS.md)。Core `50859c5`，Kit/Codegen 无变化。本轮十个专项场景：WAL 三个（关闭中失败，两个对照通过），真实 Redis 跨缓存边界一个及模式恢复六个；四包 race 通过。已验证部分场景，不是全仓收敛。下一轮 RR-20260914-01、WAL 错误/取消、跨节点水位。[运行](REVIEW-2026-09-14.md)。

收尾独立验收：合并作者修复 `885ff4f585508b7f157b89419f83d05bf0a7b5d8`（U-0189，Enter/Leave 共用未知结果恢复），解决文档索引冲突时保留两个 RR 与作者说明。原触发按新契约适配：权威确认切换成功允许返回 nil，发送前失败仍必须报错；核心 live 模式和写准入断言保留。四个模式场景及 Leave 连续三次写准入均 PASS（overlay 1.656s）。RR-12/13 现均已独立验收，旧失败证据保留；未验证真实 Redis/跨进程。最终统计：24 轮、16 篇机制文档、36 RR，索引 36 已修复、0 未修复；不是全仓审完。

第十轮：已完成 24 份运行记录、16 篇机制文档，RR 共 36（34 标修复、2 未修复），口径见[统计](PROGRESS-SNAPSHOT-2026-09-13.md)。Core `7be357c`，Kit/Codegen 同第九轮，pull 无增量。LeaveShared 后续三次准入出现 P2 RR-13；remoteentity race 包通过，状态为已验证部分场景。WAL 交错仍待验证，图谱旧代、无本轮真实 Redis。下轮 RR-12/13、WAL 和真实模式切换。[运行](REVIEW-2026-09-13-10.md)。

第九轮：Core `99be1ebc6191cf3e64acb22e39a8c439d7f3eea6`；Kit/Codegen 同第八轮，三仓 pull 无增量。ownership Enter/Leave 四个可控场景：Enter 丢回复出现 P2 RR-12，其余含两个对照及 Leave 观察；已验证部分场景。WAL Close/Sync 源码已读，交错未验证；图谱旧代、无真实 Redis。下轮 RR-12 修复、Leave 恢复、真实 Redis 和 WAL writer 屏障。[运行](REVIEW-2026-09-13-09.md)。

第八轮：Core `66f65e258bc5655d929e54300dc9a18377ff7e3b`，Kit `ac1a8801604bde4b7ec5ed60104502c542602ed0`，Codegen `cacd627b70991c5d0e38545866610db78e695b53`。状态：已验证部分场景。RR-01/05 第七轮残余、RR-02 同进程代际、RR-09 真实 Redis、两个 WAL 原故障、Mail 原拒绝、Nest 回复释放及饱和子进程通过；五个 Core 包 race 通过。图谱旧代，未验跨节点墓碑、跨进程回拨、Linux failover。下轮：EnterShared/LeaveShared 未知结果、共享 L2 重放、Sync/Close 交错。[明细](REVIEW-2026-09-13-08.md)。

第七轮：Core `76c6bac305c9c0408aa2966ad0ae05779dd50047`，Kit `ac1a8801604bde4b7ec5ed60104502c542602ed0`，Codegen 同第六轮。entity/remoteentity/cache 已验证部分场景：RR-08、RR-06 原/适配复现通过，RR-01/05 部分修复；RR-02 原场景通过但 Windows 重启代际测试两次失败。nest/nestwal/mail 仅包回归，原故障独立验收未完成。图谱旧代，无本轮真实后端。下轮先 Transfer/WAL/完成链/Mail 原复现。[运行](REVIEW-2026-09-13-07.md)。

收尾同步：首次推送因远端前进被拒，已正常 merge `06fdd2dc319856c6645ddfb79eba6c4171f681c0`。该提交新增 U-0180/0181/0183/0184 和 U-0175 补充修复，涉及 RR-20260913-02/05/06/08、RR-20260911-06。以上测试仍对应原审查 SHA；新修复待下一轮独立验收，旧状态描述是历史快照。保留远端源码和 bugfix 说明，未将其当作本轮已验证。

2026-09-13 第六轮：Core `d1b14b99a9fcf9ed4029966ac55372407b5a52a4`；Kit/Codegen 同第五轮，三仓 pull 无变化。mirror/envelope.go 与 syncbus/sync.go 已源码补证；三个生命周期场景及两包 race 通过，状态为已验证部分场景。旧 bug 未关闭，无新增 RR；图谱旧代，未测真实 broker。下一入口：实际总线解绑与回调完成。[运行](REVIEW-2026-09-13-06.md) · [机制](IMPLEMENTATION-MIRROR-LIFECYCLE.md)。

最后更新：2026-09-13。状态描述证据深度，不表示整个包已审完，不使用覆盖百分比。

## 2026-09-13 第五轮：Mirror 实施交接

Core `d7832e8249a4b8dca123fa7e28268fa05e78118a`；Kit `3855c71f5aaaf5ca4b7091ae17bf8cb0e6943b79`；Codegen `cacd627b70991c5d0e38545866610db78e695b53`。三仓 pull 无变化。

| 范围 | 本轮证据/状态 | 下一步 |
| --- | --- | --- |
| remoteentity ReadRemoteSnapshot/BindSync | 图谱定位与 stale coverage 后源码补证；已读直接权威出口和未启动 replicator 所有权；无新增运行测试 | 抽取共享 client 时统一准入与关闭语义 |
| Mirror 设计 | 已写六步实施交接；明确代际、墓碑、首载边界；尚未实施 | reader/expiry，再删除与兴趣协议 |
| 修复状态 | 沿用第四轮五项通过、RR-08 部分修复；无新源码 | 有增量后复测；不重复登记 |
| kit/codegen | 同步和方案分工，无新增源码覆盖 | 装配与真实生成消费者验收 |

[运行记录](REVIEW-2026-09-13-05.md) · [实施交接](PLAN-REMOTE-POLICY-MIRROR.md)。本轮未新增 Go/Redis 测试，历史验收见下方；图谱旧代限制仍在。

## 2026-09-13 第四轮最新进度：六项修复验收

Core 已从 `74af2af` 快进到 `e4f07b06cea450dfc4ab22ca8e3f7f39db22b81a`；Kit `3855c71f5aaaf5ca4b7091ae17bf8cb0e6943b79`、Codegen `cacd627b70991c5d0e38545866610db78e695b53` 无增量。

| 范围 | 实际证据 | 状态/下轮 |
| --- | --- | --- |
| RR-03 payload / RR-04 waiter | 原复现均通过，gap 正向通过 | 已修复并独立验收，非全包无问题 |
| RR-07 schema / RR-10 大版本 / RR-11 epoch | 原真实 Redis 复现均通过；codec/max uint64/四类溢出不写通过 | 已修复并独立验收；未测集群 failover |
| RR-08 expiry | 原 Cached 通过；Monotonic/Linearizable 权威返回及同版本回填失败 | 部分修复，继续原编号，优先统一 read 出口 |
| entity/remoteentity/cache/redis/mirror | 五包完整 race 通过 | 不代替三个新失败断言 |
| 所有权未知结果/L2 apply | 本轮因新增修复优先验收，未继续扩展 | 仍待设计与修复 |

[运行](REVIEW-2026-09-13-04.md) · [验收](../bug/REVIEW-2026-09-13-04.md) · [机制更新](IMPLEMENTATION-REMOTE-L1-L2-CONSISTENCY.md)。临时 Redis 已关闭；历史状态保留在下方各轮快照。

## 2026-09-13 第三轮最新进度：真实 Redis 与未知结果

Core `18a3790c463011050e4f05a5878d300c6144fdfe`；Kit `3855c71f5aaaf5ca4b7091ae17bf8cb0e6943b79`；Codegen `cacd627b70991c5d0e38545866610db78e695b53`；同步无增量，无新修复。

| 范围 | 实际证据 | 状态/下一入口 |
| --- | --- | --- |
| Transfer 未知结果 | 真 Lua 成功后丢回复，旧 owner 仍准入；发送前失败对照通过 | RR-09 P2；真实 commit fencing、其他模式切换待验 |
| L2 大版本 | 真 Lua 两条边界失败 | RR-10 P3；精确域与迁移策略待定 |
| marker 大 epoch | 写入科学计数法，后续不可读 | RR-11 P3；Leave/Transfer 同类路径待验 |
| 旧 RR-05/07 | 真实 Redis 复现 | 仍未修；升级证据而非重复编号 |
| Redis 正常并发/切换 | 32 snapshot 发布最大值、PTTL、唯一 claim、stale CAS 通过 | 有界真实后端验证，非 HA/性能验证 |
| remoteentity/entity/redis | 三包 race 基线通过 | 不覆盖未知结果业务提交 |

[运行](REVIEW-2026-09-13-03.md) · [问题](../bug/REVIEW-2026-09-13-03.md) · [机制](IMPLEMENTATION-REMOTE-REDIS-UNCERTAIN-OUTCOMES.md)。临时实例已关闭，真实 Mongo/订阅恢复仍待继续。

## 2026-09-13 第二轮最新进度：L1/L2 一致性

Core `2724442d886d9bb3ee5617d7ded814ce0f5267cc`；Kit `3855c71f5aaaf5ca4b7091ae17bf8cb0e6943b79`；Codegen `cacd627b70991c5d0e38545866610db78e695b53`。无源码增量，无新修复。

| 范围 | 实际证据 | 状态/下一入口 |
| --- | --- | --- |
| L2 冲突→ReadThrough→L1 | 真实组合吞冲突；网络降级对照通过 | RR-05，错误分类待修 |
| L2 读响应与 Publish 交错 | 屏障验证同版本回填覆盖 | RR-06，统一 apply 边界待修 |
| L2 schema/codec 规则 | schema 实测接受；本地拒绝；Lua 源码比较核对 | RR-07，真实 Redis/codec/数值域待验 |
| 快照 ExpiresAt | 已过期仍命中 | RR-08，绝对期限与 TTL 组合待修 |
| 权威回填失败后重试 | 下次读成功，未遗留失败 call | 部分场景通过；后台恢复未验 |
| entity/cache/remoteentity | 三包完整 race 通过 | 非全链路无问题证明 |
| kit/codegen | 仅同步，无新增行为覆盖 | 保留后续轮转 |

[运行](REVIEW-2026-09-13-02.md) · [问题](../bug/REVIEW-2026-09-13-02.md) · [实现学习](IMPLEMENTATION-REMOTE-L1-L2-CONSISTENCY.md)。

## 2026-09-13 最新进度：Remote 复制与恢复

Core `617738b1cb61f8a4f35f6d5e8365d2f525a08b0b`，Kit `3855c71f5aaaf5ca4b7091ae17bf8cb0e6943b79`，Codegen `cacd627b70991c5d0e38545866610db78e695b53`；三仓同步无增量。旧 bug 与 Mirror 方案未修/未实施。

| 范围 | 已验证 | 状态与下一入口 |
| --- | --- | --- |
| Remote 发布→Replicator→SnapshotReplicaStore | 两种删除乱序失败；upsert 乱序及顺序删除通过 | RR-20260913-01；真实 L2/重投待验 |
| Interest 发布/接收 | 旧 release 撤销新 renew；顺序对照通过 | RR-20260913-02；generation 与重启 SID 待验 |
| Remote payload 身份 | scope 与外层不一致仍写入 | RR-20260913-03；其他字段/interest 身份待验 |
| delta gap → 权威 loader | full v3 回填一次通过 | 部分场景；失败重试/迁移待验 |
| RemoteSnapshotCache 合并读 | 取消后空闲名额仍被拒绝 | RR-20260913-04；Manager fallback 放大待验 |
| room NATS/JetStream wrappers | 源码确认无版本过滤、错误不重试；七包 race 基线通过 | 已读接口边界，未跑真实网络 |
| kit/codegen | kit room 装配源码，codegen 仅同步 | 不计为新增全仓覆盖 |

[运行](REVIEW-2026-09-13.md) · [问题及限制](../bug/REVIEW-2026-09-13.md) · [实现学习](IMPLEMENTATION-REMOTE-REPLICA-ORDERING-AND-RECOVERY.md)。

## 2026-09-12 第二轮最新进度：RemotePolicy Mirror

Core `ef44d770fc231896187b6e6b104d10ffa965bb01`；Kit `3855c71f5aaaf5ca4b7091ae17bf8cb0e6943b79`；Codegen `cacd627b70991c5d0e38545866610db78e695b53`。本轮按用户要求聚焦 Mirror，三仓同步无增量。

| 范围 | 实际证据 | 状态/下一入口 |
| --- | --- | --- |
| Mirror policy / factory / Nest / codegen | 当前源码确认只有声明，没有自动只读/订阅；相关生成器测试通过 | 已读接入路径；Mirror 生成消费者写能力待实测 |
| RemoteSnapshotCache 作为 Mirror 基础 | 临时 race：Mirror kind 可接入、旧 upsert 保护、读出隔离通过；Delete 后旧值可再入 | 原语部分验证；版本墓碑为方案必需项 |
| Replicator / Remote sync / interest / entitysync | 关键源码及五包 race 通过 | 基础可复用；首次加载水位、真实重投/L2/停机交错待验 |
| 只读实现方案 | 核心 reader、轻量 kit 装配、codegen 封口；P0–P3 分阶段 | 仅文档，未实施 |

[运行](REVIEW-2026-09-12-02.md) · [实现及方案](IMPLEMENTATION-REMOTE-POLICY-MIRROR.md) · [观察](../bug/REVIEW-2026-09-12-02.md)。旧 RR 状态未变，历史进度保留。

## 2026-09-12 最新进度：饱和回退与真实 WAL

Core `c9e853e08c91d498b65d6f1d6e4dd35d39726a51`；Kit `3855c71f5aaaf5ca4b7091ae17bf8cb0e6943b79`；Codegen `cacd627b70991c5d0e38545866610db78e695b53`。三仓 pull 无增量；下方历史快照不代表当前覆盖终点。

| 范围 | 本轮证据 | 状态/后续 |
| --- | --- | --- |
| kit/mail RR-20260911-05 | 原 race 仍失败，无修复记录 | 仍未修复，MemoryStore 范围 |
| nest completion 饱和回退 | 原 RR-20260911-06 仍复现；正常子进程通过，panic 子进程退出 2 | 已验证组件饱和崩溃；继续原编号，待修复统一异常边界 |
| nestwal Enqueue/held/replay/Ack | 真实文件：held 队首挡住后继，补释放推进两条，Ack 重开后保持 | 已验证部分场景；外部 applier/publisher 为替身 |
| nestwal Close/reopen | 已持久化但未释放的两条记录，关闭重开后恢复 | 已验证正常关闭重开；非断电/强杀验证 |
| nestwal Shutdown/Flush/replayMu | 后台 apply 期间短 context 不使 Shutdown 按时返回 | 新 P2 RR-20260912-01；并发 Flush/超时后重试待扩展 |
| nestwal Sync/collectBatch | Sync 返回成功时 ticket 尚未写入；同配置 Close 对照通过 | 新 P2 RR-20260912-02；默认窗口概率和高并发准入待验 |
| nest/nestwal/worker 基线 | 三包完整 race 通过 | 不能代替上述失败边界验证 |
| codegen | 无增量，仅同步 | 本轮无新增覆盖 |

[运行记录](REVIEW-2026-09-12.md) · [问题](../bug/REVIEW-2026-09-12.md) · [复现](../bug/REPRO-2026-09-12.md) · [实现学习](IMPLEMENTATION-WAL-ADMISSION-DURABILITY-AND-SHUTDOWN.md)。下一轮先复核四个未关闭问题，再继续真实 Nest→WAL 释放整合、projector 取消协调与 Ack 失败恢复。

## 2026-09-11 第四轮最新进度

Core `dd1f270c1f044022df37a887f6510e8e26b90dab`；Kit `3855c71f5aaaf5ca4b7091ae17bf8cb0e6943b79`；Codegen `cacd627b70991c5d0e38545866610db78e695b53`。三仓 pull 无增量，下方为历史快照。

| 范围 | 实际证据与状态 | 下一入口/限制 |
| --- | --- | --- |
| kit/mail 容量拒绝 | RR-20260911-05 原 race 复现仍失败，无新修复 | 维持 P3；MemoryStore 限定 |
| nest completion / RollbackTx.Commit / worker.SafeFunc | 新增回调 panic race 复现失败，RR-20260911-06 P2；nest/worker 包 race 通过 | 已验证部分场景；优先修复回复/释放必达，饱和回退异常待验 |
| Nest Shutdown 重复等待 | 慢 ticket、慢 AfterCommit 两组均通过；两次短超时后仍正常收尾，释放一次 | 已验证模拟场景；真实 fsync、Linux 压力未验 |
| nestwal/projector held/release | 图谱定位并读取当前实现，通知丢失会阻挡 held 记录的推断 | 源码已读；真实 WAL/投影端到端待验 |
| codegen | 无源码增量，未重复审查 | 本轮无新增覆盖 |

[运行记录](REVIEW-2026-09-11-04.md) · [问题](../bug/REVIEW-2026-09-11-04.md) · [复现](../bug/REPRO-2026-09-11-04.md) · [实现学习](IMPLEMENTATION-COMPLETION-FAILURE-AND-SHUTDOWN.md)。下轮先看 RR-20260911-05/06 的 bugfix，再继续饱和回退/真实后端收尾。

## 2026-09-11 第三轮最新进度

Core `4e8f5ece714a848d8b3981333cba22d4e970f57e`；Kit `3855c71f5aaaf5ca4b7091ae17bf8cb0e6943b79`；Codegen `cacd627b70991c5d0e38545866610db78e695b53`。三仓已同步；U-0170～0173 原触发独立验收通过，旧无期限墓碑的兼容风险仍按 bugfix 披露。下方第二轮及更早结论为历史快照。

| 范围/入口 | 实测与问题 | 证据状态、限制与后续 |
| --- | --- | --- |
| remoteentity deferRemoteClose / StopFinalizer / batch.Close | 原 64 batch 通过，新增 Close/Stop 并发通过，包 race 通过；RR-20260911-03 已验收 | 已验证部分场景；真实分布式锁释放、慢后端待验 |
| nest Request / requeue / Dispatcher stop | 原显式 delay 通过；真实内部重排 Request 停机及停止后重排回复通过，包 race 通过；RR-20260911-04 已验收 | 已验证部分场景；慢 completion ticket/回调与 Linux 压力未验 |
| kit/mail clone / retention / Deliver | RR-20260911-01/02 原触发通过，包 race 通过；新拒绝原子性失败，对照通过 | 已验证部分场景；新 P3 RR-20260911-05，MemoryStore 范围；优先修复后复跑 |
| codegen | 同步无增量，未重复源码消费者实验 | 本轮无新增覆盖 |

[运行记录](REVIEW-2026-09-11-03.md) · [问题](../bug/REVIEW-2026-09-11-03.md) · [复现](../bug/REPRO-2026-09-11-03.md) · [实现学习](IMPLEMENTATION-MAIL-RETENTION-AND-ATOMIC-REFUSAL.md)。继续入口：先验新拒绝原子性，再轮转慢持久化完成或真实后端停机；本轮未重跑性能基准。

## 2026-09-11 第二轮最新进度：Remote / Nest

Core `31ffe275645ae04f5376c748feb31aa0422b6e6d`；Kit `4e69d2adf2ac02d33a25b263d2d2f2378ba81401`；Codegen `cacd627b70991c5d0e38545866610db78e695b53`。三仓 pull 无变化，跳过已审且未变的正常链路，本轮专项未重验 Mail。

| 范围 | 证据与状态 | 后续及限制 |
| --- | --- | --- |
| Remote Close / StopFinalizer | 当前源码、图谱补证、race 复现失败；RR-20260911-03 未修复 | 已验证部分场景；等待修复后复测，未验真实后端清理 |
| Nest delayed Request / Shutdown | 有效 handler 的 race 复现失败；RR-20260911-04 未修复 | 已验证部分场景；内部 requeue 停机待测 |
| completion / tracker 性能 | 选定两包 race 通过；锁等待和满容量微基准完成 | 模拟场景单次 Windows 样本；Linux 热点、慢 ticket/回调、txMu 争用未验 |

[运行记录](REVIEW-2026-09-11-02.md) · [问题](../bug/REVIEW-2026-09-11-02.md) · [复现](../bug/REPRO-2026-09-11-02.md) · [实现与性能](IMPLEMENTATION-REMOTE-NEST-LIFECYCLE-AND-PERFORMANCE.md)。已从中断处完成本轮有界范围，未确认中断原因。下方为历史快照。

## 2026-09-11 当前进度

Core `e22e815934293d3bc25a84f37338f9576f9d5c4d`，Kit `4e69d2adf2ac02d33a25b263d2d2f2378ba81401`，Codegen `cacd627b70991c5d0e38545866610db78e695b53`。三仓已同步；上轮收尾待验收四项及新到四项，共八项原触发独立验收通过。下方 09-10 表格及未验收描述保留历史，当前状态以本节为准。

| 范围/入口 | 本轮证据 | 状态与限制 | 下轮入口 |
| --- | --- | --- | --- |
| core/room 构造；kit/match Enqueue | 原 overlay 与 race 包通过，U-0163/0164 已验收 | 已验证部分场景；无真实后端压力 | 派生默认值、终态请求保留 |
| core/remoteentity tracked/wait/prune | 原容量交错与新增 TTL 清理等待者通过，U-0168 已验收 | 已验证部分场景；真实 finalizer 停机交接尚未验 | StopFinalizer 与晚到 batch Close |
| core/skill checkpoint/restore/retention | 原双 Host 端到端淘汰测试及取消场景通过，U-0169 已验收 | 已验证部分场景；旧快照缺完成顺序仍退化；RootEvent/ProcLedger 未验 | 非单调合法事件输入与恢复后的保留行为 |
| kit/mail evict/SettledClaims/clone | 原短序列与 race 包通过，U-0165 原触发已验收；新增两项独立失败 | 已验证部分场景；新 P2 RR-20260911-01、P3 RR-20260911-02；仅本地替身 | 墓碑期限、迟到 Commit、副本所有权 |
| codegen/entity parse/run、registry aliases | U-0162/0166/0167 已验收；真实 CLI 保护已有输出，四个消费者编译通过 | 已验证部分场景；源码 HEAD，非发布 tag | 更多别名/标记组合及真实 bootstrap |
| category 改值与旧 ID/存储键迁移 | 本轮未新增动态验证 | 待复核，不以先前源码观察作确认 bug | 真实存储键、混合版本消费者 |

[运行记录](REVIEW-2026-09-11.md)、[新增问题](../bug/REVIEW-2026-09-11.md)、[复现](../bug/REPRO-2026-09-11.md)、[机制更新](IMPLEMENTATION-CHECKPOINT-AND-REPLAY.md)。下轮从本节 SHA 获取增量，先看新 RR 的 bugfix，再按表中入口轮转。图谱仍为 09-08 generation，当前证据依赖源码补证，不宣称最新图谱完整覆盖。

## 收尾同步与下一轮优先项（09-10 历史）

提交前收到 U-0162～U-0165，已同步 Core `b2ae333685803846d75cf71e91e1412e6eaccad9`、Kit `4e69d2adf2ac02d33a25b263d2d2f2378ba81401`、Codegen `655b2de00f9b77e45086e72431c13cec94c2f336`。作者标记 RR-20260909-05/06、RR-20260910-01/02 已修复；本轮实测截止在下方基线，新到修复尚未独立验收。下一轮首先以原复现验收这四项，并补 Mail 墓碑上限先于信封过期耗尽的边界；之后再按第三轮表轮转。当前检出/下轮增量起点用本节 SHA，历史实测基线不要替换。

## 2026-09-10 第三轮增量（实测基线）

Core `f3eaad9b38f87b2873e6f35b517b4ad993aed680`，Kit `c4e7cef1029fb6326fa88b84c622f059bc62c0c4`，Codegen `8c38eeb2a183c1519f8a28e102133282e29c9670`。已快进新增 M-01～M-05，Kit 无新增；下方各轮表保留历史基线。

| 范围/入口 | 新增证据 | 状态与限制 | 下一入口 |
| --- | --- | --- | --- |
| core/entity factory、idgen/resolver、category_order、guard；nest | 注册表权威和派生锁序，entity/nest race 通过 | 已验证部分场景；图谱旧代，源码补证；无混合版本迁移测试 | 旧 ID 与 category 改值后的实际存储键 |
| codegen/entity parse/gen、registry gen、roost add/workflow | 相关三包通过；三个消费者一绿两红 | 已验证部分场景；RR-20260910-05/06，M-05 新增 | 修复验收、别名/标记组合、真实 bootstrap |
| core/remoteentity transaction_manager | 等待/完成/容量淘汰；包 race 通过，独立淘汰复现 panic | 已验证部分场景；RR-20260910-03；没有真实后端故障 | 等待者生命周期修复、finalizer 停机交接 |
| core/skill checkpoint、retention、Cancel、process | 包 race；独立 Host 恢复保留顺序失败；取消恢复与停止失败重试通过 | 已验证部分场景；RR-20260910-04 仅历史诊断差异 | 完成顺序修复、RootEvent/ProcLedger 保留恢复 |
| kit/mail Send、Deliver、回执与领取 | 包及回执补写/部分广播重试 race 通过 | 已验证部分场景；仍有 RR-20260910-02 淘汰边界；MemoryStore | 资产幂等期限、异步投递确认与重放 |

[运行记录](REVIEW-2026-09-10-03.md)、[生成机制](IMPLEMENTATION-CATEGORY-REGISTRY-AND-GENERATION.md)、[恢复重放机制](IMPLEMENTATION-CHECKPOINT-AND-REPLAY.md)、[复现](../bug/REPRO-2026-09-10-03.md)。本节旧问题状态只描述实测快照；提交前新到 U 系修复及续跑 SHA 以页首收尾同步节为准，先独立验收，再做增量与上述未验证入口。上一段审批额度中断未执行的命令已在本轮重新验证，未以未执行结果计入进度。

## 2026-09-10 第二轮增量

Core 76664a51be77bdeb79a62cf344d5cee0ecc15daa，Kit c4e7cef1029fb6326fa88b84c622f059bc62c0c4，Codegen aa072edb35a0b97d234e0155142e36295c5f21d1；pull 均无新提交。

| 范围 | 入口与证据 | 状态/限制 | 后续 |
| --- | --- | --- | --- |
| core/nest、remoteentity | 预声明、prepare、Finalize/Commit/Abort/Close、快照预加载；两包 race 通过，部分 prepare 失败后再准入通过 | 已验证部分场景；仅替身后端 | finalizer 停止/队列、真实故障 |
| kit/service/mail | Deliver/evict 与领取链组合；包 race 通过，新淘汰重投复现失败 | 已验证部分场景；新增 P2 RR-20260910-02；未连接资产服务 | 去重记录保留、部分 fanout 恢复 |
| codegen | 同步与旧问题记录核对 | 待复核，本轮未新增源码阅读 | 等待显式输出修复验收 |

[第二轮运行记录](REVIEW-2026-09-10-02.md)及[远程实现学习](IMPLEMENTATION-REMOTE-PREPARE-AND-FINALIZE.md)。RR-20260909-05/06、RR-20260910-01 无新修复；连同本轮新问题均留待复核。下方为此前证据。

## 2026-09-10 增量进度

三仓 pull 均无新增：Core 8ea815930b4b32bf59cb4a894e50782687046e0d（相对第四轮仅文档变化），Kit/Codegen 与下方相同。下方第四轮表保留历史证据。

| 模块 | 本轮入口与验证 | 状态与限制 | 下轮入口 |
| --- | --- | --- | --- |
| core/room | RoomManager Create/Get/Remove/Close/expireIdle；Broadcaster Close/Stop；包 race 通过 | 已验证部分场景；新 P3 RR-20260910-01；真实下游未验 | 退休重试与下游故障 |
| core/skill | scheduler、Cancel；包 race 及取消 wait 后不触发伤害测试通过 | 已验证部分场景；未覆盖取消失败/宿主重入 | checkpoint 与取消失败 |
| kit/service/match | Sweep/expireLockedLimit/QueueLength；包 race 及过期重新入队测试通过 | 已验证部分场景；RR-20260909-05 仍未修复 | 历史状态保留容量与后端故障 |
| codegen/internal/entity | 核对 SHA/bugfix，无新提交或修复记录 | 待复核；RR-20260909-06 仍未修复；本轮未重复生成实验 | 修复后独立验收 |

证据：[本轮运行记录](REVIEW-2026-09-10.md)、[Room/Skill 实现学习](IMPLEMENTATION-ROOM-AND-SKILL-SCHEDULING.md)。下一轮先读未关闭问题的 bugfix，再转 Remote Entity/Mail 回执；Room 的实现位于 core/room，不能沿用 kit/service/room。

## 第四轮源码基线（历史）

- Core：`1f7bb5425a8b82c774bac9bbf40048f44ea3de99`。
- Kit：`c4e7cef1029fb6326fa88b84c622f059bc62c0c4`。
- Codegen：`aa072edb35a0b97d234e0155142e36295c5f21d1`。

以下“第四轮”指以上对应仓库 SHA；历史行以链接运行记录中的 SHA 为准，不冒充最新代码验收。

| 模块/路径 | 最近审查 | 入口与不变量 | 实际验证与问题 | 状态/限制 | 下一入口 |
| --- | --- | --- | --- | --- | --- |
| core/entity/entity_guard.go | 第四轮 Core | Acquire/Release，新增组锁必须保持顺序 | entity race 包测试通过 | 已验证部分场景；未压测多服锁竞争 | guard 跨事务释放 |
| core/nest/cast.go、msg.go、group_transition.go、nest_dispatch.go | 第四轮 Core | CastMulti；远程实体必须预声明，失败仅回滚本次锁 | nest race 包测试通过 | 已验证部分场景；未做真实远程故障注入 | 预分派批量远程锁失败 |
| core/dataengine/engine/assembly.go、runtime.go | 第四轮 Core | Shutdown 未完成保留重试入口；分阶段关闭 | 原 RR-20260909-03 overlay 与包 race 通过 | 已验证部分场景；未跑完整部署 | 各组件独立失败/重启恢复 |
| kit/service/session/service.go | 第四轮 Kit | 冲突清理先释放旧 claim，后丢弃会话 | 原 RR-20260909-02 overlay 与包 race 通过 | 已验证部分场景；真实后端未验 | 超时后重试及租约续期 |
| kit/service/mail/service.go、mailbox.go | 第四轮 Kit | Reserve/Commit/Cancel；稳定 token 与领取状态 | mail race 包测试通过，源码阅读领取链 | 已验证部分场景；奖励服务原子发放未验 | grant receipt 与跨服重试 |
| kit/service/match/queue_store.go、store.go、match_rpc.go | 第四轮 Kit | Enqueue/Ticket/Cancel/Commit；队列 CAS 与归属 | 包 race 通过；独立重放测试失败 RR-20260909-05 | 已验证部分场景；无真实 Redis/吞吐证据 | 修复验收、过期请求清理 |
| codegen/internal/entity/main.go、gen.go | 第四轮 Codegen | 多实体默认输出与显式 -output | 默认消费者测试通过；显式输出消费者编译失败 RR-20260909-06 | 已验证部分场景；不是全部模板组合 | 显式输出修复、混合实体 |
| codegen/internal/nest | 第四轮 Codegen | 相关回归包 | race 包测试通过 | 待复核；本轮未展开完整模板链 | handler 参数到消费者 |
| core/cache、versionstore | [第二轮](REVIEW-2026-09-09-02.md) | 等待取消与名额归还 | 原问题复现变绿 | 已验证部分场景；本轮未重审 | 后端失败与饥饿 |
| core/app、worker、entitysync、nettransport、lockstep | [架构轮](REVIEW-2026-09-09.md) | 生命周期、传播水位、输入边界 | 详见历史运行记录 | 待复核；历史证据不代表当前基线 | 真实装配后的停机与传播 |
| core/saga | [第三轮](REVIEW-2026-09-09-03.md) | 生命周期与补偿相关边界 | 相关 race 测试见历史记录 | 待复核；未覆盖全部补偿组合 | step replay 与 receipt |
| codegen consolidate、发布消费者 | [第二轮](REVIEW-2026-09-09-02.md) / [架构轮](REVIEW-2026-09-09.md) | import 拆分；正式 tag 接入 | 原 consolidate 复现通过；pure-tag 受网络限制 | 待复核 | 隔离正式 tag 消费者 |
| kit 其余服务与 core/skill 深层执行路径 | 本轮未审 | 尚未建立本轮有界机制证据 | 不以历史修复账本代替 review | 未审（本轮） | 优先 room/remoteentity，再 skill |

## 下轮顺序

1. fetch 三仓，从本页 SHA 做增量；先核验 bug/bugfix 新记录及 RR-05、06。
2. 展开 Match 过期清理、Mail 发奖回执与 Remote Entity 预分派，补足本轮边界。
3. 轮转 Room/Skill，保持每轮新增机制阅读；不要只重复旧复现。
4. 维护本表、主题实现文档、每轮记录与问题索引，再提交推送。

本轮完整证据：[第四轮运行记录](REVIEW-2026-09-09-04.md)。
