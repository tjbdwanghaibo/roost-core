# Saga 学习：消费者健康与跨持久化恢复身份

2026-10-05；[运行与进度](REVIEW-2026-10-05-noncore-25.md)，[NC-37](../bugfix/RR-20261005-NC-37.md)、[NC-38](../bugfix/RR-20261005-NC-38.md)。

## 生命周期要把必需消费者列全

Saga 协调器有三个正式消费者，分别接受普通完成、Nest启动意图、原生Nest事务完成effect。Engine循环还活着并不等于能处理每类业务；原生消费者关闭后，其他两个消费者和Mongo仍可正常响应。健康状态必须同时检查三个资源，不能只检查循环运行或依赖Ping。

Assembly拥有订阅、协调循环和停止责任，Kit拥有配置解析、注册能力及health入口。Start部分失败需释放已创建订阅，Stop先Drain全部三个，再取消循环；Drain超时强停订阅并保留可再次收尾的引用。本批实际测试第三个Drain被钉住、取消后再次Stop完成，而不是只看第一次错误。

## 运行状态与持久表示必须一起检查

| 身份 | 用途 | 恢复时要求 |
| --- | --- | --- |
| BusinessKey | 启动业务去重 | 不能由单次transport delivery替代 |
| OperationKey | Saga/phase/step的业务操作 | 同一步恢复保持原身份 |
| CommandID | 某次派发身份与回执键 | Resume后不能复用旧生命周期命令 |
| Incarnation | Resume代际 | 内存、BSON、重读和完整替换必须保存 |
| Version/Lease | 持久CAS/协调者准入 | 不用放宽条件掩盖回执冲突 |

旧代际0仍使用历史ID格式，Resume后用r1/r2区分。以前内存Record与Resume已有代际，Mongo recordDoc缺字段；返回的resumed对象是1，下一worker从数据库读回0，随即复用旧命令。旧失败回执相同就当成重复，不同就冲突，两者都可能阻止新状态推进。

本次补正式 MongoStore（mongotest后端）的连续恢复消费：失败→Resume→新Store/Engine重读→派发/outbox→失败→再Resume→成功与重复回执。另用旧缺字段/0、正数与uint32最大值检查Get/GetByBusinessKey/List/Replace。后端替身不替代真实Mongo网络、事务回调重试和HA，但实际BSON字段转换已穿过，区别于只用memoryStore。

## 为什么既有测试没发现

健康测试只有正常装配/普通消费者，新增第三消费者的成功消费回归证明“有人接收”，没有证明“这个人退出后必被发现”。Resume回归用了memoryStore，直接保存Record，绕开了BSON表示，因而生成新的CommandID却没检查真实重载。

后续按资源责任列故障场景：每个必需消费者分别缺失/关闭、部分启动、Drain与重试；按持久字段列跨边界承诺：创建、普通替换、事务替换、重读、列表与恢复后的实际派发。检查最终状态与回执，而不只核对返回对象或增加包测试次数。图谱定位调用关系后必须读当前转换源码，不能把索引/类型中有字段当成数据库也保存了它。

修复是常数健康检查与一个持久字段，没有新锁、网络往返或后台机制。旧writer可能在完整Replace时丢新字段，升级不能宣称混跑安全；历史waiting/回执不自动改写，须按已有业务恢复流程处理。真实Mongo/NATS、掉电、HA及容量仍需要独立验证。
