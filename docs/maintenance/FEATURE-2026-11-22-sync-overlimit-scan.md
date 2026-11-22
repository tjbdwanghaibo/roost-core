# 方案：超额暂缓订阅很多时的 flush 成本（Sync，2026-11-22）

**状态：暂缓（维护者 2026-11-22“先不做”）。** 方案保留，以后随时可按 §8 复测、再决定做不做。来源：M-16 1000 人 c4 在负载结束后 60s 内没有收敛（[追查记录](../performance/benchmarks/m-series-rest-20261122/README.md#追查-1000-人-c42026-11-22)），维护者要求“先出优化方案”。Sync 是 core 三大块之一，确认前不改代码。

- 基线：main `1b12ce0c`，行号都对应这个提交。codebase-memory-mcp 本次连不上（connection closed），调用链靠直接读源码整理。
- 证据：CPU profile 已入库 [`m16prof-c4/baseline-1b12ce0c.cpu.pprof`](../performance/benchmarks/m-series-rest-20261122/m16prof-c4/baseline-1b12ce0c.cpu.pprof)（原件在本机 `artifacts/perf/nest/m16prof-c4-17370/`，artifacts 不入库），参数见同目录 `env.txt`，采样 125s，共 225.55s 样本（约 1.8 核）。采样期在负载结束后的收敛段。

## 0. 结论速览

| 项 | 内容 |
| --- | --- |
| 问题 | 4 个覆盖块盖住几乎整个对象群：每个会话应见约 940 个，会话上限 100，约 96 万个订阅里有 17 万～43 万个转为超额暂缓。负载结束后 1000 个会话合计每秒只出约 6 帧，60s 内没有收敛 |
| 成本在哪 | 四处，都随暂缓订阅数或覆盖规模增长：① Interest 对收紧不了的 pair 反复收紧主视野，每一步都完整重评 AOI（`deliverDeferrals` 15.2s）；② flush 按 subject 扫描时连暂缓订阅也扫（`deferUnselectedSnapshotsLocked` 22.1s、`prepareSubscribersLocked` 11.6s、`profilesLocked` 5.7s）；③ 夹具自己的核对占 42.8s，还和 flush 抢 Interest / subject 的锁；④ `deferSnapshot` 对同一个 subject 每个未入选订阅调一次 |
| 推荐 | **先 A（夹具去开销、重测，确定瓶颈是 CPU 还是节奏）→ B（Interest 只收紧确实能腾出位置的视野，一次算到位）→ C（暂缓订阅移出 flush 扫描）**；E（deferSnapshot 提到循环外）随 C 一起做。D（重复通知退避）与 F（配置告警）作为备选，见 §3 |
| 不做什么 | 不改“无距离来源超过会话上限时留在暂缓里”的既定语义，不改会话上限的计法，不让 Manager 替政策挑距离 |
| 验收 | 单元基准：暂缓订阅数从 1 万增到 10 万时，Flush 耗时基本不变（修前线性增长，先红）；M-16 1000 人 c4：负载结束后在 `ROOST_NEST_GAME_SETTLE=60s` 内收敛 |
| 决定项 | D1～D6，见 §7，每项都给了推荐 |

## 1. 现象与证据

### 1.1 收敛进度（夹具每 10s 一行 `Sync settling`）

| 负载结束后 | 未收敛客户端 | 缺对象总数 | 累计帧 | 订阅总数 | 超额暂缓 | pending subject |
| --- | --- | --- | --- | --- | --- | --- |
| 10s | 398 | 7553 | 18,428 | 963,030 | 168,195 | 1257 |
| 52s | 151 | 1145 | 18,740 | 961,649 | 360,255 | 1043 |
| ~60s | 56 | 192 | 18,804 | 961,649 | 422,078 | 1043 |

在收敛，只是极慢；暂缓数还在涨，因为 Interest 放宽上限后新进来的目标又被暂缓（`interest_over_limit.go` 的试探，见 §2.1）。

### 1.2 CPU profile（cum，225.55s 样本里）

| 段 | cum | 主要子项 |
| --- | --- | --- |
| `Manager.flush` | 70.17s | 见下 |
| └ `deferUnselectedSnapshotsLocked`（`flush.go:496`） | 22.12s | mapaccess2 10.32s、mapaccess1 4.86s、`deferSnapshot` 2.54s、map 迭代 1.88s |
| └ `applyPolicies` → `deliverDeferrals` → `Interest.deferred` | 15.22s | `tightenLocked` 13.02s → `AOI.SetObserverLimit` 12.94s → `refreshObserver` → `blockCandidates`（AOI 里合计 17.37s：`BlockIndex.AppendBlockIDs` 7.77s，其中排序 5.88s；`admitNearest` 排序 9.72s） |
| └ `prepareSubscribersLocked`（`flush.go:466`） | 11.57s | 每个订阅者一次 `m.session(sid)` |
| └ `profilesLocked` / `collectProfilesLocked` | 5.66s | |
| `renotifyOverLimit`（`over_limit.go:170`） | 3.15s | 收集、排序 43 万条只占 0.67s |
| **夹具核对**（不是框架成本） | 42.84s | `Interest.Seen` 24.08s；`Manager.Subscribers` 18.69s（其中排序 13.25s），来自 `sync_load_test.go:404` 拼首错描述 |

int64 排序（6.06s）几乎都来自 `AppendBlockIDs`；uint64 排序（13.25s）全部来自 `subject.sessionsLocked`，调用方只有夹具调的 `Manager.Subscribers`。

### 1.3 还不知道的

profile 说明了 CPU 花在哪里，**没有证明每秒 6 帧是 flush CPU 造成的**。也可能是节奏造成的：重复通知间隔 1s、Interest 收紧冷却 1～32s、快照预算每个窗口的冷创建配额。夹具核对还持有 `Interest.mu`（`Seen`）和 `subj.mu`（`Subscribers`），会直接卡住政策阶段和捕获阶段。所以 §4 的第一批先去掉夹具的干扰、补上 flush 次数和时长，再判断 B、C 能换来多少。

## 2. 成本来源

### 2.1 Interest：为收紧不了的 pair 反复收紧主视野

调用链：`renotifyOverLimit` 每 1s 把仍暂缓的全部订阅交给来源 → `Interest.deferred`（`interest_over_limit.go:77`）→ 每条找一个能让位的视野 `limitedViewForLocked`（112）→ `tightenLocked`（142）。

c4 里大多数被暂缓的 pair 只来自块覆盖：

1. `limitedViewForLocked` 跳过块覆盖和 self 这类没有距离的来源，退到玩家的**主视野**（132-134 行）。
2. `tightenLocked` 一步一步往下减主视野上限。每一步调一次 `SetObserverLimit`（`aoi.go:978`），也就是一次完整重评：`blockCandidates`（`aoi.go:758`）把观察者覆盖块里的全部 ID 取出来、排序、去重，再按距离排序。
3. 主视野里的成员同样被块覆盖持有，`heldBesidesLocked` 为真，离开主视野也腾不出会话里的位置，`need` 永远不减。所以循环一直减到 0，步数等于缺得最多的那名玩家的缺口。
4. 冷却期到期后，`relaxLimitsLocked`（211）把上限上调一档，新进入主视野的目标又被 Manager 暂缓，下一次通知再收紧到 0。每次结算都是一轮“收紧到 0 → 等冷却 → 上调 → 收紧”。

结果是做了很多次完整重评，却**一个位置也没腾出来**。指标 `entitysync_interest_over_limit_total{action="untightened"}` 只计“找不到视野”，不计这种“找到了视野但收紧无效”。

### 2.2 Manager flush：暂缓订阅仍在每个 subject 的扫描里

`flush` 对每个 pending subject（这里约 1000～1250 个）依次做下面几件事：

- `prepareSubscribersLocked`：遍历 `subj.subscribers`，查会话、清理过期 lifetime、分配 `flushSession`。
- `deferUnselectedSnapshotsLocked`：遍历全部订阅者，`waitsForRoom` 的跳过，但要先取出来判断。
- `profilesLocked`：遍历全部订阅者收集 profile。
- 捕获前 `revisions[sub] = sub.revision`（`flush.go:216`）：遍历全部订阅者。

每个 subject 约 960 个订阅者，其中一大半是暂缓的。所以每次 flush 遍历约 100 万次订阅，带 3～5 次 map 访问。暂缓的订阅不参与捕获（`over_limit.go` 第 3 点），扫描它们得不到任何结果。

### 2.3 `deferSnapshot` 在循环里重复调用

`deferUnselectedSnapshotsLocked` 每遇到一个未入选的全量订阅就调一次 `m.deferSnapshot(subj)`（`snapshot_budget.go:54`），对象却总是同一个 subject。每次都要加 `m.mu` 读锁和 `pendingMu`。一个 subject 只需要调一次。

### 2.4 夹具

`clientMismatch` 在每次轮询里重新算 `wanted`（`Interest.Seen`，持有 `in.mu`），数量不对时还对每个缺少的 ID 调一次 `Manager.Subscribers`（持有 `subj.mu`，复制并排序约 960 个会话 ID），只是为了拼首错描述。c4 里每个未收敛客户端缺约 840 个对象，每轮就是几十万次排序，还和 flush 抢同样的锁。

## 3. 选项

| 选项 | 做法 | 收益 | 代价 / 风险 |
| --- | --- | --- | --- |
| **A 夹具去开销**（测试代码） | 首错描述只在最终失败和 10s 诊断时拼；`Subscribers` 换成一次性取会话订阅集合（或者只取前 N 个缺口）；`wanted` 每轮每个客户端只算一次 | 去掉 42.8s 的 CPU 和锁竞争；重测能看清框架本身的收敛速度 | 只动测试，无框架风险 |
| **B Interest 只做有效收紧** | ① `tightenLocked` 开始前先数观察者可见成员里“离开就能给 need 里的玩家腾位”的个数（`!heldBesidesLocked`）。为 0 就不收紧，计 `untightened`（或新标签 `ineffective`）。② 不再一步一步重评：按距离从远到近排可见成员，一次算出要腾够位置时的上限，只调**一次** `SetObserverLimit`。③ 记住“这个视野对这名玩家收紧无效”，直到视野成员或来源变化才重新判断，避免每次通知都重新数 | 去掉 `deliverDeferrals` 的大头（15.2s），c4 下预期接近 0；也消除“收紧到 0 → 上调 → 再收紧”的空转 | 改 Interest 的收紧算法。要保持“最终可见集是最近 N 个”和滞回语义；一次算到位依赖 AOI 的距离序与 `admitNearest` 一致，需要等价测试 |
| **C 暂缓订阅移出 flush 扫描** | subject 上另存一张 `ready` 表（不暂缓的订阅）。`subscribers` 保持全集，供去重、每 subject 上限、退订、`Subscribers` 等 44 处用。flush 热循环（`prepareSubscribersLocked`、`deferUnselectedSnapshotsLocked`、`profilesLocked`、`revisions`、分发）只扫 `ready`。只有标暂缓（`markOverLimit` 一侧）、放回（`releaseOverLimit`）、增删订阅这几处维护 `ready`。暂缓订阅的过期 lifetime 由会话关闭和 `renotifyOverLimit`（它已经在比 `sub.lifetime == item.life`）清理 | flush 扫描量从“全部订阅者”降到“非暂缓订阅者”，c4 下约降一半以上；和暂缓数解耦 | 两张表必须一致，否则会漏发或多发。需要不变式测试：任何时刻 `ready` = `subscribers` 里 `!waitsForRoom()` 的集合。改动集中在 entitysync 包内，不改对外 API |
| D 重复通知退避 | 会话的暂缓集合没变（没有放回、没有新增）时，`renotifyOverLimit` 的间隔按会话从 1s 加倍到 32s；有空位或集合变化就回到 1s | 再省下 B 之后剩下的通知成本（43 万条/s 的收集和回调） | Interest 的“上调试探”靠重复通知发现边界，退避会让上调后的超额多停留一会儿（都只是 Manager 内部暂缓，不产生线上流量）。B 做完后收益可能已经很小，所以列为备选 |
| E `deferSnapshot` 提到循环外 | 循环里只记“有未入选的全量”，循环后对 subject 调一次 | 2.5s，锁次数从每订阅一次降到每 subject 一次 | 很小，随 C 一起做 |
| F 配置告警 | 运行时统计“只来自无距离来源、收紧无效”的暂缓 pair 数，做成指标；每周期限频一条 WARN，说明政策（块覆盖、关系视野）本身超过了会话上限 | 让运维看见“这个配置不会收敛到上限内”，而不是只看到暂缓数在涨 | 只是可观测，不降成本。现有 Warn 已经要求“政策上限要在会话上限之内”，F 补的是运行时证据 |

没有选的方向：

- **让 Manager 挤掉块覆盖的订阅**：Manager 不知道距离，这违反 D17“已持有的对象 Manager 不替政策挤掉”。
- **块覆盖也按距离收紧**：改变块覆盖语义。块覆盖本来就是“这些块里的都要看”，不该靠距离裁掉。
- **提高会话上限**：只是把问题推后，不解决暂缓订阅数无上界的问题。

## 4. 推荐与分批

| 批 | 内容 | 产出 | 估计 |
| --- | --- | --- | --- |
| 1 | A：夹具去开销；`Sync settling` 行补上 Manager 的 flush 次数与平均耗时（`ManagerStats` 现有字段不够就在测试里统计 `Flush` 调用）；重跑 1000 人 c4，同时开 CPU profile | 判断瓶颈是 flush CPU 还是节奏（§1.3）。如果去掉夹具后已在 60s 内收敛，B、C 降为容量优化，按你的决定再排 | 0.5 天，跑一轮约 10 分钟 |
| 2 | B：Interest 有效收紧（①②③），先写红：块覆盖只靠主视野的场景下，`deliverDeferrals` 触发的 `SetObserverLimit` 次数随缺口线性增长、并且一个位置也没腾出 | 等价测试（最终可见集仍是最近 N 个，滞回不变）+ 基准 | 1 天 |
| 3 | C + E：`ready` 表、`deferSnapshot` 提到循环外；先写红基准 `BenchmarkFlushWithOverLimitWaiting`，暂缓 1 万 / 10 万，修前耗时线性增长 | 不变式测试 + 基准 + entitysync 包全量 | 1 天 |
| 4 | 重跑 M-16 1000 人 c4（以及 c0 / c1 回归，确认没有暂缓时帧逐字节不变）；更新 KNOWN-LIMITS 的 M-16 行与 over_limit.go 文件头 | 测量记录 | 0.5 天 |

D、F 等第 4 批的数据出来再决定。

## 5. 验证

- **先红后绿**：批 2、批 3 的红测试先单独提交或保留修前输出，修后同一测试转绿。
- **语义不变**：
  - `interest_over_limit_test.go` 现有用例全部通过（收紧、上调、撤掉、滞回）。
  - 新增等价测试：B 前后，同一输入序列的最终可见集、Enter/Leave 事件一致。
  - entitysync 的 over-limit 测试全部通过；新增 `ready` 一致性检查，在每次 flush 后断言（测试构建开启）。
- **无暂缓时零影响**：c0 场景的帧字节和 Flush 耗时与修前一致（over_limit.go 第 1 点“常见路径帧逐字节不变”）。
- **验收**：M-16 1000 人 c4 在 `ROOST_NEST_GAME_SETTLE=60s` 内收敛；记录负载结束后的收敛时间、flush 次数和平均耗时、CPU profile 各段。

## 6. 风险与不变式

| 编号 | 不变式 | 涉及选项 | 怎么守 |
| --- | --- | --- | --- |
| I1 | 对象要么完整持有、要么完全不在客户端 | C | `ready` 只决定扫描范围，不改准入（`admitWithinLimits`） |
| I2 | 已持有的对象 Manager 不替政策挤掉（钉住例外） | B、C | 不动 |
| I3 | 有距离概念的政策，最终可见集是按距离选出的最近 N 个 | B | 等价测试 |
| I4 | 暂缓订阅在有空位时恢复，不会永远留在暂缓 | C、D | `releaseOverLimit` 同时维护 `ready`；D 的退避在空位出现时立即复位 |
| I5 | 过期 lifetime 的订阅最终被清理 | C | 会话关闭照旧清理；`renotifyOverLimit` 清理暂缓订阅；flush 只清 `ready` 里的 |
| I6 | 无暂缓时帧逐字节不变 | 全部 | c0 回归 |

最大风险在 C：两张表不一致会导致漏发（订阅在 `subscribers` 里但不在 `ready` 里）或给暂缓订阅发内容。所以 C 必须带一致性断言，并且在 entitysync 全量和 game-demo 生成测试都通过后再推。

## 7. 决定项

| 编号 | 问题 | 推荐 |
| --- | --- | --- |
| D1 | 是否先做批 1（只动夹具和测量），看结果再决定 B、C | **是**。成本最低，能区分 CPU 瓶颈和节奏瓶颈 |
| D2 | 批 1 后即使已在 60s 内收敛，B 和 C 还做不做 | **B 做**（去掉无效的完整重评，属于纯浪费）；C 看批 1 的 flush 耗时，超过 Interval（50ms）就做 |
| D3 | B 的“收紧无效”用现有 `untightened` 标签还是新标签 `ineffective` | **新标签**，区分“没有可收紧的视野”和“收紧也腾不出位置” |
| D4 | C 用 subject 上第二张表，还是把暂缓订阅从 `subscribers` 挪到单独的表 | **第二张表**（`subscribers` 保持全集）。挪表要改 44 处读写，风险更大 |
| D5 | D（重复通知退避）做不做 | **先不做**，等批 4 数据 |
| D6 | F（配置告警）做不做 | **做指标、不做 WARN**；WARN 已有“政策上限要在会话上限之内” |

## 8. 以后怎么复测

不做优化也能随时重测，看问题还在不在、有没有被别的改动顺带改善。需要本机私有 NATS（`~/.roost-it`），一轮约 10 分钟，按 perf 规则串行跑：

```sh
source ~/.roost-it/roost-dataengine-it/env.sh
ROOST_PERF_LABEL=m16prof-c4 ROOST_PERF_COUNT=1 \
ROOST_NEST_GAME_GATE=1 ROOST_NEST_GAME_SYNC=1 ROOST_NEST_GAME_DIRTY=1 ROOST_NEST_GAME_HZ=1 \
ROOST_NEST_GAME_LAYOUT=concentrated ROOST_NEST_GAME_OBSERVED_BLOCKS=4 \
ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 \
ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_IO_WORKERS=8 ROOST_NEST_GAME_SHORT_QUEUE=65536 \
ROOST_NEST_GAME_DURATION=30s ROOST_NEST_GAME_SETTLE=60s ROOST_NEST_GAME_PROFILE=1 \
bash scripts/perf/nest-game.sh
```

- **看什么**：日志里每 10s 一行 `Sync settling`（未收敛客户端、缺对象数、累计帧、超额暂缓数），对照 §1.1。60s 内收敛就是 PASS；没收敛时，看客户端读静默中止前的最后一行。
- **对比 CPU**：`go tool pprof -top -cum <新 cpu.pprof>`，对照 §1.2 各段。也可以直接和基线比：`go tool pprof -diff_base docs/performance/benchmarks/m-series-rest-20261122/m16prof-c4/baseline-1b12ce0c.cpu.pprof <新 cpu.pprof>`。
- **注意**：§1.2 里夹具核对占了 42.8s，还会和 flush 抢锁。没做批 1 之前，复测得到的是“框架 + 夹具”的合成结果，不能直接当作框架的收敛速度。
- **回归档**（确认改动没伤到常见路径）：同参数改 `ROOST_NEST_GAME_OBSERVED_BLOCKS=0` 跑 1000 人 c0，或者 `ROOST_NEST_GAME_PLAYERS=400` 跑 c4。两档在 1b12ce0c 上终值核对都通过（只差延迟门槛）。
