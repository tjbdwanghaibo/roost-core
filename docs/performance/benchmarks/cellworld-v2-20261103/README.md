# 环境

- 日期：2026-11-03；分支 feat/seamless-world-prototype，基于 e3ae67da（测量时工作树含未提交的 process_test.go / measure_test.go，内容即随后的提交）
- 机器：Apple M5，10 核，macOS 26.5.2
- Go：go1.27.3；nats-server v2.14.5；redis-server 8.10.1
- 命令：`ROOST_CELLWORLD_PERF=1 GOWORK=off go test -count=1 -run TestCellWorldMeasure -v -timeout 0 ./gameplay/cellworld/`，两轮连续执行（非 -race）
- 负载：与 v1（benchmarks/cellworld-v1-20261103）相同：2×2 网格（格边 50000、G 3000、H 200、R 2500）、每格一个 OS 进程、单机 nats-server，每格 N 个 3 m/s 随机游走 NPC，父进程 Base 上 40 个玩家随机游走
- v2 差异：每个 Cell 子进程是带单实例锁的 App（私有 redis-server）；NPC / 玩家都是 Nest 实体、各一个 50ms 心跳；区域表在 Scene 实体；Base 进程多了 Sync mirror（witness → entitysync Manager → 客户端替身解帧）
- 原始输出：measure-v2-raw.txt；其他证据：mirror-red-before.txt（Sync mirror 修前红）、mutation-checks.txt（三处变异）、probe-heartbeat-cost.txt（空心跳成本探针）
