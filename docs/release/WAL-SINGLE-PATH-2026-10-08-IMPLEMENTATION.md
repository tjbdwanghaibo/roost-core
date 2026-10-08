# WAL 单路径收敛实现（未发版）

基线 `6907150e`，工作分支 `codex/wal-simplify`。[说明 WS1～WS4](WAL-SINGLE-PATH-2026-10-08-NOTES.md)。

| 编号 | 实现入口 | 复核不变量 |
| --- | --- | --- |
| WS1 | docs/agent-skills/roost-coding/SKILL.md、roost-optimize/SKILL.md | 用户上线前边界，不能扩成自动删除数据授权；本地 Codex skill 已同步且 quick_validate 通过 |
| WS2 | `nestwal/codec.go:52` / `:159`、wal.go；`kit/dataengine/mod.go:175`；codegen/internal/roost/kitconfig_gen.go | 唯一 codec 7；非法/旧版本拒绝且不调用消费方、不推进 checkpoint；仍校验条目数、空记录和 durability |
| WS3 | `dataengine/mutation_types.go:77`、validate.go；`nest/msg.go:41`、`nest/rollback.go:313`、`nest/persist_change.go:283`；`dataengine/engine/entity_projection.go:23`、fenced_step.go | 入事务时校验并冻结；Remote 头使用 BaseVersion/NextVersion，删除显式 MutationDelete；不改 RemoteCommit 内部字段模型 |
| WS4 | engine.Projector/Assembly；`nest/nested_fence_outer_commit_nestwal_promises_test.go:115` | 原嵌套事务回归使用真实 WAL + 正式 Projector；保留先写成功后 AcceptMutation 失败时的 fence 验证 |

## 原 Committer 回归的承接

删除的是退役实现专属用例，正式行为按下表继续验收。没有在测试代码里复制旧 Committer。

| 旧验证目标 | 正式路径上的承接 |
| --- | --- |
| 锁释放前不能投影、Enqueue 持久完成后唤醒 | 新 TestProjectorSinglePathHoldsRetriesAndSerializesReplay（strict/pipelined）；既有 TestPipelinedCommitIsProjectedOnceDurableWithoutWaitingForIdlePoll |
| 准入拒绝释放 held | TestCommitAndEnqueueRejectAnOversizedRecordAlikeAndLeaveNoAdmission |
| 两个回放不能重复消费、失败后可恢复 | 新 single-path 用例；TestProjectorWaitsRespectDeadline（串行门）；前缀失败/ack 失败既有回归 |
| Shutdown/Flush 等待受 ctx 约束且保留 WAL | TestProjectorWaitsRespectDeadline、TestProjectorShutdownDeadlineBehindBackgroundProjection、TestProjectorCloseWaitsForExternalProjection |
| 自有 WAL 的关闭/重开 | TestAssemblyRetriedShutdownHandsOverWALWithoutCheckpointRegression |
| 发布失败 | TestProjectorAckNotBlockedByPublisherFailure 与 OutboxWorker 回归；正式设计是 Mongo outbox，与旧直接发布阻塞 ack 的语义不同 |
| 跨批次回放计数 | TestReplayBudgetCountsConsumedRecordsAcrossSegments |

## 证据与限制

日志保存在主检出 `artifacts/wal-single-path-20261008/`。首次测试因 Go cache 沙箱访问失败（format-red.log），不是行为证据；授权缓存访问后旧代码对 codec 5/6 返回 EOF 而非版本拒绝，两个子例均失败（format-red-r2.log）。最终用例加强为完整记录负载替换版本号，另有当前格式 Put/Patch/Delete/effect/receipt/Remote round-trip、截断与文件重开测试。

首轮普通回归发现旧 memory 测试夹具缺数据库/Remote 完整身份；按新规范补完整合法输入，继续验证 memory 不允许本地持久写，不改生产校验。新 Projector 回归最初误以为 held 返回 nil，随后误用了另一包的私有错误名；均只修测试，保留 single-path.log / race.log 原记录。

macOS Go 1.27.0、GOMAXPROCS=2、-p 1：全仓 build/vet/test 通过，131 个有测试包，含根包及生成器。七个目标包 race 均有通过结果：nestwal 5.378s、dataengine 2.698s、engine 6.442s（engine-race-r2.log）、nest 4.240s、kit/dataengine 2.065s、remoteentity 30.857s、saga 24.959s；第一次 race.log 中的 engine 编译失败保留，不冒称第一次整轮全绿。

生产 Go 代码 51 行增加、976 行删除，净减少 925 行（包括生成配置声明，不含测试）。不是吞吐或内存收益测量。

CBM 基线 2026-10-08T05:39:49Z；72 个已修改 Go/模板路径核对覆盖，63 个无记录缺口，7 个 testdata 路径按规则排除、2 个模板部分解析。排除路径的修改和模板缺口范围已直接读取，生成消费另行验证。chaos/cube/ssr 工作区搜索未找到 NewCommitter/OpenRuntime/WriterVersion/CanonicalizeMutation 调用，不代表未知仓外用户没有调用。

独立 game-demo 生成 458 文件，replace 到本次检出后 build/vet/test 通过（19 个有测试包，generated-*.log）。另按正式 DAO/Entity 命令生成 DataEngine 和 Remote 两套资源夹具，均编译通过（dataengine-compile.log / remoteflow-compile.log，-run '^$'，不计资源行为通过）。Linux/arm64 全仓交叉 build 通过（linux-build.log）；本轮没有 Linux 实机运行，不以交叉编译冒充运行验收。全仓 go generate 后工作树与已暂存源码完全一致、无新增文件（generate-final.log / generate-diff.log）。最终根包 -count=1 通过 5.919s（root-final.log）。性能测试继续暂停；本次未运行真实资源故障矩阵/跨机/部署，未自动处理旧数据。

新增关键回归：`nestwal/single_format_test.go:17`、`dataengine/engine/single_path_test.go:27`。提交状态以 Git 及核心交接顶部为准，本批不发版。

## 最终索引回填

已合入 main，代码提交 `53686c05`，其后文档提交不改 Go/模板。CBM 项目 `Users-whb-roost-roost-core` 在 main 强制刷新成功，generation `2026-10-08T06:26:11Z`，25615 nodes / 246197 edges，skipped 0，全项目 parse_partial 5。最终检查 70 个现存改动 Go/模板路径：61 个 metadata_match 且无记录缺口、7 个 testdata 按规则排除、2 个模板部分解析（enter_game_test 第127行；guild_ids_test 第169、175行），排除改动与缺口范围均再次直接读取。索引信号不代表全仓完整性证明，原始结果保留在 `artifacts/wal-single-path-20261008/cbm-final.json`。未重启共享 CBM 服务。
