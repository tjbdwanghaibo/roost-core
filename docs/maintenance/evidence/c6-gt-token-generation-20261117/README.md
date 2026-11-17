# 交接 C6：游戏账号 token 按账号整代吊销（选项 2，2026-11-17）

维护者 2026-11-17 的决定：做方案 [open-work-c6-gt1-revocation](../open-work-c6-gt1-revocation-20261117/README.md) 的选项 2——账号上加 token 代数，token 里带签发时的代数，校验时比较；运维用 accountctl 加一，改密码时自动加一。不做单张吊销（选项 3）、不做吊销后断开正在进行的房间、portal 不加登出接口。不兼容旧 gt1（未上线）。

## 做了什么

- **格式**：`gt1.<过期秒>.<游戏>.<账号>.<签名>` → `gt2.<过期秒>.<游戏>.<账号>.<代数>.<签名>`（`service/account/game_token.go`）。代数是十进制无符号整数，在签名内容里。
  - 与别的凭证不混淆：at1 四段、et1 五段、gt2 六段、角色会话令牌两段（`base64(载荷).base64(签名)`），版本前缀各不相同；at1 / et1 / gt2 的签名还各有各的域前缀（gt2 沿用 `roost.account.game-token\n`）。relay 令牌 `rt1` 八段、密钥是 lobby 与 relay 另配的那一把。旧 gt1（五段）一律答 `ErrSessionInvalid`，用例里有“同一把密钥、同一个域签出来的 gt1 形状”这一条。
  - 签名覆盖代数：把吊销前的 token 的代数段改成当前代数，签名对不上（`TestRevokingGameTokensRefusesTheOnesIssuedBefore` 里的 forged 一条、`TestATamperedGameTokenIsRefused` 的 another generation / a generation with a sign）。
- **账号记录**：`Account.GameTokenGeneration uint64`（JSON `game_token_generation,omitempty`），只增不减，零是从未吊销过。存储格式只是多一个可省略的字段。
- **签发**：代数取 `ValidateAccountTicket` 刚读到的账号记录。与吊销并发时读到旧代数的，签出来就是失效的——吊销赢。
- **校验**：验签、过期、游戏之后照旧重读账号（为了封号本来就读），先看封号（`ErrIdentityDenied`，行为不变），再比较代数，不等答 `ErrSessionInvalid`（与坏 token 同一个回答，指标上记 `revoked`）。**没有多一次读。**
- **吊销**：`Service.RevokeGameTokens(ctx, accountID) (generation uint64, error)`，同时加进总线接口 `Accounts`（`//roost:rpc`，重新生成了 `accounts_rpc_gen.go`）。经 `versionstore.Update` 把代数加一并落库，返回新代数。账号不在、空账号答 `ErrAccountMissing`，什么也不写；被封的账号也能吊销（解封之后旧 token 不复活）。存储结果未知时原样返回：重试可能多加一次，后果只是这期间新签的也失效，不会让旧的复活。
- **运维命令**：login 模块的 `cmd/accountctl` 自带子命令 `revoke-game-tokens -account <账号号>`（命名照 upsert-server / show-server / list-servers 的“动词-对象”），打印 `{"account_id", "game_token_generation"}`。accountctl 本来就是在 account 的同一份存储上直接建 `svcaccount.Service`（不连总线），这里调的就是总线接口上的那个方法。原来的 `serverRecords` 接口改名 `accountRecords` 并加了这个方法。
- **改密码自动加一：account 里没有改密码的路径。** 查过的地方：
  - `service/account`：`Login` 由 `Verifier` 向渠道确认身份，账号号由 `(渠道, open_id)` 推出；已存的账号与渠道确认的身份不符时拒绝，没有换绑、没有改凭证的接口。
  - `modules/account_auth`：`login_dev.go`（凭据等于配置里的 `login.dev_token`）、`login_channels.go`（`verifyChannel` 缺省全拒，工程自己接平台验签）、`login_player_id.go`、`collaborators.go`：没有密码。
  - `modules/login`（握手认证器、accountctl）、`modules/portal`（`/portal/login`、`/portal/servers`、`/portal/enter`、`/portal/game-token`）：没有改密码 / 换绑端点。
  - 结论：密码（如果有）在渠道那一侧。按维护者“没有就不要凭空造”的要求，没有造改密码功能；入口就是总线上的 `RevokeGameTokens`，由凭证变更发生的那一方（渠道服务、工程自己的账号中心）在改密码、换绑、报告泄露时调。写在 `game_token.go` 的注释、`account_rpc.go` 的接口注释、`modules/account_auth/README.md` 的“装完要自己做的”第 4 条。**这一点要维护者定（见末尾）。**
- **gt1 → gt2 的字样**：lobby（`service/lobby` 注释）、portal 模块（注释、测试夹具 `gt2.1800007200.party.`、README、module.yaml 的 next）、生成器（`cli.go`、`help.go`、`framework_services.go` 的提示与注释、`codegen/docs`）、Go / TS / C# SDK 的注释与测试夹具（`client/relay`、`client/ts`、`client/dotnet`）、现行文档（LOBBY、09-services、CAPABILITIES、compose.md、modules/README、方案 FEATURE-2026-11-16）。`wiring/` 里本来没有 gt1 字样。CHANGELOG、release notes、KNOWN-LIMITS、交接文档与旧证据没改（前两者按规则由协调者写；旧证据是当时的事实）。LOBBY.md 引用 2026-11-17 演练证据的那一句写明“当时的格式是 gt1”。
- **文档**：方案 §4.1.1 加“吊销”一条；LOBBY.md §3 鉴权加“吊销”一条；09-services 的游戏账号 token 一段加“按账号整代吊销”；portal README 的“票据不能单张吊销”补上 gt2 可按账号整代吊销、portal 无登出、at1 有效期内仍能换新 gt2；login README 与 module.yaml 补 `revoke-game-tokens`；account_auth README 补“凭证变更时调 `RevokeGameTokens`”。

## 先红后绿

| 文件 | 内容 |
| --- | --- |
| `red-head-compile.txt` | 新用例放到 base `30131729` 的实现上：`RevokeGameTokens`、`GameTokenGeneration` 不存在，编译失败 |
| `red-no-generation-check.txt` | 实现的第一步（格式带代数、吊销落库，但校验还不比较代数——等于修前“旧 token 一直有效”的行为）：`TestRevokingGameTokensRefusesTheOnesIssuedBefore` 红在 `local: alice's party token issued before the revocation: {AccountID:store:alice-local Game:party ExpiresAtUnix:1700007200}, <nil>; want ErrSessionInvalid`；`TestRevokingGameTokensLeavesBansAndTicketsAlone` 红在 `a token revoked while banned came back after the unban: <nil>` |
| `green.txt` | 加上比较之后，`GameToken|OtherTickets` 8 条全过（内存后端） |
| `mutations.txt` | 三个变异全部被杀（见下） |

变异（`scratchpad` 里的脚本逐个改 `game_token.go`、跑 `-run GameToken`、还原）：

- M1 校验不比较代数 → KILLED（两条吊销用例红）。
- M2 加一不持久化（Update 不保存、只在返回值里加一）→ KILLED（`bus: alice's party token issued before the revocation ... want ErrSessionInvalid`；另一条红在解封后旧 token 复活）。
- M3 签发时不写当前代数（总写 0）→ KILLED（`a token issued after the revocation = {Token:gt2....0....}; want generation 1 in it`）。

accountctl 的红：新用例 `TestLoginAccountctlRevokesAnAccountsGameTokens` 在旧模板上编译不过（`accountRecords` 不存在、没有 `revoke-game-tokens`）；它端到端依赖 account 的比较，M1 下同样红（没单独存档）。portal 模板的红：只改了 account、portal 夹具还是 `gt1.1800007200.party.` 时，生成工程里 `TestPortalHandsOutGameTokensByEndpointAndAtLogin` 红在 `portal_test.go:1550: /portal/game-token: 200 {"ok":true,"game":"party","game_token":"gt2.1800007200.party.ZGV2OmFsaWNl.0.…"}`，改夹具后绿（没单独存档）。

## 验证（全部在本分支，2026-11-17）

| 命令 | 结果 |
| --- | --- |
| `GOWORK=off go vet ./service/account/ ./service/lobby/ ./wiring/lobby/ ./client/relay/ ./codegen/internal/roost/`；`go build ./...`；`gofmt -l` | 通过，无输出 |
| `GOWORK=off go test -race -count=1 ./service/account/ ./service/lobby/ ./wiring/lobby/ ./wiring/account/ ./wiring/gate/ ./wiring/integration/ ./client/relay/ ./codegen/internal/servicerpc/` | 全部 ok |
| 真实 Redis：`source ~/.roost-it/roost-dataengine-it/env.sh`，`ROOST_REVIEW3_BACKEND=redis ROOST_REVIEW_REDIS=$ROOST_DATAENGINE_IT_REDIS_ADDR go test -race -count=1 -v ./service/account/ -run 'GameToken\|OtherTickets'` | 8 条全过（`account-redis-game-token-cases.txt`，地址已替换） |
| 同上，整包 `./service/account/ ./wiring/account/` | 两包 ok，236 条 PASS（含子用例），0 SKIP，0 FAIL（`account-redis-full-package.txt`） |
| 反证：`ROOST_REVIEW_REDIS=127.0.0.1:1` 跑两条吊销用例 | 两条都在 `dial tcp 127.0.0.1:1` 失败（`account-redis-negative-control.txt`）：吊销用例确实走 Redis（`pendingContractService`） |
| `GOWORK=off go test -count=1 -timeout 60m ./codegen/internal/roost/ -run 'TestModulePortalInstallsOnTheHostedAccountService\|TestModulePortalInstallsOnALobbyProjectWithoutGame\|TestModuleLoginReplacesTheAuthenticatorAndAccountStubs\|Lobby\|Help'` | ok（66s）；login 的场景数断言 32 → 33 |
| 生成工程：`roost project new -template game` + `roost module add login portal`，依赖 replace 到本分支，`go vet` + `go test -race ./cmd/accountctl/ ./services/account/...` | 三包 ok，accountctl 6 条（含新的一条）、account_auth 6 条、portal 16 条（`generated-game-login-portal.txt`） |
| 同一工程 `roost module verify --json` | ok：account_auth 6、login 33、portal 15、portal+login 1，零跳过；build / vet / id check / generate --check 通过（`generated-module-verify.txt`） |
| 生成 `-template game-demo`（`go mod tidy` 用 `GOPROXY=off` 只读本机模块缓存），`go vet` + `go test -race ./cmd/accountctl/ ./services/account/...` | 三包 ok（demo 的 operator 命令与新的自带命令不冲突） |
| `scripts/test-client-ts.sh`（`node_modules` 临时软链主检出里已有的那份，跑完删掉；没有下载） | 60 条全过（`client-ts.txt`） |
| `dotnet run --project client/dotnet/Roost.Client.Relay.Tests -- <本检出>` | all 409 checks passed（`client-dotnet-relay.txt`） |

`scripts/test-modules-generated.sh` 里 login 的计数改成 33，脚本本身没跑（全模块装一遍很慢，留给协调者的统一全量）。`go test ./...`、pretag、TestDemo 全量没跑（按规则由协调者最后统一跑）。

## 尚未确认

- 签名“覆盖代数段”这一点只有正向用例（改代数被拒），没有做“签名不覆盖代数”的变异：格式上签名覆盖整个 body（`verifyTicket` 取最后一段之前的全部），构造这个变异要改共用的 `verifyTicket`，没做。
- Redis Cluster（`ROOST_REVIEW_CLUSTER`）没跑；只跑了单机 Redis。
- 真实进程演练（account / lobby / relay 三进程，吊销之后 lobby 答 401）没跑：lobby 只拿 account 的校验结果，接口没变，靠 `wiring/lobby` 的 `AccountTokens` 映射（`session_invalid` → 401）与 account 的用例推出，没有实测。
- `scripts/test-modules-generated.sh` 全量、`go test ./...`、TestDemo 没跑（见上）。

## 限制（框架本身的，不是遗留）

- 只能按账号整代吊销，不能单张吊销（维护者不要选项 3）：吊销一个设备的 token 会让同一账号所有设备的 gt2 一起失效。
- 吊销只拦以后的大厅调用（建房、列房、加入、离开、改状态）；已经发出的 relay 令牌、已经连上 relay 的连接不受影响（维护者不要断开房间）。
- 吊销不碰账号票据与进服票据：at1 还在有效期内（缺省 `account.ticket_ttl` 10 分钟）时，持有 at1 的人仍能换到新的 gt2。见下面待定点 2。
- 不带 game 的工程（`-template lobby`）装不了 login，也就没有 accountctl；那种工程的运维吊销要经总线调 `RevokeGameTokens`（或自己写一个小工具）。见待定点 3。

## 要维护者定的点

1. **“改密码自动加一”没有落点**：account 里没有改密码 / 改凭证 / 换绑的路径（身份由渠道确认）。现在的做法是：`RevokeGameTokens` 在总线上，由凭证变更发生的那一方调，接法写在 account_auth README。可选：
   - A（推荐，现状）：保持这样，不在 account 里加凭证变更入口；工程接渠道时自己在改密码处调总线接口。代价：靠工程记得调，框架没有自动的那一下。
   - B：在 account_auth 的协作者里加一个“凭证变更”的登记点（例如渠道验证器返回“凭证版本”，account 在 `Login` 时发现版本变了就自动加一）。代价：要改 `Verified` / `Verifier` 的契约与 account_auth 模板，工程的渠道实现要多返回一个字段。
2. **at1 有效期内还能换新 gt2**：改密码 / 泄露场景下，如果 at1 也泄露了，吊销 gt2 之后的最多 `account.ticket_ttl`（缺省 10 分钟）内对方还能再换。可选：
   - A（推荐）：不动，文档写明（已写在 portal README）；at1 寿命短。
   - B：at1 也带同一个代数（at2），吊销时一起失效。代价：at1 的格式与全部 at1 调用方（portal、manifest、login）的夹具都要改，另一轮。
3. **不带 game 的开房间工程没有 accountctl**：accountctl 在 login 模块里，`-template lobby` 工程装不了 login。可选：A（推荐）现状，经总线调；B 把 accountctl（或只把 revoke-game-tokens）挪进 account_auth 模块。代价：模块文件归属与安装器的 `replaces` 要跟着改。

## 建议写进 CHANGELOG `[Unreleased]` 的文字

- **游戏账号 token 改成 gt2，可按账号整代吊销（交接 C6 选项 2，维护者 2026-11-17；升级需让客户端重新取 token）**：格式 `gt2.<过期秒>.<游戏>.<账号>.<代数>.<签名>`，旧 gt1 一律答 `ErrSessionInvalid`（未上线，不兼容）。账号记录多 `GameTokenGeneration`；签发时写进 token（签名覆盖它），`ValidateGameToken` 与重读的账号比较（不多一次读）。新总线接口 `Accounts.RevokeGameTokens(ctx, accountID) (generation, error)`：代数加一并落库，在此之前签发的全部失效、之后签发的照常可用，封号行为不变。login 模块的 `cmd/accountctl` 新增 `revoke-game-tokens -account <账号号>`（只对新安装的模块；login 场景 32 → 33）。account 没有改密码路径，凭证变更由渠道 / 账号中心经总线调 `RevokeGameTokens`（account_auth README）。不做单张吊销、不断开已连上 relay 的房间、portal 不加登出。lobby 接口不变。证据 本目录。

## 建议的 KNOWN-LIMITS 改动

- 第 500 行 portal 的边界里“账号票据、进服票据与游戏账号 token（gt1）不落库、不能单张吊销，有效期内可重复使用（封号对 gt1 当场生效）。”改为：“账号票据、进服票据与游戏账号 token（gt2）不落库、不能单张吊销，有效期内可重复使用（封号当场生效）；gt2 可按账号整代吊销（`RevokeGameTokens`，运维用 `accountctl revoke-game-tokens`），吊销不碰 at1 / et1，at1 有效期内仍能换新的 gt2，已连上 relay 的房间不断开。”
- 交接 C6 一条可以结掉；上面“要维护者定的点”1～3 如果维护者不当场定，作为遗留登记（尚未确认的做法，不是框架限制）。
