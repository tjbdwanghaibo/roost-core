# Core Service 第二轮：身份、迁移和失败恢复

审查基线：`e10dd3f18d2da985f969cbc54cb19a87f1f287af`，本轮 fetch 后与 `origin/main` 一致。接续[第一轮](REVIEW-2026-09-29-services.md)，只新增故障分支审查，**没有修改生产代码，也没有验收上一轮十项的修复**。

新增 **8 项：6 P2、2 P3**，均有动态反例。另在正常 Finish 路径复现旧 RR-20260909-02 的 ABA 根因，追加原编号，不重复计为新 bug。复现材料、执行结果和边界见[复跑说明](REPRO-2026-09-29-services-02.md)。

| 编号 | 优先级 | 问题 | 条件与实测后端 |
| --- | --- | --- | --- |
| [RR-20260929-11](#rr-20260929-11) | P2 | 两个已验证身份映射到同一个账号 | 自定义 channel 含冒号；MemoryStore |
| [RR-20260929-12](#rr-20260929-12) | P2 | slot 成功写入但响应丢失，补偿删除角色并永久占位 | 注入提交后返回错误；MemoryStore |
| [RR-20260929-13](#rr-20260929-13) | P2 | 路由迁移完成后，旧绑定 lease 仍可续期 | 正常公开 API 顺序；MemoryStore |
| [RR-20260929-14](#rr-20260929-14) | P2 | Attach 接受调用方伪造的释放标记，Finish 不调用 Releaser | 正常 Resource 请求字段；MemoryStore |
| [RR-20260929-15](#rr-20260929-15) | P2 | 合法正增量累加回绕为负值 | int64 上界；MemoryStore |
| [RR-20260929-16](#rr-20260929-16) | P2 | envelope 首写瞬时失败后，同一邮件请求无法恢复 | 注入写入前单次错误；内存 helper |
| [RR-20260929-17](#rr-20260929-17) | P3 | lease 返回的 Load map 可直接污染内存存储 | MemoryStore；未推定 Redis 同样受影响 |
| [RR-20260929-18](#rr-20260929-18) | P3 | Finish 重试释放成功后仍遗留 owner claim | 单次 Releaser 失败；MemoryStore |

实施建议优先处理旧 ABA 新触发、RR-12、RR-13 和 RR-14；RR-11 按部署实际 channel 命名评估紧急度。上一轮 P1 支付终态与未确认进度问题仍优先，不能被本轮覆盖。

## RR-20260929-11

**P2：合法的不同已验证身份共享账号键。未修复。**

位置：[AccountID](../../kit/service/account/types.go#L185)、[Verified.Validate](../../kit/service/account/identity.go#L40)、[Login](../../kit/service/account/service.go#L173)。`AccountID` 用 `channel + ":" + openID` 拼接，校验不禁止 channel 中的冒号；Login 读到旧账号后没有比对其原始身份。

通过两个分别验真的 credential，让 verifier 返回 `(vendor:a, b)` 和 `(vendor, a:b)`。两次 Login 都成功返回 `vendor:a:b`，第二次返回的 Channel/OpenID 仍属于第一个身份。未把客户端自行提交未验证身份当作触发条件。

影响是账号身份隔离失效；本测试只证明账号合并，没有继续生成角色会话或演示实际账号接管。需要含冒号的已验证 channel；固定且无冒号的渠道集合不会触发这个具体例子。**配置条件不能由当前无此配置推导为公共 API 没有问题。**

实施：使用可逆单射编码或长度前缀；保留既有 channel 大小写规范化策略。已存在 AccountID 被 roles、slots、session 等引用，必须给迁移/兼容策略，不能只替换拼接然后创建一批新账号。旧键读取至少验证存储身份是否匹配，冲突拒绝并交人工处理，不能静默并入。门禁包括两个合法冒号身份、普通渠道对照和历史键迁移。

## RR-20260929-12

**P2：建角提交结果不确定时，回滚制造悬空 slot 并阻止重试。未修复。**

位置：[CreateRole 尾部写入和补偿](../../kit/service/account/service.go#L315)、[releaseSlot](../../kit/service/account/service.go#L379)。slot 初建 v1，尾部 Update 把 PlayerID 写入形成 v2；如果写入已生效但响应报错，失败分支释放已提交的名字、删除 Role，再用 v1 删除 slot。版本不匹配被当作“已由其他人接管”忽略，实际是本次自己的 v2。

复现只包装 Slots.Update：先执行真实更新，再返回一次“reply lost”错误。得到 `slot v2 -> PlayerID 1000001`，角色记录已不存在；再次 CreateRole 得到 `ErrRoleLimit`。不是上一轮 RR-03 的跨服同 owner 名字复用问题。

slot 没有 TTL，这个占位不会自然到期；需要修复/运维恢复。这里实测的是后端结果不确定的故障模型，不是实际断网抓包；Redis 写已提交而响应丢失可以产生这种模型，但本轮未执行网络故障注入。

实施：优先复用 versionstore 和现有 Directory，保存本次建角操作身份/阶段。尾部错误后先核对 slot 是否已提交给本次 PlayerID，再决定确认成功、恢复或继续等待；不能一概删除角色。读取失败仍是 unknown，不得擅自补偿；版本不匹配也不能证明属于别的操作。条件回收需要 slot 身份和阶段，避免重入 ABA。门禁：未应用即失败、应用后响应丢失、核对读取失败、进程恢复以及不同 creator 重入。

## RR-20260929-13

**P2：旧路由绑定的 lease 在迁移完成后仍能续期。未修复。**

位置：[GameLease 公开约定](../../kit/service/global/types.go#L182)、[RenewLease](../../kit/service/global/service.go#L314)。类型注释要求 renewal 对不同 binding 拒绝；实现只检查 incarnation、lease 状态与到期时间，不核对当前 routing 的 SID/group/epoch。

顺序调用 Bind(game 100, group g, global 1)、AcquireLease、BeginMigration(target 2)、CompleteMigration，再用原 incarnation RenewLease。当前 routing 是 `(sid=2, epoch=3)`，续期成功返回 `(sid=1, epoch=1)`。这里没有并发，也没有直接篡改存储。

旧绑定可以持续延长存活快照，新 AcquireLease 又会因旧 lease 尚 active 被拒绝。影响是 routing 与协调租约不一致；GameLease 本身不是 remote entity 权威 fence，本报告没有声称它已导致实体双主。

实施：明确迁移期间旧 lease 的有效边界、完成后如何失效/重新获取。沿用 routing epoch 和 incarnation，将跨记录一致性纳入提交协议；单独在 Update 前 Get 一次 routing 只能修顺序例子，不能证明没有检查后迁移的竞态。若用 Redis 原子操作，明确同槽条件和 MemoryStore 对等契约；Lease/LiveGames 的读取也应遵守同一规则。门禁覆盖迁移开始/完成、旧续期、重新 acquire 和迟到 heartbeat。

## RR-20260929-14

**P2：Attach 允许调用者自行声明资源已释放，绕过正常回收。未修复。**

位置：[Resource](../../service/session/types.go#L202)、[Attach](../../service/session/service.go#L458)、[生成 RPC 字段](../../service/session/session_rpc_gen.go#L111)。Resource.Validate 只校验 Kind/ID 非空，Attach 原样追加 Resource。ReleasedAtUnix/ForcedRelease 本应是回收/管理员动作产生的服务状态，但也随公开 Attach 输入传入。

Attach(scene held, ReleasedAtUnix=now, ForcedRelease=true) 成功；Finish 成功，`Releaser(scene, held)` 调用次数为 **0**。Pending 根据 ReleasedAtUnix 跳过该资源。测试没有模拟真实场景分配，因此未量化实际外部资源泄漏；已证明回收协议被绕过、状态可伪造。RPC 是服务间入口，这里没有推定玩家可直接调用。

实施：领域入口拒绝调用方传入非零释放字段，或用仅含 Kind/ID 的 attachment 请求类型；释放状态只由 markReleased/admin 路径写入。同步生成传输并做兼容说明，不能只在某一个客户端清零。已有真实回收状态不能被迁移误清零。门禁含正常 attachment、伪造 timestamp/force、重复 attachment 与管理强制释放的审计区别。

## RR-20260929-15

**P2：非负活动进度累加可回绕为负数。未修复。**

位置：[ProgressDelta.Validate](../../kit/service/global/activity/types.go#L476)、[Participant Update](../../kit/service/global/activity/service.go#L1049)。公开输入允许非负 int64，累加直接 `+=`，未检查总和。

开放活动，第一次 Score/Progress 增量均为 `math.MaxInt64`，第二次均为 1；两个请求成功，持久化的两个总值均为 `-9223372036854775808`。极端累计值是触发条件，不是普通小值马上失败；多次合法增量也可以最终到达上界。

实施：在参加者 CAS 回调内部、改变状态/去重证明之前检查加法界限。选显式拒绝或有文档的饱和规则；拒绝不能改变 score/progress 或错误地把请求记为 applied。分别验证单字段、同时溢出、临界值、重放和错误后合法请求。不要用浮点数绕过边界而丢失整数精度。

## RR-20260929-16

**P2：邮件初次 envelope 写失败留下不可恢复的发送账本。未修复。**

位置：[Send 先占账本再写正文](../../service/mail/service.go#L191)、[写正文错误分支](../../service/mail/service.go#L211)、[replaySend](../../service/mail/service.go#L280)、[SentRecord](../../service/mail/store.go#L49)。账本只持有 MailID，没有可恢复的完整 envelope 意图。

首次 Envelopes.Create 在写入前返回一次临时错误；账本已经存在。存储恢复后再次 Send 相同请求，仍得到 `mail: conflict: send "send-1" names missing mail mail-1`，再也到不了 Create。内存账本会一直占位；有 TTL 的 Redis 账本只在到期后允许当作新请求，不等于原请求恢复。

缺失正文时拒绝回答“已发送”是正确的保护；问题是一个普通瞬时失败进入了没有恢复出口的状态。不能以“删除账本然后重新发送”作为通用方案：在结果未知、既有投递、正文被外部误删等不同情况下会改变幂等语义。

实施：保留固定 MailID 的发送意图（envelope 内容或可靠引用，以及内容摘要/阶段），重试恢复同一 envelope，再沿现有 deliverAndRecord/邮箱去重继续。若正文已存在，校验语义一致而不是生成另一个 ID。沿用 versionstore/EnvelopeStore 的 insert-only 能力；跨键事务仅在部署条件允许且明确契约后采用。门禁同时含写前失败、已写响应丢失、并发同请求、同键异内容和已有投递后正文缺失。

## RR-20260929-17

**P3：global lease 的输出 Load map 与 MemoryStore 状态共用引用。未修复。**

位置：[RenewLease 返回 result](../../kit/service/global/service.go#L348)、[Lease](../../kit/service/global/service.go#L408)、[LiveGames](../../kit/service/global/service.go#L424)。输入 Load 被 clone，但输出没 clone；内存 versionstore 的结构体复制不深拷贝 map。

分别从 RenewLease、Lease、LiveGames 拿快照并修改 `snapshot.Load["load"]`，重新读存储得到 `forged`，Version 仍为 `2 -> 2`。三个子场景都失败，没有经过任何 CAS。这是 MemoryStore/嵌入式使用场景的持久对象所有权错误；Redis JSON 解码返回新 map，本轮不把该引用污染推定到 Redis。

实施：在类型边界 cloneLoad 输出，保留已经正确的输入克隆。不要求泛型 versionstore 反射深拷贝所有值。覆盖三种输出和输入修改，证明存储内容、版本不变；并发访问另做 race 门禁，本轮顺序反例未发现 Go race。

## RR-20260929-18

**P3：Finish 的终态重试成功后遗漏 claim 清理。未修复。**

位置：[finish 的终态分支](../../service/session/service.go#L556)、[正常分支 releaseClaim](../../service/session/service.go#L578)。第一次 Finish 已把 run 置终态，但 Releaser 返回错误，claim 按正常逻辑暂时保留。第二次 Finish 进入终态分支，只调用 releasePending，不再调用 releaseClaim。

单次 Releaser 失败后重试成功，所有资源已释放，Claims.Get(owner) 仍 found=true。**下一次 Enter 的 resolveClaim 或显式 Sweep 可以修复，因此不称永久拒绝进入。** 问题是成功 Finish 的完成语义不一致、终态索引残留和清理指标不完整。

实施：成功完成资源释放的所有 finish/leave 路径共用条件 claim 清理，并修复下方 ABA 的原子身份约束；不能直接在重试分支无条件 Delete。验证首试成功、释放失败后重试成功、重复 Finish 和新 run 重入。

## 旧 RR-20260909-02：正常 Finish 下的 ABA 新触发

详见[原编号追加证据](REVIEW-2026-09-09-02.md#2026-09-29-normal-finish-aba)。历史 U-0158 修复的是 Enter 撞 RequestID 时“先释放 claim，再删仍 live 的落败 run”的顺序，本轮没有重跑旧触发或否定该顺序修复。

**新触发已在真实 Redis 复现**：A 正常 Finish 先进入终态；releaseClaim 读出 A 的 v1 claim 后暂停。B 正常 Enter 识别终态 A，释放其 claim，创建 B 的新 v1 claim。A 恢复执行版本删除，将 B 的 claim 删掉；C 再 Enter 成功，B、C 两条 run 同时 live。RunID 的 Get 检查和删除分开，不能原子证明身份。

这是同一“版本跨键重建重用”的根因，保留旧 RR，状态标为**原路径已修、新路径残留**。修复使用不可复用身份/代际的原子条件删除或跨重建单调版本，优先扩展框架已有存储能力；不在本轮修改公共 Store。影响仅按复现结论写唯一 live run 被破坏，未实测游戏资源后果。

## 未认定为 bug 的观察与后续范围

Activity ApplyProgress 在检查活动准入之后暂停；另一个 Notify 完成活动；已准入的请求随后写参加者成功。这证明物理写入可以晚于 complete 状态，但两个调用有重叠，目前没有查到“完成时必须冻结所有在途进度”的明确契约，因此仅列观察。若业务要求奖励/排名为封闭快照，再设计关闭准入与等待在途提交的门禁；不能仅因测试人为期待拒绝就登记 bug。

match 的历史 Tickets/Matches/Requests 增长已有 09-17 容量观察，不重复登记；chat 的有限去重窗口也不能直接当作本轮新缺陷。容量压测、真实 Broker/发奖、Redis HA/断网、多进程强杀恢复仍未验证。所有本轮“未修复”均指本次审查未实施；后续状态以新的 bugfix 验收为准。
