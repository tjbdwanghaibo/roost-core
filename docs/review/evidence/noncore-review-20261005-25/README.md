# N05停机与N06第一批证据范围

2026-10-05；[运行](../../REVIEW-2026-10-05-noncore-25.md)，[红绿及矩阵](../../../bugfix/evidence/noncore-bugfix-20261005-15/README.md)。

Tier2 Verify，项目roost-core / D:/whb_s/cube-core，ready32285 nodes/209828 edges，generation2026-09-30T11:58:14Z。[33路径coverage](coverage.json)无记录parse缺口，生产metadata_changed、新测试not_tracked；相关当前源码与正式执行补证。[LF源码SHA256](source-hashes.csv)用来识别后续增量，不宣称每个材料文件都做过穷尽审计。

graph search先定位Replicator Start/Stop、JetStreamSyncBus订阅、ownerroute Route、Saga Assembly/Resume/recordDoc；相关有界查询has_more=false。最初广域saga查询374结果只返55，随后按Method/Function与名字缩小范围；不将该未完成广域分页视为全Saga枚举。trace双方方向用于ConsumersClosed→Kit checkHealth、Compensate与记录转换；get_code_snippet精确读Stop、Complete、toRecordDoc。同名receiver合并/跨包误边由当前import与源码校正，不从0 caller推导无生产使用。

材料包括N05复制/路由、N06Saga的生命周期/状态/持久/consumer与Kit health、两个注册/健康公共入口及正式回归。只有saga/assembly.go、saga/mongo_store.go改变产品行为；五个新Go测试文件合计20叶子。不重审核心三域完整协议，没有把33材料路径转成产品覆盖率。

本机JetStream适配器+替身确认退订时旧在途handler可完成，重新订阅不会重复创建；测试Store有自身版本准入，这不是Remote Snapshot/L2或真实NATS正确性的证明。Saga正式MongoStore使用mongotest，覆盖实际BSON映射和恢复后派发，不代表真实Mongo事务重试、网络故障与HA。

N05外部缺口保留，接续N06具名清单；两域都部分完成，不计completed/15。进度/源码/修复/外部验收分开，未查询GitHub CI，未发布。
