# 大厅 / relay 实施：account 组证据（2026-11-17）

方案 `docs/maintenance/FEATURE-2026-11-16-lobby-relay.md` §4.1（游戏账号 token、多个服务）与 §5 里游戏账号 token 的单测。

## 文件

| 文件 | 内容 |
| --- | --- |
| `unit-account.txt` | `go test -race -count=1 -v ./service/account/ -run '<新增与改动的用例>'`、`./wiring/account/ -run TestModReadsGamesAndServices` |
| `unit-portal.txt` | 全新生成的 game 工程（`roost project new fresh -template game`，replace 到本检出）装 `portal enter` 之后，`go test -race -count=1 -v ./services/account/portal/ -run '^TestPortal'`：15 条 |
| `module-verify.txt` | 同一个工程 `roost module verify portal login`：portal 15、portal+enter 2、login 37、login+enter 6，四项工程检查全过 |
| `mutation.txt` | 20 条变异，全部被杀（脚本在 scratchpad 的 `b1117-lr-account/mutate.py`：每条替换一处关键代码、跑对应用例、恢复原文件；portal 的变异改的是生成工程里渲染好的模板副本） |

## 单测与 §5 条目的对应

| §5 条目 | 用例 |
| --- | --- |
| 签发与校验 | `TestGameTokensAreIssuedPerGameAndValidatedByAccount`（本地与经总线；形状、按游戏的寿命、只读） |
| 过期 | `TestAGameTokenExpiresOnTheSystemClock`（到点那一秒拒绝；系统时钟） |
| 篡改 | `TestATamperedGameTokenIsRefused`（改过期、游戏、账号、签名，别的密钥，截断，超长） |
| 拿 at1 / et1 冒充 | `TestOtherTicketsCannotPassAsGameTokens`（at1、et1、角色会话令牌当 gt1；at1 的签名配 gt1 的内容；gt1 反过来当 at1 / et1） |
| 游戏不在 `account.games` | `TestOnlyTheConfiguredGamesGetTokens`（不签发；配置里去掉的游戏已发的 token 也不认；名字 / 重复 / 寿命写错启动不了） |
| 拿 A 游戏的 token 进 B 游戏 | `TestAGameTokenOnlyOpensItsOwnGame`（`GameAccount.RequireGame`）；portal 侧 `TestPortalHandsOutGameTokensByEndpointAndAtLogin` 里 brawl 的 token 过不了 party |
| 封号后当场拒绝 | `TestABanRefusesGameTokensAtOnce`（已签出的 token、再签新的，本地与经总线；账号没了答 560103） |

多个服务：`TestWithoutServicesTheServerListIsUnchanged`、`TestServersBelongToTheOnlyGameService`、`TestTwoGameServicesShareOneServerNumbering`、`TestBadServicesAreRefusedAtStart`；区服上限 256 → 1024：`TestTheServerIndexRefusesNewServersWhenFull`（需求数改了，断言没有放宽）。portal：`TestPortalRefusesGameTokensItCannotStandBehind`、`TestPortalListsServersByService`。

## 已知没有覆盖

- 真实 Redis 下的 account 用例：`pendingContractService` 在 `ROOST_REVIEW3_BACKEND=redis` 时走真实 Redis，这一轮没有跑（只跑了内存存储）。
- 真实进程演练（登录 → 换 gt1 → lobby 校验）归协调者接线后统一做。
- `cmd/accountctl upsert-server` 没有 `-service` 参数（文件在 `demo/cmd/accountctl`，不归本组）。

## 建议写进 CHANGELOG 的文字（由协调者统一写）

- **account：游戏账号 token 与多个服务**（方案 2026-11-16 §4.1）：
  - 新增 `gt1.<过期秒>.<游戏>.<账号>.<签名>`：与 at1 / et1 同一把会话密钥、独立的域前缀，互相不能冒充。`account.games.<游戏>.token_ttl`（必填，至多 24h）列出服务的游戏。
  - 总线接口 `IssueGameToken(ctx, accountTicket, game)`、`ValidateGameToken(ctx, token) (GameAccount{AccountID, Game, ExpiresAtUnix}, error)`、`ListServices(ctx)`；新错误码 `ErrGameInvalid`（560120）。调用方用 `GameAccount.RequireGame` 核对自己的游戏。
  - `account.services.<服务>.kind`（game / lobby / relay）；区服记录加 `service` 字段，没写时归到唯一的 game 服务；不配置时行为不变。区服上限 `MaxServers` 256 → 1024（portal 的 `MaxEntries` 跟着）。合服只在同一个 game 服务里。
  - portal 模块（只对新安装的工程）：新增 `POST /portal/game-token`；登录可选 `game`，答复里多一张 gt1；`/portal/servers` 可选 `service` 筛选，答复多 `services`（地址来自新配置 `portal.services`），服上多 `service`。portal 场景 12 → 15。

## 建议写进 KNOWN-LIMITS 的文字（由协调者统一写）

- **portal 的边界**一段里“三个端点”改成四个，并补：游戏账号 token 不能单张吊销（同 at1）；登录带了不认识的 `game` 时整个登录失败；`portal.services` 与 `portal.servers` 一样不在配置声明里。
- **account 的配置**：`account.games` / `account.services` 的条目至少要写一个键（`token_ttl` / `kind`），空条目 `party: {}` 配置加载器看不见、会被静默忽略——所以 `token_ttl` 定为必填。
- `cmd/accountctl upsert-server` 还不能指定服务；两个以上 game 服务的工程要经代码登记区服。
- `docs/agent-skills/roost-genre/references/compose.md` 第 275 行“三个 HTTP 端点”要跟着改。
