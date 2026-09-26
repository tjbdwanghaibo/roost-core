# 2026-09-26 三模块复审核实与修复

**提交更新（2026-09-26）**：用户已授权提交，RR-03～24 的修复、回归与文档随本次 main 提交保存；未执行 push 或发布。下文“未提交”和索引刷新失败是修复验收时的记录。

基线：main `aaada47`，实现基线 `a22c5a5`。日期：2026-09-26。来源：[外部复审](REVIEW-2026-09-26-core-optimization.md)。本轮在原包内修复，不新增生产包，不改线协议、WAL 格式或默认配置；以下状态表示工作树已修复和验证，不表示已提交、推送或部署。

## 判断与处理

| 发现 | 核实结果和本轮处理 |
| --- | --- |
| RR-03：Slow 快阶段 RunLocal 自等 | 正确。实际快 worker 上就地执行，续行期间屏蔽慢 executor，返回慢阶段前恢复。单 worker / 两个 worker 全占满均有旧版失败、新版通过证据 |
| RR-04：大快照饥饿 | 正确。字节预算挡住首个候选时停止后续小包插队；会话内按请求入队顺序，避免持续增长的 Entity ID 越过旧请求。保留类别交替和会话轮转 |
| RR-05：未尝试会话被预扣预算 | 正确。计划只预留，结束时只结算实际尝试 Push 的会话；窗口预算和全局游标按同一计划顺序更新。取消、失效、编码失败、RetryLater、部分帧成功分别回归 |
| RR-06：快阶段阻塞入口缺保护 | 正确。fctx 的实际执行位置不进入 Snapshot；ManagerAccess / Repository 在冷加载及 singleflight 前 panic，Remote 准备/确认/释放和快续行自等待入口提前检查。原 Nest.Request 的 ErrSyncInHandler 保留 |
| Remote 错误不带事务 ID | 正确，登记 RR-09；每个失败事务独立包裹 ID，errors.Is 和多错误聚合保持 |
| Projected 成功后缀重放重复增长 | 是观测口径问题，不是重复写入证据。明确为成功投影尝试数，加重放计数回归；不新增无界去重表，也不以 committed == projected 证明数据正确 |
| WAL Replayed 漏计边界记录 | 正确，登记 RR-07；接收的最后记录让回调返回 nil，在下一条 lookahead 停下。真实文件 WAL 的记录数/字节边界、跨段和取消均回归 |
| Broadcast 冷目标静默失败 | 正确。逐目标捕获 Getter 错误/panic，记录 ID、handler、原因并继续后续独立目标；纳入 RR-06。异步准入成功仍不代表所有目标执行成功 |
| 快续行占 worker 时队列指标不一致 | 正确，而且准入也错误地多放行。登记 RR-08；准入、QueueLen、WaitingForWorker、PeakWaiting 共用实际占用数。OldestWaiting/PeakWaiting 是合并口径，等待累计耗时按前驱/worker 分开 |
| Close 后重连全部算 arrival | 当前语义如此。recovery 指同一 lifetime 上 Hold → Ready 的冷基线；Close → Open 是新 lifetime，没有恢复凭据来证明身份延续。不擅自新增跨会话恢复协议 |
| periodic 只配置字节预算时重复捕获 | 确认存在成本，但不是数据错误。本轮阻止字节插队，on_change 的阻塞窗口不再反复捕获；periodic 在编码前不知道当前精确大小，仍可能捕获全部候选。数量/每会话预算可限制捕获量；跨 tick 内容缓存另需版本与内存预算设计，本轮未宣称解决 |
| 空 ID Remote 批次重复归还写许可 | 正式 Nest 准备路径提前返回，复审也未给可达业务复现。本轮不为不可达分支引入流程改动 |

## 决策边界

Sync 冷预算是尝试成本：一旦开始 Push，一个会话本批计划的冷创建计费（包括 RetryLater、失败关闭、后续帧未发送），防止同窗口反复空转；从未尝试的会话、提前取消和编码失败不计费。实际交付数仍按成功帧计，成功前缀时钟/引用保留，未交付内容不结算。这个规则不是“每帧精确退款”。字节软预算允许窗口首个合法大对象独占；硬包/帧上限仍然拒绝，不切断实体包。

同会话从 Entity ID 轮转改为意图入队顺序，是公平性修正；已入队的同一快照意图刷新 Profile 不插到队尾，取消后重新订阅是新意图。未修改对外订阅 API。on_change 的字节阻塞标志由同一 flush 所有权保护，在预算窗口轮转时清除；已有对象更新仍可进行。

快池保护不是全局 I/O hook。内存命中、Guard/Entity 本地锁、回滚和 Finalize、既有锁内 WAL 准入及其持久策略是明确豁免；用户自写任意阻塞 RPC/新 goroutine 不会自动迁移或被拦截。自定义 Getter 仍须遵守 LoadedEntitiesOnly。带 loader 的冷缺失在快池 panic 后由 Nest 转为请求错误，保留 ErrColdLoadInLogic 和 ErrBlockingInFastWorker；无 loader 的 nil 占位语义保持。业务声明冷目标并使用 Slow option，由准备阶段加载。

## 验证与负对照

环境：macOS / Apple M5，Go 1.27.0，GOWORK=off，隔离 Mongo / Redis / NATS。没有重跑容量阶梯、30 分钟长稳或故障矩阵，不把历史 TPS/50ms 数据当作本版重测。

从 `git archive aaada47` 建立隔离基线，只拷贝可在旧 API 编译的回归测试，无生产代码替换。修前实际失败：

| 测试 | 修前失败摘要 | 修后 |
| --- | --- | --- |
| TestSlowHandlerRunLocalDoesNotWaitForOwnPool（1、2 worker） | Request 与 Shutdown 超时 | 通过 |
| TestLargeSnapshotFiniteBacklog / StarvedUnderChurn / SingleSession | 有限窗口内大对象未交付，含两模式、递增/随机 ID | 通过 |
| TestSnapshotRetryOnlyChargesAttemptedSessions | 首次 RetryLater 消耗全部会话额度，同窗口下一次无帧 | 通过：只扣首会话，随后交付另外两会话 |
| TestQueueStatsAndAdmissionCountRunningContinuation | 第二个等待请求意外准入，admission=nil | 通过：队列满拒绝、观测一致、排空归零 |
| TestReplayBudgetCountsConsumedRecordsAcrossSegments | records/bytes 两种边界均 consumed=2、replayed=1 | 通过：20 条各被统计一次，至少两个文件段 |
| TestRemoteProjectionFailureOnlyAcknowledgesPrefixAndReplaysSuffix | 错误缺少失败事务 ID | 通过，成功后缀仍需重放 |

RR-06 依赖新增执行标记 API，未伪造其旧版编译负对照；真实 ManagerAccess 的普通/Slow 快阶段、Get/GetMany、原始/脱离 context、Repository 在途 flight、Remote 等待入口以及单 worker 正常收尾已回归。

新回归还覆盖：on_change 同窗口重复通知不重新捕获被字节预算挡住的冷对象；下一窗口大对象和后续小对象均能排空；取消前/后 Push、第二会话 RetryLater、旧 lifetime 失效、编码硬错误；多帧成功前缀与未尝试会话额度；Stats/Audit 的 pending 清零。

首次扩大回归发现既有 TestRemoteProjectionFatalSuffixIsNotHiddenByEarlierTransientFailure 的时序假设不成立：首条先成功时第 4 条可能已经成功，ticket 为 nil 错误是合法结果。修正为首批全部返回错误，任意完成次序都不得启动下一条，同时断言未启动调用数为零、fatal 错误不会被 transient 掩盖。成功前缀与后缀重放由另一测试独立覆盖，没有改生产完成语义迁就测试。

实际通过命令（模块根目录）：

```sh
GOWORK=off go test -race ./fctx ./entity ./nest ./nestwal ./dataengine/... ./remoteentity ./sync/entitysync ./sync/frame ./kit/dataengine ./kit/nest ./kit/remoteentity ./metrics
GOWORK=off go test -race ./sync/...
GOWORK=off go test -race ./sync/entitysync -run 'TestSnapshot|TestLargeSnapshot|TestByteBlocked' -count=3
GOWORK=off go test -race ./nest ./dataengine/engine -run 'TestFast|TestSlowHandlerRunLocal|TestQueueStatsAndAdmission|TestRemoteProjectionFatalSuffix|TestRemoteProjectionFailureOnly|TestReplayBudgetCounts' -count=10
GOWORK=off go vet ./fctx ./entity ./nest ./nestwal ./dataengine/... ./remoteentity ./sync/... ./kit/dataengine ./kit/nest ./kit/remoteentity ./metrics
GOWORK=off go run ./cmd/glsvet ./fctx ./entity ./nest ./dataengine/engine ./remoteentity ./sync/entitysync
bash scripts/test-sync-modes-generated.sh
bash scripts/test-dataengine-generated.sh
bash scripts/test-remote-generated.sh
```

正式生成验收通过：Nest → 生成 DAO/Entity → Sync 双模式；DataEngine 三次独立进程的 async/strict/pipelined 持久化与恢复；Remote 三策略经过真实 Mongo/Redis/NATS。功能脚本中的压力测试按设计跳过，本轮没有性能结论。隔离环境准备见 [恢复指南](../feature/DATAENGINE-RECOVERY-2026-09-24.md)，不依赖某台机器的临时日志或凭据才能理解上述结果。

## 防止再出现

[roost-coding](../agent-skills/roost-coding/SKILL.md) 加入执行位置与请求快照分离、实际等待入口清单、one-worker/N-worker 自等待、真实 Getter/Repository 绕过路径、预算预留/尝试/成功三状态、持续到达公平性和观测口径规则。测试不只验证默认开关、固定积压或 mock 的理想路径；并行结果断言不得依赖 goroutine 完成顺序。仓库规则与本机 roost-coding / roost-optimize 已同步并通过 skill 结构检查。

图谱仍是 2026-09-25T11:41:37Z 的旧代际；本轮刷新再次被 `pre-coordination or unverified CBM generation is active` 阻止，未清理未知锁或停止其他实例。30 个本轮 Go 证据路径 coverage 均为 metadata_changed / not_tracked，已做定向查询并使用当前源码/编译/race 补证。无图谱完整性结论。代码与 docs 各自保持原目录，全部公开协议/持久格式保持；回退需整体撤回关联修复，不能只撤保护或只撤测试。
