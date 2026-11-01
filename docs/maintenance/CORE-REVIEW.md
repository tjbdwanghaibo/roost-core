# 核心执行边界审查

日期：2026-11-01。基线：`0a4e775469fe92d1a53d8e6f9f745126694b6cec`（main）。本轮只审查，不修改运行时代码。

## 结论

确认一项 P2：RV-20261101-07，Sync 在交付成功后批量结算失效，未恢复其他健康实体的内容基线。本轮检查范围内未确认新的 P0/P1；这不是全仓无 P0/P1 的证明，也不能替代生产验证。

此前 01～06 的修复仍在 main；04～06 尚未发布。本次没有重新把历史修复登记成缺陷。

## RV-20261101-07：交付后的批量结算失败缺少全量恢复

**状态：已确定性复现，未修复。等级：P2。**

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
# Preserve the output, then remove this intentionally failing probe.
rm framework/sync/entitysync/zz_probe_reserve_test.go
```

实际失败：

```text
healthy subject received invalid delta: client=1 retry base=0 version=1 full=false
FAIL github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync
```

影响是无关健康实体的同步恢复失败；标准客户端会拒绝错误基线，可能导致连接恢复。没有证据说明 Mongo/WAL 数据损坏，因此不定 P1。探针证明服务端线协议版本错误，未实跑 Unity 断线表现。

修复方向：把“已有帧交付但内容批次不能提交”作为统一失败边界，保留交付前缀的线上时钟，要求相关存活订阅恢复当前全量并重排待发送项；不能回滚已发送帧、无限重发旧 delta 或把一个关闭 Subject 的错误扩散成所有健康会话永久丢失。验收需覆盖关闭发生在预留前/后、多个会话、同 ID 重开、预算延期、取消及移除与重试组合；本轮未实施这些修复。

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
| Entity → Sync | `SyncMutation.Admit/Finish`、冻结、内容预留与交付结算、Subject.Close/Unload | 确认 RV-20261101-07；持久水位与提交门存在，不据此宣称所有时序已覆盖 |
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
