# Nest 消息热路径验证（2026-10-13）

本轮合并远端三个提交，保留本地修复，合并点为 `f1fd7fb8`。随后按 [优化设计](../framework/NEST-OPTIMIZATION.md) 实施局部优化。没有改变三池数量、默认队列容量、ID tail、提交保证或业务协议，也没有运行新的 15 分钟游戏混合负载。

## 审查与实施结论

核对范围为远端 `58d27e7a`、`fd13e178`、`cc635481`：客户端标准化结果具有独立存储，分组切片限制容量；Msg 字段整理没有删除提交状态；队列 lane 参数与调用方一致，worker option 只写执行身份，可以在 worker 内复用。此范围未发现确定缺陷，不代表全仓无缺陷。CBM 代际为 `2026-10-10T03:58:22Z`，过期文件通过当前源码补证。

方案方向合理，但采样占比不能直接当提速比例；缓存生命周期与行为校验必须先于缓存复用。最终保留：

| 改动 | 约束与证据 |
| --- | --- |
| 分组展开一次分配 | 保留目标顺序、重复项及组边界；[分批样本](benchmarks/nest-hot-path-20261013/group-prealloc.txt) |
| 直接传 Snapshot option | 避免临时切片使闭包逃逸，不跳过上下文安装 |
| 同消息身份登记复用 | 只省当前消息的 sync.Map 重复写；Guard、事务与 fctx 作用域仍独立；[上下文样本](benchmarks/nest-hot-path-20261013/context.txt) |
| 锁列表与 Touch 列表合并分配 | 两个互不重叠的容量区间，排序不影响引用归还 |
| 小分组快照放在栈上 | 1～4 实体使用局部数组，大集合扩容；重试重新采集，不进入释放闭包；[存储样本](benchmarks/nest-hot-path-20261013/storage.txt) |
| 相同前驱只登记一次 | 共享锁内检查前驱的最后一个后继，同时登记计数，不增加 map；[之前](benchmarks/nest-hot-path-20261013/edges-before.txt) / [之后](benchmarks/nest-hot-path-20261013/edges-after.txt) |

目标视图缓存（包括慢准备、Cast 复用）与指标预绑定明确延期。前者需要统一直接 Msg、Clone、重排、广播与续行的生命周期，当前仍保留显式校验；后者缺少 Registry 句柄接口，须一起设计 Reset、动态标签和 series 限额。本轮没有引入第二套缓存或指标状态机。

## 对照方法

Apple M5、macOS/arm64、Go 1.27.0；`GOMAXPROCS=0`，实际 10。原版为 `f1fd7fb8`，新版运行时代码为 `e6f54068`。基准文件在两版使用相同逻辑，正式测量无 race/profile，期间没有并跑全仓测试或索引刷新。

`BenchmarkNestHotPath` 经过正式 Client、队列、Guard 和 Handler，Getter 是内存夹具。矩阵共 96 组，每组 5 个样本：空 Handler/固定 CPU 工作量，1/8 worker，热点/独立实体，单实体/双实体/四实体/1组/8组/32组，Request/Dispatch。每组含 4 个实体。

CPU 档初次校准为 68578 次算术循环，名义约 100μs；后续对照固定同一工作量，不使用 sleep。Request 使用 Go benchmark 并行调用（本机 10 个生产者）；Dispatch 使用单生产者、64 个在途槽，计时包含 Shutdown 排空。每组核对实际完成数等于 `b.N`，准入或回复错误立即令基准失败。微基准没有记录端到端 p99/max，不能替代游戏场景压力验收，也不能换算 Remote/Saga TPS。

[全部原始基线](benchmarks/nest-hot-path-20261013/before.txt)、[全部新版样本](benchmarks/nest-hot-path-20261013/after.txt)、[96 组中位数及差值](benchmarks/nest-hot-path-20261013/comparison.csv)。表中时间为五次中位数，不作统计显著性承诺。

8 worker、热点、空 Handler、异步 Dispatch：

| 目标 | 原版 ns/op | 新版 ns/op | 原版 B/op → 新版 | 原版分配次数 → 新版 |
| --- | ---: | ---: | ---: | ---: |
| 单实体 | 3608 | 3020 | 1506 → 1120 | 42 → 34 |
| 双实体 | 3723 | 3180 | 1660 → 1272 | 47 → 39 |
| 四实体 | 4294 | 3452 | 2030 → 1450 | 50 → 39 |
| 1 组 | 4915 | 3971 | 2127 → 1514 | 55 → 43 |
| 8 组 | 13096 | 8409 | 7413 → 6467 | 61 → 45 |
| 32 组 | 32823 | 24127 | 26466 → 22821 | 66 → 45 |

8 worker、独立四实体、CPU 档异步样本为 21305→17380 ns/op、1990→1455 B/op、48→39 allocs/op。这是并行均摊时间，不是单个 Handler 耗时。CPU 主导的其他场景收益较小，全部结果保留在 CSV 中。

## 异常样本与复核

首次完整矩阵中，单 worker、独立双实体、空 Handler、异步 Dispatch 的中位数由 3244 增至 4859 ns/op（约 +49.8%）；分配仍由 1636 降至 1267 B/op、45 降至 38 次。该组没有从报告中删除。

全仓测试与 race 结束后，用 Go overlay 将四个生产文件恢复为 `f1fd7fb8`，构建原版二进制；新版单独构建。两版每轮交替先后，各测 5 次，每个样本延长至 1 秒；同时复核单实体和单组。运行的测试夹具相同，没有通过删除断言或改 worker 数回避退化。

| 单 worker、独立实体、空 Handler、异步 | 原版 ns/op 中位数（范围） | 新版 ns/op 中位数（范围） |
| --- | ---: | ---: |
| 单实体 | 2714（2625～3100） | 2613（2478～2663） |
| 双实体 | 3030（2876～3302） | 2800（2719～2906） |
| 1 组 | 3521（3416～3815） | 3259（3162～3347） |

双实体分配为 1634→1231 B/op、45→36 次。这次未复现稳定退化；首次异常尚未唯一归因，不能称已证明是某种调度噪声。[交替原版样本](benchmarks/nest-hot-path-20261013/alternating-before.txt)与[交替新版样本](benchmarks/nest-hot-path-20261013/alternating-after.txt)全部保留。

## 回归中发现的配置问题

首次全仓测试在 App 的声明式配置守卫失败：Nest 的合批窗口通过 Viper.IsSet 判断显式零值，绕过统一解码入口。根因来自已有 `9d97a2115`，不是本轮远端三个提交或六项热路径改动引入。

`45fa3f55` 增加 `app.LoadConfigWithPresence`，在同一次声明解码中记录来源存在性；Nest 使用返回值，保留未配置沿用代码值、显式 0 关闭合批、Configure 最后覆盖。无白名单豁免、无配置格式或生成快照变化。[修前失败](benchmarks/nest-hot-path-20261013/config-declaration-before.txt)、[修后定向验证](benchmarks/nest-hot-path-20261013/config-declaration-after.txt)包含配置守卫、默认/零值/环境变量、生成快照和 doctor 回归。普通 LoadConfig 不增加元数据分配，新增逻辑只影响初始化，不参与以上热路径基准。

## 验证结果与边界

- 全仓 `GOWORK=off go build ./...`、`go vet ./...` 通过。
- 首次全仓测试只有上述声明守卫失败；修复后 `GOWORK=off GOMAXPROCS=0 go test ./...` 全部通过，见 [完整输出](benchmarks/nest-hot-path-20261013/tests.txt)。
- Nest、Wiring/Nest、fctx、EntitySync 的 [race](benchmarks/nest-hot-path-20261013/race.txt) 通过；配置修复后另跑 App 与 Wiring/Nest [race](benchmarks/nest-hot-path-20261013/config-race.txt)，均通过。
- 新增分组顺序/边界、嵌套消息身份及 panic 恢复、前驱合流/分流、声明存在性回归；既有 Remote、Await、取消、关闭、回滚与提交测试保留。根包 [文档与契约检查](benchmarks/nest-hot-path-20261013/docs-check.txt) 通过。

以上是默认测试与本地基准。没有重新运行 integration 标签、跨机故障、外部资源专用矩阵或 15 分钟游戏混合负载；依赖环境条件而跳过的测试不计为资源验收。此前游戏场景的长尾限制继续保留。

## 复跑

```sh
GOWORK=off GOMAXPROCS=0 ROOST_NEST_BENCH_ITERATIONS=68578 \
go test ./framework/nest -run '^$' -bench '^BenchmarkNestHotPath$' \
  -benchtime=100ms -count=5 -benchmem
```

复跑原版时另建 `f1fd7fb8` worktree，将 `9a3630e3` 的 `framework/nest/hot_path_bench_test.go` 复制到其中；只移植基准，不移植生产改动。使用同一工作量，交替运行两版以控制运行顺序影响。

分配诊断单独运行 `BenchmarkClientRequestMulti`：`-memprofile /tmp/nest.mem -o /tmp/nest.test`，再用 `go tool pprof -top -alloc_space /tmp/nest.test /tmp/nest.mem`。本轮 [采样摘要](benchmarks/nest-hot-path-20261013/alloc-profile.txt) 来自第一批之后，不能把累计调用链占比相加或当作 CPU 占比。
