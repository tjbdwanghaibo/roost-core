# B9 / C5：activity 窗口条目统一读入口、account 建角判定表、`Live` 停机契约（2026-10-06）

> **状态（2026-10-06 核对）**：已实施（`bd6df5e5`；换名释放 `b18d5613`、O37 `f6043e44`），已随 v1.21.0 发布。下文“未发版”是实施当时的状态。

维护者第四轮决定（[DECISIONS-PENDING](../review/DECISIONS-PENDING-2026-10-05.md#维护者决定2026-10-06第四轮)）B9、C5，
方向判断见 [N06 复核](../review/REVIEW-2026-10-05-n06-revn06.md#方向判断)。分支 `b9svc`，基线 `4f0bab75`。

## 1. 目标

| 项 | 问题链 | 目标 |
| --- | --- | --- |
| B9-activity | RR-20260914-02 → RR-20261001-09 → 其残余 → NC-42 → NC-51：同一不变量“窗口里存的条目先验证再行动”在相邻循环里第 5 次被打破的风险仍在（Delivering、PendingActivities 没补） | 读取已存窗口条目只有一个入口；坏条目一律跳过、保留、计数；运维有持有方专用的清除入口 |
| N06 S3 留项 | 观察 4 | Delivering 坏条目；`RetireDelivered` 对不在 Delivering 的键回报 true；并发释放同一死计划时 `plan_released` 计两次 |
| B9-account | RR-20260929-19 → RR-20261001-06 → 其残余：同一事实只在部分入口生效 | “名额状态 × 名字状态 × 入口 → 动作”写成一个纯函数，create_role 同名 / 换名、Admin 修复都查它 |
| C5 | APP-SINGLETON-LOCK §7.2 差异 3 | 保持“停机中仍算活”，写进 `Live` 契约并用例钉住 |

## 2. 改动面

### 2.1 activity（`kit/service/global/activity`）

- 新文件 `window_entries.go`：`readWindowEntries(groupID, stored Window) windowEntries` 把已存窗口拆成
  `usable`（同一个 `Window`，只含合法且属于本组的条目）与 `malformed []MalformedWindowEntry{List, Key, Reason}`，
  判断仍是 NC-51 的 `windowKeyProblem`。`loadWindowEntries` = `Windows.Get` + 拆分。
- 读者全部改走它：`PendingActivities`（Keys + Opening）、`DeliveringActivities`、`RetireDelivered`、`AdvanceExpired`
  的批次（`pendingScanBatch` 只在 usable 上轮转）。CAS 写路径（admit / confirm / retire）本来只按显式键改动，
  坏条目原样保留，不变。
- 计数与日志合成一个 `noteMalformed(groupID, list, found, stillUnscanned)`：按列表计 `sweep.window_key_malformed`（Keys，沿用）、
  `sweep.opening_intent_malformed`（Opening，键坏或计划坏，沿用）、`sweep.delivering_key_malformed`（新增）；日志只在出现和消失时各一次。
  键坏的条目每拍都看得见（不需要 I/O），不再受批次限制；计划坏只在 sweep 真的要执行计划时才判断，批次外的旧报告保留（沿用 RR-20261001-09）。
- `RetireDelivered`：先经入口确认键在本组的 usable Delivering 里，不在就返回 false、不读活动、不写任何窗口；
  回报值改为“本次 CAS 确实移走了它”。
- `Admin` 新增两个持有方专用方法（无 `//roost:rpc`，与 `ReopenDispatch` 同理）：
  - `MalformedWindowEntries(ctx, groupID)`：列出坏条目（入口的 malformed，加上 Opening 里计划不可执行的条目）；
  - `RemoveMalformedWindowEntry(ctx, groupID, list, key, note)`：在 CAS 里重新判断，只删“确实坏”的那一条（同列表里与 key 相等的全部），
    健康条目拒绝（`ErrStatus`），不存在返回 `ErrMissing`；note 必填，写到窗口的 `AdminNote` / `AdminActionAtUnix`。
- C4 核对：活动组 id 由 `LoadGroupsFile` 校验（trim 后非空、不含 `/`），与 `Key.Validate` 对 GroupID 的要求一致；
  game 侧 `gameactivity.Key(groupID, …)` 用文件里的组 id，协调器按 `Key.GroupID` 选窗口，`windowKeyProblem` 用同一个组 id 比较，
  所以按组的 key 与统一入口一致，不需要改动。

### 2.2 account（`kit/service/account`）

- 新文件 `creation_table.go`：`classifySlot`、`classifyName`（Admin 与 create_role 共用）、`decideCreation(slot, entry, name) creationAction`。
  入口四个：同名建角、同名建角 Reserve 被拒之后、换名建角、Admin 修复。名字状态 `nameNotRead` 表示“这一步不需要读名字”
  ——同名建角以原子的 `Reserve` 为准，只有被拒后才读名字。
- `createRole`、`resumeRoleCreation` 的 ErrKeyTaken 分支、`ResolvePendingCreation` 改为按动作执行；
  执行步骤（Reserve → admit → Roles.Create → Commit → 发布）与外来 PlayerID 的补偿不在表里，那是执行中的事实，不是入口判定。
- `releaseCreationSlot` 返回“是否真的删掉”；`plan_released` 只在真的删掉时计（观察 4）。
- 判定表照搬当前行为，不顺手改格子；表里看出的不对称（未 admitted 的计划，名字只被他人 reserved：同名请求释放、换名请求不释放）
  单列给维护者，见 §5。

### 2.3 C5（`app/singleton.go` 注释与测试、USER_GUIDE、APP-SINGLETON-LOCK）

只改文字与测试：`Live` 契约写明“停机期间（Service.Shutdown、各 Mod Stop）锁还在就算活，到 Release 为止”，以及对 activity 的影响：
恰在那时开的窗口把停机中的服算进 expected，这个窗口要等到宽限期。新用例在 `Service.Shutdown` 与 Mod Stop 里查 `Live` 都得到本 sid，Release 后键不在。

## 3. 兼容

- `Window` 加 `admin_note` / `admin_action_at_unix`（omitempty），旧记录照读；`Admin` 接口加两个方法，仓内只有 `*Service` 实现。
- 行为收紧：`PendingActivities` 不再交出坏键（之前交出的是别的组或不合法的键，game 拿去查只会查错组）；
  `RetireDelivered` 返回值语义改为“本次移走”（仓内唯一调用方 Server 不看返回值）；新指标 `sweep.delivering_key_malformed`。
- account 对外行为不变，`plan_released` 计数修正。生成形状不变，不需要重生成 game-demo。

## 4. 验证

先红后绿：`TestDeliveringSkipsMalformedEntriesAndKeepsThem`、`TestPendingActivitiesSkipsMalformedEntries`、
`TestRetireDeliveredReportsOnlyWhatItRemoved`、`TestConcurrentReleasesOfOneDeadPlanCountOnce` 在基线上红；
修复入口与判定表是新 API / 纯函数，没有修前红，用表格测试逐格覆盖。RR-19 / RR-06 / RR-06 残余 / NC-42 / NC-51 既有回归照样通过。
`go test -race -count=3` 两个包与 `./app/`，根包，`go build ./... && go vet ./...`。

## 5. 实施记录（2026-10-06）

**全部已实施**（提交见 DECISIONS-PENDING 第四轮表），未发版。

### 红 → 绿

| 用例 | 修前（基线 `4f0bab75`，Memory） | 修后 |
| --- | --- | --- |
| `TestDeliveringSkipsMalformedEntriesAndKeepsThem` | `group-a's delivering list = [group-a//close group-a/local/close group-b/foreign/close]`；`sweep.delivering_key_malformed counted 0, want 2`；`group-a's sweep wrote group-b's window: … Delivering:[group-b/foreign/close] … Version:3} after … Delivering:[] … Version:4}`（后两条是临时把首个断言降为日志取得） | 只交出本组键，计 2，group-b 窗口版本不变，坏条目保留，group-b 自己的 sweep 照常移走 |
| `TestPendingActivitiesSkipsMalformedEntries` | `PendingActivities(group-a) = [group-a//close group-a/bad/id/close group-a/local/close group-b/foreign/close]` | 只有 `group-a/local/close` |
| `TestRetireDeliveredReportsOnlyWhatItRemoved` | `retiring a key that is no longer listed = true, <nil>; want false` | 第一次 true，之后与从未列入的键都是 false |
| `TestConcurrentReleasesOfOneDeadPlanCountOnce` | `one dead plan released once was counted 2 times; … dropped:create_role.plan_released=2` | 计 1，恰好一个请求建角成功 |

新 API / 纯函数没有修前红：`TestOperatorRemovesOnlyMalformedWindowEntries`（列出三类坏条目、四种拒绝、三个列表各删一条、备注落盘、修后下一拍不再计数、“已消失”日志一次）；
`TestCreationTableEveryCell`（144 格规格，用一个格子做过变异：改一格即红）与 `TestCreationTableClassifiers`；C5 的 `TestSingletonLiveCountsAStoppingProcessUntilRelease`（钉住现有行为，本来就绿）。

### 设计取舍

- 统一入口只判断“键”，不判断 Opening 的计划：活动已存在的 Opening 条目不论计划如何都应被确认，计划只在 sweep 要执行它时才相关。
  运维视角下计划坏的条目仍是坏的，所以 `MalformedWindowEntries` / `RemoveMalformedWindowEntry` 额外把它们算进来（`storedEntryProblems`）。
- 键坏的条目每拍都报（不需要 I/O），不再随 oversized 窗口的批次轮转；坏条目也不再占用批次名额。计划坏仍只在批次内判断，批次外的旧报告保留。
- 修复入口只删除、不改写：被外部写坏的条目没有可信的“正确值”可以推出来；跨组键应在它自己的组窗口里，那边若缺，由正常的开窗 / 确认路径补。
- `DeliveringActivities` 不在 Coordinator 上，只有 sweep 调，所以把它当作 Delivering 的 tick 计数；`PendingActivities` 是 RPC，只过滤不计数（计数由 sweep 负责，避免按客户端调用次数放大）。
- 建角判定表照搬当前行为。同名请求以原子 `Reserve` 为准、只在被拒后读名字，所以“同名被拒之后”单列为一个入口；读名字失败时按被拒所证明的最弱事实（他人 reserved）判定，与修改前一致。
  外来 PlayerID 的补偿是执行中的事实，不在表里。

### 留给维护者（方向判断）

表让一处不对称显形：**未 admitted 的计划、名字只被他人 reserved**——同名请求（Reserve 被拒）释放 slot，换名请求答 `ErrRoleLimit` 保留 slot。
两者都不丢数据（未 admitted 意味着没有角色记录），差别只在换名的玩家要等那个预约过期并被别人提交、或找运维。
本轮按“只表化、不改格子”保持现状；如果要让换名也释放，改 `creation_table.go` 里一处判断和规格表里两格（`unadmitted other` 行的 res-else 与 free 列是否一并放开要一起定）。

activity 侧：统一入口之后，新增读者只要从 `loadWindowEntries` 拿数据就自动受约束；没有发现需要换掉窗口设计本身的证据。

### 验证

`GOWORK=off`：`gofmt -l` 空；`go vet` 与 `go test -race -count=3` 覆盖 `./kit/service/global/activity/ ./kit/service/account/ ./app/`；`./kit/service/integration/`（Memory 部分）；
根包 `go test -count=1 .`；`go build ./... && go vet ./...`。未改 nest / entity / dataengine / sync，不跑 glsvet；未改生成形状，不重生成 game-demo。
account 的 RR-06 / NC-50 / 新并发用例另在隔离 Redis（`~/.roost-it/roost-dataengine-it`，`ROOST_REVIEW3_BACKEND=redis`，前缀 `revn06:account:<纳秒>` 用例自清）上 5/5 通过。
activity 的真实 Redis / 两者的 Cluster 未跑：改动是窗口与判定逻辑，存储调用序列不变（account 的 `creationSlot` 拆成先 Get 再 `makeCreationPlan`，调用次数与顺序同前）。

## 6. 维护者第五轮决定：换名也释放（2026-10-06，分支 `revn09f`）

决定：**计划未 admitted、名字只被他人 reserved（还没 committed）时，换名请求也释放 slot**，与同名重试对齐（§5“留给维护者”那处不对称）。

- 改动：`decideCreation` 的 `entryOtherName` 分支多一条 `slot == slotPendingUnadmitted && name == nameReservedElsewhere → actReleaseAndRetry`；规格表 `unadmitted other` 行的 res-else 格由 `limit` 改为 `retry`。
  只改这一格：同行 free 列（未 admitted、名字已无人持有）维持 `limit`——决定只覆盖“他人 reserved”，free 列是否放开没有定，仍列给维护者。
  已 admitted 的计划在同一名字事实上仍答 `ErrRoleLimit`（预约会过期，计划可能还会完成）。
- 执行沿用 `actReleaseAndRetry`：`releaseCreationSlot` 的 `DeleteIf` 只在删除那一刻计划仍未 admitted 时删（`planDead || !Admitted`），与并发的 admission 互斥；删掉后按新名字建计划，`plan_released` 计一次。
- 回归：`kit/service/account/pending_creation_unadmitted_rename_promises_test.go`（预约回执丢失留下未 admitted 的计划 → 预约过期 → 他人预约 Hero → 换名 Knight）。
  修前：`role limit reached for this server: another name is pending on server 1`，slot 仍是 Hero 的计划；修后建出 Knight、他人的预约不变、`plan_released` = 1。
  `TestCreationTableEveryCell` 随规格表更新；`TestADifferentNameKeepsAPlanThatCanStillComplete/foreign_reservation_only`（已 admitted）不变、仍通过。
- 验证：`GOWORK=off go vet ./kit/service/account && go test -race -count=3 ./kit/service/account` 通过（Memory 后端）；真实 Redis 后端未跑（`DeleteIf` 调用与既有 RR-06 换名释放同一路径，存储调用序列不变）。

### 6.1 第七轮补记：free 格也释放（O37，2026-10-06，分支 `o33`）

维护者第七轮决定按推荐：**`unadmitted other` 行的 free 格（计划未 admitted、名字已无人持有）也释放名额并用新名字建角**，与 res-else 格（`b18d5613`）一致。上面“free 列维持 limit、留给维护者”的说法到此作废。

- 理由：free 说明计划的名字没有活着的预约——要么预约过期，要么从没预约成功。admitted 之前什么都没发生，释放不会留下角色；同一行里只剩名字仍归本计划（by-plan）时保留 slot，因为可能有一次尝试已过 Reserve、正要 admit。
- 改动：`decideCreation` 的 `entryOtherName` 分支改为“名字被别人 committed → retry；未 admitted 且名字被别人 reserved 或 free → retry；其余 limit”；规格表 `unadmitted other` 行 free 格 `limit` → `retry`（`TestCreationTableEveryCell`）。执行仍走 `actReleaseAndRetry`：`releaseCreationSlot` 的 `DeleteIf` 只在删除那一刻计划仍未 admitted 时删，与并发的 admission 互斥；并发的同名尝试若随后 Reserve 成功，admission CAS 会因 slot 已变而失败，它的预约自然过期。
- 回归：`pending_creation_unadmitted_rename_promises_test.go` 新增 `TestADifferentNameReleasesAnUnadmittedPlanWhoseNameIsFree`（预约回执丢失留下未 admitted 的计划 → 预约过期、Hero 无人持有 → 换名 Knight）。修前（基线 `4da5e7ea`）：
  ```
  --- FAIL: TestCreationTableEveryCell
      unadmitted / other / name=2: decideCreation = 8, table says retry (5)
  --- FAIL: TestADifferentNameReleasesAnUnadmittedPlanWhoseNameIsFree
      a different name must release the unadmitted plan whose name is free, got account: role limit reached for this server: another name is pending on server 1 (slot {… Creation:{… Name:Hero … Admitted:false}})
  ```
  修后建出 Knight、Hero 仍无人持有、`plan_released` = 1；已 admitted 的计划在 free 上仍答 `ErrRoleLimit`（`admitted other` 行不变）。
- 验证：`GOWORK=off go vet ./kit/service/account && go test -race -count=3 ./kit/service/account` 通过（Memory 后端）；真实 Redis 后端未跑（存储调用序列与 res-else 格、RR-06 换名释放同一路径）。
