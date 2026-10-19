# 合成／超休闲：从棋盘操作到持久存档

面向准备用 core 写合成游戏的开发者。先读[入门手册](../GETTING-STARTED.md)，再按本文运行一个完整纵切。教学假设是 4×4 合成棋盘、两条物品链、生产器、订单，以及限时合出四级物品的挑战；这些是消费者业务规则，不是 core 默认玩法。跑酷、物理闯关或客户端战斗不因这份例子自动获得服务器判分能力。

## 学习：状态归谁，框架负责什么

所有持久玩法事实由静态 sid 归属的 Player 拥有。同一个 Player 锁下，棋盘、仓库、金币、体力、生产器次数、操作回执、广告身份和挑战状态一起变化。单人玩法不需要额外引入 Remote、Saga 或 Lockstep；跨服活动和多人竞争另行设计状态拥有者。

| 业务场景 | 状态与业务规则 | 正式框架接法 |
| --- | --- | --- |
| 工程、实体与存档 | Player 的 `State`、`WakeTimer` 和生成的 timer 字段 | `project new -template game`、`add dao Player --entity Player`、`add timer --entity Player`；只改 DAO 定义，不手改生成产物 |
| 合成／移动／交换 | 16 格棋盘，前 12 格开放；同链同级两物合成一级，最高四级 | 生成 `Sync_MergeBatch` 取得 Player 锁，局部解码／校验，成功后 `Dao.SetState` |
| 解锁／仓库／出售撤销 | 四格仓库、解锁扣金币；出售只能在下一条新命令且十秒内撤销，其他到账／接管也会取消撤销 | 与资产同一 DAO 事务；不用客户端存档覆盖服务器 |
| 生产与随机性 | 一次扣一体力／一次充能，私有 HMAC 密钥＋操作序号决定物品 | 同一永久序号回执内存储结果，undo 后原序号产生同一结果 |
| 离线恢复 | 体力、生产器次数按服务端累计间隔恢复，保留余数，满后丢弃溢出时间 | `recover`／新操作时惰性恢复；不为每一点体力排定时器，不自动产出离线物品 |
| 订单与挑战 | 订单精确消耗一级链的二级、三级物品；挑战须服务器实际观察到合出四级 | 消耗、服务器终态、奖励事实在同一 Player 事务；不信任客户端成功／分数／奖励额度 |
| 跨设备存档 | Epoch（设备代际）、Seq（永久操作序号）、Base（期望 Revision） | 登录接管也是 Nest strict 事务；旧设备拒写，新设备查询旧回执 |
| 广告奖励 | 签名、玩家绑定、过期、同身份重试、同 UTC 日三次上限 | `Sync_MergeAd` 内部入口；业务验签和奖励／回执同事务，不依赖有限 TTL 的消息去重 |
| 限时挑战 | 挑战身份、期限、终态与定时器 ID | 生成 `TimerComponent.NewTimerAt`、`Sync_TickPlayerTimer`；旧 ID 无效，终态删除 timer |
| 配置与恢复 | 玩家创建时冻结 Rules 与 hash；旧玩家不因登录换表 | `configdata.Store`／`Snapshot`；strict WAL、Projector、ManagerAccess 冷加载 |

核心源码：[Nest／Entity 使用](guide/02-nest-entity.md)、[DataEngine](guide/03-dataengine.md)、[实体 timer Host](../../framework/nest/entitytimer/host.go)、[timer Driver](../../framework/nest/entitytimer/driver.go)、[配置表](guide/07-config.md)。`strict` 成功表示 WAL durable；不保证 Mongo 投影已经完成。

### 推荐阅读顺序

1. [生成脚本](../../scripts/test-merge-casual-generated.sh)：模板、DAO 定义、正式生成以及运行命令。
2. [Rules 与配置](../../scripts/testdata/merge-casual/rules.go.txt)、[State 与公开 View](../../scripts/testdata/merge-casual/state.go.txt)：数据归属、配置冻结、私有密钥和余数时钟。
3. [操作状态机](../../scripts/testdata/merge-casual/operations.go.txt)、[Nest handlers](../../scripts/testdata/merge-casual/handlers.go.txt)：校验旧回执 → 校验代际／序号／版本 → 恢复服务端时间 → 执行业务 → 留回执 → SetState。
4. [广告签名与账本](../../scripts/testdata/merge-casual/reward.go.txt)、[持久 timer 回调](../../scripts/testdata/merge-casual/timer_handlers.go.txt)：外部证明、服务器期限和失效事件。
5. [玩法回归](../../scripts/testdata/merge-casual/gameplay_test.go.txt)、[配置与边界回归](../../scripts/testdata/merge-casual/rules_test.go.txt)、[真实恢复](../../scripts/testdata/merge-casual/persistence/recovery_test.go)：沿实际断言学习失败结果。

`.go.txt` 是复制进独立生成工程的业务源码，不是伪代码。运行后可在保留的工程里调试，生成器产出 Sender、DAO 和 timer 接线。

### 如何接入一个合成请求

启动先注册实体、handlers，给 `TimerDriver.SetClock` 注入服务时钟，再 Bind 生成驱动。测试为了明确触发次序手动调用生成 tick Sender；真实服务在 Bind 后调用 `TimerDriver.Start()`，排空时调用 `Stop(ctx)` 并遵循 Nest／Runtime 的停机顺序；不能只 Bind 后就假定后台轮询已启动。

已鉴权端点从会话取得 PlayerID，用服务端时间调用生成 Sender。下面代码在生成工程内使用；`scheduler` 已完成 Nest 启动，`ctx` 是请求上下文：

```go
s := syncsender.NewMergeCasualSender(scheduler)
view, err := s.Sync_MergeView(ctx, authenticatedPlayerID)
if err != nil { return err }
command := mergecasual.Command{
    Epoch: view.Epoch, Seq: view.NextSeq, Base: view.Revision,
    Kind: "merge", A: 0, B: 1,
}
receipts, err := s.Sync_MergeBatch(ctx, authenticatedPlayerID,
    []mergecasual.Command{command}, businessNow.UnixMilli())
```

`game/handler/syncsender` 和 `game/mergecasual` 的完整导入前缀是 `example.com/merge-casual-practice/`。首次 `Sync_MergeLogin` 的 key 来自服务端 `crypto/rand`，Rules 从 `CaptureRules(store.Current())` 捕获；后续登录不重新生成 key。示例测试固定密钥便于复现，不能搬入线上。

这里的内部 handler 参数不等于公网协议。客户端仅提交目标格、操作类型及 Epoch／Seq／Base；端点不得让客户端选择 PlayerID、服务端时钟、Rules、私有 key、广告验签 key、胜者或奖励值。公开响应仅使用 `View`／`Receipt`；DAO `State` 没有 sync 标记，不把私有 RNG 密钥和广告账本广播出去。读取同样经 Sender 取得锁。 接网络协议时还须将 `ErrRule` 映射为业务客户端错误码；存储／内部故障走服务端错误映射，不能直接把内部错误文本返回客户端。

### 离线批次与冲突如何处理

每批最多 16 条命令，在同一个 Player 事务中执行。客户端离线预测下一条 `Seq` 和 `Base`，依次上传；任一步非法，整个批次不落 DAO。批次中的客户端时间不作为收益依据，所有命令使用本次受信任的服务端时间，因此不能通过伪造离线时长连刷生产器。

- 原序号、完整原命令重发：返回原回执，产物、金币、体力和 timer 都不再变化。回执中的余额是原操作完成时的余额，当前状态另读 View。
- 同序号改目标、跳序号、过期 Base：拒绝；先取服务器 View，按业务冲突策略重新预测，不能把整张客户端棋盘上传覆盖。
- 新设备接管：已鉴权入口持久分配下一个登录票序号，以 `Sync_MergeLogin` 提高 Epoch，同时提高 Revision。相同当前票重试不再推进；旧票及跳票拒绝。这个票的分配／认证在业务接入层，不是由客户端传一个数字就算登录。
- 结果未知：保持原命令身份查询 `Sync_MergeReceipt(ctx, playerID, currentEpoch, originalSeq)`。新设备可查询旧设备已提交的回执；不要改 Epoch／Seq 重发扣款。没有回执且运行实例被框架 fence 时，先完成框架恢复再决定操作。
- timer、广告到账或接管会改变 Revision，离线预测需要重取状态。旧批次不静默覆盖这些已提交事实。

账本最多 128 条命令，满后新命令拒绝，旧回执仍可读／重放；广告账本同样有界且不静默淘汰。样例不适合长期直接上线，正式账本归档必须同时解决永久身份、查回执与幂等，不能简单删历史或只保留一个最近序号。

### 时间、挑战与奖励

启动挑战扣两体力，冻结到期时间，必须在期限前通过合法合成新建四级物品才获胜；已有四级物品、客户端上报赢或延迟 tick 都不能判胜。到期命令会先按服务器时间判负，即使 timer 尚未调度。胜负与取消 timer 同事务；重试启动不延长原期限，落后很久只处理当前挑战，不补开局数。

订单、广告和挑战采用不同身份：订单依靠操作 Seq；广告依靠平台证明 ID＋内容摘要；挑战依靠开局 Seq＋Claimed。不能混用一个短暂的传输请求 ID 代替这些永久业务事实。新 Seq 重试已领奖的当前挑战不再付奖，原 Seq 重放仍返回原回执。

广告样例使用 HMAC-SHA256 的模拟平台回调，签名绑定证明 ID、PlayerID、过期时间和奖励类别。通过签名校验后先查原回执，再判断新证明是否过期：同一已到账证明过期后仍可重试，不重新发放；相同 ID 改内容拒绝。体力放不下时拒绝而不吞证明，新证明同 UTC 日最多三次。真实平台的签名协议、订单查询、撤销与补偿、激励广告 SDK 尚未接入。

## 验收：如何运行，实际证明什么

```bash
# 已配置仓库要求的 Go 工具链；云环境可先 source /workspace/.cloud-setup/activate.sh
bash scripts/test-merge-casual-generated.sh

# 独立 Mongo 副本集；不要使用正式玩家库
ROOST_MERGE_CASUAL_REAL=1 \
ROOST_DATAENGINE_IT_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0' \
bash scripts/test-merge-casual-generated.sh
```

`ROOST_MERGE_CASUAL_KEEP=1` 保留成功工程。失败始终保留临时工程、日志、WAL 和本次唯一数据库名。真实入口登记在根包 `integration_coverage_test.go`，需要显式手动运行，不在普通 `go test ./...` 或 `dataengine-env.sh test` 中自动执行。

| 实际入口 | 验证内容 | 边界 |
| --- | --- | --- |
| 基础 15 条顶层测试，全部 `-race`、无 skip | 配置非法热更不发布、冻结规则；存档拒绝非法结构、公开视图排除密钥；账本满后保留证据；棋盘／仓库／解锁；八路并发生产重放；出售撤销；订单；离线批次原子性／版本冲突；跨设备接管与旧回执；服务器目标与八路并发领奖；期限／离线过期；undo；体力余数；广告签名／八路并发回调／每日上限 | 基础提交器是替身，不证明 WAL 或外部平台 |
| 生成工程 build／vet、`id check`、`generate --check` | 当前 CLI 正式生成的 DAO、Sender、timer 可编译、运行，无生成漂移 | 不等于正式客户端／公网接入 |
| `TestMergeCasualRealCrashRecovery`，`-race`、无 skip | 子进程登录、两次生产、广告到账、挑战开局留下五条未投影 strict WAL；父进程 SIGKILL 后用同一 WAL／真实 Mongo 恢复，先比较完整存档摘要／公开视图／timer ID，再重放生产／广告、处理一次过期，重新挑战并完成服务器目标／领奖；第三进程冷加载先验事实再重试，不重复生产或发奖 | 单机自有子进程故障；只证明已确认 strict 提交，不模拟回复未知、磁盘断电、跨机 HA |

本消费者不需要新增长期依赖。真实测试使用本次唯一库，成功后仅删除该库，不删除 WAL 或共享服务数据。

## 已完成与后续工作如何区分

这个纵切已覆盖单人合成后端的关键接入，规则都留在消费者中；当前 main 契约无需新增公共框架能力，本轮没有确认框架运行时缺陷。回归证明的是示例规则与已列故障边界。

正式物品链／多生产器／多订单／任务活动、数值经济与运营工具、持久登录票分配、长期账本／存档规则迁移、客户端预测与冲突 UI、真正广告／支付回调、公网鉴权与限流、云存档网络协议、作弊识别、跑酷／物理关卡判分、跨机恢复和容量长稳尚未在本消费者实施。整个 State 用 JSON 读写是教学选择，生产拆分应基于实际大小和延迟测量；不能从 15 条功能回归推算容量。
