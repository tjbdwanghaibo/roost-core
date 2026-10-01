# B 线 Service 修复独立审计（一）：account / mail / platform

日期 2026-10-01。审计基线 `db4b7009`（main）。只读代码、只跑测试；没有改仓库文件。审计员：独立会话（与修复 agent 不同机器）。另一位审计员负责 activity / rank / match / session / chat / directory / global 与 redis driver（含 RR-20260929-34），本文不重复。

## §0 范围与方法

**范围**（B 线 2026-09-29～30 落在三个域的条目）：

| 域 | 编号 | 修复提交 |
| --- | --- | --- |
| account（`kit/service/account`） | RR-20260929-03、11、12、19、22 | `83c04243`（03/11/12）、`bdbb61bc`（19/22） |
| mail（`service/mail`、`kit/service/mail`） | RR-20260929-04、16、20、33；旧 RR-20260910-02 残余 | `83c04243`（04/16）、`bdbb61bc`（20）、`b336ce62`（RR-10 残余）、`28f8fc15`（33） |
| platform（`kit/service/platform`、demo 模板） | RR-20260929-01、23、24、28、30、32 | `83c04243`（01）、`b336ce62`（23/24）、`5f81e5c8`（28/30）、`004c8cf8`（32） |

旧残余 RR-20260909-02（session）与 RR-20260914-02（activity）不在本文范围。

**读过的记录**：`AGENTS.md`、`docs/agent-skills/roost-coding/SKILL.md`、`~/.claude/skills/roost-review/SKILL.md`；`docs/bugfix/README.md` 头部、`docs/bugfix/RR-20260929-{01,03,04,11,12,16,19,20,22,28,30,32,33}.md`、`docs/bugfix/SERVICE-BUGFIX-2026-09-29{,-03,-04,-06,-07}.md`、`docs/bug/REVIEW-2026-09-29-services{,-02,-03,-04,-07}.md`（根因行）、`docs/review/SERVICE-REVIEW-COMPLETION-2026-09-29.md`、`docs/review/ARCHIVE-2026-09-30.md` §3、`CHANGELOG.md` `## [v1.18.0]` 的“行为与 API 变化”与“Fixed — Service”、`docs/bugfix/evidence/service-bugfix-20260929/results.json`（修前 18 个 FAIL 名单）。

**读过的代码**（全文）：`kit/service/account/{service,create_role,types,identity,redis_store,account_rpc}.go`；`service/mail/{service,mailbox,store,redis_store,send_intent}.go` 与 `types.go` 相关段；`kit/service/platform/{service,admin,types,redis_store,server_run}.go`、`platform_mod.go` Init 段；`kit/service/directory/store.go` Reserve/Commit/Cancel；`versionstore/memory_store.go` Update/Create/DeleteIf、`versionstore/redis_store.go` IndexRemoveIfAbsent/DeleteIf；`kit/mods/service_servicemods.go` ValidateClusterKeyPrefix；`redis/driver/pipeline.go` Exec/Discard；`demo/internal/service/platform/collaborators.go.tmpl` grantDeliverer；各条记录点名的回归测试源文件。

**codebase-memory**：项目 `Users-whb-roost-roost-core` status=ready，generation `2026-09-30T11:28:30Z`，`check_index_coverage` 对 `service/mail`、`kit/service/{account,mail,platform}` 四个 scope 均 `no_recorded_issue`。HEAD `db4b7009` 提交时间 `2026-09-30T13:35:41Z` 晚于 generation，但 generation 之后没有提交触及上述四个目录（`git log --since=2026-09-30T11:28:30Z -- service/mail kit/service/... versionstore kit/mods redis/driver kit/service/directory` 为空），所以图谱与源码一致；结论均以 `cat -n` 的当前源码行为准。

**跑过的命令与结果**（全部在 main 工作树 `/Users/whb/roost/roost-core`，`GOWORK=off`，Go 1.27.0 darwin/arm64；Redis 为 `/Users/whb/.roost-it/roost-dataengine-it/env.sh` 的隔离实例 `127.0.0.1:17379`，各用例自带时间戳前缀，未 FLUSH）：

| 命令 | 结果 |
| --- | --- |
| `go test -count=1 -race ./service/mail ./kit/service/account ./kit/service/mail ./kit/service/platform` | 4 包 ok，exit 0 |
| `REDIS_ADDR=127.0.0.1:17379 go test -tags integration -count=1 -p 1 -race` 同 4 包 | 4 包 ok；但 `-v` 显示 13 个 Redis 用例因 `ROOST_REDIS_TEST_ADDR` / `ROOST_REVIEW_CLUSTER` 未设而 SKIP（platform 的 RR-28 用例在内）——**只设 `REDIS_ADDR` 不足以跑全这些包的 Redis 用例** |
| 同上并补 `ROOST_REDIS_TEST_ADDR` `ROOST_REVIEW_REDIS` `ROOST_REVIEW3_BACKEND=redis` `ROOST_REVIEW4_BACKEND=redis` | 4 包 ok，330 个 `=== RUN`，只剩 3 个需要真实 Cluster（`ROOST_REVIEW_CLUSTER`）的 SKIP：`TestIntegrationEnvelopeBatchAcrossSlots/cluster_true`、`TestIntegrationMailModClusterPagination`、`TestBugfix6PlatformTaggedClusterControl`。Account/Mail round3 与 Platform RR-23 用例因此在真实 Redis 上执行并通过 |
| `go test -tags integration -count=1 -race -run 'Retire\|RemoveIfAbsent\|ClusterKeyPrefix\|ConditionalDelete' ./versionstore ./kit/mods` | ok；`TestBugfix6ConditionalIndexRetirementIntegration/cluster_true` SKIP（无 Cluster） |
| 红测探针（§2）：临时 worktree `scratchpad/audit-svc1`（`db4b7009`），逐条回退根因行后跑点名回归，之后 `git checkout --` 复原、`git status` 干净，最后 `git worktree remove` | 12 组探针全部变红，红文本见 §2 |
| RR-16 补充探针（§2.3）：真实 Redis 同 RequestID 恢复（有 / 无附件）＋ `sameSendIntent` 对 nil / 空切片的脆弱性 | Redis 两例 PASS；脆弱性探针复现 `ErrConflict` |

原始日志在 `scratchpad/audlog-svc1/`（`main-unit-race.txt`、`main-integ-v.txt`、`main-integ-allenv-v.txt`、`main-versionstore-mods.txt`、`probes-summary.txt`、`probe-*.txt`、`probe-rr16-audit.txt`、`probes.sh`、`zz_probe_audit_rr16_test.go`），不入库。

## §1 逐条核对

列说明：代码核对 = 记录里的根因 / 改动 / 行为是否与 `git show <修复提交>` 和当前源码一致；回归 = 记录点名的测试在 main 上的结果（上表“补全环境变量”那次）。

| 编号 | 域 | 记录声称 | 代码核对结论 | 回归测试（文件·函数，main 结果） | 结论 |
| --- | --- | --- | --- | --- | --- |
| RR-20260929-03 | account | 名字 owner 只用 accountID，跨服复用已提交名字并在失败时释放他人名字；改为 owner=slotKey，已提交同 owner claim 拒绝复用，失败只清理自身 slot | **一致但措辞过时**：`83c04243` 确实把 owner 改成 slotKey（diff 可见），但 `bdbb61bc` 又改成 `slotKey + "/" + creationID`（`create_role.go:78`）。当前行为仍满足声称：Reserve 遇 `ErrKeyTaken` 只 `releaseCreationSlot(…, false)`（`:81-85`，DeleteIf 要求 `!Admitted`，`:186`）；同 owner 已提交但 token 对不上 → `ErrNameTaken` 不 Commit / Release（`:100-102`）。RR-03 记录与 CHANGELOG L55 仍写“slotKey”，没有指向 RR-19 的改写 | `kit/service/account/rr_20260929_round1_test.go·TestReviewCrossServerRoleRollbackKeepsExistingName`、`bugfix_recovery_test.go:13·TestCommittedLegacyNameOwnerIsNeverCompensated`：PASS（Memory；round1 不切 Redis） | 通过（记录措辞过时，见 §4.7） |
| RR-20260929-11 | account | channel/openID 冒号边界未编码；转义 channel 的 `%`/`:`，读旧 ID 只有 Channel/OpenID 与 verifier 一致才复用，不匹配拒绝 | **一致**：`types.go:186-189` 对 lower+trim 后的 channel 用 `NewReplacer("%","%25",":","%3A")`，openID 原样——channel 不再含 `:`，首个 `:` 分隔唯一，编码单射；`service.go:166-176` legacyID 不同时 Get 旧账号并 `sameVerifiedIdentity` 才复用；`:181-183` 存量身份不匹配拒绝 `ErrIdentityInvalid` 不合并 | `rr_20260929_round2_test.go:11·TestReviewVerifiedIdentityEncodingIsInjective`、`bugfix_recovery_test.go:84·TestLegacyVerifiedIdentityRetainsAccountAndRefusesCollision`：PASS | 通过（错误码语义见 §4.2） |
| RR-20260929-12 | account | slot 尾写 Update 返回 error 被当作未落盘，删角色 / 名字再用旧版本删 slot；改为失败先 Get 核对 PlayerID/account/server，已应用返回成功，读失败返回角色 + unknown 错误 | **一致但措辞过时**：当前 `create_role.go:165-173`：err 后 `Slots.Get`，readErr → `return role, errors.Join(err, "slot outcome unknown")`；`found && Creation.ID==plan.ID && PlayerID==role.PlayerID` → 走 Accepted 返回成功；否则 `return role, err`。核对字段是 CreationID+PlayerID（account/server 在入口 `:47` 已核），不是记录写的“PlayerID/account/server”（那是 `83c04243` 版本）。任何分支都不再删角色 | `rr_20260929_round2_test.go:50·TestReviewLostSlotReplyCannotDeleteCommittedRole`、`bugfix_recovery_test.go:60·TestUnknownSlotOutcomePreservesRoleAndName`：PASS | 通过（记录措辞过时，见 §4.7） |
| RR-20260929-19 | account | slot 持久保存 `RoleCreation`（随机 ID、预分配 PlayerID、名字、Claim、Admitted）；同名重试共享计划；Admitted 后不删可能已提交的角色；只有确定的建角前名字冲突 / foreign ID 才补偿；未发布角色被 SelectRole/ValidateSession/UpdateProfile 拒绝；旧空 slot 返回 ErrConflict；`New` 要求 Slots 支持 DeleteIf | **一致**：`types.go:268-295`；`create_role.go:15-72`（insert-only 计划、同名续行 `:68-71`、完成后同名幂等返回 `:50-60`、旧空 slot `:65-67`）；`:74-176` resume（owner 带 creationID、admission CAS `:103-122`、Roles.Create 未创建必 Get 并核 CreationID `:132-147`、Commit/尾写错误保留计划 `:149-173`）；`:178-192` 两个补偿入口均经 DeleteIf 校验 CreationID/PlayerID/Admitted；`:194-207` roleReady；`service.go:96-100` 构造门禁。记录“步骤 1～6”与源码逐项对得上 | `rr_20260929_round3_test.go` 10 个函数（`:44,113,153,190,210,259,294,323,340,350`）＋ `create_role_race_promises_test.go`、`create_role_tail_test.go`：Memory 与 `ROOST_REVIEW3_BACKEND=redis` 真实 Redis 均 PASS | 通过（恢复入口缺口见 §4.1） |
| RR-20260929-22 | account | UpdateProfile 保存与返回分离、ValidateSession 返回私有 Profile、CreateRole 重放 / 恢复入口返回 clone | **一致**：`types.go:253-257 clone`；`service.go:316`（ValidateSession）、`:354-355`（UpdateProfile 存 `append(nil,…)`、返回 `clone()`）；`create_role.go:59,148,175` | `rr_20260929_round3_test.go:210·TestReview3ProfileOutputCannotMutateRoleWithoutCAS`（update/validate/create_retry）：PASS（Memory + Redis） | 通过 |
| RR-20260929-04 | mail | 条目无正文到期信息，列表隐藏正文但不回收 unread/容量；新增 `Entry.EnvelopeExpiresAtUnix`，List/Deliver 的 CAS 内按已知期限回收条目与 unread 并“写 evicted 墓碑”，在途未结算 claim 保留；期限 0 不推测 | **基本一致**：`mailbox.go:51` 字段；`:80-102 expireEntries`（期限 ≤0 或未到跳过；`ClaimToken!="" && Status!=Claimed` 保留；Unread 同步递减；`Evicted++`）；`service.go:387-391` deliver 在 full() 判断前回收、`:404-407` 写入期限、`:335` deliverDirect 传 `ExpiresAtUnix`、`:354-362` 导出 Deliver 读信封取期限；`:493-508` List 先在 clone 上判断再 CAS 回写。**“写 evicted 墓碑”不准确**：只递增 `Mailbox.Evicted` 计数（`:95`），不写 `SettledClaims` 墓碑；对已 Claimed 条目过期删除也不留墓碑——与 `evictSettledClaims` 在同一时刻（`:288`）丢墓碑的规则一致，不构成缺陷 | `rr_20260929_round1_test.go:10·TestReviewExpiredMailboxMakesRoom`：PASS（Memory） | 通过（措辞与成本见 §4.3/§4.7） |
| RR-20260929-16 | mail | ledger 先提交但未存正文意图；`SentRecord.Intent` 保存独立克隆，同 RequestID 恢复固定 MailID/正文/期限，读回确认一致后投递；未投递且已过期拒绝；已投递的 TTL 过去返回原 intent；旧 ledger 无 Intent 仍 ErrConflict | **一致**：`store.go:51-53`；`service.go:192-196`（`intent := envelope.clone()`）、`:283-325 replaySend`（Intent.ID==MailID 才用；DeliveredAtUnix≠0 直接返回；`Expired` → `ErrMailInvalid`；Create 后 Get 回读并 `sameSendIntent` 比较；否则 `ErrConflict`）；`send_intent.go:7` 为 `reflect.DeepEqual` | `rr_20260929_round2_test.go:21·TestReviewTransientEnvelopeFailureCanRetrySameRequest`：PASS（仅 Memory fake）。审计补跑真实 Redis：有 / 无附件两例 PASS（§2.3） | 通过；**邻接 P3 缺陷**：`sameSendIntent` 用 `reflect.DeepEqual` 区分 nil / 空切片（§3.1） |
| RR-20260929-20 | mail | `CancelClaim(ctx, playerID, mailID, token, attempts)`，CAS 内同时匹配 Token 与 ClaimAttempts；旧代次返回 false 不清新 Deadline；缺 attempts 的旧 JSON 拒绝；ReserveClaim 对负数 / MaxInt32 拒绝；RPC/样例/模板同步 | **一致**：`mail.go:71` 接口；`service.go:986-988`（`attempts<=0` → `ErrRequestInvalid`）、`:1009-1013`（Token 且 Attempts 都匹配才清）、`:843-845` 代次守卫；`mail_rpc_gen.go:218-222` wire `attempts`、`:308` handler、`:454-460` client；调用方 `demo/game/controllers/player/claim_mail.go.tmpl:65,79`、`kit/service/examples/split/consumer.go:78` 都传 `claim.Attempts`；`kit/service/mail/alias.go` 别名同步。CHANGELOG L10 列出了 API 变化与滚动顺序 | `rr_20260929_round3_test.go:40·TestReview3OldCancelCannotReleaseNewClaimAttempt`、`:107·TestCancelClaimGenerationCrossesLocalAndBusTransports`、`:152·TestCancelClaimLegacyWireIsRejected`、`:177·TestClaimAttemptGenerationNeverWraps`：PASS（Memory + Redis） | 通过 |
| RR-20260929-33 | mail | 普通 prefix 下多 key MGET 报 CROSSSLOT；`GetMany` 改为既有 `IPipeline` 排队单 key GET、一次 Exec、逐 future 取结果，ErrNil 才是缺失，空字节 / 坏 JSON / WRONGTYPE 报错；`envelopeClient` 窄接口 `MGet` → `Pipeline()`；键 / JSON / TTL / EnvelopeStore / RPC 不变；不强制共同 tag | **一致**：`redis_store.go:100-104`（窄接口）、`:199-250`（空批次 / MaxPageSize / 空 id 先校验；`defer pipe.Discard()`；逐 future `errors.Is(err, ErrNil)` 才 continue，其余错误直接返回 nil map）；`store.go:21-29` 注释改为“batch per node”；`kit/service/mail/mail_mod.go:85` 经 `NewRedisStores` 装配，`redis/client.go:70` IRedis 自带 `Pipeline()`。driver 层 `pipeline.go:101-137` Exec 先填全部 future 再处理聚合错误（RR-34，另一审计员范围） | `service/mail/redis_store_test.go`（fake pipeline 单元）、`batch_pipeline_integration_test.go:20·TestIntegrationEnvelopeBatchAcrossSlots`（`cluster_false` PASS，`cluster_true` SKIP）、`kit/service/mail/batch_cluster_integration_test.go:23·TestIntegrationMailModClusterPagination`（SKIP，需 `ROOST_REVIEW_CLUSTER`）、`redis_integration_test.go` 单机 PASS | 通过（本机只验单机；Cluster 场景依赖 B 线证据，见 §5） |
| 旧 RR-20260910-02 残余 | mail | 未领取直接删除的邮件被淘汰后重投复活；复用 SettledClaims 墓碑加 `deleted`，淘汰 Entry 时保留删除身份与 envelope expiry；Deliver 不复活、Reserve/Commit 维持 Missing；未知期限的删除证明不按 legacy 计数丢弃；容量不足拒绝投递 | **一致**：`mailbox.go:124-125` 字段；`:247-264` evict 对 `Deleted` 或带 token 的条目写墓碑（`Deleted = Status==Deleted && ClaimToken==""`，期限取 `ClaimEnvelopeExpiresAtUnix` 否则 `EnvelopeExpiresAtUnix`）；`:185-199` deliver 命中墓碑按 Deleted/Claimed 回答且不加入；`service.go:808-811`、`:916-919` Reserve/Commit 命中 Deleted 墓碑 → `ErrMailMissing`；`:295-299` legacy 计数淘汰排除 `Deleted`；`:417-421` 墓碑溢出拒绝投递 `ErrClaimHistoryFull` | `bugfix_deleted_identity_test.go:20·TestBugfix4DeletedUnclaimedMailMustNotResurrect`（Memory + Redis）、`:51·TestDeletedTombstonesAreBoundedWithoutForgettingUnknownExpiry`：PASS | 通过（未知期限墓碑不老化的取舍见 §4.4） |
| RR-20260929-01 | platform | backoff 耗尽不等于外部调用停止、旧成功回包无条件写 delivered；Order 持久化单调 `AttemptSequence`/`PendingAttempts`，结算 / 重开拒绝在途，回包按 attempt 归档；新增 owner-only `ResolvePendingAttempts`；改动 service.go/admin.go/types.go | **一致**：`types.go:244-247`；`service.go:494-499`（claim 内分配序号并压入 pending，序号溢出拒绝）、`:548-581`（完成 CAS：Settled 或序号不在 pending → 不写，返回 `ErrOrderSettled` / `ErrConflict: stale delivery completion`）、`:594-625` recordFailure 同样按 generation 归档；`admin.go:109-111`、`:178-180`（Reopen/Settle 要求 pending 为空）、`:206-232` ResolvePendingAttempts（只对 Exhausted、clone 后清空）。`git show 83c04243 --stat` 正是这三个文件 + 两个测试 | `rr_20260929_round1_test.go:10·TestReviewSettledOrderSurvivesLateDelivery`、`bugfix_reconciliation_test.go:10·TestReconciledSettlementRejectsStaleDeliveryCompletion`：PASS | 通过 |
| RR-20260929-23 | platform | 外部发货未知错误清除 pending 允许再发货 / 结算；新增 `ErrDeliveryNotApplied`，只有它才移除 pending 并按预算重试；其他错误保留 pending 进入 exhausted；退避到期仍有在途证明同样转人工；迟到成功仍能完成原 generation | **一致**：`types.go:73-75` 哨兵（`errors.New`，非 errcode，不出 RPC）；`b336ce62` diff 显示旧代码 `recordFailure` 无条件 `removeAttempt`（与 REVIEW-04 指的“598 行无条件 removeAttempt”相符）；现 `service.go:605-607` 只在 NotApplied 时移除、`:614-617` 非 NotApplied 直接 Exhausted；`:471-477` Due 但 pending 非空 → Exhausted 不再调 Deliverer；迟到成功走 `:553-571`（序号仍在 pending 则可从 Exhausted 转 Delivered）。接口注释 `:34-37` 同步 | `bugfix_external_outcome_test.go:50,68,88,118,132`（Memory + `ROOST_REVIEW4_BACKEND=redis`）、`race_test.go:35·TestElapsedBackoffDoesNotStartAnotherExternalGrant`：PASS | 通过（无独立 bugfix 文件，ARCHIVE 已注明） |
| RR-20260929-24 | platform | 重复 HandleCallback 构造 receipt 时 clone Order（含错误分支）；修改 PendingAttempts 不再绕过 MemoryStore CAS | **一致**：`service.go:377 existing.Value.clone()`，错误分支 `:384-387` 返回同一 clone；`Order.clone` 只深拷 `PendingAttempts`（`types.go:275-278`，Order 没有别的引用字段）；`AttemptDelivery` 各出口 `claimed/result/failed` 均 clone；`Order()` `:666` clone。`ReopenDelivery/SettleOutOfBand` 返回 `current` 不 clone，但 CAS 内已要求 `PendingAttempts` 为空，len 0 的共享底层数组无法通过返回值改写存储 | `bugfix_external_outcome_test.go:132·TestBugfix4CallbackReceiptMustOwnPendingProof`：PASS（Memory + Redis） | 通过（未做红测） |
| RR-20260929-28 | platform | 迟到 ghost 退休的无条件 ZREM 删掉新 paid 订单 pending 入口；`RetirePending` 改 `IndexRemoveIfAbsent`，值 key EXISTS 才决定，同一段 Lua；原无条件 API 保留 | **一致**：`5f81e5c8` diff `IndexRemove → IndexRemoveIfAbsent`（`redis_store.go:140`）；`versionstore/redis_store.go:253-281` 单脚本 `EXISTS KEYS[1] → ZREM KEYS[2]`，要求固定索引；`IndexRemove/IndexRemoveIn :219-245` 保留并补注释。两键共用 prefix，Cluster 同槽由 RR-30 的 tag 校验保证 | `bugfix_pending_retirement_test.go:48·TestBugfix6RetirementCannotHideLatePaidOrder`（reconstruct false/true，真实 Redis）、`:110·TestBugfix6PendingMaintenanceControls`：PASS；`versionstore/bugfix_index_retirement_test.go·TestBugfix6ConditionalIndexRetirementIntegration/cluster_false`：PASS | 通过 |
| RR-20260929-30 | platform | “包含左括号”判断替换为共享 `mods.ValidateClusterKeyPrefix`；空 / 未闭合首 tag 启动失败；空首对后跟有效 tag 也拒绝 | **一致**：`platform_mod.go:131-133`；`kit/mods/service_servicemods.go:51-61` 取第一个 `{` 与其后第一个 `}`，要求 `end > 0`，与 Redis hash tag 规则一致（`{}:{valid}` 拒绝、`{{nested}` 接受，kit/mods 表驱动用例覆盖）。本域没有其他多 key 原子写：account 的 directory state 无 RedisIndex（`kit/service/directory/redis_store.go` 无 Index），mail 全是单 key，所以“只有 platform 校验”在三个域内是完整的 | `bugfix_pending_retirement_test.go:187·TestBugfix6PlatformClusterRejectsMalformedTag`（4 个前缀）PASS；`:202·TestBugfix6PlatformTaggedClusterControl` SKIP（需 Cluster）；`kit/mods/service_servicemods_test.go·TestClusterKeyPrefixUsesFirstRedisHashTag` 16 子用例 PASS | 通过 |
| RR-20260929-32 | platform（demo 模板） | `grantDeliverer.Deliver` 先 HGet 已有字段核身份恢复，只有 ErrNil 才查商品并经单键 Lua 原子首写，始终返回 durable winner 并核身份；读错 / 写后丢回复 / 坏内容不带 `ErrDeliveryNotApplied`；未绑定 / 缺商品 / 编码失败标 NotApplied；新增生成测试并接入 scaffold | **一致**（源码核对）：`demo/internal/service/platform/collaborators.go.tmpl:118-123` 脚本 `HGET→存在返回，否则 HSET 并返回 ARGV[2]`；`:132-173` Deliver（`client==nil`/未知商品/Encode 失败 wrap NotApplied；HGet 非 ErrNil 错误与 Eval 错误原样返回；回包经 `validateStoredGrant :175-186` 核 OrderID/PlayerID/ProductID/PaidAtUnix，不匹配返回错误不覆盖）；`codegen/internal/roost/demo.go:612` 把 `purchase_delivery_test.go` 列入 scaffold 步骤 | 记录声称的 26 叶子生成消费者测试需要生成工程，本机未执行（§5）；`go test ./codegen/...` 未在本审计运行 | 通过（源码核对；执行证据沿用 B 线） |

**统计**：16 条——通过 15（其中 RR-03、RR-12 记录措辞过时，RR-04 一处措辞不准，均不影响行为）；缺陷 1（RR-16 邻接的 P3，§3.1）；无需要单列为“疑点”的条目，疑点作为注记放在 §4。

## §2 红是否真红

方法：在 `scratchpad/audit-svc1`（`git worktree add … db4b7009`）用 `probes.sh` 把每条修复的根因行回退成修前形态（不是整文件回退，避免被后续提交的签名变化拖垮编译），跑记录点名的回归，记录输出，`git checkout --` 复原，最后 `git status --short` 为空。环境变量同 §0 第三行。每组命令形如 `go test -count=1 [-tags integration] -run '^Test…$' ./pkg`。

| 探针 | 回退的根因行 | 结果 / 红文本 |
| --- | --- | --- |
| mail-RR20 | `service.go:1009` 去掉 `\|\| entry.ClaimAttempts != attempts` | FAIL `rr_20260929_round3_test.go:62: old A cancel released B: cancelled=true Bdeadline=1700000061 C={MailID:mail-1 Token:token-1 … Attempts:3 …} err=<nil>` |
| mail-RR16 | `service.go:289` 条件改 `if false`（不用 Intent 恢复） | FAIL `rr_20260929_round2_test.go:28: store recovered but same send cannot recover: mail: conflict: send "send-1" names missing mail mail-1` |
| mail-RR04 | `mailbox.go:85` 条件前加 `true ||`（expireEntries 不回收） | FAIL `rr_20260929_round1_test.go:26: visible=0 unread=200` / `:30: expired invisible mails still block delivery: mail: mailbox is full: player 1 holds 200 unread mails` |
| mail-RR10 残余 | `mailbox.go:247` 去掉 `\|\| entry.Status == StatusDeleted`（只给带 token 的条目留墓碑） | FAIL `bugfix_deleted_identity_test.go:47: deleted mail revived after retention: token="token-1" attachment="reward" err=<nil>` |
| account-RR11 | `types.go:188` 去掉 Replacer | FAIL `rr_20260929_round2_test.go:30: account: identity is invalid: stored account identity does not match verified identity`（第二个身份被判为与第一个账号冲突，即两个身份映射同一 ID） |
| account-RR12 | `create_role.go:165-173` 换成“err 则 `Roles.Delete` 并返回空 Role”（修前补偿形态） | FAIL `bugfix_recovery_test.go:70: unknown result omitted recovery identity: {… PlayerID:0 …} lost write reply`；`rr_20260929_round2_test.go:61: applied slot was not reconciled: {… PlayerID:0 …} injected reply lost after slot commit` |
| account-RR19 | `create_role.go:149-151` Commit 出错时 Cancel claim + `Roles.Delete` + `releaseCreationSlot(…, true)`（修前“全部补偿”形态） | FAIL `rr_20260929_round3_test.go:134: committed name burned: name={Key:hero … Owner:test:name-lost@1/… State:committed …} slot={… Version:0} slotFound=false retry=account: role name is taken: "Hero"` |
| account-RR22 | `service.go:316` `role.Value.clone()` → `role.Value` | FAIL `rr_20260929_round3_test.go:253: output mutated stored role: value="Xafe" version=3/3`（子用例 `validate`） |
| platform-RR23 | `service.go:605-607` 无条件 removeAttempt；`:614` 去掉 `!errors.Is(cause, ErrDeliveryNotApplied) \|\|`；`:471` `case len(PendingAttempts) > 0` → `case false` | FAIL ×3：`bugfix_external_outcome_test.go:64: external grant=1 … pending=[] state=exhausted; refund admitted: state=settled err=<nil>`；`:84: lost external grant reply caused 2 grants; receipt={… State:delivered Attempts:2 …}`；`race_test.go:48: pending call was retried: {… PendingAttempts:[1] …}` |
| platform-RR01 | `admin.go:178-180` 去掉 Settle 的 pending 检查；`service.go:553` 完成 CAS 的 Settled/序号门禁改 `if false` | FAIL ×2：`rr_20260929_round1_test.go:36: <nil> <nil>`（Settle 成功、迟到成功也成功）；`bugfix_reconciliation_test.go:45: <nil>`（已结算订单被迟到回包改写） |
| platform-RR28（真实 Redis） | `redis_store.go:140` `IndexRemoveIfAbsent` → `IndexRemove` | FAIL（reconstruct_false / true 两子用例）`bugfix_pending_retirement_test.go:102: pending=[] stored_state=reserved due=true attempts=1 pending_attempts=[] grants=0` / `:104: late paid order lost its retry index and never received its goods` |
| platform-RR30 | `platform_mod.go:131-133` 换回 `strings.Contains(prefix, "{")` 判断 | FAIL：`{}:platform`、`{platform`、`{}:{valid}:platform` 三个子用例 `expected configuration rejection, got <nil>`；`platform` 子用例也红，但只是因为探针错误文本不含 `platform.key_prefix`（断言要求点名配置键），不计入 |

每个域都超过了“抽 2 条”的要求（account 4、mail 4、platform 4）。**没有做红测的**：RR-24（源码判定，aliasing 类）、RR-33（单机 Redis 上 MGET 本来就能跑，没有 Cluster 就造不出 CROSSSLOT）、RR-32（需要生成工程）。

### §2.3 RR-16 补充探针（`zz_probe_audit_rr16_test.go`，跑完即删）

- `TestAuditRR16RedisRetrySameRequestNoAttachment` / `…WithAttachment`：真实 Redis（`NewRedisStores` + 首次 `Envelopes.Create` 注入失败），同 RequestID 第二次 Send 成功、第三次返回同一 ID、恢复出的邮件可 ReserveClaim——**PASS**。RR-16 记录没有声称 Redis 验证，这里补上了。
- `TestAuditRR16DeepEqualRejectsEmptyVsNilAttachment`：EnvelopeStore 的 `Get` 把 nil `Attachment` 规范成 `[]byte{}` 后，同 RequestID 重试返回 `mail: conflict: reserved envelope differs or is missing`——**FAIL（即缺陷复现）**，见 §3.1。

## §3 确定的缺陷

### §3.1 `sameSendIntent` 用 `reflect.DeepEqual` 比较信封，nil 与空切片被当成“内容不同”，使 RR-16 的同请求恢复在自定义 EnvelopeStore 上永久 `ErrConflict`（P3，潜伏）

- **现象**：Send 的首次 `Envelopes.Create` 失败后，同 RequestID 重试走 `replaySend`，`Create` 成功但紧接着的 `Get` 回读被 `sameSendIntent(stored, envelope)` 判为不同，返回 `ErrConflict: reserved envelope differs or is missing`；此后每次重试都在同一处失败（信封已存在，仍比较失败），这条发送既不能完成也不能被另起 RequestID 安全替代（ledger 已占）。
- **触发条件**：`EnvelopeStore` 的 `Get` 返回的切片字段（`Attachment`、`Recipients`）为空但非 nil，而 `SentRecord.Intent` 经 JSON `omitempty` 往返后为 nil（或反之）。内置 Redis 存储两边都走 JSON，nil 对 nil，所以**不触发**（§2.3 Redis 两例 PASS）；触发面是 `store.go:14-30` 鼓励的自定义 `EnvelopeStore`（例如文档库解码把缺失二进制字段还原成空切片）。定级 P3 是因为当前仓内没有触发路径，不是因为后果轻——触发时是一条邮件永久卡死。
- **根因**：`service/mail/send_intent.go:7`（`db4b7009`）`func sameSendIntent(a, b Envelope) bool { return reflect.DeepEqual(a, b) }`；`Envelope.clone()`（`types.go:351-356`）用 `append([]T(nil), …)` 保持 nil，JSON 解码也保持 nil，因此 intent 侧总是 nil，而比较语义却要求存储侧也精确为 nil。
- **会红的测试草稿**（已在 §2.3 跑红，放在 `service/mail` 包内）：

```go
type nilToEmptyStore struct{ EnvelopeStore }
func (s nilToEmptyStore) Get(ctx context.Context, id string) (Envelope, bool, error) {
	e, ok, err := s.EnvelopeStore.Get(ctx, id)
	if e.Attachment == nil { e.Attachment = []byte{} }
	return e, ok, err
}
func TestSameRequestRecoveryToleratesEmptyVsNilSlices(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Envelopes = &reviewEnvelopeOnceFailure{EnvelopeStore: nilToEmptyStore{c.Envelopes}}
	})
	req := directTo(1)
	if _, err := h.service.Send(context.Background(), req); err == nil { t.Fatal("fixture must fail the first envelope write") }
	if _, err := h.service.Send(context.Background(), req); err != nil {
		t.Fatalf("semantically identical envelope refused: %v", err) // 现在: mail: conflict: reserved envelope differs or is missing
	}
}
```

- **候选修法**：把 `sameSendIntent` 改成逐字段比较，标量用 `==`，`Recipients` 用 `slices.Equal`，`Attachment` 用 `bytes.Equal`（两者对 nil / 空一视同仁）；保留对 ID、期限、Audience/Scope 的严格比较，不放宽“替换正文”的拒绝语义。不要改成只比 ID。
- **等级**：P3（潜伏；自定义 store 才触发；后果是单条发送永久卡死而非数据损坏）。

## §4 疑点与记录不一致

需要维护者拍板或只是文档层面的事；都不是我能用源码判定为缺陷的。

1. **RR-19 的 pending slot 没有任何运维释放入口**。`create_role.go:178-192` 是仅有的两个删除入口，都只在“确定的建角前名字冲突（且未 Admitted）”或“foreign allocator ID”时触发。一旦 Admitted 后名字租约（30s）过期被别的 owner 提交，slot 永久 pending：同名重试 `ErrNameTaken`、换名 `ErrRoleLimit`（`:68-70`），该账号在该区服再也建不了角色，而角色记录可能存在也可能不存在。记录（RR-19“新的 pending 角色若名字预约过期后被别人提交，保留 intent 并拒绝角色登录，需要业务对账”）和测试 `TestPendingRoleNameConflictRetainsRecoveryProof` 都把这钉成设计；但 platform 同类问题给了 owner-only `ResolvePendingAttempts`，account 没有对等入口，运维只能直接改 Redis。建议维护者决定是否补一个 owner-only、带 note 的“放弃建角计划”入口（校验 CreationID、无角色或角色未发布）。
2. **RR-11 碰撞时的错误码**：旧账号 `a:b:c` 属于身份 A（channel `a:b`/openID `c`）时，合法身份 B（channel `a`/openID `b:c`）登录得到 `ErrIdentityInvalid`（560101，“identity is invalid”），客户端会把它当成凭证格式错误而不是“需要人工拆分”。另外 `identity.go:40-45 Verified.Validate` 不 trim，而 `AccountID()` trim：verifier 返回全空白 OpenID 会通过校验并折叠成 `"<channel>:"`——这是修前就有的、依赖 verifier 守信的边界，不属于 B 线引入。
3. **RR-04 的两处成本 / 口径**：(a) 导出的 `Service.Deliver`（`service.go:354-362`）现在每个 (mailbox, mail) 多一次 `Envelopes.Get` 只为取期限；广播 Deliverer 对 N 个邮箱 fanout 就是 N 次 GET，记录没有写这项成本（Deliverer 已拿到完整 Envelope，可以考虑一个带期限的入口）。(b) `Summary`（`:1047-1059`）不做回收，`Unread` 在 List/Deliver 之前都是过期前的数。(c) 记录“写 evicted 墓碑”实为 `Evicted` 计数。
4. **旧 RR-10 残余的墓碑老化规则**：`mailbox.go:295-299` 对 `Deleted` 且期限未知的墓碑不做计数淘汰，而 `:417-421` 在 `SettledClaims > 200` 时拒绝一切新投递（`ErrClaimHistoryFull`）。期限未知的删除墓碑只来自“投递时信封已不存在”或升级前的老条目；一个老邮箱升级后反复删除旧未领取邮件并被容量淘汰超过 200 次，就会永久拒收。记录（SERVICE-BUGFIX-04 “容量不足拒绝新投递”）承认了这个取舍，但没有给运维处置入口；同样请维护者定夺。
5. **过期且已 Cancel 的在途 claim 永久占容量**：`TestReview3ObserveExpiredCancelledClaimsRetainCapacity` 把“200 封 Reserve+Cancel 后过期 → `ErrMailboxFull`，只能显式 Delete”钉为观察；REVIEW-03 第 73 行写明“本轮不登记为新 bug”。理由（Cancel 不证明外部未发奖）成立，但缺的是对账指标 / 处置流程，记录也这么说。列在这里是为了不让下一轮再当新发现。
6. **RR-33 的 Cluster 场景本机未验**：三个 `ROOST_REVIEW_CLUSTER` 用例 SKIP；B 线证据在 `docs/bugfix/evidence/service-bugfix-20260929-08/`。单机 Redis 上 MGET 与 pipeline 都能过，所以此条无法在本机做红测。
7. **记录与代码 / 记录之间的不一致**（都不改变行为）：
   - `docs/bugfix/RR-20260929-03.md` 与 `CHANGELOG.md` v1.18.0 “Fixed — Service / account” 第一条仍写“nameOwner 使用 account/server slotKey”，`bdbb61bc` 之后实际是 `slotKey/creationID`（`create_role.go:78`）；RR-19 记录写对了，但 RR-03 没有加“已被 RR-19 改写”的指向。
   - `docs/bugfix/RR-20260929-12.md` “先 Get 核对 PlayerID/account/server”描述的是 `83c04243` 版本；当前核对 CreationID+PlayerID（`create_role.go:170`），account/server 在 `:47`。
   - `docs/bugfix/RR-20260929-04.md` / CHANGELOG “写 evicted 墓碑”：实为计数。
   - RR-23 / RR-24 / 旧 RR-10 残余没有独立 `docs/bugfix/RR-*.md`，只在 `SERVICE-BUGFIX-2026-09-29-04.md`；ARCHIVE 已注明“无单独 bugfix 文件”，`docs/bugfix/README.md` 的旧 RR-20260910-02 行（L394）仍指向 09-10 的原 bugfix，没有链到 09-29 的残余修复。
   - `docs/bug/README.md` 对 RR-01～10、11～18、19～22 的表格列头仍是“结论（未修复/未实施）”，靠上方加粗段落说明“已修复”；逐编号状态只有 `docs/review/ARCHIVE-2026-09-30.md` §3 是一处可查的真值。两个 README 的索引行内容本身与记录一致。
   - 记录里的“已验证”在三个域内都能对上包内具名测试（§1 表），没有发现“记录点名但包里不存在”的测试；但 RR-01/03/04/11/12/16 六条共用一段模板文字（“修前证据保留在第一轮/第二轮……”），各自的“未验证项”只有一行“兼容与边界”，比 RR-19/20/33 薄。
8. **跑这些包的 Redis 用例需要 5 个环境变量**（`REDIS_ADDR`、`ROOST_REDIS_TEST_ADDR`、`ROOST_REVIEW_REDIS`、`ROOST_REVIEW3_BACKEND`、`ROOST_REVIEW4_BACKEND`），任务书里只给了 `REDIS_ADDR`；只设它时 platform 的 RR-28 回归和 account/mail 的 round3 Redis 变体都是静默 SKIP / 落回 Memory。建议在 `SERVICE-BUGFIX-2026-09-29-03.md` 之外（例如 CI 脚本或 CONTRIBUTING）集中写一次。

## §5 没读完 / 没跑的部分

- 没跑：需要真实 Redis Cluster 的 3 个用例（RR-33 两个、RR-30 的 tagged control）；Toxiproxy 用例；RR-32 的生成消费者测试（`purchase_delivery_test.go.tmpl`，需 `roost project new` + 本地 replace）；`go test ./codegen/...`；全仓 race。
- 没做红测：RR-24、RR-32、RR-33（原因见 §2）。
- 没读：`kit/service/platform/platform_rpc_gen.go`、`kit/service/account/accounts_rpc_assembly_gen.go` 全文（只核了 CreateRole/CancelClaim 相关段）；`service/mail/types.go` 除 Envelope/常量以外的部分；`docs/review/REVIEW-2026-09-29-services-{05..11}.md` 运行记录正文（只用了 COMPLETION 与 ARCHIVE）；B 线 evidence 目录里的 JSON 只抽了 batch-1 的 `results.json`。
- RR-34（redis driver Pipeline 聚合错误）属另一位审计员；本文只确认 mail 的 `GetMany` 逐 future 检查不依赖它。
- 临时 worktree `scratchpad/audit-svc1` 已 `git worktree remove`；探针源文件与日志留在 `scratchpad/audlog-svc1/`，不入库。
