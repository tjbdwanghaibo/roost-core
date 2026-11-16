# 方案：大厅服（lobby）与 relay 服——房主做 host 的开房间场景；account 同时支持多个服务（2026-11-16）

**状态**：方案，未实施。第 6 节的 6 个问题维护者已答（2026-11-16），本文按答复改写；第 7 节还有 4 个细节问题等维护者确认。与同日的整理轮（`feat/round-20261116`）互不依赖，实施时以整理轮合入后的 main 为基础。

## 1. 场景与目标

**场景**：简单的开房间联机游戏。服务器不做玩法校验，**房主的客户端就是 host**，负责全部玩法逻辑；服务器只负责三件事：
- 让玩家找到房间；
- 把玩家连到同一个转发点；
- 把消息在成员和房主之间转发。

**目标**：
1. **大厅服**：短连接（HTTP）。负责建房、列房、加入房间、离开房间。一个大厅服同时服务多个游戏（客户端），按 `game` 区分。
2. **relay 服**：长连接（先做 TCP），只转发、不做任何逻辑、不做帧同步。成员发的消息只转给房主；房主发的消息按目标转给某个成员或全体成员。
3. **account 可配置同时支持多个服务**：同一套账号登录后，可以进大厅、进 relay，也可以进传统的 game 区服（已有工程不受影响）。

**不做**：
- game 服务。这个场景下不需要，大厅承担房间逻辑（问题 6）。
- 帧同步、状态校验、反作弊、断线托管。
- 房主迁移：房主走了房间就结束，见问题 A。

## 2. 现状与限制（main，2026-11-16 调查）

| 方面 | 现状 | 位置 |
|---|---|---|
| 票据 | `at1.<exp>.<账号>`（账号票据）、`et1.<exp>.<sid>.<账号>`（进服票据）；都由 account 签发和校验，不带服务名 | `service/account/tickets.go:13-28,44-66` |
| account 配置 | 没有服务列表，只有 game 区服表；客户端侧只能配一个 `account.service_type` | `wiring/account/account_mod.go:80-90` |
| 生成器 | `access` 只允许一个玩家服务；`accountService()` 只取第一个 | `codegen/internal/roost/manifest.go:212-233,393-395`、`gate.go:84-91` |
| 大厅、relay | 没有。physicsroom 的 Relay 跑在 Game 侧、经 Gate 绑定推送，房间有服务器侧逻辑，不能直接复用；可以借用的是 `client/wire` 的帧格式、`service/directory` 的 Redis 存储写法、etcd 服务发现 | `wiring/physicsroom/relay.go`、`service/directory/redis_store.go` |

## 3. 总体结构

```
客户端 ──HTTP──▶ account/portal：登录，拿账号票据 at1
客户端 ──HTTP──▶ lobby：凭 at1 建房 / 列房 / 加入
                   └─ 返回 relay 地址 + 短期 relay 令牌
客户端 ──TCP───▶ relay：凭令牌接入房间
                   成员 → relay → 房主
                   房主 → relay → 某个成员 / 全体成员
relay ──NATS───▶ lobby：房间心跳、成员进出、房间关闭
```

- lobby 是无状态的 HTTP 服务，可以多开实例；房间目录放在 Redis 里，带 TTL。
- relay 可以多开实例，经 etcd 服务发现；房间只活在被分配到的那个 relay 实例的内存里。
- 账号票据仍然只由 account 签发和校验（沿用 2026-11-12 定的登录架构）。relay 令牌由 lobby 签发，因为它是房间的凭证，不是账号的凭证（问题 3）。

## 4. 设计

### 4.1 account：同时支持多个服务

- **新增配置 `account.services`**：列出接受本账号体系的服务及其类型：
  ```yaml
  account:
    services:
      - {name: lobby, kind: lobby}   # 无区服，凭账号票据进入
      - {name: relay, kind: relay}   # 无区服，凭 lobby 签的房间令牌进入
      - {name: slg,   kind: game}    # 传统区服（可选；没有就不写）
  ```
- **区服号**：所有 `kind: game` 的服务共用一个全局号段（问题 1），区服表每一行加 `service` 字段，现有的 256 上限放宽。这个场景下不涉及区服。
- **portal**：登录后返回的清单按服务列出（大厅地址、各游戏的区服），`/servers?service=` 按服务筛选。
- **lobby 校验账号票据**：经 account 的 `ValidateAccountTicket` RPC（与 Gate 校验进服票据同一种方式）；lobby 不自己验签。
- **兼容**：线上未部署，不兼容旧配置。没有写 `account.services` 的工程，按现在的单一 game 服务处理。

### 4.2 lobby（大厅服）

- **定位**：框架服务 `service/lobby` + `wiring/lobby`，进 `frameworkCatalog`。生成工程用 `roost add lobby` 加一个 lobby 服务（`framework: lobby` 托管），不写业务代码就能用。
- **接口**（HTTP + JSON，全部要带账号票据 `Authorization: Bearer at1...`）：

  | 接口 | 作用 |
  |---|---|
  | `POST /rooms` | 建房，调用者是房主。参数：`game`、`mode`、`max_members`、`name`、可选 `password`、可选自定义属性（不超过 1KB、不解析）。lobby 选一个 relay 实例（etcd 发现，选房间数最少的），在 Redis 写入房间记录，签发房主令牌 |
  | `GET /rooms?game=&mode=` | 列出可加入的房间：未满、未开始、未过期 |
  | `POST /rooms/{id}/join` | 加入：检查密码、人数，签发成员令牌 |
  | `POST /rooms/{id}/leave` | 离开（也可以直接断开 relay 连接） |
  | `POST /rooms/{id}/state` | 只有房主能调：改成"已开始 / 可加入"，或改自定义属性 |

  返回统一是 `{room, relay_addr, token, expires_at}`。
- **房间记录**：存在 Redis：`<prefix>:room:<id>`，以及按 `game/mode` 的索引。TTL 由 relay 的心跳续期；relay 不再心跳（崩溃或房间关闭），房间自动过期。
- **不做玩法校验**：lobby 只看人数、密码、房主身份。
- **多个游戏共用**：`game` 和 `mode` 只是房间的标签。lobby 可以按配置限定允许的 `game` 列表、每个游戏的最大人数，不做别的区分。

### 4.3 relay 令牌

- **格式**：`rt1.<exp>.<relay实例>.<房间号>.<账号>.<角色 host|member>.<签名>`。签名是 HMAC，密钥由 lobby 和 relay 共同配置（`relay.token_key_env`）。relay 本地验签，不用回调 lobby 或 account。
- **有效期**：短，缺省 30s，只用于建立连接；连上以后与令牌无关。断线重连要重新向 lobby 要令牌（`join` 对已在房间里的人是幂等的）。
- **一次性**：同一账号在同一房间里只保留最新的一条连接，新连接接入时踢掉旧连接，所以令牌被重放也只能顶掉自己。

### 4.4 relay（转发服）

- **定位**：框架服务 `service/relay` + `wiring/relay`，进 `frameworkCatalog`（问题 4）。不写业务、不解析载荷。
- **连接**：TCP，复用 `client/wire` 的帧格式。连接后的第一帧是令牌，验证通过即加入房间；房主的连接到达时房间才算建成，不然房间在令牌有效期之后作废。
- **转发规则**：
  - 成员发的数据帧：relay 加上成员槽位号，**只转给房主**（问题 4）。
  - 房主发的数据帧：带目标（某个成员槽位，或 0 表示全体成员），relay 原样转出。
  - 控制帧（relay 自己生成，发给房主）：成员加入、离开，以及成员的槽位号和账号。房主可以发"踢出某个槽位"的控制帧。
  - 成员之间不直接互发，全部经房主。
- **限额**（配置，缺省值实施时定）：
  - 单帧大小上限；
  - 每连接每秒帧数上限；
  - 发送队列上限：超限的连接被断开，不阻塞别人；
  - 空闲超时；
  - 房间最大人数，以 lobby 给的为准，放在令牌里。
- **房间结束**：房主断开 → 房间立刻关闭，断开全部成员（问题 A）；所有成员都走了、房主还在 → 房间保留，直到房主离开或空闲超时。
- **向 lobby 报告**：每个房间定时经 NATS 发心跳（续 Redis 里房间记录的 TTL，带当前人数）；成员进出、房间关闭时立即发一条。relay 崩溃时房间丢失，Redis 记录随 TTL 过期，客户端看到连接断开。
- **伸缩**：房间固定在建房时选中的 relay 实例上，不迁移。relay 实例通过 etcd 广播自己的对外地址（`relay.advertise_addr`）和当前房间数。

### 4.5 生成器与 `roost.yaml`

- `roost add lobby`、`roost add relay`：加托管服务，补 `account.services`、`relay.token_key_env`、Redis 与 etcd 依赖。
- 一个工程可以只有 account + lobby + relay，不要 game：生成器去掉"必须有一个玩家服务"的限制；有 game 的工程不变。
- **doctor 检查**：
  - `account.services` 列全了工程里的 lobby、relay、game；
  - lobby 与 relay 的 `token_key_env` 指向同一个变量；
  - relay 的 `advertise_addr` 不是回环地址（生产配置）。
- catalog 里"九个框架服务"的说法和对应测试要改（`help.go:278`、`framework_services_test.go:404`）。
- **客户端 SDK**：client 下新增 lobby 的 HTTP 调用和 relay 连接，提供 C# 和 Go 两份，数据帧的载荷由游戏自己定。

## 5. 验证计划

- **单测**：
  - 令牌：签发与校验、过期、篡改、角色；
  - lobby：建房、满员、密码、房主改状态、Redis 过期；
  - relay：
    - 转发方向：成员只到房主，房主到单个成员或全体；
    - 槽位号、踢人、同账号顶号；
    - 限额：超大帧、超速、发送队列满时断开慢连接；
    - 房主断开后关房。
- **生成工程**：account + lobby + relay（不带 game），编译、`generate --check`、doctor 全部 OK。
- **真实进程演练**：
  - 登录 → 建房 → 3 个成员加入 → 双向转发计数准确；
  - kill -9 relay：客户端断开，房间记录在 TTL 内消失；
  - 房主断线：成员全部被断开；
  - 两个 lobby 实例、两个 relay 实例，房间分布正确；
  - 吞吐与延迟基线：每房 8 人，测转发的 p99。

## 6. 维护者已定（2026-11-16）

1. **区服号**：共用全局号段。
2. **大厅与游戏的切换**：大厅是短连接服务，负责建房和获取房间，不需要在同一条 TCP 连接上切换。
3. **客户端连 relay**：房间给出 relay 的短期令牌和连接地址，客户端直连 relay。
4. **relay 的定位**：只转发，不做任何逻辑，不做帧同步，只转发给房主。
5. **relay 协议**：先做 TCP。
6. **大厅范围**：这个场景不需要 game 服务，大厅承担房间逻辑。

## 7. 待确认的细节（括号里是推荐）

- **A. 房主断开**：房间立即关闭（推荐，最简单，符合"房主即 host"），还是保留一个短的宽限期（例如 10s）等房主重连？
- **B. 大厅身份**：凭 account 的账号票据进大厅（推荐，与现有登录链路一致），还是允许匿名（设备号）？
- **C. 房间目录**：放 Redis 带 TTL，lobby 无状态、可以多开（推荐），还是先做单实例 lobby、目录放内存？
- **D. 成员能否互发**：只经房主转发（按你的答复，推荐），还是另开一个"房主允许时成员可广播"的开关？

## 8. 工作量（agent 墙钟时间，第 7 节按推荐）

| 项 | 估计 |
|---|---|
| account：`account.services`、区服归属服务、portal 按服务列出 | 1.5～2 小时 |
| lobby 框架服务（HTTP 接口、Redis 目录、选 relay、签令牌） | 3～4 小时 |
| relay 框架服务（令牌、转发、控制帧、限额、心跳、etcd 广播） | 3～4 小时 |
| 生成器（`add lobby` / `add relay`、无 game 工程、doctor）与客户端 SDK（C# / Go） | 2.5～3.5 小时 |
| 演练、基线与文档 | 2～3 小时 |

合计约 **10～13 小时**，比原方案少约 3 小时：不再需要 Gate 切换绑定和帧同步。按惯例，全量测试只在最后跑一次。
