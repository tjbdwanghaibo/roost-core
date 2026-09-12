# Roost Review 跨轮进度

最后更新：2026-09-12。状态描述证据深度，不表示整个包已审完，不使用覆盖百分比。

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
