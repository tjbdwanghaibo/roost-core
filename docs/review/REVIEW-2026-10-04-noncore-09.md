# 第五批修复：NC-11/12 etcd 生命周期

2026-10-04。fetch/pull --ff-only后main仍为`3560a19b5c2b2fbeb936bf1bb6a9c1fa0be1f956`，开工干净。按用户“修复，并继续review”先关闭上轮两个RR，再推进[N04](REVIEW-2026-10-04-noncore-10.md)。没有发版/tag/部署，没有修改另一线Nest/Sync/DataEngine。

使用roost-bugfix、roost-review与仓库roost-coding。仓库三个同名规范skill与本机副本SHA256一致，无需同步。graph project=roost-core，Tier2，root D:/whb_s/cube-core，generation `2026-09-30T11:58:14Z`；ready不等于最新。search_graph定位→双向trace→snippet→全部证据路径coverage，metadata_changed与新测试not_tracked均回读当前源文；重名Close及跨包启发式边不作为确定调用，不作全域无问题结论。

| 修复 | 行为与兼容 | 实际证据 |
| --- | --- | --- |
| [NC-11](../bugfix/RR-20261004-NC-11.md) | setup取消桥成功前解除；成功session由client生命周期维持；context与Revoke唯一责任，错误原因保留 | SDK→本机真实Grant取消/超时；成功后取消、失败竞争、已死session/fence、Done先于Revoke控制 |
| [NC-12](../bugfix/RR-20261004-NC-12.md) | 唯一watcher关闭任务；caller取消仅结束等待，Done等handler+watcher；底层Close错误不再吞 | 原阻塞Close门闩、8caller、双收尾/重试/双错误、父取消先清理 |

修前8独立/叶子2行为失败6控制；中间一次夹具签名编译错误、一次deadline原因行为失败均保留，补齐后最终15正式项通过。最终两测试包race61 test pass事件/59叶子/独立项、0fail/skip，KitEtcd仅编译/vet；三包vet通过。原overlay8项也通过（在最后原因补修前），最终正式覆盖对应场景。[完整日志、复跑及最终source hashes](../bugfix/evidence/noncore-bugfix-20261004-05/README.md)。

NC-11/12已修复、声明场景验证、未发版；N03依然39/39源文已读且场景部分完成。正常Resign的TTL级Revoke、未知服务端Grant与失败清理、真实etcd/NATS及HA/容量仍留[台账](../bug/CARRYOVER.md)。本机无gh，未读取GitHub CI；这不等于CI通过。

[机制更新](IMPLEMENTATION-ETCD-SNAPSHOT-WATCH-AND-LIFETIME.md) · [进度](PROGRESS.md) · [继续N04的新问题与交接](../bug/REVIEW-2026-10-04-noncore-10.md)。
