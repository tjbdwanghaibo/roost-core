# 测试有效性审计（2026-10-29）

维护者要求：“整体检查下现在的test有些是否已经失效有些是否可以删除了”。在 saga JetStream 切换（第 1～6 批，main `4e2ff44c`）之后，按区域由四个 agent 并行审计，基线同为 `4e2ff44c`。

| 区域 | 记录 | 删除 | 加强 | 待维护者决定 |
| --- | --- | --- | --- | --- |
| 根目录门禁、codegen、scripts、cmd、internal | [tooling.md](tooling.md) | 1（孤儿 testdata） | 0 | 2 |
| gameplay、robot、service | [gameplay.md](gameplay.md) | 2 | 0（2 条登记需加强） | 2 |
| framework（不含 saga、dataengine） | [framework.md](framework.md) | 2 | 2（变异证明先红后绿） | 4 |
| framework/saga、framework/dataengine、infra、wiring | [core.md](core.md) | 5（3 个无引用辅助、2 条重复用例） | 0（1 条登记需加强） | 6 |

规则：只删确定的项，每项写明覆盖它的保留用例或承诺已不存在的依据，改动包的覆盖率前后对比；涉及 Nest / DataEngine / Sync 契约、文档点名的守卫、只靠测试存活的生产代码，一律列为待维护者决定，不在本次删除。

合并后验证：`GOWORK=off go build ./... && go vet ./...`、根包门禁与全量 `go test ./...`（见提交说明）。待决定项的处理结论记在各区域记录末尾。

## 待决定项的处理结论（维护者 2026-10-29：“按推荐”）

| 项 | 来源 | 结论 |
| --- | --- | --- |
| `jetstream-probe.sh` 的 Saga 背景分支静默空跑 | tooling、core | 已删；独立探针与闭环三档并入唯一路径，每段须在日志看到 `--- PASS` 才算跑过，否则退出 1；`ROOST_JS_PROBE_BACKGROUND=saga` 被拒并提示改用 `saga-js-scan.sh`；KNOWN-LIMITS 对应条改历史口径（需共享环境，未实跑，只做 `bash -n`） |
| awaitflow 守卫没有自动入口 | tooling | 新增 `codegen/scripts/awaitflow-runtime.sh`（重新生成 → `diff -r` 无漂移 → `-race` + glsvet），CI generated-code job 无条件调用，根包 `TestEveryCodegenRuntimeGuardRunsInSomeWorkflow` 覆盖；漂移检查经两次变异确认变红 |
| 只靠测试存活的 Redis 生产代码 | core | 已删 `IScript`、`IDistLock` / `DistLockFactory` / `AutoExtendLock` / `ErrLockNotHeld`、`EvalDurable` / `EvalBatchDurable`（WAITAOF）及只测它们的用例；`EvalReplicated`（L2 墓碑在用）与 `IVersionedLock` 保留；impl/03、PACKAGES、driver README、CHANGELOG 同步 |
| 现行文档点名第 4 批已删的 `wiring/saga` 真实用例 | core | impl/03 改指 `TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint`（投影重放）与 `TestRealJetStreamParticipantKill9`（saga 端到端恰好一次）；SLG / MMORPG 接入文档改指生成工程 `TestGeneratedSagaNativeStepsAndEffectConsumer` 与框架层真实用例 |
| `TestModSelectsDataEngineCommitterWithoutLegacyWALRuntime` 是子集 | core | 已删，`wiring/nest` 覆盖率 78.3% → 78.3% |
| remoteentity 两条停机用例重复 | framework | “超时的 Stop 已退订”并进 `TestRemoteAssemblyStopContract`（变异：Stop 超时不退订 → 合并后变红），删 `TestRemoteAssemblyStopWaitsForAnInFlightReplicaHandler` |
| nest create-in-handler 三对用例 | framework | **保留**：codegen 侧没有经真实生成 Lifecycle 在 handler 内新建的用例，`TestLifecycleCreateInHandler*` 是该承诺唯一守卫；手写 `generatedShapeLifecycle` 与模板有轻微漂移（不在该路径上），暂无同步守卫 |
| app 停机预算的 Mod 清单过时 | framework | 同步到 codegen 现值（game 服务 25 个 Mod、117s），用例按 Mod 实得窗口规划；同步有效性经阈值变异确认 |
| 三条断言过弱的用例 | gameplay、core | `TestDeclaredMemoryEffectsWriteTheirOwnSlot` 读回槽位值、`TestReleaseExpiredReservationIsNoOp` 断言记录不删不改、`TestRPCBudgetAssemblyClosesCompletedPool` 断言回调池停止；每条都有“旧用例绿、新用例红”的变异证据 |
| 零碎残留 | tooling、core、framework | 生产注释里已删驱逐机制的例子换掉；SYNC-OPTIMIZATION 两处历史基准标历史；integration README 示例名改成真实用例；demo 模板注释改指 `transition.go`；11 个文件的过期 Skip 提示路径改为 `wiring/scripts/integration/` |
| 吞吐用例名字带 Saga | core | `TestRealSagaReceiptTransactionThroughput` → `TestRealReceiptTransactionThroughputOnOneDocument`（另一条名字本就不带 Saga，未改；需真实 Mongo，未实跑） |
| heartbeat D20 原型、JetStream 诊断探针、审查轮次门变量名 | gameplay、core | 保留 / 不动（现行报告与脚本仍在用） |
| 压测参与方是否换生成工程 | 第 6 批 | 维持框架层正式路径 |
