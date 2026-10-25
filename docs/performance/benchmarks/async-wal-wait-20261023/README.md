# async 持锁等待定位

复用 10-20 生成工程 fixture，当前工作树生成并编译真实 Nest → DataEngine → WAL 链路。50 个玩家、50 个并发调用者、async 金币 +1、15 秒、group commit interval=2ms，默认 GOMAXPROCS（本机 10），不经 TCP / self_sync。Mongo / NATS 是隔离测试环境；不是完整客户端吞吐对比。

无 profiler 的一次测量：23783 次调用，handler 错误 0；持锁均值 6.272ms，durable_commit 均值 6.234ms，handler 均值 0.0055ms；1994 次物理段 fsync，平均 5.494ms。完整前后计数在 metrics.json，均值按增量计算；MaxNanos 是累计最大值，可能包含预热。

另一次 15 秒诊断开启全部 block / mutex 采样（不将其吞吐当正常容量）。block-wal.txt 中 WAL.Append 的累计阻塞 144.17 秒，集中在等待 writer 回执的 channel；writer 本身在 writeBatch 的锁等待只有约 6ms（累计），因此不能把请求等待简单说成每条都争抢 stateMu。唯一 writer 同时处理追加与 ticker fsync，后台 fsync 时不能处理新追加；此外批次收集也有等待。mutex 统计还显示后台写锁影响 Replay 等读侧，但不是本次 handler 的主要直接阻塞位置。

结论：async 表示本次提交不强制等待“本条记录 fsync”，并不保证不等待 writer，更不是全进程不 fsync。此次链路已复现与旧约 6.8ms 同量级的提交等待，业务执行占比很小；旧样本缺剖析，不能把本次比例精确套回旧样本。物理 fsync 指标漏记已经修复；本轮不拆分 writer/fsync、不提前确认追加，不改变持久化和提交顺序契约。

复跑：使用 ../r8-generated-tcp-strict-20261020/prepare_test.go.txt 的生成入口，把本目录 tap_perf_test.go.txt 放入生成工程 internal/bootstrap；编译后设置 R8_MODE=async、R8_WORKERS=50、R8_PLAYERS=50、R8_SECONDS=15、R8_GCI=2ms、ROOST_DIAG_METRICS_OUTPUT=<输出路径>。先正常测量，再单独加 Go test 的 blockprofile / mutexprofile 参数。加载私有测试环境但不提交凭据；测试创建唯一数据库和 stream 并自行清理。原始 pprof 留在主工作树 artifacts/perf/async-wal-wait-20261023。
