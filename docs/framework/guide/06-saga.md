# Saga 长事务：设计与使用

适用版本：v1.27.0 及当前主线（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/06-saga.md) · [模块总目录](../README.md)

## 先理解这一块

有些操作需要多个服务分几步完成，例如先扣道具，再创建跨服活动记录。Saga记录这些步骤，并在失败时按业务定义执行补偿。

补偿不等于把整个世界倒回过去；每个步骤都需要设计重复请求和失败处理方式。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 模型

Saga 是持久步骤状态机，不提供跨服务 ACID。协调器记录计划、步骤、尝试、预算、终态；执行端处理步骤和回执。正向步骤失败后按已经生效的步骤倒序补偿。补偿自身可以失败，需重试、停止或人工介入，不能承诺所有外部副作用自动撤销。

## 2. 正式执行路径

使用 Assemble/Engine 和生成步骤接线；步骤状态转换集中到 stepTransition，避免各个完成入口维护不同规则。任务领取带 owner/incarnation 与期限；旧回调不能完成新一代任务。恢复重新读取持久状态，而非以内存队列是否为空判定业务完成。

原生实体步骤使用 Nest 事务和 DataEngine native inbox，把 DAO 修改与步骤 receipt 放在同一次持久提交内。外部 Mongo 步骤使用 Mongo step inbox，把业务写与 inbox 结论放在一个 Mongo 事务。两者不是可随意互换的幂等层。

Nest 发起的 start（`EmitStart`）与原生步骤的结果都由 DataEngine 发布到效果流（`dataengine.effects.stream` / `subject_prefix`），saga 的 start / 结果消费者订阅同一条：`saga.start_effect_stream` / `start_effect_prefix` 不写时取 `dataengine.effects` 的值，效果流只在一处配置。同一进程注册了 DataEngine 时显式写了不同的值，配置检查（启动前、`--check-config`、doctor 的 `config-check:<service>`）就拒绝并点名两个键（`wiring/saga` 的 `CheckConfigAcrossMods`，经 `app.ModConfigCrossChecker`）；只跑协调器、没有 DataEngine 的进程可以用这两个键指向别的流。完成回执 TTL（`saga.completion_receipt_ttl`）必须长于结果所在效果流的保留期（`dataengine.effects.max_age`）这条跨 Mod 规则同样在 `CheckConfigAcrossMods` 里（D12 顺带项，原先在 saga Mod 的 Init 里，`--check-config` 与 doctor 放过）；它不看本进程是否注册了 DataEngine，因为只跑协调器的进程读的仍是那条流。

### 2.1 业务怎样写步骤（步骤运行时，DESIGN-TODO D12 方向 A）

维护者 2026-10-18 决定把与业务无关的胶水收进框架：业务只登记“名字 → 步骤体”，收件箱、completion 发布器、durable、租约持有者名、等待投影、停止与指标都由 `wiring/saga` 的 `Steps` 按 saga Mod 与 DataEngine Mod 的声明组装（没有新的配置键）。服务在 Init 里 Start、在 Shutdown 里 Drain：

```go
steps, err := kitsaga.NewSteps(registry, "game-gift")            // durable = game-gift-<step>
steps.Native("debit", giftitem.TopicDebit, s.debit, kitsaga.WithAdmit(s.admitOwned))
steps.Native("debit-undo", giftitem.TopicDebitCompensation, s.refund, kitsaga.WithAdmit(s.admitOwned))
steps.Mongo("deliver", giftitem.TopicDeliver, s.deliver)          // coresaga.StepHandler，Mongo 事务里运行
if err := steps.Start(ctx); err != nil { ... }                   // 登记错误（重名、非法名、空处理函数）也在这里返回
// Shutdown：steps.Drain(ctx)；超时再 steps.Stop()
```

- **原生步骤**（业务就是一笔 Nest 事务）：步骤体 `func(ctx, step saga.NativeStep) error` 把 `step` 作为参数交给自己的 Nest handler（`//roost:nest` 的参数可以是 `saga.NativeStep`），handler 在事务里改实体并调用 `step.Complete(success, reason)`。业务修改、命令回执（带 lease fence）与 completion 是同一条 WAL 记录；重投读到回执就回放，不再执行步骤体。业务拒绝同样 `Complete(false, reason)`：只提交回执与不可重试的失败 completion，协调器随即补偿（补偿被拒则 manual_required）。
- **幂等键**是 `Command.IdempotencyKey`（一个操作实例）：同一操作实例的全部尝试至多一次生效，由收件箱保证，步骤体不需要自己的幂等。**结果未知**：步骤体返回错误时 Nest 提交可能已经发生，消费者保留这次尝试的租约，重投等回执到命令截止，之后由协调器按步骤超时发下一次尝试；只有 `dataengine.ErrFencedEntityPending` 交还租约、立刻重投。
- **消费者怎样处理一条原生命令**（2026-10-18 起）：回调里只做 Reserve 与步骤体（Nest 请求），然后推迟这条消息的结算（`JetStreamMsg.DeferSettle`）、立即处理 durable 上的下一条；“等回执投影可见 → 结算状态文档 → ack”在后台完成，同时等待的数目受 `StepConsumerConfig.ReceiptWaiters`（缺省 min(32, MaxAckPending)）约束，满了回调等空位。所以一条命令的投影停顿不再挡住同一 durable 后面的命令；`Steps.Drain` 等到后台 ack 完成才返回。代价：同一 durable 上前后两条命令改**同一实体**、前一条还没投影时，后一条会撞上实体屏障（`ErrFencedEntityPending`：交还租约、按退避 nak，缺省首次 250ms 后重投），而不是在 durable 上排队等前一条；热点实体的连续步骤因此可能多出重投延迟（相邻命令间隔短于一次投影、约几十毫秒时；实测见 [性能优化报告 §6](../../performance/SAGA-OPT-20261018.md#6-热点实体与打点开销)）。
- **Mongo 步骤**：handler 经传入 ctx 的 Mongo 写与回执同一事务；调用别的服务（发邮件）不在事务里，必须按 `IdempotencyKey` 幂等（demo 用它做 mail 的 RequestID）。
- **按 sid / 分片的准入与转交是业务的拓扑**，框架不替业务猜：`WithAdmit` 在取得执行权之前问“这条命令归不归本进程”，拒绝时消息 nak 给别的副本；要把命令直接送到归属进程，由业务自己的通道转交，接收方调用 `steps.RunHandedOver(ctx, name, command, accept)`——先核对 name 是登记过的原生步骤、命令主题与它一致（扣款信封包着退款命令会被拒绝），再由 `accept` 复核归属（不信任发送方），最后自己取执行权运行同一个步骤体；与消费者互斥，不等投影，转交丢失只多耗时间。game-demo 的赠礼 saga（`internal/service/game/gift_saga.go`）是完整样例。
- 底层 API（`SubscribeDataEngineStep` / `SubscribeMongoStep` / `DataEngineStepInbox.StepHandler` / `RunHandedOver`）仍公开，只在需要自定义消费者参数时直接用。

**生成器（DESIGN-TODO D12 方向 B，维护者 2026-10-18 批准）**：上面的写法由 `roost add saga` 与 `roost add effect` 生成骨架，生成物都是业务文件、只建一次，服务的 Init / Shutdown 归业务、生成器不改写（只在 next 提示与 `roost help saga` / `roost help effect` 里给要加的两行）：

```bash
roost add saga gift -service game -steps debit,credit,audit -native debit,credit -entity Player -component Wallet
roost add effect player.level_up -consumer game
```

- `saga/<name>/definition.go`：定义、步骤主题常量、`EmitStart` / `Start`（不再生成 `Subscribe<Step>` 辅助：那是 `Steps` 之前的第二套写法）。
- `saga/<name>/steps.go`：`State` 与 `EncodeState` / `DecodeState`；每步正向与补偿的步骤体（原生步骤解出实体 ID 后调生成的 Sender，Mongo 步骤留 TODO 与“事务外副作用按 `IdempotencyKey` 幂等”的说明）；`RegisterSteps(steps, deps)` 按 `-steps` 顺序、按主题常量登记（`-native` 的用 `Native`，其余 `Mongo`；`WithAdmit` 空着并注释多进程分片要补）；`StartSteps(ctx, registry, deps)` = `NewSteps(registry, "<service>-<name>")` + `RegisterSteps` + `Start`。服务 Init 里一行 `steps, err := gift.StartSteps(context.Background(), registry, gift.StepDeps{})`，Shutdown 里 `steps.Drain(ctx)`（超时 `Stop()`）。
- 每个原生步骤两个 Nest handler：`game/handler/<name>_<step>.go` 与 `<name>_<step>_compensation.go`，参数带 `saga.NativeStep`，锁目标与 `add handler` 同一套解析（`-entity` / `-component`）。`-native` 必须是 `-steps` 的子集，不能重复；服务要有 nest Mod。
- `roost add effect`：`game/effects/<topic>.go`（载荷、`Topic<Topic>`、`Emit<Topic>(key, event)`：在 Nest handler 里调用，effect 与实体修改同一条 WAL 记录）与 `internal/service/<consumer>/<topic>_consumer.go`（`kitdataengine.SubscribeEffects` 骨架，载荷解不开返回 `PoisonEffect`）。消费服务要有 dataengine Mod；同一 effect 的第二个消费服务只建自己的消费者。
- 骨架默认拒绝：原生 handler `step.Complete(false, "… is not implemented")`、Mongo 步骤返回失败的 completion、effect 消费者返回错误（重投、不丢），写完业务之前不会报告成功。`roost project doctor` 的 `saga-steps:<name>` / `effect-consumer:<service>/<func>`（只读 WARN）在生成了却没有服务启动（或启动了步骤却没有 `Drain`）时提示。
- game-demo 的赠礼 saga 保留手写接线（`gift_saga.go`：按 sid 准入与转交要用服务 Init 里才有的玩家驻留表与总线），它的 `saga/gift_item/steps.go` 被换成指向 `gift_saga.go` 的说明；没有按 sid 绑定实体的工程直接用生成的 `StartSteps`。

## 3. 幂等与结果未知

传输可能重复或回复丢失。请求身份、步骤身份、摘要、attempt 和 incarnation 要一起核对；相同请求身份不同内容应冲突。步骤结果未知不能当“没有生效”开始无条件补偿；先按持久收件箱和状态核实。至多一次生效依赖事务性 inbox，外部非事务系统需业务自行提供幂等。

## 4. 预算与终态

总预算、步骤预算、重试退避与 lease 都是独立约束；剩余总预算不能被重试重新充满。完成判定依据必要步骤的持久结论，失败/补偿/停止不能只看当前回调数。超时之后仍可能存在需要收尾的工作，停机必须排空已承担的责任。

## 5. 接入与运维

业务给稳定 ID、可重复执行的步骤输入、补偿策略、有限超时与人工处理入口。观察运行数、待领取、lease、outbox、步骤时延、失败终态和补偿积压。长任务容量应按全链路存储/发布/回执测，不用 handler 微基准代替。

稳定版实测正向20/s、全补偿10/s为已测档；20/s全补偿出现积压。这两档是 Mongo 步骤路径；**原生步骤**（经 Nest + DataEngine）单进程容量低得多，2026-10-18 优化（逐跳打点、投影后唤醒 outbox、去冗余读、等回执移出 durable 回调）后实测：两步原生 saga 正向 5/s、全补偿 3/s 的完成耗时 P50 约 280～310 / 350～420 ms，全补偿 4/s 通过；正向 7～8/s、全补偿 5/s 全部正确完成但延迟秒级；正向 10/s、全补偿 10/s 起步骤超时、重试放大（仍恰好一次，但多数 saga 以 failed / compensated 结束）。第一瓶颈是 DataEngine 投影器串行处理带回执 / effect 的记录，见 [Saga 原生步骤性能优化](../../performance/SAGA-OPT-20261018.md) 与 KNOWN-LIMITS“Saga 原生步骤容量”。同日这些记录的 checkpoint 改为与普通记录合并（[前后对照](../../performance/SAGA-CHECKPOINT-20261018.md)）：投影器约 15 → 20 条/s，正向 8/s P50 517 ms / P99 793 ms、全补偿 5/s P50 688 ms / P99 1.2 s，正向 10/s 全部正确但延迟秒级，正向 12/s、全补偿 7/s 起崩溃（各单样本）；生成器骨架与 `roost help saga` 里的数字仍是合并前的保守值。量大时按 sid 分片多进程（每个进程只执行自己 sid 的实体，原生步骤加 `WithAdmit` 准入，见 §2.1 与 game-demo 赠礼 saga）。生成的 `steps.go`、effect 消费者骨架、`roost add saga` 的 next 提示与 `roost help saga` 都写了这几个数字与分片要求。该测量未包含 Nest 发起意图的完整前段，也不代表与普通业务混跑的容量。生产步骤依赖和故障策略需要业务单独验收。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与导出 API 以对应源码声明为准。
