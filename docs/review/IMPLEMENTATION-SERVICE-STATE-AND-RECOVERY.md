# 服务状态、去重与恢复：本轮学习及实施方案

对应 [2026-09-29 审查](REVIEW-2026-09-29-services.md)。以下是建议，不是已经实现的功能；优先沿用框架工具，生产改动由实施任务处理。

## 单对象原子性要覆盖完整逻辑身份

versionstore.Update 的价值是读取版本、纯回调、CAS 失败重新读。回调可能多次执行，因此不在回调内发货、释放资源或发送消息，度量也在结果确认后上报。rank 需要自有 Lua 维护 sorted set，但比较 member 字节仅保护分数，不能保护另一个 applied ring；无分数变化也要修改去重状态的 revision。**“主字段没有变化”不代表“整个操作不需要版本”。**

Directory 的 owner 幂等性允许同一 owner 重取旧 claim；account 建角却把这种结果当作本次新获得资源，补偿时释放旧所有权。设计补偿表时逐项记录“本次新建/复用、身份、版本/token、可撤销条件”，而不是只记录当前名称。优先复用 Directory 与 versionstore，在调用方修复 owner 粒度和条件释放。

## 有界数据不能遗忘未完成证明

rank 的 ring 是有限重放窗口，需要公开窗口与编码契约；activity 的 ring 在 ledger 两阶段确认间还充当恢复证据，两者职责不同。后者必须保留尚未确认的 requestID，满容量背压并恢复确认，再释放证明；不能只靠 FIFO。参加者已加分/ledger 未确认的中间态必须在重启后可枚举或从重试证明，不可把 mark 的错误当作“加分没有发生”。

可先用 versionstore 的有界待确认集合及原子索引实现恢复入口，避免引入第二套 store 抽象。若确需跨键 Lua，明确 Redis Cluster 同槽要求、版本更新和其他后端契约，不把 Lua 局部原子性扩大到外部 game 发奖。去重编码须可逆，所有合法 requestID 应满足 decode(encode(id))=id，旧 ring 有明确读写迁移策略。

## 退避、预算与外部结果是三个维度

platform 的 NextAttemptAt 控制下一次准入，MaxAttempts 控制预算；两者都不能证明上一次 Deliverer 已结束。建议分离 attempt generation、inflight/unknown、业务终态和回执。到期后允许接管或停止新尝试，也应保留未知结果待核对。终态转换应条件化；成功/失败迟到回写都检查代际，人工干预必须尊重外部结果不确定性。

activity 的最后一次有效 dispatch 则必须给完 ACK 窗口，不能被立即重复请求关闭。只有截止时间到期且仍未确认，才进入耗尽。测试应固定调用交错，不使用随意 sleep：通道停在副作用前后、可控时钟跨精确截止点、单次失败注入跨状态写边界。

## 清理需要身份、候选与安全回收条件

account 签名 session 的到期在 Validate 时拒绝，不持有需要后台释放的记录；global lease 的过期也可在访问时判断，删除 key 可能丢代际。业务 session 的 run 却持有外部资源，只显示 expired 不能执行 Releaser；应提供有界 owner roster/cursor，或明确让业务提供 Sweep，二者都要可验收。

mail 列表不可见并不代表 Entry 已回收。可以为 Entry 增加 expiry 并在 CAS 准入前有界清理，但保留仍可领取或结果未知的 token。优先复用现有 mailbox/Claim/SettledClaim 机制，避免同时建立重复 TTL 真相。旧数据无 expiry、envelope 丢失和过期领取交错分别定策略。

## 配置也必须维护存储隔离

chat policy 判断的是逻辑 Channel，store 按字符串 key 读写；只有 key 映射单射，逻辑授权才等价于物理隔离。参考已有 match 字段转义，给 kind、scope、参与者等字段可逆编码，并对默认/自定义规则组合做唯一性测试。改 key 会影响历史和消息去重，实施文档必须有迁移或兼容读取边界。

## 实施顺序与门禁

1. platform 终态/结果未知与 activity 未确认进度：先补本轮红测，再补接收端回执、重启和两阶段失败边界。
2. account 所有权补偿、rank 去重编码/CAS、chat key 隔离：明确旧数据迁移，避免修正新写路径后旧数据仍错误。
3. mail 回收、activity 最后 ACK 窗口、session roster 接线：证明有界且可达，记录失败/积压/恢复指标。
4. rank Around 边界与公开文档收口；9 服务 `servicerpc -check`、服务 race + 真实 Redis 集成回归。

压测分别观察热点 owner/queue/group 与大量独立键：记录对象字节量、存储调用数、CAS 重试、backlog 年龄及 p95/p99。应有错误与一致性校验，不能仅统计成功路径吞吐；本轮尚未执行该门禁。
