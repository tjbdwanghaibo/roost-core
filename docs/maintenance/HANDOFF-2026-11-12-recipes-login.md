# 交接：两个配方接入登录链路（2026-11-12）

维护者 2026-11-12：“把 2 拆出去，我让另一个 agent 去处理”。“2”指登录线收尾里的一步：配方 `casual`、`turn-pvp` 接入模块 login / portal / servertime。本文是给接手 agent 的任务书。

## 0. 从哪里开始

- **分支**：远端 `feat/business-modules-recipes-login`（本机 worktree `/Users/whb/roost/worktrees/recipes-login`，本地分支 `tmp/recipes-login`）。它在登录线分支的 `fc053259` 之上多出：一份**未经复核**的配方改动（见 §2）和本文，并已并入登录线后来的提交（到 `b94c99d5`）。
- **不要在别的 worktree 里改**：登录线的原会话在 `/Users/whb/roost/worktrees/mod-portal`（`tmp/mod-portal`，远端 `feat/business-modules-portal`）继续做 game-demo 的接入；主线 `feat/business-modules` 是另一条线。两条都不要动。
- 做完推到 `feat/business-modules-recipes-login`，**不要合进任何别的分支**；由原会话在统一跑完整测试之前把它并进登录线分支。
- `GOWORK=off`。模块与配方的模板是编进 CLI 的：每次改模板后重新 `go build -o <scratch>/roost ./codegen/cmd/roost` 再生成工程验。

## 1. 背景：登录链路的最终形态（已做完，不要重做）

先读 `modules/login/README.md`、`modules/portal/README.md`、`modules/enter/README.md`、`modules/servertime/README.md`。要点：

- `POST /portal/login`（渠道 + openid + 凭据 → 账号票据）→ `POST /portal/servers`（凭票：清单与推荐）→ `POST /portal/enter`（凭票 + 服号 → 进服票据与地址）。三个端点挂在托管 account 服务的进程上。
- 客户端用 `enter:<进服票据>` 握手，login 模块的认证器交给 account 的 `EnterServer`（没有映射就新建，只有账号 id 与玩家号，没有名字）。认证器同时仍认 `session:<player_id>:<token>`（CreateRole / SelectRole 的老路）。
- 会话激活后 game 推 `EnterStatus{code, reason, need_create}`。**用进服票据握手的会话必须先 `EnterCreate{name}` 再 `EnterGame`**（login 与 enter 的联动登记“先新建”）；名字规则在 game（装了 profile 时走 enter 与 profile 的联动）。会话令牌握手的会话不受影响，`EnterGame` 仍是“加载或创建”。
- 认证器只记不拒：承载服不是本服由进入前检查答 `login_elsewhere`（login 与 enter 的联动），握手不再因此失败。

## 2. 现状

配方原来的样子（`fc053259` 上）：

- `modules/recipes/catalog.yaml`：casual = enter, wallet, checkin, task, stamina, shop, bag, mailbox；turn-pvp = enter, profile, room, turn, leaderboard。
- `modules/recipes/common/internal/access/player/tcp/auth.go.tmpl`（及 `auth_test.go.tmpl`）是配方自带的认证器：包一层 `wiring/gate.AccountAuthenticator`，只按会话令牌写，承载服不是本服或换服在途时**握手就拒绝**。没有客户端够得着的登录入口。

分支上的半成品提交（上一个 agent 做到一半被停下，**没有生成过工程、没有跑过任何测试**）已经动了这些：

| 文件 | 改了什么 |
| --- | --- |
| `modules/recipes/catalog.yaml` | 两个配方末尾加 `login, portal, servertime`，摘要加“登录入口” |
| `common/.../auth.go.tmpl`、`auth_test.go.tmpl` | 删除（改用 login 模块的认证器） |
| `common/.../recipe_entering.go.tmpl` | 新增一条进入前检查 `recipe_transfer`：换服在途的角色答 `login_elsewhere`（配方不装 rehome，login 模块不拒这一种） |
| `common/.../network_test.go.tmpl` | 大改（约 +350 行），往新链路上改 |
| `casual/.../casual_network_test.go.tmpl`、`turn-pvp/.../turn_network_test.go.tmpl` | 小改 |
| 两份 `RECIPE.md.tmpl` | 改了登录一节 |
| `codegen/internal/roost/recipe_test.go` | 期望值改了一部分 |

**更新（同日稍晚）**：上一个 agent 的结束报告说，这份改动在它那边其实已经做完——两个配方工程 build / vet / `go test ./...` / `roost module verify` 全过，`go test -run TestRecipe ./codegen/internal/roost` 通过（164s）；它交出的补丁与这个提交内容相同。但那些结果**没有被复核过**，变异自检、§3 第 4 项（开发配置开箱可用）与第 7 项（文档）没有做。所以接手的第一步是照 §5 重新验证一遍，对得上就在它上面补完剩下的项，对不上再逐个文件判断。本分支已并入登录线的 `b94c99d5`（game-demo 接入完成）。

它留下的三条提示：

1. login 模块的 `Admit` 只拦承载服不符，不拦“换服在途”；所以才有 `recipe_entering.go.tmpl` 那条配方自己的进入前检查。
2. login 模块的 `login_test.go` 用进程级通道收会话激活后的告知；配方的网络测试结束时要清空它，否则模块自己的用例会读到脏数据。
3. 配方网络测试里的 `recipeRequest` 要跳过不带关联号的推送（进入时 servertime 推的 `ServertimeNow`、`EnterStatus`）。

## 3. 要做成什么样

1. 两个配方都带 login、portal、servertime。新建 `-recipe casual` / `-recipe turn-pvp` 的工程不做任何配置就能 `go build ./...`、`go vet ./...`、`go test ./...`、`roost module verify` 全过，重复 `roost module add` 没有变化。
2. 配方不再自带认证器。原认证器多做的两件事要有着落并有测试：
   - 承载服不是本服：由 login 与 enter 的联动在进入时带码拒绝（确认，不用再写）。
   - 换服在途：配方不装 rehome，要么保留半成品里的 `recipe_transfer` 检查，要么给出更好的做法；不能悄悄放行。
   - 行为差异（握手拒绝 → 握手成功、进入时带码拒绝）写进两份 `RECIPE.md.tmpl`。
3. 配方的网络测试走新链路（真实回环 TCP）：进服票据握手（真实 account 内存存储 + login 认证器）→ `EnterStatus need_create=true` → 不新建直接 `EnterGame` 被拒且零写入 → `EnterCreate` → `EnterGame` → 后面的玩法请求；重连不再要求新建。
   - casual 的首次发放（`casual/game/entities/player/recipe_first_enter.go.tmpl`）在“先新建再进入”下仍只发一次，要有断言（新建之后第一次 `EnterGame` 发、重连不发、`EnterCreate` 本身不发或只算一次——以代码的实际语义为准并写明）。
   - turn-pvp 装了 profile：`EnterCreate{name}` 的名字进档案，空名字被拒，要有断言。
   - 会话令牌握手的老路至少留一条用例，证明它仍是“加载或创建”。
4. 开发配置开箱可用：新建的配方工程 `make dev-run` 之后能用 curl 走完三步拿到进服票据（需要的 `login.dev_token`、`portal.servers` 怎么给，照 login / portal 模块安装后打印的 `todo:` 行与 README；配方能代办的就在配方里写好，只写进开发配置）。做不到开箱可用就在 `RECIPE.md.tmpl` 里把要手工做的步骤列全。
5. 两份 `RECIPE.md.tmpl` 的登录一节改成新链路。
6. 生成器里与配方有关的期望同步：`codegen/internal/roost/recipe_test.go`（`TestRecipe*`），以及 `scripts/test-modules-generated.sh` 里若有配方的模块数或场景数。
7. 文档：`modules/README.md`、`docs/framework/CAPABILITIES.md` 里说到配方模块列表的地方；`docs/maintenance/KNOWN-LIMITS.md`（配方的边界）；`CHANGELOG.md` 的 `[Unreleased]` 加一条，写清跑了什么、没跑什么。

## 4. 不要碰的

- `docs/agent-skills/roost-genre/references/compose.md`：整篇重写是原会话最后一步，配方模块列表由那一步同步。
- `demo/**`、`codegen/internal/roost/demo*.go`、`modules/login/**`、`modules/portal/**`、`modules/enter/**`、`modules/servertime/**`、`service/account/**`、`wiring/**`：原会话在改或刚改完。发现它们有缺陷、配方必须靠改它们才能做通时，**不要自己改**：在报告里写清现象与最小复现，其余照做。
- gateway / Nest / DataEngine / Sync 不为这件事改动。
- `scripts/test-gate-client.sh` / `scripts/test-gate-real.sh`（`TestGateGeneratedBusinessConsumer`）在模块化之后就是坏的，已记录，不在这件事里修。

## 5. 验证节奏

维护者：“测试最终再跑”。这里只跑定向的：

- 新建两个配方工程各一个：`<roost> project new <name> -module <mod> -out <dir> -recipe casual --skip-deps`，再 `go mod edit -replace github.com/tjbdwanghaibo/roost-core=<本 worktree>`；build / vet / `go test ./...` / `roost module verify` / 重复安装无变化。
- `go test -count=1 -run 'TestRecipe' ./codegen/internal/roost`，以及改到的生成器单测（按名字跑）。
- 根包门禁 `go test -count=1 .`、`python3 scripts/check-doc-anchors.py`。
- **不要跑** `scripts/test-modules-generated.sh`、完整的 `TestDemo|TestModule`、全仓 `go test ./...`；没跑的照实写进 CHANGELOG 与报告。
- 新规则先红后绿；“先新建”“换服在途不放行”“首次发放只发一次”定稿后各做一轮变异自检（故意改坏、确认有用例变红），一次只起一个变异进程，存活的补断言。

## 6. 交付

提交到本分支（按逻辑分几次，说明里写清验证范围），推到 `feat/business-modules-recipes-login`。报告给维护者：每一项做了什么、落在哪些文件；半成品里保留了什么、重写了什么；跑了哪些检查及结果；没跑的；变异自检的条数与存活项；需要原会话在别的文件里配合改的地方。
