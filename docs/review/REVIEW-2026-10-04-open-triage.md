# 2026-10-04 开放条目现状核实（v1.19.1）

基线：`v1.19.1` = `d3e69336`（只读核实；探针在临时 worktree 与临时生成工程里，未改仓库文件、未提交）。
审查员口径：`docs/agent-skills/roost-coding/SKILL.md` + roost-review skill。**W-2026-10-04-02 不在本轮范围**。

## 0. 范围与方法

**范围**

1. [`docs/bug/README.md`](../bug/README.md) 中状态列仍为“未修复 / 未修”的 RR 表行。
2. [`docs/bug/WANTED.md`](../bug/WANTED.md) 中 W-2026-09-16-01、W-2026-09-17-02～05、W-2026-09-18-01～08、W-2026-09-18-10（共 14 条）。
3. [`docs/bug/CARRYOVER.md`](../bug/CARRYOVER.md) 仍开放的项（只列出；与上面同根的注明）。

**README 表行的筛选**：`grep -n '未修复'` 只命中 3 条表行（`README.md:604-606`，RR-20260921-03/04/05）；
其余命中都是历史散文（“原时点：当时未修复”“未修复清单为空”等）。另用 awk 扫了所有 `| RR-… |` 表行的末列，
状态为“已确认、未修 / 已复现、未修”的还有 RR-20261004-NC-05～10（`README.md:85-87,101-103`），
RR-20260929-01～22 的“本轮未实施”表（`:202-237`）、RR-20260918-03/04 的“新确认”表（`:686-687`）——
这些都在同文件更新的表 / 段落里被覆盖为已修（NC-05～10 见 `README.md:77-79,93-95` 与 `docs/bugfix/RR-20261004-NC-0{5..9}.md`、`NC-10.md`；
RR-20260929-01～22 见 `README.md:198,211` 与 `docs/bugfix/SERVICE-BUGFIX-2026-09-29*.md`；RR-20260918-03/04 见 `README.md:669` 与
`docs/bugfix/RR-20260918-0{3,4}.md`），**不是开放项**。真正开放的表行只有 RR-20260921-03/04/05 三条，`docs/bugfix/` 无对应记录，
`git log --grep/-S 'RR-20260921-03'` 只有登记与文档提交（`00277bd2`、`b5ca05a5`、`69948ef9`、`a0348d4c`），无修复提交。

**方法**

- `git worktree add …/scratchpad/triage-wt v1.19.1`；在 worktree 里 `GOWORK=off go run ./codegen/cmd/roost project new planet -skip-deps -module example.com/planet -template game-demo`，
  `go mod edit -replace github.com/tjbdwanghaibo/roost-core=<worktree>` + `go mod tidy`；playerowner 探针用工程内既有 harness
  （`ownersUnderTest` / `fakeLeaseTable` / `evictorStub` / `blockingEvictor` / `projectionStub` / `evictorFunc`）。
- “已修复”条目：找 bugfix 记录里点名的回归测试，在 v1.19.1 上跑（core 包直接跑；demo 模板的在生成工程跑；codegen 运行期守卫脚本用
  `ROOST_CORE_PIN=v1.19.1 ROOST_CORE_DIR=<worktree>` 跑）。
- codebase-memory：项目 `Users-whb-roost-roost-core` 索引 generation 为 2026-09-30，`playerowner.go.tmpl`、`.github/workflows/ci.yml`
  的 `check_index_coverage` 为 `metadata_changed`，`codegen/scripts/*` 为 `excluded`，因此这几处的结论全部来自直接读 v1.19.1 源码；
  其余引用文件 `no_recorded_issue / metadata_match`。行号均为 `d3e69336` 上的行号。
- 工具：go1.27.0 darwin/arm64，`-race`。未使用隔离集成环境（本轮不需要真实依赖）。

**统计**

| 来源 | 条数 | 仍存在 | 已修复 | 已过时 | 需要维护者决定 | 未核完 |
| --- | --- | --- | --- | --- | --- | --- |
| README 开放表行 | 3 | 3 | 0 | 0 | 0 | 0 |
| WANTED（范围内） | 14 | 0 | 12 | 2 | 0 | 0 |
| 合计 | 17 | **3** | 12 | 2 | 0 | 0 |

另：核实过程中**新发现 1 条候选**（§2.4，与 RR-20260921-05 同根：codegen 四个运行期守卫脚本合仓后默认即失败且无 CI 调用），
建议并入 RR-20260921-05 的修复或另登 Wanted。CARRYOVER 仍开放项 6 条“A→线” + 2 张验证留项表（§1.3），本轮不判。

## 1. 总表

### 1.1 README 开放 RR

| 条目 | 来源 | 结论 | 证据 | 建议等级 / 下一步 |
| --- | --- | --- | --- | --- |
| RR-20260921-03 | [README:604](../bug/README.md) · [REVIEW-2026-09-21-02](../bug/REVIEW-2026-09-21-02.md) | **仍存在** | `playerowner.go.tmpl:641` 无条件 `Release`；`:874` Claim 以 `Admit` 为连续性探针、`:692` `confirmLocked` 清 `handingBack`。探针 2/2 交错红：`mine=true admitted=false redis_owner_held=false released=[77]` / `mine=true admitted=true redis_owner_held=false released=[77]`，`-race -count=30` 60/60 子测试红 | **P1 维持**；窗口比登记时更宽（撤离 ≤5s + 投影等待 ≤5s）。修法见 §2.1 |
| RR-20260921-04 | [README:605](../bug/README.md) | **仍存在**（且更差：40s → 45s） | `:327` `handBackBudget=8`、`:154` `evictBudget=5s`、`:414` 每人独立计时、`:617` 投影等待 `projectionWait=evictBudget` 在撤离之后再加一份；`playerroute.go.tmpl:46-47` Lease 30s / RefreshInterval 10s。常量断言红：`45s, not below … 20s`；缩放实跑 `409ms / 50ms = 8.2 倍 → 40.9s` | **P2 维持**。修法见 §2.2 |
| RR-20260921-05 | [README:606](../bug/README.md) · CARRYOVER A12 | **仍存在** | `ci.yml:27-170` 无 `go generate` / diff 步骤；`.github/workflows/*` 的 `generate --check` 只在生成工程（`framework-compat.yml:147`、`demo-publish.yml:55`、`release.yml:114`）。实测：给 `service/session/session_rpc_gen.go` 追加内容后 `go build` + `go vet` 通过；`go generate` 即恢复 → 漂移只有 generate 能抓，CI 不跑它 | **P2 维持**；当前 `go generate ./kit/... ./service/... ./codegen/...` 后工作树干净，加闸即绿。§2.3 |

### 1.2 WANTED（范围内 14 条）

所有 14 条的正文里**都已经写了分流结论**（`WANTED.md:405-418` 09-18 第三轮表、`:602`、`:634-636`、`:667`、`:720`）。
本轮核实的是“结论去向在 v1.19.1 上是否仍成立”。

| 条目 | 来源 | 结论 | 证据（v1.19.1） | 下一步 |
| --- | --- | --- | --- | --- |
| W-2026-09-16-01 match Grouping 注入无效 | WANTED:602 → RR-20260916-05 / U-0217 | 已修复 | `kit/service/match/grouping_contract_promises_test.go` `TestGroupingIsNotAnInjectionPointOfTheStore` PASS | 无 |
| W-2026-09-17-02 dao 嵌套二层 dirty 未接线 | WANTED:634 → RR-20260917-05 / U-0232 | 已修复 | `codegen/internal/dao/testdata/runtime/roundtrip_test.go` `TestNestedChildChangeMarksItsParentThroughEveryEntry`：按脚本方式在临时模块 replace 到 worktree 跑 PASS；`ROOST_CORE_PIN=v1.19.1 sh codegen/scripts/dao-golden-runtime.sh` 全包 ok | 守卫脚本默认失败，见 §2.4 |
| W-2026-09-17-03 attribute 生成物依赖未提供类型 | → RR-20260917-06 / U-0230 | 已修复 | `ROOST_CORE_PIN=v1.19.1 ROOST_CORE_DIR=<wt> sh codegen/scripts/attribute-runtime.sh` → `ok attributeruntime/combat` | 同 §2.4 |
| W-2026-09-17-04 原生 saga 完成效果无消费者 | → RR-20260917-07 / U-0231 | 已修复 | `saga/nest_completion_promises_test.go` `TestAssemblyConsumesNativeNestCompletionEffects`、`TestAssemblyKeepsItsExistingConsumerSubjects` PASS | 无 |
| W-2026-09-17-05 实体状态同步无装配入口 | WANTED:720 → RR-20260918-01 / 02 | 已过时（前提代码被 ARCH-10 重写） | `room` 包已删（worktree 无 `room/`，全仓无 `RoomBroadcaster` / `SubscriptionCoordinator`）；装配入口现为 `sync/entitysync.Manager` + `kit/nest/entity_sync.go:52-56` 自动接 `DurableWatermark`；`sync/entitysync/flush.go:103-105` 门控。RR-01 侧 `entity-sync-runtime.sh`（pin v1.19.1）ok；水位：`TestTheDurabilityGateHoldsTheWholeSubjectUntilTheWatermarkReachesIt`、`TestUnobservedCaptureStillRespectsDurabilityWatermark`、`TestEntitySyncModWiresPipelinedDurableWatermark` PASS | 无 |
| W-2026-09-18-01 邮件账本寿命 | → RR-20260918-05 / U-0241 | 已修复 | 生成工程 `game/handler` `TestMailClaimsArePrunedByTheMailsOwnExpiry`、`TestAClaimWithNoExpiryIsNeverPruned` PASS | 无 |
| W-2026-09-18-02 同步字段掩码包私有 | → ARCH-06 / M-12 | 已修复（按 ARCH 落实） | `codegen/internal/dao/template_dao.go:660-681` 生成 `<Dao>SyncFields() []dataengine.SyncFieldMeta`；`docs/bugfix/ARCH-06-sync-field-vocabulary.md` | 无 |
| W-2026-09-18-03 会话关闭无回调 | → RR-20260918-06 / U-0243 | 已修复 | 生成工程 `internal/service/game` `TestAClosedSessionLeavesTheSceneWithoutAnyTraffic`、`TestAClosedSessionLeavesTheWorldChannel` PASS | 无 |
| W-2026-09-18-04 syncTopic 裸标识符 | → RR-20260918-07 / U-0239 | 已修复（标记已改名） | `codegen/internal/entity/parse.go:271-276` 旧键 `syncTopic` 直接报“renamed: write syncNamespace”；`:730-760` `validateSyncNamespaceParam` 拒裸标识符；`sync_namespace_promises_test.go` 两条 PASS（bugfix 记录里的 `TestSyncTopic*` 已随 `acc0a0d9` 改名） | 无 |
| W-2026-09-18-05 单观察者 AOI 无预算 | → RR-20260918-08 / U-0240 | 已修复 | 实现已从 `spatial` 迁到 `sync/entitysync/policy`；`aoi_budget_promises_test.go` 4 条 PASS | 无 |
| W-2026-09-18-06 AOI 与订阅 id 空间 | 09-18 决定：全程 entity id，补消费契约测试 | 已修复（按决定落实） | 生成工程 `TestTheBridgeKeepsEveryIdInTheEntityIdSpace` PASS；`sync/entitysync/policy/aoi.go:498` 自观察排除仍按同一 id 空间 | 无 |
| W-2026-09-18-07 subject 双关 / 点不是 pos | 09-18 决定：文档契约 | 已修复（按决定落实） | `docs/review/IMPLEMENTATION-SCENE-INTEREST-IDENTITY-AND-LIFECYCLE.md` §2（`:13-15`）写明角色 / 点的契约 | 可选：把这段搬进 `sync/entitysync/policy` 包注释（代码迁包后包注释未带上）；不影响结论 |
| W-2026-09-18-08 多来源兴趣合并 | 09-18 决定：demo 落地、不提升 Core | 已过时（决定被 ARCH-10 超越） | 聚合已进 Core：`sync/entitysync/policy/source.go` `Source` / `RelationSource`（band 0，Interest 负责合并）| 无 |
| W-2026-09-18-10 进程本地怪物 ID | → RR-20260918-09 / U-0242 | 已修复 | 生成工程 `game/runtimeid` 5 条（`TestTwoShardsNeverCollide` 等）PASS | 无 |

（W-2026-09-18-09 不在本轮列出范围；其 09-18 结论“全 nopersist Monster DAO 成立”与现状一致：`demo/db/def/monster.go.tmpl:20-22`。）

### 1.3 CARRYOVER 仍开放项（只列出）

| 项 | 去向（CARRYOVER 原文） | 与本轮条目的关系 |
| --- | --- | --- |
| A1 毒丸 WAL 记录无隔离流程 | feature 线（带 P1 分量） | 无 |
| A2 丢失检测 / 缺口补发 | feature 线 | 无 |
| A6 无跨进程移交协议 | feature 线；OPEN-ITEMS C24 | **与 RR-20260921-03 同根**（移交靠超时近似） |
| A7 在途工作无计数（`Admit` 无 `Done`） | feature 线（契约变更） | **与 RR-20260921-03 同根**（备选修法 B） |
| A8 `EntityManager.Destroy` 不可取消 | ARCH 线 | **RR-20260921-04 的直接前提**（`playerowner.go.tmpl:139-154` 注释仍写明 Destroy 不读 ctx） |
| A14 ARCH-05“要不要打散” | ARCH 线 | 无 |
| A12 | → RR-20260921-05 | 本轮核实仍存在 |
| B 节 N03 验证留项（NC-08～12：真实 NATS/JetStream、etcd 发现 / 选主 / watch） | 证据缺口，非 RR | 无 |
| B 节 N04 验证留项（Redis Cluster/HA、RefHMap 并发 / 未知结果、Layered 容量、Mongo HA、Migration 正式消费者） | 证据缺口，非 RR | 无 |

B1～B5、A→结案各条已在原文关闭，不再列。

## 2. “仍存在”各条

### 2.1 RR-20260921-03（P1）：归还进行中被重新 Claim，Claim 报 mine=true 而共享表无主

**根因（`d3e69336`，`demo/internal/service/game/playerowner.go.tmpl`）**

1. `handBackIdle:592-643`：`:606` 在锁内置 `handingBack = true`；`:622` 只在撤离**之前**看一次 `ActiveSessions`；
   `:626` `dropResident`；`:636` 等投影落库（RR-20260926-31 新增）；`:641` **无条件** `owners.Release(ctx, playerID)`——
   不再确认 `handingBack` 仍是自己设的那一个。
2. `Claim:859-908`：`:874` `continuous := owners.Admit(playerID) == nil`——`handingBack` 让它为 false，但这只触发“跨间断扔副本”；
   `:875` `store.Claim` 此时 Redis 键仍是本进程的（`playerroute.go.tmpl:216-243`：同 token 走 `CompareAndExpire` 并返回 ours），`mine=true`；
   `:884` 扔副本成功（加入或新起一次撤离）；`:906` `confirmClaim` → `confirmLocked:692` **`state.handingBack = false`**。
3. `Release:934-944`：删本地状态 + `store.Release`（`playerroute.go.tmpl:318-323` `CompareAndDelete` 同 token → 删除成功）。

09-30～10-01 的 RR-20260930-23（`closeSessions`）与 RR-20261001-07（`abandon`、`errStaleCopyKept`）只改了刷新循环的重取分支与扔不掉副本的分支，
都没碰 `handBackIdle` 的 Release 前确认，也没让 `Claim` 看 `handingBack`。登录路径 `demo/game/controllers/player/enter_game.go.tmpl:61-76`
仍是 `mine=true` 即 `GetOrCreate` 装载。后台 `Owns`（`:953-979`）在该窗口走 `route.SID == self` 分支返回 false，不会触发——**唯一入口是登录**。

**两种结局（探针均确定性复现）**

- confirm 先于 Release：`mine=true admitted=false redis_owner_held=false`——登录成功、本地无租约，之后每条消息被 `AdmitMessage` 拒，Redis 无主。
- Release 先于 confirm：`mine=true admitted=true redis_owner_held=false`——本进程放行写入而共享表无主，另一进程可合法 Claim 并装载第二份 →
  `fatal projection version conflict`。

**窗口**：从 `:622` 之后完成 auth 的登录，到 `:641`。现在是 `dropResident`（≤ `evictBudget` 5s）+ 投影等待（≤ `projectionWait` 5s），
比 09-21 登记时（只有撤离）更宽。

**等级**：维持 P1。

**候选修法**

- A（推荐，最小）：归还与认领互斥地“交接”。`leaseState` 增加一个 `handBack chan struct{}`（`handBackIdle` 选中时创建，结束时 close）；
  `Claim` 在 `store.Claim` **之前**（锁内）发现 `handingBack` 就等这个 channel（受 ctx 约束，超时返回错误让登录重试），
  归还结束后再走正常路径——归还成功则 Redis 已空，`SetNX` 重新取得；归还放弃则租约仍在服务、`continuous` 判定照旧。
  同时 `handBackIdle` 在 `Release` 前锁内复核 `state == owners.local[playerID] && state.handingBack`，不成立就放弃（防御第二道）。
  只做“Release 前复核”**不够**：复核与 `store.Release` 之间仍可插入 `store.Claim`，即探针的第二种交错。
- B：CARRYOVER A7——`Admit` 配对 `Done` 的在途计数，归还判据不再用 `lastUsed` 近似；更彻底但是契约变更。
- C：CARRYOVER A6——跨进程移交协议（OPEN-ITEMS C24）的第一个用例。

**验收覆盖点**（沿用 REVIEW-2026-09-21-02 并补一条）：两种交错都不得出现 `mine=true && owner 无主`；归还放弃后 `handingBack` 回 false；
登录等待归还时受 `login_timeout` 约束、超时给明确错误；探针 `TestTriageRR0921_03_*` 两个子测试转绿。

### 2.2 RR-20260921-04（P2）：一次归还批次占住刷新循环超过租约

**根因**：`renew:487-567` 在 `refreshLoop:469-483` 唯一 goroutine 上，续租之后同步调用 `handBackIdle:566`；
`handBackIdle` 串行最多 `handBackBudget`(8, `:327`) 人，每人 `dropResident` 自带一个 `evictWait`(= `evictBudget` 5s, `:154,:355,:414`) 计时器，
撤离成功的再共享一份 `projectionWait`（5s, `:355,:617`）。最坏 8×5s + 5s = **45s** > `Lease` 30s（`playerroute.go.tmpl:46`），
而续租在批次**之前**刚发过一次，所以本进程其余所有租约在批次结束前过期。U-0272 / RR-20260920-12 只界住了单次等待；RR-20260926-31 又在批次上加了一份投影等待。

**红文本**：`worst-case hand-back pass = handBackBudget(8) x evictBudget(5s) + projectionWait(5s) = 45s, not below Lease(30s) - RefreshInterval(10s) = 20s`；
缩放实跑 `one renew pass took 409ms with per-eviction wait 50ms; multiple = 8.2 -> … 40.9s`。

**等级**：维持 P2（触发需 8 个空闲且实体被长事务占住的玩家；后果是批量 lease lost / fence / 断线）。

**候选修法**

- A（推荐）：整个归还回合一个**时间**预算：`passCtx, cancel := context.WithTimeout(ctx, handBackPassBudget)`，`handBackPassBudget` 由常量推导
  且断言 `< Lease − RefreshInterval`（例如 `Lease − RefreshInterval − AdmissionGuard` = 15s），`dropResident(passCtx, …)`（它已在 `:424` 读 ctx）与投影等待都用它；
  预算用尽时对剩余已选中者逐个 `cancelHandBack`（现有逻辑，别改坏）。
- B：归还移出刷新 goroutine（单飞的后台 goroutine），刷新循环只负责续租；需要处理与下一回合选择的并发（`handingBack` 已是天然去重）。
- 两者都与 CARRYOVER A8（Destroy 不可取消）相关：A8 不变，任何等待者都必须自己算相乘后的界。

**验收**：§2.5 的 `TestTriageRR0921_04_HandBackPassFitsInsideTheLease` 常量关系与缩放实跑两段转绿；被截断的玩家保留租约且 `handingBack` 复位。

### 2.3 RR-20260921-05（P2）：本仓 `go:generate` 产物无 CI 校验

**现状**：仓内 13 处 `//go:generate`（`kit/service/{chat,match,mail,platform,account,global/activity,rank,global,session}`、
`service/{match,mail,session}`，以及 `codegen/internal/roost/add_rpc.go:141`——后者 `go generate` 输出 “no //roost:rpc interfaces”，实为 no-op）。
`ci.yml` 步骤（`:27-38` mod/tidy/vet/glsvet/vuln、`:63-66` 分片测试、`:117-134` Redis 集成与 race、`:145` `go test ./...`、`:151-170` replace / 模块路径 / tag）
没有 generate；`nightly*.yml`、`scripts/pretag.sh` 也没有（`grep -rn 'go generate'` 为空）。servicerpc 的 `golden_test.go` 只锁合成样例的输出，不比对仓内产物。

**实测**（worktree）：`GOWORK=off GOFLAGS=-mod=mod go generate ./kit/... ./service/... ./codegen/...` 后 `git status` 干净（1.9s，不需网络）；
对 `service/session/session_rpc_gen.go` 追加三行后 `go build` + `go vet ./service/session/` 均通过，`go generate ./service/session/` 输出
`generated: session_rpc_gen.go` 并恢复原样——即漂移只有 generate + diff 能抓。

**修法**（与 09-21 一致）：`ci.yml` 加一步 `GOFLAGS=-mod=mod go generate ./kit/... ./service/... ./codegen/... && git diff --exit-code`。
建议同一步把 §2.4 的四个运行期守卫一并接上（同根：仓内守卫存在但 CI 不跑）。

**测试草稿**：本条是 CI 闸，不是 Go 测试；验收按 09-21：故意改脏一个 `*_gen.go`，该步骤必须红。

### 2.4 新发现候选（与 RR-20260921-05 同根，建议并入或另登 Wanted）：codegen 运行期守卫脚本合仓后默认即失败，且无任何 CI 调用

- `codegen/scripts/{dao-golden-runtime,attribute-runtime,entity-sync-runtime,cfggen-golden-runtime}.sh` 都从
  `codegen/scripts/source-head-check.sh` 里 `sed` 出 `core_pin="${ROOST_CORE_PIN:-vX}"`（如 `dao-golden-runtime.sh:15-16`）；
  `6d04aea4`（2026-09-20，“发布链收拢成一个模块一个版本”）删掉了那一行（`-core_pin="${ROOST_CORE_PIN:-v1.15.18}"`）。
  v1.19.1 上四个脚本不设 `ROOST_CORE_PIN` 时全部 `cannot determine the roost-core pin`、exit 2。
- `attribute-runtime.sh` 的 `core_dir` 缺省 `$here/../roost-core`（三仓时代路径），合仓后指向不存在的目录。
- `grep` 全仓 `.github/workflows`、`scripts/*.sh`、Makefile：四个脚本**无调用方**。它们是 RR-20260917-05 / 06、RR-20260918-01、U-0224 的唯一运行期证明
  （codegen 自身测试只比文本，见各 bugfix 记录）。
- 本轮设 `ROOST_CORE_PIN=v1.19.1 ROOST_CORE_DIR=<worktree>` 四个都 ok——**当前功能无回归，缺的是闸**。
- 建议等级 P3（守卫失效，非行为缺陷）；修法：脚本改用仓内 replace（与 `source-head-check.sh` 的 go.work 做法一致），并接入 CI。

### 2.5 测试草稿（完整源码）

放在生成工程 `internal/service/game/triage_open_probe_test.go`（`package` 名随生成工程，本次为 `Game`；转正到模板时改为 `{{GAME_SERVICE_PKG}}`、
import 改为 `"{{MODULE}}/game/playerroute"`，并入 `demo/internal/service/game/playerowner_test.go.tmpl`）。只依赖该文件已有的 harness。

```go
package Game

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"example.com/planet/game/playerroute"
)

// RR-20260921-03 探针（v1.19.1 现状核实）：闲置交还进行到一半（副本已撤离、正在等投影
// 落库、尚未 Release）时同一玩家登录 Claim。Claim 的契约是"mine=true 表示本进程拿到
// 了所有权"；交还流程随后无条件 Release，把刚确认的所有权从本地和共享表里一起删掉。
// 不变式：Claim 返回 mine=true 之后，交还完成时共享表里这个玩家仍归本进程，且本地准入
// 与共享表一致。
func TestTriageRR0921_03_ClaimDuringHandBackKeepsOwnership(t *testing.T) {
	t.Run("confirm-before-release", func(t *testing.T) {
		owners, table, _, clock := ownersUnderTest(t)
		projections := newProjectionStub()
		owners.Projections(projections)
		owners.projectionWait = 5 * time.Second
		ctx := context.Background()
		if owned, err := owners.Owns(ctx, 77); err != nil || !owned {
			t.Fatalf("owns: %v %v", owned, err)
		}
		land := projections.inFlight()
		defer land()
		*clock = clock.Add(HandBackIdle + time.Second)

		renewed := make(chan struct{})
		go func() { defer close(renewed); owners.renew(ctx) }()
		waitAsked(t, projections, 77) // hand-back: copy dropped, now waiting on projection, Release pending

		mine, err := owners.Claim(ctx, 77) // a login lands in the window
		land()
		<-renewed

		_, held := table.owner[77]
		admitted := owners.Admit(77) == nil
		t.Logf("mine=%v err=%v admitted=%v redis_owner_held=%v released=%v", mine, err, admitted, held, table.released)
		if mine && !held {
			t.Fatalf("Claim reported mine=true, but the hand-back released the lease afterwards: shared table has no owner (another process may load a second copy); admitted=%v released=%v", admitted, table.released)
		}
	})

	t.Run("release-before-confirm", func(t *testing.T) {
		owners, table, _, clock := ownersUnderTest(t)
		projections := newProjectionStub()
		owners.Projections(projections)
		owners.projectionWait = 5 * time.Second
		ctx := context.Background()
		if owned, err := owners.Owns(ctx, 77); err != nil || !owned {
			t.Fatalf("owns: %v %v", owned, err)
		}
		// Evictions: #1 is the hand-back's (returns at once); #2 is the login
		// Claim's own drop, held until the hand-back has released.
		var calls atomic.Int32
		claimDropStarted := make(chan struct{})
		letClaimDropFinish := make(chan struct{})
		owners.Evict(evictorFunc(func(context.Context, int64) error {
			if calls.Add(1) == 2 {
				close(claimDropStarted)
				<-letClaimDropFinish
			}
			return nil
		}))
		land := projections.inFlight()
		defer land()
		*clock = clock.Add(HandBackIdle + time.Second)

		renewed := make(chan struct{})
		go func() { defer close(renewed); owners.renew(ctx) }()
		waitAsked(t, projections, 77)

		type result struct {
			mine bool
			err  error
		}
		claimed := make(chan result, 1)
		go func() {
			mine, err := owners.Claim(ctx, 77)
			claimed <- result{mine, err}
		}()
		<-claimDropStarted // store.Claim already answered "ours"
		land()
		<-renewed // hand-back Release ran
		close(letClaimDropFinish)
		got := <-claimed

		_, held := table.owner[77]
		admitted := owners.Admit(77) == nil
		t.Logf("mine=%v err=%v admitted=%v redis_owner_held=%v released=%v", got.mine, got.err, admitted, held, table.released)
		if got.mine && !held {
			t.Fatalf("Claim reported mine=true and this process admits writes (admitted=%v), but the shared table has no owner: released=%v", admitted, table.released)
		}
	})
}

func waitAsked(t *testing.T, stub *projectionStub, playerID int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stub.mu.Lock()
		for _, asked := range stub.asked {
			if asked == playerID {
				stub.mu.Unlock()
				return
			}
		}
		stub.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the hand-back never reached the projection wait for %d", playerID)
}

// RR-20260921-04 探针：一次刷新回合里闲置交还的最坏等待时间，必须小于
// Lease − RefreshInterval，否则同一 goroutine 上其余租约的续期发不出去。
// 断言写成常量关系（常量一改自动重判），并用缩放的预算实跑一遍确认相乘关系成立。
func TestTriageRR0921_04_HandBackPassFitsInsideTheLease(t *testing.T) {
	worst := time.Duration(handBackBudget)*evictBudget + evictBudget // evictions + one shared projectionWait
	if limit := playerroute.Lease - playerroute.RefreshInterval; worst >= limit {
		t.Errorf("worst-case hand-back pass = handBackBudget(%d) x evictBudget(%s) + projectionWait(%s) = %s, not below Lease(%s) - RefreshInterval(%s) = %s",
			handBackBudget, evictBudget, evictBudget, worst, playerroute.Lease, playerroute.RefreshInterval, limit)
	}

	owners, _, _, clock := ownersUnderTest(t)
	const scaled = 50 * time.Millisecond
	owners.evictWait = scaled
	ctx := context.Background()
	for id := int64(1); id <= handBackBudget+2; id++ {
		if owned, err := owners.Owns(ctx, id); err != nil || !owned {
			t.Fatalf("owns %d: %v %v", id, owned, err)
		}
	}
	evictor := newBlockingEvictor()
	evictor.calls = make(chan int64, 64)
	owners.Evict(evictor)
	t.Cleanup(func() { close(evictor.release) })
	*clock = clock.Add(HandBackIdle + time.Second)

	start := time.Now()
	owners.renew(ctx)
	elapsed := time.Since(start)
	multiple := float64(elapsed) / float64(scaled)
	t.Logf("one renew pass took %s with per-eviction wait %s; multiple = %.1f -> at evictBudget=%s that is %s of refresh loop held (Lease=%s, RefreshInterval=%s)",
		elapsed, scaled, multiple, evictBudget, time.Duration(multiple*float64(evictBudget)), playerroute.Lease, playerroute.RefreshInterval)
	if time.Duration(multiple*float64(evictBudget)) >= playerroute.Lease-playerroute.RefreshInterval {
		t.Errorf("scaled pass held the refresh loop for %.1f eviction budgets; unscaled that exceeds Lease - RefreshInterval", multiple)
	}
}
```

转正注意：RR-04 的常量断言里 `+ evictBudget` 代表 `projectionWait` 的出厂值（`newPlayerOwners:355`）；修法若引入独立的回合预算常量，
断言应改为针对该常量；缩放段的判据也应改成“回合实际时长 ≤ 回合预算 + 一次调度裕量”。

**运行与红文本**（`GOWORK=off go test -race ./internal/service/game/ -run TestTriage -count=1 -v`；全文存于 scratchpad `triage-evidence/probe-red.txt`）：

```text
--- FAIL: TestTriageRR0921_03_ClaimDuringHandBackKeepsOwnership/confirm-before-release
    mine=true err=<nil> admitted=false redis_owner_held=false released=[77]
--- FAIL: TestTriageRR0921_03_ClaimDuringHandBackKeepsOwnership/release-before-confirm
    mine=true err=<nil> admitted=true redis_owner_held=false released=[77]
--- FAIL: TestTriageRR0921_04_HandBackPassFitsInsideTheLease (0.41s)
    worst-case hand-back pass = handBackBudget(8) x evictBudget(5s) + projectionWait(5s) = 45s, not below Lease(30s) - RefreshInterval(10s) = 20s
    one renew pass took 409.149125ms with per-eviction wait 50ms; multiple = 8.2 -> at evictBudget=5s that is 40.9149125s of refresh loop held
```

稳定性：RR-03 两个子测试 `-race -count=30` 共 60/60 红；整包 `-race` 只有这两个探针红，既有用例全绿（无数据竞争报告）。

## 3. 需要维护者决定

本轮无条目落入此类。两处可选项（不影响结论）：

- RR-20260921-03 修法 A（Claim 等待进行中的归还）与 B/C（CARRYOVER A7 在途计数 / A6 移交协议）谁先做——审查建议先做 A（局部、可验收），A6/A7 留在 feature 线。
- W-2026-09-18-07 的角色 / 点契约目前只在 `docs/review/IMPLEMENTATION-SCENE-INTEREST-IDENTITY-AND-LIFECYCLE.md` §2，是否补进 `sync/entitysync/policy` 包注释。

## 4. 没核完的

- 无条目未给结论。
- 边界：RR-20260921-03/04 只在单进程替身上复现（与 09-21 的探针同级）；两进程真实 Redis 下对准窄窗口的复现本轮未做（09-22 已说明机器人对不准，仍只有单测探针）。
- CARRYOVER 各项按任务要求只列出，未展开验证。
