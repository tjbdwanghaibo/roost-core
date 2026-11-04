# 环境

原型量级数据，不是容量保证。设计与解读见 [无缝大世界方案 §9](../../../maintenance/FEATURE-2026-11-03-seamless-world.md#9-v21d1-降档与-demo-接入2026-11-03)。

- 日期：2026-11-03；分支 feat/seamless-world-prototype，代码为 91a76ccd（D1 实现 + 档位判断与 witness 共用索引）
- 机器：Apple M5，10 核，macOS 26.5.2；测量期间机器有常驻后台负载（load average 约 4.5，swap 已用约 3.5GB），同机交替跑 v2 / v2+D1 两轮
- Go：go1.27.3；nats-server v2.14.5；redis-server 8.10.1
- 命令（非 -race，先 `go test -c` 编出测试二进制，再按档位环境变量各跑一次）：
  `ROOST_CELLWORLD_PERF=1 ROOST_CELLWORLD_PERF_IDLE_MS=<0|1000> ROOST_CELLWORLD_PERF_PLAYERS=<spread|cell> ./cellworld.test -test.run TestCellWorldMeasure -test.v -test.timeout 0`
  顺序：v2 spread、D1 spread、v2 spread、D1 spread、D1 cell、D1 cell、v2 cell
- 负载：与 v1 / v2 相同（2×2 网格、格边 50000、G 3000、H 200、R 2500、每格一个 OS 进程（带单实例锁的 App）、单机 nats-server、每格 N 个 3 m/s 随机游走 NPC、父进程 Base 上 40 个 5 m/s 随机游走玩家、20s 窗口）；预热从 3s 改为 6s（盖过降档滞后 IdleAfter 2s）。D1：IdlePeriod 1s、WakeRadius = R = 2500、IdleAfter 2s。`cell`：40 个玩家全部在格 (0,0) 内出生并游走
- 原始输出：measure-v21-raw.txt；修前红与变异：d1-red-before.txt、d1-mutation-checks.txt
- 第一次尝试（档位判断用每个 NPC 查 3×3 观察者桶）只跑完 v2 / D1 各一轮就停下改成共用索引，那份输出保留在 measure-v21-first-attempt.txt（5k 时区域心跳 p50 约 2.1～2.4ms）
