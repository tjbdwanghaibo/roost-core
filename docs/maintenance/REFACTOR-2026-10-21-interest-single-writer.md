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
   - 一步取到 `BatchSize` 个就绪事实即在这里切开：之后的命令与事实留到下一步（同一提交不切开，同现在），保证已就绪的事实不会被它后面的命令越过。**2026-10-22 改为纯前缀切开**（维护者决定 A，§11.2）：原写法在切点之后仍取用同一提交的事实，会越过留下的命令；现在切点只落在已取用的提交全部结束之后，部分就绪的提交整笔等。
3. 撤除命令（Leave / Unwatch / Hide）执行时，把因此释放到零的 pair 标记“换代”：同一次结算里它又被持有时，先退订再订阅，让 Manager 看到一次离开和一次进入（业务可能在两次调用之间注销 / 重新登记了目标或关闭 / 重开了会话）。

### 3.4 谁来执行（两种应用模式只差这一点）

| | ApplySync（缺省） | ApplyAsync |
| --- | --- | --- |
| 执行 | Flush 的政策阶段：一步接一步，直到没有可应用的条目或预计下一步加结算会超出耗时预算（预算 = 同步周期的一半，执行与结算一起计；2026-10-22 起，见 §12），然后结算 | 应用线程连续排空（同现在）；政策阶段只结算 |
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
- **结果（2026-10-22）**：见 [INTEREST-SINGLE-WRITER-20261022](../performance/INTEREST-SINGLE-WRITER-20261022.md)。1 成立（政策阶段在跑时直接调用均值与 p99 降一到两个数量级，AOI 基准 ±2%；视野登记表小操作每次多一条日志、慢几百纳秒）；2 成立（Nest ApplySync 0～8 视野 P50 低 6～11ms、P99 低 4～8ms，ApplyAsync 持平，ApplySync 16 视野仍超周期且政策阶段 P95 比改前高约 8ms）；3 在 80 人登录风暴规模下没有可分辨差异（改前一次进场景 handler 均值 8ms，其余与改后同量级；更多人时 demo 的复制会话因 `MaxObjects` 被关闭，见 KNOWN-LIMITS）。另修了 sync-aoi 的 Nest 模式每 tick 调 `Apply` 的夹具问题（`6cffbc03`，重构后它让压测线程替政策阶段做 AOI），direct 基线按本节第 6 条注明不可与改前直接比。

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
- 吞吐：`interest_queue.go:353` `settleQueued`，`:366` 预算循环，`:137` `syncApplyBudgetFor`。（2026-10-22 起预算循环改为 §12 的规则，强制点见 §12.5。）
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

### 11.2 批次切点之后，同一提交的后续事实越过排在它前面的命令（2026-10-22，产品缺陷，本重构引入，**已修：维护者 2026-10-22 决定 A，纯前缀切开**）

- **现象**（做 §12 时读 `runLogLocked` 发现，探针确认）：一步取满 `BatchSize` 在某条事实处切开后，之后的命令留到下一步，但与已取用事实同一提交的后续事实仍被取用（“同一提交不切开”）。于是这条事实越过了排在它前面、被留下的命令。命令是同一目标的 `Hide` → `Show`（换代际）时，这条事实的代际号是新的、而应用侧仍是旧代际，按代际规则被作废：移动丢失，目标停在 `Show` 的位置。
- **探针**（`main` `8428c75a`，未入库）：提交 X 在 Nest 守卫作用域里依次排 a 的移动、（另一提交 Y 排 d 的移动、业务线程 `Hide(b)` / `Show(b, {500 500})`）、b 的移动到 `{40 10}`，`BatchSize = 1`，`Apply()`：`b at {500 500} after log order [move a (X), move d (Y), Hide b, Show b, move b (X)], want {40 10}`；`BatchSize = 1024`（不切开）时 b 在 `{40 10}`。
- **范围**：要求同一提交的两条事实之间有别的提交的就绪事实或命令（让切点落在两者之间）与同目标的撤除 / 再放置命令；Nest 里提交 X 持有 b 的实体锁，`Hide` / `Show` 来自不经实体锁的直接调用（接入层、别的实体的提交后回调）。§12 的预算让同步模式下的切点更多（单位上限比 `BatchSize` 小），暴露面随之变大。
- **当时的选项**：A 切点只落在已取用提交全部结束之后（纯前缀切开）（推荐）；B 允许在这种情况下把提交切开（提交的 AOI 效果分两次结算可见）；C 不改，KNOWN-LIMITS 写明。

#### 11.2.1 决定与规则（维护者 2026-10-22 选 A）

维护者把四条约束收成一条规则——**一步只在日志的前缀边界停**：

1. 就绪事实不被越过；
2. 命令不被越过；
3. 提交不切开；
4. 预算 / `BatchSize` 只决定在哪个前缀边界停。

实施后的执行规则（`runLogLocked`，ApplySync 政策阶段、ApplyAsync 应用线程、显式 `Apply()` 与诊断读共用）：

- **一步的形状**：取出日志快照，从头按序处理；到了上限（取用的事实到达 `BatchSize`，或同步模式的单位数到达 `planSyncStep` 给的 n）之后，在第一个“已取用的提交全部结束”的位置切开——切点之前的条目全部处理过，之后的全部留下。“全部结束”按快照里的位置算：取用一笔提交的事实时，切点不能早于这笔提交在快照里的最后一条事实（`reach`）。
- **留在原位的事实**（切点之前也不执行，同重构前）：未确认的；同一 `(目标, 代际)` 前面有留下的事实的；以及下面这一条新规则。其余条目（命令、别的事实）越过它们不违反上面四条：命令本来就不等未确认的事实（§3.3），被越过的事实确认后按代际规则处理。
- **提交整笔取用**（新）：一笔提交在快照里只要有一条事实留在原位，它的事实这一步一条都不取用，整笔留在原位等（`waitingFacts`）。留下的事实又会挡住后面同一目标的事实，那些事实所属的提交也整笔等（连带），重复到没有新的提交被挡住为止。
- **命令也是切点的候选位置**：单位上限到了以后，切点可以落在命令之前（重构后 §12 写的“命令不触发切开”只是为了不让旧切点产生新的越过关系，纯前缀之下不再需要）。所以只有命令的一段日志在同步模式下也按预算分到几步执行，§12.4 原来的这条例外不再成立。

#### 11.2.2 未就绪的提交：整笔不取用（不选“整体等待”）

一笔提交的事实分属几个实体时，各条的就绪由各自实体的提交门链决定（`syncCommitGate.ready` 沿 `previous` 走，前序提交没确认就不就绪），所以同一笔提交可能一条就绪、一条还在等。两种处理：

- **整笔不取用（采用）**：这一步不取用它的任何事实，留在原位；后面的命令与别的提交照常执行。
- **整体等待（不采用）**：一步停在这笔提交的第一条事实前面。后果是一笔慢提交（WAL 等待、Remote 确认）挡住它后面的全部命令与无关提交，退回到“命令等未确认事实”，违反 §3.3；也让一步的前进量取决于最慢的提交。

为什么不会无限延长、不会饿死、不会死锁：

- **一步有界**：提交在全部事实登记之后才 Release，而就绪要求 Release；提交条件与日志快照在登记侧同一把锁里读，所以任何一笔可取用（全部就绪）的提交的全部事实都在这一步的快照里，`reach` 不超过快照末尾。切点不等快照之外的条目，一步不会因“提交还没结束”而等待或越过快照。
- **连带的计算有界**：`waitingFacts` 每多一轮至少多挡住一笔提交，轮数不超过快照里的提交数加一（通常一轮，有连带时两轮），单轮线性。
- **不死锁**：事实是否就绪只取决于提交门（确认 / 放行 / 拒绝 / 丢弃），提交门从不等 Interest 应用（`HoldSyncPublication` 不阻止 Admit，`queueFact` 满了返回 `ErrInterestQueueFull` 而不是等）。
- **不饿死**：就绪是单调的；前序提交确认（或拒绝、丢弃）之后，被挡住的提交在之后的一步里整笔可取用。按位置的连带只从前往后，等待集合由真实未通过的提交门决定，提交门全部通过后等待集合为空。前序提交结果不确定、永远不确认时（`Finish(indeterminate)`），它连带挡住的提交与重构前一样一直等（重构前只挡同一实体），积压由 `MaxQueuedFacts` 背压与 Drain 诊断（`commit_pending`）给出信号，等进程 fencing / recovery。
- **代价**：同一笔提交里已就绪的实体（上例的 a）要等另一个实体（b）的前序提交确认，可见性晚于重构前（重构前 a 先应用、b 后应用，提交被切成两半）；连带时，同一目标后续提交也跟着等。

#### 11.2.3 一步可能多出的量（上界）

到了上限之后、切点之前多处理的条目 = 从到达上限的位置到“已取用提交（含为了让它们结束而再取用的提交）最后一条事实”之间的条目：已取用提交在 handler 期间别的线程登记进来的命令与别的提交的事实，以及这些提交又跨过的部分。上界是这一步取到的日志快照（门控事实不超过 `MaxQueuedFacts`，命令不受这个上限约束，见 KNOWN-LIMITS ④）。所以：

- `BatchSize` 是一步取用就绪事实的软上限，AOI 一批可能多出交错进来的事实；
- 同步模式的单位上限同样可能被超过，超出部分计入这一步的耗时，下一步的估计照常跟上（§12.4 新增一条例外）；
- 交错只来自并发的 handler 与直接调用：单线程驱动（测试、离线工具）下提交是连续的，没有超出。

#### 11.2.4 实施（`framework/sync/entitysync/policy`）

- `interest_queue.go:299` `runLogLocked`：`:319` `waitingFacts` 标出留在原位的事实，`:321` `lastOf` 记每笔可取用提交在快照里最后一条事实的位置，`:340` 切点条件（到了上限且位置大于 `reach`），`:368` 取用事实时推进 `reach`，`:392` 应用失败的 `(目标, 代际)` 后续事实留到下一步；
- `interest_queue.go:440` `waitingFacts`（`:453` 三条留下的条件，重复到不变）；
- `interest_queue.go:113` 执行规则的注释、`interest.go:21` `BatchSize` 的说明。
- 例外（错误路径）：一条事实应用失败（例如位置越界）时同一 `(目标, 代际)` 后面的事实这一步不再应用，它所属的提交因此分两步应用；失败本身让一次 Flush 返回错误（`TestQueuedFactThatCannotApplyFailsOneFlushOnly`）。

#### 11.2.5 红 → 绿

修前（`a766a285`，`GOWORK=off go test -count=1 -run 'TestACutStep|TestACommitWithAWaiting|TestUnitLimitStops' ./framework/sync/entitysync/policy/`，用例已改写进 `interest_apply_test.go`）：

```
--- FAIL: TestACutStepKeepsLaterFactsOfATakenCommitBehindCommands/batch_size/sync (0.00s)
    interest_apply_test.go:808: sync, BatchSize 1: b at {500 500} after log order [move a (X), move d, Hide b, Show b, move b (X), move e], want {40 10}
--- FAIL: TestACutStepKeepsLaterFactsOfATakenCommitBehindCommands/batch_size/async (0.00s)
    interest_apply_test.go:808: async, BatchSize 1: b at {500 500} after log order [move a (X), move d, Hide b, Show b, move b (X), move e], want {40 10}
--- FAIL: TestACutStepKeepsLaterFactsOfATakenCommitBehindCommands/explicit_apply/sync (0.00s)
    interest_apply_test.go:824: sync, Apply with BatchSize 1: b at {500 500} after log order [move a (X), move d, Hide b, Show b, move b (X), move e], want {40 10}
--- FAIL: TestACutStepKeepsLaterFactsOfATakenCommitBehindCommands/explicit_apply/async (0.00s)
    interest_apply_test.go:824: async, Apply with BatchSize 1: b at {500 500} after log order [move a (X), move d, Hide b, Show b, move b (X), move e], want {40 10}
--- FAIL: TestACutStepKeepsLaterFactsOfATakenCommitBehindCommands/budget (0.00s)
    interest_apply_test.go:842: ApplySync, budget of one unit: b at {500 500} after log order [move a (X), move d, Hide b, Show b, move b (X), move e], want {40 10}
--- FAIL: TestACommitWithAWaitingFactIsNotTakenInPart/sync (0.00s)
    interest_apply_test.go:910: sync: while X's move of b waits for W: a at {25 10}, b at {900 900}, g at {60 10} (want X and Z held back whole at {900 900}); the later Show put c at {50 10} (want {50 10})
--- FAIL: TestACommitWithAWaitingFactIsNotTakenInPart/async (0.00s)
    （同上，async）
--- FAIL: TestUnitLimitStopsBeforeACommandToo (0.00s)
    interest_apply_test.go:927: a step limited to one unit over [Show 2, Show 3, Show 4] placed [{200 10} {300 10} {400 10}] (more false), want only 2 placed and more to run
```

- `TestACutStepKeepsLaterFactsOfATakenCommitBehindCommands`：探针改写的正式回归，日志 `[move a (X), move d, Hide b, Show b, move b (X), move e]`；三种切点来源——`BatchSize 1`（两种模式，先单走一步再排空）、显式 `Apply()`（两种模式）、ApplySync 预算只够一个单位（每条事实 30ms、预算 25ms、`BatchSize` 64，注入时钟）。红落在承诺本身（b 的最终位置），另检查第一次切开的位置：X 整笔在这一步（b 已在 `{40 10}`），e 还没取（切点落在 X 结束之后的第一个边界）。
- `TestACommitWithAWaitingFactIsNotTakenInPart`：`[move b (W，未确认)] [move a、move b (X)] [move g、move a (Z)] [Show c]`，一步之后 X、Z 整笔留下（Z 是连带），`Show c` 照常执行；W 确认后一步里按日志顺序全部应用。修前 a 被 X、Z 先移到 `{25 10}`，X 被切成两半。
- `TestUnitLimitStopsBeforeACommandToo`：单位上限 1 时只执行一条命令，`more` 为真（行为变化：命令也触发切开）。修前三条命令一步执行完。
- 修后全绿；包内既有用例 `TestPlanSyncStepRules` 与 4 条 `TestSyncApplyBudget*` 不改、照旧通过（`planSyncStep` 未改）。

变异核对（在修后的代码上各改一处，用例全部在修后代码上运行）：

| 变异 | 变红的用例与文本 |
| --- | --- |
| 恢复旧切点（只在就绪事实处切开、切点之后的命令留下，但仍取用已取用提交的后续事实） | `TestACutStepKeepsLaterFactsOfATakenCommitBehindCommands` 五个子用例：`b at {500 500} after log order [...], want {40 10}`；`TestUnitLimitStopsBeforeACommandToo`：`placed [{200 10} {300 10} {400 10}] (more false)` |
| 切点不等已取用提交结束（到了上限就切） | `.../batch_size/{sync,async}`、`.../budget`：`the first step left b at {900 900}: commit X was cut after its move of a, want its move of b {40 10} in the same step`（显式 `Apply` 排空到底，结果相同，不变红） |
| 去掉“提交整笔取用”（只看未确认与同目标） | `TestACommitWithAWaitingFactIsNotTakenInPart`：`a at {25 10}, b at {900 900}, g at {60 10} (want X and Z held back whole ...)` |
| 连带只算一轮（先按未确认标提交，再单遍标记） | `TestACommitWithAWaitingFactIsNotTakenInPart`：`a at {900 900}, b at {900 900}, g at {60 10}`（Z 的 g 先走，Z 被切开） |

#### 11.2.6 既有用例的调整

- `TestBatchLimitRetainsLaterEntityPublication`（同步模式预算 1ns）：两条 `Enter` 移到两条事实之前。原顺序 `[Enter 1, 事实 1, Enter 2, 事实 2]` 下，第二次政策阶段的单位上限是 1，按新规则只执行 `Enter 2` 就停（命令计入单位），断言“第二条事实在下一次应用后放行”要再走一次；断言不变，只改驱动顺序。修前修后都绿。

#### 11.2.7 验证

以下退出码均为 0（rebase 到合入“编码失败断开会话”之后的 main 上运行）：gofmt、`go vet ./framework/sync/... ./gameplay/movement/...`、`go run ./cmd/glsvet ./framework/sync/entitysync/...`、`-race -count=10 ./framework/sync/entitysync/policy/`、`-race ./framework/sync/... ./gameplay/movement/...`、根包、`scripts/test-sync-modes-generated.sh`、全量 `GOWORK=off go test -count=1 ./...`。没有跑压测：每步多一遍预扫描（`waitingFacts` 与 `lastOf`，线性，有连带时两遍）与两张小表，开销没有测。

### 11.3 执行器“批次与结算”这一层剩下的约束，以及钉住它们的用例（2026-10-22）

§10.7 的方向判断说过：日志收成一条之后，再出问题该看应用侧的批次与结算。这一层在重构后连续出了三处（§11.1 已执行未结算不计入 Drain，`d3a4486d`；§12 预算不计结算，`be49684b`；§11.2 切点越过命令，本节）。三处都不是入口的问题，而是“一步做多少、什么时候算做完”的规则各自写成了局部例外（“同一提交不切开”是切点的例外，“命令不触发切开”是预算的例外），例外之间互相打架。**方向判断**：是细节问题叠在一个缺少统一表述的前提上，不是单写者方向本身的问题；把切点收成“一步 = 日志的一个前缀减去整笔等待的提交”之后，一步的形状只剩两个自由度：哪些事实留在原位（只由提交门与同目标顺序决定），在哪个前缀边界停（只由上限决定）。之后若这一层再出问题，先看是不是又在这两条之外加了例外。

剩下的约束与钉住它们的用例（`framework/sync/entitysync/policy`，“两种模式”指用例经 `forEachApplyMode` 各跑一遍；第 16 条为 2026-10-22 审查后补）：

| # | 约束 | 用例 |
| --- | --- | --- |
| 1 | 命令按登记顺序执行，已提交的移动不被之后的放置拖回 | `TestPlacementAfterAQueuedMoveIsNotDraggedBack`（两种模式） |
| 2 | 一步只在前缀边界停：已取用提交的后续事实不越过留下的命令，切点落在第一个边界（`BatchSize` / 显式 `Apply` / 预算三种来源） | `TestACutStepKeepsLaterFactsOfATakenCommitBehindCommands`、`TestUnitLimitStopsBeforeACommandToo`、`TestBatchLimitRetainsLaterEntityPublication` |
| 3 | 提交整笔取用：部分就绪的提交整笔等，连带挡住同目标后续提交；命令不等它们 | `TestACommitWithAWaitingFactIsNotTakenInPart`（两种模式） |
| 4 | 提交门：回滚 / 拒绝的事实不生效、释放屏障；Remote 拒绝只丢自己的事实 | `TestApplierNeverAppliesRolledBackFactsAndKeepsOrder`、`TestQueuedInterestFactsFollowCommitAndRollback`、`TestRemoteRejectKeepsCommittedLocalInterestFact`、`TestPureRemoteRejectDiscardsItsInterestFacts` |
| 5 | 同一 `(目标, 代际)` 不越过未确认的前驱，其他目标可以越过（不饿死无关实体） | `TestApplierKeepsPerEntityOrderAcrossOutOfOrderConfirms`、`TestInflightFactsKeepBudgetAndPendingHeadDoesNotStarve` |
| 6 | 被撤除 / 再放置越过的旧代际事实作废 | `TestAnUnconfirmedMoveOvertakenByLeaveAndEnterIsDropped`、`TestQueuedFactsInvalidatedByLeaveOrHideDoNotWedgeFlush` |
| 7 | 撤除换代：同一次结算里撤除后又持有的 pair 先退订再订阅 | `TestLeaveAndEnterInOneSettleSubscribeTheReopenedSessionAgain` |
| 8 | 发布屏障在结算之后才释放（内容与它带来的进出视野同一帧） | `TestAppliedFactHoldsContentUntilSettled`、`TestQueuedSpatialFactBlocksContentUntilPolicyApplied`、`TestPublicationHoldsReleaseOnDiscardAndClose` |
| 9 | 应用中、已应用未结算的事实仍占 `MaxQueuedFacts` | `TestAppliedButUnsettledFactsKeepTheQueueBudget`、`TestInflightFactsKeepBudgetAndPendingHeadDoesNotStarve` |
| 10 | Drain：日志、未结算的事实与已执行未结算的命令都算没做完；永远不被接受的重试停滞后不再挡 Drain | `TestPendingCountsAppliedButUnsettledCommands`、`TestQueuedRetryThatIsNeverAcceptedDoesNotHoldDrainOpen` |
| 11 | ApplySync 一次政策阶段（执行 + 结算）按预算停，至少前进一个单位，积压在之后的 Flush 按序做完 | `TestSyncApplyBudgetFinishesWhatFitsInOnePolicyPhase`、`TestSyncApplyBudgetStopsWithinBudgetAndKeepsOrder`、`TestSyncApplyBudgetStillProgressesWhenOneStepExceedsIt`、`TestSyncApplyBudgetCountsTheSettle`、`TestPlanSyncStepRules`、`TestSyncApplyOneFlushDrainsTheCommittedBacklog` |
| 12 | 无法应用的事实丢弃并只让一次 Flush 失败 | `TestQueuedFactThatCannotApplyFailsOneFlushOnly` |
| 13 | 业务线程上的入口不等 AOI 批次 | `TestDirectCallsDoNotWaitForTheAOIBatch`（两种模式） |
| 14 | 谁、何时执行日志：ApplySync 在政策阶段，ApplyAsync 在应用线程、Flush 只结算 | `TestApplyModeDecidesWhenACommittedMoveIsVisible`、`TestFlushSettlesButDoesNotApplyQueuedFacts` |
| 15 | 政策阶段的拒绝交给 `OnRefusal` | `TestPolicyPhaseRefusalsReachOnRefusal` |
| 16 | 跨锁边界只传值：`registry.log` / `facts` / `active` 只在 `registry.mu` 内读写，应用方与诊断读（持 `Interest.mu`）经 `takeLog` / `requeue` / `releaseFacts` / `backlog` / `drainSnapshot` 在锁内取值快照，锁外不回读；Drain 诊断的全部判断用同一份快照（RV-20261022-03） | `TestDrainStallsReadsFactCountUnderTheRegistryLock`（两种模式，`-race`；状态必须是“已应用、未结算、没有排队事实”，已结算的状态会短路那次读取） |

约束 16 的来源与阶段划分：2026-10-22 审查 RV-20261022-03 发现 `drainStalls` 释放 `registry.mu` 之后又读 `r.facts`，与只持 `registry.mu` 的 `QueueMove` 写入竞争（只读诊断，未发现影响 AOI 位置或订阅）。修复时把应用方的一步拆成 `takeLog`（登记侧锁内取走日志并固定提交条件）→ `executeLogLocked`（只持 `mu`，更新空间，末尾经 `recordStepLocked` 记待结算状态；完成事实的切片作参数传入而不是作为返回值，保持在栈上）→ `requeue`（登记侧锁内放回留下的条目、带回计数快照），结算与 Drain 诊断同样只经快照函数跨锁；切点、预算、执行顺序与锁序 `mu → registry.mu` 不变。这一条是静态约束，`-race` 用例只覆盖 Drain 诊断与 `QueueMove` 的交错；新增跨锁读取时以“只经快照函数”这条规则复核，不能只靠这一条用例。

没有用例钉住的：一步多出的量的上界（§11.2.3，只检查了“切点落在第一个边界”，没有对交错量做数值断言）；应用失败把同一提交分两步应用（错误路径，§11.2.4）；`waitingFacts` 连带的轮数上界（只有两轮的用例）。

## 12. ApplySync 预算把结算也算进去（2026-10-22，维护者决定 A）

### 12.1 问题与决定

§10 的预算只在两步之间检查：检查通过后还会再执行一整步（最多 `BatchSize` 条事实），之后的结算（AOI 事件归并、订阅说给 Manager、释放发布屏障）不计入预算。16 视野（诊断构建）时政策阶段每 tick P95 38ms、Flush 超过一个周期、P99 128ms（[INTEREST-SINGLE-WRITER-20261022 §4](../performance/INTEREST-SINGLE-WRITER-20261022.md#4-16-视野与-d19-历史数字)）。

维护者 2026-10-22 选 A：**预算要把结算也算进去；检查时为结算预留时间，并按上一步的耗时估算下一步，预计会超就停，剩下的留到下一个周期。** 取舍：Flush 时长有上界，全服同步节奏不受影响；代价是重负载下 AOI 的变化会晚一两个周期可见。已提交的排队事实不丢、不乱序，代际号作废规则不变，`MaxQueuedFacts` 背压照常。

### 12.2 规则（`planSyncStep`）

- **预算** B = 同步周期的一半（至少 1ms），不变；现在覆盖一次政策阶段的“执行日志 + 结算”（含 `OnRefusal` 回调）。
- **单位**：一步里执行的一条命令或取用的一条事实（被丢弃的事实不算）。
- **成本估计**（应用侧状态，跨政策阶段保留）：
  - 每单位执行耗时 c = 最近一步的耗时 / 它的单位数；
  - 每单位结算耗时 σ = 最近一次结算的耗时 / 它结算的单位数（自上次结算以来执行的全部单位，含显式 `Apply` 与诊断读执行的）；
  - 没有单位的步 / 结算不更新估计；还没测到时为 0。
- **结算预留** R = max(B / 10, σ × 已执行未结算的单位数)。
- **每一步之前**：n = ⌊(B − 已用 − R) / (c + σ)⌋。
  - n ≥ 1：这一步最多执行 n 个单位；
  - n < 1：停止（记 `budget_stop`，提醒 Manager，on_change 下立即再来一轮），剩下的留给下一次 Flush；
  - **本次政策阶段的第一步总要执行**，n 至少取 1；
  - c + σ 都还没测到（启动后的第一步）：不限单位数，只受 `BatchSize` 限制。
- **切点**：单位数到达 n 后，在第一个前缀边界切开，与 `BatchSize` 是同一条规则（2026-10-22 起为纯前缀切开，§11.2：切点之前全部执行、之后全部留下，切点只落在已取用的提交全部结束之后；命令也计入单位，切点可以落在命令之前）。应用线程（ApplyAsync）与显式 `Apply` 不限单位数，只受 `BatchSize` 限制。

### 12.3 为什么这样定

1. **结算按单位摊，不按一次结算的绝对值预留**。结算的工作（AOI 事件、touched 的 pair、要释放的屏障）由本次政策阶段执行的单位产生，与执行量成正比。按最近几次结算的绝对值（滑动最大值或 EWMA）预留，预留量和这次要做的量脱钩，负载变化时会振荡。推演（c = σ = 1ms/单位，B = 25ms，下限 2.5ms）：
   - 滑动最大值（最近 4 次）：第 1 轮只留 2.5ms，执行 22 个单位，结算 22ms，政策阶段 44ms；之后 4 轮预留 22ms，每轮只做 3 个单位；窗口滑过去又回到 22 个单位、44ms。平均每轮约 6.8 个单位（理想 12.5），每 5 轮超一次周期。
   - EWMA（R ← R + α(S − R)）：不动点斜率 1 − α(1 + σ/c)，σ/c > 2/α − 1（α = 1/4 时为 7）时发散振荡。
   - 按单位摊：带固定成本 F 时，u ← B / (c + σ + F/u)，不动点 u* = (B − F) / (c + σ)，导数 F / B < 1，单调收敛；F ≥ B（只结算就超预算）时收敛到每轮一个单位，这时系统已经过载，由 `MaxQueuedFacts` 背压给出信号。
2. **执行耗时取最近一步**，不取 EWMA：规则只有一条、确定、可测。噪声的影响有界：低估时多出一步的误差（今天的粒度是一整个 `BatchSize`），高估时只是多走几步。
3. **“至少执行一步”和“不超预算”冲突时，先保证前进**。优先级：结算必须做（屏障要释放）> 每次政策阶段至少前进一个单位 > 预算。超出量不超过一个单位（同一提交不切开时是一个提交）加结算。同时把批切到 n（最少 1）而不是整个 `BatchSize`，一个单位本身超预算时每个 Flush 前进一条，不会饿死，也不会因一步过大把 Flush 拉长。

### 12.4 上界与边界

- 每单位成本在相邻两步 / 两次结算之间稳定时，政策阶段不超过 B。以下情形可以超出：
  - **启动后（或成本还没测到时）的第一次**：没有估计，只受 `BatchSize` 与预留下限 B/10 约束；
  - **一个单位（或一个提交）本身超预算**：超出量为这一步加结算；
  - **已取用的提交跨过了切点**：切点要等已取用的提交全部结束，一步多执行其间交错进来的条目（§11.2 的上界）；
  - （原有一条“只有命令的一段日志整段在一步里执行”，2026-10-22 纯前缀切开之后命令也触发切开，不再成立。）
  - **成本突变**：估计晚一步（执行）或一次结算（结算）跟上。
- **可见性**：积压超过预算时，排在后面的事实与命令晚一个或几个 Flush 生效（全服同步节奏不变）。门控事实的发布屏障在应用并结算之后才释放，所以实体内容与它带来的进出视野仍在同一帧；被留下的事实仍计入 `MaxQueuedFacts`。
- 同一 Interest 内单写者不变；成本估计只在应用侧（持 `mu`）读写。

### 12.5 强制点（`framework/sync/entitysync/policy`）

- `interest_queue.go:179` `planSyncStep`（规则），`:170` `applyCosts`，`:175` 预留下限；
- `interest_queue.go:510` `runSyncStepsLocked`（每步之前规划、步后更新 c、预算停止计数、剩余积压），`:472` `settleQueued`（结算计时、更新 σ、两个耗时指标）；
- `interest_queue.go:299` `runLogLocked(unitLimit)`，`:340` 切点条件（`BatchSize` 或单位上限，且已取用的提交都已结束，§11.2）；
- `interest.go:811` 结算时清零已执行未结算的单位数；时钟是 `Interest.now`（测试可替换）。
（行号为 2026-10-22 纯前缀切开之后。）

### 12.6 红 → 绿

确定性用例（`interest_apply_test.go`，`budgetHarness`）：Interest 的时钟 = 起点 + 每条已应用事实 perFact + 每个进入事件 perEvent（结算里按距离档取层级时计数，即结算耗时），不靠 sleep。估计来自之前的步与结算，用例先走一次小的 Flush 让政策阶段测到成本。

修前（`8428c75a`，`GOWORK=off go test -count=1 -run 'TestSyncApplyBudget' ./framework/sync/entitysync/policy/`，用例里引用的指标名在修前用一个临时测试文件定义）：

```
--- FAIL: TestSyncApplyBudgetStopsWithinBudgetAndKeepsOrder (0.00s)
    interest_apply_test.go:578: Flush 1: the policy phase spent 90ms (steps + settle), over the budget 25ms; 0 facts left
--- FAIL: TestSyncApplyBudgetStillProgressesWhenOneStepExceedsIt (0.00s)
    interest_apply_test.go:622: Flush 1 applied 6 facts at 30ms each (budget 25ms, BatchSize 4): want the step cut to exactly one
--- FAIL: TestSyncApplyBudgetCountsTheSettle (0.00s)
    interest_apply_test.go:650: Flush 1 applied 40 facts and spent 84ms with the settle (budget 25ms): the settle was not reserved
```

修前代码用真实时钟计预算、不读 `Interest.now`，所以注入的耗时对它不起作用，三条红在“按注入耗时计，一次政策阶段不超过预算 / 单步超预算时切到一条”这一承诺上。规则本身由下面的变异钉住。`TestSyncApplyBudgetFinishesWhatFitsInOnePolicyPhase`（预算内一次做完、不记预算停止）修前修后都绿，是与原行为一致的对照。修后全绿，另有规则表 `TestPlanSyncStepRules`。

| 变异 | 变红的用例与文本 |
| --- | --- |
| 去掉结算预留（预留为 0，n 只除以 c） | `TestSyncApplyBudgetStopsWithinBudgetAndKeepsOrder`：`Flush 1: the policy phase spent 50ms (steps + settle), over the budget 25ms; 35 facts left`；`TestSyncApplyBudgetCountsTheSettle`：`Flush 1 applied 40 facts and spent 84ms with the settle (budget 25ms): the settle was not reserved` |
| 去掉“第一步至少一个单位” | `TestSyncApplyBudgetStillProgressesWhenOneStepExceedsIt`：`Flush 1 applied 0 facts at 30ms each (budget 25ms, BatchSize 4): want the step cut to exactly one` |
| 切点忽略单位上限（只看 `BatchSize`） | `TestSyncApplyBudgetStillProgressesWhenOneStepExceedsIt`：`Flush 1 applied 4 facts ...`；`TestSyncApplyBudgetCountsTheSettle`：`Flush 1 applied 40 facts and spent 84ms ...` |

### 12.7 指标

`entitysync.interest.policy_duration`（政策阶段整次）、`entitysync.interest.settle_duration`（其中结算）、`entitysync.interest.log_backlog`（ApplySync 每次政策阶段结束时日志里剩下的条目）、`entitysync.interest.budget_stop.total`（因预算停下、仍留有可执行条目的次数），说明见 [OBSERVABILITY](../framework/OBSERVABILITY.md)。新指标按命名规范用点号分层（导出后与既有 `entitysync_interest_*` 同前缀）。

### 12.8 验证

见 CHANGELOG 与本轮汇报：gofmt、`go vet ./framework/sync/... ./gameplay/movement/...`、`go run ./cmd/glsvet ./framework/sync/entitysync/...`、`-race -count=10 ./framework/sync/entitysync/policy/`、`-race ./framework/sync/... ./gameplay/movement/...`、根包（含指标门禁）、`scripts/test-sync-modes-generated.sh`、全量 `GOWORK=off go test -count=1 ./...`，退出码均为 0。

### 12.9 压测（待补测，维护者 2026-10-22：先处理代码，压测放到今晚）

16 / 0 / 8 视野 ApplySync 的前后对比还没跑，计划与复跑命令见 [INTEREST-SINGLE-WRITER-20261022 §7](../performance/INTEREST-SINGLE-WRITER-20261022.md#7-applysync-预算计入结算待补测今晚)。代码提交 `be49684b`。
