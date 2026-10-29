# 测试审计 2026-10-29：工具与门禁区域

范围：根目录 16 个 `*_test.go`（文档 / CI / gofmt / 指标 / 依赖边界等门禁）、`codegen/`（`codegen/internal` 下的测试与 testdata / golden）、`scripts/`（含 `scripts/testdata`、`scripts/perf`）、`cmd/`、`internal/`、`client/`、`examples/`、`demo/`（`*_test.go.tmpl`）。基线为 main `4e2ff44c`（saga JetStream 切换第 1～6 批已合入）。

方法：

- 跳过：对上述 Go 包跑完整（非 `-short`）`go test -json`，统计 SKIP。只有 3 条跳过，都由环境变量控制，并且各有入口（见“检查过、保留的可疑项”）。根包 40 条测试全部 PASS，没有跳过。
- 入口：把 `scripts/`、`codegen/scripts`、`wiring/scripts`、`.github` 里 `-run` / `-test.run` 写到的每个测试名，和源码（含 `.tmpl`、`.go.txt`）里的定义对照。
- 孤儿：逐个检查 testdata 目录和 golden 文件，确认有用例或脚本引用；未使用的测试辅助函数用脚本按名字统计引用次数。
- 重复：把测试函数体去掉空白和注释后做哈希比对，找完全相同的用例。
- 已删除概念：在本区域搜索 Redis saga 权威、Store Service、`saga.redis.*`、lease fence、`saga-jslog-proto`、`cmd/physics-viewer` 等残留引用。

## 已删除

| 对象 | 原因 | 覆盖映射 | `go test -cover` 前 / 后 |
| --- | --- | --- | --- |
| `codegen/internal/entity/testdata/player_gen_wire.go`（孤儿 testdata，不是用例） | 这是 `testdata/player.go` 的生成物，但没有任何东西读取或编译它，具体有四点。(1) 所有用例（`parse_test.go`、`marker_test.go`、`category_contract_test.go`、`retirement_test.go`、`mirror_test.go`）都把生成结果写进 `t.TempDir()` 再比对。(2) `parsePackage` 用 `isGeneratedWireFile` 跳过 `*_gen_wire.go`（`codegen/internal/entity/parse.go`）。(3) `go build ./...` 不编译 testdata。(4) 没有脚本复制它（`rg "entity/testdata/player_gen_wire"` 无命中）。另外文件内容已过期：用当前生成器重新生成后，与提交的文件相比缺少 `TakeEntitySyncChanges` 方法 | 不承担任何承诺。生成器对 `player.go` 的输出由上面五个文件里的用例在临时目录检查 | `codegen/internal/entity`：83.7% → 83.7% |

提交：`2711c323`。

## 待维护者决定

| 对象 | 问题 | 建议 |
| --- | --- | --- |
| `codegen/internal/entity/testdata/awaitflow/await_test.go` 的 `TestGeneratedPlayerAwaitsAndResumesOnLongPool`、`TestGeneratedDispatchFillsTheBusinessTimeAtExecution`；`awaitflow/sender/player_nest_gen_test.go` 的 `TestPlayerSenderRefusesCallsWithoutAClient` | **没有任何自动入口**。testdata 目录不在 `go test ./...` 里；`codegen/scripts/*-runtime.sh`、`scripts/*.sh`、`.github` 都没有点名 awaitflow；根包 `TestEveryCodegenRuntimeGuardRunsInSomeWorkflow` 只检查 `*-runtime.sh`，管不到它。这两条用例是 [NEST-AWAIT](../../framework/NEST-AWAIT.md) 和 [impl/10-time](../../framework/impl/10-time.md) 点名的守卫（long 池 await 恢复、分发在执行时填业务时间），目前只有人照文档手动跑。本轮手动核对：生成物与当前生成器一致（重新生成后 diff 为空），`go test ./codegen/internal/entity/testdata/awaitflow/...` 通过 | **A（推荐）**：新增 `codegen/scripts/awaitflow-runtime.sh`（先重新生成，确认 diff 为空，再 `go test -race`），由 ci.yml 无条件调用；根包现有门禁会自动要求它被 workflow 调用。代价是一个脚本加一个 CI 步骤，时间在一分钟以内。**B**：直接在 ci.yml 的 codegen 步骤加一行 `go test ./codegen/internal/entity/testdata/awaitflow/...`。更省事，但不检查生成物是否过期，也没有门禁防止这一行以后被删 |
| `scripts/perf/jetstream-probe.sh` 缺省（`ROOST_JS_PROBE_BACKGROUND=saga`）分支 | 该分支用 `-test.run '^TestRealSagaPerformance$'` 跑 Saga 混合背景负载，但这条用例已随 `framework/saga/performance_real_integration_test.go` 在 `298e4396` 删除。现在 `-test.run` 匹配不到任何用例，测试二进制以 0 退出，`saga-mixed.log` 里只有 “no tests to run”，“mixed” 档实际是没有背景负载的单跑，脚本却正常结束。`mongo` 分支（`TestRealJetStreamProbe` / `TestRealMongoCommitProbe`）仍然有效。引用方是 `docs/performance/benchmarks/jetstream-isolation-20261023/README.md` 的复跑命令（历史样本） | **A（推荐）**：删掉 saga 分支，`mongo` 作为唯一背景；同时在样本 README 注明 saga 混合对照已随 Redis 协调器退役，正式 saga 压测入口是 `saga-js-scan.sh`。**B**：把背景负载换成 `TestRealSagaJSScanLevel`，这会改变对照口径，需要重新定义对照含义。两种都属于脚本行为变化，本轮没有改 |

## 需加强

无。本区域没有发现“会运行但不会失败”的用例。根包门禁大多做了双向校验，比如 `TestGeneratedProjectRealDependencyTestsHaveAnEntry`：表里的条目匹配不到用例时变红，用例找不到条目时也变红。`dao` 的 golden 用例还会检查多余的 golden 文件。

## 检查过、保留的可疑项

- 根包 `ci_service_redis_test.go`（`TestCIWorkflowRunsTheRedisSuitesAndRefusesTheirSkip` / `TestCIRedisJobRefusesKeysLeftBehind`）：保留。它守的是 ci.yml 的 `service-redis` job，这个 job 跑 `wiring/redis`（App 单实例锁）、`service/...`、`infra/storage/redis/...`、`versionstore`、`framework/cache` 的 Redis 用例，与已删除的 saga Redis 权威无关。job 设置的 5 个门变量（`ROOST_REDIS_TEST_ADDR`、`ROOST_REVIEW_REDIS`、`ROOST_REVIEW3_BACKEND`、`ROOST_REVIEW4_BACKEND`、`ROOST_BUGFIX5_BACKEND`）仍分别被 13 / 11 / 3 / 3 / 2 个测试文件读取。
- 根包 `integration_coverage_test.go`：保留。表格是双向校验，引用已删除的 slg-alliance、saga Redis 时会变红；当前全部 PASS。表里 `TestSLGAllianceRemoteDelivery` 说明文字中的 “lease 中途过期” 指 Remote lease，不是已删除的 DataEngine lease fence。
- 根包 `acceptance_lock_test.go`：保留。两半都还有检查对象：`wiring/dataengine/failover_integration_test.go`；`scripts/test-remote-generated.sh`、`test-remote-matrix.sh`、`perf/remote.sh`。
- 根包 `doc_links_gate_test.go` / `doc_commit_refs_gate_test.go` / `conflict_marker_gate_test.go` / `gofmt_gate_test.go` 的 `t.Skip`：只在没有 git、不在工作树或是浅克隆时跳过，本机与 release 工作流都会执行。保留。
- 根包 `genproto_pin_test.go`（只有一条空白 import，没有用例）：保留。它维持 go.mod 对拆分后 genproto 的需求，由 `TestModuleLoadsInsideAWorkspace` 守护。
- 根包 `examples_run_test.go`：`exampleRuns` 里六个示例都必须运行，没有跳过项。保留。
- `codegen/internal/roost`：完整运行时只有 3 条 SKIP。`TestGeneratedSagaNativeStepsAndEffectConsumer`、`TestGateGeneratedBusinessConsumer` 需要隔离环境，由 `scripts/test-demo-generated.sh` 选中，挂在 `dataengine-env.sh test` 下；`TestDoctorPlayerTCPPassesAfterAuthAndConfig` 需要联网，由 framework-compat.yml 的 `ROOST_NETWORK_TESTS=1` 步骤运行。其余 `t.Skip` 都是 `testing.Short()`、缺工具（docker / make / sh）或平台条件。保留。
- `codegen/internal/roost/add_saga_test.go` 与 `testdata/saga_native`：已经是 JetStream 协调器加实体内回执的写法，没有引用 Redis saga、Store Service 或 `saga.redis.*`。生成器模板与 demo 中也搜不到这些键。保留。
- `codegen/internal/entity/sync_contract_test.go` 的 `TestRetiredPackerMarkerIsRefused` / `TestPackerWithoutSyncIsRefused`：哈希查重报为疑似重复，读代码后确认是两个不同的承诺（退役写法 `syncPacker=` 的提示；`subjectPacker` 必须配 `sync=true`），两条都在 [impl/12-codegen](../../framework/impl/12-codegen.md) 点名。保留。
- `codegen/internal/{dao,protocol,servicerpc}/testdata/golden`：都由 golden 用例逐字节比对，`dao` 还会对多余的 golden 文件报错。保留。
- `codegen/internal/{attribute,cfggen,tablegen,dao}/testdata/runtime`、`entity/testdata/syncruntime`：由 `codegen/scripts/*-runtime.sh` 在 ci.yml 无条件运行。保留。
- `codegen/internal/entity/testdata/{dataengine,remote,remoteflow,syncmodes,nestgame}`：分别由 `test-dataengine-generated.sh`、`test-remote-*.sh`、`test-sync-modes-generated.sh`、`perf/nest-game.sh` 使用。保留。
- `scripts/testdata/*`（auto-chess、card-rpg、idle-sim、merge-casual、mmorpg、moba、slg、survival-extraction、tower-defense、heartbeat、level）：各自的 `scripts/test-*-generated.sh` 是文档写明的人工入口（`scripts/README.md`、`codegen/docs/CODEGEN_REFERENCE.zh-CN.md`）。需要真实依赖的用例由根包表格守住入口。没有引用已删除的 API（`activity.NewRedisStores`、`versionstore.NewRedisStore` 仍然存在）。保留。
- 所有脚本里 `-run` 写到的测试名都核对过，除上面的 `TestRealSagaPerformance` 外都存在。区域外顺带发现：`wiring/scripts/integration/README.md:89` 的 `-run 'TestToxicNATSPartition'` 没有对应用例（现有的是 `TestToxicNATS*` 其他名字），属于 wiring 区域，转交给负责该区域的审计。
- `scripts/perf/tailtrace/main_test.go`：tailtrace 是独立模块，根 `go test ./...` 不包含它，CI 也不跑；它是 `nest-game.sh` 长尾分析的解析器自测。本轮手动 `go test` 通过。保留，优先级低：将来可以考虑在 `nest-game.sh` 里先跑一次自测。
- `scripts/perf/test_sync_visibility.py`（Python unittest，6 条）：没有自动入口，手动 `python3 -m unittest` 通过。保留，优先级同上。
- `scripts/perf/fixtures/sync_business_test.go.tmpl`：`sync-business.sh` 的夹具，不经脚本直接运行时会跳过。保留。
- `internal/{configschema,hoptrace,operation,stopcontract}`、`cmd/glsvet`、`client/wire`：被测包仍有生产代码引用（`hoptrace` 被 DataEngine 投影器与 Mongo driver 使用）；`client/wire` 的 golden 读 `client/spec/packets.json`，与 C# 一侧共用。保留。
- `demo/internal/service/game/gift_saga_budget_test.go.tmpl:20`：注释写的是 “saga/engine.go 的 backoff”，退避函数现在在 `framework/saga/transition.go` 的 `backoff`。翻倍、封顶、在 [d/2, d] 内抖动这三点语义不变，用例的下界计算仍然成立，只是文件名过期。demo 是参考工程，本轮不改，建议下次改 demo 时顺手修正。
- `demo/**/*_test.go.tmpl`（49 个模板）：本轮用当前检出生成 game-demo（`--skip-deps`，replace 指回本仓），`go vet ./...` 与 `go test -count=1 ./...` 全部通过（26 个包 ok）。没有因为 saga 切换而失效的模板用例。保留。
- 未使用的测试辅助函数：本区域所有测试包（含 testdata 包）里，按名字统计没有只出现一次的非导出函数 / 变量 / 类型。
