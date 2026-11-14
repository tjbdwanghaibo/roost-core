# Checkpoint 后台写者（B-3，已合入，缺省打开）

基线 `fe83c60a`，实验分支 `experiment/v147-background-checkpoint`，实验提交 `b66266ec`（实现）与 `300be3a03e22d6426b22ffe8ced0c96782ff3dac`（栈采集夹具）。

> **2026-11-14 维护者决定：不等 M-19，先把实现合入、配置默认关闭。** 以 `--no-ff` 合入（保留上述原始提交号），`dataengine.projection.background_checkpoint` 成为正式配置项，缺省 `false`；契约写在[实现篇 §2 第 7 条](../../../framework/impl/03-dataengine.md#2-必须保持的契约)与[设计篇](../../../framework/guide/03-dataengine.md)。**M-19 NOT RUN**：没有开 / 关性能对照，不能说打开后更快；“128 / 256 并发约 3 秒全体超时簇”的根因仍未确认，不能据此认定是同步 checkpoint。（同日后续：M-19 跑完、开 / 关无退步，维护者事先定的“合理就改缺省”成立，缺省改为 `true`，见文末“M-19 结果”。）下文“已跑功能检查”是实验分支当时的记录；合入后的复跑见文末“合入后复跑”。

## 实施边界

先采用一轮 ReplayPass 内的单后台写者：只有已经成功投影、持久事务标记保证重复回放安全的连续前缀，才可以交给后台确认；待写队列只保留最新前缀。记录和 fence 仍由当前回放批持有，不额外复制记录或建立跨轮未确认链表。

- `WAL.Ack` 保持同步契约，原有 `syncActive → storeCheckpoint → 内存确认 → 回收段` 顺序不动。后台写者调用这个正式入口，不把排队当成持久成功。
- 只有写入成功后才从 projector 的 WAL 未确认账本释放额度；错误仍回传给 ReplayPass。已投影不等于 checkpoint 已确认。
- 单 mutation 快路或不承诺重放安全的 Store 必须同步等确认；并行窗口若含快路同样等待。`ReplayPass` 返回前排空并退出本轮写者，Flush、HandOff、Shutdown 与下轮回放无需维护第二份重放游标。
- 配置 `dataengine.projection.background_checkpoint`，缺省打开（M-19 之后）；写 `false` 时 `ReplayPass` 走原同步路径，行为与此前相同。

这是有意受限的设计：不能跨 ReplayPass 重叠 checkpoint 与 Mongo；并行投影只有一个窗口时收益可能很小。先测它是否能在保留当前确认边界的前提下降低同步停顿；没有证据不扩展跨轮队列和生命周期状态。

## 验证计划

定向验证持久确认前不减账本、可安全继续下笔投影、仅合并最新成功前缀、失败不越过前缀、快路先确认、方法退出无写者残留。nestwal / engine 全包 race；现有崩溃恢复与真实 Mongo 重放用例。变异删除 Ack 的 syncActive，既有测试必须失败。M-19 交替测配置关 / 开；本文件届时补实测结论。

## 已跑功能检查（实验分支，2026-11-14）

- `go test -race ./framework/nestwal ./framework/dataengine/engine ./wiring/dataengine -count=1`：三个包通过（5.269 / 14.087 / 3.590 s）。新增四个实验用例覆盖安全投影与确认重叠、账本不提前释放、合并到最新成功前缀、快路遇确认失败停止、投影失败仅确认前缀。
- 变异关闭后台执行：并行继续投影用例在 5 秒断言失败；恢复后通过。变异从 `WAL.Ack` 删除 `syncActive`：既有用例失败于 `checkpoint persisted before WAL data sync`；恢复后整包 race 通过。
- 真实 Mongo 的 `TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint` 增加配置关 / 开两个子例，均通过：checkpoint 丢失后重开 WAL，已投影记录、回执、effect 不重复生效。此用例是故障注入与重开运行时，不把它写成 OS SIGKILL。
- 修改过的真实联盟脚本已实跑：后台开启、并发 4，Found / Info / Join 各档零失败、零 ≥3 秒样本。仅作为配置与 RPC 接线冒烟，未形成 A/B 容量结论。
- 原始日志：`/tmp/roost-v147-checkpoint-{targeted,sync-mutant,fsync-mutant,race,real,rpc-smoke}.log`。使用独立 offset 22000 的 Mongo / 单 Redis / NATS；同偏移 Cluster 39401 与另一个 agent 的 account2 ops 冲突，未碰对方进程，已启动的本批依赖全部停止。上述测试没有使用 Redis Cluster，不声称整套 mirror 环境验收。

## M-19 入口

合入后直接在 main（或任一含本合入的检出）上用同一夹具交替 `ALLIANCE_E2E_BACKGROUND_CHECKPOINT=0|1`，其余参数一致；`ALLIANCE_E2E_RPC_CONCURRENCY=128|256`，`ALLIANCE_E2E_PERF=1`，`ALLIANCE_E2E_RUN='^TestAllianceE2ERPCConcurrency$'`。两档是同一棵树的配置关 / 开，不再需要单独的实验工作树。写额度固定 512；Join 分散 16 联盟，单热点档每轮限制总入会数在 500 人业务上限内。每次调用打印 ≥3 秒样本的开始时间、耗时与错误，可据此核对超时簇。正式运行前换空闲私有偏移并检查全部端口，不能复用仍占用的 39401。

`ALLIANCE_E2E_STACKS=1` 只在临时生成工程加入 SIGUSR2 栈采集器（[stack_capture.go.txt](../../../../scripts/testdata/checkpoint-experiment/stack_capture.go.txt)，由 `scripts/test-alliance-e2e.sh` 复制；正式模板与生成器不含它）。调用方每 500ms 检查一次：该测量阶段尚有请求未完成、连续至少 1s 没有完成时，向本用例持有的服务进程请求 goroutine 栈，每阶段至多两份。服务日志保留 `CHECKPOINT STACK BEGIN/END` 与 UTC，调用方记录阶段、已完成数、等待时长与信号结果；正常调用不抓栈。两个配置档都使用同一开关，触发采集的样本必须注明额外诊断开销，不能拿栈出现次数直接证明因果。独立信号冒烟已确认完整栈及进程存活，日志 `/private/tmp/v147-checkpoint-stack-smoke/run.log`；完整夹具运行结果随 M-19 记录。

## 合入后复跑

2026-11-14，合入后的树上（macOS 本机，未跑 M-19 / 联盟夹具）：

- `GOWORK=off go test -race ./framework/nestwal ./framework/dataengine/engine ./wiring/dataengine -count=1`：三个包通过（7.180 / 14.036 / 5.573 s）。
- 真实 Mongo（私有副本集，端口偏移 24300，只起 Mongo）`go test -tags integration -race -run '^TestRealMongoSpecialRecordsReplayExactlyOnceAfterLostCheckpoint$'`：`background=false` / `background=true` 两个子例均 PASS；用后已停并删除私有根目录。
- 配置：`TestBackgroundCheckpointIsDeclaredOffByDefault`（codegen，声明为 bool、缺省 false、不写进生成配置，config check 拒绝非布尔值；M-19 之后改名 `TestBackgroundCheckpointIsDeclaredOnByDefault`、缺省 true）与 wiring 的缺省断言通过；把声明缺省改为 true 的变异两条都红。`TestKitConfigSchemasMatchKitDeclarations`、`TestGameDemoConfigsPassTheDoctorConfigCheck` 等配置契约用例通过。
- 根包 `go test . -count=1` 通过（含 `TestDocumentedCommitsAreReachableFromHEAD`、`TestTrackedMarkdownRelativeLinksResolve`）；`scripts/check-doc-anchors.py` 0 处断链。
- 联盟夹具只做了语法解析，与实验提交相比只改了一行日志文字（`PERF rpc config background_checkpoint=… concurrency=… max_concurrent_writes=512`），没有重跑真实联盟脚本。

**当前结论：实现与功能验证完成并已合入；M-19 开 / 关无退步、也没测出收益，配置缺省改为打开。不能据此认定 checkpoint 是历史 3 秒整体停顿的根因。**

## M-19 结果（2026-11-14）

在含合入的同一棵树（feat/v148）上，用[性能入口](../v147-performance-preparation-20261114/README.md)的 `s14-checkpoint128` / `s15-checkpoint256` 段（`--output-name …-v148`），`ALLIANCE_E2E_BACKGROUND_CHECKPOINT=0|1` 交替各 3 轮，`ALLIANCE_E2E_STACKS=1`，写额度 512；macOS 本机，私有镜像环境偏移 18000，运行时 load 约 3.2～5.0。12 次全部退出 0。原始日志在该树的 `artifacts/perf/v147-remaining/s1{4,5}-checkpoint*-v148/`（不入库）。

第一次运行（未计入）两处入口问题先修掉：D22 之后 rehome 演练插桩锚点漂移（联盟脚本在插桩一步退出），`run-micro.py` 的循环变量 `round` 遮蔽内建 `round()`。

每阶段跨 3 轮（found 每轮两段，共 6 个样本）取中位，max 取最大，失败与 ≥3s 取合计：

| 档 | 阶段 | bg | 轮数 | 吞吐/s 中位 | p50 ms 中位 | p99 ms 中位 | max ms 最大 | 失败合计 | ≥3s 合计 |
|---|---|---|---|---|---|---|---|---|---|
| c128 | found | 0 | 6 | 43.6 | 119.90 | 144.62 | 304.0 | 0 | 0 |
| c128 | found | 1 | 6 | 45.7 | 125.00 | 173.91 | 300.9 | 0 | 0 |
| c128 | info | 0 | 3 | 3891.8 | 0.22 | 0.48 | 2.9 | 0 | 0 |
| c128 | info | 1 | 3 | 4583.6 | 0.19 | 0.49 | 2.7 | 0 | 0 |
| c128 | info-same | 0 | 3 | 31681.6 | 3.97 | 5.09 | 7.8 | 0 | 0 |
| c128 | info-same | 1 | 3 | 28885.5 | 4.12 | 6.39 | 7.0 | 0 | 0 |
| c128 | info-spread16 | 0 | 3 | 94038.5 | 1.23 | 2.96 | 13.4 | 0 | 0 |
| c128 | info-spread16 | 1 | 3 | 96043.2 | 1.22 | 2.83 | 6.8 | 0 | 0 |
| c128 | join | 0 | 3 | 21.9 | 44.72 | 56.36 | 63.5 | 0 | 0 |
| c128 | join | 1 | 3 | 22.2 | 44.47 | 55.95 | 86.9 | 0 | 0 |
| c128 | join-same | 0 | 3 | 21.2 | 5947.89 | 6071.95 | 6326.1 | 0 | 581 |
| c128 | join-same | 1 | 3 | 20.8 | 6090.50 | 6203.67 | 6259.9 | 0 | 584 |
| c128 | join-spread16 | 0 | 3 | 125.9 | 975.93 | 1132.15 | 1226.0 | 0 | 0 |
| c128 | join-spread16 | 1 | 3 | 127.3 | 972.92 | 1134.53 | 1236.7 | 0 | 0 |
| c256 | found | 0 | 6 | 45.4 | 122.38 | 165.77 | 296.4 | 0 | 0 |
| c256 | found | 1 | 6 | 44.8 | 120.14 | 165.56 | 324.1 | 0 | 0 |
| c256 | info | 0 | 3 | 4044.0 | 0.22 | 0.52 | 2.7 | 0 | 0 |
| c256 | info | 1 | 3 | 3890.9 | 0.22 | 0.56 | 2.9 | 0 | 0 |
| c256 | info-same | 0 | 3 | 29521.7 | 7.97 | 13.20 | 13.9 | 0 | 0 |
| c256 | info-same | 1 | 3 | 28036.9 | 8.44 | 12.64 | 13.6 | 0 | 0 |
| c256 | info-spread16 | 0 | 3 | 91624.5 | 2.48 | 6.68 | 12.8 | 0 | 0 |
| c256 | info-spread16 | 1 | 3 | 93364.7 | 2.44 | 6.04 | 10.9 | 0 | 0 |
| c256 | join | 0 | 3 | 22.1 | 45.33 | 55.84 | 56.8 | 0 | 0 |
| c256 | join | 1 | 3 | 21.8 | 46.32 | 53.82 | 63.2 | 0 | 0 |
| c256 | join-same | 0 | 3 | 20.9 | 6047.42 | 12126.31 | 12284.1 | 0 | 577 |
| c256 | join-same | 1 | 3 | 20.7 | 6203.79 | 12241.88 | 12391.9 | 0 | 581 |
| c256 | join-spread16 | 0 | 3 | 127.0 | 1978.62 | 2310.90 | 2429.1 | 0 | 0 |
| c256 | join-spread16 | 1 | 3 | 126.3 | 1981.70 | 2244.85 | 2360.1 | 0 | 0 |

读法：

- 开 / 关在每个阶段都在噪声内持平，零失败；没有阶段因打开变差，也没有阶段明显变好——这个夹具的瓶颈不在 checkpoint。
- ≥3 秒样本全部出在 `join-same`（同一联盟入会，约 21 次/秒串行，128 / 256 并发排队 6～12 秒），开 / 关数量一样，是单联盟热点入会的排队，不是 checkpoint 停顿。
- 栈采集器一次也没触发（没有“尚有请求、连续 ≥1 秒无完成”的阶段），历史“约 3 秒全体超时簇”本轮未复现，根因仍未确认。
- 维护者 2026-11-14 的要求是“合理的话改成缺省打开”：判据为打开后吞吐、延迟不差，失败与 ≥3 秒样本不多。判据成立，缺省改为 `true`；`TestBackgroundCheckpointIsDeclaredOnByDefault` 与 wiring 的缺省断言随之改为打开。
