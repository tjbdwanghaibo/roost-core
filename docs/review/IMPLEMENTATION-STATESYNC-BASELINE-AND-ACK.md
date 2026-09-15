# StateSync：发送承诺与 ACK 基线

09-15（Core f8ee1eb）生命周期补充：NewSnapshot 校验并排序最终对象/组件集合；diff 按标识有序合并生成增删。较小新 ID 会先创建、后删除旧 ID；ApplyDelta 每步检查最终存量上限，满容量合法替换被拒，登记 RR-20260915-01，对象/组件两层均复现。较大 ID 替换、通用组件 schema 和 archetype 变化对照通过。应统一最终集合容量与临时操作资源预算，不取消边界校验。[本轮证据](REVIEW-2026-09-15.md)。以下为历史时点机制记录。

第八轮（Core 215fffa）：LOD 先执行 upstream 和 selector，再按组件 mask/priority 过滤；未到 refreshDue 时复制上一投影的组件，避免误删。当前 refreshDue 用绝对 tick 取模，奇数 tick 发送配 interval=2 可持续冻结普通组件，已确认为 RR-13。修订设计需要组件刷新时间而非简单 Previous.Tick；准备/Abort/Commit 的资源所有权仍需保持明确。[运行](REVIEW-2026-09-14-08.md)。

本轮历史与提交边界：全局 SnapshotRing 与每会话 sent 容量独立；sent 淘汰 ACK 基线后 prepare 回到全量，旧 ACK 被拒。ForceFull 使旧 PreparedFrame 失效，新提交先于旧提交、Abort 后提交均拒绝，四个确定性场景通过。环满后平移/重建索引为 O(capacity)，LOD 逐组件找旧值最坏 O(components²)，未做基准测试。以下保留前轮描述与当时状态。

第七轮（Core a123605）：ForceFull/旧 ACK 的待查项已证实为 RR-11；直接 ACK 和 wire ACK 均可清除服务端恢复意图，有序 ControlResync 拒绝旧 ACK 的对照通过。恢复代际不能只用于拒绝 PreparedFrame 提交，还需保护待履行的恢复要求。[本轮证据](REVIEW-2026-09-14-07.md)。以下保留第六轮机制描述和当时状态。

重组机制补充：assemblyKey 包含 session、room、epoch、tick、sequence；多片准入受全局/单会话条数限制，内容冲突、超长、完成时删除。TTL 以创建时间计，不因重复片刷新；显式 Expire 和多片 Push 执行扫描。逆序/重复、超时后重传、冲突删除三个专项通过。每次多片扫描带来 O(inflight) 工作，尚未做吞吐测试。单片快路径跳过 inflight 表，同时遗漏 MaxFrameBytes，形成 RR-12；DecodeFrame 仍有长度检查，不将重组层失守夸大为全链解码绕过。

审查 Core `2c5469e`；本轮 statesync 与初始 `2c89f3d` 相同。本页区分现有机制和建议。

Publish 将快照放入 ring；PrepareLatest 读取 Latest，SessionState.prepare 在锁内生成 sequence，复制 previous 与 ACK 基线，再运行会话投影、BuildDelta、编码和分片。PreparedFrame 保存投影视图及提交凭证。

Commit 的契约是整批 accepted for delivery，不是客户端已收到；失败应 Abort。BuildLatest 是预览并 Abort，不能建立可 ACK 历史。SendLatest 用 sendMu 串行发送和提交。once 限制重复提交，generation/sequence 拒绝部分过时代际和倒序提交；mutex 保证内存一致性，但不能证明网络 ACK 指向唯一内容。

sent 存会话实际发送的投影而非全局快照，这个方向正确。ACK 验证 tick 已发布、已发送、历史仍在；prepare 在可用时读 sent[ackTick]，否则全量。ApplyDelta 校验房间、epoch、schema 与 BaseTick 后合并对象。当前缺口是同 tick 可以重新投影并覆盖 sent，而 ACK 仍只有 tick；[RR-20260914-10](../bug/REVIEW-2026-09-14-06.md) 证明了静默漏对象。

调用方须区分准备、运输接受、客户端应用三个阶段。现有 generation 没有直接成为客户端基线身份。ForceFull 与旧 ACK 的恢复屏障待继续验证，不将源码疑点写成已复现问题。

性能事实：每会话历史默认保留 64 个 tick 的 Snapshot；prepare 克隆 baseline/previous，commit 克隆投影，ApplyDelta 建对象 map。这提供所有权隔离，但历史字节量随会话数及投影视图体积增长。建议先测历史总字节、同 tick 重发的投影/编码成本及分片分配，再考虑不可变共享或缓存。本轮没有 statesync 微基准、真实网络或生产吞吐数据。
