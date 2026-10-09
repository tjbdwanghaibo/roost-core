# 外部验证操作清单

这是按需执行的验收表，不要求接入第一天完成全部项目。先按部署计划选择网络、故障切换或容量场景，再准备表中环境；没有相应环境的项应保留未验证。

以 v1.24.0 发布验收为当前状态：本机真实资源故障矩阵21/21、Remote 1h、Nest/Sync/Saga声明负载已完成，详见 [性能报告](PERFORMANCE.md)。下表保留外部场景的操作方法，不把历史版本的本机验证扩张为跨机或生产保证。E14精确崩溃窗口已验收；E25 Windows不在保证范围。

来源为清理前固定提交的外部验证清单；下文历史来源链接用于追溯，执行前按当前 API/环境确认，不依赖他人临时目录或凭据。故障注入只在独立隔离资源执行。

## 总表

| # | 类别 | 项 | 环境 | 状态 |
| --- | --- | --- | --- | --- |
| E01 | 网络 | Mirror 七类场景在 Linux 内核网络（netem / iptables）下 | Linux 主机 | 未做 |
| E02 | 分区 | 跨主机真实分区、MTU、时钟偏差 | ≥3 台主机 | 未做 |
| E03 | 网络 | Remote 跨机与弱网（OPEN-ITEMS C02） | 跨机 + 真实网关 | 未做 |
| E04 | 网络 | Sync 真实网络、弱网、慢消费者、断线重连 | Linux 服务端 + 负载机 | 未做 |
| E05 | 客户端 | 真实网关 / 反向代理后的 HTTP 与 robot 重连 | 代理 + 弱网 | 未做 |
| E06 | HA | NATS JetStream 多节点 HA | 3 台主机 | 未做 |
| E07 | HA | etcd 多节点 HA | 3 台主机 | 未做 |
| E08 | Redis Cluster | 驱动、单实例锁、Lua 布局在多机 Cluster 切主下 | 多机 Cluster | 未做 |
| E09 | Redis Cluster | account / chat / activity 在 Cluster 与多进程下 | 多机 Cluster | 未做 |
| E10 | Redis Cluster / HA | 异步复制丢写与切主：墓碑、`WAIT`、锁 | 多机主从 | 未做 |
| E11 | HA | Mongo 跨主机副本集、mongos、切主中提交 | 多机副本集 | 未做 |
| E12 | 容量 | saga Mongo 步骤延迟的生产形态 | Linux + NVMe 副本集 | 未做 |
| E13 | HA | 多主机强杀：owner / 锁迁移、双实例、旧回调 | 多主机 | 未做 |
| E14 | HA | Remote outbox“Mongo 已提交、发布前崩溃”精确注入 | 本机私有Mongo/Redis/NATS与子进程 | 2026-10-08精确SIGKILL及同sid恢复已通过；[最终版验收](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/OUTBOX-HISTORICAL-CLOSURE-2026-10-08.md) |
| E15 | 容量 / soak | Mirror 长时间容量 | 专用 Linux | 未做 |
| E16 | 容量 | Mirror 大规模扇出与目标负载 | Linux + 目标负载 | 待维护者给目标负载 |
| E17 | 容量 / soak | Remote / DataEngine 24 小时与稳定容量 | 专用静默 Linux | 未做 |
| E18 | 容量 | Linux fsync 次数与吞吐对照 | Linux 物理机 | 未做 |
| E19 | HA | 物理断电与 dm-flakey 故障注入 | 有 root 的 Linux | 未做 |
| E20 | 容量 | Sync 与 Nest 的 MMO 目标负载线上验收 | Linux 服务端 + 负载机 | 未做 |
| E21 | 部署 | 真实 systemd 的 shell 部署与回滚 | 带 systemd 的 Linux | 未做 |
| E22 | 部署 | k8s 滚动停机与部署物 | k8s 集群 | 未做 |
| E23 | 部署 | distroless 镜像与多阶段构建 | 可拉镜像的环境 | 未做 |
| E24 | 部署 | 仓库外生产配置的 doctor 检查 | 部署方 | 未做 |
| E25 | Windows | CLI 信号与进程树、暂存树、autocrlf、偶发项 | Windows | **排除**：Windows 专属问题不处理、不作为验收门槛（维护者 2026-10-08） |
| E26 | 部署 | Linux 上 CLI 信号、强杀 / 磁盘故障、离线代理 | Linux | 未做 |
| E27 | 部署 | hotcode 真实插件加载 | Linux | Linux 未做；Windows 专属范围排除 |
| E28 | 容量 | 生产流量下竞态窗口的实际频率 | 准生产流量 | 未做 |

## 一、网络与分区

### E01 Linux 内核网络（Mirror）

- **来源**：[Mirror 第 6 步本机替代](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/MIRROR-STEP-6-LOCAL-2026-10-06.md) §5 第 1 行、§3.5 O-M6-4（静默断线靠 ping 20s × 2 才发现）；[Mirror 第 5 步](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/MIRROR-STEP-5-2026-10-06.md) §6；[PLAN-REMOTE-POLICY-MIRROR](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/PLAN-REMOTE-POLICY-MIRROR.md)；[下一轮规划](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/NEXT-ROUND-PLAN-2026-10-06.md) 第 3 项。
- **验证什么**：Mirror 的 S1～S7 七类场景（owner 与只读服务两进程、`SnapshotClient` 推送、JetStream durable 续投、L2 回源）在内核层丢包、乱序、延迟、半开连接、RST、keepalive 下的表现。macOS 的 TCP 栈和用户态 toxiproxy 模拟不了这些。
- **怎么做**：Linux 主机上跑 `scripts/mirror-local.sh test`（`ROOST_MIRROR_LOCAL_HOME=<目录>`，`ROOST_MIRROR_LOCAL_ONLY=S2,S3` 可选场景），把 toxiproxy 注入换成 `tc qdisc netem`（loss / delay / reorder / duplicate）与 iptables DROP / REJECT；补“只丢 FIN、不丢数据”的半开连接。固定 CPU / 内核 / Go 版本，不与别的压测并跑。
- **通过标准**：每个场景 0 条 `MIRROR VIOLATION`（不回退、不复活），在 T + `cached_max_staleness` + 1.5s 内收敛。（建议）netem 丢包 1% / 5%、延迟 100ms / 500ms 下同样满足；半开连接的发现时间与 NATS `PingInterval` 配置一致，记录实测值。

### E02 跨主机真实分区

- **来源**：Mirror 第 6 步 §5 第 2 行；[REMOTE-ACCEPTANCE](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REMOTE-ACCEPTANCE-2026-09-24.md)（“跨机网络分区……不由这些用例证明”）；[REMOTE-AUTHORITY](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REMOTE-AUTHORITY-2026-09-25.md)（多主机部署）。
- **验证什么**：NATS、Redis、Mongo 与 owner、只读服务分处不同主机时，双向 / 单向隔离与恢复后，Mirror 的陈旧上限判定、JetStream durable 续投、Remote 持久权威 fence。本机共用回环，没有网关、ARP、MTU、跨机时钟偏差。
- **怎么做**：≥3 台主机分放依赖与两个服务；iptables 做双向与单向隔离，故意让主机间时钟差到秒级。复用 `mirror-local.sh` 的场景编排与 `scripts/test-remote-matrix.sh`，需要补远程编排。
- **通过标准**：Mirror 同 E01“0 违例”；Remote 新权威 fence 单调增长、旧业务提交被拒、新提交成功（[REFACTOR-2026-09-25-remote-authority](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-25-remote-authority.md) 的口径）。（建议）分区恢复后 60s 内全部读者收敛，最终核验 `.verified` 通过。

### E03 Remote 跨机与弱网（C02）

- **来源**：[REMAINING-2026-09-28](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/REMAINING-2026-09-28.md) §2 C02；[OPEN-ITEMS-2026-09-27](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/OPEN-ITEMS-2026-09-27.md) C02；[ARCHIVE-2026-09-30](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/ARCHIVE-2026-09-30.md) §5 P3；[核心优化交接](README.md) §5“旧疑点核对”。
- **验证什么**：跨机部署 game-demo 或业务工程、接真实网关、注入延迟与丢包，覆盖 RR-20260926-15 / 40 / 52 / 55 各自记录的“未验证项”。
- **怎么做**：`roost project new -template game-demo` 生成工程跨机部署，加 `scripts/test-remote-generated.sh`，netem 注入；网关、客户端与业务 schema 由部署方提供。
- **通过标准**：来源只写“覆盖四条 RR 记录的场景”。（建议）逐条对照四条 RR 的“未验证项”，每条给出场景、结果、是否复现。

### E04 Sync 真实网络与弱网

- **来源**：[SYNC-COMPLETION](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SYNC-COMPLETION-2026-09-23.md)、[sync-modes 实施](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/IMPLEMENTATION-2026-09-24-sync-modes.md)、[快照调度](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-25-sync-snapshot-scheduling.md)、[资源预算与会话恢复](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-25-resource-budgets-and-session-recovery.md)（单轮 1% 门禁不替代 5% / 长稳 / 真实网络）、[恢复公平性](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-25-sync-recovery-fairness.md)、[SYNC-BUSINESS-LOAD](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SYNC-BUSINESS-LOAD-2026-09-23.md)（未测生产 player-TCP 包装、认证、弱网、慢消费者、断线重连）。
- **验证什么**：periodic 与 on_change 两种模式在真实网络、弱网、慢消费者、断线重连下的快照与恢复预算（`Manager.Flush`、1000 客户端集中恢复、5% 变化率），接生产形态的 player-TCP 包装与认证。共享帧协议 / 网关多播属另一条线，不在此列。
- **怎么做**：Linux 服务端 + 分机负载端；`scripts/perf/sync-aoi.sh -mode=periodic|on_change`、`scripts/test-sync-modes-generated.sh`，加 netem。
- **通过标准**：SLA 是 1000 玩家、约 50 可见、1% / 5% 变化、50ms 同步周期（[NEST-COMPLETION](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/NEST-COMPLETION-2026-09-24.md)）。（建议）p99 同步延迟 ≤ 50ms；断线重连后全量到齐时间有界并记录；原始失败样本保留，不放宽阈值。

### E05 真实网关 / 反向代理与客户端重连

- **来源**：[后续 Review 清单](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/REMAINING-REVIEW-HANDOFF-2026-10-05.md) N02、N12 两行。
- **验证什么**：httpserver / gateway / webroute / security 在真实反向代理后的行为（HTTP/2、连接与请求容量、卡死回调）；robot 的连接、认证、会话、重连在弱网下；跨进程 account RPC 握手。
- **怎么做**：生成 game-demo，前面放 nginx 或 envoy，用 `cmd/loadtest` 加 netem；account 握手用 `second-game.sh` 两进程部署。
- **通过标准**：（建议）N02 修过的同形停机路径（NC-80～83）在代理之后仍在 `shutdown.total_timeout` 内退出；robot 重连后 success 全绿、无 `manual_required`。

## 二、HA 与多节点

### E06 NATS JetStream 多节点 HA

- **来源**：清单 N03 行；Mirror 第 6 步 S2a / S2b、O-M6-2、O-M6-4；Mirror 第 5 步 §6；REMOTE-ACCEPTANCE（普通 NATS 不提供持久投递）。
- **验证什么**：多主机 JetStream 集群里流 leader 被杀 / 暂停时 owner 写回复耦合（本机观察到 6～10.6s 才写成）、只读方推送续投、静默断线发现时间；ACK、Term、重投、drain。
- **怎么做**：3 台主机各一个 NATS 节点，在流 leader 上 kill、SIGSTOP、iptables 断网；用 `mirror-local.sh` 的 S2 编排加 `scripts/test-remote-matrix.sh` 的 NATS 单停 / 全停组合。
- **通过标准**：成功 / 失败 / 结果未知分清，再次 Stop 或接管收敛（清单外部验证表口径）。（建议）主机级故障后 owner 写最长不可用时间有记录且小于 Request 截止；结果未知的写不被当成未提交；只读方在 `cached_max_staleness` + 1.5s 内收敛。

### E07 etcd 多节点 HA

- **来源**：清单 N03 行（Resign 预算、服务端资源清理、watch / lease 恢复）；[App 单实例锁](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/APP-SINGLETON-LOCK-2026-10-05.md) §13 观察 3（单节点 etcd 上 SIGSTOP 超过 `etcd.lease_ttl` 后 lease lost）。
- **验证什么**：多节点 etcd 里 leader 迁移或分区时，Discovery 的 lease 与 watch 恢复、Resign 预算、服务端资源清理。
- **怎么做**：3 节点 etcd 分处不同主机，杀 leader、做分区；复用 `-tags integration` 的真实 etcd 用例（如 `TestRealEtcdCloseAfterLeaseVanishedIsClean`）。
- **通过标准**：（建议）leader 切换后服务发现在 `etcd.lease_ttl` 内恢复；停机退出码 0；服务端无残留 lease / key。

### E08 多机 Redis Cluster：驱动与单实例锁

- **来源**：[A2 驱动契约](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/A2-DRIVER-REPLAY-CONTRACT-2026-10-05.md)“未完成 / 观察”（`noReplay` 在 `ClusterClient` 里的 MOVED / ASK 只按源码核对）；清单 N01、N04、N14 行（Cluster 两客户端 integration、跨 slot、`cluster_addrs` 列表写法起服）；App 单实例锁方案（Cluster 下真实进程演练未验）。
- **验证什么**：kit 单实例锁 store（CAS 与 Live 两个客户端）、写命令不重放、Lua 跨 slot 布局在多机 Cluster 切主与 MOVED / ASK 期间的行为；go-redis 写路由缓存旧主的刷新（RR-20260924-24）；`cluster_addrs` 起服。
- **怎么做**：多台主机组 Cluster，重跑 `wiring/scripts/integration/redis-cluster-suites.sh`（`ROOST_REMOTE_CLUSTER_IT=1`，本机已在同机 3 主 3 从上跑过），再按 App 单实例锁 §13 的步骤做两个 sid 的真实进程演练。
- **通过标准**：不误放权、不永久残留、合法新 ctx / 新实例可恢复（清单外部验证表口径）。（建议）切主期间没有重复执行的写，MOVED / ASK 后不重放写命令。

### E09 Cluster 下的业务服务组合

- **来源**：清单 N06 表 S1（跨进程同时换名、Cluster）、S2（Cluster 分页与 prune 交错、热点频道容量）、S3（Cluster、旧 writer 排空）；[B9 / C5](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/B9-C5-WINDOW-ENTRIES-ROLE-TABLE-2026-10-06.md)（activity / account 的真实 Cluster 未跑）。
- **验证什么**：account 换名释放与建角、chat 分页与 prune、activity 窗口条目，在 Cluster 与多进程下是否保持本机 Memory 与单机 Redis 的结论；activity 升级时旧 writer 排空。
- **怎么做**：多机 Cluster + 多进程，跑各服务 integration（`dataengine-env.sh test`、`redis-cluster-suites.sh`）。
- **通过标准**：（建议）同一组操作序列，结果与单机 Redis 一致；不留坏窗口条目（`Admin.MalformedWindowEntries` 为空）。

### E10 Redis 异步复制丢写与切主

- **来源**：Mirror 第 6 步 O-M6-3 与 S4a′；[Mirror 第 6 步观察](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md)（`WAIT` 只能缩小窗口，不能消除）；[REFACTOR-2026-09-25-remote-authority](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-25-remote-authority.md)；REMOTE-ACCEPTANCE RR-26。
- **验证什么**：多机主从间副本滞后时被提升，对 L2 墓碑、`snapshot_l2_tombstone_wait_replicas` / `_timeout`、`min-replicas-to-write` 部署约束的真实影响。需要真实复制延迟，不是 toxiproxy 的确定性延迟。
- **怎么做**：多机主从 / Cluster，主机间加 netem 延迟，写墓碑后立刻停主；把本机的 `TestMirrorLocalTombstoneWait*`、`redis-standalone-failover lossy` 换到多机。
- **通过标准**：新读者不读到已删除实体，水位与墓碑不回退、不复活（S4 口径）；“未复制即切主”是边界记录、不作通过条件，结果计为 `no_replicas` / `short`，可观测即可。

### E11 Mongo 跨主机副本集与 mongos

- **来源**：清单 N04 行（mongos / 主从切换中提交）与外部验证表（事务回调与提交重试、`UnknownTransactionCommitResult`、网络丢回复、唯一 / 稀疏索引与迁移消费）；Mirror 第 6 步 S5、O-M6-5；Mirror 第 5 步 §6。
- **验证什么**：副本集成员分处多主机、经 mongos 时，提交结果未知的分类、重试、唯一索引冲突；stepDown 与选举期间的 owner 提交，owner 启动撞上选举（O-M6-5 的有界重试，`7b73aabc`）。
- **怎么做**：多机副本集 + mongos，`replSetStepDown`、杀主、iptables 丢回复；跑 `-tags integration` 的 `TestRealMongo*`，结合 `dataengine-env.sh fault mongo-primary`。
- **通过标准**：最终 DAO / 版本 / receipt / outbox / checkpoint 一致，不能取消即假定未提交（清单口径）；选举期间 owner 提交成功率与最大延迟有记录，最后版本被读到（S5 口径）。

### E12 saga Mongo 步骤延迟的生产形态

- **来源**：[SAGA-MONGO-STEP-LATENCY](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SAGA-MONGO-STEP-LATENCY-2026-10-06.md)“未完成”与“复跑”。
- **验证什么**：本机 macOS 单盘上 Mongo 步骤每次尝试约 18.3ms、两次落盘提交；在 Linux + NVMe + 跨主机副本集上一次落盘提交是否约 1～5ms（文档推断，未测）；64 个以上协程的吞吐（单个 durable 的 `MaxAckPending` 缺省 256）；选项 C（乐观单事务）的实测。
- **怎么做**：在 Linux 跨主机副本集上复跑 `BenchmarkRealMongoStepThroughput`、`BenchmarkRealMongoStepLatencyBreakdown`、`BenchmarkRealMongoCommitWriteConcern`（`GOWORK=off go test -tags integration -bench ... ./framework/saga/`）。
- **通过标准**：维护者已选 A，接受“55 tps 足够”。（建议）生产形态下 8 与 32 协程的 ops/s ≥ 55，提交耗时与 p99 有记录；与本机数值并列，注明 Linux 与 macOS 不可比。

### E13 多主机强杀：owner / 锁迁移、双实例、旧回调

- **来源**：清单外部验证表（多节点 HA / 强杀）、N05 行（多节点 HA、兴趣表满载容量）、N01 行（真实失锁与进程演练）、S5 行（原生步骤跨进程强杀）；[PLAYEROWNER-LEASE-STATE-MACHINE](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/PLAYEROWNER-LEASE-STATE-MACHINE-2026-10-04.md)（无两进程真实 Redis 端到端演练，方案已被取代，演练要求转到 App 单实例锁）。
- **验证什么**：owner 迁移、双实例并存、失锁、旧回调、旧 ack、冷恢复、跨服业务在多主机上不双写、不复活；兴趣表满载容量（O4 按 consumer 计，第九轮 `23e17d81`）。
- **怎么做**：App 单实例锁 §13 的进程演练（SIGSTOP / SIGCONT / kill -9 / SIGTERM，两个 sid + 跨服赠礼）改到多主机，`second-game.sh` 两进程部署；Mirror S6（owner 转交）改为跨主机。
- **通过标准**：不双写 / 防复活、责任与回执可恢复，已有其他线实测标明来源（清单口径）。（建议）kill -9 后同 sid 新进程立即接管上一代的 Remote 实体锁（O-M6-6，`d483238e`）；saga 全部终结，无 `manual_required`。

### E14 Remote outbox“Mongo 已提交、发布前崩溃”

- **来源**：Mirror 第 6 步 §3.3、§5 第 5 行。
- **验证什么**：Remote outbox（`RecoverOutbox`）在 Mongo 提交与发布之间进程崩溃时的补发。本机两点之间没有正式注入点，S1 只覆盖“投影前崩溃的 WAL 重放”。**这一项缺的是插桩，不是外部环境**，可以在本机先做。
- **怎么做**：加进程级断点或测试缝，在 Mongo 提交后、`afterRemoteCommit` 发布前 SIGKILL owner，同 sid 重启；入口 `ROOST_MIRROR_LOCAL_ONLY=S1`。
- **通过标准**：（建议）重启后 outbox 补发，只读方在 `cached_max_staleness` + 1.5s 内读到该版本，0 违例。

## 三、容量与长稳

### E15 Mirror 长时间容量

- **来源**：Mirror 第 6 步 §5 第 3 行；Mirror 第 5 步 §6“时长与额度”；PLAN-REMOTE-POLICY-MIRROR“性能与完成标准”。
- **验证什么**：数十分钟以上持续负载下的内存与 goroutine 泄漏、JetStream 流与 durable 的长期增长、L2 TTL 周期（`DeliverAll` 重放后不复活）、兴趣租约大规模过期；吞吐、RSS、队列深度、合并加载比例、回填放大、慢消费者公平性。
- **怎么做**：专用 Linux 环境，时长单独申请（维护者要求节省额度）；沿用 `remoteentity/mirror_local_bench_integration_test.go` 与 `mirror-local.sh bench` 的口径加长时间，带堆采样。
- **通过标准**：方案没给阈值。（建议）套 C01 判据：堆每小时最低值无单调增长、pprof 前后增量不在框架、goroutine 平稳、错误 0。

### E16 Mirror 大规模扇出与目标负载

- **来源**：Mirror 第 6 步 §4、§5 第 4 行。
- **验证什么**：本机只测到 100 个只读 Manager × 10 个 key，扇出近似线性（1.8ms → 25ms）；真实部署的 key 数、热点比例、更新频率未知。每个读者每条消息一次 L2 CAS，单机 Redis 串行，要评估 Redis 是否成瓶颈。
- **怎么做**：**前置条件是维护者给出目标负载**；之后在 Linux 上把 `mirror-local.sh bench` 按同一口径扩展。
- **通过标准**：（建议）给定负载下推送完成 p99 与 L1 命中 p50 不退化（本机基线：L1 命中 p50 相对 v1.20.2 +41ns）。

### E17 Remote / DataEngine 24 小时与稳定容量

- **来源**：[REMOTE-AUTHORITY](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REMOTE-AUTHORITY-2026-09-25.md)（30 分钟 / 24 小时、多主机、真实业务流量回放、100 TPS 稳定容量未验收）；REMOTE-ACCEPTANCE（未测出最大可持续吞吐）；[nest-remote-stages](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-25-nest-remote-stages.md)；[core-nine-items](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-26-core-nine-items.md)；[DATAENGINE-PRESSURE](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/DATAENGINE-PRESSURE-2026-09-24.md)；[REFACTOR-2026-09-24-dataengine](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-24-dataengine.md)；[C01-RUNBOOK](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/C01-RUNBOOK-2026-09-30.md)。
- **现状**：C01 已在 macOS 单机通过（1 小时 × 80 TPS，另有两次 24 小时的内存平台），本机阶梯 120 TPS 通过、160 过载。都是本机限定负载，不是生产跨机 SLA。
- **验证什么**：专用静默 Linux 主机、正式 kit 装配下，Remote strict 的最大可持续吞吐、24 小时 × 80 TPS 的内存与 goroutine 有界、DataEngine 在峰谷负载下的积压。
- **怎么做**：`ROOST_REMOTE_LABEL=<新标签> ROOST_REMOTE_DURATION=24h ROOST_REMOTE_TIMEOUT=25h ROOST_REMOTE_RATE=80 bash scripts/perf/remote.sh`；容量阶梯 `ROOST_REMOTE_RATES='80 120 160' bash scripts/perf/remote-capacity.sh`；环境检查见 C01-RUNBOOK §3，用新 label，不覆盖旧结果。
- **通过标准**：C01-RUNBOOK §5——`result.json.verified` 存在；Errors = 0、Dropped = 0、ProjectionFailures = 0、FatalProjectionConflicts = 0；堆每小时最低值无单调增长，pprof 增量不在框架，tracker 在上限下持平；一致性判据与错误判据分开判定。（建议）稳定容量取 p99 回复等待 < 5s 的最高档。

### E18 Linux fsync 次数与吞吐对照

- **来源**：[REMAINING-2026-09-28](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/REMAINING-2026-09-28.md) §1.2 C03“可做部分”；OPEN-ITEMS C03；RR-20260926-16、RR-20260928-11 的“未验证项”；[DATAENGINE-BATCH](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/DATAENGINE-BATCH-2026-09-24.md)（Linux 目标硬件）；核心优化交接 §5 B29 条（“Linux 对照仍不做”是当时的决定，物理 Linux 环境到位时补）。
- **验证什么**：RR-16（async + Ack 多出的 fsync）与 RR-20260928-11（pipelined 回退 strict 等 fsync）只有 macOS `F_FULLFSYNC` 的数据（group-commit 2ms 时慢 4～9%，fsync 次数 +12～25%）。
- **怎么做**：Linux 物理机跑 `framework/nestwal` 的 `BenchmarkWALAppendStrict`、`BenchmarkWALAppendAsyncParallel`、`BenchmarkBroadcastPipelinedCommit` 与 `dataengine/engine` 的 `BenchmarkProjectorAdmissionMatrix`、`BenchmarkProjectorWALReplayAckMatrix`；修前修后按 B29 的 D / E 组选，benchstat n ≥ 10 交替。
- **通过标准**：得到 Linux 上的 fsync 次数（`Stats().Syncs`）与吞吐对照，写进两条 RR 的修复记录（REMAINING 口径）。（建议）延迟与 allocs 无新退化，代价与 macOS 同向。

### E19 物理断电与 dm-flakey

- **来源**：REMAINING-2026-09-28 §2 C03 断电部分；OPEN-ITEMS C03；ARCHIVE-2026-09-30 §5 P3；[DATAENGINE-RECOVERY](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/DATAENGINE-RECOVERY-2026-09-24.md)；REMOTE-ACCEPTANCE（磁盘损坏 / 断电不由这些用例证明）；[B6](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/B6-CLI-SIGNAL-OWNERSHIP-2026-10-06.md)（CLI 提交 / 回滚在断电、磁盘故障下未覆盖）。
- **验证什么**：RR-41 零尾截断、RR-33 fsync 失败（本机只注入了返回值）、RR-20260928-11 的 fsync；strict、pipelined 与 group-commit 窗口内真实断电后 WAL 与 checkpoint 的恢复；ext4 `data=writeback`、网络盘、VM 快照。
- **怎么做**：有 root 的 Linux 物理机或 VM，用 dm-flakey 或 `echo b > /proc/sysrq-trigger`；重开后用 `scripts/test-dataengine-generated.sh` 的三进程同一 WAL 恢复链路核对。
- **通过标准**：已确认的记录不丢，checkpoint 不越过 fsync 位置。

### E20 Sync 与 Nest 的 MMO 目标负载

- **来源**：[NEST-COMPLETION](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/NEST-COMPLETION-2026-09-24.md)；[nest-and-immediate-sync](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/REFACTOR-2026-09-24-nest-and-immediate-sync.md)；[SYNC-AOI-1000-10000](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SYNC-AOI-1000-10000-2026-09-23.md)（独占 Linux 服务端与分机负载端复测）；[SYNC-BENCHMARKS](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SYNC-BENCHMARKS.md)（NATS / JetStream 吞吐与网络传输需真实 broker 与连接）。
- **验证什么**：1000 玩家、10000 全局实体、每人约 50 可见、1% / 5% 变化、50ms SLA 的线上验收；本机 Sync 的 28 条尖峰样本是否来自开发机调度与内核竞争。Nest 微基准和本机回环 TCP 的 AOI 测试替代不了。
- **怎么做**：Linux 服务端 + 另一台负载机；`scripts/perf/sync-aoi.sh`、`scripts/perf/nest.sh`、`scripts/perf/sync-business.sh`，同配置 A/B，不挑绿色样本。
- **通过标准**：（建议）Flush 与恢复的尾延迟分别报 p99 与 max，原严格门禁不放宽；失败样本原样保留。

## 四、部署与平台

### E21 真实 systemd 的 shell 部署与回滚

- **来源**：REMAINING-2026-09-28 §2 N10；ARCHIVE-2026-09-30 §5 P3；清单 N08 行；RR-20260928-04 / 05 / 10 / 12 的“未验证项”。
- **验证什么**：`deploy/shell/install.sh` 与 `rollback.sh` 从没在 Linux 上实跑：`WorkingDirectory` 是否跟随 `current` 链接、`ProtectSystem=strict` / `ReadWritePaths` 挂载语义、`daemon-reload` 之后 `stop` 用哪份 unit 的 `TimeoutStopSec`、`Restart=on-failure` 的崩溃重启循环、`useradd` / `install -o/-g` 属主、GNU `mv -T`。
- **怎么做**：带 systemd 的 Linux（VM 即可）：真实安装 → 升级 → 自动回滚 → 手工回滚。
- **通过标准**：stats_log 落盘、`configs/data` 可读、停机时限内退出；构建 / 生成进程与子孙不遗留。

### E22 k8s 滚动停机与部署物

- **来源**：REMAINING-2026-09-28 §2 C04 k8s 部分与 N12；OPEN-ITEMS C04；清单 N08 行；[D1](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/D1-READYZ-DEGRADED-IS-READY-2026-10-06.md)（单副本 readiness）；[第十二轮 kit 批](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/DECISIONS-R12-KIT-2026-10-06.md)（探针 `timeoutSeconds: 2` 与 `/readyz` 每个 checker 1.5s）。
- **验证什么**：按生成的 k8s README（overlay + `deploy.sh`）部署 game-demo；滚动发布时在 `terminationGracePeriodSeconds` 内正常退出（无 SIGKILL）、emptyDir 可写与 sizeLimit 驱逐、ConfigMap 覆盖 + Reload、Secret 示例能让服务起来、Degraded 不摘 endpoint、`/readyz` 2s 内答完。compose 已在本机实跑，不重复。
- **怎么做**：运维提供的集群，`deploy/k8s/base` + overlay。
- **通过标准**：逐服务 Init 成功、stats_log 落盘、Secret 内嵌 config 生效；滚动发布无 SIGKILL。

### E23 distroless 镜像与多阶段构建

- **来源**：REMAINING-2026-09-28 §2 N11；ARCHIVE-2026-09-30 §5 P3；RR-20260927-34、RR-20260928-04 的“未验证项”。
- **验证什么**：真实 distroless 基础镜像与多阶段 `golang` 构建没跑过（本机按规则不拉镜像，只执行过运行阶段的 `COPY`）。
- **怎么做**：允许拉镜像的环境（CI 或维护者许可的机器），用生成的 Dockerfile `docker build`，compose 起 10 个服务。**拉基础镜像属于下载，先取得维护者许可。**
- **通过标准**：10 个服务 healthy，`configs/data` 与 `infra/observe/log` 目录正确。

### E24 仓库外生产配置的 doctor 检查

- **来源**：REMAINING-2026-09-28 §2 N13；ARCHIVE-2026-09-30 §5 P3；RR-20260927-04、RR-20260928-06 / 07；OPEN-ITEMS C04。
- **验证什么**：`roost project doctor` 读不到仓库外的生产配置；已生成工程的 prod / Secret 示例与真在用的生产配置都要手工补段（`project sync` 不更新脚手架）。本批 [DEPLOYMENT §7.1](README.md) 的旧键清理同样由部署方执行。
- **怎么做**：部署方对生产配置跑 `roost project doctor`，或把配置拷回仓内再跑。
- **通过标准**：`shutdown:*` 与各段完整。

### E25 Windows

> **当前排除**（维护者 2026-10-08）：Windows 专属问题不处理，不作为 macOS/Linux 验收门槛。下面保留 10-06 的历史验证设想，不是当前执行清单；跨平台也成立的缺陷不能因此跳过。

- **来源**：清单 N08 行（taskkill 进程树与暂存树清理）；[B6](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/B6-CLI-SIGNAL-OWNERSHIP-2026-10-06.md)（Windows 不接管信号，只做了 `GOOS=windows go vet`）；清单外部验证表（未编入 Windows 的 Unix 用例另验）；REMAINING-2026-09-28 §2 N14（真实 `core.autocrlf=true` 检出）；核心优化交接 §7 RR-20261004-13（Windows 未实跑）；ARCHIVE-2026-09-30 §5 P3（windows-compatibility 上 `TestPipelinedCommitIsProjectedOnceDurableWithoutWaitingForIdlePoll` 偶发）。
- **验证什么**：Ctrl-C 与 taskkill 进程树、暂存树清理不遗留进程；真实 autocrlf 检出上 `add transport tcp` / `add mod` / `project sync` 编辑 Secret 示例且行尾保持 CRLF；上述偶发项再现时改成确定性屏障或按平台放宽预算。
- **怎么做**：Windows 机器（或 CI 的 windows job 加一步）；`codegen/scripts/install-windows.ps1`（在 `codegen/` 目录下运行）、`roost project ...` 命令组。
- **通过标准**：构建 / 生成进程与子孙不遗留；Secret 示例被编辑且行尾保持 CRLF；偶发项再现即登记。

### E26 Linux 上的 CLI 信号、强杀与磁盘故障、离线代理

- **来源**：清单 N08 行；B6；清单外部验证表。
- **验证什么**：Linux 上跑 codegen 的 Unix 信号 / 进程树用例（race × 3）与具名 skip 里依赖环境的部分；生成器提交窗口在 SIGKILL、磁盘满或磁盘故障时的暂存树与回滚；离线 GOPROXY 或私有模块环境。
- **怎么做**：Linux 机器；磁盘故障用小容量 tmpfs 或 dm-error 注入。
- **通过标准**：（建议）SIGINT / SIGTERM 中断后暂存树被清、无孙进程残留；SIGKILL 后下次运行能识别并清理残留暂存树，或报明确错误。

### E27 hotcode 真实插件加载

- **来源**：清单 N10 行（Linux / Windows 真实插件加载）。
- **验证什么**：hotcode 注册 / 替换的真实 `.so` 加载（macOS 已跑 H9～H13）在 Linux 上的并发可见性、旧请求生命周期、回滚。Windows 专属报错验证按维护者 2026-10-08 决定排除。
- **怎么做**：Linux 机器，复用 [N10 第二批](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/REVIEW-2026-10-06-noncore-n10b.md) 的 H9～H13 用例。
- **通过标准**：（建议）Linux 与 macOS 行为一致。

### E28 生产流量下竞态窗口的实际频率

- **来源**：REMAINING-2026-09-28 §2 N15；RR-20260927-22、RR-20260927-28 的“未验证项”。
- **验证什么**：这两条 RR 的竞态窗口只用测试缝确定性进入过，生产里的真实频率未知。
- **怎么做**：在对应分支加计数或日志，在生产或准生产流量下观察（流量由部署方提供）。
- **通过标准**：（建议）给出观察时长内每个窗口的命中次数，再由维护者判断是否加固。

## 不在本清单

- **本机能做、不算外部**：saga 新旧协调器混跑与原生步骤跨进程强杀混跑（需要旧版本二进制，不需要外部环境；[B1](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/B1-SAGA-COMPLETION-INCARNATION-2026-10-06.md)、[saga 方向](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/feature/SAGA-DIRECTION-STEP-TRANSITION-AND-MONGO-INBOX-2026-10-06.md)、清单 S5 行）；C03 的 Docker Linux 容器 fsync 对照（E18 的本机替代，口径是“虚拟化、非物理机”）。E14 也可以在本机先做。
- **没有可判定内容的**：B 线非 RR 待办（外部幂等与对账、HA、Match 归档、生产迁移，ARCHIVE §5 P3 第 13 条只记录不评判）。
- **待维护者的方向判断**不是外部验证，不列；截至本文，维护者决定表没有未决项，见 [DECISIONS-PENDING](https://github.com/tjbdwanghaibo/roost-core/blob/9d955fb0df35f082dfc9be24c2f3a4524d437067/docs/review/DECISIONS-PENDING-2026-10-05.md) 文首“当前总状态”。
