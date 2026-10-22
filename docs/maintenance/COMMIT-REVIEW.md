# 新增提交审查

审查日期：2026-10-22。同步时 main 与 origin/main 均为 `13bc939e`；范围为上一轮审查提交 `a73c75cf..13bc939e`，413 个提交、无合并提交，净变化 1335 个文件，其中 Go / Go 模板 651 个路径。在独立 worktree 审查最终代码状态，没有逐个运行历史中间版本。上一轮报告见 [固定提交](https://github.com/tjbdwanghaibo/roost-core/blob/a73c75cf/docs/maintenance/COMMIT-REVIEW.md)。

## 确认缺陷

三个问题均通过临时探针复现，已登记到 [KNOWN-LIMITS](KNOWN-LIMITS.md)。状态：RV-20261022-01 **已修复**（`6edebb13`）；RV-20261022-02、RV-20261022-03 **未修复**。探针调用产品代码；运行后从包内删除，不提交一次性测试。后续修复应把行为回归并入现有测试文件。

### RV-20261022-01 / P1：批量执行绕过组内新增 Fence

**状态：已修复**（`6edebb13`）。每个 handler 开始前的检查收在 `NestMgr.checkHandlerStart`（fence + 取消），普通派发、广播与批量作业逐目标共用（可读性建议 1）；广播同样只在作业开始时检查一次，一并修复；同组先取锁失败、收尾时才转单目标的目标在 fence 后也交回 `ErrNestFenced`。两种触发（handler 主动 Fence、注入 committer 返回结果未知）的回归、红文本、变异与 benchstat 见 [v1.31.0 实现篇 H2](../release/v1.31.0-IMPLEMENTATION.md#h2)。

位置：`framework/nest/try_batch.go:398` 的 `runTryBatchLocked`，逐目标执行在 410–415 行。`prepareTryBatchGroupLocked` 在准备一组时读取 Fence；随后每个 `tryBatchExec.run` 没有再次核对。`invokeTryBatchTransaction` 可以因提交结果未知触发 Fence，因此该状态能在一组执行期间变化。

复现：单 worker、两个不同 ID 的内存 handler；第一个调用公开的 `NestMgr.Fence`，第二个记录调用次数。Shutdown 等待收尾后断言第二个没有执行且通过 HandBack 返回 `ErrNestFenced`。实际：

```text
handler after fence=1, second target handback=<nil>; want 0 and ErrNestFenced
```

Fence 允许已经执行的 handler 收尾，不允许尚未开始的目标继续执行业务。现有 `TestTryBatchRefusesTargetsAfterFence` 覆盖组准备前 Fence，没有覆盖组内变化。修复验收应包括组内主动 Fence 和真实 committer 返回结果未知两种触发；本轮探针只执行前者，后者是源码调用链判定。

### RV-20261022-02 / P2：旧重投被当成新一拍

位置：`gameplay/heartbeat/host.go:374` 的 `runBeat` 与 `driver.go` 的 `step`。重投和新拍携带相同的 `*Reg`；runBeat 只检查 `inflight` 并在返回时清零。

确定顺序：驱动投递原始拍 → 等待超过兜底阈值再投递旧拍 → 执行原始拍 → 驱动投递新拍 → 执行队列中的旧重投。实际：

```text
stale retry executed: calls=2 new beat inflight=false; want 1,true
```

旧重投执行额外回调并消费新拍的在途状态，新拍随后被当成 duplicate。探针使用包内 fake dispatch 控制交付顺序，但调度 step 与 runBeat 均为产品实现；没有声称跑了真实 Nest 排队压力。修复需按投递代际识别执行和交回，补旧重投、下一拍、停止重启以及异步 HandBack 的交错验证。

### RV-20261022-03 / P2：Drain 诊断越过锁边界读事实计数

位置：`framework/sync/entitysync/policy/interest_queue.go:665`。`drainStalls` 在 664 行释放 registry 锁后读取 `r.facts`；`queueFact` 在 90 行持 registry 锁更新该字段，不持 Interest 应用锁。

复现：ApplySync 模式，Show 后执行一次 applyStep 形成 applied-unsettled 状态，并发 QueueMove 与 drainStalls。`go test -race` 明确报告 665 行读和 90 行写冲突。普通已 settle 状态会短路该读取，不能用那个状态下的 race 通过否定问题。

影响是诊断数据竞争及不一致读取；没有证据证明此读会破坏 AOI 位置或订阅。修复在 registry 锁内捕获计数，再生成诊断，保持锁顺序。

## 可读性与设计建议

1. **Nest 批量执行的契约入口统一。** 批量与普通派发可以保留各自调度方式，但每个 handler 开始前的 Fence、取消、实体有效性检查应有明确统一的职责，避免准备阶段检查被误认为覆盖执行阶段。不为统一而添加新包或额外分配。**已按此实施**（`6edebb13`）：fence 与取消收在 `NestMgr.checkHandlerStart`；实体有效性依赖取到的实体，仍由各路径逐目标做（只读已加载 + Touch、取锁拒绝已摘除实例、锁组核对），契约写在同一处注释与 impl/02；没有新包，热路径不增分配。
2. **Heartbeat 显式表达投递身份。** 登记状态、投递代际、重试次数和正在执行状态含义不同；单一 inflight 不能表达全部。先明确状态转换和归还所有权，再整理调度、投递、观测职责到同包文件。
3. **Interest 显式区分锁内状态与锁外快照。** registry 记录事实、应用器更新空间、结算生成关系、Drain 读取诊断，各阶段用小函数表达；跨锁边界传值，不依赖读者记住某字段究竟由哪把锁保护。维持现有分层，不额外增加入口包装。
4. **源码注释描述现行规则。** movement、heartbeat 等新代码仍有 D20、决定编号、日期与过程叙述，应迁入设计文档；保留中文锁边界、失败语义、事务发布顺序。长函数按准备、执行、收尾拆分，不按历史优化轮次组织。
5. **MapBasic 的保存语义需更直观。** 单独 SetFacing 后 Save 不调用保存回调，探针已复现；但 Save、dirty 与使用文档明确限定为位置/场景变化，所以本轮不登记为确定 bug。建议明确“原地转向是否必须持久化”：若需要，dirty 应包含朝向并支持事务回滚；否则文档明确朝向仅随位置保存，避免 Save 回调包含 facing 造成误解。

方向判断：本轮两个执行时序缺陷都来自状态含义不够明确：批组已准备不等于每个 handler 已开始，登记在途不等于当前投递属于哪一拍。优先修正这些状态和检查边界，不继续增加时间阈值或重试分支。热路径可读性改函数体仍需遵守维护手册的同机交替至少 10 次 benchmark/benchstat 要求；本轮没有做性能改动或宣称性能收益。

## 验证与范围限制

已运行 `GOWORK=off go test ./... -count=1 -timeout=10m`，退出码 0（临时探针加入前启动，日志中没有探针测试）。受影响包现有 race 回归 `go test -race ./framework/nest ./gameplay/heartbeat ./framework/sync/entitysync/policy ./gameplay/movement -count=1 -timeout=4m` 退出码 0。三个新增探针分别按预期失败，说明现有全仓通过不能覆盖上述交错。

重点检查了 Nest TryBatch/Fence、Heartbeat 调度重投、Interest 单写应用/多视野/Drain、Sync 会话版本与交付结算、Movement 事务发布和保存，以及 DataEngine 投影/回放/checkpoint、Remote 数据库作用域、Saga 延迟确认相关改动。没有对全部 413 个提交逐个背书，也未逐个审计所有新增 service 功能、生成模板和 C# 客户端；没有执行真实 Mongo/Redis/NATS 故障矩阵、长稳压测、Linux 验证或性能 AB 对比。

CBM 项目为 `Users-whb-roost-roost-core`，当前可用索引代际落后于 HEAD。主动刷新被共享后台协调状态阻塞（worker exit=1），未重启共享服务。对 651 个变动 Go/模板路径做 coverage 检查后以当前源码与 diff 补证；没有把“无记录缺口”视为新代码完整索引。

本地原始日志、提交清单和探针源码备份保留在 `artifacts/review/20261022-main/`（忽略目录，探针后缀为 .txt，不参与 Go 包枚举）。文档记录结论与复现条件，不将本机证据路径当作其他环境已经完成的验证。
