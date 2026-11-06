# v1.41～v1.42 新功能审查（2026-11-06）

基线：[main / v1.42.0](https://github.com/tjbdwanghaibo/roost-core/commit/a6235227)，对比 [v1.40.1](https://github.com/tjbdwanghaibo/roost-core/commit/b3e9ea69)。审查覆盖 Remote 独占写准入、best_effort effects、outbox 发布与并发、跨 Gate 鉴权、路由 RPC、镜像同步及 cellworld 交接的选定路径；不代表 542 个变更文件全部完成穷尽审计。索引工具不可用，依据源码、差异及本机 Linux 验证。

**结论（含续审）：确认三项缺陷——失败创建事件逃逸、旧 Leave 删除新实例、BaseState 编码丢字段；cellworld 完整包另有一次未闭环失败。不能宣布零 bug 或上线验收全部完成。** 本分支仅提交报告与复现证据，不改 main 运行时。

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

## 续审：跨区生命周期与状态编码

再次 fetch 后 main 仍是本报告同一 v1.42.0 基线；以下是本轮新增发现，不重复登记前述问题。

### RV-20261106-03 / P2：旧 Leave 会删除重新入场的新实例

位置：[leaveReq](https://github.com/tjbdwanghaibo/roost-core/blob/a6235227/gameplay/cellworld/wire.go#L208)、[onLeave](https://github.com/tjbdwanghaibo/roost-core/blob/a6235227/gameplay/cellworld/node.go#L1037)。

触发：同一 SceneActor 入场 → Leave 完成且旧实例销毁 → 以同 ID 再入场（epoch 已递增）→ 旧 Leave 的迟到副本到达。当前消息只有 ID，onLeave 只检查 RoleReal，没有生命周期身份检查，因此直接销毁新的 Real。Transport 的契约明确允许重复、乱序，不能依赖传输永远按业务生命周期有序。

使用现有 loopback、多 Node、EntityManager 与真实 Nest handler 捕获第一次真实 Leave 的编码消息；等待销毁，再等待新实例的 witness 确认 epoch 1 → 2，最后只回放旧消息。race 连续三轮均失败：present=false。见 [探针](evidence/feature-review-20261106/review_late_leave_test.go.txt)、[三轮证据](evidence/feature-review-20261106/late-leave.log)。控制组仅取消回放，三轮均存活；不是通过延长等待掩盖故障。

玩法后果：重新入场的玩家被旧离场请求踢出空间世界，视野中断，并可能由看门狗再次重建；本轮未证明永久无法恢复或持久资产损失。此问题位于 cellworld 原型，不泛化为所有 Remote 实体生命周期均有同样问题。

建议增加独立的入场生命周期身份，随 Enter、Leave 及跨格交接保留，并在删除前校验；同时核对 Base 来源。不要简单认为每次迁移变化的 owner epoch 等价于玩家的一次入场生命周期。回归应覆盖 Leave 重复／乱序、离场后同 ID 重入、交接期间离场，以及无旧消息的正常对照。

### RV-20261106-04 / P3：BaseState 的重复 MsgPack 标签丢失 X 与 VX

位置：[BaseState 字段声明](https://github.com/tjbdwanghaibo/roost-core/blob/a6235227/gameplay/cellworld/base.go#L33)。`X, Y` 共享标签 `x`，`VX, VY` 共享标签 `v`。Go 组合字段声明会把同一 tag 应用于两个字段，MsgPack 因而发生键冲突。

以 X=1200、Y=3400、VX=100、VY=-200 做 Marshal/Unmarshal，编码器记录重复字段，结果 X=0、VX=0，Y/VY 保留。见 [探针](evidence/feature-review-20261106/review_base_codec_test.go.txt)、[失败日志](evidence/feature-review-20261106/base-codec.log)。overlay 对照仅将四字段拆开并使用独立标签，两字段值不再丢失。

影响边界：接入方直接用 MsgPack 保存／传递公开 BaseState 时会损坏坐标与速度；当前 demo 逐字段映射到 PlayerDao，本轮未发现它使用该 MsgPack 路径，因此不宣称当前 demo 已发生位置持久化损坏，也不影响标签正确的 EntityState 网络消息。建议独立标签并加往返测试；如已有消费者保存旧格式，需明确旧数据恢复／迁移边界。

### 本轮复跑

将两个 `.go.txt` 探针复制到目标基线的 gameplay/cellworld 对应 `_test.go` 文件，不能覆盖已有文件。原样执行下列命令均预期退出 1，验证完成后移除临时探针：

```bash
go test -race ./gameplay/cellworld -run '^TestReviewOldLeaveMustNotDeleteReenteredActor$' -count=3 -v
go test -race ./gameplay/cellworld -run '^TestReviewBaseStateMsgpackRoundTrip$' -count=1 -v
```

[控制组日志](evidence/feature-review-20261106/cell-controls.log) 中，旧 Leave 用例只取消旧消息回放；编码用例将 X/Y/VX/VY 改为四个独立标签。两个控制组各三轮通过。取消回放仅用于证明触发因素，不是 Leave 问题的修复。未提交运行时代码修改。

现有生命周期六个用例额外原样 race 通过（崩溃重入、Base 消失租约过期、回写有序幂等、交接回滚、Node 单绑定、入站关闭屏障），见 [日志](evidence/feature-review-20261106/cell-lifecycle.log)：

```bash
go test -race ./gameplay/cellworld -run '^Test(CellCrashPlayerReentersFromSnapshot|BaseGoneSceneActorExpires|WritebackIsIdempotentAndOrdered|HandoffRollbackDoesNotHandOver|HandlersBindToExactlyOneNode|CloseIngressStopsTakingMessagesAndWaitsForTheOneInFlight)$' -count=1 -v
```

选定六用例通过不覆盖新增旧 Leave 重放场景。本轮未重跑完整包、跨进程 NATS 或压测；上一轮完整包边界抖动失败没有因此关闭。
