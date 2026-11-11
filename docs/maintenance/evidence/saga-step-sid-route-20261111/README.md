# saga 步骤按 sid 确切路由：红绿、变异与延迟（2026-11-11）

基线：main `0ce2eb63`（v1.43.0 之后）。方案见 [FEATURE-2026-11-11-saga-step-sid-route](../../FEATURE-2026-11-11-saga-step-sid-route.md)，契约见 [SAGA §按 sid 确切路由](../../../framework/SAGA.md#按-sid-确切路由路由步骤维护者-2026-11-11-方案-③)。宿主：Apple M5，macOS，Go 1.27.3，`GOWORK=off`；有后台负载（CorpLink），延迟只作同机参考。

## 1. 红：多个进程争抢一个共享 durable

用例 `framework/saga` 的 `TestARoutedStepIsDeliveredOnlyToTheOwnerSidsConsumer`：真实的分区协调器（`unitPartition`，派发出的命令事件按线上编码）、三个 sid（1000、1001、1002）的原生步骤消费者（`SubscribeNativeStep`，按生产形状给 `Route = SidRoute(本 sid)`），中间是按 NATS subject 匹配投递的 JetStream 替身：同一 durable 的多个成员按轮转拿投递，handler 返回错误即 nak，按消费者缺省的 nak 退避（250ms 起倍增）重投给下一个成员；退避不真等，只累计，所以事件顺序确定、不靠 sleep。玩家绑定在 1002，排在轮转最后。

修前的代码（只加了 `Step.Routed` / `StartRequest.Route` / `StepConsumerConfig.Route` 三个字段、不改行为，等价于原来的共享 durable）：

```text
--- FAIL: TestARoutedStepIsDeliveredOnlyToTheOwnerSidsConsumer (0.00s)
    step_consumer_test.go:577: non-owner sids [1000 1001] received the step; the result came after 750ms of nak backoff over 3 deliveries ([{sid:1000 durable:game-gift-debit nak:true} {sid:1001 durable:game-gift-debit nak:true} {sid:1002 durable:game-gift-debit nak:false}]); want one delivery to sid 1002 and no backoff
```

三个进程时属主第 3 个拿到，结果晚 0.25 + 0.5 = 0.75s；同一规律下 5 个进程、属主排第 5 时是 0.25 + 0.5 + 1 + 2 = 3.75s，正是 [11-10 演练](../perf-terrain-acceptance-20261110/README.md#第-2-轮无缝演练最后一步的-p95已定位)日志里每个步骤的拒绝时刻。

## 2. 绿

```bash
GOWORK=off go test -count=1 -v -run 'TestARoutedStep|TestAStepBounced|TestAStepThatKeeps|TestARoutedSaga|TestEngineReportsWhichTopicsAreRouted' ./framework/saga/
```

| 用例 | 证明 |
| --- | --- |
| `TestARoutedStepIsDeliveredOnlyToTheOwnerSidsConsumer` | 一次投递、只到 1002、nak 退避 0；JetStream 重投（同一条消息再来）仍只到 1002，回执回放、业务只执行一次，两份结果同一 MsgID、同一回执摘要；协调器收到结果与重复后 saga 完成 |
| `TestAStepBouncedDuringAMoveRunsOnceOnTheNewSid` | 玩家正在从 1000 换到 1001：投到 1000 的步骤被退回发送端（一次 ack 的投递、零执行、零回执写入、发回不带地址的可重试失败，没有 nak）；协调器照原地址再派；换服完成后 1000 退回并给出 1001；协调器改 `Record.Route`，第三次尝试投到 1001 执行一次；1002 全程没收到 |
| `TestAStepThatKeepsBouncingEndsInCompensation` | 换服一直不完成：每次尝试都被退回，尝试用尽后按原有语义结束（第 0 步没有可补偿的，saga failed），业务一次也没执行 |
| `TestARoutedStepWaitsForItsOwnerToComeBack` | 属主 1002 停机：命令留在 `game-gift-debit-1002` 上，别的 sid 收不到；协调器按超时派第二次尝试，地址不变；1002 重新订阅后积压的两次尝试都投给它，业务只执行一次，两份结果都是生效那次的 CommandID，saga 完成 |
| `TestARoutedSagaWithoutARouteIsRefused` | 有路由步骤而 Route 为空、`10.02`、`a b`：启动被拒绝（分区不写事件；`StartSaga` 返回 `ErrInvalidRecord`）；载荷没有 sid 的命令在属主被拒收：零执行、发回不可重试失败，saga failed |
| `TestEngineReportsWhichTopicsAreRouted` | `Engine.RoutedTopic`：路由步骤的正向、补偿（含缺省的 `<forward>.compensate`）主题是路由的 |

`wiring/saga`：`TestStepsRouteSidBoundStepsToThisSidsDurable`（durable `game-gift-debit-1002`、过滤 `roost.saga.*.ev.to.1002.gift.debit`，Mongo 步骤仍共享；没有 sid 拒绝启动）、`TestStepsRefuseARouteThatDisagreesWithTheDefinition`（定义与消费者一边路由一边不路由，三种组合都在订阅前拒绝）。

`codegen`：`TestAddSagaRoutedRoutesTheNamedStepsBySid`（`-routed` 的定义、`EmitStart` / `Start` 签名、登记），`TestAddSagaNativeRejectsBadInputWithoutTouchingTheProject` 加了 `-routed` 不在 `-steps` 里、重复两种。

生成的 game-demo（`internal/service/game/gift_route_test.go`）：定义只路由写 Player 的步骤；属主核对的四种结论（本服执行、离线发送方在本服接入、绑定别的服退回并指明其 sid 且不接入、没有 sid 拒收且不接入、卸载中普通错误）；公会玩家步骤同一规则。

## 3. 变异（每次只改一处，确认变红后还原）

| 变异 | 红 |
| --- | --- |
| 协调器不按路由发（`emit` 用 `CommandEventSubject`） | `non-owner sids [] received the step; the result came after 0s of nak backoff over 0 deliveries ([]); want one delivery to sid 1002 and no backoff`；`first attempt deliveries [] (backoff 0s), want one acknowledged delivery to sid 1000` |
| 协调器忽略退回的新地址 | `after the bounce with a new route the saga's route is "1000", want 1001` |
| 退回当成普通错误（nak） | 16 次投递全部回到 sid 1000、nak 退避累计 5m1.75s；`attempt 1 result = [], want one bounce` |
| 不做启动时的路由检查 | `route "": a routed saga started (1 events), want the start refused` |
| 路由消费者不按 sid 区分 durable 名 | `durable game-gift-debit on UNIT_SAGA_G0 has filter unit.saga.*.ev.to.1000.gift.debit, not unit.saga.*.ev.to.1001.gift.debit`（JetStream 拒绝用另一个过滤绑定已有 durable） |
| 生成工程：赠礼定义去掉 `Routed` | `gift_route_test.go:82: gift_item step debit: routed=false, want true` |
| 生成工程：`admitBound` 对不绑定本服的玩家返回普通错误 | `gift_route_test.go:130: a step for a sender bound to sid 1000 = leaving it, want a bounce to route 1000` |

## 4. 验证命令

```bash
gofmt -l framework wiring codegen infra                     # 空
GOWORK=off go build ./... && GOWORK=off go vet ./framework/saga/ ./wiring/saga/ ./wiring/sidmerge/ ./codegen/...
GOWORK=off go test -count=1 -race ./framework/saga/ ./wiring/saga/ ./wiring/sidmerge/
GOWORK=off go test -count=1 .
GOWORK=off go test -count=1 ./codegen/...                   # 含生成 game-demo 的 build / vet
GOWORK=off go test -count=1 ./...
# 生成 game-demo（--skip-deps，replace 指向本检出）
go run ./codegen/cmd/roost project new sagaroute -module example.com/sagaroute -out <dir> -template game-demo --skip-deps
(cd <dir> && go mod edit -replace github.com/tjbdwanghaibo/roost-core=<本检出> && go mod tidy && go build ./... && go vet ./... \
  && go test -count=1 -race ./internal/service/game && go test -count=1 ./...)
python3 scripts/check-doc-anchors.py
```

结果见第 6 节。

## 5. 延迟：修前与修后

| 场景 | 修前 | 修后 |
| --- | --- | --- |
| 单元（3 个 sid，属主排第 3） | 3 次投递，结果前 nak 退避 0.75s | 1 次投递，0 |
| 推算（5 个 sid，属主排第 5） | 每个原生步骤 +3.75s（250 + 500 + 1000 + 2000ms） | 0（一次投递） |
| 真实依赖无缝演练最后一步（`cmd/loadtest` sid 1003，2 个机器人） | p95 27.85s（11-10，[证据](../perf-terrain-acceptance-20261110/README.md#第-2-轮无缝演练最后一步的-p95已定位)；saga 终态记录：礼物完成 3.80s、补偿 7.56s、联盟领奖 3.82s） | p95 5.00s（见下） |

### 真实依赖演练

`scripts/test-seamless-demo.sh` 整轮（5 个游戏进程 + 网关，生成的 game-demo 指向本检出），依赖由 `scripts/mirror-local.sh` 起在私有根目录与私有端口偏移上（NATS 三节点、Mongo 副本集、Redis），etcd 单独私有起，用后 `clean` 退出 0；不连共享环境。当时脚本最后一步仍带 `-max-p95 40`，阈值只影响判定，不影响测得的值。

| 项 | 结果 |
| --- | --- |
| 演练步骤 | 14 步全部 ok（enter、own_state、straight、jitter、diagonal、attack_across_0/1、panel_level_up、monster_cross、cell_crash、base_restart、team_panel、team_roster_cross_base、ordinary_scene） |
| 最后一步 `cmd/loadtest`（sid 1003，2 个机器人） | 成功 2、失败 0、错误率 0；p50 4.995s、p95 5.00s（2 个机器人时 p95 就是最慢的那个） |
| 场景里的 saga 状态查询 | 礼物状态（10014）共 4 次、公会操作状态（10030）共 7 次，即每个机器人每笔 saga 一两次查询就看到终态 |
| 各游戏进程日志里的退回 / 拒收 | 0 条（玩家都绑定在发起方 sid，步骤一次投给属主） |

修前同一脚本同一步 p95 27.85s（11-10，5 进程拓扑下每个原生步骤被非属主拒一次、nak 退避，约多出 22s）。修后 5.00s 回到缺省阈值 16s 以内，所以脚本这一步撤掉 `-max-p95 40`、改回缺省；改阈值后没有再整轮复跑。本机有后台负载，数值只作同机参考。

## 6. 结果

| 检查 | 结果 |
| --- | --- |
| `gofmt -l` | 空 |
| `go build ./...`、`go vet`（上列包） | 通过 |
| `-race`：`framework/saga`、`wiring/saga`、`wiring/sidmerge` | ok |
| 根包 `go test -count=1 .` | ok（改文档前） |
| `go test ./codegen/...` | ok（改文档前） |
| `go test ./...` 全量 | 改文档前一轮：除根包外全 ok，根包的文档断链已修、根包单独复跑 ok；改文档后的一轮因暂停中途停掉，未完成 |
| 生成 game-demo（第一次生成）：build / vet、`internal/service/game -race` | ok（-race 83s） |
| 生成 game-demo（暂停前重新生成）：build / vet、全量 `go test ./...` | ok（36 个包 ok） |
| 生成 game-demo（暂停前重新生成）：`internal/service/game -race` | **FAIL**（106s，原文只留下 `FAIL	example.com/sagaroute/internal/service/game	106.143s`，失败用例未保存，**尚未确认**） |
| `check-doc-anchors.py` | 0 个断链 |
