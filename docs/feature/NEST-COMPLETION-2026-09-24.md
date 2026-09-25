# Nest N1～N4 优化实施与验收

2026-09-24。既定 [Nest 优化方案](REFACTOR-2026-09-24-nest-and-immediate-sync.md) 的 N1～N4 已完成，本轮一次收口剩余事务主线、阶段指标和 profile 驱动的分配优化。最新截止复核见第 8 节（补修 RR-07/08 后可按当前范围收口）。所有变更在工作树中，未提交、未发布。基线为 `967bc69` 加此前已实施的 Sync / Nest N1 / N2a 修改，性能比较采用本轮修改前实测，不能解释为相对该 Git commit 的整仓收益。

## 1. 完成范围

| 批次 | 最终实现 | 验收 |
| --- | --- | --- |
| N1 职责整理 | 原 nest 包内分为 handler 注册、dispatch 路由/锁、execution 事务主线、rollback 状态、trace 诊断；补核心中文注释 | 公开 import 保持，生成工程编译运行通过 |
| N2 执行与所有权 | single/multi/multiGroup 共用加载/锁/收尾；事务按业务、拒绝或准入、解锁、完成显式执行；完成交接由 completionHandoff 表达 | 参数顺序/nil/重复项/空组、回滚/panic、队列满、完成顺序、关闭、远端路径回归 |
| N3 阶段观测 | `NestOptionWithStageMetrics(true)`，默认关闭；queue/lock/handler/capture/admission/WAL/释放/完成队列等 | 各提交路径指标有无、默认关闭、不重复准入计时、实测开启成本 |
| N4 热路径 | 慢请求 watch/Timer/channel 安全复用；ticker 注册时复制快照；多实体锁与引用集合预分配；关闭阶段指标时不分配额外释放计时闭包 | 同环境前后微基准、CPU/heap profile、计时器到期竞争与回调内注册回归 |

没有新增调度子包或兼容转发层。多实体业务参数、排序锁集合、引用归还集合仍分别拥有数组，不能为了省内存共用并破坏业务顺序。单实体的短切片保持简单实现；profile 不支持为其引入专门对象池。通用 metrics/fctx 的优化不夹入 Nest 重构。

## 2. 当前执行主线

```text
Client / 生成 Sender
  → Dispatcher 队列（业务消息不等待 ticker）
  → 路由、加载、Touch、组检查和锁
  → 业务调用
       失败 → Rollback → 释放 / 回复错误
       成功 → 准备记录 → strict Commit 或 pipelined Enqueue
  → 锁内成功准入 / AfterAdmission / Sync 冻结
  → 释放实体和组锁、Guard 中动态锁
  → 完成回调 / 回复；pipelined 同时受 WAL ticket 约束
```

`callTransactionHandler` 的恢复范围只包括业务调用。准入后释放或完成失败，不能再回滚已接受的状态；明确拒绝走 `rejectCommit`，不确定提交走 abandon/fencing。没有用一个 success 标志混淆“未提交”和“已持久化但回调失败”。

广播保持逐实体事务与异常隔离，没有提前释放闭包，pipelined 在该路径仍按 strict 锁内提交；远端写批次保留原最终确认协议。RunDetachedTransaction / RunIsolatedTransaction 继续共用事务执行入口，其隔离约束不变。

完成池在锁内占用同主实体排序位置，随后必须等待本事务解锁屏障和前序完成。队列满仍降级为当前 worker 等待；完成闭包保存必要值，不持有池化 Msg。完成排序依旧按主实体，不扩展为共享任一次实体的全局 FIFO。

本轮确认修复：

- [RR-20260924-05](../bug/RR-20260924-05.md)：已完成 ticket 可以让 AfterCommit 和回复跑在 Entity 解锁之前。[fix](../bugfix/RR-20260924-05.md)
- [RR-20260924-06](../bug/RR-20260924-06.md)：两个独立 atomic 不能保证 Ticker 的 Start/Stop 状态转换，Stop 返回后仍可能启动。[fix](../bugfix/RR-20260924-06.md)

前批 RR-01～04 的准入、组锁异常、动态 Cast 和广播清理修复继续保留。问题记录和修复记录均已加入索引。

## 3. 阶段指标用法

```go
engine := nest.NewEngine(
    nest.NestOptionWithGetter(getter),
    nest.NestOptionWithStageMetrics(true),
)
```

Kit 可通过现有转发 NestOption 的入口装配，不添加第二份观测配置。`nest.stage.duration{handler,stage}` 的每阶段精确定义见 [可观测性清单](../../OBSERVABILITY.md)。标签仅含 handler 名和固定阶段，不含 entity ID。

默认关闭；旧 dispatch.cost / lock_hold 保持原计时边界。lock_hold 不包含完整 release hook，不能作为完整持锁成本。新阶段也不是端到端的可相加拆分：异步阶段重叠，动态 Cast 处于业务调用内，广播每实体采样，nested transaction 成本可重叠。Duration 没有 p99，业务端到端分位数仍由负载测试负责。

关闭阶段指标不会关闭慢请求诊断。200ms 运行中 trace 仍保留：watch 只在 Stop 成功或确认计时回调完成后回池，不提前复用 Msg 引用。

## 4. 性能实测

环境：Go 1.27.0，darwin/arm64，Apple M5，GOMAXPROCS=4，GOWORK=off。每项 300ms、3 次，表中 ns/op 取中位数；主对比均为阶段指标关闭，包含既有常规指标。Request 用真实 Engine/worker/Guard、内存 getter、空业务，不含网络和真实 WAL fsync。Multi 为 4 实体。

| 场景 | 修改前 ns/op | 最终 ns/op | 修改前 → 最终 B/op | 修改前 → 最终 allocs/op |
| --- | ---: | ---: | ---: | ---: |
| 单实体 Request | 3068 | 3142 | 2379 → 2076 | 52 → 48 |
| 四实体 RequestMulti | 3472 | 3503 | 2644 → 2340 | 58 → 54 |
| 慢请求 watch | 118.7 | 58.75 | 304 → 0 | 4 → 0 |
| Tick（8 个回调） | 61.47 | 53.10 | 64 → 0 | 1 → 0 |

完整请求的分配字节下降约 12.7% / 11.5%；耗时中位数分别增加约 2.4% / 0.9%。样本不足以断言统计显著性，不能宣传完整请求吞吐提高。明确收益是分配减少；watch 热路径耗时约减半。零分配是复用稳定后的微基准结果，不保证 sync.Pool 经 GC 清理后的每次首次请求零分配。

N3 开关另用同一个 1 实体 RequestMulti fixture 对比（这是独立的开关成本实验，不与上面的 RequestSingle 直接比较）：

| StageMetrics | ns/op 中位数 | B/op 中位数 | allocs/op |
| --- | ---: | ---: | ---: |
| false | 3083 | 1908 | 52 |
| true | 5193 | 3005 | 81 |

这个空 handler 场景开启指标增加约 2.11µs、1097B 和 29 次分配；采样经通用 metrics 标签构造和注册表，适合排障时启用，不默认让所有请求承担成本。

### Profile 的决策依据

修改前 alloc_space 中慢请求 watch 累计约占 12.07%，是有明确收益且职责属于 Nest 的对象复用点。修改后该路径不再是主要分配热点；metrics.metricKey 累计约 44.02%（包含 labelsKey 等，不能重复相加），请求超时 Timer、fctx、消息信封和 Guard 仍有成本。它们有独立生命周期，未用无证据的池化去交换可读性或正确性。

CPU profile 在本机主要落在 runtime 的 pthread_cond_signal / pthread_cond_wait 等调度函数；不据此给出线上核数、最大 QPS 或 1000 玩家 50ms 达标结论。

原始样本位于本地忽略目录 [artifacts/perf/nest/20260924-final](../../artifacts/perf/nest/20260924-final/)：

- `before.txt`、`before.cpu`、`before.mem`：本轮修改前；原编译二进制 `/tmp/roost-nest-before.test`。
- `after.txt`：第一轮结果，包含未关闭的释放计时包装分配；作为过程样本保留。
- `final.txt`、`final.cpu`、`final.mem`、`final.test`：最终版本；`final.env.txt` 记录工具链和工作树。
- `final-cpu-top.txt`、`final-alloc-top.txt`：最终 profile 文本。

对比表和方法保存在本文，可跟随源码提交；大体积 profile 与测试二进制不进入 Git。

### 复跑

```sh
GOCACHE=/tmp/roost-nest-go-cache \
ROOST_PERF_LABEL=local ROOST_PERF_COUNT=5 ROOST_PERF_BENCHTIME=1s \
ROOST_PERF_PROFILE=1 bash scripts/perf/nest.sh
```

脚本默认 GOMAXPROCS=4、GOWORK=off，产物在 artifacts/perf/nest；支持 ROOST_PERF_CPU / OUTPUT / BENCH 自定义。CPU 和 heap 使用额外一轮 2s 的单实体 Request 采集，不混入主对比样本。

## 5. 验证

新增回归覆盖解锁屏障、2 万次 Ticker 并发 Start/Stop、阶段指标各提交模式、tick callback 内注册新 callback、8 goroutine × 1000 次慢请求计时器到期/Stop 复用。

```sh
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go test -race \
  ./nest ./entity ./dataengine ./dataengine/engine ./nestwal ./remoteentity \
  ./sync/entitysync/... ./kit/nest ./codegen/internal/nest ./codegen/internal/entity \
  -timeout=120s

GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go vet \
  ./nest ./kit/nest ./codegen/internal/nest ./codegen/internal/entity
GOCACHE=/tmp/roost-nest-go-cache GOWORK=off go build ./...
GOCACHE=/tmp/roost-nest-go-cache bash scripts/test-sync-modes-generated.sh
bash -n scripts/perf/nest.sh
git diff --check
```

11 个关联包 race 全部通过，Nest 3.803s、DataEngine/engine 5.257s、NestWAL 6.290s、remoteentity 3.404s、Kit/Nest 3.529s；其余以工具输出的实际运行或有效缓存为准。vet、全仓 build 退出 0；build 有一次模块版本 stat cache 写权限提示，不影响编译退出。真实 DAO/Entity 生成最小工程 race 通过（syncmodes 1.609s），不依赖 demo。性能脚本也已实际执行，包括 profile 导出。

本轮不重跑 MMO 网络负载，不扩大为真实远端存储故障、多机长稳或全部业务包测试结论。Nest 已列出的优化批次无剩余实施项，线上业务容量仍应以实际业务和部署环境验证。

## 6. Sync 契约与回退

本轮未改变 Sync 算法、发送频率或默认模式。setter 只标脏；成功准入时、Guard 释放锁之前统一捕获，解锁且提交确认后允许发送。周期默认与 on_change + 周期兜底共用正式链路，已做兼容回归。

可单独关闭阶段指标；watch 池化、ticker 注册快照优化可独立回退。RR-05/06 正确性修复应保留，不能为了回退性能实现恢复旧竞争。N1～N4 不改变对外包路径或 Sync 线协议。

## 7. 图谱证据

使用 codebase-memory Verify，先确认项目 Users-whb-roost-roost-core 和代际 2026-09-24T06:01:52Z，查询事务、完成、调度、ticker 调用链并核对关键源码；本轮修改后刷新至 2026-09-24T06:26:45Z，17962 nodes / 141376 edges。12 个本轮代码/回归/基准文件 coverage 均为 metadata_match/no_recorded_issue，nest 范围无记录缺口。

此信号不是完备性证明。两个未改动 demo 模板的 parse_partial 保留；docs/scripts/生成夹具/profile 依 fast 规则排除，直接阅读并执行适用检查，不从图谱缺席推断没有引用。

## 8. 截止复核与收口结论

2026-09-24 后续复核。N1～N4 的实施状态保持完成，但上一轮的异常组合覆盖不足，本轮补查发现并修复 [RR-07](../bugfix/RR-20260924-07.md) 和 [RR-08](../bugfix/RR-20260924-08.md)。因此，上一轮“完成”不能理解为不存在后续可发现的缺陷。

**结论：按当前 Nest 对外接口、提交模式和正式 Sync 契约，可以收口并进入维护阶段。本次复核范围内已确认的问题均已修复，无待实施优化批次。** 这不是全仓零缺陷或线上容量保证，也不以不断增加新的重构点作为完成标准。

### 本轮核对的边界

| 边界 | 当前结果与证据 |
| --- | --- |
| 路由、加载、Touch/锁所有权 | 普通单/多/分组共享收尾；原参数顺序、重复项、nil/空组与广播隔离测试继续通过 |
| 业务失败 / 准入拒绝 | rollback、undo/state、AfterAdmission 时机、CommitRejected 与不确定提交不回滚回归通过 |
| 动态实体 | Cast 捕获、ReleaseCast 延迟、动态实体 LSN 和提前解锁回归通过 |
| pipelined 完成 | 普通等待 / 完成池 / 队列满降级统一完成责任；解锁屏障、同主实体排序、单次回复通过 |
| release hook 异常 | 新增 RR-07：durable 时完成回调和通知，indeterminate 时 abandon/fence；两类结果都保留释放错误并回复 |
| AfterCommit 异常 | 新增 RR-08：已经解锁的 pipelined 不再因为 scope 尚在而推迟回调；后续回调继续，返回 ErrAfterCommitFailed |
| 队列、取消与停止 | 满队列准入、取消上下文、延迟请求停止回复、完成池排空、Shutdown deadline 和 Ticker 并发启停既有回归通过 |
| Sync 与装配 | setter 标脏、锁内准入捕获、解锁且确认后可发的契约保持；正式生成工程双模式 race 通过 |
| 结构与观测 | 保留单包职责文件；阶段指标默认关闭；没有新增调度包或业务 demo 接线 |

### 本轮验证结果

新增 [completion_release_panic_test.go](../../nest/completion_release_panic_test.go) 两个测试、九个子用例，覆盖三种完成路径 × 两类 WAL 结果，以及三种路径的回调异常。RR-07 原三个 durable 子用例和 RR-08 两个失败子用例均保留实际修前红证据；无任意 sleep。

使用第 5 节的相关包 race 命令，11 包全部通过：Nest 2.727s，DataEngine/engine 4.940s，NestWAL 5.238s，Kit/Nest 2.898s，其余包为有效缓存或正常运行。vet 增加 `./entity` 后通过，全仓 build 退出 0（仍有模块 stat cache 写权限提示）。真实生成工程 race 通过，syncmodes 2.071s。文档相对链接、gofmt 与 diff whitespace 检查通过。

本轮改变了常规路由的幂等释放实现，故重跑同参数 Request 微基准（3×300ms，GOMAXPROCS=4）：

| 场景 | 上一轮 final ns/op | 本轮 cutoff ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| 单实体 Request | 3142 | 3310 | 2076 → 2043 | 48 → 48 |
| 四实体 RequestMulti | 3503 | 3426 | 2340 → 2300 | 54 → 54 |

本轮原始样本为 `artifacts/perf/nest/20260924-final/cutoff.txt` 和 `cutoff.env.txt`。单实体耗时中位数约增加 5.3%，多实体约减少 2.2%；仅三次样本，不宣称统计显著的吞吐提升或无性能回退。两项分配次数保持，分配字节略降；本轮属于正确性补全，性能数字原样保留。第 4 节的完整 profile 属于上一轮，未冒充本轮新采样。

### 收口边界

- strict / memory 仍保留 Guard post-release callback 的原有日志处理契约；本轮只统一 pipelined 已释放路径的回调错误返回，不把所有 callback 错误都改为业务事务失败。
- 完成排序按主实体，不承诺多实体共享任一次实体的全局完成 FIFO。
- 通用 metrics / fctx 的分配、业务 handler 热点和部署调优可按实际 profile 另行处理，不作为已列 Nest 批次的欠项。
- 本轮没有重跑跨机 MMO / 真实存储故障 / 长稳，1000 玩家、10000 全局实体、每人约 50 可见、1%/5% 变化和 50ms SLA 的线上验收不能由这些 Nest 微基准替代。

代码与文档尚未提交、未发布，保留既有工作树变更。后续按具体缺陷或实际 profile 进入维护，不继续无目标拆包或重写调度。

本轮图谱使用 Verify，搜索和双向 trace 覆盖事务交接、dispatch 与相关回归，相关查询均无剩余分页；对同名 complete/Join 等不相关图边用源码补证。最终刷新代际 `2026-09-24T06:50:16Z`（metadata recorded_at `06:51:48Z`），17968 nodes / 141504 edges；本轮六个代码/回归文件均为 metadata_match/no_recorded_issue，nest 范围无记录缺口。依然不是完备性证明；未改动 demo 模板的两个 parse_partial 与 fast 模式排除范围同第 7 节。


## 9. 消息吞吐压测前的再收尾

2026-09-24，用户要求再收尾并测 msg 吞吐。本次核对 RR-07/08 的释放异常与内联完成路径、正式 Guard/Client 调用及计数口径；未发现需要再修改运行逻辑的新问题，没有重复拆包或扩展完成回调契约。

重跑 `go test -race ./nest ./entity ./dataengine/engine ./nestwal ./kit/nest ./sync/entitysync/... -timeout=120s`：七包通过，Nest 3.388s、DataEngine/engine 3.911s、NestWAL 4.844s、Kit/Nest 3.229s，其余有效缓存。本次新增的是独立压测夹具与脚本，fixture 的四个场景 race 通过（2.670s）。

消息吞吐不使用单线程 ns/op 换算，而是正式 Client、EntityManager、Entity 锁与 worker 的并发压力实测，含成功完成计数和结束排空。方法、结果和可复跑命令见 [Nest 消息吞吐压测](NEST-MSG-THROUGHPUT-2026-09-24.md)。当前功能收口结论保持；压力结果仅适用于文档列明的业务成本与配置。

正式压力结果：30 个样本累计完成 1.5 亿条；P4/W4 分散 Dispatch / Request 中位数为 803073 / 586412 msg/s，所有样本错误与拒绝为 0。测试采用简单内存业务，不代表完整 MMO 链路容量；完整分位数、最大样本和边界见上述压测报告。

2026-09-25 补充：慢请求逐请求日志和指标保持；全进程堆栈改为每 5 秒至多一次，避免 Remote 依赖变慢时重复诊断放大压力，见 [RR-20260925-03](../bugfix/RR-20260925-03.md)。
