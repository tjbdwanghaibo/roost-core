# game-demo 玩家所有权租约：状态机方案（2026-10-04）

- 范围：`demo/internal/service/game/playerowner.go.tmpl`（`PlayerOwners`）与 `demo/game/playerroute/playerroute.go.tmpl`（Redis 共享表 `Store`），以及它们的调用方 `enter_game.go.tmpl`、`gift_saga.go.tmpl`、`matchmaker.go.tmpl`、`service.go.tmpl`。
  （任务说明与 WANTED W-2026-10-04-08 写的 `demo/internal/route/playerroute.go.tmpl` 不存在，实际路径是 `demo/game/playerroute/playerroute.go.tmpl`。）
- 基线：main `64529e39`。`playerowner.go.tmpl` 最后一次改动是 `890abdda`（RR-20261004-14），与基线逐字相同；下文 `po:N` 指 `playerowner.go.tmpl` 第 N 行，`pr:N` 指 `playerroute.go.tmpl`，`eg:N` 指 `demo/game/controllers/player/enter_game.go.tmpl`，`test:N` 指 `playerowner_test.go.tmpl`。
- 性质：**只出方案**。仓库代码没改；探针只放在 scratchpad 生成工程里，红文本原文抄在 §4 与附录 A。
- 来源：维护者 2026-10-04 决定——一天内修了 7 条交错缺陷（RR-20260930-23、RR-20261001-07、RR-20260921-03、RR-20260921-04、RR-20261004-10、RR-20261004-11、RR-20261004-14），每修一条都暴露更窄的交错，W-2026-10-04-08 又列了三处。先把“本地租约 + 共享表”的协议写成显式状态机，再决定是重写还是保留。
- 读过的记录：上述 7 条的 `docs/bug` 与 `docs/bugfix`；更早的 RR-20260920-03 / 04 / 09 / 10 / 11 / 12、RR-20260926-31、RR-20260927-02（只有 `CloseSessions` 计数，与协议无关）、RR-20260926-36（登录预算）；`docs/agent-skills/roost-coding/SKILL.md` 与 `references/fix-contract-review.md`。
- 图谱：`demo/` 在 codebase-memory 里是排除目录，`.tmpl` 也不按 Go 建图（RR-20261004-10 / 11 / 14 记录同样说明）。本文行号和结论都直接取自基线源码与生成工程实跑。

## 0. 结论先行

1. 现状是 **6 个独立字段拼出来的隐式状态**：`local` 里有没有记录、`interrupted`、`handingBack`、`validUntil`、`evictions[pid]`、`claiming[pid]`（另有一个已经没用的 `lost`）。这些字段由 4 类 goroutine 写：刷新循环、登录连接、后台消费者、撤离 goroutine。所有补丁都是同一种病：**一个时刻做的判断（`continuous`、`snapshot()`、`Refresh` 的回复、`stillOurs`），在另一个时刻、对着已经变了的状态生效**，而状态里没有任何东西能说明“已经不是你判断时的那一个了”。
2. Redis 侧有两处信息丢失，让本地没法收口：token 按进程固定（`pr:149-153`），所以 `Held` 分不出键是哪一次认领设的（RR-20261004-14 的根）；`Store.Claim` 把“新设下”“延长了”“延长没生效”都回成同一个 `Route`（`pr:216-244`，W-08 第 3 处）。
3. 在生成工程里写了 7 个确定性探针函数（W08-3 含 2 个子测试，共 8 个违例场景），**全部红，`-race -count=20` 每条都是 20/20**：W-08 三处都能真的违例（第 2 处有两种，abandon 一种、公开 `Release` 一种），另外新推出 3 条（§4 的 N-OWNS、N-CONT、N-TOLD）。现有回归（`playerowner_test` 35 个顶层用例 + 13 个子测试、`playerroute_test` 10 个，所在两个包整包）照样全绿，也就是说这 8 条都**不在现有回归覆盖之内**。
4. 推荐 **重写 `PlayerOwners` 核心，外部接口不变**（§7）。做法是：每个玩家一条记录，状态显式、带世代号；所有异步结果带着发起时的世代号，世代变了就作废；Redis 改成“每次认领一个 token”，认领用一次原子 CAS 拿到明确结论。这样 7 条 RR、W-08 三处和新推出的三条都由构造保证，不再一条一条打补丁。工作量约 3–4 个 agent 日（实现 + 回归迁移 + 复核），分三笔提交。§8 列了需要维护者先拍板的 10 个点。

---

## 1. 参与者与时间

### 1.1 参与者（谁会改一个玩家的所有权状态）

| 参与者 | goroutine | 入口（基线行号） | 会做什么 |
| --- | --- | --- | --- |
| 刷新循环 | `refreshLoop` 唯一的 goroutine（`po:521-535`），每 `RefreshInterval` 跑一次 `renew` | `renew:561-601` | 先处理全部 `Refresh` 结果（Held → `confirmRenewal`、Err → 只记一条日志、不是我们的 → `interrupt` 并记入 lost），再在同一份回合预算里先 `retakeLost:637`、后 `handBackIdle:747` |
| 登录 | 连接读循环 goroutine（RR-20260926-36），不是 Nest 快 worker | `eg:61` `owners.Claim(loginCtx, …)`；`eg:76` `GetOrCreate`；`eg:102` `scene.Join` | 跨间断时认领并扔掉副本；`mine=true` 后装载实体并进场 |
| 后台 `Owns` | saga 步骤消费者回调 goroutine（`gift_saga.go.tmpl:395` `admitPhase`） | `Owns:1328-1354` | 先 `Admitted`；Redis 里没人持有就 `Claim`，可能跨间断扔副本 |
| 后台 `OwnedHere` | matchmaker（`matchmaker.go.tmpl:115`）、handoff（`gift_saga.go.tmpl:284`） | `OwnedHere:1361-1377` | 只问、不认领 |
| 写闸 | access 边界中间件（`service.go.tmpl:157` 注册 `WriteGate`） | `AdmitMessage:1086` → `Admit:1018` | 只读本地状态、不阻塞；登录消息豁免 |
| 归还 | 刷新循环 goroutine | `handBackIdle:747-796` → `handBackOne:807-855` → `finishHandBack:861` | 闲置、无连接的玩家：撤离 → 等投影 → `store.Release` |
| 撤离 | 每个玩家至多一个后台 goroutine（`dropResident:445` 起 `runEviction:486`） | `playerEvictor.EvictPlayer:191-222`：先 `scene.Leave`（`:199-201`），再 `Destroy`（`:213`） | 不可取消；等实体锁，也就是等实体上正在跑的事务 |
| 围栏 | 刷新循环 goroutine | `fence:977-1008` | 删本地状态、关连接、异步撤离；不碰 Redis |
| 公开 `Release` | **没有调用方**（全 demo 搜 `\.Release\(` 只有 store 层与测试） | `Release:1309-1319` | 删本地状态 + `CompareAndDelete` |
| Redis 共享表 | — | `Store`（`pr:145-322`） | `SetNX` / `Get` / `CompareAndExpire` / `CompareAndDelete`；值是 `<sid>:<token>`，token 每个进程一个（`pr:149-153`、`:167`） |
| 其他进程 | — | 同一份 `Store` | 认领（`SetNX`）、续租、释放；只能看见键 |

### 1.2 时钟

- 本地窗口用本地时钟。`validUntil = asked + Lease`（`po:929`），`asked` 是**设键那次请求发出的时刻**：认领在 `store.Claim` 之前取（`po:1162`），续租在 `Refresh` 之前取（`po:563`）（RR-20261004-14）。
- Redis 键的 TTL 用 Redis 服务端时钟，从它处理请求的那一刻起算，所以不早于 `asked`。只要 Redis 不提前丢键（假设 A1），键就至少活到 `asked + Lease`。
- 回合预算用两把钟记同一份账：表时钟 `owners.now()` 管记账，回合 ctx 管真正结束等待（`refreshPass:611-630`）。

### 1.3 参数（出厂值与由来）

| 参数 | 值 | 位置 | 含义 / 关系 |
| --- | --- | --- | --- |
| `Lease` | 30s | `pr:46` | 键 TTL |
| `RefreshInterval` | 10s | `pr:47` | 刷新周期；`< Lease − AdmissionGuard`，丢一次续租不至于断服务 |
| `AdmissionGuard` | 5s | `po:330` | 在窗口结束前这么久停止准入，留给在途事务（假设 A2：事务 < 5s） |
| `evictBudget` / `evictWait` | 5s | `po:167`、`:405` | 调用方等一次撤离的上限，撤离本身不受它约束（RR-20260920-12） |
| `projectionWait` | 5s（= `evictBudget`） | `po:405` | 一个归还回合共用一份投影等待（RR-20260926-31） |
| `handBackPassBudget` / `handBackPassWait` | 15s = `Lease − RefreshInterval − AdmissionGuard` | `po:377`、`:405` | 一次刷新回合在续租之后的全部等待：先重新认领、再归还（RR-20260921-04、RR-20261004-10） |
| `handBackBudget` | 8 人 | `po:351` | 一轮最多选几个人归还（个数上限，不是时间上限） |
| `HandBackIdle` | 20s = 2 × `RefreshInterval` | `po:346` | 多久没被用过、又没有连接，就归还（假设 A3：后台工作两次 `Admit` 之间 < 20s） |
| `login_timeout` | 2s（`dispatch_timeout` 3s） | `codegen/internal/roost/render_player_tcp.go:28-29,220` | 登录里“认领 + 装载”这一段的预算；`< evictBudget`，所以登录遇上忙实体会回 `login_timeout`，客户端重试 |

现在由测试钉住的常量关系：`handBackPassBudget < Lease − RefreshInterval`、`handBackPassBudget ≥ evictBudget`（`TestTheHandBackPassBudgetFitsInsideTheLease`，`test:1280`）。

---

## 2. 现状状态机（as-is）

### 2.1 每个玩家的派生状态

现状没有枚举。下面是从字段组合里反推出来的状态（`L` = `local[pid]` 存在，`E` = `evictions[pid]` 存在，`C` = `claiming[pid] > 0`）。Redis 键另记为 `R ∈ {self, other, none}`。注意 `self` 只认 sid + **进程** token，分不出是本进程哪一次认领设的。

| 状态 | 字段组合 | `Admit`（`po:1018-1076`） | 会被续租吗（`snapshot:1403`） | 说明 |
| --- | --- | --- | --- | --- |
| **A0 Absent** | `!L ∧ !E` | 拒（无状态） | 否 | 从没持有过、已归还、被围栏且撤离已结束、被 `abandon`。R 可能还是 `self`：被 abandon 的认领、没收到回复的 SetNX、`store.Release` 失败，这几种都会留下“孤儿键” |
| **A1 Serving** | `L ∧ !I ∧ !H ∧ !E ∧ now < validUntil − guard` | 放行，并写 `lastUsed` | 是 | 正常服务 |
| **A2 Lapsing** | `L ∧ !I ∧ !H ∧ now ≥ validUntil − guard` | 拒（窗口不足） | 是 | 续租一直 Err，窗口自己跑完。`Claim` 把它当作跨间断 |
| **A3 Interrupted** | `L ∧ I` | 拒 | 是，但 Held 不确认，改走重新认领（`po:566-576`、`:892`） | Redis 已经答过“不是你的”，或者跨间断认领没收到 Redis 回复（`markUnansweredClaim:1219`） |
| **A4 HandingBack** | `L ∧ H`（带 `handBackDone`） | 拒 | 同一 goroutine，碰不上 | 子步骤：查会话 → 撤离 → 等投影 → 复核 `stillOurs` → `store.Release` |
| **A5 DroppingWithLease** | `L ∧ !H ∧ E`，租约本身可服务 | 拒（`po:1041-1043`，`refusalLocked` 返回 nil） | 是，正常确认 | 归还的撤离等超时后，租约回到服务、撤离还在跑（RR-20261004-11） |
| **A6 DroppingWithoutLease** | `!L ∧ E` | 拒（无状态） | 否 | 围栏的异步撤离；或者 `abandon` 之后撤离还在跑 |
| 叠加 **C Claiming** | 上面任一状态再加 `C` | 不变 | 不变 | 只有一个作用：归还选人时跳过它（`po:759`） |

`leaseState.lost`（`po:273`）实际上已经没用：`fence` 把它置位的同一个临界区里就把记录从 `local` 删了（`po:984-985`），持有这个指针的只有归还，而归还用 `stillOurs` 比较的是指针（`po:839`）。

### 2.2 转换表

“锁”指 `owners.mu`。`mu` 从不在等待时持有（RR-20260921-03 记录 §锁与等待顺序），这一点在现状里成立。

| # | 从 → 到 | 触发者 | 条件 | 锁内 / 锁外、Redis、等待 | 代码 | 修补它的 RR |
| --- | --- | --- | --- | --- | --- | --- |
| T1 | A0/A2/A3 → A1 | 登录 / `Owns` / 刷新循环重新认领 | `continuous=false`（`Admit` 拒绝，或者等过一次归还），`store.Claim` 回本 sid，`dropResident` 成功 | `enterClaim`（锁内）；`continuous` 用 `Admit`（锁内，**单独一次**）；`asked`；`store.Claim`（锁外，1–3 次往返）；`dropResident`（锁外，≤ `evictWait`/ctx）；`confirmClaim`（锁内，**按玩家 id 插入或更新**） | `claim:1126-1202` | 09、12、14（asked） |
| T2 | A1 → A1 | 同上 | `continuous=true` | `store.Claim` 对自己的键 `CompareAndExpire`，**不看 applied**（`pr:239`）；`confirmClaim` | `po:1158,1177,1200` | 09、11 |
| T3 | A0/A2/A3 → A0(+E) | 同上 | 认领拿到了，撤离失败或等超时 | `abandon`：锁内只有 `refusalLocked != nil` 才删 | `po:1178-1198`、`abandon:1295` | 07、23、11 |
| T4 | A2/A3 → A3 | 同上 | `store.Claim` 报错，且 `!continuous` | `markUnansweredClaim`：锁内，有状态并且被拒才置 `interrupted` | `po:1164-1171`、`:1219` | 14 |
| T5 | 任意 → 不变 | 同上 | `store.Claim` 回了别的 sid | **本地什么都不记**，直接返回 false | `po:1173-1176` | —（见 N-TOLD） |
| T6 | A1/A5 → A1/A5 | 刷新循环 | `Refresh` 答 Held，且 `!interrupted` | `confirmRenewal`→`confirmLocked`：锁内，**状态不存在就新建一份**（`po:924-928`） | `po:566-567,889-938` | 10（先确认后等待）、14 |
| T7 | A1 → A2 | 时间 + 刷新循环 | `Refresh` 报错 | 只记日志 | `po:577-583` | 04 |
| T8 | A1/A2/A5 → A3 | 刷新循环 | `Refresh` 答不是我们的 | `interrupt`：锁内，有状态才置位 | `po:584-592,943` | 09 |
| T9 | A3 → A1 + 关连接 | 刷新循环 `retakeOne` | 走 T1 并成功 | 拿到之后再 `closeSessions` | `po:687-705` | 23 |
| T10 | A3 → A0(+E) + 关连接 | 刷新循环 | 走 T3 | `errStaleCopyKept` → `closeSessions` | `po:665-678` | 23、07 |
| T11 | A3 → A3 | 刷新循环 | 认领报错（T4），或回合预算用完 | 等下一轮 | `po:639-642,679-686` | 10、14 |
| T12 | A3 → A6 | 刷新循环 | 重新认领回了别的 sid | `fence`：锁内删状态，锁外关连接，异步撤离 | `po:706-707,977-1008` | 04、09 |
| T13 | A3 且 Held → 重新认领 | 刷新循环 | `confirmRenewal` 返回 false | 并入 lost | `po:567-576` | 14 |
| T14 | A1 → A4 | 刷新循环 `handBackIdle` | 回合还有时间；`!lost ∧ !H ∧ !C ∧ 闲置 ≥ HandBackIdle`；装了 fencer | 锁内选人并置 `H` | `po:747-772` | 10、11、03、04 |
| T15 | A4 → A0 | 同上 | 没有连接；撤离成功；投影成功；`stillOurs` | `store.Release`（锁外，不计入预算）；`finishHandBack(true)` | `po:807-855,861` | 26-31、03 |
| T16 | A4 → A1 | 同上 | 有连接 / 投影失败 / 没装投影屏障 / 预算用完 | `finishHandBack(false)` | `po:811-832,779-783` | 10、04、26-31 |
| T17 | A4 → A5 | 同上 | 撤离等超时 | `finishHandBack(false)`，撤离继续 | `po:815-821` | 12、11 |
| T18 | A5 → A1 | 撤离 goroutine | 撤离结束（成功或失败都算） | `runEviction` 锁内摘掉 `E` | `po:486-494`、`:1041` | 11 |
| T19 | A6 → A0 | 撤离 goroutine | 撤离结束 | 同上 | `po:486-494` | 12 |
| T20 | `Claim` 等 A4 | 登录 / `Owns` | 撞上 `H` | 锁外等 `handBackDone`，或者调用方 ctx 结束；之后按 `waited=true` 当作跨间断 | `enterClaim:1248-1272` | 03 |
| T21 | 有记录 → A0 | 公开 `Release` | 无条件 | 锁内删状态；`store.Release` | `po:1309-1319` | —（没有调用方） |

### 2.3 补丁分布（RR → 转换）

| RR | 修补的转换 / 守卫 |
| --- | --- |
| RR-20260920-03 | Redis 侧三个 compare 操作做成原子的；token 区分进程（`pr:92-110,149-153`） |
| RR-20260920-04 | `Admit` 只认确认过的窗口；`AdmissionGuard`；`Refresh` 报错算 UNKNOWN（T7） |
| RR-20260920-09 | `interrupted`（T8）；`continuous` 判断，跨间断就扔副本（T1）；围栏时一并扔副本（T12） |
| RR-20260920-10 / 11 | 闲置归还（T14）；`handingBack` 先停准入；`confirmClaim` 算使用、`confirmRenewal` 不算 |
| RR-20260920-12 | 撤离放到自己的 goroutine 上跑完，调用方只等 `evictWait`；按玩家单航班（`evictions`） |
| RR-20260926-31 | 归还时撤离之后、释放之前等投影（T15） |
| RR-20260921-03 | `enterClaim` / `claiming` 让认领与归还互斥（T20、T14 的 `!C`）；`finishHandBack` 独占清除 `handingBack`；`stillOurs` 复核 |
| RR-20260921-04 | 归还回合的时间预算 |
| RR-20260930-23 | 重新认领之后关连接（T9、T10） |
| RR-20261001-07 | 认领了但副本扔不掉时 `abandon`（T3、T10） |
| RR-20261004-10 | `renew` 分成两段；重新认领也进回合预算（T6 先于 T9–T11） |
| RR-20261004-11 | 撤离进行中 `Admit` 拒绝（A5、T17、T18） |
| RR-20261004-14 | `asked` 起算窗口；`markUnansweredClaim`（T4）；Held 遇上 `interrupted` 改为重新认领（T13） |

可以看出几乎每一条转换都被补过不止一次，而补的方式只有两种：**再加一个标志位**，或者**在某个位置再看一眼某个标志位**。§4 的探针说明，“再看一眼”的位置总会漏掉一种时序。

---

## 3. 不变量

每条写明：现状靠什么保证，现状下还有没有违例。I2 由 I1 推出；I4、I7 到 I11 是锁、预算与生命周期的规则。

| # | 不变量 | 现状靠什么保证 | 现状违例（§4） |
| --- | --- | --- | --- |
| **I1** | `Admit(p)` 放行 ⇒ 此刻 Redis 键 `p` 是本进程**这一次**租约的值，并且 `now < asked + Lease − AdmissionGuard`（`asked` 是设键那次请求发出的时刻） | `refusalLocked:1056-1076` + `confirmLocked:929`（RR-04、14） | W08-2b（公开 `Release` 与续租交错）、W08-3（`mine` 由一次没生效的延长得来）、N-TOLD |
| **I2** | 同一玩家任何时刻至多一个进程的副本可服务 | I1 + 键唯一 + A1 + A2 | 只要 I1 被违反（W08-3 是两个进程同时认为自己持有） |
| **I3** | 告诉调用方 `mine=true` ⇒ 共享表认本进程，并且本地放行 | `enterClaim` / `finishHandBack` / `stillOurs`（RR-03） | W08-3 |
| **I4** | 一次刷新回合有界；刷新循环不等自己 | 回合预算（RR-04、10）；归还排在重新认领之后、同一 goroutine，并在返回前结束自己标记的每一个（`po:741-746`） | 没有探针违例。剩下的是 `Refresh` 逐人往返、重新认领的 SetNX、`Release` 都在预算外（RR-10 记录 §未验证项，已知） |
| **I5** | 有过间断的副本（间断前装载、此后没有被证明一直连续持有的那一份）在扔掉之前不被放行；也就是 Redis 答过“不是你的”之后，只有一次扔了副本的认领能让玩家重新进入服务 | `interrupted`、`continuous`、`abandon`、撤离进行中拒绝、`markUnansweredClaim`、Held 遇上 `interrupted` 改为重新认领（RR-09、07、11、14） | **W08-1、W08-2a、N-CONT** |
| **I6** | 连接与副本同生命周期：扔副本（会先 `scene.Leave`）时，这个玩家在本进程的连接要么一起关掉，要么就是正在登录、马上会 `Join` 的那一条 | `retakeOne` 里的 `closeSessions`（RR-23）；`fence` | **N-OWNS**（`Owns` 跨间断）；多会话登录同理（未单独探） |
| **I7** | 归还只释放自己标记、并且中途没被打断的那一次；归还进行中，不确认同一玩家的认领 | `enterClaim` + `!C` + `finishHandBack` 独占 + `stillOurs`（RR-03） | 无 |
| **I8** | 交出租约之前，本进程对该玩家已提交的写入已经落库 | `handBackOne:823-832`（RR-26-31） | 无（围栏不交出租约：别人已经持有，或者状态未知） |
| **I9** | 续租不算使用，新认领和准入算使用 | `confirmLocked` 的 `used` 参数（`po:935`），`Admit:1047`（RR-10、11） | 无 |
| **I10** | 撤离不可取消；每个玩家至多一个撤离；调用方等待有界 | `dropResident` / `runEviction`（RR-12） | 无 |
| **I11** | `mu` 不跨等待；快池上不阻塞：`Admit` 只读本地状态，`Claim` 只在登录、消费者、刷新循环这三类 goroutine 上被调用 | 源码结构；RR-03 记录 §锁与等待顺序 | 无 |

**假设**（协议靠它们成立，但现状和下文的方案都**没有**强制它们）：

- **A1** Redis 不提前丢键：没有 failover 丢写、没有被清库、没有运维删除。现状把“提前丢键”当作单故障处理：靠下一次续租发现（≤ `RefreshInterval`），在此之前本进程照常放行。要真正关掉这个窗口，只能在持久化层做围栏（见 §8 D3）。
- **A2** 被放行的事务在 `AdmissionGuard`（5s）内结束。没有强制，RR-04 已接受。
- **A3** 后台工作两次 `Admit` 之间不超过 `HandBackIdle`（20s）；归还不统计在途事务（RR-11 §未做，CARRYOVER A7）。
- **A4** 本地单调时钟与 Redis 时钟的速率差可以忽略。

---

## 4. 失败场景矩阵

### 4.1 探针环境

```text
$ cd /Users/whb/roost/roost-core    # main 64529e39
$ GOWORK=off go run ./codegen/cmd/roost project new planet -skip-deps -module example.com/planet \
    -out <scratchpad>/lease-sm -template game-demo
$ cd <scratchpad>/lease-sm && go mod edit -replace github.com/tjbdwanghaibo/roost-core=/Users/whb/roost/roost-core && GOWORK=off go mod tidy
# 生成的 internal/service/game/playerowner.go 与模板逐字相同（只有包名不同）
$ GOWORK=off go test -race -count=1 ./internal/service/game/ ./game/playerroute/ -skip TestProbe
ok  	example.com/planet/internal/service/game	12.470s
ok  	example.com/planet/game/playerroute	1.586s
$ GOWORK=off go test -race -count=20 -run TestProbe ./internal/service/game/ ./game/playerroute/
# 下面 7 个探针函数（W08-3 含 2 个子测试）每一个都 20/20 FAIL
```

探针文件放在生成工程里：`internal/service/game/zz_leasesm_probe_test.go`、`game/playerroute/zz_leasesm_probe_test.go`。全部复用既有 harness（`ownersUnderTest`、`fakeLeaseTable`、`fakeFencer`、`evictorStub`、`evictorFunc`、`claimHookTable`、`claimInBackground`、`waitDropEnded`，以及 playerroute 的 `fakeKeyspace.beforeCompare`）。用 channel 控制先后顺序，没有 sleep。全文见附录 A，可直接复跑。本机 darwin/arm64，Go 1.27.0。

### 4.2 矩阵

“已防住”指基线上的代码和回归能挡住这个场景；“探针红”是本次确认的违例；“未构造”是只能从源码推出、或者依赖某个假设的场景。

| # | 场景（触发交错） | 涉及转换 | 违反 | 现状 | 证据 |
| --- | --- | --- | --- | --- | --- |
| R-0920-03 | 先 GET 再 EXPIRE / DEL，中间租约易主 | Redis 操作 | I1 | 已防住：三个 compare 操作是原子的 | `TestAStaleReleaseMustNotDeleteTheNewOwner` 等（playerroute） |
| R-0920-04 | 续租失败后照样写 | T7 | I1 | 已防住 | `TestAnUnknownRenewalRunsAdmissionOutInsteadOfPretending`、`TestAdmissionStopsOneGuardBandBeforeTheDeadline` |
| R-0920-09 | 租约失而复得后继续用旧副本 | T8 → T1 | I5 | 已防住 | `TestALeaseThatWasInterruptedDropsTheResidentCopy`、`TestFencingAlsoDropsTheCopy` |
| R-0920-10 / 11 | 后台认领永不归还；重新认领被当成闲置还掉 | T14、T1 | 可用性、I7 | 已防住 | `TestABackgroundClaimIsHandedBackWhenNobodyIsPlaying`、`TestALeaseRetakenForNewWorkIsNotIdle`、`TestAHandBackInFlightAdmitsNothing` |
| R-0920-12 | 撤离的预算形同虚设，忙实体钉住刷新循环 | T1/T3 的 `dropResident` | I4、I10 | 已防住 | `TestAClaimWaitsAtMostTheBudgetForABusyEntity`、`TestASecondDropJoinsTheOneAlreadyRunning`、`TestOneBusyEntityDoesNotHoldUpTheRefreshLoop` |
| R-0926-31 | 先撤离、马上释放，投影还没落库 | T15 | I8 | 已防住 | `TestAHandBackWaitsForThePlayersWritesToReachTheDatabase` |
| **R-0921-03** | 归还进行中被重新认领，确认擦掉了归还标记 → `mine=true` 而 Redis 没人持有 | T14/T15 × T1/T2 | I3、I7 | 已防住 | `TestAClaimDuringAHandBackNeverEndsWithAnUnownedPlayer` 四个子测试 |
| **R-0921-04** | 一个归还批次占住刷新循环 45s | T14–T17 | I4 | 已防住 | `TestAHandBackPassHoldsTheRefreshLoopLessThanTheLeaseAllows` 等三条 |
| **R-0930-23** | 重新认领扔了副本，连接却留着（脱离场景） | T9、T10 | I6 | 刷新循环这条路径已防住；`Owns` 路径没有防住，见 N-OWNS | `TestARetakenLeaseClosesTheSessionsThatLivedThroughTheGap`、`TestARetakeWhoseCopyCannotBeDroppedStillClosesTheSessions` |
| **R-1001-07** | 认领了但副本扔不掉，下一轮续租把它续回服务 | T3/T10 → T6 | I5 | 主路径已防住（`abandon`）；它记录里那条窄交错还剩一支，见 W08-1 | `TestAClaimWhoseCopyCannotBeDroppedIsNotRevivedByTheNextRenewal` |
| **R-1004-10** | 重新认领逐个等撤离；确认排在等待之后，本地窗口越过键 TTL | T6、T9–T11 | I4、I1 | 已防住 | `TestARefreshPassRetakingLostLeasesHoldsTheLoopLessThanTheLeaseAllows`、`TestARefreshPassStopsWaitingForRetakesWhenItsBudgetRunsOut` |
| **R-1004-11** | 归还的撤离等超时后，`Admit` 照样放行，登录跳过撤离直接进场 | T17、T18 | I5、I6 | 已防住 | `TestAHandBackWhoseDropOutlivesItsWaitAdmitsNothingUntilTheDropEnds` |
| **R-1004-14** | 窗口从确认时刻起算；认领丢了回复后下一轮 Held 被当成确认 | T1、T6、T4、T13 | I1、I5 | 已防住 | `TestAdmissionStopsAGuardBeforeTheKeyRedisSetCanExpire`、`TestAClaimRedisDidNotAnswerIsNotConfirmedByTheNextRenewal` |
| **W08-1** | 登录跨间断（窗口跑完、键过期）；它的撤离被忙实体卡住；这期间续租答 Held（登录的 SetNX 把进程 token 放回去了），而状态没有 `interrupted`，于是被确认；撤离**以错误结束**；`abandon` 看到状态可服务就不删 | T1/T3 × T6 | **I5** | **探针红** | `TestProbeW08a_RenewalConfirmsAClaimWhoseDropThenFails`（下文 ①） |
| **W08-2a** | 刷新回合已经发出对 42 的 `Refresh`（回复还没到）；登录跨间断，撤离失败，`abandon` 删掉状态；`Refresh` 答 Held；`confirmLocked` **新建了一份状态** | T3 × T6 | **I5** | **探针红** | `TestProbeW08b_RenewalRecreatesStateThatAbandonDeleted`（②） |
| **W08-2b** | 同一个窗口，交错对象换成公开 `Release`：`CompareAndExpire` 先落地，`Release` 删了键和状态，续租的回复后到，又把状态建了回来 | T21 × T6 | **I1** | **探针红**（潜在：`Release` 在 demo 里没有调用方） | `TestProbeW08b_RenewalRecreatesStateThatReleaseDeleted`（③） |
| **W08-3** | `Store.Claim`：SetNX 没拿到 → `Get` 读到自己 → 键在两步之间过期（或者过期后被别人拿走）→ `CompareAndExpire` 没生效 → 仍然回答“是我们的” | `pr:216-244` → T1/T2 | **I3、I1、I2** | **探针红** | `TestProbeW08c_ClaimAnswersOursWhenItsExtendDidNotApply/{lapsed,lapsed-and-taken}`（④） |
| **N-OWNS** | 已登录玩家的续租连续失败超过租约（键过期）；Redis 恢复后，后台 `Owns`（赠礼发送方就是在线玩家）跨间断认领，扔掉副本（真实 evictor 会先 `scene.Leave`）。只有刷新循环关连接，所以这个玩家的连接还开着、却已经不在场景里；下一轮续租答 Held、正常确认，再也不会关 | T1（`Owns` 路径） | **I6**（RR-20260930-23 的症状换了一个入口） | **探针红** | `TestProbeN_OwnsAcrossAGapDropsAConnectedPlayersCopyWithoutClosingTheSession`（⑤） |
| N-OWNS′ | 同一玩家开着多个会话（生成的 TCP 按 SessionID 登记，`render_player_tcp.go:833-842`，同 SessionID 才替换），另一台设备登录并跨间断：登录路径不关连接（`po:955-958`），旧设备的会话脱离场景 | T1（登录路径） | I6 | 未构造：`fakeFencer` 分不出哪条是登录自己的会话。机制与 N-OWNS 相同，结论由源码推出 | 源码 |
| **N-CONT** | 键提前消失（failover / 清库），没人接手。重新登录的 `Claim` 已经算好 `continuous=true`，还没到 `store.Claim`；这时刷新回合从 Redis 得知键没了，置 `interrupted`；登录接着 SetNX、`confirmClaim` 清掉 `interrupted`，**没扔副本**；回合自己的重新认领看到状态可服务，也不扔，还记一条 “the stale copy was dropped” | T8 × T2 | I5 的规则（RR-09：Redis 答过“不是你的”，只有扔了副本的认领能清掉它） | **探针红**。要真造成数据损坏还需要第二次提前丢键（别人在间隙里持有、写入、又失去了键，A1 被违反两次） | `TestProbeN_ALoginThatDecidedContinuousBeforeTheInterruptionClearsIt`（⑥） |
| **N-TOLD** | `Claim` 被 Redis 告知“sid 2000 持有”，回答 `mine=false`，但本地状态原样保留，`Admit` 一直放行到下一轮续租（≤ 10s） | T5 | I1（以及 RR-09 的“Redis 是权威”） | **探针红**。这段窗口本来就是 A1 的固有窗口；问题在于本进程**已经被告知**，却没有用上这个信息 | `TestProbeN_AClaimToldAnotherOwnsThePlayerLeavesAdmissionOpen`（⑦） |
| N-RACE | 刷新循环重新认领时，同一玩家恰好在本进程重新登录，`closeSessions` 把这条新登录也关了 | T9 × 登录 | 可用性 | 已知并接受（RR-23 §兼容性 “已知竞争”），没有回归 | RR-23 记录 |
| N-ORPHAN | `abandon`、认领没收到回复、或 `Release` 失败之后留下孤儿键：`Owns` 最多 `Lease`（30s）内一直答 false | A0 + R=self | 可用性 | 已知并接受（RR-07、11 记录） | `po:1350-1353` |
| N-RTT | `Refresh` 逐人往返、重新认领的 SetNX / Get / CAE、`Release` 都不在回合预算里 | renew | I4 | 已知（RR-10 §未验证项） | 记录 |
| N-A1 | Redis 提前丢键（failover 时异步复制丢写），另一进程接手；在下一次续租发现之前，本进程照常放行 | T7/T8 | I2 | 未构造：需要真实 Redis，而且依赖假设 A1 | — |
| N-A2 / A3 | 事务超过 `AdmissionGuard`；后台工作两次 `Admit` 之间超过 `HandBackIdle` | Admit、T14 | I2 | 未构造：依赖假设 A2、A3（RR-04、11 已接受） | — |

### 4.3 探针红文本（原文）

```text
① --- FAIL: TestProbeW08a_RenewalConfirmsAClaimWhoseDropThenFails
2026/10/04 22:51:18 ERROR player owners: claimed a player whose stale copy could not be dropped; leaving the lease to lapse player_id=42 err="the entity is busy; Destroy failed"
    zz_leasesm_probe_test.go:73: login: mine=false err=player owners: player 42 claimed but not taken into service: the stale copy could not be dropped: the entity is busy; Destroy failed
    zz_leasesm_probe_test.go:75: I5 violated: the login's drop of the copy from before the gap failed (the copy is still resident), yet Admit(42)=nil — the renewal that landed during the drop confirmed the key the login's SetNX re-created, and abandon then saw a serviceable lease and kept it

② --- FAIL: TestProbeW08b_RenewalRecreatesStateThatAbandonDeleted
2026/10/04 22:51:18 ERROR player owners: claimed a player whose stale copy could not be dropped; leaving the lease to lapse player_id=42 err="the entity is busy; Destroy failed"
    zz_leasesm_probe_test.go:112: login: mine=false err=player owners: player 42 claimed but not taken into service: the stale copy could not be dropped: the entity is busy; Destroy failed abandoned=true
    zz_leasesm_probe_test.go:117: I5 violated: abandon forgot player 42 (its copy from before the gap could not be dropped, abandoned=true), the in-flight renewal answered Held and confirmLocked created a new lease state — Admit(42)=nil on the stale copy

③ --- FAIL: TestProbeW08b_RenewalRecreatesStateThatReleaseDeleted
    zz_leasesm_probe_test.go:152: I1 violated: Release gave player 42 up (shared table owner present=false), the renewal in flight answered Held and confirmLocked recreated the lease — Admit(42)=nil with nobody owning 42 in Redis

④ --- FAIL: TestProbeW08c_ClaimAnswersOursWhenItsExtendDidNotApply
    --- FAIL: …/lapsed
    zz_leasesm_probe_test.go:48: I3 violated: Claim answered ours ({SID:1 Token:self}) although its CompareAndExpire did not apply; the shared table now says {SID:0 Token:}
    --- FAIL: …/lapsed-and-taken
    zz_leasesm_probe_test.go:48: I3 violated: Claim answered ours ({SID:1 Token:self}) although its CompareAndExpire did not apply; the shared table now says {SID:2 Token:other}

⑤ --- FAIL: TestProbeN_OwnsAcrossAGapDropsAConnectedPlayersCopyWithoutClosingTheSession
    zz_leasesm_probe_test.go:183: dropped=[42] closed=[] active=1
    zz_leasesm_probe_test.go:185: I6 violated: Owns claimed player 42 across a gap and dropped their copy (scene left first), but the player's session is still open (closed=[]): connected and detached from the scene, the RR-20260930-23 symptom through the Owns path

⑥ --- FAIL: TestProbeN_ALoginThatDecidedContinuousBeforeTheInterruptionClearsIt
2026/10/04 22:51:18 WARN player owners: lease had lapsed and was retaken; the stale copy was dropped player_id=42 sessions_closed=1
    zz_leasesm_probe_test.go:245: login: mine=true err=<nil> dropped=[] closed=[42]
    zz_leasesm_probe_test.go:247: RR-20260920-09 rule violated: Redis told this process the lease on 42 was not its own (interrupted), two claims then re-took it — the login's and the refresh pass's — and neither dropped the copy from before the gap (dropped=[]); Admit(42)=nil

⑦ --- FAIL: TestProbeN_AClaimToldAnotherOwnsThePlayerLeavesAdmissionOpen
    zz_leasesm_probe_test.go:266: I1 violated: Claim was just told sid 2000 owns player 42 and answered mine=false, yet Admit(42)=nil until the next renewal notices
```

（日志里 `lease renewal failed` 的 WARN 是 `runOutTheWindow` 构造前置条件时产生的，这里省略了；完整输出见附录 A 的复跑命令。）

### 4.4 W-08 三处的结论

1. **第 1 处（撤离以错误结束、副本仍常驻时的确认路径）：确认违例（I5）。** 根因是两件事同时成立：续租的 Held 分不出键是哪一次认领设的（token 按进程固定）；`abandon` 用“此刻能不能服务”来判断“这份状态是不是我这次认领的”。RR-20261004-11 让撤离进行中 `Admit` 拒绝，所以只剩“撤离以错误结束”这一支，正好是 RR-14 记录写的那一支。补洞的话，最小改法是：跨间断认领一开始（不是等到报错）就把已有状态标成 `interrupted`，让续租不能确认它。
2. **第 2 处（`Refresh` 与 `abandon` / 公开 `Release` 并发时 `confirmLocked` 重建状态）：两种都确认违例。** abandon 那种违反 I5，`Release` 那种违反 I1（后者目前没有调用方，是 API 层的隐患）。根因是 `confirmLocked` 按玩家 id 插入或更新（`po:924-928`），而续租的结论来自一份更早的 `snapshot()`。最小改法：`confirmRenewal` 遇到状态不存在就不确认。
3. **第 3 处（`Store.Claim` 在 CAE `applied=false` 时仍回答“是我们的”）：确认违例（I3；`lapsed-and-taken` 那种是两个进程同时认为自己持有，违反 I2）。** 根因在 `pr:239`：丢掉了 `applied`；而且 SetNX、Get、CAE 是三次往返。最小改法：`applied=false` 时再 SetNX 一次，或者回答“不是我们的”。根治办法是用 core 已有的 `fredis.CompareAndSet`（`redis/cas.go:105`，`Expected=nil` 表示“键必须不存在”，失败时把 `Current` 带回来），一次原子操作就能拿到明确结论（见 §5.4）。

---

## 5. 目标状态机（to-be）

### 5.1 结构原则

1. **每个玩家一条记录**：`record{phase, gen, token, validUntil, lastUsed, done, then}`。取代 `local` / `evictions` / `claiming` / `handingBack` / `interrupted` / `lost` 六处分散的状态。撤离仍然是独立 goroutine，但它属于这条记录（`phase=Dropping`），不再放在另一张表里。
2. **世代号 `gen`**：每次进入一个 phase，`gen` 就加一。凡是在锁外完成的结果（`Refresh` 的回复、认领的回复、撤离结束、归还的每一步），都带着发起时的 `(pid, gen)` 回来；回来时记录的 `gen` 已经变了，这个结果就作废，只记日志。不再有“按玩家 id 插入或更新”：没有 `gen` 的写入只有一种，就是 `Absent → Claiming`。
3. **同一玩家同一时刻至多一个“所有权操作”**：`Claiming`、`Dropping`、`HandingBack` 三者互斥。后来的调用方在锁外等这次操作的 `done`（受自己的 ctx / `evictWait` 约束），等完重新判断。刷新循环**从不加入**别人发起的操作，遇到就跳过这一轮。
4. **跨间断 = 扔副本 + 关连接，由同一个转换完成**：进入 `Dropping{serve|forget}` 时关掉该玩家在本进程的连接。只有登录自己那一条会话例外，需要传会话 id 给 fencer（§8 D4）。`retakeOne` 里专门关连接的那段代码因此可以删掉，`Owns` 路径（N-OWNS）也被这条转换一并覆盖。
5. **Redis：每次认领一个 token**：值仍是 `<sid>:<token>`，但 token = 进程 token + 本次认领序号，例如 `a1b2c3d4e5f60718.17`。只有本条记录的那次认领会设这个值，而且只设一次。所以续租答 `Held(token)` 就说明**这个值从设下起一直在**，本身就是连续性的证明，不需要再由本地状态推断（RR-14、W08-1 在协议层就不会出现）。认领改成一次 CAS，结论分四种（§5.4）。

### 5.2 状态枚举

| phase | 含义 | `Admit` | 刷新循环会续它吗 | 记录保存的 |
| --- | --- | --- | --- | --- |
| `Absent` | 没有记录 | 拒 | 否 | — |
| `Claiming` | 一次**跨间断**认领在问 Redis | 拒 | 否 | 新 token `k'`、`asked`、`done` |
| `Dropping` | 撤离进行中；`then` 决定撤离之后去哪里：`serve`（跨间断认领拿到了，有发起者在等）/ `resume`（无间断，归还的撤离等超时）/ `forget`（围栏） | 拒 | 只有 `then=resume` 时续（token 连续） | token、`then`、`done` |
| `Serving` | 确认过的租约；副本自 token 设下以来一直连续 | `now < validUntil − guard` 时放行，并写 `lastUsed` | 是 | token `k`、`validUntil` |
| `Interrupted` | Redis 已经答过“`k` 不在了”，或者认领没收到回复（待领回的 `k'`）；副本可疑 | 拒 | 否（没有可续的 token）；刷新循环会在预算内重新认领 | 可能有 `pending k'` |
| `HandingBack` | 闲置归还：撤离 → 投影 → 释放 | 拒 | 同一 goroutine，碰不上 | token、步骤、`done` |
| `Abandoned` | 跨间断认领拿到了，但副本扔不掉（撤离失败，或发起者等不下去了）；撤离可能还在跑 | 拒 | **否**（沿用 RR-07 的语义：键自然过期） | token `k'`（供以后领回）、撤离句柄 |

窗口已经跑完的 `Serving`（续租一直 Err）不另设状态：`Admit` 按时间拒绝；`Claim` 把它当作跨间断。续租答 `Held(k)` 时可以直接确认，因为 per-claim token 下 Held 本身就证明了连续。这同时关掉了 RR-14 记录里“窗口跑完的 Held 一律重新认领会让长时间 Redis 故障变成所有人撤离重载”的顾虑。

### 5.3 转换表

记号：`[锁]` 表示在 `mu` 内一次完成，并且 `gen++`；`gen✓` 表示要求记录的 `gen` 仍等于发起时的值，否则作废。等待一律在锁外。

| # | 从 → 到 | 触发者 | 守卫 | Redis 动作 | 锁 / 等待 | 时间 |
| --- | --- | --- | --- | --- | --- | --- |
| S1 | `Absent` / `Interrupted` / `Abandoned` / `Serving`（窗口已尽）→ `Claiming` | 登录 / `Owns` / 刷新循环重新认领 | 没有进行中的操作。有的话：登录 / `Owns` 等它的 `done`；刷新循环跳过 | — | `[锁]`；铸造 `k'`，记 `asked` | — |
| S2 | `Claiming` → `Dropping{serve}` + 关连接（登录自己那条除外） | 同一调用方 | `Take` 结论是 `Taken` 或 `Adopted` | `Take(k')`（§5.4，一次或两次原子操作） | `gen✓`，然后 `[锁]`；关连接在锁外 | 往返不计入预算 |
| S3 | `Claiming` → `Absent`（之前没有副本）或 `Dropping{forget}` + 关全部连接（之前有记录） | 同上 | `Take` 结论是 `Other(sid)` | — | `gen✓ [锁]` | — |
| S4 | `Claiming` → `Interrupted{pending k'}` | 同上 | `Take` 报错（结果未知） | — | `gen✓ [锁]`；返回 err | — |
| S5 | `Dropping{serve}` → `Serving(k', asked+Lease)` | 撤离结束（成功），并且发起者还在等 | `gen✓` | — | 撤离 goroutine 结束后由**等待者**在锁内应用；返回 `mine=true` | 等待受 `evictWait`、调用方 ctx、回合 ctx 约束 |
| S6 | `Dropping{serve}` → `Abandoned` | 撤离以错误结束，或发起者等不下去了 | `gen✓` | 不释放（键自然过期） | `[锁]`；返回 `errStaleCopyKept` | — |
| S7 | `Serving(k)` → `Serving(k)` | 续租 `Held(k)`，`asked` | `gen✓`，并且 token 仍是 `k` | `CAS(k→k, TTL)`，也就是现在的 `Refresh` | `[锁]` 但不加 `gen`：`validUntil = max(validUntil, asked+Lease)` | Refresh 往返 |
| S8 | `Serving(k)` → `Serving(k)`（连续认领） | 登录 / `Owns` | 窗口开着 | `CAS(k→k, TTL)`：applied | `gen✓`；`validUntil = max(…)`，`lastUsed=now`；`mine=true` | 一次往返 |
| S9 | `Serving(k)` → `Interrupted` | 续租答 `NotHeld(k)`，或连续认领的 CAS 没生效 | `gen✓` | — | `[锁]`。如果是认领看到 `Current=missing`，在同一次调用里接着走 S1；`Current=other` 就走 S3（N-TOLD、N-CONT、W08-3 的连续那一支都由这一条关掉） | — |
| S10 | `Serving` → `HandingBack` | 刷新循环 | 闲置 ≥ `HandBackIdle`，没有进行中的操作，回合还有预算 | — | `[锁]` | 回合预算 |
| S11 | `HandingBack` → `Absent` | 刷新循环 | 没有连接；撤离成功；投影成功；`gen✓` | `CompareAndDelete(k)`（不计入预算） | 结束时 `[锁]` 并关 `done` | 回合预算 + 一次往返 |
| S12 | `HandingBack` → `Serving(k)`（窗口不变） | 刷新循环 | 有连接、投影失败、或预算用完（撤离还没开始，或已经结束） | — | `[锁]` | — |
| S13 | `HandingBack` → `Dropping{resume}` | 刷新循环 | 撤离等超时（撤离继续） | — | `[锁]` | — |
| S14 | `Dropping{resume}` → `Serving(k)` | 撤离 goroutine | 撤离结束（成败都算），`gen✓` | — | `[锁]`（RR-11：租约无间断，副本没了就冷加载，失败就照常服务） | — |
| S15 | `Interrupted` → `Dropping{forget}` + 关全部连接 → `Absent` | 刷新循环或认领看到 `Other` | — | 不碰 Redis | `[锁]`；撤离异步，结束时 `gen✓` 才删记录 | — |
| S16 | `Serving` / `Dropping{resume}` → `Absent` | 公开 `Release`（销毁） | — | `CompareAndDelete(k)` | `[锁]`。之后到达的续租回复因 `gen` 已变而作废（W08-2b 由此关掉） | — |
| S17 | `Admit` | 写闸 / `Owns` / `OwnedHere` | `phase==Serving ∧ now < validUntil − guard` | — | 锁内，不阻塞 | — |

**等待图（无环）**：登录 / 消费者 → 别人的 `Claiming` / `Dropping`（受 `evictWait`、ctx 约束）或刷新循环的 `HandingBack`（受回合预算约束）。刷新循环只等自己发起的操作（受回合 ctx 约束）。撤离 goroutine → 实体锁 → 事务；事务只调用不阻塞的 `Admit`，不会反过来等 owners。没有任何一方等登录或消费者，刷新循环也不等别人，所以图里没有环（I4、I11）。

### 5.4 Redis 侧协议

| 操作 | 现状 | to-be | 结论 |
| --- | --- | --- | --- |
| 跨间断认领 `Take(k')` | `SetNX` → `Get` → 是自己的就 `CAE`，并且不看 applied（`pr:216-244`，三次往返，中间有空隙） | `CompareAndSet{Expected:nil, Next:k', TTL}`（键不存在才写）。没生效时用返回的 `Current` 判断：`Current` 是本记录的 `pending k`，或者带本进程前缀的孤儿键（§8 D8），就 `CompareAndSet{Expected:Current, Next:k'}` 领回来；否则就是 `Other` | `Taken` / `Adopted` / `Other(sid)` / `Unknown(err)` |
| 连续认领、续租 | `CAE(进程 token)` | `CAE(k)`，也就是 `CompareAndSet{Expected:k, Next:k, TTL}` | `Held` / `NotHeld(Current)` / `Unknown` |
| 释放 | `CompareAndDelete(进程 token)` | `CompareAndDelete(k)` | — |
| 路由 / 只读 | `Get` | 不变 | 提示信息，不作为任何写入的依据 |

- **持久格式**：Redis 值的形状 `<sid>:<token>` 不变，`decodeRoute`（`pr:76`）不用改。新旧进程混跑时，两边的 token 本来就互不相等，没有兼容问题。不需要新的键，也不需要永不过期的计数器。
- **RR-20260920-09 当时没采用的方案**（每次 Claim 用 `INCR` 拿一个全局单调世代号）：per-claim token 已经回答了“这个键是不是我这一次设的”。全局世代号多回答一个问题：“中间有没有别人持有过”。它只有在被**带进持久化写路径**（Mongo CAS 校验 owner 世代）时才有新的价值，那就是 §8 D3，超出 demo 的范围。
- **`Claim` 的 `applied=false` 语义**：to-be 里 `mine=true` 只能来自 `Taken`、`Adopted` 或 CAS `applied=true`，然后经过 S5 或 S8。从结构上就不存在“没生效却回答是我们的”。
- **token 按进程固定的问题**（RR-20260920-03 修法的副作用，RR-14 的根）：per-claim token 把它消掉了。进程 token 仍然保留为前缀，所以同 sid 重启的新实例仍然算另一个主人（`TestARestartOnTheSameSidIsADifferentOwner` 保持成立）。

### 5.5 不变量如何由构造保证

| 不变量 | to-be 的构造 |
| --- | --- |
| I1 | 只有 S5、S7、S8 写 `validUntil`。三者都要求一次带本记录 token 的原子操作已经生效，而且都检查 `gen`。S16 之后到达的续租回复会被丢掉 |
| I2 | I1 + 键唯一 + A1 + A2（与现状相同的假设，不再多出别的漏洞） |
| I3 | `mine=true` 只在 S5、S8 之后返回，返回时 `phase==Serving`。归还的 S11 要求 `gen✓`，而认领在 `HandingBack` 期间会等（S1 的守卫） |
| I4 | 回合结构不变（续租 → 重新认领 → 归还，共用一份预算），而且刷新循环不加入别人的操作 |
| I5 | 进入 `Serving` 只有三个入口：S5（刚扔掉副本）、S7/S8（同一个 token 连续，自身就是连续性证明）、S12/S14（token 从没断过）。`Interrupted`、`Abandoned`、`Absent` 都只能经 S1→S2→S5 回到服务 |
| I6 | 只有 S2、S15 会因为间断扔副本，它们进入时就关连接（S2 放过登录自己那一条）。S13/S14 走的是归还，前提是没有连接 |
| I7 | `HandingBack` 是一个 phase，认领会等它；释放前要 `gen✓` |
| I8 | S11 的步骤顺序与 RR-26-31 相同 |
| I9 | S7 不写 `lastUsed`；S5、S8、S17 写 |
| I10 | `Dropping` 只属于一条记录，发起者和加入者等的是同一个 `done` |
| I11 | 每条转换的锁内部分只是赋值，等待一律在锁外；`Admit` 仍然只是一次本地读 |

---

## 6. 对照与迁移

### 6.1 现有补丁在 to-be 里的去向

| 现有符号 / 补丁 | to-be | 去留 |
| --- | --- | --- |
| `leaseState.lost` | — | 删（已经没用） |
| `interrupted`（RR-09） | `phase=Interrupted` | 合并进 phase |
| `handingBack` / `handBackDone`（RR-11、RR-03） | `phase=HandingBack` + `done` | 合并进 phase |
| `claiming` 计数、`enterClaim` / `leaveClaim`（RR-03） | 操作互斥（S1 的守卫）+ `done` | 删，语义由 phase 承担 |
| `evictions` 表（RR-12）与 `Admit` 里查撤离的那条（RR-11） | `phase=Dropping` | 单航班保留，归到记录名下 |
| `continuous := Admit()==nil && !waited`（RR-09、03、11） | S1 的守卫在锁内一次性判断，后面每一步都 `gen✓` | 改写；N-CONT 由此关掉 |
| `abandon` + `refusalLocked` 条件删除（RR-07） | S6 → `Abandoned`（不续） | 删；`refusalLocked` 的拒绝文本保留在 S17 |
| `markUnansweredClaim`（RR-14） | S4 → `Interrupted{pending}` | 删 |
| `confirmRenewal` 遇上 `interrupted` 返回 false（RR-14） | S7 只续 `Serving`、`Dropping{resume}`，而且 Held 只针对本记录的 token | 删 |
| `confirmLocked` 按玩家 id 插入或更新 | S5、S7、S8 各自 `gen✓` | 删；W08-1、W08-2 由此关掉 |
| `asked` 起算窗口（RR-14） | 原样保留 | 保留 |
| `renew` 两段 + `refreshPass` 预算（RR-04、10） | 原样保留 | 保留 |
| `retakeOne` 的四个结局 + 关连接（RR-23） | S1–S6 + S15；关连接移到 S2、S15 | 改写；N-OWNS 由此关掉 |
| `handBackOne` 的步骤与 `stillOurs`（RR-03、26-31） | S10–S14；`stillOurs` 变成 `gen✓` | 改写 |
| `fence`（RR-04、09） | S15 | 改写 |
| `Store.Claim` 三步（RR-03） | `Take` / CAS（§5.4） | 改写；W08-3 由此关掉 |
| `Store` 进程 token（RR-03） | 进程前缀 + 每次认领的序号 | 改写 |
| 外部接口：`Claim`、`Owns`、`OwnedHere`、`OwnerSID`、`SID`、`Routes`、`Admit`、`Admitted`、`AdmitMessage`、`Release`、`Evict`、`Fence`、`Projections`、`Start`、`Stop`、`NewPlayerOwners`、`ErrLeaseNotHeld`、`AdmissionGuard`、`HandBackIdle` | 不变 | 保留（调用方：`eg:61,71`、`gift_saga.go.tmpl:128,148-149,284,395,400`、`matchmaker.go.tmpl:64,115`、`service.go.tmpl:123-210,275`、`controller.go.tmpl:97-100`） |

### 6.2 回归的去留（承诺不降低）

`playerowner_test.go.tmpl` 一共 35 个顶层用例、13 个子测试；`playerroute_test.go.tmpl` 10 个。

- **原样保留（只走公开接口，或者只用 `renew` / fake 表 / fake fencer）**：`TestALostLeaseFencesThePlayer`、`TestAnUnknownRenewalRunsAdmissionOutInsteadOfPretending`、`TestAdmissionStopsOneGuardBandBeforeTheDeadline`、`TestASidMatchWithoutAConfirmedLeaseIsNotOwnership`、`TestTheWriteGateExemptsLoginAndRefusesEverythingElse`、`TestALapsedLeaseNobodyElseTookIsRetakenInsteadOfFenced`、`TestARetakenLeaseClosesTheSessionsThatLivedThroughTheGap`、`TestARetakeWhoseCopyCannotBeDroppedStillClosesTheSessions`、`TestAClaimAtLoginDoesNotCloseTheConnectionThatIsLoggingIn`、`TestALeaseThatWasInterruptedDropsTheResidentCopy`、`TestAClaimWhoseCopyCannotBeDroppedIsNotTakenIntoService`、`TestAnUninterruptedLeaseKeepsItsPlayer`、`TestFencingAlsoDropsTheCopy`、`TestABackgroundClaimIsHandedBackWhenNobodyIsPlaying`、`TestAPlayerWithASessionIsNeverHandedBack`、`TestWorkInProgressKeepsTheLease`、`TestAHandBackThatCannotDropTheCopyKeepsTheLease`、`TestALeaseRetakenForNewWorkIsNotIdle`、`TestRenewalsDoNotCountAsUse`、`TestAHandBackInFlightAdmitsNothing`、`TestAFailedHandBackPutsTheLeaseBackInService`、`TestAHandBackPassHoldsTheRefreshLoopLessThanTheLeaseAllows`、`TestTheHandBackPassBudgetFitsInsideTheLease`，以及 playerroute 全部 10 个用例（`TestARestartOnTheSameSidIsADifferentOwner` 的含义变成“进程前缀不同”，断言不变）。
- **断言保留，只换掉直接读内部字段的写法**（改用测试辅助函数 `phaseOf` / `validUntilOf` / `dropRunning`）：`TestAClaimWaitsAtMostTheBudgetForABusyEntity`、`TestASecondDropJoinsTheOneAlreadyRunning`、`TestOneBusyEntityDoesNotHoldUpTheRefreshLoop`、`TestAHandBackWaitsForThePlayersWritesToReachTheDatabase`、`TestAClaimDuringAHandBackNeverEndsWithAnUnownedPlayer`（4 个子测试；`onHandBackWait` 钩子换成“等某个 phase 的 `done`”）、`TestAHandBackPassStopsWaitingWhenItsBudgetRunsOut`、`TestAHandBackWhoseDropOutlivesItsWaitAdmitsNothingUntilTheDropEnds`（2 个子测试：`login` 子测试里的 `owners.local[77] != nil` 改成 `phase==Dropping{resume}`）、`TestARefreshPassRetakingLostLeasesHoldsTheLoopLessThanTheLeaseAllows`（`local[...]` 改成 `phase==Interrupted`）、`TestARefreshPassStopsWaitingForRetakesWhenItsBudgetRunsOut`、`TestAdmissionStopsAGuardBeforeTheKeyRedisSetCanExpire`（4 个子测试，`validUntil` 改用辅助函数读取）。
- **要改写 harness、承诺不变**：
  - `TestAClaimWhoseCopyCannotBeDroppedIsNotRevivedByTheNextRenewal`：“不续、不确认、`Lease` 之后仍然拒绝、撤离结束后的下一次 `Claim` 回到服务”全部保留（`Abandoned` 不续）。
  - `TestAClaimRedisDidNotAnswerIsNotConfirmedByTheNextRenewal`（3 个子测试）：`claimHookTable` 要能模拟“落地但没收到回复”。`fakeLeaseTable` 需要升级成按值（sid + token）存储，才能表达 per-claim token；“下一轮续租不确认、扔了副本才放行”的断言不变，在 to-be 里由 S4 + S1 + S5 达成。
  - 整个 `fakeLeaseTable`：`owner map[int64]int32` 改成 `map[int64]playerroute.Route`，`Claim` 返回 `Take` 的结论。所有用 `table.owner[42] = 2000`、`delete(table.owner, 42)` 构造场景的用例，语义都不变。
- **新增（由本方案的探针转正）**：W08-1、W08-2a、W08-2b、W08-3（2 个子测试）、N-OWNS、N-CONT、N-TOLD，共 7 个用例、8 个场景（附录 A），断言原样。

---

## 7. 决策建议

### 7.1 三个选项

| | A. 重写核心（推荐） | B. 保留并补洞 | C. 折中：先抽出显式状态 |
| --- | --- | --- | --- |
| 做什么 | §5 整体落地：记录 + phase + `gen` + per-claim token + `Take` CAS，外部接口不变，回归按 §6.2 迁移 | 在现有代码上补 7 处（见下） | 引入 `phase` 枚举与 `gen`，现有各段代码改成通过 `transition(pid, gen, to)` 改状态，并且每个异步结果 `gen✓`；Redis 先只修 `Store.Claim` 的 CAS，token 暂不改 |
| 关掉 | 7 条 RR 的全部交错 + W-08 三处 + N-OWNS / N-CONT / N-TOLD，全部由构造保证 | 已知的 8 条 | W08-1、W08-2、W08-3、N-CONT、N-TOLD；N-OWNS 要另外补；RR-14 那一类（Held 分不出键的来历）仍然靠 `markUnansweredClaim` / `confirmRenewal` 补丁 |
| 工作量（agent 日） | 实现 1.5 + 回归迁移 1 + 组合复核与生成工程验证 0.5–1 ≈ **3–4** | **约 1**（每处几行 + 一条回归） | 约 2 |
| 风险 | 中：一次替换 1400 行的核心；靠 §6.2 的 58 条现有回归（35 + 13 + 10） + 8 条新回归 + 原样保留的外部接口来压 | 低（单处）/ **高（整体）**：每一处又是“在某个位置再看一眼标志位”，这正是今天连修 7 条的模式；W08-1 的补法（认领一开始就标 `interrupted`）会让刷新循环加入登录的撤离，影响 RR-10 的预算推导，需要重新论证 | 中：两套写法并存一段时间 |
| 对已生成工程 | 一次手工合并（`playerowner.go`、`playerroute.go` 和两份测试整体替换）。外部接口不变，调用方不用动 | 每补一处就要再手工合并一次，而这些文件今天已经合并了 7 次 | 两次合并（C 一次，之后 A 再一次） |

B 要补的 7 处（如果维护者选 B）：
1. W08-1：跨间断认领一开始就把已有状态置 `interrupted`。
2. W08-2：`confirmRenewal` 遇到状态不存在就返回不确认。
3. W08-3：`Store.Claim` 在 `applied=false` 时不回答 ours（改 CAS，或者再 SetNX 一次）。
4. N-OWNS：`Owns` 路径跨间断时关连接（要给 `claim` 加一个“调用方是谁”的参数）。
5. N-CONT：`confirmClaim` 前在锁内复核“`continuous` 的判断之后有没有被 `interrupt`”，需要一个计数或世代号，等于 C 的一半。
6. N-TOLD：`Claim` 拿到别的 sid 时 `interrupt` 已有状态。
7. 删掉没用的 `lost`。

### 7.2 推荐：A，分三笔提交

1. `playerroute`：per-claim token、`Take` / `Renew` / `Release` 以 token 为参数、结论显式，加 store 层回归（W08-3 转正）。`leaseTable` 接口随之变化，fake 表升级。
2. `playerowner` 核心替换：§5.3 的转换表照着写（每条转换一个小函数，主流程能顺着读出“判断 → Redis → 撤离 → 确认 → 关连接”，符合 roost-coding §写给人阅读的代码）。外部接口与日志行尽量保留。
3. 回归迁移（§6.2）+ 8 条新回归。修后跑 fix-contract-review 的组合复核：生成工程 `go build` / `vet` / `-race -count=3 ./internal/service/game/`、`go test ./...`，仓库 `GOWORK=off go test -count=1 ./codegen/internal/roost/` 与根包 `GOWORK=off go test -count=1 .`。

理由：
- 8 条新违例里有 6 条属于同一个结构缺口：没有世代号，结论按玩家 id 生效；另外 2 条来自 Redis 回复信息不全。这两个缺口补一处只能挡住一种时序，只有改结构才能一次关掉。
- 今天的 7 条修复都以“每修一条，邻近分支就暴露更窄的交错”收尾（RR-07、11、14 记录的 §未验证项一条接一条）。再选 B，等于把同样的循环再走一遍。
- 外部接口不变，调用方（登录、赠礼 saga、matchmaker、装配）都不用改。
- 已生成工程的迁移代价，A 不比 B 高：两者都要手工合并同一组文件，A 只合并一次。

---

## 8. 未知与需要维护者决定的点

| # | 问题 | 选项 | 本方案的倾向 |
| --- | --- | --- | --- |
| D1 | 重写 / 补洞 / 折中 | §7.1 | A |
| D2 | Redis 值改成 per-claim token（格式 `<sid>:<token>` 不变） | 接受 / 只修 `Store.Claim`、token 维持按进程 | 接受 |
| D3 | 持久化层围栏：把 owner 世代号带进 DataEngine 的 Mongo CAS，类似 Remote 的 ownership / grant / fence。这是唯一能关掉 A1（Redis 提前丢键、failover 丢写）和 A2（长事务越过保护带）的办法 | 不做（维持“Redis 是协调、不是锁”，`pr:18-23`）/ 并入 CARRYOVER A6、C24 跨进程移交的 feature 线 | 不在本次重写范围；登记到 feature 线 |
| D4 | 跨间断时关连接要放过登录自己那一条：需要生成的 TCP runtime 提供 `CloseSessionsExcept(playerID, sessionID, reason)`（改 `codegen/internal/roost/render_player_tcp.go`；`server_gen.go` 是生成物，`project sync` 会更新），否则多设备登录时旧设备会话会脱离场景（N-OWNS′） | 加接口 / 接受多会话的这个缺口，只给 `Owns` 和刷新循环关连接 | 加接口 |
| D5 | 跨间断认领的撤离比发起者等得久时 | 维持 RR-07：`Abandoned`，不续，自然过期，下一次认领才恢复 / 改成撤离结束自动回到服务（期间继续续租） | 维持 RR-07（不改承诺、不改回归） |
| D6 | 认领被告知“别人持有”（N-TOLD） | 只置 `Interrupted`、由刷新循环围栏 / 当场围栏（S15） | 当场 S15。登录本来就会失败，关连接不多付代价 |
| D7 | 在途事务计数（`Admit` 配对 `Done`，CARRYOVER A7），替代 A3 的经验值 | 不做 / 一起做 | 不做（写路径契约变更，另列） |
| D8 | 没有本地记录时，是否领回带本进程前缀的孤儿键（S1/S2 的 `Adopted`），省掉最长 30s 的 `Owns=false` | 领回 / 只领回本记录的 `pending` | 只领回 `pending`。带前缀的孤儿键作为可选优化，单独评审 |
| D9 | 已生成工程的迁移：demo 文件归应用所有，`project sync` 不更新。是否把所有权表搬进 kit（例如 `kit/playerowner`），以后的修复随模块升级下发 | 维持模板 / 搬进 kit | 先按模板重写；搬进 kit 另立方案。这会改变“demo 是应用代码”的定位，属于维护者的决定 |
| D10 | 公开 `PlayerOwners.Release` 没有调用方，却是 W08-2b 的入口 | 保留，按 S16 重写 / 删掉 | 保留，按 S16 重写（注释写的“destroy、运维迁移”用途仍然成立） |

**仍然未知、本方案没有验证的**：
- 没有两进程、真实 Redis 的端到端演练。7 个探针都是单进程替身 + 确定性交错。W08-3 与 N-A1 的真实触发率取决于 Redis 往返时延和 failover 行为。
- §5 的 to-be 只是设计，没有实现，也没有性能对照。`Admit` 仍然只是一次锁内读，预计不受影响，但没有量过。
- 生成的 TCP 服务里同一玩家多 SessionID 的实际使用情况（demo 客户端会不会出现）没有核实，所以 N-OWNS′ 的现实影响面未知。

---

## 附录 A：探针源码（scratchpad 生成工程，不入库）

复跑：按 §4.1 生成工程，把下面两段分别放进 `internal/service/game/zz_leasesm_probe_test.go` 和 `game/playerroute/zz_leasesm_probe_test.go`（生成工程的包名是 `Game`，import 路径是 `example.com/planet/...`），然后执行 `GOWORK=off go test -race -count=20 -run TestProbe ./internal/service/game/ ./game/playerroute/`。

<details><summary><code>internal/service/game/zz_leasesm_probe_test.go</code></summary>

```go
package Game

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/planet/game/playerroute"
)

// runOutTheWindow makes every renewal fail for longer than the lease: the
// local window runs out and the key lapses in Redis. The state stays in
// local, not interrupted.
func runOutTheWindow(t *testing.T, owners *PlayerOwners, table *fakeLeaseTable, clock *time.Time, playerID int64) {
	t.Helper()
	table.refresh = func(id int64) playerroute.RefreshResult {
		return playerroute.RefreshResult{PlayerID: id, Err: errors.New("redis unreachable")}
	}
	for i := 0; i < 4; i++ {
		*clock = clock.Add(playerroute.RefreshInterval)
		owners.renew(context.Background())
	}
	delete(table.owner, playerID)
	table.refresh = nil
	owners.mu.Lock()
	state := owners.local[playerID]
	owners.mu.Unlock()
	if state == nil || state.interrupted {
		t.Fatalf("precondition: want local state, not interrupted: %+v", state)
	}
	if err := owners.Admit(playerID); err == nil {
		t.Fatal("precondition: the window should have run out")
	}
}

// W-08 (1)
func TestProbeW08a_RenewalConfirmsAClaimWhoseDropThenFails(t *testing.T) {
	owners, table, fencer, clock := ownersUnderTest(t)
	ctx := context.Background()
	if mine, err := owners.Claim(ctx, 42); !mine || err != nil {
		t.Fatalf("claim: %v %v", mine, err)
	}
	fencer.connect(42) // logged in, so no hand-back picks the player
	runOutTheWindow(t, owners, table, clock, 42)

	dropStarted := make(chan struct{})
	endDrop := make(chan error)
	var once sync.Once
	owners.Evict(evictorFunc(func(context.Context, int64) error {
		once.Do(func() { close(dropStarted) })
		return <-endDrop
	}))
	answered := claimInBackground(ctx, owners, 42)
	<-dropStarted
	// The next heartbeat lands while the login is waiting for the drop.
	*clock = clock.Add(time.Second)
	owners.renew(ctx)
	endDrop <- errors.New("the entity is busy; Destroy failed")
	got := <-answered
	waitDropEnded(t, owners, 42)

	t.Logf("login: mine=%v err=%v", got.mine, got.err)
	if err := owners.Admit(42); err == nil {
		t.Fatalf("I5 violated: the login's drop of the copy from before the gap failed (the copy is still resident), yet Admit(42)=nil — the renewal that landed during the drop confirmed the key the login's SetNX re-created, and abandon then saw a serviceable lease and kept it")
	}
}

// W-08 (2a)
func TestProbeW08b_RenewalRecreatesStateThatAbandonDeleted(t *testing.T) {
	owners, table, fencer, clock := ownersUnderTest(t)
	ctx := context.Background()
	if mine, err := owners.Claim(ctx, 42); !mine || err != nil {
		t.Fatalf("claim: %v %v", mine, err)
	}
	fencer.connect(42)
	runOutTheWindow(t, owners, table, clock, 42)

	atRefresh := make(chan struct{})
	letRefresh := make(chan struct{})
	table.refresh = func(id int64) playerroute.RefreshResult {
		if id == 42 {
			close(atRefresh)
			<-letRefresh
		}
		// Evaluated after the login's SetNX: the key is ours again.
		return playerroute.RefreshResult{PlayerID: id, Held: table.owner[id] == table.sid}
	}
	*clock = clock.Add(time.Second)
	renewed := make(chan struct{})
	go func() { defer close(renewed); owners.renew(ctx) }()
	<-atRefresh

	owners.Evict(&evictorStub{fail: errors.New("the entity is busy; Destroy failed")})
	mine, err := owners.Claim(ctx, 42)
	owners.mu.Lock()
	abandoned := owners.local[42] == nil
	owners.mu.Unlock()
	t.Logf("login: mine=%v err=%v abandoned=%v", mine, err, abandoned)
	close(letRefresh)
	<-renewed

	if err := owners.Admit(42); err == nil {
		t.Fatalf("I5 violated: abandon forgot player 42 (its copy from before the gap could not be dropped, abandoned=%v), the in-flight renewal answered Held and confirmLocked created a new lease state — Admit(42)=nil on the stale copy", abandoned)
	}
}

// W-08 (2b)
func TestProbeW08b_RenewalRecreatesStateThatReleaseDeleted(t *testing.T) {
	owners, table, fencer, clock := ownersUnderTest(t)
	ctx := context.Background()
	if mine, err := owners.Claim(ctx, 42); !mine || err != nil {
		t.Fatalf("claim: %v %v", mine, err)
	}
	fencer.connect(42)
	atRefresh := make(chan struct{})
	letRefresh := make(chan struct{})
	table.refresh = func(id int64) playerroute.RefreshResult {
		held := table.owner[id] == table.sid // the CompareAndExpire lands now
		if id == 42 {
			close(atRefresh)
			<-letRefresh // and its reply arrives later
		}
		return playerroute.RefreshResult{PlayerID: id, Held: held}
	}
	*clock = clock.Add(playerroute.RefreshInterval)
	renewed := make(chan struct{})
	go func() { defer close(renewed); owners.renew(ctx) }()
	<-atRefresh
	owners.Release(ctx, 42)
	close(letRefresh)
	<-renewed

	_, held := table.owner[42]
	if err := owners.Admit(42); err == nil && !held {
		t.Fatalf("I1 violated: Release gave player 42 up (shared table owner present=%v), the renewal in flight answered Held and confirmLocked recreated the lease — Admit(42)=nil with nobody owning 42 in Redis", held)
	}
}

// N-OWNS
func TestProbeN_OwnsAcrossAGapDropsAConnectedPlayersCopyWithoutClosingTheSession(t *testing.T) {
	owners, table, fencer, clock := ownersUnderTest(t)
	evictor := &evictorStub{}
	owners.Evict(evictor)
	ctx := context.Background()
	if mine, err := owners.Claim(ctx, 42); !mine || err != nil {
		t.Fatalf("claim: %v %v", mine, err)
	}
	fencer.connect(42)
	evictor.forget()
	runOutTheWindow(t, owners, table, clock, 42)

	// Redis is back; a gift step for this (online) sender arrives.
	owned, err := owners.Owns(ctx, 42)
	if !owned || err != nil {
		t.Fatalf("precondition: owns = %v %v", owned, err)
	}
	// And the following heartbeat changes nothing about it.
	*clock = clock.Add(playerroute.RefreshInterval)
	owners.renew(ctx)
	dropped := evictor.taken()
	t.Logf("dropped=%v closed=%v active=%d", dropped, fencer.closed, fencer.ActiveSessions(42))
	if len(dropped) == 1 && fencer.ActiveSessions(42) > 0 {
		t.Fatalf("I6 violated: Owns claimed player 42 across a gap and dropped their copy (scene left first), but the player's session is still open (closed=%v): connected and detached from the scene, the RR-20260930-23 symptom through the Owns path", fencer.closed)
	}
}

// N-CONT
func TestProbeN_ALoginThatDecidedContinuousBeforeTheInterruptionClearsIt(t *testing.T) {
	table := newFakeLeaseTable(1000)
	fencer := &fakeFencer{}
	clock := time.Unix(1_700_000_000, 0)
	var armed atomic.Bool
	var nowCalls atomic.Int32
	releaseLogin := make(chan struct{})
	loginFinished := make(chan struct{})
	now := func() time.Time {
		if armed.Load() && nowCalls.Add(1) == 2 { // renew: asked, then beginPass
			close(releaseLogin)
			<-loginFinished
		}
		return clock
	}
	var blockNext atomic.Bool
	atClaim := make(chan struct{})
	hooked := claimHookTable{fakeLeaseTable: table, before: func(int64) error {
		if blockNext.CompareAndSwap(true, false) {
			close(atClaim)
			<-releaseLogin
		}
		return nil
	}}
	owners := newPlayerOwners(hooked, fencer, now)
	evictor := &evictorStub{}
	owners.Evict(evictor)
	owners.Projections(newProjectionStub())
	ctx := context.Background()
	if mine, err := owners.Claim(ctx, 42); !mine || err != nil {
		t.Fatalf("claim: %v %v", mine, err)
	}
	fencer.connect(42)
	evictor.forget()
	// The key vanishes early (failover / flush / operator); nobody takes it.
	delete(table.owner, 42)

	blockNext.Store(true)
	var got claimAnswer
	go func() {
		mine, err := owners.Claim(ctx, 42) // a re-login; Admit still passes
		got = claimAnswer{mine, err}
		close(loginFinished)
	}()
	<-atClaim
	armed.Store(true)
	owners.renew(ctx)

	t.Logf("login: mine=%v err=%v dropped=%v closed=%v", got.mine, got.err, evictor.taken(), fencer.closed)
	if err := owners.Admit(42); err == nil && len(evictor.taken()) == 0 {
		t.Fatalf("RR-20260920-09 rule violated: Redis told this process the lease on 42 was not its own (interrupted), two claims then re-took it — the login's and the refresh pass's — and neither dropped the copy from before the gap (dropped=[]); Admit(42)=nil")
	}
}

// N-TOLD
func TestProbeN_AClaimToldAnotherOwnsThePlayerLeavesAdmissionOpen(t *testing.T) {
	owners, table, _, _ := ownersUnderTest(t)
	ctx := context.Background()
	if mine, err := owners.Claim(ctx, 42); !mine || err != nil {
		t.Fatalf("claim: %v %v", mine, err)
	}
	table.owner[42] = 2000 // the key was lost early and another process took it
	mine, err := owners.Claim(ctx, 42)
	if mine || err != nil {
		t.Fatalf("precondition: claim should report another owner: %v %v", mine, err)
	}
	if err := owners.Admit(42); err == nil {
		t.Fatalf("I1 violated: Claim was just told sid 2000 owns player 42 and answered mine=false, yet Admit(42)=nil until the next renewal notices")
	}
}
```

</details>

<details><summary><code>game/playerroute/zz_leasesm_probe_test.go</code></summary>

```go
package playerroute

import (
	"context"
	"testing"
)

// Store.Claim: SetNX loses (the key is live), Get reads THIS process, and the
// CompareAndExpire that should extend it does not apply because the key lapsed
// in between — or lapsed and another process took it. Claim still answers
// "ours". Invariant I3: mine=true => the shared table names this process.
func TestProbeW08c_ClaimAnswersOursWhenItsExtendDidNotApply(t *testing.T) {
	cases := map[string]func(f *fakeKeyspace, key string){
		"lapsed": func(f *fakeKeyspace, key string) {
			delete(f.values, key)
			delete(f.expiry, key)
		},
		"lapsed-and-taken": func(f *fakeKeyspace, key string) {
			f.setLocked(key, Route{SID: 2, Token: "other"}.encode(), Lease)
		},
	}
	for name, between := range cases {
		t.Run(name, func(t *testing.T) {
			keys := newFakeKeyspace()
			store, err := newStore(keys, "probe", 1, "self")
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if _, err := store.Claim(ctx, 42); err != nil {
				t.Fatal(err)
			}
			keys.mu.Lock()
			keys.beforeCompare = between
			keys.mu.Unlock()
			route, err := store.Claim(ctx, 42)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := store.Owner(ctx, 42)
			if err != nil {
				t.Fatal(err)
			}
			if route == store.self() && owner != store.self() {
				t.Fatalf("I3 violated: Claim answered ours (%+v) although its CompareAndExpire did not apply; the shared table now says %+v", route, owner)
			}
		})
	}
}
```

</details>
