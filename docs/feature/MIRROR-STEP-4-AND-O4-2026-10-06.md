# RemotePolicy Mirror 第 4 步（可确认订阅、首载缓冲、兴趣代际）与 O4 兴趣容量（2026-10-06）

依据：维护者决定（第九轮）“Mirror 第 4 步”“O4 interest 容量”，两项都按推荐方案。
前置：[第 1～3 步](MIRROR-STEPS-1-3-2026-10-06.md)（`8495c5c4`，§7 是本步入口）、[B2](B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md)（L2 水位权威、唯一写入口 `admitLocked`、`cached_max_staleness`）、[方案](../review/PLAN-REMOTE-POLICY-MIRROR.md)六步表第 4 行、[N05 审查 O4](../review/REVIEW-2026-10-05-n05-revn05.md)。
范围：`sync/syncbus`（可确认订阅能力）、`sync/syncbus/driver`（JetStream 实现）、`sync/syncbus/mirror`（Replicator 用它）、`entity/remote_snapshot.go`（首载缓冲，仍经 `admitLocked`）、`remoteentity`（SnapshotClient 推送模式 / 退化、兴趣代际与容量）、`kit/remoteentity` 与 `app/config_validation.go`（新配置键）。Remote 权限（K3）不动。

## 1. 目标

1. **可确认的推送订阅**：总线能证明“订阅确认之后发布的消息不会被静默丢掉”时才开推送；JetStream 按主题用 DeliverNew 的 durable 消费者提供这项保证。没有这项能力（普通 NATS）时显式退化为按需读取：不订阅快照推送，启动时记 Warn、`Stats().PushEnabled=false`、指标置 0；Cached 读按 `cached_max_staleness` 回源（B2 已有），正确性不依赖推送。
2. **首载缓冲**：一个 key 的权威加载在途时，它的复制消息先进有界缓冲；加载结果经 `admitLocked` 装入后，缓冲按到达顺序重放，仍经 `admitLocked`（版本 / epoch / 删除标记决定取舍）。溢出：丢弃缓冲、计数并记日志，加载装入后再回源一次（整体回源）。
3. **兴趣代际全交错**：renew / release 的 generation 在同一 key 的条带锁内分配；owner 的兴趣表对 release 留一个有界的撤销水位，比它旧的 renew 迟到后不能把租约复活。
4. **O4**：兴趣表按 consumer 计数，每个 consumer 节点各有配额（`remote_entity.snapshot_interest_per_consumer`，缺省 `snapshot_interest_subs / 16`）；`snapshot_interest_subs` 只作每节点内存上限。满了返回可识别的错误（`ErrInterestQuotaExceeded` / `ErrInterestRegistryFull`，都包裹 `entity.ErrRemoteOverloaded`），按原因计入指标并限频记日志；consumer 本机的兴趣表收到自己全部的续租，按同一配额判定，所以能在本地得到同样的拒绝，计入 `Stats().InterestRejected`，这个 key 不再有推送刷新确认时刻，读取在陈旧上限后回源（按需读取）。

## 2. 与方案原文的差别（B2 之后）

方案（09-13）写于 L2 水位之前，设想“水位与订阅代际绑定、淘汰水位时 key 退出 Ready、消息入口拒绝未准入 key”。B2 之后共享 L2 是水位权威，每次 L1 写都先过 L2 的版本 CAS / 带版本删除，非线性读只交出陈旧上限内被确认过的条目，所以：

- 旧删除 / 旧 upsert / 重建、L1 淘汰后的旧消息：由 L2 CAS 与墓碑、B2 的发布时刻过滤挡住，不再需要“Ready 门”；本步把 DeliverAll 的历史重放从源头关掉（DeliverNew），并用首载缓冲解决加载在途时的顺序问题（增量消息不再因缺基触发第二次权威加载）。
- 订阅断开 / 重连之间漏掉的推送只影响新鲜度，上界仍是 `cached_max_staleness`；JetStream durable 断开后按游标续投，不漏。
- 没有 L2 的装配（L1 自己是水位）只适合单进程或测试（第 3 步已写明），L1 删除标记被 LRU 淘汰后迟到的旧消息仍可能复活，本步不处理。

## 3. 改动面

| 位置 | 变化 |
| --- | --- |
| `sync/syncbus/sync.go` | 新能力接口 `ILiveSubscriber.SubscribeLive` |
| `sync/syncbus/driver/jetstream.go` | 实现 `SubscribeLive`：DeliverNew，durable 名与普通订阅分开（`<topic>.live`，避免在旧 DeliverAll durable 上改策略被服务端拒绝）；普通 `Subscribe` 不变 |
| `sync/syncbus/mirror/envelope.go` | `NewLive`：用 `SubscribeLive` 的复制器，总线没有这项能力时 `Start` 返回 `ErrLiveSubscribeUnsupported` |
| `entity/remote_snapshot.go` | `ApplyReplica`（复制消息唯一入口，在途加载时进缓冲）；`loadAuthoritative` 登记 / 结束首载、重放、溢出时再回源一次；`RemoteSnapshotCacheConfig.ReplicaBuffer`（缺省 64）；`BootstrapStats` |
| `remoteentity/snapshot_client.go`、`syncer.go`、`interest.go`、`config.go`、`assembly.go` | `Start` 按总线能力选推送 / 退化；复制消息经 `cache.ApplyReplica`；generation 锁内分配；撤销水位；按 consumer 配额；错误、指标、日志、Stats |
| `kit/remoteentity`、`app/config_validation.go` | `snapshot_interest_per_consumer` 严格读取并登记；配额大于总上限启动即拒绝；健康信息带推送模式与拒绝数 |

所有缓存写入仍只经 `admitLocked`：缓冲重放走 `Publish` / `DeleteAtVersion` / 增量合成，溢出回源走 `loadAuthoritative`。停机沿用第 3 步的 A3 骨架（缓冲只存在于在途加载里，加载受客户端 `work` 准入约束）。

## 4. 兼容

- 公开 API 只增不删；wire、L2 键、Lua、生成形状不变；`Manager.BindSync` 的旧用法照旧用普通订阅（调用方自管）。
- **行为变化**：
  - `Assembly.Start` / `SnapshotClient.Start` 在没有 `SubscribeLive` 的总线（kit 缺省的普通 NATS）上不再订阅快照推送：Cached 读在陈旧上限（缺省 30s）后经 L2 / 权威重新确认，之前推送能更早刷新。启动日志与 `Stats().PushEnabled` 明示。
  - JetStream 上快照主题改用新的 DeliverNew durable（服务端名字 `sync_remote_entity_snapshot_live_<sid>_<16 位十六进制>`；2026-10-06 更正：原写 `sync_remote_entity_snapshot.live_<sid>_…`，`durableSyncName` 经 `sanitizeSyncName` 把 `.` 换成 `_`，已在隔离 NATS 上列消费者核对，用例 `TestRealJetStreamLiveDurableNameShape`），旧的 DeliverAll durable（`sync_remote_entity_snapshot_<sid>_<16 位十六进制>`）不再被消费，可由运维删除。
  - 兴趣表按 consumer 配额（缺省总上限的 1/16：core `DefaultConfig` 总上限 262144，即 16384；生成工程配置模板 `snapshot_interest_subs: 100000`，即 6250）：单个 consumer 超过配额的 key 不再有推送，按需读取；不再因为别的 consumer 占满全表而被拒。兴趣表不再单独限制 key 数（`snapshot_interest_keys` 仍是 consumer 本机表上限）。
- 配置：新键 `remote_entity.snapshot_interest_per_consumer`（int，0 = 总上限 / 16，必须 ≤ `snapshot_interest_subs`）。

## 5. 验证

- 先红后绿：首载期间到达的增量让权威加载两次（红）→ 一次并装上增量（绿）；release 之后迟到的旧 renew 复活租约（红）→ 不复活（绿）；O4：一个 consumer 占满全表后另一个 consumer 的兴趣在 owner 处被拒、读路径静默吞掉（红）→ 各有配额、拒绝可识别有计数、读取照常回源（绿）。
- 行为矩阵：首载缓冲下旧删除 / 旧 upsert / 删除后重建 / 更新的删除；溢出；renew-release 全排列；退化检测；JetStream 真实环境上的确认订阅、不重放历史、退订后重订续投。
- `TestRealB2WatermarkMatrix*`、`scripts/test-remote-generated.sh` 照样通过；`go test -race -count=3` 改动包；glsvet；根包。

## 6. 实施记录（基线 `2a7d2a65`，分支 `mirror4`）

图谱 `Users-whb-roost-roost-core` 的 generation 停在 2026-09-30，本轮涉及的文件（`snapshot_client.go` 未跟踪，其余 `metadata_changed`）全部按当前源码读取。

### 6.1 实际实现

- **可确认订阅**：`fsyncbus.ILiveSubscriber`；JetStream `SubscribeLive` 建 DeliverNew durable（名 `durableSyncName(prefix, topic+".live", sid)`），与同主题的 DeliverAll durable、本地 fanout 都分开；`mirror.NewLive` / `Replicator.Live()`，总线不支持时 `Start` 返回 `mirror.ErrLiveSubscribeUnsupported`。
- **推送 / 退化**：`SnapshotClient.Start` 按 `bus.(fsyncbus.ILiveSubscriber)` 选择；支持时快照复制器用 `NewLive`；不支持时不订阅快照主题（发布用的复制器照建，供 owner 发布），Warn `snapshot push disabled …`、指标 `remote_entity.snapshot_push_enabled{sid}` = 0、`Stats().PushEnabled=false`（`Manager.Stats().SnapshotPush`，kit 健康信息 `snapshot_push=`）。兴趣主题照旧订阅。`Manager.BindSync` 的旧用法不变（普通订阅，调用方自管）。
- **首载缓冲**（`entity/remote_snapshot.go`）：`loadAuthoritative` 在取得加载名额后 `beginBootstrap`，`fetchAndAdmit`（原加载 + 准入逻辑原样搬入）之后 `endBootstrap`；复制消息经 `ApplyReplica` 进入，key 在首载时进缓冲（`ReplicaBuffer`，缺省 64，remoteentity `Config.SnapshotReplicaBuffer`），最后一个加载结束时取走并按到达顺序重放（`applyReplica` → `ApplyUpdate` / `DeleteAtVersion` / `Delete`，都经 `admitLocked`）；溢出：清空缓冲、标记，之后到达的也丢，加载结束后 `fetchAndAdmit` 再回源一次，Warn + `remote_entity.snapshot_bootstrap_overflow_total`；重放失败只计数（`…_replay_failed_total`）。`BootstrapStats()` / `SnapshotClientStats.Bootstrap`。`SnapshotReplicaStore` 改走 `ApplyReplica`，不在首载时的缺基仍由它回源（那次回源本身也是首载）。
- **兴趣代际**：`RenewInterest` / `ReleaseInterest` 在条带锁内分配 generation；兴趣表的 `release(g>0)` 留撤销水位（`released` 条目，存活 `SnapshotInterestTTL`，计入每节点条目、不计入配额），之前发出的 renew 迟到后被忽略；generation 0（旧发布者）不留水位；本机回滚 / 清理用 `drop`（不留水位）。表满、被撤销的条目又不存在（续租还在路上或曾被拒）时，原实现只撤销不留水位，表里随后空出一格就会让迟到的旧续租复活租约（[RR-20261006-11](../bug/RR-20261006-11.md)）；现在改记到每个 consumer 一个的溢出水位（代际上限 + key 指纹位图，同样存活 `SnapshotInterestTTL`，不占表容量）。
- **O4**：`remoteInterestRegistry` 按 consumer 计（`perConsumer`），配额 `Config.SnapshotInterestPerConsumer`（kit `remote_entity.snapshot_interest_per_consumer`，0 = `SnapshotInterestSubs / 16`），`SnapshotInterestSubs` 是每节点条目上限；去掉全表 key 数上限（`snapshot_interest_keys` 仍是 consumer 本机表上限）。拒绝：`ErrInterestQuotaExceeded` / `ErrInterestRegistryFull`（都 `errors.Is` `entity.ErrRemoteOverloaded`），指标 `remote_entity.remote.interest_rejected_total{reason=consumer_quota|registry_full}`，Warn 每表每 10 秒至多一条；consumer 侧 `remote_entity.remote.interest_renew_refused_total{reason}`、`Stats().InterestRejected`（`Manager.Stats().InterestRefused`，健康信息 `interest_refused=`）。配额大于总上限：`NewSnapshotClient` 与 kit `Init` 拒绝；键登记进 `app` 的 `frameworkIntKeys`（A4 严格读取，`ValidateServiceConfig` 一并检查类型）。

### 6.2 先红后绿

修前（基线 `2a7d2a65` 的实现 + 本轮新用例）：

```text
--- FAIL: TestSnapshotBootstrapBuffersDeltaDuringFirstLoad
    after the first load: version=5 payload="base" found=true err=<nil> authority loads=2 replica err=remote snapshot stale; want v6 "base+delta" from one load (the delta buffered and replayed on top of the load)
--- FAIL: TestInterestRenewReleaseConvergesInEveryDeliveryOrder
    renew then release delivered in order [1 0]: interested=true, want false (the last operation issued decides)
    renew renew release delivered in order [2 1 0] / [1 2 0] / [2 0 1] / [0 2 1]: interested=true, want false
    release renew release delivered in order [2 1 0] / [2 0 1] / [0 2 1]: interested=true, want false
--- FAIL: TestInterestCapacityIsPerConsumer
    consumer B's interest was refused at the owner because consumer A filled the cluster-wide table; capacity must be counted per consumer
```

第一条：增量在加载在途时到达，复制接收缺基、直接回源（不合并）且要求 v6 而权威还给 v5，返回 `ErrRemoteSnapshotStale`，增量丢失、权威读两次。第三条修前 B 的读路径 `_ = c.RenewInterest(...)` 吞掉错误，没有计数。

修后三条都通过。负对照（改完即恢复）：去掉首载缓冲（`bufferDuringBootstrap` 不生效）→ 第一条与 `TestSnapshotBootstrapReplayMatrix` 的计数断言红；去掉溢出后的再回源 → `TestSnapshotBootstrapOverflowDropsTheBufferAndReloads` 红（`version=5 loads=1 … Reloads:0`）。

新增的行为矩阵（全部通过）：`TestSnapshotBootstrapReplayMatrix`（旧 upsert / 旧删除 / 同版本删除 / 更新的删除 / 删除后重建 / 重建先于删除到达 / 新旧 upsert 乱序，权威只读一次）、`TestSnapshotBootstrapOverflowDropsTheBufferAndReloads`、`TestInterestQuotaRefusalIsVisibleAndReadsGoOnDemand`（配额内有兴趣、超出的 `ErrInterestQuotaExceeded`、计数、读取经权威）、`TestSnapshotClientWithoutConfirmedSubscriptionsReadsOnDemand`（不订阅快照主题、Warn、`PushEnabled=false`，经共享 L2 读到 owner 的 v1 / v2；能确认订阅的总线上推送开着）、`sync/syncbus/driver` 的 `TestJetStreamSubscribeLiveUsesASeparateDeliverNewDurable`、kit 的 `TestInterestPerConsumerConfiguration`。

既有用例的调整（语义变化，不是放宽）：`TestStaleInterestReleaseDoesNotCancelANewerRenewal` 原把“release(13) 之后迟到的 renew(12) 重建租约”当前提，改为断言不复活、再用 renew(14) 建租约验证旧续租不倒退；`TestRemoteInterestRegistryHasHardCapacityLimits` 改为按 consumer 配额 + 每节点上限；四个 Assembly 生命周期用例的测试总线补 `SubscribeLive`（它们验证两个订阅的回收，推送开着才有两个）。

### 6.3 真实环境（隔离环境 `~/.roost-it/roost-dataengine-it`，`remote-acceptance.lock` 不存在）

- `TestRealJetStreamLiveSubscriptionConfirmsAndResumes`：确认前的历史（1）不投递；确认后的（2）投递；退订期间发布的（3）在重新订阅后投递——一次运行 2.05s（退订时被停掉的拉取请求已交出 3、未确认，AckWait 2s 后重投），另一次 0.05s（直接从游标续投）；PASS。
- `TestRealJetStreamLiveSnapshotPushReachesTheReader`（真实 JetStream + Redis L2）：owner 与只读方都开推送；只读方读续租兴趣 → owner 收到 → 提交 v2 → 只读方 Cached 读在陈旧上限内读到 v2（只能来自推送）；PASS。
- `ROOST_REMOTE_CLUSTER_IT=1`：`TestRealB2WatermarkMatrixStandalone`（4.04s）、`TestRealB2WatermarkMatrixCluster`（6.43s）、`TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect`、`TestRealSnapshotL2KeyPrefixOnRedis(Cluster)`、`TestRealSnapshotL2StaleWriteIsReportedAndNotPinnedInL1`、`TestRealSnapshotL2TombstoneFencesEveryWriter` 全部 PASS，矩阵未改。
- `scripts/test-remote-generated.sh`：12 条 `TestGeneratedRemote*` PASS（发送方的 `observedBus` 包装不暴露 `SubscribeLive`，走退化模式；接收方是 `BindSync` 旧用法，照旧收到推送）。

### 6.4 验证（`GOWORK=off`）

`gofmt -l` 空；`go build ./... && go vet ./...`；`go vet -tags integration ./remoteentity ./sync/...`；`go test -race -count=3 ./entity ./remoteentity ./sync/syncbus/... ./kit/remoteentity ./kit/syncbus ./cache` 全过；`go test -count=1 . ./nest ./app` 全过；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` 无违例。生成形状未变（未改模板、生成器与 catalog 配置模板），未跑 codegen 生成与 game-demo。

### 6.5 K3 / K4 边界

- remoteentity 权限（owner / grant / fence、Mongo 写权限、finalizer）没有改动。
- `sync/syncbus/driver` 的改动是新增方法：`Subscribe` 的语义与 durable 名逐字不变（只是 fanout 的 map 键换成变量），Sync 核心（`sync/entitysync`、`syncstream`）不调用 `SubscribeLive`，执行契约（setter 只标脏、锁外发送、交付前缀结算）不受影响；glsvet 已跑。

### 6.6 兼容影响（汇总）

见 §4。运维可见：普通 NATS 部署启动时多一条 Warn（推送关闭）；JetStream 部署的快照主题换 durable，旧的 `sync_remote_entity_snapshot_<sid>_…`（DeliverAll）不再消费，可删除；兴趣表满载时按 consumer 拒绝并计数。性能：首载缓冲只在权威加载在途时取一次互斥锁（每次复制消息一次 `bootMu`，短临界区）；本批未做压测（维护者要求节省额度）。

### 6.7 未完成 / 边界

- 无 L2 的装配里 L1 删除标记被 LRU 淘汰后迟到的旧 upsert 仍可能复活（只适合单进程或测试，第 3 步已写明），本步未处理。
- 兴趣表是广播副本，consumer 的本地判定与 owner 只在两边收到同一组消息时一致；总线丢兴趣消息时由租约过期收敛。`snapshot_interest_subs`（每节点上限）被打满时拒绝原因是全表的，consumer 只能从本地同样满的副本大致感知。
- 只读方读结果仍没有“确认时刻 / 年龄”字段（第 3 步留项）。

### 6.8 第 5 步入口（kit 装配、codegen 只读产物、公会摘要样例）

| 部分 | 入口 | 要点 |
| --- | --- | --- |
| kit 只读装配 | 新 Mod（建议 `kit/remoteentity` 同包，`RemoteMirrorMod`）：`remoteentity.NewSnapshotClient(cfg, SnapshotClientDeps{L2: NewSnapshotL2StoreWithKeyPrefix(redis, …), Loader, LinearizableLoader, ConsumerSID})` + `Start(bus)` / `Stop(ctx)` | 只读 `remote_entity.*` 的快照段（A4 严格读取，新键登记进 `frameworkIntKeys` / `frameworkDurationKeys`），不要求 Mongo 原子 backend；总线来自 `kit/syncbus`，JetStream 模式才有推送（`Stats().PushEnabled`），健康信息带 `snapshot_push` 与 `interest_refused`；停机预算登记（`StopBudget`）；Loader 由 owner 侧提供（Managed 用 `Backend.LoadRemoteSnapshot`，独立服务需要一个只读 Mongo loader） |
| codegen 只读产物 | `codegen/internal/entity` 的 parse / gen：`remote=mirror` 报迁移诊断（提示改用只读 DTO）；为 DTO 生成 `entity.RemoteMirrorSpec` 与解码函数，给 `entity.NewRemoteMirrorReader` 用 | 不生成持久化 / 提交参与能力；改生成形状要重生成 fixture、跑 `go test ./codegen/...` 与 game-demo |
| 公会摘要样例 | 两进程：owner（Managed 实体提交后发布）+ 只读服务（第 5 步的 Mod）；`TestSnapshotClientReadsWhatTheOwnerPublishesInTheSameProcess` 与本步的 `TestRealJetStreamLiveSnapshotPushReachesTheReader` 是单进程雏形 | 验收：生成后真实消费者编译；两进程读写分离；跨租户 / profile 拒绝；停止取消与重复关闭 |

### 6.9 后续（追加）

第 5 步已实施（2026-10-06，[记录](MIRROR-STEP-5-2026-10-06.md)）：kit `RemoteMirrorMod`、codegen `//roost:mirror` DTO、公会摘要两进程样例（生成工程 `testdata/remoteflow`）。第 6 步的外部条件见该记录 §6。
