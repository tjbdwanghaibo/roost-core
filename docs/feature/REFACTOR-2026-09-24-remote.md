# Remote 优化：事务完成链路与验收

> 最新验收见 [容量、30 分钟长稳与集群故障](REMOTE-ACCEPTANCE-2026-09-24.md)。RR-24/25 已修复；RR-26 确认未复制写丢失后 fence 复用，生产 HA 准入尚未通过。下文保留前三轮当时的范围与结论。

日期：2026-09-24；基线 `967bc69` 加已完成 DataEngine 的当前工作树。用户已授权开始 Remote 优化。先落地本轮有证据的事务跟踪/回收热点与缺陷，再补真实依赖验收，不把历史报告中的建议直接当作未修复事实。

## 本轮目标与结构

Remote Entity 仍保留 `remoteentity` 一个实现包；Entity 保留协议，Kit 保留装配。Mongo 权威提交 → Redis L2/本地 L1/快照发布 → transaction tracker → finalizer 交接是本轮主链路。既有所有权、分布式锁与线协议保持。

当前 `transaction_manager.go` 约 1072 行，同时承载状态、finalizer、投影、snapshot/interest、事务等待和版本等待。本轮仅把事务跟踪/等待提取到同包 `transaction_tracking.go`，其余主线保留，避免机械拆出过多文件或包。

| 当前 | 目标/职责 | 依赖变化 |
| --- | --- | --- |
| transaction_manager.go 中 tracker、track/complete/status/flush | transaction_tracking.go：事务跟踪、终态缓存、等待者生命周期 | 同包移动，公开签名不变 |
| transaction_manager.go 的 map 全扫描淘汰 | 按首次完成顺序维护终态队列，满载删除队首、TTL 删除已过期前缀 | 不引入后台线程或新包 |
| assembly.go Stats 全量计数 | map 数量减终态队列数量 | 在原 txMu 内读，保持一致快照 |

队列只索引已关闭 tracker，pending 永不淘汰；等待者持有 tracker 指针，淘汰只影响历史索引。采用包含单调时间的完成时刻，终态更新不重排首次完成顺序。满容量需一次逐条 TTL 回收时仍与过期条数线性相关，摊销消除每次准入对全部缓存的扫描。

## 缺陷候选与实施顺序

1. FlushRemoteAll 目前只保存 ID；等待第一笔时其他笔完成并淘汰，后续再按 ID 查找可能新建等待或超载。先用通道屏障稳定复现，再保留调用时的 tracker 集合，不把后来新准入事务纳入此次 Flush。
2. ApplyRemoteCommits 在校验前占用 tracker；不合法输入返回后遗留未关闭条目，持续侵占上限。先构造小容量复现，校验/不可编码/空批次在准入前拒绝，已有同 ID tracker 不受坏请求破坏。
3. 运行满容量 64/1024/8192/65536 的准入+完成基准和 Stats 基准，记录改前数据。随后实现有序终态索引，覆盖 TTL、重复完成、容量全 pending、并发与淘汰等待者，再复跑同参数基准。
4. 真实 Mongo/Redis 下验收 DataEngine + Remote 原子落库、快照发布失败重放、重新创建 Manager、L2 读取与版本冲突；说明哪些实际链路被覆盖。相关包 race、vet、文档、索引同步。

发现的 bug 分别记录问题/修复证据，沿 RR 编号。不会修改历史坏 WAL、下调持久化级别或将 Remote 直接并入普通多 DAO 批量；事务缓存基准也不等同于实际 Remote 业务吞吐。

## 风险与边界

终态索引和 map 必须同锁更新，清除队首的 next 防止旧等待者持有整个队列。Indeterminate 是本次等待的关闭状态，仍允许后续 CommitStatus 更新为权威终态；不改变历史契约。真实所有权跨节点/进程、故障与业务负载的完整截止判断需依据执行证据，不能由微基准推出。

## 实施结果

本轮四步已完成，未发布。仍为一个 remoteentity 包，transaction_manager.go 由 1072 行降至 899 行，事务等待/跟踪集中在 transaction_tracking.go（215 行）。采用首次完成顺序的侵入式链表，避免额外队列对象；队列与 map 共用 txMu。TTL 仍在容量压力时清理，未引入定时器或 goroutine。

| 编号 | 实际问题 | 验证 |
| --- | --- | --- |
| [RR-16](../bug/RR-20260924-16.md) / [修复](../bugfix/RR-20260924-16.md) | FlushAll 按 ID 重查已淘汰终态，错误超载或等待 | 通道屏障修前红、修后绿 |
| [RR-17](../bug/RR-20260924-17.md) / [修复](../bugfix/RR-20260924-17.md) | 坏输入先占 pending 容量，阻止合法请求 | 容量 1 复现；坏输入不改已有结果 |
| [RR-18](../bug/RR-20260924-18.md) / [修复](../bugfix/RR-20260924-18.md) | checksum 命名类型无法编码到 Redis，L1 降级掩盖 L2 未写入 | 真实驱动修前报错、修后 L2 与冷 Manager 可读 |

持久化格式、所有权协议、Nest Guard 时序不变。空 Remote 批次明确拒绝，是准入校验的行为收紧。

## 事务缓存微基准

环境 Apple M5 / darwin arm64 / Go 1.27.0，`-cpu=4 -benchtime=200x -count=3 -benchmem`，取三个样本中位数。每组先填满已完成事务，setup 不计时；每次准入一个新 ID 并完成。TTL 为 1h，无后台 finalizer 或外部存储。

| 缓存容量 | 改前 ns/op | 改后 ns/op |
| --- | ---: | ---: |
| 64 | 2,106 | 159.2 |
| 1,024 | 23,551 | 168.3 |
| 8,192 | 98,279 | 346.9 |
| 65,536 | 761,422 | 201.2 |
| Stats（65,536 条） | 350,016 | 11.88 |

准入+完成保持 2 allocs/op，224 → 272 B/op；增加 48 字节保存身份、链指针和单调完成时刻，65,536 条约增加 3 MiB 对象内存。Stats 零分配。满容量每次准入和 Stats 不再扫描整个 map；一次清理大量过期记录仍需逐条回收。

这是单 goroutine 的缓存热点数据，200 次短样本只证明该路径成本变化，不能换算 Remote 端到端 TPS、锁争用或数据库容量。全部 pending 时仍拒绝超载，不丢失未完成事务。

```sh
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test ./remoteentity -run '^$' \
  -bench '^BenchmarkRemoteTracker' -benchmem -cpu=4 -benchtime=200x -count=3
```

原始日志 `/tmp/roost-remote-before.log`、`/tmp/roost-remote-after.log`，关键数据已归档本表。

## 实际执行的验收

`kit/dataengine/remote_integration_test.go: TestRealDataEngineRemotePublicationAndWALRecovery` 使用隔离 Mongo 副本集、真实 Redis、文件 WAL：每事务两个本地 DAO，加两个 Remote Entity 各两个 DAO，共六文档、四份快照。

1. Mongo 成功后注入快照传输失败：六文档为 v1、Remote outbox 与 WAL 保留，不误判永久冲突。
2. 重建 Manager，RecoverOutbox 发布四份快照；重开 WAL 后幂等完成，不重复递增版本。
3. 下一事务已落库并发布，注入 checkpoint 失败；再重建 Manager/重开 WAL，六文档仍为 v2，WAL 和 Remote outbox 清空。
4. 直接读取四份 Redis L2；未参与重放的新 Manager 冷读取也能看到 v2。
5. 最后一个 Remote Entity 版本冲突：前面的本地 DAO 和另一个 Remote Entity 全部回滚；六文档保持 v2，无错误发布或新增事务 marker，冲突 WAL 保留。

该场景 `-race` 通过，日志 `/tmp/roost-remote-real-final.log`。测试使用专属 Mongo database、Redis key 前缀和临时 WAL，只清理自身数据。启动依赖见 [DataEngine 恢复夹具](DATAENGINE-RECOVERY-2026-09-24.md)。

```sh
source /tmp/roost-dataengine-it/env.sh
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test -race -tags=integration ./kit/dataengine \
  -run '^TestRealDataEngineRemotePublicationAndWALRecovery$' -count=1 -v
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test -race \
  ./remoteentity ./entity ./cache ./redis/... ./dataengine/... ./nest \
  ./kit/dataengine ./kit/nest ./kit/remoteentity
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go vet -tags=integration \
  ./remoteentity ./entity ./cache ./redis/... ./dataengine/... ./nest \
  ./kit/dataengine ./kit/nest ./kit/remoteentity
```

10 个有测试的包 race 通过，kit/remoteentity 无测试；vet 通过。首次批量 race 中只有 redis/driver 因沙箱禁止本机临时端口 bind 失败，允许本机监听后单独重跑通过。日志 `/tmp/roost-remote-race-final.log`、`/tmp/roost-remote-driver-race.log`、`/tmp/roost-remote-vet-final.log`。新增覆盖还包括首次完成顺序、Indeterminate 更新、TTL 恰好过期、保留 pending 和旧等待者不保留整个链表。

## 截止边界

DataEngine 已授权的本地优化与前轮验收已完成，本轮完成 Remote 事务缓存、等待正确性和 DataEngine → Remote 持久化/发布恢复这一批优化。整个 Remote 尚不能标为最终截止：本轮直接构造正式 CommitRecord，发布传输与 checkpoint 故障为注入；未执行生成的 Remote Nest handler 到跨进程广播的完整业务压测，也未补跑多节点 ownership 迁移/抢占/续租和网络分区矩阵。后续优先验收这些边界，再依据实际 profile 决定性能改动。

图谱采用 Verify，起始 generation 为 `2026-09-24T13:46:18Z`，相关生产源码覆盖无记录缺口；测试和文档以实际源码与执行结果为准。启发式调用边不作为跨包接口分派的完整证明。

实施后索引刷新至 `2026-09-24T14:31:39Z`（18,069 nodes / 142,917 edges）；六个改动 Go 文件均 metadata_match、无记录缺口。remoteentity scope 无未覆盖记录；仓库其他三处模板 parse_partial 与本轮无关。docs 按配置不建索引，直接读取并检查新增文档链接；`git diff --check` 通过。

## 第二轮：启停与锁代际

2026-09-24 继续实施。基于上一轮剩余边界，沿 Assembly → replicator/finalizer 和 Manager ownership → versioned lock → Redis Lua 复核。仍保留 remoteentity 一个包，修复集中在 assemble.go、versioned_lock.go、versioned_lock_lua.go，没有新增生产包或通用锁抽象。生命周期所有权在 Assembly 内，Redis 状态与本地锁状态分别按 token 隔离。

| 问题 | 改动 | 证据 |
| --- | --- | --- |
| [RR-19](../bug/RR-20260924-19.md) / [修复](../bugfix/RR-20260924-19.md) | 重复启动幂等、失败重试保留已绑定依赖、启停串行等待可取消、停机超时保留资源 | 四个原始失败稳定复现；六个生命周期场景 race 通过 |
| [RR-20](../bug/RR-20260924-20.md) / [修复](../bugfix/RR-20260924-20.md) | 迟到的解锁回复只更新同一 token 的本地持锁/版本 | 成功与拒绝两种旧回复均修前红、修后绿 |
| [RR-21](../bug/RR-20260924-21.md) / [修复](../bugfix/RR-20260924-21.md) | Lua 返回精确十进制版本和 fence，避免浮点转换 | 真实 Redis 2^53+1 修前丢精度、修后精确；MaxInt64 对照通过 |
| [RR-22](../bug/RR-20260924-22.md) / [修复](../bugfix/RR-20260924-22.md) | 分配 fence 成功后才设置 owner，错误不遗留无 TTL 锁 | 真实 Redis 计数溢出修前留 owner、修后无 owner |

Assembly 生命周期现在明确为：首次绑定 bus → 启动/失败重试 → 运行 → 停止中 → 停止。开始停止后不允许复用同一个 Assembly 重启，返回 ErrAssemblyStopped；finalizer 原本就是单次生命周期，重新运行需要重新 Assemble。首次 bus 在该 Assembly 内固定，订阅函数本身受 ISyncBus 无 context 的接口约束，不能承诺外部阻塞实现可被强制中止。没有改动实体协议、Mongo/WAL 格式或业务提交顺序。

### 实际验证

新增四个测试文件：assembly_lifecycle_test.go、lock_generation_test.go、redis_lock_integration_test.go、ownership_redis_integration_test.go，全部在 remoteentity 原包。

- 生命周期六场景、迟到解锁两种结果，以及原有回归通过 race。并发测试用通道控制顺序，超时测试使用调用者 deadline。
- 真实 Redis：两个独立客户端竞争同一锁，验证 Touch/Refresh、受控到期、fence 跨 TTL 保留、旧解锁拒绝和新持有者版本读回。到期由测试对自身 key 执行 PEXPIRE 0 控制，无任意 sleep。
- 真实 Redis + 两个独立 Manager：并发争抢所有权只允许一个成功；Local → Shared → 转移给另一个 SID → 新 owner 回到 Local，marker/route epoch 增长，旧 owner 修改被拒绝，旧节点重新读取看到权威新值。
- 2^53+1 / MaxInt64 计数精度与 fence 耗尽拒绝验证通过。
- 再次运行前轮真实 Mongo/Redis/文件 WAL 的多实体六 DAO 原子提交、发布失败恢复、checkpoint 丢失重放和冲突回滚，race 通过。
- 10 个有测试包的 race 通过，kit/remoteentity 无测试；vet 通过。

```sh
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test -race \
  ./remoteentity ./entity ./nest ./dataengine/... ./kit/dataengine ./kit/nest \
  ./kit/remoteentity ./sync/syncbus/...
source /tmp/roost-dataengine-it/env.sh
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test -race -tags=integration \
  ./remoteentity -run '^TestReal(VersionedLock|RemoteOwnership)' -count=1 -v
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test -race -tags=integration \
  ./kit/dataengine -run '^TestRealDataEngineRemotePublicationAndWALRecovery$' -count=1 -v
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go vet -tags=integration \
  ./remoteentity ./kit/remoteentity ./kit/dataengine
```

日志：`/tmp/roost-remote-lifecycle-red.log`、`/tmp/roost-remote-generation-red.log`、`/tmp/roost-remote-lock-redis-red.log` 保留修前证据；修后为 `/tmp/roost-remote-lifecycle-generation-green.log`、`/tmp/roost-remote-ownership-real.log`、`/tmp/roost-remote-round2-race.log`、`/tmp/roost-remote-round2-recovery.log`、`/tmp/roost-remote-round2-vet.log`。关键证据已写入各 RR，临时日志不是唯一归档。

第二轮已完成，未发布。本轮所有权测试是同进程双 Manager/双 Redis 客户端，实体加载器为测试实现；不是多进程真实业务压测。自动续租的网络分区、节点强杀与进程级抢占仍需独立故障验收；也未将 generated Remote Nest handler → 网络广播的全链路标为完成。本轮没有新增性能结论，前轮 tracker 微基准仍仅代表事务缓存热点。

最后一次 Remote 全包 race 通过（3.161s，`/tmp/roost-remote-round2-last.log`）。索引刷新至 `2026-09-24T14:59:23Z`，七个改动 Go 文件 metadata_match、无记录缺口；三处历史模板 parse_partial 不在本轮范围。图谱的同名方法/接口启发式边经实际源码复核，不据零搜索结果判断不存在调用。新增文档链接和 `git diff --check` 通过。

## 第三轮：进程故障与正式业务链路

2026-09-24 继续实施。补齐前两轮明确留下的进程级租约故障与正式生成业务链路，并修复 [RR-23 冷准入和整批预算](../bugfix/RR-20260924-23.md)。生产改动只涉及 manager.go、batch.go、ownership.go：构造不执行权威 RPC，公开准入入口以调用方 deadline 与 OpTimeout 的较早值建立整批预算；不新增生产包，不改变持久化或快照协议。

### 真实进程故障

新增 `remoteentity/process_lock_integration_test.go`。父测试启动独立测试子进程，使用正式 Redis 锁和自动续租；只强杀自己启动的进程。每个故障场景独立 Redis key 和 Toxiproxy 代理，不重置共用代理或数据库。锁初始 TTL 为 1200ms，自动续租间隔 200ms、每次扩展 600ms；不能将这些测试参数当作线上默认值。

| 场景 | 断言与实际观察（race） |
| --- | --- |
| 健康续租后强杀 | 跨两倍初始 TTL 观察到 11 次 touch，强杀后新进程约 **2.413902292s** 接管，fence 1 → 2 |
| 隔离旧 owner 的 Redis 连接 | 观察到 4 次真实续租网络错误，新进程接管；恢复网络后旧进程解锁返回 not owned，新进程仍可正常解锁；接管及恢复检查约 **1.213908333s** |
| 冷准入遇到慢 Redis | 注入 1500ms 下行延迟；调用方 deadline 50ms、OpTimeout 1s，在 **51.960166ms** 返回 context deadline exceeded |

以上是单次故障观察，包含调度和网络开销，不是生产恢复 SLA。代理恢复固定首次分配的监听端口，避免重新启用 :0 换端口导致夹具误判；先前一次夹具端口错误已修正，最终三场景均通过。

### 正式生成多实体、多 DAO 链路

新增 `scripts/test-remote-generated.sh` 与 `codegen/internal/entity/testdata/remoteflow/`。脚本调用正式 DAO/Entity 生成器，构建独立最小测试工程；业务 loader 持有生成的实体，接入正式 Nest、DataEngine Runtime、文件 WAL v2、Mongo Remote projection、Remote Assembly 和 Redis 锁，进入 shared 模式。没有 demo 依赖。

每种 async / strict / pipelined 模式分别验证：

1. 两个 Remote Entity，每实体 balance/items 两个 DAO，`RequestMulti` 连续提交三次，每次同时修改四个 DAO。
2. 随后执行业务拒绝与 panic 请求，四个 DAO 都恢复到成功提交后的值 3。
3. Flush 后逐文档读取真实 Mongo，四文档 `_ver=3`、值 3，Remote outbox 排空。
4. 独立 NATS 连接的接收 Manager 先订阅四个 scope，再收到全部 v3 快照且值为 3。接收端没有 Mongo loader 或 Redis L2，因此结果只能来自真实 NATS 交付，不会被存储回源掩盖。

三模式均在 race 下通过：async 1.92s、strict 0.16s、pipelined 0.14s；测试主体总计 2.32s。这是小规模功能验收，各模式时间含初始化、轮询和清理，不用于性能排名。发布与接收在同一个测试进程中使用两个真实网络客户端，连接隔离 NATS 集群的同一节点；多进程故障由上面的锁测试独立覆盖。

### 回归与复跑

```sh
source /tmp/roost-dataengine-it/env.sh
export GOCACHE=/tmp/roost-nest-go-cache GOWORK=off
# 依赖环境启动方式见 DATAENGINE-RECOVERY-2026-09-24.md。
go test -race -tags=integration ./remoteentity -count=1 -v \
  -run '^TestRealRemote(ProcessKillRecoversLease|ProcessPartitionFencesOldOwner|ColdAdmissionDeadlineUnderLatency)$'
./scripts/test-remote-generated.sh
go test -race ./remoteentity ./entity ./nest ./dataengine/... \
  ./kit/dataengine ./kit/nest ./kit/remoteentity ./sync/syncbus/...
go test -race -tags=integration ./remoteentity -count=1 -v \
  -run '^TestReal(VersionedLock|RemoteOwnershipClaim)'
go test -race -tags=integration ./kit/dataengine -count=1 -v \
  -run '^TestRealDataEngineRemotePublicationAndWALRecovery$'
go vet -tags=integration ./remoteentity ./kit/remoteentity ./kit/dataengine
bash -n scripts/test-remote-generated.sh
```

关联 10 个有测试包 race 通过，kit/remoteentity 无测试；vet、脚本语法检查通过。前轮 Redis 所有权/精确计数和真实 Mongo/WAL 恢复重新通过。主要日志：`/tmp/roost-remote-round3-process-final.log`、`/tmp/roost-remote-generated.log`、`/tmp/roost-remote-round3-race.log`、`/tmp/roost-remote-round3-vet.log`、`/tmp/roost-remote-round3-ownership.log`、`/tmp/roost-remote-round3-recovery.log`。临时日志可能被系统清理，关键参数、断言和结果已归档于此。

### 当前截止判断

三轮已确认的正确性问题、事务缓存热点和本轮授权的验收已完成，未发布。前两轮“未执行多进程故障、未执行 generated Nest 到网络广播”的状态已由上述验收补足。尚未覆盖生产规模 Remote 压测与长时间稳定性、Mongo/WAL 任意提交点强杀、Redis/NATS 集群故障切换的完整矩阵；普通 NATS 的在线快照交付测试也不等于网络分区期间的持久可靠交付证明。后续性能调整应以真实 Remote 业务负载的 profile 为依据，不从 tracker 微基准或本轮功能耗时推算容量。

实施后图谱刷新至 `2026-09-24T15:19:57Z`（18,124 nodes / 143,601 edges）。本轮六个改动 Go 文件及关联源码 metadata_match、无记录缺口；这仍是尽力覆盖信号。生成夹具、脚本和文档按配置不建索引，已直接读取检查；新增文档链接和 `git diff --check` 通过。三处历史模板 parse_partial 与本轮无关。
