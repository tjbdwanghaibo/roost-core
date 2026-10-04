# N04第四批：Mongo替身的嵌套路径、索引与事务隔离

2026-10-04，main起点 `ce90e90d43cfaf7b71d3487a84ea2f3d32f97d0b`，同树NC-21～25已修；以下四项 **已确认、未修**。只针对公开 `mongo/mongotest`，不是生产Mongo故障结论。[运行](../review/REVIEW-2026-10-04-noncore-16.md) · [13场景7fail/6控制](../review/evidence/noncore-review-20261004-16/README.md) · [实施交接](../review/IMPLEMENTATION-MONGOTEST-IDENTITY-COPY-AND-UNKNOWN-WRITES.md)。

## RR-20261004-NC-26

等级：P3。

[mongotest.go](../../mongo/mongotest/mongotest.go)的normalizeDoc通过真实BSON codec保存嵌套文档为bson.D，但lookupPath/setPath/unsetPath只接受M/maps。Seed `{meta:{n:1,keep:"yes"}}` 后，`meta.n=1`查询返回0；`$set meta.n=9`删除无关keep；`$unset meta.n`成功但n仍1。三平面操作控制通过。深复制NC-22故意保留M/D形状，未替本项修改路径语义。

建议在现有路径访问器支持D，或在明确的统一规范化边界使用M；不能仅在测试里手工转M。保留兄弟字段、嵌套数组/空文档、缺路径和非容器报错等契约需先列矩阵。查询、排序、唯一索引和更新共享访问器，要测其正式消费者；不扩展为全部Mongo路径语法。本轮没有真实Mongo差分。

## RR-20261004-NC-27

等级：P3。

EnsureIndexes直接登记Indexes/uniqueIndexes，没有检查既有docs。先Seed两条不同_id、同name=1，再建立name唯一索引，返回nil且HasIndex=true；不同name控制通过。后续insert的唯一检查不能证明索引建立时数据合法，会让repository初始化测试假成功。

建议复用现有字段读取和精确比较，在发布唯一索引前检查已有数据，冲突保留ErrDuplicateKey；失败不得留下声称成功的索引。非唯一、复合、稀疏/缺字段、定义冲突与单个索引发布原子性另列验收，不擅自承诺多个索引全批原子，因为正式driver逐个CreateOne。未做真实服务端索引差分。

## RR-20261004-NC-28

等级：P3。

BulkWrite逐个执行models，遇末尾非法Type才返回ErrUnsupported。`[InsertOne(_id=1),Type=255]` 后集合已有一条；合法InsertOne控制正常。正式[driver BulkWrite](../../mongo/driver/collection.go)先完整转换模型Type，非法Type在调用底层coll.BulkWrite前直接返回，所以这种非法模型不会提交前缀。

建议在替身执行前验证所有模型Type，再按原顺序执行。不把正常ordered bulk的服务端duplicate-key前缀成功回滚，也不要求driver未知结果自动补偿。需验收首/中/尾非法类型、全合法模型及真实写冲突部分成功；本项只确认非法模型Type预检差异，不外推全部非法filter/update的预检时点。

## RR-20261004-NC-29

等级：P3。

WithTransaction snapshot/restore恢复全client集合，不区分写所有者。channel控制事务A写id1并停住；事务B用另一session写id2、成功提交；随后A返回business abort。旧集合和B新建集合两个场景都使B的id2消失，Count=0；顺序执行的A回滚控制保持id1=1。race未报数据竞争，逻辑隔离仍失败；不使用任意sleep。

建议先明确替身支持的并发事务模型，再复用transaction context、collection身份和写集：abort只撤销自身未提交状态，不能恢复全库覆盖已提交写。可评估受限的串行事务模式，但全局锁若callback等待另一个事务会死锁，也不隔离非事务写；禁止简单加锁后宣称Mongo等价。验证同/异集合、事务外写、新集合、读可见性、冲突与重试，区分测试替身支持范围。此项不要求改生产Mongo、Nest或DataEngine。

## 留项

本轮13新场景7失败对应4根因，6控制通过；未修。其余N04源文仍40/40复用已读，场景部分完成。Redis锁/续租/pubsub真实故障、Cluster、真实Mongo cursor/bulk/事务以及正式迁移消费者留在[CARRYOVER](CARRYOVER.md)，不由本次替身审查宣布收敛。
