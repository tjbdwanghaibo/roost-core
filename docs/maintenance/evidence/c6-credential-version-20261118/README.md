# 交接 C6 第 1 点：登录时按凭证版本自动吊销游戏账号 token（维护者 2026-11-18 选 B）

背景见 [c6-gt-token-generation-20261117](../c6-gt-token-generation-20261117/README.md)“要维护者定的点”第 1 条：account 没有改密码路径（身份由渠道确认），维护者选 B——渠道验证器返回一个凭证版本，account 在登录时发现版本变了就自动把 gt2 代数加一。第 2、3 点（at1 有效期内仍能换新 gt2、`-template lobby` 工程没有 accountctl）维护者没改，按现状。基线 main `616c5a90`（v1.54.0 已发布）。

## 做法

- **契约**（`service/account/identity.go`）：`Verified` 多一个 `CredentialVersion string`（JSON `credential_version,omitempty`）。不透明字符串，account 只比较是否相等、不排序；空表示渠道不提供，行为同修改前。上限 `MaxCredentialVersionBytes = 128`，超过的是渠道实现的错，`Verified.Validate` 答 `ErrIdentityInvalid`，什么也不写。选字符串而不是 uint64：渠道给的可能是版本号、改密时刻、凭证指纹，字符串都放得下，渠道不用自己编号。
- **账号记录**（`types.go`）：`Account.CredentialVersion string`（JSON `credential_version,omitempty`），上一次渠道给的版本。存储格式只是多一个可省略的字段；未上线，不做旧数据兼容（旧记录读出来是空，等于“从没见过版本”，第一次登录只记录）。
- **登录**（`service.go`）：`Login` 本来就在 `Accounts.Update`（versionstore 的比较后写入，冲突重读重跑 mutate）里写最后登录时间；新函数 `withCredentialVersion` 在同一个 mutate 里判断：
  - 渠道没给（空）：不动，已存的版本也不抹掉；
  - 账号上没存过：只记录，不加一；
  - 相同：不动；
  - 不同：`GameTokenGeneration++`（与 `RevokeGameTokens` 同一个字段、同一种落库路径），换成新版本。
  - **被封的账号**：登录照旧答 `ErrIdentityDenied`、不改最后登录时间，但版本变化照样落库（只写版本与代数）。原因：封号期间改了密码，解封之后改密之前签发的 gt2 不应复活到下一次登录。为此把封号的拒绝从 mutate 的返回错误挪到 Update 之后（mutate 每次调用重设 `banned`），指标 `login/banned` 也随之只记一次（原来 Update 重试时可能记多次）。
- **原子性与并发**：判断与写入在同一次比较后写入里，不另读。并发登录各自基于最新提交的记录判断：
  - 同一个新版本的并发登录：只加一（先提交的看到变化，其余重读之后版本已相同）；
  - **两个不同新版本的并发登录：加二**，存下的版本是后提交的那个（后提交的重读到先提交的版本，仍然不同）。两个版本谁新谁旧 account 不知道（不排序），如果后提交的是渠道那边较旧的值，下一次用较新的值登录会再加一次——后果只是多吊销一次，不会让旧 token 复活；
  - 与手动 `RevokeGameTokens` 并发：加二，两者都不丢；
  - `Login` 存储结果未知后重试：写入已落地的，版本已存，重试不再加；没落地的，重试加一次。都是恰好一次。
- **其他“经渠道确认身份”的入口**：查过，account 里只有 `Login` 调 `Verifier.Verify`（`rg 'Verifier.Verify'`）。`service/platform` 的 `AuthSession` 也问渠道，但它是另一个服务（支付 / 平台会话），有自己的 `platform.Verified`，不读写 account 的账号记录、不签 gt2，所以没改它。portal 的 `/portal/login` 经总线调 `Login`，自动带上。
- **总线**：`Verified` 不上总线（验证器在 account 进程里）；`Account` 经 `Login` 的回答带上 `credential_version` 给内部调用方（portal 不把它转给客户端，查过 `portal.go.tmpl` 的 `loginResponse`）。`account_rpc.go` 的接口注释改了，重新 `go generate` 了 `accounts_rpc_gen.go`（只有注释变化）。
- **account_auth 模板**：
  - `login_dev.go`：开发凭据可以是 `<dev_token>` 或 `<dev_token>#<密码版本>`（例如 `dev-local#2`），后者把版本放进 `CredentialVersion`。切分点由令牌长度定（先按令牌长度取前缀定长比较，再看剩下的是不是空或 `#` 加非空版本），令牌本身含 `#` 也不受影响。`dev-local#`、`dev-loca#1`、`dev-local2#1`、`#1`、`dev-local 1` 都拒绝（加进了原有的拒绝用例，不增加场景数）。
  - `login_channels.go`：`verifyChannel` 的注释加“渠道能给凭证版本就放进 `CredentialVersion`、它只能在凭证真变时变、不要放密码本身”。
  - `login_test.go`：新场景 `TestLoginDevCredentialVersionRevokesGameTokens`（真实 `svcaccount.Service` + 模板的验证器，`loginAccounts` 配了游戏 `party`）。场景数 6 → 7：`module.yaml`、account_auth README、`modules/README.md` 表格、login README 的“account 一侧 7 条”、`codegen/internal/roost` 的 `module_portal_test.go`（两处）与 `module_test.go`（一处）、`scripts/test-modules-generated.sh`（两处）。
- **文档**：account_auth README“装完要自己做的”第 2、4 条（第 4 条改成“渠道返回凭证版本即可自动吊销；这是下一次登录生效，当场吊销用手动接口”）；login README 的 `revoke-game-tokens` 一段、portal README 的“票据不能单张吊销”一条补上“改密码不用另外调，渠道返回凭证版本即可，手动接口仍在”；`game_token.go`（`RevokeGameTokens`“谁让代数加一”）、`account_rpc.go`（`Login` 与 `RevokeGameTokens` 的接口注释）；现行文档 `docs/framework/guide/09-services.md`、`docs/framework/LOBBY.md`、方案 `docs/maintenance/FEATURE-2026-11-16-lobby-relay.md`（原“待维护者定”改成已定的 B）。CHANGELOG、KNOWN-LIMITS 按规则没改，建议文字在末尾。

## 先红后绿

| 文件 | 内容 |
| --- | --- |
| `red-head-compile.txt` | 新用例放到基线实现上：`Verified.CredentialVersion`、`Account.CredentialVersion` 不存在，编译失败 |
| `red-fields-only.txt` | 只加两个字段、`Login` 不处理：四条全红，例如 `local: the first version seen: generation 0, version ""; want 0 and p1`、`after banned logins: {... GameTokenGeneration:0 CredentialVersion:}; want generation 1, version p2 ...`、`4 concurrent logins with one new version: generation 0, version ""; want 1 and p2`（这一份是调整断言顺序之前抓的，当时第一处断言是存储状态；调整后的红落在 token 被拒上，见变异 M1） |
| `green-memory.txt` | 实现之后四条新用例 `-race` 全过（内存） |
| `green-redis-cases.txt` | 真实 Redis 单机（共享隔离环境的 Redis，地址不写进证据）：`GameToken\|CredentialVersion\|CredentialChanges` 12 条 `-race` 全过 |
| `redis-concurrency-x30.txt` | 并发用例在真实 Redis 单机上 `-race -count=30`：ok |
| `redis-negative-control.txt` | 反证：`ROOST_REVIEW_REDIS=127.0.0.1:1` 时四条都在 `dial tcp 127.0.0.1:1` 失败——这几条确实走 Redis |
| `redis-full-package.txt` | 真实 Redis 单机整包 `./service/account/ ./wiring/account/` `-race`：两包 ok，240 条 PASS（含子用例），0 SKIP，0 FAIL |
| `green-cluster-cases.txt` | 私有 3 主 3 从 Redis Cluster（scratchpad 里自起，端口 37100-37105，用完已停、目录已删）：12 条 `-race` 全过；并发用例 `-count=20` ok |
| `cluster-preexisting-failures-on-base.txt` | Cluster 上整包跑有 10 条与本改动无关的失败（建角 / 名字 / slot 一类，`CROSSSLOT Keys in request don't hash to the same slot`）；把 `service/account` 换回基线 `616c5a90` 在同一集群上跑这 10 条，**同样失败**，是预先存在的，见“尚未确认” |
| `red-template-base-dev-verifier.txt` | 模板的红：新场景在、`login_dev.go.tmpl` 换回基线（不认 `#<版本>`），`TestModulePortalInstallsOnALobbyProjectWithoutGame` 生成工程里 `TestLoginDevCredentialVersionRevokesGameTokens` 红在 `login with "dev-local#1": ... the development credential does not match`；放回新模板后绿 |
| `codegen-module-cases.txt` | `codegen/internal/roost` 选跑三条模块用例全过（生成工程装 account_auth / login / portal，`VerifyModules` 断言 account_auth 7、login 33、portal 15、portal+login 1） |

## 变异（`mutations.txt`，逐个改 `service.go`、内存与真实 Redis 各跑一次、还原；全部被杀）

- **M1 撤掉“版本不同就加一”**（`account.GameTokenGeneration++`）→ KILLED。红在承诺本身：`local: a token issued before the credential changed: {AccountID:store:alice-local Game:party ExpiresAtUnix:1700007200}, <nil>; want ErrSessionInvalid`；被封用例红在 `a token from before the credential changed came back after the unban: <nil>`；并发用例 `generation 0 ... want 1`。
- M2 第一次见到版本也加一 → KILLED（`the first version seen: generation 1, version "p1"; want 0 and p1`，并发用例 `generation 2 ... want 1`）。
- M3 被封的登录不记凭证变化 → KILLED（解封后旧 token 复活）。
- M4 渠道没给版本时清掉已存的版本 → KILLED（`the same version / no version: generation 1, version ""; want 0 and p1`：清掉之后再给版本被当成第一次以外的变化）。
- M5 正常登录不调 `withCredentialVersion` → KILLED。

## 验证（本分支，2026-11-18）

| 命令 | 结果 |
| --- | --- |
| `GOWORK=off go build ./...`；`go vet ./service/account/... ./wiring/account/... ./codegen/internal/roost/`；`gofmt -l service wiring codegen` | 通过，无输出 |
| `GOWORK=off go test -race -count=1 ./service/account/... ./wiring/account/... ./wiring/lobby/... ./service/lobby/...` | 全部 ok（内存） |
| 真实 Redis 单机整包、Cluster 选跑 | 见上表 |
| `GOWORK=off go test -count=1 -timeout 60m ./codegen/internal/roost/ -run 'TestModulePortalInstallsOnTheHostedAccountService\|TestModulePortalInstallsOnALobbyProjectWithoutGame\|TestModuleLoginReplacesTheAuthenticatorAndAccountStubs'` | ok（43s） |
| `GOWORK=off go test -count=1 ./codegen/internal/servicerpc/` | ok |
| `GOWORK=off go test -count=1 . -run 'Link\|Anchor\|Doc'` | ok |

没跑：`scripts/test-modules-generated.sh` 全量（计数已改成 7）、`go test ./...`、pretag、TestDemo（按规则由协调者最后统一跑）。

## 尚未确认

- **Cluster 上 `service/account` 有 10 条预先存在的失败**（`TestBeforeWriteFailuresRemainRetryable`、`TestNameCommitLostReplyCannotBurnName`、`TestRoleCreateLostReplyCannotLeaveDuplicateNames`、`TestInitialSlotUnknownCannotPermanentlyBlockCreate`、`TestProfileOutputCannotMutateRoleWithoutCAS`、`TestPendingRoleRecoversAcrossServiceRestart`、`TestConcurrentCreationRetriesShareOneDurablePlan`、`TestNameCommittedElsewhereReleasesThePendingSlot`、`TestNameReservedElsewhereKeepsThePendingSlot`、`TestResolvePendingCreationFreesTheSlotAndRecordsTheNote`），基线上同样失败，错误是 `CROSSSLOT`。没有查是测试夹具（这几条自己建的 Redis 存储前缀没带 hash tag）还是产品代码的 Cluster 问题，也没查 `wiring/scripts/integration/redis-cluster-suites.sh` 上一次全绿是什么时候。与本改动无关，但 `redis-cluster-suites.sh` 现在跑 `./service/account` 会红。
- 并发用例在内存后端上不太能逼出交错（内存存储的 Update 整个在锁里）；真实 Redis 单机 ×30、Cluster ×20 都过，但没有构造“判断放在 Update 之外”的变异来证明并发用例能杀死非原子实现（那要换一种实现形状，不是删一行）。原子性的依据是写法本身：判断与写入在同一个 `versionstore.Update` mutate 里。
- 真实进程演练（portal 登录 → 改凭证版本再登录 → lobby 答 401）没跑：lobby 只拿 account 的校验结果，链路与 C6 的手动吊销相同，靠 account 的用例与模板场景推出。
- 被封账号登录时记下版本变化是本次自己的判断（理由见“做法”），不是维护者原话；如果维护者希望被封的登录什么都不写，撤掉 `service.go` 里被封分支的两行即可（变异 M3 就是那个形状，会让 `TestABannedLoginWithANewCredentialVersionStillRevokes` 红，用例要一起改）。

## 限制（框架本身的，不是遗留）

- 自动吊销在“改密之后的下一次登录”生效：玩家改了密码却一直不再登录，改密之前的 gt2 有效到过期（寿命按游戏，至多 24 小时）。要当场吊销仍须手动调 `RevokeGameTokens` / `accountctl revoke-game-tokens`。
- 凭证版本是不透明的、只比较相等：渠道必须保证它只在凭证真变时才变。每次登录都变的值（会话 token、随机数）会让每次登录都吊销一次；两台设备拿到渠道给的不同版本交替登录也会交替加一。
- 仍是按账号整代吊销、不碰 at1 / et1、不断开已连上 relay 的房间（C6 原有限制不变）。

## 建议写进 CHANGELOG `[Unreleased]` 的文字

- **改密码自动吊销游戏账号 token（交接 C6 第 1 点，维护者 2026-11-18 选 B；账号记录多一个字段，未上线不兼容）**：`svcaccount.Verified` 多 `CredentialVersion`（不透明字符串，至多 128 字节，空 = 渠道不提供，行为不变），账号记录多 `CredentialVersion`。`Login` 在写账号记录的同一次比较后写入里比较：存过且不同就把 `GameTokenGeneration` 加一（与 `RevokeGameTokens` 同一个计数），此前签发的 gt2 全部失效、之后签发的可用；第一次见到只记录，相同或空不动；被封的账号登录照样被拒，但版本变化照样落库。并发登录不会多加或丢加（同一个新版本只加一次，两个不同新版本加两次）。account_auth 模板的开发登录支持 `<dev_token>#<密码版本>` 演示，`verifyChannel` 注释写明接法；场景 6 → 7（只对新安装的模块）。手动的 `RevokeGameTokens` / `accountctl revoke-game-tokens` 仍在，用于当场吊销。证据 `docs/maintenance/evidence/c6-credential-version-20261118/`。

## 建议的 KNOWN-LIMITS 改动

- “portal 的边界”一条里“account 没有改密码路径，凭证变更由渠道 / 账号中心经总线调 `RevokeGameTokens`。”改为：“account 没有改密码路径；渠道验证器在 `Verified.CredentialVersion` 返回凭证版本时，改密之后的下一次登录自动吊销（2026-11-18），要当场吊销由渠道 / 账号中心经总线调 `RevokeGameTokens`。凭证版本只比较相等，渠道必须只在凭证真变时改它。”
- 交接 C6 第 1 点可以结掉；第 2、3 点维护者未改，按现状保留。
- 新增一条遗留（尚未确认）：“`service/account` 在 Redis Cluster 上有 10 条建角 / 名字 / slot 用例报 `CROSSSLOT`（基线 `616c5a90` 同样失败，2026-11-18 在私有 3 主 3 从集群上观察到），`redis-cluster-suites.sh` 跑到这个包会红；是夹具前缀没带 hash tag 还是产品代码问题尚未确认。证据 `docs/maintenance/evidence/c6-credential-version-20261118/cluster-preexisting-failures-on-base.txt`。”
