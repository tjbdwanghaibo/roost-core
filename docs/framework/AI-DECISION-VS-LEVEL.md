# AI 决策树与关卡 ECA：同一组事实，两种承诺

单个 AI 要决定“看到信号后做什么”，用 `gameplay/ai/decision`；副本要知道“刷怪请求是不是已提交、这份回执能否推进关卡”，用 `gameplay/level`。两者都能用事件、条件、阶段描述规则，**这不意味着它们拥有相同的状态、时间和交付保证**。level 是实体拥有的事件—条件—动作（ECA）组件，不是每拍遍历的行为树。需要动作跨拍返回 Running/Success/Failure 的策略，另看 [AI 行为树](../../gameplay/ai/behavior_strategy.go)。

## 选择与接入

| | AI：决定提出哪些意图 | Level：推进哪个权威关卡实例 |
|---|---|---|
| 加载与实例 | `decision.Compile(d, reg)` 或 `decision.Decode(raw, reg)` 加载原生 `roost.ai.decision/v1`。`Program.NewState()` 为每个实体建立独立状态；恢复后调用 `ValidateState`。 | `level.DecodeDefinition(raw)` → `level.Compile(d, reg)` → `NewLevelComponent(dao, program)`；在 Nest 事务中 `Initialize(instance, epoch, now)`、`Start(now)`。定义版本、注册表和持久快照须匹配。 |
| 何时求值 | `next, intents, err := p.Step(s, decision.Input{Now: t, Event: kind, Fields: facts})`。`t` 是逻辑拍；事件立即派发，只有 `Event == ""` 才跑 tick 根并处理到期延迟。一个当前阶段，按定义的监听顺序派发。 | `Handle(Event{Instance, Epoch, Source, Sequence, Kind, Fields, …}, now)` 处理可信事实；`AdvanceTick(t)` 或毫秒域 `OnBeat` 到期才推进定时工作。`Phase` 是关卡生命周期，`Active` 可有多个阶段；阶段重入换 generation（同名阶段的不同轮次）。 |
| 产物与提交 | `Intent{NodeID, Name, Arg}` 是建议，不是回执或执行票据。用 [DecisionStrategy / DecisionHost](../../gameplay/ai/decision_strategy.go) 接 `ai.Controller` 时，宿主 `Load/Input/Commit/OnError` 负责权威输入、状态版本和事务。把**下一状态与全部意图**原子记录，提交后才执行；可重投的输入另需业务幂等键。版本比较不能替代输入去重。 | `request` 在 DAO 中产生带唯一实例内身份的 `Effect`。事务 settled 后读 `PendingActions()`，宿主按 `ID` 幂等执行，再 `Complete(Receipt{Instance, Epoch, ID, Success}, now)`。同阶段同代可派发内置 `action.done`。没有执行适配器，pending 本身不会刷怪或发奖。 |

接口依据：[decision 类型](../../gameplay/ai/decision/types.go)、[Step 实现](../../gameplay/ai/decision/step.go)、[level 定义](../../gameplay/level/definition.go)、[编译](../../gameplay/level/compile.go)、[收件/回执与执行队列](../../gameplay/level/component.go)、[时钟](../../gameplay/level/time.go)。持久/瞬态 DAO、阶段结束时清理、终局关闭和公开投影的接线见 [LevelComponent 手册](LEVEL-COMPONENT.md)。可以让关卡拥有“本波是否已清空”，AI 消费宿主给的事实来选择下一动作；不要让两边各建一份刷怪或结算账本。

## 可执行对照：只比较真正相同的观察量

[测试](../../gameplay/level/decision_comparison_test.go) 分别加载手写的 [decision 原生定义](../../gameplay/level/testdata/decision_comparison/decision.json) 和 [level 原生定义](../../gameplay/level/testdata/decision_comparison/level.json)。不是转译器。decision 运行真实 `Program.Step`；level 复用 [newHarness](../../gameplay/level/component_test.go) 的真实 `LevelComponent`、瞬态 `LevelDAO`、Nest handler 与 `RequestSettled`。没有用脚本模拟关卡规则。复跑：

```sh
go test -race -count=1 -run '^TestDecisionComparison$' -v ./gameplay/level
```

**严格交集 `shared_trace`**：两边只有一个活动阶段，`sight/order` 取同一服务器 `score`（阈值 ≥2），每次 `arm` 在本例中只投递一次，不跨阶段。每一行使用同一份预期的阶段和**本步新增**动作有序列表。`signal(7)`/`flash(9)` 指名字与全部载荷（decision 的 `Arg` 对 level 的唯一整数参数 `channel`），不是执行成功。level 旧 pending 不重复计数。

| 逻辑拍 | 实际输入（事件依次为同一 source 的 seq 1–4） | 两侧活动阶段 | 两侧新增动作 |
|---:|---|---|---|
| 0 | 初始状态 / 启动关卡 | `watch` | 无 |
| 1 | `sight(score=1)` | `watch` | 无 |
| 2 | `sight(score=2)` | `alert` | 无 |
| 3 | `order(score=2)` | `alert` | `signal(7)` × 1，当步 |
| 4 | `arm(score=0)` | `alert` | 无；两侧期限实读为 6 |
| 5 | 显式 tick | `alert` | 无 |
| 6 | 显式 tick | `alert` | `flash(9)` × 1 |
| 9 | 显式 tick | `alert` | 无，不补发 |

拍 2 是 decision 请求单阶段切换，level 则在同一事务排队 `end watch`、`start alert`。拍 4 是 decision 延迟一个 emit，level 则设一个阶段定时器，到期通过 `timer` 规则请求动作。**所给输入下的轨迹相同，不是两个计时器、所有阶段切换或持久化契约相同。**

### 反例一：一次性订阅 ≠ 最多命中一次

`once_is_not_max_fires` 在 `watch` 连续投递三个全新 `probe`，分数依次为 **1、2、3**。第 1 拍两边均不输出，但 decision 的 `Once` 监听已关闭，另一个监听仍开启；level 的 `MaxFires: 1` 尚未计次。第 2 拍只有 level 请求 `signal(7)`；第 3 拍都没有新动作。逐步核对的 level `Count` 是 **0、1、1**，唯一 `LastAt` 是 **2**。换一个新实例，以 `score=2` 开始时两边首步均请求一次，随后都不再请求——排除了监听器本来就关着的解释。

### 反例二：重复输入 ≠ 重复读取 pending；意图 ≠ 回执

`replay_and_receipt` 先在第 1 拍进入 `alert`，然后检查以下边界。level 用默认回执留存策略，未开启 `ForgetSettledActions()`：

| 拍 / 输入 | decision 真实输出 | level 已提交状态 / 返回值 |
|---|---|---|
| 3：`order(2)`，seq=2；同拍原样重投 | 两次各得一个 `signal(7)`，同一个 `NodeID` | 首次建一个 pending。重投返回 nil；整份快照不变（包括游标、revision、动作 ID） |
| 4：seq=2 改为 `order(3)`；5：旧 seq=1；6：seq=3 但旧 epoch | 三步各得一个 `signal(7)`；`Input` 不含这些信封字段 | 分别 `ErrConflict`、`ErrStale`、`ErrStale`；均不改快照 |
| 7：旧 epoch 回执；8：正确 ID、epoch、成功回执 | 没有可调用的内置 `Complete`，测试不伪造 AI 输入 | 先 `ErrStale`，仍 pending；后置为 done，从 pending 消失，`action.done` 规则使全局 `acks` 由 0 变 1 |
| 9：原成功回执；10：同 ID 改报失败 | 同上 | 重复成功返回 nil，冲突回执 `ErrConflict`；快照和 `acks=1` 不变 |
| 11：新 seq=3、当前 epoch，`order(2)` | 同一节点又得一个 `signal(7)` | 这是新事实：新请求 ID，effect sequence 加 1，来源游标推进到 3 |

这里故意把旧信封的事件载荷直接喂给决策内核，**不是建议宿主接受过期输入**。`NodeID` 是配置节点身份，不是每次发生的请求 ID；decision 的 `State.Pending` 是尚未到时的 emit，不是 level 那种“已提交、等待外部执行结果”的 pending。如果 AI 应等刷怪结果，宿主须按实例和请求身份验证执行回报，再作为允许的业务事件输入，不能看到 intent 就当作成功。

## SSR 的“副本决策树”实际从哪里进来？

以下是 SSR 检出 `854fe4f7` 的**源码审阅**，路径相对参考仓库的 `ssr/`；不需要把该仓库接入 core 测试。

1. **辨认调试入口。** `tools/debug_http/web.go` 的 `Sync_GetLevelDebugStateTree` 与 `Sync_GetAIDebugStateTree` 是两条 RPC；副本走 `kingdom/modules/level/internal/level_debug.go` → `level_agent.go:KLAGetLevelDebugStateTree`，枚举 `ecaMan.EngineMan().RangeOnProcessTriggers`。这不是 `kingdom/modules/ai/internal/strategy_ai_editor.go` 的 AI，也不是 `kingdom/modules/bt_engine`。
2. **从地图配置到模板。** `kingdom/modules/level/level_mod/level_mod.go:init` 注册 `EcaSystemLevel` 的模板与事件参数构造器；`kingdom/view/mod_eca_man.go:BuildAllLevelTriggerStub` 从编辑器模式的 `MapInstance.EditorName` 取阶段和 JSON 路径，以副本配置 ID 建 stub。`kingdom/modules/trigger_engine/internal/builder.go:genConfDataFromJson/buildStage` 根据 `root/child` 建树，根必须是 event；`kingdom/modules/level/internal/trigger/level_trigger_manager.go` 为项目专有的 event、condition、action、query 查注册表。不是给 level 或 decision 的 JSON 换一个 schema 名。
3. **从实例到事件。** `kingdom/modules/level/internal/level.go` 构建 trigger 导演，`level_director_trigger.go:newTrigger` 以当前副本作 parent，注入运行数据并挂心跳；`kingdom/modules/eca_man/internal/engine_man.go:innerInit/StartEngine` 取 stub、`Copy()`、`InitRunData`。`level_control.go:doStartStage` 开启阶段；例如 `level_director_trigger.go:EventEnterRegion` 把目标、区域、名称交给 `eca_man/internal/eca_man.go:OnEvent` 建 `EventData`，继而调用 `trigger_engine/internal/engine.go:OnEvent`。`OnTick` 则推进定时回调，不是遍历一棵 AI 行为树。

### 可映射到 level 的交集，以及必须改写的边界

| SSR 副本源码中的事实（下列 `internal/…` 均指 `kingdom/modules/trigger_engine/internal/`） | 对 `gameplay/level` 的结论 |
|---|---|
| `internal/engine.go:StartStage/StopStage` 管多个活动阶段；事件类型索引找到当前触发器；`internal/stage.go:OnStart` 开启配置为 open 的根。 | 可映射多阶段、简单事件 + 纯条件 + 动作，以及全局/阶段/触发器作用域。先把数值事件/位置参数变成注册过的权威字段；阶段重入、清理和 generation 要按 level 合同设计，不能假定相同。 |
| `internal/engine.go:OnEvent` 在 `t.OnTrigger(...) == true && !t.Repeat()` 时才停。`internal/trigger.go:OnTrigger` 返回事件根的 `do`；`internal/condition.go:conditionBase.do` 只用**本节点** checker 判成败，命中后遍历子节点，忽略子节点返回值。 | 若只有事件根过滤，且子动作无额外条件，可用 `MaxFires: 1` 表达首次命中；有子条件时，**根命中、子条件未中也会停 SSR 非重复触发器**。把所有条件拍平成 level `Conditions` 会多等一次。与 SSR **AI** 的 `kingdom/modules/ai_engine/internal/engine.go:OnEvent` 更不同：后者在派发后关非重复监听，不看过滤结果。 |
| `internal/selector.go` 依序尝试条件子树，首个成功即停；`internal/run_data.go:RangeOnProcessTriggerByEventType` 在同类触发器间遍历 map。还可 `StartTrigger/StopTrigger` 动态开闭。 | level 的 `Conditions` 是合取，同事件下按阶段名与定义的触发器顺序执行；没有这个 selector 或任意触发器开关。把选择器子树摊成多个可同时命中的规则会多发动作；要显式编码互斥/作用域并逐轨迹验证。不要为 SSR 同类监听虚构稳定顺序。 |
| `internal/action.go` 在 trigger 数据内按**动作节点**记 `MaxDoCount`；worker 直接取得 RunData。延迟用 `NowNano` 和回调；`internal/run_data.go:AddDelayFunc` 为同一动作 ID 可存多个未执行回调。 | level `MaxFires`/Cooldown 约束的是整个触发器；命名 timer 在同阶段同名时替换，不是挂多份闭包，阶段移除还会清理。项目查询/刷怪/区域资格必须移到权威宿主；跨实体动作改为 `request → 已提交 pending → 幂等执行 → Complete`。不要用动作节点 ID 冒充新回执 ID，或承诺与源端的执行/恢复时机相同。 |

**已测。** 上述 golden 离线 `-race -count=1` 三个子场景均通过。完整默认包 `go test -race -count=1 ./gameplay/level` 也通过（15 个顶层测试）。对照本身使用两套 **core** 运行时及 level 的内存 Nest 事务；表中的相同/不同仅指指定输入下的实测结果。

**未测。** SSR 入口和节点细节是静态审阅，没有运行 SSR、回放真实线上副本资产。本对照没有运行 `integration` 标签的崩溃恢复测试，没有验证网络、跨服提交或外部动作执行，也不据此迁移旧存档。本文不提供 SSR 副本资产导入器，不改变 level、现有 BT 或 AI 的默认装配。
