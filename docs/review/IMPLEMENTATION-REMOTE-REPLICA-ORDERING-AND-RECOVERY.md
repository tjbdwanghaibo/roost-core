# Remote 复制：身份、顺序与恢复

2026-09-13，Core `617738b1cb61f8a4f35f6d5e8365d2f525a08b0b`。这里解释当前实现及本轮验证，不代表修复方案已实施。关联[问题](../bug/REVIEW-2026-09-13.md)、[测试源码](../bug/REPRO-2026-09-13.md)、[上轮 Mirror 方案](IMPLEMENTATION-REMOTE-POLICY-MIRROR.md)。

## 现有数据链

`remoteSyncer.PublishRemoteSnapshot` 先问 interest registry 是否有人关注完整 key；有兴趣才将 RemoteSnapshotRecord 克隆进 remoteSnapshotWire，然后放入 mirror.Envelope，由 Replicator 生成独立 MessageID 并发布 SyncMsg。接收时顺序反过来：bus → Replicator → SnapshotReplicaStore → RemoteSnapshotCache.ApplyUpdate。

三层身份各有职责：SyncMsg.MessageID 标识一次投递；Envelope.Key 是完整 key 的 hash，用于传输身份域；RemoteSnapshotKey 里的 tenant/entity/kind/scope/policy 才是实际快照身份。StateVersion/MarkerEpoch/RouteEpoch 决定快照版本。即使第一、二层一致，也不能省略第三层校验。本轮 RR-20260913-03 就是通过前两层后向另一 scope 写入的实测。

缓存对仍在 L1 中的旧 upsert 有版本保护；冻结 payload 的复制读取防止业务修改缓存数据。删除目前是裸失效，没有保留水位，因此版本保护随值一起消失。版本检查与应用应该属于同一并发边界，不能在订阅回调外增加一次读比较就声称原子。本轮 RR-20260913-01 验证了两个相反方向的失序。

## 兴趣的资源所有权

Manager 的 localInterests 记录本服务认为已经发布的 lease，合并频繁读取产生的续租；remoteInterestRegistry 保存 key→SID→ExpiresAt，用来决定发布者是否跳过推送。两张表的职责不同，网络消息可能让它们暂时不一致。

renew 只延长同 SID 的过期时间，release 却无条件删除。故延长本身幂等不等于整个订阅协议抗重排。RR-20260913-02 表明旧 release 可以撤销新兴趣。修复应区分“会话/订阅代际”和“lease 过期时间”；不能直接拿 release 时间与 lease 到期时间比较大小，否则正常 release 也可能被错误忽略。

兴趣是有界软状态，不是数据持久性证明；缺失兴趣会影响更新及时性，应依靠明确定义的缓存陈旧策略、恢复加载和续租来补偿，而不是让业务误以为一直收到所有更新。

## 缺口恢复及传输错误语义

SnapshotReplicaStore 遇到 delta 的 base gap、epoch/schema 不匹配时执行 `LoadAuthoritative(..., RemoteReadMonotonic, update.StateVersion)`。本轮构造缺失 base=2 的 delta v3，真实缓存回调加载一次权威 full v3，成功恢复；loader 是确定性替身，未验证数据库一致性。

缓存的权威加载有并发 slot 与 timeout；相同 key/minVersion 的 monotonic 读另有一层 `loads` 合并。该层独立于通用 ReadThrough，也需要独立管理取消生命周期。RR-20260913-04 说明底层已修复等待名额，并不会自动修复上层自建的计数器。

当前 room NATS wrapper 只记录 handler 错误；JetStream wrapper 也返回 nil，让适配层把处理视为成功。`syncbus.Handler` 明确说明报错不会重试。因此既有 gap 回填路径的“调用一次 loader”不等于“直到恢复成功”。连接恢复、回填失败后的重试、owner 换代应有独立状态机；本轮只审查这一接口事实，未把尚未实现的自动恢复重复报成 bug。

## 设计与性能含义

- 版本墓碑与订阅 generation 是正确性状态，不应被普通 LRU 淘汰后直接允许旧消息进入。容量限制需要配合重同步策略。
- 每个消息都做完整身份校验只涉及固定数量字段与 hash，通常比反序列化/网络便宜；仍应测实际 payload 与扇出，不能虚构成本倍数。
- 全量快照提供简单恢复路径；增量降低带宽，但增加 base、schema、epoch、回填失败和队列背压的状态组合。Mirror 第一版优先全量的建议维持。
- 陈旧缓存命中可以避免 I/O，但资产与竞争结果不应靠这种视图裁决。调用者要选择缓存读或权威读，并明确携带观察版本的语义。

后续验证顺序建议：先修复并保留本轮七个场景；再测真实 L2 CAS/删除交错、真实 broker 的旧消息与重连、gap 回填失败和进程重启的版本水位。Linux 压测需报告热点、payload、扇出、超时比例及内存，不用包测试通过替代可靠性结论。
