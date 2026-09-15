# EntitySync：订阅、批次准入与持久化

首次审查 Core faf4631；09-15 第四轮在 b205653 补充失败交接与生命周期。历史问题是否修复与本轮机制阅读分开记录。

SubscriptionCoordinator 把 membership 独立于实体内容保存。Subscribe 按 subject 分片锁串行操作，先登记 pending，再 CaptureSnapshot 和 sink 准入，成功后 active；失败恢复旧订阅或删除新记录。profile 改变也走快照路径。Unsubscribe 先 closing，Leave 准入成功后移除，失败恢复 active。

DistributeBatch 收集更新并按分片编号排序加锁，读取订阅 profile，构造每接收者的信封，ReservePreparedSubjectSyncBatch 之后一次准入所有信封，成功后提交实体内容并推进订阅 ContentVersion。ReliableEnvelopeSink 必须整批接受或失败，具体 session 序号、历史和运输属于 sink 的职责。

在首次审查 faf4631 中，FlushSubject 先以 LastCommitLSN 对比 durableWatermark；未持久化则跳过并保留 dirty。当时 Subscribe 与直接 Distribute 路径没有相同门槛，RR-20260915-03 已在三入口复现。b205653 的当前源码已加入快照和预备更新的水位检查；本轮按用户要求跳过旧问题验收，不能把源码变化视为修复验收通过。持久化是内容版本的性质，不能只附着于一个便利方法。

资源/性能：64 个 subject 分片锁避免同主体操作交错，但不同主体可能落在相同分片；锁覆盖 sink 准入，慢 sink 会扩大阻塞范围。批量锁排序有助于统一获取顺序；同 profile 的预备 payload 可复用给多个订阅者。当前没有压测或字节开销测量，不把这些源码成本分析当性能缺陷。

## 失败、取消和所有权交接（b205653）

Subscribe 失败时，新订阅从两个索引移除；profile 切换失败恢复原订阅。Unsubscribe 失败恢复 active，但 revision 可以推进，调用方不应依赖失败前后的 revision 相等。admitEnvelopes 在调用 sink 前检查 context 并恢复 sink panic；已经部分交付再返回错误不在协调器可撤销的范围内，sink 必须兑现整批原子契约。

DistributeBatch 先检查活动 profile 是否都有 prepared 更新，再 reservation 和调用 sink；缺 profile 或准入失败不消费 dirty。批次提交通过 dirtyGeneration 判断本批准备后是否又有变化：旧内容版本仍提交，新变更保留给下轮。新场景验证上述失败后的再次 Prepare/重试没有 reservation 泄漏。

锁在外部调用期间保持。subject 1 与 67 实测落在同一分片 38，取消不能打断 Mutex.Lock；不同分片对照可以返回。应先约束 sink 的准入等待时间，再评估可取消锁，不能随意移出锁内调用而破坏顺序。未做吞吐或生产尾延迟测量。

SetSink 只在元数据锁下替换引用，在途操作仍可使用此前取得的旧 sink；它不是排空屏障，也不自动回放既有订阅快照。需要热迁移时，宿主应先确定谁负责历史和路由衔接。Close 获取全部分片锁后清空 membership，且注释要求宿主先停新操作、等待在途完成；本轮验证关闭后的批次被拒且不消费 dirty，没有验证真实服务停机流程。

本轮 17 个新受控场景与相关包 race 通过，不能推出所有调用交错都安全。[第四轮证据](REVIEW-2026-09-15-04.md)。下一步真实 sink 原子准入、room flush 重试与背压，然后 syncstream/syncbus。[首次水位证据](REVIEW-2026-09-15-03.md)。
