# 测试有效性审计 2026-10-29：framework/saga、framework/dataengine、infra、wiring

范围：`framework/saga`、`framework/dataengine`（含 `engine`）、`infra/`（全部子包）、`wiring/`（全部子包），共 263 个测试文件、约 7.5 万行。
基线：main `4e2ff44c`（saga JetStream 切换第 1～6 批与第 7 批清理已落地）。分支 `audit/tests-core`。

方法（每条结论都来自下面某一种检查，不靠读名字猜）：

- 无引用的测试辅助：`gopls check`（v0.23，`unusedfunc`）分别在缺省与 `GOFLAGS=-tags=integration` 下扫全部范围；另用 AST 脚本列出测试文件里的顶层类型 / 变量 / 函数，在本包全部源文件里按词计数，只出现一次的即无引用。
- 断言为空：AST 脚本列出“没有任何 `t.Fatal*/Error*/Fail*/Run`，或只有 `if err != nil { t.Fatal }`”的用例，逐条读过（多数是“返回 nil 即承诺”的停机 / 幂等用例，属正常）。
- 重复：同包用例按 6-gram 记号做 Jaccard / 包含度比对，取 > 0.6 / > 0.85 的对逐条读，重复的再做变异核对。
- 永不运行：全部 `t.Skip` 的准入条件、全部 `os.Getenv` 变量与构建标签，逐个在 `scripts/`、`wiring/scripts/`、`codegen/ci`、`.github`、`docs/` 里找设置方；子进程角色用例逐个核对父用例仍在。
- 被删功能：在测试里搜 fence / lease / authority / storeservice / AOF / DurableScript / `saga.redis` 等；测试里出现的配置键逐个核对声明仍在；脚本与现行文档里点名的测试名逐个核对仍存在。
- 第 4～6 批的迁移表（设计稿 §13.3～§13.5）已做过的不重做，只找漏项。

## 已删除

提交 `58ab7909`。每条都给出覆盖同一承诺的保留用例，或承诺为什么不再存在。

| 删除项 | 原因 | 覆盖 / 依据 | `go test -cover` 前 → 后 |
| --- | --- | --- | --- |
| `framework/saga/assembly_test.go` 的 `recordingJetStream`（类型、`newRecordingJetStream`、`EnsureStream` / `Publish` / `Subscribe` / `subjects` / `deliver`） | 无引用的测试辅助（gopls `unusedfunc`；缺省与 integration 标签下都没有调用方）。第 4 批迁移后启动中继与步骤消费者的单元改用 `step_consumer_test.go` 自己的替身 | 不承载断言；同文件里仍被 `step_consumer_test.go` 使用的 `recordedSubscription` 保留 | saga 65.9% → 65.9% |
| `framework/saga/wire_validation_test.go` 的 `expectErr` | 无调用方 | 不承载断言 | 同上 |
| `framework/dataengine/engine/entity_unload_test.go` 的 `unloadResyncEntity.setLevel` | 无调用方：fence 用例在第 5 批改为运维卸载后，`TestEntityUnloadResyncsSubscribersFromMongo` 改由 Mongo 侧“另一写者”改 level，不再改内存 | 不承载断言；`mu` / `level` 字段仍被 packer 与 DAO 使用 | dataengine/engine 80.3% → 80.3% |
| `framework/saga/engine_test.go` 的 `TestStartSagaIsIdempotentByBusinessKey` | 重复：同一 `simEngine` 夹具，同一请求再启动返回同一 ID | `start_contract_test.go` 的 `TestStartIdentityCompatibilityAndForeignIntents/canonical`：等价但不同写法的请求（业务键带空格、空 Data、不同 Now、换时区且亚毫秒的截止）仍返回同一 ID，并断言 saga 只有一条、List 保留原摘要——更强。另有同文件 `TestStartSagaIdentityUsesStorageSafeDeadlinePrecision`（完全相同的重投幂等） | saga 65.9% → 65.9% |
| `framework/saga/engine_test.go` 的 `TestStartSagaRejectsBusinessKeyWithDifferentIntent` | 重复：同一夹具，同一业务键不同 Data → `ErrIdentityConflict` | `TestStartIdentityCompatibilityAndForeignIntents/data`：同一场景，另断言没有多建 saga；`/deadline`、`/version` 覆盖另两种不同意图 | 同上 |

变异核对（两条重复用例，修改 `framework/saga/partition.go` 启动判定里 `existing.Record.StartDigest == record.StartDigest`，跑被删用例与保留用例，之后还原）：

- 改成恒真（漏判身份冲突）：`TestStartSagaRejectsBusinessKeyWithDifferentIntent` 红；保留用例 `/data`、`/deadline`、`/version` 红。
- 改成恒假（合法重复被当冲突）：`TestStartSagaIsIdempotentByBusinessKey` 红；保留用例 `/canonical` 红。

覆盖率：两个包前后的覆盖档（`-coverprofile`，set 模式）逐行一致，不只是百分比相同。只删了缺省构建的单元用例与辅助，integration 标签下的覆盖不受影响。

## 待维护者决定

| 对象 | 问题 | 建议 |
| --- | --- | --- |
| `scripts/perf/jetstream-probe.sh` 第 46～47 行 | 第三段用 `-test.run '^TestRealSagaPerformance$'` 跑 `framework/saga`，该用例第 4 批已删。测试二进制对不匹配的 `-test.run` 打印 “no tests to run” 并以 0 退出，所以这一段**静默空跑**：`saga-mixed.json` 不会生成，随后的 `probe mixed` 实际是在没有 saga 背景负载时测的，脚本不报错、结果却按“混跑”命名。另外 KNOWN-LIMITS 第 398 行（Saga Mongo 步骤中间档位）写“`TestRealSagaPerformance` 限制单次 ≤10 分钟”，该条描述的是已删除的旧协调器与旧压测夹具 | A（推荐）：删掉脚本的 saga 混跑段与 `probe mixed`，只保留 JetStream / Mongo 两个探针对照；KNOWN-LIMITS 那一条改为历史口径或随旧协调器一起关闭。B：改成在背景里跑 `TestRealSagaJSScanLevel` 的一个档位（要补 `ROOST_SAGA_SCAN_*` 变量与私有 NATS / Mongo，等于重写脚本）。本次只改测试，不改脚本与 KNOWN-LIMITS |
| `TestRealJetStreamProbe`、`TestRealMongoCommitProbe`（`framework/saga/jetstream_performance_integration_test.go`） | 2026-10-23 JetStream 隔离诊断（`docs/performance/benchmarks/jetstream-isolation-20261023/`）的探针，只被 `scripts/perf/jetstream-{probe,resources,waits}.sh` 调用；这三个脚本要求共享隔离环境 `ROOST_DATAENGINE_IT_*` 并持 `remote-acceptance.lock`。探针本身不依赖已删的 saga 代码，仍可运行 | 保留（脚本仍在用）。若维护者认为 JetStream 隔离诊断已经结束，可与三个脚本一起退役；否则维持现状 |
| 现行文档点名已删除的测试 | `docs/framework/impl/03-dataengine.md:25` 的 `TestRealStepsRestartReplaysUnconfirmedProjectionsExactlyOnce`；`docs/framework/SLG-INTEGRATION.md:385` 的 `TestRealStepsRunNativeAndMongoStepsThroughTheMods`、`TestRealStepsRunNativeStepsUnderTheRealNestScheduler`；`docs/framework/MMORPG-INTEGRATION.md:294` 的 `TestRealStepsRunNativeStepsUnderTheRealNestScheduler`。三条都在第 4 批随 `wiring/saga/steps_real_integration_test.go` 删除，设计稿 §13.3 已写去向（生成工程 `TestGeneratedSagaNativeStepsAndEffectConsumer` + `TestRealJetStreamParticipantKill9`），现行文档没跟上 | 改文档指向现在的用例（不在本次测试审计范围，未改）；impl/03 那一句说的是“saga 级重启后未确认投影恰好重放一次”，现在最接近的是 `TestRealJetStreamParticipantKill9`（参与方 kill -9，含投影）与 `TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint`，请维护者确认对应关系后再改 |
| `wiring/nest/nest_mod_test.go` 的 `TestModSelectsDataEngineCommitterWithoutLegacyWALRuntime` | 与同文件 `TestModProvidesInstanceClientAndHealth` 同一夹具（`persistence.engine=dataengine`、`dataEngineNestProvider{noOpCommitter}`、Init + Provide），只多断言 `Engine() != nil`；后者在同样装配后还 Start 并读 `Engine().Stats()`，Engine 为 nil 时必然失败。名字里的 “legacy WAL runtime” 在 `wiring/nest` 里已无对应代码 | 建议删除（重复）。属于 Nest 装配，按规则不自行删除 |
| 只靠测试活着的生产代码（本次不删） | ① `infra/storage/redis/script.go` 的 `IScript`：全仓没有实现、没有使用者，也没有测试。② `infra/storage/redis/client.go` 的 `DurableEvaler` 与 `driver.Client.EvalDurable` / `EvalBatchDurable`（脚本 + WAITAOF）：Redis 权威删除后没有生产调用方（PROPOSAL-2026-10-26 当时就记“没有生产调用方”），只被 `driver/client_test.go` 的 `TestEvalDurableRejectsClusterTopologyBeforeIO`、`TestEvalDurableRejectsMalformedWAITAOFReply`、`TestRedisIntegerParsesWAITAOFReplyValues`、`TestRedisIntegerRejectsInvalidWAITAOFReplyValue` 与 `no_replay_test.go` 的 `TestDurableBatchIsResentWhenNothingExecuted` 使用；impl/03 点名了其中几条。③ `driver.DistLockFactory` / `AutoExtendLock`（`fredis.IDistLock`）：`redis.lock` 能力已不发布（`wiring/mods/name.go`），仓内没有使用者，只被 `lock_test.go`、`lock_integration_test.go`、`lock_toxic_integration_test.go`、`close_test.go` 使用；impl/03 列为现行 API | ① 建议删（纯死代码）。② 若不打算再用 WAITAOF，连同上述测试一起删，并改 impl/03 与 driver README；若作为库能力保留，维持现状。③ 是否作为对业务开放的库 API 保留由维护者定；保留则测试有意义，不保留则连同 4 个测试文件里的锁用例一起删 |
| `wiring/dataengine/real_integration_test.go` 的 `TestRealSagaReceiptTransactionThroughput`、`TestRealMongoMixedRatioWALReplayAckThroughput` | 吞吐形状的真实 Mongo 用例，没有脚本或性能文档引用，只打印速率；但断言仍有效（版本链、事务 / 回执集合条数、重放 / ack 结果）。前者名字里的 “Saga” 指旧 Mongo 步骤回执，现在只是“带回执的 DataEngine 事务” | 保留（DataEngine 契约，断言有效）。可考虑改名去掉 Saga 字样；不改不影响有效性 |

## 需加强

| 用例 | 问题 | 建议 |
| --- | --- | --- |
| `infra/network/nats/driver/rpc_contract_test.go` 的 `TestRPCBudgetAssemblyClosesCompletedPool` | 名字说“关闭已完成的池”，断言只有 `Close` 返回 nil（隐含：不挂住）。没检查 RPC 池确实停了（例如之后的调用被拒、worker 退出） | 补一条“Close 之后 RPC 客户端已停止”的断言。不是一行能改完并给出红绿的修复，本次未改 |

## 检查过、保留的可疑项

- `framework/saga` 的 `BenchmarkMongoStepInboxHandle`、`BenchmarkRealMongoStepInboxHandle`：脚本与文档未引用，但设计稿 §11.1 写明“benchmark 保留形状”，且只在 `-bench` 下运行；保留。
- `framework/saga` 的 `BenchmarkRealMongoStepLatencyBreakdown` / `Throughput` / `CommitWriteConcern`：`docs/maintenance/EXTERNAL-VERIFICATION.md` 引用；保留。
- `TestRealSagaJSScanLevel`：`scripts/perf/saga-js-scan.sh` 逐档调用；保留。
- 三个子进程角色（`TestRealJetStreamCoordinatorChild`、`TestRealJetStreamColdCopyCoordinatorChild`、`TestRealJetStreamParticipantChild`）与 `wiring/dataengine` 的 `TestDataEngineModWALDirectoryIsPerSid` 自调用：父用例都在同文件，准入变量由父用例设置；保留。
- `ROOST_SAGA_JS_*`（PREFIX / STREAM / PARTITIONS / GROUPS / DB / ACCOUNTS / WAL）在脚本与文档里都找不到设置方：它们是父用例传给子进程的参数，不是外部准入；正常。
- 其余全部准入变量（`ROOST_DATAENGINE_IT*`、`ROOST_NATS_CLUSTER_*`、`ROOST_REVIEW_CLUSTER`、`ROOST_REVIEW_REDIS`、`ROOST_REDIS_TEST_ADDR`、`REDIS_ADDR`、`ROOST_IT_TOXIPROXY`、`ROOST_MIRROR_LOCAL_REDIS_CLUSTER`、`ROOST_DATAENGINE_IT_BACKLOG[_MULTI]`、`ROOST_PROJECTOR_PERF*`、`ROOST_PHYSICS_*`、`ROOST_SAGA_MUTATE`、`ROOST_SAGA_JS_RESTART_DELAY`、`ROOST_JS_PROBE_*`、`ROOST_MONGO_PROBE_OUTPUT`）都有脚本、CI 或文档设置；没有永不运行的用例。
- 构建标签：`physx`（`scripts/physics/verify-service.sh`、PHYSICS-ROOM-SERVICE.md 设置）、`integration`（多个入口）、`!unix` 的 `infra/base/lock/cputime_windows_test.go`（只是给 unix 专用基准的跳过桩，平台范围只保 macOS / Linux，留着无害）、`!race` 的 `infra/network/httpserver/response_buffering_test.go`；都有效。
- `framework/saga/testdata/transition_golden.json`：被 `TestTransitionRulesMatchTheRecordedRedisCoordinator` 读取、impl/06 引用；全范围没有其他 testdata 目录；无孤儿金样。
- 测试里出现的 114 个 `saga.` / `dataengine.` / `nats.` / `redis.` / `mongo.` 等配置键都能在声明或文档里找到（`saga.steps.<名>.<步>.*` 是按步骤展开的键）；没有对已删配置键的断言。
- 搜 fence / lease / authority / storeservice / AOF / DurableScript 在范围内测试里的出现：剩下的都是现行概念（Nest `CommitFence`、Remote `LockFence`、outbox 的 `lease_until`、`fail-stop` 意义的 fence），不是已删机制。
- `wiring/sidmerge/mongo_real_integration_test.go` 设 `saga.database`：该键仍声明（Mongo 步骤收件箱与冷副本所在库）；正常。
- `wiring/scripts/integration/redis-cluster-suites.sh` 与 `dataengine-env.sh` 跑的包清单没有已删包。
- `infra/observe/log/log_test.go` 的 `TestCallerAttrsIncludeFileLineAndFunction` 与 `TestELogCallerAttrsIncludeUserFunction`（相似度 0.87）：分别走 slog 与 elog 包装，不重复。
- `wiring/mongo/stop_test.go` 的 `TestMongoModStopConvergesWhenTheClientIsAlreadyDisconnected` 与 `TestMongoModStopTwiceReturnsNil`：前者是客户端先被关，后者是 Mod 先停；不重复。
- `wiring/nats` 的 `TestNatsModStopAfterConnectionDrainBudgetConverges`（替身）与 `TestRealNatsModStopAfterConnectionDrainBudgetConverges`（真实服务端）：同一承诺、不同强度的夹具，后者只在真实依赖入口跑；都保留。
- `wiring/dataengine/toxic_integration_test.go` 三条 `TestToxicNATS*`（相似度 0.8）：延迟、连接重置、半开 ack 丢失三种故障，不重复。
- `framework/dataengine/engine` 的 `projector_parallel_test.go`、`projector_test.go` 内相似度较高的几对（同实体 / 多实体 / effect 记录的 WAL 顺序；projection-only 存储的逐条与失败前缀）：场景不同，且属 DataEngine 契约，不动。
- `infra/base/security/ratelimit_test.go:15`、`infra/base/misc/misc_test.go:9` 被 gopls 报“两侧表达式相同”：前者限流器有状态（两次调用结果不同），后者是纯函数确定性检查且同用例还有分布断言；都不是空断言。
- AST 扫描列出的 40 余条“只有错误分支断言”的用例（如 `TestOutboxConcurrentStartClose`、`TestServiceWatcherCloseIsNotReportedAsFailure`、`TestSingletonStoreCloseIsIdempotent`、`TestModIgnoresTheActivitySettingsThatMovedOut`）：承诺本身就是“返回 nil / 不挂住 / 不报错”，断言有效。
- 若干 Skip 提示里的入口路径过时（`framework/dataengine/engine/projector_replay_integration_test.go:56` 与 `wiring/mongo/close_contract_real_test.go:28` 写 `kit/scripts/integration/dataengine-env.sh`，`infra/storage/mongo/driver/real_mongo_promises_test.go:24` 写 `scripts/integration/dataengine-env.sh`，现在是 `wiring/scripts/integration/dataengine-env.sh`）；`wiring/scripts/integration/README.md:89` 的示例 `-run 'TestToxicNATSPartition'` 没有对应用例。只影响提示文字，不影响用例是否运行；未改。
- 范围内此前已有多轮“按主题合并、删除重复”（`infra/storage` `411fbf1f`、`infra/observe` `b6b08438`、`infra/network/*` `52d1c7e5` 等、`wiring/*` `fc8756f3` 等），这次没有发现新的重复，未再动。
