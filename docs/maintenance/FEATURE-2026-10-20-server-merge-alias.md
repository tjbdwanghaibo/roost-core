# 功能设计：别名合服（2026-10-20）

**状态：提案，待维护者评审，未实施。** 对应 [DESIGN-TODO D15](DESIGN-TODO.md#d15-合服把一个服并入另一个服)。来源：新用户试用第八轮（游戏服务器专家视角）登记的“合服没有框架支持”；维护者 2026-10-20 的意见：合服是停服窗口内的**离线**操作（A、B 都停机排空），`dbscope=sid` 只表示该逻辑库的数据归一个 sid 独占，与“能不能合服”无关，合服时作为离线迁移步骤处理。

本篇由审查按 main（`8ca14004`，D9 修订版 A 与映射冲突拒绝启动之后）的源码重新核对、改写，§1.4 列出对原提案事实陈述的更正，§6.3 补上原提案没有考虑到的约束（D9 库名映射、outbox、总线与持久化里的 sid）。源码行号按该基线，实施前对照当时的 HEAD 再核一次。

## 1. 现状

### 1.1 玩家与服的绑定只有两处

- **account 的角色记录**：`Role.ServerID`（`service/account/types.go:261`）在建角时写定，之后不变；会话 `Session.ServerID`（`types.go:283`）由 `SelectRole`（`service/account/service.go:267`）发出，`ValidateSession`（`service.go:307`）每次按存储返回角色记录。
- **Gate / game 按会话里的 sid 静态判断**：
  - 独立 Gate：`AccountAuthenticator` 拒绝 `role.ServerID <= 0`，用它填 `Principal.ServerID`（`wiring/gate/auth.go:33-37`）；`Gate.BeforeACK` 按它 `ResolveGame` 并要求两边 sid 一致（`infra/network/gateway/gate.go:179-183`）。
  - 嵌入 TCP（game-demo）：认证器把 `role.ServerID` 写进 `Claims[svcaccount.ServerIDClaim]`（`demo/internal/access/player/tcp/auth.go.tmpl:87`、`:101`），登录端点 `BoundServerID`（`demo/game/controllers/player/enter_game.go.tmpl:177`）读它与本进程 sid 比较，不等即 `player_elsewhere`。
  - 全仓非测试代码里读 `Role.ServerID` 的只有上面两处（`rg '\.ServerID\b'` 核对；`infra/network/gateway` 其余命中是进程身份 `Identity.ServerID`）。

玩家**数据**的位置由 DAO 标记决定：`roost add dao` 骨架与 game-demo 的 Player / Guild / World DAO 都是 `db=main`、不带 `dbscope=sid`（`demo/db/def/*.go.tmpl`）。是否“所有服的玩家文档在同一个物理集合”取决于部署：见 §6.3 第 1 条。

### 1.2 合服的硬阻碍

1. **双角色 slot 冲突**：一个账号一个服一个角色，由 insert-only 的 slot 保证，键 `slotKeyFor(accountID, serverID)`（`service.go:257`）。同一账号在 A、B 各有角色时，把 A 的角色 `ServerID` 改成 B 必然冲突。
2. **没有改绑接口**：`cmd/accountctl` 只有 `upsert-server`（`demo/cmd/accountctl/main.go.tmpl:45`），account 没有修改 `Role.ServerID` 的方法。
3. **一个进程只认一个 sid**：会话 sid 静态比对；`dbscope=sid` 的物理库是 `<物理名>_<本进程 sid>`（`framework/dataengine/engine/mongo_store.go:289` `DatabaseForSid`）；单实例锁按 `server_type + sid`；World 单例的 UniqueID 就是 sid（`codegen/internal/roost/framework_services.go:534` `WorldUniqueID`）。

### 1.3 可以复用的部分

`cmd/sidmove`（迁服检查工具，game-demo）的 `wal-drained`、`singleton`、`players -out / -compare` 可以直接作为合服前置门槛：停机后 WAL 全部投影确认、单实例锁已释放、空窗期没有第三个写者。`players` 的物理库经 `kitdataengine.LoadDatabaseMapFile`（`wiring/dataengine/databases.go:55`）按服务配置换算，与 game 进程同一条规则。

### 1.4 对原提案事实陈述的更正

| 原陈述 | 核对结果 |
| --- | --- |
| `Role.ServerID` 在 `types.go:252` | 252 是 `type Role struct`，字段在 261；`Session.ServerID` 在 283 |
| “所有服的玩家文档在同一个物理集合里”，合服不需要搬玩家文档 | 只在 A、B 的部署把每个非按 sid 的逻辑库映射到**同一 Mongo 上的同一物理库**时成立。D9 修订版 A 之后物理库是每个服务配置自己的 `dataengine.databases`，A、B 可以不同（§6.3 第 1 条） |
| `sidmove wal-drained` 证明“排空” | 只证明 WAL 已全部投影确认；DataEngine 停机时已暂存、未发布的 effect 留在 Mongo outbox“等下次启动”（`framework/dataengine/engine/runtime.go:165`），A 不再启动就没人发（§6.3 第 2 条） |
| `_id` 冲突举例“以 sid 为 UniqueID 的单例会撞” | 不会：UniqueID = sid 的文档在 A、B 的 `_id` 本来就不同。会撞的是在各服取同一个常量 ID、或用按服（非全局）序列发 ID 的文档。World DAO 是 `db=main`、不按 sid 分库，World(A) 文档本来就在共享集合里，不在 `sidmerge` 搬迁范围 |
| 记录改动“沿用 account 现有的 admin 审计方式” | account 没有独立的审计日志；现有做法是把操作时间与备注写进记录本身（`ResolvePendingCreation`，`service/account/admin.go:91`、`:146`）。`MergeServer` 同样把合服时间与备注写进服务器记录，另打 Info 日志 |
| 新增 `ErrServerNotFound`（“现有或新增”） | 没有这个错误；现有的是 `ErrServerInvalid`（“server %d is unknown”，`types.go:93`），沿用它 |
| `ServerIDClaim` 的含义改为承载服，“现有比较逻辑不变” | 比较逻辑不变成立；`types.go:288-295` 的契约注释写的是“Role.ServerID”，要同步改 |

## 2. 目标与非目标

**目标**：把服 A 并入服 B，A 的角色此后由 B 的 game 进程承载；**角色身份（PlayerID、`Role.ServerID`、slot）不变**；框架改动只在 account（数据模型、解析、运维入口）和读会话 sid 的两处消费者，game 进程与 Nest / DataEngine / Sync 三大块不改；合服在停服窗口内离线完成，B 的配置与代码不变；可回退；双角色账号合服后两个角色都能登录。

**非目标**：不把两个角色合成一个身份（物理合服，§9）；需要业务语义的合并（排行、World 单例、活动组等）框架只给清单（§6.2）；客户端接入地址的发布（服务器列表、LB）属于部署（§5.4）。

## 3. 数据模型与解析

### 3.1 服务器记录

`GameServer`（`types.go:242`）新增 `MergedInto int32`（非 0 表示本服已并入该服）与合服时间 / 备注。不新增 `ServerStatus` 取值：合服时把 A 的 `Status` 置 `closed`，`acceptsNewRoles`（调用点 `service.go:238`）已让它拒绝建角。

### 3.2 承载服解析（唯一解析点）

account 内新增 `servingServer(origin)`，所有返回角色 / 会话的出口都经它。推荐**拍平**（§10-2）：`merge-server -from B -into C` 时把所有 `MergedInto == B` 的记录一并改为 C，记录永远只有一跳，解析只看一跳；写入时校验“目标服自己没有 `MergedInto`”。不拍平则沿链解析、做环检测与深度上限（8）。承载服的 `Status`（如 maintenance）是否放行登录与现状一致，由承载服自己的状态决定。

### 3.3 返回结构与 Claims

- `Role`、`Session` 增加只读的 `ServingServerID`（存储里不写）；`ValidateSession`、`SelectRole`、`CreateRole`、`UpdateProfile`（`service.go:331`）等返回 Role 的出口统一填入。没有别名时等于 `ServerID`。**消费者一律用 `ServingServerID` 决定路由与驻留**，`ServerID` 只表示原服（显示、日志、按服业务数据归属）。不采用“在返回值里把 `ServerID` 改写成承载服”：同一字段在存储与返回里含义不同，调用方拿去做 slot / 名字操作会用错服。
- `ServerIDClaim` 的含义改为承载服；新增 `OriginServerIDClaim = "origin_server_id"`（认证器写 `Role.ServerID`，供显示原服标识与按服数据归属）。

## 4. 消费者改动

| 位置 | 现状 | 改为 |
| --- | --- | --- |
| `wiring/gate/auth.go:33-37` | 校验与赋值用 `role.ServerID` | 用 `role.ServingServerID`；另写 `OriginServerIDClaim` |
| `infra/network/gateway/gate.go:179-183` | 按 `principal.ServerID` 选 game | 不改（principal 里已是承载服） |
| `demo/internal/access/player/tcp/auth.go.tmpl:87` | `newPrincipal(role.PlayerID, role.ServerID)` | 传 `ServingServerID`，另写 `OriginServerIDClaim` |
| `demo/game/controllers/player/enter_game.go.tmpl:177` | `BoundServerID` 读 claim 与本进程 sid 比较 | 不改逻辑，改注释语义 |
| `service/account/types.go:288-295` | `ServerIDClaim` 注释写 Role.ServerID | 改为承载服 |
| 生成的 `auth.go` 骨架（`codegen/internal/roost/render_player_tcp.go`） | 默认拒绝 | 注释写明接 account 时用 `ServingServerID` 填 `ServerIDClaim` |

game 进程、`PlayerOwners`、Nest / DataEngine / Sync、global `Routing`（按**进程** sid 绑定）都不改。

## 5. 运维流程

### 5.1 account 运维入口

`accountctl merge-server -from A -into B [-dry-run]`、`unmerge-server -from A`、`show-server -sid A`，走 account `Service` 新增的本地方法 `MergeServer` / `UnmergeServer`（与 `UpsertServer` 同级，不上总线）。`MergeServer` 校验 `from != into`、两服记录存在（否则 `ErrServerInvalid`）、`into` 自身没有别名（拍平时由同一次操作改写指向 `from` 的记录）；写 `from.MergedInto = into`、`from.Status = closed`，条件写（版本比较），并发冲突返回明确错误，重复执行同一对幂等成功。`UnmergeServer` 清空 `MergedInto`，`Status` 保持 `closed`，由运维在 A 重新部署后 `upsert-server -status open`。

### 5.2 合服步骤（离线；runbook 写进 `docs/framework/PLAYER-MIGRATION.md` 新增一节）

1. 公告停服；A、B 接入层停止接受新连接，game(A)、game(B) 优雅停机。
2. A、B 各跑 `sidmove wal-drained`（都 Drained）、`sidmove singleton`（锁都已释放），并确认 §6.3 的 outbox / 总线 / saga 前置条件。
3. `sidmove players -ids <抽样> -out before.json`。
4. 按 sid 独占数据的离线迁移（§6.1）：`sidmerge migrate -from A -into B -dry-run`，无冲突后正式执行并生成迁移清单。
5. 业务语义合并（§6.2，业务脚本）。
6. `accountctl merge-server -from A -into B`。
7. 部署层把 A 的客户端入口指向 B（§5.4）。
8. 启动 game(B)；开放登录前 `sidmove players -compare before.json`，按清单核对迁入文档。
9. 开放登录。之后不再部署 game(A)。

### 5.3 防止 A 被误启动

合服后 A 的会话已经指向 B，误启动的 game(A) 收不到玩家，但会照常跑 World(A) 等按服逻辑、认领共享 outbox。建议（§10-3）：game 进程装配了 account 能力时启动时查本服记录，`MergedInto != 0` 拒绝启动并点名承载服；没装配时只靠 runbook。

### 5.4 对部署的要求（框架不实现）

服务器列表显示“A（已并入 B）”并给出 B 的接入地址；独立 Gate 无需额外配置；嵌入 TCP 部署把 A 的接入域名 / 端口指到 B。

### 5.5 回退

1. 停服窗口内停 game(B)，`sidmove wal-drained` 确认排空；
2. `sidmerge revert -manifest <清单>`：把合服时迁入 B 的文档按当前版本搬回 A 的物理库；
3. 合服期间**新产生**的按 sid 独占数据都写在 B 的库里，框架无法判断归属（进程只知道自己是 B）；业务按自己的键规则（例如文档里记了原服）判断，否则只能留在 B，runbook 写明由业务确认；
4. `accountctl unmerge-server -from A`；用新 WAL 目录部署 game(A)，`upsert-server -sid A -status open`，接入层改回；启动 B。

没有按 sid 独占数据的工程只有第 1、4 步。

## 6. 按服数据

### 6.1 按 sid 独占的数据：离线迁移（框架工具）

- **工具**：`cmd/sidmerge migrate -from A -into B [-dry-run] [-manifest out.json]`（同 sidmove 生成进 game-demo）。逻辑库清单取 DAO 常量文件 `db/gen_databases.go` 里登记为按 sid 分库的逻辑库（`dataengine.DeclareDatabases`）。
- **物理库换算**：A 的源库用 **A 的服务配置** 与 sid A 换算，B 的目标库用 **B 的服务配置** 与 sid B 换算（`LoadDatabaseMapFile(path, sid, resolver)` 一次只换算一个 sid，所以工具要两份配置）；工程用了 `WithDatabaseResolver` 回调时，工具与 game 进程传同一个回调（工具在工程内编译）。
- **前置**：A、B 的 `wal-drained` 与 `singleton` 都通过，§6.3 的检查都通过，否则拒绝执行。
- **逐集合拷贝**：原样拷贝文档，含 `_version` / `_schema` / `_last_tx` 与墓碑字段；B 冷加载从这些元数据继续。不拷贝 DataEngine 自己的控制集合（receipts / transactions / outbox，在 `dataengine.database` 指定的控制库里，见 §6.3 第 2 条）。
- **`_id` 冲突**：PlayerID 由全局唯一的分配器发出，玩家相关文档不撞；在各服取同一常量 ID 或用按服序列发 ID 的文档会撞。冲突时整体停止并逐条报告，不覆盖、不跳过，由业务处置后重跑。
- **幂等与清单**：重跑时与目标摘要相同的文档跳过；每条迁入记录（库、集合、`_id`、版本、摘要）写进清单，供开服前核对与回退。
- **源库**：保留为只读归档还是删除，见 §10-4。
- **Remote 托管（`remote=managed`）的 kind**：所有权元数据不在本工具范围，dry-run 点名，按 Remote 规则另行处理。

### 6.2 需要业务语义的合并（框架给清单）

| 类别 | 现状 | 合服时的处置（业务决定） |
| --- | --- | --- |
| World 单例 | UniqueID = sid；World DAO 在共享库，合服后只加载 World(B) | 合并计数 / 进度到 World(B) 或丢弃；World(A) 文档留在原处 |
| 排行（按服） | board 名常含 sid | 并存（A 榜只读）或重算合并榜 |
| 活动组 `activity.groups_file` | 按 game sid 列组 | 从组里移除 A，必要时并入 A 的贡献 |
| 跨服 global `Routing` | 按 game 进程 sid 绑定组 | 解除 A 的绑定 |
| 服内唯一名字 | directory 是全局域则无冲突 | 按服唯一的名字需要业务改名规则 |
| 邮件、聊天、充值待发 | 按 PlayerID 或按服 | 按 PlayerID 的不受影响；按服频道由业务处理 |

### 6.3 原提案没有考虑到的约束（审查补充）

1. **D9 库名映射与“玩家文档不用搬”**：D9 修订版 A 之后，代码里只有逻辑库名，物理库来自每个服务配置的 `dataengine.databases`（加上 `mongo` 连接）。A、B 是两份部署配置，可以把同一个非按 sid 的逻辑库映射到不同物理库或不同 Mongo。只有两边映射到同一 Mongo 上的同一物理库时，A 的 Player 等文档才已经在 B 能读到的地方。否则这些共享库里还可能混着其他服的文档、没有按服的归属字段，工具无法只挑出 A 的那部分。**建议**：`sidmerge migrate` 的前置检查比较 A、B 对每个非按 sid 逻辑库的物理位置（Mongo 地址 + 换算后的库名），不同则拒绝合服并点名逻辑库，不做通用搬迁。
2. **D9 不能代替数据迁移**：映射以逻辑库为单位、对本进程 sid 加 `_<sid>`，两个逻辑库不得映射到同一物理库（`framework/dataengine/databases.go:236-239` 的一次校验），所以“只改 B 的映射让它读到 A 的库”做不到，按 sid 独占的数据必须搬。合服不改 B 的映射，B 的 WAL 目录 `databases.json` 不变，不触发 `ErrDatabaseMappingChanged`；A 的 WAL 目录随 A 退役。
3. **outbox 没有被 `wal-drained` 覆盖**：DataEngine 停机先 Flush 投影，再停 outbox 认领，已暂存的 effect 留在 Mongo 等下次启动（`runtime.go:165`）。outbox 认领不按 sid 过滤（`framework/dataengine/engine/outbox_store.go:62-72`），所以 A、B 共用同一 Mongo 上的同一 `dataengine.database` 时，B 启动后会发出 A 剩下的 effect；不共用时 A 的剩余 effect 永远没人发。**建议**：前置条件写成“A 的 outbox 为空，或 A、B 共用同一控制库”，由 `sidmerge`（或给 `sidmove` 加 `outbox-drained`）检查。迁服流程同 sid 接管、同一控制库，不受影响。
4. **持久化里记着的 sid**：Saga 状态里存着发起方的绑定 sid 并按它路由（game-demo 赠礼 `gift.State.FromSID` → `svc.<game>.<sid>`，`demo/internal/service/game/gift_saga.go.tmpl:140-143`、`:194-196`）。合服后 A 没有进程，指向 A 的非终态 saga 无法推进，所以“合服前全部 saga 终态”是**必需**的前置条件（`saga.Engine.List` 按状态查，dry-run 报数量），不是建议。业务自己持久化的 sid 字段同样进 §6.2 的清单。
5. **总线上发往 A 的消息**：跨进程的服务消息按 sid 寻址（如赠礼转交发往 `<prefix>.svc.<game>.<sid>`，同上 `:143`）。A 退役后，发往 A 的消息若落在持久流 / 消费者里就无人消费；哪些主题是持久的取决于工程装配，审查未逐个核对（尚未确认）。runbook 要求停服前确认发往 A 的持久消费者没有积压（或由业务决定丢弃 / 重投到 B），实施时把这类主题列进 §6.2 的清单。

## 7. 错误语义

- `ErrServerMergeCycle`（新增 errcode）：不拍平时写别名形成环或超深度；解析遇到（数据被手工改坏）同样返回，登录失败并记 Error。拍平时只用于写入校验。
- `merge-server` 的 from / into 不存在：沿用 `ErrServerInvalid`。
- `CreateRole` 在已并入的服：现有 `ErrServerClosed`，消息补“已并入 <B>”。
- 并发冲突：条件写失败返回明确错误，提示重读后重试；重复执行同一对幂等成功。

## 8. 测试与验收

- **单元（`service/account`）**：解析（无别名、单跳、拍平后的多次合服、承载服记录缺失）；各出口的 `ServingServerID`；已并入的服 `CreateRole` 拒绝；`MergeServer` 幂等 / 冲突 / 目标已有别名；`UnmergeServer` 后解析回原服；存储里不写 `ServingServerID`。
- **Gate**：A 并入 B 后，A 的角色经 `AccountAuthenticator` 得到 `Principal.ServerID == B`，`ResolveGame` 选到 B，`OriginServerIDClaim == A`。
- **sidmerge**：两份配置分别换算源 / 目标物理库；非按 sid 逻辑库物理位置不同则拒绝；outbox 非空且不共用控制库则拒绝；`_id` 冲突整体停止；幂等重跑；清单与回退（真实 Mongo）。
- **真实进程（game-demo 两服）**：sid 1000、1001 各建角色（其中一个账号两服都有）并写数据；验收工程加一个 `dbscope=sid` 的 DAO（game-demo 本身没有）并放一个在两服取同一常量 ID 的文档，dry-run 报出冲突；处置后迁移、`merge-server`、启动 1000；1001 的角色被 1000 承载、数据一致、能继续写；双角色账号两个角色先后登录互不影响；1001 上 `CreateRole` 被拒；回退后 1001 的角色回到 1001；独立 Gate 部署同样走一遍承载服路由。

## 9. 未采用的方案

- **一个 game 进程同时服务一组 sid**（第八轮最初的推荐）：要改接入层与 `PlayerOwners`、每个 sid 一把单实例锁、按 sid 分库的库名解析、多个 World 共存，改动落在 Nest / DataEngine / 接入三处。别名合服把“承载服”放到 account 一处解析，进程仍只认一个 sid。
- **物理合服（改绑 `Role.ServerID`）**：要先由产品决定双角色怎么处理（改 slot 模型或合并 / 删除角色，不可逆），还要处理按服唯一名字与排行重算，不可回退。留给确有“合并成一个身份”需求的项目。
- **在 game 进程配置里做别名**：Gate 路由、会话 claim、`PlayerOwners` 都要知道别名，改动分散到每个进程配置，且容易与 account 的服务器列表不一致。

## 10. 待维护者决定（含推荐）

1. **字段命名与语义**：`ServingServerID`、`ServerIDClaim` 改为承载服、新增 `OriginServerIDClaim`。推荐按本篇。
2. **多次合服**：拍平（推荐，离线窗口里多改几条记录，解析只看一跳）；或保留链、解析沿链走、限深 8。
3. **合服后误启动 A**：装配了 account 的 game 启动时检查并拒绝（推荐）；或只靠 runbook。
4. **按 sid 独占数据迁移后的源库**：保留为只读归档、确认无误后运维手动删除（推荐）；或迁移成功即删除。
5. **工具划分**：account 记录用 `accountctl merge-server`；离线数据迁移、前置检查（§6.3）与按服清单用独立的 `cmd/sidmerge`（推荐）；或把 account 改写也并进 `sidmerge`（一条命令完成，但要拿 account 存储凭据）。
6. **原服标识显示**：框架只提供 `OriginServerIDClaim`，名字后缀由业务做（推荐）；或 account 提供显示名模板。
7. **（审查补充）非按 sid 逻辑库物理位置不同的部署**：拒绝合服并点名（推荐，§6.3 第 1 条）；或另行设计带归属字段的通用搬迁。
8. **（审查补充）outbox 前置检查放在哪里**：`sidmerge` 前置检查（推荐，合服专用）；或给 `sidmove` 加 `outbox-drained`（迁服也能用，但同 sid 接管本来不需要）。

## 11. 分期

| 期 | 内容 | 验收 |
| --- | --- | --- |
| P1 | account：`MergedInto`、解析、`ServingServerID`、`MergeServer` / `UnmergeServer`、errcode、`ServerIDClaim` 注释 | §8 单元 |
| P2 | 消费者：Gate 认证器、game-demo 认证器与登录端点注释、生成骨架注释、`OriginServerIDClaim` | §8 Gate |
| P3 | 运维：accountctl 三个子命令（含拍平）、`cmd/sidmerge migrate / revert`（含 §6.3 前置检查）、按 §10-3 的启动自检、PLAYER-MIGRATION 合服 runbook | runbook 走查；sidmerge 单元与真实 Mongo 用例 |
| P4 | 工程侧清单：doctor 只读 `merge-readiness` 复用 sidmerge 的清单；真实进程两服验收（含 `dbscope=sid` 迁移与回退） | §8 真实进程 |
