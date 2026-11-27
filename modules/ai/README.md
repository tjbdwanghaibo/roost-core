# ai：选哪种行为，与怎样执行它

## 能力

`roost module add ai` 安装一个无需旧玩法的服务端样例：值守点在 `watch`（观察）和 `assist`（协助）之间选择。**一个 Controller 跑选择器，另一个 Controller 保留并推进当前执行策略。** 它们不注册心跳，也不占用 Monster 的 AI 槽。没有客户端协议、自动启动入口或新增组件 ID。

- `game/modules/decisionai/definition.go`：`Vocabulary` 注册字段 `needs_help`、本地事件 `help`、意图 `watch/assist`；`SampleProgram` 用互斥条件选策。`Tick` 从可信 `Frame` 取事实；`Event(CallForHelp)` 同一逻辑拍立即选择 `assist`，不顺带跑 Tick。事件不接受客户端 Data。
- `NewSelector(p)` / `NewSelectorWithProgram(p, program)` 把核心 `ai.DecisionStrategy` 安到选择器的 `ai.Controller`。宿主显式加载、映射输入、比较版本并提交意图。**每次输入只能产生一个无载荷的合法选择**，零个、多个、未知名字均报错；不取第一个或最后一个糊弄。可在实体间共享只读 Program，不能共享 Selector 或 Executor。
- `PlayerDao.AiDecision`（`ai_decision`，只持久化，不下发）：一次生成的 DAO setter 写完整 `Version`、`StateJSON`、`Desired`。严格 Nest undo 事务拒绝时三者一起回滚；`PublishChoice` 只在外层事务成功后发布所选名字及版本。`Desired` 是期望值，不代表策略已成功切换。
- `NewExecutor(p, hooks, factories)` 按名字找工厂。`Apply` 只接受对应 Player 实例的最新已提交 `Choice`；同名更新版本但不重建树。`WorkFactories` 给两个名字各创建独立的 `BehaviorStrategy` 与 `TaskflowAction` 等待叶子；业务提供 `Start`、`Abort`、`Result`。运行中的叶子可跨拍等待；实例被替换或 `Close` 时按启动时的动作 ID 收尾。只有结构化的成功结束才判成功。

## 能力边界

本样例的意图是**可覆盖的期望状态**：若 `watch → assist → watch` 都已经提交，但执行器还没收到通知，那么只需实现最新 `watch`。这不是需要逐条送达的邮件、扣费或施放动作队列。那类意图须在同一事务写业务待办（含稳定键），消费端幂等处理；不可在 `DecisionHost.Commit` 里先执行动作再写档。

`Frame.Sequence` 从 1 连续递增，每个输入（事件也算）占一个序号，可与 `Frame.Tick` 不同；重试必须复用同一序号。`ErrReplay` 不重算、不重新发布；响应不明时重新读最新已提交选择。跳号 `ErrSequence`，逻辑时间不得倒退。**调用方负责给可信输入分配稳定序号**，不能把客户端提供的序号、事实、时钟直接投给选择器。并不按请求内容识别伪装成新序号的重复消息。

恢复要经过 `Program.DecodeState` 校验字段、计划身份与形状；不能把不同规则的旧档默认为初始状态。改规则需显式迁移或在另一个状态槽启动新版本。运行中的执行树、动作与 `Lease` 都是**进程内**实例，不是存档；重启只恢复期望策略和决策游标。会产生不可逆效果的业务动作，另设持久动作标识、去重和故障恢复。这里不宣称动作跨崩溃 exactly-once。

## 复用方式

在支持 Player、DAO、Nest 的生成工程运行：

```sh
roost module add ai
roost module verify --json
go test -count=1 -v ./game/modules/decisionai -run '^TestDecisionAI'
```

**可执行例子**是安装后的 `game/modules/decisionai/decision_test.go`，尤其 `TestDecisionAIRunningAndInterrupt`：用 `NewSelector → Tick/PublishChoice → NewExecutor → Apply/Tick` 贯通两个真实 Controller，用同名选择、异名选择先后验证保留 Running 与中断。`fixture_test.go` 提供真实 Nest `RollbackUndo`、`DurabilityStrict` handler、Player 和提交器接线。

生产接入用同样的顺序：

1. 在拥有此 Player 的严格 Nest undo handler 中构造 Selector（每个实体可复用）。传 `Frame` 调 `Tick` 或 `Event`，把错误返回给 Nest。也可先用 `decision.Decode(raw, decisionai.Vocabulary())` 编译业务原生定义，再传 `NewSelectorWithProgram`；要换词表、输入来源或不止一个意图，另写自己的 `ai.DecisionHost`。
2. 在选择 handler **最后**调用 `PublishChoice`。提交后回调只接收不可变选择；不要在已解锁的回调里驱动实体。用线程安全邮箱或调用方局部值接收；进程在发布前中断，可从新装载的 Player 再 `PublishChoice`，不用重跑已经提交的输入。
3. 在后续该实体的 Guard 内 `Apply(choice)` 并检查返回值，再按宿主拍 `Tick(now)`。未选中时不运行；`Active()` 才是最后一次成功应用的选择。切换被拒、工厂错误或新策略 Init 失败时旧实例仍能继续 Tick。同一执行器只建一次，不在每拍重建，也不连续把两个策略放进同一 Controller。
4. 业务启动动作时捕获本次 `Lease`，回执到达时在同一 Player Guard 内用这个 `lease.ActionEnd(id, kind, reason)` 发送。过期实例不再能投递，即使动作数字 ID 被新实例重用。一次实例内仍要求独立动作 ID，不把“收到回执时的当前 lease”错当成启动时 lease。卸载时在有 owner guard 和 undo 的生命周期业务入口 `Close`。选择器、执行器的宿主回调不做网络或磁盘 I/O。

## 联动

无业务模块依赖。需要真实动作组件时沿用现有 `roost add behavior --entity Player --ai`（已有则复用，不要重复添加）。在实体包外的业务 handler 或装配包取 `player.BehaviorOf(target).Behavior()`，将其作为 `ai.Context.ActionList`，用注册好的 kind/参数 `CreateAction`；中断只 `EndAction` 自己启动的 ID。回执按启动时捕获的 lease 路由；不要覆盖正在服务其他策略的行为观察者。动作允许在 Start 里同步完成，业务桥也要处理这种回执，不能假设 CreateAction 返回后才结束。

本模块不替用户选择实体主时钟，不需要 actor，也不覆盖生成的 `onBehaviorReady`、`ExampleStrategy`、全局冻结或动作回执语义。不要另开周期器重复驱动执行器；若已有 `--ai` 心跳，应由业务明确选一个驱动源、保留既有策略所有权。原生定义与其他行为树或事件体系并非自动兼容。

## 验证范围

`module.yaml` 声明 **6 个顶层场景**，不以测试包编译替代运行：

| 场景 | 判据 |
| --- | --- |
| 同拍输入 | 一个事务里 Tick 后立即投事件，最后选 `assist`；重复序号、跳号和多余事件数据均不写入 |
| 拒绝与恢复 | 提交器拒绝，状态/选择全退且不发布；用同一序号重试一次，再从真实 DAO 完整提交的 BSON 文件经 `RestorePersisted` 重建 Player，不重复承诺 |
| 跨拍运行、中断 | 相同选择连续 Apply/Tick 只启动一次；换名只中断旧叶子；关闭中断当前叶子 |
| 切换失败 | 旧策略拒绝和新 Init 失败，都保留旧实例、进度、活动版本；故障解除后可应用同一选择 |
| 回执隔离 | 新旧执行实例故意使用相同动作 ID，旧 lease 不能推进新树，正确 lease 可以，关闭后不能 |
| 拒绝错误宿主与配置 | 无锁、已持锁但无 undo、同 ID 异实例、注入双意图定义均零写；不接纳不同计划、重复字段或未知字段的存档 |

持久化覆盖真实生成 DAO 编解码、真实 Nest 回滚、可拒绝提交器以及从提交字节新建实体。未连生产数据库、未运行跨进程的动作恢复；动作启动/中断用可观察的业务叶子钩子验证，非实机负载或网络验收。
