# N09 skill 第二批：同步/接入（skillsync）与执行/状态余项（checkpoint / 回放 / owned 进程）

2026-10-05，基线 `855c2a38236f4efc9575fa9ba84bf8bf4100daf3`（origin/main，含第一批 `8c2d21a6` / `855c2a38`），独立 worktree 分支 `revn09b`，NC 编号段 114～119（本批用 114～117）。上一批：[第一批记录](REVIEW-2026-10-05-n09-batch1.md)。[接力清单](REMAINING-REVIEW-HANDOFF-2026-10-05.md) · [跨轮进度](PROGRESS.md)。

图谱：项目 `Users-whb-roost-roost-core`，generation 2026-09-30T11:28:30Z。本批引用的 `skill/skillsync/*`、`skill/runtime_sync.go`、`runtime_mutation.go`、`runtime_checkpoint.go`、`runtime_owned_process.go`、`process_owned.go`、`replay.go` coverage 为 `no_recorded_issue / metadata_match`；但第一批修复（`855c2a38`）改过 `runtime.go`、`runtime_cast_window.go`、`scheduler.go`，generation 早于它，本批一律按当前源码逐行读取，图谱只用于定位。skillsync 的仓内正式调用方用 `rg` 核对：除 `skill/` 自身与 `codegen/.../consolidation_imports.yaml`（导入改写表）外没有（O3 仍成立）。

## 1. 本批审过的场景

| 编号 | 场景 | 读过的源码 / 跑过的 | 结论 |
| --- | --- | --- | --- |
| Y1 | Coordinator：view 锁、Open/CloseObserver 栅栏、source cursor 推进、Append→Outbox 窗口、Acknowledge 先删 outbox 再 ACK History、Recover | `skillsync/coordinator.go` 全文 | 主链成立；Recover 不推进 source cursor、Put+Reconcile 同时失败时重复 Append（客户端按 mutation 序号去重，见 O8 / O10） |
| Y2 | durable outbox：Put / PutBatch 预检、Reconcile、ACK 删除、发布占用与退避、容量 / 年龄 | `skillsync/outbox.go` 全文 | 无确认缺陷；最老 pending 超龄让全部 observer 停发（文档写明的 fail-closed，见 O5） |
| Y3 | 文件 outbox：临时文件 → fsync → 原子替换 → 目录 fsync；Windows `MoveFileExW(REPLACE_EXISTING|WRITE_THROUGH)`；校验和、文件名身份、上限 | `file_outbox.go`、`file_replace_unix.go`、`file_replace_windows.go` | 替换与计数在各失败点保持一致；崩溃遗留的 `outbox-*.tmp` 不清理（O6）。Windows 实现只读源码，本机未跑 |
| Y4 | Applier：admission、epoch 切换、Commit 失败 Rollback、mutation / presentation 序号去重、manifest 依赖 | `skillsync/applier.go` 全文；探针 | **NC-116**：被拒绝的 full 包留下 pendingEpoch，Applier 永久卡死 |
| Y5 | 可见性：快照 / 增量 / reset 三条下发路径 | `skillsync/visibility.go` 全文、`presentation_recovery.go`、`presentation.go`；探针 | **NC-114**：presentation reset 完全不过滤；**NC-115**：ability 按 handle 只在增量生效、三类 remove 对不可见实体放行 |
| Y6 | 增量 state mutation：写点登记、wholesale 域、时钟派生字段、首提交 baseline、`StateDeltas` 游标过期 | `runtime_mutation.go`、`runtime_sync.go` 全文 | 无确认缺陷（包内测试全程开着 `stateMutationVerifyIncremental` 影子校验）；时钟字段不可见时 mutation 头仍带 Tick（O9） |
| Y7 | checkpoint：采集前 baseline 校验、任务 / 帧 / pendingTasks 交叉校验、完成队列、恢复后 baseline | `runtime_checkpoint.go:520-1170`；探针：NC-110～112 四条失败路径后 checkpoint → 恢复 → 再 checkpoint、38 个 fixture 各 3 步往返 | **NC-117**：提交前失败的 cast 不进完成队列、永不回收，超过上限后 checkpoint 无法恢复；另见 O7（payload 字节不确定） |
| Y8 | NC-110 修复后的终止路径与 checkpoint 一致性 | `scheduler.go:410-520`、`runtime.go:395-446`、`runtime_retention.go` | 任务 / 帧 / pendingTasks 在启动失败、Cancel / Release 失败、pulse 失败后均能恢复；但失败终态与完成队列的关系不一致，即 NC-117 |
| Y9 | owned 进程：事务性 spawn（预检 → Host 事务 → 逐个起进程 → Commit / Rollback）、终止幂等、reap | `runtime_owned_process.go`、`process_owned.go:1-420`、`process.go:189-315` | 无确认缺陷：`stopProcess` 对 nil / 已停进程幂等，失败分支先停已起进程再 Rollback |
| Y10 | 回放：RecordingHost / ReplayHost | `replay.go` 全文 | 调试适配器；不转发可选接口（O11），不登记 |

探针 `zz_probe_n09b_test.go`（skill、skillsync 两包）跑完已删除，结论落为正式回归。

## 2. 确认缺陷

| 编号 | 等级 | 一句话 |
| --- | --- | --- |
| [NC-114](../bug/RR-20261005-NC-114.md) | P2 | presentation reset 不经过 VisibilityPolicy，不可见施法者的持续表现、目标与坐标发给所有 observer |
| [NC-115](../bug/RR-20261005-NC-115.md) | P2 | state 可见性快照与增量口径不一致：ability 按 handle 只在增量生效，cast / process / persistent remove 对不可见实体放行 |
| [NC-116](../bug/RR-20261005-NC-116.md) | P3 | Applier 拒绝 BaseSequence 非零的 full 包后 pendingEpoch 不复位，之后永远 ErrApplyInProgress |
| [NC-117](../bug/RR-20261005-NC-117.md) | P2 | 提交前失败的 cast 不进完成队列、永不回收；累计超过 CompletedCastLimit 后 checkpoint 无法恢复 |

## 3. 观察与设计建议（不登记 RR）

- **O5 一个 observer 卡住会停掉整个 Coordinator**。`capacityError` 的年龄检查是全局最老 pending（`outbox.go:228-230`），`PublishDue` / `Put` / `NewOutbox` 都先过它：一个断线却没 `CloseObserver` 的 observer 超过 `MaxPendingAge`（默认 24h）后，所有 observer 停发、新 Append 失败、重启装载也失败。`docs/skill/skill-implementation-guide.md` §11.5 写明这是有意的硬限制，故不登记；建议维护者考虑按 observer 驱逐 / 自动 Close，而不是全局停摆。
- **O6 文件 outbox 不清理崩溃遗留的 `outbox-*.tmp`**（`file_outbox.go:193-203` 只在本次调用内删除）。每次写入中途崩溃留一个，不计入上限，启动时也不扫。
- **O7 checkpoint payload 字节不确定**。`ActivePolicies`、`ProcLedger`、`RootEventCounts`、`AbilityByProgram` 按 map 迭代顺序写出未排序（`runtime_checkpoint.go:630-655`）；恢复时 `rootEventOrder` 按 ID 排序，而 live 是插入顺序。目前没有承诺 checkpoint 字节可比，root 淘汰只淘汰不活跃 root，语义无影响；若以后用 checkpoint 摘要做一致性比对，需要先排序。
- **O8 Recover 不推进 source cursor**。Recover 生成新的 state full / presentation reset 后，Coordinator 的 `cursors` 不变，下一次 Flush 会再发 full 之前的增量，客户端 Applier 按 mutation / presentation 序号丢弃（`applier.go:272, 311`），只是浪费。
- **O9 时钟字段不可见时增量仍带 Tick / WorldRevision**。`FilterStateSnapshot` 在 `clock` 不可见时清零 Tick，但每条 mutation 与 record header 都带 Tick / WorldRevision（`skillsync.go:98-101`）。是否算泄漏取决于业务把 clock 设为不可见的意图，未登记。
- **O10 Append 成功、Outbox Put 与 Reconcile 同时失败时，source cursor 不前进**（`coordinator.go:360-373`），下一次 Flush 会把同一 mutation 再 Append 一次（新的网络序号）；客户端按 mutation 序号去重，无语义后果。
- **O11 RecordingHost 不转发 `RuntimeStateExtensionProvider` / `HostEventCompactor` / `OwnedEntityRuntimeHost` 等可选接口**，录制运行与直接运行的行为不同；它是调试适配器（`replay.go:21-22` 注释），不登记。
- **O12 observer 可见性中途变化没有重发快照的入口**。过滤按当前可见性逐条判断，实体从可见变为不可见（或反之）时，客户端保留旧条目 / 缺少新条目，直到下一份 full。NC-115 修复让 remove 也按实体过滤后，这种“变不可见之后的 remove”同样不再下发；需要业务在可见性变化时调用 `PublishSnapshot`，建议写进接入指南或提供显式入口。
- **O1～O4（第一批）**：本批没有找到新的真实触发路径（skillsync / Runtime 仍无正式调用方），不改行为，等维护者决定。

## 4. 未审 / 未验证

- 数据/属性：Parse + Compile 的拒绝路径与 `skillcompose/` 未审（只读了 `parse.go:1-120` 与生成的 `CompileAll`，见 §6）。
- `presentation_asset_cache.go`、`presentation_assets.go`、`observability.go`、`schema.go` 的迁移图只读未深审；`process_motion.go`、`process_area.go`、`process_numeric.go` 未读。
- 没有接真实传输（kit syncstream / NATS）做 skillsync 端到端；`skill/integration/sync-e2e` 只跑既有用例。Windows 文件替换只读源码。

## 5. 方向判断

skill 的“同一事实经多条路径下发 / 收尾，每条路径手写一套规则”在两批里反复出缺陷：第一批 NC-110～112 是 8 个终止点各自手写收尾；本批 NC-117 是终态 cast 的保留规则在 live（`trackCompletedCastLocked`）和恢复（`restoreCheckpointPayload`）两处各写一套；NC-114 / NC-115 是可见性在快照、增量、reset 三处各写一套。信号是“同一不变量第二次被打破”（终止路径 → 保留集合）以及“新增下发路径默认不过滤”。建议方向：

1. 终态 cast 只有一个“进入终态”入口（第一批的 `failCastLocked` 加上完成路径），保留 / 淘汰规则只在这个入口登记一次，checkpoint 恢复复用同一判定，而不是再推一遍；
2. 可见性以“每个实体归属条目”为单位只定义一次规则（谁拥有、谁是目标、有没有空间字段），快照、增量、reset 都调用它；新增下发路径必须经过 Coordinator 的统一过滤点。

本批修复按这个方向做最小收敛（见修复记录），不另立新抽象。代价：remove mutation 多带一个实体字段（追加 JSON 字段，旧客户端忽略）；observer 可见性变化后的 remove 不再下发（O12）。

## 6. 停点与下一批入口

已审：同步/接入的 skillsync 全链（Coordinator → outbox → 文件 outbox → Applier → 可见性）与 `runtime_sync.go` / `runtime_mutation.go`；执行/状态余项的 checkpoint / 回放 / owned 进程。下一批（N09 第三批）：

1. 数据/属性：game-demo 启动链实际用到的 `Parse`（`rejectDuplicateKeysWithLimits`、`decodeStrictSingle`、各 `parse*` 的拒绝分支）与 `Compile` 的 error diagnostic 路径；`skillcompose/`。
2. `process_motion.go` / `process_area.go` / `process_numeric.go` 与 `presentation_asset_cache.go`。
3. O5 / O12 若维护者决定改契约，再回到 skillsync。

## 7. 修复与验证（审查提交之后追加）

审查提交 `1b2a51c4`（`docs(review)`）之后按授权修复，单独一笔 `fix(skill,skillsync)`。修复单元：NC-114 + NC-115（[记录](../bugfix/RR-20261005-NC-114.md)）、NC-116（[记录](../bugfix/RR-20261005-NC-116.md)）、NC-117（[记录](../bugfix/RR-20261005-NC-117.md)）。

| 命令（`GOWORK=off`，模块根） | 结果 |
| --- | --- |
| 8 条新用例修前：7 条 NC 回归（skill 2、skillsync 5）+ 1 条控制（skill `TestFailedStartLeavesNoCompletedQueueEntry`） | 7 FAIL，原文见 NC-114～117；控制修前 ok |
| 同一组修后 | 全部 ok |
| `gofmt -l skill` | 空 |
| `go vet ./skill/... && go test -race -count=3 ./skill/...` | skill / combat / combatcomponent / skillcompose / skillsync 全部 ok（skill 包全量在 `stateMutationVerifyIncremental` 影子校验下运行，覆盖 remove 新字段） |
| `go build ./... && go vet ./...`；`go test -count=1 .` | 通过 |
| `skill/examples`：`go build ./...`、`go run ./fireball`；`skill/integration/sync-e2e`：`go test -count=1 ./...` | 通过 |

修复后的组合契约复核：reset 的新错误只经 `prepareFlush` 与 `Recover` 的 provider 返回，两处调用方本来就处理 state 快照的同类错误（fail-closed、计 `VisibilityFailures`）；remove 新字段由同一 `StateMutation` 结构体编码，`ApplyStateMutation` 与 Applier 不依赖；完成队列的变化只影响终态 cast 的回收时机，`castEvictableLocked` 的“仍被引用不回收”条件不变，恢复规则与 live 现在一致（`TestUncommittedFailedCastsStayWithinTheCompletedCastLimit` 断言重新 checkpoint 的完成队列相同）。没有改生成形状（模板、codegen 未动），没有跑 codegen 测试与 game-demo 生成：game-demo 只用 Parse + Compile（O3）。外部依赖（Mongo / Redis / NATS）本批没有用到。
