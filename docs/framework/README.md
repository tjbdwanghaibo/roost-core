# roost 框架文档

roost 框架整体的说明与实现，按 13 个分区（00 总览 + 01～12）各写两篇。源码基准是 tag `v1.23.0`（`28912cd6`），文中 `path:line` 都以它为准。

内容与旧文档（[USER_GUIDE](../USER_GUIDE.md)、[INTERNALS](../INTERNALS.md) 等）冲突时，**以框架文档与源码为准**。

- 从这里开始：[00 总览（说明）](guide/00-overview.md) · [00 总览（实现）](impl/00-overview.md)
- 已知问题：[FRAMEWORK-DOCS-FINDINGS-2026-10-07](../review/FRAMEWORK-DOCS-FINDINGS-2026-10-07.md)

## 目录

- [怎么读](#怎么读)
- [两套文档的关系](#两套文档的关系)
- [分区目录](#分区目录)
- [术语表](#术语表)
- [包 → 分区索引](#包--分区索引)
- [已知问题入口](#已知问题入口)
- [维护这套文档](#维护这套文档)

---

## 怎么读

| 读者 | 从哪里开始 | 接着读 |
| --- | --- | --- |
| **维护者**（了解整体、做决定） | [说明篇 00 总览](guide/00-overview.md)：三大块、包地图、三条端到端走读 | 按关心的分区读说明篇 `guide/NN-*.md`；每篇先看“速览”和 §7“保证与不保证” |
| **review agent**（找缺陷、核对实现） | [实现篇 00 总览](impl/00-overview.md)：依赖图与层守卫、走读时序、全局不变量索引、建议的 review 顺序 | 按 [impl §6.1](impl/00-overview.md#61-建议顺序) 的顺序读实现篇 `impl/NN-*.md`；每篇的 §4 不变量、§10 review 检查点、§11 源码疑点是重点 |
| **业务作者**（写 handler、配 Mod） | 说明篇 00 的 [§4 端到端走读](guide/00-overview.md#4-端到端走读) | 相关分区说明篇的 §4“怎么用”与 §5“配置” |
| **运维** | 说明篇 00 的 [§6 运维入口](guide/00-overview.md#6-运维入口) | 各分区说明篇 §6“运行与运维”、[TROUBLESHOOTING](../TROUBLESHOOTING.md) |

每篇的开头都有“速览”（是什么、最重要的保证、最容易踩的坑）和“本篇覆盖的包”。只想知道某个包归谁，查 [包 → 分区索引](#包--分区索引)。

---

## 两套文档的关系

| | 说明篇 `guide/NN-*.md` | 实现篇 `impl/NN-*.md` |
| --- | --- | --- |
| 读者 | 维护者、业务作者、运维 | review agent、改框架代码的人 |
| 回答 | 是什么、为什么这样设计、怎么用、怎么配、怎么运维、保证什么 | 代码在哪里、数据结构、控制流 / 状态机、不变量在哪里强制、谁守住、怎么失败、怎么测 |
| 章节 | 1 定位与边界 · 2 核心概念与术语 · 3 设计原因 · 4 怎么用 · 5 配置 · 6 运行与运维 · 7 保证与不保证 · 8 相关文档 | 1 包与文件地图 · 2 关键类型 · 3 主流程 · 4 不变量清单 · 5 并发 · 6 失败与不确定结果 · 7 持久化 / 协议格式 · 8 测试与门禁 · 9 历史与重要修复 · 10 review 检查点（多数还有 11 源码疑点） |
| 证据 | 设计结论 + 出处（维护者决定、方案、`path:line`） | 每条不变量写清内容、强制位置（`path:line`）、守卫测试 |

同一编号的两篇互相链接：开头一行和每一节末尾都有跳到对方的链接。跨分区的内容只引用对方，不展开重复写。规格见 [framework-docs-spec](../review/handoff-v1.23.0/framework-docs-spec.md)。

---

## 分区目录

| NN | 分区 | 说明篇 | 实现篇 |
| --- | --- | --- | --- |
| 00 | 总览：三大块、包地图、端到端走读、全局索引 | [guide/00](guide/00-overview.md) | [impl/00](impl/00-overview.md) |
| 01 | app 与生命周期：Mod、单实例锁、fail-stop、三步停机、readyz | [guide/01](guide/01-app-lifecycle.md) | [impl/01](impl/01-app-lifecycle.md) |
| 02 | nest 调度与实体：快慢双池、Guard、事务、actionflow、glsvet | [guide/02](guide/02-nest-entity.md) | [impl/02](impl/02-nest-entity.md) |
| 03 | dataengine：WAL、投影、outbox、DAO 与 A1、驱动契约（A2）、cache | [guide/03](guide/03-dataengine.md) | [impl/03](impl/03-dataengine.md) |
| 04 | sync：entitysync、syncbus、syncstream、lockstep、帧、玩家接入 | [guide/04](guide/04-sync.md) | [impl/04](impl/04-sync.md) |
| 05 | remote 实体与 Mirror：L2 水位、兴趣、ownerroute、bus / NATS | [guide/05](guide/05-remote-mirror.md) | [impl/05](impl/05-remote-mirror.md) |
| 06 | saga：协调器、步骤收件箱、`stepTransition`、步骤预算 | [guide/06](guide/06-saga.md) | [impl/06](impl/06-saga.md) |
| 07 | 配置：服务配置声明、configdata、tablegen / cfggen、featureflag、hotcode | [guide/07](guide/07-config.md) | [impl/07](impl/07-config.md) |
| 08 | skill 与战斗：编译器、Runtime、Host、combat、attribute、spatial | [guide/08](guide/08-skill.md) | [impl/08](impl/08-skill.md) |
| 09 | kit 服务：account、platform、session、global / activity、mail、chat、match、rank、directory | [guide/09](guide/09-kit-services.md) | [impl/09](impl/09-kit-services.md) |
| 10 | 时间：业务 / 系统双时钟、高水位、timer | [guide/10](guide/10-time.md) | [impl/10](impl/10-time.md) |
| 11 | 可观测与运维：metrics、health、log、admin、security、servicemetrics | [guide/11](guide/11-observability.md) | [impl/11](impl/11-observability.md) |
| 12 | 代码生成与工程：roost CLI、生成器、模板、robot、脚本、根包门禁 | [guide/12](guide/12-codegen.md) | [impl/12](impl/12-codegen.md) |

---

## 术语表

合并自各分区说明篇 §2。一个词只给一个定义；同一个词在不同分区指不同的东西时，用括号限定（例如“准入（Nest）”“准入（WAL）”），并在 [同词异义](#同词异义) 里列出。定义里的位置以分区原文为准，“分区”列链到出处。

### 跨分区通用

| 术语 | 定义 | 分区 |
| --- | --- | --- |
| 三大块 | nest 调度、dataengine、sync：core 的三块基础，其余是建在其上的服务或通用基建 | [00](guide/00-overview.md#2-三大块原则) |
| A1（DAO 统一回滚） | 事务里会改、回滚时要恢复的状态一律放在 DAO，由 Nest 的 DAO 回滚统一兜住；组件不维护需要回滚的内存状态、不自己登记 undo | [02](guide/02-nest-entity.md#36-回滚统一走-dao维护者决定-a1)、[03](guide/03-dataengine.md#35-a1回滚统一走-dao) |
| A2（结果未知交给调用方） | 驱动不重放写；只在能证明命令没执行时重发；结果未知原样交给调用方 | [03](guide/03-dataengine.md#36-a2驱动不重放写结果未知交给调用方) |
| A3 ②（退订本身排空） | 总线 `Subscription.Unsubscribe(ctx)` 返回 nil 即没有在途回调、也不会再有新回调 | [04](guide/04-sync.md#23-服务间总线与流) |
| A4 ①（配置声明） | 每个 Mod 和业务服务用“配置结构体 + tag”声明自己读的键，启动检查、读取、生成、doctor 共用这一份 | [07](guide/07-config.md#2-核心概念与术语) |
| 结果未知（indeterminate） | 操作可能已生效也可能没有。框架不猜：Nest 事务 → `ErrCommitIndeterminate` 并 fence；Mongo → `ErrCommitResultUnknown`；Remote → `ErrRemotePersistenceIndeterminate`；versionstore → `ErrOutcomeUnknown` | [03](guide/03-dataengine.md#34-结果未知fence不猜)、[05](guide/05-remote-mirror.md#33-结果未知交给持久结论不猜) |
| fail-stop / `RuntimeFailure` | 进程级“不能再安全写”信号。第一次 `Fail` 同步执行全部 `OnFail` 回调（含 Nest fence），之后走同一条停机路径并非零退出 | [01](guide/01-app-lifecycle.md#2-核心概念与术语) |
| fence（Nest 引擎） | 结果未知或 `RuntimeFailure` 时，Nest 引擎立刻拒绝新消息和排队消息；幂等，只记第一个原因 | [02](guide/02-nest-entity.md#2-核心概念与术语) |
| 三步停机 | ① 发起关闭（幂等）→ ② 在调用方 ctx 内等真实排空（超时返回 ctx 错误、保留对象、可重试）→ ③ 排空后才释放资源 | [01](guide/01-app-lifecycle.md#47-写停机对象三步停机--共用类型) |
| 停机不完整 | 某一步因 ctx 取消 / 超时没停完；App 不再停它之后（更底层）的 Mod、不释放单实例锁 | [01](guide/01-app-lifecycle.md#2-核心概念与术语) |
| 静态注册 | 不需要 Registry、不做 I/O 的注册（实体 kind / builder、组件工厂、配置表、Nest handler、路由），由生成的 `registry` 聚合在进程启动早期执行 | [01](guide/01-app-lifecycle.md#410-静态注册)、[12](guide/12-codegen.md#2-核心概念与术语) |
| 提交点 | 事务**不再回滚、不再重排**的时刻：`committer.Commit` 成功（async 是 WAL 写入、strict 是 fsync）、pipelined 的记录被 `Enqueue` 接纳、memory 快路径准入。注意 pipelined 的**回复**还要等票据（fsync）完成——03 说明篇把“成功对外可见”也叫提交点，见 [同词异义](#同词异义) | [02](guide/02-nest-entity.md#2-核心概念与术语)、[03](guide/03-dataengine.md#32-四种持久模式与提交点) |
| L1 / L2 / 权威 | L1 = 进程内缓存；L2 = Redis 共享缓存；数据权威在 Mongo。Remote 快照里 L2 是“快照水位”的唯一权威（维护者 B2），但仍不是数据权威 | [03](guide/03-dataengine.md#310-cache缓存不是权威)、[05](guide/05-remote-mirror.md#34-l2-是快照水位的唯一权威维护者第三轮-b2) |
| 就绪位 / readiness | ops Mod 的布尔：`service.started` 置真、`service.stopping` 置假；`/readyz` = 就绪位 ∧ 没有 checker 为 Fail | [01](guide/01-app-lifecycle.md#63-readiness)、[11](guide/11-observability.md#2-核心概念与术语) |
| Degraded | checker 的“还能服务、需要关注”；不影响 `Snapshot.OK`，`/readyz` 仍返回 200 并列出 | [11](guide/11-observability.md#2-核心概念与术语) |

### app 与生命周期（01）

| 术语 | 定义 |
| --- | --- |
| App | 顶层容器。一个二进制一个 App，含多个服务类型（CLI 子命令），每次启动只跑一个 |
| Mod | 基础设施模块。生命周期 `Init(cfg) → Provide(registry) → Start() → Stop()`，停机逆序 |
| 共享 Mod / 服务专属 Mod | `App.Mods(...)` 注册的对所有服务类型生效；`RegisterServer(type, svc, mods...)` 的只对该服务类型生效 |
| Service | 业务主体：`Init(registry) → Serve(ctx) → Shutdown(ctx)`，一个进程只跑一个 |
| Registry / capability | 能力表 `ModName → any`；键叫能力名，常与 Mod 名相同，但一个 Mod 可登记多个能力 |
| DependsOn / OptionalDependsOn | Mod 的硬依赖 / 存在时才排序的可选依赖，**写 Mod 名**，不是能力名 |
| StopWithContext / StopBudget | Mod 的有界停止入口 / 声明自己需要的固定停机时长 |
| IManager / `manager.Engine` / ManagerMod | 服务内存单例 / 驱动它们的生命周期引擎 / 把引擎包成 Mod |
| lifecycle hook | 在 `app.init`、`mods.started`、`service.started`、`service.stopping`、`service.stopped`、`config.reload` 阶段被调用的回调 |
| 单实例锁（singleton） | Redis 键 `<key_prefix>:<server_type>:<sid>`：任何 Mod Init 之前获取、持有期间续期、全部 Mod 停完才释放 |
| Live（活性） | 只读查询“这些 sid 里哪些有进程持有锁”；停机中的进程仍算活（维护者 C5） |
| SingletonIncarnation | 本进程持有的锁的身份（键、sid、本次启动 token），模块据此接管上一代进程的残留 |

出处：[01 §2](guide/01-app-lifecycle.md#2-核心概念与术语)。

### nest 调度与实体（02）

| 术语 | 定义 |
| --- | --- |
| `NestMgr`（引擎） | 一个实例化的 Nest；`Start` / `Shutdown` 一次性，不能重启 |
| `Client` | 生成的 Sender 依赖的发送接口：`Dispatch` / `Request` / `...Multi` / `...MultiGroup` / `DispatchBroadcast` |
| handler | `BaseHandler`：参数是已加锁的实体与业务参数；由 `//roost:nest` 生成包装并注册 |
| 声明目标 | 消息上的 `Tid` / `Tids` / `GroupTIds`，即 handler 的实体参数；同 ID 顺序与冷热判断只看它们 |
| 准入（Nest） | `TrySendMsg` 把消息放进派发队列的那一步；成功后消息归派发器所有 |
| 快池 / 慢池 | 两条执行通道：快池执行全部 handler、本地锁、回滚，不得等待；慢池只做冷加载、Remote 准备等等待 |
| 快续行 | 慢阶段把需要本地锁的步骤交回快池同步执行的通道，不排在任何同 ID 链上 |
| 冷目标改道 | 快池首跑发现声明目标未加载，原位交给慢池准备，不丢失同 ID 位置 |
| Guard / Guard 作用域 | `EntityGuard` 是一个 goroutine 持有的锁账本；`GuardScope` 是它唯一的所有者，作用域结束放全部锁 |
| 锁序 | 按实体 kind 所属 category 的值从小到大、同 category 内按 ID 升序取锁；Remote category 固定最先 |
| 锁组 / 组迁移 | 一组实体共用一把组锁串行化；加入、离开、换组走内部消息 |
| Cast | handler 内按锁序动态取得未声明实体的锁并纳入当前事务 |
| `RollbackTx` | 一次事务：回滚函数、提交参与者、持久化变更、effect、回执 |
| 回滚策略 / 持久策略 | `none` / `state` / `undo`；`memory` / `async` / `strict` / `pipelined` |
| `RunLocal` | 后台 / 慢 worker 把需要实体锁的步骤交给快池执行并等它结束 |
| 独立事务 | `RunIsolatedTransaction` / `RunDetachedTransaction`：基础设施用、不认领当前消息的事务 |
| fctx | 每 goroutine 一个请求上下文（Meta、Trace、Base ctx、KV），按 GoID 存 |
| 快 worker 标记 / `LoadedEntitiesOnly` | 执行位置标记（只由派发队列在快 lane 建立）/ Getter 契约：只读内存，冷目标返回 `entity.ErrColdLoadInLogic` |
| actionflow / 延后队列 | 实体内的动作 / 任务状态机（`ActionRunner`、`MissionRunner`）/ runner 回调期间对 runner 的修改延后到最外层返回前执行 |
| finding / hint | glsvet 的违例（计数、退出码 1）/ 提示（`hint:`，不计数） |

出处：[02 §2](guide/02-nest-entity.md#2-核心概念与术语)。

### dataengine（03）

| 术语 | 定义 |
| --- | --- |
| 一次写入的五个阶段 | ① 内存修改（DAO setter）→ ② WAL 准入 → ③ WAL durable → ④ Mongo projected → ⑤ effect published / consumed；每个阶段由不同的一方确认 |
| 准入（WAL） | 事务结束、仍持实体锁时，`committer.Commit` 返回或 `Enqueue` 返回票据；准入失败是明确拒绝 |
| DAO | 生成的数据对象；事务会改的状态都在这里（A1） |
| `Tracker` | DAO 已接受的持久版本 + 同步脏掩码；持久脏数据不在这里，在事务里 |
| `PersistChange` | 一个 DAO 在本事务内累积的持久变化（掩码、Set / Unset 路径、整字段、删除） |
| `Mutation` | 一个文档的一次版本化变化：`Put`（整文档）、`Patch`（字段级）、`Delete` |
| `CommitRecord` | 一次事务的全部 mutation、effect、receipt，作为一条 WAL 记录原子准入 |
| `Effect` | 事务性 outbox 消息；`ID` 是全局去重键（缺省 `<事务ID>:<序号>`） |
| 回执（DataEngine `Receipt`） | 与事务同提交的持久回执；`(namespace, id)` 唯一，digest 不同即身份冲突 |
| lease fence 回执 | 特殊 receipt：投影时要求某个协调文档仍归同一 owner / token，否则整条记录跳过 |
| LSN / `DurableLSN` | pipelined 记录的单调序号；`DurableLSN` 以下的记录全部 durable |
| ack checkpoint | 已投影完的连续前缀在 WAL 里的位置；双槽文件，只前进 |
| held（投影） | 事务仍持实体锁，投影器不得越过它 |
| fatal 投影冲突 | Mongo 版本 / 事务身份 / 回执身份与 WAL 不一致；进程 fence |
| `ErrCommitIndeterminate` / `ErrCommitRejected` | 提交结果不确定（fence、不回滚）/ 写任何持久记录之前被明确拒绝（已回滚） |
| `ErrCommitResultUnknown` | Mongo 事务的提交命令已发出但没拿到结论 |
| `IsDefinitelyNotExecuted` | Redis 错误能否证明命令没在服务端执行 |
| `ErrOutcomeUnknown` | versionstore 写回复丢失、按写令牌也核对不出结果 |

出处：[03 §2](guide/03-dataengine.md#2-核心概念与术语)。

### sync（04）

| 术语 | 定义 |
| --- | --- |
| Manager（entitysync） | 通常一个进程一个，拥有全部 subject 与会话；每 tick 把脏 subject 变成每个会话自己的帧 |
| subject | 一个实体的同步状态 `*entity.SubjectSyncState` 加上它的订阅者表（谁收它的唯一真相） |
| 会话（entitysync session） | 一个接收者：帧时钟（epoch / tick）与已持有对象的引用表；id 由政策定义 |
| 订阅（subscription） / 订阅状态 | 一个会话对一个 subject 的状态 / `kindSnapshot`（等全量）、`kindLive`（持有 baseVersion、发增量）、`kindLeaving`（欠一个 remove） |
| 订阅来源 | 一个独立的订阅所有者；同来源幂等、不同来源各自释放，最后一个来源离开才退订 |
| profile（视图）/ LOD | `SyncProfile{Key, LOD, SchemaVersion}`，一个有限视图；不含订阅者身份 |
| held / ready 会话 | held：已开、可订阅、不出帧；`ReadySession` 后第一帧是新 epoch 的全量帧 |
| 帧准入（entitysync） | `Transport.Push` 返回 nil：这一帧已交给传输，不能撤回 |
| `ErrRetryLater` | 传输整体不可用：整个 tick 作废、全部保持脏、谁也不罚 |
| 持久化水位门控 | capture 的 `CommitLSN` 高于 `DurableWatermark` 时整个 subject 本 tick 不发 |
| periodic / on_change | 两种同步模式：按 `Interval` 合并检查 / 锁内冻结、解锁且提交确认后唤醒 |
| 快照预算 | 限制客户端尚未持有对象的创建（冷创建）；增量、remove、已持有对象的全量不占额度 |
| 退役 | `Unregister` 之后：每个持有对象的订阅者欠一个 remove，全部发出后 subject 被遗忘（remove-before-create） |
| 卸载重载（RR-59） | 实体被仅内存卸载后仍有订阅者时，框架从权威重载并 `Rebind` |
| 政策阶段 | 每次 Flush 捕获之前、持有 `flushGate` 时：先交付释放，再交付重新提交，再跑 `RegisterPolicy` 处理器 |
| namespace | `syncNamespace=` 生成标记，随组件下发，客户端唯一的分流键 |
| Interest（政策） | AOI + 关系来源聚合成每个 (observer, subject) 一份订阅 |
| AOI / AOICluster | 格子索引的增量兴趣层 / 多区域 AOI 拼成的无缝空间（仓内只有测试使用） |
| RelationSource / 排队事实 | 集合驱动的关系来源（team、friends、self）/ handler 在锁内入队、提交确认后在政策阶段应用的兴趣事实（`QueueMove` / `QueueRelation`） |
| Refusal | Manager 拒绝的订阅；Interest 每次 Apply 都再提，直到被接受或 pair 释放 |
| Group / Direct | 成员 × 实体全互见（不叫 Room）/ 逐对 `Bind` / `Unbind` |
| `SyncMsg` | 总线线格式：`MessageID`、Topic、Key、Version、Data（nil = 删除）、FromSid 等 |
| `ISyncBus` / `ILiveSubscriber` | 发布 + 订阅 / 可确认订阅：`SubscribeLive` 返回 nil 后每条消息至少一次交给 handler |
| `*Subscription` / 投递 ctx | 一次订阅的句柄（排空在这里实现一次）/ `Deliver` 交给 handler 的 ctx，带“正在执行这个订阅的回调”标记 |
| syncstream History / FileHistoryJournal | 每个 (Observer, Stream) 一条序号链，保留到客户端 ACK / 它的 checkpoint + WAL 持久化 |
| lockstep Room | 输入帧同步房间：收输入、按 tick 切帧、冗余广播、追帧、关键帧哈希裁决 |
| 迟到折入 / 追帧 | 帧号小于当前帧的输入改写到当前帧 / 重连或缺口时从历史分页经可靠通道补发 |
| `access.player` / `access.player.tcp` | 生成的两个 Mod：协议注册表 + WriteGate / TCP 监听、握手、会话表、推送 |
| dispatch 超时（玩家 TCP） | 每个请求的等待上限；只限制等待，不限制不配合 ctx 的 handler |

出处：[04 §2](guide/04-sync.md#2-核心概念与术语)。

### remote 实体与 Mirror（05）

| 术语 | 定义 |
| --- | --- |
| owner 进程（Remote） | 装了 `RemoteEntityMod` + DataEngine、能写 Remote 实体的进程 |
| 只读服务 / 同进程读者 | 装 `RemoteMirrorMod` 的进程 / 不另装 Mod、经 `MirrorSource(registry)` 拿 owner 的 `SnapshotClient` |
| Mirror | 只读方只拿到“按完整 key 读快照”这一项能力的那套机制（Mirror 1～6 步） |
| Managed Remote 实体 | `//roost:entity ... remote=managed` 生成的实体 |
| `RemoteCommit` | 一次事务里一个实体的冻结提交：Base / Next 版本、三个 epoch、整文档 mutation 等 |
| 四维版本向量 | `StateVersion`（内容）、`MarkerEpoch`（所有权代）、`LockFence`（写许可代）、`RouteEpoch`（路由代） |
| owner-routed / shared | 两种写模式：独占 owner 直接写 / 先取 Redis 版本锁再取 Mongo 许可 |
| 写许可（WriteGrant） | Mongo `_remote_entity_meta` 上的一次 FindAndModify：递增 `_grant_fence`、写一次性 token |
| 所有权状态 | `local_owned / sharing / shared / draining / fenced / recovering / quarantined` |
| 隔离（quarantine） | 持久结论没拿到或被拒绝时冻住活实体，直到重新加载 |
| finalizer | 后台收尾 worker：只按持久结论释放资源、执行提交后工作 |
| 确认时刻 `confirmedAt` / `cached_max_staleness` | L2 或权威最近一次担保“没有更新版本或删除”的时刻 / 非线性读能交出的条目距确认时刻的上限 |
| 墓碑（L2） | L2 上带版本删除留下的 `deleted_version`，挡住不新于它的快照写 |
| 观察 token | `RemoteObservation{MarkerEpoch, RouteEpoch, StateVersion}`，可作下一次读的下限 |
| 兴趣（Remote） | consumer 对某个 key 的软状态租约；owner 只推送有兴趣的 key |
| 兴趣代际 / 溢出水位 | 同一 consumer 对同一 key 的 renew / release 单调序号 / 兴趣表满时每个 consumer 一个的“代际上限 + key 指纹位图” |
| 首载缓冲 | 某 key 权威加载在途时到达的复制消息先缓冲、加载后按序重放 |
| 推送 | 快照复制消息经可确认订阅（JetStream DeliverNew）送到 consumer |
| Mirror DTO | `//roost:mirror` 标记的普通 struct，生成 spec / 解码 / reader |
| 可靠消息（bus） / 持久 RPC | 带 MsgID 的模块消息，消费端 SETNX 去重、失败进死信，投递仍是 core NATS / `CallReliable`：请求与响应都经 JetStream 流 |

出处：[05 §2](guide/05-remote-mirror.md#2-核心概念与术语)。

### saga（06）

| 术语 | 定义 |
| --- | --- |
| saga 状态 | `pending` / `waiting` / `compensating` / `completed` / `compensated`（可能被重开）/ `failed`（可能被重开）/ `manual_required` |
| saga 定义（`saga.Definition`） | `Type` + `Version` + 有序步骤；记录固化定义版本 |
| 步骤（`Step`） | 名字、正向主题、补偿主题、超时 / 重试预算 |
| 操作 | 一个 saga 的一个步骤的一个方向，键 `<sagaID>:<phase>:<step>` |
| 尝试 | 操作的一次派发；`CommandID` 带尝试号（与代际） |
| 代际（saga incarnation，B1） | `Resume` 与补偿方向的人工 `Compensate` 各开新一生，`Incarnation+1` |
| 结果（`Completion`） | 成功 / 拒绝 / 可重试失败；成功的 `Data` 成为下一步命令的 `Payload` |
| 结果回执（saga） | 协调器侧 `_saga_completions`，`_id = CommandID`，与状态推进同事务 |
| 操作 tombstone（saga） | 操作关闭时写 `_saga_operations`，记关闭方式 `result` / `abandoned` |
| 步骤收件箱 / 操作状态文档 | 步骤侧判断“执行、回放还是等待”（原生与 Mongo 共用一份判定）/ 每个操作一份的当前尝试、租约、结论 |
| 步骤租约 | 一次尝试的执行权，`lease_until = min(now + LeaseDuration, 命令截止)` |
| 生效点 | 原生步骤：投影事务对状态文档的条件写；Mongo 步骤：handler 事务里的 `settleOwnAttempt` |
| 接替 | 当前尝试租约过期、结论未知时，新尝试成为当前尝试（token+1） |
| 迟到步骤（`LateStep`）/ 重开 | 放弃之后才送达成功的正向步骤 / `failed` / `compensated` 因迟到成功回到 `compensating` |

出处：[06 §2](guide/06-saga.md#2-核心概念与术语)。

### 配置（07）

| 术语 | 定义 |
| --- | --- |
| 配置声明（ConfigSchema） / 键 | 一组键的声明，由配置结构体经 `app.SchemaOf` 得到 / 完整点分键名 + 类型 + default / example / min / max / enum / secret |
| 配置结构体 / `ModConfigSchema` | 字段类型即键类型、tag 写键名与规则的 Go 结构体 / Mod 与业务服务实现的 `ConfigSchema()` 接口 |
| `LoadConfig` | 读服务配置的唯一入口：按声明一次读完、检查、调 `ValidateConfig`，错误一次报全 |
| 启动检查 | App 在任何 Mod Init 之前检查并合并 App、本服务全部 Mod、服务本身的声明 |
| `ValidateConfig(production)` | 声明表达不了的跨键规则，声明检查通过后调用 |
| 生产环境 | `env` / `app.env` / `environment` 任一为 `prod` / `production`（不分大小写） |
| starter 键 / 声明快照 / 业务声明 | 声明了 `example`、生成器写成生效行的键 / kit 与 app 全部声明的 Go 字面量（codegen 用）/ 快照里没有、doctor 编译工程读回的键 |
| 配置数据快照（`configdata.Snapshot`） | 业务数据的一代：全部表、对象、custom，带 `Version` / `LoadedAt` / `Hash`，发布后不再改 |
| TableDef / ObjectDef / CustomDef / 外部表 / auto 表 | 表 / 单例对象 / 派生运行时结构 / 外部工具（Luban）生成的聚合体 / 由 `cfg` 标签推出主键与规则的表 |
| 列规则 / 键拼写规则 | required / unique / min / enum / ref，加载层检查 / 数据键必须与字段 json 名逐字一致 |
| ReloadListener / ReloadOutcome | 热更的四段回调 / 每次 Load / Reload / Rollback 恰好一份的结果 |
| 钉住（ActiveSnapshot） | 请求上下文带着准入时的那一代快照，同一请求内的读不跨代 |
| tablegen / cfggen | 两条业务数据生成管线（维护者选 A：保持两条） |
| 补丁点 | `hotcode.Register(name, fn)` 登记的、可在运行期替换的函数 |

出处：[07 §2](guide/07-config.md#2-核心概念与术语)。

### skill 与战斗（08）

| 术语 | 定义 |
| --- | --- |
| 技能定义（skill `Definition`） | 严格解析后的 wire 定义，编译完即可丢弃 |
| `CompileEnvironment` / `Program` | 权威目录与上限 / 编译产物：名字降成 handle、槽位、下标，带三个 digest |
| `Runtime` / `Host` | 确定性调度器（cast、冷却、衍生物、被动、checkpoint）/ 世界：实体、属性、空间、效果落地；Runtime 的一切读写只经 Host |
| 召唤物（Summon） | 用单位模板在场景里生成的真实单位，Host 管它的存活与指令 |
| 衍生物（Spawn） | 飞行物、法术场、光束、位移、环绕、随从；Runtime 逐 tick 推进，Host 只执行每一步 |
| 衍生物分区 | 施放中 / 已移交 / 待停止 / 已停止 / 已放弃；只有 `setState` 改分区 |
| 移交 | 施法逻辑结束时，施法期间召出的衍生物从“施放中”移交给 owner |
| cast / 窗口阶段 / cast 状态 | 一次施放 / `preparing → committed → executing → recovering` 等 / `running` / `suspended` / `finished` / `failed` |
| policy（skill） | `tap` / `toggle` / `hold` / `charge` / `ammo` |
| GCD / 互斥 | 从 commit tick 起算的全局冷却 / 同一施法者有 cast 在 preparing / committed / recovering 时新施放返回 `ErrCasterBusy` |

出处：[08 §2](guide/08-skill.md#2-核心概念与术语)。

### kit 服务（09）

| 术语 | 定义 |
| --- | --- |
| owner 进程（服务） | 持有某服务存储、注册其总线 handler 的进程（装 `<svc>.NewMod` + `NewServer`） |
| 调用方进程 | 只装 `<svc>.NewClientMod()`、拿到 `BusClient` 的进程 |
| 公开能力名 / `.local` | `service.mail` 等发布包装后的接口 / `.local` 是实现本体 |
| 传输半 / 装配半 | 生成物的两半：wire 类型、handler 表、`BusClient` / owner 与 client 的 Mod 装配 |
| 业务错误 / 总线错误 | 响应信封里的 `code/reason` / “调用没发生或不知道发生没有” |
| 请求 ID / 按状态幂等 | 写操作调用方给的幂等键 / 不存请求 ID，按存储里的值是否已是目标状态判断重放 |
| run（session 服务） | 一次有截止时间的运行（副本、对局），挂外部资源，结束或过期时逐个释放 |
| slot（account） | 一个账号在一个区服的一个角色名额，同时是持久的建角计划 |
| 结果投递（activity dispatch） | 活动结果发往某个 game 的一次投递记录，带 ACK 令牌 |
| 绑定 sid / FromSID | 角色只由建角时那个 sid 的 game 进程服务 / 赠礼信封里发送方的 sid |

出处：[09 §2](guide/09-kit-services.md#2-核心概念与术语)。

### 时间（10）

| 术语 | 定义 |
| --- | --- |
| 业务时钟 / 系统时钟 | 真实时间 + `time.logic_offset`（玩法时间读它）/ 真实时间，即 `time` 包（租约、超时、日志读它） |
| `time.logic_offset` | 业务时钟偏移，缺省 0，生产必须为 0，只在启动时读一次 |
| Registry 业务时钟 / 进程级业务时钟 | 能力 `clock.business`，经 `app.BusinessClock(r)` 取 / `clock.Now()` 等，偏移由 App 启动时设一次 |
| 业务时间高水位 / 容差 | 这套部署到过的最大业务时间（Redis 键，不过期）/ 启动检查放过的回退量（1 分钟，不可配） |
| typed 定时器 / 闭包定时器 | 有类型号、会持久化 / 纯内存、不持久化 |
| priority / 未注册类型 | 期限相同时小的先触发 / 到期时类型没有 handler：节点照样删除，记 Warn 与计数 |

出处：[10 §2](guide/10-time.md#2-核心概念与术语)。

### 可观测（11）

| 术语 | 定义 |
| --- | --- |
| 指标注册表 / 默认注册表 | `metrics.Registry`，按名字 + 排序后的标签建序列 / 包级函数写入的那个，App 每次设为自己新建的 |
| 序列 / 序列上限 / `obs.series.dropped` | 名字 + 一组标签值 / 每个名字最多 2048 条（可配）/ 被上限丢弃的写入次数 |
| `DeleteSeries` | 按名字 + 标签子集删序列并归还名额；空 match 不删 |
| Duration 指标 / Histogram | 只记次数、总和、最大、最近 / 17 个固定指数桶，插值估分位数 |
| checker | `health.Checker`，返回 ok / degraded / fail |
| admin 命令 | `admin.CommandDef`，经 `/admin/execute` 执行，只认 token |
| Reporter（servicemetrics） | 服务事件契约：Accepted / Refused / Replayed / Dropped / Conflict / Depth |
| failurelog | Redis 列表式失败记录（RPUSH + LTRIM + PEXPIRE 一条脚本） |
| stats_log 记录 | 一次采集（运行时、实体计数、Nest 池）写成一行 JSON |

出处：[11 §2](guide/11-observability.md#2-核心概念与术语)。

### 代码生成与工程（12）

| 术语 | 定义 |
| --- | --- |
| 清单 `roost.yaml` | 工程的唯一手写描述：项目名、module、版本策略、CI/CD、共享 Mod、服务 |
| codegen 所有 / 业务所有 | 每次 `sync` 都重新渲染、可覆盖的文件 / 只在不存在时创建一次的文件 |
| 生成产物 | 生成器写出的文件，靠内容里的 `Code generated` 字样识别 |
| 暂存树 | 工程旁边的 `.roost-*` 目录：先在里面做完全部工作，最后一次提交 |
| 标记（marker） | 源码注释 `//roost:<kind> key=value …` |
| 托管框架服务 / `uses` | `services.<名字>.framework: <kit 服务>` / 业务服务调用托管框架服务 |
| `minimumVersions` / 兼容矩阵 lane | 生成物能编译的最低框架版本 / `framework-compat.yml` 的 minimum / released / source-head |
| 运行期守卫 | `codegen/scripts/*-runtime.sh`：把生成输出编译进已发布的最低 core 真跑一次 |

出处：[12 §2](guide/12-codegen.md#2-核心概念与术语)。

### 同词异义

下列词在不同分区指不同的东西。读到时按分区理解；写新文档时用上面带括号的限定名。

| 词 | 含义 |
| --- | --- |
| 准入 | Nest：消息进派发队列（02）；WAL：记录被 committer 接受（03）；帧：`Transport.Push` 返回 nil（04）；Remote 写准入：`PrepareRemoteWriteBatch`（05）；saga `Admit`：多进程下谁能执行步骤（06） |
| 提交点 | 02：不再回滚 / 重排的时刻（pipelined = `Enqueue` 接纳）；03 说明 §3.2：成功对外可见的时刻（pipelined = 票据完成） |
| fence | Nest 引擎 fence（02）；lease fence 回执（03）；`LockFence` / `_grant_fence` 写许可代（05）；saga 步骤“被 fence”指实体屏障 / lease fence 拒绝（06） |
| held | 投影器不得越过仍持锁的事务（03）；已开但不出帧的会话（04） |
| Manager | `entitysync.Manager`（04）；`remoteentity` 写管理器（05）；`manager.Engine` / `IManager` 服务内存单例（01） |
| 会话 / session | entitysync 会话（04）；玩家 TCP 会话（04 生成层）；session 服务的 run（09）；`security` 的会话 token（11） |
| owner 进程 | Remote 实体的写者进程（05）；kit 服务存储的持有进程（09） |
| 兴趣 / Interest | `policy.Interest` 订阅政策（04）；Remote consumer 的兴趣租约（05） |
| 代际 | Remote 兴趣 generation（05）；saga incarnation（06）；`RemoteCommit` 的三个 epoch（05）；configdata 快照的“一代”（07） |
| 快照 / Snapshot | `configdata.Snapshot`（07）；Remote 快照（05）；entitysync 全量帧 / `kindSnapshot`（04）；DAO 回滚快照（02 / 03） |
| 墓碑 / tombstone | `Mutation` 的 `Delete`（03）；L2 带版本删除（05）；saga `_saga_operations`（06）；lockstep 已裁决帧（04） |
| 回执 | DataEngine `Receipt`（03）；effect 收件箱回执（03）；saga 结果回执 `_saga_completions`（06） |
| Definition | `saga.Definition`（06）；skill 的 wire 定义（08） |
| dispatch | Nest `Dispatch`（02）；玩家 TCP dispatch 超时（04）；activity 结果投递（09） |
| 租约 | 单实例锁（01）；outbox 认领（03）；saga 步骤租约（06）；Remote 兴趣（05）；mail 领取（09） |
| Mirror | 只读快照能力（05）；`sync/syncbus/mirror` 副本信封与 `Replicator`（04）；`cache/mirror.go` 的 `ReplicaSyncer`（04） |
| outbox | DataEngine Mongo outbox（03）；skillsync file outbox（03 / 08）；saga outbox（06） |
| timer | `timer` 定时器包（10）；指标类型 Duration（11 说明篇写作“Duration（timer）”） |
| Room | `lockstep.Room` 战斗房间（04）；entitysync 帧头的 `RoomID`（恒为 1，04）；旧名 `policy.Room` 已改为 `Group` |

---

## 包 → 分区索引

覆盖 tag 上 `go list ./...` 的全部顶层包与 kit 子包。“主”是讲这个包的分区；“另见”是从自己角度讲到它的分区。标“未展开”的包没有分区专门写，按依赖关系归入，细节读源码。

### core

| 包 | 主 | 另见 | 职责（一句话） |
| --- | --- | --- | --- |
| `actionflow` | [02](guide/02-nest-entity.md) | | 实体内动作 / 任务状态机 |
| `admin` | [11](guide/11-observability.md) | | admin 命令注册表与执行 |
| `ai` | [02](guide/02-nest-entity.md) | [10](guide/10-time.md) | 实体 AI 策略边界，经 actionflow 驱动（02 只提一句，未展开） |
| `app`、`app/buildinfo` | [01](guide/01-app-lifecycle.md) | [07](guide/07-config.md)（配置部分）、[10](guide/10-time.md)（业务时钟与高水位）、[11](guide/11-observability.md)（注册表） | App、Mod、Registry、单实例锁、停机 |
| `attribute` | [08](guide/08-skill.md) | [12](guide/12-codegen.md) | 实体属性容器（代码生成用） |
| `bus` | [05](guide/05-remote-mirror.md) | [09](guide/09-kit-services.md) | 服务间消息与 RPC、可靠消费、死信 |
| `cache` | [03](guide/03-dataengine.md) | [04](guide/04-sync.md)（`ReplicaSyncer`）、[05](guide/05-remote-mirror.md) | L1 / L2 缓存原语 |
| `clock` | [10](guide/10-time.md) | | 业务时钟 |
| `cmd/glsvet` | [02](guide/02-nest-entity.md) | [03](guide/03-dataengine.md)（A1 提示）、[10](guide/10-time.md)（时钟提示） | 静态检查 |
| `configdata`、`configdata/rules` | [07](guide/07-config.md) | | 业务数据快照、规则 |
| `container` | [02](guide/02-nest-entity.md) | | 桶、对象池、拓扑排序（被 entity / lock 用；未展开） |
| `dataengine`、`dataengine/engine` | [03](guide/03-dataengine.md) | | 持久化契约与生产实现 |
| `demo` | [12](guide/12-codegen.md) | | game-demo 模板原件 |
| `entity` | [02](guide/02-nest-entity.md) | [05](guide/05-remote-mirror.md)（Remote 部分）、[04](guide/04-sync.md)（`SubjectSyncState`） | 实体、Guard、实体管理 |
| `errcode` | [12](guide/12-codegen.md) | [09](guide/09-kit-services.md) | 错误码（生成器 + 运行时） |
| `etcd`、`etcd/driver` | [09](guide/09-kit-services.md) | [11](guide/11-observability.md)（健康） | etcd 契约与驱动：发现、选举（servicerpc 用；未展开） |
| `event` | [02](guide/02-nest-entity.md) | [12](guide/12-codegen.md)（eventgen） | 实体事件总线（未展开） |
| `failurelog` | [11](guide/11-observability.md) | [05](guide/05-remote-mirror.md)（死信） | Redis 失败列表 |
| `fctx` | [02](guide/02-nest-entity.md) | [07](guide/07-config.md)（快照钉住） | 每 goroutine 请求上下文 |
| `featureflag` | [07](guide/07-config.md) | | 进程内开关表 |
| `gateway` | [04](guide/04-sync.md) | | 网关中间件链、限流、认证兜底 |
| `goroutine` | [02](guide/02-nest-entity.md) | | GoID 等 goroutine 工具 |
| `health` | [11](guide/11-observability.md) | [01](guide/01-app-lifecycle.md) | checker 注册表、并发快照 |
| `hotcode`、`hotcode/plugintest` | [07](guide/07-config.md) | | 函数级热补丁点 |
| `httpclient` | [11](guide/11-observability.md) | | 带签名的 HTTP 客户端（仓内无生产调用方；未展开） |
| `httpserver` | [11](guide/11-observability.md) | [12](guide/12-codegen.md)（webroute） | HTTP 服务器（ops 端点用；未展开） |
| `index` | — | | 泛型内存索引（仓内无调用方；未展开） |
| `internal/configschema` | [07](guide/07-config.md) | [01](guide/01-app-lifecycle.md) | 服务配置声明 |
| `internal/operation`、`internal/stopcontract` | [01](guide/01-app-lifecycle.md) | | 停机共用类型、三步停机契约骨架 |
| `internal/rangecontract` | [02](guide/02-nest-entity.md) | | 遍历回调契约的测试辅助（维护者 C7；未展开） |
| `lifecycle` | [01](guide/01-app-lifecycle.md) | | 生命周期 hook |
| `lock` | [02](guide/02-nest-entity.md) | | 实体锁、可重入锁（未展开） |
| `log` | [11](guide/11-observability.md) | | 结构化日志 |
| `manager` | [01](guide/01-app-lifecycle.md) | | 服务单例生命周期引擎 |
| `metrics` | [11](guide/11-observability.md) | | 指标注册表 |
| `migration` | [03](guide/03-dataengine.md) | [12](guide/12-codegen.md) | DAO schema 迁移原语（被 DAO 生成器用；未展开） |
| `misc` | — | | 哈希、整数小工具（基建；未展开） |
| `mongo`、`mongo/driver`、`mongo/mongotest` | [03](guide/03-dataengine.md) | | Mongo 契约、驱动、测试替身 |
| `nats`、`nats/driver` | [05](guide/05-remote-mirror.md) | [04](guide/04-sync.md)（JetStream 结算） | NATS 契约与驱动 |
| `nest` | [02](guide/02-nest-entity.md) | [03](guide/03-dataengine.md)（提交部分） | 调度与事务 |
| `nestwal` | [03](guide/03-dataengine.md) | | 物理 WAL、effect 收件箱 |
| `ownerroute` | [05](guide/05-remote-mirror.md) | | 按 owner sid 分派命令 |
| `redis`、`redis/driver` | [03](guide/03-dataengine.md) | | Redis 契约与驱动（A2） |
| `remoteentity` | [05](guide/05-remote-mirror.md) | | Remote 写管理器、快照客户端 |
| `safemap` | [03](guide/03-dataengine.md) | | 生成 DAO 集合与注册表用的并发 map（未展开） |
| `saga` | [06](guide/06-saga.md) | | 协调器、收件箱、存储 |
| `security` | [11](guide/11-observability.md) | | 会话 token、签名、限流 |
| `service/mail`、`service/match`、`service/session` | [09](guide/09-kit-services.md) | | 服务领域实现（kit 包是别名 + Mod） |
| `servicemetrics` | [11](guide/11-observability.md) | [09](guide/09-kit-services.md) | 服务事件上报 |
| `servicerpc` | [09](guide/09-kit-services.md) | [12](guide/12-codegen.md)（生成器） | 生成的服务 RPC 运行时 |
| `skill`、`skill/combat`、`skill/combatcomponent`、`skill/skillcompose`、`skill/skillsync` | [08](guide/08-skill.md) | [03](guide/03-dataengine.md)（file outbox） | 技能编译器、Runtime、战斗、同步 |
| `spatial` | [08](guide/08-skill.md) | [04](guide/04-sync.md)（AOI 用） | 空间索引、寻路基础件 |
| `sync/entitysync`、`sync/entitysync/policy`、`sync/frame`、`sync/lockstep`、`sync/nettransport` | [04](guide/04-sync.md) | | 实体复制、政策、帧、帧同步、传输 |
| `sync/syncbus`、`sync/syncbus/driver` | [04](guide/04-sync.md) | [05](guide/05-remote-mirror.md) | 服务间总线契约与驱动 |
| `sync/syncbus/mirror` | [04](guide/04-sync.md) | [05](guide/05-remote-mirror.md) | 副本信封与 `Replicator` |
| `syncstream` | [04](guide/04-sync.md) | [08](guide/08-skill.md) | 有序持久流 |
| `timer` | [10](guide/10-time.md) | | 实体定时器调度器 |
| `versionstore` | [03](guide/03-dataengine.md) | [09](guide/09-kit-services.md) | 带版本的 KV、写令牌 |
| `webroute` | [12](guide/12-codegen.md) | | 生成 HTTP 路由的运行时 |
| `worker` | [02](guide/02-nest-entity.md) | | worker 池（glsvet 允许的异步方式） |

### kit

| 包 | 主 | 另见 | 职责 |
| --- | --- | --- | --- |
| `kit`（根，只有测试） | [01](guide/01-app-lifecycle.md) | | 全部基础设施 Mod 的生命周期门禁 |
| `kit/configdata` | [07](guide/07-config.md) | | `config_data` Mod |
| `kit/dataengine` | [03](guide/03-dataengine.md) | | DataEngine Mod |
| `kit/etcd` | [09](guide/09-kit-services.md) | [11](guide/11-observability.md) | etcd Mod（未展开） |
| `kit/internal/configschemagen` | [07](guide/07-config.md) | | 声明快照生成器 |
| `kit/lock` | [02](guide/02-nest-entity.md) | | 锁管理 Mod（未展开） |
| `kit/manager` | [01](guide/01-app-lifecycle.md) | | `ManagerMod` |
| `kit/mods` | [01](guide/01-app-lifecycle.md) | [09](guide/09-kit-services.md) | Mod / 能力名常量、`RegisterAll` |
| `kit/mongo` | [03](guide/03-dataengine.md) | [11](guide/11-observability.md) | Mongo Mod（未展开） |
| `kit/nats` | [05](guide/05-remote-mirror.md) | | NATS Mod、bus 装配、DLQ 命令 |
| `kit/nest` | [02](guide/02-nest-entity.md) | [04](guide/04-sync.md)（`NewModWithEntitySync`） | Nest Mod |
| `kit/ops` | [11](guide/11-observability.md) | [01](guide/01-app-lifecycle.md)（readiness） | ops 端点 |
| `kit/redis` | [03](guide/03-dataengine.md) | [01](guide/01-app-lifecycle.md)（单实例锁 opener） | Redis Mod |
| `kit/remoteentity` | [05](guide/05-remote-mirror.md) | | `RemoteEntityMod` / `RemoteMirrorMod` |
| `kit/saga` | [06](guide/06-saga.md) | | saga Mod |
| `kit/service/{account,chat,directory,global,global/activity,mail,match,platform,rank,session}` | [09](guide/09-kit-services.md) | | 十个 kit 服务 |
| `kit/service/servicemetrics` | [11](guide/11-observability.md) | [09](guide/09-kit-services.md) | servicemetrics 别名 |
| `kit/service/examples/split`、`kit/service/integration` | [09](guide/09-kit-services.md) | | 拆分部署示例、真实 Redis 集成测试 |
| `kit/statslog` | [11](guide/11-observability.md) | | stats_log Mod |
| `kit/syncbus` | [04](guide/04-sync.md) | | `SyncBusMod` |

### 工具与工程

| 包 / 目录 | 主 | 说明 |
| --- | --- | --- |
| `codegen/cmd/*`、`codegen/internal/*` | [12](guide/12-codegen.md) | `tablegen` / `cfggen` 主讲在 [07](guide/07-config.md)；`dao` 另见 [03](guide/03-dataengine.md)；`entity`（mirror）另见 [05](guide/05-remote-mirror.md) |
| `robot/*` | [12](guide/12-codegen.md) | 虚拟客户端与压测 |
| `scripts/`、`scripts/perf/*` | [12](guide/12-codegen.md) | 发布、矩阵、压测脚本 |
| 根包 `*_test.go` | [12](guide/12-codegen.md) | 仓库级门禁；层守卫见 [impl/00 §1.2](impl/00-overview.md#12-层与守卫) |

---

## 已知问题入口

- 统一登记与处置状态：[FRAMEWORK-DOCS-FINDINGS-2026-10-07](../review/FRAMEWORK-DOCS-FINDINGS-2026-10-07.md)（编号 F01-*、F02-*……，每条要在本轮闭环：修复、结构守卫或写明证明）。
- 各分区的源码疑点与文档不一致（原始分析）：见 [impl/00 §6.2](impl/00-overview.md#62-各分区检查点与已知疑点) 的汇总表。
- 文档按 v1.23.0 写；之后的修复（例如 [RR-20261006-41](../bug/RR-20261006-41.md)）在分区正文里以“v1.23.1 起”注明。

---

## 维护这套文档

- 新增或改动后跑两道检查，都必须零失败：

  ```sh
  GOWORK=off go test -count=1 -run 'Markdown|Conflict' .   # 根包：相对链接目标存在、无冲突标记
  python3 scripts/check-doc-anchors.py                      # 相对链接 + #锚点（GitHub slug 规则）
  ```

- 跨分区引用写真链接；目标章节改名时，跑锚点脚本找出全部引用方。
- 写作规则沿用 [framework-common](../review/handoff-v1.23.0/framework-common.md)：先结论后细节，事实可追溯到 `path:line` 或文档链接，推断写明“推断 / 未验证”。
