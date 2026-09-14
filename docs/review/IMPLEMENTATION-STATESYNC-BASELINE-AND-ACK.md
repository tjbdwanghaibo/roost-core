# StateSync：发送承诺与 ACK 基线

审查 Core `2c5469e`；本轮 statesync 与初始 `2c89f3d` 相同。本页区分现有机制和建议。

Publish 将快照放入 ring；PrepareLatest 读取 Latest，SessionState.prepare 在锁内生成 sequence，复制 previous 与 ACK 基线，再运行会话投影、BuildDelta、编码和分片。PreparedFrame 保存投影视图及提交凭证。

Commit 的契约是整批 accepted for delivery，不是客户端已收到；失败应 Abort。BuildLatest 是预览并 Abort，不能建立可 ACK 历史。SendLatest 用 sendMu 串行发送和提交。once 限制重复提交，generation/sequence 拒绝部分过时代际和倒序提交；mutex 保证内存一致性，但不能证明网络 ACK 指向唯一内容。

sent 存会话实际发送的投影而非全局快照，这个方向正确。ACK 验证 tick 已发布、已发送、历史仍在；prepare 在可用时读 sent[ackTick]，否则全量。ApplyDelta 校验房间、epoch、schema 与 BaseTick 后合并对象。当前缺口是同 tick 可以重新投影并覆盖 sent，而 ACK 仍只有 tick；[RR-20260914-10](../bug/REVIEW-2026-09-14-06.md) 证明了静默漏对象。

调用方须区分准备、运输接受、客户端应用三个阶段。现有 generation 没有直接成为客户端基线身份。ForceFull 与旧 ACK 的恢复屏障待继续验证，不将源码疑点写成已复现问题。

性能事实：每会话历史默认保留 64 个 tick 的 Snapshot；prepare 克隆 baseline/previous，commit 克隆投影，ApplyDelta 建对象 map。这提供所有权隔离，但历史字节量随会话数及投影视图体积增长。建议先测历史总字节、同 tick 重发的投影/编码成本及分片分配，再考虑不可变共享或缓存。本轮没有 statesync 微基准、真实网络或生产吞吐数据。
