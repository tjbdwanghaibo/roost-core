# 同形停机 6 处核实与修复（stopshape，2026-10-05）

基线 `origin/main` `25bb0d66`（NC-83 记录的扫描基于更旧的 `3d4fe9f3`），修复提交 rebase 到 `c35fe606` 之后。规则：roost-coding 生命周期“三步停机”（①发起关闭幂等 → ②在调用方 ctx 内等待排空、可重试 → ③排空后才释放）。codebase-memory 图谱代际 2026-09-30 且指向主 checkout，相关文件均为 `metadata_changed` / 未索引，结论全部以 worktree 当前源码为准。

## 逐处结论

| # | 位置 | 结论 | 编号 |
| --- | --- | --- | --- |
| 1 | `manager/engine.go` `stopStarted` | 确认，本次修复 | [NC-170](../../RR-20261005-NC-170.md) |
| 2 | `kit/nest/nest_mod.go` `stopUnloadResync` | 形状确认（正式路径触发窗口窄），本次修复 | [NC-171](../../RR-20261005-NC-171.md) |
| 3 | `sync/syncbus/driver/jetstream.go` `Stop` | 确认（真实 NATS 复现），本次修复 | [NC-172](../../RR-20261005-NC-172.md) |
| 4 | `bus` JetStream RPC 订阅 | **已由 NC-90（`ae742984`）修掉**，不登记 | 见下 |
| 5 | `etcd/driver` `Deregister` / `Assembly.Close` | 确认（真实 etcd 复现），`b67d5945` / NC-93 未覆盖，本次修复 | [NC-173](../../RR-20261005-NC-173.md) |
| 6 | `remoteentity` replicator | 确认，本次修复（只改停机生命周期） | [NC-174](../../RR-20261005-NC-174.md) |

正式调用方：App 的 Mod 停机（`app/app.go` `stopModsReverseBefore` / `stopModSafely`）对每个 Mod **只调用一次**、不重试；返回 ctx 错误即判定不完整、中断并保留后续 Mod。所以“重试报成功 / 重试必然失败”只影响直接调用方；“停机报成功而工作仍在跑、依赖被提前释放”在 App 的一次停机里就会发生的是 #1（过期 ctx 下继续停依赖）、#3、#6。

### #4 为什么不构成（已修）

`3d4fe9f3` 是 `ae742984` 的祖先（`git merge-base --is-ancestor` 确认）。当前 `bus/bus.go` `stopResources` 第一步 `stopJetStreamRPCRequests` 关准入并停请求消费者，之后才排空 pool、`waitJetStreamRPCRequests(ctx)` 等在途 handler，最后停响应消费者；`awaitStop` 重试依次再等 pool 与 handler。“不等 handler”和“池排空期间仍接收新请求”两点都已覆盖（准入关闭后的投递返回 `errJetStreamRPCStopping` → NAK）。NC-90 的三条包内回归在当前 main 上复跑通过：[item4-nc90-recheck.txt](item4-nc90-recheck.txt)。审查结论见 [N03 记录](../../../review/REVIEW-2026-10-05-noncore-n03.md)。

## 红绿

| 编号 | 修前红 | 修后绿 |
| --- | --- | --- |
| NC-170 | [nc170-red.txt](nc170-red.txt) | [nc170-green.txt](nc170-green.txt) |
| NC-171 | [nc171-red.txt](nc171-red.txt) | [nc171-green.txt](nc171-green.txt) |
| NC-172 包内 | [nc172-red.txt](nc172-red.txt) | [nc172-green.txt](nc172-green.txt) |
| NC-172 真实 NATS | [nc172-real-red.txt](nc172-real-red.txt) | [nc172-real-green.txt](nc172-real-green.txt) |
| NC-173 包内 | [nc173-red.txt](nc173-red.txt) | [nc173-green.txt](nc173-green.txt) |
| NC-173 真实 etcd | [nc173-real-red.txt](nc173-real-red.txt) | [nc173-real-green.txt](nc173-real-green.txt) |
| NC-174 | [nc174-red.txt](nc174-red.txt) | [nc174-green.txt](nc174-green.txt) |

红都来自现有 API 上的行为断言（`Stop()` / `Stop(ctx)` / `Close(ctx)` / `StopWithContext(ctx)`），不是编译失败；新增 API（syncbus `StopWithContext`、mirror `StopWithContext`）的用例只有绿。真实依赖的红是把对应实现文件 `git stash` 回基线后跑同一个用例。阻塞一律用 channel / 测试持有的锁控制；需要确认“停止正在等待”时用有界的 goroutine 栈探测，不用 sleep 制造顺序。

真实依赖都由测试自起私有进程（`nats-server -js`、`etcd`），不使用共享隔离环境；共享环境的发版故障矩阵锁（`remote-acceptance.lock`）消失后才运行。

## 复跑

```
GOWORK=off go test -count=1 -run 'TestEngineStopRetryWaits|TestEngineConcurrentStopWaits' ./manager/
GOWORK=off go test -count=1 -run TestNestModStopRetryKeepsUnloadResync ./kit/nest/
GOWORK=off go test -count=1 -run 'TestJetStreamSyncBusStop' ./sync/syncbus/driver/
GOWORK=off go test -count=1 -run TestSyncModStop ./kit/syncbus/
GOWORK=off go test -tags integration -count=1 -run TestRealJetStreamSyncBusStopDrainsAnInFlightHandler ./kit/syncbus/
GOWORK=off go test -count=1 -run 'TestDiscoveryDeregisterWaitsForTheRegistrationLoopWithinItsContext|TestAssemblyCloseKeepsTheClientUntilDeregisterSucceeds' ./etcd/driver/
GOWORK=off go test -tags integration -count=1 -run TestRealEtcdAssemblyCloseRetriesTheRevokeAfterAFrozenBudget ./etcd/driver/
GOWORK=off go test -count=1 -run 'TestRemoteAssemblyStopWaitsForAnInFlightReplicaHandler' ./remoteentity/
GOWORK=off go test -count=1 -run TestReplicatorStopWithContext ./sync/syncbus/mirror/
```

## 整体验证（GOWORK=off，rebase 后）

见本目录 [verify.txt](verify.txt)：`gofmt -l` 空；改动包 `go vet`；`go test -race -count=3` 改动包（manager、kit/manager、kit/nest、kit/syncbus、sync/syncbus/...、etcd/...、kit/etcd、remoteentity、kit/remoteentity、cache）；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync`；根包 `go test -count=1 .`；`go build ./... && go vet ./...`；导入改动包的 `kit`、`dataengine/engine`、`app` `go test -count=1`；`go vet -tags integration ./kit/syncbus/ ./etcd/driver/`。

## 方向判断（同形问题反复出现）

近期同一不变量（“停止返回 = 回调已静止、资源可释放；重试收敛”）的修复链：RR-20261004-07 / 08（Bus pool、连接 drain）、NC-04（Ops）、NC-09（RPC 回调）、NC-83（player TCP）、NC-90（Bus JetStream）、本轮 NC-170～174。归成三类：

1. **“字段已清空 / 列表已取走 = 已停”**：NC-170、NC-171、NC-173 的 `loopDone = nil` 与 `Close` 先关 client、NC-83。
2. **退订不等在途回调**：NC-90、NC-172、NC-174。根因在传输契约：nats.go `ConsumeContext.Stop` 与 `ISyncBus.Subscribe` 返回的 `func()` 都不等在途回调，于是每个订阅者各自补一个“准入 + 在途计数 + idle 通道”。仓内现在有三份几乎相同的实现（`bus/jetstream_rpc.go`、`sync/syncbus/driver/jetstream.go`、`sync/syncbus/mirror/envelope.go`），加上 `worker.Pool`。
3. **等待不看 ctx**：普通 Mutex、裸 `<-done`（NC-173、NC-83 的会话关闭派发器）。

判断：是实现方向问题，不是个别疏漏——停止语义散在每个使用方各写一遍，没有一处能统一保证。建议：

- **共用小类型值得做了**（与 NC-83 时“单一使用方不抽 helper”的结论不同：第 2 类已有三份副本）。候选：一个低层包里的准入门（`Enter() bool` / `Leave()` / `Close()` / `Wait(ctx) error`），替换上述三份；代价是一次跨 bus / syncbus / mirror 的机械替换与回归。
- **更根本的方向**：把排空责任下沉到传输——`ISyncBus` 增加带 ctx 的退订（`func(ctx) error`，返回时本订阅的在途回调已返回），nats driver 的 JetStream 订阅提供 `StopWithContext`。订阅者不再各自计数。代价：ISyncBus 是公开接口，需要兼容期（旧 `func()` 保留为“只发起”）。
- **lint 只适合做复审提示，不宜做门禁**：可在 glsvet 加两条启发式——停止类函数（`Stop*` / `Close*` / `Shutdown*` / `Deregister*`，带 ctx 参数）里出现不在 `select` 中、没有 `ctx.Done()` 分支的裸通道接收或 `sync.Mutex.Lock`；以及 `err := f(ctx)` 之后未判断 err 就把同一个字段置 nil。第一条误报低，第二条误报较多，建议先只报告不失败。
- 测试侧更有效的是一个共用的停机契约用例骨架（首次超时 → 放行阻塞工作 → 新 ctx 重试 → 资源确实释放、再调用返回 nil），新增停止入口按它写回归。
