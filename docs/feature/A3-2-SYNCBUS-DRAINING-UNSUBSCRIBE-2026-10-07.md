# A3 ②：排空下沉到传输层——`ISyncBus` 带 ctx 的退订（2026-10-07）

来由：[DECISIONS-PENDING A3](../review/DECISIONS-PENDING-2026-10-05.md) 的 ②（第二轮定为“下个大版本、有兼容期”），第十三轮维护者要求本版完成（“还有留到下个版本的几项在本机能完成吗？希望本次能完成了”），并说明线上未部署、**不要兼容期，直接替换**。A3 ① 已实施（`50f2ac2a`，[方案](REFACTOR-2026-10-05-shared-stop-contract.md)）。

## 1. 当前问题

`ISubscriber.Subscribe(topic, handler) (unsub func(), error)` 的退订函数只发起退订，不等在途回调：

- 普通 NATS：`sub.Unsubscribe()` 只发 UNSUB，nats.go 的回调 goroutine 可能正在执行 handler；
- JetStream：fanout 在 `b.mu` 下取 handler 快照、锁外逐个调用，退订只是从 map 里删掉，快照里的旧 handler 照样被调；`ConsumeContext.Stop` 也不等回调。

于是“退订返回 = 回调已静止，可以释放依赖”这个不变量只能由每个订阅者自己补：`mirror.Replicator` 每次 Start 一个 `operation.Lifetime`、停掉的放进 `draining`、`StopWithContext` 逐个等；`cache.ReplicaSyncer.Stop`、`syncbus.PatchSyncer.Stop`、`syncstream.Subscribe` 的退订则干脆不等（返回后 Store / Apply / handler 仍可能在跑）。同一不变量已被打破 11 次（A3 来由），根子在传输层的退订本身不承诺排空。

## 2. 新接口

`sync/syncbus/sync.go`：

```go
type Handler func(ctx context.Context, msg *SyncMsg) error

type ISubscriber interface {
	Subscribe(topic string, handler Handler) (*Subscription, error)
}
type ILiveSubscriber interface {
	SubscribeLive(topic string, handler Handler) (*Subscription, error)
}

// Subscription 是一次订阅的句柄，由传输实现用 NewSubscription 建、用 Deliver 投递。
func NewSubscription(topic string, handler Handler, release func()) *Subscription
func (s *Subscription) Deliver(ctx context.Context, msg *SyncMsg) error // 退订开始后返回 ErrUnsubscribed，不调 handler
func (s *Subscription) Unsubscribe(ctx context.Context) error
```

`Subscription` 是**具体类型**，不是接口：排空语义只实现一次（内部就是 A3 ① 的 `operation.Lifetime`），每个传输与测试替身都通过 `NewSubscription` + `Deliver` 得到同一份语义，不再各写一遍。传输只负责两件事：把每条消息交给 `Deliver`，以及提供 `release`（从 fanout 删除 / 停消费者 / 发 UNSUB，不等待）。

### 2.1 `Unsubscribe(ctx)` 的语义（三步停机）

1. **发起（幂等）**：关闭这次订阅的投递准入，之后到达的消息不再调用 handler（`Deliver` 返回 `ErrUnsubscribed`）；`release` 只调用一次。
2. **在 ctx 内等排空**：等这次订阅已准入的 handler 调用全部返回。ctx 先结束返回 ctx 错误（`errors.Is(err, context.DeadlineExceeded / Canceled)`），订阅保持“退订中”，准入仍关闭；用新 ctx 重试继续等同一批。
3. **返回 nil**：此后这个订阅既没有在途回调，也不会再有新回调，订阅者可以释放 handler 用到的依赖。返回 nil 后再调用（任何 ctx，包括已取消的）都返回 nil。nil `*Subscription` 的 `Unsubscribe` 返回 nil。

排空后 `Subscription` 丢掉对 handler 的引用，订阅者闭包里的状态（如 syncstream 的分片重组表）随之可回收。

### 2.2 回调里退订自己（不死锁）

`Deliver` 把投递 ctx 交给 handler，ctx 里带着“正在执行这个订阅的回调”的标记。handler 用这个 ctx（或由它派生的 ctx）调用自己订阅的 `Unsubscribe` 时：先归还本次调用（以及同一调用链上同一订阅更外层的同步重入调用）的准入，再等**其他**在途调用；返回 nil 表示除了调用者自己之外没有在途回调、也不会再有新回调，调用者自己在 handler 返回时结束。

Go 没有 goroutine 身份，传输层只能经 ctx 认出“自己”，所以 handler 里退订自己必须传入 handler 收到的 ctx。传入无关的 ctx（如 `context.Background()`）会等自己：带期限的 ctx 到期返回 ctx 错误（不是永久死锁），`Background` 会一直等——契约写明，不做运行时猜测。

handler 在别的 goroutine 里拿着投递 ctx 退订：本次调用若已返回，标记已归还，不会重复归还；若仍在执行，按“调用者自己”处理。

### 2.3 投递 ctx

投递 ctx 由传输给出：JetStream 用 `fctx.BaseContext()`，普通 NATS 用 `fctx.BaseContext()`，替身用调用方的 ctx；`Deliver` 只往上面加标记。**退订不取消在途回调的 ctx**：旧行为是退订后在途回调照常做完（durable 消费者随后 ACK），取消会让停机窗口里已 ACK 的消息在 Store 里半途放弃。需要停机时让回调尽快放弃的订阅者，仍用自己的停止信号（如 `SnapshotClient.stopCtx` 取消权威加载）。

mirror / PatchSyncer 原来传给 Store / Apply 的是 `fctx.BaseContext()` / `context.Background()`，现在传投递 ctx（同为不取消的基 ctx 加标记），行为不变。

### 2.4 与总线停止的关系

`jetStreamSyncBus.StopWithContext` 的 `deliveries`（NC-172）保留：它是**传输自己**的生命周期——停止时 NAK 新投递、等 consume 回调返回，之后 SyncBusMod 才把 NATS 连接交还。单个订阅的退订排空与总线停止互不依赖：总线停后再退订，`release` 找不到 fanout 直接返回，等待只看这次订阅自己的计数。

## 3. 各驱动实现

| 驱动 | 投递 | release |
| --- | --- | --- |
| JetStream（`driver/jetstream.go`） | fanout 里存 `*Subscription`；consume 回调在 `b.mu` 下取快照，锁外逐个 `Deliver`（每个订阅各拷一份消息，panic 隔离保留）；`ErrUnsubscribed` 静默跳过 | `b.mu` 下从 fanout 删除；最后一个本地订阅时删 fanout 并 `Stop` 消费者（不等待，等待在 `Subscription` 里） |
| 普通 NATS（`driver/nats.go`） | nats.go 回调里解码、跳过本服消息后 `Deliver` | `sub.Unsubscribe()`；连接已关等失败记 Warn（订阅准入已关，不会再有回调） |
| 内存 / 测试替身 | 仓内没有生产用的内存总线；各包测试替身（mirror、remoteentity、cache、syncstream、PatchSyncer、kit、codegen remoteflow、skill sync-e2e）改为 `NewSubscription` + `Deliver` | 从替身的 handler 表删除 |

不新增生产内存驱动：现有替身各带故障注入（发布失败、计数、阻塞），合成一个通用内存总线不减少代码。

## 4. 订阅者迁移

用 `rg -n "\.(Subscribe|SubscribeLive)\(|\.Subscribe\b|SubscribeLive\b"`（非测试文件）穷举 `ISyncBus` 的订阅方（图谱共享 generation 停在 09-30、缺 `jetStreamSyncBus.Subscribe` 等节点，以当前源码 rg 为准）：

| 订阅方 | 之前 | 之后 |
| --- | --- | --- |
| `sync/syncbus/mirror.Replicator` | 每次 Start 一个 `operation.Lifetime` 准入门 + `draining` 列表，handler 里 Begin/End | **删掉自己的准入门**：只记当前 `*Subscription` 与尚未确认排空的旧订阅；`Stop` = 发起退订（`Unsubscribe` 用已取消的 ctx，只做第 1 步），`StopWithContext(ctx)` = 对每个未排空订阅 `Unsubscribe(ctx)`，nil 的移出列表 |
| `remoteentity.SnapshotClient` | 经 Replicator 停；`work` Lifetime 管 L2 / 权威 / 兴趣广播 | 不变（`work` 守的是读路径对依赖的调用，不只是复制回调，不能删） |
| `cache.ReplicaSyncer` | `Stop()` 不等在途 Store | `Stop(ctx) error`：转 `Replicator.StopWithContext`，返回 nil 后 Store 不再被调用（行为收紧） |
| `syncbus.PatchSyncer` | `Stop()` 不等在途 Apply | `Stop(ctx) error`，同 Replicator 的写法（行为收紧） |
| `syncstream.Subscribe*` | 返回 `func()`，退订后清空重组表，不等在途 handler | 返回 `*Subscription`；重组表随 handler 引用在排空后释放（行为收紧） |
| `bus`（服务 RPC，`bus/jetstream_rpc.go` 的 `handlers`） | — | 不是 `ISyncBus`，是 bus 自己的 JetStream RPC 消费者；不在本项范围，保持 |
| `jetStreamSyncBus.deliveries` | — | 传输自己的停机（§2.4），保留 |

## 5. 停机契约骨架

`internal/stopcontract.Check` 直接套 `Subscription.Unsubscribe`：Start = 订阅一个会卡住的 handler，Block = 投递一条消息并等 handler 进入，Stop = `Unsubscribe(ctx)`，Released = `stopcontract.CallerReleases`（订阅者在 nil 之后才释放依赖）。在三处跑：`syncbus` 包里的 `Subscription` 本身（`subscription_promises_test.go`）、JetStream 驱动（fake JetStream）与普通 NATS 驱动（fake 客户端，退订后回调登记不撤，模拟 nats.go UNSUB 之后仍交出已出队消息）（`driver/unsubscribe_drain_promises_test.go`）。真实 nats-server 上的同一组承诺由 `kit/syncbus/unsubscribe_drain_integration_test.go` 覆盖（不套骨架，断言相同）。mirror 原有的骨架用例（`mirror/stop_contract_test.go`）保留，现在验证的是“Replicator 把排空交给传输”。

## 6. 兼容

不保留兼容期（维护者：线上未部署）。破坏性变化：

- `syncbus.Handler` 加 `ctx` 参数；`Subscribe` / `SubscribeLive` 返回 `*syncbus.Subscription`；
- `cache.ReplicaSyncer.Stop`、`syncbus.PatchSyncer.Stop` 改为 `Stop(ctx) error`；`syncstream.Subscribe*` 返回 `*syncbus.Subscription`；
- 自己实现 `ISyncBus` 的替身要用 `syncbus.NewSubscription` / `Deliver`。

wire、持久格式、durable 名、生成形状不变（codegen 只有 `remoteflow` 测试夹具里的替身随之改）。

## 7. 验证计划

- 先红后绿（`sync/syncbus/driver`、`sync/syncbus`）：带屏障的用例——handler 卡住时旧 `unsub()` 已返回而回调仍在跑（红，修前在旧接口上取文本）；新 `Unsubscribe` 在回调返回前不返回 nil。回调里退订自己不死锁；ctx 超时返回错误、放行后重试 nil；重复退订 nil。
- 停机契约骨架套新接口（§5）。
- 真实 NATS：`scripts/mirror-local.sh` 起私有 NATS，跑 JetStream 驱动的 integration 用例（`-run` 限定）；有条件跑 mirror / remote 生成工程脚本。
- `GOWORK=off`：gofmt、改动包 vet（含 `-tags integration`）、`-race -count=3`、glsvet（含 `-stophints`）、根包、`go build ./... && go vet ./...`、`go test ./codegen/...`、`bash scripts/test-sync-modes-generated.sh`。

## 8. 实施记录

已实施（提交见 DECISIONS-PENDING 第十三轮“A3②”行），未发版。

| 项 | 结果 |
| --- | --- |
| 接口 | `sync/syncbus/sync.go`：`Handler` 加 ctx，`Subscribe` / `SubscribeLive` 返回 `*Subscription`；新 `sync/syncbus/subscription.go`（`NewSubscription` / `Deliver` / `Unsubscribe` / `ErrUnsubscribed`） |
| 驱动 | JetStream fanout 存 `*Subscription`、`invoke` 经 `Deliver`（panic 隔离保留），release 删 fanout / 停消费者；普通 NATS 经 `Deliver`，release 发 UNSUB（失败记 Warn） |
| 订阅方 | `mirror.Replicator` 删掉自己的 `gate` / `draining`（`operation.Lifetime` 准入门），只记未排空的订阅；`cache.ReplicaSyncer.Stop(ctx)`、`PatchSyncer.Stop(ctx)`、`syncstream.Subscribe*` 返回 `*Subscription`（后三处修前根本不等，登记为 [RR-20261006-36](../bug/RR-20261006-36.md)） |
| 未改 | `remoteentity.SnapshotClient.work`（守读路径对 L2 / 权威的调用）、`jetStreamSyncBus.deliveries`（传输自己的停机，NC-172）、`bus` 的 JetStream RPC `handlers`（不是 ISyncBus） |
| 测试替身 | remoteentity（6 个）、mirror（2）、cache、syncstream、PatchSyncer、kit/syncbus、kit/remoteentity、codegen `remoteflow`（2 个包装总线，透传投递 ctx）、skill `sync-e2e` 改为 `NewSubscription` + `Deliver` |
| 规范 | roost-coding 生命周期一节“排空下沉是下个大版本”改为现行规则；`~/.codex/skills/roost-optimize` 入口加一行 |

代码量（`git diff --numstat`，`.go`）：生产代码 +275 / −141（净 +134）。其中新增 `subscription.go` 114 行（注释 32 行、代码 71 行），这是排空的唯一实现；订阅方 mirror −52 / +34（去掉准入门，`Stop` 改为只发起的 `StopWithContext`，非注释行 −43 / +24）、syncstream −13 / +6；PatchSyncer +34 / −16 与 cache +6 / −3 是**补上原来缺失的等待**（RR-20261006-36），不是重复的排空。驱动 jetstream +49 / −45、nats +24 / −7。测试 +864 / −152（新用例与替身迁移）。

## 9. 红绿与验证

基线 `66d72a33`。

**修前红（旧接口，屏障卡住 handler 后调退订函数）**——fake（`sync/syncbus/driver`）：

```text
--- FAIL: TestOldUnsubscribeReturnsWhileHandlerRuns (0.00s)
    --- FAIL: TestOldUnsubscribeReturnsWhileHandlerRuns/jetstream (0.00s)
        unsubscribe_drain_promises_test.go:50: unsubscribe returned while the handler was still running: the subscriber would release its dependencies under an in-flight callback
    --- FAIL: TestOldUnsubscribeReturnsWhileHandlerRuns/nats (0.00s)
        unsubscribe_drain_promises_test.go:50: unsubscribe returned while the handler was still running: the subscriber would release its dependencies under an in-flight callback
```

真实 nats-server（私有进程，经 kit NatsMod + SyncBusMod，基线代码临时用例）：

```text
    old_unsubscribe_red_integration_test.go:90: unsubscribe returned while the handler was still running (real jetstream)
    old_unsubscribe_red_integration_test.go:90: unsubscribe returned while the handler was still running (real nats)
--- FAIL: TestRealSyncBusOldUnsubscribeReturnsWhileHandlerRuns (0.67s)
```

**修后绿**：

- `sync/syncbus`：`Subscription` 契约骨架；退订后 `Deliver` 返回 `ErrUnsubscribed`、重复退订 nil；回调里用投递 ctx 退订自己不死锁且仍等其他在途回调；同步重入（handler 投递回自己）里退订不死锁；传入无关 ctx 退订自己到期返回 `DeadlineExceeded`、回调返回后重试 nil；handler panic 也归还准入。
- `sync/syncbus/driver`（JetStream、普通 NATS 各一遍）：短预算 `Unsubscribe` 返回 `DeadlineExceeded`、放行后重试 nil 且回调已返回、之后投递不再调 handler、重复退订 nil；回调里退订自己不死锁；契约骨架。
- 真实 nats-server（`kit/syncbus`，`-tags integration -race`）：

```text
    unsubscribe_drain_integration_test.go:120: first Unsubscribe=context deadline exceeded retry=<nil> handler_returned_at_nil=true calls_after_publish=1   (jetstream)
    unsubscribe_drain_integration_test.go:120: first Unsubscribe=context deadline exceeded retry=<nil> handler_returned_at_nil=true calls_after_publish=1   (nats)
--- PASS: TestRealSyncBusUnsubscribeDrainsTheSubscription (2.08s)
--- PASS: TestRealSyncBusUnsubscribeFromOwnHandler (1.68s)
--- PASS: TestRealJetStreamSyncBusStopDrainsAnInFlightHandler (0.48s)
```

- `scripts/mirror-local.sh up`（私有根目录、端口偏移 23700，3 节点 JetStream + Mongo 副本集 + Redis + Cluster + toxiproxy）：`remoteentity` 的 `^TestRealJetStream|^TestMirrorLocal` 10 个用例全过；`scripts/test-remote-generated.sh` 13 个 `TestGeneratedRemote*` 通过（含 `MirrorGuildSummary` 的 jetstream / nats 两个子用例）；`TestGeneratedRemoteMirrorLocal`（两进程故障场景 S1～S3）通过（109.7s）。同一次运行里排在它之后的 7 个用例因它做过 Redis 切主、直连地址变成只读副本而报 `READONLY`，与本改动无关；重建环境、不带 `ROOST_MIRROR_LOCAL` 重跑全部通过（`mirror-local.sh test` 平时只跑 MirrorLocal 一个用例，不会遇到）。环境用完 `clean`。
- `GOWORK=off`：gofmt 空；`go build ./... && go vet ./...`、改动包 `go vet -tags integration`；`go test -race -count=3` 覆盖 `sync/syncbus/...`、`syncstream`、`cache`、`remoteentity`、`kit/syncbus`、`kit/remoteentity`；glsvet（含默认 `-stophints`）三大模块与 `sync/syncbus/...` 无提示；根包 `go test -count=1 .`；`go test ./codegen/...`；`bash scripts/test-sync-modes-generated.sh`；`skill/integration/sync-e2e` 模块测试。生成模板未改，未跑 `go generate` / game-demo 重生成。
