# Roost Review 跨轮进度

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
