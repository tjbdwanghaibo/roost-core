# sync/ —— roost 的同步块

roost core 的三块基础之一（另两块：nest 调度、dataengine）。这个目录只是收纳，**没有 Go 文件**，所以不存在叫 `sync` 的包，也不会与标准库冲突；import 的是它下面的包。

两条互不相干的轴：

| 轴 | 包 | 一句话 |
| --- | --- | --- |
| 服务 → 客户端（实体复制） | `entitysync` | 机制：进程一个 `Manager`，subject 自己持有订阅者，每会话一帧，prepare/commit 两阶段，held/ready 会话，持久化门槛 |
| | `entitysync/policy` | 组织：谁订谁——`Interest`（AOI + 关系源）、`Group`（全互见）、`Direct`（显式绑定）；只调 `Subscribe / Unsubscribe` |
| | `frame` | 帧格式：`Frame` 的 `Encode / Decode`、对象 / 组件 delta、`Limits`。不知道会话和传输 |
| | `nettransport` | 传输：UDP / KCP / QUIC、AEAD、`AsyncTransport`（reliable lane 给 entitysync，datagram lane latest-only）、`SessionID`、分片头。不知道帧里有什么 |
| | `lockstep` | 帧同步（输入帧）：与状态同步并列的另一种模型，共用 `nettransport` |
| 服务 ↔ 服务（总线） | `syncbus` | `ISyncBus` 契约、`DeliveryIDs`、`PatchSyncer` |
| | `syncbus/driver` | NATS（至多一次）与 JetStream（持久、确认）实现；kit 的 `SyncBusMod` 二选一装配 |
| | `syncbus/mirror` | 在总线上的副本复制器（`Envelope` upsert / delete）；`cache.ReplicaSyncer`、`remoteentity` 的快照发布用它 |

依赖箭头只有一个方向：

```
policy → entitysync → frame
                    → nettransport ← lockstep
syncbus/mirror → syncbus ← syncbus/driver
entity（内容层，在块外）← entitysync        spatial（基建）← policy
```

不在块内、但常被一起提起的：`entity/subject_sync.go`（内容层归实体）、`spatial`（几何基建）、`syncstream`（有序持久流，给 skill 用）、`remoteentity`（跨服实体协议，dataengine 块的消费者）。

文档：[ENTITY_SYNC.md](../ENTITY_SYNC.md)（怎么用）、[ARCH-10](../docs/bugfix/ARCH-10-sync-manager.md)（为什么是这个形状）、[ARCH-12](../docs/bugfix/ARCH-12-sync-package-layout.md)（为什么是这个目录）。
