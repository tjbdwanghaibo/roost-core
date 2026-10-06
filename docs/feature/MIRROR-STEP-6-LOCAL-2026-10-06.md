# RemotePolicy Mirror 第 6 步的本机替代：私有依赖进程上的故障与性能（2026-10-06）

依据：[下一轮规划](../review/NEXT-ROUND-PLAN-2026-10-06.md) 第 3 项，维护者原话“mirror 的第 6 项看下能否在本机用别的方式替代”。
前置：[第 5 步](MIRROR-STEP-5-2026-10-06.md)（§6 是第 6 步原本需要的外部条件）、[第 4 步](MIRROR-STEP-4-AND-O4-2026-10-06.md)、[方案](../review/PLAN-REMOTE-POLICY-MIRROR.md)“性能与完成标准”。
基线：v1.21.0 之后的 `7d837dab`（tag v1.21.0 → `4881f2b7`）；分支 `mirror6`。本机：macOS（Darwin 25.5，Apple M5，10 核），Go 1.27.0，brew 二进制（nats-server、mongod / mongosh、redis-server / redis-cli、toxiproxy-server）。

## 1. 结论

- **能替代的都做了**：7 类故障在本机私有进程上两进程（owner 子进程 + 只读服务子进程，基于生成工程 `testdata/remoteflow`）跑通，每个场景断言“不回退、不复活、有界收敛”并记录收敛时间；`-race` 下整套 108s 通过，0 违例。
- **发现并修复一个缺陷**：[RR-20261006-01](../bug/RR-20261006-01.md)（P2）——删除 Managed Remote 实体（strict）已提交到 Mongo，确认却报“身份不符”，删除的发布（L2 墓碑 + 推送）整段被跳过，只读方在 L2 TTL（5 分钟）内一直读到已删除的实体。先红后绿，[修复记录](../bugfix/RR-20261006-01.md)。
- **观察 5 条**（§3.8），其中需要维护者判断的是 O-M6-1（owner 重启后兴趣表为空、推送最长停 15s）与 O-M6-3（Redis 未复制即切主时新读者在陈旧上限内读回已删除实体，属于 B2 的前提）。
- **性能**：同机同负载对照 v1.20.2 与当前源码（n=6，benchstat），见 §4。
- **替代不了的**：Linux 内核网络行为、跨主机真实分区、长时间容量与长稳，见 §5 外部验证清单。

## 2. 环境：全部私有进程

`scripts/mirror-local.sh`（默认不在 CI 跑；`test` / `bench` 才起进程）复用 `kit/scripts/integration/lib`（根目录校验、端口平移、按命令行认领 pid），另加 Redis 副本与 Cluster：

| 组件 | 进程 | 端口（偏移 20000） | 故障动作（`scripts/mirror-local.sh fault …`） |
| --- | --- | --- | --- |
| NATS JetStream | 3 节点集群 | 34222-34224（路由 36222-、监控 38222-） | `nats-kill N`（SIGKILL）、`nats-stop N` / `nats-cont N`（SIGSTOP 静默断线）、`nats-start N` |
| Mongo | 3 节点副本集 `roost-it`（缓存 0.5GB / 节点） | 47117-47119 | `mongo-stepdown`（`replSetStepDown 10`）、`mongo-settle`（等最高优先级节点选回） |
| Redis 单机 | 主 36379 + 副本 36380，客户端经 toxiproxy 固定入口 46379 | 36379 / 36380 / 46379 | `redis-standalone-failover graceful`（等偏移量追平、提升副本、代理改指新主、旧主降为副本）；`redis-standalone-pause-replica` + `redis-standalone-failover lossy`（副本 SIGSTOP 并断开复制连接，旧主 SIGKILL，副本未追平即提升） |
| Redis Cluster | 3 主 3 从，node-timeout 1s，AOF everysec | 37400-37405 | `redis-cluster-kill-master <键>`（SIGKILL 该键槽位的主，等副本接管，旧主以副本身份拉起并追平）、`redis-cluster-pause-replica <键>`、`redis-cluster-heal` |
| toxiproxy | 私有实例 | API 38474，NATS 代理 44222-44224 | 用例直接调它的 HTTP API（latency / 禁用代理 / timeout 毒） |

隔离：根目录 `$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it`（缺省在 `$TMPDIR` 下，本次在 scratchpad），偏移缺省 20000；拒绝 `~/.roost-it` 与偏移 0 / 1000；启动前清掉继承的 `ROOST_DATAENGINE_IT_*` 变量，不读共享环境的 env.sh。`test` / `bench` 结束时 `clean`：先 SIGCONT 被暂停的进程，再停全部进程、核对根目录下与本偏移的 toxiproxy 都没有残留进程，然后删根目录（本次三次运行都输出 `no residual processes`）。用例自己的 Mongo 库在结束时 drop；L2 键与 JetStream 流随整个私有环境一起删除。

## 3. 故障场景

### 3.1 场景矩阵

正式运行：`ROOST_MIRROR_LOCAL_HOME=<目录> scripts/mirror-local.sh test`（生成工程经 `scripts/test-remote-generated.sh` 用正式 DAO / Entity 生成器生成，`-race`），2026-10-06 一次完整运行全部 PASS（107.5s）。收敛时间是“owner 开始提交 → 只读方第一次读到”（同一台机器的墙钟，含 Nest / WAL / Mongo 提交）；重放补发没有 Request，从 owner 重启就绪起算。

| # | 注入 | 断言 | 结果与收敛时间 |
| --- | --- | --- | --- |
| S1 | owner 第二笔写在投影前被拖住（WAL 已持久、Mongo 未提交、未发布），owner SIGKILL；同一 WAL 目录、同一 sid 重启 | 停机期间只读方仍读已确认的 v1；重启后重放补发 v2；重启后的 owner 冷加载公会、下一笔是 v3 | 重启就绪 574ms；v2 在就绪后 1.9s 读到；v3 3.0s 读到（都是陈旧上限 3s 回源，见 O-M6-1）；0 违例 |
| S2a | 只读方连着的 nats-2 SIGKILL（开着发现，自动连别的节点） | 推送恢复、durable 续投 | 本次流 leader 不在 nats-2：owner 无感，提交 → 读到 31ms。开发期一次运行 leader 恰在 nats-2：owner 4 次尝试 6.1s 才写成（首错 `nats: no response from stream`，回复为结果未知，见 O-M6-2），只读方随即读到 |
| S2b | 另一只读方连 nats-3，nats-3 SIGSTOP 8s（静默断线，ping 20s × 2 才能发现）后 SIGCONT | 推送停住期间按 `cached_max_staleness`（3s）回源 | 2.98s 读到（L2 重新确认）；SIGCONT 后推送 34ms。第二次完整运行流 leader 恰在 nats-3：owner 6 次尝试 10.6s 才写成（首错 `nest: sync canceled context deadline exceeded`），写成后 1.4s 只读方读到（陈旧上限回源），SIGCONT 后 55ms |
| S3 | 只读方只经 3 个 toxiproxy 代理（不用发现的地址），陈旧上限 60s（读到新版本只能来自推送） | 延迟、分区、丢数据后推送续投 | 基线 30ms；latency 200ms → 239ms；全部代理断开 5s 期间提交 → 恢复后 269ms 读到（共 5.3s，DeliverNew durable 续投）；timeout 毒丢数据 3s → 共 4.1s；之后 33ms |
| S4a | Redis 单机 graceful 切主；提交；删除；再切主；新起一个只读方（L1 空）读 | L2 水位与墓碑不回退、不复活 | 切主后提交 40ms 读到；删除 24ms 读到“不存在”；第二次切主后新读者读到“不存在”；0 违例（**修复 RR-20261006-01 之前删除不发布，本场景红**） |
| S4a′ | 单机**未复制**即切主（副本暂停并断开复制，墓碑只写在旧主） | 边界记录，不作通过条件 | 新读者约 2.9s（145 次读）读到已删除的 v1，之后回源 Mongo 得“不存在”；原读者（L1 有墓碑）0 次读到（O-M6-3） |
| S4b | Redis Cluster SIGKILL 负责该键的主（副本接管）；提交；删除；再杀新主；新读者读 | 同 S4a | 切主后提交 33ms 读到；删除 38ms；新读者读到“不存在”；0 违例 |
| S5 | owner 每 100ms 提交（共 60 笔），只读方每 50ms 直接调只读 Mongo loader，期间 `replSetStepDown` | 选举期间 owner 提交与权威回源的行为；最后收敛 | owner 60/60 成功、最大延迟 48ms；权威回源 164/164 成功；最后版本 v61 读到（推送先于 owner 回复到达，−8ms）。mongo-1 让位期满选回又一次选举，2.5s 后稳定 |
| S6 | owner A 把所有权转给 B（另一 sid、另一 WAL），B 冷加载公会后提交；A 再写 | 只读方读到 B 的版本；A 被 fence | B 就绪 268ms，v3 34ms 读到；A 的写 `writer fenced`；B 第二笔 27ms |
| S7 | 只读方 SIGKILL，停机期间 owner 提交 v3，同一 sid 重启 | 首载与兴趣恢复 | 首读 v3 59ms（经 L2）；重启后推送 30ms；首载缓冲 0 次（首读没有走权威加载，缓冲路径由第 4 步单元矩阵覆盖） |

每个只读方的计数（`STATS`）：所有场景 `errors=0`、`violations=0`；权威回源次数（只读 Mongo loader 外包一层计数）除 S4a′ 的新读者 1 次外均为 0——L2 始终在，回源都在 L2 完成。

### 3.2 断言怎么做

只读子进程（kit `SyncBusMod` + `RemoteMirrorMod` + 生成的 `NewGuildSummaryReader`，与第 5 步样例同一装配）后台每 5ms 做一次 Cached 读，在本进程里核对：

1. **不回退**：读到的版本不低于此前读到的最大版本。
2. **不复活**：编排宣布删除（`PREDEL`，推送可能先于 owner 的回复到达）或删除确认之后、本进程读到过“不存在”，再读到存在即违例；删除之后才起的只读方从启动就知道删除时刻。
3. **有界收敛**：owner 确认版本 v（或删除）的时刻 T 之后，超过 T + `cached_max_staleness` + 1.5s 仍读到低于 v（或仍存在）即违例。陈旧上限内读到旧值是契约允许的。

违例打印 `MIRROR VIOLATION …`，编排在场景结束时要求 0 条；边界场景（S4a′）切到只计数模式。

### 3.3 各场景要点

- **S1 / outbox 补发**：拖住的是投影（`holdingStore` 包住 `MongoStore.ProjectFenced`，与 reject 样例同一注入点），所以补发走的是 WAL 重放 → 投影 → Remote 提交 → `afterRemoteCommit` 发布。“Mongo 已提交、发布前崩溃”的 Remote outbox（`RecoverOutbox`）这次没有单独注入：两个时间点之间没有正式的注入点，只能靠插桩，本轮不做。
- **S2**：流是三副本（`Replicas: 3`，内存存储）；owner 受不受影响取决于流 leader 落在哪个节点，所以两次运行的 owner 侧数字差很多，只读方的收敛都在陈旧上限内。
- **S3**：toxiproxy 的 `timeout`（timeout=0）毒丢弃数据而不关连接，NATS 协议流被破坏后连接重建，durable 把未确认的消息重投。分区用禁用代理（连接被关闭、重连被拒）。
- **S4**：owner 与只读方共用同一 L2 前缀；Cluster 下锁键带 hash tag `{m6}`（多键 Lua 同槽），L2 快照键不带（单键脚本）。
- **S5**：Go 驱动的可重试读写吸收了 stepDown，owner 与权威回源都没有看到错误；owner **启动**撞上选举会失败（O-M6-5）。
- **S6**：B 是新 sid，兴趣主题的 DeliverAll durable 从头重放，B 一启动就知道只读方的兴趣，推送立即生效（与 S1 对照，见 O-M6-1）。

### 3.4 发现的缺陷

[RR-20261006-01](../bug/RR-20261006-01.md)（P2，已修复）：生成的 Managed `Guild` 在自己的 strict handler 里删除，调用方得到 `remote entity persistence outcome is indeterminate: Guild: remote acknowledgement identity mismatch`；Mongo 已删，L2 仍是删除前的 v3，只读方 8 秒后仍读到 v3。根因：删除随事务在准入后清空实例（ID 归零），`acknowledgeRemoteCommit` 仍把删除提交交给登记里的这个实例，生成代码按身份拒绝，`afterRemoteCommit` 在确认处返回、发布被跳过。修复：实例身份与提交不符时摘掉并视为确认完成，照常发布。单元红测（`remoteentity/remote_delete_ack_promises_test.go`）修前的错误文本与真实链路逐字一致。

### 3.5 观察（不是确定缺陷）

- **O-M6-1 owner 重启后兴趣表为空**：兴趣是广播软状态；同 sid 重启的 owner 从 durable 游标续读兴趣主题，之前已确认的兴趣消息不重放，表是空的；只读方的续租在剩余不足一半时才发（缺省 TTL 30s → 最长 15s），这段时间 owner 不推送，只读方只靠陈旧上限回源（S1：v2 1.9s、v3 3.0s，都卡在 3s 陈旧上限）。正确性不受影响，新鲜度退化到陈旧上限。新 sid 的 owner（S6）反而从头重放、立刻知道兴趣。可选方向：owner 启动时广播一次“请求续租”，或兴趣主题按 subject 取最新（DeliverLastPerSubject）；属于协议调整，交维护者判断。 **后续**：维护者第十轮按推荐实施（owner 启动广播续租请求），见 [O-M6-1 / O-M6-3 实施记录](MIRROR-M6-OBSERVATIONS-2026-10-06.md)。
- **O-M6-2 推送与写回复耦合**：JetStream 流 leader 所在节点被杀 / 暂停时，owner 的写在 leader 重新选出之前一直失败（6～10.6s，三次里出现；首错为 `nats: no response from stream` 带结果未知，或 Request 截止），其中 Remote 提交可能已落 Mongo、只是发布失败，回复是“结果未知”，本用例的朴素重试每次都是新的一笔写。这是既有契约（结果未知不得当作未提交），业务侧应按结果未知处理（查询或等 finalizer 的持久结论），而不是盲目重试；只读方经 L2 在陈旧上限内收敛。记录在案，不改。
- **O-M6-3 Redis 未复制即切主**：墓碑只在旧主上，副本被提升后 L2 回到删除前的版本；L1 里有墓碑的读者不受影响，L1 空的新读者在陈旧上限（3s）内把已删除的实体读成存在，之后回源 Mongo 得“不存在”。这是 B2“L2 为水位权威”的前提（Redis 异步复制会丢已确认写，`TestRealRemoteRedisClusterUnreplicatedFence` 同一类）。可选缓解：L2 写后 `WAIT` 副本、`min-replicas-to-write`，或陈旧上限内也带权威校验——都是设计 / 部署取舍，交维护者。 **后续**：维护者第十轮按推荐实施（只对墓碑写入加 `WAIT`），见 [实施记录](MIRROR-M6-OBSERVATIONS-2026-10-06.md)；S4a′ 这种副本已断开的情形 WAIT 也挡不住，计为 no_replicas / short。
- **O-M6-4 静默断线靠 ping 发现**：SIGSTOP 节点后客户端要 ping 20s × 2 才断开重连；期间推送停住，读取在陈旧上限后回源（S2b 2.98s）。缩短 `PingInterval` 是部署参数，不改。
- **O-M6-5 owner 启动撞上 Mongo 选举即失败**：开发期一次，S5 之后 mongo-1 选回期间起 owner，`EnsureInfrastructure` 返回 `InterruptedDueToReplStateChange`，子进程启动失败（fail-fast，由进程管理重启）。只读方不受影响。用例在 S5 结束时等选举稳定（`mongo-settle`）。

## 4. 性能（同机前后对照）

`ROOST_MIRROR_LOCAL_BASELINE=<v1.20.2 的 detached worktree> scripts/mirror-local.sh bench <目录>`：同一份 `remoteentity/mirror_local_bench_integration_test.go` 复制到基线上，当前源码与基线交替各跑 6 次（`-benchtime 1x -count 1`，每次内部固定工作量），benchstat 出对照。只用两个版本都有的入口（`Assemble` / `Assembly.Start` / `Manager.ReadRemoteSnapshot` / owner 提交后的 `afterRemoteCommit`）；只读方在两个版本里都是装配的 Manager——v1.20.2 经 `BindSync` 普通订阅（DeliverAll durable），当前经 `SnapshotClient` 可确认订阅（DeliverNew durable）。快照 256 字节；JetStream 单副本内存流；Redis 单机直连；Mongo 三节点副本集。

2026-10-06 一次运行（`goos: darwin`、`cpu: Apple M5`；中位数 ± 置信区间，n=6，交替运行）：

| 基准 | 口径 | v1.20.2 | 当前（v1.21.0 + 本轮） | benchstat |
| --- | --- | --- | --- | --- |
| Read/L1Hit（已确认、陈旧上限内） | 平均 / p50 / p99 每次读 | 720.5ns / 667ns / 1041ns | 731.6ns / 708ns / 959ns | 平均 ~（p=0.18）；p50 +6.15%（p=0.002，约 41ns）；p99 ~ |
| Read/L2Fetch（L1 无、L2 有） | 平均 / p50 / p99 | 24.60µs / 23.50µs / 50.79µs | 24.97µs / 23.73µs / 52.60µs | 均 ~ |
| Read/Authority（L1 / L2 都无，Monotonic 回源 Mongo） | 平均 / p50 / p99 | 125.6µs / 120.4µs / 270.5µs | 128.4µs / 121.9µs / 286.5µs | 平均 ~（p=0.093）；p50 +1.28%（p=0.041）；p99 ~ |
| 回源次数 | 权威加载 / 每次读 | L1Hit 0、L2Fetch 0、Authority 1.000 | 同左 | 相等 |
| Push 扇出 1 读者 × 10 key | 一轮全部读到（平均 / p50 / p99） | 1.841ms / 1.777ms / 2.335ms | 1.852ms / 1.808ms / 2.721ms | 均 ~ |
| Push 扇出 10 读者 × 10 key | 同上 | 4.036ms / 4.017ms / 4.762ms | 4.063ms / 4.009ms / 5.090ms | 均 ~ |
| Push 扇出 100 读者 × 10 key | 同上 | 26.06ms / 25.89ms / 33.04ms | 25.42ms / 24.94ms / 35.26ms | 均 ~ |

结论：Mirror 第 1～5 步（只读契约与唯一读出口、SnapshotClient、可确认订阅与首载缓冲、兴趣代际）加上本轮修复，在读延迟、回源次数与推送扇出上与 v1.20.2 没有可分辨的差别；唯一显著项是 L1 命中 p50 多约 41ns（+6%；原因未逐项拆分，读路径经第 2～3 步的统一读出口与 SnapshotClient 委托多了一层），平均值与 p99 不显著。扇出从 1 到 100 个读者，一轮 10 个 key 的完成时间约 1.8ms → 25ms，近似线性（每个读者每条消息一次 L2 CAS，单机 Redis 串行）。

不可比 / 只有当前值的部分（macOS 绝对值，不能外推到 Linux）：故障场景的收敛时间见 §3.1；`RemoteMirrorMod` 只读装配（不建写 backend）v1.20.2 没有，读路径与上表的 Manager 读是同一个 `SnapshotClient`。

## 5. 外部验证清单（本机替代不了）

| 项 | 为什么本机替代不了 | 需要的条件 |
| --- | --- | --- |
| Linux 内核网络行为 | macOS 的 TCP 栈（keepalive、重传、RST、backlog）、调度与 epoll / kqueue 不同；toxiproxy 是用户态代理，模拟不了内核层丢包、乱序、半开连接的真实计时 | Linux 主机，`tc netem` / iptables 注入丢包、延迟、乱序；同一套 `scripts/mirror-local.sh` 的场景 |
| 跨主机真实分区 | 本机所有进程共用回环接口，“分区”只是代理断开；没有网关、ARP、MTU、跨机时钟偏差 | ≥3 台主机分别放 NATS / Redis / Mongo 与 owner / 只读服务，iptables 双向 / 单向隔离；时钟偏差下的陈旧上限判定 |
| 长时间容量与长稳 | 维护者要求节省额度，本轮规模适中（每场景秒级、基准 6 轮）；没有做数十分钟以上的持续负载、内存 / goroutine 泄漏、JetStream 流与 durable 的长期增长、L2 TTL 周期、兴趣租约大规模过期 | 专用环境、单独批准的时长；方案“性能与完成标准”的完整口径（吞吐、RSS、队列深度、合并加载比例、回填放大、慢消费者公平性） |
| 大规模扇出与真实负载 | 本机扇出到 100 个只读 Manager、10 个 key；真实部署的 key 数、热点比例、更新频率需要维护者给出目标负载 | 维护者给出目标负载后在 Linux 上按同一基准扩展 |
| Remote outbox 精确注入 | “Mongo 已提交、发布前崩溃”两点之间没有正式注入点（S1 覆盖的是投影前崩溃的 WAL 重放） | 需要插桩或进程级断点，评估后再做 |

## 6. 验证（`GOWORK=off`）

- 先红后绿（RR-20261006-01）：`go test -count=1 -run TestRemoteDeleteCommitPublishesItsTombstoneAfterTheInstanceIsCleared ./remoteentity` 修前红（`Guild: remote acknowledgement identity mismatch`，与真实链路逐字一致）、修后绿；真实链路 S4a / S4b 修前红（探针：删除 8s 后只读方仍读到 v3，L2 无墓碑）、修后绿。
- `gofmt -l` 空；`go build ./... && go vet ./...`；`go vet -tags integration ./remoteentity`；生成工程（正式 DAO / Entity 生成器生成）`go vet` 与编译通过。
- `go test -race -count=3 ./remoteentity` 通过；根包 `go test -count=1 .` 通过（`TestGlobalEnvironmentOperationsHoldTheAcceptanceLock` 起初因脚本注释提到隔离环境脚本名而误报，注释改写，脚本本身不调用共享环境的入口）；`go test -count=1 ./codegen/...` 通过；`go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` 无违例（只改了 remoteentity，按要求可不跑，顺带跑了）。
- 真实依赖（全部私有进程）：`scripts/mirror-local.sh test` 完整运行两次 PASS（`-race`；rebase 前 107.5s，rebase 到 `a26c9454`（含业务时间只许前进与 configdata 大小写敏感）之后 111.7s），`scripts/mirror-local.sh bench`（基线 v1.20.2 交替 6 轮）完成；每次运行结束都核对“无残留进程”并删除根目录。
- 未改生成形状（模板、生成器、catalog 配置模板都没动）：没有跑 `go generate` porcelain 与 game-demo。

## 7. 复跑

```bash
# 故障场景（起私有环境 → 生成工程两进程场景 → 清理并核对无残留进程）
ROOST_MIRROR_LOCAL_HOME=/abs/path scripts/mirror-local.sh test
# 只跑某个场景：ROOST_MIRROR_LOCAL_ONLY=S4 …（逗号分隔多个：S1,S7）；调试保留环境：ROOST_MIRROR_LOCAL_KEEP=1 …，之后 scripts/mirror-local.sh clean
# 性能对照（基线放在一个 v1.20.2 的 detached worktree）
git worktree add --detach /abs/base v1.20.2
ROOST_MIRROR_LOCAL_HOME=/abs/path ROOST_MIRROR_LOCAL_BASELINE=/abs/base scripts/mirror-local.sh bench /abs/out
```

用例文件：`codegen/internal/entity/testdata/remoteflow/mirror_local_test.go`（`ROOST_MIRROR_LOCAL=1` 才运行，否则 skip）、`remoteentity/mirror_local_bench_integration_test.go`（integration tag，`ROOST_MIRROR_LOCAL=1` 才运行）。原始输出不入库（scratchpad 的 `m6-artifacts`），关键数字已抄在本文。
