# DataEngine 优化审查与分批方案

日期：2026-09-24。基线：`967bc69` 加已完成的 Sync/Nest 工作树。最新状态：**D1～D4 的本地 Entity 范围与剩余三个优先项均已完成并验证，可收口转入维护；新增 RR-14/15 已修复，未发布。** 历史 Remote W-02 已修复发号根因，Remote 扩展验收按用户要求留后续。最新判断见第 12 节；此前各节保留当时的实施证据。

## 1. 当前职责与主链路

数据变更围绕 Entity 组织：生成 DAO setter 登记 transaction-local PersistChange，同时独立标记客户端/服务间 Sync dirty；Tracker 保存已接受版本，不能把 Sync dirty 当作另一条持久化脏数据队列。

```text
Nest 持有 Entity/Guard 锁
  → setter + PersistChange
  → PrepareMutation，形成 CommitRecord
  → Projector.Commit / Enqueue
  → WAL 接受 / 写入 / 持久化 ticket
  → Nest 准入、Sync 锁内冻结、释放锁、提交确认
  → TransactionReleased 清除 Projector held
  → 顺序 WAL replay → 分段投影 → Mongo version CAS / transaction
  → 投影成功后推进 WAL ack
  → Mongo outbox → OutboxWorker → JetStream（独立交付）

启动：恢复 WAL 投影 → 注册聚合 loader/delete gate → ready
装载：一致性读 → 必需 DAO 校验 → 迁移写回并等投影 → 发布 Entity
停机：停止业务生产者 → Sync 排空（仍需水位和传输）→ DataEngine Flush/Close
```

准确区分：WAL durable 不等于 Mongo projected，Mongo staged effect 不等于 Broker/消费者完成；System projection ticket 与 Nest durable ticket 不是同一承诺。Sync 的 on_change 等提交确认和解锁，不要求等待 Mongo 投影；periodic/on_change 共用持久化水位门槛。优化不能把这些阶段合成一个模糊的“保存成功”。

## 2. 当前与目标目录

当前核心有 **2 个 Go 包**，不是图谱 architecture 把接口节点也列作 package 时显示的 8 个。真实 `go list` 已核对依赖。

```text
dataengine/                 # mutation/Store 契约、Tracker、校验、迁移/Sync 视图
  engine/                   # Projector、MongoStore、聚合仓库、Outbox、Runtime/Assembly
nestwal/                    # WAL 文件/格式/刷盘/ack 原语及既有独立 committer
kit/dataengine/             # 配置解析与 Mod 装配
codegen/internal/dao/       # DAO 持久化参与者与变化登记生成器
```

保留这两个核心包：nest/nestwal 依赖 dataengine 契约，而 engine 实现反向使用 nest/nestwal。把 engine 塞进根包会形成 import cycle，[历史合并依据](../history/P2-04_dataengine.md)仍适用。nestwal 有独立日志职责，不为这轮优化迁入 engine；Kit 保留装配边界。

目标包树不变，仅将两个承载多类职责的文件各拆为两个文件：

```text
dataengine/
  doc.go                    # 核心中文契约说明（已新增）
  ...                       # 现有契约与 Tracker 保持
  engine/
    doc.go                  # 持久化阶段与阅读入口（已新增）
    projector.go            # 类型/配置、准入、ticket、held、状态与生命周期
    projector_replay.go     # 重放、分段执行、ack、后台重试（已新增）
    projection_plan.go      # 原有纯分段规则
    mongo_store.go          # Store 配置、基础设施、单文档 CAS、receipt/effect 辅助
    mongo_projection.go    # Project/ProjectBatch 的事务编排（已新增）
    entity_repository.go   # 保留聚合加载主线
    migration_runner.go    # 保留 schema 写回边界
    outbox_store.go
    outbox_worker.go
    runtime.go
    assembly.go
    operation_gate.go      # 本次 bugfix 已加入，私有可取消等待
```

不新增 manager/service/helper 包。不是按每个类型拆文件：先保留完整主线，把独立的重放流程和 Mongo 事务编排从配置/存储原语中分开。

| 现有位置/符号 | 目标与责任 | 调用影响 |
| --- | --- | --- |
| projector.go 的 ReplayPass/projectSegment/run/acknowledge | 同包 projector_replay.go；完整保留读取→分段→投影→ack 顺序 | 无公开 API/import 变化 |
| mongo_store.go 的 Project/ProjectBatch/batchMutationModel | 同包 mongo_projection.go；保留普通快路径和带副作用事务路径的清楚分支 | 无 BSON/receipt/线格式变化 |
| entity_repository.go / migration_runner.go | 原地保留；补充一致性读重试、迁移票据和发布条件注释 | 不改变聚合生命周期 |
| kit/dataengine 与 codegen | 不迁移；核对配置、生成物和测试 | 不要求业务更换接口 |

依赖仍为 kit → engine → dataengine/nest/nestwal/mongo/nats；nest/nestwal → dataengine。文件拆分不改变业务调用路径。

## 3. 本轮发现与处置

### 已确认 bug

[RR-20260924-09](../bug/RR-20260924-09.md)：Projector Flush/ReplayPass 和 Runtime 并发 Shutdown 等待 Mutex 时不响应截止时间，五个场景实际复现。已用同包 operationGate 修复，真实文件 WAL 重开确认未投影记录保留；[修复与测试](../bugfix/RR-20260924-09.md)。这是正确性修复，不等待重构批次。

### 重构与证据不足的项目

- 修复后的 projector.go 620 行，同时管理准入、两种 ticket、held、重放、ack、重试、健康与关闭；mongo_store.go 735 行，包含事务编排、批量回退、CAS 与多类 receipt/effect 操作。拆分依据是职责跨度，行数仅作导航成本参考。
- Mongo 多 mutation 排序仍用 sort.Slice；D1 可改成 slices.SortFunc + 明确比较函数，并对照既有 key 顺序。保留 Go 1.27，不为追新改变语义。
- 每次 pipelined Enqueue 创建一个等待 durable 的 goroutine；每轮 ReplayPass 分配 records/fences/segments/processedIDs。是可测候选，尚无本轮 profile 证明它们是业务瓶颈，不能先引入池或并行投影。
- Assembly/Runtime/Outbox 的 Start、失败回滚、Close-before-Start、重复启动/并发停机需要统一列出支持契约并做有界验证。当前只对已复现的截止时间问题完成修复，其他组合未据代码外形登记 bug。
- [W-2026-09-22-02](../bugfix/W-2026-09-22-02.md) 已定位：持久化公会误用进程临时 ID，重启后新事务复用旧 ID。发号根因已修复；[RR-12](../bugfix/RR-20260924-12.md) 补齐永久冲突分类。旧 WAL 是实际磁盘记录，不能凭缺少 receipt 就跳过，也不能删除或推进 ack 来掩盖冲突。旧“进程化身导致 ack 错位”猜测已撤回。

## 4. 分批实施与验收

| 批次 | 工作与顺序 | 验收与回退 |
| --- | --- | --- |
| D1 阅读主线 | 先补包契约与中文注释，再按上表同包搬移 Projector/Mongo 编排；处理明确的 Go API 写法与文档旧路径 | dataengine/engine、kit、Nest/WAL 相关测试/race，go list 依赖与 build；公开签名、Mongo/WAL 格式不变。每个文件搬移独立回退 |
| D2 生命周期 | D1 后列出状态/所有权表，核对启动失败资源回收、停止新准入、已接收工作排空、超时重试、重复/并发调用；只对确定 bug 改行为 | 通道控制的故障点、真实临时 WAL、未 ack 记录重开恢复、每笔回复/票据只完成一次；复用本轮 deadline 用例。每个 bug 单独 RR/fix，语义新增另写决定 |
| D3 性能与观测 | 先建立同机基线，分别测准入、durable、释放等待、投影/ack、outbox；确认热点后再做有限容量工作区复用/通知合并 | 相同 records/顺序/ack/失败语义；记录吞吐、p95/p99、分配、积压与恢复速率。无稳定收益不引入复杂实现；单项优化可独立回退 |
| D4 正式联调与恢复 | Nest 真实事务 + Entity 生成 DAO + 本地 WAL + 隔离 Mongo 副本集/JetStream；重点复现旧 W-02 的两次化身切换和第三次启动 | 多 DAO 原子性、晚冲突回滚、ack 丢失重放、100k backlog、慢存储、Broker 中断后 outbox 恢复、停机重启；保留真实数据证据。失败不改门槛，不跨代丢记录 |

各批保持既有 strict/async/pipelined 语义、Rollback 策略、Patch 无全量覆盖回退、事务 identity/receipt、remote fence、Sync 解锁前冻结边界。并行 Mongo projection、跳过坏 WAL、改持久化格式、换存储后端、新消息投递保证都不属于本次纯重构。

代码生成器与 Kit 不是旁路：D1 核对原签名仍能生成编译；涉及 mutation/tracker 的后续行为变更须补真实生成最小工程，不能仅用手写 DAO 替身。既有 Sync/Nest 工作树完整保留。

## 5. 性能验证口径

已有基准入口可复用，不另建类似框架：

- BenchmarkProjectionSegmentPlanner：纯内存分段规划。
- BenchmarkProjectorAdmissionMatrix：真实文件 WAL，async/strict/pipelined，1/8/32 writers。当前随 writers 改 GOMAXPROCS，且 pipelined 每笔等 ticket，报告不能冒充无限流水线或严格控制的单变量对照；正式 D3 应将 CPU 数和 writers 独立控制。
- BenchmarkProjectorWALReplayAckMatrix：每轮 256 条真实 WAL replay/ack，ProjectionStore 是替身，适合验证批次、ack 数和框架开销。
- BenchmarkMongoProjectionMatrix：mongotest 替身，包括单 CAS、outbox、多文档和冲突；名字不代表真实 Mongo 吞吐。

D3 基线命令示例（本轮未执行，不能据此报告性能）：

```sh
GOWORK=off go test ./dataengine/engine -run '^$' \
  -bench 'BenchmarkProjectionSegmentPlanner|BenchmarkProjectorWALReplayAckMatrix' \
  -benchmem -benchtime=3x -count=3 -cpu=4
```

实际业务沿用 1000 玩家、10000 实体、约 50 可见/人、每 50ms 变化 1%/5%，但 **实体变化数不等于持久化 mutation 数**。高频位置是否落库、一次业务涉及几份 DAO、单/多实体事务比例、payload、effects 比例须在正式负载中明确并分别报告，不把每个 Sync 位置变化强制写 WAL。初始基线可以单 persistent DAO + 单业务消息建立可比样本，再补热实体争用、跨实体与多 DAO 组合；这属于测量假设，不替业务决定存储策略。

已有隔离集成脚本位于 `kit/scripts/integration/dataengine-env.sh`；后续在独立测试目录/端口运行，不复用或修改开发数据。本轮修复已经使用真实本地 WAL，无真实数据库/Broker 运行证据。

## 6. 本轮实际验证与证据范围

- RR-09 修前五场景全部红，修后定向 race 绿；增加重开恢复断言后再次定向 race 通过。
- `go test -race ./dataengine/... ./nestwal ./nest ./entity ./kit/dataengine ./kit/nest ./sync/entitysync/... ./codegen/internal/dao ./codegen/internal/entity -count=1 -timeout=120s`：11 包通过。
- `go vet ./dataengine/... ./kit/dataengine`、`go build ./...`：通过。未运行新的性能基准或真实 Mongo/NATS 故障矩阵。
- codebase-memory Verify，最近项目 `Users-whb-roost-roost-core`，开始代际 `2026-09-24T06:50:16Z`。dataengine method 查询 117 条已处理两页；限定准入/重放、Kit、基准查询及 ReplayPass/LoadEntity 双向一跳均无剩余分页。接口调用与同名 heuristic 边以真实源码和 go list 校正，不以零入边证明未使用。
- 初始候选运行路径 metadata_match/no_recorded_issue，dataengine scope 无记录缺口；这是 best-effort，不是完整性证明。结束时索引仍是原代际，projector/runtime 为 metadata_changed、新增 operation_gate/测试为 not_tracked，已直接阅读、编译与运行补证；后续查询这些改动前须复核索引新鲜度。docs 按规则排除，直接阅读。未扩大为 nestwal、remoteentity、Saga 的逐函数全审计。724 个文档本地链接目标检查及 diff 空白检查通过。

以上第 6 节保留首轮审查时的验证记录；后续实施与当前交付边界如下。


## 7. 2026-09-24 继续实施

用户授权先调查历史 Remote，再继续优化；roost-optimize skill 已记录历史 bug 可直接复现、修复，不再以旧“待拍板/只审查”文字阻断已授权工作。

### 已实施

- **D1**：保留 dataengine/engine 两包。新增两个包的中文契约文档；Projector 拆为准入/生命周期与 replay 两个文件；MongoStore 拆为存储原语与投影事务编排。Projector 主文件约 620→443 行，MongoStore 735→460 行。搬移前后函数清单一致：除上一轮 Flush/ReplayPass 的 context 修复外，Projector 函数体无变化；Mongo Project 的排序改为 `slices.SortFunc`，排序键不变。聚合仓库与迁移加中文说明，保留连续加载主线。
- **D2 已确认修复**：[RR-10](../bugfix/RR-20260924-10.md) Outbox close-before-start 和并发启停；修前真实 panic，修后 race 通过。RR-09 的截止时间和关闭重试回归保持通过。这里只完成 Outbox 的状态审查，不声称 Assembly/Runtime 所有并发启动/启动失败路径已逐项穷尽。
- **历史 W-02**：从原 WAL 找到不同化身创建相同公会 ID 的记录，修正生成模板对 runtimeid 的误用。采用 Mongo 持久化发号及错误上下文；[证据和修复](../bugfix/W-2026-09-22-02.md)。不跳过未知事务、不自动改历史 WAL。
- **D3 测量入口**：Admission benchmark 的 writers 改为明确数量的 goroutine，由 `-cpu` 单独控制 GOMAXPROCS；新增 p95/p99 准入观测。每 writer 最多一笔等待中的 pipelined ticket，因此不是无限流水线峰值。停止计时后 Flush 并检查失败。基线和局限见 [性能记录](DATAENGINE-BENCHMARKS-2026-09-24.md)。未引入无收益证据的对象池或并行投影。
- **D4 部分恢复验证**：新增真实 Mongo + 文件 WAL 的三次同 SID 组件重建/第二次丢 ack/第三次重放回归。另运行 kit 现有真实 Mongo/NATS 的 6 项测试：多文档 receipt/outbox 原子性、晚失败回滚、Patch 冲突、加载迁移恢复版本、混合分段顺序、Mongo 成功但 ack 失败后的重启顺序，均通过（race，5.984s）。这些测试未模拟三次完整 OS 游戏进程启动。

### 本批验证

`go test -race ./dataengine/... ./nestwal ./remoteentity ./kit/dataengine ./kit/nest -count=1 -timeout=120s` 六包通过；Outbox 定向 race 通过。`go vet ./dataengine/... ./remoteentity ./kit/dataengine` 与 core、生成工程的 `go build ./...` 通过。生成工程 Controller/runtimeid race、真实公会 Mongo/WAL 回归和 codegen Demo 配方测试通过。所有数据库写入仅发生在独立测试库，既有 game/remote_entity 历史数据库保持只读。

图谱 Verify 起始代际仍为 `2026-09-24T06:50:16Z`，全部新增/修改证据路径已查 coverage；修改后为 metadata_changed/not_tracked，已用当前源码、函数搬移比较、编译及回归补证。scope 无记录缺口仅是 best-effort。模板图谱只提供文件节点的部分，以模板源和实际生成工程补证；没有以图谱零结果作全量否定结论。结束时已主动刷新索引到 `2026-09-24T08:38:17Z`，17990 节点 / 141741 边，新增主线文件已是 metadata_match；公会测试模板的复合字面量仍报 parse_partial，已读取对应源码范围并由实际生成编译补证。

### 当批结束时的后续边界（D2 最新进展见第 8 节）

当时 D2 的 Assembly/Runtime 并发 Start/Shutdown、启动失败资源回收仍需独立受控场景复核；D3 尚无真实业务写入比例下的全链路 p99 或已证实的吞吐优化；D4 的正式 Nest+生成 DAO 业务负载、100k backlog、慢 Mongo、Broker 中断恢复及完整游戏进程三轮启动未在本批重跑。继续沿本方案完成，不将已有少量集成测试等同于整个模块收尾。


## 8. 生命周期与重放分配继续实施

用户明确要求按当前证据修正文档，代码 bug 直接修复并继续优化。本批完成：

- **D2**：[RR-11](../bugfix/RR-20260924-11.md) 覆盖重复 Runtime.Start、并发 Assembly.Start、启动中 Shutdown、取消恢复后的清理所有权，以及 Projector 不关闭 WAL 时的 Runtime 回收；另覆盖构造失败后重试。启停共用已有 operationGate，未增加包。启动失败清理未完成时保留 Ready=false 的 Runtime，由 Shutdown 继续回收，避免 deadline 后遗失文件锁。
- **Remote 正确性**：[RR-12](../bugfix/RR-20260924-12.md) 将新建 meta 的 duplicate key 识别为确定版本冲突，通过已有 fatal 路径拒绝后续准入；同事务幂等、瞬时故障重试与 Mongo 事务回滚边界保持。历史 W-02 文档改为已确认的 ID 复用原因，原始二进制 WAL 未改写。
- **D3**：alloc_space profile 定位到 ReplayPass 提前分配整个批次。改为读到首条可投影记录后才分配，并移除 ack 前临时 ID 切片。空闲/held B/op 中位数分别 51,290→2,121、52,178→3,009；无进展轮询不再申请整个批次。未引入对象池、并行投影或跨分段 ack 合并。[完整数据](DATAENGINE-BENCHMARKS-2026-09-24.md)。

### 本批验证

- `go test -race ./dataengine/... ./nestwal ./remoteentity ./entity ./nest ./kit/dataengine ./kit/nest -count=1 -timeout=120s`：8 包通过；日志 `/tmp/roost-dataengine-next/regression.log`。
- `go vet ./dataengine/... ./remoteentity ./kit/dataengine`、`go build ./...`：退出码 0。build 有模块版本 stat cache 写入受沙箱限制的提示，不影响编译完成。
- 隔离三节点 Mongo / NATS 下现有 6 项真实集成测试再次通过（race，5.832s）；生成工程公会 ID 三项测试通过，含真实 Mongo + 文件 WAL 三次组件重建/丢 ack 恢复（race，2.433s）。日志 `real.log` / `remote-real.log` 位于上述临时目录。
- 性能前后串行、固定 `-cpu=4`，idle/held 各 2,000 次、重复 3 轮，并采集内存 profile；准入矩阵优化后按原口径复跑。结果不作为真实 Mongo 全链路吞吐或 MMO 容量结论。

本批完成已确认的生命周期问题与可测分配优化。D4 的正式 Nest+生成 DAO 业务负载、100k backlog、慢 Mongo、Broker 中断恢复及完整游戏 OS 进程三轮启动仍未在本批验证，因此不把模块标为全部收尾。

本批结束已刷新图谱至 `2026-09-24T09:09:45Z`（18,010 节点 / 142,025 边）。本批实现和测试文件 coverage 均为 metadata_match/no_recorded_issue，engine scope 无已记录缺口且无剩余分页；此结论不是完整性证明。公会测试模板第 157/163 行仍报部分解析，已直接读取复合字面量并通过实际生成工程测试补证。文档按规则排除，以直接读取和 758 个本地链接目标检查补证；`git diff --check` 通过。


## 9. 大积压与正式生成链路恢复验收

本批沿 D2～D4 继续实施，新增 [RR-13](../bugfix/RR-20260924-13.md)：慢发布耗时会耗掉 Outbox 的失败退避窗口，已改为从发布失败返回时计算，受控时钟修前红/修后 race 绿。没有扩大包树或改变 WAL/Mongo 协议。

新增 100k 文件 WAL / 10k 实体的跨 20 段恢复夹具；慢存储边界取消后全部记录保留，真实 Mongo 恢复约 11.28 秒，最终全部版本正确、再次重开 pending=0。已有 Mongo 主切换、NATS 全断、JetStream leader 切换及三个网络故障用例均已实际通过。

正式 DAO/Entity 生成器的最小业务工程通过三个独立 OS 进程运行，覆盖 async/strict/pipelined、加载恢复、继续写入和业务失败回滚；没有借用示例游戏。详细结果、复跑命令和证据范围见 [恢复验收记录](DATAENGINE-RECOVERY-2026-09-24.md)。

D1 主线整理、D2 已确认生命周期缺陷、D3 已测分配优化，以及 D4 本文定义的恢复矩阵已形成可复跑验收。真实 MMO 落库比例和长时容量、任意点进程强杀/断电、物理磁盘 I/O 故障仍是部署扩展验证，不能把短测推导为线上 SLA。

## 10. 多实体、多 DAO 验收扩展

按用户最新范围，先完成本地 Entity，Remote 后续验证。正式生成的钱包/背包 DAO 接入 Nest.RequestMulti，一笔交易修改两个实体的四份文档；8 个并发请求方共享 8 个实体，三种持久化策略与三个独立 OS 进程全部通过。

新增 576 笔四文档交易、6 笔单实体双 DAO 购买、18 次错误/panic 回滚检查；另验证三种策略下 Mongo 已提交但 WAL ack 丢失的跨进程幂等恢复，以及最后一份文档版本冲突时整笔 Mongo 事务回滚、fatal 拒绝新提交并保留 WAL。没有发现新的运行逻辑缺陷，本批仅扩展测试与文档。相关 7 包 race 回归通过；细节、边界和复跑入口见 [恢复验收第 6 节](DATAENGINE-RECOVERY-2026-09-24.md#6-多实体多-dao-的正式业务验收)。

## 11. 真实业务压测与截止判断

新增可复跑的正式生成 DAO → Nest Request/RequestMulti → 文件 WAL → Mongo 压测，最终统计 36 个样本、50,688 笔计时事务，逐文档与版本检查、最终 WAL pending=0。相关 9 包 race、vet、三进程生成链路及四种压力形态的 race 小样本通过。本轮只新增测量与记录，没有修改运行逻辑。

单 DAO 的 async/strict/pipelined 最终落库吞吐中位数分别为 1,238/685/4,996 事务/s；多 DAO 三种策略、分散/热点形态约 46～50 事务/s。四 DAO strict 以 20/s 持续约一分钟时 record→Mongo p99=20.1ms、未 ack 采样峰值 1；100/s 诊断运行积压 352 笔，停止发送后需约 7.5 秒排空。后者带 CPU profile，仅用于诊断超载，不作为无采样容量基准。

**现有功能正确性批次可收口，多 DAO 高吞吐尚不建议完整结尾。** 剩余重点是有界成功前缀的 checkpoint 合并，以及多 DAO 投影的批量/分片设计；不能通过跳过 fsync、提高排队量或混淆 Request 与 Mongo 完成来绕过。业务落库频率和延迟目标未确定前，不把 Sync 负载换算成持久化容量。完整测量口径、数据、源码依据及后续建议见 [压力报告](DATAENGINE-PRESSURE-2026-09-24.md)。


## 12. 剩余优先项全部实施与截止

按用户授权实施多 DAO 批量投影、安全 checkpoint 合并、积压原子准入与健康预警，保持核心两个包、旧 Store 兼容、单 DAO 快路与特殊事务边界。默认不启用业务准入上限，Kit 提供正式配置。真实晚冲突测试发现并修复 RR-14；身份测试发现并修复 RR-15，问题与修复分别留档。

36 个同参数性能样本、50,688 笔计时事务全部通过。pair 的 async/strict/pipelined 最终落库分别约 1,294/623/2,846 事务/s，旧基线约 48～50；单 DAO 基本持平。100/s 四 DAO 输入 record→Mongo p99=53.8ms、结束未 ack=7、29ms 排空。10 万条双 DAO WAL 跨 32 段恢复通过，版本正确，最终重开无积压。九包 race、vet、真实集成、正式 DAO/Nest 三进程链路全部通过。

本轮本地 DataEngine 的计划和已确认问题已经收口，转入维护；Remote 扩展验证按用户约定后置，生产长期容量和故障演练另行验收。完整实现边界、配置、36 样本表、持续输入、大积压数据和复跑命令见 [剩余优先项实施与验收](DATAENGINE-BATCH-2026-09-24.md)。第 11 节保留优化前证据，其性能阻塞已由本节覆盖。
