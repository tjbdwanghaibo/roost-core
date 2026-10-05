# B2：Remote 快照缓存以共享 L2 为水位权威，L1 只是有界副本（2026-10-06）

依据：[DECISIONS-PENDING-2026-10-05](../review/DECISIONS-PENDING-2026-10-05.md) B2 行与“维护者决定（第三轮）”：共享 L2 为快照水位权威，L1 只是有界副本；Cached 读最大陈旧时间写成配置与契约。
背景：[N05 审查 revn05](../review/REVIEW-2026-10-05-n05-revn05.md)（方向判断、O4～O6）；修复链 [RR-20260913-01](../bugfix/RR-20260913-01.md)（U-0187 及两轮残余）、[RR-20260913-05](../bugfix/RR-20260913-05.md)、[RR-20260913-06](../bugfix/RR-20260913-06.md)、[RR-20260913-08](../bugfix/RR-20260913-08.md)、[NC-35](../bugfix/RR-20261005-NC-35.md) / [NC-36](../bugfix/RR-20261005-NC-36.md)、[NC-130](../bugfix/RR-20261005-NC-130.md)；Mirror 方案 [PLAN-REMOTE-POLICY-MIRROR](../review/PLAN-REMOTE-POLICY-MIRROR.md)；A2 的“未完成”（[A2 方案](A2-DRIVER-REPLAY-CONTRACT-2026-10-05.md)：L2 DEL 结果未知时旧快照最多留到 TTL）。

范围：`entity/remote_snapshot.go`（快照缓存）、`remoteentity/snapshot_l2.go` / `syncer.go` / `config.go` / `transaction_manager.go` 的缓存装配、`kit/remoteentity` 的配置读取。Remote 权限（owner / grant / fence、Mongo 写权限、finalizer）属核心线 K3，不动。

## 1. 盘点：L1 的写入口与现在的新旧判定（基线 `b7f98343`）

| 入口 | 调用链 | 现在怎样判定新旧 |
| --- | --- | --- |
| owner 提交后发布 | `afterRemoteCommit` → `RemoteSnapshotCache.Publish` | 本机墓碑早查 → L1 同版本冲突 → **L1 冷时预查 L2**（只找同版本冲突）→ `ReadThroughStore.Set`：L2 CAS，非致命错误当断网照写 L1 → CAS 落败（`ErrStaleWrite`）时 `adoptNewerFromL2` 再读 L2 |
| 复制消息（全量 / 增量） | `SnapshotReplicaStore.ApplyReplica` → `ApplyUpdate` → `Publish` | 同上；增量以 L1 当前值为基，缺基 / epoch / schema 不符回源 |
| 权威加载回填 | `LoadAuthoritative` → loader → `Publish` → 读回 L1 | 同上，另有 NC-35 身份、NC-36 最终版本、RR-08 过期的读后检查 |
| L2 回填 | `Get` → `ReadThroughStore.Get` → `loadOne` → `setLocal` | **不经 publish 锁**；靠 AtomicLocal 的 `Superseded` 钩子查本机墓碑侧表 |
| 删除（版本化） | 提交 `Invalidations` / 复制删除 → `DeleteAtVersion` | L1 有更新的就不动；否则记本机墓碑侧表、L2 `DeleteAtVersion`（结果 `_ =` 吞掉）、删 L1 |
| 删除（无版本） | 旧发布者的复制删除 → `Delete` | `ReadThroughStore.Delete`，不留水位 |
| 迁移接管 | 没有单独入口 | L1 `Stale` 与 L2 Lua 同一条 epoch 规则：更新的 marker / route epoch 胜，混合两向拒绝 |
| DeliverAll 历史重放（O5） | JetStream durable → `ApplyReplica` | 与复制消息相同，没有“消息有多老”的概念 |

分散的判定状态共五处：

- **本机墓碑侧表**（`tombstones` + `tombMu`）：只挡本进程；时间有界。
- **`Superseded` 钩子**：为了让不经 publish 锁的 L2 回填也查墓碑侧表（RR-20260913-01 复核）。
- **`FatalRemoteError` 错误分类**：`ReadThroughStore` 把 L2 错误一律当断网，再列出“不是断网”的三类（冲突、同版本异值、stale）。
- **L1 冷时的 L2 预查**：只为提前发现同版本冲突（RR-20260913-05），真正的原子边界是 CAS。
- **L2 墓碑**（`deleted_version`，N05）：共享层水位的第一步。

每次补一个入口或一层判定，状态在增加。根因是“谁是水位权威”没有单一答案：L1、L2、本机墓碑各持有一部分。

## 2. 目标不变量

1. **一个判定入口。** 一个 key 的全部 L1 写入（发布、复制、加载回填、L2 回填、删除、修复）都走 `admitLocked`，并在该 key 的 publish 分片锁下进行。L2 回填的 L2 读在锁外，写 L1 时在锁内重新比较。
2. **先 L2，后 L1。** 写入先在 L2 上以版本 CAS 落地（`Set` / `DeleteAtVersion` 两个脚本，规则不变），只有 L2 接受、或 L2 已有同值 / 更新值时，L1 才记下这份值，并带上**确认时刻**（`confirmedAt`，L2 调用开始前的本机时间，保守）。L2 拒绝时 L1 改记 L2 当前持有的值；L2 已无值（墓碑或过期）时 L1 记一个删除标记。
3. **L1 的删除标记取代本机墓碑侧表。** 删除在 L1 里是一个条目（`deleted` + 版本），与快照条目用同一条 `Stale` 规则排序：标记挡住不新于它的快照，不新于快照的删除被拒。墓碑侧表、`Superseded` 钩子、`FatalRemoteError` 分类、L1 冷时预查四处一起删掉；`cache` 包里的通用 `Superseded` / `FatalRemoteError` 能力保留（公开 API，有自己的测试），Remote 快照不再使用。
4. **L2 回答不了时降级，但不冒充已确认。** L2 断网或结果未知时，L1 仍记下这份值，但 `confirmedAt = 0`（未确认）。权威加载的结果例外：它本身就是权威在加载开始之后的状态，记为在加载开始时刻已确认。
5. **陈旧上限。** `remote_entity.cached_max_staleness`（core `Config.CachedMaxStaleness`，缺省等于 `snapshot_cache_ttl`，30s）。非线性读（Cached / Monotonic）只交出 `now − confirmedAt ≤ 上限` 的 L1 条目；否则先**重新确认**（见 §3），确认不了就不交出。Linearizable 照旧每次读权威。
6. 没有共享 L2 的装配（测试、单进程）里 L1 自己就是水位：写入直接算确认；删除标记受 L1 容量淘汰（生产装配 `Assemble` 一定带 Redis L2）。

## 3. 读取与重新确认；L2 不可用时

| L1 状态 | Cached | Monotonic(min) |
| --- | --- | --- |
| 已确认且未超上限的快照 | 交出 | 版本 ≥ min 交出，否则回源 |
| 已确认且未超上限的删除标记 | 未找到 | 回源（同今天的 miss） |
| 无条目 | 读 L2：有值则记入 L1 并交出；无值 / 出错为未找到（**Cached 不因 miss 读权威**，与今天相同） | 读 L2，仍不足则回源 |
| 未确认、或超过上限 | 重新确认 | 重新确认，仍不足则回源 |

重新确认（同一 key 的并发读合并成一次，等待者可按 ctx 放弃）：

1. 读 L2（一次 HGET）。L2 与 L1 同值 → 改记确认时刻；L2 更新 → 改记 L2 的值；L1 更新（L1 是未确认的、L2 写丢了）→ 把 L1 这份重新 CAS 进 L2（版本化 CAS 与带版本删除都是幂等的，重发安全），成功即确认；L1 是删除标记而 L2 还持有不更新的快照 → 重发 `DeleteAtVersion`。
2. L2 读不到（断网）或没有值（L2 已过期或是墓碑），而 L1 原来是快照 → **回源读权威**：结果按“权威加载”记入 L1（加载开始时刻为确认时刻）；权威说不存在 → 删掉 L1 里加载开始之前确认的条目，返回未找到。
3. 都失败 → 返回错误（Cached 不再交出超过上限或未确认的条目）。

L2 不可用时的语义因此是“**继续降级、标为不可信**”：写入不失败、L1 照记但未确认；读取不交出未确认的条目，而是回源权威，权威的结果在上限内可以直接服务。每个 key 每个陈旧窗口每个节点最多回源一次（并发合并）。不选“拒绝缓存”：Cached 读不回源，拒绝缓存会让 L2 一断就全部变成未找到。

陈旧上限的含义（写进 USER_GUIDE）：Cached / Monotonic 交出的快照，在交出前 `cached_max_staleness` 之内被 L2 或权威确认过“没有更新的版本或删除”。**不覆盖**：L2 本身落后于权威的情形——owner 提交后写 L2 失败或结果未知，L2 在 `snapshot_l2_ttl` 内仍持有旧版本，确认会确认到它。缓解：owner 的 L1 条目未确认，自己的下一次读会把新版本补进 L2；复制删除到达的每个节点都会对 L2 重发带版本删除（幂等），复制更新同理。

## 4. N05 观察与 A2 的未完成项

- **O5（新 sid 的 durable 用 DeliverAll 重放历史）**：重放的旧快照写进 L2 前要过 CAS。只要 L2 还记得更新的版本或墓碑，就会被拒——这在 L2 为权威后对每条消息都成立。L2 已经过期（键 5 分钟没有提交）时 CAS 会接受旧版本，L2 与 L1 一起复活旧值，这是 L2 本身覆盖不到的。补法：复制消息带上发布时刻（wire 增加 `published_at`，旧消息没有该字段时照旧接受），接收方丢弃早于 `snapshot_l2_ttl / 2` 的快照更新——更老的消息 L2 已经无法替它担保（L2 对一次写入的记忆只有 TTL 那么长，留一半给跨节点时钟偏差与投递延迟）。丢掉只意味着之后按需读取。删除消息不过滤（带版本删除重放是无害的）。先用真实 JetStream + Redis 拿红。
- **O4（兴趣容量是全集群总量，满后续期被静默拒绝）**：推送停止后，消费者的 L1 条目在陈旧上限到期后重新确认，陈旧度从“L1 TTL 与 L2 TTL 的组合”收紧为 `cached_max_staleness`。容量本身（全集群合计、拒绝不可见）不变，列为后续。
- **A2：L2 DEL 结果未知**：带版本删除的结果不再吞掉，未确认时 L1 留删除标记；本机下一次读会重发（幂等）；收到复制删除的每个节点也会对 L2 重发。Remote 快照只用带版本删除；无版本 `Delete` 只为兼容旧发布者保留。
- **O6（Cached 没有成文的最大陈旧时间）**：由 §2.5 / §3 解决。

## 5. 兼容与性能

- **L2 键格式不变**：两个 Lua 脚本一字不改；`remoteSnapshotL2Store.DeleteAtVersion` 在 L2 持有更新快照时改为返回 `cache.ErrStaleWrite`（之前把脚本的 0 当成功），只有 `entity` 调用。
- **复制 wire 增加 `published_at`**（`omitempty`）：旧接收方忽略未知字段；新接收方收到旧消息（没有该字段）时照旧接受，旧发布者在滚动升级期间仍可能触发 O5。
- **新旧进程混跑**：L2 规则相同，旧进程仍按旧规则写 L1，新进程的确认只依赖 L2。旧进程仍可能被 NC-130 / O5 的旧路径影响，直到全部升级。
- **行为收紧**：L2 断网时，复制或发布写入的条目不再直接服务 Cached 读，要回源权威（今天会一直交出，直到 L1 TTL）；超过陈旧上限的条目同理。
- **公开 API**：`RemoteSnapshotCacheConfig` 增加 `MaxStaleness`、`Now`；`Config` 增加 `CachedMaxStaleness`；`RemoteSnapshotCache.Stats` 签名不变（第二个返回值改由缓存自己的计数填充）。已生成工程不迁移，不配置时使用缺省值。生成配置模板本轮不加新键（A1 正在改 codegen，避免冲突；缺省值等同于今天的 L1 TTL）。
- **性能**：今天每次 L1 写已经要做一次 L2 CAS（`ReadThroughStore.Set`），L1 冷时还多一次 HGET 预查。新设计：热路径仍是一次 CAS，去掉 L1 冷时的预查；只在冷路径（CAS 被拒、重新确认、修复）多一次 HGET。读命中不访问 L2；超过上限的 key 每个窗口一次 HGET。实测见 §7。

## 6. 验证计划

- 组合矩阵（真实 Redis + 自建 Redis Cluster，3 主 3 从）：旧版本覆盖新版本、删除后复活、迁移后旧 epoch、DeliverAll 重放 × 写入来源（发布、复制、加载回填；L2 回填与删除交错在单元测试）× L1 冷 / 热。
- O5：真实 JetStream + Redis 先红后绿。
- 陈旧上限：可控时钟，超过上限不再服务、转为回源；L2 断网时回源；L2 与权威都不可用时返回错误。
- 既有回归（entity / remoteentity / cache / kit/remoteentity）全量 `-race -count=3`；glsvet；根包。

## 7. 实施状态

**已实施**（分支 `b2l2`，基线 `b7f98343`，提交号见 DECISIONS-PENDING 末表）。图谱 `Users-whb-roost-roost-core` 的共享 generation 停在 09-30，早于 10-05 的 N05 / A2 改动，本轮以当前源码为准。

### 改动

| 文件 | 变化 |
| --- | --- |
| `entity/remote_snapshot.go` | L1 条目改为 `remoteSnapshotEntry`（快照或删除标记 + `confirmedAt`）；唯一写入口 `admitLocked`（`publishLocked` / `DeleteAtVersion` 都经它）；`refresh` 重新确认（读 L2、修复、回源）；`readConfirmed` 热路径只读一次 L1 与一次时钟；`coalesce` 合并同 key 的重新确认与回源。删除本机墓碑侧表、`Superseded` 钩子用法、`ReadThroughStore`（及其 `FatalRemoteError` 分类）与 L1 冷时的 L2 预查。`RemoteSnapshotCacheConfig` 增加 `MaxStaleness`、`Now` |
| `remoteentity/snapshot_l2.go` | `DeleteAtVersion` 在 L2 持有更新快照时返回 `cache.ErrStaleWrite`；两个 Lua 脚本不变 |
| `remoteentity/syncer.go` | 复制 wire 增加 `published_at`；`ApplyReplica` 丢弃早于 `snapshot_l2_ttl / 2` 的快照更新（计数 `remote_entity.snapshot_replica_historic_dropped_total`） |
| `remoteentity/config.go`、`transaction_manager.go` | `Config.CachedMaxStaleness` 传给缓存 |
| `kit/remoteentity/remote_entity_mod.go` | `remote_entity.cached_max_staleness` 用 `app.ConfigDuration` 严格读取（A4 尚未推送，`app.ConfigDuration` 已在 main，NC-190）；必须为正 |
| `cache/store.go`、`cache/read_through.go` | 只加注释：Remote 快照不再使用 `Superseded` / `FatalRemoteError`，二者作为通用能力保留 |

### 先红后绿

修前（`b7f98343` 的实现；新用例需要的 `MaxStaleness` / `Now` / `CachedMaxStaleness` 字段先以“只加字段、不生效”的形式补上）：

```text
--- FAIL: TestB2CachedReadPastMaxStalenessReconfirmsAgainstL2
    Cached read 11s after confirmation (max staleness 10s) returned payload="v1" version=1 found=true err=<nil>; L2 holds v2
--- FAIL: TestB2EntryWrittenDuringL2OutageIsNotServedUnconfirmed
    an entry written while L2 was unreachable was served unconfirmed: payload="v1" version=1 found=true err=<nil>; L2 holds v2
--- FAIL: TestB2PublisherRepairsALostL2Write
    L2 after the owner's next read holds version=1 held=true err=<nil>; the lost v2 write was never repaired
--- FAIL: TestB2LostL2DeleteIsRepairedByTheNextRead
    L2 still serves the deleted snapshot version=1 held=true err=<nil>; the lost delete was never repaired
--- FAIL: TestB2HistoricReplicaPastL2MemoryIsNotAdmitted
    a replica published 10m ago (L2 TTL 1m) was admitted: payload="v1-from-history" found=true err=<nil>

# 真实 JetStream + 真实 Redis（O5 复现）
--- FAIL: TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect
    replayed messages applied=1; late joiner Cached read found=true payload="v1" err=<nil>; L2 held=true version=1 err=<nil>
    the late joiner serves the replayed v1 (authority is at v2)

# 组合矩阵：单机与自建 Cluster 各 20 格，各红 8 格（两个后端相同）
--- FAIL: .../old-over-new/{publish,replica,load}/hot        the reader (hot=true) read version=1 ... found=true
--- FAIL: .../delete-resurrect/{publish,replica,load}/hot    the reader (hot=true) read version=1 ... found=true
--- FAIL: .../deliverall-replay/replica/cold                 L2 holds the stale input version=1 route=1
--- FAIL: .../deliverall-replay/replica/hot                  the reader (hot=true) read version=1 ... found=true
```

其余 12 格修前已绿（RR-20260913-01/05/06、NC-130 的历次修复覆盖了冷节点），作为回归保留。修后全部通过：

```text
ok  remoteentity  TestB2*（6 条，含并发收敛；-race -count=5）
--- PASS: TestRealB2WatermarkMatrixStandalone (4.04s)    20/20
--- PASS: TestRealB2WatermarkMatrixCluster (6.48s)       20/20（3 主 3 从，端口 17380～17385，自建自杀）
--- PASS: TestRealJetStreamReplayAfterL2ExpiryDoesNotResurrect (2.12s)
    replayed messages applied=1; late joiner Cached read found=false ...; L2 held=false
--- PASS: TestRealSnapshotL2KeyPrefixOnRedis / OnRedisCluster / StaleWrite / Tombstone（既有真实 Redis 用例）
```

矩阵里“L2 回填与删除交错”需要暂停 HGET，只在单元测试覆盖（`entity/snapshot_delete_l2_promises_test.go`，修后仍绿）。

### 改了断言的既有用例（契约变化，不是放宽）

- `TestRemoteSnapshotPublishDoesNotPinShardOnUnresponsiveL2`、`TestStaleBackfillControls/L2 outage`：L2 断网时写入仍不失败、L1 仍持有（`l1Snapshot` / `WaitForVersion` 观察），但 Cached 读不再交出未确认的条目。
- `TestPublishConflictAfterPreflight`：预查已删除，改为直接验证 L2 CAS 的同版本异值裁决返回 `ErrRemoteVersionConflict` 且 L1 不写。
- `TestRemoteSnapshotL2DeleteAtVersionKeepsNewerSnapshot`、`TestRealSnapshotL2KeyPrefixOnRedis(Cluster)`：被更新快照拒绝的带版本删除返回 `cache.ErrStaleWrite`（之前 nil）。
- `TestRemoteSnapshotDeleteAtVersionPromiseClearedByNewerSnapshot`：墓碑侧表不存在了，改为检查 L1 条目已从删除标记换成新快照。

### 性能（真实 Redis 单机，同机交替 3 轮 × count 2，`-benchtime 3000x`，Apple M5，Go 1.27.0）

```text
                         │   修前      │   修后                         │
RealB2PublishWarm-10       34.53µ ± 7%   34.42µ ± 1%        ~ (p=0.589 n=6)
RealB2ReplicaCold-10       55.99µ ± 3%   34.68µ ± 2%  -38.06% (p=0.002 n=6)
RealB2CachedHit-10         147.2n ± 2%   143.8n ± 3%   -2.31% (p=0.015 n=6)
RealB2CachedReconfirm-10                 21.79µ ± 1%   （修前无此路径）
allocs/op: PublishWarm 29 → 29，ReplicaCold 46 → 28，CachedHit 0 → 0，CachedReconfirm 20
```

结论：L1 写的 L2 往返数不变（一次 CAS）；冷节点的复制写入少了一次 HGET 预查（−38%）；读命中不碰 Redis、无退化（第一版热路径读了两次时钟，+17%，已改为一次）；超过上限的读多一次 HGET（约 22µs），每个 key 每个陈旧窗口一次。基准在 `remoteentity/snapshot_l2_watermark_bench_integration_test.go`。

### 其他验证

`GOWORK=off`：`gofmt -l` 空；`go build ./... && go vet ./...`；`go vet -tags integration ./remoteentity`；`go test -race -count=3 ./entity ./remoteentity ./kit/remoteentity ./cache`；`go test ./nest ./kit/dataengine ./codegen/internal/nest`；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync`（无违例）；根包 `go test -count=1 .`；`scripts/test-remote-generated.sh`（隔离环境，生成工程 12 条 `TestGeneratedRemote*` 全过，含 -race）。生成形状未变，未跑 codegen 生成与 game-demo。

### 未完成 / 后续

- **L2 落后于权威**（owner 写 L2 失败或结果未知）：陈旧上限确认到的是 L2，L2 本身最长落后 `snapshot_l2_ttl`。已有缓解（owner 自己的下一次读补写、每个收到复制删除的节点重发带版本删除），没有后台补写队列。
- **O4 兴趣容量**：全集群合计、满后续期在 owner 处被拒且消费者不知道。B2 只把它的后果收紧到陈旧上限，容量设计未改。
- **O5 的滚动升级窗口**：旧发布者的消息不带 `published_at`，新接收方照旧接受；全部升级后才完全覆盖。同步总线按主题选 `DeliverNew` 仍是 Mirror 方案的方向，本轮未改 `sync/syncbus/driver`。
- **生成配置模板**未加 `cached_max_staleness`（A1 正在改 codegen；缺省值等于今天的 L1 TTL，行为一致）。
- 没有共享 L2 的装配里，删除标记受 L1 容量淘汰（之前的墓碑侧表只按时间老化）。生产装配 `Assemble` 一定带 Redis L2。
- Mirror DTO 方案（订阅代际、首载缓冲、只读 reader）仍未实施；本轮实现了其中的 MaxStaleness 与共享层水位两项。
