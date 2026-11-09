# 新增提交审查

## 2026-11-09：剩余地形方案与按类型同步

main 仍为 f0ff1727，原两项去重问题未修。补充 P3/P4/P5 的副本恢复、数据就绪、建筑并发、部分成功对账及换版联动设计边界；纳入 Player/Monster on_change、TerrainChunk 1 秒脏增量的建议。当前 Mode/Interval 是 Manager 级，尚无同 Manager 按类型混用。见 [续审与验收建议](TERRAIN-PLAN-FOLLOWUP-20261109.md)。本轮未改运行时、未执行压测。

## 2026-11-08：可编辑地形方案与 P1/P2 审查

main f0ff1727 确认两项 P2：RV-20261108-01，32 条块内收据淘汰后旧请求再次执行（已知有界窗口缺过期处理）；RV-20261108-02，63 位 EditID 可确定性碰撞，不同合法请求被跳过。两项各三轮复现，窗口内控制组三轮通过。地形/同步 race、生成工程定向测试、C# 地形测试和生成工程全包编译通过。P3 与配额 270 尚未合入；详见 [方案审查及证据](TERRAIN-REVIEW-20261108.md)。

## 2026-11-08：提交全部首发缺失项

按用户要求将上一轮首发缺口完整拆为 [L01–L15 实施清单](LAUNCH-BACKLOG-3000-20261108.md)，供后续自行实施；不新建 issue/PR、不写运行时或执行压测。已有修复关闭状态不变。

## 2026-11-07：全能力首发准备评估

按用户给出的 3000 同时在线、单逻辑服 10 万玩家整理 [上线缺口与验收清单](LAUNCH-READINESS-3000-20261107.md)。区分 demo 资产补偿策略、cellworld/UGC 联合验收、目标容量、部署恢复和上线后兼容政策；不是新增运行时缺陷报告，未运行压测或改动生产环境。

## 2026-11-07：v1.42.2 修复复验通过

联盟按所有权任期缓存，原迁出扫描／迁回探针及完整联盟包 race 三轮通过，RV-20261107-03 与 RV-20261107-01 关闭。Saga busy 重试保留原 fence，正式回归与补充边界五轮通过；Lockstep 深缓冲目标三轮及完整包 Linux race 一轮通过，夹具 Linux 待验项关闭。本轮未发现新确认缺陷，不代替真实集群压测及生产验收。见 [v1.42.2 复验](FIX-REVIEW-1422-20261107.md)；以下旧状态按各自基线阅读。

## 2026-11-07：v1.42.1 修复复验

RV-20261106-01/03/04 与 RV-20261107-02 原缺陷复验通过，关闭；cellworld 边界抖动夹具修正经 Linux 完整包及三轮重复验证通过。联盟 RV-20261107-01 只部分修复：新增确认 RV-20261107-03（P2），迁出前 drop 后的扫描可重新写入 picked，导致再次迁回时永久跳过待确认宣战；三轮复现、控制组三轮通过。Lockstep 未闭环，main 新登记的 Saga 偶发 fenced 仍根因待查。详见 [修复复验与续审](FIX-REVIEW-20261107.md)。以下旧结论按其历史基线阅读。

## 2026-11-07：新功能审查收尾

新增 RV-20261107-01（P2）：联盟迁移检查后仍可提交宣战，迁移完成后补发责任没有自动接续；RV-20261107-02（P2）：best_effort 单 worker Stop 在 handler 返回前关闭 Closed。两项均连续三轮复现。之前三项确认缺陷仍未修复，Lockstep／cellworld 间歇失败仍未关闭。根模块与生成工程 race、客户端及联盟真实进程证据和验证边界见 [收尾报告](FEATURE-REVIEW-20261107.md)。

## 2026-11-06 续审：cellworld 生命周期与编码

新增 RV-20261106-03（P2）：旧 Leave 迟到副本删除同 ID 重新入场的新角色，三轮复现；RV-20261106-04（P3）：BaseState 重复 MsgPack 标签丢失 X/VX，影响直接编码该类型的接入方，未证明当前 demo 的字段映射受影响。源码、探针、控制组及范围见 [续审报告](FEATURE-REVIEW-20261106.md#续审跨区生命周期与状态编码)。前述 best_effort、边界抖动和 Lockstep 状态不变。

## 2026-11-06：v1.41～v1.42 新功能审查

确认失败创建的 best_effort 事件逃逸（P2）；cellworld 完整包跨区抖动失败一次，独立五轮通过，根因待定。具体基线、源码、复现与验证边界见 [新功能报告](FEATURE-REVIEW-20261106.md)。

## 2026-11-03：v1.40.1 Linux 修复复验收尾

main 仍为 [b3e9ea69](https://github.com/tjbdwanghaibo/roost-core/commit/b3e9ea69)。NATS、SLG 原问题复验通过，etcd 停止屏障重复验证通过；Lockstep 原样完整包和独立五轮仍出现深缓冲连接断开。测试缓冲对照五轮通过但未作为正式修复。证据与上线判断见 [复验报告](FOLLOWUP-REVIEW-20261102.md)。

## 2026-11-02：v1.38～v1.40 新功能复核

基线 [v1.40.0](https://github.com/tjbdwanghaibo/roost-core/commit/2be1f1e5)，本轮选定范围未确认新增缺陷。递归 DAO 同步、BSON 持久路径、稳定 store 键及生成接线的复核、Linux 回归、独立连续事务探针和升级边界见 [新功能报告](FEATURE-REVIEW-20261102.md)。不据此关闭上一轮 Linux 验收失败，也不声明生产全量验收完成。

## 2026-11-02：Linux 非压测补验

继续补跑全仓、真实依赖、游戏生成消费者、Redis、hotcode 与 PhysX。原失败、跳过与环境阻塞均保留，最终范围见 [Linux 验收续报](LINUX-FUNCTIONAL-20261102.md)。本轮不改运行时、不启动压测，以下为历史审查结果。

## 2026-11-01：v1.37.1 最新提交复核与 Linux 续审

基线：[main / v1.37.1 773ca443](https://github.com/tjbdwanghaibo/roost-core/commit/773ca443)。相对上一轮新增 8 个提交、21 个文件变更。已读取新运行时改动及 main 的 [CORE-REVIEW](https://github.com/tjbdwanghaibo/roost-core/blob/773ca443/docs/maintenance/CORE-REVIEW.md)，不把其他审查者的 macOS 结果冒充本机执行结果。

**结论：RV-20261101-06 的原问题已修复；main 新增登记并修复的 07／08／09 也通过本轮对应回归，修复方向合理。继续检查未确认新的缺陷。** 此前 01～05 的关闭结论保持；下方旧章节中的“待修复”仅描述当时基线。

| 项目 | 代码复核与本轮证据 |
| --- | --- |
| 06：纯 C# 销毁后回调 | 使用已有 Notify 逐订阅者检查 disposed，覆盖 StateChanged、Reconnected、UnhandledPush、请求成功／失败、PB 解析后通知，以及 ResetEntities 触发销毁后的返回。原 dispose_callbacks.py 两组通过；main 新增 DisposalTests 的状态／重置／失败和真实 TCP 推送／请求场景通过。 |
| 07：Sync 交付后内容结算失败 | Reserve 与 Commit 失败共用 retryCaptured，保留已交付帧时钟，撤销捕获并把存活订阅改为欠 Full，失败立即返回，不走成功清理。entitysync 包含新增 settlement_recovery 回归，race 通过；继续核对订阅替换／移除与恢复时的身份判断，没有确认新违例。 |
| 08：Native 托管销毁通知 | 成功／失败／校时／Notify／PB 订阅使用 NotifyAll，Error 逐观察者检查，generation 重置后复核 disposed。本轮用当前 C++ 源码编译 Linux 动态库，main 的托管 DisposalTests 在真实 native 事件及回环 TCP 上通过。 |
| 09：排队回放越过 fatal | ReplayPass 在取得 replayGate 后复查 fatal，覆盖等待中被前一轮置为 fatal 和直接调用入口。原 queued Flush／direct Replay 永久回归随 engine 包通过；失败记录未被再次投影、仍留在 WAL。 |

### 继续检查的相邻路径

阅读了 PreparedSubjectSyncBatch 的预留／提交／撤销、Sync 已交付前缀与 requireSnapshotsAfterRetry、投影串行及并行窗口停止补位／连续成功前缀 checkpoint、fatal 通知与等待项释放，以及托管客户端请求解析／错误／重置／重连回调。没有将标准软预算、已经发出的并行工作、结果未知或已有回执期限限制直接登记为新缺陷。该范围不是逐函数覆盖全部核心，也不是新的全仓无问题声明。

### 本轮实际验证（Linux）

```bash
go test -race ./framework/dataengine/... ./framework/nestwal ./framework/entity ./framework/sync/... ./framework/nest -count=1
python3 scripts/testdata/client-review/dispose_callbacks.py /path/to/reviewed-main /path/to/dotnet-root
```

15 个 Go 包全部通过，包含 Sync 新恢复用例和 DataEngine 排队 fatal 用例。原回调探针退出 0，不重复添加相同探针。纯 C# 另外执行 main 的 TestGameClientDisposal、TestGameClientNetworkDisposal，覆盖新增首位／中间观察者销毁、重置时销毁、失败多播、真实 TCP 推送和请求解析／回调销毁。

Native 动态库由当前 client.cpp、lockstep.cpp、snapshot_digest.cpp、lockstep_c.cpp 以 C++17／pthread／shared／PIC 和 ROOST_SHARED、ROOST_BUILD 编译。托管层编译当前 Roost.Client、client/cpp/unity 与 client/cpp/tests/managed 源码，执行结果：

```text
PASS managed/native error, request, parser, notification, push and clock disposal boundaries
PASS managed/native PInvoke layout, worker error and disposal
PASS managed/native PB callbacks, Lockstep reverse PInvoke, reconnect
```

最后一项经 main 的 game.py 启动真实两代 TCP 连接，验证 PB、Lockstep 反向 P/Invoke 和重连；不是 Unity Editor/Player 运行。未启用可选 Protobuf native DAO views 后端。托管测试使用 .NET SDK 10.0.402 / runtime 10.0.13 的直接 Roslyn 入口，避开 SDK 首次迁移；不是完整项目 NuGet／TreatWarningsAsErrors 门禁。合并编译时 nullable 设置产生提示，不能作为零警告证明；没有据此登记运行时问题。

本轮图谱工具不可用，按当前差异、精确源码与 rg 核实；没有沿用其他机器的 CBM generation。未运行真实 Mongo/Redis/NATS 故障、Unity/IL2CPP、生产 WAN、长稳、性能或全部游戏生成矩阵。固定分支只更新审查材料，不修改 main 运行时。

## 2026-11-01：续审客户端销毁回调与恢复边界

基线仍为 [main 5fe16dab](https://github.com/tjbdwanghaibo/roost-core/commit/5fe16dab)，fetch 后没有新提交。RV-20261101-01～05 的关闭结论保持；本轮确认以下独立问题，未修改运行时代码。

### RV-20261101-06 / P2：StateChanged 回调中销毁客户端后，后续订阅者仍会执行

**确定性复现。** [GameClient.cs:116](https://github.com/tjbdwanghaibo/roost-core/blob/5fe16dab/client/dotnet/Roost.Client/GameClient.cs#L116) 通过一次 `Invoke(() => StateChanged?.Invoke(state))` 执行整个多播委托。第一个订阅者调用 GameClient.Dispose 后，C# 多播委托继续调用快照中的后续订阅者，没有再次检查 disposed。这违反同文件类注释“Dispose 后不再通知业务”的约束，也与 EntityChanged 等事件现有 Notify 的逐订阅者检查不同。

触发不依赖竞态：正式 GameClient.Connection.ConnectAsync 的异步取票保持未完成，连接进入 Connecting；注册两个 StateChanged 订阅者，第一个 Dispose，第二个记录调用；在创建线程 Pump。实际第二个被调用一次，并观察到 Connection.State=Disposed，期望零次。对照不在第一个订阅者 Dispose 时，第二个正常调用一次。

影响：连接状态回调切换场景／销毁客户端后，后续 UI 或玩法订阅者仍可能操作已销毁对象、重新更新已清空界面或执行不应继续的副作用。本探针是普通 C# 的生命周期复现，没有宣称实跑 Unity MissingReferenceException。

建议复用现有逐订阅者 Notify，每次通知前检查 disposed，并核对 ResetEntities 回调销毁之后的通知边界。同文件 Reconnected 与 UnhandledPush 也使用直接多播调用，建议一并检查；本轮的独立失败探针只覆盖 StateChanged，不把其他事件写成已实跑。验收应覆盖首个／中间订阅者销毁、普通多订阅者通知、实体重置回调销毁，以及销毁后不再继续业务通知。业务暂可在回调入口检查自有销毁标记并提前返回。

探针：[dispose_callbacks.py](../../scripts/testdata/client-review/dispose_callbacks.py)、[dispose_callbacks.cs.txt](../../scripts/testdata/client-review/dispose_callbacks.cs.txt)。运行：

```bash
python3 scripts/testdata/client-review/dispose_callbacks.py /path/to/reviewed-main /path/to/dotnet-root
```

main 原源码结果：

```text
disposeInFirst=False: secondCalls=1, afterDispose=False, expected=1
disposeInFirst=True: secondCalls=1, afterDispose=True, expected=0
```

退出 1。仅在 /tmp 源码副本把 StateChanged 调用换为 `Notify(StateChanged, state)` 后，两组通过，退出 0；该对照用于定位原因，不是已交付修复。取票任务不会完成，Dispose 取消连接开启过程；探针不打开 TCP、不依赖服务端或 sleep 调度。直接用 .NET 10 Roslyn 编译目标源码，原有 CS8600 提示仍存在，不将结果当作完整项目编译门禁。

### 其他续审范围与证据边界

继续读了 GameClient 完成回调的连接代际过滤、EntityViews 提交与 Reset、Saga 人工 Resume／Compensate 的回执期限检查和超时更新路径。Saga 自动重试在超长停机／积压后可能超出回执保留期，main 已在 KNOWN-LIMITS 明确记录，本轮没有把该既有边界另报新问题；也没有验证出新的重复执行案例。

main 未变化，不重复宣称运行上一轮的全部测试；上一节的 Go race、心跳容量、晚应答及重连测试结果继续对应同一基线。本轮新增运行的是上述回调探针和临时修法对照。未验证 Unity、全仓或生产长稳。图谱工具不可用，以源码与 rg 继续；本轮未穷尽事务／Saga 的所有故障组合。

## 2026-11-01：心跳与回执修复关闭复验

基线：[main 5fe16dab](https://github.com/tjbdwanghaibo/roost-core/commit/5fe16dab)，为 v1.37.0 之后的修复提交，不将已发布 v1.37.0 描述为包含这两项修复。本节覆盖下方历史状态。

**结论：RV-20261101-04／05 已修复，修法合理；连同此前 01／02／03，五项原复现均已关闭。本轮继续审查没有确认新的缺陷。** 这不是全仓零缺陷或上线验收结论。

| 问题 | 代码核对 | 独立复验 |
| --- | --- | --- |
| 04：C# 心跳受业务容量挤占 | HeartbeatAsync 只供连接监控器使用，独立单槽；业务／Notify／显式校时仍共用原容量。心跳继续共用发送锁和序号，finally 释放各自槽位。 | 原 heartbeat_capacity.py 三组真实 TCP 对照全部成功，退出 0。另执行 main 的 HeartbeatCapacityTests：业务在途期间心跳成功、额外业务拒绝、容量释放后请求成功、后续心跳真正无应答会断线。 |
| 05：失败创建改写既有回执 | SetReceiptPayload 每次写入递增 receiptRevision，包括同值写；createdBuildMark 保存并在失败创建时比较，触发整条事务失败。AddReceipt 同键同摘要仍是无操作，新增项由数量检查覆盖。 | 原 receipt_payload_test.go.txt 的吞错／传播对照均通过；main 的六场景测试随 nest 包通过，覆盖同值、嵌套、成功创建和重复 AddReceipt。 |

### 继续检查的范围与结果

- 发送与连接：检查了心跳和业务的槽位释放、发送锁与序号共享、写期间取消、等待应答取消及监控器关闭路径。额外运行现有 LocalSession、Capacity、TestConnectionReconnect、TestReconnectFailureLimit：请求关联、Notify 取消、晚应答丢弃、非法预算不占槽、请求／Notify 容量、推送溢出断线、重连换票／代际和有界重试回调全部通过。
- 事务：复核回执新建与原位更新、失败嵌套创建以及成功创建的差别。receiptRevision 只提供失败构建的副作用检测，不伪装成可局部撤销的保存点；当前整条失败策略与既有 DAO 处理一致。
- Saga 与 Sync：继续核对 WaitTerminal 的查询／注册／补查与同生命周期终态过滤，以及发送预算和冷快照预算的组合、延后项 Full 回退、实际 Push 计费、Remove 放行。未确认新的错误；MaxWait 是老化优先级而非交付期限，不能将文档明确的软预算视作硬带宽保证。

### 本轮运行证据与限制

```bash
go test -race ./framework/nest ./framework/saga ./wiring/saga ./framework/sync/entitysync ./infra/network/gateway -count=1
python3 scripts/testdata/client-review/heartbeat_capacity.py /path/to/reviewed-main /path/to/dotnet-root
```

五个 Go 包全部通过（含原回执探针）；C# 原心跳探针全部通过。托管扩展测试使用 Linux / .NET SDK 10.0.402、runtime 10.0.13，直接 Roslyn 编译 main 的 Roost.Client 源码与上述测试方法，通过最小入口调用；NetworkExperienceTests 的时钟／插值／预测／trace 同样通过。测试方法来自 main，连接用回环 TCP；未跑完整托管项目的 NuGet／TreatWarningsAsErrors 门禁。编译仍有 TcpSession.cs 的 CS8600 提示，不能将此次选定测试通过当作项目零警告。

未重新运行本提交未改动的 native 客户端；其并发序号结果沿用上一节并明确基线。未运行 Unity/IL2CPP、WAN、真实 WAL/Mongo 崩溃恢复或生产长稳。回执验证仍使用记录型 committer，不扩大为持久层恢复证明。图谱索引工具不可用，继续以精确源码、差异和 rg 补证。本固定分支只更新审查材料。

## 2026-11-01：v1.37.0 修复复验与新增逻辑审核

基线：[main / v1.37.0 6bf23d6a](https://github.com/tjbdwanghaibo/roost-core/commit/6bf23d6a)。本节为最新状态；下方各节保留当时基线和失败证据，不能将历史“待修复”直接用于当前版本。

### 前三项修复结论

| 编号 | 当前结论 | 修复与复验依据 |
| --- | --- | --- |
| RV-20261101-01 | 原 DAO 路径已修复 | [dd2e8335](https://github.com/tjbdwanghaibo/roost-core/commit/dd2e8335) 为持久登记增加 revision，失败创建比对原有参与者代次；正式 Nest 回归通过，既有 DAO 再写不再随外层提交。下面另列回执 payload 遗漏。 |
| RV-20261101-02 | 已修复 | 同提交使 WaitTerminal 绑定首次权威查询的 Lifecycle，并在注册后重读补齐终态窗口；生命周期变化显式报错。原旧高版本终态探针以及新增生命周期边界测试通过。 |
| RV-20261101-03 | 已修复 | [572fd93d](https://github.com/tjbdwanghaibo/roost-core/commit/572fd93d) 将空队列检查与心跳序号分配移入同一互斥区，与业务准入串行；500 个并发业务请求、1ms 心跳的真实 TCP 用例通过，线上帧序号严格递增。旧调度插桩位置现已在锁内，不能照搬旧脚本并在此等待另一个准入线程。 |

这三项修复方向合理。修复已包含在 main，本分支只交付复验与问题材料。

### RV-20261101-04 / P2：C# 心跳容量问题仍未修复

在本次基线重新运行上节 heartbeat_capacity.py：关闭心跳且容量 1、开启心跳且容量 2 都完成 400ms 业务请求；开启 100ms 心跳且容量 1 仍发生 ObjectDisposedException，连接 Disconnected，LastError 为 Pending request capacity exhausted。业务超时 2 秒、心跳超时 1 秒，因此这不是服务端应答超时。

现行位置：[TcpSession.cs:96](https://github.com/tjbdwanghaibo/roost-core/blob/6bf23d6a/client/dotnet/Roost.Client/TcpSession.cs#L96)、[Connection.cs:264](https://github.com/tjbdwanghaibo/roost-core/blob/6bf23d6a/client/dotnet/Roost.Client/Connection.cs#L264)。新增时钟探测没有改变心跳共享容量的故障路径，建议及原探针继续适用。

### RV-20261101-05 / P2：失败创建修改既有回执 payload 仍可随事务提交

**新增确定复现；与已修复的 DAO 原位修改分别跟踪。** [createdBuildMark / abandonCreatedEntityBuild](https://github.com/tjbdwanghaibo/roost-core/blob/6bf23d6a/framework/nest/rollback.go#L1014) 对回执仍只记录数量；[SetReceiptPayload](https://github.com/tjbdwanghaibo/roost-core/blob/6bf23d6a/framework/nest/persist_change.go#L336) 原位替换 Payload，没有增加数量，也没有参与 DAO revision。

正式 Nest RollbackState / Strict handler 先 AddReceipt，payload 为 before；随后创建的 builder 调用 SetReceiptPayload 将同一回执改为 failed-build，再拒绝创建。handler 处理创建错误后正常返回。实际请求 err=nil，committer 收到 payload=failed-build，新实体没有发布。按照“失败创建留下自身 DAO 以外的持久副作用应令事务失败”的契约，应该 ErrFailedCreateLeftEffects 并禁止该提交，或者精确恢复原回执。当前两者都未发生。

影响限定：初始化代码会改动外层已绑定回执、且外层吞掉创建错误的组合，可能持久化来自失败初始化的幂等结果，后续重试读取错误结果。未声称所有 Saga 路径必然触发。传播创建错误的对照通过，提交记录中没有回执；临时规避是传播这类创建错误。

建议把 SetReceiptPayload 纳入失败创建的修改代次检查，或者为既有回执建立可恢复保存点；补齐同值替换、嵌套创建、成功创建和错误传播的边界。不要只对 AddReceipt 新增项计数。

探针：[receipt_payload_test.go.txt](../../scripts/testdata/entity-review/receipt_payload_test.go.txt)，复制到目标检出的 framework/nest/review_receipt_payload_test.go 后运行：

```bash
go test -race ./framework/nest -run '^TestReviewFailedBuild(ModifiesAlreadyBoundReceipt|ReceiptPropagatedError)$' -count=1 -v
```

当前吞错场景失败、传播错误场景通过；修复后两者应通过。使用正式请求与记录型 committer，验证的是提交内容，不代替真实 WAL/Mongo 恢复实验。探针以 .txt 交付，不向默认包测试加入已知失败测试。

### 新增逻辑与验证范围

审读了发送预算的优先级／等待老化／Full 回退／Remove 保留，以及客户端时钟、快照插值、预测历史容量和 trace；本轮没有在这些新增路径确认其他缺陷，不代表穷尽审核。

- `go test -race ./framework/nest ./framework/saga ./wiring/saga ./framework/sync/entitysync ./infra/network/gateway -count=1`：五包全部通过，包含 main 新增回归。此命令在另加回执失败探针前运行。
- Linux g++ 构建并运行 native sequence、network_experience、smoke 与 weak：全部通过，包括 500 请求序号检查、时钟／插值／预测、协议金样，以及心跳超时、截断、队列溢出和发送停滞。
- .NET 10 直接编译 main 未修改的 Roost.Client 源码和 NetworkExperienceTests.cs，用最小入口调用该测试：时钟／插值／预测／trace 通过。原源码有 CS8600 警告，本次不是项目 TreatWarningsAsErrors 门禁，也未跑完整托管测试工程。心跳容量独立探针仍按上述条件失败。
- 图谱索引工具不可用，依据精确源码与 rg 补证；未重跑全仓、Unity/IL2CPP、真实 WAN 或生产持续负载，不能据此宣布上线风险清零。

## 2026-11-01：续审 C# 客户端心跳与容量

基线：[main / v1.36.0 c31e65cc](https://github.com/tjbdwanghaibo/roost-core/commit/c31e65cc)。相对上一轮只新增验收入口与发版修改，RV-20261101-01～03 对应实现未变化，仍待修复。本轮新增一项独立问题；没有修改运行时代码。

### RV-20261101-04 / P2：C# 心跳因本地在途容量满而断开健康连接

**已通过真实回环 TCP 连续三轮复现，待修复。** [TcpSession.cs:96](https://github.com/tjbdwanghaibo/roost-core/blob/c31e65cc/client/dotnet/Roost.Client/TcpSession.cs#L96) 对心跳与业务请求共用 inFlight/maxPending，没有心跳保留容量；[Connection.cs:252](https://github.com/tjbdwanghaibo/roost-core/blob/c31e65cc/client/dotnet/Roost.Client/Connection.cs#L252) 的 WatchConnectionAsync 用普通 RequestAsync 发心跳，本地容量不足异常与网络故障同样返回给 MonitorAsync，后者在第 190 行 Dispose 连接。

触发：合法配置 MaxPendingRequests=1，业务请求超时 2 秒，心跳间隔 100 毫秒、心跳超时 1 秒。服务端已收到业务请求，在 400 毫秒后正常应答。期望本地饱和不使健康连接断开、已准入业务请求仍可正常完成；实际心跳在本地被拒，连接转为 Disconnected，LastError 为 Pending request capacity exhausted，业务请求报 ObjectDisposedException。心跳尚未发到服务端，不能将此情形归为服务端心跳无响应。

三轮对照均一致：

| 心跳 | 请求容量 | 400 毫秒业务应答 |
| --- | --- | --- |
| 关闭 | 1 | 正常完成 |
| 100 毫秒 | 2 | 正常完成 |
| 100 毫秒 | 1 | 本地容量满 → 连接断开 → 在途请求失败 |

业务影响：并发请求短暂占满容量时，开启心跳的 C# 接入会放大为整条连接失败，原本可以完成的交易／奖励请求变成结果未知，启用自动重连时还可能造成重连抖动。默认容量为 64，本轮用容量 1 缩小条件，不声称默认配置必然触发；缺陷条件是“心跳到期时业务占满容量”。也不将它套到 native C++ 心跳路径。

建议为控制消息保留有界容量，或者将本地准入失败与已发送后的心跳超时区分，并在明确期限内重新调度；业务请求仍须受原有容量保护。验收覆盖业务满载、心跳准入等待、服务端真正不响应、恢复后新请求正常，以及自动重连不会由本地正常背压触发。临时规避是额外限制业务并发，为心跳留槽位；仅调大总容量不能保证永不占满，不建议在未评估服务端空闲期限时直接禁用心跳。

复现文件：[heartbeat_capacity.py](../../scripts/testdata/client-review/heartbeat_capacity.py)、[heartbeat_capacity.cs.txt](../../scripts/testdata/client-review/heartbeat_capacity.cs.txt)。需 .NET 10 SDK；运行：

```bash
python3 /path/to/heartbeat_capacity.py /path/to/reviewed-main /path/to/dotnet-root
```

脚本在临时目录用 SDK 自带编译器编译被审查 main 的未修改 Roost.Client 源码和探针，无 NuGet 包依赖、不启动 SDK 首次迁移。运行成功对照两项、缺陷场景一项，当前退出 1；修复后应全部成功、退出 0。环境为 Linux、.NET SDK 10.0.402 / runtime 10.0.13；不是 Unity 或真实 Gate 验收。SDK CLI 本轮首次运行因尝试在只读 home 创建 NuGet 迁移锁而失败，直接调用编译器后完成验证，环境启动问题不登记为框架问题。

### 本轮其他路径与边界

检查了 NativeReconnect、NativeGameClient、C# GameClient/Connection、Lockstep 消费回调与按组同步提交回调。一个“同房间先 Join 后 Leave 的提交回调可能乱序”的猜测在双 worker 的正式 Nest 单实体路径未复现：后一个请求被前一个的调度顺序阻挡，不能据此登记新缺陷；这不代表已验证所有多实体或异步完成组合。未重跑全仓、Unity、IL2CPP 或长稳压力测试。上轮问题仍沿原编号跟踪。

## 2026-11-01：昨天新增代码与修复复核

基线：[main e2cda3c4](https://github.com/tjbdwanghaibo/roost-core/commit/e2cda3c4)，包含 v1.36.0 客户端与验收边界文档。相对上次基线有 1522 个文件变化（含大量历史文档整理），本轮重点审查 Saga 生命周期、创建失败事务边界、按组同步、弹性成局和客户端发送顺序，不声称逐个审完所有文件。

**旧问题复核**：操作键加入持久 Lifecycle 后，新一轮与旧回执／完成命令隔离；Register 与 wiring 配置入口验证回执保留期至少为每步重试窗口的两倍，原短保留配置被拒；失败创建会撤销自身 DAO 登记。原问题的对应回归通过，修复方向合理。超长停机超过回执窗口仍是文档明确的边界。创建失败对“其他已登记对象”的处理、终态等待对新 Lifecycle 的处理仍有以下遗漏。TCP 快照夹具修复后的包测试也已通过。

### RV-20261101-01 / P1：失败创建修改已登记 DAO 时，副作用检查漏报

**已复现，待修复；RV-20261031-01 修复的边界遗漏。** [rollback.go:1012](https://github.com/tjbdwanghaibo/roost-core/blob/e2cda3c4/framework/nest/rollback.go#L1012) 的 createdBuildMark 只记录各列表长度；abandonCreatedEntityBuild 通过新增参与者数量判断是否动了其他实体。MarkPersist 对已登记参与者更新原来的 PersistChange，不增加列表长度，因此检测不到。

复现：真实 Nest RollbackState/Strict handler 先把自己的 DAO 写为 5 并登记；随后 Create 的 builder 把该 DAO 改为 77，再返回初始化失败；handler 处理错误后成功返回。期望按当前契约返回 ErrFailedCreateLeftEffects、整条回滚到初始值 1；实际请求 err=nil，内存和提交记录都是 77。新实体自身 DAO 已被移除，遗漏的是其他实体的修改。房间／公会创建失败时，由初始化修改的玩家状态可能仍被提交。

建议不能只比较集合长度：为构建期间每次持久登记记录触及对象／修改代次，或建立能覆盖既有参与者的保存点；其他实体有修改但不能准确撤销时令整个事务失败。验收补“进入创建之前已经登记的 DAO 再写”，并检查同类原位更新入口（例如已有回执 payload），后者本轮未单独复现。临时规避仍是把可能产生外部副作用的创建错误向上传播，不正常提交外层事务。

探针：[existing_participant_test.go.txt](../../scripts/testdata/entity-review/existing_participant_test.go.txt) → 最新 main 的 `framework/nest/review_existing_participant_test.go`。`go test -race ./framework/nest -run '^TestReviewFailedBuildModifiesAlreadyRegisteredDAO$' -count=1 -v` 失败。使用正式 Nest 请求路径与记录型 committer，未把提交记录验证扩大为真实 Mongo 冷恢复。

### RV-20261101-02 / P1：WaitTerminal 可返回上一生命周期的终态

**已复现，待修复；生命周期隔离在终态等待路径遗漏。** [terminal_watch.go](https://github.com/tjbdwanghaibo/roost-core/blob/e2cda3c4/framework/saga/terminal_watch.go#L83) 的 offer/newerThan 只比较 Version，WaitTerminal 从 getOwned 取得记录后也只保存 Version，没有绑定 Lifecycle。

同业务键重开后版本从头开始；等待者已登记，查询得到 NEWLIFE 的 Waiting v2，此时落后的终态 follower 交来 OLDLIFE Completed v10。即使又收到 NEWLIFE Completed v4，best 仍保留 v10，WaitTerminal 返回 OLDLIFE 的成功。业务等待本轮跨服转账／结算时可能提前把上一轮成功当成这轮结果；这不表示协调器本轮状态已经被旧完成命令推进（那条修复已生效）。

探针：[terminal_lifecycle_test.go.txt](../../scripts/testdata/saga-review/terminal_lifecycle_test.go.txt) → 最新 main 的 `framework/saga/review_terminal_lifecycle_test.go`。`go test -race ./framework/saga -run '^TestReviewWaitTerminalRejectsPreviousLifecycle$' -count=1 -v` 失败。使用 stub query 与直接终态观察，确定性模拟查询回包期间 follower 追上旧事件；未做真实 JetStream 积压时长测量。

建议等待者与已查询的 Lifecycle 绑定，只在同一生命周期比较版本，并明确尚未查到记录时等待哪一轮；不能跨轮用最大 Version 合并。验收包含旧高版本先到／后到、新低版本终态、同轮 Resume、查询暂不可用和取消。临时规避是对 WaitTerminal 结果核对 StartSaga 返回的 Lifecycle；不匹配不能视为本轮完成，使用权威查询核实。

### RV-20261101-03 / P2：Native 心跳与并发请求可能倒序发送序列号

**受控交错下已复现，待修复。** [client.cpp:200](https://github.com/tjbdwanghaibo/roost-core/blob/e2cda3c4/client/cpp/src/client.cpp#L200) 在 mutex 内发现 commands 为空，解锁后才为心跳分配 sequence；admit 在另一线程可以先取得较小序列并入队。网络线程仍发送较大心跳，再取出较小业务请求。服务端 [tcp.go:469](https://github.com/tjbdwanghaibo/roost-core/blob/e2cda3c4/infra/network/gateway/tcp.go#L469) 要求包括心跳在内的请求序列递增，会关闭连接。

真实回环 TCP 捕获为 `(message_id, seq) = (0,1), (0,3), (42,2)`。诊断探针只在临时源文件的“空队列检查解锁后、心跳取号前”插入调度钩子，让独立应用线程调用正式 roost_request 并完成入队；不改分配、排队和发送算法。**这是受控调度复现，不是未插桩压力测试，也未测自然触发频率。** Python 对端只记录并应答帧，服务端断开结论来自已核对的正式序列校验，不冒充真实 Gate 联测。

探针：[native_sequence.py](../../scripts/testdata/client-review/native_sequence.py)。需 g++ / Python 3，运行 `python3 /path/to/native_sequence.py /path/to/reviewed-main`，当前输出上述倒序并断言失败。修改只在临时目录，结束自动清理。

建议把“选择下一条发送内容”和“心跳分配序列”放进与请求准入同一临界区，或由网络线程按最终发送顺序统一编号（须保留公开请求 handle 的取消／关联契约）；不能仅依赖 atomic 保证线上顺序。不建议关闭服务端防重放检查。该问题影响使用 native 网络路径的 Unity/C++ 接入，不能推广为纯 C# TcpSession 的同一缺陷。

### 验证与交付范围

`go test -race` 的 Saga、wiring/saga、Nest、Entity、GroupView policy、wiring/selfsync、Lockstep 七包原有测试通过；另运行 service/match、wiring/match、wiring/nest 三包 race 测试也通过。新增两个 Go 缺陷断言失败，未出现 race 报告。Linux g++ 直接构建未修改的 native 源码并运行仓库 smoke、lockstep、weak 三套 Python 回环驱动通过，覆盖既有同步金样、请求／心跳、快照与弱网场景；没有 Protobuf 可选 C ABI、Unity、IL2CPP 或真实 Gate 的本轮验证。只提交审查证据，不修改运行时、不发 PR/issue。

## 2026-10-31：继续审查持久实体创建与 TCP 恢复

基线仍为 [main v1.33.0，030a9f11](https://github.com/tjbdwanghaibo/roost-core/commit/030a9f11)，本轮 fetch 无新增提交。RV-20261030-01、02 两项 Saga 风险仍未修复；本次新增一个独立问题，并缩小上轮 TCP 测试失败的原因范围。只提交问题与证据，不修改运行时。

### RV-20261031-01 / P1：实体构建失败后，其 DAO 仍随外层成功事务提交

**类别：框架事务边界；状态：已复现，待修复。** [entity_factory.go:334–343](https://github.com/tjbdwanghaibo/roost-core/blob/030a9f11/framework/entity/entity_factory.go#L334) 在 builder 前绑定聚合，允许 Init 登记持久修改；builder 失败直接返回。[manager_factory.go 的 CreateInScope](https://github.com/tjbdwanghaibo/roost-core/blob/030a9f11/framework/entity/manager_factory.go#L79) 在 build 失败时直接返回，没有撤销该次初始化登记，也没有令当前事务失败。稍后的 CaptureCreatedEntity 保护只覆盖成功构建、发布后的捕获失败，覆盖不到此路径。

触发：在持久 Nest handler 中 Create；builder 初始化 DAO 并通过 MarkPersist 登记，再因校验失败返回错误；业务处理该错误并返回一个正常业务结果。期望失败创建的 DAO 不出现在提交中（或者框架令整个事务回滚）；实际 handler 成功，Manager.Get(id) 为 nil，但 committer 收到该 ID 的版本 0→1 Put，载荷为 value=99。这样可能持久化未完成初始化的角色、房间或公会，后续加载／重试会面对“之前创建失败却已有数据”的状态。不能将 API 返回创建错误等同于这次创建没有持久副作用。

两条复现：GuardScope + RunIsolatedTransaction，以及真实 Nest Engine 注册的 RollbackState/Strict handler + Request。均出现失败实体的 Put；对照将创建错误向上传播，事务回滚且没有该 Put。复现使用仓库已有带 Tracker 的 DAO 夹具和 recordingCommitter，**证明提交边界收到错误记录，不冒充真实 Mongo/WAL 冷恢复验证**。普通与 race 执行的缺陷断言都失败，没有观察到数据竞争报告。

探针：[create_failure_test.go.txt](../../scripts/testdata/entity-review/create_failure_test.go.txt)。在该 main 的独立检出复制成 `framework/nest/review_create_failure_test.go`，运行：

```bash
GOWORK=off go test -race ./framework/nest -run '^TestReviewFailed' -count=1 -v
```

当前结果：isolated 与 handled_error 两个断言失败，propagated_error 对照通过。建议为从初始化开始的创建过程建立可撤销事务边界，或在已登记副作用后构建失败时将外层事务标为失败；不要只清理 Manager 索引，因为此时尚未发布。验收覆盖 builder／组件 Init／OnInitFinish 失败、业务处理错误后继续、重复 ID 发布失败，以及普通成功创建。不能笼统把所有 ErrEntityExists 都变成不可恢复错误而破坏 GetOrCreate 契约。

临时规避：持久 handler 内初始化可能写 DAO 时，Create 的初始化错误必须向上传播并回滚；不要把失败转换为正常结果后提交。独立的补偿／告警应在失败事务结束后执行。

原有测试对照：`GOWORK=off go test -race ./framework/entity ./framework/dataengine/engine ./framework/remoteentity ./framework/nest -skip '^TestReview' -count=1` 四包通过（分别 2.838、4.873、32.061、5.876 秒）；外部依赖门控测试的跳过不算集成验收。本轮没有重跑全仓或生产环境压测。

### TCP 快照门禁：已缩小到测试夹具的平台假设，未确认运行时缺陷

上轮原始测试的两项失败仍是真实失败。本轮保持运行时代码不变，分别做诊断性对照，测试文件随后恢复原样：

- 仅把 written_lane 在服务器追帧结束之后等待客户端收齐的预算从 5 秒改成 60 秒，原 4 MiB 快照、校验字节／哈希／无帧洞／另一连接存活等断言全部通过，耗时 27.97 秒。服务器“已写出”不等于客户端已经读完，原 5 秒预算在当前 Linux 小接收窗口下不足。
- 仅将 admission-only 对照快照改为 16 MiB 并同步设置 SnapshotConfig.MaxBytes，只运行该子测试，0.41 秒通过溢出断言。原 4 MiB 不保证超过应用出站队列和内核 socket 缓冲的合计容量；仅比较应用队列约 2 MiB 不足以建立这个对照。

这两次对照支持测试夹具依赖平台缓冲与时间假设，不等于完整弱网验收，更不能据此把原失败改写成通过。建议夹具显式控制缓冲／背压，并分别观测服务器写出完成与客户端收齐；保留原有业务断言，不简单删测试或无限延长超时。当前没有足够证据将这项登记为框架丢快照。

## 2026-10-30：上线前 Saga 与验证门禁审查

当前复验基线：[main v1.33.0，030a9f11](https://github.com/tjbdwanghaibo/roost-core/commit/030a9f11)。固定审查分支仅提交报告与探针，不合入 main、不修改运行时；交由另一位 agent 修复。此前原型的三项问题已由正式实现的独立启动尝试身份、多等待者和准入初始化门处理，不再作为当前缺陷。

### RV-20261030-01 / P1：终态清理后重用业务键，旧回执与迟到结果串入新 Saga

**已在当前基线复现，待修复。** `framework/saga/transition.go` 的 `newStartRecord` 重置尝试代际；`operationKey` 只含 Saga ID、方向、步骤，`commandID` 再加尝试序号。Saga ID 来自类型与业务键。文档允许终态保留期结束后同业务键开始新一轮，但新旧两轮的操作身份相同。

触发：旧 Saga 完成，终态被清理，参与方成功回执尚在；同业务键启动新一轮。期望新业务实际执行，实际 `StepReplayed`，业务执行数不变；若旧完成事件迟到，还能把新一轮从 step 0 推至 1，并把 Data 改为 old-result。跨服奖励、交易等会出现漏执行或串轮推进。

建议：每次新建生命周期持久化唯一身份，贯穿操作键、命令、完成事件与回执；Resume 同一生命周期须保留成功操作的去重语义。临时规避是每轮使用永不复用的业务键。验收必须覆盖终态裁剪、旧成功回执、迟到完成事件、重启与 Resume，不能仅增加内存计数。

### RV-20261030-02 / P1：允许重试窗口超过回执保留期，清理后同操作再次生效

**已在当前基线复现，待修复；属于非默认自定义配置。** `wiring/saga/config.go:171` 与 `framework/saga/engine.go` 只检查终态保留期不超过回执保留期；Register 没检查实际步骤预算。`step_receipt.go` 的 `pruneStepReceipts` 会按回执时间清理，常规重试没有相应过期保护。

配置回执与终态保留 60 秒，步骤超时 10 秒、最多 10 次、固定退避 10 秒，最长预算 190 秒，`app.CheckConfig` 接受。第一次业务已提交但完成结果未送达；同实体另一操作在第 61 秒清理旧回执；第 62 秒投递期限内的第四次尝试，实际执行数由 1 变成 2。期望同一操作只生效一次。

复现使用真实 Nest 隔离事务与 NativeStep Begin/Complete、受控时钟；没有模拟真实网络故障或 Mongo TTL。默认回执保留 168 小时，不能将此结果描述为默认配置必然重复扣款。

建议：按实际解析后的每步预算验证保留期，协调参与方与协调器配置，并为超期停机、延迟投递、恢复后回执证据缺失定义拒绝或人工对账策略。只验证理想重试时间仍不足以覆盖无限停机。临时规避是禁止短回执保留配置，按最大恢复与重投时间留余量。

### 复现入口

将本分支以下文本复制到所审查 main 检出中（不要在旧固定分支直接执行）：

- [lifetime_test.go.txt](../../scripts/testdata/saga-review/lifetime_test.go.txt) → `framework/saga/review_lifetime_test.go`
- [retention_test.go.txt](../../scripts/testdata/saga-review/retention_test.go.txt) → `framework/saga/review_retention_test.go`
- [config_retention_test.go.txt](../../scripts/testdata/saga-review/config_retention_test.go.txt) → `wiring/saga/review_retention_test.go`

运行 `GOWORK=off go test ./framework/saga ./wiring/saga -run '^TestReview' -count=1 -v`。当前基线四个期望正确行为的断言均失败；修复后应全部通过。文本探针不进入普通测试集，最终回归应合入正式测试。

### 验证结果与上线条件

此前基线 [3b06d98f](https://github.com/tjbdwanghaibo/roost-core/commit/3b06d98f) 在 Linux / Go 1.27.2 下全仓 build、vet 通过，完整 `go test ./...` 失败于 `framework/sync/lockstep` 的 `TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive`。该基线独立复跑两次、最新 v1.33.0 再复跑一次均失败：只准入对照没有溢出，等待写出的恢复路径最终 snapshot=false、provider calls=1。**这是可重复的验收失败，根因尚未确认，不能直接认定框架丢快照。** 应检查 Linux socket 缓冲、写出与客户端收齐的差异、超时和夹具假设，禁止仅删断言使门禁通过。

同一旧基线 Saga/Nest/NATS 定向 race 通过；私有 NATS Server 2.12.3 三节点的正向补偿、发布结果未知、旧写者隔离、快照重放、迟到成功五项集成通过。未做真实 Mongo 或跨机故障验证。最新 v1.33.0 四个 Saga 探针已复验，不能将旧基线全仓结果冒充最新发布验收。

上线前还需完成：

1. 修复以上资产风险并保留修前失败、修后通过证据，解释并修复 TCP 快照测试失败。
2. 用正式生成消费者在目标 Linux 部署拓扑验证真实 Mongo、三节点 JetStream、进程重启和依赖故障下的资产守恒。现行 Saga 性能报告主要是 macOS 单机，不代表生产容量。
3. 结合准入限流验证持续负载与投影追平目标。现行 KNOWN-LIMITS 已记录云 Mongo c128 追平 45.9 秒、strict64 39.8 秒超过 30 秒目标；正向容量档不开准入，不能直接当作生产配置结论。
4. 发布前明确版本支持、数据迁移与回退兼容政策；维护手册目前仍保留“线上尚未部署、可破坏旧格式”的阶段约定。

结论：可以准备有明确边界的候选版试运行；当前证据不足以按已经充分验证的生产级资产保证大规模推广。以上缺口不等同于所有游戏模块不可用。

## 2026-10-28：main JetStream Saga 原型审查（历史问题，正式实现已处理）

2026-10-30 复核：以下保留的是当时报告与失败证据，文内“待修复”是历史状态；当前正式实现已处理这三项，当前风险见上节。

审查基线：[main `4170bcad`](https://github.com/tjbdwanghaibo/roost-core/commit/4170bcad)。以下三项属于 `scripts/perf/saga-jslog-proto` 实验包，不代表生产 `framework/saga` 的同名缺陷。用户安排另一位 agent 修复，本次只提交问题与复现输入，没有修改运行时代码。固定审查分支未为本次文档提交合入最新 main；复现必须在该 main 基线或其后继版本运行。

### RV-20261028-01 / P1：客户端重建后，拒绝重试被 JetStream 去重吞掉

**状态：已复现，待修复。** 位置：[client.go:142](https://github.com/tjbdwanghaibo/roost-core/blob/4170bcad/scripts/perf/saga-jslog-proto/client.go#L142)。`MsgID` 只由 Saga ID 和 Client 内存计数组成，新的 Client 从 1 重新计数。

触发：同 ID 首次启动因积压被拒绝；在流的两分钟去重窗口内重建客户端，读取旧 Rejected 终态；准入恢复后用同 ID 重试。期望写入新的启动意图，协调器重新判定；实际 Publish 返回成功，但流中最后一条启动意图序号仍为 1，新请求没有入流。客户端已经删除旧终态并登记新等待者，可能一直等不到结果。影响是失败重试不执行，并非已证实重复扣款。

原始失败：`retry acknowledged but not appended: start seq remains 1; restarted client reused MsgID`。

建议：启动尝试身份加入客户端实例唯一标识或独立的全局唯一请求 ID；一次发布内部重试保持同一 MsgID，不能每次网络重发都换身份。修复验收覆盖客户端重建、不同客户端同 ID 拒绝重试、同一次发布结果未知重试不重复追加。

### RV-20261028-02 / P2：同 ID 在途重试覆盖第一次调用的等待通道

**状态：已复现，待修复。** 位置：[client.go:130](https://github.com/tjbdwanghaibo/roost-core/blob/4170bcad/scripts/perf/saga-jslog-proto/client.go#L130)，尤其 `c.wait[id] = ch`。

触发：同一 Client 连续两次 StartIntent 同一 ID，两次均发生在终态到达前。期望两位等待者都能观察终态；实际第二次调用覆盖第一条通道，终态消费者仅关闭第二条，第一条永不关闭。复现直接发布合法终态事件以隔离客户端等待逻辑，不依赖转账状态机或执行时延。

原始失败：`first caller stranded after second StartIntent overwrote its waiter`。

建议：复用同 ID 的在途通道或保存全部等待者，同时明确取消／发布失败的清理方式。修复验收包含同时和先后重试、终态到达竞争、Rejected 后发起新一轮等待，避免旧拒绝回执误结束新一轮。

### RV-20261028-03 / P2：准入初始 KV 快照未就绪时默认放行

**状态：初始化判定已复现，待修复。** 位置：[admission.go:98](https://github.com/tjbdwanghaibo/roost-core/blob/4170bcad/scripts/perf/saga-jslog-proto/admission.go#L98)、[coordinator.go:141](https://github.com/tjbdwanghaibo/roost-core/blob/4170bcad/scripts/perf/saga-jslog-proto/coordinator.go#L141)。`newAdmission` 初始 open=true；WatchAll 异步读取积压，收到初始结束标记才设置 ready，但 evaluate/admitStart 没有检查 ready。

触发：协调器启动／接管期间，已有参与方积压超过阈值，KV 初始读取尚未完成。空的本地 loads 被当作“没有需要保护的投影”，新 Saga 可能被放入。期望初始快照读取前关闭准入或等待初始化；实际 ready=false 时 evaluate 直接返回 open。复现是无网络的状态单测，尚未测量真实重启时窗口长度或超额准入数量。

原始失败：`admission opens before initial KV load snapshot is read`。

建议：初始状态 fail-closed；watch 初始结束后才允许按实际空表放行，避免把“未知”混成“已确认无参与方”。验收用受控 watcher 延迟初始快照，确认高积压期间不派发新步骤；初始快照为空时可恢复放行，已在途 Saga 的补偿／重试不受影响。

### 复现与修复验收入口

复现代码：[review_regression_test.go.txt](../../scripts/testdata/saga-review/review_regression_test.go.txt)。保存为文本以免把预期失败的审查探针直接纳入主分支测试，也避免当前固定分支缺少实验包导致无法编译。修复 agent 应在 main 检出中将其复制到实验包，并把最终回归合入对应现有测试文件。

```bash
# 在 main 4170bcad 或后继版本的独立检出中；复制本提交的探针文件到以下位置：
# scripts/perf/saga-jslog-proto/review_regression_test.go
# 选择专用数据目录及未占用端口，勿使用共享环境。
export SAGAJS_ROOT=/tmp/saga-review-private
export SAGAJS_PORT_BASE=29622
bash scripts/perf/saga-jslog-proto/nats-cluster.sh start
export SAGAJS_URL=nats://127.0.0.1:29622,nats://127.0.0.1:29623,nats://127.0.0.1:29624
go test -count=1 -run '^TestReview' -v ./scripts/perf/saga-jslog-proto
# 基线预期：三个 FAIL；修复后应全部 PASS。
bash scripts/perf/saga-jslog-proto/nats-cluster.sh stop
```

本轮环境 Linux、Go 1.27.2、NATS Server 2.12.3 私有三节点，nats.go 1.53.1；原型历史报告使用的 Server 2.14.5 不应混为本轮环境。前两项通过真实 JetStream 复现，第三项为直接初始化状态断言。三项探针共 0.582s，均按期望行为断言失败。

对照验证：原有 `TestEndToEndForwardAndCompensate`、`TestEndToEndDuplicateCommandsAndResults`、`TestEndToEndLateSuccessReopensAndCompensates`、`TestFenceRejectsStaleWriter`、`TestSnapshotPurgeReplayMatches`、`TestPublishOutcomeUnknownIsResolved` 在同一私有集群 `-race -count=1` 全部通过（包耗时 8.627s）。此外 `go test -race ./framework/saga ./wiring/saga ./scripts/perf/saga-jslog-proto` 默认环境通过，依赖外部服务的跳过项不算验收。真实 Mongo 投影、生产 Redis 全链路、跨机及容量不在本轮已验证范围。集群已停止，主工作区运行时代码未改。

## 2026-10-22：历史审查

审查日期：2026-10-22。同步时 main 与 origin/main 均为 `13bc939e`；范围为上一轮审查提交 `a73c75cf..13bc939e`，413 个提交、无合并提交，净变化 1335 个文件，其中 Go / Go 模板 651 个路径。在独立 worktree 审查最终代码状态，没有逐个运行历史中间版本。上一轮报告见 [固定提交](https://github.com/tjbdwanghaibo/roost-core/blob/a73c75cf/docs/maintenance/COMMIT-REVIEW.md)。

## 确认缺陷

三个问题均通过临时探针复现，已登记到 [KNOWN-LIMITS](KNOWN-LIMITS.md)。状态：RV-20261022-01 **已修复**（`6edebb13`）；RV-20261022-03 **已修复**（`b7b52486`）；RV-20261022-02 **已修复**（`47691000`）。探针调用产品代码；运行后从包内删除，不提交一次性测试。后续修复应把行为回归并入现有测试文件。

### RV-20261022-01 / P1：批量执行绕过组内新增 Fence

**状态：已修复**（`6edebb13`）。每个 handler 开始前的检查收在 `NestMgr.checkHandlerStart`（fence + 取消），普通派发、广播与批量作业逐目标共用（可读性建议 1）；广播同样只在作业开始时检查一次，一并修复；同组先取锁失败、收尾时才转单目标的目标在 fence 后也交回 `ErrNestFenced`。两种触发（handler 主动 Fence、注入 committer 返回结果未知）的回归、红文本、变异与 benchstat 见 [v1.31.0 实现篇 H2](../release/v1.31.0-IMPLEMENTATION.md#h2)。

位置：`framework/nest/try_batch.go:398` 的 `runTryBatchLocked`，逐目标执行在 410–415 行。`prepareTryBatchGroupLocked` 在准备一组时读取 Fence；随后每个 `tryBatchExec.run` 没有再次核对。`invokeTryBatchTransaction` 可以因提交结果未知触发 Fence，因此该状态能在一组执行期间变化。

复现：单 worker、两个不同 ID 的内存 handler；第一个调用公开的 `NestMgr.Fence`，第二个记录调用次数。Shutdown 等待收尾后断言第二个没有执行且通过 HandBack 返回 `ErrNestFenced`。实际：

```text
handler after fence=1, second target handback=<nil>; want 0 and ErrNestFenced
```

Fence 允许已经执行的 handler 收尾，不允许尚未开始的目标继续执行业务。现有 `TestTryBatchRefusesTargetsAfterFence` 覆盖组准备前 Fence，没有覆盖组内变化。修复验收应包括组内主动 Fence 和真实 committer 返回结果未知两种触发；本轮探针只执行前者，后者是源码调用链判定。

### RV-20261022-02 / P2：旧重投被当成新一拍

状态：**已修复**（`47691000`）。按审查建议 2 把登记状态、投递代际、重试次数、正在执行分开表达：每次投递分配代际、凭据随消息交给 Nest，重投沿用原代际，执行与交回只结算自己那一代，其余按 `duplicate` 丢弃；旧重投落在下一拍之后、停止后重启、批量异步交回（下一拍投出后、执行期间）与 `Reg.Close` 的交错在两种投递方式下有回归，修前红文本与变异核对见 [v1.31.0 实现篇 R2](../release/v1.31.0-IMPLEMENTATION.md#r2)，现行契约见 [impl/10-time §2B 第 4A 条](../framework/impl/10-time.md#2b-实体心跳)。

微基准问题（每拍多一次 32 字节凭据分配，`allocs/op` 2 → 3）**已决定**：维护者 2026-10-22 选方案 A——“投递凭据按当前实现，每拍分配一个，简单方便”，不做凭据回收复用。理由：队列里晚到的旧消息仍持有自己的凭据，复用需要再加一套归还协议，才能保证旧消息读到的永远是旧代际；按 H2 的实测，这次分配约占单目标 Nest 消息每拍 CPU 的 1% 以内（估算）。

位置：`gameplay/heartbeat/host.go:374` 的 `runBeat` 与 `driver.go` 的 `step`。重投和新拍携带相同的 `*Reg`；runBeat 只检查 `inflight` 并在返回时清零。

确定顺序：驱动投递原始拍 → 等待超过兜底阈值再投递旧拍 → 执行原始拍 → 驱动投递新拍 → 执行队列中的旧重投。实际：

```text
stale retry executed: calls=2 new beat inflight=false; want 1,true
```

旧重投执行额外回调并消费新拍的在途状态，新拍随后被当成 duplicate。探针使用包内 fake dispatch 控制交付顺序，但调度 step 与 runBeat 均为产品实现；没有声称跑了真实 Nest 排队压力。修复需按投递代际识别执行和交回，补旧重投、下一拍、停止重启以及异步 HandBack 的交错验证。

### RV-20261022-03 / P2：Drain 诊断越过锁边界读事实计数

位置：`framework/sync/entitysync/policy/interest_queue.go:665`。`drainStalls` 在 664 行释放 registry 锁后读取 `r.facts`；`queueFact` 在 90 行持 registry 锁更新该字段，不持 Interest 应用锁。

复现：ApplySync 模式，Show 后执行一次 applyStep 形成 applied-unsettled 状态，并发 QueueMove 与 drainStalls。`go test -race` 明确报告 665 行读和 90 行写冲突。普通已 settle 状态会短路该读取，不能用那个状态下的 race 通过否定问题。

影响是诊断数据竞争及不一致读取；没有证据证明此读会破坏 AOI 位置或订阅。修复在 registry 锁内捕获计数，再生成诊断，保持锁顺序。

**状态：已修复**（`b7b52486`）。`drainStalls` 改为在 `registry.mu` 内取一次 `drainSnapshot`（日志里的事实与提交状态、事实数、命令数、预算占用），锁外只用这份快照；同时按可读性建议 3 把应用方一步拆成 `takeLog` → `executeLogLocked`（含 `recordStepLocked`）→ `requeue`，结算与 `queuedPending` 也只经 `releaseFacts` / `backlog` 跨锁，锁序与切点 / 预算规则不变。回归 `TestDrainStallsReadsFactCountUnderTheRegistryLock`（`interest_test.go`，两种模式，applied-unsettled 状态下并发 `QueueMove` 与 `drainStalls`）在修前 `-race` 报 `interest_queue.go:665` 读与 `:90` 写，修后 `-race -count=50` 通过；policy 基准同机交替 12 轮无显著差异（[样本](../performance/benchmarks/interest-rv03-20261022/)）。同类扫描：policy 包内其他 registry 字段的锁外读取没有发现（`view.handle` 构造后不变，`aoi.config` / 块几何构造后不变）。约束记入[单写者方案 §11.3 第 16 条](REFACTOR-2026-10-21-interest-single-writer.md#113-执行器批次与结算这一层剩下的约束以及钉住它们的用例2026-10-22)。

## 可读性与设计建议

1. **Nest 批量执行的契约入口统一。** 批量与普通派发可以保留各自调度方式，但每个 handler 开始前的 Fence、取消、实体有效性检查应有明确统一的职责，避免准备阶段检查被误认为覆盖执行阶段。不为统一而添加新包或额外分配。**已按此实施**（`6edebb13`）：fence 与取消收在 `NestMgr.checkHandlerStart`；实体有效性依赖取到的实体，仍由各路径逐目标做（只读已加载 + Touch、取锁拒绝已摘除实例、锁组核对），契约写在同一处注释与 impl/02；没有新包，热路径不增分配。
2. **Heartbeat 显式表达投递身份。** 登记状态、投递代际、重试次数和正在执行状态含义不同；单一 inflight 不能表达全部。先明确状态转换和归还所有权，再整理调度、投递、观测职责到同包文件。
3. **Interest 显式区分锁内状态与锁外快照。** registry 记录事实、应用器更新空间、结算生成关系、Drain 读取诊断，各阶段用小函数表达；跨锁边界传值，不依赖读者记住某字段究竟由哪把锁保护。维持现有分层，不额外增加入口包装。
4. **源码注释描述现行规则。** movement、heartbeat 等新代码仍有 D20、决定编号、日期与过程叙述，应迁入设计文档；保留中文锁边界、失败语义、事务发布顺序。长函数按准备、执行、收尾拆分，不按历史优化轮次组织。
5. **MapBasic 的保存语义需更直观。** 单独 SetFacing 后 Save 不调用保存回调，探针已复现；但 Save、dirty 与使用文档明确限定为位置/场景变化，所以本轮不登记为确定 bug。建议明确“原地转向是否必须持久化”：若需要，dirty 应包含朝向并支持事务回滚；否则文档明确朝向仅随位置保存，避免 Save 回调包含 facing 造成误解。**维护者 2026-10-22 决定：朝向作为 DAO 数据，已实施**（`169a7865`）：`MapBasic.SetFacing` 与 `SetPos` 同一机制——`guard` 登记 `mapState` 回滚、标 dirty、写 `Fields`（DAO 字段的逆操作回滚朝向值）、提交后发布；`Save` / `SaveOnLeave` / `SaveEvery` 在只改朝向时也调保存回调，朝向和位置一样不每拍落库。game-demo `PlayerDao` 新增持久 `saved_facing`，保存回调写它、进场景时恢复。回归 `TestFacingIsSavedAndRolledBackLikeThePosition`（`gameplay/movement/nest_test.go`），修前红：`Save after SetFacing alone: saved [], want [1@7,7/9000]`；三处变异（去掉 dirty、去掉 `guard`、`MemFields.SetFacing` 不登记逆操作）各让它变红。契约见 [impl/10-time §2C 第 12 条](../framework/impl/10-time.md#2c-移动与动作的心跳驱动)，说明见 [v1.31.0 说明 R5](../release/v1.31.0-NOTES.md#r5)。

方向判断：本轮两个执行时序缺陷都来自状态含义不够明确：批组已准备不等于每个 handler 已开始，登记在途不等于当前投递属于哪一拍。优先修正这些状态和检查边界，不继续增加时间阈值或重试分支。热路径可读性改函数体仍需遵守维护手册的同机交替至少 10 次 benchmark/benchstat 要求；本轮没有做性能改动或宣称性能收益。

## 验证与范围限制

已运行 `GOWORK=off go test ./... -count=1 -timeout=10m`，退出码 0（临时探针加入前启动，日志中没有探针测试）。受影响包现有 race 回归 `go test -race ./framework/nest ./gameplay/heartbeat ./framework/sync/entitysync/policy ./gameplay/movement -count=1 -timeout=4m` 退出码 0。三个新增探针分别按预期失败，说明现有全仓通过不能覆盖上述交错。

重点检查了 Nest TryBatch/Fence、Heartbeat 调度重投、Interest 单写应用/多视野/Drain、Sync 会话版本与交付结算、Movement 事务发布和保存，以及 DataEngine 投影/回放/checkpoint、Remote 数据库作用域、Saga 延迟确认相关改动。没有对全部 413 个提交逐个背书，也未逐个审计所有新增 service 功能、生成模板和 C# 客户端；没有执行真实 Mongo/Redis/NATS 故障矩阵、长稳压测、Linux 验证或性能 AB 对比。

CBM 项目为 `Users-whb-roost-roost-core`，当前可用索引代际落后于 HEAD。主动刷新被共享后台协调状态阻塞（worker exit=1），未重启共享服务。对 651 个变动 Go/模板路径做 coverage 检查后以当前源码与 diff 补证；没有把“无记录缺口”视为新代码完整索引。

本地原始日志、提交清单和探针源码备份保留在 `artifacts/review/20261022-main/`（忽略目录，探针后缀为 .txt，不参与 Go 包枚举）。文档记录结论与复现条件，不将本机证据路径当作其他环境已经完成的验证。
