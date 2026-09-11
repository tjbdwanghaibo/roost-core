# Mail：保留期限、拒绝原子性与对象所有权

基线：Core `4e8f5ece714a848d8b3981333cba22d4e970f57e`；Kit `3855c71f5aaaf5ca4b7091ae17bf8cb0e6943b79`。本文记录现有实现、测试证据与建议，不表示建议已落地。

## 已实现的领取身份保留

ReserveClaim 从不可变信封读取 ExpiresAtUnix，在 Entry 中保存 ClaimEnvelopeExpiresAtUnix。已领取或删除的 Entry 被显示容量淘汰时，仍有 ClaimToken 的记录转为 SettledClaim，并带上信封截止时间。墓碑存在时重复 Deliver 不新建未读条目，ReserveClaim 返回 ErrAlreadyClaimed。

evictSettledClaims 仅按截止时间清理有窗口记录；新墓碑超过 MaxSettledClaims 时 Deliver 明确返回 ErrClaimHistoryFull。计数限制管理资源，时间窗口决定何时可遗忘，这两个条件不能互相替代。没有截止时间的旧记录仍按条数收界，升级期旧身份的残余风险见[U-0171](../bugfix/RR-20260911-01.md)。

容量失败是可观察的业务拒绝，调用方需要决定延后投递或调整保留预算。当前上限是编译期常量；本轮没有修改配置、加入自动重试或证明生产容量够用。过期清理在相应保留路径执行，不能假设时钟一过就已物理删除。

## 一次 Update 的对象流

| 阶段 | 当前实现 | 必须维护的不变量 |
| --- | --- | --- |
| MemoryStore 取值 | 在锁内读取 Versioned[Mailbox]，把 current.Value 传给 mutate | 锁能串行化调用，但结构体赋值不复制 map 内容 |
| Deliver 候选计算 | 初始化 map，插入 Entry、改 Unread，再淘汰并生成墓碑 | 决策被拒绝前的修改应只存在于候选对象 |
| 容量检查 | 墓碑过多返回 save=false 与 ErrClaimHistoryFull | 拒绝后的存储值和版本都保持原状 |
| 保存 | 仅无错且 save=true 时写回结构体并增加版本 | 条目、计数、版本应描述同一次变化 |

当前第二步修改共享 map，第三步返回错误无法撤销该修改；值类型 Unread 与版本却没有保存，导致 [RR-20260911-05](../bug/REVIEW-2026-09-11-03.md)。这不是多线程竞争，也不是“加一把锁”就能解决的问题。

U-0170 在 Mailbox.clone 中复制两张 map，修复的是 Service.Mailbox 对外读取快照。Deliver 的 Update 回调输入是另一条所有权边界。对读取结果做深拷贝，并不自动让写入计算具有失败回滚能力。

## 如何验证拒绝

先用公开 API 制造接近上限的合法状态，保存完整 Mailbox 快照和版本；触发明确错误后重新读取，逐项比较 Entries、SettledClaims、Unread、Evicted、UpdatedAtUnix、Version。版本不增长本身不足以证明状态没有改变。

本轮失败结果为墓碑 200→201、版本 1202→1202、Unread=199 而实际未读条目 200。对照测试只在 Update 回调入口 clone current，其余公开 API 序列不变，拒绝后状态完全不变。它支持“共享可变对象是根因”的判断；对照仅保存在测试 overlay，不是代码修复，更不是真实 Redis 验收。

建议明确 Mutate 的纯函数/所有权约束，并在独立副本上完成可能失败的候选计算。若改为存储层统一隔离，需覆盖任意 T 的 map、slice、pointer，并评估复制成本；不能用只含标量的测试代表所有存储值。此次未穷举其他 service 的同类模式，下一轮可按失败后仍可能保存副作用的入口继续审查。

## Remote / Nest 修复带来的共同经验

Remote Close 的关键是资源交接只有一个接收方：在同一屏障内登记发送者，停止禁止新登记，已登记者结束后才完成排空。Nest 重排的关键是回复所有权随 clone 转移：原 Msg 清空 RetChan，排队成功由停止回收分支回复，停止后拒绝由准入包装层回复。

这与 Mail 的失败原子性对应同一类审查问题：一个操作被拒绝或关闭之后，谁仍持有对象、谁负责终态、哪些副作用已经对外可见。应分别验证成功、拒绝、交接中停止三个阶段，避免只用正常链路推断失败语义。
