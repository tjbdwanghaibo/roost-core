# gen 组证据（2026-11-17）：生成器与 roost.yaml（方案 §4.5）

方案见 `docs/maintenance/FEATURE-2026-11-16-lobby-relay.md` §4.5。所有命令在干净树上跑：HEAD 37bf7c2e 加 gen 组的改动，没有别组未提交的文件。

## 做了什么

- **框架服务目录**：`frameworkCatalog` 加 `lobby`、`relay` 两项，标 `Direct`（直接面向客户端，不在总线上应答）。
  - 业务服务不能 `uses` 它们。
  - `-framework all`、game-demo、模块安装器只认总线上的九个（`frameworkServiceNames`）。
  - 模块的 `runtime.framework` 不能指向它们。
  - bootstrap 用 wiring 包的 `ServiceName` 注册：
    - lobby：redis、nats、etcd、`account.NewClientMod()`、`lobby.NewMod(serviceLobby.Tokens())`。`Tokens()` 在 `services/<lobby>/collaborators.go`，缺省返回 nil，即经 account 校验。
    - relay：nats、etcd、`relay.NewMod()`。relay 没有协作者文件。
  - 新字段 `Clients`：托管进程自己装的框架 ClientMod。lobby 用 `[account]`，Validate 要求同一工程托管了 account。
- **命令**：
  - `roost add lobby [name]`、`roost add relay [name]`：声明托管服务。工程没有托管 account 时一并声明 account。
  - 执行 sync 之后再补 account 的配置（开发配置、生产示例、k8s Secret 示例）：
    - `account.services`：工程里的每个 lobby、relay 与玩家服务，kind 分别是 lobby、relay、game；
    - `account.games`：lobby.games 里的游戏，`token_ttl: 2h`。
  - 已有的另一侧改过 `token_key_env` 时，新服务跟着它。
  - 同名同种再加一次时什么都不改；名字被业务服务占了时拒绝。
- **不带 game 的工程**：`roost project new <name> -template lobby` 只托管 account、lobby、relay。
  - 现状调查：只有托管服务的 `roost.yaml` 本来就能生成、编译（Validate 没有“必须有玩家服务”这一条）。缺的是一条能直接生成这种工程的命令，所以加了这个模板。
  - 有 game 的工程不受影响，见下面的逐字节比对。
- **生成的配置**：
  - 起始段来自 `kitConfigSchemas`。`configschemagen` 加了 `lobby`（lobby Mod 与 account ClientMod 的声明）和 `relay` 两个单元，`kitconfig_gen.go` 由它重新生成，没有手改。
  - 端口按端口块派生：
    - `lobby.listen`：`0.0.0.0:(8080+N)`；
    - `relay.listen`：`0.0.0.0:(7100+N)`，`advertise_addr`：`127.0.0.1:(7100+N)`，生产示例里是 `CHANGE_ME`；
    - WebSocket：`7200+N`，`ws_allowed_origins` 写 `localhost:*`。
  - `relay.instance` 不写，按 `<server_type>-<sid>` 取，多个 sid 不会撞名。
  - `lobby.games` 写一个以工程名命名的游戏（`max_members: 8`）。
  - 两边的 `token_key_env` 都是 `ROOST_RELAY_TOKEN_KEY`；subject 与 registry 前缀按工程名派生，两边相同。
  - `deploy/dev/run.sh` 在这个变量没设时给一个开发密钥。
- **doctor**（`lobby-relay:*`，只在工程托管了 lobby 或 relay 时出现）：
  - `account-services`：account 的开发配置与生产示例里，`account.services` 列全了 lobby、relay、玩家服务，kind 对得上；
  - `token-key-env`：全部 lobby、relay 配置里的 `token_key_env` 非空且相同；
  - `advertise-addr`：relay 生产示例的 `advertise_addr` 不是回环地址（localhost、127/8、::1）。
- **文字**：
  - `roost help services` 改为“总线上的九个 + lobby、relay”；
  - 新增 `roost help lobby`；
  - `project new` 的帮助加 `-template lobby`；
  - 生成工程的 `docs/SERVICES.zh-CN.md` 加“大厅 lobby 与转发 relay”一节；
  - `docs/framework/README.md` 的索引链接到 LOBBY.md、RELAY.md；
  - `wiring/config_schema_test.go` 的 `everyKitMod` 加了 `new(lobby.Mod)`、`relay.NewMod()`。

## 验收

| 项 | 结果 | 文件 |
|---|---|---|
| account + lobby + relay（`-template lobby`，不带 game） | build、`generate --check`、`id check` 通过；doctor 没有 FAIL（account 协作者按登录模块的写法实现之后，见下） | `acceptance.txt` |
| game（`-template game` + `module add login portal`）加 `add lobby`、`add relay` | build、`generate --check`、`id check` 通过；doctor 没有 FAIL | `acceptance.txt` |
| 没加 lobby / relay 的工程与 HEAD 的生成器输出逐字节相同 | plain、`-template game`、`-template game -framework all`、game-demo 四个工程整目录相同（1708 个文件）；`game + module add login portal` 也相同 | `byte-identical.txt` |
| 生成的配置能直接起服 | `-template lobby`：account、lobby、relay 三个进程用 `run.sh` 起来，全部 ready；lobby 不带 token 答 401，伪造的 gt1 经 account RPC 被拒（401）；relay 在 etcd 广播了 tcp 与 ws 地址。game 工程里的 lobby、relay 也起来、ready | `devrun-*.txt` |
| doctor 三项各有正反用例 | `TestDoctorLobbyRelayAccountServices`（漏 relay、kind 写错）、`TestDoctorLobbyRelayTokenKeyEnv`（变量不同、为空）、`TestDoctorLobbyRelayAdvertiseAddr`（127.0.0.1、127.0.0.2、localhost、[::1]）；生成的配置是 OK | — |
| 变异 14 处 | 全部变红，恢复后全绿 | `mutations.txt`、`mutate.py.txt` |
| codegen 相关单测（`-run` 挑选） | ok（342s） | `codegen-targeted-tests.txt` |
| `GOWORK=off go test -run 'Link\|Anchor\|Doc' .` | 只有 `TestObservabilityDocNamesExistingMetrics` 红，原因是 ws 组 67a2c9e5 新增的指标 `player_tcp_websocket_rejected_total` 没写进 OBSERVABILITY.md；在不含本组改动的 HEAD 上同样红。其余通过 | `root-doc-wiring-tests.txt` |

**不带 game 的工程与 account 协作者**：`-template lobby` 生成的 `services/account/collaborators.go` 是缺省全部拒绝的桩（与任何托管 account 的工程相同），doctor 的 `collaborators:account` 报 FAIL，直到工程实现它为止。登录模块要求有 game，所以这种工程装不上。验收脚本把登录模块的四个 account 文件拷进来，模拟工程自己实现，之后 doctor 没有 FAIL。

## 建议给 CHANGELOG 的文字（由协调者统一写）

- **生成器：大厅 lobby 与转发 relay（方案 FEATURE-2026-11-16-lobby-relay §4.5）**：
  - `frameworkCatalog` 加 lobby、relay（直接面向客户端，不能 uses）。
  - 新命令 `roost add lobby` / `roost add relay`：声明托管服务（没有托管 account 时一并声明），补上 `account.services`、`account.games`。
  - `roost project new -template lobby`：只托管 account、lobby、relay，不带 game。
  - 生成配置的端口按端口块派生：lobby 8080+N，relay 7100+N，WebSocket 7200+N；`token_key_env` 两边都是 `ROOST_RELAY_TOKEN_KEY`，`make dev-run` 在它没设时给开发密钥。
  - doctor 加 `lobby-relay:account-services`、`lobby-relay:token-key-env`、`lobby-relay:advertise-addr` 三项。
  - 没有 lobby / relay 的工程，生成结果不变。

## 建议给 KNOWN-LIMITS 的文字

框架限制：
- 生成的部署模板（k8s、compose、shell）不发布 lobby / relay 的客户端端口，也不注入 `ROOST_RELAY_TOKEN_KEY`。部署时按自己的负载均衡与 Secret 配置。
- `-template lobby` 的 account 协作者需要工程自己实现：登录模块要求有 game 服务，装不上。

遗留问题：
- doctor 不检查 `lobby.games` 的每个游戏都在 `account.games` 里。add 命令会写全，手改之后漏掉的话，lobby 对这个游戏的每个请求都答 401。
