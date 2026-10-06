# 维护者第十二轮决定：kit / core 批（2026-10-06）

依据：[DECISIONS-PENDING 第十二轮](../review/DECISIONS-PENDING-2026-10-05.md)（“B 类的都按照推荐即可”）。基线 `53fd9e9c`，分支 `bkit`，未发版。
本批九项：O-S5-2、metrics 按标签删除、readyz checker 期限、Ops Bearer、CAS 冲突率口径、robot Stage 序号（RR-20261006-09）、O-M6-5、业务时间高水位推进失败计数、game-demo `configdata_rollback_total` 面板。
不在本批：skill / cfggen / configdata 规则（`wt-bsk`）、saga 的 Mongo 收件箱与 command_consumer / step_operation_inbox（`wt-mongolat`），以及只写文档的几项。

代码阅读：codebase-memory 共享 generation 停在 09-30，本批以当前源码为准（`rg` + 直接读文件）。

## 1. O-S5-2 效果流保留期与回执 TTL

**原观察**（[REVIEW-2026-10-06-n06s5](../review/REVIEW-2026-10-06-n06s5.md) §4、O-S5-2）：原生 Nest 步骤的完成结果走 DataEngine 的效果流（`ROOST_EFFECTS`），保留期 `dataengine.effects.max_age`（缺省 168h）；完成回执 `saga.completion_receipt_ttl`（缺省 720h）。回执先过期时，流里的结果再投递一次，协调器既无回执也无 tombstone，只能 `ErrNotWaiting` → Term。saga 流的同一前提（ttl > `saga.stream_max_age`）启动时已校验，效果流跨 Mod 没有。关系：**`completion_receipt_ttl` 必须大于 `effects.max_age`**。

**改动**：`kit/saga/mod.go` `checkEffectRetention`——结果效果流（`saga.result_effect_stream`，未配置跟随 `saga.start_effect_stream`，缺省 `ROOST_EFFECTS`）等于 DataEngine 效果流（`dataengine.effects.stream`，同一读法）时比较，不满足 Init 返回 `saga: saga.completion_receipt_ttl (720h0m0s) must exceed dataengine.effects.max_age (800h0m0s): …`。流名不同（结果在别的流上）不比较：那条流的保留期不归这份配置管。DataEngine 的缺省值与读法收成 `kit/dataengine.EffectStreamRetention` / `DefaultEffectStream` / `DefaultEffectMaxAge`，两边读同一份。

**红绿**：`kit/saga/effect_retention_promises_test.go`。修前：

```
effect_retention_promises_test.go:19: saga Mod started although dataengine.effects.max_age (800h) outlives saga.completion_receipt_ttl (720h)
effect_retention_promises_test.go:43: renamed shared stream with 800h retention: <nil>; want the cross-Mod check to refuse
```

修后通过；缺省值、同流且回执更长、结果在别的流、两边一起改名四种都照常启动。

**兼容**：只有把效果流保留期调到不小于回执 TTL 的部署会被拒绝启动（之前这种配置会让 TTL 之后的重投被 Term）。生成配置不写这两个键，取缺省，不受影响。

## 2. metrics 按标签删除

**原观察**（[REVIEW-2026-10-05-n12-revn12](../review/REVIEW-2026-10-05-n12-revn12.md) O2 / O3 与方向判断“gauge 生命周期”）：loadtest 给 `robot.runner.*` 加 `run` 标签，每次运行一组新序列、永不删除，控制面约一千次运行后触到每指标序列上限。建议：Registry 加按标签删除，由对象的拥有者在销毁时删除；O2 点名的拥有者是“一次运行”（运行结束、评估完毕后删）。

**改动**：

- `metrics.Registry.DeleteSeries(name, match Labels) int`（包级 `metrics.DeleteSeries`）：删除名字为 `name`（空 = 任何名字）、标签包含 `match` 全部键值对的序列，四种类型都算，返回删除条数，并把名额还给该指标；`match` 为空不删任何东西（清空只能 `Reset`）。`SeriesCount()` 返回当前序列数。删除后同名同标签再写入是从零开始的新序列。
- `robot/loadtest`：序列的拥有者是运行记录。运行被挤出历史（`HistoryLimit`，缺省 20）时 `appendHistoryLocked` 删掉带它 `run` 标签的全部序列；同名 RunID（`StartRequest.RunID` 可指定）仍在历史里或正在跑时不删。没有在运行刚结束时删：还在历史里的运行保持可抓取，最后一个抓取周期内的增量不丢；基数上界是 `HistoryLimit` 次运行。

**用例**：`metrics/delete_series_promises_test.go`（四种类型按标签删除、`/metrics` 文本里不再出现、名额归还后新序列不被丢弃、空 match 不删）；`robot/loadtest/run_series_lifecycle_promises_test.go`（`HistoryLimit=2` 连跑 6 次：挤出历史的运行不在 `/metrics` 里，历史内的仍在，序列数稳定）。修前 loadtest 用例：

```
run_series_lifecycle_promises_test.go:66: series count kept growing across runs: map[1:7 2:11 3:15 4:19 5:23 6:27] (history limit 2)
```

~~**未改**：O3 的 `nest.dispatch.*{dispatcher}`（Nest 属核心线，原观察只记录）与 `bus_rpc_pending{method}`（受 2048 上限约束）。拥有者需要时直接调用 `DeleteSeries`。~~
**已实施（2026-10-06，分支 `oa`）**：`nest.dispatch.*{dispatcher}` 在派发器排空停止后删除（同名按计数、超时不删，[RR-20261006-18](../bug/RR-20261006-18.md)）；
`bus_rpc_pending{method}` 等可靠 RPC 的 method 标签没有注销可挂，改为有界并用断言测试固定——被调方只用注册方法，调用方每 Bus 至多 256 个（[RR-20261006-19](../bug/RR-20261006-19.md)）。`robot.loadtest.active{profile}` 的 profile 来自配置，有界。

## 3. readyz 每个 checker 的期限

**原观察**（[REVIEW-2026-10-06-n01b](../review/REVIEW-2026-10-06-n01b.md) O-H1）：`/readyz` 用请求 ctx 串行跑全部 checker，不配合 ctx 的 checker 让 handler 一直挂着，每次探针多留一个，Ops 停机还要等它们。建议：Snapshot 给短期限，卡住的 checker 报 Fail。

**改动**（`health/health.go`）：`Registry.Snapshot` 并发调用全部 checker，每个最多等 `DefaultCheckTimeout`（1.5s；`SetCheckTimeout` 可改）。到期没返回记 Fail：`message: check timed out`，`error: health check did not return within its deadline (per-check limit 1.5s; in flight for …)`；请求 ctx 先结束记 `check abandoned`。checker 拿到的 ctx 脱离请求的取消、带这个期限。同一个 checker 同一时刻只有一次调用：后来的快照等同一次调用（各自的期限内），返回后下一次快照才重新调用——卡住的 checker 杀不掉，但不会每次探针多一个 goroutine。

取 1.5s 的理由：k8s 探针 `timeoutSeconds: 2`，并发之后整个 `/readyz` 在探针断开之前答完；Redis / Mongo / etcd checker 自带的 2s ping 超时被截到 1.5s。

**红绿**：`kit/ops/readyz_checker_deadline_promises_test.go`（一个永不返回、不看 ctx 的 checker 加一个正常的；连续两次探针）。修前：

```
readyz_checker_deadline_promises_test.go:60: /readyz did not answer within 3.5s: one checker that never returns hangs the whole probe
```

修后两次都在约 1.5s 返回 503，卡住的报 Fail 并写明期限，正常的仍是 ok，卡住的 checker 只被调用 1 次。`health/checker_deadline_promises_test.go`：两个卡住的只等一个期限（并发），配合 ctx 的 checker 拿到带期限的 ctx。

## 4. Ops 必须带 Bearer

**原观察**（n01b O-P1）：`bearerToken` 没有前缀时原样返回整个头，`Authorization: <token>` 也能通过。

**改动**（`kit/ops/ops_mod.go`）：没有 `Bearer `（大小写不敏感）前缀返回空串，空串永远不等于 token。`X-Admin-Token` 不变。

**红绿**：`TestOpsAdminAuthorizationAcceptsOnlyTheExactToken` 把“bare authorization”移到拒绝组并加“padded bare token”。修前：`ops_mod_test.go:139: bare authorization: authorized when it must not be`；修后通过。

**兼容（行为收紧）**：用 `Authorization: <token>` 调 `/admin/*` 的脚本得到 401，改用 `Authorization: Bearer <token>` 或 `X-Admin-Token: <token>`。仓内脚本、生成工程与文档里没有裸 token 的用法（`rg Authorization` 核对）。

## 5. CAS 冲突率统一计数

**原观察**（[REVIEW-2026-10-05-n06-revn06](../review/REVIEW-2026-10-05-n06-revn06.md) 观察 2）：只有 chat 对 `versionstore.ErrConflict` 计数，其他服务只在 insert-only 碰撞处报 `Conflict`，account / activity 一处都没有；建议在 versionstore 层统一计数。

**改动**：

- `versionstore`：`MetricCompareAndSet = "versionstore.cas.total"`（标签 `store`、`result=applied|lost`）与 `MetricConflict = "versionstore.conflict.total"`（标签 `store`），`CountCompareAndSet` / `CountConflict`。`RedisStore.Update` 每次 compare-and-set 计一次，预算用尽返回 `ErrConflict` 时计 conflict。`store` 是存储的键前缀（每个存储一个固定值，低基数），`RedisConfig.Prefix` 注释写明。冲突率 = lost / (applied + lost)。
- 删掉调用方各自的口径：chat 不再在 `ErrConflict` 时报 `Conflict("append")` / `Conflict("prune")`；rank 自己的 CAS 循环（有序集合 + 哈希脚本，不走 `Update`）改为每次 swap 计 `CountCompareAndSet`、用尽计 `CountConflict`（store = `<prefix>:o:`），不再报 `Conflict("submit")`。
- 保留：`servicemetrics.Conflict` 的业务冲突（session `enter`、mail `send` 撞号，global `bind` 已绑到别处，directory `release` 身份变化，platform `deliver` 竞态）——它们不是存储竞争。`kit/service/README.md` 第 6 条写明分工。

**用例**：`versionstore/cas_metrics_promises_test.go`（两次成功、一次 3 次全输：applied=2、lost=3、conflict=1；不保存不计数）；rank `TestSubmitAndPageReportWhatTheyDid` 改为断言 versionstore 计数（lost = 8、conflict = 1，`conflict:submit` = 0）；chat 两条改为断言不再自报。修前新用例不编译（新 API）。

**兼容**：按 `service_conflict_total{op="append"|"prune"|"submit"}` 做看板 / 告警的改查 `versionstore_conflict_total{store=…}`（CAS 竞争）与 `versionstore_cas_total`（冲突率）。

## 6. robot Stage 序号只增不回收（RR-20261006-09）

见 [问题](../bug/RR-20261006-09.md) / [修复](../bugfix/RR-20261006-09.md)。回收点：`runPopulation` 的 `shrinkTo` 把 `launched` 减回去，`launchUpTo` 从 `launched + 1` 编号。

## 7. O-M6-5 owner 启动遇到 Mongo 选举

**原观察**（[MIRROR-STEP-6-LOCAL](MIRROR-STEP-6-LOCAL-2026-10-06.md) §3.5）：S5 之后 mongo-1 选回期间起 owner，`EnsureInfrastructure` 返回 `InterruptedDueToReplStateChange`，子进程启动失败。普通读写由驱动的可重试读写吸收，`createIndexes` 不在其列。

**改动**（`mongo/driver/collection.go`）：`EnsureIndexes` 的每个索引经 `retryDuringElection`——只认换主错误码（11602 InterruptedDueToReplStateChange、10107 NotWritablePrimary、13435、13436、189 PrimarySteppedDown、91、11600，`mongo.ServerError.HasErrorCode`，写关注错误里的也算），最多 10 次、间隔 1s；用完返回 `mongo: replica set primary election did not settle after 10 attempts 1s apart: <最后的错误>`，ctx 先到期返回 `… did not settle before the deadline (attempt n of 10): <ctx 错误>; last error: …`，原错误都在链里。其他错误立即原样返回。每次因换主失败计 `mongo.ensure_index.election_retries.total`。放在 `EnsureIndexes` 一处：它只在启动时调用（DataEngine、Remote owner 的 `EnsureRemoteStorage`、saga、效果收件箱），一处覆盖全部启动 DDL，不改 saga 收件箱文件。上界 10 × 1s 在调用方的启动期限之内（`dataengine.startup_timeout`、`remote_entity.op_timeout` 缺省 30s）。（2026-10-06 补：10 次 × 1s 是**每个索引各自**的预算——`EnsureIndexes` 对每个索引分别调 `retryDuringElection`；一次建 N 个索引且每个都撞上选举时最坏约 N×10s（9 次间隔加每次尝试本身的耗时），这时由调用方 ctx 的启动期限截断并返回点名期限的错误。正常一次选举只让最先撞上的一两个索引重试，实测每轮 1 次重试。）索引创建幂等，重做安全。

**验证**：

- 单元：`mongo/driver/election_retry_promises_test.go`（选举结束后成功并计 2 次重试；不结束时恰好 attempts 次后失败并点名、原错误在链里；ctx 先到期点名期限；非选举错误不重试）。
- 私有副本集（`scripts/mirror-local.sh test-core`，`ROOST_MIRROR_LOCAL_HOME` 在本次 scratchpad、偏移 23000，不碰共享环境）：`remoteentity/owner_startup_election_integration_test.go` 一边 `fault mongo-stepdown`，一边连续执行 owner 的存储初始化（`NewMongoCommitter(...).EnsureRemoteStorage`，每次一个新库）直到新主选出后 2s。修前（把上界临时改成 1 次，等同旧行为，未提交）：

```
owner_startup_election_integration_test.go:86: owner storage init #9 failed during the election: remote_entity: ensure transaction indexes: mongo: replica set primary election did not settle after 1 attempts 1s apart: write exception: write concern error: (InterruptedDueToReplStateChange) operation was interrupted
```

  修后（上界 10 次）同一用例连跑 6 轮，每轮 20～22 次初始化全部成功，每轮 `election retries 1`、最慢一次约 1.1s（一次 1s 间隔的重试）：

```
owner_startup_election_integration_test.go:104: stepDown: mongo-2; 20 owner storage inits all succeeded; slowest #4 took 1.120542541s; election retries 1
--- PASS: TestMirrorLocalOwnerStorageInitSurvivesAMongoElection (12.47s)
…（round 2～5 同形）
owner_startup_election_integration_test.go:104: stepDown: mongo-3; 20 owner storage inits all succeeded; slowest #4 took 1.126167625s; election retries 1
--- PASS: TestMirrorLocalOwnerStorageInitSurvivesAMongoElection (12.50s)
```

  复跑：`ROOST_MIRROR_LOCAL_HOME=<私有目录> ROOST_MIRROR_LOCAL_OFFSET=<非 0 / 1000> ROOST_MIRROR_LOCAL_CORE_RUN='^TestMirrorLocalOwnerStorageInitSurvivesAMongoElection$' scripts/mirror-local.sh test-core`。环境用完已 `clean`。

## 8. 业务时间高水位推进失败计数

**原观察**（[BUSINESS-TIME-MONOTONIC](BUSINESS-TIME-MONOTONIC-2026-10-06.md) “运行中推进失败只记 Warn，没有指标”）。

**改动**（`app/business_time.go`）：`advanceLoop` 每次推进失败计 `app.business_time.advance_failed.total`（本 App 的指标注册表），仍只 Warn、不 fail-stop。

**红绿**：`TestAFailedHighWaterMarkAdvanceIsCounted`（高水位存储写失败期间计数增长，恢复后高水位继续推进、计数不再增长）。修前：`business_time_promises_test.go:278: advance failures counted 0 while every write of the high-water mark failed, want them counted`。

## 9. `configdata_rollback_total` 面板

**原观察**（[收尾第 2 批](../bugfix/CLOSING-BATCH-2-2026-10-06.md) A17 的顺带观察）：game-demo 可观测性 README 列了 `configdata_rollback_total{trigger}`，仪表盘没有查询它。

**改动**：`demo/deploy/dev/observability/grafana/dashboards/roost-demo.json.tmpl` “事件链与配置”行改成三列（id 16 / 17 宽 8），新增“配置撤回（按触发方式，5 分钟内次数）”（id 27），查询 `sum by (trigger)(increase(configdata_rollback_total{job="{{GAME_SERVICE}}"}[5m]))`，说明写明 `apply_failed` / `operator` 的含义与 No data。其后各行不动。`codegen/internal/roost/demo_test.go` 加断言：面板按 trigger 查询该指标、README 仍列出它、`kit/configdata/configdata.go` 仍以 `configdata.rollback.total{trigger}` 计数（导出名去点加 `_total` 已存在不重复）。修前：`demo_test.go:565: the dashboard has no panel querying configdata_rollback_total by trigger`。

## 验证（全部 `GOWORK=off`，2026-10-06 本机 macOS）

| 命令 | 结果 |
| --- | --- |
| `gofmt -l`（改动的 Go 文件） | 空 |
| `go vet`（改动包 + `-tags integration ./remoteentity`） | 通过 |
| `go test -race -count=3` robot/runner、robot/loadtest、metrics、health、kit/ops、versionstore、kit/service/rank、kit/service/chat、kit/saga、kit/dataengine、mongo/driver、app | 全部 ok |
| 根包 `go test -count=1 .` | ok |
| `go build ./... && go vet ./...` | 通过 |
| `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` | 退出 0 |
| 相邻包 `go test -count=1 ./kit/... ./service/... ./servicemetrics/... ./dataengine/... ./saga/...` | 只有一次 `saga` 的 `TestAssemblyConsumesNativeNestCompletionEffects` 失败（整批并行时；单独 `-count=10` 与整包 `-count=3` 都通过）。本批对 core saga 唯一的依赖变化是 metrics 新增函数，判为负载下的既有时序敏感，记观察不改（saga 属 `wt-mongolat`） |
| `go test -count=1 ./codegen/...` | 15 个包 ok |
| `go generate ./...` 后 `git status --porcelain` | 只有本批改动，无生成漂移 |
| 生成 game-demo（`project new -template game-demo -skip-deps` → `go mod edit -replace` 到 worktree → `go mod tidy`）`go build ./... && go vet ./... && go test -count=1 ./...` | 通过（18 个包 ok）；仪表盘含 `configdata_rollback_total` 面板 |
| 私有副本集 `scripts/mirror-local.sh test-core`（O-M6-5） | 修前红、修后 6 轮绿，见 §7 |

生成器的 Core 下限（`manifest.go`）不变：生成工程只多了一个仪表盘面板，没有用到新 API。

## 兼容与未完成

- 行为收紧：Ops `Authorization` 必须带 `Bearer `（§4）；saga 在效果流保留期不小于回执 TTL 时拒绝启动（§1）；`/readyz` 的 checker 超过 1.5s 记 fail（§3，之前是一直等）。
- 指标口径：chat 的 `conflict:append` / `conflict:prune`、rank 的 `conflict:submit` 不再上报，改为 `versionstore_cas_total` / `versionstore_conflict_total`（§5）。
- 新 API：`metrics.Registry.DeleteSeries` / `SeriesCount` / `metrics.DeleteSeries`、`health.DefaultCheckTimeout` / `Registry.SetCheckTimeout`、`versionstore.MetricCompareAndSet` / `MetricConflict` / `CountCompareAndSet` / `CountConflict`、`kit/dataengine.EffectStreamRetention` / `DefaultEffectStream` / `DefaultEffectMaxAge`。
- ~~未做：O3 的 nest 派发器 gauge 与 `bus_rpc_pending{method}` 的删除（见 §2）~~ 已实施（分支 `oa`，[RR-20261006-18](../bug/RR-20261006-18.md)、[RR-20261006-19](../bug/RR-20261006-19.md)）；account / activity 等直接用 `versionstore.RedisStore` 的服务自动获得 CAS 计数，没有逐服务补测试（计数在 versionstore 一处，已有用例）。
