# alliance：联盟（独立联盟服务单写者、game 侧端点与只读镜像、建盟 / 捐赠 / 领奖三个跨域 Saga）

`roost module add alliance` 安装独立的联盟服务、game 侧的协议端点、联盟只读镜像、`Player` 上的展示组件与三个 Saga；依赖 `bag`。新工程无需手写接线即可生成、build 和 verify，运行需要 Mongo / Redis / NATS / etcd。鉴权、连接监听与实际部署仍由工程配置。

## 能力

联盟数据不在 game 进程里。每个联盟（`Alliance`）和每名玩家的“成员守卫”（`AllianceMember`，保证一名玩家同时只在一个联盟）都归独立的 `alliance` 服务所有，只有拥有它的那个联盟服务实例能写。game 进程通过三条路与它打交道：只改联盟的操作发一次 RPC；同时要改玩家背包的操作走 Saga（跨两个提交域的补偿式流程）；读联盟走只读镜像。

- **客户端协议**（`protocol/def/alliance.go`、`alliance_manage.go`、`alliance_notice.go`；端点在 `game/controllers/alliance/alliance.go` 与 `alliance_manage.go`）
  - `FoundAlliance`（请求 `Name`、`OpenJoin`）：建盟。启动 `alliance_found` Saga：先扣建盟费，再让联盟服务建盟；被拒则退费。应答只带 `SagaKey`，表示已受理。
  - `JoinAlliance`（请求 `AllianceID`）：加入一个开放加入的联盟，一次 RPC，应答带加入后的联盟信息。不开放的联盟答 `alliance_denied`，改用 `ApplyAlliance`。
  - `LeaveAlliance`：退出当前所在的联盟，一次 RPC。盟内还有别的成员时盟主要先转让才能退（`alliance_leader`）；**盟主是最后一名成员时可以直接退出，联盟随之解散**（见能力边界“解散”）。
  - `ApplyAlliance`（请求 `AllianceID`）：向一个不开放加入的联盟提交申请，等盟主处理。同一联盟已有待批申请时答 `alliance_application`。申请不占用玩家的守卫，展示副本不变。
  - `ApproveAllianceApply`（请求 `PlayerID`）：盟主批准一份申请，申请人入盟；在线的申请人收到 `AllianceNotice`（`joined`）。
  - `RejectAllianceApply`（请求 `PlayerID`）：盟主拒绝一份申请。申请人不会收到通知，可以再申请。
  - `KickAllianceMember`（请求 `PlayerID`）：盟主把一名成员移出联盟；在线的被踢者收到 `AllianceNotice`（`kicked`）。
  - `TransferAllianceLeader`（请求 `PlayerID`）：盟主把盟主交给另一名成员，自己留在盟内成为普通成员。
  - `AllianceRoster`（请求 `AllianceID`、`MinVersion`；应答 `AllianceRosterResponse`）：从镜像读名单。`AllianceID` 为 0 读自己所在的联盟。应答带 `LeaderID`、`OpenJoin`、`Version`、`Members`（每项 `PlayerID`、`Role`：1 成员 / 2 盟主、`Contribution`，按玩家号排序）与 `Applicants`（待批的申请人；只下发给镜像里的盟主本人）。
  - 上面四个盟主操作都作用在调用者自己所在的联盟（取自展示副本）上，game 端点只转交；“是不是盟主、对方在不在、有没有申请”由联盟服务按名单裁决，拒绝码是 `alliance_denied` / `alliance_missing` / `alliance_application` / `alliance_membership` / `alliance_full`。
  - `AllianceInfo`（请求 `AllianceID`、`MinVersion`）：读联盟镜像；`AllianceID` 为 0 表示读自己所在的联盟；`MinVersion` 填上一次写操作应答的 `Version`，可保证读到的不比那次写更旧。
  - `DonateAlliance`（请求 `Amount`）：捐赠。启动 `alliance_donate` Saga：先扣道具，再记入联盟资金与个人贡献；被拒则退还。
  - `ClaimAllianceReward`：领取本周期的联盟奖励。启动 `alliance_reward` Saga：联盟服务先标记已领，再由 game 发道具；背包放不下则撤销标记。
  - `AllianceOpStatus`（请求 `Op`、`SagaKey`）：查询上面三个 Saga 的进展。`Op` 是 `found` / `donate` / `reward`；应答带 `Status`、`Step`、`LastError`、`AllianceID`。只能查自己发起的。
  - `AllianceNotice`：服务端推送 `AllianceNoticePush`（`AllianceID`、`Change`、`Version`），在线成员的成员关系变化（`joined` / `left` / `kicked`）时发出。
  - 联盟应答 `AllianceResponse` 的字段：`AllianceID`、`Name`、`Members`（人数）、`ServedBySID`、`LeaderID`、`Funds`、`Version`、`SagaKey`。
- **服务端入口**
  - 联盟服务 RPC 接口 `Alliance`（`internal/rpc/alliance/alliance.go`，能力名 `rpc.alliance`）。写方法第一个参数都是 `requestID`（调用方为一次用户操作生成一个，重试时保持不变）：
    - 面向 game 的写：`Found`、`Apply`（申请）、`Approve`（盟主批准）、`Reject`（盟主拒绝）、`Join`、`Leave`（最后一名成员是盟主时即解散）、`Kick`（盟主踢人）、`Transfer`（转让盟主）、`Donate`、`DeclareWar`（盟主宣战；没有客户端入口）、`ClaimReward`、`ReleaseReward`、`RefreshHome`（修正成员的通知地址）。
    - 读：`Info`（权威读一个联盟）、`Guard`（读守卫原样）、`Membership`（按名单校正后回答玩家在哪个联盟）、`Result`（查某个 `requestID` 的结果）。
    - 联盟服务实例之间用的：`Establish`、`ReserveGuard`、`ResolveGuard`、`ClearGuard`、`AcknowledgeWar`、`Settle`；game 进程不需要调。
    - 目标号构造：`AllianceTarget(allianceID)`、`GuardTarget(playerID)`（`internal/rpc/alliance/service.go`）。
  - game 侧 `Controller`（`game/controllers/alliance/controller.go`，经 `FromRegistry` 取得）：除十三个 `Handle…` 端点外，公开方法 `RefreshMembership(ctx, playerID) error`（`refresh.go`）在进入或重连后对账玩家的展示副本与通知地址。
  - `Player` 组件（`game/components/alliance/alliance.go`）：`AllianceComp().AllianceID()`、`SetAlliance(id)`，窄接口 `IAllianceEntity`。
  - game 侧 Nest handler（`game/handler/`）：`handlerPlayerAlliance`（读展示副本）、`handlerShowAlliance`（改展示副本，`replaces` 非零时只替换指定的旧联盟）、`handlerStartAllianceOp`（在 `Player` 事务里启动 Saga）、`handlerAllianceDebit` / `handlerAllianceCredit`（Saga 的背包扣除与放入步骤）。
  - `game/alliance` 包：`Answer(err)`（把 RPC 错误映射成客户端码）、`Binding(ctx, registry, playerID, sid)`（查玩家当前归属的 game 实例）、`BusinessKey` / `RequestID`（幂等键格式）、`State` 及 `Encode` / `Decode`（Saga 载荷）、常量 `Deadline`（Saga 期限 10 分钟）。
  - `game/alliance/runtime` 包：运行期入口 `Start(registry)`（启动 Saga 步骤消费者与成员通知处理），`AdmitPlayerStep`（Saga 原生步骤进 Nest 之前的归属准入，可供自定义消费者复用）。
  - 运维命令（联盟服务进程的 ops `/admin`，`internal/service/alliance/admin.go`）：`alliance.info`、`alliance.guard`、`alliance.transfer`（把本实例拥有的联盟与守卫迁给另一个实例）、`alliance.stop_founding`（本实例不再接新建盟请求，缩容第一步）。
- **登记点 / 钩子**
  - `PlayerBindingCapability` 与接口 `PlayerBinding`（`game/alliance/binding.go`）：工程有自己的玩家驻留表时，实现 `Binding(ctx, playerID, sid) (sid, moving, err)` 并在联盟消费者启动之前发布这个能力，替换缺省的静态归属判断。
  - 没有“入盟后 / 退盟后”的业务回调登记点。
- **状态**
  - `PlayerDao.Alliance`（`AllianceState{ID}`，`db/def/player_alliance.go`，`persist,sync`）：玩家所在联盟号的展示副本，下发给本人。它不是权限依据。
  - `PlayerDao.SagaReceipts`：Saga 原生步骤的回执，与背包变化同一笔事务（清单的 `sagas[].receipts`）。
  - `AllianceDao`（集合 `alliance`，`db/def/alliance.go`）：`Name`、`LeaderID`（0 表示没建成或已解散）、`OpenJoin`、`Funds`、`CreatedAtUnix`（建盟时间，秒；解散后保留，标记这个号用过）、`Members`（玩家号 → 席位：角色、加入时间、`HomeSID`、贡献、入盟请求号）、`Applications`、`Wars`、`WarAcks`、`Requests`（请求回执）。
  - `AllianceMemberDao`（集合 `alliance_member`，`db/def/alliance_member.go`）：玩家的守卫——所在联盟、入盟请求号、待定预约、上次退盟时间、已领奖励的周期、请求回执。不生成镜像快照。
  - game 侧只读镜像 `AllianceMirror`（`game/entities/alliance/mirror.go`）：名称、盟主、是否开放、资金、成员、申请人、交战列表。
- **配置**
  - 联盟服务的配置段 `alliance.*`（`internal/rpc/alliance/settings.go` 的 `Settings`）：`max_members`（缺省 100，硬上限 500）、`reservation_ttl`（缺省 30s）、`receipt_retention`（缺省 168h）、`rejoin_cooldown`（缺省 0，关闭）、`reward_item_id`（缺省 1002）、`reward_count`（缺省 5）、`reward_period`（缺省 24h）。
  - 留给工程改的业务常量：`game/alliance/rules.go` 的 `CoinItemID`（建盟与捐赠用的背包道具号，缺省 1003）；`game/controllers/alliance/controller.go` 里的 `foundCost`（建盟费，缺省 100）。
  - 规则常量 `game/alliancerule/alliancerule.go`：`MaxNameBytes = 48`、`MaxWars = 8`、角色 `RoleMember` / `RoleLeader`。
- **错误码**
  - 业务拒绝（`internal/errors/alliance.go`，同一个 `requestID` 重发得到相同的拒绝）：`alliance_name`、`alliance_missing`、`alliance_full`、`alliance_denied`、`alliance_membership`、`alliance_application`、`alliance_leader`、`alliance_amount`、`alliance_cooldown`、`alliance_war`、`alliance_expired`、`alliance_reward`。
  - 给客户端的四个状态码（各自一个文件）：`alliance_busy`（没有执行，原样重发）、`alliance_moved`（联盟或玩家正在迁移，原样重发）、`alliance_pending`（结果未知，原样重发会从回执得到答案）、`alliance_view_missing`（联盟不存在，或玩家不在任何联盟）。
  - `alliance_request_invalid`（`internal/rpc/alliance/alliance.go` 的 `ErrRequestInvalid`）：RPC 请求无法解码、缺请求号或目标种类不对。
  - 背包不足时直接返回 `bag` 模块的 `bag_short`。

**日志**：建盟、申请 / 审批 / 入盟 / 退盟 / 踢人、转让、捐赠、领奖、宣战确认，以及实际成员变更；记录业务键、玩家、联盟、数量、版本或重放标记。 玩家成功操作使用框架 `slog.InfoContext`（无 ctx 时 `Info`），非玩家低频运维 / 生命周期使用 `Debug`。记录位于 `internal/rpc/alliance/guard.go`, `internal/rpc/alliance/membership.go`, `internal/rpc/alliance/service.go`, `internal/rpc/alliance/war.go`；成功日志仅在提交返回后写，幂等确认可能重复，不能当资产流水。

## 能力边界

- **宣战没有客户端入口**：联盟服务有 `DeclareWar`（盟主宣战，同实例一笔事务、跨实例最终双侧），但服务侧没有结束战争的能力——一旦宣战，这条记录永久占用双方各自 8 个交战名额中的一个。只有“开战”没有“停战”的入口对玩家是单向门，所以模块不给客户端开放宣战；要用的工程先在联盟服务补上结束战争，再照 `HandleKickAllianceMember` 加端点。
- **解散只有一条路**：没有“盟主一键解散”的入口。盟主把别的成员踢出或等他们退出之后，作为最后一名成员退出，联盟即解散。解散后的联盟没有盟主：`Info`、申请、加入、捐赠一律答 `alliance_missing`，镜像读答 `alliance_view_missing`；待批的申请随解散清空（不通知申请人）；**资金不退还、不结算**；交战记录留在双方的记录里（对方仍占一个交战名额）；这条联盟记录不删除（仍是联盟服务启动时预加载的一个实体，量大时计入预加载耗时），它的号不会再被建成另一个联盟。
- **申请流程的边界**：申请不占用守卫，玩家可以同时向多个联盟申请，也可以在申请期间加入或创建别的联盟——之后的批准会被拒（`alliance_membership`），那份申请留在原处等盟主拒绝。申请人不能撤回申请；被拒绝时没有通知；待批申请数没有单独的上限（每名玩家在一个联盟至多一份）。`AllianceRoster` 的申请人列表按镜像里的盟主下发，镜像滞后时刚转让的新盟主可能要过一会儿才看得到。
- **不做的事**：没有结束战争、没有联盟列表与搜索（`AllianceID` 从哪来不在模块范围内）、没有联盟聊天、没有职位体系（只有盟主与成员两种角色）、没有联盟等级与科技、没有改名与改“是否开放加入”的入口。联盟名不查重、不查敏感词，只拒绝空白、超过 48 字节、含控制字符或非法 UTF-8 的名字。“交战”只是一条记录，模块不赋予它任何效果。联盟资金只增不减，没有花费入口。
- **数量上限**：每个联盟成员数由 `alliance.max_members` 限制（硬上限 500；整份名单是一个文档整体写入）；同时交战至多 8 个联盟；领奖每个 `reward_period` 一次；每次捐赠数量为正的 `int32`。
- **一致性与幂等**
  - 联盟服务的每个写都带请求号，回执与变更同一笔事务提交；同一个请求号重发得到相同结果（包括拒绝）。回执保留 `receipt_retention`，过期后同一请求号会被当作新请求。
  - 客户端请求的幂等键是“操作 + 玩家号 + 会话号 + 帧序号”：同一帧重发折叠成同一次操作；重连后新会话里重发是新的一次。
  - 三个 Saga 不是一笔事务：扣费与建盟 / 捐赠之间、标记领奖与发奖之间有中间状态，靠补偿收敛。`Player` 侧步骤的回执与背包变化在同一条持久记录里，重投不会多扣或多发。Saga 期限 10 分钟，超时后失败并补偿已完成的步骤。
  - 两种补偿失败需要人工处理：退费或退还捐赠时背包放不下（协调器标记为需人工）；联盟步骤在 Saga 已放弃之后才成功（结果保留、道具已退，只记一条警告日志）。
  - 启动 Saga 时对道具数量的检查只是预检，真正的扣除在后续步骤；期间道具被花掉则 Saga 失败、无需补偿。
- **尽力而为的部分**：成员通知（`AllianceNotice`）至多发一次，只发给在线玩家，可能丢。`PlayerDao.Alliance.ID` 展示副本靠自己的操作、通知与进入时对账更新，可能短暂过时；所有决定以联盟服务的名单为准。镜像读是最终一致的。
- **跨实例**：一名玩家的守卫与他要加入的联盟可能在不同的联盟服务实例上。入盟先预约守卫、再由联盟名单裁决、最后确认守卫；中途失败由预约期限或下一次请求修复，不靠时钟单方面释放。跨实例宣战先记本方，对方一侧最终补上。
- **前提**
  - 依赖 `bag` 模块；需要 `nest`、`saga`、`remote_mirror` 三个 Mod 与玩家 TCP 接入、自身视图（安装时代办）。
  - 新增独立的 `alliance` 服务（带 `dataengine`、`etcd`、`mongo`、`nats`、`nest`、`redis`、`remote_entity`），开发配置用独立的 WAL 目录 `data/wal/alliance`。同机第二个联盟实例必须独占 SID、端口与 WAL 目录。
  - game 进程不能认领 `Alliance` 与 `AllianceMember` 两种实体。
  - 工程已有同名的 `alliance` 服务或同名 Saga 时拒绝安装。
- **已知限制**：`docs/maintenance/KNOWN-LIMITS.md` 的“业务模块 R10 联盟的验收范围”一节，以及“路由 RPC 与 Redis 路由登记的边界”“实体服务的 RPC”相关条目（同一联盟的请求逐个处理，热点联盟的吞吐受单个事务耗时限制）。

## 复用方式

- **需求正好是它**：`roost module add alliance`，连带安装 `bag`，新建 `alliance` 服务，写入三个 Saga 定义。
- **需求比它多一点**
  - 改建盟费、货币道具：改 `controller.go` 的 `foundCost` 与 `game/alliance/rules.go` 的 `CoinItemID`。
  - 改成员上限、奖励、冷却：改联盟服务配置的 `alliance.*` 键，不改代码。
  - 再加一个只改联盟的客户端入口（例如补上结束战争之后的宣战）：新增协议与端点，照 `alliance_manage.go` 的 `manageAlliance` 写——用 `alliance.RequestID` 生成请求号、`controller.boundSID` 核对归属、调 `controller.alliances` 上对应的 RPC 方法、用 `allianceRefusal` / `allianceReply` 组应答；权限判断留在联盟服务的 handler 里，端点不读镜像判断角色。
  - 改“谁能看申请人列表”：改 `HandleAllianceRoster` 里那一处筛选（缺省只给镜像里的盟主）。
  - 改“盟主什么时候能退”：改 `game/entities/alliance/roster_component.go` 的 `CheckRemoval` 与 `Remove`（缺省：还有别的成员时拒绝；只剩自己时退出即解散）。
  - 改名字规则：改 `game/alliancerule/alliancerule.go` 的 `ValidateName`（端点与联盟服务用同一个函数）。
  - 自有玩家驻留表：实现 `PlayerBinding` 并发布 `PlayerBindingCapability`。
  - 不建议改：`internal/rpc/alliance/` 下的守卫预约与对账（`guard.go`、`membership.go`）、请求回执（`alliancerule.Execute`）、所有权迁移（`transfer.go`）。
- **别的模块 / 业务代码要用它**
  - 读玩家在哪个联盟（仅作展示或预检）：持有 `Player` 锁时 `target.AllianceComp().AllianceID()`；没有持锁时 `syncsender.NewPlayerAllianceSender(client).Sync_PlayerAlliance(ctx, 完整实体号)`。
  - 要权威答案（例如发联盟奖励前核对成员资格）：取得 `rpcalliance.Alliance` 客户端，调 `Membership(ctx, GuardTarget(playerID))` 或 `Info(ctx, AllianceTarget(allianceID))`。
  - 读联盟展示信息：照 `Controller` 的 `allianceMirror` 用镜像读。
  - 自定义进入流程：在进入成功后调 `controller.RefreshMembership(ctx, playerID)`。
- **只适合借鉴的情形**：需要“一个进程内的小公会”（成员几十人、不跨服、不要求高可用）时，这套独立服务过重；值得借鉴的是“名单为唯一权威、守卫只做一人一盟的互斥、每个写带请求号与回执、跨提交域才用 Saga”这几条划分。

## 联动

- `alliance` + `enter`（`with/enter/game/controllers/alliance/enter.go`）：经 `gamelogin.OnEntered("alliance", …)` 在每次进入或重连后调 `RefreshMembership`；失败只记日志，留待下次进入。
- `alliance` + `rehome`（`with/rehome/game/alliance/rehome.go`）：把归属查询换成 `rehome.Binding`，Saga 的 `Player` 步骤进 Nest 之前先核对玩家当前在哪个 game 实例——换服中则等待，已换服则改投，结果未知不放行。
- `activity_alliance` 以 `requires` 依赖它，在 `Alliance` 实体上追加活动状态与组件。
- 别的模块没有 `with/alliance`。

## 验证范围

- `roost module verify alliance`：55 个场景（`./internal/rpc/alliance`、`./internal/service/alliance`、`./game/handler`、`./game/alliance/runtime`、`./game/controllers/alliance`、`./db`，`^TestAlliance`）；与 `enter`、`rehome` 的联动各 2 个场景。
- 场景是离线的。真实的部分：每个实例的 Nest 引擎、生成的 Sender 与分发、handler 与实体规则、生成的路由 RPC 客户端与服务端处理（路由检查、一次改投）。替身的部分：进程内的测试总线、内存的 Remote 所有权表与提交器；Saga 场景里没有真实的协调器，用例按协调器的顺序逐步调用步骤函数，联盟服务一侧是桩。
- 覆盖：一人一盟（单实例与跨实例）、守卫并发创建只有一个属主、预约期限由联盟裁决、建盟在另一实例重试不产生第二个联盟、同实例宣战一笔事务与跨实例最终双侧、路由检查先于 Nest、写错误映射成四种应答、所有权迁移、守卫的各种丢失后修复、通知地址的条件更新、回执重放、权限、成员上限、奖励每周期一次与撤销、原生步骤每次操作只生效一次、建盟被拒退费、发奖被拒撤销标记、归属准入、成员通知到达在线成员、进入对账的四种情况、展示副本落库；最后一名成员（盟主）退出即解散（同实例与守卫在另一实例两种、有成员时仍拒绝、解散后一律 `alliance_missing`、申请清空、号不再被建成联盟、原盟主可再建盟）；八个端点场景（建盟两条、加入、读联盟、申请只转交且不动展示副本、四个盟主操作只转交并原样带回服务的拒绝、名单里申请人只给盟主、退出成功才清展示副本）；`alliance.stop_founding` 只退出建盟的队列组、可重复执行、路由 RPC 仍到达属主。
- `internal/service/alliance` 的 `TestAllianceStopFoundingSendsNewAlliancesToTheOtherInstances` 有两种跑法：设了 `ROOST_DATAENGINE_IT_NATS_URL` 时在真实 NATS 上跑（`scripts/test-demo-generated.sh` 的入口）；没设时同一套步骤跑在进程内的队列组替身上，不跳过——`roost module verify` 走的是后一种，验证的是模块自己的部分（命令退出的是哪个方法的队列组、重复执行、生成的客户端与 handler），队列组本身的行为这时是替身的，不是 NATS 的。
- 2026-11-12 补充真实单机进程验收：独立 account / game / alliance，真实 Mongo 副本集、Redis、NATS JetStream、etcd；两个账号经 Portal + TCP 新建进入，完成建盟 Saga、申请、拒绝、重新申请、批准、成员名单、转让、踢人、末位盟主退出解散。见[本轮修复证据](../../docs/maintenance/evidence/module-fixes-20261112/README.md)。未验证多实例故障、跨机、独立 Gate、强杀恢复与容量；较早离线证据见[模块初始验收](../../docs/maintenance/evidence/module-alliance-20261112/README.md)。

## 权威与调用链

- `Alliance` 与 `AllianceMember` 是归 `alliance` 服务的 Remote 托管实体，分别保存名单与成员守卫，只有联盟服务实例能写。game 即使因为 `team` 模块装了 `remote_entity`，也不能认领这两种实体。
- 只改联盟的 `Join` / `Leave` / 申请批准 / 踢人 / 转让 / 宣战走路由 RPC；同实例的多实体写仍在同一笔 Nest 事务里。跨实例入盟保留预约、名单裁决与守卫惰性修复，不用 Saga 包住每次调用。
- 建盟扣费、捐赠、领取奖励涉及 `Player` 与联盟两个提交域，才使用 `alliance_found` / `alliance_donate` / `alliance_reward`。`Player` 步骤按 sid 路由，原生回执与背包同一笔事务；联盟步骤靠 RPC 请求号去重。
- 协议号在安装时分配，客户端以生成的消息号与 PB 为准。
- `AllianceInfo` 用单调的镜像读，写后可带 `MinVersion`。成员守卫不生成 Remote 快照。

## 跨服与惰性校正

与 `enter` 同装时，每次进入或重连调用 `Controller.RefreshMembership`。全部查询共用 1 秒预算：先在 I/O 上下文询问当前归属与权威的 `Membership`，再经 Nest 更新 `Player` 的展示副本。只有确定“守卫实体不存在”才清空展示；超时或服务不可用时保留展示并把错误交给调用方记录，不阻塞登录成功。

与 `rehome` 同装时，Saga 在进入 `Player` 的 Nest 之前先查归属：换服中退回等待，已到别服改投当前 sid，结果未知时重投。未装 `rehome` 的工程用可信会话或持久载荷里的静态 sid。

联盟成员允许换服。对账拿到相同入盟批次的镜像后，经 `RefreshHome` 比较入盟请求号和旧的 `HomeSID` 再更新通知地址；退盟重入、旧 SID 已被替换都不会被迟到的请求改写。`HomeSID` 只是通知的投递提示，不参与写权限；归属查询与这次 RPC 不是一笔事务，快速往返换服后可能暂时留下旧提示，由后续进入时的对账继续修正。

## 安装与文件所有权

安装器在一次模块写入里新增 `services.alliance`、`uses_rpcs`、实体生命周期、Saga 定义和 `Player` 的原生回执；生成失败时恢复这些文件，重复安装不改工程。已有的只读 `remote_mirror` 升级为 `remote_entity` 时去掉重复的 Mod，其他服务的读能力保留。

game-demo 只把规则里的货币道具覆盖成它已有的道具 9001，并在 `internal/service/game/alliance.go` 里先发布自己的归属适配、再手动启动模块的运行期入口（demo 的工程清单关闭了这个模块的自动启动，避免启动两份消费者）。模块没有为旧的 guild 接口、字段或旧生成工程保留兼容层。

只读声明：AllianceInfo / AllianceReceipt / GuardInfo / GuardReceipt 使用 `//roost:nest durability=memory`，仍在正式 Nest 实体锁内执行，不取持久状态回滚快照、不写 WAL。裸 `//roost:nest` 的默认值是 state / async，不能与 memory 混同；带状态推进、回执写入或引用校正的入口保持写事务声明。
