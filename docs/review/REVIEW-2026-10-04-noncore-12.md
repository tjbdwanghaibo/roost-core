# N04 第二批：RefHMap 全文、Redis 生命周期与 Mongo 边界

2026-10-04，main起点`1502f97372139a238e73850ff7337b9454146f9e`，fetch/pull无增量，干净开工；同树先修[NC-13～15](REVIEW-2026-10-04-noncore-11.md)，本批RefHMap/mongotest产品未改。

## 本轮推进与图谱证据

固定N04清单40项，补齐19个此前未全读候选：RefHMap1、Mongo接口/config/driver database/errors/index/session7、Redis接口/config/driver assembly/client/cluster recovery/lock/pubsub/errors/lock/pubsub/script11。累计**39/40完整源文已读**；原20按同baseline blob复用与六个修复文件当前源码核对，不将其算19个新文件。mongotest仅1–595行，余下源码未完整审查。[清单、来源/blob和当前working hash](evidence/noncore-review-20261004-12/inventory.csv)。39/40不是97.5%业务覆盖，也不是本域完成。

另读正式Redis DAO模板/CLI/参数与tests对应片段，Manifest/generate只为消费者发现读取具名范围；一次假设redis_test.go不存在，随后图谱定位真实parse_test.go的Redis用例，不依据缺文件判无测试。正式生成采用codegen/cmd/dao，不需要完整游戏manifest。

Tier2，project roost-core，generation `2026-09-30T11:58:14Z`，ready但陈旧。search_graph RefHMap52条、Redis四文件精确查询均全页、DAO Redis44条、mongotest Find相关12条；联合regex误用file_pattern返回0，改为四个精确文件查询，0不是不存在证明。trace双向覆盖Patch/evalWriteHashes/编码/layout、watch/newPubSub、Find，snippet核对关键源码；泛型方法/同名receiver/跨包启发式边不完整，以当前源码补证。全部具名证据路径coverage为metadata_changed或新附件not_tracked；cache/redis/mongo/DAO scopes无记录缺口仍非完整证明，未停止或重建共享索引。

## 实际执行

| 执行 | 结果 | 边界 |
| --- | --- | --- |
| 专属真实Redis8.8.0 RefHMap | 八叶子5fail/3控制，确认NC-16～19 | 真Lua、pipeline、hash与framework反射，不是替身重写脚本；单节点Windows/MSYS |
| 同实例原七项Redis集成 | 7pass、0fail/skip | 不包含真实Cluster/failover，不能推断durable WAITAOF通过 |
| 同实例六项cache旧写/较新写 | 6pass、0fail/skip | 属NC-15修复验证，与新RefHMap失败分开计 |
| mongotest分页overlay | 八叶子5fail/3控制，确认NC-20 | 正式公开替身，不是Mongo服务端；Find/Stream分别核对 |
| Mongo/mongotest/migration及Kit正常回归 | 三测试包race34项通过、0fail/skip；五包vet exit0 | Mongo接口与KitMongo无测试；没有真实Mongo |

[原始JSONL、退出码、复跑脚本及SHA256](evidence/noncore-review-20261004-12/README.md)。两个新审查共16叶子10fail/6控制，五根因；原七项与六项修复控制不重复当新业务覆盖。Redis为本轮独占loopback/无持久化实例，PID10916、port6723，finally退出确认true；未连接用户业务实例。

## 设计、性能与接续

[新问题/实施方向](../bug/REVIEW-2026-10-04-noncore-12.md)：指针根panic、TextMarshaler寻址、nil父Patch不可见、内部名称碰撞四P2，测试替身分页一P3。均未修，与已修NC-13～15分别登记。机制、存储格式兼容和资源责任见[学习文档](IMPLEMENTATION-REFHMAP-LAYOUT-PATCH-AND-REDIS-LIFETIME.md)。

RefHMap每store一次layout复用，不是每次重建；Get对plan全部hash网络批读，子hash数量、reflection/文本codec与Lua args共同影响成本。已有缓存设计可以复用，未新增抽象或无收益优化。没有吞吐/alloc benchmark，不宣称性能改善。Redis锁无fence及更新未知结果、Watch/订阅退出、Mongo majority/snapshot/retry等边界保留，不由39文件阅读宣布整体正确。

下一新范围仍N04：mongotest595以后（filter/update/bulk/index/snapshot事务忠实性）→ RefHMap Eval失败/未知结果与Patch TTL/schema故障 → Redis订阅/自动续租真实故障/Cluster → Mongo真实cursor/partial bulk/transaction retry与正式DAO/codegen迁移消费者。用户声明未修则跳过NC-16～20验收直接接续；要求bugfix时先修本轮五项。N04场景部分完成，不计completed/15；约50～90有效小时仍为跨域风险预算，未按源码39/40扣减。[进度](PROGRESS.md) · [计划](NONCORE-REVIEW-PLAN-2026-10-03.md) · [留项](../bug/CARRYOVER.md)。本轮没有发版/tag/部署，GitHub CI未查。
