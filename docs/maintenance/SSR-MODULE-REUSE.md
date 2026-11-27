# SSR 的活动、任务组件：哪些适合 core 的可选模块

只读比较：SSR `854fe4f7`，core `main@58f47cbe`。SSR 路径以下相对 `ssr/kingdom/`（单仓的业务子树），core 路径相对仓库根。本评估不修改 activity 或 task；列出的用例是建议验收项，不是本轮跑过的测试。

## 优先顺序

| 顺序 | 建议 | 归属 | 不改变的现有合同 |
|---|---|---|---|
| 1 | 任务事件保留对象键，区分累计量与权威现状 | `modules/task` + 已安装的玩法桥 | 上游首次生效时才发布；已有同一事务内推进、发奖和已领事实不变 |
| 2 | 允许指定活动从可信历史事实恢复首次激活锚点 | `modules/activity` 的可选事件适配 | 不取客户端时间，不用当前状态伪造原时间；不改已激活实例的冻结规则、期次或总截止 |
| 3 | 排行奖励在封榜时冻结实际奖单，重试只读奖单 | `activity` 与 `leaderboard/mailbox` 的可选联动 | 不把外部邮件的“已请求”当“已送达”；不把排行榜服务或发奖服务重写进活动内核 |
| 4 | 受限的全满足／任一满足前置组 | 用到多前置的任务定义 | 旧 `Previous` 意指前置**已领取**，不是仅达标；不允许一条事件顺着新解锁链重复消费 |

### 任务：为什么一个事件字符串不总是够用

SSR `modules/bag/internal/callback.go:onAddItem` 发物品号和数量，`modules/task/internal/condition/cond_inc_item.go:CondIncItem` 按物品号订阅，累计模式加本次增量，现状模式重读权威持有量。这里的物品号用于选择条件，**不是防重流水号**。core 原本已经按事件索引；差别在 `modules/task/with/shop/game/components/task/task_shop.go.tmpl:OnShopBought` 只向任务传 `shop.bought` 和购买份数。上游可信事件还有商品和物品维度，不必注册大量由业务 ID 拼出来的事件名。

最小验收：A、B 两个商品共用事件类型，购 A 三份只推进 A；重放原购买回执仍是三份；现状任务对同一库存读取两次不累加。保留当前业务桥的不同可靠性：商店首次提交与任务在同一事务，独立事务的尽力通知不能泛化成恰好一次。如果要支持“目前持有”资格，先明确持有量下降是否撤销可领，不能以累计任务封顶的状态机替它决定。

组合条件参考 SSR `modules/task/internal/condition/cond_task_finished.go:CondTaskFinished` 和 `modules/taskcore/impl/cond.go`，而不是照搬所有枚举和状态。core `modules/task/files/game/components/task/catalog.go.tmpl` 已验证单前置、周期、成环；`task.go.tmpl:unlocked/Report/Claim` 已区分前置领奖、当次事件和发奖。同一组里先校验 ID、周期与环，再测“A 已领、B 仅达标时 C 仍锁；领 B 后 C 只消费下一次事件”。不重复建设规则表、每日周重置或任务领取事务。

### 活动：先有事实、后加载活动

SSR `modules/activity/internal/player_activity.go:EventTaskStateChange` 接实时事件，`modules/activity/internal/event.go` 能查询完成事实；`modules/task/internal/task.go` 的相应入口读的是持久完成时间。core `modules/activity/files/game/components/activity/engine.go.tmpl:Activate` 已能接收 `factTime` 和 `now`，可复用这个区别；通常事件入口 `Reconcile` 取当下时间。新增点应只是显式允许追溯的业务适配器，例如 `LookupFact(owner,event) → 时间/不存在/错误`。

验收：昨天完成任务，今天首次遇到绑定定义，窗口按昨天计算；重复加载不平移窗口；事实缺失不激活，查询失败绝不能记作永久不存在。无可信历史时间的玩法不启用。不要在 Nest 快事务中同步查远端，也不默认补发所有旧期的奖励。

### 活动：封榜和实际发出是两件事

SSR `entities/top_list/internal/top_list_activity.go` 保存关榜名次/分数，`modules/top_list_player/internal/activity.go` 验开期并找奖档；这是需求证据，不是可靠发奖合同。奖档序号不是奖励物品和数量，改表会改变它的解释。core `modules/activity/with/leaderboard/.../leaderboard.go.tmpl` 已有实例分榜，`with/mailbox/.../mailbox.go.tmpl` 已有稳定邮件键；`modules/activity/files/game/components/activity/resume.go.tmpl` 有提交后重试。缺口是业务在封榜边界冻结可重放的**收件人、稳定奖项标识及奖品内容**，或持久化不可变奖单的引用。

验收：截止前已提交的有效贡献入榜；半数邮件成功后中断、热更奖表并迁属；恢复只读原奖单，成功项不重复，失败项仍可重试。滚动翻页正在变化的榜不是快照；大榜不能强塞进一个实体事务。对“跨期补领”只在玩法显式开启时处理，不能改普通日／周任务原本不补领的规则。不要照搬 SSR `modules/player_activity_reward/internal/node.go` 中先设已领再尝试外部发送的路径。

### 其他组件：通用额度比再建调度器更值得二次核查

SSR `modules/limit/internal/limit.go` 提供查剩余、增减、重置及按时恢复；在联盟标签、联盟战和预设编队中都有实际调用。core 的 `modules/shop` 和 `modules/stamina` 分别已有与交易、体力语义耦合的限额或恢复。暂不建议立即抽公共模块；先找出至少两项能共享**同一周期与恢复、退款和重放**合同的业务。如果抽取，在权威实体的一笔 Nest 事务内检查并消耗，不推广“先 Check，后跨服务 Add”两次调用。SSR 的红点、活动展示和具体奖励值属于产品层，不能仅因存在一个同名组件就挪进 core。
