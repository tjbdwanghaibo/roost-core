# A3 共用停机类型与契约骨架：证据（2026-10-05）

基线 `origin/main` `d346b45c`，分支 `a3stop`。方案与实施：[REFACTOR-2026-10-05-shared-stop-contract](../../../feature/REFACTOR-2026-10-05-shared-stop-contract.md)。
codebase-memory 图谱代际 2026-09-30、指向主 checkout，涉及文件均为 `metadata_changed`，结论以 worktree 当前源码为准。

## 1. 迁移是重构：原有回归保证行为不变

三份副本（bus / syncbus driver / mirror）改用 `internal/operation.Lifetime` 后，原有回归 NC-90（`bus/jetstream_stop_promises_test.go`）、NC-172（`sync/syncbus/driver/jetstream_stop_*_promises_test.go`，含按栈帧 `jetStreamSyncBus).StopWithContext` 探测等待的用例）、NC-174（`sync/syncbus/mirror/stop_drain_promises_test.go`、`remoteentity/assembly_replica_drain_promises_test.go`，按栈帧 `mirror.(*Replicator).StopWithContext` 探测）不改一行，`-race -count=3` 全部通过：[verify.txt](verify.txt)。真实依赖复跑（syncbus / etcd 自起私有进程，bus 用隔离环境 NATS）：[real-deps.txt](real-deps.txt)。

## 2. 骨架能抓住已知错法（先红后绿）

| 对象 | 红 | 绿 |
| --- | --- | --- |
| 故意写错的对象：清空字段当成已停 | [skeleton-red.txt](skeleton-red.txt)：`retry Stop while the work is still in flight = <nil>`、`Stop returned without releasing the resource` | `TestCheckCatchesKnownWrongStoppers` 断言三种错法（清空字段、排空前释放、裸接收不看 ctx）都红在对应承诺上；`TestCheckPassesAStopperBuiltOnTheSharedLifetime` 绿 |
| manager Engine（NC-170 修前 `c99a687d^`） | [nc170-manager-prefix-red.txt](nc170-manager-prefix-red.txt) | `TestEngineStopContract` |
| kit/nest Mod（NC-171 修前） | [nc171-nest-prefix-red.txt](nc171-nest-prefix-red.txt) | `TestNestModStopContract` |
| etcd Discovery.Deregister（NC-173 修前） | [nc173-discovery-prefix-red.txt](nc173-discovery-prefix-red.txt)：`the stop ignored its context` | `TestDiscoveryDeregisterStopContract` |
| etcd Assembly.Close（NC-173 修前） | [nc173-assembly-prefix-red.txt](nc173-assembly-prefix-red.txt) | `TestAssemblyCloseStopContract` |
| remoteentity Assembly（NC-174 修前 assemble.go + envelope.go） | [nc174-remoteentity-prefix-red.txt](nc174-remoteentity-prefix-red.txt) | `TestRemoteAssemblyStopContract` |
| bus JetStream RPC（NC-90 修前 `ae742984^`） | [nc90-bus-prefix-red.txt](nc90-bus-prefix-red.txt) | `TestJetStreamRPCStopContract` |
| 生成 TCP Server / Mod（NC-83 修前形状，夹具副本里改回） | [nc83-generated-tcp-prefix-red.txt](nc83-generated-tcp-prefix-red.txt) | `TestGeneratedPlayerTCPStopContract`（codegen） |
| syncbus JetStream、mirror Replicator | 修前没有 `StopWithContext`，骨架用例无法在修前编译；修前红见 [stopshape nc172-red](../noncore-bugfix-20261005-stopshape/nc172-red.txt) / [nc174-red](../noncore-bugfix-20261005-stopshape/nc174-red.txt) | `TestJetStreamSyncBusStopContract`、`TestReplicatorStopContract` |

“修前红”的做法：把对应实现文件临时换成修前提交的版本（`git show <rev>:<file>`），跑同一个契约用例，抄下输出后换回；生成 TCP 是在夹具副本里把生成的 `server_gen.go` 改回修前形状（临时用例，取证后删除）。

## 3. 骨架发现的新问题（NC-173 复核补修）

`TestAssemblyCloseStopContract` 第 4 步在当前 main 上红：停完后再 `Close` 返回 `context canceled`（`clientv3.Client.Close` 不幂等）。红 [nc173-close-idempotent-red.txt](nc173-close-idempotent-red.txt) → 修（`driver.Client.Close` 只关一次）→ 绿 [nc173-close-idempotent-green.txt](nc173-close-idempotent-green.txt)。记录见 [NC-173 复核后的补修](../../RR-20261005-NC-173.md#复核后的补修2026-10-05a3-停机契约骨架发现)。

## 4. glsvet 停止入口提示

[glsvet-stophints.txt](glsvet-stophints.txt)：NC-173 修前 `discovery.go` 命中 `Deregister(ctx) calls waitLoopDone ...`，修后与全仓非测试文件 0 条，退出码均为 0（提示不计入违例）。Mutex.Lock 提示实测 33 处全为短临界区，没有加（理由见方案文档）。

## 复跑

```
GOWORK=off go test -race -count=3 -run 'StopContract|Lifetime' ./internal/... ./bus/ ./sync/syncbus/... ./manager/ ./kit/nest/ ./etcd/driver/ ./remoteentity/
GOWORK=off go test -count=1 -run TestGeneratedPlayerTCPStopContract ./codegen/internal/roost/
GOWORK=off go test -count=1 ./cmd/glsvet/ && GOWORK=off go run ./cmd/glsvet ./...
```
