# 第九批修复：NC-26～29 的 Mongo 替身边界

基线 `3d3b22c9f8398c032c6a2c7a574197dfe1bf59a2`；fetch/pull --ff-only 无 main 增量。按用户“修复，并继续 review”完成四个 P3；[NC-26](../bugfix/RR-20261004-NC-26.md)、[NC-27](../bugfix/RR-20261004-NC-27.md)、[NC-28](../bugfix/RR-20261004-NC-28.md)、[NC-29](../bugfix/RR-20261004-NC-29.md)已修、声明场景验证，未发版。

原 13 叶子在修改前实际 7 fail/6 控制通过，旧产品 overlay 可复跑相同红。最终新增 28 正式叶子通过：原反例，加深层 D、复合索引发布、非法 bulk 首中尾、正常 ordered 前缀，以及私有事务可见性/冲突/取消/panic 等。十包 race/vet：7 测试包、320 叶子 pass、0 fail/skip，3 包无测试；正式 DAO CLI 独立 module 的两个真实 Redis 消费者通过。

受影响 DataEngine/Kit/Service 首轮 672 pass/2 fail/17 skip：两个旧夹具依赖全库回滚。只更新必要调用链测试夹具，不改生产核心；保留原业务承诺并增加其他写者数据存活断言。最终 14 测试包、674 pass/17 环境 skip，race/vet 均退出 0；skip 未当通过。[原始事件、退出码与复跑入口](../bugfix/evidence/noncore-bugfix-20261004-09/README.md)。

事务采用集合粒度冲突、私有快照与固定锁序原子发布，成本增加且比真实 Mongo 保守；数组路径、完整索引语义、Drop/namespace 并发和未知 commit 不由本轮证明。纯替身修复无新的线上排障分支，T-206 保持。仓库与本机 coding/bugfix/optimize skill 内容一致，本轮未改技能。

MCP Tier 2 roost-core generation `2026-09-30T11:58:14Z` 落后源码；相关图查询无剩余分页，receiver 合并/跨包 heuristic 不作为真实调用边。coverage 的 metadata_changed/not_tracked 用当前源码、diff 和正式回归补证，没有宣称索引更新。[覆盖与清单](evidence/noncore-review-20261004-18/README.md)。

后续同轮继续[Redis 锁/续租/pubsub 审查](REVIEW-2026-10-04-noncore-18.md)，不因修复完就停止。N04 仍为场景部分完成，真实 Mongo/Cluster/HA/长期容量留项；[进度](PROGRESS.md) · [机制](IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md)。
