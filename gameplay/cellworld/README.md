# CellWorld：固定分区大世界

CellWorld 是 Scene 的一种运行模式：固定网格的每个 Cell 由一个进程执行，Player 留在静态归属的 Base 进程，SceneActor 随位置跨 Cell 迁移；NPC 使用同一套迁移协议。业务修改始终经 Nest 与 DAO，客户端同步仍由 Base 的 EntitySync / Interest 承载。

算法与协议在本包，正式装配在 [modules/cellworld](../../modules/cellworld/README.md)，完整 Entity / DAO / 业务接线由 [game-demo](../../demo/README.md) 生成。不要复制另一套授权、迁移、持久恢复或客户端协议。

## 当前范围

提供共享写授权、App 租约失效停止、交接与故障恢复、Buff / 冷却快照、死亡与奖励幂等、NPC 出生账本、内部增量复制、出站隔离与可重试排空。支持固定拓扑，一个进程可同时承载 Base 和一个 Cell。

不提供动态拆格、一个进程多个 Cell、跨多个 Cell 的原子战斗结算、玩家复活玩法或掉落规则。Linux / 跨机 / 生产 Redis HA 必须另行实跑；本机故障测试、交叉编译与短时容量样本不能代替这些验收。具体测量范围见 [容量与验收](../../docs/performance/CELLWORLD.md)。

## 数据归属与事务

| 对象 | 权威与保存方式 |
| --- | --- |
| Player | 静态 Base 的持久 Entity；保存养成、入场计数、回写快照、死亡奖励回执 |
| SceneActor / Monster | 当前 Cell 的临时 Entity；状态写 DAO，参与回滚；位置、速度、HP、面板、Recovery 随 Offer 交接 |
| Scene.Region | 当前 Cell 的区域索引；只在 Scene 的 Nest 锁内读写 |
| OwnerRecord | 共享 Redis CAS 权威；唯一写者 Grant、单调 Epoch、入场高水位、死亡与出生状态，不设 TTL |
| Ghost / Sync mirror | 可重建只读副本；从不执行战斗、授予写权限或覆盖权威数据 |

Actor 心跳只锁自身；提交后的 ActorReport 交给 Scene handler 汇总。多实体请求按 Nest 的锁序，Scene 在前、Actor 在后。报告只能更新已有区域记录，晚到报告不能复活已退出的对象。Mirror / Interest、外发、观察 Hook 都在事务提交后生效，回滚不发布。

DAO 的 `persist` / `sync` 分别默认开启。Player 的授权、回执、Recovery 等私有字段显式 `nosync`；Actor 临时内部字段使用 `nopersist,nosync`。定义与升级检查见 [tag 手册](../../codegen/docs/TAGS.zh-CN.md)。

## 唯一写者与迁移

Cell 必须绑定 App singleton 的本次 incarnation 和 `SingletonLease.Check`。共享授权记录标识完整的实体 ID、Epoch、SID 与 incarnation。只有确认得到当前 Grant 的实例可以进入 Real。

1. 入场创建 Pending，占容量但不模拟。授权 I/O 在有界 worker 队列执行，返回后进 Nest 核对请求与实例身份，才激活 Real。
2. 越过本格迟滞带后，旧 Actor 在 Nest 中冻结；锁外 CAS 冻结共享 Grant，随后发送 Offer。目标确认共享授权与旧 incarnation 活性，CAS 接手并激活。
3. 旧格根据确认结论退役旧 Grant。迟到的接受、拒绝、退出或 I/O 回调不能改变新 Grant；冻结超时不能自行恢复写权。
4. 存储或存活查询结果未知时保持 Pending / Frozen / Dying，不以超时推断另一个写者已死。心跳负责有限间隔续办；队列满返回背压统计。
5. 共享记录确认另一个 Grant 已接手时，本地旧实例退出；死亡记录冲突只清理匹配旧 Grant 的实例，不能卸载新建实例。
6. 已持有授权的实例（同进程现存 Real 被新 Entry 接管）认领被拒时，先按自己的 Grant 退役再销毁；不能只删本地实例而把共享记录留给一个不存在的写者。

租约守卫检查业务入口、准备、提交与外发。本地安全窗口过期即不可恢复地报告失锁；迟到的续租成功不能复活旧进程。长暂停恢复后同样拒绝工作。该保护依赖 Redis 权威记录不丢失、singleton 与业务使用同一身份；不宣称普通网络 Publish 与任意物理暂停之间存在分布式原子性。

Player 入场计数由独立 strict 事务递增并持久到 WAL，再发送 Enter；不从墙钟生成。看门狗重试复用该计数。Input / Attack / Leave 必须匹配当前 Base 与 Entry。移动和续租不增加 strict fsync。静态 Player 跨机迁移仍以已落库数据为准，不能拿旧 checkpoint 或丢失的 WAL 越过共享高水位。

## 心跳、移动与视野

`Period` 默认 50ms（20Hz）。Heartbeat 对迟到的拍不集中补跑；Actor 按 `Beat.Elapsed` 及驱动上限积分，并按 Period 子步检查地面与交接。位置保留千分位整数余量：速度变化、心跳换档和交接保留余量，碰撞 / 世界边缘清除相应余量。新入场从整数位置开始。

静止对象不因心跳推进内容版本。移动、伤害、面板和显式 Recovery 修改推进 `(Epoch, Seq)`；Ghost 在心跳频率发送独立续活脉冲（静止时是空字段增量），Base 接受新视野 Tick 后对相同版本调用 `Mirrors.Touch`，不重打包客户端内容。旧版本、已删除或关闭对象不能靠 Touch 复活。

GhostBand 为 G、交接迟滞 H、视野半径 R、NPC 唤醒半径 W，必须 `R + H ≤ G`、`W + H ≤ G`，并且 R 覆盖 Base AOI 最大离开半径。Ghost 只覆盖邻格边界带，客户端可见性最终由 Base Interest 决定。视野超限按距离取最近对象，并标记 Truncated，不能把被截断的视野称为完整大范围 AOI。

NPC 可配置 `IdlePeriod`：附近没有玩家持续 `IdleAfter` 后降档，有玩家靠近立即升档；Frozen 使用快档完成协议。慢档 Ghost 的过期时间加上 IdlePeriod。降档只改变移动心跳，不代替 AI 调度。`Surface` 钩子提供地面、高度、阻挡和减速规则，相关用法见 [地形说明](../terrain/README.md)。

## 恢复、战斗与资产结算

`RecoveryState` 包含最多 128 个 Buff / 冷却条目，以业务 Key 标识，绝对 UTC UnixMilli 表示到期时间。`SetRecovery` 在 Actor handler 内验证、复制并写 DAO；交接和回写携带快照，重载时按当前时间剔除过期项，不把停机期间重新加到剩余时长。框架不代替业务 Buff 叠加与属性计算。

位置 / HP / Recovery 定时回写 Player，默认 5s。这是发送周期，不是硬 RPO：网络、队列、WAL 和异步 Mongo 投影都可能增加恢复距离。需要更强保证的业务动作必须单独定义持久提交边界。

跨 Cell 攻击由攻击者所在 Cell 固定伤害，转发给目标的当前 owner。随机 HitID 在重传中保持不变，业务 PB 的 AttackID 关联异步结果；重新调用 Attack 是新攻击。同一目标的临时去重表有窗口和容量上限，满时拒绝新伤害，不能淘汰仍有效的记录。Offer 传去重剩余时长，接收机在自己的系统时钟重建期限，避免机器时钟差导致重复伤害。

致死动作先进入 Dying：

- 将 DeathIntent 写进可扫描索引，再在目标 OwnerRecord 上 CAS 持久死亡事实；未确认前不报告结算成功。
- 任意 Cell 可扫描待结算事实并重投。获奖 Player 的 strict 事务内同时执行 `SettleDeath` 与写入目标 Life 高水位回执；回滚时两者一起撤销，重复事实不重复发奖。
- 仅已提交回执的确认允许 NPC 出生槽进入复活流程。确认丢失可以重试；业务回调报错保持待结算。
- 回执确认后尸体是静止对象：不再周期读共享记录、不推进内容版本、不重发伤害结论（丢失的结论由攻击方按同一 HitID 重发得到重复 Applied）。之后的 Leave / 租约到期才做一次收尾并退役。
- `SettleDeath=nil` 只记录幂等回执，不发资产。业务应在此调用钱包 / 背包组件，不做网络 I/O、不另开事务，也不在观察 Hook 中发奖。

死亡索引无 TTL，有分桶容量上限；满时拒绝而不丢资产事实。Player 回执也有容量上限，默认 20000 个目标；超过上限需明确的业务生命周期 / 归档设计，不能直接清空。临时非致死攻击去重不承诺无限历史重放；最初 Base 请求在目标首次解析前尚未绑定 Life，不能将它当永久资产命令。

Player 死亡后重新登录不自动复活。复活规则由业务定义，不通过新 Entry 隐式恢复 HP。死亡回执确认之前不接受新 Entry：现存 Real 退役授权，Base 收到拒绝并按看门狗重试，确认后的入场以 HP 0 装入。死亡玩家 Leave 的关闭标记只属于那一次 Entry，新 Entry 接管时清除。跨多个目标的伤害和奖励不是一笔跨 Cell 原子事务。

## NPC 出生账本

`SpawnBook.Reserve` 按 home SID 与稳定出生 Key 分配持久序号；`SpawnID` 将它映射到业务保留的静态 ID 段。数量、规格、Generation、LastDeath、RespawnAt 可恢复，不依赖进程内随机号或启动时间。

出生槽只拥有数量配额，Actor 跨格不会让原格补刷。原格巡检查共享授权：其他 Cell 仍存活则等待，活性未知则拒绝，死亡事实已确认且达到绝对复活时间后才推进下一代 Life。owner 在确认之后、退役之前崩溃时，巡检核实旧进程已死（与认领相同：活性在 CAS 外查询，CAS 内核对整份记录未变）后代为关闭记录；owner 活着或活性未知时不动。整个过程 CAS 核对上一代，多个巡检者不能重复占同一槽。

重启必须提供相同出生目录；删除、改名或修改已有槽规格被拒绝，须明确离线处理旧槽，避免悄悄遗留活体。动态改刷怪规则不是当前 API 的能力。

## 内部复制与出站

内部协议为 MessagePack v12，与客户端 PB / EntitySync 分离。不兼容旧协议，升级先停旧节点；PlayerDao schema 5 同样须按当前 schema 重建或离线迁移，不能新旧节点混跑。

Ghost / Witness 传全量或字段 mask 增量，零值变化也可表达；接收方严格匹配 BaseVersion。Witness 成员传 add/remove，按 Base 批量共享实体编码，默认最多 16 个观察者一批；Cell 按观察者所在的刷新区块（边长 2R）排序后合批，相邻观察者进同一条消息。定期全量自包含：每个观察者的成员表至少每 `ReplicaRefresh` 发一次全量并连带成员状态全量；同一区块的观察者按区块相位对齐刷新（`refreshDue`），被强制全量的对象在一条消息里只编一次。缺口只拒绝解不出的部分：增量对不上的对象从接收缓存删掉，引用它的视野不交付但成员表照常推进；成员表对不上的观察者不交付。接收方只请求发送方忘掉这些对象与观察者的基线（`replicaReset`），同一对象 / 观察者每个刷新窗口最多请求一次；一次丢包不再让整个 peer 退回全量。缓存有界、可丢弃；缓存不是权威。消息按完整观察者 / 实体拆分，不在任意字节处切断。

出站分为本地调度、控制网络、状态网络与销毁处理：慢网络或销毁不能卡住本地事实提交。控制消息有界重试，默认最多 3 次；耗尽计数并交给业务协议的重试 / 超时状态机。状态队列保存完整对象，按目标、种类和对象合并，真正发送时才计算增量；同版本删除优先，更新不改变 FIFO 排队位置。

`Node.SendExt` 给其他模块复用可信内网通道。队列满返回 false；扩展协议自己处理丢包 / 重复 / 乱序。传输回调必须快速返回。NATS Subject 级 ACL 与部署网络隔离由运维配置，框架不把主题名当身份认证。

停止顺序：关业务准入 → 停出生巡检 → `Drain(ctx)` 等事务、授权 I/O、销毁与出站完成 → 停 Node → 释放实体和 Mods。第一次超时保持依赖与待办责任，新 ctx 可重试；不能因已调用过 Stop 就报告成功。

## 容量边界与诊断

| 参数 | 缺省与含义 |
| --- | --- |
| `OwnershipWorkers / Queue / Timeout` | 8 / 1024 / 2s；只承接授权存储 I/O，不执行 Entity 业务 |
| `ControlQueue / PublishAttempts` | 4096 / 3；传输错误重试有上限，协议仍负责最终收尾 |
| `DestroyWorkers / DestroyQueue` | 4 / 1024；销毁与网络隔离 |
| `ReplicaCacheSize / WitnessBatch` | 131072 / 16；缓存容量与同 Base 合批数 |
| `ReplicaRefresh / MaxWireBytes` | 250ms / 512KiB；自包含修复全量与消息硬上限 |
| `MaxDeathReceipts` | 每 Player 20000 个目标高水位；满时拒绝，不能静默丢弃 |
| `MaxViewEntities / MaxReals` | 256 / 50000；视野截断上限与单 Cell 准入上限，不是实测吞吐保证 |

`Node.Stats()` 可并发读取；game-demo 的 `gm.scene.partition` 返回 Node、mirror、角色数及区域快照。优先检查 `OwnershipErrors / OwnershipQueueFull`、`ReplicaGaps / DecodeErrors`、`OutboxDrops / ControlExhausted`、`QueueAgeMaxNS / OutboundPending`、`Reenters / LeaseExpired` 与 `DeathErrors / SpawnErrors`。它们是诊断计数，不能当资产流水；要结合时间窗口的增量与当前队列，而不是看到累计非零就误判当前故障。

存储断连时保留 Pending / Frozen / Dying，先恢复权威存储与本次进程身份；不得手动删除授权键来强行让旧写者运行。Redis 数据丢失后的恢复不是普通重启，需要停写并按完整备份与业务恢复方案处理。慢消费者先定位 Cell 计算、Base 解码 / AOI、网络与接收端积压，不直接靠增大队列掩盖。

## 正式接入

优先使用 `roost module add cellworld`，装配契约见 [模块 README](../../modules/cellworld/README.md)。game-demo 的关键位置：

| 路径 | 职责 |
| --- | --- |
| `game/partition` | Entity / DAO 适配、面板与客户端载荷 |
| `game/entities/player/cell_base.go` | Base 快照、持久入场计数与死亡回执 |
| `game/cellworldhost` | 通用 Runtime、App / Redis / NATS 装配、稳定出生 ID |
| `internal/service/game/partition.go` | 心跳、镜像、Scene 与服务生命周期 |
| `cmd/loadtest` | 正式 Robot 登录、PB 移动请求、EntitySync 解码与统计 |

game-demo 在死亡结算回调中给击杀 NPC 的玩家增加 1 金币，与回执同一笔 strict 事务；示例不规定产品掉落表。

养成留在 Base；`UpdatePanel` 将版本化战斗面板发给 Real，未确认按同序号重发。最大 HP 增大不自动回血，缩小时夹住当前 HP。跨 Base 队伍视野通过 `GroupView` 与成员快照实现；成员表仍由业务 Team 管理，周期回源兜住通知丢失，超过三个快照周期移除失联成员。

## 验证与复跑

```sh
GOWORK=off go test -race -count=1 -timeout 10m ./framework/app ./framework/sync/entitysync/policy ./gameplay/cellworld
# 本机多进程，真实 App / Redis / NATS；客户端传输、持久提交用替身
ROOST_CELLWORLD_PERF=1 ROOST_CELLWORLD_PERF_COUNT=1000 ROOST_CELLWORLD_PERF_NPCS=2250 \
  ROOST_CELLWORLD_PERF_ACTIVE=5 ROOST_CELLWORLD_PERF_SECONDS=60 \
  GOWORK=off go test ./gameplay/cellworld -run '^TestCellWorldMeasure$' -count=1 -timeout 5m -v
```

`PERF_PLAYERS=cell` 测热点，`spread` 为分散；`ACTIVE=1|5` 是运动实体比例，NPCS 是每 Cell 数量（4 个 Cell）。GOMAXPROCS 沿用环境，不强制限制。测量不要与 race、编译、索引重建并跑。

真实账号 / TCP / 存储 / 重启：`scripts/test-seamless-demo.sh`，只对脚本声明的私有依赖运行，参数见脚本。生成工程的 `TestCellDeathRewardAndRecoverySurviveFileWAL` 覆盖 strict 回执 / 钱包 / Recovery 的文件 WAL 重开恢复，不等于 Mongo 投影验收。

Robot 使用 `-scenario cellworld -count 1000 -cellworld-duration 15m -cellworld-active 5 -max-p95 1020`。输入 ACK 延迟与同步交付延迟分开：当前 Robot 输出 PB 请求 ACK P99、实际请求数、错误数、Sync 帧数 / 字节与可见对象数；没有把 ACK 当成客户端收到状态的 50ms 指标。`scripts/test-seamless-demo.sh` 的 `ROOST_SEAMLESS_CAPACITY=1` 配合 `ROOST_SEAMLESS_ROBOT_ONLY=1` 创建 9000 NPC，Robot 用 30s 正式移动预热分散位置；用 `ROOST_SEAMLESS_ROBOTS=1000` 得到总计 10000 个 Actor。普通故障演练不启用该配置。
