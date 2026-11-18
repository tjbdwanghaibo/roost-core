# 一套 account 服务多个游戏：账号号按游戏分开（维护者 2026-11-18 “现在就改”）

基于 main `4652985a`（v1.55.0）。本机 macOS（Darwin 25.5，arm64），Go 1.27。线上未部署：账号存储与票据格式不做旧数据、旧进程兼容。

## 1. 决定与实现

| 维护者的目标 | 实现 | 位置 |
|---|---|---|
| 映射键是（游戏，渠道，open id）→ 账号号 | 身份记录 `IdentityRecord{Key, Channel, OpenID, Accounts map[游戏]账号号, Banned, GameTokenGeneration, CredentialVersion, …}`，键是转义后的“渠道:open_id”（原 `Identity.AccountID()` 的形态，改为内部 `identityKey`）；账号记录 `Account{ID, Game, Identity, …}` 只属于一个游戏 | `service/account/types.go`、`identity_accounts.go` |
| 身份级记录不返回客户端 | `Account.Identity` 只存不交：`Login` / `ValidateAccountTicket` 返回 `public()`（清掉身份键）；`IdentityRecord` 只在 account 内部与运维面 `Admin.SetIdentityBanned` | `types.go` `public()` |
| 账号号来自全局计数器，不随机 | 见“偏离”第 1 条：新增 `Config.AccountIDs`（`AccountIDAllocator`），Redis 是 `<account.key_prefix>:account_id` 上的 `INCR`，内存是 `NewMemoryAccountIDs()`；十进制字符串，顺序发号。随机化未做（可选） | `identity_accounts.go`、`redis_store.go` `RedisAccountIDs` |
| 登录要求游戏，必须在 `account.games`；有缺省可省略 | `Identity.Game`；`loginGame`：空则用 `account.default_game`，没配缺省而只有一个游戏就用它，否则 `ErrGameInvalid`（560120）；不在 `account.games` 同样 560120。验证器看得到 `identity.Game` | `identity_accounts.go` `pickDefaultGame` / `loginGame`；`wiring/account` 新键 `account.default_game`，`account.games` 为空 Init 报错 |
| 生成器给新工程的 `config.account.yaml` 写一个游戏 | `accountServiceConfigSection`：`account.games.<工程名>.token_ttl: 2h`（开发与生产示例都写） | `codegen/internal/roost/lobby_relay.go`、`render.go` |
| at1 绑定游戏；`/portal/game-token` 只刷新票据自己的游戏 | 账号票据 `at2.<过期秒>.<游戏>.<账号 base64url>.<签名>`（五段，签名覆盖游戏段），旧 at1 不认；校验时核对账号记录的游戏与票据一致。`IssueGameToken` 的 `game` 不是票据的游戏答 `ErrGameInvalid`；`ValidateGameToken` 核对 token 的游戏是账号的游戏 | `tickets.go`、`game_token.go` |
| 封号单一种类，身份级；`accountctl ban` 按账号号解析到身份 | `Admin.SetIdentityBanned(ctx, accountID, banned, note)`；`accountInGoodStanding` 读账号再读身份，被封答 `ErrIdentityDenied`；login 模块的 accountctl 新增 `ban` / `unban -account <账号号> -note <备注>`，打印身份记录 | `identity_accounts.go`、`admin.go`、`modules/login/files/cmd/accountctl/main.go.tmpl` |
| 凭证版本与 gt2 代数在身份级，各游戏共用 | `IdentityRecord.CredentialVersion` / `GameTokenGeneration`；登录时在 `Identities.Update` 里比较（C6 B 语义不变）；`RevokeGameTokens(accountID)` 由账号号找到身份加一 | `identity_accounts.go` `recordIdentity` / `withCredentialVersion`，`game_token.go` |
| lobby / relay / rehome / 合服 / 踢人名单 / 角色不变 | 未改：它们都按账号号键控；账号号现在只属于一个游戏，角色与 slot 随之按游戏分开 | — |

登录顺序（每一步停下重试都安全）：定游戏 → 渠道验证 → `Identities.Update`（没有就建、记凭证版本变化、读封号）→ 这个游戏还没有号：
发号、`Accounts.Create`（insert-only）建账号记录，再 `Identities.Update` 写映射（并发登录以先写下的为准，输家的账号记录没人引用、
不删）→ `stampLogin` 记最近登录并核对账号确实属于这个身份与游戏。

### 偏离与自行做出的决定（请维护者确认）

1. **“沿用现有的全局分配器”做不到**：main 上没有账号号分配器——账号号就是确定性的“渠道:open_id”（`Identity.AccountID()`）。
   复用玩家号分配器（`Config.Allocator`）会消耗玩家号，并破坏大量“第一个玩家号是 100001”的约定。改为 account 自带一个全局计数器
   （`Config.AccountIDs`，必填；Redis 键 `<account.key_prefix>:account_id`）。若维护者希望别的来源（例如与玩家号同一个计数器），需要再定。
2. **身份记录是新的必填存储** `Config.Identities`（Redis 前缀 `<key_prefix>:ident:`）；`RedisStores` 多两项 `Identities`、`AccountIDs`。
3. **先建账号记录、后写映射**：映射一旦写下就指向已存在、属于这个身份与游戏的账号记录；代价是并发首登的输家、或中途停下，会留下
   没人引用的账号记录（不删；号不再发）。
4. **进服票据仍是 et1**（它带的是这个游戏的账号号，已天然按游戏分开）；账号票据改 at2。
5. **portal 仍看得到渠道与 open id**：那是客户端自己登录请求里的字段，不是 account 交出去的。portal 日志改记 `account.Game`。
6. **`svcaccount.New` 允许 `Games` 为空**（accountctl 不登录也要能构造服务）；运行期的 `account` Mod 要求至少一个游戏（Init 报错）。
7. **没有新增 doctor 检查**：现有 `lobby-relay:account-games` 仍成立，`account.games` 为空由 Mod Init 拒绝。
8. **区服不与 `account.games` 关联**：传统工程只有一个游戏，区服与角色按账号号走即可；多游戏共用一套区服表的语义未定（尚未确认，留给以后）。
9. **签名改为严格 base64 解码**（顺带发现）：32 字节签名编成 43 字符，最后一个字符有 2 位不用；宽松解码让同一张票据有几种拼法都验得过。
   新的 at2 内容让 `TestEveryBadTicketIsRefusedTheSameWay` 改到第 65 字节（签名最后一个字符）时暴露出来；main 上同样存在（潜在，
   只是旧票据的那个位置恰好没被改到）。`verifyTicket` 改用 `base64.RawURLEncoding.Strict()`，变异 M10 证明测试钉住它。

## 2. 先红（main `4652985a` 上的红树）

红树是 main 的独立检出，只加了编译所需的桩（`red-redstub.go.txt`：新字段存在但保持 main 语义——账号号仍是“渠道:open_id”，
`SetIdentityBanned` 落到旧的 `Account.Banned`），改动见 `red-tree.diff`。日志原样：

- `red-account-memory.txt`、`red-account-redis.txt`（`service/account`，内存与单机 Redis）：6 条新场景 5 红——
  `one identity got account "store:alice-local" in party and "store:alice-local" in brawl; want two different ids`；
  `party's account ticket asked for a brawl token: {Token:gt2.…brawl… }, <nil>; want ErrGameInvalid`；
  `one account "store:alice" serves both games; the ban below would prove nothing`（封号、凭证变化两条同）；
  `no game with two games and no default: {ID:store:alice …}, <nil>; want ErrGameInvalid`。
  `TestConcurrentFirstLoginsOfOneGameShareOneAccount` 在 main 上是绿的（号是确定性的，不存在并发发号），它钉的是新实现的并发收敛，由变异 M3 证明有效。
- `red-lobby.txt`（`wiring/lobby` 新增 `TestAccountTokensSeeOnlyTheGameAccount`）：`the party lobby sees "store:alice-open-id", which carries the channel identity`。
- `red-codegen.txt`（`TestNewProjectAccountConfigListsTheProjectGame`）：`configs/service/config.account.yaml does not list the project's game under account.games`。
- accountctl 的 `TestLoginAccountctlBansThePersonInEveryGame` 是新命令的场景，main 上没有 `ban` 命令，未单独录红。

## 3. 后绿

| 范围 | 命令 | 结果 |
|---|---|---|
| 受影响包，内存，`-race` | `GOWORK=off go test -race -count=1 ./service/account/ ./service/lobby/... ./wiring/lobby/ ./wiring/relay/ ./client/relay/... ./wiring/account/`（另跑过 `./service/relay/... ./wiring/gate/`） | 全 ok（`green-race-memory.txt`） |
| account 单机 Redis（共享隔离环境，只 source） | `ROOST_REVIEW3_BACKEND=redis ROOST_REVIEW_REDIS=… go test -race -v ./service/account/`；`REDIS_ADDR=… go test -race -v ./wiring/integration/ ./wiring/account/ ./wiring/lobby/` | ok，0 SKIP；`green-account-redis-single.txt`（227 个 PASS 行）、`green-wiring-redis-single.txt` |
| account 私有 Redis Cluster（3 主 3 从，端口 37100-37105，scratchpad，用完停掉） | `ROOST_REVIEW_CLUSTER=… go test -race -p 1 -v ./service/account/ ./wiring/integration/` | ok，0 SKIP，228 个 PASS 行；`green-redis-cluster.txt` |
| lobby / relay 真实 Redis 与 NATS（共享隔离环境，用例自带随机前缀与私有 subject） | `ROOST_REDIS_TEST_ADDR=… go test -race -v ./service/lobby/... ./wiring/lobby/ ./wiring/relay/ ./client/relay/...` | ok，0 SKIP，128 个 PASS 行；`green-lobby-relay-env.txt` |
| 模块场景（只这一组） | 新工程 `-template game` + `roost module add enter rehome login portal`，`roost module verify --json` | `green-module-verify.json`：account_auth=7 enter=29 login=34 login+enter=6 portal=15 portal+enter=2 portal+login=1 rehome=62 rehome+enter=5，四项工程检查全过 |
| game-demo（只这些包） | `roost project new -template game-demo` 后 build / vet，及 `cmd/loadtest cmd/accountctl services/account/... internal/service/game game/controllers internal/access/... game/modules/login game/managers` | 全 ok（`green-demo-tests.txt`）；`config.account.yaml` 带 `games: planet: token_ttl: 2h`；gate 的 testdata（`codegen/internal/roost/testdata/gate/*.txt`）与 rehome-preload 的 seed 放进生成工程 vet 通过（未执行） |
| codegen（只这几条） | `go test -run 'TestNewProjectAccountConfigListsTheProjectGame|TestLobbyTemplate|TestProjectNewGameTemplateHostsOnly' ./codegen/internal/roost/` | ok |
| 生成物 | `go generate ./service/account/`、`configschemagen` | 与提交内容一致 |

**没跑**（协调者 2026-11-18 改计划：合并后在 main 上并行跑）：`scripts/test-modules-generated.sh` 全矩阵、其余模块的 verify、
`codegen/internal/roost` 整包（按 Account|Lobby|Relay|… 过滤的一次跑了 16 分钟未完，已停，无结论）、根包 `go test -count=1 .`、
仓库全量、pretag、需要真实进程的脚本（test-demo-login / merge / rehome / rehome-preload-cluster、Gate 真实消费者 `TestGateGeneratedBusinessConsumer`）。

场景条数变化：login 33 → 34（`cmd/accountctl` 6 → 7，新增 ban / unban 场景），`modules/login/module.yaml`（`pass: 34`）、
`scripts/test-modules-generated.sh`（两处 `login=34`）与 `modules/login/README.md` 同步；account_auth、portal、rehome 条数不变；codegen 测试里没有钉住条数的地方需要改。

## 4. 变异（每条都应变红）

`mutations.txt`，11 条全红（M3 第一遍幸存，见下）：

| 编号 | 变异 | 变红的测试 |
|---|---|---|
| M1 | 校验路径不看身份的封号 | `TestAnIdentityBanBlocksEveryGame` 等 |
| M2 | 去掉 `IssueGameToken` 的跨游戏检查 | `TestTicketsOfOneGameOpenNothingInAnother` |
| M3 | 去掉 `openGameAccount` 的“采用先写下的号” | `TestConcurrentFirstLoginsOfOneGameShareOneAccount`（`got accounts [8 7 6 5 4 1 2 3]; want one`） |
| M4 | 已有任何游戏的号就沿用（不按游戏取号） | `TestAnIdentityHasADifferentAccountInEachGame` |
| M5 | 账号票据不核对账号的游戏 | `TestTicketsOfOneGameOpenNothingInAnother`（用会话密钥签一张游戏写错的票据） |
| M6 | gt2 不核对账号的游戏 | 同上 |
| M7 | 凭证变化不加代数 | `TestACredentialChangeInOneGameRevokesTheOthers` |
| M8 | 没写游戏、没有缺省游戏也照样登录 | `TestLoginNeedsAGame` |
| M9 | 交出去的账号带身份键 | account 与 `wiring/lobby` 的 `TestAccountTokensSeeOnlyTheGameAccount` |
| M10 | 签名宽松解码 | `TestEveryBadTicketIsRefusedTheSameWay` |
| M11 | Mod 不要求 `account.games` | `TestModReadsGamesAndServices` |

M3 第一遍幸存：8 个并发首登在内存存储上实际是一个接一个完成的，竞争没有发生（`mutations-first-pass` 的结论，原样记在 `mutations.txt`）。
测试改为在领号处汇合（`rendezvousAccountIDs`：每个登录都读到“还没有映射”、各自建了账号记录之后才往下走），重跑变红，修后内存、
单机 Redis、Cluster 均绿。M5 / M6 原本没有场景能触到（签名覆盖游戏段），本轮在场景里加了“用会话密钥签一张游戏写错的票据 / token”一格。
未做的变异：把代数改成按账号存（改动面大，等价于重写身份记录）；由 M7 与跨游戏吊销场景间接覆盖。

## 5. 建议的 CHANGELOG / KNOWN-LIMITS / 升级说明（本轮不改这两个文件）

**CHANGELOG `[Unreleased]` 建议**：

- **account：一套服务多个游戏，账号号按游戏分开**（维护者 2026-11-18）。同一个人（渠道 + open id）在每个游戏里是不同的账号号，
  由 account 的全局计数器发（十进制、顺序）；客户端、lobby、relay 与游戏代码只见到这个游戏的号，看不到别的游戏的号与“是同一个人”。
  身份级记录（新存储 `Config.Identities`）存各游戏的号、封号、凭证版本与 gt2 代数（各游戏共用）。`Login` 要带游戏
  （`Identity.Game`，portal 的 `game` 字段），不带时用 `account.default_game`，只配一个游戏时用它；`account.games` 必填。
  账号票据 at1 → at2（带游戏，五段），`IssueGameToken` 只给票据自己的游戏刷新，跨游戏答 560120。封号改为身份级：
  `Admin.SetIdentityBanned`，login 模块的 accountctl 新增 `ban` / `unban -account <账号号> -note <备注>`。
  `Account` 去掉 `Channel` / `OpenID` / `Banned` / `GameTokenGeneration` / `CredentialVersion`，多 `Game`；`Identity.AccountID()` 删除。
  新工程的 `config.account.yaml` 写上工程名的游戏。签名改为严格 base64 解码（同一张票据只有一种写法）。
- **升级需清空 account 的存储（Redis 前缀下全部键）；先停旧进程再起新进程**；已生成的工程需要手工：account 配置补 `account.games`，
  自己构造 `svcaccount.Config` 的代码补 `Identities`、`AccountIDs`（`NewRedisStores` 已带）与 `Games`，按账号号写死的测试改为读 `Login` 的返回。

**KNOWN-LIMITS 建议**：

- 账号号顺序发号，可被猜到相邻号；需要时可改为随机化（维护者可选，本轮未做）。
- 按游戏封号未做：封号只有身份级一种。
- 首登并发的输家、或首登中途停下，留下一条没人引用的账号记录（不删）。
- 区服表与 `account.games` 不关联：多个游戏共用一套 account 时，区服 / 角色按账号号天然分开，但“哪个服属于哪个游戏”没有声明与校验（尚未确认是否需要）。
