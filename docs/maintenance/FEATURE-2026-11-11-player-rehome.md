# 单个玩家换服（改绑 game sid）（2026-11-11）

状态：**设计已定稿、一期实施中**（account 一侧已实现并合入，见 §3.1 末尾的“实施结果”；DataEngine 的交出实体、模块 `rehome` 与 demo 接线未完）。维护者 2026-11-11：“main 中有一个未完成‘换服’逻辑，最后帮我完成，但是是以模块方式，如果底层的话就还是放在框架内”。本文是实施依据；第 6 节的六项是按推荐项先实施、**待维护者确认**的决定。

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
| 一 | account 接口与 accountctl；`HandOffEntity`；模块 `rehome` 最小形态、enter 的进入前检查、`with/enter`；demo 的 `admitBound` 与 `PlayerOwners` 接上并改写用例；文档 | **前半已合入**（account 接口与 accountctl、`HandOffEntity`）；**后半未开始**（模块 `rehome`、enter 的进入前检查、demo 接线）。2026-11-11 因额度在这里停下 |
| 二 | demo 全量联动：队伍与匹配（换服前必须不在队里 / 自动取消排队）、副本与战斗中拒绝、场景（离开时离场景、到达回出生点）、联盟 `HomeSID` 惰性修正、全服邮件范围、名下建筑的规则；其余联动（mailbox、chat、leaderboard）；真实依赖演练（两个 game sid + account + saga，每个阶段后 kill -9） | 未开始 |
| 三 | 在途换服的索引、列举与过期清扫；`sidmerge` 清单加“无在途换服”；物理库不同的服；合服交叉；独立 Gate（转给 game 的绑定不带 claim） | 未开始 |

### 接手后半的提示

- 模块的 `Residency` 缺省实现调 `wiring/dataengine` 的 `Mod.HandOffEntity`，返回值的 `Release(ctx) error` 会等待、会失败（它先卸载交出期间被读回内存的实例），要处理 `ErrEntityUnloadBusy` 并重试；交出成立后的写入拿到 `ErrEntityHandedOff`（不要重试），进行中拿到可重试的 `ErrEntityUnloading`。调用顺序：先断开会话、离场景、退订，再交出。
- account：Begin 结果未知时直接 `AbortRoleTransfer(id)` 拿确定结论；到达时不必重发 Commit，slot 的收尾会在玩家下次 `SelectRole` 时补完；两种拒绝都带回当前 `RoleBinding`。
- enter 的进入钩子现在带会话 claims（`Entered.Claims`，邮件换用时加的）；换服要加的“进入前检查”是另一个登记点（能否决登录），两者互不相干。认证器已把 `BindEpochClaim` 与 `TransferClaim` 写进 claims。
- game-demo 的现状：进入游戏、聊天、排行榜、背包、档案、邮件都来自模块，全部模块在同一步里安装；`rehome` 加在模块列表末尾。`admitBound`、`playerMoving` 在 `demo/internal/service/game/gift_saga.go.tmpl` 与 `alliance_saga.go.tmpl`，v1.44.0 之后没有再动。
- DataEngine 一侧留给维护者定的三点（见 [DataEngine 指南“交出实体”](../framework/guide/03-dataengine.md) 与 KNOWN-LIMITS）：`Release` 是现在的“先卸载再撤屏障”还是改成交出期间直接拒绝冷加载；等 checkpoint 用现在的轮询还是按实体记账；`ErrEntityHandedOff` 放在 `engine` 还是挪到 `ErrEntityUnloading` 旁边。

## 8. 已知风险与没查清的

- 物理库不同的服之间换服，目标服会读到“从未创建”并新建玩家。一期靠规则文件的可去列表与文档前提，没有框架检查。
- 冷准入多一次 account 调用；account 不可用时离线玩家的步骤只能重试。
- 退回不耗尝试，时间上限是 saga 截止；换服拖得久的在途 saga 会走补偿，补偿方向一直等。
- GM 等不过准入的写入口（demo 的 `gm.player.add_item` 不查 `PlayerOwners`）现在就能在错的 sid 上加载玩家，换服后更容易踩到；“加载守卫”二期单独评估。
- 没查清：换过服的 slot，名字目录里的 owner 仍是旧键，`decideCreation` 对已发布的 slot 是否读名字（要用用例钉住）；Player 聚合将来挂 `dbscope=sid` 的 DAO 时模块能否检查出来。
