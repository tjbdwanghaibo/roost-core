# Service 审查完成矩阵（2026-09-29）

## 最新：第八批 Mail 修复与第十轮邻接审查

基线68cf87fc加本轮源摘要，[运行](REVIEW-2026-09-29-services-10.md)。**RR-33原触发/声明场景已修已验；新RR-34 P2未修。**10/10域主链有界整理保持完成，100路径97复用/3Mail变化复读，外加相关Redis/cache专项。

| 层级 | 当前事实 | 仍未完成 |
| --- | --- | --- |
| Mail跨槽读取 | 有界Pipeline，原2叶子绿，Mod普通prefix跨3owner16封分页/claim通过 | 窄客户端补Pipeline、旧读取owner升级，未部署 |
| Pipeline邻接错误 | 生命周期控制通过；standalone/Cluster首缺失后HSet错误被Exec吞掉，RR-34确认 | driver检查全部命令结果的实施/回归 |
| 回归与消费 | Mail race/count2 314pass事件280叶子0skip；18包965pass事件888叶子；Core/consumer编译、vet、12check绿 | 3Toxiproxy skip；真实资源/HA/強杀/迁移/長稳未验 |

新问题有复跑/实施入口；“主链整理完”不等于“所有逻辑无缺陷”。下方保留第九轮时点。


## 最新：第七批修复与第九轮专项收口

**本阶段10/10域主链及本轮具名本机专项已完成有界审查；RR-31/32已修复验收，新RR-33未实施。**[本轮](REVIEW-2026-09-29-services-09.md) · [两项修复](../bugfix/SERVICE-BUGFIX-2026-09-29-07.md) · [新问题](../bug/REVIEW-2026-09-29-services-09.md)。当前源码 bcebb805 加本轮内容，[100路径](evidence/service-review-20260929-09/inventory.csv)记录95不变blob复用/5变化复读，不冒称本轮逐行重读100文件。

| 层级 | 已完成 | 未完成 / 不计为完成 |
| --- | --- | --- |
| 10域源码主链/契约 | 10/10阶段范围，历史证据复用+当前差异核对；12RPC check和当前回归 | 逐行/分支/所有故障证明不是该分母 |
| 前轮两项缺陷 | RR-31/32 2/2修复，原8/8断言、新正式33叶子/37事件通过 | 旧prefix搬迁、已有write-once工程升级、历史goods被覆盖后的对账 |
| 本轮专项 | Activity多game恢复、purchase并发/失联/下架、Mail跨槽页/Pipeline、Match4规模两后端、永久ledger与fulfilled边界 | 新RR-33未修；归档/分页drain/fulfilled尚未实现 |
| 本机回归 | integration+race17包/909事件/841叶子、0test skip/fail；consumer107事件/100叶子、全编译；demo5/5、RPC12/12、vet | 不外推真实资产/资金/资源服务 |
| 外部/生产专项 | 具名接口与验收条件已交接 | allocator幂等、渠道/资产回执、强杀/分区/HA、生产迁移、长稳/热点SLO未执行 |

“本阶段service review完成”有明确源码/场景边界；“service完全无问题/全部生产验证完成”不成立。下一入口RR-33与具名设计/外部验收，不重开10域主链。下方保留原历史时点。

## 最新：第六批修复与第八轮审查

基线 db642494 加[本批修复](../bugfix/SERVICE-BUGFIX-2026-09-29-06.md)。[当前运行](REVIEW-2026-09-29-services-08.md)与[新增问题](../bug/REVIEW-2026-09-29-services-08.md)记录实际边界；本阶段10/10域主链有界整理保留，全service仍不能称无缺陷。

| 层级 | 本轮状态 | 未完成/未验证 |
| --- | --- | --- |
| 第七轮缺陷 | RR-28/29/30 **3/3修复/原场景验证**，旧15叶子全绿 | 历史丢pending索引自动对账、旧错误prefix迁移 |
| 新正式回归 | 43叶子/51事件；完整17包/826事件/764叶子通过 | test skip=0，servicemetrics无测试package skip=1；未穷尽故障组合 |
| 第八轮新缺陷 | RR-31/32两个P2，实际反例确认 | **0/2实施**；Activity Cluster准入、首次grant内容固定 |
| 新审查证据 | 8次叶子执行3pass/5预期fail；生成消费者编译/已有测试通过 | 真实资产/渠道、HA/网络/强杀、归档及长期容量 |
| 范围进度 | 延续100路径/10域主链整理，新增具名专项 | 不当行/分支覆盖；K1/K2/K3与全仓专项不自动升级状态 |

下方保留原时点“未修/无新增”，不推翻原第五批4/4已验收。

## 第七轮历史快照：1625fee0

**Service 主链有界整理仍为 10/10 域；本轮维护/Cluster 专项确认三项新缺陷，均未实施。**[问题与交接](../bug/REVIEW-2026-09-29-services-07.md)、[运行记录](REVIEW-2026-09-29-services-07.md)、[机制学习](IMPLEMENTATION-SERVICE-INDEX-MAINTENANCE-AND-CLUSTER.md)。本轮未改生产源码；上轮 100 路径清单与 4/4 修复验收沿未变源码复用。

| 当前层级 | 完成状态 | 未完成/未验证 |
| --- | --- | --- |
| 原第五批缺陷 | 4/4 原触发与声明场景已修/已验证 | 不外推到新触发 |
| 第七轮缺陷 | RR-28 P1 支付 pending 竞争；RR-29/30 P2 Cluster 启动校验，均有真实反例 | **0/3 修复** |
| 本轮场景 | 15 叶子：9 正常控制通过、6 确认反例失败；四包268既有测试/子测试通过 | 未穷尽所有维护/故障交错 |
| 本机 Cluster | 三 master 同槽/不同槽配置与业务脚本已验证 | 没有 replica、HA、failover/网络分区/跨进程强杀 |
| 生产专项 | 具名剩余范围已有交接 | 实际支付/allocator、旧数据对账、归档、长稳容量仍未验 |

**当前不能宣布 service 完全收敛。**下方第六轮快照的“修后无新增”仅指该轮已列场景；本轮新问题不改变旧四项原触发已修的事实，也不取消主链整理已完成的进度。

## 第六轮修复验收快照：4b0837d7

**第五轮四项缺陷现已 4/4 修复并通过声明场景验证，修后邻接 review 没有新增确认缺陷。**[第五批实施/兼容](../bugfix/SERVICE-BUGFIX-2026-09-29-05.md) · [第六轮审查](REVIEW-2026-09-29-services-06.md)。Service 10/10 域主链整理完成，当前 100 路径（6 变更复读、94 blob 不变复用），[最新清单](evidence/service-review-20260929-06/inventory.csv)保存实际源码 SHA。

| 当前层级 | 完成状态 | 未计为完成 |
| --- | --- | --- |
| 本批缺陷 | RR-25/26/27、旧 RR-14-02 残余 4/4 已修复/声明场景通过；新的定向两模式各 27 叶子全绿 | 未外推到全部故障/历史数据 |
| Service 主链/契约整理 | 10/10 域；100/100 生产路径核算，源码 4b0837d7 | 不是逐行/分支正确率 |
| 回归/消费者 | 完整 16 包/830 事件绿测；后补两个邻接叶子，最终两模式共 54 叶子执行通过；编译/vet/生成消费通过 | 真实资金/allocator、跨进程强杀/HA/长稳性能 |
| 升级与旧数据 | 有明确实施和恢复说明；旧超容量窗口可轮转 | legacy 无计划 Opening 需同 key 恢复，遗忘 orphan/证明需对账；未生产迁移 |

**可以说本阶段主链与本批缺陷已收口，不能说 service 的所有生产逻辑已经完全收敛。**剩余容量/履约/真实资源系统专项见第六轮记录。下方完整保留第五轮历史快照，其中 FAIL、未实施、0/4 已由本节替代，不代表当前状态。

## 第五轮历史快照：b336ce62

**service 的 10 个功能域主链、存储契约与组装链已完成本阶段有界审查；问题修复和生产故障验证分别计算。**本阶段不再留“其余 service 随后再看”的未列范围。当前仍有本轮 3 个新问题、1 个旧活动残余未实施；正式资源分配、HA、强杀、真实支付/消息与性能专项仍未验证，不能称所有逻辑和交错已穷尽或已无 bug。

范围是 Core 单仓 `service/` 与 `kit/service/` 的 tracked 生产 Go 路径；source SHA `b336ce62f75ce0d763138bc8523c4695724214a2`。不把另一 agent 的其他模块纳入服务结论。当前 **100 个路径，82 个非生成、18 个生成**；[清单](evidence/service-review-20260929-05/inventory.csv)逐行保存源码 blob、证据方式与基线。这是范围核算，**不是代码覆盖率或逐行审完率**。

## 10 域闭环与证据

下表“完成”表示列出的公开主链/存储/组装契约已整理，每域均包含 Redis 构造、owner/client capability、手写 run/管理入口及生成运输；生成文件用当前 12 次只读一致性检查与抽样审查，不称逐行阅读。此前原触发修复状态见三份 bugfix 记录，本轮没有将历史绿测升级成全部故障交错已验收。

| 域（生产路径） | 已完成主链范围 | 本阶段证据 / 当前问题 | 专项未验证边界 |
| --- | --- | --- | --- |
| Account（10） | Login/identity、server/slot、名字预约、持久建角计划与发布、session/profile、Redis/Mod/RPC | 首轮至第四批源码与回归；本轮 pending 角色拒绝 SelectRole/Profile、读失败关闭准入、同 store 重建恢复，Memory/Redis PASS | 旧孤儿/legacy slot 自动恢复、跨进程强杀、业务封禁撤销策略 |
| Directory（4） | Normalize、Reserve/Commit/Cancel/Release、代次/token、过期、条件删除、Redis/Mod | RR-21 修复后的 DeleteIf/同 owner 重建 ABA 正式回归与 Redis 通过 | 自定义后端、HA、业务名字迁移 |
| Global route/lease（8） | Bind/Resolve、迁移 Begin/Complete/Abort、Acquire/Renew/Release、epoch/incarnation、Load、Redis/runner | 迁移旧租约正式回归；本轮旧 Acquire 阻塞、新 route/new lease 后拒绝旧 CAS，Memory PASS | 真实业务写入使用 fence、双进程迟到调用/强杀 |
| Activity（9） | Open/window、Notify/Advance、progress/participant/ledger、dispatch/ACK、owed/分页/runner/Admin | 普通恢复/计分/ACK 回归；本轮 Opening 回收晚确认、满容量与扫描漏项 Memory/Redis FAIL；旧 RR-14-02 残余 | game 真消费、超容量旧数据恢复、贡献接受项沿 CARRYOVER |
| Mail（12） | send 意图/ledger/envelope、direct/broadcast、邮箱状态与页读取、Reserve/Commit/Cancel、删除/淘汰/墓碑、Redis/runner | 三批源代码修复与正式 Memory/Redis 回归；第四批未领取删除不复活已通过 | 真实资产回执、在途删除产品契约、混合旧 owner 与旧数据迁移 |
| Match（11） | Queue identity、Enqueue/Cancel、候选/分组/Commit、Ticket→Match、expiry/sweep、Redis/runner | 提交未知读回正式证据；新 Group 非法大小 8 叶子 FAIL，正常控制 PASS；历史容量观察已去重 | 终态归档/幂等保留窗口、真实房间 allocator、长期公平/大对象压力 |
| Platform（10） | auth/callback 校验、Order/pending/index、attempt/backoff、失败分类/未知证明、Admin/对账、Redis/runner | RR-23/24 第四批修复已通过；未知效果拒绝退款/重发，同 store 重建；生成购买消费链编译/已有行为测试通过 | 实际渠道权威对账、真实资金、强杀/Redis HA、旧 pending 恢复 |
| Rank（9） | 编码/member、Submit/模式/Lua CAS、幂等 ring、Remove/Reset、Page/Rank/Around、Redis/RPC/runner | 编码与并发回归通过；新 int64 加法正负溢出真实 Redis FAIL，合法边界 PASS | 高热点容量/延迟、复杂 Remove/Reset 并发、编码旧数据迁移 |
| Session（12） | Enter/claim/request、Attach、Finish/Leave、Pending Release/Admin、rotating OwnerSource/Sweep、Redis/Mod/runner | 正式 ABA/cleanup/输出验证；本轮 3 页 sweep 清理 PASS；并发释放幂等 fixture 2 calls/1 effect PASS，均 Memory | 正式 allocator 的持久幂等与 allocation incarnation、进程/资源服务失联 |
| Chat（9） | policy/body/auth、ChannelRef、Append/Seq/History、请求去重/保留、Prune、Redis/RPC/runner | 频道隔离回归；有限窗口控制 Memory/Redis PASS；2 秒偏移过期漏清理 FAIL，RR-27 | 实际入口限流/授权、年龄清理 Gap 协议、多节点时钟/负载 |

辅助 6 路径：split 四文件、integration/doc.go、servicemetrics 别名。已复查 split CancelClaim 传递 Attempts、owner/client 对称装配与资产侧稳定 token；示例 grant 是空实现，绝非资产服务验收。integration 是测试契约说明，servicemetrics 是类型转发，不虚设第 11 个服务域。

## 证据如何复用

首轮源码 SHA `6b73289c` 的[主链记录](REVIEW-2026-09-29-services.md)覆盖当时 96 路径。当前逐路径比较 blob/变更，**57 个非生成路径无变化，复用首轮和后续记录**；**25 个非生成变更/新增路径按当前主链复读**，关联后续 review/bugfix 的执行证据；18 个生成文件按当前生成门禁与样本。不是这一轮重新完整阅读 100 文件。

前三批与第四批累计记录入口：
[两轮 19 项实施](../bugfix/SERVICE-BUGFIX-2026-09-29.md)、
[RR-19～22](../bugfix/SERVICE-BUGFIX-2026-09-29-03.md)、
[RR-23/24 与删除残余](../bugfix/SERVICE-BUGFIX-2026-09-29-04.md)。
当前 16 个有测试包 / 801 个测试及子测试通过，是回归基线；不当作 801 个独立故障场景。

第五轮新增 **21 叶子 × 两种运行模式 = 42 次执行，18 pass / 24 预期 fail / 0 skip**；Rank 始终真实 Redis；Account/Activity/Chat 切换真实后端；Global/Session/Match 历史观察仍为 Memory fixture。生成消费工程实际 handler/game 测试与编译通过，不把生成 CLI 依赖获取被阻止写成完整生成安装成功。

## 完成判定与后续范围

源码主链与契约整理：**10/10 域完成本阶段范围**。范围清单核算：**100/100 当前路径已归类**，不是源码行覆盖率。缺陷修复：本轮 **0/4**（3 新 + 1 旧残余），另此前第四批 **3/3 原触发修复通过**。上线级故障、外部资源系统及性能：**未完成，不计入已验证**。

下一步若实施 bugfix，按 RR-25/26/27 和 RR-14-02 的验收执行；若继续 review，转入表中具名专项或其他核心域，不再以“service 主链未整理”重复从头审。图谱仍有 freshness/方法解析限制，实际源码与行为证据为结论依据，详见第五轮运行。
