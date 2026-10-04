# N04 第一批：缓存、Redis 批次、Mongo 与迁移

2026-10-04，起点main`3560a19b5c2b2fbeb936bf1bb6a9c1fa0be1f956`，pull无增量；同一工作树先修[NC-11/12](REVIEW-2026-10-04-noncore-09.md)。本批N04/Kit数据库产品源码未改，仅overlay验证和文档。

## 实际范围与图谱限制

N04固定清单40个生产Go候选，**20/40完整源文已读**：cache十文件（不含RefHMap）、migration1、Mongo接口/批次/driver三文件共5、Redis CAS/pipeline接口/pipeline driver/versioned lock共4。另读KitRedis/KitMongo2个N14同行文件。每个实际文件、blob与未读状态见[inventory](evidence/noncore-review-20261004-10/inventory.csv)，这不是50%业务覆盖率。

RefHMap存取/注册keys/Lua fallback（约1–400行）、mongotest事务恢复片段、Redis driver client配置/NewClient/Get前段（1–103行）只计具名范围，不算完整源文。依赖与tests只为对应场景读取，不从包测试绿升级它们为全读。

Tier2，project roost-core，root D:/whb_s/cube-core，generation `2026-09-30T11:58:14Z`。search_graph按文件/符号定位，相关结果页已读；初次Kit宽搜索142个结果仅读第一页40条，用两个精确Kit目录查询补齐相应范围，没有声称宽查询全覆盖。双向trace覆盖loadOne/RunFrom/MigrateDAO、CAS、StreamFind/WithTransaction、Kit入口；snapshot snippet覆盖关键函数。coverage调用覆盖所有具名源码/fixture，均metadata_changed，新增文本not_tracked，scope无记录缺口不代表完整；当前源文补证。generic/重名Get/Set并非都出现在符号结果，跨包启发式边不能据此判实际调用；一次WithTransaction project参数误写后用roost-core重查成功。

## 新反例与实际验证

| 执行 | 结果 | 限制 |
| --- | --- | --- |
| cache/migration普通overlay | 26叶子/独立项：9行为失败、16控制、1仅观察 | 3新RR，不把失败父测试另计；没有改产品让反例变绿 |
| 五个有测试包race | 113 test pass事件/105叶子、0fail、7skip | 未设ROOST_REDIS_TEST_ADDR；Mongo接口与两个Kit包无测试，仅编译/vet |
| 八包vet | exit=0，无输出 | 不是connected装配或全仓门禁 |
| 专属真实Redis8.8.0七项 | 原7skip全部实际执行通过、0fail/skip | 本机单节点Windows/MSYS，不是Redis Cluster/HA/生产版本认证 |
| core cache→正式Redis driver→实际Redis | 6叶子：Raw/Hash旧写2fail、JSON旧写及三种较新写4pass | 补强NC-15，没有另造两个RR或证明并发CAS |

完整命令、版本、退出码、skip原因、实际日志和源码SHA256见[证据](evidence/noncore-review-20261004-10/README.md)。既有Service驱动结论按[完成矩阵](SERVICE-REVIEW-COMPLETION-2026-09-29.md)及[10-01复审](REVIEW-2026-10-01-bline-audit-service-2.md)复用其历史SHA，不将引用等同本批复验。

真实Redis首次启动失败：MSYS可执行文件把绝对Windows配置路径D:/…当相对POSIX路径；修为专属working directory和相对redis.conf后，第二实例入场并跑测试。首次实例实际退出、第二实例仅按本次创建PID关闭，退出确认留档。启动失败是环境路径问题，没有登记产品RR；原7skip日志保留，补测另列，不回写原结果。没有碰用户其他Redis实例。

## 问题、设计与停点

[新RR主记录](../bug/REVIEW-2026-10-04-noncore-10.md)：NC-13 Get/Delete遗漏fatal策略（P2）、NC-14 Layered交付一致性门禁拒绝的回填（P2）、NC-15四Store旧写结果漂移（P3），均未修。新反例/控制解释当前实现与建议方向，详见[学习/实施文档](IMPLEMENTATION-CACHE-ADMISSION-AND-MIGRATION.md)。

Layered过期元数据增长只观察；没有许诺其不存在的独立上限。Atomic分片配额与LRU命中锁、ReadThrough单key等待限制与全局冷key数量、RefHMap跨key存取和fallback原子性、Mongocursor/事务副作用、Migration原地失败前缀均在学习文档分清。未做吞吐/分配比较，不宣称性能改善。

N04源文与场景都部分完成，仍不计completed/15。下一新入口：RefHMap余下完整反射/patch/schema与Lua结果未知 → Redis assembly/锁/pubsub/cluster恢复 → Mongo真实cursor/部分bulk/事务重试与正式DAO迁移消费者。用户明确未修时跳过NC-13～15验收直接接续；要求bugfix时先修这些新RR。N03正常Resign预算/服务端租约回收、真实etcd/NATS仍留台账。既有约50～90有效小时是风险预算，本批尚未逐个核定余项新工时，不按20/40文件数减半。

[进度](PROGRESS.md) · [15域计划](NONCORE-REVIEW-PLAN-2026-10-03.md) · [验证留项](../bug/CARRYOVER.md)。本机无gh，GitHub CI未查；本轮没有发版/tag/部署。
