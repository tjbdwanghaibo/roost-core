# CellWorld 容量与验收

本轮基线 `cd2493ec`，工作树 `feat/cellworld-production`。采样时使用未提交工作树，源码摘要随证据保存；本文只记录实际证据，不替代生产上线验收。macOS arm64、Go 1.27.3，GOMAXPROCS 未强制限制。本轮代码与测量证据随下表更新。

## 验证口径

- 核心多进程：4 个 Cell、1000 玩家、每 Cell 2250 NPC，共 10000 个 Actor；Actor / Region 20Hz，运动比例 1% / 5%；Witness 上限 64，Base AOI 再过滤。真实 App 单实例、私有 Redis / NATS；持久提交与客户端传输为测试替身，客户端确实经过 EntitySync 编解码。Player 改方向每 2 秒一次，**不是每秒 10 条业务消息的 Robot 负载**。
- Robot：正式生成工程、账号服务、玩家 TCP、PB 移动请求、Nest、EntitySync、文件 WAL 与私有 Mongo 副本集。容量档预热后通过 `SceneView` 申请进入半径 150（离开半径 180），可见数量仍以客户端实收为准。每玩家最多 10 请求/s，逐请求等待响应，服务慢时完成速率下降；不得把目标输入率直接当成完成 TPS。静止玩家仍发送零速度意图。
- Robot `input_ack_p99_ms` 量的是请求 ACK；核心测试 Witness 延迟量的是 Cell 发送到 Base 接受，均不是完整的客户端状态交付延迟。50ms 状态同步要求要另用提交时间戳到客户端应用的端到端测量。
- 有界水库样本最多 200000 条计算核心 P50/P99，最大值覆盖整个测量窗口；Robot 延迟保留最近 100000 条。记录失败样本，性能压测不与本 agent 的 race / build / 索引任务并行。

## 已执行证据

| 项目 | 结果与范围 |
| --- | --- |
| App / Mirror / CellWorld 整包 race | 本轮通过：16.656s / 9.890s / 158.538s；包含本机真实进程故障，不包含 Linux |
| 静止镜像续活回归 | 修前第 22 拍删除仍存活对象；修后同版本续活不标脏，停止续活按 TTL 删除；旧版本 / 删除 / 关闭不能复活 |
| 生成工程 WAL 恢复 | strict 死亡回执、钱包与 Recovery 在真实文件 WAL 重开后恢复；不是 Mongo 投影证明 |
| 账号 / TCP / 存储联合 | 15 个故障 / 业务步骤全部通过；普通场景 Robot 2 个通过；修复移动 PB 接线后，CellWorld Robot 20 个 × 10s 全部通过 |
| 自定义服务名生成工程 | `realm` 全新工程 build / vet / 全包测试通过；partition / handler / cellworldhost / loadtest / realm race 通过，ID / 生成漂移与 cellworld 模块 3 条验证通过 |
| Linux/amd64 编译 | App、Mirror、CellWorld 测试二进制交叉编译通过；未执行 Linux 测试 |
| 最终容量 | 尚未通过，不宣称 1000 / 10000 的 50ms 指标已达标 |

容量原始失败（同机不同宿主负载，**不作严格前后性能对比**）：

| 实现与负载 | Witness P50 / P99 / max | 观察 |
| --- | --- | --- |
| 每拍推进内容版本，100% 运动，60s | 1372.01 / 2102.17 / 2714.80ms | NATS 慢消费者；当时门禁缺失，进程退出成功不等于性能通过 |
| 每拍推进内容版本，5% 运动，30s | 45.41 / 318.17 / 364.88ms | 大量重复打包、NATS 丢包，失败 |
| 静止内容版本不变，续活尚未修复，5%，60s | 6.47 / 119.56 / 186.85ms | 镜像仅剩 5759，样本有功能缺陷，不能当优化收益 |
| 补齐镜像续活，5%，60s | 1568.57 / 3539.23 / 3683.54ms | 缺口 4136，重进入场 356，失败；本机同时有另一个生成器测试与桌面工作负载 |

最终固定配置（1000 玩家 + 9000 NPC，4 Cell，60s）实测：

| 变化比例 | Witness P50 / P99 / max | Base CPU | 四个 Cell CPU | Cell 出站总量 |
| --- | --- | --- | --- | --- |
| 1% | 8.64 / 207.12 / 333.21ms | 140.5% | 112.1%～121.1% | 34195 KB/s |
| 5% | 449.80 / 1304.70 / 1387.59ms | 167.6% | 116.8%～124.9% | 35043 KB/s |

CPU 以一个核心为 100%。两档均未达到 Witness P99 ≤ 50ms；丢弃、解码缺口、调度错误、授权冲突为零，不能因此改判容量通过。Cell 区域拍 P99 约 63～85ms，出站最大排队年龄约 122～235ms；5% 档 Base 接受延迟超过 Cell 出站排队，说明还需分解接收解码与 Base 处理，不能仅凭宿主机负载归因。250ms 自包含恢复帧、每观察者成员列表与重复状态展开仍有成本；本轮没有延长恢复周期来换指标。

首次正式千人 Robot 启动中，开发配置 `max_connections_per_ip: 128` 正常拒绝同一主机的另外 872 条连接。剩余 128 人的 60s 请求完成 76796 次、1254.23 次/s、ACK P99 4.50ms，但**这轮不是千人容量通过**。复跑只在私有容量配置把单 IP 配额设为 Robot 数。Witness / Ghost 保留原有 400 / 500：服务必须覆盖 Base 允许的最大离开半径 300，不能只按本次客户端申请的 180 缩小它；180 的试配被启动校验明确拒绝。生产默认配额与协议不变，实体数与 20Hz 不变；这两轮不作吞吐前后对照。

修正单 IP 配额后的正式联合复跑仍失败：1000 个发起者中 81 个完成，306 个账号 RPC 超时、125 个入场连接关闭、33 个连接阶段关闭、455 个 30s 分散预热超时。账号链路失败交由主线 agent 处理，本轮没有修改账号实现；预热时 Cell 出站最大排队 0.98 / 1.35s，出现 27 次租约过期、Base 重入 334 次、复制缺口 2 次，保留为 CellWorld 容量未关闭的证据。存活客户端汇总的 688.89 请求/s、ACK P99 89.34ms **不能代表千人稳态吞吐**，也不能按其请求零错误宣称整轮通过。

复跑的客户端测量窗口中，Base 平均 CPU 79.62%、峰值 RSS 454.17MiB；两 Cell 平均 CPU 226.29% / 243.32%、峰值 RSS 370.95 / 349.38MiB。此时多数客户端已失败退出，资源指标不能当千人运行成本。服务计数差值含入场与预热，CPU / RSS 窗口按 Robot 时间戳截取；本机另有主线生成器测试与桌面活动，未给出隔离主机的硬件容量结论。

20 人 × 10s Robot 冒烟：1999 次请求、零错误，实际 194.84 次/s，ACK P99 10.48ms；同步 273 帧 / 38668 字节，可见均值 24.78（普通故障场景，不是 10000 Actor 容量档）。小型原始证据保存在 [benchmarks/cellworld-production](benchmarks/cellworld-production/README.md)。

故障矩阵与实现边界见 [CellWorld 契约](../../gameplay/cellworld/README.md)。本轮对固定拓扑负责，不以增加队列、放宽断言或减少实体来宣称目标负载通过。

## 本轮测试边界

维护者要求本轮只验证本次逻辑修改，不承担主线发版验收。此前全仓尝试保留日志：生成器整包超过 30 分钟超时，不能记为全仓通过；后续按 CellWorld、DAO 默认值、模块安装、属性联动与实际生成工程补测。

全仓尝试另发现 `TestModulePortalInstallsOnTheHostedAccountService`、`TestModuleAddDeclaresTheFrameworkServicesItUses` 的不漂移断言失败：测试初始 go.mod 固定最低版本，清单使用 latest，`project sync` 随线上新 tag 正常更新版本。独立复现仅 go.mod 版本变化，与 CellWorld 无关；本轮撤回该夹具试改，交由主线 agent 处理。没有为此修改依赖解析规则或放宽测试断言。

同机交替 10 轮 Witness 批次解码微基准（16 个观察者共享 64 个实体，成员集合未变）中，复用不可变成员列表后：87.21 → 46.95µs/op（−46.16%），192.7 → 138.7KiB/op（−28.04%），219 → 53 allocs/op（−75.80%）。这是内部解码优化，不是整个服务器 TPS 提升。

## 复跑入口

```sh
# 4 Cell 核心多进程测量；改 ACTIVE=1 或 PLAYERS=cell 复测低变化与热点
ROOST_CELLWORLD_PERF=1 ROOST_CELLWORLD_PERF_COUNT=1000 ROOST_CELLWORLD_PERF_NPCS=2250 \
  ROOST_CELLWORLD_PERF_ACTIVE=5 ROOST_CELLWORLD_PERF_SECONDS=60 \
  GOWORK=off go test ./gameplay/cellworld -run '^TestCellWorldMeasure$' -count=1 -timeout 5m -v
# 真实生成工程：先导出脚本要求的私有 Mongo / Redis / NATS 地址
ROOST_SEAMLESS_OUT=/absolute/evidence ROOST_SEAMLESS_ROBOTS=20 \
  ROOST_SEAMLESS_ROBOT_SECONDS=10 bash scripts/test-seamless-demo.sh
# 2 Cell 正式容量配置：9000 NPC，1000 玩家，30s 正常移动预热后 60s 测量
ROOST_SEAMLESS_OUT=/absolute/capacity ROOST_SEAMLESS_ROBOT_ONLY=1 \
  ROOST_SEAMLESS_CAPACITY=1 ROOST_SEAMLESS_ROBOTS=1000 \
  ROOST_SEAMLESS_ROBOT_SECONDS=60 ROOST_SEAMLESS_ROBOT_ACTIVE=5 \
  bash scripts/test-seamless-demo.sh
# 已运行的分区工程可直接从别的主机执行相同 Robot，填写真实 account 与 TCP 地址
GOWORK=off go run ./cmd/loadtest -scenario cellworld -count 1000 \
  -endpoint host:7000 -account-nats nats://host:4222 -nats-prefix project \
  -cellworld-duration 15m -cellworld-active 5 -ramp-step 20 -ramp-interval 100ms \
  -max-p95 1020 -report cellworld.json
```

脚本可在 macOS / Linux 运行；跨机应在服务器部署相同版本生成工程与配置，再从独立机器运行 Robot。单机脚本不是跨机部署自动化。测试依赖须私有，日志 / profile 不含凭据；生产 HA、持续容量和跨机链路仍需部署方验证。

## 交付状态

本轮功能修改、相关 race、真实故障演练、生成工程与微基准已完成记录；**千人 / 万 Actor 的延迟与联合稳态容量未验收通过**。尚需处理 CellWorld 接收解码 / Base 接受积压与高密度入场的预热退化，不把这些混为无关主线问题。账号接入超时、latest 版本测试夹具交由主线；Linux 实跑、跨机与 Redis HA 属于目标部署环境验证。

CBM 已刷新到 `2026-11-13T13:31:18Z`；本轮 122 个代码 / 脚本 / 配置路径中 117 个无已记录解析缺口，5 个模板局部解析缺口已按源码补证，详见随附 coverage。本轮通过 `feat/cellworld-production` 分支交付；主线合并与发版不属于本次操作，本机私有测试依赖已关闭。
