# Service 第五轮：修复后审查收口（2026-09-29）

先按 roost-bugfix 修复 RR-23、RR-24、旧 Mail 删除残余，提交 `b336ce62` 已推送；随后按 roost-review 只写文档继续审新内容。**10 域主链的本阶段有界审查已收口**，当前确认 3 新问题与旧 Activity Opening 残余未实施。[完成矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md) · [实施交接](../bug/REVIEW-2026-09-29-services-05.md) · [机制学习](IMPLEMENTATION-SERVICE-BOUNDS-AND-RECOVERY-CLOSURE.md) · [可复跑证据](evidence/service-review-20260929-05/README.md)。

## 基线 / 同步 / 图谱

Core 单仓模块 github.com/tjbdwanghaibo/roost-core，初始 `66b58d90856b3c31f82b415a271f1450df1769f3`，fetch 无新增；fix 后 main 与 origin/main 为 `b336ce62f75ce0d763138bc8523c4695724214a2`。源码在隔离 detached worktree `D:/whb_s/.tmp/review-service-20260928` 审查，主仓 `D:/whb_s/cube-core` 同步提交。此轮新 review 不修改任何生产源码、正式测试或依赖。旧独立 kit/codegen 不参与当前 service 实现，集成生成器取 Core/codegen。

MCP roost-core root 是主仓，ready、30533 nodes/204289 edges；使用 Verify，search、trace inbound/outbound、snippet 定位，最后 100 生产路径与两个 scope 调用 check_index_coverage，generation `2026-09-29T05:34:58Z`，全部 metadata_changed、scope 无记录 gap。之前 full 刷新调用超时，新 generation 虽发布，节点 freshness 仍不足；当前进一步查询确认 ready，**不把 live Git HEAD 或 scope 干净当成节点完整/新鲜证明**。关键方法存在遗漏，demo 模板多是 File/Module 节点；相关实际源码补证，保留[原覆盖响应](evidence/service-review-20260929-05/COVERAGE.json)。

## 新范围与结论

继续覆盖 Account 待发布角色准入、Global 迁移重叠 acquire、Activity late opening/满容量 sweep、Match 公共分组与历史容量、Rank int64、Chat 年龄和有限去重、Session 分页/并发资源释放；复查 alias/options/Mod、split 使用的新 Attempts。正式 game-demo 的 Platform 发奖→Redis 购买凭证→drain/handler/Nest 与 Activity/Matchmaker/Battle 消费边界有源码证据及已有生成工程行为测试，不扩张为真实资产或房间服务验证。

新增 RR-25：两个 Grouping 跳过 Queue.Validate；RR-26：UpdateAdd 溢出成功落库；RR-27：Chat 非单调 StoredAt 的过期漏清理。旧 RR-20260914-02：Opening grace 回收后迟到 confirm 将窗口写成 257/256，末尾过期聚合被扫描排除。均已有可执行反例，建议与验收逐项记录，不新造包/存储体系。

Account pending 门禁与同计划恢复通过；Global 新租约不能被旧 Acquire 覆盖；Session 轮转来源清理每页、释放端幂等控制通过。源注释的 exactly/at most once 不能当作资源端 exactly-once 效果保证。Match 终态增长复用 09-17 观察，Chat 有限去重窗外允许重发符合契约；Account 封禁范围沿源码；CARRYOVER 已接受的活动贡献设计没有重开。

## 实际执行

Go 1.27.0 Windows amd64；race 可用；独立 Redis loopback 16395、无持久化。第四批修复已有 16 包 / 801 pass 事件、0 test fail/skip，两个新正式包 Memory/Redis、全仓编译、定向 vet、正式生成工程编译与 handler/game 行为测试通过，见 bugfix 原记录。

新 overlay 两种模式各 21 个叶子，**9 pass / 12 预期 fail / 0 skip**；总计 42 次执行 / 18 pass / 24 fail。失败不是新源码修复的回归失败，而是保持当前未修缺陷的安全断言。Rank 两种模式均 Redis；Account/Activity/Chat 随模式切换；Global/Session/Match 的 Memory 控制不能记作真实后端测试。没有 race detector 警报。初次 overlay 的 Session Finish 参数少 State 导致编译失败，已仅修复证据 fixture 后重跑两种模式，最终均无编译失败；没有计入最终通过数。

当前 RPC 9 个接口、12 次 -check（mail/match/session 两半），全部 exit 0，覆盖 18 个生成文件，不写生成产物。本轮没有实际生产 allocator、支付、NATS、Redis HA、强杀/断网、多节点或容量压测。

## 设计与性能

CAS 加显式持久意图的结构适合恢复业务流程；风险集中在跨键生命周期、外部效果证明和历史保留边界。Activity 准入数与扫描上限必须一起证明；Match Waiting 上限不能限制历史 Tickets/Requests，每次整 queueState CAS 成本随保留量增长。Chat Ring 有界，全扫描年龄的最坏成本可估，但修改 Gap 契约需要一起设计。Rank 是真正数值边界，不宜靠“游戏积分不可能很大”掩盖。

生成 demo PurchaseDrain 的 HGetAll 和按 OrderID 长期保留需要业务侧定义消费/归档窗口；Match.ID→房间分配、Session run/resource→释放、Mail token→奖励、Platform OrderID→购买入账都要求消费者的持久幂等。demo 中日志/进程内对象或空 grant 不承担生产恢复证明。沿现有 versionstore、Nest、稳定业务 ID、owner fence、显式 pending/Admin 设计交接，不引入另一套调度和存储机制。

停点：本阶段主链与文档完成，新增问题交接完成，未自动实施。后续入口是完成矩阵的具名专项与本批四项 bugfix；不是重复第一轮所有 service。
