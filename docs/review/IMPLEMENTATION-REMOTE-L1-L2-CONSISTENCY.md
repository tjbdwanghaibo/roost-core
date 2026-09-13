# Remote 快照 L1/L2：组合后的正确性边界

第七轮（Core `76c6bac`）：实际 Set 错误分类不能被提前 Get 取代；删除墓碑须覆盖 ReadThrough 回填，冷 L1 不能无条件删除新 L2。预检查增加一次 L2 RTT，未实测性能。[机制与交错](REVIEW-2026-09-13-07.md)。

## 09-13 第四轮更新：有效期检查必须覆盖提前返回

Core `e4f07b06cea450dfc4ab22ca8e3f7f39db22b81a` 的 U-0175 为普通缓存读增加 expiry 检查，原 Cached 复现通过。但 Linearizable 直接返回 LoadAuthoritative，Monotonic miss 直接返回 loadMonotonic；loader 返回结果与最后读回 L1 的结果均可能绕过新增检查。新增三场景见[验收](../bug/REVIEW-2026-09-13-04.md)。

因此 read 的公共后置条件应覆盖所有出口，而不是只在一条命中分支检查。Publish 同版本去重还可能保留旧 ExpiresAt；无论是否允许同版本续期，都不能以成功返回过期旧值处理。L2 schema 修复及大计数脚本已在真实 Redis 通过，错误降级与统一 apply 设计仍未实施。下方为最初审查基线的历史机制说明。

审查日期 2026-09-13，Core `2724442d886d9bb3ee5617d7ded814ce0f5267cc`。解释现有机制和实测，修复方向尚未实施。见[问题](../bug/REVIEW-2026-09-13-02.md)、[运行](REVIEW-2026-09-13-02.md)。

## 三条入站路径

1. **Publish**：RemoteSnapshotCache 按 key 取 publish 分片锁，检查当前 L1 的同版本 schema/codec/checksum，再经 ReadThroughStore.Set 先写 L2、后写 L1，最后唤醒版本等待者。
2. **L2 回填**：Cached/Monotonic Get 未命中 L1，ReadThroughStore 合并同 key 的 load，L2 返回值后直接 setLocal。此路径不经过 Publish 的分片锁及同版本内容检查。
3. **权威回填**：LoadAuthoritative 有并发槽与超时，检查最低版本后调用 Publish，再读 L1 返回。Monotonic 的加载合并还有独立 key/minVersion 维度；上轮等待名额问题仍未修复。

单独看 L1 的版本谓词、L2 的 CAS 或 Publish 的冲突检查都像是有保护；组合后，每条入站路径是否执行同一规则才决定结果。RR-20260913-05 是错误类型在层间丢失；RR-06 是另一入口绕开规则；RR-07 是两层规则本身不一致。

## 一致性失败与可用性失败

当前快照缓存固定忽略 L2 错误，为 Redis 暂时不可用时保留 L1 服务能力。测试证明这种网络故障降级正常。但 IgnoreRemoteError 不区分 ErrRemoteVersionConflict，导致 L2=A、L1=B 的同版本分叉。

建议将错误分成可降级的依赖故障与不可降级的协议/一致性冲突，不必为此新造一套缓存。可在现有组合层增加明确错误分类，保证语义冲突返回到 Publish/调用方，并避免写入被拒绝的内容。只观察“Set 返回 nil”和 Redis 存储没被覆盖都不够，还要同时验证 L1/L2 及调用方结果。

## 原子比较必须覆盖回填

ReadThrough 的 L2 Get 与 setLocal 之间可以发生 Publish。当前同版本旧响应可覆盖已发布值，因为 AtomicLocalStore 的 Stale 只判断更旧版本，不判相同版本不同内容。

修复应让比较与写入位于同一原子边界，并使所有入口共享快照专属规则。通用 Store 不必理解所有 entity 协议；可以通过明确的验证/合并接口或实体适配层承载。修改时避免回填再次调用会写 L2 的路径而形成递归或无谓回写；也不要持有全局锁跨慢网络 I/O。

## 解释元数据与过期

payload 字节相同不意味着语义相同，schema/codec 决定如何解码；L1 目前将它们纳入冲突，L2 CAS 没有。两层必须共享相同版本的不可变字段定义，同时说明哪些投递元数据可变化。

ExpiresAt 是绝对有效期，LocalTTL/L2 TTL 是缓存保留策略。读取时间晚于绝对期限就不应因刚回填而再获得完整 TTL。当前字段只被保存，未用于读取准入；实测已过期快照仍返回命中。修复应同时覆盖入口校验和读取期间跨越过期时刻，不能只在 Publish 时检查一次。

## 恢复、性能与后续

权威 loader 首次失败后，下一次读能重新加载成功，本轮对照通过；这证明失败没有永久留下已失败的 in-flight call，不证明系统会在没有新读时自动恢复订阅。持续后台恢复和真实总线故障仍需另审。

分片缓存、加载合并、容量与超时机制仍值得复用。新的冲突规则主要是固定元数据比较，但冲突时重新读取、L2 往返、负缓存和回填风暴的成本需要真实基准。本轮没有测吞吐，不提供性能倍数。

后续依次验证：真实 Lua 对 schema/codec、uint64 数值域和 TTL 的行为；L1 淘汰后 L2 新代际胜出的处理；L2 删除与并发 Set；权威失败、缓存失效与订阅重连组合。不要把本轮 Redis 替身当成真实 Redis 并发证明。
