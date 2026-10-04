# 测试替身的身份、复制与未知写结果

2026-10-04，首次源码起点`08d18be9`；随后在`ce90e90d`起点[修复NC-21～25](REVIEW-2026-10-04-noncore-15.md)，[接续审查](REVIEW-2026-10-04-noncore-16.md)确认NC-26～29四项未修。下文区分已实施和待实施；[旧反例](evidence/noncore-review-20261004-14/README.md)保留。

## 框架现有链路

公开[mongotest](../../mongo/mongotest/mongotest.go)按真实BSON规范化输入，collections维护docs/order和uniqueIndexes；transaction context记录首个write error，后续操作拒绝NoSuchTransaction，WithTransaction以snapshot/restore负责abort与forced/transient retry。它不是生产Mongo，但Repository、Remote和业务测试可能把它的结果作为正确性证据；本页不复审核心另一线实现。

原cloneDoc浅copy导致嵌套M污染快照、D切片读输出改库存储。NC-22现已递归复制BSON可变容器、保持M/D及二进制/数组/scope形状，不靠重复codec转换或失败时浅copy退路；正常codec归一的BSON标量按值保留，不是任意Go interface深复制。嵌套abort、读输出和身份拒写工作副本通过；[正式红绿](../bugfix/evidence/noncore-bugfix-20261004-08/README.md)。并发snapshot仍是全库级，NC-29已实测会擦除另一成功事务；深复制不提供逻辑隔离。

## 查询优化仍要守物理身份

_id等值/$in走idCandidatesLocked快路径，其他filter扫描order，再matchDoc、排序、分页。NC-23现已在归一ID后稳定去重，Find/Count/UpdateMany不重复物理文档；混合宽度和missing ID已验。分页排序→skip→limit与候选唯一是独立契约。

NC-24现用标准库big.Rat/Int精确比较整数及有限float，不先舍入；包含signed/unsigned极值与混合float。非有限float明确unsupported，Decimal128/服务端完整类型排序并未由此获得支持。增加分配成本，未做benchmark，不给收益百分比。

NC-25现保存被选中的物理键，upsert复用实际插入键，ReturnAfter取同一post-image；不重跑旧filter选另一文档。before/after/upsert/nil及失败结果已验。这与分页/排序问题不同，不合并编号。

## Redis未知结果不是可用性重试

[RefHMap](../../cache/ref_hmap.go)全量Set仍Get→建议性Stale→Lua，NC-21现直接透传Eval原因，取消DEL重放/降级成功。真实Lua应用后丢回复、另一store写v3，旧行为回写v2/nil；修后返回DeadlineExceeded且v3保留，正式生成DAO同样通过。已应用v2不能因错误自行回滚；此修复也不新增CAS。Lua不可用的adapter需要实现已有Eval能力；旧degraded计数/告警已移除，改由原始Set错误判别，T-206。

NC-18修后的Patch已经采用单一同槽Lua，没有全量Set的fallback；检查沿路TYPE后维护引用、registry和路径TTL，root miss明确拒绝。这只证明该Patch执行单元，不能将其外推到全量Set未知结果或读的跨hash快照。后续优先已有错误分类、权威读取/恢复与版本能力，不新建重试层或清理生产记录。

## 代价与继续审查

复制会增加内存成本；Patch多维护祖先键；ID去重需要有界临时候选集合。没有benchmark，本轮不给加速百分比。先保住接口真实语义，再同机比较；不能为测试速度把隔离、精度或身份校验删掉。N04清单全文40/40、场景仍部分完成，真实资源见[留项](../bug/CARRYOVER.md)。

## 新问题的实施交接（未实施）

[NC-26～29](../bug/REVIEW-2026-10-04-noncore-16.md)已用13场景7fail/6控制确认，仅测试替身，不能当生产Mongo故障。

路径首先协调BSON形状：真实codec把嵌套解为D，访问器却只认M/maps。建议复用现有lookup/set/unset统一支持D，保持兄弟字段；若统一规范化为M，需要审查对公开读输出和codec形状的兼容，而不能靠测前手工转换。查询、排序、unique和更新共享路径访问器，新增测试要跟随这些调用。

唯一索引要检查建立前已有数据，候选定义校验完成并拒绝冲突后再发布；复用精确数值和现有唯一字段组合。正式driver逐个CreateOne，不默认整批索引全原子。Sparse、缺字段/null、复合及重建策略仍需声明。

非法bulk模型Type先做全量预检，然后执行既有ordered流程；不要把正常服务端duplicate-key造成的部分成功改成回滚。正式driver在调用底层BulkWrite前完成模型转换，因此这个预检可以按实际调用契约补齐；filter/update支持范围另列。

并发事务是最需要先定模型的一项：A的全库snapshot覆盖B提交，连B新建集合也被清空。当前collection锁只是保护数据访问，不提供事务隔离。优先复用transaction context记写所有权和未提交状态，明确读可见性、冲突与重试；abort只能撤销自身数据。替身若选择限制并发，必须明确拒绝或定义行为，不能全局锁持有期间运行任意callback后宣称等价，否则callback等待另一事务会死锁，事务外写仍会被恢复覆盖。正式Nest/DataEngine核心链本轮只跑回归，不由此改其生产实现。
