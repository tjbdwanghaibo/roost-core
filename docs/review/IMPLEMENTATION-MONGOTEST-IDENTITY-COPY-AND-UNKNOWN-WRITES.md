# 测试替身的身份、复制与未知写结果

2026-10-04，源码起点`08d18be9`，同树已修NC-16～20；[运行](REVIEW-2026-10-04-noncore-14.md) · [新问题](../bug/REVIEW-2026-10-04-noncore-14.md) · [实测](evidence/noncore-review-20261004-14/README.md)。NC-21～25建议尚未实施。

## 框架现有链路

公开[mongotest](../../mongo/mongotest/mongotest.go)按真实BSON规范化输入，collections维护docs/order和uniqueIndexes；transaction context记录首个write error，后续操作拒绝NoSuchTransaction，WithTransaction以snapshot/restore负责abort与forced/transient retry。它不是生产Mongo，但Repository、Remote和业务测试可能把它的结果作为正确性证据；本页不复审核心另一线实现。

cloneDoc只复制顶层map，供snapshot、update工作副本、Find输出、Documents/Lookup等共用。一个浅copy函数的多处调用不产生隔离：嵌套M写入可以污染快照，D切片读输出可以改库存储。NC-22两类实际失败说明为什么平面事务测试不足；正式修复应保留codec的数据形状，明确复制错误传播，不能默认任意interface都能零成本深copy。并发事务snapshot是全库级，是否误恢复别的事务另需控制调度验证。

## 查询优化仍要守物理身份

_id等值/$in走idCandidatesLocked快路径，其他filter扫描order，再matchDoc、排序、分页。NC-23重复$in把同一个键放进候选多次，优化改变了返回基数；应在归一ID后去重，避免把比较重复解释成文档重复。新分页修复已经统一skip→limit，却不替它修复候选身份。

valuesEqual/compareValues先asFloat导致大整数精度丢失（NC-24）；ID键路径用整数不代表版本过滤同样精确。先处理整数比较，再定义混合数值的受支持范围；无法忠实处理时明确unsupported，比静默相等更有利于测试。不要只改测试输入绕开2^53。

FindOneAndUpdate维护目标身份与返回身份是同一承诺：更新后不再匹配旧filter很正常，应从既定ID取post-image，不能让另一个仍符合filter的文档成为结果（NC-25）。这与分页/排序问题不同，不合并编号。

## Redis未知结果不是可用性重试

[RefHMap](../../cache/ref_hmap.go)全量Set仍Get→建议性Stale→Lua；Lua通常一次完成DEL/HSET/TTL。evalWriteHashes遇任何Eval错误就降级pipeline/串行写，丢失原原因。NC-21在真实Lua应用后注入回复错误，插入另一store写v3，fallback最终覆盖回v2、返回nil。普通读前版本检查不提供CAS，这里也不拟新增CAS；应先停止无身份未知重放，保留调用方恢复依据。

NC-18修后的Patch已经采用单一同槽Lua，没有全量Set的fallback；检查沿路TYPE后维护引用、registry和路径TTL，root miss明确拒绝。这只证明该Patch执行单元，不能将其外推到全量Set未知结果或读的跨hash快照。后续优先已有错误分类、权威读取/恢复与版本能力，不新建重试层或清理生产记录。

## 代价与继续审查

复制会增加BSON/内存成本；Patch多维护祖先键；ID去重需要有界临时候选集合。没有benchmark，本轮不给加速百分比。先保住接口真实语义，再同机比较；不能为测试速度把隔离、精度或身份校验删掉。N04清单全文40/40、场景仍部分完成，继续嵌套D路径/索引/bulk/并发事务与真实资源，详见[留项](../bug/CARRYOVER.md)。
