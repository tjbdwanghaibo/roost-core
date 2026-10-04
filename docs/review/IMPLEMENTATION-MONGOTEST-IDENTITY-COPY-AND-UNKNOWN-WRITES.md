# 测试替身的身份、复制与未知写结果

2026-10-04，首次源码起点`08d18be9`；随后在`ce90e90d`起点[修复NC-21～25](REVIEW-2026-10-04-noncore-15.md)，[接续审查](REVIEW-2026-10-04-noncore-16.md)确认NC-26～29，随后在`3d3b22c9`起点[四项已实施](REVIEW-2026-10-04-noncore-17.md)。下文记录当前实现和未证明边界；[旧反例](evidence/noncore-review-20261004-14/README.md)保留。

## 框架现有链路

公开[mongotest](../../mongo/mongotest/mongotest.go)按真实BSON规范化输入，collections维护docs/order和uniqueIndexes；transaction context记录首个write error，后续操作拒绝NoSuchTransaction，WithTransaction以私有snapshot、revision检查及整体发布负责abort与forced/transient retry，不再restore共享全库。它不是生产Mongo，但Repository、Remote和业务测试可能把它的结果作为正确性证据；本页不复审核心另一线实现。

原cloneDoc浅copy导致嵌套M污染快照、D切片读输出改库存储。NC-22现已递归复制BSON可变容器、保持M/D及二进制/数组/scope形状，不靠重复codec转换或失败时浅copy退路；正常codec归一的BSON标量按值保留，不是任意Go interface深复制。嵌套abort、读输出和身份拒写工作副本通过；[正式红绿](../bugfix/evidence/noncore-bugfix-20261004-08/README.md)。NC-29现已用私有快照隔离，abort只丢弃本次数据，集合粒度冲突比真实Mongo保守；深复制本身不提供隔离。

## 查询优化仍要守物理身份

_id等值/$in走idCandidatesLocked快路径，其他filter扫描order，再matchDoc、排序、分页。NC-23现已在归一ID后稳定去重，Find/Count/UpdateMany不重复物理文档；混合宽度和missing ID已验。分页排序→skip→limit与候选唯一是独立契约。

NC-24现用标准库big.Rat/Int精确比较整数及有限float，不先舍入；包含signed/unsigned极值与混合float。非有限float明确unsupported，Decimal128/服务端完整类型排序并未由此获得支持。增加分配成本，未做benchmark，不给收益百分比。

NC-25现保存被选中的物理键，upsert复用实际插入键，ReturnAfter取同一post-image；不重跑旧filter选另一文档。before/after/upsert/nil及失败结果已验。这与分页/排序问题不同，不合并编号。

## Redis未知结果不是可用性重试

[RefHMap](../../cache/ref_hmap.go)全量Set仍Get→建议性Stale→Lua，NC-21现直接透传Eval原因，取消DEL重放/降级成功。真实Lua应用后丢回复、另一store写v3，旧行为回写v2/nil；修后返回DeadlineExceeded且v3保留，正式生成DAO同样通过。已应用v2不能因错误自行回滚；此修复也不新增CAS。Lua不可用的adapter需要实现已有Eval能力；旧degraded计数/告警已移除，改由原始Set错误判别，T-206。

NC-18修后的Patch已经采用单一同槽Lua，没有全量Set的fallback；检查沿路TYPE后维护引用、registry和路径TTL，root miss明确拒绝。这只证明该Patch执行单元，不能将其外推到全量Set未知结果或读的跨hash快照。后续优先已有错误分类、权威读取/恢复与版本能力，不新建重试层或清理生产记录。

## 代价与继续审查

复制会增加内存成本；Patch多维护祖先键；ID去重需要有界临时候选集合。没有benchmark，本轮不给加速百分比。先保住接口真实语义，再同机比较；不能为测试速度把隔离、精度或身份校验删掉。原N04清单40/40；新增事务实现后当前41/41累计已读、场景仍部分完成，真实资源见[留项](../bug/CARRYOVER.md)。

## NC-26～29实施回顾与边界

[四项修复](REVIEW-2026-10-04-noncore-17.md)原13场景7fail/6控制已转绿，追加后28正式叶子通过。路径lookup直接支持D，set/unset沿路转换保留兄弟；未访问数组形状不改，不承诺完整数组路径。唯一索引复用精确比较检查存量，单项成功才发布，前项成功不因后项失败撤回；缺字段/null/sparse/同名定义完整服务端语义未核定。bulk只前置Type预检，正常ordered重复键仍保留成功前缀。

事务复用context、collection身份和深复制：attempt私有docs/order，操作持collection锁时切换视图并在退出恢复；计数和错误注入留原对象。实际写/索引发布递增revision，提交按database/name固定锁序，先验全部写集合再整体发布。冲突标TransientTransactionError、保留ErrTransactionConflict并按既有限额重试；abort/forced retry不恢复共享数据，取消或panic丢弃，finished context拒绝。Seed/Lookup/Documents是事务外已提交视图。

旧消费者“snapshot miss后Seed制造duplicate”改为明确注入其原承诺的ErrDuplicateKey，同时保留真实事务外Seed，新增abort不抹并发提交断言；原身份分类、mutation/marker回滚和幂等重试断言保留。只改必要测试夹具，生产DataEngine不改，674pass/17环境skip单列。

全client已存在集合每attempt深复制，冲突粒度为集合而非真实Mongo文档；Drop/namespace并发替换、真正服务端写冲突时点、UnknownTransactionCommitResult、HA不建模。事务内索引明确unsupported，aborted新集合可保留空句柄，callback必须等待自身操作结束。没有benchmark，不把修复转换为新分包/业务协议。后续[Redis学习](IMPLEMENTATION-REDIS-LOCK-RENEWAL-AND-PUBSUB-LIFETIME.md)与schema/未知恢复/正式迁移消费继续按现有工具验证。
