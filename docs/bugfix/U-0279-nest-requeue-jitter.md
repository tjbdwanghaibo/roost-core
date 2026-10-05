# U-0279：暂时性冲突的重新准入固定 5ms，对称交叉创建靠调度噪声解开，正常负载下常常耗尽 400 次上限

**仓库 / 位置**：roost-core `nest/group_transition.go` `requeueNestDispatch`（新增 `transientRequeueDelay`）。
**来源**：v1.20.0 整体验证 `bash scripts/test-dataengine-generated.sh` 的一次失败（维护者 2026-10-05 授权“确认是缺陷就按先红后绿修”）。
**没有 RR 编号**；是 [RR-20260926-48](RR-20260926-48.md)“未验证项 / 风险”第一条与 [OPEN-ITEMS-2026-09-27](../review/OPEN-ITEMS-2026-09-27.md) B24 / C09 的落地：
C09 的预案是“先做 B24；只有出现耗尽 400 次时才加抖动”，当时 3100 次试验 0 耗尽、没有加；这次在真实 WAL + Mongo 的生成工程里出现了耗尽。
**定位文档**：TROUBLESHOOTING T-220。

## 现象

```text
=== RUN   TestGeneratedDataEngineCrossCreateResolvesOnRealWAL/strict
    create_in_handler_test.go:264: side 0 exhausted the requeue budget after 401 attempts: nest: lock timeout: nest: created entity is locked by another holder: entity 37093124 cannot be waited for in lock order
=== RUN   TestGeneratedDataEngineCrossCreateResolvesOnRealWAL/pipelined
    create_in_handler_test.go:272: policy=pipelined winner=side 1 attempts=74/73
```

用例（`codegen/internal/entity/testdata/dataengine/create_in_handler_test.go:196`）：两条可回滚（RollbackState）消息在 handler 内交叉新建
X、Y（一侧先 X 后 Y，另一侧先 Y 后 X），首轮两侧都持有自己的第一个新实体后才建第二个，承诺“一方成功、一方 `ErrEntityExists`，
没有请求以锁超时耗尽”。失败时一侧跑满 1 + 400 次，调用方收到 `ErrLockTimeout`；同次 pipelined 虽然通过也重试了 73 轮。

## 复现与对照（重复跑同一用例）

方法：按 `scripts/test-dataengine-generated.sh` 生成同一工程（正式 DAO / Entity 生成、`-race` 编译、隔离库 `roost_generated_it_<pid>_<ts>`、WAL 在临时目录），
`ROOST_GENERATED_PHASE=1 ./persist.test -test.run '^TestGeneratedDataEngineCrossCreateResolvesOnRealWAL$' -test.count N`，结束后跑脚本的 cleanup 相删库。
v1.20.0、v1.19.2 各一个 detached worktree，修后为本修复的 worktree；三侧**交替**运行（每轮每侧 100 次），抵消同机其他会话的负载变化。
用例只有 strict / pipelined 两种策略（没有 async）。“最多执行次数”取两侧中较大的一个，失败行的 401 不计入分布。

负载分两种条件分开统计（同机另一个 agent 在 12:10～12:50 左右做过满载压测：20 个 `yes` 加两路 `go test -race`；我的压力组另加 10 个忙循环）：

- **高负载**：12:12～12:51（load average 17～40），含 v1.20.0 / v1.19.2 各 300 次顺序跑、三侧 5 轮 × 100 交替、10 个忙循环下 3 轮 × 100 交替；
- **正常负载**：12:53 之后、压测结束（load average 3～6），三侧交替；12:53 前后负载下降过程中的一轮 v1.20.0（200 次，strict 0 / pipelined 3 失败、最多 296 次）单列不计。

| 条件 | 版本 | strict 失败 | strict 执行次数 p50 / p90 / p99 / 最多（通过的样本） | pipelined 失败 | pipelined 执行次数 p50 / p90 / p99 / 最多 |
| --- | --- | --- | --- | --- | --- |
| 正常负载 | v1.20.0 | 174 / 400（43.5%） | 154 / 344 / 399 / 400 | 212 / 400（53.0%） | 190 / 356 / 400 / 401 |
| 正常负载 | v1.19.2 | 139 / 400（34.8%） | 53 / 304 / 393 / 401 | 146 / 400（36.5%） | 74 / 317 / 397 / 401 |
| 正常负载 | 修后 | **0 / 1000** | 3 / 4 / 7 / 18 | **0 / 1000** | 2 / 4 / 6 / 12 |
| 高负载 | v1.20.0 | 0 / 1100 | 4 / 8 / 17 / 346 | 0 / 1100 | 4 / 8 / 16 / 341 |
| 高负载 | v1.19.2 | 0 / 1100 | 5 / 9 / 33 / 250 | 3 / 1100（0.3%） | 4 / 8 / 32 / 377 |
| 高负载 | 修后 | 0 / 800 | 3 / 4 / 6 / 8 | 0 / 800 | 2 / 3 / 4 / 5 |

“最多 401”是通过的样本里输家第 401 次执行才拿到 `ErrEntityExists`（恰好压线）。v1.20.0 与 v1.19.2 在正常负载下的差异来自运行时段的负载差别
（v1.19.2 第一组在 load 4～6 时跑），两版这条路径代码相同（见下）。

v1.19.2 与 v1.20.0 之间，用例、`nest/group_transition.go`、`nest/rollback.go`、`entity/entity_guard.go`、`entity/manager_factory.go` 都没有改动
（`git diff --stat v1.19.2 v1.20.0 --` 这些路径为空）：**是既有问题，不是 v1.20.0 回归**。用例由 `6b1d1dcb`（B27 第 2 批，2026-09-30）加入；
固定 5ms / 400 次来自 RR-20260926-48 的修复，B24 统计后（OPEN-ITEMS C09）决定不加抖动。

## 根因

交叉创建的冲突路径本身符合 RR-48 的设计：新实体与调用方同锁组，按锁序不能等待，`entity/entity_guard.go:386` `lockCreated` 只 try-lock，
被占用时 `nest/rollback.go:866` `CreatedEntityLockBusy` 给出带 `ErrLockTimeout` 的冲突错误，handler 结束时整条回滚（撤销已发布的第一个新实体），
`nest/nest_dispatch.go:114` → `requeueTransientDispatch` → `requeueNestDispatch` 把消息重新准入。快池不等待、锁序不成环，这两条都没问题。

问题在“重新准入”没有任何打破对称的机制，是**活锁**，不是饿死：

1. 修前 `nest/group_transition.go:393` 用固定的 `entityGroupDispatchRequeueDelay`（5ms）。两侧在同一轮里因同一冲突失败，失败时刻只差 δ（几十微秒），
   5ms 后重新准入时仍只差 δ——固定延迟原样保留两侧的错开，不放大它。
2. 延迟队列只有一个定时器（`nest/dispatcher.go:424` `delayLoop` / `:475` `nextDelayedWait`）：醒来时把所有已到期的消息逐个 `sendMsg`（`:471`），
   中间不等待。定时器晚醒（进程停顿、GC、`-race`）超过 δ 时，两侧被背靠背放回队列，δ 被压回几微秒——已经错开的两侧会被**重新对齐**。
3. 每一轮两侧几乎同时开始，各自先建自己的第一个实体（不冲突），再建对方那个（必冲突），再一起回滚、一起重排。
   只有调度噪声偶然把 δ 推到超过冲突窗口（一次 `Create` 加回滚 / 撤销收尾的时间）时，先到的一侧才能连建两个、胜出。
   这是随机游走式的逃逸，没有上界，**唯一的逃逸途径是调度噪声，所以机器越安静越严重**：同机满载期间（其他会话压测、load 17～40）多数 3～5 轮解开、
   失败罕见；回到正常负载（load 3～6）后两侧长时间保持同步，v1.19.2 / v1.20.0 失败率升到 25%～55%（见上表）。
   原始报告推测“负载偏高导致失败”，实测方向相反：高负载下的失败只是这一活锁在噪声较少的间隙里偶发。

strict 并不比 pipelined 更容易出事：冲突、回滚、重排都发生在 handler 内、`prepareCommitRecord` 之前，失败的轮次不产生 WAL 记录，
持久策略只影响最终胜出那一轮的提交。strict 胜者在锁内等 fsync 期间，输家的重试会再撞几次锁（每次 5ms），只是常数项。
对照数据里两种策略的分布与失败没有系统差异（v1.19.2 的 3 次失败都在 pipelined）；原始报告里 strict 失败是单个样本。

**400 次 / 5ms 的预算本身是合理的**：它对应约 2s 的重排窗口，足以等完别的持有者的正常提交与撤销收尾；问题不在预算太小，
而在固定延迟让“对称的两侧下一轮仍对称”这件事每轮都以接近 1 的概率成立，任何有限预算都只是把失败推到更长的尾部。
因此不放宽预算、不改用例断言——那会掩盖“没有打破对称”的缺陷。

## 修法

`requeueNestDispatch` 的延迟改为 `transientRequeueDelay()`：下限仍是 5ms，加 `[0, 5ms)` 的均匀抖动（`math/rand/v2` 的 `rand.N`，并发安全、无分配）。

- 每次重排独立取样，两侧下一轮仍落进同一冲突窗口（含定时器晚醒造成的对齐）的概率 p 小于 1 且与上一轮无关，连续 400 轮都撞上的概率是 p^400。
  即使冲突窗口 + 定时器延迟有 2.5ms（p ≈ 0.75），p^400 ≈ 1e-50。
- 下限不变，400 次上限对应的最短重排窗口（约 2s）不缩短；平均延迟 5ms → 7.5ms，用尽上限的总时长约 2～4s。
- 作用于这条函数的全部调用方（`lock_timeout`、`group_changed`、`group_transition_pending`）：它们都是“等别的持有者交还”的暂时性冲突，
  对称冲突同样适用。组迁移自己的重试（`entityGroupTransitionRetryDelay`）不经过这里，未改。
- 执行契约：延迟只决定消息何时回到准入队列，不占用 worker、不持锁、不在快池等待；锁序、同 ID 顺序、RR-49 / RR-64 / RR-73 的“哪些消息可以重排”都未改。

### 未采用

- **放宽用例预算或断言**（如允许 `ErrLockTimeout`、把上限调大）：承诺“交叉创建最终一方胜出”是框架给业务的，用例只是把它钉住；放宽等于接受活锁。
- **确定性的打破对称（wait-die：按消息年龄，年轻的一方挂起到持有者结束再重排）**：能给确定性保证，但要把“锁的持有者”映射回消息、
  在调度器里加挂起 / 唤醒通道，涉及准入资格与同 ID 顺序，改动面远大于本缺陷；随机抖动已把耗尽概率降到可忽略，且是 C09 预先认可的修法。
- **指数退避**：会拉长所有暂时性冲突（包括正常的 Cast 锁冲突）的尾延迟，而对称冲突只需要打破对称，不需要退避。

## 改动

- `nest/group_transition.go`：新增 `transientRequeueDelay`，`requeueNestDispatch` 改用它；常量注释说明 5ms 是下限。
- `nest/requeue_jitter_promises_test.go`（新增）：确定性回归，见下。
- `nest/cross_create_requeue_budget_promises_test.go`：文件头注释补“更正”，指向本记录。

## 证明

确定性红测试 `TestSymmetricTransientRequeuesAreNotReadmittedInLockstep`：不启动延迟循环、不等待时间，直接看 `requeueNestDispatch` 写进延迟堆的到期时刻。
64 条同原因、同重排次数的消息在同一轮重排，要求每条延迟不短于 5ms 下限，且彼此错开（用调用前后两次 `time.Now` 夹住，max(下界) − min(上界) > 2.5ms）。
固定延迟时这个差值恒 ≤ 0，与机器快慢无关。

修前（红）：

```text
--- FAIL: TestSymmetricTransientRequeuesAreNotReadmittedInLockstep (0.00s)
    requeue_jitter_promises_test.go:61: 64 messages that hit the same conflict in the same round are re-admitted in lockstep: delays spread at most 0s (shortest <= 5.000459ms, longest >= 4.999917ms), want > 2.5ms so symmetric conflicts break up
FAIL
FAIL	github.com/tjbdwanghaibo/roost-core/nest	1.095s
```

修后（绿）：

```text
=== RUN   TestSymmetricTransientRequeuesAreNotReadmittedInLockstep
--- PASS: TestSymmetricTransientRequeuesAreNotReadmittedInLockstep (0.00s)
PASS
ok  	github.com/tjbdwanghaibo/roost-core/nest	0.706s
```

生成工程端到端（上表“修后”两行）：正常负载 strict / pipelined 各 1000 次、高负载各 800 次，0 失败，单侧执行次数最多 18 次（修前正常负载下中位数就有 53～190 次）。

## 验证

全部 `GOWORK=off`，在修复 worktree（基线 `3f29a921`）：

- `go test -race -count=3 ./nest`：ok（251 个顶层用例 × 3）；`go test -race -count=200 -run 'TestSymmetricCrossCreatePairsResolveWithinRequeueBudget|TestSymmetricTransientRequeuesAreNotReadmittedInLockstep' ./nest`：ok。
- 根包 `go test -count=1 .`：ok。
- `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync`：无输出、退出 0。
- `go build ./... && go vet ./...`：通过；`gofmt -l nest entity` 空。
- `bash scripts/test-dataengine-generated.sh`（隔离环境 `~/.roost-it/roost-dataengine-it`，库名 `roost_generated_it_<pid>_<ts>`、WAL 在临时目录，脚本结束删库）：三相全部 PASS，
  交叉创建 strict 3/2、pipelined 2/3 次。
- 隔离核对：生成工程测试 `reloadTestClient` 拒绝非 `roost_generated_it_` 前缀或占位库名；复现脚本与原脚本相同地生成库名、结束跑 cleanup 相删库，没有写 game / saga / remote_entity 库或 `ROOST_*` 流。

代码图谱（codebase-memory，项目 `Users-whb-roost-roost-core`）索引代际 2026-09-30，`nest/rollback.go`、`nest/nest_dispatch.go`、`entity/entity_guard.go`
报 metadata_changed，相关结论以当前源码为准；`requeueNestDispatch` 的调用方（`requeueTransientDispatch`、`requeuePendingEntityGroupTransition`，经 `dispatchNest`）由图谱 trace 与源码一致。

## 未验证项 / 风险

- 抖动给的是概率保证，不是确定性保证：p^400 可以忽略，但不是 0。需要确定性保证时走 wait-die（见“未采用”）。
- 平均重排延迟从 5ms 变为 7.5ms：高冲突业务（同一实体被大量消息 Cast）重排后的完成时间会略长；没有做性能对照（重排只在冲突时发生，不在正常路径上）。
- 统计在单机 macOS（Apple M5，同机有其他会话负载）上做，其他平台、更多对交叉创建同时发生的情况没有测。
