# Lockstep 深缓冲链路子例：本机（macOS）实验（2026-11-07）

用例 `TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive/deep-buffered_link/written_lane_follows_the_link`。
Linux 上原失败：约 67 秒后 `closed=true`、0 个 Tick 错误，五轮一败（[审查记录](../../FOLLOWUP-REVIEW-20261102.md)）。
本机没有 Linux 环境，以下实验都只临时改测试（环境变量注入，不提交），生产代码不变；测试里 `write_timeout` 与
`CatchupSendTimeout` 都是 10 秒。

| 实验 | 结果 | 说明 |
| --- | --- | --- |
| 原样（中转接收缓冲 16 KiB）× 3 | 全过，约 5 秒 | macOS 回环 MTU 16 KiB，复现不了 Linux 的条件 |
| 中转接收缓冲 4 KiB（小于 macOS 回环 MSS）× 3 | 全过 | macOS 协议栈不进入 Linux 那种持续定时器节流 |
| 中转从服务器读到 1 MiB 后整段停读 4s / 6s / 8s × 2 | 全过 | 停顿短于 10 秒，快照随链路恢复 |
| 同上停读 12s × 2 | 全败（[失败文本](stall-12s-failure.txt)） | 追帧单块等写出超过 10 秒，`closed=false`、1 个 Tick 错误：与 Linux 的失败形态（`closed=true`、0 个 Tick 错误）不同，说明 Linux 上是 Gate 的写超时先触发 |
| 中转那一跳限速 128 KiB/s（夹具声明 4 MiB/s 的 1/32）× 1 | 通过，114 秒 | 均匀变慢不会断连、不会溢出 |
| 同上 512 KiB/s × 1 | 通过，28 秒 | |
| 修正后（中转接收缓冲 = `slowReadBuffer` 256 KiB）整个顶层用例 × 5 | 全过；只准入对照五轮都溢出 | 负对照仍成立，用例仍落在“随链路放慢”的承诺上 |

结论：框架对“慢”的链路成立，对 10 秒以上完全写不出的链路按设计放弃。Linux 上的失败要求中转那一跳出现 10 秒以上的
停顿；夹具让这一跳的接收窗口（16 KiB，内核加倍约 32 KiB）小于 Linux 回环 MSS（约 64 KiB），发送端糊涂窗口规避只能
靠持续定时器探测、退避可到 10 秒以上，这是夹具条件，真实网络 MSS 约 1460 字节不会如此。修正与同文件直连路径一致
（`slowReadBuffer` 的注释早已写明同一原因）。

未验证：修正后没有在 Linux 上复跑；内核持续定时器这一环是按审查的 Linux 对照（只改该值，五轮全过）与 Linux TCP 行为推断，
本机复现不了。用例失败时现在会打出 Gate 关连接计数的增量（`player_tcp_write_error_total` 等）与链路各跳进度、最后一次
推进距今多久，下次 Linux 失败可直接判断。
