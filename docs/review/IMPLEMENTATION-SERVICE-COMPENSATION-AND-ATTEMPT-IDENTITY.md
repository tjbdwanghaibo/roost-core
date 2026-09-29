# Service 学习：提交未知、预约身份与对象所有权

**后续实施更新（2026-09-29）**：RR-19～22 已实施，[修复与验证](../bugfix/SERVICE-BUGFIX-2026-09-29-03.md)和本文末尾的实际实现为最新交接；以下至“性能与接入评价”保留原 `83c04243` review 时点，不把当时建议冒充实现。

源码时点 `83c042438ea9ae235cfe3a41a1a7217949139670`，2026-09-29。[运行证据](REVIEW-2026-09-29-services-03.md) · [问题与验收交接](../bug/REVIEW-2026-09-29-services-03.md)。以下先解释当前实现，建议均未实施。

## Account：多记录创建不等于一个事务

`kit/service/account/service.go:232` 的 CreateRole 串行执行 slot 排他、directory 名字预约、Allocator 分配 ID、角色 Create、名字 Commit、slot 回填 PlayerID。slot 是账号/区服唯一角色的排他记录；directory 的 reserved 带 Token/期限，committed 名字没有预约期限；角色与 slot 不设 Redis TTL。

本轮源码的最后 slot.Update 已在错误后读回，保护 RR-12 的“写成但回复丢失”场景。此前的 Slot.Create、Role.Create、Names.Commit 仍缺相同语义：接口返回 error，并不能告诉服务后台是否已经执行。补偿又是几个独立写入，补偿失败也不是原事务不存在。

这解释了永久空 slot、同名孤儿角色、无角色的 committed 名字三种反例。空 slot 不能直接按年龄删：仍活跃的创建者与恢复者必须有操作身份/所有权协议。提交名字后误用 Cancel 会拒绝，不能期待 reserved 的 TTL 解救 committed 状态。

建议以现有 versionstore CAS 保存建角 intent，包含稳定 operation ID、账号/区服、计划 PlayerID、名字 Claim Token/Owner、已确认阶段；用确定 ID 重试插入，用 Get/Lookup 确认实际结果，以 ConditionalDeleter 保护补偿身份。状态不能判定时保留 intent 并暴露对账入口。各阶段业务成功/可补偿/未知的处理表需要先定；CAS retry callback 内不要分配 ID、调用外部服务或执行另一个不可撤销写入。这里没有引入已实现的恢复 worker 承诺，也没有建议跨任意 Redis key 硬拼一个事务。

## Mail：发奖身份与一次预约身份分开

`service/mail/service.go:745` 的 ReserveClaim 把稳定 Token 留在 Mailbox Entry 上，重试租约仍复用它；ClaimAttempts 与 Deadline 每次预约递增/更新。稳定 Token 让“发奖已完成、确认丢失”的重试仍被业务发奖方识别为同一次奖励。

`CancelClaim:976` 当前只比较 Token 就清 Deadline，因而发奖身份与预约控制身份混用：A 与接替它的 B 是同一奖励、不同尝试；A 的迟到取消不应影响 B。

建议保留 delivery Token，新增/采用 attempt generation，用 CAS 比较后取消该次预约。新协议必须同时更新 typed RPC 和 game 领取调用者，保证旧接口无法悄悄绕过 fencing；不要每次换发奖 Token。迟到 Commit 可能是对真实奖励的确认，与“取消旧 attempt”的权限不同，需要按业务证明分析。

`service/mail/mailbox.go:82` 的 expireEntries 只清已知期限且没有未结算证明的记录。200 条 Reserve 后 Cancel 的过期记录会阻塞新 Send，属于保守安全取舍：Cancel 只说明预约释放，不证明发奖没发生。对账后显式处置能恢复容量；Delete 也应作为业务决策而非无条件扫尾。建议监控 pending claim 年龄/数量、容量拒绝，给人工或业务协调器提供确认后清理入口。正文 TTL、幂等凭证保留期、邮箱容量是三个不同窗口。

## Directory：版本不是永久身份

`kit/service/directory/store.go:211/263` 在 Delete 之前验证 Token/Owner，版本匹配能阻止普通更新，却阻止不了删除后重建同版本。版本在当前存在的记录生命周期内递增，不是跨消失/重建永久增长的 incarnation。

现有 `versionstore.ConditionalDeleter.DeleteIf` 是合适原语：Memory 在同一个锁内检查版本和 predicate；Redis 读取当前 raw/版本/身份，再 Lua 比较同一 raw 并删除、维护索引。因此它保证 predicate 检查的正是被删除的值。

Directory 当前调用普通 Delete，predicate=nil。此时 Redis 的 Lua 依然原子，但原子删除的是重新读取到的 B；旧 caller 只持有一个与 B 相等的版本。修复重点是把原来的 Token/Owner/状态要求送入真正的删除边界，而非再加一次外层 Get。

建议复用现有能力并要求后端支持；Release 同样绑定读到的 Token，防同一 Owner 换代。跨租约/跨重建操作均需明确“业务 owner”与“这次占有身份”，不能以相同业务 owner 推断相同操作。Session 已使用该原语，Directory 是本轮新发现的遗漏消费者。

## 输出值是另一种所有权边界

Go struct 值复制不复制 slice/map 指向的数据。Account.UpdateProfile 复制输入，却把同一 Profile 切片同时保存和返回；ValidateSession 返回存储值也共享切片。MemoryStore 是浅值存储，因此返回值的后续修改绕过 CAS；Redis codec 的序列化使持久值隔离。

建议服务层提供 Role.clone，把存储拥有的 Profile 与调用者拥有的 Profile 分开，并核查所有公开输出入口。不要把 Redis 对照通过外推成 Memory 也安全，不要仅在 RPC 层复制而忽视本地 owner 调用。

## Session：记录完成不等于外部回调只调用一次

`service/session/service.go:650` releasePending 先调用 Releaser，成功后才 markReleased。这能重试失败；同时意味着回复丢失和重叠 Finish 都可能再次调用回调。当前接口要求容忍已释放资源，源码“at most once”注释需要更精确的解释。

本轮 Memory 门控观察到两个 Finish 对同一资源调用两次回调，未接真实 allocator。接入方应按 runID + resource kind/id 实现并发幂等释放，不能只做进程内一次标记；外部释放成功但 markReleased 失败的恢复同样需要该契约。加一次 CAS 抢释放状态仍需回答抢占者崩溃如何恢复，不能据此宣称跨系统 exactly-once。

## 性能与接入评价

目前复用版本存储、目录预约和 mailbox proof，业务规则可在 owner 内集中，减少多处手写补偿；这套基础适合继续做通用游戏框架。当前风险集中在多个持久对象之间的结果不确定，以及同一幂等身份被用于不同控制目的。

新增 intent/attempt 字段会增加持久化和对账成本，但能让故障恢复有明确对象；先证明正确性，再量化热点 CAS 重试、Mailbox 大对象克隆/序列化、pending 积压和扫尾耗时。本轮只验证 200 条容量边界，**没有测 p95/p99、吞吐或证明性能回归**。

## 本批修复后的实际实现

基于 23f92d73 的本批修复提交，详见 [四项实施](../bugfix/SERVICE-BUGFIX-2026-09-29-03.md)。原先的“建议未实施”是前半历史时点，此节解释当前代码。

Account 使用现有 Slot 的 RoleCreation 值保存计划，先预分配 PlayerID、insert-only 保存计划，再预约名字、CAS 保存 Claim/Admitted、Create 角色、Commit 名字、发布 slot.PlayerID。角色带同一 CreationID。没有新增持久 store、后台 worker 或跨 key 事务。

\`\`\`mermaid
flowchart LR
    I["slot 持久计划"] --> A["保存 Claim / Admitted"]
    A --> R["固定 ID 创建或读回角色"]
    R --> N["提交名字"]
    N --> P["发布 slot.PlayerID"]
    E["写入结果未知"] --> I
    P --> S["同名重试返回原角色"]
\`\`\`

图中的阶段是现有记录/事实组合，不是另一个枚举状态机。未知结果保留已有事实，重试从计划继续；Names.Commit 已完成但本调用快照旧时，会读回持久 Claim 验证。名字 Token 更新也通过 CAS 防旧 worker 覆盖新预约。不同创建计划按 CreationID fencing，补偿删除复用 DeleteIf；确定的 pre-role 名字冲突可释放未 admitted slot，确定的 foreign ID 可取消自己的名字预约并释放自己的 slot，普通 error 不走删除角色的补偿。

同名重试为幂等，新 pending 角色不允许 SelectRole/ValidateSession/UpdateProfile；完成后才能使用。旧角色 CreationID 为空保持兼容。旧空 slot 与新 pending 名字冲突不猜测处理，恢复/迁移步骤见 RR-19。严格说没有全系统自动恢复证明，只有正式可重试入口和已执行场景。

Directory 的 Cancel/Release 现在把 Token/Owner/State 放进现有 DeleteIf 的原子边界。New 显式检查能力；Account Slots 同样要求该能力，wrapper 需转发。通用版本 Delete 的契约保持。

Mail CancelClaim 新增 attempts int32，CAS 内同时匹配稳定 Token 和代次。正代次必填；负数/耗尽计数不能回绕。CommitClaim 保留稳定 Token，协议区分奖励确认与取消预约。Mail Go API、typed transport、split 样例、正式 game 模板已同步，旧业务工程 controller 要迁移同一参数。

Role.clone 隔离 UpdateProfile、ValidateSession 和 CreateRole 恢复/完成输出的切片；测试修改返回值后，两个后端的存储和版本保持不变。Memory 的浅复制契约没有偷偷改变。

上面的 pending mail 容量取舍与 Session 回调观察仍保持，未在这批改变。新增 slot admission 和 roleReady 查询带来额外 I/O，尚未测本流程吞吐/尾延迟，不外推历史框架性能指标。
