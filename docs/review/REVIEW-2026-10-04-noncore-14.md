# N04 第三批：mongotest全文与Lua未知结果

2026-10-04，main源码起点`08d18be9608598c042a58b3658aa4735d88f8c7b`；同树先修[NC-16～20](REVIEW-2026-10-04-noncore-13.md)，再review其余路径。新[NC-21～25](../bug/REVIEW-2026-10-04-noncore-14.md)均未修，不递归扩成本轮修复范围。

## 新覆盖与证据

补读mongotest此前595以后：update/find-and-modify、bulk、索引、过滤/排序/数值、snapshot/restore/输出复制；复核其1–595与当前分页改动。RefHMap复核当前修复及evalWriteHashes故障路径。N04固定清单累计**40/40全文已读**，其余38按前轮当前hash复用，[清单/blob](evidence/noncore-review-20261004-14/inventory.csv)；这仅关闭源文阅读缺口，不等于100%业务覆盖或本域场景完成。

Tier2 project roost-core，generation`2026-09-30T11:58:14Z`仍陈旧，未重建/停止共享服务。RefHMap52条、mongotest首批17条与后续9/4条查询均全页；双向trace覆盖Patch/codec/cloneDoc/evalWriteHashes。泛型Get/Set或动态模板调用不全、同名启发式存在误边；陈旧snippet的旧行号还截到了相邻方法，故关键结论以当前完整源码/模板补证，不以0caller断言无人使用。[coverage/摘要](evidence/noncore-review-20261004-14/README.md)。

## 实际执行

| 执行 | 结果 / 边界 |
| --- | --- |
| RefHMap真实Redis+受控执行后丢回复 | 2叶子1fail/1控制；v2已执行、另一store写v3后返回DeadlineExceeded，fallback覆盖回v2且Set返回nil；不是真实断网/HA |
| mongotest公开契约overlay | 10叶子5fail/5控制；浅复制2失败为同根因，另重复$in、大整数eq、ReturnAfter身份各1；不是Mongo服务端测试 |
| 修复/正常相邻回归 | 十包race/vet、252叶子pass/0fail/skip；41新正式场景与正式生成ref-hmap消费通过，单独存于修复证据 |

[原始日志与复跑](evidence/noncore-review-20261004-14/README.md)。初版探针误判BSON嵌套表示而panic已纠正，原样保留并排除，不当产品红测；最终十二叶子6fail/6控制，五根因。两个独占Redis实例均退出。

## 机制、性能与下一入口

[RefHMap修后机制](IMPLEMENTATION-REFHMAP-LAYOUT-PATCH-AND-REDIS-LIFETIME.md)与[替身可信度学习](IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md)区分实现、验证与拟议修复。Patch新增Lua/祖先HSET、TTL与registry维护有成本，但避免Get→全量Set，没有benchmark，不声称加速。mongotest _id快速候选减少扫描，却重复$in值；float转换不能替代整数精确比较；浅快照不能证明事务隔离。正式DAO nil父Patch实测补上了上轮仅模板可达的缺口。

下一新范围：Mongo替身其余场景（BSON.D嵌套路径、unique-index建立/失败与bulk/并发事务隔离）→ Redis锁/续租/pubsub真实故障/Cluster → Mongo真实cursor/partial bulk/transaction retry与正式迁移消费。源码阅读完成而场景仍部分完成；用户说未修直接继续新场景，要求bugfix先修NC-21～25。N01～N04仍不计completed/15，约50～90有效小时是跨域风险预算，未按40/40扣减。[进度](PROGRESS.md) · [计划](NONCORE-REVIEW-PLAN-2026-10-03.md)。未发版、未部署。
