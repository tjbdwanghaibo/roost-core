# N04第四批：Mongo替身边界接续

2026-10-04，源码起点`ce90e90d`，同树[NC-21～25已修](REVIEW-2026-10-04-noncore-15.md)。本轮读取WANTED未见新10-02～04候选，10-01四项沿用已登记分流，不重复验收。继续此前具名未完成项，没有重复Service十域或核心三模块全域审查。Tier2旧图谱作定位，以当前源码、公开API和可复跑反例补证。

| 新审查范围 | 实际结果 | 结论 |
| --- | --- | --- |
| BSON.D dotted-path读/set/unset | 3失败、3平面对照通过 | [NC-26](../bug/REVIEW-2026-10-04-noncore-16.md#rr-20261004-nc-26) P3 |
| 唯一索引建立既有重复 | 1失败、1不同值对照通过 | [NC-27](../bug/REVIEW-2026-10-04-noncore-16.md#rr-20261004-nc-27) P3 |
| bulk非法模型Type预检 | 1失败、1正常对照通过；与正式driver调用前预检对照源码 | [NC-28](../bug/REVIEW-2026-10-04-noncore-16.md#rr-20261004-nc-28) P3 |
| A abort/B commit隔离 | 旧集合与B新集合2失败、1顺序abort控制通过；channel排序，无sleep | [NC-29](../bug/REVIEW-2026-10-04-noncore-16.md#rr-20261004-nc-29) P3 |

**13叶子7fail/6控制，4根因，全部未修**。[overlay/日志](evidence/noncore-review-20261004-16/README.md)。这批只确认公开替身错误，不把其结果当真实Mongo差分。包race全绿与逻辑契约失败可以并存，race只检查数据竞争。

设计上复用现有BSON codec、路径访问、唯一比较和transaction context；优先明确替身支持范围，避免为可用性猜测、无条件恢复全库或新的重试抽象。最难的是并发事务隔离：全局锁可能阻塞callback交叉等待，应先决定写集/可见性及冲突契约，详见[机制与实施交接](IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md)。精确比较/深复制会有成本，本轮无性能对照，不给收益比例。

源文固定候选 **40/40累计已读**，不是本轮新增40文件：38文件核对前轮WorkingSHA256一致，两个产品文件复用未变范围并读当前diff/相关方法；Blob为起点、WorkingSHA256为本轮当前树，二者不能混用。[inventory](evidence/noncore-review-20261004-16/inventory.csv)和[source hashes](evidence/noncore-review-20261004-16/source-hashes.csv)。45路径覆盖检查记录旧索引freshness限制，没有新的索引完整性证明。

N04 **场景部分完成**，不计completed/15。下一新范围Redis锁/AutoExtend/pubsub真实故障、RefHMap schema与未知结果恢复、正式迁移消费；真实Mongo cursor/partial bulk/事务、Cluster/HA与容量留档。用户说“没有修复”时跳过NC-26～29验收；明确bugfix时先修这四项。约50～90有效小时仍是跨域风险预算，未根据文件数或测试数自动减工时，不是完成日期。[进度](PROGRESS.md) · [计划](NONCORE-REVIEW-PLAN-2026-10-03.md) · [余项](../bug/CARRYOVER.md)。
