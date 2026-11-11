# saga 步骤按 sid 确切路由（2026-11-11）

状态：方案 ③ 已实施（未发版）。框架契约写在 [SAGA §按 sid 确切路由](../framework/SAGA.md#按-sid-确切路由路由步骤维护者-2026-11-11-方案-③)，本文记问题、决定过程、设计取舍、未采用的方案与剩下的待确认项。证据见 [saga-step-sid-route-20261111](evidence/saga-step-sid-route-20261111/README.md)。

## 1. 问题

[第 2 轮性能验收](evidence/perf-terrain-acceptance-20261110/README.md#第-2-轮无缝演练最后一步的-p95已定位)定位了无缝演练最后一步（`cmd/loadtest`，sid 1003，2 个机器人）p95 约 28s 的原因：

- 5 个 game 进程（sid 1000～1004）共用一个 JetStream durable 消费 saga 原生步骤（赠礼扣款 / 退款、公会玩家步骤），一条命令落到随便哪个空闲进程；
- 步骤只能在玩家绑定的 sid 上执行，非属主的 `WithAdmit` 拒收（nak）并经总线把工作交接给属主，但结果要等原消息按 nak 退避（250ms 起倍增）重投到属主才发布；
- 属主排在轮转第 5 个时每步多 0.25 + 0.5 + 1 + 2 = 3.75s，机器人场景 6 个原生步骤合计约 22s。

不是性能退化，是“多个进程争抢、再由属主补救”这个形状本身的代价，任何多 game 进程部署的 saga 延迟都受它影响。v1.43.0 先把演练阈值放宽到 40s（方案 A）。

## 2. 决定过程

| 时间（2026-11-11） | 维护者原话 / 决定 |
| --- | --- |
| 上午 | “按照2方式处理，当前多进程的方式确实会有问题，理想情况是有确切的路由，而不是多个争抢”——先做 ②（非属主交接给属主，属主直接发结果），③ 只评估 |
| 随后 | “改做3吧”——停做 ②，改实施 ③：写 payload 的一方在发步骤时就知道目标 sid，就应该直接投到属主 |
| 补充 | 玩家换服按换服状态处理：属主收到时玩家正在换服，不在本服执行，**丢回发送端**（saga 编排侧）由它处理——迁移完成后按新 sid 重发，等到超时就走补偿；不在换服中但已不绑定本服的，也走同一条路径，由发送端按最新绑定重发。旧消息不兼容：没有路由 sid 的步骤直接拒绝、记错误日志 |

② 在改决定之前没有写代码，没有半成品需要撤回。

## 3. 方案 ③ 的设计

### 3.1 路由键从哪里来

**发起方在启动时写一次**：`StartRequest.Route`（按 sid 路由时是 `saga.SidRoute(sid)`，十进制 sid）。它进记录（`Record.Route`）和启动摘要；协调器派发路由步骤时写进 `Command.Route`。考虑过两种别的来源：

- 定义里给每步一个“从载荷算 sid”的函数：协调器要执行业务代码，载荷格式变成协调器的依赖；补偿迟到步骤用的 `LateData` 也要能解。没采用。
- 消费者自己按载荷判断（就是旧形状）：仍是争抢。

代价是一笔 saga 只有一个地址：全部路由步骤投给同一个 sid（见 §6 限制）。

### 3.2 subject、durable 与 JetStream 资源

| 项 | 不路由的步骤（不变） | 路由步骤（新） |
| --- | --- | --- |
| 命令事件 subject | `<prefix>.<k>.ev.cmd.<topic>` | `<prefix>.<k>.ev.to.<route>.<topic>` |
| 消费者过滤 | `<prefix>.*.ev.cmd.<topic>` | `<prefix>.*.ev.to.<sid>.<topic>` |
| durable 名 | `<group>-<step>`，全部副本共用 | `<group>-<step>-<sid>`，只有这个 sid 的进程绑定 |
| 每条 saga 流上的 durable 数 | 每个步骤（每个方向）1 个 | 每个步骤（每个方向）× 部署的 sid 数 |

- 路由键放在主题之前、用单独的去向 `to`：主题可以带点，这样“某个 sid 的全部路由命令”（`<prefix>.*.ev.to.<sid>.>`）与按主题过滤都没有歧义，也不会与已有主题撞名。
- 命令事件仍在分区的 `<prefix>.<k>.>` 里：**流的 subjects、分区布局、快照与清理都不变**。清理守卫按每个 durable 的 ack floor 算；没有待投消息的路由 durable 的 floor 是流末（`DurableAckFloors` 对过滤消费者按“下一条匹配消息”计），不挡清理。
- demo 的数量：赠礼扣款 / 退款 2 个、公会玩家步骤 6 个（建盟扣费 / 退费、捐献扣费 / 退费、领奖发放 / 收回），每个 sid 每条流 8 个路由 durable；无缝演练 5 个 sid、缺省 4 条流（`saga.stream_groups`）共 160 个，原来是 32 个共享的。Mongo 步骤（deliver、found、donate、claim 及补偿）仍共享。

### 3.3 框架改动

- `framework/saga`：`Step.Routed`；`StartRequest` / `Record` / `Command` 的 `Route`；`Completion.Route`（只出现在退回结果上）；`SidRoute`；`Layout.RoutedCommandEventSubject` / `RoutedCommandFilter`；启动检查 `checkStartRoute`（分区与 `StartSaga` 同一个函数）；派发时把 `Record.Route` 写进路由命令；`StepConsumerConfig.Route`（durable 名与过滤）；准入的两种新结论 `BounceStep` / `RejectStep`（`admitCommand`）；`applyCompletion` 按退回的新地址改 `Record.Route`；`Engine.RoutedTopic`；回执摘要与启动摘要含 Route；冷副本存 Route；**WireVersion 3 → 4**。
- `wiring/saga`：`WithSidRoute()`（`config.Route = SidRoute(本进程 sid)`）；`Start` 核对本进程有 sid、同进程有协调器时与定义的 `Routed` 一致；删除 `RunHandedOver` 与 `ErrStepsNotStarted`。
- `codegen`：`roost add saga ... -routed <step>`（定义 `Routed: true`，`EmitStart` / `Start` 多一个 `route` 参数，生成的登记加 `WithSidRoute`）。

### 3.4 demo 改动

- 赠礼：`add saga GiftItem -routed debit`；`StartGift` 写 `Route: saga.SidRoute(fromSID)`；扣款 / 退款登记 `WithSidRoute()` + `WithAdmit(admitBound)`。
- 公会：三个 saga 的玩家步骤 `-routed`（建盟、捐献的 debit，领奖的 grant）；`StartGuildOp` 从载荷的 `guild.State.SID` 取 Route（载荷与地址同源，不会不一致）；登记同上。
- 删除（确切路由下成了死代码）：`giftStepHandoff` / `guildStepHandoff`、`giftHandoffRouter`、`sidRoute` / `sidRoutes`、`runHandoff`、`handOver`、`admitOwned` / `admitPhase`、两个总线 handler 模块，`gift_handoff_test.go` 换成 `gift_route_test.go`。`ownerroute` 包保留（联盟通知还在用）。
- 保留并改写**属主核对** `admitBound`（防第二个写者的最后一道）：载荷没有 sid → `RejectStep` + ERROR 日志；玩家不绑定本 sid → `BounceStep(SidRoute(绑定的 sid))`；副本正在卸载 → 普通错误（nak，回到本 sid）。

### 3.5 必须处理的四种情况

| 情况 | 行为 | 谁负责“结果不确定” |
| --- | --- | --- |
| **玩家换服 / 不再绑定本服** | 属主核对在执行之前（实体未加载），`BounceStep` 退回协调器：一份可重试失败（`Completion.Route` = 新地址或空），PubAck 之后 ack。协调器消耗一次尝试、按退避派下一次，有新地址就改 `Record.Route`。换服中不给地址 → 照原地址重试；换服完成后旧 sid 退回时给出新 sid → 下一次投到新 sid 执行一次。尝试用尽 → 补偿（补偿方向 → `ManualRequired`）。不用 nak：路由 durable 只有本 sid 绑定，nak 只会投回自己 | 退回只发生在执行之前，零写入；同一操作若更早的一次尝试已在旧 sid 生效，回执随实体搬到新 sid，新 sid 读到回执回放 |
| **属主停机** | 命令留在它的 durable 上（未确认），别的 sid 拿不到；协调器按 Timeout 派下一次尝试到同一地址，用尽走补偿——与此前“别的进程只拒绝、等到超时”的超时与补偿语义相同。属主恢复后积压的各次尝试依次投给它 | 回执：至多一次生效，回放的结果 CommandID 是生效的那次，协调器按“成功接收规则”接收或补偿迟到成功 |
| **重复投递**（JetStream 重投、协调器的新尝试） | 都只到属主；原生步骤读实体里的回执回放，Mongo 步骤读收件箱；结果 MsgID `res.<CommandID>`，协调器按回执摘要去重 | 沿用现有机制，没有新增去重层 |
| **没有路由键的旧载荷** | 启动阶段：有路由步骤而 `StartRequest.Route` 为空 → 拒绝启动（`ErrInvalidRecord`，分区计 `rejected_input_total{reason="route"}`）。步骤阶段（理论上到不了）：载荷没有 sid → `RejectStep`，ERROR 日志，协调器随即补偿 / 人工。不迁移、不兜底 | — |

“换服中”的状态由业务提供。game-demo 的玩家是**静态绑定**，没有单个玩家换服：整服搬机器时 sid 不变（命令在 durable 上等新进程，[PLAYER-MIGRATION §2](../framework/PLAYER-MIGRATION.md)），别名合服是离线操作、要求全部 saga 终态。所以 demo 的退回只用于“载荷里的 sid 与本服不符”，换服中的那一支由框架用例（`TestAStepBouncedDuringAMoveRunsOnceOnTheNewSid`）以替身绑定表证明，见 §7 待确认 1。

## 4. 未采用：方案 ②（考虑过，维护者 11-11 改为 ③）

② 的设计（没有写代码）：保留共享 durable；非属主收到步骤后**同步**经总线 RPC（`CallTo` / JetStream RPC 按 `nats.rpc.transport`）把命令交给属主；属主执行并把结果发回协调器（PubAck 之后才回复），非属主拿到成功回复后 ack 原消息（框架新增“已由别处结算”的准入结论），回复失败或超时则 nak 回退到重投。去重沿用回执与结果 MsgID。

没有采用的原因（维护者）：写载荷的一方本来就知道属主，争抢再补救是绕路。工程上 ② 还留有：每条错投的命令多一次跨进程 RPC 与一个在途槽、RPC 传输要跟 `nats.rpc.transport` 走两套、错投比例随进程数上升（5 进程时 4/5 的命令先落到非属主）。

更早的备选（KNOWN-LIMITS 2026-11-10 的方案 B 列表）：

- 路由拒绝用短的固定 nak 延迟：只缩短退避，争抢与 4/5 的错投仍在，且固定短延迟在属主停机时空转。未采用。
- 只改演练（方案 A）：v1.43.0 已做；本轮之后按 §8 改回缺省阈值。

## 5. ② 能否平滑演进到 ③（原评估要求，留档）

能，但两者没有共用代码：② 的“非属主同步交接 + 属主发结果”在 ③ 下没有非属主可言，整条交接路径（RPC、超时回退、已结算结论）会成为死代码；共用的只有去重（回执 + 结果 MsgID + 协调器回执摘要）与“属主核对”。直接做 ③ 省掉了这一层。

## 6. 限制

- 一笔 saga 一个路由地址：改两个不同 sid 上实体的 saga（两名玩家之间的交易）需要“按步骤的地址”，未实现。需要时的方向：`StartRequest.Routes map[步骤名]route` 或让步骤结果带下一步的地址；改记录与线格式。
- 退回消耗一次尝试、按步骤退避重派：换服窗口长于“尝试数 × 退避”时走补偿。demo 扣款的预算（15 次）按崩溃重启定，退回时最长约 Σ 退避 ≈ 46s。
- 同一条命令先后被退回两次、两次的地址不同（换服完成前后）：第二份结果按身份冲突被分区跳过（`rejected_input_total{reason="identity"}`，WARN），协调器已按第一份推进，无害但有日志噪声。
- 定义与消费者的路由一致性只在协调器与步骤消费者同进程时由 `Steps.Start` 核对；拆分部署靠两边用同一份生成的定义。
- JetStream durable 数随 sid 数线性增长（§3.2）；没有测 1000 个 sid 量级的服务端开销。
- 别名合服后 A 的路由 durable 是空闲遗留，要运维删（`sidmerge checklist` 已列命令）。

## 7. 待确认

1. **玩家换服的状态来源**：维护者要求“用现有迁移的状态”。仓库里没有单个玩家改绑 sid 的能力（[PLAYER-MIGRATION §1](../framework/PLAYER-MIGRATION.md#1-先说清迁移的对象)：account 没有修改 `ServerID` 的接口），现有的“迁移”是整服搬机器（sid 不变，不需要退回）与离线别名合服（要求 saga 全部终态）。框架的退回机制与用例已就绪；demo 的 `admitBound` 里换服中那一支要等单个玩家换服的功能存在（或维护者指定现有哪个状态算“换服中”）再接。
2. 退回是否应当**不消耗**尝试（例如按 Timeout 而不是按退避重派）：现在与可重试失败同一语义，见 §6 第 2 条。

## 8. 验证与演练阈值

单元与生成工程的红绿、变异与命令见[证据](evidence/saga-step-sid-route-20261111/README.md)。演练阈值：本机私有真实依赖复跑 14 步 ok，最后一步 p95 5.00s（11-10 为 27.85s），回到 16s 以内，所以 `scripts/test-seamless-demo.sh` 最后一步撤掉 `-max-p95 40`、改回 loadtest 缺省 16s。改阈值后没有再整轮复跑（5.00s 远低于 16s）；维护者合入时复跑。

## 9. 工作量（实际）

框架（framework/saga + wiring/saga）约 300 行改动、测试约 600 行；codegen `-routed` 约 80 行与用例；demo 净删交接代码约 250 行；文档。约一个工作日（含生成工程全测与演练）。

## 10. 进度（2026-11-11 暂停）

额度用完，按协调者要求在安全点暂停，改动以一笔 WIP 提交推到 `feat/saga-step-direct-route`（未合 main、未发版）。

**已完成**

- 框架（framework/saga）：`Step.Routed`、`StartRequest.Route`（`SidRoute`）、启动时的路由检查、`ev.to.<route>.<topic>` 路由主题、`Command.Route`、退回 / 拒收（`BounceStep` / `RejectStep`，退回可带新地址并改 `Record.Route`）、WireVersion 4、冷备 Route；参与方按 sid 的 durable 与过滤；`Engine.RoutedTopic`。
- wiring/saga `WithSidRoute`（定义与消费者不一致在订阅前拒绝），删 `RunHandedOver`；sidmerge 清单文字。
- codegen `roost add saga -routed`；game-demo 赠礼 / 公会玩家步骤改路由，删交接代码，`admitBound` 保留。
- 红（修前原文见证据 §1）、绿、5 处框架变异与 2 处生成工程变异都红过；文档（SAGA、guide / impl 06、MMORPG-INTEGRATION、PLAYER-MIGRATION、OBSERVABILITY、WALKTHROUGH、KNOWN-LIMITS、CHANGELOG、本文、证据）。
- 本机私有真实依赖无缝演练 14 步 ok，最后一步 p95 27.85s → 5.00s；`scripts/test-seamless-demo.sh` 撤掉 `-max-p95 40`，私有环境已 `clean`。

**暂停时正在做**：收尾验证。改文档后的全量 `go test ./...` 跑到 codegen 一段时被停掉（已完成的包全 ok，未完成）；重新生成的 game-demo build / vet / 全量 `go test ./...` ok，但 `internal/service/game -race` 红了（见下）。

**已知问题 / 红着的测试（原文）**

```text
FAIL	example.com/sagaroute/internal/service/game	106.143s
```

重新生成的 game-demo（`--skip-deps`，replace 指向本分支）上 `go test -count=1 -race ./internal/service/game`。只保留了最后一行，失败的用例与输出没有保存，**尚未确认**是本分支引入的缺陷还是负载下的偶发；同一包在暂停前第一次生成的工程上 `-race` 通过过（83s），之后只改了文档与演练脚本，没改模板。

**下一步（按顺序）**

1. 重新生成 game-demo，`go test -count=1 -race -v ./internal/service/game 2>&1 | tee` 保存全文，找出失败用例；按证据定性（本分支缺陷则先红后绿修，偶发则多轮复现并记录）。
2. `GOWORK=off go test -count=1 ./...` 全量、根包 `go test -count=1 .`、`python3 scripts/check-doc-anchors.py`、`go generate ./...` 后 porcelain 干净。
3. 把证据 §6 的结果表改成最终结果；`git fetch origin && git merge origin/main`，整理成正式提交（合 main 由维护者决定）。
4. 维护者合入前按 §8 复跑一次无缝演练（阈值已是缺省 16s）。
