# Entity Sync 代码地图与问题清单（2026-09-22，供整体 review）

> 只读整理，不含新的结论性判断；末节"值得整体看的几处"是我在修 U-0277 过程中看到的模式，供 review 时有的放矢。
> 基线 core `606df4a`（U-0277 已入，未发版）。行号以该基线为准。
>
> **2026-09-22 晚 M-13 之后**：§0 的链路与 §1.1 的 entitysync / room 三行已过时——coordinator、RoomBroadcaster / EnvelopeSink / TransportSink / RoomManager 全部删除，
> 新形状见 [ARCH-10](../bugfix/ARCH-10-sync-manager.md) 与 [M-13](../bugfix/M-13-entitysync-manager.md)。§2 的历史问题清单与 §3 的观察仍然有效（作为"为什么要重写"的依据）。

## 0. 一条链：状态怎么从实体到客户端

```
写入侧                                   订阅侧
─────────────────────────────           ────────────────────────────────────────
DAO setter → owner.PublishSyncDirty()    policy.AOI（AOI 格 / 半径 / band）
  → entity.SubjectSyncState.MarkDirty      → demo InterestSystem.Tick() → []SubscriptionChange
  → dirtyGeneration++ / notifier            → demo Scene.applyChanges
  → room.RoomBroadcaster.markDirty            → room.Subscribe / Unsubscribe
                                                → entitysync.SubscriptionCoordinator（订阅表、快照、Leave）
                                    ↓
room.RoomBroadcaster.flushDirty（ReplicationInterval，demo 50ms）
  → flushStateBatch：每个脏 subject Prepare(profiles)      ← entity.PreparedSubjectSync（版本、mask、CommitLSN）
  → coordinator.DistributeBatch：按 Active 订阅展开成 DeliveryEnvelope
      ├ durability gate：update.CommitLSN > DurableWatermark() → 整批推迟，脏位保留   ← dataengine.DurableLSN
      └ RoomEnvelopeSink.AdmitEnvelopes：按 (room, subscriber) 分组、subscriber id 升序 → []RoomFrame
          → RoomTransportSink.AdmitRoomFrames：编码 statesync 帧（ObjectRef / 组件 delta），SlowConsumerPolicy
              → nettransport.AtomicBatchTransport.AdmitBatch（demo：sceneLane → PushPlayer → player TCP）
  → 成功：PreparedSubjectSyncBatch.Commit（按 generation 清 mask、ContentVersion 前进）
  → 失败：AbortWithError（脏位保留，下一 tick 重试）
                                    ↓
客户端：statesync.Reassembler（datagram 分片）→ DecodeRoomWireFrame → DecodeRoomSubjectUpdate → bson 合并到本地视图

生命周期：player TCP OnSessionClosed → Scene.Leave → Interest.Leave（emit InterestLeave）→ Unsubscribe ×N → RetireSubject → UnregisterSubject
跨进程：room/jetstream_syncbus.go / nats_syncbus.go（ISyncBus，房间间的同步总线，不在单进程链路上）
```

## 1. 代码目录

### 1.1 core 主链（roost-core/`sync/`，ARCH-12 S3 之后的布局，2026-09-23）

| 包 / 文件 | 角色 | 关键类型 / 入口 |
| --- | --- | --- |
| `entity/subject_sync.go` | 内容：每个 subject 的同步状态机（脏位、代际、`PrepareTick`、Commit/Abort、CommitLSN） | `SubjectSyncState`、`PreparedSubjectSync` |
| `sync/entitysync/` | 机制：进程一个 `Manager`，subject 私有订阅者表，held/ready 会话，每会话一帧，prepare/commit 两阶段，持久化门槛 | `Manager`、`Transport`、`AsyncTransport`、`SessionID`（= `nettransport.SessionID`） |
| `sync/entitysync/policy/` | 组织：谁订谁 | `Interest`（AOI + 关系源）、`Group`、`Direct`、`RelationSource` |
| `sync/frame/` | 帧格式：`Frame` 的 `Encode / Decode`、对象 / 组件 delta 类型、`Limits` | `Frame`、`ObjectDelta`、`ComponentDelta`、`ObjectRef`、`Limits` |
| `sync/nettransport/` | 传输：UDP / KCP / QUIC 会话传输、AEAD、`AsyncTransport` 双 lane、会话与传输契约、datagram 分片头 | `SessionID / SessionInfo`、`Transport`、`AsyncTransport`、`FragmentDatagrams` |
| `sync/lockstep/` | 帧同步（输入帧）：与状态同步并列 | `Room`、`Sequencer`、`RedundantEncoder` |
| `sync/syncbus/` + `driver/` + `mirror/` | 服务↔服务总线：契约、NATS / JetStream 实现、副本复制器 | `ISyncBus`、`NewJetStreamSyncBus`、`mirror.Replicator` |
| `spatial/` | 基建：二维网格几何（`Point / Rect / BlockIndex / Terrain / 寻路`） | 只被 policy 与 demo 用 |

历史（M-13 之前的 `room/`、`entitysync/subscription.go`、`statesync` 的 Replicator）已全部删除，见 [M-13](../bugfix/M-13-entitysync-manager.md)、[M-15](../bugfix/M-15-statesync-dead-code.md)、[M-17](../bugfix/M-17-sync-layout.md)。

### 1.2 相邻但不在这条链上的（review 时可按需取舍）

| 包 | 关系 |
| --- | --- |
| `syncstream/` | JetStream 上的"状态流 + 文件 journal"（History/Publisher），服务于 skill / 副本同步，不是场景实体同步；历史上 RR 密集（U-0211–U-0216）。 |
| `lockstep/` | 帷幕战斗的帧同步房间，与 entity sync 只共用 player TCP push 通道；U-0193–U-0199 一批。 |
| `entity/remote_*.go` + `remoteentity/` | 远端实体快照（L1/L2、版本屏障、删除墓碑），是"进程间的实体状态"，与客户端复制无关；T-68–T-83 一批。 |
| `dataengine/` | 只通过 `DurableLSN()` 进入本链（水位门槛）。 |

### 1.3 生成侧（codegen 模板 `roost-core/demo/`，生成物落在工程里）

| 模板 | 生成物 | 角色 |
| --- | --- | --- |
| `demo/internal/service/game/scene.go.tmpl` | `internal/service/game/scene.go` | **应用与 core 的接缝**：`sceneLane.AdmitBatch`（逐个 PushPlayer，FailBatch）、`newScene`（sink/manager 装配、水位接入、`SlowConsumerFailBatch`）、`Join/Moved/Leave/applyChanges`、`watchSessions`（会话关闭 → Leave）、`dropAsync`（推送失败 → Leave） |
| `demo/game/scene/runtime/interest.go.tmpl` + `relations.go.tmpl` | `game/scene/runtime/` | `InterestSystem`：spatial 之上的 band / profile 选择、`SourceSelf`（自订阅，band 恒 0）、team 关系、`SubscribeFailed` 重试（U-0267） |
| `demo/game/scene/runtime/{pathfind,terrain,refresh}.go.tmpl` | 同上 | 出生点 Place、可走性/占位、刷怪 |
| `demo/game/entities/player/sync_packer.go.tmpl`（monster 同） | `game/entities/*/sync_packer.go` | `SubjectSyncPacker`：把 DAO 的 `MarshalSync` 文档当 payload；**忽略 profile**（mask 不透明） |
| `demo/game/entities/player/map_component.go.tmpl` | `map_component.go` | `EnterScene / MoveTo`：`from == to` 短路、`Walkable` 校验、`PublishSyncDirty` |
| `demo/game/handler/enter_scene.go.tmpl` | `handler/enter_scene.go` | 记住的位置 → Place → EnterScene |
| `demo/protocol/def/entity_sync.go.tmpl` | `protocol/pb` | `EntitySyncPush{Datagram, Payload}` 推送消息 |
| `demo/cmd/loadtest/main.go.tmpl` | `cmd/loadtest` | 机器人侧：`scene_watch`（装 capture、Reassembler）、`consume`（合并文档）、`scene_expect`（只查字段存在） |
| `demo/internal/access/player/tcp/server_gen.go`（生成） | 接入层 | `pushPlayer`（无会话 → `ErrSessionNotFound`，不计指标）、`OnSessionClosed` 生命周期源（U-0243） |
| `codegen/internal/entity/{parse,gen}.go` | `<entity>_gen_wire.go` | `sync=true` / `subjectPacker` / `syncTopic` 标记 → `EntitySyncBuilderParam`（RR-20260918-01/-07） |

## 2. 历史问题清单（按层）

状态：✅ 已修复（U 号）· 🟡 已登记未修 · ⚪ 观察 / 候选（W）。P 级沿用登记时的。

### 2.1 entitysync + entity（订阅协调 / 状态机）

| 编号 | P | 一句话 | 状态 |
| --- | --- | --- | --- |
| RR-20260922-01 | P1 | 断线观察者撤不掉订阅（Leave 投递成功才算撤），room 每 flush 都为死会话生成帧，排在其后的观察者全部停摆 | ✅ U-0277（未发版）· T-172 |
| RR-20260915-03 | P2 | 持久化水位门槛只在 FlushSubject，订阅快照 / profile 切换 / 直接分发绕过 | ✅ U-0206 · T-100 |
| RR-20260918-02 | P2 | RoomBroadcaster 私有持有 coordinator，房间层没有水位接入口 | ✅ U-0233 · T-127 |
| B-18 | — | 入口守卫无覆盖（Subscribe/Unsubscribe/FlushSubject 参数、nil sink） | ✅ U-0104 |
| RR-20260913-01/-04/-05/-06/-08 | P2 | `entity` 远端快照一批（删除版本屏障、等待名额泄漏、L2 冲突可见性、同版本校验、过期准入） | ✅ U-0174/0175/0180/0181/0187（远端实体，不在客户端复制链上） |

### 2.2 room（房间 / 帧 / 传输准入）

| 编号 | P | 一句话 | 状态 |
| --- | --- | --- | --- |
| RR-20260920-02 | P1 | 普通 delta 走 latest-only datagram，待发帧被下一帧替掉，独有字段永久丢失 | ✅ U-0260 · T-154 |
| RR-20260915-04 | P2 | 慢连接剔除通知绑在批次结果上，剩余批次失败即丢失，room 保留失效订阅 | ✅ U-0207 · T-101 |
| RR-20260915-05 | P2 | SetDownstream 只换指针不迁移慢消费者回调 | ✅ U-0208 · T-102 |
| RR-20260916-02 / -03 | P2 | JetStream 持久消费者身份不含 Prefix / 同 topic 多本地订阅竞争同一消费者 | ✅ U-0210 / U-0209 · T-103 |
| RR-20260910-01 | P3 | 极短 IdleTTL 推导出零扫描周期 | ✅ U-0163 · T-57 |
| B（kit 时代） | — | 同步总线 / 信封汇参数守卫 | ✅ U-0095 |

### 2.3 statesync（帧格式 / 老 Replicator / LOD）

| 编号 | P | 一句话 | 状态 |
| --- | --- | --- | --- |
| RR-20260914-10 / RR-20260915-02 | P2 | 同 tick 重投影覆盖 sent[tick]，迟到 ACK 绑错基线；残余窗口在"先发后 Commit" | ✅ U-0200 / U-0205 · T-94 T-99 |
| RR-20260914-11 | P2 | ACK 路径清 forceFull，旧发送的 ACK 取消恢复意图 | ✅ U-0201 · T-95 |
| RR-20260914-12 | P3 | 单片重组绕过 MaxFrameBytes | ✅ U-0202 |
| RR-20260914-13 | P2 | LOD 按绝对 tick 采样，错相组件永远保留旧值 | ✅ U-0203 · T-97 |
| RR-20260915-01 | P2 | ApplyDelta 暂存 map 用最终存量上限检查，满容量替换看 ID 排序 | ✅ U-0204 · T-98 |
| B-22 | — | 增量帧编解码计数 / 大小上限 | ✅ U-0067 / U-0139 |

> 2026-09-23 · M-15：本节涉及的 Replicator / SessionState / LOD / ApplyDelta / Reassembler 已整体删除（ARCH-10 后零引用），上面的修复记录只剩历史意义；`statesync` 现在只有帧编解码、帧类型、Limits、分片头与传输契约。

### 2.4 spatial + 生成工程的兴趣 / 场景层

| 编号 | P | 一句话 | 状态 |
| --- | --- | --- | --- |
| RR-20260920-06 | P1 | 被房间拒绝的 subscribe 只记日志就丢掉，观察者永久收不到该 subject（兴趣系统先标已订阅，重发只在 band 变化时） | ✅ U-0267（codegen v1.15.28）· T-161 |
| RR-20260918-06 | P2 | 会话关闭没有生命周期事件，scene / presence 靠推送失败懒清理 | ✅ U-0243（+09-19 补修）· T-137 |
| RR-20260919-03 | P1 | 会话关闭订阅者 panic 穿出生命周期 goroutine，game 进程崩 | ✅ U-0247 · T-141 |
| RR-20260918-08 | P2 | 单观察者 AOI block 数无预算，合法配置可登记 40,401 格 | ✅ U-0240 · T-134 |
| RR-20260913-02 | P2 | 旧兴趣释放取消重新订阅（remoteentity 的兴趣代际） | ✅ U-0184 · T-78 |
| RR-20260918-01 / -07 | P2 | `sync=true` 生成物与 Core 配置不兼容 / `syncTopic` 裸标识符被当字面量 | ✅ U-0229 / U-0239 · T-123 T-133 |
| U-0224 | — | DAO 嵌套 struct 无 BSON 表示，同步 / 落库只剩 `{"dirtyhook": {}}` | ✅ · T-118 T-126 |
| M-12 / ARCH-06 | — | 生成的同步字段词汇表（packer 看不到字段 mask 常量的问题，承接 W-2026-09-18-02） | ✅ 重构 |
| RR-20260917-09 | P3 | battle demo 宽限期不开帧（lockstep，相邻） | ✅ U-0228 |

### 2.5 仍开着的（W，等 review 分流）

| 编号 | 一句话 | 与本链的关系 |
| --- | --- | --- |
| ~~W-2026-09-22-03~~ | 生成工程 `sceneLane.AdmitBatch` 逐个推、遇错整批放弃；`pushPlayer` 的"无会话"不计指标 | RR-01 的另一半，同日 ✅ U-0278（按失败种类分流）· T-173 |
| W-2026-09-22-02 | 进程正常停止后重启 WAL 回放 `duplicate key`，进程起不来 | dataengine，判别实验准备阶段撞出 |
| W-2026-09-22-01 | `redis/driver` toxiproxy 锁用例 3/4 红 | 无关 |

### 2.6 排障行索引（本链相关的 T）

T-57 · T-78 · T-94 · T-95 · T-97 · T-98 · T-99 · T-100 · T-101 · T-102 · T-103 · T-118 · T-123 · T-126 · T-127 · T-133 · T-134 · T-136 · T-137 · T-141 · T-154 · T-161 · T-172

## 3. 值得整体看的几处（我在修 U-0277 时看到的模式，非结论）

1. **"通知"与"状态变更"的耦合**。U-0277 之前 Unsubscribe / RetireSubject 把 Leave 投递当撤订阅的前置条件；Subscribe 把快照投递当激活的前置条件（这个是对的：没有基线就不该有订阅）。建议 review 时把 coordinator 里每个"投递失败怎么办"的分支列一遍：`Subscribe`(:183，快照准入失败 :244)、`Unsubscribe`(:265，Leave 投递 :305-320)、`DistributeBatch` 的 abort、`FlushSubject` 的 gate skip，看每处的"失败后状态"是否都有人负责收敛。
2. **两层"原子批"语义不一致**。core 的 `AtomicBatchTransport.AdmitBatch` 与 `SlowConsumerFailBatch` 假定"要么全进要么全不进"；demo 的 `sceneLane.AdmitBatch` 实际是"前缀进、后缀不进"。core 侧对慢消费者只认 `ErrReliableBackpressure`+`AdmissionError{Session}`，"会话不存在"这类永久错误没有类型化的表达，于是应用层错误进不了 Evict 策略。W-2026-09-22-03 记了修法候选；review 可以决定契约该定在哪一层。
3. **没有"遗忘一个不可达观察者"的原语**。room / coordinator 都只能经"投递 Leave"退出；sink 的 `ReleaseSession` 没有调用者；`deadSessions` 只由 Evict 策略写。会话关闭钩子（U-0243）与推送失败懒清理（`dropAsync`）两条清理路径最终都汇到 `Leave`，而 `Leave` 每一步都要投递。U-0277 改的是投递失败不阻塞，更干净的形状可能是一个显式的 `EvictSubscriber`。
4. **重试没有形状**。coordinator 分发失败 → abort 保留脏位 → room 下一 tick 原样重试，无退避、无上限、无"这批已经失败 N 次"的可见性；幸存者每 tick 收重复帧（同版本再发一次，客户端合并语义吞掉了它）。`flushFailures` / `failedBatches` 只在 `Stats()`，`/readyz` 没有 room / scene 依赖项。
5. **可观测性断层**。RR-01 全程服务端零 WARN / ERROR：coordinator 与 room 的失败进计数器，demo 的 `applyChanges` / `Leave` 把错误记 Debug，`pushPlayer` 无会话不计指标，`slow dispatch` 是唯一 WARN。建议 review 定一条规矩：哪一类失败必须至少一条限频 WARN。
6. **profile / LOD 在 packer 处断掉**。`SubjectSyncState.Prepare(profiles)` 按 profile 各准备一份，但 demo packer 忽略 profile（mask 不透明，字段常量对包外不可见，ARCH-06 处理了词汇表）。band 切换于是只影响"订不订"，不影响"发哪些字段"。看 review 是否接受这个简化。
7. **首帧竞态是设计问题不是 bug**。`Scene.Join` 在 `enter_game` 应答前就 `applyChanges` 推首帧，客户端在应答后才装 `scene_watch`，无 handler 的 push 静默丢弃——首帧字段随机到不到，`scene_expect` 靠 move 增量兜底。要么客户端先装 capture 再 join，要么服务端把首快照挂到"客户端 ready"之后。
8. **端到端覆盖只有一条 16 机器人脚本**，且机器人跑完即断线——RR-01 就是这么被撞出来的。建议在 loadtest 里加一个"会话 churn"场景（一部分机器人中途断线、其余继续断言收帧），把 U-0277 的端到端承诺固化下来；目前它只在 promise test 层。
9. **durability gate 的静默推迟**。`FlushSubject` 对 `CommitLSN > watermark` 只计数不报错（"WAL group-commit 间隔远小于 tick"的假设）；水位若因 W-2026-09-22-02 那类问题停住，客户端会安静地停在旧状态。T-100 有排障行，但没有告警。

## 4. 建议的阅读顺序

1. `entity/subject_sync.go:334-489`（MarkDirty → Prepare/prepareLocked）→ `:618-790`（PreparedSubjectSync/Batch 的 Commit / AbortWithError，按 generation 清 mask）
2. `entitysync/subscription.go:183-340`（Subscribe / Unsubscribe / forgetClosing）→ `:361-500`（DistributeBatch）→ `:528-585`（FlushSubject 与 durability gate）
3. `room/room_broadcast.go:290-355`（信封 → 帧的分组排序）→ `:924-1030`（flushDirty / flushStateBatch）→ `:1133`（retryRetirement）→ `:487`（handleSlowConsumer）
4. `room/room_transport_sink.go:236-450`（AdmitRoomFrames + 慢消费者策略）→ `:526-600`（encodeFrame）
5. `nettransport/channel.go`（AdmitBatch / AdmissionError）
6. 生成工程：`internal/service/game/scene.go` 全文（≈520 行）、`game/scene/runtime/interest.go`
7. 复现材料：`docs/bug/REVIEW-2026-09-22.md` RR-01 一节（含判别实验）、`docs/bug/REPRO-2026-09-22.md` §6–§9、`docs/bugfix/RR-20260922-01.md`

> 2026-09-22 晚：§3 的 1–4、6 五条已由维护者拍板收进 [ARCH-10](../bugfix/ARCH-10-sync-manager.md)（SyncManager；room / AOI 降为 policy），§1 的分层表在第 1 批之后需要重画。
> 同日更晚 M-14：demo 的 InterestSystem / RelationSource 搬进 core `entitysync/policy`，`syncTopic` 改名 `syncNamespace`，会话 held/ready + `scene_ready`；§1.3 里 `interest.go.tmpl` / `relations.go.tmpl` 已删除。
> 2026-09-23：`room/` 包退场（ISyncBus 实现 → `syncbus/driver`，kit `RoomMod` → `SyncBusMod`，`ModRoom` → `ModSyncBus`），`policy.Room` → `Group`；wdsync 对照与两条可借优化见 [ARCH-11](../bugfix/ARCH-11-view-priority-and-shared-encoding.md)。
