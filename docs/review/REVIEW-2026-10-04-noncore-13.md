# 第七批修复：NC-16～20

2026-10-04，main起点`08d18be9608598c042a58b3658aa4735d88f8c7b`，fetch/pull无增量，干净开工；仓库三套Roost skill包与本机镜像一致。使用roost-bugfix、roost-review、roost-coding和codebase-memory Tier2。没有发版/tag/部署。

五项已修、声明场景验证：[16](../bugfix/RR-20261004-NC-16.md)、[17](../bugfix/RR-20261004-NC-17.md)、[18](../bugfix/RR-20261004-NC-18.md)、[19](../bugfix/RR-20261004-NC-19.md)、[20](../bugfix/RR-20261004-NC-20.md)。原16正式叶子10fail/6控制，修后最终41新正式叶子通过；旧产品overlay实际复跑同样红。十包race/vet通过，七测试包252叶子/273pass事件、0fail/skip，三包无测试。

RefHMap保留根V形状、指针codec用地址副本、nil根写前拒绝；Patch同槽原子补祖先/登记键/TTL，缺整条记录明确拒绝，错误不降级重放。布局在I/O前拒绝内部名称/重复字段/路径分隔符及重复物理键。mongotest两分页入口共用排序后skip→limit，大int64比较后切片。正式DAO CLI独立生成ref-hmap业务module，连接同一专属Redis验证nil父与已有父Patch；模板没改，无存储迁移。

[原始日志/退出码/复跑](../bugfix/evidence/noncore-bugfix-20261004-07/README.md)。一次中间vet因测试夹具重复json tag失败已保留，换成同义redisdao碰撞夹具后正式回归通过；不是放宽门禁。Redis由本轮创建并退出，未碰用户实例。真实Mongo、Cluster/HA、未知外部网络结果、长期容量和性能未验，GitHub CI未查。

然后继续[N04第三批](REVIEW-2026-10-04-noncore-14.md)，新NC-21～25未修，与本页修复分开。[进度](PROGRESS.md) · [留项](../bug/CARRYOVER.md)。
