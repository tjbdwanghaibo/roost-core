RV-20261022-03 policy 基准前后对照（同机交替 12 轮，奇数轮先跑修前、偶数轮先跑修后）。
修前：fix/rv-20261022-03 基线 1df1fdb8 的 policy 包测试二进制；修后：同分支修复提交。
命令（在 framework/sync/entitysync/policy 目录）：
  <bin> -test.run '^$' -test.bench 'BenchmarkSharedSpatialView|BenchmarkSpatialViewsPerPlayer|BenchmarkRelationViewAttach' -test.benchmem -test.benchtime 300ms
benchstat before.txt after.txt > benchstat.txt
机器：Apple M5 / darwin arm64，测量期间有其他 agent 的测试与基准在跑（uptime 负载 4～6），交替执行抵消时段差。
结论：sec/op 各项均不显著（p 全部 > 0.05，RelationViewAttach/members=1024 为 0.052），几何均值 +1.44%（不显著）；B/op、allocs/op 与修前相同。
中途发现并修掉的回退：takeLog 在日志为空时也把 r.log 置 nil，丢掉 requeue 留下的容量，views=64 固定 3000 次时 B/op +320（record 的分配 2.7MB→7.9MB）；修后与修前相同。
