# v1.41～v1.42 新功能审查（2026-11-06）

基线：[main / v1.42.0](https://github.com/tjbdwanghaibo/roost-core/commit/a6235227)，对比 [v1.40.1](https://github.com/tjbdwanghaibo/roost-core/commit/b3e9ea69)。审查覆盖 Remote 独占写准入、best_effort effects、outbox 发布与并发、跨 Gate 鉴权、路由 RPC、镜像同步及 cellworld 交接的选定路径；不代表 542 个变更文件全部完成穷尽审计。索引工具不可用，依据源码、差异及本机 Linux 验证。

**结论：确认一项新增事务缺陷；cellworld 完整包存在一次未闭环失败。不能宣布零 bug 或上线验收全部完成。** 本分支仅提交报告与复现证据，不改 main 运行时。

## RV-20261106-01 / P2：失败创建中的 best_effort 事件逃逸

位置：[createdBuildMark](https://github.com/tjbdwanghaibo/roost-core/blob/a6235227/framework/nest/rollback.go#L1069)、[失败创建检查](https://github.com/tjbdwanghaibo/roost-core/blob/a6235227/framework/nest/rollback.go#L1148)。

实体初始化通过 Emit 登记 best_effort 事件，随后创建失败；外层持久 handler 处理创建错误并正常返回时，事件仍发布，尽管 EntityManager 中没有该实体。游戏中可表现为创建副本／队伍失败，却向其他服务或玩家发出了成功通知。

原因：新 delivery 模式写入独立的 tx.bestEffort；createdBuildMark 只记录 tx.effects，abandonCreatedEntityBuild 也只检查后者。失败创建守卫因此没有设置 ErrFailedCreateLeftEffects，事务随后正常调度 best_effort。best_effort 允许有效提交后的消息丢失，不应允许已拒绝创建产生成功事件。

最小探针使用仓库现有真实失败创建夹具与 Nest handler，以 reliable 为对照；原样运行 reliable 通过、best_effort 失败，观察到 error=nil、published 非空、entityVisible=false。见 [探针](evidence/feature-review-20261106/review_failed_create_best_effort_test.go.txt)、[红色证据](evidence/feature-review-20261106/failed-create.log)。

建议将 bestEffort 纳入创建前标记与失败创建副作用检查，沿用已有整体回滚语义。仅以 Go overlay 增加该字段、长度快照与比较后，原探针两组均通过，见 [定位对照](evidence/feature-review-20261106/failed-create-control.log)。这是根因定位对照，未作为正式修复交付；修复方还应补不同创建失败阶段及异步／流水线持久模式回归。

复跑：把探针复制到目标 main 的 framework/nest/review_failed_create_best_effort_test.go（勿覆盖已有文件），执行：

```bash
go test -race ./framework/nest -run '^TestReviewFailedCreateBestEffortEscapes$' -count=1 -v
```

验证完成后移除该临时探针；它用于证明当前缺陷，当前 main 上预期退出 1。

## RV-20261106-02：cellworld 跨区抖动验收不稳定（根因待定）

完整包 race 中 TestWorldJitterAtBoundary 失败：最终 owner sid=101、epoch=13，但旧格 (1,0) 仍报告 frozen。独立重跑同用例五轮全部通过。见 [完整包输出](evidence/feature-review-20261106/targeted.log)、[五轮复测](evidence/feature-review-20261106/cellworld-jitter.log)。

[scenarioJitter](https://github.com/tjbdwanghaibo/roost-core/blob/a6235227/gameplay/cellworld/world_test.go#L415) 停止输入后仅 sleep 三个 worldPeriod 就检查没有 Frozen；目标接收、owner 更新与旧格收到 OfferReply／销毁并非同一时刻。现有证据尚不能区分夹具等待不足和运行时交接收敛异常，不能称为永久冻结、双 Real 或丢失实体。

建议以有期限的交接完成条件代替固定 sleep，超时输出所有格角色、epoch、未完成 OfferReply 和出站队列；再跑完整包及边界抖动重复验证。不要只放宽连续性断言，也不要用五轮独立通过覆盖完整包的失败。

## 新契约是否合理

- Remote 独占写减少每次请求的权威查询，在单 owner 进程、fail-stop、旧 owner 停止后交接的明确前提下成立；共享路径仍用锁与 GrantWrite。不能把此优化宣传为任意重叠 owner 进程下仍逐请求 fencing。
- best_effort 与可靠 outbox 分离、提交后发布的总体设计合理；发现的问题在于未同步更新失败创建保护。
- effect 消费按 key 保持到达顺序，并不承诺无共享 mutation 的独立事务按 WAL 顺序完成 effect 投递；不要把默认并发消费者当作全局业务排序器。
- outbox writer_sid 与宽限期减少慢 finalize 被抢占；仍须遵守文档的 SID 身份及时间偏差前提。
- cellworld 文档标记为原型；本次结果不支持直接提升为已完成生产跨区验收。

## 验证范围

Linux / Go 1.27.2 / GOMAXPROCS=4，均未启动压测。新增失败创建探针在基础回归的 Nest 包完成之后加入，因此基础回归不含该预期失败探针。

```bash
go test -race -p 1 ./framework/remoteentity ./framework/nest ./framework/dataengine/... \
  ./framework/entity ./framework/sync/entitysync/policy ./infra/base/worker \
  ./infra/network/gateway ./wiring/dataengine ./wiring/remoteentity ./gameplay/cellworld -count=1
go test -race ./gameplay/cellworld -run '^TestWorldJitterAtBoundary$' -count=5 -v
```

首条命令前 10 个包通过，cellworld 失败，整体退出 1；第二条退出 0。没有把包级通过解释为真实 Mongo／etcd／Redis 故障、Unity 客户端及所有部署组合均通过；本轮未重跑全仓与这些外部验收。

路由 RPC 补验 `go test -race ./infra/network/servicerpc -count=1` 通过，见 [日志](evidence/feature-review-20261106/servicerpc.log)。

## 历史未关闭项：Lockstep 最新 main 仍失败

本轮在同一最新基线原样执行：

```bash
go test -race ./framework/sync/lockstep -run '^TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive$' -count=1 -v
```

退出 1：deep-buffered_link/written_lane_follows_the_link 在 67.35 秒后报告 closed=true、Tick errors=0；直连和 admission-only 负对照通过。见 [本轮日志](evidence/feature-review-20261106/lockstep.log)。测试文件从 v1.40.1 到本轮基线没有变更。这不是引用修复前的 90 秒记录，当前代码已包含真实心跳与 2 秒空闲期限。

此前缓冲对照及定位限制仍见 [v1.40.1 复验](FOLLOWUP-REVIEW-20261102.md)。仍应优先诊断 Linux 测试链路的接收窗口与断开原因；当前证据不能直接证明生产 Lockstep 丢快照，也不能关闭 Linux 验收问题。
