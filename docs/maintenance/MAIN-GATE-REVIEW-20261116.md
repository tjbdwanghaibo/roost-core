# main v1.51.0 Gate 命令续审（2026-11-16）

审查对象：main **0f314be50572385e73b7e3d222513a7015fa90ff**；本轮开始时是 `b0de8a62a0cd4aa3205a5056617b30ab5a2e0427`。相对上轮 `f2c1ac443`，主要新增 `roost add gate` 与相关接线、配置检查、部署产物；收尾新增 v1.51.0 发版、最低版本更新及三份未实施重构方案。Gate 生成与运行时在本轮两个 main 快照间未变；正式生成脚本及网络探针已在最终 main 重新执行。

**结论：新增一项 P2（RV-20261116-02），自定义服务名生成成功、doctor 通过，但 Gate 握手按错误的服务类型校验单实例锁，所有登录被拒绝。** 默认 gate/game 对照连续三轮完成握手和转发。只提交问题、报告和探针至 `fix/core-game-server-review`，没有修改运行时。

## RV-20261116-02：自定义 Gate / 玩家服务名没有映射到单实例身份（P2）

新入口 [addGate 第 313–323 行](https://github.com/tjbdwanghaibo/roost-core/blob/0f314be50572385e73b7e3d222513a7015fa90ff/codegen/internal/roost/gate.go#L313) 接受 `roost add gate edge`，并允许为 `access.player.service` 指定的玩家服务接 Gate。生成的 [NewGateMod](https://github.com/tjbdwanghaibo/roost-core/blob/0f314be50572385e73b7e3d222513a7015fa90ff/codegen/internal/roost/gate.go#L250) 及 Ingress 接线没有传入实际服务名。

两端当前身份契约不一致：

- App 按实际启动的服务名持锁：[singleton.go 第 638 行](https://github.com/tjbdwanghaibo/roost-core/blob/0f314be50572385e73b7e3d222513a7015fa90ff/framework/app/singleton.go#L638)，键为 `<prefix>:<service>:<sid>`。`edge` 的锁就是 `...:edge:11`。
- Gateway 在 ACK 前先核对本端 `gate`，再核对目标 `game`：[gate.go 第 182–192 行](https://github.com/tjbdwanghaibo/roost-core/blob/0f314be50572385e73b7e3d222513a7015fa90ff/infra/network/gateway/gate.go#L182)。这是协议角色名。
- wiring 直接把协议角色名交给单实例检查，没有转换：[mod.go 第 116–117 行](https://github.com/tjbdwanghaibo/roost-core/blob/0f314be50572385e73b7e3d222513a7015fa90ff/wiring/gate/mod.go#L116)。因此检查 `...:gate:11` 或 `...:game:22` 时读不到真实持锁实例，返回 `ErrBindingStale`；TCP 握手 ACK 不会发出，客户端得到 EOF。

影响条件明确：默认 `gate/game` 不受此问题影响；选择 CLI 明确支持的自定义 Gate 名称，例如 `edge`，即可触发。自定义玩家服务名具有同样的问题。进程可以成功监听，`--check-config` 与 doctor 都未识别这个名字契约，因此容易在生成验收通过后才发现无法登录。这是身份核对拒绝，不是网络超时或 account 校验失败。

## 证据

1. 初始 main `b0de8a62a` 的 CLI 创建自定义 module 工程，安装 login / enter / portal，执行 `roost add gate edge` 成功。`roost project doctor` 返回成功，`config-check:edge`、`config-check:game` 及五项 `gate:*` 均 OK；另有未配置玩家同步字段的普通 WARN。日志在证据目录。
2. 实际生成的 `NewGateMod` + 正式 `wiring/gate.GameIngressMod`，连接私有 NATS 2.12.3；客户端真实 TCP 握手后发一条 echo 请求。身份权威按实际服务名提供持锁代次，account 与发现为替身。三轮结果：

| 实际服务名 Gate / 玩家服 | 身份查询 | 结果 |
| --- | --- | --- |
| gate / game | gate:11=true，game:22=true | 3 次握手和转发 PASS |
| edge / game | gate:11=false | 3 次握手 FAIL，EOF |
| gate / world | gate:11=true，game:22=false | 3 次握手 FAIL，EOF |

3. 单独调用正式 `app.singletonLiveness.Matches`，使用仓库已有的单实例存储替身。对实际 `edge` / `world` 锁，以实际服务名检查成功，以固定 `gate` / `game` 角色检查失败，三轮一致。这验证了网络探针替身的查键语义，没有假设服务名会自动成为别名。

前两层分别证明命令允许该配置与运行时握手失败，第三层核对真实 App 身份查询契约。不是完整 App 多进程 + Redis/etcd/account 的联合演练；真实网络仅覆盖 TCP 与 NATS，不能扩大为跨机或所有外部依赖验收。

建议：生成接线明确区分协议角色与实际服务类型，使身份检查在两端都映射到实际服务名，保留 incarnation 核对。若本版暂不支持名字映射，应在 CLI / manifest / doctor 提前拒绝非 gate/game 的组合，并同步收窄命令契约。补默认名、自定义 Gate、自定义玩家服务三组完整握手回归；仅检查生成文本或配置合法性不足以覆盖此问题。

## 上轮问题与本轮范围

- **RV-20261116-01 room 登记时序问题仍未改相关代码**：`f2c1ac443..0f314be50` 对 `modules/room` 无差异。本轮为源码复核，没有重复运行上轮三个 room 故障探针，也没有将其重复计为新问题。详见 [上轮报告](MAIN-REVIEW-20261116.md)。
- 此前换服修复及其剩余窗口沿用上轮限定结论，本轮未重新执行迁服故障演练。
- 本轮检查了 Gate 命令的准入与幂等、认证器接线、发现与身份、开发/生产配置修改、doctor、Kubernetes 端口及停止预算。除上述问题外未确认新缺陷，不宣称全仓无 bug。
- 收尾新增的 singleton-etcd、controllers/view 布局与事务事件文档明确标记“方案已定，未实施”；本轮仅核对其状态，不把它们当成已实现逻辑，也没有完成这三份方案的全面评审。单纯换单实例存储不会解决本问题：身份键仍由 App 的实际服务类型决定。

Linux 验证：Gate 命令四个顶层测试及子场景 race 通过；wiring/gate 默认测试集 race 通过；正式 `scripts/test-gate-generated.sh` 在最终 main 通过 build、vet、doctor、generate 检查以及重复 add / sync 幂等检查。未启用完整 integration 测试集、未跑全仓/全部玩法或负载长稳；历史压测资料未删除。代码图谱工具不可用，依据源码差异与精确路径核查。

[探针、命令与原始日志](evidence/gate-review-20261116/README.md)。
