# 不用 roost 的联机方案：Steam Lobby + Netcode for GameObjects + Steam 传输层

适用：[compose.md §0C](compose.md#0c-先判断这个需求真的需要这套服务器框架吗) 判断为“不需要 roost”或“只有局内部分不需要”。典型情况是只在 Steam 上发行的 Unity 游戏，需要联机开房间，一个房间人数少，由某个玩家的机器当主机。

这篇是通用方案，按它给用户的项目写一份具体的联机方案（见第 9 节），**不生成 roost 工程**。第三方库的包名与 API 随版本变化，下文的名字以写作时（2026-11）的公开版本为准，动手前要对照项目实际装的版本核对；下文代码没有在本仓库编译验证过。

## 1. 三层各管什么

| 层 | 用什么 | 管什么 | 不管什么 |
| --- | --- | --- | --- |
| 房间与社交 | Steam Lobby（Steam Matchmaking） | 建房、房间列表与筛选、好友邀请、成员进出、房间元数据（地图、模式、版本、房主的 SteamID） | 游戏内同步；房间里的游戏状态 |
| 游戏内同步 | Unity Netcode for GameObjects（NGO） | 房主机器上跑 Host（同时是服务器与客户端）；网络对象生成、`NetworkVariable` 状态同步、RPC、场景切换、连接审批 | 找房间、邀请；房主离开后的接管 |
| 传输 | Steam 传输层（走 Steam 的 P2P 与中继） | 用 SteamID 寻址，不需要公网 IP、端口转发和自建服务器；NAT 穿透与中继由 Steam 负责 | 以上两层的逻辑 |

Steam SDK 的 C# 封装二选一：

- **Facepunch.Steamworks** 配社区的 **Facepunch 传输**（`com.community.netcode.transport.facepunch`）。新项目建议用这一组：社区反馈里能用的例子更多。
- **Steamworks.NET** 配社区的 **SteamNetworkingSockets 传输**（`com.community.netcode.transport.steamnetworkingsockets`）。项目已经在用 Steamworks.NET 时选这一组。它有过连接问题的报告，接入后先做第 7 节的连通测试。

两种传输都在 Unity 的 `multiplayer-community-contributions` 仓库的 `Transports/` 下，不是 Unity 官方维护的包。装之前看它们标的 NGO 版本要求。

## 2. 整体流程

```text
启动：SteamClient.Init(appId) → 读命令行有没有 +connect_lobby <id>（从好友邀请启动游戏时会带）
建房（房主）：CreateLobbyAsync(人数上限) → 设可见性（公开 / 仅好友 / 私密）与 Joinable
            → SetData：version、mode、map、host_steam_id → NetworkManager.StartHost()
找房（公开房）：LobbyList.WithKeyValue("version", 当前版本)… RequestAsync() → 列表 → Join
邀请（好友）：OpenGameInviteOverlay(lobby.Id) → 对方 OnGameLobbyJoinRequested → Join
加入（成员）：OnLobbyEntered → 读 host_steam_id（或 lobby.Owner）→ 设到传输层的目标 SteamID
            → NetworkManager.StartClient() → 房主的连接审批（版本、人数、黑名单）
开局：房主 SetJoinable(false)（不想中途加入时）→ NetworkManager.SceneManager.LoadScene(...)
局内：NGO 同步（第 3 节）
结束：房主回到房间场景、SetJoinable(true)；或所有人 Shutdown() + lobby.Leave()
掉线：成员掉线由房主的 OnClientDisconnectCallback 处理；房主掉线见第 5 节
```

## 3. 局内同步的写法

- **房主权威**：会影响结果的逻辑（伤害、拾取、胜负、计分、刷怪、随机）只在房主上跑（`IsServer` 分支），成员只发意图。
- **状态用 `NetworkVariable`**，变化由 NGO 推给所有人。**意图用 RPC**：成员 → 房主的 RPC 在房主上重新校验（距离、冷却、是否拥有该对象），不信任成员传来的结果。NGO 版本较新时用统一的 `[Rpc(SendTo.Server)]` 写法，较旧的是 `[ServerRpc]` / `[ClientRpc]`。
- **移动**：用 `NetworkTransform`。小人数合作游戏可以让各自的角色由拥有者权威（owner authoritative），手感好、实现简单，代价是可以改包瞬移；对公平敏感时改成房主权威加客户端预测，复杂度高很多。这一条是“服务器 / 客户端边界”的典型取舍，专业模式下由开发者定。
- **网络预制体**：要同步的对象都登记进 NetworkManager 的预制体列表，由房主 `Spawn`。
- **连接审批**（`ConnectionApprovalCallback`）：检查版本号（成员把版本放进连接载荷）、人数上限、被踢过的 SteamID；不通过就拒绝，并给出原因。
- **场景**：只用 `NetworkManager.SceneManager` 切场景，不要各端各自 `SceneManager.LoadScene`。
- **带宽**：同步频率与 `NetworkVariable` 的数量决定上行，压力都在房主的上行带宽上；人数越多，房主的网络越是瓶颈。这也是这个方案只适合小人数的原因。

## 4. 房间元数据约定

| 键 | 内容 | 用途 |
| --- | --- | --- |
| `version` | 游戏的网络协议版本 | 列表只显示同版本的房；连接审批再查一次 |
| `host_steam_id` | 房主的 SteamID | 成员设传输层目标；也可以直接取 `lobby.Owner` |
| `mode` / `map` | 玩法与地图 | 房间列表的筛选与展示 |
| `state` | `lobby` / `in_game` | 局内是否允许加入 |

Lobby 的元数据谁都看得见，不要放任何秘密。

## 5. 房主离开

NGO 没有内置的房主迁移：房主退出或掉线，这一局就结束了。Unity 给 NGO 的迁移方案是 Distributed Authority，它依赖 Unity 自己的联机服务，不属于这条 Steam 方案。可以做的是：

- **最简单（建议先做）**：房主断开时所有成员收到断开回调，回到主菜单，提示“房主已离开”。
- **可恢复**：房主定期把可恢复的局内状态存在本地（存档或 Steam Cloud），成员重新进房间后由原房主重开这一局。要先想清楚哪些状态要恢复，做不到无缝。
- **换房主重开**：房主离开时 Steam Lobby 会把房主身份交给另一个成员（`Owner` 改变）。新房主可以用自己保存的最近状态重新 `StartHost`，其余人重连。这比上一种复杂，只在需求明确要求“房主走了游戏还能继续”时做。

需求里有“房主走了也要继续”“长时间的持久世界”时，回到 [§0C](compose.md#0c-先判断这个需求真的需要这套服务器框架吗) 重新判断，可能需要专用服务器。

## 6. 局外数据

| 需求 | 用什么 |
| --- | --- |
| 存档、设置 | Steam Cloud（每个玩家自己的文件） |
| 成就、统计 | Steam Stats / Achievements |
| 道具、皮肤（不要求防作弊） | Steam Inventory Service |
| 好友、邀请、在线状态 | Steam Friends 与 Rich Presence |
| 要防作弊的货币、养成、付费到账、排行榜要求可信 | **这条方案做不到**：房主的机器可以改包。局外这部分用 roost（[§0C](compose.md#0c-先判断这个需求真的需要这套服务器框架吗) 的“只有一部分需要”），见第 8 节 |

## 7. 开发与测试

- 原型阶段用 Valve 的测试 AppID 480（Spacewar）。正式 AppID 下来后改掉，并在 Steamworks 后台配好。
- 同一台机器上不能用同一个 Steam 账号开两个客户端联机测试。要么两台机器两个账号，要么开发期把传输层换成 Unity Transport 走本机地址，传输层做成可配置的。
- 连通测试至少做：同一局域网、不同网络（验证 Steam 中继）、一方在严格 NAT 后面、好友邀请从“游戏没开”启动（`+connect_lobby`）、房主掉线、成员掉线后重进。

## 8. 与 roost 一起用（只有局外需要服务器时）

局内照上面的方案，局外的持久状态放进 roost 工程（按 compose.md 正常生成）。分工与对接：

- **登录**：客户端向 Steam 取认证票据（Steam 的 auth session ticket），交给 roost 的登录入口；roost 工程在 account 的渠道验签（`internal/service/account/login_channels.go`）里调 Steam 的 Web API 校验票据，拿到 SteamID 作为 open_id。渠道验签要项目自己写（compose §3A）。
- **局内结果上报**：只有房主知道结果，而房主可以作弊。要可信的结算就不能只信房主：可以让每个成员各自上报、服务器比对；或者只发放“作弊也无所谓”的奖励。这个取舍要写进说明文档，并问开发者。
- **两边的号**：Lobby、NGO 用 SteamID；roost 用自己的账号号与玩家号。对应关系由 account 维护，客户端不传玩家号。

## 9. 交付物

选了这条方案时，skill 不生成 roost 工程，而是给用户写一份联机方案文档（建议放在用户游戏项目的 `docs/NETWORK-PLAN.zh-CN.md`；用户没有项目目录时直接在报告里给出）。照第 1～7 节逐项填成这个游戏自己的：

1. 判断依据（§0C 的信号逐条对照）与用户的选择；
2. 选用的 Steam 封装、传输层与版本（写明要核对兼容性）；
3. 房间：人数上限、可见性、元数据键、是否允许中途加入；
4. 网络对象清单：每个对象由谁生成、谁拥有、同步哪些 `NetworkVariable`；
5. RPC 清单：谁发给谁、房主上校验什么；
6. 服务器（房主）/ 客户端边界表：每个逻辑点放哪一端、理由、风险（专业模式由开发者定）；
7. 房主离开与掉线的处理；
8. 局外数据放在哪里；需要 roost 的部分与对接方式；
9. 测试清单（第 7 节）。

两种模式都适用：业余模式按上文的缺省做法填并标出替用户定的项，专业模式就第 3～8 项逐项问开发者。后续增改需求同样追加到这份文档的末尾，记录每一轮的原话与决定（与 compose §2A 的做法一致）。

## 参考

- Unity `multiplayer-community-contributions` 仓库的 `Transports/`（Facepunch、SteamNetworkingSockets 两种传输的 README 与版本要求）
- Netcode for GameObjects 文档（NetworkManager、NetworkVariable、RPC、连接审批、场景管理）
- Steamworks 文档：Matchmaking & Lobbies、Steam Networking（SDR 中继）、User Authentication and Ownership（认证票据）、Steam Cloud、Inventory Service
