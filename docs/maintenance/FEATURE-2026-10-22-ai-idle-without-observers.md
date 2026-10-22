# 功能设计：附近没有玩家观察时 AI 暂停（2026-10-22）

**状态：方案已写，待优化阶段实施（维护者 2026-10-22：“这项先留到优化阶段做，现在不实施”）。本篇只有方案，仓库里没有对应代码。**

登记在 [DESIGN-TODO“优化项（下一阶段）”](DESIGN-TODO.md#优化项下一阶段) 第 1 组方向二。基线 `603ec537`（origin/main），文中 `path:line` 都按这个提交核对。codebase-memory 图的代际早于本轮，结论以源码为准。

## 0. 维护者原话与已定事项

- 触发（2026-10-22）：“一般是没有 AOI 计数时候是 AI 停止”。
- 暂停的做法（2026-10-22）：“我之前的处理是单开一个 running 处理这个事，发现可以停的就使用 nest 的广播信息，通知暂时停止。”
- 恢复的做法（2026-10-22）：“恢复的话一般是 entity 自己心跳恢复。”
- 定位（2026-10-22）：这是一个 feature，使用者自己决定用不用，**缺省不启用**。
- 排期（2026-10-22）：留到优化阶段做，现在不实施。

已定、不再拍板：

| # | 事项 | 决定 |
| --- | --- | --- |
| F1 | 暂停由谁发起 | 一个独立扫描者按周期读 AOI 观察者计数，对可以停的实体发 Nest 广播（维护者做法） |
| F2 | 恢复由谁发起 | 实体自己的心跳：暂停期间留一个低频心跳，每拍查自己的计数，大于 0 就恢复（维护者做法） |
| F3 | 缺省开关 | 缺省关闭。代码层（每个 AI 显式启用）和进程层（wiring Mod 配置）都要打开才生效；不启用时扫描者不运行，Interest 不维护计数，开销为零 |
| F4 | 生成器 | `roost add behavior --ai` 缺省不启用，只在生成的注释里写怎么打开 |

## 1. 要解决的问题

一个场景里怪物多、玩家少，大部分怪物附近没有玩家。它们的 `"ai"` 心跳（`gameplay/ai/beat.go:88` `syncBeat`，缺省 500ms）照常决策，决策又发起移动动作，动作组心跳（`actionflow` 的 `BeatRunner`，缺省 100ms）跟着推进位置，位置再进 Interest 的排队事实。没人看见的怪物在消耗三份 CPU：AI 决策、动作推进、AOI 移动。

本功能只停 **AI 决策**。AI 不再发起新动作后，当前动作走完，动作组空闲，组心跳自己停（`gameplay/actionflow/heartbeat.go:18`“组空闲时停”），移动也就停了。所以停 AI 就够了，不需要另外处理移动和 AOI。

不在范围：移动心跳的缺省档位（同一组的方向一）、无观察者降档（同一组的次要选项）。

## 2. 总体结构

```
          ┌─────────────────────── 扫描者（每进程一个 goroutine，周期 P_scan，缺省 2s）──────────────┐
          │ 1. 按计数源分组，批量读已登记 AI 实体的观察者计数（Interest 只读快照，RLock）            │
          │ 2. 连续两轮读到 0、且实体没在暂停 → 放进暂停名单                                         │
          │ 3. nest.Client.DispatchBroadcast("ai.idle.pause", 名单)                                   │
          └───────────────────────────────────────────────────────────────────────────────────────────┘
                                │ Nest 广播（按锁组 / worker 拆批，每批至多 32 个）
                                ▼
   实体 worker，实体锁内：ai.idle.pause handler
     核对实例身份 → 再读一次计数（仍是 0 才停）→ Controller 进入“无观察者暂停”
     → 结束当前任务、清空排队动作，当前动作走完 → "ai" 心跳换到探测档 P_probe（缺省 2s）
                                │
                                ▼
   实体 worker，实体锁内：每 P_probe 一拍 "ai" 心跳（探测拍）
     不决策，只读自己的观察者计数 → 大于 0：退出暂停 → 策略重新决策（Resume 钩子）→ 本拍立即 Tick 一次
     → "ai" 心跳回到策略的周期
```

三处各管一件事：Interest 只维护计数、提供只读快照；扫描者只负责“发现可以停的实体并通知”；Controller 在实体锁内切换状态。没有任何一方跨线程直接改 Controller。

## 3. 观察者计数（Interest，`framework/sync/entitysync/policy`）

### 3.1 计什么

**定义**：一个目标的观察者计数 = 本 Interest 里持有 `(玩家, 目标)` pair、且持有它的视野里至少有一个不是 self 的玩家数。

- 按 **held**（Interest 要它订阅）计，不按 Manager 是否接受订阅计。被拒绝的订阅（例如 `MaxObjects` 满）说明玩家就在附近，怪物仍要行动（可能马上会打到他）。held 的写入口是 `contribute`（`interest.go:847`）。
- 排除 self：contribution 的 view 为 0 就是 self（`interest.go` 里 `contribution` 的注释）。玩家自己的实体不因为“自己看自己”而算有人看。主视野本来就不订阅自己跟随的实体。
- 空间视野、业务视野、关系视野（队伍、联盟）、块覆盖都计入：只要有玩家能看到这个目标，就算有人看。
- 计的是结算之后的状态。计数在 `settlePair`（`interest.go:869`）里更新，结算发生在政策阶段（ApplySync）或应用线程之后的政策阶段（ApplyAsync），所以计数和 Manager 看到的订阅同一时刻变化。

### 3.2 怎么维护（只在打开时）

新增配置 `InterestConfig.TrackObservers bool`，缺省 false。

- 关闭时：`settlePair` 只多一次 bool 判断，不建计数表，查询返回 `ErrObserversNotTracked`。满足 F3“开销为零”。
- 打开时，应用侧（持 `mu`）：
  - `hold` 多一个 `counted bool`；
  - `settlePair` 末尾算“现在是否有非 self 的贡献”，和 `counted` 不同时给 `observerCount[subject]` 加或减 1，并把 subject 记进 `countDirty`；
  - pair 被删除（layers 为空）且 `counted` 为真时减 1。
- 发布：`applyLocked`（`interest.go:801`）结尾一次性把 `countDirty` 里的目标拷到快照表 `observed map[int64]int32`。快照表由一把独立的 `observedMu sync.RWMutex` 保护，计数为 0 的从表里删掉，表的大小就是“有人看的目标数”。写锁只持有 O(本次结算变化的目标数)，例如几十到几千次 map 写，约几微秒到几十微秒。
- `Close`（`interest.go:931`）清空快照，之后全部目标读到 0。场景关闭，怪物在之后的扫描里被停掉。
- 锁序：`mu → observedMu`。`observedMu` 下不取别的锁。

不改执行器的切点、预算和日志规则：计数只在结算侧记账，不加回调、不加命令。这和维护者“只读快照、不在结算路径加回调”的要求一致。

### 3.3 只读查询

```go
// ObserverCount 返回目标的观察者计数（见 3.1）；没打开 TrackObservers 时返回 ErrObserversNotTracked。
// 只取 observedMu 的读锁，不等 AOI 批次，可以在 Nest 快池（心跳回调）里调用。
func (in *Interest) ObserverCount(subject int64) (int, error)

// ObserverCounts 一次读一批目标，counts[i] 对应 subjects[i]；一次读锁覆盖整批（扫描者用）。
func (in *Interest) ObserverCounts(subjects []int64, counts []int32) error
```

**锁代价**

- 读：一次 `RLock` / `RUnlock`（两次原子操作）加一次 map 查找。没有竞争时约 30～60ns。读者之间不互斥，读锁从不等 `mu`，也就不等 AOI 批次。
- 读者唯一可能等的是 3.2 的发布写锁，等待时间以发布的那几十微秒为上限。按 roost-coding 的快池规则，这属于“短锁”，和业务线程取登记侧 `registry.mu` 同一级（REFACTOR-2026-10-21 §3.1）。
- 探测拍的读频率：1 万个暂停中的怪物、`P_probe = 2s` 时约 5000 次/s，总开销约 0.3ms CPU/s，可以忽略。没暂停的 AI 不读，它们的心跳不查计数。
- 扫描者：每轮把已登记的实体按 1024 个一组调 `ObserverCounts`，一次读锁约 30～60µs。1 万个实体每 2s 一轮，读锁总时长约 0.5ms/轮。
- RWMutex 读者多、写者少时，Go 的写锁会让新读者排在它后面，所以读者最坏等一次发布。
- 备选（不推荐首期做）：给每个登记的目标一个 `*atomic.Int32`，结算直接写原子数，读完全无锁。代价是登记要经日志进入应用侧，计数槽也要有生命周期管理。等实测证明 RWMutex 有竞争再换。

## 4. Controller 的“无观察者暂停”（`gameplay/ai`）

### 4.1 状态

`Controller` 增加一个独立的原因位 `unobserved bool`，和业务冻结的 `frozen` / `frozenUntil`（`controller.go:139`～`:180`）互不覆盖：

- 决策条件 `ready()`（`controller.go:322`）改为 `notifiable() && !frozen && !unobserved`。
- `Freeze` / `FreezeFor` / `Recover` 只动 `frozen`，暂停 / 恢复只动 `unobserved`。两个原因都清掉，AI 才决策。这就是要求里的“叠加”：现在业务 `Freeze` 是布尔、互相覆盖（`Freeze` 覆盖 `FreezeFor` 的定时），本功能不改它的语义，只是多一个独立原因。不需要把 `Freeze` 改成计数：它的调用方只有业务一方。
- 结束通知照常送达（`notifiable()` 不看 `unobserved`）：暂停期间当前动作结束时，策略照样收到 `OnActionEnd`，否则恢复后 `BehaviorStrategy` 的叶子会挂在 Running（与 `notifiable` 注释给冻结的理由相同）。
- `OnEvent` 在暂停期间丢弃（走 `ready()`），与冻结一致。

### 4.2 心跳档位

`beatInterval`（`beat.go:121`）按下表取档：

| 状态 | `"ai"` 心跳 |
| --- | --- |
| 没有策略，或 `Freeze`（无定时） | 停（同现在）。之后业务 `Recover` 时，如果仍在暂停，就回到探测档 |
| 暂停中（`unobserved`），没有业务冻结 | 探测档 `P_probe` |
| 暂停中 + `FreezeFor` | 探测档 `P_probe`（到期在探测拍里检查。暂停期间本来就不决策，晚一个探测周期发现到期没有可见差别） |
| 只有 `FreezeFor` | 同现在：策略周期，用来检查到期 |
| 正常 | 同现在：策略的 `TickInterval`，或 `BeatConfig.Period` |

探测拍复用同一个 `"ai"` 登记，只是 `SetPeriod` 换档（`heartbeat/host.go:256`），不新登记，也不新占名字。

### 4.3 暂停（广播 handler 里，实体锁内）

1. 核对：Controller 已登记、没 Close、属于消息目标那个实例（同 `heartbeat` 的 `runBeat` 对实例身份的核对，`host.go:374`）。不符合就丢弃并计 `stale`。
2. **再读一次计数**：大于 0 就不停，计 `observed`。扫描到 handler 执行之间可能有玩家走近。
3. 置 `unobserved = true`。按 4.5 的选项处理在途动作，缺省是“结束当前任务、清空排队动作，当前动作走完”。最后 `syncBeat` 换到探测档。
4. 这个 handler 永不返回错误。`unobserved` 和 actionflow 一样不随事务回滚（D20 P2 决定 3），心跳换档随事务提交生效（`host.go:296` `change`）。handler 不失败，两者就一致。

### 4.4 恢复（探测拍，实体锁内）

`onBeat`（`beat.go:133`）在 `unobserved` 时只做三件事：

1. 检查 `FreezeFor` 是否到期；
2. 读自己的观察者计数；
3. 计数大于 0 时退出暂停：清 `unobserved`，调策略的可选 `Resume` 钩子，然后在**本拍**立即 `Tick` 一次，最后 `syncBeat` 回到策略周期。

恢复后让策略**重新决策**，不续接旧计划。理由：

- 暂停可能持续很久，旧计划里的目标点、追击对象、冷却判断都可能过时；
- 暂停时已经结束了任务、清空了排队动作（4.5），旧计划本来也不完整；
- 重新决策和“刚出生”的路径相同，测试面最小。

新增可选接口：

```go
// ResumableStrategy 是在“无观察者暂停”结束时需要重置的策略（可选）。
type ResumableStrategy interface{ Resume(ctx *Context) }
```

`BehaviorStrategy` 实现它，做的事和换阶段时一样（`behavior_strategy.go` `applyStage`）：Reset 当前阶段的树（运行中的 `TaskflowAction` 叶子经 `OnInterrupt` 收尾），丢掉缓冲的事件和动作结束。不实现它的策略（例如 demo 的 `Patrol`）每拍本来就按当前世界状态决策，什么都不用做。

### 4.5 暂停时在途的动作与任务

actionflow 包说明里的“清场顺序”（`gameplay/actionflow/action_types.go:16`）：任务（`PlanMission`）会在动作结束时自己推进到下一步，所以只停 AI 不够，任务还会接着发起新动作。按要求“当前动作走完，不再发起新的”，缺省做法是：

- `ActionList.EndCurMission("ai unobserved")`：任务结束后送来的动作结束被忽略，任务不再推进；
- `ActionList.ClearNextActions()`：清掉排队的动作；
- 不调 `EndAllAction`：当前动作走完。

这一步取的是 `ControllerHooks.Context` 给出的 `ActionList`。业务可以用 `IdleConfig.OnPause func(*Context)` 替换缺省做法（见待拍板 Q3）。

### 4.6 启用方式（代码层，显式）

Controller 在实体 `OnInitFinish` 时构造，那时实体还没进场景，不知道该读哪个 Interest，所以启用不放在 `BeatConfig` 里，而是在进场景之后单独调用：

```go
// EnableIdle 打开“无观察者暂停”：之后由 scanner 判断暂停、由探测拍恢复。scanner 为 nil 或进程层没启用时什么都不做、
// 返回 nil（开销为零）。Remote 托管实体返回 ErrIdleRemote（见 7.1）。只能在实体锁内调用；换场景时 Controller 按现有
// 约定 Close 后重建，重建后重新 EnableIdle。
func (c *Controller) EnableIdle(cfg IdleConfig) error

type IdleConfig struct {
    Scanner *IdleScanner            // 进程里的扫描者（wiring Mod 装配），nil 不启用
    Source  ObserverSource          // 计数源：通常是实体所在场景的 *policy.Interest
    Subject int64                   // 计数源里这个实体的目标 ID（通常就是实体 ID）
    Probe   time.Duration           // 探测档，0 取 Scanner 的缺省（heartbeat.Periods 之一）
    OnPause func(*Context)          // 替换 4.5 的缺省做法；nil 用缺省
}

type ObserverSource interface {
    ObserverCount(subject int64) (int, error)
    ObserverCounts(subjects []int64, counts []int32) error
}
```

`Close`（`controller.go:286`）同时从扫描者注销。

## 5. 扫描者

### 5.1 放在哪一层

结构和心跳一样分两层：

- **`gameplay/ai`：`IdleScanner`**，负责登记表、扫描和广播 handler 的逻辑。它不依赖 wiring，可以用假的 `nest.Client` 和假的计数源做单测。对照 `gameplay/heartbeat.Driver`。
  - 登记表 `map[int64]*idleEntry`，带一把短锁。登记和注销在实体锁内调用，O(1)。
  - entry 里有：Controller 实例、计数源、目标 ID、连续读到 0 的轮数（只有扫描者 goroutine 读写）、`paused atomic.Bool`（Controller 在实体锁内写，扫描者读）、最近一次发出暂停请求的轮号（防重发）。
  - handler 名 `ai.idle.pause`，在 `init` 里用 `nest.MustRegisterHandlerWithMeta` 登记，`RollbackUndo` 加 `DurabilityMemory`，与心跳 handler 相同（`host.go:330`）。参数只有扫描者自己（全局一份），handler 按实体 ID 查登记表。
- **wiring：`wiring/aiidle` 的 Mod**（`mods.ModAIIdle = "ai_idle"`），负责进程层的开关和生命周期：
  - 依赖 Nest 和 heartbeat；
  - Provide 时把 Nest 客户端交给扫描者，并登记扫描者，业务经 `app.Lookup` 取，或用包级变量，同 heartbeat 的写法；
  - Start / Stop 启停 goroutine，Stop 先于 Nest 停；
  - 配置声明一次（A4① schema）：

```yaml
ai_idle:
  enabled: false        # 缺省关闭：关闭时扫描者不运行，EnableIdle 什么都不做
  scan_interval: 2s     # P_scan
  probe_period: 2s      # P_probe，必须是 heartbeat.Periods 之一
```

只用 ai 包、不装 Mod 的工程（测试、工具）可以自己 `NewIdleScanner` 并 `Bind` / `Start`。

### 5.2 一轮扫描

1. 拍一份登记表快照，按计数源分组，每组按 1024 个调一次 `ObserverCounts`。
2. 对每个 entry：
   - 计数大于 0：连续 0 轮数清零；
   - 计数为 0：轮数加 1。轮数达到 2（连续两轮都是 0，也就是无人观察至少持续 `P_scan`）、`paused` 为假、并且最近两轮没发过暂停请求，就进暂停名单。
3. 名单非空时调一次 `DispatchBroadcast`。Nest 按锁组和 worker 拆批，每批至多 32 个（`dispatcher.go:244`）。准入失败时这一轮的请求作废，下一轮会重新发现。
4. 记指标（第 8 节）。

恢复不经扫描者：扫描者只发暂停。`paused` 在 Controller 里由暂停 handler 置位、由探测拍清除，扫描者只读。

### 5.3 默认周期与理由

- **`P_scan = 2s`**：无人观察后 2～4s 内停下。
  - 观察者离开时 AI 多跑这几秒，和“停下来”节省的时间（通常几十秒到几分钟）相比可以忽略；
  - 连续两轮的条件起去抖作用：玩家在视野边缘进进出出时，2s 内的往返不会触发暂停；AOI 的进入 / 离开半径本身也有滞回；
  - 1 万个实体一轮约 0.5ms 读锁加一次广播，每 2s 一次可以忽略。
  - 1s 去抖太短，5s 让刚走开的玩家回头时更可能看到“还在动”，差别不大，取中间值。
- **`P_probe = 2s`**：恢复延迟最多约 `P_probe` + 一个同步周期（计数在结算后才变化）+ Nest 排队，平均约 1s。
  - 玩家以 5～7m/s 走近时，2s 走 10～14m。AOI 的进入半径通常远大于怪物的仇恨半径，所以怪物在进入仇恨范围之前就已经恢复。
  - 走到玩家视野边缘的怪物可能有最多 2s 站着不动，那时它在画面最远处，可以接受。
  - 1s 档让探测读数翻倍（仍可忽略），5s 档会让“冲到怪物面前”的情况看得出反应慢。
  - 恢复延迟是这个方案唯一能被玩家感知的代价，见待拍板 Q1。

## 6. 和执行契约的关系

- **Nest 快池不阻塞**：探测拍在快池上只取 `observedMu` 读锁（第 3.3 节），不取 `Interest.mu`。暂停 handler 同样如此。扫描者不在 Nest worker 上，它在自己的 goroutine 里调 `DispatchBroadcast`。`validateClientDispatch` 要求不在 handler 里调用（`client.go:33`），扫描者满足。
- **Sync 冻结 / 交付**：计数只在结算之后发布，不读未冻结的业务字段，不改发布屏障。
- **runner 不随事务回滚**（D20 P2 决定 3）：`unobserved` 和 actionflow 的变更都是 runner 侧状态，handler 和探测拍都不失败，所以不出现“状态改了、事务回滚”的情况。
- **Interest 执行器**：不加回调、不改切点 / 预算 / 日志，只在结算侧记账（第 3.2 节）。

## 7. 边界

### 7.1 Remote

- Nest 广播不支持 Remote 托管实体（`nest_dispatch.go:682` `ErrRemoteBroadcastUnsupported`）。按 D20 / DESIGN-TODO 第 6 条的方向，Remote 托管实体本来就不该挂移动或心跳组件。所以 `EnableIdle` 对 Remote 托管实体返回 `ErrIdleRemote`，不登记，这类实体的 AI 照常运行。
- 如果以后要支持，可以对这类实体改用单目标 `Dispatch`，但要先解决共享模式下心跳不能挂载的问题（`host.go` `runBeat` 的 `remoteShared`）。本期不做。
- **跨进程观察者**：计数源是实体所在进程里那个场景的 Interest。观察者的会话经独立 Gate 接入不影响计数，因为 Interest 和 Manager 都在场景进程。实体和它的场景 Interest 不在同一进程的部署本方案不支持，写进 KNOWN-LIMITS。

### 7.2 实体卸载与重载

- 卸载：组件 `OnDestroy` 里 `Controller.Close` 注销。还在队列里的暂停广播到达时，实体如果已不在内存，Nest 跳过它：广播不为冷目标加载（`io_load.go:132`），缺失的目标会被跳过（`TestABroadcastSkipsMissingEntitiesAndKeepsGoing`）。如果已有同 ID 新实例，实例身份核对会把它作为 `stale` 丢弃。
- 重载：新实例重新 `EnableIdle`，从运行状态开始。之后由扫描者在两轮之内重新判断。
- 新出生的实体同样从运行状态开始，不从暂停开始：避免出生和玩家进视野的先后顺序造成“出生即睡、要等一个探测周期才醒”。代价是没人看的出生点要多跑 2～4s。

### 7.3 场景时间域

- 扫描周期、探测周期、去抖都是**真实时间**：心跳调度本来就是真实时间，时间域只改 `Beat.Now` / `Elapsed`（D22）。
- `FreezeFor` 的期限仍是域时间（`Controller.now` 取域时间），在探测拍里按域时间判断，规则不变。
- 时停（倍率 0）的场景里 AI 本来就不推进。暂停和时停互不影响：探测拍照样读计数。

### 7.4 两种心跳投递方式

- 探测拍就是普通的 `"ai"` 心跳拍，`batch` 和 `dispatch` 两种投递方式（`wiring/heartbeat` 的 `heartbeat.delivery`）对它没有区别。
- 暂停广播是独立的 Nest 广播，不经心跳驱动。它和同一实体的批量心跳作业之间没有顺序要求：两者都在实体锁内执行，handler 再读一次计数，探测拍按当时的计数决定，任意交错的结果都对。
- 端到端测试两种投递方式各跑一遍（第 9 节）。

### 7.5 场景切换

成员换场景时按现有约定 `Controller.Close` 再重建（`controller.go:286` 的注释），重建后对新场景的 Interest `EnableIdle`。旧登记随 Close 注销，不会读旧场景的计数。

## 8. 指标

都在 `gameplay/ai`，标签 `scanner`：

| 指标 | 类型 | 含义 |
| --- | --- | --- |
| `ai.idle.watched` | Gauge | 已启用“无观察者暂停”的 AI 数 |
| `ai.idle.paused` | Gauge | 因无观察者暂停中的 AI 数（暂停 +1，恢复 / Close -1） |
| `ai.idle.scan_seconds` | Histogram | 每轮扫描耗时（读计数 + 广播准入） |
| `ai.idle.broadcast_total` | Counter | 暂停广播调用次数；标签 `result`：`ok` / `rejected` |
| `ai.idle.pause_requested_total` | Counter | 广播里的目标数 |
| `ai.idle.paused_total` | Counter | handler 实际暂停的次数 |
| `ai.idle.pause_skipped_total` | Counter | handler 没暂停的次数；标签 `reason`：`observed`（再读时已有人看）/ `stale`（实例已换或已 Close）/ `remote` |
| `ai.idle.resumed_total` | Counter | 探测拍恢复的次数 |

判读：

- `paused_total` 和 `resumed_total` 都很高，而且 `pause_skipped_total{reason="observed"}` 也高，说明视野边缘的抖动比预期大，可以调大 `scan_interval`；
- `paused / watched` 就是省下的比例。

`docs/framework/OBSERVABILITY.md` 实施时新增“AI 无观察者暂停（gameplay/ai，可选）”一节。

## 9. 测试计划

先红后绿，全部用确定性用例。断言提交后的状态时用 settle 写法：轮询条件直到满足，带超时，失败时打印当前状态，不用 sleep 等待。

| # | 层 | 用例 | 变异核对（每条都要让对应用例变红） |
| --- | --- | --- | --- |
| T1 | policy | 计数：Enter / Leave / Hide / 关系视野 / 块覆盖 / 被拒订阅仍计 / self 不计 / Close 清零；`TrackObservers` 关闭时返回 `ErrObserversNotTracked`、且不建表 | 去掉 self 排除；删 pair 时不减；计数按 subscribed 而不是 held |
| T2 | policy | 读不等 AOI 批次：用 `BandLayer` 钉住政策阶段（同 `TestDirectCallsDoNotWaitForTheAOIBatch`），`ObserverCount` 仍立即返回；`-race -count=10` | 读改取 `mu` |
| T3 | ai | 原因叠加表：`Freeze` × 暂停 × `FreezeFor` × `Recover` 的组合下，`ready()`、心跳档位、到期恢复都对；暂停期间结束通知照常送达 | `ready()` 不看 `unobserved`；`Recover` 清掉 `unobserved` |
| T4 | ai | 暂停走完当前动作：任务被结束、排队动作清空、当前动作继续到结束；恢复调 `Resume` 并在本拍 Tick | 暂停时调 `EndAllAction`；恢复不在本拍 Tick |
| T5 | ai | 扫描者（假 client + 假计数源）：连续两轮 0 才发；序列 0,1,0,1… 永不发；丢失的请求两轮后重发；已暂停的不重发；1024 分组 | 只要一轮 0 就发；不防重发 |
| T6 | ai | handler：实例已换 → `stale`；再读计数大于 0 → `observed`；Close 后注销 | 去掉 handler 里的再读 |
| T7 | 端到端 | 真实 Interest（两种 ApplyMode）+ 真实 Nest + 心跳驱动（`dispatch` / `batch` 两种投递）+ 扫描者：玩家离开 → 两轮内 `"ai"` 换到探测档、策略 Tick 次数不再增长；玩家进入 → 一个探测周期内恢复、Tick 增长；视野边缘抖动不切换；业务 `Freeze` 期间玩家来去，`Recover` 后状态正确；卸载时广播在途 | 去掉探测拍的计数检查（永不恢复） |
| T8 | 生成 / demo | `scripts/test-heartbeat-generated.sh`（生成工程缺省不启用、注释里有打开方法）；game-demo 生成后 build / vet / `go test -race ./...`；demo 开启示范时的场景用例 | — |

验证线：gofmt、vet、glsvet（sync / gameplay）、`-race ./framework/sync/... ./gameplay/... ./wiring/...`、根包、`scripts/test-sync-modes-generated.sh`、全量 `GOWORK=off go test -count=1 ./...`。

压测项（实施后再加进压测清单，现在不加）：在 sync-aoi 或 nest-game 心跳驱动下，用“1 万怪物、200 玩家聚在一角”的场景，对比 `ai_idle.enabled` 开 / 关时的进程 CPU、`heartbeat.executed_total`、`ai.idle.paused`、业务 p99。

## 10. 工作量（估计）

| 期 | 内容 | 估计 |
| --- | --- | --- |
| 1 | policy：`TrackObservers`、计数维护与发布、两个查询；T1、T2 | 0.5 天 |
| 2 | ai：`unobserved` 原因位、探测档、暂停 / 恢复、`ResumableStrategy`（含 `BehaviorStrategy`）、`EnableIdle`；T3、T4 | 0.5～1 天 |
| 3 | ai：`IdleScanner`、handler、指标；wiring：`wiring/aiidle` Mod、配置 schema、configschemagen；T5、T6、T7 | 1 天 |
| 4 | 生成器注释、game-demo 接线（按 Q4）、文档（guide / impl ai、10-time、04-sync，OBSERVABILITY，CHANGELOG，KNOWN-LIMITS）；T8 与全量验证 | 0.5～1 天 |
| 合计 | | 约 2.5～3.5 天，超过“一天内稳妥完成”，所以按维护者指示留到优化阶段 |

## 11. 待拍板（附推荐）

1. **Q1 恢复延迟**（`P_probe` 缺省值）：
   - A：2s 档，平均约 1s 恢复（推荐，理由见第 5.3 节）；
   - B：1s 档，探测读数翻倍，仍可忽略；
   - C：探测拍之外，再给“计数从 0 变成大于 0”加一条边沿通知。延迟降到一个同步周期，但要在 Interest 结算侧加回调，和“只读快照”原则冲突。不推荐。
2. **Q2 去抖**（`P_scan` 和轮数）：
   - A：2s × 连续两轮（推荐）；
   - B：1s × 连续三轮，停得更快，抖动处理更细，扫描频率翻倍；
   - C：5s × 两轮。
3. **Q3 暂停时的在途动作与任务**：
   - A：结束当前任务、清空排队动作，当前动作走完，并可用 `OnPause` 替换（推荐，符合“当前动作走完、不再发起新的”）；
   - B：只停决策，不碰任务，任务可能接着发起动作，不符合要求；
   - C：连当前动作一起结束（`EndAllAction`）。停得最快，但怪物会停在半路，客户端可能看到瞬停。
4. **Q4 game-demo 是否开启作为示范**：
   - A：demo 装 `wiring/aiidle` Mod，配置里 `enabled: true`，怪物 `Spawn` 后调 `EnableIdle`，场景开 `TrackObservers`（推荐）。demo 是参考工程，功能只有在真实链路里跑着才算示范到；现有“没有玩家时验证巡逻”的 demo 用例改为在测试配置里关闭，或放一个观察者。
   - B：demo 只接线、配置关闭，文档写怎么打开，示范价值低。
5. **Q5 计数口径**：
   - A：按 held、含关系视野和块覆盖、排除 self（推荐，第 3.1 节）；
   - B：只算空间视野（距离）。关系视野里的单位（例如队友召唤物）没人在附近时也会暂停，但队友在地图另一端也能看到它，会看到它不动。
6. **Q6 新生实体的初始状态**：
   - A：从运行开始，两轮内由扫描者决定（推荐，第 7.2 节）；
   - B：出生即暂停，等探测拍恢复，省掉出生后的 2～4s，但出生在玩家面前的怪物会有最多一个探测周期不动。

## 12. 未采用的做法

- **在 Interest 结算时按边沿回调 + 投递 Nest 消息**（本任务最初的设想）：要在结算路径上加回调，触碰执行器所在的锁和时机；边沿消息的投递失败、乱序和去抖都要另外处理。维护者给出扫描 + 心跳的做法后不再采用。
- **暂停也由实体自己的 AI 心跳判断**（每拍读计数，连续 N 秒为 0 就自己暂停，不要扫描者和广播）：代码更少。但 `TickInterval` 为 0、只靠动作结束驱动的策略没有心跳，判断不到，而且没有集中的扫描指标。维护者指定扫描者做法，记在这里备查。
