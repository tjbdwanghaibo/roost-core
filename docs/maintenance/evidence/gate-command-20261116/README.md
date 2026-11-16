# roost add gate 验收（2026-11-16）

`roost add gate`（`codegen/internal/roost/gate.go`）把 `-template game` + `roost module add login enter portal` 的工程改成独立 Gate。

## 离线

- `scripts/test-gate-generated.sh`（进 pretag）：新工程装 login / enter / portal → `roost add gate` → build、vet、`roost project doctor`（gate:* 与 gate / game 的 `--check-config`）、`roost generate --check`，再跑一次 `roost add gate` 与 `project sync` 不改工程。输出 [test-gate-generated.log](test-gate-generated.log)（约 26 秒）。
- doctor 的五项检查：`TestDoctorPlayerGateChecks` 每项先绿、再改坏它查的那一处只让它变 FAIL。把 `checkPlayerGate` 换成直接返回 nil 时它在第一项就红：[doctor-checks-red-without-impl.log](doctor-checks-red-without-impl.log)。

## 单机真实进程

`scripts/testdata/gate-drill/gate-drill.sh`，私有 mirror-local（偏移 19000：Mongo 三节点副本集、NATS 三节点 JetStream、Redis）与脚本自起的 etcd；开发端口整体平移 31500，工程名随机（本次 `gtdcf2d5f`，见 [env.txt](env.txt)），结束时停掉全部进程、删本工程的库 / 流 / 键，mirror-local 随后 `clean`。

- 三个独立 OS 进程：account 1000（登录入口）、game 1000（`bootstrap.New()` 走 Game Ingress，嵌入 TCP 关闭）、gate 1000（生成的 `NewGateMod`）。客户端端口 38500 由 Gate 进程持有，game 不监听（[drill.out](drill.out)）。
- 客户端（`gatelogin.go.txt`，正式 Robot TCP transport 与工程 PB）：`/portal/login`（dev 渠道）→ `/portal/servers` 里 1000 服的地址等于 Gate 的 `advertise_addr`（来自 game 的 `login.presence.addr` 上报，命令写入）→ `/portal/enter` 拿 `enter:` 票据 → 连 Gate 握手 ACK → EnterStatus（need_create）→ EnterCreate → EnterGame（first=true）。[gatelogin.log](gatelogin.log)。
- 三个进程日志 0 条 ERROR；配置开发版本（[config.game.yaml](config.game.yaml)、[config.gate.yaml](config.gate.yaml)）、doctor 全部结果（[doctor.log](doctor.log)）、Gate 与 game 日志一并保存。

没有做：跨机、多个 Gate 进程、game / Gate 故障重启（这些只有 game-demo 手工接法的 `scripts/test-demo-login.sh` 覆盖）、负载与延迟。数值只作功能诊断。
