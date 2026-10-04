# N04第三批新问题：未知写与Mongo测试契约

**后续状态（2026-10-04）：NC-21～25均已修复、声明场景验证，未发版。** [修复运行](../review/REVIEW-2026-10-04-noncore-15.md) · [NC-21](../bugfix/RR-20261004-NC-21.md) · [NC-22](../bugfix/RR-20261004-NC-22.md) · [NC-23](../bugfix/RR-20261004-NC-23.md) · [NC-24](../bugfix/RR-20261004-NC-24.md) · [NC-25](../bugfix/RR-20261004-NC-25.md)。原始失败及下文未修时点保留；后续[NC-26～29](REVIEW-2026-10-04-noncore-16.md)独立未修。

2026-10-04，main基线`08d18be9608598c042a58b3658aa4735d88f8c7b`，同树NC-16～20已修；**以下NC-21～25已确认、未修**。[运行](../review/REVIEW-2026-10-04-noncore-14.md) · [十二叶子6fail/6控制](../review/evidence/noncore-review-20261004-14/README.md) · [机制/建议](../review/IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md)。

## RR-20261004-NC-21

等级：P2。

[ref_hmap.go](../../cache/ref_hmap.go):367–424 evalWriteHashes对任何Eval错误走无条件DEL/HSET fallback，吞掉原错误。受控IRedis包装先执行真实Redis Lua v2，再由另一正式store完成v3，返回DeadlineExceeded模拟回复丢失；Set最终nil、存储v2，正常v2控制通过。没有依赖任意sleep或模拟Lua；不是实际TCP丢包验证。

Stale本来就是建议性检查，不能据此要求整个Set变CAS；本项针对已经执行但未知的写被无身份重放，重复覆盖另一完成的写并报成功。已有非原子降级告警不能区分未应用/已应用/未知。建议复用错误链/既有Lua能力：网络取消/超时/结果未知直接保留原因，不自动DEL重放；只有能证明未应用且已明确接受的降级类型才另定策略。需要正式消费者、执行前/执行后失败、context取消与保留较新值控制；不自动回滚已提交数据。

## RR-20261004-NC-22

等级：P3。

[mongotest.go](../../mongo/mongotest/mongotest.go):1200–1206 cloneDoc只复制顶层map，snapshot/update/decodeSlice共享嵌套M/D。两反例：先正常Update建立meta.n=1，再WithTransaction改9并返回业务abort，最终仍9；Find输出中的BSON.D元素改9，不做写API，后续FindOne也9。平面abort/输出复制两控制通过。

这是公开替身声称的回滚/输出隔离未履行，不是生产Mongo事务缺陷。建议复用真实BSON编解码做深复制或明确完整类型复制，保证snapshot、读输出与写入工作副本隔离；错误时不能悄悄退回浅复制。验收M/D/array/binary、callback失败/重试、当前数据不与输出或旧快照共享；并发事务全库restore隔离另留项，不由深复制顺便宣布已解决。

## RR-20261004-NC-23

等级：P3。

[mongotest.go](../../mongo/mongotest/mongotest.go):1001–1030 _id $in优化逐个append候选、不去重。两文档[1,2]、查询$in=[1,1,2]，Find返回[1,1,2]；[1,2]控制正常。重复候选还可进入Count/UpdateMany/Bulk的共同matchLocked；这些扩展由源码可达，未逐个实测。

建议在按idKey归一后的候选键上保持顺序去重，复用现有成员过滤；不全表扫描或去重最终业务字段。验收重复/混合数字类型/不存在ID、Find/Count/UpdateMany计数；测试结果不能因一个条件重复而重复同一物理文档。

## RR-20261004-NC-24

等级：P3。

[mongotest.go](../../mongo/mongotest/mongotest.go):1686以后valuesEqual与compareValues把整数转float64。真实BSON编码保存int64 9007199254740992，查询version=9007199254740993仍命中；7查询8控制为空。非_id过滤走该路径，_id直接键定位不能证明所有版本比较精确。

建议先按有符号/无符号整数精确比较，再明确整数与浮点混合规则，不能靠提高测试容差。验收2^53附近、int64极值、排序/比较操作符与CAS谓词；不要求改变Mongo数据为浮点。本机未做真实Mongo差分，此处是测试替身数值承诺与实际错误。

## RR-20261004-NC-25

等级：P3。

[mongotest.go](../../mongo/mongotest/mongotest.go):675–728 FindOneAndUpdate ReturnAfter先更新首匹配，再重跑旧filter；仅在after为空时按旧ID找回。两文档version=1，更新首个为2，返回却是另一文档id2/version1；只一文档的控制返回id1/version2。

建议保存被更新物理身份并从同一身份返回post-image，upsert使用实际插入身份，不能重新选择旧filter匹配者。正式[driver](../../mongo/driver/collection.go)直接Decode FindOneAndUpdate的single result，不做第二次匹配；本项只确认替身错误。验收匹配多条/离开filter、ReturnBefore/After、upsert、nil result与失败无结果。

## 观察和剩余验证

mongotest normalizeDoc把嵌套文档解为D，而lookupPath/setPath主要认M/maps；本轮先纠正探针对D的假设，没有把最初panic当RR，也未补完整dotted-path忠实性矩阵。unique index建立是否拒绝既有重复、unsupported bulk前缀及并发transaction restore仍留项。真实Mongo、Cluster/HA、锁/订阅故障、容量与性能未执行；源文40/40不是这些场景已通过。
