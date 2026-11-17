# RV-20261116-02 修复证据：Gate 身份按实际服务名核对（2026-11-17）

问题报告：分支 `fix/core-game-server-review` 的 `docs/maintenance/MAIN-GATE-REVIEW-20261116.md`（证据 `docs/maintenance/evidence/gate-review-20261116/`）。修前基线 main `d1a6ab2a`（v1.52.0 之后，单实例锁存 etcd）。本机 macOS arm64、Go 1.27.3、本机集成环境的 NATS 2.14.5（`~/.roost-it`，`ROOST_DATAENGINE_IT_NATS_URL`）；演练的 etcd 是脚本自起的私有实例。

## 根因

App 按实际服务名持单实例锁 `<key_prefix>:<服务名>:<sid>`（`framework/app/singleton.go` 的 `singletonKey`）。gateway 在握手 / 续约 / 绑定时按协议角色 `gate` / `game` 调 `Matches`（`infra/network/gateway/gate.go` 的 BeforeACK 与 Renew、`ingress.go` 的 Bind），wiring/gate 把角色原样交给 `SingletonIdentityChecker.Matches`；发现候选也按角色名登记（`ServiceType: "gate"/"game"`），Gate 按 `"game"` 查。服务名不是 gate / game 时（`roost add gate edge`、玩家服务 `-services world`），身份查询落在没人持有的键上 → `ErrBindingStale`，ACK 不发，客户端 EOF。

## 改法

- `wiring/gate.Options` 新增 `GateService` / `GameService`（空串取 gate / game，现有接线行为不变）。`connections.matches` 把角色换成实际服务名再核对，incarnation 照常比较；`register` 按本端实际服务名登记；GateMod 的缺省 `FixedGameResolver` 按 GameService 查。NATS 主题与收件箱里的角色不变，`infra/network/gateway` 未改。
- `connect` 校验本端：App 的锁键（`SingletonIncarnation.Key`）必须以 `:<本端服务名>:<sid>` 结尾，否则 Provide 拒绝启动（把“监听成功、每次握手被拒”变成启动错误）。服务名带冒号 / 空白或两端同名也拒绝。
- 生成器：Gate 服务的 `NewGateMod`（server_gen.go 的 `gateOptions`）与玩家服务的 `gateIngressOptions`（bootstrap）都写 `options.GateService = "<Gate 服务>"`、`options.GameService = "<玩家服务>"`。

## 先红后绿

| 用例 | 修前 | 修后 | 变异 |
| --- | --- | --- | --- |
| `wiring/gate` `TestRealGateHandshakeUsesActualServiceNames`（integration 标签，真实 NATS + 真实 TCP 握手与一次转发；正式 GateMod + GameIngressMod；身份替身按 `<服务名>:<sid>` 持锁，发现替身按 ServiceType 登记 / 查找） | gate/game PASS；edge/game、gate/world、edge/world FAIL：`singleton checks=[gate:11=false]` / `[gate:11=true game:22=false]`，`ACK=<nil> err=EOF`，发现登记在 gate/game 名下（`red-wiring.log`） | 四组 PASS，身份查询都是实际服务名（`edge:11=true world:22=true`），Gate 按实际名查发现（`green-wiring.log`） | 变异 1：`matches` 去掉角色→服务名映射，三组自定义名重新 EOF（`mutation-wiring.log`） |
| `TestGateRefusesAServiceNameThatIsNotItsSingletonLock` | FAIL：锁键 `edge:11` 的 Gate 以 gate 名启动成功 | PASS，Provide 报“holds singleton lock … but is wired as gate service "gate"” | — |
| `codegen` `TestGeneratedGateWiringCarriesActualServiceNames`（四组名字，直接渲染两端接线）与 `TestAddGateWithACustomNameWiresItsServiceName`（正式 CLI：module add login enter portal、`roost add gate edge`） | 全部 FAIL：生成的接线不带服务名（`red-codegen.log`，用修前的 gate.go / render_player_tcp.go 跑新用例） | PASS（`green-codegen.log`，含既有 TestAddGate* / TestDoctorPlayerGate* / TestPlayerGateManifestRules） | 变异 2：Gate 端 `gateOptions` 不写名字，两个用例都红（`mutation-codegen.log`） |
| 生成工程真实进程演练 `scripts/testdata/gate-drill/gate-drill.sh`（account、经 Game Ingress 的玩家服务、Gate 三个进程，真实 NATS / Redis / Mongo 与私有 etcd 单实例锁，经登录入口拿票据、经 Gate 握手并新建 / 进入） | `DRILL_GATE_NAME=edge`：doctor 五项 gate:* 全 OK，握手 `panic: EOF`（`drill-edge-prefix-red.log`，修前代码的临时检出 + 本次的演练脚本） | 缺省 / `edge` / `DRILL_PLAYER_SERVICE=world` / edge+world 四组 PASS，三个进程 0 条 ERROR（`drill-gate.log`、`drill-edge.log`、`drill-world.log`、`drill-edge-world.log`） | — |
| `scripts/test-gate-generated.sh`（离线：build、vet、doctor、generate --check、重复 add / sync 不改工程） | — | gate 与 edge 两遍 PASS，并检查两端接线写的实际服务名（`gate-generated.log`） | — |

命令：

```bash
source ~/.roost-it/roost-dataengine-it/env.sh
go test -tags integration -count=1 -run 'TestRealGateHandshakeUsesActualServiceNames|TestGateRefusesAServiceNameThatIsNotItsSingletonLock' -v ./wiring/gate/
go test -count=1 -run 'TestGeneratedGateWiringCarriesActualServiceNames|TestAddGateWithACustomNameWiresItsServiceName' -v ./codegen/internal/roost/
DRILL_GATE_NAME=edge DRILL_PLAYER_SERVICE=world DEMO_LOGIN_OUT=<目录> bash scripts/testdata/gate-drill/gate-drill.sh
bash scripts/test-gate-generated.sh
```

## 边界

- 网络回归的身份权威是替身（按 App 锁键语义建键），不是真实 etcd；真实 App + etcd 单实例锁由演练覆盖，单机、单个 Gate 进程、一次登录，不是跨机或故障重启验收。
- 对端名字写错（两端 Options 不是同一对）只能在握手时发现；生成工程由生成器写同一对名字。`system` 广播发起方仍按 `system` 服务名核对，没有映射。
- `TestGateGeneratedBusinessConsumer`（game-demo 模板）本轮没跑通：demo 组同一检出里正在迁移组件，模板缺文件，与本修复无关；它的测试接线 Key 为空，不触发新加的锁键校验。
