# 核心执行边界审查

日期：2026-11-01。最近一轮审查基线：`0400e8d4255179d056e306af7a0d0f397d9a03bd`（main，已与远端对齐）。本轮审核 Nest / DataEngine / Sync，随后按授权修复 RV-09。此前 RV-07/08 的修前基线为 `0a4e775469fe92d1a53d8e6f9f745126694b6cec`，红绿证据保留在下文。

## 当前结论

- **2026-11-02 修复 RV-20261102-02**：压测初始版本断言过时；旧版与本轮均复现。生产版本语义未变，实跑结果见性能报告。

- **2026-11-02 新增并修复 RV-20261102-01（P2）**：Patch 准入缺少重复及祖先路径检查。见下节，本项不代表重新完成全仓审计。

- **已修复 P2：RV-20261101-09**，回放取得执行权后复查 fatal，排队 Flush 和直接 ReplayPass 均返回原错误，不再调用存储。
- RV-07/08/09 已修复；发布状态以 CHANGELOG 为准。
- Nest 与 Sync 在本轮列明的路径中未确认新增缺陷；未确认新增 P0/P1。这不是全仓无缺陷的证明。

## RV-20261102-03：无持久字段 DAO 生成未使用局部变量

状态：已修复，发布前门禁发现。基线 `56d165f5` 的模板在每个 DAO 的补丁编码方法中无条件声明 `root`，但只有持久字段分支会使用它；全 nopersist DAO 生成后编译报 `declared and not used: root`。这类 DAO 在心跳场景实体与双模式 Sync 夹具中出现，普通持久 DAO 的 golden 和压力场景未覆盖该形状。

修前 `scripts/test-heartbeat-generated.sh` 的 Monster/Scene DAO、`scripts/test-sync-modes-generated.sh` 的 State DAO 均编译失败，pretag 阻止 tag 发布。修复按 `persistFields` 是否非空生成声明，不添加无效读或放宽编译检查。有持久字段的 golden 输出完全不变，故已有持久化压力对照仍对应最终生成代码。

修后 DAO golden 校验通过；两个正式生成脚本分别完成构建、race 和运行验收，心跳 dispatch/batch 均通过，生成物检查无漂移。这两个脚本已是 pretag 的永久验收入口，不以额外的文本断言代替消费工程编译。

## RV-20261102-02：正式 DataEngine 压测使用过时的初始版本

状态：已修复，测试夹具问题，不是业务数据丢失。`tradeFixture.seed` 在创建时提交版本 1、业务初始化再提交版本 2，已有 `seededVersion=2`；`pressure_test.go` 的 wallet、inventory 与 Mongo 最终断言仍写死 `1+updates`，导致正常更新也无法通过验收。

修前 v1.38.0 与本轮源码使用相同诊断，各以 2 个实体、1000 次 pair/async 请求复现：`coins=11000 transfers=1000 version=1002; want coins=11000 transfers=1000 version=1001`。第一轮 1000 Entity / 10 万请求也在同一断言失败，不能把那轮记作验收通过。

修复三处期望统一使用 seed 的共享常量，补齐错误信息中的实际值与期望值。金额、次数、Mongo 内容、版本、WAL 排空与投影失败检查全部保留；未改任何运行时代码或放宽一致性标准。正式复跑结果见[持久化性能报告](../performance/DATAENGINE-PERSIST-PATHS.md)。

## RV-20261102-01：冲突 Patch 路径可进入 WAL

状态：已修复；基线 v1.38.0 `c8e65071`。`validatePatchPaths` 原先只验证单条路径语法，没有拒绝重复键、父子重叠或跨 Set/Unset 的冲突。手写 DAO 可生成 Mongo 无法应用的 Patch，并通过持久准入验证；这会留下不能正常投影的记录。本轮测试只证明错误准入，没有制造生产积压或数据损坏。

修前回归 `TestPatchRejectsConflictingPathsBeforeWAL` 的 parent_child、set_unset、same_path、duplicate_set 四例均失败：`conflicting paths admitted: <nil>`。修复统一检查两组路径及所有点分隔祖先，返回 ErrInvalidPatchPath；生成器的路径树在生成阶段合并父子写，正常嵌套补丁仍能通过。

红绿入口：`go test ./framework/dataengine -run TestPatchRejectsConflictingPathsBeforeWAL -count=1`。修后定向 race、全仓 build/vet/test、生成 game-demo build/vet/test 与无漂移检查，以及 DAO 真实 Mongo/WAL 重开回归通过；不更改 Mongo 版本条件、WAL codec 或 checkpoint。CBM 已刷新到 generation `2026-11-02T00:35:21Z`，本项关键文件 coverage 无记录缺口；全仓仍有 19 个其他 partial 文件，未声称完整索引。

## RV-20261101-09：排队回放在获得执行权后未复查 fatal

**状态：已修复并验证。等级：P2。发布状态见 CHANGELOG。**

触发顺序：

1. 回放 A 持有 `replayGate`，进入 Store.Project，尚未返回。
2. 并发 Flush B 通过 `projector.go:380` 的 fatal 检查和 WAL.Sync，等待相同的 replayGate。
3. A 返回 ErrProjectionConflict，记录 fatal 并唤醒等待方，然后释放 replayGate。
4. B 得到执行权后直接读取 WAL 并再次调用 Store.Project，没有在这个共同入口检查 fatal。

根因：`framework/dataengine/engine/projector_replay.go:26-30` 获得 replayGate 后直接 readReplayBatch；`framework/dataengine/engine/projector.go:380-387` 的检查早于等待，无法覆盖等待期间的状态变化。直接调用 ReplayPass 同样绕过检查。正常后台循环遇 fatal 会退出，但已经进入 Flush 的请求不随该循环退出；Runtime.stop 的 Flush 与后台回放也可能处于这个交错中。

影响：fail-stop 后仍会发起新的投影调用。原冲突仍存在时会重复调用并失败；本探针只证明再次调用，不证明真实 Mongo 数据损坏、冲突被跳过或错误 checkpoint，因此定为 P2。已启动的并行通道仍应等待完成并确认成功前缀，不能用提前返回破坏这项既有承诺。

复现使用正式文件 WAL、Projector 和可控 ProjectionStore；`testing/synctest.Wait` 固定第二个调用已阻塞的顺序，再放行首个存储错误。没有 sleep 赌竞态、手工设置 fatal 或改运行时代码。夹具：[queued_flush_after_fatal_test.go.txt](../../scripts/testdata/dataengine-review/queued_flush_after_fatal_test.go.txt)。

```sh
cp scripts/testdata/dataengine-review/queued_flush_after_fatal_test.go.txt framework/dataengine/engine/zz_probe_fatal_test.go
GOWORK=off go test -race ./framework/dataengine/engine -run '^TestReviewQueuedFlushStopsAfterFatal$' -count=1 -timeout=15s
rm framework/dataengine/engine/zz_probe_fatal_test.go
```

实际失败：

```text
--- FAIL: TestReviewQueuedFlushStopsAfterFatal (0.02s)
    zz_probe_fatal_test.go:34: queued Flush called Store.Project after fatal: calls=2, want 1
FAIL github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine
```

修复约束：在取得回放执行权的共同入口检查粘滞终态，返回原始 fatal 原因，不再读取/投影后续记录；不要只给 Flush 再加一处等待前检查。验收补齐直接 ReplayPass、排队 Flush、正常临时错误重试，以及 fatal 当轮已经成功的前缀仍能确认。还应核对 WAL terminal 与 Close 的边界，不能把普通取消和可重试错误变成永久 fatal。


修复验收：`ReplayPass` 取得 replayGate 后检查粘滞 fatal，保留原始原因；不修改执行中的投影、成功前缀确认、WAL 格式或普通临时错误重试。永久回归并入 `projector_test.go`：`TestQueuedFlushAndDirectReplayStopAfterFatal` 覆盖排队 Flush、直接 ReplayPass、存储只调用一次与失败记录仍留在 WAL。修前同一探针再次失败（calls=2），修后通过。

实际执行：`GOWORK=off go test -race ./framework/dataengine/engine ./framework/nestwal -count=1`、目标包 vet、根包测试全部通过；包内既有并行 fatal、成功前缀、临时错误恢复、OnFatal 关闭及 checkpoint terminal 回归一并通过。此次没有增加额外的终态状态或把取消转为 fatal；真实存储故障矩阵仍未复跑。

## 最近一轮三核心审核范围与验证

| 模块 | 本轮重点 | 结论 |
| --- | --- | --- |
| Nest | 全声明 ID 的 tail 准入、I/O 原位迁移和业务续行、Await 的段间移交、事务拒绝/未知结果、pipelined 完成顺序与回调 | 未确认新增缺陷。曾检查嵌套事务重复占完成链的可能性；RunIsolatedTransaction / RunDetachedTransaction 传空 completionRoute，排除该候选。异步完成只按主实体排序、Cast 不提供声明 ID 顺序，是已写明的边界，未重新登记为 bug |
| DataEngine | WAL 准入与释放、投影预算、冲突键分道、逐条退回与快路幂等、连续成功前缀 checkpoint、fatal 与 Shutdown 交错 | 确认 RV-09。reserve 在 heldMu 下复查 fatal、WAL terminal；并行窗口等待已启动通道后计算连续前缀，这两项本轮未发现新违例 |
| Sync | 提交门与 Guard 解锁、失败交付恢复、帧前缀采纳、订阅 revision/lifetime、Hold/Ready、发送与冷快照预算组合 | 未确认新增缺陷。复核 RV-07 的统一恢复入口，既有多会话/重绑/重连/移除回归通过；不回退已交付时钟，不以放宽客户端版本比较恢复 |

CBM：项目 `Users-whb-roost-roost-core`，审查 generation `2026-11-01T08:28:30Z`。使用 search_graph、get_code_snippet、双向 trace 及 check_index_coverage；关键 trace 已读完整页，范围 coverage 未记录缺口。图谱对 `errors.Is`、`sync.Once.Do` 等同名调用存在错误归属，对接口调用也有漏连，因此关键关系均以当前源码核实，不用零入边证明没有调用者。`settlement_recovery_test.go` 的 metadata_changed 是上轮注释更新，已直接读取源码补证。全仓 19 个 partial 文件不在本轮关键执行路径中，未据此声称完整索引或完整审计。

实际验证（macOS）：

```sh
GOWORK=off go test -race ./framework/nest/... ./framework/entity ./framework/dataengine/... ./framework/nestwal ./framework/sync/... -count=1
GOWORK=off go vet ./framework/nest/... ./framework/entity ./framework/dataengine/... ./framework/nestwal ./framework/sync/...
GOWORK=off go test -count=1 .
```

既有回归 16 包通过，vet 与根包检查通过；审查时新探针单独运行并失败；后续修复已将其并入永久回归，红绿证据见上文。没有运行 integration 标签的真实 Mongo/Redis/NATS 故障、Linux、性能、长稳或游戏生成矩阵；AOI/Lockstep 等子包参与回归，但未逐函数人工审计。

方向判断：目前证据指向失败边界的检查位置，不要求重写三大模块。应将状态复核放在实际取得执行权的共同入口，并验证排队期间状态变化；不要在各调用方继续复制判断。和 RV-07 一样，审查必须覆盖等待/交付前后两个阶段，单次成功路径与 race 无报告都不能证明这个契约。

## RV-20261101-07：交付后的批量结算失败缺少全量恢复

**状态：已修复并验证。等级：P2。发布状态见 CHANGELOG。**

触发顺序：

1. 同一 Manager 管理 A、B 两个 Subject，客户端已收到初始全量。
2. 两者变脏，Flush 捕获并成功发送增量，客户端已应用 A 的版本 1。
3. B 在发送成功后、内容结算之前关闭。正式来源包括 `ManagerAccess.Unload`，不是必须修改私有字段才能触发。
4. `ReservePreparedSubjectSyncBatch` 因 B 已关闭拒绝整批。A 内容版本没有推进，但对应会话的帧时钟和订阅已推进。
5. A 再次变化，下一 Flush 仍发 `base=0, version=1, Full=false`；客户端已持有版本 1，无法连续应用。

根因与基线位置：

- `framework/entity/manager_access.go:683`：卸载结束关闭 SubjectSyncState。
- `framework/entity/subject_sync.go:741`：预留要求 `state.enabled`，关闭使整批预留失败。
- `framework/sync/entitysync/flush.go:297`：先发送并采纳线上状态；`pushSessionFrames` 成功后还结算订阅。
- `framework/sync/entitysync/flush.go:318`：预留失败只 abort 并记录错误，没有调用 `requireSnapshotsAfterRetry`。与同文件 301～312 行的传输重试路径不对称。
- `client/dotnet/Roost.Client/EntityViews.cs:145` 附近：增量必须与上一 Subject 版本匹配，否则拒绝；不能靠放宽客户端校验“修复”。

探针：[close_during_delivery_test.go.txt](../../scripts/testdata/sync-review/close_during_delivery_test.go.txt)。它使用正式 Manager/Subject 状态、记录型 Transport 和确定性 Push 后屏障；没有 sleep 赌竞态，也未修改生产实现。复跑：

```sh
cp scripts/testdata/sync-review/close_during_delivery_test.go.txt framework/sync/entitysync/zz_probe_reserve_test.go
GOWORK=off go test -race ./framework/sync/entitysync -run '^TestReviewCloseDuringDeliveryKeepsOtherSubjectBaseline$' -count=1
# Fails on the review baseline; passes after the fix. Remove the temporary copy.
rm framework/sync/entitysync/zz_probe_reserve_test.go
```

实际失败：

```text
healthy subject received invalid delta: client=1 retry base=0 version=1 full=false
FAIL github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync
```

影响是无关健康实体的同步恢复失败；标准客户端会拒绝错误基线，可能导致连接恢复。没有证据说明 Mongo/WAL 数据损坏，因此不定 P1。探针证明服务端线协议版本错误，未实跑 Unity 断线表现。

修复：传输失败、内容批量预留失败与提交失败统一走 `retryCaptured`。保留已交付的帧时钟，放弃内容捕获，要求相应存活订阅恢复当前全量并重排待发送项；失败后立即返回，不进入成功清理。不能回滚已发送帧或放宽客户端版本校验。

新增永久回归 `settlement_recovery_test.go`：原始失败场景转绿；两会话分别验证无下一次 setter、Subject 重绑、同 ID 会话重开、Subject 移除；恢复为 Full 后必须重新连续发送 Delta，脏状态最终结算且健康会话不丢失。预留后关闭与预算延期等沿用包内既有测试；未逐一穷举它们与本次失败的所有组合。

## 审查方法与范围

使用真实 Codebase Memory 官方 CLI 执行 `index_repository`、`index_status`、`search_graph`、`trace_path`、`get_code_snippet`、`check_index_coverage`，没有用纯 rg 代替图谱。

- 项目：`Users-whb-roost-roost-core`；根路径 `/Users/whb/roost/roost-core`。
- 审查索引 generation：`2026-11-01T08:07:31Z`，full、ready，42,503 节点、392,769 关系；对应上述基线。后续文档提交刷新会产生新代际。
- 所引用核心源码均检查 coverage，得到 `metadata_match / no_recorded_issue`。全仓 19 个 partial 文件主要为 C++ 宏、模板及配置，不在本轮确认缺陷的执行路径中；无缺口记录仍不等于图谱完整。
- Sync Flush 的双向 trace 存在分页，已读完。图谱对 Go 接口分派及同名方法会漏连/误连，例如 Remote FinalizeLocked 的入边不完整，不能据此认定没有调用者；相应关系以 `msg.go`、`execution.go` 等当前源码补证。
- 本机查询原始记录与 race 日志放在 `artifacts/review/20261101-cbm/`；这是本地诊断产物，不随 Git 分发。可复跑的失败夹具随仓库维护。

| 审查链路 | 核对内容 | 结果与边界 |
| --- | --- | --- |
| Nest → WAL | `runTransaction`、`commitDurable`、`commitPipelined`、`durableCommit`、`pipelinedEnqueue`、`acceptPersistence` | 检查准入前拒绝、准入后未知、不重排及不回滚、释放与完成顺序；未确认新缺陷，未穷尽第三方 Committer 行为 |
| 失败创建 | `createdBuildMark`、`AddMutation`、`SetReceiptPayload`、DAO 登记、嵌套回滚冲突 | 现有 DAO/回执代次与新增项边界生效；不把任意业务组件副作用视为自动可回滚 |
| Entity → Sync | `SyncMutation.Admit/Finish`、冻结、内容预留与交付结算、Subject.Close/Unload | RV-20261101-07 已修复；持久水位与提交门存在，不据此宣称所有时序已覆盖 |
| WAL → DataEngine | `Enqueue`、`ReplayPass`、`readReplayBatch`、`projectParallelWindow/projectLane`、冲突键与 checkpoint | 已成功连续前缀才 ack，失败停止补位并等待已启动工作；本轮未确认新缺陷 |
| Nest → Remote | `finishRemoteWriteBatch`、`FinalizeLocked`、`Commit/Abort/Close` | 已提交、明确拒绝、结果未知分开收尾；本轮未确认新缺陷，未注入真实 Mongo/NATS 故障 |
| Saga | `WaitTerminal`、Lifecycle 绑定与登记后补查 | 已有终态复用缺陷修复仍在；保留回执超期停机等已知限制，不把它误报为本轮新增缺陷 |

## 实际执行的验证

移除临时失败测试后，以下八包 race 全部通过（macOS，本机）：

```sh
GOWORK=off go test -race ./framework/nest ./framework/entity ./framework/dataengine/engine ./framework/nestwal ./framework/remoteentity ./framework/saga ./framework/sync/entitysync ./framework/sync/entitysync/policy -count=1
```

新探针单独运行并失败，不能与既有回归通过合并成“全部通过”。未重跑全仓、真实 Mongo/Redis/NATS 集群故障、Linux、Unity/IL2CPP、跨机、长稳和性能。没有发现 P0 不代表证明不存在 P0。

## 为什么多轮 review 仍会漏掉严重问题

之前主要围绕已知复现做局部修复，验证了单一出口，未完整枚举同一契约的其他状态来源。失败创建从 DAO 原位修改漏到回执原位修改，客户端从实体事件的逐订阅者检查漏到直接多播事件；本轮 Sync 又是同一个“交付后失败”契约只在传输错误分支实现，在内容预留失败分支遗漏。更多普通测试或重复跑 race 不能补上缺失的场景。

方向判断：优先收敛同一契约的多个实现入口，而不是每发现一个出口就增加独立状态。审查必须先列事实与所有权（已执行、已准入、已持久、已交付），再逐个审查失败、取消、卸载、重建、回调重入组合。P0/P1 的修复必须有修前失败证据、相邻分支对照和修后状态检查；图谱负责发现调用关系，不能替代这些语义验证。报告中必须写范围，停止使用“整个模块已收尾”代替有限证据。

## RV-20261101-08：Native 托管客户端的销毁通知边界遗漏

**状态：已修复并验证。等级：P2。发布状态见 CHANGELOG。** `NativeGameClient.Report` 只在开始时检查 disposed，遍历 Error 观察者期间没有复核。第一个观察者 Dispose 后，剩余观察者仍执行，可能访问已经销毁的引擎对象。Request、ProbeClock、Notify、Subscribe 结果通知也直接调用委托，与普通事件的 NotifyAll 契约不一致。

修前在当前 native 动态库上执行 `DYLD_LIBRARY_PATH=/tmp/roost-native-full-shared dotnet run --project client/cpp/tests/managed/NativeInterop.csproj`，新增 DisposalTests 使用真实 native 无效地址关闭事件，结果：`native Error callbacks after disposal: calls=3, expected=1`。不是仅用反射模拟回调。

修复应复用逐观察者通知，错误观察者保持异常隔离；解析器或重置通知销毁客户端后停止后续处理。需覆盖首位/中间观察者销毁、正常多观察者、请求结果和 PB 解析器，以及既有 TCP/PB/Lockstep/重连链路。此问题属于 RV-06 在 native 托管实现中的遗漏，不能只以纯 C# 客户端通过推断两套客户端均通过。


## 修复验收与继续审查边界

- RV-07：`GOWORK=off go test -race ./framework/sync/entitysync ./framework/entity ./framework/nest -count=1` 全部通过；`GOWORK=off go test -count=1 .` 与 `GOWORK=off go vet ./framework/sync/entitysync` 通过。
- RV-08：请求成功/失败、校时、Notify、PB Subscribe 使用既有 `NotifyAll`；Error 保持独立异常隔离并逐观察者检查销毁。解析器销毁后不再交付结果；generation 重置通知销毁后停止当前事件。没有增加新生命周期状态或修改协议。
- 新增 `client/cpp/tests/managed/DisposalTests.cs`：真实 native 错误事件覆盖不销毁、首位销毁、中间销毁；真实 TCP 覆盖请求成功/断线失败、解析器销毁、Notify、PB 推送及校时结果。全部通过。测试初版的主动 Disconnect 可能先于服务端读到请求，导致测试端 EOF；现由服务端读到请求后主动关闭，确定性验证断线失败回调。
- 纯 C# `dotnet run --project client/dotnet/Roost.Client.Tests -- client/spec/packets.json` 全部通过，包含共享 EntityViews、PB、心跳、销毁和重连回归。
- 既有 `game.py` + `NativeInterop.csproj` 的真实 TCP/PB/Lockstep 反向 PInvoke/重连通过；当前 native 动态库重新构建后 CTest 5/5 通过。
- 继续审查使用 CBM MCP 的结构查询、双向 trace、源码与 coverage；检查 Sync 恢复、订阅重绑/退订、预算调度，以及 NativeGameClient 的回调出口。修改后的文件曾显示 metadata_changed，均读当前源码补证并在完成时刷新索引（generation `2026-11-01T08:28:30Z`，42,525 节点、392,380 关系，0 skipped）。未把全仓 19 个 partial 文件视为已审计，也未把没有记录缺口当完整性证明。
- 本轮没有新增 P0/P1 证据。未重新进行全仓测试、Unity Editor/IL2CPP、Linux、真实存储集群故障、性能或长稳验证。本轮是上述边界的修复与复核，不是全仓无缺陷认证。
