# 核心执行边界审查

日期：2026-11-01。基线：`0a4e775469fe92d1a53d8e6f9f745126694b6cec`（main）。初次审查基线如上；本轮已修复并继续审查交付失败与客户端回调边界。

## 结论

确认并修复两项 P2：RV-20261101-07（Sync 交付后批量结算失败未恢复健康实体基线）与 RV-20261101-08（Native 托管客户端销毁后仍通知回调）。本轮检查范围内未确认新的 P0/P1；这不是全仓无 P0/P1 的证明，也不能替代生产验证。

此前 01～06 的修复仍在 main；04～06 尚未发布。本次没有重新把历史修复登记成缺陷。

## RV-20261101-07：交付后的批量结算失败缺少全量恢复

**状态：已修复并验证，未发版。等级：P2。**

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

**状态：已修复并验证，未发版。等级：P2。** `NativeGameClient.Report` 只在开始时检查 disposed，遍历 Error 观察者期间没有复核。第一个观察者 Dispose 后，剩余观察者仍执行，可能访问已经销毁的引擎对象。Request、ProbeClock、Notify、Subscribe 结果通知也直接调用委托，与普通事件的 NotifyAll 契约不一致。

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
