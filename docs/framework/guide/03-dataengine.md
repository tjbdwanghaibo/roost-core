# 03 DataEngine 说明

> 本篇是框架整体文档 03 分区的**说明文档**，面向业务作者与运维。实现细节、不变量强制点和 review 检查点见 [实现文档](../impl/03-dataengine.md)。
>
> 源码基准：tag `v1.23.0`（`28912cd6`）。文中 `path:line` 都按这个 tag。本篇没有依赖 codebase-memory 图谱（图谱可能落后于 tag），全部结论按 tag 源码直接读取。

## 速览

- DataEngine 是 roost 唯一的保存链路：handler 在 Nest 实体锁内改生成 DAO，事务结束时冻结成一条 `CommitRecord`，先进文件 WAL，再由投影器按 WAL 顺序写进 Mongo；外部消息（effect）随同一条记录进 Mongo outbox，再由 outbox worker 发到 JetStream。
- 最重要的保证：**成功回复不会先于 handler 声明的提交点**（memory / async / strict / pipelined 各有一个），WAL 里的记录**至少投影一次**、按 Mongo 里的版本与事务标记**幂等**；结果不确定时框架 **fence 进程、不猜、不回滚**，由新进程从 WAL 恢复。
- 最容易踩的坑：把“WAL durable”“Mongo 已投影”“broker 已接受”当成一回事；在组件字段里放事务会改的状态（A1 要求一律放 DAO）；把 Redis / Mongo 的传输错误当成“没写”再重试一遍（A2：结果未知交给调用方）；`durability=memory` 的 handler 改了持久字段（见 §7.2）。

本篇覆盖的包：

| 包路径 | 职责 |
| --- | --- |
| `dataengine/` | 持久化数据契约：`Mutation` / `CommitRecord` / `Effect` / `Receipt`、`Tracker`、校验、加载器、lease fence 回执、Sync 视图 |
| `dataengine/engine/` | 生产实现：`Assembly` → `Runtime`，WAL 准入与投影（`Projector`）、`MongoStore`、聚合仓库（冷加载）、迁移、删除准入、outbox |
| `nestwal/` | 物理日志：分段 WAL、组提交、fsync、ack 双槽 checkpoint、尾部恢复、pipelined 票据；effect 发布 / 收件箱；独立的通用 `Committer` |
| `kit/dataengine/` | Mod 胶水：读 `dataengine.*` / `nest.pipelined.*` 配置、查依赖能力、转发生命周期、fatal 接 fence |
| `nest/`（提交相关部分） | 持久模式、`RollbackTx`、提交点、`ErrCommitIndeterminate`；调度部分属 02 分区 |
| `codegen/internal/dao/` | 生成 DAO：setter 登记 undo 与持久变化、`PrepareMutation` / `AcceptMutation`、回滚快照 |
| `mongo/`、`mongo/driver/` | Mongo 接口与驱动：事务循环、`ErrCommitResultUnknown`、选举重试、Close 口径 |
| `redis/`、`redis/driver/` | Redis 接口与驱动：A2 重放契约、`IsDefinitelyNotExecuted`、CAS 脚本、DistLock、Close 口径 |
| `versionstore/` | 带版本的 KV 契约与 Redis / 内存实现：CAS 重试、一次性写令牌、索引、`ErrOutcomeUnknown` |
| `cache/` | L1 / L2 缓存原语：`LocalStore`、`AtomicLocalStore`、`LayeredStore`、`ReadThroughStore`、Redis 存储、`RefHMap` |
| `skill/skillsync/`（file outbox 部分） | 技能表现包的文件 outbox：每包一个可原子替换的文件；技能本身属 08 分区 |

---

## 1. 定位与边界

**一句话**：DataEngine 把 Nest 事务里的 DAO 变化变成可恢复、可幂等重放的持久记录，并把它们可靠地送进 Mongo 与消息总线。

| 负责 | 不负责 |
| --- | --- |
| 事务结束时把 DAO 变化物化为 Put / Patch / Delete（`PrepareMutation`） | 取实体锁、排队调度、回复调用方（02 nest） |
| WAL 准入、组提交、fsync、崩溃后的尾部恢复与按序重放 | 客户端同步与 durable 水位门槛的发送侧（04 sync） |
| 按版本 CAS 投影到 Mongo，同一事务内暂存 receipt 与 effect | Remote 实体所有权、L2 快照水位、Mirror（05 remote）；只在投影时调用 Remote 适配器 |
| outbox 认领 / 发布 / 退避重试，effect 收件箱去重 | saga 状态机（06 saga）；只提供 lease fence 回执与原生步骤屏障 |
| 冷加载完整聚合、schema 迁移写回、删除墓碑 | 业务字段语义、跨 database 原子性（用 saga） |
| 驱动层“写不重放、结果未知交给调用方”的契约（Redis / Mongo） | kit 各服务怎么用 versionstore（09 kit 服务） |
| versionstore、cache 的一致性原语 | 指标面板与告警规则本身（11 可观测） |

跨分区引用：调度与事务边界见 02 nest 分区（<!-- pending: ./02-nest.md -->`guide/02-nest.md`，尚未写出）；同步水位门槛见 04 sync 分区（<!-- pending: ./04-sync.md -->`guide/04-sync.md`）；Remote 与 Mirror 见 05 分区（<!-- pending: ./05-remote-mirror.md -->`guide/05-remote-mirror.md`）；saga 原生步骤见 06 分区（<!-- pending: ./06-saga.md -->`guide/06-saga.md`）。

[↑ 速览](#速览) · [实现文档 §1](../impl/03-dataengine.md#1-包与文件地图)

---

## 2. 核心概念与术语

### 2.1 一次写入经过的五个阶段

这五个阶段的确认**不能互相替代**（`dataengine/engine/doc.go:7-9`）。

| 阶段 | 发生在哪里 | 谁确认 | 说明 |
| --- | --- | --- | --- |
| ① 内存修改 | handler 内，DAO setter | 无 | 失败时由 DAO undo / 快照回滚 |
| ② WAL 准入 | 事务结束、仍持实体锁 | `committer.Commit` 返回 / `Enqueue` 返回票据 | 准入失败是**明确拒绝**，内存回滚 |
| ③ WAL durable | writer 组提交 fsync 之后 | strict 的 `Append` 返回；pipelined 的票据完成；`DurableLSN` 前进 | 进程崩溃、断电后可从 WAL 恢复 |
| ④ Mongo projected | 解锁之后，投影器后台 | 投影成功并推进 ack checkpoint；系统票据（`CommitSystem`）完成 | 冷加载能读到；跨进程交出所有权前要等到这一步 |
| ⑤ effect published / consumed | outbox worker 发布；消费方收件箱 | JetStream 接受（MsgID 去重）；消费方回执与业务写同一 Mongo 事务 | 与 ①～④ 完全解耦，总线断了不挡提交 |

### 2.2 术语表

| 术语 | 含义 | 出处 |
| --- | --- | --- |
| DAO | 生成的数据对象，事务会改的状态都在这里（A1） | `codegen/internal/dao/template_dao.go` |
| `Tracker` | DAO 已接受的持久版本 + 同步脏掩码。**持久脏数据不在这里**，在事务里 | `dataengine/tracker.go:7-12` |
| `PersistChange` | 一个 DAO 在本事务内累积的持久变化（掩码、Set / Unset 路径、整字段、删除） | `nest/persist_change.go:20-26` |
| `Mutation` | 一个文档的一次版本化变化：`Put`（整文档）、`Patch`（字段级）、`Delete`（墓碑）；`NextVersion = ExpectedVersion + 1` | `dataengine/mutation_types.go:69-88`、`dataengine/validate.go:73-75` |
| `CommitRecord` | 一次事务的全部 mutation、effect、receipt，作为一条 WAL 记录原子准入 | `dataengine/mutation_types.go:108-117` |
| `Effect` | 事务性 outbox 消息；`ID` 是全局去重键（缺省 `<事务ID>:<序号>`） | `dataengine/mutation_types.go:90-97`、`nest/rollback.go:335-360` |
| `Receipt` | 与事务同提交的持久回执（saga 等用）；`(namespace, id)` 唯一，digest 不同即身份冲突 | `dataengine/mutation_types.go:99-105` |
| lease fence 回执 | 特殊 receipt：投影时要求某个协调文档仍归同一 owner / token，否则整条记录跳过 | `dataengine/lease_fence.go:14-62` |
| 提交点（commit point） | 成功可以对外可见的最早时刻；按持久模式不同 | §3.2 |
| LSN / `DurableLSN` | pipelined 记录的单调序号；`DurableLSN` 以下的记录全部 durable（前缀性质） | `nestwal/wal.go:438-442` |
| ack checkpoint | “已投影完的连续前缀”的 WAL 位置，双槽文件，只前进 | `nestwal/checkpoint.go` |
| held | 事务仍持实体锁时，投影器不得越过它 | `dataengine/engine/projector.go:114-116` |
| fatal 投影冲突 | Mongo 版本 / 事务身份 / 回执身份与 WAL 不一致，进程 fence | `dataengine/engine/mongo_store.go:27-29` |
| `ErrCommitIndeterminate` | 提交结果不确定（写 / fsync 失败、准入后接受失败）；fence，不回滚 | `nest/nest.go:86-93` |
| `ErrCommitRejected` | 写任何持久记录之前被明确拒绝；可回滚事务已回滚 | `nest/nest.go:71-75` |
| `ErrCommitResultUnknown` | Mongo 事务的提交命令已发出但没拿到结论，可能已提交 | `mongo/errors.go:11-16` |
| `IsDefinitelyNotExecuted` | Redis 错误能否证明命令没在服务端执行 | `redis/driver/replay.go:23-54` |
| `ErrOutcomeUnknown` | versionstore 写回复丢失、按令牌也核对不出结果 | `versionstore/write_token.go:36-41` |
| L1 / L2 | 进程内缓存 / 共享 Redis 缓存；权威另有其处 | `cache/` |

[↑ 速览](#速览) · [实现文档 §2](../impl/03-dataengine.md#2-关键类型与数据结构)

---

## 3. 设计原因

### 3.1 一条保存链路：DAO → WAL → 投影

- **锁内不做数据库 I/O**。Nest 在锁内只做到 WAL 准入（内存拷贝、组提交 fsync），Mongo 写在解锁之后。热点实体的锁持有时间不含 Mongo 往返。
- **WAL 先于 Mongo**。WAL 是单写者、按 append 顺序的日志，同一实体的 LSN 顺序就是修改顺序；崩溃后只截掉后缀，恢复出的历史总是某个前缀（`NEST_PIPELINED_COMMIT.md` §2 的论证）。
- **只有一条投影路径**。主动 `Flush`、启动恢复、后台循环都走 `Projector.ReplayPass`（`dataengine/engine/projector.go:342-374`），没有绕开版本检查的第二套写法；旧的“实体快照 Checkpoint Mod”已删除，由守卫 `TestLegacyCheckpointWritePathIsAbsent`（`dataengine/architecture_test.go:13`）挡住回潮。
- 包结构保持两个核心包：`dataengine` 是契约，`engine` 是实现；合并会形成 `nest ↔ engine` 的 import 环（[DataEngine 重构方案 §2](../../feature/REFACTOR-2026-09-24-dataengine.md)）。

### 3.2 四种持久模式与提交点

| 模式 | 锁内做什么 | 提交点（成功何时可见） | 崩溃会丢什么 | 适用 |
| --- | --- | --- | --- | --- |
| `memory` | 不写 WAL | handler 返回、内存提交 | 全部（只承诺内存） | 只读查询、可重建的临时状态 |
| `async` | WAL `Append`，**等写入不等 fsync** | 记录写进段文件（页缓存）后 | 断电 / 内核崩溃时，最后一个组提交间隔（缺省 10ms）的记录；进程崩溃本身不丢页缓存（推断，依赖 OS 语义） | 普通写，吞吐优先 |
| `strict` | WAL `Append`，**等所在批 fsync** | 组提交 fsync 成功之后（仍在锁内） | 无（fsync 已成功） | 支付、唯一奖励、跨实体资产 |
| `pipelined` | `Enqueue`（锁内唯一拒绝点）后**提前放锁** | 票据完成（fsync 覆盖其 LSN）之后才回复、执行 AfterCommit、外发同步 | 无（回复晚于 fsync） | 热点实体、durable handler 密集 |

依据：`nestwal/wal.go:316-337`（async 只等写入、strict 与 pipelined 等 fsync）、`nest/execution.go:337-394`（pipelined 放锁后等票据）。

注意：

- **strict 不等 Mongo**。四种模式的提交点都不包含 ④ 投影。要等 Mongo 用 `Flush` 或 `WaitEntityProjection`（§4.5）。
- 带 Remote 批次的消息与 broadcast 不走 pipelined 的提前放锁，回退到 `Append`，并且从 RR-20260928-11 起与 strict 一样等 fsync（`nest/execution.go:184-189`、`nestwal/wal.go:333-337`）。
- `nest.Emit` 把 memory 事务自动提升为 strict（`nest/rollback.go:352-357`）；删除实体同样提升为 strict（`nest/rollback.go:247-249`）。名义上有 outbox 却可能丢，是不允许的。
- `//roost:nest` 标记只认 `memory|async|strict`（`codegen/internal/nest/parse.go:171-183`）；pipelined 只能用 `nest.RegisterHandlerWithMeta` 手工注册，且要在 `nest.pipelined.allowlist` 里（`nest/execution.go:20-24`）。

### 3.3 至少一次重放 + 持久身份幂等

WAL 重放是 at-least-once：Mongo 已写而 ack 没落盘、ack 文件写失败、进程在两者之间崩溃，都会重放同一条记录。框架不追求“恰好一次投影”，而是让重放可识别：

- 单文档快路径：按 `_version` 精确 CAS；不匹配时读回 `_version` 与 `_last_tx`，二者都等于本次才算“已应用”（`dataengine/engine/mongo_store.go:258-269`）。
- 多文档 / 带 effect / receipt：同一个 Mongo 事务里先查事务标记 `_dataengine_transactions`（digest 必须相同），再写业务文档、receipt、outbox，最后插标记（`dataengine/engine/mongo_projection.go:80-142`）。
- effect：outbox 文档 `_id` 就是 effect ID；JetStream 用 effect ID 作 MsgID（只是优化）；**正确性来自消费方收件箱**（`nestwal/effect_inbox.go:33-36`）。

所以 `Projected` 计数会大于业务提交数，`committed == projected` 不能证明一致（`dataengine/engine/projector.go:79-82`）。

### 3.4 结果未知：fence，不猜

磁盘写或 fsync 报错时，WAL 无法证明那批页是否已落盘（Linux 同一 fd 的写回错误只报告一次，RR-20260926-33）。此时：

- WAL 进入 terminal：不再接受写、不再 fsync、`Ack` 失败、`DurableLSN` 不再前进（`nestwal/wal.go:1206-1235`）。
- Nest 不回滚内存（可能已提交，回滚会制造第二条历史），`abandon` 事务，fence 引擎（`nest/execution.go:45-47`、`nest/rollback.go:510-532`）。
- Mod 的 `onFatal` 让进程 fail-stop：Nest fence + `RuntimeFailure`（`kit/dataengine/mod.go:460-479`）。新进程从 WAL 恢复出权威历史。

这也是 roost-coding 执行契约“超时 / 未知结果不等于未提交或回滚”的落地。

### 3.5 A1：回滚统一走 DAO

维护者第二轮决定（[DECISIONS-PENDING](../../review/DECISIONS-PENDING-2026-10-05.md) A1 行，**不采用推荐**）：

> 回滚都使用 DAO 的实现方式，这样回滚都可以统一

背景：同一缺口出过四次（NC-61、NC-65、NC-140、N09 O1），都是组件自己调 `RecordUndo` 补，漏一处就是资产错。现在：

- 事务内会改、回滚要恢复的状态一律在 DAO；不落库的用 `dao:"nopersist,sync"`（只同步）或 `dao:"nopersist,nosync"`（只参与事务）。
- 派生值也是 DAO 字段，由组件里唯一的 derive 在加载与改源字段的同一事务里写；**回滚不是重算触发点**。
- 组件不调 `RecordUndo` / `RecordUndoToken` / `DeferRollback`；手写 DAO 自己的方法登记逆操作是允许的（`skill/combatcomponent.CombatDao`）。
- 例外 B4：`skill.Runtime` 的冷却、ammo 等不进事务（08 分区）。

方案与证据：[A1 方案](../../feature/REFACTOR-2026-10-05-dao-unified-rollback.md)、[A1 字段写提示](../../feature/A1-COMPONENT-FIELD-WRITE-HINT-2026-10-06.md)。

### 3.6 A2：驱动不重放写，结果未知交给调用方

维护者第二轮 A2 行（原话）：

> 按推荐：① 驱动行为契约表 ② RedisMod 默认不重放写命令；③ 暂不做

起因：go-redis 的 `MaxRetries` 会在 EOF / 读超时后把同一条命令换连接重发；脚本或写已执行、只是回复丢了时，第二次执行把自己刚写的值当成别人的（NC-100：versionstore 一次 Update 写了两次）。Mongo 便捷 API 用 background ctx 提交，网络黑洞时阻塞到恢复（NC-101）。现在：

- Redis 写命令、脚本、含写的 pipeline、DistLock 都带 `NoRetry`，只在 `IsDefinitelyNotExecuted` 为真时由驱动重发（`redis/driver/replay.go:107-123`）。
- Mongo `WithTransaction` 自己实现重试循环，整体受 `transaction_timeout` 约束；提交发出后的失败包 `ErrCommitResultUnknown`（`mongo/driver/session.go:56-115`）。
- 契约表写进仓库：[redis/driver/README.md](../../../redis/driver/README.md)、[mongo/driver/README.md](../../../mongo/driver/README.md)。

### 3.7 A2 ③：versionstore 一次性写令牌；墓碑选 A

第十三轮维护者要求本版完成 ③（原话：“还有留到下个版本的几项在本机能完成吗？希望本次能完成了”）。每次 CAS 写在信封里带一个 store 生成的令牌，回复丢失时读一次键按令牌判定（§4.8）。“键不存在 / 删除之后”无法核对的两种情况，维护者选 A：

> 维护者选 A：不加墓碑；键不存在时 Create / Delete 回复丢失返回 `ErrOutcomeUnknown`，由调用方按请求 ID 去重或回读裁决

（[DECISIONS-PENDING](../../review/DECISIONS-PENDING-2026-10-05.md) 第十三轮的转述。）没选 B（删除写墓碑）的理由：墓碑保留期是推不出来的产品选择，期间空间不释放，期满后照样无法核对（[A2 ③ 方案 §8](../../feature/A2-3-VERSIONSTORE-WRITE-TOKEN-2026-10-07.md)）。

### 3.8 Close 统一口径（RR-20261006-10）

第十二轮维护者决定把 Close 写进 A2 契约表（原话“写进 A2 驱动契约表”），实测发现三种口径并存后统一为：**重复 Close 返回 nil，第一次的错误只报一次；并发 Close 的后到者等第一个做完；Close 之后的其他调用返回可 `errors.Is` 的已关闭错误**。不采用“粘滞返回第一次错误”：它让停机重试永远不收敛（[RR-20261006-10 修复记录](../../bugfix/RR-20261006-10.md)）。

### 3.9 O-M6-5：启动建索引遇选举有界重试

第十二轮“Mirror 剩余观察”行（原话）：

> 保持；O-M6-5 owner 启动遇 Mongo 选举做有界重试

只在 `EnsureIndexes` 一处做：它只在启动时调用，一处覆盖 DataEngine、Remote owner、saga、效果收件箱的全部启动 DDL；索引创建幂等（`mongo/driver/collection.go:247-320`）。

### 3.10 cache：缓存不是权威

- L1 只在自己的 TTL 窗口内算“已准入”，窗口外不能否决权威（RR-20261004-02，`cache/layered.go:56-70`）。
- 每条写 L1 的路径（发布、loader 回填、L2 回填）走同一条 Stale / Conflict / Superseded 准入规则，被拒的值不交付（RR-20260913-06、RR-20261004-04）。
- `StaleFunc` 在 Redis 存储上只是建议性的（读与写分两次往返）；正确性依赖淘汰输家时，用带版本的 CAS（`cache/store.go:37-49`）。

[↑ 速览](#速览) · [实现文档 §3](../impl/03-dataengine.md#3-主流程)

---

## 4. 怎么用

### 4.0 最小可运行示例

仓库里能直接跑通整条链路（生成 DAO / Entity → Nest 事务 → 文件 WAL → Mongo → 三次独立进程重启）的例子是 [`codegen/internal/entity/testdata/dataengine/`](../../../codegen/internal/entity/testdata/dataengine)：

- DAO 定义 [`def/state.go`](../../../codegen/internal/entity/testdata/dataengine/def/state.go)、[`def/trade.go`](../../../codegen/internal/entity/testdata/dataengine/def/trade.go)；
- handler 注册与三种持久模式（async / strict / pipelined）的用法见 `codegen/internal/entity/testdata/dataengine/create_in_handler_test.go:36-52`、`multi_entity_test.go`；
- 运行：先按 `kit/scripts/integration/README.md` 起隔离 Mongo，然后 `ROOST_DATAENGINE_IT_MONGO_URI=… bash scripts/test-dataengine-generated.sh`。

业务工程的完整形态看 game-demo 模板（[`demo/`](../../../demo/README.md)，`roost project new <name> -module <mod> -template game-demo` 生成）：下文 §4.1～§4.4 的示例都取自它。

### 4.1 声明 DAO

真实示例：game-demo 模板 [`demo/db/def/player.go.tmpl`](../../../demo/db/def/player.go.tmpl)；生成物的完整形状见 codegen golden [`gen_hero_dao.go`](../../../codegen/internal/dao/testdata/golden/gen_hero_dao.go)（定义 [`hero.go`](../../../codegen/internal/dao/testdata/def/hero.go)）。

```go
//roost:dao coll=player db=game schema=2
type PlayerDao struct {
    Gold      int64           `bson:"gold" dao:"persist,sync"`
    AttrFinal map[int32]int64 `bson:"attr_final" dao:"nopersist,sync,map=fast"`  // 派生值：只同步
    AttrGear  map[int32]int64 `bson:"attr_gear" dao:"nopersist,nosync,map=fast"` // 只参与事务
    DungeonClaims map[string]int64 `bson:"dungeon_claims" dao:"persist,map=fast"` // 只落库
}
```

`dao` tag 规则（`codegen/internal/dao/parse.go:495-547`）：

| 写法 | 落库（进 WAL / Mongo） | 同步 | 参与回滚 | 事务外调用 setter |
| --- | --- | --- | --- | --- |
| 不写 tag | 是 | 是 | 是 | panic |
| `persist` | 是 | 否 | 是 | panic |
| `persist,sync` | 是 | 是 | 是 | panic |
| `nopersist,sync` | 否 | 是 | 是 | 可以 |
| `nopersist,nosync` | 否 | 否 | 是 | 可以 |
| `-` | 不生成 | — | — | — |
| 只写 `map=fast` 等、没有意图 | 生成期报错 | | | |

“事务外 panic”来自生成的 `mark<Field>Dirty` 调 `nest.MarkPersist`，没有活动事务时返回错误并 panic（`codegen/internal/dao/template_dao.go:100-104`；golden `codegen/internal/dao/testdata/golden/gen_hero_dao.go:84-89`）。只在内存里存在、从不落库的实体用 `//roost:dao nocoll`（全部字段 `nopersist`，[方案](../../feature/DAO-NO-COLLECTION-2026-10-04.md)）。`map=small|fast|sharded` 选容器实现。`schema=N` 提升后需要给 `migration.MigrateDAO` 注册迁移步骤（§4.7）。

### 4.2 选 rollback 与 durability

```go
//roost:nest rollback=undo durability=strict
func handlerAddExp(target player.IProfileEntity, stats world.IStatsEntity, amount int64) (int32, error) {
    gained, err := target.ProfileComp().AddExp(amount)
    if err != nil {
        return 0, err
    }
    stats.StatsComp().RecordExp(amount)
    return gained, nil
}
```

出处 [`demo/game/handler/add_exp.go.tmpl`](../../../demo/game/handler/add_exp.go.tmpl)：两个实体的修改进同一条 WAL 记录，要么都 durable，要么都不。

| 选择 | 推荐 |
| --- | --- |
| `rollback=undo` | 改动字段少的高频请求：生成 setter 首次改字段时登记逆操作 |
| `rollback=state` | 修改范围复杂：事务前对 DAO 做无副作用快照（`CaptureRollbackState`） |
| 不写 rollback | memory 时为 `none`（快路径，失败不撤销），否则为 `state`（`codegen/internal/nest/gen.go:202-225`） |
| `durability=strict` | 交易、付费、唯一奖励、跨实体资产 |
| `durability=async`（缺省） | 普通写 |
| `durability=memory` | 只读或可重建；**不要改持久字段**（§7.2） |
| pipelined | 手工注册 + allowlist，见 `NEST_PIPELINED_COMMIT.md` |

durable 的 handler 必须带回滚策略（`nest/handler.go:45-47`）。不确定时先 strict，压测与故障演练后再放宽（`codegen/internal/roost/render_docs.go:223-231` 生成到工程文档的同一建议）。

### 4.3 handler 与组件的写法规则

1. **只经 DAO 改状态**。组件字段只放只读引用、配置句柄、函数（投影函数等）；确属缓存的字段标 `//roost:cache`。`cmd/glsvet` 对组件登记 undo、组件写非 DAO 字段打印 `hint:`（不计入失败，`cmd/glsvet/main.go:687-740`、`cmd/glsvet/componentfields.go:275-290`）。
2. **需要索引 / 堆时在一次调用内从 DAO 构建**，不常驻（game-demo World 定时器就是这样，[A1 方案 §4.2](../../feature/REFACTOR-2026-10-05-dao-unified-rollback.md)）。
3. **外部副作用用 `nest.Emit`**，不要放进 `AfterCommit`：AfterCommit 不在 WAL 里，崩溃会丢，重放也不会再执行。AfterCommit 只放幂等、可丢的通知。
4. 业务里**不要吞掉 `RunIsolatedTransaction` / `RunDetachedTransaction` 的错误**；回复判别表见 USER_GUIDE §4“回复错误判别”（属 02 分区，本篇不重复）。
5. 冷目标用正式 Slow option 声明，快阶段不做冷加载（`EntityRepository.LoadEntity` 在快 worker 上 panic，`dataengine/engine/entity_repository.go:132-134`）。

### 4.4 效果流（effect）：生产与消费

生产方，在 handler 内（[`demo/game/effects/level_up.go.tmpl`](../../../demo/game/effects/level_up.go.tmpl)）：

```go
return nest.Emit(nest.Effect{
    Topic:   TopicPlayerLevelUp,          // 主题；线上主题是 <subject_prefix>.<topic>
    Key:     strconv.FormatInt(playerID, 10),
    Payload: payload,                     // JSON，消费方不一定是 Go
})
```

消费方（[`demo/internal/service/game/level_up_mail.go.tmpl`](../../../demo/internal/service/game/level_up_mail.go.tmpl)）：

```go
database, prefix, stream, err := kitdataengine.EffectSettings(registry.Config()) // 读同一份声明
inbox, err := nestwal.NewMongoEffectInbox(mongoClient, database, levelUpMailInbox)
sub, err := nestwal.SubscribeJetStreamEffects(ctx, jetstream, inbox, nestwal.JetStreamEffectConsumerConfig{
    Stream: stream, Durable: levelUpMailDurable, FilterSubject: prefix + "." + effects.TopicPlayerLevelUp,
    AckWait: 30 * time.Second, MaxDeliver: 20,
}, func(ctx context.Context, envelope nestwal.EffectEnvelope) error {
    // ctx 是收件箱的 Mongo 事务：经它写的 Mongo 文档与回执一起提交。
    // 不是 Mongo 写的外部调用（如 RPC），要自带幂等键（示例用 envelope.EffectID 作 RequestID）。
    return doBusiness(ctx, envelope)
})
```

链路：`Emit` → 记录进 WAL → 投影时在同一 Mongo 事务里写 `_dataengine_outbox` → outbox worker 认领、发布（MsgID = effect ID）→ 消费方收件箱查回执、执行 handler、插回执（同一事务，`nestwal/effect_inbox.go:78-114`）。回滚的事务从不产生 effect，提交的事务至少发布一次，消费方 Mongo 侧效果恰好一次。

### 4.5 主动 Flush 与投影屏障

| 入口 | 等到什么 | 用途 |
| --- | --- | --- |
| `dataMod.Flush(ctx)` | WAL `Sync` 屏障，然后重放到空 | 停机、迁服、运维检查、版本升级；**不要每个请求都调**（破坏组提交与批量投影） |
| `dataMod.WaitEntityProjection(ctx, id)` | 本进程对该实体已准入的全部投影完成 | 跨进程交出实体所有权前（RR-20260926-31）；只能在慢路径调用 |
| `dataMod.DurableLSN()` | 读当前水位 | 给 Sync 的 `DurableWatermark` 接线（kit 自动接，04 分区） |

`WaitEntityProjection` 在快 worker 上调用会 fail-fast（`dataengine/engine/entity_projection.go:87`）；fatal、WAL terminal、Runtime 已停都会立即返回可判别的错误，不会一直等（`dataengine/engine/entity_projection.go:115-128`）。

### 4.6 删除实体

handler 里 `Destroy(..., true)` 进入 DataEngine 删除准入（`dataengine/engine/entity_delete.go:27-54`）：

- 事务内：登记删除意图（事务升 strict），生成的 `PrepareDelete` 写 `Delete` mutation；准入后才从内存摘除（`AfterAdmission`）。回滚时实体保持存活。
- 事务外的本地实体：自己开一个 strict 独立事务。
- 事务外的 Remote 实体：在快 worker 上直接拒绝（`fctx.ErrBlockingInFastWorker`，RR-20260926-27），慢路径走系统提交并等投影。
- Mongo 里留墓碑：`_deleted: true` + 更高 `_version`；更旧的 Put 不能复活它（`dataengine/engine/mongo_store.go:199-215`）。

### 4.7 schema 迁移

1. DAO 标记 `schema=N` 提升版本，`db/migrations` 注册迁移步骤（game-demo 见 `demo/db/migrations`）。
2. 冷加载读到旧 schema 时，`MigrationRunner` 先在内存迁移并校验 BSON、`_id`、目标 DAO 能解码，再作为系统事务（strict）写 WAL，**等 Mongo 投影完成**，然后重读整个聚合（`dataengine/engine/migration_runner.go:40-108`、`dataengine/engine/entity_repository.go:208-237`）。
3. 竞争写让迁移记录过时：投影把它当无操作，仓库重读后最多再迁移一轮；三轮不收敛返回 `ErrMigrationConflict`。
4. 多 DAO 的迁移逐 DAO 提交，不是全有或全无；完整聚合校验通过前不发布实体。Remote 信封不能经这个入口迁移（`ErrRemoteMigrationLeaseRequired`）。

### 4.8 versionstore：带版本的 KV

接口只有 `Get` / `Update` / `Create` / `Delete`，**没有无条件 Set**（`versionstore/versionstore.go:83-131`）。`Update` 的 mutate 必须是纯函数，可能被调用多次。

```go
store, err := versionstore.NewRedisStore(client, versionstore.RedisConfig[string, Order]{
    Prefix: "platform:order:", KeyOf: func(id string) string { return id },
    Codec:  versionstore.JSONCodec[Order]{},
    Index:  &versionstore.RedisIndex[Order]{Key: "platform:order:due", Entry: dueEntry}, // 可选：与值同一次写
})
result, applied, err := store.Update(ctx, key, mutate)
if errors.Is(err, versionstore.ErrOutcomeUnknown) {
    // 下一次调用先按令牌核对上一次写，已生效就直接返回它的结果，不再叠加一次
    result, applied, err = store.Update(versionstore.Resume(ctx, err), key, mutate)
}
```

回复丢失时 store 自己读一次键判定（`versionstore/write_token.go:229-266`）：

| 读到的当前值 | 判定 | 动作 |
| --- | --- | --- |
| 令牌列表里有这次写的令牌 | 已生效 | 返回那次写的结果 |
| 与这次写的基准字节相同 | 没执行 | 原样重发同一条命令（同一令牌） |
| 由基准演进而来、没有这次写的令牌 | 别人写了下一个版本 | `Update` 当比输、重读重跑；`Delete` 返回 `ErrVersionMismatch` |
| 键不存在、链断了、令牌已被挤出（> 8 次后续写） | 证明不了 | `ErrOutcomeUnknown` |

**`ErrOutcomeUnknown` 的两种必然情况**（墓碑选 A 之后的契约）：① `Create`（或对不存在键的 `Update`）回复丢失、核对时键仍不存在；② `Delete` / `DeleteIf` 回复丢失、核对时键不存在。两者都由调用方按请求 ID 去重或回读裁决。`Resume` 只在进程内有效；`MemoryStore` 不会产生结果未知。

### 4.9 直接调 Redis / Mongo 时的核对清单

摘自两份驱动契约表 §6：

1. Redis 写命令返回错误 = 结果未知。要重试先让写可安全重复（请求 ID、版本 CAS、值守卫令牌）或先回读；只想在“确定没执行”时重试用 `driver.IsDefinitelyNotExecuted`，不要按错误文本判断。
2. 写命令的返回值（SETNX 的 bool、计数、INCR 新值、LPOP 元素）只在 `err == nil` 时可信。
3. 多步写（先写再 EXPIRE）第一步结果未知时仍补后续保护（`cache.RedisJSONHashStore.Set`、`RedisRawSortedSetStore.SetScore` 就是这样）。
4. 不要用 `Client.Raw()` 发写命令（不受契约约束）。
5. Mongo `WithTransaction` 返回 `ErrCommitResultUnknown` 时按持久回执裁决，不重做；事务外的多文档写出错同样结果未知。
6. 需要在调用方 ctx 之外收尾（abort、释放）的，用 `context.WithoutCancel` 加上限，不要用 `context.Background()` 无限等待。

### 4.10 cache 选型

| 场景 | 用什么 | 注意 |
| --- | --- | --- |
| 进程内、不可变值、高读 | `AtomicLocalStore`（分片 RLock 读、插入时钟淘汰、按条 TTL） | Stale / Conflict 在分片锁内判定，是原子的（`cache/atomic_local.go:147-190`） |
| 简单进程内 LRU | `LocalStore` | 测试与 `NewLocal<Dao>RedisDAO` 用 |
| Redis DAO（`//roost:redisdao`） | 生成的 `NewRedis…` / `NewCached…`（`RedisRawJSONStore` / `RedisRefHMapStore` + `LayeredStore`） | `NewCached…WithTTL` 的 ttl ≤ 0 表示**不从 L1 读**而不是永久缓存（`cache/layered.go:173-198`） |
| L1 + 可选 L2 + 权威加载器 | `ReadThroughStore` | 按键合并 miss、等待者上限；仓内生产代码已不再用它（Remote 快照缓存自 B2 起自己做准入，05 分区） |
| 多进程 L1 复制 | `ReplicaSyncer`（经同步总线） | `Stop(ctx)` 是三步停机 |

### 4.11 file outbox（skillsync）

技能表现包（syncstream 包）在客户端 ACK 之前需要跨重启保留时，`skillsync.Outbox` 配一个 `FileOutboxStore`（`skill/skillsync/file_outbox.go:43-49`）：

- 每个包一个 `<sha256>.packet` 文件，内容是带 sha256 校验的 JSON 信封；写入走“临时文件 → fsync → rename → fsync 目录”，崩溃后要么是旧文件、要么是新文件（`skill/skillsync/file_outbox.go:188-253`）。
- 目录归一个 store 独占；打开时删掉崩溃遗留的 `outbox-<数字>.tmp`（RR-20261006-04）。
- 有记录数与单条字节上限（缺省 100000 / 16MiB），超了返回 `ErrOutboxStoreLimit`。
- 它与 DataEngine 的 Mongo outbox 无关：不在 WAL 事务里，语义是“包保留到客户端 ACK”。技能侧用法见 08 分区（<!-- pending: ./08-skill.md -->`guide/08-skill.md`）。

[↑ 速览](#速览) · [实现文档 §3](../impl/03-dataengine.md#3-主流程)

---

## 5. 配置

所有键由 kit Mod 的配置结构体声明（A4 ①，`kit/dataengine/mod.go:96-157`），缺省值以声明为准；`min:"0"` 的键写 0 表示取 core 缺省。

### 5.1 `dataengine.*`

| 键 | 缺省 | 范围 / 校验 | 含义 |
| --- | --- | --- | --- |
| `persistence.engine` / `dataengine.enabled` | `dataengine` / `true` | 只能是这两个值，否则拒绝启动 | DataEngine 是唯一持久化引擎（`kit/mods/persistence.go:16-27`） |
| `dataengine.database` | `game` | | 业务库与 `_dataengine_*` 集合所在的默认库 |
| `dataengine.startup_timeout` | 30s | > 0 | 建索引 + 建流 + 打开 WAL + 恢复投影的总预算 |
| `dataengine.shutdown_timeout` | 30s | > 0 | 停机预算，声明给 App（`StopBudget`）；改大时同步改 `shutdown.total_timeout` |
| `dataengine.transaction_receipt_ttl` | 720h | > 0 | `_dataengine_transactions` 标记的 TTL；必须远大于 WAL 最长未确认时间 |
| `dataengine.receipt_ttl` | 720h | > 0 | 业务 receipt 未自带过期时的 TTL |
| `dataengine.wal.dir` | `data/wal/dataengine/<sid>` | | WAL 目录；一个目录同时只能一个进程写（OS 文件锁） |
| `dataengine.wal.writer_version` | 2 | 1～2 | 写格式；v1 写不出 Patch / Delete / receipt，生产用 2 |
| `dataengine.wal.segment_bytes` | 256MiB | ≥ 0 | 段大小 |
| `dataengine.wal.max_disk_bytes` | 8GiB | ≥ 0 | 磁盘上限：超过时 async / strict 准入拒绝、pipelined 同步拒绝；健康检查失败 |
| `dataengine.wal.max_unacked_age` | 24h | ≥ 0 | 最老未确认记录的年龄上限，超过健康检查失败 |
| `dataengine.wal.queue_capacity` | 8192 | ≥ 0 | 准入队列；满了 pipelined 同步拒绝，async / strict 在锁内等 |
| `dataengine.wal.group_commit_interval` | 10ms | ≥ 0 | async 的后台 fsync 间隔 |
| `dataengine.projection.remote_workers` | 8 | 1～64 | 互不相交的纯 Remote 记录并行投影数；1 关闭 |
| `dataengine.projection.batch_records` / `batch_bytes` / `read_bytes` | 256 / 4MiB / 4MiB | ≥ 0 | 一轮重放的记录数、批量投影字节、读取保留字节 |
| `dataengine.projection.retry_min` / `retry_max` | 10ms / 5s | ≥ 0 | 投影失败退避 |
| `dataengine.projection.checkpoint_records` / `checkpoint_interval` | 256 / 20ms | ≥ 0 | 可重放安全的批次，攒多少 / 多久 ack 一次 |
| `dataengine.projection.max_unacked_records` | 0（不限） | ≥ 0 | 未 ack 记录硬上限：准入时拒绝（`ErrProjectionBackpressure`，可重试） |
| `dataengine.projection.warn_unacked_records` | 0 | ≤ max | 达到即健康 degraded（仍算 ready） |
| `dataengine.outbox.owner` | `dataengine-<sid>` | | outbox 租约持有者名 |
| `dataengine.outbox.workers` / `batch_size` | 2 / 64 | ≥ 1 | 认领并发与批大小 |
| `dataengine.outbox.lease_duration` / `poll_interval` | 30s / 100ms | > 0 | 租约与轮询 |
| `dataengine.outbox.retry_min` / `retry_max` | 1s / 1m | > 0 | 发布失败后的指数退避 |
| `dataengine.outbox.max_pending` / `max_oldest_age` | 0 / 0（不限） | ≥ 0 | **硬上限：超过即 fence 进程**（`OnHardLimit` 接 `onFatal`，`kit/dataengine/mod.go:243`） |
| `dataengine.outbox.backlog_interval` | 1s | > 0 | 积压探测（count + 最老一条）的最小间隔 |
| `dataengine.effects.subject_prefix` | `roost.effect` | | 线上主题 `<prefix>.<topic>` |
| `dataengine.effects.stream` | `ROOST_EFFECTS` | | JetStream 流名，启动时 `EnsureStream` |
| `dataengine.effects.max_age` / `max_bytes` / `duplicate_window` / `replicas` | 168h / 8GiB / 10m / 1 | | 流保留；saga Mod 校验完成回执 TTL 必须大于 `max_age`（T-281） |

### 5.2 `nest.pipelined.*`（由本 Mod 声明，作用于 Nest）

| 键 | 缺省 | 含义 |
| --- | --- | --- |
| `nest.pipelined.allowlist` | `[]` | 允许 pipelined 的 handler 名；不在名单里的返回 `ErrPipelinedNotAllowed` |
| `nest.pipelined.async` | false | Phase 2 异步完成（AfterCommit 在完成池上跑，无请求上下文、无实体锁） |
| `nest.pipelined.async_workers` / `async_queue_capacity` | 0（取 core 缺省 4 / 8192） | 完成池 |

### 5.3 驱动相关

| 键 / 选项 | 缺省 | 含义 |
| --- | --- | --- |
| `mongo.transaction_timeout` | 30s；≤ 0 取 120s | 端到端约束 `WithTransaction`（含提交）；回调失败另加至多 5s abort |
| `mongo.connect_timeout` | 10s | 也作为 server selection 超时 |
| `mongo.require_replica_set` | true | 事务需要副本集 |
| `mongo.index.allow_recreate` | false | 索引定义变了时是否允许删掉重建 |
| Redis `Config.MaxRetries` | 3 | 读命令驱动重试次数；写命令只在确定没执行时重发同样次数；-1 都不重发 |
| `versionstore.RedisConfig.MaxAttempts` | 8 | CAS 重试上限，超了 `ErrConflict` |
| `versionstore.RedisConfig.WriteTokenHistory` | 8 | 每键保留的令牌数（每个约 12 字节） |
| `versionstore.RedisConfig.TTL` | 0 | 有 TTL 的键过期后版本从 1 重来，只给“缺失也合法”的状态用 |

[↑ 速览](#速览) · [实现文档 §7](../impl/03-dataengine.md#7-持久化与协议格式)

---

## 6. 运行与运维

### 6.1 启动顺序（恢复屏障）

`Assembly.Start`（`dataengine/engine/assembly.go:96-167`）：

1. Mongo 建索引（遇选举有界重试）→ JetStream 建效果流；
2. 打开 WAL：拿目录锁，读 checkpoint，检查段连续，修复最后一段尾部（§6.5）；
3. 构造投影器、outbox worker、Runtime；
4. `Runtime.Start`：**先把 WAL 投影到空**（`Projector.Flush`），再挂冷加载器与删除准入，启动 outbox，最后标记 ready（`dataengine/engine/runtime.go:69-108`）。

在 ready 之前，冷加载返回 `ErrRecoveryIncomplete`，Nest 拿到的 committer 返回 `ErrCommitterRequired`。任何一步失败都会停掉已打开的组件；清理也超时时保留 Runtime 等 `Shutdown` 重试，不丢 WAL 目录所有权。

### 6.2 停机

推荐顺序：网关停止接流 → Nest 请求与 Guard 排空 → `dataMod.StopWithContext(ctx)` → 关 Mongo / NATS。DataEngine 内部：摘加载器与删除准入 → **只尝试一次** Flush → 关投影器 → 关 WAL → 关 outbox（`dataengine/engine/runtime.go:158-200`）。超时返回错误并保留没停完的组件，用新 ctx 重试只等剩下的（RR-20260909-03）。没投影完的记录留在 WAL，下次启动恢复。

### 6.3 健康与 readyz

| 状态 | 条件（`kit/dataengine/mod.go:481-511`） |
| --- | --- |
| Fail `not ready` | Runtime 没起来 |
| Fail `fenced` | 发生过 fatal（投影冲突、WAL terminal、outbox 硬上限、删除准入结果未知） |
| Fail `unhealthy` | 投影最后一次失败未恢复、fatal、WAL terminal、WAL 磁盘或未确认年龄超限、outbox 硬上限 |
| Degraded | 未 ack 记录达到预警水位（degraded 仍算 ready，D1） |
| OK | 其余 |

健康消息是一行固定顺序的摘要（`dataengine/engine/health.go:13-15`）：`wal_unacked wal_oldest projection_failures outbox_pending outbox_oldest publish_failures store_failures fatal_projection_conflicts projection_backlog_warning admission_rejected`。

### 6.4 指标

| 指标 | 标签 | 看什么 |
| --- | --- | --- |
| `nestwal.fsync.duration` | | 组提交 fsync 耗时；strict 的锁持有时间主要是它 |
| `nestwal.batch.total` / `append.total` / `bytes.total` | | append / batch 比值是组提交放大 |
| `nestwal.disk.bytes`、`nestwal.pending.tickets` | | 磁盘占用、未完成的 pipelined 票据 |
| `nestwal.reject.total` | `reason=queue_full\|disk_cap` | WAL 准入被拒 |
| `nestwal.recovery.tail_truncated.total` / `.bytes` | `reason=torn_frame\|zero_fill` | 启动时截掉了尾部；`zero_fill` 要告警并核对投影 |
| `nest.pipelined.durable_wait` | `handler` | pipelined 等 fsync 的时间 |
| `dataengine.fence.skipped.total` | `resource` | lease fence 跳过的原生步骤；全部同时跳过多半是协调文档字段漂移 |
| `dataengine.fence.evictions.started.total` / `.failed.total` | | 跳过后驱逐内存实体 |
| `dataengine.load.skipped.total` | `resource` | 非严格加载模板跳过的坏行 |
| `mongo.ensure_index.election_retries.total` | | 启动撞上选举 |
| `versionstore.cas.total` | `store`、`result=applied\|lost` | CAS 冲突率 = lost / (applied + lost) |
| `versionstore.conflict.total` | `store` | CAS 预算用尽 |
| `versionstore.unknown_outcome.total` | `store`、`result=applied\|lost\|unresolved` | 回复丢失后令牌核对的结论 |
| `cache.layered.backfill_failed.total` | | L1 回填被拒或删除失败 |

Projector / Outbox 的计数（Committed、Projected、WALUnacked、FencedEntities、Claimed、Published、PublishFailures、StoreFailures…）经健康消息与 `Stats()` 暴露（`dataengine/engine/projector.go:79-93`、`dataengine/engine/outbox_worker.go:39-47`）。

### 6.5 WAL 启动时的尾部处理

只修最后一段；同时满足以下条件才截断坏点之后的内容，否则 `ErrCorrupt` 拒绝启动且不改文件（RR-20260926-41，`nestwal/wal.go:1086-1144`）：

1. 坏点不早于 checkpoint fence（截掉的从未被确认）；
2. 形态是“不完整尾帧”（帧头不足或帧体越过文件末尾）或“零填充尾”（坏点到文件末尾全 0）。

帧头有效但 payload CRC 不符（例如跨 4 KiB 页的半写回）**拒绝启动**，维护者决定不放宽（`NEST_TRANSACTION_WAL.md` §5）。处置需要人工：确认坏帧在 checkpoint 之后、从未被确认，再决定截断。残余风险：已 fsync 但未投影的记录所在块被介质整块清零时，会被当作零填充尾截掉，唯一线索是 `reason=zero_fill` 指标与告警日志。

### 6.6 错误速查

| 错误（`errors.Is`） | 含义 | 处理 |
| --- | --- | --- |
| `nest.ErrCommitIndeterminate` | 提交结果未知；进程已 fence | 不重试；等新进程从 WAL 恢复后按业务幂等键核对（T-01） |
| `nest.ErrCommitRejected` + `dataengine.ErrFencedEntityPending` | 该实体有原生 saga 步骤在等投影结论 | 可重试，通常毫秒级 |
| `nest.ErrCommitRejected` + `engine.ErrProjectionBackpressure` | 未 ack 记录达到上限 | 可重试；查投影为什么慢 |
| `nest.ErrCommitRejected` + `nestwal.ErrCapacity` | WAL 磁盘或队列满 | 可重试；查磁盘与投影积压 |
| `nest.ErrCommitRejected` + `nestwal.ErrRecordTooLarge` | 单条记录超 16MiB | 业务错误：拆分 |
| `engine.ErrRemoteLeaseFenceUnsupported` | Remote 写与原生步骤回执混在一条事务里 | 设计上拒绝，改业务 |
| `nest.ErrCommitterRequired` | Runtime 未就绪 | 启动未完成或已停 |
| `dataengine.ErrRecoveryIncomplete` | 启动恢复未完成时冷加载 | 稍后重试 |
| `engine.ErrProjectionConflict` / `ErrTransactionIdentity` / `ErrReceiptIdentity` | fatal：Mongo 与 WAL 不一致 | 进程 fence；WAL 保留；人工核对（T-153、T-162） |
| `engine.ErrEntityAggregateNotFound`（也满足 `entity.ErrAuthorityEntityNotFound`） | 聚合不存在或全是墓碑 | 业务按不存在处理 |
| `engine.ErrEntityAggregateCorrupt` | 部分 DAO 缺失 / 重复 / 身份不符 | 数据问题，人工处理 |
| `dataengine.ErrMigrationConflict` | 迁移三轮不收敛 | 查并发写 |
| `fmongo.ErrCommitResultUnknown` | Mongo 提交已发出、结果未知 | 按回执裁决，不重做（T-239） |
| Redis 写 `EOF` / `i/o timeout`（`IsDefinitelyNotExecuted` 为假） | 结果未知 | 见 §4.9（T-259、T-238） |
| `versionstore.ErrOutcomeUnknown` | 令牌核对不出结论 | `Resume` 续核，或按请求 ID 去重 / 回读 |
| `versionstore.ErrConflict` | 单键竞争超预算 | 可重试（T-229） |
| `versionstore.ErrMalformedRecord` | 记录读不出来（含旧信封格式） | 不要原样重试；升级时清空（T-291） |

### 6.7 常见故障 → TROUBLESHOOTING

| T 行 | 现象 |
| --- | --- |
| T-01 | 启动即熔断，日志有 `ErrCommitIndeterminate` |
| T-37 | outbox 积压但 `publish_failures=0`：是 Mongo 侧认领 / ack 失败 |
| T-54 | 停服第一次超时、重试立刻成功但 worker 还在跑（旧版本） |
| T-79 | `Sync` / `Flush` 后票据未完成（旧版本） |
| T-153 / T-162 | `fatal projection version conflict` |
| T-210 | 旧 schema 冷加载失败 / 迁移 |
| T-226 | 原生 saga 步骤回执重复（lease fence） |
| T-229 | versionstore 版本冲突 |
| T-238 / T-259 | Redis 写回复丢失后的重复 / 现在的传输错误 |
| T-239 | Mongo 分区时投影卡住（旧版本） |
| T-267 | readyz degraded 但 200 |
| T-281 | saga 完成回执 TTL 必须大于效果流保留期 |
| T-282 | 启动建索引撞上选举 |
| T-291 | versionstore 旧信封，升级要清空 |

全表见 [TROUBLESHOOTING](../../TROUBLESHOOTING.md)。

[↑ 速览](#速览) · [实现文档 §6](../impl/03-dataengine.md#6-失败与不确定结果处理)

---

## 7. 保证与不保证

### 7.1 保证

| # | 保证 | 条件 |
| --- | --- | --- |
| G1 | 成功回复不早于 handler 的提交点（§3.2） | 使用正式 committer（Mod） |
| G2 | 一次事务的全部 mutation / effect / receipt 作为一条 WAL 记录原子准入；投影时多文档在一个 Mongo 事务里 | 同一 Mongo 部署 |
| G3 | 同一文档的版本按 WAL 顺序前进；旧写不能覆盖新写、不能复活墓碑 | Mongo 版本 CAS |
| G4 | 重放可识别：已应用的记录再投影是无操作，不一致即 fatal 而不是覆盖 | 事务标记 TTL 内（720h） |
| G5 | ack checkpoint 只覆盖连续成功前缀，且不超过已落盘的日志 | |
| G6 | 准入失败一定是“没写”：内存回滚、调用方拿到 `ErrCommitRejected` | 可回滚事务 |
| G7 | 结果不确定时不回滚、不继续服务：fence 并 fail-stop | `OnFatal` 接线（Mod 已接） |
| G8 | 冷加载要么得到完整聚合（全部 DAO、版本、Remote 版本向量），要么失败；不发布半加载实体 | |
| G9 | 冷加载前等待本进程对该实体的在途投影；启动时先投影完 WAL 才接受加载 | 同一进程 |
| G10 | effect：回滚的事务不发；提交的至少发布一次；消费方经收件箱 Mongo 效果恰好一次 | 消费方用 `MongoEffectInbox` |
| G11 | Redis 写 / 脚本一次调用在服务端至多执行一次（驱动层） | 不经 `Raw()` |
| G12 | versionstore 一次 `Update` 不会因回复丢失被应用两次 | 同一进程用 `Resume`，或 mutate 幂等 |
| G13 | 驱动与 Mod 的 Close 幂等、并发后到者等待、关闭后调用返回已关闭错误 | |

### 7.2 不保证与已知限制

- **memory durability 的 handler 改持久字段**：`rollback=none`（memory 快路径）时没有事务，生成 setter 直接 panic；带 `rollback=state|undo` 的 memory 事务里，持久变化会被登记，但 `durableCommit` 的 memory 分支直接返回、不准备也不提交，**变化不进 WAL、也不报错**，DAO 版本不前进（`nest/rollback.go:586-600`；本篇写作时用包内探针确认：committer 调用 0 次、`PrepareMutation` 0 次、err 为 nil）。`NEST_TRANSACTION_WAL.md` §3 写的“禁止修改 persistent 字段”没有运行期强制。业务不要在 memory handler 里改持久字段。
- async 的成功回复可能早于 fsync（§3.2）；async 记录也可能在 fsync 前被投影到 Mongo（重放读的是段文件，推断，未写复现）。
- 跨 database / 跨部署的原子性不在这里，用 saga。
- `StaleFunc` 在 Redis 存储上只是建议性的（读写两次往返）。
- 残余 WAL 风险：已 fsync 未投影的记录所在块被整块清零（§6.5）。
- versionstore 令牌被挤出（同一键后续 > 8 次写）后返回结果未知，不猜。
- Redis Cluster 下的 MOVED / ASK、多机复制延迟、mongos 与主从切换期间的提交只在本机或单机验证过（外部验证 E08 / E10 / E11，[外部验证清单](../../review/EXTERNAL-VERIFICATION-2026-10-06.md)）。
- 投影函数纯度、`//roost:cache` 是否真是缓存等由约定保证，glsvet 只给 `hint:`。

[↑ 速览](#速览) · [实现文档 §4](../impl/03-dataengine.md#4-不变量清单)

---

## 8. 相关文档

| 文档 | 内容 |
| --- | --- |
| [实现文档 03](../impl/03-dataengine.md) | 本分区的实现、不变量、并发、review 检查点 |
| [NEST_TRANSACTION_WAL.md](../../../NEST_TRANSACTION_WAL.md) | WAL 格式、尾部截断判据、fsync 不确定结果（部分包路径是旧的 kit 结构，见实现文档 §9） |
| [NEST_PIPELINED_COMMIT.md](../../../NEST_PIPELINED_COMMIT.md) | pipelined 正确性论证、外化闸门、Phase 2 |
| [USER_GUIDE §3～§5](../../USER_GUIDE.md) | DAO、Nest 回复错误判别表、Commit / Load / Flush |
| [redis/driver README](../../../redis/driver/README.md)、[mongo/driver README](../../../mongo/driver/README.md) | A2 驱动契约表全文 |
| [A1 方案](../../feature/REFACTOR-2026-10-05-dao-unified-rollback.md)、[A2 方案](../../feature/A2-DRIVER-REPLAY-CONTRACT-2026-10-05.md)、[A2 ③ 方案](../../feature/A2-3-VERSIONSTORE-WRITE-TOKEN-2026-10-07.md) | 维护者决定与实施证据 |
| [DataEngine 重构方案](../../feature/REFACTOR-2026-09-24-dataengine.md)、[恢复验收](../../feature/DATAENGINE-RECOVERY-2026-09-24.md)、[压测](../../feature/DATAENGINE-PRESSURE-2026-09-24.md)、[批量优化](../../feature/DATAENGINE-BATCH-2026-09-24.md) | 历史设计与性能证据（有日期和配置，不是永久保证） |
| [v1.23.0 发版说明 DRV / DAO](../../release/v1.23.0/guide-saga-drv-dao-rem.md) | 本版驱动与 DAO 的改动记录 |
| 02 nest、04 sync、05 remote、06 saga、08 skill、09 kit 服务分区 | <!-- pending: ./02-nest.md -->`guide/02-nest.md`、<!-- pending: ./04-sync.md -->`guide/04-sync.md`、<!-- pending: ./05-remote-mirror.md -->`guide/05-remote-mirror.md`、<!-- pending: ./06-saga.md -->`guide/06-saga.md`、<!-- pending: ./08-skill.md -->`guide/08-skill.md`、<!-- pending: ./09-kit-services.md -->`guide/09-kit-services.md`（尚未写出） |

[↑ 速览](#速览) · [实现文档](../impl/03-dataengine.md)
