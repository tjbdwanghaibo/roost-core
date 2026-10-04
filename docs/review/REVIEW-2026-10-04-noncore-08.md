# 非三大核心第五批：etcd / KitEtcd 源码接续

2026-10-04，起点main`e62729ac`，工作树先[修NC-08～10](REVIEW-2026-10-04-noncore-07.md)；etcd/KitEtcd未改。承接上一轮25/39，本批完整读剩余14个生产文件、KitEtcd1，**N03清单源文39/39已读，场景仍部分完成**。[路径/blob/范围](evidence/noncore-review-20261004-08/source-manifest.json)与[上一批manifest](evidence/noncore-review-20261004-06/source-manifest.json)可对账。driver/discovery上轮只读Discover，本批全读；worker仅必要共享范围，不计整个核心域完成。

| 主链 | 当前源码事实与本批实证 | 余项 |
| --- | --- | --- |
| KV/Lease/Txn driver | clientv3直接转发，revisioned prefix snapshot、CAS、租约lost channel；配置/Assembly/KitEtcd装配全读 | 真实KV/租约与断线恢复、CAS未知结果；Kit仅编译/vet |
| Discovery | 注册lease/Put/KeepAlive→lease loss重注册→Deregister/revoke；serviceWatcher错误/通道/取消全读，复用现有包回归 | 生命周期lock/等待预算、真实重注册/租约切换仍缺 |
| Election | session/campaign/leadership/fence/Resign状态全读；实际gRPC LeaseGrant取消反例 | NC-11未修；完整etcd选主/隔离/fencing写及清理预算未验 |
| Watcher/WatchCallback | Created/compaction/error/readiness、取消解阻塞与回调所有权 | readiness三控制、128事件/64队列关闭控制通过；第三方关闭预算NC-12未修 |
| LocalMirror/订阅 | snapshot→revision+1 watch、重快照/错误状态、clone隔离、CAS、bounded slow subscriber | 本批read copy/CAS/watch才改本地/旧revision控制通过；真实compaction/重连、总订阅容量仍待验 |

新增[NC-11/12两个未修P2](../bug/REVIEW-2026-10-04-noncore-08.md)。8个新叶子/独立项=2行为失败/6控制；etcd、etcd/driver两测试包race45 test pass事件、0测试fail/skip，KitEtcd无测试，仅包编译及三包vet通过。真实etcd集成选中1项，PATH缺etcd而skip。[实跑/复跑](evidence/noncore-review-20261004-08/README.md)。没有共享生产资源、未回收的测试进程或真实集群认证。

Tier2 Verify、roost-core root/ready确认，generation仍`2026-09-30T11:58:14Z`，metadata_changed/new probes not_tracked以当前源文补证。File查询26个etcd路径/KitEtcd1，分页has_more=false；生命周期/镜像/选主定向查询14和12节点完整。Campaign、CloseWithContext、reload、Subscribe双向depth1和关键片段核对；heuristic误连到其他包不作调用事实。15生产/Kit路径加etcd/Kit scopes及7追加证据路径coverage无记录gap，不等于索引/源码穷尽。依赖NewSession在本地固定版本client/v3@v3.7.1直接阅读，不声称仓内图谱覆盖依赖源码。[coverage](evidence/noncore-review-20261004-08/graph-coverage.json)。

[学习文档](IMPLEMENTATION-ETCD-SNAPSHOT-WATCH-AND-LIFETIME.md)说明当前一致性、状态/资源责任和拟议修法，不把未实施方向写成产品。39/39只是固定清单源码读取，未建立N03完整正常/恢复/并发/容量矩阵，不计completed/15或全仓逻辑覆盖率。

下一新范围 **N04 redis/mongo/cache/migration**，复用Service驱动专项，只补事务/批次、游标、缓存过期/容量、回调与迁移失败缺口；N03同时保留两新RR、真实NATS/etcd、关闭fallback和选主清理余项。用户要求bugfix时先修新RR，用户说没有修复时直接进入N04，不重跑旧验收。[进度](PROGRESS.md) · [计划](NONCORE-REVIEW-PLAN-2026-10-03.md)。
