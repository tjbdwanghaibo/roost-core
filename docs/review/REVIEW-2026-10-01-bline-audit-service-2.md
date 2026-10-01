# B 线 Service 后半部分与 Redis driver 修复复审（2026-10-01）

独立审计，只读代码、跑测试；没有改仓库文件、没有提交。基线 main `db4b7009`。被审对象：B 线 2026-09-29～30 在 activity / rank / match / session / chat / directory / global 与 `redis/driver` 上的修复——RR-20260929-02、05、06、07、08、09、10、13、14、15、17、18、21、25、26、27、29、31、34 以及顺带修的旧残余 RR-20260909-02（session 正常 Finish ABA）、RR-20260914-02（activity Opening 迟到确认）。account / mail / platform（RR-01、03、04、11、12、16、19、20、22、23、24、28、30、32、33、RR-20260910-02）由另一位审计员负责，这里不重复。

## §0 范围与方法

- 规则源：`AGENTS.md`、`docs/agent-skills/roost-coding/SKILL.md`（"不许的修法"、Update 回调所有权、幂等 / 未知结果区分、版本只在键生命周期内单调）、`~/.claude/skills/roost-review/SKILL.md` 的审查口径（确认缺陷 / 疑点 / 文档不一致分开列，不替修的一半编号）。
- B 线记录：`docs/bugfix/SERVICE-BUGFIX-2026-09-29.md` 与 `-03/-05/-06/-07/-09`、逐条 `docs/bugfix/RR-20260929-*.md`、问题主记录 `docs/bug/REVIEW-2026-09-29-services{,-02,-03,-05,-07,-08,-10}.md`、`docs/review/SERVICE-REVIEW-COMPLETION-2026-09-29.md`、`REVIEW-2026-09-30-services-12/13.md`、`ARCHIVE-2026-09-30.md`、CHANGELOG `## [v1.18.0]`。
- 修复提交（`git show --stat`）：`83c04243`（RR-01～18 + RR-20260909-02 残余）、`bdbb61bc`（RR-19～22）、`4b0837d7`（RR-25～27 + RR-20260914-02 残余）、`5f81e5c8`（RR-28～30）、`004c8cf8`（RR-31/32）、`16579298`（RR-34）。每条都把提交 diff 与当前源码对读；行号一律是 main `db4b7009` 上的。
- 图谱：`Users-whb-roost-roost-core` status=ready，generation `2026-09-30T11:28:30Z`，`generation_matches: true`；对本报告引用的 26 个源码路径 `check_index_coverage` 全部 `no_recorded_issue / metadata_match`。本次核对以 `git show` + 直接读源码为主（审的是 diff，行级证据必须来自源码），没有用 search_graph / trace_path 做结构发现；调用方扫描用 `rg`。
- 跑过的命令（都在 main 工作树，`GOWORK=off`）：
  1. `go test -count=1 -race ./service/session ./service/match ./kit/service/global/... ./kit/service/rank ./kit/service/match ./kit/service/session ./kit/service/chat ./kit/service/directory ./redis/driver` → 10 包全部 `ok`，EXIT=0（`scratchpad/unit-race.log`）。
  2. `REDIS_ADDR=ROOST_REDIS_TEST_ADDR=ROOST_REVIEW_REDIS=127.0.0.1:17379 ROOST_REVIEW3_BACKEND=redis ROOST_BUGFIX5_BACKEND=redis go test -tags integration -count=1 -p 1 -race -v ./kit/service/integration/ ./kit/service/rank/ ./service/session ./versionstore ./redis/driver ./kit/service/global/activity ./kit/service/chat ./kit/service/directory ./service/match` → 9 包 `ok`，EXIT=0；叶子 554 PASS / 10 SKIP / 0 FAIL（`scratchpad/integration-redis.log`）。10 个 SKIP 全是本机没有的环境：Cluster 5 个（`TestBugfix6RankTaggedClusterLifecycle`、`TestBugfix7ActivityTaggedClusterLifecycle`、`TestBugfix7ActivityClusterRecoversPartialCompletion`、pipeline/index-retirement 的 `cluster_true` 子项）、Toxiproxy 3 个（lock 用例，不在本审范围）。隔离 Redis 17379 只用测试 prefix，没有 FLUSH。
  3. `go vet` 同上 11 包 + `./versionstore ./kit/mods` → 通过。
  4. 红是否真红：临时 worktree `scratchpad/audit-svc2`（`db4b7009`），逐条 revert 实现后跑回归，见 §2；用完已 `git worktree remove`。
  5. 两个探针（`zz_probe_*_test.go`，跑完即删，只在 worktree）：activity 孤儿 pending 证明（§3.1）、chat 无游标最新页 Gap（§4.2）。

## §1 逐条表

"记录声称"取 bugfix 记录的根因 / 实现句；"代码核对"指到当前 main 行；"回归"列出记录点名的测试与本次在 main 上的结果（U = 上面第 1 条命令，I = 第 2 条）。

| 编号 | 域 | 记录声称 | 代码核对结论（main `db4b7009`） | 回归测试（文件·函数，main 上结果） | 结论 |
| --- | --- | --- | --- | --- | --- |
| RR-20260929-02 | activity | 最近 32 ring 会淘汰已应用未确认请求；改为独立 `PendingRequestIDs` 不淘汰、满则背压，Applies 快照拦旧 reserved 读者 | `types.go:507-510` 新字段，`:519` `Applied` 同时查两张表；`service.go:1014` 外层按 `ErrVersionMismatch` 重试，`:1063` 先读 participant、`:1072-1080` 再查 ledger 收集 `confirmed`，`:1134` `current.Applies != observed.Applies` 拒旧快照，`:1148` 满 32 返回 `ErrConflict`，`:1179 clearPendingProgress` 在 mark 之后清。与记录一致 | `activity/rr_20260929_round1_test.go·TestReviewUnconfirmedProgressSurvivesRingEviction` PASS(I)；`bugfix_progress_proof_test.go·TestUnconfirmedProgressBackpressureNeverEvictsProof`、`TestStaleReservedReaderCannotApplyAfterProofWasReclaimed` PASS(I) | **疑点→缺陷**：ledger TTL 过期后的 pending 永不回收，32 个即永久拒绝该参与者，见 §3.1 |
| RR-20260929-05 | rank | 逗号拼接不能表示含分隔符的 requestID；新 `applied_v2` JSON 数组，tagged 读取 legacy/v2，损坏账本拒绝 | `redis_store.go:107-109` Lua 读 `applied_v2`/`applied` 组 tagged ring，`:121-122` 写 v2 并 HDEL 旧字段，`:222-236` `readOwner` 校验 v2 JSON（坏则 `invalid request ledger` 错误），`:458 decodeRing`。一致 | `rank/rr_20260929_round1_test.go·TestReviewCommaRequestIsIdempotent` PASS(I)；`bugfix_ring_compatibility_test.go·TestLegacyRequestRingUpgradesWithoutDroppingPriorIDs`、`TestMalformedRequestRingFailsClosed` PASS(I) | 通过 |
| RR-20260929-06 | rank | 只 CAS member 不能发现 ring 并发变动；Lua 同时比 member 与完整 tagged ring，no-op CAS 失败也退避重读 | `redis_store.go:109` `currentRing ~= previousRing → {0,…}`；`:172-183` no-op 分支 `if !applied { RetryBackoff; continue }`；`:134` Remove HDEL 两个字段。一致 | `rank/rr_20260929_round1_test.go·TestReviewUnchangedMemberCASProtectsRequestRing` PASS(I，真实 Redis Lua) | 通过 |
| RR-20260929-07 | chat | Kind 原样拼冒号可伪造 pair 键；仅对 Kind 的 `%`/`:` 可逆转义 | `chat.go:408` `NewReplacer("%","%25",":","%3A")`，`:411/:417` shared/pair 两处都用转义后的 kind。键只在 `resolveRef` 一处生成（`store.go:315-320`、`:353` 都经它），retention runner 也走 `Resolve`；`Channel.String()`(`chat.go:359`) 仍用原 Kind，但只用于文案 | `chat/rr_20260929_round1_test.go·TestReviewCustomSharedChannelCannotReadPrivatePair` PASS(I) | 通过 |
| RR-20260929-08 | activity | AttemptDispatch 先查预算后查 Due，最后一次成功后立即重试把状态写成 exhausted；改为先判 Due | `service.go:1436` `if !current.Due(nowUnix) → ErrDispatchNotDue` 已移到 `Attempts >= MaxAttempts` 之前。一致 | `activity/rr_20260929_round1_test.go·TestReviewLastDispatchRetryKeepsAckWindow` PASS(I) | 通过 |
| RR-20260929-09 | session | 私有 `sweepOwners` 恒 nil；新增 `Config.Owners / OwnerSource / SweepPending`、kit `WithSweepOwners` | `service/session/service.go:56-66` 接口与 Func；`sweep_source.go:8` `BackgroundSweepEnabled`，`:12-27` `SweepPending`（limit ≤ MaxPageSize=200、source 超 limit 拒绝、未配置返回 nil）；`kit/service/session/session_mod.go:34-37` `NewMod(..., options...)`、`:97` `Owners: m.owners`；`server_run.go:58` 每 tick `SweepPending(ctx, SweepBatch=100)`；`options.go:7`。`Sweep`(`service.go:759`) 的 limit 是"最多解决的 run 数"，与 `len(owners) > limit` 的拒绝口径一致 | `service/session/sweep_source_test.go·TestConfiguredOwnerSourceReleasesExpiredRun`、`TestOwnerSourceIsBoundedAndFailuresAreRetryable` PASS(I)；`kit/service/session/options_test.go·TestWithSweepOwnersCarriesDeploymentRoster` PASS(U，另跑 -v 确认) | 通过（记录如实写"没有等待真实 30 秒 ticker"） |
| RR-20260929-10 | rank | radius=100 被接受、内部 Page limit=201 又被拒；Around 把长度限到 MaxPageSize | `redis_store.go:343` `s.Page(ctx, board, offset, min(radius*2+1, MaxPageSize))`。一致，窗口远端少一条已写进记录 | `rank/rr_20260929_round1_test.go·TestReviewAroundAcceptedMaximumRadius` PASS(I) | 通过 |
| RR-20260929-13 | global | 续期只查 incarnation；Acquire/Renew 依 route 快照 CAS 并写后再核验，Lease/LiveGames 同样核验 | `service.go:281-283` Acquire 拒 `current.RouteEpoch > binding.Epoch`，`:284` 只有同 binding 的 active lease 才算 held；`:306` 写后 `checkLeaseBinding`；`:331-345` Renew 先 `Resolve` 再 CAS 内 `sameLeaseBinding`，`:373` 写后再核验；`:433-440` Lease 不同 binding 报 lapsed；`:470-478` LiveGames 跳过；`:488/:494` 比较 GameSID/Epoch/GlobalSID/GroupID 四元。`ReleaseLease`(`:381`) 只按 incarnation，旧持有者释放自己的 stale lease 无害 | `kit/service/global/rr_20260929_round2_test.go·TestReviewLeaseCannotRenewAgainstChangedRoute` PASS(U，-v 确认) | 通过；疑点 §4.3（写后核验失败时 lease 已物理写入，记录已声明"核验之后仍可能迁移"） |
| RR-20260929-14 | session | Attach 拒绝非零 `ReleasedAtUnix`/`ForcedRelease` | `service.go:480-482`。相邻入口：`EnterRequest`(`service.go:166-178`) 只有 Kind/RequestID/Context，没有资源字段，Enter 不是第二条通路 | `service/session/rr_20260929_round2_test.go·TestReviewAttachCannotAssertAlreadyReleased` PASS(I) | 通过 |
| RR-20260929-15 | activity | 累计和超 int64 回绕；CAS 内检查 `Score/Progress > MaxInt64-delta` 与 `Applies==MaxUint64` | `service.go:1150-1152`，在修改任何字段之前返回 `ErrRequestInvalid`，`current` 原样返回。`ProgressDelta.Validate` 保证 delta ≥ 0，所以减法不下溢 | `activity/rr_20260929_round2_test.go·TestReviewPositiveProgressCannotWrapNegative` PASS(I) | 通过 |
| RR-20260929-17 | global | 输出 Load map 与 MemoryStore 共享；Renew/Lease/LiveGames 返回 `cloneLease` | `service.go:367` `result = cloneLease(next)`，`:433`、`:470`，`:505 cloneLease`。相邻：`AcquireLease:300` `result = next` 未 clone，但 Acquire 不写 Load（`next.Load` 为 nil），无共享引用；Bind/Migration 返回的是 RouteBinding，无 map | `kit/service/global/rr_20260929_round2_test.go·TestReviewLeaseSnapshotsOwnLoadMaps/{renew,lookup,live}` PASS(U，-v 确认) | 通过 |
| RR-20260929-18 | session | 终态分支 `releasePending` 后直接返回不释放 claim；改为释放成功才 `releaseClaim` | `service.go:582-586`：`run, err := releasePending(...)`；err 直接返回（claim 保留供恢复）；成功才 `releaseClaim(ctx, ownerID, runID)`（带 RunID 身份校验） | `service/session/rr_20260929_round2_test.go·TestReviewSuccessfulFinishRetryFreesClaim` PASS(I，真实 Redis) | 通过 |
| RR-20260909-02 残余 | session + versionstore | 正常 Finish 的 Get/RunID 校验与 Delete 之间仍有 v1 ABA；新增 `ConditionalDeleter.DeleteIf`，`ClaimStore` 强制要求 | `versionstore/versionstore.go:69` 接口；`memory_store.go:72` 锁内版本 + `match`；`redis_store.go:413-453`：DeleteIf 内部**重新读** raw/current，版本比对、`match(current.Value)`，再用 `:455 deleteIfScript` 以同一 raw 字节 `GET==ARGV[1]` 才 `DEL`+`ZREM`——predicate 针对的正是被原子删除的那份值，符合接口注释。`Delete` 变成 `DeleteIf(nil)`（旧的 sentinel+TTL 两步改为一段 Lua，语义不变）。`service/session/service.go:24-27` ClaimStore 接口，`:456-458` predicate 比 `RunID && OwnerID`（Claim 的全部身份字段；Lua 还比完整字节，更强）。只有 `Delete` 不带 predicate 时版本重复才会误删，所以身份论证完整 | `service/session/rr_20260929_round2_test.go·TestReviewTerminalFinishCannotDeleteReacquiredClaim` PASS(I，真实 Redis)；`versionstore/conditional_delete_test.go·TestConditionalDeleteProtectsRecreatedIdentity/{memory,redis}` PASS(I) | 通过；红已证 §2.2 |
| RR-20260929-21 | directory | Cancel/Release 的 Token/Owner 检查与版本 Delete 分离；改用 DeleteIf 原子匹配，`New` 拒绝不支持的 store | `store.go:53-57` 构造门禁；`:241-243` Cancel 匹配 `Token==claim.Token && Owner==读到的 Owner && State==Reserved`；`:289-291` Release 匹配 `Owner==调用 owner && Token==读到的 Token && State==读到的 State`。两处 `ErrVersionMismatch` 分支语义保留（Cancel no-op、Release owner mismatch）。仓内 production 没有包装 `versionstore.Store` 的 wrapper 丢失该能力（`rg 'versionstore.Store\['` 只有别名 / 字段） | `kit/service/directory/rr_20260929_round3_test.go·TestReview3DirectoryDeleteCannotRemoveRecreatedOwner`（memory/redis × cancel/release × owner-a/b，8 子项）PASS(I)；`TestDirectoryRequiresConditionalDelete`(`:125-131`) 断言构造拒绝，PASS(U) | 通过；红已证 §2.4 |
| RR-20260929-25 | match | 公开 Group 不校验 Queue，非法组大小 panic/伪成功；入口先 `Queue.Validate` | `grouping.go:31`、`:59` 两个策略各自先 `queue.Validate()`。一致 | `service/match/bugfix_grouping_boundary_test.go·TestBugfix5GroupingValidatesBeforeReadingCandidates`（fifo/score × size -1/0/1/65 × 候选 0/2）PASS(I) | 通过 |
| RR-20260929-26 | rank | UpdateAdd 溢出落库；`nextScore` 在 Lua CAS 和 ring 写入前拒绝 | `redis_store.go:410-413` 正负两向检查返回 `ErrScoreInvalid`，发生在 `swap` 之前，所以 requestID 未被占用 | `kit/service/rank/bugfix_add_overflow_test.go·TestBugfix5OverflowDoesNotChangeScoreOrConsumeRequest/{positive,negative}`、`TestBugfix5ConcurrentAddsCannotWrapAfterCASRetry` PASS(I) | 通过 |
| RR-20260929-27 | chat | Prune 只清前缀，时钟偏移漏掉过期后续；改为扫整个有界 Ring，`pageOf` 报告页内/尾部洞 | `store.go:642-672` Prune 逐条按 `StoredAtUnix<=cutoff` 过滤、最多 limit、保留 Seq 顺序与 Requests；`:590-619` Gap 逻辑。一致 | `kit/service/chat/bugfix_retention_gap_test.go·TestBugfix5AgePruneHandlesInteriorAndTailGaps`、`TestBugfix5GapOnlyReportsTheRangeActuallyPaged` PASS(I，Redis 模式) | 通过；疑点 §4.2（无游标最新页在普通容量淘汰后也报 Gap，记录没写） |
| RR-20260929-29 | rank | Cluster 下 Mod.Init 调 `mods.ValidateClusterKeyPrefix` | `rank_mod.go:61`；`kit/mods/service_servicemods.go:51-63`：`redis.cluster_addrs` 非空时要求第一个 `{` 之后第一个 `}` 且中间非空——与 Redis 的 hash tag 规则一致；配置键名与 `kit/redis/redis_mod.go:49` 读的同一个 | `kit/service/rank/bugfix_cluster_prefix_test.go·TestBugfix6RankClusterRejectsInvalidPrefix/{rank,{}:rank,{rank,{}:{valid}:rank}` PASS(I)；`TestBugfix6RankTaggedClusterLifecycle` **SKIP**（本机无 Cluster，未实跑） | 通过（真实 Cluster 生命周期本机未实跑） |
| RR-20260929-31 | activity | Mod.Init 读 KeyPrefix 后调 `ValidateClusterKeyPrefix` | `activity_mod.go:72-74`，注释 `:60-62` 同步。相邻：仓内用 `versionstore.RedisIndex`（双 key Lua）的只有 activity dispatch 与 platform（`rg 'Index: &versionstore.RedisIndex'`），rank 自己的双 key 脚本——三处现在都校验；其他服务都是单 key CAS，没有漏网入口 | `kit/service/global/activity/bugfix_cluster_prefix_test.go·TestBugfix7ActivityClusterRejectsInvalidPrefix`（4 子项）、`TestBugfix7ActivityStandaloneKeepsPlainPrefix` PASS(I)；`TestBugfix7ActivityTaggedClusterLifecycle`、`TestBugfix7ActivityClusterRecoversPartialCompletion` **SKIP**（未实跑） | 通过；红已证 §2.3 |
| RR-20260914-02 残余 | activity | Opening 回收后迟到 Create 突破窗口、超容量窗口扫描不可达；改为 `OpeningEntry.Intent` 持久计划、confirm 只消费已有名额、`ScanAfter` 轮转 | `service.go:240-260` Open 先 `admitToWindow(activity)` 落 Intent 再 Create；`:268-290 confirmWindow` 无 admission 返回 `ErrConflict`，不再无条件补 Keys；`:295-345 admitToWindow` 重试返回已存 Intent、第 257 次拒绝；`:770-800` sweep 对过 grace 且有 Intent 的 entry 代为 Create，无 Intent 的 legacy 不回收；`:858 pendingScanBatch` 游标轮转，`AdvanceExpired` 在 `pendingScanBatch` 之后、任何 I/O 之前先持久化 `ScanAfter`（仅超容量窗口）；`types.go:789-791、810`；`Window.clone` 深拷贝 Intent/ScanAfter（`:853-862`）。与记录一致 | `activity/bugfix_opening_recovery_test.go·TestBugfix5LateCreateCannotBorrowAnAlreadyPromisedSlot`、`TestBugfix5LostAdmissionReplyRecoversItsOriginalPlan`、`TestBugfix5CreatedOrConfirmedReplyLossRemainsRecoverable/{create,confirm}`、`TestBugfix5OversizedLegacyWindowRotatesAcrossServiceRebuild`、`TestBugfix5LegacyOpeningNeedsAPlanInsteadOfTimeoutReclaim` 全部 PASS(I) | 通过；疑点 §4.4（无 Intent 的 legacy Opening 永久占名额；单条坏 Intent 会让整个 group 的 AdvanceExpired 每 tick 失败） |
| RR-20260929-34 | redis/driver | aggregate 为 `redis.Nil` 时 wrapper 吞掉后续写命令错误；改为先填全部 future、保留传输错误，再逐命令返回首个非 Nil 错误 | `pipeline.go:107` 保留 `commands`；`:109-124` 先给三类 future 赋值；`:128-130` 非 Nil 的 aggregate/传输错误原样返回；`:131-135` 遍历 `commands` 返回首个非 nil 非 Nil 的 `Err()`；`defer`(`:102-106`) 仍清本批 future。go-redis v9.22 `Pipeline.Exec` 文档："always returns list of commands and error of the first failed command"，所以 `commands` 在错误路径也非 nil。`redis/pipeline.go:22-27` 契约注释同步。调用方 `rg 'Pipeline()'`：`cache/ref_hmap.go:168/249/268/348`（四处都 `return pipe.Exec(ctx)` 或 `if err := pipe.Exec(); err != nil { return }`，新错误直接上抛，没有把 nil 当成功的残留逻辑）、`service/mail/redis_store.go:218`（RR-33，逐 future 检查，不依赖本修复）；仓内只有 driver 一个 IPipeline 实现 | `redis/driver/pipeline_errors_test.go·TestPipelineExecChecksEveryCommandError/{missing_before_failure,nil_aggregate_with_failure,only_missing,all_success,transport_error_preserved}`、`TestPipelineExecPopulatesAllFuturesOnFailure` PASS(I)；`pipeline_errors_integration_test.go·TestIntegrationPipelineDoesNotHideWriteErrors/cluster_false/{hset,rpush,zadd}×{first,middle,last}`、`TestIntegrationPipelineFuturesAndLifecycle/cluster_false`、`TestIntegrationPipelinePreservesPostWriteUnknownError/cluster_false` PASS(I)；三者的 `cluster_true` **SKIP** | 通过；红已证 §2.1 |

**§1 统计**：21 条（19 个新编号 + 2 个旧残余）。记录与代码一致 21/21；点名的回归在 main 上全部存在且通过 21/21（其中 5 个 Cluster 子用例本机 SKIP，未实跑）。结论：通过 20 条，其中带疑点 4 条（RR-13、RR-27、RR-20260914-02 残余、RR-31 Cluster 未实跑）；**缺陷 1 条**（RR-02 的修法引入新的永久拒绝路径，§3.1）。

## §2 "红是否真红"抽查

都在临时 worktree `scratchpad/audit-svc2`（`db4b7009`）上做：只 revert 实现、保留测试，跑完 `git checkout --` 复原。环境变量同 §0 第 2 条。

### §2.1 RR-20260929-34（pipeline）

`git show 16579298 -- redis/driver/pipeline.go | git apply -R`（该文件自修复提交后未再改动，`git diff --quiet 16579298 db4b7009 -- redis/driver/pipeline.go` 为空），然后 `go test -count=1 -tags integration ./redis/driver -run 'TestPipelineExec|TestIntegrationPipeline'`：

```
--- FAIL: TestIntegrationPipelineDoesNotHideWriteErrors/cluster_false/hset/first    pipeline_errors_integration_test.go:78: failed hset hidden: <nil>
--- FAIL: .../hset/middle   failed hset hidden: <nil>
--- FAIL: .../rpush/first   failed rpush hidden: <nil>
--- FAIL: .../rpush/middle  failed rpush hidden: <nil>
--- FAIL: .../zadd/first    failed zadd hidden: <nil>
--- FAIL: .../zadd/middle   failed zadd hidden: <nil>
--- FAIL: TestIntegrationPipelineFuturesAndLifecycle/cluster_false   pipeline_errors_integration_test.go:127: map error hidden: <nil>
--- FAIL: TestPipelineExecChecksEveryCommandError/missing_before_failure   pipeline_errors_test.go:47: Exec=<nil>, want command failed
--- FAIL: TestPipelineExecChecksEveryCommandError/nil_aggregate_with_failure   pipeline_errors_test.go:47: Exec=<nil>, want command failed
--- FAIL: TestPipelineExecPopulatesAllFuturesOnFailure   pipeline_errors_test.go:69: Exec: <nil>
FAIL	github.com/tjbdwanghaibo/roost-core/redis/driver	0.510s
```

真红（10 个叶子）。`.../last` 子项不红是对的：写命令在最后时 go-redis 的 aggregate 本来就是它的错误。

### §2.2 session `ClaimStore` / `DeleteIf`（RR-20260909-02 残余）

先试把 `service/session/service.go:456` 的 `DeleteIf` 改回 `Delete`：`TestReviewTerminalFinishCannotDeleteReacquiredClaim` **挂死**（7 分钟后手动 kill）——它的暂停钩子 `reviewPausedClaimDelete` 挂在 `DeleteIf` 上（`rr_20260929_round2_test.go:66-71`），不走 DeleteIf 就永远等不到 `entered`。这是测试与修法耦合，不是绿。改为保留 `DeleteIf` 调用但传 `nil` 谓词（即只比版本，钩子仍触发）：

```
--- redis backend (ROOST_REVIEW_REDIS=127.0.0.1:17379):
    rr_20260929_round2_test.go:89: real Redis backend
    rr_20260929_round2_test.go:110: late Finish deleted replacement claim: b=run-2 c=run-3 both live
--- FAIL: TestReviewTerminalFinishCannotDeleteReacquiredClaim (0.00s)
--- memory backend:
    rr_20260929_round2_test.go:110: late Finish deleted replacement claim: b=run-2 c=run-3 both live
--- FAIL: TestReviewTerminalFinishCannotDeleteReacquiredClaim (0.00s)
```

真红（两种后端）。这同时证明 Redis `DeleteIf` 内的"重新读 + 同字节 Lua"本身挡不住 ABA——身份谓词才是围栏，和记录的论证一致。

### §2.3 RR-20260929-31（activity Cluster 前缀）

删掉 `activity_mod.go:72-74` 三行，`go test -count=1 -tags integration ./kit/service/global/activity -run TestBugfix7ActivityClusterRejectsInvalidPrefix`：

```
    bugfix_cluster_prefix_test.go:124: configuration rejection: <nil>   (×4)
--- FAIL: TestBugfix7ActivityClusterRejectsInvalidPrefix/activity
--- FAIL: .../{}:activity
--- FAIL: .../{activity
--- FAIL: .../{}:{valid}:activity
FAIL	github.com/tjbdwanghaibo/roost-core/kit/service/global/activity	0.593s
```

真红。注意这个用例只需要配置里有 `cluster_addrs`，不需要真集群；真集群生命周期用例本机 SKIP。

### §2.4 RR-20260929-21（directory DeleteIf）

把 `store.go:241/:289` 两处 `s.deleter.DeleteIf(..., predicate)` 改回 `s.state.Delete(ctx, key, current)`，`ROOST_REVIEW3_BACKEND=redis go test -count=1 -tags integration ./kit/service/directory -run TestReview3DirectoryDeleteCannotRemoveRecreatedOwner`：

```
    rr_20260929_round3_test.go:117: old cancel deleted new owner on memory: found=false entry={...} oldErr=<nil>
    ... (memory/redis × cancel/release × owner-a/owner-b)
--- FAIL: TestReview3DirectoryDeleteCannotRemoveRecreatedOwner  (8/8 子项 FAIL)
FAIL	github.com/tjbdwanghaibo/roost-core/kit/service/directory	0.601s
```

真红（8 个叶子，含真实 Redis）。

## §3 确定的缺陷

### §3.1 activity：ledger TTL 过期后的 pending 证明永不回收，32 个即永久拒绝该参与者（RR-20260929-02 修法引入）

**现象。** 一个参与者累计 32 个"participant 已加分、ledger mark 失败、客户端没有在 `ReservationTTL`（默认 30 分钟）内重放"的请求之后，它之后的**每一个**新请求都返回 `versionstore: version conflict: pending progress confirmations are full`（`errors.Is(err, versionstore.ErrConflict)` 为真），ledger 已经恢复健康也一样；唯一能释放一个名额的动作是原客户端用**完全相同的 requestID** 重放。没有管理入口（`admin.go` 只有 `ReopenDispatch`）。

**触发条件。** `applyProgress` 的 participant CAS 成功（`service.go:1156` 把 requestID 追加进 `PendingRequestIDs`）之后 `markReservationApplied`(`:1169`) 失败，且该请求之后没有重放。ledger 条目只会被 TTL 删除（`rg 'Ledger\.'` 只有 Create/Get/Update，`redis_store.go:32-39` 注释也说 ledger 是唯一带 TTL 的 store）；TTL 到期后 `:1072-1080` 的确认扫描只认 `found && State == ReservationApplied`，`!found` 的 pending 永远留在 `PendingRequestIDs`，`:1148` 的 `len >= MaxProgressWindow` 从此恒真。Memory 后端没有 TTL，所以只在 Redis 部署发生；一次 Redis 短暂不可用落在 CAS 与 mark 之间的窗口、或 fire-and-forget 的进度上报，都能一次性制造多个孤儿。

**根因。** `83c04243`，`kit/service/global/activity/service.go` 现 `:1072-1080`（confirmed 只收 Applied）与 `:1147-1149`（满 32 背压）。修复把"未确认不能淘汰"做成了无条件，但 ledger 的契约（`service.go:58-61`、`redis_store.go:32-39`：TTL 之后客户端不再重试，重放等同新请求）意味着 `!found` 已经越过了 pending 要保护的那条地平线——继续保留只剩成本没有收益。而且背压用 `ErrConflict`（可重试类）报告一个永久状态，调用方会一直重试。

**会红的测试草稿**（本次在 worktree 上以 `zz_probe` 跑过，输出如下；正式版把 `t.Logf` 换成断言即可）：

```go
// kit/service/global/activity/…_test.go
func TestOrphanedPendingProofsAreReclaimedAfterLedgerExpiry(t *testing.T) {
	ledger := versionstore.NewMemoryStore[RequestKey, ProgressReservation]()
	s, _ := newActivityService(t, func(c *Config) { c.Ledger = failedProgressMark{ledger} }) // mark 永远失败
	ctx := context.Background()
	key := activityKey("orphan-pending")
	openActivity(t, s, key, 1)
	for i := 0; i < MaxProgressWindow; i++ {
		if _, err := s.ApplyProgress(ctx, key, "p", fmt.Sprintf("lost-%d", i), ProgressDelta{Score: 1}); err == nil {
			t.Fatal("expected mark failure")
		}
	}
	for i := 0; i < MaxProgressWindow; i++ { // 模拟 ReservationTTL 到期
		rk := RequestKey{Activity: key, ParticipantID: "p", RequestID: fmt.Sprintf("lost-%d", i)}
		stored, _, _ := ledger.Get(ctx, rk)
		if err := ledger.Delete(ctx, rk, stored); err != nil { t.Fatal(err) }
	}
	s.cfg.Ledger = ledger // ledger 恢复健康
	if _, err := s.ApplyProgress(ctx, key, "p", "fresh-1", ProgressDelta{Score: 1}); err != nil {
		t.Fatalf("participant permanently refused after ledger expiry: %v", err) // 当前：ErrConflict
	}
}
```

探针实际输出（main `db4b7009`）：

```
after 32 orphaned marks + ledger expiry: Score=32 pending=32
fresh request with healthy ledger: err=versionstore: version conflict: pending progress confirmations are full (ErrConflict=true)
second fresh request: err=versionstore: version conflict: pending progress confirmations are full
replay of lost-0: err=<nil> pending=31 Score=32
fresh after one replay freed a slot: err=<nil>
```

**候选修法**（不替修的一半拍板）：
- 在 `:1072-1080` 的确认扫描里，把 `!found` 也视为可回收——依据就是 ledger 的既有契约：条目只会因 TTL 消失，消失即越过客户端重试地平线；需要同时核对 Memory 后端（无 TTL）下 `!found` 不会出现，或给 pending 项记录 `AdmittedAtUnix` 只回收 `now - admitted > ReservationTTL` 的项（更保守，但要改 JSON 形状）。
- 背压的错误类型：永久/需人工的状态不应是 `ErrConflict`；至少文案要能区分。
- 补一个 owner-only 的对账入口或 Stats 暴露 pending 数，让运维看得见。
- 回归：上面草稿 + 原三条 RR-02 回归不回退（特别是 `TestUnconfirmedProgressBackpressureNeverEvictsProof`：TTL 内的 pending 仍不能被淘汰）。

**等级建议**：P2（可用性：单参与者永久不能计分、错误分类误导重试；没有数据损坏；触发需要 32 次孤儿事件，但一次 Redis 抖动即可批量制造）。

## §4 疑点与记录不一致

### §4.1 索引 / 记录不一致（记录质量）

1. `docs/bug/README.md:708` RR-20260909-02 行仍写"正常 Finish 新触发仍有残留，09-29 真实 Redis 已复现"，而同文件 `:73` 与 CHANGELOG v1.18.0 "session" 小节都说已随 `83c04243` 修复；表格行没有更新为"已修复"。会误导下一位 reviewer 重复登记。
2. RR-20260914-02 残余：`docs/bugfix/RR-20260914-02.md`（49 行）没有 2026-09-29 的追加段，残余修复只记在 `SERVICE-BUGFIX-2026-09-29-05.md` 与 `docs/bug/REVIEW-2026-09-14-02.md` 顶部两行；`docs/bug/README.md:673` 该 RR 行仍只写 U-0191 已发版。对照 RR-20260909-02（有"2026-09-29 正常 Finish 残留补修"段），同类残余两种写法。
3. RR-25/26/27 没有单独 `docs/bugfix/RR-20260929-2{5,6,7}.md`，只有批次记录；`ARCHIVE-2026-09-30.md:191-193` 已如实标"无单独 bugfix 文件"，`docs/bugfix/README.md` 的 RR 表也没有它们的行（只有 `:35` 的批次行）。roost-coding 允许聚合记录当主记录，这里只是与同批其他编号不一致，列出供主会话决定是否补。
4. RR-20260929-02 记录的"兼容与边界"写"缺失 ledger 的旧证明不可猜测为未应用，需对账"，但没有任何对账入口，且 §3.1 表明这不只是 legacy 升级问题。

### §4.2 chat（RR-27）：无游标最新页在普通容量淘汰后也报 Gap，记录与 CHANGELOG 只提"页内 / 尾部洞"

探针（`Retain=4`，发 10 条）：

```
latest page (no cursor, limit 10): Gap=true  HasMore=false msgs=[m7 m8 m9 m10] oldest=7 latest=10
latest page (no cursor, limit 2):  Gap=false HasMore=true  msgs=[m9 m10]
before 9 (limit 10):               Gap=true  HasMore=false msgs=[m7 m8]
no eviction, latest page:          Gap=false HasMore=false msgs=[m1 m2 m3]
```

来源是 `store.go:616-617` 的 `(!page.HasMore && selected[0].Seq > 1)`——这是有意写的"头部洞"。影响：任何曾按容量淘汰过的频道，客户端拉完整最新页时永远 `Gap=true`，并且 `store.go:540-541` 每次都 `metrics.Dropped("history.gap.<kind>")`——该指标从"游标早于保留窗口"变成"频道曾淘汰过"的计数，长期会失真；客户端若把 Gap 当"需要重同步"会空转。`Page.Gap` 的注释（`chat.go:576-578`）列的是"age pruning / cursor boundary / exhausted retained tail"，没有这一条；bugfix -05 记录与 CHANGELOG 也只写"页内 / 尾部洞"。定性：行为比记录描述的更宽的 wire 语义变化 + 指标口径变化，不是逻辑错误；建议主会话拍板是记录补写还是把头部洞从无游标页里去掉。

### §4.3 global（RR-13）：写后核验失败时 lease 已物理写入

`service.go:306/:373` 的 `checkLeaseBinding` 在 CAS 成功**之后**；Resolve→CAS 之间 route 迁移时，lease 已以调用方的 incarnation 落库，调用方却拿到 `ErrLeaseNotHolder`。后续新 route 的 Acquire 会因 `sameLeaseBinding` 不成立而覆盖，`Lease()` 对它报 lapsed，所以没有可观察到的双持有；记录也写了"核验之后仍可能迁移"。只记为疑点：调用方收到错误但留下了一条带自己 incarnation 的 stale 记录，`ReleaseLease` 按 incarnation 仍能成功释放它，口径自洽。

### §4.4 activity（RR-20260914-02 残余）两个边界

1. 无 `Intent` 的 legacy Opening 不再被回收（`service.go:792` `entry.Intent == nil → continue`），而 `Window.pending()` 把 Opening 计入容量：升级前已经孤儿化的条目会永久占名额，直到有人用同 key 再 Open 一次。记录已声明（"legacy 无计划 Opening 需同 key 恢复"），但没有说"占名额"这一后果；旧版本里它们在 grace 后会被回收。
2. `:794-800`：一条 Intent 畸形（`Key` 不符 / 非 Pending / `validateExpectedGames` 失败）或 `Activities.Create` 持续报错的 Opening 会让该 group 的 `AdvanceExpired` 整体 `return nil, err`，其他到期活动的 complete/dispatch 也跟着停。只有数据损坏或后端持续失败才触发，且 `ScanAfter` 只在超容量窗口才轮转（`AdvanceExpired` 的 `snapshot.pending() > MaxPendingActivities` 分支），正常窗口会每 tick 卡在同一条上。疑点，未做探针。

### §4.5 测试与修法耦合（session）

`rr_20260929_round2_test.go:66-71` 的暂停钩子只认 `DeleteIf`；若将来 `releaseClaim` 换回任何不经 `DeleteIf` 的删除，该回归会挂死而不是失败（§2.2）。建议钩子同时覆盖 `Delete` 或给 `<-s.release` 加超时断言，否则"红"不可见。

### §4.6 其他核对过、无问题的点

- `IPipeline.Exec` 语义变化：仓内全部调用方（`cache/ref_hmap.go` 四处、`service/mail/redis_store.go`）都把 Exec 错误直接上抛，没有依赖 nil 的写路径；没有第二个 IPipeline 实现。
- `versionstore.RedisStore.Delete` 由"CAS 到 sentinel(TTL 1s) + DEL"改为一段 Lua：去掉了原先可观察的 sentinel 窗口，语义仍是"字节相同才删"，索引 ZREM 同脚本；Cluster 下 value key 与 index key 同槽要求与之前一致。
- RR-29/31 的 `ValidateClusterKeyPrefix` 只校验 prefix，但 prefix 在完整键最前，Redis 取第一个 `{…}`，所以足够。
- RR-05 的 `decodeRing` 对 v2 JSON 忽略错误（`_ = json.Unmarshal`），但所有调用路径的 ring 都先经 `readOwner:231-236` 校验，不会把坏账本当空。

## §5 没读完 / 没跑的部分

- 没有本机 Redis Cluster：`TestBugfix6RankTaggedClusterLifecycle`、`TestBugfix7ActivityTaggedClusterLifecycle`、`TestBugfix7ActivityClusterRecoversPartialCompletion`、pipeline 三个 `cluster_true` 子项、`TestBugfix6ConditionalIndexRetirementIntegration/cluster_true` 均 SKIP，B 线记录的三 master 结果本次未复核。
- Toxiproxy 3 个 lock 用例 SKIP（不在本审范围）。
- account / mail / platform 域（含 RR-28 的 `IndexRemoveIfAbsent` 使用方、RR-33 mail GetMany）只看了与本域相邻的部分（调用方扫描），未复审。
- `docs/review/REVIEW-2026-09-30-services-12/13.md` 的强杀 / HA / Toxiproxy 专项只读了结论，没有复跑；它们声明"未确认新生产 bug"，与本审无冲突。
- 没有做性能或长稳；RR-02 每次有 pending 时多读 ≤32 个 ledger 的成本记录自己也说未压测。
- 图谱只做了 `index_status` 与引用路径的 `check_index_coverage`，结构发现用的是 `git show` + `rg`，没有 `search_graph/trace_path`。
- §3.1 的探针用 Memory ledger 手动 Delete 模拟 TTL，没有在真实 Redis 上等 TTL 到期复现（逻辑路径相同：`Ledger.Get` 返回 `!found`）。
