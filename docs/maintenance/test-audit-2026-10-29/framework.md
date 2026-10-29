# 测试审计 2026-10-29：framework（不含 saga、dataengine）

范围：`framework/` 下 app、cache、configdata（含 rules）、entity、hotcode（含 plugintest）、manager、nest（含 entitytimer）、
nestwal、remoteentity、statslog、sync（chunksync / entitysync / policy / frame / lockstep / nettransport / syncbus / driver /
mirror / syncstream），共约 1600 个 `Test*`。基线 main `4e2ff44c`，分支 `audit/tests-framework`。

## 怎么查的

| 类别 | 方法 | 结果 |
| --- | --- | --- |
| 永不运行 | `rg t.Skip` 全部条件；测试读的每个 `ROOST_*` 环境变量与 build tag 对照 `scripts/`、`.github`、`codegen/ci`、docs | 只有 `ROOST_REMOTE_FINALIZE_LOAD` 无人设置（已删，见下）；子进程入口（nestwal crash、outbox crash、process lock、RR45 assembly）的父用例都在；4 个 `Example` 都有 `// Output:`；hotcode plugintest 本机实际运行（未跳过） |
| 删掉的特性 | `rg` lease fence / FencedStep / ProjectFenced / historical / storeservice / Redis 权威 / completion effect / 原生步骤；对照 f9517915、c776dbe6 与近两周 `--diff-filter=D` | 测试里没有残留（nest 的 fence 是 `ErrNestFenced` / `CommitFence`，remoteentity 的 fence 是版本锁 fence，都是现行机制）；**生产注释**有两处残留，见“附带发现” |
| 断言缺失 | AST 扫描：从不调用 Fatal / Error / Fail / Run / Skip、也不把 `t` 交给别的函数的 `Test*` | 1 个：`TestSlowDispatchWatchReuseAfterTimerExpiry`，是 `-race` 下的所有权交接压力用例，保留 |
| 只测替身 | `go/packages` 类型信息：从 `Test*` 出发沿测试文件里的声明递归，看是否触到模块内任何非 `_test.go` 对象 | 1 个：`TestRemoteSnapshotCacheWritesStayInTheListedFunctions`，按 AST 读本包源码的结构守卫，保留 |
| 重复 | ①函数体格式化后完全相同；②逐个用例跑 `-coverprofile`，覆盖块集合完全相同的分组（23 个包、约 1500 个用例） | ①无；②45 组（其中 3 组是覆盖为 0 的守卫 / 外部依赖用例）。读过代码的 8 组见下文，均为同一路径上的不同承诺或部分重叠；`manager` 的一组经变异证明**不是**重复（见“需加强”）；其余按名字与断言目标判断为不同承诺，未逐行读 |
| 孤儿 testdata / helper | `go/packages`：测试文件里未被引用的包级非导出对象（默认与 `integration` tag 各一遍）；`testdata` 目录引用 | 无未用 helper；`lockstep/testdata/fuzz/FuzzDecodeSnapshotChunk` 有对应 fuzz 目标，`hotcode/plugintest/testdata/bundle` 被 `plugin_load_test.go` 构建 |
| 过时基准 / 探针 | 每个 `Benchmark*` 对照 `scripts/perf/*`、docs | 1 个重复基准已删；其余未被引用的都是现行代码路径的微基准，保留 |

## 已删除

| 用例 | 原因 | 覆盖映射 | cover 前 / 后 |
| --- | --- | --- | --- |
| `framework/remoteentity/finalizer_test.go` `TestRemoteAsyncFinalizerProjectionLoad`（连同只给它用的 `latencyCountingStorage`、`loadEnvInt`，共 175 行） | 类别 1 + 6：RR-20260926-38 的负载对照探针，默认 `t.Skip`，开关 `ROOST_REMOTE_FINALIZE_LOAD` 在 `scripts/`、`.github`、`codegen/ci`、docs 里都没有人设置；输出 `RR38LOAD …` 只打印吞吐，不断言任何承诺（只有写失败时 `t.Errorf`） | 无承诺需要映射。它量的“异步收尾交给投影器、不重复回源 / 发布”由同文件 `TestAsyncFinalizerLeavesProjectionToProjector`、`TestOutboxOwnsPublicationAfterIndeterminateProjection` 断言 | remoteentity 82.9% → 82.9%（默认运行本就跳过） |
| `framework/nestwal/wal_benchmark_test.go` `BenchmarkWALAppendV2AsyncParallel` | 类别 4 + 6：2026-08-31 “versioned data engine records” 引入时用来对照新旧记录格式；现在只有一种格式（旧格式拒绝表见 `TestRetiredFormatsLeaveWALUntouched`），它与 `BenchmarkWALAppendAsyncParallel` 只差 `Codec` 字符串（WAL 把它当不透明字符串写入）。没有脚本或文档引用 | 保留 `BenchmarkWALAppendAsyncParallel`（`docs/maintenance/EXTERNAL-VERIFICATION.md` 引用它） | 基准，不影响覆盖率；删后 `-bench WALAppendAsyncParallel -benchtime 100x` 冒烟通过 |

同步改了 `docs/framework/impl/05-remote-mirror.md` 里 `finalizer_test.go` 的“其余 4 项”为“其余 3 项”。

## 需加强（已修，先红后绿）

| 用例 | 问题 | 修法 | 红 / 绿 |
| --- | --- | --- | --- |
| `framework/manager/engine_lifecycle_test.go` `TestRepeatedStartDoesNotDuplicateManagers` | 注释写“显式拒绝或幂等都可以”，第二次 `Start` 的返回值只 `t.Logf`。`Engine.Start` 的契约（engine.go:126）是之后的调用返回 `ErrStartState`。逐用例覆盖率与 `TestConcurrentStartHasOneOwner` 完全相同，起初当重复看；变异“成功启动后放开启动权（`e.starting = false`）”只有本用例变红，所以它是这条承诺唯一的守卫，不能删 | 断言 `errors.Is(second, ErrStartState)`，`Stop` 的错误也检查；注释写明它与并发用例的区别 | 变异“已启动时第二次 `Start` 返回 nil（幂等）”：旧用例 `ok`，新用例 `engine_lifecycle_test.go:270: second Start after a successful start = <nil>, want ErrStartState` 变红；还原后绿。manager 覆盖率前后都在 95.1%～95.7% 之间抖动（并发时序分支），不是本改动造成 |
| `framework/sync/lockstep/submit_input_test.go` `TestSubmitInputPromiseReplayAfterCutIsIdempotent` | 重传被拒时 `return // rejecting a replay is acceptable`，跳过“返回当初折入的帧号”的检查；`SubmitInput` 的契约（sequencer.go 的已记身份分支）是应答原帧号、不报错 | 重传报错改为 `t.Fatalf` | 变异“已记身份的重传返回错误”：旧用例 `ok`，新用例 `submit_input_test.go:39: replay of an accepted input was rejected: MUTATION: replay rejected` 变红；还原后绿。注意包内另有 5 个用例也会被这个变异打红，所以承诺本身并非没人守，这里只是去掉本用例的空转出口 |

## 待维护者决定

| 用例 | 问题 | 建议 |
| --- | --- | --- |
| `framework/remoteentity/assembly_test.go` `TestRemoteAssemblyStopWaitsForAnInFlightReplicaHandler` 与同文件 `TestRemoteAssemblyStopContract` | 逐用例覆盖块完全相同；后者走共用的 `stopcontract.Check`（超时报 ctx 错误、重试、放行后 nil、再调 nil），前者在同一场景上多断言“超时的 Stop 已经退订”。remoteentity 属 Remote 持久链路，按保守处理 | 合并：把“超时后已退订”作为一条断言并进 `TestRemoteAssemblyStopContract` 的 Stop 钩子之后，然后删前者。不合并也无害 |
| `framework/sync/syncbus/mirror/stop_test.go` `TestReplicatorStopWithContextWaitsForAdmittedHandlers` 与 `TestReplicatorStopContract` | 同上的形状：前者另有“退订后迟到投递不进 Store”“停后重启”两条独有断言 | 保留两者（独有断言是真承诺）；如要收敛，把契约骨架部分从前者删掉、只留独有断言 |
| `framework/nest/create_in_handler_test.go` 的 `TestCreateInScope*` 与 `TestLifecycleCreateInHandler*` 三对（Revoked / WaitsForDurable / OutsideTransaction） | 每对覆盖块相同：差别只在实体是直接 `Create` 还是经测试里手写的 `generatedShapeLifecycle`（模拟生成代码）创建；直接版断言更多 | Nest 契约，不删。若生成的 Lifecycle 已在 codegen 生成工程里有同样的用例，可考虑删 `TestLifecycle*` 这一组；需要先核对 codegen 侧 |
| `framework/app/stop_budget_test.go` 的 `generatedGameServiceMods`（23 个 Mod、108s） | 注释自己写明 codegen 的 `shutdown_test.go` 已数出 24 个 Mod、114s，这里仍按旧清单规划。用例测的是预算算法，不是生成值，结论不受影响 | 保留；要消除歧义就把清单与总时长改成 codegen 现值（不是删除） |

## 附带发现（不是测试，未改）

- 生产注释仍提到已删的“DataEngine 驱逐被跳过的原生步骤”（f9517915 删除了这套机制）：
  `framework/nest/remote_dispatch.go:79`（`RunLocal` 的调用方举例）、`framework/entity/entity_kind.go:49`（`DestroyReasonMemoryUnload` 的来源举例）。建议删掉这一例，只留现行来源（Remote 事务被持久拒绝 / 运维卸载）。
- `docs/framework/SYNC-OPTIMIZATION.md:255` 的复跑命令引用 `BenchmarkRelationUpdates` / `BenchmarkRelationSetSize` / `BenchmarkRelationMemberChange`，它们随 d2c2cdce（D19 删除 `RelationSource`）删除；属于历史性能记录，按需加一句“已随 D19 删除”。

## 检查过、保留的可疑项

- `nest/dispatch_metrics_test.go` `TestSlowDispatchWatchReuseAfterTimerExpiry`：没有断言调用，是 `-race` / 双重归还 panic 下的所有权交接压力用例。
- `entity/remote_snapshot_write_guard_test.go` `TestRemoteSnapshotCacheWritesStayInTheListedFunctions`：不调用生产代码，按 AST 读源码的结构守卫，有效。
- `app/config_contract_test.go` 三个配置声明守卫（包覆盖为 0）：读源码的守卫，`TestConfigDeclarationGuardsCatchTheirDrift` 证明守卫能红。
- `remoteentity/snapshot_replica_test.go` `TestAChecksumIsStoredAsEightBytes` / `…NotStoredAsEightBytesIsRefused`、`policy/interest_test.go` `TestRejectEntitiesJudgesEntitiesWithoutSyncStateByID`：对本包覆盖为 0，测的是 entity 包的类型 / API，位置不理想但承诺有效。
- `remoteentity/assembly_test.go` `TestValidateRemoteManagedDaoScopes`、`TestAssembleRejectsRemoteManagedServerScopedDAO`：前者测 entity 的校验函数，后者走子进程（覆盖在子进程里），有效。
- `remoteentity/write_admission_test.go` `TestRemoteWriteBudgetConfigCompatibility`：名字像兼容层，实际钉的是“写额度不超过收尾容量”的现行规则（transaction_manager.go:98）。
- `remoteentity/*_integration_test.go` 全部 46 个 `TestReal*` / `TestMirrorLocal*` / 基准：都被 `scripts/test-remote-matrix.sh`、`scripts/mirror-local.sh`（`^TestMirrorLocal`、`^BenchmarkMirrorLocal`）或 impl/05、EXTERNAL-VERIFICATION 点名。
- `remoteentity/snapshot_l2_watermark_bench_integration_test.go` `BenchmarkRealB2*`：impl/05 列出该文件，保留。
- `sync/syncbus/driver/retirement_real_promises_test.go` `TestRealRR25*`：impl/04 点名。
- `cache` 的 7 个 Real Redis 用例：`ROOST_REDIS_TEST_ADDR` 由 `.github/workflows/ci.yml` 设置。
- `sync/lockstep/snapshot_test.go` `TestMatchLongRunPeriodicSnapshotRecoveryKeepsHistoryBounded`：只在 `-short` 下跳过，默认运行。
- 未被脚本 / 文档引用的微基准（`BenchmarkAtomicLocalStoreOverwrite`、`BenchmarkAdmissionColdProbe`、`BenchmarkClientRequestManagerAccess`、`BenchmarkHandlerCreateEntity`、`BenchmarkSlowDispatchWatch`、`BenchmarkTickCallbackSnapshot`、`BenchmarkRecordEncodingMatrix`、`BenchmarkPrepareRemoteWriteBatch`、`BenchmarkRemoteTracker*`、entitysync / frame / lockstep / syncstream 的若干个）：都量现行代码路径，不是原型探针；`BenchmarkRecordEncodingMatrix` 有 `docs/performance/benchmarks/nestwal-codec-20261015` 的样本。
- `nestwal/wal_benchmark_test.go` `BenchmarkWALAppendAsyncParallel` / `…StrictParallel` 用的 `Codec: "bson-full-v1"` 是旧标签（生产是 `bson-v2`），WAL 不解释它，不影响结果。
- 读过代码、判定为不同承诺的同覆盖组：`app` 的 `TestLoadConfigFillsDefaultsAndRefusesOutOfRangeValues` / `TestNegativeLogRotationIsRejected`；`manager` 的 Order 菱形 / 依赖列表顺序、Start 顺序 / Stop 幂等；`entity` 的 `TestEntityManager_AddGet` / `TestEntityManager_ConcurrentAccess`（后者是 `-race` 并发用例，不检查身份与未命中）；nest 的 `TestCreateInScopeRevokedWhenHandlerFails` / `TestLifecycleCreateInHandlerRevokedWhenHandlerFails`；remoteentity 与 mirror 的两组停机用例（见“待维护者决定”）。其余同覆盖组（`app` 四组、`entity` 两组、`hotcode` 一组、`manager` 一组、nest 八组、remoteentity 八组、sync 各包九组）按名字与断言目标判断为不同承诺，未逐行读，保留。
