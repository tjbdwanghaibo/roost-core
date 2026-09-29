# Service 第三轮：提交未知、预约代次与删除身份

**2026-09-29 后续 bugfix**：RR-19～22 已实施，原触发 Memory/Redis、RPC/并发及正式回归通过，未发版。[最终实现、升级与验证](../bugfix/SERVICE-BUGFIX-2026-09-29-03.md)。下文完整保留修复前 review 的源码时点和结论；旧数据恢复及未测外部系统仍见新交接。

审查源码：`83c042438ea9ae235cfe3a41a1a7217949139670`，Core 单仓，2026-09-29。状态：**4 个新增确认问题，3 P2、1 P3，尚未实施**。上轮 19 项修复的已有回归在本轮通过；下面是邻近的新触发，不把旧修复重新判为无效。

[运行与证据边界](../review/REVIEW-2026-09-29-services-03.md) · [复现材料](../review/evidence/service-review-20260929-03/README.md) · [机制与实施交接](../review/IMPLEMENTATION-SERVICE-COMPENSATION-AND-ATTEMPT-IDENTITY.md)。

## RR-20260929-19

**P2：Account 建角其他写入点的未知结果缺少恢复协议。**

源码：`kit/service/account/service.go:262`（Slots.Create）、`:309`（Roles.Create）、`:324`（Names.Commit）、`:390`（rollback）；`kit/service/account/redis_store.go` 中 Slots/角色持久化不设 TTL。

正常流程是占 slot → 预约名字 → 分配 ID → 创建角色 → 提交名字 → 回填 slot。RR-20260929-12 已处理最后一步的写后未知；当前前三个写入边界仍把错误当作未提交，或者直接放弃恢复：

| 写入已完成、响应丢失的位置 | 当前实际结果 | 业务影响 |
| --- | --- | --- |
| 初始 Slots.Create | 返回错误，但留下 PlayerID=0 的持久 slot；重试 ErrRoleLimit | 玩家无法再次建角，业务时钟推进 24h 仍不恢复 |
| Roles.Create | 取消名字、释放 slot；已写角色没有被协调 | 重试生成另一个 ID；两个持久角色具有相同账号、区服、名字，旧角色成为孤儿 |
| Names.Commit | 删除角色、Cancel 已提交名字、释放 slot；Cancel 返回 ErrClaimStale 被 rollback 忽略 | 名字成为 committed、无角色/slot；原玩家重试 ErrNameTaken，名字没有预约 TTL 可等 |

三个反例均在 **Memory + 真实 Redis** 上执行存储操作后注入一次错误；相同位置的**写前失败**控制用例均能干净重试。这里只模拟“后端已执行、调用层收到错误”，没有进行 Redis 断网或强杀；24h 是业务测试时钟推进，并非真实等待一天。角色孤儿不等于已证明能被正常 SelectRole 使用。

期望：未知提交应进入可查询、可恢复状态；不要永久空占位、抹掉已提交名字关联，或释放排他状态后遗留同名角色。三个点归为同一建角恢复协议问题，不拆成三个 RR。

建议优先用现有 versionstore 的 CAS 和目录 Lookup/Token：给建角保存持久 intent/operation identity、计划 PlayerID、名字 Token、阶段；每个写入错误先核对权威记录。确认提交则继续或恢复，确认未提交才补偿，读取也失败则保留待对账证明。不能仅凭空 slot 就删除，它也可能属于仍在进行的创建。Names.Commit 未知后要区分 reserved/committed 和 Token/Owner；不能把 Cancel 当作 Release。仅做单次读回仍不能补足进程在两步之间崩溃的恢复能力。

验收：三种写后错误、对应写前错误、读回再失败、同账号并发、名字重新被占、进程在各阶段停止后重试；避免恢复者误删另一操作的状态。旧空 slot/孤儿角色/无角色的 committed 名字需要扫描对账方案。关联 [旧 RR-12](REVIEW-2026-09-29-services-02.md#rr-20260929-12)，旧 slot 尾写入修复保留。

## RR-20260929-20

**P2：Mail 旧 CancelClaim 可以解除新一次领取预约。**

源码：`service/mail/service.go:745`（ReserveClaim）、`:844`（Deadline/Attempts）、`:976`（CancelClaim，Token 比较约 999 行、清 Deadline 1007 行）；`service/mail/mailbox.go:62`（预约字段）。

触发：A 预约获得 Token=T、Attempts=1；30s 租约到期后 B 重新预约，仍为 T、Attempts=2；迟到的 A 调用 CancelClaim(T)。当前返回 true，清除 B 的新 Deadline；立即第三次预约成功，Attempts=3，而 B 的期限尚未到。**Memory + 真实 Redis** 均复现。

期望：旧一次尝试不能取消新一次尝试。稳定 Token 是正确的发奖去重身份，不能为了阻止旧取消而每次更换 Token。这里证明的是预约代次被越权取消；**没有证明奖励重复发放**，正确按稳定 Token 去重的发奖方仍可避免重奖。

建议复用现有 ClaimAttempts 或独立 attempt nonce，作为预约控制的身份，与稳定 delivery Token 分开；取消在 Mailbox CAS 内同时匹配 Token + attempt generation。需处理代次字段宽度/溢出、typed RPC 参数和调用者迁移。旧接口只有 Token，无法识别迟到调用，不能用“当前仍未过期”替代代次判断。Commit 的迟到确认可能对应真实已发奖，不能未经分析照搬取消规则而拒绝真实结算。

验收：旧取消/旧重试/并发取消、新预约、同 attempt 幂等取消、已发奖后迟到 Commit；真实业务领取器仍按稳定 Token 去重。协议升级前明确旧调用者的处理策略。

## RR-20260929-21

**P2：Directory Cancel/Release 的版本检查不能阻止删除重建 ABA。**

源码：`kit/service/directory/store.go:211`（Cancel）、`:263`（Release，Delete 在 285 行）；`versionstore/memory_store.go:68`、`versionstore/redis_store.go:377`（普通 Delete）；现成能力在 `versionstore/versionstore.go:69` 的 ConditionalDeleter。

Cancel/Release 先 Get 检查 Token/Owner，再普通 Delete(expectedVersion)。在 Delete 前暂停旧调用：另一相同旧调用完成删除；新 owner B 预约同 key，必要时 Commit；恢复旧删除。Cancel 的 reserved 版本均为 1，Release 的 committed 版本均为 2。旧删除成功，B 的条目消失。

**Memory/Redis × Cancel/Release 四个确定性交错均失败**；不依赖 TTL、不需要猜测时间。Redis Delete 内虽然用 GET 原始值 + Lua 比较删除，但普通 Delete 不带身份 predicate：重新读取到 B、版本仍匹配时，它会原子删除 B。这不是 Lua 非原子，而是删除边界缺少逻辑身份。

期望：校验与删除针对同一 incarnation；旧操作不移除新 owner。建议复用已实现的 **ConditionalDeleter.DeleteIf**，在原子删除边界匹配读到的 Token/Owner/适用 State。Release 也应绑定读到的 Token，仅同 Owner 不能防同一 Owner 重建。Directory 构造/存储接口需明确要求能力，不能在不支持时退回不安全 Delete；保存 Cancel 的幂等语义和 Release 的 owner mismatch 语义。

验收：本轮四场景、同 owner 换 Token、普通版本冲突、身份匹配正常删除、重复取消、Redis 二级索引一致性。与 [旧 Session ABA](REVIEW-2026-09-09-02.md#2026-09-29-normal-finish-aba) 同属身份 fencing 原则，但不同消费者、不同路径，旧 Session DeleteIf 修复不回滚。

## RR-20260929-22

**P3：Account 返回的 Profile 切片可以绕过 CAS 修改 MemoryStore。**

源码：`kit/service/account/service.go:472`（ValidateSession 返回 role.Value）、`:493`（UpdateProfile 的 result=current）；`kit/service/account/types.go` 的 Role.Profile；`versionstore/memory_store.go:27`、`:38` 的值复制。

UpdateProfile 复制输入，但 result 与写入的 current 仍共享 Profile 底层数组；ValidateSession 也直接返回持久 Role。调用者修改返回值 Profile[0]='X' 后，MemoryStore 数据变为 "Xafe"，版本仍分别为 2/2、3/3。两个入口均复现。真实 Redis 同样用例通过，序列化隔离了持久值；typed RPC 返回通常也有编码边界，不能据此声称 Redis 持久数据被客户端直接改写。

期望：公开返回值由调用者持有，修改不应改变服务内部状态。建议使用 Role 的私有 clone，在保存与返回两端分别隔离 Profile，核查其他公开角色读取/返回入口。不要只复制输入，也不要随意要求通用 Store 对任意 T 做反射深拷贝。

验收：输入修改、两个输出修改、后续合法更新的版本/数据、Memory/Redis 对照；并发使用风险可另补 race，当前没有人为制造数据竞争。关联旧 Global map 别名 RR-17，属于另一个公共接口消费点。

## 观察与设计边界

1. **过期、Cancel 过但尚未结算的 mail 会占容量**。200 条附件邮件都 Reserve + Cancel、业务时钟越过正文期限后，新 Send 报 ErrMailboxFull；显式 Delete 一条后，同 RequestID 重试 Send 恢复。Memory 和 Redis 均观察通过。Cancel 不证明外部发奖未发生，保留稳定 Token 是保守安全策略，不能自动删除证明来“修容量”。需要对账后处置的操作流程/指标；本轮不登记为新 bug。
2. **并发 Finish 可对同一资源调用两次 Releaser**。重叠释放回调、尚未 markReleased 时，第二个 Finish 也调用释放；确定性观察为 2 次。接口同时要求容忍已释放资源，因此应按 run/resource 做并发幂等释放。注释中的“at most once”承诺过强；本轮未接真实场景分配器，未证明生产双重 free，列契约与接入提醒。
3. 未做真实支付/发奖、Broker、多进程强杀、Redis HA/断网、容量 p95/p99；不把测试计数当全 service 无问题证明。

建议实施顺序：**RR-21 原子身份删除 → RR-19 建角可恢复 intent → RR-20 预约代次协议 → RR-22 输出所有权**。
