# N05 revn05 审查证据

基线 `a80be80c`，macOS / Go 1.27.0，`GOWORK=off`。命令从模块根执行。

## NC-130 修复前红

```
GOWORK=off go test ./remoteentity -count=1 -v -run 'TestLateReplicaOnColdNodeDoesNotPinL1BelowSharedL2|TestAuthoritativeLoadThatLosesTheL2RaceReturnsTheNewerSnapshot|TestStaleBackfillControls'
--- FAIL: TestLateReplicaOnColdNodeDoesNotPinL1BelowSharedL2/same_epoch,_newer_version_in_L2
    consistency 2 read version=1 route=1 payload="v1" after a late replica; L2 holds version=2 route=1 — L1 was pinned below the shared layer
--- FAIL: TestLateReplicaOnColdNodeDoesNotPinL1BelowSharedL2/takeover:_newer_route_epoch_in_L2
    consistency 2 read version=5 route=1 payload="v1" after a late replica; L2 holds version=6 route=2 — L1 was pinned below the shared layer
--- FAIL: TestAuthoritativeLoadThatLosesTheL2RaceReturnsTheNewerSnapshot
    monotonic read returned version=1 payload="v1"; L2 already held version 2
--- PASS: TestStaleBackfillControls/L2_outage_still_degrades_to_L1
--- PASS: TestStaleBackfillControls/identical_republish_is_accepted
--- FAIL: TestStaleBackfillControls/L2_store_reports_a_refused_older_write_as_ErrStaleWrite
    older write = <nil>, want cache.ErrStaleWrite (cache.Store contract)
```

（`consistency 2` 是 `RemoteReadCached`。）

## NC-131 修复前红

```
GOWORK=off go test ./kit/remoteentity -count=1 -v -run TestExpiredLocalInterestsDoNotKeepHealthFailing
    Stats().LocalInterests = 4 after every interest expired, want 0
    health after every interest expired = fail (capacity exhausted wrappers=0 local_interests=4 transactions=0 active_transactions=0), want ok
--- FAIL: TestExpiredLocalInterestsDoNotKeepHealthFailing
```

## RR-20260913-01 残余（跨节点 L2 删除水位）修复前红

```
GOWORK=off go test ./remoteentity -count=1 -v -run TestDeleteWatermark
--- FAIL: TestDeleteWatermarkHoldsInL2AgainstAnInflightLoadOnAnotherNode
    L2 after the delete at v2: held=true version=1 err=<nil> — the deleted snapshot was written back
--- FAIL: TestDeleteWatermarkHoldsInL2AgainstALateReplicaOnAnotherNode
    L2 after the delete at v2: held=true version=1 err=<nil> — the deleted snapshot was written back
--- FAIL: TestDeleteWatermarkControls/recreate_above_the_watermark_is_visible
    a snapshot at the delete's own version reached L2: held=true err=<nil>
--- PASS: TestDeleteWatermarkControls/delete_never_removes_a_newer_L2_snapshot
```

最初的临时探针（已删除，未提交）：owner 发布 v1 → 节点 B 的权威加载在 owner `DeleteAtVersion(2)` 之后返回 v1 → 冷节点 C：`found=true version=1`，owner 自己 `found=false`。

## 真实 JetStream 重放探针（O5，临时，已删除）

隔离环境 `~/.roost-it/roost-dataengine-it` 的 NATS（只 source env，未输出凭据），独立前缀 `revn05.p<ns>.sync`、内存存储，用 `-tags integration -run '^TestZZProbeRevn05JetStreamReplay$'` 单独运行（不触发共享故障注入用例）。owner / consumer / 迟到节点各自 `NewJetStreamSyncBus` + `Manager.BindSync`：

```
owner sees consumer interest: true
consumer got v1
late joiner (never read the key) holds replayed v1 payload="v1"
late joiner replayed history into L1: true; late joiner registry sees consumer interest (replayed): true
delete stream REVN05_P15131000_SYNC: <nil>
```

结论：新 sid 的 durable（DeliverAll）在启动时重放保留期内的快照与兴趣，即使它从未读过该 key。流在用例结束时删除（durable 随流删除）。

## 图谱

`Users-whb-roost-roost-core`，generation `2026-09-30T11:28:30Z`，26 证据路径与 3 个 scope 无记录缺口，10 个 metadata_changed 以当前源码补证；`entity.Publish` 入边漏 `remoteentity.afterRemoteCommit`，以源码补证。
