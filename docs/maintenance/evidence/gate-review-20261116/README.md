# Gate 自定义服务名续审证据（2026-11-16）

最终 main：`0f314be50572385e73b7e3d222513a7015fa90ff`；初始 main：`b0de8a62a0cd4aa3205a5056617b30ab5a2e0427`。两者的 Gate 生成器、Gate/Ingress runtime、App 单实例键与校验实现没有差异；后者到前者只增加发版元数据、最低 core 版本与方案文档。Linux amd64、Go 1.27.2、NATS 2.12.3，见 `env.txt`。

| 检查 | 结果 / 日志 |
| --- | --- |
| 初始 main 的 Gate CLI 四个顶层单测及子场景，race count=1 | PASS，`gate-unit.log` |
| wiring/gate 默认测试集，race count=1 | PASS，`gate-wiring.log`；未启用 integration 标签 |
| 正式生成工程验收 | 初始与最终 main 均 PASS，`gate-generated.log`、`gate-generated-latest.log`；最终 doctor 详见 `default-doctor-latest.log` |
| 初始 main CLI：新工程 + login/enter/portal + add gate edge | 成功，`named-add-gate.log`；doctor 返回 0，`named-doctor.log`，五项 gate:* 与 account/edge/game 的进程 config-check 全部 OK |
| 生成 Gate 的真实 TCP + NATS 探针，race count=3 | 初始/最终 main 各一轮三次；最终日志 `gate-names-runtime-latest.log`：默认服务名三次 PASS，自定义 Gate、玩家服各三次 FAIL |
| 正式 App 单实例 checker 的服务名/协议角色对照，race count=3 | 特征验证 PASS，`singleton-names.log`；自定义名的协议角色查询均为 false |

生成工程脚本实际检查 build、vet、doctor、generate --check 和重复 add gate / project sync 无漂移，不是完整模块业务场景或生产联合验收。doctor 的 self-sync WARN 是默认 Player 无同步字段，不是本问题。测试过程第一次网络夹具重复登记 Registry 已内置的 RuntimeFailure/Admin，已在夹具修正后重跑；证据中的网络日志均来自修正后的夹具，对照成功而实验失败。

## 复现

使用最终 main 的独立检出，不在固定报告分支的旧运行时上执行。探针保存为 `.txt`，不会被日常测试自动运行。

1. 在 main 检出执行 `ROOST_GATE_KEEP=1 bash scripts/test-gate-generated.sh`。成功后会打印临时目录，其中 `gated` 是工程，`roost` 是同检出构建的 CLI。
2. 启动私有 NATS，例如 `nats-server -a 127.0.0.1 -p 14222`；不需要 JetStream。
3. 将 `review_gate_names_test.go.txt` 复制为该工程的 `internal/access/player/tcp/review_gate_names_test.go`，在工程执行：

```bash
GOWORK=off REVIEW_NATS_URL=nats://127.0.0.1:14222 \
  go test -race -count=3 -timeout 2m -v \
  -run '^TestReviewGeneratedGateServiceNames$' ./internal/access/player/tcp
```

预期退出 1：`gate_game` PASS，`edge_game` / `gate_world` FAIL。探针调用实际生成的 NewGateMod 与正式 GameIngressMod；请求走真实 TCP 和 NATS，测试没有改 Gate 的身份判断。替身范围：account ValidateSession、etcd 发现、单实例身份权威。身份表按实际服务名建键；不模拟 Redis 过期或多机网络。

4. 将 `review_singleton_names_test.go.txt` 复制到 main 的 `framework/app/review_gate_names_test.go`，执行：

```bash
GOWORK=off go test -race -count=3 -v \
  -run '^TestReviewGateSingletonServiceNames$' ./framework/app
```

预期 PASS：正式 checker 对实际 edge/world 持锁名答 true，对固定 gate/game 角色答 false。它使用已有的 fakeSingletonStore，验证网络替身的查键契约，不是实际 Redis 验收。

5. CLI 接受自定义名的复现：另建新的 `-template game` 工程，`go mod edit -replace github.com/tjbdwanghaibo/roost-core=<上述main检出>`，依次执行 `roost module add login enter portal`、`roost add gate edge`、`roost project doctor`；本轮初始 main 记录见 named-* 日志。

## 边界与收尾

本轮没有运行真实 Redis/etcd/account 多进程链、跨机、完整 Gate integration 集合、全部模块/玩法、容量或长稳测试。单进程测试中的两端使用独立 NATS 客户端，不能声称部署了两个真实 App 进程。临时工程、临时探针与私有 NATS 在保存证据后清理；既有压测材料保留。
