# Mirror 第 6 步观察 O-M6-1 / O-M6-3 的实施（2026-10-06）

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

**可观测**：`remote_entity.remote.interest_refresh_requests_total{result=accepted|coalesced|historic|own|invalid}`、`remote_entity.remote.interest_refresh_renewed_total`；owner 发送失败 Warn。

## 3. O-M6-3 方案：墓碑写入后 WAIT

**驱动（redis，A2 契约）**：新增可选能力 `fredis.ReplicatedEvaler.EvalReplicated(ctx, script, keys, numReplicas, timeout, args...)`，`redis/driver.Client` 实现：

- 在一条独占的物理连接上流水线发 `EVAL` 与 `ROLE`（一次往返）；`ROLE` 报告主节点当前连着的副本数。为 0 时不发 `WAIT`（单机开发环境不被 `WAIT` 卡到超时），结果标 `no_replicas`；否则同一连接上发 `WAIT <numReplicas> <timeout>`。`WAIT` 只统计本连接之前的写，所以必须与脚本同连接（与 `EvalDurable` 的 WAITAOF 同理）。
- Cluster：先按脚本第一个键取该槽的主节点（`MasterForKey`），在那个节点的独占连接上做同样的事，`WAIT` 只发往该主节点。脚本回 `MOVED` / `ASK`（拓扑刚变，脚本没执行）时改经集群客户端发一次普通脚本（自动跟随重定向），不再 `WAIT`，结果标 `redirected`。其他客户端类型（Ring 等）发普通脚本，标 `unsupported`。
- 重放：脚本照旧不重放，只有流水线里每条命令都证明没执行时才整条换连接重发（与 `EvalBatchDurable` 相同）；`WAIT` 从不重放。`WAIT` 或 `ROLE` 出错时脚本回复已经收到：脚本属于“已执行”，副本是否收到属于**结果未知**（复制未知），只体现在 `WaitErr`，不改变脚本的分类。

**L2 store**：`DeleteAtVersion` 在配置了副本数且 redis 提供上述能力时改走 `EvalReplicated`；脚本结果的处理不变。`WAIT` 的结果**不回滚、不报错给调用方**——墓碑已经写在主节点上，报错只会把一次已提交的删除变成“结果未知”：

| 结果 | 含义 | 体现 |
| --- | --- | --- |
| `confirmed` | 副本确认数 ≥ 配置 | 计数 |
| `short` | `WAIT` 超时、确认数不足（副本落后或暂停） | 计数 + 限频 Warn（每个 store 10s 一条） |
| `no_replicas` | 主节点没有连着的副本 | 计数 + 每个 store 首次一条 Info |
| `error` | `WAIT` / `ROLE` 出错（复制未知） | 计数 + 限频 Warn |
| `redirected` / `unsupported` | 没有执行 `WAIT` | 计数 |

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
- 测试设施：`scripts/mirror-local.sh test-core`、`fault redis-cluster-stop-replica` / `redis-cluster-cont`、env 导出 `ROOST_MIRROR_LOCAL_REDIS_REPLICA`；生成工程用例 `ROOST_MIRROR_LOCAL_ONLY` 支持逗号列表，S1 的 v3 先等被强杀 owner 的锁过期再写（见 6.5 O-M6-6），并报 `commit_ms` / `visible_after_confirm_ms`。

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
- **观察 O-M6-6（不是本轮引入，交维护者）**：owner 被 SIGKILL 后同 sid 重启，第一笔写要等旧进程持有的共享锁过 TTL（本用例 `LockTTL` 3s）：锁的重试预算用完报 `versioned lock not acquired`，Request 截止则是结果未知，朴素重试会多写一笔。以前 S1 的 v3 在 v2 读到之后才写，而 v2 要等陈旧上限约 1.9s，写开始时锁差不多已过期，于是被掩盖；O-M6-1 之后 v2 立即读到，用例改为先等旧锁过期再写。这与 O-M6-2 同属“故障后的写要按结果未知处理”，是既有契约；是否让同 sid 新进程更早接管锁（需要能证明旧进程已死，如进程级单实例锁的代际）属于设计取舍。
- O-M6-3 的边界不变：副本在确认之前断开（S4a′ 的“未复制即切主”）时 WAIT 挡不住，结果计为 `no_replicas` / `short`。

### 6.6 未完成

无。发版不在本轮（v1.22.0 正在发，本分支不进这一版）。
