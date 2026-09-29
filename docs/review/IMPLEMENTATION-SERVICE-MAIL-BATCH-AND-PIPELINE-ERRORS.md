# Mail 批量读取与 Pipeline 错误分层

2026-09-29；基线68cf87fc加[RR-33实现](../bugfix/RR-20260929-33.md)，[运行](REVIEW-2026-09-29-services-10.md)。以下“现有”指本次代码，“待实施”不算已修。

## 当前读取链

Kit Mod Init读取prefix/send_ttl，Provide共用clock构建Redis三store并发布Mail/owner capability。消费者的List或生成RPC处理器进入Core：读mailbox→必要的过期CAS→按delivery/id排序与cursor取有界窗口→GetMany→把可读信封合并成Page，缺失计数但不为填满页追加无界读取。Mailbox与发送ledger仍用现有versionstore单key协议。

GetMany先校验全部id，创建独占pipeline，最多100GET/100FutureBytes，一次Exec后读取每个future；defer Discard释放缓冲。真正ErrNil跳过，存在的空字节仍解码失败；读取/解码失败返回nil map防止部分页被误用。键未变，Cluster沿已有driver分节点，不集中所有Mail到一个slot。信封不可变而会过期，批次不提供跨key原子快照；业务仍在组Page时检查expires。

IPipeline是缓冲批处理，不能代替Lua/CAS事务。并发使用一个pipeline/Future并无同步保证；Mail每次调用单独持有并在Exec完成后读，生命周期明确。副本/failover、未知回复与生产网络没有被这次本机测试证明。

## 两层错误都要看

底层Exec的aggregate可能只是首个命令错误，wrapper目前忽略redis.Nil。即使Exec=nil，单future仍可能WRONGTYPE。Mail修复逐项检查，因此missing先于bad也返回错误；cancelled context返回取消，未把transport故障折成缺失。

新[RR-34](../bug/REVIEW-2026-09-29-services-10.md)证明无future写命令更危险：GET缺失→HSET wrong type时Exec=nil、写未应用，调用方连future也无从查。修法应在wrapper检查Exec返回的所有Cmder错误并保留已经填充的future，而非让每个业务服务猜命令状态。该driver行为本轮**未实施**；目前不能以nil Exec证明任意混合批次全部成功。

## 设计与性能评价

批次上限在Service和store两层，避免删邮件产生额外无界网络读；Cluster可用普通prefix，复用已有工具，无key迁移、全局slot热点和新infra。代价是相较单机MGET排队N个GET、N个future并逐项解码，跨节点有多批网络请求。两种实现都随page大小增长，既有信封TTL/不可变模型适配该取舍。此次是正确性验证，没有相同负载前后性能测量，不能宣称提升或长期容量保证。

100service生产路径的97未变blob仅复用既有有界证据，3变化重新核对；cache读/写pipeline及pinned durable eval只跟进与错误分层有关的范围，不把整个cache/DataEngine重新标成已审完。Service10域主链有界整理保持完成，外部allocator/资产/支付、历史对账、fulfilled归档、HA/强杀/长稳仍是独立入口。
