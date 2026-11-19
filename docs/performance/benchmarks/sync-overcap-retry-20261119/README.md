# 超过每 subject 订阅者上限后的重试：修前修后证据（2026-11-19）

方案与实施结果：[FEATURE-2026-11-19-sync-overcap-retry](../../../maintenance/FEATURE-2026-11-19-sync-overcap-retry.md)。

- 机器：Apple M5（10 核）、32 GB、macOS；同机另有其他会话的负载（Unity 编辑器、另一个 agent 的测试），开始时 loadavg 4～6，守卫基准顺序跑时一度到 22～32，见 `host.txt`。
- 修前 = `e7ae5ceb`（只改了基准：联盟基准上限随人数提高、新增超上限基准）；修后 = `f4117a45`（批 2 + 批 3）。两侧都用 `go test -c` 编出的测试二进制，在 policy 包目录运行。
- 联盟基准：`-test.bench 'RelationViewAlliance(Flush|OverCap)' -test.benchtime 20x -test.count 6`，修前跑两轮（07:54、08:16）、修后两轮（08:12、08:21），交替进行，每侧合并成 12 个样本（`before-alliance.txt` / `after-alliance.txt`，脚本 `run.sh`）。
- 回归守卫：`RelationViewMemberChange|GroupViewFlush|ClusterFlushRandomWalk$`，先按 `run.sh` 顺序跑各 6 次（`*-g-guards.txt`，修后那段负载 22～32，结果被负载污染，见下），再按 `guards-interleaved.sh` 逐次交替各 6 次（`*-gi.txt`）。

## 联盟基准（benchstat-alliance.txt）

| 档 | 修前 | 修后 | 变化 |
| --- | --- | --- | --- |
| OverCap 2000 人、缺省上限 1024：每轮耗时 | 985 ms ± 18% | 6.40 ms ± 23% | −99.35%（p=0.000） |
| 同上：每轮分配 | 570.8 MiB / 299,871 次 | 1.125 MiB / 6,937 次 | −99.80% / −97.69% |
| 同上：收敛后堆增量 | 3.588 GiB | 3.359 GiB | −6.37%（每个被拒 pair 不再有 `retry` + `refused` 两条记录） |
| Flush 1000 人（上限内）：每轮耗时 | 3.34 ms ± 26% | 3.15 ms ± 46% | ~（p=0.932） |
| Flush 2000 人（上限 2016）：每轮耗时 | 14.5 ms ± 17% | 14.3 ms ± 17% | ~（p=0.347） |
| Flush 两档：分配 / 字节 / 堆 | — | — | 全部 ~（分配次数逐样本相同） |

验收口径（方案 §5.3）：
- OverCap 降到上限内档的量级：6.4 ms，低于同机 Flush 2000 档（14.3 ms）。达到。
- 每轮分配不超过 2 倍 2 MB：1.125 MiB。达到。
- 上限内各档无显著变化：两档都是 `~`。但本机负载下单档离散度 17%～46%，benchstat 只能说明没有可检出的变化，测不出 ±3% 这一级；分配次数与字节逐样本相同，说明上限内路径没有多出分配。
- WARN 行数：基准把日志丢弃了；由 `TestSubscriberRoomWarnIsAggregated` 管住（200 个观察者 30 次 Flush 只有 1 条汇总，修前 198 条）。

## 回归守卫

- 交替跑（`benchstat-guards-interleaved.txt`）：耗时、分配字节、分配次数 11 档全部 `~`，耗时几何平均 −1.92%。
- 顺序跑（`benchstat-guards-sequential.txt`）：修后那段在负载 22～32 下运行，`ClusterFlushRandomWalk` +65%、`RelationViewMemberChange` 各档 +15%～+58%（`GroupViewFlush` 有的档反而 −9.6%）。分配次数前后相同，交替复测耗时也没有差异，判定为负载造成；原始数据保留不删。

## 先红后绿与变异

- `red-before-fix.txt`：修前输出。Manager 侧与 Interest 侧用例在 `e7ae5ceb` 的源码上运行，只覆盖了新增的只读 API（`SubscriberRoom`、`MaxSubscribersPerSubject`、`SubscriberLimitRefused`，不含 `!pinned`）让新用例能编译，另加一个只定义 `waitWarnInterval` 常量的测试文件；GroupView 用例在批 2 提交 `e5292281` 上运行。
  - `TestOverCapPairSubscribesInTheSettleAfterRoomIsFreed` 的“下一次 Flush 订阅上”部分修前就通过（每轮都重说），是守卫；修前失败的是同一用例里新增的 `subscriber_room` 诊断断言。由变异 M2 证明它能变红。
- `mutations.txt`：M1～M7（方案 §5.2）与 G1（GroupView 不看空位）逐个应用、跑用例、还原：
  - M1 不看空位全部重说 → `TestOverCapPairsWaitForRoomInsteadOfBeingSaidEverySettle` 红；
  - M2 删掉等空位处理 → 空位后订阅（Flush 与 on_change 两个子用例）、不插队、Drain 三条红；
  - M3 结算不检查是否有人排队 → `TestANewPairDoesNotJumpTheRoomQueue` 红；
  - M4a 结算不看 `hold.wait`（等待中的 pair 也直接说）→ `TestARetriedPairTouchedAgainIsSaidOncePerSettle` 红；M4b 只去掉 `queueRetryLocked` 的重复入队守卫 → 没有用例变红：现有调用点都先把 `wait` 置回 `waitNone` 才调用它，守卫目前不可达，只是不变量的防线；
  - M5 去掉 `!pinned` → Manager、Interest、SelfView 三条钉住用例红；
  - M6 每个 pair 一条 WARN → `TestSubscriberRoomWarnIsAggregated` 红；
  - M7 `queuedPending` 不看空位 → `TestOverCapWaitingHoldsDrainOnlyWhileThereIsRoom` 红；
  - G1 GroupView 不看空位 → `TestGroupViewMembersWaitForSubscriberRoom` 红。
- 方案 §4.5 的“尚未确认”项（周期类型 subject 的 remove 是否受节奏影响）：`TestTypeScheduleRemoveFreesSubscriberRoomWithoutWaitingForTheInterval` 在周期 1 小时、时钟不前进时，一次 Flush 后空位就出现，确认 remove 不等周期，G2 的时延口径对周期类型同样成立。
