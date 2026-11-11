# 单个玩家换服（改绑 game sid）（2026-11-11）

状态：**一期已实现**（2026-11-11）：account 的换服接口、DataEngine 的交出实体、模块 `rehome`、enter 的进入前检查、game-demo 接线都已完成，**只有离线验证**；二、三期未开始。本文第 2～5 节是设计，实现与设计的出入记在 §3.1 末尾、§3.2 与下面的“一期后半的实施结果”；第 6 节的决定按推荐项实施，**仍待维护者确认**。

起因：v1.44.0 让 saga 的路由步骤按 sid 确切投递（[saga 步骤按 sid 确切路由](FEATURE-2026-11-11-saga-step-sid-route.md) §3.5、§7 第 1 条），game-demo 给“玩家正在换服”留了钩子 `playerMoving`，缺省恒返回“不在换服中”，因为仓库里没有单个玩家换服的能力。

## 1. 现状（读源码得到的事实）

- 绑定的唯一权威是 account 的 `Role.ServerID`（`service/account/types.go`，Redis）。没有接口能改它：`UpdateProfile` 明说不能改 server，`MergeServer` 是离线别名合服。slot 键是 `accountID@serverID`（`slotKeyFor`），`roleReady` 要求 slot 与角色一致——换服必须连 slot 一起搬。名字目录不分服，换服不改名。
- 会话令牌不含服；`ValidateSession` 每次读当前 Role。认证器把承载服写进会话 claim（`ServerIDClaim`），claim 是认证那一刻的快照。
- game 侧只做静态比较：demo 的 `PlayerOwners.Serve / AdmitBound`（`demo/internal/service/game/playerowner.go.tmpl`）比较 claim 或 saga 载荷里的 sid 与本进程的 sid；`admitBound`（`gift_saga.go.tmpl`、`alliance_saga.go.tmpl`）以**载荷**里的 sid 为绑定。
- Player 文档 `db=main`、没有 `dbscope=sid`：各服把 `main` 映射到同一物理库时，**换服不用搬数据**，只交接写权。saga 回执、实体定时器都在玩家文档里，随文档走。
- 卸载：`Runtime.UnloadEntity`（`framework/dataengine/engine/entity_unload.go`）立屏障 → 等投影 → 卸载 → **返回时撤屏障**。单 mutation 快路的幂等只靠 `_last_tx`（`mongo_store.go` 的 `classifyNoMatch`），“投影成功 → 确认 checkpoint”不是原子的。
- “同一玩家至多一个写者”现在等于“同一 sid 至多一个进程”（App 单实例锁）加“只在绑定的 sid 上服务”；兜底是投影的版本 CAS，冲突时进程 fail-stop。

## 2. 状态机

| # | 阶段 | 执行者 | 权威与存储 | 幂等键 |
| --- | --- | --- | --- | --- |
| 0 | 申请与检查：规则、登记的检查、记意图 | 源服，Player 上一笔 Nest 事务 | `PlayerDao.Rehome.Pending{ID, To}` | 换服 ID |
| 1 | 登记换服 | account | `Role.Transfer`，同时占目标 slot | 换服 ID + `ExpectEpoch` |
| 2 | 源服停写、断开、落库、卸载并保持屏障 | 源服进程 | 进程内迁出表 + DataEngine 屏障（都不持久） | 可重做 |
| 3 | 改绑 | account | `Role.ServerID = To`、`BindEpoch++`、`LastTransfer` | 换服 ID |
| 4 | 到达 | 目标服，Player 上一笔事务 | 清 `Rehome.Pending`、记冷却、到达钩子 | 换服 ID（`Rehome.ArrivedID`） |

- 源服本地的“离开中”标记先于第 1 步打上（Begin 失败再撤），第 2 步内部顺序：标记 → 断开会话 → 离开钩子 → 交出实体（屏障、排空、卸载、等 checkpoint、屏障保持）→ 删驻留记录。
- **account 的 `Role.Transfer` 是“正在换服”的唯一持久事实**；源服的迁出表是它的缓存与关竞态的闸；玩家档里的 `Rehome.Pending` 只是业务意图（冷却、到达恰好一次），不当栅栏用。
- 回退只在 Commit 之前有；Commit 之后要回去就是再换一次。

至多一个写者：

| 阶段 | 谁能写 | 靠什么 |
| --- | --- | --- |
| Begin 之前 | 源服 | 静态绑定 |
| Begin → 源服卸载完 | 源服 | 目标服按 claim / account 判定“不在本服” |
| 卸载完 → Commit | 无 | 源服屏障保持；account 仍指源服 |
| Commit 之后 | 目标服 | 源服进程内屏障 + 迁出表；重启后冷准入问 account |

崩溃恢复：

| 停在哪 | 恢复 |
| --- | --- |
| 0 之后、1 之前 | 下次进入源服时对账：account 没有这个 ID → 清 `Pending` |
| 1 之后、2 完成前 | 源服对该玩家的任何准入读到“换服中”→ 拒绝或退回；**向前恢复**：下次触达时重做第 2 步再 Commit；过期的由运维 Abort |
| 2 之后、3 结果未知 | 重发 Commit（按 ID 幂等）；Abort 返回“已提交”时按已提交处理 |
| 3 之后、4 之前 | 到达是惰性的：目标服首次触达时做 |
| 第 2 步内部失败（钩子否决、排空超时） | Abort → 撤屏障与标记 |
| 3 的应答丢失（Commit 或 Abort 已生效），重发之前 account 上又有别的换服了结 | 重发得到的 stale / 成功不说明这个 ID 的历史：按带回的当前绑定判定——归别的服就保持屏障与迁出记录，换走又换回（绑定代数至少 +2）才撤（RV-20261111-BM-04） |

## 3. 进框架的部分

### 3.1 `service/account`：换服接口与状态

类型（`types.go`，接口实现放新文件 `transfer.go`）：

- `Role` 加 `BindEpoch uint64`、`Transfer *RoleTransfer`、`LastTransfer *RoleTransferRecord`。
- `RoleTransfer{ID string; From, To int32; Epoch uint64; StartedAtUnix, DeadlineUnix int64; Note string}`。
- `RoleTransferRecord{ID string; From, To int32; Committed bool; AtUnix int64}`：Commit / Abort 回放的依据。
- `Slot` 加 `TransferID string`（目标 slot 的占位标记）。
- `RoleBinding{PlayerID; ServerID, ServingServerID; BindEpoch; Transfer; LastTransfer}`：给 game 的窄读模型。
- claim 键 `BindEpochClaim`、`TransferClaim`。

接口（加到 `Accounts`，上总线）：

- `RoleBinding(ctx, playerID) (RoleBinding, error)`
- `BeginRoleTransfer(ctx, playerID, TransferRequest{ID; FromSID, To; ExpectEpoch; TTLSeconds; Note}) (RoleBinding, error)`
- `CommitRoleTransfer(ctx, playerID, transferID) (RoleBinding, error)`
- `AbortRoleTransfer(ctx, playerID, transferID, reason) (RoleBinding, error)`

Begin 的拒绝：角色不存在或未就绪；`FromSID` 不是当前承载服；目标服不存在或等于当前服（`ErrServerInvalid`）；目标服已并入别服或不是 open（`ErrServerClosed`）；账号在目标服已有角色或待定计划（`ErrRoleLimit`）；`BindEpoch != ExpectEpoch`（新码 `ErrTransferStale`）；已有另一个 ID 在换服（新码 `ErrTransferInProgress`）。一期拒绝涉及已合服的服（源或目标）。

写入顺序（跨记录不原子，照 `MergeServer` 的“任何中途停下都可重跑”）：

- Begin：① 角色写 `Transfer`；② 建目标 slot（带 `TransferID`）。目标键被别的占用 → 撤 ① 答 `ErrRoleLimit`；同 ID 重发补完 ②。
- Commit：确认目标 slot 在（缺了补上）→ 角色改 `ServerID`、`BindEpoch++`、清 `Transfer`、写 `LastTransfer` → 清目标 slot 的 `TransferID` → 条件删旧 slot。`LastTransfer.ID` 相同且已提交时幂等成功并补做后两步。
- Abort：条件删目标 slot → 清 `Transfer`、`LastTransfer` 记未提交。该 ID 已提交时答 `ErrTransferStale` 并带回 binding。

**实施结果（2026-11-11，已合入）**——与上面设计的出入，以实现为准（现行说明在 [PLAYER-MIGRATION §7](../framework/PLAYER-MIGRATION.md) 与 KNOWN-LIMITS“单个角色换服（account 一侧）”）：

- **Abort 改为“先写角色、再删占位”**。按设计的顺序（先删占位），并发的 Commit 会在占位被删后把角色改绑过去，角色绑定在新服却没有 slot；有并发用例证明。Commit 的 CAS 发现已被中止时撤掉自己可能刚补建的占位。
- `Slot` 多一个 `BindEpoch`，`RoleTransferRecord` 多 `Epoch / Note / Settled`：提交的收尾按绑定代数认旧 slot，迟到的收尾不会删掉角色后来换回来的 slot；`Settled` 记“结果已定、slot 没收尾”，重发、下一次 `SelectRole` 或下一次 Begin 读到就补完。
- 中止一个没发起成功的 ID 也记为已中止，迟到的 Begin 不能再用它；已了结的 ID 再 Begin 答 `ErrTransferStale`。**给模块的提示：Begin 结果未知时直接 `AbortRoleTransfer(id)` 拿确定结论；到达时不必重发 Commit，收尾会在玩家下次 `SelectRole` 时补完。**
- 人工提交另有 `Admin.CommitRoleTransferWithNote`（不上总线），备注必填。
- “错的 from”答 `ErrTransferStale`；两种拒绝（`ErrTransferStale` 560118、`ErrTransferInProgress` 560119）都带回当前 `RoleBinding`，经总线调用时 `errors.Is` 成立。
- “涉及已合服的服”按“绑定服或目标服的 `MergedInto` 非 0 才拒绝”实现，承载着别的服的服照常可用。
- 换服 claim 的值是 `"<目标 sid>:<换服 ID>"`（`TransferClaimValue` / `ParseTransferClaim`）。期限只记录、不执行。
- §8 第一条已查清：换过服的 slot 在名字目录里的 owner 仍是旧键，没有影响（`decideCreation` 对已发布的 slot 与换服占位都不读名字，有用例钉住）。

换服中 `SelectRole` / `ValidateSession` 照常返回并带 `Transfer`（握手失败客户端拿不到原因，拒绝留给登录端点）。两个认证器写新 claim。`accountctl` 加 `show-role`、`transfer-role begin|commit|abort`。

**审查之后的两处修正（2026-11-11，`fix/core-game-server-review` 的 RV-20261111-BM-01 / BM-02）**——上面“Begin 结果未知时 `AbortRoleTransfer(id)` 拿确定结论”在两种时序下不成立，已修：

- **BM-01 迟到的 Begin 留下占位**：Begin 写了角色、停在占位写入之前，Abort 看到“还没有占位”就收了尾并记 `Settled`，随后占位才落地；Begin 不复核，返回成功与旧的“换服在途”，之后重发的 Abort、`SelectRole`、下一次 Begin 都因为 `Settled` 跳过清理，目标服的名额被永久占住。现在 Begin 占完位再读一次角色，不认这次换服了就撤掉占位、答 `ErrTransferStale` / `ErrTransferInProgress` 并带回当前绑定；删占位统一成一条规则（`releaseStrayHold`：slot 在角色的绑定服上、或角色在途的换服就是它才作数，其余没有人认的撤掉），中止的收尾、迟到的 Begin / Commit、下一次 Begin、`CreateRole` 都走它，已中止的记录即使 `Settled` 也每次再看目标服——“占位刚写完进程就退出”因此由之后的入口收尾。
- **BM-02 连续中止之后旧 Begin 被重放复活**：只留最近一次了结的 `LastTransfer`、中止不推进任何代数，`Begin A → Abort A → Begin B → Abort B` 之后原样重放 Begin A（它带的 `ExpectEpoch` 一直合法）会被受理。现在另设**换服尝试序号** `Role.TransferSeq`：每受理一次发起、每记下一次中止（含中止没发起过的 ID）加 1，随 `RoleBinding` 给出，`TransferRequest.ExpectSeq` 必须是当前值，在途 ID 的重发不看它。没有让中止推进 `BindEpoch`：它是绑定的代数，会话 claim、slot、模块判断“换回来了”（代数至少加 2）都按它。没有扩大“记住最近 N 个 ID”的环：淘汰之后仍然挡不住，单调序号不需要记住任何 ID。
- 对调用方：`TransferRequest` 多一个必填语义的字段 `ExpectSeq`（零值只对从没发起、中止过换服的角色成立），`RoleBinding` 多 `TransferSeq`。模块 rehome 的驱动器、demo 的 `accountctl transfer-role begin` 已带上；自己直接调 `BeginRoleTransfer` 的代码要先读 `RoleBinding`。发起方会多遇到一种 `ErrTransferStale`（读绑定与发起之间有别的发起或中止落地），重读再发。

### 3.2 DataEngine：“交出实体”

`Runtime.HandOffEntity(ctx, id) (*EntityHandOff, error)`，`EntityHandOff.Release(ctx) error`；`wiring/dataengine` 包一层（`Mod.HandOffEntity`、`ErrEntityHandedOff`）。与 `UnloadEntity` 的三点差别：实体不常驻时也立屏障；排空之后、卸载之前等 WAL checkpoint 盖过该实体已准入的记录；屏障保持到 `Release` 或进程退出。不新增持久状态。已实现（2026-11-11），契约与用法见 [guide/03 “交出实体”](../framework/guide/03-dataengine.md#交出实体handoffentity)，实现约束见 [impl/03 §2 第 13 条](../framework/impl/03-dataengine.md#2-必须保持的契约)。

为什么要等 checkpoint：源服在“已投影、checkpoint 未确认”的窗口崩溃，而目标服随后推进了文档，源服重启重放单 mutation 记录时 `_last_tx` 对不上 → `ErrProjectionConflict`（fatal），起不来。**结论（2026-11-11）：推断已证实，等待已实现。** 刻画用例 `TestUnconfirmedProjectionReplayedAfterAnotherWriterAdvancedTheDocument`（Mongo 替身）与 `TestRealMongoUnconfirmedProjectionReplayedAfterAnotherWriterAdvancedTheDocument`（真实副本集）在没有任何改动的代码上通过：单 mutation 快路记录已投影、checkpoint 未确认，另一个写者把文档推进一版之后，重启后的启动回放是 fatal 的 `ErrProjectionConflict`；不必重启，同一进程的下一轮回放也一样（fail-stop）；没有别的写者时重放幂等；写事务标记的记录（带回执）在同样的情形下重放安全。窗口也比“崩溃”宽：并行投影窗口里别的通道失败时，快路记录会停在“已投影、未确认”直到那条通道恢复。

实施时与本节原文不同、或原文没写的三处：

- **`Release(ctx) error` 而不是 `Release()`**。交出不拦加载（卸载后重载、任何访问都能把实体读回来，那是只读的实例），目标服写过之后这样的实例落后于库；屏障一撤，它上面的第一笔写入就是投影冲突。所以 `Release` 要先卸载此刻常驻的实例（等待、可能因实体被占用而失败并保持屏障），只在本进程（重新）拥有写权之后调用：Abort，或玩家后来换回本服。
- **两个错误**：交出进行中写入拿到可重试的 `ErrEntityUnloading`，成立之后是 `ErrEntityHandedOff`（不重试，互不满足对方的 `errors.Is`）。
- **重复交出幂等**（第 2 步“可重做”）：返回同一份交出，期间加载回来的实例再卸载一次。Remote 托管实体拒绝。

DataEngine 是三块基础之一：这一处改动只加新入口，不改 `UnloadEntity` 与投影路径的既有行为。

## 4. 做成模块的部分：`rehome`

- 清单：`needs`（`mods [nest]`、`features [protocol, entity, dao, errcode]`、`uses [account]`），`access.transport: tcp`，`entity: Player`，状态 `PlayerDao.Rehome`（只落库），组件 `Rehome`，协议 `RehomeApply`，错误码 `rehome_refused / rehome_moving / rehome_elsewhere`，联动 `with/enter`。
- `game/rehome`（登记点包，不 import 实体与 controller）：`Directory` 接口（Begin / Commit / Abort / Binding）与 account 适配；`Residency` 接口（离开 / 落定 / 当前绑定）与缺省实现（断开会话 + `HandOffEntity` + 进程内迁出表）；给 saga 准入用的 `Binding(ctx, registry, playerID) (sid, moving, err)`。
- `game/rehome/rehome_rules.go`（归工程所有）：可去的服、冷却、费用。
- 三个登记点：**换服前检查**（事务内，拿到 `*Player`，返回错误即拒绝；事务外，给要问别的服务的检查用）；**离开时**（事务外，Begin 之后、卸载之前，失败即 Abort）；**到达后**（事务内的一半恰好一次，事务外的一半失败只记日志）。
- 客户端交互：`RehomeApply` 在第 1 步成功后就应答“已受理”并带换服 ID，其后在后台继续（断连接发生在 Commit 之前）；客户端重连后选角，看到的服变了就是成功。
- enter 模块新增**能否决登录的“进入前检查”登记点**（现有 `OnEntered` 失败只记日志）；rehome 的 `with/enter` 登记：换服中 → `rehome_moving`；不在本服 → `rehome_elsewhere` 带目标 sid；首次进入目标服做到达。
- 只有一个 sid 的 `-template game` 工程：能编译，场景用替身 Directory / Residency 离线跑通；运行时申请换到不存在的服，account 答 `ErrServerInvalid`，模块转成 `rehome_refused`。不用它的工程不多做事。

## 5. game-demo 怎么接（一期）

- `PlayerOwners` 实现模块的 `Residency` 并发布到 Registry；加迁出表；`Serve` 收 claim 里的 `BindEpoch` 与换服标记；`AdmitBound` 改成按**当前绑定**回答（有驻留记录答本服；迁出表答目标或换服中；否则问 account，锁内复核迁出表后建记录）。
- `admitBound`：载荷没有 sid 仍 `RejectStep`；换服中 `BounceStep("")`；绑定别处 `BounceStep(SidRoute(当前 sid))`。`playerMoving` / `notMoving` 与 TODO 删掉。载荷里的 sid 降为“发起时的服”，只用于初始路由。
- 登录：开头跑进入前检查，`auth.go` 写新 claim。GM 加 `gm.player.rehome`。
- 用例：`TestAStepForAMovingSenderIsBouncedBackToTheSender` 改用真实的 `account.Service`（MemoryStore）与两张真实的 `PlayerOwners`（1000、1001）。

## 6. 按推荐项先实施、待维护者确认的决定

1. 换服接口**上总线**（源服进程调用），`accountctl` 作人工兜底。
2. `Role.ServerID` 的含义改为**当前绑定服**，不另存建角服。
3. 目标服必须 open、未并入、账号在目标服没有角色；涉及已合服的服一期拒绝。
4. Begin 之后崩溃一律**向前恢复**，只有显式失败、运维、过期才 Abort。
5. `RehomeApply` **缺省对客户端开放**（规则文件缺省允许、带冷却）。
6. 模块名 `rehome`；checkpoint 等待先写用例证实再定（§3.2）——**已证实并实现（2026-11-11）**：替身与真实副本集上的刻画用例都得到 fatal 的 `ErrProjectionConflict`，`HandOffEntity` 等 checkpoint 盖过该实体的记录后才卸载、返回。另有一处需要确认：`Release` 带 `ctx`、会失败（§3.2 的说明）。

## 7. 分期

| 期 | 内容 | 状态 |
| --- | --- | --- |
| 一 | account 接口与 accountctl；`HandOffEntity`；模块 `rehome` 最小形态、enter 的进入前检查、`with/enter`；demo 的 `admitBound` 与 `PlayerOwners` 接上并改写用例；文档 | **已实现**，只有离线验证 |
| 二 | demo 全量联动：队伍与匹配（换服前必须不在队里 / 自动取消排队）、副本与战斗中拒绝、场景（离开时离场景、到达回出生点）、联盟 `HomeSID` 惰性修正、全服邮件范围、名下建筑的规则；其余联动（mailbox、chat、leaderboard）；真实依赖演练（两个 game sid + account + saga，每个阶段后 kill -9） | 未开始 |
| 三 | 在途换服的索引、列举与过期清扫；`sidmerge` 清单加“无在途换服”；物理库不同的服；合服交叉；独立 Gate（转给 game 的绑定不带 claim） | 未开始 |

### 一期后半的实施结果（2026-11-11）

模块 `rehome`（30 条场景，审查修正之后 40 条）、联动 rehome + enter（5 条）、enter 的进入前检查（enter 6 → 11 条）与 game-demo 接线已完成。现行说明：[modules/README](../../modules/README.md)、[PLAYER-MIGRATION §7](../framework/PLAYER-MIGRATION.md)、KNOWN-LIMITS“单个玩家换服”。与 §2～§5 设计的出入：

- ~~离开钩子在断开会话之前~~：**已改回 §2 的顺序**（见下面“审查之后的修正”）——标记 → 断开会话并排空 → 复核 → 离开钩子 → 交出实体。
- **受理之后中止也断开会话**；受理与断开之间留 500 毫秒让“已受理”的应答先到。
- **迁出表的“已迁走”只直接回答 30 秒**，之后向 account 核对；“换回来”的认定是承载服是本服且绑定代数 ≥ 发起时 +2。
- **意图对账用 `AbortRoleTransfer(id)` 挡住迟到的发起**，不是只读；清不清意图看它带回的当前绑定（见“审查之后的修正”），不看“中止成功”本身。
- 多一个错误码 `rehome_cooldown`；费用没有实现；缺省冷却 7 天。
- enter 多了进入骨架 `gameenter.Enter`（检查 → 加载 → 钩子）：game-demo 覆盖了 enter 的端点文件，放在端点包里的测试到 demo 里会失败，骨架放进不被覆盖的 `game/enter` 才测得到“被拒绝的进入什么都没碰”。
- game-demo：`PlayerOwners.Serve` 的签名没有改、不直接解析 claims（claims 只在模块的 `Admit` 一处解释，登录在 `Serve` 之前跑它，`Serve` 在锁内复核迁出表）；`AdmitBound` 按当前绑定回答；demo 自己的换服前检查拦公会、队伍、分区场景三样。
- **没有用主线新加的服务级接线点**：rehome 目前没有必须在服务启动时做的事（迁出表用到时才建、驱动器每次用到时才取依赖、向前恢复由登录触发）。account 有了“在途换服”的索引（三期）之后值得用它在启动时续做、停机时收尾。

维护者 2026-11-11 对验证节奏的要求：完整测试不必每次修改都跑，最后统一跑一次。

### 审查之后的修正（2026-11-11，RV-20261111-BM-04 / BM-05）

出处：[业务模块续审报告](https://github.com/tjbdwanghaibo/roost-core/blob/fix/core-game-server-review/docs/maintenance/BUSINESS-MODULES-FOLLOWUP-20261111.md)。两项都在模块 `rehome` 与 game-demo 的接线里，account 没有改。

**BM-04（P1）：结论按 account 的当前绑定判定，不按单个错误码。** 原来的 `conclude` 丢掉 Commit / Abort 带回的 `RoleBinding`：Commit 得到 `ErrTransferStale` 一律当未提交，Abort 成功一律当未提交。可 account 按换服 ID 只认在途的与最近了结的那一次——提交 A 的应答丢了、角色在目标服又发起并中止了 B 之后，重发的 Commit A 得到的 stale 只是“不认识这个 ID 了”；同理，重发的 Abort A 会被当成“中止一个没发起过的 ID”而成功。驱动器据此撤了屏障、删了迁出记录、清了意图，源服又能写一名 account 已经判给目标服的玩家。现在：

- account 一旦明确回答（照做、stale、in-progress、拒绝），结论由 `judge(move, self, binding)` 给出，只看三样：在途的是不是这个 ID、承载服是不是本服、绑定代数相对发起时走了几步。承载服不是本服 → **归别的服**，按已提交落定（屏障保持，没交出过的补交，迁出表记他现在的服，意图不动）；承载服是本服、代数没变 → 这一次没有成；承载服是本服、代数至少 +2 → 换走又换回，撤旧屏障；对不上的组合（代数恰好 +1 却在本服）不下结论、稍后再问。
- 写入没有带回绑定时读一次 `RoleBinding`；读不到按结果未知重发。
- 发起结果未知时的中止（`beginUnknown`）、发起被拒时带回的绑定、进入时的意图对账（`Arrive`）用同一个判断。对账：他归本服、那个 ID 不在途才清意图。
- `Directory` 的契约注释改成：写入的错误码只分“请求被处理了”与“结果未知”。

**BM-05（P2）：换服前的检查要撑到交出实体。** 原来申请时查一次（demo 在 Prepare 事务里读 `PlayerDao.GuildID`），之后客户端还连着、写闸门不看迁出标记、成员通知照常写档，后台离开与提交不再验证；离开钩子又排在断开会话之前。现在的顺序与理由：

1. **标记即拒绝**。`Reserve` 打上“离开中”的那一刻起，这名玩家新的请求在接入边界的写闸门被拒（连接随之关闭）。模块的缺省迁出表第一次用到时把自己发布成写闸门（`access.player.write_gate`；工程已有自己的闸门时不抢，由那个闸门调 `rehome.AdmitMessage`）；game-demo 的驻留表在 `Admit` 里看迁出表。进入游戏那条消息豁免（进入前检查给它带码的回答）。
2. **排空**。受理之后先 `Residency.Quiesce`：断开会话，等这名玩家一条会话都不剩。依据是玩家传输的两条性质——一条连接的读循环串行分发请求；会话要等读循环退出（它正在执行的那个请求的 handler 已经返回、Nest 事务已经提交或失败）才从会话表里摘掉。所以归零时，标记之前已经准入的请求都执行完了。等不到（预算 30 秒）就中止。
3. **复核**。`Profile.Recheck` 把事务内的换服前检查重跑一遍（实现是同一个换服 ID 的 `RehomePrepare` 再发一次：意图已记着时它只重跑检查）。只看这名玩家自己的档的规则在这里是可靠的——能改这份档的他自己的请求已经没有了。
4. **离开钩子**（事务外）在复核之后、交出之前：向别的服务问关于他的权威事实。demo 在这里问联盟服务的成员卡（`Membership`）：是成员、有入会预留在途、或者问不到，都中止。
5. 之后才交出实体、提交。

仍然开着的窗口（KNOWN-LIMITS）：入会请求已经发出、还在去联盟服务的路上、比离开钩子的询问晚到；别人替他改成员关系（GM 的代加入、会长批准）发生在询问之后——这两种要靠联盟服务认得“这名玩家正在换服”，属于二期。推送失败时被传输提前摘掉的连接、独立 Gate 部署下的会话不在排空的保证里。

接口变化：`Residency` 多一个方法 `Quiesce(ctx, move)`，`Depart` 不再负责“等请求执行完”；`Profile` 多一个方法 `Recheck(ctx, move)`；`RehomePrepare` 对同一个换服 ID 的重发从“什么都不做”改为“重跑事务内检查”；新增 `rehome.AdmitMessage` / `ExemptFromGate` 与 `LocalResidency.AdmitMessage`。自己实现了 `Residency` 的工程要补 `Quiesce`，并让自己的写闸门拒绝迁出表里的玩家。

### 接手后半的提示

- 模块的 `Residency` 缺省实现调 `wiring/dataengine` 的 `Mod.HandOffEntity`，返回值的 `Release(ctx) error` 会等待、会失败（它先卸载交出期间被读回内存的实例），要处理 `ErrEntityUnloadBusy` 并重试；交出成立后的写入拿到 `ErrEntityHandedOff`（不要重试），进行中拿到可重试的 `ErrEntityUnloading`。调用顺序：先断开会话、离场景、退订，再交出。
- account：Begin 结果未知时直接 `AbortRoleTransfer(id)` 拿确定结论；到达时不必重发 Commit，slot 的收尾会在玩家下次 `SelectRole` 时补完；两种拒绝都带回当前 `RoleBinding`。发起带上读到的 `BindEpoch` 与 `TransferSeq`（`ExpectEpoch` / `ExpectSeq`）；中止拿到的结论跨之后的发起与中止都成立（见 §3.1 末尾“审查之后的两处修正”）。
- enter 的进入钩子现在带会话 claims（`Entered.Claims`，邮件换用时加的）；换服要加的“进入前检查”是另一个登记点（能否决登录），两者互不相干。认证器已把 `BindEpochClaim` 与 `TransferClaim` 写进 claims。
- game-demo 的现状：进入游戏、聊天、排行榜、背包、档案、邮件都来自模块，全部模块在同一步里安装；`rehome` 加在模块列表末尾。`admitBound`、`playerMoving` 在 `demo/internal/service/game/gift_saga.go.tmpl` 与 `alliance_saga.go.tmpl`，v1.44.0 之后没有再动。
- DataEngine 一侧留给维护者定的三点（见 [DataEngine 指南“交出实体”](../framework/guide/03-dataengine.md) 与 KNOWN-LIMITS）：`Release` 是现在的“先卸载再撤屏障”还是改成交出期间直接拒绝冷加载；等 checkpoint 用现在的轮询还是按实体记账；`ErrEntityHandedOff` 放在 `engine` 还是挪到 `ErrEntityUnloading` 旁边。

## 8. 已知风险与没查清的

- 物理库不同的服之间换服，目标服会读到“从未创建”并新建玩家。一期靠规则文件的可去列表与文档前提，没有框架检查。
- 冷准入多一次 account 调用；account 不可用时离线玩家的步骤只能重试。
- 退回不耗尝试，时间上限是 saga 截止；换服拖得久的在途 saga 会走补偿，补偿方向一直等。
- GM 等不过准入的写入口（demo 的 `gm.player.add_item` 不查 `PlayerOwners`）现在就能在错的 sid 上加载玩家，换服后更容易踩到；“加载守卫”二期单独评估。
- 没查清：换过服的 slot，名字目录里的 owner 仍是旧键，`decideCreation` 对已发布的 slot 是否读名字（要用用例钉住）；Player 聚合将来挂 `dbscope=sid` 的 DAO 时模块能否检查出来。
