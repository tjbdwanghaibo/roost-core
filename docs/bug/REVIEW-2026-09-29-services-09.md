# Service 第九轮：补齐 Cluster 读取、历史成本与消费边界

源码基线 `bcebb80568559dcf895db0683cb85eee75b9ef53` 加[第七批修复](../bugfix/SERVICE-BUGFIX-2026-09-29-07.md)。本轮先修 RR-31/32，再审查；新问题仅记录，未修改 Mail/Redis 生产实现。Service 的 10 域主链与本轮四项本机专项已完成有界整理，仍有以下新缺陷，不能称全 service 无 bug。

## RR-20260929-33：Mail 在 Cluster 上接受配置，但正常多封分页失败

**P2；已确认，未实施。**位置：`kit/service/mail/mail_mod.go:53` Init、`service/mail/service.go:465` List、`service/mail/redis_store.go:212–234` GetMany、`redis/driver/client.go:108–115` MGet。图谱 Verify，当前源文件补读；实际三 master Cluster，不是 Redis mock。

触发：`redis.cluster_addrs` 有效，`mail.key_prefix` 非空但无有效共同 hash tag；同一玩家邮箱有两封仍可读的邮件，信封 key 分属不同 slot。Init/Provide 和两次 Send 都成功；List(limit=1) 返回一封，List(limit=2) 却报 `mail: batch read 2 envelopes: CROSSSLOT Keys in request don't hash to the same slot`，整页不可用。同拓扑带 tag 控制返回两封。默认页上限不能消除此错误；既有单机集成测试通过也不能证明 Cluster 分页正常。

根因：Mailbox/SendLedger 的单 key CAS 不要求同槽，但信封页仍调用原始多 key MGET。go-redis ClusterClient 没有将此命令拆成各 slot 的 GET；所有信封共用一个业务 prefix 不代表它们自然同槽。此前第八轮只指出这段待补查，本轮补上实际反例，没有重复登记 Activity 的写入问题。

证据：[可复跑源码/脚本](../review/evidence/service-review-20260929-09/README.md)、[真实结果](../review/evidence/service-review-20260929-09/RESULTS.json)。固定 mail id `review9-mail-1/2`，两种前缀分别运行；先核验 Mod 准入，再用相同公共构造器及可注入 NewMailID 重建服务，避免随机 id 的偶然同槽。反例 1 fail，tagged 控制 1 pass；编译/环境失败不算问题证据。

实施建议优先复用框架现有工具：在 `redisEnvelopes.GetMany` 使用有界 `IRedis.Pipeline`/`FutureBytes`，最多 MaxPageSize 个单 key GET、一次 Exec、按位置收集。这样无需把整个 Mail keyspace 强制集中到一个 slot，也不需要引入另一套 Redis 客户端。当前窄 envelopeClient 要补 Pipeline 能力并更新实际执行测试替身；空批次提前返回，ErrNil 转缺失，空字节保留为坏信封，任何命令/解码错误返回页错误，不静默成功。Cluster 下是每节点批次，不能承诺跨所有节点只发生一次网络往返或跨节点原子快照；信封不可变/自身 TTL 的当前模型并不依赖这种快照。

本轮已实际运行 Pipeline 候选：无 tag 的跨槽二进制、空字节、缺失定位正确，WRONGTYPE 在 Exec 和对应 future 上保留为错误，其余 future 可读取。**它是方案证据，不是 Mail 已修复。**不建议贸然改变通用 MGet 的所有调用者语义：原始 MGET 对非字符串 key 与 GET 的 WRONGTYPE 行为不同。另一较小方案是在 Mail Mod 复用 ValidateClusterKeyPrefix，但会让旧配置启动失败，并需迁移旧信封/邮箱/发送幂等记录；集中 slot 的负载取舍须明确。

验收：真实 standalone/Cluster、tagged/untagged、同/不同节点、单/多页、present/absent/empty/bad JSON/wrong type、过期和中途读失败，正式 Mod→Send→List 与生成 RPC；维护原页面上限及缺失计数。保持现有 key 格式的 Pipeline 方案不要求数据迁移；混合旧 Mail owner 仍可能发出失败 MGET，全部读取 owner 升级后才兑现修复。

## 观察与去重

**2026-09-29第八批更新：RR-33已实施并通过原触发与声明场景。**[修复/兼容/红绿证据](../bugfix/RR-20260929-33.md)。上文“未实施/候选”保留原时点；现有Mail采用Pipeline并逐future检查。新独立[RR-34](REVIEW-2026-09-29-services-10.md)另行交接，不推翻本次Mail修复有效性。

- Match 终态 Tickets/Requests、完整队列 clone/JSON 重写的成本延续 [09-17 观察](REVIEW-2026-09-17.md#观察项终态历史保留使队列空了但聚合状态仍增长)。本轮补 Memory/Redis 64/256/1024/4096 历史规模、各 16 次 Enqueue/Cancel，最终 Waiting=0 仍保存历史。未测出规定 SLO 失败，不重复分配 RR；结果和归档约束见[机制学习](../review/IMPLEMENTATION-SERVICE-FINAL-SPECIALTIES.md)。
- PurchaseDrain 的 HGetAll 与永久 ClaimPurchase ledger 没有履约阻断/归档协议，沿旧观察；本轮 RR-32 修复只固定首次仍存在的 outbox 字段。不能据此删除 ledger 或把 delivered 改解释为资产已入账。
- Session 释放回调“exactly/at most once”注释过强，真实调用可重复，资源端须持久且并发幂等；沿第三/五轮契约观察，不把 demo 日志当 allocator 验收。真实支付、资产/房间、HA/强杀与长稳没有执行。
- Wanted 当前标题均已有分流/判定，无新活动候选。本轮结论不改变其他 agent 的模块或 K1/K2/K3 专项状态。
