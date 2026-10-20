# 功能设计：别名合服（2026-10-20）

**状态：提案，待维护者评审，未实施。** 来源：新用户试用第八轮（游戏服务器专家视角）登记的“合服没有框架支持”，维护者问“合服可以用迁服实现吗”“方案 A 是不是有些麻烦”之后，按“最小框架改动、可回退、不触发双角色冲突”重新给出的方案。本篇写给评审者与实施 agent；源码位置按本分支基线（origin/main，2026-10-20）核对，实施前请对照当时的 HEAD 再核一次行号。

## 1. 现状与问题

### 1.1 玩家与服的绑定只有两处

- **account 的角色记录**：`Role.ServerID`（[types.go:252](../../service/account/types.go)）在建角时写定，之后不变；会话 `Session.ServerID`（[types.go:280](../../service/account/types.go)）由 `SelectRole` 发出（[service.go:267](../../service/account/service.go)），`ValidateSession` 返回角色记录（[service.go:307](../../service/account/service.go)）。
- **game / Gate 按会话里的 sid 做静态判断**：
  - 独立 Gate：`AccountAuthenticator` 用 `role.ServerID` 填 `Principal.ServerID`（[wiring/gate/auth.go:29-37](../../wiring/gate/auth.go)），`gate.go` 按它 `ResolveGame(ctx, principal.ServerID)` 选 game 进程并要求两边 sid 一致（[infra/network/gateway/gate.go:179-183](../../infra/network/gateway/gate.go)）。
  - 嵌入 TCP（game-demo）：认证器把 `role.ServerID` 写进 `Claims[svcaccount.ServerIDClaim]`（[auth.go.tmpl:87](../../demo/internal/access/player/tcp/auth.go.tmpl)），登录端点 `BoundServerID` 读它与本进程 sid 比较，不等即 `player_elsewhere`（[enter_game.go.tmpl:177-181](../../demo/game/controllers/player/enter_game.go.tmpl)）。

玩家**数据**本身不按服：`roost add dao` 生成的 DAO 默认不带 `dbscope=sid`，game-demo 里没有任何 `dbscope=sid` 的 DAO（`cmd/sidmove` 注释：“Player 不按 sid 分库，sid 只参与校验”）。所有服的玩家文档在同一个物理集合里。

### 1.2 合服的硬阻碍

1. **双角色 slot 冲突**：一个账号一个服一个角色，由 insert-only 的 slot 保证，键是 `slotKeyFor(accountID, serverID)`（[service.go:257](../../service/account/service.go)）。同一账号在 A、B 两服各有角色时，把 A 服角色的 `ServerID` 改成 B 必然冲突。
2. **没有改绑接口**：`cmd/accountctl` 只有 `upsert-server`；account 没有修改 `Role.ServerID` 的接口。
3. **一个进程只认一个 sid**：会话 sid 静态比对、按 sid 分库的物理库名带 `_<sid>`（D9）、单实例锁按 `server_type + sid`、World 单例的 UniqueID 就是 sid（[framework_services.go:534](../../codegen/internal/roost/framework_services.go) `WorldUniqueID`）。

### 1.3 已有可复用的部分

`cmd/sidmove`（迁服检查工具）的 `wal-drained`、`singleton`、`players -out / -compare` 三项检查正好是合服的前置门槛：被合服停机后 WAL 全部投影确认、单实例锁已释放、空窗期没有第三个写者。

## 2. 目标与非目标

**目标**

- 把服 A 并入服 B：A 的角色此后由 B 的 game 进程承载，**角色身份（PlayerID、`Role.ServerID`、slot）不变**。
- 框架改动只在 account（数据模型、解析、运维入口）和读会话 sid 的两处消费者；game 进程与 Nest / DataEngine / Sync 三大块不改。
- B 进程**不需要重启**即可开始承载 A 的玩家。
- 可回退：撤销别名后 A 可以重新单独部署。
- 双角色账号在合服后两个角色都能正常登录。

**非目标**

- 不把两个角色合并成一个身份（那是“物理合服”，见 §9 未采用方案 B）。
- 不自动合并按服的业务数据（排行、World 单例、活动组等）——框架给清单与检查，处置由业务决定（§6）。
- 不处理客户端接入地址的发布（服务器列表、网关 / LB 路由属于部署，§5.4 只给要求）。
- 不支持把 A 并入一个本身已并入别处的服（链式合服）以外的拓扑变化；链式解析见 §3.2。

## 3. 数据模型与解析规则

### 3.1 服务器记录加一个字段

`GameServer`（[types.go:242](../../service/account/types.go)）新增：

```go
// MergedInto 非 0 表示本服已并入该服：本服角色由它的 game 进程承载，本服不再接受建角。
MergedInto int32 `json:"merged_into,omitempty"`
```

不新增 `ServerStatus` 取值：`acceptsNewRoles`（[service.go:238](../../service/account/service.go)）已经让非 open 的服拒绝建角，合服时把 A 的 `Status` 置为 `closed` 即可；`MergedInto` 只表达“谁承载”。

### 3.2 承载服解析（唯一解析点）

account 内新增一个函数，所有返回角色 / 会话的路径都经它：

```
servingServer(origin) =
    s := origin; seen := {}
    loop: rec := servers[s]
          if rec.MergedInto == 0: return s
          if s in seen or len(seen) >= maxMergeDepth(=8): return error ErrServerMergeCycle
          seen.add(s); s = rec.MergedInto
```

- 链式允许（A→B，之后 B→C，A 的角色由 C 承载），深度上限与环检测在**写入别名时**就拒绝，解析时再防御一次。
- 承载服记录不存在、或承载服 `Status` 为 `maintenance`：解析成功（返回承载 sid），是否放行登录与现状一致——由承载服自己的状态决定，不在解析里另设规则。

### 3.3 返回结构加字段（只读、不存储）

- `Role` 增加 ``ServingServerID int32 `json:"serving_server_id,omitempty"` ``：存储里不写（或写入时置 0），`ValidateSession`、`SelectRole`、`CreateRole`、`UpdateProfile` 等**返回 Role 的出口**统一按 §3.2 填入。
- `Session` 增加同名字段，`SelectRole` 填入。
- 没有别名时 `ServingServerID == ServerID`；**消费者一律用 `ServingServerID` 决定路由与驻留**，`ServerID` 只表示“原服”（显示、日志、按服业务数据归属）。

推荐这样（新增字段）而不是“在返回值里把 `ServerID` 改写成承载服”：后者让同一个字段在存储与返回里含义不同，调用方拿返回值去做 slot / 名字等操作时会用错服。

### 3.4 会话 Claims

- `ServerIDClaim`（[types.go:295](../../service/account/types.go)）的含义改为**承载服**（game 进程据此判断“该不该在本服”，现有比较逻辑不变）。
- 新增 `OriginServerIDClaim = "origin_server_id"`，认证器写入 `Role.ServerID`，供业务显示（如角色名后缀 [S1]）与按服数据归属使用。

## 4. 消费者改动

| 位置 | 现状 | 改为 |
| --- | --- | --- |
| [wiring/gate/auth.go:33-37](../../wiring/gate/auth.go) | `role.ServerID <= 0` 拒绝；`Principal.ServerID: role.ServerID` | 校验与赋值都用 `role.ServingServerID`；`Claims[OriginServerIDClaim]` 写 `role.ServerID` |
| [infra/network/gateway/gate.go:179-183](../../infra/network/gateway/gate.go) | 按 `principal.ServerID` 解析 game | 不改（principal 里已是承载服） |
| game-demo 认证器 [auth.go.tmpl:87](../../demo/internal/access/player/tcp/auth.go.tmpl) | `newPrincipal(role.PlayerID, role.ServerID)` | 传 `role.ServingServerID`，另写 `OriginServerIDClaim` |
| game-demo 登录端点 [enter_game.go.tmpl:177](../../demo/game/controllers/player/enter_game.go.tmpl) | `BoundServerID` 读 `ServerIDClaim` 与本进程 sid 比较 | 不改（claim 已是承载服）；注释改写语义 |
| 生成的 `auth.go` 骨架（`codegen/internal/roost/render_player_tcp.go`） | 默认拒绝 | 注释写明：接 account 时用 `ServingServerID` 填 `ServerIDClaim` |
| 其他读 `Role.ServerID` 的地方 | — | 实施时 `rg '\.ServerID\b'` 全仓逐个判定是“路由 / 驻留”（改用 Serving）还是“原服归属”（保留） |

game 进程、`PlayerOwners`、Nest / DataEngine / Sync、global `Routing`（按**进程** sid 绑定跨服组，承载服进程用自己的 sid）都不改。

## 5. 运维流程

### 5.1 新增 accountctl 子命令

```
go run ./cmd/accountctl -redis … merge-server   -from 1001 -into 1000 [-dry-run]
go run ./cmd/accountctl -redis … unmerge-server -from 1001
go run ./cmd/accountctl -redis … show-server    -sid 1001
```

- 走 account `Service` 新增的本地方法（与 `UpsertServer` 同级，不上总线）：`MergeServer(ctx, from, into)`、`UnmergeServer(ctx, from)`。
- `MergeServer` 校验：`from != into`；两服记录都存在；`into` 自身的解析链无环且深度 < 上限；`from` 当前没有别名（重复执行同一对是幂等成功）。写入：`from.MergedInto = into`、`from.Status = closed`，条件写（版本比较），并发冲突返回明确错误。
- `UnmergeServer`：清空 `MergedInto`，`Status` 保持 `closed`，由运维在 A 重新部署后用 `upsert-server -status open` 打开。
- 每次写入记审计日志（操作者、from、into、时间），沿用 account 现有的 admin 审计方式。

### 5.2 合服步骤（runbook，写进 `docs/framework/PLAYER-MIGRATION.md` 新增一节）

1. 公告停服；A 的接入层停止接受新连接，game(A) 优雅停机。
2. `sidmove wal-drained -dir <A 的 WAL 目录>` → 必须 Drained。
3. `sidmove singleton -key …:game:<A>` → 锁已释放。
4. `sidmove players -ids <A 的抽样玩家> -out before.json`。
5. 按 §6 清单处置 A 的按服数据（业务脚本）；`accountctl merge-server -from A -into B -dry-run` 打印检查结果。
6. `accountctl merge-server -from A -into B`。
7. 部署层把 A 的客户端入口指向 B（服务器列表把 A 显示为“已并入 B”，网关 / LB 路由改到 B，§5.4）。
8. 玩家登录 A 服角色：会话承载服为 B，由 game(B) 加载；`sidmove players -compare before.json` 在首批登录前比对，证明空窗期无写入。
9. 不再部署 game(A)。

B 全程不停机、不改配置。

### 5.3 防止 A 被误启动

合服后若有人再启动 game(A)，A 的玩家会话已经指向 B，A 进程收不到会话；但 A 进程本身会照常跑起 World(A) 等按服逻辑。建议（待决定 §10-3）：game 进程启动时若装配了 account 能力，查询本服记录，`MergedInto != 0` 则拒绝启动并点名“已并入 <B>”。

### 5.4 对部署的要求（框架不实现）

- 服务器列表接口要能显示“A（已并入 B）”并给出 B 的接入地址；
- 独立 Gate 部署无需额外配置（Gate 按会话承载服路由，§4）；
- 嵌入 TCP 部署要把 A 的接入域名 / 端口指到 B。

### 5.5 回退

在 B 承载期间 A 的玩家产生的数据都写在全服玩家集合里，撤销别名不需要搬数据：

1. game(B) 照常运行；A 的玩家下线（公告）；
2. `accountctl unmerge-server -from A`；
3. 部署 game(A)（新 WAL 目录），`upsert-server -sid A -status open`，接入层改回。

若在合服期间业务把 A 的按服数据并进了 B（§6），回退需要业务反向处置，runbook 写明这一点。

## 6. 按服数据清单与检查

`merge-server -dry-run` 打印、doctor 新增只读检查 `merge-readiness`（需要工程上下文）列出：

| 类别 | 现状 | 合服时的处置（业务决定） |
| --- | --- | --- |
| `dbscope=sid` 的 DAO | 物理库 `<logical>_<sid>`，B 进程读不到 A 的库 | **默认拒绝合服**并点名 DAO；业务把数据搬进 B 的库后用 `-allow-sid-dao <name>` 声明已处置 |
| World 单例 | UniqueID = sid（`WorldUniqueID`），合服后 World(A) 不再加载 | 业务决定合并计数 / 丢弃；清单点名 World(A) 文档 |
| 排行（rank board 按服） | board 名常含 sid | 并存（保留 A 榜只读）或重算合并榜 |
| 活动组 `activity.groups_file` | 按 game sid 列组 | 从组里移除 A，必要时把 A 的贡献并入 |
| 跨服 global `Routing` | 按 game sid 绑定组 | A 的绑定解除（`Routing` 已有迁移操作，按进程 sid 不再需要 A） |
| saga | 涉及 A 玩家的非终态 saga | 合服前要求全部终态（`Engine.List` 按状态查，dry-run 报数量） |
| 服内唯一名字（如公会名，若按服唯一） | directory 是全局域则无冲突 | 按服唯一的名字需要业务改名规则 |
| 邮件、聊天、充值待发 | 按 PlayerID 或按服 | 按 PlayerID 的不受影响；按服频道由业务处理 |

## 7. 错误语义

- `ErrServerMergeCycle`（新增 errcode）：写别名时形成环或超过深度；解析时遇到（数据被手工改坏）同样返回，登录失败并记 Error。
- `ErrServerNotFound`（现有或新增）：`merge-server` 的 from / into 不存在。
- `CreateRole` 在已并入的服：现有 `ErrServerClosed`，消息补一句“已并入 <B>”。
- `merge-server` 的并发冲突：条件写失败返回明确错误，提示重读后重试；重复执行同一对幂等成功。

## 8. 测试与验收

**单元（service/account）**

- 解析：无别名、单跳、两跳链、环、深度上限、承载服记录缺失；
- `ValidateSession` / `SelectRole` / `CreateRole` 返回值的 `ServingServerID`；已并入的服 `CreateRole` 拒绝；
- `MergeServer` 幂等、冲突、环拒绝；`UnmergeServer` 后解析回到原服；
- 存储里不写 `ServingServerID`（读回旧记录、写回后字段为 0）。

**Gate（wiring/gate、infra/network/gateway）**：A 并入 B 后，A 服角色的票据经 `AccountAuthenticator` 得到 `Principal.ServerID == B`，`ResolveGame` 选到 B 进程；`OriginServerIDClaim == A`。

**真实进程验收（game-demo 两服）**

1. 起 sid 1000、1001 两个 game 与 account；在两服各建若干角色，其中一个账号在两服各有一个角色；各自写入一些数据；
2. 停 1001，`sidmove wal-drained / singleton / players -out`；
3. `accountctl merge-server -from 1001 -into 1000`；
4. 1001 的角色登录：被 1000 进程承载，数据与停机前一致（`players -compare`），能继续写入；
5. 双角色账号的两个角色先后在 1000 登录，互不影响；
6. 在 1001 上 `CreateRole` 被拒；
7. 回退：`unmerge-server`、重新部署 1001、1001 的角色回到 1001 承载，期间在 1000 写入的数据仍在；
8. 独立 Gate 部署同样走一遍步骤 4（按承载服路由）。

## 9. 未采用的方案

- **方案 A：一个 game 进程同时服务一组 sid**（第八轮最初的推荐）。要改接入层与 `PlayerOwners` 的判断、单实例锁（每个 sid 一把）、按 sid 分库的库名解析、多个 World 单例共存——改动落在 Nest / DataEngine / 接入三处，成本高。别名合服把“承载服”放到 account 一处解析，进程仍只认一个 sid，达到同样效果。
- **方案 B：物理合服（改绑 `Role.ServerID`）**。必须先由产品决定双角色怎么处理（改 slot 模型允许一服多角色，或合并 / 删除角色——不可逆），还要处理按服唯一名字、排行重算；不可回退。留给确有“合并成一个身份”需求的项目，本提案不做。
- **在 game 进程里做别名（配置 `alias_sids`）**：可以不改 account，但 Gate 路由、会话 claim、`PlayerOwners` 都要知道别名，等于把 §4 的改动分散到每个进程配置里，且容易与 account 的服务器列表不一致。不采用。

## 10. 待维护者决定（含推荐）

1. **字段命名与语义**：`Role.ServingServerID` / `Session.ServingServerID`、`ServerIDClaim` 改为承载服、新增 `OriginServerIDClaim`。**推荐**按本篇。
2. **链式合服**：允许并限深 8（推荐），或只允许一跳（实现更简单，后续再并需先把下游改指向终点）。
3. **合服后误启动 A**：game 启动时经 account 检查 `MergedInto` 并拒绝启动（推荐，需要 game 装配了 account 能力；没装配时只靠 runbook），或只靠 runbook。
4. **`dbscope=sid` 的默认处置**：默认拒绝合服、需显式声明已处置（推荐），或只警告。
5. **按服数据检查放在哪**：`accountctl merge-server -dry-run` 只做 account 侧检查，工程侧清单由 doctor `merge-readiness` 做（推荐，二者都只读）；或全部放进一个独立的 `cmd/sidmerge` 工具（类似 sidmove，生成进 game-demo）。
6. **显示原服标识**：框架只提供 `OriginServerIDClaim`，名字后缀由业务做（推荐）；或 account 在 `Role` 里提供显示名模板。

## 11. 分期

| 期 | 内容 | 验收 |
| --- | --- | --- |
| P1 | account：`MergedInto`、解析、`ServingServerID`、`MergeServer` / `UnmergeServer`、errcode、单元测试 | §8 单元 |
| P2 | 消费者：Gate 认证器、game-demo 认证器与登录端点注释、生成骨架注释、`OriginServerIDClaim`；全仓 `.ServerID` 判定 | §8 Gate |
| P3 | 运维：accountctl 三个子命令、dry-run 检查、（按 §10-3）启动自检、PLAYER-MIGRATION 合服 runbook | runbook 走查 |
| P4 | 工程侧清单：doctor `merge-readiness` 或 `cmd/sidmerge`（按 §10-5）；真实进程两服验收 | §8 真实进程 |
