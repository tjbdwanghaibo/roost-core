# 方案：大厅服（lobby）与 relay 服，多个 game 共用；account 同时支持多个服务（2026-11-16）

**状态**：方案，未实施，第 6 节的问题等维护者决定。与同日的整理轮（`feat/round-20261116`）互不依赖；实施时以整理轮合入后的 main 为基础。

## 1. 目标

1. 一个工程里可以有**多个 game 服务**（例如 `slg`、`moba`），共用同一套 account、Gate、大厅和 relay。
2. **大厅服**：玩家登录后先进大厅（好友、聊天、组队、匹配入口、选游戏），再进入某个 game，或进入 relay 房间。
3. **relay 服**：给需要实时对局的玩法提供房间转发和帧同步，多个 game 共用一个 relay 集群。
4. **account 可配置同时支持多个服务**：区服、角色、票据都能区分属于哪个服务。

## 2. 现状与限制（main，2026-11-16 调查）

| 方面 | 现状 | 位置 |
|---|---|---|
| 票据 | `at1.<exp>.<账号>`、`et1.<exp>.<sid>.<账号>`、角色会话令牌。都只带一个 sid，不带服务名 | `service/account/tickets.go:13-24` |
| 区服、角色 | Role、Slot、Presence 只认 sid。区服号全局扁平，最多 256 个 | `service/account/types.go:267,303-322`、`servers.go:30` |
| account 配置 | 没有服务列表；客户端只能配一个 `account.service_type` | `wiring/account/account_mod.go:80-90` |
| 一个工程几个玩家服务 | 只能有一个：`access` 只允许 `player` 键，`AccessSpec` 只有一个 Service 字段，`accountService()` 只取第一个 | `codegen/internal/roost/manifest.go:212-233,393-395`、`gate.go:84-91` |
| Gate | 一条连接只绑定一个 game；所有消息号都转给它；角色名 "game" 写死在代码里 | `infra/network/gateway/packet.go:35`、`gate_forward.go:58`、`wiring/gate/mod.go:161` |
| 大厅 | 没有实现 | — |
| relay 可借用的部分 | `framework/sync/lockstep` 是进程内的帧同步；physicsroom 是独立服务加 Relay 的写法；room、matchroom、turn 模块是本服内的 Nest 实体；`service/match` 是按 affinity 路由的匹配器 | `lockstep/match.go:241`、`wiring/physicsroom/relay.go` |

## 3. 总体结构

```
客户端 ──HTTP──▶ account/portal（登录、选游戏、选服、发票据）
   │
   └─TCP──▶ Gate ──▶ lobby（大厅：好友/聊天/组队/匹配入口）
              │  └─切换绑定─▶ game:slg / game:moba（按 sid 所属服务路由）
   └─TCP/UDP──▶ relay（房间：帧同步/状态转发，凭房间票据直连）
```

不变的原则（沿用 2026-11-12 定的登录架构）：
- 所有票据都由 account 签发和校验；
- Gate 不调用 game 的 RPC；
- 角色名在 game 里。

## 4. 设计

### 4.1 account 支持多个服务

- **区服归属**：区服表每一行加 `service`（这个区服属于哪个 game 服务）。sid 仍然全局唯一，在所有 game 之间共用一个号段，推荐这样做，见问题 1。上限从 256 提高，具体值实施时定。
- **配置**：account 新增 `account.services: [lobby, slg, moba, relay]`，列出允许签发票据的服务。签发和校验时，核对 sid 所属服务在这个列表里。
- **票据**：进服票据改成 `et2.<exp>.<service>.<sid>.<账号>`。Gate 直接按票据里的服务名路由，不用再查一次表。新增房间票据 `rt1.<exp>.<relay sid>.<房间号>.<账号>`。线上没部署，`et1` 直接替换，不做兼容。
- **角色**：本来就按 sid 存，多个 game 时不用改；同一账号在不同 game 有各自的角色。portal 的 `/servers` 加 `service=` 参数，按游戏列出区服。
- **大厅身份**：大厅不建角色，用账号身份（账号票据，或一种"大厅票据"）进大厅。见问题 6。

### 4.2 Gate：大厅和 game 之间切换绑定

- **推荐**：同一条连接上切换绑定。客户端在大厅里拿到某个 game 的进服票据后，发送"切换"消息。Gate 用这张票据重新做一次握手校验，然后解除和大厅的绑定、改绑到这个 game，TCP 连接不断。从 game 回大厅也走同一条路。
- **另一种做法**：Gate 同时绑定多个后端，按消息号段分发（大厅消息给大厅，游戏消息给 game）。改动大很多：要改 Binding、Forward、Ingress 和客户端 SDK。见问题 2。
- **角色名去写死**：Gate 内部按服务名路由，"game" 不再写死在代码里。

### 4.3 大厅服

- **性质**：生成工程里的一个业务服务，新增 `roost add lobby` 命令，做法和 `roost add gate` 一样。大厅有玩家实体（`LobbyPlayer`，按账号 id，持久化内容很少）。
- **装进大厅的模块**：friends、chat、team（组队）、matchmaking 入口、notice。到底装哪些，见问题 6。
- **进入 game**：大厅向 account 申请目标 game 区服的进服票据，交给客户端，客户端用 §4.2 的切换消息进入 game。
- **进入 relay**：匹配成功后，由大厅（或 game）调用 relay 建房，拿到房间号；再向 account 申请房间票据，交给客户端。

### 4.4 relay 服

- **性质**：框架服务。新增 `service/relay`（接口 + `//roost:rpc service_type=relay affinity=room`）和 `wiring/relay`，加进 `frameworkCatalog`。game 或大厅写 `uses: [relay]` 来调用它。
- **房间**：relay 内部的运行期实体，不持久化，复用 `framework/sync/lockstep` 的帧同步，另外支持"状态转发"模式。按房间号 affinity 分片，由 etcd 发现决定房间在哪个实例上。relay 重启后房间丢失，对局结果由 game 侧兜底。
- **多个 game 共用**：建房请求里带 `game`（服务名）和 `mode`（模式配置名），relay 按 `game/mode` 选择帧率、人数、超时等参数。房间结束后，relay 回调发起建房的服务，上报结果（调用 game 的 RPC，或走 `nest.Emit` 投递）。
- **客户端怎么连**：推荐凭房间票据**直连 relay**。relay 调用 account 校验票据，做法和 Gate 一样。另一种做法是经 Gate 多一条房间绑定。见问题 3。
- **协议**：先做 TCP，UDP/KCP 以后再加。见问题 5。

### 4.5 生成器与 `roost.yaml`

- **多个 game 服务**：`access.player` 改成可以列出多个玩家服务，例如 `access.player.services: [lobby, slg, moba]`。去掉"只有一个 Service"的限制，`accountService()` 等只取第一个的地方都要改。
- **新命令**：`roost add lobby`；relay 用 `uses: [relay]` 加框架服务托管（`framework: relay`）。
- **doctor 新增检查**：
  - `account.services` 覆盖了所有玩家服务和 relay；
  - 不同 game 的区服号不重叠；
  - Gate 能路由到每一个玩家服务。
- **框架服务数量**：catalog 里"九个"的说法和对应测试要改（`help.go:278`、`framework_services_test.go:404`）。

## 5. 验证计划

- **单测**：
  - account：多个服务的签发与校验；sid 不属于列表里的服务时拒绝；`et2` / `rt1` 的格式和过期。
  - Gate：切换绑定的握手、拒绝和回滚。
  - relay：房间分片、票据校验、帧同步、超时。
- **生成工程**：lobby + 两个 game + relay + Gate；编译、`generate --check`、doctor 全部 OK。
- **真实进程演练**：
  - 登录 → 进大厅 → 切到 slg → 切回大厅 → 匹配 → 拿到 relay 房间票据 → 直连 relay 对局 → 结果回到 game；
  - relay 实例被杀时，房间丢失，game 侧兜底生效；
  - 大厅或 game 重启，切换绑定失败时，客户端能拿到明确的错误。

## 6. 需要维护者决定的问题

1. **区服号**：所有 game 共用一个全局号段（推荐，改动最小，票据和角色表不用加字段），还是每个服务各自从 1 编号（这样所有按 sid 存的表都要改成按"服务 + sid"存）？
2. **大厅和 game 之间怎么切**：同一条连接上切换绑定（推荐），还是一条连接同时连大厅和 game、按消息号分发？
3. **客户端怎么连 relay**：凭 account 签发的房间票据直连（推荐，延迟最低，也是对战服常见做法），还是经 Gate 多一条绑定（统一入口，但多一跳）？
4. **relay 的定位**：做成框架服务（推荐，多个工程通用、不写业务），还是每个工程生成一个业务服务、自己写房间逻辑？
5. **relay 协议**：先只做 TCP（推荐），还是这次就做 UDP/KCP？
6. **大厅的范围**：哪些模块放在大厅、哪些留在 game？大厅要不要有自己的"大厅角色"，还是只用账号身份？推荐只用账号身份；好友、聊天、组队、匹配入口放大厅，其余留在 game。

## 7. 工作量（agent 墙钟时间，问题都按推荐选项）

| 项 | 估计 |
|---|---|
| account 支持多个服务（区服归属、`et2`/`rt1`、配置、portal） | 3～4 小时 |
| Gate 切换绑定、去掉写死的角色名 | 2～3 小时 |
| 生成器：多个玩家服务、`roost add lobby`、doctor | 3～4 小时 |
| relay 框架服务（房间、帧同步、分片、票据、结果回调）和客户端 SDK | 5～7 小时 |
| 演练和文档 | 2～3 小时 |

关键路径上约 **12～16 小时**。最后的全量测试按惯例只在最后跑一次。
