# Roost 持续 Review 与学习记录

每轮先获取 core、kit、codegen 最新代码，再审查和记录，默认不修改代码。
长期流程由个人 skill `roost-review` 执行，可用 `$roost-review` 或“继续 Roost review”调用。

- [跨轮审查进度](PROGRESS.md)：源码基线、已验证场景、限制和下轮入口。
- [实现学习：锁与生命周期](IMPLEMENTATION-STATE-AND-LIFECYCLE.md)。
- [实现学习：领取、匹配重放与生成消费者](IMPLEMENTATION-SERVICE-REPLAY-AND-CODEGEN.md)。

| 日期 | 范围 | 结论 | 文档 |
| --- | --- | --- | --- |
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
