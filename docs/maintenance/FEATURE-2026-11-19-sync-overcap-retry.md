# 方案：超过每 subject 订阅者上限后的重试（Sync，2026-11-19）

**状态：方案稿，未实施，等维护者确认 §7 的决定项。** 维护者 2026-11-19 选定 M-9 处置的 B（“改重试机制”），本文是 B 的方案。Sync 是 core 三大块之一，确认前不改代码。

- 基线：main `ba158535`。行号均为该提交的源码。codebase-memory-mcp 本次连不上（connection closed），调用链靠直接读源码与 `rg` 整理，没有用图谱。
- 证据来源：[M-9 测量与“疑似框架问题”](../performance/benchmarks/m-series-s1-s2-20261119/README.md)、[KNOWN-LIMITS“超过每 subject 订阅者上限时被拒 pair 每轮全量重说”](KNOWN-LIMITS.md)（第 915 行，标“尚未确认”）。
- 本次写方案时另外用一个临时测试（未提交，跑完已删）确认了两个相关缺陷，见 §1.4。

## 0. 结论速览

| 项 | 内容 |
| --- | --- |
| 问题 | 被拒 pair 留在一张不分原因的重试表里，**每次结算全量重说**；超上限时被拒 pair 数是 O(N²)，2000 人联盟每轮 Flush 约 960 ms（上限内 10.8 ms），每轮分配 600 MB，收敛期每个 pair 打一条 WARN |
| 推荐做法 | **C：按原因分开等待。** 超上限被拒的 pair 按 subject 排队，只在该 subject 有空位时按先来后到重说；其他原因的拒绝仍走重试表，但去重、每次结算限量。WARN 按周期汇总，指标给出等待数 |
| 顺带修的缺陷 | F1 重试中的 pair 被视野再次改动后，每次结算重说两遍，并在表里永久留两份；F2 玩家看自己的钉住订阅也会被上限拒绝，玩家看不到自己 |
| 时延口径 | 空位出现后，等待的 pair 在**下一次政策阶段**订阅上：remove 在第 k 次 Flush 交付，在第 k+1 次 Flush 订阅上。周期模式下不超过 1 个 `Interval`（缺省 50 ms）；on_change 模式下有兜底 ticker，同样不超过 1 个 `Interval` |
| 决定项 | D1～D8，见 §7，每项都给了推荐 |
| 分批 | 批 1 基准（测试代码）→ 批 2 Manager + Interest → 批 3 GroupView → 批 4 测量、文档、全量；合计约 1.5～2 个工作日，见 §6 |

## 1. 现状

### 1.1 调用链

| 步骤 | 位置 | 行为 |
| --- | --- | --- |
| 拒绝 | `framework/sync/entitysync/subscriptions.go:439-440`（`Manager.subscribe`，386 起） | 只拦“该会话在这个 subject 上还没有订阅项”的情况：`subj.subscribers[session] == nil && len(subj.subscribers) >= MaxSubscribersPerSubject` 时返回 `ErrSubscriberLimit`。同一会话已经被别的来源订阅时（413-437）不受上限限制。上限检查在钉住处理之前，**钉住的订阅也会被拒**。缺省上限 `DefaultMaxSubscribersPerSubject = 1024`（`manager.go:92`） |
| 占位的口径 | 同上 439 | `len(subj.subscribers)` 包含 `kindLeaving`（欠 remove）的项：退订后要等 remove 帧交付，空位才出现 |
| 空位出现 | `subscriptions.go:554` `removeSubscriptionLocked` | 调用点：无在途时直接退订（519）、remove 帧准入后结算（`flush.go:704` `settleSubscriptions`）、会话关闭（`subscriptions.go:248`）、会话重置时的 leaving 项（146）、subject 退役（`manager.go:645`）、准备阶段发现过期 lifetime（`flush.go:478`）、Manager 关闭（`manager.go:982`） |
| 记录拒绝 | `policy/interest.go:982-1002` `Interest.subscribe` | 先把 pair 记成已订阅；被拒就改回未订阅，追加到 `in.retry`（231 行），调 `noteRefusedLocked`，再追加一条 `Refusal` |
| 计数与告警 | `policy/interest_queue.go:691-709` `noteRefusedLocked` | 每个 pair 一条 `refused` 记录（`interest.go:247`）。连续第 10 次被拒（`retryStallAfter`，681 行）时计 `entitysync_interest_retry_stalled_total`，**每个 pair 打一条 WARN**（706 行） |
| 重说 | `policy/interest.go:873-918` `applyLocked` | 结算开头整张取走 `retry`；先结算本轮变过的 pair（`settlePair`，943 行），再把取走的每个 pair 重新 `subscribe`。**没有退避，也不区分原因** |
| 谁触发结算 | `policy/interest_queue.go:576-606` `settleQueued` | Manager Flush 的政策阶段（`policy_queue.go:56` `applyPolicies`）调用；政策阶段的拒绝逐条交给 `InterestConfig.OnRefusal` |
| 何时有 Flush | `manager.go:850-885` `Manager.run` | 每个 `Interval` 一次兜底 ticker（周期与 on_change 都有）；on_change 下 `WakeSync`（`mode.go:49`）另外唤醒 |
| Drain | `policy/interest_queue.go:651-670` `queuedPending`；718-763 `drainStalls` | 重试表里拒绝不到 10 次的 pair 让 Drain 等待；到 10 次后不再阻挡（注释 672-681） |
| GroupView | `policy/group_view.go:343-425`（`apply` / `align`），472-485（`refused`） | 有成员没订阅上的组留在 `waiting` 里，每个政策阶段把所有没订阅上的成员重新 `SubscribeStamped` 一遍；日志每个成员每种原因记一次，超上限记 WARN。成本按组成员数线性增长，不是 O(N²) |

### 1.2 必须保持的不变式

| 编号 | 不变式 | 出处 |
| --- | --- | --- |
| I1 | 拒绝不是对 pair 的裁决：只要 pair 还被持有，就要继续争取订阅；容量释放、会话重开、subject 重新登记之后自动订阅上，业务不用再调用 | `interest_queue.go:674-680`；`Refusal` 注释 `interest.go:143-147`；[ENTITY_SYNC](../framework/ENTITY_SYNC.md) 第 95 行 |
| I2 | Interest 单写者：`held` / `retry` / `refused` 是应用侧状态，只在 `in.mu` 下由应用方读写，登记侧不碰；锁序 `mu → registry.mu`；对 Manager 的调用本来就在 `in.mu` 下 | `interest.go:165-169`、194 |
| I3 | 一次结算里每个 pair 只说一次（“Retries after the events…”） | `interest.go:904-905` 注释 |
| I4 | 永远不会被接受的 pair 不能让 Drain 永不结束 | `interest_queue.go:674-677` |
| I5 | 钉住的订阅（玩家看自己）永不暂缓 | `over_limit.go:26`、`subscriptions.go:416-422` |
| I6 | 退订后 remove 交付前，客户端仍持有该对象，所以 leaving 项仍占位 | `subscriptions.go:518-523` |

### 1.3 已有测试

| 用例 | 位置 | 覆盖 |
| --- | --- | --- |
| `TestARefusedSubscribeIsSaidAgainUntilItIsTaken` | `policy/policy_test.go:246` | subject 未登记 → 每次 Apply 重说（第二次带 `Retry`）→ 登记后订阅上 |
| `TestARefusedSubscribeForAReleasedPairIsDropped` | `policy/policy_test.go:279` | 拒绝前已释放的 pair 不再重说 |
| `TestQueuedRetryThatIsNeverAcceptedDoesNotHoldDrainOpen` | `policy/interest_test.go:547` | 会话已关 → 停滞计数一次，Drain 不被卡住，会话重开后订阅上 |
| `TestDrainTimeoutListsInterestStalls` | `policy/interest_test.go:925` | Drain 超时诊断里有 `subscribe_refused` |
| `TestPolicyPhaseRefusalsReachOnRefusal` | `policy/interest_apply_test.go:319` | 政策阶段的拒绝交给 `OnRefusal`，第二次带 `Retry` |
| `TestGroupViewRefusesBeyondTheMemberLimit` | `policy/group_view_test.go:210` | GroupView 自己的 `MaxMembers`（不是 Manager 的上限） |

**`ErrSubscriberLimit` 与 `MaxSubscribersPerSubject` 在整个仓库没有任何测试**（`rg` 只命中 `errors.go`、`manager.go`、`subscriptions.go` 与 `group_view.go` 的一行注释）。

### 1.4 本次确认的两个缺陷

临时测试放在 `framework/sync/entitysync/policy/` 下跑，没有提交，跑完已删。

**F1 重试中的 pair 被再次改动后重说两遍，并在表里永久留两份（已确认）。** 玩家 1 经距离看见未登记的实体 7（被拒），再让第二个视野也持有 (1, 7)：

```
apply1 refusals=1 retry=1
apply2 refusals=1 retry=1
apply3 (touched) refusals=2 retry=2
apply4 refusals=2 retry=2
```

根因：`applyLocked` 先在 `settlePair`（`interest.go:958`，`!current.subscribed` 分支）里重说本轮变过的 pair，被拒后又追加进 `retry`；随后循环取走的旧 `retry` 时（906-915）只看 `hold.subscribed`，同一个 pair 再说一遍、再追加一份。之后每轮都有两份，每再改动一次多一份。违反 I3。在空间视野里，玩家走动会不断改动 pair（换档、换层），重复项会随之增长。

**F2 玩家看自己的钉住订阅被上限拒绝（已确认）。** `MaxSubscribersPerSubject = 2`，4 个玩家互相看见（含看自己）：

```
flush 0 retry=8 subs(1)=[1 2] subs(4)=[1 2]
```

玩家 4 看不到自己的实体。根因：`subscriptions.go:439` 的上限检查在钉住处理之前；`applyLocked` 按（观察者，subject）升序结算（`interest.go:892-897`），所以第一轮里观察者 ID 小的先占位。SelfView 经 Direct 用 `SubscribePinned`（`direct.go:81`）订阅自己的实体，同样会被拒。违反 I5。

**F3 空位的分配顺序（现象，不算缺陷，交 D3 决定）。** 第一轮按观察者 ID 分配，之后谁先被重说谁拿到空位；本轮新出现的 pair 先于排队已久的 pair 结算（`settlePair` 在重试之前），所以新来的会插队。

### 1.5 成本（M-9 实测）

| 档 | 每轮耗时 | 每轮分配 | 被拒 pair |
| --- | --- | --- | --- |
| 2000 人，上限 2016 | 10.76～11.00 ms | 2.06 MB / 12,720 次 | 0 |
| 2000 人，缺省上限 1024 | 949～966 ms | 600 MB / 201,904 次 | 1,952,000（每轮全部重说一遍） |

按差值摊算，每个被拒 pair 每轮约 0.49 µs、约 300 B 分配（每个 pair 都要锁一次 subject、追加 `retry`、写 `refused`、追加 `Refusal`、调一次 `OnRefusal`）。常驻内存方面，每个被拒 pair 额外有一条 `retry` 与一条 `refused` 记录，约 100 B 以上，195 万个合计约 200 MB。

### 1.6 方向判断（同一机制第三次出问题）

| 序 | 问题 | 处置 |
| --- | --- | --- |
| 1 | 永远不会被接受的 pair 让 Drain 永不结束 | 加停滞上界 `retryStallAfter`，到上界后不再阻挡 Drain（`interest_queue.go:672-681`） |
| 2 | 超上限时全量重说的成本，以及每个 pair 一条 WARN | 本方案 |
| 3 | F1 重复项 | 本方案 |

前提问题在于：重试表是一张**不分原因的 pair 列表**，“每次结算全部重说”被当成了唯一的唤醒方式。可是各种拒绝等的事件不同：超上限等的是这个 subject 出现空位；会话未开等的是会话打开；subject 未登记等的是重新登记。把等待按原因分开，“何时重说”就由对应的事件决定，成本也就跟着事件走。本方案只把量级最大、能由政策自己查到事件的“超上限”拆出来；其他原因仍按原方式重说，但加上去重和限量（D5）。

## 2. 目标

| 编号 | 目标 | 验收口径 |
| --- | --- | --- |
| G1 | 被拒 pair 不再每轮全量重说 | 稳态（没有空位）时，每次结算的额外成本只和**等待中的 subject 数**相关（每个 subject 查一次空位），和被拒 pair 数无关；不再产生 `Refusal`。2000 人缺省上限档的每轮耗时不高于上限 2016 档（10.8 ms 量级），每轮分配不超过它的 2 倍 |
| G2 | 容量释放后仍自动订阅 | 空位出现后，按排队顺序在下一次政策阶段订阅上：remove 在第 k 次 Flush 交付，第 k+1 次 Flush 订阅上。时间上不超过 1 个 `Interval`（缺省 50 ms），on_change 与周期模式相同 |
| G3 | WARN 聚合、指标可见 | 每个 Interest 每 10 s 至多一条汇总 WARN，带等待 pair 数、subject 数、新增数与等待最多的几个 subject；指标给出等待的 pair 数与 subject 数，Manager 侧给出被上限拒绝的累计次数 |
| G4 | 不变式 | I1～I6 全部保持；修掉 F1、F2 |

## 3. 可选做法

| 做法 | 要点 | 正确性 | 内存 | Flush 成本（稳态超上限） | 复杂度 |
| --- | --- | --- | --- | --- | --- |
| A 按 subject 等空位 | 超上限被拒的 pair 进入该 subject 的等待队列；每次结算对每个等待的 subject 查一次空位（新增只读 `Manager.SubscriberRoom`），有空位才按队列顺序重说至多“空位数”个 | I1 成立；时延为 1 个政策阶段。漏唤醒的边角见 §4.6 | 每个等待 pair 只占队列里的一项（约 16～40 B），去掉每 pair 的 `refused` 记录，比现在少 | O(等待 subject 数) 次加锁读，2000 个 subject 约 0.1 ms | 中：政策侧新增一个按 subject 的队列；Manager 只加一个只读方法 |
| A′ 改为事件通知 | 同 A，但空位不靠查询：Manager 在 `removeSubscriptionLocked` 里对“有来源在等”的 subject 登记通知，政策阶段像 `Deferred` 一样交付（新钩子 `RoomFreed`） | 同 A | Manager 每个 subject 多一组等待来源，外加通知队列 | 空闲时为 0 | 高：Manager 要多一份状态，还要处理退役、忘掉 subject、Close、来源关闭时的清理与锁序 |
| B 每 pair 指数退避 | 被拒后按 1、2、4…个结算周期递增，封顶 T（如 1 s 或 5 s），用按到期时间分桶的队列 | I1 成立，但空位出现后最长要等 T 才订阅上 | 每 pair 一个到期时间 + 桶，比 A 多 | 195 万 pair、T = 1 s 时每轮仍有约 10 万次重说，约 50 ms，降不到 G1 | 中 |
| C（推荐）A + 其他原因去重限量 + 汇总 | 超上限走 A；其他原因仍每轮重说，但修 F1（去重）并给每次结算设上限 K；钉住订阅不受上限（F2）；WARN 汇总 | 同 A；其他原因在等待数不超过 K 时行为与现在完全相同 | 同 A | 同 A | 中：政策侧约 200 行，Manager 约 40 行 |
| D Manager 接受并挂起 | 超上限不报错，像超额暂缓（`over_limit.go`）一样把订阅挂在 Manager 里，有空位时提升 | 政策不再看到拒绝 | 被拒 pair 在 Manager 里各有一份完整的 `subscription`，195 万个约数百 MB，上限保护内存的作用也没了 | 低 | 很高：订阅索引、快照请求、Drain、退役、Stats 都要跳过“挂起”项；还要改变 `MaxSubscribersPerSubject` 的语义 |

**推荐 C，空位探测用查询（A），不用事件（A′）。** 理由：
- 查询只多一个只读方法，状态全在政策侧，没有新的跨层回调，也没有清理时序要处理（三块局部聚集）；
- 稳态成本是 O(等待 subject 数)，2000 个 subject 约 0.1 ms，相对 10 ms 的 Flush 可以忽略；
- 时延与事件方案相同（都在下一次政策阶段）。

B 降不到目标，而且会拉长时延；D 改的是 Manager 的语义和内存边界，代价最大。

## 4. 推荐方案细节

### 4.1 Manager（`framework/sync/entitysync`）

| 改动 | 位置 | 说明 |
| --- | --- | --- |
| 钉住的订阅不受上限（F2，D2） | `subscriptions.go:439` | 条件加上 `!pinned`。实际超出上限的量等于钉住订阅的个数：现有钉住来源只有 Interest 的 self 与 SelfView（经 Direct），都是“会话看自己的实体”，所以每个 subject 至多超出 1 |
| `func (m *Manager) SubscriberRoom(subjectID int64) int` | `subscriptions.go`，放在 `Subscribers` 旁边 | 返回 `max(0, MaxSubscribersPerSubject - len(subj.subscribers))`；subject 未登记时返回 0（重新登记后是新 subject，空位为上限，下一次查询就能看到）。只取 `subj.mu`，与 `Subscribers` 相同 |
| 被上限拒绝的计数 | 439 行与 `ManagerStats` | 新增原子计数 `subscriberLimitRefused`，导出 `ManagerStats.SubscriberLimitRefused` 与指标 `entitysync_subscriber_limit_refused_total`。改动后重说次数有界，可以每次拒绝都计 |

### 4.2 Interest（`framework/sync/entitysync/policy`）

状态都在应用侧、受 `in.mu` 保护（I2）。

| 状态 | 说明 |
| --- | --- |
| `hold.wait`（新增字段，取值为 none / retry / room） | pair 正在哪种等待里。用来去重（F1）：只有从 none 变成等待时才入队；结算时 `settlePair` 遇到正在等待的 pair 不直接说，交给对应队列 |
| `retry []pair` | 只放**非上限原因**的拒绝。每次结算最多重说 K 个（D5，K = 4096，常量，不做配置），没轮到的按原顺序留到下次。显式 `Apply()` 不受 K 限制（测试与工具要一次排空） |
| `roomWait map[int64]*roomQueue` | subject → 等空位的观察者。`roomQueue` 是“观察者 → 入队序号”的 map，再加最早入队时间与本周期新增数。pair 被释放时用 O(1) 删除；有空位时取序号最小的前“空位数”个（先来先得，D3） |
| `refused map[pair]refusal` | 只给非上限原因用（停滞计数与 Drain 诊断），与现在一样 |

**一次结算（`applyLocked`）的顺序：**

| 步 | 动作 |
| --- | --- |
| 1 | 取走 `retry` 的前 K 个（显式 Apply 取全部） |
| 2 | 结算本轮变过的 pair（`settlePair`，与现在相同）。其中未订阅且 `wait != none` 的 pair 不说；未订阅、没有在等、但它的 subject 在 `roomWait` 里有人排队的，直接入队、不说（新来的不插队，F3） |
| 3 | 非上限重试：重说第 1 步取走的 pair（`hold` 已没了或已订阅的跳过，与现在相同） |
| 4 | 等空位：按 subject 升序（结果确定）逐个查 `SubscriberRoom`。空位为 0 的跳过；空位为 r 时，按序号取前 r 个重说。重说时再遇到 `ErrSubscriberLimit`（空位被别的来源抢了），就放回队列、停止处理这个 subject；成功的出队；遇到其他错误的转入 `retry` |
| 5 | 释放发布屏障（与现在相同） |

**`subscribe` 的改动：**
- 被拒且错误是 `ErrSubscriberLimit`：进入 `roomWait`，设 `wait = room`，不写 `refused`。只在**第一次**进入时产生一条 `Refusal`（`Retry = false`），之后排队期间不再产生；第 4 步重说又被拒时，产生 `Retry = true`。
- 被拒且是其他错误：进入 `retry`（`wait` 原来是 none 时才追加），其余与现在相同。

**释放：** `settlePair` 删除 `held` 时，`wait == room` 就从 `roomWait[subject]` 删掉该观察者（O(1)），队列空了就删掉这个 subject。`Close` 清空 `roomWait`。

**其他入口：**
- `resubscribeLocked`（`interest.go:783`）在 `wait == none` 时才追加到 `retry`。
- `resubmit`（`interest.go:798`）走 `subscribe`，自然按原因分流。

### 4.3 Drain 与诊断

| 项 | 规则 |
| --- | --- |
| `queuedPending` | 等空位的 pair 只在它的 subject **此刻有空位**时阻挡 Drain：下一次政策阶段就会处理它们，Drain 应当等到这一步。空位为 0 时不阻挡，与超额暂缓的订阅“不让 Drain 等待”一致（`over_limit.go:32`），也满足 I4。非上限原因维持“拒绝不到 10 次的才阻挡”。代价：每次 Drain 检查对每个等待的 subject 查一次空位，只在 Drain 时发生 |
| `drainStalls` | 增加 `subscriber_room` 一项：列出有空位但还没处理的 subject（数量与最早入队时长），非上限原因仍是 `subscribe_refused` |

### 4.4 WARN 与指标

| 项 | 规则 |
| --- | --- |
| 汇总 WARN | 每个 Interest 每 10 s 至多一条（与 `overLimitWarnInterval` 同值，在 policy 包里定义自己的常量）：`subscriptions wait for subscriber room`，带等待 pair 数、subject 数、本周期新增数、等待最多的 3 个 subject 及其人数、Manager 的上限。只在有等待时输出。非上限原因的“停滞”告警也改成同样按周期汇总（每周期一条，带停滞 pair 数与前 3 个样本）；`entitysync_interest_retry_stalled_total` 仍按 pair 计数（现有测试依赖） |
| 新指标 | `entitysync_interest_room_waiting`（Gauge，等空位的 pair 数）、`entitysync_interest_room_waiting_subjects`（Gauge）、`entitysync_interest_room_refused_total`（Counter，pair 进入等空位的次数，每次等待只计一次）；Manager 侧的 `entitysync_subscriber_limit_refused_total` 见 §4.1 |
| 文档 | [OBSERVABILITY](../framework/OBSERVABILITY.md) 的指标表与来源表 |

### 4.5 与其他机制的交互

| 机制 | 交互与结论 |
| --- | --- |
| Interest 单写者 | 新状态都在应用侧（`in.mu`），登记侧（`registry.mu`）不变。`SubscriberRoom` 在 `in.mu` 下调用，与现在在 `in.mu` 下调用 `Subscribe` 的锁序相同（`in.mu → subj.mu`），不引入新的锁序。同步与异步模式的结算都只在 `applyLocked` 里，两种模式行为相同 |
| AOI（空间视野） | 人群走动不断产生和释放 pair。释放时从队列 O(1) 删除；新出现的 pair 在 subject 有排队时直接入队。距离档或层级变化（reband）只影响已订阅的 pair；等待中的 pair 在真正重说时才算层级（`selectedProfile`），不需要额外处理 |
| Interest 超额降级（`interest_over_limit.go`） | 会话这一类已满导致的暂缓，订阅项已在 Manager 里，不会走到上限检查，两者互不影响。不过暂缓的订阅项也占 subject 的位置；Interest 收紧视野、退订最远的对象之后会空出位置，就会被第 4 步用上 |
| RelationView | 被拒最多的就是它（联盟、公会互见，O(N²)）。成员变化只是触发 `contribute` / `settlePair`，规则与上面相同 |
| GroupView | 规模是线性的，影响小，但做法相同：`align` 里，成员上次失败是 `ErrSubscriberLimit` 且 `SubscriberRoom == 0` 时跳过，不再调 Manager；日志改成每组每周期汇总。放在批 3（D6） |
| SelfView / Direct 钉住 | 由 F2 的修复解决（钉住不受上限） |
| TypeSchedules | 只决定**内容捕获**的时机（`type_schedule.go:13`），不影响订阅准入。remove 交付是否受周期类型的节奏影响，代码上看不出来（`settled` 注释说 snapshot-only 不走节奏），**尚未确认**。批 2 补一条“周期类型 subject 的空位时延”用例来确认；如果 remove 受节奏影响，G2 的口径要写成“remove 交付之后的下一次政策阶段”，并在文档里说明 |
| on_change 模式 | 空位在第 k 次 Flush 的推送结算里出现；第 k+1 次 Flush 由兜底 ticker（每个 `Interval`）或其他唤醒触发。可选优化：第 4 步发现“有空位但这轮没取完”时调用 `in.wake`。不需要 Manager 在 `removeSubscriptionLocked` 里额外唤醒 |
| 线格式 / 存储 | 不变。没有协议或存储格式改动，不涉及兼容 |

### 4.6 已知的边角（D4）

pair (S, X) 因上限在排队，之后会话 S 经**另一个非钉住来源**订阅上了 X（这只可能发生在 X 出现过空位、又被那个来源拿走的情况下）。这时 Interest 的这个 pair 其实已经可以加入（同会话已有订阅项，不受上限），但 X 的空位可能是 0，要等 X 再出现空位时才会被重说。按现有接线，能与 Interest 重叠在同一 (会话, 玩家实体) 上的来源只有 SelfView 和 Interest 的 self，二者都是钉住的，F2 修好后不排队。所以这个边角目前没有实际触发路径。推荐不为它加兜底轮询，把它写进 KNOWN-LIMITS 的框架限制；以后出现新的重叠来源时再加（例如每次结算以小预算轮转重说）。

## 5. 验收

### 5.1 先红后绿用例

放在现有文件里，按主题命名，不带编号（maintenance §7）。“红”指在 `ba158535` 上断言本身失败，失败文本在实施时原样抄进记录。

| 用例（暂名） | 文件 | 断言 | 修前 |
| --- | --- | --- | --- |
| `TestOverCapPairsWaitForRoomInsteadOfBeingSaidEverySettle` | `policy/interest_test.go` | 上限 2，一个 subject 被 6 个观察者经关系视野持有。第一次 Flush 报 4 条首次拒绝；之后 5 次 Flush `OnRefusal` 收到 0 条，Manager 的上限拒绝计数不再增加 | 红：每次 Flush 4 条 `Retry` |
| `TestOverCapPairSubscribesInTheSettleAfterRoomIsFreed` | 同上 | 订阅上的观察者解除挂接后：remove 交付那次 Flush 之后，下一次 Flush 时排在最前的等待者已订阅；周期与 on_change 各跑一遍（on_change 用 `Start`，在 2 × `Interval` 内轮询） | 守卫：修前已经通过（每轮都重说）。由变异 M2 证明它能变红 |
| `TestANewPairDoesNotJumpTheRoomQueue` | 同上 | 释放一个位置的同一次结算里，新挂上一个 ID 更小的观察者 E：空位给最早排队的 C，不给 E | 红：E 先结算，拿到空位 |
| `TestARetriedPairTouchedAgainIsSaidOncePerSettle` | `policy/policy_test.go`（`TestARefusedSubscribe…` 旁） | 重试中的 pair 被第二个视野持有：之后每次 Apply 只有 1 条 Refusal | 红：2 条（§1.4 F1 输出） |
| `TestPinnedSelfIsNotRefusedBySubscriberLimit` | `entitysync/subscription_test.go` 与 `policy/self_view_test.go` | 上限 2，4 个玩家互相看见：每个玩家都订阅了自己；SelfView 同样 | 红：`subs(4)=[1 2]`（§1.4 F2 输出） |
| `TestSubscriberRoomWarnIsAggregated` | `policy/interest_test.go` | 上限 2、200 个观察者，跑 30 次 Flush（模拟时钟跨过停滞上界）：WARN 记录不超过 1 条，并带 `pairs=198` | 红：198 条 |
| `TestOverCapWaitingHoldsDrainOnlyWhileThereIsRoom` | 同上 | 空位为 0 时 Drain 立即返回；有空位时 Drain 等到订阅上 | 部分红：修前前 10 轮阻挡 Drain，用短 ctx 断言“立即返回”会红 |
| `TestSubscriberRoomCountsLeavingSubscriptions` | `entitysync/subscription_test.go` | 退订后 remove 交付前空位仍为 0，交付后变成 1 | 新 API，没有修前状态，作为契约用例 |
| 周期类型 subject 的空位时延 | `entitysync/type_schedule_test.go` 或 policy | 确认 §4.5 的“尚未确认”项 | 结果先记录，不预设 |
| GroupView 超上限 | `policy/group_view_test.go` | 成员数超过上限：稳态下 Manager 的上限拒绝计数不增加，有空位时订阅上 | 红：批 2 先加计数，批 3 修 GroupView 前这条是红的 |

现有 §1.3 的用例全部保持通过，断言不改（等待数不超过 K 时，非上限原因的行为不变）。

### 5.2 变异

| 变异 | 应变红的用例 |
| --- | --- |
| M1 第 4 步不看空位，全部重说 | 等空位不重说的用例 |
| M2 删掉第 4 步 | 容量释放后订阅的用例 |
| M3 `settlePair` 不检查 subject 是否有排队 | 不插队的用例 |
| M4 入队不检查 `hold.wait` | 每次结算只说一次的用例 |
| M5 去掉 439 行的 `!pinned` | 钉住不受上限的用例 |
| M6 改回每个 pair 一条 WARN | WARN 汇总的用例 |
| M7 `queuedPending` 不看空位 | Drain 的用例 |

### 5.3 基准对比

- **批 1 先修基准**：`benchAllianceFlush`（`policy/benchmark_test.go:559`）把 `MaxSubscribersPerSubject` 设成 `players+16`（与 M-9 补充档的 overlay 相同），这样 2000 档量的才是“联盟成本”。另外新增 `BenchmarkRelationViewAllianceOverCap`：2000 人、缺省上限 1024，专门量超上限路径；收敛期把 `slog` 默认 logger 换成丢弃（修前每 pair 一条 WARN，会产生 GB 级输出；收敛期本来就不计时）。
- **前后两侧**：前 = 批 1 提交，后 = 批 2（或批 3）提交，同机交替跑，跑前看 `sysctl vm.swapusage`（2000 人上限 2016 档堆增量约 4.85 GB）。

```
GOWORK=off go test ./framework/sync/entitysync/policy -run '^$' \
  -bench 'RelationViewAlliance(Flush|OverCap)' -benchtime 20x -count 6 -timeout 0
/Users/whb/go/bin/benchstat before.txt after.txt
```

| 档 | 通过标准 |
| --- | --- |
| OverCap 2000（上限 1024） | 每轮耗时降到 Flush 2000（上限 2016）档的量级或更低，预期从约 960 ms 降到约 10 ms 以下；每轮分配不超过 2 倍 2 MB。WARN 行数降到 0（基准里丢弃了）；另用 `TestSubscriberRoomWarnIsAggregated` 管住 |
| Flush 1000 / 2000（上限内） | benchstat 显示无显著变化（`~`），或 p < 0.05 时变化在 ±3% 以内 |
| 回归守卫 | `BenchmarkRelationViewMemberChange`、`BenchmarkGroupViewFlush`、`BenchmarkClusterFlushRandomWalk` 同口径前后各 6 次，无显著变化 |

单侧预计约 8～9 分钟（OverCap 修前每次约 35 s，Flush 两档合计约 3～4 min），符合每轮约 10 分钟的约定。原始输出与 benchstat 放进 `docs/performance/benchmarks/sync-overcap-retry-<日期>/`。

### 5.4 门禁

本地：`GOWORK=off go build ./...`、`go vet ./framework/sync/...`、`go test -race -count=1 ./framework/sync/...`。改动跨包（Manager 的导出 API 与 Stats），推送前跑全量 `GOWORK=off go test ./...` 与根包 `GOWORK=off go test -count=1 .`。不改生成模板，不需要重新生成 game-demo。

## 6. 分批、工作量与风险

| 批 | 内容 | 预计 |
| --- | --- | --- |
| 1 | 基准修正 + `OverCap` 基准（只有测试代码）+ 修前基线测量 | 0.5 h + 测量约 10 min |
| 2 | Manager：F2、`SubscriberRoom`、计数与 Stats。Interest：`hold.wait` 去重（F1）、`roomWait`、结算第 2～4 步、`queuedPending` / `drainStalls`、汇总 WARN、指标、非上限重试限量。§5.1 的红绿用例与 M1～M7 变异 | 约 1 个工作日（生产代码约 250 行，测试约 300 行） |
| 3 | GroupView 跳过无空位的重说与汇总日志，加 GroupView 用例 | 2～3 h |
| 4 | 修后测量与 benchstat；文档：[ENTITY_SYNC](../framework/ENTITY_SYNC.md) 第 95 行（重说规则）与第 179 行（上限语义、钉住例外）、[guide/04-sync](../framework/guide/04-sync.md) 第 92 / 217 行、[impl/04-sync](../framework/impl/04-sync.md) §2E / §2G / §5、[OBSERVABILITY](../framework/OBSERVABILITY.md)；KNOWN-LIMITS 第 915 行改为已解决、删除（§4.6 的边角留作框架限制）；CHANGELOG `[Unreleased]`（行为变化：`OnRefusal` 对等待空位的 pair 只报一次；新 API；指标）；跑全量 | 2 h + 测量约 10 min |

合计约 1.5～2 个工作日。

| 风险 | 影响 | 缓解 |
| --- | --- | --- |
| R1 `OnRefusal` 调用频率下降 | 依赖“每轮都收到”的调用方会变安静。仓库里只有 `gameplay/movement/map_basic.go:87` 的注释提到它，没有调用方依赖每轮回调 | CHANGELOG 写明 |
| R2 钉住订阅超出上限 | 每个 subject 至多超出钉住订阅的个数（现有接线下是 1） | ENTITY_SYNC 写明“钉住不计入上限”；如果担心，可以改成“钉住只能多出 1 个”（D2 的备选） |
| R3 先来先得会让远处的关系视野观察者占着位置，近处的空间视野观察者排队 | 产品体验问题 | D3：维护者选排队顺序 |
| R4 Drain 语义变化 | 超上限、没有空位的 pair 不再阻挡 Drain（现在最多阻挡 10 轮） | 与超额暂缓一致；现有用例不涉及超上限 |
| R5 §4.6 的边角 | 目前没有触发路径 | 写进 KNOWN-LIMITS 的框架限制 |
| R6 新增锁调用 | `SubscriberRoom` 在 `in.mu` 下取 `subj.mu`，与现有 `Subscribe` 同序 | `-race` 跑 entitysync 与 policy 包 |

## 7. 需要维护者决定的点

| 编号 | 问题 | 选项 | 推荐与理由 |
| --- | --- | --- | --- |
| D1 | 总体做法 | A / A′ / B / **C** / D（§3） | **C，空位用查询探测。** 成本降到 O(等待 subject 数)，时延 1 个政策阶段；Manager 只加一个只读方法，状态都在政策侧 |
| D2 | 钉住订阅（玩家看自己）是否不受上限（F2） | **a. 钉住不计入上限** / b. 钉住只能多出 1 个 / c. 不改，写进文档 | **a。** 钉住的契约是“永不暂缓”（I5）；现有钉住来源都是“会话看自己的实体”，实际只多 1 个。b 要多维护一个计数；c 会让玩家看不到自己 |
| D3 | 空位按什么顺序分配 | **a. 先来先得（按第一次被拒的时间）** / b. 先按 Profile 优先级、同级先来先得（与超额暂缓的排序一致） / c. 按观察者 ID | **a。** 简单、可预测、不会饿死。b 更贴近“近处优先”，但等待项要记优先级，空位分配时要排序；如果确实需要“联盟成员优先 / 近处优先”，再升级到 b。c 等于让 ID 小的永远优先 |
| D4 | §4.6 的边角要不要加兜底轮询 | **a. 不加，写进 KNOWN-LIMITS** / b. 每次结算以小预算（如 256 个 pair）轮转重说等待项 | **a。** F2 修好后没有触发路径；b 在超上限期间每轮固定多出约 0.1 ms，还要多一个游标 |
| D5 | 非上限原因的重说要不要限量 | **a. 每次结算最多 K = 4096 个，常量** / b. 不限（维持现状） | **a。** 大批会话关闭但业务没调 Leave 时，也会出现同样的全量重说；K 以内行为与现在相同，现有用例不受影响；显式 `Apply()` 不限 |
| D6 | GroupView 是否同批处理 | **a. 同一轮做（批 3）** / b. 只记 KNOWN-LIMITS | **a。** 用的是同一个 `SubscriberRoom`，改动小；它的成本虽是线性，日志也是每个成员一条 |
| D7 | WARN 的粒度 | **a. 每个 Interest 每 10 s 一条汇总，带前 3 个 subject** / b. 每个 subject 每 10 s 一条 | **a。** b 在 2000 个 subject 都超上限时仍是每 10 s 2000 行；各 subject 的详情由指标与 Drain 诊断提供 |
| D8 | 等空位的 pair 何时阻挡 Drain | **a. 只在它的 subject 有空位时** / b. 从不 / c. 维持“10 轮内阻挡” | **a。** Drain 的含义是“该做的都做完了”：有空位就应该等到订阅上，没空位就是在等外部事件，与超额暂缓一致。b 会让“释放后马上 Drain、然后断言已订阅”这类用法不可靠；c 每个 pair 记轮数，正是要去掉的成本 |

另外说明：M-9 的选项 A（定义视图或设置成员时，成员数超过上限就报错）不在本方案里。维护者已选 B；而且上限是按 subject 统计所有来源的，单个视图的成员数并不决定最终会不会超上限，在定义视图时也判断不准。

## 8. 不做的事

- 不加兼容层（线上未部署，且不涉及协议与存储）。
- 不改 `MaxSubscribersPerSubject` 的缺省值与配置方式。
- 不改超额降级（会话持有对象上限）的机制。
- 不新建包。Manager 的改动放在 `subscriptions.go` / `manager.go`；政策侧放在 `interest.go` / `interest_queue.go`（重试相关的代码已在 `interest_queue.go` 的“重试停滞”一节，新增的等空位部分放在它旁边）。
