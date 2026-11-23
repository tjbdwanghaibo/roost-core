# M 系列复测：最终数据（2026-11-23）

本目录只保留最终测试数据，中间诊断（分段采样、候选补丁、事实级时间线、逐档原始包等）已按维护者 2026-11-23 的决定删除，不再入库。

| 文件 | 内容 |
|---|---|
| [final-ab-20261123.json](final-ab-20261123.json) | M-16 最终对照：main 与修复后代码各跑一组，用同一份夹具 |
| [measurements.json](measurements.json) | M-7、M-12、M-13、M-14、M-16 修复分支复测的汇总数值 |
| [m14-baseline-cell-1-0.pprof](m14-baseline-cell-1-0.pprof) | M-14 game 尺度 Cell 子进程的 CPU profile |

## M-16 最终对照

**测法：**
- 时间：2026-11-23 11:19 起。本机没有其他重负载（Unity 已关），开跑前 1 分钟负载 3.9，跑测时 5～7，主要来自压测本身。
- 代码：main `0ced5d3e` 与修复后代码（即本次合入 main 的代码）。两边都用修复后的夹具，交替串行执行。
- 参数：1000 玩家、10000 实体、集中布局、独立 Gate、每人每秒 10 条业务、1Hz 心跳、1% 改值、short/io 8/8、queue 65536。预热 30s 后计时 30s，收尾上限 60s。
- c0 / c4 指每个玩家在距离视野之外，额外覆盖右侧 0 / 4 个块（`ROOST_NEST_GAME_OBSERVED_BLOCKS`）。c4 下每人应见约 880～940 个对象，超过会话对象上限 100，会触发超额暂缓。

| 档 | main `0ced5d3e` | 修复后 |
|---|---|---|
| c4 #1 | 60s 内终值未收齐：最后 267 人落后，单次 flush 平均 133ms | 收尾 7.2s，全员终验通过；同步 p99 ≤480ms，全部超 50ms |
| c4 #2 | 60s 内终值未收齐：最后 205 人落后 | 收尾 6.8s，全员终验通过；同步 p99 ≤1000ms，全部超 50ms |
| c0 | 同步 p99 ≤3.92s，69% 超 50ms；业务 p99 顶到 100ms 桶，Gate 往返 p99 99ms | 同步 p99 ≤30ms，61 条超 50ms，**通过**；业务 p99 17ms，Gate 往返 p99 13ms |

同步 p99 取 10ms 宽桶的上界。所有出了报告的档，数量、成员、DAO 终值都通过。修复后 c4 的业务、心跳、Gate 往返 p99 都在 23ms 以内。

**结论：**
- **c4 收敛修好了。** 原因是满会话的冷创建请求全部因容量被暂缓时，预算一直停在同一个会话上，其他会话轮不到（队首阻塞）。修法是轮转到下一个会话（`snapshot_budget.go` 的 `rotateCapacityOnlyPlans`）。
- **c0 大幅改善。** 集中布局下 AOI 重评不再对全部候选排序、去重，只取最近的 K+1 个（`policy/aoi.go` 的 `nearestPrefix`）。这一项与上一项是一起测的，没有单独拆开。
- **c4 稳态同步时延未达标。** 预热后 p99 仍在 0.5～1s，超过夹具对这一配置的 100ms 门槛（`sync_delivery_test.go` 的 `syncP99LimitMS`；其他配置仍为 50ms）。业务与网络都不慢，慢在同步这条链：发布屏障等待，以及政策阶段每轮能处理的事实数。异步应用、单遍扫描等都还只是候选，没有进生产代码，需要另出方案。
- **开局瞬态单独计。** 不预热时 c4 的秒级积压出现在发压前 15s，随后回落。夹具已新增 `ROOST_NEST_GAME_WARMUP`：预热期照常发压但不计时延，数量与终值仍全程核对。

两边各只跑了一次（c4 两次），同一台机器，不是容量结论。原始 JSON 和日志在本机 `artifacts/perf/nest/ab-20261123-q1119/`，不入库。

## 复跑

```sh
source ~/.roost-it/roost-dataengine-it/env.sh
ROOST_PERF_LABEL=m16-c4-w30 ROOST_PERF_COUNT=1 ROOST_PERF_CPU=0 \
ROOST_NEST_GAME_GATE=1 ROOST_NEST_GAME_SYNC=1 ROOST_NEST_GAME_DIRTY=1 ROOST_NEST_GAME_HZ=1 \
ROOST_NEST_GAME_LAYOUT=concentrated ROOST_NEST_GAME_OBSERVED_BLOCKS=4 \
ROOST_NEST_GAME_PLAYERS=1000 ROOST_NEST_GAME_ENTITIES=10000 ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10 \
ROOST_NEST_GAME_SHORT_WORKERS=8 ROOST_NEST_GAME_IO_WORKERS=8 ROOST_NEST_GAME_SHORT_QUEUE=65536 \
ROOST_NEST_GAME_DURATION=30s ROOST_NEST_GAME_SETTLE=60s ROOST_NEST_GAME_WARMUP=30s \
bash scripts/perf/nest-game.sh
```

c0 把 `ROOST_NEST_GAME_OBSERVED_BLOCKS` 改为 0。跑之前先确认本机没有其他重负载。
