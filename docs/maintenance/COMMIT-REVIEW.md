# 新增提交审查

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
