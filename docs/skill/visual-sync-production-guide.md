# Skill Visual 与数据同步生产指南

本文描述当前 `roost-skill`、`roost-core`、`roost-kit` 三仓实现的正式边界、接入顺序、
恢复语义、运行指标和发布门槛。它既是学习入口，也是生产接入检查表。

## 1. 最终能力边界

```text
Skill JSON
   │ Parse / Compile / Lower
   ▼
immutable Program ── InspectPresentationPlan ── VisualAssetResolver ── renderer assets
   │
   ▼
Runtime ── StateSnapshot / StateDeltas / PresentationSnapshot / PollPresentation
   │
   ▼
skillsync.Coordinator ── roost-core/syncstream.History ── roost-kit/syncstream
   │                                                        │
   └──────── observer visibility policy                     └── NATS / JetStream
                                                            │
client skillsync.Applier ◀───────────────────────────────────┘
```

三个仓库各自只负责一层：

| 仓库 | 正式职责 | 明确不负责 |
| --- | --- | --- |
| `roost-core` | 通用有序流、ACK、有限历史、全量恢复、持久化表示、指标 | Skill payload、NATS、可见性规则 |
| `roost-skill` | DSL/Runtime、视觉契约、状态快照、强类型记录、服务端协调器、客户端应用器 | 引擎资源路径、具体消息中间件 |
| `roost-kit` | Packet 与 SyncMsg 的严格适配、observer 防串流、负载上限、有界发布队列 | 解释 Skill 状态或视觉语义 |

具体游戏负责实现生产 `Host`、observer 可见性策略、视觉资产目录解析器、History
持久化后端，以及客户端 consumer。`MemoryHost` 仅是确定性参考实现。

## 2. Visual 完整链路

### 2.1 定义与编译

Visual 可以挂在三个位置：

- `presentation.cast`：施法表现；
- `effect.visual`：Host 成功提交后的效果表现；
- `process.visual`：process start/update/signal/stop 生命周期表现。

定义中只允许 `category`、`theme`、`elements`，不允许 prefab、bundle、URL 或本地路径。
编译器用 `CompileEnvironment.Visual` 验证：

1. category/theme 必须存在；
2. 挂载种类必须在 `AllowedEffects` 中；
3. element 数量、内容、重复项和字节数必须符合目录；
4. 引用总数和去重后的 manifest 数量都必须在预算内；
5. 相同视觉三元组会被 intern，但每个 mount 仍计入引用预算。

`InspectPresentationPlan` 输出不可变 Program 的脱离副本：Manifest 带
`CatalogRevision`、`CatalogDigest`、自身 Digest；mount 分为 Cast、Effects、Processes。

### 2.2 客户端资源解析

客户端实现 `skill.VisualAssetResolver`：

```go
type VisualAssetResolver interface {
    CatalogIdentity() (revision string, digest string)
    Resolve(skill.VisualView) (skill.VisualAsset, error)
}
```

调用 `ResolvePresentationPlan` 时会先验证目录 revision 和 digest，再要求每个 manifest
entry 都解析到非空 asset key，最后验证所有 mount 的 visual index。任一失败都会整体
失败，不会返回半套资源。部署时必须把目录文件和客户端资源包作为同一个发布单元。

### 2.3 Runtime 事件时机

- Cast visual：cast commit 后产生；
- Effect visual：Host command 成功并返回 receipt 后产生，预期失败不产生；
- Process start：权威 process 创建完成后产生；
- Process update/signal：每次 Host step 返回后按规范化 signal 顺序产生；
- Process stop：Host stop receipt 成功后产生。

每个事件包含 Runtime 内部 Sequence、Tick、WorldRevision、Program digest、Cast/Process
标识以及真实 Source/Target/Position/Direction/Path anchor。`PollPresentation` 显式返回
`CursorExpired` 和 `More`；过期后必须用 presentation reset/state snapshot 恢复，不能
假定漏掉的动画仍可重放。

## 3. 权威状态同步

`Runtime.StateSnapshot()` 是 skill-owned 全量状态，包括：

- casts 与 cast-window 状态；
- cooldown 和 ammo/recharge；
- abilities、disable overlays；
- processes、motion、numeric tracks；
- active cast policies；
- Host 可选提供的 persistent/shared state；
- 最新 state-event 和 presentation sequence。

复制协议使用 `Runtime.StateDeltas(after, limit)` 返回的封闭 `StateMutation` union。
cast、cooldown、resource、ability、process、policy、persistent state 均有明确的
upsert/remove 变体；`ApplyStateMutation` 折叠后的规范 JSON 必须与新快照完全一致。
`CursorExpired=true` 时必须发送 full snapshot。原始 `StateEvents` 只用于诊断。

这份快照不替代通用世界同步。角色坐标、血量等完整世界模型仍应由游戏自己的实体
同步 topic 负责；Skill topic 只同步技能执行和拥有的状态。

## 4. 服务端 Coordinator 接入

构造时必须显式传入 Runtime、History、Publisher、Projector 和 VisibilityPolicy。
nil visibility 会被拒绝；确实允许全量可见时也必须写出 `AllowAllVisibility{}`。

```go
projector, _ := skillsync.NewProjector(1)
coordinator, err := skillsync.NewCoordinator(skillsync.CoordinatorOptions{
    Runtime: runtime,
    History: history,
    Publisher: publisher,
    Projector: projector,
    Visibility: matchVisibility,
    MaxPacketsPerFlush: 256,
})
```

推荐调用顺序：

1. 编译并注册 Program：`RegisterProgram(key, program)`；
2. 新 observer 加入时发布 manifest full 和 state snapshot；
3. 每个逻辑 tick 提交 Runtime 后调用 `Flush(observer, key)`；
4. 客户端处理成功后提交 `Epoch + Packet.Sequence` ACK；
5. 重连提交 ResyncRequest，调用 `Recover`；
6. 定期持久化 `History.Export()`，重启时在接收流量前 `Import()`。

Coordinator 对每个 observer/key 保存独立的 Runtime source cursor。可见性拒绝的事件只
推进该 observer 的 source cursor，不会进入其 History。Packet 一旦 Append 成功，即使
Publisher 随后失败也仍可由 Resync 重放；如果 Append 失败，source cursor 不前进。

## 5. History 恢复与持久化

`syncstream.History` 为每个 `Observer + Stream` 独立排序。核心不变量：

- delta 的 `BaseSequence` 必须指向前一网络包；
- full 的 `BaseSequence` 必须为 0；
- schema 变化只能由 full 开新链；
- ACK 单调且不能超过 Latest；
- payload、stream 数量、每流保留包数均可配置上限。

`Recover(request, provider)` 先尝试连续 replay；遇到 missing、gap、schema mismatch 或
client ahead 时，自动调用 provider 生成 full、Append 成新恢复锚点并返回。provider
不会在 History 锁内执行。

持久化使用 `HistorySnapshot` 或 `HistoryStore`：

- `Export` 返回 payload 脱离、stream 稳定排序的快照；
- `Import` 先验证全部 stream/packet identity、序列链、schema transition、ACK 和限制；
- 只有整份快照合法才原子替换内存状态；
- 导入窗口超过当前限制时保留最新尾部并累计 Dropped。

生产存储必须用“写临时对象 → fsync/提交 → 原子切换版本”策略，不能直接覆盖唯一副本。
建议在 match/shard 生命周期边界保存，并在进程退出前再保存一次。

## 6. roost-kit 传输边界

同步发送使用 `roost-kit/syncstream` 的 `NewPublisherWithOptions` 设置：

- `ExpectedObserver`：防止服务端路由代码把另一 observer 的包发进当前通道；
- `MaxPayloadBytes`：在 JSON envelope 前拒绝超大业务 payload；
- `FromSID` 与错误回调。

订阅端使用 `SubscribeWithOptions` 或 `SubscribeForObserver`。它会：

1. 严格 JSON 解码并拒绝未知字段/尾随内容；
2. 比较内层 Packet 与外层 SyncMsg 的 topic/key/sequence；
3. 校验 expected observer；
4. 校验 envelope 和 payload 大小；
5. 给 handler 传递脱离副本。

高吞吐场景可在 Publisher 外包 `BufferedPublisher`。队列满时返回 `ErrBackpressure`，
不静默丢包；关闭时停止接收并排空已接收包。异步下游最终失败会进入 OnError 和指标，
客户端通过 History/Resync 修复。需要 broker 级确认与持久投递时选择 JetStream。

## 7. 客户端 Applier

`skillsync.Applier` 在调用任何 consumer 前验证：

- Observer 必须完全一致（Kind/ID/Session/Scope）；
- SchemaVersion 必须一致；
- delta sequence/base 必须连续，full 必须 base=0；
- topic 与 record kind/full 形态一致；
- state delta 必须含强类型 sequence/event kind；
- presentation event 使用的 PresentationDigest 必须已有 manifest。

网络重复包返回 `Duplicate=true`，不会重复调用 consumer。StateEvent.Sequence 和
PresentationEvent.Sequence 也各自去重，因此恢复链中重复投影不会重复应用业务变化。
Consumer 应在单次调用内原子应用；返回错误表示未提交，之后允许重试同一 Packet。

客户端上线顺序必须是 Manifest → State Full → Presentation。收到 gap 时不要尝试跳过；
发送 ResyncRequest。收到 PresentationReset 后清理无法证明仍有效的临时动画，再以权威
state 重建持续表现。

网络 Sequence 只在一个 History 生命周期内有意义。若服务端在没有成功恢复 History
的情况下启动，必须为 observer 分配新的 `Session`（客户端同时创建新的 Applier），
不能沿用旧 Session 却把序号重新从 1 开始；否则旧客户端会把新的 Full 当成重复包。

## 8. 安全、容量和可观测性

至少导出以下指标：

| 指标 | 来源 | 建议告警 |
| --- | --- | --- |
| retained/dropped/pending/streams | `History.Metrics` | dropped 持续增长；pending 长时间不回落 |
| oldest/latest/acked | `History.Status` | latest-acked 接近保留窗口 |
| published/publish_failures/filtered/recoveries | `Coordinator.Metrics` | failures 或 recoveries 突增 |
| published/failures | kit `Publisher.Metrics` | broker 错误连续出现 |
| queued/published/failures/backpressure | `BufferedPublisher.Metrics` | backpressure > 0；queued-published 持续扩大 |
| Runtime CursorExpired/Dropped | poll batch | 任一稳定出现 |

所有上限必须来自部署配置并有合理硬上限。不要按客户端输入动态扩大 history、payload、
Runtime event buffer 或 manifest budget。日志必须包含 observer scope、topic、key、网络
sequence、source sequence 和 resync reason，但不要记录完整敏感 payload。

## 9. 发布、兼容与回滚

发布顺序固定：

1. 发布含新 syncstream 的 `roost-core`；
2. `roost-skill` 升级到正式 core 版本并移除本地 replace，再发布；
3. `roost-kit` 升级 core 版本并移除本地 replace，再发布；
4. 游戏服务接入 Coordinator/visibility/history store；
5. 客户端先支持新 schema 和 manifest catalog，再启用服务端流量。

Schema 或视觉目录升级采用双版本窗口：先部署能读取新旧版本的客户端，再让服务端以 full
切换 schema/catalog。回滚时停止产生新 schema，恢复旧 producer；客户端收到 mismatch
会请求 full。绝不能在同一个 revision 标签下替换 visual catalog 内容，digest 会把这种
错误识别为不匹配。

## 10. 生产验证流程

以下命令在三个仓库分别执行，并设置 `GOWORK=off` 验证模块边界：

```powershell
# roost-core
go test ./syncstream -count=1
go vet ./syncstream
go test -race ./syncstream -count=1

# roost-skill
go test ./skill ./skillcompose ./skillsync -count=1
go vet ./skill ./skillcompose ./skillsync
go test -race ./skill ./skillcompose ./skillsync -count=1
go test ./skill -run TestAllFixturesParseCompileInspectAndRun -count=1
go test -run=^$ -fuzz=FuzzParseGeneratedNeverPanics -fuzztime=10s ./skill

# roost-kit
go test ./syncstream -count=1
go vet ./syncstream
go test -race ./syncstream -count=1
```

发布环境还必须做端到端故障注入：

1. 正常 Manifest → Full → Delta → Presentation → ACK；
2. 丢一个 delta，确认客户端拒绝 gap 并 replay；
3. 覆盖 history 窗口，确认自动 full；
4. 服务重启并 Import History，确认 ACK/Latest 连续；
5. 注入 publisher failure/backpressure，确认数据留在 History；
6. 交换两个 observer 的 packet，确认服务端和客户端均拒绝；
7. 修改 schema/catalog digest，确认 full 或整体资源解析失败；
8. 超大 payload、非法 JSON、内外 envelope 不一致均被拒绝；
9. process visual 完整出现 start/update/signal/stop；
10. race 测试和 30 分钟压力测试中无竞态、goroutine 泄漏和 pending 单调增长。

上线门槛：普通测试、fixture、vet、race 全绿；故障矩阵全绿；指标和告警已接入；History
持久化恢复演练成功；客户端能够处理 full/reset；不存在 nil visibility 或无限队列配置。

## 11. 当前稳定版生产闭环补充

本节覆盖旧章节中基于“定期 Export/Import、发布后等待 Resync”的保守描述。当前正式
可靠链路如下：

1. `History.Append` 在修改内存前把 mutation 写入 `HistoryJournal`；Packet 自动携带随机
   非零 Epoch。ACK、删除、空闲清理和 Epoch 轮换也遵守 write-ahead 顺序。
2. `FileHistoryJournal` 使用不可变的分代 checkpoint/WAL。它先 fsync 下一代空 WAL，再
   原子发布唯一名称的 checkpoint；恢复只组合相同 generation。最新代损坏会阻止启动，
   上一代仅供运维显式恢复，协议不会静默回退并丢失已确认进度。
3. `Outbox` 在 broker 接受后仍保留 Packet，直到客户端 ACK。`FileOutboxStore` 提供进程
   重启恢复；Coordinator 构造时执行一次 crash reconciliation，运维显式修复调用
   `ReconcilePending`。正常 `RetryPending` 只扫描有界 pending，不重复全量 Export History。
4. 建议启用 `PruneAcknowledged`，在 ACK durable 后释放历史 Packet；`Pruned` 与容量不足
   导致的 `Dropped` 分开统计。observer 离开调用 `CloseObserver`，定期调用 `SweepIdle`。
5. 生产 Publisher 设置 `RequireConfirmation=true`、`CompressionThreshold`、
   `MaxFrameBytes`。订阅端限制 envelope/chunk/assembly/decoded 大小和 TTL，并强制 checksum。
6. 新 Epoch 的第一包必须为 full。Schema 用 `SupportedSchema` 协商，跨版本必须配置
   `SchemaMigrator`；不能把未知 schema 当成当前结构直接解码。
7. 客户端优先实现 `TransactionalManifestConsumer`、`TransactionalStateConsumer` 和
   `TransactionalPresentationConsumer`。Prepare 只准备，Commit 原子生效，Rollback 幂等；
   Commit 失败时 Applier 不推进 Epoch、网络 sequence 或 source sequence。
8. Visual 生产加载使用 `TrustedVisualCatalogs + VisualPlanCache`。目录 revision/digest
   不在信任集合时，在任何资源加载之前拒绝；缓存负责并行 preload、fallback、引用计数、
   unload、空闲淘汰和 catalog 失效。重连用 `PresentationSnapshot` 恢复仍持续的 cast/process
   表现，瞬时 effect 不伪造重放。

### 11.1 生产构造基线

```go
journal, _ := syncstream.NewFileHistoryJournal(historyDir, 0)
history, _ := syncstream.NewHistoryWithJournal(syncstream.HistoryOptions{
    SchemaVersion: 1,
    MaxPacketsPerStream: 1024,
    MaxPayloadBytes: 1 << 20,
    MaxStreams: 100000,
    IdleTTL: 30 * time.Minute,
    PruneAcknowledged: true,
}, journal)

store, _ := skillsync.NewFileOutboxStoreWithOptions(outboxDir, skillsync.FileOutboxOptions{
    MaxRecords: 100000, MaxRecordBytes: 2 << 20,
})
outbox, _ := skillsync.NewOutbox(skillsync.OutboxOptions{
    Store: store, RequireDurable: true,
    MaxPublishBatch: 512,
})

publisher, _ := syncstream.NewPublisherWithOptions(bus, syncstream.PublisherOptions{
    RequireConfirmation: true,
    CompressionThreshold: 4 << 10,
    MaxFrameBytes: 256 << 10,
})
```

必须为 `History.Health`、`Coordinator.Health`（包含 Outbox）和 kit Publisher/Subscriber
指标接入告警。重点观察：history dropped、outbox pending age/失败、assembly 超限、checksum
失败、visibility failure、schema/epoch mismatch 和 snapshot recovery 突增。

### 11.2 完整测试入口

```powershell
# 三个发布模块：在本地临时 go.work 中组合，生产 go.mod 不含 replace
go test ./... -count=1
go vet ./...
go test -race ./... -count=1

# 基准
go test ./syncstream -run '^$' -bench . -benchmem       # roost-core / roost-kit
go test ./skill -run '^$' -bench . -benchmem          # roost-skill

# 跨模块：确认失败 -> 重启 -> WAL/outbox 恢复 -> gzip/分片/checksum -> ACK/裁剪
cd roost-skill/integration/sync-e2e
go test ./... -count=1

# 发布前 30 分钟 soak（可先用 5s 验证任务配置）
$env:ROOST_SYNC_SOAK='1'
$env:ROOST_SYNC_SOAK_DURATION='30m'
go test ./... -run TestProtocolSoak -count=1 -timeout 35m
```

发布顺序为 `roost-core`（当时 v1.10.0）→ `roost-skill` → `roost-kit`（当时 v1.10.0）→ 业务服务/客户端。
`roost-skill` 的模块路径是 `github.com/tjbdwanghaibo/roost-core/skill`。生产模块只依赖
语义版本；相对 `replace` 仅存在于 `integration/sync-e2e` 测试模块。正式发布 tag 前应先用
临时 workspace 执行三仓全量测试，然后在无 workspace 环境验证已发布版本可解析。

## 12. 当前生产基线补充

以下约束已经进入正式实现，接入方不应再在业务层用弱版本替代：

1. `VisualPlanCache` 在 plan 缓存之下维护全局 asset registry。相同 asset key 跨 plan 共享
   preload/refcount，descriptor 冲突直接失败；catalog invalidation 是全批 preflight 后提交。
2. `EntityVisibilityPolicy` 支持封闭的字段枚举、handle evaluator、默认拒绝、递归
   `RuntimeValue` 脱敏以及独立 spatial/opaque 控制。新增 Runtime 字段不会自动泄漏。
3. `OutboxOptions` 必须在生产显式设置 `MaxPendingPackets`、`MaxPendingBytes`、
   `MaxPendingPerStream` 和 `MaxPendingAge`。文件存储使用版本化 checksum envelope，损坏启动
   失败；健康检查同时比较 pending count/bytes/age。
4. `Coordinator.CloseObserver` 是 observer 生命周期栅栏。关闭后所有 key 均拒绝发布；只有
   完成新 session 初始化后才调用 `OpenObserver`。短命 key 不会永久占用 view lock registry。
5. Runtime 写 API 返回前已提交 canonical mutation；外部 Host 扩展状态变化后调用
   `CaptureExternalState`。flush 路径只消费，不承担全量 diff 成本。
6. 世界和 Runtime 分开持久化但必须成对恢复。世界恢复到 checkpoint 的
   `WorldRevision/AuthorityIdentity` 后，使用 `ProgramResolver` 调用 `RestoreRuntime`；任何
   version/checksum/program/host 不匹配都中止 shard 开放。恢复后先发布 state full 和
   presentation reset/snapshot，再允许 delta。
7. Schema 迁移统一注册在 `SchemaRegistry`，启动结束调用 `Seal`。CI 必须保存每条正式迁移链
   的 golden payload；不允许在运行中覆盖同一 `(from,to)` 边，也不允许缺路径时猜测结构。

推荐的 Outbox 基线示例：

```go
outbox, err := skillsync.NewOutbox(skillsync.OutboxOptions{
    Store:               store,
    RequireDurable:      true,
    MaxPendingPackets:   100_000,
    MaxPendingBytes:     256 << 20,
    MaxPendingPerStream: 4_096,
    MaxPendingAge:       24 * time.Hour,
    MaxPublishBatch:     512,
})
```

容量值必须由“最大断连时长 × 峰值每秒包数 × 平均/高分位 payload”推导，并留出恢复重放
余量；示例只是默认安全阀，不是所有部署的容量规划答案。

v2 不读取 v1 checkpoint、旧 packet-only outbox 或 v1 composition contract，也删除了旧的
Trace/Process/Status 兼容 API。生产升级必须按
[稳定包迁移手册](breaking-upgrade-skill-package.md) 更换 Go 导入路径；不能把网络 schema 的
双版本窗口误用于本地持久化格式。
