# 重构方案：Interest 单写者——全部视野状态变更进同一条有序日志（2026-10-21）

维护者 2026-10-21 批准重构（“有个问题出现过好几次了，是不是可以重构”；同日补充：参考 ssr 的同步方式，方案定稿后直接实施，产品选择先按推荐，Remote 保持现状，压测之后统一安排）。基线 `494134ae`（origin/main）。范围：`framework/sync/entitysync/policy` 的 `Interest`（`interest.go`、`interest_queue.go`、`view.go`、`spatial_component.go`），以及调用方 `gameplay/movement`、demo 场景桥、测试夹具与文档。`AOI` / `AOICluster` / `Group` / `Direct` / `SelfView` 与 Manager 的契约不变。

## 1. 为什么重构（方向判断）

### 1.1 缺陷与修复链

| 时间 | 提交 | 问题 | 修法加了什么 |
| --- | --- | --- | --- |
| 10-07 | `04378f7c`（RR-20261006-48） | Leave / Hide 之后仍就绪的排队移动在 `MoveSubject` 处失败，Flush 每次都失败、整个 Manager 停发 | Leave / Hide 改写队列里的事实（`supersedeQueuedMovesLocked`） |
| 10-12 | `b1ec31a3` | 关系变更要与提交一致 | 关系事实也排队（D19 又删掉，改为直接调用） |
| 10-20 | `3794f74e` / `812cca9c` | D14：观察者改玩家；已 Enter 的玩家改 Watch 会看见自己 | 入口加状态检查 |
| 10-20 | `fe811f13` | D14：客户端视野请求 | handler 里直接 `SetObserverView` + `Apply`（快池上拿 `Interest.mu`） |
| 10-20 | `d2c2cdce` | D19：视野登记表 + 跟随 | 改写规则改为“目标那一半作废、跟随者那一半保留”（`subjectGone`） |
| 10-20 夜 | — | 16 个额外视野时 Nest 模式失守 | `6304ffa0` 只评估移动过的目标；`01e4f736` 应用线程 + “已应用未结算”状态；`b6ef5573` 两种应用模式 |
| 10-21 | 学习分支审查第 1 条 | Leave→Enter / Hide→Show 窗口里旧排队移动把新视野拖回旧位置（D19 回归） | — |
| 10-21 | 第 5 条 | ApplySync 每个 Flush 只应用一批，`BatchSize / 周期` 成了吞吐上限 | — |
| 10-21 | 第 6 条 | 直接 API 与 AOI 批次争 `Interest.mu`，业务 worker 上阻塞 15～46ms | — |

KNOWN-LIMITS 里还有两条同根的窗口：“进出场景在提交后、实体锁外调用 `Space`”（提交后回调晚于同实体下一条消息时 `QueueMove` 返回 `ErrInterestUnknown`）与“`Join` 之前的移动 AOI 不跟”。

### 1.2 判断：是前提问题，不是细节问题

现在 Interest 的状态有**三个写入口，彼此之间没有全序**：

1. **直接调用**（`Enter` / `Watch` / `Move` / `MoveView` / `SetObserverView` / `Leave` / `Unwatch` / `Show` / `Hide` / `Resubscribe`）：取 `Interest.mu`，当场改 AOI 与 `held`。
2. **视野登记表**（`Define*` / `Set*` / `Attach` / `Detach` / `Remove`）：取 `viewMu`，记一条 `viewOp`，由下一个持 `mu` 的入口按序应用（这本身已经是“有序日志 + 单应用方”）。
3. **排队事实**（`QueueMove` / `QueueSpatial` / `QueueObservedBlocks`）：取 `queueMu` 入队，过提交门，在政策阶段（ApplySync）或应用线程（ApplyAsync）分批应用。

2 与 3 之间靠“每个持 `mu` 的入口先 `syncViewsLocked`”对齐；1 与 3 之间没有对齐——调用顺序上早于直接调用的已提交事实，在直接调用之后才应用。`04378f7c` 与 `d2c2cdce` 都是给其中一种交错（Leave / Hide 在事实之后）打补丁，Enter / Show / 直接放置这几种交错没有补，所以修一次冒一次。红测试 `TestPlacementAfterAQueuedMoveIsNotDraggedBack` 的 `enter_again` 子用例（已登记的玩家再次 Enter，不经 Leave）就是没有被任何补丁覆盖的那一种；按学习分支建议的点修（`subjectGone` 时不移跟随者）临时试过，`leave_then_enter` / `hide_then_show` 变绿、`enter_again` 仍红（见 §7）。

第 6 条也来自同一前提：直接调用与 AOI 批次共用 `mu`，业务线程只要调直接 API 就可能等一个批次；ApplyAsync 把 AOI 搬出 Flush，但直接调用仍在 `mu` 上。第 5 条是 `b6ef5573` 引入 ApplySync 时沿用了应用线程“一步一批”的节奏，而政策阶段一个周期只来一次。

结论：把写入口收成一个——全部变更都是有序日志里的一条，只有一个应用方按序执行；业务线程只登记、不碰应用侧状态。这正是视野登记表已有的模式，推广到全部写入口。

## 2. ssr 的做法与取舍

读了 `ssr/kingdom/entities/scene/internal/aoi`（`aoi_se_move.go`、`aoi_se_sight.go`、`aoi_slg_*`）、`ssr/kingdom/entities/scene/internal/wrpc/updateAOI.go`、`ssr/kingdom/modules/map/block`、`ssr/kingdom/modules/map/observer/observer.go`，以及它们依赖的 `ssr/watcher/wdsync/aoi/entityTheater.go`、`ssr/watcher/wdsync/easyncable.entity.go`（只读）。

| 问题 | ssr 的做法 | roost 取舍 |
| --- | --- | --- |
| 视野 / AOI 在哪个线程、谁驱动 | **没有 AOI 线程**。移动（`AoiSe.Move` / `AoiSlg.Move`）在实体自己的业务线程、持实体锁同步执行：算新旧块，`Block.TransferEntity` 把实体从旧块“剧场”（`EntityTheater`）搬到新块；客户端视野请求 `UpdateAOIHandler` 在持场景玩家锁的 handler 里同步增删订阅的块。 | **不照搬**。roost 的移动是事务内的 DAO 写，回滚的移动不能被看见（提交门），所以 AOI 不能在事务里同步做；Nest 快池不能阻塞，而 roost 的 AOI 是“按距离逐对评估 + 滞回 + 分档 + MaxVisible”，一次移动要读整个邻域、选最近 N 个，不能像 ssr 那样只锁两个块。保留“由一个所有者按序改状态”的思路，所有者是 Interest 的应用方而不是实体线程。 |
| 移动与进出怎么排队、去重、按什么顺序 | 不排队：调用即生效，顺序就是持锁顺序（同一实体的移动都在它自己的线程上）。订阅是计数的：目标实体的 `Easyncable` 按 (连接, 视图) 计数，`newlyAddedRemovedEasyncObservers` 记本周期新增 / 移除，同步循环按周期下发——一个周期里加了又减的自然抵消。 | **照搬计数与按周期合并**：roost 的 `held`（按视野记各自的贡献，最后一个释放才退订）与 `touched`（结算前合并）就是同一件事，保留。**排队必须保留**（提交门），所以顺序由“同一条日志 + 唯一应用方”给出，而不是由持锁顺序给出。 |
| 业务线程与 AOI 怎么交互 | 细粒度锁：每个块一把 `EntityTheater.mu`，搬家时按 `uniqueNum` 顺序锁两个块；目标实体的订阅计数另有一把可重入锁（与实体锁分开）。没有全局锁，也没有消息。 | **只照搬“短锁、锁序固定”**：业务线程只取登记侧一把短锁（O(1)，不含空间计算）。块锁那一层 roost 已有（`aoi_block.go` 的块锁供并行重评），不让业务线程直接碰。 |
| 怎样避免旧事件覆盖新状态 | 没有队列就没有旧事件：状态原地改。离线 / 断线走 `CleanPlayerObservers` 一次性把连接从全部块拔掉（`UprootObserver`），之后的进入从空状态开始。 | **借鉴“离开是一次性的边界”**：Leave / Hide / 再次放置给目标换一个代际号，旧代际、还没确认的排队事实应用时丢弃；已确认的事实在日志里排在 Leave 之前，先应用。这就是“先应用、再 Leave”的语义，不需要改写队列。 |
| 吞吐与排空 | 没有积压：成本在移动者自己的线程里付掉，块锁让不同区域并行。 | **不照搬**（同第一行）。roost 的积压在日志里：ApplySync 一次政策阶段按耗时预算排空、ApplyAsync 的应用线程连续排空，`MaxQueuedFacts` 仍限制排队事实。 |

其余差异：ssr 的视野粒度是块（客户端报一个矩形 + LOD），没有逐对距离、滞回和容量；roost 的 D14 / D19 语义（半径、分档、MaxVisible、多视野合并、同步层级）都比它细，这些全部保留在应用侧，不受本重构影响。

## 3. 目标结构

### 3.1 两侧

| | 登记侧（intake，原 `viewRegistry` 扩展而来） | 应用侧（apply，原 `Interest.mu` 保护的部分） |
| --- | --- | --- |
| 锁 | 一把短锁（原 `viewMu` 与 `queueMu` 合并） | `Interest.mu`（不变） |
| 谁持有 | 业务线程（任何 Nest worker、提交后回调、接入层）、应用方取日志时 | 应用方：ApplySync 的政策阶段 / ApplyAsync 的应用线程 / 显式 `Apply()`；Manager 的 `resubmit` 钩子；诊断读 |
| 状态 | 视野登记表（定义、挂接、成员、主视野的视点 / 视野 / 跟随）、已登记目标与代际号、主视野限速、**有序日志**、排队事实的预算 | AOI、`held` / `touched` / `retry` / `refused`、`followers` / `spatialPlayers` / `selfOf`、已应用目标的代际、已应用未结算的事实 |
| 做什么 | 校验参数（边界、视野归一、上限、目标是否登记、限速），改登记事实，追加一条命令；错误当场返回 | 按日志顺序执行命令；把 AOI 事件与关系变更归并进 `held`；结算（说给 Manager），结算后释放事实的发布屏障 |

锁序：`mu → intake.mu`；业务线程只取 `intake.mu`，从不取 `mu`，所以永远不等 AOI 批次。

### 3.2 日志与命令

日志里每一条是一个命令，带种类与参数：

- **门控事实**（`QueueMove` / `QueueSpatial` / `QueueObservedBlocks`）：同现在，带提交条件、所属提交、发布屏障，另带登记时目标的代际号。
- **放置**：`Enter`、`Watch`、`Show`、直接 `Move`；**撤除**：`Leave`、`Unwatch`、`Hide`。登记侧给目标分配新代际号（全 Interest 一个递增计数，撤除时删除，不随 ID 增长）。
- **视点 / 视野**：`MoveView`、`SetObserverView`（主视野），以及原有的 `viewOp`（空间视野放置、挂接、解除、移除、关系贡献）。
- **重说**：`Resubscribe`。

### 3.3 执行规则（应用方，一步一批）

1. 取日志时在登记侧锁内一次性读齐每条门控事实的提交条件（同现在：保证同一提交的事实全部在队列里）。
2. 按日志顺序执行：
   - 门控事实：被丢弃（回滚 / 拒绝）的完成并释放屏障；**代际号不等于目标当前的代际**（期间被 Leave / Hide / 再次放置）的同样丢弃；未确认的留在原位，同一 (目标, 代际) 后面的事实不能越过它，其他目标可以越过（同现在）；就绪的进入 AOI 批次。
   - 其他命令不等未确认的事实：它们不依赖提交，今天的直接调用也是在未确认事实之前就生效，这一点不变。与该事实同一目标的撤除 / 再次放置会换代际号，被越过的旧事实确认后作废，不会再把目标或跟随视野拖回去；与“先应用它、再执行命令”相比只差 Hide 之后跟随视野停在哪里（见 §4 行为变化 3）。
   - 命令在 AOI 批次之外串行执行（与今天直接调用相同的计算），连续的门控事实组成一个批次（同今天的写阶段 / 并行重评）。
   - 一步取到 `BatchSize` 个就绪事实即在这里切开：之后的命令与事实留到下一步（同一提交不切开，同现在），保证已就绪的事实不会被它后面的命令越过。
3. 撤除命令（Leave / Unwatch / Hide）执行时，把因此释放到零的 pair 标记“换代”：同一次结算里它又被持有时，先退订再订阅，让 Manager 看到一次离开和一次进入（业务可能在两次调用之间注销 / 重新登记了目标或关闭 / 重开了会话）。

### 3.4 谁来执行（两种应用模式只差这一点）

| | ApplySync（缺省） | ApplyAsync |
| --- | --- | --- |
| 执行 | Flush 的政策阶段：一步接一步，直到没有可应用的条目或耗时预算用完（预算 = 同步周期的一半），然后结算 | 应用线程连续排空（同现在）；政策阶段只结算 |
| 结果何时可见 | 提交 / 调用之后的下一次 Flush（积压超过预算时顺延，on_change 下立即再来一轮） | 应用线程走到之后的那次 Flush |
| 显式 `Apply()` | 两种模式相同：在调用方线程排空并结算，返回本次的拒绝；会等正在进行的一步。**不能在 Nest 快池上调用**（测试、工具、离线驱动用） |

## 4. 与现有契约的关系

- **Sync 冻结 / 交付**：门控事实的发布屏障仍在结算之后才释放，新位置与它带来的进出视野同一帧（不变）；政策阶段仍在锁外、捕获前执行，不读未冻结的业务字段——命令带的是调用时给的值。
- **Nest 快池不阻塞**：业务线程上的全部 Interest 写入口只取登记侧短锁；红测试 `TestDirectCallsDoNotWaitForTheAOIBatch` 用 `BandLayer` 钉住政策阶段，验证每个入口都立即返回。`Apply()`、诊断读（`Visible` / `Seen` / `ViewVisible` / `ObservedBlocks`）仍取应用侧锁，文档写明不在快池上调用。
- **提交门**：`QueueMove` / `QueueSpatial` / `QueueObservedBlocks` 不变。其余入口不过提交门：`SetObserverView` 不改业务状态（D14 决定 6②）；视野操作由业务在提交后调用（D19 决定⑤，仍不进 Nest、不做排队版——它们本来就是登记侧记一条变更）；`Enter` / `Leave` / `Show` / `Hide` 由业务在提交后调用（`MapBasic` 经 `AfterCommit`）。是否给 `MapBasic` 提供事务内的门控进出场景见 §8 第 6 项。
- **D19 视野语义**：主视野、业务视野、多对多挂接、层级合并、`MainView` 保留、上限检查全部不变；上限检查本来就在登记侧（`viewRegistry`）。主视野与业务视野的内部编号（handle）不复用，所以视野这一侧天然有“代际”，不需要另加。
- **MaxVisible / 块预算**：在应用侧 AOI，不变。
- **两种应用模式**：保留，阈值与选择指引不变；变化只在“直接调用的结果也由所选模式的应用方算”。

### 行为变化（未上线，不做兼容）

1. 直接调用只登记：返回时效果还没进 AOI。结果在下一次结算可见（ApplySync：下一次 Flush；ApplyAsync：应用线程走到之后的 Flush），或调用方自己 `Apply()`。诊断读（`Visible` 等）先应用已登记的条目再读，测试里“调用后立即读”的写法照旧成立。
2. ApplySync 一次 Flush 排空已确认的积压（预算内），`BatchSize` 只是一批的大小。
3. 未确认的事实被同一目标之后的撤除 / 再次放置作废：作废的事实整条丢弃（今天保留“跟随者那一半”）。唯一可见差别：Hide 时它还有一条未确认的移动，跟随它的视野停在 Hide 前已应用的位置，而不是那条移动的目标；随后的 Show 会把视野带到新位置。已确认的事实不受影响（日志里排在前面，先应用）。
4. `Resubscribe` 不再返回重说的 pair 数（要等应用方执行才知道）。
5. 直接调用产生的拒绝今天由 `Apply()` 的返回值报告；政策阶段里结算的拒绝新增 `InterestConfig.OnRefusal` 回调（政策阶段调用，不得阻塞、不得调用 Interest）。
6. 纯直接驱动（只用直接调用、自己调 `Apply()`）也会被政策阶段排空：Flush 可能先于调用方的 `Apply()` 应用这些条目。sync-aoi 的 direct 基线因此不再是“AOI 全部在压测线程”，对照时注明。

## 5. 调用点（全部要改或复核）

| 位置 | 现在 | 改后 |
| --- | --- | --- |
| `demo/internal/service/game/scene.go.tmpl` `Join` | `ObserverView` → `Enter` → `AttachView` → `Resubscribe` → `apply()`，在 `enter_game` handler（快池）上 | 只登记；去掉 `apply()`；拒绝经 `OnRefusal` 记日志 |
| 同 `SetView`（`scene_view` handler） | `SetObserverView` + `apply()` | 只 `SetObserverView` |
| 同 `leaveLocked` / `ShowSubject` / `HideSubject` / `SetTeam` / `openLocked` | 调用后 `apply()` | 去掉 `apply()`；`Leave` 之后立即 `Unregister` / `CloseSession` 由 §3.3 第 3 条保证重进时重新订阅；`openLocked` 不再读 `Resubscribe` 的返回值 |
| `gameplay/movement/map_basic.go` `InterestSpace` | `Enter` / `Show` / `Leave` / `Hide` 后 `Apply()`（提交后回调，在 worker / 完成泵上） | 只登记 |
| `demo/game/actor/actor.go.tmpl`、`scripts/perf/sync-aoi/{nest,heartbeat}.go` | `QueueMove` | 不变 |
| `scripts/perf/sync-aoi/main.go` | 直接 `Enter` / `Show` / `Move` + `Apply`、视野操作、`Seen` | 不变（`Apply` 在压测线程，见行为变化 6） |
| `scripts/testdata/mmorpg/scene_runtime.go.txt`、`scripts/testdata/slg/alliance/runtime_test.go`、`codegen/internal/entity/testdata/{dataengine,nestgame}` | 直接调用 + `Apply` | 复核，签名不变 |
| `framework/sync/entitysync/policy` 包内测试 | 部分用例读 `queueMu` / `facts` / `applierStarted` | 跟随新结构；`TestBatchLimitRetainsLaterEntityPublication` 的同步模式改用耗时预算触发切分；`TestQueuedFactsInvalidatedByLeaveOrHideDoNotWedgeFlush` 的改写语义改为代际 |
| 文档 | guide / impl 04-sync §2E、INTEREST-BLOCK-AOI、KNOWN-LIMITS（AOI 视野、进出场景窗口）、DESIGN-TODO D19、CHANGELOG、OBSERVABILITY、demo WALKTHROUGH | 写新结构、行为变化、升级说明 |

生成器（`codegen`）不生成 Interest 代码；game-demo 是参考工程，改模板后按维护手册重新生成跑 build / vet / test。

## 6. 性能预期与验证（压测之后由维护者统一安排）

- **业务线程**：直接调用从“可能等一个 AOI 批次（实测 15～46ms）+ 自己做 AOI”变成 O(1) 登记（一次短锁、一次 append）。`QueueMove` 不变；登记侧锁现在也被视野操作与直接调用使用，都是短临界区。
- **ApplySync 的 Flush**：直接调用的 AOI 计算（进场景、改视野、放置）从调用方线程移到政策阶段；以移动为主的负载不变。积压超过一批时 Flush 变长但不再丢吞吐，受“周期一半”的预算约束。
- **ApplyAsync**：直接调用的 AOI 计算进应用线程，Flush 不变。
- **对照方法**（同机、改前 `494134ae` 与改后交替多轮，`benchstat`）：
  1. `go test -bench 'Interest|Relation|AOI|Cluster' -count 10 ./framework/sync/entitysync/policy/`（现有基准），另加两条新基准：登记侧并发 `SetObserverView` / `Enter`（政策阶段同时在跑）、ApplySync 积压 4×BatchSize 时一次 Flush 的耗时；
  2. `scripts/perf/sync-aoi` 原场景与小块场景，`-views-per-player` 0 / 4 / 8 / 16，`-apply-mode sync|async`，Nest 与 direct，看 P50 / P99、帧数、Flush 均值与政策阶段 P95（与 DESIGN-TODO D19“两种应用模式”的表同口径）；
  3. demo 登录风暴（enter_game + scene_view），看 handler 服务端 P99。
- 预期：1 中登记侧入口与基线同量级或更快，AOI 基准不变；2 中 Nest 两种模式 P99 不高于改前，ApplySync 16 视野仍超周期（上限不变）；3 中 handler P99 不再有 AOI 批次长度的尾巴。不符合时记录，不改结论。
- **结果（2026-10-22）**：见 [INTEREST-SINGLE-WRITER-20261022](../performance/INTEREST-SINGLE-WRITER-20261022.md)。1 成立（政策阶段在跑时直接调用均值与 p99 降一到两个数量级，AOI 基准 ±2%；视野登记表小操作每次多一条日志、慢几百纳秒）；2 成立（Nest ApplySync 0～8 视野 P50 低 6～11ms、P99 低 4～8ms，ApplyAsync 持平，ApplySync 16 视野仍超周期且政策阶段 P95 比改前高约 8ms）；3 未测。另修了 sync-aoi 的 Nest 模式每 tick 调 `Apply` 的夹具问题（`9ad54c3c`，重构后它让压测线程替政策阶段做 AOI），direct 基线按本节第 6 条注明不可与改前直接比。

## 7. 修前证据（基线 `494134ae`，`GOWORK=off go test -count=1 -run '<三条>' ./framework/sync/entitysync/policy/`）

```
--- FAIL: TestPlacementAfterAQueuedMoveIsNotDraggedBack/sync/leave_then_enter
    sync: rejoined at {30 10} but entity 2 at {30 10}, main view at {900 900}; player 2 sees [], subscribed 2→1 false, 1→2 true
--- FAIL: TestPlacementAfterAQueuedMoveIsNotDraggedBack/sync/hide_then_show
    sync: shown again at {30 10} but entity 3 at {30 10}, its view at {900 900}; the view sees [], subscribed 5→1 false
--- FAIL: TestPlacementAfterAQueuedMoveIsNotDraggedBack/sync/enter_again
    sync: entered again at {30 10} but entity 2 at {900 900}, main view at {900 900}; player 2 sees [], subscribed 2→1 false, 1→2 false
（async 三个子用例文本相同，前缀 async）
--- FAIL: TestSyncApplyOneFlushDrainsTheCommittedBacklog
    ApplySync: one Flush applied 2 of 5 committed moves (BatchSize 2); still queued: [13 14 15]
--- FAIL: TestDirectCallsDoNotWaitForTheAOIBatch/sync
    sync: while the policy phase held the AOI batch, these calls did not return: [Enter Leave Show Hide Move MoveView SetObserverView ObserverView Resubscribe Apply]
--- FAIL: TestDirectCallsDoNotWaitForTheAOIBatch/async
    async: while the policy phase held the AOI batch, these calls did not return: [Enter Leave Show Hide Move MoveView SetObserverView ObserverView Resubscribe Apply]
```

`DefineSpatialView` + `AttachView`（只取 `viewMu`）在同一用例里按时返回，说明用例能区分阻塞与不阻塞。`-race` 下三条同样只在断言上失败，无数据竞争。

点修对照（临时改动、未提交，验证用例能变绿且确认“点修不收敛”）：`settleQueued` 循环排空 + `subjectGone` 时不移跟随者——`TestSyncApplyOneFlushDrainsTheCommittedBacklog`、`leave_then_enter`、`hide_then_show` 变绿，`enter_again` 两种模式仍红（文本同上）。

## 8. 待拍板（均先按推荐实施）

1. **命令能否越过未确认的事实**：A 能越过，靠代际号作废被越过的同目标事实（推荐：断线 / Leave 不等提交，语义只在“Hide + 未确认移动”的跟随视野位置上与今天不同）；B 严格先进先出，Leave 等同目标的提交确认（远端结果未知时可能等很久）。
2. **ApplySync 一次政策阶段的耗时预算**：A 固定为同步周期的一半，不加配置项（推荐，规则只有一条）；B 新增 `InterestConfig.SyncApplyBudget`；C 不设预算、排空为止。
3. **直接调用产生的拒绝怎么报告**：A 新增 `InterestConfig.OnRefusal`（推荐）；B 只有指标与 Drain 诊断。
4. **命令是否计入 `MaxQueuedFacts`**：A 不计，只有门控事实受限（推荐：与今天视野操作的变更记录一致，`Enter` / `Leave` 不会因队列满失败）；B 计入，撤除类命令例外。
5. **`Apply()` 的去留**：A 保留为“在调用方线程排空并结算”，文档写明不在快池上调用（推荐）；B 删除，测试与工具改用 `Manager.Flush` / `Drain`。
6. **`MapBasic` 的进出场景是否改为事务内的门控命令**（`QueueEnter` / `QueueLeave`，消除提交后回调晚于下一条消息的窗口）：A 本轮不做，KNOWN-LIMITS 保留该窗口（推荐：要处理“回滚的进场景”在登记侧的暂定状态，单独一期）；B 本轮一起做。
7. **诊断读**：A 取应用侧锁、先应用已登记的条目（推荐，与今天 `syncViewsLocked` 一致）；B 发布只读快照。

## 9. 分期与工作量（估计）

| 期 | 内容 | 红 → 绿 | 估计 |
| --- | --- | --- | --- |
| 0 | 本方案 + 三条红测试 | 记录修前失败文本 | 已完成 |
| 1 | 统一日志与单写者：登记侧（`viewRegistry` 扩展、`queueMu` 合并）、命令种类、执行器（取代 `syncViewsLocked` + `applyBatchLocked` + Leave / Hide 改写）、代际号、撤除换代；包内测试跟随 | 第 1、6 条 | 1.5～2 天 |
| 2 | ApplySync 排空与预算；`OnRefusal` | 第 5 条 | 0.5 天 |
| 3 | 调用方（demo、movement、夹具）与文档；`glsvet`、`-race ./framework/sync/...`、`scripts/test-sync-modes-generated.sh`、game-demo 生成、根包与全量 | 调用方的回归 | 1 天 |
| — | 压测对照（§6） | 维护者安排 | — |

代码量：预计删除 `supersedeQueuedMovesLocked` / `rewriteFactsLocked` / `subjectGone` / `wakeViews` 的原子标志与两把锁之间的规则，新增命令种类与执行器；净变化实施后如实报告。

## 10. 实施结果（2026-10-21）

维护者同日指示“方案定稿后直接实施，产品选择先按推荐”，§8 七项全部按推荐（1A、2A、3A、4A、5A、6A、7A）实施。

### 10.1 提交

| 提交 | 内容 |
| --- | --- |
| `c78ff0ec` | 本方案 + 三条修前红测试 |
| `eed9c870` | 第 1、2 期合在一次提交：登记侧 + 有序日志 + 单应用方、代际号、撤除换代、ApplySync 预算排空、`OnRefusal`、`Resubscribe` 去返回值；包内测试跟随 |
| `3ffe605c` | 代际号作废的独立用例 |
| `254d61cf` | 第 3 期：调用方（game-demo 场景桥、`gameplay/movement`）与文档 |

### 10.2 强制点（`framework/sync/entitysync/policy`）

- 登记侧：`view.go:102` `viewRegistry`（日志、目标代际、限速、预算），`view.go:137` `record`；写入口 `interest.go:378` `Enter`、`:487` `SetObserverView`、`:545` `leave`、`:599` `Hide` 等，`interest_queue.go:65` `queueFact`——都只取 `registry.mu`。
- 单应用方：`interest_queue.go:229` `runLogLocked`（`:283` 切开处之后的命令与事实留到下一步，`:294` 代际号不符的事实作废），`view.go:636` `runOpLocked`；撤除换代 `interest.go:663` `leaveLocked` / `:678` `removeSubjectLocked` 记 `departed*`，`:873` `settlePair` 先退订再订阅。
- 吞吐：`interest_queue.go:353` `settleQueued`，`:366` 预算循环，`:137` `syncApplyBudgetFor`。
- `Apply`：`interest.go:779`（取 `mu`，给测试 / 工具）。

### 10.3 红 → 绿

修前文本见 §7。修后 `GOWORK=off go test -count=5 ./framework/sync/entitysync/policy/` 与 `-race -count=2` 全绿。变异核对：

| 变异 | 变红的用例与文本 |
| --- | --- |
| 去掉代际比较（只看目标是否在场） | `TestAnUnconfirmedMoveOvertakenByLeaveAndEnterIsDropped`：`sync: entity 2 at {900 900}, main view at {900 900}, player 2 sees [], facts left 0`（async 同） |
| 去掉 `settlePair` 的撤除换代分支 | `TestLeaveAndEnterInOneSettleSubscribeTheReopenedSessionAgain`：`sync: after Leave → reopen → Enter in one settle: 2 sees 1 false, 2 sees itself false, 1 sees 2 true`（async 同） |

撤除换代这一条在修前代码上同样成立（不是本重构引入）：在 `494134ae` 上用探针（Leave → CloseSession → OpenSession → Enter，之后才 `Apply` 一次）得到 `2 sees 1 false, 2 sees itself false, 1 sees 2 true`。之前的调用方（demo）在 Leave 之后立即 `Apply`，所以没有撞到；重构后去掉了这些 `Apply`，必须由 Interest 自己保证。

### 10.4 与方案的偏差

1. 第 1、2 期合在一次提交（`eed9c870`）：日志执行器重写时预算循环只是 `settleQueued` 里的几行，拆开反而要写一个过渡版本。两条红测试在提交前都已存在。
2. `TestDirectCallsDoNotWaitForTheAOIBatch` 的检查列表去掉了 `Apply`：按 §8 第 5 项 A，`Apply` 保留为“在调用方线程排空并结算”，按设计会等正在进行的一步，业务路径上的 `Apply` 调用全部删除；其余 9 个入口在修前红、修后绿，断言未放宽。同一文件因 `Resubscribe` 去掉返回值改了调用写法。
3. 包内既有用例的调整（都是驱动方式，断言的承诺不变）：`TestBatchLimitRetainsLaterEntityPublication` 同步模式把预算设到最小，让政策阶段在第一批之后停；`TestPublicationHoldsReleaseOnDiscardAndClose` 的 hide / leave 在断言前结算一次（被作废事实的屏障在下一次结算、同一 Flush 的捕获之前释放）；`TestQueuedFactsInvalidatedByLeaveOrHideDoNotWedgeFlush`、`TestDrainTimeoutListsInterestStalls` 异步模式先让应用线程走一步（视野操作现在也由应用线程执行）；`TestQueuedFactThatCannotApplyFailsOneFlushOnly` 注入的事实改为“目标在场、位置越界”（旧注入的“目标不在场”现在按代际作废，不再是应用失败）；`TestResubscribeRestoresAReopenedObserversView` 改为数重试表；`manualApplier` 在应用线程已启动时先停掉它。
4. 撤除换代（§3.3 第 3 条）用“本次结算内撤除过的玩家 / 目标集合”实现，而不是按 pair 打标记：AOI 的离开事件要到结算时才归并，按 pair 打标记会漏掉它们。

### 10.5 验证

- `GOWORK=off go build ./...`；`go vet`（policy、`framework/sync/...`、`gameplay/movement`、`scripts/perf/sync-aoi`）；`GOWORK=off go test -race -count=1 ./framework/sync/... ./gameplay/movement/...` 全部 ok。
- `GOWORK=off go run ./cmd/glsvet ./framework/sync/entitysync/...`：退出 0，无输出。
- `scripts/test-sync-modes-generated.sh`：`ok syncmodes`。
- `scripts/test-mmorpg-scenes.sh`（直接调用 `Leave` / `Enter` + `Apply` 的场景夹具）：通过。
- `scripts/test-slg-generated.sh`（缺省模式）：失败于 `game/handler/slg_tasks_test.go:93`“too many arguments in call to `Sync_TickPlayerTimer`”，在未改动的 `494134ae` 上同样失败，与本重构无关；联盟夹具（`scripts/testdata/slg/alliance`，用 `Apply`）只在真实依赖模式运行，本次未跑。
- game-demo、根包与全量：见 10.6。
- 代码量（三个实现文件，含注释）：`interest.go` +298 / −238，`interest_queue.go` +198 / −178，`view.go` +106 / −84，净增约 100 行；删除 `supersedeQueuedMovesLocked` / `rewriteFactsLocked` / `subjectGone` / `syncViewsLocked` / `applyBatchLocked` / `wakeViews` 与两把锁之间的规则，新增命令种类、执行器、代际与撤除换代。

### 10.6 生成工程与全量

- game-demo（`roost project new planet -template game-demo --skip-deps` + replace 指回本检出）：`go build` / `go vet` / `go test ./...` 通过；`-race` 跑 `internal/service/game` 与 `game/controllers/player` 通过。场景桥两条用例（`TestTheBridgeKeepsEveryIdInTheEntityIdSpace`、`TestSceneRetriesASubscribeTheRoomRefused`）原来在 `Join` 返回后立即读 Manager 的订阅表，改为先走一个 tick（`scene.Flush`）再读——`Join` 现在只登记，tick 应用。
- 根包 `GOWORK=off go test -count=1 .` 与全量 `GOWORK=off go test -count=1 ./...` 通过。
- 未做：压测（§6，维护者统一安排）；`scripts/perf/nest-game.sh` 里的 nestgame 夹具（只在压测脚本里运行）；SLG 真实依赖模式。

### 10.7 维护者需要知道的

- §8 七项均按推荐实施，其中会改变对外语义的是：直接调用不再在返回时生效（行为变化 1）、`Resubscribe` 去返回值、`Apply()` 不在快池上调用、`Hide` 越过未确认移动时跟随视野的位置（行为变化 3）、`Hide` → `Show` 同一周期多一次全量。
- 实施中确认的一处既有缺陷：Leave → 关闭 / 重开会话 → 再 Enter 之间不调 `Apply` 时，重开会话的订阅丢失（修前探针原文见 10.3），本重构一并修复。
- 方向判断：Interest 的三个写入口收成一条日志之后，“谁在什么时候改状态”只有一个答案；之后同类问题（新的入口、新的交错）应当表现为日志里一条命令的执行语义，而不是又一处队列改写。若后续仍在这块反复出问题，下一步该看的是应用侧的批次与结算（`runLogLocked` / `applyLocked`），不是入口。

## 11. 实施后发现

### 11.1 异步模式下已执行、未结算的命令不计入 Drain（2026-10-22，产品缺陷，本重构引入）

- **现象**：合入 main（`e5b382a7`）后，`TestQueuedRetryThatIsNeverAcceptedDoesNotHoldDrainOpen/async` 在负载下偶发失败：`interest_test.go:569: stalled retry counted 0 times, want 1`（main `de3955a1` 上单跑本包 `-count=10` 失败 4 次，宿主 load 约 8；本机 `-count=30 -run` 复现 7 次，全部是 async 子用例）。
- **根因**：重构后视野操作与 Enter 等命令在异步模式下由应用线程执行（重构前视野变更由结算入口自己应用）。应用线程恰在 Drain 的某次 Flush 结算之后、Drain 检查 `Pending` 之前执行完一步时，变化只在 `touched` / AOI 事件缓冲里，还没说给 Manager；而 `queuedPending` 只看日志、事实预算与重试表，返回假，Drain 提前返回。违反“Drain 等到在途条目全部应用并结算”。门控事实不受影响（结算前一直计入 `facts`）；同步模式不受影响（执行与结算在同一次政策阶段里）。
- **修法**：应用侧新增 `unsettled`，`runLogLocked` 有进展时置位，`applyLocked` 结算时清除；`queuedPending` 为真时它也为真，`drainStalls` 列出 `applied_unsettled`。
- **红 → 绿**：确定性用例 `TestPendingCountsAppliedButUnsettledCommands`（手动让应用线程走一步后直接问 Pending，不依赖时序），修前 `the applier executed the relation view but Pending is false: Drain would return before the subscribe is said (subscribers of 2: [2])`，修后通过；原偶发用例 `-count=50 -run` 0 失败。
- **同时修正的用例口径**：`TestFlushSettlesButDoesNotApplyQueuedFacts` 假设“不经 queueFact 就没有应用线程”，重构后 Enter 等命令也会启动应用线程，它可能在断言前把注入的事实应用掉（`-race -count=50` 时出现 1 次 `Flush applied a queued fact on its own path: position={20 10} queued=0`）。改为先停掉后台应用线程（`manualApplier`），断言不变。
- **验证**（rebase 到 `b86c4fa4` 之后）：`-race -count=50 ./framework/sync/entitysync/policy/` 通过；负载下（两份 `entitysync` 包 `-race -count=6` 并行，`policy` 包 `-race -count=20 -cpu=1,2,8`）通过；`glsvet ./framework/sync/entitysync/...` 退出 0；根包、`scripts/test-sync-modes-generated.sh` 通过。
