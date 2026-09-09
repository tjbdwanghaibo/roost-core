# Roost Review 跨轮进度

最后更新：2026-09-09 第四轮。状态描述证据深度，不表示整个包已审完，不使用覆盖百分比。

## 当前源码基线

- Core：`1f7bb5425a8b82c774bac9bbf40048f44ea3de99`。
- Kit：`c4e7cef1029fb6326fa88b84c622f059bc62c0c4`。
- Codegen：`aa072edb35a0b97d234e0155142e36295c5f21d1`。

以下“第四轮”指以上对应仓库 SHA；历史行以链接运行记录中的 SHA 为准，不冒充最新代码验收。

| 模块/路径 | 最近审查 | 入口与不变量 | 实际验证与问题 | 状态/限制 | 下一入口 |
| --- | --- | --- | --- | --- | --- |
| core/entity/entity_guard.go | 第四轮 Core | Acquire/Release，新增组锁必须保持顺序 | entity race 包测试通过 | 已验证部分场景；未压测多服锁竞争 | guard 跨事务释放 |
| core/nest/cast.go、msg.go、group_transition.go、nest_dispatch.go | 第四轮 Core | CastMulti；远程实体必须预声明，失败仅回滚本次锁 | nest race 包测试通过 | 已验证部分场景；未做真实远程故障注入 | 预分派批量远程锁失败 |
| core/dataengine/engine/assembly.go、runtime.go | 第四轮 Core | Shutdown 未完成保留重试入口；分阶段关闭 | 原 RR-20260909-03 overlay 与包 race 通过 | 已验证部分场景；未跑完整部署 | 各组件独立失败/重启恢复 |
| kit/service/session/service.go | 第四轮 Kit | 冲突清理先释放旧 claim，后丢弃会话 | 原 RR-20260909-02 overlay 与包 race 通过 | 已验证部分场景；真实后端未验 | 超时后重试及租约续期 |
| kit/service/mail/service.go、mailbox.go | 第四轮 Kit | Reserve/Commit/Cancel；稳定 token 与领取状态 | mail race 包测试通过，源码阅读领取链 | 已验证部分场景；奖励服务原子发放未验 | grant receipt 与跨服重试 |
| kit/service/match/queue_store.go、store.go、match_rpc.go | 第四轮 Kit | Enqueue/Ticket/Cancel/Commit；队列 CAS 与归属 | 包 race 通过；独立重放测试失败 RR-20260909-05 | 已验证部分场景；无真实 Redis/吞吐证据 | 修复验收、过期请求清理 |
| codegen/internal/entity/main.go、gen.go | 第四轮 Codegen | 多实体默认输出与显式 -output | 默认消费者测试通过；显式输出消费者编译失败 RR-20260909-06 | 已验证部分场景；不是全部模板组合 | 显式输出修复、混合实体 |
| codegen/internal/nest | 第四轮 Codegen | 相关回归包 | race 包测试通过 | 待复核；本轮未展开完整模板链 | handler 参数到消费者 |
| core/cache、versionstore | [第二轮](REVIEW-2026-09-09-02.md) | 等待取消与名额归还 | 原问题复现变绿 | 已验证部分场景；本轮未重审 | 后端失败与饥饿 |
| core/app、worker、entitysync、nettransport、lockstep | [架构轮](REVIEW-2026-09-09.md) | 生命周期、传播水位、输入边界 | 详见历史运行记录 | 待复核；历史证据不代表当前基线 | 真实装配后的停机与传播 |
| core/saga | [第三轮](REVIEW-2026-09-09-03.md) | 生命周期与补偿相关边界 | 相关 race 测试见历史记录 | 待复核；未覆盖全部补偿组合 | step replay 与 receipt |
| codegen consolidate、发布消费者 | [第二轮](REVIEW-2026-09-09-02.md) / [架构轮](REVIEW-2026-09-09.md) | import 拆分；正式 tag 接入 | 原 consolidate 复现通过；pure-tag 受网络限制 | 待复核 | 隔离正式 tag 消费者 |
| kit 其余服务与 core/skill 深层执行路径 | 本轮未审 | 尚未建立本轮有界机制证据 | 不以历史修复账本代替 review | 未审（本轮） | 优先 room/remoteentity，再 skill |

## 下轮顺序

1. fetch 三仓，从本页 SHA 做增量；先核验 bug/bugfix 新记录及 RR-05、06。
2. 展开 Match 过期清理、Mail 发奖回执与 Remote Entity 预分派，补足本轮边界。
3. 轮转 Room/Skill，保持每轮新增机制阅读；不要只重复旧复现。
4. 维护本表、主题实现文档、每轮记录与问题索引，再提交推送。

本轮完整证据：[第四轮运行记录](REVIEW-2026-09-09-04.md)。
