# rehome：单个玩家换服（把一名玩家从本服改绑到另一个 game sid）

“换服”在这里的意思是：一名玩家原来由 A 服（一个 game 进程，按 `sid` 区分）读写，换完之后由 B 服读写。交接两样东西：“谁可以写这名玩家”，以及他的档——v1.47.0 起本地实体的 DAO 缺省 `dbscope=sid`（物理库 `<库>_<sid>`，每个服一个），交出之后把玩家实体的 `dbscope=sid` 文档从 A 服的库复制到 B 服的库，改绑之后删掉 A 服库里的那几份（**搬档**）；`dbscope=global` 的 DAO 各服共用一份，不动。它是合服、整服迁移之外的另一件事，一次只动一名玩家。

谁管什么（后面都用这几个词）：

- **绑定的权威是 account 服务**的角色记录：这名玩家现在绑定在哪个服、是否正在换服、绑定换过几代（绑定代数）。模块经 `Directory` 接口问它、改它。
- **迁出表**是本进程内的一张表（`Residency`）：记着正在离开与已经从本进程换走的玩家。它是 account 那份事实在本进程的缓存，也是关竞态的闸——玩家一开始换服，本进程对他的新请求、进入、后台步骤立刻答“换服中”，不等 account。
- **屏障**：实体被“交出”之后，DataEngine 拒绝本进程再写这个实体，直到屏障撤销或进程退出。
- **意图**：记在玩家档里的 `PlayerDao.Rehome`，跟着档走。只管两件事——冷却从哪一刻算起、到达只做一次。它不是栅栏。

## 能力

**客户端协议**（`protocol/def/rehome_apply.go`）

- `RehomeApply{To}` → `RehomeApplyResponse{Code, Reason, TransferID, To}`：申请把自己换到 `To` 服。玩家是谁取自认证连接。`Code` 为 0 表示**已受理**（换服已经在 account 登记），不是已完成。
- 没有推送协议。受理之后，不论换成没换成，服务端都会断开这名玩家的连接；客户端重新选角，角色所在的服变了就是换成了，没变就是没换成。应答丢了也按同样的办法处理。

**服务端入口**

- `Controller.Move(ctx, playerID, to, note) (rehome.Ticket, error)`（`game/controllers/rehome/controller.go`，`rehome.FromRegistry(registry)` 取 `*Controller`）：GM / 运维发起的换服。与 `RehomeApply` 走同一个驱动器，区别是不受规则文件约束（可去的服、冷却）；登记的检查与 account 的条件照常生效。必须在玩家当前绑定的那个服的进程里调用。返回 `Ticket{ID, From, To}` 表示已受理。
- `DriverFromRegistry(registry) (*rehome.Driver, error)`（同一个文件）：取本进程唯一的驱动器。自己写进入流程的工程用它调下面两个方法。
  - `Driver.Admit(ctx, playerID, claims) error`（`game/rehome/rehome_admit.go`）：加载 Player **之前**调，回答这名玩家现在能不能进本服。`nil` 放行；`rehome_moving` 正在换服；`rehome_elsewhere` 绑定在别的服；可重试或不带码的错误表示现在判断不了，不放行。
  - `Driver.Arrive(ctx, playerID) error`：进入成功**之后**调。意图指向本服时做到达（只做一次）；意图指向别的服时向 account 确认那次没成并清掉。
  - `Driver.Apply(ctx, rehome.Request{PlayerID, To, Operator, Note})`、`Driver.Resume(move)`：申请与“接着做完”，一般经 `Move` 与 `Admit` 间接用到。
- `rehome.Binding(ctx, registry, playerID) (sid int32, moving bool, err error)`（`game/rehome/rehome_binding.go`）：给不经登录的后台路径（saga 步骤的准入、后台发放）问“这名玩家现在归哪个服”。`moving` 为 true 时不要在任何服上替他执行，稍后再试；否则 `sid` 不是本服就把这一步改投到那个服。它会等待（可能问 account），不能在 Nest 业务 worker 上调。
- `rehome.AdmitMessage(registry, playerID, messageID) error`（`game/rehome/rehome_residency.go`）：给有自己的写闸门的工程在闸门里调，迁出表里的玩家一律拒绝。
- `rehome.ExemptFromGate(messageID)`：登记一条不受写闸门约束的消息（它自己会查迁出表并给客户端带码的回答）。只在 `init` 里调。
- `rehome.OwnerSID(err) (sid int32, ok bool)`：从 `rehome_elsewhere` 的拒绝里取出玩家现在的服。`rehome.SelfSID(registry)`：本进程的 sid。
- Nest handler（`game/handler/rehome_*.go`，都是 `rollback=undo durability=strict`，由驱动器调用，不对客户端开放）：`RehomePrepare`（核对规则与事务内检查、记意图）、`RehomeCancel`（按换服 ID 清意图）、`RehomeArrive`（到达）。组件 `RehomeComp()` 另有只读的 `LastAt()`、`Count()`。

**登记点**（三个时机；有的分事务内与事务外两半，一个功能两半都需要时两处都登记。都只在 `init` 里登记，空名、nil、重名直接 panic，按名字顺序执行）

| 时机 | 事务之外（`game/rehome/rehome_hooks.go`） | 事务之内（`game/components/rehome/hooks.go`） |
| --- | --- | --- |
| 换服前检查 | `rehome.OnApplying(name, func(ctx, registry, move) error)`：记意图、向 account 发起之前执行。可以问别的服务或进程内状态。返回错误即拒绝，什么都没写 | `rehomecomp.OnRehomeApplying(name, func(owner view.PlayerEntity, move, nowUnixMilli) error)`：与记意图同一笔 Player 事务。只看这名玩家自己的档。当成只读来写 |
| 离开时 | `rehome.OnLeaving(name, func(ctx, registry, move) error)`：已受理、玩家已经安静下来（会话已断、先前的请求都执行完、新请求进不来）、实体还没交出。在这里向别的服务问关于他的权威事实，或把他从进程内的东西里拿出来。返回错误即中止这一次换服。可能被重复调用 | 无 |
| 到达后 | `rehome.OnArrived(name, func(ctx, registry, arrival) error)`：到达的事务提交之后、在进入游戏的请求里执行。**不是恰好一次**：失败只记日志、不重试 | `rehomecomp.OnRehomeArrived(name, func(owner view.PlayerEntity, arrival, nowUnixMilli) error)`：与“到达过了”的标记同一笔事务，**恰好一次**。返回错误则这次到达整笔回滚，下一次进入重做 |

事务内的换服前检查会执行两次：申请时一次，交出实体之前复核一次。

**可替换的两个接口**（按能力名发布到 Registry 即生效）

- `rehome.Directory`（能力名 `game.rehome.directory`）：绑定的权威，四个方法 `Binding` / `Begin` / `Commit` / `Abort`。缺省是 account 服务的客户端。给不经 account 管理角色绑定的工程与测试用。
- `rehome.Residency`（能力名 `game.rehome.residency`）：迁出表加“断开会话、交出实体”的执行者，方法 `Reserve` / `Quiesce` / `Depart` / `Settle` / `Where`。缺省是 `LocalResidency`。工程有自己的驻留表（按玩家记着谁在本进程）时，让那张表实现它，迁出表与驻留表才在同一把锁下。

**状态**：`PlayerDao.Rehome`（`db/def/rehome.go` 的 `RehomeState`），只落库、不下发。`PendingID` / `PendingFrom` / `PendingTo` / `PendingAt`（至多一份意图）、`ArrivedID`（最近一次到达的换服 ID）、`LastAt`（最近一次到达的业务时间，冷却从它算起）、`Count`（换过几次服）。迁出表只在内存里，进程重启清空。

**配置**：没有配置表与配置键。规则在归工程所有的 `game/rehome/rehome_rules.go`：

- `TargetAllowed(from, to) bool`：玩家自己申请时能去哪些服。缺省任何别的服都可以。
- `Cooldown() time.Duration`：两次换服之间至少隔多久，从上一次到达算起（业务时间）。缺省 7 天；返回 0 或负数表示不设冷却。

**错误码**（`internal/errors/rehome.go`）

- `rehome_refused`：申请没有被接受，什么都没改——目标服不存在、没开、就是本服、不在可去的列表里，账号在目标服已有角色，或登记的检查否决（检查返回了自己的业务码时客户端拿到那个码）。
- `rehome_cooldown`：距上一次换服还不够久。
- `rehome_moving`：正在换服、结论未定。申请时表示已有一次在途；进入游戏时表示现在不能进，稍后重新选角。
- `rehome_elsewhere`：这名玩家绑定在别的服，本服不为他服务。
- 另外会答框架的 `retryable`：没有执行，可以原样再发（例如 account 一时问不到）。

### 一次换服的顺序

源服一侧（`game/rehome/rehome_driver.go` 的 `Driver`）：

1. **申请的请求里**：读 account 的绑定 → 在迁出表打上“离开中”的标记（`Reserve`）→ 事务外的换服前检查 → `RehomePrepare`（规则、事务内检查、记意图，一笔事务）→ 向 account 发起（`Begin`：登记换服、占住目标服的名额）。到这里答“已受理”。前面任何一步不成：撤标记、清意图、答拒绝的原因。
2. **后台**（受理之后等 500 毫秒让应答先到客户端）：安静下来（`Quiesce`：断开他的会话，等标记之前已经准入的请求执行完）→ 复核（事务内检查重跑一遍）→ 离开钩子 → 交出实体（`Depart`：已准入的写入落库、从内存卸载、立起屏障）→ 搬档（`Documents.Copy`：把他的文档复制到目标服的库，覆盖那里的旧副本）→ 向 account 提交（`Commit`：改绑到目标服）→ 落定（迁出表记“已迁走”，屏障保持）→ 删本服库里的那几份（`Documents.Drop`，只删版本仍是复制时版本的文档）。
3. 后台任何一步不成（安静不下来、复核不过、离开钩子失败、交出失败、搬档失败、account 明确拒绝提交）：向 account 中止；已经搬过档的，先删掉写到目标服库里的副本；再撤屏障与标记，断开仍连着的会话，清意图。玩家留在本服，可以再申请。

目标服一侧：玩家重新选角连到目标服，第一次进入时 `Arrive` 在一笔事务里清意图、记冷却起点、次数加一、跑事务内的到达钩子；提交之后跑事务外的到达钩子。

从交出成立到提交之间，没有任何进程能写这名玩家；提交之后只有目标服能写。

**搬档**（缺省经 DataEngine Mod 的 `CopyHandedOffEntity` / `DropEntityCopy`；接口是 `rehome_residency.go` 的 `Documents`，`DriverConfig.Documents` 可以替换）：

- 谁写：只有源服，而且只在本进程保持着这名玩家的交出屏障时（DataEngine 拒绝在没交出的实体上复制或删除）。交出保证本进程写过的都已落库、之后不再写，所以源库里的就是最终版本。文档原样复制（`_version` / `_schema` / `_last_tx` / 墓碑字段不改），目标服冷加载时从这些元数据继续。
- 原子与幂等：一次复制或删除涉及的全部文档在一个 Mongo 事务里。复制时目标库已有同一版本算“已在”；已有别的版本时，提交之前（他仍绑在本服）覆盖——那只能是以前留下的旧副本；之后补搬（见下一条）不覆盖。删除带 `_version` 条件，之后被写过的不删；删除失败一直重发。
- 没经本进程搬档就归了别的服（应答丢失后 account 已提交、运维用 `accountctl` 提交、源服重启后由登录续做）：先补搬到他现在的服，那里没有才写；那里已有别的版本时不覆盖、也不删本服的，记 Error 留给运维。补搬成功后删本服的。
- 他换走又换回本服：本服库里已是目标服搬回来的新版本，不动。
- 中止：已经搬过档的，在撤屏障之前删掉目标服库里这一次写的副本（版本条件同上）。

**日志**：规则 / 冷却 / account 拒绝申请记 Info，记录玩家、目标服和码；原有受理 / 完成日志保留。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `game/controllers/rehome/rehome_apply.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **搬档的前提**：两个服把同一个逻辑库映射到同一个 Mongo 上的同一个物理前缀（源服按自己的映射算出目标服的库 `<前缀>_<to>`）。映射不同、或各服用不同的 Mongo 时，目标服读不到搬过去的档，读到“从未创建”而新建空档；框架不检查这一点，要靠 `TargetAllowed` 把关。只搬玩家实体自己的 `dbscope=sid` DAO；他在别的实体（独立的宠物、邮箱实体等）里的数据不搬，要跟着走就在离开钩子里自己处理，或放进 Player 的 DAO。
- **搬档的崩溃窗口**：提交之后、删除源库之前源服崩溃，源库留着一份旧档：源服重启时的起服预加载会把它载进内存（接不到他的请求：account 已把他绑到目标服；实体自己的定时逻辑仍可能写这份旧档，只写源库、不影响目标服的那份），直到他再来登录源服触发续做才处理；而续做时目标服多半已经写过他，补搬不覆盖、源库这份留给运维删。搬档之后、中止删掉目标副本之前崩溃，目标库留着一份旧副本，下一次换去那个服时被覆盖。见 KNOWN-LIMITS。
- **不做的事**：不收费（模块不依赖货币；做法写在 `rehome_rules.go` 的注释里——在事务内的换服前检查里只读地核对付得起，在事务内的到达钩子里扣）；没有换服进度的推送；不搬玩家在别的实体或别的服务里的关系（公会、队伍、好友、场景、排行、全服邮件范围）——要拦就登记检查，要清理就登记离开钩子；不做批量换服、合服；不记“最初的服”——换服之后 account 给会话的原服 claim 是当前绑定的服，要按最初的服做显示或归属的业务自己在玩家数据里记。
- **频率与大小**：一名玩家同一时刻至多一次换服在途；冷却缺省 7 天（GM 发起的不受约束）；意图至多一份、到达标记只记最近一次，玩家档里的记录大小不随换服次数增长。
- **时间预算**（`rehome_driver.go` 的常量）：一次 account 调用或一笔 Nest 事务 5 秒；全部离开钩子合计 10 秒；安静下来 30 秒、交出实体 30 秒（等不到就中止）；结果未知时的重发间隔从 1 秒翻倍、封顶 30 秒。
- **一致性**：
  - 记意图与事务内检查是一笔 Player 严格事务；到达（清意图、冷却、次数、事务内钩子）是另一笔，按换服 ID 只做一次。
  - 与 account 之间不是一笔事务。结论**只按 account 带回的当前绑定判定**（承载服、绑定代数、在途的换服 ID），不从错误码推断某个换服 ID 当年是提交还是中止。
  - **结果未知时无限重发**：提交、中止、撤屏障在 account 或实体不给结论时一直重发，没有次数上限，也没有停机钩子（后台 goroutine 随进程退出）。期间这名玩家在哪个服都进不去——有意如此，因为没有人知道他归谁。
  - 离开钩子可能被重复调用（源服重启后接着做完时再调一次）；钩子做的事在换服被中止时不会被撤回。
  - 事务外的到达钩子至多执行一次，可能一次都不执行。
- **“安静下来”的保证有两个缺口**（缺省的 `LocalResidency`）：推送失败时被传输提前摘掉的连接，它的 handler 可能还没返回；独立 Gate 部署下经 Game Ingress 转来的请求没有核对过。另外，被断开而取消的请求，它发给别的服务的调用可能已经生效——离开钩子要向那个服务问“包含在途操作”的事实；请求已发出、比钩子的询问晚到那个服务的情形拦不住。
- **跨服务的关系不能靠一次检查冻结**：检查之后别的玩家、别的服务仍可能改这名玩家的状态。需要完全冻结的要另做业务协议。
- **屏障不持久，向前恢复只由登录触发**：源服受理之后重启，迁出表没了、account 上换服仍在途；这名玩家再来登录源服时由 `Admit` 接着做完。他不再来登录源服，换服就一直停在在途——account 没有“在途换服”的索引，要运维用 `accountctl transfer-role commit|abort` 人工了结。`rehome.Binding` 不触发恢复。
- **迁出表随换走的玩家数增长**：一行只在那名玩家换回本服时删除，进程重启清空。“已迁走”的记录只在 30 秒内直接回答（`departedTrust`），之后向 account 核对。
- **写闸门**：缺省的迁出表把自己发布成接入边界的写闸门（能力名 `access.player.write_gate`），离开中与已迁走的玩家发来的请求在到达 handler 之前被拒、连接被关。工程已经有自己的写闸门时模块不抢，只记一条 Warn——那个闸门要自己调 `rehome.AdmitMessage`，否则离开中的玩家的新请求不会被拒。
- **受理之后中止也会断开会话**。客户端在受理之后唯一的信号就是断线后重新选角。
- **运维绕过模块直接用 `accountctl` 换服**时没有记意图：目标服不做到达（不起冷却、不跑到达钩子）。
- **前提**：业务服务要 `uses: [account]`、有 `nest` Mod、有玩家 TCP 接入；缺省实现还按能力名取玩家 TCP 传输（断开会话）与 DataEngine Mod（交出实体）。没装 `enter` 的工程要自己在加载 Player 之前调 `Admit`、进入之后调 `Arrive`，否则换走的玩家仍能进旧服、目标服不做到达。一次换服涉及源服、目标服两个 game 进程与 account；模块的每个入口只在本进程内生效。
- **已知限制**：[KNOWN-LIMITS“单个玩家换服（模块 rehome 与 game-demo 接线）”与“单个角色换服（account 一侧）”](../../docs/maintenance/KNOWN-LIMITS.md)；设计与取舍见[换服方案](../../docs/maintenance/FEATURE-2026-11-11-player-rehome.md)，account 一侧的接口与运维手册见 [PLAYER-MIGRATION §7](../../docs/framework/PLAYER-MIGRATION.md)。

## 复用方式

- **需求正好是它**：`roost module add rehome`。它不依赖别的模块；清单要求业务服务 `uses: [account]`、有玩家 TCP 接入。几乎总是要同时装 `enter`（两个都装着时自动接上进入前检查与到达）；要 GM 命令再装 `gm`。
- **需求比它多一点**（改留给工程的文件、登记钩子）：
  - 可去的服、冷却：改 `game/rehome/rehome_rules.go` 的 `TargetAllowed` 与 `Cooldown`。至少要保证 `to` 与 `from` 把玩家的逻辑库映射到同一个 Mongo、同一个前缀（见“搬档的前提”）。
  - 换服费：在自己的文件里登记 `rehomecomp.OnRehomeApplying`（只读核对付得起）与 `rehomecomp.OnRehomeArrived`（扣费，与到达同一笔事务）。不要在换服前检查里扣。
  - 某种状态下不许换服（在队伍里、在排队、等级不够）：只看玩家自己的档的，登记 `rehomecomp.OnRehomeApplying`；要问别的服务或进程内状态的，登记 `rehome.OnApplying`，并在 `rehome.OnLeaving` 里再问一次。
  - 离开前清理（离开场景、取消排队、退订）：登记 `rehome.OnLeaving`。做的事要么“留在本服也无妨”，要么自己能重做。
  - 到达后重置（位置回出生点、清只在原服有意义的状态）：登记 `rehomecomp.OnRehomeArrived`。
  - 有自己的驻留表或写闸门：发布 `game.rehome.residency`，或在自己的闸门里调 `rehome.AdmitMessage`。
  - 自己改写了进入端点：加载 Player 之前调 `DriverFromRegistry(registry)` 的 `Admit`，进入成功之后调 `Arrive`；用 enter 模块的登记点时直接调它的 `RunEntering` / `RunEntered`。
  - 不要改的文件：`rehome_driver.go`（步骤顺序与结论判定）、`rehome_directory.go` 的 `judge` / `classify`、`rehome_residency.go`、`rehome_admit.go`、`rehome_binding.go`，以及 `RehomeComponent` 的 `Prepare` / `Arrive` / `Cancel`。
- **别的模块 / 业务代码要用它**：
  - 后台步骤执行前问归属：`sid, moving, err := rehome.Binding(ctx, registry, playerID)`。
  - 服务端发起换服：`controller.Move(ctx, playerID, to, note)`。
  - 在同一个 Player 上读换服历史：`owner.RehomeComp().LastAt()`、`Count()`；判断“有没有换服意图”：`owner.Dao().GetRehome().GetPendingID() != ""`。
  - 让自己的某条消息在换服期间仍能得到带码的回答：`rehome.ExemptFromGate(msgid)`。
- **不适合直接用、只适合借鉴的情形**：
  - 各服用不同的 Mongo（跨集群搬数据）：缺省的搬档不适用，要实现自己的 `Documents`（跨集群复制没有事务，幂等与中止要自己处理）。
  - 绑定权威不是 roost 的 account：可以实现 `Directory` 接上，但四个方法的幂等与拒绝语义要与 account 的一致（见 `rehome_directory.go` 的注释）。
  - 值得借鉴的做法：先在本进程立闸再去问权威；先排空再复核再交出；写权“交出”之后再让权威改绑；结论只看权威的当前状态而不看某次调用的错误码；结果未知一律重发而不是猜。

## 联动

本模块的 `with/`：

- `rehome` + `enter`（`with/enter/game/controllers/rehome/rehome_enter.go`）：登记 enter 的进入前检查（`Admit`：正在换服答 `rehome_moving`，绑定在别的服答 `rehome_elsewhere`，判断不了不放行；源服重启后在这里把停在半路的换服接着做完）与进入之后的钩子（`Arrive`：到达与意图对账，失败只记日志、不让登录失败）；并把进入游戏那条消息（`EnterGame`）登记为不受写闸门约束。enter 的新建角色消息 `EnterCreate` 也会跑同一组进入前检查，所以联动给 `EnterGame` 与 `EnterCreate` 两条消息都登记了写闸门豁免：迁出表里的玩家发它们，拿到的是进入前检查的带码回答，连接不会先被闸门关掉。
- `rehome` + `gm`（`with/gm/game/gm/rehome.go`）：运维命令 `gm.player.rehome`，payload `{player_id, to, note}`，调 `Move`；返回 `accepted` 只表示已受理，最终状态看 account 的角色记录。

别的模块的 `with/rehome`：

- `team` + `rehome`：在队伍里不能换服；档里有换服意图时不能建队或入队。
- `matchmaking` + `rehome`：有匹配状态时不能换服；有换服意图时不能排队。
- `alliance` + `rehome`：联盟的原生步骤在执行前用 `rehome.Binding` 核对玩家当前归哪个服，换服中等待、已换服改投。

## 验证范围

- `roost module verify rehome`：44 条场景（`game/rehome` 33、`game/handler` 8、`game/controllers/rehome` 3）；与 `enter` 的联动 5 条，与 `gm` 的联动 4 条。
- **全部是离线的**：account 是真实的 `service/account` 服务但存储在进程内；Nest 是真实的引擎加替身提交器；玩家传输与 DataEngine 是只记事件的替身，所以“断开会话”“交出实体”与“搬档”都没有经过真实实现（搬档的 DataEngine 一侧另有框架单测与真实 Mongo 副本集上的 `TestRealMongoLocalPreloadAndCopyAcrossServers`）；两个服共用同一个进程内的 account 与同一份玩家档。没有 NATS、Redis、Mongo。
- 覆盖：整条顺序、各种拒绝不写任何东西、发起被拒清意图、发起结果未知用中止拿结论、离开钩子与交出失败时中止、提交结果未知重发直到有结论、提交被拒转中止、搬档失败中止、中止时删目标副本、删除失败重试、补搬冲突时保留源档、撤屏障忙时重试、迁出表一人一行、`Binding` 先表后 account、离开中的玩家在闸门被拒、排空等待先前的请求、排空不完中止、复核不过中止、应答丢失后按当前绑定落定、对不上的绑定继续问、`Admit` 的三种回答、换回来的玩家重新放行、源服重启后续做、到达只做一次、意图对账、GM 入口。
- **没有验证**：真实的 `HandOffEntity` 与经模块走到的真实搬档；两个真实 game 进程加 account 的演练（含每个阶段之后 kill -9）；等 WAL checkpoint 实际要多久；独立 Gate 部署；account 长时间不可用。
