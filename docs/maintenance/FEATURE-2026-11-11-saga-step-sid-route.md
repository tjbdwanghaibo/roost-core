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
| **玩家换服 / 不再绑定本服** | 属主核对在执行之前（实体未加载），`BounceStep` 退回协调器：一份退回结果（`Completion.Bounced`，`Route` = 新地址或空），PubAck 之后 ack。退回不算重试（维护者 2026-11-11）：协调器按退回次数退避后重派，不消耗尝试次数（`Record.Bounces`），有新地址就改 `Record.Route`。换服中不给地址 → 照原地址重派；换服完成后旧 sid 退回时给出新 sid → 下一次投到新 sid 执行一次。正向到 saga 截止 → 补偿；补偿方向没有截止，等到属主收下。不用 nak：路由 durable 只有本 sid 绑定，nak 只会投回自己 | 退回只发生在执行之前，零写入；同一操作若更早的一次尝试已在旧 sid 生效，回执随实体搬到新 sid，新 sid 读到回执回放 |
| **属主停机** | 命令留在它的 durable 上（未确认），别的 sid 拿不到；协调器按 Timeout 派下一次尝试到同一地址，用尽走补偿——与此前“别的进程只拒绝、等到超时”的超时与补偿语义相同。属主恢复后积压的各次尝试依次投给它 | 回执：至多一次生效，回放的结果 CommandID 是生效的那次，协调器按“成功接收规则”接收或补偿迟到成功 |
| **重复投递**（JetStream 重投、协调器的新尝试） | 都只到属主；原生步骤读实体里的回执回放，Mongo 步骤读收件箱；结果 MsgID `res.<CommandID>`，协调器按回执摘要去重 | 沿用现有机制，没有新增去重层 |
| **没有路由键的旧载荷** | 启动阶段：有路由步骤而 `StartRequest.Route` 为空 → 拒绝启动（`ErrInvalidRecord`，分区计 `rejected_input_total{reason="route"}`）。步骤阶段（理论上到不了）：载荷没有 sid → `RejectStep`，ERROR 日志，协调器随即补偿 / 人工。不迁移、不兜底 | — |

“换服中”的状态由业务提供。game-demo 的玩家是**静态绑定**，没有单个玩家换服：整服搬机器时 sid 不变（命令在 durable 上等新进程，[PLAYER-MIGRATION §2](../framework/PLAYER-MIGRATION.md)），别名合服是离线操作、要求全部 saga 终态。demo 的 `admitBound` 保留换服中的入口：钩子 `playerMoving`（`giftSteps.moving` / `guildSteps.moving`），缺省 `notMoving` 恒返回“不在换服中”，代码里标 `TODO（维护者 2026-11-11）`；生成工程用例 `TestAStepForAMovingSenderIsBouncedBackToTheSender` 注入钩子证明“换服中 → 退回、不接入；换服后接入”。协调器一侧由框架用例 `TestAStepBouncedDuringAMoveRunsOnceOnTheNewSid` 以替身绑定表证明，见 §7。

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
- 退回不消耗尝试（维护者 2026-11-11），时间上只受 saga 截止约束：正向到截止补偿；**补偿方向没有截止**，退回的补偿（例如退款）按 `BackoffMax` 间隔一直重派到属主收下；~~**没有截止的 saga** 正向也一样一直等。~~ 维护者 2026-11-11 定方案 A：有路由步骤的 saga 启动必须带截止、Resume 不许清截止（见 §7 第 2 条），正向因此总有时间上限；补偿方向仍等属主收下。
- 同一条命令先后被退回两次、两次的地址不同（换服完成前后）：第二份结果按身份冲突被分区跳过（`rejected_input_total{reason="identity"}`，WARN），协调器已按第一份推进，无害但有日志噪声。
- 定义与消费者的路由一致性只在协调器与步骤消费者同进程时由 `Steps.Start` 核对；拆分部署靠两边用同一份生成的定义。
- JetStream durable 数随 sid 数线性增长（§3.2）；没有测 1000 个 sid 量级的服务端开销。
- 别名合服后 A 的路由 durable 是空闲遗留，要运维删（`sidmerge checklist` 已列命令）。

## 7. 待确认

1. **玩家换服的状态来源（维护者 2026-11-11 定：先标 TODO）**：仓库里没有单个玩家改绑 sid 的能力（[PLAYER-MIGRATION §1](../framework/PLAYER-MIGRATION.md#1-先说清迁移的对象)：account 没有修改 `ServerID` 的接口），现有的“迁移”是整服搬机器（sid 不变，不需要退回）与离线别名合服（要求 saga 全部终态）。demo 保留入口：`gift_saga.go` 的钩子 `playerMoving`，缺省 `notMoving` 返回“不在换服中”，标 `TODO（维护者 2026-11-11）`，测试注入；单个玩家换服的功能存在后把它的状态接到这里。退回路径（换服中 / 不绑定本服）与用例保留。KNOWN-LIMITS 同记。
2. **退回不算重试（维护者 2026-11-11 定，已实施）**：退回不消耗尝试次数（`Completion.Bounced` + `Record.Bounces`），按退回次数退避重派，时间上受 saga 截止约束、到期照常补偿；用例 `TestABouncedStepKeepsItsAttemptsAndCompensatesAtTheDeadline`（退回 9 次、MaxAttempts 3，saga 仍在正向；截止时补偿）。由此带出的**待确认**：补偿方向没有截止、没有截止的 saga 正向也没有时间上限，退回会一直重派（按 `BackoffMax` 间隔）。可选：A（推荐）有路由步骤的 saga 启动时必须带截止，补偿方向维持“等到属主收下”（退款只能落在属主上），代价是 `StartRequest` 多一条校验；B 维持现状，靠 `saga.step.bounced_total` 与 `LastError` 监控。
   - **截止（维护者 2026-11-11 定方案 A，已实施）**：有路由步骤的 saga 启动必须带 `StartRequest.DeadlineAt`，缺了 `ErrInvalidRecord`（`checkStartRoute`）；Resume 可以改截止但不许 `ClearDeadline`（`planResume`）。补偿方向保持“等到属主收下”（退款只能落在属主上）。未采用 B（不限制、只靠 `saga.step.bounced_total` 监控）。用例 `TestARoutedSagaMustCarryADeadline`（修前红：`StartSaga with a route and no deadline = saga: engine is closed, want ErrInvalidRecord`；去掉 Resume 检查的变异红：`Resume clearing the deadline of a routed saga = <nil>, want ErrInvalidRecord`）。

## 8. 验证与演练阈值

单元与生成工程的红绿、变异与命令见[证据](evidence/saga-step-sid-route-20261111/README.md)。演练阈值：本机私有真实依赖复跑 14 步 ok，最后一步 p95 5.00s（11-10 为 27.85s），回到 16s 以内，所以 `scripts/test-seamless-demo.sh` 最后一步撤掉 `-max-p95 40`、改回 loadtest 缺省 16s。改回缺省后复跑一次：14 步 ok，最后一步 p95 5.45s（阈值 16s）。

## 9. 工作量（实际）

框架（framework/saga + wiring/saga）约 300 行改动、测试约 600 行；codegen `-routed` 约 80 行与用例；demo 净删交接代码约 250 行；文档。约一个工作日（含生成工程全测与演练）。

## 10. 进度

- 11-11 额度用完暂停，WIP 提交 `e90d1dbe` 推到 `feat/saga-step-direct-route`；当时重新生成的 game-demo 上 `internal/service/game -race` 红过一次。
- 11-11 恢复：按维护者对 §7 的两条决定实施——退回不算重试（`Completion.Bounced` / `Record.Bounces`，用例 `TestABouncedStepKeepsItsAttemptsAndCompensatesAtTheDeadline`）；“换服中”留钩子 `playerMoving` 与 `TODO（维护者 2026-11-11）`（用例 `TestAStepForAMovingSenderIsBouncedBackToTheSender`）。那次 `-race` 失败 5 次复跑没有复现，记为尚未确认（证据 §6）。全量验证与阈值 16s 下的无缝演练（p95 5.45s）通过，结果见证据 §5、§6。
- 没有合入 main、没有发版；合入由维护者决定。§7 第 2 条的截止问题维护者已定 A 并实施。
