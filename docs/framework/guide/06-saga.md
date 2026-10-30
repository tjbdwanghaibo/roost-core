# Saga 长事务：设计与使用

跨服交易的使用约束与容量口径见 [Saga：跨服交易](../SAGA.md#跨服交易使用约束)。

**部署依赖**：协调器的权威是 NATS JetStream 分区事件日志（NATS ≥ 2.12.1，生产 R3 文件存储），状态在分区所有者进程的内存里，快照在 JetStream
KV / 对象存储；没有 Redis、没有 Store Service、Mongo 不在协调器的关键路径上（Mongo 步骤的回执与 DataEngine 投影仍要副本集）。生成工程加 saga
时带上 `nats`、`mongo`、`dataengine`，配置里写 `saga.coordinators`（跑协调器的 sid）。契约与运维见
[SAGA.md「协调器：JetStream 分区事件日志」](../SAGA.md#协调器jetstream-分区事件日志)。

适用版本：当前 main 与最新发布版本（之后未发版变化见 [CHANGELOG](../../../CHANGELOG.md)）。[实现与维护入口](../impl/06-saga.md) · [模块总目录](../README.md)

## 先理解这一块

有些操作需要多个服务分几步完成，例如先扣道具，再创建跨服活动记录。Saga记录这些步骤，并在失败时按业务定义执行补偿。

补偿不等于把整个世界倒回过去；每个步骤都需要设计重复请求和失败处理方式。

不熟悉框架名词时，先读 [入门与术语](../../GETTING-STARTED.md)。

## 1. 模型

Saga 是持久步骤状态机，不提供跨服务 ACID。协调器记录计划、步骤、尝试、预算、终态；执行端处理步骤和回执。正向步骤失败后按已经生效的步骤倒序补偿。
补偿自身可以失败，需重试、停止或人工介入，不能承诺所有外部副作用自动撤销。

## 2. 正式执行路径

saga 按 ID（由类型与业务键派生）落在固定分区上；每个分区只有一个写者（`saga.coordinators` 静态分给 sid，App 单实例锁保证一个“服务类型 + sid”一个进程；**部署规则：一个 sid 只能在一种服务类型里装 saga Mod**，否则两种类型的同号进程认领同一组分区、一方被期望序号隔离并 fail-stop，见 [SAGA.md](../SAGA.md) 分区归属），
它取一批输入（启动意图、步骤结果、运维意图）与到期的定时器，用纯函数状态机（`transition.go`，从原协调器原样抽出）判定，再把这一轮的事件一次发布
到分区 subject（PubAck 之后才改内存、才回复）。事件就是新状态加要派发的命令；参与方的 durable 按步骤主题过滤命令事件，执行后把结果直接发回命令
所属分区。旧写者由期望序号隔离、整体 fail-stop。

原生实体步骤是一笔 Nest 事务：业务修改与这一步的回执（放在被改的实体里）同一条 WAL 记录，落盘后结果直接发回协调器。外部 Mongo 步骤在一个 Mongo
事务里读回执、执行业务、写回执。两者不是可随意互换的幂等层。

Nest 发起的 start（`EmitStart`）由 DataEngine 发布到效果流（`dataengine.effects.stream` / `subject_prefix`），装了 saga Mod 的进程的启动中继订阅
同一条转发到分区：`saga.start_effect_stream` / `start_effect_prefix` 不写时取 `dataengine.effects` 的值。同一进程注册了 DataEngine 时显式写了不同的值，
配置检查（启动前、`--check-config`、doctor 的 `config-check:<service>`）就拒绝并点名两个键（`wiring/saga` 的 `CheckConfigAcrossMods`）。同一条规则
还要求准入开启时 `dataengine.projection.max_unacked_records` ≥ 4 × `saga.admission.high_records`。

### 2.1 业务怎样写步骤（步骤运行时）

业务只登记“名字 → 步骤体”；命令消费、结果发布、durable、回执、准入信号、停止与指标都由 `wiring/saga` 的 `Steps` 按 saga Mod 与 DataEngine Mod 的
声明组装（`kitsaga.ParticipantSettings`，没有额外的配置键）。服务在 Init 里 Start、在 Shutdown 里 Drain：

```go
steps, err := kitsaga.NewSteps(registry, "game-gift")            // durable = game-gift-<step>，每条 saga 流上一个
steps.Native("debit", giftitem.TopicDebit, s.debit, kitsaga.WithAdmit(s.admitOwned))
steps.Native("debit-undo", giftitem.TopicDebitCompensation, s.refund, kitsaga.WithAdmit(s.admitOwned))
steps.Mongo("deliver", giftitem.TopicDeliver, s.deliver)          // coresaga.StepHandler，Mongo 事务里运行
if err := steps.Start(ctx); err != nil { ... }                   // 登记错误（重名、非法名、空处理函数）也在这里返回
// Shutdown：steps.Drain(ctx)；超时再 steps.Stop()
```

- **原生步骤**（业务就是一笔 Nest 事务）：步骤体 `func(ctx, step saga.NativeStep) error` 把 `step` 作为参数交给自己的 Nest handler（`//roost:nest`
  的参数可以是 `saga.NativeStep`，`durability` 必须是 strict / pipelined）。生成的 Nest 包装在业务函数之前调用 `step.Begin(第一个锁目标)`：有回执就
  回放、过了截止就跳过；业务在事务里改实体并调用 `step.Complete(success, reason)` 写回执。锁目标的实体要有回执字段（`roost add saga -native -entity X`
  生成 `SagaReceipts` 与 `game/entities/<x>/saga_receipts.go`）。业务拒绝同样 `Complete(false, reason)`：只提交拒绝回执，协调器随即补偿（补偿被拒则
  manual_required）。
- **提交方式**（维护者 2026-10-29）：原生步骤推荐 `durability=pipelined`，`roost add saga -native` 就这样生成；saga 步骤是 Nest pipelined
  白名单的标准用户。生成的注册经 `saga.MustRegisterNativeStepHandler` 记下“这是原生步骤”，`wiring/dataengine` 启动时自动把声明 pipelined 的
  原生步骤补进 `nest.pipelined.allowlist`（写了名单时），并只为它们打开 Nest 异步完成（按 handler 生效，没有开关配置），项目不用手写；
  其他 handler 不受影响，业务自己声明 pipelined 的 handler 也仍是派发 worker 等票据（框架内只有 saga 原生步骤用 pipelined，心跳不支持）。strict 仍可显式选择，但在锁内等 fsync，会占满生产缺省的 short 池。同一实体上紧跟在原生步骤后面的普通请求，回复与 AfterCommit
  排在原生步骤完成之后（维护者 2026-10-29 决定：同实体对外可见效果按提交序），最多多等一次组提交。数字与运维信号见
  [SAGA §原生步骤的提交方式](../SAGA.md#原生步骤的提交方式维护者-2026-10-29-决定)。
- **幂等键**是 `Command.IdempotencyKey`（一个操作实例）：同一操作实例的全部尝试至多一次生效，由实体锁与实体里的回执保证，步骤体不需要自己的幂等。
  **结果未知**：步骤体返回错误时 Nest 提交可能已经发生，消费者 nak，重投读到回执就回放。
- **消费者怎样处理一条命令**：同一订阅同时在途至多 `min(32, MaxAckPending)` 条（原生）/ 1 条（Mongo，`MongoWorkers` 可调）；执行或回放后把结果发到
  命令所属分区的 `in.res`，PubAck 之后才 ack；过期未执行直接 ack。同一实体的事务由 Nest 实体锁串行。
- **Mongo 步骤**：handler 经传入 ctx 的 Mongo 写与回执同一事务；调用别的服务（发邮件）不在事务里，必须按 `IdempotencyKey` 幂等（demo 用它做 mail 的 RequestID）。
- **准入信号**：有原生步骤时 `Steps` 把本进程 DataEngine 投影积压发到准入信号桶，积压过高时新 saga 被拒（`ErrBackpressure`，经 `EmitStart` 的启动会重试）。
- **按 sid / 分片的准入与转交是业务的拓扑**，框架不替业务猜：`WithAdmit` 在执行之前问“这条命令归不归本进程”，拒绝时消息 nak 给别的副本；要把命令
  直接送到归属进程，由业务自己的通道转交，接收方调用 `steps.RunHandedOver(ctx, name, command, accept)`——先核对 name 是登记过的原生步骤、命令主题与它
  一致（扣款信封包着退款命令会被拒绝），再由 `accept` 复核归属（不信任发送方），最后运行同一个步骤体；与消费者的互斥由实体锁与回执保证，不发布、
  不 ack，转交丢失只多耗时间。game-demo 的赠礼 saga（`internal/service/game/gift_saga.go`）是完整样例。
- 底层 API（`SubscribeNativeStep` / `SubscribeMongoStep` / `RunNativeStep` / `NewLoadPublisher`）公开，只在需要自定义消费者参数时直接用。

**生成器**：上面的写法由 `roost add saga` 与 `roost add effect` 生成骨架，生成物都是业务文件、只建一次，服务的 Init / Shutdown 归业务、生成器不改写：

```bash
roost add saga gift -service game -steps debit,credit,audit -native debit,credit -entity Player -component Wallet
roost add effect player.level_up -consumer game
```

- `saga/<name>/definition.go`：定义、步骤主题常量、`EmitStart` / `Start`、按业务键查结果的 `Lookup(ctx, engine, businessKey)`（`ErrNotFound`
  表示还没开始，或结束超过 `saga.terminal_retention`；开启 `saga.cold_copy` 时保留期外的终态从 Mongo 冷副本读回）。
- `saga/<name>/steps.go`：`State` 与编解码；每步正向与补偿的步骤体；`RegisterSteps(steps, deps)` 按 `-steps` 顺序、按主题常量登记；
  `StartSteps(ctx, registry, deps)` = `NewSteps(registry, "<service>-<name>")` + `RegisterSteps` + `Start`。
- 每个原生步骤两个 Nest handler：`game/handler/<name>_<step>.go` 与 `<name>_<step>_compensation.go`，参数带 `saga.NativeStep`，声明
  `//roost:nest rollback=undo durability=pipelined`；`-entity` 的 DAO 加
  回执字段、实体加 `saga_receipts.go`（同一实体的第二个原生 saga 不再改动）。`-native` 必须是 `-steps` 的子集；服务要有 nest Mod。
- 骨架默认拒绝：写完业务之前不会报告成功。`roost project doctor` 的 `saga-steps:<name>` / `effect-consumer:<service>/<func>`（只读 WARN）在生成了却没有
  服务启动时提示。
- game-demo 的赠礼 saga 保留手写接线（`gift_saga.go`：按 sid 准入与转交要用服务 Init 里才有的玩家驻留表与总线），PlayerDao 的回执字段与
  `saga_receipts.go` 也是手写的参考实现。

## 3. 幂等与结果未知

传输可能重复或回复丢失。请求身份、步骤身份、摘要、attempt 和 incarnation 要一起核对；相同请求身份不同内容应冲突（启动摘要不同 → `ErrIdentityConflict`；
同一 CommandID 不同结论 → 身份冲突）。启动的结果未知是 `ErrStartUnconfirmed`：意图已持久，用同一请求重试安全；`ErrBackpressure` 表示这次没有建立，
同样用同一请求重试。步骤结果未知不能当“没有生效”开始无条件补偿；回执是权威。外部非事务系统需业务自行提供幂等。

## 4. 预算与终态

总预算、步骤预算、重试退避都是独立约束；剩余总预算不能被重试重新充满。完成判定依据必要步骤的持久结论（`judgeCompletion` 一处）。终态在内存留
`saga.terminal_retention`（缺省 1h，同业务键去重与迟到成功补偿的窗口），之后剪掉；`ManualRequired` 常驻。超时之后仍可能存在需要收尾的工作，停机必须
排空已承担的责任（`Steps.Drain`、协调器三步停机）。

需要更久的历史查询时开冷副本（`saga.cold_copy.enabled`，缺省关）：协调器进程把终态事件批量投影进 `<saga.database>.sagas`，
`Get` / `GetByBusinessKey` / `List` 在所有者内存里没有这条记录时回落读它。它只读、最终一致（kill -9 后最晚一个 AckWait 追平），**不参与唯一性**：
同一业务键在保留期之后仍可以再开一笔，冷副本里的旧一生被新一生覆盖。冷副本 durable 挡着分区清理，所以开着它就要求 Mongo 可写；关掉时删掉
每条流上的 `-cold-copy` durable（[SAGA.md](../SAGA.md#协调器jetstream-分区事件日志)）。

## 5. 接入与运维

业务给稳定业务键、可重复执行的步骤输入、补偿策略、有限超时与人工处理入口。观察准入状态、分区 fail-stop、清理阻塞、步骤时延、失败终态和补偿积压
（[OBSERVABILITY](../OBSERVABILITY.md#sagacoresaga)）。容量应按全链路测，不用 handler 微基准代替；协调器原型的实测见
[原型报告](../../performance/SAGA-JETSTREAM-LOG-PROTO-20261027.md)，正式实现的压测入口是 `scripts/perf/saga-js-scan.sh`（设计 §12，**尚未跑正式一轮**），
旧 Redis / 旧原生路径的历史数字不再适用。

## 源码与核对范围

当前设计的关键结论、纠正的旧口径及验证限制见 [文档—代码核对表](../../maintenance/CONSISTENCY.md)。本篇不以测试数量证明全部路径正确；具体默认值与
导出 API 以对应源码声明为准。
