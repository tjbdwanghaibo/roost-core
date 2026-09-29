# Service 索引维护与 Cluster 原子性

2026-09-29，源码 `1625fee0`；[第七轮审查](REVIEW-2026-09-29-services-07.md)、[问题/实施交接](../bug/REVIEW-2026-09-29-services-07.md)。本文解释现有实现，条件退休及配置修正是建议，尚未实施。

## 索引是恢复责任

Platform `HandleCallback` 先验证签名/输入，再以 OrderID insert-only Create。`NewRedisOrders` 的 versionstore 配置把订单值与 pending 索引交给同一个 CAS 脚本：初始记录入索引、失败退避移动 score、终态退出。后台 `retryPendingOnce` 只从索引获取 due ID，不扫全部订单。

因此“订单保存在 Redis”只满足支付事实留存；“订单仍在 pending”才让后台找到恢复工作。两者必须在所有维护操作中保持关联，不能只保护 Create/Update 正常路径。

| 维护路径 | 当前事实 | 已验证/限制 |
| --- | --- | --- |
| 订单 Create/Update | 值与 index 同 CAS 脚本 | 单机既有集成、有效 tag Cluster 回调控制通过 |
| versionstore DeleteIf | 原始 envelope 比较、同 Lua 删除值及索引 | 当前源码已是原子删除，不沿用旧文档的 sentinel 两写解释 |
| ghost 退休 | 先在业务读中确认缺失，再另调无条件 IndexRemove/ZREM | RR-28：期间 Create 可把真实待办的索引加入，随后被旧清理删除 |
| malformed 延后 | IndexDefer 在同 Lua 内确认 ZSCORE 存在才改 score | 终态已退出索引后调用延后不重建成员；不代表覆盖了所有并发延后时序 |

判定“无记录”有时效。当前程序的问题不是没读，而是读后的事实没有进入最终副作用的条件。采用现有 Redis/versionstore 增加条件退休原语即可；靠重启、单独再 Get、扫描全库或换队列系统都不能替代原子条件。存在但坏格式的订单仍需要人看，不应因解码失败被当不存在。

## 多 key Lua 与装配

Rank 每 board 有 ZSet（member 内包含排序字段与 Brief）及 owner Hash（当前 member 和有限请求 ring）。readOwner 是单 key；swap/remove 是双 key。在单机 Redis 中 Lua 保持原子，在 Cluster 上原子操作必须把相关 key 放进同一 slot。通用连接健康检查没有验证这个业务条件。

Platform 的订单与共享 pending ZSet 也依赖同槽。Mod 已有检查意图，但 `Contains("{")` 不能识别有效 tag。Rank Mod 完全未做 Cluster 条件检查。用实际 service Mod.Init/Provide、生产 driver 与三 master 本机 Cluster，已验证 RR-29/30；未启动完整多进程 app/RPC 网络。

现有简单接入选择是为该 service 配置带非空共同 tag 的 prefix；单机无需强制变更。按 Redis 首对花括号规则做验证，复用现有 `kit/mods`，使错误在启动期点名配置。自动给现存 prefix 加 tag 会换 keyspace；对真实榜/订单不可当作无害修复。需要按 board 分槽时另设计，因为共用 prefix tag 把不同 board 集中到一个 slot，而 Platform 的共享 pending 索引本来就绑定同槽。

## 维护操作不自动成为写屏障

Rank Reset/Remove 删除记录及请求证明；它们不是“封季”或“封禁这个玩家所有未来提交”。在途 Submit 的 expected member/ring 不再匹配时重读，缺失态 Add 从本次 delta 开始。测试旧分=100、挂起+5、删除、恢复，结果=5；再次同 RequestID仍=5。这个时序通过，不需要杜撰 ABA 缺陷。若业务要封季后拒绝所有迟到写，需要单独定义权威 season 状态/fence，现有 Reset 注释没有此承诺。

Match 的 Waiting、Tickets、Matches、Requests 同聚合 CAS 保证不会半提交；代价是状态大小影响每次读写/克隆。已解析源码中 expireLockedLimit 的 limit 限解决数量，扫描仍可穿过所有未过期 Waiting；历史 maps 的克隆不受 limit 控制。本次 64 终态观察只是保留机制证明，不是生产最大队列压测。裁掉历史前先确定查询和请求重放期限，沿已有稳定 ID、versionstore 和显式分页做归档交接。

## 证据如何继续

先修 RR-28 的原子缺失条件，再完善 RR-29/30 启动校验；保留原本的真实 Redis 和 tagged Cluster 正常对照。下一阶段进入真实故障/容量时，分别保存 replica/网络/进程事件和结果证明；对象重建、同槽测试和 268 个已有测试事件不能替代 HA、生产资金或长稳结论。
