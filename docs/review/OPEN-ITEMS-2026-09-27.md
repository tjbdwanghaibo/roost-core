# 2026-09-27 未完成 / 未验证 / 残留条目清单（v1.17.1 之后）

- 基线：main `ffcf902`（= tag v1.17.1 之后的文档提交；v1.17.1 tag 指向同一提交），工作树干净。行号均基于 `ffcf902`。
- 盘点日期：2026-09-27。由只读盘点员逐份读取后汇总，主会话复核；本文件是之后逐条处理的主清单，进度记在 §H。
- codebase-memory 图谱已过期（交接文档写 generation `2026-09-25T11:41:37Z`，audit3/4 写 09-26 08:36），本稿全部以源码、`rg`、`git` 和实跑为准。
- 链接写法：`bf-NN` = `docs/bugfix/RR-20260926-NN.md`，`bug-NN` = `docs/bug/RR-20260926-NN.md`。节名写在 `§` 后面。中文锚点不稳定，所以链接只到文件，节名请在文件内搜索。

## 0. 读取范围与方法

| 来源 | 读取情况 |
| --- | --- |
| `docs/bugfix/RR-20260926-03.md` ～ `-85.md`（83 份） | 全部读到末尾。由 7 个并行只读子任务分段读，每个子任务逐文件回报“读完”和行数 |
| `docs/bug/RR-20260926-01.md` ～ `-85.md`（85 份） | 全部读到末尾（2 个子任务）。末尾后加节只有 73/74/75/76/80 的“复核残留（2026-09-27 第四轮审计，未修）”；没有标题叫“更正”的节，更正写在正文里 |
| `docs/review/REVIEW-2026-09-26-{audit,core-optimization,fix-verification,followup,release,release-fixes,v1170-triage}.md`、`REVIEW-2026-09-27-audit{2,3,4}.md`、`EVIDENCE-2026-09-26-985d5ba-negative.md` | 本人全文读完 |
| `docs/CORE-OPTIMIZATION-HANDOFF.md` | 本人全文读完（§1～§7，273 行）。§7 全部 85 行都是“修复” |
| `docs/bug/CARRYOVER.md`、`docs/bug/WANTED.md` | CARRYOVER 全文读完；WANTED 读了全部二级标题，未分流条目读了正文 |
| `docs/bugfix/README.md` | 读了头部和末尾的“要先定什么 / ARCH 状态”段落 |

没有读不完的文件。说明两点：
- bf-03～09 的未验证项指向 `REVIEW-2026-09-26-followup.md`，bf-10～24 指向 `REVIEW-2026-09-26-release-fixes.md`，这两份已读，内容已并入。
- CARRYOVER 的“A→线”6 条（A1/A2/A6/A7/A8/A14）已在 09-21 分流到 feature / ARCH 线，按要求不列。

本次额外实跑（只读，不改文件）：
- `GOWORK=off go test -count=2 ./...`：只有本地未跟踪的 `artifacts/perf/remote/*` 三个源码备份包失败（交接文档 §6 已说明不能纳入 `./...`）。**没有其他包在整包重复运行时失败。**
- `GOWORK=off go test -count=2 ./nest ./hotcode ./entity`：ok。
- `GOWORK=off go test -count=2 -run TestRollbackStateRestoresDaoAndDirty ./nest`：**FAIL**，`panic: nest: duplicate handler "test_rollback_state"`（见 A07）。
- `gofmt -l nest entity sync codegen kit dataengine remoteentity app`：列出 21 个文件（见 A06）。
- `gh run view 36306850323 --log-failed`：`ffcf902` 的 ci / windows-compatibility 失败（见 A01）。

---

## A. 确定缺陷，可在本地修（13 条）

| 编号 | 一句话 | 来源 | 当前状态 | 建议处理 | 规模 |
| --- | --- | --- | --- | --- | --- |
| A01 | **`ffcf902` / v1.17.1 的 CI windows-compatibility 是红的**：RR-80 新增的两个用例用 `os.Chmod(dir, 0o555)` 造只读目录，Windows 上不生效，`SyncProject` 没有失败 | 本次核对 `gh run view 36306850323`（main）和 `36306851665`（tag v1.17.1）；bug-01～42 子任务回报的新发现；用例由 `520855b`（RR-80）引入 | 已核实仍成立：`codegen/internal/roost/shutdown_budget_promises_test.go:296-299`（readOnlyDir）、`:319`、`:341`；CI 日志 `sync succeeded although configs/service is read-only` | Windows 上 `t.Skip`，或改用 Windows 也生效的写失败注入（例如把目标文件换成同名目录）；然后看 windows job 转绿 | S |
| A02 | 生成的 player TCP `CloseSessions` 永远返回 0：`session.Close` 返回的 `closeErr` 至少是 `reason`（nil 时补成 `ErrSessionClosed`），不可能为 nil，所以 `closed++` 永远不执行 | [audit2 §疑点](REVIEW-2026-09-27-audit2.md)（“生成传输 closeSessions 恒返回 0”） | 已核实仍成立：`codegen/internal/roost/render_player_tcp.go:939-952`、`:1039-1048`；唯一消费方 `demo/internal/service/game/playerowner.go.tmpl:686`，只写进日志字段 `sessions_closed`（`:703`） | 按“是否真的关掉了这个会话”计数（例如 `closeOnce` 首次执行时计数），不看 `closeErr` | S |
| A03 | 生成的 `deploy/dev/run.sh` 注册游戏服时不传 `redis.db`：非 0 db 的开发配置会把服务器登记写进 db 0，机器人报 `account: server is invalid` | [bf-56 §未验证项与观察](../bugfix/RR-20260926-56.md) | 已核实仍成立：`codegen/internal/roost/render_dev_run.go:122-132` 只读 `addr` 和 `key_prefix`；`accountctl` 已有 `-redis-db` 参数（`demo/cmd/accountctl/main.go.tmpl:33`） | awk 同时读 `redis.db`（以及 `password`），传 `-redis-db` | S |
| A04 | demo 账号 ID 计数键 `roost:demo:player_id` 写死，不带配置的键前缀；同一 Redis db 上的多份部署会共用计数器 | [bf-56 §未验证项与观察](../bugfix/RR-20260926-56.md) | 已核实仍成立：`demo/internal/service/account/collaborators.go.tmpl:54`；`demo/README.md:398` 也按这个固定键描述 | 改为从 `account.key_prefix` 派生；同时改 README。core 的 Remote 快照键另见 C11 | S |
| A05 | 仓库根目录被 git 跟踪了一个 3 MB 的 `glsvet` Mach-O arm64 可执行文件 | 维护者给出的已知事实 | 已核实仍成立：`git ls-files` 命中 `glsvet`；由 `05f109a`（2026-08-26，“v6.4”）加入；`.gitignore` 没有对应条目；正式入口是 `go run ./cmd/glsvet` | `git rm --cached glsvet` 并在 `.gitignore` 加 `/glsvet`（历史里的大小不会变小，要不要改写历史由维护者定） | S |
| A06 | 21 个 Go 文件未 gofmt（全仓 34 个，不含 `.claude/`）；CI 不检查 gofmt | [bf-37 §验证](../bugfix/RR-20260926-37.md)、[bf-63 §验证](../bugfix/RR-20260926-63.md)、[bf-84 §未验证项 / 风险](../bugfix/RR-20260926-84.md)、[release §发布门禁](REVIEW-2026-09-26-release.md)（当时 33 个） | 已核实仍成立。本次 `gofmt -l` 结果：`nest/missing_entity_promises_test.go`、`codegen/internal/entity/testdata/syncruntime/roundtrip_test.go`、`kit/configdata/configdata.go`、`kit/dataengine/fatal_fence_test.go`、`kit/dataengine/mod_promises_test.go`、`kit/lock/lock_mod.go`、`kit/lock/lock_mod_test.go`、`kit/manager/manager_mod.go`、`kit/manager/manager_mod_test.go`、`kit/mongo/mongo_mod.go`、`kit/nats/jetstream_rpc_toxic_integration_test.go`、`kit/nats/nats_mod.go`、`kit/nest/mod_guards_promises_test.go`、`kit/ops/ops_mod.go`、`kit/redis/redis_mod.go`、`kit/saga/mod.go`、`kit/service/chat/prune_failure_promises_test.go`、`kit/service/examples/split/split_test.go`、`kit/service/global/activity/sweep_failures_promises_test.go`、`kit/service/rank/redis_store.go`、`kit/statslog/gauges_test.go` | 一次性 `gofmt -w`（单独提交，不混逻辑改动）；要不要在 CI 加 `gofmt -l` 检查由维护者定 | S |
| A07 | nest 包几个用例直接注册全局 handler，**单独**用 `-count>1` 跑会 panic `duplicate handler`，限制了“对单个用例 -count=N 复验” | [bf-32 §验证](../bugfix/RR-20260926-32.md)、[bf-35 §验证](../bugfix/RR-20260926-35.md)、[bf-48 §验证](../bugfix/RR-20260926-48.md)、[bf-59 §验证](../bugfix/RR-20260926-59.md) | 已核实仍成立：`go test -count=2 -run TestRollbackStateRestoresDaoAndDirty ./nest` 在 `nest/nest_test.go:856` panic。子任务另报 `nest_test.go:1053`、`pipelined_commit_test.go:105` 同类。**整包** `-count=2` 通过（别的用例的 `t.Cleanup(ResetHandlersForTest)` 碰巧清掉了） | 这些用例在注册后 `t.Cleanup(ResetHandlersForTest)`，或改用每个用例唯一的 handler 名 | S |
| A08 | doctor 对 `shutdown.total_timeout: 0s` 或负值的判定和 App 不一致：App 把非正值兜底成 30s，doctor 的 WARN 分支按 0 算成“覆盖不了”并给建议值 | [bf-80 §复核后的补修（2026-09-27）](../bugfix/RR-20260926-80.md)（自报“本次未改”） | 已核实仍成立：`codegen/internal/roost/shutdown_budget.go:426` 与 `:548-553`（`parseConfigDuration` 原样返回 0）；FAIL 分支 `:170-178` 按 `<=0 → appDefaultShutdownTotal` 处理；App 兜底 `app/app.go:313-316` | WARN 分支也走同一个“非正值按 30s”规则 | S |
| A09 | doctor WARN 里的 “Set it to …” 建议值按常量 30s 的 dataengine 预算算，不读配置里的 `dataengine.shutdown_timeout`；调大后建议值偏小。RR-77 说“属 RR-80 范围”，RR-80 说“属 RR-77 描述的范围”，两边都没修 | [bf-77 §决策与实现](../bugfix/RR-20260926-77.md)、[bf-80 §未验证项 / 风险](../bugfix/RR-20260926-80.md) | 已核实仍成立：`shutdown_budget.go:41`（`generatedDataEngineShutdownTimeout = 30s`）、`:106`、`:463` | doctor 已经解析了配置，建议值改用配置里的 dataengine 预算；`project sync` 的公式保持（D11） | S |
| A10 | `RemoteCommit.Validate` / 提交路径不拒绝 sid 作用域 DAO：手写实体注册的 DaoBuilders 与实际 DAO 不一致时，能绕过 RR-45 的装配校验，回到“提交与加载选库不一致” | [bf-45 §未验证项](../bugfix/RR-20260926-45.md)（自报“后续加固点”） | 已核实仍成立：`entity/remote_protocol.go:267` 的 `Validate` 不看 `DatabaseScope`；`remoteentity/mongo_payload.go:36` 仍按 `DatabaseScope` 选库；`DatabaseScope` 在 remoteentity 生产代码里只出现在 `assemble.go:274` 和 `mongo_payload.go:36,39` | 在 `Validate` 或 `mongo_payload` 选库处对 sid 作用域 fail-fast。这是加固，需维护者点头 | M |
| A11 | `kit/README.md` 仍写“共用一个 NATS 的部署用不同 prefix 即各有各的流”，没有注明 `roost.room` / `roost.sync` 的兼容映射例外（两者都映射到 `ROOST_SYNC`） | [audit2 §须随发布补的文档](REVIEW-2026-09-27-audit2.md) | 已核实仍成立：`kit/README.md:502`；`CHANGELOG.md:17` 已经承认这句不成立，但 README 本身没改 | 改 README 这一句 | S |
| A12 | 修复记录和代码注释里有若干已被后续修复推翻、却没改的说法 | 见右列各处 | 已核实仍成立：① bf-76 称“真实 WAL 已 terminal、会拒绝”，对 `acceptPersistence` 来源不成立（[bug-76 §复核残留](../bug/RR-20260926-76.md)；`nest/persist_change.go:312-327`）；② [bf-33 §5 自查](../bugfix/RR-20260926-33.md)“成功路径只多一次 RLock”，现在是原子指针（`nestwal/wal.go:132`、`:1262-1266`）；③ [bf-39 §与 RR-30/RR-35 的统一](../bugfix/RR-20260926-39.md)“RR-37 提交后回调另有就地兜底”，已被 RR-61 改成不执行（`remoteentity/transaction_manager.go:374`）；④ [followup §决策边界](REVIEW-2026-09-26-followup.md)“带 loader 的冷缺失在快池 panic”，已被 RR-26 推翻；⑤ [bf-02:17,19](../bugfix/RR-20260926-02.md) 的证据只在本机 `/tmp/roost-nine-logic-race*.txt`；⑥ `remoteentity/replay_publication_e2e_test.go:234` 注释仍说 dirty 不是并发安全的（RR-63 已加锁）；⑦ [bf-68 §交付前对抗性自查](../bugfix/RR-20260926-68.md) 称“短截止推送不关半死连接”已写进注释，生成器 `render_player_tcp.go:171-179`、`:1017-1024` 没写这个后果 | 按“后续更正”格式在原记录末尾追加更正，不改写原文；代码注释直接改 | S |
| A13 | 索引、状态字段、交接文档里的状态文字滞后或互相矛盾 | 见右列 | 已核实仍成立：① `docs/bugfix/README.md:9` 写 RR-82“未发版”，交接文档 `:269` 和 `docs/bug/README.md` 写 v1.17.1；② bug 主记录第 3 行的“状态”全是登记时的原值（43～85 全部，01～42 大多数），只 grep“未修复”会全部误中；③ 交接文档顶部 `:3`、`:5` 和 §5 `:104`、`:105` 仍写“未提交 / 未修复 / 当前工作树未提交”；④ `docs/bugfix/README.md:430-431` ARCH-11/12 仍写“待拍板”，而 `sync/` 布局与 statesync 删除已落地、共享帧多播已在交接文档 §5 `:100` 定为独立需求；⑤ 交接文档 §3 `:59` 的“N1～N4”和 `:63` 的“N1～N3”编号重名；⑥ §5 `:114` 的图谱代际写 `2026-09-25T11:41:37Z`，audit3/4 写 09-26 08:36；⑦ §5 `:102`“最新九项版本未完整重跑”已被 `:110` 的 21/21 取代；⑧ RR-54 包装 getter 的契约只写在修复记录，`docs/USER_GUIDE.md:243-244` 没提（见 D05） | 一次文档整理提交：状态字段回写或加“最终状态见修复节”说明；交接文档顶部与 §5 改成当前状态 | M |

## B. 未验证，可在本地验证（38 条）

本机集成环境见记忆：`source /tmp/roost-dataengine-it/env.sh`（brew 二进制，不需要 docker）。性能对照每侧 25～35 分钟，要后台跑。

| 编号 | 一句话 | 来源 | 当前状态 | 具体做法 | 规模 |
| --- | --- | --- | --- | --- | --- |
| B01 | **WANTED 里唯一未分流的条目**：Redis toxiproxy 用例 `TestToxicRedisDroppedAcquireReplyIsReconciledNotRetried` 在真实矩阵里 3/4 红，疑似 SETNX 回复丢失后 `Release` 没走按 token 删 | [WANTED §W-2026-09-22-01](../bug/WANTED.md)（`:97`）；`docs/bugfix/RR-20260922-02.md:76`（“等 review 定性”） | 已核实仍成立：用例仍用固定键 `toxic:acquire`（`redis/driver/lock_toxic_integration_test.go:143`、`:160`），相邻运行会互相污染；`lock.go:99-158` 已有 uncertain 状态；`scripts/test-remote-matrix.sh` 不含该用例（只有 `:48` 的 `TestToxicNATS`），`kit/scripts/integration/dataengine-env.sh:138` 的 `./redis/...` 会跑到它。09-26 以来的门禁都跳过了 Toxic | `source /tmp/roost-dataengine-it/env.sh && GOWORK=off go test -tags=integration ./redis/driver -run TestToxicRedisDroppedAcquireReplyIsReconciledNotRetried -count=5 -v`；先给用例加随机键名或 `t.Cleanup` 删键再看是否仍红，然后按 WANTED 流程三选一 | S |
| B02 | RR-41 跨页半写回形态没有测试：帧跨 4 KiB 边界，前页（含帧头）写回、后页未写回为 0，会以 payload CRC 错误拒绝启动（不截断） | [audit §记录但不修](REVIEW-2026-09-26-audit.md)；[bf-41 §2 未采用方案、§6](../bugfix/RR-20260926-41.md) | 已核实仍成立：`nestwal/wal.go:1398`（payload checksum）、`:1093-1111`（`recoverTail`）、`:1141`（`allZeroFrom`）；现有 `zero_tail_promises_test.go` 只覆盖整段追加零和整帧清零 | 在 `nestwal/zero_tail_promises_test.go` 加用例：写一条跨 4 KiB 边界的记录，把后半页清零、文件长度不变，断言 `ErrCorrupt` 且文件未改。先固化成契约；要不要放宽见 C08 | S |
| B03 | RR-76：`acceptPersistence` 在 committer 成功后返回 `ErrCommitIndeterminate` 时 WAL 并未 terminal，fence 之后外层自己的提交仍会被接受 | [bug-76 §复核残留](../bug/RR-20260926-76.md)；[bf-76 §未验证项 / 风险](../bugfix/RR-20260926-76.md) | 已核实仍成立：`nest/persist_change.go:312-327`（`:322` Join `ErrCommitIndeterminate`）；`nest/execution.go:271-282` 只 fence / abandon；`nestwal/wal.go:340-342` 只在 terminal 时拒绝 | nest 包写探针：外层 undo_strict + 真实 nestwal committer，handler 内 `RunIsolatedTransaction` 写一个 `AcceptMutation` 返回错误的 DAO，断言 `FenceError()!=nil` 之后外层记录是否仍被追加。结果交 C07 | S |
| B04 | `skill/combatcomponent` 的 `RunDetachedTransaction` 会不会在 memory handler 里被调用（决定 RR-76 的影响面） | [audit3 §疑点](REVIEW-2026-09-27-audit3.md)；[bf-76 §未验证项 / 风险](../bugfix/RR-20260926-76.md) | 已核实仍成立：`skill/combatcomponent/adapter.go:71-72`、`:297`，`status_bridge.go:56`，条件是 `nest.CurrentRollbackTx()==nil`，memory handler 满足 | memory handler 内调 `HostAdapter.Apply`，committer 返回 `ErrCommitIndeterminate`，断言 `FenceError()` 满足 `ErrNestFenced` | S |
| B05 | 嵌套派发的 try-lock 分支（`GuardedCount()>0`）在 handler 内是否可达，一直没找到入口 | [audit3 §疑点](REVIEW-2026-09-27-audit3.md)；[bf-73 §未验证项 / 风险](../bugfix/RR-20260926-73.md) | 已核实仍成立：`nest/nest_dispatch.go:407`，唯一调用方 `nest/group_lock.go:265` | 从 `group_lock.go:265` 所在函数往上追调用链；可达就补回归，不可达就在代码里注明 | S |
| B06 | 自定义 `lock.Mutex` 若是不可比较的值类型，`holding` 的接口比较会 panic | [bf-67 §未验证项 / 风险](../bugfix/RR-20260926-67.md) | 已核实仍成立：`entity/entity_guard.go:304-312`（`current.GetMutex() == ent.GetMutex()`）；框架实现都是指针（`lock/reentrant_mutex.go:28`、`lock/lock.go:28`） | entity 包写一个带 func 字段、值接收者的 Mutex，同 ID 两实例各一份，调 `RequireEntity` 看是否 panic | S |
| B07 | `ReleaseCast` / `ReleaseEntity` 按 ID 释放：Destroy 并重建同 ID 后，用旧指针调用会释放新实例 | [audit3 §疑点](REVIEW-2026-09-27-audit3.md)（“理论角落，无探针”） | 已核实仍成立：`nest/cast.go:205`（`ReleaseEntity(e.GUId())`）、`entity/entity_guard.go:422-428` | 无事务 handler 内 Cast X → Destroy X → 新建 X → `ReleaseCast(旧X)`，断言新 X 的锁仍被持有 | S |
| B08 | D2 并行窗口“成功后缀重放”没有 remoteentity 端到端用例；RR-21 修好后正式装配已开启并行，这条的意义变大了 | [bug-11 §复核后的补修](../bug/RR-20260926-11.md)；[bf-11 §兼容与未验证项](../bugfix/RR-20260926-11.md) | 已核实仍成立：`remoteentity/replay_publication_e2e_test.go:106` 只测串行；`dataengine/engine/projector_remote_test.go:84` 用假 store | e2e：Backend + `RemoteProjectionWorkers>1` + 真实 nestwal + mongotest；前缀一笔瞬时失败、后缀热点实体已被新 fence 覆盖；断言重试后 WAL 全 ack、tracker 仍 Committed、fence 不回退 | M |
| B09 | RR-19 端到端没补：MongoStore 跳过 → 真实 Manager 收到拒绝 → 实体恢复可写（新写入在准入处已禁，只剩历史混合记录会走） | [bug-19 §复核后的补修](../bug/RR-20260926-19.md)；[bf-19 §未验证项](../bugfix/RR-20260926-19.md) | 已核实仍成立：只有单元级 `remoteentity/replay_outcome_test.go:148`；`remoteentity/backend.go:103` | 绕过准入直接往 WAL 写“Remote + 过期 lease fence”混合记录，MongoStore + mongotest + 真实 Manager 投影，断言 Rejected、gate / 额度释放、重载后可写 | M |
| B10 | RR-17：Runtime / Assembly 级“重试停机 + 新拥有者 checkpoint 不回退”只有审查探针覆盖 | [bug-17 §复核残留 (2)](../bug/RR-20260926-17.md)；[bf-17 §未验证](../bugfix/RR-20260926-17.md) | 可能仍成立：有 Projector 级 `dataengine/engine/projection_lifetime_test.go:24` 和 `nestwal/close_replay_test.go:103`，`assembly_shutdown_promises_test.go:28` 只测“重试等同一批组件”，没找到新拥有者重开断言 | 外部 `rt.Flush` 阻塞在 store，先 `Shutdown(50ms)` 再 `Shutdown(Background)`，同目录 `OpenRuntime`，断言 checkpoint 不回退；去掉修复做负对照 | M |
| B11 | RR-10：正式 Nest 慢阶段入口（`SendOptionSlow` → Repository 等待投影）没有入库的端到端回归；RR-25 用过的临时测试没入库 | [bf-10 §未验证](../bugfix/RR-20260926-10.md)；[bf-25 §未验证项](../bugfix/RR-20260926-25.md) | 已核实仍成立：`codegen/internal/entity/testdata/dataengine/reload_projection_test.go` 两个用例都不经 Nest Request；rg 找不到 `TestRR25` | 把“投影挂起 → 卸载 → RequestMulti”加进 testdata/dataengine，用 `scripts/test-dataengine-generated.sh` 跑 | M |
| B12 | RR-28：真实 Mongo 上“非事务插入拒绝记录 vs 在途事务插入同 `_id`”的裁决时序没验证，只在 mongotest 上验证过 | [bug-28 §修复](../bug/RR-20260926-28.md)；[bf-28 §未验证项与边界](../bugfix/RR-20260926-28.md) | 已核实仍成立：`remoteentity/mongo_committer.go:463`（`RejectUnresolvedRemoteCommits`）、`:471-472` | 本地副本集 `-tags integration`：两种先后都测（事务先插未提交 / 拒绝先插） | M |
| B13 | RR-37/38：tracker 已被容量或 TTL 淘汰时 finalizer 直接回源，这条补充没有专门回归 | [bf-37 §RR-38 复核补充](../bugfix/RR-20260926-37.md) | 已核实仍成立：`remoteentity/transaction_manager.go:294-299`、`transaction_tracking.go:222-233`；`transaction_wait_eviction_promises_test.go:39` 只测 `waitRemoteTransaction` | Durability 1 strict 超时收尾入队后删掉 tracker，`FinalizeProjectionTimeout=30s`，断言远早于超时就调了 `RemoteCommitStatus` | S |
| B14 | RR-38：两个发布者同时在解冻中途（一方已 Quarantined→Recovering，另一方拿着过期 Quarantined）的窗口没构造过 | [bf-38 §未验证项与边界](../bugfix/RR-20260926-38.md) | 已核实仍成立：`remoteentity/transaction_manager.go:1160`（`thawAcknowledgedRemote`）、`:1171` | 用 stub 让一方停在 Recovering→LocalOwned 之间，另一方 `acknowledgeRemoteCommit`，断言报错后下一轮收敛、投影失败计数只 +1 | S |
| B15 | RR-34：“快照读没命中却撞键”的非 fatal 分支没有用例 | [bf-34 §未验证项](../bugfix/RR-20260926-34.md) | 已核实仍成立：`dataengine/engine/mongo_store.go:429` 起的 `stageEffect` 分支，rg 在测试里找不到该错误文本 | 伪 Mongo 上在 FindOne 与 InsertOne 之间插入同 ID 文档，断言非 fatal、`errors.Is(ErrDuplicateKey)`、整笔回滚 | S |
| B16 | RR-64：带 Remote 批次的 memory handler 内新建实体冲突没有单独回归 | [bf-64 §未验证项 / 风险](../bugfix/RR-20260926-64.md)（audit3 另用探针补验过，未入库） | 仍成立（仓内无对应用例） | 仿 `non_rollback_create_conflict_promises_test.go`，挂 RemoteWriteBatch 替身；断言只执行 1 次、带 `ErrCreatedEntityLockConflict`、不带 `ErrLockTimeout` | S |
| B17 | RR-67：Remote 实体 Destroy 后同 ID 重建没有单独回归 | [bf-67 §未验证项 / 风险](../bugfix/RR-20260926-67.md) | 已核实仍成立：`nest/destroy_recreate_same_id_promises_test.go` 无 remote 字样 | 用 remote-managed 实体 + 受控批次复刻 `TestDestroyThenRecreateSameIDInHandlerLocksNewInstance` | S |
| B18 | RR-81：Destroy **收尾**窗口撞上 handler 内新建，以及同一 Guard 嵌套撤销后外层再建，这两种形状没有单独回归 | [bf-81 §未验证项 / 风险、§对抗性自查](../bugfix/RR-20260926-81.md) | 分支仍在：`entity/manager_factory.go:91` | 仿 `created_entity_revoke_window_promises_test.go` 在 Destroy post-release 挂同步点；另写嵌套 `RunIsolatedTransaction` 新建后回滚的用例 | S |
| B19 | RR-84：消息自己的事务开始前（如慢阶段 prepare）调用 `RunIsolatedTransaction` 只有源码推断 | [bf-84 §未验证项 / 风险](../bugfix/RR-20260926-84.md) | 判据在 `nest/execution.go:112-116` | 慢阶段 prepare 回调里调用：Remote 消息断言 `ErrNestedTransactionInRemoteMessage`，本地消息断言不认领 | S |
| B20 | RR-54：非 Nest 的领头方持有别的实体锁时触发冷加载，发布改在加载 goroutine 上就地执行，没有专门覆盖 | [bf-54 §未验证项 / 风险](../bugfix/RR-20260926-54.md) | 已核实仍成立：`entity/manager_access.go:353-371` | `WithGuardScope` 持有 X 时 Get 冷加载 Y，loader 发布需要 Y 的锁，断言不死锁、Y 已发布 | S |
| B21 | RR-78：“登记与 `releaseRetracted` 之间被撤销”的窗口，仓内没有确定性回归（audit4 用的测试缝没入库） | [bf-78 §未验证项 / 风险](../bugfix/RR-20260926-78.md)；[audit4 §修复核验结论](REVIEW-2026-09-27-audit4.md) | 已核实仍成立：`sync/entitysync/manager.go:283-286` | 在这两行之间加未导出测试缝，把 audit4 的 `aud5RegisterBeforeHandBack` 探针收进仓 | S |
| B22 | RR-59：退回 remove 之后 Interest/AOI 是否重新订阅没验证；“Remote 持久拒绝 + 订阅者”只在 remoteentity 层用替身 loader 验证 | [bf-59 §未验证项与边界](../bugfix/RR-20260926-59.md) | 已核实仍成立：`sync/entitysync/manager.go:499`、`:523` | policy 包：订阅中实体经 `RetractUnloadedSubject` 收到 remove，再重载 + Rebind，断言观察者重新收到 create；另在 remoteflow fixture 做真实拒绝 | M |
| B23 | RR-77 判别表第 2 行（`ErrRemotePersistenceIndeterminate`）按 RR-37 记录写入，没有重跑 Remote 用例核对哨兵组合 | [bf-77 §未验证项 / 风险](../bugfix/RR-20260926-77.md) | 仍成立 | 跑 remoteentity 里 RR-37 的结果未知用例，打印回复链上的哨兵组合，对照 USER_GUIDE 判别表 | S |
| B24 | RR-48：对称交叉创建是否会活锁：重排延迟固定 5ms、无抖动，理论上可能连续冲突到 400 次上限 | [audit2 §疑点](REVIEW-2026-09-27-audit2.md)（“300 次试验未达上限”）；[bf-48 §未验证项 / 风险](../bugfix/RR-20260926-48.md)；[bf-81 §未验证项 / 风险](../bugfix/RR-20260926-81.md) | 已核实仍成立：`nest/group_transition.go:17-18`、`:381-393` | 多对 handler 每次在屏障后同时交叉 Create，`-race -count=500`，统计 `nest.dispatch.requeue.total` 和是否有请求耗尽 400 次。要不要加抖动见 C09 | M |
| B25 | demo 场景自建的 entitysync Manager 没接 `ConfigureUnloadResync`，实体被仅内存卸载后场景订阅者不会主动重载 | [audit2 §疑点](REVIEW-2026-09-27-audit2.md)（“demo scene Manager 未接 RR-59 重载链路”） | 已核实仍成立：`demo/internal/service/game/scene.go.tmpl:212-217` 自建 Manager；框架只在 `kit/nest/nest_mod.go:175` 给 kit 自己的 Manager 接线；USER_GUIDE `:275` 要求自建装配自己接 | 在生成的 game-demo 里构造“场景内玩家实体被 RR-30 跳过驱逐 / RR-39 拒绝卸载”，看观察者是否停在旧内容；可达则在模板里接线（变 A） | S |
| B26 | RR-28：finalizer 回滚是不是在 Nest 快池执行，两份子报告结论相反 | [bf-28 §未验证项与边界](../bugfix/RR-20260926-28.md)（称“不是投递到快池”）；[bug-28](../bug/RR-20260926-28.md) | 本人核对：`remoteentity/transaction_manager.go:530` 用 `entity.RunLocal(m.localContext(ctx), …)`，`local_runtime.go:26-31` 在绑定了 Nest 执行器时注入 `NestMgr.RunLocal`，未绑定时就地执行。**kit 装配下原说法可能已不成立** | 写断言：kit 装配下拒绝回滚的 fn 在快 worker 上执行（`fctx.InFastWorker`）；然后更正 bf-28 | S |
| B27 | 真实环境 / 生成工程端到端补测（多条修复只用替身、受控 committer、回环或 recorder 验证） | bf-25、bf-27、bf-28 §复核更正、bf-30 §6、bf-32、bf-35、bf-36、bf-37、bf-39、bf-40、bf-43、bf-44、bf-46、bf-48、bf-52、bf-53、bf-55、bf-63、bf-68、bf-70、bf-71、bf-74、bf-75、bf-84 各自的“未验证项”节 | 仍成立（各记录自报，没有后续补测记录） | 按每份记录给的场景写成 remoteflow / dataengine fixture 用例或集成用例，统一用 `scripts/test-remote-generated.sh`、`test-dataengine-generated.sh` 跑。可按“Remote 结果未知 / 提交后 hook 失败 / 冷登录与重连 / 真实 Redis 续期”分四批 | L |
| B28 | 发版 HEAD 没跑的门禁：`test-dataengine-generated.sh`、`test-remote-generated.sh`、`go test -tags integration`（不含故障注入） | [bf-51 §未验证项](../bugfix/RR-20260926-51.md)、[bf-54](../bugfix/RR-20260926-54.md)、[bf-66](../bugfix/RR-20260926-66.md)、[bf-80](../bugfix/RR-20260926-80.md)；交接文档 `:110` | 仍成立：`:110` 只列 build / vet / glsvet / 整仓 race / sync-modes / 故障矩阵 | `source /tmp/roost-dataengine-it/env.sh` 后按交接文档 §6 顺序跑这三项，结果补进交接文档 | M |
| B29 | 性能对照缺失 | [bug-10 §复核残留 (3)](../bug/RR-20260926-10.md)、[bug-16 §验收覆盖点](../bug/RR-20260926-16.md)、[bf-04 §性能对照](../bugfix/RR-20260926-04.md)、[bf-05 §性能](../bugfix/RR-20260926-05.md)、[bf-25](../bugfix/RR-20260926-25.md)、[bf-30 §5](../bugfix/RR-20260926-30.md)、[bf-47](../bugfix/RR-20260926-47.md)、[bf-48 §性能](../bugfix/RR-20260926-48.md)、[bf-73](../bugfix/RR-20260926-73.md)、[bf-74](../bugfix/RR-20260926-74.md)；[fix-verification §未做](REVIEW-2026-09-26-fix-verification.md) | 仍成立。注意 bf-04 的前提“sync-aoi 没有字节预算参数”不成立：`scripts/perf/sync-aoi/main.go:61` 有 `-snapshot-bytes`，`:318` 传进 MaxBytes | 空闲机器上分批：Nest 微基准 + `nest-msg`（RR-25/47/73/74），`BenchmarkProjectorAdmissionMatrix` async/pipelined（RR-10/16/30），`sync-aoi -snapshot-bytes`（RR-04/05）；各用 benchstat 对比修前父提交与 HEAD | L |
| B30 | 正式 kit Backend 装配下 Remote 30 分钟持续与容量阶梯没复测；交接文档 §4 的 80/120/160 TPS 仍来自直接注入 MongoCommitter 的装配 | 交接文档 §5 `:105`；[bug-21](../bug/RR-20260926-21.md)、[bf-21 §兼容与边界](../bugfix/RR-20260926-21.md)；[release-fixes §正式 Backend 装配短测](REVIEW-2026-09-26-release-fixes.md)（只有 60s/100TPS） | 仍成立。fixture 已走 Backend（`codegen/internal/entity/testdata/remoteflow/flow_test.go:157-158,248`） | 后台跑 `scripts/perf/remote.sh`（30 分钟）和 `remote-capacity.sh`，新 label 目录，完成后更新 §4 | L |
| B31 | RR-56：没在真实 NATS 上演练“非默认 prefix 旧部署升级 → subjects overlap → 配 `stream: ROOST_SYNC` 恢复” | [bf-56 §未验证项与观察](../bugfix/RR-20260926-56.md) | 仍成立：`sync/syncbus/driver/jetstream.go:301` | 本机 `nats-server -js`：基线版本以非默认 prefix 起，换 HEAD 启动断言报 overlap，再配 `stream: ROOST_SYNC` 断言能起、游标连续 | S |
| B32 | RR-24：v1.16.1 工程在不加 replace 的前提下升级到已发布的 v1.17.1 没验证过（当时 tag 还不存在） | [bug-24 §2026-09-26 修复更新](../bug/RR-20260926-24.md)；[bf-24 §未验证项](../bugfix/RR-20260926-24.md) | 前提已变：v1.17.0 / v1.17.1 已发布；CI 的 released lane 是新建工程，不是升级 | 用 v1.16.1 生成器建 game-demo，`GOWORK=off` 用 v1.17.1 CLI 跑 `project upgrade --consolidate` 后 `go build`，框架符号应全部被列出或能编译 | M |
| B33 | RR-45：`codegen/internal/entity/testdata/remote/guild_gen_wire.go` 与重新生成结果不同（基线就有的夹具漂移），当时已还原、未处理 | [bf-45 §验证 / 门禁结果](../bugfix/RR-20260926-45.md) | 可能仍成立（最后一次改动是合仓 `4dcfbaf`，本次未重新生成比对） | 在临时 worktree 重新生成该 testdata 与仓内 diff；有差异就更新夹具，或确认它不是 golden | S |
| B34 | 真实进程停机时长分布没实测（新默认值下每个 Mod 拿到多少、总耗时多少） | [bf-42 §未验证项](../bugfix/RR-20260926-42.md)、[bf-51 §未验证项](../bugfix/RR-20260926-51.md)、[bf-66 §未验证项](../bugfix/RR-20260926-66.md) | 仍成立（audit3 在隔离环境看过“23 个 Mod 均 ≥3s、无告警”，但没记分布） | 生成 game-demo，`deploy/dev/run.sh start` 后对 game 发 SIGTERM，从 `mod stop` 日志统计每个 Mod 的 budget 与实际耗时、是否在 106s 内退出 | M |
| B35 | RR-35 与 RR-36 对生成工程 go.work 模式说法矛盾：35 报 genproto `ambiguous import` 只好改 replace，36 用 go.work 却通过 | [bf-35 §验证](../bugfix/RR-20260926-35.md)、[bf-47 §验证](../bugfix/RR-20260926-47.md)、[bf-36](../bugfix/RR-20260926-36.md) | 未复跑 | HEAD 生成 game-demo，`go.work use . <repo>` 后 `go build ./...`，记录是否仍报 ambiguous | S |
| B36 | RR-81～85，以及 audit4 之后做的 RR-73、RR-80 复核补修（`62b3ad7`、`eb05c7a`），都没有经过非修复方独立审计 | 交接文档 `:110`（“RR-83～85 未经独立审计”）；[audit4](REVIEW-2026-09-27-audit4.md) `:3-4`（范围只到 RR-73～80，RR-81/82 不在范围）；`docs/review` 下没有 audit5 | 已核实仍成立 | 按前四轮格式做第五轮审计：修前红（各修复提交的父提交）/ 修后绿 / 约束符合度 / 跨组交互 | M |
| B37 | 小的复验缺口：RR-83 `-shuffle=on` 的 seed 没记录；RR-85 在 `b95c895` 上的探针对照没重跑；RR-05 持续 RetryLater 下的会话公平性只做了推理 | [bf-83 §验证（修后）](../bugfix/RR-20260926-83.md)、[bf-85 §验证](../bugfix/RR-20260926-85.md)、[bf-05 §未验证项与边界](../bugfix/RR-20260926-05.md) | 仍成立 | `go test -count=2 -shuffle=on -v ./entity` 记 seed；`git worktree add` 到 `b95c895` 跑 REPRO-08 §2 探针 F；entitysync 写“持续 RetryLater N 个窗口，每个会话最终交付”用例 | S |
| B39 | RR-85：`Unregister` 在已被 forget 的旧 subject 上取释放戳，可能删掉同 ID 新登记的记录与 Direct 活绑定（audit5 疑点，推断） | [audit5 §疑点](REVIEW-2026-09-27-audit5.md) | 推断，未写探针 | 在 `Unregister` 取 `subj` 与加 `subj.mu` 之间加测试缝，构造 forget + 重新登记 + Bind，看新登记的绑定与撤销记录是否被删 | S |
| B40 | `TestSameIDCreateDoesNotOccupyFastPool/memory` 在高负载整包 `-race -count=20` 中偶发一次 `got ret=exists`；基线与分支重跑 `-count=20` ×2、单测 `-count=200` 均通过（批次 4 报告） | 批次 4 报告 | 未证明：疑为用例只等到 follower 计数自增、没等它真正进入 Create | 在用例里用事件钉住“follower 已进入 Create”，高负载（`-race -count=20` 与另一重包并行）下复跑；是测试时序问题就修测试，否则转 RR | S |
| B41 | `RetractSyncSubject` 在锁外比对 `subj.state==state` 后按 ID 调 `Unregister`，其间同 ID 以另一个状态重新登记时注销的是新登记（批次 10 报告，修前即有、RR-22 未扩大） | [bf-RR-20260927-22](../bugfix/RR-20260927-22.md) | 推断，未写探针 | 在比对与 `Unregister` 之间加测试缝构造交错；证实则改为在 `subj.mu` 内比对并按实例注销 | S |
| B42 | `entity.unload_resync.backlog` 为无标签 gauge，一个进程有多个 ManagerAccess 时互相覆盖（audit6 推断） | audit6 | 推断 | 构造两个 ManagerAccess 各有积压，看 gauge 值；证实则按实例聚合或文档写明“只反映最近写入者” | S |
| B43 | `revokedCreated` 按 Guard 记而 `removing` 按 EntityManager 记，一个 Guard 跨多个 Manager 同 ID 时可能误判（audit6 推断） | audit6 | 推断 | 两个 Manager 同 ID，一边撤销、另一边新建，看 RR-21 分支是否误判 | S |
| B44 | B08 回归 `TestParallelWindowReplaysSucceededSuffixUnderNewerFence` 无 race 时约 1/100 超时（`:115 timed out waiting for the suffix publication`）；推测用例未强制两笔落在同一投影窗口 | 批次 7 | 未证明 | 钉住“两笔进入同一窗口”再复跑 `-count=1000`；是用例问题就修测试 | S |
| B45 | 集成测试残留：`kit/nats` `TestToxicJetStreamRPCCall…` 每次在共享 NATS 留两条流；只认 `REDIS_ADDR` 的三个包在目标 Redis 留 118 个键（部分无 TTL） | 批次 7 | 已观察 | 用例 `t.Cleanup` 删除自己创建的流 / 键 | S |
| B38 | CARRYOVER 里仍未分流的 B4：活动窗口的机器人分支逻辑没有真实跨过 300 秒边界跑过 | [CARRYOVER §B](../bug/CARRYOVER.md)（B4“本轮进展：无”） | 仍成立（之后没有记录） | 集成环境让机器人运行跨过一个完整 300 秒窗口边界，或在 loadtest 里对齐起跑时间 | S |

## C. 本地做不了，或需要维护者决定（33 条）

| 编号 | 一句话 | 来源 | 当前状态 | 缺什么 / 建议 | 规模 |
| --- | --- | --- | --- | --- | --- |
| C01 | 24 小时长稳从未运行；30 分钟堆 86.94–307.58MB、后半程基线偏高，事务保留接近 65536 上限，不能宣称排除泄漏 | 交接文档 §5 `:101`；[fix-verification §未做](REVIEW-2026-09-26-fix-verification.md)；bf-37、bf-63 §未验证项 | 已核实仍成立（入口在九项报告“复跑入口”） | 需要一台可独占 24 小时的机器与维护者安排的窗口 | L |
| C02 | 真实跨机 / 弱网、生产客户端与业务 schema 未验证 | 交接文档 §5 `:102`；bf-15、bf-40、bf-52、bf-55 §未验证项 | 仍成立 | 需要部署环境与真实网关 / 客户端 | L |
| C03 | Linux fsync 语义与物理断电未实测：RR-16 的额外 fsync 只有 macOS `F_FULLFSYNC` 数据；RR-33 只注入返回值；RR-41 没在 ext4 `data=writeback`、网络盘、VM 快照上断电 | [triage §性能](REVIEW-2026-09-26-v1170-triage.md)；[bug-16](../bug/RR-20260926-16.md)、[bf-16 §兼容与边界](../bugfix/RR-20260926-16.md)；[bf-33 §6](../bugfix/RR-20260926-33.md)；[bf-41 §6](../bugfix/RR-20260926-41.md)；[bf-76](../bugfix/RR-20260926-76.md) | 仍成立 | 需要 Linux 机器与断电 / dm-flakey 类故障注入 | L |
| C04 | k8s / compose / systemd 实际滚动停机没执行；仓库外的生产配置 doctor 读不到 | [bf-51](../bugfix/RR-20260926-51.md)、[bf-66 §未验证项、§复核补修](../bugfix/RR-20260926-66.md)、[bf-80 §未验证项 / 风险](../bugfix/RR-20260926-80.md) | 仍成立 | 需要集群环境；生产配置位置由部署方提供 | M |
| C05 | **player TCP Mod（accessplayertcp）不声明停机预算**，只拿约 3s 保底（game 服务约 3.7s），低于自身 `shutdown_timeout: 10s`；保底 3s 与 `nest.request_timeout` 3s 也没有余量 | [bf-66 §未验证项](../bugfix/RR-20260926-66.md)（“是否让它声明预算属另一项行为变化，未做”）；[audit2 §疑点](REVIEW-2026-09-27-audit2.md)（RR-51 项） | 已核实仍成立：生成器 `codegen/internal/roost/render_player_tcp.go:204`、`:229`、`:362` 只有 `ShutdownTimeout`，没有 `StopBudget()`；全仓唯一实现是 `kit/dataengine/mod.go:292`；`app/stop_budget_generated_test.go:14` 注释写明它只得约 3s | 维护者定：让它声明 `StopBudget() = ShutdownTimeout`（生成公式与 doctor 同步计入）还是接受。决定后本地改是 S～M | S |
| C06 | RR-74：嵌套独立事务用导出的 `AddMutation` 直写原始 mutation、不经 DAO，就能绕过“外层已快照实体不能写”的拒绝，外层回滚后内存与持久分叉 | [bug-74 §复核残留](../bug/RR-20260926-74.md)；[bf-74 §未验证项 / 风险](../bugfix/RR-20260926-74.md) | 已核实仍成立：`nest/rollback.go:297`（导出）、`:618-632`（只看 DAO 参与方）；仓内生产调用只有 `nest/msg.go:57`、`nest/persist_change.go:304` | 维护者定：扩大到按 EntityID 匹配原始 mutation，或在文档声明不支持 | S |
| C07 | RR-76：fence 之后是否还允许外层 handler 自己的提交交给 committer | [bug-76 §复核残留](../bug/RR-20260926-76.md)；[bf-76 §未验证项 / 风险](../bugfix/RR-20260926-76.md) | 已核实仍成立（见 B03） | 先做 B03，再由维护者定是否在提交路径检查 fence | S |
| C08 | RR-41 跨页半写回要不要放宽判据（例如坏帧在 fence 之后、帧体从某个 4 KiB 边界起到文件末尾全零时也截断） | 同 B02 | 仍成立：按 `NEST_TRANSACTION_WAL.md` §5 第 3 条“帧头有效但 CRC 不符”拒绝 | 语义选择：放宽有吞掉真实 CRC 错误的风险 | M |
| C09 | RR-48 重排延迟要不要加抖动 | 同 B24 | 仍成立：固定 5ms | 视 B24 结果由维护者定 | S |
| C10 | 导出的 `entity.ResetEntityRegistryForTest` 仓内已无调用方；它清空整张表，再用会重现 RR-83 | [bf-83 §未验证项 / 风险](../bugfix/RR-20260926-83.md) | 已核实仍成立：定义在 `entity/entity_factory.go:229-236`；`rg` 只命中测试注释 `entity/remote_view_test.go:7`、`entity/registry_fixture_test.go:6` | 删除导出 API（破坏性）或加弃用注释，维护者定 | S |
| C11 | core 的 Remote L2 快照键 `remote_entity:snapshot:*` 不带部署前缀，同一 Redis db 多部署共用 | [bf-56 §未验证项与观察](../bugfix/RR-20260926-56.md) | 已核实仍成立：`remoteentity/snapshot_l2.go:199` | 改键格式有兼容 / 迁移影响，维护者定 | M |
| C12 | 生成 DAO 的库名写死 `"game"`（DAO 与 demo 发号器集合） | [release §疑点](REVIEW-2026-09-26-release.md)、[fix-verification §疑点](REVIEW-2026-09-26-fix-verification.md)、[bf-56 §执行记录](../bugfix/RR-20260926-56.md) | 已核实仍成立（子报告核对）：`codegen/internal/dao/gen.go:333` 生成 `DaoDBName`；golden `codegen/internal/dao/testdata/golden/gen_hero_dao.go:37` | 要不要可配置由维护者定 | M |
| C13 | 可调参数没接到 kit 配置：`UnloadResyncConfig`（Workers / Attempts / 队列）、`ConfigureLoadTimeout`（共享加载 30s） | [bf-59 §未验证项与边界](../bugfix/RR-20260926-59.md)、[bf-54 §兼容性 / 默认值变化](../bugfix/RR-20260926-54.md) | 已核实仍成立：`kit/nest/nest_mod.go:175` 传零值 `entity.UnloadResyncConfig{}`；`entity/manager_access.go:40`、`:76`，`ConfigureLoadTimeout` 无生产调用方 | 是否暴露配置项，维护者定 | S |
| C14 | RR-59 重载风暴的最坏延迟：修复记录说“有界但可能是分钟级”，audit2 算出 4096 实体约 43 小时上界 | [audit2 §疑点](REVIEW-2026-09-27-audit2.md)；[bf-59 §对抗性自查](../bugfix/RR-20260926-59.md) | 仍成立：`entity/unload_resync.go:49-57`（默认 Workers=4、Attempts=5） | 维护者定：收紧（缩短退避 / 失败快速退回 remove）还是只把文档改成真实上界 | S |
| C15 | RR-45：`entity.ValidateEntityRegistry` 不校验 DAO scope，“可作为后续加固另议” | [bf-45 §决策](../bugfix/RR-20260926-45.md) | 已核实仍成立：`entity/category_order.go:197` 无 scope 检查 | 与 A10 一起定 | S |
| C16 | RR-71 实现方追加的决定：“builder 先注册、kind 后声明托管”现在启动期 panic，不在维护者约束里 | [bf-71 §未验证项 / 风险](../bugfix/RR-20260926-71.md) | 已核实仍成立：`entity/entity_factory.go:195`（`declared after its builder`） | 维护者确认这个拒绝 | S |
| C17 | `RegisterEntityKindDefs` 批量注册中途出错不回退已写入的定义 | [audit3 §疑点](REVIEW-2026-09-27-audit3.md)（既有） | 已核实仍成立：`entity/entity_factory.go:115-124` 逐条写入，出错直接返回 | 要不要保证原子（`Must*` 版本 panic 后通常进程退出），维护者定 | S |
| C18 | RR-40/55：resync 重开会话重试用尽（例如会话数到上限）后只记 Warn，玩家留在场景但自身视图要等下次登录 | [bf-40 §未验证项与已知边界](../bugfix/RR-20260926-40.md)、[bf-55 §兼容性与行为变化](../bugfix/RR-20260926-55.md) | 仍成立：`demo/internal/service/game/scene.go.tmpl:331`、`:664` | 语义选择：继续重试、踢下线还是接受 | S |
| C19 | loader 不支持卸载时，混合事务里 Remote 被拒，整笔门保持冻结，连同一事务的本地实体也冻结（和 RR-58 “只丢 Remote 事实”的目标有张力） | [bf-58 §兼容性](../bugfix/RR-20260926-58.md)、[bf-37 §决策第 4 条](../bugfix/RR-20260926-37.md) | 已核实仍成立：`remoteentity/transaction_manager.go:359-364`（`rejected_unload_unsupported_total`） | 维护者判断是否接受 | S |
| C20 | Direct：RR-70 交还不带会话 lifetime，同 ID 关闭重开的微秒窗口里重新提交可能落到新连接；Direct 在生产代码没有调用方，也没有生成工程端到端场景 | [bf-79 §未验证项 / 风险](../bugfix/RR-20260926-79.md)、[bf-85 §未验证项 / 风险](../bugfix/RR-20260926-85.md)、[audit4 §疑点](REVIEW-2026-09-27-audit4.md)（判“不构成新缺陷”） | 仍成立：`NewDirect` 非测试调用只有定义 `sync/entitysync/policy/direct.go:40` | 等 Direct 有真实使用方时再定 | S |
| C21 | “重连恢复”的定义：旧会话先 Close 再 Open 的订阅全部算新入场，只有同 lifetime 的 Hold→Ready 算恢复 | [core-optimization §疑点](REVIEW-2026-09-26-core-optimization.md)（“需业务确认”）；[followup §判断与处理](REVIEW-2026-09-26-followup.md)（“当前语义如此，不擅自新增”） | 仍成立 | 需要业务方确认；跨会话恢复需要新协议 | M |
| C22 | periodic 只配字节预算时仍会捕获全部候选（编码前不知道精确大小）；跨 tick 内容缓存需要版本与内存预算设计 | [core-optimization §疑点](REVIEW-2026-09-26-core-optimization.md)；[followup §判断与处理](REVIEW-2026-09-26-followup.md)；交接文档 §5 `:104`；[bf-04 首段](../bugfix/RR-20260926-04.md) | 仍成立 | 设计题，是否做由维护者定 | L |
| C23 | 历史 FlushFailures=1 的根因没证明，只修了“失败原因要留下” | 交接文档 §5 `:103`；[bug-01](../bug/RR-20260926-01.md) | 仍成立（原 main 4 次复跑都是 0） | 历史样本无法重现；只能等再次出现时看 LastError | S |
| C24 | 跨进程所有权迁移：RR-30 契约点 4（WAL 没排空就迁走所有权）没纳入框架，仍靠 demo 的 `Admit` 与 RR-31 约定；两进程端到端没做 | [bf-30 §6](../bugfix/RR-20260926-30.md)、[bf-31 §未验证项](../bugfix/RR-20260926-31.md)、[audit §记录但不修](REVIEW-2026-09-26-audit.md)；CARRYOVER A6/A7 已转 feature 线 | 仍成立 | 需要移交协议设计（feature 线） | L |
| C25 | saga 步骤收件箱 `reserveInTransaction` 用 `_ = inbox.markCompleted(...)` 吞掉写错误；将来若出现确定性失败会重跑到超时 | [bf-34 §对抗性自查](../bugfix/RR-20260926-34.md)（作者判定不是缺陷，“供 review 参考”） | 已核实仍成立（子报告）：`saga/dataengine_step_inbox.go:177` | 由 review 拍板是否改成返回错误 | S |
| C26 | RR-35：`CreateInScope` 捕获失败而 handler 吞掉错误继续提交时，已登记的提交参与者可能仍会准备记录 | [bf-35 §未验证项 / 风险](../bugfix/RR-20260926-35.md) | 仍成立：`entity/manager_factory.go:101-106` | 语义选择：是否强制整笔失败（与 Cast 捕获失败同类） | S |
| C27 | 撤销新建实体（RR-35 `revokeCreated`）仍调 `OnDestroy(DestroyReasonCommon)`，而 RR-30/39 驱逐已改用 `DestroyReasonMemoryUnload`，两处不一致 | [bf-35 §兼容性与新导出 API](../bugfix/RR-20260926-35.md)、[bf-30 §3](../bugfix/RR-20260926-30.md) | 已核实仍成立（子报告）：`entity/manager_factory.go:175-176` vs `entity/entity_kind.go:48` | 要不要为“撤销新建”单独给 reason，维护者定 | S |
| C28 | RR-15：没有“旧队列已排空”的通知，复用 SessionID 的重试节奏由调用方自己决定 | [bf-15 §方案、§未验证项与边界](../bugfix/RR-20260926-15.md) | 仍成立：`sync/entitysync/errors.go:63` 只有 `SessionOpenRetryable` | 是否加通知，维护者定 | S |
| C29 | RR-05：非冷创建会话在 RetryLater 下仍按会话 ID 顺序，低 ID 先尝试 | [bf-05 §未验证项与边界](../bugfix/RR-20260926-05.md) | 仍成立：`sync/entitysync/snapshot_budget.go:351-353` | 公平性语义待定 | S |
| C30 | RR-06：`Nest.Request*` 保留 `ErrSyncInHandler`、不改 panic，主记录要求“由维护者决定并在 CHANGELOG 标注”，没找到维护者确认的记载 | [bug-06 §实施方向](../bug/RR-20260926-06.md)；[bf-06](../bugfix/RR-20260926-06.md)`:11` | 仍成立：`nest/client.go:74`、`:114` 返回 `ErrSyncInHandler`（`nest/nest.go:35`） | 维护者补一句确认即可关闭 | S |
| C31 | RR-66：手写 Mod（生成器不知道的）实现 `StopBudget` 也不计入生成值；新测试注册 kind 没有占用清单（RR-83） | [bf-66 §未验证项](../bugfix/RR-20260926-66.md)、[bf-83 §未验证项 / 风险](../bugfix/RR-20260926-83.md) | 仍成立：`codegen/internal/roost/shutdown_budget.go:99` 只看 Manifest | 流程约定，维护者定是否做清单 / 扫描测试 | S |
| C32 | triage 留的“残留加固”：同 fence 下拒绝 StateVersion 回退（只影响非 authority 兼容装配） | [triage §核实后判为非问题](REVIEW-2026-09-26-v1170-triage.md)（RR-11 项） | 未见后续处理 | 维护者定是否做 | S |
| C34 | B22 后一半：remoteflow 真实持久拒绝 + 订阅者。当前正式链路新写入可达的持久拒绝只有 Durability 0 结果未知（finalizer `RejectUnresolvedRemoteCommits`，B12 已验证）；要让提交“没到达 Mongo”需适配器替身或 Mongo 故障注入，且 remoteflow 的 `vaultLoader` 不支持卸载，需换正式 `ManagerAccess` + entitysync——fixture 设计量级 | [bf-59](../bugfix/RR-20260926-59.md) 更正节 | 保留：需要 fixture 设计 | M |
| C33 | codebase-memory 图谱没刷新，刷新被 “pre-coordination or unverified CBM generation is active” 阻止 | 交接文档 §5 `:114`；多份 bf（51/54/66/76～85）| 仍成立 | 需确认没有其他 CBM 实例在用，再按记忆里的锁处理步骤重建；不要清未知锁 | S |

## D. 已接受的边界 / 契约（不处理，39 条）

| 编号 | 边界 | 依据 | 源码 / 文档位置 |
| --- | --- | --- | --- |
| D01 | Sync 集中恢复 8 次中 2 次原 50ms 门禁失败（28 条计划超标，6 条实际超标） | 交接文档 §4 `:88`：2026-09-26 用户明确“这个指标目前可以接受”；不外推 | 原门禁继续失败 |
| D02 | RR-41：已 fsync、strict 已确认但未 ack 的记录，若所在块及其后被介质整块清零，会被当零尾截断、静默丢失 | [bug-41 §修复约束](../bug/RR-20260926-41.md) 要求写入文档；`NEST_TRANSACTION_WAL.md:112-113` 残余风险段 | `nestwal/wal.go:1093`、`:1119-1120`（指标 `tail_truncated{reason=zero_fill}`）、`:1129` |
| D03 | RR-41：零页之后还有非零字节（含中间零页后仍有完整帧）拒绝启动，需人工处置 | [audit §记录但不修](REVIEW-2026-09-26-audit.md)；`NEST_TRANSACTION_WAL.md` §5 第 3 条；有测试 `TestOpenRefusesTailThatIsNotAllZero` | 跨页半写回子形态见 B02 / C08 |
| D04 | saga：实体屏障期间的重投会消耗 JetStream `MaxDeliver`，耗尽后 `Term`，靠 coordinator `Timeout` / `MaxAttempts` 兜底；默认 25000 次、约 8 天 | [bf-63 §决策 6](../bugfix/RR-20260926-63.md)；`SAGA.md:163-174` | `kit/saga/mod.go:86`、`:102`、`:113`；`saga/nest_completion_consumer.go:65-66` |
| D05 | RR-54：包装 `ManagerAccess` 却不转发 `BindLocalExecutor` 的自定义 Getter，领头方离开后的发布在加载 goroutine 上就地执行（正式装配不受影响） | [bf-54 §兼容性 / 默认值变化](../bugfix/RR-20260926-54.md) `:57-58`；[audit2 §疑点](REVIEW-2026-09-27-audit2.md)“写入契约文档” | `nest/nest.go:434-438`（只做类型断言）；`entity/manager_access.go:366-369`。注意：契约只在修复记录，`docs/USER_GUIDE.md:243-244` 没写（并入 A13） |
| D06 | RR-37/61：持久结论到达时 Nest 已停机或已 fence，AfterCommit 不执行、Sync 门保持冻结，只计数告警（“至多一次”） | 维护者 2026-09-26 批准“不离池执行、计数告警”（[audit §新登记](REVIEW-2026-09-26-audit.md)）；USER_GUIDE `:290` | `remoteentity/transaction_manager.go:378-389`（`deferred_outcome_not_run_total`） |
| D07 | RR-30：屏障只看写集合，只读含被跳过效果的实体再写别的实体的事务不在屏障内 | [audit §记录但不修](REVIEW-2026-09-26-audit.md)；[bf-30 §6](../bugfix/RR-20260926-30.md) | - |
| D08 | 同 ID 并发创建在快池等本地锁（Guard/本地锁豁免） | [audit §记录但不修](REVIEW-2026-09-26-audit.md) | 越锁序时已改为 TryRequire 冲突（RR-48/64，`entity/entity_guard.go:329-337`），这条的适用面已缩小 |
| D09 | RR-75：“嵌套事务触及 Remote 实体”这一支仍返回旧哨兵 `ErrDurableRemoteWriteUnsupported`，语义同为拒绝 | [bug-75 §复核残留](../bug/RR-20260926-75.md)；bf-75 修复节写明该路径不变 | `nest/rollback.go:822`；有批次时 `nest/execution.go:122` 返回新哨兵 |
| D10 | RR-80：prod / secret 示例配置解析失败只 WARN、不 FAIL（dev 配置仍 FAIL） | [bf-80 §复核后的补修](../bugfix/RR-20260926-80.md)；[bug-80 §复核残留](../bug/RR-20260926-80.md) | `codegen/internal/roost/shutdown_budget.go:418-446` |
| D11 | RR-77：`project sync` 的停机公式按常量 30s 计 dataengine，不读配置 | RR-77 约束只要求 DEPLOYMENT 如实描述 | `shutdown_budget.go:41`、`:106`（doctor 建议值问题另见 A09） |
| D12 | RR-70/78/79：撤销记录在实体永不回来、政策一直持有 pair 时一直保留；会话关闭不删撤销记录；`Manager.Close` 不通知政策；停止未 Close 时释放通知排队不交付 | [audit4 §疑点](REVIEW-2026-09-27-audit4.md)“RR-70 已接受的边界”；bf-70、bf-78、bf-79 | `sync/entitysync/policy_queue.go:317`、`:415-421`；`manager.go:739`、`:789` |
| D13 | 各处有界重试上限：GetOrCreate 3 轮后仍可能 `ErrEntityRemoved`（Nest 外）；组迁移重排 400 次；场景重开退避 8 次 / 单次 1s；共享加载 30s | bf-57、bf-81、bf-48、bf-55、bf-54 的约束“有界” | `codegen/internal/roost/add_workflow.go:154`；`nest/group_transition.go:18`；`demo/.../scene.go.tmpl:62-68`；`entity/manager_access.go:40` |
| D14 | Sync 冷预算按“尝试成本”计费、不逐帧退款；字节软预算允许窗口首个合法大对象独占；常数次多余捕获（固定 2 次） | [followup §决策边界](REVIEW-2026-09-26-followup.md)；bug-04/05 复核补修；bf-04/05 | `sync/entitysync/snapshot_budget.go:373-374` 注释 |
| D15 | 快池保护不是全局 I/O hook：内存命中、Guard/本地锁、回滚与 Finalize、锁内 WAL 准入（含 pipelined 阶段一等 group-commit）是明确豁免；用户自写阻塞 RPC 不拦截 | [followup §决策边界](REVIEW-2026-09-26-followup.md)；[triage](REVIEW-2026-09-26-v1170-triage.md)“已写入 roost-coding 豁免清单” | `docs/agent-skills/roost-coding/SKILL.md` |
| D16 | v1.17.0 triage 判为非问题：RR-11 同 fence 窗口回退、Remote 停机按 NextVersion 解锁、kit/nest 停机超时 entitySync 不收尾、重启积压不计 `WALUnacked`、WAL terminal 后 Shutdown 恒报错（fsyncgate）、CommitSystem 关闭孤儿票据、RR-08 已入队续行不计准入、无 Guard scope 时 `GetEntityGuard` 不归还 | [triage §核实后判为非问题](REVIEW-2026-09-26-v1170-triage.md) | RR-08 说明已写 `nest/dispatch_queue.go:173` |
| D17 | Remote 结果语义：收到释放错误 / hook 错误 / 已持久提交仍返回错误，都不等于未提交，调用方不能据此重复业务；截止只停止等待、不撤销已准入业务 | bf-13、bf-14、bf-32、bf-36；USER_GUIDE `:269` | `nest/msg.go:90`；`nest/execution.go:304` |
| D18 | RR-19：生成 Entity 的 `RollbackRemoteCommit` 是 no-op，不支持投影阶段事后撤销；自定义 remoteStore 回放历史混合记录必须实现 `RejectRemoteCommitsInTransaction`；原生 Saga 步骤不能改 Remote | [release-fixes §最终行为](REVIEW-2026-09-26-release-fixes.md)；bf-19；`SAGA.md:107-116` | `codegen/internal/entity/gen.go:653-655` |
| D19 | RR-38：投影器停滞期间 gate 与写额度保持占用到 `FinalizeProjectionTimeout`（默认 30s）才回源；新配置键没进 codegen 配置样例（零值取默认） | bf-38 维护者批准的决策 1 | `remoteentity/config.go:69`；`kit/remoteentity/remote_entity_mod.go:106` |
| D20 | loader 不支持卸载时退回旧行为（实例一直隔离、裸 `ErrRemoteFenced`）；停机后已登记的旧实例在本进程剩余时间一直返回“可重试”哨兵 | bf-37 决策 4、bf-39、bf-58、bf-62；USER_GUIDE `:289` | `remoteentity/wrapper.go:23-26`、`:117-139`；`entity/remote_protocol.go:27` |
| D21 | 自定义 Getter 契约：没实现 `LoadedChecker` 的退回 RR-25 之前（冷目标 `ErrColdLoadInLogic`，要显式 Slow）；`IsLoaded` 内做 I/O 框架拦不住；冷缺失要返回错误不 panic | 维护者批准（bf-47、bf-26） | `entity/entity.go:63`；`nest/slow_load.go:114-120`；`entity/manager_access.go:233` |
| D22 | 生成 TCP 行为：写失败即断开；写前拒绝（ctx 结束、超限）不关连接；慢连接让该次推送等满 `write_timeout`；调用方截止短于 `write_timeout` 的推送不会关停读连接（靠 90s idle）；只改了 TCP | RR-52/68 约束；[audit3 §疑点](REVIEW-2026-09-27-audit3.md)；CHANGELOG `:18` | `codegen/internal/roost/render_player_tcp.go:180`、`:849`、`:865`、`:1005-1029` |
| D23 | RR-56：写了非默认 prefix、没写 `stream` 的旧部署升级后换流，旧流仍占 subjects 时启动失败 | 维护者兼容约束；`CHANGELOG.md:14-17` 迁移说明 | `sync/syncbus/driver/jetstream.go:301` |
| D24 | RR-33：terminal 后 `Replay` 仍按 `w.offset` 读，可能投影未证明落盘的记录；重复 Ack 快速返回慢约 8ns | `NEST_TRANSACTION_WAL.md` §7；维护者约束“checkpointMu 内先查 terminal” | `nestwal/wal.go:488`、`:535-551` |
| D25 | RR-50：驱逐 worker 在 WAL terminal 后照旧重试到成功或关闭 | bf-50 设计取舍 | `dataengine/engine/fenced_step.go:153-180` |
| D26 | 停机预算规则：剩余时间给不起保底时均分并告警；配置缺 `total_timeout` 运行时兜底 30s；旧 60s 格式配置 `project sync` 不改写、doctor 持续 WARN；每个 Mod 用满预算时共享段不再有保底 | RR-42/51/66 维护者约束 | `app/app.go:313-316`、`:460-464`、`:571-575`；`shutdown_budget.go:256`、`:381` |
| D27 | 已生成工程要重新生成 / `project sync` 才能拿到新模板（GetOrCreate 重试形状、TCP 行为、停机值） | bf-52、bf-57、bf-66、bf-68 | `codegen/internal/roost/add_workflow.go:154` 等 |
| D28 | 错误文本多了哨兵前缀，按文本匹配的调用方要改用 `errors.Is` | bf-46、bf-53、bf-62、bf-64、bf-65、bf-73 | `nest/nest.go:77-99`；`nest/cast.go:177` |
| D29 | 公会发号创建失败允许空号、不回收；v1.16.1 demo 业务文件按“重生成 / 人工合并”契约升级 | bf-22、bf-24；CHANGELOG `:99`、`:141` | - |
| D30 | RR-45：生成期判定不了 DAO scope 时放行，交给装配期；已有 `dbscope=sid` 托管实体的项目 generate / 装配失败，已写入 `db_<sid>` 的数据业务自行迁移 | 维护者批准禁止该组合 | `codegen/internal/entity/remote_dao_scope.go:20`；`remoteentity/assemble.go:60`、`:74` |
| D31 | 测试夹具：`isolateEntityRegistry` 只适用非并行用例；dataengine/engine 两处用相同定义注册 kind 235（幂等） | bf-83、bf-82 | `entity/registry_fixture_test.go:15-17`；`dataengine/engine/mongo_store_test.go:570`、`projector_remote_test.go:30` |
| D32 | RR-84：收尾阶段已释放实体锁，“调用方持有全部锁”的约定框架不检查；纯本地消息按“不认领”处理 | RR-84 约束允许二选一 | `nest/execution.go:112-116` |
| D33 | 不遵守 ctx 的自定义 handler（阻塞 RPC）不受截止与快池保护 | roost-coding `SKILL.md:36`；bf-06、bf-36 | - |
| D34 | 空 ID Remote 批次在停机时多归还一个写额度：正式准备路径提前返回，不可达 | [followup §判断与处理](REVIEW-2026-09-26-followup.md) | - |
| D35 | 生命周期 API 收紧：自定义 RecoveryGate 要提供 `WaitEntityProjection`；关闭超时不表示资源已释放、要再次 Close；复用 SessionID 要等旧发送退出后重试；OnFatal 改异步 | bf-10、bf-15、bf-17；`CHANGELOG.md:89-96` | `dataengine/engine/projector.go:416` |
| D36 | RR-34：驱动按错误链里第一个带标签的错误决定是否重试；outbox 投递后复用同一 effect ID 不再识别为身份冲突（修前即有窗口）；同一 saga start 两事务各发一次判 fatal | bf-34 / bug-34 | `mongo/driver/collection.go:360-371`；`dataengine/engine/mongo_store.go:425-428` |
| D37 | `app.ValidateServiceConfig` 不校验 `syncbus.*` 数值，校验放在 kit Mod | bf-12“未采用”段 | `app/config_validation.go:12` 起 |
| D38 | audit2 疑点里判为边界的两条：RR-59 无 loader 时当作“权威没有”（kit 装配不可达）；RR-62 业务自身删除 Remote 实体的并发窗口也拿到“重载中”标签 | bf-59 §兼容性、bf-62 §兼容性（“窗口二也覆盖业务 Destroy 进行中”） | - |
| D39 | RR-73：memory handler 开始执行后，锁超时 / 组迁移错误不再自动重排，回复 `ErrNonRollbackNotRequeued`；Cast 等锁期间目标被摘除改报 `ErrEntityNotFound` | 维护者 2026-09-27 约束（沿用 RR-64 “返回错误，不重排”）；[audit4 §疑点](REVIEW-2026-09-27-audit4.md) | `nest/msg.go:236`、`:247`；`nest/cast.go:166`、`:177` |

---

## E. 已经不成立、可以关闭的旧条目

| 旧条目 | 来源 | 可关闭理由 |
| --- | --- | --- |
| W-2026-09-22-02（持久化公会复用进程临时 ID） | [WANTED](../bug/WANTED.md) `:83-85` | 正文“当前结论（2026-09-24）”已写发号根因与冲突分类都修了，只是标题没按惯例标“分流结论 / 已处理”。建议补标题 |
| CARRYOVER B1、B3、B5 | [CARRYOVER §B](../bug/CARRYOVER.md) | 09-22 已跑：B1 → RR-20260922-02，B5 → RR-20260922-01，B3 正向已跑通（只剩 RR-20260921-03 的窄窗口单测探针） |
| CARRYOVER B2（迁移前后没有同机性能对比） | 同上 | 建议关闭：合仓搬迁已过三周，之后九项报告、v1.17.0 triage 等已在当前代码上做过多轮同机性能记录，“搬迁前后”对比已无决策价值。需维护者确认 |
| 交接文档 §5 “故障矩阵有版本边界……最新九项版本未完整重跑” | 交接文档 `:102` | `f1d7591` 上 `scripts/test-remote-matrix.sh` 21/21 PASS（`:110`）。跨机 / 弱网那半句仍在 C02 |
| 交接文档 §5 RR-10～24“登记、未修复”及发版手续（清单 v1.16.1、生成器下限、CHANGELOG） | 交接文档 `:105`；[release §发版手续](REVIEW-2026-09-26-release.md)；[fix-verification §门禁](REVIEW-2026-09-26-fix-verification.md) | 全部已修；`8973a78` 已把清单与生成器下限升到 v1.17.1。RR-21 的性能口径那一半保留为 B30 |
| core-optimization 复审的疑点与文档不一致 | [core-optimization](REVIEW-2026-09-26-core-optimization.md) | 事务 ID / Replayed 漏计 / 队列口径已登记为 RR-07/08/09 并修复；Broadcast 冷目标已逐目标记日志（`nest/nest_dispatch.go:491`）；`sync/README.md:78-80` 已改为“增量计数、完整遍历在 AuditStats”；`docs/bugfix/RR-20260925-05.md:3` 与 `REFACTOR-2026-09-25-remote-throughput.md:3` 已加“后续实现变更”说明；`projector_remote_test.go` 已没有 `"workers"` 子用例名。剩余两条（Close 后重连、periodic 重复捕获）在 C21/C22 |
| release 复审、fix-verification 的“疑点（未登记）” | [release](REVIEW-2026-09-26-release.md)、[fix-verification](REVIEW-2026-09-26-fix-verification.md) | 已由 [triage](REVIEW-2026-09-26-v1170-triage.md) 逐条处理：登记 RR-33～47（均已修）或判非问题（D16）。只剩“生成 DAO 库名写死 game”（C12）、RR-16 Linux（C03） |
| fix-verification “证据只在本机 /tmp” | [fix-verification](REVIEW-2026-09-26-fix-verification.md) | 已收进 [EVIDENCE-2026-09-26-985d5ba-negative.md](EVIDENCE-2026-09-26-985d5ba-negative.md)；弱负对照由 RR-17/19/22 复核补修加强。bf-02 的同类问题仍在 A12 |
| audit2 “须随发布补的文档” | [audit2](REVIEW-2026-09-27-audit2.md) | CHANGELOG 已写 RR-56 迁移与 RR-52 行为（`CHANGELOG.md:14-18`）；只剩 kit/README 那句（A11） |
| audit2 疑点“`unsubscribe` 中 defer 顺序若走到会自锁” | 同上 | 已改为解锁后再 `forget`：`sync/entitysync/subscriptions.go:423-431`（RR-69） |
| RR-40 两条观察项（同 tick Leave→Join 得 `ErrSubjectRetiring`；`ROOST_SYNC` 流名不随前缀隔离），RR-36 同一观察项 | bf-40 §未验证项与已知边界 / §执行记录；bf-36 §执行记录 | 分别由 RR-55（`demo/.../scene.go.tmpl:447-449`）和 RR-56（`sync/syncbus/driver/jetstream.go:22,35,293-296`）修掉 |
| RR-42 两个待维护者确认项（未声明 Mod 按 5s 缩放；是否上调生成默认 `total_timeout`） | bf-42 §决策 | 被 RR-51（保底 3s、不参与缩放，`app/app.go:460-464`）和 RR-66（按每个服务的 Mod 计算）取代 |
| RR-60 同类两处；RR-64/65 的三条残留；RR-66“减 Mod 要等下次 sync”；RR-75 收尾窗口；RR-70 二次撤销 | bf-60、bf-63、bf-64、bf-65、bf-66、bf-75、bf-70 | 分别由 RR-71、RR-73/74/75/76、RR-80、RR-84、RR-78 修复（子报告逐条核对源码） |
| 包测试不能重复运行（bf-71、bf-74、bf-81、bf-82 都提到） | 同左 | RR-82/83 已修；本次 `go test -count=2 ./...` 除本地未跟踪 artifacts 外全部通过。nest 单用例重复运行问题另见 A07 |
| `TestHandlerCreateCrossOrderResolvesWithoutDeadlock` 偶发失败 | bf-73、bf-75 | RR-81 已修；子任务实跑 `-race -count=300` 通过 |
| `entity/entity_factory_test.go` 未 gofmt | bf-63、bf-70 | 已格式化（`gofmt -l entity/` 为空） |
| CHANGELOG 未写（RR-15/17/29/37/61） | 各 bf 兼容性节 | 已补：`CHANGELOG.md:29-30`、`:86-106` |
| CI 步骤只能在 CI 跑（RR-12 日志断言、RR-22 真实 Mongo 并发发号） | bf-12、bf-22 §未验证项 | framework-compat run `36306850395`（`ffcf902`）9 个 job 全部 success，相关步骤实跑 |
| RR-23 “Windows 实机未验证” | bf-23 | CI windows-compatibility 实跑，RR-23 点名的包都通过；当前唯一失败是 A01 |
| RR-15 demo `OpenHeldSession` 出错无重试 | bf-15 §未验证项与边界 | `demo/internal/service/game/scene.go.tmpl:331-335` 已按 `SessionOpenRetryable` 有界退避（RR-55） |
| RR-25 “自定义 Getter 不遵守 LoadedEntitiesOnly 时准入判定在调用方 goroutine 做 I/O” | bf-25 | RR-47 改为只用 `entity.LoadedChecker`（`nest/slow_load.go:114-120`），不再调 Get |
| RR-30 “SKILL.md 那句由维护者定”“驱逐用 `DestroyReasonCommon`”“重载前不发 remove” | bf-30 §3/§6 | `docs/agent-skills/roost-coding/SKILL.md:70` 已改；驱逐走 `ManagerAccess.Unload`，reason 为 `DestroyReasonMemoryUnload`（`dataengine/engine/fenced_step.go:228`）；RR-59 已加主动重载 / 退回 remove |
| RR-11 “测试夹具 dirty 非原子” | bf-11 §兼容与未验证项 | RR-63 已加锁（`remoteentity/manager_test.go:180-199`）；残留一句过时注释并入 A12 |
| RR-39 “业务分不清 `ErrRemoteFenced` 与真 fence” | bug-39 §根因 | RR-62 新增可重试哨兵 `ErrRemoteEntityReloading`（`entity/remote_protocol.go:27`） |
| RR-49 “真实可达路径未确认” | bug-49 等级注 | 修复节已推翻：纯本地 strict 提交后 release hook 抛锁超时同样重复执行 401 次 |
| RR-63 “本机 `~/.codex` skill 副本需重新同步” | bf-63 §未验证项 | 子任务 `diff -q` 与仓库版本一致 |
| RR-28 “finalizer 回滚不在快池” | bf-28 | kit 装配下已不成立的可能性很大（见 B26），核实后可关 |

---

## F. 已知事实逐条核实结果

| 事实 | 结论 | 归档 |
| --- | --- | --- |
| 仓库根有被跟踪的 `glsvet` 二进制 | 成立：`git ls-files` 命中；`05f109a`（v6.4，2026-08-26）加入；Mach-O arm64，3,020,386 字节 | A05 |
| demo `run.sh` 忽略 `redis.db`；demo Redis 键不带 prefix | 成立：`render_dev_run.go:122-132`；`collaborators.go.tmpl:54`；core 快照键 `snapshot_l2.go:199` 同样不带 | A03、A04、C11 |
| accessplayertcp 没有声明停机预算 | 成立：全仓唯一 `StopBudget()` 实现在 `kit/dataengine/mod.go:292` | C05 |
| RR-41 跨页残余 | 成立，分两种：零页后非零（有测试、有契约）；帧体后半页未写回（按契约拒绝，但没有测试） | D03、B02、C08 |
| saga MaxDeliver | 已写成运维契约：`SAGA.md:163-174` | D04 |
| RR-54 包装 getter 离池发布 | 已写成契约，但只在修复记录，USER_GUIDE 未写 | D05、A13 |
| `entity.ResetEntityRegistryForTest` 无调用方 | 成立：`entity/entity_factory.go:229`，只剩测试注释提到 | C10 |
| 其他包是否有“收尾清空全局注册表”的测试 | `rg` 只在 entity 包命中（已改成快照恢复）。同类的全局 reset 还有 `hotcode/admin_test.go:12` 的 `t.Cleanup(ResetForTest)`、nest 大量 `t.Cleanup(ResetHandlersForTest)`、`resetTickCallbacksForTest`，`entity/entity_base.go:619` 的 `ResetComponentRegistryForTest`。整仓 `-count=2` 通过，只有 nest 单用例单独重复会失败 | A07 |
| 未格式化文件 | 成立：指定目录 21 个，全仓 34 个 | A06 |
| `.claude/worktrees/` 残留 agent worktree | 成立，**只报告，未删**：`.claude/worktrees/` 下 19 个（`agent-a069cf2613a7124ac` [fix-r4-7879]、`a0c0954b843be925c`、`a1ef5e007afac476d`、`a2295b4bed066c301`、`a396e1d247911d5f2`、`a406a54aa8b8d56f0`、`a454079b835aebc68` [fix-r4-7377]、`a50f2495d58a86ca5`、`a6c30ced35b575dc3`、`a8703217a211680a4`、`a8d11608db1e826b0`、`a9448ef2e52e534d1`、`abd825be605f660e7`、`ad316be8d6779368c`、`ad5a63b50e0dfe8ee`、`ae50d3b938ed68caa`、`af7f8b529349bc06a`、`af8b3c0e25a16d0af` [fix-r5-81-82]、`afc9658bf9c499b5b` [fix-r4-80，**locked**]），另有 scratchpad 下 `gate-2330d57`（detached） | 维护者决定是否 `git worktree remove` / `prune` |
| RR-83～85 未经独立审计 | 成立（范围实际更大：RR-81～85 以及 RR-73/80 的 audit4 后复核补修） | B36 |

## G. 统计

- A 类 13 条，B 类 38 条，C 类 33 条，D 类 39 条；E 节可关闭 27 组。

## H. 处理计划与进度

规则：A 类按 RR 流程登记 → 先红后绿 → 修复；B 类写探针 / 回归判真伪，确认是缺陷的转为 RR 修复，确认无缺陷的把探针收进仓库当回归；C 类按下表“推荐处理”（维护者确认后执行），标“保留”的写明缺什么；D 类不处理；E 类在原文件里标关闭。
使用本地隔离集成环境的条目串行执行，性能条目最后在无其他负载时跑。

### C 类推荐处理

维护者 2026-09-27 决定：**C 类全部按推荐执行**；长稳与性能对照最后在本机跑（结果注明单机 macOS、有背景负载）；残留 worktree 全部移除。

| 编号 | 推荐 | 本地能否完成 |
| --- | --- | --- |
| C01 | 本机后台跑一次 24 小时长稳（入口见九项报告“复跑入口”），结果如实记为“单机 macOS、有背景负载” | 能（占用 24 小时） |
| C02 | 保留：需要部署环境与真实网关 / 客户端 | 否 |
| C03 | 用 Docker Desktop 的 Linux 容器补 RR-16 fsync 次数与吞吐对照（虚拟化，标注口径）；断电 / dm-flakey 保留 | 部分 |
| C04 | 用本机 docker compose 实跑一次 `stop_grace_period` 停机；k8s / systemd 保留 | 部分 |
| C05 | player TCP Mod 声明 `StopBudget() = ShutdownTimeout`，生成公式与 doctor 同步计入（game 服务 total 相应增加） | 能 |
| C06 | 与 RR-74 同一“拒绝”：嵌套独立事务的原始 mutation 按 EntityID 命中外层已快照实体时同样拒绝 | 能 |
| C07 | 先做 B03；若证实，fence 之后外层自己的提交在交给 committer 之前返回 `ErrNestFenced` | 能 |
| C08 | 不放宽判据；B02 把“跨页半写回拒绝启动”固化为测试与文档 | 能 |
| C09 | 先做 B24；只有出现耗尽 400 次时才加抖动 | 能 |
| C10 | `ResetEntityRegistryForTest` 加 `Deprecated:` 注释（不删导出，非破坏） | 能 |
| C11 | Remote L2 快照键增加可选前缀配置，缺省为空（键不变、无需迁移），kit 从部署前缀接线 | 能 |
| C12 | 保留为 feature 需求（生成 DAO 库名可配置涉及生成形状与数据迁移） | 否（需设计） |
| C13 | kit 暴露 `UnloadResyncConfig` 与共享加载超时的配置键，缺省值不变 | 能 |
| C14 | 默认值不变；文档写真实上界（按 Workers × Attempts × 退避计算），加积压指标 | 能 |
| C15 | 与 A10 一起：提交路径与装配校验对 `remote=managed` + sid 作用域 DAO fail-fast | 能 |
| C16 | 确认“builder 先注册、kind 后声明托管且生命周期矛盾”启动期拒绝（维护者确认即关闭） | 能 |
| C17 | `RegisterEntityKindDefs` 先整体校验再写入，出错不留半批 | 能 |
| C18 | 接受（重试用尽记 Warn，下次登录恢复），补一个计数指标 | 能 |
| C19 | 接受（loader 不支持卸载属兼容装配，已计数） | — |
| C20 | 保留到 Direct 有生产使用方 | — |
| C21 | 接受当前语义（跨会话恢复需新协议，业务未提需求） | — |
| C22 | 保留为设计题 | 否（需设计） |
| C23 | 保留观察（历史样本不可重现，LastError 已留存） | — |
| C24 | 保留在 feature 线（跨进程移交协议） | 否（需设计） |
| C25 | 不改返回值；`markCompleted` 失败记 Warn + 计数，不再静默 | 能 |
| C26 | 与 Cast 捕获失败同类处理：`CreateInScope` 捕获失败标记事务必须失败（业务吞错也回滚） | 能 |
| C27 | 新增 `DestroyReasonCreateRevoked`，撤销新建实体时使用（新增枚举值，非破坏） | 能 |
| C28 | 接受（`SessionOpenRetryable` + 有界退避已够） | — |
| C29 | 接受并写文档（非冷创建在 RetryLater 下按 ID 顺序） | 能 |
| C30 | 确认 `Request*` 保留 `ErrSyncInHandler`（维护者确认即关闭） | 能 |
| C31 | 在 roost-coding / 生成器文档写明“手写 Mod 的 StopBudget 不计入生成值”与测试 kind 占用约定 | 能 |
| C32 | 做：同 fence 下拒绝 StateVersion 回退（只影响非 authority 兼容装配） | 能 |
| C33 | 按记忆里的锁处理步骤尝试刷新图谱；有其他实例在用则保留 | 视环境 |

另：`.claude/worktrees/` 下 19 个与 scratchpad 下 1 个残留 worktree 都是本会话修复方的，分支已合入 main，推荐全部 `git worktree remove`（locked 的先 unlock）。A05 只 `git rm --cached` 并加 `.gitignore`，不改写历史。

### 执行批次与编号

新登记缺陷用 `RR-20260927-NN`（主记录 `docs/bug/`，修复记录 `docs/bugfix/`，来源写本清单条目号）。B 类探针确认是缺陷的，由主会话接续编号登记。

| 编号 | 条目 | 批次 |
| --- | --- | --- |
| RR-20260927-01 | A01 Windows CI 只读目录用例 | 1 生成器 / demo |
| RR-20260927-02 | A02 生成 TCP `CloseSessions` 恒为 0 | 1 |
| RR-20260927-03 | A03 + A04 demo `run.sh` 不传 redis.db、账号计数键不带前缀 | 1 |
| RR-20260927-04 | A08 + A09 doctor 非正 total 与建议值口径 | 1 |
| RR-20260927-05 | C05 player TCP Mod 声明停机预算 | 1 |
| RR-20260927-06 | C07 fence 后外层提交仍交给 committer（B03 证实后） | 3 Nest |
| RR-20260927-07 | C06 嵌套独立事务裸 mutation 绕过 RR-74 | 3 |
| RR-20260927-08 | C09 交叉创建重排抖动（仅 B24 出现耗尽时）——**未使用**：B24 3100 次试验 0 耗尽，不加抖动 | 3 |
| RR-20260927-09 | A10 + C15 Remote 托管 + sid 作用域 DAO 提交路径 fail-fast | 4 entity |
| RR-20260927-10 | C17 `RegisterEntityKindDefs` 原子化 | 4 |
| RR-20260927-11 | C26 `CreateInScope` 捕获失败强制事务失败 | 4 |
| RR-20260927-12 | C27 撤销新建实体用专门的销毁原因 | 4 |
| RR-20260927-13 | C13 kit 暴露卸载重载与共享加载超时配置 | 4 |
| RR-20260927-14 | C14 卸载重载积压指标与真实上界文档 | 4 |
| RR-20260927-15 | C32 同 fence 下拒绝 StateVersion 回退 | 5 DataEngine / Remote |
| RR-20260927-16 | C25 saga 收件箱 `markCompleted` 失败不再静默 | 5 |
| RR-20260927-17 | C11 Remote L2 快照键可选前缀 | 5 |
| RR-20260927-18 | B25 demo 场景 Manager 接卸载重载（可达时） | 6 Sync |
| RR-20260927-19 | C18 场景重开重试用尽计数 | 6 |
| RR-20260927-20 | A07 nest 单用例重复运行 duplicate handler | 2 卫生 |
| RR-20260927-21 | audit5 N1：同一 Guard 自我撤销后再建同 ID 空转重排 | 4 entity |
| RR-20260927-22 | B39 证实：`Unregister` 作用在已 forget 的旧 subject 上释放新登记的订阅 | 10 追加 |
| RR-20260927-23 | demo 场景未接 `OnEntityLoaded → Rebind`（RR-18 剩余边界） | 10 追加 |
| RR-20260927-24 | B23 证实：strict 截止回复不带 `ErrRemotePersistenceIndeterminate` | 11 追加 |
| RR-20260927-25 | B06 证实：不可比较的自定义 Mutex 在 `holding` 比较时 panic | 12 追加 |
| RR-20260927-26 | B07 证实：并发 Touch 下 `ReleaseCast(旧实例)` 释放新实例的锁 | 12 追加 |
| RR-20260927-27 | B20 相邻形状：loader 发布要锁领头方持有的实体时永久死锁 | 12 追加 |
| RR-20260927-28 | B41 证实：`RetractSyncSubject` 锁外比对后按 ID 注销，误注销同 ID 新登记 | 12 追加 |
| RR-20260927-29 | audit6 N1：RR-27 后领头方正常完成时不关 `leaderAway`，迟到 `RunLocal` 永久阻塞 | 13 追加 |
| RR-20260927-30 | audit6 N2：RR-25 不完整，含接口字段的 Mutex 仍 panic | 13 追加 |
| RR-20260927-31 | audit6：Cast 捕获失败被吞后事务照常提交 | 13 追加 |
| RR-20260927-32 | audit6：提交前拒绝不带 `ErrCommitRejected`、判别表无兜底行、RR-06 RollbackNone 表述 | 13 追加 |
| RR-20260927-33 | 批次 7：生成 compose 的 `tmpfs` 被逗号拆开，所有服务起不来（P2） | 14 追加 |
| RR-20260927-34 | 批次 7：生成镜像没有 `configs/data`，容器启动即失败（P2） | 14 追加 |
| RR-20260927-35 | 批次 7：demo 模板测试期望流名未含 `roost.room` 兼容映射 | 14 追加 |
| RR-20260928-01 | B42 证实：多 ManagerAccess 下积压 gauge 互相覆盖 | 15 追加 |
| RR-20260928-02 | B43 证实：Guard 跨 Manager 同 ID 时 RR-21 误判 | 15 追加 |
| RR-20260928-03 | 本地已提交、Remote 明确拒绝时回复无哨兵（维护者 09-28 决定新增哨兵 + 判别表兜底行） | 15 追加 |
| RR-20260928-04 | stats_log 在容器 / systemd 里从不落盘也不告警 | 16 追加 |
| RR-20260928-05 | shell / systemd 安装缺 `configs/data`（推断，待实证） | 16 追加 |
| RR-20260928-06 | game-demo 生产示例配置缺 `game_route` / `activity` / `platform`（推断，待实证） | 16 追加 |
| RR-20260928-07 | k8s Secret 示例缺 `saga` / `player_access` 段 | 17 追加 |
| RR-20260928-08 | audit7：strict 下 tracker 淘汰后 Overloaded 被误标 `ErrRemotePartRejected`；表测试改号、措辞 | 18 追加 |

| 批次 | 条目 | 状态 |
| --- | --- | --- |
| 0 | B36（RR-81～85 与 RR-73/80 补修的第五轮独立审计） | **完成**：全部成立，见 [audit5](REVIEW-2026-09-27-audit5.md)；新登记 RR-20260927-21（交批次 4），CHANGELOG 误写已更正，疑点记为 B39（交批次 6） |
| 1 生成器 / demo | A01、A02、A03、A04、A08、A09、C05 | **完成**（`441ced4`～`afade7a`）：RR-20260927-01～05 修复（RR-01 Windows 上未实跑，待 CI）；C05 使托管 TCP 的服务停机时长 +7s |
| 2 仓库卫生与文档 | A05、A06、A07、A11、A12、A13、E 节、C 类“接受 / 保留 / 确认”项的文档记录（C02、C10、C12、C16、C19～C24、C28～C31） | **完成**（`d9b9a82`～`97e8f4e`）：glsvet 取消跟踪；gofmt 后 `gofmt -l` 为空；RR-20260927-20 修复（25 个用例收尾清表）；A12 七处更正；A13 状态回写；E 节在原文件标注关闭（core-optimization 的 4 条未逐条核对、未关闭，RR-28 等 B26）；C 类决定写入各记录 |
| 3 Nest | B03→C07、B04、B05、B16、B17、B19、B24→C09、B26、C06 | **完成**（`dd6ec84`～`13b2b8d`）：B03 证实并修复为 RR-20260927-06；C06 修复为 RR-20260927-07；B04/B16/B17/B19/B26 无缺陷、收为回归（B26 更正 bf-28）；B05 不可达并注明；B24 3100 次试验 0 耗尽，不加抖动（C09 不做）；B19 边界：`PrepareRemoteWriteBatch` 执行期间调用不被拒，只影响文档字面，记入 RR-84 残留 |
| 4 entity / kit | B06、B07、B18、B20、A10+C15、C13、C14、C17、C26、C27、RR-20260927-21 | **完成**（`ceb7c70`～`3ff024f`，判别表合并修正 `00f67c7`）：RR-20260927-09～14、21 修复（A10/C15 的拒绝放在 `FinalizeLocked`——Validate 与选库处都在投影 / 重放路径上，在那里拒绝会造成投影毒丸，偏离清单候选但符合 bf-45）；B18 部分被 RR-21 推翻并更正 bf-81；B20 指定形状无缺陷；B06、B07、B20 相邻形状证实 → RR-20260927-25～27；整包高负载下 `TestSameIDCreateDoesNotOccupyFastPool/memory` 偶发一次失败 → B40 |
| 5 DataEngine / Remote / WAL / saga | B02、B08、B09、B10、B13、B14、B15、B23、C08（文档）、C11、C25、C32 | **完成**（`c9cf0cc`～`4c7424c`）：B02/B08/B09/B10/B13/B14/B15 无缺陷、收为回归（B14 更正 bf-38）；B23 判别表补 `ErrRemoteCommitTimeout`，代码与 RR-37 承诺不一致 → RR-20260927-24；C32/C25/C11 修复为 RR-20260927-15/16/17（C11 改为新增显式配置 `remote_entity.snapshot_l2_key_prefix`，缺省空：kit 没有缺省下能保持键不变的部署级 Redis 前缀，论证见 bf-17） |
| 6 Sync / demo 场景 | B21、B22、B25、B37、C18、B39 | **完成**（`366e7c9`～`879b909`）：B21/B22/B37 无缺陷、收为回归；B25 可达并修复为 RR-20260927-18；C18 修复为 RR-20260927-19；B39 证实 → RR-20260927-22；剩余边界 → RR-20260927-23；B22 的 remoteflow 真实拒绝留批次 7 |
| 10 追加修复 | RR-20260927-22、23，RR-84 残留（B19 边界文档） | **完成**（`ed32ba3`～`e16e356`）：RR-22 取锁后确认表项、旧 subject 已 forget 时注销当前登记；RR-23 场景接 `OnEntityLoaded → Rebind`；RR-84 残留文档更正；候选窗口 → B41 |
| 11 追加修复 | RR-20260927-24 | **完成**（`7b56060`）：strict 截止回复同时满足 `ErrRemotePersistenceIndeterminate`；仓内按两哨兵分支的调用点逐处核对无回退 |
| 12 追加修复 | RR-20260927-25、26、27，B40、B41 | **完成**（`84f9424`～`1f0ba87`）：RR-25～28 修复（RR-27 选“发布交回领头方 goroutine”）；B40 为用例时序问题（follower 的 try-lock 判定晚于放开提交），已修测试；B41 证实 → RR-28 |
| 门禁 | 批次 1～12 合入后 HEAD `afade7a`：build / vet / glsvet、整仓 race 120 包、entity / nest / dataengine/engine 整包 `-count=2`、sync-modes、新生成 game-demo build / vet / test 17 包 / game 与 account race / doctor（game 114s / 119s）/ `project diff` 0 文件，全部通过；剩余 11 个未格式化文件 `001b03b` 补齐，全仓 `gofmt -l` 为空 | **完成** |
| 审计 | 第六轮独立审计 RR-20260927-01～28（两路） | **完成**：[audit6](REVIEW-2026-09-27-audit6.md)；登记 RR-20260927-29～32，三处文档直接更正 |
| 13 追加修复 | RR-20260927-29～32，B42、B43 | **完成**（`0d3205d`～`354c68f`）：RR-29～31 修复；RR-32 提交前拒绝统一带 `ErrCommitRejected`、判别表第 1 / 11 行与 RR-06 表述更正，兜底行因反例待定 → 维护者决定新增哨兵（RR-20260928-03）；B42、B43 证实 → RR-20260928-01、02 |
| 14 追加修复 | RR-20260927-33～35，B44、B45 | **完成**（`f2d0575`～`149eaac`）：compose `tmpfs` 修正、镜像自带 `configs/data`（本机 compose 10 服务 healthy、stop 0.69s 全部 exit 0）、kit 导出流名解析；B44 为用例时序问题已修测试；B45 清理后隔离环境零残留；新发现 → RR-20260928-04～06 |
| 15 追加修复 | RR-20260928-01～03 | **完成**（`55e2fac`～`adfa7ac`）：gauge 按进程汇总；撤销记录以（Manager, ID）为键；新增 `ErrRemotePartRejected`（判别表第 4 行），兜底行因反例先补第 14 行“调用方等待截止：结果未知”再加第 15 行兜底 |
| 16 追加修复 | RR-20260928-04～06 | **完成**（`c5857a5`～`e80d88e`）：stats_log 写失败计数告警、部署物给可写目录（本机 compose 10 服务落盘）；shell / systemd 安装实证缺数据并修复（release 内带数据、WorkingDirectory 改为 `$APP_ROOT/current`）；prod / Secret 示例补三段（compose 按 prod 示例起 game healthy）；新发现 → RR-20260928-07；T-177 |
| 17 追加修复 | RR-20260928-07 | **完成**（`414e965`，DEPLOYMENT k8s 章节对齐 `462c8d7`） |
| 审计 | 第七轮独立审计（RR-20260927-29～35、RR-20260928-01～06） | **完成**：[audit7](REVIEW-2026-09-28-audit7.md)；登记 RR-20260928-08 |
| 18 追加修复 | RR-20260928-08 | 进行中 |
| 7 集成环境（串行） | B01、B12、B28、B31、B11、B32、B33、B34、B35、B38、C04（compose）、B22 后一半 | **完成**（`a2d08d6`～`21555fd`）：B28 三项门禁全绿（注：`dataengine-env.sh test` 含故障注入，会重启隔离 mongo-1 / nats）；B01 为用例固定键残留，非问题，WANTED 分流；B12 / B31 / B11 收为回归；B32 升级人工处理清单写入 bf-24；B33 漂移文件非 golden（D）；B35 两份记录不矛盾（取决于 Mod 集）；B34 / C04 停机实测无缺陷（compose 需先修两个部署物缺陷）；B38 跨窗口实跑通过、CARRYOVER B4 关闭；B22 后一半本地做不了 → C34；新缺陷 → RR-20260927-33～35，偶发 → B44，观察 → B45 |
| 8 长跑与性能（最后、独占机器） | B29、B30、C01、C03 | 待开始 |
| 9 真实环境端到端补测（分四批） | B27 | 待开始 |
| — | C33 图谱刷新 | 批次 8 前尝试 |
