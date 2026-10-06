# v1.23.0 发版前补充验证与门禁（2026-10-06）

分支 `relprep`（worktree `wt-relprep`），基线 `origin/main` = `d6a677e0`。Go 1.27.0 darwin/arm64，Apple M5 10 核。
全部命令 `GOWORK=off`。修复按先红后绿；只做验证的项如实记录。本机原始输出复制在主检出（被忽略）的
`artifacts/perf/relprep-20261006/`，故障矩阵结果目录见第 5 项。

| 项 | 结论 | 改动 |
| --- | --- | --- |
| 1 saga 偶发失败 | **已复现，根因是用例的时序假设**（产品行为正确），已修用例 | `saga/nest_completion_promises_test.go` |
| 2 示例实跑门禁 | 门禁已加；先红（两处）后绿。顺带发现并修了 `examples/` 模块 go.sum 过期、GOWORK=off 下编译不过 | `examples_run_test.go`、`examples/go.mod`、`examples/go.sum`、roost-coding / roost-optimize |
| 3 两条真实依赖用例 | 都通过；global Bind 用例补上 RR-20261006-05 的同参数重试断言，负对照（退回修复）为红 | `kit/service/integration/redis_test.go` |
| 4 可选本地复验 | saga 真实 NATS nak / MaxDeliver 通过；Redis Cluster ASK / MOVED 上的 L2 读写与墓碑 WAIT 通过。顺带修了 mirror-local 集群就绪判定（副本未 online 时 WAIT 被跳过，既有用例单独跑必红） | 新增两个 integration 用例、`scripts/mirror-local.sh` |
| 5 故障矩阵预跑 | 见第 5 项 | 无 |

## 1. saga `TestAssemblyConsumesNativeNestCompletionEffects` 偶发失败

**复现条件**（`artifacts/perf/relprep-20261006/stress1.sh`）：`go test -race -c` 编出 `saga.test`，背景并行跑
`go test -race -count=1 -p 8 ./nest/... ./dataengine/... ./sync/... ./entity/... ./skill/...` 三轮制造压力，同时：

- A：8 个实例并行 × 3 轮，`-test.run '^TestAssemblyConsumesNativeNestCompletionEffects$' -test.count=20 -test.cpu 1,2,8`（共 1440 次）；
- B：整包 4 个实例并行 × 2 轮，`-test.count=3 -test.cpu 1,2,8`；
- C：字面命令 `go test -race -count=20 -p 8 -cpu 1,2,8 -run TestAssemblyConsumesNativeNestCompletionEffects ./saga/...`。

**修前（红）**：A 失败 8 次，**全部在 `-cpu 1`**（用例名无 `-N` 后缀；8/480）；B、C 各 0 次。失败文本（6 个输出文件同形）：

```
--- FAIL: TestAssemblyConsumesNativeNestCompletionEffects (0.00s)
    nest_completion_promises_test.go:145: the saga is still waiting after its native step reported success: {ID:native-1 Type:rally ... Status:waiting Phase:forward Step:1 CompletedSteps:1 Attempt:1 Incarnation:0 Version:3 ... OperationKey:native-1:1:1 CommandID:native-1:1:1:1 ...}
```

**根因**：读到的记录是 `Step:1 CompletedSteps:1 Version:3`、在等的是**第 1 步**（`native-1:1:1`）——第 0 步的原生完成已被收下。
`Engine.Complete`（`saga/engine.go` Complete 末尾）接收结果后 `e.signal(e.dueKick)` 唤醒协调器，`Assembly.Start` 起的
`Engine.Run` 协调器循环随即把第 1 步派发出去（`recordingJetStream.Publish` 立即成功），记录重新进入 `waiting`。用例断言
`stored.Status != StatusWaiting`，假设测试 goroutine 的 `Get` 一定先于协调器派发；`GOMAXPROCS=1` 加负载时协调器常先运行，
于是偶发失败。产品行为正确（`testDefinition` 是两步 saga，收下第 0 步后派发第 1 步正是契约），不是产品缺陷，不登记 RR。

**修法**：断言改成与协调器时序无关的事实——不再等第 0 步（`!(Status==waiting && OperationKey==第 0 步)`）、
`CompletedSteps==1 && Step==1`，并新增 `Store.CompletionRecorded` 断言回执已写。同包其余 `Status == StatusWaiting` 断言逐个核对：
`completion_definition_rollout_promises_test.go:74` 的引擎没有 `Run`，`step_operation_incarnation_promises_test.go:309` 断言的是
`Complete` 的返回值（`after.Clone()`），都不受协调器影响，不改。

**修后（绿）**：同一脚本重跑（重新编译测试二进制），A 1440 次 0 失败（其中 `-cpu 1` 480 次），B、C 0 失败。修前 8/480 的失败率下
480 次全过的概率约 e⁻⁸。证据：`artifacts/perf/relprep-20261006/item1-red/`、`item1-green/`（`stress1.summary` 与各 `A-*.out`）。

## 2. 示例实跑门禁

**问题**：A1 之后 `skill/examples/statusbridge` 一运行就 panic（战斗组件改动在事务外），build / vet / 测试都发现不了——示例只编译不运行。
排查时发现更早的一处：`examples/`（独立模块，`replace => ..`）的 go.sum 缺 `robot` / `sync/nettransport` 新依赖
（quic-go、kcp-go）的条目，`GOWORK=off go run ./robotdemo` 直接报 `missing go.sum entry`。`examples/` 与 `skill/examples/` 都是
独立模块，根模块的 `go build ./...` / `go vet ./...` 不包含它们，CI 也没有任何一步构建它们。

**门禁**：根包 `TestExamplesRun`（`examples_run_test.go`）——

- 穷尽发现：遍历仓库（跳过 `.git` / `artifacts` / `testdata` / `node_modules`），路径含 `examples` 段、且有非测试 `package main` 源文件的目录；
  与 `exampleRuns` 登记表逐字比对，新示例不登记就失败。`rg -l '^package main' | rg examples` 核对：共 6 个——
  `examples/{configgen,lubanreal,robotdemo}`、`skill/examples/{combat,fireball,statusbridge}`。`kit/service/examples/split` 是库包
  （`package split`，自带测试，随根模块 `go test ./...` 运行），不是可运行示例。
- 每个示例在自己的模块里 `GOWORK=off go build`（5 分钟上限），在示例目录里运行（1 分钟上限），要求退出码 0；子用例并行。
- 跳过：只有需要外部依赖的示例允许在表里写理由跳过。现有 6 个都只用进程内或回环资源（robotdemo 自带回环 TCP echo 服务器），**没有跳过项**。
- 耗时：缓存热时根包这条约 2.5～4 s。

**先红**（`artifacts/perf/relprep-20261006/examples-red.out`）：`git show 229a5aa0^:skill/examples/statusbridge/main.go` 覆盖当前文件
（A1 之后、修复之前的版本），并 `git stash` 掉 go.sum 修复：

```
--- FAIL: TestExamplesRun/examples/robotdemo (0.07s)
    examples_run_test.go:67: 编译 examples/robotdemo（模块 examples）失败：exit status 1
        ../robot/dialers.go:19:2: missing go.sum entry for module providing package github.com/quic-go/quic-go (imported by github.com/tjbdwanghaibo/roost-core/robot); to add:
--- FAIL: TestExamplesRun/skill/examples/statusbridge (2.02s)
    examples_run_test.go:67: 运行 skill/examples/statusbridge 失败：exit status 2
        panic: combatcomponent: persistence mutation outside transaction: nest: transaction is already closed
```

**后绿**：恢复两处（`cmp` 确认 statusbridge 与修复版逐字一致）后 6 个子用例全部 PASS（`examples-green.out`）。
go.sum 修复：`cd examples && GOWORK=off go mod tidy`（go.mod 只增 indirect 依赖）；`skill/examples` tidy 会删 4 行多余 require，
不影响编译，保持不动。

**规范**：`docs/agent-skills/roost-coding/SKILL.md`“验证与性能纪律”加一条“示例要实跑，不能只编译”；`docs/agent-skills/roost-optimize/SKILL.md`
同步一句入口提示。本机 `~/.claude/skills` 副本未动（维护者同步）。

## 3. 两条之前没跑的真实依赖用例

共享隔离环境 `~/.roost-it/roost-dataengine-it`（只 source env.sh、不打印；运行前确认 `remote-acceptance.lock` 不存在；`-run` 只跑这两条）。
资源：saga 用例库名 `roost_revn06s5_<pid>_<ns>`（用例自建自删）；global 用例键前缀 `itest:global:<ns>`（Cleanup 按前缀删键）。

| 用例 | 命令 | 结果 |
| --- | --- | --- |
| saga `TestRealMongoCoordinatorLeaseTakeover` | `go test -tags integration -count=1 -v -run '^TestRealMongoCoordinatorLeaseTakeover$' ./saga/` | PASS（3 个子用例：B 领取后 A 先 Apply、B 已 Apply 后 A 晚到、A 的超时决定输给 B 与完成结果；真实副本集上走完整 `MongoStore.ClaimDue`） |
| `kit/service/integration` `TestGlobalRunsOnRedis` | `REDIS_ADDR=$ROOST_DATAENGINE_IT_REDIS_ADDR go test -tags integration -count=1 -race -v -run '^TestGlobalRunsOnRedis$' ./kit/service/integration/` | PASS |

原 global 用例没有 RR-20261006-05 的同参数重试：本轮在第一次 `Bind` 之后补两条断言——同参数再 `Bind` 返回同一个绑定（真实 Redis 的
`Create` 报已存在，`Bind` 回读比较），换 group 仍是 `ErrConflict`。**负对照**：临时把 `kit/service/global/service.go` 的回读比较改成
`if false && ...`（即退回 RR-05 修复前），同一命令红：

```
redis_test.go:520: retried Bind against real Redis = {GameSID:0 ...}, global: conflict: game 7 is already bound to group-a/100; want the same binding {GameSID:7 GlobalGroupID:group-a GlobalSID:100 ... Epoch:1 ...}
```

恢复后绿。证据 `item3-*.out`。

## 4. 可选的本地复验

### 4a saga 在真实 NATS 上的 nak / MaxDeliver

新增 `saga/consumer_nak_maxdeliver_real_integration_test.go`（`-tags integration`，共享环境的 NATS，流 `SAGANAK_<pid>_<ns>` 用后删除；
协调器存储用 mongotest）。用原生完成消费者 `SubscribeNestCompletions`，结果先到了还没有该定义版本的协调器（`ErrDefinitionMissing`，可重试）：

- 定义一直不来，`MaxDeliver=3`、`NakBackoffMin=200ms`：投递 3 次，间隔 202 ms / 402 ms（nak 带延迟、按次翻倍），第 3 次失败后 Term，
  再等一个 AckWait（3s）+1s 无第 4 次，consumer `NumAckPending=0`、`NumPending=0`，记录仍在等第 0 步；
- 定义在第 1 次投递之后上线：第 2 次投递被接收并 ack，记录推进到第 1 步，之后不再投递。

命令：`go test -tags integration -count=1 -race -v -run '^TestRealNatsCompletionNakBackoffAndMaxDeliver$' ./saga/` → PASS（`item4-saga-nats.out`）。
首次运行两个子用例共用一个 effect 前缀，第二个消费者（DeliverAll）读到了第一个子用例的消息（`saga: not found`，按终态 Term）——
用例隔离问题，改成每个子用例独立前缀后通过；它同时说明 `ErrNotFound` 按终态处理、不会无限重投，与 `isTerminalCompletionError` 一致。

### 4b Redis Cluster MOVED / ASK 上的 remote entity 读写与墓碑 WAIT

新增 `remoteentity/cluster_slot_migration_integration_test.go`（`TestMirrorLocalClusterSlotMigrationSnapshotReadWriteAndTombstone`，只在
`scripts/mirror-local.sh test-core` 起的私有 3 主 3 从 Cluster 上运行，不碰共享环境）。对 Remote 快照 L2（`remoteSnapshotL2Store`）：

- **ASK**（源 `MIGRATING`、目标 `IMPORTING`，键已 `MIGRATE` 到目标）：HGET 读、写脚本跟随 ASK 到目标（源上没有重建键）；墓碑脚本在源上回 ASK，
  按 `EvalReplicated` 的设计退回集群客户端普通发送一次、不 WAIT，计 `skipped`；墓碑落在目标上，之后旧版本写不能复活；
- **MOVED**（`SETSLOT NODE` 完成迁移，客户端槽位表仍指向源，`stale=true`）：墓碑同样 `skipped`、写读跟随 MOVED；槽位表刷新后墓碑 WAIT
  打到新主并被它的副本确认（`confirmed`）；
- 槽位迁回源后读写照常。最终 stats `{Confirmed:1 Skipped:2}`。

**顺带修的环境就绪缺口**：`scripts/mirror-local.sh` 的 `cluster_up` 只等 `cluster_state:ok`，刚建好的集群副本还在全量同步，`ROLE` 不列出它们，
墓碑 WAIT 按 `no_replicas` 跳过。既有 `TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary` 单独（或排第一个）跑时因此必红：

```
snapshot_l2_tombstone_wait_integration_test.go:319: WAIT calls on 127.0.0.1:37401 went up by 0, want 1 (the key's primary is 127.0.0.1:37401)
```

（完整 `^TestMirrorLocal` 顺序下前面的故障用例要跑十几秒，副本早已 online，所以以前是绿的。）加 `cluster_replicas_online`：每个主节点
`INFO replication` 至少一个 `state=online` 的副本，30s 上限。修后该用例单独跑 PASS（`item4-cluster-existing-only-{red,green}.out`）。
这是测试环境脚本的就绪判定，不是产品缺陷，不登记 RR。

命令与结果：`ROOST_MIRROR_LOCAL_HOME=<scratch> ROOST_MIRROR_LOCAL_CORE_RUN='^TestMirrorLocalClusterSlotMigration|^TestMirrorLocalTombstoneWaitOnClusterGoesToTheKeysPrimary$' scripts/mirror-local.sh test-core`
→ 两条 PASS；默认集合 `scripts/mirror-local.sh test-core`（`^TestMirrorLocal`，5 条）全部 PASS（`item4-mirror-test-core-all.out`）。

## 5. 故障矩阵预跑

MATRIX_PLACEHOLDER

## 验证

| 命令（`GOWORK=off`） | 结果 |
| --- | --- |
| `gofmt -l .` | 空 |
| `go vet -tags integration ./saga/ ./remoteentity/ ./kit/service/integration/` | 通过 |
| `go test -race -count=3 ./saga/` | ok |
| `go test -race -count=3 .`（根包，含 `TestExamplesRun`） | ok |
| `go test -count=1 .` | ok |
| `go build ./... && go vet ./...` | 通过 |
| `examples/`、`skill/examples/` 模块内 `go vet ./...` | 通过 |

改动不涉及 nest / entity / dataengine / sync 源码与生成形状，未跑 glsvet 与 codegen 生成链。

## 未验证与风险

- 门禁运行示例需要能下载示例模块依赖（与根模块同一批模块，CI 的 `go test ./...` 本就需要）；离线且模块缓存缺失时会在编译阶段失败而不是跳过——这是有意的，不静默放行。
- 4b 的 ASK 窗口只覆盖“键已搬到目标”的情形；“键还在源上、槽位处于 MIGRATING”时命令直接在源上执行，不涉及重定向，未单列。
