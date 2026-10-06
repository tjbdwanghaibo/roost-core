# Mirror 第 6 步观察 O-M6-1 / O-M6-3 的实施（2026-10-06）

> **状态（2026-10-06 核对）**：O-M6-1 / O-M6-3（`db67b8ee`）与 O-M6-6（`d483238e`）已实施，均不在 v1.22.0 里，随 v1.23.0 发布；O-M6-5 见 [第十二轮 kit 批](DECISIONS-R12-KIT-2026-10-06.md)（`7b73aabc`，同样随 v1.23.0）。

依据：[维护者决定第十轮](../review/DECISIONS-PENDING-2026-10-05.md)（两项都按推荐）；观察原文见 [第 6 步本机替代](MIRROR-STEP-6-LOCAL-2026-10-06.md) §3.5。
基线：`fa98f53f`（v1.22.0 发版流程进行中，本分支不进这一版）；分支 `m6obs`。

## 1. 目标

- **O-M6-1**：owner 以相同 sid 重启后兴趣表是空的（兴趣主题的 durable 从游标续读，已确认的兴趣不重放），要等只读方的下一次续租（剩余不足一半才发，缺省最长约 15s）才恢复推送，期间读取只靠陈旧上限回源。改为 owner 启动时广播一条“请重新续租兴趣”，只读方收到后立即把持有的兴趣续租一次，推送在一次请求往返内恢复。
- **O-M6-3**：Redis 切主时如果墓碑还没复制到副本，L1 空的新只读方会在陈旧上限内读回已删除的实体。改为 L2 带版本删除（写墓碑的脚本）成功后在同一连接上发 `WAIT`，等至少一个副本确认后再返回；只对墓碑做，普通快照写入不变。

## 2. O-M6-1 方案：兴趣续租请求

**wire（只新增）**：新主题 `remote_entity_interest_refresh`，消息体 `{"requester_sid":<owner sid>,"requested_at":<Unix 纳秒>}`；信封 `Key` 由 requester sid 派生（非零）、`Version = requested_at`，`Op = Upsert`，三者与消息体逐项核对，不符即拒绝（与快照 / 兴趣消息相同的身份绑定）。兴趣主题与快照主题的格式都不变。

**谁发**：`Assembly.Start` 在 `SnapshotClient.Start` 订阅成功之后（兴趣订阅已经确认，之后发出的续租一定能到达），且只在快照推送开着时（`PushEnabled`，JetStream）发一次。发送失败只记 Warn 与计数，不让启动失败：退化到原来的续租周期。重复 Start（失败重试）会再发一次，请求幂等。只读的 `RemoteMirrorMod` 不发（它不是 owner，没有要恢复的兴趣表）。

**谁收**：每个 `SnapshotClient` 在快照推送开着时多订阅这个主题（JetStream 上走 `SubscribeLive`，即 DeliverNew durable：请求只对当下有意义，新 sid 不重放历史请求）；推送关着时不订阅（没有推送，续租也没有用）。收到后：

1. 丢弃自己发的（requester sid = 本机）与过期的（`now - requested_at > snapshot_interest_ttl`：那之后每个租约都至少经过了一个续租周期，请求已无意义；同 sid 重启的只读方从 durable 游标续读到的旧请求在这里被过滤）。
2. 触发一次“续租遍历”：单飞（同一时刻最多一个遍历），遍历进行中到达的请求只置一个待办标记，当前遍历结束后再做**一次**，多少个请求都合并成这一次；两次遍历的开始间隔至少 `interestRefreshMinGap`（1s）。遍历在单独的 goroutine 里做（受 `work` 准入约束、`Stop` 取消并等待），不占复制 handler：JetStream 的 AckWait 内必须返回。
3. 遍历对本机兴趣表里**仍然有效**的每个 key 走与读时续租同一个入口：条带锁内分配新的兴趣代际（第 4 步）、先经本机兴趣表按同一配额判定（O4）、再广播。与读时续租唯一的差别是不看“剩余不足一半”的门槛。条带锁内重新核对 key 仍在本机表里且未过期：遍历开始之后被 release 的 key 不会被复活（release 留下的撤销水位也挡住更旧的续租）。

**有界**：一次遍历的消息数不超过本机兴趣表的 key 数（上限 `snapshot_interest_keys`），owner 端按 O4 配额接纳，和一个正常续租周期的消息量同级；请求再多也只合并成“当前一次 + 之后一次”，且间隔不小于 1s。缓存写入不经这里，`admitLocked`（B2）不受影响。

**混跑**：旧 owner 不发请求 → 新只读方行为不变；旧只读方不订阅新主题 → 新 owner 的请求没人收，旧只读方按原来的续租周期收敛（最长约 15s，与现在相同）；JetStream 流的 subject 是 `<prefix>.>`，新主题不用改流配置。普通 NATS（推送本来就关着）两端都不收不发。

**可观测**（2026-10-06 按源码更正：补 `stopped` 与 owner 侧发送计数）：只读方 `remote_entity.remote.interest_refresh_requests_total{result=accepted|coalesced|historic|own|invalid|stopped}`（`stopped`：客户端已停，`work` 准入关闭，请求不再安排遍历）、`remote_entity.remote.interest_refresh_renewed_total`；owner `remote_entity.remote.interest_refresh_sent_total{result=sent|error}`，发送失败另记 Warn。

## 3. O-M6-3 方案：墓碑写入后 WAIT

**驱动（redis，A2 契约）**：新增可选能力 `fredis.ReplicatedEvaler.EvalReplicated(ctx, script, keys, numReplicas, timeout, args...)`，`redis/driver.Client` 实现：

- 在一条独占的物理连接上流水线发 `EVAL` 与 `ROLE`（一次往返）；`ROLE` 报告主节点当前连着的副本数。为 0 时不发 `WAIT`（单机开发环境不被 `WAIT` 卡到超时），结果标 `no_replicas`；否则同一连接上发 `WAIT <numReplicas> <timeout>`。`WAIT` 只统计本连接之前的写，所以必须与脚本同连接（与 `EvalDurable` 的 WAITAOF 同理）。
- Cluster：先按脚本第一个键取该槽的主节点（`MasterForKey`），在那个节点的独占连接上做同样的事，`WAIT` 只发往该主节点。脚本回 `MOVED` / `ASK`（拓扑刚变，脚本没执行）时改经集群客户端发一次普通脚本（自动跟随重定向），不再 `WAIT`，驱动结果的跳过原因为 `ReplicatedSkipRedirected`。其他客户端类型（Ring 等）发普通脚本，跳过原因为 `ReplicatedSkipUnsupported`。两种跳过在 L2 store 的指标与统计里合并为 `skipped`（见下表）。
- 重放：脚本照旧不重放，只有流水线里每条命令都证明没执行时才整条换连接重发（与 `EvalBatchDurable` 相同）；`WAIT` 从不重放。`WAIT` 或 `ROLE` 出错时脚本回复已经收到：脚本属于“已执行”，副本是否收到属于**结果未知**（复制未知），只体现在 `WaitErr`，不改变脚本的分类。

**L2 store**：`DeleteAtVersion` 在配置了副本数且 redis 提供上述能力时改走 `EvalReplicated`；脚本结果的处理不变。`WAIT` 的结果**不回滚、不报错给调用方**——墓碑已经写在主节点上，报错只会把一次已提交的删除变成“结果未知”：

| 结果 | 含义 | 体现 |
| --- | --- | --- |
| `confirmed` | 副本确认数 ≥ 配置 | 计数 |
| `short` | `WAIT` 超时、确认数不足（副本落后或暂停） | 计数 + 限频 Warn（每个 store 10s 一条） |
| `no_replicas` | 主节点没有连着的副本 | 计数 + 每个 store 首次一条 Info |
| `error` | `WAIT` / `ROLE` 出错（复制未知） | 计数 + 限频 Warn |
| `skipped` | 没有执行 `WAIT`：驱动报重定向（Cluster 拓扑刚变）或客户端不支持；redis 不提供该能力时 store 自己发普通脚本，同样计 `skipped` | 计数（统计字段 `Skipped`） |

（2026-10-06 按源码更正：原表写 `redirected` / `unsupported` 两个结果；`recordTombstoneWait`（`remoteentity/snapshot_l2.go`）的指标标签只有 `confirmed|short|no_replicas|error|skipped` 五个值，跳过原因只在驱动返回值里区分。）

指标 `remote_entity.snapshot_l2_tombstone_wait_total{result}`；store 另有 `TombstoneWaitStats()` 供测试与诊断。调用方最多多等 `timeout`。

**配置（A4 严格读取并登记到 `app/config_validation.go`）**：

| 键 | 缺省 | 说明 |
| --- | --- | --- |
| `remote_entity.snapshot_l2_tombstone_wait_replicas` | 1 | 等几个副本确认；0 关闭。负数拒绝 |
| `remote_entity.snapshot_l2_tombstone_wait_timeout` | 50ms | `WAIT` 的超时；必须为正（Redis 的 `WAIT … 0` 是永久阻塞）且不超过 1s（调用方最多多等这么久；也要低于 Redis 客户端读超时 3s，否则客户端先超时、计为 error） |

缺省取 1 个副本、50ms：正常的异步复制在同机房是亚毫秒到几毫秒，`WAIT` 阻塞时主节点立即向副本要 ACK，确认一次往返即返回；50ms 覆盖正常抖动，又把副本异常时调用方（owner 的提交后发布、只读方的删除消息 apply）多等的时间限制在一个很小的值。删除本来就少，普通快照写入不变。core 的 `Config` 零值是关闭；`DefaultConfig` 与 kit 缺省打开。

**范围之外**：`WAIT` 只能缩小窗口，不能消除（副本在确认之前断开 / 被暂停时 `WAIT` 超时，墓碑仍只在主上；这正是 S4a′ 的“未复制即切主”，切主后仍会回到删除前的版本，结果标 `short` / `no_replicas`，可观测）。要彻底消除需要 `min-replicas-to-write` 等部署约束，或陈旧上限内也做权威校验，属于另外的取舍，不在本轮。

## 4. 改动面

- `remoteentity`：`interest_refresh.go`（新，请求的发送、接收与遍历）、`snapshot_client.go`（订阅 / 停机 / 续租入口带强制标志）、`assemble.go`（Start 发请求）、`snapshot_l2.go`（WAIT、配置构造 `NewSnapshotL2StoreFromConfig`）、`config.go`（两项配置）。
- `redis`：`client.go` 新接口；`redis/driver/client.go` 实现；`redis/driver/README.md` 契约。
- `kit/remoteentity`：`readSnapshotConfig` 读两项新键，两个 Mod 用新构造；`app/config_validation.go` 登记。
- `scripts/mirror-local.sh`：导出副本端口、新增 `test-core`（跑 remoteentity 的私有环境集成用例）与 `redis-cluster-cont`。
- 文档：本文、第 6 步记录、USER_GUIDE、TROUBLESHOOTING、CHANGELOG `[Unreleased]`、DECISIONS-PENDING 第十轮、交接。

不改：快照 / 兴趣 wire、L2 键与 Lua、生成形状与 catalog 配置模板。

## 5. 验证

- O-M6-1 单元（可控：进程内同步总线、遍历执行器可注入）：owner 同 sid 重启后，修前兴趣表为空、新提交不推送，要把只读方本机租约推到半衰期之后的下一次读才恢复（红）；修后 `Assembly.Start` 返回时兴趣已恢复、新提交直接推送（绿）。另：遍历不复活已 release 的 key、合并与间隔、过期请求与自己的请求被丢、推送关着时不订阅、旧 owner（不发请求）行为不变、身份不符的消息被拒。
- O-M6-3 单元：替身驱动下 `confirmed / short / no_replicas / error` 的计数与 `DeleteAtVersion` 返回值；配置为 0 时不调用新能力；kit 严格读取。
- 私有环境（`scripts/mirror-local.sh`）：单机主 + 副本，副本经私有 toxiproxy 代理复制并加延迟（确定性的复制滞后），写墓碑后立刻提升副本：修前（不 `WAIT`）新只读方读到已删实体（红），修后墓碑在提升后仍在（绿）；Cluster：`WAIT` 只打到该键的主节点（`INFO commandstats` 计数）、副本暂停时按超时返回 `short` 不卡住。S1 / S7 前后各跑一次对比恢复时间。

## 6. 实施记录（基线 `fa98f53f`，分支 `m6obs`）

代码阅读：codebase-memory 项目 `Users-whb-roost-roost-core` 的图谱停在主检出的旧代际（共享 generation，Mirror 第 4～6 步之后的文件未必反映），本轮以 worktree 当前源码为准，按源码读取补证。

### 6.1 实际实现

- O-M6-1：`remoteentity/interest_refresh.go`（新）——请求主题与消息、`InterestRefreshStore`、单飞遍历（`acceptInterestRefresh` / `runInterestRefresh`，持 `work` 准入，`Stop` 取消间隔等待并等它退出）；`SnapshotClient.renewInterest(ctx, key, refresh)` 成为续租的唯一入口，`RenewInterest` 委托它；`Start` 在推送开着时第三个订阅（`mirror.NewLive`），失败逐步回收；`Assembly.Start` 在 `SnapshotClient.Start` 之后、存储初始化与 outbox 恢复之前调 `requestInterestRefresh`，失败只 Warn。
- O-M6-3：`redis.ReplicatedEvaler` / `ReplicatedEvalResult`（`redis/client.go`），`redis/driver/replicated.go` 的 `EvalReplicated`（EVAL + ROLE 一条流水线，副本数 > 0 才同连接 WAIT；Cluster 用 `MasterForKey`；MOVED / ASK 改普通发送）；`remoteSnapshotL2Store.DeleteAtVersion` 在配置了副本数且客户端支持时走它，`recordTombstoneWait` 计数 / 限频 Warn / 首次 Info；`NewSnapshotL2StoreFromConfig` + `ValidateSnapshotL2TombstoneWait`（超时上限 `MaxSnapshotL2TombstoneWaitTimeout` = 1s：调用方最多多等这么久，也要低于 Redis 客户端读超时 3s，否则客户端先超时、计为 error）；`Config.SnapshotL2TombstoneWaitReplicas / Timeout`（`DefaultConfig` 1 / 50ms，零值关闭）；kit `readSnapshotConfig` 严格读取两项新键，`app/config_validation.go` 登记。
- 测试设施：`scripts/mirror-local.sh test-core`、`fault redis-cluster-stop-replica` / `redis-cluster-cont`、env 导出 `ROOST_MIRROR_LOCAL_REDIS_REPLICA`；生成工程用例 `ROOST_MIRROR_LOCAL_ONLY` 支持逗号列表，S1 的 v3 先等被强杀 owner 的锁过期再写（见 6.5 O-M6-6；§7 实施后改回立即写），并报 `commit_ms` / `visible_after_confirm_ms`。

### 6.2 先红后绿

**O-M6-1 单元**（`remoteentity/interest_refresh_promises_test.go` 的 `TestOwnerRestartWithTheSameSidGetsInterestBackWithoutWaitingForRenewal`）。修前：同一用例（去掉遍历执行器的替换）放到基线 `fa98f53f` 的 detached worktree 上跑：

```
--- FAIL: TestOwnerRestartWithTheSameSidGetsInterestBackWithoutWaitingForRenewal (0.00s)
    interest_refresh_red_test.go:127: after the owner restarted with the same sid the reader still reads v1, want v2 pushed right away: the restarted owner's interest table is empty (interested=false) and the reader renews only past half its lease
```

同一用例的对照段（把只读方本机租约推到半衰期之后再读）在修前也通过：推送要到下一次续租才恢复。修后整条用例通过。其余承诺：遍历不复活已 release / 已过期的 key 且用新代际（`TestInterestRefreshRenewsOnlyLiveInterests`）；遍历开始前的请求被这次遍历覆盖、遍历中途的请求合并成之后的一次、过期 / 自己的请求不触发、身份不符被拒（`TestInterestRefreshRequestsCoalesceAndAreValidated`）；间隔等待中 `Stop` 不挂住、停后不再接受（`TestInterestRefreshGapWaitEndsOnStop`）；推送关着时不订阅不发送、没有订阅者（旧只读方）时 owner 照常启动（`TestInterestRefreshNeedsPushAndToleratesOldConsumers`）；请求主题订阅失败逐步回收（`TestSnapshotClientRefreshSubscriptionFailureLeavesNoSubscription`）。既有 Assembly 生命周期用例的订阅数从 2 改为 3（`liveSubscriptions`）。

**O-M6-3 真实 Redis**（`remoteentity/snapshot_l2_tombstone_wait_integration_test.go`，`scripts/mirror-local.sh test-core`）。确定性手段：副本改经私有 toxiproxy 的临时代理复制，“主 → 副本”方向加 500ms 延迟；写墓碑后立刻 `REPLICAOF NO ONE` 提升副本，再用 L1 空的新只读方（只有 L2）读。修前：同一场景在基线 worktree 上（`Assemble` / `RemoteMirrorMod` 修前用的构造 `NewSnapshotL2StoreWithKeyPrefix`）：

```
--- FAIL: TestMirrorLocalTombstoneSurvivesAFailoverOnlyWithWait/pre_fix (0.03s)
    snapshot_l2_tombstone_red_integration_test.go:213: the tombstone must survive the failover: delete took 0s; fresh reader found=true version=1, tombstone=""
```

修后（同一私有环境脚本，`-v`）：

```
MIRROR O-M6-3 without_wait: delete took 0s; after promoting the replica tombstone="" fresh reader found=true version=1; wait stats {Confirmed:0 ...}
MIRROR O-M6-3 with_wait: delete took 502ms; after promoting the replica tombstone="2" fresh reader found=false version=0; wait stats {Confirmed:1 ...}
MIRROR O-M6-3 cluster: stopped replica, delete took 241ms, wait stats {Confirmed:1 Short:1 NoReplicas:0 Failed:0 Skipped:0}
--- PASS: TestMirrorLocalTombstoneSurvivesAFailoverOnlyWithWait (9.56s)
--- PASS: TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary (0.43s)
```

`without_wait` 子用例保留为边界记录（不 WAIT 时的现象可确定复现）；`with_wait` 之后旧主没有副本，再删一次走 `no_replicas`、不等。Cluster：`INFO commandstats` 的 `cmdstat_wait` 只在该键所在的主节点 +1；只 SIGSTOP 它的副本（复制连接仍在）时 WAIT 按 200ms 超时返回（241ms），计为 short，删除照常成功。单元：`redis/driver/replicated_promises_test.go`（假服务端按连接记录命令：WAIT 与 EVAL 同连接、没有副本不发 WAIT、WAIT 出错只进 WaitErr、脚本错误不发 WAIT、关闭时只发 EVAL），`remoteentity/snapshot_l2_tombstone_wait_promises_test.go`（各结果的计数、返回值只由脚本决定、快照写入不走 WAIT、副本数 0 / 旧构造不调用新能力、设置校验），`kit/remoteentity/tombstone_wait_config_test.go`（两个 Mod 的严格读取）。

### 6.3 S1 / S7 前后对照（私有环境，同机，基线与本分支各起一套环境，`ROOST_MIRROR_LOCAL_ONLY=S1,S7`）

| 场景 | 指标 | 基线 `fa98f53f` | 本分支 |
| --- | --- | --- | --- |
| S1 owner 强杀、同 sid 重启 | 重放补发的 v2：重启就绪 → 只读方读到 | 1895ms（陈旧上限回源） | −24ms（推送先于就绪信号到达） |
| S1 | 重启后的写 v3：确认之后只读方还要等多久（`visible_after_confirm_ms`；写本身 45 / 46ms） | 2246ms（陈旧上限回源） | −5ms（推送先于回复） |
| S1 | 只读方计数 | errors=0、违例 0、权威回源 0 | errors=0、违例 0、权威回源 0 |
| S7 只读方强杀、同 sid 重启 | 首读 v3 / 重启后推送 | 71ms / 33ms | 75ms / 29ms |

S1 的恢复从“陈旧上限（3s）回源”变成推送，在一次请求往返内；S7 不受影响（只读方重启后本来就在首读时续租）。两套环境结束都输出 `no residual processes` 并删除根目录。

### 6.4 验证（`GOWORK=off`）

- `gofmt -l` 空；`go build ./... && go vet ./...`；`go vet -tags integration ./remoteentity ./redis/...`。
- `go test -race -count=3 ./remoteentity ./redis/... ./kit/remoteentity ./sync/syncbus/...` 通过；`go test -count=1 ./app .`（根包门禁）通过；`go test -count=1 ./codegen/... ./kit/...` 通过；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` 无违例（本轮没改这四个包，顺带跑）。
- 私有环境：`scripts/mirror-local.sh test-core` 两次通过、基线上的修前红一次；生成工程 S1 / S7 基线与本分支各若干次（最终对照见 6.3）。每次都由脚本 `clean` 并核对无残留；结束后 `ps` 也没有私有根目录下的进程。共享环境 `~/.roost-it` 没有碰。
- 没改生成形状（模板、生成器、catalog 配置模板）：没跑 `go generate` porcelain 与 game-demo。

### 6.5 兼容与观察

- wire：只新增 `remote_entity_interest_refresh` 主题；快照 / 兴趣主题、L2 键与 Lua 不变。混跑语义见 §2。JetStream 上每个节点多一个 DeliverNew durable（推送开着时）。
- 行为：删除 Remote 实体（以及只读方 apply 删除消息时写墓碑）最多多等 `snapshot_l2_tombstone_wait_timeout`（缺省 50ms，上限 1s），通常是一次副本往返；没有副本时不等。core `Config{}` 零值不等；`DefaultConfig` 与 kit 缺省等 1 个副本。旧构造 `NewSnapshotL2Store` / `NewSnapshotL2StoreWithKeyPrefix` 不等（行为与修前相同）。
- 公开 API 只增：`redis.ReplicatedEvaler`、`driver.Client.EvalReplicated`、`remoteentity.NewSnapshotL2StoreFromConfig`、`ValidateSnapshotL2TombstoneWait`、`MaxSnapshotL2TombstoneWaitTimeout`、`SnapshotL2TombstoneWaitStats`、`InterestRefreshStore`、`SyncTopicInterestRefresh`。
- **观察 O-M6-6（不是本轮引入，交维护者）**：owner 被 SIGKILL 后同 sid 重启，第一笔写要等旧进程持有的共享锁过 TTL（本用例 `LockTTL` 3s）：锁的重试预算用完报 `versioned lock not acquired`，Request 截止则是结果未知，朴素重试会多写一笔。以前 S1 的 v3 在 v2 读到之后才写，而 v2 要等陈旧上限约 1.9s，写开始时锁差不多已过期，于是被掩盖；O-M6-1 之后 v2 立即读到，用例改为先等旧锁过期再写。这与 O-M6-2 同属“故障后的写要按结果未知处理”，是既有契约；是否让同 sid 新进程更早接管锁（需要能证明旧进程已死，如进程级单实例锁的代际）属于设计取舍。**更正（2026-10-06）**：维护者第十一轮按推荐决定接管，已实施，见 §7。
- O-M6-3 的边界不变：副本在确认之前断开（S4a′ 的“未复制即切主”）时 WAIT 挡不住，结果计为 `no_replicas` / `short`。

### 6.6 未完成

无。发版不在本轮（v1.22.0 正在发，本分支不进这一版）。

## 7. O-M6-6：同 sid 重启立即接管上一代进程留下的 Remote 实体锁（2026-10-06，第十一轮决定）

依据：[维护者决定第十一轮](../review/DECISIONS-PENDING-2026-10-05.md)（按推荐）；观察原文见 §6.5。基线：`e65f1cb6`；分支 `m6lock`。
代码阅读：codebase-memory 项目 `Users-whb-roost-roost-core` 的图谱是主检出的共享 generation（停在 09-30 之后的旧代际），本节以 worktree 当前源码为准。

### 7.1 现状

- Remote 实体的共享锁是 Redis hash `lock:<lock_key>:<id>`，字段 `owner` 是取锁 token。token = 锁对象前缀（`crand.Text()` + `.`）+ 序号（RR-20261004-01）：Lua 只认“本锁对象更早序号”的 owner 可以取回，其余 owner 一律等 TTL。锁值里**没有 sid，也没有进程代际**，所以同 sid 重启的新进程分不清 owner 是自己上一代留下的还是别的进程正在用的，只能等 TTL（`remote_entity.lock_ttl`）。
- App 单实例锁的值是 `<token>|<hostname>|<pid>|<started_unix_ms>`，`token` 是每次启动 `crypto/rand` 的 16 位十六进制（[APP-SINGLETON-LOCK](APP-SINGLETON-LOCK-2026-10-05.md) §3.2）；它在任何 Mod Init 之前拿到。拿到即说明同一 `<key_prefix>:<server_type>:<sid>` 的上一代进程已经死了，或者卡住超过 TTL、恢复后会失锁 fail-stop。

### 7.2 设计

1. **代际令牌复用 App 单实例锁的 token**（不另造身份）。App 在 `singleton.enabled=true` 时多登记一个只读能力 `app.ModSingletonIncarnation`（值 `app.SingletonIncarnation{Key, Sid, Token}`：锁键、sid、本次启动的 token）；未启用时不登记。
2. **锁值只增字段**：`AssemblyDeps.Incarnation`（`*remoteentity.ProcessIncarnation{Holder, Token}`）非空时，取锁 token 变为 `~<sid>~<holder 摘要>~<incarnation>~<锁对象随机串>.<序号>`（`holder 摘要` 是单实例锁键 SHA-256 的前 16 位十六进制，区分同 sid 不同服务类型 / 不同项目前缀）。前缀 `~<sid>~<摘要>~` 是“同一个单实例锁持有者”的范围，再加 `<incarnation>~` 是“本进程这一代”。旧格式 token 以 base32 大写字母开头，不会以 `~` 开头；没有 Incarnation 的进程照旧用旧格式。
3. **接管时机：第一次取锁时在 Lua 里当场接管**（不在启动时扫描）。取锁脚本多收两个参数（范围前缀、本代前缀）：owner 带同一范围前缀、但不是本代前缀 → 视同 owner 不存在，在同一条脚本里换成新 token、分配新 fence（与 TTL 过期后的取锁是同一分支），并回报“接管了上一代”。选它的理由：
   - 原子：判定与换 owner 在一条 Lua 里，是对 Redis 的 CAS；不需要先读后写。
   - 简单可靠：锁键按实体分布、不按 sid 索引，启动时扫描要 `SCAN` 全部锁键（Cluster 下逐节点），代价与实体数成正比，扫描期间新锁还会出现；而懒接管只发生在真的要写的实体上，零额外往返。
   - 时机天然在“持有单实例锁之后”：Remote 在 Mod Provide/Start 才构造锁，App 在任何 Mod Init 之前已经拿到单实例锁。
4. **不接管的情况**：owner 是别的 sid / 别的服务类型 / 别的项目前缀（范围前缀不同）、本代自己留下的（同一进程里被回收的 wrapper 留下的结果未知 token，照旧等 TTL 或按 RR-20261004-01 由同一锁对象取回）、旧格式 token（升级前的进程）。`singleton.enabled=false` 时 kit 不传 Incarnation：没有“旧进程已死”的证明，**不接管**，维持按 TTL 等待。kit 只在 Remote 的 `localSid` 等于单实例锁的 sid 时传（`NewRemoteEntityMod(sid)` 显式给了别的 sid 时不传）。
5. **可观察**：接管计数 `remote_entity.lock_takeover_total`；首次接管记一条 Info（之后不逐条记）。

### 7.3 owner / grant / fence 语义（K3）逐项论证

接管在语义上等价于“上一代的租约在此刻过期”：TTL 过期发生的时刻相对于进程事件本来就是任意的（旧进程可能在任何一步之后卡住超过 TTL），所以协议对“任意时刻过期”必须已经正确；接管只是把这个时刻提前到新进程第一次取锁时，并且只在单实例锁证明旧进程已死 / 将 fail-stop 时才这样做。逐项：

- **owner（Mongo `_owner_sid` / `_owner_shared` / `_owner_epoch` / `_owner_route`）**：不动。接管只改 Redis 锁 hash 的 `owner` 字段；所有权的转移、进出共享模式仍只经 `changeOwnership` 的 CAS。取锁之后的 `GrantWrite` 照旧按 `_owner_shared` 过滤，写入路径照旧核对 `grant.Ownership.Shared`。
- **grant（Mongo `_grant_fence` / `_grant_token`）**：接管后新进程照常 `GrantWrite(新 token)`，一次 FindAndModify 递增 `_grant_fence` 并写入新 token，上一代的许可随之作废；上一代若还有在途提交（只可能是卡住后恢复、即将 fail-stop 的进程），`CommitRemote` 的过滤条件要求 `_grant_fence` 等于它的 fence，会被拒绝。没有新增“跳过许可”的路径。
- **fence（Redis `lock:...:fence` 计数器）**：接管与 TTL 过期后的取锁走同一分支：先 `INCR` 再写 owner，fence 单调递增；`grant.Fence != lockFence` 的核对不变。
- **version**：锁 hash 的 `version` 不动（与过期后取锁相同）；有权威时版本取自 `GrantWrite` 的返回值。
- **上一代的续期 / 释放**：Touch / Refresh / Unlock 的 Lua 都要求 owner 等于自己的 token，被接管后分别得到过期 / 未持有，不会改动新一代。
- **旧进程卡住后恢复的残余边界**（与 APP-SINGLETON-LOCK §4 同类）：恢复后到续期拿到“不是我的”之间（一次 Redis 往返）若它恰好取锁，会按同一规则反向接管新进程的锁；新进程那一笔被 Mongo fence 拒绝（确定的拒绝，不是结果未知），它下一次取锁再接管回来，至多持续到旧进程 fail-stop。带 DataEngine 的服务里旧进程卡住期间持有 WAL `flock`，新进程在打开 WAL 处退出，不会进入服务阶段，这个交错不会出现。

### 7.4 改动面与兼容

- `app/singleton.go`：`SingletonIncarnation`、`ModSingletonIncarnation`，`openSingleton` 登记；token 从锁值第一段取得（不改锁值格式）。
- `remoteentity`：`ProcessIncarnation`、`AssemblyDeps.Incarnation`（校验）、锁工厂带代际、`versionedTryLockLua` 多两个参数与一个回报字段、接管计数。
- `kit/remoteentity`：`Provide` 查 `app.ModSingletonIncarnation` 并在 sid 一致时传入。
- 生成工程 S1（`codegen/internal/entity/testdata/remoteflow/mirror_local_test.go`）：owner 子进程按 S1 的前提（只在 SIGKILL 之后重启）给 Assemble 传本次启动的代际，代替 App 单实例锁；去掉“先等旧锁过期再写”的绕行。
- 兼容：锁 token 是不透明字符串，wire / Mongo 字段不变（`_grant_token` 变长）；混跑时旧进程看不懂新格式，只按 TTL；新进程不接管旧格式。公开 API 只增。

### 7.5 实施记录（基线 `e65f1cb6`，分支 `m6lock`）

**实际实现**（与 §7.2 一致，没有偏离）：
- `app/singleton.go`：`SingletonIncarnation{Key, Sid, Token}`、`ModSingletonIncarnation`；`openSingleton` 在登记 `ModSingleton` 之后登记它（token 取锁值第一段，锁值格式不变）。`kit/mods` 同名常量。
- `remoteentity/versioned_lock.go`：`ProcessIncarnation`、`lockIncarnation`（`newLockIncarnation` 校验 sid 非 0、Holder 非空、Token 1～64 位且只含字母数字 `-` `_`；`prefixes`、`noteTakeover`：计数 `remote_entity.lock_takeover_total` + 每个 Assembly 第一次 Info）；工厂带 `incarnation`，`NewVersionedLock` 把代际前缀放在锁对象前缀之前；`TryLock` 把范围前缀 / 本代前缀作为 ARGV[5] / ARGV[6] 交给取锁脚本，第 4 项回报为 1 时计数。
- `remoteentity/versioned_lock_lua.go`：`versionedTryLockLua` 增加接管判定（与 owner 不存在 / 本锁对象更早序号同一分支），返回值多一项（旧调用方只读前三项）。
- `remoteentity/assemble.go`：`AssemblyDeps.Incarnation`，非 nil 时校验并装到锁工厂，格式错误 `Assemble` 失败（`ErrVersionedLockConfig`）。
- `kit/remoteentity/remote_entity_mod.go`：`Provide` 查 `ModSingletonIncarnation`，`Sid == localSid` 时传入。
- 生成工程 S1 夹具：owner 子进程传 `ProcessIncarnation{Holder: mirror-local:<prefix>:owner:<sid>, Token: <pid>-<启动纳秒>}`（编排只在 SIGKILL 之后同 sid 重启，代替单实例锁的保证）；去掉“先等旧锁过期再写”，加断言：重启后第一笔写耗时 < lock_ttl/2、只读方最终停在 v3（无多写）。

**先红后绿**：

1. 单元（`remoteentity/lock_takeover_promises_test.go`，内存 Redis 替身按取锁脚本逐行模拟，共用一份 Mongo 权威替身）。修前红：同一场景只用旧 API（两次 `Assemble` 同 sid、旧进程取锁后不释放不续期，新进程 `Lock` 带 3 次重试）在基线上：

```
--- FAIL: TestRedSameSidRestartFirstLockWaitsForTheOldProcesssLease (0.01s)
    lock_takeover_red_test.go:98: the restarted same-sid process's first lock = versioned lock not acquired after 7ms, want it acquired right away (the old process is dead; its lease runs for lock_ttl 3s)
```

   修后绿：`TestSameSidRestartTakesOverThePreviousIncarnationsSharedLock`（第一次取锁就成、Redis fence 与 Mongo 许可都更新、上一代许可提交被拒、上一代 Touch 过期 / Unlock 未持有、新一代提交与释放成功）；对照 `TestLockTakeoverOnlyAppliesToThePreviousIncarnationOfTheSameSingletonHolder`（别的 sid、同 sid 别的服务类型、新进程未启用单实例锁、旧格式 token、同一代另一个锁对象：全部 NotAcquired 且持有者续期不受影响）；`TestAssembleRejectsAMalformedIncarnation`。app：`TestSingletonIncarnationIsTheHeldLocksIdentity`（启用时 Key / Sid / Token 与 Init 时锁值一致；未启用不登记）。kit：`TestRemoteEntityModPassesTheSingletonIncarnationToTheLocks`（启用且 sid 一致 → token 以 `~1101~` 开头并含本次 token；未启用 / sid 不一致 → 旧格式）。

2. 真实 Redis Lua + 真实 Mongo（`remoteentity/lock_takeover_integration_test.go`，`scripts/mirror-local.sh test-core`，私有根目录、端口偏移 26000）。修前：基线 detached worktree 上同一场景（旧 API，新进程第一次 `TryLock`、不重试）：

```
    lock_takeover_red_integration_test.go:69: the restarted same-sid process's first TryLock = versioned lock not acquired, want it to take over the dead process's lease (lock_ttl 3s)
--- FAIL: TestMirrorLocalSameSidRestartTakesOverTheOldIncarnationsLock (0.20s)
```

   修后（同时跑 `^TestMirrorLocal` 全部用例）：

```
MIRROR O-M6-6: restarted same-sid TryLock took 9ms (lock_ttl 3s); fence 1 -> 2; old grant commit: remote entity: state version conflict
--- PASS: TestMirrorLocalSameSidRestartTakesOverTheOldIncarnationsLock (0.29s)
--- PASS: TestMirrorLocalTombstoneSurvivesAFailoverOnlyWithWait (10.24s)
--- PASS: TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary (0.81s)
```

   同一用例里别的 sid、同 sid 未启用单实例锁的 `TryLock` 都是 NotAcquired。

3. 生成工程 S1（`ROOST_MIRROR_LOCAL_ONLY=S1 scripts/mirror-local.sh test`，去掉绕行、立即写）。修前（基线，夹具去掉 Incarnation）：

```
    mirror_local_test.go:418: the first write after the restart took 2408ms (lock_ttl 3s): it waited for the killed owner's lease instead of taking it over (O-M6-6)
--- FAIL: TestGeneratedRemoteMirrorLocal/S1_owner_kill_restart_wal_replay (4.71s)
```

   修后：

```
MIRROR6 .../S1_owner_kill_restart_wal_replay owner_restart_ready_ms=566 replayed_v2_converge_ms=-19
MIRROR6 .../S1_owner_kill_restart_wal_replay first_write_started_after_kill_ms=566 (lock_ttl 3s)
MIRROR6 .../S1_owner_kill_restart_wal_replay after_restart_commit_v3 commit_ms=33 converge_ms=25 visible_after_confirm_ms=-8
MIRROR6 .../S1_owner_kill_restart_wal_replay reader_stats loads=0 errors=0 reads=182
--- PASS: TestGeneratedRemoteMirrorLocal/S1_owner_kill_restart_wal_replay (4.38s)
```

   写在强杀后 566ms 开始（旧租约还有约 2.4s），33ms 写成，版本是 v3，只读方最终停在 v3，没有多写的 v4。

每次私有环境运行结束都由脚本 `clean`，输出 `no residual processes`；共享环境 `~/.roost-it` 没有碰。

**验证**（`GOWORK=off`）：`gofmt -l` 空；`go build ./... && go vet ./...`；`go vet -tags integration ./remoteentity`；`go test -race -count=3 ./remoteentity ./kit/remoteentity ./app` 通过；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync ./remoteentity` 无违例；`go test -count=1 . ./kit/mods ./kit ./codegen/internal/entity` 通过；私有环境 `test-core` 与 S1 见上。没改生成形状（模板、生成器、catalog 配置模板），没跑 `go generate` porcelain 与 game-demo。

**组合契约复核**：错误分类不变（接管成功即普通取锁成功；不接管时仍是 NotAcquired）；TTL 只对“同一单实例锁持有者的上一代”提前到期，其余照旧；接管后的 GrantWrite 失败照旧按 `versionedAbandonLua` 只清本 token（上一代 owner 已被替换，不恢复，与过期后取锁失败相同）；取锁回复丢失时 owner 是本锁对象的 token，下一次 `TryLock` 按 RR-20261004-01 取回。

**兼容**：锁 token 只是变长的不透明字符串（Redis `owner`、Mongo `_grant_token`），wire、键与 Mongo 字段不变；混跑时旧进程不认新格式、新进程不接管旧格式，都退回 TTL。公开 API 只增：`app.SingletonIncarnation`、`app.ModSingletonIncarnation`、`mods.ModSingletonIncarnation`、`remoteentity.ProcessIncarnation`、`AssemblyDeps.Incarnation`。core `DefaultConfig` 的 `LockTTL` 是 24h（生成配置 15s）：直接用 core 默认值又没开单实例锁的装配，强杀后同 sid 重启仍可能等很久，这一点没有改变。

**未完成**：无。不发版（等收尾统一发）。
