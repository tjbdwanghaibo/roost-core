# Interest 的 Block AOI 与手写空间组件

状态：工作分支已实施，尚未发布。复用正式 Nest/Guard 提交门；不增加 Guard 业务回调，不修改生成器。Nest short 池默认等待容量为65536。功能/race验收与性能验收分开：本轮没有重新证明15分钟重档容量。

## 1. 正式链路

```text
Nest handler（持 Entity Guard）
  → 手写 SpatialComponent.Move / MoveWithObservedBlocks
  → 校验并准入不可变空间事实 → 写业务 DAO setter
  → Nest 原有 Admit / Confirm / Release
  → Sync Manager 政策阶段读取已确认事实
  → 更新块索引 → 并行计算 Observer → 归并全部订阅来源
  → 释放事实的内容发布屏障 → Sync 编码发送
```

setter仍只标脏。组件在业务线程中执行位置写入，handler必须传播错误。先做事实准入，队列满/越界不会先调用位置writer。writer应只写可回滚DAO，不能I/O、派发消息或在异步线程修改实体。DAO回滚沿用Nest，空间事实由 `SyncConditionFor` 的提交/拒绝结论控制。

组件要求当前Guard确实持有owner实例，并存在SyncMutation；没有配置 `NestOptionWithEntitySync` 或绕过正式执行作用域时返回 `ErrSpatialScope`。组件不保存另一份业务位置，不自动拦截直接DAO写入。

## 2. 接入：无需代码生成

以下代码展示接入形状，`positionDAO` 和 `player` 是业务已有对象；X/Y使用业务对应的生成或手写setter。组件可以作为手写实体/业务组件的普通字段保存，不需要新增生成标记。

```go
spatialComponent, err := policy.NewSpatialComponent(player, interest, true,
    func(at spatial.Point) {
        positionDAO.SetX(at.X)
        positionDAO.SetY(at.Y)
    })
// 初始化时处理 err；true 表示同时移动它的观察者（玩家 ID 由 InterestConfig.ObserverOf 从实体换算）。

// Nest handler 内：
return spatialComponent.Move(next)

// 位置与被观察覆盖必须一起变化时，只登记一个空间事实：
return spatialComponent.MoveWithObservedBlocks(next, observedBlocks)

// 只改覆盖、不移动位置：
return spatialComponent.SetObservedBlocks(observedBlocks)
```

保留低层 `Interest.QueueMove` / `QueueRelation`，新增 `QueueSpatial` / `QueueObservedBlocks`，便于业务已有组件接入。`QueueSpatial` 的空覆盖集合表示清空，普通 `QueueMove` 保留原覆盖。`BlockAt` 返回本Interest的块编号，越界为-1；块编号不能跨Interest混用。`ObservedBlocks` 返回覆盖副本。

## 3. Interest 的职责与锁

Interest继续归并 `spatial/block/self/team/...` 来源，不新增入口服务、另一套ID调度器或常驻AOI线程池。

- `queueMu` 只保护准入、角色登记、待处理事实与在途预算。Queue*不获取AOI计算锁。
- `mu` 串行化政策批次、直接生命周期操作及订阅归并。Profile/ObserverOf 等业务回调维持串行调用（ObserverOf 在入队时于业务线程调用，须为纯函数），不要求业务突然改成并发安全。
- 只允许 `mu → queueMu`，不能反向获取。生命周期退出的末尾在短队列锁内改写退出前的关系事实；Hide只撤销subject，仍保留合法observer事实。
- 已抽取事实与订阅归并中的事实仍占队列预算，不能用隐藏的第二条队列绕过容量。

`BatchSize` 默认1024，是每轮就绪事实的软上限，同一Nest提交的多条事实不会被切开；事实总数由 `MaxQueuedFacts`（默认65536）限制。会检查待确认前缀后面的其他ID，避免一个未确认事务饿死无关实体；同ID后续事实不得越过前驱。批次剩余就绪工作在on_change下继续唤醒，不等待20Hz兜底。抽取时在同一个短队列锁内固定框架原子提交门的结果，避免业务刚补齐后半段并提交、政策却只处理已抽出的前半段。

直接Enter/Watch/Show/Move/MoveView/Hide/Leave/Unwatch/SetObserverView与批次由同一协调锁串行。业务持Entity锁时使用Queue*或SpatialComponent，不同步等待直接生命周期操作或AOI计算。

## 4. Block 与 Observer

物理成员索引复用 `infra/base/spatial.BlockIndex`，已有每块RWMutex和按块编号加锁的跨块Move。新增同包 `aoiBlock` 保存observer反向索引及额外被观察实体ID，不复制Entity/DAO。唯一已应用位置由AOI的subject位置表保存，按位置计算唯一物理归属块。

observer 的编号与视野（D14，维护者 2026-10-20 决定 A1 + B1）：observer 按玩家 ID（会话 ID）编号，subject 按实体 ID 编号，两张表互不相通；observer 登记时带自身实体 `self`（只看不被看的 `Watch` 为 0），`evaluatePair`、`evaluateObserver` 的入场候选与 `evaluateObserved` 的覆盖块都按 `self` 自排除。每个 observer 记自己的 `ObserverView`（LOD 层级 + 视野大小）推出的进入 / 离开半径与分档：`resubscribe` 的方框、进入 / 保持 / 分档、候选筛选都读它；`self` 变化时连同覆盖可见集一起重评。单个 observer 的块预算按 `AOIConfig.MaxLeaveRadius` 在构造时计算（`worstObserverBlocks`），所有视野先经 `normalizeView` 夹取量化，运行期不会超过启动期检查过的块数。用法见 [guide/04-sync](guide/04-sync.md#附近的人观察者是玩家视野按观察者d14)。

当前政策批次分为：**串行短写入 → 并行Observer计算 → 串行订阅归并**。没有再实现并行写入的提交协调器，也没有读写同时进行的乐观重试。全部块写入完成后才开始读阶段，从而避免逐块读取时同一实体迁移造成漏读/重复。此选择缩短Nest入队路径，并把计算并行集中到较重的候选/距离工作，不能据此宣称空间写入吞吐随worker线性增长。

每个observer独占可见集和事件缓冲。`Workers` 零值取GOMAXPROCS，批次内最多启动该数量的短计算任务；数量还受dirty observer数限制，不为每个块建立goroutine，也不把AOI计算放到Nest io 池。任务完成后才开始下一批写入。

读锁用于短候选复制，精确距离、排序和订阅在块锁外。观察覆盖按该observer自己的leaveRadius实际求交，不写死九宫格。同块移动仍检查距离、滞回与档位。空间MaxVisible对observer跨块集合应用一次；保留现有距离并列与滞回契约。

AOICluster继续复用相同AOI块基础，跨区域成员和边界镜像仍由Cluster协调锁串行，镜像方框按观察者自己的离开半径（`SetObserverView` 改视野时重建镜像）；本轮没有把它扩成并行区域写入系统，正式Interest入队不经过该锁。

AOICluster 的 `MaxVisible` 在所有区域候选中统一选择最近 N 个，同距按实体 ID 升序；每次操作之后，每个观察者的入选集合都等于这个排序的前 N 个（不变量）。目标或观察者移动时，即使仍在同一距离档、区域没有产生 Enter/Leave/BandChanged，也必须重新选择（[aoi_cluster.go](../../framework/sync/entitysync/policy/aoi_cluster.go) `collectAfterMove`）：

- 目标移动：看得见它的观察者都登记在旧、新位置块的反向索引里（包括边界镜像），区域 AOI 判定可见性用的也是这份索引，不扫描全部观察者。其中没收到区域事件的观察者先做 O(候选数) 的边界检查 `crossesCapacityLocked`——入选的目标排到某个被抑制候选之后、或被抑制的目标排到某个入选目标之前——跨过边界才完整排序重排；收到区域事件的照常重排。检查的前提就是上面的不变量：上次重排后只有这一个目标的位置变了。
- 观察者移动：到所有目标的距离都变了，只完整重排它自己。
- `AddSubject` / `AddObserver` 对已存在 ID 的移动同样遵守此规则；未设容量时不做这项工作。

代价（2026-10-17，`BenchmarkClusterFlushRandomWalkCapped`：1000 目标、100 观察者、每观察者约 45 个候选、上限 16、每步全部目标移动）：每步 5.8ms → 10.7ms，分配 13.1k → 14.5k；对每个看得见目标的观察者都完整重排的做法为 62ms；不设容量的 `BenchmarkClusterFlushRandomWalk` 无差异（[样本](../performance/benchmarks/aoi-cluster-reselect-20261017/)）。修前的数字低，是因为同档移动根本不重选。容量层没有滞回：两个目标在第 N 名附近来回交换时每次移动都会产生 Leave/Enter，同一负载 97 步的 Enter 数 25012 → 28918（+15.6%）；维护者 2026-10-17 决定不加滞回（[KNOWN-LIMITS](../maintenance/KNOWN-LIMITS.md)）：抖动与重排代价只在候选数超过 N 时出现，不设 `MaxVisible` 或把 N 设在常规候选数之上时两者都没有；容量用作人群聚集时的带宽保护阀，N 按“正常场景达不到、聚集时限住带宽”来取。这是 Cluster 的容量规则，单区域 AOI 的 `MaxVisible` 只在未入选目标进入或移动时淘汰严格更远的成员（`evictFarther`，同距保留在位者），入选目标远离不会让其他目标补位，二者不能互相推断。

## 5. BeObservedEntity：唯一位置，多块可见

| 数据 | 含义 |
| --- | --- |
| ownerBlock | 实际位置对应的唯一物理归属块 |
| observedBlocks | 业务额外声明的被观察块集合；可为空，不移动实体 |
| observer.watchBlocks | 观察者覆盖的块集合 |

可见条件为 `watchBlocks ∩ observedBlocks ≠ ∅`，不再用实体到观察者的普通进入半径过滤。实体真实位于A而覆盖B/C时，观察B/C的人仍可见；同时观察B和C，也只有一个对象和一份订阅。移除B但仍命中C，不退订。

`block`是固定来源，命中的各个块不是独立订阅来源。与spatial/self/team收齐最终关系后再向Manager发布，来源切换不会先remove再create。支持 `SourceProfiles[SourceBlock][0]`，未配置时沿用现有Profile fallback；视图仍经过现有白名单和优先级校验，不因覆盖增多扩大权限。

block来源不占普通spatial的MaxVisible，因此总可见数可以超过原来49+自己；仍受会话传输、快照预算与背压约束，后续负载必须单独统计额外扇出。

覆盖集合先复制、排序、去重并完整校验，`AOIConfig.MaxObservedBlocks` 默认1024。非法块或超上限整体拒绝，不截断。更新只改旧/新差集，保持未变覆盖；写阶段结束前不允许observer读取部分结果。Hide/Leave清掉被撤销角色的覆盖和排队事实，同ID重建不能继承被撤销的旧事实。

本机SSR参考是 `aoi_abase_beobserved_entity.go`、`modules/map/block/func_aoi.go` 及 `watcher/wdsync/aoi/entityTheater.go`。借鉴其位置与被观察登记分离、上层去重及跨块锁序；Roost额外保留正式提交、回滚与多来源视图契约。

## 6. 提交门与内容发布屏障

只依赖“解锁且提交确认”不足以保证AOI事实已应用。分批处理或并发入队时，内容可能已经可发，而位置/覆盖还留在政策队列，导致新内容发送给旧订阅。

每条事实准入时调用 `SubjectSyncState.HoldSyncPublication`，在已应用并完成来源归并、被回滚/拒绝丢弃、角色撤销或Interest关闭时释放。释放幂等，多个Interest/多条事实各持一份。未完成事实令 `PrepareViews` 返回现有 `ErrSyncCommitPending`，保持脏数据等待后续轮次，不把它当发送失败。

此屏障**不参与SyncConditionFor，也不阻止Guard内冻结**，否则会出现“事实等自身完成”的循环。Confirm/Release、Remote部分拒绝等原有提交语义保持。没有在Guard挂AOI业务回调，也没有让WAL确认线程获取block锁。

## 7. 实现与验证

主要源码：`policy/spatial_component.go`（手写接入）、`interest_queue.go`（事实/预算/提交）、`aoi_block.go`（扩展覆盖与批次）、`aoi.go`（空间/observer）、`interest.go`（多来源最终归并）、`entity/sync_commit.go` 与 `subject_sync.go`（内容发布屏障）。全部留在既有包内。

```sh
GOWORK=off go test -race ./framework/entity ./framework/sync/entitysync/... ./framework/nest ./wiring/nest
# 正式生成DAO/Entity + Nest handler/Guard/undo + 手写Spatial组件；无NATS、无压测
ROOST_NEST_GAME_RACE=1 bash scripts/perf/nest-game.sh --check-spatial
```

定向回归包括多块去重/远块可见、唯一物理位置、晚到observer、集合复制、覆盖预算、spatial→block不重建基线、回滚/Remote拒绝、Hide/Leave/Close释放、入队不等政策锁、在途预算、未确认前缀公平、分批发布屏障、BatchSize=1下单handler多次移动不被切开、并行observer结果与独立距离/滞回参考对比。既有AOICluster与Sync/Nest回归一并运行。

修前 `TestQueuedSpatialFactBlocksContentUntilPolicyApplied` 确实失败：空间事实未处理时PrepareViews返回成功；修后返回ErrSyncCommitPending，处理后正常发布。证据 `/private/tmp/roost-policy-content-gate-red.log`；完整race日志 `/private/tmp/roost-spatial-final-race.log`；正式生成链路日志 `/private/tmp/roost-spatial-entry.log`。

## 8. 性能验证状态

2026-10-09已按1000玩家、10000实体、每玩家10消息/s跑两档各15分钟。1Hz/1%通过，业务/HB完成P99=17.50/12.18ms、Sync提交到客户端P99=20.06ms，零拒绝；10Hz/5%在802.919秒先Gate准入耗尽、805.465秒Nest队列满，最终EOF/值不一致，失败。详情及原始证据见[验收记录](../performance/GATE-AOI.md)。本轮客户端仍与Game同进程，负载使用QueueMove，未压额外observedBlocks扇出；手写SpatialComponent另有正式Nest功能回归。后续定位首错，隔离客户端，并补边界密集移动、观察者集中/分散和覆盖扇出，不能宣称重档瓶颈已关闭。

比较相同队列容量、输入与布局。保留历史4096失败数据，新默认65536单独标注。记录事实排队时间、块锁等待、observer计算、Nest最老等待、handler/HB及Sync到客户端P99、拒绝/内存和NATS出站。仍按P99≤50ms验收，保留最大值及>50ms数量；65536只是突发缓冲，不能将扩大队列当作吞吐提高。
