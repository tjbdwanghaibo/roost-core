# 物理房间 Go 分配 profile 与削减（2026-10-29，macOS ARM64）

结论与改动说明见 [PHYSICS-ROOM-SERVICE §Go 分配 profile 与削减](../../../framework/PHYSICS-ROOM-SERVICE.md#go-分配-profile-与削减2026-10-29macos-arm64)。

环境：Apple M5（10 核），macOS 26，Go 1.27.3，GOMAXPROCS=10，PhysX 4.1.2 ABI 2 arm64 动态库。负载：`TestMultiRoomReplicationLoad` 的 crowd/8 与 mechanisms/8（预热 1 秒、采集 5 秒）。

| 文件 | 内容 |
| --- | --- |
| `baseline-6levels.txt` | 基线（c8825078）六档各一次的原始 `MULTI_ROOM_RESULT` |
| `m-c1.txt` … `m-c8.txt` | 逐项改动的同机交替 3 轮原始输出（标签对应前后两个测试二进制；c6 为 4 个二进制交替） |
| `m-final.txt` | 基线与 92f7e6f9 交替 3 轮 |
| `m-c8.txt` | 基线、92f7e6f9（c7）、59211a6f（c8）交替 3 轮，即最终表 |
| `baseline-mechanisms8.alloc.pprof` / `baseline-alloc-top.txt` | 基线机关 8 房间 alloc profile（memprofilerate=65536）及 top |
| `92f7e6f9-mechanisms8.alloc.pprof` / `92f7e6f9-alloc-top.txt` | 92f7e6f9 同口径 profile 及 top |
| `baseline-8rooms.cpu.pprof` / `baseline-cpu-top.txt` | 基线两档 8 房间单独一次 CPU profile |
| `measure.sh` / `summ.py` | 交替运行与中位数汇总脚本 |

标签 `before` 是 c8825078 加上只增加报告字段的测试提交 e16f8417 编出的二进制。分配为进程级，包含驱动本身。
