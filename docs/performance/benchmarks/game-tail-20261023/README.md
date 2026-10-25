# 游戏重档长尾诊断

2 分钟、GOMAXPROCS=0（实际10）、short/io 各8、short queue=65536、1000玩家、10000实体、每人每秒10消息、每实体10Hz正式 heartbeat、5%修改、Gate + NATS + on_change Sync、视野28～50。使用已有 ROOST_NEST_GAME_TAIL=trace，诊断开销不能当正常容量基线；用户告知电池模式，实时供电读数见 power.txt，功耗可能影响吞吐和长尾。

120万条普通消息与11,999,010拍 heartbeat 全部执行、零拒绝、实体与 Sync 数据核验通过。普通业务服务端 P99 6.11ms；Sync 提交到客户端 P99 12.49ms、最大86.91ms、1442/5,324,547样本超过50ms。计划时刻到客户端最大102.93ms（不同起点，不能混用）。满足本次 P99≤50ms，未声明所有更新最大值≤50ms。

捕获的第一个102.93ms样本中，提交后交付约80.02ms，接收侧 goroutine 主要是 network wait，runnable约2.46ms；观察窗口 GC STW 总计约0.30ms，没有同量级GC停顿。各秒的调度延迟高值与长尾相关，但仅凭接收端 network wait 不能再区分发送方、NATS、内核、调度与功耗各自占比；不认定历史整体停顿已经修复，也不直接归因于桌面活动。episodes 的 span 是事件簇跨度，不是整个进程停顿时长。

原始 trace（约221MB，包含测试二进制等）留在主工作树 artifacts/perf/nest/fix-20261023-game-tail，本文保留摘要。复跑 scripts/perf/nest-game.sh：ROOST_PERF_CPU=0、ROOST_PERF_COUNT=1、ROOST_NEST_GAME_DURATION=2m、ROOST_NEST_GAME_SHORT_WORKERS=8、ROOST_NEST_GAME_IO_WORKERS=8、ROOST_NEST_GAME_SHORT_QUEUE=65536、ROOST_NEST_GAME_PLAYERS=1000、ROOST_NEST_GAME_ENTITIES=10000、ROOST_NEST_GAME_MESSAGES_PER_PLAYER=10、ROOST_NEST_GAME_HZ=10、ROOST_NEST_GAME_DIRTY=5、ROOST_NEST_GAME_HEARTBEAT_DRIVER=heartbeat、ROOST_NEST_GAME_GATE=1、ROOST_NEST_GAME_SYNC=1、ROOST_NEST_GAME_TAIL=trace；加载隔离环境并持有其测试锁，不与其他负载并行。
