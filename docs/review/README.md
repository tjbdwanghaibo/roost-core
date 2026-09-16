# Roost 持续 Review 与学习记录

[09-16 第四轮验收](REVIEW-2026-09-16-04.md) · [统一实施交接（七项验收、两项新问题与 Kit 分层）](../bug/REVIEW-2026-09-16-04.md)。

[09-16 第三轮真实 Broker](REVIEW-2026-09-16-03.md) · [本地扇出新问题](../bug/REVIEW-2026-09-16-03.md) · [35 场景完整代码](../bug/REPRO-2026-09-16-03.md) · [机制补充](IMPLEMENTATION-JETSTREAM-SYNCBUS-AND-LIFECYCLE.md)。

[09-16 第二轮 JetStream/NATS](REVIEW-2026-09-16-02.md) · [确认与生命周期机制](IMPLEMENTATION-JETSTREAM-SYNCBUS-AND-LIFECYCLE.md) · [新问题及观察](../bug/REVIEW-2026-09-16-02.md) · [32 场景代码](../bug/REPRO-2026-09-16-02.md)。

[09-16 Journal 故障与 SyncBus](REVIEW-2026-09-16.md) · [Patch/Delivery 机制](IMPLEMENTATION-SYNCBUS-PATCH-AND-DELIVERY.md) · [新问题](../bug/REVIEW-2026-09-16.md) · [28 场景](../bug/REPRO-2026-09-16.md)。

[09-15 第八轮快照交接](REVIEW-2026-09-15-08.md) · [两个新问题](../bug/REVIEW-2026-09-15-08.md) · [29 场景代码](../bug/REPRO-2026-09-15-08.md)。

[09-15 第七轮 History/Journal](REVIEW-2026-09-15-07.md) · [机制](IMPLEMENTATION-SYNCSTREAM-HISTORY-AND-JOURNAL.md) · [两个新问题](../bug/REVIEW-2026-09-15-07.md) · [20 场景](../bug/REPRO-2026-09-15-07.md)。

[09-15 第六轮生命周期与分片](REVIEW-2026-09-15-06.md) · [SyncStream 机制](IMPLEMENTATION-SYNCSTREAM-FRAGMENTS-AND-DELIVERY.md) · [新问题](../bug/REVIEW-2026-09-15-06.md) · [24 场景](../bug/REPRO-2026-09-15-06.md)。

[09-15 第五轮 Room 传输审查](REVIEW-2026-09-15-05.md) · [准入与剔除机制](IMPLEMENTATION-ROOM-ADMISSION-AND-EVICTION.md) · [新问题](../bug/REVIEW-2026-09-15-05.md) · [21 场景代码](../bug/REPRO-2026-09-15-05.md)。

[09-15 第四轮失败与生命周期](REVIEW-2026-09-15-04.md) · [观察项](../bug/REVIEW-2026-09-15-04.md) · [17 场景代码](../bug/REPRO-2026-09-15-04.md)。

[09-15 第三轮 EntitySync](REVIEW-2026-09-15-03.md) · [订阅与持久化机制](IMPLEMENTATION-ENTITYSYNC-SUBSCRIPTION-AND-DURABILITY.md) · [问题](../bug/REVIEW-2026-09-15-03.md) · [复现](../bug/REPRO-2026-09-15-03.md)。

[09-15 第二轮旧修复验收](REVIEW-2026-09-15-02.md) · [重叠准备新问题](../bug/REVIEW-2026-09-15-02.md) · [复现](../bug/REPRO-2026-09-15-02.md)。

[09-15 生命周期与容量边界](REVIEW-2026-09-15.md) · [问题](../bug/REVIEW-2026-09-15.md) · [复现](../bug/REPRO-2026-09-15.md)。

[第八轮 LOD、历史与提交交接](REVIEW-2026-09-14-08.md) · [新问题](../bug/REVIEW-2026-09-14-08.md) · [独立复现](../bug/REPRO-2026-09-14-08.md)。

[第七轮：恢复屏障与分片重组](REVIEW-2026-09-14-07.md) · [两项新问题](../bug/REVIEW-2026-09-14-07.md) · [复现](../bug/REPRO-2026-09-14-07.md)。

[第六轮](REVIEW-2026-09-14-06.md) · [StateSync 机制](IMPLEMENTATION-STATESYNC-BASELINE-AND-ACK.md) · [U-0199 漏审复盘](POSTMORTEM-LOCKSTEP-U0199.md)。

[09-14 第五轮](REVIEW-2026-09-14-05.md)：去重环验收、真实 TCP 追帧、新 Bot 跳帧问题 · [问题](../bug/REVIEW-2026-09-14-05.md) · [复现](../bug/REPRO-2026-09-14-05.md)。

[09-14 第四轮 lockstep 续审](REVIEW-2026-09-14-04.md) · [内存边界问题](../bug/REVIEW-2026-09-14-04.md) · [复现](../bug/REPRO-2026-09-14-04.md) · [lockstep / sync 覆盖与优先级](LOCKSTEP-AND-SYNC-COVERAGE.md)。

[09-14 第三轮 lockstep](REVIEW-2026-09-14-03.md) · [四项问题](../bug/REVIEW-2026-09-14-03.md) · [复现](../bug/REPRO-2026-09-14-03.md) · [输入与追帧机制](IMPLEMENTATION-LOCKSTEP-INPUT-AND-CATCHUP.md)。

[09-14 第二轮](REVIEW-2026-09-14-02.md)：WAL 修复验收、Activity 两个 P2 · [问题](../bug/REVIEW-2026-09-14-02.md) · [复现](../bug/REPRO-2026-09-14-02.md) · [Activity 机制](IMPLEMENTATION-ACTIVITY-WINDOW-AND-DISPATCH.md)。累计 26 轮、39 RR，详见 [进度](PROGRESS.md)。

[09-14 运行](REVIEW-2026-09-14.md) · [问题](../bug/REVIEW-2026-09-14.md) · [复现](../bug/REPRO-2026-09-14.md) · [未收敛工作表](OPEN-QUESTIONS.md)。

[进度统计](PROGRESS-SNAPSHOT-2026-09-13.md)：24 轮、36 RR；[第十轮运行](REVIEW-2026-09-13-10.md)、[问题](../bug/REVIEW-2026-09-13-10.md)。

[第九轮运行](REVIEW-2026-09-13-09.md) · [问题](../bug/REVIEW-2026-09-13-09.md) · [复现](../bug/REPRO-2026-09-13-09.md)。

[第八轮运行](REVIEW-2026-09-13-08.md) · [验收与复跑](../bug/REVIEW-2026-09-13-08.md) · [修复机制学习](IMPLEMENTATION-REPAIR-ACCEPTANCE.md)。

[第七轮运行](REVIEW-2026-09-13-07.md) · [问题](../bug/REVIEW-2026-09-13-07.md) · [复现](../bug/REPRO-2026-09-13-07.md)。

[09-13 第六轮运行](REVIEW-2026-09-13-06.md) · [生命周期机制](IMPLEMENTATION-MIRROR-LIFECYCLE.md) · [测试源码](../bug/REVIEW-2026-09-13-06.md)。

[09-13 第五轮：Mirror 实施就绪审查](REVIEW-2026-09-13-05.md) · [实施交接与六步验收](PLAN-REMOTE-POLICY-MIRROR.md)。

每轮先获取 core、kit、codegen 最新代码，再审查和记录，默认不修改代码。
长期流程由个人 skill `roost-review` 执行，可用 `$roost-review` 或“继续 Roost review”调用。

- [跨轮审查进度](PROGRESS.md)：源码基线、已验证场景、限制和下轮入口。
- [实现学习：锁与生命周期](IMPLEMENTATION-STATE-AND-LIFECYCLE.md)。
- [实现学习：领取、匹配重放与生成消费者](IMPLEMENTATION-SERVICE-REPLAY-AND-CODEGEN.md)。

| 日期 | 范围 | 结论 | 文档 |
| --- | --- | --- | --- |
| 2026-09-13 第四轮 | 六项修复、真实 Redis 上限与 expiry 返回路径 | 五项验收通过，一项部分修复 | [运行](REVIEW-2026-09-13-04.md)、[验收](../bug/REVIEW-2026-09-13-04.md)、[复现](../bug/REPRO-2026-09-13-04.md) |
| 2026-09-13 第三轮 | 真实 Redis/Lua、owner 转移回复丢失、计数域 | 新 P2/P3×2；旧两项真实补证；十个场景 | [运行](REVIEW-2026-09-13-03.md)、[实现](IMPLEMENTATION-REMOTE-REDIS-UNCERTAIN-OUTCOMES.md)、[问题](../bug/REVIEW-2026-09-13-03.md)、[复现](../bug/REPRO-2026-09-13-03.md) |
| 2026-09-13 第二轮 | L1/L2 冲突、回填交错、绝对过期与加载恢复 | 四项新 P2，八个临时场景，三包 race 通过 | [运行](REVIEW-2026-09-13-02.md)、[实现](IMPLEMENTATION-REMOTE-L1-L2-CONSISTENCY.md)、[问题](../bug/REVIEW-2026-09-13-02.md)、[复现](../bug/REPRO-2026-09-13-02.md) |
| 2026-09-13 | Remote 接收删除/兴趣/身份、加载取消、总线语义 | 四项新 P2；七个临时场景；七包 race 基线通过 | [运行](REVIEW-2026-09-13.md)、[实现](IMPLEMENTATION-REMOTE-REPLICA-ORDERING-AND-RECOVERY.md)、[问题](../bug/REVIEW-2026-09-13.md)、[复现](../bug/REPRO-2026-09-13.md) |
| 2026-09-12 第二轮 | RemotePolicy Mirror 与已有缓存/副本工具 | 尚无完整只读运行时；原语验证与复用方案完成 | [运行](REVIEW-2026-09-12-02.md)、[实现及方案](IMPLEMENTATION-REMOTE-POLICY-MIRROR.md)、[观察](../bug/REVIEW-2026-09-12-02.md) |
| 2026-09-12 | 饱和回退子进程；真实 WAL 释放、Ack/重开、Shutdown/Sync | 新两个 P2；旧 panic 崩溃风险实测确认；三个正向 WAL 场景通过 | [运行](REVIEW-2026-09-12.md)、[实现](IMPLEMENTATION-WAL-ADMISSION-DURABILITY-AND-SHUTDOWN.md)、[问题](../bug/REVIEW-2026-09-12.md) |
| 2026-09-11 第四轮 | completion 慢确认、回调 panic、Shutdown 重试 | 新 P2；慢完成重试通过；旧 Mail P3 仍复现 | [运行](REVIEW-2026-09-11-04.md)、[实现](IMPLEMENTATION-COMPLETION-FAILURE-AND-SHUTDOWN.md)、[问题](../bug/REVIEW-2026-09-11-04.md) |
| 2026-09-11 第三轮 | 四项修复、Remote/Nest 停机交错、Mail 拒绝原子性 | 四项验收通过；新 P3 一项 | [运行](REVIEW-2026-09-11-03.md)、[实现](IMPLEMENTATION-MAIL-RETENTION-AND-ATOMIC-REFUSAL.md)、[问题](../bug/REVIEW-2026-09-11-03.md) |
| 2026-09-11 第二轮 | Remote / Nest 停机与性能 | 新两个 P2 已复现 | [运行](REVIEW-2026-09-11-02.md)、[实现](IMPLEMENTATION-REMOTE-NEST-LIFECYCLE-AND-PERFORMANCE.md)、[问题](../bug/REVIEW-2026-09-11-02.md) |
| 2026-09-11 | 八项修复独立验收；Mail 墓碑、事务 TTL | 八项原触发通过；新 Mail P2/P3 各一项 | [运行](REVIEW-2026-09-11.md)、[问题](../bug/REVIEW-2026-09-11.md)、[复现](../bug/REPRO-2026-09-11.md) |
| 2026-09-10 第三轮 | M-01～M-05、Remote 等待者、Skill 恢复、Mail 重试 | 新 3 P2 / 1 P3；含两个新增生成消费者编译缺陷 | [运行](REVIEW-2026-09-10-03.md)、[注册生成实现](IMPLEMENTATION-CATEGORY-REGISTRY-AND-GENERATION.md)、[恢复重放实现](IMPLEMENTATION-CHECKPOINT-AND-REPLAY.md)、[问题](../bug/REVIEW-2026-09-10-03.md) |
| 2026-09-10 第二轮 | Remote 预分派、Mail 淘汰与重投 | 3 包 race 通过；新 P2：淘汰后重投生成新发奖 token | [运行](REVIEW-2026-09-10-02.md)、[实现](IMPLEMENTATION-REMOTE-PREPARE-AND-FINALIZE.md)、[问题](../bug/REVIEW-2026-09-10-02.md) |
| 2026-09-10 | Match 过期、Room 生命周期、Skill 取消 | 3 包 race 通过，2 个新增行为验收通过；1 个低频 P3 | [运行记录](REVIEW-2026-09-10.md)、[实现学习](IMPLEMENTATION-ROOM-AND-SKILL-SCHEDULING.md)、[问题](../bug/REVIEW-2026-09-10.md) |
| 2026-09-09 第四轮 | 三项修复验收；Guard/Cast、Mail、Match、Entity 输出 | 8 个相关包 race 通过；新增 2 个 P2；建立进度与机制文档 | [学习与验证](REVIEW-2026-09-09-04.md)、[问题](../bug/REVIEW-2026-09-09-04.md) |
| 2026-09-09 第三轮 | Data Engine/Saga 生命周期、Entity 消费者生成 | 新增 2 个 P2；相关基线测试通过，独立复现失败 | [学习与验证](REVIEW-2026-09-09-03.md)、[问题](../bug/REVIEW-2026-09-09-03.md) |
| 2026-09-09 第二轮 | 优先核验用户 bugfix 四项修复 | 原 3 条复现变绿；新增 Session claim ABA；文档版本仍有收尾 | [修复验收与学习](REVIEW-2026-09-09-02.md)、[问题](../bug/REVIEW-2026-09-09-02.md) |
| 2026-09-09 | 最新三仓架构、消费者生成、首轮问题复核 | 旧 3 个 P2 仍复现；新增接入文档 P2；源码消费者编译通过，pure-tag 验证受网络限制 | [评估/学习/验证](REVIEW-2026-09-09.md)、[问题](../bug/REVIEW-2026-09-09.md) |
| 2026-09-08 | core cache/versionstore；kit session；codegen consolidate | 3 个 P2 已复现、未修复，图谱补证待完成 | [学习/验证](REVIEW-2026-09-08.md)、[问题](../bug/REVIEW-2026-09-08.md) |

下一轮从运行记录的完整 SHA 做增量比较，再轮转未覆盖范围。关闭问题必须有独立修复
与验证依据，包测试成功不能代表全仓审计完成。
