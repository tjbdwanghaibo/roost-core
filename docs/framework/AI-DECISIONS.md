# 配置驱动的实体 AI：谁选，谁执行，谁记账

同一实体可以同时需要三种东西：受到刺激时立即选一个目标、花几拍追上目标、在副本里只结算一次。不要给这三件事共用一个树游标：

| 要解决的问题 | 推荐入口 | 能依赖的结果 |
| --- | --- | --- |
| 当前收到的事件或经过的逻辑拍，应选什么策略？ | `gameplay/ai/decision`、`ai.NewDecisionStrategy` | 一步返回新的决策状态和有序意图；不会自己操作实体。 |
| 追击、等待动作完成、条件失效时打断，下拍从哪里继续？ | 既有 `ai.BehaviorStrategy` / `actionflow` | 行为树保留运行中节点；控制器管理抢占、冻结和既有动作生命周期。事件型行为叶子下一拍才取事件。 |
| 房间哪一阶段生效、哪一波已开始、外部结算是否已确认？ | `gameplay/level.LevelComponent` | 关卡实例、代际、来源序号、定时器、待办及完成回执。不是每个怪物的临时选策状态。 |

## 先独立运行一步

新程序的配置由业务代码或严格原生 JSON 提供。原生模式名是 `roost.ai.decision/v1`。下例故意不接数据库和 ActionFlow，以便看清第一步究竟产生了什么：

```go
reg := decision.Registry{Events: []string{"notice"}, Fields: []string{"strength"}, Intents: []string{"select_alert"}}
p, err := decision.Decode([]byte(`{
  "schema":"roost.ai.decision/v1", "name":"watcher", "start":"idle",
  "stages":[{"name":"idle", "events":[{
    "id":1, "event":"notice", "enabled":true,
    "filter":{"field":"strength","op":"ge","value":3},
    "body":[{"kind":"do","actions":[{"id":2,"kind":"emit","name":"select_alert","arg":7}]}]
  }]}]
}`), reg)
if err != nil { return err }
next, intents, err := p.Step(p.NewState(), decision.Input{
    Now: 10, Event: "notice", Fields: map[string]int64{"strength": 4},
})
if err != nil { return err }
// 结果是 {NodeID: 2, Name: "select_alert", Arg: 7}，还没有发起动作。
return host.CommitDecision(next, intents) // 业务接口示意：状态和意图必须一起接受或一起拒绝。
```

`Now` 是同一宿主提供的单调逻辑时刻；同一时刻可以先后投递多个不同事件。不要以 `Now` 当网络请求防重键。空 `Event` 表示真正的一拍；非空 `Event` 是立即投递，不顺带 tick。字段必须来自宿主的可信快照；配置词表不允许任意调用函数。`Once` 表示收到一次该类事件，不等于第一次过滤命中；若要“成功才用掉”不要用 `Once` 冒充。组合节点先检查子节点，再按顺序运行附属动作，`all` / `any` 短路。

## 接回现有控制器

`ai.NewDecisionStrategy(name, program, ai.DecisionHost{...})` 创建一个**选策**策略。`Load` 返回每个宿主自己的带版本状态；`Input` 将可信 `ai.Event` 或 tick 映射到原生输入；`Commit` 按期望版本在同一事务内保存新状态和有序动作意图；`OnError` 把拒绝交还外层 handler。`Controller.OnEvent` 表示事件已交给策略，**不是事务已提交**。恢复时先校验定义身份，不要把旧树状态挂到规则已改的新树上。

需要跨拍或允许中断的工作，由独立执行策略来做。不要把选策器和执行器轮流写进同一个 `Controller` 槽；不要每拍重建执行器。业务要在已承诺的选择与实际执行间建立身份或代际关联，旧回执不能误推进新的一次执行。若意图会跨进程或产生不可撤销的副作用，使用宿主的持久待办和幂等接收方；把 `Step` 结果写到内存或只注册一个回调不等于已落库。

- [可选 `ai` 模块：安装和可执行宿主示例](../../modules/ai/README.md)。无需为未采用决策树的怪物更换现有策略。
- [与 LevelComponent 的同输入轨迹和反例](AI-DECISION-VS-LEVEL.md)。
