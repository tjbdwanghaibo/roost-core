# 985d5ba 修前负对照证据（自本机 /tmp 收入仓库）

`985d5ba` 的 bugfix 记录只写了“修前失败”，原文仅存在于作者机器的 `/tmp/roost-release-negative.log` 与 `/tmp/roost-review-before-observations.log`，
违反 roost-coding “下一位 agent 无需依赖某台机器的 /tmp 文件”。本文件原样收录这两份日志（无凭据），并附 2026-09-26 复验方在 `aaada47` 上的独立核对结论。

**基线说明**：作者的负对照基线是“`381efc9` + 当时未提交的 RR-03～09 修复”，不对应任何提交；复验方在 `aaada47`（修复前提交）上把修复方测试放回修前代码独立复跑，结论见文末。

## 1. /tmp/roost-release-negative.log（原文）

```text
--- FAIL: TestRemoteCloseFailureStillConfirmsEntitySync (0.00s)
    remote_close_confirmation_test.go:59: first reply (commit ok, close failed): nest: finish remote write batch: remote entity release is incomplete
    remote_close_confirmation_test.go:61: committed transaction left Sync held after Close failure
2026/09/26 14:48:25 ERROR nest after-commit callback panic err="lifecycle finalizer panic"
--- FAIL: TestRemoteAfterAdmissionPanicDoesNotAbortDurableCommit (0.00s)
    remote_committed_hook_test.go:46: reply=nest: transaction committed but after-commit work failed: lifecycle finalizer panic
    remote_committed_hook_test.go:47: WAL record committed: mutations=1; local value=11; remote batch Commit called=false Abort called=true
    remote_committed_hook_test.go:49: remote batch aborted after the transaction was durably committed
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/nest	2.009s
--- FAIL: TestAckSyncsAsyncRecordBeforeCheckpoint (0.03s)
    close_replay_test.go:32: checkpoint persisted before WAL data sync
--- FAIL: TestCloseRetainsDirectoryUntilExternalReplayReturns (0.02s)
    close_replay_test.go:58: directory reused while replay active
--- FAIL: TestOpenRuntimeZeroOptionsOwnsWAL (0.01s)
    close_replay_test.go:91: nestwal: directory is already locked
        resource temporarily unavailable
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/nestwal	1.034s
--- FAIL: TestCommittedReplayPreservesNewerLiveFenceAndDirtyState (0.00s)
    replay_outcome_test.go:57: remote entity persistence outcome is indeterminate
        remote entity: writer fenced
--- FAIL: TestMemoryLostCommitReplyKeepsFinalizerOwnership (0.00s)
    replay_outcome_test.go:82: unknown commit=context deadline exceeded
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/remoteentity	1.443s
2026/09/26 14:48:25 WARN syncbus mod: config section "sync" is deprecated, rename it to "room" key=prefix
2026/09/26 14:48:25 WARN syncbus mod: config section "sync" is deprecated, rename it to "room" key=transport
2026/09/26 14:48:25 WARN syncbus mod: config section "sync" is deprecated, rename it to "room" key=stream
2026/09/26 14:48:25 WARN syncbus mod: config section "sync" is deprecated, rename it to "room" key=ack_wait
2026/09/26 14:48:25 WARN syncbus mod: config section "sync" is deprecated, rename it to "room" key=replicas
--- FAIL: TestCanonicalSyncBusConfigAndLegacyPrecedence (0.00s)
    --- FAIL: TestCanonicalSyncBusConfigAndLegacyPrecedence/syncbus (0.00s)
        config_test.go:23: config ignored: &{bus:<nil> localSid:7 prefix:roost.room transport: jsCfg:{LocalSid:7 Prefix:roost.room Stream: Storage: AckWait:0 MaxDeliver:0 StreamMaxAge:0 Duplicates:0 Replicas:0 MaxBytes:0 SetupTimeout:0 PublishTime:0}}
    config_test.go:36: legacy overrode canonical config
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/kit/syncbus	2.508s
--- FAIL: TestReopenRefusesDrainingTransportLifetime (0.00s)
    reconnect_transport_test.go:39: open=<nil>; must refuse old queue
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/sync/entitysync	0.494s
--- FAIL: TestConsolidationMapSyncBusLegacyPaths (0.00s)
    syncbus_migration_test.go:31: missing roost-core/kit/syncbus: package wiring
        import (
         oldkit "github.com/tjbdwanghaibo/roost-core/kit/room"
         olddriver "github.com/tjbdwanghaibo/roost-core/room"
        )
        var _ = oldkit.NewRoomMod
        var _ oldkit.RoomMod
        var _ = olddriver.NewNatsSyncBus
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/codegen/internal/roost	3.041s
FAIL
```

## 2. /tmp/roost-review-before-observations.log（原文，RR-07 / RR-09 修前观察）

```text
--- FAIL: TestQueueStatsAndAdmissionCountRunningContinuation (0.00s)
    dispatch_queue_test.go:328: admission=<nil>
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/nest	0.559s
--- FAIL: TestRemoteProjectionFailureOnlyAcknowledgesPrefixAndReplaysSuffix (0.05s)
    projector_remote_test.go:103: missing failed transaction: dataengine projector: remote window stopped after 1/4 records: context deadline exceeded
--- FAIL: TestReplayBudgetCountsConsumedRecordsAcrossSegments (0.47s)
    --- FAIL: TestReplayBudgetCountsConsumedRecordsAcrossSegments/records (0.22s)
        replay_read_budget_test.go:110: consumed=2 replayed=1
    --- FAIL: TestReplayBudgetCountsConsumedRecordsAcrossSegments/bytes (0.25s)
        replay_read_budget_test.go:110: consumed=2 replayed=1
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/dataengine/engine	2.274s
FAIL
```

## 3. 复验方在 aaada47 上的独立核对（2026-09-26）

| RR | 修复方测试在修前是否为真正的红 | 复验补证 |
| --- | --- | --- |
| 03、08、11、12、13、14、20、24 | 是；失败文本与上面日志逐字一致 | — |
| 06 | 否：修前编译失败（缺 `fctx.InFastWorker/WithFastWorker/ErrBlockingInFastWorker`） | 只用旧 API 的等价探针：冷访问修前 `err=<nil>` 且 loader 执行，修后拒绝 |
| 07、09 | 是（`consumed=2 replayed=1`；`missing failed transaction`） | — |
| 10 | 生成测试是（`stale reload: coins=10000 version=1`）；单元测试修前编译失败 | 真实 Mongo 等价探针：修前重启恢复 fatal、删档复活；修后 10/10 |
| 16 | `TestAckSyncsAsyncRecordBeforeCheckpoint` 是；`TestMultiCheckpointCancellation…` 修前即绿（守卫，不是负对照） | 模拟断电探针：修前重开 `acknowledgement is beyond recovered WAL end`，修后成功 |
| 17 | WAL 层用例是；Projector 用例修前为挂起超时；**`TestCommitterCloseWaitsForExternalFlush` 修前 100/100 通过、删修复仍通过——不是负对照** | Runtime / Committer 等价探针：修前 checkpoint 回退，修后 20/20 |
| 18 | 是（行号 91 对当前 94：负对照用的是提交前旧版测试） | — |
| 19 | 部分：准入用例使用空 RemoteCommit，修前即被 WAL 规范化拒绝，不是负对照；store / status 用例是 | 合法 commit 探针：修前三入口全部准入，修后拒绝 |
| 21 | 单测修前编译失败 | 修复方 flow_test 放回修前：真实 Mongo/Redis/NATS 三策略报 `production Backend wiring lost parallel projection capability` |
| 22 | 弱：新库并发修前仅 2/30 失败；上限用例修前也通过 | 集合已存在无计数文档：修前 30/30 失败、修后 0 |
| 23 | 测试修复，无需修前红；FatalSuffix 旧测试修前 race 200 次失败 50 次 | 修后 `-race -count=500` 0 失败 |

回归测试本身都在仓库中可复跑；上述“不是负对照”的用例由 RR-20260926-17 / 19 / 22 复核补修加强。
