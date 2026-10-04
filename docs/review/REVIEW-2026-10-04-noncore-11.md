# 第六批修复：NC-13～15 缓存准入

2026-10-04。开工干净，fetch/pull --ff-only后main仍为`1502f97372139a238e73850ff7337b9454146f9e`。使用roost-bugfix/roost-review与仓库roost-coding；仓库三个同名规范skill的完整包与本机镜像逐文件摘要一致，无更新。WANTED头部无新未分流候选。未修改另一线Nest/Sync/DataEngine，没有发版/tag/部署。

Tier2，roost-core，D:/whb_s/cube-core，generation `2026-09-30T11:58:14Z`；ready非最新。search_graph定位loadOne/degradable/RefHMap，双向trace与snippet后检查具名证据路径及cache/redis/mongo/DAO scopes；metadata_changed/not_tracked回读当前源文。generic Get/Set与receiver重名缺失，启发式跨包边不作实际调用证明。代码生成模板和实际消费者补证，不从图谱缺失推定没有调用。

| RR | 实施 | 验证/兼容 |
| --- | --- | --- |
| [NC-13](../bugfix/RR-20261004-NC-13.md) | Get复用fatal分类，Delete拒绝前保留L1 | 原2失败转绿；10项Get/Delete分流/副作用控制，普通strict清L1行为保持 |
| [NC-14](../bugfix/RR-20261004-NC-14.md) | 不交付被拒回填；读当前/miss/错误，不续TTL | 原3失败转绿；4项读回失败/普通故障控制；正式生成cached DAO通过 |
| [NC-15](../bugfix/RR-20261004-NC-15.md) | 四Store旧写返回ErrStaleWrite | 原4失败转绿；六Store等/新/旧版本，真实Redis6项全绿；正式生成Local DAO透传错误 |

修前21正式叶子9fail/12控制；归档Red入口复跑结果一致。修后41正式叶子全部通过，五包race186 test pass事件/170叶子、0fail/7环境skip，KitRedis无测试仅编译/vet；五包vet exit0。随后独占真实Redis七项集成全部通过，cache真实Redis六项修复控制也全绿。正式DAO CLI生成两个消费者用例并race/vet通过，归档Run-Consumer实际复跑也通过。[日志、退出码、当前hash与复跑入口](../bugfix/evidence/noncore-bugfix-20261004-06/README.md)。不同执行的重复场景不相加作为覆盖率。

行为收紧和失败结果写入USER_GUIDE/CHANGELOG，原问题只追加修复状态。N04继续[第二批审查](REVIEW-2026-10-04-noncore-12.md)：本轮旧三个RR已修，新NC-16～20仍待修；真实Mongo/etcd、Cluster/HA、expiry容量仍留项。未查GitHub CI，不把本地通过称CI通过。
