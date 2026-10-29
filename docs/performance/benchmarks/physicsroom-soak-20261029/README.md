# 物理房间长稳、RSS 与原生内存（2026-10-29，macOS ARM64）

结论见 [PHYSICS-ROOM-SERVICE §长稳、RSS 与原生内存](../../../framework/PHYSICS-ROOM-SERVICE.md#长稳rss-与原生内存2026-10-29macos-arm64)。

环境：Apple M5（10 核），macOS 26.5.2（25F84），Go 1.27.3，GOMAXPROCS=10，PhysX 4.1.2 ABI 2 arm64 动态库。负载：`TestPhysicsRoomSoak`，机关场景 8 房间 × 20 逻辑客户端（每房 83 个非静态刚体），直接调用 Service，不含 Gate/NATS/TCP。Linux 未测。

| 文件 | 内容 |
| --- | --- |
| `soak20-go.tsv` / `soak20-proc.tsv` / `soak20-summary.txt` | `067d4606`（物理房间依赖的产品代码同 `ddc3d3d0`）20 分钟：测试内每 15 秒的 Go 运行时指标、RSS 与频率；进程外每 30 秒 footprint、每 2 分钟 vmmap；汇总（预热 300 秒后的斜率） |
| `soak4-final-go.tsv` / `soak4-final-proc.tsv` / `soak4-final-summary.txt` | `c3946490`（两项削减之后）同口径 4 分钟复跑 |
| `m-frame.txt` | `067d4606` 与 `e04c2efd`（`ReplicationFrame` 改切片）交替 3 轮，`MULTI_ROOM_RESULT` 原始输出 |
| `m-spatial.txt` | `e04c2efd` 与 `c3946490`（`SpatialFrame` 改切片）交替 3 轮（`f9e01cfc` 只改未排序输入的复制路径与注释，对原生有序输出无影响） |
| `m-final.txt` | `067d4606` 与 `c3946490` 交替 3 轮，即合计 |
| `0b8e20ff-*.alloc.pprof` / `-alloc-top.txt`、`0d6be921-*` | 机关 8 房间 alloc profile（memprofilerate=65536）与 top |
| `run.sh` / `sample.py` / `summ.py` | 长稳运行、进程外采样（footprint / vmmap）、汇总脚本 |

交替测量用 [physicsroom-alloc-20261029](../physicsroom-alloc-20261029/README.md) 的 `measure.sh` 与 `summ.py`。vmmap 会挂起目标进程约 0.4～0.5 秒，`soak*-proc.tsv` 里 `vmmap_ms` 非空的时刻对应的测试窗口物理均频偏低，判断频率时排除。

提交号：测量跑在 rebase 进 main 之前的等价提交上，上表已换成 main 上的对应提交（物理房间依赖的代码逐字相同，见 [PHYSICS-ROOM-SERVICE §Go 分配 profile 与削减](../../../framework/PHYSICS-ROOM-SERVICE.md#go-分配-profile-与削减2026-10-29macos-arm64) 的说明）；pprof 文件名保留 rebase 前的前缀 0b8e20ff / 0d6be921。
