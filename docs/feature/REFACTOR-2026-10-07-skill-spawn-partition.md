# 重构：skill 衍生物记录按分区存放，删掉重复的 owned 表

日期 2026-10-07，分支 `skpart`，基线 main `443d08f6`（召唤物改名 `509c381f` 之后）。维护者第十三轮“skill 衍生物两张表”（[DECISIONS-PENDING](../review/DECISIONS-PENDING-2026-10-05.md)）。实施中登记 [RR-20261006-34](../bug/RR-20261006-34.md)，同一提交修复。未发版。
读代码：codebase-memory 共享 generation 停在 09-30，`skill/runtime.go`、`runtime_checkpoint.go` 为 metadata_changed，`spawn_owned.go`、`runtime_spawn_stop.go` 不在图里（10 月新文件），以当前源码为准；两张表的全部读写点用 `rg 'ownedSpawns|\.spawns\b' skill/` 穷举（非测试 13 个文件），改完由 go/types 守卫再核一遍。

命名以 `509c381f` 之后为准：生成真实单位的叫召唤物（Summon），技能驱动的对象叫衍生物（Spawn）。本文只涉及衍生物。

## 1. 维护者决定

DECISIONS-PENDING 第十三轮“skill 衍生物两张表”：选 A——“移交给谁”只由记录字段（`Owner` + `handedOff`）表达，不再有重复索引；原话“如果为了性能可以给spawns分类map，这样也不用扫全部”。要求：

- 衍生物按类别分区存放，每条记录恰好在一个分区里，分区由记录字段决定；
- 只有一个函数负责“改字段并把记录挪到对应分区”，源码守卫禁止别处直接写分区 map；
- `OwnedSpawns(owner)` 等按宿主查询的接口只扫“已移交”分区，按 ID 查询不引入第二份数据；
- checkpoint 只存一份记录列表，恢复时按字段重新分区，删掉 `OwnedSpawns` 的重复存储与恢复时的一致性比对，版本号加 1；
- 行为不变，原有测试除名字外不改断言。

## 2. 之前：两张表

`Runtime.spawns map[SpawnID]*SpawnInstance` 存全部记录，`Runtime.ownedSpawns` 存其中“已移交、仍在运行”的那部分，指向同一批指针：

- 移交（`handoffEntitySpawns`）置 `handedOff = true` 并写进 `ownedSpawns`；停止成功、转入待停止（`requestSpawnStop` / `enterStopPendingLocked`）、超限删除（`makeRoomForStopPendingLocked`）、随 cast 回收（`forgetCastSpawnsLocked`）都要记得从 `ownedSpawns` 再删一次；
- “谁在 owned 表里”与 `Status`、`handedOff` 两个字段表达的是同一件事，靠各处同步维护；`castHasRunningSpawnLocked`、`forgetCastSpawnsLocked`、`rootEventReferencedLocked` 要把两张表各扫一遍；
- checkpoint 写两份（`spawns` 与 `owned_spawns`），恢复时逐条 `reflect.DeepEqual` 比对、再把 owned 表的指针换成 spawns 表的那一个，`stopPendingRecordsValidLocked` 还要核对“待停止的不在 owned 表里”。

## 3. 方案：四个分区

`skill/spawn_table.go` 的 `spawnTable` 持有四个 map，记录所在分区由字段决定（`SpawnInstance.partition()`）：

| 分区 | 字段 | 谁按它查 | 之前怎么查 |
| --- | --- | --- | --- |
| `spawnCasting` 施放中 | `Status == running`，`handedOff == false` | 施法收尾 / 打断 / goto / 失败停衍生物（`stopScopedSpawns`），移交（`handoffEntitySpawns`），施法期间 lifecycle 失效回收（`reapUnhandedEntitySpawns`） | 扫全部 `spawns`，按 `Status == running && !handedOff` 过滤 |
| `spawnHandedOff` 已移交 | `Status == running`，`handedOff == true` | `OwnedSpawns(owner)`，移交后的逐 tick 推进（`advanceOwnedSpawns`）、到期 / 失效回收（`reapInvalidOwnedSpawns`、`reapOwnedSpawns`、`terminateOwnedSpawn`），下一次推进的 tick（`nextOwnedSpawnTick`） | 扫 `ownedSpawns` |
| `spawnStopPending` 待停止 | `Status == stop_pending`（不论是否移交） | 退避重试（`retrySpawnStopsLocked`），待停止上限（`makeRoomForStopPendingLocked`），`RetentionStats` | 扫全部 `spawns`，按 `Status` 过滤 |
| `spawnStopped` 已停止 | `ended` / `cancelled` / `failed` | 只是历史：随 cast 回收、checkpoint、状态快照 | 扫全部 `spawns` |

为什么这样分：每个分区正好对应一组查询的过滤条件，Runtime 的每个 tick 入口（推进、回收、重试、下一次推进的 tick）只扫自己那一个分区，不再扫已停止的历史记录。“仍在宿主侧运行”（`liveOnHost`）是前三个分区的并集（`spawnLivePartitions`）：owned 容量、`castHasRunningSpawnLocked`、`Shutdown`、`RemoveProgram`、presentation reset 按它查。按 ID 查（`get(id, partitions...)`）依次查给出的分区（不给就查四个），没有第二份索引。

“移交给谁”只看记录的 `Owner` 字段：`OwnedSpawns(owner)` 扫已移交分区、按 `Owner` 过滤。没有再按宿主分 map——调用方是查询接口，不在 tick 热路径上，按宿主再分一层就又多一份要同步的结构。

### 3.1 唯一的写入口

| 函数 | 做什么 |
| --- | --- |
| `spawnTable.setState(spawn, status, handedOff)` | **唯一**改分区字段（`Status`、`handedOff`）的地方：从字段对应的旧分区摘下，改字段，放进新分区。记录不在表里（已被 drop，或测试直接构造未入表）时只改字段、不入表——被删掉的记录不能因为一次迟到的状态变化回到表里 |
| `spawnTable.add(spawn)` | 新记录（`startEntitySpawn` 启动的、checkpoint 恢复的）按字段入表；ID 已存在返回 false |
| `spawnTable.drop(id)` | 删记录（随 cast 回收、待停止超限、启动失败且已停） |
| `newSpawnTable()` | 建四个空 map |

调用点：`stopSpawn` 宿主停掉后 `setState(spawn, ended/cancelled/failed, handedOff)`；`enterStopPendingLocked` `setState(spawn, stop_pending, handedOff)`；`handoffEntitySpawns` `setState(spawn, running, true)`。`requestSpawnStop` 里“停掉了就摘出 owned 表”那一支删掉了——分区随字段走。

### 3.2 源码守卫

`TestSpawnPartitionWritesStayInSpawnTable`（`skill/spawn_partition_promises_test.go`）参照 `TestSpawnStopEntriesAreRegistered` 读包源码，但用 `go/types` 做类型检查（约 0.8s），按对象而不是按名字判断：

- `spawnTable.partitions` 只能在 `spawn_table.go` 里引用（分区 map 不外泄，别处拿不到别名）；
- 对任何 `map[SpawnID]*SpawnInstance` 的下标赋值、`delete` 只能在 `setState` / `add` / `drop` / `newSpawnTable` 里——另起一张衍生物索引表会红；
- 给 `SpawnInstance.Status`、`SpawnInstance.handedOff` 赋值（含取地址、整体覆盖 `*spawn = …`）只能在 `setState` 里；创建记录的复合字面量不算（创建之后经 `add` 按字段入表）；
- `Runtime.spawns` 不能被整体重新赋值。

### 3.3 不变量守卫

`TestSpawnPartitionsFollowRecordFields` 在操作序列的每一步之后核对：每条记录恰好在一个分区里、map 的键就是记录 ID、所在分区等于 `partition()`；同时 checkpoint 一次、恢复到新 Runtime，核对恢复出的分区不变量，并要求恢复前后各分区的 ID 列表相同。序列：

- 停止入口登记表 `spawnStopEntries` 的全部 9 行（施法失败、打断、启动失败、owned 事务提交失败、移交时 lifecycle 已失效、施法期间 lifecycle 消失、移交后到期、`RemoveProgram`、`Shutdown`）：施放 / 移交 → 宿主拒绝 → 待停止 → 之后 12 个 tick 逐 tick 推进（退避重试成功 → 已停止），每个 tick 都核对；
- 施放中 → 已移交 → 到期已停止 → 随 cast 回收（`CompletedCastLimit: 1`，夹一个一直运行的召唤），18 个 tick；
- 宿主一直拒绝：三轮“施放 → 移交 → `Shutdown` → 6 个 tick”，覆盖重试到上限（`SpawnStopRetryLimit: 1`）、待停止超过 `MaxStopPendingSpawns: 2` 删掉最早的一条，最后宿主恢复、`Shutdown` 全部停掉。

### 3.4 checkpoint（版本 5 → 6）

- payload 删掉 `owned_spawns`，`spawns` 写全部分区的记录（按 ID 升序，每条一份），分区不进 checkpoint；
- 恢复逐条 `add`，按 `status` / `handed_off` 落分区；删掉两份列表的逐条比对与指针替换；`stopPendingRecordsValidLocked` 不再核对“待停止的不在 owned 表里”（分区由 status 决定，不可能同时在两处）；
- 分区由字段决定，所以恢复时新增两条字段核对：`status` 必须是已知值（`validSpawnStatus`），`handed_off` 只能出现在 entity 衍生物上（只有 `handoffEntitySpawns` 置它，live Runtime 不会产生别的组合）。之前未知 status 的记录会被原样恢复，`handed_off` 与 owned 表不一致的记录靠两份比对挡住；
- `RuntimeCheckpointVersion` 5 → 6，版本 5 及更早得到 `ErrCheckpointUnsupported`（线上未部署，排空后再升级）。既有用例 `unsupported.Version++` 继续覆盖“非当前版本拒绝”。

## 4. 删掉了什么

- `Runtime.ownedSpawns` 字段与 `NewRuntime` 里它的初始化；
- 8 处 `delete(runtime.ownedSpawns, …)` / `runtime.ownedSpawns[…] = …`（移交、`requestSpawnStop`、`enterStopPendingLocked`、`makeRoomForStopPendingLocked`、`forgetCastSpawnsLocked`）；
- `castHasRunningSpawnLocked`、`forgetCastSpawnsLocked`、`rootEventReferencedLocked` 里“两张表各扫一遍”；
- `stopScopedSpawns`、`handoffEntitySpawns`、`reapUnhandedEntitySpawns`、`advanceOwnedSpawns`、`retrySpawnStopsLocked`、`RetentionStats`、`nextOwnedSpawnTick`、`makeRoomForStopPendingLocked` 里按 `Status` / `handedOff` 的过滤（现在由分区表达），以及各处手写的“收 ID → 排序”；
- checkpoint 的 `OwnedSpawns` 字段、`checkpointSpawnMap` 对第二张表的调用、恢复时 `ownedWire` 的逐条 `reflect.DeepEqual` 比对与指针替换（`runtime_checkpoint.go` 不再 import `reflect`）、`stopPendingRecordsValidLocked` 对 owned 表的两段核对。

## 5. 代码量（如实）

非测试代码（`skill/` 下改动的 10 个文件 + 新文件 `spawn_table.go`）：

| | 行数 |
| --- | --- |
| 改动的 10 个既有文件 | 4159 → 4061（+154 / −252，净 −98） |
| 新文件 `spawn_table.go` | +146（注释 26 行、空行 17 行，代码 103 行） |
| 合计 | 净 +48 行 |

即：两张表的同步代码、checkpoint 比对与各处过滤删掉约 100 行，换来一个 146 行的分区类型（含分区说明与写入口注释）。行数没有变少；变的是不变量的位置——之前“owned 表 = 已移交且运行中的记录”要靠 8 处删除和一次恢复比对共同维持，现在由 `setState` 一处保证，守卫在源码与行为两层核对。

测试：既有测试 +32 / −34（只改访问内部结构的写法：`runtime.spawns[id]` → `runtime.spawns.get(id)`，`len(runtime.ownedSpawns)` → `runtime.spawns.count(spawnHandedOff)`，直接写 map 的布置改为 `fileSpawnForTest` / `setState`）；新增 `spawn_partition_promises_test.go` 375 行（守卫两条与辅助函数）、`runtime_spawn_stop_sweep_promises_test.go` 85 行（RR-20261006-34）。

## 6. 行为

不变：原有测试除名字外没有改断言，RR-20261006-21 / 22 / 23 / 30 / 31 / 32 与停止入口统一那组（`TestEveryStopEntryDefersARefusedStopTheSameWay`、`TestSpawnStopEntriesAreRegistered` 等）原样通过。迭代顺序：所有会产生副作用或输出的遍历本来就按 ID 排序，现在仍是（`sortedIDs`）；只做计数 / 存在判断的遍历顺序无关。

变化只有两处，都不是正常路径：

- checkpoint 版本 6，恢复额外拒绝未知 `status` 与非 entity 衍生物上的 `handed_off`（§3.4）；
- [RR-20261006-34](../bug/RR-20261006-34.md)：`Shutdown` / `RemoveProgram` 在同一轮停止里遇到被待停止上限删掉的记录时跳过它，不再空指针 panic（`3fad5b6e` 停止入口统一引入，未发版）。

## 7. 变异证明（未提交）

| 变异 | 结果 |
| --- | --- |
| `setState` 不从旧分区摘下 | `TestSpawnPartitionsFollowRecordFields` 红：`record 1 sits in both casting and stop_pending`、`record 1 (status "stop_pending", handed off false) sits in casting, its fields say stop_pending`（打断、移交时 lifecycle 已失效、施法期间 lifecycle 消失等子用例） |
| `handoffEntitySpawns` 直接 `spawn.handedOff = true`（旧写法，不挪分区） | 源码守卫红：`spawn_owned.go:240:3: Runtime.handoffEntitySpawns: assigns a partition field (SpawnInstance.Status / handedOff); only spawnTable.setState may, so the record moves with it`；不变量守卫同时红（移交后到期、`RemoveProgram`、`Shutdown` 等子用例） |
| 恢复时丢掉 `handed_off`（全部按未移交落分区） | 不变量守卫红：恢复出的 Runtime 与宿主状态不符（`restore: skill: runtime checkpoint does not match host state`） |
| `enterStopPendingLocked` 另记一张 `map[SpawnID]*SpawnInstance` 索引 | 源码守卫红：`runtime_spawn_stop.go:92:2: Runtime.enterStopPendingLocked: writes a map[SpawnID]*SpawnInstance; only spawnTable.setState / add / drop and newSpawnTable may (no second spawn index)` |

四个变异都已撤回，`git diff` 核对过。

## 8. 验证（2026-10-07，`GOWORK=off`，go1.27.0 darwin/arm64，worktree 基线 `443d08f6`；含 RR-20261006-33 / 34）

| 命令 | 结果 |
| --- | --- |
| `gofmt -l`（改动的 .go） | 空 |
| `go vet ./skill/...` | 通过 |
| `go test -race -count=3 ./skill/...` | skill 66s、combat、combatcomponent、skillcompose、skillsync 全部 ok（含 `compile_mutation_property_test.go` 性质测试） |
| `go test -run '^$' -fuzz FuzzParseGeneratedNeverPanics -fuzztime 25s ./skill/` | PASS，231 万次 |
| `go test -run '^$' -fuzz FuzzRestoreRuntimeCheckpointNeverPanics -fuzztime 25s ./skill/` | PASS，59 万次 |
| `skill/examples`：`go vet ./... && go test ./...` | 通过（无测试文件，实跑由根包 `TestExamplesRun` 覆盖） |
| `skill/integration/sync-e2e`：`go vet ./... && go test ./...` | ok |
| 根包 `go test -count=1 .` | ok；`TestExamplesRun` 实跑 6 个示例（含 `skill/examples` 的 combat / fireball / statusbridge）全部 PASS |
| `go build ./... && go vet ./...` | 通过 |
| `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` | rc=0，无违例（本次不涉及这四个包，按要求确认） |

没跑的：codegen 测试、`go generate ./...` 与 game-demo 重新生成——codegen、demo 模板、kit 不引用 `skill` 包的内部结构与源文档 digest，生成形状不变。真实依赖用例不涉及。

## 9. 实施状态

已实施，未发版（分支 `skpart`）。`docs/release/v1.23.0/*` 未改，受影响条目：`guide-cfg-skill-noncore.md` / `impl-cfg-skill-noncore.md` / `_summary-cfg-skill-noncore.md` 的 SKILL-18（写着“格式与版本不变，新旧版本写出的 checkpoint 双向可读”，版本 6 拒绝 5，已不成立）、SKILL-21 与 SKILL-5（checkpoint 恢复口径）；发版文档也还没有收录 10-06 之后的衍生物改名、停止入口统一、召唤物改名与本批（分区存放 / checkpoint 6、RR-20261006-33 源文档 digest 变化、RR-20261006-34）。

## 10. 方向判断（给维护者）

衍生物停止 / 记录生命周期这一块两天内的问题链：RR-20261006-21 / 22 / 23（`5c1f4176`）→ RR-21 后续、RR-20261006-30 / 31（`1ce01e5c`）→ 停止入口统一与 RR-20261006-32（`3fad5b6e`）→ 本次分区存放与 RR-20261006-34。RR-34 是 `3fad5b6e` 引入的：统一之后 `Shutdown` / `RemoveProgram` 也会进待停止，于是第一次碰上“待停止上限删除记录”这条既有机制——修复引出新缺陷，符合“反复出问题”的信号。

判断：根因不在停止状态机本身，而在**上限删除**这一条：它在别的循环进行中删除记录、并让 Runtime 放弃一个宿主仍在运行的衍生物，是这一块唯一“记录在别人手里时消失”的路径（cast 回收只删已停止且无引用的记录）。本次修法（跳过）与它的既有语义一致，但凡是“取 ID 列表 → 逐个处理”的新循环都要记得判空。

候选方向（不在本次范围，交维护者选）：

- **A：维持现状**（本次已做）。代价：新循环要判空；分区不变量守卫与停止入口登记表兜底。
- **B：上限删除改为“放弃”状态而不是删记录**：超限时把最早的条目标成第五个分区“已放弃”（不再重试、不计入上限、随 cast 回收），记录本身不在循环中途消失。代价：多一个状态、进 checkpoint；内存仍随放弃条目增长，需要另一个更大的硬上限或随 cast 回收。
- **C：去掉 `MaxStopPendingSpawns`**，只靠重试上限（exhausted 后不再重试）与 cast 回收约束内存。代价：宿主长时间故障时待停止记录无界增长（每条约数百字节），要靠运维告警发现。

倾向 B，或者在确认宿主故障时记录量可接受后选 C；两者都能删掉“循环中途记录消失”这一类问题。
