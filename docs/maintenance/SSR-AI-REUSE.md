# SSR 的行为树、决策树与 AI strategy：可复用的层次

评估范围：SSR `854fe4f7`，以下 SSR 路径相对 `ssr/kingdom/`；core 隔离树基点 `24b704bf`。本文是代码对照与建议，不是已交付的导入器或新的 AI 模块。本轮只实现道具使用和通用额度，没有改 `gameplay/ai`、`actionflow` 或怪物行为。

这里把“AI 决策树”按真实调试入口辨认：SSR `tools/debug_http/web.go` 的 AI 调试路由进入 `modules/ai/internal/debug_ai.go`，枚举当前 `AIEditor.engine` 的触发器；另一个**副本决策树**入口 `modules/level/internal/level_agent.go` 查的是 `trigger_engine`，不是同一个解释器。不要仅凭编辑器界面上的“树”字判断格式。若目标是**副本决策树**，建议另走 `trigger_engine → gameplay/level` 的对照：后者已有 Stage、事件—条件—动作、带触发上限/冷却的规则、定时器与可持久化的外部效果请求（`gameplay/level/definition.go` 的 `TriggerDefinition` / `StageDefinition`）。这是一条候选映射，不是已支持旧副本配置，也不该装进每个怪物的策略控制器。

## 三者不是同一种运行时

| 源端与调用路径 | core 现状 | 建议 |
|---|---|---|
| **行为树** `modules/bt_engine`：节点返回 success/failure/running；`internal/sequence.go`、`selector.go`、`parallel.go` 定义组合语义。这个版本的 `engineImpl.InitRunData` 是空实现，`newSelector` 没有返回构造出的节点，不能当作经过验证的迁移基线。 | `gameplay/ai/{tree,nodes,tree_parser,behavior_strategy}.go` 已有跨拍节点、组合／装饰器、叶子工厂、严格的 `roost.ai/v1` JSON、stage 和等待 actionflow 动作完成的叶子。 | **复用 core 行为树，不照抄执行器**。先判断真实资产是否用此格式、有哪些节点；若确需旧格式，做显式导入与差异报错。名称相同也不代表规则等价，例如任意成功／失败数量阈值不能随意改成全成功或任一成功。 |
| **事件／阶段决策树** `modules/ai_engine`：`modules/ai/internal/editor_stub_manager.go:BuildAllAIEditorStub` 读命名 JSON 与数值模板词表；`strategy_ai_editor.go:Init` 复制 stub、绑定实体和数据；`ai_engine/internal/engine.go:OnEvent/OnTick/SetNextStage` 立即处理已开启的事件分支，遍历中请求的开关事件或切阶段推迟到本轮执行结束。规则的条件、动作与事件解释器来自项目注册表。 | `BehaviorStrategy.OnEvent` 是有界缓存，到下一拍消费；`ParseStages` 和 `SetStage` 支持阶段，但不是 SSR 动态订阅和即时事件运行时。`gameplay/level` 也有事件、变量与动作，但拥有自己的关卡状态／持久化语义。 | **最有提取价值，但不能直接序列化成 core 的行为树**。可先做纯决策计划（编译+每个宿主的状态+输出意图），将即时事件、订阅开闭、一次性触发、延迟动作和阶段切换顺序列为显式合同；无支持的模板 ID / 运行模式应在导入时拒绝。不要把规则决策树当成学习型分类模型。 |
| **AI strategy／导演层** `modules/ai/internal/{ai_ctrl,strategy_factory,director,director_priority}.go`：`StartSelectStrategy` 启用选策略的导演；当前策略、挑选策略与其它导演不是同一个角色，后面还接游戏专有事件和技能。 | `gameplay/ai/controller.go` 的 `SetStrategy` 已有旧策略可否退出、新策略初始化失败保留旧策略、延后切换、冻结/恢复；`gameplay/actionflow` 驱动动作。生成器已有 `roost add behavior --entity … --ai` 的可选心跳与动作回执接线。`modules/monster/.../brain.go.tmpl:Decide` 是保留的短命适配，不是正式热路径：实际是 `game/components/monsteractor` 的 `Body.AfterTick → Decide → Sense/Nearest → Body.Follow/Walk`。不要让新模块成为第二个移动或心跳所有者。 | **复用控制器和动作执行，在确有使用者时再抽选策略／优先级仲裁**。将 SSR 的嘲讽、复活、目标选择等具体规则留为项目适配；可选 `modules/ai` 应负责装配已编译定义、实体输入、时钟与叶子，不再创建另一个通用控制器。 |

**三个不能靠改名解决的差异**：两个 SSR 方言的数值节点类型并非统一，例如 type 5 在 `ai_engine` 是事件、在 `bt_engine` 是并行；`ai_engine` 的非重复订阅在被派发后关闭，即使此次过滤没通过；SSR AICtrl 与 core 对冻结期间的动作完成回执、解冻的当拍是否决策，也有不同处理。SSR 选策略的导演和当前执行策略可以同时存在，不能连续向 core 的单一策略槽写两次来模拟。具体技能选择/施放、仇恨和视野适配仍由业务提供。

## 实施入口与原评估的关系

后续实现已把**实体决策**单独放在 `gameplay/ai/decision`，用 `ai.DecisionStrategy` 接到原控制器。见[原生定义及策略选型](../framework/AI-DECISIONS.md)、[SSR 旧方言允许/拒绝清单及固定版本回放](../../gameplay/ai/decision/ssrimport/README.md)、[与实际 LevelComponent 的逐步对照](../framework/AI-DECISION-VS-LEVEL.md)、[可选安装模块](../../modules/ai/README.md)。这些链接对应当前实现；以下保留拆分时的范围和基线，不是全部完成项或线上资产兼容声明。

## 第一块值得验证的切片（原评估）

1. 选一个真实 SSR 决策定义和它实际使用的模板 ID／比较方式／参数（本次未拿到经确认的线上编辑器资产和对应执行轨迹），另选一个可跨拍动作；先生成支持清单（能直接映射、需适配、拒绝并说明）。光看到 JSON 文件或看到同名节点不能证明兼容。
2. 固定初始状态、时钟、随机序列和同拍事件次序。在源端与目标端分别记下：事件是否命中、动作及其次数、何时切阶段、下拍是否又执行一遍。特别测“一次性事件请求切阶段，同一拍还有后续事件”以及“旧策略不准抢占／新策略初始化出错，原动作是否还在”。不要以最后一个目标 ID 一样代替轨迹对照。
3. 若玩法要求重启／迁属后继续运行，**明确哪些事实写 DAO**（如阶段、定时期限、动作业务键）；闭包、树节点实例和进程内黑板不是持久化格式。在宿主事务拒绝和重载时验证不丢动作、不重发奖。SSR `ai_engine/internal/run_data.go` 的延迟回调是内存函数，不能直接复制进存档。没有这种需求的短命怪物则不用一律持久化。
4. 最后才做安装层：把控制器安装、业务叶子注册、动作适配、配置选择与预检交给生成工程；不复制 SSR 的城堡、兵种、场景单例或 AOI 实现。怪物仍用其自身的权威状态，AI 不另造视野或角色状态来源。

**拆分前基线：**`go test -race -count=1 ./gameplay/ai ./gameplay/actionflow` 通过 AI 32 个、actionflow 68 个顶层场景。**实施后的证据：**固定 SSR 提交的 21 个原始代码文件在隔离进程中运行人工树；原版与导入后的十步活动根、活动监听集合、有序动作均吻合。`level` 另用两份原生配置比较阶段、延迟和效果回执，不走 SSR AI 导入器。没有运行完整 SSR 服务或部署中的真实编辑器资产，不能由人工夹具推断全量旧树兼容。
