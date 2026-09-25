# Entity Sync 双模式正式接入方案

日期：2026-09-24。状态：方案已实施，未提交、未发布。本文保留设计时的接口草案与验收安排；实际 API、落点、验证与限制以[实施记录](IMPLEMENTATION-2026-09-24-sync-modes.md)为准。

本方案接续 [Nest 审查](REFACTOR-2026-09-24-nest-and-immediate-sync.md)，替代其中以示例业务接线为参考的 S1–S3 实施描述。周期同步与变化触发同步都是正式能力，前者保持默认；实现、装配、生成器及验收均落在 roost-core 正式模块中。

## 1. 设计结论

保留 [Entity Sync 四层设计](../../ENTITY_SYNC.md)：Entity 管内容，Manager 管订阅与交付，policy 管兴趣关系，应用提供实际网络及业务规则。两种模式共享同一套内容、Profile、版本链、订阅、组帧、可靠传输和恢复实现。

| 模式 | 业务成功之后 | 发送调度 | Interval 的含义 |
| --- | --- | --- | --- |
| `periodic`（默认） | 合并 dirty，登记 pending，不立即打包 | 到周期时锁内捕获，锁外组帧发送 | 正常批处理周期，默认 50ms |
| `on_change` | Entity 解锁前冻结同步内容并登记任务 | 解锁及提交条件满足后立即唤醒发送 | 兜底检查周期，默认 50ms，即 20Hz |

两种模式并不保证每个 setter 或每笔事务单独发一个网络包。一次业务操作的多字段修改合并处理，多个实体的完整更新包仍可合装一帧。即时模式不会人为等待下一个 50ms 窗口。

业务 Handler、组件、DAO setter、订阅规则、客户端解码在两种模式下相同。第一版的模式作用域是一个 Manager，启动后固定；不增加 per-handler/per-profile 模式，也不在运行中热切换一半完成的交付。Profile 继续只表达有限内容视图，不承载发送频率。

## 2. 配置和启动 API

建议增加下列 API，以下均为待实现的接口草案，不是当前可直接编译的用法：

```go
type SyncMode uint8

const (
    ModePeriodic SyncMode = iota // 零值，保持现有行为
    ModeOnChange
)

type ManagerConfig struct {
    Mode SyncMode
    Interval time.Duration
    // 其余现有 Transport、Profile、预算、水位、容量字段保留。
}
```

沿用 `NewManager(ManagerConfig)`，不为这个开关重写为另一套构造器。Nest 增加一个注入同步提交能力的 option：

```go
syncManager, err := entitysync.NewManager(entitysync.ManagerConfig{
    Mode: entitysync.ModePeriodic, // 改为 ModeOnChange 即选择变化触发
    Interval: 50 * time.Millisecond,
    Transport: transport,
    DurableWatermark: durableLSN,
})
if err != nil { return err }

engine := nest.NewEngine(
    nest.NestOptionWithGetter(entityAccess),
    nest.NestOptionWithTransactionCommitter(committer),
    nest.NestOptionWithEntitySync(syncManager), // 拟新增，两种模式都使用
)
```

`NestOptionWithEntitySync` 接收定义在 `entity` 中的窄提交接口，Manager 实现该接口；Nest 不导入 sync，Sync 不导入 Nest。该 option 负责绑定提交/解锁边界，不再提供第二个 Mode 开关。

生产配置采用一个来源：

```yaml
sync:
  entity:
    mode: periodic    # periodic | on_change，省略为 periodic
    interval: 50ms   # 省略为现有 DefaultInterval
```

正式启动装配把配置转换为同一个 ManagerConfig，纯 Go 项目直接填该结构体。配置解析使用现有配置层，`kit/nest` 继续转发显式 NestOption，不让 Nest ticker 接管 Sync 的 interval。若启动层提供显式覆盖，顺序为默认值 → 配置 → 显式 option/参数，并输出最终生效值；模式只存一份。

未知模式、非法周期在启动时拒绝。`on_change` 必须具备正式提交边界接线，缺失时启动失败，不能名义上即时却继续等待周期。独立于 Nest 的系统可以实现同一提交协议；单独调用 MarkDirty 不足以证明事务已经成功。已有不注入该 option 的周期调用方继续有效。

## 3. 正式写入链路

```text
业务组件 / 正式生成 DAO setter
  → 记录同步变化（与持久化 PersistChange 分开）
  → Nest 完成业务操作与提交判定
  → 在仍持有 Entity 锁时交给统一同步提交入口
      ├─ periodic：合并 dirty / pending
      └─ on_change：合并 dirty → 冻结内容 → 登记尚未放行的任务
  → 释放本次业务持有的全部 Entity 锁
  → 标记任务可放行；持久化或远端确认未满足时继续暂缓
  → 同一个 Manager 执行捕获/交付流水线
```

### 变化如何自动收集

- 正式 `codegen/internal/entity` 与 DAO 生成器提供同步变化收集能力，Nest 在统一成功出口消费。业务不再需要在每个方法末尾调用 PublishSyncDirty，也不自行注册发送回调。
- 沿用生成 DAO 的同步字段 schema 与 `SubjectSyncPacker`。同步 mask 的映射由实体内容声明负责；多个 DAO 的字段位不能直接 OR，因为各自 bit 0 可能代表不同字段。单 DAO 可使用现有 schema；多 DAO 必须声明到实体内容掩码的映射，无法无歧义映射时使用明确的 full-dirty，不能猜测。
- 增加事务内的同步变化记录，用同一实体去重；回滚丢弃本次新增标记，不清除以前尚未交付的 dirty。同步专用、非持久化字段也必须进入该记录，不能仅以存在 PersistChange 推断实体发生同步变化。
- `Tracker.TakeSyncDirty` 会消费掩码。正式接线必须统一发布同一份变化给需要的消费者，或使用独立消费确认；客户端同步不得先 Swap(0) 吞掉服务间同步尚需的变化。不可把接线简化成 Nest 无条件遍历所有 DAO 并清空 tracker。
- 手写 Entity 可以实现同一内容变化收集能力；已有 `MarkSyncDirty` / `MarkSyncFullDirty` 保留。事务内调用交由正式边界暂存，事务外调用仍登记 pending；要取得锁内冻结保证，手写系统必须使用正式的锁/提交作用域。
- Entity 未注册到 Manager 时不生成无人消费的内容队列，保留必要的内容状态；后续注册/订阅走现有初始快照流程。

这是一项正式生成能力升级。标准业务工程重新生成即可获得接线；自定义字段聚合只需在实体 schema/packer 层适配一次，两种模式共用。

## 4. 提交、解锁和确认是三个不同条件

同步任务具有“已准入内容、实体已释放、允许外化”三个条件。真正发送必须同时满足，不以某个回调名称推断全部条件成立。

| Nest 路径 | 锁内处理 | 外发条件 |
| --- | --- | --- |
| memory | 成功出口收集变化；on_change 冻结 | 本次操作成功且全部锁释放 |
| async / strict | 按原提交语义完成准入，再处理同步 | 锁释放；遵守配置的持久化门槛，async 不冒充 strict |
| pipelined | Enqueue/Accept 成功，写入 CommitLSN 后冻结 | 全部锁释放且 CommitLSN 不高于 durable watermark；ticket 确认可唤醒重查 |
| broadcast | 每个独立实体操作分别执行同一协议 | 不把整批广播伪装成跨实体事务 |
| remote batch | 可锁内准备，但任务保持暂缓 | 原有远端最终确认、解锁与持久化条件全部满足 |

已确认的回滚/准入拒绝不发布本次结果；提交结果不确定沿用 fencing/recovery，不外发成功状态。即使 WAL 很快、完成池抢先运行，也不能绕过解锁屏障。捕获失败不回滚已经提交的业务，保留 pending 并记录错误，走明确重试/快照恢复。

普通 Entity release hook 不具有成功事务信息，不作为唯一入口。`AfterCommit` 已经可能在锁外运行，也不用于读取并冻结本笔事务的实体内容。使用本轮已修正的锁内准入边界，并在框架执行路径中显式接入 release/confirm；业务不手写这些钩子。

跨实体事务的每个 Entity 仍保持自己的内容版本，不新增全局客户端原子事务协议。同一事务的可发送屏障等待全部目标锁释放；同一 Entity 的捕获顺序取自其锁内变化顺序，不依赖完成池的主实体哈希顺序。

## 5. 共用 Sync 流水线，避免另造一套同步

```text
周期到期 / 即时唤醒 / 手动 Flush
              ↓
       单一 Manager 执行入口
              ↓
        处理待生效的兴趣关系
              ↓
  取得内容：消费冻结结果，或锁内捕获 pending
              ↓
     检查放行条件 / 持久化水位
              ↓
     原有订阅、Profile 与快照预算
              ↓
     共享组件编码 → 完整实体包组帧
              ↓
     Transport.Push → 逐帧采纳与版本结算
```

- Entity 继续只管理内容，不保存 session、observer 或历史；订阅者表仍属于 Manager 的 subject。
- Profile 的字段白名单、优先级和授权规则不变；锁内捕获只读取不可变的 Profile 需求描述，不反向获取 Manager 的订阅锁。
- 当前 Flush 的锁顺序是订阅锁 → Entity 锁。重构为“复制需求和 revision → 释放订阅锁 → 捕获/取得冻结内容 → 按 revision 核对交付”，不得保留相反的锁嵌套。
- Manager 的即时通知和周期任务共用执行所有权，不能并行修改同一会话的 tick、引用表和基线；手动 Flush 也通过同一入口。
- 保留同版本、同 Profile 内容的共享编码；每会话外层帧独立。完整 EntitySync 包仍是分帧最小单位，单包超硬上限明确失败。
- `ErrRetryLater`、逐帧成功前缀、全量恢复、SessionLost、held/ready、订阅来源与 revision 规则继续复用。线格式、客户端解码、Namespace 及 Profile 身份不变。

### 连续变化与有限缓存

不把当前 `PreparedSubjectSync` 改成无限业务提交历史。冻结内容与 prepared 交付结算分开，保留“交付结算推进内容版本、dirty 按代际清理”的约束。

推荐每个 subject 最多一个在途交付和一个待处理内容槽，并设置 Manager 总冻结字节上限：

1. 空闲且基线一致时捕获 delta；内容记录带捕获代际、CommitLSN、Profile 与基线身份。
2. 已有在途/待处理内容，又发生修改时，不能直接覆盖旧 delta 或拼接任意 packer 的字节。锁内冻结最新完整视图，用 Full 内容合并尚未准入网络的状态；已经准入的字节不能撤销。
3. 交付前再次校验基线；冻结 delta 的基线失配时不重新贴一个版本号强发，转完整快照恢复。新 Profile 缺少内容时，对整个 subject 重新捕获一致版本，不能把不同时间点的数据拼成同一内容版本。
4. 槽位或字节上限不足时保留 dirty/恢复意图，不在 Entity 锁内等待网络，不无限分配；由同一 Manager 重试最新全量。降级有明确计数，不能宣称过载下每次变化都已锁内冻结并即时送达。
5. 两种模式都属于状态复制，允许合并未交付的中间状态；必须逐个交付的伤害事件、交易消息走事件/Outbox，不能偷偷依赖状态字段经历了每个中间值。

此处是即时路径最需要先验证的部分。需要先证明基线、代际、不可变数据所有权和有界内存，再进行性能优化；不能复用现有单在途 token 后忽略其约束。

## 6. Interest、预算和兜底

空间移动、关系变化、ReadySession、新订阅、退订、Profile 切换、实体退役都可以产生同步工作，不只字段 dirty 才唤醒。

正式 policy 提供有序的待处理事件入口；Entity 锁内只记录不可变业务事实，锁外在同一调度轮先 Apply 兴趣关系，再决定接收者。具体地图、位置提取及关系授权由正式应用规则提供；这些规则通过统一接口装配，不由 Nest 推断游戏语义。两种模式使用同一规则：periodic 到周期处理，on_change 收到事件立即处理。

对发送在途才发生的关系变化继续使用订阅 revision，已准入的帧不能撤销，但后续发送必须按新授权视图处理。启动必须确保初始政策完成应用后才放行会话。

50ms 周期用于补处理 pending、持久化暂缓、预算延期、恢复和合并通知遗漏。数据先登记、后通知；内部重排不反复发送即时通知，防止水位未推进时空转。

**即时唤醒不能绕过快照预算。** on_change 下所有唤醒共用当前 50ms 窗口的快照额度，周期边界补充一次；不能每次 Flush 重新获得 1000 个对象额度。periodic 的现有每轮预算行为保持。增量/full-dirty 更新、remove 的原语义不变，全局可靠队列字节/年龄限制继续生效。恢复用全量不代表无限制资源使用，额外受冻结内容容量和原有传输额度约束。

## 7. 正式文件落点与依赖

沿用现有包，无新增 `syncmode`、`scheduler`、`bridge` 包，无两套 Session/Manager：

```text
entity/
  subject_sync.go          内容捕获与 prepared 结算分离、代际一致性
  sync_commit.go          拟新增：观察者无关的提交/释放能力契约
nest/
  nest.go                 拟新增 NestOptionWithEntitySync 注入
  rollback.go             成功准入、失败和提交确认出口
  nest_dispatch.go        全部 Entity 解锁屏障，覆盖各分派类型
  pipelined_completion.go 提交确认通知，沿用原完成顺序
sync/entitysync/
  manager.go              Mode/Interval 校验、统一运行循环与生命周期
  flush.go                共用捕获/交付流程、避免反向锁序
  pending.go              拟新增：有界冻结内容、放行条件和合并唤醒
  snapshot_budget.go      即时模式共用窗口额度
  policy/interest.go      统一事件处理接点
kit/nest/nest_mod.go       保持正式启动 option 接线及启动校验
codegen/internal/entity/  生成 Entity 变化收集与内容映射接线
codegen/internal/dao/     同步字段记录与消费者所有权验证
scripts/perf/sync-aoi/    正式 Nest → Sync 负载入口与双模式对照
```

依赖保持 Nest → Entity、Sync → Entity、policy → Sync。配置层只负责解析和装配；业务提供 packer/Interest 规则/Transport，不实现同步调度状态机。

启动顺序：DataEngine 恢复就绪 → 创建并连接 Manager/Interest/Nest → 启动 Sync 消费者 → 开启 Nest 和接流。停止顺序：停止接流 → 停止 Nest 准入并排空提交 → 排空 Sync → 停止传输 → 停止 DataEngine。水位来源不能在 Sync 排空前被关闭。

## 8. 实施与验收

| 批次 | 交付 | 必须验证 |
| --- | --- | --- |
| A | 模式零值/解析、启动 option、正式变化收集 | 默认仍周期；错误模式/漏接线拒绝；多个 DAO mask 映射；客户端与服务间同步不互相消费 dirty |
| B | Nest 提交与解锁协议，先接周期模式 | single/multi/multiGroup/broadcast、memory/async/strict/pipelined、remote 确认；回滚、拒绝、panic、不确定提交不漏边界 |
| C | 统一捕获/交付及 on_change | 锁内冻结、锁外发送；连续修改、基线失配、Profile 缩小、部分成功重试、通知早于解锁、队列满、停机 |
| D | Interest 与预算调度 | 移动及关系立即生效；新订阅/ready/退役唤醒；密集唤醒不突破窗口预算；关闭通知后周期仍能推进已登记任务 |
| E | 正式端到端和业务负载 | 生成器最小工程编译与行为测试；真实 Nest + Sync + 独立 TCP 客户端；两模式相同输入对照 |

功能验收使用正式包的集成测试和 codegen testdata 最小编译夹具；夹具只用于验证生成能力，不承载生产机制。性能程序直接装配正式 Nest、Entity、Interest、Manager 和 AsyncTransport，不生成或启动示例游戏。

保持 1000 玩家、10000 全局实体、每人约 50 可见；固定变化时间线：1% 为每 50ms 100 次变化、5% 为每 50ms 500 次，即 2000/10000 次每秒，与发送频率脱钩。分别记录两模式的内容正确性、版本/最终可见集、持锁时间、业务准入到客户端延迟、p99/max、frames/s、bytes/s、分配量、恢复次数与冻结队列峰值。即时模式包数可能增加，不要求帧数与周期模式相同。

延迟从实际业务修改及成功提交两个时点分别计量；WAL 等待和网络耗时单列。使用相同提交器、相同网络及相同 durable 配置对照，不通过关闭水位门槛制造即时模式的性能收益。保留严格 50ms 的原门禁与失败样本。

每批运行适用的 race 与生成工程检查，配置/生命周期接入完成后检查实际 imports 和全模块 build。未跑真实 WAL、跨机弱网或长稳测试时明确标注，不将本机测试解释为生产容量承诺。

回退方式是停止并排空当前实例后以 `periodic` 配置重启。默认与线格式不变，客户端无需为调度模式升级；注册、初始基线和会话 epoch 仍按现有恢复规则处理。

## 9. 本次核对依据

最近图谱项目 `Users-whb-roost-roost-core`，代际 `2026-09-24T03:57:02Z`。核对了 ManagerConfig/Register/Flush、SubjectSyncState/PreparedSubjectSyncBatch、Tracker、Nest 准入/完成、kit/nest 装配及正式 entity 生成代码；十个相关路径 coverage 均为 metadata_match/no_recorded_issue。该信号不代表全仓完备。

现有源码的 ManagerConfig 尚无 Mode；Register 的 notifier 只登记 pending；prepared commit 按代际清 dirty；Tracker.TakeSyncDirty 消费 mask；kit/nest 已支持追加正式 NestOption。因此本方案需要实际新增上述能力，不能只写一个 YAML 就称为接通。
