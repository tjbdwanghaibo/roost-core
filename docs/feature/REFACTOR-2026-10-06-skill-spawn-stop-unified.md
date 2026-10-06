# 重构：skill 衍生物的停止入口统一走一套“停止 / 待停止”状态机

日期 2026-10-07，分支 `skstop`，基线 main `57c87634`。按 [RR-20261006-21](../bugfix/RR-20261006-21.md) 的后续处理（后续二），实施中登记 [RR-20261006-32](../bug/RR-20261006-32.md)。未发版。
读代码：codebase-memory 共享 generation 停在 09-30（`stop_pending`、`Spawn` 改名等 10 月符号不在图里），以当前源码为准；停止入口用 `rg 'terminateSpawn\(|stopSpawn\(|StopSpawn\('` 在 `skill/` 穷举，守卫测试用 `go/ast` 再核一遍。

## 1. 维护者决定

维护者 2026-10-07，原话“停止入口统一”：skill 衍生物（Spawn）的所有停止入口统一走一套“停止 / 待停止”状态机。每个入口只负责“请求停止”，失败处理只在一个地方；不再按入口分别打补丁。宿主实体生成那套 `SpawnCommand` / `OwnedSpawn*` 可能改名为 Summon，本次**不改**这些名字。

## 2. 为什么：同一处一天出了六条

| 编号 | 提交 | 症状 | 当时的修法 |
| --- | --- | --- | --- |
| [RR-20261006-21](../bugfix/RR-20261006-21.md) | `5c1f4176` | 失败启动删 cast、还 ID，已停衍生物的记录挂到下一个 cast 名下 | 删 cast 前删记录；有停不下的衍生物就保留 cast |
| [RR-20261006-22](../bugfix/RR-20261006-22.md) | `5c1f4176` | presentation reset 的衍生物条目与增量不一致 | 按增量来源填 PrimaryTarget |
| [RR-20261006-23](../bugfix/RR-20261006-23.md) | `5c1f4176` | 已停记录被当作引用，cast 永不回收 | 只把运行中的算引用，回收 cast 时删记录 |
| RR-21 后续 | `1ce01e5c` | 施法失败时宿主停不下，Runtime 就此不管 | `failCastLocked` 之后标 `stop_pending`、退避重试 |
| [RR-20261006-30](../bugfix/RR-20261006-30.md) | `1ce01e5c` | 被钉住的 cast 多于上限时 checkpoint 恢复判 corrupt | 恢复按 prune 的不变量核对 |
| [RR-20261006-31](../bugfix/RR-20261006-31.md) | `1ce01e5c` | tick 驱动的停止被拒，Runtime 的 tick 冻住 | 在两处 tick 入口加 `deferRefusedStopLocked` |
| [RR-20261006-32](../bug/RR-20261006-32.md) | 本次 | 施法里的停止被拒后同一请求再停一次，cancel 回调跑两遍 | 本次统一后消失 |

根因是同一件事：停止失败的处理散在入口里。`failCastLocked` 停完后补一个 `deferUnstoppedSpawnsLocked`，tick 回收的两处各补一个 `deferRefusedStopLocked`，`Shutdown` / `RemoveProgram` 把记录留成 running、错误交给调用方重试（之后 Runtime 不管），施法里的 goto / Cancel / Interrupt / 收尾 / 启动失败清理 / 移交则靠错误一路传到 `failCastLocked` 再停一次——第二次停止又跑一遍回调（RR-32）。每条修复都在给某个入口加分支。

## 3. 方案

一个函数 `requestSpawnStop(cast, spawn, cause, callbackEvent)`（`skill/runtime_spawn_stop.go`）是衍生物停止的唯一入口：

1. 已不在宿主侧（已停止）：直接返回。
2. 停止中：`terminateSpawn`（解除 carry → 区域离开信号 → 回调，后两步只对 running → `stopSpawn` 调宿主 `StopSpawn`）。
3. 之后仍不在宿主侧：摘出 owned 表，结束。
4. 之后仍是 running（宿主拒绝）：`enterStopPendingLocked` 转入 `stop_pending`、摘出 owned 表、排第一次重试、发一条 `spawn_update`。
5. 本来就是 `stop_pending`（重试或调用方再次请求）且仍失败：重试记账不变（次数只由重试累加）。停止原因沿用第一次请求。
6. 错误（宿主拒绝、回调或区域信号出错）照常返回给这一次请求的调用方。

重试（`retrySpawnStopsLocked`）也调用 `requestSpawnStop`；退避、上限、告警、内存上限、checkpoint 字段与 RR-21 后续完全相同，未改。

### 3.1 状态迁移表

| 从 | 事件 | 到 | 副作用 |
| --- | --- | --- | --- |
| running | 任一入口请求停止，宿主已停 | ended / cancelled / failed | 回调与离开信号各一次；`spawn_stop` 表现；摘出 owned 表；entity 衍生物清 Program |
| running | 任一入口请求停止，宿主拒绝 | stop_pending | 回调与离开信号各一次；`spawn_update(stop_pending)`；摘出 owned 表；重试时刻 = 当前 tick + `SpawnStopRetryBackoff`；错误返回这一次 |
| stop_pending | 重试到期，宿主已停 | ended / cancelled / failed | 只重发宿主 StopSpawn；`spawn_stop`；随后 `pruneCompletedCastsLocked`，cast 与记录按 RR-23 回收 |
| stop_pending | 重试到期，宿主拒绝 | stop_pending | 次数 +1，间隔翻倍（最多 64 倍）；到 `SpawnStopRetryLimit` 标 exhausted、计指标、Warn 日志 |
| stop_pending（含 exhausted） | Shutdown / RemoveProgram 再次请求 | 宿主已停 → 已停止；拒绝 → 不变 | 只重发宿主 StopSpawn；重试记账不变 |
| stop_pending | 新条目会使总数超过 `MaxStopPendingSpawns` | 记录删除 | 先丢最早的 exhausted、没有就丢最早仍在重试的；计指标、Error 日志、`spawn_remove` |
| 已停止 | cast 被回收 | 记录删除 | RR-23 |

stop_pending 期间：钉住 cast、ID 不复用（RR-21）；不步进、不派发信号、不跑回调；占 owned 容量；在 `StateSnapshot().Spawns` 与 presentation reset 里可见，不在 `OwnedSpawns` 里。

### 3.2 各入口前后对照

| 入口 | 之前（宿主拒绝时） | 之后 |
| --- | --- | --- |
| 施法失败 `failCastLocked`（经 `stopScopedSpawns`） | 停完后 `deferUnstoppedSpawnsLocked` 扫一遍标待停止 | `stopScopedSpawns` 调 `requestSpawnStop`；删掉 `deferUnstoppedSpawnsLocked` |
| goto / Cancel / Interrupt / 施法收尾（`stopScopedSpawns`） | 记录留 running，错误传到 `failCastLocked` 再停一次，**cancel 回调再跑一遍**（RR-32） | 第一次被拒即 stop_pending；`failCastLocked` 只选 running，不再重停 |
| 衍生物启动失败清理（`startEntitySpawn`、`executeOwnedSpawn`） | 记录留 running，`failCastLocked` 再停一次，**跑了成功路径不跑的 cancel 回调**（RR-32） | 第一次被拒即 stop_pending，不跑回调 |
| 移交时 lifecycle 已失效（`handoffEntitySpawns`） | 记录留 running，下一 tick 的 reap 再停一次，**cancel 回调再跑一遍**（RR-32） | 第一次被拒即 stop_pending |
| tick：施法期间 lifecycle 消失（`reapUnhandedEntitySpawns`） | `terminateSpawn` 出错后补 `deferRefusedStopLocked` | 调 `requestSpawnStop`，删掉补的分支 |
| tick：移交后到期 / 失效 / 步进失败 / area finish（`terminateOwnedSpawn`） | 同上，另在成功分支 `delete(ownedSpawns)` | 只查表 + `requestSpawnStop`；删掉 `stopOwnedSpawn` 转发、`cast` 参数（调用方一律 nil） |
| `RemoveProgram` | 记录留 running、留在 `OwnedSpawns`，错误交调用方重试；Runtime 不再重试（lifecycle 也还在时直到寿命结束） | stop_pending，Runtime 在 tick 上重试；错误照常返回 |
| `Shutdown` | 同上 | 同上，见 §4 |

### 3.3 删掉了什么

- `deferUnstoppedSpawnsLocked`（failCastLocked 之后的补扫）、`deferRefusedStopLocked`（tick 回收的补分支）、`markStopPendingLocked` 改为由 `requestSpawnStop` 唯一调用的 `enterStopPendingLocked`。
- `terminateOwnedSpawn` 与 `reapUnhandedEntitySpawns` 的失败分支，`Shutdown` / `RemoveProgram` 的 `if 出错 … else delete(ownedSpawns)` 分支，`stopOwnedSpawn` 转发函数。
- `failCastLocked` 里的待停止补调用。

### 3.4 代码量（如实）

按 `git diff` 前后五个非测试文件（`runtime_spawn_stop_retry.go` → `runtime_spawn_stop.go`、`spawn.go`、`spawn_owned.go`、`scheduler.go`、`runtime_owned_spawn.go`）计：

| 口径 | 之前 | 之后 |
| --- | --- | --- |
| 代码行（去掉空行与整行注释） | 1524 | 1501（−23） |
| 总行数（含注释） | 1725 | 1722（−3；新增的状态迁移注释抵掉了删掉的代码） |
| 直接调用 `terminateSpawn` 的位置 | 14 | 1（`requestSpawnStop`） |
| 处理宿主拒绝的位置 | 5 处（`failCastLocked` 的补扫、`terminateOwnedSpawn` 与 `reapUnhandedEntitySpawns` 的补分支、`Shutdown` / `RemoveProgram` 的 `else` 分支）；施法里的停止（goto / Cancel / Interrupt / 收尾）、两处启动失败清理、移交不处理，靠 `failCastLocked` / reap 再停 | 1 处（`requestSpawnStop`；`enterStopPendingLocked` 只由它调用） |
| `delete(runtime.ownedSpawns, …)` | 5 | 3（`requestSpawnStop`、`enterStopPendingLocked`、内存上限丢弃） |
| 函数 | — | 删 3 个（`deferUnstoppedSpawnsLocked`、`deferRefusedStopLocked`、`stopOwnedSpawn`），加 1 个（`requestSpawnStop`），1 个改名（`markStopPendingLocked` → `enterStopPendingLocked`） |

代码量只少了一点；收敛体现在“失败处理的位置从 5 处变成 1 处、入口不再有自己的失败分支”，以及守卫把这一点固定下来。测试另加 `skill/spawn_stop_entries_promises_test.go`（418 行）。

## 4. Shutdown 与 RemoveProgram 的语义

**选择：停不下的衍生物照样留成 stop_pending、写进 checkpoint，之后由 Runtime 在 tick 上接着重试；不在 Shutdown 里同步重试。**

理由：

- Runtime 是按 tick 推进的确定性状态机，没有 ctx、不读系统时钟。`Shutdown()` 没有 ctx 参数；“在 ctx 预算内同步重试”要么原地连打宿主（宿主状态不变，几微秒内重试多半还是失败），要么按墙钟等待——等待会让 `RecordingHost` / `ReplayHost` 记录的调用次数随时间变化，回放与 checkpoint 恢复不再确定。
- 宿主方法在 Runtime 锁内调用（Host 契约），调用方可能在快池 / 帧线程上调 `Shutdown`；同步等待会阻塞它（roost-coding“快池内不得阻塞等待”）。
- 同一套状态与 checkpoint 字段已经存在（RR-21 后续），不需要第二套机制；“停机时没有后续 tick”由调用方决定是否还有后续：继续 `Advance`、或 `Checkpoint` 后在新进程 `RestoreRuntime` 再 `Advance`，Runtime 都会按原来的重试时刻接着停。

调用方看到什么：

- `Shutdown` / `RemoveProgram` 仍返回第一个错误（`errors.Is` 宿主 StopSpawn 的错误；宿主的实体清理 `RemoveOwnedEntitiesForMatchEnd` / `ByProgram` 失败同样返回），与之前一样只报这一次。
- 停不下的衍生物在 `StateSnapshot().Spawns` 里是 `stop_pending`，`RetentionStats().StopPendingSpawns` 计数，不再出现在 `OwnedSpawns`（之前留在里面、状态 running）。
- 再调用一次 `Shutdown` / `RemoveProgram` 会立即再请求一次（不消耗自动重试次数，exhausted 的也会再停）；调用方若直接丢弃 Runtime，宿主侧的清理由宿主自己负责，错误已经告诉调用方。

RemoveProgram 之后：程序的 stop_pending 衍生物仍由 Runtime 在 tick 上重试。重试只重发宿主 `StopSpawn`（`SpawnHostState` 与 ID），不执行程序代码，所以程序已移除不影响重试；记录仍引用程序，checkpoint 恢复时 resolver 仍要能解析它（cast 记录本来就引用它，与之前一致）。

## 5. 守卫

`skill/spawn_stop_entries_promises_test.go`：

- `spawnStopEntries` 登记全部停止入口：施法失败、Interrupt（`stopScopedSpawns`）、衍生物启动失败（`startEntitySpawn`）、owned 事务提交失败（`executeOwnedSpawn`）、移交时 lifecycle 失效（`handoffEntitySpawns`）、施法期间 lifecycle 消失（`reapUnhandedEntitySpawns`）、移交后到期（`terminateOwnedSpawn`）、`RemoveProgram`、`Shutdown`。
- `TestEveryStopEntryDefersARefusedStopTheSameWay`：每个入口让宿主拒绝一次 StopSpawn，断言入口返回该错误、记录为 `stop_pending`、同一请求只打一次宿主、不在 `OwnedSpawns`、`StateSnapshot` 可见；再逐 tick 推进 20 tick，宿主恰好在拒绝后第 4 个 tick 被重试一次并停掉，回调次数符合（cancel / end 一次，启动失败的清理零次）。
- `TestSpawnStopEntriesAreRegistered`：用 `go/ast` 读包内非测试源码，调用 `requestSpawnStop` 的函数（除状态机自己的 `retrySpawnStopsLocked`）必须与表里登记的 callers 完全一致；`terminateSpawn` 只能由 `requestSpawnStop` 调，`stopSpawn` 只能由 `terminateSpawn` 调，宿主 `StopSpawn` 只能由 `stopSpawn` 调。**以后新增停止入口必须在表里登记一行并给出触发场景**，否则这条测试红。

变异证明（未提交）：把 `Shutdown` 改回直接调 `terminateSpawn` 加 `else delete(ownedSpawns)`：

```text
--- FAIL: TestEveryStopEntryDefersARefusedStopTheSameWay (0.01s)
    --- FAIL: TestEveryStopEntryDefersARefusedStopTheSameWay/Shutdown (0.00s)
        spawn_stop_entries_promises_test.go:289: spawn status after the refused stop = "running", want stop_pending
        spawn_stop_entries_promises_test.go:296: spawn 1 still listed by OwnedSpawns (status "running"): the runtime would keep stepping it
        spawn_stop_entries_promises_test.go:306: StateSnapshot shows spawn status "running", want stop_pending
        spawn_stop_entries_promises_test.go:316: StopSpawn called at host ticks [0], want [0 4] (the runtime retries after the backoff)
        spawn_stop_entries_promises_test.go:319: spawn status at tick 20 = "running", want stopped by the retry
        spawn_stop_entries_promises_test.go:325: host still runs spawn 1 at tick 20
--- FAIL: TestSpawnStopEntriesAreRegistered (0.03s)
    spawn_stop_entries_promises_test.go:360: spawnStopEntries registers Shutdown, which no longer calls requestSpawnStop
    spawn_stop_entries_promises_test.go:366: Shutdown calls terminateSpawn directly; only requestSpawnStop may (stop entries go through requestSpawnStop)
FAIL
```

## 6. 红绿

修前（基线 `57c87634`，`GOWORK=off go test ./skill/ -run 'TestEveryStopEntryDefersARefusedStopTheSameWay|TestSpawnStopEntriesAreRegistered' -count=1`）：施法失败、tick 到期、施法期间 lifecycle 消失三行已是绿的（RR-21 后续 / RR-31）；其余六行红。

```text
--- FAIL: TestEveryStopEntryDefersARefusedStopTheSameWay (0.01s)
    --- FAIL: TestEveryStopEntryDefersARefusedStopTheSameWay/interrupt (0.00s)
        spawn_stop_entries_promises_test.go:289: spawn status after the refused stop = "cancelled", want stop_pending
        spawn_stop_entries_promises_test.go:292: StopSpawn called at host ticks [0 0] within the request, want exactly one refused call
        spawn_stop_entries_promises_test.go:306: StateSnapshot shows spawn status "cancelled", want stop_pending
        spawn_stop_entries_promises_test.go:316: StopSpawn called at host ticks [0 0], want [0 4] (the runtime retries after the backoff)
        spawn_stop_entries_promises_test.go:328: owned_spawn_callback_cancel ran 2 times, want 1 (a refused stop must not run the callback again)
    --- FAIL: TestEveryStopEntryDefersARefusedStopTheSameWay/failed_spawn_start (0.00s)
        spawn_stop_entries_promises_test.go:289: spawn status after the refused stop = "cancelled", want stop_pending
        spawn_stop_entries_promises_test.go:292: StopSpawn called at host ticks [0 0] within the request, want exactly one refused call
        spawn_stop_entries_promises_test.go:306: StateSnapshot shows spawn status "cancelled", want stop_pending
        spawn_stop_entries_promises_test.go:316: StopSpawn called at host ticks [0 0], want [0 4] (the runtime retries after the backoff)
        spawn_stop_entries_promises_test.go:328: owned_spawn_callback_cancel ran 1 times, want 0 (a refused stop must not run the callback again)
    --- FAIL: TestEveryStopEntryDefersARefusedStopTheSameWay/failed_owned_spawn_commit (0.00s)
        （与 failed_spawn_start 相同的五行）
    --- FAIL: TestEveryStopEntryDefersARefusedStopTheSameWay/handoff_with_a_vanished_lifecycle_entity (0.00s)
        spawn_stop_entries_promises_test.go:289: spawn status after the refused stop = "running", want stop_pending
        spawn_stop_entries_promises_test.go:306: StateSnapshot shows spawn status "running", want stop_pending
        spawn_stop_entries_promises_test.go:316: StopSpawn called at host ticks [0 1], want [0 4] (the runtime retries after the backoff)
        spawn_stop_entries_promises_test.go:328: owned_spawn_callback_cancel ran 2 times, want 1 (a refused stop must not run the callback again)
    --- FAIL: TestEveryStopEntryDefersARefusedStopTheSameWay/RemoveProgram (0.00s)
        spawn_stop_entries_promises_test.go:289: spawn status after the refused stop = "running", want stop_pending
        spawn_stop_entries_promises_test.go:296: spawn 1 still listed by OwnedSpawns (status "running"): the runtime would keep stepping it
        spawn_stop_entries_promises_test.go:306: StateSnapshot shows spawn status "running", want stop_pending
        spawn_stop_entries_promises_test.go:316: StopSpawn called at host ticks [0], want [0 4] (the runtime retries after the backoff)
        spawn_stop_entries_promises_test.go:319: spawn status at tick 20 = "running", want stopped by the retry
        spawn_stop_entries_promises_test.go:325: host still runs spawn 1 at tick 20
    --- FAIL: TestEveryStopEntryDefersARefusedStopTheSameWay/Shutdown (0.00s)
        （与 RemoveProgram 相同的六行）
--- FAIL: TestSpawnStopEntriesAreRegistered (0.04s)
    spawn_stop_entries_promises_test.go:351: no function calls requestSpawnStop: the stop entries do not share the unified stop
FAIL
```

`Shutdown` / `RemoveProgram` 两行就是本任务要求的修前红：宿主停止失败后衍生物残留（running、仍在 `OwnedSpawns`），到 tick 20 Runtime 一次也没有重试，宿主侧仍在运行（场景里宿主的实体清理也失败，lifecycle 实体还在，不会被 reap 顺手再停）。interrupt / 启动失败 / 移交三行是 [RR-20261006-32](../bug/RR-20261006-32.md)。

修后：两条全部通过。`TestOwnedSpawnStopFailureRemainsTrackedForRetry` 是旧的“RemoveProgram 由调用方重试”契约，按新语义改断言（记录 stop_pending、不在 `OwnedSpawns`、再次 RemoveProgram 立即停掉、cancel 回调一次）。RR-21 / 22 / 23 / 30 / 31 与撤除重试的原有用例（`runtime_spawn_stop_retry_promises_test.go` 六个、`runtime_cast_terminal_branches_promises_test.go` 四个、`skillsync/presentation_reset_spawn_promises_test.go`、`runtime_spawn_retention_promises_test.go`）原样通过，未改一行。

## 7. 兼容与行为变化

- API、checkpoint 格式（版本 4）、wire、指标不变。
- `Shutdown` / `RemoveProgram` 停不下的衍生物：之前 running、列在 `OwnedSpawns`；之后 `stop_pending`、不在 `OwnedSpawns`，Runtime 自动重试。依赖“失败后从 `OwnedSpawns` 里找到它再调一次”的调用方改看 `StateSnapshot().Spawns` / `RetentionStats()`，或直接再调一次（仍然有效）。
- 施法里的停止被拒：cancel 回调不再跑第二遍；启动失败清理不再跑 cancel 回调（与宿主停得了时一致）；同一请求里宿主只被打一次，之后按退避重试（之前同一 tick 内再打一次）。
- `terminateOwnedSpawn` 去掉总为 nil 的 `cast` 参数、删掉 `stopOwnedSpawn`（包内私有）。
- 文件 `skill/runtime_spawn_stop_retry.go` 改名为 `skill/runtime_spawn_stop.go`（状态机在这里）。

## 8. 验证（`GOWORK=off`）

| 命令 | 结果 |
| --- | --- |
| `gofmt -l skill` | 空 |
| `go vet ./skill/...` | 通过 |
| `go test -race -count=3 ./skill/...`（含性质测试 `compile_mutation_property_test.go`） | 5 包通过 |
| `go test ./skill/ -run '^$' -fuzz '^FuzzRestoreRuntimeCheckpointNeverPanics$' -fuzztime 30s`；`FuzzParseGeneratedNeverPanics` 15s | 无失败 |
| `skill/examples`：`go vet ./...`、`go test ./...`、`go run ./combat|./fireball|./statusbridge` | 通过，三个示例退出码 0 |
| `skill/integration/sync-e2e`：`go vet ./...`、`go test -race -count=1 ./...` | 通过 |
| 根包 `go test -count=1 .`（含 `TestExamplesRun`） | 通过 |
| `go build ./... && go vet ./...` | 通过 |

未在真实宿主上演练（无外部依赖）。

## 9. 实施状态

已实施（分支 `skstop`），未发版。宿主实体生成的 Summon 改名是下一项，不在本次范围。

## 10. 方向判断（给维护者）

skill 衍生物的停止在 10-06 一天里出了 RR-21 / 22 / 23 / 30 / 31，本次又查出 RR-32，属于 roost-coding“同一机制反复出缺陷”的信号。判断：根因在实现结构（失败处理散在入口、靠错误传播到另一个入口“补一刀”），不是前提错误；“Runtime 对自己启动的东西负责到底”这个方向（维护者 10-06 的 B 选项）本身成立。本次把失败处理收进一个函数、用登记表 + 源码守卫固定下来，之后新增入口在测试里就会红，不再需要逐入口补丁。剩下的风险点是状态仍有两张表（`spawns` 与 `ownedSpawns`），“摘出 owned 表”要和状态同步；如果之后这里再出问题，候选方向是把 owned 表改为由 `spawns` 按 `handedOff && Status == running` 推导（去掉第二张表，代价是 `nextOwnedSpawnTick` / `OwnedSpawns` 每次扫全表，规模受 `MaxOwnedSpawns` 约束）。
