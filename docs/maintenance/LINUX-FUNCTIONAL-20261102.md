# Linux 非压测验收续报（2026-11-02）

## 范围与结论

本轮遵照“保留压测，补齐其他 Linux 测试并提交报告”，验证基线为 [main / v1.37.1](https://github.com/tjbdwanghaibo/roost-core/commit/773ca443dfb21e97cedc1aac59067de6f980e2a8)。固定分支只交付验证记录，不改运行时、不提交 PR/issue。未启动容量、backlog throughput、长稳或物理性能测试，也未删除原有压测材料。

**不能宣布全绿或上线验收完成。** 全仓默认测试保留一项 Lockstep 快照失败；真实依赖入口保留 NATS 关闭状态和 etcd 冻结用例失败，SLG 真实恢复夹具也未通过。局部复跑结果分别记录，不能覆盖原失败。当前机器可以补的功能证据与仍需要外部环境的验收分开列出。

本轮结果、跳过清单与日志 SHA-256 见 [机器可读汇总](evidence/linux-functional-20261102/results.json)；游戏矩阵详见 [13 类结果](evidence/linux-functional-20261102/genres.json)。测试事件计数含子测试／重复轮次，不代表不同业务场景数。完整日志保留在执行环境 `/workspace/linux-acceptance/logs`，不提交数据库、二进制或大体积构建产物；哈希用于核对原日志，不代替失败摘录。

## 环境和可复现前提

- Linux x86_64 6.18.57；Go 1.27.2；GOMAXPROCS=4；CGO 可用。
- 本机独立 Mongo 7.0.45 三节点副本集、NATS JetStream 三节点、Redis 7.4.11 standalone 与六节点 Cluster、etcd 3.5.21、Toxiproxy 2.12.0。均为同机多进程，不是跨主机容灾。
- 使用仓库隔离环境脚本与独立端口／库名／键前缀。基础目录 `/workspace/linux-acceptance/it`、端口偏移 6000，Mongo cache 0.25GB/节点。故障矩阵与共享依赖消费者串行执行。
- 源码检出 `/workspace/core-review-current`；已有 `.review/`、审查探针不构成本轮运行时改动。生成工程采用当前源码 replace；`GOFLAGS='-mod=readonly -buildvcs=false'` 避免工作区外生成目录 VCS stamping 失败，不能据此证明发布包下载流程通过。
- 容器 PID 1 不回收孤儿进程。初次故障测试结束节点成为 zombie，脚本 `kill -0` 误判存活；本轮以 Linux `PR_SET_CHILD_SUBREAPER` 的父进程启动依赖与测试、回收退出子进程。另一次遗留 Toxiproxy 占端口导致代理缺失，原结果保留；清理本任务进程后重跑。未修改框架来绕过故障断言。
- 图谱工具不可用，按精确源码、脚本与日志核对覆盖，不声明索引穷尽审计。

## 已执行功能验证

| 套件 | 结果与边界 |
| --- | --- |
| `go test -json ./... -count=1` | 161 包通过、1 包失败、22 包无测试；9069 PASS、95 SKIP 测试事件。唯一失败叶节点是下述 Lockstep 用例。无集成标签／外部环境门的默认通过不等于依赖测试通过。 |
| CLI Linux 进程与信号 | InterruptedCommand、InterruptInEveryStagePhase、DoctorGoCommandTimeout、DependencyCommandCancel/Interrupt，`-race -count=3`；36 PASS，零 SKIP。 |
| hotcode | `go test -race ./framework/hotcode/... -count=1`；2 包、12 测试事件通过、零 SKIP，含真实 Linux `.so` 构建／加载／回滚。 |
| 13 类游戏生成矩阵 | SLG、MMORPG、MOBA、卡牌 RPG、塔防、生存撤离、合成休闲、放置经营、自走棋、派对房间、回合竞技、合作 Roguelike、社交模拟均通过；自定义 module 共 181 PASS、零 SKIP。含生成／编译／vet／生成物检查，默认持久化替身，不是全部真实存储或容量验收。 |
| Level／同步模式／Heartbeat 生成消费者 | 三个正式脚本均退出 0；日志及事件统计见汇总。 |
| Redis standalone 服务接线 | integration 标签下 wiring、service、Redis、versionstore、cache 共 52 包通过，1897 PASS；72 SKIP，包含其他依赖门和 Cluster 分支，不能称零跳过。 |
| Redis Cluster | 正式 `redis-cluster-suites.sh` 退出 0；standalone 分支跳过，以上独立 standalone 运行补证。 |
| Redis race | wiring/integration、wiring/mail、wiring/rank、service/mail 四包加 integration/race，退出 0。 |
| PhysX Linux 原生服务 | 用当前 bridge 与已有 PhysX 4.1 源码重新编译 native 库，358 构建目标完成；`scripts/physics/verify-service.sh` 退出 0。六包 `-race -tags physx -count=3`，以及三包真实 NATS/Gate/TCP 选择用例 `-race -tags integration,physx -count=3`。不含 Unity/IL2CPP 或物理容量证明。 |
| NATS 弱网 | Toxiproxy 延迟、断连接、半开 ACK 丢失三个真实 DataEngine 用例 `-race` 通过；这是应用代理注入，不是 Linux netem。 |
| 真实依赖总入口 | 1937 PASS、71 SKIP、2 FAIL 测试事件，退出 1；失败为 NATS 关闭状态夹具和 etcd 冻结用例，见下文。Redis 的不同门变量及各类性能门仍会跳过，由独立 Redis 套件补部分覆盖。 |
| Remote 故障矩阵 | [最终 21 格](evidence/linux-functional-20261102/linux-functional-verified-20261102.tsv)全部 PASS，各格拒绝 SKIP/no tests；包括 Mongo 主节点／多数派、NATS 单节点／全停的 async/strict/pipelined 十二组合，以及租约进程、Redis 切主／未复制 fence、持久权威隔离、owner 计数、Mongo WAL 恢复、broker 故障和弱网。单机功能证据。 |
| 10 类游戏真实依赖模式 | MMORPG、MOBA、卡牌 RPG、塔防、生存撤离、自走棋、放置经营、合成休闲、派对房间九类脚本通过；SLG 失败。默认矩阵另外三类没有同名 REAL 入口，不声明真实恢复已验证。MOBA 可选跨 sid routing 模式未额外启用。 |
| 生成 DataEngine／Remote／demo | 独立补跑均退出 0，分别 108/24/28 PASS，33/3/1 SKIP。DataEngine 多阶段重复运行中的非当前阶段及压力门按设计跳过；Remote 的 Mirror Local 独立入口仍未执行；demo 的 Cluster 子项未执行。不能把脚本退出 0 当作所有生成测试零跳过。 |
| 生成 Saga／DAO | Saga native steps + effect consumer 2 PASS、零 SKIP；DAO golden runtime 99 PASS、3 SKIP（map 回调内 clear 的契约排除）。 |

默认全仓的 95 个跳过事件、其他套件跳过名称全部记录在汇总，未将其改记为通过。没有运行 `mirror-local.sh test-core`：该入口包含十万记录 backlog 验收，不能按本次功能补验授权直接启动；其可拆分的独立私有集群、Mirror Local 及 demo Cluster 功能仍需后续专门执行。本轮磁盘余量约 1GB，没有再建立一套完整私有 Mongo/NATS 集群。

## 必须保留的失败与阻塞

### 全仓 Lockstep 失败：隔离通过不等于稳定性已修复

`TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive/deep-buffered_link/written_lane_follows_the_link` 在约 90 秒后失败：

```text
tcp_test.go:892: live Tick after the snapshot:
receiver 1 session 1: player tcp: session not found: slow
receiver 2 session 2: player tcp: session not found: fast
```

同一顶层用例单独 `-race -count=1` 复跑通过（9.212 秒）。原全仓三个 FAIL 测试事件是叶节点及其父节点，不是三个独立缺陷。尚未确认是运行时故障、并发执行资源影响还是测试超时假设；保留为验收失败，不用重跑绿覆盖。

### etcd 冻结用例失败

真实集成入口 `TestRealEtcdResignHonoursTheCallerBudget` 失败，关键输出：

```text
Resign(500ms) against a frozen etcd: err=<nil> elapsed=3ms is_leader=false
Resign did not honour its budget, or kept the local leadership
```

源码在发送 SIGSTOP 后立即发 Resign，没有等待进程进入 stopped 状态；成功返回并已放弃本地 leader 与断言要求的 DeadlineExceeded 不符，不能仅凭此判定运行时违反 deadline。隔离 `-race -count=5` 五轮均通过（各约 501ms 返回 DeadlineExceeded）；原失败仍保留。进程停止屏障是后续夹具定位方向，本轮没有修改测试来获得通过。

### NATS 真实关闭状态夹具漏初始化日志客户端（已定位）

`TestRealNatsEveryExportedMethodAnswersFromTheDriverStateAfterAnUndrainedClose` 在总入口失败，独立 `-race -count=3` 三次也失败。十个 JetStreamLogClient 方法都返回 `nats log: not initialized`，断言要求 `errors.Is(err, fnats.ErrClosed)`。

定位：[real_nats_integration_test.go](https://github.com/tjbdwanghaibo/roost-core/blob/773ca443/infra/network/nats/driver/real_nats_integration_test.go#L344) 构造 `closedStateEnv` 时没有设置 `log`；共享测试表已调用该字段上的日志方法。`JetStreamLogClient.ready` 对 nil receiver 正确返回未初始化错误。另一套普通关闭夹具已调用 `NewJetStreamLog(js)` 并填入字段。本例应在关闭前按相同方式创建日志客户端后重跑，当前失败不能证明生产关闭错误，但证明新增日志方法的真实 undrained-close 覆盖缺失。本轮只报告，未修改测试或运行时。

### SLG Player 到账与 Remote 交接恢复被固定 WAL 条数挡住

正式 `ROOST_SLG_REAL=1` 入口的 `TestSLGAlliancePlayerDelivery` 在父进程强杀前失败：

```text
home 1000 write: writer exited before kill: exit status 1
want two unprojected strict records:
{Committed:3 Projected:0 WALUnacked:3 ProjectionFailures:0 FatalProjectionConflicts:0 ...}
```

位置：[player_delivery_test.go](https://github.com/tjbdwanghaibo/roost-core/blob/773ca443/scripts/testdata/slg/alliance/player_delivery_test.go#L103)。活动链路先通过，随后统计要求 WALUnacked 恰好为 2，实际为 3；因此本轮没有完成 Player 的 SIGKILL／冷恢复／第三进程奖励防重验证。单独补跑 `TestSLGAllianceRemoteDelivery` 也被同一 Player 子进程断言挡住，日志已到 owner 2000 → 2001 交接并拒绝旧 owner，但不能将整个链路算通过。RPC 接收端预算剩余用例单独补跑通过。

应逐项核对当前创建、初始化、奖励、钱包读取的提交语义，调整夹具预期并保留正确的 WAL 未投影／冷事实／防重断言，再完整跑两条链路；不能简单放宽为“至少两条”掩盖额外提交来源。本轮未证明发生奖励丢失或重复到账。

第一次单独补跑 Remote 时我使用了不符合消费者安全前缀的数据库名，被 `refusing database ... outside this consumer` 拒绝；修正为 `roost_slg_it_` 前缀后才得到上述有效失败。两次日志都保留，不将前一次操作错误算作产品问题。

### 不能视为框架编译失败的环境阻塞

| 验收 | 原始结果 | 尚缺证据 |
| --- | --- | --- |
| 发布模块网络流程 | 两个 ROOST_NETWORK_TESTS codegen 用例在 go mod tidy 失败；独立下载 v1.37.1 显示 HTTPS git 无法读取 Username、terminal prompts disabled | 配置可用的发布下载认证后重跑；本地 replace 通过不能替代 |
| 完整 .NET SDK 项目 | `dotnet build` 在 NuGet 首次迁移创建命名互斥量 `NuGet-Migrations` 时 IOException，退出 1 | 完整 restore/build/WarningsAsErrors 和 generated TCP SDK 门禁；此前直接 Roslyn 的 Linux 托管/native 验证仍仅限原报告范围 |
| 生成 distroless 镜像 | 真实基础镜像已拉取；在线多阶段构建下载 compress 依赖被 storage.googleapis.com 的 403 拒绝；本地 vendor 离线对照在可用磁盘下降至约 624MB 时主动取消，退出 130 | 完成多阶段镜像构建、非 root 启动与全部服务 health；不能把 vendor 成功或基础镜像可用记作 E23 通过 |

## 外部验证边界

对照 [外部验证细则](EXTERNAL-VERIFICATION.md)，本轮不删除原验收项：

- E27 的 Linux 热更新功能已有实测；E26 补了真实 Unix 信号与进程树取消，仍没有提交窗口 SIGKILL、ENOSPC 和发布依赖离线验收全套证据。
- 同机 Mongo/NATS/Redis/etcd 多进程故障与 Toxiproxy 补充了恢复功能证据，不能关闭 E02/03 等真实多机隔离、时钟、MTU，以及相关 HA 项的跨主机要求。
- E01 需要内核网络故障注入权限；本进程无 NET_ADMIN。E19 物理断电／块设备故障、E21 真 systemd（当前 PID 1 为 tail）、E22 Kubernetes、E24 生产配置、Unity/IL2CPP 不具备目标环境。
- E23 镜像验收未完成；E25 Windows 不在本轮 Linux 范围。
- E12/15/16/17/18/20/28 等容量、性能和长稳项按用户要求保留，不在本轮启动或宣布关闭。

## 重跑入口

在同一源码基线、工具与独立依赖齐备后执行；故障套件不得并发共享依赖：

```bash
export GOMAXPROCS=4 GOFLAGS='-mod=readonly -buildvcs=false'
go test -json ./... -count=1
go test -race ./framework/hotcode/... -count=1
python3 scripts/test-genres-generated.py --out /path/to/genre-results
export ROOST_IT_HOME=/path/to/isolated-it ROOST_IT_PORT_OFFSET=6000
export ROOST_IT_TOXIPROXY=1 ROOST_IT_MONGO_CACHE_GB=0.25
bash wiring/scripts/integration/dataengine-env.sh test
source "$ROOST_IT_HOME/roost-dataengine-it/env.sh"
ROOST_REMOTE_MATRIX_LABEL=linux-functional-rerun bash scripts/test-remote-matrix.sh
# 其余游戏以对应 ROOST_<GENRE>_REAL=1 配合正式脚本执行
ROOST_SLG_REAL=1 bash scripts/test-slg-generated.sh
bash scripts/test-dataengine-generated.sh
bash scripts/test-remote-generated.sh
bash scripts/test-demo-generated.sh
ROOST_CORE_REPLACE="$PWD" bash codegen/scripts/dao-golden-runtime.sh
bash wiring/scripts/integration/dataengine-env.sh down
```

容器 PID 1 不回收退出子进程时，应使用具备 subreaper/reaping 的测试 supervisor；不可将 zombie 超时算作数据库真实停机恢复失败。以上重跑命令不隐含压测授权，也不包含未配置的生产环境操作。
