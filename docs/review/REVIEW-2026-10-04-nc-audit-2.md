# B 线 NC-08～12 修复独立复审（审计 2）

2026-10-04，审计对象 tag `v1.19.0` = `74e1ba39`。修复提交为 `3560a19b`（NC-08～10）和 `1502f973`（NC-11/12），`114d906d` 只做 tidy。只读审查：没有改仓库文件，也没有提交。所有试验都在临时 worktree 里做，用完已经 `git worktree remove --force`。审计用的两份测试草稿保存在会话 scratchpad 的 `audit-nc2-drafts/` 目录，没有入库。

## §0 范围与方法

- **范围**：RR-20261004-NC-08（JetStream 无 handler 拒绝回包的版本 envelope）、NC-09（RPC 停止预算与回调排空的所有权）、NC-10（发现与传输共用调用预算）、NC-11（竞选 setup 取消与长期 session 分离）、NC-12（watch callback 唯一收尾与可取消等待）。NC-01～07 和 NC-13～29 由另外两位审计员负责，本文不涉及。
- **读过的材料**：`AGENTS.md`、roost-coding 契约；[bug 登记 06](../bug/REVIEW-2026-10-04-noncore-06.md)（NC-08～10）和 [bug 登记 08](../bug/REVIEW-2026-10-04-noncore-08.md)（NC-11/12）；[NC-08](../bugfix/RR-20261004-NC-08.md)～[NC-12](../bugfix/RR-20261004-NC-12.md) 五份修复记录；[证据 04](../bugfix/evidence/noncore-bugfix-20261004-04/README.md) 和 [证据 05](../bugfix/evidence/noncore-bugfix-20261004-05/README.md) 的 README、summary 和 jsonl；两个修复提交的完整 diff；CHANGELOG 的 `## [v1.19.0]` 段。
- **图谱**：已对 15 个所引文件调用 `check_index_coverage`。索引代次是 2026-09-30，在两个修复提交之前，所以 `jetstream_rpc.go`、`nats/driver/{rpc,assembly}.go`、`kit/nats/nats_mod.go`、`servicerpc/client.go`、`etcd/driver/election.go`、`etcd/{watch_callback,watcher}.go`、`app/app.go` 都标为 `metadata_changed`。这些文件**全部直接读 v1.19.0 源码**，结论不依赖图谱的边。没有重建共享索引。
- **etcd SDK 源码**：直接读模块缓存里的 `go.etcd.io/etcd/client/v3@v3.7.1/concurrency/{session,election}.go`。
- **运行环境**：macOS、`GOWORK=off`。真实 etcd 用的是 `/opt/homebrew/bin/etcd`：单节点，每个测试启动一个临时实例，测试结束就杀掉。没有使用 NATS 隔离环境，也没有碰 `/tmp/roost-dataengine-it` 和 `ROOST_*` 流。

## §1 逐条表

| RR | 记录与代码一致 | 回归在包里、v1.19.0 通过 | 回退实现会变红 | 契约 / 新问题 | 结论 |
| --- | --- | --- | --- | --- | --- |
| NC-08 | 一致：`bus/jetstream_rpc.go:404` 改为 `encodeRPCFailure` | `bus` 中 4 个 `TestRPCBudget*` 通过 | 是（§2） | 同类入口已查：`bus.go:673/808/849/858` 早已使用 envelope，`rpcErrorResponse` 不再有裸 Marshal。wire 版本没有变。新服务端对旧客户端兼容；旧服务端对新客户端仍是修前的错误，属于既有问题，不是回归 | 通过 |
| NC-09 | 部分一致：`nats/driver/{rpc,assembly}.go` 与记录相符；**Kit 层“保留引用供再次排空”的承诺不成立**（§3-D2） | `nats/driver` 7 项、`kit/nats` 通过 | 是（§2） | 新缺陷 D2；疑点 S1（WaitGroup 时序和注释）、S2（callback 内的 `Stop()` 由立即返回变成永久等待） | **有缺陷** |
| NC-10 | 一致：`servicerpc/client.go:163–182` | `servicerpc` 6 项通过 | 是（§2） | `c.timeout` 在构造时把 ≤0 归为 3s，不会出现 0 deadline。发现与传输的组合只有 `CallDiscoveredChecked` 一处，codegen 模板不调用 `PickServer` | 通过 |
| NC-11 | Grant 取消、长期 session 分离与记录一致；但失败清理**先 cancel lifetime 再 Close** 带来回归（§3-D1） | `etcd/driver` 的 `TestEtcdLifetime*` / `TestElection*` 通过 | 是（§2） | 新缺陷 D1：失败或取消竞选后 lease 不再 Revoke，候选键或领导键残留到 TTL（默认 60s）；修前能即时 Revoke | **有缺陷** |
| NC-12 | 一致：`etcd/watch_callback.go` 的唯一关闭任务和 Done 等待双方退出 | `etcd` 4 项通过 | 是（§2） | 核心 `watcher.Close` 和 `serviceWatcher.Close` 都返回 nil，所以干净关闭时 `Err()` 仍为 nil；行为收紧已写进 CHANGELOG。仓内没有 `WatchCallback` 的生产调用方 | 通过 |

**统计**：5 条中 3 条通过（NC-08、10、12），2 条有确定缺陷（NC-09、11）；5 条回退实现后都确认变红。

**全局检查**：
- `GOWORK=off go test -count=1 -race ./bus ./nats/driver ./kit/nats ./servicerpc ./etcd ./etcd/driver ./kit/etcd` 全部 ok（`kit/etcd` 没有测试）。
- 上述 7 个包 `go vet` 为 0。
- `go mod tidy -diff` 无差异。grpc 只被 `etcd/driver/election_lifetime_promises_test.go` 直接引用，作为直接依赖成立。
- `GOWORK=off go test -run TestCoreDependencyBoundary .` 通过。
- CHANGELOG `## [v1.19.0]` 的第 12 行和第 14 行已收录 NC-11/12 与 NC-08/09/10，含 `RPCClient.StopWithContext`、Assembly/Kit 保留资源和 WatchCallback 返回 Close 错误这几项行为变化。

## §2 红是否真红

在 `v1.19.0` worktree 里逐项回退实现部分，测试文件保持不动。所有变红都是行为断言失败，不是编译失败。

1. **NC-08**：把 `jetstream_rpc.go:404` 改回 `b.codec.Marshal(rpcErrorResponse(...))`。
   ```
   rpc_envelope_promises_test.go:50: wire={"code":1,"reason":"server error"} decode_error=bus: unsupported rpc response version 0
   --- FAIL: TestRPCBudgetJetStreamResponsesUseTheClientEnvelope/missing
   rpc_envelope_promises_test.go:118: formal CallReliable lost remote refusal: bus: unsupported rpc response version 0
   --- FAIL: TestRPCBudgetMissingHandlerRetainsPublishAndMarshalFailures/marshal   (cause lost: <nil>)
   ```
   success、business_error 和 panic 三个对照仍为绿。

2. **NC-09**：把 `assembly.go:62` 改回 `a.RPC.Stop()`。
   ```
   rpc_stop_budget_promises_test.go:48: returned_before_callback_release=false close_error=<nil>
   --- FAIL: TestRPCBudgetAssemblyCloseBoundsCallbackDrain (0.10s)
   ```
   `TestRPCBudgetStopRetainsFullQueueFallbackAndAllowsRetry` 在回退后**挂到 `-timeout` 才结束**，没有自己的失败断言。见 §4-S4。

3. **NC-10**：把 `CallDiscoveredChecked` 改回 PickServer(ctx) 后接 CallChecked(ctx)。
   ```
   discovery_budget_promises_test.go:35: configured_timeout=20ms discovery_deadline_present=false
   discovery_budget_promises_test.go:70: configured_budget=20ms observed_wait=152.21775ms discovery_budget=149.994708ms
   --- FAIL: ...DiscoveryPickerTransportShareDeadline/{nil_context,normal}
   ```
   `TestRPCBudgetLateDiscoveryDoesNotStartTransport` 同样挂到超时。

4. **NC-11**：把 `election.go:67` 改回 `concurrency.NewSession(e.cli)`。
   ```
   election_lifetime_promises_test.go:168: returned_after_campaign_cancel=false err=context canceled
   --- FAIL: TestEtcdLifetimeCampaignCancellationReachesSessionGrant (0.11s)
   election_lifetime_promises_test.go:172: returned_after_campaign_cancel=false err=context deadline exceeded
   --- FAIL: TestEtcdLifetimeCampaignDeadlinePreservesCause (0.35s)
   ```

5. **NC-12**：把 `startWatcherClose` 改回在 Once 内同步调用 `watcher.Close()`。
   ```
   watch_lifetime_promises_test.go:148: returned_before_watcher_release=false err=context canceled
   --- FAIL: TestEtcdLifetimeCallbackCloseBudgetIncludesOwnedWatcher (0.10s)
   watch_lifetime_promises_test.go:51: timed out waiting for canceled close wait
   --- FAIL: TestEtcdLifetimeConcurrentCloseWaitsForHandlerAndWatcherAndPreservesError (5.00s)
   ```

## §3 确定缺陷

按要求不登记编号，等级只是建议。

### D1（建议 P2）：竞选失败或取消的清理先取消 session lifetime，SDK 的 Revoke 用这个已取消的 ctx 立即失败，候选键或领导键残留到 TTL

**现象**：`election.Campaign` 在 session 创建后的任一失败分支，都不会再即时撤销 lease。lease 和挂在上面的竞选键会一直留到 TTL（SDK 默认 60s）才过期。

真实 etcd v3.7.1 单节点实测，v1.19.0 上：
```
campaign_err=audit: lost watcher waiting for delete
lease_ttl_after_failed_campaign=59 ttl_err=<nil> candidate_keys=1
... "method":"/etcdserverpb.Lease/LeaseRevoke","error":"rpc error: code = Canceled desc = context canceled"
--- FAIL: TestAuditCampaignFailureCleanupRevokesLease
```
同一测试换成修前的 `1502f973^:etcd/driver/election.go`：
```
lease_ttl_after_failed_campaign=-1 candidate_keys=0
--- PASS
```
这说明是这次修复引入的**回归**。

成功与取消竞争的分支（修复刻意加的 `!stopCancellation() || ctx.Err() != nil` 分支）也是这样：
```
first_campaign_err=context canceled is_leader=false
other_campaign_err=context deadline exceeded other_is_leader=false residual_keys_before=1
--- FAIL: TestAuditCampaignSuccessCancelRaceDoesNotLeavePhantomLeader (3.31s)
```
第一个候选已经在 etcd 里写下最低 revision 的领导键，却向调用方返回失败。keepalive 已停，键不删除，其他候选在 TTL 内都选不上，集群在这段时间里没有真正的领导者。

**触发条件**：
- (a) `elect.Campaign` 返回非 ctx 错误，而 Txn 已经写入。例如 SDK `waitDeletes` 的 "lost watcher waiting for delete"（压缩或 watch 关闭），或者 Txn 结果未知。
- (b) caller 在 `elect.Campaign` 成功和 `stopCancellation` 之间取消（成功与取消竞争）。
- (c) 发布前发现 `e.session != session || !e.campaign`。
- (d) caller 在竞选等待中取消。SDK 自己会用 `client.Ctx()` 删除候选键，但 lease 仍不 Revoke，残留一个空 lease 到 TTL。周期性超时重试的候补节点会因此累积孤儿 lease。

**根因**：`1502f973` 中 `etcd/driver/election.go` 第 132/138/147/154 行（以及第 122 行）在 `session.Close()` 前显式调用 `cancelSession()`。SDK `Session.Close()` 的实现是 `Orphan()` 之后执行 `client.Revoke(context.WithTimeout(s.opts.ctx, ttl))`，而 `s.opts.ctx` 正是传给 `concurrency.WithContext` 的 `sessionCtx`，所以 Revoke 必然以 Canceled 立即失败。

`campaignSession.Close`（第 187–195 行）本来就是先调用 SDK Close、再 cancel。它的注释也写明要防止 Revoke 被取消。也就是说，正常 Resign 路径保住了 Revoke，失败路径却自己先把它取消了。

修复记录 NC-11 §兼容与边界只写了“失败清理使用已取消 lifetime……没有证明 lease 已即时删除”，把这一点当作未验证项。它没有指出这一行为比修前退化，也没有说明残留的是**领导键**，会阻塞其他候选。

**会红的测试草稿**：见 `audit-nc2-drafts/audit_nc11_cleanup_integration_test.go`，build tag 为 `integration`，复用 `real_etcd_promises_test.go` 的 `startEtcd`。核心逻辑如下：
```go
e := &election{cli: cli, prefix: prefix, leaderCh: make(chan struct{})}
e.create = func(ctx context.Context) (electionSession, electionBackend, error) {
    s, b, err := e.createWithEtcd(ctx)
    lease = s.(*concurrency.Session).Lease()
    return s, &auditFailAfterPut{electionBackend: b, err: errors.New("lost watcher")}, err
}
_ = e.Campaign(context.Background(), "a")      // 真实 Txn 写键后报非 ctx 错误
ttl, _ := cli.TimeToLive(ctx, lease)            // 期望 TTL==-1 且前缀下 0 个键
```
运行命令：`PATH=/opt/homebrew/bin:$PATH GOWORK=off go test -tags integration -race -run TestAudit ./etcd/driver`。

**候选修法**：
1. 第 122/132/147/154 行删除 Close 前的 `cancelSession()`，让 `campaignSession.Close` 按“先 SDK Close（Revoke 用活的 lifetime）、后 cancel”的既有顺序执行。caller 仍在等待时，Revoke 受 SDK 的 TTL 上限约束，和修前一样。
2. caller 已取消的分支（AfterFunc 已经取消 sessionCtx）改为调用 `Orphan()`，再由 election 自己持有一个**唯一、有界**的 Revoke 任务：用 `cli.Ctx()` 派生、带短超时，下一次 Campaign 或 Resign 可以等待它。不能每次调用丢一个后台 goroutine。
3. 补 (a)(b) 两个真实 etcd 回归，断言 lease 已撤销、另一个候选能及时当选。

### D2（建议 P3）：NatsMod 在 Bus 停止超时后“保留引用供再次排空”，但 Bus 的停止错误是粘滞的，重试永远失败，Assembly 永不关闭

**现象**：Kit 停止预算内某个 Bus 业务 handler 没有退出时，`NatsMod.StopWithContext` 返回 DeadlineExceeded 并保留 `m.bus` 和 `m.asm`。之后即使 handler 已经退出，重试仍立刻返回同一个 DeadlineExceeded，`m.asm`（NATS 连接、RPC client 和 callback pool）永远不会关闭。

v1.19.0 实测：
```
first_stop_err=context deadline exceeded
retry_after_handler_exit_err=context deadline exceeded asm_retained=true
--- FAIL: TestAuditNatsModRetryAfterBusStopTimeoutEventuallyClosesAssembly
```
修前的 `3560a19b^:kit/nats/nats_mod.go` 会继续执行 `asm.Close` 并置空，重试返回 nil，测试通过：
```
first_stop_err=context deadline exceeded ... retry_after_handler_exit_err=<nil> asm_retained=false
--- PASS
```

App 只调用一次 `stopModSafely`（`app/app.go:582–600`），不会重试。所以在 App 路径上，结果是这次停止期间连接既不 drain 也不关闭，直到进程退出；修前至少会硬关闭。

**根因**：
- `3560a19b` 中 `kit/nats/nats_mod.go:161–164` 在 Bus 停止失败时直接 `return stopErr`，前提是 Bus 停止可以重试。
- 但 `bus/bus.go:316–327,347–352` 的 `Bus.StopWithContext` 在第一次调用时就把 `b.pool` 置 nil，并把 `stopResources(ctx)` 的结果（含 `pool.StopWithContext(ctx)` 的 ctx 错误）存进 `b.stopErr`。之后每次调用只返回这个缓存的错误，不再等待 pool 真正排空。
- `worker.Pool.StopWithContext` 本身可以重新等待（`worker/pool.go:93–96`），但 Bus 已经丢掉了 pool 引用。

NC-09 记录写的是“KitNats只在Bus/Assembly实际成功结束后释放引用……不提前关闭它仍使用的连接”，CHANGELOG 写的是“Assembly/Kit取消后保留资源供再次排空”。这两处的前提都不成立。

**会红的测试草稿**：见 `audit-nc2-drafts/audit_nc09_retry_test.go`（`package nats`，放在 kit/nats 内部）。它用回环 IClient 和真实 `bus.New`/`Start`/`Handle`，注册一个阻塞 handler，再用 `b.Send` 投递一条消息让 handler 进入执行；然后 `m := &NatsMod{bus: b, asm: &natsdriver.Assembly{RPC: rpc}}`，先用 20ms 预算 Stop，放行 handler，再用 Background 重试。断言重试返回 nil 且 `m.asm == nil`。

**候选修法（二选一）**：
- 让 `Bus.StopWithContext` 可重新等待：保留 pool 或 done 的引用，第二次调用对 ctx 错误重新执行 `pool.StopWithContext(ctx)`，只缓存非 ctx 的终态错误。和 `RPCClient.StopWithContext` 的模式对齐。
- 或者 NatsMod 只在 Bus 返回非 ctx 错误时继续关闭 asm，并在记录里改写承诺。

前一种更符合“唯一停止任务、可重复等待”的契约。还要补“超时后重试成功关闭 asm”的回归。

## §4 疑点与不一致

- **S1（NC-09，理论竞态，未能稳定复现）**：`nats/driver/rpc.go:285` 的 `drainCallbacks` 在 `pending.Range` 之后直接 `callbacks.Wait()`，没有用 `callbackMu` 做屏障。
  - `finishPending` 在锁内先 `LoadAndDelete`（第 296 行），再 `callbacks.Add(1)`（第 306 行）。
  - 如果 reply 或 timeout goroutine 刚删掉最后一个键、还没执行 Add，Range 就会跳过该键，接着 Wait 在计数 0 时立即返回，pool 被停掉。之后该 callback 走同步 fallback，在 `StopWithContext` 返回 nil 之后才执行。这也违反了 WaitGroup “Add 不得与计数 0 的 Wait 并发”的规则。
  - 第 282–284 行注释写的是 “added under callbackMu before deletion”，与代码顺序（删除后才 Add）不符。
  - 窗口只有锁内几条指令，没有 hook 可以确定性复现，所以没有列入 §3。
  - 修法很便宜：在 Wait 前加一次 `r.callbackMu.Lock(); r.callbackMu.Unlock()`。停止后不会再有新的 pending，凡是被 Range 漏掉的删除，都在这把锁的临界区里先完成了 Add。
- **S2（NC-09 兼容）**：修前，callback 里第二次调用 `RPCClient.Stop()` 会因为 CAS 立即返回；现在它会等待 `stopDone`，而 `stopDone` 又在等这个 callback 退出，形成永久互等。记录的限制段写了“callback内不能用Stop/Background同步等待自身”，但 CHANGELOG `## [v1.19.0]` 没有写“重复 Stop 现在等待同一任务 / callback 内的 `Stop()` 会自锁”这一行为变化。建议补进升级注意事项。
- **S3（NC-11/12 记录计数）**：记录说“新增正式生命周期15项（含NC-12）”。`formal-results.json` 里的 15 项包含 `WatcherReadiness…` 的 3 个子测试、`WatcherCloseUnblocksFullDeliveryQueue` 和 `MirrorReadCopyCAS…`。这 5 个叶子属于 N03 审查 overlay 的正式化，不是 NC-11/12 的回归。直接针对 NC-11/12 的是 10 项（election 6 项、watch 4 项）。`green-formal.jsonl` 实际只有 4 个叶子（中间那次 driver 编译失败的运行），README 对此有说明，但文件名容易误读。
- **S4（测试质量）**：回退实现后，`TestRPCBudgetStopRetainsFullQueueFallbackAndAllowsRetry` 和 `TestRPCBudgetLateDiscoveryDoesNotStartTransport` 不会给出失败断言，而是挂到 `go test -timeout`。它们对首个阻塞点缺少自带的 guard 超时，CI 在默认 10 分钟超时下才会报错，红文本也不可读。
- **S5（NC-08 观察）**：JetStream 请求解码失败（`jetstream_rpc.go:377–379`）不回包，只把 error 交给 driver 决定 ACK 或重投。core-NATS 路径在 `bus.go:673` 会回 envelope 拒绝。两条路径不对称，但这属于既有设计，不在 NC-08 范围内。
- **S6**：证据 04/05 都在 Windows/amd64 上生成。本审计在 macOS 上复跑，结论一致。

## §5 没读完 / 没跑的

- 没跑 NATS 真实 broker 用例：没有 source 隔离环境，`kit/nats` 的 integration 用例未执行。NC-08 的真实重投、NC-09 的 connected Assembly drain 都没有验证。
- 真实 etcd 只跑了单节点，用于 D1 的两个审计用例。仓库现有的 `real_etcd_promises_test.go`（integration tag）没有全部复跑，HA 和 lease 失联恢复都没有覆盖。
- S1 没有写压力复现，判断基于代码推理。
- 没有做 benchmark。NC-09 的短锁和计数成本、NC-11 的观察者 goroutine 成本都未测。
- `kit/etcd` 没有测试，只确认了编译和 vet。
- 图谱代次落后于两个修复提交，结构性调用方是用 grep 结合源码确认的（`CallDiscoveredChecked`/`PickServer`、`WatchCallback`、`RPC.Stop`、`rpcErrorResponse` 的使用点），不是完整的图谱遍历。
