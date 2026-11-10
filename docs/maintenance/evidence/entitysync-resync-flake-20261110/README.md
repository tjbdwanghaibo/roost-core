# entitysync：on_change 冻结在“忙”时升成全量（建筑用例偶发全量）证据

分支 `fix/entitysync-resync-flake`，基于 main `7da1f62e`。对应 KNOWN-LIMITS 顶部“建筑进场景主视野后的 demo 尺度”条目里的“尚未确认”，以及[建筑进主视野证据 §3](../building-main-interest-20261110/README.md#3-偶发第二次改参数以全量发出尚未确认)。机器：Apple M5（macOS），本机有后台负载、交换区接近用满；下面的数字都是用例通过 / 失败计数，不是性能结论。

生成工程的做法同[建筑进主视野证据](../building-main-interest-20261110/README.md)：`roost project new gd … -template game-demo --skip-deps`，go.mod 追加 `replace github.com/tjbdwanghaibo/roost-core => <core>`，`go mod tidy`。下文命令在生成工程根目录或 roost-core 根目录里跑，`GOWORK=off`。

## 1. 结论

**已证实**：

- 现象来自 `SubjectSyncState.FreezeSyncViews`（on_change 提交时在 Entity 锁内冻结内容）。修前它在“冻结槽已有内容”或“同一 subject 的捕获在途（Flush 已捕获、尚未提交版本）”时，把这次冻结一律升成 `Full`（原因 Resync）。在途捕获提交、脏位按代际清掉之后，这份冻结留在槽里不释放；下一次变化冻结时又因为“已有冻结”升成全量。
- 用例里的触发者是判重的第二次 `SetParams(seq 3)`：它不改同步字段，但实体此刻仍脏着（第一次改动还没提交版本），`Admit` 照样冻结一次：
  - 落在第一次改动的冻结尚未被 Flush 取走时：第一次改动以全量发出。§27.3 末条记的“放置后第一次改参数可能以全量发出”就是它。修前不加负载 19/20 次如此，见 §3。
  - 落在后台复制循环推送第一次改动、尚未提交版本的窗口里：第一次改动照常是增量，但留下一份过时冻结，第二次改动（`SetParams(seq 4)`）以全量发出。这就是本条偶发。
- 与 `preserveDirty` 无关：它只由 `PrepareSnapshotViews` 设置，而 `PrepareSnapshotViews` 只用于周期类型（TypeSchedules 的 periodic）窗口内的即时快照；建筑、玩家、怪都是 on_change。
- 是框架缺陷，不是设计内必须全量的时序：修前的全量只是多发了字节，内容一直正确；但“已有基线的观察者收增量”的承诺在合法时序下被打破，并且过时冻结会一直占着 `MaxFrozenBytes`，要等这个 subject 的下一次变化才归还。

**尚未确认**：建筑进主视野（`db33ebeb`）之后，这个偶发在重负载下更常见（之前 0/400，之后约 1/50～1/400）。具体是哪段时序变宽的，没有查。它只影响窗口的宽窄，不影响根因：修前的代码在不加负载时同样会触发（见 §3）。

## 2. 复现

### 2.1 确定性复现（entitysync 单元层，不靠计时）

`manager_lifecycle_test.go` 的 `pushHookTransport` 在下一次 `Push` 之前运行一个钩子。此时 Flush 已经捕获、还没有提交内容版本，在钩子里提交就是“提交落在一次在途捕获里”的确定次序。`commitChange` 模拟 `SyncMutation.Admit`：有同步变化时标脏，实体脏着就 `CaptureSync`（冻结）。mask 为 0 的提交就是判重、什么都不写的那笔事务。修前原文（行号是红跑时的文件）：

```
--- FAIL: TestACommitDuringAnInFlightCaptureLeavesTheNextChangeADelta (0.00s)
    manager_lifecycle_test.go:447: 1 frozen byte(s) still held after the change they froze was delivered
    manager_lifecycle_test.go:452: the second change went out as full=true reason=2 1→2 payload [2], want a delta 1→2 carrying 2
--- FAIL: TestAChangeCommittedDuringAnInFlightCaptureGoesOutAsADelta (0.00s)
    --- FAIL: TestAChangeCommittedDuringAnInFlightCaptureGoesOutAsADelta/the_in-flight_capture_commits (0.00s)
        manager_lifecycle_test.go:471: the change committed during the capture went out as full=true reason=2 1→2 payload [2], want a delta 1→2 carrying 2
    --- FAIL: TestAChangeCommittedDuringAnInFlightCaptureGoesOutAsADelta/the_in-flight_capture_is_retried (0.00s)
        manager_lifecycle_test.go:491: both changes after the retry went out as full=true reason=2 0→1 payload [2], want a delta 0→1 carrying 2
--- FAIL: TestTwoCommitsBeforeAFlushCoalesceIntoOneDelta (0.00s)
    manager_lifecycle_test.go:510: two coalesced changes went out as full=true reason=2 0→1 payload [2], want a delta 0→1 carrying 2
```

第一条就是生成工程用例的序列：快照 → 第一次改动 → 推送时插入一笔不改字段的提交 → 第二次改动。第二次改动以全量（reason 2 = Resync）1→2 发出，与偶发原文（`full:true componentBytes:132`）相同。

`framework/entity` 的 `TestAFreezeDuringAnInFlightCaptureNeverReplaysTheCapturedOperations` 用一个按“确认之后的操作记录”出增量的 packer（模拟生成的变化树 packer）守住修法的安全边界。修前红（`full=true 1→2 ops "ab"`）是因为修前给的是全量，内容不错。

### 2.2 生成工程：放宽时序窗口

在 Flush 的推送与内容提交之间临时加 30ms 延时（只在诊断环境变量下生效，未提交），并在 `FreezeSyncViews` 升全量处打日志。不改用例时：

| 代码 | 次数 | 结果 |
| --- | --- | --- |
| 修前 | `-count=20` | 4 次失败，原文同偶发（`the second parameter change went out as [{… full:true componentBytes:132 payloadBytes:53 params:true}]`） |
| 修前 | `-count=30 -v`，带诊断日志 | 3 次失败 |
| 修后 | `-count=30` | 0 次失败 |

带诊断日志的 30 次里，3 次失败全部是同一个两步序列（subject 是 hall）：

```
DIAG freeze busy subject=… inflight=true frozen=false frozenGen=<nil> gen=2 version=1 dirty=8
DIAG freeze busy subject=… inflight=false frozen=true frozenGen=2 gen=3 version=2 dirty=8
```

- 第一步：判重的 `SetParams` 在第一次改动的捕获在途时冻结。代际没变，是不改字段的提交；升成全量。
- 第二步：第二次改动冻结时，槽里还是第一步那份第 2 代、版本 1 的过时冻结，而此时状态已是版本 2；又升成全量。

另外 27 次通过，各有一条 `inflight=false frozen=true frozenGen=2 gen=2 version=1`：判重提交落在第一次改动的冻结尚未被取走时，第一次改动以全量发出。旧用例允许第一次是全量，所以这 27 次都算通过。

## 3. 修法与前后对照

修在 `framework/entity`（entitysync 的内容层）：

- `FreezeSyncViews` 不再因“已有冻结 / 交付在途”升全量。冻下的就是此刻一次普通捕获会得到的内容：增量从当前版本起，带全部未确认的变化，整份替换槽里的旧冻结。
- 正确性靠 `frozenSubjectSync.prepare` 已有的三项校验：内容代际、CommitLSN、增量的基线版本。
  - 在途捕获提交后版本前进，这份增量被拒绝复用，Flush 在 Entity 锁内按新基线重新捕获，只带确认之后的变化，列表操作不重放。
  - 在途捕获被整轮重试作废时，版本不变，原样复用。
- 批次 `Commit` 清脏位时，一并释放同代际的过时冻结。
- 代价：交付在途时提交的变化，Flush 多取一次 Entity 锁、多打包一次增量。修前是每个观察者多收一份全量。

game-demo 的 `TestBuildingsReachAnotherClientOverRealTCP` 改为两次改参数都必须是增量。原来第一次允许全量，理由是 preserveDirty 的那段注释已改正。

| 条件 | 修前（新用例） | 修后（新用例） |
| --- | --- | --- |
| 不加负载，`-count=20` | 20 次全红：19 次第一次改动全量，1 次第二次改动全量 | 0 / 20 |
| 放宽时序窗口（§2.2），`-count=30` | — | 0 / 30 |

重负载对照：两路整包 `-race` 测试循环作负载（按 pid 起停），`-race` 测试二进制 `-test.run 'TestBuildingsReachAnotherClientOverRealTCP$' -test.count=100 -test.cpu 1,2`，修前 / 修后交替两轮。修前用 `7da1f62e` 生成的工程（旧用例，只断言第二次改动），修后用本分支生成的工程（新用例）：

| 轮次 | 修前（旧用例） | 修后（新用例，第一次改动也必须是增量） |
| --- | --- | --- |
| 第 1 轮 | 200 通过 / 0 失败 | 200 / 0 |
| 第 2 轮 | 200 / 0 | 200 / 0 |

这次重负载下，修前的旧用例 400 次没有撞上偶发。原始频率约 1/50～1/400，没撞上不说明什么，所以修前的失败证据以 §2 的确定性复现、放宽窗口和上表的不加负载对照为准。修后的新用例更严：旧代码不加负载就 20 / 20 红，在这次重负载下 400 次全部通过。

## 4. 变异

| 变异 | 结果 |
| --- | --- |
| 恢复修前的“忙时升全量” | entitysync 4 处红（§2.1）；生成工程新用例不加负载 20 / 20 红 |
| 去掉 `frozenSubjectSync.prepare` 对增量基线版本的校验 | `TestAFreezeDuringAnInFlightCaptureNeverReplaysTheCapturedOperations` 红：`full=false 1→2 ops "ab", want a delta 1→2 with only "b"`（把在途捕获已交付的操作重放一次）；`TestFrozenContentCoalescesAndRetainsBudgetUntilSettlement` 红：`packed 3 time(s), want 3 freezes and one recapture on the new base` |
| 去掉 `Commit` 里对同代际过时冻结的释放 | `TestACommitDuringAnInFlightCaptureLeavesTheNextChangeADelta` 红：`1 frozen byte(s) still held after the change they froze was delivered` |

`TestFrozenContentCoalescesAndRetainsBudgetUntilSettlement` 原来断言“在途时冻成 Full、Flush 不重新打包”，这是修前的取舍。现在改为：增量、基线 1、内容是最新值，多一次重新打包。

## 5. 验证

VERIFY_PLACEHOLDER
