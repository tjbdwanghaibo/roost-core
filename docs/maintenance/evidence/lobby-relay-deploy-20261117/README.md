# deploy 组证据（2026-11-17）：部署模板、doctor 三项、生成文档、accountctl -service、account.services 顺序

方案 `docs/maintenance/FEATURE-2026-11-16-lobby-relay.md` §4.5、§4.7.1；补 ws 组（67a2c9e5）与 gen 组（67bdf4a8）报告里没做的部分。
所有命令在干净的 worktree 上跑：HEAD 89c42f67 加本组改动（`git diff` 与主检出逐字节相同），不含别组未提交的文件。

## 做了什么

- **部署模板**（`codegen/internal/roost/lobby_relay.go` 的“部署模板”一节、`render_deploy.go`、`render_cicd.go`）：
  - 客户端端口 `serviceClientPorts`：lobby `lobby-http`（8080+N）、relay `relay-tcp`（7100+N）与 `relay-ws`（7200+N），端口与生成配置同一处派生；声明了 websocket 时 Gate（或嵌入 TCP 的玩家服务）`player-ws` 7080。
  - k8s：容器端口、Service 端口；NetworkPolicy 对带 `player-access` 标签的命名空间放行这些端口（与原来的 7000 同一条规则）。
  - 生产 compose：这些服务加 `ports`；docker README 的示例加 `-p`。
  - relay 令牌密钥：k8s 的 lobby / relay 容器 `env` 用 `secretKeyRef` 引用 Secret `<app>-relay-token`（新的 `deploy/k8s/base/secret.relay-token.example.yaml`，只生成一次）；compose 用 `${<变量>:?…}` 环境变量，`env.example` 加一行；shell `install.sh` 要求安装者环境里有它（不足 16 字节拒绝），写进 release 的 `relay-token.env`（umask 077），unit 加 `EnvironmentFile=-$APP_ROOT/current/relay-token.env`。
  - 变量名取工程配置里 `token_key_env` 的实际值（`Manifest.withRelayTokenKeyEnv`，与 `withConfiguredShutdown` 同样在渲染前读配置）；改名后 `roost project sync` 跟着改。`deploy/dev/run.sh` 的开发密钥也用它，并在注释里列出客户端端口。
  - 三份 README（k8s / docker / shell）加一段客户端端口与密钥的说明。
- **doctor**：
  - `lobby-relay:account-games`：每个 lobby 的 `lobby.games` 都在 `account.games` 里（开发配置对开发配置、生产示例对生产示例），漏了 FAIL。
  - WebSocket 地址参与端口冲突检查：`player_access.{tcp,gate}.websocket.addr` 带 `listen` 标签（嵌入 TCP 跟着 `player_access.tcp.enabled`，Gate 总是）；`listenAddrProblems` 跳过取值为空的监听键（没写、也没有缺省值），所以“不写就不开”的地址不参与。
  - 没有声明 websocket 的工程写了 `player_access.{tcp,gate}.websocket.*`：`roost config check` 与 doctor 的 `config-schema:<服务>` 报失败，点名键并给出 `roost add transport websocket`（`webSocketKeysWithoutTransport`）。原来只有 doctor 的 WARN“another service's keys”，说法会误导，config check 不报。
- **生成工程的文档**：声明了 websocket 时，`docs/ROOST_YAML.zh-CN.md` 的“当前支持 tcp”改成 tcp 与 websocket，`docs/PLAYER_ACCESS_TCP.zh-CN.md` 末尾加“WebSocket 接入”一节；没声明的工程原样。`docs/SERVICES.zh-CN.md` 的 lobby / relay 一节改写部署说明、加 account-games 一条。`roost help` 的 transport、lobby 两段同步。
- **accountctl**（`modules/login/files/cmd/accountctl/main.go.tmpl`，game-demo 的 `cmd/accountctl` 由它渲染）：`upsert-server -service NAME`。accountctl 不读 account 配置，`-service` 经 `recordService` 交给 `openService`，作为唯一的 game 服务配进 account，由 account 检查写法并写进记录。记录已有服务名而这次没带 `-service` 时，错误点名该带的 `-service`。
- **account.services 顺序**：`ensureYAMLMapEntries` 把新条目按名字排进已有条目之间（第一个名字更大的条目之前，带头注释时在注释之前；没有就接在最后一个条目之后）。先 add relay 再 add lobby，结果仍是 game、lobby、relay（修前是 relay、game、lobby）。
- **现行文档**：`docs/framework/GATEWAY.md` §6 写明部署模板的 7080、doctor 的两项；`codegen/docs/PROJECT_GENERATOR.zh-CN.md` 的 transports 一行。

## 证据文件

| 文件 | 内容 |
| --- | --- |
| `tests-and-mutations.txt` | 新增与改动的用例（`-v`）、相关已有用例（k8s / compose / shell / dev-run / config 契约 / help / gate / endpoints / 生成契约，含 `TestKitConfigSchemasMatchKitDeclarations`）、vet、15 处 codegen 变异 |
| `mutate.py.txt` | 变异脚本：每处替换一行关键代码，跑对应用例，必须变红，恢复后全部变绿 |
| `byte-identical.txt` | 没有 websocket / lobby / relay 的四种工程：HEAD 生成器与本组生成器整目录对比 |
| `acceptance.txt`、`accept.sh.txt` | 两个生成工程的 build、vet、`generate --check`、doctor、部署模板摘录；accountctl 单测与 3 处变异 |

## 命令与结果

```
GOWORK=off go test -count=1 -v -run '<新增与改动的 16 个用例>' ./codegen/internal/roost/    # 全部 PASS
GOWORK=off go test -count=1 -run '^(<相关文件的全部用例>)$' ./codegen/internal/roost/          # ok（169s）
GOWORK=off go vet ./codegen/internal/roost/                                                  # 通过
python3 mutate.py                                                                            # 15/15 变红，恢复后变绿
```

新用例：`TestDoctorLobbyRelayAccountGames`（三种漏法 + 生成配置 OK）、`TestAddLobbyRelayKeepsAccountServicesInNameOrder`、`TestEnsureYAMLMapEntriesInsertsInNameOrder`、`TestLobbyRelayDeploymentPublishesClientPortsAndTheTokenKey`（含改名后 sync）、`TestPlayerWebSocketPortIsDeployed`（Gate 与嵌入 TCP；不声明时没有 7080 与 player-ws，ROOST_YAML 原样）、`TestWebSocketAddressesTakePartInThePortCheck`（Gate 冲突 / 没写 / 空串；嵌入 TCP 开 / 关；“不写就不开”的 relay 写法）、`TestWebSocketKeysWithoutTheTransportAreRefused`（config check 与 doctor 两条路径，声明了时不报）。accountctl：`TestLoginAccountctlWritesTheServerService`（生成工程里 `go test ./cmd/accountctl/` ok）。

| 项 | 结果 |
| --- | --- |
| 逐字节相同 | plain、`-template game`、`-template game -framework all` 三个工程整目录相同（共 1708 个文件）。game-demo 只有 `cmd/accountctl/main.go`、`main_test.go`（第 4 项要改的就是它）与 `roost.yaml` 里 login 模块的摘要不同，其余相同 |
| `-template lobby` | build、vet、`generate --check` 通过；doctor 0 个 FAIL / WARN（account 协作者按 gen 组验收的做法拷入登录模块的实现） |
| game + login portal + websocket + lobby + relay + gate | build、vet、`generate --check` 通过；doctor 0 个 FAIL / WARN；`account.services` 为 game、lobby、relay |
| doctor 新检查的变异 | M1、M2（account-games）、M3、M4（WebSocket 端口冲突）、M5（没声明 websocket 的键）各自变红 |
| accountctl 变异 | A1'、A2、A3 变红（A1 第一次的写法编译不过，不算，改写后重跑）|

## 拿不准、留给协调者的点

1. **玩家 WebSocket 的端口是约定的 7080，生成器不写 `websocket.addr`**。ws 组的现行契约是“命令不改配置文件，写上 addr 才开”（GATEWAY.md §6），我没有改它。部署模板因此发布一个约定的端口，开 WebSocket 时要写 `addr: 0.0.0.0:7080`；没写时 Service 端口后面没有监听，无害。另一种做法是 `add transport websocket` 直接写 `addr`（开发 `localhost:*` 来源、生产 `CHANGE_ME`），要处理 add gate 时把这段搬到 Gate 配置、second-game.sh 改端口，改动更大，需要维护者决定。
2. **relay.listen / relay.ws_listen / lobby.listen 还没有 `listen` 标签**，端口冲突检查目前看不到它们。标签要加在 `wiring/relay/relay_mod.go`、`wiring/lobby/lobby_mod.go` 上（host 组 / lobby 组的文件，我没碰），再重新生成 `kitconfig_gen.go`：`relay.listen`、`lobby.listen` 写 `listen:"always"`，`relay.ws_listen` 也写 `listen:"always"`。不写时空串跳过的处理已经在 `listenAddrProblems` 里（`TestWebSocketAddressesTakePartInThePortCheck` 用同样写法的声明验过），加标签之后 codegen 不用再改。lobby 与 relay 的端口按端口块派生，生成的配置不会互撞。
3. **生产 compose 原来就不发布玩家 TCP 7000**（只有 docker README 的 `docker run -p`）。为了逐字节不变，我没有给 7000 加 `ports`；声明了 websocket 的工程里 Gate 的 compose 只发布 7080。要不要统一，请维护者定。
4. **k8s Secret 的数据键就是环境变量名**。改 `token_key_env` 之后 Deployment 的 `secretKeyRef` 跟着改，但 `secret.relay-token.example.yaml` 只生成一次（同其他 Secret 示例），已经 apply 的 Secret 要自己改键名。
5. **relay 的每个实例要能被客户端直连**（advertise_addr 各不相同）。模板给的是普通 ClusterIP Service 加端口，对外暴露（每实例 LoadBalancer / NodePort / hostPort）留给部署环境，README 里写了这一句。
6. **accountctl 不读 account.services**：`-service` 的名字交给 account 时被当作唯一的 game 服务，只检查写法、不核对它确实是配置里的 game 服务；写错时那个服务的区服清单里没有它，用 show-server 核对。记录写了服务名之后，`run.sh` 的 `upsert-server -sid $SID`（不带 `-service`）会被拒并点名 `-service`；单一 game 服务的工程不写 `-service`，不受影响。

## 建议给 CHANGELOG 的文字（由协调者统一写）

- **生成器：lobby / relay / WebSocket 的部署模板**：
  - 托管 lobby、relay 的工程在 k8s（容器与 Service 端口、NetworkPolicy）与生产 compose（`ports`）里发布 lobby 的 HTTP 端口、relay 的 TCP 与 WebSocket 端口。
  - relay 令牌密钥注入 lobby 与 relay：k8s 引用新的 Secret `<app>-relay-token`（`secret.relay-token.example.yaml`），compose 用环境变量，shell 的 `install.sh` 要求安装者环境里有它，并写进 release 的 `relay-token.env`。变量名取配置里 `token_key_env` 的实际值。
  - 声明了 websocket 的工程按约定的 7080 发布玩家 WebSocket 端口（`player-ws`），配置里写 `websocket.addr: 0.0.0.0:7080` 才开。
  - 没有 websocket、lobby、relay 的工程生成结果不变。
- **doctor**：
  - 新增 `lobby-relay:account-games`：`lobby.games` 的游戏必须在 `account.games` 里；
  - WebSocket 地址参与端口冲突检查，没写地址的监听键不参与；
  - 没有声明 websocket 的工程写了 websocket 的键，`config check` 与 doctor 报失败。
- **生成工程文档**：声明了 websocket 时 ROOST_YAML 写“tcp 与 websocket”，PLAYER_ACCESS_TCP 加“WebSocket 接入”一节。
- **accountctl**（login 模块，只对新安装的工程）：`upsert-server -service <game 服务>`。
- **roost add lobby / relay**：补 `account.services`、`account.games` 时新条目按名字排进已有条目之间，不再插到最前面。

## 建议给 KNOWN-LIMITS 的文字

- 框架限制：
  - 玩家 WebSocket 的部署端口是约定的 7080，生成器不写 `websocket.addr`（写上才开）；
  - relay 的每个实例要能被客户端直连，模板只给 ClusterIP Service，对外暴露按部署环境配；
  - accountctl 不读 `account.services`，`-service` 的名字由运维保证与配置一致。
- 遗留问题：
  - `relay.listen`、`relay.ws_listen`、`lobby.listen` 没有 `listen` 标签，不在端口冲突检查里（见上面第 2 条，加标签后自动生效）；
  - 生产 compose 不发布玩家 TCP 7000（原有行为，见上面第 3 条）。
- 删掉 gen 组建议里的两条：“部署模板不发布 lobby / relay 的客户端端口、不注入 `ROOST_RELAY_TOKEN_KEY`”、“doctor 不检查 lobby.games 都在 account.games 里”；以及 ws 组建议里的“生成的部署模板不发布 WebSocket 端口”。
