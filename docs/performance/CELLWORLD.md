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

## 千人排查（2026-11-14）

基线 `067c14f7`，分支 `tmp/cellworld-perf`，证据见 [benchmarks/cellworld-diagnosis-20261114](benchmarks/cellworld-diagnosis-20261114/README.md)。核心测量 30s 窗口、5% 运动，带临时分段探针（未提交）。Witness 延迟的口径是 **Cell 发布（`publish` 里打 SentAt，编码之前）到 Base handler 开始**，不含 Cell 区域拍与出站排队。

| 阶段（修复后，nice 0，GOMAXPROCS 缺省，一轮） | p50 / p99 |
| --- | --- |
| Cell 区域拍（不在 Witness 口径内） | 4.8～6.3 / 10.3～10.8ms |
| Cell 出站排队 enqueue → take（不在口径内） | 1.9 / 16.3ms |
| Cell 编码（encodeReplica + msgpack） / Publish | 0.17 / 2.4ms；≈0 |
| Cell 发布 → Base 回调入口（含 NATS 与 Base 客户端待处理队列） | 1.2 / 27.0ms |
| Base 回调内解码（decodeMsg + decodeReplica） | 0.13 / 2.6ms |
| Base 回调入口 → handleBase 开始（Nest 排队） | 0.28 / 3.9ms |
| Witness（合计） | 1.8 / 28.8ms |

nats-server 自身 CPU 约 20～30%，服务端待发字节约 0；Cell 编码与 Publish 很小。**主导阶段是 Base 唯一的 NATS 订阅回调**（`Node.receive`，`node.go` 订阅处）：四个 Cell 的全部状态消息在一个 goroutine 里串行 msgpack 解码与重建（约 32～33MB/s、每拍成批到达），回调墙钟占用 46～67%（同一回调的 CPU 只占约 30%，其余是宿主机抢占）。占用超过约 65% 时排队不收敛：同一代码同一配置，Witness P99 有 29ms、47ms，也有 459ms、2037ms（P50 228ms 的那一轮重现了此前 5% 档 P50 450ms 的失败形态）。

成本来源依次是：

1. **周期全量帧**（`replication.go` `encodeReplica`：观察者成员全量刷新时 `forceFull` 把它的全部成员状态发全量）。状态条目里全量占 85%（每 Cell 30s 约 130～164 万全量对 18～26 万增量），1%、5% 两档出站字节几乎相同即由此而来。实验开关（成员全量刷新时不强制成员状态全量，状态仍各自 250ms 刷新全量；**违反现行“整份视野自包含”契约，`TestPeriodicFullWitnessIsSelfContained` 会红，未提交**）下：Cell 出站 8～10 → 3.5～3.9MB/s、全量条目 −74%、Base 回调占用 460 → 292 / 237ms/s，Witness P99 14.4 / 9.8ms（GOMAXPROCS 缺省 / 4）。
2. **Base 每份视野重复展开**：`decodeReplica` 给每个观察者重建 64 个 `EntityState`，`feedMirrors` 再逐个续活；千人档每秒约 128 万次。续活的锁争用已修（见下）；重复展开本身仍在。
3. **宿主机**：五个 Go 进程各 GOMAXPROCS=10 跑在 10 核（4P + 6E）上，测量期间 load average 12～19。测试负载约 5.5～6.5 核（4 Cell 共约 4.2 核、Base 1.1～1.7 核、nats-server 0.25 核），加上常驻约 1～1.5 核，已接近饱和；zsh 后台启动还会 nice 5。按 CPU 时间计：Cell 约 21µs / Actor / 拍（其中 Go 调度器空转约四分之一），Base 约 55～66µs / 份 Witness（约 1µs / 视野对象）。本机能复现失败形态，**不能给出千人 50ms 是否达标的结论**；同一代码在本机时过时不过。

已修（行为不变）：Base `feedMirrors` 改用 `Mirrors.TouchMany`（feedMirrors CPU 10.08s → 1.35s / 30s，`BenchmarkMirrorRenewView` 6.65µs → 0.66µs）；Cell `witnesses` 改用 `BlockIndex.AppendRadius` 与复用缓冲（`BenchmarkRegionWitnesses` −49% 时间、−77% 字节、−80% 分配，区域拍 p50 10～13 → 5～6ms）。区域拍 P99 超过 50ms 的样本（此前 63～85ms）在修复后多为 10～40ms，偶发 60～140ms 与宿主机 load 同时出现。

Robot 联合复跑（修复后代码，私有环境，1000 人，20s 测量）：账号 RPC 超时与入场断连**没有复现**，1000 人全部入场；787 人在 30s 分散预热里没走到目标。同时采样：nats-server 无服务端慢消费者、待发约 0、CPU 20～30%；Redis 平均 0.1ms；account CPU 11～16%。Base（game sid 1000，Gate 在同一进程）峰值收 45～60MB/s，客户端侧出现 330 次 `slow consumer, messages dropped`（CellWorld 订阅的待处理上限溢出），随后 1200 次复制缺口 → 基线重置 → 全量风暴（Cell 全量条目 95%）、447 次重入；Base 的 enter 钩子（聊天公告、邮箱 RPC、Nest 同步）约 450 次超过期限，`enter_game` P50 3.25s。此前那一轮 Gate 的 125 次 `account gave no verdict`（ValidateSession 超时）与本轮同类，都是 Base 进程过载；那一轮 Robot 侧 306 次 CreateRole 超时本轮未复现，账号服务、NATS、Redis 本轮实测正常，当时另有生成器测试同机运行，**原因尚未确认**。

### 合批、刷新相位与按缺口修复（`62f88d7a`）

在上面两处修复之上做了排查里的 A 与 D，都不改“整份视野自包含”与“全量间隔不超过 `ReplicaRefresh`”：

- **A**：Cell 按观察者所在的刷新区块（边长 2R）排序后合批；成员表全量刷新除了满一个周期，也在区块相位边界触发，同块观察者同拍刷新，强制全量的成员在一条消息里只编一次。
- **D**：缺口只拒绝解不出的对象与观察者，接收方用 `replicaReset` 只请求这些基线，同一对象 / 观察者每个刷新窗口最多请求一次；一次丢包不再让整个 peer 退回全量。线上格式 v12。

单元回归（确定性）：丢一批 witness 后 10 拍内的全量条目 256 → 64（这一批成员 64）；入场错开的 16 个同区块观察者 4 个刷新周期的全量 1280 → 256。

同机交替各 2 轮（30s，4 Cell，1000 + 9000；宿主机另有压 CPU 的任务，load average 10～30）：

| 档 | 版本 | 全量条目 / s（4 Cell） | 全量占比 | Cell 出站 MB/s | Base 回调占用 ms/s | CPU µs / Actor / 拍（Cell + Base） | Witness P50 / P99 ms（load） |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1% | 修前 | 194.6k / 193.7k | 89% / 88% | 34.2 / 33.9 | 489 / 826 | 27.7 / 25.4 | 2.1 / 42.5（10.6）；21.7 / 187（24.9） |
| 1% | 修后 | 110.7k / 111.0k | 81% / 82% | 22.4 / 22.4 | 551 / 484 | 23.5 / 22.7 | 3.3 / 66.6（13.1）；3.1 / 201（25.1） |
| 5% | 修前 | 196.3k / 195.4k | 86% / 86% | 35.1 / 35.0 | 765 / 992 | 26.5 / 27.2 | 32 / 1376（14.3）；1270 / 1746（24.2） |
| 5% | 修后 | 112.6k / 112.2k | 78% / 79% | 23.5 / 22.8 | 740 / 925 | 26.8 / 22.0 | 9.0 / 98（18.2）；1093 / 3025（24.6） |

全量条目 −43%、Cell 出站 −34%，两轮之间几乎不变，是这一组的确定性结果。Base 回调占用与 Witness 延迟仍随宿主机负载大幅波动（同一版本 484～925ms/s），不能用来判定达标；P99 只作参考。全量占比仍约 80%：自包含要求每条带全量成员表的消息自带全部成员，一个对象落在几个区块的视野里就每周期被强制全量几次，每条消息最多 16 个观察者，这是当前契约下的下限附近。再往下要么放宽自包含（排查里的 B），要么加大合批（`WitnessBatch`）。

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
