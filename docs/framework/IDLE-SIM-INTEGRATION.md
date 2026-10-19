# 放置／模拟经营：生产队列、离线时间与经济一致性

本篇用“三工坊＋有限生产队列＋放置收益箱＋订单／每日奖励”学习 core 的正式接入。业务数值是教学假设：三种配方、每工坊四个在途任务／四个待领产物、默认 30 秒离线收益窗口。产品可选择小时级窗口，但须保持本文的时间和幂等契约。先读[入门手册](../GETTING-STARTED.md)，再运行[生成消费者](../../scripts/test-idle-sim-generated.sh)。

## 学习：业务状态与框架对应

同一玩家的三个工坊、原料、仓库、金币、钻石、待领收益和操作回执，都属于静态 sid 的 Player。生产并行表示每个工坊享有自己的时间预算；修改这些状态仍由 Player 的 Nest 锁串行处理。这个纵切不需要跨服实体或分布式交易。

| 场景 | 业务契约 | 框架使用方式 |
| --- | --- | --- |
| 初始化、存档与配置 | 首次初始化捕获 Rules；已有存档不因重试或换表重新定价 | `configdata.Store`／`Snapshot`，生成 Player DAO 的 `State` 与 `WakeTimer` |
| 三工坊排队 | 入队即预扣原料，冻结工时／原料／产出；每队最多四任务 | `Sync_IdleCommand` → Player 锁 → 局部解码 → 校验／修改 → `SetState` → strict 提交 |
| 取消与升级 | 只退尚未开工任务；队列非空拒绝工坊升级，升级只影响以后创建的任务 | 原料、队列和升级费用同事务；不通过内存 goroutine 修改工坊 |
| 时间倍率 | 先结算旧倍率，再购买双倍或升级收入等级；到期前后分段 | 业务整数积分，服务端 BusinessClock；期限与状态落 DAO |
| 离线收益 | 固定最后有效新操作之后的 Horizon；读和后台 tick 不延长窗口 | `entitytimer` 单次有界补算，截止后不再挂 timer；不是逐秒重放 |
| 收益箱、产出缓冲与仓库 | 收益箱封顶丢弃溢出；产出缓冲满则暂停后续工时；仓库不够则整次领取拒绝 | 同一事务原子修改库存／产出／回执；失败不吞任务或收益 |
| 出售、订单与每日奖励 | 精确消耗库存，服务器决定售价与配方；UTC 日只领一次，不补发错过天数 | 永久命令 Seq 回执＋DailyDay，防重与资产一并持久化 |
| 崩溃与结果未知 | 回到原 WAL 恢复；按原 Seq 查回执／重试 | DataEngine Runtime、Projector、ManagerAccess、生成 Sender；strict 成功是 WAL durable，不等于 Mongo 已投影 |

正式生成命令为 `project new -template game`、`add dao Player --entity Player`、`add timer --entity Player`。只修改业务 DAO 定义和 handlers，Sender、DAO 访问器及 timer 接线由生成器生成。`State` 只标 `dao:"persist"`，公开数据从锁内的 `View` 取得，不直接同步整个原始存档。

### 推荐源码阅读顺序

1. [生成脚本](../../scripts/test-idle-sim-generated.sh)：独立模块、当前 core replace、正式生成与检查入口。
2. [配置定义](../../scripts/testdata/idle-sim/rules.go.txt)、[State／Job／Receipt／View](../../scripts/testdata/idle-sim/state.go.txt)：经济事实归属、队列深拷贝、固定配置版本。
3. [生产与时间积分](../../scripts/testdata/idle-sim/production.go.txt)：`Advance`、`Apply`、`NextDeadline`，先看收益截止和倍率，再看业务操作。
4. [Nest handlers](../../scripts/testdata/idle-sim/handlers.go.txt)、[timer 回调](../../scripts/testdata/idle-sim/timer_handlers.go.txt)：事务局部状态、生成 timer 的添加／删除／回滚。
5. [玩法回归](../../scripts/testdata/idle-sim/gameplay_test.go.txt)、[积分与配置回归](../../scripts/testdata/idle-sim/rules_test.go.txt)、[真实恢复](../../scripts/testdata/idle-sim/persistence/recovery_test.go)：用失败边界理解调用方式。

框架入口：[Nest 与 Entity](guide/02-nest-entity.md)、[DataEngine](guide/03-dataengine.md)、[时间使用](guide/10-time.md)、[timer Host](../../framework/nest/entitytimer/host.go)、[timer Driver](../../framework/nest/entitytimer/driver.go)。`.go.txt` 是会被复制、编译和运行的业务源码，不是伪代码。

### 如何调用

生成工程的包路径是 `example.com/idle-sim-practice/game/handler/syncsender` 和 `example.com/idle-sim-practice/game/idlesim`。服务启动注册实体与 handlers，将真实 Nest client 绑定到 timer 驱动。示例以显式调用 `TimerDriver.DeliverDue` 控制测试顺序；真实服务由 `wiring/entitytimer` 装配生命周期，手动装配则需在实体加载前 `SetClock`、Nest 可用后 `Bind`／`Start`，停机调用 `Stop(ctx)` 并排空 Nest／Runtime。

下面片段用于已鉴权端点内，`playerID` 来自会话、`businessNow` 来自服务端时钟：

```go
s := syncsender.NewIdleSimSender(scheduler)
view, err := s.Sync_IdleView(ctx, playerID)
if err != nil { return err }
request := idlesim.Command{
    Seq: view.NextSeq, Kind: "queue", Station: 0, Recipe: 1,
}
receipt, err := s.Sync_IdleCommand(ctx, playerID, request, businessNow.UnixMilli())
```

`Station`／`Recipe` 从 0 开始。`Kind` 支持 `queue`、`cancel`（JobID 是原入队 Seq）、`collect`、`sell`（Recipe／Count）、`buy`（Count）、`upgrade`、`income_upgrade`、`boost`、`claim`、`daily`、`order` 和 `settle`。取消不会退已经开工或已完成的任务；产物先进入工坊 Ready，再通过 collect 入仓。卖货或订单消费的都是仓库实际物品。

内部门参数不是客户端协议：客户端不能指定 PlayerID、Rules、服务端时间、离线时长、工时、收益或余额。首次 `Sync_IdleInit` 的 Rules 从配置 Snapshot 捕获。接公网时将 `ErrRule` 映射为业务错误码，内部故障经框架错误映射处理；本消费者尚未实现网络端点、账号登录或客户端 UI。

每个玩家有一个有序命令流：新 Seq 必须是上次＋1；同 Seq、同完整命令重试返回原回执，同 Seq 改内容或跳号拒绝。回执中的余额是该操作当时的值，当前余额需读 View。结果未知用 `Sync_IdleReceipt(ctx, playerID, originalSeq)` 查询，不换 Seq 再扣费；框架 fence／提交未知按[回复错误判别](guide/02-nest-entity.md#6-回复错误判别是否可能已提交能否重试)恢复。样例永久账本最多 128 条，满后拒新写、保留原回执；不把有限 TTL 消息去重当作永久资产幂等。

### 时间为何这样结算

`LastAt` 是已经处理到的服务端时刻；`Horizon` 是最后一个有效新玩家操作之后允许累计收益的截止时刻。后台 timer 只计算，不续 Horizon；同 Seq 重试也不续。玩家重新操作时，先按旧窗口结算，再为未来建立新窗口。`LastAt` 即使遇到上限也推进到本次 now，因此反复 settle 无法补领被丢弃的时间。

令 `a=LastAt`，`b=min(now,Horizon)`。`b>a` 时，本段有效工作量是普通时长 `(b-a)` 加双倍覆盖区间 `max(0,min(b,BoostUntil)-a)`。每个工坊独立使用这份预算，按队首顺序消耗，至多遍历四个任务。不会因离线一年而执行一年的逐秒循环。

收入用 `旧余数＋工作量×IncomeLevel` 除以 `IncomePeriod`，商进收益箱、余数留给下一段。升级收入等级前先结算旧等级，因此不存在“升级后把整个离线期都按新倍率计算”。收益箱满时丢弃溢出与余数；领取后从下一段真实时间继续累计。金币溢出则拒绝领取并保留收益箱，玩家可先消费金币。

工坊升级把未来任务的 Required 工时按等级缩短，已有任务保存自己的 Required。双倍只改变时间预算，不修改任务投入或产出。四格 Ready 满时，后续任务暂停且丢弃满缓冲期间的预算；领取后不会突然补出那段时间的产品。收货仓库不足则整次 collect 失败，Ready 保留。

定时器一次到期调用 `Advance`，然后只挂一个下一期限；离线很久不补投历史 tick。旧 timer ID 不处理新状态，timer ID、状态和下一期限一起 undo／strict 提交。Driver 索引在 AfterCommit 更新，测试在读到状态后仍有界等待索引更新，不能把二者当成同步返回的一件事。

每日奖励采用演示用 UTC 日界：`day = serverMillis / 86400000`。午夜前后是两天，长期缺席只领当前一天，原 Seq 跨日重试仍返回原回执。其他时区或活动重置规则由业务定义，不能使用客户端日期或机器本地默认时区。

## 验收：运行入口与实际边界

```bash
# 使用仓库要求的 Go；云环境可先 source /workspace/.cloud-setup/activate.sh
bash scripts/test-idle-sim-generated.sh

# 隔离 Mongo 副本集，只使用脚本生成的唯一数据库
ROOST_IDLE_SIM_REAL=1 \
ROOST_DATAENGINE_IT_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0' \
bash scripts/test-idle-sim-generated.sh
```

`ROOST_IDLE_SIM_KEEP=1` 保留成功工程；失败始终保留工程、日志、WAL 和唯一数据库名。成功只清理本次数据库，保留 WAL 与否由临时工程保留选项决定，不改共享服务或正式玩家数据。真实入口登记在根包覆盖表，需要显式运行，不把普通全仓中的 skip 算验收。

| 实际运行 | 证据 | 不包含 |
| --- | --- | --- |
| 12 条基础顶层 race 场景，无 skip | 配置拒绝非法热更／冻结；整段和分段积分相同；非法状态／身份／溢出；三工坊并行；取消退款；仓库／订单／出售；升级与双倍；八路并发排队／收益领取／每日奖励；一年离线由驱动补算一次且不延 Horizon；日界／缺席；undo；满 Ready 暂停与满账本保留身份 | 基础提交器为替身，不证明磁盘或 Mongo |
| 消费者 build／vet、ID 与生成状态检查 | 当前 CLI、DAO、Sender、timer 接线可生成、编译和执行 | 不等于公网登录或正式客户端 |
| `TestIdleSimRealCrashRecovery`，race、无 skip | 子进程首登、两任务、双倍和半程结算留下五条未投影 strict WAL 后被父进程 SIGKILL；父进程用原 WAL／真实 Mongo 冷加载，先比完整存档摘要／View／timer，再由驱动补算一次；收货、离线收益、每日奖励、订单成功后第三进程冷加载并重放全部命令，事实不变 | 已确认 strict 提交、同机进程故障；不是未知回复故障注入、磁盘断电或自动 HA |

真实故障边界保留了一份已完成产物和一份半成品。恢复时额外离线一小时仍只结算到原 Horizon；最终金币 129、原料 45、钻石 8、订单一次、仓库清空。它验证的是投入／工作量／倍率／产出／收益事实共同恢复，不仅是进程能重新启动。

## 业务、框架问题与后续项目

当前关键生产与经济闭环使用现有公开能力完成，未据此新增公共经济规则或确认框架运行时缺陷。离线补多少、何时退款、仓库和任务上限、日界、活动数值都属于业务决策。

本例使用三种配方、固定配置、整体 JSON 存档与有界永久账本。正式内容、多工序依赖／运输／人口／雇员、自动循环生产、经营活动日历、配置迁移、长期账本归档、真实客户端／鉴权／支付／广告、跨服市场和容量／跨机长稳尚未实施。默认 30 秒窗口与一秒 tick 只为教学；不代表产品应采用的周期或生产调度预算。收益窗口设计已覆盖一年时间跳跃，不能从功能测试推算在线容量。
