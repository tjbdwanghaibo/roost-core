# Cache 准入与迁移：现有机制及实施交接

2026-10-04，N04源文`3560a19b`未改变，Tier2当前源码与反例验证。[运行/范围](REVIEW-2026-10-04-noncore-10.md) · [新问题](../bug/REVIEW-2026-10-04-noncore-10.md) · [证据](evidence/noncore-review-20261004-10/README.md)。本文件区分现有实现和待实施建议。

## Store 的准入事实与包装责任

StoreConfig 的KeyOf/ValidateKey/ValidateValue负责合法性，Stale判断旧版本，Conflict判断同版本不同内容，Superseded允许外部较新删除对缺失key仍拒绝回填。它们不是一个布尔开关：旧写迟到与同版本内容矛盾的业务含义不同。

AtomicLocal在分片锁内完成这些规则与写入；Get返回内部value，不自动深拷贝，因此生产value须不可变。按插入时钟淘汰，覆盖写产生新generation，TTL过期删除核对generation，order定期压缩以限制覆盖/删除后的旧索引。配置limit分摊到分片并向上取整，热点片不能借其他片容量。Local用可选LRU，命中需要写锁；Grouped维护分组map，缺少容量/TTL限制。二者目前Stale拒绝静默返回nil，见NC-15；不要从Atomic行为推断所有Store相同。

RedisJSON/RawJSON/JSONHash把序列化value存string/hash。Get→Stale→Set是多个RPC，文档明确只是建议性比较；正确性依赖CAS的场景应走Redis CompareAndSet等正式能力，不能把读前比较称为原子准入。Hash TTL作用于整个hash，写字段和Expire也不是事务；Raw/Hash旧写仍返回nil是本批结果契约问题，不等于这次已经验证它们的并发原子性。

ReadThrough：先L1，miss在calls表按key合并；leader同步做L2/loader/backfill，followers按各自ctx等待同一done，取消只归还仍属同一call的名额。LoadTimeout覆盖leader链，RemoteTimeout用于直接Set/Delete；没有每load一个后台goroutine。首caller取消会结束共享load，不保证其他followers继续独立加载。MaxWaitersPerKey限制单key followers，不限制同时冷key总数。

远端不可达可按IgnoreRemoteError降级，FatalRemoteError应保留一致性拒绝；目前Get/Delete遗漏分类，NC-13待实施。ReadThrough已对L1 conflict/stale回填读取已准入当前value或miss。Layered TTL期间可直接用L1，TTL<=0带remote时每次重查；其被拒回填仍交付remote旧value是NC-14，不能用backfill失败指标替代准入处理。

建议修复直接复用现有degradable、errors.Is、setLocal和Store能力，不新建缓存协议/包。NC-15统一拒写错误前检查旧消费者，明确行为收紧。Clone、CAS、墓碑与持久确认是不同保证，三项修复都不能替代Remote权威提交链。

## 资源、容量与性能

已有AtomicLocal适合不可变值的低争用命中；Local精确LRU的命中锁和RefHMap反射/多key网络代价应结合实际负载比较，本批无benchmark，不宣称哪个“更快”。Atomic使用每片容量带来碎片/大对象限制，需按部署分片数定容。

Layered expiry只在相同key重查/显式删除时回收；两层各1条数据而1000个短TTL历史key留下1000条expiry是本批观察。现有API没有expiry单独上限，不能凭L1 MaxEntries推断整个组合内存有界。可优先在支持ExpiringStore时复用entry TTL，其他store走有界清理/显式容量策略；先定兼容规则并测高基数长期内存，再实施优化。RefHMap本批只核对存取、注册keys与Lua写失败降级的范围，其递归layout/patch/跨版本schema还需接续。

Redis pipeline减少批次网络往返，既不是事务也不是跨key原子快照；Exec要检查所有command/future错误，已执行命令不回滚。CAS索引与value在同一Lua脚本维护，需要同slot；同脚本的隔离不意味着发生runtime错误后自动撤销前缀写入。对于超时未知结果，应按记录身份/当前值恢复，不能盲目当未应用后重做副作用。

## Migration 的版本、数据与失败责任

Registry维护From唯一且To递增的step，在ctx检查后逐步Apply，并在每个成功step后SetDataVersion；RunFrom不是事务，失败callback对对象已做的修改不会自动回滚。业务应使用离线副本或在正式业务事务里负责恢复，不能仅把version保持旧值称为回滚成功。

DAORegistry按collection+From登记，在输入、传入step和获得输出时复制bytes；失败返回nil bytes、已完成版本cur和可errors.Is提取的原因。本批证明输入不被step改写、返回不与step保留buffer别名、第二步失败不发布partial bytes和取消不进下一step。cur说明成功前缀，不代表调用方已经持久化该前缀；没输出不能自行把cur写回数据库。全局MigrateDAO使用Background，不等于外部提供的业务预算自动传播。

公开迁移registry只是数据转换工具，正式DAO/codegen加载、存储格式发布和失败重启还需完整消费者证据。本批没有改用户数据、注册默认迁移或验证滚动升级。

## Mongo 接入和后续顺序

NewClient设置majority/journal、primary和连接池；StartSession设置snapshot事务读与majority写，session.WithTransaction委托SDK重试并加配置的TransactionTimeout。业务callback可能再次执行，外部奖励/消息不是Mongo事务的一部分。Find/aggregate调用cursor.All，会聚合全部结果；StreamFind逐条复制raw BSON给同步consume，可控制积压，但业务handler仍需响应取消。

BulkWrite转换支持四类model，未知类型先拒绝，驱动错误保留duplicate与transaction label链。网络/部分写错误不证明无副作用；写了多少、未知提交和重试恢复要结合SDK结果与业务身份。EnsureIndexes默认失败，重建需要策略和index声明允许，不把drop/recreate当无风险操作。StreamFind defer关闭使用当前ctx，取消后的killCursors、callback错误与关闭错误归因尚需真实资源验证。

KitRedis主要解析配置、发布core Assembly并转发生命周期；KitMongo仍持有client、配置与health/启动/部署校验逻辑。只读这两文件不证明所有Kit只做组装，也未将它们的connected生命周期列为已测试。

下一入口依次是：RefHMap完整反射/patch/schema及真实Lua失败结果 → Redis driver/assembly、锁/续租/pubsub/cluster恢复 → Mongo真实cursor与partial bulk/transaction重试 → 正式DAO/codegen迁移消费。复用Service既有SHA的驱动故障证据，补缺口而不全量重跑十域。当前N04场景部分完成，固定文件清单不等于业务覆盖分母。
