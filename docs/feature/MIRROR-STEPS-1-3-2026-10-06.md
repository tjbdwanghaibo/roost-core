# RemotePolicy Mirror 第 1～3 步：只读契约、统一读出口、共享 snapshot client（2026-10-06）

依据：维护者第四轮决定“补齐 PLAN-REMOTE-POLICY-MIRROR 剩余实现”，本批做方案六步表的 1～3，4～6 留给下一批。
方案：[PLAN-REMOTE-POLICY-MIRROR](../review/PLAN-REMOTE-POLICY-MIRROR.md)；前置：[B2](B2-REMOTE-SNAPSHOT-L2-WATERMARK-2026-10-06.md)（L2 水位权威、唯一写入口 `admitLocked`、`cached_max_staleness`、DeliverAll 过老快照丢弃）。
范围：`entity`（只读契约、快照缓存的读出口）与 `remoteentity`（Manager 的快照读 / 兴趣 / 复制接收 / 生命周期提取为 `SnapshotClient`）。Remote 权限（owner / grant / fence、Mongo 写权限、finalizer）属核心线 K3，没有改动。

## 1. 目标

维护者原则：由底层统一兜住，结构简单易懂，使用方手写代码尽量少。

1. **只读契约**（第 1 步）：只读方拿到的唯一能力是“按完整 key 读不可变快照”（`entity.RemoteSnapshotReadOnly`）；读结果带观察 token；业务拿到的 DTO 由独立字节副本解码，改了也不污染缓存；只读方不注册 kind、不注册全局解码器、不持有写 backend，所以 owner 与 consumer 同身份、同进程不冲突。
2. **一个读出口**（第 2 步）：Cached / Monotonic / Linearizable / 直接加载 / 同版本刷新全部经快照缓存的 `Read`（或它的内部函数 `loadAuthoritative`），最低要求用同一个判定 `RemoteObservation.Covers`；Linearizable 只在 loader 声明线性化能力时开放。Manager 外层不再保留自己的回退加载。
3. **一份协议**（第 3 步）：`remoteentity.SnapshotClient` 拥有快照缓存、兴趣（本机续租表 + 全集群兴趣表）、复制接收（按 key apply）、权威回填、统计和生命周期；Manager 组合它、委托它。客户端不要求写 backend。

## 2. B2 之后还缺什么（核对结果，基线 `bc98f05c`）

图谱 `Users-whb-roost-roost-core` 的 generation 停在 2026-09-30，本轮涉及的 7 个文件全是 `metadata_changed`，以当前源码为准（`trace_path ReadRemoteSnapshot` 入边只有 nest 两处，与源码一致）。

| 方案第 2 步要求 | B2 之后的状态 | 本批 |
| --- | --- | --- |
| 过期（ExpiresAt）不出 | `serveAt` / `LoadAuthoritative` 前后两次检查已覆盖（RR-20260913-08 及复核） | 保留；读出口矩阵再钉一次 |
| 陈旧上限 | B2 `readConfirmed` / `refresh` 已覆盖 | 不重复 |
| 同版本刷新 | `publishLocked` 同版本只向后延长 ExpiresAt、补确认时刻（RR-08 复核 + B2） | 保留；矩阵逐出口验证 |
| Manager 外层回退 | **缺**：`ReadRemoteSnapshot` 在 `Get` 已合并回源之后，未命中 / 低于最低版本时再等 2ms、再直接 `LoadAuthoritative`——第二次不经合并、不受等待名额约束，一次 Monotonic 未命中回源两次 | 删除外层回退，唯一读出口 |
| Cached 的最低要求 | **缺**：`Get(Cached, minVersion)` 在 L1 有更低版本时照样交出（nest `RemoteAcquireCache` + `MinVersion` 就会拿到低于要求的快照） | 不满足返回 `ErrRemoteSnapshotStale` |
| 观察 token | **缺**：只有版本下限，换代（route / marker epoch 更新）后版本更小的快照被误判为旧 | `RemoteObservation` + `Covers`；带 epoch 的 token 不把版本下推给 loader |
| Linearizable 能力 | **缺**：任何 loader 都被当成线性化 | 客户端按 `LinearizableLoader` 开放；Manager 声明为 true（沿用） |
| 等待名额 / 身份 / schema 回归 | RR-20260913-04 / NC-35 / RR-20260913-07 已有 | 全部保留并通过；DTO reader 在读侧再校验身份与 schema / codec |

## 3. 实施

| 文件 | 变化 |
| --- | --- |
| `entity/remote_mirror.go`（新） | `RemoteObservation`、`Covers`（与 `remoteSnapshotStale` / L2 CAS 同一排序：更新 epoch 胜、同 epoch 比版本、混合不可比；两个 epoch 都为零的 token 只比版本）；`RemoteSnapshotRead`；`RemoteSnapshotReadOnly`；`RemoteMirrorSpec` / `RemoteMirrorReader[T]` / `RemoteMirrorValue[T]`；`ErrRemoteObservationIncomparable`、`ErrRemoteReadUnsupported` |
| `entity/remote_snapshot.go` | `Read` 是唯一读出口；`Get` / `LoadAuthoritative` 改为外观；`covers` 统一最低要求；合并键 `(key, After)`；`loaderMinVersion` 只下推只约束版本的 token |
| `remoteentity/snapshot_client.go`（新） | `SnapshotClient`、`NewSnapshotClient`（非法组合直接拒绝）、`ReadSnapshot` / `ReadRemoteSnapshot`、`RenewInterest` / `ReleaseInterest`、`Stats`、`Start(bus)` / `Stop(ctx)`；包内 `publishCommitted`（owner 提交后发布）；依赖准入 `gatedSnapshotL2` / `gatedLoader`（共用 `operation.Lifetime`） |
| `remoteentity/manager.go`、`transaction_manager.go`、`assembly.go`、`assemble.go`、`syncer.go`、`interest.go` | Manager 组合 `snapshots *SnapshotClient`；读 / 兴趣 / Stats / BindSync / SetSyncer 委托；`afterRemoteCommit` 的发布改为 `publishCommitted`；`remoteState` 删除缓存与兴趣字段；Assembly 的复制启停交给客户端；`SnapshotReplicaStore` / `InterestReplicaStore` 落到客户端 |

写入路径没有新增：客户端的全部缓存写入（复制、提交后发布、回填、删除）仍经 `RemoteSnapshotCache` 的 `Publish` / `DeleteAtVersion` / `LoadAuthoritative` → `admitLocked`（B2 的唯一入口）。

### 停机（套 A3 骨架）

`SnapshotClient.Stop(ctx)`：①读准入关闭（之后的读返回 `ErrSnapshotClientStopped`）、依赖准入关闭、取消在途权威加载、退掉两个复制订阅；②在 ctx 内等已准入的复制 handler（`Replicator.StopWithContext`）与依赖调用（`Lifetime.Wait`），超时返回 ctx 错误，重试再等；③返回 nil 之后调用方才释放 Redis / 权威 / 总线。客户端只有一次生命，Stop 之后 Start 报 `ErrSnapshotClientStopped`。`Assembly.Stop` 先停 finalizer，再停客户端。

启动：`Start(bus)` 先订快照主题，再订兴趣主题；后者失败时退掉前者再返回，重试不会重复订阅。Assembly 启动的后续步骤失败时退订（`unsubscribe`），客户端仍可重试启动。

### 迁移策略（第 1 步）

- 旧 `remote=mirror` 声明只是元数据（不订阅、不加载、不强制只读）；仓内没有使用方。新用法：owner 照旧（Managed 或普通权威 + 提交后发布），consumer 用 `entity.NewRemoteMirrorReader(source, spec, decode)` 读 DTO，`source` 是同进程 owner 的 `Manager.SnapshotClient()`，或独立服务的 `remoteentity.NewSnapshotClient(cfg, deps)`。
- 生成器对 `remote=mirror` 的诊断（报错并提示改用只读 DTO）与 kit 只读装配属于第 5 步。
- 默认配置：`NewSnapshotClient(nil, …)` 用 `DefaultConfig()` 的快照段；拒绝 ConsumerSID 为 0、声明线性化却没有 loader、兴趣 TTL / 加载超时非正、陈旧上限为负、带 L2 却没有 L2 TTL。

## 4. 先红后绿

### 修前红（基线实现上实际失败）

```text
# Manager 外层回退：一次 Monotonic 读回源两次
--- FAIL: TestReadRemoteSnapshotMonotonicMissLoadsAuthorityOnce
    one Monotonic miss loaded the authority 2 times, want 1 (the reader's fallback reloaded outside the coalesced load)
--- FAIL: TestReadRemoteSnapshotMonotonicBelowMinimumLoadsAuthorityOnce
    one Monotonic read below the minimum loaded the authority 2 times, want 1

# Cached 读交出低于最低版本的快照（基线 remote_snapshot.go 上用只走旧 API 的同场景用例）
--- FAIL: TestRedCachedReadBelowMinimum
    Cached read with minVersion=5 returned version=3 found=true err=<nil>; want ErrRemoteSnapshotStale, not a value below the minimum
```

同进程无冲突注册的修前事实（作为断言保留在 `TestRemoteMirrorReaderNeedsNoRegistrationBesideTheOwner`）：旧消费方式只能再注册一次全局解码器，必然得到 `ErrRemoteSnapshotDecoderDuplicate`。

### 新契约的负对照（去掉一条约束即红，之后恢复）

```text
N1 去掉启动失败时退订快照主题
--- FAIL: TestSnapshotClientStartFailureLeavesNoSubscription
    a failed Start left 1 snapshot subscription(s)
N2 Stop 不等依赖调用排空（去掉 c.work.Wait）
--- FAIL: TestSnapshotClientStopContract
    stopcontract: first Stop with work in flight = <nil>, want the ctx error (the stop must not report a drain that did not happen)
    stopcontract: first Stop released the resource while the work was still in flight
    stopcontract: retry Stop while the work is still in flight = <nil>, want the ctx error ...
N3 给客户端加导出的 PublishRemoteSnapshot / DeleteRemoteSnapshot
--- FAIL: TestSnapshotClientHasNoWriteCapability
    SnapshotClient implements entity.IRemoteSnapshotPublisher: a read-only client must not carry write capability
N4 去掉 Linearizable 能力门
--- FAIL: TestSnapshotClientLinearizableNeedsADeclaredLoader
    Linearizable without a declared loader: found=true err=<nil> loads=1; want ErrRemoteReadUnsupported without loading
N5 解码器拿缓存底层切片（不复制）
--- FAIL: TestRemoteMirrorReaderDTOMutationDoesNotPolluteCache
    after the caller rewrote its DTO, the next read returned "XXXXXXXXXX" found=true err=<nil>; the cache was polluted
N6 去掉 reader 的 schema / codec 检查
--- FAIL: TestRemoteMirrorReaderRejectsForeignIdentityAndSchema/another_schema、/another_codec
    found=true decoded=true err=<nil>; want an error (remote snapshot: schema mismatch) before decoding
N7 token 只比版本
--- FAIL: TestRemoteObservationCoversFollowsAdmissionOrder（newer route / marker epoch、older route epoch、mixed 四格）
N8 带 epoch 的 token 也把版本下推给 loader
--- FAIL: TestRemoteSnapshotReadExitsShareOnePostCondition/token_across_an_epoch_change
    Read/Monotonic: a token from before the takeover returned route=0 version=0 found=false err=<nil>; the newer epoch satisfies it
```

### 修后

新用例 `entity`：`TestRemoteObservationCoversFollowsAdmissionOrder`、`TestRemoteMirrorReaderDTOMutationDoesNotPolluteCache`、`TestRemoteMirrorReaderNeedsNoRegistrationBesideTheOwner`、`TestRemoteMirrorReaderRejectsForeignIdentityAndSchema`、`TestRemoteSnapshotReadExitsShareOnePostCondition`（三个出口 × 同版本刷新 / 跨 epoch token，另加 Cached 最低要求）；`remoteentity`：`TestReadRemoteSnapshotMonotonic*`（2 条）、`TestSnapshotClientHasNoWriteCapability`、`TestSnapshotClientReadsWhatTheOwnerPublishesInTheSameProcess`（同进程 owner + 只读客户端 + DTO reader，兴趣经总线到 owner、复制经总线到客户端）、`TestSnapshotClientLinearizableNeedsADeclaredLoader`、`TestSnapshotClientStartFailureLeavesNoSubscription`、`TestSnapshotClientStopContract`（`stopcontract.Check` + `CallerReleases`，卡住的工作是不响应取消的权威加载）、`TestSnapshotClientStopCancelsLoads`。全部通过。

## 5. 验证（`GOWORK=off`）

- `gofmt -l` 空；`go build ./... && go vet ./...`；`go vet -tags integration ./remoteentity ./kit/dataengine`。
- `go test -race -count=3 ./entity ./remoteentity ./kit/remoteentity ./cache`：全过（既有 B2 / RR-04 等待名额 / NC-35 身份 / RR-07 schema / NC-174 停机 / 兴趣代际回归不改断言）。
- `go test ./nest ./kit/dataengine ./sync/syncbus/mirror`；根包 `go test -count=1 .`；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync`（无违例）。
- 真实依赖（隔离环境，`ROOST_REMOTE_CLUSTER_IT=1`）：`TestRealB2WatermarkMatrixStandalone`（4.04s）、`TestRealB2WatermarkMatrixCluster`（6.43s，自建 3 主 3 从）、`TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect`、`TestRealSnapshotL2KeyPrefixOnRedis(Cluster)`、`TestRealSnapshotL2StaleWrite…`、`TestRealSnapshotL2TombstoneFencesEveryWriter` 全部 PASS。
- `scripts/test-remote-generated.sh`：生成工程 12 条 `TestGeneratedRemote*` 全过（含 `BindSync` 的旧用法）。
- 生成形状未变，未跑 codegen 生成与 game-demo。

## 6. 兼容

- 公开 API 只增不删：`Manager.ReadRemoteSnapshot`、`Renew/ReleaseRemoteSnapshotInterest`、`BindSync`、`SetSyncer`、`Stats`、`SnapshotReplicaStore`、`InterestReplicaStore`、`RemoteSnapshotCache.Get/LoadAuthoritative` 签名不变。新增 `Manager.ReadSnapshot`、`Manager.SnapshotClient`、`remoteentity.SnapshotClient` 一组、`entity.RemoteObservation` 一组、`RemoteSnapshotCache.Read`。
- **行为变化（收紧）**：
  - Monotonic 未命中 / 权威低于最低版本只回源一次（之前两次，第二次不合并、不受等待名额约束），也不再等 2ms 再重读。
  - `Cached` + 最低版本：L1 有更低版本时返回 `ErrRemoteSnapshotStale`（之前交出低于要求的值）。nest `RemoteAcquireCache` + `MinVersion` 受影响。
    （发版前复审更正：nest 带 `AllowStale`（生成器标签 `allow_stale`）的 Cached 访问明确接受低于 `MinVersion` 的快照，收紧之后错误在 `Accepts` 之前返回、`allow_stale` 失效；nest 现在对这种访问不把下限交给读出口，由 `Accepts` 判定，行为回到 Mirror 之前。不带 `AllowStale` 的访问照旧被拒绝，只是错误文本变成 `remote snapshot stale`。回归 `nest/remote_cached_allow_stale_promises_test.go`。）
  - `Assembly.Stop` 返回 nil 之后，Manager 的快照读返回 `ErrSnapshotClientStopped`，不再访问 L2 / 权威 / 总线；owner 的提交后发布照旧（L2 写入按 B2 降级为未确认，总线发布不经客户端准入——提交路径的停机顺序属 K3，未改）。
- wire、L2 键格式、Lua 脚本、生成形状、配置键不变。
- 性能：读命中路径只多一次原子读（停止标志），兴趣续租与指标本来就有；每次 L2 调用多一次 `Lifetime` Begin / End（短临界区，相对一次 Redis RTT 可忽略），每次权威加载多一次 `WithCancel` + `AfterFunc`。本批没有做压测（维护者要求节省额度）；B2 的缓存层基准不经客户端，不受影响。

## 7. 第 4～6 步的入口与前置条件

| 步 | 入口 | 前置条件 / 要先定的 |
| --- | --- | --- |
| 4 订阅代际、首载缓冲、墓碑、恢复 | `SnapshotClient.Start` / `Stop`（订阅与生命周期都在这里）；`SnapshotReplicaStore.ApplyReplica`（按 key apply）；兴趣的 generation 在 `RenewInterest` / `ReleaseInterest`；全集群兴趣表 `remoteInterestRegistry` | ① 总线能否证明“确认后的发布不被静默丢弃”：普通 NATS 不满足，JetStream 要按主题选 `DeliverNew`（`sync/syncbus/driver`，B2 未改）——不满足时第 4 步只交付按需读，持续推送保持禁用（方案原文）；② session ID + generation 的 wire 字段（复制 wire 已有 `published_at`，兴趣 wire 有 Generation）；③ 首载缓冲溢出、淘汰后的 bootstrap 屏障都必须经 `admitLocked` 写缓存，不另开写路径；④ O4 兴趣容量（全集群合计）仍待维护者定 |
| 5 kit 装配、codegen 只读产物、公会摘要样例 | `NewSnapshotClient(cfg, SnapshotClientDeps{L2, Loader, LinearizableLoader, ConsumerSID})` + `Start(bus)` / `Stop(ctx)` 直接可作 kit 只读 Mod 的核心；DTO 读用 `entity.NewRemoteMirrorReader` | kit：新 Mod 只读 `remote_entity.*` 的快照段（按 A4 严格读取），不要求 Mongo 原子 backend；codegen：`remote=mirror` 的迁移诊断、DTO 解码函数与 `RemoteMirrorSpec` 生成；样例需要 owner 提供权威 loader（Managed 用 `Backend.LoadRemoteSnapshot`） |
| 6 Linux 真实故障与性能 | 第 5 步的两进程样例 | 方案“性能与完成标准”节的对照口径；客户端停机与 owner 切换要在真实 bus 上测 |

## 8. 未完成 / 边界

- owner 的提交后总线发布不经客户端准入（停止后仍可发布）：提交路径与 Assembly 的停机顺序属 K3，本批没动。
- 读结果没有“确认时刻 / 年龄”字段：新鲜度由 `cached_max_staleness` 保证（交出前该时长内被确认过）。需要逐条年龄时在第 4 步一起加。
- 第 4～6 步未开始。
