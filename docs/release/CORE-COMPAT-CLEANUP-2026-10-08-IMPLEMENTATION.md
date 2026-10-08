# 三大模块旧兼容清理实现（未发版）

基线 `eedc0a41`，分支 `codex/core-compat-cleanup`。[说明 CC1～CC5](CORE-COMPAT-CLEANUP-2026-10-08-NOTES.md)。

| 编号 | 实现 | 契约与回归 |
| --- | --- | --- |
| CC1 | nest/nest.go、dispatcher.go、stats.go；kit/nest、kit/statslog；codegen/internal/nest/gen.go | 快慢边界、同 ID 调度、延迟容量不变；kit TestModProvidesInstanceClientAndHealth / statslog TestFormatNestStatsUsesReadableQueueNames；既有 Nest 1-worker 与续行回归 |
| CC2 | entity/subject_sync.go，旧调用转 PrepareViews | TestFullDirtyReusesSnapshotAndEmptyViewsStillCommit；TestSubjectSyncSnapshotDoesNotAdvanceVersion；on_change/periodic 与 CommitLSN 原回归保持 |
| CC3 | codegen/internal/entity/parse.go、gen.go | TestRetiredPackerMarkerIsRefused 明确拒绝旧标记；当前 subjectPacker 正式生成与编译继续通过 |
| CC4 | dataengine/engine/projector.go、projector_replay.go、projection_plan.go、mongo_projection.go | TestMultiBatchCapabilityAndSpecialBoundaries 改为批量/非批量两种现行 Store；checkpoint 丢失/失败前缀/取消/held 测试继续保留；Mongo 多 DAO 原子与 marker 重放沿用现有实现 |
| CC5 | entity/remote_protocol.go、remoteentity/batch.go、wrapper.go、codegen/internal/entity/gen.go | HasRemoteCommitLocked 成为必需方法；TestRemoteWriteBatchUsesOnlyTransactionLocalChanges（覆盖 clean Sync/有持久变更与 dirty Sync/无持久变更两个方向）；delete/reject/unknown/outbox 原回归继续验收 |

测试替身显式实现新接口，没有把旧 fallback 搬到生产 helper。生成模板和仓内调用方同步改用新 API；仅修改性能入口的编译接线，没有运行性能负载。目录未拆分。

## 证据

原始日志保留在主检出 `artifacts/core-compat-20261008/`。macOS Go 1.27.0，GOMAXPROCS=2、-p 1；功能测试顺序执行，不与索引刷新并跑。首轮受影响 18 包测试通过（target.log），此后追加生成标记清理，最终验证结果在收尾追加。本轮是已授权去兼容重构，未把旧兼容行为当作历史 bug 编造红证据。

CBM 基线 generation `2026-10-08T06:26:11Z`；默认 Verify，图查 PrepareTick、syncPackerFactoryExpr、NewEngine、planProjectionSegments、hasEntityDirty 的相关链路并读当前源。覆盖核对 119 个改动/证据 Go/模板路径，109 个 metadata_match 且无记录缺口，10 个 testdata/性能脚本被规则排除，已读全部修改源码补证；nest/dataengine/sync scopes 无记录缺口。图谱信号不是全仓穷尽证明。索引使用 main 基线，工作树修改以源 diff 和实际编译为准。

Linux 实机、真实 Mongo/NATS/Redis 故障矩阵和长压测不在本轮已验收范围。未自动删除数据、未发布。

## 最终验证

全仓 `GOMAXPROCS=2 GOWORK=off go test -p 1 ./...` 131 个有测试包通过（all.log），加强 Remote 双向变更判断后最终全仓再次通过（all-final.log）。`go build -p 1 ./...` / `go vet -p 1 ./...` 通过。相关七包 `go test -p 1 -race ./nest ./entity ./sync/entitysync ./dataengine/engine ./remoteentity ./kit/nest ./kit/statslog` 全通过：3.384s / 3.157s / 1.604s / 6.383s / 30.035s / 1.788s / 1.635s（race.log）。没有失败样本被删掉，也没有更改门禁。

生产 Go（含生成器与性能入口，不含测试）57 行增加、235 行删除，净减 178 行；这是代码量，不是性能收益。

独立 game-demo 正式生成 458 文件，build/vet/test 全通过，19 个有测试包（generated-*.log）。DataEngine/Remote 两套资源夹具生成与编译通过（dataengine-compile.log / remoteflow-compile.log，-run '^$'），没有运行资源行为。生成 Sync 实体实际构造/捕获通过（syncruntime-test.log，0.597s）。Linux/arm64 全仓交叉构建通过（linux-build.log），不是 Linux 实机运行。最后 `go generate ./...` 与暂存源码完全一致、无新增文件（generate-final.log / generate-diff.log），根包 `go test -p 1 -count=1 .` 通过 7.192s（root-final.log）。

### 生成消费复跑入口

所有命令使用当前 Go 工具链与 `GOWORK=off GOMAXPROCS=2`，在临时目录运行；模块通过 `go mod edit -replace github.com/tjbdwanghaibo/roost-core=<当前检出绝对路径>` 指向待验源码。

1. 在 core 根运行 `go run ./codegen/cmd/roost project new compatverify -module example.com/compatverify -out <临时目录>/game -template game-demo`；进入 game 设置 replace，再依次 `go build -p 1 ./...`、`go vet -p 1 ./...`、`go test -p 1 ./...`。
2. 分别复制 `codegen/internal/entity/testdata/dataengine` / `remoteflow` 顶层 Go 和 def Go 到独立临时模块（模块名分别 persistflow/remoteflow）。设置 Go 1.27.0、core require v0.0.0 和本地 replace；在 core 根运行 `go run ./codegen/cmd/dao -def <目录>/def -out <目录> -pkg <模块名> -force`，再 `go run ./codegen/cmd/entity -dir <目录> -force`；在临时模块 `go test -p 1 -mod=mod -run '^$' ./...`。这里只编译，不计资源行为验收。
3. 复制 `codegen/internal/entity/testdata/syncruntime` Go 到临时模块 entitysyncruntime，以相同方式设置 require/replace；执行正式 entity 生成器，然后 `go test -p 1 -mod=mod -tags entitysyncruntime ./...`，实际构造实体并用 PrepareViews 捕获生成 packer 的内容。
