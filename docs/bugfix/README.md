# Bugfix 记录

`docs/bug/` 是只审查不改码的发现（RR 编号）；本目录记录这些发现是怎样被修掉的：
改了哪几行、为什么这样改、用什么测试证明修前红修后绿、留下了什么没做。
每条一个文件，编号沿用 RR，账本单元编号（U-）指向 [history/ledger.md](../history/ledger.md)。

| 编号 | 仓库 | 问题 | 修复单元 | 记录 |
| --- | --- | --- | --- | --- |
| RR-20260908-01 | kit | Session `Enter` 丢掉幂等账本 Create 的落败结果，两个 owner 都成功 | U-0154（方案 B） | [RR-20260908-01.md](RR-20260908-01.md) |
| RR-20260908-02 | core | ReadThrough 跟随者取消后不归还等待名额 | U-0155 | [RR-20260908-02.md](RR-20260908-02.md) |
| RR-20260908-03 | codegen | `--consolidate` 对单行 import 的混合分流产出非法 Go | U-0156 | [RR-20260908-03.md](RR-20260908-03.md) |
| RR-20260909-01 | core/docs | Quickstart 固定过时 codegen 版本且缺 `-module` | U-0157 | [RR-20260909-01.md](RR-20260909-01.md) |
| RR-20260909-02 | kit | Session 撞 RequestID 的撤回误删同 owner 重新取得的 claim（ABA） | U-0158 | [RR-20260909-02.md](RR-20260909-02.md) |
| RR-20260909-03 | core | Assembly 停机未完成就忘掉 Runtime，重试虚报成功 | U-0159 | [RR-20260909-03.md](RR-20260909-03.md) |
| RR-20260909-04 | codegen | 同包多个 Entity 生成重名注册符号 | U-0160 | [RR-20260909-04.md](RR-20260909-04.md) |
| RR-20260909-05 | kit | Enqueue 幂等重放返回其他 Subject 的 Ticket | U-0164 | [RR-20260909-05.md](RR-20260909-05.md) |
| RR-20260909-06 | codegen | 多 Entity 配合显式 `-output` 静默覆盖生成结果 | U-0162 | [RR-20260909-06.md](RR-20260909-06.md) |
| RR-20260910-01 | core | 极短 IdleTTL 推导出零扫描周期 | U-0163 | [RR-20260910-01.md](RR-20260910-01.md) |
| RR-20260910-02 | kit | 已领取邮件淘汰后重投生成新发奖 token | U-0165 | [RR-20260910-02.md](RR-20260910-02.md) |
| RR-20260910-03 | core | 完成事务的等待者因缓存淘汰读到已删记录而 panic | U-0168 | [RR-20260910-03.md](RR-20260910-03.md) |
| RR-20260910-04 | core | Checkpoint 恢复按 ID 重排完成历史,淘汰顺序分叉 | U-0169 | [RR-20260910-04.md](RR-20260910-04.md) |
| RR-20260910-05 | codegen | category 指向业务包常量时生成物遗漏 import(M-05 引入) | U-0166 | [RR-20260910-05.md](RR-20260910-05.md) |
| RR-20260910-06 | codegen | 聚合注册的固定 entity 导入与业务包别名冲突(M-05 引入) | U-0167 | [RR-20260910-06.md](RR-20260910-06.md) |
| RR-20260911-01 | kit | 墓碑按计数淘汰早于信封过期,再铸领取 token(U-0165 残余) | U-0171 | [RR-20260911-01.md](RR-20260911-01.md) |
| RR-20260911-02 | kit | `Mailbox.clone` 未复制 SettledClaims,快照与存储共享(U-0165 引入) | U-0170 | [RR-20260911-02.md](RR-20260911-02.md) |
| RR-20260911-03 | core | finalizer 停止后晚到的 Close 仍被接受,遗留 entries 与 slot | U-0173 | [RR-20260911-03.md](RR-20260911-03.md) |
| RR-20260911-04 | core | Nest 停机回收延迟消息而不回复等待者 | U-0172 | [RR-20260911-04.md](RR-20260911-04.md) |
| RR-20260913-03 | core | Remote payload 身份未与信封绑定,可写到别的 scope | U-0179 | [RR-20260913-03.md](RR-20260913-03.md) |
| RR-20260913-04 | core | 快照合并加载的等待名额在取消后泄漏 | U-0174 | [RR-20260913-04.md](RR-20260913-04.md) |
| RR-20260913-07 | core | L2 同版本比较遗漏 schema / codec | U-0176 | [RR-20260913-07.md](RR-20260913-07.md) |
| RR-20260913-08 | core | 快照的绝对过期时间未参与读取准入 | U-0175 | [RR-20260913-08.md](RR-20260913-08.md) |
| RR-20260913-10 | core | 快照 Lua 把 uint64 版本转浮点,排序失真 | U-0177 | [RR-20260913-10.md](RR-20260913-10.md) |
| RR-20260913-11 | core | 大 epoch 拼成科学计数法并持久化不可读 marker | U-0178 | [RR-20260913-11.md](RR-20260913-11.md) |
| RR-20260913-05 | core | L2 版本冲突被 IgnoreRemoteError 吞掉,L1/L2 同版本分叉 | U-0180 | [RR-20260913-05.md](RR-20260913-05.md) |
| RR-20260913-06 | core | L2 回填绕过同版本内容检查,覆盖已发布值 | U-0181 | [RR-20260913-06.md](RR-20260913-06.md) |
| RR-20260913-02 | core | 迟到的旧 release 取消更新的 renewal | U-0184 | [RR-20260913-02.md](RR-20260913-02.md) |
| RR-20260911-05 | kit | 被拒绝的投递仍修改 MemoryStore 邮箱(回调未 clone) | U-0182 | [RR-20260911-05.md](RR-20260911-05.md) |
| RR-20260911-06 | core | AfterCommit panic 遗漏回复与释放,饱和回退可崩溃 | U-0183 | [RR-20260911-06.md](RR-20260911-06.md) |

重构类改动(不对应任何 RR,不关闭任何 RR)另记,编号 M-:

| 编号 | 仓库 | 改了什么 | 记录 |
| --- | --- | --- | --- |
| M-01 | core | entity kind 注册表改为按 kind 的无锁定长表;含 category 重设计四步方案,仅第一步已实施 | [M-01-entity-kind-registry.md](M-01-entity-kind-registry.md) |
| M-02 | core | category 离开 EntityID,注册表成为唯一权威;ID 那两位降为历史填充,零数据迁移 | [M-02-category-leaves-the-id.md](M-02-category-leaves-the-id.md) |
| M-03 | core | 声明 category 后值即锁序,remote 档强制最先,锁序注册期派生;新增 `ValidateEntityRegistry` | [M-03-category-lock-order.md](M-03-category-lock-order.md) |
| M-04 | core + codegen | **破坏性**:删除 `RemotePolicyCapable`、`GetEntityGroupFunc`、`EntityGroup*`;推荐分类常量;codegen 拒绝 `remote=capable`、脚手架默认 Other | [M-04-drop-capable-and-the-group-hook.md](M-04-drop-capable-and-the-group-hook.md) |
| M-05 | codegen | `category=` 进实体标记并直接进生成物,拆掉运行期查表的隐式前置;生成的聚合注册末尾调 `ValidateEntityRegistry` | [M-05-marker-owns-the-category.md](M-05-marker-owns-the-category.md) |

写法约定：**问题**（一句话）→ **根因**（指向具体行）→ **方案选择**（列出考虑过的方案与取舍）→
**改动**（文件与要点）→ **证明**（红测试名、修前失败文本、修后结果）→ **未做 / 边界**。
前四项已随 core v1.15.2 / kit v1.14.3 / codegen v1.15.4（2026-09-09）发版；RR-20260909-02/03/04 已修复待下次发版（core v1.15.3 / kit v1.14.4 / codegen v1.15.5）。

## 仍未修复的四项(2026-09-13 第二轮后)

| 编号 | 为什么还没修 |
| --- | --- |
| RR-20260913-01 | 快照删除没有版本屏障。要先定"删除水位保留多久、与可重放窗口和重新同步代际怎么协调",否则墓碑要么泄漏要么早失效 —— kit mail 那条墓碑 RR 被反复打回的形状。 |
| RR-20260913-09 | Transfer 回复丢失后恢复旧 owner。要引入"结果不确定"状态:失效本地 marker、进入不可写、用独立有界 context 查权威,查不到就继续冻结。这是所有权状态机的扩展。 |
| RR-20260912-01 | nestwal Shutdown 等锁时忽略截止时间。要让"等待 Flush / replay 所有权"变成可取消的等待,`sync.Mutex` 没有带 context 的获取。 |
| RR-20260912-02 | WAL.Sync 成功返回时已准入 ticket 仍未写入。要一个有序屏障或 LSN 水位等待,兼顾取消、关闭与 terminal 错误;只 fsync 文件覆盖不到还在内存队列里的记录。 |

上一轮列为"需先定契约"的六项里,RR-20260913-02(订阅代际)、05、06(L2 一致性)本轮已收敛:
契约分别定为"发布端只增代际、release 只删不落后的"和"同版本同值是所有写 L1 入口共用的一条规则,
冲突是一致性错误不可吞"。
