# M-13：实体同步统一为 entitysync.Manager，room 广播器三件退场（ARCH-10 实施）

- 单元：M-13（架构重构，不占 U 编号，不计缺陷）· 来源 ARCH-10（维护者 2026-09-22 拍板，破坏性实施，不做兼容）
- 仓库：roost-core `entitysync`（重写）、`room`（只剩 ISyncBus）、`entity`（`PrepareTick`）、`demo` 模板、`codegen/internal/roost`
- 记录：[ARCH-10](ARCH-10-sync-manager.md) 是设计；本文是改了什么、删了什么、怎么证明

## 一句话

实体同步现在是一件事：`entitysync.Manager` 一个进程一个，拥有全部 subject 与会话；subject 自己持有订阅者表；每 tick 给每个会话**一帧**；
room / AOI 只是决定谁订谁的政策。coordinator、RoomBroadcaster、RoomEnvelopeSink、RoomTransportSink、RoomManager 全部删除。

## 删了什么

| 删除 | 行 | 去处 |
| --- | --- | --- |
| `entitysync/subscription.go`（SubscriptionCoordinator） | 725 | 订阅表进 `entitysync/subject.go`，分发与门槛进 `entitysync/manager.go` |
| `room/room_broadcast.go`（RoomBroadcaster + RoomEnvelopeSink） | 1265 | 脏集 / tick / 退役进 Manager；分组排序不再需要（一会话一帧） |
| `room/room_transport_sink.go`（RoomTransportSink） | 967 | ObjectRef 分配与编码进 `entitysync/session.go`；组件编解码进 `entitysync/wire.go`；慢消费者策略变成"推送失败即关会话" |
| `room/room_manager.go`（RoomManager） | 475 | 预算 / 健康检查进 Manager；多房间宿主不再存在——房间只是政策 |
| 以上四者的 12 个测试文件 | — | 承诺重新落在 `entitysync/manager_promises_test.go`（8 条）与 demo `scene_test.go.tmpl` |
| codegen `renderReplication` 的 `NewRoomSink` | — | `NewSyncTransport(async) (*entitysync.AsyncTransport, error)` |
| 协议 `EntitySyncPush.Datagram` | — | 删除；一帧一条可靠推送，datagram 分片通道不再用于实体同步 |

`room/` 保留 `jetstream_syncbus.go` / `nats_syncbus.go`（服务间 ISyncBus，与本线无关）及其测试。

## 新形状（对应 ARCH-10 的对象模型）

| 对象 | 文件 | 说明 |
| --- | --- | --- |
| `entity.SubjectSyncState.PrepareTick(deltaProfiles, snapshotProfiles)` | `entity/subject_sync.go` | 一次实体锁内同时捕获 delta（给在线者）与全量快照（给新订阅者）；subject 脏时版本前进、快照带新版本。`Prepare(profiles)` 成为它的薄包装；`PreparedSubjectSync.Snapshots()` / `BaseVersion()` 新增 |
| `entitysync.subject` | `entitysync/subject.go` | `state + subscribers map[SessionID]{profile, kind(需要快照/在线/正在离开), baseVersion} + retiring`。**订阅者表在 subject 里**，是唯一真相 |
| `entitysync.Manager` | `entitysync/manager.go` | `Register/Unregister`、`OpenSession/CloseSession`、`Subscribe/Unsubscribe`、`Flush/Start/Stop/Close`、`Stats/CheckHealth`、`SessionLost` 钩子。**没有反向索引**：会话关闭时遍历 subject 删条目（O(subjects)，只在关闭时） |
| `entitysync.session` | `entitysync/session.go` | 每会话：Epoch/Tick 时钟、ObjectRef 表；`encode` 把本 tick 的条目切成帧（按 MaxObjects 分片），**在副本上编码、准入成功才采纳**（重试的 tick 重发同样的帧） |
| `entitysync.Transport` | `entitysync/transport.go` | `Push(ctx, session, frame)`；`ErrRetryLater` = 传输整体不可用、tick 作废；其他错误 = 该会话关闭。`AsyncTransport` 适配 nettransport 的可靠通道 |
| 线格式 | `entitysync/wire.go` | `WireVersion = 2`：去掉外层 room 帧头，帧就是 statesync 帧，`Epoch/Tick` 是会话时钟，`RoomID` 恒为流常量 1；组件 = `EncodeSubjectUpdate` |

### 与 ARCH-10 设计的三处出入

1. **三批合成一批**。维护者要求破坏性实施，不保留 `RoomBroadcaster` API 过渡层。
2. **订阅者表放在 `entitysync.subject` 包装里，不放进 `entity.SubjectSyncState`**。entity 是最底层包，不该知道 session；"subject 私有订阅"由 Manager 的 subject 对象兑现，`SubjectSyncState` 仍只有内容。
3. **demo 的会话 = player id**，不是连接。接入层 `pushPlayer` 对一个 player 的所有连接扇出，多连接由它处理；一连接一 SessionSink 的形状留给有真实会话 id 的部署（`SessionID` 只要求稳定、唯一）。

## tick 的顺序（`Manager.Flush`）

取 pending（脏 + 订阅变更 + 退役）→ 对每个 subject 在其锁内 `PrepareTick`（每个不同 profile 一次 pack；门槛按 `CommitLSN` 整体判定，被挡的整 subject 保留脏位与待发快照）
→ 按会话聚合成帧条目（快照 = create / 已持有则 update-full，delta = update，离开 = remove）→ 每会话编码到副本、逐帧 `Push`
→ `ErrRetryLater`：全部 abort、全部重新 pending、返回；其他推送错误：`loseSession`（关会话、遍历删订阅、`SessionLost`）；成功：采纳副本、结算订阅（快照/在线 → 在线 @version，离开 → 删）
→ 批量 Commit 所有 prepared（脏位按代际清）→ 退役完成的 subject 遗忘 → 被挡的重新 pending。

## 证明

- `go test -race ./entitysync`：8 条承诺——快照后 delta 且同 profile 一次 pack、一个会话失败只关它自己、`ErrRetryLater` 不怪任何会话、退订 / 退役只对持有者欠 remove、关会话不出帧、门槛挡整个 subject、换 profile 重发全量、一会话一帧且超对象上限关会话。
- `go test ./entity ./room ./codegen/internal/roost`：绿；core 全仓 `go test ./... -count=1`：115 包 ok。
- 渲染工程（rvArch，`replace` 本地 core）：`go vet ./...` 干净、`go test ./...` 全绿（含 `scene_test.go` 的 8 条：复制、退役、会话关闭、id 空间、被拒 subscribe 重试、不可达玩家不饿死他人、传输不可用不踢人、presence）。
- 端到端（rvArch，`replace` 本地 core，冷进程 16 机器人，`scene_expect` 超时 40s，debug 日志，观察者 × subject 探针；证据目录 `<scratch>/expG/`）：

```text
run done state=finished started=16 success=16 failure=0 elapsed=11.6s
首个断线 19:15:35.035（100001 / 100002 / 100004 / 100005 四个先跑完的）；仍在线的 12 个观察者之后各收到 13–14 帧
FRAME 2710 行，重复 (obs, subj, ver) = 0（RR-20260922-01 那轮幸存者各 21 次重复帧；这条路径已不存在）
scene: 日志只有 1 × "player unreachable, leaving the scene"；没有任何 unsubscribe / retire 失败日志（那条路径已不存在）；WARN 仍只有 slow dispatch
```

## 已生成工程怎么迁

`roost project upgrade` 重生成 `internal/service/game/scene.go`、`scene_test.go`、`cmd/loadtest/main.go`、`protocol/def/entity_sync.go`（`Datagram` 字段删除，需重新生成 pb）；
自写的场景代码：`room.NewRoomManager/NewRoomBroadcaster/NewRoomTransportSink` → `entitysync.NewManager(ManagerConfig{Transport, Interval, DurableWatermark, SessionLost})`，
`room.Subscribe(observerRef, subject, profile)` → `manager.Subscribe(session, subject, profile)`（先 `OpenSession`），`RetireSubject + UnregisterSubject` → `Unregister`，
客户端解码 `room.DecodeRoomWireFrame` → `entitysync.DecodeFrame`（无外层帧头、无 datagram 重组）。

## 边界 / 未做

- 快照与 remove 随 tick 走（默认 50ms），不再在 Subscribe / Leave 时同步投递。demo 的首帧竞态（客户端在 `enter_game` 应答后才装 `scene_watch`）因此**变窄**而非消失：快照在应答之后至多一个 tick 到达；ARCH-10"会话 ready 之后再开始"的钩子未做。
- 每房不同的复制周期、IdleTTL、多房间预算不再存在——全局一个 tick、一份容量。需要按区域限频的部署用 profile 的 LOD。
- datagram 分片通道从实体同步路径移除；`statesync.Reassembler` 与 nettransport 的 datagram 能力保留给别的用途（lockstep）。
- `syncTopic` / `Namespace` 现在随每个组件下发且是客户端唯一的分流依据（`RoomID` 已是常量）；标记文档改写留给下一轮。
- 未发版；生成工程钉的 core v1.16.1 与本仓不兼容，发 core 与 codegen 之前不要 `roost project upgrade`。
