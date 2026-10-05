# N05 revn05 修复证据

审查 `be2713ad`（rebase 后），修复 `6f06f0da`（NC-130）、`c3475150`（NC-131）、`366058a7`（RR-20260913-01 残余）。macOS / Go 1.27.0 / Apple M5，`GOWORK=off`，模块根执行。修前红文本见[审查证据](../../../review/evidence/noncore-review-20261005-n05/README.md)。

## 修后（定向）

```
GOWORK=off go test ./remoteentity -count=1 -v -run 'TestLateReplicaOnColdNodeDoesNotPinL1BelowSharedL2|TestAuthoritativeLoadThatLosesTheL2RaceReturnsTheNewerSnapshot|TestStaleBackfillControls|TestL2ComparesVersionsExactlyBeyondFloatPrecision|TestRemoteSnapshotL2RejectsDelayedPublisher|TestDeleteWatermark'
--- PASS: TestLateReplicaOnColdNodeDoesNotPinL1BelowSharedL2 (same epoch / takeover 两个子用例)
--- PASS: TestAuthoritativeLoadThatLosesTheL2RaceReturnsTheNewerSnapshot
--- PASS: TestStaleBackfillControls (L2 断网降级 / 同版本重复发布 / Store 约定)
--- PASS: TestDeleteWatermarkHoldsInL2AgainstAnInflightLoadOnAnotherNode
--- PASS: TestDeleteWatermarkHoldsInL2AgainstALateReplicaOnAnotherNode
--- PASS: TestDeleteWatermarkControls (重建可见 / 旧删除不降水位 / 删除不清更新的 L2)
GOWORK=off go test ./kit/remoteentity -count=1 -run TestExpiredLocalInterestsDoNotKeepHealthFailing
--- PASS: TestExpiredLocalInterestsDoNotKeepHealthFailing
```

两条旧测试的 nil 期待改为 `errors.Is(err, cache.ErrStaleWrite)`（`TestL2ComparesVersionsExactlyBeyondFloatPrecision`、`TestRemoteSnapshotL2RejectsDelayedPublisher`），仍断言 L2 值不变；真实 Redis 的 `TestRealSnapshotL2KeyPrefixOnRedis` 与 Cluster 版同步改为“删除后只剩无 data 的墓碑”。

## 真实 Redis（隔离环境 `~/.roost-it/roost-dataengine-it`，只 source、未输出凭据；键前缀 `revn05:<pid>:<ns>`，用完逐键删除，结束时 `--scan revn05:*` 计数 0）

修前（`173738a0`（= rebase 后 `be2713ad`）的临时 worktree，复制新测试）：

```
older write on real Redis = <nil>, want cache.ErrStaleWrite
write v1 (any epoch) over tombstone 2 = <nil>, want cache.ErrStaleWrite
```

修后：

```
source env.sh; GOWORK=off go test -tags integration -race ./remoteentity -run '^TestRealSnapshotL2' -count=1 -v
--- PASS: TestRealSnapshotL2KeyPrefixOnRedis
--- SKIP: TestRealSnapshotL2KeyPrefixOnRedisCluster (无外部 Cluster)
--- PASS: TestRealSnapshotL2StaleWriteIsReportedAndNotPinnedInL1
--- PASS: TestRealSnapshotL2TombstoneFencesEveryWriter
ROOST_REMOTE_CLUSTER_IT=1 GOWORK=off go test -tags integration ./remoteentity -run '^TestRealSnapshotL2KeyPrefixOnRedisCluster$' -count=1 -v
--- PASS: TestRealSnapshotL2KeyPrefixOnRedisCluster (untagged keys=16 owners=3 / hash_tagged owners=1)
```

Cluster 由本包 harness 在临时目录自起 3 主 3 从，用例结束停止（`pgrep redis-server.*1738` 为空）。

## 矩阵

- `gofmt -l` 空；`go vet ./entity ./remoteentity ./kit/remoteentity ./cache` 与 `go vet -tags integration ./remoteentity` 通过。
- `go test -race -count=3 ./entity ./remoteentity ./cache ./kit/remoteentity`：每个修复提交各跑一次，均 ok。
- `go build ./... && go vet ./...` 通过；根包 `go test -count=1 .` ok；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` 通过；相邻 `./nest ./kit/nest ./kit/dataengine ./sync/syncbus/... ./codegen/internal/entity/...` ok。

未执行：`scripts/test-remote-generated.sh` 与故障矩阵（会写共享 `remote_entity` 库 / 注入故障，按共享环境规则不跑）；没有改生成形状，未跑生成 game-demo 编译。
