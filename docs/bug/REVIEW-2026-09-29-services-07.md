# Service 第七轮：待办清理竞争与 Cluster 装配

2026-09-29；Core 源码 `1625fee0bf0884792acf8520fd5bb26183dd0b64`，实际审查树 `D:/whb_s/.tmp/review-service-20260928`。本轮只审查和记录，**以下三项均未修复**。上轮四项修复保持原验收状态，不因新触发被推翻。

| 编号 | 优先级 | 确认问题 |
| --- | --- | --- |
| [RR-20260929-28](#rr-20260929-28) | P1 | Platform 清理“无订单”待办时隐藏并发创建的真实已付订单 |
| [RR-20260929-29](#rr-20260929-29) | P2 | Rank 在 Cluster 接受无有效 hash tag 的前缀，正式装配后 Submit 报 CROSSSLOT |
| [RR-20260929-30](#rr-20260929-30) | P2 | Platform 的 Cluster 前缀检查只看左花括号，空/未闭合 tag 仍可启动，回调无法记录订单 |

[运行记录](../review/REVIEW-2026-09-29-services-07.md) · [复现入口](../review/evidence/service-review-20260929-07/README.md) · [结果](../review/evidence/service-review-20260929-07/RESULTS.json)。真实单机 Redis + 三主节点 Redis Cluster；15 个叶子场景，9 pass / 6 预期 fail，无 skip/build-fail/race 报告。6 个失败归属三个问题，不是六个独立 bug。

<a id="rr-20260929-28"></a>
## RR-20260929-28：迟到清理隐藏新的支付待办

位置：[server_run.go:96](../../kit/service/platform/server_run.go#L96) 的 `ErrOrderInvalid` 分支，[redis_store.go:134](../../kit/service/platform/redis_store.go#L134) 的 `RetirePending`，以及 [versionstore/redis_store.go:232](../../versionstore/redis_store.go#L232) 的 `IndexRemoveIn`/第 251 行脚本。

**触发需要已有异常/旧数据状态**：pending 索引含一个暂时没有订单记录的 ID，后台读到它；同 ID 的迟到渠道回调在后台判断“不存在”之后、退休动作之前记录真实订单。普通新订单在健康索引上首次成功发货，不自动触发这个问题。复现把 ghost 作为受支持的恢复入口初态，来源可以是旧数据/人工修复或异常索引；不声称当前 `versionstore.DeleteIf` 仍然是两次非原子删除，它已用 Lua 同时删除值与索引。

受控时序：

1. 实际 `retryPendingOnce → AttemptDelivery → Orders.Update` 返回该 ID 未记录；在实际 `RetirePending` 前暂停。
2. 实际签名回调 `HandleCallback` 创建订单和值同写的 pending 索引。发货端明确返回一次 `ErrDeliveryNotApplied`，订单正确保留为 `reserved`，无未知外部效果。
3. 恢复旧清理。`IndexRemoveIn` 的脚本只执行 `ZREM`，不在同一原子操作里判断订单是否仍不存在。
4. 推进时钟到退避到期，再驱动后台；真实订单仍在、可重试，但 pending 页为空，未发货。重建 Orders/Service 也不能重新发现它。

两条真实 Redis 反例均得到：

```text
pending=[] stored_state=reserved due=true attempts=1 pending_attempts=[] grants=0
```

期望是“旧的无记录判定不能删除新记录的恢复入口”；实际支付记录没有丢，但后台恢复入口丢了。只靠定时重试和服务对象重建无法补发。**渠道再次回调、人工按 ID 驱动或补回索引仍可能恢复**，因此不能写成任何情况下都永久丢单。P1 是支付履约的恢复责任被静默丢失，而非已证明资金扣错/重复发货。

这是 [RR-20260919-07 原 ghost 修复](../bugfix/RR-20260919-07.md) 之后的**新并发触发**：无竞争 ghost 清理控制仍通过；此处需要保持“清理时仍不存在”的条件。旧文档“先确定不存在”只能证明读的那个时点。

实施交接：

- 继续用现有 versionstore/Redis 索引，不加扫描订单全库的恢复器。
- 增加明确的条件退休操作：在同一 Lua 中针对订单值 key 与 pending key 检查 `EXISTS`；值存在，包括不可解码的值，都保留索引；确实缺失才 `ZREM`。两个 key 继续满足 Cluster 同槽要求。
- 单独在 Go 中多做一次 Get 仍有竞争窗口。不要把 `IndexRemoveIn` 的所有调用偷偷改为订单专属规则；Activity 等调用方用途不同，应保留现有原语并审计新操作的接线。
- 交错两方向验收：创建先完成则保留新索引；退休先完成则后续 Create 原子加入索引；ghost-only、损坏值、写后丢回复、重建、单机/带 tag Cluster 控制均保留。索引修复/历史对账单列，不能删除真实订单作为补偿。

<a id="rr-20260929-29"></a>
## RR-20260929-29：Rank 未拒绝 Cluster 多 key 不同槽

位置：[rank_mod.go:56](../../kit/service/rank/rank_mod.go#L56) 的 `Mod.Init`，[redis_store.go:73](../../kit/service/rank/redis_store.go#L73) 的 boardKey/ownerKey，以及第 96/124 行 swap/remove 脚本、第 343 行 Reset。

配置 `redis.cluster_addrs` 为真实三主 Cluster，rank.key_prefix 为普通 `review7:…:rank` 或含空 `{}` 的前缀。`Mod.Init` 仅通过 `mods.KeyPrefix` 拒绝空白，没有同槽准入；`Mod.Provide` 使用正式 Redis driver 并注册 owner capability 成功。接着通过真实 store 调用 `Submit`，两个不同槽的 `prefix:z:<board>` 与 `prefix:o:<board>` 被 Redis 拒绝：

```text
CROSSSLOT Keys in request don't hash to the same slot
```

没有有效 tag 的 key 偶然可能同槽，但配置不能保证所有 board 同槽。不能把无 tag 的所有 key 一概说成必然不同槽。当前两个被测前缀实际返回 CROSSSLOT，tagged 控制使用 `review7:{rank-…}`，提交、重放、分页、Remove 和 Reset 全通过，排除了 Cluster 未就绪/连接故障。

影响：看似装配成功的 rank owner 在合法业务提交时不可用；Ping 健康不证明业务脚本可用。Remove 的双 key Lua 同样需要同槽；本轮直接失败证据是 Submit，不把推导写成已执行每个失败接口。Reset 的批量 DEL 行为还取决于 driver 如何路由，不能据 Submit 失败自动断言 Reset 同样失败。

实施交接：Cluster 模式下在正式 Mod/config 准入检查**有效的共同 hash tag**，错误点名 `rank.key_prefix`；单机保持已有前缀兼容。直接 `NewRedisStore` 的窄接口不携带拓扑，应明确调用方责任或用现有配置能力传递，不能猜测后端。不要为“自动修好”改变所有部署的 key 编码：老榜迁移需要独立计划。所有 board 共用 prefix tag 会集中到一个 slot，是当前保证原子性的简单接法，不能等同均匀水平扩展；如需按 board 分槽，另写兼容设计。

<a id="rr-20260929-30"></a>
## RR-20260929-30：Platform 只检查左花括号

位置：[platform_mod.go:132](../../kit/service/platform/platform_mod.go#L132)。这里已经试图拒绝 Cluster 无 tag 配置，但只做 `strings.Contains(prefix,"{")`。`review7:…:{}:platform` 与 `review7:…:{platform` 均通过 Init/Provide；真实签名回调在订单与 pending 索引的 CAS 中返回 CROSSSLOT，未能记录该支付。

期望在启动时拒绝不能保证共同 slot 的配置。实际错误推迟到第一笔合法回调；这是支付入口可用性问题，**不声称回调返回成功或已经丢失一笔落库订单**。带有效 `review7:{platform-…}` 的正式装配/回调控制通过，交付终态退出 pending 也通过。

实施交接：沿现有 `kit/mods` 配置工具复用可读的 hash tag 校验；按 Redis 对**第一个左括号及其后第一个右括号**的规则检查非空 tag。不能只查有两个括号/任意某一对非空括号：首对为空时，后面的 tag 不能挽救它。覆盖空、未闭合、空首对后跟非空对、有效 tag、单机无需 tag；Rank 同类准入复用此验证。保持订单与索引同一脚本；不以拆成两写规避 CROSSSLOT。

## 观察与未验证项

Rank 的 Remove/Reset 与在途 Add 交错控制通过：旧 base=100，删除后挂起提交 CAS 重读缺失态得到 5，重放仍是 5。重叠的 Submit 可以在线性化顺序上发生在维护之后，不据“删除后又出现条目”本身登记防复活 bug；本轮不承诺 Reset 会永久封禁后续写。

Match 64 次 Enqueue/Cancel，推进一年并 Sweep，Memory/Redis 都得到 Waiting=0、Tickets=64、Requests=64、Matches=0、JSON=23084 bytes；旧 RequestID 仍返回原终态票据。延续 [09-17 已有保留观察](REVIEW-2026-09-17.md#观察项终态历史保留使队列空了但聚合状态仍增长)，不重复新增 RR。Sweep 限制本次解决的过期数量，不限制整个历史 JSON 的读取/克隆；本轮没有正式容量/延迟压测。

仍未验证真实支付渠道/资产回执、replica/failover、网络分区、真实进程强杀、归档迁移和长稳。三个本机 master 的同槽验证不是 HA 验收。
