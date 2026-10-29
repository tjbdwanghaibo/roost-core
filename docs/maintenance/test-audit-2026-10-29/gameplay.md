# 测试审计 2026-10-29：gameplay / robot / service

范围：`gameplay/`（127 个测试文件）、`robot/`（12 个）、`service/`（61 个），约 1600 个 Test / Benchmark / Fuzz / Example。
基线 main `4e2ff44c`，分支 `audit/tests-gameplay`。codebase-memory-mcp 本轮连不上，结论都来自当前源码（`rg`）、AST 扫描和变异核对。

## 检查方法

| 类别 | 做法 | 结果 |
| --- | --- | --- |
| 永远不跑 | `t.Skip` / `os.Getenv` / build tag 逐条对 `scripts/`、`codegen/ci`、`.github`、`docs`、`wiring/scripts` | 每个门变量都有人设置（CI service-redis job、`scripts/physics/verify-service.sh`、`dataengine-env.sh`、`docs/performance` 脚本、`docs/skill`）；根包 `integration_coverage_test.go` 已经守着“真实依赖用例必须在某个执行器的包清单里”。没有找到永不运行的用例 |
| 删除功能的残留 | `rg` saga / lease fence / projection / jslog 等；`git log --diff-filter=D` 看本区近期删除 | 本区没有 saga Redis 权威、Store Service、lease fence 的残留 |
| 断言为空 | AST 扫描：没有 `t.Fatal*` / `t.Error*`、且不把 `t` 交给辅助函数的 Test；只在 `if err != nil` 里断言的 Test | 31 个“无直接断言”的都经辅助函数断言；22 个“只断言无错误”的逐个读过，见下文 |
| 重复 | 函数体规范化后取哈希（完全相同）；语句多重集合的 Jaccard / 包含度（近似重复） | 完全相同的只有各服务各自的 errcode 模板（测的是各自包，不算重复）；近似重复命中 1 对确实重复，见“已删除” |
| 孤儿 testdata / 辅助函数 | testdata 逐文件 `rg`；`gopls check -severity=hint`（unusedfunc，含 `-tags integration` 与 `-tags physx,integration`）；未导出声明按标识符计数 | 无孤儿；`gameplay/skill/testdata` 由 `acceptance_test.go` 整目录读取 |
| 原型基准 | 每个 `Benchmark*` 在 docs / scripts 里的引用 | 见“待维护者决定”第 1 条与“检查过、保留的可疑项” |

## 已删除

| 用例 | 原因 | 覆盖映射 | cover 前 / 后 |
| --- | --- | --- | --- |
| `service/mail/errcode_test.go` `TestNoTwoSentinelsShareACode` | **两半都不会单独变红。** 前半段在 `codeBySentinel`（`service/mail/types.go:143`，`map[int32]error`）的键上查重：键来自常量，重复的常量键在 map 字面量里编译不过。变异：把 `CodeBroadcastFull` 改成与 `CodeClaimHistoryFull` 相同的 590115，`go vet ./service/mail` 报 `duplicate key 590115 in map literal`，测试根本跑不起来。后半段检查每个码在号段内；保留的 `TestTheCodeSegmentIsContiguousFromItsFirstCode` 已经钉住“首码 590101 + 恰好 16 个 + 连续”，号段外的码必然同时让它变红。变异：`CodeBroadcastFull = 590200`，两个用例同时 FAIL（“outside mail's segment” / “the segment has a hole … found 590200”） | `service/mail/errcode_test.go` `TestTheCodeSegmentIsContiguousFromItsFirstCode`（注释补了一句：首码加条数同时保证码在号段内，重复码编译不过）。其他 8 个服务的 errcode 模板本来就只有“号段恰如分配”一个用例，与此一致 | service/mail 82.0% → 82.0%；coverprofile 覆盖块逐块比对：丢失 0、新增 0 |
| `gameplay/skill/spawn_stop_test.go` `TestStopSweepSkipsSpawnsDroppedAtTheStopPendingLimit` | **被同一场景的更强用例完全包含。** f28285ad（2026-10-07，待停止到上限不再删记录、改挪进“已放弃”分区）把这个旧用例的结尾断言改成 `SpawnAbandoned`，同时新增 `spawn_retention_test.go` 的 `TestStopSweepAbandonsAtTheStopPendingLimit`：夹具一字不差（同一 `asyncSkillJSON` + `longSummonWithCountingCancel`、`MaxStopPendingSpawns: 1`、同样的 Shutdown / RemoveProgram 两个子测试、同样的拒停步骤），旧用例的每条断言新用例都有，新用例另外核对 RetentionStats、`skill.spawn.abandoned.total`、ERROR 日志、state mutation 轨迹、不再打宿主、不再出现在 OwnedSpawns、不再钉住 cast。旧用例唯一多出的一条是“回收被拒后第二个衍生物是 stop_pending”：这一步由 `TestEveryStopEntryDefersARefusedStopTheSameWay` 的 `lifecycle entity vanished before handoff`（`reapUnhandedEntitySpawns` 入口）以同样断言覆盖（`runtime.spawns` 与 `StateSnapshot` 两处都要求 `stop_pending`）；而且不是 stop_pending 时新用例里“上限把它挪进已放弃”根本不会发生，同样变红。旧用例注释里还写着“上限把后一个删掉”，是改分区前的说法 | `gameplay/skill/spawn_retention_test.go` `TestStopSweepAbandonsAtTheStopPendingLimit`（`docs/framework/impl/08-skill.md:364` 列出的就是它）＋ `gameplay/skill/spawn_stop_test.go` `TestEveryStopEntryDefersARefusedStopTheSameWay` | gameplay/skill 84.4% → 84.4%；coverprofile 覆盖块逐块比对：丢失 0、新增 0 |

删除前 `rg` 两个用例名：除测试文件本身外没有任何文档、脚本、CI 引用。

## 待维护者决定

1. **`gameplay/heartbeat/internal/bench` 的原型结构与对照用例**（`heap.go` / `wheel.go` / `ring_ssr.go` / `ring_tier.go` / `tier.go` 与 `TestSchedulersMatchReferenceRule`、`TestSSRRingAndTierRingFireIdentically`、`TestPhaseDistribution`、`TestDeliveryRun` 的 x / y / z 模式、`BenchmarkSched`、`BenchmarkSSRMapHint`）。
   问题：D20 的结构与投递方式已经定案（正式环在 `gameplay/heartbeat`，投递缺省 batch），这些是“已完成原型”的对照；其中三个对照用例在每次 `go test ./...` 里都跑（整包约 4.6s）。
   现状：`docs/performance/HB-STRUCTURES-20261021.md`、`HB-DRIVEN-LOAD-20261021.md` 和 `docs/performance/benchmarks/hb-structures-20261021/scripts/run-sched.sh`、`nest-batch-heartbeat-20261021/scripts/*` 仍把它们写成复现命令，所以本轮按规则保留。
   建议：A（推荐）继续保留，直到这些报告不再作为现行复现入口。B 像 `scripts/perf/saga-jslog-proto` 那样删掉原型文件，报告里写“取回用 `git checkout <提交> -- gameplay/heartbeat/internal/bench`”。B 只删原型部分，保留同包里用正式 `heartbeat.Driver` 的 `TestBusyShortPoolSkipsBeatsAndResumes`、`TestDriverOpenLoop`、`BenchmarkDriverDispatch`（CHANGELOG 与 V1310-PRERELEASE 引用了它们），代价是要改报告里的命令。
2. **门变量名带审查轮次**：`ROOST_REVIEW3_BACKEND`、`ROOST_REVIEW4_BACKEND`、`ROOST_BUGFIX5_BACKEND`（account / activity / chat / mail / platform 的双后端夹具）。这些变量仍在 `.github/workflows/ci.yml` 的 service-redis job 里设置，也被“不许跳过”的 jq 检查认作 Redis 门变量，功能上有效。
   建议：A（推荐）不动。B 统一改成一个中性名字，例如 `ROOST_SERVICE_BACKEND=redis`，要同步 ci.yml、`integration_coverage_test.go` 的识别规则和 `docs/maintenance/EXTERNAL-VERIFICATION.md`，属于跨区改动。

## 需加强

1. `gameplay/skill/compile_contract_test.go` `TestDeclaredMemoryEffectsWriteTheirOwnSlot`：名字承诺“写自己的槽位”，实际只断言编译无 error、`Activate` 不报错，没有读回 `count` / `flag` 的值。它是 `TestCompileRejectsMemoryEffectsOnUndeclaredOrNonIntMemory` 的对照组，“能编译能施法”这一半有效，“写的是自己的槽位”没有断言。技能结束即 finish，memory 随施法消失，要读回需改夹具（施法里用 `if` 读 memory 产生可观察效果，或停在 wait 里读 cast 状态），不算小改，本轮没改。
2. `service/directory/directory_test.go` `TestReleaseExpiredReservationIsNoOp`：只断言 `Release` 返回 nil（“过期预留不冲突”）。名字里的“no-op”（不删、不改任何记录）没有断言。可补：Release 后用另一 owner `Reserve` 同 key 成功，并核对 store 里没有多出 / 少掉条目。优先级低。

## 检查过、保留的可疑项

- `gameplay/skill/compile_mutation_property_test.go` 的 `SKILL_MUTATION_FULL`：除测试文件注释外没有任何地方设置。它只是可选的加大档（子树移植），用例本身缺省照常跑，保留。
- `gameplay/skill/integration/sync-e2e` `TestProtocolSoak`（`ROOST_SYNC_SOAK=1`）：没有脚本调用，但 `docs/skill/visual-sync-production-guide.md` 与 `skill-implementation-guide.md` 写了运行方式，保留。
- 8 个服务 errcode 模板里 `twoDistinctSentinels` 的 `t.Skip("fewer than two coded sentinels")`：现在每个包都有至少 6 个码，这个分支不会触发；它是模板对将来新包的保护，保留。
- `gameplay/heartbeat/nest_test.go` `TestFiftyThousandEntitiesSmoke`：只在 `-short` 下跳过，常规运行都会跑，保留。
- `gameplay/movement/shared_test.go` 与 `shared_integration_test.go` 三对同名用例（`TestRemote…` / `TestRealRemote…`）：前者用进程内权威替身，后者用真实 Redis + Mongo + NATS（`ROOST_DATAENGINE_IT`），CHANGELOG 明确两层都要，保留。
- `gameplay/heartbeat` 的 `TestInstanceChecksDropStaleBeats`（假客户端单元）与 `TestBeatOfAnUnloadedInstanceDoesNotRunOnItsReplacement` / `TestBeatOfAnUnloadedInstanceIsDroppedWithoutColdLoad`（真实 Nest）：前两个是同一承诺的单元层与集成层，第三个是 batch 特有的交回路径，保留。
- `service/mail` `TestSettledClaimsAgeOutWithTheirEnvelope`（直接调用 `evictSettledClaims`，含“无窗口记录不按条数删”）与 `TestSettledClaimsAreDroppedOnceTheEnvelopeExpires`（经投递触发回收）：有一部分重叠，但层次不同，前者多出“无窗口”和“最旧但仍可领”两条，保留。
- `service/match` `TestCommitReplayInsideRetentionReturnsTheOriginalMatch` 与 `TestCommitReplayReturnsOriginalMatchForSameTicketSet`：后者另断言“票集不同不重放”，保留。
- `robot` `TestLockstepBotRequestsCatchupOncePerGap`（MaxBuffer 上限触发）与 `TestLockstepBotRequestsCatchupOnAnyUnhealedGap`（冗余覆盖不了的缺口）：触发路径不同，保留。
- `gameplay/skill` `TestRestoreRuntimeKeepsTheEventsEmittedAfterTheCheckpoint`（压缩事件的 Host、恢复成功）与 `TestRestoreRuntimePreservesPostCheckpointEvents`（Host 版本不符、恢复被拒）：场景不同，保留。
- `gameplay/skill` `TestRuntimeRestoresUnsortedLegacyCheckpointLists`：名字带 legacy，但承诺是“恢复时把无序列表规范化、再写出与有序版逐字节相同”，不依赖旧数据兼容，保留。
- 旧格式拒绝类用例（`service/mail` `TestCancelClaimLegacyWireIsRejected`、`service/rank` `TestARetiredCommaRequestLedgerIsRefused`、`service/chat` `TestChannelCodecRejectsOldUnidentifiedLedger`、`service/session` `TestRunCodecRequiresAdmissionFormat`、`service/global` `TestRouteCodecRequiresCompletionReceiptFormat`、`gameplay/skill/ability_test.go` 的 checkpoint 版本 9 拒绝）：与“未上线、改格式升版本号并拒绝旧版本”的规则一致，保留。
- `service/global/errcode_test.go` 的退役码空洞（570105–570109、570111–570124）：守的是“退役码不复用”，与 saga / lease fence 无关（那是已删的 global lease API），保留。
- `gameplay/skill/host_contract_test.go` `TestNoHostCapabilitySkipBranch`：源码级守卫，`docs/skill/skill-implementation-guide.md:192` 点名，保留。
- `gameplay/skill/motion_test.go` `TestMovingSpawnUsesTemplateLifetimeAndCompletesOnTerminalTick` 与 `TestTerminalCompletionEndSignalInvokesEndCallbackOnce`：语句包含度 0.94，但后者的宿主 `emitEndOnCompletion: true`，测的是宿主自己发 end 信号时 end 回调仍只跑一次，保留。
- `gameplay/skill/skillsync` `TestCoordinatorAckKeepsHistoryWhenOutboxDeleteFails` 与 `TestCoordinatorCloseKeepsHistoryWhenOutboxDeleteFails`：入口不同（Ack / Close），保留。
- `service/account/pending_contract_test.go` 的“名字在别处被预留 / 名字空闲”和“死计划释放失败 / 同名路径释放失败”两对：分支不同，保留。
- 未被文档引用的微基准（`BenchmarkTenThousandBots`、`BenchmarkActionRunnerTick` / `Lifecycle`、`BenchmarkStepSpawnMotion`、`BenchmarkStateMutationCommit1000Cooldowns` / `…TickAdvance`、`BenchmarkCombatRepeatedAttributeWrites`、`BenchmarkDecodeEffect`、`BenchmarkApplyStateMutation`、`BenchmarkActivateFinishingCast`、`BenchmarkOutboxPutSameStream`）：测的是正式代码不是原型，`go test` 不执行、只参与编译，保留。
- `robot/scenario/scenario_test.go:96` `pick(42) != pick(42)`：gopls 报“左右相同表达式”，但两边是两次独立调用，检查的是同种子的确定性，不是空断言，保留。
- 22 个“只在 err 分支里断言”的用例（各服务 `TestANilReporterChangesNothing`、`TestKCPDialerEndToEnd`、`TestRecordingAndReplayHostMatchTypedInteractionOrder`（`AssertComplete` 返回 error）等）：承诺本身就是“不报错 / 不 panic / 回放走完”，保留。上面“需加强”两条除外。

## 门禁（GOWORK=off，在本 worktree）

| 命令 | 退出码 |
| --- | --- |
| `go build ./...` | 0 |
| `go vet ./...` | 0 |
| `go vet -tags integration ./...` | 0 |
| `go test -race -count=1 ./gameplay/skill ./service/mail` | 0 |
| `go test -count=1 .`（根包） | 0 |
| `go test -count=1 -cover ./gameplay/... ./robot/... ./service/...`（删除前基线） | 0 |
