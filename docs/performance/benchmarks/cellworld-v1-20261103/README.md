# cellworld v1 原型测量（2026-11-03）

原型量级数据，不是容量保证。设计与解读见 [无缝大世界方案 §6](../../../maintenance/FEATURE-2026-11-03-seamless-world.md#6-v1-实测结果与已知缺口2026-11-03)。

- 分支 `feat/seamless-world-prototype`，代码基线 3b2d862f（测量代码随后提交）；Go 1.27.3 darwin/arm64，Apple M5 10 核，macOS；机器有常驻后台负载（两轮开始时 load average 5.1 / 9.0），swap 已用约 3.4G。
- 拓扑：单机，1 个 nats-server 2.14.5（core NATS，无 JetStream）、4 个 Cell 子进程（2×2 网格，每格 500m×500m，G=30m，H=2m，视野 25m，20Hz）、父进程 1 个 Base 带 40 个玩家（5 m/s 随机游走，每 2s 换向）。
- 每格 N 个 NPC 随机位置、3 m/s 随机方向，碰世界边缘反弹，跨格即交接。预热 3s 后测 20s 窗口；每档两轮（顺序执行，两轮之间没有改代码）。
- 命令：`ROOST_CELLWORLD_PERF=1 GOWORK=off go test -count=1 -timeout 10m -run TestCellWorldMeasure -v ./gameplay/cellworld/`
- 指标口径：CPU = 子进程 getrusage(user+sys) / 墙钟；tick = 每拍处理耗时；ghost latency = 发送方打时间戳到邻格事件循环应用完（同机系统时钟）；handoff = 旧格冻结到得到确认；view latency = Cell 发出视野到 Base 收到。消息量含 Ghost、交接、视野。

## 第一轮

```
level 1000 NPC/cell, 40 players, window 20s; base views accepted 15997 stale 0 owner changes 5 re-enters 0; view latency p50 0.22ms p99 1.24ms max 9.37ms (n=15997)
cell (0,0): reals 1015 ghosts 135 | CPU 4.4% of one core | tick p50 0.93ms p99 3.68ms max 14.60ms | out 234 msg/s 319 KB/s, in 72 msg/s 256 KB/s | ghost latency p50 1.53ms p99 5.88ms (n=1200) | handoffs 79 (p50 1.95ms p99 14.79ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
cell (1,0): reals 1017 ghosts 111 | CPU 5.0% of one core | tick p50 1.12ms p99 4.60ms max 13.74ms | out 372 msg/s 400 KB/s, in 75 msg/s 215 KB/s | ghost latency p50 1.62ms p99 5.67ms (n=1200) | handoffs 71 (p50 1.97ms p99 4.82ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
cell (0,1): reals 989 ghosts 125 | CPU 4.3% of one core | tick p50 0.90ms p99 2.99ms max 9.20ms | out 227 msg/s 321 KB/s, in 71 msg/s 230 KB/s | ghost latency p50 1.65ms p99 5.49ms (n=1200) | handoffs 74 (p50 1.62ms p99 8.03ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
cell (1,1): reals 1019 ghosts 105 | CPU 4.6% of one core | tick p50 1.18ms p99 2.62ms max 4.76ms | out 235 msg/s 308 KB/s, in 70 msg/s 209 KB/s | ghost latency p50 1.60ms p99 5.93ms (n=1200) | handoffs 60 (p50 1.90ms p99 2.82ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
level 5000 NPC/cell, 40 players, window 20s; base views accepted 15997 stale 0 owner changes 5 re-enters 0; view latency p50 0.15ms p99 0.77ms max 4.68ms (n=15997)
cell (0,0): reals 5043 ghosts 637 | CPU 13.4% of one core | tick p50 3.77ms p99 6.16ms max 6.82ms | out 302 msg/s 1417 KB/s, in 140 msg/s 1171 KB/s | ghost latency p50 2.69ms p99 6.73ms (n=2000) | handoffs 341 (p50 4.57ms p99 6.31ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
cell (1,0): reals 5013 ghosts 595 | CPU 15.4% of one core | tick p50 4.68ms p99 8.09ms max 17.80ms | out 438 msg/s 1684 KB/s, in 141 msg/s 1084 KB/s | ghost latency p50 2.59ms p99 6.04ms (n=2002) | handoffs 330 (p50 5.74ms p99 11.14ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
cell (0,1): reals 5000 ghosts 623 | CPU 13.9% of one core | tick p50 4.06ms p99 6.64ms max 8.57ms | out 295 msg/s 1467 KB/s, in 139 msg/s 1151 KB/s | ghost latency p50 2.61ms p99 6.17ms (n=2002) | handoffs 356 (p50 5.04ms p99 7.44ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
cell (1,1): reals 4987 ghosts 593 | CPU 13.4% of one core | tick p50 3.73ms p99 5.96ms max 6.27ms | out 301 msg/s 1385 KB/s, in 137 msg/s 1117 KB/s | ghost latency p50 2.65ms p99 6.66ms (n=2001) | handoffs 341 (p50 4.52ms p99 6.90ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
--- PASS: TestCellWorldMeasure (46.17s)
ok  	github.com/tjbdwanghaibo/roost-core/gameplay/cellworld	46.464s
```

## 第二轮

```
level 1000 NPC/cell, 40 players, window 20s; base views accepted 15997 stale 0 owner changes 5 re-enters 0; view latency p50 0.28ms p99 1.10ms max 16.14ms (n=15997)
cell (0,0): reals 1015 ghosts 135 | CPU 5.8% of one core | tick p50 1.45ms p99 2.84ms max 13.53ms | out 234 msg/s 319 KB/s, in 72 msg/s 256 KB/s | ghost latency p50 1.76ms p99 3.91ms (n=1200) | handoffs 79 (p50 2.31ms p99 11.07ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
cell (1,0): reals 1017 ghosts 111 | CPU 6.3% of one core | tick p50 1.62ms p99 3.17ms max 3.58ms | out 372 msg/s 400 KB/s, in 75 msg/s 215 KB/s | ghost latency p50 1.82ms p99 3.78ms (n=1200) | handoffs 71 (p50 2.32ms p99 12.44ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
cell (0,1): reals 989 ghosts 125 | CPU 5.4% of one core | tick p50 1.17ms p99 2.53ms max 2.83ms | out 227 msg/s 321 KB/s, in 71 msg/s 230 KB/s | ghost latency p50 1.92ms p99 4.05ms (n=1200) | handoffs 74 (p50 1.88ms p99 2.84ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
cell (1,1): reals 1019 ghosts 105 | CPU 5.5% of one core | tick p50 1.41ms p99 3.32ms max 8.54ms | out 235 msg/s 308 KB/s, in 71 msg/s 209 KB/s | ghost latency p50 1.85ms p99 4.03ms (n=1200) | handoffs 60 (p50 2.17ms p99 4.35ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
level 5000 NPC/cell, 40 players, window 20s; base views accepted 15997 stale 0 owner changes 5 re-enters 0; view latency p50 0.15ms p99 0.74ms max 4.46ms (n=15997)
cell (0,0): reals 5043 ghosts 637 | CPU 15.3% of one core | tick p50 4.27ms p99 6.45ms max 6.93ms | out 302 msg/s 1417 KB/s, in 140 msg/s 1171 KB/s | ghost latency p50 3.24ms p99 6.62ms (n=2000) | handoffs 341 (p50 5.03ms p99 7.33ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
cell (1,0): reals 5013 ghosts 595 | CPU 17.1% of one core | tick p50 5.12ms p99 6.92ms max 8.00ms | out 438 msg/s 1684 KB/s, in 141 msg/s 1082 KB/s | ghost latency p50 3.27ms p99 6.49ms (n=2000) | handoffs 330 (p50 5.98ms p99 8.19ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
cell (0,1): reals 5000 ghosts 623 | CPU 15.4% of one core | tick p50 4.63ms p99 9.79ms max 11.80ms | out 295 msg/s 1467 KB/s, in 139 msg/s 1149 KB/s | ghost latency p50 3.27ms p99 6.15ms (n=2000) | handoffs 356 (p50 5.58ms p99 10.82ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
cell (1,1): reals 4987 ghosts 593 | CPU 14.7% of one core | tick p50 4.14ms p99 5.89ms max 6.08ms | out 301 msg/s 1385 KB/s, in 137 msg/s 1117 KB/s | ghost latency p50 3.50ms p99 6.64ms (n=2000) | handoffs 341 (p50 4.83ms p99 6.81ms), rejected 0 aborted 0 retransmits 0 | inbox drops 0 conflicts 0
--- PASS: TestCellWorldMeasure (46.17s)
ok  	github.com/tjbdwanghaibo/roost-core/gameplay/cellworld	46.460s
```
